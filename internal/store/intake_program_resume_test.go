package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	experimentexec "openfactory/internal/executor"
	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// These consumer tests start at the already-verified Store authorization seam.
// commandTestAuthorization supplies inert persistence evidence, not a WebAuthn
// ceremony. Typed worker results below exercise Store validation, not a model.
type programResumeConsumer struct {
	s        *Store
	path     string
	cfg      model.Config
	session  model.BrowserSession
	revision model.ProgramRevision
	step     model.ProgramStep
	attempt  model.Attempt
	secrets  []string
}

func newProgramResumeConsumer(t *testing.T, limits model.ProgramLimits) *programResumeConsumer {
	t.Helper()
	name := filepath.Join(t.TempDir(), "resume.sqlite")
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, _, session := authTestActor(t, s, time.Now().UTC(), "program-resume")
	cfg := commandTestConfig()
	cfg.AstraModel = "provider/research"
	c := &programResumeConsumer{s: s, path: name, cfg: cfg, session: session}
	c.revision = c.importApproved(t, model.ProgramSpec{
		ID: "resume-program", Project: "demo", Title: "Recover exact internal work", Objective: "Preserve settled research history",
		Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}, Limits: limits,
	})
	// A real failed predecessor must never be revived by a later revision's
	// resume, even though its kind and Program identity still match.
	historical := c.nextStep(t, model.StepResearch)
	c.fail(t, c.start(t, historical))
	spec := c.revision.Spec
	spec.Objective = "Recover only the current architecture after completed research"
	c.revision = c.importApproved(t, spec)
	research := c.nextStep(t, model.StepResearch)
	a := c.start(t, research)
	if err = s.CompleteProgramStep(context.Background(), a, researchStepResult()); err != nil {
		t.Fatal(err)
	}
	completed, err := s.ProgramStepForFeature(context.Background(), research.FeatureID)
	if err != nil || completed.State != "completed" || completed.OutputManifestSHA == "" {
		t.Fatalf("actual research result was not settled: %+v %v", completed, err)
	}
	c.step = c.nextStep(t, model.StepArchitecture)
	c.attempt = c.fail(t, c.start(t, c.step))
	c.step, err = s.ProgramStepForFeature(context.Background(), c.step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	c.wait(t)
	return c
}

