package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"openfactory/internal/model"
)

// These consumers use the authenticated public command boundary for durable Save
// and explicit draft consent. Scheduler operations below are the ordinary Store
// APIs; no test directly fabricates an intake step, grant, attempt, or reservation.
func progressRequest(t *testing.T, s *Store, session model.BrowserSession, cfg model.Config) model.IntakeRequest {
	t.Helper()
	ctx := context.Background()
	create := commandTestCommand(t, lifecycleUUID(), model.ActionRequestCreate, model.RequestCreatePayload{
		Project: "demo", Text: "Keep this exact saved intake private until explicit bounded drafting consent.", ClientMessageID: lifecycleUUID(),
	})
	receipt, err := s.ExecuteCommand(ctx, session.TokenHash, session.CSRFHash, create, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var r model.IntakeRequest
	if err = json.Unmarshal(receipt.Result, &r); err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeCollecting || r.ActiveStepID != "" || r.LifetimeDispatches != 0 {
		t.Fatalf("Save scheduled intake work: %+v", r)
	}
	disclosure, err := s.IntakeDisclosure(ctx, cfg, r.OperatorID, r.Project, r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := commandTestCommand(t, lifecycleUUID(), model.ActionIntakeDraft, model.IntakeDraftPayload{Disclosure: disclosure})
	command.RequestID, command.ExpectedContentVersion = r.ID, &r.ContentVersion
	if _, err = s.ExecuteCommand(ctx, session.TokenHash, session.CSRFHash, command, cfg, nil); err != nil {
		t.Fatal(err)
	}
	r, err = s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeDraftQueued || r.ActiveStepID != "" || r.LifetimeDispatches != 0 {
		t.Fatalf("draft consent claimed or dispatched work: %+v", r)
	}
	return r
}

func progressStep(t *testing.T, s *Store, r model.IntakeRequest, cfg model.Config) model.IntakeStep {
	t.Helper()
	step, err := s.CreateIntakeStep(context.Background(), r.ID, r.ContentVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func progressClaim(t *testing.T, s *Store, step model.IntakeStep, cfg model.Config) model.Attempt {
	t.Helper()
	a, err := s.Claim(context.Background(), step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, cfg.Projects["demo"].MaxConcurrent, 3600)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func progressRead(t *testing.T, s *Store, id string) model.IntakeRequest {
	t.Helper()
	ctx := context.Background()
	r, err := s.IntakeRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListIntakeRequests(ctx, r.OperatorID, r.Project, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page {
		if item.ID == r.ID {
			if !reflect.DeepEqual(item, r) {
				t.Fatalf("history and detail disagree: history=%+v detail=%+v", item, r)
			}
			return r
		}
	}
	t.Fatalf("request missing from consumer history: %s", id)
	return r
}

// Internal features deliberately have no public product-board detail. An
// unclaimed ordinary feature in the same project is the public admission oracle
// for pause/capacity; it has no intake-specific gate and is never claimed.
func progressAdmissionProbe(t *testing.T, s *Store, cfg model.Config) {
	t.Helper()
	ctx := context.Background()
	f, err := s.Import(ctx, feature("intake-admission-progress-probe"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
}

func progressCanonicalWait(t *testing.T, s *Store, cfg model.Config, code string) *model.WaitReason {
	t.Helper()
	status, err := s.FeatureStatus(context.Background(), "intake-admission-progress-probe", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status.Wait == nil || status.Wait.Code != code {
		t.Fatalf("ordinary admission wait: %+v, want code %s", status.Wait, code)
	}
	return status.Wait
}

func progressUnclaimed(t *testing.T, s *Store, original model.IntakeRequest, step model.IntakeStep, cfg model.Config, code string) model.IntakeRequest {
	t.Helper()
	r := progressRead(t, s, original.ID)
	wait := progressCanonicalWait(t, s, cfg, code)
	if r.State != model.IntakeDraftQueued || r.ActiveStepID != step.ID || !reflect.DeepEqual(r.Wait, wait) {
		t.Fatalf("unclaimed intake progress differs from ordinary admission: request=%+v gate=%+v", r, wait)
	}
	if r.ContentVersion != original.ContentVersion || r.ContextManifest.SHA != original.ContextManifest.SHA || r.MaxDraftDispatches != original.MaxDraftDispatches || r.LifetimeDispatches != 0 {
		t.Fatalf("progress changed frozen content, consent, or budget: original=%+v current=%+v", original, r)
	}
	steps, err := s.IntakeSteps(context.Background(), r.ID)
	if err != nil || len(steps) != 1 || steps[0].ID != step.ID || steps[0].AttemptID != "" || steps[0].State != "ready" {
		t.Fatalf("waiting created/replaced/admitted a step: %+v %v", steps, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts WHERE feature_id=?`, 0, step.FeatureID)
	commandTestCount(t, s, `SELECT count(*) FROM calls WHERE attempt_id IN (SELECT id FROM attempts WHERE feature_id=?)`, 0, step.FeatureID)
	commandTestCount(t, s, `SELECT count(*) FROM intake_dispatches WHERE request_id=?`, 0, r.ID)
	return r
}

func TestIntakeQueuedProgressUsesOrdinaryAdmissionUntilRealClaim(t *testing.T) {
	for _, tc := range []struct {
		name, code      string
		global, project int
	}{
		{name: "global capacity", code: "global_limit", global: 2, project: 2},
		{name: "project capacity", code: "project_limit", global: 3, project: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			_, _, session := authTestActor(t, s, time.Now().UTC(), "progress")
			cfg := commandTestConfig()
			cfg.MaxConcurrent = tc.global
			project := cfg.Projects["demo"]
			project.MaxConcurrent = tc.project
			cfg.Projects["demo"] = project
			progressAdmissionProbe(t, s, cfg)
			held := make([]model.Attempt, 0, 2)
			for range 2 {
				r := progressRequest(t, s, session, cfg)
				a := progressClaim(t, s, progressStep(t, s, r, cfg), cfg)
				if err := s.ReserveCall(ctx, a, "intake", a.ID+":intake", cfg.MaxCallsPerDay); err != nil {
					t.Fatal(err)
				}
				held = append(held, a)
			}
			r := progressRequest(t, s, session, cfg)
			step := progressStep(t, s, r, cfg)
			queued := progressUnclaimed(t, s, r, step, cfg, tc.code)
			// Both direct Claim rejection and repeated controller/read cycles keep
			// the exact canonical wait without another job, charge, or event.
			for range 3 {
				if _, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, project.MaxConcurrent, 3600); !errors.Is(err, ErrWaiting) {
					t.Fatalf("capacity gate escaped Claim: %v", err)
				}
				if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
					t.Fatal(err)
				}
				current := progressUnclaimed(t, s, r, step, cfg, tc.code)
				if current.EventSequence != queued.EventSequence {
					t.Fatalf("unchanged progress emitted another event: before=%d after=%d", queued.EventSequence, current.EventSequence)
				}
			}
			commandTestCount(t, s, `SELECT count(*) FROM attempts`, 2)
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 2)
			// An externally unsettled owner remains counted even while waiting.
			if err := s.SetWait(ctx, held[0].ID, &model.WaitReason{Code: "bb_session_reconciling", Detail: "external ownership remains unsettled"}); err != nil {
				t.Fatal(err)
			}
			if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			progressUnclaimed(t, s, r, step, cfg, tc.code)
			held[0].Stage, held[0].State = "failed", "blocked"
			if err := s.Save(ctx, held[0], "blocked", "settled external owner", "", false); err != nil {
				t.Fatal(err)
			}
			if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			available := progressRead(t, s, r.ID)
			if available.State != model.IntakeDraftQueued || available.Wait != nil || available.EventSequence <= queued.EventSequence {
				t.Fatalf("capacity release falsely started work or retained its wait: %+v", available)
			}
			commandTestCount(t, s, `SELECT count(*) FROM attempts`, 2)
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 2)
			a := progressClaim(t, s, step, cfg)
			started := progressRead(t, s, r.ID)
			bound, err := s.GetIntakeStep(ctx, step.ID)
			if err != nil || bound.AttemptID != a.ID || started.State != model.IntakeDrafting || started.Wait != nil || started.ActiveStepID != step.ID || started.LifetimeDispatches != 0 || started.ContentVersion != r.ContentVersion {
				t.Fatalf("Claim did not atomically publish exact attempt ownership: request=%+v step=%+v attempt=%+v err=%v", started, bound, a, err)
			}
			commandTestCount(t, s, `SELECT count(*) FROM attempts WHERE feature_id=?`, 1, step.FeatureID)
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 2)
			if err = s.ReserveCall(ctx, a, "intake", a.ID+":intake", cfg.MaxCallsPerDay); err != nil {
				t.Fatal(err)
			}
			if got := progressRead(t, s, r.ID); got.LifetimeDispatches != 1 || got.MaxDraftDispatches != r.MaxDraftDispatches {
				t.Fatalf("ordinary dispatch lost frozen lifetime accounting: %+v", got)
			}
			commandTestCount(t, s, `SELECT count(*) FROM calls`, 3)
		})
	}
}

func TestIntakeQueuedPauseWaitRefreshesAndClearsWithoutAdmission(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _, session := authTestActor(t, s, time.Now().UTC(), "paused-progress")
	cfg := commandTestConfig()
	progressAdmissionProbe(t, s, cfg)
	r := progressRequest(t, s, session, cfg)
	step := progressStep(t, s, r, cfg)
	if err := s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("paused Claim did not wait: %v", err)
	}
	paused := progressUnclaimed(t, s, r, step, cfg, "paused")
	if err := s.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cleared := progressRead(t, s, r.ID)
	if cleared.State != model.IntakeDraftQueued || cleared.Wait != nil || cleared.ContentVersion != r.ContentVersion || cleared.EventSequence <= paused.EventSequence {
		t.Fatalf("resume falsely admitted work or retained pause: %+v", cleared)
	}
	if err := s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	progressUnclaimed(t, s, r, step, cfg, "paused")
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM intake_dispatches`, 0)
}

func TestIntakeActiveWaitTracksDailyGateAndClearsWithoutResettingCounters(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _, session := authTestActor(t, s, time.Now().UTC(), "daily-progress")
	cfg := commandTestConfig()
	cfg.MaxConcurrent = 2
	project := cfg.Projects["demo"]
	project.MaxConcurrent = 2
	cfg.Projects["demo"] = project
	first := progressRequest(t, s, session, cfg)
	owner := progressClaim(t, s, progressStep(t, s, first, cfg), cfg)
	if err := s.ReserveCall(ctx, owner, "intake", owner.ID+":intake", 1); err != nil {
		t.Fatal(err)
	}
	r := progressRequest(t, s, session, cfg)
	step := progressStep(t, s, r, cfg)
	a := progressClaim(t, s, step, cfg)
	key := a.ID + ":intake"
	err := s.ReserveCall(ctx, a, "intake", key, 1)
	if !errors.Is(err, ErrWaiting) {
		t.Fatalf("daily gate did not retain the admitted owner: %v", err)
	}
	wait := &model.WaitReason{Code: "model_call_limit", Detail: err.Error()}
	if err = s.SetWait(ctx, a.ID, wait); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = s.ReconcileIntakeRequests(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		got := progressRead(t, s, r.ID)
		if got.State != model.IntakeDrafting || !reflect.DeepEqual(got.Wait, wait) || got.LifetimeDispatches != 0 || got.MaxDraftDispatches != r.MaxDraftDispatches || got.ContentVersion != r.ContentVersion {
			t.Fatalf("active daily wait lost its canonical progress or reset authority: %+v", got)
		}
		active, activeErr := s.Active(ctx)
		if activeErr != nil || len(active) != 2 {
			t.Fatalf("daily wait released or duplicated ownership: %+v %v", active, activeErr)
		}
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 2)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM intake_dispatches`, 1)
	if err = s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	paused := &model.WaitReason{Code: "paused", Detail: "OpenFactory paused"}
	if err = s.SetWait(ctx, a.ID, paused); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := progressRead(t, s, r.ID); got.State != model.IntakeDrafting || !reflect.DeepEqual(got.Wait, paused) || got.LifetimeDispatches != 0 {
		t.Fatalf("active pause lost admitted ownership or changed counters: %+v", got)
	}
	if err = s.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	// Preserve all existing wait fields, not just the UI's known codes.
	resourceWait := &model.WaitReason{Code: "integration_owner", Resource: "project:demo", OwnerID: owner.ID, FeatureID: owner.FeatureID}
	if err = s.SetWait(ctx, a.ID, resourceWait); err != nil {
		t.Fatal(err)
	}
	if got := progressRead(t, s, r.ID); !reflect.DeepEqual(got.Wait, resourceWait) {
		t.Fatalf("active wait resource/owner fields were reinterpreted: %+v", got.Wait)
	}
	if err = s.SetWait(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := progressRead(t, s, r.ID); got.Wait != nil || got.State != model.IntakeDrafting {
		t.Fatalf("clearing the owned attempt wait did not clear request progress: %+v", got)
	}
	if err = s.SetWait(ctx, a.ID, wait); err != nil {
		t.Fatal(err)
	}
	// This is the engine's existing save-before-reservation path. A successful
	// active save must clear the old daily wait immediately, not a cycle later.
	a.Wait = nil
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	if got := progressRead(t, s, r.ID); got.Wait != nil || got.LifetimeDispatches != 0 {
		t.Fatalf("successful active save retained stale daily progress: %+v", got)
	}
	if err = s.ReserveCall(ctx, a, "intake", key, 1); !errors.Is(err, ErrWaiting) {
		t.Fatalf("clearing progress reset the consumed day allowance: %v", err)
	}
	if err = s.SetWait(ctx, a.ID, wait); err != nil {
		t.Fatal(err)
	}
	// A larger available allowance permits exactly the existing dispatch;
	// historical day and lifetime charges must remain present, not be reset.
	if err = s.ReserveCall(ctx, a, "intake", key, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got := progressRead(t, s, r.ID)
	retained := progressRead(t, s, first.ID)
	if got.Wait != nil || got.State != model.IntakeDrafting || got.LifetimeDispatches != 1 || got.MaxDraftDispatches != r.MaxDraftDispatches || retained.LifetimeDispatches != 1 {
		t.Fatalf("reservation did not clear wait while preserving lifetime charges: current=%+v previous=%+v", got, retained)
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 2)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 2)
	commandTestCount(t, s, `SELECT count(*) FROM intake_dispatches`, 2)
}

func TestIntakeProgressStorageFailuresRemainFatalAndAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _, session := authTestActor(t, s, time.Now().UTC(), "progress-failure")
	cfg := commandTestConfig()
	r := progressRequest(t, s, session, cfg)
	step := progressStep(t, s, r, cfg)
	inject := func() {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER intake_progress_failure BEFORE UPDATE ON intake_requests BEGIN SELECT RAISE(ABORT,'injected intake progress failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	remove := func() {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `DROP TRIGGER intake_progress_failure`); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	inject()
	if _, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600); err == nil || errors.Is(err, ErrWaiting) {
		t.Fatalf("wait persistence failure was masked as an ordinary queue gate: %v", err)
	}
	if got := progressRead(t, s, r.ID); got.Wait != nil || got.State != model.IntakeDraftQueued {
		t.Fatalf("failed wait transaction changed request progress: %+v", got)
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 0)
	remove()
	if _, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("durable waiting path failed after storage recovery: %v", err)
	}
	before := progressRead(t, s, r.ID)
	if err := s.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	inject()
	if _, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600); err == nil {
		t.Fatal("admission committed without its consumer-visible transition")
	}
	got := progressRead(t, s, r.ID)
	bound, err := s.GetIntakeStep(ctx, step.ID)
	if err != nil || !reflect.DeepEqual(got, before) || bound.AttemptID != "" || bound.State != "ready" {
		t.Fatalf("failed Claim partially committed: before=%+v after=%+v step=%+v err=%v", before, got, bound, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM features WHERE id=? AND json_extract(data,'$.attempts')=0 AND json_extract(data,'$.state')='ready'`, 1, step.FeatureID)
	remove()
	a := progressClaim(t, s, step, cfg)
	inject()
	if err = s.SetWait(ctx, a.ID, &model.WaitReason{Code: "model_call_limit", Detail: "daily gate"}); err == nil {
		t.Fatal("attempt wait committed without its request projection")
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 1 || active[0].Wait != nil || progressRead(t, s, r.ID).Wait != nil {
		t.Fatalf("failed wait projection partially changed attempt or request: %+v %v", active, err)
	}
	remove()
}

func TestIntakeQueuedCancellationCannotBeOverwrittenByWaitRefresh(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _, session := authTestActor(t, s, time.Now().UTC(), "cancel-progress")
	cfg := commandTestConfig()
	r := progressRequest(t, s, session, cfg)
	step := progressStep(t, s, r, cfg)
	cancel := commandTestCommand(t, lifecycleUUID(), model.ActionIntakeCancel, model.EmptyCommandPayload{})
	cancel.RequestID, cancel.ExpectedContentVersion = r.ID, &r.ContentVersion
	if _, err := s.ExecuteCommand(ctx, session.TokenHash, session.CSRFHash, cancel, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("cancelled queued step escaped Claim: %v", err)
	}
	if got := progressRead(t, s, r.ID); got.State != model.IntakeCancelling || got.ContentVersion != r.ContentVersion+1 {
		t.Fatalf("Claim wait resurrected a cancelled request: %+v", got)
	}
	if err := s.ReconcileIntakeRequests(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := progressRead(t, s, r.ID); got.State != model.IntakeCancelled || got.ActiveStepID != "" || got.LifetimeDispatches != 0 {
		t.Fatalf("non-admitted cancellation retained fictitious ownership: %+v", got)
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 0)
}
