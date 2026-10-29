package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestSnapshotExcludesIgnoredArtifactsAndAllowsCheckOutputs(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	original := f.a.Workspace
	put(t, filepath.Join(original, ".gitignore"), "build/\n")
	if err := os.Mkdir(filepath.Join(original, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(original, "build", "stale"), "must never reach checks")
	f.candidate(t)
	if filepath.Dir(original) != filepath.Dir(f.a.Workspace) || original == f.a.Workspace {
		t.Fatal("snapshot is not a fresh sibling BB checkout")
	}
	if _, err := os.Stat(filepath.Join(f.a.Workspace, "build")); !os.IsNotExist(err) {
		t.Fatalf("snapshot retained ignored candidate artifacts: %v", err)
	}
	if got := testGit(t, f.a.Workspace, "rev-parse", "HEAD"); got != f.a.CandidateSHA {
		t.Fatalf("review checkout is not exact candidate: %s", got)
	}
	before, err := f.m.Fingerprint(ctx, f.a.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(f.a.Workspace, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.a.Workspace, "build", "check-output"), "ordinary deterministic check artifact")
	after, err := f.m.Fingerprint(ctx, f.a.Workspace)
	if err != nil || after != before {
		t.Fatalf("ignored check output invalidated approved source: %v", err)
	}
	if _, err = f.m.Integrate(ctx, "sample", f.p, f.a, f.a.BaseSHA); err != nil {
		t.Fatalf("ignored artifact blocked exact candidate integration: %v", err)
	}
	put(t, filepath.Join(f.a.Workspace, "app.txt"), "mutated tracked source")
	after, err = f.m.Fingerprint(ctx, f.a.Workspace)
	if err != nil || after == before {
		t.Fatalf("tracked mutation escaped fingerprint: %v", err)
	}
	if _, err = f.m.Integrate(ctx, "sample", f.p, f.a, f.a.BaseSHA); err == nil {
		t.Fatal("integrated a changed checked source")
	}
}

func TestAgentGitHistoryAndConfigurationNeverControlCapture(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	workspace := f.a.Workspace
	// Agent commits, including a now-deleted protected file, are not promoted.
	put(t, filepath.Join(workspace, "hidden.txt"), "hidden intermediate history")
	testGit(t, workspace, "add", "hidden.txt")
	testGit(t, workspace, "commit", "-m", "unapproved hidden history")
	if err := os.Remove(filepath.Join(workspace, "hidden.txt")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "controller-executed-agent-config")
	hook := filepath.Join(t.TempDir(), "malicious-hook")
	put(t, hook, "#!/bin/sh\nprintf compromised > "+quote(marker)+"\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(workspace, ".git", "config"), "[core]\n repositoryformatversion = 0\n bare = false\n fsmonitor = "+hook+"\n[filter \"attack\"]\n clean = "+hook+"\n required = true\n")
	put(t, filepath.Join(workspace, ".gitattributes"), "app.txt filter=attack\n")
	f.candidate(t)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("controller executed shared configuration: %v", err)
	}
	r, err := f.m.workspace(ctx, f.a.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	parents, err := f.m.git(ctx, r.Common, "rev-list", "--parents", "-n", "1", f.a.CandidateSHA)
	if err != nil || strings.TrimSpace(parents) != f.a.CandidateSHA+" "+f.a.BaseSHA {
		t.Fatalf("promoted hidden agent history: %q %v", parents, err)
	}
	count, err := f.m.git(ctx, r.Common, "rev-list", "--count", f.a.BaseSHA+".."+f.a.CandidateSHA)
	if err != nil || strings.TrimSpace(count) != "1" {
		t.Fatalf("candidate is not a single squashed commit: %q %v", count, err)
	}
	if _, err = os.Stat(filepath.Join(f.a.Workspace, "hidden.txt")); !os.IsNotExist(err) {
		t.Fatalf("hidden intermediate source was retained: %v", err)
	}
	if strings.HasPrefix(r.Common, f.m.cfg.WorkspaceDir+string(filepath.Separator)) {
		t.Fatal("controller repository is exposed on agent volume")
	}
	// Even an unusable .git pointer cannot redirect privileged operations.
	if err = os.RemoveAll(filepath.Join(f.a.Workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.a.Workspace, ".git"), "gitdir: /untrusted/controller-config\n")
	if _, err = f.m.Integrate(ctx, "sample", f.p, f.a, f.a.BaseSHA); err != nil {
		t.Fatalf("shared .git became controller authority: %v", err)
	}
}

func approvedRelease() model.Release {
	return model.Release{Allowed: true, Qualified: true, AllowedRisks: []string{"low"}, AllowedPaths: []string{"**/*.txt"}, ExpiresAt: time.Now().Add(time.Hour), Deploy: model.Command{Argv: []string{"true"}, TimeoutSeconds: 2}, Observe: model.Command{Argv: []string{"true"}, TimeoutSeconds: 2}, Rollback: model.Command{Argv: []string{"true"}, TimeoutSeconds: 2}, ObservationSeconds: 1, MinimumObservations: 1}
}

func releaseFeature(f *fixture) model.Feature {
	return model.Feature{ID: f.a.FeatureID, Project: "sample", Risk: "low", CompletionTarget: "production"}
}

func TestReleaseRunsOperatorScriptInPrivateExactCandidate(t *testing.T) {
	proof := filepath.Join(t.TempDir(), "release-proof")
	f := setup(t, func(p *model.Project) {
		r := approvedRelease()
		r.Deploy.Argv = []string{"sh", "-c", "test \"$(cat app.txt)\" = candidate && test ! -e build && pwd > " + quote(proof)}
		p.Releases = map[string]model.Release{"production": r}
	})
	put(t, filepath.Join(f.a.Workspace, ".gitignore"), "build/\n")
	// This test also proves the globstar root match is the shared policy matcher.
	r := f.p.Releases["production"]
	r.AllowedPaths = []string{"**/*.txt", ".gitignore"}
	f.p.Releases["production"] = r
	f.m.cfg.Projects["sample"] = f.p
	f.candidate(t)
	if err := os.Mkdir(filepath.Join(f.a.Workspace, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.a.Workspace, "build", "poison"), "shared ignored artifact")
	if _, err := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); err != nil {
		t.Fatal(err)
	}
	evidence, err := f.m.Release(context.Background(), "sample", f.p, f.a, releaseFeature(f))
	if err != nil || len(evidence) != 2 || !evidence[0].Passed {
		t.Fatalf("private release failed: %+v %v", evidence, err)
	}
	data, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	cwd := strings.TrimSpace(string(data))
	stateRoot, err := filepath.EvalSymlinks(f.m.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	sharedRoot, err := filepath.EvalSymlinks(f.m.cfg.WorkspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cwd, stateRoot+string(filepath.Separator)) || strings.HasPrefix(cwd, sharedRoot+string(filepath.Separator)) {
		t.Fatalf("release used shared candidate cwd: %q", cwd)
	}
	if _, err = os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("private release export not removed: %v", err)
	}
}

func TestPreflightRejectsInfeasibleOrUntrustedReleaseBeforeEffects(t *testing.T) {
	for _, scenario := range []string{"budget", "timeout", "outside", "relative", "symlink", "risk", "path", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "deployed")
			f := setup(t, func(p *model.Project) {
				r := approvedRelease()
				r.Deploy.Argv = []string{"sh", "-c", "printf effect > " + quote(marker)}
				p.Releases = map[string]model.Release{"production": r}
			})
			f.candidate(t)
			r := f.p.Releases["production"]
			feature := releaseFeature(f)
			ctx := context.Background()
			switch scenario {
			case "budget":
				f.a.Deadline = time.Now().Add(3 * time.Second)
			case "timeout":
				r.Observe.TimeoutSeconds = 3601
			case "outside":
				r.Deploy.Argv = []string{"/bin/true"}
			case "relative":
				r.Deploy.Argv = []string{"./deploy"}
			case "symlink":
				link := filepath.Join(f.m.cfg.ReleaseDir, "outside-link")
				if err := os.Symlink("/bin/true", link); err != nil {
					t.Fatal(err)
				}
				r.Deploy.Argv = []string{link}
			case "risk":
				feature.Risk = "high"
			case "path":
				r.AllowedPaths = []string{"unrelated/**"}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			f.p.Releases["production"] = r
			f.m.cfg.Projects["sample"] = f.p
			before := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main")
			if err := f.m.PreflightRelease(ctx, "sample", f.p, f.a, feature); err == nil {
				t.Fatal("preflight admitted known infeasible authority")
			}
			if receipts, err := f.m.Release(ctx, "sample", f.p, f.a, feature); err == nil || len(receipts) != 0 {
				t.Fatalf("release had effects after failed preflight: %+v %v", receipts, err)
			}
			if after := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main"); after != before {
				t.Fatal("preflight mutated remote")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("preflight executed deployment: %v", err)
			}
		})
	}
}

