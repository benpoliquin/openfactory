package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
)

func programReportFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := openTest(t)
	s.blobs = artifacts.New(t.TempDir())
	ctx := context.Background()
	if _, e := s.db.ExecContext(ctx, programSchema+programResearchSchema); e != nil {
		t.Fatal(e)
	}
	spec := model.ProgramSpec{ID: "report-program", Project: "demo", Title: "Evidence discovery", Objective: "Compare actual evidence", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	if _, e := s.ImportProgram(ctx, spec, model.ProgramBundle{}); e != nil {
		t.Fatal(e)
	}
	return s, spec.ID
}
func programReportArtifact(t *testing.T, s *Store, id, artifactID, kind string, payload any) model.ProgramArtifact {
	t.Helper()
	ctx := context.Background()
	raw, e := json.Marshal(payload)
	if e != nil {
		t.Fatal(e)
	}
	blob, e := s.blobs.Put(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	a := model.ProgramArtifact{ID: artifactID, ProgramID: id, Revision: 1, Kind: kind, SchemaVersion: 1, ContentSHA: blob.SHA, ProducerStepID: "step", CreatedAt: time.Now().UTC()}
	b, _ := json.Marshal(a)
	if _, e = s.db.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, a.ID, id, 1, kind, a.ContentSHA, b); e != nil {
		t.Fatal(e)
	}
	return a
}
func programReportInsert(t *testing.T, s *Store, query string, value any, args ...any) {
	t.Helper()
	b, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	args = append(args, b)
	if _, e = s.db.ExecContext(context.Background(), query, args...); e != nil {
		t.Fatal(e)
	}
}

