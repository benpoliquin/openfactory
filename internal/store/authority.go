package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"openfactory/internal/model"
)

// BeginOperation fences each non-idempotent operation against concurrent pause/cancel
// and stale revision authority before persisting its intent.
func (s *Store) BeginOperation(ctx context.Context, a model.Attempt, policy string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, old, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if f.State == "cancelling" {
		return waiting("cancellation pending")
	}
	if policy == "" || f.PolicyHash != policy {
		return errors.New("operation policy authority changed")
	}
	if err = intakeFeatureGateTx(ctx, tx, f, policy); err != nil {
		return err
	}
	if err = s.programFeatureGateTx(ctx, tx, f, policy); err != nil {
		return err
	}
	var paused int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='paused' AND value='true'`).Scan(&paused); err != nil {
		return err
	}
	if paused != 0 {
		return waiting("OpenFactory paused")
	}
	if protectedStage(a) {
		if err = validateIntegrationOwner(ctx, tx, a); err != nil {
			return err
		}
	}
	a.Wait = nil
	if err = saveAttempt(ctx, tx, a, old); err != nil {
		return err
	}
	if err = event(ctx, tx, f.ID, a.Stage, "operation intent", progressOf(f, a)); err != nil {
		return err
	}
	return tx.Commit()
}

// AttemptEvidence excludes historical revisions and policies from acceptance.
func (s *Store) AttemptEvidence(ctx context.Context, id string) ([]model.Evidence, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, revision, policy, _, err := loadAttempt(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	featureID := a.FeatureID
	f, err := get(ctx, tx, featureID)
	if err != nil {
		return nil, err
	}
	if f.Revision != revision || f.PolicyHash != policy {
		return nil, errors.New("evidence authority does not match current feature")
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM evidence WHERE attempt_id=? AND revision=? AND policy_hash=? ORDER BY rowid`, id, revision, policy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Evidence{}
	for rows.Next() {
		var b []byte
		var v model.Evidence
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		if v.CandidateGeneration == a.CandidateGeneration && v.CandidateSHA == a.CandidateSHA {
			out = append(out, v)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, tx.Commit()
}

// AcceptedCandidate reads the immutable candidate of the current acceptance;
// deployment receipts are external attestations, not necessarily Git SHAs.
func (s *Store) AcceptedCandidate(ctx context.Context, featureID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	f, err := get(ctx, tx, featureID)
	if err != nil {
		return "", err
	}
	if !DependencySatisfied(f) {
		return "", errors.New("dependency lacks an integrated acceptance")
	}
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM attempts WHERE feature_id=? AND revision=? AND policy_hash=? AND active=0 AND json_extract(data,'$.state')='accepted' ORDER BY rowid DESC LIMIT 1`, featureID, f.Revision, f.PolicyHash).Scan(&data); err != nil {
		return "", err
	}
	var a model.Attempt
	if err = json.Unmarshal(data, &a); err != nil {
		return "", err
	}
	if a.CandidateSHA == "" {
		return "", errors.New("accepted attempt has no candidate SHA")
	}
	return a.CandidateSHA, tx.Commit()
}
