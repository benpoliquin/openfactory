package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func commandTestConfig() model.Config {
	return model.Config{
		PolicyHash: authTestHash("command-policy"), AstraModel: "test",
		MaxAttempts: 3, MaxConcurrent: 1, MaxCallsPerDay: 10,
		Intake:   model.IntakePolicy{Enabled: true, MaxDraftCalls: 3},
		Projects: map[string]model.Project{"demo": {Trusted: true, MaxConcurrent: 1, Checks: map[string]model.Command{"unit": {}}}},
	}
}

func commandTestCommand(t *testing.T, id, action string, payload any) model.TypedCommand {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return model.TypedCommand{CommandID: id, Action: action, Payload: raw}
}

func commandTestCreateRequest(t *testing.T, s *Store, o model.Operator, b model.BrowserSession, cfg model.Config) model.IntakeRequest {
	t.Helper()
	ctx := context.Background()
	text := "Draft a bounded feature for durable acknowledgements."
	command := commandTestCommand(t, "00000000-0000-4000-8000-000000000001", model.ActionRequestCreate, model.RequestCreatePayload{
		Project: "demo", Text: text, ClientMessageID: "10000000-0000-4000-8000-000000000001",
	})
	receipt, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var request model.IntakeRequest
	if err = json.Unmarshal(receipt.Result, &request); err != nil {
		t.Fatal(err)
	}
	if request.State != model.IntakeCollecting || request.OperatorID != o.ID {
		t.Fatalf("saving request must collect without authorizing a model: %+v", request)
	}
	return request
}

