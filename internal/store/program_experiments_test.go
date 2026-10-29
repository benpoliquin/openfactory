package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestExperimentCanonicalIdentityBindsAttemptContractAndOrdinal(t *testing.T) {
	original := canonicalExperimentRequest(model.ExperimentRunRequest{ProgramID: "p", AttemptID: "a", Contract: model.ExperimentContract{ID: "c"}})
	for _, change := range []func(*model.ExperimentRunRequest){func(r *model.ExperimentRunRequest) { r.AttemptID = "other" }, func(r *model.ExperimentRunRequest) { r.Contract.Hypothesis = "changed" }, func(r *model.ExperimentRunRequest) { r.Ordinal = 1 }} {
		changed := original
		change(&changed)
		changed = canonicalExperimentRequest(changed)
		if changed.RunID == original.RunID || changed.RequestSHA == original.RequestSHA {
			t.Fatal("identity did not bind launch inputs")
		}
	}
	changed := original
	changed.ProgramID = "other"
	changed = canonicalExperimentRequest(changed)
	if changed.RequestSHA == original.RequestSHA {
		t.Fatal("request digest did not bind program")
	}
}

func TestExperimentReceiptRequiresDurableIntent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.RecordExperimentReceipt(ctx, "unknown", model.ExperimentReceipt{RunID: "unknown", Accepted: true, Conclusive: true}); err == nil {
		t.Fatal("accepted an unrecorded runner launch")
	}
	if err := s.ResolveProgramExperimentFailed(ctx, "unknown", "forged", "operator assertion"); err == nil {
		t.Fatal("resolved without runner evidence")
	}
}

func TestExperimentReceiptIndependentlyVerifiesCapturedObservations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	output, err := s.blobs.Put(ctx, []byte(`{"ok":false}`))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := s.blobs.Put(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := []model.ProgramBundleFile{{Path: "result.json", ContentSHA: output.SHA, Bytes: output.Bytes}}
	manifest, err := json.Marshal(struct {
		Files []model.ProgramBundleFile `json:"files"`
	}{files})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.blobs.Put(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	c := model.ExperimentContract{Commands: []model.ExperimentCommand{{Profile: "fixture"}}, Observations: []model.ExperimentObservationSpec{{Name: "ok", Type: "boolean", OutputPath: "result.json", JSONPointer: "/ok"}}, Acceptance: []model.ExperimentPredicate{{Observation: "ok", Operator: "eq", Value: json.RawMessage(`true`)}}}
	profiles := map[string]model.ExperimentProfile{"fixture": {Argv: []string{"fixture"}}}
	exit := 0
	now := time.Now()
	r := model.ExperimentReceipt{State: "completed", FinishedAt: &now, Commands: []model.ExperimentCommandReceipt{{Argv: []string{"fixture"}, BinarySHA: strings.Repeat("a", 64), ExitCode: &exit, Termination: "exited", StdoutSHA: empty.SHA, StderrSHA: empty.SHA}}, Artifacts: model.ProgramBundle{ManifestSHA: blob.SHA, Files: files}, Observations: []model.ExperimentObservation{{Name: "ok", Type: "boolean", SourcePath: "result.json", SourceSHA: output.SHA, Value: json.RawMessage(`false`)}}, Conclusive: true, Accepted: false, ExecutionOutcome: "completed", ObservationOutcome: "complete", HypothesisOutcome: "falsified", FailureKind: "hypothesis_falsified"}
	if err = s.validateExperimentEvidence(ctx, c, profiles, r); err != nil {
		t.Fatalf("complete falsification rejected: %v", err)
	}
	forged := r
	forged.Accepted = true
	forged.HypothesisOutcome = "supported"
	if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
		t.Fatal("trusted asserted acceptance")
	}
	forged = r
	forged.Observations = append([]model.ExperimentObservation(nil), r.Observations...)
	forged.Observations[0].Value = json.RawMessage(`true`)
	forged.Accepted = true
	forged.HypothesisOutcome = "supported"
	if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
		t.Fatal("trusted invented observation")
	}
	for _, kind := range []string{"timeout", "infrastructure", "output_truncated", "unknown_exit"} {
		forged = r
		forged.FailureKind = kind
		if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
			t.Fatalf("accepted conclusive infrastructure outcome %s", kind)
		}
	}
	forged = r
	forged.Commands = append([]model.ExperimentCommandReceipt(nil), r.Commands...)
	forged.Commands[0].ExitCode = nil
	if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
		t.Fatal("unknown exit was conclusive")
	}
	forged = r
	forged.State = "uncertain"
	if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
		t.Fatal("uncertain execution was conclusive")
	}
	forged = r
	forged.Commands = append([]model.ExperimentCommandReceipt(nil), r.Commands...)
	forged.Commands[0].Argv = []string{"unapproved-executable"}
	if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
		t.Fatal("accepted executable swap with identical contract argument suffix")
	}
	forged.Commands[0].Argv = []string{"fixture", "injected-argument"}
	if err = s.validateExperimentEvidence(ctx, c, profiles, forged); err == nil {
		t.Fatal("accepted argument injection outside approved profile")
	}
	nonzero := r
	nonzero.Commands = append([]model.ExperimentCommandReceipt(nil), r.Commands...)
	exitOne := 1
	nonzero.Commands[0].ExitCode = &exitOne
	nonzero.ExecutionOutcome = "nonzero"
	if err = s.validateExperimentEvidence(ctx, c, profiles, nonzero); err != nil {
		t.Fatalf("complete failed predicates with known nonzero execution rejected: %v", err)
	}
	missing := nonzero
	missing.Observations = nil
	missing.ObservationOutcome = "incomplete"
	if err = s.validateExperimentEvidence(ctx, c, profiles, missing); err == nil {
		t.Fatal("nonzero exit alone falsified hypothesis without observations")
	}
	missing.State = "failed"
	missing.Conclusive = false
	missing.HypothesisOutcome = "inconclusive"
	missing.FailureKind = "observation_incomplete"
	if err = s.validateExperimentEvidence(ctx, c, profiles, missing); err != nil {
		t.Fatalf("honest incomplete nonzero execution rejected: %v", err)
	}
	passing := c
	passing.Acceptance = []model.ExperimentPredicate{{Observation: "ok", Operator: "eq", Value: json.RawMessage(`false`)}}
	if err = s.validateExperimentEvidence(ctx, passing, profiles, nonzero); err == nil {
		t.Fatal("passing predicates plus nonzero exit falsely falsified hypothesis")
	}
	nonzero.State = "failed"
	nonzero.Conclusive = false
	nonzero.HypothesisOutcome = "inconclusive"
	nonzero.FailureKind = "nonzero_exit"
	if err = s.validateExperimentEvidence(ctx, passing, profiles, nonzero); err != nil {
		t.Fatalf("honest passing predicates with nonzero exit rejected: %v", err)
	}
	c.BundleFiles = []string{"result.json"}
	if err = s.validateExperimentEvidence(ctx, c, profiles, r); err == nil {
		t.Fatal("accepted observation from frozen input file")
	}
}

