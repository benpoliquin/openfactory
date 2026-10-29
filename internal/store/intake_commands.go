package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

const intakeCommandSchema = `
CREATE TABLE IF NOT EXISTS intake_commands(operator_id TEXT NOT NULL REFERENCES intake_operators(id),command_id TEXT NOT NULL,digest TEXT NOT NULL,command BLOB NOT NULL,data BLOB NOT NULL,PRIMARY KEY(operator_id,command_id));
CREATE TABLE IF NOT EXISTS intake_audits(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,subject_kind TEXT NOT NULL,subject_id TEXT NOT NULL,action TEXT NOT NULL,data BLOB NOT NULL);
`

// CommandAuthorization is populated only after the maintained WebAuthn verifier
// succeeds. No part of this type is decoded from a browser command payload.
type CommandAuthorization struct {
	ChallengeID string
	Credential  model.OperatorCredential
	Evidence    model.WebAuthnEvidence
}

type intakeAuditRecord struct {
	ID          string             `json:"id"`
	SubjectKind string             `json:"subject_kind"`
	SubjectID   string             `json:"subject_id"`
	Action      string             `json:"action"`
	Audit       model.AuditContext `json:"audit"`
	CreatedAt   time.Time          `json:"created_at"`
}

func internalIntakeFeature(f model.Feature) bool {
	return f.IntakeStepID != "" || f.Kind == model.IntakeFeatureKind || strings.HasPrefix(f.ID, model.IntakeFeaturePrefix)
}

func validateAuditContext(a model.AuditContext) error {
	switch a.Kind {
	case model.AuditTrustedLocal:
		if a.OperatorID != "" || a.CredentialID != "" || a.CommandID != "" || a.ApprovalID != "" {
			return intakeError("invalid_contract", "trusted local audit cannot impersonate a verified operator")
		}
	case model.AuditVerifiedOperator:
		if a.OperatorID == "" || a.CredentialID == "" || a.CommandID == "" {
			return intakeError("invalid_contract", "verified operator audit is incomplete")
		}
	default:
		return intakeError("invalid_contract", "typed audit context is required")
	}
	return nil
}

