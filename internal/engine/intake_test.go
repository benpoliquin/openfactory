package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openfactory/internal/gitrepo"
	"openfactory/internal/intake"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

// Only the semantic model boundary is deterministic here. SQLite lifecycle,
// consent manifests, reservations and the sealed empty intake workspace are real.
// Neither these outputs nor the persistence-only credential fixture constitute
// model reasoning, WebAuthn cryptographic verification, or product/check evidence.
type intakeEngineBackend struct {
	db                                                     *store.Store
	requests                                               []model.SpawnRequest
	sessions                                               map[string]model.Session
	operations                                             []string
	projectRoots                                           []string
	output                                                 string
	idle, uncertainSpawn, missingFind, stopPending, mutate bool
	stopCompletes                                          bool
	onPoll                                                 func()
}

func (*intakeEngineBackend) Doctor(context.Context) error { return nil }
func (b *intakeEngineBackend) EnsureProject(_ context.Context, _ string, root string) (string, error) {
	b.projectRoots = append(b.projectRoots, root)
	return "deterministic-intake-project", nil
}
func (b *intakeEngineBackend) Spawn(ctx context.Context, r model.SpawnRequest) (model.Session, error) {
	active, err := b.db.Active(ctx)
	if err != nil {
		return model.Session{}, err
	}
	var owner model.Attempt
	for _, a := range active {
		if a.DispatchKey == r.AttemptID {
			owner = a
		}
	}
	if owner.ID == "" || owner.Stage != "intake_dispatching" {
		return model.Session{}, errors.New("spawn lacks persisted ordinary attempt reservation")
	}
	step, err := b.db.IntakeStepForFeature(ctx, owner.FeatureID)
	if err != nil {
		return model.Session{}, err
	}
	request, err := b.db.IntakeRequest(ctx, step.RequestID)
	if err != nil {
		return model.Session{}, err
	}
	if request.LifetimeDispatches < 1 || r.Model != step.Model || r.EnvironmentID != "" || r.AttemptID != owner.ID+":intake:"+step.ID {
		return model.Session{}, errors.New("spawn lacks exact persisted model, lifetime charge, or fresh intake identity")
	}
	b.requests = append(b.requests, r)
	b.operations = append(b.operations, "Spawn:"+r.AttemptID)
	s := model.Session{ThreadID: fmt.Sprintf("deterministic-intake-thread-%d", len(b.requests)), EnvironmentID: fmt.Sprintf("deterministic-intake-environment-%d", len(b.requests))}
	b.sessions[r.AttemptID] = s
	if b.uncertainSpawn {
		return model.Session{}, errors.New("deterministic lost response after worker creation")
	}
	return s, nil
}
func (b *intakeEngineBackend) Find(_ context.Context, _ string, key string) ([]model.Session, error) {
	b.operations = append(b.operations, "Find:"+key)
	if !b.missingFind {
		if s, ok := b.sessions[key]; ok {
			return []model.Session{s}, nil
		}
	}
	return nil, nil
}
func (b *intakeEngineBackend) Stop(_ context.Context, thread string) error {
	b.operations = append(b.operations, "Stop:"+thread)
	return nil
}
func (b *intakeEngineBackend) Poll(_ context.Context, thread, _ string) (model.Observation, error) {
	b.operations = append(b.operations, "Poll:"+thread)
	stopped := false
	for _, op := range b.operations {
		if op == "Stop:"+thread {
			stopped = true
		}
	}
	if stopped {
		if b.stopPending {
			return model.Observation{State: "running", Cursor: "stop-not-settled"}, nil
		}
		if b.stopCompletes {
			return model.Observation{State: "completed", Cursor: "completed-after-stop", Output: b.output}, nil
		}
		return model.Observation{State: "interrupted", Cursor: "stop-settled"}, nil
	}
	if b.idle {
		return model.Observation{State: "running", Cursor: "durable-intake-cursor"}, nil
	}
	if b.mutate {
		for _, r := range b.requests {
			if b.sessions[r.AttemptID].ThreadID == thread {
				if err := os.WriteFile(filepath.Join(r.Workspace, "unauthorized.txt"), []byte("forbidden model mutation"), 0600); err != nil {
					return model.Observation{}, err
				}
			}
		}
	}
	if b.onPoll != nil {
		hook := b.onPoll
		b.onPoll = nil
		hook()
	}
	return model.Observation{State: "completed", Cursor: "terminal-intake-cursor", Output: b.output}, nil
}

// Intake uses the real repository manager's isolated workspace implementation.
// All product-code/check paths fail, rather than fabricating successful evidence.
type intakeEngineRepositories struct {
	*gitrepo.Manager
	forbidden  []string
	prepared   []model.Project
	reopened   []string
	workspaces []model.IntakeWorkspace
}

func (r *intakeEngineRepositories) PrepareIntake(ctx context.Context, name string, project model.Project, attempt, request, step string) (model.IntakeWorkspace, error) {
	r.prepared = append(r.prepared, project)
	w, err := r.Manager.PrepareIntake(ctx, name, project, attempt, request, step)
	if err == nil {
		r.workspaces = append(r.workspaces, w)
	}
	return w, err
}
func (r *intakeEngineRepositories) ReopenIntake(ctx context.Context, path, project, attempt, request, step string) (model.IntakeWorkspace, error) {
	r.reopened = append(r.reopened, path)
	return r.Manager.ReopenIntake(ctx, path, project, attempt, request, step)
}

func (r *intakeEngineRepositories) deny(op string) error {
	r.forbidden = append(r.forbidden, op)
	return fmt.Errorf("intake has no %s authority", op)
}
func (r *intakeEngineRepositories) Prepare(context.Context, string, model.Project, string) (model.Workspace, error) {
	return model.Workspace{}, r.deny("product-prepare")
}
func (r *intakeEngineRepositories) Reconcile(context.Context, string, model.Project, model.ReconcileRequest) (model.ReconciledCandidate, error) {
	return model.ReconciledCandidate{}, r.deny("candidate-reconcile")
}
func (r *intakeEngineRepositories) Fingerprint(context.Context, string) (string, error) {
	return "", r.deny("product-fingerprint")
}
func (r *intakeEngineRepositories) Commit(context.Context, string, string) (string, error) {
	return "", r.deny("commit")
}
func (r *intakeEngineRepositories) Snapshot(context.Context, string, string) (model.Workspace, error) {
	return model.Workspace{}, r.deny("candidate-snapshot")
}
func (r *intakeEngineRepositories) Check(context.Context, string, model.Command) (string, error) {
	return "", r.deny("check")
}
func (r *intakeEngineRepositories) ChangedPaths(context.Context, string, string, string) ([]string, error) {
	return nil, r.deny("product-diff")
}
func (r *intakeEngineRepositories) Integrate(context.Context, string, model.Project, model.Attempt, string) (string, error) {
	return "", r.deny("integration")
}
func (r *intakeEngineRepositories) PullRequest(context.Context, string, model.Project, model.Attempt, model.Feature) (string, error) {
	return "", r.deny("pull-request")
}
func (r *intakeEngineRepositories) PreflightRelease(context.Context, string, model.Project, model.Attempt, model.Feature) error {
	return r.deny("release-preflight")
}
func (r *intakeEngineRepositories) Release(context.Context, string, model.Project, model.Attempt, model.Feature) ([]model.Evidence, error) {
	return nil, r.deny("release")
}

type intakeEngineFixture struct {
	e                 *Engine
	db                *store.Store
	backend           *intakeEngineBackend
	repos             *intakeEngineRepositories
	path, token, csrf string
	sequence          int
}

