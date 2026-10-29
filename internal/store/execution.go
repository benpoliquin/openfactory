package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"openfactory/internal/model"
)

// ErrWaiting marks an ordinary queue gate rather than a storage failure.
var ErrWaiting = errors.New("queue gate")

func waiting(reason string) error { return fmt.Errorf("%w: %s", ErrWaiting, reason) }

// Active includes uncertain operations: an operator retry cannot discard their ownership.
func (s *Store) Active(ctx context.Context) ([]model.Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM attempts WHERE active=1 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Attempt{}
	for rows.Next() {
		var b []byte
		var a model.Attempt
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) Claim(ctx context.Context, id, policy string, maxAttempts, maxConcurrent, projectMaxConcurrent, timeoutSeconds int) (model.Attempt, error) {
	var a model.Attempt
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	f, err := get(ctx, tx, id)
	if err != nil {
		return a, err
	}
	wait, err := s.admissionGate(ctx, tx, f, policy, maxAttempts, maxConcurrent, projectMaxConcurrent)
	if err != nil {
		return a, err
	}
	if wait != nil {
		if err = intakeClaimWaitTx(ctx, tx, f, wait); err != nil {
			return a, err
		}
		if err = tx.Commit(); err != nil {
			return a, err
		}
		return a, waiting(wait.Detail)
	}
	now := time.Now().UTC()
	a = model.Attempt{ID: ID(), FeatureID: f.ID, Stage: "prepare", State: "running", StartedAt: now, StageStartedAt: now, UpdatedAt: now, Deadline: now.Add(time.Duration(timeoutSeconds) * time.Second)}
	b, _ := json.Marshal(a)
	if _, err = tx.ExecContext(ctx, `INSERT INTO attempts(id,feature_id,active,revision,policy_hash,data) VALUES(?,?,1,?,?,?)`, a.ID, f.ID, f.Revision, policy, b); err != nil {
		return a, err
	}
	f.Attempts++
	f.State = "running"
	if err = put(ctx, tx, f); err != nil {
		return a, err
	}
	if err = event(ctx, tx, f.ID, "attempt_started", a.ID, progressOf(f, a)); err != nil {
		return a, err
	}
	if err = intakeClaimStartedTx(ctx, tx, f, a); err != nil {
		return a, err
	}
	return a, tx.Commit()
}

// Save atomically records a stage, evidence, and its externally visible transition.
// It never overwrites an operator cancellation that raced an external operation.
func (s *Store) Save(ctx context.Context, a model.Attempt, state, reason, receipt string, active bool, evidence ...model.Evidence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.saveTx(ctx, tx, a, state, reason, receipt, active, evidence...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) saveTx(ctx context.Context, tx *sql.Tx, a model.Attempt, state, reason, receipt string, active bool, evidence ...model.Evidence) error {
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	var rev, policy string
	var current int
	var previous []byte
	oldState := f.State
	if err = tx.QueryRowContext(ctx, `SELECT revision,policy_hash,active,data FROM attempts WHERE id=? AND feature_id=?`, a.ID, f.ID).Scan(&rev, &policy, &current, &previous); err != nil {
		return err
	}
	if current == 0 {
		return errors.New("attempt already settled")
	}
	if rev != f.Revision || policy != f.PolicyHash {
		return errors.New("attempt authority no longer matches feature")
	}
	var old model.Attempt
	if err = json.Unmarshal(previous, &old); err != nil {
		return err
	}
	if old.IntegrationToken != a.IntegrationToken {
		return errors.New("stale integration token")
	}
	if err = validateCandidate(old, a); err != nil {
		return err
	}
	if old.SourceBaseSHA != "" && (old.SourceBaseSHA != a.SourceBaseSHA || old.SourceCandidateSHA != a.SourceCandidateSHA) {
		return errors.New("immutable source delta changed")
	}
	if a.IntegrationToken != "" {
		if err = validateIntegrationOwner(ctx, tx, a); err != nil {
			return err
		}
	}
	if !active {
		if old.Stage == "uncertain" || a.Stage == "uncertain" {
			return errors.New("uncertain attempt requires explicit operator resolution")
		}
		if err = releaseIntegration(ctx, tx, a); err != nil {
			return err
		}
		if err = settleChecks(ctx, tx, a.ID, state); err != nil {
			return err
		}
		a.IntegrationToken = ""
		a.Wait = nil
	}
	a.UpdatedAt = time.Now().UTC()
	if old.Stage != a.Stage || a.StageStartedAt.IsZero() {
		a.StageStartedAt = a.UpdatedAt
	}
	if err = rememberSession(ctx, tx, old, ""); err != nil {
		return err
	}
	previousStage := ""
	if a.ThreadID == old.ThreadID {
		previousStage = old.Stage
	}
	if err = rememberSession(ctx, tx, a, previousStage); err != nil {
		return err
	}
	b, _ := json.Marshal(a)
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET data=?,active=? WHERE id=?`, b, active, a.ID); err != nil {
		return err
	}
	for _, e := range evidence {
		if e.ID == "" {
			e.ID = ID()
		}
		e.FeatureID = f.ID
		e.AttemptID = a.ID
		e.CreatedAt = time.Now().UTC()
		if e.CandidateSHA != a.CandidateSHA || e.CandidateGeneration != a.CandidateGeneration {
			return errors.New("evidence candidate or generation mismatch")
		}
		b, _ := json.Marshal(e)
		if _, err = tx.ExecContext(ctx, `INSERT INTO evidence(id,feature_id,attempt_id,revision,policy_hash,data) VALUES(?,?,?,?,?,?)`, e.ID, f.ID, a.ID, rev, policy, b); err != nil {
			return err
		}
	}
	if f.State != "cancelling" || state == "cancelled" {
		f.State = state
		if reason != "" || state != "running" {
			f.Reason = reason
		}
		if receipt != "" || state != "cancelled" {
			f.Receipt = receipt
		}
	} else {
		if reason != "" {
			f.Reason = "cancellation pending; " + reason
		}
		if receipt != "" {
			f.Receipt = receipt
		}
	}
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	if old.Stage != a.Stage || oldState != f.State || len(evidence) > 0 {
		if err = event(ctx, tx, f.ID, a.Stage, reason, progressOf(f, a)); err != nil {
			return err
		}
	}
	if active {
		if err = intakeAttemptProgressTx(ctx, tx, f, a); err != nil {
			return err
		}
	} else if err = settleIntakeFeatureTx(ctx, tx, f, a, state); err != nil {
		return err
	}
	return nil
}

// ReserveCall persists the dispatch intent and consumes the UTC-day allowance before any network call.
func (s *Store) ReserveCall(ctx context.Context, a model.Attempt, stage, dispatchKey string, maxCalls int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, old, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if stage == "" || dispatchKey == "" {
		return errors.New("stage and dispatch key required")
	}
	if a.CandidateGeneration != old.CandidateGeneration || a.CandidateSHA != old.CandidateSHA {
		return errors.New("dispatch candidate authority mismatch")
	}
	if err = s.programFeatureGateTx(ctx, tx, f, f.PolicyHash); err != nil {
		return err
	}
	if err = intakeFeatureGateTx(ctx, tx, f, f.PolicyHash); err != nil {
		return err
	}
	var reservedStage string
	var reservedGeneration int
	err = tx.QueryRowContext(ctx, `SELECT stage,generation FROM calls WHERE attempt_id=? AND dispatch_key=?`, a.ID, dispatchKey).Scan(&reservedStage, &reservedGeneration)
	if err == nil {
		if reservedStage != stage || reservedGeneration != a.CandidateGeneration {
			return errors.New("dispatch key already belongs to another stage or generation")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if f.State == "cancelling" {
		return waiting("feature cancellation pending")
	}
	var paused string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='paused'`).Scan(&paused)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if paused == "true" {
		return waiting("OpenFactory paused")
	}
	var n int
	day := time.Now().UTC().Format("2006-01-02")
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM calls WHERE day=?`, day).Scan(&n); err != nil {
		return err
	}
	if maxCalls < 1 || n >= maxCalls {
		return waiting("daily model-call allowance exhausted")
	}
	if err = reserveProgramCallTx(ctx, tx, f); err != nil {
		return err
	}
	if err = reserveIntakeCallTx(ctx, tx, f, a, dispatchKey); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO calls(id,attempt_id,stage,dispatch_key,generation,day) VALUES(?,?,?,?,?,?)`, ID(), a.ID, stage, dispatchKey, a.CandidateGeneration, day); err != nil {
		return err
	}
	if err = rememberSession(ctx, tx, a, ""); err != nil {
		return err
	}
	a.Stage = stage + "_dispatching"
	a.DispatchKey = dispatchKey
	a.Wait = nil
	a.ThreadID = ""
	a.Cursor = ""
	a.Summary = ""
	if err = saveAttempt(ctx, tx, a, old); err != nil {
		return err
	}
	if err = intakeAttemptProgressTx(ctx, tx, f, a); err != nil {
		return err
	}
	if err = event(ctx, tx, f.ID, "dispatch_reserved", stage, progressOf(f, a)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Block(ctx context.Context, id, reason string) error {
	return s.mutate(ctx, id, func(f *model.Feature) error { f.State = "blocked"; f.Reason = reason; return nil }, "blocked")
}
func (s *Store) SettleCancellation(ctx context.Context, id string) error {
	return s.mutate(ctx, id, func(f *model.Feature) error {
		if f.State != "cancelling" {
			return errors.New("cancellation not requested")
		}
		f.State = "cancelled"
		return nil
	}, "cancelled")
}