func TestPreflightDoesNotRequireAlreadyIntegratedRemote(t *testing.T) {
	f := setup(t, func(p *model.Project) { p.Releases = map[string]model.Release{"production": approvedRelease()} })
	f.candidate(t)
	if err := f.m.PreflightRelease(context.Background(), "sample", f.p, f.a, releaseFeature(f)); err != nil {
		t.Fatalf("side-effect-free admission required integration: %v", err)
	}
	if receipts, err := f.m.Release(context.Background(), "sample", f.p, f.a, releaseFeature(f)); err == nil || len(receipts) != 0 {
		t.Fatalf("release ignored current remote identity: %+v %v", receipts, err)
	}
}

func TestGitProtocolSeparatesStdoutFromStderr(t *testing.T) {
	out, err := runGit(context.Background(), "", []string{"sh", "-c", "printf 'abc123\\trefs/heads/main\\n'; printf 'remote warning\\n' >&2"}, nil, nil, 1)
	if err != nil || out != "abc123\trefs/heads/main\n" {
		t.Fatalf("stderr contaminated Git stdout: %q %v", out, err)
	}
	out, err = runGit(context.Background(), "", []string{"sh", "-c", "printf diagnostic-out; printf secret-diagnostic >&2; exit 1"}, nil, []string{"secret"}, 1)
	if err == nil || !strings.Contains(err.Error(), "diagnostic-out") || !strings.Contains(err.Error(), "[REDACTED]-diagnostic") || strings.Contains(out, "secret") {
		t.Fatalf("failed Git diagnostics lost or leaked: %q %v", out, err)
	}
}

