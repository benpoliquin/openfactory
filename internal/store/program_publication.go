package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// PublishProgramProposals installs the entire evaluated batch, never approving it.
func (s *Store) PublishProgramProposals(ctx context.Context, programID, manifestSHA string, cfg model.Config) ([]model.Feature, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := programLoad(ctx, tx, programID)
	if err != nil {
		return nil, err
	}
	p, r, err := s.programAuthorityTx(ctx, tx, programID, p.CurrentRevision, cfg.PolicyHash)
	if err != nil {
		return nil, err
	}
	m, err := publicationManifestTx(ctx, tx, programID, r.Number, manifestSHA)
	if err != nil {
		return nil, err
	}
	if err = programInvalidTx(ctx, tx, programID, manifestSHA); err != nil {
		return nil, err
	}
	artifacts := map[string]model.ProgramArtifact{}
	payloads := map[string][]byte{}
	for _, ref := range m.Inputs {
		var a model.ProgramArtifact
		var raw []byte
		e := tx.QueryRowContext(ctx, `SELECT data FROM program_artifacts WHERE id=?`, ref.ID).Scan(&raw)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		if !publicationArtifactMatches(programID, r.Number, a, ref) {
			return nil, programError("evidence_invalid", "artifact reference mismatch")
		}
		if e = programInvalidTx(ctx, tx, programID, a.ID); e != nil {
			return nil, e
		}
		if e = programInvalidTx(ctx, tx, programID, a.ContentSHA); e != nil {
			return nil, e
		}
		raw, e = s.blobs.Read(ctx, a.ContentSHA, 8<<20)
		if e != nil {
			return nil, e
		}
		raw, e = model.CanonicalProgramPayload(a.Kind, raw)
		if e != nil {
			return nil, e
		}
		artifacts[a.ID] = a
		payloads[a.ID] = raw
	}
	requirements := map[string]bool{}
	decisions := map[string]bool{}
	for id, a := range artifacts {
		switch a.Kind {
		case model.ArtifactRequirements:
			var v model.RequirementsPayload
			if err = json.Unmarshal(payloads[id], &v); err != nil {
				return nil, err
			}
			for _, x := range v.Requirements {
				requirements[x.ID] = true
			}
		case model.ArtifactArchitecture:
			var v model.ArchitecturePayload
			if err = json.Unmarshal(payloads[id], &v); err != nil {
				return nil, err
			}
			for _, x := range v.Decisions {
				decisions[x.ID] = true
			}
		}
	}
	var evaluationID string
	var evaluated model.ProgramManifest
	for _, ref := range m.Inputs {
		a, ok := artifacts[ref.ID]
		if !ok || a.Kind != model.ArtifactEvaluation || a.Revision != r.Number || a.Iteration != p.Iteration {
			continue
		}
		var v model.EvaluationPayload
		if err = json.Unmarshal(payloads[a.ID], &v); err != nil {
			return nil, err
		}
		if v.Verdict != "accept" {
			continue
		}
		var st model.ProgramStep
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE id=?`, a.ProducerStepID).Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &st); err != nil {
			return nil, err
		}
		if st.Kind != model.StepContractEvaluation || st.State != "completed" || st.ProgramID != programID || st.Revision != r.Number || st.Iteration != p.Iteration || st.InputManifestSHA != v.EvaluatedManifestSHA || a.InputManifestSHA != v.EvaluatedManifestSHA {
			continue
		}
		em, e := publicationManifestTx(ctx, tx, programID, r.Number, v.EvaluatedManifestSHA)
		if e != nil {
			return nil, e
		}
		if !publicationSubset(em, m) {
			return nil, programError("evidence_invalid", "evaluation manifest is not publication input")
		}
		coverage := map[string]bool{}
		for _, c := range v.Coverage {
			if c.Status == "covered" || c.Status == "satisfied" {
				if len(c.EvidenceRefs) == 0 {
					return nil, programError("evidence_invalid", "coverage requires evidence")
				}
				for _, ref := range c.EvidenceRefs {
					if !publicationHasRef(m, ref) {
						return nil, programError("evidence_invalid", "coverage evidence is outside manifest")
					}
				}
				coverage[c.SubjectID] = true
			}
		}
		complete := len(requirements) > 0
		for id := range requirements {
			if !coverage[id] {
				complete = false
			}
		}
		if !complete {
			continue
		}
		if evaluationID != "" {
			return nil, programError("evidence_invalid", "ambiguous contract evaluation")
		}
		evaluationID = a.ID
		evaluated = em
	}
	if evaluationID == "" {
		return nil, programError("evidence_invalid", "accepted exact contract evaluation and requirement coverage required")
	}
	batch := map[string]model.Feature{}
	links := map[string]model.ProgramFeatureLink{}
	var order []string
	for _, ref := range evaluated.Inputs {
		a, ok := artifacts[ref.ID]
		if !ok || a.Kind != model.ArtifactImplementationContracts || a.Revision != r.Number || a.Iteration != p.Iteration {
			continue
		}
		var producer model.ProgramStep
		var producerRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE id=?`, a.ProducerStepID).Scan(&producerRaw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(producerRaw, &producer); err != nil {
			return nil, err
		}
		if producer.ProgramID != programID || producer.Revision != r.Number || producer.Iteration != p.Iteration || producer.Kind != model.StepContracts || producer.State != "completed" {
			return nil, programError("evidence_invalid", "implementation contract producer is not current")
		}
		var v model.ImplementationContractsPayload
		if err = json.Unmarshal(payloads[a.ID], &v); err != nil {
			return nil, err
		}
		for _, c := range v.Contracts {
			f := c.Feature
			if _, ok = batch[f.ID]; ok {
				return nil, programError("invalid_contract", "duplicate proposal ID")
			}
			if f.Project != p.Project || f.DiscoveryStepID != "" || strings.HasPrefix(f.ID, "openfactory-discovery-") || strings.HasPrefix(f.ID, "discovery-") || strings.HasPrefix(f.ID, "discovery/") {
				return nil, programError("invalid_contract", "reserved or foreign proposal")
			}
			if !publicationContains(r.Spec.Implementation.PermittedFeatureKinds, f.Kind) || !publicationContains(r.Spec.Implementation.PermittedCompletionTargets, f.CompletionTarget) {
				return nil, programError("invalid_contract", "implementation policy rejects feature")
			}
			project, ok := cfg.Projects[f.Project]
			if !ok {
				return nil, programError("invalid_contract", "unknown project")
			}
			for _, check := range f.Checks {
				if _, ok = project.Checks[check]; !ok {
					return nil, programError("invalid_contract", "unknown project check")
				}
			}
			for _, id := range c.RequirementRefs {
				if !requirements[id] {
					return nil, programError("evidence_invalid", "unknown requirement trace")
				}
			}
			for _, id := range c.DecisionRefs {
				if !decisions[id] {
					return nil, programError("evidence_invalid", "unknown decision trace")
				}
			}
			if len(c.EvidenceRefs) == 0 {
				return nil, programError("evidence_invalid", "contract evidence required")
			}
			for _, ref := range c.EvidenceRefs {
				if !publicationHasRef(evaluated, ref) {
					return nil, programError("evidence_invalid", "contract evidence outside evaluated manifest")
				}
			}
			f.ProgramEvidenceGate = &model.ProgramEvidenceGate{ProgramID: programID, Revision: r.Number, ManifestSHA: manifestSHA, EvaluationArtifactID: evaluationID}
			if err = intake.Validate(f); err != nil {
				return nil, programError("invalid_contract", err.Error())
			}
			f.Revision = intake.Revision(f)
			if err = s.programFeatureGateTx(ctx, tx, f, cfg.PolicyHash); err != nil {
				return nil, err
			}
			batch[f.ID] = f
			order = append(order, f.ID)
			links[f.ID] = model.ProgramFeatureLink{ProgramID: programID, Revision: r.Number, FeatureID: f.ID, ContractArtifactID: a.ID, ManifestSHA: manifestSHA}
		}
	}
	if len(order) == 0 {
		return nil, programError("invalid_contract", "no evaluated implementation contracts")
	}
	if err = validateProgramProposalBatchTx(ctx, tx, batch); err != nil {
		return nil, err
	}
	out := make([]model.Feature, 0, len(order))
	created := false
	for _, id := range order {
		f := batch[id]
		old, e := publicationReplayTx(ctx, tx, f, links[id])
		if e == nil {
			out = append(out, old)
			continue
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		created = true
		f.State = "proposed"
		f.PolicyHash = ""
		f.Reason = ""
		f.Receipt = ""
		f.Attempts = 0
		f.CreatedAt = time.Now().UTC()
		f.UpdatedAt = f.CreatedAt
		if err = put(ctx, tx, f); err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(f)
		if _, err = tx.ExecContext(ctx, `INSERT INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, id, f.Revision, raw); err != nil {
			return nil, err
		}
		raw, _ = json.Marshal(links[id])
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_feature_links(program_id,feature_id,data) VALUES(?,?,?)`, programID, id, raw); err != nil {
			return nil, err
		}
		if err = event(ctx, tx, id, "imported", f.Revision, progressOf(f)); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if created || p.State != model.ProgramProposed {
		p.State = model.ProgramProposed
		p.Phase = "publication"
		p.Wait = nil
		if err = programPut(ctx, tx, p); err != nil {
			return nil, err
		}
		if err = programEvent(ctx, tx, programID, "proposed", manifestSHA); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

func publicationManifestTx(ctx context.Context, tx *sql.Tx, id string, revision int, sha string) (model.ProgramManifest, error) {
	var m model.ProgramManifest
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT data FROM program_manifests WHERE sha=? AND program_id=? AND revision=?`, sha, id, revision).Scan(&raw); err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.SHA != sha || programHash(m.Inputs) != sha {
		return m, programError("evidence_invalid", "manifest hash mismatch")
	}
	return m, nil
}
func publicationContains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
func publicationHasRef(m model.ProgramManifest, r model.ProgramInputRef) bool {
	for _, x := range m.Inputs {
		if x == r {
			return true
		}
	}
	return false
}
func publicationSubset(a, b model.ProgramManifest) bool {
	for _, r := range a.Inputs {
		if !publicationHasRef(b, r) {
			return false
		}
	}
	return true
}
func publicationDependenciesTx(ctx context.Context, tx *sql.Tx, batch map[string]model.Feature) error {
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if strings.HasPrefix(id, "openfactory-discovery-") {
			return programError("invalid_contract", "discovery analysis cannot satisfy a product dependency")
		}
		if state[id] == 1 {
			return programError("invalid_contract", "dependency cycle")
		}
		if state[id] == 2 {
			return nil
		}
		f, ok := batch[id]
		if !ok {
			var err error
			f, err = get(ctx, tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				return programError("invalid_contract", fmt.Sprintf("dependency %s does not exist", id))
			}
			if err != nil {
				return fmt.Errorf("dependency %s: %w", id, err)
			}
		}
		if f.DiscoveryStepID != "" {
			return programError("invalid_contract", "discovery analysis cannot satisfy a product dependency")
		}
		state[id] = 1
		for _, dep := range f.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range batch {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func publicationReplayTx(ctx context.Context, tx *sql.Tx, f model.Feature, want model.ProgramFeatureLink) (model.Feature, error) {
	old, err := get(ctx, tx, f.ID)
	if err != nil {
		return old, err
	}
	var raw []byte
	var link model.ProgramFeatureLink
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_feature_links WHERE feature_id=?`, f.ID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return old, programError("idempotency_conflict", "feature exists without exact publication link")
		}
		return old, err
	}
	if err = json.Unmarshal(raw, &link); err != nil {
		return old, err
	}
	if old.Revision != f.Revision || programHash(link) != programHash(want) {
		return old, programError("idempotency_conflict", "feature ID belongs to a different immutable publication")
	}
	return old, nil
}

// validateProgramProposalBatchTx reports model contract mistakes as durable
// domain failures while preserving database and corrupt-ledger errors.
func validateProgramProposalBatchTx(ctx context.Context, tx *sql.Tx, batch map[string]model.Feature) error {
	for _, f := range batch {
		if err := intake.Validate(f); err != nil {
			return programError("invalid_contract", err.Error())
		}
	}
	return publicationDependenciesTx(ctx, tx, batch)
}

// Historical evidence retains its immutable revision identity when explicitly
// bound by a current manifest; authority-bearing producers are checked separately.
func publicationArtifactMatches(programID string, currentRevision int, a model.ProgramArtifact, ref model.ProgramInputRef) bool {
	return a.ProgramID == programID && a.Revision >= 1 && a.Revision <= currentRevision && a.Revision == ref.Revision && a.ContentSHA == ref.SHA && a.Kind == ref.Kind && a.ID == ref.ID
}
