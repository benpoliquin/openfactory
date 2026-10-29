package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

type programCLIFixture struct {
	t                   *testing.T
	root, config, state string
}

func newProgramCLIFixture(t *testing.T, discovery string) *programCLIFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	f := &programCLIFixture{t: t, root: root, config: filepath.Join(root, "openfactory.yaml"), state: filepath.Join(root, "state")}
	// This is an ordinary enrolled check, never evidence that an experiment ran.
	f.write("openfactory.yaml", []byte("state_dir: "+f.state+"\nworkspace_dir: "+filepath.Join(root, "workspaces")+"\nprojects:\n  demo:\n    repository: "+repo+"\n    trusted: true\n    checks:\n      ordinary:\n        argv: [/usr/bin/git, diff, --check]\n"+discovery))
	return f
}
func (f *programCLIFixture) write(name string, data []byte) string {
	f.t.Helper()
	p := filepath.Join(f.root, name)
	if err := os.WriteFile(p, data, 0600); err != nil {
		f.t.Fatal(err)
	}
	return p
}
func (f *programCLIFixture) jsonFile(name string, v any) string {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.write(name, b)
}
func (f *programCLIFixture) invoke(args ...string) ([]byte, []byte, error) {
	f.t.Helper()
	var out, errout bytes.Buffer
	err := run(append([]string{"--config", f.config, "program"}, args...), &out, &errout)
	return out.Bytes(), errout.Bytes(), err
}
func (f *programCLIFixture) success(target any, args ...string) {
	f.t.Helper()
	out, stderr, err := f.invoke(args...)
	if err != nil {
		f.t.Fatalf("program %v: %v; stderr=%s", args, err, stderr)
	}
	if len(stderr) != 0 {
		f.t.Fatalf("successful command wrote stderr: %s", stderr)
	}
	if target != nil {
		if err := json.Unmarshal(out, target); err != nil {
			f.t.Fatalf("invalid JSON output %s: %v", out, err)
		}
	}
}
func (f *programCLIFixture) failure(code string, args ...string) {
	f.t.Helper()
	out, stderr, err := f.invoke(args...)
	if err == nil {
		f.t.Fatalf("program %v unexpectedly succeeded: %s", args, out)
	}
	if len(out) != 0 {
		f.t.Fatalf("failed mutation emitted successful output: %s", out)
	}
	var envelope struct {
		Error *model.ProgramError `json:"error"`
	}
	if e := json.Unmarshal(stderr, &envelope); e != nil || envelope.Error == nil || envelope.Error.Message == "" {
		f.t.Fatalf("missing JSON error envelope: %s (%v)", stderr, e)
	}
	if envelope.Error.Code != code {
		f.t.Fatalf("error code=%q, want %q: %s", envelope.Error.Code, code, stderr)
	}
}
func cliProgramSpec() model.ProgramSpec {
	return model.ProgramSpec{ID: "cli-discovery", Project: "demo", Title: "CLI authority", Objective: "Explore explicitly supplied evidence", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"openai"}}}
}
func (f *programCLIFixture) importSpec(spec model.ProgramSpec, args ...string) model.ProgramRevision {
	f.t.Helper()
	p := f.jsonFile("program.json", spec)
	var r model.ProgramRevision
	f.success(&r, append([]string{"import", p}, args...)...)
	return r
}
func cliSHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func TestProgramCLIPublicOnlyImportHasNoSyntheticBundle(t *testing.T) {
	f := newProgramCLIFixture(t, "")
	spec := cliProgramSpec()
	spec.Seeds = []model.ProgramSeed{{ID: "public", Kind: "public_url", URL: "https://example.com/research"}}
	r := f.importSpec(spec)
	if r.SeedManifestSHA != "" {
		t.Fatalf("public-only import created a seed manifest: %q", r.SeedManifestSHA)
	}
	if len(r.Spec.Seeds) != 1 || r.Spec.Seeds[0] != spec.Seeds[0] {
		t.Fatalf("public-only seeds changed: %+v", r.Spec.Seeds)
	}
	var status store.ProgramStatus
	f.success(&status, "status", spec.ID)
	if status.Revision.SeedManifestSHA != "" {
		t.Fatalf("persisted public-only revision has a seed manifest: %q", status.Revision.SeedManifestSHA)
	}
	entries, err := os.ReadDir(filepath.Join(f.state, "artifacts"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("public-only import wrote synthetic CAS content: %v", entries)
	}
}

func TestProgramCLIImportsRepositoryExample(t *testing.T) {
	f := newProgramCLIFixture(t, "")
	config, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.write("openfactory.yaml", bytes.Replace(config, []byte("\n  demo:\n"), []byte("\n  discovery-fixture:\n"), 1))
	examples := filepath.Join("..", "..", "examples")
	var r model.ProgramRevision
	f.success(&r, "import", filepath.Join(examples, "program.json"), "--bundle", filepath.Join(examples, "program.bundle.json"))
	if r.ProgramID != "event-ordering-discovery" || r.Spec.Project != "discovery-fixture" {
		t.Fatalf("unexpected example identity: %+v", r)
	}
	if kinds := r.Spec.Implementation.PermittedFeatureKinds; len(kinds) != 1 || kinds[0] != "feature" {
		t.Fatalf("example must use the canonical implementation feature kind: %v", kinds)
	}
	contents, err := os.ReadFile(filepath.Join(examples, "experiment.py"))
	if err != nil {
		t.Fatal(err)
	}
	cas := artifacts.New(filepath.Join(f.state, "artifacts"))
	bundle, err := cas.LoadBundle(context.Background(), r.SeedManifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Files) != 1 || bundle.Files[0].Path != "experiment.py" || bundle.Files[0].ContentSHA != cliSHA(contents) || bundle.Files[0].Bytes != int64(len(contents)) {
		t.Fatalf("example bundle differs from the repository seed: %+v", bundle)
	}
	got, err := cas.Read(context.Background(), bundle.Files[0].ContentSHA, int64(len(contents)))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("imported example seed differs: err=%v", err)
	}
}

