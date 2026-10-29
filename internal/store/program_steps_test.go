package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func programStepsFixture(t *testing.T) (*Store, model.Config, model.ProgramRevision) {
	t.Helper()
	s := openTest(t)
	cfg := model.Config{PolicyHash: "policy", AstraModel: "provider/astra", FastModel: "other/fast", Projects: map[string]model.Project{"demo": {}}}
	r, err := s.ImportProgram(context.Background(), model.ProgramSpec{ID: "steps-program", Project: "demo", Title: "Discover behavior", Objective: "Determine behavior", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(context.Background(), r.ProgramID, r.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	return s, cfg, r
}
func firstProgramStep(t *testing.T, s *Store, cfg model.Config) model.ProgramStep {
	t.Helper()
	items, err := s.ProgramWork(context.Background(), cfg)
	if err != nil || len(items) != 1 || items[0].Step == nil {
		t.Fatalf("work=%+v error=%v", items, err)
	}
	step, err := s.EnsureProgramStep(context.Background(), *items[0].Step, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return step
}
func researchStepResult() model.ProgramStepResult {
	return model.ProgramStepResult{Outcome: "completed", Artifacts: []model.ProgramArtifactDraft{{Kind: model.ArtifactRequirements, Payload: json.RawMessage(`{"requirements":[{"id":"behavior","behavior":"Determine ordering","rationale":"Ordering affects correctness","constraints":[],"acceptance":[{"id":"ordering","scenario":"Run duplicate arrivals","expected":"Ordering is documented"}],"priority":"must","claim_refs":[],"question_refs":[]}]}`)}, {Kind: model.ArtifactQuestions, Payload: json.RawMessage(`{"questions":[]}`)}, {Kind: model.ArtifactClaims, Payload: json.RawMessage(`{"claims":[]}`)}}}
}
func programAttemptFixture(t *testing.T, s *Store, step model.ProgramStep) model.Attempt {
	t.Helper()
	a := model.Attempt{ID: "attempt-" + step.ID, FeatureID: step.FeatureID, ThreadID: "author-thread", State: "running", Stage: "poll"}
	f, err := s.Get(context.Background(), step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	if _, err = s.db.ExecContext(context.Background(), `INSERT INTO attempts(id,feature_id,active,revision,policy_hash,data) VALUES(?,?,1,?,?,?)`, a.ID, a.FeatureID, f.Revision, f.PolicyHash, raw); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestProgramStepIdentityAndBudgetDedup(t *testing.T) {
	s, cfg, _ := programStepsFixture(t)
	ctx := context.Background()
	step := firstProgramStep(t, s, cfg)
	again, err := s.EnsureProgramStep(ctx, programStepSpec(step), cfg)
	if err != nil || again.ID != step.ID {
		t.Fatalf("identity changed: %+v %v", again, err)
	}
	if step.Model != cfg.AstraModel || !strings.HasPrefix(step.FeatureID, "openfactory-discovery-") {
		t.Fatalf("unexpected server identity: %+v", step)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := getProgramBudgetTx(ctx, tx, step.ProgramID)
	tx.Rollback()
	if err != nil || budget.Steps != 1 || budget.Experiments != 0 || budget.ModelCalls != 0 {
		t.Fatalf("duplicate consumed budget: %+v %v", budget, err)
	}
	inputs, err := s.ProgramStepInputs(ctx, step.ID)
	if err != nil || inputs.Manifest.SHA != step.InputManifestSHA || inputs.Step == nil {
		t.Fatalf("immutable inputs missing: %+v %v", inputs, err)
	}
	f, err := s.Get(ctx, step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if err = intake.Validate(f); err != nil {
		t.Fatalf("server-generated step cannot pass normal admission: %v", err)
	}
	if len(f.Checks) != 0 || len(f.Acceptance) != 1 || !strings.Contains(f.Acceptance[0].Scenario, step.InputManifestSHA) {
		t.Fatalf("step must declare its real typed-output acceptance without fabricated checks: %+v", f)
	}
}
func TestProgramStepRejectsForgedWorkAndUndisclosedModel(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	ctx := context.Background()
	v := model.ProgramStepSpec{ProgramID: r.ProgramID, Revision: r.Number, Kind: model.StepContracts, InputManifestSHA: strings.Repeat("a", 64)}
	if _, err := s.EnsureProgramStep(ctx, v, cfg); err == nil {
		t.Fatal("accepted graph bypass")
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AstraModel = "undisclosed/model"
	if _, err = s.EnsureProgramStep(ctx, *items[0].Step, cfg); err == nil {
		t.Fatal("accepted undisclosed provider")
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_steps`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejection created work: %d %v", n, err)
	}
}
func TestProgramCompletionAtomicAndIdempotent(t *testing.T) {
	s, cfg, _ := programStepsFixture(t)
	ctx := context.Background()
	step := firstProgramStep(t, s, cfg)
	a := programAttemptFixture(t, s, step)
	bad := researchStepResult()
	bad.Artifacts = bad.Artifacts[:2]
	if err := s.CompleteProgramStep(ctx, a, bad); err == nil {
		t.Fatal("accepted missing artifact")
	}
	var n, active int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial artifacts leaked: %d %v", n, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT active FROM attempts WHERE id=?`, a.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("rejected completion settled attempt: %d %v", active, err)
	}
	result := researchStepResult()
	if err := s.CompleteProgramStep(ctx, a, result); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteProgramStep(ctx, a, result); err != nil {
		t.Fatalf("identical completion not idempotent: %v", err)
	}
	changed := researchStepResult()
	changed.Artifacts[2].Payload = json.RawMessage(`{"claims":[{"id":"unknown","statement":"Unmeasured","classification":"unknown","citations":[],"inference_premises":[],"scope":[],"limitations":[]}]}`)
	if err := s.CompleteProgramStep(ctx, a, changed); err == nil {
		t.Fatal("accepted conflicting completion")
	}
	f, err := s.Get(ctx, step.FeatureID)
	if err != nil || f.State != "reviewed" {
		t.Fatalf("feature not reviewed: %+v %v", f, err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT active FROM attempts WHERE id=?`, a.ID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("completion did not settle attempt: %d %v", active, err)
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil || len(items) != 1 || items[0].Step == nil || items[0].Step.Kind != model.StepArchitecture {
		t.Fatalf("graph did not advance: %+v %v", items, err)
	}
}
func TestProgramRevisionDoesNotResetStepBudget(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	ctx := context.Background()
	firstProgramStep(t, s, cfg)
	spec := r.Spec
	spec.Title = "Revised objective"
	spec.Limits.MaxSteps = 1
	r, err := s.ImportProgram(ctx, spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(ctx, r.ProgramID, r.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil || len(items) != 1 || items[0].Step == nil {
		t.Fatalf("missing revised work: %+v %v", items, err)
	}
	if _, err = s.EnsureProgramStep(ctx, *items[0].Step, cfg); err == nil {
		t.Fatal("revision reset lifetime budget")
	}
}
func TestProgramExperimentStepUsesExperimentsPhase(t *testing.T) {
	if got := programPhaseForStep(model.StepExperimentRun); got != "experiments" {
		t.Fatalf("experiment phase=%q", got)
	}
	for _, kind := range []string{model.StepResearch, model.StepArchitecture, model.StepPlanEvaluation, model.StepResultEvaluation, model.StepContracts, model.StepContractEvaluation} {
		if got := programPhaseForStep(kind); got != kind {
			t.Fatalf("phase for %q=%q", kind, got)
		}
	}
}
func TestProgramOutputManifestRetainsInputsAndCapturedSources(t *testing.T) {
	s, cfg, _ := programStepsFixture(t)
	ctx := context.Background()
	step := firstProgramStep(t, s, cfg)
	a := programAttemptFixture(t, s, step)
	blob, err := s.blobs.Put(ctx, []byte("captured public text"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(time.Hour)
	source := model.SourceCapture{ID: "captured-source", ProgramID: step.ProgramID, Revision: step.Revision, TextSHA256: blob.SHA, ContentSHA256: blob.SHA, Valid: true, FreshUntil: &fresh}
	raw, _ := json.Marshal(source)
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, source.ID, source.ProgramID, source.Revision, raw); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteProgramStep(ctx, a, researchStepResult()); err != nil {
		t.Fatal(err)
	}
	completed, err := s.ProgramStepForFeature(ctx, step.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	input, _, _, err := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	output, _, _, err := programManifestLoadTx(ctx, tx, completed.OutputManifestSHA)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, ref := range output.Inputs {
		key := ref.Kind + "/" + ref.ID
		if _, exists := found[key]; exists {
			t.Fatalf("duplicate output input: %+v", ref)
		}
		found[key] = ref.SHA
	}
	for _, ref := range input.Inputs {
		if found[ref.Kind+"/"+ref.ID] != ref.SHA {
			t.Fatalf("output dropped immutable input: %+v", ref)
		}
	}
	if found["source/"+source.ID] != source.TextSHA256 {
		t.Fatal("output omitted captured source")
	}
	if len(output.Inputs) != len(input.Inputs)+4 {
		t.Fatalf("output inputs=%d, want prior inputs plus three artifacts and source", len(output.Inputs))
	}
	source.ID = "later-capture"
	raw, _ = json.Marshal(source)
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, source.ID, source.ProgramID, source.Revision, raw); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteProgramStep(ctx, a, researchStepResult()); err != nil {
		t.Fatalf("later capture changed completed step identity: %v", err)
	}
}
func TestProgramWorkSelectsFreshUnsupersededEvidence(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	for _, source := range []model.SourceCapture{{ID: "fresh", ProgramID: r.ProgramID, Revision: r.Number, TextSHA256: strings.Repeat("a", 64), Valid: true, FreshUntil: &future}, {ID: "expired", ProgramID: r.ProgramID, Revision: r.Number, TextSHA256: strings.Repeat("b", 64), Valid: true, FreshUntil: &past}, {ID: "rejected", ProgramID: r.ProgramID, Revision: r.Number, TextSHA256: strings.Repeat("c", 64), Valid: false, FreshUntil: &future}} {
		raw, _ := json.Marshal(source)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, source.ID, source.ProgramID, source.Revision, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, artifact := range []model.ProgramArtifact{{ID: "old-claims", ProgramID: r.ProgramID, Revision: r.Number, Kind: model.ArtifactClaims, ContentSHA: strings.Repeat("d", 64)}, {ID: "new-claims", ProgramID: r.ProgramID, Revision: r.Number, Kind: model.ArtifactClaims, ContentSHA: strings.Repeat("e", 64), SupersedesID: "old-claims"}} {
		raw, _ := json.Marshal(artifact)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, artifact.ID, artifact.ProgramID, artifact.Revision, artifact.Kind, artifact.ContentSHA, raw); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil || len(items) != 1 || items[0].Wait != nil {
		t.Fatalf("fresh evidence cannot proceed: %+v %v", items, err)
	}
	ids := map[string]bool{}
	for _, ref := range items[0].Manifest.Inputs {
		ids[ref.ID] = true
	}
	if !ids["fresh"] || !ids["new-claims"] || ids["expired"] || ids["rejected"] || ids["old-claims"] {
		t.Fatalf("incorrect evidence selection: %+v", items[0].Manifest.Inputs)
	}
}

func TestProgramDomainWaitDoesNotBlockOtherWork(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	ctx := context.Background()
	research := firstProgramStep(t, s, cfg)
	if err := s.CompleteProgramStep(ctx, programAttemptFixture(t, s, research), researchStepResult()); err != nil {
		t.Fatal(err)
	}
	// A completed evaluator whose immutable evidence is unavailable is a local
	// domain failure, not a reason to stop unrelated OpenFactory admission.
	for _, kind := range []string{model.StepArchitecture, model.StepPlanEvaluation} {
		step := model.ProgramStep{ID: "missing-output-" + kind, ProgramID: r.ProgramID, Revision: r.Number, Kind: kind, State: "completed", FeatureID: "missing-feature-" + kind}
		raw, _ := json.Marshal(step)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, step.ProgramID, step.Revision, step.FeatureID, raw); err != nil {
			t.Fatal(err)
		}
	}
	expired := time.Now().Add(-time.Hour)
	source := model.SourceCapture{ID: "expired-evaluator-input", ProgramID: r.ProgramID, Revision: r.Number, TextSHA256: strings.Repeat("a", 64), Valid: true, FreshUntil: &expired}
	sourceBytes, _ := json.Marshal(source)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, source.ID, r.ProgramID, r.Number, sourceBytes); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	expiredManifest, err := programManifestTx(ctx, tx, r.ProgramID, r.Number, []model.ProgramInputRef{{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: r.Number}})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	evaluation := model.ProgramArtifact{ID: "retained-expired-evaluation", ProgramID: r.ProgramID, Revision: r.Number, Kind: model.ArtifactEvaluation, ContentSHA: strings.Repeat("b", 64), InputManifestSHA: expiredManifest.SHA, ProducerStepID: "missing-output-" + model.StepPlanEvaluation}
	evaluationBytes, _ := json.Marshal(evaluation)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, evaluation.ID, r.ProgramID, r.Number, evaluation.Kind, evaluation.ContentSHA, evaluationBytes); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	spec := r.Spec
	spec.ID = "healthy-program"
	healthy, err := s.ImportProgram(ctx, spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(ctx, healthy.ProgramID, healthy.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	ordinary := approved(t, s, "unrelated-product")
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil {
		t.Fatalf("program domain error escaped coordinator: %v", err)
	}
	seenBad, seenHealthy := false, false
	for _, item := range items {
		if item.Program.ID == r.ProgramID {
			seenBad = item.Wait != nil && item.Wait.Code == "evidence_invalid" && item.Step == nil
		}
		if item.Program.ID == healthy.ProgramID {
			seenHealthy = item.Wait == nil && item.Step != nil && item.Step.Kind == model.StepResearch
		}
	}
	if !seenBad || !seenHealthy {
		t.Fatalf("domain failure was not isolated: %+v", items)
	}
	var retained int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts WHERE id=?`, evaluation.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("wait removed accepted evidence history: %d %v", retained, err)
	}
	if _, err = s.Claim(ctx, ordinary.ID, "policy", 3, 5, 5, 300); err != nil {
		t.Fatalf("ordinary admission blocked by program wait: %v", err)
	}
	var eventBytes []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_events WHERE program_id=? AND kind='waiting' ORDER BY seq DESC LIMIT 1`, r.ProgramID).Scan(&eventBytes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(eventBytes), r.RevisionHash) {
		t.Fatalf("wait event lacks exact revision binding: %s", eventBytes)
	}
}

func TestRecordProgramWaitRejectsStaleRevisionAndPersistsBudgetGate(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	ctx := context.Background()
	firstProgramStep(t, s, cfg)
	spec := r.Spec
	spec.Title = "New revision"
	spec.Limits.MaxSteps = 1
	next, err := s.ImportProgram(ctx, spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordProgramWait(ctx, r.ProgramID, r.RevisionHash, "evidence_invalid", "old result"); err == nil {
		t.Fatal("stale wait overwrote new revision")
	}
	if err = s.ApproveProgram(ctx, next.ProgramID, next.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil || len(items) != 1 || items[0].Step == nil {
		t.Fatalf("work: %+v %v", items, err)
	}
	_, err = s.EnsureProgramStep(ctx, *items[0].Step, cfg)
	pe, ok := err.(*model.ProgramError)
	if !ok || pe.Code != "budget_exhausted" {
		t.Fatalf("expected lifetime budget gate: %v", err)
	}
	if err = s.RecordProgramWait(ctx, next.ProgramID, next.RevisionHash, pe.Code, pe.Message); err != nil {
		t.Fatal(err)
	}
	p, err := programLoad(ctx, s.db, next.ProgramID)
	if err != nil || p.Wait == nil || p.Wait.Code != "budget_exhausted" || p.State != model.ProgramWaiting {
		t.Fatalf("budget wait not persisted: %+v %v", p, err)
	}
}

func TestProgramWorkDoesNotSuppressCorruptPersistence(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	if _, err := s.db.ExecContext(context.Background(), `UPDATE programs SET data=? WHERE id=?`, []byte("{invalid"), r.ProgramID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProgramWork(context.Background(), cfg); err == nil {
		t.Fatal("corrupt persistence became an ordinary wait")
	}
}

func TestResultEvaluationCannotAcceptFalsifiedExperiment(t *testing.T) {
	for _, conclusive := range []bool{true, false} {
		t.Run(map[bool]string{true: "falsified", false: "infrastructure-inconclusive"}[conclusive], func(t *testing.T) {
			s, cfg, r := programStepsFixture(t)
			ctx := context.Background()
			step := firstProgramStep(t, s, cfg)
			a := programAttemptFixture(t, s, step)
			receipt := model.ExperimentReceipt{RunID: "measured-run", RequestSHA: strings.Repeat("a", 64), ProgramID: step.ProgramID, Revision: step.Revision, StepID: "experiment-step", AttemptID: "runner-attempt", ContractSHA: strings.Repeat("b", 64), BundleManifestSHA: strings.Repeat("c", 64), State: "completed", Conclusive: conclusive, Accepted: false, Commands: []model.ExperimentCommandReceipt{{}}}
			raw, _ := json.Marshal(receipt)
			if _, err := s.db.ExecContext(ctx, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?)`, receipt.RunID, receipt.ProgramID, receipt.AttemptID, receipt.RequestSHA, raw); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			experiment := model.ProgramStep{ID: receipt.StepID, ProgramID: r.ProgramID, Revision: r.Number, Kind: model.StepExperimentRun, SubjectID: receipt.ContractSHA, FeatureID: "openfactory-discovery-" + receipt.StepID, State: "completed"}
			experimentBytes, _ := json.Marshal(experiment)
			if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, experiment.ID, r.ProgramID, r.Number, experiment.FeatureID, experimentBytes); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,?,?,'{}')`, r.ProgramID, r.RevisionHash, receipt.ContractSHA, cfg.PolicyHash); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			manifest, err := programManifestTx(ctx, tx, step.ProgramID, step.Revision, []model.ProgramInputRef{{Kind: "experiment_receipt", ID: receipt.RunID, SHA: programHash(receipt), Revision: step.Revision}})
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			step.Kind = model.StepResultEvaluation
			step.InputManifestSHA = manifest.SHA
			raw, _ = json.Marshal(step)
			if _, err = tx.ExecContext(ctx, `UPDATE program_steps SET data=? WHERE id=?`, raw, step.ID); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			evaluation := model.EvaluationPayload{EvaluatedManifestSHA: manifest.SHA, Verdict: "accept"}
			payload, _ := json.Marshal(evaluation)
			result := model.ProgramStepResult{Outcome: "completed", Artifacts: []model.ProgramArtifactDraft{{Kind: model.ArtifactEvaluation, Payload: payload}}}
			if err = s.CompleteProgramStep(ctx, a, result); err == nil {
				t.Fatal("evaluator forged acceptance of failed measurements")
			}
			var active, artifacts int
			if err = s.db.QueryRowContext(ctx, `SELECT active FROM attempts WHERE id=?`, a.ID).Scan(&active); err != nil || active != 1 {
				t.Fatalf("rejected evaluation settled attempt: %d %v", active, err)
			}
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts`).Scan(&artifacts); err != nil || artifacts != 0 {
				t.Fatalf("rejected evaluation committed artifacts: %d %v", artifacts, err)
			}
			evaluation.Verdict = "revise"
			evaluation.RequiredChanges = []string{"Revise ordering decision to address observed failure"}
			payload, _ = json.Marshal(evaluation)
			result.Artifacts[0].Payload = payload
			err = s.CompleteProgramStep(ctx, a, result)
			if conclusive && err != nil {
				t.Fatalf("falsification must permit bounded revision: %v", err)
			}
			if !conclusive && err == nil {
				t.Fatal("infrastructure-inconclusive receipt was treated as decisive experiment evidence")
			}
		})
	}
}

func TestRevisedIterationSelectsCorrectedReceiptsWithoutDeletingFalsification(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
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
	for iteration := 0; iteration <= 1; iteration++ {
		id := []string{"falsified-step", "corrected-step"}[iteration]
		step := model.ProgramStep{ID: id, ProgramID: r.ProgramID, Revision: r.Number, Iteration: iteration, Kind: model.StepExperimentRun, SubjectID: strings.Repeat([]string{"b", "c"}[iteration], 64), FeatureID: "openfactory-discovery-" + id, State: "completed"}
		raw, _ := json.Marshal(step)
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, id, r.ProgramID, r.Number, step.FeatureID, raw); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		receipt := model.ExperimentReceipt{RunID: id + "-run", ProgramID: r.ProgramID, Revision: r.Number, StepID: id, AttemptID: id + "-attempt", ContractSHA: step.SubjectID, RequestSHA: strings.Repeat("a", 64), State: "completed", Conclusive: true, Accepted: iteration == 1}
		raw, _ = json.Marshal(receipt)
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?)`, receipt.RunID, r.ProgramID, receipt.AttemptID, receipt.RequestSHA, raw); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,?,?,'{}')`, r.ProgramID, r.RevisionHash, step.SubjectID, cfg.PolicyHash); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil || len(items) != 1 {
		t.Fatalf("revised work: %+v %v", items, err)
	}
	seenPass, seenFailure := false, false
	var failureRef model.ProgramInputRef
	for _, ref := range items[0].Manifest.Inputs {
		if ref.ID == "falsified-step-run" {
			seenFailure = true
			failureRef = ref
		}
		if ref.ID == "corrected-step-run" {
			seenPass = true
		}
	}
	if !seenPass || !seenFailure {
		t.Fatal("revision context must retain both falsification and corrected measured result")
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	revision := model.RevisionPayload{Trigger: "Observed ordering falsification", FalsifiedEvidence: []model.ProgramInputRef{failureRef}, ChangedDecisions: []string{"ordering"}, Materiality: "non_material", PredecessorManifestSHA: items[0].Manifest.SHA}
	revisionBytes, _ := json.Marshal(revision)
	architecture := model.ProgramStep{ProgramID: r.ProgramID, Revision: r.Number, Iteration: 1, Kind: model.StepArchitecture, InputManifestSHA: items[0].Manifest.SHA}
	if err = s.validateProgramPayloadTx(ctx, tx, architecture, model.ArtifactRevision, revisionBytes); err != nil {
		tx.Rollback()
		t.Fatalf("revised architecture cannot cite actual prior failed receipt: %v", err)
	}
	step := model.ProgramStep{ProgramID: r.ProgramID, Revision: r.Number, Iteration: 1, Kind: model.StepResultEvaluation, InputManifestSHA: items[0].Manifest.SHA}
	err = validateProgramResultVerdictTx(ctx, tx, step, model.EvaluationPayload{EvaluatedManifestSHA: step.InputManifestSHA, Verdict: "accept"}, cfg.PolicyHash)
	tx.Rollback()
	if err != nil {
		t.Fatalf("corrected conclusive pass cannot be independently accepted: %v", err)
	}
	var historical int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_experiments WHERE program_id=?`, r.ProgramID).Scan(&historical); err != nil || historical != 2 {
		t.Fatalf("replanning removed receipt history: %d %v", historical, err)
	}
}
