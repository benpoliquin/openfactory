package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

const intakeStage = "intake"

var errIntakeSessionUncertain = errors.New("intake spawn intent did not reconcile to exactly one durable session")

// Intake work uses the ordinary persisted Feature/Attempt queue. The coordinator
// only materializes a version-bound internal feature; it never calls a model.
func (e *Engine) coordinateIntakes(ctx context.Context) error {
	if err := e.db.ReconcileIntakeRequests(ctx, e.cfg); err != nil {
		return err
	}
	if !e.cfg.Intake.Enabled {
		return nil
	}
	after := ""
	for {
		requests, err := e.db.ListIntakeRequests(ctx, "", "", after, 200)
		if err != nil {
			return err
		}
		for _, request := range requests {
			if request.State != model.IntakeDraftQueued || request.ActiveStepID != "" {
				continue
			}
			if _, err = e.db.CreateIntakeStep(ctx, request.ID, request.ContentVersion, e.cfg); err != nil {
				var domain *model.ProgramError
				if errors.Is(err, store.ErrWaiting) || errors.As(err, &domain) {
					continue
				}
				return err
			}
		}
		if len(requests) < 200 {
			return nil
		}
		after = requests[len(requests)-1].ID
	}
}

func (e *Engine) intakeRepositories() (model.IntakeRepositories, error) {
	repos, ok := e.repos.(model.IntakeRepositories)
	if !ok {
		return nil, errors.New("repository manager does not support intake workspaces")
	}
	return repos, nil
}

func (e *Engine) intakeWorkspace(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep) (model.IntakeWorkspace, error) {
	repos, err := e.intakeRepositories()
	if err != nil {
		return model.IntakeWorkspace{}, err
	}
	w, err := repos.PrepareIntake(ctx, f.Project, e.cfg.Projects[f.Project], a.ID, step.RequestID, step.ID)
	if err == nil && (w.Path == "" || w.ProjectRoot == "" || w.BaseSHA == "" || a.Workspace != "" && (w.Path != a.Workspace || w.BaseSHA != a.BaseSHA)) {
		err = errors.New("persisted intake workspace identity changed")
	}
	return w, err
}

func (e *Engine) advanceIntake(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep, paused bool) error {
	// Cancellation, policy and content changes must reconcile an existing intent
	// even after the immutable execution deadline. No new dispatch is permitted.
	// Save and step binding are separate durable writes. Repair the exact
	// recorded session association before validating or consuming output after
	// a restart; this never creates or dispatches a replacement worker.
	if a.ThreadID != "" {
		if err := e.db.BindIntakeAttempt(ctx, a); err != nil {
			var domain *model.ProgramError
			if errors.As(err, &domain) {
				return e.block(ctx, a, "intake persisted session binding: "+domain.Message, true)
			}
			return err
		}
	}
	if f.State == "cancelling" {
		return e.stopIntake(ctx, a, f, step, "intake cancellation settled", true)
	}
	if a.Stage == "intake_stopping" {
		reason := "intake worker ownership remains unsettled"
		if a.Wait != nil && a.Wait.Code == "intake_stop_pending" {
			reason = a.Wait.Detail
		}
		return e.stopIntake(ctx, a, f, step, reason, false)
	}
	if err := e.validate(f); err != nil {
		return e.stopIntake(ctx, a, f, step, err.Error(), false)
	}
	if err := e.db.ValidateIntakeAttempt(ctx, a, e.cfg); err != nil {
		return e.stopIntake(ctx, a, f, step, err.Error(), false)
	}
	if !time.Now().Before(a.Deadline) {
		return e.stopIntake(ctx, a, f, step, "intake attempt deadline expired", false)
	}
	workctx, cancel := context.WithDeadline(ctx, a.Deadline)
	defer cancel()
	if a.Stage == "uncertain" {
		return e.reconcileIntake(workctx, a, f, step)
	}
	if strings.HasSuffix(a.Stage, "_dispatching") {
		return e.reconcileIntake(workctx, a, f, step)
	}
	if strings.HasSuffix(a.Stage, "_running") {
		return e.pollIntake(workctx, a, f, step)
	}
	if paused {
		return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: "paused", Detail: "OpenFactory paused"})
	}
	switch a.Stage {
	case "prepare":
		w, err := e.intakeWorkspace(workctx, a, f, step)
		if err != nil {
			return e.db.FailIntakeStep(ctx, a, step, "intake prepare: "+err.Error(), false)
		}
		a.Workspace, a.BaseSHA = w.Path, w.BaseSHA
		a.ThreadID, a.EnvironmentID = "", ""
		a.Stage = intakeStage + "_pending"
		if err = e.save(ctx, a); err != nil {
			return err
		}
		return e.db.BindIntakeAttempt(ctx, a)
	case "intake_pending":
		return e.dispatchIntake(workctx, a, f, step)
	default:
		return e.stopIntake(ctx, a, f, step, "unknown persisted intake stage", false)
	}
}

