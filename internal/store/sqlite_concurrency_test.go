package store

import (
	"context"
	"database/sql"
	"errors"
	"openfactory/internal/model"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openPeerStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	cli, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return writer, cli
}

func TestSQLiteWriterReservesBeforeReadAndReadOnlySnapshotRemainsConcurrent(t *testing.T) {
	writer, cli := openPeerStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := writer.Import(ctx, feature("cross-process"))
	if err != nil {
		t.Fatal(err)
	}
	writeTx, err := writer.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writeTx.Rollback()
	f, err = get(ctx, writeTx, f.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Zero timeout makes writer reservation observable without sleeps or a
	// scheduler race: even before writeTx mutates a row, another writer cannot
	// begin. A deferred BEGIN incorrectly succeeds here and can read stale state.
	conn, err := cli.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		t.Fatal(err)
	}
	other, err := conn.BeginTx(ctx, nil)
	if err == nil {
		other.Rollback()
		t.Fatal("write transaction did not reserve SQLite writer before reading authority")
	}
	var coded interface{ Code() int }
	if !errors.As(err, &coded) || coded.Code()&0xff != 5 {
		t.Fatalf("expected writer contention, got %v", err)
	}
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}

	// Dashboard/read-only transactions must not request that writer reservation.
	readTx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readTx.Rollback()
	before, err := get(ctx, readTx, f.ID)
	if err != nil || before.State != "proposed" {
		t.Fatalf("read snapshot unavailable: %+v %v", before, err)
	}
	f.State = "running"
	if err = put(ctx, writeTx, f); err != nil {
		t.Fatal(err)
	}
	if err = writeTx.Commit(); err != nil {
		t.Fatal("WAL reader blocked writer commit", err)
	}
	after, err := get(ctx, readTx, f.ID)
	if err != nil || after.State != "proposed" {
		t.Fatalf("read snapshot changed after concurrent commit: %+v %v", after, err)
	}
	if err = readTx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err = cli.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err == nil || !strings.Contains(err.Error(), "not awaiting approval") {
		t.Fatalf("CLI did not observe latest committed eligibility: %v", err)
	}
}

func TestSQLiteConcurrentCLIApprovalWaitsAndEvaluatesCommittedState(t *testing.T) {
	writer, cli := openPeerStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := writer.Import(ctx, feature("approval-race"))
	if err != nil {
		t.Fatal(err)
	}
	writeTx, err := writer.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writeTx.Rollback()
	f, err = get(ctx, writeTx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- cli.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()) }()
	// Wait until the separate Store has checked out its connection for BeginTx.
	// No operation is retried: it must wait at writer acquisition, then read the
	// daemon's committed state instead of failing a deferred snapshot upgrade.
	for cli.db.Stats().InUse == 0 {
		select {
		case err = <-result:
			t.Fatalf("CLI approval overtook reserved daemon transaction: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			runtime.Gosched()
		}
	}
	f.State = "running"
	if err = put(ctx, writeTx, f); err != nil {
		t.Fatal(err)
	}
	if err = writeTx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err == nil || !strings.Contains(err.Error(), "not awaiting approval") {
			t.Fatalf("expected current-state eligibility conflict, not lock/snapshot failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	current, err := writer.Get(ctx, f.ID)
	if err != nil || current.State != "running" || current.PolicyHash != "" {
		t.Fatalf("concurrent approval changed daemon state/authority: %+v %v", current, err)
	}
}

func TestSQLiteDSNEscapesPathAndReappliesPragmasOnReconnect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory ?#% state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	f, err := s.Import(ctx, feature("reconnect"))
	if err != nil {
		t.Fatal(err)
	}
	// Force every following statement onto a replacement physical connection.
	s.db.SetMaxIdleConns(0)
	for _, tc := range []struct {
		pragma string
		want   int
	}{{"foreign_keys", 1}, {"busy_timeout", 5000}} {
		var got int
		if err = s.db.QueryRowContext(ctx, "PRAGMA "+tc.pragma).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("reopened %s=%d, want %d: %v", tc.pragma, got, tc.want, err)
		}
	}
	var journal string
	if err = s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("reopened journal mode %q: %v", journal, err)
	}
	if persisted, err := s.Get(ctx, f.ID); err != nil || persisted.Revision != f.Revision {
		t.Fatalf("escaped filename did not preserve state: %+v %v", persisted, err)
	}
}

func TestSQLiteMemoryDSNRetainsSingleConnectionState(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	f, err := s.Import(ctx, feature("memory"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, f.ID, "policy", 1, 1, 1, 60); err != nil {
		t.Fatal(err)
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 1 || active[0].State != "running" {
		t.Fatalf("memory database state lost: %+v %v", active, err)
	}
}
