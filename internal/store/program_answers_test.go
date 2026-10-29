package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"openfactory/internal/model"
)

func answerProgramFixture(t *testing.T, s *Store, id string) (model.ProgramRevision, model.ProgramArtifact) {
	t.Helper()
	ctx := context.Background()
	r, err := s.ImportProgram(ctx, model.ProgramSpec{ID: id, Project: "demo", Title: "Answer authority", Objective: "Preserve exact evidence authority", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"test"}}, Limits: model.ProgramLimits{MaxIterations: 2, MaxModelCalls: 20, MaxSteps: 32, MaxExperiments: 8}}, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(ctx, id, r.RevisionHash, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	q := answerArtifactFixture(t, s, id, r.Number, model.ArtifactQuestions, "", model.QuestionsPayload{Questions: []model.ProgramQuestion{{ID: "durability", Text: "Must acknowledgements survive a crash?", Decisive: true, Route: "operator"}, {ID: "source-only", Text: "What does the public documentation say?", Route: "source"}}})
	return r, q
}

func answerArtifactFixture(t *testing.T, s *Store, id string, revision int, kind, manifest string, payload any) model.ProgramArtifact {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.blobs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	a := model.ProgramArtifact{ID: ID(), ProgramID: id, Revision: revision, Kind: kind, SchemaVersion: 1, ContentSHA: blob.SHA, InputManifestSHA: manifest, CreatedAt: time.Now().UTC()}
	raw, err = json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, a.ID, id, revision, kind, a.ContentSHA, raw); err != nil {
		t.Fatal(err)
	}
	return a
}

