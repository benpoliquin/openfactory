// Package engine implements the deterministic, bounded OpenFactory scheduler.
package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"openfactory/internal/executor"
	"openfactory/internal/intake"
	"openfactory/internal/model"
	"openfactory/internal/policy"
	"openfactory/internal/store"
)

type Engine struct {
	cfg         model.Config
	db          *store.Store
	backend     model.Backend
	repos       model.Repositories
	experiments model.ExperimentRunner

	cycleMu sync.Mutex
	workers map[string]context.CancelFunc
	running bool
}

func New(cfg model.Config, db *store.Store, backend model.Backend, repos model.Repositories) *Engine {
	e := &Engine{cfg: cfg, db: db, backend: backend, repos: repos}
	if cfg.CheckSocket != "" {
		e.experiments = executor.NewClient(cfg.CheckSocket)
	}
	return e
}

// Tick advances each idle attempt once, synchronously, then admits queued work.
// Attempts currently owned by Run are skipped rather than advanced twice.
func (e *Engine) Tick(ctx context.Context) error {
	return e.cycle(ctx, nil)
}

func (e *Engine) admit(ctx context.Context, paused bool, active []model.Attempt) error {
	features, err := e.db.List(ctx)
	if err != nil {
		return err
	}
	// Deterministic priority/age ordering; admission and delivery have separate caps.
	sort.Slice(features, func(i, j int) bool {
		if features[i].Priority != features[j].Priority {
			return features[i].Priority > features[j].Priority
		}
		if !features[i].CreatedAt.Equal(features[j].CreatedAt) {
			return features[i].CreatedAt.Before(features[j].CreatedAt)
		}
		return features[i].ID < features[j].ID
	})
	for _, f := range features {
		if f.State == "cancelling" {
			found := false
			for _, a := range active {
				if a.FeatureID == f.ID {
					found = true
				}
			}
			if !found {
				if err = e.db.SettleCancellation(ctx, f.ID); err != nil {
					return err
				}
			}
			continue
		}
		if paused || f.State != "ready" {
			continue
		}
		if err = e.validate(f); err != nil {
			if err = e.db.Block(ctx, f.ID, err.Error()); err != nil {
				return err
			}
			continue
		}
		if f.Attempts >= e.cfg.MaxAttempts {
			if err = e.db.Block(ctx, f.ID, "attempt allowance exhausted"); err != nil {
				return err
			}
			continue
		}
		projectLimit := e.cfg.Projects[f.Project].MaxConcurrent
		if projectLimit == 0 {
			projectLimit = 1
		}
		_, err = e.db.Claim(ctx, f.ID, e.cfg.PolicyHash, e.cfg.MaxAttempts, e.cfg.MaxConcurrent, projectLimit, e.cfg.AttemptTimeoutSeconds)
		// Dependency gates and capacity are ordinary queue conditions, not errors or model work.
		if err != nil {
			if !errors.Is(err, store.ErrWaiting) {
				return err
			}
			continue
		}
	}
	return nil
}
func (e *Engine) validate(f model.Feature) error {
	if err := intake.Validate(f); err != nil {
		return err
	}
	if f.Revision != intake.Revision(f) {
		return errors.New("source revision does not match immutable contract")
	}
	if e.cfg.PolicyHash == "" || f.PolicyHash != e.cfg.PolicyHash {
		return errors.New("policy changed; exact revision requires fresh approval")
	}
	p, ok := e.cfg.Projects[f.Project]
	if !ok || !p.Trusted {
		return errors.New("project is not enrolled and trusted")
	}
	for _, name := range f.Checks {
		c, ok := p.Checks[name]
		if !ok || len(c.Argv) == 0 {
			return fmt.Errorf("check %s has no approved command", name)
		}
	}
	if f.CompletionTarget == "pull_request" && p.GitHubRepository == "" {
		return errors.New("pull request target requires enrolled GitHub repository")
	}
	if f.CompletionTarget == "staging" || f.CompletionTarget == "production" {
		r, ok := p.Releases[f.CompletionTarget]
		if !ok || !r.Allowed || !r.Qualified || !r.ExpiresAt.After(time.Now()) || !contains(r.AllowedRisks, f.Risk) || len(r.AllowedPaths) == 0 || len(r.Deploy.Argv) == 0 || len(r.Observe.Argv) == 0 || len(r.Rollback.Argv) == 0 || r.MinimumObservations < 1 || r.ObservationSeconds < 1 {
			return errors.New("release target lacks current qualified risk/path authorization and observation/rollback hooks")
		}
	}
	return nil
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func (e *Engine) save(ctx context.Context, a model.Attempt) error {
	a.Wait = nil
	return e.db.Save(ctx, a, "running", "", "", true)
}
func (e *Engine) block(ctx context.Context, a model.Attempt, reason string, uncertain bool, evidence ...model.Evidence) error {
	a.Wait = nil
	a.State = "blocked"
	if uncertain {
		a.Stage = "uncertain"
	} else {
		a.Stage = "failed"
	}
	return e.db.Save(ctx, a, "blocked", reason, "", uncertain, evidence...)
}
func (e *Engine) advance(ctx context.Context, a model.Attempt, paused bool) error {
	f, err := e.db.Get(ctx, a.FeatureID)
	if err != nil {
		return err
	}
	intakeStep, intakeErr := e.db.IntakeStepForFeature(ctx, f.ID)
	if intakeErr == nil {
		if f.IntakeStepID != intakeStep.ID {
			return e.halt(ctx, a, "intake feature association does not match persisted step")
		}
		return e.advanceIntake(ctx, a, f, intakeStep, paused)
	}
	if f.IntakeStepID != "" {
		return e.halt(ctx, a, "intake feature has no valid server association")
	}
	if !errors.Is(intakeErr, sql.ErrNoRows) {
		return intakeErr
	}
	step, stepErr := e.db.ProgramStepForFeature(ctx, f.ID)
	if stepErr == nil {
		if f.DiscoveryStepID != step.ID {
			return e.halt(ctx, a, "discovery feature association does not match persisted step")
		}
		return e.advanceProgram(ctx, a, f, step, paused)
	}
	if f.DiscoveryStepID != "" {
		return e.halt(ctx, a, "discovery feature has no valid server association: "+stepErr.Error())
	}
	if !errors.Is(stepErr, sql.ErrNoRows) {
		return stepErr
	}
	p := e.cfg.Projects[f.Project]
	if a.Stage == "uncertain" {
		return nil
	}
	if f.State == "cancelling" {
		return e.cancel(ctx, a)
	}
	if strings.HasSuffix(a.Stage, "_dispatching") {
		if !time.Now().Before(a.Deadline) {
			return e.block(ctx, a, "attempt deadline expired with an unreconciled spawn intent", true)
		}
		reconcilectx, cancel := context.WithDeadline(ctx, a.Deadline)
		defer cancel()
		return e.reconcile(reconcilectx, a, f)
	}
	if a.Stage == "committing" || a.Stage == "integrating" || a.Stage == "opening_pr" || a.Stage == "releasing" {
		return e.block(ctx, a, "restart found an uncertain "+a.Stage+" operation; reconcile externally before further execution", true)
	}
	if err = e.validate(f); err != nil {
		return e.halt(ctx, a, err.Error())
	}
	if time.Now().After(a.Deadline) {
		return e.halt(ctx, a, "attempt deadline expired")
	}
	workctx, cancel := context.WithDeadline(ctx, a.Deadline)
	defer cancel()
	if strings.HasSuffix(a.Stage, "_running") {
		return e.poll(workctx, a, f)
	}
	if paused {
		return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: "paused", Detail: "OpenFactory paused"})
	}
	if a.Wait != nil {
		if err := e.db.SetWait(ctx, a.ID, nil); err != nil {
			return err
		}
		a.Wait = nil
	}
	switch a.Stage {
	case "prepare":
		w, err := e.repos.Prepare(workctx, f.Project, p, a.ID)
		if err != nil {
			return e.block(ctx, a, "prepare: "+err.Error(), false)
		}
		a.Workspace = w.Path
		a.BaseSHA = w.BaseSHA
		if w.Path == "" || w.BaseSHA == "" {
			return e.block(ctx, a, "repository preparation returned no workspace/baseline", false)
		}
		a.Stage = "build_pending"
		if e.cfg.FastModel != "" {
			a.Stage = "scout_pending"
		}
		if f.Kind == "plan" {
			a.Stage = "plan_pending"
		}
		return e.save(ctx, a)
	case "scout_pending", "build_pending", "review_pending", "plan_pending":
		return e.dispatch(workctx, a, f)
	case "checks":
		return e.check(workctx, a, f)
	case "reviewed":
		return e.complete(workctx, a, f)
	case "integration_queued":
		return e.acquireIntegration(workctx, a, f)
	case "reconciliation_pending", "reconciling":
		return e.reconcileCandidate(workctx, a, f)
	case "integrated":
		return e.release(workctx, a, f)
	default:
		return e.block(ctx, a, "unknown persisted stage "+a.Stage, true)
	}
}
func (e *Engine) cancel(ctx context.Context, a model.Attempt) error {
	if strings.HasSuffix(a.Stage, "_dispatching") {
		f, err := e.db.Get(ctx, a.FeatureID)
		if err != nil {
			return err
		}
		return e.reconcile(ctx, a, f)
	}
	if a.Stage == "integrating" || a.Stage == "opening_pr" || a.Stage == "releasing" || a.Stage == "committing" {
		return e.block(ctx, a, "cancellation requires reconciliation of uncertain "+a.Stage, true)
	}
	if a.ThreadID != "" {
		if err := e.backend.Stop(ctx, a.ThreadID); err != nil {
			return nil
		}
		o, err := e.backend.Poll(ctx, a.ThreadID, a.Cursor)
		if err != nil || !terminal(o.State) {
			return nil
		}
	}
	a.State = "cancelled"
	a.Stage = "cancelled"
	return e.db.Save(ctx, a, "cancelled", "operator cancellation settled", "", false)
}
func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "interrupted"
}
func (e *Engine) halt(ctx context.Context, a model.Attempt, reason string) error {
	if a.ThreadID != "" {
		if err := e.backend.Stop(ctx, a.ThreadID); err != nil {
			return e.db.Save(ctx, a, "blocked", reason+"; cancellation pending", "", true)
		}
		o, err := e.backend.Poll(ctx, a.ThreadID, a.Cursor)
		if err != nil || !terminal(o.State) {
			return e.db.Save(ctx, a, "blocked", reason+"; cancellation pending", "", true)
		}
	}
	return e.block(ctx, a, reason, false)
}
func (e *Engine) dispatch(ctx context.Context, a model.Attempt, f model.Feature) error {
	stage := strings.TrimSuffix(a.Stage, "_pending")
	prompt, err := e.prompt(ctx, a, f, stage)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	project, err := e.backend.EnsureProject(ctx, f.Project, filepath.Dir(a.Workspace))
	if err != nil {
		var classified interface{ Uncertain() bool }
		if errors.As(err, &classified) && classified.Uncertain() {
			return e.wait(ctx, a, "bb_project_reconciling", err)
		}
		return e.block(ctx, a, "BB project: "+err.Error(), false)
	}
	if stage == "review" || stage == "plan" || stage == "scout" {
		fp, err := e.repos.Fingerprint(ctx, a.Workspace)
		if err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		if stage == "review" && fp != a.Fingerprint {
			return e.block(ctx, a, "candidate changed before independent review", false)
		}
		a.Fingerprint = fp
		if err = e.save(ctx, a); err != nil {
			return err
		}
	}
	a.EnvironmentID = ""
	a.DispatchKey = fmt.Sprintf("%s:%s:%d", a.ID, stage, a.CandidateGeneration)
	a.Wait = nil
	if err = e.db.ReserveCall(ctx, a, stage, a.DispatchKey, e.cfg.MaxCallsPerDay); err != nil {
		// A daily allowance or pause is a waiting condition; persisted attempt deadline still applies.
		if errors.Is(err, store.ErrWaiting) {
			return e.wait(ctx, a, "model_call_limit", err)
		}
		return err
	}
	a.Stage = stage + "_dispatching"
	a.ThreadID = ""
	a.Cursor = ""
	a.Summary = ""
	modelName := e.cfg.AstraModel
	if stage == "scout" {
		modelName = e.cfg.FastModel
	}
	session, err := e.backend.Spawn(ctx, model.SpawnRequest{AttemptID: a.DispatchKey, ProjectID: project, Workspace: a.Workspace, EnvironmentID: a.EnvironmentID, Model: modelName, Prompt: prompt})
	if err != nil {
		var classified interface{ Uncertain() bool }
		if errors.As(err, &classified) && !classified.Uncertain() {
			return e.block(ctx, a, "spawn rejected: "+err.Error(), false)
		}
		return nil
	} // Reconcile uncertain intents, never blindly respawn.
	if session.ThreadID == "" {
		return e.block(ctx, a, "spawn returned no durable thread identity", true)
	}
	if err = e.db.RecordSession(ctx, a.ID, stage, a.DispatchKey, session); err != nil {
		return err
	}
	a.ThreadID = session.ThreadID
	a.EnvironmentID = session.EnvironmentID
	a.Stage = stage + "_running"
	return e.save(ctx, a)
}
func (e *Engine) reconcile(ctx context.Context, a model.Attempt, f model.Feature) error {
	stage := strings.TrimSuffix(a.Stage, "_dispatching")
	project, err := e.backend.EnsureProject(ctx, f.Project, filepath.Dir(a.Workspace))
	if err != nil {
		if time.Now().After(a.Deadline) {
			return e.block(ctx, a, "spawn reconciliation project lookup exceeded deadline: "+err.Error(), true)
		}
		var classified interface{ Uncertain() bool }
		if errors.As(err, &classified) && classified.Uncertain() {
			return e.wait(ctx, a, "bb_project_reconciling", err)
		}
		// A definitive registration failure cannot establish the outcome of an
		// already reserved spawn. Keep that external intent fenced.
		return e.block(ctx, a, "spawn reconciliation project lookup failed: "+err.Error(), true)
	}
	if a.DispatchKey == "" {
		// Persisted pre-generation intents used this exact marker. Never rename
		// an uncertain external operation while discovering its session.
		a.DispatchKey = a.ID + ":" + stage
	}
	sessions, err := e.backend.Find(ctx, project, a.DispatchKey)
	if err != nil {
		if time.Now().After(a.Deadline) {
			return e.block(ctx, a, "spawn reconciliation exceeded deadline: "+err.Error(), true)
		}
		return e.wait(ctx, a, "bb_session_reconciling", err)
	}
	if len(sessions) != 1 || sessions[0].ThreadID == "" {
		return e.block(ctx, a, "spawn intent could not be reconciled to exactly one session", true)
	}
	if err = e.db.RecordSession(ctx, a.ID, stage, a.DispatchKey, sessions[0]); err != nil {
		return err
	}
	a.ThreadID = sessions[0].ThreadID
	a.EnvironmentID = sessions[0].EnvironmentID
	a.Stage = stage + "_running"
	return e.save(ctx, a)
}
func (e *Engine) poll(ctx context.Context, a model.Attempt, f model.Feature) error {
	o, err := e.backend.Poll(ctx, a.ThreadID, a.Cursor)
	if err != nil {
		return nil
	}
	a.Cursor = o.Cursor
	limit := e.cfg.MaxPromptBytes
	if limit < 1 {
		limit = 32768
	}
	if o.Output != "" {
		if len(a.Summary)+len(o.Output) > limit {
			return e.halt(ctx, a, "agent output exceeds configured evidence bound")
		}
		a.Summary += o.Output
	}
	if o.State == "running" {
		return e.save(ctx, a)
	}
	if o.State != "completed" {
		if !terminal(o.State) {
			return e.halt(ctx, a, "agent blocked or unknown: "+o.State+" "+o.Reason)
		}
		return e.block(ctx, a, "agent did not complete: "+o.State+" "+o.Reason, false)
	}
	stage := strings.TrimSuffix(a.Stage, "_running")
	if stage == "plan" {
		return e.finishPlan(ctx, a, f)
	}
	if stage == "scout" {
		fp, err := e.repos.Fingerprint(ctx, a.Workspace)
		if err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		if fp != a.Fingerprint {
			return e.block(ctx, a, "scout modified workspace", false)
		}
		a.Stage = "build_pending"
		a.ThreadID = ""
		return e.save(ctx, a)
	}
	if stage == "build" {
		var result buildResult
		if err = decodeResult(a.Summary, &result); err != nil {
			return e.block(ctx, a, "builder result: "+err.Error(), false)
		}
		if result.Question != "" {
			return e.block(ctx, a, "question: "+result.Question, false)
		}
		if strings.TrimSpace(result.Summary) == "" {
			return e.block(ctx, a, "builder omitted summary", false)
		}
		if err = acceptance(f, result.Acceptance); err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		a.Stage = "checks"
		a.ThreadID = ""
		return e.save(ctx, a)
	}
	if stage == "review" {
		fp, err := e.repos.Fingerprint(ctx, a.Workspace)
		if err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		if fp != a.Fingerprint {
			return e.block(ctx, a, "reviewer modified candidate workspace", false)
		}
		var result reviewResult
		if err = decodeResult(a.Summary, &result); err != nil {
			return e.block(ctx, a, "review result: "+err.Error(), false)
		}
		if !result.Approved || result.Findings == nil || len(result.Findings) > 0 || strings.TrimSpace(result.Summary) == "" {
			return e.repair(ctx, a, f, "review rejected candidate: "+result.Summary)
		}
		if err = acceptance(f, result.Acceptance); err != nil {
			return e.block(ctx, a, "review "+err.Error(), false)
		}
		a.Stage = "reviewed"
		a.ThreadID = ""
		return e.db.Save(ctx, a, "running", "independent review passed", "", true, model.Evidence{Kind: "review", CandidateSHA: a.CandidateSHA, CandidateGeneration: a.CandidateGeneration, Passed: true, Detail: a.Summary})
	}
	return e.block(ctx, a, "unknown agent stage", true)
}

