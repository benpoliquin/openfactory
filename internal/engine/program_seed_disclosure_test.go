package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"openfactory/internal/model"
)

// Import a real CAS manifest, including the empty manifest rather than the
// absent-bundle sentinel, and approve only a fully disclosed revision.
func programSeedDisclosureFixture(t *testing.T, seeds []model.ProgramSeed, content *string) (*programTestFixture, model.ProgramBundle) {
	t.Helper()
	ctx := context.Background()
	f := newProgramTestFixture(t)
	root := t.TempDir()
	var paths []string
	if content != nil {
		paths = []string{"private.txt"}
		if err := os.WriteFile(filepath.Join(root, paths[0]), []byte(*content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := f.repos.blobs.ImportBundle(ctx, root, paths, model.BundleLimits{MaxFiles: 4, MaxFileBytes: 1024, MaxTotalBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ManifestSHA == "" {
		t.Fatal("fixture must retain an exact seed manifest even without seed files")
	}
	stored, err := f.db.ProgramBundle(ctx, bundle.ManifestSHA)
	if err != nil || !reflect.DeepEqual(stored, bundle) {
		t.Fatalf("seed manifest did not round-trip through CAS: got %+v, want %+v, err=%v", stored, bundle, err)
	}
	for _, file := range bundle.Files {
		data, err := f.repos.blobs.Read(ctx, file.ContentSHA, file.Bytes)
		if err != nil || string(data) != *content || programDigest(data) != file.ContentSHA {
			t.Fatalf("seed bytes are not exact CAS evidence: file=%+v err=%v", file, err)
		}
	}
	spec := model.ProgramSpec{
		ID:        "seed-disclosure",
		Project:   "demo",
		Title:     "Exact seed disclosure",
		Objective: "Keep exact seed evidence behind explicit model disclosure",
		Seeds:     seeds,
		Disclosure: model.ProgramDisclosure{
			ModelProviderIDs:        []string{"test-only"},
			PublicSearchAllowed:     true,
			PrivateMaterialToModels: content != nil,
		},
		Implementation: model.ProgramImplementationPolicy{
			PermittedFeatureKinds:      []string{"feature"},
			PermittedCompletionTargets: []string{"integrated"},
		},
	}
	revision, err := f.db.ImportProgram(ctx, spec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	f.programIDs = append(f.programIDs, spec.ID)
	if revision.SeedManifestSHA != bundle.ManifestSHA {
		t.Fatalf("import discarded exact seed manifest: %+v", revision)
	}
	if err := f.db.ApproveProgram(ctx, spec.ID, revision.RevisionHash, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	return f, bundle
}

func TestProgramPromptEmptySeedManifestWithoutPrivateDisclosureDispatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seeds []model.ProgramSeed
	}{
		{name: "no explicit seeds"},
		{name: "public seed with explicit public consent", seeds: []model.ProgramSeed{{ID: "public", Kind: "public_url", URL: "https://example.com/protocol"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, bundle := programSeedDisclosureFixture(t, tc.seeds, nil)
			f.backend.idle = true
			f.until(t, "ordinary research dispatch with an exact empty seed bundle", func() bool {
				return len(f.backend.requests) > 0
			})
			if len(f.backend.requests) != 1 || len(f.repos.sessions) != 1 {
				t.Fatalf("expected one ordinary model dispatch and input materialization: requests=%d sessions=%d", len(f.backend.requests), len(f.repos.sessions))
			}
			request, session := f.backend.requests[0], f.repos.sessions[0]
			if request.Model != f.e.cfg.AstraModel || request.Workspace == "" || !strings.Contains(request.Prompt, "inputs/context.json") {
				t.Fatalf("empty bundle bypassed ordinary model dispatch: %+v", request)
			}
			if len(session.InputFiles) != 1 || session.InputFiles[0].Path != "inputs/context.json" {
				t.Fatalf("empty bundle should materialize context without evidence blobs: %+v", session.InputFiles)
			}
			var inputs struct {
				Revision model.ProgramRevision `json:"revision"`
				Bundles  []model.ProgramBundle `json:"frozen_bundles"`
				Evidence []programPromptBlob   `json:"evidence_blobs"`
			}
			if err := json.Unmarshal(session.InputFiles[0].Data, &inputs); err != nil {
				t.Fatal(err)
			}
			if inputs.Revision.Spec.Disclosure.PrivateMaterialToModels || !inputs.Revision.Spec.Disclosure.PublicSearchAllowed || !reflect.DeepEqual(inputs.Revision.Spec.Disclosure.ModelProviderIDs, []string{"test-only"}) {
				t.Fatalf("dispatch changed the explicit disclosure: %+v", inputs.Revision.Spec.Disclosure)
			}
			if !reflect.DeepEqual(inputs.Revision.Spec.Seeds, tc.seeds) || inputs.Revision.SeedManifestSHA != bundle.ManifestSHA || len(inputs.Bundles) != 1 || !reflect.DeepEqual(inputs.Bundles[0], bundle) || len(inputs.Evidence) != 0 {
				t.Fatalf("prompt lost the exact empty seed bundle: %+v", inputs)
			}
			materialized, err := os.ReadFile(filepath.Join(request.Workspace, "inputs", "context.json"))
			if err != nil || !bytes.Equal(materialized, session.InputFiles[0].Data) {
				t.Fatalf("spawned model did not receive exact immutable context: %v", err)
			}
		})
	}
}

func TestProgramPromptNonemptySeedManifestRequiresPrivateDisclosureWithoutExplicitSeeds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "private bytes", content: "PRIVATE_SEED_MUST_NOT_REACH_AN_UNCONSENTED_MODEL"},
		{name: "zero-byte private file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, bundle := programSeedDisclosureFixture(t, nil, &tc.content)
			if err := f.e.coordinatePrograms(ctx); err != nil {
				t.Fatal(err)
			}
			status := f.status(t, "seed-disclosure")
			if len(status.Steps) != 1 {
				t.Fatalf("expected one authorized research step: %+v", status.Steps)
			}
			step := status.Steps[0]
			work, err := f.db.ProgramStepInputs(ctx, step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(work.Revision.Spec.Seeds) != 0 || len(bundle.Files) != 1 || work.Revision.SeedManifestSHA != bundle.ManifestSHA {
				t.Fatalf("fixture must expose the nonempty manifest without any explicit bundle seed: %+v", work)
			}
			feature, err := f.db.Get(ctx, step.FeatureID)
			if err != nil {
				t.Fatal(err)
			}
			attempt := model.Attempt{FeatureID: feature.ID}
			prompt, files, err := f.e.programPrompt(ctx, attempt, feature, step, work)
			if err != nil || prompt == "" {
				t.Fatalf("fully consented immutable fixture must produce a prompt: %v", err)
			}
			blobPath := "inputs/blobs/" + bundle.Files[0].ContentSHA + ".txt"
			found := false
			for _, file := range files {
				if file.Path == blobPath {
					found = true
					if string(file.Data) != tc.content || programDigest(file.Data) != file.ContentSHA {
						t.Fatalf("consented prompt did not carry exact private bytes: %+v", file)
					}
				}
			}
			if !found {
				t.Fatal("positive control omitted the unlisted private seed file")
			}

			// Exercise the prompt's defense in depth without forging an approval,
			// changing persisted revisions, or weakening the Store's consent gate.
			work.Revision.Spec.Disclosure.PrivateMaterialToModels = false
			prompt, files, err = f.e.programPrompt(ctx, attempt, feature, step, work)
			if err == nil || !strings.Contains(err.Error(), "operator seed bundle evidence requires explicit private-material disclosure") {
				t.Fatalf("nonempty seed manifest without private consent was not denied: %v", err)
			}
			if prompt != "" || files != nil {
				t.Fatalf("denied prompt exposed model inputs: prompt=%q files=%+v", prompt, files)
			}
			if len(f.backend.requests) != 0 || len(f.repos.sessions) != 0 {
				t.Fatal("private evidence reached a model dispatch or materialized session before disclosure")
			}
			persisted, err := f.db.ProgramStepInputs(ctx, step.ID)
			if err != nil || !persisted.Revision.Spec.Disclosure.PrivateMaterialToModels {
				t.Fatalf("pure prompt boundary test changed the approved fixture: %v", err)
			}
		})
	}
}
