package gitrepo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
)

var _ model.DiscoveryRepositories = (*Manager)(nil)

type discoveryReceipt struct {
	Workspace                                        model.DiscoveryWorkspace
	Project, Repository, Target, Attempt, Role, Step string
}

func (m *Manager) discoveryRecords() string {
	return filepath.Join(m.cfg.StateDir, "discovery-records")
}
func (m *Manager) discoveryReceiptPath(path string) string {
	return filepath.Join(m.discoveryRecords(), digest(path)+".json")
}
func (m *Manager) discoveryLimits() model.BundleLimits {
	p := m.cfg.Discovery
	l := model.BundleLimits{MaxFiles: 128, MaxFileBytes: 8 << 20, MaxTotalBytes: 64 << 20}
	if p.BundleMaxFiles > 0 && p.BundleMaxFiles < l.MaxFiles {
		l.MaxFiles = p.BundleMaxFiles
	}
	if p.BundleMaxFileBytes > 0 && p.BundleMaxFileBytes < l.MaxFileBytes {
		l.MaxFileBytes = p.BundleMaxFileBytes
	}
	if p.BundleMaxTotalBytes > 0 && p.BundleMaxTotalBytes < l.MaxTotalBytes {
		l.MaxTotalBytes = p.BundleMaxTotalBytes
	}
	return l
}

