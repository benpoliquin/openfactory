package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestProgressRetainsEveryObservedSessionAcrossStagesAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "openfactory.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	f := approved(t, s, "history")
	a := claim(t, s, f.ID)
	a.DispatchKey = "history-build"
	if err = s.ReserveCall(ctx, a, "build", a.DispatchKey, 10); err != nil {
		t.Fatal(err)
	}
	a.Stage, a.ThreadID = "build_running", "builder-thread"
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	// Polling the same thread must not duplicate its historical identity.
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	a.Stage, a.ThreadID = "checks", ""
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	a.DispatchKey = "history-review"
	if err = s.ReserveCall(ctx, a, "review", a.DispatchKey, 10); err != nil {
		t.Fatal(err)
	}
	a.Stage, a.ThreadID = "review_running", "reviewer-thread"
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	a.Stage, a.ThreadID, a.State = "accepted", "", "accepted"
	if err = s.Save(ctx, a, "accepted", "target integrated proven", "integrated-receipt", false); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, model.StatusQuery{}, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if status.ObservedAt.IsZero() || len(status.Features) != 1 {
		t.Fatalf("invalid settled status: %+v", status)
	}
	history := status.Features[0]
	if history.Feature.ID != f.ID || len(history.Sessions) != 2 {
		t.Fatalf("sessions lost or duplicated: %+v", history)
	}
	for i, want := range []struct{ thread, stage string }{{"reviewer-thread", "review"}, {"builder-thread", "build"}} {
		got := history.Sessions[i]
		if got.ThreadID != want.thread || got.Stage != want.stage || got.AttemptID != a.ID || got.CreatedAt.IsZero() {
			t.Fatalf("session %d: %+v", i, got)
		}
	}
	if len(history.Attempts) != 1 || history.Attempts[0].Active || history.Attempts[0].Stage != "accepted" {
		t.Fatalf("invalid settled attempt: %+v", history.Attempts)
	}
	var stages []string
	for _, interval := range history.Timeline {
		if interval.AttemptID == a.ID {
			stages = append(stages, interval.Stage)
		}
	}
	if !reflect.DeepEqual(stages, []string{"building", "checking", "reviewing", "integrated"}) {
		t.Fatalf("lost stage history after thread clears and restart: %+v", history.Timeline)
	}
	progress := durableProgressFacts(t, s, f.ID)
	last := progress[len(progress)-1]
	if last.State != "accepted" || last.Receipt != "integrated-receipt" || last.AttemptID != a.ID || last.Revision != f.Revision {
		t.Fatalf("missing historical acceptance facts: %+v", last)
	}
	changed := feature(f.ID)
	changed.Body = "A new immutable revision."
	changed.CompletionTarget = "reviewed"
	if _, err = s.Import(ctx, changed); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(ctx, model.StatusQuery{}, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Features) != 1 {
		t.Fatalf("re-import lost feature: %+v", status)
	}
	reimported := status.Features[0]
	if !reflect.DeepEqual(reimported.Sessions, history.Sessions) {
		t.Fatalf("re-import rewrote observed session history: %+v", reimported.Sessions)
	}
	if len(reimported.Timeline) != len(history.Timeline)+1 || !reflect.DeepEqual(reimported.Timeline[:len(history.Timeline)], history.Timeline) {
		t.Fatalf("re-import rewrote completed stage history: before=%+v after=%+v", history.Timeline, reimported.Timeline)
	}
	queued := reimported.Timeline[len(reimported.Timeline)-1]
	if queued.State != "proposed" || queued.Stage != "queued" || queued.AttemptID != "" || queued.FinishedAt != nil {
		t.Fatalf("old acceptance leaked into re-import: %+v", queued)
	}
	progress = durableProgressFacts(t, s, f.ID)
	previous, current := progress[len(progress)-2], progress[len(progress)-1]
	if !reflect.DeepEqual(previous, last) || previous.CompletionTarget != "integrated" || current.State != "proposed" || current.CompletionTarget != "reviewed" || current.Receipt != "" || current.Revision == previous.Revision {
		t.Fatalf("re-import rewrote event authority: previous=%+v current=%+v", previous, current)
	}
}

