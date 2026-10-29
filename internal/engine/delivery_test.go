package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"openfactory/internal/model"
)

func ownIntegration(t *testing.T, e *Engine, a model.Attempt) model.Attempt {
	t.Helper()
	ctx := context.Background()
	if a.BaseSHA == "" {
		a.BaseSHA = "base"
	}
	if a.CandidateSHA == "" {
		a.CandidateSHA = "candidate"
	}
	a.SourceBaseSHA, a.SourceCandidateSHA = a.BaseSHA, a.CandidateSHA
	a.Stage = "integration_queued"
	if err := e.save(ctx, a); err != nil {
		t.Fatal(err)
	}
	f, err := e.db.Get(ctx, a.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.enqueueIntegration(ctx, a, f); err != nil {
		t.Fatal(err)
	}
	lease, err := e.db.AcquireIntegration(ctx, a.ID, e.cfg.PolicyHash)
	if err != nil {
		t.Fatal(err)
	}
	a.IntegrationToken = lease.Token
	return a
}

func changedCandidate(req model.ReconcileRequest) model.ReconciledCandidate {
	return model.ReconciledCandidate{Workspace: model.Workspace{Path: req.Attempt.Workspace + "-reconciled", BaseSHA: "new-base"}, CandidateSHA: "renewed-candidate", Fingerprint: "renewed-fingerprint", Changed: true}
}

func TestReconciledCandidateRequiresNewBoundedIndependentSession(t *testing.T) {
	for _, limit := range []int{2, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			e, db, b, r := fixture(t)
			b.spawnErr = errors.New("request accepted but response lost")
			e.cfg.MaxCallsPerDay = limit
			r.reconcileHook = func(req model.ReconcileRequest) (model.ReconciledCandidate, error) { return changedCandidate(req), nil }
			enqueue(t, db, contract("renewed"))
			ticks(t, e, 18)
			f := state(t, db, "renewed")
			if r.commits != 1 || r.snapshots != 1 || r.checks != 2 {
				t.Fatalf("reconciliation recaptured source or reused old checks: commits=%d snapshots=%d checks=%d", r.commits, r.snapshots, r.checks)
			}
			if len(b.requests) != limit {
				t.Fatalf("renewed proof escaped call limit: calls=%d limit=%d", len(b.requests), limit)
			}
			if limit == 2 {
				active, err := db.Active(context.Background())
				if err != nil || len(active) != 1 || active[0].IntegrationToken == "" || active[0].Wait == nil || active[0].Wait.Code != "model_call_limit" || f.State == "accepted" || r.integrations != 0 {
					t.Fatalf("call gate lost ownership or accepted stale proof: %+v %+v %v", f, active, err)
				}
				return
			}
			if f.State != "accepted" || r.integrations != 1 || r.expectedBase != "new-base" {
				t.Fatalf("renewed candidate did not deliver against its exact base: %+v base=%s", f, r.expectedBase)
			}
			if b.requests[1].AttemptID == b.requests[2].AttemptID || !strings.HasSuffix(b.requests[1].AttemptID, ":review:1") || !strings.HasSuffix(b.requests[2].AttemptID, ":review:2") || b.requests[1].Workspace == b.requests[2].Workspace {
				t.Fatal("renewed review reused a dispatch identity or candidate workspace")
			}
			status, err := db.FeatureStatus(context.Background(), f.ID, e.cfg)
			if err != nil || len(status.Sessions) != 3 || len(status.Checks) != 2 {
				t.Fatalf("dispatch/check reporting lost generation history: %+v %v", status, err)
			}
			for _, session := range status.Sessions {
				if session.DispatchKey == "" || session.ThreadID == "" || session.EnvironmentID == "" {
					t.Fatalf("session identity was not recorded: %+v", session)
				}
			}
		})
	}
}