func recordAuditTx(ctx context.Context, tx *sql.Tx, kind, id, action string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	if audit.Kind == model.AuditVerifiedOperator {
		if _, err := intakeAuthLiveOperator(ctx, tx, audit.OperatorID, ""); err != nil {
			return err
		}
		if _, err := intakeAuthLiveCredential(ctx, tx, audit.CredentialID, audit.OperatorID, time.Now().UTC()); err != nil {
			return err
		}
		if (action == "approved" || action == "resumed" || action == "retry_requested") && audit.ApprovalID == "" {
			return intakeError("human_verification_required", "fresh exact-command verification is required")
		}
		if audit.ApprovalID != "" {
			var approval model.ApprovalReceipt
			if err := intakeAuthLoad(ctx, tx, "intake_approval_receipts", audit.ApprovalID, &approval); err != nil {
				return err
			}
			if approval.Audit != audit {
				return ErrAuthRequired
			}
			if kind == "command" {
				if approval.Claims.CommandID != id || approval.Claims.Action != action {
					return ErrAuthRequired
				}
			} else {
				resumeRetry := kind == "feature" && action == "retry_requested" && approval.Claims.Action == model.ActionProgramResume
				if resumeRetry {
					if err := intakeResumeFeatureAuditTx(ctx, tx, id, approval); err != nil {
						return err
					}
				} else if approval.Claims.SubjectID != id || kind == "program" && approval.Claims.SubjectKind != "program" || kind == "feature" && approval.Claims.SubjectKind != "feature" && approval.Claims.SubjectKind != "plan" {
					return ErrAuthRequired
				}
				switch action {
				case "approved":
					permitted := approval.Claims.Action == model.ActionIntakeApproveQueue || kind == "feature" && approval.Claims.Action == model.ActionFeatureApprove || kind == "program" && (approval.Claims.Action == model.ActionProgramApproveRevision || approval.Claims.Action == model.ActionProgramApproveArchitecture || approval.Claims.Action == model.ActionProgramApproveExperiment)
					if !permitted {
						return ErrAuthRequired
					}
				case "retry_requested":
					if approval.Claims.Action != model.ActionSubjectRetry && approval.Claims.Action != model.ActionIntakeRetry && !resumeRetry {
						return ErrAuthRequired
					}
				case "resumed":
					if approval.Claims.Action != model.ActionProgramResume {
						return ErrAuthRequired
					}
				}
			}
		}
	}
	record := intakeAuditRecord{ID: ID(), SubjectKind: kind, SubjectID: id, Action: action, Audit: audit, CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_audits(id,subject_kind,subject_id,action,data) VALUES(?,?,?,?,?)`, record.ID, kind, id, action, data)
	return err
}

func operatorProjectTx(ctx context.Context, tx *sql.Tx, operatorID, project string) error {
	_, err := intakeAuthLiveOperator(ctx, tx, operatorID, project)
	return err
}

// commandProjectTx resolves only identity and immutable project membership. It
// deliberately performs no revision/freshness check: permanent command receipts
// must be returned before a repeated command's now-stale domain preconditions.
func commandProjectTx(ctx context.Context, tx *sql.Tx, command model.TypedCommand) (string, error) {
	payload, err := intake.CommandPayload(command)
	if err != nil {
		return "", err
	}
	if command.RequestID != "" {
		r, err := intakeRequestTx(ctx, tx, command.RequestID)
		return r.Project, err
	}
	switch p := payload.(type) {
	case *model.RequestCreatePayload:
		return p.Project, nil
	case *model.SubjectCommandPayload:
		if p.Kind == "program" {
			program, err := programLoad(ctx, tx, p.ID)
			return program.Project, err
		}
		f, err := get(ctx, tx, p.ID)
		if err != nil {
			return "", err
		}
		if internalIntakeFeature(f) || f.DiscoveryStepID != "" {
			return "", ErrNotFound
		}
		return f.Project, nil
	case *model.ProgramArtifactCommandPayload:
		program, err := programLoad(ctx, tx, p.ProgramID)
		return program.Project, err
	case *model.ProgramAnswerPayload:
		program, err := programLoad(ctx, tx, p.ProgramID)
		return program.Project, err
	case *model.PlanningAnswerPayload:
		f, err := get(ctx, tx, p.FeatureID)
		if err != nil {
			return "", err
		}
		if f.Kind != "plan" || internalIntakeFeature(f) {
			return "", ErrNotFound
		}
		return f.Project, nil
	}
	return "", intakeError("unsupported_action", "unsupported command action")
}

func intakeCommandOwnerTx(ctx context.Context, tx *sql.Tx, command model.TypedCommand, operatorID string) error {
	if command.RequestID != "" {
		r, err := intakeRequestTx(ctx, tx, command.RequestID)
		if err != nil {
			return err
		}
		if r.OperatorID != operatorID {
			return ErrProjectForbidden
		}
		return nil
	}
	payload, err := intake.CommandPayload(command)
	if err != nil {
		return err
	}
	var kind, id string
	switch p := payload.(type) {
	case *model.SubjectCommandPayload:
		kind, id = p.Kind, p.ID
	case *model.ProgramArtifactCommandPayload:
		kind, id = "program", p.ProgramID
	case *model.ProgramAnswerPayload:
		kind, id = "program", p.ProgramID
	case *model.PlanningAnswerPayload:
		kind, id = "plan", p.FeatureID
	case *model.RequestCreatePayload:
		return nil
	}
	return intakeSubjectOwnerTx(ctx, tx, operatorID, kind, id)
}

func commandReceiptTx(ctx context.Context, q intakeAuthQuery, operatorID, commandID string) (model.CommandReceipt, error) {
	var out model.CommandReceipt
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM intake_commands WHERE operator_id=? AND command_id=?`, operatorID, commandID).Scan(&data)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	return out, err
}