func newIntakeEngineFixture(t *testing.T, configure func(*model.Config)) *intakeEngineFixture {
	t.Helper()
	// A short root also exercises OMP's actual flattened cwd length constraint
	// without making the length depend on the descriptive Go subtest name.
	dir, err := os.MkdirTemp("", "fi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{StateDir: filepath.Join(dir, "state"), WorkspaceDir: filepath.Join(dir, "work"), PolicyHash: intake.SHA256([]byte("intake-test-policy")), AstraModel: "test-only", MaxConcurrent: 1, MaxAttempts: 2, MaxCallsPerDay: 20, AttemptTimeoutSeconds: 3600, MaxPromptBytes: 1 << 20, Intake: model.IntakePolicy{Enabled: true, MaxDraftCalls: 12}, Discovery: model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{}), Projects: map[string]model.Project{"demo": {Trusted: true, Repository: filepath.Join(dir, "never-clone-product"), Branch: "main", MaxConcurrent: 1, Checks: map[string]model.Command{"unit": {Argv: []string{"must-not-execute"}}}}}}
	if configure != nil {
		configure(&cfg)
	}
	if err = os.MkdirAll(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	f := &intakeEngineFixture{path: filepath.Join(cfg.StateDir, "state.sqlite"), token: intake.SHA256([]byte("session-token")), csrf: intake.SHA256([]byte("csrf-token"))}
	f.db, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	operator := model.Operator{ID: "intake-test-operator", Label: "Persistence-only test operator", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("persistence-only-handle")}
	if err = f.db.CreateOperator(ctx, operator); err != nil {
		t.Fatal(err)
	}
	secret := intake.SHA256([]byte("pairing-secret"))
	if err = f.db.CreatePairing(ctx, model.OperatorPairing{ID: "pairing", SecretHash: secret, OperatorID: operator.ID, ProjectScope: operator.ProjectScope, MaxCredentials: 1, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err = f.db.CreateAuthChallenge(ctx, model.AuthChallenge{ID: "registration", Purpose: "registration", OperatorID: operator.ID, PairingID: "pairing", SessionData: json.RawMessage(`{}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	credential := model.OperatorCredential{ID: "intake-test-credential", OperatorID: operator.ID, PublicKey: []byte("persistence-only-public-key"), CredentialData: json.RawMessage(`{}`), UserHandle: operator.UserHandle, Fingerprint: intake.SHA256([]byte("persistence-only-credential")), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err = f.db.RegisterPendingCredential(ctx, "pairing", secret, "registration", credential); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.ConfirmPairing(ctx, "pairing", credential.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err = f.db.CreateBrowserSession(ctx, model.BrowserSession{ID: "browser-session", TokenHash: f.token, CSRFHash: f.csrf, OperatorID: operator.ID, CredentialID: credential.ID, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.backend = &intakeEngineBackend{db: f.db, sessions: map[string]model.Session{}, output: intakeBlockedOutput()}
	f.repos = &intakeEngineRepositories{Manager: gitrepo.New(cfg)}
	f.e = New(cfg, f.db, f.backend, f.repos)
	return f
}
func (f *intakeEngineFixture) uuid() string {
	f.sequence++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.sequence)
}
func (f *intakeEngineFixture) command(t *testing.T, action string, request *model.IntakeRequest, payload any) model.CommandReceipt {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	c := model.TypedCommand{CommandID: f.uuid(), Action: action, Payload: data}
	if request != nil {
		c.RequestID = request.ID
		version := request.ContentVersion
		c.ExpectedContentVersion = &version
	}
	r, err := f.db.ExecuteCommand(context.Background(), f.token, f.csrf, c, f.e.cfg, nil)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return r
}
func (f *intakeEngineFixture) createSaved(t *testing.T, text string) model.IntakeRequest {
	t.Helper()
	receipt := f.command(t, model.ActionRequestCreate, nil, model.RequestCreatePayload{Project: "demo", Text: text, ClientMessageID: f.uuid()})
	var request model.IntakeRequest
	if err := json.Unmarshal(receipt.Result, &request); err != nil {
		t.Fatal(err)
	}
	if request.ID == "" {
		t.Fatalf("request.create omitted durable request: %s", receipt.Result)
	}
	if request.State != model.IntakeCollecting || request.ActiveStepID != "" || request.LifetimeDispatches != 0 {
		t.Fatalf("save granted drafting authority: %+v", request)
	}
	return request
}
func (f *intakeEngineFixture) draft(t *testing.T, id string) model.IntakeRequest {
	t.Helper()
	r := f.request(t, id)
	disclosure, err := f.db.IntakeDisclosure(context.Background(), f.e.cfg, r.OperatorID, r.Project, r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.command(t, model.ActionIntakeDraft, &r, model.IntakeDraftPayload{Disclosure: disclosure})
	return f.request(t, id)
}
func (f *intakeEngineFixture) create(t *testing.T, text string) model.IntakeRequest {
	t.Helper()
	r := f.createSaved(t, text)
	return f.draft(t, r.ID)
}
func (f *intakeEngineFixture) request(t *testing.T, id string) model.IntakeRequest {
	t.Helper()
	r, err := f.db.IntakeRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *intakeEngineFixture) append(t *testing.T, id, text string, q *model.IntakeQuestionRef) model.IntakeRequest {
	t.Helper()
	r := f.request(t, id)
	f.command(t, model.ActionTurnAppend, &r, model.TurnAppendPayload{ClientMessageID: f.uuid(), Text: text, Question: q})
	return f.request(t, id)
}
func (f *intakeEngineFixture) until(t *testing.T, description string, done func() bool) {
	t.Helper()
	for range 20 {
		if done() {
			return
		}
		ticks(t, f.e, 1)
	}
	t.Fatalf("did not reach %s; operations=%v", description, f.backend.operations)
}
func (f *intakeEngineFixture) active(t *testing.T) []model.Attempt {
	t.Helper()
	a, err := f.db.Active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *intakeEngineFixture) running(t *testing.T, id string) model.Attempt {
	t.Helper()
	f.until(t, "persisted intake_running", func() bool {
		for _, a := range f.active(t) {
			s, err := f.db.IntakeStepForFeature(context.Background(), a.FeatureID)
			if err == nil && s.RequestID == id && a.Stage == "intake_running" {
				return true
			}
		}
		return false
	})
	for _, a := range f.active(t) {
		s, err := f.db.IntakeStepForFeature(context.Background(), a.FeatureID)
		if err == nil && s.RequestID == id {
			return a
		}
	}
	t.Fatal("running attempt disappeared")
	return model.Attempt{}
}
func (f *intakeEngineFixture) restart(t *testing.T) {
	t.Helper()
	cfg := f.e.cfg
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.backend.db = f.db
	f.repos.Manager = gitrepo.New(cfg)
	f.e = New(cfg, f.db, f.backend, f.repos)
}
func (f *intakeEngineFixture) assertIntakeWork(t *testing.T, id string, wantAttempts, wantCalls, wantSpawns int) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(f.path)}).String() + "?mode=ro"
	ledger, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var attempts, calls int
	if err = ledger.QueryRow(`SELECT
		(SELECT count(*) FROM attempts a JOIN intake_steps s ON s.feature_id=a.feature_id WHERE s.request_id=?),
		(SELECT count(*) FROM calls c JOIN attempts a ON a.id=c.attempt_id JOIN intake_steps s ON s.feature_id=a.feature_id WHERE s.request_id=?)`, id, id).Scan(&attempts, &calls); err != nil {
		t.Fatal(err)
	}
	steps, err := f.db.IntakeSteps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	spawns := 0
	for _, step := range steps {
		for _, request := range f.backend.requests {
			if strings.HasSuffix(request.AttemptID, ":intake:"+step.ID) {
				spawns++
			}
		}
	}
	if attempts != wantAttempts || calls != wantCalls || spawns != wantSpawns {
		t.Fatalf("request %s work: attempts=%d calls=%d spawns=%d; want %d/%d/%d", id, attempts, calls, spawns, wantAttempts, wantCalls, wantSpawns)
	}
}
func (f *intakeEngineFixture) noCodeAuthority(t *testing.T) {
	t.Helper()
	if len(f.repos.forbidden) != 0 {
		t.Fatalf("intake attempted product authority: %v", f.repos.forbidden)
	}
	features, err := f.db.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range features {
		if feature.IntakeStepID == "" {
			t.Fatalf("draft created a product feature without human approval: %+v", feature)
		}
		if feature.State == "accepted" || feature.Receipt != "" || len(feature.Checks) != 0 {
			t.Fatalf("intake manufactured code authority: %+v", feature)
		}
		if _, err := f.db.FeatureStatus(context.Background(), feature.ID, f.e.cfg); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("internal drafting feature leaked onto product status: %v", err)
		}
		evidence, err := f.db.Evidence(context.Background(), feature.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(evidence) != 0 {
			t.Fatalf("historical fabricated evidence: %+v", evidence)
		}
	}
	// Product-facing detail deliberately hides internal features. Inspect the
	// actual ledger read-only so historical candidate/check absence is still
	// asserted, rather than weakening the test to a hidden dashboard response.
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(f.path)}).String() + "?mode=ro"
	ledger, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var checks, evidence, candidates int
	if err = ledger.QueryRow(`SELECT (SELECT count(*) FROM checks),(SELECT count(*) FROM evidence),(SELECT count(*) FROM attempts WHERE coalesce(json_extract(data,'$.candidate_sha'),'')!='' OR coalesce(json_extract(data,'$.source_candidate_sha'),'')!='' OR coalesce(json_extract(data,'$.candidate_generation'),0)!=0)`).Scan(&checks, &evidence, &candidates); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || evidence != 0 || candidates != 0 {
		t.Fatalf("intake acquired code/check authority: checks=%d evidence=%d candidates=%d", checks, evidence, candidates)
	}
}
func intakeQuestionsOutput() string {
	return `{"type":"questions","message":"Durability changes the contract.","questions":[{"local_id":"durability","text":"Must acknowledgement survive process loss?","consequence":"Determines whether durable storage is required.","answer_type":"single_choice","choices":["yes","no"]}]}`
}
func intakeBlockedOutput() string {
	return `{"type":"blocked","message":"Cannot draft an exact contract yet.","missing_prerequisites":["Operator must specify the external delivery guarantee."]}`
}
func intakeDraftOutput(t *testing.T, route string) (string, string) {
	t.Helper()
	var source []byte
	var err error
	format := model.IntakeFormatFeature
	switch route {
	case model.IntakeRouteFeature:
		source, err = intake.Export(contract("phone-draft"))
	case model.IntakeRoutePlan:
		var plan model.Feature
		plan, err = intake.NewPlan("phone-plan", "demo", "Plan durable delivery", "Identify consequential requirements before implementation.")
		if err == nil {
			source, err = intake.Export(plan)
		}
	case model.IntakeRouteProgram:
		format = model.IntakeFormatProgram
		source, err = json.Marshal(model.ProgramSpec{ID: "phone-program", Project: "demo", Title: "Delivery discovery", Objective: "Evaluate duplicate and reordered delivery before implementation", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"test-only"}}, Implementation: model.ProgramImplementationPolicy{PermittedFeatureKinds: []string{"feature"}, PermittedCompletionTargets: []string{"integrated"}}})
	}
	if err != nil {
		t.Fatal(err)
	}
	assumptions := []string{"Only the disclosed demo project is in scope."}
	risks := []string{"No implementation or model proof is supplied by this deterministic test."}
	if route == model.IntakeRouteFeature {
		assumptions = []string{}
		risks = []string{}
	}
	// Empty arrays are valid and required wire fields: do not let omitempty
	// accidentally remove them before the real engine/parser sees the result.
	data, err := json.Marshal(map[string]any{"type": "draft", "message": "Exact contract for operator review only.", "route": route, "format": format, "canonical_source": string(source), "assumptions": assumptions, "unresolved_risks": risks})
	if err != nil {
		t.Fatal(err)
	}
	return string(data), string(source)
}

func TestIntakeEngineTypedResultsUseOrdinaryAttemptsWithoutCodeAuthority(t *testing.T) {
	for _, kind := range []string{"questions", "blocked", model.IntakeRouteFeature, model.IntakeRoutePlan, model.IntakeRouteProgram} {
		t.Run(kind, func(t *testing.T) {
			f := newIntakeEngineFixture(t, nil)
			wantState := model.IntakeReviewReady
			source := ""
			switch kind {
			case "questions":
				f.backend.output = intakeQuestionsOutput()
				wantState = model.IntakeNeedsAnswers
			case "blocked":
				f.backend.output = intakeBlockedOutput()
				wantState = model.IntakeBlocked
			default:
				f.backend.output, source = intakeDraftOutput(t, kind)
			}
			r := f.create(t, "Draft a bounded delivery contract; ask before choosing consequential guarantees.")
			a := f.running(t, r.ID)
			step, err := f.db.IntakeStepForFeature(context.Background(), a.FeatureID)
			if err != nil {
				t.Fatal(err)
			}
			if step.AttemptID != a.ID || step.ThreadID != a.ThreadID || step.ContentVersion != r.ContentVersion || a.CandidateSHA != "" {
				t.Fatalf("missing ordinary persisted ownership: step=%+v attempt=%+v", step, a)
			}
			f.until(t, "typed result settlement", func() bool { return f.request(t, r.ID).State == wantState })
			r = f.request(t, r.ID)
			if r.ActiveStepID != "" || r.LifetimeDispatches != 1 || len(f.active(t)) != 0 || len(f.backend.requests) != 1 {
				t.Fatalf("result did not settle exactly one reserved attempt: %+v", r)
			}
			wantFeatureState := "reviewed"
			if kind == "blocked" {
				wantFeatureState = "blocked"
			}
			if got := state(t, f.db, a.FeatureID).State; got != wantFeatureState {
				t.Fatalf("typed %s result settled feature as %s, want %s", kind, got, wantFeatureState)
			}
			step, err = f.db.GetIntakeStep(context.Background(), step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if step.OutputSHA == "" {
				t.Fatal("typed result lacks immutable output hash")
			}
			switch kind {
			case "questions":
				questions, err := f.db.IntakeQuestions(context.Background(), r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(questions) != 1 || questions[0].ID == questions[0].LocalID || questions[0].ContentVersion != r.ContentVersion || questions[0].InputManifestSHA != step.InputManifestSHA || questions[0].OutputSHA != step.OutputSHA {
					t.Fatalf("consequential question lacks controller binding: %+v", questions)
				}
			case "blocked":
				if r.Wait == nil || r.Wait.Code != "prerequisite_missing" || !strings.Contains(r.Wait.Detail, "external delivery guarantee") {
					t.Fatalf("missing durable prerequisite wait: %+v", r)
				}
			default:
				draft, err := f.db.IntakeDraft(context.Background(), r.ID, r.LatestDraftRevision)
				if err != nil {
					t.Fatal(err)
				}
				if draft.Route != kind || draft.CanonicalSource != source || draft.SourceSHA != intake.SHA256([]byte(source)) || draft.StepID != step.ID || draft.InputManifestSHA != step.InputManifestSHA || draft.OutputSHA != step.OutputSHA {
					t.Fatalf("review did not preserve exact route/source/producer: %+v", draft)
				}
			}
			ticks(t, f.e, 3)
			if len(f.backend.requests) != 1 {
				t.Fatal("idle review/questions/blocked polling spent another model call")
			}
			f.noCodeAuthority(t)
		})
	}
}

func TestIntakeEngineSavingTextDoesNotGrantDraftingAuthority(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.output = intakeQuestionsOutput()
	r := f.createSaved(t, "Save an idea before consenting to model drafting.")
	ticks(t, f.e, 3)
	r = f.request(t, r.ID)
	if r.State != model.IntakeCollecting || r.ActiveStepID != "" || len(f.active(t)) != 0 || len(f.backend.requests) != 0 || r.LifetimeDispatches != 0 {
		t.Fatalf("saved initial text started model work without intake.draft: %+v", r)
	}
	r = f.append(t, r.ID, "Save another requirement without sending it to a provider.", nil)
	f.restart(t)
	ticks(t, f.e, 3)
	r = f.request(t, r.ID)
	if r.ActiveStepID != "" || len(f.active(t)) != 0 || len(f.backend.requests) != 0 || r.LifetimeDispatches != 0 {
		t.Fatalf("saved refinement/restart started model work without intake.draft: %+v", r)
	}
	f.draft(t, r.ID)
	f.until(t, "explicitly authorized question draft", func() bool { return f.request(t, r.ID).State == model.IntakeNeedsAnswers })
	questions, err := f.db.IntakeQuestions(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 {
		t.Fatalf("questions=%+v", questions)
	}
	q := questions[0]
	r = f.append(t, r.ID, "Yes, persist acknowledgements.", &model.IntakeQuestionRef{QuestionID: q.ID, ContentVersion: q.ContentVersion, InputManifestSHA: q.InputManifestSHA, OutputSHA: q.OutputSHA})
	ticks(t, f.e, 3)
	r = f.request(t, r.ID)
	if r.ActiveStepID != "" || len(f.active(t)) != 0 || len(f.backend.requests) != 1 || r.LifetimeDispatches != 1 {
		t.Fatalf("saved consequential answer implicitly dispatched another draft: %+v", r)
	}
	f.backend.output = intakeBlockedOutput()
	f.draft(t, r.ID)
	f.until(t, "second explicit draft", func() bool { return f.request(t, r.ID).State == model.IntakeBlocked })
	if len(f.backend.requests) != 2 || f.request(t, r.ID).LifetimeDispatches != 2 {
		t.Fatal("explicit saved-context grant did not dispatch exactly once")
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineSharesCapacityAndDailyCallAllowance(t *testing.T) {
	f := newIntakeEngineFixture(t, func(c *model.Config) { c.MaxCallsPerDay = 1 })
	f.backend.idle = true
	first := f.create(t, "First bounded draft.")
	a := f.running(t, first.ID)
	second := f.create(t, "Second bounded draft.")
	ticks(t, f.e, 4)
	active := f.active(t)
	if len(active) != 1 || active[0].ID != a.ID || len(f.backend.requests) != 1 {
		t.Fatalf("one active capacity bypassed: %+v requests=%d", active, len(f.backend.requests))
	}
	status, err := f.db.Status(context.Background(), model.StatusQuery{}, f.e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status.Limits.Global.Active != 1 || status.Calls.Used != 1 {
		t.Fatalf("internal work omitted from capacity/budget truth: %+v", status)
	}
	f.backend.idle = false
	f.until(t, "first result", func() bool { return f.request(t, first.ID).State == model.IntakeBlocked })
	f.until(t, "second waits on daily reservation", func() bool {
		for _, a := range f.active(t) {
			if a.Wait != nil && a.Wait.Code == "model_call_limit" {
				return true
			}
		}
		return false
	})
	ticks(t, f.e, 3)
	if len(f.backend.requests) != 1 || f.request(t, second.ID).LifetimeDispatches != 0 {
		t.Fatal("daily allowance rejection spawned or charged an intake call")
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineQueuedDraftWaitTracksCapacityAndOwnedAttempt(t *testing.T) {
	for _, tc := range []struct {
		name             string
		globalLimit      int
		maxCalls         int
		capacityWaitCode string
		dailyWait        bool
	}{
		{name: "global_limit", globalLimit: 2, maxCalls: 20, capacityWaitCode: "global_limit"},
		{name: "project_limit_with_global_capacity", globalLimit: 3, maxCalls: 20, capacityWaitCode: "project_limit"},
		{name: "daily_call_wait_after_claim", globalLimit: 2, maxCalls: 2, capacityWaitCode: "global_limit", dailyWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeEngineFixture(t, func(c *model.Config) {
				c.MaxConcurrent = tc.globalLimit
				c.MaxCallsPerDay = tc.maxCalls
				project := c.Projects["demo"]
				project.MaxConcurrent = 2
				c.Projects["demo"] = project
			})
			ctx := context.Background()
			f.backend.idle = true
			f.backend.uncertainSpawn = true
			first := f.create(t, "Explicitly consent to the draft whose Spawn response is lost.")
			if first.State != model.IntakeDraftQueued {
				t.Fatalf("explicit consent did not queue the first draft: %+v", first)
			}
			f.until(t, "first external worker created with a lost Spawn response", func() bool { return len(f.backend.requests) == 1 })
			active := f.active(t)
			if len(active) != 1 || active[0].Stage != "intake_dispatching" || active[0].ThreadID != "" {
				t.Fatalf("lost Spawn response did not retain its exact attempt: %+v", active)
			}
			intent := active[0]
			session := f.backend.sessions[intent.DispatchKey]
			if session.ThreadID == "" || session.EnvironmentID == "" {
				t.Fatalf("lost response fixture did not create a real owned session: %+v", session)
			}
			f.backend.uncertainSpawn = false
			firstAttempt := f.running(t, first.ID)
			found := false
			for _, operation := range f.backend.operations {
				if operation == "Find:"+intent.DispatchKey {
					found = true
				}
			}
			if !found || firstAttempt.ID != intent.ID || firstAttempt.DispatchKey != intent.DispatchKey || firstAttempt.ThreadID != session.ThreadID || firstAttempt.EnvironmentID != session.EnvironmentID {
				t.Fatalf("Find did not recover the original owned session: intent=%+v recovered=%+v operations=%v", intent, firstAttempt, f.backend.operations)
			}
			second := f.create(t, "Explicitly consent to a second held draft.")
			if second.State != model.IntakeDraftQueued {
				t.Fatalf("explicit consent did not queue the second draft: %+v", second)
			}
			secondAttempt := f.running(t, second.ID)
			secondSession := f.backend.sessions[secondAttempt.DispatchKey]
			if secondSession.ThreadID == "" || secondSession.EnvironmentID == "" || secondAttempt.ThreadID != secondSession.ThreadID || secondAttempt.EnvironmentID != secondSession.EnvironmentID || secondAttempt.ThreadID == firstAttempt.ThreadID {
				t.Fatalf("second draft lacks its own real session: first=%+v second=%+v session=%+v", firstAttempt, secondAttempt, secondSession)
			}
			for _, id := range []string{first.ID, second.ID} {
				if request := f.request(t, id); request.State != model.IntakeDrafting || request.LifetimeDispatches != 1 {
					t.Fatalf("held worker lacks its drafting request and single lifetime charge: %+v", request)
				}
				f.assertIntakeWork(t, id, 1, 1, 1)
			}
			third := f.create(t, "Explicitly consent to a draft that must wait for an owned slot.")
			if third.State != model.IntakeDraftQueued || third.LifetimeDispatches != 0 {
				t.Fatalf("explicit consent bypassed the draft queue: %+v", third)
			}
			f.until(t, "concrete queued capacity wait", func() bool { return f.request(t, third.ID).Wait != nil })
			assertQueued := func() {
				t.Helper()
				request := f.request(t, third.ID)
				if request.State != model.IntakeDraftQueued || request.Wait == nil || request.Wait.Code != tc.capacityWaitCode || request.Wait.Detail == "" || request.LifetimeDispatches != 0 {
					t.Fatalf("capacity-blocked intake must remain queued with the canonical gate wait: %+v", request)
				}
				steps, err := f.db.IntakeSteps(ctx, third.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(steps) != 1 || steps[0].ID != request.ActiveStepID || steps[0].AttemptID != "" {
					t.Fatalf("capacity wait created an attempt or replaced its queued step: request=%+v steps=%+v", request, steps)
				}
				feature, err := f.db.Get(ctx, steps[0].FeatureID)
				if err != nil {
					t.Fatal(err)
				}
				if feature.Attempts != 0 {
					t.Fatalf("capacity rejection consumed an attempt: %+v", feature)
				}
				f.assertIntakeWork(t, third.ID, 0, 0, 0)
				active := f.active(t)
				if len(active) != 2 {
					t.Fatalf("capacity wait changed owned worker count: %+v", active)
				}
				for _, attempt := range active {
					if attempt.ID != firstAttempt.ID && attempt.ID != secondAttempt.ID {
						t.Fatalf("capacity wait replaced an owned worker: %+v", active)
					}
				}
				status, err := f.db.Status(ctx, model.StatusQuery{}, f.e.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if status.Limits.Global.Active != 2 || status.Limits.Global.Limit != tc.globalLimit || status.Calls.Used != 2 {
					t.Fatalf("held intake work was not counted by shared admission and calls: %+v", status)
				}
				for _, project := range status.Limits.Projects {
					if project.Name == "demo" && (project.Active != 2 || project.Limit != 2) {
						t.Fatalf("project capacity does not explain the queued request: %+v", project)
					}
				}
			}
			assertQueued()
			queuedStepID := f.request(t, third.ID).ActiveStepID

			first = f.request(t, first.ID)
			f.command(t, model.ActionIntakeCancel, &first, model.EmptyCommandPayload{})
			f.backend.stopPending = true
			f.until(t, "nonterminal Stop/Poll retains the cancelled worker", func() bool {
				for _, attempt := range f.active(t) {
					if attempt.ID == firstAttempt.ID && attempt.Stage == "intake_stopping" && attempt.Wait != nil && attempt.Wait.Code == "intake_stop_pending" {
						return true
					}
				}
				return false
			})
			stopped, polledAfterStop := false, false
			for _, operation := range f.backend.operations {
				if operation == "Stop:"+firstAttempt.ThreadID {
					stopped = true
				}
				if stopped && operation == "Poll:"+firstAttempt.ThreadID {
					polledAfterStop = true
				}
			}
			if !polledAfterStop || f.request(t, first.ID).State != model.IntakeCancelling {
				t.Fatalf("cancellation did not preserve ownership through Stop/Poll: %v", f.backend.operations)
			}
			assertQueued()
			f.backend.stopPending = false
			f.until(t, "third intake claims the terminally released slot", func() bool {
				return f.request(t, third.ID).State == model.IntakeDrafting
			})
			third = f.request(t, third.ID)
			if third.ActiveStepID != queuedStepID || third.Wait != nil || third.LifetimeDispatches != 0 || f.request(t, first.ID).State != model.IntakeCancelled {
				t.Fatalf("successful claim did not clear the queue wait without dispatching: %+v", third)
			}
			steps, err := f.db.IntakeSteps(ctx, third.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != 1 || steps[0].ID != queuedStepID || steps[0].AttemptID == "" {
				t.Fatalf("released slot did not bind exactly one attempt to the original step: %+v", steps)
			}
			claimed := steps[0]
			active = f.active(t)
			if len(active) != 2 {
				t.Fatalf("terminal cancellation did not replace one owned worker: %+v", active)
			}
			claimedActive, secondActive := false, false
			for _, attempt := range active {
				claimedActive = claimedActive || attempt.ID == claimed.AttemptID && attempt.FeatureID == claimed.FeatureID
				secondActive = secondActive || attempt.ID == secondAttempt.ID
			}
			if !claimedActive || !secondActive {
				t.Fatalf("claim did not preserve the second worker and bind the third: %+v", active)
			}
			f.assertIntakeWork(t, third.ID, 1, 0, 0)

			if tc.dailyWait {
				var waiting model.Attempt
				f.until(t, "claimed third attempt waits on the real daily call reservation", func() bool {
					for _, attempt := range f.active(t) {
						if attempt.ID == claimed.AttemptID && attempt.Wait != nil && attempt.Wait.Code == "model_call_limit" {
							waiting = attempt
							return true
						}
					}
					return false
				})
				third = f.request(t, third.ID)
				if third.State != model.IntakeDrafting || third.ActiveStepID != claimed.ID || third.LifetimeDispatches != 0 || waiting.Wait.Detail == "" || !reflect.DeepEqual(third.Wait, waiting.Wait) {
					t.Fatalf("request did not expose its actual owned attempt's daily-call wait: request=%+v attempt=%+v", third, waiting)
				}
				f.assertIntakeWork(t, third.ID, 1, 0, 0)
			} else {
				thirdAttempt := f.running(t, third.ID)
				third = f.request(t, third.ID)
				if thirdAttempt.ID != claimed.AttemptID || thirdAttempt.FeatureID != claimed.FeatureID || third.ActiveStepID != claimed.ID || third.State != model.IntakeDrafting || third.Wait != nil || third.LifetimeDispatches != 1 {
					t.Fatalf("third intake did not dispatch exactly once from its original claim: request=%+v attempt=%+v", third, thirdAttempt)
				}
				f.assertIntakeWork(t, third.ID, 1, 1, 1)
			}
			f.assertIntakeWork(t, first.ID, 1, 1, 1)
			f.assertIntakeWork(t, second.ID, 1, 1, 1)
			f.noCodeAuthority(t)
		})
	}
}

func TestIntakeEngineLifetimeAllowanceSurvivesAnswerAndRestart(t *testing.T) {
	f := newIntakeEngineFixture(t, func(c *model.Config) { c.Intake.MaxDraftCalls = 1 })
	f.backend.output = intakeQuestionsOutput()
	r := f.create(t, "Ask whether acknowledgement must be durable.")
	f.until(t, "bound consequential question", func() bool { return f.request(t, r.ID).State == model.IntakeNeedsAnswers })
	questions, err := f.db.IntakeQuestions(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 {
		t.Fatalf("questions=%+v", questions)
	}
	q := questions[0]
	r = f.append(t, r.ID, "Yes, acknowledgement must survive process loss.", &model.IntakeQuestionRef{QuestionID: q.ID, ContentVersion: q.ContentVersion, InputManifestSHA: q.InputManifestSHA, OutputSHA: q.OutputSHA})
	f.restart(t)
	// Raising current policy cannot replenish the frozen per-request lifetime cap.
	f.e.cfg.Intake.MaxDraftCalls = 12
	r = f.request(t, r.ID)
	disclosure, err := f.db.IntakeDisclosure(context.Background(), f.e.cfg, r.OperatorID, r.Project, r.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(model.IntakeDraftPayload{Disclosure: disclosure})
	if err != nil {
		t.Fatal(err)
	}
	version := r.ContentVersion
	command := model.TypedCommand{CommandID: f.uuid(), Action: model.ActionIntakeDraft, RequestID: r.ID, ExpectedContentVersion: &version, Payload: payload}
	_, err = f.db.ExecuteCommand(context.Background(), f.token, f.csrf, command, f.e.cfg, nil)
	var domain *model.ProgramError
	if !errors.As(err, &domain) || domain.Code != "input_limit" {
		t.Fatalf("exhausted lifetime accepted an explicit new draft grant: %v", err)
	}
	ticks(t, f.e, 5)
	r = f.request(t, r.ID)
	if r.ContentVersion != 2 || r.LifetimeDispatches != 1 || r.MaxDraftDispatches != 1 || r.ActiveStepID != "" || len(f.backend.requests) != 1 {
		t.Fatalf("answer/restart replenished lifetime authority: %+v", r)
	}
	questions, err = f.db.IntakeQuestions(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if questions[0].AnsweredBySequence == 0 {
		t.Fatal("exact bound answer was not recorded")
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineRestartRepairsSavedSessionBeforeStepBinding(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.idle = true
	f.backend.output, _ = intakeDraftOutput(t, model.IntakeRouteFeature)
	r := f.create(t, "Publish the original worker after Save committed but Bind did not.")
	a := f.running(t, r.ID)
	step, err := f.db.IntakeStepForFeature(context.Background(), a.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if a.ThreadID == "" || step.ThreadID != a.ThreadID {
		t.Fatalf("fixture did not reach bound running ownership: attempt=%+v step=%+v", a, step)
	}
	// Reconstruct only the legitimate Save-before-Bind crash window: the
	// ordinary attempt/session ledger is durable, but the step has no thread yet.
	step.ThreadID = ""
	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(f.path)}).String() + "?mode=rw"
	ledger, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ledger.ExecContext(context.Background(), `UPDATE intake_steps SET data=? WHERE id=? AND active=1`, raw, step.ID)
	if err != nil {
		ledger.Close()
		t.Fatal(err)
	}
	rows, err := result.RowsAffected()
	if closeErr := ledger.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || rows != 1 {
		t.Fatalf("did not reconstruct exact crash window: rows=%d err=%v", rows, err)
	}
	f.restart(t)
	persisted := f.active(t)
	unbound, err := f.db.GetIntakeStep(context.Background(), step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || !reflect.DeepEqual(persisted[0], a) || unbound.ThreadID != "" {
		t.Fatalf("restart lost the saved-attempt/unbound-step window: attempts=%+v step=%+v", persisted, unbound)
	}
	opStart := len(f.backend.operations)
	f.backend.idle = false
	ticks(t, f.e, 1)
	r = f.request(t, r.ID)
	repaired, err := f.db.GetIntakeStep(context.Background(), step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != model.IntakeReviewReady || r.ActiveStepID != "" || r.LifetimeDispatches != 1 || len(f.active(t)) != 0 || len(f.backend.requests) != 1 {
		t.Fatalf("saved session was rejected or replayed instead of published: request=%+v operations=%v", r, f.backend.operations)
	}
	draft, err := f.db.IntakeDraft(context.Background(), r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.AttemptID != a.ID || repaired.ThreadID != a.ThreadID || repaired.OutputSHA == "" || draft.StepID != step.ID || draft.OutputSHA != repaired.OutputSHA || f.backend.requests[0].AttemptID != a.DispatchKey {
		t.Fatalf("repair replaced original producer identity: attempt=%+v step=%+v draft=%+v", a, repaired, draft)
	}
	if !reflect.DeepEqual(f.backend.operations[opStart:], []string{"Poll:" + a.ThreadID}) {
		t.Fatalf("known saved session was found, stopped, or respawned: %v", f.backend.operations[opStart:])
	}
	status, err := f.db.Status(context.Background(), model.StatusQuery{}, f.e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status.Calls.Used != 1 || status.Limits.Global.Active != 0 {
		t.Fatalf("binding repair changed the original reserved budget: %+v", status)
	}
	f.noCodeAuthority(t)
}

func TestIntakeEnginePolicyChangeReopensUncertainPrivateOwnershipBeforeFreshDraft(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.uncertainSpawn = true
	f.backend.idle = true
	r := f.create(t, "Settle the original private worker across a controller policy change.")
	f.until(t, "uncertain dispatch before configuration change", func() bool { return len(f.backend.requests) == 1 })
	a := f.active(t)[0]
	step, err := f.db.IntakeStepForFeature(context.Background(), a.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Stage != "intake_dispatching" || a.ThreadID != "" || step.ThreadID != "" || len(f.backend.projectRoots) != 1 {
		t.Fatalf("fixture lacks uncertain private ownership: attempt=%+v step=%+v roots=%v", a, step, f.backend.projectRoots)
	}
	original := f.repos.workspaces[len(f.repos.workspaces)-1]
	root := f.backend.projectRoots[0]
	thread := f.backend.sessions[a.DispatchKey].ThreadID
	prepares := len(f.repos.prepared)
	reopens := len(f.repos.reopened)
	opStart := len(f.backend.operations)
	project := f.e.cfg.Projects["demo"]
	project.Checks = map[string]model.Command{"unit": {Argv: []string{"must-not-execute-new-check"}}}
	project.MaxConcurrent = 2
	project.ProtectedPaths = []string{"new-private/**"}
	project.Releases = map[string]model.Release{"staging": {Allowed: true, Qualified: true, AllowedPaths: []string{"new-path/**"}, Deploy: model.Command{Argv: []string{"must-not-deploy"}}}}
	f.e.cfg.Projects["demo"] = project
	f.e.cfg.PolicyHash = intake.SHA256([]byte("changed-intake-test-policy"))
	f.restart(t)
	f.backend.stopPending = true
	ticks(t, f.e, 1)
	active := f.active(t)
	r = f.request(t, r.ID)
	if len(active) != 1 || active[0].ID != a.ID || active[0].DispatchKey != a.DispatchKey || active[0].ThreadID != thread || active[0].Stage != "intake_stopping" || active[0].Workspace != original.Path || active[0].BaseSHA != original.BaseSHA || r.ActiveStepID != step.ID || r.LifetimeDispatches != 1 {
		t.Fatalf("changed enrollment abandoned or replaced original ownership: request=%+v attempts=%+v", r, active)
	}
	if len(f.repos.prepared) != prepares || !reflect.DeepEqual(f.repos.reopened[reopens:], []string{original.Path}) || !reflect.DeepEqual(f.backend.projectRoots, []string{root, root}) {
		t.Fatalf("reconciliation used current enrollment instead of private identity: prepares=%d reopened=%v roots=%v", len(f.repos.prepared)-prepares, f.repos.reopened[reopens:], f.backend.projectRoots)
	}
	wantOps := []string{"Find:" + a.DispatchKey, "Stop:" + thread, "Poll:" + thread}
	if !reflect.DeepEqual(f.backend.operations[opStart:], wantOps) || len(f.backend.requests) != 1 {
		t.Fatalf("changed policy did not reconcile Find/Stop/Poll without replay: %v", f.backend.operations[opStart:])
	}
	f.backend.stopPending = false
	ticks(t, f.e, 1)
	r = f.request(t, r.ID)
	if r.State != model.IntakeBlocked || r.ActiveStepID != "" || r.LatestDraftRevision != 0 || r.LifetimeDispatches != 1 || len(f.active(t)) != 0 || len(f.backend.requests) != 1 || len(f.repos.prepared) != prepares {
		t.Fatalf("old policy worker did not settle without new work: request=%+v operations=%v", r, f.backend.operations)
	}
	status, err := f.db.Status(context.Background(), model.StatusQuery{}, f.e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status.Calls.Used != 1 || status.Limits.Global.Active != 0 {
		t.Fatalf("old ownership settlement replenished the model budget: %+v", status)
	}
	// Leave every old source/workspace/receipt in place. New consent must use the
	// changed policy, not require deletion of the previous enrollment's files.
	f.backend.uncertainSpawn = false
	f.backend.idle = false
	f.backend.output, _ = intakeDraftOutput(t, model.IntakeRouteFeature)
	r = f.append(t, r.ID, "Save current requirements under the newly disclosed policy.", nil)
	f.draft(t, r.ID)
	f.until(t, "fresh explicit draft under changed enrollment", func() bool { return f.request(t, r.ID).State == model.IntakeReviewReady })
	r = f.request(t, r.ID)
	if r.LifetimeDispatches != 2 || len(f.backend.requests) != 2 || len(f.repos.prepared) <= prepares || !reflect.DeepEqual(f.repos.prepared[len(f.repos.prepared)-1], project) {
		t.Fatalf("fresh consent failed to use current enrollment: request=%+v prepared=%+v", r, f.repos.prepared)
	}
	draft, err := f.db.IntakeDraft(context.Background(), r.ID, r.LatestDraftRevision)
	if err != nil {
		t.Fatal(err)
	}
	if draft.PolicySHA != f.e.cfg.PolicyHash || draft.StepID == step.ID || f.backend.requests[1].Workspace == original.Path {
		t.Fatalf("fresh draft reused old authority: draft=%+v requests=%+v", draft, f.backend.requests)
	}
	if info, err := os.Stat(original.Path); err != nil || !info.IsDir() {
		t.Fatalf("old private workspace was removed to make new consent work: %v", err)
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineRestartFindsUncertainSpawnWithoutReplay(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.uncertainSpawn = true
	f.backend.idle = true
	r := f.create(t, "Draft with restart-safe ownership.")
	f.until(t, "uncertain persisted dispatch", func() bool { return len(f.backend.requests) == 1 })
	active := f.active(t)
	if len(active) != 1 || active[0].Stage != "intake_dispatching" || active[0].ThreadID != "" {
		t.Fatalf("lost response discarded durable dispatch intent: %+v", active)
	}
	key := active[0].DispatchKey
	f.restart(t)
	f.backend.missingFind = true
	ticks(t, f.e, 2)
	if len(f.active(t)) != 1 || len(f.backend.requests) != 1 {
		t.Fatal("unknown ownership released capacity or replayed Spawn")
	}
	f.backend.missingFind = false
	a := f.running(t, r.ID)
	if a.DispatchKey != key || a.ThreadID != f.backend.sessions[key].ThreadID || len(f.backend.requests) != 1 || f.request(t, r.ID).LifetimeDispatches != 1 {
		t.Fatalf("restart did not recover original worker: %+v", a)
	}
	found := false
	for _, op := range f.backend.operations {
		if op == "Find:"+key {
			found = true
		}
	}
	if !found {
		t.Fatal("restart never reconciled exact persisted dispatch with Find")
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineRestartsPersistedUncertainStageThroughFind(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.uncertainSpawn = true
	f.backend.idle = true
	r := f.create(t, "Recover an explicitly uncertain persisted intake attempt without replay.")
	f.until(t, "durable lost-response dispatch", func() bool { return len(f.backend.requests) == 1 })
	a := f.active(t)[0]
	key := a.DispatchKey
	a.Stage = "uncertain"
	a.State = "blocked"
	if err := f.db.Save(context.Background(), a, "blocked", "external ownership uncertain", "", true); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.backend.missingFind = true
	ticks(t, f.e, 1)
	active := f.active(t)
	if len(active) != 1 || active[0].Stage != "uncertain" || len(f.backend.requests) != 1 || f.request(t, r.ID).LifetimeDispatches != 1 {
		t.Fatalf("missing Find released or replayed explicit uncertain ownership: %+v", active)
	}
	f.backend.missingFind = false
	recovered := f.running(t, r.ID)
	if recovered.ID != a.ID || recovered.DispatchKey != key || recovered.ThreadID != f.backend.sessions[key].ThreadID || len(f.backend.requests) != 1 || f.request(t, r.ID).LifetimeDispatches != 1 {
		t.Fatalf("explicit uncertain restart did not recover its original exact worker: %+v", recovered)
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineRestartContinuesIrreversibleAsynchronousStop(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.output, _ = intakeDraftOutput(t, model.IntakeRouteFeature)
	r := f.create(t, "Never publish a completed result after the controller has committed Stop.")
	a := f.running(t, r.ID)
	// Trigger a stop while the backend still reports running after Stop.
	// The original trigger is removed after restart: only the persisted stop
	// intent, not another preflight failure, can keep this result nonpublishable.
	f.e.cfg.Intake.MaxOutputBytes = 1
	f.backend.stopPending = true
	ticks(t, f.e, 1)
	active := f.active(t)
	if len(active) != 1 || active[0].ID != a.ID || active[0].Stage != "intake_stopping" || active[0].Wait == nil || active[0].Wait.Code != "intake_stop_pending" {
		t.Fatalf("asynchronous stop was not durably retained before release: %+v", active)
	}
	f.restart(t)
	f.e.cfg.Intake.MaxOutputBytes = 256 << 10
	f.backend.stopPending = false
	f.backend.stopCompletes = true
	ticks(t, f.e, 1)
	r = f.request(t, r.ID)
	if len(f.active(t)) != 0 || r.ActiveStepID != "" || r.State != model.IntakeBlocked || r.LatestDraftRevision != 0 || len(f.backend.requests) != 1 {
		t.Fatalf("restart published terminal output after an irreversible stop: %+v", r)
	}
	stops := 0
	for _, op := range f.backend.operations {
		if op == "Stop:"+a.ThreadID {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("restart did not continue Stop before terminal Poll: %v", f.backend.operations)
	}
	ops := f.backend.operations
	if len(ops) < 2 || !reflect.DeepEqual(ops[len(ops)-2:], []string{"Stop:" + a.ThreadID, "Poll:" + a.ThreadID}) {
		t.Fatalf("restart skipped ordered stop reconciliation: %v", ops)
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineMissingFindRequiresTrustedFailureReceiptBeforeRelease(t *testing.T) {
	for _, mode := range []string{"uncertain", "cancellation", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newIntakeEngineFixture(t, nil)
			f.backend.uncertainSpawn = true
			f.backend.missingFind = true
			r := f.create(t, "Missing Find is not proof that an external worker does not exist.")
			f.until(t, "durable lost-response dispatch", func() bool { return len(f.backend.requests) == 1 })
			a := f.active(t)[0]
			if mode == "cancellation" {
				r = f.request(t, r.ID)
				f.command(t, model.ActionIntakeCancel, &r, model.EmptyCommandPayload{})
			}
			if mode == "deadline" {
				a.Deadline = time.Now().Add(-time.Second)
				if err := f.db.Save(context.Background(), a, "running", "", "", true); err != nil {
					t.Fatal(err)
				}
			}
			ticks(t, f.e, 3)
			active := f.active(t)
			wantStage := "uncertain"
			if mode != "uncertain" {
				wantStage = "intake_stopping"
			}
			if len(active) != 1 || active[0].ID != a.ID || active[0].Stage != wantStage || f.request(t, r.ID).ActiveStepID == "" {
				t.Fatalf("missing Find released or hid unsettled external ownership: %+v", active)
			}
			feature := state(t, f.db, a.FeatureID)
			if feature.State != "blocked" && !(feature.State == "cancelling" && active[0].State == "blocked") {
				t.Fatalf("missing Find is not eligible for explicit trusted reconciliation: feature=%+v attempt=%+v", feature, active[0])
			}
			if len(f.backend.requests) != 1 || f.request(t, r.ID).LifetimeDispatches != 1 {
				t.Fatal("missing Find replayed a model dispatch")
			}
			f.noCodeAuthority(t)
			if err := f.db.ResolveFailed(context.Background(), a.FeatureID, a.ID, ""); err == nil {
				t.Fatal("empty trusted receipt released uncertain ownership")
			}
			if len(f.active(t)) != 1 {
				t.Fatal("rejected empty receipt changed ownership")
			}
			if err := f.db.ResolveFailed(context.Background(), a.FeatureID, a.ID, "Deterministic unit fixture: trusted operator attests the exact external worker has settled."); err != nil {
				t.Fatal(err)
			}
			if len(f.active(t)) != 0 {
				t.Fatal("committed trusted failure receipt did not release ordinary capacity")
			}
			ticks(t, f.e, 2)
			r = f.request(t, r.ID)
			if r.ActiveStepID != "" || r.LatestDraftRevision != 0 || r.LifetimeDispatches != 1 || len(f.backend.requests) != 1 {
				t.Fatalf("trusted failure receipt left ownership, published output, or replenished dispatch authority: %+v", r)
			}
			evidence, err := f.db.Evidence(context.Background(), a.FeatureID)
			if err != nil {
				t.Fatal(err)
			}
			if len(evidence) != 1 || evidence[0].Kind != "operator_reconciled_failure" || evidence[0].Passed || evidence[0].CandidateSHA != "" {
				t.Fatalf("trusted failure receipt fabricated positive code evidence: %+v", evidence)
			}
			status, err := f.db.Status(context.Background(), model.StatusQuery{}, f.e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if status.Limits.Global.Active != 0 || status.Calls.Used != 1 {
				t.Fatalf("trusted resolution reset reserved calls or retained capacity: %+v", status)
			}
			if len(f.repos.forbidden) != 0 {
				t.Fatalf("trusted failure receipt invoked product authority: %v", f.repos.forbidden)
			}
		})
	}
}

func TestIntakeEngineCancellationStopsAndPollsBeforeReleasingCapacity(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("uncertain-spawn-%t", uncertain), func(t *testing.T) {
			f := newIntakeEngineFixture(t, nil)
			f.backend.idle = true
			f.backend.uncertainSpawn = uncertain
			r := f.create(t, "Cancelable bounded intake.")
			f.until(t, "owned worker", func() bool { return len(f.backend.requests) == 1 })
			a := f.active(t)[0]
			thread := f.backend.sessions[a.DispatchKey].ThreadID
			r = f.request(t, r.ID)
			f.command(t, model.ActionIntakeCancel, &r, model.EmptyCommandPayload{})
			f.backend.stopPending = true
			ticks(t, f.e, 1)
			r = f.request(t, r.ID)
			if r.State != model.IntakeCancelling || r.ActiveStepID == "" || len(f.active(t)) != 1 {
				t.Fatalf("nonterminal Stop/Poll released ownership: %+v", r)
			}
			ops := f.backend.operations
			if len(ops) < 2 || ops[len(ops)-2] != "Stop:"+thread || ops[len(ops)-1] != "Poll:"+thread {
				t.Fatalf("cancellation omitted ordered Stop then Poll: %v", ops)
			}
			f.backend.stopPending = false
			ticks(t, f.e, 1)
			r = f.request(t, r.ID)
			if r.State != model.IntakeCancelled || r.ActiveStepID != "" || len(f.active(t)) != 0 || len(f.backend.requests) != 1 {
				t.Fatalf("terminal cancellation not settled: %+v", r)
			}
			f.noCodeAuthority(t)
		})
	}
}

func TestIntakeEngineSupersededOutputIsHistoricalNeverCurrent(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	oldOutput, oldSource := intakeDraftOutput(t, model.IntakeRouteFeature)
	f.backend.output = oldOutput
	r := f.create(t, "Original version requests an integrated feature.")
	a := f.running(t, r.ID)
	oldStep, err := f.db.IntakeStepForFeature(context.Background(), a.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a real content-write race after engine preflight but before the
	// terminal output transaction; the Store, not an earlier UI check, must gate it.
	f.backend.onPoll = func() { f.append(t, r.ID, "Changed scope: ask before selecting persistence.", nil) }
	ticks(t, f.e, 1)
	r = f.request(t, r.ID)
	if r.ContentVersion != 2 || r.LatestDraftRevision != 0 || r.State == model.IntakeReviewReady {
		t.Fatalf("superseded output became current: %+v", r)
	}
	history, err := f.db.IntakeDrafts(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ContentVersion != 1 || history[0].CanonicalSource != oldSource || history[0].StepID != oldStep.ID {
		t.Fatalf("old exact output lost historical provenance: %+v", history)
	}
	oldStep, err = f.db.GetIntakeStep(context.Background(), oldStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !oldStep.Superseded || oldStep.State != "superseded" || oldStep.OutputSHA != history[0].OutputSHA {
		t.Fatalf("historical step incorrectly current: %+v", oldStep)
	}
	f.backend.output = intakeQuestionsOutput()
	f.draft(t, r.ID)
	f.until(t, "current version questions", func() bool { return f.request(t, r.ID).State == model.IntakeNeedsAnswers })
	if f.request(t, r.ID).LatestDraftRevision != 0 {
		t.Fatal("new version inherited historical draft authority")
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineRevocationPreservesCommittedDraftButDeniesNewCommands(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.idle = true
	f.backend.output, _ = intakeDraftOutput(t, model.IntakeRouteFeature)
	r := f.create(t, "The explicit bounded grant survives revocation, but grants no future commands.")
	a := f.running(t, r.ID)
	if err := f.db.RevokeOperator(context.Background(), r.OperatorID); err != nil {
		t.Fatal(err)
	}
	ticks(t, f.e, 1)
	if len(f.active(t)) != 1 || f.request(t, r.ID).ActiveStepID == "" {
		t.Fatal("revocation discarded a committed bounded draft grant")
	}
	for _, op := range f.backend.operations {
		if op == "Stop:"+a.ThreadID {
			t.Fatal("revocation retroactively stopped a committed bounded draft")
		}
	}
	f.backend.idle = false
	f.until(t, "committed draft settles after revocation", func() bool { return f.request(t, r.ID).State == model.IntakeReviewReady })
	r = f.request(t, r.ID)
	if len(f.active(t)) != 0 || r.ActiveStepID != "" || r.LatestDraftRevision != 1 || len(f.backend.requests) != 1 {
		t.Fatalf("committed draft did not settle exactly once: %+v", r)
	}
	payload, err := json.Marshal(model.TurnAppendPayload{ClientMessageID: f.uuid(), Text: "Revoked operator cannot change saved content."})
	if err != nil {
		t.Fatal(err)
	}
	version := r.ContentVersion
	command := model.TypedCommand{CommandID: f.uuid(), Action: model.ActionTurnAppend, RequestID: r.ID, ExpectedContentVersion: &version, Payload: payload}
	if _, err = f.db.ExecuteCommand(context.Background(), f.token, f.csrf, command, f.e.cfg, nil); err == nil {
		t.Fatal("revoked session accepted a future command")
	}
	ticks(t, f.e, 3)
	if len(f.backend.requests) != 1 || f.request(t, r.ID).ContentVersion != version {
		t.Fatal("revocation allowed new content or another dispatch")
	}
	f.noCodeAuthority(t)
}

func TestIntakeEngineRejectsReadOnlyWorkspaceMutation(t *testing.T) {
	f := newIntakeEngineFixture(t, nil)
	f.backend.output, _ = intakeDraftOutput(t, model.IntakeRouteFeature)
	f.backend.mutate = true
	r := f.create(t, "Draft only; modifying the workspace is forbidden.")
	a := f.running(t, r.ID)
	f.until(t, "mutation rejected", func() bool { return f.request(t, r.ID).State == model.IntakeBlocked })
	drafts, err := f.db.IntakeDrafts(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	feature := state(t, f.db, a.FeatureID)
	if len(drafts) != 0 || f.request(t, r.ID).LatestDraftRevision != 0 || !strings.Contains(feature.Reason, "immutable workspace") {
		t.Fatalf("mutating worker published a draft: feature=%+v drafts=%+v", feature, drafts)
	}
	f.noCodeAuthority(t)
}

func TestPlanningQuestionAnswerCreatesUnapprovedRevisionWithExactOutputLineage(t *testing.T) {
	e, db, backend, repos := fixture(t)
	ctx := context.Background()
	plan, err := intake.NewPlan("clarify-delivery", "demo", "Plan delivery", "Plan bounded duplicate and reordered delivery behavior.")
	if err != nil {
		t.Fatal(err)
	}
	child := contract("must-not-be-approved")
	payload, err := json.Marshal(planResult{Question: "Must acknowledgement survive process loss?", Features: []model.Feature{child}})
	if err != nil {
		t.Fatal(err)
	}
	backend.plan = string(payload)
	enqueue(t, db, plan)
	ticks(t, e, 8)
	questions, err := db.PlanningQuestions(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 {
		t.Fatalf("planning question was not persisted: %+v", questions)
	}
	q := questions[0]
	before := state(t, db, plan.ID)
	if before.State != "blocked" || q.FeatureRevision != before.Revision || q.AttemptID == "" || q.OutputSHA != intake.SHA256(payload) || q.Question != "Must acknowledgement survive process loss?" {
		t.Fatalf("planning question lost exact terminal output binding: feature=%+v question=%+v", before, q)
	}
	active, err := db.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("question retained settled planning worker ownership: %+v", active)
	}
	for _, mutate := range []struct {
		name   string
		change func(*model.PlanningQuestion)
	}{
		{"revision", func(q *model.PlanningQuestion) { q.FeatureRevision = intake.SHA256([]byte("not-current")) }},
		{"attempt", func(q *model.PlanningQuestion) { q.AttemptID = "not-the-producer" }},
		{"output", func(q *model.PlanningQuestion) { q.OutputSHA = intake.SHA256([]byte("not-the-output")) }},
		{"question", func(q *model.PlanningQuestion) { q.Question = "Substituted consequential question" }},
	} {
		t.Run("reject-stale-"+mutate.name, func(t *testing.T) {
			stale := q
			mutate.change(&stale)
			if _, err := db.AnswerPlanningQuestion(ctx, model.PlanningAnswer{Question: stale, Text: "Yes, persist acknowledgements.", Audit: model.TrustedLocalAudit()}); err == nil {
				t.Fatal("planning answer accepted substituted binding")
			}
			if state(t, db, plan.ID).Revision != before.Revision {
				t.Fatal("rejected stale answer changed the plan")
			}
		})
	}
	answer := model.PlanningAnswer{Question: q, Text: "Yes, acknowledgement must survive process loss.", Audit: model.TrustedLocalAudit()}
	revised, err := db.AnswerPlanningQuestion(ctx, answer)
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision == before.Revision || revised.PolicyHash != "" || revised.State != "proposed" {
		t.Fatalf("answer did not create a fresh unapproved planning revision: %+v", revised)
	}
	for _, text := range []string{plan.Body, q.Question, answer.Text, q.FeatureRevision, q.AttemptID, q.OutputSHA, q.ID} {
		if !strings.Contains(revised.Body, text) {
			t.Fatalf("amended plan lost original goal/answer lineage %q: %s", text, revised.Body)
		}
	}
	if _, err := db.AnswerPlanningQuestion(ctx, answer); err == nil {
		t.Fatal("old question replay applied to amended current revision")
	}
	ticks(t, e, 4)
	features, err := db.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(features) != 1 || features[0].ID != plan.ID || len(backend.requests) != 1 || repos.checks != 0 || repos.commits != 0 || repos.integrations != 0 || repos.prs != 0 || repos.releases != 0 {
		t.Fatalf("question/answer inherited execution or child approval: features=%+v calls=%d repos=%+v", features, len(backend.requests), repos)
	}
	history, err := db.PlanningQuestions(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !reflect.DeepEqual(history[0], q) {
		t.Fatalf("answer rewrote historical question: %+v", history)
	}
}
