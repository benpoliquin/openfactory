package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"openfactory/internal/config"
	"openfactory/internal/model"
)

type prewriteOutcomeRepos struct {
	*testRepos
	beforeWrite func(context.Context, model.Attempt) error
}

func (r *prewriteOutcomeRepos) Integrate(ctx context.Context, name string, p model.Project, a model.Attempt, expected string) (string, error) {
	if err := r.beforeWrite(ctx, a); err != nil {
		return "", err
	}
	return r.testRepos.Integrate(ctx, name, p, a, expected)
}

func (r *prewriteOutcomeRepos) PullRequest(ctx context.Context, name string, p model.Project, a model.Attempt, f model.Feature) (string, error) {
	if err := r.beforeWrite(ctx, a); err != nil {
		return "", err
	}
	return r.testRepos.PullRequest(ctx, name, p, a, f)
}

func TestCancellationDuringKnownPrewriteOutcomeDoesNotFenceOtherLanes(t *testing.T) {
	for _, target := range []string{"integrated", "pull_request"} {
		for _, kind := range []string{"rejected", "conflict", "target_moved"} {
			t.Run(target+"/"+kind, func(t *testing.T) {
				e, db, _, r := fixture(t)
				p := e.cfg.Projects["demo"]
				p.MaxConcurrent = 2
				e.cfg.Projects["demo"] = p
				for _, id := range []string{"cancel-before-write", "continue-after-cancel"} {
					f := contract(id)
					f.CompletionTarget = target
					enqueue(t, db, f)
				}
				ticks(t, e, 10)
				active, err := db.Active(context.Background())
				if err != nil || len(active) != 2 {
					t.Fatalf("missing concurrent delivery lanes: %+v %v", active, err)
				}
				var owner, waiter model.Attempt
				for _, a := range active {
					if a.IntegrationToken != "" {
						owner = a
					} else {
						waiter = a
					}
				}
				if owner.Stage != "reviewed" || waiter.ID == "" {
					t.Fatalf("expected reviewed owner and queued peer: %+v", active)
				}
				workerctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				e.repos = &prewriteOutcomeRepos{testRepos: r, beforeWrite: func(ctx context.Context, a model.Attempt) error {
					if a.ID != owner.ID {
						return nil
					}
					if err := db.Cancel(context.Background(), a.FeatureID, model.TrustedLocalAudit()); err != nil {
						t.Fatal(err)
					}
					cancel()
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatal("operation did not observe worker cancellation")
					}
					return &model.DeliveryError{Kind: kind, Message: "positively established no remote write"}
				}}
				if err = e.advance(workerctx, owner, false); err != nil {
					t.Fatalf("classified outcome was lost to cancelled worker context: %v", err)
				}
				ticks(t, e, 6)
				if got := state(t, db, owner.FeatureID); got.State != "cancelled" {
					t.Fatalf("known no-write owner did not settle cancellation: %+v", got)
				}
				if got := state(t, db, waiter.FeatureID); got.State != "accepted" {
					t.Fatalf("known no-write cancellation falsely fenced peer: %+v", got)
				}
				status, err := db.IntegrationStatus(context.Background(), owner.ID)
				if err != nil || status.State != "settled" || r.integrations+r.prs != 1 {
					t.Fatalf("cancelled no-write owner retained or wrote resources: %+v writes=%d %v", status, r.integrations+r.prs, err)
				}
			})
		}
	}
}

func TestClassifiedDeliverySettlementSurvivesExpiredWorkerDeadline(t *testing.T) {
	for _, kind := range []string{"target_moved", "rejected", "conflict", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			e, db, _, _ := fixture(t)
			f := contract("expired-delivery")
			enqueue(t, db, f)
			a, err := db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
			if err != nil {
				t.Fatal(err)
			}
			a = ownIntegration(t, e, a)
			a.Stage = "integrating"
			if err = e.save(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			if err = e.deliveryFailure(ctx, a, "integration", &model.DeliveryError{Kind: kind, Message: "classified after deadline"}); err != nil {
				t.Fatalf("classified result was not durably settled: %v", err)
			}
			active, err := db.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "target_moved":
				if len(active) != 1 || active[0].Stage != "reconciliation_pending" || active[0].IntegrationToken == "" {
					t.Fatalf("known movement became uncertainty: %+v", active)
				}
			case "uncertain":
				if len(active) != 1 || active[0].Stage != "uncertain" || active[0].IntegrationToken == "" {
					t.Fatalf("ambiguous write lost ownership: %+v", active)
				}
			default:
				if len(active) != 0 || state(t, db, f.ID).State != "blocked" {
					t.Fatalf("known no-write rejection retained an uncertain owner: %+v", active)
				}
			}
		})
	}
}