func TestProgramCLIExplicitManifestCopiesOnlyDeclaredBytes(t *testing.T) {
	f := newProgramCLIFixture(t, "")
	declared := []byte("declared operator evidence\n")
	private := []byte("unlisted host secret must never enter CAS\n")
	f.write("notes.txt", declared)
	f.write("private.txt", private)
	// An unlisted dangling link must not even be traversed during import.
	if err := os.Symlink(filepath.Join(f.root, "missing"), filepath.Join(f.root, "unlisted-link")); err != nil {
		t.Fatal(err)
	}
	spec := cliProgramSpec()
	spec.Seeds = []model.ProgramSeed{{ID: "notes", Kind: "bundle", Path: "notes.txt"}}
	spec.Disclosure.PrivateMaterialToModels = true
	manifest := f.jsonFile("bundle.json", []string{"notes.txt"})
	r := f.importSpec(spec, "--bundle", manifest)
	cas := artifacts.New(filepath.Join(f.state, "artifacts"))
	bundle, err := cas.LoadBundle(context.Background(), r.SeedManifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Files) != 1 || bundle.Files[0].Path != "notes.txt" || bundle.Files[0].ContentSHA != cliSHA(declared) || bundle.Files[0].Bytes != int64(len(declared)) {
		t.Fatalf("unexpected imported manifest: %+v", bundle)
	}
	got, err := cas.Read(context.Background(), bundle.Files[0].ContentSHA, 1024)
	if err != nil || !bytes.Equal(got, declared) {
		t.Fatalf("CAS bytes=%q err=%v", got, err)
	}
	if _, err := cas.Read(context.Background(), cliSHA(private), 1024); err == nil {
		t.Fatal("unlisted file entered CAS")
	}
}

