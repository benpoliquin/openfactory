// Package gitrepo owns controller-only Git credentials and repository promotion.
package gitrepo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/policy"
)

type Manager struct {
	cfg    model.Config
	locks  sync.Map
	client *http.Client
	apiURL string
}
type record struct {
	Path, Common, Branch, Base, Candidate, Project, Repository, Target, ConfigHash string
	SourceBase, SourceCandidate                                                    string
}

func (m *Manager) lock(key string) func() {
	value, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
var _ model.Repositories = (*Manager)(nil)

func New(cfg model.Config) *Manager {
	return &Manager{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, apiURL: "https://api.github.com"}
}
func digest(s string) string       { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (m *Manager) records() string { return filepath.Join(m.cfg.StateDir, "git-workspaces") }
func (m *Manager) recordPath(path string) string {
	return filepath.Join(m.records(), digest(path)+".json")
}
func (m *Manager) secretValues() []string {
	var values []string
	for _, p := range m.cfg.Projects {
		if p.Auth.TokenFile != "" {
			if b, e := os.ReadFile(p.Auth.TokenFile); e == nil {
				token := strings.TrimSpace(string(b))
				if token != "" {
					values = append(values, token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
				}
			}
		}
		if p.Auth.SSHKeyFile != "" {
			if b, e := os.ReadFile(p.Auth.SSHKeyFile); e == nil && len(b) > 0 {
				values = append(values, strings.TrimSpace(string(b)))
			}
		}
	}
	return values
}
func gitArgs(args ...string) []string {
	return append([]string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always"}, args...)
}
func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		dir = m.cfg.StateDir
	}
	// Successful structured output must remain byte-exact for hashes and paths.
	out, err := runGit(ctx, dir, gitArgs(args...), nil, nil, 120)
	if err != nil {
		detail := err.Error()
		for _, secret := range m.secretValues() {
			if secret != "" {
				detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
			}
		}
		return "", errors.New(detail)
	}
	return out, nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (m *Manager) auth(p model.Project) ([]string, []string, error) {
	switch p.Auth.Mode {
	case "", "none":
		if strings.HasPrefix(p.Repository, "ssh://") || strings.HasPrefix(p.Repository, "git@") {
			return nil, nil, errors.New("SSH transport requires enrolled SSH key and strict known_hosts")
		}
		return nil, nil, nil
	case "token":
		u, e := url.Parse(p.Repository)
		if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
			return nil, nil, errors.New("token authentication requires credential-free https://github.com repository")
		}
		if e = safeFile(p.Auth.TokenFile, true); e != nil {
			return nil, nil, e
		}
		b, e := os.ReadFile(p.Auth.TokenFile)
		if e != nil {
			return nil, nil, e
		}
		t := strings.TrimSpace(string(b))
		if t == "" || strings.ContainsAny(t, "\r\n") {
			return nil, nil, errors.New("invalid token file")
		}
		encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + t))
		return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + encoded}, []string{t, encoded}, nil
	case "ssh":
		if !(strings.HasPrefix(p.Repository, "ssh://") || strings.HasPrefix(p.Repository, "git@")) {
			return nil, nil, errors.New("SSH authentication requires SSH repository")
		}
		if e := safeFile(p.Auth.SSHKeyFile, true); e != nil {
			return nil, nil, e
		}
		if e := safeFile(p.Auth.KnownHostsFile, false); e != nil {
			return nil, nil, e
		}
		command := "ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none -o ClearAllForwardings=yes -o ForwardAgent=no -o PermitLocalCommand=no -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + quote(p.Auth.KnownHostsFile) + " -o GlobalKnownHostsFile=/dev/null -i " + quote(p.Auth.SSHKeyFile)
		return []string{"GIT_SSH_COMMAND=" + command}, nil, nil
	default:
		return nil, nil, errors.New("unsupported git authentication mode")
	}
}
func (m *Manager) network(ctx context.Context, dir string, p model.Project, args ...string) (string, error) {
	env, secrets, e := m.auth(p)
	if e != nil {
		return "", e
	}
	return runGit(ctx, dir, gitArgs(args...), env, append(secrets, m.secretValues()...), 120)
}
func validProject(p model.Project) error {
	if !p.Trusted {
		return errors.New("repository is not enrolled as trusted")
	}
	if p.Repository == "" || strings.HasPrefix(p.Repository, "-") || strings.ContainsAny(p.Repository, "\r\n\x00") {
		return errors.New("invalid repository")
	}
	if p.Branch == "" || strings.HasPrefix(p.Branch, "-") || strings.ContainsAny(p.Branch, " ~^:?*[\\\r\n") || strings.Contains(p.Branch, "..") || strings.Contains(p.Branch, "@{") || strings.HasSuffix(p.Branch, "/") || strings.HasSuffix(p.Branch, ".lock") {
		return errors.New("invalid target branch")
	}
	if strings.Contains(p.Repository, "://") {
		u, e := url.Parse(p.Repository)
		if e != nil {
			return e
		}
		if u.Scheme != "https" && u.Scheme != "ssh" && u.Scheme != "file" {
			return errors.New("repository transport not allowed")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return errors.New("repository URL must not contain query or fragment")
		}
		if u.User != nil && (u.Scheme != "ssh" || strings.Contains(u.User.String(), ":")) {
			return errors.New("repository URL must not contain credentials")
		}
	}
	if !strings.Contains(p.Repository, "://") && !strings.HasPrefix(p.Repository, "git@") && !filepath.IsAbs(p.Repository) {
		return errors.New("local repository path must be absolute")
	}
	return nil
}
func (m *Manager) enrolled(name string, p model.Project) error {
	approved, ok := m.cfg.Projects[name]
	if !ok || !reflect.DeepEqual(approved, p) {
		return errors.New("project does not match controller enrollment")
	}
	return validProject(p)
}
func (m *Manager) Prepare(ctx context.Context, name string, p model.Project, attempt string) (model.Workspace, error) {
	defer m.lockRepository(p)()
	if err := m.enrolled(name, p); err != nil {
		return model.Workspace{}, err
	}
	if err := CheckCapabilities(ctx); err != nil {
		return model.Workspace{}, err
	}
	if attempt == "" || name == "" {
		return model.Workspace{}, errors.New("project and attempt required")
	}
	root, e := filepath.Abs(m.cfg.WorkspaceDir)
	if e != nil {
		return model.Workspace{}, e
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return model.Workspace{}, e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return model.Workspace{}, e
	}
	if e = os.MkdirAll(m.cfg.StateDir, 0700); e != nil {
		return model.Workspace{}, e
	}
	state, e := filepath.EvalSymlinks(m.cfg.StateDir)
	if e != nil {
		return model.Workspace{}, e
	}
	state, e = filepath.Abs(state)
	if e != nil {
		return model.Workspace{}, e
	}
	if state == root || strings.HasPrefix(state, root+string(filepath.Separator)) {
		return model.Workspace{}, errors.New("controller Git state must not be on the shared workspace")
	}
	path := filepath.Join(root, "attempts", digest(name), digest(name+"\x00"+attempt))
	shared, relative, e := m.sharedRoot(path)
	if e != nil {
		return model.Workspace{}, e
	}
	defer shared.Close()
	common, e := filepath.Abs(filepath.Join(m.cfg.StateDir, "git-repositories", digest(name)+".git"))
	if e != nil {
		return model.Workspace{}, e
	}
	if data, e := os.ReadFile(m.recordPath(path)); e == nil {
		var r record
		if e = json.Unmarshal(data, &r); e != nil {
			return model.Workspace{}, e
		}
		if r.Project != name || r.Repository != p.Repository || r.Target != p.Branch {
			return model.Workspace{}, errors.New("workspace enrollment changed")
		}
		if _, e = m.workspace(ctx, path); e != nil {
			return model.Workspace{}, e
		}
		return model.Workspace{Path: path, BaseSHA: r.Base}, nil
	}
	if _, e = shared.Lstat(relative); e == nil {
		return model.Workspace{}, errors.New("unrecorded workspace exists; refusing to overwrite")
	} else if !os.IsNotExist(e) {
		return model.Workspace{}, e
	}
	if e = os.MkdirAll(filepath.Dir(common), 0700); e != nil {
		return model.Workspace{}, e
	}
	if e = shared.MkdirAll(filepath.Dir(relative), 0700); e != nil {
		return model.Workspace{}, e
	}
	if e = os.MkdirAll(m.records(), 0700); e != nil {
		return model.Workspace{}, e
	}
	// An existing cache must match the enrollment receipt; never repair or prune it.
	cacheRecord := filepath.Join(m.records(), "cache-"+digest(name)+".json")
	var enrolled record
	if b, e := os.ReadFile(cacheRecord); e == nil {
		if e = json.Unmarshal(b, &enrolled); e != nil {
			return model.Workspace{}, e
		}
		if enrolled.Repository != p.Repository || enrolled.Target != p.Branch {
			return model.Workspace{}, errors.New("repository enrollment changed")
		}
		if e = verifyConfig(common, enrolled.ConfigHash); e != nil {
			return model.Workspace{}, e
		}
	} else if !os.IsNotExist(e) {
		return model.Workspace{}, e
	} else {
		if _, e = os.Lstat(common); !os.IsNotExist(e) {
			return model.Workspace{}, errors.New("unrecorded repository cache exists")
		}
		if _, e = m.git(ctx, "", "init", "--bare", common); e != nil {
			return model.Workspace{}, e
		}
		hash, e := configHash(common)
		if e != nil {
			return model.Workspace{}, e
		}
		enrolled = record{Repository: p.Repository, Target: p.Branch, ConfigHash: hash}
		if e = saveExclusive(cacheRecord, enrolled); e != nil {
			return model.Workspace{}, e
		}
	}
	if _, e = m.network(ctx, common, p, "fetch", "--no-tags", "--no-recurse-submodules", "--", p.Repository, "refs/heads/"+p.Branch); e != nil {
		return model.Workspace{}, e
	}
	base, e := m.git(ctx, common, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if e != nil {
		return model.Workspace{}, e
	}
	base = strings.TrimSpace(base)
	branch := "openfactory/" + digest(name+"\x00"+attempt)
	if _, e = m.git(ctx, common, "update-ref", "refs/openfactory/bases/"+digest(name+"\x00"+attempt), base); e != nil {
		return model.Workspace{}, e
	}
	if e = m.checkout(ctx, common, base, path); e != nil {
		return model.Workspace{}, e
	}
	r := record{Path: path, Common: common, Branch: branch, Base: base, Project: name, Repository: p.Repository, Target: p.Branch, ConfigHash: enrolled.ConfigHash}
	if e = saveExclusive(m.recordPath(path), r); e != nil {
		return model.Workspace{}, e
	}
	return model.Workspace{Path: path, BaseSHA: base}, nil
}
func saveExclusive(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return e
	}
	return f.Sync()
}
func configHash(common string) (string, error) {
	b, e := os.ReadFile(filepath.Join(common, "config"))
	if e != nil {
		return "", e
	}
	return digest(string(b)), nil
}
func verifyConfig(common, expected string) error {
	h, e := configHash(common)
	if e != nil {
		return e
	}
	if h != expected {
		return errors.New("repository config changed unexpectedly")
	}
	if _, e = os.Lstat(filepath.Join(common, "info", "grafts")); e == nil {
		return errors.New("unexpected repository ancestry grafts")
	} else if !os.IsNotExist(e) {
		return e
	}
	return nil
}
func (m *Manager) workspace(ctx context.Context, path string) (record, error) {
	var r record
	absolute, e := filepath.Abs(path)
	if e != nil {
		return r, e
	}
	b, e := os.ReadFile(m.recordPath(absolute))
	if e != nil {
		return r, fmt.Errorf("unknown openfactory workspace: %w", e)
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return r, e
	}
	if r.Path != absolute {
		return r, errors.New("workspace identity changed")
	}
	private, e := filepath.Abs(filepath.Join(m.cfg.StateDir, "git-repositories", digest(r.Project)+".git"))
	if e != nil || r.Common != private {
		return r, errors.New("workspace receipt does not reference a controller-private repository")
	}
	if e = verifyConfig(r.Common, r.ConfigHash); e != nil {
		return r, e
	}
	info, e := os.Lstat(absolute)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return r, errors.New("workspace is not a regular directory")
	}
	resolved, e := filepath.EvalSymlinks(absolute)
	if e != nil || resolved != absolute {
		return r, errors.New("workspace path was redirected")
	}
	return r, nil
}