// GetCommandReceipt is a controller-internal lookup. HTTP callers authenticate
// current session/access before using it; ExecuteCommand repeats that check.
func (s *Store) GetCommandReceipt(ctx context.Context, operatorID, commandID string) (model.CommandReceipt, error) {
	var receipt model.CommandReceipt
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT command FROM intake_commands WHERE operator_id=? AND command_id=?`, operatorID, commandID).Scan(&data); err != nil {
		return receipt, err
	}
	var command model.TypedCommand
	if err = json.Unmarshal(data, &command); err != nil {
		return receipt, err
	}
	project, err := commandProjectTx(ctx, tx, command)
	if err != nil {
		return receipt, err
	}
	if err = operatorProjectTx(ctx, tx, operatorID, project); err != nil {
		return receipt, err
	}
	if err = intakeCommandOwnerTx(ctx, tx, command, operatorID); err != nil {
		return receipt, err
	}
	receipt, err = commandReceiptTx(ctx, tx, operatorID, commandID)
	if err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}

// ExecuteCommand is the sole browser mutation boundary. Authentication,
// permanent deduplication, challenge consumption, library credential state,
// domain changes, approval/audit and recorded result share one write transaction.
func (s *Store) ExecuteCommand(ctx context.Context, tokenSHA, csrfSHA string, command model.TypedCommand, cfg model.Config, proof *CommandAuthorization) (model.CommandReceipt, error) {
	var receipt model.CommandReceipt
	if !cfg.Intake.Enabled {
		return receipt, intakeError("unsupported_action", "intake is disabled")
	}
	digest, err := intake.CommandDigest(command)
	if err != nil {
		return receipt, intakeError("invalid_request", "invalid command contract")
	}
	if csrfSHA == "" {
		return receipt, ErrCSRF
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	session, operator, credential, err := intakeAuthenticateTx(ctx, tx, tokenSHA, csrfSHA, "", now)
	if err != nil {
		return receipt, err
	}
	project, err := commandProjectTx(ctx, tx, command)
	if err != nil {
		return receipt, err
	}
	if _, ok := cfg.Projects[project]; !ok {
		return receipt, ErrProjectForbidden
	}
	if err = operatorProjectTx(ctx, tx, operator.ID, project); err != nil {
		return receipt, err
	}
	if err = intakeCommandOwnerTx(ctx, tx, command, operator.ID); err != nil {
		return receipt, err
	}
	existing, err := commandReceiptTx(ctx, tx, operator.ID, command.CommandID)
	if err == nil {
		if existing.Digest != digest {
			return receipt, intakeError("idempotency_conflict", "command ID already has a different body")
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	audit := model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: operator.ID, CredentialID: credential.ID, CommandID: command.CommandID}
	if intake.RequiresHumanVerification(command.Action) {
		if proof == nil || proof.ChallengeID == "" || !proof.Evidence.UserPresent || !proof.Evidence.UserVerified || proof.Evidence.ChallengeID != proof.ChallengeID || proof.Evidence.CredentialID != credential.ID || proof.Credential.ID != credential.ID || proof.Credential.OperatorID != operator.ID || !intakeAuthHashValid(proof.Evidence.AssertionSHA) {
			return receipt, intakeError("human_verification_required", "fresh exact-command verification is required")
		}
		var challenge model.AuthChallenge
		if err = intakeAuthLoad(ctx, tx, "intake_auth_challenges", proof.ChallengeID, &challenge); err != nil {
			return receipt, err
		}
		if challenge.CommandID != command.CommandID || challenge.CommandDigest != digest || challenge.SessionID != session.ID || challenge.OperatorID != operator.ID || challenge.CredentialID != credential.ID {
			return receipt, ErrAuthRequired
		}
		if proof.Evidence.SignCount != proof.Credential.SignCount || proof.Evidence.VerifiedAt.IsZero() || proof.Evidence.VerifiedAt.Before(challenge.CreatedAt) || proof.Evidence.VerifiedAt.After(now) || !proof.Evidence.VerifiedAt.Before(challenge.ExpiresAt) {
			return receipt, ErrAuthRequired
		}
		review, err := intakeVerifyReviewTx(ctx, tx, challenge.ReviewID, operator.ID, credential.ID, digest, now)
		if err != nil {
			return receipt, err
		}
		current, err := s.reviewIntakeCommandTx(ctx, tx, command, cfg, session, operator, credential, &review)
		if err != nil {
			return receipt, err
		}
		if review.Claims.PolicySHA != cfg.PolicyHash {
			return receipt, intakeError("stale_policy", "execution policy changed")
		}
		if current.ClaimsSHA != review.ClaimsSHA {
			return receipt, intakeError("stale_revision", "reviewed subject or manifest changed")
		}
		if _, err = intakeConsumeCommandChallengeTx(ctx, tx, proof.ChallengeID, session.ID, digest, proof.Credential, now); err != nil {
			return receipt, err
		}
		audit.ApprovalID = ID()
		approval := model.ApprovalReceipt{ID: audit.ApprovalID, ReviewID: review.ID, Claims: review.Claims, ClaimsSHA: review.ClaimsSHA, Audit: audit, Evidence: proof.Evidence, CreatedAt: now}
		if err = intakeSaveApprovalTx(ctx, tx, approval); err != nil {
			return receipt, err
		}
	} else if proof != nil {
		return receipt, intakeError("invalid_request", "unexpected authority proof")
	}
	result, err := s.applyDomainCommandTx(ctx, tx, command, cfg, audit)
	if err != nil {
		return receipt, err
	}
	resultData, err := json.Marshal(result)
	if err != nil {
		return receipt, err
	}
	receipt = model.CommandReceipt{OperatorID: operator.ID, CommandID: command.CommandID, Digest: digest, Action: command.Action, State: "committed", Result: resultData, Audit: audit, ApprovalID: audit.ApprovalID, CreatedAt: now}
	data, err := json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	commandData, err := json.Marshal(command)
	if err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_commands(operator_id,command_id,digest,command,data) VALUES(?,?,?,?,?)`, operator.ID, command.CommandID, digest, commandData, data); err != nil {
		return receipt, err
	}
	if err = recordAuditTx(ctx, tx, "command", command.CommandID, command.Action, audit); err != nil {
		return receipt, err
	}
	if err = intakeTouchSessionTx(ctx, tx, session, now); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}