func commandTestDraftCommand(t *testing.T, s *Store, r model.IntakeRequest, cfg model.Config) model.TypedCommand {
	t.Helper()
	disclosure, err := s.IntakeDisclosure(context.Background(), cfg, r.OperatorID, r.Project, r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := commandTestCommand(t, "00000000-0000-4000-8000-000000000060", model.ActionIntakeDraft, model.IntakeDraftPayload{Disclosure: disclosure})
	command.RequestID, command.ExpectedContentVersion = r.ID, &r.ContentVersion
	return command
}

// This fixture starts AFTER library verification at the trusted Store boundary.
// Its inert opaque credential/assertion data proves persistence atomicity only;
// operatorauth's real WebAuthn ceremonies must prove cryptographic verification.
func commandTestAuthorization(t *testing.T, s *Store, b model.BrowserSession, command model.TypedCommand, cfg model.Config) (model.IntakeReview, CommandAuthorization) {
	t.Helper()
	ctx := context.Background()
	review, err := s.CreateIntakeReview(ctx, b.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := intake.CommandDigest(command)
	if err != nil {
		t.Fatal(err)
	}
	challenge := model.AuthChallenge{
		ID: review.Claims.ChallengeID, Purpose: "command", OperatorID: b.OperatorID, CredentialID: b.CredentialID, SessionID: b.ID,
		CommandID: command.CommandID, CommandDigest: digest, ClaimDigest: review.ClaimsSHA, ReviewID: review.ID,
		SessionData: json.RawMessage(`{"opaque":"verified-library-session"}`), CreatedAt: review.Claims.IssuedAt, ExpiresAt: review.Claims.ExpiresAt,
	}
	if err = s.CreateAuthChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	credential, err := s.GetCredential(ctx, b.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	credential.SignCount++
	credential.CredentialData = json.RawMessage(`{"opaque":"updated-library-credential"}`)
	return review, CommandAuthorization{ChallengeID: challenge.ID, Credential: credential, Evidence: model.WebAuthnEvidence{
		ChallengeID: challenge.ID, CredentialID: credential.ID, UserPresent: true, UserVerified: true,
		AssertionSHA: authTestHash("trusted-verifier-output"), SignCount: credential.SignCount, VerifiedAt: time.Now().UTC(),
	}}
}

func commandTestCount(t *testing.T, s *Store, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil || got != want {
		t.Fatalf("persisted count: got %d, want %d: %v (query %s)", got, want, err, query)
	}
}

func TestExecuteCommandIntakeReceiptPrecedesContentVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	o, _, b := authTestActor(t, s, time.Now().UTC(), "content")
	cfg := commandTestConfig()
	r := commandTestCreateRequest(t, s, o, b, cfg)
	oldConsent := commandTestDraftCommand(t, s, r, cfg)
	text := "Only acknowledge after the durable write finishes."
	command := commandTestCommand(t, "00000000-0000-4000-8000-000000000002", model.ActionTurnAppend, model.TurnAppendPayload{
		ClientMessageID: "10000000-0000-4000-8000-000000000002", Text: text,
	})
	command.RequestID, command.ExpectedContentVersion = r.ID, &r.ContentVersion
	first, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("lost-response replay did not return the committed receipt: %+v %v", replay, err)
	}
	changed := commandTestCommand(t, command.CommandID, command.Action, model.TurnAppendPayload{
		ClientMessageID: "10000000-0000-4000-8000-000000000002", Text: text + " Changed.",
	})
	changed.RequestID, changed.ExpectedContentVersion = command.RequestID, command.ExpectedContentVersion
	_, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, changed, cfg, nil)
	answerErrorCode(t, err, "idempotency_conflict")
	freshID := command
	freshID.CommandID = "00000000-0000-4000-8000-000000000003"
	_, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, freshID, cfg, nil)
	answerErrorCode(t, err, "stale_revision")
	stored, err := s.IntakeRequest(ctx, r.ID)
	if err != nil || stored.ContentVersion != r.ContentVersion+1 || stored.State != model.IntakeCollecting {
		t.Fatalf("saving or replaying text changed content incorrectly or authorized a model: %+v %v", stored, err)
	}
	// A failed draft may not erase the newer saved text or silently authorize
	// the older context just because the caller supplies the current version.
	oldConsent.ExpectedContentVersion = &stored.ContentVersion
	_, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, oldConsent, cfg, nil)
	answerErrorCode(t, err, "disclosure_required")
	turns, err := s.IntakeTurns(ctx, r.ID, 0, 50)
	if err != nil || len(turns) != 2 || turns[1].Text != text || turns[1].Audit.OperatorID != o.ID {
		t.Fatalf("turn effect not exactly once/server attributed: %+v %v", turns, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM intake_events WHERE request_id=? AND json_extract(data,'$.kind')='turn_appended'`, 1, r.ID)
	commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 2)
	// Saving and read-only refreshes must never spend draft/call capacity or
	// create scheduler work, including when the exact command is replayed.
	for range 2 {
		if _, err = s.IntakeRequest(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.IntakeTurns(ctx, r.ID, 0, 50); err != nil {
			t.Fatal(err)
		}
		if _, err = s.IntakeEvents(ctx, r.ID, 0, 50); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Status(ctx, model.StatusQuery{}, cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"features", "attempts", "calls", "intake_steps", "intake_dispatches"} {
		commandTestCount(t, s, `SELECT count(*) FROM `+table, 0)
	}
	if _, err = s.CreateIntakeStep(ctx, stored.ID, stored.ContentVersion, cfg); err == nil {
		t.Fatal("saved text was schedulable without explicit exact-context draft consent")
	} else {
		answerErrorCode(t, err, "stale_revision")
	}
}

func TestExecuteCommandProgramAnswerReplaysBeforeDomainStaleCheck(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_, _, b := authTestActor(t, s, time.Now().UTC(), "answer")
	cfg := commandTestConfig()
	r, q := answerProgramFixture(t, s, "command-answer")
	answer := answerForQuestion(q)
	payload := model.ProgramAnswerPayload{ProgramID: r.ProgramID, RevisionHash: r.RevisionHash, QuestionArtifactID: q.ID,
		QuestionArtifactSHA: q.ContentSHA, QuestionID: answer.QuestionID, Text: answer.Text, SupportingRefs: answer.SupportingRefs}
	command := commandTestCommand(t, "00000000-0000-4000-8000-000000000010", model.ActionProgramAnswer, payload)
	first, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var next model.ProgramRevision
	if err = json.Unmarshal(first.Result, &next); err != nil {
		t.Fatal(err)
	}
	p, err := programLoad(ctx, s.db, r.ProgramID)
	if err != nil || p.CurrentRevision != r.Number+1 || p.State != model.ProgramDraft || next.RevisionHash == r.RevisionHash {
		t.Fatalf("answer did not create a new unapproved domain revision: %+v %+v %v", p, next, err)
	}
	// The domain API deliberately rejects this stale predecessor even for an
	// identical answer. The outer command boundary must return its tombstone first.
	_, err = s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	answerErrorCode(t, err, "stale_revision")
	replay, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("committed program answer lost to stale domain precondition: %+v %v", replay, err)
	}
	payload.Text += " Changed."
	changed := commandTestCommand(t, command.CommandID, command.Action, payload)
	_, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, changed, cfg, nil)
	answerErrorCode(t, err, "idempotency_conflict")
	freshID := command
	freshID.CommandID = "00000000-0000-4000-8000-000000000011"
	_, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, freshID, cfg, nil)
	answerErrorCode(t, err, "stale_revision")
	commandTestCount(t, s, `SELECT count(*) FROM program_answers WHERE program_id=?`, 1, r.ProgramID)
	commandTestCount(t, s, `SELECT count(*) FROM program_revisions WHERE program_id=?`, 2, r.ProgramID)
	commandTestCount(t, s, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=?`, 0, r.ProgramID, next.RevisionHash)
	commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 1)
}

