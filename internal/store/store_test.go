package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"openfactory/internal/model"
)

func feature(id string) model.Feature {
	return model.Feature{ID: id, Project: "demo", Title: "Implement " + id, Kind: "feature", Risk: "normal", CompletionTarget: "integrated", Checks: []string{"unit"}, Acceptance: []model.Acceptance{{ID: "behavior", Scenario: "Exercise behavior", Expected: "Expected result"}}, Body: "Bounded implementation."}
}
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func approved(t *testing.T, s *Store, id string) model.Feature {
	t.Helper()
	ctx := context.Background()
	f, err := s.Import(ctx, feature(id))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, id, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f, err = s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func claim(t *testing.T, s *Store, id string) model.Attempt {
	t.Helper()
	a, err := s.Claim(context.Background(), id, "policy", 3, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestImmutableImportApprovalAndCycles(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f := approved(t, s, "first")
	events, _ := s.Events(ctx, f.ID)
	again, err := s.Import(ctx, feature(f.ID))
	if err != nil || again.State != "ready" {
		t.Fatalf("idempotent import: %v %s", err, again.State)
	}
	after, _ := s.Events(ctx, f.ID)
	if len(events) != len(after) {
		t.Fatal("idempotent import emitted another event")
	}
	if err = s.Approve(ctx, f.ID, "wrong", "policy", model.TrustedLocalAudit()); err == nil {
		t.Fatal("approved wrong revision")
	}
	second := feature("second")
	second.DependsOn = []string{"first"}
	if _, err = s.Import(ctx, second); err != nil {
		t.Fatal(err)
	}
	f.DependsOn = []string{"second"}
	if _, err = s.Import(ctx, f); err == nil {
		t.Fatal("dependency cycle accepted")
	}
	missing := feature("missing")
	missing.DependsOn = []string{"not-enqueued"}
	if _, err = s.Import(ctx, missing); err == nil {
		t.Fatal("unresolved dependency accepted")
	}
	f = feature("first")
	f.Body = "Changed contract."
	updated, err := s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PolicyHash != "" || updated.State != "proposed" || updated.Revision == again.Revision {
		t.Fatal("revision retained authority")
	}
}
func TestProjectWriterAndIntegratedDependencyGate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "parent")
	child := feature("child")
	child.DependsOn = []string{"parent"}
	child, err := s.Import(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, child.ID, child.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, child.ID, "policy", 3, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("dependency gate: %v", err)
	}
	a := claim(t, s, "parent")
	approved(t, s, "other")
	if _, err = s.Claim(ctx, "other", "policy", 3, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatal("two project writers admitted")
	}
	if err = s.Save(ctx, a, "accepted", "integrated", "commit-receipt", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, child.ID, "policy", 3, 2, 1, 3600); err != nil {
		t.Fatal(err)
	}
}
func TestReviewedReceiptCannotUnlockCodeDependency(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f := feature("review")
	f.CompletionTarget = "reviewed"
	f, err := s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a := claim(t, s, f.ID)
	if err = s.Save(ctx, a, "accepted", "reviewed", "reviewed:sha", false); err != nil {
		t.Fatal(err)
	}
	g := feature("dependent")
	g.DependsOn = []string{f.ID}
	g, err = s.Import(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, g.ID, g.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, g.ID, "policy", 3, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatal("reviewed receipt unlocked code dependency")
	}
}
func TestDurableReservationCancellationAndEvidence(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "ledger.sqlite")
	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	approved(t, s, "work")
	a := claim(t, s, "work")
	if err = s.ReserveCall(ctx, a, "build", a.ID+":build", 1); err != nil {
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
	attempts, err := s.Active(ctx)
	if err != nil || len(attempts) != 1 || attempts[0].Stage != "build_dispatching" {
		t.Fatalf("lost dispatch intent: %+v %v", attempts, err)
	}
	if err = s.ReserveCall(ctx, attempts[0], "review", attempts[0].ID+":review", 1); !errors.Is(err, ErrWaiting) {
		t.Fatal("restart reset daily budget")
	}
	if err = s.Retry(ctx, "work", model.TrustedLocalAudit()); err == nil {
		t.Fatal("retry discarded live intent")
	}
	if err = s.Approve(ctx, "work", feature("work").Revision, "other", model.TrustedLocalAudit()); err == nil {
		t.Fatal("approval replaced active attempt authority")
	}
	a = attempts[0]
	a.CandidateSHA = "candidate"
	if err = s.Save(ctx, a, "running", "", "", true, model.Evidence{Kind: "check:unit", CandidateSHA: "wrong", Passed: true}); err == nil {
		t.Fatal("wrong candidate evidence accepted")
	}
	if err = s.Cancel(ctx, "work", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a.Stage = "reviewed"
	if err = s.Save(ctx, a, "accepted", "raced success", "receipt", true); err != nil {
		t.Fatal(err)
	}
	f, _ := s.Get(ctx, "work")
	if f.State != "cancelling" {
		t.Fatal("engine overwrote concurrent cancellation")
	}
	if err = s.BeginOperation(ctx, a, "policy"); !errors.Is(err, ErrWaiting) {
		t.Fatal("cancelled work authorized external operation")
	}
	a.Stage = "cancelled"
	if err = s.Save(ctx, a, "cancelled", "settled", "", false); err != nil {
		t.Fatal(err)
	}
	if err = s.Retry(ctx, "work", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, "work", "policy", 1, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatal("retry reset attempt allowance")
	}
}
func TestBackupPermissionsAndNotificationLedger(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "notify")
	events, err := s.PendingNotifications(ctx, "webhook", 100)
	if err != nil || len(events) != 2 {
		t.Fatalf("outbox: %d %v", len(events), err)
	}
	if err = s.AckNotification(ctx, "webhook", events[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordNotificationFailure(ctx, "webhook", events[1].ID, "connection refused"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingNotifications(ctx, "webhook", 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("backoff ignored: %v %v", pending, err)
	}
	other, _ := s.PendingNotifications(ctx, "cli", 100)
	if len(other) != 2 {
		t.Fatal("ack leaked across channels")
	}
	for range 8 {
		if err = s.RecordNotificationFailure(ctx, "webhook", events[1].ID, "failed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE notifications SET next_at='' WHERE channel='webhook'`); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingNotifications(ctx, "webhook", 100)
	if len(pending) != 0 {
		t.Fatal("delivery retry limit ignored")
	}
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	if err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %o", info.Mode().Perm())
	}
	copy, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if _, err = copy.Get(ctx, "notify"); err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(ctx, backup); err == nil {
		t.Fatal("backup overwrote existing file")
	}
}
func TestSourceReplacementBlockedWhileAttemptActive(t *testing.T) {
	s := openTest(t)
	approved(t, s, "active")
	claim(t, s, "active")
	f := feature("active")
	f.Body = "New revision"
	if _, err := s.Import(context.Background(), f); err == nil {
		t.Fatal("mutated active source")
	}
}

func TestResolveFailedRequiresExactBlockedOwnershipAndNeverAccepts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "uncertain")
	a := claim(t, s, "uncertain")
	if err := s.ResolveFailed(ctx, a.FeatureID, a.ID, "observed stopped"); err == nil {
		t.Fatal("resolved an ordinary running attempt")
	}
	a.Stage = "uncertain"
	a.State = "blocked"
	if err := s.Save(ctx, a, "blocked", "push outcome unknown", "", true); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []string{"", "   ", string(make([]byte, 4097))} {
		if err := s.ResolveFailed(ctx, a.FeatureID, a.ID, receipt); err == nil {
			t.Fatal("accepted invalid reconciliation receipt")
		}
	}
	if err := s.ResolveFailed(ctx, a.FeatureID, "wrong-attempt", "observed stopped"); err == nil {
		t.Fatal("resolved wrong attempt")
	}
	if err := s.ResolveFailed(ctx, "wrong-feature", a.ID, "observed stopped"); err == nil {
		t.Fatal("resolved another feature's attempt")
	}
	if err := s.ResolveFailed(ctx, a.FeatureID, a.ID, "BB session stopped; upstream ref inspected; no release remains active."); err != nil {
		t.Fatal(err)
	}
	f, err := s.Get(ctx, a.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != "blocked" || f.Attempts != 1 || f.Receipt != "" {
		t.Fatalf("operator attestation granted acceptance or budget: %+v", f)
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 0 {
		t.Fatal("operator reconciliation did not release ownership")
	}
	evidence, err := s.Evidence(ctx, f.ID)
	if err != nil || len(evidence) != 1 || evidence[0].Kind != "operator_reconciled_failure" || evidence[0].Passed {
		t.Fatalf("missing failure attestation: %+v %v", evidence, err)
	}
	if err = s.ResolveFailed(ctx, f.ID, a.ID, "second receipt"); err == nil {
		t.Fatal("resolved already settled attempt")
	}
	if err = s.Retry(ctx, f.ID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, f.ID, "policy", 1, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatal("reconciliation reset attempt budget")
	}
}

func TestHistoricalEvidenceCannotSatisfyNewRevision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "revision")
	a := claim(t, s, "revision")
	a.CandidateSHA = "candidate"
	if err := s.Save(ctx, a, "blocked", "known failed attempt", "", false, model.Evidence{Kind: "check:unit", CandidateSHA: "candidate", Passed: true, Detail: "old check output"}); err != nil {
		t.Fatal(err)
	}
	f := feature("revision")
	f.Body = "Changed approved behavior."
	f, err := s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "new-policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AttemptEvidence(ctx, a.ID); err == nil {
		t.Fatal("old revision evidence reused under new policy")
	}
	historical, err := s.Evidence(ctx, f.ID)
	if err != nil || len(historical) != 1 {
		t.Fatal("historical evidence was destroyed")
	}
}

func TestGlobalCapacityAllowsSeparateProjectsButNeverExceedsLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, name := range []string{"one", "two", "three"} {
		f := feature(name)
		f.Project = name
		f, err := s.Import(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Claim(ctx, "one", "policy", 3, 2, 1, 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, "two", "policy", 3, 2, 1, 3600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, "three", "policy", 3, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatal("global concurrency exceeded")
	}
}