func TestProgramCLIRejectsUnsafeAndUnboundedBundles(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		manifest     any
		symlink      bool
		content      string
	}{
		{name: "traversal", manifest: []string{"../outside.txt"}},
		{name: "absolute", manifest: []string{"/etc/hosts"}},
		{name: "symlink", manifest: []string{"notes.txt"}, symlink: true},
		{name: "not-array", manifest: map[string]any{"files": []string{"notes.txt"}}},
		{name: "null", manifest: nil},
		{name: "file-cap", policy: "discovery:\n  bundle_max_file_bytes: 4\n", manifest: []string{"notes.txt"}, content: "12345"},
		{name: "count-cap", policy: "discovery:\n  bundle_max_files: 1\n", manifest: []string{"notes.txt", "other.txt"}},
		{name: "total-cap", policy: "discovery:\n  bundle_max_file_bytes: 4\n  bundle_max_total_bytes: 4\n", manifest: []string{"notes.txt", "other.txt"}, content: "123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProgramCLIFixture(t, tc.policy)
			content := tc.content
			if content == "" {
				content = "x"
			}
			f.write("other.txt", []byte(content))
			if tc.symlink {
				if err := os.Symlink(filepath.Join(f.root, "other.txt"), filepath.Join(f.root, "notes.txt")); err != nil {
					t.Fatal(err)
				}
			} else {
				f.write("notes.txt", []byte(content))
			}
			spec := cliProgramSpec()
			spec.Disclosure.PrivateMaterialToModels = true
			file := f.jsonFile("program.json", spec)
			manifest := f.jsonFile("bundle.json", tc.manifest)
			f.failure("invalid_contract", "import", file, "--bundle", manifest)
			var page store.ProgramPage
			f.success(&page, "status")
			if len(page.Items) != 0 {
				t.Fatal("rejected bundle created a program")
			}
		})
	}
}

func TestProgramCLIStrictCanonicalSpecAndFlags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"unknown", func(b []byte) []byte { return append([]byte(`{"unknown":true,`), b[1:]...) }},
		{"duplicate", func(b []byte) []byte { return append([]byte(`{"id":"shadow",`), b[1:]...) }},
		{"trailing", func(b []byte) []byte { return append(b, []byte(` {}`)...) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProgramCLIFixture(t, "")
			b, _ := json.Marshal(cliProgramSpec())
			p := f.write("invalid.json", tc.mutate(b))
			f.failure("invalid_contract", "import", p)
		})
	}
	f := newProgramCLIFixture(t, "")
	spec := cliProgramSpec()
	spec.Seeds = []model.ProgramSeed{{ID: "seed", Kind: "bundle", Path: "notes.txt"}}
	spec.Disclosure.PrivateMaterialToModels = true
	p := f.jsonFile("program.json", spec)
	f.failure("invalid_contract", "import", p)
	f.failure("invalid_contract", "import", p, "--bundle", "one", "--bundle", "two")
	f.failure("invalid_contract", "import", p, "--unknown", "value")
	spec = cliProgramSpec()
	spec.Limits.MaxModelCalls = 21
	p = f.jsonFile("program.json", spec)
	f.failure("invalid_contract", "import", p)
}

