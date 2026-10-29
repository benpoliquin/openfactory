package gitrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func intakeFixture(t *testing.T) (*Manager, model.Project, model.IntakeWorkspace, model.IntakeSession) {
	t.Helper()
	root := shortDiscoveryWorkspaceDir(t)
	// A nonexistent enrolled product repository makes any accidental clone/fetch
	// fail deterministically, without an HTTP mock or substituted Git executable.
	p := model.Project{Repository: filepath.Join(root, "product-does-not-exist.git"), Branch: "main", Trusted: true}
	m := New(model.Config{WorkspaceDir: filepath.Join(root, "w"), StateDir: filepath.Join(root, "state"), Projects: map[string]model.Project{"sample": p}})
	w, err := m.PrepareIntake(context.Background(), "sample", p, "attempt", "request", "step")
	if err != nil {
		t.Fatal(err)
	}
	s := model.IntakeSession{Project: "sample", RequestID: "request", StepID: "step", AttemptID: "attempt", Deadline: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), InputFiles: map[string][]byte{"inputs/context.json": []byte("{\"goal\":\"private intake text\"}"), "inputs/bundle/readme.txt": []byte("complete disclosed material\n")}}
	return m, p, w, s
}

func intakeReceiptForTest(t *testing.T, m *Manager, w model.IntakeWorkspace) intakeReceipt {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.intakeRecords(), m.intakeReceiptName(w.Path)))
	if err != nil {
		t.Fatal(err)
	}
	var r intakeReceipt
	if err = json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func intakeWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestIntakeFreshNamespaceStablePrivateSourceNoProductFetch(t *testing.T) {
	m, p, w, _ := intakeFixture(t)
	ctx := context.Background()
	if !shaPattern.MatchString(w.BaseSHA) {
		t.Fatalf("missing actual empty Git baseline: %#v", w)
	}
	if got := testGit(t, filepath.Join(m.cfg.StateDir, "intake-git", digest("sample")+".git"), "ls-tree", "-r", w.BaseSHA); got != "" {
		t.Fatalf("baseline is not empty: %s", got)
	}
	seen := map[string]bool{}
	for _, ids := range [][3]string{{"attempt", "request", "step"}, {"other-attempt", "request", "step"}, {"attempt", "other-request", "step"}, {"attempt", "request", "other-step"}} {
		got, err := m.PrepareIntake(ctx, "sample", p, ids[0], ids[1], ids[2])
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(m.cfg.WorkspaceDir, "intake", digest("sample")[:32], digest(ids[1])[:32], digest(ids[2])[:32], digest(ids[0])[:32])
		if got.Path != want || seen[got.Path] || got.ProjectRoot != w.ProjectRoot || got.BaseSHA != w.BaseSHA {
			t.Fatalf("incorrect namespace: %#v", got)
		}
		seen[got.Path] = true
		entries, err := os.ReadDir(got.Path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("new workspace is not empty: %v %v", entries, err)
		}
		again, err := m.PrepareIntake(ctx, "sample", p, ids[0], ids[1], ids[2])
		if err != nil || again != got {
			t.Fatalf("unstable retry: %#v %v", again, err)
		}
	}
	if entries, err := os.ReadDir(w.ProjectRoot); err != nil || len(entries) != 0 {
		t.Fatalf("private BB source not empty: %v %v", entries, err)
	}
	for _, name := range []string{"discovery-git", "discovery-sources", "git-workspaces"} {
		if _, err := os.Lstat(filepath.Join(m.cfg.StateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("intake touched unrelated state %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(p.Repository); !os.IsNotExist(err) {
		t.Fatalf("intake created a product repository: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(m.cfg.StateDir, "intake-git", digest("sample")+".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte("remote")) || bytes.Contains(config, []byte(p.Repository)) {
		t.Fatalf("private baseline inherited product remote: %s", config)
	}
	if strings.HasPrefix(w.ProjectRoot, m.cfg.WorkspaceDir+string(filepath.Separator)) {
		t.Fatal("private source is on writable worker volume")
	}
}

func TestIntakeRejectsFullIdentityReceiptMismatch(t *testing.T) {
	for _, field := range []string{"project", "request", "step", "attempt", "enrollment", "path", "source", "base"} {
		t.Run(field, func(t *testing.T) {
			m, p, w, _ := intakeFixture(t)
			r := intakeReceiptForTest(t, m, w)
			switch field {
			case "project":
				r.Project += "collision"
			case "request":
				r.Request += "collision"
			case "step":
				r.Step += "collision"
			case "attempt":
				r.Attempt += "collision"
			case "enrollment":
				r.EnrollmentSHA = strings.Repeat("0", 64)
			case "path":
				r.Workspace.Path += "other"
			case "source":
				r.Workspace.ProjectRoot += "other"
			case "base":
				r.Workspace.BaseSHA = strings.Repeat("0", 40)
			}
			// Simulated prefix collision: the original tuple's receipt filename
			// contains another full raw identity, without needing a SHA preimage.
			intakeWriteJSON(t, filepath.Join(m.intakeRecords(), m.intakeReceiptName(w.Path)), r)
			if got, err := m.PrepareIntake(context.Background(), "sample", p, "attempt", "request", "step"); err == nil || got != (model.IntakeWorkspace{}) {
				t.Fatalf("mismatch accepted: %#v %v", got, err)
			}
			if _, err := m.FingerprintIntake(context.Background(), w); err == nil {
				t.Fatal("guard accepted altered receipt")
			}
		})
	}
}

func TestIntakeRejectsPrefixCollisionAtEveryNamespaceComponent(t *testing.T) {
	for depth := 1; depth <= 4; depth++ {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			m, p, w, _ := intakeFixture(t)
			parts := []string{"intake", digest("sample")[:32], digest("request")[:32], digest("step")[:32], digest("attempt")[:32]}
			prefix := filepath.Join(append([]string{m.cfg.WorkspaceDir}, parts[:depth+1]...)...)
			path := filepath.Join(m.intakeRecords(), "namespace-"+digest(prefix)+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var nr struct {
				Raw  []string
				Node intakeNode
			}
			if err = json.Unmarshal(data, &nr); err != nil {
				t.Fatal(err)
			}
			nr.Raw[depth] += "collision"
			intakeWriteJSON(t, path, nr)
			// Force the namespace creation path, retaining all existing directory
			// entries and namespace receipts to model a colliding raw tuple.
			if err = os.Remove(filepath.Join(m.intakeRecords(), m.intakeReceiptName(w.Path))); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(m.intakeRecords(), "complete-"+digest(w.Path)+".json")); err != nil {
				t.Fatal(err)
			}
			if _, err = m.PrepareIntake(context.Background(), "sample", p, "attempt", "request", "step"); err == nil || !strings.Contains(err.Error(), "collision") {
				t.Fatalf("prefix collision accepted: %v", err)
			}
		})
	}
}

func TestIntakeRejectsEnrollmentChangeAndUnrecordedWorkspace(t *testing.T) {
	m, p, w, _ := intakeFixture(t)
	changed := p
	changed.Branch = "other"
	m.cfg.Projects["sample"] = changed
	if _, err := m.PrepareIntake(context.Background(), "sample", changed, "attempt", "request", "step"); err == nil {
		t.Fatal("changed enrollment reused receipt")
	}
	if _, err := m.FingerprintIntake(context.Background(), w); err == nil {
		t.Fatal("changed enrollment passed guard")
	}
	m.cfg.Projects["sample"] = p
	if err := os.Remove(filepath.Join(m.intakeRecords(), m.intakeReceiptName(w.Path))); err != nil {
		t.Fatal(err)
	}
	// A receipt-less existing leaf must not be silently adopted.
	if _, err := m.PrepareIntake(context.Background(), "sample", p, "attempt", "request", "step"); err == nil {
		t.Fatal("unrecorded existing workspace adopted")
	}
}

func TestIntakeControlContainsOnlyHashesAndExactImmutableContext(t *testing.T) {
	m, _, w, s := intakeFixture(t)
	ctx := context.Background()
	if err := m.ConfigureIntakeSession(ctx, w, s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.Path, ".openfactory", "intake-session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var control intakeControl
	if err = json.Unmarshal(data, &control); err != nil {
		t.Fatal(err)
	}
	if control.Project != s.Project || control.RequestID != s.RequestID || control.StepID != s.StepID || control.AttemptID != s.AttemptID || !control.Deadline.Equal(s.Deadline) {
		t.Fatalf("control identity lost: %#v", control)
	}
	for name, expected := range s.InputFiles {
		if control.InputFiles[name] != intakeBytesSHA(expected) || bytes.Contains(data, expected) {
			t.Fatalf("input bytes leaked or hash changed: %s", name)
		}
		actual, err := os.ReadFile(filepath.Join(w.Path, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("input truncated: %s %v", name, err)
		}
		info, err := os.Lstat(filepath.Join(w.Path, filepath.FromSlash(name)))
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatalf("input not immutable: %s %v", name, err)
		}
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("control not private: %v", err)
	}
	before, err := m.FingerprintIntake(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.ConfigureIntakeSession(ctx, w, s); err != nil {
		t.Fatal(err)
	}
	after, err := m.FingerprintIntake(ctx, w)
	if err != nil || before != after {
		t.Fatalf("exact retry changed fingerprint: %s %s %v", before, after, err)
	}
}

func TestIntakeRejectsRetryIdentityDeadlineAndManifestChanges(t *testing.T) {
	for _, field := range []string{"project", "request", "step", "attempt", "deadline", "bytes", "add", "remove"} {
		t.Run(field, func(t *testing.T) {
			m, _, w, s := intakeFixture(t)
			if err := m.ConfigureIntakeSession(context.Background(), w, s); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "project":
				s.Project += "x"
			case "request":
				s.RequestID += "x"
			case "step":
				s.StepID += "x"
			case "attempt":
				s.AttemptID += "x"
			case "deadline":
				s.Deadline = s.Deadline.Add(time.Second)
			case "bytes":
				s.InputFiles["inputs/context.json"] = []byte("changed")
			case "add":
				s.InputFiles["inputs/new"] = []byte("new")
			case "remove":
				delete(s.InputFiles, "inputs/context.json")
			}
			if err := m.ConfigureIntakeSession(context.Background(), w, s); err == nil {
				t.Fatal("immutable session change accepted")
			}
		})
	}
}

func TestIntakeRejectsUnsafeInputPathsAndText(t *testing.T) {
	for _, name := range []string{"../outside", "/inputs/file", "inputs/../outside", "inputs//file", "inputs/./file", "inputs/.hidden", "inputs/a/.secret", "inputs/a\\b", "inputs/a\x00b", "inputs/a\nb", "inputs/a\u007fb", "not-inputs/file", "inputs"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			m, _, w, s := intakeFixture(t)
			s.InputFiles = map[string][]byte{name: []byte("secret")}
			if err := m.ConfigureIntakeSession(context.Background(), w, s); err == nil {
				t.Fatalf("unsafe path accepted: %q", name)
			}
		})
	}
	for _, data := range [][]byte{{0xff}, {'a', 0, 'b'}} {
		s := model.IntakeSession{Deadline: time.Now(), InputFiles: map[string][]byte{"inputs/text": data}}
		if _, _, err := intakeSessionControl(s); err == nil {
			t.Fatal("non-text input accepted")
		}
	}
	s := model.IntakeSession{Deadline: time.Now(), InputFiles: map[string][]byte{"inputs/a": []byte("a"), "inputs/a/b": []byte("b")}}
	if _, _, err := intakeSessionControl(s); err == nil {
		t.Fatal("parent/file collision accepted")
	}
}

func TestIntakeRejectsLinksReplacementsAndRetryDiskMutation(t *testing.T) {
	for _, kind := range []string{"input-symlink", "input-hardlink", "control-symlink", "control-hardlink", "parent-symlink", "input-directory-replace", "workspace-replace", "input-mutate", "input-delete", "undeclared-input", "control-mutate"} {
		t.Run(kind, func(t *testing.T) {
			m, _, w, s := intakeFixture(t)
			ctx := context.Background()
			if err := m.ConfigureIntakeSession(ctx, w, s); err != nil {
				t.Fatal(err)
			}
			before, err := m.FingerprintIntake(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(w.Path, "inputs", "context.json")
			control := filepath.Join(w.Path, ".openfactory", "intake-session.json")
			outside := filepath.Join(t.TempDir(), "outside")
			if err = os.WriteFile(outside, []byte("outside private bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "input-symlink", "control-symlink":
				path := input
				if kind == "control-symlink" {
					path = control
				}
				if err = os.Remove(path); err == nil {
					err = os.Symlink(outside, path)
				}
			case "input-hardlink", "control-hardlink":
				path := input
				if kind == "control-hardlink" {
					path = control
				}
				err = os.Link(path, outside+"-link")
			case "parent-symlink":
				path := filepath.Join(w.Path, "inputs")
				if err = os.Rename(path, path+"-old"); err == nil {
					err = os.Symlink(path+"-old", path)
				}
			case "input-directory-replace":
				path := filepath.Join(w.Path, "inputs", "bundle")
				if err = os.Rename(path, path+"-old"); err == nil {
					err = os.Mkdir(path, 0700)
				}
				if err == nil {
					err = os.WriteFile(filepath.Join(path, "readme.txt"), s.InputFiles["inputs/bundle/readme.txt"], 0400)
				}
				if err == nil {
					err = os.RemoveAll(path + "-old")
				}
			case "workspace-replace":
				if err = os.Rename(w.Path, w.Path+"-old"); err == nil {
					err = os.Mkdir(w.Path, 0700)
				}
			case "input-mutate":
				if err = os.Chmod(input, 0600); err == nil {
					err = os.WriteFile(input, []byte("modified input"), 0600)
				}
				if err == nil {
					err = os.Chmod(input, 0400)
				}
			case "input-delete":
				err = os.Remove(input)
			case "undeclared-input":
				err = os.WriteFile(filepath.Join(w.Path, "inputs", "undeclared"), []byte("undeclared"), 0400)
			case "control-mutate":
				err = os.WriteFile(control, []byte("{}"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, guardErr := m.FingerprintIntake(ctx, w)
			if guardErr == nil && before == after {
				t.Fatal("read-only guard ignored mutation")
			}
			if err = m.ConfigureIntakeSession(ctx, w, s); err == nil {
				t.Fatal("retry repaired or accepted tampered immutable context")
			}
			actual, err := os.ReadFile(outside)
			if err != nil || string(actual) != "outside private bytes" {
				t.Fatalf("outside file modified: %q %v", actual, err)
			}
		})
	}
}

func TestIntakeRootedDirectoryReplacementDuringOperation(t *testing.T) {
	m, _, w, _ := intakeFixture(t)
	c, _, err := m.openIntake(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err = os.Rename(w.Path, w.Path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(w.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = c.verify(); err == nil {
		t.Fatal("held-open chain accepted directory replacement")
	}
}

func TestIntakeFlattenedCwdByteBoundary(t *testing.T) {
	for _, tc := range []struct {
		bytes   int
		unicode bool
		reject  bool
	}{{255, false, false}, {256, false, true}, {256, true, true}} {
		t.Run(fmt.Sprintf("%d-unicode-%t", tc.bytes, tc.unicode), func(t *testing.T) {
			m, p, _, _ := intakeFixture(t)
			base := shortDiscoveryWorkspaceDir(t)
			suffix := string(filepath.Separator) + filepath.Join("intake", digest("sample")[:32], digest("request")[:32], digest("step")[:32], digest("boundary")[:32])
			paddingBytes := tc.bytes - 3 - len(base) - 1 - len(suffix)
			if paddingBytes < 1 {
				t.Fatal("short root required")
			}
			padding := strings.Repeat("x", paddingBytes)
			if tc.unicode {
				padding = strings.Repeat("é", paddingBytes/2) + strings.Repeat("x", paddingBytes%2)
			}
			m.cfg.WorkspaceDir = filepath.Join(base, padding)
			m.cfg.StateDir = filepath.Join(base, "state")
			w, err := m.PrepareIntake(context.Background(), "sample", p, "boundary", "request", "step")
			if !tc.reject {
				if err != nil || len(w.Path)+3 != 255 {
					t.Fatalf("boundary rejected: %#v %v", w, err)
				}
				return
			}
			var prerequisite *model.ProgramError
			if !errors.As(err, &prerequisite) || prerequisite.Code != "prerequisite_missing" || strings.Contains(prerequisite.Message, base) || w != (model.IntakeWorkspace{}) {
				t.Fatalf("incorrect sanitized prerequisite: %#v %v", w, err)
			}
			if _, err = os.Lstat(filepath.Join(m.cfg.WorkspaceDir, "intake")); !os.IsNotExist(err) {
				t.Fatalf("overlong namespace created: %v", err)
			}
		})
	}
}

func TestIntakeLimitsRejectWholeContextWithoutTrimming(t *testing.T) {
	deadline := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, count := range []int{intakeMaxFiles, intakeMaxFiles + 1} {
		s := model.IntakeSession{Deadline: deadline, InputFiles: map[string][]byte{}}
		for i := range count {
			s.InputFiles[fmt.Sprintf("inputs/%03d", i)] = []byte("full")
		}
		_, names, err := intakeSessionControl(s)
		if count == intakeMaxFiles && (err != nil || len(names) != count) {
			t.Fatalf("boundary silently trimmed: %d %v", len(names), err)
		}
		if count > intakeMaxFiles && err == nil {
			t.Fatal("file count limit not enforced")
		}
	}
	large := bytes.Repeat([]byte("x"), intakeInputMaxBytes+1)
	s := model.IntakeSession{Deadline: deadline, InputFiles: map[string][]byte{"inputs/too-large": large}}
	if _, _, err := intakeSessionControl(s); err == nil {
		t.Fatal("oversized file accepted")
	}
	s.InputFiles = map[string][]byte{"inputs/one": large[:intakeInputMaxBytes], "inputs/two": []byte("x")}
	if _, _, err := intakeSessionControl(s); err == nil {
		t.Fatal("complete total bound not enforced")
	}
}

func TestIntakeSourceAndPrivateReceiptSafety(t *testing.T) {
	for _, kind := range []string{"source-nonempty", "source-replaced", "receipt-symlink", "receipt-hardlink", "private-parent-symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, p, w, _ := intakeFixture(t)
			receipt := filepath.Join(m.intakeRecords(), m.intakeReceiptName(w.Path))
			var err error
			switch kind {
			case "source-nonempty":
				err = os.WriteFile(filepath.Join(w.ProjectRoot, "unexpected"), []byte("not empty"), 0600)
			case "source-replaced":
				if err = os.Rename(w.ProjectRoot, w.ProjectRoot+"-old"); err == nil {
					err = os.Mkdir(w.ProjectRoot, 0700)
				}
			case "receipt-symlink":
				if err = os.Rename(receipt, receipt+"-old"); err == nil {
					err = os.Symlink(receipt+"-old", receipt)
				}
			case "receipt-hardlink":
				err = os.Link(receipt, receipt+"-link")
			case "private-parent-symlink":
				if err = os.Rename(m.intakeRecords(), m.intakeRecords()+"-old"); err == nil {
					err = os.Symlink(m.intakeRecords()+"-old", m.intakeRecords())
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.PrepareIntake(context.Background(), "sample", p, "attempt", "request", "step"); err == nil {
				t.Fatal("unsafe private state accepted")
			}
		})
	}
}

func TestIntakeSessionSerializationDeterministic(t *testing.T) {
	s := model.IntakeSession{Deadline: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), InputFiles: map[string][]byte{"inputs/z": []byte("z"), "inputs/a": []byte("a")}}
	first, names, err := intakeSessionControl(s)
	if err != nil {
		t.Fatal(err)
	}
	s.InputFiles = map[string][]byte{"inputs/a": []byte("a"), "inputs/z": []byte("z")}
	second, again, err := intakeSessionControl(s)
	if err != nil || !bytes.Equal(first, second) || !reflect.DeepEqual(names, again) || !reflect.DeepEqual(names, []string{"inputs/a", "inputs/z"}) {
		t.Fatalf("unstable control serialization: %s %s %v", first, second, err)
	}
}

func TestIntakeInterruptedSetupPinsIntentBeforeFilesystemWrites(t *testing.T) {
	m, _, w, s := intakeFixture(t)
	ctx := context.Background()
	outside := t.TempDir()
	parent := filepath.Join(w.Path, "inputs")
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if err := m.ConfigureIntakeSession(ctx, w, s); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	changed := s
	changed.Deadline = changed.Deadline.Add(time.Minute)
	if err := m.ConfigureIntakeSession(ctx, w, changed); err == nil {
		t.Fatal("failed setup allowed deadline reenrollment")
	}
	changed = s
	changed.InputFiles = map[string][]byte{"inputs/other": []byte("new private context")}
	if err := m.ConfigureIntakeSession(ctx, w, changed); err == nil {
		t.Fatal("failed setup allowed context reenrollment")
	}
	if err := m.ConfigureIntakeSession(ctx, w, s); err != nil {
		t.Fatalf("exact interrupted retry failed: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("escaped input publication: %v %v", entries, err)
	}
}

func TestIntakeIgnoresDiscoveryAndLegacyNamespaces(t *testing.T) {
	m, p, initial, _ := intakeFixture(t)
	marker := []byte("private research context must not become intake context")
	legacy := filepath.Join(m.cfg.WorkspaceDir, "intake", digest("sample"), digest("request"), digest("step"), digest("fresh-attempt"))
	discovery := filepath.Join(m.cfg.WorkspaceDir, "discovery", digest("sample")[:32])
	for _, path := range []string{legacy, discovery, filepath.Join(m.cfg.StateDir, "discovery-git"), filepath.Join(m.cfg.StateDir, "discovery-sources")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "research-cache"), marker, 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := m.PrepareIntake(context.Background(), "sample", p, "fresh-attempt", "request", "step")
	if err != nil {
		t.Fatal(err)
	}
	if w.Path == legacy || w.ProjectRoot != initial.ProjectRoot {
		t.Fatalf("legacy/discovery identity reused: %#v", w)
	}
	if entries, err := os.ReadDir(w.Path); err != nil || len(entries) != 0 {
		t.Fatalf("inherited discovery context: %v %v", entries, err)
	}
	for _, path := range []string{legacy, discovery} {
		data, err := os.ReadFile(filepath.Join(path, "research-cache"))
		if err != nil || !bytes.Equal(data, marker) {
			t.Fatalf("unrelated research bytes changed: %q %v", data, err)
		}
	}
}

func TestIntakeResumesDurableNamespaceInitialization(t *testing.T) {
	for _, phase := range []string{"intent", "staged", "published", "directory-receipt"} {
		t.Run(phase, func(t *testing.T) {
			m, p, initial, _ := intakeFixture(t)
			path := filepath.Join(filepath.Dir(initial.Path), digest("recover-attempt")[:32])
			raw := []string{"intake", "sample", "request", "step", "recover-attempt"}
			intakeWriteJSON(t, filepath.Join(m.intakeRecords(), "directory-intent-"+digest(path)+".json"), raw)
			if phase != "intent" {
				stage := ".intake-stage-recovery"
				stagePath := filepath.Join(filepath.Dir(path), stage)
				if err := os.Mkdir(stagePath, 0700); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(stagePath)
				if err != nil {
					t.Fatal(err)
				}
				node, err := intakeIdentity(info)
				if err != nil {
					t.Fatal(err)
				}
				intakeWriteJSON(t, filepath.Join(m.intakeRecords(), "directory-stage-"+digest(path)+".json"), intakeDirectoryReceipt{Raw: raw, Node: node, Stage: stage})
				if phase != "staged" {
					if err = os.Rename(stagePath, path); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "directory-receipt" {
					intakeWriteJSON(t, filepath.Join(m.intakeRecords(), "namespace-"+digest(path)+".json"), intakeDirectoryReceipt{Raw: raw, Node: node})
				}
			}
			w, err := m.PrepareIntake(context.Background(), "sample", p, "recover-attempt", "request", "step")
			if err != nil || w.Path != path || w.ProjectRoot != initial.ProjectRoot {
				t.Fatalf("durable %s recovery failed: %#v %v", phase, w, err)
			}
			if entries, err := os.ReadDir(w.Path); err != nil || len(entries) != 0 {
				t.Fatalf("recovered workspace not empty: %v %v", entries, err)
			}
		})
	}
}

func TestIntakePendingIntentRejectsUnrecordedAndReplacedInodes(t *testing.T) {
	for _, phase := range []string{"unrecorded-target", "replaced-stage", "replaced-target"} {
		t.Run(phase, func(t *testing.T) {
			m, p, initial, _ := intakeFixture(t)
			path := filepath.Join(filepath.Dir(initial.Path), digest("recover-attempt")[:32])
			raw := []string{"intake", "sample", "request", "step", "recover-attempt"}
			intakeWriteJSON(t, filepath.Join(m.intakeRecords(), "directory-intent-"+digest(path)+".json"), raw)
			if phase == "unrecorded-target" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				stage := ".intake-stage-recovery"
				stagePath := filepath.Join(filepath.Dir(path), stage)
				if err := os.Mkdir(stagePath, 0700); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(stagePath)
				if err != nil {
					t.Fatal(err)
				}
				node, err := intakeIdentity(info)
				if err != nil {
					t.Fatal(err)
				}
				intakeWriteJSON(t, filepath.Join(m.intakeRecords(), "directory-stage-"+digest(path)+".json"), intakeDirectoryReceipt{Raw: raw, Node: node, Stage: stage})
				if phase == "replaced-stage" {
					if err = os.Rename(stagePath, stagePath+"-old"); err != nil {
						t.Fatal(err)
					}
					if err = os.Mkdir(stagePath, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					if err = os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := m.PrepareIntake(context.Background(), "sample", p, "recover-attempt", "request", "step"); err == nil {
				t.Fatalf("%s adopted an unrecorded inode", phase)
			}
		})
	}
}

func TestIntakeResumesPrivateSourceAfterPartialGitInitialization(t *testing.T) {
	for _, phase := range []string{"directories", "git-init"} {
		t.Run(phase, func(t *testing.T) {
			m, p, _, _ := intakeFixture(t)
			m.cfg.Projects["recover"] = p
			ctx := context.Background()
			private, err := m.intakePrivate()
			if err != nil {
				t.Fatal(err)
			}
			defer private.close()
			state := m.cfg.StateDir
			source := filepath.Join(state, "intake-sources", digest("recover"))
			common := filepath.Join(state, "intake-git", digest("recover")+".git")
			if err = intakeSave(ctx, private, "source-intent-"+digest("recover")+".json", []string{"recover", source, common}); err != nil {
				t.Fatal(err)
			}
			for _, parent := range []string{"intake-sources", "intake-git"} {
				c, err := intakeChain(state, parent)
				if err != nil {
					t.Fatal(err)
				}
				leaf := digest("recover")
				if parent == "intake-git" {
					leaf += ".git"
				}
				err = intakeEnsureDirectory(ctx, private, c, filepath.Join(state, parent, leaf), leaf, []string{parent, "recover"})
				c.close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if phase == "git-init" {
				if _, err = m.git(ctx, "", "init", "--bare", common); err != nil {
					t.Fatal(err)
				}
			}
			w, err := m.PrepareIntake(ctx, "recover", p, "recover-attempt", "request", "step")
			if err != nil || w.ProjectRoot != source || !shaPattern.MatchString(w.BaseSHA) {
				t.Fatalf("partial %s recovery failed: %#v %v", phase, w, err)
			}
			if _, err = os.Lstat(p.Repository); !os.IsNotExist(err) {
				t.Fatalf("recovery touched product repository: %v", err)
			}
			again, err := m.PrepareIntake(ctx, "recover", p, "recover-attempt", "request", "step")
			if err != nil || again != w {
				t.Fatalf("recovered source not stable: %#v %v", again, err)
			}
		})
	}
}

func TestIntakePolicyChangeAllowsFreshAttemptAndPersistedReopen(t *testing.T) {
	m, p, w, s := intakeFixture(t)
	ctx := context.Background()
	if err := m.ConfigureIntakeSession(ctx, w, s); err != nil {
		t.Fatal(err)
	}
	p.Branch = "new-policy"
	p.Checks = map[string]model.Command{"unit": {Argv: []string{"must-not-execute"}}}
	p.ProtectedPaths = []string{"protected"}
	p.MaxConcurrent = 2
	m.cfg.Projects["sample"] = p
	fresh, err := m.PrepareIntake(ctx, "sample", p, "fresh-policy-attempt", "request", "step")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Path == w.Path || fresh.ProjectRoot != w.ProjectRoot || fresh.BaseSHA != w.BaseSHA {
		t.Fatalf("fresh policy attempt lost stable source: %#v", fresh)
	}
	freshIDs, err := m.PrepareIntake(ctx, "sample", p, "new-attempt", "new-request", "new-step")
	if err != nil {
		t.Fatal(err)
	}
	if freshIDs.Path == w.Path || freshIDs.Path == fresh.Path || freshIDs.ProjectRoot != w.ProjectRoot || freshIDs.BaseSHA != w.BaseSHA {
		t.Fatalf("fresh namespace under changed policy lost stable source: %#v", freshIDs)
	}
	if _, err = m.PrepareIntake(ctx, "sample", p, "attempt", "request", "step"); err == nil {
		t.Fatal("old attempt accepted new enrollment")
	}
	if err = m.ConfigureIntakeSession(ctx, w, s); err == nil {
		t.Fatal("persisted reopen bypassed dispatch policy")
	}
	p.Trusted = false
	m.cfg.Projects["sample"] = p
	if _, err = m.PrepareIntake(ctx, "sample", p, "untrusted-attempt", "untrusted-request", "untrusted-step"); err == nil {
		t.Fatal("untrusted enrollment granted a fresh intake workspace")
	}
	reopened, err := m.ReopenIntake(ctx, w.Path, "sample", "attempt", "request", "step")
	if err != nil || reopened != w {
		t.Fatalf("persisted reopen failed: %#v %v", reopened, err)
	}
	delete(m.cfg.Projects, "sample")
	if _, err = m.ReopenIntake(ctx, w.Path, "sample", "attempt", "request", "step"); err != nil {
		t.Fatalf("removed enrollment blocked recovery: %v", err)
	}
	for _, identity := range [][4]string{{"other", "attempt", "request", "step"}, {"sample", "other", "request", "step"}, {"sample", "attempt", "other", "step"}, {"sample", "attempt", "request", "other"}} {
		if _, err = m.ReopenIntake(ctx, w.Path, identity[0], identity[1], identity[2], identity[3]); err == nil {
			t.Fatal("persisted reopen accepted mismatched full identity")
		}
	}
	if err = os.Rename(w.ProjectRoot, w.ProjectRoot+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(w.ProjectRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ReopenIntake(ctx, w.Path, "sample", "attempt", "request", "step"); err == nil {
		t.Fatal("persisted reopen accepted replaced source inode")
	}
}

func TestIntakeRecoversOnlyItsPrivateInputStagingSlot(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			m, _, w, s := intakeFixture(t)
			target := filepath.Join(w.Path, "inputs", "context.json")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			stageDir := filepath.Join(w.Path, ".openfactory")
			if err := os.Mkdir(stageDir, 0700); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(stageDir, ".intake-"+digest(filepath.Dir(target)+"\x00"+filepath.Base(target)))
			if err := os.WriteFile(stage, s.InputFiles["inputs/context.json"], 0400); err != nil {
				t.Fatal(err)
			}
			if published {
				if err := os.Rename(stage, target); err != nil {
					t.Fatal(err)
				}
			}
			unrelated := filepath.Join(stageDir, ".intake-unrecognized")
			if err := os.WriteFile(unrelated, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := m.ConfigureIntakeSession(context.Background(), w, s); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(stage); !os.IsNotExist(err) {
				t.Fatalf("recognized staging slot remains: %v", err)
			}
			if _, err := os.Lstat(unrelated); err != nil {
				t.Fatalf("unrecognized stage removed: %v", err)
			}
			if _, err := m.FingerprintIntake(context.Background(), w); err != nil {
				t.Fatalf("private abandoned stage contaminated fingerprint: %v", err)
			}
		})
	}
}

func TestIntakePublicationNeverReplacesExistingInput(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			m, _, w, s := intakeFixture(t)
			target := filepath.Join(w.Path, "inputs", "context.json")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			winner := []byte("other publisher's immutable winner")
			outside := filepath.Join(t.TempDir(), "winner")
			if err := os.WriteFile(outside, winner, 0400); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, winner, 0400); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.ConfigureIntakeSession(context.Background(), w, s); err == nil {
				t.Fatal("different winner accepted")
			}
			after, err := os.Lstat(target)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("existing winner replaced: %v", err)
			}
			content, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(content, winner) {
				t.Fatalf("existing winner modified: %q %v", content, err)
			}
		})
	}
}
