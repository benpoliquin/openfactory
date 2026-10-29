package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/policy"
	"openfactory/internal/store"
)

type testBackend struct {
	mu        sync.Mutex
	requests  []model.SpawnRequest
	sessions  map[string]string
	stages    map[string]string
	stopped   map[string]bool
	spawnErr  error
	findEmpty bool
	idle      bool
	plan      string
	review    string
	pollHook  func(string)
	cursors   []string
}

func (b *testBackend) Doctor(context.Context) error { return nil }
func (b *testBackend) EnsureProject(context.Context, string, string) (string, error) {
	return "project", nil
}
func (b *testBackend) Spawn(_ context.Context, r model.SpawnRequest) (model.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.EnvironmentID != "" {
		return model.Session{}, errors.New("fresh BB stage cannot reuse another thread's environment")
	}
	if b.sessions == nil {
		b.sessions = map[string]string{}
		b.stages = map[string]string{}
		b.stopped = map[string]bool{}
	}
	b.requests = append(b.requests, r)
	id := fmt.Sprintf("thread-%d", len(b.requests))
	b.sessions[r.AttemptID] = id
	parts := strings.Split(r.AttemptID, ":")
	stage := parts[len(parts)-1]
	if stage != "build" && stage != "review" && stage != "scout" && stage != "plan" && len(parts) > 1 {
		stage = parts[len(parts)-2]
	}
	b.stages[id] = stage
	return model.Session{ThreadID: id, EnvironmentID: "environment-" + id}, b.spawnErr
}
func (b *testBackend) Find(_ context.Context, _ string, marker string) ([]model.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.findEmpty {
		return nil, nil
	}
	if id := b.sessions[marker]; id != "" {
		return []model.Session{{ThreadID: id, EnvironmentID: "environment-" + id}}, nil
	}
	return nil, nil
}
func (b *testBackend) Stop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped[id] = true
	return nil
}
func (b *testBackend) Poll(_ context.Context, id, cursor string) (model.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cursors = append(b.cursors, cursor)
	if b.stopped[id] {
		return model.Observation{State: "interrupted", Cursor: "stopped"}, nil
	}
	if b.pollHook != nil {
		b.pollHook(b.stages[id])
	}
	if b.idle {
		return model.Observation{State: "running", Cursor: "opaque-cursor"}, nil
	}
	out := `{"summary":"implemented","acceptance":[{"id":"behavior","passed":true,"evidence":"unit behavior assertion passes"}]}`
	switch b.stages[id] {
	case "review":
		out = `{"approved":true,"summary":"independently inspected assertions","findings":[],"acceptance":[{"id":"behavior","passed":true,"evidence":"observed unit behavior assertion and diff"}]}`
		if b.review != "" {
			out = b.review
		}
	case "scout":
		out = "Relevant source is main.go; preserve existing command contract."
	case "plan":
		out = b.plan
	}
	return model.Observation{State: "completed", Cursor: "terminal-cursor", Output: out}, nil
}

type testRepos struct {
	fp                                           string
	paths                                        []string
	checkErr                                     error
	mutateCheck                                  bool
	checks, commits, integrations, prs, releases int
	integrationHook                              func()
	releaseEvidence                              []model.Evidence
	releaseErr                                   error
	snapshots, preflights                        int
	operations                                   []string
	checkWorkspace                               string
	snapshotErr, preflightErr                    error
	checkHook                                    func(context.Context, string) (string, error)
	releaseHook                                  func(context.Context) ([]model.Evidence, error)
	reconcileHook                                func(model.ReconcileRequest) (model.ReconciledCandidate, error)
	reconcileRequests                            []model.ReconcileRequest
	integrationErr                               error
	expectedBase                                 string
}