func TestExecuteCommandReplayStillRequiresCurrentAccess(t *testing.T) {
	for _, kind := range []string{"operator revoked", "credential revoked", "session revoked", "scope removed", "project removed", "csrf mismatch"} {
		t.Run(kind, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			o, c, b := authTestActor(t, s, time.Now().UTC(), "access")
			cfg := commandTestConfig()
			f, err := s.Import(ctx, feature("cancel-access"))
			if err != nil {
				t.Fatal(err)
			}
			command := commandTestCommand(t, "00000000-0000-4000-8000-000000000020", model.ActionSubjectCancel, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil); err != nil {
				t.Fatal(err)
			}
			csrf := b.CSRFHash
			want := ErrAuthRequired
			switch kind {
			case "operator revoked":
				err = s.RevokeOperator(ctx, o.ID)
			case "credential revoked":
				err = s.RevokeCredential(ctx, c.ID)
			case "session revoked":
				err = s.RevokeBrowserSession(ctx, b.ID)
			case "scope removed":
				o.ProjectScope = []string{"other"}
				authTestInsert(t, s, "intake_operators", o.ID, o)
				want = ErrProjectForbidden
			case "project removed":
				delete(cfg.Projects, "demo")
				want = ErrProjectForbidden
			case "csrf mismatch":
				csrf, want = authTestHash("wrong-csrf"), ErrCSRF
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, csrf, command, cfg, nil); !errors.Is(err, want) {
				t.Fatalf("committed receipt bypassed live access: got %v, want %v", err, want)
			}
			if kind == "scope removed" || kind == "operator revoked" {
				if _, err = s.GetCommandReceipt(ctx, o.ID, command.CommandID); !errors.Is(err, want) {
					t.Fatalf("receipt lookup bypassed live operator/project access: %v", err)
				}
			}
			commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 1)
		})
	}
}

func TestExecuteCommandGrantRollsBackAllAuthorityWithReceiptFailure(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_, beforeCredential, b := authTestActor(t, s, time.Now().UTC(), "rollback")
	cfg := commandTestConfig()
	f, err := s.Import(ctx, feature("approval-rollback"))
	if err != nil {
		t.Fatal(err)
	}
	command := commandTestCommand(t, "00000000-0000-4000-8000-000000000030", model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
	_, proof := commandTestAuthorization(t, s, b, command, cfg)
	before, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Abort after challenge/counter, approval, domain mutation and audit writes.
	// A late receipt failure must roll the whole transaction back, not just auth.
	if _, err = s.db.ExecContext(ctx, `CREATE TRIGGER command_receipt_failure BEFORE INSERT ON intake_commands BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, &proof); err == nil || !strings.Contains(err.Error(), "injected receipt failure") {
		t.Fatalf("did not reach injected late transaction failure: %v", err)
	}
	after, err := s.Get(ctx, f.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("domain approval escaped rollback: %+v %v", after, err)
	}
	credential, err := s.GetCredential(ctx, b.CredentialID)
	if err != nil || !reflect.DeepEqual(credential, beforeCredential) {
		t.Fatalf("credential state escaped rollback: %+v %v", credential, err)
	}
	challenge, err := s.GetAuthChallenge(ctx, proof.ChallengeID)
	if err != nil || challenge.ConsumedAt != nil {
		t.Fatalf("challenge escaped rollback: %+v %v", challenge, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM intake_approval_receipts`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM intake_audits`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM events WHERE feature_id=? AND kind='approved'`, 0, f.ID)
	if _, err = s.GetCommandReceipt(ctx, b.OperatorID, command.CommandID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed command left a receipt: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, `DROP TRIGGER command_receipt_failure`); err != nil {
		t.Fatal(err)
	}
	// The exact already-verified result remains usable after a storage rollback.
	if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, &proof); err != nil {
		t.Fatalf("rollback stranded the unconsumed authorization: %v", err)
	}
}

