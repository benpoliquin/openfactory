package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// These fixtures exercise the domain transaction helpers, not WebAuthn. The
// command/auth packages separately verify how authenticated audit is obtained.
func lifecycleConfig() model.Config {
	return model.Config{Intake: model.IntakePolicy{Enabled: true, MaxDraftCalls: 12}, PolicyHash: strings.Repeat("a", 64), AstraModel: "draft-model", MaxAttempts: 3, Projects: map[string]model.Project{"demo": {Trusted: true, Checks: map[string]model.Command{"unit": {}}}}}
}
func lifecycleOperator(t *testing.T, s *Store) {
	t.Helper()
	if err := s.CreateOperator(context.Background(), model.Operator{ID: "operator", Label: "Operator", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("operator")}); err != nil {
		t.Fatal(err)
	}
}
func lifecycleUUID() string {
	id := ID()
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}
func lifecycleCommand(action string, r model.IntakeRequest, payload any) model.TypedCommand {
	raw, _ := json.Marshal(payload)
	c := model.TypedCommand{CommandID: lifecycleUUID(), Action: action, Payload: raw}
	if action != model.ActionRequestCreate {
		v := r.ContentVersion
		c.RequestID = r.ID
		c.ExpectedContentVersion = &v
	}
	return c
}
func lifecycleApply(s *Store, cfg model.Config, c model.TypedCommand) (model.IntakeRequest, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.IntakeRequest{}, err
	}
	defer tx.Rollback()
	result, err := s.applyIntakeCommandTx(ctx, tx, c, cfg, model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: "operator", CredentialID: "credential", CommandID: c.CommandID})
	if err != nil {
		return model.IntakeRequest{}, err
	}
	r := result.(model.IntakeRequest)
	return r, tx.Commit()
}
func lifecycleSave(t *testing.T, s *Store, cfg model.Config, text string) model.IntakeRequest {
	t.Helper()
	r, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionRequestCreate, model.IntakeRequest{}, model.RequestCreatePayload{Project: "demo", Text: text, ClientMessageID: lifecycleUUID()}))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func lifecycleDraft(t *testing.T, s *Store, cfg model.Config, r model.IntakeRequest) model.IntakeRequest {
	t.Helper()
	d, err := s.IntakeDisclosure(context.Background(), cfg, r.OperatorID, r.Project, r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionIntakeDraft, r, model.IntakeDraftPayload{Disclosure: d}))
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func lifecycleStart(t *testing.T, s *Store, cfg model.Config, r model.IntakeRequest) (model.IntakeStep, model.Attempt) {
	t.Helper()
	ctx := context.Background()
	step, err := s.CreateIntakeStep(ctx, r.ID, r.ContentVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, 3, 10, 10, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, a, "intake", "intake-dispatch", 100); err != nil {
		t.Fatal(err)
	}
	a.ThreadID = "thread-" + a.ID
	a.Stage = "intake_running"
	a.State = "running"
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	if err = s.BindIntakeAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	step, err = s.GetIntakeStep(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	return step, a
}
func lifecycleResult(t *testing.T, s *Store, cfg model.Config, step model.IntakeStep, a model.Attempt, raw string) {
	t.Helper()
	a.Summary = raw
	out, err := intake.ParseModelOutput([]byte(raw), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyIntakeResult(context.Background(), a, step, out, cfg); err != nil {
		t.Fatal(err)
	}
}
func lifecycleFeatureOutput(t *testing.T, id string) string {
	t.Helper()
	source, err := intake.Export(feature(id))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"type": "draft", "message": "Exact canonical draft", "route": "feature", "format": "feature_markdown", "canonical_source": string(source), "assumptions": []string{}, "unresolved_risks": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func lifecycleErrorCode(err error) string {
	var domain *model.ProgramError
	if errors.As(err, &domain) {
		return domain.Code
	}
	return ""
}

func TestIntakeSaveHasNoModelAuthorityAndConcurrentContentCAS(t *testing.T) {
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleSave(t, s, cfg, "Exact dictated text")
	if r.State != model.IntakeCollecting || r.ContextManifest.SHA != "" {
		t.Fatalf("save granted model authority: %+v", r)
	}
	if _, err := s.CreateIntakeStep(context.Background(), r.ID, r.ContentVersion, cfg); lifecycleErrorCode(err) != "stale_revision" {
		t.Fatalf("save created work: %v", err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = intakeEventTx(context.Background(), tx, &r, "progress", "no content mutation"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	commands := []model.TypedCommand{lifecycleCommand(model.ActionTurnAppend, r, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "First racing edit"}), lifecycleCommand(model.ActionTurnAppend, r, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Second racing edit"})}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, command := range commands {
		workers.Add(1)
		go func(c model.TypedCommand) {
			defer workers.Done()
			<-start
			_, err := lifecycleApply(s, cfg, c)
			results <- err
		}(command)
	}
	close(start)
	workers.Wait()
	close(results)
	successes, stale := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if lifecycleErrorCode(err) == "stale_revision" {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("CAS results success=%d stale=%d", successes, stale)
	}
	turns, err := s.IntakeTurns(context.Background(), r.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("lost or duplicated racing turn: %+v", turns)
	}
	current, err := s.IntakeRequest(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ContentVersion != 2 || current.LifetimeDispatches != 0 {
		t.Fatalf("content/progress versions conflated: %+v", current)
	}
}

func TestIntakeLifetimeCapPersistsAcrossRestartAndPolicyIncrease(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	cfg.Intake.MaxDraftCalls = 1
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Bounded request"))
	step, a := lifecycleStart(t, s, cfg, r)
	if err = s.ReserveCall(ctx, a, "intake", "intake-dispatch", 100); err != nil {
		t.Fatalf("exact reservation replay failed at lifetime cap: %v", err)
	}
	if err = s.ReserveCall(ctx, a, "intake", "second-dispatch", 100); !errors.Is(err, ErrWaiting) {
		t.Fatalf("second dispatch escaped lifetime cap: %v", err)
	}
	lifecycleResult(t, s, cfg, step, a, `{"type":"blocked","message":"Need information","missing_prerequisites":["Project requirement"]}`)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxDraftDispatches != 1 || r.LifetimeDispatches != 1 {
		t.Fatalf("lifetime allowance lost on restart: %+v", r)
	}
	cfg.Intake.MaxDraftCalls = 99
	_, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionIntakeRetry, r, model.EmptyCommandPayload{}))
	if lifecycleErrorCode(err) != "input_limit" {
		t.Fatalf("retry reset frozen request cap: %v", err)
	}
	saved, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, r, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Saved even when budget is exhausted"}))
	if err != nil {
		t.Fatal(err)
	}
	disclosure, err := s.IntakeDisclosure(ctx, cfg, saved.OperatorID, saved.Project, saved.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionIntakeDraft, saved, model.IntakeDraftPayload{Disclosure: disclosure}))
	if lifecycleErrorCode(err) != "input_limit" {
		t.Fatalf("new draft reset frozen cap: %v", err)
	}
}

func TestIntakeSupersededResultRetainsHistoryAndFrozenInputs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Original private goal"))
	step, a := lifecycleStart(t, s, cfg, r)
	r, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, r, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "New undisclosed revision"}))
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != model.IntakeCollecting || saved.ActiveStepID != step.ID {
		t.Fatalf("save released old ownership: %+v", saved)
	}
	frozen, turns, _, err := s.IntakeStepInputs(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.ContentVersion != step.ContentVersion || len(turns) != 1 || turns[0].Text != "Original private goal" {
		t.Fatalf("old provider received changed context: %+v %+v", frozen, turns)
	}
	queued := lifecycleDraft(t, s, cfg, saved)
	if _, err = s.CreateIntakeStep(ctx, r.ID, queued.ContentVersion, cfg); lifecycleErrorCode(err) != "attempt_unsettled" {
		t.Fatalf("second active step: %v", err)
	}
	raw := lifecycleFeatureOutput(t, "superseded-product")
	lifecycleResult(t, s, cfg, step, a, raw)
	current, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.IntakeDraftQueued || current.LatestDraftRevision != 0 || current.ActiveStepID != "" {
		t.Fatalf("stale result became current: %+v", current)
	}
	drafts, err := s.IntakeDrafts(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].ContentVersion != step.ContentVersion || drafts[0].OutputSHA != intake.SHA256([]byte(raw)) {
		t.Fatalf("stale draft history lost: %+v", drafts)
	}
	old, err := s.GetIntakeStep(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !old.Superseded || old.State != "superseded" || old.OutputSHA != intake.SHA256([]byte(raw)) {
		t.Fatalf("old result identity lost: %+v", old)
	}
	if _, err = s.CreateIntakeStep(ctx, r.ID, current.ContentVersion, cfg); err != nil {
		t.Fatalf("settled old step still owns capacity: %v", err)
	}
}

func TestIntakeQuestionsRequireAllBoundAnswersThenExplicitDraft(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Clarify consequences"))
	step, a := lifecycleStart(t, s, cfg, r)
	raw := `{"type":"questions","message":"Consequential decisions","questions":[{"local_id":"a","text":"Who owns data?","consequence":"Access boundary","answer_type":"text"},{"local_id":"b","text":"What is retained?","consequence":"Retention boundary","answer_type":"text"},{"local_id":"c","text":"Who may execute?","consequence":"Execution authority","answer_type":"text"}]}`
	lifecycleResult(t, s, cfg, step, a, raw)
	questions, err := s.IntakeQuestions(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 3 {
		t.Fatalf("questions lost: %+v", questions)
	}
	for i, q := range questions {
		r, err = s.IntakeRequest(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		ref := &model.IntakeQuestionRef{QuestionID: q.ID, ContentVersion: q.ContentVersion, InputManifestSHA: q.InputManifestSHA, OutputSHA: q.OutputSHA}
		r, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, r, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: fmt.Sprintf("Exact answer %d", i), Question: ref}))
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && r.State != model.IntakeNeedsAnswers {
			t.Fatalf("partial answers dispatched: %+v", r)
		}
		if _, err = s.CreateIntakeStep(ctx, r.ID, r.ContentVersion, cfg); lifecycleErrorCode(err) != "stale_revision" {
			t.Fatalf("saving answer created model work: %v", err)
		}
	}
	if r.State != model.IntakeCollecting {
		t.Fatalf("final saved answer implicitly drafted: %+v", r)
	}
	queued := lifecycleDraft(t, s, cfg, r)
	if queued.ContentVersion != r.ContentVersion {
		t.Fatal("draft grant changed content version")
	}
	next, err := s.CreateIntakeStep(ctx, r.ID, queued.ContentVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	foundOutput, boundAnswers := false, 0
	for _, ref := range next.Manifest.Inputs {
		bytes, err := s.IntakeContextBlob(ctx, ref.SHA, ref.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if ref.Kind == "output" && string(bytes) == raw {
			foundOutput = true
		}
		if ref.Kind == "turn" {
			var turn intakeContextTurn
			if err = json.Unmarshal(bytes, &turn); err != nil {
				t.Fatal(err)
			}
			if turn.Question != nil {
				boundAnswers++
			}
		}
	}
	if !foundOutput || boundAnswers != 3 {
		t.Fatalf("immutable question context lost output=%v answers=%d", foundOutput, boundAnswers)
	}
}

func TestIntakeCommittedGrantSurvivesRevocationButNewSavesFail(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Already authorized bounded drafting"))
	if err := s.RevokeOperator(ctx, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	step, a := lifecycleStart(t, s, cfg, r)
	lifecycleResult(t, s, cfg, step, a, lifecycleFeatureOutput(t, "authorized-before-revoke"))
	current, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.IntakeReviewReady {
		t.Fatalf("revocation erased historical authority: %+v", current)
	}
	_, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, current, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Forbidden future command"}))
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("revoked operator saved new work: %v", err)
	}
}

func TestIntakeModelLimitsBlockWithoutTruncatingRawOutput(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	cfg.Intake.MaxTurnBytes = 32
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Bounded user text"))
	step, a := lifecycleStart(t, s, cfg, r)
	rawBytes, err := json.Marshal(map[string]any{"type": "blocked", "message": strings.Repeat("x", 33), "missing_prerequisites": []string{"Requirement"}})
	if err != nil {
		t.Fatal(err)
	}
	lifecycleResult(t, s, cfg, step, a, string(rawBytes))
	current, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.IntakeBlocked || current.Wait == nil || current.Wait.Code != "input_limit" || current.ActiveStepID != "" {
		t.Fatalf("oversized model text not settled per request: %+v", current)
	}
	turns, err := s.IntakeTurns(ctx, r.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatal("oversized model message was appended or truncated")
	}
	old, err := s.GetIntakeStep(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := s.IntakeContextBlob(ctx, old.OutputSHA, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(rawBytes) {
		t.Fatal("exact over-limit output history lost")
	}
}

func TestIntakeCancelUndispatchedStepAndRejectForgedQueueState(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleSave(t, s, cfg, "Saved without consent")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.State = model.IntakeDraftQueued
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateIntakeStep(ctx, r.ID, r.ContentVersion, cfg); err == nil {
		t.Fatal("mutable queue state substituted for explicit drafting authority")
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.State = model.IntakeCollecting
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r = lifecycleDraft(t, s, cfg, r)
	step, err := s.CreateIntakeStep(ctx, r.ID, r.ContentVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionIntakeCancel, r, model.EmptyCommandPayload{}))
	if err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeCancelling {
		t.Fatal("cancellation claimed settlement before reconciliation")
	}
	if err = s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	r, err = s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeCancelled || r.ActiveStepID != "" {
		t.Fatalf("undispatched cancellation stuck: %+v", r)
	}
	f, err := s.Get(ctx, step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != "cancelled" {
		t.Fatalf("cancelled internal feature stayed runnable: %+v", f)
	}
}

func TestIntakeInvalidCanonicalOutputPersistsFindingsAndExactBytes(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Draft contract"))
	step, a := lifecycleStart(t, s, cfg, r)
	raw := `{"type":"draft","message":"Contract source","route":"feature","format":"feature_markdown","canonical_source":"not a feature contract","assumptions":[],"unresolved_risks":[]}`
	lifecycleResult(t, s, cfg, step, a, raw)
	current, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s.IntakeDraft(ctx, r.ID, current.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.IntakeBlocked || len(draft.Findings) == 0 || draft.CanonicalSource != "not a feature contract" || draft.SourceSHA != intake.SHA256([]byte(draft.CanonicalSource)) {
		t.Fatalf("invalid source was repaired, hidden or left active: %+v %+v", current, draft)
	}
}

// This fixture persists a real exact-command review and installs previously
// verified approval state for import/link atomicity tests, not WebAuthn proof.
func lifecycleApprovalFixture(t *testing.T, s *Store, cfg model.Config, c model.TypedCommand) model.AuditContext {
	t.Helper()
	ctx := context.Background()
	audit := model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: "operator", CredentialID: "credential", CommandID: c.CommandID, ApprovalID: "approval-" + c.CommandID}
	credential := model.OperatorCredential{ID: "credential", OperatorID: "operator", ExpiresAt: time.Now().Add(time.Hour)}
	raw, _ := json.Marshal(credential)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, credential.ID, credential.OperatorID, raw); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	operator, err := intakeAuthLiveOperator(ctx, tx, audit.OperatorID, "")
	if err != nil {
		t.Fatal(err)
	}
	session := model.BrowserSession{OperatorID: audit.OperatorID, CredentialID: credential.ID}
	review, err := s.reviewIntakeCommandTx(ctx, tx, c, cfg, session, operator, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = intakeSaveReviewTx(ctx, tx, review); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(model.ApprovalReceipt{ID: audit.ApprovalID, ReviewID: review.ID, Claims: review.Claims, ClaimsSHA: review.ClaimsSHA, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_approval_receipts(id,review_id,challenge_id,data) VALUES(?,?,?,?)`, audit.ApprovalID, review.ID, review.Claims.ChallengeID, raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return audit
}
func TestIntakeSubmitImmutableLineageAndAmendment(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleSave(t, s, cfg, "Human-authored exact contract")
	source, err := intake.Export(feature("submitted-product"))
	if err != nil {
		t.Fatal(err)
	}
	r, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionDraftReplace, r, model.DraftReplacePayload{Route: "feature", Format: "feature_markdown", CanonicalSource: string(source)}))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	command := lifecycleCommand(model.ActionIntakeApproveQueue, r, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
	audit := lifecycleApprovalFixture(t, s, cfg, command)
	approval, err := intakeLoad[model.ApprovalReceipt](ctx, s.db, `SELECT data FROM intake_approval_receipts WHERE id=?`, audit.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Claims.RequestID != r.ID || approval.Claims.ContentVersion != r.ContentVersion || approval.Claims.DraftRevision != draft.Revision || approval.Claims.SubjectKind != draft.Route || approval.Claims.SubjectID != "submitted-product" || approval.Claims.SubjectRevision != draft.DomainRevision || approval.ClaimsSHA == "" {
		t.Fatalf("handoff approval lost exact request and subject binding: %+v", approval)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.applyIntakeCommandTx(ctx, tx, command, cfg, audit)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r = out.(model.IntakeRequest)
	linked, err := s.Get(ctx, "submitted-product")
	if err != nil {
		t.Fatal(err)
	}
	links, err := s.IntakeLinks(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeSubmitted || linked.State != "ready" || len(links) != 1 || links[0].SubjectRevision != linked.Revision || linked.Revision != draft.DomainRevision || draft.CanonicalSource != string(source) || draft.SourceSHA != intake.SHA256(source) {
		t.Fatalf("import, grant and lineage not atomic: %+v %+v %+v", r, linked, links)
	}
	_, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, r, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Must not rewrite submitted work"}))
	if lifecycleErrorCode(err) != "stale_subject" {
		t.Fatalf("submitted request was edited: %v", err)
	}
	amended, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionRequestCreate, model.IntakeRequest{}, model.RequestCreatePayload{Project: "demo", Text: "New amendment", ClientMessageID: lifecycleUUID(), ParentRequestID: r.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if amended.ID == r.ID || amended.ParentRequestID != r.ID || amended.State != model.IntakeCollecting {
		t.Fatalf("amendment lineage lost: %+v", amended)
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_links WHERE request_id=?`, r.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("amendment rewrote accepted linkage")
	}
	retained, err := s.IntakeDraft(ctx, r.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if programHash(retained) != programHash(draft) {
		t.Fatal("amendment changed the immutable submitted source")
	}
}

func TestIntakeMessageDedupAndExactSavedContextConsent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleSave(t, s, cfg, "Original exact text")
	messageID := lifecycleUUID()
	payload := model.TurnAppendPayload{ClientMessageID: messageID, Text: "Saved second message"}
	next, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, r, payload))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, next, payload))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ContentVersion != next.ContentVersion {
		t.Fatal("message dedup changed content")
	}
	payload.Text = "Different text with reused message ID"
	if _, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, next, payload)); lifecycleErrorCode(err) != "idempotency_conflict" {
		t.Fatalf("message conflict accepted: %v", err)
	}
	turns, err := s.IntakeTurns(ctx, r.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("message dedup lost append-only transcript: %+v", turns)
	}
	disclosure, err := s.IntakeDisclosure(ctx, cfg, next.OperatorID, next.Project, next.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	tampered := disclosure
	tampered.ModelProviderIDs = []string{"other-provider"}
	if _, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionIntakeDraft, next, model.IntakeDraftPayload{Disclosure: tampered})); lifecycleErrorCode(err) != "disclosure_required" {
		t.Fatalf("changed provider silently accepted: %v", err)
	}
	edited, err := lifecycleApply(s, cfg, lifecycleCommand(model.ActionTurnAppend, next, model.TurnAppendPayload{ClientMessageID: lifecycleUUID(), Text: "Saved after disclosure was offered"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionIntakeDraft, edited, model.IntakeDraftPayload{Disclosure: disclosure})); lifecycleErrorCode(err) != "disclosure_required" {
		t.Fatalf("old context consent silently reused: %v", err)
	}
	queued := lifecycleDraft(t, s, cfg, edited)
	if queued.ContentVersion != edited.ContentVersion {
		t.Fatal("drafting changed saved content version")
	}
	step, err := s.CreateIntakeStep(ctx, r.ID, queued.ContentVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, frozen, _, err := s.IntakeStepInputs(ctx, step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 3 || frozen[2].Text != "Saved after disclosure was offered" {
		t.Fatalf("exact consent context silently trimmed: %+v", frozen)
	}
}

func TestIntakeResultRejectsDifferentProducerAndRetainsActiveOwnership(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	r := lifecycleDraft(t, s, cfg, lifecycleSave(t, s, cfg, "Bind exact producer"))
	step, a := lifecycleStart(t, s, cfg, r)
	a.Summary = lifecycleFeatureOutput(t, "producer-bound-product")
	parsed, err := intake.ParseModelOutput([]byte(a.Summary), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	forged := step
	forged.InputManifestSHA = strings.Repeat("b", 64)
	if err = s.ApplyIntakeResult(ctx, a, forged, parsed, cfg); lifecycleErrorCode(err) != "stale_subject" {
		t.Fatalf("wrong context association accepted: %v", err)
	}
	forgedAttempt := a
	forgedAttempt.ThreadID = "different-thread"
	if err = s.ApplyIntakeResult(ctx, forgedAttempt, step, parsed, cfg); lifecycleErrorCode(err) != "stale_subject" {
		t.Fatalf("wrong output thread accepted: %v", err)
	}
	active, err := s.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != a.ID {
		t.Fatal("invalid output released external ownership")
	}
	if err = s.ApplyIntakeResult(ctx, a, step, parsed, cfg); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyIntakeResult(ctx, a, step, parsed, cfg); err != nil {
		t.Fatalf("identical settled result replay was not idempotent: %v", err)
	}
	drafts, err := s.IntakeDrafts(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 {
		t.Fatal("result replay duplicated immutable draft history")
	}
}

func TestIntakeManualProgramBundleImportDoesNotGrantModelDisclosure(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	lifecycleOperator(t, s)
	cfg := lifecycleConfig()
	cfg.Discovery.BundleMaxFiles = 8
	cfg.Discovery.BundleMaxFileBytes = 4096
	cfg.Discovery.BundleMaxTotalBytes = 8192
	bundle := intakeBundlesFinalized(t, s, cfg, "operator", []byte("Private manually selected source\n"))
	r := lifecycleSave(t, s, cfg, "Save a human-authored Program contract")
	spec := model.ProgramSpec{ID: "manual-bundle-program", Project: "demo", Title: "Manual seed", Objective: "Import the exact selected seed without drafting", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"draft-model"}}, Seeds: []model.ProgramSeed{{ID: "private", Kind: "bundle", Path: "inputs/private.md"}}}
	source, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err = lifecycleApply(s, cfg, lifecycleCommand(model.ActionDraftReplace, r, model.DraftReplacePayload{Route: "program", Format: "program_json", CanonicalSource: string(source), BundleManifestSHA: bundle.ManifestSHA}))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeReviewReady || draft.BundleManifestSHA != bundle.ManifestSHA || draft.PolicySHA != cfg.PolicyHash || draft.DomainRevision == "" {
		t.Fatalf("manual seed review lost exact identity: %+v %+v", r, draft)
	}
	if r.ContextManifest.Disclosure.PrivateMaterialToModels || len(r.ContextManifest.Disclosure.ModelProviderIDs) != 0 || r.LifetimeDispatches != 0 {
		t.Fatal("manual seed import manufactured model consent")
	}
	command := lifecycleCommand(model.ActionIntakeApproveQueue, r, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
	now := time.Now().UTC()
	credential := model.OperatorCredential{ID: "credential", OperatorID: r.OperatorID, ExpiresAt: now.Add(time.Hour)}
	credentialData, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?)`, credential.ID, credential.OperatorID, credentialData); err != nil {
		t.Fatal(err)
	}
	session := model.BrowserSession{ID: "manual-session", TokenHash: strings.Repeat("b", 64), CSRFHash: strings.Repeat("c", 64), OperatorID: r.OperatorID, CredentialID: credential.ID, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(time.Hour)}
	if err = s.CreateBrowserSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg); lifecycleErrorCode(err) != "disclosure_required" {
		t.Fatalf("approval review offered a proof ceremony without private seed consent: %v", err)
	}
	audit := model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: r.OperatorID, CredentialID: credential.ID, CommandID: command.CommandID}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.applyIntakeCommandTx(ctx, tx, command, cfg, audit)
	tx.Rollback()
	if lifecycleErrorCode(err) != "disclosure_required" {
		t.Fatalf("approval treated manual seed selection as model consent: %v", err)
	}
	if _, err = programLoad(ctx, s.db, spec.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected approval left a partial Program handoff: %v", err)
	}
	current, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	links, err := s.IntakeLinks(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.IntakeReviewReady || current.ContentVersion != r.ContentVersion || len(links) != 0 {
		t.Fatalf("rejected approval changed saved request or lineage: %+v %+v", current, links)
	}
	canonical, err := intake.ParseCanonical(draft.Route, draft.Format, []byte(draft.CanonicalSource), r.Project, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportProgram(ctx, *canonical.Program, bundle); err != nil {
		t.Fatalf("manual source could not be imported as an unapproved proposal: %v", err)
	}
	program, err := programLoad(ctx, s.db, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := programRevision(ctx, s.db, spec.ID, program.CurrentRevision)
	if err != nil {
		t.Fatal(err)
	}
	if program.State != model.ProgramDraft || revision.Spec.Disclosure.PrivateMaterialToModels || revision.SeedManifestSHA != bundle.ManifestSHA || revision.RevisionHash != draft.DomainRevision || draft.CanonicalSource != string(source) || draft.SourceSHA != intake.SHA256(source) {
		t.Fatalf("proposal import changed reviewed source, seed, revision or consent: %+v", revision)
	}
	if err = s.ApproveProgram(ctx, program.ID, revision.RevisionHash, cfg.PolicyHash, model.AuditContext{Kind: model.AuditTrustedLocal}); lifecycleErrorCode(err) != "disclosure_required" {
		t.Fatalf("execution approval accepted undisclosed private seeds: %v", err)
	}
	var grants, calls, programGrants int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_draft_authorizations WHERE request_id=?`, r.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM calls`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=?`, program.ID).Scan(&programGrants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 || calls != 0 || programGrants != 0 {
		t.Fatal("manual import or rejected approval created drafting, execution authority or model calls")
	}
}