func answerSourceFixture(t *testing.T, s *Store, id string, revision int) model.SourceCapture {
	t.Helper()
	ctx := context.Background()
	blob, err := s.blobs.Put(ctx, []byte("A public source with exact captured text.\n"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(time.Hour)
	source := model.SourceCapture{ID: ID(), ProgramID: id, Revision: revision, ContentSHA256: blob.SHA, TextSHA256: blob.SHA, Valid: true, FreshUntil: &fresh}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, source.ID, id, revision, raw); err != nil {
		t.Fatal(err)
	}
	return source
}

func answerManifestFixture(t *testing.T, s *Store, id string, revision int, refs ...model.ProgramInputRef) model.ProgramManifest {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	manifest, err := programManifestTx(ctx, tx, id, revision, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func answerForQuestion(q model.ProgramArtifact) model.ProgramAnswer {
	return model.ProgramAnswer{QuestionArtifactID: q.ID, QuestionArtifactSHA: q.ContentSHA, QuestionID: "durability", Text: "Acknowledgements must be durable before returning.", Actor: "operator"}
}

func answerErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *model.ProgramError
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func TestProgramAnswerCreatesUnapprovedImmutableRevisionWithoutResettingBudget(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, q := answerProgramFixture(t, s, "answer-budget")
	budget := model.ProgramBudget{ModelCalls: 7, Steps: 9, Experiments: 2, Searches: 3, Sources: 4, CaptureBytes: 1234, ReservedCaptureBytes: 128}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = putProgramBudgetTx(ctx, tx, r.ProgramID, budget); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	p, err := programLoad(ctx, tx, r.ProgramID)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	p.Iteration = 1
	if err = programPut(ctx, tx, p); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	answer := answerForQuestion(q)
	answer.CreatedAt = time.Unix(1, 0)
	next, err := s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil {
		t.Fatal(err)
	}
	if next.Number != r.Number+1 || next.Supersedes != r.Number || next.Reason != "answer" || next.RevisionHash == r.RevisionHash || programHash(next.Spec) != programHash(r.Spec) {
		t.Fatalf("incorrect answer revision: %+v", next)
	}
	p, err = programLoad(ctx, s.db, r.ProgramID)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != model.ProgramDraft || p.Iteration != 1 || p.Wait == nil || p.Wait.Code != "approval_required" {
		t.Fatalf("answer granted authority or reset iteration: %+v", p)
	}
	stored, err := programRevision(ctx, s.db, r.ProgramID, r.Number)
	if err != nil || programHash(stored) != programHash(r) {
		t.Fatalf("predecessor was changed: %v", err)
	}
	var raw []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_budgets WHERE program_id=?`, r.ProgramID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var retained model.ProgramBudget
	if err = json.Unmarshal(raw, &retained); err != nil {
		t.Fatal(err)
	}
	if retained != budget {
		t.Fatalf("lifetime budget changed: %+v", retained)
	}
	var answerID string
	if err = s.db.QueryRowContext(ctx, `SELECT id,data FROM program_answers WHERE program_id=?`, r.ProgramID).Scan(&answerID, &raw); err != nil {
		t.Fatal(err)
	}
	var recorded model.ProgramAnswer
	if err = json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.RevisionHash != r.RevisionHash || recorded.ProgramID != r.ProgramID || recorded.CreatedAt.Equal(answer.CreatedAt) || answerID != model.ProgramAnswerSHA(recorded) {
		t.Fatalf("answer not server bound: %+v", recorded)
	}
	recorded.CreatedAt = recorded.CreatedAt.Add(time.Hour)
	if answerID != model.ProgramAnswerSHA(recorded) {
		t.Fatal("answer identity depends on server timestamp")
	}
	if err = s.ResumeProgram(ctx, r.ProgramID, next.RevisionHash, model.Config{PolicyHash: "policy"}, model.TrustedLocalAudit()); err == nil {
		t.Fatal("answer inherited prior revision approval")
	}
	if err = s.ApproveProgram(ctx, r.ProgramID, next.RevisionHash, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
}

func TestProgramAnswerRejectsStaleReplayAndExactQuestionBinding(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, q := answerProgramFixture(t, s, "answer-bindings")
	answer := answerForQuestion(q)
	for _, mutate := range []func(*model.ProgramAnswer){
		func(a *model.ProgramAnswer) { a.QuestionArtifactSHA = programHash("not captured") },
		func(a *model.ProgramAnswer) { a.QuestionID = "absent" },
		func(a *model.ProgramAnswer) { a.QuestionID = "source-only" },
		func(a *model.ProgramAnswer) { a.Text = "\n" },
		func(a *model.ProgramAnswer) { a.ProgramID = "another-program" },
	} {
		bad := answer
		mutate(&bad)
		if _, err := s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, bad, model.TrustedLocalAudit()); err == nil {
			t.Fatalf("accepted invalid answer: %+v", bad)
		}
	}
	next, err := s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	answerErrorCode(t, err, "stale_revision")
	replay, err := s.AnswerProgramQuestion(ctx, r.ProgramID, next.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil || replay.RevisionHash != next.RevisionHash {
		t.Fatalf("current replay changed revision: %+v %v", replay, err)
	}
	if err = s.ApproveProgram(ctx, r.ProgramID, next.RevisionHash, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	answer.Text = "The operator now requires an explicit durable journal."
	changed, err := s.AnswerProgramQuestion(ctx, r.ProgramID, next.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil || changed.Number != next.Number+1 || changed.RevisionHash == next.RevisionHash {
		t.Fatalf("changed answer not revisioned: %+v %v", changed, err)
	}
	p, err := programLoad(ctx, s.db, r.ProgramID)
	if err != nil || p.State != model.ProgramDraft {
		t.Fatalf("changed answer retained approval: %+v %v", p, err)
	}
	var answers, artifacts int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_answers WHERE program_id=?`, r.ProgramID).Scan(&answers); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts WHERE program_id=?`, r.ProgramID).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if answers != 2 || artifacts != 1 {
		t.Fatalf("answer history/questions mutated: answers=%d artifacts=%d", answers, artifacts)
	}
}

func TestProgramAnswerValidatesSupportingEvidenceScopeHashAndInvalidations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, q := answerProgramFixture(t, s, "answer-support")
	foreign, _ := answerProgramFixture(t, s, "answer-foreign")
	source := answerSourceFixture(t, s, r.ProgramID, r.Number)
	other := answerSourceFixture(t, s, foreign.ProgramID, foreign.Number)
	answer := answerForQuestion(q)
	for _, ref := range []model.ProgramInputRef{
		{Kind: "source", ID: source.ID, SHA: programHash("wrong"), Revision: r.Number},
		{Kind: "source", ID: other.ID, SHA: other.TextSHA256, Revision: foreign.Number},
		{Kind: "bundle", ID: programHash("unowned-bundle"), SHA: programHash("unowned-bundle"), Revision: r.Number},
		{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: r.Number + 1},
		{Kind: model.ArtifactQuestions, ID: q.ID, SHA: q.ContentSHA, Revision: r.Number + 1},
	} {
		answer.SupportingRefs = []model.ProgramInputRef{ref}
		if _, err := s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit()); err == nil {
			t.Fatalf("accepted invalid supporting ref: %+v", ref)
		}
	}
	if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, "source retracted", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	answer.SupportingRefs = []model.ProgramInputRef{{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: r.Number}}
	_, err := s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	answerErrorCode(t, err, "evidence_invalid")
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_answers WHERE program_id=?`, r.ProgramID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("invalid supporting evidence persisted an answer")
	}
}

func answerCascadeFixture(t *testing.T, s *Store, id string) (model.ProgramRevision, model.SourceCapture, model.ProgramArtifact, model.ProgramManifest) {
	t.Helper()
	r, _ := answerProgramFixture(t, s, id)
	source := answerSourceFixture(t, s, id, r.Number)
	first := answerManifestFixture(t, s, id, r.Number, model.ProgramInputRef{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: r.Number})
	claims := answerArtifactFixture(t, s, id, r.Number, model.ArtifactClaims, first.SHA, model.ClaimsPayload{Claims: []model.ProgramClaim{{ID: "claim-durability", Statement: "The documentation describes durability.", Classification: "public_statement", Citations: []model.SourceCitation{{SourceID: source.ID, TextSHA: source.TextSHA256, Quote: "public source"}}}}})
	second := answerManifestFixture(t, s, id, r.Number, model.ProgramInputRef{Kind: model.ArtifactClaims, ID: claims.ID, SHA: claims.ContentSHA, Revision: r.Number})
	evaluation := answerArtifactFixture(t, s, id, r.Number, model.ArtifactEvaluation, second.SHA, model.EvaluationPayload{EvaluatedManifestSHA: second.SHA, Verdict: "accept"})
	last := answerManifestFixture(t, s, id, r.Number, model.ProgramInputRef{Kind: model.ArtifactEvaluation, ID: evaluation.ID, SHA: evaluation.ContentSHA, Revision: r.Number})
	return r, source, evaluation, last
}

func TestProgramInvalidationCascadesBeforeEffectsAndPreservesUncertainOwnership(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, source, evaluation, manifest := answerCascadeFixture(t, s, "invalidation-active")
	ready := approved(t, s, "invalidation-ready")
	running := approved(t, s, "invalidation-running")
	a := claim(t, s, running.ID)
	a.Stage, a.State = "uncertain", "blocked"
	if err := s.Save(ctx, a, "blocked", "external operation uncertain", "", true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []model.Feature{ready, running} {
		stored, err := s.Get(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		stored.ProgramEvidenceGate = &model.ProgramEvidenceGate{ProgramID: r.ProgramID, Revision: r.Number, ManifestSHA: manifest.SHA, EvaluationArtifactID: evaluation.ID}
		if err = put(ctx, s.db, stored); err != nil {
			t.Fatal(err)
		}
	}
	var before []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM attempts WHERE id=?`, a.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, "publisher retraction", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{source.ID, source.ContentSHA256, evaluation.ID, evaluation.ContentSHA, manifest.SHA} {
		if err := programInvalidTx(ctx, s.db, r.ProgramID, subject); err == nil {
			t.Fatalf("missing transitive invalidation: %s", subject)
		}
	}
	blocked, err := s.Get(ctx, ready.ID)
	if err != nil || blocked.State != "blocked" || blocked.PolicyHash != "" || blocked.Reason != "evidence_invalid" {
		t.Fatalf("ready feature kept authority: %+v %v", blocked, err)
	}
	cancelling, err := s.Get(ctx, running.ID)
	if err != nil || cancelling.State != "cancelling" || cancelling.PolicyHash != "policy" {
		t.Fatalf("active owner was discarded: %+v %v", cancelling, err)
	}
	var after []byte
	var active int
	if err = s.db.QueryRowContext(ctx, `SELECT data,active FROM attempts WHERE id=?`, a.ID).Scan(&after, &active); err != nil {
		t.Fatal(err)
	}
	if active != 1 || string(after) != string(before) {
		t.Fatal("invalidation settled or rewrote uncertain attempt ownership")
	}
	if err = s.ReserveCall(ctx, a, "implement", "after-invalidation", 100); err == nil {
		t.Fatal("invalid evidence authorized a model effect")
	}
	if err = s.BeginOperation(ctx, a, "policy"); err == nil {
		t.Fatal("invalid evidence authorized an external effect")
	}
	var calls int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM calls WHERE attempt_id=?`, a.ID).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("failed evidence gate consumed a call")
	}
	p, err := programLoad(ctx, s.db, r.ProgramID)
	if err != nil || p.State != model.ProgramBlocked || p.Wait == nil || p.Wait.Code != "evidence_invalid" {
		t.Fatalf("program not blocked: %+v %v", p, err)
	}
}

func TestProgramInvalidationRetainsDeliveredHistoryAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r, source, evaluation, manifest := answerCascadeFixture(t, s, "invalidation-history")
	delivered := feature("invalidation-delivered")
	delivered.State, delivered.PolicyHash, delivered.Revision, delivered.Receipt = "delivered", "policy", "delivered-revision", "immutable-delivery-receipt"
	delivered.ProgramEvidenceGate = &model.ProgramEvidenceGate{ProgramID: r.ProgramID, Revision: r.Number, ManifestSHA: manifest.SHA, EvaluationArtifactID: evaluation.ID}
	if err = put(ctx, s.db, delivered); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM features WHERE id=?`, delivered.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = s.InvalidateProgramEvidence(ctx, r.ProgramID, "not-a-program-subject", "bad", model.TrustedLocalAudit()); err == nil {
		t.Fatal("accepted arbitrary invalidation subject")
	}
	if err = s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ContentSHA256, "retracted", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM features WHERE id=?`, delivered.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("delivered history was rewritten")
	}
	if err = programInvalidTx(ctx, s.db, r.ProgramID, manifest.SHA); err == nil {
		t.Fatal("transitive invalidation disappeared on restart")
	}
	var findings, artifacts int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_events WHERE program_id=? AND kind='impact_finding'`, r.ProgramID).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if findings != 1 {
		t.Fatalf("missing delivered impact finding: %d", findings)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts WHERE program_id=?`, r.ProgramID).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 3 {
		t.Fatalf("invalidation erased immutable artifacts: %d", artifacts)
	}
}