func TestExecuteCommandConcurrentExactGrantConsumesOnceAndReplaysReceipt(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "commands.sqlite")
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	other, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	o, _, b := authTestActor(t, s, time.Now().UTC(), "concurrent")
	cfg := commandTestConfig()
	f, err := s.Import(ctx, feature("approve-once"))
	if err != nil {
		t.Fatal(err)
	}
	command := commandTestCommand(t, "00000000-0000-4000-8000-000000000040", model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
	review, proof := commandTestAuthorization(t, s, b, command, cfg)
	// UP and UV are independently mandatory even at this already-verified seam.
	for _, flag := range []string{"presence", "verification"} {
		bad := proof
		if flag == "presence" {
			bad.Evidence.UserPresent = false
		} else {
			bad.Evidence.UserVerified = false
		}
		_, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, &bad)
		answerErrorCode(t, err, "human_verification_required")
	}
	start := make(chan struct{})
	type result struct {
		receipt model.CommandReceipt
		err     error
	}
	results := make(chan result, 8)
	var ready sync.WaitGroup
	if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil); err == nil {
		t.Fatal("new execution grant accepted without exact-command verification")
	} else {
		answerErrorCode(t, err, "human_verification_required")
	}
	ready.Add(cap(results))
	for i := range cap(results) {
		db := s
		if i%2 != 0 {
			db = other
		}
		go func() {
			ready.Done()
			<-start
			receipt, err := db.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, &proof)
			results <- result{receipt, err}
		}()
	}
	ready.Wait()
	close(start)
	var first model.CommandReceipt
	for i := range cap(results) {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent identical command: %v", got.err)
		}
		if i == 0 {
			first = got.receipt
		} else if !reflect.DeepEqual(got.receipt, first) {
			t.Fatalf("race returned different receipts: %+v %+v", first, got.receipt)
		}
	}
	stored, err := s.Get(ctx, f.ID)
	if err != nil || stored.State != "ready" || stored.PolicyHash != cfg.PolicyHash {
		t.Fatalf("exact grant did not authorize real feature: %+v %v", stored, err)
	}
	challenge, err := s.GetAuthChallenge(ctx, proof.ChallengeID)
	if err != nil || challenge.ConsumedAt == nil {
		t.Fatalf("successful challenge not consumed: %+v %v", challenge, err)
	}
	credential, err := s.GetCredential(ctx, b.CredentialID)
	if err != nil || credential.SignCount != proof.Credential.SignCount || !reflect.DeepEqual(credential.CredentialData, proof.Credential.CredentialData) {
		t.Fatalf("verifier-updated credential not persisted exactly once: %+v %v", credential, err)
	}
	approval, err := s.GetApprovalReceipt(ctx, first.ApprovalID)
	if err != nil || approval.ReviewID != review.ID || approval.ClaimsSHA != review.ClaimsSHA || approval.Evidence != proof.Evidence || approval.Audit != first.Audit {
		t.Fatalf("exact approval evidence/binding not retained: %+v %v", approval, err)
	}
	if first.Audit.OperatorID != o.ID || first.Audit.CommandID != command.CommandID || first.Audit.Kind != model.AuditVerifiedOperator {
		t.Fatalf("wrong command audit authority: %+v", first.Audit)
	}
	commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM intake_approval_receipts`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM events WHERE feature_id=? AND kind='approved'`, 1, f.ID)
	commandTestCount(t, s, `SELECT count(*) FROM intake_audits WHERE subject_kind='feature' AND subject_id=? AND action='approved'`, 1, f.ID)
	// Committed replay needs no fresh proof, even after policy and domain revision
	// change; it returns history, never re-approves the replacement contract.
	changed := feature(f.ID)
	changed.Body += " New scope requires a distinct review."
	next, err := s.Import(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PolicyHash = authTestHash("replacement-policy")
	replay, err := other.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("historical grant replay rechecked stale authority: %+v %v", replay, err)
	}
	stored, err = s.Get(ctx, next.ID)
	if err != nil || stored.Revision != next.Revision || stored.State != "proposed" || stored.PolicyHash != "" {
		t.Fatalf("receipt replay authorized replacement revision: %+v %v", stored, err)
	}
}

