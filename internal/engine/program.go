package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfactory/internal/executor"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

// coordinatePrograms only materializes persisted work. Admission, capacity,
// deadlines and workers remain the ordinary Feature/Attempt scheduler's job.
func (e *Engine) coordinatePrograms(ctx context.Context) error {
	work, err := e.db.ProgramWork(ctx, e.cfg)
	if err != nil {
		return err
	}
	for _, item := range work {
		if item.Wait != nil {
			continue
		}
		if item.Publish {
			_, err = e.db.PublishProgramProposals(ctx, item.Program.ID, item.Manifest.SHA, e.cfg)
		} else if item.Step != nil {
			_, err = e.db.EnsureProgramStep(ctx, *item.Step, e.cfg)
		} else {
			continue
		}
		if err != nil {
			var domain *model.ProgramError
			code, detail := "waiting", err.Error()
			if errors.As(err, &domain) {
				code, detail = domain.Code, domain.Message
			} else if !errors.Is(err, store.ErrWaiting) {
				return err
			}
			if waitErr := e.db.RecordProgramWait(ctx, item.Program.ID, item.Revision.RevisionHash, code, detail); waitErr != nil {
				var stale *model.ProgramError
				if !errors.As(waitErr, &stale) || stale.Code != "stale_revision" {
					return waitErr
				}
			}
		}
	}
	return nil
}

func programReadOnly(kind string) bool {
	return kind != model.StepArchitecture && kind != model.StepContracts
}
func programEvaluation(kind string) bool {
	return kind == model.StepPlanEvaluation || kind == model.StepResultEvaluation || kind == model.StepContractEvaluation
}

func programArtifactInput(kind string) bool {
	switch kind {
	case model.ArtifactRequirements, model.ArtifactQuestions, model.ArtifactClaims, model.ArtifactArchitecture, model.ArtifactExperimentContracts, model.ArtifactEvaluation, model.ArtifactImplementationContracts, model.ArtifactRevision:
		return true
	default:
		return false
	}
}

func (e *Engine) advanceProgram(ctx context.Context, a model.Attempt, f model.Feature, step model.ProgramStep, paused bool) error {
	if a.Stage == "uncertain" {
		return nil
	}
	if f.State == "cancelling" {
		return e.cancelProgram(ctx, a, step, "")
	}
	if err := e.validate(f); err != nil {
		return e.cancelProgram(ctx, a, step, err.Error())
	}
	if !time.Now().Before(a.Deadline) {
		return e.cancelProgram(ctx, a, step, "attempt deadline expired")
	}
	if err := e.db.ValidateProgramAttempt(ctx, a, e.cfg); err != nil {
		return e.cancelProgram(ctx, a, step, err.Error())
	}
	workctx, cancel := context.WithDeadline(ctx, a.Deadline)
	defer cancel()
	if step.Kind == model.StepExperimentRun {
		return e.advanceProgramExperiment(workctx, a, f, step, paused)
	}
	if strings.HasSuffix(a.Stage, "_dispatching") {
		return e.reconcileProgram(workctx, a, f, step)
	}
	if strings.HasSuffix(a.Stage, "_running") {
		return e.pollProgram(workctx, a, f, step)
	}
	if paused {
		return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: "paused", Detail: "OpenFactory paused"})
	}
	switch a.Stage {
	case "prepare":
		repos, ok := e.repos.(model.DiscoveryRepositories)
		if !ok {
			return e.block(ctx, a, "repository manager does not support discovery workspaces", false)
		}
		w, err := repos.PrepareDiscovery(workctx, f.Project, e.cfg.Projects[f.Project], a.ID, step.Kind, step.ID)
		if err != nil {
			return e.block(ctx, a, "discovery prepare: "+err.Error(), false)
		}
		if w.Path == "" || w.ProjectRoot == "" || w.BaseSHA == "" {
			return e.block(ctx, a, "discovery preparation returned no workspace/source/baseline", false)
		}
		a.Workspace = w.Path
		a.BaseSHA = w.BaseSHA
		a.Stage = step.Kind + "_pending"
		a.ThreadID = ""
		a.EnvironmentID = ""
		return e.save(ctx, a)
	case step.Kind + "_pending":
		return e.dispatchProgram(workctx, a, f, step)
	default:
		return e.block(ctx, a, "unknown persisted discovery stage "+a.Stage, true)
	}
}

