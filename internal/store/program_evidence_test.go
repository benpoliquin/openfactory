package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func evidenceProgramFixture(t *testing.T, s *Store) (model.Program, model.ProgramRevision) {
	t.Helper()
	ctx := context.Background()
	spec := model.ProgramSpec{ID: "evidence-program", Project: "test", Title: "Evidence", Objective: "Check exact evidence", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}, Limits: model.ProgramLimits{MaxModelCalls: 5}}
	p := model.Program{ID: spec.ID, Project: spec.Project, CurrentRevision: 1, State: model.ProgramActive}
	r := model.ProgramRevision{ProgramID: p.ID, Number: 1, RevisionHash: strings.Repeat("1", 64), Spec: spec}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = programPut(ctx, tx, p); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_revisions(program_id,number,revision_hash,data) VALUES(?,?,?,?)`, p.ID, 1, r.RevisionHash, raw); err != nil {
		t.Fatal(err)
	}
	if err = putProgramBudgetTx(ctx, tx, p.ID, model.ProgramBudget{}); err != nil {
		t.Fatal(err)
	}
	g := model.ProgramGrant{ProgramID: p.ID, RevisionHash: r.RevisionHash, PolicyHash: "policy"}
	raw, _ = json.Marshal(g)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,'',?,?)`, p.ID, r.RevisionHash, "policy", raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return p, r
}
func evidenceSourceFixture(t *testing.T, s *Store, p model.Program, text string) model.SourceCapture {
	t.Helper()
	ctx := context.Background()
	blob, err := s.blobs.Put(ctx, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(time.Hour)
	source := model.SourceCapture{ID: "captured-source", ProgramID: p.ID, Revision: 1, ContentSHA256: blob.SHA, TextSHA256: blob.SHA, ByteLength: blob.Bytes, Valid: true, FreshUntil: &fresh}
	raw, _ := json.Marshal(source)
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, source.ID, p.ID, 1, raw); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestProgramCitationsProveOccurrenceNotInventedStatements(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := evidenceProgramFixture(t, s)
	source := evidenceSourceFixture(t, s, p, "Publisher says this may work.\nUnknown vendor internals remain unknown.\n")
	step := model.ProgramStep{ProgramID: p.ID, Revision: 1}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	good := model.SourceCitation{SourceID: source.ID, TextSHA: source.TextSHA256, Quote: "may work", StartLine: 1, EndLine: 1}
	if err = s.verifyProgramCitationTx(ctx, tx, step, good); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*model.SourceCitation){func(c *model.SourceCitation) { c.Quote = "guaranteed correct" }, func(c *model.SourceCitation) { c.TextSHA = strings.Repeat("f", 64) }, func(c *model.SourceCitation) { c.SourceID = "model-invented" }, func(c *model.SourceCitation) { c.StartLine = 2; c.EndLine = 2 }, func(c *model.SourceCitation) { c.ExtractionBlock = "invented-block" }} {
		forged := good
		change(&forged)
		if err = s.verifyProgramCitationTx(ctx, tx, step, forged); err == nil {
			t.Fatalf("accepted forged citation %+v", forged)
		}
	}
}