func TestProgramReportingQuestionElementPaginationAndBindings(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	a := programReportArtifact(t, s, id, "artifact-a", model.ArtifactQuestions, model.QuestionsPayload{Questions: []model.ProgramQuestion{{ID: "q1", Text: "Durability?", Decisive: true, Route: "operator"}, {ID: "q2", Text: "Ordering?", Route: "experiment"}, {ID: "q3", Text: "Provenance?", Route: "source"}}})
	answer := model.QuestionAnswer{ProgramID: id, RevisionHash: "original-revision", QuestionArtifactID: a.ID, QuestionArtifactSHA: a.ContentSHA, QuestionID: "q1", Text: "Operator acknowledgement", Actor: "operator"}
	programReportInsert(t, s, `INSERT INTO program_answers(id,program_id,data) VALUES(?,?,?)`, answer, "answer", id)
	p, e := s.ProgramQuestionPage(ctx, id, ProgramQuery{Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 1 || p.Items[0].Question.ID != "q1" || len(p.Items[0].Answers) != 1 || p.NextCursor == "" {
		t.Fatalf("first question page: %+v", p)
	}
	p2, e := s.ProgramQuestionPage(ctx, id, ProgramQuery{Limit: 1, After: p.NextCursor})
	if e != nil {
		t.Fatal(e)
	}
	if len(p2.Items) != 1 || p2.Items[0].Question.ID != "q2" || p2.Snapshot != p.Snapshot || p2.RevisionHash != p.RevisionHash {
		t.Fatalf("second question page: %+v", p2)
	}
	p3, e := s.ProgramQuestionPage(ctx, id, ProgramQuery{Limit: 1, After: p2.NextCursor})
	if e != nil || len(p3.Items) != 1 || p3.Items[0].Question.ID != "q3" || p3.NextCursor != "" {
		t.Fatalf("last question page: %+v %v", p3, e)
	}
	if _, e = s.ProgramArtifactPage(ctx, id, ProgramQuery{After: p.NextCursor}); !errors.Is(e, ErrInvalidQuery) {
		t.Fatalf("cross-endpoint cursor accepted: %v", e)
	}
	if _, e = s.ProgramQuestionPage(ctx, id, ProgramQuery{After: p.NextCursor, Kind: "different"}); !errors.Is(e, ErrInvalidQuery) {
		t.Fatalf("changed-filter cursor accepted: %v", e)
	}
	status, e := s.ProgramStatus(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	spec := status.Revision.Spec
	spec.Objective = "Changed authoritative objective"
	if _, e = s.ImportProgram(ctx, spec, model.ProgramBundle{}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ProgramQuestionPage(ctx, id, ProgramQuery{After: p.NextCursor}); !errors.Is(e, ErrInvalidQuery) {
		t.Fatalf("revision-stale cursor accepted: %v", e)
	}
	historical, e := s.ProgramQuestionPage(ctx, id, ProgramQuery{Revision: 1})
	if e != nil || len(historical.Items) != 3 {
		t.Fatalf("historical evidence inaccessible: %+v %v", historical, e)
	}
}

func TestProgramReportingCursorDetectsMutableReceiptAndBudget(t *testing.T) {
	for _, change := range []string{"receipt", "budget"} {
		t.Run(change, func(t *testing.T) {
			s, id := programReportFixture(t)
			ctx := context.Background()
			programReportArtifact(t, s, id, "a", model.ArtifactQuestions, model.QuestionsPayload{})
			programReportArtifact(t, s, id, "b", model.ArtifactQuestions, model.QuestionsPayload{})
			receipt := model.ExperimentReceipt{RunID: "run", ProgramID: id, Revision: 1, State: "running"}
			programReportInsert(t, s, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,'attempt','request',?)`, receipt, receipt.RunID, id)
			p, e := s.ProgramArtifactPage(ctx, id, ProgramQuery{Limit: 1})
			if e != nil || p.NextCursor == "" {
				t.Fatalf("initial page: %+v %v", p, e)
			}
			if change == "receipt" {
				receipt.State = "completed"
				receipt.Conclusive = true
				receipt.Accepted = false
				programReportInsert(t, s, `UPDATE program_experiments SET data=? WHERE run_id='run'`, receipt)
			} else {
				programReportInsert(t, s, `UPDATE program_budgets SET data=? WHERE program_id='report-program'`, model.ProgramBudget{ModelCalls: 1})
			}
			if _, e = s.ProgramArtifactPage(ctx, id, ProgramQuery{Limit: 1, After: p.NextCursor}); !errors.Is(e, ErrInvalidQuery) {
				t.Fatalf("same-row %s change not detected: %v", change, e)
			}
		})
	}
}

func TestProgramReportingProvenanceRedactionAndScope(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	sources := []model.SourceCapture{{ID: "a", ProgramID: id, Revision: 1, FinalURL: "https://example.org/article?token=hidden#part", RequestedURL: "https://example.org/article", RetrievedAt: time.Now().UTC(), ContentSHA256: strings.Repeat("a", 64), TextSHA256: strings.Repeat("b", 64), Valid: true, FreshUntil: &future}, {ID: "b", ProgramID: id, Revision: 1, FinalURL: "https://example.org/expired", Valid: true, FreshUntil: &past}, {ID: "c", ProgramID: id, Revision: 1, Valid: true, FreshUntil: &future}}
	for _, source := range sources {
		programReportInsert(t, s, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,1,?)`, source, source.ID, id)
	}
	invalid := model.ProgramInvalidation{ID: "invalidation", ProgramID: id, Subject: "c", Reason: "Evidence retracted"}
	programReportInsert(t, s, `INSERT INTO program_invalidations(id,program_id,subject,data) VALUES(?,?,?,?)`, invalid, invalid.ID, id, invalid.Subject)
	p, e := s.ProgramSourcePage(ctx, id, ProgramQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 3 || !p.Items[0].Valid || p.Items[1].Valid || p.Items[2].Valid {
		t.Fatalf("freshness/invalidation omitted: %+v", p)
	}
	if p.Items[0].FinalURL != "https://example.org/article" || p.Items[0].ContentSHA256 != sources[0].ContentSHA256 {
		t.Fatalf("unsafe URL or corrupted provenance: %+v", p.Items[0])
	}
	a := programReportArtifact(t, s, id, "artifact", model.ArtifactRevision, model.RevisionPayload{Trigger: "Read /Users/operator/private/source then token=do-not-expose", ChangedDecisions: []string{"new-decision"}})
	detail, e := s.ProgramArtifactDetail(ctx, id, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if detail.Artifact.ContentSHA != a.ContentSHA || strings.Contains(string(detail.Payload), "/Users/operator") || strings.Contains(string(detail.Payload), "do-not-expose") {
		t.Fatalf("artifact leak or lost immutable identity: %+v", detail)
	}
	other := model.ProgramSpec{ID: "other-program", Project: "demo", Title: "Other", Objective: "Separate scope", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	if _, e = s.ImportProgram(ctx, other, model.ProgramBundle{}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ProgramArtifactDetail(ctx, other.ID, a.ID); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-program artifact exposed: %v", e)
	}
	if _, e = s.ProgramStatus(ctx, "unknown"); !errors.Is(e, ErrNotFound) {
		t.Fatalf("unknown program: %v", e)
	}
	for _, q := range []ProgramQuery{{Limit: -1}, {Limit: 501}, {Revision: -1}, {After: "invalid"}} {
		if _, e = s.ProgramSourcePage(ctx, id, q); !errors.Is(e, ErrInvalidQuery) {
			t.Fatalf("invalid query accepted: %+v %v", q, e)
		}
	}
}

func TestProgramReportingExactExperimentApprovalAndFalsification(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	contract := model.ExperimentContract{ID: "ordering", Hypothesis: "Arrival order preserves causality", Falsification: "A later version is overwritten", Environment: model.ExperimentEnvironment{Profile: "python", Prerequisites: []string{"public fixture"}}, BundleManifestSHA: strings.Repeat("b", 64)}
	sha := programHash(contract)
	programReportArtifact(t, s, id, "experiments", model.ArtifactExperimentContracts, model.ExperimentContractsPayload{Experiments: []model.ExperimentContract{contract}})
	status, e := s.ProgramStatus(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	grant := model.ProgramGrant{ProgramID: id, RevisionHash: status.Revision.RevisionHash, SubjectHash: sha, PolicyHash: "policy", Actor: "operator"}
	programReportInsert(t, s, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,?,?,?)`, grant, id, grant.RevisionHash, sha, grant.PolicyHash)
	good := model.ExperimentReceipt{RunID: "actual-run", ProgramID: id, Revision: 1, ContractSHA: sha, State: "completed", Conclusive: true, Accepted: false, Observations: []model.ExperimentObservation{{Name: "causal_order_preserved", Type: "boolean", Value: json.RawMessage(`false`)}}, FailureKind: "hypothesis_falsified"}
	unrelated := good
	unrelated.RunID = "different-contract"
	unrelated.ContractSHA = strings.Repeat("c", 64)
	for _, receipt := range []model.ExperimentReceipt{good, unrelated} {
		programReportInsert(t, s, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,'attempt','request',?)`, receipt, receipt.RunID, id)
	}
	p, e := s.ProgramExperimentPage(ctx, id, ProgramQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 1 {
		t.Fatalf("missing experiment: %+v", p)
	}
	v := p.Items[0]
	if v.ContractSHA != sha || len(v.Grants) != 1 || v.Grants[0].SubjectHash != sha || len(v.Receipts) != 1 || v.Receipts[0].RunID != good.RunID || !v.Receipts[0].Conclusive || v.Receipts[0].Accepted || string(v.Receipts[0].Observations[0].Value) != "false" {
		t.Fatalf("exact approval/falsification not represented: %+v", v)
	}
	good.Conclusive = false
	good.FailureKind = "infrastructure"
	programReportInsert(t, s, `UPDATE program_experiments SET data=? WHERE run_id='actual-run'`, good)
	p, e = s.ProgramExperimentPage(ctx, id, ProgramQuery{})
	if e != nil || p.Items[0].Receipts[0].Conclusive || p.Items[0].Receipts[0].FailureKind != "infrastructure" {
		t.Fatalf("infrastructure failure became falsification: %+v %v", p, e)
	}
}

func TestProgramReportingPayloadPreservesEvidenceLocatorsAndIntegers(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	payload := model.ClaimsPayload{Claims: []model.ProgramClaim{{ID: "claim", Statement: "Pinned JSON evidence", Citations: []model.SourceCitation{{SourceID: "source", TextSHA: strings.Repeat("a", 64), Quote: "observed", JSONPointer: "/observations/0/value"}}}}}
	a := programReportArtifact(t, s, id, "claims", model.ArtifactClaims, payload)
	detail, e := s.ProgramArtifactDetail(ctx, id, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	var shown model.ClaimsPayload
	if e = json.Unmarshal(detail.Payload, &shown); e != nil {
		t.Fatal(e)
	}
	if shown.Claims[0].Citations[0].JSONPointer != "/observations/0/value" {
		t.Fatalf("evidence locator rewritten as private host path: %s", detail.Payload)
	}
	contract := model.ExperimentContract{ID: "integer", Acceptance: []model.ExperimentPredicate{{Observation: "version", Operator: "eq", Value: json.RawMessage(`9007199254740993`)}}}
	a = programReportArtifact(t, s, id, "integer", model.ArtifactExperimentContracts, model.ExperimentContractsPayload{Experiments: []model.ExperimentContract{contract}})
	detail, e = s.ProgramArtifactDetail(ctx, id, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	var experiments model.ExperimentContractsPayload
	if e = json.Unmarshal(detail.Payload, &experiments); e != nil {
		t.Fatal(e)
	}
	if string(experiments.Experiments[0].Acceptance[0].Value) != "9007199254740993" {
		t.Fatalf("presentation rounded experiment criterion: %s", detail.Payload)
	}
}

func TestProgramReportingNewestHistoryAndProducerStageCurrentness(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	p, e := programLoad(ctx, tx, id)
	if e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	p.Iteration = 2
	if e = programPut(ctx, tx, p); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	insertStep := func(stepID, kind, state string, iteration int) {
		t.Helper()
		step := model.ProgramStep{ID: stepID, ProgramID: id, Revision: 1, Iteration: iteration, Kind: kind, SubjectID: "", FeatureID: "feature-" + stepID, State: state, OutputManifestSHA: "output-" + stepID}
		programReportInsert(t, s, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,1,?,?)`, step, stepID, id, step.FeatureID)
	}
	insertArtifact := func(artifactID, stepID, verdict, supersedes string, iteration int) {
		t.Helper()
		payload := model.EvaluationPayload{EvaluatedManifestSHA: strings.Repeat("a", 64), Verdict: verdict}
		raw, e := json.Marshal(payload)
		if e != nil {
			t.Fatal(e)
		}
		blob, e := s.blobs.Put(ctx, raw)
		if e != nil {
			t.Fatal(e)
		}
		artifact := model.ProgramArtifact{ID: artifactID, ProgramID: id, Revision: 1, Iteration: iteration, Kind: model.ArtifactEvaluation, ContentSHA: blob.SHA, ProducerStepID: stepID, SupersedesID: supersedes, EvaluationVerdict: verdict}
		programReportInsert(t, s, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,1,?,?,?)`, artifact, artifactID, id, artifact.Kind, artifact.ContentSHA)
	}
	insertStep("old-result", model.StepResultEvaluation, "completed", 1)
	insertArtifact("zz-old-accept", "old-result", "accept", "", 1)
	insertStep("plan", model.StepPlanEvaluation, "completed", 2)
	insertArtifact("zz-plan-current", "plan", "accept", "", 2)
	insertStep("result", model.StepResultEvaluation, "completed", 2)
	insertArtifact("aa-result-new", "result", "revise", "zz-old-accept", 2)
	insertStep("contract", model.StepContractEvaluation, "completed", 2)
	insertArtifact("mm-contract-current", "contract", "accept", "", 2)
	first, e := s.ProgramArtifactPage(ctx, id, ProgramQuery{Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Items) != 1 || first.Items[0].ID != "mm-contract-current" || first.NextCursor == "" {
		t.Fatalf("not deterministic newest-first history: %+v", first)
	}
	second, e := s.ProgramArtifactPage(ctx, id, ProgramQuery{Limit: 1, After: first.NextCursor})
	if e != nil || len(second.Items) != 1 || second.Items[0].ID != "aa-result-new" {
		t.Fatalf("descending cursor skipped newer revision: %+v %v", second, e)
	}
	all, e := s.ProgramArtifactPage(ctx, id, ProgramQuery{})
	if e != nil {
		t.Fatal(e)
	}
	states := map[string]ProgramArtifactOutputState{}
	for _, state := range all.OutputStates {
		states[state.ArtifactID] = state
	}
	for artifactID, kind := range map[string]string{"zz-plan-current": model.StepPlanEvaluation, "aa-result-new": model.StepResultEvaluation, "mm-contract-current": model.StepContractEvaluation} {
		state := states[artifactID]
		if !state.CurrentStageOutput || state.ProducerStepKind != kind {
			t.Fatalf("evaluation stages incorrectly flattened by artifact kind: %+v", state)
		}
	}
	old := states["zz-old-accept"]
	if old.CurrentStageOutput || len(old.SupersededBy) != 1 || old.SupersededBy[0] != "aa-result-new" {
		t.Fatalf("superseded acceptance presented current: %+v", old)
	}
	detail, e := s.ProgramArtifactDetail(ctx, id, "zz-old-accept")
	if e != nil || detail.OutputState.CurrentStageOutput || len(detail.OutputState.SupersededBy) != 1 {
		t.Fatalf("artifact drilldown lost supersession: %+v %v", detail, e)
	}
	insertStep("result-pending", model.StepResultEvaluation, "ready", 2)
	detail, e = s.ProgramArtifactDetail(ctx, id, "aa-result-new")
	if e != nil || detail.OutputState.CurrentStageOutput {
		t.Fatalf("prior output remains current with newer pending producer: %+v %v", detail, e)
	}
	historicalSpec := model.ProgramSpec{ID: id, Project: "demo", Title: "Revised program", Objective: "New authoritative revision", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	if _, e = s.ImportProgram(ctx, historicalSpec, model.ProgramBundle{}); e != nil {
		t.Fatal(e)
	}
	detail, e = s.ProgramArtifactDetail(ctx, id, "mm-contract-current")
	if e != nil || !detail.OutputState.HistoricalRevision || detail.OutputState.CurrentStageOutput {
		t.Fatalf("superseded revision remained current: %+v %v", detail, e)
	}
}