func TestSameSHAPreviousGenerationEvidenceCannotAccept(t *testing.T) {
	e, db, _, r := fixture(t)
	f := contract("stale-generation")
	f.CompletionTarget = "reviewed"
	enqueue(t, db, f)
	a, err := db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.Workspace, a.BaseSHA, a.CandidateSHA, a.Fingerprint = "reviewed", "/workspaces/candidate", "base", "candidate", "candidate"
	a.CandidateGeneration = 1
	r.fp = "candidate"
	proof := []model.Evidence{{Kind: "check:unit", CandidateSHA: "candidate", CandidateGeneration: 1, Passed: true, Detail: "old passing check"}, {Kind: "review", CandidateSHA: "candidate", CandidateGeneration: 1, Passed: true, Detail: "old review"}}
	if err = db.Save(context.Background(), a, "running", "", "", true, proof...); err != nil {
		t.Fatal(err)
	}
	a.CandidateGeneration = 2
	if err = e.save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 1)
	if got := state(t, db, f.ID); got.State != "blocked" || !strings.Contains(got.Reason, "lacks current") || r.integrations != 0 {
		t.Fatalf("previous-generation evidence accepted: %+v", got)
	}
}

func TestKnownTargetMoveRenewsProofUsingImmutableOriginalDelta(t *testing.T) {
	e, db, b, r := fixture(t)
	r.integrationErr = &model.DeliveryError{Kind: "target_moved", Message: "moved before write"}
	r.reconcileHook = func(req model.ReconcileRequest) (model.ReconciledCandidate, error) {
		if len(r.reconcileRequests) == 1 {
			a := req.Attempt
			return model.ReconciledCandidate{Workspace: model.Workspace{Path: a.Workspace, BaseSHA: a.BaseSHA}, CandidateSHA: a.CandidateSHA, Fingerprint: a.Fingerprint}, nil
		}
		r.integrationErr = nil
		return changedCandidate(req), nil
	}
	enqueue(t, db, contract("moved"))
	ticks(t, e, 22)
	if got := state(t, db, "moved"); got.State != "accepted" || len(r.reconcileRequests) != 2 || r.integrations != 2 || len(b.requests) != 3 || r.checks != 2 || r.expectedBase != "new-base" {
		t.Fatalf("known target movement was not locally renewed: %+v reconciles=%d pushes=%d calls=%d", got, len(r.reconcileRequests), r.integrations, len(b.requests))
	}
	first, second := r.reconcileRequests[0], r.reconcileRequests[1]
	if first.OperationID == second.OperationID || first.Generation >= second.Generation {
		t.Fatal("successive reconciliation operations reused identity")
	}
	for _, req := range r.reconcileRequests {
		if req.Attempt.SourceBaseSHA != "base" || req.Attempt.SourceCandidateSHA != "candidate" || req.Attempt.IntegrationToken == "" {
			t.Fatalf("reconciliation changed original delta or lost ownership: %+v", req)
		}
	}
}

func TestConflictBlocksWithoutDeliveryAndReleasesKnownOwnership(t *testing.T) {
	e, db, _, r := fixture(t)
	r.reconcileHook = func(model.ReconcileRequest) (model.ReconciledCandidate, error) {
		return model.ReconciledCandidate{}, &model.DeliveryError{Kind: "conflict", Message: "overlapping changes", Paths: []string{"main.go"}}
	}
	enqueue(t, db, contract("conflict"))
	ticks(t, e, 16)
	active, err := db.Active(context.Background())
	if f := state(t, db, "conflict"); f.State != "blocked" || !strings.Contains(f.Reason, "main.go") || r.integrations != 0 || r.prs != 0 || err != nil || len(active) != 0 {
		t.Fatalf("conflict wrote or retained a known-safe owner: %+v active=%+v err=%v", f, active, err)
	}
}