func TestProgramSourceReferenceScopeFreshnessAndInvalidation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := evidenceProgramFixture(t, s)
	source := evidenceSourceFixture(t, s, p, "Actual captured source")
	ref := model.ProgramInputRef{Kind: "source", ID: source.ID, SHA: source.TextSHA256}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = programValidateRefTx(ctx, tx, p.ID, 1, ref); err != nil {
		t.Fatal(err)
	}
	if err = programValidateRefTx(ctx, tx, "other-program", 1, ref); err == nil {
		t.Fatal("cross-program source accepted")
	}
	expired := time.Now().Add(-time.Second)
	source.FreshUntil = &expired
	raw, _ := json.Marshal(source)
	if _, err = tx.ExecContext(ctx, `UPDATE program_sources SET data=? WHERE id=?`, raw, source.ID); err != nil {
		t.Fatal(err)
	}
	if err = programValidateRefTx(ctx, tx, p.ID, 1, ref); err == nil {
		t.Fatal("stale source accepted")
	}
	fresh := time.Now().Add(time.Hour)
	source.FreshUntil = &fresh
	raw, _ = json.Marshal(source)
	if _, err = tx.ExecContext(ctx, `UPDATE program_sources SET data=? WHERE id=?`, raw, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_invalidations(id,program_id,subject,data) VALUES('invalidate',?,?, '{}')`, p.ID, source.TextSHA256); err != nil {
		t.Fatal(err)
	}
	if err = programValidateRefTx(ctx, tx, p.ID, 1, ref); err == nil {
		t.Fatal("invalidated source digest accepted")
	}
}

func TestProgramIndependentEvaluationCannotReuseAuthorThread(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := evidenceProgramFixture(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	a := model.ProgramArtifact{ID: "authored", ProgramID: p.ID, Revision: 1, Kind: model.ArtifactArchitecture, ContentSHA: strings.Repeat("a", 64), ProducerThreadID: "author-thread"}
	raw, _ := json.Marshal(a)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, a.ID, p.ID, 1, a.Kind, a.ContentSHA, raw); err != nil {
		t.Fatal(err)
	}
	m, err := programManifestTx(ctx, tx, p.ID, 1, []model.ProgramInputRef{{Kind: a.Kind, ID: a.ID, SHA: a.ContentSHA}})
	if err != nil {
		t.Fatal(err)
	}
	step := model.ProgramStep{ID: "evaluator", ProgramID: p.ID, Revision: 1, Kind: model.StepPlanEvaluation, InputManifestSHA: m.SHA, FeatureID: "openfactory-discovery-evaluator"}
	raw, _ = json.Marshal(step)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, p.ID, 1, step.FeatureID, raw); err != nil {
		t.Fatal(err)
	}
	f := model.Feature{DiscoveryStepID: step.ID}
	if err = independentProgramThreadTx(ctx, tx, f, "author-thread"); err == nil {
		t.Fatal("reused author session")
	}
	if err = independentProgramThreadTx(ctx, tx, f, "fresh-evaluator-thread"); err != nil {
		t.Fatal(err)
	}
	if err = independentProgramThreadTx(ctx, tx, f, ""); err == nil {
		t.Fatal("missing session accepted")
	}
}

func TestProgramLifetimeCallsHonorFrozenPolicyWithoutReset(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := evidenceProgramFixture(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	step := model.ProgramStep{ID: "research-step", ProgramID: p.ID, Revision: 1, Kind: model.StepResearch, FeatureID: "openfactory-discovery-research-step"}
	raw, _ := json.Marshal(step)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, p.ID, 1, step.FeatureID, raw); err != nil {
		t.Fatal(err)
	}
	policy := model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{})
	policy.Limits.MaxModelCalls = 1
	raw, _ = json.Marshal(policy)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_policies(program_id,revision,policy_hash,data) VALUES(?,?,?,?)`, p.ID, 1, "policy", raw); err != nil {
		t.Fatal(err)
	}
	f := model.Feature{DiscoveryStepID: step.ID, PolicyHash: "policy"}
	if err = reserveProgramCallTx(ctx, tx, f); err != nil {
		t.Fatal(err)
	}
	if err = reserveProgramCallTx(ctx, tx, f); err == nil {
		t.Fatal("exceeded lower frozen config cap")
	}
	budget, err := getProgramBudgetTx(ctx, tx, p.ID)
	if err != nil || budget.ModelCalls != 1 {
		t.Fatalf("budget mutated on rejection %+v %v", budget, err)
	}
}

