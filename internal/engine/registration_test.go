package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"openfactory/internal/model"
)

type projectRegistrationError struct {
	uncertain bool
}

func (e projectRegistrationError) Error() string {
	return "project registration listing cannot yet establish the create outcome"
}
func (e projectRegistrationError) Uncertain() bool { return e.uncertain }

// registrationBackend represents the BB registration contract: ambiguous create
// outcomes remain uncertain until EnsureProject can return an exact listing match.
// Actual list/create request counts are covered by the BB client's tests.
type registrationBackend struct {
	*testBackend
	registrationMu sync.Mutex
	projectErrors  map[string]error
	lookups        map[string]int
}

func (b *registrationBackend) EnsureProject(_ context.Context, name, _ string) (string, error) {
	b.registrationMu.Lock()
	defer b.registrationMu.Unlock()
	b.lookups[name]++
	if err := b.projectErrors[name]; err != nil {
		return "", err
	}
	return "project-" + name, nil
}

func (b *registrationBackend) setProjectError(name string, err error) {
	b.registrationMu.Lock()
	defer b.registrationMu.Unlock()
	b.projectErrors[name] = err
}

func registrationFixture(t *testing.T) (*Engine, *registrationBackend) {
	t.Helper()
	e, _, backend, _ := fixture(t)
	b := &registrationBackend{testBackend: backend, projectErrors: map[string]error{}, lookups: map[string]int{}}
	e.backend = b
	return e, b
}

func TestAmbiguousProjectRegistrationWaitsWithoutCallsAndDoesNotPoisonOtherLanes(t *testing.T) {
	e, b := registrationFixture(t)
	p := e.cfg.Projects["demo"]
	p.MaxConcurrent = 2
	e.cfg.Projects["demo"] = p
	p.Repository = "/trusted/other"
	e.cfg.Projects["other"] = p
	e.cfg.MaxConcurrent = 3
	b.setProjectError("demo", projectRegistrationError{uncertain: true})
	for _, id := range []string{"waiting-one", "waiting-two", "unrelated"} {
		f := contract(id)
		f.CompletionTarget = "reviewed"
		if id == "unrelated" {
			f.Project = "other"
		}
		enqueue(t, e.db, f)
	}
	ticks(t, e, 10)
	active, err := e.db.Active(context.Background())
	if err != nil || len(active) != 2 {
		t.Fatalf("ambiguous registration terminated waiting lanes: %+v %v", active, err)
	}
	for _, a := range active {
		if a.Stage != "build_pending" || a.DispatchKey != "" || a.Wait == nil || a.Wait.Code != "bb_project_reconciling" {
			t.Fatalf("registration ambiguity consumed a spawn intent instead of waiting: %+v", a)
		}
	}
	if got := state(t, e.db, "unrelated"); got.State != "accepted" {
		t.Fatalf("ambiguous project poisoned independent project progress: %+v", got)
	}
	status, err := e.db.Status(context.Background(), model.StatusQuery{}, e.cfg)
	if err != nil || status.Calls.Used != 2 || len(b.requests) != 2 {
		t.Fatalf("registration waits consumed model calls: status=%+v requests=%d err=%v", status.Calls, len(b.requests), err)
	}
	b.setProjectError("demo", nil)
	ticks(t, e, 12)
	for _, id := range []string{"waiting-one", "waiting-two"} {
		if got := state(t, e.db, id); got.State != "accepted" || got.Attempts != 1 {
			t.Fatalf("exact registration match did not resume original lane: %+v", got)
		}
	}
	if len(b.requests) != 6 {
		t.Fatalf("registration recovery duplicated or omitted dispatches: %d", len(b.requests))
	}
}

func TestProjectRegistrationWaitRetainsOriginalAttemptDeadline(t *testing.T) {
	e, b := registrationFixture(t)
	b.setProjectError("demo", projectRegistrationError{uncertain: true})
	enqueue(t, e.db, contract("registration-deadline"))
	ticks(t, e, 4)
	active, err := e.db.Active(context.Background())
	if err != nil || len(active) != 1 || active[0].Wait == nil || active[0].Wait.Code != "bb_project_reconciling" {
		t.Fatalf("missing registration wait: %+v %v", active, err)
	}
	a := active[0]
	a.Deadline = time.Now().Add(-time.Second)
	if err = e.db.Save(context.Background(), a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 1)
	active, err = e.db.Active(context.Background())
	if got := state(t, e.db, a.FeatureID); got.State != "blocked" || err != nil || len(active) != 0 || len(b.requests) != 0 {
		t.Fatalf("registration wait reset deadline or retained nonexistent agent: %+v active=%+v err=%v", got, active, err)
	}
}

func TestDefinitiveProjectRegistrationRejectionBlocksWithoutCalls(t *testing.T) {
	e, b := registrationFixture(t)
	b.setProjectError("demo", projectRegistrationError{uncertain: false})
	enqueue(t, e.db, contract("registration-rejected"))
	ticks(t, e, 4)
	active, err := e.db.Active(context.Background())
	if got := state(t, e.db, "registration-rejected"); got.State != "blocked" || len(b.requests) != 0 || err != nil || len(active) != 0 {
		t.Fatalf("definitive registration rejection retained or spawned work: %+v active=%+v err=%v", got, active, err)
	}
}

func TestSpawnReconciliationProjectWaitPreservesExactReservedIdentity(t *testing.T) {
	for _, outcome := range []string{"resume", "deadline", "definitive-rejection"} {
		t.Run(outcome, func(t *testing.T) {
			e, b := registrationFixture(t)
			b.spawnErr = errors.New("spawn accepted but response lost")
			enqueue(t, e.db, contract("recover-registration"))
			ticks(t, e, 3)
			if len(b.requests) != 1 {
				t.Fatal("missing reserved uncertain spawn")
			}
			marker := b.requests[0].AttemptID
			b.setProjectError("demo", projectRegistrationError{uncertain: true})
			ticks(t, e, 2)
			active, err := e.db.Active(context.Background())
			if err != nil || len(active) != 1 || active[0].Stage != "build_dispatching" || active[0].DispatchKey != marker || active[0].Wait == nil || active[0].Wait.Code != "bb_project_reconciling" || len(b.requests) != 1 {
				t.Fatalf("registration wait lost reserved identity: %+v requests=%d %v", active, len(b.requests), err)
			}
			if outcome == "resume" {
				b.setProjectError("demo", nil)
				ticks(t, e, 18)
				if got := state(t, e.db, "recover-registration"); got.State != "accepted" || len(b.requests) != 2 {
					t.Fatalf("registration recovery duplicated uncertain spawn: %+v requests=%d", got, len(b.requests))
				}
				return
			}
			if outcome == "deadline" {
				a := active[0]
				a.Deadline = time.Now().Add(-time.Second)
				if err = e.db.Save(context.Background(), a, "running", "", "", true); err != nil {
					t.Fatal(err)
				}
			} else {
				b.setProjectError("demo", projectRegistrationError{uncertain: false})
			}
			ticks(t, e, 1)
			active, err = e.db.Active(context.Background())
			if got := state(t, e.db, "recover-registration"); got.State != "blocked" || err != nil || len(active) != 1 || active[0].Stage != "uncertain" || len(b.requests) != 1 {
				t.Fatalf("unreconciled spawn ownership was discarded: %+v active=%+v err=%v", got, active, err)
			}
		})
	}
}
