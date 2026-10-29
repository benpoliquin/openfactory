package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func discoveryFixture(t *testing.T) (*fixture, model.DiscoveryWorkspace) {
	t.Helper()
	f := setup(t, nil)
	f.m.cfg.WorkspaceDir = shortDiscoveryWorkspaceDir(t)
	w, err := f.m.PrepareDiscovery(context.Background(), "sample", f.p, "discovery-attempt", "architecture", "step-one")
	if err != nil {
		t.Fatal(err)
	}
	return f, w
}

func TestDiscoveryWorkspaceIsolationAndStableProject(t *testing.T) {
	f, w := discoveryFixture(t)
	ctx := context.Background()
	before := testGit(t, f.remote, "rev-parse", "refs/heads/main")
	if w.BaseSHA == before || !shaPattern.MatchString(w.BaseSHA) {
		t.Fatal("discovery baseline incorrectly claims target provenance")
	}
	for _, root := range []string{w.Path, w.ProjectRoot} {
		entries, e := os.ReadDir(root)
		if e != nil || len(entries) != 0 {
			t.Fatalf("discovery root contains undeclared product files: %s %v %v", root, entries, e)
		}
	}
	again, err := f.m.PrepareDiscovery(ctx, "sample", f.p, "discovery-attempt", "architecture", "step-one")
	if err != nil || again != w {
		t.Fatalf("retry: %#v %v", again, err)
	}
	other, err := f.m.PrepareDiscovery(ctx, "sample", f.p, "evaluation-attempt", "plan_evaluation", "step-two")
	if err != nil {
		t.Fatal(err)
	}
	if other.Path == w.Path || other.ProjectRoot != w.ProjectRoot {
		t.Fatalf("unstable project or shared role workspace: %#v %#v", w, other)
	}
	if strings.HasPrefix(w.ProjectRoot, f.m.cfg.WorkspaceDir+string(filepath.Separator)) {
		t.Fatal("source project root is agent writable")
	}
	put(t, filepath.Join(w.Path, "app.txt"), "discovery-only mutation")
	if got := testGit(t, f.remote, "rev-parse", "refs/heads/main"); got != before {
		t.Fatal("discovery moved product target")
	}
	if entries, err := os.ReadDir(other.Path); err != nil || len(entries) != 0 {
		t.Fatalf("role leaked mutable or product files: %v %v", entries, err)
	}
}

func TestDiscoverySessionExcludedAndFrozenBytesImmutable(t *testing.T) {
	f, w := discoveryFixture(t)
	ctx := context.Background()
	s := model.DiscoverySession{AttemptID: "discovery-attempt", Role: "architecture", StepID: "step-one", CapabilityToken: "must-not-be-bundled"}
	if err := f.m.ConfigureDiscoverySession(ctx, w, s); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(w.Path, "experiment.txt"), "approved bytes")
	put(t, filepath.Join(w.Path, "undeclared.txt"), "not approved")
	bundle, err := f.m.FreezeDiscoveryBundle(ctx, w, []string{"experiment.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Files) != 1 || bundle.Files[0].Path != "experiment.txt" {
		t.Fatalf("nonexplicit capture: %#v", bundle)
	}
	put(t, filepath.Join(w.Path, "experiment.txt"), "mutated after approval")
	snapshot, err := f.m.MaterializeExperiment(ctx, f.a, bundle)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(snapshot.Path, "experiment.txt"))
	if err != nil || string(data) != "approved bytes" {
		t.Fatalf("snapshot changed: %q %v", data, err)
	}
	for _, name := range []string{"undeclared.txt", ".openfactory/discovery-session.json"} {
		if _, err := os.Lstat(filepath.Join(snapshot.Path, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected snapshot member %s: %v", name, err)
		}
	}
	bundle.Files[0].Bytes++
	if _, err := f.m.MaterializeExperiment(ctx, f.a, bundle); err == nil {
		t.Fatal("altered manifest accepted")
	}
}