func (m *Manager) PrepareDiscovery(ctx context.Context, name string, p model.Project, attempt, role, step string) (model.DiscoveryWorkspace, error) {
	defer m.lock("discovery-project:" + name)()
	var zero model.DiscoveryWorkspace
	if err := m.enrolled(name, p); err != nil {
		return zero, err
	}
	if name == "" || attempt == "" || role == "" || step == "" {
		return zero, errors.New("discovery identity required")
	}
	if err := CheckCapabilities(ctx); err != nil {
		return zero, err
	}
	if err := os.MkdirAll(m.cfg.WorkspaceDir, 0700); err != nil {
		return zero, err
	}
	root, err := filepath.EvalSymlinks(m.cfg.WorkspaceDir)
	if err != nil {
		return zero, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return zero, err
	}
	if err = os.MkdirAll(m.discoveryRecords(), 0700); err != nil {
		return zero, err
	}
	state, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err != nil {
		return zero, err
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return zero, err
	}
	if state == root || strings.HasPrefix(state, root+string(filepath.Separator)) {
		return zero, errors.New("discovery state must be private")
	}
	// BB flattens cwd into a single session-directory component. Four full
	// SHA256 values exceed NAME_MAX; 128-bit path identities retain the complete
	// raw identity in the private receipt and fail closed on any collision.
	project := filepath.Join(root, "discovery", digest(name)[:32])
	path := filepath.Join(project, digest(role)[:32], digest(step)[:32], digest(attempt)[:32])
	// OMP 18.10.3 session/session-paths.ts encodes an absolute cwd as
	// "--" + cwd without its leading slash (separators become "-") + "--".
	// Bound that maximum in bytes: the model's home/tmp roots may differ from
	// the controller's, so their shorter relative encodings are not guaranteed.
	if len(path)+3 > 255 {
		return zero, &model.ProgramError{Code: "prerequisite_missing", Message: "discovery cwd exceeds OMP's 255-byte flattened session directory limit; shorten the operator WorkspaceDir"}
	}
	if b, e := os.ReadFile(m.discoveryReceiptPath(path)); e == nil {
		var r discoveryReceipt
		if e = json.Unmarshal(b, &r); e != nil {
			return zero, e
		}
		if r.Project != name || r.Repository != p.Repository || r.Target != p.Branch || r.Attempt != attempt || r.Role != role || r.Step != step {
			return zero, errors.New("discovery enrollment changed")
		}
		if r.Workspace.Path != path {
			return zero, errors.New("discovery workspace path mismatch")
		}
		chain, e := m.openDiscovery(r.Workspace)
		if e != nil {
			return zero, e
		}
		defer chain.close()
		return r.Workspace, chain.verify()
	} else if !os.IsNotExist(e) {
		return zero, e
	}
	common := filepath.Join(state, "discovery-git", digest(name)+".git")
	cacheRecord := filepath.Join(m.discoveryRecords(), "cache-"+digest(name)+".json")
	var cache record
	if b, e := os.ReadFile(cacheRecord); e == nil {
		if e = json.Unmarshal(b, &cache); e != nil {
			return zero, e
		}
		if cache.Repository != p.Repository || cache.Target != p.Branch {
			return zero, errors.New("discovery cache enrollment changed")
		}
		if e = verifyConfig(common, cache.ConfigHash); e != nil {
			return zero, e
		}
		if !shaPattern.MatchString(cache.Base) {
			return zero, errors.New("discovery baseline receipt missing")
		}
	} else if !os.IsNotExist(e) {
		return zero, e
	} else {
		if _, e = os.Lstat(common); !os.IsNotExist(e) {
			return zero, errors.New("unrecorded discovery cache")
		}
		if e = os.MkdirAll(filepath.Dir(common), 0700); e != nil {
			return zero, e
		}
		if _, e = m.git(ctx, "", "init", "--bare", common); e != nil {
			return zero, e
		}
		hash, e := configHash(common)
		if e != nil {
			return zero, e
		}
		tree, e := m.git(ctx, common, "mktree")
		if e != nil {
			return zero, e
		}
		baseline, e := m.git(ctx, common, "-c", "user.name=OpenFactory", "-c", "user.email=openfactory@localhost", "commit-tree", strings.TrimSpace(tree), "-m", "Empty discovery baseline")
		if e != nil {
			return zero, e
		}
		baseline = strings.TrimSpace(baseline)
		if _, e = m.git(ctx, common, "update-ref", "refs/discovery/baseline", baseline); e != nil {
			return zero, e
		}
		cache = record{Repository: p.Repository, Target: p.Branch, ConfigHash: hash, Base: baseline}
		if e = saveExclusive(cacheRecord, cache); e != nil {
			return zero, e
		}
	}
	// Discovery never fetches product history. BaseSHA identifies an empty,
	// controller-private baseline, not provenance from the enrolled target.
	base := cache.Base
	// A stable empty source root is BB's project identity, not a product checkout.
	source := filepath.Join(state, "discovery-sources", digest(name))
	sourceRecord := filepath.Join(m.discoveryRecords(), "source-"+digest(name)+".json")
	if data, e := os.ReadFile(sourceRecord); e == nil {
		var receipt record
		if e = json.Unmarshal(data, &receipt); e != nil {
			return zero, e
		}
		if receipt.Path != source || receipt.Repository != p.Repository || receipt.Target != p.Branch {
			return zero, errors.New("discovery source enrollment changed")
		}
		info, e := os.Lstat(source)
		if e != nil {
			return zero, e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return zero, errors.New("invalid discovery source root")
		}
		entries, e := os.ReadDir(source)
		if e != nil {
			return zero, e
		}
		if len(entries) != 0 {
			return zero, errors.New("discovery source root is not empty")
		}
	} else if !os.IsNotExist(e) {
		return zero, e
	} else {
		if _, e = os.Lstat(source); !os.IsNotExist(e) {
			return zero, errors.New("unrecorded discovery source exists")
		}
		if err = os.MkdirAll(filepath.Dir(source), 0700); err != nil {
			return zero, err
		}
		if err = os.Mkdir(source, 0700); err != nil {
			return zero, err
		}
		if e = saveExclusive(sourceRecord, record{Path: source, Repository: p.Repository, Target: p.Branch, Base: base}); e != nil {
			return zero, e
		}
	}
	shared, relative, err := m.sharedRoot(path)
	if err != nil {
		return zero, err
	}
	defer shared.Close()
	if err = shared.MkdirAll(filepath.Dir(relative), 0700); err != nil {
		return zero, err
	}
	if err = shared.Mkdir(relative, 0700); err != nil {
		return zero, err
	}
	w := model.DiscoveryWorkspace{Path: path, ProjectRoot: source, BaseSHA: base}
	r := discoveryReceipt{Workspace: w, Project: name, Repository: p.Repository, Target: p.Branch, Attempt: attempt, Role: role, Step: step}
	if err = saveExclusive(m.discoveryReceiptPath(path), r); err != nil {
		return zero, err
	}
	return w, nil
}