func (s *Store) applyDomainCommandTx(ctx context.Context, tx *sql.Tx, command model.TypedCommand, cfg model.Config, audit model.AuditContext) (any, error) {
	payload, err := intake.CommandPayload(command)
	if err != nil {
		return nil, err
	}
	switch command.Action {
	case model.ActionRequestCreate, model.ActionTurnAppend, model.ActionIntakeDraft, model.ActionDraftReplace, model.ActionIntakeCancel, model.ActionIntakeRetry, model.ActionIntakeApproveQueue:
		return s.applyIntakeCommandTx(ctx, tx, command, cfg, audit)
	case model.ActionFeatureApprove, model.ActionSubjectRetry, model.ActionSubjectCancel, model.ActionProgramApproveRevision, model.ActionProgramResume:
		p := payload.(*model.SubjectCommandPayload)
		if p.Kind == "program" {
			switch command.Action {
			case model.ActionSubjectCancel:
				err = s.cancelProgramTx(ctx, tx, p.ID, p.Revision, audit)
			case model.ActionProgramApproveRevision:
				err = s.approveProgramTx(ctx, tx, p.ID, p.Revision, cfg.PolicyHash, audit)
			case model.ActionProgramResume:
				err = s.resumeProgramTx(ctx, tx, p.ID, p.Revision, cfg, audit)
			default:
				err = intakeError("unsupported_action", "program requires its explicit resume action")
			}
			if err != nil {
				return nil, err
			}
			return programLoad(ctx, tx, p.ID)
		}
		f, err := get(ctx, tx, p.ID)
		if err != nil {
			return nil, err
		}
		if f.Revision != p.Revision {
			return nil, intakeError("stale_revision", "subject revision changed")
		}
		if internalIntakeFeature(f) || f.DiscoveryStepID != "" || p.Kind == "plan" && f.Kind != "plan" || p.Kind == "feature" && f.Kind == "plan" {
			return nil, intakeError("stale_subject", "subject kind does not match")
		}
		switch command.Action {
		case model.ActionFeatureApprove:
			err = s.approveFeatureTx(ctx, tx, p.ID, p.Revision, cfg.PolicyHash, audit)
		case model.ActionSubjectRetry:
			if f.PolicyHash != cfg.PolicyHash {
				return nil, intakeError("stale_policy", "fresh approval under current policy is required")
			}
			err = retryFeatureTx(ctx, tx, p.ID, audit)
		case model.ActionSubjectCancel:
			err = s.cancelFeatureTx(ctx, tx, p.ID, audit)
		default:
			err = intakeError("unsupported_action", "action does not match subject")
		}
		if err != nil {
			return nil, err
		}
		return get(ctx, tx, p.ID)
	case model.ActionProgramApproveArchitecture, model.ActionProgramApproveExperiment:
		p := payload.(*model.ProgramArtifactCommandPayload)
		program, err := programLoad(ctx, tx, p.ProgramID)
		if err != nil {
			return nil, err
		}
		revision, err := programRevision(ctx, tx, p.ProgramID, program.CurrentRevision)
		if err != nil {
			return nil, err
		}
		if revision.RevisionHash != p.Revision {
			return nil, intakeError("stale_revision", "program revision changed")
		}
		var artifact model.ProgramArtifact
		if err = intakeAuthLoad(ctx, tx, "program_artifacts", p.ArtifactID, &artifact); err != nil {
			return nil, err
		}
		if artifact.ProgramID != p.ProgramID || artifact.Revision != program.CurrentRevision || artifact.Kind != p.ArtifactKind || artifact.ContentSHA != p.ArtifactSHA {
			return nil, intakeError("stale_subject", "artifact binding changed")
		}
		if err = s.approveProgramArtifactTx(ctx, tx, p.ProgramID, p.Revision, model.ProgramArtifactKind(p.ArtifactKind), p.SubjectSHA, cfg.PolicyHash, audit); err != nil {
			return nil, err
		}
		return programLoad(ctx, tx, p.ProgramID)
	case model.ActionProgramAnswer:
		p := payload.(*model.ProgramAnswerPayload)
		answer := model.ProgramAnswer{QuestionArtifactID: p.QuestionArtifactID, QuestionArtifactSHA: p.QuestionArtifactSHA, QuestionID: p.QuestionID, Text: p.Text, SupportingRefs: p.SupportingRefs}
		return s.answerProgramQuestionTx(ctx, tx, p.ProgramID, p.RevisionHash, answer, audit)
	case model.ActionPlanningAnswer:
		p := payload.(*model.PlanningAnswerPayload)
		var q model.PlanningQuestion
		if err = intakeAuthLoad(ctx, tx, "planning_questions", p.QuestionID, &q); err != nil {
			return nil, err
		}
		if q.FeatureID != p.FeatureID || q.FeatureRevision != p.FeatureRevision || q.AttemptID != p.AttemptID || q.OutputSHA != p.OutputSHA {
			return nil, intakeError("stale_revision", "planning question binding changed")
		}
		return s.answerPlanningQuestionTx(ctx, tx, model.PlanningAnswer{Question: q, Text: p.Text, Audit: audit})
	}
	return nil, intakeError("unsupported_action", "unsupported command action")
}
