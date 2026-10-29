package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// Both consumers use the actual Store. Only the semantic output and the exact
// pre-publication race are deterministic; neither supplies product evidence.
type planningFailureBackend struct {
	*testBackend
	questionThread string
	questionOutput string
}

func (b *planningFailureBackend) Poll(ctx context.Context, thread, cursor string) (model.Observation, error) {
	observation, err := b.testBackend.Poll(ctx, thread, cursor)
	if err == nil && observation.State == "completed" && thread == b.questionThread {
		observation.Output = b.questionOutput
	}
	return observation, err
}

type planningFailureRepositories struct {
	*testRepos
	mu                sync.Mutex
	fingerprintPath   string
	beforeFingerprint func() error
}

func (r *planningFailureRepositories) Prepare(ctx context.Context, project string, enrollment model.Project, attempt string) (model.Workspace, error) {
	workspace, err := r.testRepos.Prepare(ctx, project, enrollment, attempt)
	workspace.Path = filepath.Join(workspace.Path, attempt)
	return workspace, err
}

func (r *planningFailureRepositories) Fingerprint(ctx context.Context, path string) (string, error) {
	r.mu.Lock()
	var hook func() error
	if path == r.fingerprintPath {
		hook = r.beforeFingerprint
		r.beforeFingerprint = nil
	}
	r.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return "", err
		}
	}
	return r.testRepos.Fingerprint(ctx, path)
}

func TestPlanningQuestionDomainFailuresDoNotStopHealthyConsumers(t *testing.T) {
	for _, consumer := range []string{"Tick", "Run"} {
		for _, failure := range []string{"whitespace-question", "cancellation-race"} {
			t.Run(consumer+"/"+failure, func(t *testing.T) {
				e, db, backend, repositories := fixture(t)
				ctx := context.Background()
				project := e.cfg.Projects["demo"]
				project.MaxConcurrent = 2
				e.cfg.Projects["demo"] = project
				e.cfg.PollSeconds = 1
				plan, err := intake.NewPlan("affected-plan", "demo", "Clarify delivery", "Ask before choosing durability guarantees.")
				if err != nil {
					t.Fatal(err)
				}
				healthy, err := intake.NewPlan("healthy-plan", "demo", "Plan independent work", "Propose an unrelated bounded feature without inheriting approval.")
				if err != nil {
					t.Fatal(err)
				}
				backend.idle = true
				payload, err := json.Marshal(planResult{Features: []model.Feature{contract("healthy-proposal")}})
				if err != nil {
					t.Fatal(err)
				}
				backend.plan = string(payload)
				b := &planningFailureBackend{testBackend: backend, questionOutput: `{"question":"Must acknowledgement survive process loss?"}`}
				r := &planningFailureRepositories{testRepos: repositories}
				e.backend, e.repos = b, r
				enqueue(t, db, plan)
				ticks(t, e, 3)
				active, err := db.Active(ctx)
				if err != nil || len(active) != 1 || active[0].Stage != "plan_running" || active[0].ThreadID == "" {
					t.Fatalf("affected plan did not reach durable running ownership: %+v %v", active, err)
				}
				a := active[0]
				b.questionThread = a.ThreadID
				enqueue(t, db, healthy)
				ticks(t, e, 3)
				active, err = db.Active(ctx)
				if err != nil || len(active) != 2 {
					t.Fatalf("healthy plan did not share ordinary capacity: %+v %v", active, err)
				}
				var raceErr error
				raced := false
				if failure == "whitespace-question" {
					b.questionOutput = "{\"question\":\" \\t\\n \"}"
				} else {
					// finishPlan has already loaded its active parent before this
					// callback. The real transaction must see cancellation, reject
					// the question with stale_subject, and settle only this attempt.
					r.fingerprintPath = a.Workspace
					r.beforeFingerprint = func() error {
						raced = true
						raceErr = db.Cancel(ctx, plan.ID, model.TrustedLocalAudit())
						return raceErr
					}
				}
				backend.idle = false
				if consumer == "Tick" {
					ticks(t, e, 1)
				} else {
					func() {
						runctx, cancel := context.WithCancel(ctx)
						exited := make(chan struct{})
						var runErr error
						go func() {
							runErr = e.Run(runctx)
							close(exited)
						}()
						defer func() {
							cancel()
							select {
							case <-exited:
								if !errors.Is(runErr, context.Canceled) {
									t.Errorf("Run returned a fatal per-plan failure: %v", runErr)
								}
							case <-time.After(3 * time.Second):
								t.Error("Run did not drain planning workers")
							}
						}()
						deadline := time.After(3 * time.Second)
						poll := time.NewTicker(10 * time.Millisecond)
						defer poll.Stop()
						for {
							affected := state(t, db, plan.ID)
							if (affected.State == "blocked" || affected.State == "cancelled") && state(t, db, healthy.ID).State == "accepted" {
								return
							}
							select {
							case <-exited:
								t.Fatalf("Run exited before healthy work progressed: %v", runErr)
							case <-deadline:
								t.Fatal("Run did not settle the affected plan and advance unrelated work")
							case <-poll.C:
							}
						}
					}()
				}
				wantState := "blocked"
				if failure == "cancellation-race" {
					wantState = "cancelled"
					if !raced || raceErr != nil {
						t.Fatalf("actual Store cancellation race was not injected: raced=%t err=%v", raced, raceErr)
					}
				}
				if affected := state(t, db, plan.ID); affected.State != wantState || affected.Attempts != 1 || affected.Receipt != "" {
					t.Fatalf("per-plan failure lost settlement or gained authority: %+v", affected)
				}
				if current := state(t, db, healthy.ID); current.State != "accepted" {
					t.Fatalf("unrelated healthy work did not progress: %+v", current)
				}
				questions, err := db.PlanningQuestions(ctx, plan.ID)
				if err != nil || len(questions) != 0 {
					t.Fatalf("invalid/stale planning output published a question: %+v %v", questions, err)
				}
				active, err = db.Active(ctx)
				if err != nil || len(active) != 0 || len(backend.requests) != 2 {
					t.Fatalf("failed question retained or replayed worker ownership: attempts=%+v calls=%d err=%v", active, len(backend.requests), err)
				}
				if proposal := state(t, db, "healthy-proposal"); proposal.State != "proposed" || proposal.PolicyHash != "" {
					t.Fatalf("healthy planning result inherited implementation approval: %+v", proposal)
				}
				if repositories.checks != 0 || repositories.commits != 0 || repositories.integrations != 0 || repositories.prs != 0 || repositories.releases != 0 {
					t.Fatalf("planning manufactured implementation effects: %+v", repositories)
				}
			})
		}
	}
}