func TestProgressReportsOnlyRecoverableLegacySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := context.Background()
	approved(t, s, "legacy")
	a := claim(t, s, "legacy")
	for _, stage := range []string{"build", "review"} {
		a.DispatchKey = "legacy-" + stage
		if err := s.ReserveCall(ctx, a, stage, a.DispatchKey, 10); err != nil {
			t.Fatal(err)
		}
		a.Stage, a.ThreadID = stage+"_running", stage+"-thread"
		if err := s.Save(ctx, a, "running", "", "", true); err != nil {
			t.Fatal(err)
		}
	}
	a.Stage, a.State = "failed", "blocked"
	if err := s.Save(ctx, a, "blocked", "check unit failed: private output", "", false); err != nil {
		t.Fatal(err)
	}
	// Model a pre-history database. The overwritten builder identity genuinely
	// cannot be recovered; only the latest reviewer identity is still persisted.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions; DELETE FROM event_progress`); err != nil {
		t.Fatal(err)
	}
	for _, restart := range []bool{false, true} {
		if restart {
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
		}
		status, err := s.Status(ctx, model.StatusQuery{}, reportingConfig())
		if err != nil {
			t.Fatal(err)
		}
		if len(status.Features) != 1 {
			t.Fatalf("lost legacy feature: %+v", status)
		}
		sessions := status.Features[0].Sessions
		if len(sessions) != 1 || sessions[0].ThreadID != "review-thread" || sessions[0].Stage != "review" || sessions[0].AttemptID != a.ID || sessions[0].DispatchKey != "legacy-review" || !sessions[0].CreatedAt.IsZero() {
			t.Fatalf("fabricated or lost legacy identity (restart=%v): %+v", restart, sessions)
		}
		if progress := durableProgressFacts(t, s, a.FeatureID); len(progress) != 0 {
			t.Fatalf("reporting fabricated legacy progress: %+v", progress)
		}
	}
}

