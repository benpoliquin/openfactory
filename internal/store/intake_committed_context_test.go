package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func intakeCommittedIntegrityError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("immutable context corruption was accepted")
	}
	var domain *model.ProgramError
	if errors.As(err, &domain) || errors.Is(err, ErrAuthRequired) || errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("persisted context corruption became a recoverable authority/domain error: %v", err)
	}
}

func intakeCommittedHistory(t *testing.T) (*Store, model.Config, model.IntakeRequest, string) {
	t.Helper()
	s := openTest(t)
	blobRoot := t.TempDir()
	s.blobs = artifacts.New(blobRoot)
	op, _, session := authTestActor(t, s, time.Now().UTC(), "committed-integrity")
	cfg := commandTestConfig()
	cfg.Discovery.BundleMaxFiles, cfg.Discovery.BundleMaxFileBytes, cfg.Discovery.BundleMaxTotalBytes = 8, 4096, 8192
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("Exact committed private context\n"))
	bundles := []string{bundle.ManifestSHA}
	r := manifestDraft(t, s, session, cfg, commandTestCreateRequest(t, s, op, session, cfg), bundles)
	producer, attempt := lifecycleStart(t, s, cfg, r)
	lifecycleResult(t, s, cfg, producer, attempt, lifecycleFeatureOutput(t, "committed-original-product"))
	r = manifestCurrent(t, s, r.ID)
	r = manifestExecute(t, s, session, cfg, r, model.ActionTurnAppend, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Retain the exact previous output, source and metadata."}, false)
	r = manifestDraft(t, s, session, cfg, r, bundles)
	kinds := map[string]bool{}
	for _, ref := range r.ContextManifest.Inputs {
		kinds[ref.Kind] = true
	}
	for _, kind := range []string{"turn", "output", "draft", "draft_metadata", "capabilities", "bundle"} {
		if !kinds[kind] {
			t.Fatalf("real committed history omitted %s", kind)
		}
	}
	return s, cfg, r, blobRoot
}

func TestIntakeManifestContextDigestPreservesHistoricalRepresentation(t *testing.T) {
	for _, bundles := range [][]string{nil, {}, {authTestHash("bundle")}} {
		m := model.IntakeManifest{RequestID: "committed-request", ContentVersion: 1, Project: "demo", PolicySHA: authTestHash("policy"), Inputs: []model.IntakeInputRef{{Kind: "turn", ID: "1", SHA: authTestHash("turn"), Bytes: 17}}, Disclosure: model.IntakeDisclosure{ModelProviderIDs: []string{"provider/model"}, BundleManifestSHAs: bundles}}
		// This is the original digest's field names/order and nil/empty slice
		// representation, independent of the shared producer/resolver helper.
		want := programHash(struct {
			Project   string
			Policy    string
			Inputs    []model.IntakeInputRef
			Providers []string
			Bundles   []string
		}{m.Project, m.PolicySHA, m.Inputs, m.Disclosure.ModelProviderIDs, m.Disclosure.BundleManifestSHAs})
		if intakeManifestContextSHA(m) != want {
			t.Fatal("context digest changed historical representation")
		}
		m.ContentVersion++
		if intakeManifestContextSHA(m) != want {
			t.Fatal("exact-context retry changed the disclosure root")
		}
		m.Inputs[0].Bytes++
		if intakeManifestContextSHA(m) == want {
			t.Fatal("context digest omitted exact input length")
		}
	}
}

