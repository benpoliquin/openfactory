package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"openfactory/internal/model"
)

func programTxSpec(id string) model.ProgramSpec {
	return model.ProgramSpec{ID: id, Project: "demo", Title: "Atomic program authority", Objective: "Preserve exact revision and command authority", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"test"}}, Limits: model.ProgramLimits{MaxIterations: 2, MaxModelCalls: 20, MaxSteps: 32, MaxExperiments: 8}}
}

func TestProgramImportAndApprovalParticipateInOuterTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	r, err := s.importProgramTx(ctx, tx, programTxSpec("program-import-tx"), model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.approveProgramTx(ctx, tx, r.ProgramID, r.RevisionHash, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('program-import-receipt','receipt')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"programs", "program_revisions", "program_budgets", "program_grants", "program_events"} {
		var n int
		column := "program_id"
		if table == "programs" {
			column = "id"
		}
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE `+column+`=?`, r.ProgramID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("outer rollback retained %s: %d %v", table, n, err)
		}
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='program-import-receipt'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("receipt survived rollback: %d %v", n, err)
	}
}

func TestProgramArtifactApprovalBindsExpectedRevisionKindHashPolicyAndAudit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, err := s.ImportProgram(ctx, programTxSpec("program-artifact-exact"), model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	a := answerArtifactFixture(t, s, r.ProgramID, r.Number, model.ArtifactArchitecture, "", model.ArchitecturePayload{})
	for _, test := range []struct {
		revision     string
		kind         model.ProgramArtifactKind
		hash, policy string
		audit        model.AuditContext
	}{
		{"wrong-revision", model.ArtifactArchitecture, a.ContentSHA, "policy", model.TrustedLocalAudit()},
		{r.RevisionHash, model.ArtifactExperimentContracts, a.ContentSHA, "policy", model.TrustedLocalAudit()},
		{r.RevisionHash, model.ArtifactQuestions, a.ContentSHA, "policy", model.TrustedLocalAudit()},
		{r.RevisionHash, model.ArtifactArchitecture, programHash("other subject"), "policy", model.TrustedLocalAudit()},
		{r.RevisionHash, model.ArtifactArchitecture, a.ContentSHA, "", model.TrustedLocalAudit()},
		{r.RevisionHash, model.ArtifactArchitecture, a.ContentSHA, "policy", model.AuditContext{}},
	} {
		if err = s.ApproveProgramArtifact(ctx, r.ProgramID, test.revision, test.kind, test.hash, test.policy, test.audit); err == nil {
			t.Fatalf("accepted mismatched artifact authority: %+v", test)
		}
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=?`, r.ProgramID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected authority wrote a grant: %d %v", n, err)
	}
	if err = s.ApproveProgramArtifact(ctx, r.ProgramID, r.RevisionHash, model.ArtifactArchitecture, a.ContentSHA, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_grants WHERE program_id=? AND revision_hash=? AND subject_hash=? AND policy_hash=?`, r.ProgramID, r.RevisionHash, a.ContentSHA, "policy").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var grant model.ProgramGrant
	if err = json.Unmarshal(raw, &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Actor != model.AuditTrustedLocal || grant.Audit != model.TrustedLocalAudit() {
		t.Fatalf("CLI fabricated operator/passkey evidence: %+v", grant)
	}
	changed := r.Spec
	changed.Objective += "; changed source"
	next, err := s.ImportProgram(ctx, changed, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgramArtifact(ctx, r.ProgramID, r.RevisionHash, model.ArtifactArchitecture, a.ContentSHA, "policy", model.TrustedLocalAudit()); err == nil {
		t.Fatal("approval silently rebound a stale subject to a newer revision")
	}
	answerErrorCode(t, err, "stale_revision")
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=?`, r.ProgramID, next.RevisionHash).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale artifact granted new revision: %d %v", n, err)
	}
}