func (e *Engine) dispatchIntake(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep) error {
	prompt, inputs, err := e.intakePrompt(ctx, a, f, step)
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, "intake context: "+err.Error(), false)
	}
	w, err := e.intakeWorkspace(ctx, a, f, step)
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, err.Error(), false)
	}
	repos, err := e.intakeRepositories()
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, err.Error(), false)
	}
	session := model.IntakeSession{Project: f.Project, RequestID: step.RequestID, StepID: step.ID, AttemptID: a.ID, Deadline: a.Deadline, InputFiles: inputs}
	if err = repos.ConfigureIntakeSession(ctx, w, session); err != nil {
		return e.db.FailIntakeStep(ctx, a, step, "intake input materialization: "+err.Error(), false)
	}
	project, err := e.backend.EnsureProject(ctx, "intake-"+f.Project, w.ProjectRoot)
	if err != nil {
		return e.wait(ctx, a, "bb_project_reconciling", err)
	}
	a.Fingerprint, err = repos.FingerprintIntake(ctx, w)
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, err.Error(), false)
	}
	if err = e.save(ctx, a); err != nil {
		return err
	}
	a.DispatchKey = fmt.Sprintf("%s:%s:%s", a.ID, intakeStage, step.ID)
	a.EnvironmentID, a.ThreadID, a.Cursor, a.Summary = "", "", "", ""
	a.Wait = nil
	if err = e.db.ReserveCall(ctx, a, intakeStage, a.DispatchKey, e.cfg.MaxCallsPerDay); err != nil {
		if errors.Is(err, store.ErrWaiting) {
			return e.wait(ctx, a, "model_call_limit", err)
		}
		var domain *model.ProgramError
		if errors.As(err, &domain) {
			return e.db.FailIntakeStep(ctx, a, step, err.Error(), false)
		}
		return err
	}
	a.Stage = intakeStage + "_dispatching"
	spawned, err := e.backend.Spawn(ctx, model.SpawnRequest{AttemptID: a.DispatchKey, ProjectID: project, Workspace: a.Workspace, Model: step.Model, Prompt: prompt})
	if err != nil {
		var classified interface{ Uncertain() bool }
		if errors.As(err, &classified) && !classified.Uncertain() {
			return e.db.FailIntakeStep(ctx, a, step, "intake spawn rejected: "+err.Error(), false)
		}
		// ReserveCall already persisted the intent. Restart must Find, never Spawn.
		return nil
	}
	return e.recordIntakeSession(ctx, a, step, spawned)
}

func (e *Engine) recordIntakeSession(ctx context.Context, a model.Attempt, step model.IntakeStep, session model.Session) error {
	if session.ThreadID == "" {
		return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: "bb_session_reconciling", Detail: "intake spawn returned no durable thread identity; exact intent remains reserved"})
	}
	if err := e.db.RecordSession(ctx, a.ID, intakeStage, a.DispatchKey, session); err != nil {
		return err
	}
	a.ThreadID, a.EnvironmentID = session.ThreadID, session.EnvironmentID
	a.Stage = intakeStage + "_running"
	if err := e.save(ctx, a); err != nil {
		return err
	}
	return e.db.BindIntakeAttempt(ctx, a)
}

func (e *Engine) findIntakeSession(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep) (model.Session, error) {
	repos, err := e.intakeRepositories()
	if err != nil {
		return model.Session{}, err
	}
	w, err := repos.ReopenIntake(ctx, a.Workspace, f.Project, a.ID, step.RequestID, step.ID)
	if err == nil && (w.Path != a.Workspace || w.BaseSHA != a.BaseSHA || w.ProjectRoot == "") {
		err = errors.New("persisted intake reconciliation workspace identity changed")
	}
	if err != nil {
		return model.Session{}, err
	}
	project, err := e.backend.EnsureProject(ctx, "intake-"+f.Project, w.ProjectRoot)
	if err != nil {
		return model.Session{}, err
	}
	if a.DispatchKey == "" {
		return model.Session{}, errors.New("intake dispatch intent omitted its exact key")
	}
	sessions, err := e.backend.Find(ctx, project, a.DispatchKey)
	if err != nil {
		return model.Session{}, err
	}
	if len(sessions) != 1 || sessions[0].ThreadID == "" {
		// Preserve every observed external effect for trusted reconciliation. A
		// cardinality mismatch is uncertain ownership, never proof of absence.
		for _, session := range sessions {
			if session.ThreadID != "" {
				if err = e.db.RecordSession(ctx, a.ID, intakeStage, a.DispatchKey, session); err != nil {
					return model.Session{}, err
				}
			}
		}
		return model.Session{}, errIntakeSessionUncertain
	}
	return sessions[0], nil
}

