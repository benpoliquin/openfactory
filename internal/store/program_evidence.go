package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/research"
)

func programStepTx(ctx context.Context, q programQuerier, id string) (model.ProgramStep, error) {
	var step model.ProgramStep
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &step)
	}
	return step, err
}
func programArtifactTx(ctx context.Context, q programQuerier, id string) (model.ProgramArtifact, error) {
	var a model.ProgramArtifact
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM program_artifacts WHERE id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &a)
	}
	return a, err
}
func programManifestLoadTx(ctx context.Context, q programQuerier, id string) (model.ProgramManifest, string, int, error) {
	var m model.ProgramManifest
	var raw []byte
	var programID string
	var revision int
	err := q.QueryRowContext(ctx, `SELECT data,program_id,revision FROM program_manifests WHERE sha=?`, id).Scan(&raw, &programID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return m, programID, revision, programError("evidence_invalid", "input manifest missing")
	}
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	if err == nil && (m.SHA != id || programHash(m.Inputs) != id) {
		err = errors.New("manifest content identity mismatch")
	}
	return m, programID, revision, err
}
func programValidateRefTx(ctx context.Context, tx *sql.Tx, id string, revision int, ref model.ProgramInputRef) error {
	if ref.ID == "" || ref.SHA == "" {
		return programError("evidence_invalid", "reference identity missing")
	}
	if err := programInvalidTx(ctx, tx, id, ref.ID); err != nil {
		return err
	}
	if err := programInvalidTx(ctx, tx, id, ref.SHA); err != nil {
		return err
	}
	switch ref.Kind {
	case "program_revision":
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT data FROM program_revisions WHERE program_id=? AND revision_hash=?`, id, ref.SHA).Scan(&raw); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("evidence_invalid", "revision reference missing")
		}
		var r model.ProgramRevision
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if ref.ID != r.RevisionHash || r.Number > revision || (ref.Revision != 0 && ref.Revision != r.Number) {
			return programError("evidence_invalid", "revision reference scope mismatch")
		}
	case "bundle":
		if ref.ID != ref.SHA {
			return programError("evidence_invalid", "bundle identity mismatch")
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM program_bundles WHERE program_id=? AND manifest_sha=? AND revision<=?`, id, ref.SHA, revision).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return programError("evidence_invalid", "bundle is not owned by this program")
		}
	case "source":
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT data FROM program_sources WHERE id=? AND program_id=?`, ref.ID, id).Scan(&raw); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("evidence_invalid", "source is not controller-captured")
		}
		var source model.SourceCapture
		if err := json.Unmarshal(raw, &source); err != nil {
			return err
		}
		if source.TextSHA256 != ref.SHA || source.Revision > revision || (ref.Revision != 0 && ref.Revision != source.Revision) || !source.Valid || source.FreshUntil == nil || !time.Now().Before(*source.FreshUntil) {
			return programError("evidence_invalid", "source scope, identity or freshness invalid")
		}
		if err := programInvalidTx(ctx, tx, id, source.ContentSHA256); err != nil {
			return err
		}
	case "answer":
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT data FROM program_answers WHERE id=? AND program_id=?`, ref.ID, id).Scan(&raw); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("evidence_invalid", "answer reference missing")
		}
		var answer model.QuestionAnswer
		if err := json.Unmarshal(raw, &answer); err != nil {
			return err
		}
		if model.ProgramAnswerSHA(answer) != ref.SHA || ref.ID != ref.SHA {
			return programError("evidence_invalid", "answer content mismatch")
		}
	case "experiment_receipt", "experiment", "receipt":
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE run_id=? AND program_id=?`, ref.ID, id).Scan(&raw); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("evidence_invalid", "experiment receipt missing")
		}
		var receipt model.ExperimentReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		if programHash(receipt) != ref.SHA || receipt.Revision > revision || (ref.Revision != 0 && ref.Revision != receipt.Revision) || receipt.State == "uncertain" || receipt.State == "running" || receipt.State == "pending" {
			return programError("evidence_invalid", "experiment receipt not terminal or identity mismatch")
		}
	default:
		a, err := programArtifactTx(ctx, tx, ref.ID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("evidence_invalid", "artifact reference missing")
		}
		if a.ProgramID != id || a.Revision > revision || (ref.Revision != 0 && a.Revision != ref.Revision) || a.ContentSHA != ref.SHA || (ref.Kind != "artifact" && a.Kind != ref.Kind) {
			return programError("evidence_invalid", "artifact reference scope or content mismatch")
		}
	}
	return nil
}
func programManifestValidTx(ctx context.Context, tx *sql.Tx, id string, revision int, sha string, seen map[string]bool) error {
	if sha == "" {
		return programError("evidence_invalid", "input manifest missing")
	}
	if seen[sha] {
		return nil
	}
	seen[sha] = true
	if len(seen) > 1024 {
		return programError("evidence_invalid", "manifest graph exceeds bound")
	}
	if err := programInvalidTx(ctx, tx, id, sha); err != nil {
		return err
	}
	m, pid, n, err := programManifestLoadTx(ctx, tx, sha)
	if err != nil {
		return err
	}
	if pid != id || n > revision {
		return programError("evidence_invalid", "manifest scope mismatch")
	}
	for _, ref := range m.Inputs {
		if err = programValidateRefTx(ctx, tx, id, revision, ref); err != nil {
			return err
		}
		switch ref.Kind {
		case "program_revision", "bundle", "source", "answer", "experiment_receipt", "experiment", "receipt":
		default:
			a, e := programArtifactTx(ctx, tx, ref.ID)
			if e != nil {
				return e
			}
			if a.InputManifestSHA != "" {
				if e = programManifestValidTx(ctx, tx, id, revision, a.InputManifestSHA, seen); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func (s *Store) programFeatureGateTx(ctx context.Context, tx *sql.Tx, f model.Feature, policy string) error {
	if f.DiscoveryStepID != "" {
		step, err := programStepTx(ctx, tx, f.DiscoveryStepID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("invalid_contract", "discovery association is not server-issued")
		}
		if step.FeatureID != f.ID || f.ID != "openfactory-discovery-"+step.ID || f.Kind != "research" || f.CompletionTarget != "reviewed" {
			return programError("invalid_contract", "discovery feature identity mismatch")
		}
		_, r, err := s.programAuthorityTx(ctx, tx, step.ProgramID, step.Revision, policy)
		if err != nil {
			return err
		}
		if !programModelAllowed(r.Spec, step.Model) && step.Kind != model.StepExperimentRun {
			return programError("approval_required", "step provider disclosure no longer permits model")
		}
		if err = programManifestValidTx(ctx, tx, step.ProgramID, step.Revision, step.InputManifestSHA, map[string]bool{}); err != nil {
			return err
		}
		if step.Kind == model.StepExperimentRun {
			if err = programSubjectApprovedTx(ctx, tx, step.ProgramID, r.RevisionHash, step.SubjectID, policy); err != nil {
				return err
			}
			manifest, _, _, e := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
			if e != nil {
				return e
			}
			found := false
			for _, ref := range manifest.Inputs {
				if ref.Kind != model.ArtifactArchitecture {
					continue
				}
				architecture, e := programArtifactTx(ctx, tx, ref.ID)
				if e != nil {
					return e
				}
				if architecture.Iteration != step.Iteration {
					continue
				}
				found = true
				if e = programSubjectApprovedTx(ctx, tx, step.ProgramID, r.RevisionHash, architecture.ContentSHA, policy); e != nil {
					return e
				}
			}
			if !found {
				return programError("approval_required", "exact current architecture approval required")
			}
		}
	} else if strings.HasPrefix(f.ID, "openfactory-discovery-") {
		return programError("invalid_contract", "reserved discovery identity lacks server association")
	}
	if gate := f.ProgramEvidenceGate; gate != nil {
		_, _, err := s.programAuthorityTx(ctx, tx, gate.ProgramID, gate.Revision, policy)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				var exists int
				if lookupErr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM programs WHERE id=?)`, gate.ProgramID).Scan(&exists); lookupErr != nil {
					return lookupErr
				}
				if exists == 0 {
					return programError("evidence_invalid", "program evidence source is unavailable")
				}
			}
			return err
		}
		if err = programManifestValidTx(ctx, tx, gate.ProgramID, gate.Revision, gate.ManifestSHA, map[string]bool{}); err != nil {
			return err
		}
		a, err := programArtifactTx(ctx, tx, gate.EvaluationArtifactID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return programError("evidence_invalid", "independent contract evaluation missing")
		}
		if a.ProgramID != gate.ProgramID || a.Revision != gate.Revision || a.Kind != model.ArtifactEvaluation || a.EvaluationVerdict != "accept" || a.EvaluatedManifestSHA == "" || a.EvaluatedManifestSHA != a.InputManifestSHA {
			return programError("evidence_invalid", "contract evaluation is not accepted for exact input")
		}
		step, err := programStepTx(ctx, tx, a.ProducerStepID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return programError("evidence_invalid", "evaluation producer step missing")
			}
			return err
		}
		if step.Kind != model.StepContractEvaluation || step.State != "completed" {
			return programError("evidence_invalid", "evaluation is not an independent completed contract review")
		}
		m, _, _, err := programManifestLoadTx(ctx, tx, gate.ManifestSHA)
		if err != nil {
			return err
		}
		found := false
		for _, ref := range m.Inputs {
			if ref.ID == a.ID && ref.SHA == a.ContentSHA {
				found = true
			}
		}
		if !found {
			return programError("evidence_invalid", "evaluation is not in publication manifest")
		}
		if err = independentProgramThreadTx(ctx, tx, model.Feature{DiscoveryStepID: step.ID}, a.ProducerThreadID); err != nil {
			return err
		}
	}
	return nil
}
func programSubjectApprovedTx(ctx context.Context, tx *sql.Tx, id, revisionHash, subject, policy string) error {
	if err := programInvalidTx(ctx, tx, id, subject); err != nil {
		return err
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=? AND subject_hash=? AND policy_hash=?`, id, revisionHash, subject, policy).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return programError("approval_required", "exact immutable artifact approval required")
	}
	return nil
}
func reserveProgramCallTx(ctx context.Context, tx *sql.Tx, f model.Feature) error {
	if f.DiscoveryStepID == "" {
		return nil
	}
	step, err := programStepTx(ctx, tx, f.DiscoveryStepID)
	if err != nil {
		return err
	}
	if step.Kind == model.StepExperimentRun {
		return programError("invalid_contract", "experiments cannot reserve model calls")
	}
	r, err := programRevision(ctx, tx, step.ProgramID, step.Revision)
	if err != nil {
		return err
	}
	budget, err := getProgramBudgetTx(ctx, tx, step.ProgramID)
	if err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_policies WHERE program_id=? AND revision=? AND policy_hash=?`, step.ProgramID, step.Revision, f.PolicyHash).Scan(&raw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return programError("policy_changed", "step has no frozen policy limits")
	}
	var policy model.DiscoveryPolicy
	if err = json.Unmarshal(raw, &policy); err != nil {
		return err
	}
	max := model.NormalizeDiscoveryPolicy(policy).Limits.MaxModelCalls
	if max > 20 {
		max = 20
	}
	if r.Spec.Limits.MaxModelCalls > 0 && r.Spec.Limits.MaxModelCalls < max {
		max = r.Spec.Limits.MaxModelCalls
	}
	if budget.ModelCalls >= max {
		return programError("budget_exhausted", "program lifetime model-call allowance exhausted")
	}
	budget.ModelCalls++
	return putProgramBudgetTx(ctx, tx, step.ProgramID, budget)
}
func independentProgramThreadTx(ctx context.Context, tx *sql.Tx, f model.Feature, thread string) error {
	if f.DiscoveryStepID == "" {
		return nil
	}
	step, err := programStepTx(ctx, tx, f.DiscoveryStepID)
	if err != nil {
		return err
	}
	if step.Kind != model.StepPlanEvaluation && step.Kind != model.StepResultEvaluation && step.Kind != model.StepContractEvaluation {
		return nil
	}
	if thread == "" {
		return programError("evidence_invalid", "independent evaluation session missing")
	}
	m, _, _, err := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
	if err != nil {
		return err
	}
	for _, ref := range m.Inputs {
		switch ref.Kind {
		case "program_revision", "bundle", "source", "answer", "experiment_receipt":
			continue
		}
		a, e := programArtifactTx(ctx, tx, ref.ID)
		if e != nil {
			return e
		}
		if a.ProducerThreadID == thread {
			return programError("evidence_invalid", "evaluator cannot reuse author session")
		}
	}
	return nil
}
func (s *Store) ValidateProgramAttempt(ctx context.Context, a model.Attempt, cfg model.Config) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, old, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if f.DiscoveryStepID == "" {
		return nil
	}
	if time.Now().After(old.Deadline) {
		return programError("approval_required", "attempt deadline expired")
	}
	if f.PolicyHash != cfg.PolicyHash {
		return programError("policy_changed", "operator policy changed")
	}
	return s.programFeatureGateTx(ctx, tx, f, cfg.PolicyHash)
}