func (e *Engine) discoveryWorkspace(ctx context.Context, a model.Attempt, f model.Feature, step model.ProgramStep) (model.DiscoveryWorkspace, error) {
	repos, ok := e.repos.(model.DiscoveryRepositories)
	if !ok {
		return model.DiscoveryWorkspace{}, errors.New("repository manager does not support discovery workspaces")
	}
	w, err := repos.PrepareDiscovery(ctx, f.Project, e.cfg.Projects[f.Project], a.ID, step.Kind, step.ID)
	if err == nil && (w.Path != a.Workspace || w.BaseSHA != a.BaseSHA || w.ProjectRoot == "") {
		err = errors.New("persisted discovery workspace identity changed")
	}
	return w, err
}

func (e *Engine) dispatchProgram(ctx context.Context, a model.Attempt, f model.Feature, step model.ProgramStep) error {
	work, err := e.db.ProgramStepInputs(ctx, step.ID)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	prompt, inputs, err := e.programPrompt(ctx, a, f, step, work)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	w, err := e.discoveryWorkspace(ctx, a, f, step)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	cap, err := e.db.IssueResearchCapability(ctx, a, e.cfg)
	if err != nil {
		return e.block(ctx, a, "discovery capability: "+err.Error(), false)
	}
	sessionConfig := model.DiscoverySession{ProgramID: step.ProgramID, Revision: step.Revision, StepID: step.ID, AttemptID: a.ID, Role: step.Kind, BrokerURL: e.cfg.Discovery.BrokerURL, CapabilityToken: cap.Token, Deadline: a.Deadline, ReadOnly: programReadOnly(step.Kind), InputFiles: inputs}
	if !sessionConfig.ReadOnly {
		sessionConfig.AllowedWritePaths = []string{"experiments", "artifacts"}
	}
	if err = e.repos.(model.DiscoveryRepositories).ConfigureDiscoverySession(ctx, w, sessionConfig); err != nil {
		return e.block(ctx, a, "discovery input/session materialization: "+err.Error(), false)
	}
	project, err := e.backend.EnsureProject(ctx, "discovery-"+f.Project, w.ProjectRoot)
	if err != nil {
		var classified interface{ Uncertain() bool }
		if errors.As(err, &classified) && classified.Uncertain() {
			return e.wait(ctx, a, "bb_project_reconciling", err)
		}
		return e.block(ctx, a, "discovery BB project: "+err.Error(), false)
	}
	if programReadOnly(step.Kind) {
		a.Fingerprint, err = e.repos.(model.DiscoveryRepositories).FingerprintDiscovery(ctx, w)
		if err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
		if err = e.save(ctx, a); err != nil {
			return err
		}
	}
	a.DispatchKey = fmt.Sprintf("%s:%s:%d", a.ID, step.Kind, a.CandidateGeneration)
	a.EnvironmentID = ""
	a.Wait = nil
	if err = e.db.ReserveCall(ctx, a, step.Kind, a.DispatchKey, e.cfg.MaxCallsPerDay); err != nil {
		if errors.Is(err, store.ErrWaiting) {
			return e.wait(ctx, a, "model_call_limit", err)
		}
		var domain *model.ProgramError
		if errors.As(err, &domain) {
			return e.block(ctx, a, err.Error(), false)
		}
		return err
	}
	a.Stage = step.Kind + "_dispatching"
	a.ThreadID = ""
	a.Cursor = ""
	a.Summary = ""
	session, err := e.backend.Spawn(ctx, model.SpawnRequest{AttemptID: a.DispatchKey, ProjectID: project, Workspace: a.Workspace, Model: step.Model, Prompt: prompt})
	if err != nil {
		var classified interface{ Uncertain() bool }
		if errors.As(err, &classified) && !classified.Uncertain() {
			return e.block(ctx, a, "discovery spawn rejected: "+err.Error(), false)
		}
		return nil
	}
	return e.recordProgramSession(ctx, a, step, session)
}