// Every component is held open and compared with its directory entry again after
// IO. os.Root prevents escapes; identity checks additionally refuse internal links
// and directory replacements rather than silently reading a different snapshot.
type discoveryChain struct {
	roots []*os.Root
	names []string
	infos []os.FileInfo
}

func (c *discoveryChain) close() {
	for i := len(c.roots) - 1; i >= 0; i-- {
		c.roots[i].Close()
	}
}
func (c *discoveryChain) verify() error {
	for i, name := range c.names {
		info, err := c.roots[i].Lstat(name)
		if err != nil {
			return err
		}
		if !os.SameFile(info, c.infos[i]) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("discovery directory changed")
		}
	}
	return nil
}
func (c *discoveryChain) descend(name string) error {
	parent := c.roots[len(c.roots)-1]
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("discovery directory is not a real directory")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		child.Close()
		return errors.New("discovery directory raced")
	}
	c.names = append(c.names, name)
	c.infos = append(c.infos, info)
	c.roots = append(c.roots, child)
	return c.verify()
}
func (m *Manager) openDiscovery(w model.DiscoveryWorkspace) (*discoveryChain, error) {
	b, err := os.ReadFile(m.discoveryReceiptPath(w.Path))
	if err != nil {
		return nil, err
	}
	var r discoveryReceipt
	if err = json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Workspace != w {
		return nil, errors.New("discovery workspace receipt mismatch")
	}
	root, relative, err := m.sharedRoot(w.Path)
	if err != nil {
		return nil, err
	}
	c := &discoveryChain{roots: []*os.Root{root}}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if err = c.descend(part); err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

func (m *Manager) ConfigureDiscoverySession(ctx context.Context, w model.DiscoveryWorkspace, s model.DiscoverySession) error {
	defer m.lock("discovery:" + w.Path)()
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := os.ReadFile(m.discoveryReceiptPath(w.Path))
	if err != nil {
		return err
	}
	var receipt discoveryReceipt
	if err = json.Unmarshal(b, &receipt); err != nil {
		return err
	}
	if s.AttemptID != receipt.Attempt || s.Role != receipt.Role || s.StepID != receipt.Step {
		return errors.New("discovery session identity mismatch")
	}
	c, err := m.openDiscovery(w)
	if err != nil {
		return err
	}
	defer c.close()
	if err = materializeDiscoveryInputs(ctx, c, s); err != nil {
		return err
	}
	root := c.roots[len(c.roots)-1]
	if err = root.Mkdir(".openfactory", 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err = c.descend(".openfactory"); err != nil {
		return err
	}
	root = c.roots[len(c.roots)-1]
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// Exclusive random temporary file plus rooted rename never follows an existing
	// controlfile symlink and never truncates a hard-linked file.
	name := ".session-" + rand.Text()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if old, e := root.Lstat("discovery-session.json"); e == nil && !old.Mode().IsRegular() {
		return errors.New("invalid discovery controlfile")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	if err = c.verify(); err != nil {
		return err
	}
	if err = root.Rename(name, "discovery-session.json"); err != nil {
		return err
	}
	if err = c.verify(); err != nil {
		root.Remove("discovery-session.json")
		return err
	}
	return nil
}

func discoveryMember(name string) error {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\\") || filepath.ToSlash(filepath.Clean(name)) != name {
		return errors.New("invalid bundle path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "." || part == ".git" || part == ".openfactory" || part == "" {
			return errors.New("bundle path contains reserved component")
		}
	}
	return nil
}

func (m *Manager) FreezeDiscoveryBundle(ctx context.Context, w model.DiscoveryWorkspace, declared []string) (model.ProgramBundle, error) {
	defer m.lock("discovery:" + w.Path)()
	var zero model.ProgramBundle
	limits := m.discoveryLimits()
	if len(declared) > limits.MaxFiles {
		return zero, errors.New("too many bundle files")
	}
	staging, err := os.MkdirTemp(m.discoveryRecords(), "bundle-")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(staging)
	staging, err = filepath.EvalSymlinks(staging)
	if err != nil {
		return zero, err
	}
	var total int64
	seen := map[string]bool{}
	for _, name := range declared {
		if err = discoveryMember(name); err != nil {
			return zero, err
		}
		if seen[name] {
			return zero, errors.New("duplicate bundle path")
		}
		seen[name] = true
		c, e := m.openDiscovery(w)
		if e != nil {
			return zero, e
		}
		e = func() error {
			defer c.close()
			parts := strings.Split(name, "/")
			for _, part := range parts[:len(parts)-1] {
				if e := c.descend(part); e != nil {
					return e
				}
			}
			root := c.roots[len(c.roots)-1]
			leaf := parts[len(parts)-1]
			before, e := root.Lstat(leaf)
			if e != nil {
				return e
			}
			if !before.Mode().IsRegular() || before.Size() > limits.MaxFileBytes {
				return errors.New("invalid or oversized bundle file")
			}
			f, e := root.OpenFile(leaf, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if e != nil {
				return e
			}
			defer f.Close()
			opened, e := f.Stat()
			if e != nil {
				return e
			}
			if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
				return errors.New("bundle file raced")
			}
			data, e := io.ReadAll(io.LimitReader(contextReader{ctx, f}, limits.MaxFileBytes+1))
			if e != nil {
				return e
			}
			after, e := f.Stat()
			if e != nil {
				return e
			}
			current, e := root.Lstat(leaf)
			if e != nil {
				return e
			}
			if !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || int64(len(data)) != opened.Size() {
				return errors.New("bundle file changed during capture")
			}
			if e = c.verify(); e != nil {
				return e
			}
			total += int64(len(data))
			if int64(len(data)) > limits.MaxFileBytes || total > limits.MaxTotalBytes {
				return errors.New("bundle byte limit exceeded")
			}
			dest := filepath.Join(staging, filepath.FromSlash(name))
			if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
				return e
			}
			return os.WriteFile(dest, data, 0600)
		}()
		if e != nil {
			return zero, fmt.Errorf("freeze %s: %w", name, e)
		}
	}
	state, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err != nil {
		return zero, err
	}
	return artifacts.New(filepath.Join(state, "artifacts")).ImportBundle(ctx, staging, declared, limits)
}

func (m *Manager) MaterializeExperiment(ctx context.Context, a model.Attempt, bundle model.ProgramBundle) (model.Workspace, error) {
	defer m.lock("experiment:" + a.ID)()
	var zero model.Workspace
	if a.ID == "" {
		return zero, errors.New("experiment attempt required")
	}
	for _, file := range bundle.Files {
		if err := discoveryMember(file.Path); err != nil {
			return zero, err
		}
	}
	if err := os.MkdirAll(m.discoveryRecords(), 0700); err != nil {
		return zero, err
	}
	staging, err := os.MkdirTemp(m.discoveryRecords(), "experiment-")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(staging)
	staging, err = filepath.EvalSymlinks(staging)
	if err != nil {
		return zero, err
	}
	source := filepath.Join(staging, "snapshot")
	state, err := filepath.EvalSymlinks(m.cfg.StateDir)
	if err != nil {
		return zero, err
	}
	if err = artifacts.New(filepath.Join(state, "artifacts")).MaterializeBundle(ctx, bundle, source, m.discoveryLimits()); err != nil {
		return zero, err
	}
	root, err := filepath.EvalSymlinks(m.cfg.WorkspaceDir)
	if err != nil {
		return zero, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return zero, err
	}
	path := filepath.Join(root, "discovery", "experiments", digest(a.ID), digest(bundle.ManifestSHA), rand.Text())
	shared, relative, err := m.sharedRoot(path)
	if err != nil {
		return zero, err
	}
	defer shared.Close()
	// Never reuse agent-mutable bytes, even when the frozen identity is identical.
	if err = shared.MkdirAll(filepath.Dir(relative), 0700); err != nil {
		return zero, err
	}
	if err = m.copyCheckout(ctx, source, path); err != nil {
		return zero, err
	}
	return model.Workspace{Path: path, BaseSHA: a.BaseSHA}, nil
}

// FingerprintDiscovery guards read-only analysis without putting repository
// contents or controller capabilities into CAS. Only Git metadata and the exact
// controller-managed session file are excluded.
func (m *Manager) FingerprintDiscovery(ctx context.Context, w model.DiscoveryWorkspace) (string, error) {
	defer m.lock("discovery:" + w.Path)()
	c, err := m.openDiscovery(w)
	if err != nil {
		return "", err
	}
	defer c.close()
	h := sha256.New()
	encoder := json.NewEncoder(h)
	buffer := make([]byte, 32<<10)
	count := 0
	var total int64
	var walk func(string) error
	walk = func(prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		root := c.roots[len(c.roots)-1]
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, err := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			name := entry.Name()
			path := name
			if prefix != "" {
				path = prefix + "/" + name
			}
			if name == ".git" || path == ".openfactory/discovery-session.json" {
				continue
			}
			count++
			if count > 100000 {
				return errors.New("discovery fingerprint file count exceeded")
			}
			before, err := root.Lstat(name)
			if err != nil {
				return err
			}
			if !before.IsDir() && !before.Mode().IsRegular() {
				return errors.New("unsupported discovery fingerprint member")
			}
			if err = encoder.Encode(struct {
				Path string
				Mode uint32
			}{path, uint32(before.Mode())}); err != nil {
				return err
			}
			if before.IsDir() {
				if err = c.descend(name); err != nil {
					return err
				}
				err = walk(path)
				child := c.roots[len(c.roots)-1]
				child.Close()
				c.roots = c.roots[:len(c.roots)-1]
				c.names = c.names[:len(c.names)-1]
				c.infos = c.infos[:len(c.infos)-1]
				if err != nil {
					return err
				}
				continue
			}
			if before.Size() > 128<<20 {
				return errors.New("discovery fingerprint file exceeds byte bound")
			}
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			err = func() error {
				defer f.Close()
				opened, e := f.Stat()
				if e != nil {
					return e
				}
				if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
					return errors.New("discovery fingerprint file raced")
				}
				content := sha256.New()
				n, e := io.CopyBuffer(content, io.LimitReader(contextReader{ctx, f}, (128<<20)+1), buffer)
				if e != nil {
					return e
				}
				total += n
				if n > 128<<20 || total > 1<<30 {
					return errors.New("discovery fingerprint byte bound exceeded")
				}
				after, e := f.Stat()
				if e != nil {
					return e
				}
				current, e := root.Lstat(name)
				if e != nil {
					return e
				}
				if !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || n != opened.Size() {
					return errors.New("discovery fingerprint file changed")
				}
				return encoder.Encode(hex.EncodeToString(content.Sum(nil)))
			}()
			if err != nil {
				return err
			}
		}
		// Refuse concurrent additions/removals instead of returning a partial guard.
		dir, err = root.Open(".")
		if err != nil {
			return err
		}
		final, err := dir.ReadDir(-1)
		closeErr = dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(final, func(i, j int) bool { return final[i].Name() < final[j].Name() })
		if len(final) != len(entries) {
			return errors.New("discovery fingerprint directory changed")
		}
		for i := range entries {
			if entries[i].Name() != final[i].Name() {
				return errors.New("discovery fingerprint directory changed")
			}
		}
		return c.verify()
	}
	if err = walk(""); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
