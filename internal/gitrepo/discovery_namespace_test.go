package gitrepo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/model"
)

// Keep valid-path fixtures below OMP's absolute flattened-cwd limit even when
// testing.T's temporary directory includes a long descriptive test name.
func shortDiscoveryWorkspaceDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "sf-d-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readNamespaceReceipt(t *testing.T, m *Manager, w model.DiscoveryWorkspace) discoveryReceipt {
	t.Helper()
	data, err := os.ReadFile(m.discoveryReceiptPath(w.Path))
	if err != nil {
		t.Fatal(err)
	}
	var receipt discoveryReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func writeNamespaceReceipt(t *testing.T, m *Manager, path string, receipt discoveryReceipt) {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.discoveryReceiptPath(path), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryNamespaceSeparatesEachRawIdentity(t *testing.T) {
	f, initial := discoveryFixture(t)
	ctx := context.Background()
	before := testGit(t, f.remote, "rev-parse", "refs/heads/main")
	identities := []struct{ attempt, role, step string }{
		{"discovery-attempt", "architecture", "step-one"},
		{"discovery-attempt", "architecture/other", "step-one"},
		{"discovery-attempt", "architecture", "step-one/other"},
		{"discovery-attempt/other", "architecture", "step-one"},
	}
	seen := make(map[string]bool)
	for _, identity := range identities {
		w, err := f.m.PrepareDiscovery(ctx, "sample", f.p, identity.attempt, identity.role, identity.step)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(f.m.cfg.WorkspaceDir, "discovery", digest("sample")[:32], digest(identity.role)[:32], digest(identity.step)[:32], digest(identity.attempt)[:32])
		if w.Path != want || seen[w.Path] || w.ProjectRoot != initial.ProjectRoot {
			t.Fatalf("namespace identity was aliased: got %#v, want path %q", w, want)
		}
		seen[w.Path] = true
		entries, err := os.ReadDir(w.Path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("workspace leaked another identity's bytes: %v %v", entries, err)
		}
		put(t, filepath.Join(w.Path, "private-role-output"), identity.attempt+identity.role+identity.step)
		again, err := f.m.PrepareDiscovery(ctx, "sample", f.p, identity.attempt, identity.role, identity.step)
		if err != nil || again != w {
			t.Fatalf("unstable namespace retry: %#v %v", again, err)
		}
		r := readNamespaceReceipt(t, f.m, w)
		if r.Project != "sample" || r.Role != identity.role || r.Step != identity.step || r.Attempt != identity.attempt {
			t.Fatalf("raw identity lost in private receipt: %#v", r)
		}
		if got := filepath.Base(f.m.discoveryReceiptPath(w.Path)); got != digest(w.Path)+".json" || len(got) != 69 {
			t.Fatalf("private receipt digest was shortened: %q", got)
		}
	}
	for _, name := range []string{"cache-" + digest("sample") + ".json", "source-" + digest("sample") + ".json"} {
		if _, err := os.Stat(filepath.Join(f.m.discoveryRecords(), name)); err != nil {
			t.Fatalf("full private cache identifier missing: %v", err)
		}
	}
	if got := testGit(t, f.remote, "rev-parse", "refs/heads/main"); got != before {
		t.Fatal("discovery modified the enrolled product target")
	}
}

func TestDiscoveryNamespaceRejectsRawReceiptMismatch(t *testing.T) {
	f, w := discoveryFixture(t)
	original := readNamespaceReceipt(t, f.m, w)
	for _, field := range []string{"project", "role", "step", "attempt", "repository", "target"} {
		t.Run(field, func(t *testing.T) {
			r := original
			switch field {
			case "project":
				r.Project += "-collision"
			case "role":
				r.Role += "-collision"
			case "step":
				r.Step += "-collision"
			case "attempt":
				r.Attempt += "-collision"
			case "repository":
				r.Repository += "-changed"
			case "target":
				r.Target += "-changed"
			}
			// Simulate a truncated-hash collision without needing a SHA256 preimage:
			// a different raw tuple occupies this tuple's private receipt filename.
			writeNamespaceReceipt(t, f.m, w.Path, r)
			got, err := f.m.PrepareDiscovery(context.Background(), "sample", f.p, "discovery-attempt", "architecture", "step-one")
			if err == nil || got != (model.DiscoveryWorkspace{}) {
				t.Fatalf("mismatched raw receipt accepted: %#v %v", got, err)
			}
			writeNamespaceReceipt(t, f.m, w.Path, original)
		})
	}
}

func TestDiscoveryNamespaceRejectsReceiptRedirectToExistingWorkspace(t *testing.T) {
	f, w := discoveryFixture(t)
	other, err := f.m.PrepareDiscovery(context.Background(), "sample", f.p, "other-attempt", "architecture", "step-one")
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(other.Path, "other-private-output"), "must not cross namespaces")
	r := readNamespaceReceipt(t, f.m, w)
	r.Workspace = other
	writeNamespaceReceipt(t, f.m, w.Path, r)
	got, err := f.m.PrepareDiscovery(context.Background(), "sample", f.p, "discovery-attempt", "architecture", "step-one")
	if err == nil || got != (model.DiscoveryWorkspace{}) {
		t.Fatalf("receipt redirected preparation into another valid workspace: %#v %v", got, err)
	}
	if chain, err := f.m.openDiscovery(w); err == nil {
		chain.close()
		t.Fatal("altered workspace receipt accepted by guarded open")
	}
}