func TestPlanningQuestionStorageFailureRemainsFatal(t *testing.T) {
	e, db, backend, repositories := fixture(t)
	ctx := context.Background()
	plan, err := intake.NewPlan("storage-failure-plan", "demo", "Clarify delivery", "Ask before selecting a durability guarantee.")
	if err != nil {
		t.Fatal(err)
	}
	backend.idle = true
	backend.plan = `{"question":"Must acknowledgement survive process loss?"}`
	r := &planningFailureRepositories{testRepos: repositories}
	e.repos = r
	enqueue(t, db, plan)
	ticks(t, e, 3)
	active, err := db.Active(ctx)
	if err != nil || len(active) != 1 || active[0].Stage != "plan_running" {
		t.Fatalf("plan did not reach question publication boundary: %+v %v", active, err)
	}
	ledger, err := sql.Open("sqlite", filepath.Join(e.cfg.StateDir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	injected := false
	r.fingerprintPath = active[0].Workspace
	r.beforeFingerprint = func() error {
		// Break the real publication table after Tick has loaded the attempt,
		// rather than returning a fabricated error from a mock Store.
		_, err := ledger.ExecContext(ctx, `DROP TABLE planning_questions`)
		injected = err == nil
		return err
	}
	backend.idle = false
	err = e.Tick(ctx)
	var domain *model.ProgramError
	if !injected || err == nil || errors.As(err, &domain) {
		t.Fatalf("raw question publication storage failure was swallowed as an ordinary model result: injected=%t err=%v", injected, err)
	}
	active, loadErr := db.Active(ctx)
	if loadErr != nil || len(active) != 1 || active[0].Stage != "plan_running" || state(t, db, plan.ID).State != "running" || len(backend.requests) != 1 {
		t.Fatalf("storage failure falsely settled or replayed the question producer: attempts=%+v err=%v", active, loadErr)
	}
}