func (c *programResumeConsumer) importApproved(t *testing.T, spec model.ProgramSpec) model.ProgramRevision {
	t.Helper()
	ctx := context.Background()
	r, err := c.s.ImportProgram(ctx, spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.s.ApproveProgram(ctx, r.ProgramID, r.RevisionHash, c.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	return r
}

func (c *programResumeConsumer) nextStep(t *testing.T, kind string) model.ProgramStep {
	t.Helper()
	work, err := c.s.ProgramWork(context.Background(), c.cfg)
	if err != nil || len(work) != 1 || work[0].Wait != nil || work[0].Step == nil || work[0].Step.Kind != kind {
		t.Fatalf("current public work: %+v %v", work, err)
	}
	step, err := c.s.EnsureProgramStep(context.Background(), *work[0].Step, c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func (c *programResumeConsumer) start(t *testing.T, step model.ProgramStep) model.Attempt {
	t.Helper()
	ctx := context.Background()
	a, err := c.s.Claim(ctx, step.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := c.s.IssueResearchCapability(ctx, a, c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.secrets = append(c.secrets, capability.Token)
	if err = c.s.ReserveCall(ctx, a, "research", a.ID+":research", c.cfg.MaxCallsPerDay); err != nil {
		t.Fatal(err)
	}
	a = programResumeStoredAttempt(t, c.s, a.ID)
	a.Stage = "research_poll"
	a.ThreadID = "private-worker-thread-" + a.ID
	a.Workspace = "/private/resume-workspace/" + a.ID
	a.Summary = "private prompt and output for " + a.ID
	c.secrets = append(c.secrets, a.Workspace, a.Summary)
	if err = c.s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	return programResumeStoredAttempt(t, c.s, a.ID)
}

func (c *programResumeConsumer) fail(t *testing.T, a model.Attempt) model.Attempt {
	t.Helper()
	a.Stage, a.State = "failed", "blocked"
	a.Summary = "private terminal output with prompt and token " + c.secrets[len(c.secrets)-3]
	c.secrets = append(c.secrets, a.Summary)
	if err := c.s.Save(context.Background(), a, "blocked", "private failure at "+a.Workspace, "private-worker-receipt", false); err != nil {
		t.Fatal(err)
	}
	return programResumeStoredAttempt(t, c.s, a.ID)
}

func (c *programResumeConsumer) wait(t *testing.T) {
	t.Helper()
	work, err := c.s.ProgramWork(context.Background(), c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitingCurrent := false
	for _, item := range work {
		if item.Program.ID != c.revision.ProgramID || item.Revision.Number != c.revision.Number {
			continue
		}
		// Public ProgramWork deliberately suppresses a blocked Step on its
		// waiting item; healthy parallel experiment siblings may remain runnable.
		if item.Wait != nil {
			waitingCurrent = true
		}
		if c.step.ID != "" && item.Step != nil && item.Step.Kind == c.step.Kind && item.Step.SubjectID == c.step.SubjectID && item.Step.InputManifestSHA == c.step.InputManifestSHA {
			t.Fatalf("failed worker was offered as ordinary runnable work: %+v", item)
		}
	}
	if !waitingCurrent {
		t.Fatalf("current Program lost its failed-work or external-prerequisite wait: %+v", work)
	}
	if c.step.ID == "" {
		return
	}
	// A waiting representation alone is not admission proof: explicitly try
	// the ordinary Claim and require every ownership row and counter unchanged.
	before := programResumeSnapshot(t, c.s)
	attempt, err := c.s.Claim(context.Background(), c.step.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600)
	if !errors.Is(err, ErrWaiting) || attempt.ID != "" {
		t.Fatalf("failed or uncertain worker was admitted without fresh recovery authority: %+v %v", attempt, err)
	}
	if !reflect.DeepEqual(programResumeSnapshot(t, c.s), before) {
		t.Fatal("ordinary waiting Claim changed failed/uncertain ownership, history, or budgets")
	}
}

func (c *programResumeConsumer) command(t *testing.T, number int) model.TypedCommand {
	t.Helper()
	return commandTestCommand(t, fmt.Sprintf("40000000-0000-4000-8000-%012d", number), model.ActionProgramResume,
		model.SubjectCommandPayload{Kind: "program", ID: c.revision.ProgramID, Revision: c.revision.RevisionHash})
}

func programResumeStoredAttempt(t *testing.T, s *Store, id string) model.Attempt {
	t.Helper()
	var raw []byte
	var a model.Attempt
	if err := s.db.QueryRowContext(context.Background(), `SELECT data FROM attempts WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// Snapshots include the SQL ownership bit, not just the attempt JSON. They are
// read-only observations of real persisted effects, never fabricated fixtures.
func programResumeRows(t *testing.T, s *Store, query string, args ...any) [][]string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		line := make([]string, len(values))
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				line[i] = string(raw)
			} else {
				line[i] = fmt.Sprint(value)
			}
		}
		out = append(out, line)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func programResumeSnapshot(t *testing.T, s *Store) map[string][][]string {
	t.Helper()
	out := map[string][][]string{}
	for _, table := range []string{"programs", "program_revisions", "program_steps", "program_manifests", "program_artifacts", "program_budgets", "program_grants", "program_events", "program_invalidations", "program_experiment_intents", "program_experiments", "features", "attempts", "calls", "events", "intake_credentials", "intake_auth_challenges", "intake_approval_receipts", "intake_commands", "intake_audits"} {
		out[table] = programResumeRows(t, s, `SELECT * FROM `+table+` ORDER BY rowid`)
	}
	return out
}

func programResumeUnselected(t *testing.T, c *programResumeConsumer) map[string][][]string {
	t.Helper()
	return map[string][][]string{
		"features":  programResumeRows(t, c.s, `SELECT * FROM features WHERE id<>? ORDER BY rowid`, c.step.FeatureID),
		"steps":     programResumeRows(t, c.s, `SELECT * FROM program_steps WHERE id<>? ORDER BY rowid`, c.step.ID),
		"attempts":  programResumeRows(t, c.s, `SELECT * FROM attempts ORDER BY rowid`),
		"artifacts": programResumeRows(t, c.s, `SELECT * FROM program_artifacts ORDER BY rowid`),
	}
}

func programResumeAssertHistory(t *testing.T, c *programResumeConsumer, before map[string][][]string) {
	t.Helper()
	after := programResumeUnselected(t, c)
	after["attempts"] = programResumeRows(t, c.s, `SELECT * FROM attempts ORDER BY rowid LIMIT ?`, len(before["attempts"]))
	if !reflect.DeepEqual(after, before) {
		t.Fatal("retry dispatch or replay rewrote historical attempts, nonselected states, or artifacts")
	}
}

func programResumeAssertReview(t *testing.T, c *programResumeConsumer, review model.IntakeReview) {
	t.Helper()
	f, err := c.s.Get(context.Background(), c.step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	want := model.ProgramResumeWork{
		StepID: c.step.ID, StepRevision: c.step.Revision, StepKind: c.step.Kind, StepState: c.step.State,
		Iteration: c.step.Iteration, InputManifestSHA: c.step.InputManifestSHA, SubjectID: c.step.SubjectID,
		FeatureID: f.ID, FeatureRevision: f.Revision, FeatureState: f.State, FeatureAttempts: f.Attempts, FeatureMaxAttempts: c.cfg.MaxAttempts,
		AttemptID: c.attempt.ID, AttemptState: c.attempt.State, AttemptStage: c.attempt.Stage, AttemptActive: false,
		AttemptOutputSHA: intake.SHA256([]byte(c.attempt.Summary)), AttemptUpdatedAt: c.attempt.UpdatedAt,
	}
	if review.ResumeWork == nil || !reflect.DeepEqual(*review.ResumeWork, want) {
		t.Fatalf("review did not bind exact settled retry work: got %+v want %+v", review.ResumeWork, want)
	}
	var manifest struct {
		ResumeWork *model.ProgramResumeWork `json:"resume_work"`
	}
	if err = json.Unmarshal([]byte(review.ManifestCanonical), &manifest); err != nil || !reflect.DeepEqual(manifest.ResumeWork, review.ResumeWork) {
		t.Fatalf("outer review and canonical manifest selected different retry work: %+v %v", manifest.ResumeWork, err)
	}
	if intake.SHA256([]byte(review.ManifestCanonical)) != review.Claims.ReviewManifestSHA || intake.SHA256([]byte(review.ClaimsCanonical)) != review.ClaimsSHA || review.Claims.SubjectRevision != c.revision.RevisionHash {
		t.Fatal("safe retry work is not covered by the exact review claims")
	}
	if review.Feature != nil {
		t.Fatal("Program resume exposed the full internal feature instead of narrow retry metadata")
	}
	raw, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(append([]string{}, c.secrets...), f.Body, f.Reason, f.Receipt) {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatal("resume review disclosed private worker material")
		}
	}
	workRaw, err := json.Marshal(review.ResumeWork)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(workRaw, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"step_id": true, "step_revision": true, "step_kind": true, "step_state": true, "iteration": true, "input_manifest_sha": true, "subject_id": true, "feature_id": true, "feature_revision": true, "feature_state": true, "feature_attempts": true, "feature_max_attempts": true, "attempt_id": true, "attempt_state": true, "attempt_stage": true, "attempt_active": true, "attempt_output_sha": true, "attempt_updated_at": true}
	for name := range fields {
		if !allowed[name] {
			t.Fatalf("research retry scope contains non-safe or execution-only field %q", name)
		}
	}
	if string(fields["attempt_active"]) != "false" {
		t.Fatal("settled SQL ownership was omitted from retry scope")
	}
}

func TestIntakeProgramResumeExactSettledWorkAndNewDispatch(t *testing.T) {
	c := newProgramResumeConsumer(t, model.ProgramLimits{})
	ctx := context.Background()
	unchanged := programResumeUnselected(t, c)
	budget := programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`)
	calls := programResumeRows(t, c.s, `SELECT * FROM calls ORDER BY rowid`)
	beforeDaily := map[string]int{}
	for _, row := range programResumeRows(t, c.s, `SELECT day FROM calls ORDER BY rowid`) {
		beforeDaily[row[0]]++
	}
	command := c.command(t, 1)
	beforeReview := programResumeSnapshot(t, c.s)
	review, proof := commandTestAuthorization(t, c.s, c.session, command, c.cfg)
	programResumeAssertReview(t, c, review)
	afterReview := programResumeSnapshot(t, c.s)
	delete(beforeReview, "intake_auth_challenges")
	delete(afterReview, "intake_auth_challenges")
	if !reflect.DeepEqual(afterReview, beforeReview) {
		t.Fatal("review selection mutated Program state, manifests, budgets, or failed work")
	}
	if got := programResumeUnselected(t, c); !reflect.DeepEqual(got, unchanged) {
		t.Fatal("creating resume review changed domain work or settled history")
	}
	for _, flag := range []string{"presence", "verification"} {
		bad := proof
		if flag == "presence" {
			bad.Evidence.UserPresent = false
		} else {
			bad.Evidence.UserVerified = false
		}
		_, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &bad)
		answerErrorCode(t, err, "human_verification_required")
	}
	_, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, nil)
	answerErrorCode(t, err, "human_verification_required")
	receipt, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &proof)
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.s.Get(ctx, c.step.FeatureID)
	if err != nil || f.State != "ready" || f.Attempts != 1 || f.Revision != review.ResumeWork.FeatureRevision || f.PolicyHash != c.cfg.PolicyHash {
		t.Fatalf("resume did not retry the exact approved internal feature: %+v %v", f, err)
	}
	if !reflect.DeepEqual(programResumeUnselected(t, c), unchanged) || !reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`), budget) || !reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM calls ORDER BY rowid`), calls) {
		t.Fatal("resume reset history, another step, or lifetime/daily accounting")
	}
	commandTestCount(t, c.s, `SELECT count(*) FROM events WHERE feature_id=? AND kind='retry_requested'`, 1, c.step.FeatureID)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_audits WHERE subject_kind='feature' AND subject_id=? AND action='retry_requested' AND json_extract(data,'$.audit.approval_id')=?`, 1, c.step.FeatureID, receipt.ApprovalID)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_audits WHERE subject_kind='program' AND subject_id=? AND action='resumed' AND json_extract(data,'$.audit.approval_id')=?`, 1, c.revision.ProgramID, receipt.ApprovalID)
	approval, err := c.s.GetApprovalReceipt(ctx, receipt.ApprovalID)
	if err != nil || approval.ReviewID != review.ID || approval.ClaimsSHA != review.ClaimsSHA || approval.Audit != receipt.Audit || approval.Evidence != proof.Evidence {
		t.Fatalf("resume lost the exact persisted verifier/review linkage: %+v %v", approval, err)
	}
	var otherFeature string
	if err = c.s.db.QueryRowContext(ctx, `SELECT id FROM features WHERE id<>? ORDER BY rowid LIMIT 1`, c.step.FeatureID).Scan(&otherFeature); err != nil {
		t.Fatal(err)
	}
	beforeWrongTarget := programResumeSnapshot(t, c.s)
	if err = c.s.Retry(ctx, otherFeature, receipt.Audit); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Program resume approval did not reject nonselected internal audit authority: %v", err)
	}
	if !reflect.DeepEqual(programResumeSnapshot(t, c.s), beforeWrongTarget) {
		t.Fatal("rejected audit target substitution changed nonselected work")
	}
	step := c.nextStep(t, c.step.Kind)
	if step.ID != c.step.ID || step.FeatureID != c.step.FeatureID {
		t.Fatal("resume created replacement step identity instead of retrying selected work")
	}
	next := c.start(t, step)
	if next.ID == c.attempt.ID {
		t.Fatal("retry reused a settled attempt instead of ordinary admission")
	}
	if _, err = c.s.Claim(ctx, step.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("second worker acquired retried feature ownership: %v", err)
	}
	if err = c.s.ReserveCall(ctx, next, "research", next.ID+":research", c.cfg.MaxCallsPerDay); err != nil {
		t.Fatal(err)
	}
	commandTestCount(t, c.s, `SELECT count(*) FROM calls`, len(calls)+1)
	commandTestCount(t, c.s, `SELECT count(*) FROM calls WHERE attempt_id=?`, 1, next.ID)
	var dispatchDay string
	if err = c.s.db.QueryRowContext(ctx, `SELECT day FROM calls WHERE attempt_id=?`, next.ID).Scan(&dispatchDay); err != nil {
		t.Fatal(err)
	}
	commandTestCount(t, c.s, `SELECT count(*) FROM calls WHERE day=?`, beforeDaily[dispatchDay]+1, dispatchDay)
	commandTestCount(t, c.s, `SELECT json_extract(data,'$.model_calls') FROM program_budgets WHERE program_id=?`, 4, c.revision.ProgramID)
	commandTestCount(t, c.s, `SELECT json_extract(data,'$.steps') FROM program_budgets WHERE program_id=?`, 3, c.revision.ProgramID)
	commandTestCount(t, c.s, `SELECT count(*) FROM attempts WHERE feature_id=?`, 2, step.FeatureID)
	beforeReplay := programResumeSnapshot(t, c.s)
	replayed, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, nil)
	if err != nil || !reflect.DeepEqual(replayed, receipt) || !reflect.DeepEqual(programResumeSnapshot(t, c.s), beforeReplay) {
		t.Fatalf("lost-response replay retried or reset an already-running attempt: %+v %v", replayed, err)
	}
	f, err = c.s.Get(ctx, step.FeatureID)
	if err != nil || f.State != "running" || f.Attempts != 2 {
		t.Fatalf("receipt replay disturbed current worker ownership: %+v %v", f, err)
	}
	if got := programResumeStoredAttempt(t, c.s, c.attempt.ID); !reflect.DeepEqual(got, c.attempt) {
		t.Fatal("retry or receipt replay rewrote the prior settled attempt")
	}
	programResumeAssertHistory(t, c, unchanged)
}

