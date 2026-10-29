// Package config loads operator-owned policy, never policy from an agent checkout.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"openfactory/internal/model"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)
var githubRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func Load(path string) (model.Config, error) {
	var c model.Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if len(data) > 1<<20 {
		return c, errors.New("configuration exceeds 1 MiB")
	}
	var document yaml.Node
	if err = yaml.Unmarshal(data, &document); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	if err = validateDiscoveryYAML(&document, "", make(map[*yaml.Node]bool)); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	if err = validateIntakeYAML(&document, "", make(map[*yaml.Node]bool)); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	root := filepath.Dir(abs)
	expandPath := func(s string) string {
		if s == "" {
			return ""
		}
		s = os.ExpandEnv(s)
		if s == "" {
			return ""
		}
		if !filepath.IsAbs(s) {
			s = filepath.Join(root, s)
		}
		return filepath.Clean(s)
	}
	if c.StateDir == "" {
		c.StateDir = "state"
	}
	if c.WorkspaceDir == "" {
		c.WorkspaceDir = "workspaces"
	}
	c.StateDir, c.WorkspaceDir = expandPath(c.StateDir), expandPath(c.WorkspaceDir)
	c.CheckSocket = expandPath(c.CheckSocket)
	if c.ReleaseDir == "" {
		c.ReleaseDir = filepath.Join(root, "releases")
	}
	c.ReleaseDir = expandPath(c.ReleaseDir)
	c.BBURL = os.ExpandEnv(c.BBURL)
	if c.BBURL == "" {
		c.BBURL = "http://127.0.0.1:38886"
	}
	c.BBHostAuthority = os.ExpandEnv(c.BBHostAuthority)
	if c.BBHostAuthority == "" {
		c.BBHostAuthority = os.Getenv("OPENFACTORY_BB_HOST_AUTHORITY")
	}
	c.AstraModel, c.FastModel = os.ExpandEnv(c.AstraModel), os.ExpandEnv(c.FastModel)
	c.BBHostID = os.ExpandEnv(c.BBHostID)
	c.WebhookURL, c.WebhookSecretFile = os.ExpandEnv(c.WebhookURL), expandPath(c.WebhookSecretFile)
	if c.PollSeconds == 0 {
		c.PollSeconds = 10
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 2
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.MaxCallsPerDay == 0 {
		c.MaxCallsPerDay = 30
	}
	if c.AttemptTimeoutSeconds == 0 {
		c.AttemptTimeoutSeconds = 1800
	}
	if c.MaxPromptBytes == 0 {
		c.MaxPromptBytes = 24000
	}
	if c.GitName == "" {
		c.GitName = "OpenFactory"
	}
	if c.GitEmail == "" {
		c.GitEmail = "openfactory@localhost"
	}
	c.Discovery, err = normalizeDiscovery(c.Discovery)
	if err != nil {
		return c, err
	}
	c.Intake, err = normalizeIntake(c.Intake)
	if err != nil {
		return c, err
	}
	if c.Projects == nil {
		c.Projects = map[string]model.Project{}
	}
	for name, p := range c.Projects {
		p.Auth.TokenFile, p.Auth.SSHKeyFile, p.Auth.KnownHostsFile = expandPath(p.Auth.TokenFile), expandPath(p.Auth.SSHKeyFile), expandPath(p.Auth.KnownHostsFile)
		p.Repository = os.ExpandEnv(p.Repository)
		if p.Repository != "" && !strings.Contains(p.Repository, ":") {
			p.Repository = expandPath(p.Repository)
		}
		if p.Auth.Mode == "" {
			p.Auth.Mode = "none"
		}
		if p.Branch == "" {
			p.Branch = "main"
		}
		if p.MaxConcurrent == 0 {
			p.MaxConcurrent = 1
		}
		p.ProtectedPaths = append([]string{".github/workflows/", ".openfactory/", ".omp/", ".agents/", ".claude/", ".bb-env-setup.sh", ".bb-env-teardown.sh", ".worktreeinclude"}, p.ProtectedPaths...)
		c.Projects[name] = p
	}
	if err = Validate(c); err != nil {
		return c, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	sum := sha256.Sum256(raw)
	c.PolicyHash = hex.EncodeToString(sum[:])
	return c, nil
}

func Validate(c model.Config) error {
	if c.StateDir == "" || c.WorkspaceDir == "" || c.StateDir == c.WorkspaceDir {
		return errors.New("distinct state_dir and workspace_dir are required")
	}
	if within(c.WorkspaceDir, c.StateDir) || within(c.StateDir, c.WorkspaceDir) {
		return errors.New("state and workspace directories must not contain each other")
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 32 || c.MaxAttempts < 1 || c.MaxAttempts > 20 || c.MaxCallsPerDay < 1 || c.MaxCallsPerDay > 10000 {
		return errors.New("invalid concurrency, attempts or daily call limit")
	}
	if c.PollSeconds < 1 || c.PollSeconds > 300 || c.AttemptTimeoutSeconds < 10 || c.AttemptTimeoutSeconds > 14400 || c.MaxPromptBytes < 1024 || c.MaxPromptBytes > 262144 {
		return errors.New("invalid polling, timeout or prompt size")
	}
	if strings.ContainsAny(c.GitName+c.GitEmail, "\x00\r\n") {
		return errors.New("invalid git identity")
	}
	u, err := url.Parse(c.BBURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("bb_url must be an HTTP(S) base URL without credentials or query")
	}
	if err := validateBBHostAuthority(c.BBHostAuthority); err != nil {
		return err
	}
	if c.WebhookURL != "" {
		w, e := url.Parse(c.WebhookURL)
		if e != nil || w.Scheme != "https" || w.Host == "" || w.User != nil || w.Fragment != "" || c.WebhookSecretFile == "" {
			return errors.New("webhook requires an HTTPS URL and webhook_secret_file")
		}
	}
	if _, err := discoverySettings(c.Discovery); err != nil {
		return err
	}
	if _, err := normalizeIntake(c.Intake); err != nil {
		return err
	}
	for name, p := range c.Projects {
		if !identifier.MatchString(name) {
			return fmt.Errorf("invalid project name %q", name)
		}
		if p.MaxConcurrent < 1 || p.MaxConcurrent > 32 {
			return fmt.Errorf("project %s max_concurrent must be between 1 and 32", name)
		}
		if !p.Trusted {
			return fmt.Errorf("project %s must explicitly set trusted: true", name)
		}
		if p.Repository == "" || strings.ContainsAny(p.Repository, "\x00\r\n") || strings.HasPrefix(p.Repository, "-") {
			return fmt.Errorf("project %s has invalid repository", name)
		}
		if u, e := url.Parse(p.Repository); e == nil && u.Scheme != "" && u.User != nil && u.Scheme != "ssh" {
			return fmt.Errorf("project %s: credentials must not appear in repository URL", name)
		}
		if p.Branch == "" || strings.HasPrefix(p.Branch, "-") || strings.ContainsAny(p.Branch, " \x00\r\n~^:?*[\\") || strings.Contains(p.Branch, "..") || strings.Contains(p.Branch, "@{") {
			return fmt.Errorf("project %s has invalid branch", name)
		}
		switch p.Auth.Mode {
		case "none":
		case "token":
			if p.Auth.TokenFile == "" || !strings.HasPrefix(p.Repository, "https://") {
				return fmt.Errorf("project %s token auth requires HTTPS and token_file", name)
			}
		case "ssh":
			if p.Auth.SSHKeyFile == "" || p.Auth.KnownHostsFile == "" {
				return fmt.Errorf("project %s SSH requires ssh_key_file and known_hosts_file", name)
			}
		default:
			return fmt.Errorf("project %s has unknown auth mode", name)
		}
		if p.GitHubRepository != "" && !githubRepository.MatchString(p.GitHubRepository) {
			return fmt.Errorf("project %s invalid github_repository", name)
		}
		identity, err := RepositoryIdentity(p)
		if err != nil {
			return fmt.Errorf("project %s repository identity: %w", name, err)
		}
		if p.GitHubRepository != "" && identity != "github.com/"+strings.ToLower(p.GitHubRepository) {
			return fmt.Errorf("project %s github_repository must match its GitHub repository", name)
		}
		for id, cmd := range p.Checks {
			if !identifier.MatchString(id) {
				return fmt.Errorf("invalid check name %q", id)
			}
			if err := validateCommand(cmd); err != nil {
				return fmt.Errorf("project %s check %s: %w", name, id, err)
			}
		}
		for target, r := range p.Releases {
			if target != "staging" && target != "production" {
				return fmt.Errorf("project %s invalid release target", name)
			}
			if !r.Allowed {
				continue
			}
			if !r.Qualified || r.ExpiresAt.IsZero() || len(r.AllowedRisks) == 0 || len(r.AllowedPaths) == 0 {
				return fmt.Errorf("project %s release %s requires qualified, expiry, risks and paths", name, target)
			}
			if r.ObservationSeconds < 1 || r.ObservationSeconds > 3600 || r.MinimumObservations < 1 || r.MinimumObservations > 100 {
				return fmt.Errorf("project %s invalid release observation envelope", name)
			}
			for _, cmd := range []model.Command{r.Deploy, r.Observe, r.Rollback} {
				if err := validateCommand(cmd); err != nil {
					return fmt.Errorf("project %s release %s: %w", name, target, err)
				}
				if !filepath.IsAbs(cmd.Argv[0]) || !within(c.ReleaseDir, cmd.Argv[0]) || cmd.Argv[0] == c.ReleaseDir {
					return fmt.Errorf("project %s release %s requires an absolute operator executable under release_dir", name, target)
				}
				if within(c.WorkspaceDir, c.ReleaseDir) || within(c.ReleaseDir, c.WorkspaceDir) {
					return errors.New("release_dir must not overlap shared workspaces")
				}
				if cmd.TimeoutSeconds > 3600 {
					return fmt.Errorf("project %s release %s command timeout exceeds 3600 seconds", name, target)
				}
			}
		}
	}
	return nil
}

func validateCommand(c model.Command) error {
	if len(c.Argv) == 0 || c.Argv[0] == "" || len(c.Argv) > 128 {
		return errors.New("command argv is required")
	}
	for _, a := range c.Argv {
		if strings.ContainsRune(a, 0) {
			return errors.New("NUL in command argument")
		}
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 14400 {
		return errors.New("command timeout must be 0..14400 seconds")
	}
	return nil
}
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RepositoryIdentity identifies the repository independently of project aliases
// and authentication. GitHub HTTPS/SSH/scp spellings share one identity; local
// paths and file URLs resolve symlinks, including an existing parent of a path
// that has not been created yet.
func RepositoryIdentity(p model.Project) (string, error) {
	repository := p.Repository
	if repository == "" || strings.ContainsAny(repository, "\x00\r\n") || strings.HasPrefix(repository, "-") {
		return "", errors.New("invalid repository")
	}
	if !strings.Contains(repository, "://") {
		// Git's scp-style remote has a colon before its repository path.
		if colon := strings.IndexByte(repository, ':'); colon > 0 && !strings.ContainsAny(repository[:colon], `/\`) {
			host := repository[:colon]
			if at := strings.LastIndexByte(host, '@'); at >= 0 {
				host = host[at+1:]
			}
			repository = "ssh://" + host + "/" + strings.TrimPrefix(repository[colon+1:], "/")
		} else {
			return localRepositoryIdentity(repository)
		}
	}
	u, err := url.Parse(repository)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("repository URL must not contain query or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "file" {
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") || u.User != nil || !filepath.IsAbs(u.Path) {
			return "", errors.New("file repository must be an absolute local path")
		}
		return localRepositoryIdentity(u.Path)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || u.Path == "" || u.Path == "/" {
		return "", errors.New("repository URL requires host and path")
	}
	if u.User != nil && scheme != "ssh" {
		return "", errors.New("repository URL must not contain credentials")
	}
	port := u.Port()
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" || scheme == "ssh" && port == "22" || scheme == "git" && port == "9418" {
		port = ""
	}
	path := strings.Trim(strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git"), "/")
	if path == "" || strings.Contains(path, "//") {
		return "", errors.New("invalid repository path")
	}
	for _, component := range strings.Split(path, "/") {
		if component == "." || component == ".." {
			return "", errors.New("invalid repository path component")
		}
	}
	if host == "github.com" && port == "" {
		if !githubRepository.MatchString(path) {
			return "", errors.New("GitHub repository requires owner/name")
		}
		return host + "/" + strings.ToLower(path), nil
	}
	u = &url.URL{Scheme: scheme, Host: strings.ToLower(u.Host), Path: "/" + path}
	if port == "" {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	return u.String(), nil
}

func localRepositoryIdentity(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return (&url.URL{Scheme: "file", Path: resolved}).String(), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// IntegrationResources returns the complete durable delivery lock set. A
// release adds an environment resource, never additional release authority.
func IntegrationResources(p model.Project, target string) ([]string, error) {
	identity, err := RepositoryIdentity(p)
	if err != nil {
		return nil, err
	}
	if p.Branch == "" || strings.HasPrefix(p.Branch, "-") || strings.ContainsAny(p.Branch, " \x00\r\n~^:?*[\\") || strings.Contains(p.Branch, "..") || strings.Contains(p.Branch, "@{") {
		return nil, errors.New("invalid integration branch")
	}
	resources := []string{"git:" + identity + ":refs/heads/" + p.Branch}
	if target == "staging" || target == "production" {
		resources = append(resources, "release:"+identity+":"+target)
	}
	sort.Strings(resources)
	return resources, nil
}
