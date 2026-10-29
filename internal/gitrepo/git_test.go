package gitrepo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openfactory/internal/model"
)

type fixture struct {
	m            *Manager
	p            model.Project
	a            model.Attempt
	seed, remote string
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(t.TempDir()), "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=OpenFactory Test", "GIT_CONFIG_KEY_1=user.email", "GIT_CONFIG_VALUE_1=test@localhost")
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v: %s", args, e, out)
	}
	return strings.TrimSpace(string(out))
}
func put(t *testing.T, path, text string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
}
func setup(t *testing.T, configure func(*model.Project)) *fixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	if e := os.Mkdir(seed, 0700); e != nil {
		t.Fatal(e)
	}
	testGit(t, root, "init", "--bare", remote)
	testGit(t, seed, "init", "-b", "main")
	put(t, filepath.Join(seed, "app.txt"), "baseline\n")
	testGit(t, seed, "add", "app.txt")
	testGit(t, seed, "commit", "-m", "baseline")
	testGit(t, seed, "push", remote, "HEAD:refs/heads/main")
	p := model.Project{Repository: remote, Branch: "main", Trusted: true, Auth: model.GitAuth{Mode: "none"}, Checks: map[string]model.Command{
		"environment": {Argv: []string{"sh", "-c", "env"}, TimeoutSeconds: 2},
		"timeout":     {Argv: []string{"sh", "-c", "sleep 30 & wait"}, TimeoutSeconds: 1},
		"output":      {Argv: []string{"sh", "-c", "yes x"}, TimeoutSeconds: 1},
		"redaction":   {Argv: []string{"sh", "-c", "printf sensitive-token"}},
	}}
	if configure != nil {
		configure(&p)
	}
	releaseDir := filepath.Join(root, "operator-releases")
	if e := os.Mkdir(releaseDir, 0700); e != nil {
		t.Fatal(e)
	}
	for target, profile := range p.Releases {
		for kind, command := range map[string]*model.Command{"deploy": &profile.Deploy, "observe": &profile.Observe, "rollback": &profile.Rollback} {
			var argv []string
			for _, arg := range command.Argv {
				argv = append(argv, quote(arg))
			}
			script := filepath.Join(releaseDir, target+"-"+kind)
			put(t, script, "#!/bin/sh\nexec "+strings.Join(argv, " ")+"\n")
			if e := os.Chmod(script, 0700); e != nil {
				t.Fatal(e)
			}
			command.Argv = []string{script}
		}
		p.Releases[target] = profile
	}
	cfg := model.Config{StateDir: filepath.Join(root, "state"), WorkspaceDir: filepath.Join(root, "workspaces"), ReleaseDir: releaseDir, Projects: map[string]model.Project{"sample": p}, GitName: "OpenFactory", GitEmail: "openfactory@localhost", PolicyHash: "policy"}
	m := New(cfg)
	w, e := m.Prepare(context.Background(), "sample", p, "attempt-one")
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{m: m, p: p, a: model.Attempt{ID: "attempt-one", FeatureID: "feature", Workspace: w.Path, BaseSHA: w.BaseSHA, Deadline: time.Now().Add(time.Hour)}, seed: seed, remote: remote}
}
func (f *fixture) candidate(t *testing.T) {
	t.Helper()
	put(t, filepath.Join(f.a.Workspace, "app.txt"), "candidate\n")
	sha, e := f.m.Commit(context.Background(), f.a.Workspace, "build candidate")
	if e != nil {
		t.Fatal(e)
	}
	f.a.CandidateSHA = sha
	w, e := f.m.Snapshot(context.Background(), f.a.Workspace, sha)
	if e != nil {
		t.Fatal(e)
	}
	f.a.Workspace = w.Path
}