func TestIntakeProgramResumeConcurrentSameIDReturnsOneReceipt(t *testing.T) {
	c := newProgramResumeConsumer(t, model.ProgramLimits{})
	other, err := Open(c.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	command := c.command(t, 2)
	review, proof := commandTestAuthorization(t, c.s, c.session, command, c.cfg)
	programResumeAssertReview(t, c, review)
	unchanged := programResumeUnselected(t, c)
	type result struct {
		receipt model.CommandReceipt
		err     error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(cap(results))
	for i := range cap(results) {
		db := c.s
		if i%2 != 0 {
			db = other
		}
		go func() {
			ready.Done()
			<-start
			r, err := db.ExecuteCommand(context.Background(), c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &proof)
			results <- result{r, err}
		}()
	}
	ready.Wait()
	close(start)
	var first model.CommandReceipt
	for i := range cap(results) {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent same-ID resume: %v", got.err)
		}
		if i == 0 {
			first = got.receipt
		} else if !reflect.DeepEqual(first, got.receipt) {
			t.Fatal("same-ID concurrent resume returned different receipts")
		}
	}
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_commands`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_approval_receipts`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM events WHERE kind='retry_requested'`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_audits WHERE action='resumed'`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_audits WHERE action='retry_requested'`, 1)
	challenge, err := c.s.GetAuthChallenge(context.Background(), proof.ChallengeID)
	if err != nil || challenge.ConsumedAt == nil {
		t.Fatalf("successful exact authorization was not consumed: %+v %v", challenge, err)
	}
	credential, err := c.s.GetCredential(context.Background(), c.session.CredentialID)
	if err != nil || credential.SignCount != proof.Credential.SignCount || !reflect.DeepEqual(credential.CredentialData, proof.Credential.CredentialData) {
		t.Fatalf("same-ID replay updated verifier state more than once: %+v %v", credential, err)
	}
	if !reflect.DeepEqual(programResumeUnselected(t, c), unchanged) {
		t.Fatal("same-ID race mutated nonselected work or historical attempts")
	}
}

func TestIntakeProgramResumeTwoFreshReviewsCannotRetryTwice(t *testing.T) {
	c := newProgramResumeConsumer(t, model.ProgramLimits{})
	other, err := Open(c.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	commands := []model.TypedCommand{c.command(t, 3), c.command(t, 4)}
	firstReview, firstProof := commandTestAuthorization(t, c.s, c.session, commands[0], c.cfg)
	secondReview, secondProof := commandTestAuthorization(t, c.s, c.session, commands[1], c.cfg)
	unchanged := programResumeUnselected(t, c)
	budgets := programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`)
	if !reflect.DeepEqual(firstReview.ResumeWork, secondReview.ResumeWork) || firstReview.ID == secondReview.ID || firstProof.ChallengeID == secondProof.ChallengeID {
		t.Fatal("distinct fresh reviews did not bind the same pre-commit target with independent challenges")
	}
	start := make(chan struct{})
	type result struct {
		index   int
		receipt model.CommandReceipt
		err     error
	}
	results := make(chan result, 2)
	proofs := []CommandAuthorization{firstProof, secondProof}
	for i, db := range []*Store{c.s, other} {
		go func() {
			<-start
			r, err := db.ExecuteCommand(context.Background(), c.session.TokenHash, c.session.CSRFHash, commands[i], c.cfg, &proofs[i])
			results <- result{i, r, err}
		}()
	}
	close(start)
	winner, loser := -1, -1
	for range 2 {
		got := <-results
		if got.err == nil {
			if winner != -1 {
				t.Fatal("both independent reviews retried the same failed work")
			}
			winner = got.index
		} else {
			answerErrorCode(t, got.err, "stale_revision")
			loser = got.index
		}
	}
	if winner == -1 || loser == -1 {
		t.Fatal("fresh review race had no unique committed winner")
	}
	if !reflect.DeepEqual(programResumeUnselected(t, c), unchanged) || !reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`), budgets) {
		t.Fatal("independent review race reset history or consumed worker budgets")
	}
	credential, err := c.s.GetCredential(context.Background(), c.session.CredentialID)
	if err != nil || credential.SignCount != proofs[winner].Credential.SignCount {
		t.Fatalf("stale independent review advanced the credential counter: %+v %v", credential, err)
	}
	for i, proof := range proofs {
		challenge, err := c.s.GetAuthChallenge(context.Background(), proof.ChallengeID)
		if err != nil || (challenge.ConsumedAt != nil) != (i == winner) {
			t.Fatalf("stale review consumed its independent challenge: %+v %v", challenge, err)
		}
	}
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_commands`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_approval_receipts`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM events WHERE kind='retry_requested'`, 1)
	commandTestCount(t, c.s, `SELECT count(*) FROM intake_audits WHERE json_extract(data,'$.audit.command_id')=?`, 0, commands[loser].CommandID)
	before := programResumeSnapshot(t, c.s)
	_, err = c.s.ExecuteCommand(context.Background(), c.session.TokenHash, c.session.CSRFHash, commands[loser], c.cfg, &proofs[loser])
	answerErrorCode(t, err, "stale_revision")
	if !reflect.DeepEqual(programResumeSnapshot(t, c.s), before) {
		t.Fatal("stale second review changed domain, ownership, counters, budgets, or audit")
	}
}