func TestDiscoveryNamespaceDoesNotReuseLegacyFullDigestPath(t *testing.T) {
	f, w := discoveryFixture(t)
	legacy := filepath.Join(f.m.cfg.WorkspaceDir, "discovery", digest("sample"), digest("architecture"), digest("step-one"), digest("legacy-attempt"))
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(legacy, "legacy-output"), "must not migrate or alias")
	r := readNamespaceReceipt(t, f.m, w)
	r.Workspace.Path = legacy
	r.Attempt = "legacy-attempt"
	writeNamespaceReceipt(t, f.m, legacy, r)
	got, err := f.m.PrepareDiscovery(context.Background(), "sample", f.p, "legacy-attempt", "architecture", "step-one")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.m.cfg.WorkspaceDir, "discovery", digest("sample")[:32], digest("architecture")[:32], digest("step-one")[:32], digest("legacy-attempt")[:32])
	if got.Path != want {
		t.Fatalf("legacy namespace reused: got %q, want %q", got.Path, want)
	}
	if entries, err := os.ReadDir(got.Path); err != nil || len(entries) != 0 {
		t.Fatalf("legacy bytes leaked into canonical namespace: %v %v", entries, err)
	}
}

func TestDiscoveryFlattenedCwdByteBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bytes     int
		unicode   bool
		wantError bool
	}{
		{name: "255-bytes", bytes: 255},
		{name: "256-bytes", bytes: 256, wantError: true},
		{name: "256-utf8-bytes", bytes: 256, unicode: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := discoveryFixture(t)
			base := shortDiscoveryWorkspaceDir(t)
			suffix := string(filepath.Separator) + filepath.Join("discovery", digest("sample")[:32], digest("architecture")[:32], digest("step-one")[:32], digest("boundary-attempt")[:32])
			paddingBytes := tc.bytes - 3 - len(suffix) - len(base) - 1
			if paddingBytes < 1 {
				t.Fatal("fixture base too long for boundary test")
			}
			padding := strings.Repeat("x", paddingBytes)
			if tc.unicode {
				padding = strings.Repeat("é", paddingBytes/2) + strings.Repeat("x", paddingBytes%2)
			}
			f.m.cfg.WorkspaceDir = filepath.Join(base, padding)
			// Isolate the prerequisite check from existing private cache/source state.
			f.m.cfg.StateDir = filepath.Join(base, "state")
			w, err := f.m.PrepareDiscovery(context.Background(), "sample", f.p, "boundary-attempt", "architecture", "step-one")
			if !tc.wantError {
				if err != nil {
					t.Fatal(err)
				}
				encoded := "--" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(strings.TrimPrefix(w.Path, "/")) + "--"
				if len(encoded) != tc.bytes {
					t.Fatalf("wrong flattened boundary: %d bytes, want %d", len(encoded), tc.bytes)
				}
				return
			}
			var prerequisite *model.ProgramError
			if !errors.As(err, &prerequisite) || prerequisite.Code != "prerequisite_missing" || !strings.Contains(prerequisite.Message, "WorkspaceDir") || w != (model.DiscoveryWorkspace{}) {
				t.Fatalf("long root was not a preparation prerequisite error: %#v %v", w, err)
			}
			if strings.Contains(prerequisite.Message, base) {
				t.Fatal("prerequisite message disclosed a private operator path")
			}
			for _, path := range []string{filepath.Join(f.m.cfg.WorkspaceDir, "discovery"), filepath.Join(f.m.cfg.StateDir, "discovery-git"), filepath.Join(f.m.cfg.StateDir, "discovery-sources")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("long-root preparation created discovery artifacts before rejecting prerequisite: %s %v", path, err)
				}
			}
		})
	}
}