func TestIntakeInternalWorkIsHiddenButRetainsCapacityAndCannotBeForged(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	o, _, b := authTestActor(t, s, time.Now().UTC(), "internal")
	cfg := commandTestConfig()
	r := commandTestCreateRequest(t, s, o, b, cfg)
	draft := commandTestDraftCommand(t, s, r, cfg)
	draftReceipt, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, draft, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(draftReceipt.Result, &r); err != nil {
		t.Fatal(err)
	}
	step, err := s.CreateIntakeStep(ctx, r.ID, r.ContentVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := s.Import(ctx, feature("visible-product"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, ordinary.ID, ordinary.Revision, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(ctx, feature("visible-unapproved")); err != nil {
		t.Fatal(err)
	}
	cfg.Projects["other"] = model.Project{Trusted: true, MaxConcurrent: 1}
	a, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, a, "intake", a.ID+":intake", cfg.MaxCallsPerDay); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Status(ctx, model.StatusQuery{}, cfg)
	if err != nil || len(snapshot.Features) != 2 || snapshot.Features[0].Feature.ID != ordinary.ID || snapshot.Limits.Global.Active != 1 || snapshot.IntakeStepCount != 1 || snapshot.Calls.Used != 1 {
		t.Fatalf("hidden drafting work distorted product board/capacity: %+v %v", snapshot, err)
	}
	if len(snapshot.Limits.Projects) != 2 || snapshot.Limits.Projects[0].Name != "demo" || snapshot.Limits.Projects[0].Active != 1 {
		t.Fatalf("internal work disappeared from project capacity: %+v", snapshot.Limits)
	}
	firstPage, err := s.Status(ctx, model.StatusQuery{Limit: 1}, cfg)
	if err != nil || firstPage.NextCursor == "" || firstPage.IntakeStepCount != 1 {
		t.Fatalf("first product page lost global internal activity: %+v %v", firstPage, err)
	}
	secondPage, err := s.Status(ctx, model.StatusQuery{Limit: 1, After: firstPage.NextCursor}, cfg)
	if err != nil || len(secondPage.Features) != 1 || secondPage.IntakeStepCount != 1 || secondPage.Limits.Global.Active != 1 {
		t.Fatalf("paginated product page lost global internal activity: %+v %v", secondPage, err)
	}
	for _, query := range []model.StatusQuery{{Project: "other"}, {State: "failed"}} {
		filtered, err := s.Status(ctx, query, cfg)
		if err != nil || len(filtered.Features) != 0 || filtered.IntakeStepCount != 1 || filtered.Limits.Global.Active != 1 {
			t.Fatalf("empty filtered product page hid active intake capacity: %+v %v", filtered, err)
		}
	}
	if _, err = s.FeatureStatus(ctx, step.FeatureID, cfg); !errors.Is(err, ErrNotFound) {
		t.Fatalf("internal feature leaked through product detail: %v", err)
	}
	if _, err = s.Claim(ctx, ordinary.ID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("hidden work released real scheduler capacity: %v", err)
	}
	for _, forged := range []model.Feature{
		func() model.Feature { f := feature(model.IntakeFeaturePrefix + "forged"); return f }(),
		func() model.Feature { f := feature("forged-association"); f.IntakeStepID = step.ID; return f }(),
		func() model.Feature { f := feature("forged-kind"); f.Kind = model.IntakeFeatureKind; return f }(),
		func() model.Feature {
			f := feature("forged-dependency")
			f.DependsOn = []string{step.FeatureID}
			return f
		}(),
	} {
		if _, err = s.Import(ctx, forged); err == nil {
			t.Fatalf("accepted external controller association/dependency: %+v", forged)
		}
	}
	commandTestCount(t, s, `SELECT count(*) FROM features`, 3)
	const privateDetail = "private-intake-transcript-sentinel"
	if err = s.FailIntakeStep(ctx, a, step, privateDetail, false); err != nil {
		t.Fatal(err)
	}
	settled, err := s.Status(ctx, model.StatusQuery{}, cfg)
	if err != nil || settled.IntakeStepCount != 0 || settled.Limits.Global.Active != 0 || settled.Calls.Used != 1 {
		t.Fatalf("settled intake activity retained capacity or lost its daily charge: %+v %v", settled, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM events WHERE feature_id=? AND detail=?`, 1, step.FeatureID, privateDetail)
	if err = s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingNotifications(ctx, "external-webhook", 100)
	if err != nil {
		t.Fatal(err)
	}
	var productEvent, globalEvent bool
	for _, notification := range pending {
		if notification.FeatureID == step.FeatureID || strings.Contains(notification.Detail, privateDetail) {
			t.Fatalf("internal intake detail escaped through generic notifications: %+v", notification)
		}
		productEvent = productEvent || notification.FeatureID == ordinary.ID
		globalEvent = globalEvent || notification.FeatureID == "" && notification.Kind == "paused"
	}
	if !productEvent || !globalEvent {
		t.Fatalf("privacy filtering suppressed ordinary product or global events: %+v", pending)
	}
}

func TestCommandRevocationBlocksPendingGrantsButNotCommittedFeatureWork(t *testing.T) {
	for _, revoke := range []string{"operator", "credential"} {
		t.Run(revoke, func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			o, c, b := authTestActor(t, s, time.Now().UTC(), "revoke-grants")
			cfg := commandTestConfig()
			committedFeature, err := s.Import(ctx, feature("committed-before-revoke"))
			if err != nil {
				t.Fatal(err)
			}
			committed := commandTestCommand(t, "00000000-0000-4000-8000-000000000050", model.ActionFeatureApprove,
				model.SubjectCommandPayload{Kind: "feature", ID: committedFeature.ID, Revision: committedFeature.Revision})
			_, proof := commandTestAuthorization(t, s, b, committed, cfg)
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, committed, cfg, &proof); err != nil {
				t.Fatal(err)
			}
			pendingFeature, err := s.Import(ctx, feature("pending-at-revoke"))
			if err != nil {
				t.Fatal(err)
			}
			pending := commandTestCommand(t, "00000000-0000-4000-8000-000000000051", model.ActionFeatureApprove,
				model.SubjectCommandPayload{Kind: "feature", ID: pendingFeature.ID, Revision: pendingFeature.Revision})
			_, pendingProof := commandTestAuthorization(t, s, b, pending, cfg)
			if revoke == "operator" {
				err = s.RevokeOperator(ctx, o.ID)
			} else {
				err = s.RevokeCredential(ctx, c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, pending, cfg, &pendingProof); !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("revoked actor committed a previously uncommitted challenge: %v", err)
			}
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, committed, cfg, nil); !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("revoked actor accessed a committed receipt: %v", err)
			}
			retained, err := s.Get(ctx, pendingFeature.ID)
			if err != nil || retained.State != "proposed" || retained.PolicyHash != "" {
				t.Fatalf("uncommitted challenge granted domain authority after revocation: %+v %v", retained, err)
			}
			retainedCredential, err := s.GetCredential(ctx, c.ID)
			if err != nil || retainedCredential.SignCount != proof.Credential.SignCount {
				t.Fatalf("uncommitted challenge changed credential counter: %+v %v", retainedCredential, err)
			}
			commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 1)
			commandTestCount(t, s, `SELECT count(*) FROM intake_approval_receipts`, 1)
			// Revocation withdraws future browser authority, not work that was
			// already atomically granted under this exact revision and policy.
			a, err := s.Claim(ctx, committedFeature.ID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600)
			if err != nil {
				t.Fatalf("revocation retroactively cancelled committed feature authority: %v", err)
			}
			if err = s.ReserveCall(ctx, a, "build", a.ID+":build", cfg.MaxCallsPerDay); err != nil {
				t.Fatalf("committed feature could not reserve its bounded work: %v", err)
			}
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 1)
		})
	}
}

func TestCommittedIntakeDraftSurvivesRevocationWithoutGrantingNewCommands(t *testing.T) {
	for _, revoke := range []string{"operator", "credential"} {
		t.Run(revoke, func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			o, c, b := authTestActor(t, s, time.Now().UTC(), "revoke-draft")
			cfg := commandTestConfig()
			r := commandTestCreateRequest(t, s, o, b, cfg)
			draft := commandTestDraftCommand(t, s, r, cfg)
			first, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, draft, cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, draft, cfg, nil)
			if err != nil || !reflect.DeepEqual(replay, first) {
				t.Fatalf("exact draft receipt replay changed bounded authority: %+v %v", replay, err)
			}
			queued, err := s.IntakeRequest(ctx, r.ID)
			if err != nil || queued.State != model.IntakeDraftQueued || queued.ContentVersion != r.ContentVersion {
				t.Fatalf("draft did not authorize the exact current saved content: %+v %v", queued, err)
			}
			commandTestCount(t, s, `SELECT count(*) FROM intake_turns WHERE request_id=?`, 1, r.ID)
			commandTestCount(t, s, `SELECT count(*) FROM intake_events WHERE request_id=? AND json_extract(data,'$.kind')='draft_requested'`, 1, r.ID)
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 0)
			if revoke == "operator" {
				err = s.RevokeOperator(ctx, o.ID)
			} else {
				err = s.RevokeCredential(ctx, c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, draft, cfg, nil); !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("revoked actor replayed/accessed a receipt: %v", err)
			}
			step, err := s.CreateIntakeStep(ctx, queued.ID, queued.ContentVersion, cfg)
			if err != nil {
				t.Fatalf("revocation retroactively withdrew committed draft consent: %v", err)
			}
			a, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ReserveCall(ctx, a, "intake", a.ID+":intake", cfg.MaxCallsPerDay); err != nil {
				t.Fatalf("revoked actor stranded already-authorized bounded drafting: %v", err)
			}
			retained, err := s.IntakeRequest(ctx, r.ID)
			if err != nil || retained.LifetimeDispatches != 1 {
				t.Fatalf("committed draft lost lifetime charge: %+v %v", retained, err)
			}
			commandTestCount(t, s, `SELECT count(*) FROM intake_commands`, 2)
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 1)
		})
	}
}

func commandTestApprovalForSource(t *testing.T, s *Store, o model.Operator, b model.BrowserSession, cfg model.Config, sourceFeature model.Feature) (model.IntakeRequest, model.TypedCommand) {
	t.Helper()
	ctx := context.Background()
	r := commandTestCreateRequest(t, s, o, b, cfg)
	source, err := intake.Export(sourceFeature)
	if err != nil {
		t.Fatal(err)
	}
	replace := commandTestCommand(t, "00000000-0000-4000-8000-000000000070", model.ActionDraftReplace,
		model.DraftReplacePayload{Route: model.IntakeRouteFeature, Format: model.IntakeFormatFeature, CanonicalSource: string(source)})
	replace.RequestID, replace.ExpectedContentVersion = r.ID, &r.ContentVersion
	receipt, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, replace, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(receipt.Result, &r); err != nil {
		t.Fatal(err)
	}
	draft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	approve := commandTestCommand(t, "00000000-0000-4000-8000-000000000071", model.ActionIntakeApproveQueue,
		model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
	approve.RequestID, approve.ExpectedContentVersion = r.ID, &r.ContentVersion
	return r, approve
}

func TestExecuteCommandLinksIdenticalAcceptedFeatureWithoutResettingHistory(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	o, _, b := authTestActor(t, s, time.Now().UTC(), "accepted-link")
	cfg := commandTestConfig()
	f, err := s.Import(ctx, feature("existing-accepted-product"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, f.ID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, a, "build", a.ID+":build", cfg.MaxCallsPerDay); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, a, "accepted", "integrated", "integrated:existing-product", false); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.Events(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var attemptBefore []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM attempts WHERE id=?`, a.ID).Scan(&attemptBefore); err != nil {
		t.Fatal(err)
	}
	r, command := commandTestApprovalForSource(t, s, o, b, cfg, before)
	_, proof := commandTestAuthorization(t, s, b, command, cfg)
	first, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, &proof)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, nil)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("link replay did not preserve the committed result: %+v %v", replay, err)
	}
	after, err := s.Get(ctx, f.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("linking an identical accepted contract reset/reapproved product history: %+v %v", after, err)
	}
	afterHistory, err := s.Events(ctx, f.ID)
	if err != nil || !reflect.DeepEqual(afterHistory, history) {
		t.Fatalf("linking accepted work emitted duplicate feature transitions: %+v %v", afterHistory, err)
	}
	var attemptAfter []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM attempts WHERE id=?`, a.ID).Scan(&attemptAfter); err != nil || !reflect.DeepEqual(attemptAfter, attemptBefore) {
		t.Fatalf("linking accepted work rewrote its settled attempt: %s %v", attemptAfter, err)
	}
	links, err := s.IntakeLinks(ctx, r.ID)
	if err != nil || len(links) != 1 || links[0].SubjectID != f.ID || links[0].SubjectRevision != f.Revision {
		t.Fatalf("accepted feature was not linked exactly once: %+v %v", links, err)
	}
	request, err := s.IntakeRequest(ctx, r.ID)
	if err != nil || request.State != model.IntakeSubmitted {
		t.Fatalf("accepted link was not atomically submitted: %+v %v", request, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM features`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM attempts WHERE feature_id=?`, 1, f.ID)
	commandTestCount(t, s, `SELECT count(*) FROM revisions WHERE feature_id=?`, 1, f.ID)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM intake_approval_receipts`, 1)
}