func TestExecutorInfrastructureClassificationSurvivesRedaction(t *testing.T) {
	f := setup(t, nil)
	f.m.cfg.CheckSocket = filepath.Join(t.TempDir(), "unavailable.sock")
	_, err := f.m.Check(context.Background(), f.a.Workspace, f.p.Checks["environment"])
	var infrastructure interface{ Infrastructure() bool }
	if !errors.As(err, &infrastructure) || !infrastructure.Infrastructure() {
		t.Fatalf("executor outage became repairable command failure: %v", err)
	}
}

func TestReleaseObservationDoesNotLockUnrelatedGitWork(t *testing.T) {
	started := filepath.Join(t.TempDir(), "deployment-started")
	f := setup(t, func(p *model.Project) {
		r := approvedRelease()
		r.ObservationSeconds = 30
		r.Deploy.Argv = []string{"sh", "-c", "printf started > " + quote(started)}
		p.Releases = map[string]model.Release{"production": r}
	})
	f.candidate(t)
	if _, err := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan error, 1)
	go func() {
		_, err := f.m.Release(ctx, "sample", f.p, f.a, releaseFeature(f))
		released <- err
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-released:
			t.Fatalf("release failed before observation: %v", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("deployment never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	prepared := make(chan error, 1)
	go func() {
		_, err := f.m.Prepare(context.Background(), "sample", f.p, "unrelated-attempt")
		prepared <- err
	}()
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatalf("unrelated preparation failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-released
		<-prepared
		t.Fatal("release observation held mutable Git lock")
	}
	cancel()
	select {
	case err := <-released:
		if err == nil {
			t.Fatal("cancelled observation reported release success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled release did not finish bounded rollback")
	}
}

func TestLegacySharedRepositoryReceiptCannotAuthorizeControllerGit(t *testing.T) {
	f := setup(t, nil)
	r, err := f.m.workspace(context.Background(), f.a.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	// A receipt from the old shared-worktree layout is not a migration shim.
	r.Common = filepath.Join(f.a.Workspace, ".git")
	r.ConfigHash, err = configHash(r.Common)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.m.saveRecord(r); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Fingerprint(context.Background(), f.a.Workspace); err == nil {
		t.Fatal("legacy shared config was accepted as controller authority")
	}
}