func TestPrepareFingerprintAndFastForward(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	again, e := f.m.Prepare(ctx, "sample", f.p, f.a.ID)
	if e != nil || again.Path != f.a.Workspace || again.BaseSHA != f.a.BaseSHA {
		t.Fatalf("prepare not stable: %+v %v", again, e)
	}
	first, e := f.m.Fingerprint(ctx, f.a.Workspace)
	if e != nil {
		t.Fatal(e)
	}
	put(t, filepath.Join(f.a.Workspace, "new.txt"), "untracked review edit")
	second, e := f.m.Fingerprint(ctx, f.a.Workspace)
	if e != nil || first == second {
		t.Fatalf("untracked mutation not detected: %v", e)
	}
	if e = os.Remove(filepath.Join(f.a.Workspace, "new.txt")); e != nil {
		t.Fatal(e)
	}
	f.candidate(t)
	paths, e := f.m.ChangedPaths(ctx, f.a.Workspace, f.a.BaseSHA, f.a.CandidateSHA)
	if e != nil || len(paths) != 1 || paths[0] != "app.txt" {
		t.Fatalf("paths %v: %v", paths, e)
	}
	receipt, e := f.m.Integrate(ctx, "sample", f.p, f.a, f.a.BaseSHA)
	if e != nil || receipt != f.a.CandidateSHA {
		t.Fatalf("integrate %s: %v", receipt, e)
	}
	if got := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main"); !strings.HasPrefix(got, f.a.CandidateSHA) {
		t.Fatalf("remote not candidate: %s", got)
	}
	if _, e = f.m.Integrate(ctx, "sample", f.p, f.a, f.a.BaseSHA); e != nil {
		t.Fatalf("exact receipt reconcile: %v", e)
	}
	config, e := os.ReadFile(filepath.Join(f.m.cfg.StateDir, "git-repositories", digest("sample")+".git", "config"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(config), f.remote) || strings.Contains(string(config), "credential") {
		t.Fatal("repository persisted remote or auth")
	}
}
func TestRejectMovedRemoteWithoutOverwriting(t *testing.T) {
	f := setup(t, nil)
	f.candidate(t)
	put(t, filepath.Join(f.seed, "other.txt"), "concurrent user work")
	testGit(t, f.seed, "add", "other.txt")
	testGit(t, f.seed, "commit", "-m", "user update")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	before := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main")
	if _, e := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); e == nil {
		t.Fatal("accepted moved baseline")
	}
	if after := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main"); before != after {
		t.Fatal("overwrote remote work")
	}
}
func TestRejectConfigBranchAndProtectedPathMutations(t *testing.T) {
	t.Run("config", func(t *testing.T) {
		f := setup(t, nil)
		common := filepath.Join(f.m.cfg.StateDir, "git-repositories", digest("sample")+".git")
		file := filepath.Join(common, "config")
		out, e := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = out.WriteString("\n[credential]\n helper = malicious\n")
		_ = out.Close()
		if _, e = f.m.Fingerprint(context.Background(), f.a.Workspace); e == nil {
			t.Fatal("accepted altered git configuration")
		}
	})
	t.Run("shared branch has no authority", func(t *testing.T) {
		f := setup(t, nil)
		testGit(t, f.a.Workspace, "checkout", "-b", "user-branch")
		f.candidate(t)
		if _, e := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); e != nil {
			t.Fatalf("shared branch controlled controller candidate: %v", e)
		}
	})
	t.Run("protected", func(t *testing.T) {
		f := setup(t, func(p *model.Project) { p.ProtectedPaths = []string{"app.txt"} })
		f.candidate(t)
		if _, e := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); e == nil {
			t.Fatal("accepted protected change")
		}
	})
	t.Run("enrollment", func(t *testing.T) {
		f := setup(t, nil)
		f.candidate(t)
		changed := f.p
		changed.ProtectedPaths = []string{"new-policy"}
		if _, e := f.m.Integrate(context.Background(), "sample", changed, f.a, f.a.BaseSHA); e == nil {
			t.Fatal("accepted caller supplied policy")
		}
	})
}
func TestCheckEnvironmentTimeoutOutputAndRedaction(t *testing.T) {
	f := setup(t, nil)
	t.Setenv("OPENROUTER_API_KEY", "provider-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("SSH_AUTH_SOCK", "/fake/agent")
	t.Setenv("GIT_CONFIG_COUNT", "99")
	out, e := f.m.Check(context.Background(), f.a.Workspace, model.Command{Argv: []string{"sh", "-c", "env"}, TimeoutSeconds: 2})
	if e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{"provider-secret", "github-secret", "/fake/agent", "GIT_CONFIG_COUNT=99"} {
		if strings.Contains(out, secret) {
			t.Fatalf("inherited credential: %s", secret)
		}
	}
	started := time.Now()
	_, e = f.m.Check(context.Background(), f.a.Workspace, model.Command{Argv: []string{"sh", "-c", "sleep 30 & wait"}, TimeoutSeconds: 1})
	if e == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("timeout not bounded: %v", e)
	}
	out, e = f.m.Check(context.Background(), f.a.Workspace, model.Command{Argv: []string{"sh", "-c", "yes x"}, TimeoutSeconds: 1})
	if e == nil || len(out) > outputLimit+100 {
		t.Fatalf("output not bounded: %d %v", len(out), e)
	}
	token := filepath.Join(t.TempDir(), "token")
	put(t, token, "sensitive-token")
	p := f.p
	p.Auth.TokenFile = token
	f.m.cfg.Projects["sample"] = p
	out, e = f.m.Check(context.Background(), f.a.Workspace, model.Command{Argv: []string{"sh", "-c", "printf sensitive-token"}})
	if e != nil || strings.Contains(out, "sensitive-token") {
		t.Fatalf("token not redacted: %q %v", out, e)
	}
	if _, e = f.m.Check(context.Background(), f.a.Workspace, model.Command{Argv: []string{"sh", "-c", "echo unapproved"}}); e == nil {
		t.Fatal("ran an unenrolled command")
	}
}
func TestAuthenticationRejectsUnsafeCredentialInputs(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	put(t, token, "secret")
	m := New(model.Config{})
	p := model.Project{Repository: "https://github.com/owner/repo.git", Auth: model.GitAuth{Mode: "token", TokenFile: token}}
	env, secrets, e := m.auth(p)
	if e != nil {
		t.Fatal(e)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:secret"))
	if len(env) != 3 || len(secrets) != 2 || !strings.Contains(env[2], encoded) {
		t.Fatal("missing command-scoped auth")
	}
	p.Repository = "https://secret@github.com/owner/repo.git"
	if _, _, e = m.auth(p); e == nil {
		t.Fatal("accepted credential URL")
	}
	p.Repository = "https://example.com/owner/repo.git"
	if _, _, e = m.auth(p); e == nil {
		t.Fatal("sent GitHub token to other host")
	}
	if e = os.Chmod(token, 0644); e != nil {
		t.Fatal(e)
	}
	p.Repository = "https://github.com/owner/repo.git"
	if _, _, e = m.auth(p); e == nil {
		t.Fatal("accepted public token file")
	}
	key := filepath.Join(root, "key")
	known := filepath.Join(root, "known_hosts")
	put(t, key, "private key")
	put(t, known, "github.com pinned-host-key")
	p = model.Project{Repository: "git@github.com:owner/repo.git", Auth: model.GitAuth{Mode: "ssh", SSHKeyFile: key, KnownHostsFile: known}}
	env, _, e = m.auth(p)
	if e != nil || !strings.Contains(env[0], "StrictHostKeyChecking=yes") || !strings.Contains(env[0], "IdentitiesOnly=yes") {
		t.Fatalf("SSH not pinned: %v %v", env, e)
	}
}
func TestPullRequestReconcilesLostAcknowledgement(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	put(t, token, "test-token")
	f := setup(t, func(p *model.Project) { p.GitHubRepository = "owner/repo"; p.Auth.TokenFile = token })
	f.candidate(t)
	var mu sync.Mutex
	created := false
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing API auth")
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path != "/repos/owner/repo/pulls" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		if r.Method == http.MethodPost {
			posts++
			created = true
			http.Error(w, "acknowledgement lost", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !created {
			_, _ = w.Write([]byte("[]"))
			return
		}
		branch := "openfactory/" + digest("sample\x00"+f.a.ID)
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"html_url": "https://github.com/owner/repo/pull/1", "state": "open", "head": map[string]string{"ref": branch, "sha": f.a.CandidateSHA}, "base": map[string]string{"ref": "main", "sha": f.a.BaseSHA}}})
	}))
	defer server.Close()
	f.m.apiURL = server.URL
	feature := model.Feature{ID: f.a.FeatureID, Project: "sample", Title: "Feature", Revision: "approved-revision"}
	for range 2 {
		got, e := f.m.PullRequest(context.Background(), "sample", f.p, f.a, feature)
		if e != nil || got != "https://github.com/owner/repo/pull/1" {
			t.Fatalf("PR reconcile: %s %v", got, e)
		}
	}
	if posts != 1 {
		t.Fatalf("duplicate PR creations: %d", posts)
	}
}
func TestReleaseFailurePreservesRollbackReceipts(t *testing.T) {
	rollbackFile := filepath.Join(t.TempDir(), "rolled-back")
	f := setup(t, func(p *model.Project) {
		p.Releases = map[string]model.Release{"staging": {Allowed: true, Qualified: true, AllowedRisks: []string{"low"}, AllowedPaths: []string{"app.txt"}, ExpiresAt: time.Now().Add(time.Hour), Deploy: model.Command{Argv: []string{"sh", "-c", "test -n \"$OPENFACTORY_SHA\""}}, Observe: model.Command{Argv: []string{"sh", "-c", "echo unhealthy; exit 1"}}, Rollback: model.Command{Argv: []string{"sh", "-c", fmt.Sprintf("printf rollback > %s", quote(rollbackFile))}}, ObservationSeconds: 1, MinimumObservations: 1}}
	})
	f.candidate(t)
	if _, e := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); e != nil {
		t.Fatal(e)
	}
	feature := model.Feature{ID: f.a.FeatureID, Project: "sample", Risk: "low", CompletionTarget: "staging"}
	evidence, e := f.m.Release(context.Background(), "sample", f.p, f.a, feature)
	if e == nil || len(evidence) != 3 || evidence[1].Passed || !evidence[2].Passed {
		t.Fatalf("missing failure/rollback receipts: %+v %v", evidence, e)
	}
	if _, e = os.Stat(rollbackFile); e != nil {
		t.Fatal("rollback not executed")
	}
	feature.Risk = "high"
	evidence, e = f.m.Release(context.Background(), "sample", f.p, f.a, feature)
	if e == nil || len(evidence) != 0 {
		t.Fatal("unapproved release risk executed commands")
	}
}
func TestReleaseRejectsExpiryPathsAndUnintegratedCandidate(t *testing.T) {
	for _, scenario := range []string{"expiry", "path", "qualification", "unintegrated"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t, func(p *model.Project) {
				r := model.Release{Allowed: true, Qualified: true, AllowedRisks: []string{"low"}, AllowedPaths: []string{"app.txt"}, ExpiresAt: time.Now().Add(time.Hour), Deploy: model.Command{Argv: []string{"true"}}, Observe: model.Command{Argv: []string{"true"}}, Rollback: model.Command{Argv: []string{"true"}}, ObservationSeconds: 1, MinimumObservations: 1}
				switch scenario {
				case "expiry":
					r.ExpiresAt = time.Now().Add(-time.Second)
				case "path":
					r.AllowedPaths = []string{"other/**"}
				case "qualification":
					r.Qualified = false
				}
				p.Releases = map[string]model.Release{"production": r}
			})
			f.candidate(t)
			evidence, e := f.m.Release(context.Background(), "sample", f.p, f.a, model.Feature{ID: f.a.FeatureID, Project: "sample", Risk: "low", CompletionTarget: "production"})
			if e == nil || len(evidence) != 0 {
				t.Fatalf("release gate %s accepted: %+v %v", scenario, evidence, e)
			}
		})
	}
}