func TestUncertainDeliveryRetainsResourceAcrossRestartAndCancellation(t *testing.T) {
	e, db, _, r := fixture(t)
	p := e.cfg.Projects["demo"]
	p.MaxConcurrent = 2
	e.cfg.Projects["demo"] = p
	r.integrationErr = &model.DeliveryError{Kind: "uncertain", Message: "connection lost after push"}
	enqueue(t, db, contract("first"))
	enqueue(t, db, contract("second"))
	ticks(t, e, 16)
	if r.integrations != 1 {
		t.Fatalf("another lane bypassed uncertain writer: %d", r.integrations)
	}
	active, err := db.Active(context.Background())
	if err != nil || len(active) != 2 {
		t.Fatalf("uncertain owner stopped counting toward admission: %+v %v", active, err)
	}
	var owner, waiter model.Attempt
	for _, a := range active {
		if a.Stage == "uncertain" {
			owner = a
		} else {
			waiter = a
		}
	}
	if owner.IntegrationToken == "" || waiter.Wait == nil || waiter.Wait.OwnerID != owner.ID {
		t.Fatalf("structured wait lost uncertain authority: owner=%+v waiter=%+v", owner, waiter)
	}
	if err = db.Retry(context.Background(), owner.FeatureID, model.TrustedLocalAudit()); err == nil {
		t.Fatal("retry abandoned uncertain delivery")
	}
	if err = db.Cancel(context.Background(), owner.FeatureID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if err = db.PrepareIntegrationRecovery(context.Background(), e.cfg); err != nil {
		t.Fatal(err)
	}
	e = New(e.cfg, db, e.backend, r)
	ticks(t, e, 4)
	status, err := db.IntegrationStatus(context.Background(), waiter.ID)
	if err != nil || status.OwnerAttemptID != owner.ID || r.integrations != 1 {
		t.Fatalf("restart/cancel stole uncertain lease: %+v %v", status, err)
	}
}

func TestRestartResumesPersistedLocalReconciliationIdentity(t *testing.T) {
	e, db, _, r := fixture(t)
	f := contract("local-restart")
	enqueue(t, db, f)
	a, err := db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Workspace, a.BaseSHA, a.CandidateSHA, a.Fingerprint, a.CandidateGeneration = "/workspaces/candidate", "base", "candidate", "candidate", 1
	r.fp = "candidate"
	a = ownIntegration(t, e, a)
	a.Stage, a.ReconcileOperationID, a.ReconcileGeneration = "reconciling", a.ID+":reconcile:2", 2
	proof := []model.Evidence{{Kind: "check:unit", CandidateSHA: "candidate", CandidateGeneration: 1, Passed: true, Detail: "passing check"}, {Kind: "review", CandidateSHA: "candidate", CandidateGeneration: 1, Passed: true, Detail: "independent review"}}
	if err = db.Save(context.Background(), a, "running", "", "", true, proof...); err != nil {
		t.Fatal(err)
	}
	e = New(e.cfg, db, e.backend, r)
	ticks(t, e, 2)
	if got := state(t, db, f.ID); got.State != "accepted" || len(r.reconcileRequests) != 1 || r.reconcileRequests[0].OperationID != a.ReconcileOperationID || r.integrations != 1 {
		t.Fatalf("restart did not replay the exact local receipt identity: %+v requests=%+v", got, r.reconcileRequests)
	}
}

// concurrentRepos models isolated immutable snapshots and a moving shared target;
// gates make real Run workers overlap and keep renewed proof observable.
type concurrentRepos struct {
	mu                                      sync.Mutex
	fingerprints                            map[string]string
	target                                  string
	integrated                              map[string]bool
	initialChecks                           chan string
	allowInitial                            chan struct{}
	refreshedChecks                         chan string
	allowRefreshed                          chan struct{}
	checks, integrations, inside, maxInside int
}

func (r *concurrentRepos) Prepare(_ context.Context, _ string, _ model.Project, id string) (model.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := "/workspaces/demo/" + id
	r.fingerprints[path] = "initial:" + id
	return model.Workspace{Path: path, BaseSHA: "base"}, nil
}
func (r *concurrentRepos) Fingerprint(_ context.Context, path string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fingerprints[path], nil
}
func (r *concurrentRepos) Commit(_ context.Context, path, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sha := "candidate:" + path[strings.LastIndex(path, "/")+1:]
	r.fingerprints[path] = sha
	return sha, nil
}
func (r *concurrentRepos) Snapshot(_ context.Context, path, sha string) (model.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fingerprints[path+"-snapshot"] = sha
	return model.Workspace{Path: path + "-snapshot", BaseSHA: "base"}, nil
}
func (r *concurrentRepos) ChangedPaths(context.Context, string, string, string) ([]string, error) {
	return []string{"main.go"}, nil
}
func (r *concurrentRepos) Check(ctx context.Context, path string, _ model.Command) (string, error) {
	r.mu.Lock()
	r.checks++
	r.mu.Unlock()
	if strings.Contains(path, "-reconciled-") {
		r.refreshedChecks <- path
		select {
		case <-r.allowRefreshed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else {
		r.initialChecks <- path
		select {
		case <-r.allowInitial:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "observed passing behavior assertion", nil
}
func (r *concurrentRepos) Reconcile(_ context.Context, _ string, _ model.Project, req model.ReconcileRequest) (model.ReconciledCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := req.Attempt
	if a.SourceBaseSHA != "base" || !strings.HasPrefix(a.SourceCandidateSHA, "candidate:") || a.IntegrationToken == "" {
		return model.ReconciledCandidate{}, errors.New("lost immutable delta or ownership")
	}
	if r.target == a.BaseSHA {
		return model.ReconciledCandidate{Workspace: model.Workspace{Path: a.Workspace, BaseSHA: a.BaseSHA}, CandidateSHA: a.CandidateSHA, Fingerprint: a.Fingerprint}, nil
	}
	path := fmt.Sprintf("%s-reconciled-%d", a.Workspace, req.Generation)
	sha := fmt.Sprintf("reconciled:%s:%d", a.ID, req.Generation)
	r.fingerprints[path] = sha
	return model.ReconciledCandidate{Workspace: model.Workspace{Path: path, BaseSHA: r.target}, CandidateSHA: sha, Fingerprint: sha, Changed: true}, nil
}
func (r *concurrentRepos) Integrate(_ context.Context, _ string, _ model.Project, a model.Attempt, expected string) (string, error) {
	r.mu.Lock()
	r.inside++
	if r.inside > r.maxInside {
		r.maxInside = r.inside
	}
	if a.IntegrationToken == "" || expected != a.BaseSHA || expected != r.target {
		r.inside--
		r.mu.Unlock()
		return "", errors.New("overlapping or stale integration")
	}
	r.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target = a.CandidateSHA
	r.integrated[a.FeatureID] = true
	r.integrations++
	r.inside--
	return a.CandidateSHA, nil
}
func (*concurrentRepos) PullRequest(context.Context, string, model.Project, model.Attempt, model.Feature) (string, error) {
	return "", errors.New("unexpected PR")
}
func (*concurrentRepos) PreflightRelease(context.Context, string, model.Project, model.Attempt, model.Feature) error {
	return errors.New("unexpected release")
}
func (*concurrentRepos) Release(context.Context, string, model.Project, model.Attempt, model.Feature) ([]model.Evidence, error) {
	return nil, errors.New("unexpected release")
}

func TestRunSameProjectChecksOverlapAndDeliveryProofOwnsFIFOLease(t *testing.T) {
	e, db, _, _ := fixture(t)
	e.cfg.MaxConcurrent, e.cfg.PollSeconds = 3, 1
	p := e.cfg.Projects["demo"]
	p.MaxConcurrent = 3
	e.cfg.Projects["demo"] = p
	r := &concurrentRepos{fingerprints: map[string]string{}, target: "base", integrated: map[string]bool{}, initialChecks: make(chan string, 3), allowInitial: make(chan struct{}), refreshedChecks: make(chan string, 3), allowRefreshed: make(chan struct{})}
	e.repos = r
	for _, id := range []string{"one", "two", "three"} {
		enqueue(t, db, contract(id))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not drain")
		}
	}()
	for range 3 {
		select {
		case <-r.initialChecks:
		case <-time.After(8 * time.Second):
			t.Fatal("same-project Run checks did not overlap under cap3")
		}
	}
	active, err := db.Active(context.Background())
	if err != nil || len(active) != 3 {
		t.Fatalf("same-project admission missing: %+v %v", active, err)
	}
	close(r.allowInitial)
	select {
	case <-r.refreshedChecks:
	case <-time.After(12 * time.Second):
		t.Fatal("moving target did not require fresh deterministic checks")
	}
	active, err = db.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owners, waiting := 0, 0
	for _, a := range active {
		status, err := db.IntegrationStatus(context.Background(), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "owned" {
			owners++
		}
		if status.State == "queued" || status.State == "waiting" {
			waiting++
		}
	}
	if owners != 1 || waiting != 1 {
		t.Fatalf("renewed proof did not retain exclusive delivery ownership: owners=%d waiting=%d active=%+v", owners, waiting, active)
	}
	close(r.allowRefreshed)
	deadline := time.After(18 * time.Second)
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		all := true
		for _, id := range []string{"one", "two", "three"} {
			if state(t, db, id).State != "accepted" {
				all = false
			}
		}
		if all {
			break
		}
		select {
		case <-timer.C:
		case <-deadline:
			t.Fatal("concurrent lanes did not all deliver")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.integrated) != 3 || r.integrations != 3 || r.maxInside != 1 || r.checks != 5 {
		t.Fatalf("concurrent candidate delivery lost updates/proof: integrated=%v merges=%d max=%d checks=%d", r.integrated, r.integrations, r.maxInside, r.checks)
	}
}

var _ model.Repositories = (*concurrentRepos)(nil)

func TestReconciledProofFailureNeverRepairsImmutableSource(t *testing.T) {
	for _, failure := range []string{"check", "review"} {
		t.Run(failure, func(t *testing.T) {
			e, db, b, r := fixture(t)
			r.reconcileHook = func(req model.ReconcileRequest) (model.ReconciledCandidate, error) {
				if failure == "check" {
					r.checkErr = errors.New("renewed candidate assertion failed")
				} else {
					b.review = `{"approved":false,"summary":"renewed candidate breaks behavior","findings":["regression"],"acceptance":[]}`
				}
				return changedCandidate(req), nil
			}
			enqueue(t, db, contract("renewed-failure"))
			ticks(t, e, 24)
			f := state(t, db, "renewed-failure")
			if f.State != "blocked" || f.Attempts != 1 || r.commits != 1 || r.integrations != 0 || !strings.Contains(f.Reason, "reconciled candidate failed") {
				t.Fatalf("renewed failure repaired or accepted immutable source: %+v commits=%d writes=%d", f, r.commits, r.integrations)
			}
		})
	}
}

func TestCompletionTargetsPreserveReceiptsAndReleaseOwnership(t *testing.T) {
	for _, target := range []string{"reviewed", "pull_request", "integrated", "staging", "production"} {
		t.Run(target, func(t *testing.T) {
			e, db, _, r := fixture(t)
			stagingPolicy(e)
			p := e.cfg.Projects["demo"]
			p.Releases["production"] = p.Releases["staging"]
			e.cfg.Projects["demo"] = p
			r.releaseEvidence = []model.Evidence{{Kind: "release.observe", CandidateSHA: "candidate", Passed: true, Detail: "observed healthy target"}, {Kind: "release.rollback_qualification", CandidateSHA: "candidate", Passed: true, Detail: "qualified operator rollback hook"}}
			f := contract("target")
			f.CompletionTarget = target
			enqueue(t, db, f)
			ticks(t, e, 18)
			got := state(t, db, f.ID)
			if got.State != "accepted" || got.Receipt == "" {
				t.Fatalf("completion target lost its receipt: %+v", got)
			}
			if target == "reviewed" {
				if r.integrations != 0 || r.prs != 0 || r.releases != 0 || len(r.reconcileRequests) != 0 || got.Receipt != "reviewed:candidate" {
					t.Fatalf("reviewed-only target took delivery authority: %+v", got)
				}
			} else {
				if len(r.reconcileRequests) != 1 {
					t.Fatalf("delivery bypassed reconciliation: %d", len(r.reconcileRequests))
				}
				req := r.reconcileRequests[0]
				status, err := db.IntegrationStatus(context.Background(), req.Attempt.ID)
				if err != nil || status.State != "settled" {
					t.Fatalf("successful target retained ownership: %+v %v", status, err)
				}
				if target == "pull_request" {
					if r.prs != 1 || r.integrations != 0 || r.releases != 0 {
						t.Fatal("PR target integrated or released")
					}
				} else if r.integrations != 1 {
					t.Fatal("integrated target skipped its exact-base push")
				}
				if target == "staging" || target == "production" {
					if r.releases != 1 || len(status.Resources) != 2 {
						t.Fatalf("release target lost environment ownership: %+v releases=%d", status, r.releases)
					}
				}
			}
		})
	}
}

func TestDependencyCandidatesAreRequestedOnlyForSameRepositoryAndBranch(t *testing.T) {
	e, db, _, _ := fixture(t)
	for _, name := range []string{"alias", "other-repository", "other-branch"} {
		p := e.cfg.Projects["demo"]
		switch name {
		case "other-repository":
			p.Repository = "/trusted/different"
		case "other-branch":
			p.Branch = "release"
		}
		e.cfg.Projects[name] = p
		f := contract(name)
		f.Project = name
		if name == "alias" {
			f.CompletionTarget = "staging"
		}
		enqueue(t, db, f)
		a, err := db.Claim(context.Background(), f.ID, "policy", 2, 2, 1, 3600)
		if err != nil {
			t.Fatal(err)
		}
		a.Stage, a.State = "accepted", "accepted"
		a.CandidateSHA = "accepted-candidate-" + name
		if err = db.Save(context.Background(), a, "accepted", "", "staging-observation-receipt-"+name, false); err != nil {
			t.Fatal(err)
		}
	}
	child := contract("dependent")
	child.DependsOn = []string{"alias", "other-repository", "other-branch"}
	ancestors, err := e.requiredAncestors(context.Background(), child)
	if err != nil || len(ancestors) != 1 || ancestors[0] != "accepted-candidate-alias" {
		t.Fatalf("incorrect dependency ancestry requirement: %v %v", ancestors, err)
	}
}

func TestCancellationOfKnownLocalOwnerReleasesOnlyItsLane(t *testing.T) {
	e, db, _, r := fixture(t)
	p := e.cfg.Projects["demo"]
	p.MaxConcurrent = 2
	e.cfg.Projects["demo"] = p
	enqueue(t, db, contract("cancel-owner"))
	enqueue(t, db, contract("continue-lane"))
	ticks(t, e, 9)
	active, err := db.Active(context.Background())
	if err != nil || len(active) != 2 {
		t.Fatalf("missing concurrent lanes: %+v %v", active, err)
	}
	var owner, waiter model.Attempt
	for _, a := range active {
		if a.IntegrationToken != "" {
			owner = a
		} else {
			waiter = a
		}
	}
	if owner.ID == "" || waiter.ID == "" || r.integrations != 0 {
		t.Fatalf("expected one pre-write owner and one waiter: %+v", active)
	}
	if err = db.Cancel(context.Background(), owner.FeatureID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 5)
	if got := state(t, db, owner.FeatureID); got.State != "cancelled" {
		t.Fatalf("known local owner did not settle cancellation: %+v", got)
	}
	if got := state(t, db, waiter.FeatureID); got.State != "accepted" || r.integrations != 1 {
		t.Fatalf("cancelled lane prevented peer delivery: %+v writes=%d", got, r.integrations)
	}
	status, err := db.IntegrationStatus(context.Background(), owner.ID)
	if err != nil || status.State != "settled" {
		t.Fatalf("cancelled local owner retained resources: %+v %v", status, err)
	}
}

func TestConcurrentAdmissionDoesNotTreatReviewOrPRAsIntegratedDependency(t *testing.T) {
	for _, target := range []string{"reviewed", "pull_request"} {
		t.Run(target, func(t *testing.T) {
			e, db, b, r := fixture(t)
			p := e.cfg.Projects["demo"]
			p.MaxConcurrent = 2
			e.cfg.Projects["demo"] = p
			parent := contract("parent")
			parent.CompletionTarget = target
			child := contract("child")
			child.DependsOn = []string{parent.ID}
			enqueue(t, db, parent)
			enqueue(t, db, child)
			ticks(t, e, 20)
			if got := state(t, db, parent.ID); got.State != "accepted" {
				t.Fatalf("parent target did not finish: %+v", got)
			}
			if got := state(t, db, child.ID); got.State != "ready" || got.Attempts != 0 || len(b.requests) != 2 || r.integrations != 0 {
				t.Fatalf("review/PR dependency incorrectly admitted under concurrency: %+v calls=%d", got, len(b.requests))
			}
		})
	}
}
