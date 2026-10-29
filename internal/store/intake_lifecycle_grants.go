package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"openfactory/internal/model"
)

// A durable drafting grant is distinct from both saved text and the mutable
// request state. Revoking a credential prevents new commands, not the bounded
// work this exact, already-committed grant authorized.
type intakeDraftAuthorization struct {
	RequestID      string                 `json:"request_id"`
	ContentVersion int                    `json:"content_version"`
	ManifestSHA    string                 `json:"manifest_sha"`
	Disclosure     model.IntakeDisclosure `json:"disclosure"`
	Audit          model.AuditContext     `json:"audit"`
	CreatedAt      time.Time              `json:"created_at"`
}

func intakeGrantDraftTx(ctx context.Context, tx *sql.Tx, r model.IntakeRequest, audit model.AuditContext) error {
	if err := intakeManifestIdentity(r.ContextManifest); err != nil {
		return err
	}
	if r.ContextManifest.RequestID != r.ID || r.ContextManifest.Project != r.Project || r.ContextManifest.ContentVersion != r.ContentVersion || r.ContextManifest.PolicySHA != r.ContextManifest.Disclosure.PolicySHA {
		return intakeError("invalid_contract", "draft context differs from saved request scope")
	}
	if audit.OperatorID != r.OperatorID || audit.CommandID == "" || audit.Kind != model.AuditVerifiedOperator {
		return intakeError("human_verification_required", "authenticated exact drafting consent is required")
	}
	grant := intakeDraftAuthorization{RequestID: r.ID, ContentVersion: r.ContentVersion, ManifestSHA: r.ContextManifest.SHA, Disclosure: r.ContextManifest.Disclosure, Audit: audit, CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_draft_authorizations(request_id,content_version,data) VALUES(?,?,?)`, r.ID, r.ContentVersion, raw)
	return err
}

func intakeDraftAuthorityTx(ctx context.Context, tx *sql.Tx, r model.IntakeRequest, m model.IntakeManifest) error {
	if m.RequestID != r.ID || m.Project != r.Project || m.PolicySHA != m.Disclosure.PolicySHA {
		return errors.New("committed draft context scope differs")
	}
	grant, err := intakeLoad[intakeDraftAuthorization](ctx, tx, `SELECT data FROM intake_draft_authorizations WHERE request_id=? AND content_version=?`, r.ID, m.ContentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return intakeError("stale_subject", "exact saved context has no committed drafting authorization")
	}
	if err != nil {
		return err
	}
	if grant.RequestID != r.ID || grant.ContentVersion != m.ContentVersion || grant.Audit.OperatorID != r.OperatorID || grant.Audit.Kind != model.AuditVerifiedOperator || grant.Audit.CommandID == "" || grant.ManifestSHA != m.SHA || programHash(grant.Disclosure) != programHash(m.Disclosure) {
		return errors.New("drafting authorization does not match immutable context")
	}
	return intakeManifestIdentity(m)
}
