package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"openfactory/internal/model"
)

// ResolveFailed records an operator's explicit attestation that an uncertain or
// blocked attempt's external sessions/effects have settled. It never grants
// acceptance, approval, or a fresh budget. The exact owned attempt is mandatory.
func (s *Store) ResolveFailed(ctx context.Context, featureID, attemptID, receipt string) error {
	receipt = strings.TrimSpace(receipt)
	if receipt == "" || len(receipt) > 4096 {
		return errors.New("operator reconciliation receipt must contain 1..4096 bytes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := get(ctx, tx, featureID)
	if err != nil {
		return err
	}
	var data []byte
	var active int
	var revision, policy string
	if err = tx.QueryRowContext(ctx, `SELECT data,active,revision,policy_hash FROM attempts WHERE id=? AND feature_id=?`, attemptID, featureID).Scan(&data, &active, &revision, &policy); err != nil {
		return err
	}
	var a model.Attempt
	if err = json.Unmarshal(data, &a); err != nil {
		return err
	}
	eligible := f.State == "blocked" || (f.State == "cancelling" && (a.State == "blocked" || a.Stage == "uncertain"))
	if active != 1 || !eligible {
		return errors.New("only an active blocked or uncertain attempt can be reconciled")
	}
	if err = releaseIntegration(ctx, tx, a); err != nil {
		return err
	}
	if err = settleChecks(ctx, tx, a.ID, "operator_reconciled_failure"); err != nil {
		return err
	}
	a.IntegrationToken = ""
	a.Wait = nil
	a.UpdatedAt = time.Now().UTC()
	a.StageStartedAt = a.UpdatedAt
	a.State = "failed"
	a.Stage = "operator_reconciled_failure"
	data, _ = json.Marshal(a)
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET active=0,data=? WHERE id=? AND active=1`, data, a.ID); err != nil {
		return err
	}
	evidence := model.Evidence{ID: ID(), FeatureID: featureID, AttemptID: attemptID, Kind: "operator_reconciled_failure", CandidateSHA: a.CandidateSHA, CandidateGeneration: a.CandidateGeneration, Passed: false, Detail: receipt, CreatedAt: time.Now().UTC()}
	data, _ = json.Marshal(evidence)
	if _, err = tx.ExecContext(ctx, `INSERT INTO evidence(id,feature_id,attempt_id,revision,policy_hash,data) VALUES(?,?,?,?,?,?)`, evidence.ID, featureID, attemptID, revision, policy, data); err != nil {
		return err
	}
	f.State = "blocked"
	f.Reason = "operator reconciled failed attempt; separate retry or approval required"
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	if err = event(ctx, tx, featureID, "operator_reconciled_failure", receipt, progressOf(f, a)); err != nil {
		return err
	}
	return tx.Commit()
}