func TestSessionAndProgressWritesRollBackWithRejectedEvidence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "rollback")
	a := claim(t, s, "rollback")
	a.DispatchKey = "rollback-build"
	if err := s.ReserveCall(ctx, a, "build", a.DispatchKey, 10); err != nil {
		t.Fatal(err)
	}
	before, err := s.FeatureStatus(ctx, a.FeatureID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.EventPage(ctx, a.FeatureID, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempts, err := s.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeProgress := durableProgressFacts(t, s, a.FeatureID)
	a.Stage, a.ThreadID, a.CandidateSHA = "build_running", "not-committed-thread", "candidate"
	a.CandidateGeneration = 1
	if err := s.Save(ctx, a, "running", "", "", true,
		model.Evidence{ID: "rolled-back-proof", CandidateSHA: a.CandidateSHA, CandidateGeneration: a.CandidateGeneration},
		model.Evidence{CandidateSHA: "wrong-candidate", CandidateGeneration: a.CandidateGeneration},
	); err == nil || err.Error() != "evidence candidate or generation mismatch" {
		t.Fatalf("expected evidence rejection after transactional writes, got %v", err)
	}
	after, err := s.FeatureStatus(ctx, a.FeatureID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := s.EventPage(ctx, a.FeatureID, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	afterAttempts, err := s.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sessions) != 0 || len(afterAttempts) != 1 || afterAttempts[0].Stage != "build_dispatching" || afterAttempts[0].ThreadID != "" || afterAttempts[0].CandidateSHA != "" || afterAttempts[0].CandidateGeneration != 0 {
		t.Fatalf("rejected save leaked partial progress: status=%+v attempts=%+v", after, afterAttempts)
	}
	if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(afterEvents, beforeEvents) || !reflect.DeepEqual(afterAttempts, beforeAttempts) || !reflect.DeepEqual(durableProgressFacts(t, s, a.FeatureID), beforeProgress) {
		t.Fatal("rejected save changed durable reporting, attempt, event, or progress facts")
	}
	evidence, err := s.Evidence(ctx, a.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 0 {
		t.Fatalf("rejected save retained evidence: %+v", evidence)
	}
}

func TestProgressRecordsCancellationRatherThanRacedSuccess(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	approved(t, s, "cancel-race")
	a := claim(t, s, "cancel-race")
	if err := s.Cancel(ctx, a.FeatureID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a.Stage, a.State = "accepted", "accepted"
	if err := s.Save(ctx, a, "accepted", "target integrated proven", "receipt", true); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, model.StatusQuery{}, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Features) != 1 {
		t.Fatalf("lost cancelling feature: %+v", status)
	}
	progress := durableProgressFacts(t, s, a.FeatureID)
	last := progress[len(progress)-1]
	history := status.Features[0]
	if history.Feature.State != "cancelling" || last.State != "cancelling" || last.AttemptID != a.ID {
		t.Fatalf("raced save invented acceptance: status=%+v progress=%+v", history, last)
	}
	for _, interval := range history.Timeline {
		if interval.State == "accepted" || interval.Stage == "integrated" {
			t.Fatalf("raced success appeared in shared history: %+v", history.Timeline)
		}
	}
	wait := history.Timeline[len(history.Timeline)-1]
	if wait.State != "cancelling" || wait.AttemptID != a.ID || wait.FinishedAt != nil {
		t.Fatalf("cancellation wait was lost or closed: %+v", wait)
	}
}

func TestProgressReportsAllDurableSessions(t *testing.T) {
	s := openTest(t)
	f := approved(t, s, "many-sessions")
	a := claim(t, s, f.ID)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	at := time.Unix(1, 0).UTC().Format(time.RFC3339Nano)
	for range reportMaxLimit + 1 {
		if _, err = tx.Exec(`INSERT INTO sessions(attempt_id,thread_id,stage,dispatch_key,environment_id,created_at) VALUES(?,?,?,?,?,?)`, a.ID, ID(), "build", "dispatch", "", at); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(context.Background(), model.StatusQuery{}, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Features) != 1 || len(status.Features[0].Sessions) != reportMaxLimit+1 {
		t.Fatalf("shared reporting truncated durable sessions: %+v", status)
	}
	detail, err := s.FeatureStatus(context.Background(), f.ID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detail.Sessions, status.Features[0].Sessions) {
		t.Fatal("detail and shared status disagree on complete session history")
	}
}

func TestReportingHistoryKeepsBlockedAndUncertainWaitIntervalsOpen(t *testing.T) {
	for _, state := range []string{"blocked", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			at := func(second int64) time.Time { return time.Unix(second, 0).UTC() }
			var history reportIntervals
			history.append("building", "running", "attempt", at(1))
			history.append("building", state, "attempt", at(2))
			history.append("building", state, "attempt", at(3))
			open := history.result()
			if len(open) != 2 || open[0].FinishedAt == nil || !open[0].FinishedAt.Equal(at(2)) || open[1].FinishedAt != nil || !open[1].StartedAt.Equal(at(2)) {
				t.Fatalf("wait was closed or restarted by polling: %+v", open)
			}
			history.append("building", "running", "attempt", at(5))
			resumed := history.result()
			if len(resumed) != 3 || resumed[1].State != state || resumed[1].FinishedAt == nil || !resumed[1].FinishedAt.Equal(at(5)) || resumed[2].FinishedAt != nil {
				t.Fatalf("wait duration lost on resume: %+v", resumed)
			}
		})
	}
}

// Shared reporting intentionally projects Progress into timeline intervals rather
// than exposing immutable revision/receipt metadata. Read only those durable facts
// here so regressions cannot silently rewrite the authority of historical events.
func durableProgressFacts(t *testing.T, s *Store, featureID string) []Progress {
	t.Helper()
	rows, err := s.db.Query(`SELECT p.data FROM events e JOIN event_progress p ON p.event_id=e.id WHERE e.feature_id=? ORDER BY e.seq`, featureID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Progress
	for rows.Next() {
		var data []byte
		var progress Progress
		if err = rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &progress); err != nil {
			t.Fatal(err)
		}
		out = append(out, progress)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