func TestExperimentPredicatesPreserveNumericPrecision(t *testing.T) {
	matched, err := experimentPredicate(json.Number("9007199254740993"), json.Number("9007199254740992"), "gt")
	if err != nil || !matched {
		t.Fatalf("lost numeric precision: %v", err)
	}
	if _, err = experimentValue([]byte(`{"x":true,"x":false}`), model.ExperimentObservationSpec{Type: "boolean", JSONPointer: "/x"}); err == nil {
		t.Fatal("accepted ambiguous output")
	}
}

func TestExperimentFailedResolutionRequiresRunnerAndExactAttestation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "experiment-owner")
	a := claim(t, s, "experiment-owner")
	// An uncertain receipt is evidence of retained ownership, not permission to
	// accept an operator's unsupported assertion that the runner has stopped.
	a.Stage = "uncertain"
	a.State = "blocked"
	if err := s.Save(ctx, a, "blocked", "runner unknown", "", true); err != nil {
		t.Fatal(err)
	}
	r := model.ExperimentReceipt{RunID: "run", RequestSHA: "request", AttemptID: a.ID, State: "uncertain"}
	save := func() {
		t.Helper()
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET data=excluded.data`, r.RunID, r.ProgramID, r.AttemptID, r.RequestSHA, data); err != nil {
			t.Fatal(err)
		}
	}
	save()
	if err := s.ResolveProgramExperimentFailed(ctx, "run", "request", "stopped"); err == nil {
		t.Fatal("resolved before runner endpoint")
	}
	r.State = "resolved_failed"
	r.ResolutionAttestation = "exact stopped attestation"
	save()
	if err := s.ResolveProgramExperimentFailed(ctx, "run", "request", "different attestation"); err == nil {
		t.Fatal("accepted mismatched runner attestation")
	}
	if err := s.ResolveProgramExperimentFailed(ctx, "run", "wrong-request", r.ResolutionAttestation); err == nil {
		t.Fatal("accepted mismatched request")
	}
	if err := s.ResolveProgramExperimentFailed(ctx, "run", "request", r.ResolutionAttestation); err == nil {
		t.Fatal("resolved receipt without durable bound intent")
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 1 || active[0].ID != a.ID {
		t.Fatalf("uncertain owner was discarded: %+v %v", active, err)
	}
	r.Accepted = true
	save()
	if err = s.ResolveProgramExperimentFailed(ctx, "run", "request", r.ResolutionAttestation); err == nil {
		t.Fatal("operator granted acceptance")
	}
}

func TestExperimentFrozenPolicyRejectsMissingAndStaleDigest(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "policy-bound-experiment")
	a := claim(t, s, "policy-bound-experiment")
	policy := model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": {Argv: []string{"python3", "-I"}}}})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, _, policyHash, _, err := loadAttempt(ctx, tx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_policies(program_id,revision,policy_hash,data) VALUES(?,?,?,?)`, "historical-program", 1, policyHash, data); err != nil {
		t.Fatal(err)
	}
	request := model.ExperimentRunRequest{ProgramID: "historical-program", Revision: 1, AttemptID: a.ID}
	for _, digest := range []string{"", strings.Repeat("0", 64)} {
		request.ExecutionPolicySHA = digest
		if _, err = experimentFrozenPolicy(ctx, tx, request); err == nil {
			t.Fatal("accepted absent or stale execution policy digest")
		}
	}
	request.ExecutionPolicySHA = model.ExperimentPolicySHA(policy.ExperimentProfiles, policy.ExperimentLimits)
	got, err := experimentFrozenPolicy(ctx, tx, request)
	if err != nil || got.ExperimentProfiles["fixture"].Argv[0] != "python3" {
		t.Fatalf("exact historical policy rejected: %+v %v", got, err)
	}
}

