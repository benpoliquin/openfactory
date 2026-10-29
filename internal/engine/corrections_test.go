package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

type infrastructureFailure struct{}

func (infrastructureFailure) Error() string        { return "executor unavailable" }
func (infrastructureFailure) Infrastructure() bool { return true }

func TestExecutorInfrastructureFailureBlocksWithoutRepair(t *testing.T) {
	e, db, b, r := fixture(t)
	r.checkErr = fmt.Errorf("check service: %w", infrastructureFailure{})
	enqueue(t, db, contract("outage"))
	ticks(t, e, 20)
	f := state(t, db, "outage")
	if f.State != "blocked" || f.Attempts != 1 || len(b.requests) != 1 || r.integrations != 0 {
		t.Fatalf("infrastructure failure consumed repair work: %+v calls=%d", f, len(b.requests))
	}
	proof, err := db.Evidence(context.Background(), f.ID)
	if err != nil || len(proof) != 1 || proof[0].Passed || proof[0].CandidateSHA != "candidate" || !strings.Contains(proof[0].Detail, "executor unavailable") {
		t.Fatalf("missing candidate-bound infrastructure evidence: %+v %v", proof, err)
	}
}

func TestSnapshotFailureCannotProduceCheckOrReviewEvidence(t *testing.T) {
	e, db, b, r := fixture(t)
	r.snapshotErr = errors.New("cannot export clean candidate")
	enqueue(t, db, contract("snapshot"))
	ticks(t, e, 12)
	if f := state(t, db, "snapshot"); f.State != "blocked" || r.checks != 0 || len(b.requests) != 1 || r.integrations != 0 {
		t.Fatalf("snapshot failure reached unchecked candidate: %+v", f)
	}
	proof, err := db.Evidence(context.Background(), "snapshot")
	if err != nil || len(proof) != 0 {
		t.Fatalf("snapshot failure fabricated check evidence: %+v %v", proof, err)
	}
}

func stagingPolicy(e *Engine) {
	p := e.cfg.Projects["demo"]
	p.Releases = map[string]model.Release{"staging": {
		Allowed: true, Qualified: true, AllowedRisks: []string{"normal"}, AllowedPaths: []string{"**"}, ExpiresAt: time.Now().Add(time.Hour),
		Deploy: model.Command{Argv: []string{"/operator/deploy"}}, Observe: model.Command{Argv: []string{"/operator/observe"}}, Rollback: model.Command{Argv: []string{"/operator/rollback"}}, MinimumObservations: 1, ObservationSeconds: 1,
	}}
	e.cfg.Projects["demo"] = p
}

func TestReleasePreflightBlocksBeforeIntegration(t *testing.T) {
	e, db, b, r := fixture(t)
	stagingPolicy(e)
	f := contract("preflight")
	f.CompletionTarget = "staging"
	r.preflightErr = errors.New("insufficient remaining deployment and rollback budget")
	enqueue(t, db, f)
	ticks(t, e, 14)
	if got := state(t, db, f.ID); got.State != "blocked" || !strings.Contains(got.Reason, "release preflight") || got.Attempts != 1 || r.preflights != 1 || r.integrations != 0 || r.releases != 0 || len(b.requests) != 2 {
		t.Fatalf("known infeasible release pushed candidate: %+v preflights=%d integrations=%d", got, r.preflights, r.integrations)
	}
}

func TestFreshBBEnvironmentForEveryStageIncludingRecoveredDispatch(t *testing.T) {
	e, db, b, _ := fixture(t)
	e.cfg.FastModel = "fast"
	b.spawnErr = errors.New("accepted but response lost")
	enqueue(t, db, contract("environments"))
	ticks(t, e, 20)
	if got := state(t, db, "environments"); got.State != "accepted" || len(b.requests) != 3 {
		t.Fatalf("real BB environment identities prevented fresh stage: %+v calls=%d", got, len(b.requests))
	}
	for _, request := range b.requests {
		if request.EnvironmentID != "" {
			t.Fatalf("fresh stage inherited another BB environment: %+v", request)
		}
	}
}

func TestRestartedChecksKeepCapturedCandidate(t *testing.T) {
	e, db, _, r := fixture(t)
	f := contract("resume-checks")
	enqueue(t, db, f)
	a, err := db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.Workspace, a.BaseSHA, a.CandidateSHA, a.Fingerprint = "checks", "/workspaces/existing-snapshot", "base", "candidate", "candidate"
	r.fp = "candidate"
	if err = db.Save(context.Background(), a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 1)
	if r.commits != 0 || r.snapshots != 0 || r.checks != 1 || r.checkWorkspace != a.Workspace {
		t.Fatalf("restart recaptured the candidate: commits=%d snapshots=%d checks=%d workspace=%s", r.commits, r.snapshots, r.checks, r.checkWorkspace)
	}
	proof, err := db.Evidence(context.Background(), f.ID)
	if err != nil || len(proof) != 1 || proof[0].CandidateSHA != a.CandidateSHA {
		t.Fatalf("restart lost checked identity: %+v %v", proof, err)
	}
}
func TestMutationBetweenChecksAndReviewCannotResetFingerprint(t *testing.T) {
	e, db, b, r := fixture(t)
	enqueue(t, db, contract("between-stages"))
	// claim -> prepare -> build dispatch -> build completion -> checks
	ticks(t, e, 5)
	r.fp = "unapproved-source-write"
	ticks(t, e, 4)
	if got := state(t, db, "between-stages"); got.State != "blocked" || len(b.requests) != 1 || r.integrations != 0 || !strings.Contains(got.Reason, "before independent review") {
		t.Fatalf("review dispatch replaced checked fingerprint: %+v calls=%d", got, len(b.requests))
	}
}

