package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"openfactory/internal/model"
)

func (s *Store) RecordSession(ctx context.Context, id, stage, dispatchKey string, session model.Session) error {
	if id == "" || stage == "" || dispatchKey == "" || session.ThreadID == "" {
		return errors.New("session identity fields are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, _, _, active, err := loadAttempt(ctx, tx, id)
	if err != nil {
		return err
	}
	if active != 1 {
		return errors.New("session attempt is settled")
	}
	var reserved string
	if err = tx.QueryRowContext(ctx, `SELECT stage FROM calls WHERE attempt_id=? AND dispatch_key=?`, id, dispatchKey).Scan(&reserved); err != nil {
		return err
	}
	if reserved != stage || a.DispatchKey != dispatchKey {
		return errors.New("session dispatch authority mismatch")
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	if err = independentProgramThreadTx(ctx, tx, f, session.ThreadID); err != nil {
		return err
	}
	var oldStage, oldKey, oldEnv string
	err = tx.QueryRowContext(ctx, `SELECT stage,dispatch_key,environment_id FROM sessions WHERE attempt_id=? AND thread_id=?`, id, session.ThreadID).Scan(&oldStage, &oldKey, &oldEnv)
	if err == nil {
		if oldStage != stage || oldKey != dispatchKey || oldEnv != session.EnvironmentID {
			return errors.New("session identity changed")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// A second discovered session for one dispatch is preserved for operator
	// diagnosis rather than silently coalesced with the first external effect.
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(attempt_id,thread_id,stage,dispatch_key,environment_id,created_at) VALUES(?,?,?,?,?,?)`, id, session.ThreadID, stage, dispatchKey, session.EnvironmentID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordCheck(ctx context.Context, id, name string, generation int, sha, state, detail string) error {
	if name == "" || sha == "" || generation < 0 {
		return errors.New("check identity required")
	}
	if state != "running" && state != "passed" && state != "failed" {
		return errors.New("invalid check state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, _, _, active, err := loadAttempt(ctx, tx, id)
	if err != nil {
		return err
	}
	if active != 1 || a.CandidateGeneration != generation || a.CandidateSHA != sha {
		return errors.New("check candidate authority mismatch")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO checks(attempt_id,name,generation,sha,state,detail,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(attempt_id,name,generation,sha) DO UPDATE SET state=excluded.state,detail=excluded.detail,updated_at=excluded.updated_at`, id, name, generation, sha, state, detail, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func settleChecks(ctx context.Context, tx *sql.Tx, id, state string) error {
	_, err := tx.ExecContext(ctx, `UPDATE checks SET state='failed',detail=?,updated_at=? WHERE attempt_id=? AND state='running'`, "attempt settled as "+state+" before check completion was recorded", time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}