func TestProgramArtifactGrantRollsBackWithOuterReceipt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, err := s.ImportProgram(ctx, programTxSpec("program-artifact-rollback"), model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	a := answerArtifactFixture(t, s, r.ProgramID, r.Number, model.ArtifactArchitecture, "", model.ArchitecturePayload{})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = s.approveProgramArtifactTx(ctx, tx, r.ProgramID, r.RevisionHash, model.ArtifactArchitecture, a.ContentSHA, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('artifact-command-receipt','receipt')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=?`, r.ProgramID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("artifact grant survived receipt rollback: %d %v", n, err)
	}
}

func TestProgramAnswerAndCancelNeverCommitOuterTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, q := answerProgramFixture(t, s, "program-answer-tx")
	answer := answerForQuestion(q)
	answer.Actor = "forged-network-actor"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	next, err := s.answerProgramQuestionTx(ctx, tx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.cancelProgramTx(ctx, tx, r.ProgramID, next.RevisionHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_answers WHERE program_id=?`, r.ProgramID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var recorded model.ProgramAnswer
	if err = json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Actor != model.AuditTrustedLocal || recorded.Audit != model.TrustedLocalAudit() {
		t.Fatalf("answer trusted caller-supplied actor: %+v", recorded)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	p, err := programLoad(ctx, s.db, r.ProgramID)
	if err != nil || p.CurrentRevision != r.Number || p.State != model.ProgramActive {
		t.Fatalf("answer/cancel committed outer mutation: %+v %v", p, err)
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_answers WHERE program_id=?`, r.ProgramID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("answer survived rollback: %d %v", n, err)
	}
	if _, err = programRevision(ctx, s.db, r.ProgramID, next.Number); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("amendment revision survived rollback: %v", err)
	}
}

func TestProgramResumeParticipatesInOuterTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := answerProgramFixture(t, s, "program-resume-tx")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := programLoad(ctx, tx, r.ProgramID)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	p.State, p.Wait = model.ProgramWaiting, &model.WaitReason{Code: "capacity", Detail: "saved wait"}
	if err = programPut(ctx, tx, p); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = s.resumeProgramTx(ctx, tx, r.ProgramID, r.RevisionHash, model.Config{PolicyHash: "policy"}, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	p, err = programLoad(ctx, s.db, r.ProgramID)
	if err != nil || p.State != model.ProgramWaiting || p.Wait == nil || p.Wait.Detail != "saved wait" {
		t.Fatalf("resume committed caller transaction: %+v %v", p, err)
	}
}

func TestProgramAnswerHashPreservesHistoricalShapeWithoutAudit(t *testing.T) {
	answer := model.ProgramAnswer{ProgramID: "legacy", RevisionHash: "revision", QuestionArtifactID: "questions", QuestionArtifactSHA: "sha", QuestionID: "question", Text: "exact answer", Actor: "operator", SupportingRefs: []model.ProgramInputRef{}, CreatedAt: time.Now().UTC()}
	legacy := struct {
		ProgramID           string                  `json:"program_id"`
		RevisionHash        string                  `json:"revision_hash"`
		QuestionArtifactID  string                  `json:"question_artifact_id"`
		QuestionArtifactSHA string                  `json:"question_artifact_sha"`
		QuestionID          string                  `json:"question_id"`
		Text                string                  `json:"text"`
		Actor               string                  `json:"actor"`
		SupportingRefs      []model.ProgramInputRef `json:"supporting_refs"`
		CreatedAt           time.Time               `json:"created_at"`
	}{answer.ProgramID, answer.RevisionHash, answer.QuestionArtifactID, answer.QuestionArtifactSHA, answer.QuestionID, answer.Text, answer.Actor, answer.SupportingRefs, time.Time{}}
	before := model.ProgramAnswerSHA(answer)
	if before != programHash(legacy) {
		t.Fatal("historical answer hash drifted after adding audit")
	}
	answer.Audit = model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: "operator-id", CredentialID: "credential", CommandID: "command", ApprovalID: "approval"}
	if model.ProgramAnswerSHA(answer) != before {
		t.Fatal("audit provenance changed immutable domain answer identity")
	}
}

func TestLegacyProgramAnswerLocalReplayPreservesActorHistoryAndStaleGate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, q := answerProgramFixture(t, s, "legacy-answer-local-replay")
	legacy := answerForQuestion(q)
	legacy.ProgramID, legacy.RevisionHash = r.ProgramID, r.RevisionHash
	legacy.Actor, legacy.CreatedAt = "alice-before-passkeys", time.Now().UTC()
	answerID := model.ProgramAnswerSHA(legacy)
	// Seed a schema-3 answer envelope: its free-form actor is historical data,
	// not a newly supplied trusted-local identity, and it has no Audit field.
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	delete(envelope, "audit")
	legacyRaw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	next := model.ProgramRevision{ProgramID: r.ProgramID, Number: r.Number + 1, Spec: r.Spec, SeedManifestSHA: r.SeedManifestSHA, Supersedes: r.Number, Reason: "answer", CreatedAt: time.Now().UTC()}
	next.RevisionHash = programHash(struct {
		ProgramID       string
		Number          int
		Spec            model.ProgramSpec
		SeedManifestSHA string
		Predecessor     string
		AnswerID        string
	}{r.ProgramID, next.Number, next.Spec, next.SeedManifestSHA, r.RevisionHash, answerID})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_answers(id,program_id,data) VALUES(?,?,?)`, answerID, r.ProgramID, legacyRaw); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_revisions(program_id,number,revision_hash,data) VALUES(?,?,?,?)`, r.ProgramID, next.Number, next.RevisionHash, raw); err != nil {
		t.Fatal(err)
	}
	p, err := programLoad(ctx, tx, r.ProgramID)
	if err != nil {
		t.Fatal(err)
	}
	p.CurrentRevision, p.State = next.Number, model.ProgramDraft
	p.Wait = &model.WaitReason{Code: "approval_required", Detail: "legacy answer requires new revision approval"}
	if err = programPut(ctx, tx, p); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	answer := answerForQuestion(q)
	answer.Actor = "untrusted-incoming-actor"
	_, err = s.AnswerProgramQuestion(ctx, r.ProgramID, r.RevisionHash, answer, model.TrustedLocalAudit())
	answerErrorCode(t, err, "stale_revision")
	replay, err := s.AnswerProgramQuestion(ctx, r.ProgramID, next.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil || replay.RevisionHash != next.RevisionHash {
		t.Fatalf("legacy local replay created a new revision: %+v %v", replay, err)
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_answers WHERE program_id=?`, r.ProgramID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("local replay appended an answer: %d %v", n, err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_answers WHERE id=?`, answerID).Scan(&raw); err != nil || string(raw) != string(legacyRaw) {
		t.Fatalf("local replay rewrote historical provenance: %s %v", raw, err)
	}
	answer.Text = "A genuinely changed answer must create new typed provenance."
	changed, err := s.AnswerProgramQuestion(ctx, r.ProgramID, next.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil || changed.Number != next.Number+1 {
		t.Fatalf("changed answer failed to create a revision: %+v %v", changed, err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_answers WHERE program_id=? ORDER BY rowid DESC LIMIT 1`, r.ProgramID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var recorded model.ProgramAnswer
	if err = json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Actor != model.AuditTrustedLocal || recorded.Audit != model.TrustedLocalAudit() {
		t.Fatalf("legacy replay exception accepted an actor override for new content: %+v", recorded)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_answers WHERE id=?`, answerID).Scan(&raw); err != nil || string(raw) != string(legacyRaw) {
		t.Fatalf("new answer rewrote legacy history: %s %v", raw, err)
	}
}

func TestProgramArtifactApprovalDistinguishesMalformedContractFromMissingEvidence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, err := s.ImportProgram(ctx, programTxSpec("program-artifact-errors"), model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	a := answerArtifactFixture(t, s, r.ProgramID, r.Number, model.ArtifactArchitecture, "", model.ArchitecturePayload{})
	for _, test := range []struct {
		name string
		kind model.ProgramArtifactKind
		hash string
		code string
	}{
		{"short digest", model.ArtifactArchitecture, "short", "invalid_contract"},
		{"nonhex digest", model.ArtifactArchitecture, "gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg", "invalid_contract"},
		{"uppercase digest", model.ArtifactArchitecture, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "invalid_contract"},
		{"unsupported kind", model.ArtifactQuestions, a.ContentSHA, "invalid_contract"},
		{"absent subject", model.ArtifactArchitecture, programHash("not a current artifact"), "evidence_invalid"},
		{"wrong evidence kind", model.ArtifactExperimentContracts, a.ContentSHA, "evidence_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := s.ApproveProgramArtifact(ctx, r.ProgramID, r.RevisionHash, test.kind, test.hash, "policy", model.TrustedLocalAudit())
			answerErrorCode(t, err, test.code)
		})
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=?`, r.ProgramID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected approval left a grant: %d %v", n, err)
	}
}