func TestIntakeCommittedContextCorruptionIsFatalBeforeSettlement(t *testing.T) {
	for _, mutation := range []string{"manifest_hash", "context_hash", "project", "provider", "content_version", "bundle_disclosure", "input_bytes", "turn", "output", "draft", "draft_metadata", "capabilities", "bundle", "bundle_manifest", "output_producer", "grant"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			s, cfg, r, blobRoot := intakeCommittedHistory(t)
			if err := s.RevokeOperator(ctx, r.OperatorID); err != nil {
				t.Fatal(err)
			}
			step, attempt := lifecycleStart(t, s, cfg, r)
			r = manifestCurrent(t, s, r.ID)
			refs := map[string]model.IntakeInputRef{}
			for _, ref := range r.ContextManifest.Inputs {
				refs[ref.Kind] = ref
			}
			manifestChanged, contextChanged := false, false
			switch mutation {
			case "manifest_hash":
				r.ContextManifest.SHA = authTestHash("different-manifest")
				manifestChanged = true
			case "context_hash":
				r.ContextManifest.Disclosure.ContextSHA = authTestHash("different-context")
				manifestChanged = true
			case "project":
				r.ContextManifest.Project = "different-project"
				manifestChanged, contextChanged = true, true
			case "provider":
				r.ContextManifest.Disclosure.ModelProviderIDs = []string{"different/provider"}
				manifestChanged, contextChanged = true, true
			case "content_version":
				r.ContextManifest.ContentVersion++
				manifestChanged = true
			case "bundle_disclosure":
				r.ContextManifest.Disclosure.BundleManifestSHAs = []string{authTestHash("different-bundle")}
				manifestChanged, contextChanged = true, true
			case "input_bytes":
				r.ContextManifest.Inputs[0].Bytes++
				manifestChanged, contextChanged = true, true
			case "turn":
				if _, err := s.db.ExecContext(ctx, `UPDATE intake_turns SET data=json_set(data,'$.text','changed persisted turn') WHERE request_id=? AND sequence=?`, r.ID, refs["turn"].ID); err != nil {
					t.Fatal(err)
				}
			case "output":
				if _, err := s.db.ExecContext(ctx, `UPDATE attempts SET data=json_set(data,'$.summary','changed persisted output') WHERE id=(SELECT json_extract(data,'$.attempt_id') FROM intake_steps WHERE id=?)`, refs["output"].ID); err != nil {
					t.Fatal(err)
				}
			case "draft":
				if _, err := s.db.ExecContext(ctx, `UPDATE intake_drafts SET data=json_set(data,'$.canonical_source','changed persisted source') WHERE request_id=? AND revision=?`, r.ID, refs["draft"].ID); err != nil {
					t.Fatal(err)
				}
			case "draft_metadata":
				if _, err := s.db.ExecContext(ctx, `UPDATE intake_drafts SET data=json_set(data,'$.domain_revision',?) WHERE request_id=? AND revision=?`, authTestHash("changed-domain-revision"), r.ID, refs["draft_metadata"].ID); err != nil {
					t.Fatal(err)
				}
			case "capabilities", "bundle", "bundle_manifest":
				sha := refs[mutation].SHA
				if mutation == "bundle_manifest" {
					sha = r.ContextManifest.Disclosure.BundleManifestSHAs[0]
				}
				raw, err := s.blobs.Read(ctx, sha, 4<<20)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] ^= 1
				if err = os.WriteFile(filepath.Join(blobRoot, sha), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "output_producer":
				if _, err := s.db.ExecContext(ctx, `UPDATE intake_steps SET data=json_set(data,'$.input_manifest_sha',?) WHERE id=?`, authTestHash("changed-producer-context"), refs["output"].ID); err != nil {
					t.Fatal(err)
				}
			case "grant":
				if _, err := s.db.ExecContext(ctx, `UPDATE intake_draft_authorizations SET data=json_set(data,'$.audit.operator_id','different-operator') WHERE request_id=? AND content_version=?`, r.ID, r.ContentVersion); err != nil {
					t.Fatal(err)
				}
			}
			if manifestChanged {
				if contextChanged {
					r.ContextManifest.Disclosure.ContextSHA = intakeManifestContextSHA(r.ContextManifest)
				}
				if mutation != "manifest_hash" {
					r.ContextManifest.SHA = ""
					r.ContextManifest.SHA = programHash(r.ContextManifest)
				}
				step.Manifest = r.ContextManifest
				step.InputManifestSHA = r.ContextManifest.SHA
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = intakePutRequestTx(ctx, tx, r); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = intakePutStepTx(ctx, tx, step, true); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			_, _, _, err := s.IntakeStepInputs(ctx, step.ID)
			intakeCommittedIntegrityError(t, err)
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			source, err := intake.Export(feature("committed-next-product"))
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			_, err = s.intakeCanonicalCommittedTx(ctx, tx, "feature", model.IntakeFormatFeature, source, r, cfg)
			tx.Rollback()
			intakeCommittedIntegrityError(t, err)
			attempt.Summary = lifecycleFeatureOutput(t, "committed-next-product")
			parsed, err := intake.ParseModelOutput([]byte(attempt.Summary), 256<<10)
			if err != nil {
				t.Fatal(err)
			}
			err = s.ApplyIntakeResult(ctx, attempt, step, parsed, cfg)
			intakeCommittedIntegrityError(t, err)
			commandTestCount(t, s, `SELECT count(*) FROM intake_drafts WHERE request_id=?`, 1, r.ID)
			commandTestCount(t, s, `SELECT count(*) FROM intake_links WHERE request_id=?`, 0, r.ID)
			commandTestCount(t, s, `SELECT count(*) FROM intake_draft_authorizations WHERE request_id=?`, 2, r.ID)
			commandTestCount(t, s, `SELECT count(*) FROM features WHERE id=?`, 0, "committed-next-product")
			active, err := s.Active(ctx)
			if err != nil || len(active) != 1 || active[0].ID != attempt.ID {
				t.Fatalf("fatal context corruption released attempt ownership: %+v %v", active, err)
			}
			stored, err := s.GetIntakeStep(ctx, step.ID)
			if err != nil || stored.OutputSHA != "" || stored.State != step.State {
				t.Fatalf("fatal context corruption partially settled the producer: %+v %v", stored, err)
			}
			current := manifestCurrent(t, s, r.ID)
			if current.State != model.IntakeDrafting || current.ActiveStepID != step.ID || current.LatestDraftRevision != 1 {
				t.Fatalf("fatal context corruption changed request ownership or accepted a draft: %+v", current)
			}
		})
	}
}

func TestIntakeCommittedContextInvalidModelSourceRemainsDomainFinding(t *testing.T) {
	ctx := context.Background()
	s, cfg, r, _ := intakeCommittedHistory(t)
	step, attempt := lifecycleStart(t, s, cfg, r)
	output, err := json.Marshal(map[string]any{"type": "draft", "message": "Invalid canonical source", "route": "feature", "format": model.IntakeFormatFeature, "canonical_source": "not a canonical feature contract", "assumptions": []string{}, "unresolved_risks": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	lifecycleResult(t, s, cfg, step, attempt, string(output))
	current := manifestCurrent(t, s, r.ID)
	draft, err := s.IntakeDraft(ctx, r.ID, current.LatestDraftRevision)
	if err != nil || current.State != model.IntakeBlocked || current.ActiveStepID != "" || len(draft.Findings) == 0 || draft.Findings[0].Code != "invalid_contract" || draft.CanonicalSource != "not a canonical feature contract" {
		t.Fatalf("untrusted invalid model source became a fatal store error or lost findings: %+v %+v %v", current, draft, err)
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("ordinary invalid model source retained external ownership: %+v %v", active, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM intake_drafts WHERE request_id=?`, 2, r.ID)
	commandTestCount(t, s, `SELECT count(*) FROM intake_links WHERE request_id=?`, 0, r.ID)
	commandTestCount(t, s, `SELECT count(*) FROM intake_draft_authorizations WHERE request_id=?`, 2, r.ID)
}