func TestExecuteCommandRejectsGlobalIDCollisionAfterExactReviewWithoutPartialGrant(t *testing.T) {
	for _, collisionProject := range []string{"demo", "other"} {
		t.Run(collisionProject, func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			o, beforeCredential, b := authTestActor(t, s, time.Now().UTC(), "late-collision")
			cfg := commandTestConfig()
			r, command := commandTestApprovalForSource(t, s, o, b, cfg, feature("global-product-id"))
			_, proof := commandTestAuthorization(t, s, b, command, cfg)
			// A global ID can be occupied by a trusted CLI import after the phone
			// reviewed an absent subject. Commit must reconstruct under its lock.
			collision := feature("global-product-id")
			collision.Project, collision.Body = collisionProject, "A different operator's immutable contract."
			collision, err := s.Import(ctx, collision)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Approve(ctx, collision.ID, collision.Revision, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
			priorAttempt, err := s.Claim(ctx, collision.ID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Save(ctx, priorAttempt, "accepted", "integrated", "integrated:prior-global-product", false); err != nil {
				t.Fatal(err)
			}
			collision, err = s.Get(ctx, collision.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ExecuteCommand(ctx, b.TokenHash, b.CSRFHash, command, cfg, &proof); err == nil {
				t.Fatal("late global feature ID collision was overwritten or silently linked")
			}
			retained, err := s.Get(ctx, collision.ID)
			if err != nil || !reflect.DeepEqual(retained, collision) {
				t.Fatalf("failed phone submission changed the existing global contract: %+v %v", retained, err)
			}
			request, err := s.IntakeRequest(ctx, r.ID)
			if err != nil || request.State != model.IntakeReviewReady || request.ContentVersion != r.ContentVersion {
				t.Fatalf("collision lost the saved review-ready intake: %+v %v", request, err)
			}
			challenge, err := s.GetAuthChallenge(ctx, proof.ChallengeID)
			if err != nil || challenge.ConsumedAt != nil {
				t.Fatalf("failed collision consumed fresh authority: %+v %v", challenge, err)
			}
			credential, err := s.GetCredential(ctx, b.CredentialID)
			if err != nil || !reflect.DeepEqual(credential, beforeCredential) {
				t.Fatalf("failed collision advanced the credential: %+v %v", credential, err)
			}
			commandTestCount(t, s, `SELECT count(*) FROM intake_links`, 0)
			commandTestCount(t, s, `SELECT count(*) FROM intake_approval_receipts`, 0)
			commandTestCount(t, s, `SELECT count(*) FROM intake_commands WHERE command_id=?`, 0, command.CommandID)
			commandTestCount(t, s, `SELECT count(*) FROM revisions WHERE feature_id=?`, 1, collision.ID)
			commandTestCount(t, s, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=0`, 1, collision.ID)
		})
	}
}