func (e *Engine) recordProgramSession(ctx context.Context, a model.Attempt, step model.ProgramStep, session model.Session) error {
	if session.ThreadID == "" {
		return e.block(ctx, a, "discovery spawn returned no durable thread identity", true)
	}
	if programEvaluation(step.Kind) {
		work, err := e.db.ProgramStepInputs(ctx, step.ID)
		if err != nil {
			return e.block(ctx, a, err.Error(), true)
		}
		for _, ref := range work.Manifest.Inputs {
			if !programArtifactInput(ref.Kind) {
				continue
			}
			artifact, _, err := e.db.ProgramArtifactPayload(ctx, ref.ID)
			if err != nil {
				return e.block(ctx, a, "evaluator input: "+err.Error(), true)
			}
			if artifact.ProducerThreadID == session.ThreadID {
				return e.block(ctx, a, "independent evaluator reused an author thread", true)
			}
		}
	}
	if err := e.db.RecordSession(ctx, a.ID, step.Kind, a.DispatchKey, session); err != nil {
		return err
	}
	a.ThreadID = session.ThreadID
	a.EnvironmentID = session.EnvironmentID
	a.Stage = step.Kind + "_running"
	return e.save(ctx, a)
}

func (e *Engine) reconcileProgram(ctx context.Context, a model.Attempt, f model.Feature, step model.ProgramStep) error {
	w, err := e.discoveryWorkspace(ctx, a, f, step)
	if err != nil {
		return e.block(ctx, a, "discovery spawn workspace reconciliation: "+err.Error(), true)
	}
	project, err := e.backend.EnsureProject(ctx, "discovery-"+f.Project, w.ProjectRoot)
	if err != nil {
		return e.wait(ctx, a, "bb_project_reconciling", err)
	}
	if a.DispatchKey == "" {
		return e.block(ctx, a, "discovery dispatch intent omitted its exact key", true)
	}
	sessions, err := e.backend.Find(ctx, project, a.DispatchKey)
	if err != nil {
		return e.wait(ctx, a, "bb_session_reconciling", err)
	}
	if len(sessions) != 1 {
		return e.block(ctx, a, "discovery spawn intent could not be reconciled to exactly one session", true)
	}
	return e.recordProgramSession(ctx, a, step, sessions[0])
}

func (e *Engine) pollProgram(ctx context.Context, a model.Attempt, f model.Feature, step model.ProgramStep) error {
	o, err := e.backend.Poll(ctx, a.ThreadID, a.Cursor)
	if err != nil {
		return nil
	}
	// The ordinary prompt bound is not an artifact transport limit. Large
	// frozen evidence is paged from immutable inputs; typed model results have
	// their own controller-side JSON envelope bound.
	const limit = 8 << 20
	if len(o.Output) > limit-len(a.Summary) {
		return e.cancelProgram(ctx, a, step, "discovery result exceeds typed JSON envelope bound")
	}
	a.Summary += o.Output
	a.Cursor = o.Cursor
	if o.State == "running" {
		return e.save(ctx, a)
	}
	if o.State != "completed" {
		if !terminal(o.State) {
			return e.cancelProgram(ctx, a, step, "discovery agent blocked or unknown: "+o.State+" "+o.Reason)
		}
		return e.block(ctx, a, "discovery agent did not complete: "+o.State+" "+o.Reason, false)
	}
	if err = e.finishProgramAnalysis(ctx, a, f, step); err != nil {
		return e.block(ctx, a, "discovery result: "+err.Error(), false)
	}
	return nil
}

