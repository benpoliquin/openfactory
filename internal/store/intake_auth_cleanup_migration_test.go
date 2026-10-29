package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSchema4OpenAddsCeremonyCleanupIndexWithoutResettingAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "existing-schema4.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, credential, session := authTestActor(t, s, time.Now().UTC().Add(-time.Second), "cleanup-index-migration")
	generation, err := s.AuthGeneration(ctx)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	installation, err := s.InstallationID(ctx)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	var before []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM intake_credentials WHERE id=?`, credential.ID).Scan(&before); err != nil {
		s.Close()
		t.Fatal(err)
	}
	// Reproduce an existing schema-4 database from before the additive bounded
	// ceremony-retention index. Its valid authority must survive ordinary Open.
	if _, err = s.db.ExecContext(ctx, `DROP INDEX intake_auth_challenges_unused_ceremony_expiry`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	var indexes int
	if err = reopened.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='intake_auth_challenges_unused_ceremony_expiry'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatal("existing schema-4 database lacks bounded ceremony cleanup index")
	}
	afterGeneration, err := reopened.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterInstallation, err := reopened.InstallationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration != generation || afterInstallation != installation {
		t.Fatal("ordinary index migration reset installation/session authority")
	}
	var after []byte
	if err = reopened.db.QueryRowContext(ctx, `SELECT data FROM intake_credentials WHERE id=?`, credential.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("index migration rewrote credential history")
	}
	if _, _, _, err = reopened.BrowserSessionByTokenHash(ctx, session.TokenHash); err != nil {
		t.Fatalf("normal restart revoked a valid session: %v", err)
	}
}