func TestRunSupervisesOtherAttemptsDuringLongOperation(t *testing.T) {
	for _, operation := range []string{"check", "release"} {
		for _, reason := range []string{"cancel", "deadline"} {
			t.Run(operation+"/"+reason, func(t *testing.T) {
				e, db, b, r := fixture(t)
				e.cfg.PollSeconds = 1
				stagingPolicy(e)
				p := e.cfg.Projects["demo"]
				p.MaxConcurrent = 2
				e.cfg.Projects["demo"] = p
				ctx := context.Background()
				long := contract("long")
				if operation == "release" {
					long.CompletionTarget = "staging"
				}
				enqueue(t, db, long)
				a, err := db.Claim(ctx, long.ID, "policy", 2, 2, 2, 3600)
				if err != nil {
					t.Fatal(err)
				}
				a.Stage, a.Workspace, a.BaseSHA = "checks", "/workspaces/long", "base"
				var evidence []model.Evidence
				if operation == "release" {
					a.Stage, a.CandidateSHA, a.Fingerprint, a.Summary = "integrated", "candidate", "candidate", "integrated:candidate"
					r.fp = "candidate"
					evidence = []model.Evidence{{Kind: "check:unit", CandidateSHA: "candidate", Passed: true, Detail: "observed passing unit assertion"}, {Kind: "review", CandidateSHA: "candidate", Passed: true, Detail: "independent source review"}}
				}
				if operation == "release" {
					a = ownIntegration(t, e, a)
					a.Stage = "integrated"
				}
				if err = db.Save(ctx, a, "running", "", "", true, evidence...); err != nil {
					t.Fatal(err)
				}
				other := contract("other")
				enqueue(t, db, other)
				a, err = db.Claim(ctx, other.ID, "policy", 2, 2, 2, 3600)
				if err != nil {
					t.Fatal(err)
				}
				session, err := b.Spawn(ctx, model.SpawnRequest{AttemptID: a.ID + ":build"})
				if err != nil {
					t.Fatal(err)
				}
				a.Stage, a.ThreadID, a.EnvironmentID = "build_running", session.ThreadID, session.EnvironmentID
				if reason == "deadline" {
					a.Deadline = time.Now().Add(500 * time.Millisecond)
				}
				if err = db.Save(ctx, a, "running", "", "", true); err != nil {
					t.Fatal(err)
				}
				b.idle = true
				entered, stopped := make(chan struct{}), make(chan struct{})
				block := func(ctx context.Context) error {
					close(entered)
					defer close(stopped)
					<-ctx.Done()
					return ctx.Err()
				}
				r.checkHook = func(ctx context.Context, _ string) (string, error) { return "", block(ctx) }
				r.releaseHook = func(ctx context.Context) ([]model.Evidence, error) { return nil, block(ctx) }
				runctx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- e.Run(runctx) }()
				t.Cleanup(func() {
					cancel()
					select {
					case err := <-done:
						if !errors.Is(err, context.Canceled) {
							t.Errorf("Run failed: %v", err)
						}
					case <-time.After(3 * time.Second):
						t.Error("Run did not drain canceled stage worker")
					}
				})
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("long operation never started")
				}
				// A concurrent deterministic tick must not start a second worker for
				// the long attempt. Its native BB peer can still make progress.
				if err = e.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if reason == "cancel" {
					if err = db.Cancel(ctx, other.ID, model.TrustedLocalAudit()); err != nil {
						t.Fatal(err)
					}
				}
				want := "cancelled"
				if reason == "deadline" {
					want = "blocked"
				}
				limit := time.After(3 * time.Second)
				poll := time.NewTicker(10 * time.Millisecond)
				defer poll.Stop()
				for state(t, db, other.ID).State != want {
					select {
					case <-poll.C:
					case <-limit:
						t.Fatalf("long %s delayed another attempt's %s: %+v", operation, reason, state(t, db, other.ID))
					}
				}
				select {
				case <-stopped:
					t.Fatal("other attempt's supervision canceled unrelated long operation")
				default:
				}
				b.mu.Lock()
				wasStopped := b.stopped[session.ThreadID]
				b.mu.Unlock()
				if !wasStopped {
					t.Fatal("native BB session was not stopped")
				}
			})
		}
	}
}