func (r *testRepos) Prepare(context.Context, string, model.Project, string) (model.Workspace, error) {
	r.fp = "initial"
	return model.Workspace{Path: "/workspaces/attempt", BaseSHA: "base"}, nil
}
func (r *testRepos) Reconcile(_ context.Context, _ string, _ model.Project, req model.ReconcileRequest) (model.ReconciledCandidate, error) {
	r.reconcileRequests = append(r.reconcileRequests, req)
	if r.reconcileHook != nil {
		candidate, err := r.reconcileHook(req)
		if err == nil {
			r.fp = candidate.Fingerprint
		}
		return candidate, err
	}
	a := req.Attempt
	return model.ReconciledCandidate{Workspace: model.Workspace{Path: a.Workspace, BaseSHA: a.BaseSHA}, CandidateSHA: a.CandidateSHA, Fingerprint: a.Fingerprint}, nil
}
func (r *testRepos) Fingerprint(context.Context, string) (string, error) { return r.fp, nil }
func (r *testRepos) Commit(context.Context, string, string) (string, error) {
	r.commits++
	r.operations = append(r.operations, "commit")
	r.fp = "candidate"
	return "candidate", nil
}
func (r *testRepos) Snapshot(_ context.Context, workspace, sha string) (model.Workspace, error) {
	r.snapshots++
	r.operations = append(r.operations, "snapshot:"+sha)
	return model.Workspace{Path: workspace + "-snapshot", BaseSHA: "base"}, r.snapshotErr
}
func (r *testRepos) Check(ctx context.Context, workspace string, _ model.Command) (string, error) {
	r.checks++
	r.operations = append(r.operations, "check")
	r.checkWorkspace = workspace
	if r.checkHook != nil {
		return r.checkHook(ctx, workspace)
	}
	if r.mutateCheck {
		r.fp += "changed"
	}
	return "one behavior assertion passed", r.checkErr
}
func (r *testRepos) ChangedPaths(context.Context, string, string, string) ([]string, error) {
	return r.paths, nil
}
func (r *testRepos) Integrate(_ context.Context, _ string, _ model.Project, _ model.Attempt, expected string) (string, error) {
	r.expectedBase = expected
	r.integrations++
	if r.integrationHook != nil {
		r.integrationHook()
	}
	return "integrated:candidate", r.integrationErr
}
func (r *testRepos) PullRequest(context.Context, string, model.Project, model.Attempt, model.Feature) (string, error) {
	r.prs++
	return "https://github.com/example/demo/pull/1", nil
}
func (r *testRepos) PreflightRelease(context.Context, string, model.Project, model.Attempt, model.Feature) error {
	r.preflights++
	return r.preflightErr
}
func (r *testRepos) Release(ctx context.Context, _ string, _ model.Project, _ model.Attempt, _ model.Feature) ([]model.Evidence, error) {
	r.releases++
	if r.releaseHook != nil {
		return r.releaseHook(ctx)
	}
	return r.releaseEvidence, r.releaseErr
}
func contract(id string) model.Feature {
	return model.Feature{ID: id, Project: "demo", Title: "Implement behavior", Kind: "feature", Risk: "normal", CompletionTarget: "integrated", Checks: []string{"unit"}, Acceptance: []model.Acceptance{{ID: "behavior", Scenario: "Exercise a real behavior", Expected: "The expected result"}}, Body: "Implement the approved behavior without changing scope."}
}
func fixture(t *testing.T) (*Engine, *store.Store, *testBackend, *testRepos) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := model.Config{StateDir: dir, PolicyHash: "policy", AstraModel: "astra", MaxConcurrent: 2, MaxAttempts: 2, MaxCallsPerDay: 20, AttemptTimeoutSeconds: 3600, MaxPromptBytes: 32768, Projects: map[string]model.Project{"demo": {Trusted: true, Repository: "/trusted/demo", Branch: "main", GitHubRepository: "example/demo", Checks: map[string]model.Command{"unit": {Argv: []string{"go", "test", "./..."}}}, ProtectedPaths: []string{"secrets/**"}}}}
	b := &testBackend{}
	r := &testRepos{paths: []string{"main.go"}}
	return New(cfg, db, b, r), db, b, r
}
func enqueue(t *testing.T, db *store.Store, f model.Feature) {
	t.Helper()
	ctx := context.Background()
	saved, err := db.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Approve(ctx, f.ID, saved.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
}
func ticks(t *testing.T, e *Engine, n int) {
	t.Helper()
	for range n {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func state(t *testing.T, db *store.Store, id string) model.Feature {
	t.Helper()
	f, err := db.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func TestEndToEndIndependentEvidenceAndIdle(t *testing.T) {
	e, db, b, r := fixture(t)
	enqueue(t, db, contract("behavior"))
	ticks(t, e, 12)
	f := state(t, db, "behavior")
	if f.State != "accepted" || f.Receipt != "integrated:candidate" {
		t.Fatalf("target not proven: %+v", f)
	}
	if r.checks != 1 || r.commits != 1 || r.integrations != 1 || len(b.requests) != 2 {
		t.Fatalf("wrong lifecycle checks=%d commits=%d integrates=%d calls=%d", r.checks, r.commits, r.integrations, len(b.requests))
	}
	if b.requests[0].AttemptID == b.requests[1].AttemptID || !strings.HasSuffix(b.requests[1].AttemptID, ":review:1") {
		t.Fatal("review reused builder session")
	}
	for _, request := range b.requests {
		if request.EnvironmentID != "" {
			t.Fatal("fresh stage retained a previous BB environment")
		}
	}
	if r.snapshots != 1 || r.checkWorkspace != b.requests[1].Workspace || r.checkWorkspace == b.requests[0].Workspace {
		t.Fatal("review and checks did not share a fresh committed snapshot")
	}
	if got := strings.Join(r.operations, ","); got != "commit,snapshot:candidate,check" {
		t.Fatalf("candidate operations out of order: %s", got)
	}
	if !strings.Contains(b.requests[1].Prompt, "Controller-observed deterministic evidence") {
		t.Fatal("reviewer lacked actual controller check results")
	}
	evidence, err := db.Evidence(context.Background(), f.ID)
	if err != nil || len(evidence) != 2 {
		t.Fatalf("missing evidence: %+v %v", evidence, err)
	}
	ticks(t, e, 5)
	if len(b.requests) != 2 {
		t.Fatal("idle loop dispatched model work")
	}
}
func TestFailureConsumesBoundedRepairAttempts(t *testing.T) {
	e, db, b, r := fixture(t)
	r.checkErr = errors.New("regression assertion failed")
	enqueue(t, db, contract("failure"))
	ticks(t, e, 20)
	f := state(t, db, "failure")
	if f.State != "blocked" || f.Attempts != 2 || len(b.requests) != 2 || r.integrations != 0 {
		t.Fatalf("repair bound failed: %+v calls=%d", f, len(b.requests))
	}
	if !strings.Contains(b.requests[1].Prompt, "regression assertion failed") {
		t.Fatal("repair omitted concrete failure")
	}
	evidence, err := db.Evidence(context.Background(), f.ID)
	if err != nil || len(evidence) != 2 {
		t.Fatalf("missing failure evidence: %+v %v", evidence, err)
	}
	for _, item := range evidence {
		if item.CandidateSHA != "candidate" || item.Passed {
			t.Fatalf("failure was not bound to the checked candidate: %+v", item)
		}
	}
	if err := db.Retry(context.Background(), f.ID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 3)
	if len(b.requests) != 2 {
		t.Fatal("operator retry reset configured attempt limit")
	}
}
func TestReviewerAndCheckCannotMutateCandidate(t *testing.T) {
	for _, stage := range []string{"review", "check"} {
		t.Run(stage, func(t *testing.T) {
			e, db, b, r := fixture(t)
			if stage == "check" {
				r.mutateCheck = true
			} else {
				b.pollHook = func(s string) {
					if s == "review" {
						r.fp = "reviewer-write"
					}
				}
			}
			enqueue(t, db, contract("readonly"))
			ticks(t, e, 12)
			f := state(t, db, "readonly")
			if f.State != "blocked" || r.integrations != 0 {
				t.Fatalf("mutating %s accepted: %+v", stage, f)
			}
		})
	}
}
func TestRestartReconcilesUncertainSpawnWithoutDuplicate(t *testing.T) {
	e, db, b, r := fixture(t)
	b.spawnErr = errors.New("connection lost after request")
	enqueue(t, db, contract("restart"))
	ticks(t, e, 3)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Join(e.cfg.StateDir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e = New(e.cfg, reopened, b, r)
	ticks(t, e, 15)
	if f := state(t, reopened, "restart"); f.State != "accepted" {
		t.Fatalf("recovery failed: %+v", f)
	}
	if len(b.requests) != 2 {
		t.Fatal("uncertain dispatch was sent more than once")
	}
}
func TestAmbiguousSpawnBlocksOwnershipAndRetry(t *testing.T) {
	e, db, b, _ := fixture(t)
	b.spawnErr = errors.New("unknown transport outcome")
	b.findEmpty = true
	enqueue(t, db, contract("ambiguous"))
	ticks(t, e, 6)
	if state(t, db, "ambiguous").State != "blocked" || len(b.requests) != 1 {
		t.Fatal("ambiguous effect retried or accepted")
	}
	if err := db.Retry(context.Background(), "ambiguous", model.TrustedLocalAudit()); err == nil {
		t.Fatal("retry discarded uncertain owned work")
	}
}
func TestRunningCursorDurableAndCancellationSettles(t *testing.T) {
	e, db, b, _ := fixture(t)
	b.idle = true
	enqueue(t, db, contract("running"))
	ticks(t, e, 5)
	active, err := db.Active(context.Background())
	if err != nil || len(active) != 1 || active[0].Cursor != "opaque-cursor" {
		t.Fatalf("cursor not persisted: %+v %v", active, err)
	}
	if len(b.requests) != 1 {
		t.Fatal("idle incorrectly treated as completion")
	}
	if err = db.Cancel(context.Background(), "running", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 2)
	if state(t, db, "running").State != "cancelled" {
		t.Fatal("cancel did not reconcile native session")
	}
}
func TestDailyCallLimitAndPolicyRevocation(t *testing.T) {
	e, db, b, r := fixture(t)
	e.cfg.MaxCallsPerDay = 1
	enqueue(t, db, contract("limited"))
	ticks(t, e, 10)
	if len(b.requests) != 1 || r.integrations != 0 {
		t.Fatal("daily budget exceeded")
	}
	e.cfg.PolicyHash = "revoked"
	ticks(t, e, 2)
	f := state(t, db, "limited")
	if f.State != "blocked" || !strings.Contains(f.Reason, "policy changed") {
		t.Fatalf("stale approval used: %+v", f)
	}
}
func TestProtectedPathsAndReleaseAllowlist(t *testing.T) {
	e, db, _, r := fixture(t)
	r.paths = []string{"secrets/nested/key"}
	enqueue(t, db, contract("protected"))
	ticks(t, e, 10)
	if state(t, db, "protected").State != "blocked" || r.integrations != 0 {
		t.Fatal("protected change integrated")
	}
	for _, file := range []string{"../escape", "a/../secrets/key", "/absolute", "x\\y"} {
		if err := e.paths(contract("x"), []string{file}); err == nil {
			t.Fatalf("unsafe path accepted: %s", file)
		}
	}
	if !policy.Matches("**/credentials/**", "src/credentials/token") || policy.Matches("src/*.go", "src/sub/main.go") {
		t.Fatal("path matcher violated segment boundaries")
	}
}
func TestProductionRequiresCurrentQualification(t *testing.T) {
	e, db, b, r := fixture(t)
	f := contract("production")
	f.CompletionTarget = "production"
	enqueue(t, db, f)
	ticks(t, e, 2)
	if state(t, db, f.ID).State != "blocked" || len(b.requests) != 0 || r.releases != 0 {
		t.Fatal("production dispatched without operator allowlist")
	}
}
func TestPlanProposesDAGWithoutApprovingOrCommitting(t *testing.T) {
	e, db, b, r := fixture(t)
	parent := contract("goal")
	parent.Kind = "plan"
	parent.Checks = nil
	parent.CompletionTarget = "reviewed"
	first := contract("first")
	second := contract("second")
	second.DependsOn = []string{"first"}
	payload, _ := json.Marshal(planResult{Features: []model.Feature{second, first}})
	b.plan = string(payload)
	enqueue(t, db, parent)
	ticks(t, e, 8)
	if state(t, db, "goal").State != "accepted" {
		t.Fatalf("plan failed: %+v", state(t, db, "goal"))
	}
	for _, id := range []string{"first", "second"} {
		f := state(t, db, id)
		if f.State != "proposed" || f.PolicyHash != "" {
			t.Fatal("planner approved child")
		}
	}
	if len(b.requests) != 1 || r.commits != 0 || r.checks != 0 || r.integrations != 0 {
		t.Fatal("planning performed implementation effects")
	}
}
func TestPlanRejectsCyclesBeforeImport(t *testing.T) {
	e, db, b, _ := fixture(t)
	parent := contract("goal")
	parent.Kind = "plan"
	parent.Checks = nil
	parent.CompletionTarget = "reviewed"
	a := contract("a")
	c := contract("b")
	a.DependsOn = []string{"b"}
	c.DependsOn = []string{"a"}
	payload, _ := json.Marshal(planResult{Features: []model.Feature{a, c}})
	b.plan = string(payload)
	enqueue(t, db, parent)
	ticks(t, e, 8)
	if state(t, db, "goal").State != "blocked" {
		t.Fatal("cyclic plan accepted")
	}
	features, err := db.List(context.Background())
	if err != nil || len(features) != 1 {
		t.Fatal("invalid DAG partially imported")
	}
}
func TestRestartDoesNotRepeatIntegrationIntent(t *testing.T) {
	e, db, _, r := fixture(t)
	enqueue(t, db, contract("intent"))
	a, err := db.Claim(context.Background(), "intent", "policy", 2, 2, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a = ownIntegration(t, e, a)
	a.Stage = "integrating"
	if err = db.Save(context.Background(), a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 2)
	if r.integrations != 0 || state(t, db, "intent").State != "blocked" {
		t.Fatal("uncertain integration repeated")
	}
}
func TestCancellationBetweenIntegrationAndReleasePreventsDeploy(t *testing.T) {
	e, db, _, r := fixture(t)
	p := e.cfg.Projects["demo"]
	p.Releases = map[string]model.Release{"staging": {Allowed: true, Qualified: true, AllowedRisks: []string{"normal"}, AllowedPaths: []string{"**"}, ExpiresAt: time.Now().Add(time.Hour), Deploy: model.Command{Argv: []string{"deploy"}}, Observe: model.Command{Argv: []string{"observe"}}, Rollback: model.Command{Argv: []string{"rollback"}}, MinimumObservations: 1, ObservationSeconds: 1}}
	e.cfg.Projects["demo"] = p
	f := contract("staging")
	f.CompletionTarget = "staging"
	r.integrationHook = func() {
		if err := db.Cancel(context.Background(), f.ID, model.TrustedLocalAudit()); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(t, db, f)
	ticks(t, e, 12)
	if r.integrations != 1 || r.releases != 0 || state(t, db, f.ID).State != "cancelled" {
		t.Fatalf("cancellation allowed deployment: %+v releases=%d", state(t, db, f.ID), r.releases)
	}
}

func TestFailedReleaseRetainsObservationAndRollbackEvidence(t *testing.T) {
	e, db, _, r := fixture(t)
	p := e.cfg.Projects["demo"]
	p.Releases = map[string]model.Release{"staging": {Allowed: true, Qualified: true, AllowedRisks: []string{"normal"}, AllowedPaths: []string{"**"}, ExpiresAt: time.Now().Add(time.Hour), Deploy: model.Command{Argv: []string{"deploy"}}, Observe: model.Command{Argv: []string{"observe"}}, Rollback: model.Command{Argv: []string{"rollback"}}, MinimumObservations: 1, ObservationSeconds: 1}}
	e.cfg.Projects["demo"] = p
	f := contract("failed-release")
	f.CompletionTarget = "staging"
	r.releaseErr = errors.New("observation failed")
	r.releaseEvidence = []model.Evidence{
		{Kind: "release.observe", CandidateSHA: "candidate", Passed: false, Detail: "route unhealthy"},
		{Kind: "release.rollback", CandidateSHA: "candidate", Passed: true, Detail: "prior version restored"},
	}
	enqueue(t, db, f)
	ticks(t, e, 14)
	if state(t, db, f.ID).State != "blocked" || r.releases != 1 {
		t.Fatalf("release not blocked: %+v", state(t, db, f.ID))
	}
	evidence, err := db.Evidence(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]model.Evidence{}
	for _, item := range evidence {
		found[item.Kind] = item
	}
	if found["release.observe"].Detail != "route unhealthy" || found["release.observe"].Passed || !found["release.rollback"].Passed {
		t.Fatalf("lost failed release receipts: %+v", evidence)
	}
	if err = db.Retry(context.Background(), f.ID, model.TrustedLocalAudit()); err == nil {
		t.Fatal("unreconciled deployment allowed blind retry")
	}
}

func TestTerminalJSONCannotCarryStaleApprovalFromMalformedPrefix(t *testing.T) {
	var result reviewResult
	text := `{"approved":true,"summary":"incomplete" invalid prefix {"summary":"only a summary","findings":[],"acceptance":[]}`
	if err := decodeResult(text, &result); err != nil {
		t.Fatal(err)
	}
	if result.Approved {
		t.Fatal("malformed prefix leaked approval into terminal object")
	}
	if err := decodeResult(`{"approved":true} trailing commentary`, &reviewResult{}); err == nil {
		t.Fatal("nonterminal JSON accepted")
	}
	if err := decodeResult("Explanation\n```json\n{\"approved\":false,\"summary\":\"rejected\",\"findings\":[]}\n```", &reviewResult{}); err != nil {
		t.Fatal(err)
	}
}

func TestPausePreventsDispatchAndDeadlineSurvivesRestart(t *testing.T) {
	e, db, b, _ := fixture(t)
	enqueue(t, db, contract("paused"))
	if err := db.SetPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 3)
	if len(b.requests) != 0 || state(t, db, "paused").Attempts != 0 {
		t.Fatal("pause admitted work")
	}
	if err := db.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	ticks(t, e, 2)
	attempts, err := db.Active(context.Background())
	if err != nil || len(attempts) != 1 {
		t.Fatal("missing attempt")
	}
	a := attempts[0]
	a.Deadline = time.Now().Add(-time.Second)
	if err = db.Save(context.Background(), a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	e = New(e.cfg, db, b, e.repos)
	ticks(t, e, 2)
	if len(b.requests) != 0 || state(t, db, "paused").State != "blocked" {
		t.Fatal("restart reset attempt deadline")
	}
}