func TestProgramCLIPrivateBundleStorageDoesNotGrantModelConsent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bundleSeed  bool
		emptyBundle bool
	}{
		{name: "declared-bundle-seed", bundleSeed: true},
		{name: "no-bundle-seeds"},
		{name: "empty-bundle-without-seeds", emptyBundle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProgramCLIFixture(t, "")
			contents := []byte("Private operator input stored for review, not disclosed to a model.\n")
			f.write("notes.txt", contents)
			spec := cliProgramSpec()
			spec.Disclosure.PrivateMaterialToModels = false
			if tc.bundleSeed {
				spec.Seeds = []model.ProgramSeed{{ID: "local", Kind: "bundle", Path: "notes.txt"}}
			}
			paths := []string{"notes.txt"}
			if tc.emptyBundle {
				paths = []string{}
			}
			manifest := f.jsonFile("bundle.json", paths)
			revision := f.importSpec(spec, "--bundle", manifest)
			if revision.Spec.Disclosure.PrivateMaterialToModels || len(paths) > 0 && revision.SeedManifestSHA == "" {
				t.Fatalf("import granted disclosure or lost selected private material: %+v", revision)
			}
			var bundle model.ProgramBundle
			if revision.SeedManifestSHA != "" {
				var err error
				bundle, err = artifacts.New(filepath.Join(f.state, "artifacts")).LoadBundle(context.Background(), revision.SeedManifestSHA)
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(bundle.Files) != len(paths) || len(bundle.Files) > 0 && (bundle.Files[0].Path != "notes.txt" || bundle.Files[0].ContentSHA != cliSHA(contents)) {
				t.Fatalf("stored bundle differs from explicitly selected material: %+v", bundle)
			}
			var before store.ProgramStatus
			f.success(&before, "status", spec.ID)
			if before.Program.State != model.ProgramDraft || len(before.Grants) != 0 || len(before.Steps) != 0 || before.Budget.ModelCalls != 0 {
				t.Fatalf("shape-only import authorized or dispatched work: %+v", before)
			}
			var after store.ProgramStatus
			if tc.emptyBundle {
				// An empty manifest is not private material. The explicit provider
				// consent remains valid; no private-consent bit is synthesized.
				f.success(&after, "approve", spec.ID, "--revision", revision.RevisionHash)
				if after.Program.State != model.ProgramActive || len(after.Grants) != 1 || after.Grants[0].RevisionHash != revision.RevisionHash || after.Grants[0].SubjectHash != "" {
					t.Fatalf("empty bundle blocked explicit program authority or granted broader authority: %+v", after)
				}
			} else {
				// A model-readable nonempty bundle needs private consent;
				// omitting seed-path references must not imply that consent.
				f.failure("disclosure_required", "approve", spec.ID, "--revision", revision.RevisionHash)
				f.success(&after, "status", spec.ID)
				if after.Program.State != model.ProgramDraft || len(after.Grants) != 0 {
					t.Fatalf("undisclosed private bundle gained execution authority: %+v", after)
				}
			}
			if len(after.Steps) != 0 || after.Budget.ModelCalls != 0 || after.Revision.RevisionHash != revision.RevisionHash || after.Revision.SeedManifestSHA != revision.SeedManifestSHA || after.Revision.Spec.Disclosure.PrivateMaterialToModels {
				t.Fatalf("CLI storage/grant changed source, consent or dispatched a model: %+v", after)
			}
			db, err := store.Open(filepath.Join(f.state, "openfactory.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			outputs, err := db.ProgramArtifactPage(context.Background(), spec.ID, store.ProgramQuery{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			features, err := db.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(outputs.Items) != 0 || len(features) != 0 {
				t.Fatalf("CLI import/grant published model output: artifacts=%d features=%d", len(outputs.Items), len(features))
			}
		})
	}
}

func TestProgramCLIVersionBoundAuthority(t *testing.T) {
	f := newProgramCLIFixture(t, "")
	spec := cliProgramSpec()
	first := f.importSpec(spec)
	var status store.ProgramStatus
	f.success(&status, "approve", spec.ID, "--revision", first.RevisionHash)
	if status.Program.State != model.ProgramActive {
		t.Fatalf("approved state=%s", status.Program.State)
	}
	spec.Objective = "Changed scope requires a new exact approval"
	second := f.importSpec(spec)
	if second.Number != first.Number+1 || second.RevisionHash == first.RevisionHash {
		t.Fatalf("revision did not change: %+v", second)
	}
	for _, command := range []string{"approve", "cancel", "resume"} {
		f.failure("stale_revision", command, spec.ID, "--revision", first.RevisionHash)
	}
	f.success(&status, "status", spec.ID)
	if status.Program.State != model.ProgramDraft {
		t.Fatalf("new revision inherited approval: %+v", status.Program)
	}
	f.failure("approval_required", "resume", spec.ID, "--revision", second.RevisionHash)
	f.success(&status, "approve", spec.ID, "--revision", second.RevisionHash)
	f.success(&status, "cancel", spec.ID, "--revision", second.RevisionHash)
	if status.Program.State != model.ProgramCancelled {
		t.Fatalf("cancel state=%s", status.Program.State)
	}
}

