package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// programResumeSelection keeps full frozen review material inside the Store.
// Only Work is the safe public retry binding; runner receipts stay private.
type programResumeSelection struct {
	Work              *model.ProgramResumeWork
	Artifact          *model.ProgramArtifact
	ArtifactCanonical []byte
	Experiment        *model.ExperimentContract
}

// programResumeWorkTx is the read-only authority hook shared by review and commit.
// It selects at most the first blocked existing current step in the coordinator's
// fixed graph, never an arbitrary failed row or an earlier revision/iteration.
// Healthy sibling experiments do not hide a later current failed contract.
// A nil target preserves external-prerequisite/ordinary queued-work behavior.
func (s *Store) programResumeWorkTx(ctx context.Context, tx *sql.Tx, p model.Program, r model.ProgramRevision, cfg model.Config) (*programResumeSelection, error) {
	if p.ID != r.ProgramID || p.CurrentRevision != r.Number {
		return nil, programError("stale_revision", "program revision changed")
	}
	_, current, err := s.programAuthorityTx(ctx, tx, p.ID, r.Number, cfg.PolicyHash)
	if err != nil {
		return nil, err
	}
	if current.RevisionHash != r.RevisionHash {
		return nil, programError("stale_revision", "program revision changed")
	}
	if p.State != model.ProgramActive && p.State != model.ProgramWaiting {
		return nil, programError("stale_subject", "program is not resumable")
	}
	if err = programInvalidTx(ctx, tx, p.ID, r.RevisionHash); err != nil {
		return nil, err
	}
	items, err := s.programWorkSelectionTx(ctx, tx, p, cfg)
	if err != nil {
		return nil, err
	}
	var step model.ProgramStep
	var f model.Feature
	var selectedItem *model.ProgramWorkItem
	running := false
	for i := range items {
		item := &items[i]
		if item.Step == nil {
			continue
		}
		selected := *item.Step
		candidate, loadErr := programStepTx(ctx, tx, programStepIdentity(selected))
		if errors.Is(loadErr, sql.ErrNoRows) {
			// New current work is not a retry of an existing failed step.
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		if candidate.ID != programStepIdentity(programStepSpec(candidate)) || candidate.ProgramID != p.ID || candidate.Revision != r.Number || programStepIdentity(programStepSpec(candidate)) != programStepIdentity(selected) {
			return nil, programError("evidence_invalid", "current step identity changed")
		}
		if candidate.State != "ready" && candidate.State != "blocked" {
			return nil, programError("stale_subject", "current step is not a resumable failure")
		}
		feature, loadErr := get(ctx, tx, candidate.FeatureID)
		if loadErr != nil {
			return nil, loadErr
		}
		if feature.State == "blocked" || candidate.State == "blocked" {
			step, f, selectedItem = candidate, feature, item
			break
		}
		switch feature.State {
		case "ready":
		case "running":
			running = true
		default:
			return nil, programError("stale_subject", "current step is not a resumable failure")
		}
	}
	if selectedItem == nil {
		if running {
			return nil, programError("attempt_unsettled", "current step attempt is not settled")
		}
		return nil, nil
	}
	if expectedProgramArtifacts(step.Kind) == nil {
		return nil, programError("invalid_contract", "unknown current step kind")
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active!=0`, f.ID).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, programError("attempt_unsettled", "current step attempt is not settled")
	}
	if f.State != "blocked" {
		return nil, programError("stale_subject", "only a blocked current step can resume")
	}
	if f.DiscoveryStepID != step.ID || f.ID != "openfactory-discovery-"+step.ID || f.Project != p.Project || f.Kind != "research" || f.CompletionTarget != "reviewed" || f.IntakeStepID != "" || f.Revision == "" || intake.Revision(f) != f.Revision {
		return nil, programError("evidence_invalid", "current step feature identity changed")
	}
	if f.PolicyHash != cfg.PolicyHash {
		return nil, programError("policy_changed", "current step policy changed")
	}
	if err = programInvalidTx(ctx, tx, p.ID, step.ID); err != nil {
		return nil, err
	}
	if err = s.programFeatureGateTx(ctx, tx, f, cfg.PolicyHash); err != nil {
		return nil, err
	}
	manifest, manifestProgram, manifestRevision, err := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
	if err != nil {
		return nil, err
	}
	if manifestProgram != p.ID || manifestRevision != r.Number {
		return nil, programError("evidence_invalid", "current step input scope changed")
	}
	currentInputs := make(map[string]model.ProgramInputRef, len(selectedItem.Manifest.Inputs))
	for _, ref := range selectedItem.Manifest.Inputs {
		currentInputs[ref.Kind+"/"+ref.ID] = ref
	}
	for _, ref := range manifest.Inputs {
		if current, ok := currentInputs[ref.Kind+"/"+ref.ID]; !ok || current != ref {
			return nil, programError("evidence_invalid", "current step input has been superseded")
		}
	}
	root := false
	for _, ref := range manifest.Inputs {
		if ref.Kind == "program_revision" && ref.ID == r.RevisionHash && ref.SHA == r.RevisionHash && ref.Revision == r.Number {
			root = true
		}
	}
	if !root {
		return nil, programError("evidence_invalid", "current step input lacks exact revision")
	}
	var raw []byte
	var attemptID, revision, policy string
	if err = tx.QueryRowContext(ctx, `SELECT id,data,active,revision,policy_hash FROM attempts WHERE feature_id=? ORDER BY rowid DESC LIMIT 1`, f.ID).Scan(&attemptID, &raw, &active, &revision, &policy); errors.Is(err, sql.ErrNoRows) {
		return nil, programError("stale_subject", "current step has no failed attempt")
	}
	if err != nil {
		return nil, err
	}
	var attempt model.Attempt
	if err = json.Unmarshal(raw, &attempt); err != nil {
		return nil, err
	}
	if attemptID == "" || attempt.ID != attemptID || attempt.FeatureID != f.ID || revision != f.Revision || policy != f.PolicyHash || f.Attempts < 1 {
		return nil, programError("evidence_invalid", "current failed attempt identity changed")
	}
	settled := attempt.State == "blocked" && attempt.Stage == "failed"
	if step.Kind == model.StepExperimentRun && attempt.State == "failed" && attempt.Stage == "operator_reconciled_failure" {
		settled = true
	}
	if active != 0 || !settled {
		return nil, programError("attempt_unsettled", "current attempt is not a settled failure")
	}
	if cfg.MaxAttempts < 1 || f.Attempts >= cfg.MaxAttempts {
		return nil, programError("budget_exhausted", "feature attempt allowance exhausted")
	}
	var frozenRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_policies WHERE program_id=? AND revision=? AND policy_hash=?`, p.ID, r.Number, cfg.PolicyHash).Scan(&frozenRaw); errors.Is(err, sql.ErrNoRows) {
		return nil, programError("policy_changed", "current step has no frozen policy")
	}
	if err != nil {
		return nil, err
	}
	var frozen model.DiscoveryPolicy
	if err = json.Unmarshal(frozenRaw, &frozen); err != nil {
		return nil, err
	}
	if programHash(model.NormalizeDiscoveryPolicy(frozen)) != programHash(model.NormalizeDiscoveryPolicy(cfg.Discovery)) {
		return nil, programError("policy_changed", "current discovery policy differs from frozen authority")
	}
	work := &model.ProgramResumeWork{
		StepID: step.ID, StepRevision: step.Revision, StepKind: step.Kind,
		StepState: step.State, Iteration: step.Iteration, InputManifestSHA: step.InputManifestSHA,
		FeatureID: f.ID, FeatureRevision: f.Revision, FeatureState: f.State, FeatureAttempts: f.Attempts,
		FeatureMaxAttempts: cfg.MaxAttempts,
		AttemptID:          attempt.ID, AttemptState: attempt.State, AttemptStage: attempt.Stage,
		AttemptActive: false, AttemptUpdatedAt: attempt.UpdatedAt,
	}
	output := sha256.Sum256([]byte(attempt.Summary))
	work.AttemptOutputSHA = hex.EncodeToString(output[:])
	if _, ok := cfg.Projects[p.Project]; !ok {
		return nil, programError("prerequisite_missing", "discovery project is not configured")
	}
	selection := &programResumeSelection{Work: work}
	if step.Kind == model.StepExperimentRun {
		if err = s.programResumeExperimentTx(ctx, tx, step, manifest, cfg, &attempt, selection); err != nil {
			return nil, err
		}
	} else {
		if cfg.AstraModel == "" {
			return nil, programError("prerequisite_missing", "discovery project or model is not configured")
		}
		if step.Model != cfg.AstraModel || !programModelAllowed(r.Spec, step.Model) {
			return nil, programError("policy_changed", "current discovery model differs from disclosed authority")
		}
		budget, e := getProgramBudgetTx(ctx, tx, p.ID)
		if e != nil {
			return nil, e
		}
		// Match reserveProgramCallTx, including its hard lifetime ceiling. The
		// actual charge stays in ReserveCall's attempt/dispatch-key transaction.
		max := programLower(r.Spec.Limits.MaxModelCalls, model.NormalizeDiscoveryPolicy(frozen).Limits.MaxModelCalls)
		if max > 20 {
			max = 20
		}
		if budget.ModelCalls >= max {
			return nil, programError("budget_exhausted", "program lifetime model-call allowance exhausted")
		}
	}
	return selection, nil
}

// programResumeExperimentTx preserves the existing frozen contract and grants.
// Every old durable launch must have a matching terminal runner receipt; retry
// never manufactures resolution or frees an uncertain external owner.
func (s *Store) programResumeExperimentTx(ctx context.Context, tx *sql.Tx, step model.ProgramStep, manifest model.ProgramManifest, cfg model.Config, attempt *model.Attempt, selection *programResumeSelection) error {
	work := selection.Work
	var contract *model.ExperimentContract
	for _, ref := range manifest.Inputs {
		if ref.Kind != model.ArtifactExperimentContracts {
			continue
		}
		artifact, err := programArtifactTx(ctx, tx, ref.ID)
		if err != nil {
			return err
		}
		if artifact.ProgramID != step.ProgramID || artifact.Revision != step.Revision || artifact.Iteration != step.Iteration || artifact.Kind != model.ArtifactExperimentContracts || artifact.ContentSHA != ref.SHA {
			return programError("evidence_invalid", "frozen experiment artifact identity changed")
		}
		raw, err := s.blobs.Read(ctx, artifact.ContentSHA, 8<<20)
		if err != nil {
			return err
		}
		var payload model.ExperimentContractsPayload
		if err = model.StrictDecode(raw, &payload); err != nil {
			return err
		}
		for i := range payload.Experiments {
			if programHash(payload.Experiments[i]) == step.SubjectID {
				c := payload.Experiments[i]
				contract = &c
				selection.Artifact = &artifact
				selection.ArtifactCanonical = raw
				selection.Experiment = contract
			}
		}
	}
	if contract == nil {
		return programError("evidence_invalid", "exact frozen experiment contract is absent")
	}
	if _, err := model.CanonicalProgramPayload(model.ArtifactExperimentContracts, selection.ArtifactCanonical); err != nil {
		return programError("invalid_contract", "frozen experiment contract is invalid")
	}
	if _, ok := cfg.Discovery.ExperimentProfiles[contract.Environment.Profile]; !ok {
		return programError("prerequisite_missing", "experiment profile is not configured")
	}
	for _, command := range contract.Commands {
		profile := command.Profile
		if profile == "" {
			profile = contract.Environment.Profile
		}
		if _, ok := cfg.Discovery.ExperimentProfiles[profile]; !ok {
			return programError("prerequisite_missing", "experiment command profile is not configured")
		}
	}
	bundle, err := s.blobs.LoadBundle(ctx, contract.BundleManifestSHA)
	if err != nil {
		return err
	}
	declared := make(map[string]bool, len(contract.BundleFiles))
	for _, path := range contract.BundleFiles {
		if declared[path] || !strings.HasPrefix(path, "experiments/"+contract.ID+"/") {
			return programError("invalid_contract", "invalid declared experiment member")
		}
		declared[path] = true
	}
	if len(declared) != len(bundle.Files) {
		return programError("evidence_invalid", "frozen experiment bundle differs from declared files")
	}
	for _, file := range bundle.Files {
		if !declared[file.Path] {
			return programError("evidence_invalid", "undeclared frozen experiment member")
		}
		data, e := s.blobs.Read(ctx, file.ContentSHA, file.Bytes)
		if e != nil {
			return e
		}
		if int64(len(data)) != file.Bytes {
			return programError("evidence_invalid", "frozen experiment member size changed")
		}
	}
	executionPolicy := model.ExperimentPolicySHA(cfg.Discovery.ExperimentProfiles, cfg.Discovery.ExperimentLimits)
	rows, err := tx.QueryContext(ctx, `SELECT run_id,attempt_id,request_sha,data FROM program_experiment_intents WHERE program_id=? AND json_extract(data,'$.step_id')=? ORDER BY rowid`, step.ProgramID, step.ID)
	if err != nil {
		return err
	}
	type launch struct {
		runID, attemptID, requestSHA string
		request                      model.ExperimentRunRequest
	}
	var launches []launch
	for rows.Next() {
		var v launch
		var raw []byte
		if err = rows.Scan(&v.runID, &v.attemptID, &v.requestSHA, &raw); err == nil {
			err = json.Unmarshal(raw, &v.request)
		}
		if err != nil {
			rows.Close()
			return err
		}
		launches = append(launches, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	resolved := false
	for _, v := range launches {
		request := v.request
		if request.RunID != v.runID || request.AttemptID != v.attemptID || request.RequestSHA != v.requestSHA || request.ProgramID != step.ProgramID || request.StepID != step.ID || request.Revision != step.Revision || request.Ordinal != 0 || request.ContractSHA != step.SubjectID || programHash(request.Contract) != step.SubjectID || request.ExecutionPolicySHA != executionPolicy || programHash(request.Bundle) != programHash(bundle) || programHash(canonicalExperimentRequest(request)) != programHash(request) {
			return programError("evidence_invalid", "durable experiment launch identity changed")
		}
		attempt, revision, policy, active, e := loadAttempt(ctx, tx, request.AttemptID)
		if e != nil {
			return e
		}
		if attempt.FeatureID != step.FeatureID || revision != work.FeatureRevision || policy != cfg.PolicyHash || active != 0 {
			return programError("attempt_unsettled", "old experiment attempt ownership is not settled")
		}
		if _, e = experimentFrozenPolicy(ctx, tx, request); e != nil {
			return e
		}
		var raw []byte
		var runID, attemptID, requestSHA string
		e = tx.QueryRowContext(ctx, `SELECT run_id,attempt_id,request_sha,data FROM program_experiments WHERE run_id=? AND program_id=?`, request.RunID, step.ProgramID).Scan(&runID, &attemptID, &requestSHA, &raw)
		if errors.Is(e, sql.ErrNoRows) {
			return programError("attempt_unsettled", "durable experiment launch lacks terminal runner ownership")
		}
		if e != nil {
			return e
		}
		var receipt model.ExperimentReceipt
		if e = json.Unmarshal(raw, &receipt); e != nil {
			return e
		}
		if runID != request.RunID || attemptID != request.AttemptID || requestSHA != request.RequestSHA || receipt.RunID != runID || receipt.AttemptID != attemptID || receipt.RequestSHA != requestSHA || receipt.ProgramID != step.ProgramID || receipt.Revision != step.Revision || receipt.StepID != step.ID || receipt.ContractSHA != step.SubjectID || receipt.BundleManifestSHA != bundle.ManifestSHA {
			return programError("evidence_invalid", "terminal experiment receipt identity changed")
		}
		switch receipt.State {
		case "failed", "cancelled", "resolved_failed", "waiting_prerequisite":
		case "completed":
			if receipt.Conclusive {
				return programError("stale_subject", "conclusive experiment cannot be repeated by resume")
			}
		default:
			return programError("attempt_unsettled", "experiment runner ownership is not terminal")
		}
		if e = s.validateExperimentEvidence(ctx, *contract, cfg.Discovery.ExperimentProfiles, receipt); e != nil {
			return e
		}
		if receipt.AttemptID == work.AttemptID {
			work.PriorRunID = receipt.RunID
			work.PriorRequestSHA = receipt.RequestSHA
			work.PriorReceiptSHA = programHash(receipt)
			work.PriorReceiptState = receipt.State
			work.PriorReceiptConclusive = receipt.Conclusive
			work.PriorExecutionOutcome = receipt.ExecutionOutcome
			work.PriorObservationOutcome = receipt.ObservationOutcome
			work.PriorHypothesisOutcome = receipt.HypothesisOutcome
			resolved = receipt.State == "resolved_failed"
		}
	}
	if work.AttemptStage == "operator_reconciled_failure" && !resolved {
		return programError("attempt_unsettled", "operator reconciliation lacks exact resolved runner receipt")
	}
	if work.PriorRunID == "" {
		// A settled failure before any dispatch has no external runner owner.
		// Absence is proven for this exact attempt, never inferred from a
		// missing receipt for an existing launch. Older launches above must
		// independently retain their terminal evidence.
		if attempt.DispatchKey != "" || attempt.ThreadID != "" || attempt.EnvironmentID != "" {
			return programError("attempt_unsettled", "current experiment dispatch ownership is not settled")
		}
		var owned int
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM calls WHERE attempt_id=?) + (SELECT count(*) FROM program_experiment_intents WHERE attempt_id=? OR json_extract(data,'$.attempt_id')=?) + (SELECT count(*) FROM program_experiments WHERE attempt_id=? OR json_extract(data,'$.attempt_id')=?)`, attempt.ID, attempt.ID, attempt.ID, attempt.ID, attempt.ID).Scan(&owned); err != nil {
			return err
		}
		if owned != 0 {
			return programError("attempt_unsettled", "current experiment attempt lacks terminal runner ownership")
		}
		work.PriorLaunchAbsent = true
	}
	work.SubjectID = step.SubjectID
	work.ExecutionPolicySHA = executionPolicy
	work.BundleManifestSHA = bundle.ManifestSHA
	return nil
}
