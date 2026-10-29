package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"openfactory/internal/model"
)

// These exercise the persisted consumer lifecycle after the trusted WebAuthn
// verifier seam, not cryptographic authenticator verification.
func manifestExecute(t *testing.T, s *Store, b model.BrowserSession, cfg model.Config, r model.IntakeRequest, action string, payload any, grant bool) model.IntakeRequest {
	t.Helper()
	command := lifecycleCommand(action, r, payload)
	var proof *CommandAuthorization
	if grant {
		_, authorization := commandTestAuthorization(t, s, b, command, cfg)
		proof = &authorization
	}
	receipt, err := s.ExecuteCommand(context.Background(), b.TokenHash, b.CSRFHash, command, cfg, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(receipt.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func manifestDraft(t *testing.T, s *Store, b model.BrowserSession, cfg model.Config, r model.IntakeRequest, bundles []string) model.IntakeRequest {
	t.Helper()
	disclosure, err := s.IntakeDisclosure(context.Background(), cfg, r.OperatorID, r.Project, r.ID, bundles)
	if err != nil {
		t.Fatal(err)
	}
	return manifestExecute(t, s, b, cfg, r, model.ActionIntakeDraft, model.IntakeDraftPayload{Disclosure: disclosure}, false)
}

func manifestCurrent(t *testing.T, s *Store, id string) model.IntakeRequest {
	t.Helper()
	r, err := s.IntakeRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIntakeManifestRealQuestionsDraftRetryAndHandoff(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	op, _, session := authTestActor(t, s, time.Now().UTC(), "manifest-lifecycle")
	cfg := commandTestConfig()
	cfg.Intake.MaxDraftCalls = 8
	cfg.Discovery.BundleMaxFiles, cfg.Discovery.BundleMaxFileBytes, cfg.Discovery.BundleMaxTotalBytes = 8, 4096, 8192
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("Exact private requirements, not a public seed.\n"))
	bundles := []string{bundle.ManifestSHA}
	r := commandTestCreateRequest(t, s, op, session, cfg)
	r = manifestDraft(t, s, session, cfg, r, bundles)
	step, attempt := lifecycleStart(t, s, cfg, r)
	lifecycleResult(t, s, cfg, step, attempt, `{"type":"questions","message":"Choose retention","questions":[{"local_id":"retention","text":"How long is data retained?","consequence":"Privacy boundary","answer_type":"text"}]}`)
	// Terminal current-thread cleanup must not erase the immutable session
	// provenance needed by all later worker inputs, retries and approval reviews.
	if _, err := s.db.ExecContext(ctx, `UPDATE attempts SET data=json_set(data,'$.thread_id','') WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	questions, err := s.IntakeQuestions(ctx, r.ID)
	if err != nil || len(questions) != 1 {
		t.Fatalf("bound question missing: %+v %v", questions, err)
	}
	q := questions[0]
	r = manifestCurrent(t, s, r.ID)
	r = manifestExecute(t, s, session, cfg, r, model.ActionTurnAppend, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Retain for one day.", Question: &model.IntakeQuestionRef{QuestionID: q.ID, ContentVersion: q.ContentVersion, InputManifestSHA: q.InputManifestSHA, OutputSHA: q.OutputSHA}}, false)
	r = manifestDraft(t, s, session, cfg, r, bundles)
	step, attempt = lifecycleStart(t, s, cfg, r)
	_, turns, _, err := s.IntakeStepInputs(ctx, step.ID)
	if err != nil || len(turns) != 3 || turns[1].ProducerStepID == "" || turns[2].Question == nil || *turns[2].Question != (model.IntakeQuestionRef{QuestionID: q.ID, ContentVersion: q.ContentVersion, InputManifestSHA: q.InputManifestSHA, OutputSHA: q.OutputSHA}) {
		t.Fatalf("worker lost exact producer/question context: %+v %v", turns, err)
	}
	lifecycleResult(t, s, cfg, step, attempt, lifecycleFeatureOutput(t, "manifest-product"))
	r = manifestCurrent(t, s, r.ID)
	firstDraft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	// A new draft includes the previous source, metadata and full raw result.
	r = manifestExecute(t, s, session, cfg, r, model.ActionTurnAppend, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Keep all prior requirements and metadata."}, false)
	r = manifestDraft(t, s, session, cfg, r, bundles)
	step, attempt = lifecycleStart(t, s, cfg, r)
	input, _, drafts, err := s.IntakeStepInputs(ctx, step.ID)
	if err != nil || len(drafts) != 1 || !reflect.DeepEqual(drafts[0], firstDraft) {
		t.Fatalf("worker changed immutable draft history: %+v %v", drafts, err)
	}
	kinds := map[string]bool{}
	for _, ref := range input.ContextManifest.Inputs {
		kinds[ref.Kind] = true
		if _, err = s.IntakeContextBlob(ctx, ref.SHA, ref.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"turn", "output", "draft", "draft_metadata", "capabilities", "bundle"} {
		if !kinds[kind] {
			t.Fatalf("real manifest omitted %s", kind)
		}
	}
	lifecycleResult(t, s, cfg, step, attempt, `{"type":"blocked","message":"Temporary prerequisite missing","missing_prerequisites":["An explicit retry decision"]}`)
	r = manifestCurrent(t, s, r.ID)
	// This review and grant reconstruct the full six-kind real manifest.
	r = manifestExecute(t, s, session, cfg, r, model.ActionIntakeRetry, model.EmptyCommandPayload{}, true)
	step, attempt = lifecycleStart(t, s, cfg, r)
	if _, _, retained, err := s.IntakeStepInputs(ctx, step.ID); err != nil || len(retained) != 1 {
		t.Fatalf("retry dropped historical context: %+v %v", retained, err)
	}
	lifecycleResult(t, s, cfg, step, attempt, lifecycleFeatureOutput(t, "manifest-product"))
	r = manifestCurrent(t, s, r.ID)
	draft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	r = manifestExecute(t, s, session, cfg, r, model.ActionIntakeApproveQueue, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision}, true)
	links, err := s.IntakeLinks(ctx, r.ID)
	if err != nil || r.State != model.IntakeSubmitted || len(links) != 1 || links[0].SubjectID != "manifest-product" {
		t.Fatalf("real model draft did not hand off: %+v %+v %v", r, links, err)
	}
	product, err := s.Get(ctx, "manifest-product")
	if err != nil || product.State != "ready" || product.PolicyHash != cfg.PolicyHash {
		t.Fatalf("handoff did not grant ordinary work: %+v %v", product, err)
	}
}

func TestIntakeManifestCommittedBundleContextSurvivesRevocation(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	op, _, session := authTestActor(t, s, time.Now().UTC(), "manifest-revoked")
	cfg := commandTestConfig()
	cfg.Discovery.BundleMaxFiles, cfg.Discovery.BundleMaxFileBytes, cfg.Discovery.BundleMaxTotalBytes = 8, 4096, 8192
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("Committed private context"))
	r := manifestDraft(t, s, session, cfg, commandTestCreateRequest(t, s, op, session, cfg), []string{bundle.ManifestSHA})
	if err := s.RevokeOperator(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	step, attempt := lifecycleStart(t, s, cfg, r)
	input, turns, _, err := s.IntakeStepInputs(ctx, step.ID)
	if err != nil || len(turns) != 1 || !reflect.DeepEqual(input.ContextManifest, step.Manifest) {
		t.Fatalf("revocation trimmed committed bundle/context: %+v %v", input, err)
	}
	lifecycleResult(t, s, cfg, step, attempt, lifecycleFeatureOutput(t, "manifest-revoked-product"))
	r = manifestCurrent(t, s, r.ID)
	draft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
	if err != nil || r.State != model.IntakeReviewReady {
		t.Fatalf("committed model output was rejected after revoke: %+v %v", r, err)
	}
	command := lifecycleCommand(model.ActionIntakeApproveQueue, r, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
	if _, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg); err == nil {
		t.Fatal("revoked operator received new review authority")
	}
}

func TestIntakeManifestConsumersRejectForgedProducerIdentity(t *testing.T) {
	for _, field := range []string{"producer", "request", "hash", "input_hash"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			op, _, session := authTestActor(t, s, time.Now().UTC(), "manifest-forged")
			cfg := commandTestConfig()
			r := manifestDraft(t, s, session, cfg, commandTestCreateRequest(t, s, op, session, cfg), nil)
			producer, attempt := lifecycleStart(t, s, cfg, r)
			lifecycleResult(t, s, cfg, producer, attempt, lifecycleFeatureOutput(t, "manifest-original"))
			r = manifestCurrent(t, s, r.ID)
			r = manifestExecute(t, s, session, cfg, r, model.ActionTurnAppend, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Preserve original output and draft."}, false)
			r = manifestDraft(t, s, session, cfg, r, nil)
			consumer, attempt := lifecycleStart(t, s, cfg, r)
			lifecycleResult(t, s, cfg, consumer, attempt, `{"type":"blocked","message":"Wait","missing_prerequisites":["Explicit retry"]}`)
			r = manifestCurrent(t, s, r.ID)
			stored, err := s.GetIntakeStep(ctx, producer.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "producer":
				stored.ID = "different-producer"
			case "request":
				stored.RequestID = "different-request"
			case "hash":
				stored.OutputSHA = authTestHash("different-output")
			case "input_hash":
				stored.InputManifestSHA = authTestHash("different-input")
			}
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.ExecContext(ctx, `UPDATE intake_steps SET data=? WHERE id=?`, raw, producer.ID); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = s.IntakeStepInputs(ctx, consumer.ID)
			intakeCommittedIntegrityError(t, err)
			command := lifecycleCommand(model.ActionIntakeRetry, r, model.EmptyCommandPayload{})
			_, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
			intakeCommittedIntegrityError(t, err)
		})
	}
}