type acceptanceResult struct {
	ID       string `json:"id"`
	Passed   bool   `json:"passed"`
	Evidence string `json:"evidence"`
}
type buildResult struct {
	Summary    string             `json:"summary"`
	Question   string             `json:"question,omitempty"`
	Acceptance []acceptanceResult `json:"acceptance"`
}
type reviewResult struct {
	Approved   bool               `json:"approved"`
	Summary    string             `json:"summary"`
	Findings   []json.RawMessage  `json:"findings"`
	Acceptance []acceptanceResult `json:"acceptance"`
}

func acceptance(f model.Feature, results []acceptanceResult) error {
	seen := map[string]bool{}
	for _, r := range results {
		if seen[r.ID] || !r.Passed || strings.TrimSpace(r.Evidence) == "" {
			return fmt.Errorf("acceptance %s lacks passing concrete evidence", r.ID)
		}
		seen[r.ID] = true
	}
	if len(seen) != len(f.Acceptance) {
		return errors.New("acceptance evidence coverage does not match approved criteria")
	}
	for _, a := range f.Acceptance {
		if !seen[a.ID] {
			return fmt.Errorf("acceptance %s has no evidence", a.ID)
		}
	}
	return nil
}
func decodeResult(text string, out any) error {
	text = strings.TrimSpace(text)
	if strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	}
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```json"))
	}
	// Validate syntax before decoding: failed partial decodes must never retain an earlier approved=true.
	data := []byte(text)
	for i := range len(data) {
		if data[i] == '{' && json.Valid(data[i:]) {
			return json.Unmarshal(data[i:], out)
		}
	}
	return errors.New("terminal output must end in a complete JSON result")
}
func (e *Engine) prompt(ctx context.Context, a model.Attempt, f model.Feature, stage string) (string, error) {
	source := struct {
		Title, Body, Risk string
		Checks            []string
		Acceptance        []model.Acceptance
		ProtectedPaths    []string
	}{f.Title, f.Body, f.Risk, f.Checks, f.Acceptance, e.cfg.Projects[f.Project].ProtectedPaths}
	b, _ := json.Marshal(source)
	role := "Implement exactly this approved feature. Do not integrate, push, deploy, alter acceptance, or touch protected paths. Run necessary checks but do not claim unobserved evidence. Return terminal JSON {\"summary\":\"...\",\"question\":\"\",\"acceptance\":[{\"id\":\"...\",\"passed\":true,\"evidence\":\"specific observed result\"}]}. Missing prerequisites: return question, never invent success."
	if stage == "scout" {
		role = "Read-only discovery. Identify relevant files and pitfalls for this exact feature. Do not edit or launch other agents. Return a concise factual summary."
	}
	if stage == "review" {
		role = "Independent read-only Astra review of committed candidate " + a.CandidateSHA + ". Inspect the actual diff and check evidence; verify every approved acceptance criterion. Do not edit any files. Never infer success from builder prose. Return terminal JSON {\"approved\":true,\"summary\":\"...\",\"findings\":[],\"acceptance\":[{\"id\":\"...\",\"passed\":true,\"evidence\":\"observed source/check evidence\"}]}. Reject if any criterion lacks evidence."
	}
	if stage == "plan" {
		role = "Read-only Astra planning. Inspect this goal and repository. Do not edit files or execute implementation. Propose a complete bounded feature DAG covering every stated requirement. Each child must name approved check profiles and concrete acceptance scenarios, with kind feature, risk low/normal/high/critical, completion_target reviewed/pull_request/integrated/staging/production. Never self-approve. Return terminal JSON {\"features\":[{\"id\":\"safe-unique-id\",\"project\":\"" + f.Project + "\",\"title\":\"...\",\"kind\":\"feature\",\"risk\":\"normal\",\"priority\":0,\"depends_on\":[],\"completion_target\":\"integrated\",\"checks\":[\"approved-check-name\"],\"acceptance\":[{\"id\":\"...\",\"scenario\":\"...\",\"expected\":\"...\"}],\"body\":\"markdown implementation scope\"}]}. No nested planning features. Existing dependencies must already exist; proposed dependencies may reference other proposed IDs."
	}
	prompt := role + "\nApproved immutable contract:\n" + string(b)
	if stage == "plan" {
		profiles, _ := json.Marshal(e.cfg.Projects[f.Project].Checks)
		prompt += "\nApproved check profiles:\n" + string(profiles)
	}
	if stage != "plan" {
		profiles := map[string]model.Command{}
		for _, name := range f.Checks {
			profiles[name] = e.cfg.Projects[f.Project].Checks[name]
		}
		commands, _ := json.Marshal(profiles)
		prompt += "\nApproved check profiles:\n" + string(commands)
	}
	if stage == "review" {
		proof, err := e.db.AttemptEvidence(ctx, a.ID)
		if err != nil {
			return "", err
		}
		current := proof[:0]
		for _, v := range proof {
			if v.CandidateSHA == a.CandidateSHA && v.CandidateGeneration == a.CandidateGeneration {
				current = append(current, v)
			}
		}
		proof = current
		for i := range proof {
			if len(proof[i].Detail) > 2048 {
				proof[i].Detail = proof[i].Detail[:1024] + "\n[excerpt: full output retained in evidence ledger]\n" + proof[i].Detail[len(proof[i].Detail)-1024:]
			}
		}
		checks, _ := json.Marshal(proof)
		prompt += "\nController-observed deterministic evidence:\n" + string(checks)
	}
	if stage == "build" && a.Summary != "" {
		summary := a.Summary
		if len(summary) > 4096 {
			summary = summary[:4096] + "\n[scout summary excerpt]"
		}
		prompt += "\nPrevious scout evidence (untrusted):\n" + summary
	}
	if stage == "build" && f.Reason != "" {
		reason := f.Reason
		if len(reason) > 4096 {
			reason = reason[:4096]
		}
		prompt += "\nConcrete previous attempt defect (repair only approved scope):\n" + reason
	}
	if len(prompt) > e.cfg.MaxPromptBytes {
		return "", errors.New("approved contract/evidence exceeds configured prompt bound; split feature")
	}
	return prompt, nil
}

func (e *Engine) check(ctx context.Context, a model.Attempt, f model.Feature) error {
	p := e.cfg.Projects[f.Project]
	proof := []model.Evidence{}
	if proceed, err := e.intent(ctx, a); err != nil || !proceed {
		return err
	}
	files, err := e.repos.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
	if err != nil {
		return e.block(ctx, a, "working-tree path inspection: "+err.Error(), false)
	}
	if err = e.paths(f, files); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	sha := a.CandidateSHA
	if sha == "" {
		// Checks run only in a fresh checkout of the controller-authored commit.
		a.Stage = "committing"
		if proceed, err := e.intent(ctx, a); err != nil || !proceed {
			return err
		}
		sha, err = e.repos.Commit(ctx, a.Workspace, f.Title)
		if err != nil {
			return e.block(ctx, a, "candidate commit result uncertain: "+err.Error(), true)
		}
		if sha == "" {
			return e.block(ctx, a, "candidate commit returned no identity", true)
		}
		a.CandidateSHA = sha
		a.CandidateGeneration = 1
		snapshot, err := e.repos.Snapshot(ctx, a.Workspace, sha)
		if err != nil {
			return e.block(ctx, a, "clean candidate snapshot: "+err.Error(), false)
		}
		if snapshot.Path == "" || snapshot.Path == a.Workspace {
			return e.block(ctx, a, "candidate snapshot did not return a fresh workspace", false)
		}
		a.Workspace = snapshot.Path
		paths, err := e.repos.ChangedPaths(ctx, a.Workspace, a.BaseSHA, sha)
		if err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		if err = e.paths(f, paths); err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		a.Fingerprint, err = e.repos.Fingerprint(ctx, a.Workspace)
		if err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		// Persist checked identity before execution; restart reruns checks on this
		// snapshot, never captures a second candidate from check-produced files.
		a.Stage = "checks"
		if err = e.save(ctx, a); err != nil {
			return err
		}
	}
	before, err := e.repos.Fingerprint(ctx, a.Workspace)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if before != a.Fingerprint {
		return e.block(ctx, a, "candidate changed before deterministic checks", false)
	}
	for _, name := range f.Checks {
		if proceed, err := e.intent(ctx, a); err != nil || !proceed {
			return err
		}
		if err := e.db.RecordCheck(ctx, a.ID, name, a.CandidateGeneration, sha, "running", ""); err != nil {
			return err
		}
		output, checkErr := e.repos.Check(ctx, a.Workspace, p.Checks[name])
		after, err := e.repos.Fingerprint(ctx, a.Workspace)
		if err != nil {
			if recordErr := e.db.RecordCheck(ctx, a.ID, name, a.CandidateGeneration, sha, "failed", "candidate inspection after check: "+err.Error()); recordErr != nil {
				return recordErr
			}
			return e.block(ctx, a, err.Error(), false)
		}
		if a.Fingerprint != after {
			if err := e.db.RecordCheck(ctx, a.ID, name, a.CandidateGeneration, sha, "failed", "approved check mutated candidate workspace"); err != nil {
				return err
			}
			return e.block(ctx, a, "approved check mutated candidate workspace: "+name, false)
		}
		if checkErr != nil {
			reason := "check " + name + " failed: " + checkErr.Error() + "\n" + output
			if err := e.db.RecordCheck(ctx, a.ID, name, a.CandidateGeneration, sha, "failed", reason); err != nil {
				return err
			}
			var classified interface{ Infrastructure() bool }
			if errors.As(checkErr, &classified) && classified.Infrastructure() {
				return e.block(ctx, a, reason, false, model.Evidence{Kind: "check:" + name, CandidateSHA: sha, CandidateGeneration: a.CandidateGeneration, Passed: false, Detail: reason})
			}
			return e.repair(ctx, a, f, reason)
		}
		detail := "exit status 0\n" + output
		if err := e.db.RecordCheck(ctx, a.ID, name, a.CandidateGeneration, sha, "passed", detail); err != nil {
			return err
		}
		proof = append(proof, model.Evidence{Kind: "check:" + name, CandidateSHA: sha, CandidateGeneration: a.CandidateGeneration, Passed: true, Detail: detail})
	}
	a.Stage = "review_pending"
	return e.db.Save(ctx, a, "running", "deterministic checks passed", "", true, proof...)
}
func (e *Engine) paths(f model.Feature, files []string) error {
	p := e.cfg.Projects[f.Project]
	for _, file := range files {
		if file == "" || path.IsAbs(file) || file == ".." || path.Clean(file) != file || strings.Contains(file, "\\") || strings.HasPrefix(file, "../") {
			return errors.New("unsafe changed path")
		}
		for _, protected := range p.ProtectedPaths {
			if policy.Matches(protected, file) {
				return fmt.Errorf("candidate modifies protected path %s", file)
			}
		}
		if f.CompletionTarget == "staging" || f.CompletionTarget == "production" {
			allowed := false
			for _, pattern := range p.Releases[f.CompletionTarget].AllowedPaths {
				if policy.Matches(pattern, file) {
					allowed = true
				}
			}
			if !allowed {
				return fmt.Errorf("release path %s is not allowlisted", file)
			}
		}
	}
	return nil
}
func (e *Engine) complete(ctx context.Context, a model.Attempt, f model.Feature) error {
	if a.CandidateSHA == "" {
		return e.block(ctx, a, "missing candidate identity", false)
	}
	// Revalidate authority and diff immediately before any externally visible effect.
	if err := e.validate(f); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	files, err := e.repos.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if err = e.paths(f, files); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	fp, err := e.repos.Fingerprint(ctx, a.Workspace)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if fp != a.Fingerprint {
		return e.block(ctx, a, "candidate changed after independent review", false)
	}
	p := e.cfg.Projects[f.Project]
	receipt := "reviewed:" + a.CandidateSHA
	if err = e.proven(ctx, a, f); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if f.CompletionTarget != "reviewed" && a.IntegrationToken == "" {
		// Freeze the feature delta once, before it joins the delivery queue.
		if a.SourceBaseSHA == "" {
			a.SourceBaseSHA, a.SourceCandidateSHA = a.BaseSHA, a.CandidateSHA
		}
		a.Stage = "integration_queued"
		if err := e.save(ctx, a); err != nil {
			return err
		}
		return e.enqueueIntegration(ctx, a, f)
	}
	if f.CompletionTarget == "pull_request" {
		a.Stage = "opening_pr"
		if proceed, err := e.intent(ctx, a); err != nil || !proceed {
			return err
		}
		receipt, err = e.repos.PullRequest(ctx, f.Project, p, a, f)
		if err != nil {
			return e.deliveryFailure(ctx, a, "PR", err)
		}
	} else if f.CompletionTarget != "reviewed" {
		if f.CompletionTarget == "staging" || f.CompletionTarget == "production" {
			if err = e.repos.PreflightRelease(ctx, f.Project, p, a, f); err != nil {
				return e.block(ctx, a, "release preflight: "+err.Error(), false)
			}
		}
		a.Stage = "integrating"
		if proceed, err := e.intent(ctx, a); err != nil || !proceed {
			return err
		}
		receipt, err = e.repos.Integrate(ctx, f.Project, p, a, a.BaseSHA)
		if err != nil {
			return e.deliveryFailure(ctx, a, "integration", err)
		}
		if receipt == "" {
			return e.block(ctx, a, "integration returned no receipt", true)
		}
		if f.CompletionTarget == "staging" || f.CompletionTarget == "production" {
			a.Stage = "integrated"
			a.Summary = receipt
			return e.db.Save(ctx, a, "running", "integration proven; release pending", receipt, true)
		}
	}
	if receipt == "" {
		return e.block(ctx, a, "target operation returned no receipt", true)
	}
	a.State = "accepted"
	a.Stage = "accepted"
	return e.db.Save(ctx, a, "accepted", "target "+f.CompletionTarget+" proven", receipt, false)
}