func TestIntakeProgramResumeCommitRejectsChangedOwnershipAndAuthority(t *testing.T) {
	for _, kind := range []string{"active terminal", "running", "uncertain", "cancelling", "Program cancelled", "changed revision", "invalidated manifest"} {
		t.Run(kind, func(t *testing.T) {
			c := newProgramResumeConsumer(t, model.ProgramLimits{})
			ctx := context.Background()
			command := c.command(t, 5)
			_, proof := commandTestAuthorization(t, c.s, c.session, command, c.cfg)
			var err error
			switch kind {
			case "active terminal", "running", "uncertain", "cancelling":
				if err = c.s.Retry(ctx, c.step.FeatureID, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
				a, e := c.s.Claim(ctx, c.step.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600)
				if e != nil {
					t.Fatal(e)
				}
				switch kind {
				case "active terminal":
					a.State, a.Stage = "blocked", "failed"
					err = c.s.Save(ctx, a, "blocked", "worker ownership not settled", "", true)
				case "uncertain":
					a.State, a.Stage = "uncertain", "uncertain"
					err = c.s.Save(ctx, a, "blocked", "external operation unresolved", "", true)
				case "cancelling":
					err = c.s.Cancel(ctx, c.step.FeatureID, model.TrustedLocalAudit())
				}
			case "Program cancelled":
				err = c.s.CancelProgram(ctx, c.revision.ProgramID, c.revision.RevisionHash, model.TrustedLocalAudit())
			case "changed revision":
				spec := c.revision.Spec
				spec.Objective += "; new scope is not approved"
				_, err = c.s.ImportProgram(ctx, spec, model.ProgramBundle{})
			case "invalidated manifest":
				err = c.s.InvalidateProgramEvidence(ctx, c.revision.ProgramID, c.step.InputManifestSHA, "withdraw exact evidence", model.TrustedLocalAudit())
			}
			if err != nil {
				t.Fatal(err)
			}
			before := programResumeSnapshot(t, c.s)
			credential, err := c.s.GetCredential(ctx, c.session.CredentialID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &proof)
			if err == nil {
				t.Fatal("resume committed after reviewed ownership or authority changed")
			}
			var domain *model.ProgramError
			if !errors.As(err, &domain) {
				t.Fatalf("expected a domain rejection rather than storage failure: %v", err)
			}
			if !reflect.DeepEqual(programResumeSnapshot(t, c.s), before) {
				t.Fatal("rejected resume mutated ownership, budgets, counters, domain state, or audit")
			}
			stored, err := c.s.GetCredential(ctx, c.session.CredentialID)
			if err != nil || !reflect.DeepEqual(stored, credential) {
				t.Fatalf("rejection advanced verifier state: %+v %v", stored, err)
			}
			challenge, err := c.s.GetAuthChallenge(ctx, proof.ChallengeID)
			if err != nil || challenge.ConsumedAt != nil {
				t.Fatalf("rejection consumed challenge: %+v %v", challenge, err)
			}
			if _, err = c.s.GetCommandReceipt(ctx, c.session.OperatorID, command.CommandID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("rejection left a command receipt: %v", err)
			}
		})
	}
}

func TestIntakeProgramResumeCannotResetAttemptOrLifetimeCaps(t *testing.T) {
	for _, kind := range []string{"attempt cap", "Program lifetime model cap"} {
		t.Run(kind, func(t *testing.T) {
			limits := model.ProgramLimits{}
			if kind == "Program lifetime model cap" {
				limits.MaxModelCalls = 3
			}
			c := newProgramResumeConsumer(t, limits)
			if kind == "attempt cap" {
				c.cfg.MaxAttempts = 1
			}
			before := programResumeSnapshot(t, c.s)
			command := c.command(t, 6)
			_, err := c.s.CreateIntakeReview(context.Background(), c.session.TokenHash, command, c.cfg)
			answerErrorCode(t, err, "budget_exhausted")
			if !reflect.DeepEqual(programResumeSnapshot(t, c.s), before) {
				t.Fatal("exhausted resume review reset counters or changed persisted work")
			}
			commandTestCount(t, c.s, `SELECT count(*) FROM intake_reviews`, 0)
			commandTestCount(t, c.s, `SELECT count(*) FROM intake_auth_challenges`, 0)
			commandTestCount(t, c.s, `SELECT json_extract(data,'$.model_calls') FROM program_budgets WHERE program_id=?`, 3, c.revision.ProgramID)
			commandTestCount(t, c.s, `SELECT json_extract(data,'$.attempts') FROM features WHERE id=?`, 1, c.step.FeatureID)
		})
	}
}

func TestIntakeProgramResumeExternalPrerequisiteHasNoHiddenRetry(t *testing.T) {
	s := openTest(t)
	_, _, session := authTestActor(t, s, time.Now().UTC(), "resume-question")
	cfg := commandTestConfig()
	cfg.AstraModel = "provider/research"
	c := &programResumeConsumer{s: s, cfg: cfg, session: session}
	c.revision = c.importApproved(t, model.ProgramSpec{ID: "resume-question", Project: "demo", Title: "Needs operator input", Objective: "Do not retry completed research to bypass a question", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}})
	step := c.nextStep(t, model.StepResearch)
	a := c.start(t, step)
	result := researchStepResult()
	result.Artifacts[1].Payload = json.RawMessage(`{"questions":[{"id":"durability","text":"Must acknowledgements survive a crash?","decisive":true,"affected_ids":[],"route":"operator"}]}`)
	if err := s.CompleteProgramStep(context.Background(), a, result); err != nil {
		t.Fatal(err)
	}
	c.wait(t)
	command := c.command(t, 7)
	review, proof := commandTestAuthorization(t, s, session, command, cfg)
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal([]byte(review.ManifestCanonical), &manifest); err != nil {
		t.Fatal(err)
	}
	if review.ResumeWork != nil || manifest["resume_work"] != nil {
		t.Fatal("external-prerequisite-only review granted a hidden worker retry")
	}
	before := map[string][][]string{}
	for _, table := range []string{"features", "attempts", "calls", "program_steps", "program_budgets", "program_artifacts"} {
		before[table] = programResumeRows(t, s, `SELECT * FROM `+table+` ORDER BY rowid`)
	}
	if _, err := s.ExecuteCommand(context.Background(), session.TokenHash, session.CSRFHash, command, cfg, &proof); err != nil {
		t.Fatal(err)
	}
	for table, rows := range before {
		if !reflect.DeepEqual(programResumeRows(t, s, `SELECT * FROM `+table+` ORDER BY rowid`), rows) {
			t.Fatalf("external-prerequisite resume changed worker table %s", table)
		}
	}
	work, err := s.ProgramWork(context.Background(), cfg)
	if err != nil || len(work) != 1 || work[0].Step != nil || work[0].Wait == nil || work[0].Wait.Code != "question_unresolved" {
		t.Fatalf("resume bypassed decisive operator question: %+v %v", work, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM events WHERE kind='retry_requested'`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 1)
}

func programResumeJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type programResumeExecutionConsumer struct {
	*programResumeConsumer
	request  model.ExperimentRunRequest
	receipt  model.ExperimentReceipt
	artifact model.ProgramArtifact
	sibling  model.ProgramStep
}

func newProgramResumeExecutionConsumer(t *testing.T, receiptState string) programResumeExecutionConsumer {
	t.Helper()
	return newProgramResumeExecutionContracts(t, receiptState, false)
}

func newProgramResumeExecutionContracts(t *testing.T, receiptState string, withSibling bool) programResumeExecutionConsumer {
	t.Helper()
	ctx := context.Background()
	s := openTest(t)
	_, _, session := authTestActor(t, s, time.Now().UTC(), "resume-experiment")
	cfg := commandTestConfig()
	cfg.AstraModel = "provider/research"
	cfg.Discovery.ExperimentProfiles = map[string]model.ExperimentProfile{
		"bounded": {Argv: []string{"/private/runner/profile-secret"}, ImageIdentity: "sha256:reviewed-image"},
	}
	maxExperiments := 1
	if withSibling {
		maxExperiments = 2
	}
	cfg.Discovery.Limits.MaxExperiments = maxExperiments
	cfg.Discovery.Limits.MaxModelCalls = 3
	c := &programResumeConsumer{s: s, cfg: cfg, session: session}
	c.revision = c.importApproved(t, model.ProgramSpec{
		ID: "resume-experiment", Project: "demo", Title: "Retry frozen execution", Objective: "Keep exact grants and durable runner ownership",
		Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}},
		Limits:     model.ProgramLimits{MaxExperiments: maxExperiments, MaxModelCalls: 3},
	})
	research := c.nextStep(t, model.StepResearch)
	if err := s.CompleteProgramStep(ctx, c.start(t, research), researchStepResult()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	member := "experiments/ordering/input.txt"
	if err := os.MkdirAll(filepath.Join(root, "experiments", "ordering"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(member)), []byte("reviewed immutable input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bundle, err := s.blobs.ImportBundle(ctx, root, []string{member}, model.BundleLimits{MaxFiles: 1, MaxFileBytes: 4096, MaxTotalBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	contract := model.ExperimentContract{
		ID: "ordering", Hypothesis: "Ordering is stable", Falsification: "Captured ordering differs",
		Environment:       model.ExperimentEnvironment{Profile: "bounded", ImageIdentity: "sha256:reviewed-image"},
		BundleManifestSHA: bundle.ManifestSHA, BundleFiles: []string{member},
		Commands:     []model.ExperimentCommand{{Profile: "bounded"}},
		Observations: []model.ExperimentObservationSpec{{Name: "ordered", Type: "boolean", OutputPath: "result.json", JSONPointer: "/ordered"}},
		Acceptance:   []model.ExperimentPredicate{{Observation: "ordered", Operator: "eq", Value: json.RawMessage(`true`)}},
		Limits:       model.NormalizeDiscoveryPolicy(cfg.Discovery).ExperimentLimits,
	}
	contracts := []model.ExperimentContract{contract}
	if withSibling {
		earlierMember := "experiments/earlier/input.txt"
		if err = os.MkdirAll(filepath.Join(root, "experiments", "earlier"), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, filepath.FromSlash(earlierMember)), []byte("earlier reviewed input\n"), 0600); err != nil {
			t.Fatal(err)
		}
		earlierBundle, err := s.blobs.ImportBundle(ctx, root, []string{earlierMember}, model.BundleLimits{MaxFiles: 1, MaxFileBytes: 4096, MaxTotalBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		earlier := contract
		earlier.ID, earlier.BundleFiles, earlier.BundleManifestSHA = "earlier", []string{earlierMember}, earlierBundle.ManifestSHA
		contracts = []model.ExperimentContract{earlier, contract}
	}
	architecture := model.ArchitecturePayload{Decisions: []model.ArchitectureDecision{{
		ID: "ordering-choice", Title: "Measure before implementation",
		Alternatives: []model.ArchitectureAlternative{{ID: "measure", Description: "Use bounded observation"}, {ID: "assume", Description: "Assume ordering"}},
		Criteria:     []string{"Measured evidence"}, Selected: "measure", Rejected: []string{"assume"},
		Tradeoffs: []string{"Requires execution"}, Consequences: []string{"No implementation authority yet"},
	}}}
	architect := c.nextStep(t, model.StepArchitecture)
	if err = s.CompleteProgramStep(ctx, c.start(t, architect), model.ProgramStepResult{Outcome: "completed", Artifacts: []model.ProgramArtifactDraft{
		{Kind: model.ArtifactArchitecture, Payload: programResumeJSON(t, architecture)},
		{Kind: model.ArtifactExperimentContracts, Payload: programResumeJSON(t, model.ExperimentContractsPayload{Experiments: contracts})},
	}}); err != nil {
		t.Fatal(err)
	}
	evaluation := c.nextStep(t, model.StepPlanEvaluation)
	if err = s.CompleteProgramStep(ctx, c.start(t, evaluation), model.ProgramStepResult{Outcome: "completed", Artifacts: []model.ProgramArtifactDraft{{
		Kind: model.ArtifactEvaluation, Payload: programResumeJSON(t, model.EvaluationPayload{EvaluatedManifestSHA: evaluation.InputManifestSHA, Verdict: "accept"}),
	}}}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ProgramArtifactPage(ctx, c.revision.ProgramID, ProgramQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var selected model.ProgramArtifact
	for _, artifact := range page.Items {
		switch artifact.Kind {
		case model.ArtifactArchitecture:
			if err = s.ApproveProgramArtifact(ctx, c.revision.ProgramID, c.revision.RevisionHash, model.ArtifactArchitecture, artifact.ContentSHA, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
		case model.ArtifactExperimentContracts:
			selected = artifact
			for _, frozen := range contracts {
				if err = s.ApproveProgramArtifact(ctx, c.revision.ProgramID, c.revision.RevisionHash, model.ArtifactExperimentContracts, experimentexec.ContractSHA(frozen), cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if selected.ID == "" {
		t.Fatal("actual architecture completion did not publish frozen experiment contract")
	}
	var sibling model.ProgramStep
	if withSibling {
		work, err := s.ProgramWork(ctx, cfg)
		if err != nil || len(work) != 2 {
			t.Fatalf("expected both current frozen contracts: %+v %v", work, err)
		}
		for _, item := range work {
			if item.Step == nil || item.Wait != nil || item.Step.Kind != model.StepExperimentRun {
				t.Fatalf("unapproved experiment work: %+v", item)
			}
			step, err := s.EnsureProgramStep(ctx, *item.Step, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if step.SubjectID == experimentexec.ContractSHA(contract) {
				c.step = step
			} else {
				sibling = step
			}
		}
		if c.step.ID == "" || sibling.ID == "" {
			t.Fatal("current contracts did not create distinct actual steps")
		}
	} else {
		c.step = c.nextStep(t, model.StepExperimentRun)
	}
	a, err := s.Claim(ctx, c.step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	request, err := experimentexec.NormalizeRequest(model.ExperimentRunRequest{
		ProgramID: c.step.ProgramID, Revision: c.step.Revision, StepID: c.step.ID, AttemptID: a.ID,
		Contract: contract, Bundle: bundle, ExecutionPolicySHA: model.ExperimentPolicySHA(cfg.Discovery.ExperimentProfiles, cfg.Discovery.ExperimentLimits),
	})
	if err != nil {
		t.Fatal(err)
	}
	if receiptState == "not_launched" {
		// The request was normalized locally but never recorded or launched.
		// Store authority must derive absence from its durable ledgers.
		c.attempt = c.fail(t, a)
		c.step, err = s.ProgramStepForFeature(ctx, c.step.FeatureID)
		if err != nil {
			t.Fatal(err)
		}
		c.wait(t)
		return programResumeExecutionConsumer{programResumeConsumer: c, request: request, artifact: selected, sibling: sibling}
	}
	if err = s.RecordExperimentIntent(ctx, request); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// This is an honest nonconclusive Store persistence receipt: no commands,
	// binary identity, captured observations, acceptance, or physical-run proof
	// are invented. Real runner execution remains the executor's boundary.
	receipt := model.ExperimentReceipt{
		RunID: request.RunID, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID, Revision: request.Revision,
		StepID: request.StepID, AttemptID: request.AttemptID, ContractSHA: request.ContractSHA, BundleManifestSHA: bundle.ManifestSHA,
		State: receiptState, StartedAt: now, FinishedAt: &now, ExecutionOutcome: "infrastructure",
		ObservationOutcome: "incomplete", HypothesisOutcome: "inconclusive", FailureKind: "infrastructure",
		Detail: "private runner failure at /private/runner/receipt-secret", AttestationScope: "store-persistence-only-no-runner-process",
	}
	switch receiptState {
	case "cancelled":
		receipt.ExecutionOutcome, receipt.FailureKind = "cancelled", "cancelled"
	case "waiting_prerequisite":
		receipt.ExecutionOutcome, receipt.FailureKind = "prerequisite_missing", "prerequisite_missing"
	case "completed":
		receipt.ExecutionOutcome, receipt.FailureKind = "completed", "observation_incomplete"
	case "resolved_failed":
		receipt.FailureKind = "operator_resolved_failed"
		receipt.ResolutionAttestation = "store-persistence exact terminal resolution"
	case "uncertain", "running", "cancelling":
		receipt.ExecutionOutcome, receipt.FailureKind = "uncertain", "uncertain"
		receipt.FinishedAt = nil
	}
	if err = s.RecordExperimentReceipt(ctx, request.RunID, receipt); err != nil {
		t.Fatal(err)
	}
	a.Stage, a.Workspace = "experiment_poll", "/private/runner/attempt-secret"
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	a = programResumeStoredAttempt(t, s, a.ID)
	if receiptState == "resolved_failed" {
		a.Stage, a.State = "uncertain", "blocked"
		if err = s.Save(ctx, a, "blocked", "await exact recorded terminal resolution", "", true); err != nil {
			t.Fatal(err)
		}
		if err = s.ResolveProgramExperimentFailed(ctx, request.RunID, request.RequestSHA, receipt.ResolutionAttestation); err != nil {
			t.Fatal(err)
		}
		c.attempt = programResumeStoredAttempt(t, s, a.ID)
	} else if receiptState == "uncertain" || receiptState == "running" || receiptState == "cancelling" {
		a.Stage, a.State = "uncertain", "blocked"
		if err = s.Save(ctx, a, "blocked", "runner ownership unresolved", "", true); err != nil {
			t.Fatal(err)
		}
		c.attempt = programResumeStoredAttempt(t, s, a.ID)
	} else {
		c.attempt = c.fail(t, a)
	}
	c.step, err = s.ProgramStepForFeature(ctx, c.step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	c.wait(t)
	return programResumeExecutionConsumer{programResumeConsumer: c, request: request, receipt: receipt, artifact: selected, sibling: sibling}
}

func TestIntakeProgramResumeFrozenExperimentCreatesNewBoundIntent(t *testing.T) {
	for _, state := range []string{"failed", "cancelled", "waiting_prerequisite", "completed", "resolved_failed", "not_launched"} {
		t.Run(state, func(t *testing.T) {
			c := newProgramResumeExecutionConsumer(t, state)
			ctx := context.Background()
			command := c.command(t, 8)
			review, proof := commandTestAuthorization(t, c.s, c.session, command, c.cfg)
			work := review.ResumeWork
			if work == nil || work.StepID != c.step.ID || work.StepKind != model.StepExperimentRun || work.FeatureID != c.step.FeatureID || work.AttemptID != c.attempt.ID || work.AttemptActive ||
				work.SubjectID != c.request.ContractSHA || work.BundleManifestSHA != c.request.Bundle.ManifestSHA || work.ExecutionPolicySHA != c.request.ExecutionPolicySHA ||
				work.FeatureMaxAttempts != c.cfg.MaxAttempts || work.AttemptOutputSHA != intake.SHA256([]byte(c.attempt.Summary)) {
				t.Fatalf("execution resume lost exact frozen/settled bindings: %+v", work)
			}
			if state == "not_launched" {
				if !work.PriorLaunchAbsent || work.PriorRunID != "" || work.PriorRequestSHA != "" || work.PriorReceiptSHA != "" || work.PriorReceiptState != "" ||
					work.PriorReceiptConclusive || work.PriorExecutionOutcome != "" || work.PriorObservationOutcome != "" || work.PriorHypothesisOutcome != "" {
					t.Fatalf("unlaunched failure invented prior runner evidence: %+v", work)
				}
				commandTestCount(t, c.s, `SELECT count(*) FROM program_experiment_intents WHERE attempt_id=?`, 0, c.attempt.ID)
				commandTestCount(t, c.s, `SELECT count(*) FROM program_experiments WHERE attempt_id=?`, 0, c.attempt.ID)
			} else if work.PriorLaunchAbsent || work.PriorRunID != c.request.RunID || work.PriorRequestSHA != c.request.RequestSHA || work.PriorReceiptState != state ||
				work.PriorReceiptConclusive || work.PriorReceiptSHA != intake.SHA256(programResumeJSON(t, c.receipt)) ||
				work.PriorExecutionOutcome != c.receipt.ExecutionOutcome || work.PriorObservationOutcome != c.receipt.ObservationOutcome || work.PriorHypothesisOutcome != c.receipt.HypothesisOutcome {
				t.Fatalf("execution resume did not bind exact terminal runner evidence: %+v", work)
			}
			var manifest struct {
				ResumeWork       *model.ProgramResumeWork `json:"resume_work"`
				SelectedArtifact *model.ProgramArtifact   `json:"selected_artifact"`
			}
			if err := json.Unmarshal([]byte(review.ManifestCanonical), &manifest); err != nil || !reflect.DeepEqual(manifest.ResumeWork, work) {
				t.Fatalf("execution canonical manifest differs from displayed work: %+v %v", manifest, err)
			}
			if review.Claims.SubjectID != c.revision.ProgramID || review.Claims.SubjectRevision != c.revision.RevisionHash ||
				review.Claims.CanonicalPayloadSHA != c.request.ContractSHA || review.Claims.ExecutionPolicySHA != c.request.ExecutionPolicySHA ||
				intake.SHA256([]byte(review.CanonicalPayload)) != c.request.ContractSHA || manifest.SelectedArtifact == nil || manifest.SelectedArtifact.ID != c.artifact.ID ||
				intake.SHA256([]byte(review.SelectedArtifactCanonical)) != c.artifact.ContentSHA {
				t.Fatal("execution resume review did not retain exact contract/containing artifact material")
			}
			raw := string(programResumeJSON(t, review))
			for _, secret := range append(append([]string{}, c.secrets...), c.receipt.Detail, c.receipt.AttestationScope, "/private/runner/profile-secret", c.attempt.Workspace) {
				if secret != "" && strings.Contains(raw, secret) {
					t.Fatal("execution review disclosed a full receipt, argv, worker output, or capability")
				}
			}
			grants := programResumeRows(t, c.s, `SELECT * FROM program_grants ORDER BY rowid`)
			budgets := programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`)
			calls := programResumeRows(t, c.s, `SELECT * FROM calls ORDER BY rowid`)
			priorIntents := programResumeRows(t, c.s, `SELECT * FROM program_experiment_intents ORDER BY rowid`)
			priorReceipts := programResumeRows(t, c.s, `SELECT * FROM program_experiments ORDER BY rowid`)
			unchanged := programResumeUnselected(t, c.programResumeConsumer)
			receipt, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &proof)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(programResumeUnselected(t, c.programResumeConsumer), unchanged) {
				t.Fatal("execution resume changed historical or nonselected work")
			}
			step := c.nextStep(t, model.StepExperimentRun)
			if step.ID != c.step.ID {
				t.Fatal("execution resume replaced logical step")
			}
			next, err := c.s.Claim(ctx, step.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600)
			if err != nil {
				t.Fatal(err)
			}
			newRequest := c.request
			newRequest.RunID, newRequest.RequestSHA, newRequest.AttemptID = "", "", next.ID
			newRequest, err = experimentexec.NormalizeRequest(newRequest)
			if err != nil || newRequest.RunID == c.request.RunID || newRequest.RequestSHA == c.request.RequestSHA || newRequest.ContractSHA != c.request.ContractSHA {
				t.Fatalf("retry did not create attempt-bound run/request under same contract: %+v %v", newRequest, err)
			}
			beforeIntent := programResumeSnapshot(t, c.s)
			forged := newRequest
			forged.RunID, forged.RequestSHA = c.request.RunID, c.request.RequestSHA
			if err = c.s.RecordExperimentIntent(ctx, forged); err == nil {
				t.Fatal("new attempt reused historical runner identity")
			}
			if !reflect.DeepEqual(programResumeSnapshot(t, c.s), beforeIntent) {
				t.Fatal("rejected reused identity changed durable execution state")
			}
			if err = c.s.RecordExperimentIntent(ctx, newRequest); err != nil {
				t.Fatal(err)
			}
			if err = c.s.RecordExperimentIntent(ctx, newRequest); err != nil {
				t.Fatal(err)
			}
			commandTestCount(t, c.s, `SELECT count(*) FROM program_experiment_intents`, len(priorIntents)+1)
			commandTestCount(t, c.s, `SELECT json_extract(data,'$.experiments') FROM program_budgets WHERE program_id=?`, 1, c.revision.ProgramID)
			commandTestCount(t, c.s, `SELECT json_extract(data,'$.model_calls') FROM program_budgets WHERE program_id=?`, 3, c.revision.ProgramID)
			if !reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_grants ORDER BY rowid`), grants) ||
				!reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`), budgets) ||
				!reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM calls ORDER BY rowid`), calls) ||
				!reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_experiments ORDER BY rowid`), priorReceipts) ||
				!reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_experiment_intents WHERE run_id=?`, c.request.RunID), priorIntents) {
				t.Fatal("execution retry regranted, recharged logical/model budgets, or rewrote prior execution evidence")
			}
			beforeReplay := programResumeSnapshot(t, c.s)
			programResumeAssertHistory(t, c.programResumeConsumer, unchanged)
			replayed, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, nil)
			if err != nil || !reflect.DeepEqual(replayed, receipt) || !reflect.DeepEqual(programResumeSnapshot(t, c.s), beforeReplay) {
				t.Fatalf("execution receipt replay disturbed the newly owned launch: %+v %v", replayed, err)
			}
		})
	}
}

func TestIntakeProgramResumeUncertainExecutionRetainsOwnership(t *testing.T) {
	for _, state := range []string{"uncertain", "running", "cancelling"} {
		t.Run(state, func(t *testing.T) {
			c := newProgramResumeExecutionConsumer(t, state)
			before := programResumeSnapshot(t, c.s)
			_, err := c.s.CreateIntakeReview(context.Background(), c.session.TokenHash, c.command(t, 9), c.cfg)
			answerErrorCode(t, err, "attempt_unsettled")
			if !reflect.DeepEqual(programResumeSnapshot(t, c.s), before) {
				t.Fatal("unsettled execution review released ownership or reset budget/evidence")
			}
			commandTestCount(t, c.s, `SELECT count(*) FROM attempts WHERE id=? AND active=1`, 1, c.attempt.ID)
			commandTestCount(t, c.s, `SELECT count(*) FROM intake_auth_challenges`, 0)
			commandTestCount(t, c.s, `SELECT count(*) FROM intake_approval_receipts`, 0)
			commandTestCount(t, c.s, `SELECT count(*) FROM events WHERE kind='retry_requested'`, 0)
		})
	}
}

func TestIntakeProgramResumePreservesOrdinaryAdmissionAndDailyWaits(t *testing.T) {
	for _, gate := range []string{"daily allowance", "global capacity"} {
		t.Run(gate, func(t *testing.T) {
			c := newProgramResumeConsumer(t, model.ProgramLimits{})
			ctx := context.Background()
			if gate == "daily allowance" {
				// Close the allowance independently of the wall-clock UTC day;
				// resume must leave ReserveCall's ordinary gate authoritative.
				c.cfg.MaxCallsPerDay = 0
			}
			command := c.command(t, 10)
			_, proof := commandTestAuthorization(t, c.s, c.session, command, c.cfg)
			if _, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &proof); err != nil {
				t.Fatal(err)
			}
			if gate == "global capacity" {
				ordinary, err := c.s.Import(ctx, feature("resume-capacity-owner"))
				if err != nil {
					t.Fatal(err)
				}
				if err = c.s.Approve(ctx, ordinary.ID, ordinary.Revision, c.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
				if _, err = c.s.Claim(ctx, ordinary.ID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600); err != nil {
					t.Fatal(err)
				}
			}
			budget := programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`)
			calls := programResumeRows(t, c.s, `SELECT * FROM calls ORDER BY rowid`)
			beforeClaim := programResumeSnapshot(t, c.s)
			next, err := c.s.Claim(ctx, c.step.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600)
			if gate == "global capacity" {
				if !errors.Is(err, ErrWaiting) || !reflect.DeepEqual(programResumeSnapshot(t, c.s), beforeClaim) {
					t.Fatalf("resume bypassed ordinary capacity admission or changed ownership: %v", err)
				}
				commandTestCount(t, c.s, `SELECT count(*) FROM attempts WHERE feature_id=?`, 1, c.step.FeatureID)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			beforeReserve := programResumeSnapshot(t, c.s)
			err = c.s.ReserveCall(ctx, next, "research", next.ID+":research", c.cfg.MaxCallsPerDay)
			if !errors.Is(err, ErrWaiting) || !reflect.DeepEqual(programResumeSnapshot(t, c.s), beforeReserve) {
				t.Fatalf("resume bypassed exhausted daily allowance or discarded claimed ownership: %v", err)
			}
			if !reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`), budget) ||
				!reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM calls ORDER BY rowid`), calls) {
				t.Fatal("daily dispatch wait changed lifetime or daily accounting")
			}
			commandTestCount(t, c.s, `SELECT count(*) FROM attempts WHERE id=? AND active=1`, 1, next.ID)
			commandTestCount(t, c.s, `SELECT json_extract(data,'$.attempts') FROM features WHERE id=?`, 2, c.step.FeatureID)
		})
	}
}