func TestDiscoveryRejectsUnsafeMembersAndSessionParents(t *testing.T) {
	f, w := discoveryFixture(t)
	ctx := context.Background()
	outside := t.TempDir()
	put(t, filepath.Join(outside, "secret"), "private")
	if err := os.Symlink(outside, filepath.Join(w.Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(w.Path, "file-link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret", "/etc/passwd", "link/secret", "file-link", ".openfactory/discovery-session.json", ".git/config", "app.txt/../app.txt"} {
		if _, err := f.m.FreezeDiscoveryBundle(ctx, w, []string{name}); err == nil {
			t.Fatalf("unsafe member accepted: %q", name)
		}
	}
	if err := os.Symlink(outside, filepath.Join(w.Path, ".openfactory")); err != nil {
		t.Fatal(err)
	}
	s := model.DiscoverySession{AttemptID: "discovery-attempt", Role: "architecture", StepID: "step-one", CapabilityToken: "secret"}
	if err := f.m.ConfigureDiscoverySession(ctx, w, s); err == nil {
		t.Fatal("symlink controlfile parent accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "discovery-session.json")); !os.IsNotExist(err) {
		t.Fatalf("wrote outside workspace: %v", err)
	}
}

func TestDiscoveryDirectoryReplacementIsRefused(t *testing.T) {
	f, w := discoveryFixture(t)
	c, err := f.m.openDiscovery(w)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	moved := w.Path + "-old"
	if err := os.Rename(w.Path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.verify(); err == nil {
		t.Fatal("replaced workspace directory retained authority")
	}
}

func TestDiscoveryPolicyLimitsAndEnrollment(t *testing.T) {
	f, w := discoveryFixture(t)
	ctx := context.Background()
	put(t, filepath.Join(w.Path, "app.txt"), "oversized explicit model artifact")
	f.m.cfg.Discovery.BundleMaxFileBytes = 3
	if _, err := f.m.FreezeDiscoveryBundle(ctx, w, []string{"app.txt"}); err == nil {
		t.Fatal("policy byte cap ignored")
	}
	f.m.cfg.Discovery.BundleMaxFiles = 1
	if _, err := f.m.FreezeDiscoveryBundle(ctx, w, []string{"app.txt", "another"}); err == nil {
		t.Fatal("policy count cap ignored")
	}
	changed := f.p
	changed.Branch = "other"
	if _, err := f.m.PrepareDiscovery(ctx, "sample", changed, "new", "research", "step"); err == nil {
		t.Fatal("unenrolled discovery accepted")
	}
}

func TestDiscoveryFingerprintDetectsContentAndUntrackedMutations(t *testing.T) {
	f, w := discoveryFixture(t)
	ctx := context.Background()
	put(t, filepath.Join(w.Path, "app.txt"), "initial explicit model artifact")
	s := model.DiscoverySession{AttemptID: "discovery-attempt", Role: "architecture", StepID: "step-one", CapabilityToken: "first-token"}
	if err := f.m.ConfigureDiscoverySession(ctx, w, s); err != nil {
		t.Fatal(err)
	}
	first, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	s.CapabilityToken = "replacement-token"
	if err = f.m.ConfigureDiscoverySession(ctx, w, s); err != nil {
		t.Fatal(err)
	}
	second, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("controller session change affected content guard")
	}
	put(t, filepath.Join(w.Path, "app.txt"), "mutated baseline")
	changed, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("tracked mutation escaped content guard")
	}
	put(t, filepath.Join(w.Path, "new-artifact.txt"), "untracked")
	untracked, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if untracked == changed {
		t.Fatal("untracked mutation escaped content guard")
	}
	if err = os.Symlink("app.txt", filepath.Join(w.Path, "unsafe-link")); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.FingerprintDiscovery(ctx, w); err == nil {
		t.Fatal("fingerprint accepted symlink")
	}
}

func TestDiscoveryDoesNotFetchEnrolledProductRepository(t *testing.T) {
	f, _ := discoveryFixture(t)
	cfg := f.m.cfg
	cfg.StateDir = filepath.Join(t.TempDir(), "discovery-state")
	p := f.p
	p.Repository = filepath.Join(t.TempDir(), "unavailable-product.git")
	cfg.Projects = map[string]model.Project{"offline": p}
	manager := New(cfg)
	w, err := manager.PrepareDiscovery(context.Background(), "offline", p, "offline-attempt", "research", "offline-step")
	if err != nil {
		t.Fatalf("empty discovery unexpectedly needs product repository IO: %v", err)
	}
	for _, root := range []string{w.Path, w.ProjectRoot} {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("nonempty discovery root: %v %v", entries, err)
		}
	}
}