func TestWorkingTreePathsIgnoreAgentIndexAndIncludeUntracked(t *testing.T) {
	f := setup(t, nil)
	put(t, filepath.Join(f.a.Workspace, "app.txt"), "staged change")
	testGit(t, f.a.Workspace, "add", "app.txt")
	// Only captured working source is authoritative, not the agent's staged edit.
	put(t, filepath.Join(f.a.Workspace, "app.txt"), "baseline\n")
	put(t, filepath.Join(f.a.Workspace, "untracked.txt"), "new source")
	paths, e := f.m.ChangedPaths(context.Background(), f.a.Workspace, f.a.BaseSHA, "")
	if e != nil || len(paths) != 1 || paths[0] != "untracked.txt" {
		t.Fatalf("working paths %v: %v", paths, e)
	}
}

func TestQualifiedReleaseObservesFullWindow(t *testing.T) {
	receiptsFile := filepath.Join(t.TempDir(), "observations")
	f := setup(t, func(p *model.Project) {
		p.Releases = map[string]model.Release{"production": {
			Allowed: true, Qualified: true, AllowedRisks: []string{"low"}, AllowedPaths: []string{"app.txt"}, ExpiresAt: time.Now().Add(time.Hour),
			Deploy:   model.Command{Argv: []string{"sh", "-c", "test \"$OPENFACTORY_PROJECT\" = sample && test \"$OPENFACTORY_FEATURE_ID\" = feature && test \"$OPENFACTORY_POLICY_HASH\" = policy && test -n \"$OPENFACTORY_SHA\""}},
			Observe:  model.Command{Argv: []string{"sh", "-c", "printf observed\\\\n >> " + quote(receiptsFile)}},
			Rollback: model.Command{Argv: []string{"false"}}, ObservationSeconds: 1, MinimumObservations: 2,
		}}
	})
	f.candidate(t)
	if _, e := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	receipts, e := f.m.Release(context.Background(), "sample", f.p, f.a, model.Feature{ID: f.a.FeatureID, Project: "sample", Risk: "low", CompletionTarget: "production"})
	if e != nil || len(receipts) != 3 || time.Since(start) < time.Second {
		t.Fatalf("release qualification receipts %+v: %v", receipts, e)
	}
	for _, receipt := range receipts {
		if !receipt.Passed || receipt.CandidateSHA != f.a.CandidateSHA {
			t.Fatalf("wrong release receipt: %+v", receipt)
		}
	}
	contents, e := os.ReadFile(receiptsFile)
	if e != nil || strings.Count(string(contents), "observed") != 2 {
		t.Fatalf("observation count %q: %v", contents, e)
	}
}

func TestUnavailableCheckExecutorNeverFallsBackToController(t *testing.T) {
	f := setup(t, nil)
	f.m.cfg.CheckSocket = filepath.Join(t.TempDir(), "missing.sock")
	out, e := f.m.Check(context.Background(), f.a.Workspace, f.p.Checks["environment"])
	if e == nil || strings.Contains(out, "HOME=") {
		t.Fatalf("executor outage ran controller check: %q %v", out, e)
	}
}