func TestIntakeProgramResumeClientCannotSelectInternalRetryTarget(t *testing.T) {
	c := newProgramResumeConsumer(t, model.ProgramLimits{})
	for _, field := range []string{"step_id", "feature_id", "attempt_id", "resume_work"} {
		t.Run(field, func(t *testing.T) {
			payload := map[string]any{
				"kind": "program", "id": c.revision.ProgramID, "revision": c.revision.RevisionHash,
				field: map[string]string{"step_id": c.step.ID, "feature_id": c.step.FeatureID, "attempt_id": c.attempt.ID},
			}
			command := commandTestCommand(t, "40000000-0000-4000-8000-000000000011", model.ActionProgramResume, payload)
			before := programResumeSnapshot(t, c.s)
			_, err := c.s.ExecuteCommand(context.Background(), c.session.TokenHash, c.session.CSRFHash, command, c.cfg, nil)
			answerErrorCode(t, err, "invalid_request")
			if !reflect.DeepEqual(programResumeSnapshot(t, c.s), before) {
				t.Fatal("client-selected internal retry metadata changed Store authority or work")
			}
		})
	}
}

func TestIntakeProgramResumeSkipsHealthyEarlierExperiment(t *testing.T) {
	for _, state := range []string{"ready", "running"} {
		t.Run(state, func(t *testing.T) {
			c := newProgramResumeExecutionContracts(t, "failed", true)
			ctx := context.Background()
			if state == "running" {
				if _, err := c.s.Claim(ctx, c.sibling.FeatureID, c.cfg.PolicyHash, c.cfg.MaxAttempts, c.cfg.MaxConcurrent, 1, 3600); err != nil {
					t.Fatal(err)
				}
			}
			unchanged := programResumeUnselected(t, c.programResumeConsumer)
			budgets := programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`)
			command := c.command(t, 12)
			review, proof := commandTestAuthorization(t, c.s, c.session, command, c.cfg)
			if review.ResumeWork == nil || review.ResumeWork.StepID != c.step.ID || review.ResumeWork.SubjectID != c.request.ContractSHA || review.ResumeWork.FeatureID == c.sibling.FeatureID {
				t.Fatalf("healthy earlier contract hid or replaced the current failed target: %+v", review.ResumeWork)
			}
			if _, err := c.s.ExecuteCommand(ctx, c.session.TokenHash, c.session.CSRFHash, command, c.cfg, &proof); err != nil {
				t.Fatal(err)
			}
			programResumeAssertHistory(t, c.programResumeConsumer, unchanged)
			if !reflect.DeepEqual(programResumeRows(t, c.s, `SELECT * FROM program_budgets ORDER BY rowid`), budgets) {
				t.Fatal("multi-contract resume reset or recharged execution budgets")
			}
			sibling, err := c.s.Get(ctx, c.sibling.FeatureID)
			if err != nil || sibling.State != state {
				t.Fatalf("resume disturbed healthy sibling state: %+v %v", sibling, err)
			}
			selected, err := c.s.Get(ctx, c.step.FeatureID)
			if err != nil || selected.State != "ready" || selected.Attempts != 1 {
				t.Fatalf("later failed contract was not retried exactly once: %+v %v", selected, err)
			}
			commandTestCount(t, c.s, `SELECT count(*) FROM events WHERE kind='retry_requested' AND feature_id=?`, 1, c.step.FeatureID)
			commandTestCount(t, c.s, `SELECT count(*) FROM events WHERE kind='retry_requested' AND feature_id=?`, 0, c.sibling.FeatureID)
			commandTestCount(t, c.s, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, map[string]int{"ready": 0, "running": 1}[state], c.sibling.FeatureID)
		})
	}
}
