package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfactory/internal/model"
)

// migrate atomically backfills legacy dispatch identities and records which
// uncertain attempts predate durable integration ownership.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 4 {
		return fmt.Errorf("database schema %d is newer than supported schema 4", version)
	}
	if version == 4 {
		// This additive index also bounds ceremony cleanup for databases opened
		// by an earlier schema-4 controller. No authority/history is rewritten.
		if _, err = tx.ExecContext(ctx, intakeAuthChallengeCleanupIndex); err != nil {
			return err
		}
	}
	if version < 2 {
		// Schema 1 already records delivery authority. Its unowned uncertainty
		// must never be reclassified as legacy delivery work on upgrade.
		if _, err = tx.ExecContext(ctx, `CREATE TABLE legacy_integration_recovery(attempt_id TEXT PRIMARY KEY REFERENCES attempts(id))`); err != nil {
			return err
		}
	}
	if version == 0 {
		_, err = tx.ExecContext(ctx, `ALTER TABLE calls RENAME TO calls_legacy;
 CREATE TABLE calls(id TEXT PRIMARY KEY,attempt_id TEXT NOT NULL REFERENCES attempts(id),stage TEXT NOT NULL,dispatch_key TEXT NOT NULL,generation INTEGER NOT NULL DEFAULT 0,day TEXT NOT NULL,UNIQUE(attempt_id,dispatch_key));
 INSERT INTO calls(id,attempt_id,stage,dispatch_key,day) SELECT id,attempt_id,stage,attempt_id||':'||stage,day FROM calls_legacy;
 DROP TABLE calls_legacy;
 CREATE INDEX calls_by_day ON calls(day);
 CREATE INDEX attempts_by_authority ON attempts(feature_id,revision,policy_hash,active);
 ALTER TABLE sessions ADD COLUMN dispatch_key TEXT NOT NULL DEFAULT '';
 ALTER TABLE sessions ADD COLUMN environment_id TEXT NOT NULL DEFAULT '';
 ALTER TABLE sessions ADD COLUMN created_at TEXT NOT NULL DEFAULT '';
 UPDATE sessions SET dispatch_key=attempt_id||':'||stage;
 CREATE TABLE integration_requests(seq INTEGER PRIMARY KEY AUTOINCREMENT,attempt_id TEXT UNIQUE NOT NULL REFERENCES attempts(id),revision TEXT NOT NULL,policy_hash TEXT NOT NULL,resources BLOB NOT NULL,state TEXT NOT NULL,token TEXT NOT NULL DEFAULT '');
 CREATE TABLE integration_owners(resource TEXT PRIMARY KEY,attempt_id TEXT NOT NULL REFERENCES attempts(id),token TEXT NOT NULL);
 CREATE TABLE checks(attempt_id TEXT NOT NULL REFERENCES attempts(id),name TEXT NOT NULL,generation INTEGER NOT NULL,sha TEXT NOT NULL,state TEXT NOT NULL,detail TEXT NOT NULL,started_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(attempt_id,name,generation,sha));`)
		if err != nil {
			return err
		}
		rows, e := tx.QueryContext(ctx, `SELECT id,data,active FROM attempts`)
		if e != nil {
			return e
		}
		type row struct {
			id     string
			a      model.Attempt
			active int
		}
		var attempts []row
		for rows.Next() {
			var r row
			var b []byte
			if e = rows.Scan(&r.id, &b, &r.active); e == nil {
				e = json.Unmarshal(b, &r.a)
			}
			if e != nil {
				rows.Close()
				return e
			}
			attempts = append(attempts, r)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, r := range attempts {
			a := r.a
			// Before durable ownership existed, uncertainty could conceal an
			// external delivery effect. Preserve only that migration-time set.
			if r.active == 1 && a.Stage == "uncertain" {
				if _, err = tx.ExecContext(ctx, `INSERT INTO legacy_integration_recovery(attempt_id) VALUES(?)`, r.id); err != nil {
					return err
				}
			}
			if a.StageStartedAt.IsZero() {
				a.StageStartedAt = a.StartedAt
			}
			if a.UpdatedAt.IsZero() {
				a.UpdatedAt = a.StageStartedAt
			}
			stage := agentStage(a.Stage)
			if a.DispatchKey == "" && stage != "" && (strings.HasSuffix(a.Stage, "_dispatching") || strings.HasSuffix(a.Stage, "_running")) {
				a.DispatchKey = a.ID + ":" + stage
			}
			b, _ := json.Marshal(a)
			if _, err = tx.ExecContext(ctx, `UPDATE attempts SET data=? WHERE id=?`, b, r.id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE sessions SET created_at=? WHERE attempt_id=? AND created_at=''`, a.StartedAt.UTC().Format(time.RFC3339Nano), a.ID); err != nil {
				return err
			}
			if a.ThreadID != "" {
				if _, err = tx.ExecContext(ctx, `UPDATE sessions SET environment_id=? WHERE attempt_id=? AND thread_id=?`, a.EnvironmentID, a.ID, a.ThreadID); err != nil {
					return err
				}
			}
		}
	}
	if version < 2 {
		if _, err = tx.ExecContext(ctx, `PRAGMA user_version=2`); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.ExecContext(ctx, programSchema+programResearchSchema+programExperimentSchema); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `PRAGMA user_version=3`); err != nil {
			return err
		}
	}
	if version < 4 {
		if _, err = tx.ExecContext(ctx, intakeAuthSchema+intakeRequestSchema+intakePlanningSchema+intakeCommandSchema+intakeBundleSchema); err != nil {
			return err
		}
		// Preserve legacy revision/grant bytes and budgets. Historical grants
		// retain their original Actor; absence of a typed audit is legacy/local,
		// never retroactively represented as a phone authorization.
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO settings(key,value) VALUES('installation_id',?);`, ID()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `PRAGMA user_version=4`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadAttempt(ctx context.Context, tx *sql.Tx, id string) (model.Attempt, string, string, int, error) {
	var a model.Attempt
	var b []byte
	var revision, policy string
	var active int
	err := tx.QueryRowContext(ctx, `SELECT data,revision,policy_hash,active FROM attempts WHERE id=?`, id).Scan(&b, &revision, &policy, &active)
	if err == nil {
		err = json.Unmarshal(b, &a)
	}
	return a, revision, policy, active, err
}

func saveAttempt(ctx context.Context, tx *sql.Tx, a model.Attempt, old model.Attempt) error {
	now := time.Now().UTC()
	a.UpdatedAt = now
	if a.Stage != old.Stage || a.StageStartedAt.IsZero() {
		a.StageStartedAt = now
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE attempts SET data=? WHERE id=?`, b, a.ID)
	return err
}

func validateAttempt(ctx context.Context, tx *sql.Tx, a model.Attempt) (model.Feature, model.Attempt, error) {
	old, revision, policy, active, err := loadAttempt(ctx, tx, a.ID)
	if err != nil {
		return model.Feature{}, old, err
	}
	if old.FeatureID != a.FeatureID || active != 1 {
		return model.Feature{}, old, errors.New("attempt is not active under this feature")
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return f, old, err
	}
	if revision != f.Revision || policy != f.PolicyHash {
		return f, old, errors.New("attempt authority no longer matches feature")
	}
	if old.IntegrationToken != a.IntegrationToken {
		return f, old, errors.New("stale integration token")
	}
	if err = validateCandidate(old, a); err != nil {
		return f, old, err
	}
	if old.SourceBaseSHA != "" && (old.SourceBaseSHA != a.SourceBaseSHA || old.SourceCandidateSHA != a.SourceCandidateSHA) {
		return f, old, errors.New("immutable source delta changed")
	}
	return f, old, nil
}

func validateCandidate(old, a model.Attempt) error {
	if a.CandidateGeneration < 0 || a.CandidateGeneration < old.CandidateGeneration {
		return errors.New("stale candidate generation")
	}
	if old.CandidateSHA != "" && a.CandidateSHA != old.CandidateSHA && a.CandidateGeneration <= old.CandidateGeneration {
		return errors.New("changed candidate requires a fresh generation")
	}
	return nil
}