func (s *Store) ProgramSourcePayload(ctx context.Context, id string) (model.SourceCapture, []byte, error) {
	var source model.SourceCapture
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM program_sources WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return source, nil, err
	}
	if err = json.Unmarshal(raw, &source); err != nil {
		return source, nil, err
	}
	text, err := s.blobs.Read(ctx, source.TextSHA256, 1<<20)
	return source, text, err
}
func (s *Store) ProgramExperimentReceipt(ctx context.Context, id string) (model.ExperimentReceipt, error) {
	var receipt model.ExperimentReceipt
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE run_id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &receipt)
	}
	return receipt, err
}
func (s *Store) ProgramAnswerPayload(ctx context.Context, id string) (model.QuestionAnswer, error) {
	var answer model.QuestionAnswer
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM program_answers WHERE id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &answer)
	}
	return answer, err
}

func (s *Store) validateProgramPayloadTx(ctx context.Context, tx *sql.Tx, step model.ProgramStep, kind string, raw []byte) error {
	// The complete transaction stages every output envelope before checking references,
	// allowing requirements and claims from one result to refer to one another.
	m, _, _, err := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
	if err != nil {
		return err
	}
	available := map[string]bool{}
	claims := map[string]bool{}
	questions := map[string]bool{}
	requirements := map[string]bool{}
	decisions := map[string]bool{}
	experiments := map[string]bool{}
	collect := func(k string, data []byte) error {
		switch k {
		case model.ArtifactClaims:
			var p model.ClaimsPayload
			if e := model.StrictDecode(data, &p); e != nil {
				return e
			}
			for _, v := range p.Claims {
				claims[v.ID] = true
				available[v.ID] = true
			}
		case model.ArtifactQuestions:
			var p model.QuestionsPayload
			if e := model.StrictDecode(data, &p); e != nil {
				return e
			}
			for _, v := range p.Questions {
				questions[v.ID] = true
				available[v.ID] = true
			}
		case model.ArtifactRequirements:
			var p model.RequirementsPayload
			if e := model.StrictDecode(data, &p); e != nil {
				return e
			}
			for _, v := range p.Requirements {
				requirements[v.ID] = true
				available[v.ID] = true
			}
		case model.ArtifactArchitecture:
			var p model.ArchitecturePayload
			if e := model.StrictDecode(data, &p); e != nil {
				return e
			}
			for _, v := range p.Decisions {
				decisions[v.ID] = true
				available[v.ID] = true
			}
		case model.ArtifactExperimentContracts:
			var p model.ExperimentContractsPayload
			if e := model.StrictDecode(data, &p); e != nil {
				return e
			}
			for _, v := range p.Experiments {
				experiments[v.ID] = true
				available[v.ID] = true
			}
		}
		return nil
	}
	var artifacts []model.ProgramArtifact
	for _, ref := range m.Inputs {
		switch ref.Kind {
		case "program_revision", "bundle", "source", "answer", "experiment_receipt":
			continue
		}
		a, e := programArtifactTx(ctx, tx, ref.ID)
		if e != nil {
			return e
		}
		artifacts = append(artifacts, a)
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM program_artifacts WHERE program_id=? AND revision=?`, step.ProgramID, step.Revision)
	if err != nil {
		return err
	}
	for rows.Next() {
		var data []byte
		var a model.ProgramArtifact
		if err = rows.Scan(&data); err != nil {
			break
		}
		if err = json.Unmarshal(data, &a); err != nil {
			break
		}
		if a.ProducerStepID == step.ID {
			artifacts = append(artifacts, a)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range artifacts {
		data, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
		if e != nil {
			return e
		}
		if e = collect(a.Kind, data); e != nil {
			return e
		}
	}
	for _, artifact := range artifacts {
		if artifact.ProducerStepID != step.ID || artifact.SupersedesID == "" {
			continue
		}
		predecessor, e := programArtifactTx(ctx, tx, artifact.SupersedesID)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e != nil || predecessor.ProgramID != step.ProgramID || predecessor.Kind != artifact.Kind || predecessor.Revision > step.Revision || predecessor.ID == artifact.ID || predecessor.ProducerStepID == step.ID {
			return programError("evidence_invalid", "artifact supersession is not a prior same-kind program artifact")
		}
		explicit := false
		for _, ref := range m.Inputs {
			if ref.ID == predecessor.ID && ref.SHA == predecessor.ContentSHA && (ref.Kind == predecessor.Kind || ref.Kind == "artifact") {
				explicit = true
				break
			}
		}
		if !explicit {
			return programError("evidence_invalid", "artifact supersession predecessor is not in exact input manifest")
		}
		producer, e := programStepTx(ctx, tx, predecessor.ProducerStepID)
		if e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return programError("evidence_invalid", "artifact supersession producer missing")
			}
			return e
		}
		if producer.Kind != step.Kind {
			return programError("evidence_invalid", "artifact supersession producer role mismatch")
		}
	}
	refs := func(ids []string, set map[string]bool) error {
		for _, id := range ids {
			if !set[id] {
				return programError("evidence_invalid", "unresolved typed reference: "+id)
			}
		}
		return nil
	}
	switch kind {
	case model.ArtifactClaims:
		var payload model.ClaimsPayload
		if err = model.StrictDecode(raw, &payload); err != nil {
			return err
		}
		for _, claim := range payload.Claims {
			if err = refs(claim.InferencePremises, claims); err != nil {
				return err
			}
			for _, citation := range claim.Citations {
				if err = s.verifyProgramCitationTx(ctx, tx, step, citation); err != nil {
					return err
				}
			}
		}
	case model.ArtifactRequirements:
		var p model.RequirementsPayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, v := range p.Requirements {
			if err = refs(v.ClaimRefs, claims); err != nil {
				return err
			}
			if err = refs(v.QuestionRefs, questions); err != nil {
				return err
			}
		}
	case model.ArtifactQuestions:
		var p model.QuestionsPayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, v := range p.Questions {
			if err = refs(v.AffectedIDs, available); err != nil {
				return err
			}
		}
	case model.ArtifactArchitecture:
		var p model.ArchitecturePayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, v := range p.Decisions {
			if err = refs(v.RequirementRefs, requirements); err != nil {
				return err
			}
			if err = refs(v.QuestionRefs, questions); err != nil {
				return err
			}
			if err = refs(v.ExperimentRefs, experiments); err != nil {
				return err
			}
		}
	case model.ArtifactExperimentContracts:
		var p model.ExperimentContractsPayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, v := range p.Experiments {
			if err = refs(v.QuestionRefs, questions); err != nil {
				return err
			}
			if err = refs(v.DecisionRefs, decisions); err != nil {
				return err
			}
			bundle, e := s.blobs.LoadBundle(ctx, v.BundleManifestSHA)
			if e != nil {
				if errors.Is(e, os.ErrNotExist) {
					return programError("evidence_invalid", "experiment frozen bundle missing")
				}
				return e
			}
			declared := map[string]bool{}
			for _, name := range v.BundleFiles {
				if declared[name] || !strings.HasPrefix(name, "experiments/"+v.ID+"/") {
					return programError("evidence_invalid", "experiment bundle member scope invalid")
				}
				declared[name] = true
			}
			if len(declared) == 0 || len(declared) != len(bundle.Files) {
				return programError("evidence_invalid", "frozen experiment bundle differs from declared files")
			}
			for _, file := range bundle.Files {
				if !declared[file.Path] {
					return programError("evidence_invalid", "undeclared experiment bundle member")
				}
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO program_bundles(program_id,manifest_sha,revision) VALUES(?,?,?)`, step.ProgramID, bundle.ManifestSHA, step.Revision); err != nil {
				return err
			}
			for _, ref := range v.Environment.DatasetRefs {
				if err = programValidateRefTx(ctx, tx, step.ProgramID, step.Revision, ref); err != nil {
					return err
				}
			}
		}
	case model.ArtifactEvaluation:
		var p model.EvaluationPayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		if p.EvaluatedManifestSHA != step.InputManifestSHA {
			return programError("evidence_invalid", "evaluation does not bind exact input manifest")
		}
		input := map[string]bool{}
		for _, ref := range m.Inputs {
			input[ref.Kind+"/"+ref.ID+"/"+ref.SHA] = true
		}
		for _, finding := range p.Findings {
			for _, ref := range finding.Refs {
				if !input[ref.Kind+"/"+ref.ID+"/"+ref.SHA] {
					return programError("evidence_invalid", "finding references unreviewed evidence")
				}
			}
		}
		for _, coverage := range p.Coverage {
			if !available[coverage.SubjectID] {
				return programError("evidence_invalid", "coverage subject does not exist")
			}
			for _, ref := range coverage.EvidenceRefs {
				if !input[ref.Kind+"/"+ref.ID+"/"+ref.SHA] {
					return programError("evidence_invalid", "coverage references unreviewed evidence")
				}
			}
		}
	case model.ArtifactImplementationContracts:
		var p model.ImplementationContractsPayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		batch := make(map[string]model.Feature, len(p.Contracts))
		for _, contract := range p.Contracts {
			batch[contract.Feature.ID] = contract.Feature
		}
		if err = validateProgramProposalBatchTx(ctx, tx, batch); err != nil {
			return err
		}
		for _, v := range p.Contracts {
			if err = refs(v.RequirementRefs, requirements); err != nil {
				return err
			}
			if err = refs(v.DecisionRefs, decisions); err != nil {
				return err
			}
			for _, ref := range v.EvidenceRefs {
				if err = programValidateRefTx(ctx, tx, step.ProgramID, step.Revision, ref); err != nil {
					return err
				}
			}
		}
	case model.ArtifactRevision:
		var p model.RevisionPayload
		if err = model.StrictDecode(raw, &p); err != nil {
			return err
		}
		if p.PredecessorManifestSHA != step.InputManifestSHA {
			return programError("evidence_invalid", "revision predecessor mismatch")
		}
		for _, ref := range p.FalsifiedEvidence {
			if err = programValidateRefTx(ctx, tx, step.ProgramID, step.Revision, ref); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Store) verifyProgramCitationTx(ctx context.Context, tx *sql.Tx, step model.ProgramStep, c model.SourceCitation) error {
	if c.Quote == "" || c.TextSHA == "" {
		return programError("evidence_invalid", "citation requires exact quote and extracted text identity")
	}
	ref := model.ProgramInputRef{Kind: "source", ID: c.SourceID, SHA: c.TextSHA}
	if err := programValidateRefTx(ctx, tx, step.ProgramID, step.Revision, ref); err != nil {
		return err
	}
	text, err := s.blobs.Read(ctx, c.TextSHA, 1<<20)
	if err != nil {
		return err
	}
	if !research.VerifyQuote(string(text), c.Quote, c.StartLine, c.EndLine) {
		return programError("evidence_invalid", "citation quote does not occur within captured text locator")
	}
	if c.JSONPointer != "" {
		var doc any
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.UseNumber()
		if err = dec.Decode(&doc); err != nil {
			return programError("evidence_invalid", "JSON pointer citation requires captured JSON")
		}
		value, e := programJSONPointer(doc, c.JSONPointer)
		if e != nil {
			return e
		}
		encoded, _ := json.Marshal(value)
		str, ok := value.(string)
		if (!ok || !strings.Contains(str, c.Quote)) && !bytes.Contains(encoded, []byte(c.Quote)) {
			return programError("evidence_invalid", "JSON pointer does not contain quote")
		}
	}
	if c.ExtractionBlock != "" {
		return programError("evidence_invalid", "extractor does not issue authenticated extraction block identifiers")
	}
	return nil
}
func programJSONPointer(doc any, pointer string) (any, error) {
	if pointer == "" {
		return doc, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, programError("evidence_invalid", "invalid JSON pointer")
	}
	value := doc
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, programError("evidence_invalid", "JSON pointer missing")
			}
		case []any:
			var n int
			if _, err := fmt.Sscanf(part, "%d", &n); err != nil || fmt.Sprint(n) != part || n < 0 || n >= len(v) {
				return nil, programError("evidence_invalid", "JSON pointer index invalid")
			}
			value = v[n]
		default:
			return nil, errors.New("JSON pointer traverses scalar")
		}
	}
	return value, nil
}
