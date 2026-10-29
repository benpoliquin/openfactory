package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

const resumeWorkerPresentation = "One current settled failed worker is selected for retry under its existing authority."
const resumePrerequisitePresentation = "No failed worker is selected; resume continues only previously approved Program work and prerequisites."

func intakeResumeWait(work bool) *model.WaitReason {
	detail := resumePrerequisitePresentation
	if work {
		detail = resumeWorkerPresentation
	}
	return &model.WaitReason{Code: "prerequisite_missing", Detail: detail}
}

// Runtime failure strings can contain internal paths or capabilities. Resume
// authority presents a fixed description and the one safe typed target, never
// the internal Feature, Attempt, runner receipt, or copied failure prompt.
func (s *Store) intakeResumeReviewTx(ctx context.Context, tx *sql.Tx, p model.Program, r model.ProgramRevision, cfg model.Config, template *model.IntakeReview, claims *model.ReviewClaims, out *model.IntakeReview, m *intakeReviewManifest) error {
	selection, err := s.programResumeWorkTx(ctx, tx, p, r, cfg)
	if err != nil {
		var domain *model.ProgramError
		if template != nil && errors.As(err, &domain) {
			switch domain.Code {
			case "attempt_unsettled", "stale_subject", "budget_exhausted", "approval_required", "operation_uncertain":
				return programError("stale_revision", "reviewed resume work changed")
			}
		}
		return err
	}
	p.CreatedAt, p.UpdatedAt = time.Time{}, time.Time{}
	p.Wait = intakeResumeWait(selection != nil)
	m.Program = &p
	if selection == nil {
		return nil
	}
	work := selection.Work
	if work == nil {
		return errors.New("Program resume selection lacks safe authority")
	}
	out.ResumeWork = work
	m.ResumeWork = work
	claims.InputManifestSHA = work.InputManifestSHA
	if err = s.intakeReviewEvidenceTx(ctx, tx, p.ID, r.Number, work.InputManifestSHA, m, map[string]bool{}); err != nil {
		return err
	}
	if work.StepKind != model.StepExperimentRun {
		if selection.Artifact != nil || selection.Experiment != nil || len(selection.ArtifactCanonical) != 0 {
			return errors.New("model worker resume contains execution authority")
		}
		return nil
	}
	if selection.Artifact == nil || selection.Experiment == nil {
		return errors.New("experiment resume lacks frozen contract material")
	}
	artifact, contract := selection.Artifact, selection.Experiment
	canonical, err := model.ExperimentContractCanonical(*contract)
	if err != nil {
		return err
	}
	if artifact.ProgramID != p.ID || artifact.Revision != r.Number || artifact.Kind != model.ArtifactExperimentContracts || intake.SHA256(selection.ArtifactCanonical) != artifact.ContentSHA || intake.SHA256(canonical) != work.SubjectID || contract.BundleManifestSHA != work.BundleManifestSHA {
		return errors.New("experiment resume review differs from frozen authority")
	}
	m.SelectedArtifact = artifact
	m.SelectedExperiment = contract
	out.SelectedArtifactCanonical = string(selection.ArtifactCanonical)
	// As for explicit experiment approval, the effect-specific payload is the
	// frozen contract; canonical_source/source_sha still bind the Program spec.
	out.CanonicalPayload = string(canonical)
	claims.CanonicalPayloadSHA = work.SubjectID
	claims.ExecutionPolicySHA = work.ExecutionPolicySHA
	m.ExecutionPolicySHA = work.ExecutionPolicySHA
	m.BundleManifestSHAs = append(m.BundleManifestSHAs, work.BundleManifestSHA)
	m.Evidence = append(m.Evidence,
		model.ProgramInputRef{Kind: model.ArtifactExperimentContracts, ID: artifact.ID, SHA: artifact.ContentSHA, Revision: artifact.Revision},
		model.ProgramInputRef{Kind: "bundle", ID: work.BundleManifestSHA, SHA: work.BundleManifestSHA, Revision: work.StepRevision},
	)
	if !work.PriorLaunchAbsent {
		m.Evidence = append(m.Evidence, model.ProgramInputRef{Kind: "experiment_receipt", ID: work.PriorRunID, SHA: work.PriorReceiptSHA, Revision: work.StepRevision})
	}
	return nil
}