func (e *Engine) programExperimentRequest(ctx context.Context, a model.Attempt, step model.ProgramStep) (model.ExperimentRunRequest, error) {
	work, err := e.db.ProgramStepInputs(ctx, step.ID)
	if err != nil {
		return model.ExperimentRunRequest{}, err
	}
	for _, ref := range work.Manifest.Inputs {
		if ref.Kind != model.ArtifactExperimentContracts {
			continue
		}
		artifact, raw, err := e.db.ProgramArtifactPayload(ctx, ref.ID)
		if err != nil {
			return model.ExperimentRunRequest{}, err
		}
		if artifact.Kind != model.ArtifactExperimentContracts {
			continue
		}
		var payload model.ExperimentContractsPayload
		if err = json.Unmarshal(raw, &payload); err != nil {
			return model.ExperimentRunRequest{}, err
		}
		for _, contract := range payload.Experiments {
			if executor.ContractSHA(contract) != step.SubjectID {
				continue
			}
			bundle, err := e.db.ProgramBundle(ctx, contract.BundleManifestSHA)
			if err != nil {
				return model.ExperimentRunRequest{}, err
			}
			return executor.NormalizeRequest(model.ExperimentRunRequest{ProgramID: step.ProgramID, Revision: step.Revision, StepID: step.ID, AttemptID: a.ID, ContractSHA: step.SubjectID, Contract: contract, Bundle: bundle, Ordinal: 0, ExecutionPolicySHA: model.ExperimentPolicySHA(e.cfg.Discovery.ExperimentProfiles, e.cfg.Discovery.ExperimentLimits)})
		}
	}
	return model.ExperimentRunRequest{}, errors.New("exact approved experiment contract is absent from step inputs")
}

func (e *Engine) storedProgramExperiment(ctx context.Context, a model.Attempt, step model.ProgramStep) (model.ExperimentRunRequest, error) {
	runID := executor.RunID(a.ID, step.SubjectID, 0)
	req, err := e.db.ProgramExperimentIntent(ctx, runID)
	if err != nil {
		return req, err
	}
	if req.RunID != runID || req.AttemptID != a.ID || req.StepID != step.ID || req.ContractSHA != step.SubjectID || req.ProgramID != step.ProgramID || req.Revision != step.Revision {
		return req, errors.New("durable experiment request does not match the owning attempt")
	}
	return executor.NormalizeRequest(req)
}

func (e *Engine) advanceProgramExperiment(ctx context.Context, a model.Attempt, f model.Feature, step model.ProgramStep, paused bool) error {
	if paused && (a.Stage == "prepare" || a.Stage == "experiment_pending") {
		return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: "paused", Detail: "OpenFactory paused"})
	}
	if e.experiments == nil {
		return e.block(ctx, a, "experiment runner is unavailable", false)
	}
	if a.Stage == "prepare" {
		a.Stage = "experiment_pending"
		return e.save(ctx, a)
	}
	runID := executor.RunID(a.ID, step.SubjectID, 0)
	var err error
	var receipt model.ExperimentReceipt
	starting := a.Stage == "experiment_pending"
	switch a.Stage {
	case "experiment_pending":
		req, requestErr := e.programExperimentRequest(ctx, a, step)
		if requestErr != nil {
			return e.block(ctx, a, requestErr.Error(), false)
		}
		if err = e.db.RecordExperimentIntent(ctx, req); err != nil {
			return e.block(ctx, a, "experiment intent: "+err.Error(), false)
		}
		a.Stage = "experiment_starting"
		a.DispatchKey = req.RunID
		if err = e.db.BeginOperation(ctx, a, e.cfg.PolicyHash); err != nil {
			if errors.Is(err, store.ErrWaiting) {
				return e.wait(ctx, a, "experiment_authority", err)
			}
			return e.block(ctx, a, err.Error(), false)
		}
		receipt, err = e.experiments.Start(ctx, req)
	case "experiment_starting", "experiment_running":
		receipt, err = e.experiments.Status(ctx, runID)
		var absent *executor.ExperimentError
		if a.Stage == "experiment_starting" && errors.As(err, &absent) && absent.Code == "experiment_not_found" && absent.Status == 404 {
			// A healthy runner has certified that this exact run has no journal
			// entry. Retry the immutable intent, never a freshly generated request.
			req, loadErr := e.storedProgramExperiment(ctx, a, step)
			if loadErr != nil {
				return e.block(ctx, a, "experiment recovery intent: "+loadErr.Error(), true)
			}
			if err = e.db.BeginOperation(ctx, a, e.cfg.PolicyHash); err != nil {
				if errors.Is(err, store.ErrWaiting) {
					return e.wait(ctx, a, "experiment_authority", err)
				}
				return e.cancelProgram(ctx, a, step, err.Error())
			}
			starting = true
			receipt, err = e.experiments.Start(ctx, req)
		}
	default:
		return e.block(ctx, a, "unknown persisted experiment stage "+a.Stage, true)
	}
	if err != nil {
		var runnerError *executor.ExperimentError
		if errors.As(err, &runnerError) {
			if starting && (runnerError.Code == "prerequisite_missing" || runnerError.Code == "invalid_contract" || runnerError.Code == "policy_changed") {
				return e.block(ctx, a, "experiment could not start: "+err.Error(), false)
			}
			if runnerError.Status == 404 || runnerError.Code == "idempotency_conflict" {
				return e.block(ctx, a, "experiment identity cannot be reconciled: "+err.Error(), true)
			}
		}
		return e.wait(ctx, a, "experiment_reconciling", err)
	}
	if err = e.db.RecordExperimentReceipt(ctx, runID, receipt); err != nil {
		return e.block(ctx, a, "experiment receipt: "+err.Error(), true)
	}
	switch receipt.State {
	case "intent", "running", "cancelling":
		a.Stage = "experiment_running"
		a.DispatchKey = runID
		return e.save(ctx, a)
	case "completed":
		if !receipt.Conclusive {
			return e.block(ctx, a, "experiment completed without conclusive evidence: "+receipt.Detail, false)
		}
		if err = e.db.CompleteProgramStep(ctx, a, model.ProgramStepResult{Outcome: "completed", Message: "experiment receipt recorded"}); err != nil {
			return e.block(ctx, a, "experiment completion: "+err.Error(), false)
		}
		return nil
	case "resolved_failed":
		return e.block(ctx, a, "experiment uncertainty resolved as failed; not acceptance", false)
	case "uncertain":
		return e.block(ctx, a, "experiment runner reports uncertain external ownership", true)
	case "waiting_prerequisite":
		return e.block(ctx, a, "experiment prerequisite: "+receipt.Detail, false)
	case "failed", "cancelled":
		return e.block(ctx, a, "experiment did not produce conclusive evidence: "+receipt.FailureKind+" "+receipt.Detail, false)
	default:
		return e.block(ctx, a, "unknown experiment runner state "+receipt.State, true)
	}
}