func (e *Engine) reconcileIntake(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep) error {
	session, err := e.findIntakeSession(ctx, a, f, step)
	if err != nil {
		if errors.Is(err, errIntakeSessionUncertain) || !time.Now().Before(a.Deadline) {
			return e.block(ctx, a, err.Error(), true)
		}
		return e.wait(ctx, a, "bb_session_reconciling", err)
	}
	return e.recordIntakeSession(ctx, a, step, session)
}

func (e *Engine) stopIntake(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep, reason string, cancelled bool) error {
	if a.Stage == "uncertain" && a.ThreadID == "" && a.DispatchKey == "" {
		return e.block(ctx, a, "intake uncertain ownership has no durable dispatch identity", true)
	}
	// A stop is a durable, irreversible intent. If Stop/Poll or Find is
	// unavailable, keep capacity and expose a blocked attempt for trusted
	// reconciliation; never return to ordinary polling and publish a result.
	if a.Stage != "intake_stopping" || a.Wait == nil || a.Wait.Code != "intake_stop_pending" || a.Wait.Detail != reason {
		a.Stage, a.State = "intake_stopping", "blocked"
		a.Wait = &model.WaitReason{Code: "intake_stop_pending", Detail: reason}
		if err := e.db.Save(ctx, a, "blocked", reason, "", true); err != nil {
			return err
		}
	}
	if a.ThreadID == "" && a.DispatchKey != "" {
		session, err := e.findIntakeSession(ctx, a, f, step)
		if err != nil {
			return nil
		}
		// RecordSession is the durable external ownership receipt; do not require
		// superseded execution authority merely to record and stop an existing worker.
		if err = e.db.RecordSession(ctx, a.ID, intakeStage, a.DispatchKey, session); err != nil {
			return err
		}
		a.ThreadID, a.EnvironmentID = session.ThreadID, session.EnvironmentID
		if err = e.db.Save(ctx, a, "blocked", reason, "", true); err != nil {
			return err
		}
		if err = e.db.BindIntakeAttempt(ctx, a); err != nil {
			return err
		}
	}
	if a.ThreadID != "" {
		if err := e.backend.Stop(ctx, a.ThreadID); err != nil {
			return nil
		}
		observation, err := e.backend.Poll(ctx, a.ThreadID, a.Cursor)
		if err != nil || !terminal(observation.State) {
			return nil
		}
		limit := e.cfg.Intake.MaxOutputBytes
		if limit <= 0 || limit > 256<<10 {
			limit = 256 << 10
		}
		if len(observation.Output) <= limit-len(a.Summary) {
			a.Summary += observation.Output
			a.Cursor = observation.Cursor
		} else {
			reason += "; terminal output exceeded the bounded result envelope"
		}
	}
	return e.db.FailIntakeStep(ctx, a, step, reason, cancelled)
}

func (e *Engine) pollIntake(ctx context.Context, a model.Attempt, f model.Feature, step model.IntakeStep) error {
	observation, err := e.backend.Poll(ctx, a.ThreadID, a.Cursor)
	if err != nil {
		return nil
	}
	limit := e.cfg.Intake.MaxOutputBytes
	if limit <= 0 || limit > 256<<10 {
		limit = 256 << 10
	}
	if len(observation.Output) > limit-len(a.Summary) {
		return e.stopIntake(ctx, a, f, step, "intake output exceeds 256 KiB result bound", false)
	}
	a.Summary += observation.Output
	a.Cursor = observation.Cursor
	if observation.State == "running" {
		return e.save(ctx, a)
	}
	if observation.State != "completed" {
		if terminal(observation.State) {
			return e.db.FailIntakeStep(ctx, a, step, "intake worker did not complete", false)
		}
		return e.stopIntake(ctx, a, f, step, "intake worker returned an unknown or blocked state", false)
	}
	repos, err := e.intakeRepositories()
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, err.Error(), false)
	}
	w, err := e.intakeWorkspace(ctx, a, f, step)
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, err.Error(), false)
	}
	fingerprint, err := repos.FingerprintIntake(ctx, w)
	if err != nil || fingerprint != a.Fingerprint {
		return e.db.FailIntakeStep(ctx, a, step, "intake worker modified its immutable workspace", false)
	}
	result, err := intake.ParseModelOutput([]byte(a.Summary), limit)
	if err != nil {
		return e.db.FailIntakeStep(ctx, a, step, "invalid intake model output: "+err.Error(), false)
	}
	// Store validates exact canonical source and version/consent authority in the
	// same transaction that publishes questions/draft and settles the attempt.
	if err = e.db.ApplyIntakeResult(ctx, a, step, result, e.cfg); err != nil {
		var domain *model.ProgramError
		if errors.As(err, &domain) {
			return e.db.FailIntakeStep(ctx, a, step, "intake result validation: "+domain.Message, false)
		}
		return err
	}
	return nil
}