func acceptDependency(t *testing.T, e *Engine, f model.Feature, sha string) model.Feature {
	t.Helper()
	enqueue(t, e.db, f)
	a, err := e.db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.State, a.CandidateSHA = "accepted", "accepted", sha
	if err = e.db.Save(context.Background(), a, "accepted", "", "observation receipt for "+sha, false); err != nil {
		t.Fatal(err)
	}
	return state(t, e.db, f.ID)
}

func TestTransitiveCrossRepositoryDependencyChangesDoNotInvalidateDirectAcceptedParent(t *testing.T) {
	for _, change := range []string{"revision", "unenrolled"} {
		t.Run(change, func(t *testing.T) {
			e, db, _, r := fixture(t)
			p := e.cfg.Projects["demo"]
			p.Repository = "/trusted/another-repository"
			e.cfg.Projects["external"] = p
			root := contract("external-root")
			root.Project = "external"
			root = acceptDependency(t, e, root, "external-root-candidate")
			parent := contract("accepted-parent")
			parent.DependsOn = []string{root.ID}
			acceptDependency(t, e, parent, "accepted-parent-candidate")
			if change == "revision" {
				root.Body += "\nA newly proposed revision, not a revocation of its accepted dependent."
				if _, err := db.Import(context.Background(), root); err != nil {
					t.Fatal(err)
				}
			} else {
				delete(e.cfg.Projects, "external")
			}
			child := contract("child")
			child.DependsOn = []string{parent.ID}
			enqueue(t, db, child)
			ticks(t, e, 16)
			if got := state(t, db, child.ID); got.State != "accepted" || r.integrations != 1 {
				t.Fatalf("irrelevant transitive dependency invalidated accepted direct parent: %+v", got)
			}
			if len(r.reconcileRequests) != 1 || len(r.reconcileRequests[0].RequiredAncestors) != 1 || r.reconcileRequests[0].RequiredAncestors[0] != "accepted-parent-candidate" {
				t.Fatalf("wrong direct applicable ancestry request: %+v", r.reconcileRequests)
			}
		})
	}
}

func queuedAttempt(t *testing.T, e *Engine, f model.Feature) model.Attempt {
	t.Helper()
	enqueue(t, e.db, f)
	a, err := e.db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.BaseSHA, a.CandidateSHA = "integration_queued", "base", "candidate"
	a.SourceBaseSHA, a.SourceCandidateSHA = a.BaseSHA, a.CandidateSHA
	if err = e.save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestIntegrationWaitNamesReleaseOnlyContendedResource(t *testing.T) {
	e, db, _, _ := fixture(t)
	stagingPolicy(e)
	p := e.cfg.Projects["demo"]
	p.Branch = "other-branch"
	e.cfg.Projects["alias"] = p
	ownerFeature := contract("release-owner")
	ownerFeature.CompletionTarget = "staging"
	owner := queuedAttempt(t, e, ownerFeature)
	owner = ownIntegration(t, e, owner)
	waiterFeature := contract("release-waiter")
	waiterFeature.Project, waiterFeature.CompletionTarget = "alias", "staging"
	waiter := queuedAttempt(t, e, waiterFeature)
	if err := e.acquireIntegration(context.Background(), waiter, waiterFeature); err != nil {
		t.Fatal(err)
	}
	resources, err := config.IntegrationResources(p, "staging")
	if err != nil {
		t.Fatal(err)
	}
	var release string
	for _, resource := range resources {
		if strings.HasPrefix(resource, "release:") {
			release = resource
		}
	}
	active, err := db.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == waiter.ID {
			if a.Wait == nil || a.Wait.Code != "integration_owner" || a.Wait.Resource != release || a.Wait.OwnerID != owner.ID {
				t.Fatalf("release contention mislabeled as independent git branch: %+v", a.Wait)
			}
			return
		}
	}
	t.Fatal("missing queued release waiter")
}

func TestIntegrationAcquireRaceReportsPauseAndCancellationRatherThanOwner(t *testing.T) {
	for _, gate := range []string{"paused", "cancellation"} {
		t.Run(gate, func(t *testing.T) {
			e, db, _, _ := fixture(t)
			f := contract("gate-race")
			a := queuedAttempt(t, e, f)
			if gate == "paused" {
				if err := db.SetPaused(context.Background(), true); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Cancel(context.Background(), f.ID, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
			if err := e.acquireIntegration(context.Background(), a, f); err != nil {
				t.Fatal(err)
			}
			active, err := db.Active(context.Background())
			if err != nil || len(active) != 1 || active[0].Wait == nil || active[0].Wait.Code != gate || active[0].Wait.Resource != "" || active[0].Wait.OwnerID != "" {
				t.Fatalf("acquisition race invented integration ownership contention: %+v %v", active, err)
			}
		})
	}
}
