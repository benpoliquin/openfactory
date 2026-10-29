package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"openfactory/internal/model"
)

func (s *Store) AnswerProgramQuestion(ctx context.Context, id, revisionHash string, answer model.ProgramAnswer, audit model.AuditContext) (model.ProgramRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ProgramRevision{}, err
	}
	defer tx.Rollback()
	out, err := s.answerProgramQuestionTx(ctx, tx, id, revisionHash, answer, audit)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) answerProgramQuestionTx(ctx context.Context, tx *sql.Tx, id, revisionHash string, answer model.ProgramAnswer, audit model.AuditContext) (model.ProgramRevision, error) {
	var out model.ProgramRevision
	if err := validateAuditContext(audit); err != nil {
		return out, err
	}
	answer.Actor = programAuditActor(audit)
	answer.Audit = audit
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return out, err
	}
	current, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return out, err
	}
	// Check this before replay detection: a retry with stale authority is stale,
	// even when the same answer was previously accepted.
	if current.RevisionHash != revisionHash || (answer.RevisionHash != "" && answer.RevisionHash != revisionHash) {
		return out, programError("stale_revision", "answer requires the exact current revision")
	}
	if p.State == model.ProgramCancelled {
		return out, programError("approval_required", "program is cancelled")
	}
	if (answer.ProgramID != "" && answer.ProgramID != id) || strings.TrimSpace(answer.Text) == "" || strings.TrimSpace(answer.Actor) == "" || answer.QuestionID == "" || answer.QuestionArtifactID == "" || answer.QuestionArtifactSHA == "" {
		return out, programError("invalid_contract", "answer requires a question binding, text, and actor for this program")
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_artifacts WHERE id=? AND program_id=? AND kind=?`, answer.QuestionArtifactID, id, model.ArtifactQuestions).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, programError("question_unresolved", "question artifact does not belong to this program")
		}
		return out, err
	}
	var artifact model.ProgramArtifact
	if err = json.Unmarshal(raw, &artifact); err != nil {
		return out, err
	}
	if artifact.ContentSHA != answer.QuestionArtifactSHA || artifact.Revision > current.Number {
		return out, programError("stale_revision", "question artifact binding changed")
	}
	// Carry-forward is permitted through answer-only revisions. An import with a
	// new specification must produce its own questions rather than revive old ones.
	for n := current.Number; n > artifact.Revision; {
		r, e := programRevision(ctx, tx, id, n)
		if e != nil {
			return out, e
		}
		if r.Reason != "answer" || r.Supersedes <= 0 || r.Supersedes >= n {
			return out, programError("stale_revision", "question is not carried into the current revision")
		}
		n = r.Supersedes
	}
	artifacts, err := programEvidenceDataTx(ctx, tx, "program_artifacts", id)
	if err != nil {
		return out, err
	}
	for _, data := range artifacts {
		var newer model.ProgramArtifact
		if err = json.Unmarshal(data, &newer); err != nil {
			return out, err
		}
		if newer.SupersedesID == artifact.ID && newer.Revision <= current.Number {
			return out, programError("stale_revision", "question artifact was superseded")
		}
	}
	if err = programValidateRefTx(ctx, tx, id, current.Number, model.ProgramInputRef{Kind: model.ArtifactQuestions, ID: artifact.ID, SHA: artifact.ContentSHA, Revision: artifact.Revision}); err != nil {
		return out, err
	}
	raw, err = s.blobs.Read(ctx, artifact.ContentSHA, 8<<20)
	if err != nil {
		return out, err
	}
	var questions model.QuestionsPayload
	if err = model.StrictDecode(raw, &questions); err != nil {
		return out, err
	}
	found := false
	for _, question := range questions.Questions {
		if question.ID == answer.QuestionID {
			if question.Route != "operator" {
				return out, programError("question_unresolved", "question is not routed to the operator")
			}
			found = true
		}
	}
	if !found {
		return out, programError("question_unresolved", "question does not exist in the exact artifact")
	}
	answer.SupportingRefs = append([]model.ProgramInputRef(nil), answer.SupportingRefs...)
	sort.Slice(answer.SupportingRefs, func(i, j int) bool {
		return programHash(answer.SupportingRefs[i]) < programHash(answer.SupportingRefs[j])
	})
	for _, ref := range answer.SupportingRefs {
		if err = programValidateRefTx(ctx, tx, id, current.Number, ref); err != nil {
			return out, err
		}
	}
	answer.ProgramID, answer.RevisionHash = id, revisionHash
	answer.CreatedAt = time.Time{}
	previous, err := programEvidenceDataTx(ctx, tx, "program_answers", id)
	if err != nil {
		return out, err
	}
	// Only the latest answer to this exact immutable question can be replayed.
	for i := len(previous) - 1; i >= 0; i-- {
		var old model.ProgramAnswer
		if err = json.Unmarshal(previous[i], &old); err != nil {
			return out, err
		}
		if old.QuestionArtifactID != answer.QuestionArtifactID || old.QuestionID != answer.QuestionID {
			continue
		}
		old.RevisionHash, old.CreatedAt = revisionHash, time.Time{}
		replay := answer
		// Schema-3 answers recorded a free-form local actor label. A trusted
		// local retry may recognize that immutable content without rewriting its
		// historical provenance or accepting an actor override for new answers.
		if audit.Kind == model.AuditTrustedLocal && old.Audit.Kind == "" {
			replay.Actor = old.Actor
		}
		if model.ProgramAnswerSHA(old) == model.ProgramAnswerSHA(replay) {
			return current, nil
		}
		break
	}
	answerID := model.ProgramAnswerSHA(answer)
	answer.CreatedAt = time.Now().UTC()
	raw, err = json.Marshal(answer)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_answers(id,program_id,data) VALUES(?,?,?)`, answerID, id, raw); err != nil {
		return out, err
	}
	out = model.ProgramRevision{ProgramID: id, Number: current.Number + 1, Spec: current.Spec, SeedManifestSHA: current.SeedManifestSHA, Supersedes: current.Number, Reason: "answer", CreatedAt: time.Now().UTC()}
	out.RevisionHash = programHash(struct {
		ProgramID       string
		Number          int
		Spec            model.ProgramSpec
		SeedManifestSHA string
		Predecessor     string
		AnswerID        string
	}{id, out.Number, out.Spec, out.SeedManifestSHA, current.RevisionHash, answerID})
	raw, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_revisions(program_id,number,revision_hash,data) VALUES(?,?,?,?)`, id, out.Number, out.RevisionHash, raw); err != nil {
		return out, err
	}
	p.CurrentRevision, p.State, p.Phase = out.Number, model.ProgramDraft, model.StepResearch
	p.Wait = &model.WaitReason{Code: "approval_required", Detail: "changed operator answer requires exact revision approval"}
	// Iteration and the lifetime budget are intentionally not reset.
	if err = programPut(ctx, tx, p); err != nil {
		return out, err
	}
	if err = programStopRevisionFeaturesTx(ctx, tx, id, current.Number); err != nil {
		return out, err
	}
	if err = programEvent(ctx, tx, id, "answered", answerID); err != nil {
		return out, err
	}
	if err = recordAuditTx(ctx, tx, "program", id, "answered", audit); err != nil {
		return out, err
	}
	return out, nil
}

// Read and close before any dependent query: the Store has a sole connection.
func programEvidenceDataTx(ctx context.Context, tx *sql.Tx, table, id string) ([][]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM `+table+` WHERE program_id=? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		out = append(out, data)
	}
	return out, rows.Err()
}

func programStopRevisionFeaturesTx(ctx context.Context, tx *sql.Tx, id string, revision int) error {
	data, err := programEvidenceDataTx(ctx, tx, "program_steps", id)
	if err != nil {
		return err
	}
	features := make(map[string]bool)
	for _, raw := range data {
		var step model.ProgramStep
		if err = json.Unmarshal(raw, &step); err != nil {
			return err
		}
		if step.Revision == revision {
			features[step.FeatureID] = true
		}
	}
	data, err = programEvidenceDataTx(ctx, tx, "program_feature_links", id)
	if err != nil {
		return err
	}
	for _, raw := range data {
		var link model.ProgramFeatureLink
		if err = json.Unmarshal(raw, &link); err != nil {
			return err
		}
		if link.Revision == revision {
			features[link.FeatureID] = true
		}
	}
	for featureID := range features {
		if err = programBlockFeatureTx(ctx, tx, id, featureID, "stale_revision", false); err != nil {
			return err
		}
	}
	return nil
}

// Cancellation changes queue intent only. Attempt rows and operation ownership
// remain active until the ordinary cancellation/reconciliation path settles them.
func programBlockFeatureTx(ctx context.Context, tx *sql.Tx, programID, featureID, code string, impact bool) error {
	f, err := get(ctx, tx, featureID)
	if err != nil {
		return err
	}
	if f.State == "accepted" || f.State == "delivered" || f.State == "reviewed" {
		if impact {
			if err = event(ctx, tx, f.ID, "impact_finding", code); err != nil {
				return err
			}
			return programEvent(ctx, tx, programID, "impact_finding", f.ID+":"+code)
		}
		return nil
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, f.ID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		f.State = "cancelling"
	} else if f.State != "cancelled" {
		f.State, f.PolicyHash = "blocked", ""
	}
	f.Reason = code
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	return event(ctx, tx, f.ID, code, "program evidence authority revoked")
}

type programInvalidationGraph struct {
	edges map[string]map[string]bool
	known map[string]bool
}

func (g *programInvalidationGraph) edge(input, output string) {
	if input == "" || output == "" {
		return
	}
	if g.edges[input] == nil {
		g.edges[input] = make(map[string]bool)
	}
	g.edges[input][output] = true
}
func (g *programInvalidationGraph) identity(values ...string) {
	for _, a := range values {
		if a == "" {
			continue
		}
		g.known[a] = true
		for _, b := range values {
			g.edge(a, b)
		}
	}
}
func (g *programInvalidationGraph) refs(refs []model.ProgramInputRef, output string) {
	for _, ref := range refs {
		g.edge(ref.ID, output)
		g.edge(ref.SHA, output)
	}
}

// Build dependencies from immutable envelopes and their typed payloads; no
// delivered feature, source capture, receipt, or artifact is rewritten.
func (s *Store) programInvalidationGraphTx(ctx context.Context, tx *sql.Tx, id string) (*programInvalidationGraph, error) {
	g := &programInvalidationGraph{edges: make(map[string]map[string]bool), known: make(map[string]bool)}
	data, err := programEvidenceDataTx(ctx, tx, "program_sources", id)
	if err != nil {
		return nil, err
	}
	for _, raw := range data {
		var source model.SourceCapture
		if err = json.Unmarshal(raw, &source); err != nil {
			return nil, err
		}
		g.identity(source.ID, source.ContentSHA256, source.TextSHA256)
	}
	data, err = programEvidenceDataTx(ctx, tx, "program_manifests", id)
	if err != nil {
		return nil, err
	}
	for _, raw := range data {
		var manifest model.ProgramManifest
		if err = json.Unmarshal(raw, &manifest); err != nil {
			return nil, err
		}
		g.identity(manifest.SHA)
		g.refs(manifest.Inputs, manifest.SHA)
	}
	data, err = programEvidenceDataTx(ctx, tx, "program_artifacts", id)
	if err != nil {
		return nil, err
	}
	for _, raw := range data {
		var a model.ProgramArtifact
		if err = json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		g.identity(a.ID, a.ContentSHA)
		g.edge(a.InputManifestSHA, a.ID)
		g.edge(a.EvaluatedManifestSHA, a.ID)
		payload, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
		if e != nil {
			return nil, e
		}
		if e = programArtifactDependencies(g, a, payload); e != nil {
			return nil, e
		}
	}
	data, err = programEvidenceDataTx(ctx, tx, "program_answers", id)
	if err != nil {
		return nil, err
	}
	for _, raw := range data {
		var answer model.ProgramAnswer
		if err = json.Unmarshal(raw, &answer); err != nil {
			return nil, err
		}
		answerID := model.ProgramAnswerSHA(answer)
		g.edge(answer.QuestionArtifactID, answerID)
		g.edge(answer.QuestionArtifactSHA, answerID)
		g.refs(answer.SupportingRefs, answerID)
	}
	data, err = programEvidenceDataTx(ctx, tx, "program_experiment_intents", id)
	if err != nil {
		return nil, err
	}
	for _, raw := range data {
		var request model.ExperimentRunRequest
		if err = json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		g.identity(request.RunID)
		g.edge(request.ContractSHA, request.RunID)
		g.edge(request.Bundle.ManifestSHA, request.RunID)
		g.refs(request.Contract.Environment.DatasetRefs, request.RunID)
	}
	data, err = programEvidenceDataTx(ctx, tx, "program_experiments", id)
	if err != nil {
		return nil, err
	}
	for _, raw := range data {
		var receipt model.ExperimentReceipt
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return nil, err
		}
		g.identity(receipt.RunID, programHash(receipt))
		g.edge(receipt.ContractSHA, receipt.RunID)
		g.edge(receipt.BundleManifestSHA, receipt.RunID)
		for _, observation := range receipt.Observations {
			g.edge(observation.SourceSHA, receipt.RunID)
		}
		for _, file := range receipt.Artifacts.Files {
			g.identity(file.ContentSHA)
			g.edge(file.ContentSHA, receipt.RunID)
		}
	}
	return g, nil
}

func programArtifactDependencies(g *programInvalidationGraph, a model.ProgramArtifact, raw []byte) error {
	semantic := func(id string) { g.edge(a.ID, id) }
	dependencies := func(ids []string) {
		for _, id := range ids {
			g.edge(id, a.ID)
		}
	}
	switch a.Kind {
	case model.ArtifactClaims:
		var p model.ClaimsPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, claim := range p.Claims {
			semantic(claim.ID)
			dependencies(claim.InferencePremises)
			for _, citation := range claim.Citations {
				g.edge(citation.SourceID, a.ID)
				g.edge(citation.TextSHA, a.ID)
			}
		}
	case model.ArtifactRequirements:
		var p model.RequirementsPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, r := range p.Requirements {
			semantic(r.ID)
			dependencies(r.ClaimRefs)
			dependencies(r.QuestionRefs)
		}
	case model.ArtifactQuestions:
		var p model.QuestionsPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, q := range p.Questions {
			semantic(q.ID)
		}
	case model.ArtifactArchitecture:
		var p model.ArchitecturePayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, d := range p.Decisions {
			semantic(d.ID)
			dependencies(d.RequirementRefs)
			dependencies(d.QuestionRefs)
			dependencies(d.ExperimentRefs)
		}
	case model.ArtifactExperimentContracts:
		var p model.ExperimentContractsPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, c := range p.Experiments {
			semantic(c.ID)
			sha := programHash(c)
			g.identity(sha)
			g.edge(a.ID, sha)
			g.edge(sha, a.ID)
			g.edge(c.BundleManifestSHA, a.ID)
			g.refs(c.Environment.DatasetRefs, a.ID)
			dependencies(c.QuestionRefs)
			dependencies(c.DecisionRefs)
		}
	case model.ArtifactEvaluation:
		var p model.EvaluationPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		g.edge(p.EvaluatedManifestSHA, a.ID)
		for _, f := range p.Findings {
			g.refs(f.Refs, a.ID)
		}
		for _, c := range p.Coverage {
			g.refs(c.EvidenceRefs, a.ID)
		}
	case model.ArtifactImplementationContracts:
		var p model.ImplementationContractsPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		for _, c := range p.Contracts {
			dependencies(c.RequirementRefs)
			dependencies(c.DecisionRefs)
			g.refs(c.EvidenceRefs, a.ID)
		}
	case model.ArtifactRevision:
		var p model.RevisionPayload
		if err := model.StrictDecode(raw, &p); err != nil {
			return err
		}
		g.refs(p.FalsifiedEvidence, a.ID)
		g.edge(p.PredecessorManifestSHA, a.ID)
	}
	return nil
}

func (s *Store) InvalidateProgramEvidence(ctx context.Context, id, subject, reason string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.invalidateProgramEvidenceTx(ctx, tx, id, subject, reason, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) invalidateProgramEvidenceTx(ctx context.Context, tx *sql.Tx, id, subject, reason string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(reason) == "" {
		return programError("invalid_contract", "invalidation requires an exact subject and reason")
	}
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	graph, err := s.programInvalidationGraphTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if !graph.known[subject] {
		return programError("evidence_invalid", "subject does not belong to this program")
	}
	affected := make(map[string]bool)
	queue := []string{subject}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if affected[current] {
			continue
		}
		affected[current] = true
		for dependent := range graph.edges[current] {
			if !affected[dependent] {
				queue = append(queue, dependent)
			}
		}
	}
	subjects := make([]string, 0, len(affected))
	for item := range affected {
		subjects = append(subjects, item)
	}
	sort.Strings(subjects)
	for _, item := range subjects {
		invalidation := model.ProgramInvalidation{ID: ID(), ProgramID: id, Subject: item, Reason: reason, Actor: programAuditActor(audit), CreatedAt: time.Now().UTC()}
		raw, e := json.Marshal(invalidation)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_invalidations(id,program_id,subject,data) VALUES(?,?,?,?)`, invalidation.ID, id, item, raw); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM program_grants WHERE program_id=? AND subject_hash=?`, id, item); err != nil {
			return err
		}
		if err = programEvent(ctx, tx, id, "evidence_invalidated", item+":"+reason); err != nil {
			return err
		}
	}
	features := make(map[string]bool)
	steps, err := programEvidenceDataTx(ctx, tx, "program_steps", id)
	if err != nil {
		return err
	}
	for _, raw := range steps {
		var step model.ProgramStep
		if err = json.Unmarshal(raw, &step); err != nil {
			return err
		}
		if affected[step.InputManifestSHA] || affected[step.OutputManifestSHA] || affected[step.SubjectID] {
			features[step.FeatureID] = true
			if step.State != "completed" {
				step.State, step.UpdatedAt = "blocked", time.Now().UTC()
				data, e := json.Marshal(step)
				if e != nil {
					return e
				}
				if _, err = tx.ExecContext(ctx, `UPDATE program_steps SET data=? WHERE id=?`, data, step.ID); err != nil {
					return err
				}
			}
		}
	}
	// Inspect actual gates as well as publication links. A feature's immutable
	// evidence gate remains the authority even if a link is absent or historical.
	rows, err := tx.QueryContext(ctx, `SELECT data FROM features`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		var f model.Feature
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &f); err != nil {
			rows.Close()
			return err
		}
		if gate := f.ProgramEvidenceGate; gate != nil && gate.ProgramID == id && (affected[gate.ManifestSHA] || affected[gate.EvaluationArtifactID]) {
			features[f.ID] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	links, err := programEvidenceDataTx(ctx, tx, "program_feature_links", id)
	if err != nil {
		return err
	}
	for _, raw := range links {
		var link model.ProgramFeatureLink
		if err = json.Unmarshal(raw, &link); err != nil {
			return err
		}
		if affected[link.ManifestSHA] || affected[link.ContractArtifactID] {
			features[link.FeatureID] = true
		}
	}
	for featureID := range features {
		if err = programBlockFeatureTx(ctx, tx, id, featureID, "evidence_invalid", true); err != nil {
			return err
		}
	}
	if p.State != model.ProgramCancelled && p.State != model.ProgramProposed {
		p.State = model.ProgramBlocked
		p.Wait = &model.WaitReason{Code: "evidence_invalid", Detail: subject + ":" + reason}
		if err = programPut(ctx, tx, p); err != nil {
			return err
		}
	}
	return recordAuditTx(ctx, tx, "program", id, "evidence_invalidated", audit)
}