// cancelProgram retains ownership until a durable model/runner terminal state
// has been observed. Uncertain external work is never turned into free capacity.
func (e *Engine) cancelProgram(ctx context.Context, a model.Attempt, step model.ProgramStep, reason string) error {
	if step.Kind == model.StepExperimentRun && (a.Stage == "experiment_starting" || a.Stage == "experiment_running") {
		if e.experiments == nil {
			return e.block(ctx, a, "experiment runner unavailable during cancellation", true)
		}
		req, err := e.storedProgramExperiment(ctx, a, step)
		if err != nil {
			return e.block(ctx, a, "experiment cancellation intent: "+err.Error(), true)
		}
		runID := req.RunID
		receipt, err := e.experiments.CancelIntent(ctx, req)
		if err != nil {
			var failure *executor.ExperimentError
			if errors.As(err, &failure) && (failure.Status == 404 || failure.Code == "idempotency_conflict" || failure.Code == "invalid_contract") {
				return e.block(ctx, a, "experiment cancellation cannot reconcile identity: "+err.Error(), true)
			}
			return e.wait(ctx, a, "experiment_cancelling", err)
		}
		if err = e.db.RecordExperimentReceipt(ctx, runID, receipt); err != nil {
			return e.block(ctx, a, "experiment cancellation receipt: "+err.Error(), true)
		}
		switch receipt.State {
		case "intent", "running", "cancelling":
			return nil
		case "uncertain":
			return e.block(ctx, a, "experiment ownership uncertain during cancellation", true)
		case "completed", "cancelled", "failed", "resolved_failed", "waiting_prerequisite":
		default:
			return e.block(ctx, a, "unknown experiment cancellation state", true)
		}
	} else {
		if strings.HasSuffix(a.Stage, "_dispatching") {
			f, err := e.db.Get(ctx, a.FeatureID)
			if err != nil {
				return err
			}
			return e.reconcileProgram(ctx, a, f, step)
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
	}
	if reason != "" {
		return e.block(ctx, a, reason, false)
	}
	a.State = "cancelled"
	a.Stage = "cancelled"
	return e.db.Save(ctx, a, "cancelled", "program cancellation settled", "", false)
}