func TestProgramExperimentIntentReturnsOriginalImmutableRequest(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	request := canonicalExperimentRequest(model.ExperimentRunRequest{ProgramID: "p", AttemptID: "a", ExecutionPolicySHA: strings.Repeat("a", 64), Contract: model.ExperimentContract{ID: "frozen"}})
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_experiment_intents(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?)`, request.RunID, request.ProgramID, request.AttemptID, request.RequestSHA, data); err != nil {
		t.Fatal(err)
	}
	got, err := s.ProgramExperimentIntent(ctx, request.RunID)
	if err != nil || programHash(got) != programHash(request) {
		t.Fatalf("immutable recovery request changed: %+v %v", got, err)
	}
	got.Contract.ID = "caller-mutation"
	reloaded, err := s.ProgramExperimentIntent(ctx, request.RunID)
	if err != nil || reloaded.Contract.ID != "frozen" {
		t.Fatalf("caller mutated durable intent: %+v %v", reloaded, err)
	}
	request.ExecutionPolicySHA = strings.Repeat("b", 64)
	data, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE program_experiment_intents SET data=? WHERE run_id=?`, data, request.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProgramExperimentIntent(ctx, request.RunID); err == nil {
		t.Fatal("accepted stored request with invalid canonical digest")
	}
}

func TestExperimentNeverStartedCancellationIsSettlementNotConclusion(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	contract := model.ExperimentContract{Commands: []model.ExperimentCommand{{Profile: "fixture"}}, Observations: []model.ExperimentObservationSpec{{Name: "measurement", Type: "number", OutputPath: "result.json"}}}
	receipt := model.ExperimentReceipt{State: "cancelled", FinishedAt: &now, ExecutionOutcome: "cancelled", ObservationOutcome: "not_evaluated", HypothesisOutcome: "inconclusive"}
	if err := s.validateExperimentEvidence(context.Background(), contract, nil, receipt); err != nil {
		t.Fatalf("honest never-started tombstone rejected: %v", err)
	}
	receipt.Accepted = true
	if err := s.validateExperimentEvidence(context.Background(), contract, nil, receipt); err == nil {
		t.Fatal("cancellation tombstone granted acceptance")
	}
	receipt.Accepted = false
	receipt.Conclusive = true
	if err := s.validateExperimentEvidence(context.Background(), contract, nil, receipt); err == nil {
		t.Fatal("cancellation tombstone falsified hypothesis")
	}
}