func TestProgramInvalidationTraversesExperimentReceiptAndRevokesExactSubjectGrant(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := answerProgramFixture(t, s, "invalidation-experiment")
	source := answerSourceFixture(t, s, r.ProgramID, r.Number)
	contract := model.ExperimentContract{
		ID: "durable-ordering", Hypothesis: "The journal preserves ordering.", Falsification: "An acknowledgement precedes durable commit.",
		Environment:       model.ExperimentEnvironment{Profile: "cpu-test", DatasetRefs: []model.ProgramInputRef{{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: r.Number}}},
		BundleManifestSHA: programHash("frozen-bundle"),
		Commands:          []model.ExperimentCommand{{Profile: "cpu-test"}},
		Observations:      []model.ExperimentObservationSpec{{Name: "ordered", Type: "boolean", OutputPath: "result.json"}},
		Acceptance:        []model.ExperimentPredicate{{Observation: "ordered", Operator: "eq", Value: json.RawMessage("true")}},
		Limits:            model.ExperimentLimits{WallSeconds: 30, MaxOutputBytes: 1024, MaxArtifactBytes: 1024, MaxArtifactFiles: 1},
	}
	artifact := answerArtifactFixture(t, s, r.ProgramID, r.Number, model.ArtifactExperimentContracts, "", model.ExperimentContractsPayload{Experiments: []model.ExperimentContract{contract}})
	contractSHA := programHash(contract)
	if err := s.ApproveProgramArtifact(ctx, r.ProgramID, r.RevisionHash, model.ArtifactExperimentContracts, contractSHA, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	receipt := model.ExperimentReceipt{RunID: ID(), ProgramID: r.ProgramID, Revision: r.Number, AttemptID: "owned-attempt", ContractSHA: contractSHA, BundleManifestSHA: contract.BundleManifestSHA, State: "completed", Conclusive: true}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?)`, receipt.RunID, r.ProgramID, receipt.AttemptID, "", raw); err != nil {
		t.Fatal(err)
	}
	manifest := answerManifestFixture(t, s, r.ProgramID, r.Number, model.ProgramInputRef{Kind: "experiment_receipt", ID: receipt.RunID, SHA: programHash(receipt), Revision: r.Number})
	if err = s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, "dataset retracted", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{artifact.ID, artifact.ContentSHA, contractSHA, receipt.RunID, programHash(receipt), manifest.SHA} {
		if err = programInvalidTx(ctx, s.db, r.ProgramID, subject); err == nil {
			t.Fatalf("missing experiment dependency invalidation: %s", subject)
		}
	}
	var grants int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND subject_hash=?`, r.ProgramID, contractSHA).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Fatal("invalidated experiment retained exact subject approval")
	}
	if err = s.ApproveProgramArtifact(ctx, r.ProgramID, r.RevisionHash, model.ArtifactExperimentContracts, contractSHA, "policy", model.TrustedLocalAudit()); err == nil {
		t.Fatal("invalidated experiment could be reapproved")
	}
	var retained []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE run_id=?`, receipt.RunID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(raw) {
		t.Fatal("invalidation changed the immutable experiment receipt")
	}
}

func TestProgramInvalidationRejectsForeignSubjectsAndRetainsAuditEntries(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := answerProgramFixture(t, s, "invalidation-owner")
	foreign, _ := answerProgramFixture(t, s, "invalidation-other")
	source := answerSourceFixture(t, s, r.ProgramID, r.Number)
	other := answerSourceFixture(t, s, foreign.ProgramID, foreign.Number)
	if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, other.ID, "wrong owner", model.TrustedLocalAudit()); err == nil {
		t.Fatal("invalidated another program's source")
	}
	if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, "*", "wildcard", model.TrustedLocalAudit()); err == nil {
		t.Fatal("accepted wildcard evidence invalidation")
	}
	for _, fields := range []struct {
		reason string
		audit  model.AuditContext
	}{{"", model.TrustedLocalAudit()}, {"retracted", model.AuditContext{}}} {
		if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, fields.reason, fields.audit); err == nil {
			t.Fatal("accepted missing reason or typed audit authority")
		}
	}
	if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, "initial retraction", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	var entryID string
	var original []byte
	if err := s.db.QueryRowContext(ctx, `SELECT id,data FROM program_invalidations WHERE program_id=? AND subject=? ORDER BY rowid LIMIT 1`, r.ProgramID, source.ID).Scan(&entryID, &original); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, "additional impact", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM program_invalidations WHERE id=?`, entryID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if string(original) != string(retained) {
		t.Fatal("later invalidation replaced immutable audit history")
	}
	var entries int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_invalidations WHERE program_id=? AND subject=?`, r.ProgramID, source.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 2 {
		t.Fatalf("lost invalidation audit history: %d", entries)
	}
}

func TestProgramInvalidationRetainsAcceptedFeatureReceiptAndRecordsImpact(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, source, evaluation, manifest := answerCascadeFixture(t, s, "invalidation-accepted")
	f := approved(t, s, "invalidation-accepted-feature")
	a := claim(t, s, f.ID)
	receipt := "accepted integration receipt: immutable candidate and target"
	if err := s.Save(ctx, a, "accepted", "", receipt, false); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	accepted.ProgramEvidenceGate = &model.ProgramEvidenceGate{ProgramID: r.ProgramID, Revision: r.Number, ManifestSHA: manifest.SHA, EvaluationArtifactID: evaluation.ID}
	if err = put(ctx, s.db, accepted); err != nil {
		t.Fatal(err)
	}
	var featureBefore, attemptBefore []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM features WHERE id=?`, f.ID).Scan(&featureBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM attempts WHERE id=?`, a.ID).Scan(&attemptBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.InvalidateProgramEvidence(ctx, r.ProgramID, source.ID, "post-delivery evidence retraction", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	retained, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != "accepted" || retained.Receipt != receipt || retained.PolicyHash != "policy" {
		t.Fatalf("accepted delivery was revoked or its receipt changed: %+v", retained)
	}
	var featureAfter, attemptAfter []byte
	var active int
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM features WHERE id=?`, f.ID).Scan(&featureAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT data,active FROM attempts WHERE id=?`, a.ID).Scan(&attemptAfter, &active); err != nil {
		t.Fatal(err)
	}
	if string(featureAfter) != string(featureBefore) || string(attemptAfter) != string(attemptBefore) || active != 0 {
		t.Fatal("evidence invalidation rewrote accepted feature or settled attempt history")
	}
	var programFindings, featureFindings int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_events WHERE program_id=? AND kind='impact_finding'`, r.ProgramID).Scan(&programFindings); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE feature_id=? AND kind='impact_finding'`, f.ID).Scan(&featureFindings); err != nil {
		t.Fatal(err)
	}
	if programFindings != 1 || featureFindings != 1 {
		t.Fatalf("missing accepted delivery impact findings: program=%d feature=%d", programFindings, featureFindings)
	}
	if err = programInvalidTx(ctx, s.db, r.ProgramID, manifest.SHA); err == nil {
		t.Fatal("accepted history preservation suppressed evidence invalidation")
	}
}
