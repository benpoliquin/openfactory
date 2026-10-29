package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openfactory/internal/config"
	"openfactory/internal/model"
)

func lane(t *testing.T, s *Store, id string) model.Attempt {
	t.Helper()
	approved(t, s, id)
	a, err := s.Claim(context.Background(), id, "policy", 4, 8, 8, 3600)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestConcurrentAdmissionCapsAndDependencyPrecedence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"alpha", "beta", "gamma"} {
		approved(t, s, id)
	}
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for _, id := range []string{"alpha", "beta", "gamma"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _, err := s.Claim(ctx, id, "policy", 3, 3, 2, 3600); results <- err }(id)
	}
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrWaiting) {
			t.Fatal(err)
		}
	}
	if admitted != 2 {
		t.Fatalf("project cap admitted %d, want 2", admitted)
	}
	dep := feature("dependent")
	dep.DependsOn = []string{"alpha"}
	dep, err := s.Import(ctx, dep)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, dep.ID, dep.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, dep.ID, "policy", 3, 2, 2, 3600); !errors.Is(err, ErrWaiting) || !strings.Contains(err.Error(), "dependency alpha") {
		t.Fatalf("dependency gate not shared or precedes caps: %v", err)
	}
}

func TestIntegrationFIFOAtomicResourcesAndStaleToken(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	first := lane(t, s, "first")
	second := lane(t, s, "second")
	third := lane(t, s, "third")
	disjoint := lane(t, s, "disjoint")
	resources := map[string][]string{first.ID: {"git:repo:refs/heads/main"}, second.ID: {"release:repo:staging", "git:repo:refs/heads/main"}, third.ID: {"release:repo:staging"}, disjoint.ID: {"git:other:refs/heads/main"}}
	for _, a := range []model.Attempt{first, second, third, disjoint} {
		if err := s.EnqueueIntegration(ctx, a, resources[a.ID]); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := s.AcquireIntegration(ctx, first.ID, "policy")
	if err != nil {
		t.Fatal(err)
	}
	first.IntegrationToken = lease.Token
	if err = s.EnqueueIntegration(ctx, first, resources[first.ID]); err != nil {
		t.Fatal("same resources should be idempotent", err)
	}
	if _, err = s.AcquireIntegration(ctx, second.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("shared owner excluded: %v", err)
	}
	if _, err = s.AcquireIntegration(ctx, third.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("FIFO queue was bypassed: %v", err)
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM integration_owners WHERE resource='release:repo:staging'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial multi-resource acquisition: %d %v", n, err)
	}
	if _, err = s.AcquireIntegration(ctx, disjoint.ID, "policy"); err != nil {
		t.Fatal("disjoint resource blocked", err)
	}
	stale := first
	stale.IntegrationToken = "old-token"
	stale.Stage = "cancelled"
	if err = s.Save(ctx, stale, "cancelled", "", "", false); err == nil {
		t.Fatal("stale token released ownership")
	}
	stale.Stage = "integrating"
	if err = s.BeginOperation(ctx, stale, "policy"); err == nil {
		t.Fatal("stale token authorized operation")
	}
	status, err := s.IntegrationStatus(ctx, first.ID)
	if err != nil || status.State != "owned" || status.Token != lease.Token {
		t.Fatalf("ownership lost: %+v %v", status, err)
	}
	first.Stage = "accepted"
	if err = s.Save(ctx, first, "accepted", "", "receipt", false); err != nil {
		t.Fatal(err)
	}
	lease, err = s.AcquireIntegration(ctx, second.ID, "policy")
	if err != nil {
		t.Fatal(err)
	}
	second.IntegrationToken = lease.Token
	if _, err = s.AcquireIntegration(ctx, third.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatal("second resource owner ignored", err)
	}
	if err = s.Cancel(ctx, second.FeatureID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	second.Stage = "cancelled"
	if err = s.Save(ctx, second, "cancelled", "sessions settled", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIntegration(ctx, third.ID, "policy"); err != nil {
		t.Fatal("settled cancellation did not release all resources", err)
	}
}

func TestAliasResourcesAndUncertainRestartRetention(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	p := model.Project{Repository: repo, Branch: "main"}
	q := p
	q.Repository = alias
	r1, err := config.IntegrationResources(p, "integrated")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := config.IntegrationResources(q, "integrated")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "state.sqlite")
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	a := lane(t, s, "owned")
	b := lane(t, s, "alias")
	if err = s.EnqueueIntegration(ctx, a, r1); err != nil {
		t.Fatal(err)
	}
	if err = s.EnqueueIntegration(ctx, b, r2); err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireIntegration(ctx, a.ID, "policy")
	if err != nil {
		t.Fatal(err)
	}
	a.IntegrationToken = lease.Token
	a.Stage = "uncertain"
	a.State = "blocked"
	if err = s.Save(ctx, a, "blocked", "remote push outcome unknown", "", true); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := model.Config{Projects: map[string]model.Project{"demo": p}}
	if err = s.PrepareIntegrationRecovery(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIntegration(ctx, b.ID, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatalf("restart/alias lost uncertain ownership: %v", err)
	}
	a.Deadline = time.Now().Add(-time.Hour)
	if err = s.Save(ctx, a, "blocked", "expired uncertain effects still owned", "", true); err != nil {
		t.Fatal(err)
	}
	cancelled := a
	cancelled.Stage = "cancelled"
	if err = s.Save(ctx, cancelled, "cancelled", "deadline expired", "", false); err == nil {
		t.Fatal("uncertain ownership silently settled without operator")
	}
	if err = s.ResolveFailed(ctx, a.FeatureID, a.ID, "remote ref inspected; external session stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIntegration(ctx, b.ID, "policy"); err != nil {
		t.Fatal("operator resolution failed to release", err)
	}
}

func legacyDatabase(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE features(id TEXT PRIMARY KEY,data BLOB NOT NULL);CREATE TABLE attempts(id TEXT PRIMARY KEY,feature_id TEXT NOT NULL,active INTEGER NOT NULL,revision TEXT NOT NULL,policy_hash TEXT NOT NULL,data BLOB NOT NULL);CREATE TABLE calls(id TEXT PRIMARY KEY,attempt_id TEXT NOT NULL,stage TEXT NOT NULL,day TEXT NOT NULL,UNIQUE(attempt_id,stage));CREATE TABLE sessions(attempt_id TEXT NOT NULL,thread_id TEXT NOT NULL,stage TEXT NOT NULL,PRIMARY KEY(attempt_id,thread_id));`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRecoveryRejectsConflictingLegacyUncertaintyAtomically(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "legacy.sqlite")
	db := legacyDatabase(t, name)
	for _, id := range []string{"legacy-one", "legacy-two", "inactive-legacy"} {
		f := feature(id)
		f.Revision = "revision"
		f.PolicyHash = "policy"
		f.State = "blocked"
		a := model.Attempt{ID: id + "-attempt", FeatureID: id, Stage: "uncertain", State: "blocked", StartedAt: time.Now().UTC()}
		active := 1
		if id == "inactive-legacy" {
			active = 0
		}
		fb, _ := json.Marshal(f)
		ab, _ := json.Marshal(a)
		if _, err := db.Exec(`INSERT INTO features VALUES(?,?)`, f.ID, fb); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO attempts VALUES(?,?,?,?,?,?)`, a.ID, f.ID, active, f.Revision, f.PolicyHash, ab); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM legacy_integration_recovery`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("legacy provenance did not preserve only active uncertainty: %d %v", n, err)
	}
	cfg := model.Config{Projects: map[string]model.Project{"demo": {Repository: t.TempDir(), Branch: "main"}}}
	if err := s.PrepareIntegrationRecovery(ctx, cfg); err == nil || !strings.Contains(err.Error(), "conflicting unresolved") {
		t.Fatalf("conflicting legacy ownership allowed: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM integration_requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed recovery partially backfilled queue: %d %v", n, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM integration_owners`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed recovery partially backfilled ownership: %d %v", n, err)
	}
	attempts, err := s.Active(ctx)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("read CLI blocked by recovery: %v", err)
	}
	if err = s.ResolveFailed(ctx, attempts[0].FeatureID, attempts[0].ID, "legacy external work inspected and stopped"); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM legacy_integration_recovery`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("operator resolution retained settled provenance: %d %v", n, err)
	}
	if err = s.PrepareIntegrationRecovery(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	status, err := s.IntegrationStatus(ctx, attempts[1].ID)
	if err != nil || status.State != "owned" || status.Token == "" {
		t.Fatalf("remaining legacy uncertainty was not protected: %+v %v", status, err)
	}
}

func TestCurrentUncertaintyDoesNotAcquireOnRecovery(t *testing.T) {
	for _, schema := range []struct {
		name    string
		version int
	}{{"schema1", 1}, {"schema2", 2}} {
		t.Run(schema.name, func(t *testing.T) {
			ctx := context.Background()
			name := filepath.Join(t.TempDir(), "state.sqlite")
			s, err := Open(name)
			if err != nil {
				t.Fatal(err)
			}
			build := lane(t, s, "build-uncertain")
			spawn := lane(t, s, "spawn-uncertain")
			owner := lane(t, s, "owner")
			first := lane(t, s, "first-waiter")
			last := lane(t, s, "last-waiter")
			build.Stage = "uncertain"
			build.State = "blocked"
			build.DispatchKey = build.ID + ":build:0"
			if err = s.Save(ctx, build, "blocked", "build spawn outcome unknown", "", true); err != nil {
				t.Fatal(err)
			}
			spawn.SourceBaseSHA = "source-base"
			spawn.SourceCandidateSHA = "source-candidate"
			spawn.Stage = "uncertain"
			spawn.State = "blocked"
			spawn.DispatchKey = spawn.ID + ":review:0"
			if err = s.Save(ctx, spawn, "blocked", "review spawn outcome unknown", "", true); err != nil {
				t.Fatal(err)
			}
			project := model.Project{Repository: t.TempDir(), Branch: "main"}
			cfg := model.Config{Projects: map[string]model.Project{"demo": project}}
			resources, err := config.IntegrationResources(project, "integrated")
			if err != nil {
				t.Fatal(err)
			}
			queued := []model.Attempt{owner, first, spawn, last}
			sequences := make(map[string]int64)
			for _, a := range queued {
				if err = s.EnqueueIntegration(ctx, a, resources); err != nil {
					t.Fatal(err)
				}
				status, err := s.IntegrationStatus(ctx, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				sequences[a.ID] = status.Sequence
			}
			lease, err := s.AcquireIntegration(ctx, owner.ID, "policy")
			if err != nil {
				t.Fatal(err)
			}
			owner.IntegrationToken = lease.Token
			owner.Stage = "integrating"
			if err = s.Save(ctx, owner, "running", "", "", true); err != nil {
				t.Fatal(err)
			}
			if schema.version == 1 {
				// Recreate the original schema-1 shape, before provenance existed.
				if _, err = s.db.Exec(`DROP TABLE legacy_integration_recovery;PRAGMA user_version=1`); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for range 2 {
				if err = s.PrepareIntegrationRecovery(ctx, cfg); err != nil {
					t.Fatal("non-delivery uncertainty conflicted with a valid owner", err)
				}
			}
			var n int
			if err = s.db.QueryRow(`SELECT count(*) FROM legacy_integration_recovery`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("current uncertainty misclassified as legacy: %d %v", n, err)
			}
			status, err := s.IntegrationStatus(ctx, build.ID)
			if err != nil || status.State != "none" || status.Token != "" {
				t.Fatalf("build uncertainty acquired a queue or ownership: %+v %v", status, err)
			}
			for i, a := range queued {
				status, err = s.IntegrationStatus(ctx, a.ID)
				if err != nil || status.Sequence != sequences[a.ID] {
					t.Fatalf("recovery reordered queue for %s: %+v %v", a.ID, status, err)
				}
				if i == 0 {
					if status.State != "owned" || status.Token != lease.Token {
						t.Fatalf("recovery changed protected ownership: %+v", status)
					}
				} else if status.State != "queued" || status.Token != "" || status.Position != i+1 || status.OwnerAttemptID != owner.ID {
					t.Fatalf("uncertainty bypassed FIFO or acquired ownership: %+v", status)
				}
			}
			active, err := s.Active(ctx)
			if err != nil || len(active) != 5 {
				t.Fatalf("uncertainty no longer active: %+v %v", active, err)
			}
			for _, a := range active {
				if (a.ID == build.ID || a.ID == spawn.ID) && (a.Stage != "uncertain" || a.IntegrationToken != "") {
					t.Fatalf("recovery changed non-delivery uncertainty: %+v", a)
				}
			}
			approved(t, s, "next")
			if _, err = s.Claim(ctx, "next", "policy", 4, 5, 5, 3600); !errors.Is(err, ErrWaiting) {
				t.Fatalf("active uncertainty stopped counting against admission: %v", err)
			}
			owner.Stage = "accepted"
			if err = s.Save(ctx, owner, "accepted", "", "receipt", false); err != nil {
				t.Fatal(err)
			}
			if err = s.PrepareIntegrationRecovery(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err = s.AcquireIntegration(ctx, last.ID, "policy"); !errors.Is(err, ErrWaiting) {
				t.Fatalf("last waiter bypassed FIFO: %v", err)
			}
			if _, err = s.AcquireIntegration(ctx, spawn.ID, "policy"); !errors.Is(err, ErrWaiting) {
				t.Fatalf("queued uncertainty bypassed older waiter: %v", err)
			}
			if _, err = s.AcquireIntegration(ctx, first.ID, "policy"); err != nil {
				t.Fatal("recovery blocked the legitimate FIFO head", err)
			}
		})
	}
}

func TestGenerationEvidenceAndDispatchIdempotence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := lane(t, s, "generation")
	a.CandidateSHA = "sha-zero"
	if err := s.Save(ctx, a, "running", "", "", true, model.Evidence{Kind: "review", CandidateSHA: a.CandidateSHA, Passed: true}); err != nil {
		t.Fatal(err)
	}
	key := a.ID + ":review:0"
	if err := s.ReserveCall(ctx, a, "review", key, 1); err != nil {
		t.Fatal(err)
	}
	active, err := s.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a = active[0]
	if err = s.ReserveCall(ctx, a, "review", key, 1); err != nil {
		t.Fatal("same key consumed budget twice", err)
	}
	if err = s.RecordSession(ctx, a.ID, "review", key, model.Session{ThreadID: "thread-zero", EnvironmentID: "env-zero"}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCheck(ctx, a.ID, "unit", 0, a.CandidateSHA, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCheck(ctx, a.ID, "unit", 0, a.CandidateSHA, "passed", "passed"); err != nil {
		t.Fatal(err)
	}
	old := a
	a.CandidateGeneration = 1
	a.CandidateSHA = "sha-one"
	a.Stage = "checks"
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, old, "running", "", "", true); err == nil {
		t.Fatal("stale generation overwrote current candidate")
	}
	if err = s.Save(ctx, a, "running", "", "", true, model.Evidence{Kind: "review", CandidateSHA: a.CandidateSHA, CandidateGeneration: 0, Passed: true}); err == nil {
		t.Fatal("stale generation accepted evidence")
	}
	ev, err := s.AttemptEvidence(ctx, a.ID)
	if err != nil || len(ev) != 0 {
		t.Fatalf("old generation proof reused: %+v %v", ev, err)
	}
	if err = s.ReserveCall(ctx, a, "review", key, 2); err == nil {
		t.Fatal("dispatch key reused across generations")
	}
	a.DispatchKey = a.ID + ":review:1"
	if err = s.ReserveCall(ctx, a, "review", a.DispatchKey, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCheck(ctx, a.ID, "unit", 0, "sha-zero", "passed", "late check"); err == nil {
		t.Fatal("late generation check accepted")
	}
	if err = s.Save(ctx, a, "running", "", "", true, model.Evidence{Kind: "review", CandidateSHA: a.CandidateSHA, CandidateGeneration: 1, Passed: true}); err != nil {
		t.Fatal(err)
	}
	ev, err = s.AttemptEvidence(ctx, a.ID)
	if err != nil || len(ev) != 1 || ev[0].CandidateGeneration != 1 {
		t.Fatalf("current generation missing: %+v %v", ev, err)
	}
}

func TestLegacyDispatchMigrationPreservesIdentityAndAllowance(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "legacy.sqlite")
	db := legacyDatabase(t, name)
	f := feature("legacy")
	f.Revision = "revision"
	f.PolicyHash = "policy"
	f.State = "running"
	a := model.Attempt{ID: "attempt", FeatureID: f.ID, Stage: "review_dispatching", State: "running", StartedAt: time.Now().UTC()}
	fb, _ := json.Marshal(f)
	ab, _ := json.Marshal(a)
	if _, err := db.Exec(`INSERT INTO features VALUES(?,?)`, f.ID, fb); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO attempts VALUES(?,?,1,?,?,?)`, a.ID, f.ID, f.Revision, "policy", ab); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO calls VALUES('call',?,'review',?)`, a.ID, time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active, err := s.Active(ctx)
	if err != nil || len(active) != 1 || active[0].DispatchKey != "attempt:review" {
		t.Fatalf("legacy dispatch identity not recovered: %+v %v", active, err)
	}
	if err = s.ReserveCall(ctx, active[0], "review", "attempt:review", 1); err != nil {
		t.Fatal("migration repeated reservation", err)
	}
	if err = s.ReserveCall(ctx, active[0], "build", "attempt:build:0", 1); !errors.Is(err, ErrWaiting) {
		t.Fatalf("migration reset budget: %v", err)
	}
}

func TestTerminalSettlementClosesInterruptedChecksAndQueue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := lane(t, s, "interrupted")
	a.CandidateSHA = "candidate"
	if err := s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueIntegration(ctx, a, []string{"git:repo:refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheck(ctx, a.ID, "unit", 0, a.CandidateSHA, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, a.FeatureID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a.Stage = "cancelled"
	if err := s.Save(ctx, a, "cancelled", "session stopped", "", false); err != nil {
		t.Fatal(err)
	}
	st, err := s.IntegrationStatus(ctx, a.ID)
	if err != nil || st.State != "settled" {
		t.Fatalf("queued cancellation remained queued: %+v %v", st, err)
	}
	var state, detail string
	if err = s.db.QueryRow(`SELECT state,detail FROM checks WHERE attempt_id=?`, a.ID).Scan(&state, &detail); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || !strings.Contains(detail, "before check completion was recorded") {
		t.Fatalf("phantom check after settlement: %s %s", state, detail)
	}
}

func TestAcceptedCandidateUsesAttemptNotDeploymentReceipt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f := feature("released")
	f.CompletionTarget = "staging"
	f, err := s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, f.ID, "policy", 3, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage = "accepted"
	a.State = "accepted"
	a.CandidateSHA = "immutable-accepted-sha"
	if err = s.Save(ctx, a, "accepted", "", "deployment://staging/release-42", false); err != nil {
		t.Fatal(err)
	}
	sha, err := s.AcceptedCandidate(ctx, f.ID)
	if err != nil || sha != a.CandidateSHA {
		t.Fatalf("deployment receipt mistaken for candidate: %s %v", sha, err)
	}
	f.Body = "new source revision"
	if _, err = s.Import(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptedCandidate(ctx, f.ID); err == nil {
		t.Fatal("historical accepted SHA reused for unaccepted revision")
	}
}
