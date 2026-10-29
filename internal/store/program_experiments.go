package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"openfactory/internal/model"
)

const programExperimentSchema = `CREATE TABLE IF NOT EXISTS program_experiment_intents(run_id TEXT PRIMARY KEY,program_id TEXT NOT NULL,attempt_id TEXT NOT NULL,request_sha TEXT NOT NULL,data BLOB NOT NULL);`

func canonicalExperimentRequest(r model.ExperimentRunRequest) model.ExperimentRunRequest {
	r.ContractSHA = programHash(r.Contract)
	r.RunID = programHash([]string{r.AttemptID, r.ContractSHA, strconv.Itoa(r.Ordinal)})
	r.RequestSHA = ""
	r.RequestSHA = programHash(r)
	return r
}

// experimentBinding reconstructs authority from immutable controller records, not
// receipt assertions. Receipt recording remains possible after cancellation so
// that an outstanding external operation never loses its owner.
func (s *Store) experimentBinding(ctx context.Context, tx *sql.Tx, r model.ExperimentRunRequest, launch bool) (model.Attempt, model.ProgramStep, error) {
	a, rev, policy, active, err := loadAttempt(ctx, tx, r.AttemptID)
	var step model.ProgramStep
	if err != nil {
		return a, step, err
	}
	if active != 1 {
		return a, step, programError("operation_uncertain", "experiment attempt is not active")
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return a, step, err
	}
	if launch && (f.Revision != rev || f.PolicyHash != policy) {
		return a, step, programError("stale_revision", "attempt authority changed")
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE id=? AND feature_id=?`, r.StepID, f.ID).Scan(&raw); err != nil {
		return a, step, err
	}
	if err = json.Unmarshal(raw, &step); err != nil {
		return a, step, err
	}
	if launch && f.DiscoveryStepID != step.ID {
		return a, step, programError("invalid_contract", "feature step association changed")
	}
	if step.Kind != model.StepExperimentRun || step.ID != r.StepID || step.ProgramID != r.ProgramID || step.Revision != r.Revision || step.SubjectID != r.ContractSHA || r.Ordinal != 0 {
		return a, step, programError("invalid_contract", "experiment step identity mismatch")
	}
	if _, err = experimentFrozenPolicy(ctx, tx, r); err != nil {
		return a, step, err
	}
	p, err := programLoad(ctx, tx, r.ProgramID)
	if err != nil {
		return a, step, err
	}
	if launch && p.CurrentRevision != r.Revision {
		return a, step, programError("stale_revision", "experiment revision changed")
	}
	if launch {
		if !time.Now().Before(a.Deadline) {
			return a, step, programError("operation_uncertain", "experiment deadline elapsed")
		}
		if f.State == "cancelling" || f.State == "cancelled" {
			return a, step, programError("approval_required", "experiment cancelled")
		}
		if _, _, err = s.programAuthorityTx(ctx, tx, p.ID, r.Revision, policy); err != nil {
			return a, step, err
		}
		if err = s.programFeatureGateTx(ctx, tx, f, policy); err != nil {
			return a, step, err
		}
	}
	revision, err := programRevision(ctx, tx, p.ID, r.Revision)
	if err != nil {
		return a, step, err
	}
	if launch {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=? AND subject_hash=? AND policy_hash=?`, p.ID, revision.RevisionHash, r.ContractSHA, policy).Scan(&n); err != nil {
			return a, step, err
		}
		if n != 1 {
			return a, step, programError("approval_required", "exact experiment approval required")
		}
	}
	if launch {
		if err = programInvalidTx(ctx, tx, p.ID, r.ContractSHA); err != nil {
			return a, step, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_manifests WHERE sha=? AND program_id=? AND revision=?`, step.InputManifestSHA, p.ID, r.Revision).Scan(&raw); err != nil {
		return a, step, err
	}
	var manifest model.ProgramManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return a, step, err
	}
	if manifest.SHA != step.InputManifestSHA || programHash(manifest.Inputs) != manifest.SHA {
		return a, step, programError("evidence_invalid", "invalid input manifest")
	}
	found := false
	for _, ref := range manifest.Inputs {
		var artifact model.ProgramArtifact
		var data []byte
		e := tx.QueryRowContext(ctx, `SELECT data FROM program_artifacts WHERE id=? AND program_id=? AND revision=?`, ref.ID, p.ID, r.Revision).Scan(&data)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return a, step, e
		}
		if e = json.Unmarshal(data, &artifact); e != nil {
			return a, step, e
		}
		if artifact.Kind != model.ArtifactExperimentContracts || artifact.ContentSHA != ref.SHA {
			continue
		}
		data, e = s.blobs.Read(ctx, artifact.ContentSHA, 8<<20)
		if e != nil {
			return a, step, e
		}
		var payload model.ExperimentContractsPayload
		if e = json.Unmarshal(data, &payload); e != nil {
			return a, step, e
		}
		for _, c := range payload.Experiments {
			if programHash(c) == r.ContractSHA && programHash(c) == programHash(r.Contract) {
				found = true
			}
		}
	}
	if !found {
		return a, step, programError("evidence_invalid", "contract absent from exact input manifest")
	}
	bundle, err := s.blobs.LoadBundle(ctx, r.Contract.BundleManifestSHA)
	if err != nil {
		return a, step, err
	}
	if programHash(bundle) != programHash(r.Bundle) {
		return a, step, programError("evidence_invalid", "frozen bundle mismatch")
	}
	for _, file := range bundle.Files {
		b, e := s.blobs.Read(ctx, file.ContentSHA, file.Bytes)
		if e != nil {
			return a, step, e
		}
		if int64(len(b)) != file.Bytes {
			return a, step, programError("evidence_invalid", "bundle member size mismatch")
		}
	}
	declared := make(map[string]bool, len(r.Contract.BundleFiles))
	for _, path := range r.Contract.BundleFiles {
		if declared[path] || !strings.HasPrefix(path, "experiments/"+r.Contract.ID+"/") {
			return a, step, programError("invalid_contract", "invalid declared experiment member")
		}
		declared[path] = true
	}
	if len(declared) != len(bundle.Files) {
		return a, step, programError("evidence_invalid", "bundle differs from exact declared files")
	}
	for _, file := range bundle.Files {
		if !declared[file.Path] {
			return a, step, programError("evidence_invalid", "undeclared experiment bundle member")
		}
	}
	if programHash(canonicalExperimentRequest(r)) != programHash(r) {
		return a, step, programError("invalid_contract", "noncanonical experiment identity")
	}
	return a, step, nil
}

func (s *Store) RecordExperimentIntent(ctx context.Context, r model.ExperimentRunRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, err = s.experimentBinding(ctx, tx, r, true); err != nil {
		return err
	}
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiment_intents WHERE run_id=?`, r.RunID).Scan(&previous)
	data, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if err == nil {
		if !bytes.Equal(previous, data) {
			return programError("idempotency_conflict", "experiment intent changed")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO program_experiment_intents(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?)`, r.RunID, r.ProgramID, r.AttemptID, r.RequestSHA, data)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordExperimentReceipt(ctx context.Context, runID string, r model.ExperimentReceipt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var data []byte
	var request model.ExperimentRunRequest
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiment_intents WHERE run_id=?`, runID).Scan(&data); err != nil {
		return programError("operation_uncertain", "no durable experiment intent")
	}
	if err = json.Unmarshal(data, &request); err != nil {
		return err
	}
	if r.RunID != runID || r.RequestSHA != request.RequestSHA || r.AttemptID != request.AttemptID || r.ProgramID != request.ProgramID || r.Revision != request.Revision || r.StepID != request.StepID || r.ContractSHA != request.ContractSHA || r.BundleManifestSHA != request.Bundle.ManifestSHA {
		return programError("invalid_contract", "receipt identity mismatch")
	}
	if _, _, err = s.experimentBinding(ctx, tx, request, false); err != nil {
		return err
	}
	frozenPolicy, err := experimentFrozenPolicy(ctx, tx, request)
	if err != nil {
		return err
	}
	if err = s.validateExperimentEvidence(ctx, request.Contract, frozenPolicy.ExperimentProfiles, r); err != nil {
		return err
	}
	var previous model.ExperimentReceipt
	err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE run_id=?`, runID).Scan(&data)
	if err == nil {
		if err = json.Unmarshal(data, &previous); err != nil {
			return err
		}
		if previous.State == "completed" || previous.State == "failed" || previous.State == "cancelled" || previous.State == "resolved_failed" {
			if programHash(previous) != programHash(r) {
				return programError("idempotency_conflict", "terminal receipt changed")
			}
		}
		if previous.State == "uncertain" && r.State != "uncertain" && r.State != "resolved_failed" {
			return programError("operation_uncertain", "uncertain run requires runner resolution")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	data, err = json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO program_experiments(run_id,program_id,attempt_id,request_sha,data) VALUES(?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET data=excluded.data`, runID, r.ProgramID, r.AttemptID, r.RequestSHA, data)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResolveProgramExperimentFailed(ctx context.Context, runID, requestSHA, attestation string) error {
	if strings.TrimSpace(attestation) == "" || len(attestation) > 4096 {
		return programError("invalid_contract", "operator attestation required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var data []byte
	var r model.ExperimentReceipt
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE run_id=? AND request_sha=?`, runID, requestSHA).Scan(&data); err != nil {
		return err
	}
	if err = json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.RunID != runID || r.RequestSHA != requestSHA || r.State != "resolved_failed" || r.ResolutionAttestation != attestation || r.Accepted || r.Conclusive {
		return programError("operation_uncertain", "exact runner-resolved failed receipt required")
	}
	var request model.ExperimentRunRequest
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiment_intents WHERE run_id=? AND request_sha=?`, runID, requestSHA).Scan(&data); err != nil {
		return err
	}
	if err = json.Unmarshal(data, &request); err != nil {
		return err
	}
	if request.AttemptID != r.AttemptID || request.ProgramID != r.ProgramID || request.StepID != r.StepID || request.ContractSHA != r.ContractSHA {
		return programError("invalid_contract", "resolved receipt identity differs from intent")
	}
	if _, _, err = s.experimentBinding(ctx, tx, request, false); err != nil {
		return err
	}
	a, _, _, active, err := loadAttempt(ctx, tx, r.AttemptID)
	if err != nil {
		return err
	}
	if active != 1 {
		return programError("operation_uncertain", "attempt is not active")
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	if a.Stage != "uncertain" && a.State != "blocked" && f.State != "cancelling" {
		return programError("operation_uncertain", "attempt is not awaiting resolution")
	}
	a.State = "failed"
	a.Stage = "operator_reconciled_failure"
	a.Wait = nil
	a.UpdatedAt = time.Now().UTC()
	a.StageStartedAt = a.UpdatedAt
	data, err = json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET active=0,data=? WHERE id=? AND active=1`, data, a.ID); err != nil {
		return err
	}
	cancelled := f.State == "cancelling" || f.State == "cancelled"
	f.State = "blocked"
	if cancelled {
		f.State = "cancelled"
	}
	f.Reason = "operator resolved experiment failed; separate retry required"
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	if err = programEvent(ctx, tx, r.ProgramID, "experiment_resolved_failed", fmt.Sprintf("%s:%s", runID, requestSHA)); err != nil {
		return err
	}
	return tx.Commit()
}

func experimentValue(data []byte, spec model.ExperimentObservationSpec) (any, error) {
	var value any
	if err := model.StrictDecode(data, &value); err != nil {
		return nil, err
	}
	if spec.JSONPointer != "" {
		if !strings.HasPrefix(spec.JSONPointer, "/") {
			return nil, errors.New("invalid observation pointer")
		}
		for _, part := range strings.Split(spec.JSONPointer[1:], "/") {
			for i := 0; i < len(part); i++ {
				if part[i] == '~' {
					if i+1 >= len(part) || (part[i+1] != '0' && part[i+1] != '1') {
						return nil, errors.New("invalid pointer escape")
					}
					i++
				}
			}
			key := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			switch object := value.(type) {
			case map[string]any:
				var ok bool
				value, ok = object[key]
				if !ok {
					return nil, errors.New("missing observation field")
				}
			case []any:
				index, err := strconv.Atoi(key)
				if err != nil || index < 0 || index >= len(object) || (len(key) > 1 && key[0] == '0') {
					return nil, errors.New("invalid observation index")
				}
				value = object[index]
			default:
				return nil, errors.New("observation pointer is not traversable")
			}
		}
	}
	switch spec.Type {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return nil, errors.New("expected boolean")
		}
	case "string":
		if _, ok := value.(string); !ok {
			return nil, errors.New("expected string")
		}
	case "number", "integer":
		n, ok := value.(json.Number)
		if !ok {
			return nil, errors.New("expected number")
		}
		r, ok := new(big.Rat).SetString(string(n))
		if !ok || (spec.Type == "integer" && !r.IsInt()) {
			return nil, errors.New("invalid numeric observation")
		}
	default:
		return nil, errors.New("unsupported observation type")
	}
	return value, nil
}
func experimentPredicate(actual, expected any, operator string) (bool, error) {
	equal := reflect.DeepEqual(actual, expected)
	a, aok := actual.(json.Number)
	b, bok := expected.(json.Number)
	comparison := 0
	if aok && bok {
		ar, ok := new(big.Rat).SetString(string(a))
		if !ok {
			return false, errors.New("invalid number")
		}
		br, ok := new(big.Rat).SetString(string(b))
		if !ok {
			return false, errors.New("invalid number")
		}
		comparison = ar.Cmp(br)
		equal = comparison == 0
	}
	switch operator {
	case "eq":
		return equal, nil
	case "ne":
		return !equal, nil
	case "in":
		values, ok := expected.([]any)
		if !ok {
			return false, errors.New("membership requires array")
		}
		for _, v := range values {
			matched, err := experimentPredicate(actual, v, "eq")
			if err != nil {
				return false, err
			}
			if matched {
				return true, nil
			}
		}
		return false, nil
	case "lt", "le", "gt", "ge":
		if !aok || !bok {
			return false, errors.New("ordering requires numbers")
		}
		switch operator {
		case "lt":
			return comparison < 0, nil
		case "le":
			return comparison <= 0, nil
		case "gt":
			return comparison > 0, nil
		default:
			return comparison >= 0, nil
		}
	default:
		return false, errors.New("unsupported acceptance predicate")
	}
}
func (s *Store) validateExperimentEvidence(ctx context.Context, c model.ExperimentContract, profiles map[string]model.ExperimentProfile, r model.ExperimentReceipt) error {
	invalid := func(message string) error { return programError("evidence_invalid", message) }
	for _, spec := range c.Observations {
		for _, path := range c.BundleFiles {
			if spec.OutputPath == path {
				return invalid("observation output collides with frozen bundle input")
			}
		}
	}
	switch r.State {
	case "intent", "running", "cancelling", "uncertain", "waiting_prerequisite", "failed", "cancelled", "completed", "resolved_failed":
	default:
		return invalid("unknown runner state")
	}
	if r.State == "resolved_failed" {
		if strings.TrimSpace(r.ResolutionAttestation) == "" || len(r.ResolutionAttestation) > 4096 || r.FailureKind != "operator_resolved_failed" {
			return invalid("invalid runner resolution")
		}
	}
	switch r.ExecutionOutcome {
	case "pending", "running", "completed", "nonzero", "infrastructure", "prerequisite_missing", "cancelled", "uncertain":
	default:
		return invalid("unknown execution outcome")
	}
	switch r.ObservationOutcome {
	case "pending", "complete", "incomplete", "truncated", "not_evaluated":
	default:
		return invalid("unknown observation outcome")
	}
	switch r.HypothesisOutcome {
	case "pending", "supported", "falsified", "inconclusive":
	default:
		return invalid("unknown hypothesis outcome")
	}
	maxOutput := c.Limits.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	maxArtifacts := c.Limits.MaxArtifactBytes
	if maxArtifacts <= 0 {
		maxArtifacts = 16 << 20
	}
	maxFiles := c.Limits.MaxArtifactFiles
	if maxFiles <= 0 {
		maxFiles = 128
	}
	complete := len(r.Commands) == len(c.Commands) && len(c.Commands) > 0
	nonzero := false
	captured := int64(0)
	if len(r.Commands) > len(c.Commands) {
		return invalid("unexpected command receipts")
	}
	for i, command := range r.Commands {
		if command.StdoutBytes < 0 || command.StderrBytes < 0 {
			return invalid("negative capture size")
		}
		profileName := c.Commands[i].Profile
		if profileName == "" {
			profileName = c.Environment.Profile
		}
		profile, enrolled := profiles[profileName]
		if !enrolled || len(profile.Argv) == 0 {
			return invalid("command profile is not approved")
		}
		expectedLength := len(profile.Argv) + len(c.Commands[i].Args)
		if len(command.Argv) != expectedLength || !reflect.DeepEqual(command.Argv[:len(profile.Argv)], profile.Argv) {
			return invalid("command executable differs from approved profile")
		}
		for j, arg := range c.Commands[i].Args {
			if command.Argv[len(profile.Argv)+j] != arg {
				return invalid("command arguments differ from contract")
			}
		}
		if command.ExitCode == nil || command.Termination != "exited" || command.Signal != "" || command.StdoutTruncated || command.StderrTruncated || len(command.BinarySHA) != 64 {
			complete = false
		}
		if command.ExitCode != nil && *command.ExitCode != 0 {
			nonzero = true
		}
		for _, output := range []struct {
			sha            string
			size, observed int64
			truncated      bool
		}{{command.StdoutSHA, command.StdoutBytes, command.StdoutObservedBytes, command.StdoutTruncated}, {command.StderrSHA, command.StderrBytes, command.StderrObservedBytes, command.StderrTruncated}} {
			if output.observed < output.size || (!output.truncated && output.observed != output.size) || (output.truncated && output.observed <= output.size) {
				return invalid("observed capture length and truncation disagree")
			}
			if output.sha == "" {
				complete = false
				continue
			}
			data, err := s.blobs.Read(ctx, output.sha, maxOutput)
			if err != nil {
				return err
			}
			captured += int64(len(data))
			if int64(len(data)) != output.size {
				return invalid("capture size mismatch")
			}
		}
	}
	if captured > maxOutput {
		return invalid("output bound exceeded")
	}
	files := map[string]model.ProgramBundleFile{}
	total := int64(0)
	if r.Artifacts.ManifestSHA != "" {
		bundle, err := s.blobs.LoadBundle(ctx, r.Artifacts.ManifestSHA)
		if err != nil {
			return err
		}
		if programHash(bundle) != programHash(r.Artifacts) {
			return invalid("artifact manifest mismatch")
		}
		if len(bundle.Files) > maxFiles {
			return invalid("artifact count exceeded")
		}
		for _, file := range bundle.Files {
			if _, ok := files[file.Path]; ok {
				return invalid("duplicate output member")
			}
			if file.Bytes < 0 || file.Bytes > maxArtifacts-total {
				return invalid("artifact byte bound exceeded")
			}
			data, err := s.blobs.Read(ctx, file.ContentSHA, file.Bytes)
			if err != nil {
				return err
			}
			if int64(len(data)) != file.Bytes {
				return invalid("artifact size mismatch")
			}
			total += file.Bytes
			files[file.Path] = file
		}
	}
	observed := map[string]model.ExperimentObservation{}
	specs := map[string]model.ExperimentObservationSpec{}
	for _, spec := range c.Observations {
		specs[spec.Name] = spec
	}
	for _, observation := range r.Observations {
		spec, ok := specs[observation.Name]
		if !ok {
			return invalid("undeclared observation")
		}
		if _, ok = observed[observation.Name]; ok {
			return invalid("duplicate observation")
		}
		file, ok := files[spec.OutputPath]
		if !ok || observation.SourcePath != spec.OutputPath || observation.SourceSHA != file.ContentSHA || observation.Type != spec.Type || observation.Unit != spec.Unit {
			return invalid("observation provenance mismatch")
		}
		data, err := s.blobs.Read(ctx, file.ContentSHA, file.Bytes)
		if err != nil {
			return err
		}
		value, err := experimentValue(data, spec)
		if err != nil {
			return invalid(err.Error())
		}
		var actual any
		if err = model.StrictDecode(observation.Value, &actual); err != nil {
			return err
		}
		if !reflect.DeepEqual(value, actual) {
			return invalid("observation does not match captured output")
		}
		observed[spec.Name] = observation
	}
	observationsComplete := len(observed) == len(c.Observations)
	accepted := observationsComplete
	if observationsComplete {
		for _, predicate := range c.Acceptance {
			observation, ok := observed[predicate.Observation]
			if !ok {
				return invalid("predicate observation absent")
			}
			if predicate.Unit != "" && predicate.Unit != observation.Unit {
				return invalid("predicate unit mismatch")
			}
			var actual, expected any
			if err := model.StrictDecode(observation.Value, &actual); err != nil {
				return err
			}
			if err := model.StrictDecode(predicate.Value, &expected); err != nil {
				return err
			}
			match, err := experimentPredicate(actual, expected, predicate.Operator)
			if err != nil {
				return invalid(err.Error())
			}
			accepted = accepted && match
		}
	}
	// A nonzero exit is not an approved falsification predicate. Only complete
	// captured observations may support a conclusion; passing predicates with a
	// nonzero exit remain an inconclusive execution failure.
	conclusive := r.State == "completed" && complete && observationsComplete && (!nonzero || !accepted) && r.FinishedAt != nil
	if r.FailureKind != "" && r.FailureKind != "hypothesis_falsified" {
		conclusive = false
	}
	accepted = conclusive && accepted && !nonzero
	if r.Conclusive != conclusive || r.Accepted != accepted {
		return invalid("receipt conclusion is not supported by execution and captured evidence")
	}
	if conclusive {
		expected := "falsified"
		if accepted {
			expected = "supported"
		}
		if r.HypothesisOutcome != expected {
			return invalid("hypothesis outcome mismatch")
		}
		execution := "completed"
		if nonzero {
			execution = "nonzero"
		}
		if r.ExecutionOutcome != execution {
			return invalid("execution outcome mismatch")
		}
	} else if r.HypothesisOutcome == "supported" || r.HypothesisOutcome == "falsified" {
		return invalid("incomplete execution cannot conclude hypothesis")
	}
	if r.ObservationOutcome == "complete" && !observationsComplete {
		return invalid("observation outcome mismatch")
	}
	return nil
}

func experimentFrozenPolicy(ctx context.Context, tx *sql.Tx, request model.ExperimentRunRequest) (model.DiscoveryPolicy, error) {
	var frozen model.DiscoveryPolicy
	_, _, policyHash, _, err := loadAttempt(ctx, tx, request.AttemptID)
	if err != nil {
		return frozen, err
	}
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_policies WHERE program_id=? AND revision=? AND policy_hash=?`, request.ProgramID, request.Revision, policyHash).Scan(&data); err != nil {
		return frozen, programError("policy_changed", "experiment has no frozen execution policy")
	}
	if err = json.Unmarshal(data, &frozen); err != nil {
		return frozen, err
	}
	if request.ExecutionPolicySHA == "" || request.ExecutionPolicySHA != model.ExperimentPolicySHA(frozen.ExperimentProfiles, frozen.ExperimentLimits) {
		return frozen, programError("policy_changed", "experiment execution policy digest differs from frozen authority")
	}
	return frozen, nil
}

// ProgramExperimentIntent returns only the immutable controller-persisted launch
// request. Recovery must not reconstruct execution policy from current config.
func (s *Store) ProgramExperimentIntent(ctx context.Context, runID string) (model.ExperimentRunRequest, error) {
	var request model.ExperimentRunRequest
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM program_experiment_intents WHERE run_id=?`, runID).Scan(&data)
	if err != nil {
		return request, err
	}
	if err = json.Unmarshal(data, &request); err != nil {
		return request, err
	}
	if request.RunID != runID || programHash(canonicalExperimentRequest(request)) != programHash(request) {
		return model.ExperimentRunRequest{}, programError("evidence_invalid", "stored experiment intent identity is inconsistent")
	}
	return request, nil
}