func intakeResumeSame(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

func intakeResumeHasRef(refs []model.ProgramInputRef, kind, id, sha string, revision int) bool {
	for _, ref := range refs {
		if ref.Kind == kind && ref.ID == id && ref.SHA == sha && ref.Revision == revision {
			return true
		}
	}
	return false
}

// This checks the public projection against its exact canonical material. The
// selector separately proves currentness and settlement from private records.
func intakeValidateResumeMaterial(r model.IntakeReview, m intakeReviewManifest) error {
	invalid := func() error { return errors.New("Program resume review material integrity mismatch") }
	if !intakeResumeSame(r.ResumeWork, m.ResumeWork) {
		return invalid()
	}
	if r.Command.Action != model.ActionProgramResume {
		if r.ResumeWork != nil {
			return invalid()
		}
		return nil
	}
	if r.Claims.SubjectKind != "program" || r.ProgramRevision == nil || m.Program == nil || r.ProgramRevision.ProgramID != r.Claims.SubjectID || r.ProgramRevision.RevisionHash != r.Claims.SubjectRevision || m.Program.ID != r.Claims.SubjectID || m.Program.CurrentRevision != r.ProgramRevision.Number || !intakeResumeSame(m.Program.Wait, intakeResumeWait(r.ResumeWork != nil)) {
		return invalid()
	}
	work := r.ResumeWork
	if work == nil {
		if r.Claims.InputManifestSHA != "" || r.Claims.ExecutionPolicySHA != "" || m.SelectedArtifact != nil || m.SelectedExperiment != nil {
			return invalid()
		}
		return nil
	}
	settled := work.AttemptState == "blocked" && work.AttemptStage == "failed"
	if work.StepKind == model.StepExperimentRun && work.AttemptState == "failed" && work.AttemptStage == "operator_reconciled_failure" {
		settled = true
	}
	if work.StepID == "" || work.StepRevision != r.ProgramRevision.Number || expectedProgramArtifacts(work.StepKind) == nil || (work.StepState != "ready" && work.StepState != "blocked") || work.Iteration < m.Program.Iteration || work.FeatureID != "openfactory-discovery-"+work.StepID || !intakeAuthHashValid(work.FeatureRevision) || work.FeatureState != "blocked" || work.FeatureAttempts < 1 || work.FeatureMaxAttempts <= work.FeatureAttempts || work.AttemptID == "" || work.AttemptActive || !settled || !intakeAuthHashValid(work.AttemptOutputSHA) || !intakeAuthHashValid(work.InputManifestSHA) || r.Claims.InputManifestSHA != work.InputManifestSHA {
		return invalid()
	}
	if work.StepKind != model.StepExperimentRun {
		if work.SubjectID != "" || work.ExecutionPolicySHA != "" || work.BundleManifestSHA != "" || work.PriorRunID != "" || work.PriorRequestSHA != "" || work.PriorReceiptSHA != "" || work.PriorReceiptState != "" || work.PriorReceiptConclusive || work.PriorLaunchAbsent || work.PriorExecutionOutcome != "" || work.PriorObservationOutcome != "" || work.PriorHypothesisOutcome != "" || r.Claims.ExecutionPolicySHA != "" || m.SelectedArtifact != nil || m.SelectedExperiment != nil {
			return invalid()
		}
		return nil
	}
	if !intakeAuthHashValid(work.SubjectID) || !intakeAuthHashValid(work.ExecutionPolicySHA) || !intakeAuthHashValid(work.BundleManifestSHA) || r.Claims.CanonicalPayloadSHA != work.SubjectID || r.Claims.ExecutionPolicySHA != work.ExecutionPolicySHA || m.ExecutionPolicySHA != work.ExecutionPolicySHA || m.SelectedArtifact == nil || m.SelectedExperiment == nil || m.SelectedArtifact.Kind != model.ArtifactExperimentContracts || m.SelectedArtifact.ProgramID != r.Claims.SubjectID || m.SelectedArtifact.Revision != work.StepRevision || m.SelectedExperiment.BundleManifestSHA != work.BundleManifestSHA {
		return invalid()
	}
	if !intakeResumeHasRef(m.Evidence, model.ArtifactExperimentContracts, m.SelectedArtifact.ID, m.SelectedArtifact.ContentSHA, m.SelectedArtifact.Revision) || !intakeResumeHasRef(m.Evidence, "bundle", work.BundleManifestSHA, work.BundleManifestSHA, work.StepRevision) {
		return invalid()
	}
	bundle := false
	for _, sha := range r.Claims.BundleManifestSHAs {
		if sha == work.BundleManifestSHA {
			bundle = true
		}
	}
	if !bundle {
		return invalid()
	}
	if work.PriorLaunchAbsent {
		if work.AttemptState != "blocked" || work.AttemptStage != "failed" || work.PriorRunID != "" || work.PriorRequestSHA != "" || work.PriorReceiptSHA != "" || work.PriorReceiptState != "" || work.PriorReceiptConclusive || work.PriorExecutionOutcome != "" || work.PriorObservationOutcome != "" || work.PriorHypothesisOutcome != "" {
			return invalid()
		}
		return nil
	}
	if work.PriorRunID == "" || !intakeAuthHashValid(work.PriorRequestSHA) || !intakeAuthHashValid(work.PriorReceiptSHA) || !intakeResumeHasRef(m.Evidence, "experiment_receipt", work.PriorRunID, work.PriorReceiptSHA, work.StepRevision) {
		return invalid()
	}
	switch work.PriorReceiptState {
	case "failed", "cancelled", "waiting_prerequisite":
	case "resolved_failed":
		if work.AttemptState != "failed" || work.AttemptStage != "operator_reconciled_failure" {
			return invalid()
		}
	case "completed":
		if work.PriorReceiptConclusive {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

// A Program resume grant may retry only its separately bound internal Feature.
// The root Program claim is not a wildcard Feature retry authorization.
func intakeResumeFeatureAuditTx(ctx context.Context, tx *sql.Tx, id string, approval model.ApprovalReceipt) error {
	var review model.IntakeReview
	if err := intakeAuthLoad(ctx, tx, "intake_reviews", approval.ReviewID, &review); err != nil {
		return err
	}
	if err := validateIntakeReviewMaterial(review); err != nil {
		return err
	}
	sha, err := intakeReviewSHA(review.Claims)
	if err != nil {
		return err
	}
	if !intakeResumeSame(review.Claims, approval.Claims) {
		return errors.New("Program resume approval claims integrity mismatch")
	}
	work := review.ResumeWork
	if sha != review.ClaimsSHA || review.ClaimsSHA != approval.ClaimsSHA || review.Claims.Action != model.ActionProgramResume || review.Command.Action != model.ActionProgramResume || approval.Claims.SubjectKind != "program" || work == nil || work.FeatureID != id || work.AttemptActive {
		return ErrAuthRequired
	}
	p, err := programLoad(ctx, tx, approval.Claims.SubjectID)
	if err != nil {
		return err
	}
	if p.CurrentRevision != work.StepRevision || (p.State != model.ProgramActive && p.State != model.ProgramWaiting) {
		return ErrAuthRequired
	}
	r, err := programRevision(ctx, tx, p.ID, p.CurrentRevision)
	if err != nil {
		return err
	}
	if r.RevisionHash != approval.Claims.SubjectRevision {
		return ErrAuthRequired
	}
	step, err := programStepTx(ctx, tx, work.StepID)
	if err != nil {
		return err
	}
	if step.ProgramID != approval.Claims.SubjectID || step.Revision != work.StepRevision || step.Kind != work.StepKind || step.Iteration != work.Iteration || step.InputManifestSHA != work.InputManifestSHA || step.FeatureID != id || step.SubjectID != work.SubjectID {
		return ErrAuthRequired
	}
	f, err := get(ctx, tx, id)
	if err != nil {
		return err
	}
	if f.DiscoveryStepID != step.ID || f.Revision != work.FeatureRevision || f.PolicyHash != approval.Claims.PolicySHA || f.State != "ready" || f.Attempts != work.FeatureAttempts {
		return ErrAuthRequired
	}
	var latest string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM attempts WHERE feature_id=? ORDER BY rowid DESC LIMIT 1`, id).Scan(&latest); err != nil {
		return err
	}
	if latest != work.AttemptID {
		return ErrAuthRequired
	}
	attempt, revision, policy, active, err := loadAttempt(ctx, tx, latest)
	if err != nil {
		return err
	}
	if active != 0 || attempt.FeatureID != id || revision != f.Revision || policy != f.PolicyHash || attempt.State != work.AttemptState || attempt.Stage != work.AttemptStage || !attempt.UpdatedAt.Equal(work.AttemptUpdatedAt) || intake.SHA256([]byte(attempt.Summary)) != work.AttemptOutputSHA {
		return ErrAuthRequired
	}
	return nil
}