func TestProgramExperimentCannotReusePreviousIterationArchitectureApproval(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, r := evidenceProgramFixture(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	refs := []model.ProgramInputRef{}
	for iteration := range 2 {
		a := model.ProgramArtifact{ID: []string{"old-architecture", "current-architecture"}[iteration], ProgramID: p.ID, Revision: 1, Iteration: iteration, Kind: model.ArtifactArchitecture, ContentSHA: strings.Repeat([]string{"a", "b"}[iteration], 64)}
		raw, _ := json.Marshal(a)
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, a.ID, p.ID, 1, a.Kind, a.ContentSHA, raw); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, model.ProgramInputRef{Kind: a.Kind, ID: a.ID, SHA: a.ContentSHA})
	}
	manifest, err := programManifestTx(ctx, tx, p.ID, 1, refs)
	if err != nil {
		t.Fatal(err)
	}
	step := model.ProgramStep{ID: "experiment-step", ProgramID: p.ID, Revision: 1, Iteration: 1, Kind: model.StepExperimentRun, SubjectID: strings.Repeat("c", 64), InputManifestSHA: manifest.SHA, FeatureID: "openfactory-discovery-experiment-step"}
	raw, _ := json.Marshal(step)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, p.ID, 1, step.FeatureID, raw); err != nil {
		t.Fatal(err)
	}
	grant := func(subject string) {
		t.Helper()
		if _, e := tx.ExecContext(ctx, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,?,'policy','{}')`, p.ID, r.RevisionHash, subject); e != nil {
			t.Fatal(e)
		}
	}
	grant(strings.Repeat("a", 64))
	grant(step.SubjectID)
	f := model.Feature{ID: step.FeatureID, DiscoveryStepID: step.ID, Kind: "research", CompletionTarget: "reviewed"}
	if err = s.programFeatureGateTx(ctx, tx, f, "policy"); err == nil {
		t.Fatal("previous iteration architecture approval authorized new experiment")
	}
	grant(strings.Repeat("b", 64))
	if err = s.programFeatureGateTx(ctx, tx, f, "policy"); err != nil {
		t.Fatalf("exact current approval rejected: %v", err)
	}
}

func TestProgramBlockedReimportCreatesFreshApprovalWithoutResettingLifetime(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, r := evidenceProgramFixture(t, s)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.State = model.ProgramBlocked
	p.Iteration = 2
	if err = programPut(ctx, tx, p); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	budget := model.ProgramBudget{ModelCalls: 4, Steps: 9, Experiments: 2, Searches: 3, Sources: 4, CaptureBytes: 100}
	if err = putProgramBudgetTx(ctx, tx, p.ID, budget); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(ctx, p.ID, r.RevisionHash, "policy", model.TrustedLocalAudit()); err == nil {
		t.Fatal("approval reactivated the blocked evidence revision")
	}
	blocked, err := programLoad(ctx, s.db, p.ID)
	if err != nil || blocked.State != model.ProgramBlocked {
		t.Fatalf("rejected approval changed blocked state: %+v %v", blocked, err)
	}
	revised, err := s.ImportProgram(ctx, r.Spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if revised.Number != 2 || revised.RevisionHash == r.RevisionHash {
		t.Fatal("blocked evidence recovery reused old approved revision")
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	current, err := programLoad(ctx, tx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := getProgramBudgetTx(ctx, tx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Iteration != 2 || after != budget {
		t.Fatalf("revision reset lifetime counters: %+v %+v", current, after)
	}
	if _, _, err = s.programAuthorityTx(ctx, tx, p.ID, 2, "policy"); err == nil {
		t.Fatal("new revision inherited approval")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(ctx, p.ID, revised.RevisionHash, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatalf("new evidence revision could not receive fresh approval: %v", err)
	}
}

func TestProgramCitationPointerCannotBorrowQuoteFromAnotherField(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := evidenceProgramFixture(t, s)
	source := evidenceSourceFixture(t, s, p, `{"statement":"actual value","elsewhere":"borrowed quote"}`)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	citation := model.SourceCitation{SourceID: source.ID, TextSHA: source.TextSHA256, Quote: "borrowed quote", JSONPointer: "/statement"}
	if err = s.verifyProgramCitationTx(ctx, tx, model.ProgramStep{ProgramID: p.ID, Revision: 1}, citation); err == nil {
		t.Fatal("JSON pointer citation borrowed quote outside selected field")
	}
	citation.JSONPointer = "/elsewhere"
	if err = s.verifyProgramCitationTx(ctx, tx, model.ProgramStep{ProgramID: p.ID, Revision: 1}, citation); err != nil {
		t.Fatal(err)
	}
}
