// Package store owns the durable queue, immutable source revisions, and side-effect ledger.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"openfactory/internal/artifacts"
	"openfactory/internal/intake"
	"openfactory/internal/model"
)

type Store struct {
	db          *sql.DB
	blobs       *artifacts.Store
	blobTempDir string
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("empty database path")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return nil, errors.New("database must be a regular file")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = file.Chmod(0600); err != nil {
			file.Close()
			return nil, err
		}
		if err = file.Close(); err != nil {
			return nil, err
		}
	}
	// Reserve the writer before a read-modify-write transaction observes state.
	// A deferred WAL transaction can otherwise fail its write upgrade with
	// SQLITE_BUSY_SNAPSHOT, which cannot wait on busy_timeout. ReadOnly BeginTx
	// remains deferred in modernc, so dashboard snapshots do not reserve writers.
	dsn := ":memory:"
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
	}
	params := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)"}}
	// Driver parameters reapply connection-local pragmas after a pool reconnect.
	db, err := sql.Open("sqlite", dsn+"?"+params.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS features(id TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS revisions(feature_id TEXT NOT NULL REFERENCES features(id), revision TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(feature_id,revision));
 CREATE TABLE IF NOT EXISTS attempts(id TEXT PRIMARY KEY, feature_id TEXT NOT NULL REFERENCES features(id), active INTEGER NOT NULL, revision TEXT NOT NULL, policy_hash TEXT NOT NULL, data BLOB NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS one_active_attempt ON attempts(feature_id) WHERE active=1;
 CREATE TABLE IF NOT EXISTS calls(id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL REFERENCES attempts(id), stage TEXT NOT NULL, day TEXT NOT NULL, UNIQUE(attempt_id,stage));
 CREATE INDEX IF NOT EXISTS calls_by_day ON calls(day);
 CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,feature_id TEXT NOT NULL,kind TEXT NOT NULL,detail TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS events_by_feature ON events(feature_id,seq);
 CREATE TABLE IF NOT EXISTS event_progress(event_id TEXT PRIMARY KEY REFERENCES events(id),data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions(attempt_id TEXT NOT NULL REFERENCES attempts(id),thread_id TEXT NOT NULL,stage TEXT NOT NULL,PRIMARY KEY(attempt_id,thread_id));
 CREATE TABLE IF NOT EXISTS evidence(id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),attempt_id TEXT NOT NULL REFERENCES attempts(id),revision TEXT NOT NULL,policy_hash TEXT NOT NULL,data BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS evidence_by_feature ON evidence(feature_id);
 CREATE INDEX IF NOT EXISTS evidence_by_attempt ON evidence(attempt_id,revision,policy_hash);
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS notifications(channel TEXT NOT NULL,event_id TEXT NOT NULL REFERENCES events(id),acked INTEGER NOT NULL DEFAULT 0,failures INTEGER NOT NULL DEFAULT 0,next_at TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',PRIMARY KEY(channel,event_id));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	blobRoot := filepath.Join(filepath.Dir(path), "artifacts")
	if path == ":memory:" {
		s.blobTempDir, err = os.MkdirTemp("", "openfactory-artifacts-")
		if err != nil {
			db.Close()
			return nil, err
		}
		blobRoot = filepath.Join(s.blobTempDir, "artifacts")
	}
	s.blobs = artifacts.New(blobRoot)
	if path != ":memory:" {
		if err = os.Chmod(path, 0600); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	err := s.db.Close()
	if s.blobTempDir != "" {
		err = errors.Join(err, os.RemoveAll(s.blobTempDir))
	}
	return err
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func event(ctx context.Context, q executor, id, kind, detail string, progress ...Progress) error {
	eventID := ID()
	_, err := q.ExecContext(ctx, `INSERT INTO events(id,feature_id,kind,detail,created_at) VALUES(?,?,?,?,?)`, eventID, id, kind, detail, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil || len(progress) == 0 {
		return err
	}
	data, err := json.Marshal(progress[0])
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO event_progress(event_id,data) VALUES(?,?)`, eventID, data)
	return err
}
func put(ctx context.Context, q executor, f model.Feature) error {
	f.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO features(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, f.ID, b)
	return err
}
func get(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (model.Feature, error) {
	var f model.Feature
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM features WHERE id=?`, id).Scan(&b)
	if err != nil {
		return f, err
	}
	err = json.Unmarshal(b, &f)
	return f, err
}
func (s *Store) Get(ctx context.Context, id string) (model.Feature, error) { return get(ctx, s.db, id) }
func (s *Store) List(ctx context.Context) ([]model.Feature, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM features ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Feature{}
	for rows.Next() {
		var b []byte
		var f model.Feature
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
func (s *Store) Import(ctx context.Context, f model.Feature) (model.Feature, error) {
	return s.importFeature(ctx, f, false)
}

// ImportProposal cannot replace an operator's existing feature, even under concurrent CLI imports.
func (s *Store) ImportProposal(ctx context.Context, f model.Feature) (model.Feature, error) {
	return s.importFeature(ctx, f, true)
}
func (s *Store) importFeature(ctx context.Context, f model.Feature, proposalOnly bool) (model.Feature, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return f, err
	}
	defer tx.Rollback()
	f, err = importFeatureTx(ctx, tx, f, proposalOnly)
	if err != nil {
		return f, err
	}
	return f, tx.Commit()
}

func importFeatureTx(ctx context.Context, tx *sql.Tx, f model.Feature, proposalOnly bool) (model.Feature, error) {
	if f.DiscoveryStepID != "" || strings.HasPrefix(f.ID, "openfactory-discovery-") {
		return f, intakeError("invalid_contract", "discovery features are controller-owned")
	}
	if f.IntakeStepID != "" || f.Kind == model.IntakeFeatureKind || strings.HasPrefix(f.ID, model.IntakeFeaturePrefix) {
		return f, intakeError("invalid_contract", "intake features are controller-owned")
	}
	if err := intake.Validate(f); err != nil {
		return f, intakeError("invalid_contract", err.Error())
	}
	f.Revision = intake.Revision(f)
	old, err := get(ctx, tx, f.ID)
	if err == nil {
		if old.Revision == f.Revision {
			return old, nil
		}
		if proposalOnly {
			return f, intakeError("stale_subject", "proposal ID already belongs to a different immutable source")
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, f.ID).Scan(&n); err != nil {
			return f, err
		}
		if n > 0 {
			return f, intakeError("attempt_unsettled", "cannot replace revision with an unsettled attempt")
		}
		f.CreatedAt = old.CreatedAt
		f.Attempts = old.Attempts
	} else if !errors.Is(err, sql.ErrNoRows) {
		return f, err
	} else {
		f.CreatedAt = time.Now().UTC()
		f.Attempts = 0
	}
	if err = validateFeatureDependenciesTx(ctx, tx, f); err != nil {
		return f, err
	}
	f.State = "proposed"
	f.PolicyHash = ""
	f.Reason = ""
	f.Receipt = ""
	f.UpdatedAt = time.Now().UTC()
	if err = put(ctx, tx, f); err != nil {
		return f, err
	}
	b, _ := json.Marshal(f)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, f.ID, f.Revision, b); err != nil {
		return f, err
	}
	if err = event(ctx, tx, f.ID, "imported", f.Revision, progressOf(f)); err != nil {
		return f, err
	}
	return f, nil
}

// Resolve durable dependencies again at approval, including existing global
// cross-project prerequisites, while never treating private controller drafting
// or discovery evidence as a code prerequisite.
func validateFeatureDependenciesTx(ctx context.Context, tx *sql.Tx, f model.Feature) error {
	visiting := map[string]bool{f.ID: true}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return intakeError("invalid_contract", "dependency cycle")
		}
		dep, err := get(ctx, tx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return intakeError("invalid_contract", fmt.Sprintf("dependency %s does not exist", id))
			}
			return fmt.Errorf("dependency %s: %w", id, err)
		}
		if dep.DiscoveryStepID != "" || strings.HasPrefix(id, "openfactory-discovery-") || internalIntakeFeature(dep) {
			return intakeError("invalid_contract", fmt.Sprintf("dependency %s is internal controller work, not a code prerequisite", id))
		}
		visiting[id] = true
		defer delete(visiting, id)
		for _, child := range dep.DependsOn {
			if err = visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range f.DependsOn {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Approve(ctx context.Context, id, revision, policyHash string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.approveFeatureTx(ctx, tx, id, revision, policyHash, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) approveFeatureTx(ctx context.Context, tx *sql.Tx, id, revision, policyHash string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	return s.mutateFeatureTx(ctx, tx, id, func(f *model.Feature) error {
		if internalIntakeFeature(*f) || f.DiscoveryStepID != "" || strings.HasPrefix(f.ID, "openfactory-discovery-") {
			return intakeError("invalid_contract", "internal controller work cannot receive product approval")
		}
		if revision != f.Revision || policyHash == "" {
			return intakeError("stale_revision", "exact current revision and policy hash required")
		}
		if f.State != "proposed" && f.State != "blocked" && f.State != "ready" {
			return intakeError("stale_subject", "feature is not awaiting approval")
		}
		f.PolicyHash = policyHash
		f.State = "ready"
		f.Reason = ""
		return recordAuditTx(ctx, tx, "feature", id, "approved", audit)
	}, "approved")
}
func (s *Store) mutate(ctx context.Context, id string, fn func(*model.Feature) error, kind string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.mutateFeatureTx(ctx, tx, id, fn, kind); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) mutateFeatureTx(ctx context.Context, tx *sql.Tx, id string, fn func(*model.Feature) error, kind string) error {
	f, err := get(ctx, tx, id)
	if err != nil {
		return err
	}
	if kind == "approved" {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return intakeError("attempt_unsettled", "cannot approve an unsettled attempt")
		}
	}
	if err = fn(&f); err != nil {
		return err
	}
	if kind == "approved" {
		if err = validateFeatureDependenciesTx(ctx, tx, f); err != nil {
			return err
		}
		if err = s.programFeatureGateTx(ctx, tx, f, f.PolicyHash); err != nil {
			return err
		}
	}
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	if err = event(ctx, tx, id, kind, f.Reason, progressOf(f)); err != nil {
		return err
	}
	return nil
}
func (s *Store) Retry(ctx context.Context, id string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = retryFeatureTx(ctx, tx, id, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func retryFeatureTx(ctx context.Context, tx *sql.Tx, id string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	f, err := get(ctx, tx, id)
	if err != nil {
		return err
	}
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, id).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return intakeError("attempt_unsettled", "attempt unsettled; cancel and reconcile before retry")
	}
	if f.State != "blocked" && f.State != "cancelled" {
		return intakeError("stale_subject", "only blocked or cancelled features can retry")
	}
	if f.PolicyHash == "" {
		return intakeError("stale_policy", "revision approval required")
	}
	f.State = "ready"
	f.Reason = ""
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	if err = event(ctx, tx, id, "retry_requested", "", progressOf(f)); err != nil {
		return err
	}
	return recordAuditTx(ctx, tx, "feature", id, "retry_requested", audit)
}
func (s *Store) Cancel(ctx context.Context, id string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.cancelFeatureTx(ctx, tx, id, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) cancelFeatureTx(ctx context.Context, tx *sql.Tx, id string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	return s.mutateFeatureTx(ctx, tx, id, func(f *model.Feature) error {
		if f.State == "accepted" {
			return intakeError("stale_subject", "accepted feature cannot be cancelled")
		}
		f.State = "cancelling"
		f.Reason = "operator cancellation"
		return recordAuditTx(ctx, tx, "feature", id, "cancel_requested", audit)
	}, "cancel_requested")
}
func (s *Store) SetPaused(ctx context.Context, p bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='paused'`).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	value, kind := "false", "resumed"
	if p {
		value, kind = "true", "paused"
	}
	if old == value || old == "" && !p {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('paused',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, value); err != nil {
		return err
	}
	if err = event(ctx, tx, "", kind, "operator "+kind); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Paused(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='paused'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v == "true", err
}
func (s *Store) Events(ctx context.Context, id string) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,feature_id,kind,detail,created_at FROM events WHERE (?='' OR feature_id=?) ORDER BY seq`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}
func scanEvents(rows *sql.Rows) ([]model.Event, error) {
	out := []model.Event{}
	for rows.Next() {
		var e model.Event
		var t string
		if err := rows.Scan(&e.ID, &e.FeatureID, &e.Kind, &e.Detail, &t); err != nil {
			return nil, err
		}
		var err error
		e.CreatedAt, err = time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Evidence(ctx context.Context, id string) ([]model.Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM evidence WHERE feature_id=? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Evidence{}
	for rows.Next() {
		var b []byte
		var e model.Evidence
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Backup(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("backup path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// VACUUM INTO accepts a zero-byte destination. Exclusive creation prevents
	// overwrite races and protects the backup from the first byte, not just after it.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	_, err = s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(path, "'", "''")+`'`)
	if err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
func (s *Store) PendingNotifications(ctx context.Context, channel string, limit int) ([]model.Event, error) {
	if channel == "" {
		return nil, errors.New("notification channel required")
	}
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.feature_id,e.kind,e.detail,e.created_at FROM events e LEFT JOIN notifications n ON n.event_id=e.id AND n.channel=? LEFT JOIN features f ON f.id=e.feature_id WHERE coalesce(n.acked,0)=0 AND coalesce(n.failures,0)<8 AND coalesce(n.next_at,'')<=? AND e.created_at>=? AND e.feature_id NOT GLOB 'openfactory-intake-*' AND coalesce(json_extract(f.data,'$.intake_step_id'),'')='' AND coalesce(json_extract(f.data,'$.kind'),'')!='intake' ORDER BY e.seq LIMIT ?`, channel, now.Format(time.RFC3339Nano), now.Add(-7*24*time.Hour).Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}
func (s *Store) AckNotification(ctx context.Context, channel, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notifications(channel,event_id,acked) VALUES(?,?,1) ON CONFLICT(channel,event_id) DO UPDATE SET acked=1`, channel, id)
	return err
}
func (s *Store) RecordNotificationFailure(ctx context.Context, channel, id, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	err = tx.QueryRowContext(ctx, `SELECT failures FROM notifications WHERE channel=? AND event_id=?`, channel, id).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	delay := time.Minute * time.Duration(1<<min(n, 6))
	if delay > time.Hour {
		delay = time.Hour
	}
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notifications(channel,event_id,failures,next_at,reason) VALUES(?,?,1,?,?) ON CONFLICT(channel,event_id) DO UPDATE SET failures=failures+1,next_at=excluded.next_at,reason=excluded.reason`, channel, id, time.Now().UTC().Add(delay).Format(time.RFC3339Nano), reason)
	if err != nil {
		return err
	}
	return tx.Commit()
}