// Fixtures persist typed CAS payloads and immutable envelopes, not successful
// research steps or experiment receipts. All authority mutations still use run.
func (f *programCLIFixture) artifact(r model.ProgramRevision, kind string, payload any) model.ProgramArtifact {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	blob, err := artifacts.New(filepath.Join(f.state, "artifacts")).Put(context.Background(), raw)
	if err != nil {
		f.t.Fatal(err)
	}
	a := model.ProgramArtifact{ID: "fixture-" + kind, ProgramID: r.ProgramID, Revision: r.Number, Kind: kind, SchemaVersion: 1, ContentSHA: blob.SHA, CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(a)
	if err != nil {
		f.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(f.state, "openfactory.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, a.ID, a.ProgramID, a.Revision, a.Kind, a.ContentSHA, data); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestProgramCLISeparateExactSubjectApprovals(t *testing.T) {
	f := newProgramCLIFixture(t, "")
	r := f.importSpec(cliProgramSpec())
	questions := f.artifact(r, model.ArtifactQuestions, model.QuestionsPayload{Questions: []model.ProgramQuestion{{ID: "q", Text: "Which durability boundary?", Route: "operator", Decisive: true}}})
	architecture := f.artifact(r, model.ArtifactArchitecture, model.ArchitecturePayload{})
	for _, tc := range []struct{ command, flag string }{{"approve-architecture", "--artifact"}, {"approve-experiment", "--contract"}} {
		f.failure("invalid_contract", tc.command, r.ProgramID, tc.flag, questions.ContentSHA)
		f.failure("stale_revision", tc.command, r.ProgramID, "--revision", strings.Repeat("f", 64), tc.flag, questions.ContentSHA)
		f.failure("invalid_contract", tc.command, r.ProgramID, "--revision", r.RevisionHash, tc.flag, "short")
		f.failure("evidence_invalid", tc.command, r.ProgramID, "--revision", r.RevisionHash, tc.flag, strings.Repeat("f", 64))
		f.failure("evidence_invalid", tc.command, r.ProgramID, "--revision", r.RevisionHash, tc.flag, questions.ContentSHA)
	}
	// An architecture hash is not an experiment contract approval subject.
	f.failure("evidence_invalid", "approve-experiment", r.ProgramID, "--revision", r.RevisionHash, "--contract", architecture.ContentSHA)
	f.failure("invalid_contract", "approve-architecture", r.ProgramID, "--revision", r.RevisionHash, "--contract", architecture.ContentSHA)
	f.failure("invalid_contract", "approve-experiment", r.ProgramID, "--revision", r.RevisionHash, "--artifact", architecture.ContentSHA)
	var status store.ProgramStatus
	f.success(&status, "status", r.ProgramID)
	if len(status.Grants) != 0 {
		t.Fatalf("rejected subject approval persisted grants: %+v", status.Grants)
	}
}

func TestProgramCLIAnswerRevisionDoesNotInheritApproval(t *testing.T) {
	f := newProgramCLIFixture(t, "")
	r := f.importSpec(cliProgramSpec())
	var status store.ProgramStatus
	f.success(&status, "approve", r.ProgramID, "--revision", r.RevisionHash)
	q := f.artifact(r, model.ArtifactQuestions, model.QuestionsPayload{Questions: []model.ProgramQuestion{{ID: "durability", Text: "Must acknowledged writes survive process restart?", Route: "operator", Decisive: true}}})
	answer := model.ProgramAnswer{QuestionArtifactID: q.ID, QuestionArtifactSHA: q.ContentSHA, QuestionID: "durability", Text: "Yes, acknowledged writes must survive process restart."}
	file := f.jsonFile("answer.json", answer)
	var next model.ProgramRevision
	f.success(&next, "answer", r.ProgramID, "--revision", r.RevisionHash, "--file", file)
	if next.Number != r.Number+1 || next.RevisionHash == r.RevisionHash {
		t.Fatalf("answer did not create revision: %+v", next)
	}
	f.failure("stale_revision", "approve", r.ProgramID, "--revision", r.RevisionHash)
	f.failure("stale_revision", "cancel", r.ProgramID, "--revision", r.RevisionHash)
	f.failure("approval_required", "resume", r.ProgramID, "--revision", next.RevisionHash)
	f.success(&status, "status", r.ProgramID)
	if status.Program.State != model.ProgramDraft {
		t.Fatalf("answer inherited spend authority: %+v", status.Program)
	}
}