// capture uses only private Git configuration and a fresh private index. Shared
// HEAD, index, refs, hooks, filters and hidden agent commits are never authority.
func (m *Manager) capture(ctx context.Context, r record) (string, error) {
	indexDir, err := os.MkdirTemp(m.records(), "index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(indexDir)
	workspace := filepath.Join(indexDir, "source")
	if err = os.Mkdir(workspace, 0700); err != nil {
		return "", err
	}
	shared, relative, err := m.sharedRoot(r.Path)
	if err != nil {
		return "", err
	}
	defer shared.Close()
	private, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer private.Close()
	if err = copyRootedTree(ctx, shared, relative, private, ".", true); err != nil {
		return "", err
	}
	base := r.Candidate
	if base == "" {
		base = r.Base
	}
	args := []string{"--git-dir=" + r.Common, "--work-tree=" + workspace, "-c", "core.bare=false"}
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index")}
	command := func(tail ...string) (string, error) {
		return runGit(ctx, workspace, gitArgs(append(append([]string{}, args...), tail...)...), env, m.secretValues(), 120)
	}
	if _, err = command("read-tree", base); err != nil {
		return "", err
	}
	if _, err = command("add", "--all", "--", "."); err != nil {
		return "", err
	}
	tree, err := command("write-tree")
	return strings.TrimSpace(tree), err
}

func (m *Manager) Fingerprint(ctx context.Context, path string) (string, error) {
	r, err := m.workspace(ctx, path)
	if err != nil {
		return "", err
	}
	tree, err := m.capture(ctx, r)
	if err != nil {
		return "", err
	}
	return digest(tree), nil
}

func (m *Manager) Commit(ctx context.Context, path, message string) (string, error) {
	defer m.lock(path)()
	r, err := m.workspace(ctx, path)
	if err != nil {
		return "", err
	}
	if r.SourceCandidate != "" && r.SourceCandidate != r.Candidate {
		return "", errors.New("reconciled source delta is immutable; candidate cannot be recaptured")
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("commit message required")
	}
	tree, err := m.capture(ctx, r)
	if err != nil {
		return "", err
	}
	name, email := m.cfg.GitName, m.cfg.GitEmail
	if name == "" {
		name = "OpenFactory"
	}
	if email == "" {
		email = "openfactory@localhost"
	}
	out, err := m.git(ctx, r.Common, "-c", "user.name="+name, "-c", "user.email="+email, "commit-tree", tree, "-p", r.Base, "-m", message)
	if err != nil {
		return "", err
	}
	r.Candidate = strings.TrimSpace(out)
	r.SourceBase, r.SourceCandidate = r.Base, r.Candidate
	// Keep candidate objects reachable without trusting any shared refs.
	if _, err = m.git(ctx, r.Common, "update-ref", "refs/heads/"+r.Branch, r.Candidate); err != nil {
		return "", err
	}
	if err = m.saveRecord(r); err != nil {
		return "", err
	}
	return r.Candidate, nil
}

func (m *Manager) saveRecord(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.records(), "receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), m.recordPath(r.Path))
}

// checkout constructs a self-contained repository privately, then copies it to
// the shared volume. No Git process ever reads configuration from that volume.
func (m *Manager) checkout(ctx context.Context, common, sha, destination string) error {
	dir, err := os.MkdirTemp(m.records(), "checkout-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	repo := filepath.Join(dir, "source")
	if err = m.privateCheckout(ctx, common, sha, repo); err != nil {
		return err
	}
	return m.copyCheckout(ctx, repo, destination)
}

// Fetch only this SHA's reachable history, never every rejected/in-flight blob.
func (m *Manager) privateCheckout(ctx context.Context, common, sha, repo string) error {
	if _, err := m.git(ctx, "", "init", repo); err != nil {
		return err
	}
	if _, err := m.git(ctx, repo, "fetch", "--no-tags", "--no-recurse-submodules", "--", common, sha); err != nil {
		return err
	}
	_, err := m.git(ctx, repo, "checkout", "--detach", sha, "--")
	return err
}

func (m *Manager) Snapshot(ctx context.Context, workspace, candidate string) (model.Workspace, error) {
	defer m.lock(workspace)()
	r, err := m.workspace(ctx, workspace)
	if err != nil {
		return model.Workspace{}, err
	}
	if !shaPattern.MatchString(candidate) || r.Candidate != candidate {
		return model.Workspace{}, errors.New("snapshot candidate lacks private capture receipt")
	}
	path := filepath.Join(filepath.Dir(r.Path), "snapshot-"+rand.Text())
	if err = m.checkout(ctx, r.Common, candidate, path); err != nil {
		return model.Workspace{}, err
	}
	r.Path = path
	if err = saveExclusive(m.recordPath(path), r); err != nil {
		return model.Workspace{}, err
	}
	return model.Workspace{Path: path, BaseSHA: r.Base}, nil
}
func (m *Manager) ChangedPaths(ctx context.Context, path, base, candidate string) ([]string, error) {
	r, e := m.workspace(ctx, path)
	if e != nil {
		return nil, e
	}
	if !shaPattern.MatchString(base) || (candidate != "" && !shaPattern.MatchString(candidate)) {
		return nil, errors.New("invalid commit SHA")
	}
	if base != r.Base || (candidate != "" && candidate != r.Candidate) {
		return nil, errors.New("diff does not match private candidate receipt")
	}
	if candidate == "" {
		candidate, e = m.capture(ctx, r)
		if e != nil {
			return nil, e
		}
	}
	commands := [][]string{{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, candidate, "--"}}
	seen := map[string]bool{}
	var paths []string
	for _, args := range commands {
		out, e := m.git(ctx, r.Common, args...)
		if e != nil {
			return nil, e
		}
		for _, p := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
			if p != "" && !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}
func (m *Manager) validateCandidate(ctx context.Context, name string, p model.Project, a model.Attempt) (record, error) {
	r, e := m.workspace(ctx, a.Workspace)
	if e != nil {
		return r, e
	}
	if e = m.enrolled(name, p); e != nil {
		return r, e
	}
	if r.Project != name || r.Repository != p.Repository || r.Target != p.Branch || r.Base != a.BaseSHA || !shaPattern.MatchString(a.CandidateSHA) {
		return r, errors.New("candidate enrollment or baseline mismatch")
	}
	if r.Candidate != a.CandidateSHA {
		return r, errors.New("candidate lacks private capture receipt")
	}
	tree, e := m.capture(ctx, r)
	if e != nil {
		return r, e
	}
	expectedTree, e := m.git(ctx, r.Common, "rev-parse", a.CandidateSHA+"^{tree}")
	if e != nil {
		return r, e
	}
	if tree != strings.TrimSpace(expectedTree) {
		return r, errors.New("candidate source changed after capture")
	}
	parents, e := m.git(ctx, r.Common, "rev-list", "--parents", "-n", "1", a.CandidateSHA)
	if e != nil || strings.TrimSpace(parents) != a.CandidateSHA+" "+a.BaseSHA {
		return r, errors.New("candidate is not one controller-authored commit on baseline")
	}
	paths, e := m.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
	if e != nil {
		return r, e
	}
	for _, path := range paths {
		for _, pattern := range p.ProtectedPaths {
			if policy.Matches(pattern, path) {
				return r, fmt.Errorf("protected path changed: %s", path)
			}
		}
	}
	return r, nil
}
func (m *Manager) remoteHead(ctx context.Context, r record, p model.Project, branch string) (string, error) {
	out, e := m.network(ctx, r.Common, p, "ls-remote", "--exit-code", "--heads", "--", p.Repository, "refs/heads/"+branch)
	if e != nil {
		return "", e
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch || !shaPattern.MatchString(fields[0]) {
		return "", errors.New("unexpected remote branch state")
	}
	return fields[0], nil
}
func (m *Manager) Integrate(ctx context.Context, name string, p model.Project, a model.Attempt, expectedBase string) (string, error) {
	defer m.lockRepository(p)()
	if !shaPattern.MatchString(expectedBase) || expectedBase != a.BaseSHA {
		return "", deliveryError("rejected", "integration expected base does not match private candidate baseline", "", nil)
	}
	r, err := m.validateCandidate(ctx, name, p, a)
	if err != nil {
		return "", deliveryError("rejected", err.Error(), "", nil)
	}
	base, err := m.fetchTarget(ctx, r, p)
	if err != nil {
		return "", deliveryError("rejected", err.Error(), "", nil)
	}
	if base == a.CandidateSHA {
		return base, nil
	}
	if base != expectedBase {
		return "", deliveryError("target_moved", "target branch moved; candidate requires renewed checks and review", base, nil)
	}
	return m.publishCandidate(ctx, r, p, p.Branch, a.CandidateSHA, expectedBase)
}
