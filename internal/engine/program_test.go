package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/executor"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

// These adapters are deliberately test-only deterministic model/runner backends.
// They exercise controller authority and persistence, not public-source research,
// a real model's reasoning, or actual subprocess execution. No product candidate,
// check, integration, or approval is manufactured by either adapter.
type programTestBackend struct {
	db                         *store.Store
	repos                      *programTestRepositories
	requests                   []model.SpawnRequest
	projects                   [][2]string
	sessions                   map[string]model.Session
	steps                      map[string]model.ProgramStep
	stopped                    map[string]bool
	idle                       bool
	spawnError                 bool
	findMissing                bool
	decisive                   bool
	multipleExperiments        bool
	reviseAll                  bool
	reviseFirst                bool
	mutateReadOnly             bool
	reuseEvaluator             bool
	staleEvaluation            bool
	invalidImplementationCheck bool
	largePayloadBytes          int
}

func (*programTestBackend) Doctor(context.Context) error { return nil }
func (b *programTestBackend) EnsureProject(_ context.Context, name, root string) (string, error) {
	b.projects = append(b.projects, [2]string{name, root})
	return "test-only-discovery-project", nil
}
func (b *programTestBackend) Spawn(ctx context.Context, request model.SpawnRequest) (model.Session, error) {
	active, err := b.db.Active(ctx)
	if err != nil {
		return model.Session{}, err
	}
	var step model.ProgramStep
	for _, attempt := range active {
		if strings.HasPrefix(request.AttemptID, attempt.ID+":") {
			step, err = b.db.ProgramStepForFeature(ctx, attempt.FeatureID)
			if err != nil {
				return model.Session{}, err
			}
			if request.AttemptID != attempt.ID+":"+step.Kind+":0" {
				return model.Session{}, fmt.Errorf("noncanonical discovery dispatch key %q", request.AttemptID)
			}
			break
		}
	}
	if step.ID == "" {
		return model.Session{}, errors.New("test backend received unassociated discovery work")
	}
	if request.EnvironmentID != "" || request.Model != step.Model {
		return model.Session{}, errors.New("discovery did not use a fresh session and persisted model")
	}
	b.requests = append(b.requests, request)
	session := model.Session{ThreadID: fmt.Sprintf("test-only-discovery-thread-%d", len(b.requests)), EnvironmentID: fmt.Sprintf("test-only-discovery-environment-%d", len(b.requests))}
	if b.reuseEvaluator && (step.Kind == model.StepPlanEvaluation || step.Kind == model.StepResultEvaluation || step.Kind == model.StepContractEvaluation) {
		for _, prior := range b.sessions {
			session.ThreadID = prior.ThreadID
			break
		}
	}
	b.sessions[request.AttemptID] = session
	b.steps[session.ThreadID] = step
	if b.spawnError {
		return model.Session{}, errors.New("test-only transport lost spawn response after creation")
	}
	return session, nil
}
func (b *programTestBackend) Find(_ context.Context, _ string, key string) ([]model.Session, error) {
	if b.findMissing {
		return nil, nil
	}
	if session, ok := b.sessions[key]; ok {
		return []model.Session{session}, nil
	}
	return nil, nil
}
func (b *programTestBackend) Stop(_ context.Context, thread string) error {
	b.stopped[thread] = true
	return nil
}
func (b *programTestBackend) Poll(ctx context.Context, thread, _ string) (model.Observation, error) {
	if b.stopped[thread] {
		return model.Observation{State: "interrupted", Cursor: "test-only-stopped"}, nil
	}
	if b.idle {
		return model.Observation{State: "running", Cursor: "test-only-durable-cursor"}, nil
	}
	step, ok := b.steps[thread]
	if !ok {
		return model.Observation{}, errors.New("unknown test-only discovery thread")
	}
	if b.mutateReadOnly && step.Kind == model.StepResearch {
		if err := os.WriteFile(filepath.Join(b.repos.workspaces[step.ID].Path, "unauthorized.txt"), []byte("test-only forbidden research mutation"), 0600); err != nil {
			return model.Observation{}, err
		}
	}
	result, err := b.result(ctx, step)
	if err != nil {
		return model.Observation{}, err
	}
	out, err := json.Marshal(result)
	return model.Observation{State: "completed", Cursor: "test-only-terminal", Output: string(out)}, err
}
func programTestDraft(kind string, payload any) model.ProgramArtifactDraft {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return model.ProgramArtifactDraft{Kind: kind, Payload: data}
}
func (b *programTestBackend) result(ctx context.Context, step model.ProgramStep) (model.ProgramStepResult, error) {
	contextBytes, err := os.ReadFile(filepath.Join(b.repos.workspaces[step.ID].Path, "inputs", "context.json"))
	if err != nil {
		return model.ProgramStepResult{}, fmt.Errorf("test-only model cannot read materialized immutable context: %w", err)
	}
	var inputs struct {
		Step     model.ProgramStep     `json:"step"`
		Revision model.ProgramRevision `json:"revision"`
		Manifest model.ProgramManifest `json:"manifest"`
	}
	if err := json.Unmarshal(contextBytes, &inputs); err != nil {
		return model.ProgramStepResult{}, err
	}
	if inputs.Step.ID != step.ID || inputs.Manifest.SHA != step.InputManifestSHA || inputs.Revision.ProgramID != step.ProgramID {
		return model.ProgramStepResult{}, errors.New("materialized model input identity does not match owning step")
	}
	result := model.ProgramStepResult{Outcome: "completed", Message: "Test-only deterministic discovery output; not production evidence"}
	refs := inputs.Manifest.Inputs
	evidence := []model.ProgramInputRef{}
	for _, ref := range refs {
		if ref.Kind != "revision" && ref.Kind != "program_revision" && ref.Kind != "bundle" {
			evidence = append(evidence, ref)
		}
	}
	switch step.Kind {
	case model.StepResearch:
		questions := []model.ProgramQuestion{}
		questionRefs := []string{}
		if b.decisive {
			questions = append(questions, model.ProgramQuestion{ID: "durability", Text: "Must acknowledgement survive process loss?", Decisive: true, AffectedIDs: []string{"ordered-delivery"}, Route: "operator"})
			questionRefs = append(questionRefs, "durability")
		}
		result.Artifacts = []model.ProgramArtifactDraft{
			programTestDraft(model.ArtifactRequirements, model.RequirementsPayload{Requirements: []model.ProgramRequirement{{ID: "ordered-delivery", Behavior: "Duplicate and reordered deliveries do not change the final value", Rationale: "State transitions must be deterministic", Acceptance: []model.Acceptance{{ID: "reorder", Scenario: "Deliver duplicates in reverse order", Expected: "The highest version wins"}}, Priority: "must", ClaimRefs: []string{"unknown-vendor"}, QuestionRefs: questionRefs}}}),
			programTestDraft(model.ArtifactQuestions, model.QuestionsPayload{Questions: questions}),
			programTestDraft(model.ArtifactClaims, model.ClaimsPayload{Claims: []model.ProgramClaim{{ID: "unknown-vendor", Statement: "Vendor-internal ordering guarantees have not been established", Classification: "unknown", Limitations: []string{"Test-only fixture makes no public-source assertion"}}}}),
		}
		if b.largePayloadBytes > 0 {
			var requirements model.RequirementsPayload
			if err := json.Unmarshal(result.Artifacts[0].Payload, &requirements); err != nil {
				return result, err
			}
			requirements.Requirements[0].Rationale += strings.Repeat("TEST_ONLY_LARGE_ARTIFACT\n", b.largePayloadBytes/24+1)
			result.Artifacts[0] = programTestDraft(model.ArtifactRequirements, requirements)
		}
	case model.StepArchitecture:
		workspace := b.repos.workspaces[step.ID]
		count := 1
		if b.multipleExperiments {
			count = 2
		}
		experiments := []model.ExperimentContract{}
		experimentRefs := []string{}
		for index := range count {
			id := fmt.Sprintf("reorder-%d-%d", step.Iteration, index)
			path := "experiments/" + id + "/probe.py"
			if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace.Path, path)), 0700); err != nil {
				return result, err
			}
			body := fmt.Sprintf("# Test-only experiment input, iteration %d, contract %s\n", step.Iteration, id)
			if b.largePayloadBytes > 0 {
				body += strings.Repeat("TEST_ONLY_LARGE_FROZEN_INPUT\n", b.largePayloadBytes/28+1)
			}
			if err := os.WriteFile(filepath.Join(workspace.Path, path), []byte(body), 0600); err != nil {
				return result, err
			}
			experiments = append(experiments, model.ExperimentContract{
				ID: id, DecisionRefs: []string{"ordering"}, Hypothesis: "Reordering preserves the highest value",
				Falsification: "A stale value remains after reordered delivery",
				Environment:   model.ExperimentEnvironment{Profile: "test-only-deterministic"},
				BundleFiles:   []string{path},
				Commands:      []model.ExperimentCommand{{Profile: "test-only-deterministic"}},
				Observations:  []model.ExperimentObservationSpec{{Name: "safe", Type: "boolean", OutputPath: "observations.json", JSONPointer: "/safe"}},
				Acceptance:    []model.ExperimentPredicate{{Observation: "safe", Operator: "eq", Value: json.RawMessage("true")}},
				Limits:        model.ExperimentLimits{WallSeconds: 5, MaxOutputBytes: 128 << 10, MaxArtifactBytes: 128 << 10, MaxArtifactFiles: 4},
				Limitations:   []string{"Deterministic test runner, not actual process execution"},
			})
			experimentRefs = append(experimentRefs, id)
		}
		selected, rejected := "arrival", "version"
		if step.Iteration > 0 {
			selected, rejected = rejected, selected
		}
		result.Artifacts = []model.ProgramArtifactDraft{
			programTestDraft(model.ArtifactArchitecture, model.ArchitecturePayload{Decisions: []model.ArchitectureDecision{{ID: "ordering", Title: "Order state transitions", Alternatives: []model.ArchitectureAlternative{{ID: "arrival", Description: "Apply arrival order"}, {ID: "version", Description: "Fence stale versions"}}, Criteria: []string{"Reordering safety"}, Selected: selected, Rejected: []string{rejected}, Tradeoffs: []string{"Version fencing stores one additional integer"}, Consequences: []string{"Consumers must preserve version identity"}, Assumptions: []string{"Versions are monotonic"}, RequirementRefs: []string{"ordered-delivery"}, ExperimentRefs: experimentRefs}}}),
			programTestDraft(model.ArtifactExperimentContracts, model.ExperimentContractsPayload{Experiments: experiments}),
		}
		if step.Iteration > 0 {
			for index := range result.Artifacts {
				latest := -1
				for _, ref := range evidence {
					if ref.Kind != result.Artifacts[index].Kind {
						continue
					}
					prior, _, err := b.db.ProgramArtifactPayload(ctx, ref.ID)
					if err != nil {
						return result, err
					}
					if prior.Iteration < step.Iteration && prior.Iteration > latest {
						result.Artifacts[index].SupersedesID = prior.ID
						latest = prior.Iteration
					}
				}
			}
			falsified := []model.ProgramInputRef{}
			for _, ref := range evidence {
				if ref.Kind == "experiment_receipt" {
					falsified = append(falsified, ref)
				}
			}
			result.Artifacts = append(result.Artifacts, programTestDraft(model.ArtifactRevision, model.RevisionPayload{Trigger: "A conclusive test-only observation falsified arrival ordering", FalsifiedEvidence: falsified, ChangedDecisions: []string{"ordering"}, ChangedAssumptions: []string{"Arrival order is not version order"}, Materiality: "non_material", PredecessorManifestSHA: step.InputManifestSHA}))
		}
	case model.StepPlanEvaluation, model.StepResultEvaluation, model.StepContractEvaluation:
		verdict := "accept"
		changes := []string{}
		if step.Kind == model.StepResultEvaluation && (b.reviseAll || b.reviseFirst && step.Iteration == 0) {
			verdict = "revise"
			changes = append(changes, "Replace arrival ordering with stale-version fencing")
		}
		result.Artifacts = []model.ProgramArtifactDraft{programTestDraft(model.ArtifactEvaluation, model.EvaluationPayload{EvaluatedManifestSHA: step.InputManifestSHA, Findings: []model.EvaluationFinding{{ID: "observed-evidence", Severity: "info", Detail: "Independent test-only evaluator considers the exact persisted evidence", Refs: evidence}}, Coverage: []model.EvaluationCoverage{{SubjectID: "ordered-delivery", Status: "covered", EvidenceRefs: evidence}, {SubjectID: "ordering", Status: "covered", EvidenceRefs: evidence}}, Verdict: verdict, RequiredChanges: changes})}
		if b.staleEvaluation {
			var evaluation model.EvaluationPayload
			if err := json.Unmarshal(result.Artifacts[0].Payload, &evaluation); err != nil {
				return result, err
			}
			evaluation.EvaluatedManifestSHA = strings.Repeat("f", 64)
			result.Artifacts[0] = programTestDraft(model.ArtifactEvaluation, evaluation)
		}
	case model.StepContracts:
		feature := contract("program-product-" + step.ProgramID)
		feature.Project = inputs.Revision.Spec.Project
		if b.invalidImplementationCheck {
			feature.Checks = []string{"test-only-unenrolled-check"}
		}
		result.Artifacts = []model.ProgramArtifactDraft{programTestDraft(model.ArtifactImplementationContracts, model.ImplementationContractsPayload{Contracts: []model.ImplementationContract{{Feature: feature, RequirementRefs: []string{"ordered-delivery"}, DecisionRefs: []string{"ordering"}, EvidenceRefs: evidence}}})}
	default:
		return result, fmt.Errorf("test-only backend cannot author step %q", step.Kind)
	}
	return result, nil
}

// programTestRepositories implements the real optional discovery interface. Its
// workspaces and bundles are real private files; product operations remain the
// existing test fixture and must never be called by discovery.
type programTestRepositories struct {
	testRepos
	root       string
	blobs      *artifacts.Store
	workspaces map[string]model.DiscoveryWorkspace
	sessions   []model.DiscoverySession
	frozen     []model.ProgramBundle
}

func (r *programTestRepositories) PrepareDiscovery(_ context.Context, project string, _ model.Project, attempt, role, step string) (model.DiscoveryWorkspace, error) {
	workspace := model.DiscoveryWorkspace{Path: filepath.Join(r.root, project, role, step, attempt), ProjectRoot: filepath.Join(r.root, project), BaseSHA: "test-only-discovery-baseline"}
	if err := os.MkdirAll(workspace.Path, 0700); err != nil {
		return workspace, err
	}
	r.workspaces[step] = workspace
	return workspace, nil
}
func (r *programTestRepositories) ConfigureDiscoverySession(_ context.Context, workspace model.DiscoveryWorkspace, session model.DiscoverySession) error {
	for _, input := range session.InputFiles {
		if !strings.HasPrefix(input.Path, "inputs/") || filepath.ToSlash(filepath.Clean(input.Path)) != input.Path || filepath.IsAbs(input.Path) {
			return errors.New("test-only materialization received unsafe controller input path")
		}
		sum := sha256.Sum256(input.Data)
		if hex.EncodeToString(sum[:]) != input.ContentSHA {
			return errors.New("controller input file content hash mismatch")
		}
		path := filepath.Join(workspace.Path, filepath.FromSlash(input.Path))
		if prior, err := os.ReadFile(path); err == nil {
			if !reflect.DeepEqual(prior, input.Data) {
				return errors.New("immutable discovery input changed during session replay")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, input.Data, 0400); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".openfactory"), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, ".openfactory", "discovery-session.json"), data, 0600); err != nil {
		return err
	}
	for index := range r.sessions {
		if r.sessions[index].AttemptID == session.AttemptID {
			r.sessions[index] = session
			return nil
		}
	}
	r.sessions = append(r.sessions, session)
	return nil
}
func (r *programTestRepositories) FreezeDiscoveryBundle(ctx context.Context, workspace model.DiscoveryWorkspace, files []string) (model.ProgramBundle, error) {
	bundle, err := r.blobs.ImportBundle(ctx, workspace.Path, files, model.BundleLimits{MaxFiles: 16, MaxFileBytes: 1 << 20, MaxTotalBytes: 4 << 20})
	if err == nil {
		r.frozen = append(r.frozen, bundle)
	}
	return bundle, err
}
func (r *programTestRepositories) MaterializeExperiment(ctx context.Context, attempt model.Attempt, bundle model.ProgramBundle) (model.Workspace, error) {
	workspace := model.Workspace{Path: filepath.Join(r.root, "runner", attempt.ID), BaseSHA: "test-only-frozen-bundle"}
	err := r.blobs.MaterializeBundle(ctx, bundle, workspace.Path, model.BundleLimits{MaxFiles: 16, MaxFileBytes: 1 << 20, MaxTotalBytes: 4 << 20})
	return workspace, err
}
func (*programTestRepositories) Fingerprint(ctx context.Context, root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel == ".openfactory" || rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("nonregular test workspace file %q", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}
func (r *programTestRepositories) FingerprintDiscovery(ctx context.Context, workspace model.DiscoveryWorkspace) (string, error) {
	return r.Fingerprint(ctx, workspace.Path)
}

// programTestRunner returns synthetic, explicitly test-labelled observations in
// actual CAS blobs. Store receipt validation therefore exercises real immutable
// identity, observation extraction, and manifest checks, not receipt booleans.
type programTestRunner struct {
	db                    *store.Store
	root                  string
	blobs                 *artifacts.Store
	starts                []model.ExperimentRunRequest
	statuses              []string
	cancelled             []string
	cancelIntents         []model.ExperimentRunRequest
	receipts              map[string]model.ExperimentReceipt
	acceptedRequests      map[string]model.ExperimentRunRequest
	processes             map[string]int
	executionPolicySHA    string
	loseStartBeforeIntent bool
	statusError           error
	failFirst             bool
	loseStart             bool
	largeOutputBytes      int
	uncertain             bool
	hold                  bool
	failAll               bool
}

func (r *programTestRunner) Start(ctx context.Context, request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	r.starts = append(r.starts, request)
	if prior, ok := r.acceptedRequests[request.RunID]; ok {
		if !reflect.DeepEqual(prior, request) {
			return model.ExperimentReceipt{}, &executor.ExperimentError{Code: "idempotency_conflict", Message: "test-only journal request changed", Status: 409}
		}
		return r.Status(ctx, request.RunID)
	}
	active, err := r.db.Active(ctx)
	if err != nil {
		return model.ExperimentReceipt{}, err
	}
	persisted := false
	for _, attempt := range active {
		if attempt.ID == request.AttemptID && attempt.Stage == "experiment_starting" {
			persisted = true
		}
	}
	if !persisted {
		return model.ExperimentReceipt{}, errors.New("runner called before durable starting stage")
	}
	if err := r.db.RecordExperimentIntent(ctx, request); err != nil {
		return model.ExperimentReceipt{}, fmt.Errorf("runner called without exact intent: %w", err)
	}
	if r.loseStartBeforeIntent && len(r.starts) == 1 {
		return model.ExperimentReceipt{}, &executor.ExperimentError{Code: "operation_uncertain", Message: "test-only transport lost before runner journal acceptance", Status: 503}
	}
	if request.ExecutionPolicySHA != r.executionPolicySHA {
		return model.ExperimentReceipt{}, &executor.ExperimentError{Code: "policy_changed", Message: "test-only executor execution policy differs", Status: 409}
	}
	if r.acceptedRequests == nil {
		r.acceptedRequests = map[string]model.ExperimentRunRequest{}
	}
	if r.processes == nil {
		r.processes = map[string]int{}
	}
	r.acceptedRequests[request.RunID] = request
	r.processes[request.RunID]++
	receipt, err := r.complete(ctx, request)
	if err != nil {
		return receipt, err
	}
	r.receipts[request.RunID] = receipt
	if r.loseStart {
		return model.ExperimentReceipt{}, errors.New("test-only start response lost after durable runner acceptance")
	}
	if r.hold {
		receipt.State = "running"
		receipt.FinishedAt = nil
		receipt.Conclusive = false
		receipt.Accepted = false
		receipt.ExecutionOutcome = "running"
		receipt.HypothesisOutcome = "pending"
	}
	return receipt, nil
}
func (r *programTestRunner) complete(ctx context.Context, request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	accepted := !(r.failAll || r.failFirst && len(r.starts) == 1)
	body, err := json.Marshal(map[string]any{"safe": accepted, "test_only_padding": strings.Repeat("TEST_ONLY_LARGE_OBSERVATION\n", r.largeOutputBytes/27)})
	if err != nil {
		return model.ExperimentReceipt{}, err
	}
	root := filepath.Join(r.root, request.RunID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return model.ExperimentReceipt{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "observations.json"), body, 0600); err != nil {
		return model.ExperimentReceipt{}, err
	}
	bundle, err := r.blobs.ImportBundle(ctx, root, []string{"observations.json"}, model.BundleLimits{MaxFiles: 4, MaxFileBytes: 128 << 10, MaxTotalBytes: 128 << 10})
	if err != nil {
		return model.ExperimentReceipt{}, err
	}
	empty, err := r.blobs.Put(ctx, nil)
	if err != nil {
		return model.ExperimentReceipt{}, err
	}
	stdoutBytes := []byte(strings.Repeat("TEST_ONLY_LARGE_STDOUT\n", r.largeOutputBytes/22))
	stdout, err := r.blobs.Put(ctx, stdoutBytes)
	if err != nil {
		return model.ExperimentReceipt{}, err
	}
	binary, err := r.blobs.Put(ctx, []byte("test-only deterministic runner; no subprocess was executed"))
	if err != nil {
		return model.ExperimentReceipt{}, err
	}
	finished := time.Now().UTC()
	exit := 0
	value := json.RawMessage("false")
	hypothesis := "falsified"
	if accepted {
		value = json.RawMessage("true")
		hypothesis = "supported"
	}
	return model.ExperimentReceipt{
		RunID: request.RunID, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID,
		Revision: request.Revision, StepID: request.StepID, AttemptID: request.AttemptID,
		ContractSHA: request.ContractSHA, BundleManifestSHA: request.Bundle.ManifestSHA,
		State: "completed", StartedAt: finished.Add(-time.Millisecond), FinishedAt: &finished,
		Commands:  []model.ExperimentCommandReceipt{{Argv: []string{"test-only-deterministic-runner"}, BinarySHA: binary.SHA, ExitCode: &exit, Termination: "exited", StdoutSHA: stdout.SHA, StdoutBytes: int64(len(stdoutBytes)), StdoutObservedBytes: int64(len(stdoutBytes)), StderrSHA: empty.SHA}},
		Artifacts: bundle, Observations: []model.ExperimentObservation{{Name: "safe", Type: "boolean", Value: value, SourcePath: "observations.json", SourceSHA: bundle.Files[0].ContentSHA}},
		Accepted: accepted, Conclusive: true, AttestationScope: "test-only deterministic adapter, not process execution",
		ExecutionOutcome: "completed", ObservationOutcome: "complete", HypothesisOutcome: hypothesis,
	}, nil
}
func (r *programTestRunner) Status(_ context.Context, id string) (model.ExperimentReceipt, error) {
	r.statuses = append(r.statuses, id)
	if r.statusError != nil {
		return model.ExperimentReceipt{}, r.statusError
	}
	receipt, ok := r.receipts[id]
	if !ok {
		return receipt, &executor.ExperimentError{Code: "experiment_not_found", Message: "test-only healthy durable journal has no intent for this run", Status: 404}
	}
	if receipt.State == "cancelled" || receipt.State == "resolved_failed" {
		return receipt, nil
	}
	if r.uncertain {
		receipt.State = "uncertain"
		receipt.Conclusive = false
		receipt.Accepted = false
		receipt.FinishedAt = nil
		receipt.FailureKind = "operation_uncertain"
		receipt.ExecutionOutcome = "uncertain"
		receipt.HypothesisOutcome = "inconclusive"
	}
	if r.hold && !r.uncertain {
		receipt.State = "running"
		receipt.Conclusive = false
		receipt.Accepted = false
		receipt.FinishedAt = nil
		receipt.ExecutionOutcome = "running"
		receipt.HypothesisOutcome = "pending"
	}
	return receipt, nil
}
func (r *programTestRunner) Cancel(_ context.Context, id string) (model.ExperimentReceipt, error) {
	r.cancelled = append(r.cancelled, id)
	receipt, ok := r.receipts[id]
	if !ok {
		return receipt, &executor.ExperimentError{Code: "experiment_not_found", Message: "test-only healthy durable journal has no intent for this run", Status: 404}
	}
	receipt.State = "cancelled"
	receipt.Accepted, receipt.Conclusive = false, false
	receipt.ExecutionOutcome, receipt.HypothesisOutcome = "cancelled", "inconclusive"
	r.receipts[id] = receipt
	return receipt, nil
}
func (r *programTestRunner) CancelIntent(ctx context.Context, request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	r.cancelIntents = append(r.cancelIntents, request)
	persisted, err := r.db.ProgramExperimentIntent(ctx, request.RunID)
	if err != nil || !reflect.DeepEqual(persisted, request) {
		return model.ExperimentReceipt{}, errors.New("test-only cancellation did not use exact controller-persisted request")
	}
	if prior, ok := r.acceptedRequests[request.RunID]; ok {
		if !reflect.DeepEqual(prior, request) {
			return model.ExperimentReceipt{}, &executor.ExperimentError{Code: "idempotency_conflict", Message: "test-only cancellation request changed", Status: 409}
		}
		return r.Cancel(ctx, request.RunID)
	}
	if r.statusError != nil {
		return model.ExperimentReceipt{}, r.statusError
	}
	if r.acceptedRequests == nil {
		r.acceptedRequests = map[string]model.ExperimentRunRequest{}
	}
	finished := time.Now().UTC()
	receipt := model.ExperimentReceipt{
		RunID: request.RunID, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID,
		Revision: request.Revision, StepID: request.StepID, AttemptID: request.AttemptID,
		ContractSHA: request.ContractSHA, BundleManifestSHA: request.Bundle.ManifestSHA,
		State: "cancelled", FinishedAt: &finished, FailureKind: "cancelled_before_start",
		ExecutionOutcome: "cancelled", ObservationOutcome: "not_evaluated", HypothesisOutcome: "inconclusive",
		AttestationScope: "test-only durable never-started cancellation; no process executed",
	}
	r.acceptedRequests[request.RunID], r.receipts[request.RunID] = request, receipt
	r.cancelled = append(r.cancelled, request.RunID)
	return receipt, nil
}
func (*programTestRunner) ResolveFailed(context.Context, string, string, string) (model.ExperimentReceipt, error) {
	return model.ExperimentReceipt{}, errors.New("test-only runner resolution must not be invoked automatically")
}

type programTestFixture struct {
	e          *Engine
	db         *store.Store
	backend    *programTestBackend
	repos      *programTestRepositories
	runner     *programTestRunner
	programIDs []string
}

func newProgramTestFixture(t *testing.T) *programTestFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := artifacts.New(filepath.Join(root, "artifacts"))
	repos := &programTestRepositories{root: filepath.Join(root, "discovery"), blobs: blobs, workspaces: map[string]model.DiscoveryWorkspace{}}
	backend := &programTestBackend{db: db, repos: repos, sessions: map[string]model.Session{}, steps: map[string]model.ProgramStep{}, stopped: map[string]bool{}}
	runner := &programTestRunner{db: db, root: filepath.Join(root, "test-only-runner"), blobs: blobs, receipts: map[string]model.ExperimentReceipt{}}
	cfg := model.Config{StateDir: root, PolicyHash: "test-only-discovery-policy", AstraModel: "test-only/deterministic-discovery", MaxConcurrent: 2, MaxAttempts: 2, MaxCallsPerDay: 100, AttemptTimeoutSeconds: 3600, MaxPromptBytes: 1 << 20, Discovery: model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"test-only-deterministic": {Argv: []string{"test-only-deterministic-runner"}}}}), Projects: map[string]model.Project{"demo": {Trusted: true, Repository: filepath.Join(root, "enrolled-product"), Branch: "main", Checks: map[string]model.Command{"unit": {Argv: []string{"go", "test", "./..."}}}}}}
	runner.executionPolicySHA = model.ExperimentPolicySHA(cfg.Discovery.ExperimentProfiles, cfg.Discovery.ExperimentLimits)
	engine := New(cfg, db, backend, repos)
	engine.experiments = runner
	fixture := &programTestFixture{e: engine, db: db, backend: backend, repos: repos, runner: runner}
	t.Cleanup(func() { fixture.db.Close() })
	return fixture
}
func (f *programTestFixture) importProgram(t *testing.T, id string, limits model.ProgramLimits, approve bool) model.ProgramRevision {
	t.Helper()
	spec := model.ProgramSpec{ID: id, Project: "demo", Title: "Test-only ordering discovery", Objective: "Evaluate duplicate and reordered delivery before proposing implementation", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"test-only"}}, Limits: limits, Implementation: model.ProgramImplementationPolicy{PermittedFeatureKinds: []string{"feature"}, PermittedCompletionTargets: []string{"integrated"}}}
	revision, err := f.db.ImportProgram(context.Background(), spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	f.programIDs = append(f.programIDs, id)
	if approve {
		if err := f.db.ApproveProgram(context.Background(), id, revision.RevisionHash, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
			t.Fatal(err)
		}
	}
	return revision
}
func (f *programTestFixture) status(t *testing.T, id string) store.ProgramStatus {
	t.Helper()
	status, err := f.db.ProgramStatus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return status
}
func (f *programTestFixture) diagnostics() string {
	ctx := context.Background()
	var out strings.Builder
	fmt.Fprintf(&out, "model_calls=%d runner_starts=%d", len(f.backend.requests), len(f.runner.starts))
	for _, id := range f.programIDs {
		status, err := f.db.ProgramStatus(ctx, id)
		if err != nil {
			fmt.Fprintf(&out, "\nprogram=%s status_error=%q", id, err)
			continue
		}
		program := status.Program
		fmt.Fprintf(&out, "\nprogram=%s state=%s phase=%s revision=%d iteration=%d budget=%+v", id, program.State, program.Phase, program.CurrentRevision, program.Iteration, status.Budget)
		if program.Wait != nil {
			fmt.Fprintf(&out, " wait_code=%q wait_detail=%q", program.Wait.Code, program.Wait.Detail)
		}
		for _, step := range status.Steps {
			fmt.Fprintf(&out, "\n  step=%s kind=%s iteration=%d state=%s feature=%s", step.ID, step.Kind, step.Iteration, step.State, step.FeatureID)
			feature, err := f.db.Get(ctx, step.FeatureID)
			if err != nil {
				fmt.Fprintf(&out, " feature_error=%q", err)
				continue
			}
			fmt.Fprintf(&out, " feature_state=%s block_reason=%q attempts=%d", feature.State, feature.Reason, feature.Attempts)
		}
	}
	active, err := f.db.Active(ctx)
	if err != nil {
		fmt.Fprintf(&out, "\nactive_error=%q", err)
	} else {
		fmt.Fprintf(&out, "\nactive_attempts=%d", len(active))
		for _, attempt := range active {
			fmt.Fprintf(&out, "\n  attempt=%s feature=%s state=%s stage=%s", attempt.ID, attempt.FeatureID, attempt.State, attempt.Stage)
			if attempt.Wait != nil {
				fmt.Fprintf(&out, " wait_code=%q wait_detail=%q", attempt.Wait.Code, attempt.Wait.Detail)
			}
		}
	}
	return out.String()
}
func (f *programTestFixture) until(t *testing.T, description string, done func() bool) {
	t.Helper()
	for range 160 {
		if done() {
			return
		}
		if err := f.e.Tick(context.Background()); err != nil {
			t.Fatalf("%s: %v\n%s", description, err, f.diagnostics())
		}
	}
	t.Fatalf("discovery failed to reach %s\n%s", description, f.diagnostics())
}
func (f *programTestFixture) approvalGate(t *testing.T, id string, iteration int) {
	t.Helper()
	f.until(t, "plan evaluation before exact approval", func() bool {
		for _, step := range f.status(t, id).Steps {
			if step.Kind == model.StepPlanEvaluation && step.Iteration == iteration && step.State == "completed" {
				return true
			}
		}
		return false
	})
	ticks(t, f.e, 3)
}
func (f *programTestFixture) approveIteration(t *testing.T, id string, iteration int) {
	t.Helper()
	page, err := f.db.ProgramArtifactPage(context.Background(), id, store.ProgramQuery{Kind: model.ArtifactArchitecture, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, artifact := range page.Items {
		if artifact.Iteration != iteration {
			continue
		}
		found = true
		if err := f.db.ApproveProgramArtifact(context.Background(), id, f.status(t, id).Revision.RevisionHash, model.ProgramArtifactKind(artifact.Kind), artifact.ContentSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatal("no architecture to approve")
	}
	experiments, err := f.db.ProgramExperimentPage(context.Background(), id, store.ProgramQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, experiment := range experiments.Items {
		if experiment.Artifact.Iteration == iteration {
			if err := f.db.ApproveProgramArtifact(context.Background(), id, f.status(t, id).Revision.RevisionHash, model.ArtifactExperimentContracts, experiment.ContractSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func (f *programTestFixture) publish(t *testing.T, id string) {
	t.Helper()
	f.until(t, "unapproved proposal publication", func() bool { return f.status(t, id).Program.State == model.ProgramProposed })
}
func (f *programTestFixture) restart(t *testing.T) {
	t.Helper()
	cfg := f.e.cfg
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(cfg.StateDir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	f.db, f.backend.db, f.runner.db = db, db, db
	f.e = New(cfg, db, f.backend, f.repos)
	f.e.experiments = f.runner
}
func programTestError(t *testing.T, err error, code string) {
	t.Helper()
	var programError *model.ProgramError
	if !errors.As(err, &programError) || programError.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestProgramGeneratedResearchFeatureSatisfiesAdmissionContract(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "research-admission", model.ProgramLimits{}, true)
	if err := f.e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := f.status(t, "research-admission")
	if len(status.Steps) != 1 || status.Steps[0].Kind != model.StepResearch {
		t.Fatalf("expected initial research step\n%s", f.diagnostics())
	}
	feature := state(t, f.db, status.Steps[0].FeatureID)
	if err := f.e.validate(feature); err != nil {
		t.Fatalf("server-generated discovery contract fails ordinary admission: %v\n%s", err, f.diagnostics())
	}
	if feature.State != "running" || feature.Attempts != 1 {
		t.Fatalf("valid research contract was not admitted\n%s", f.diagnostics())
	}
}

func TestProgramFullGraphPublishesOnlyUnapprovedProduct(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "full-graph", model.ProgramLimits{}, true)
	f.approvalGate(t, "full-graph", 0)
	if len(f.backend.requests) != 3 || len(f.runner.starts) != 0 {
		t.Fatal("experiment escaped exact approval gate")
	}
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 0 {
		t.Fatalf("approval wait consumed capacity: %+v %v", active, err)
	}
	f.approveIteration(t, "full-graph", 0)
	f.publish(t, "full-graph")
	status := f.status(t, "full-graph")
	if len(status.Steps) != 7 || status.Budget.ModelCalls != 6 || status.Budget.Experiments != 1 || len(f.runner.starts) != 1 {
		t.Fatalf("wrong fixed graph or budgets\n%s", f.diagnostics())
	}
	kinds := []string{}
	threads := map[string]bool{}
	for _, step := range status.Steps {
		kinds = append(kinds, step.Kind)
		if step.State != "completed" || step.OutputManifestSHA == "" {
			t.Fatalf("nonterminal persisted step: %+v", step)
		}
		feature := state(t, f.db, step.FeatureID)
		if feature.State != "reviewed" || len(feature.Checks) != 0 || feature.DiscoveryStepID != step.ID {
			t.Fatalf("discovery used ordinary candidate proof: %+v", feature)
		}
	}
	sort.Strings(kinds)
	want := []string{model.StepResearch, model.StepArchitecture, model.StepPlanEvaluation, model.StepExperimentRun, model.StepResultEvaluation, model.StepContracts, model.StepContractEvaluation}
	sort.Strings(want)
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("wrong graph: %v", kinds)
	}
	for _, session := range f.backend.sessions {
		if threads[session.ThreadID] {
			t.Fatal("independent discovery roles reused a thread")
		}
		threads[session.ThreadID] = true
	}
	product := state(t, f.db, "program-product-full-graph")
	if product.State != "proposed" || product.PolicyHash != "" || product.Attempts != 0 || product.Receipt != "" || product.ProgramEvidenceGate == nil || len(product.DependsOn) != 0 {
		t.Fatalf("discovery autoapproved product or used code dependencies: %+v", product)
	}
	before := status.Budget
	f.restart(t)
	ticks(t, f.e, 8)
	if got := f.status(t, "full-graph"); got.Budget != before || len(got.Steps) != 7 || len(f.backend.requests) != 6 || len(f.runner.starts) != 1 {
		t.Fatalf("idle/restarted coordinator duplicated work\n%s", f.diagnostics())
	}
	if f.repos.checks != 0 || f.repos.commits != 0 || f.repos.integrations != 0 || f.repos.prs != 0 || f.repos.releases != 0 {
		t.Fatal("discovery manufactured ordinary product checks/candidates")
	}
	for _, project := range f.backend.projects {
		if project != [2]string{"discovery-demo", filepath.Join(f.repos.root, "demo")} {
			t.Fatalf("discovery BB project root was not stable: %v", project)
		}
	}
	if len(f.repos.sessions) != 6 || len(f.repos.frozen) == 0 {
		t.Fatalf("missing actual discovery sessions/bundle freeze: %+v", f.repos.sessions)
	}
	for _, session := range f.repos.sessions {
		if session.ProgramID != "full-graph" || session.AttemptID == "" || session.CapabilityToken == "" || session.Deadline.IsZero() {
			t.Fatalf("missing attempt-bound control session: %+v", session)
		}
		path := filepath.Join(f.repos.workspaces[session.StepID].Path, ".openfactory", "discovery-session.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var persisted model.DiscoverySession
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		expected := session
		expected.InputFiles = nil
		if err != nil || info.Mode().Perm() != 0600 || !reflect.DeepEqual(persisted, expected) {
			t.Fatalf("discovery session was not privately persisted for its exact attempt: %v", err)
		}
		if session.Role == model.StepResearch || strings.HasSuffix(session.Role, "evaluation") {
			if !session.ReadOnly {
				t.Fatalf("analysis authority not read-only: %+v", session)
			}
		}
		for _, request := range f.backend.requests {
			if strings.Contains(request.Prompt, session.CapabilityToken) {
				t.Fatal("research capability leaked into model prompt")
			}
		}
	}
	for _, bundle := range f.repos.frozen {
		for _, member := range bundle.Files {
			if strings.HasPrefix(member.Path, ".openfactory/") {
				t.Fatal("control capability escaped into frozen bundle")
			}
		}
	}
}

func TestProgramApprovalsBindRevisionArchitectureAndEachFrozenExperiment(t *testing.T) {
	f := newProgramTestFixture(t)
	f.backend.multipleExperiments = true
	revision := f.importProgram(t, "approval-gates", model.ProgramLimits{}, false)
	ticks(t, f.e, 5)
	if len(f.backend.requests) != 0 || len(f.status(t, "approval-gates").Steps) != 0 {
		t.Fatal("unapproved program consumed work")
	}
	programTestError(t, f.db.ApproveProgram(context.Background(), "approval-gates", strings.Repeat("0", 64), f.e.cfg.PolicyHash, model.TrustedLocalAudit()), "stale_revision")
	if err := f.db.ApproveProgram(context.Background(), "approval-gates", revision.RevisionHash, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f.approvalGate(t, "approval-gates", 0)
	beforeUnknown := f.status(t, "approval-gates")
	programTestError(t, f.db.ApproveProgramArtifact(context.Background(), "approval-gates", beforeUnknown.Revision.RevisionHash, model.ArtifactExperimentContracts, strings.Repeat("f", 64), f.e.cfg.PolicyHash, model.TrustedLocalAudit()), "evidence_invalid")
	if after := f.status(t, "approval-gates"); !reflect.DeepEqual(after.Grants, beforeUnknown.Grants) || len(f.runner.starts) != 0 {
		t.Fatal("unknown frozen experiment approval granted authority or started execution")
	}
	experiments, err := f.db.ProgramExperimentPage(context.Background(), "approval-gates", store.ProgramQuery{})
	if err != nil || len(experiments.Items) != 2 {
		t.Fatalf("experiment contracts missing: %+v %v", experiments, err)
	}
	t.Run("reject stale revision and mismatched artifact kind", func(t *testing.T) {
		before := f.status(t, "approval-gates")
		programTestError(t, f.db.ApproveProgramArtifact(context.Background(), "approval-gates", strings.Repeat("0", 64), model.ArtifactExperimentContracts, experiments.Items[0].ContractSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()), "stale_revision")
		programTestError(t, f.db.ApproveProgramArtifact(context.Background(), "approval-gates", before.Revision.RevisionHash, model.ArtifactArchitecture, experiments.Items[0].ContractSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()), "evidence_invalid")
		ticks(t, f.e, 5)
		if after := f.status(t, "approval-gates"); !reflect.DeepEqual(after.Grants, before.Grants) || len(f.runner.starts) != 0 || len(f.backend.requests) != 3 {
			t.Fatal("rejected artifact approval granted authority or advanced program work")
		}
	})
	if err := f.db.ApproveProgramArtifact(context.Background(), "approval-gates", f.status(t, "approval-gates").Revision.RevisionHash, model.ArtifactExperimentContracts, experiments.Items[0].ContractSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	ticks(t, f.e, 5)
	if len(f.runner.starts) != 0 || len(f.backend.requests) != 3 {
		t.Fatal("experiment approval implicitly approved architecture")
	}
	architectures, err := f.db.ProgramArtifactPage(context.Background(), "approval-gates", store.ProgramQuery{Kind: model.ArtifactArchitecture})
	if err != nil || len(architectures.Items) != 1 {
		t.Fatalf("architecture missing: %+v %v", architectures, err)
	}
	if err := f.db.ApproveProgramArtifact(context.Background(), "approval-gates", f.status(t, "approval-gates").Revision.RevisionHash, model.ProgramArtifactKind(architectures.Items[0].Kind), architectures.Items[0].ContentSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	ticks(t, f.e, 5)
	if len(f.runner.starts) != 0 {
		t.Fatal("one frozen contract approval authorized another experiment")
	}
	if err := f.db.ApproveProgramArtifact(context.Background(), "approval-gates", f.status(t, "approval-gates").Revision.RevisionHash, model.ArtifactExperimentContracts, experiments.Items[1].ContractSHA, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "approval-gates")
	if len(f.runner.starts) != 2 {
		t.Fatal("not every approved frozen experiment ran exactly once")
	}
}

func TestProgramFailedExperimentRequiresFreshRevisionAndApproval(t *testing.T) {
	f := newProgramTestFixture(t)
	f.backend.reviseFirst, f.runner.failFirst = true, true
	f.importProgram(t, "falsification", model.ProgramLimits{}, true)
	f.approvalGate(t, "falsification", 0)
	f.approveIteration(t, "falsification", 0)
	f.approvalGate(t, "falsification", 1)
	if len(f.runner.starts) != 1 {
		t.Fatal("revised experiment ran under superseded approval")
	}
	page, err := f.db.ProgramExperimentPage(context.Background(), "falsification", store.ProgramQuery{Limit: 100})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("revision discarded experiment history: %+v %v", page, err)
	}
	failed := false
	for _, experiment := range page.Items {
		for _, receipt := range experiment.Receipts {
			if receipt.Conclusive && !receipt.Accepted && receipt.HypothesisOutcome == "falsified" {
				failed = true
			}
		}
	}
	if !failed {
		t.Fatal("failed hypothesis was not retained as actual conclusive evidence")
	}
	revisions, err := f.db.ProgramArtifactPage(context.Background(), "falsification", store.ProgramQuery{Kind: model.ArtifactRevision})
	if err != nil || len(revisions.Items) != 1 {
		t.Fatalf("replan lost immutable revision rationale: %+v %v", revisions, err)
	}
	_, raw, err := f.db.ProgramArtifactPayload(context.Background(), revisions.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var rationale model.RevisionPayload
	if err := model.StrictDecode(raw, &rationale); err != nil {
		t.Fatal(err)
	}
	if len(rationale.FalsifiedEvidence) != 1 || rationale.FalsifiedEvidence[0].ID != f.runner.starts[0].RunID || len(rationale.ChangedDecisions) != 1 || rationale.PredecessorManifestSHA == "" {
		t.Fatalf("revision is not traced to actual falsified evidence: %+v", rationale)
	}
	for _, experiment := range page.Items {
		if experiment.Artifact.Iteration == 1 && experiment.Artifact.SupersedesID == "" {
			t.Fatal("new frozen experiments lost their immutable predecessor")
		}
	}
	f.approveIteration(t, "falsification", 1)
	f.publish(t, "falsification")
	status := f.status(t, "falsification")
	if len(f.runner.starts) != 2 || status.Budget.ModelCalls != 10 || status.Budget.Experiments != 2 || status.Budget.Steps != 12 {
		t.Fatalf("revision reset lifetime budgets or skipped independent evaluation\n%s", f.diagnostics())
	}
	if f.runner.starts[0].ContractSHA == f.runner.starts[1].ContractSHA || f.runner.starts[0].Bundle.ManifestSHA == f.runner.starts[1].Bundle.ManifestSHA {
		t.Fatal("revision reused a mutable or superseded experiment contract")
	}
}

func TestProgramDecisiveQuestionAndStaleAnswerCannotGrantAuthority(t *testing.T) {
	f := newProgramTestFixture(t)
	f.backend.decisive = true
	revision := f.importProgram(t, "question-gate", model.ProgramLimits{}, true)
	f.until(t, "decisive question", func() bool {
		page, err := f.db.ProgramQuestionPage(context.Background(), "question-gate", store.ProgramQuery{})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Items) == 1
	})
	ticks(t, f.e, 8)
	if len(f.backend.requests) != 1 || len(f.runner.starts) != 0 {
		t.Fatal("unanswered decisive question allowed downstream model/experiment work")
	}
	questions, err := f.db.ProgramQuestionPage(context.Background(), "question-gate", store.ProgramQuery{})
	if err != nil {
		t.Fatal(err)
	}
	question := questions.Items[0]
	answer := model.ProgramAnswer{ProgramID: "question-gate", RevisionHash: revision.RevisionHash, QuestionArtifactID: question.Artifact.ID, QuestionArtifactSHA: strings.Repeat("0", 64), QuestionID: question.Question.ID, Text: "Acknowledgement must survive process loss", Actor: "test-operator"}
	_, err = f.db.AnswerProgramQuestion(context.Background(), "question-gate", revision.RevisionHash, answer, model.TrustedLocalAudit())
	programTestError(t, err, "stale_revision")
	answer.QuestionArtifactSHA = question.Artifact.ContentSHA
	updated, err := f.db.AnswerProgramQuestion(context.Background(), "question-gate", revision.RevisionHash, answer, model.TrustedLocalAudit())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Number <= revision.Number || updated.RevisionHash == revision.RevisionHash {
		t.Fatal("authoritative answer did not create a fresh revision")
	}
	ticks(t, f.e, 5)
	if len(f.backend.requests) != 1 || f.status(t, "question-gate").Budget.ModelCalls != 1 {
		t.Fatal("answer inherited program approval or reset budget")
	}
	_, err = f.db.AnswerProgramQuestion(context.Background(), "question-gate", revision.RevisionHash, answer, model.TrustedLocalAudit())
	programTestError(t, err, "stale_revision")
	programTestError(t, f.db.ApproveProgram(context.Background(), "question-gate", revision.RevisionHash, f.e.cfg.PolicyHash, model.TrustedLocalAudit()), "stale_revision")
	if err := f.db.ApproveProgram(context.Background(), "question-gate", updated.RevisionHash, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f.approvalGate(t, "question-gate", 0)
	if len(f.backend.requests) != 4 || f.status(t, "question-gate").Budget.ModelCalls != 4 || len(f.runner.starts) != 0 {
		t.Fatal("fresh answer revision reset lifetime calls or implicitly approved experiments")
	}
	f.approveIteration(t, "question-gate", 0)
	f.publish(t, "question-gate")
	if f.status(t, "question-gate").Budget.ModelCalls != 7 {
		t.Fatal("answer revision discarded the predecessor's model usage")
	}
}

func TestProgramCallBudgetsAndStepCapsAreLifetimeBounds(t *testing.T) {
	for _, scope := range []string{"global-calls", "program-calls", "program-steps", "policy-calls"} {
		t.Run(scope, func(t *testing.T) {
			f := newProgramTestFixture(t)
			limits := model.ProgramLimits{}
			switch scope {
			case "global-calls":
				f.e.cfg.MaxCallsPerDay = 1
			case "program-calls":
				limits.MaxModelCalls = 1
			case "program-steps":
				limits.MaxSteps = 1
			case "policy-calls":
				f.e.cfg.Discovery.Limits.MaxModelCalls = 1
			}
			revision := f.importProgram(t, scope, limits, true)
			ticks(t, f.e, 24)
			before := f.status(t, scope)
			if len(f.backend.requests) != 1 || before.Budget.ModelCalls != 1 || len(f.runner.starts) != 0 {
				t.Fatalf("%s exceeded bound\n%s", scope, f.diagnostics())
			}
			if scope == "program-steps" && before.Budget.Steps != 1 {
				t.Fatalf("step budget exceeded: %+v", before.Budget)
			}
			beforeActive, err := f.db.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			beforeCalls, err := f.db.Status(context.Background(), model.StatusQuery{}, f.e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if beforeCalls.Calls.Used != 1 || (scope == "global-calls" && len(beforeActive) != 1) {
				t.Fatal("budget fixture lacks the charged call or unsettled daily-limit owner")
			}
			resumeErr := f.db.ResumeProgram(context.Background(), scope, revision.RevisionHash, f.e.cfg, model.TrustedLocalAudit())
			switch scope {
			case "global-calls":
				programTestError(t, resumeErr, "attempt_unsettled")
			case "program-calls", "policy-calls":
				programTestError(t, resumeErr, "budget_exhausted")
			case "program-steps":
				if resumeErr != nil {
					t.Fatal(resumeErr)
				}
			}
			afterResumeActive, err := f.db.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			afterResume := f.status(t, scope)
			if !reflect.DeepEqual(afterResumeActive, beforeActive) || afterResume.Budget != before.Budget || (resumeErr != nil && !reflect.DeepEqual(afterResume.Grants, before.Grants)) {
				t.Fatal("resume changed owned attempts, lifetime usage, or authority at an exhausted gate")
			}
			f.restart(t)
			ticks(t, f.e, 8)
			if after := f.status(t, scope); after.Budget != before.Budget || len(f.backend.requests) != 1 {
				t.Fatalf("resume/restart reset exhausted budget: before=%+v after=%+v\n%s", before.Budget, after.Budget, f.diagnostics())
			}
			afterCalls, err := f.db.Status(context.Background(), model.StatusQuery{}, f.e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			afterActive, err := f.db.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterCalls.Calls, beforeCalls.Calls) || len(afterActive) != len(beforeActive) || len(f.runner.starts) != 0 {
				t.Fatal("resume/restart changed daily reservations or released active ownership")
			}
			for _, old := range beforeActive {
				found := false
				for _, current := range afterActive {
					if current.ID == old.ID {
						found = current.FeatureID == old.FeatureID && current.DispatchKey == old.DispatchKey && current.ThreadID == old.ThreadID && current.Deadline.Equal(old.Deadline)
					}
				}
				if !found {
					t.Fatal("resume/restart replaced or extended an owned attempt")
				}
			}
		})
	}
}

func TestProgramSharesGlobalAndProjectAttemptCapacity(t *testing.T) {
	for _, scope := range []string{"global", "project"} {
		t.Run(scope, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.backend.idle = true
			project := f.e.cfg.Projects["demo"]
			project.MaxConcurrent = 2
			f.e.cfg.Projects["demo"] = project
			if scope == "global" {
				f.e.cfg.MaxConcurrent = 1
			} else {
				project.MaxConcurrent = 1
				f.e.cfg.Projects["demo"] = project
			}
			f.importProgram(t, "capacity-a", model.ProgramLimits{}, true)
			f.importProgram(t, "capacity-b", model.ProgramLimits{}, true)
			ticks(t, f.e, 8)
			active, err := f.db.Active(context.Background())
			if err != nil || len(active) != 1 || len(f.backend.requests) != 1 {
				t.Fatalf("%s capacity bypassed: err=%v\n%s", scope, err, f.diagnostics())
			}
			if err := f.db.CancelProgram(context.Background(), "capacity-a", f.status(t, "capacity-a").Revision.RevisionHash, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
			f.until(t, "released capacity for second program", func() bool { return len(f.backend.requests) == 2 })
		})
	}
}

func TestProgramRestartReconcilesSpawnWithoutDuplicateReservation(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-session-%t", missing), func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.backend.spawnError, f.backend.findMissing = true, missing
			f.importProgram(t, "spawn-restart", model.ProgramLimits{}, true)
			f.until(t, "uncertain spawn intent", func() bool { return len(f.backend.requests) == 1 })
			f.restart(t)
			ticks(t, f.e, 12)
			if missing {
				active, err := f.db.Active(context.Background())
				if err != nil || len(active) != 1 || active[0].Stage != "uncertain" || len(f.backend.requests) != 1 {
					t.Fatalf("unresolved spawn lost ownership or replayed: %+v %v", active, err)
				}
				if f.status(t, "spawn-restart").Budget.ModelCalls != 1 {
					t.Fatal("uncertain spawn refunded a potentially spent call")
				}
			} else {
				f.approvalGate(t, "spawn-restart", 0)
				if len(f.backend.requests) != 3 || f.status(t, "spawn-restart").Budget.ModelCalls != 3 {
					t.Fatal("spawn recovery duplicated a session or model reservation")
				}
			}
		})
	}
}

func TestProgramRestartDoesNotReexecuteKnownRunnerIntent(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("uncertain-%t", uncertain), func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.runner.loseStart, f.runner.uncertain = true, uncertain
			f.importProgram(t, "runner-restart", model.ProgramLimits{}, true)
			f.approvalGate(t, "runner-restart", 0)
			f.approveIteration(t, "runner-restart", 0)
			f.until(t, "durable runner start", func() bool { return len(f.runner.starts) == 1 })
			f.restart(t)
			if uncertain {
				ticks(t, f.e, 12)
				active, err := f.db.Active(context.Background())
				if err != nil || len(active) != 1 || active[0].Stage != "uncertain" {
					t.Fatalf("uncertain runner released its owned lane: %+v %v", active, err)
				}
				if _, err := f.db.Get(context.Background(), "program-product-runner-restart"); err == nil {
					t.Fatal("uncertain execution became product proof")
				}
				if err := f.db.Retry(context.Background(), active[0].FeatureID, model.TrustedLocalAudit()); err == nil {
					t.Fatal("retry discarded uncertain experiment ownership")
				}
				if err := f.db.CancelProgram(context.Background(), "runner-restart", f.status(t, "runner-restart").Revision.RevisionHash, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
				ticks(t, f.e, 4)
				active, err = f.db.Active(context.Background())
				if err != nil || len(active) != 1 || active[0].Stage != "uncertain" {
					t.Fatalf("cancellation freed uncertain runner lane without resolution: %+v %v", active, err)
				}
			} else {
				f.publish(t, "runner-restart")
			}
			if f.runner.processes[f.runner.starts[0].RunID] != 1 || len(f.runner.processes) != 1 || len(f.runner.statuses) == 0 {
				t.Fatal("restart reexecuted a process instead of recovering exact runner identity")
			}
			for _, id := range f.runner.statuses {
				if id != f.runner.starts[0].RunID {
					t.Fatal("recovery queried a different run")
				}
			}
		})
	}
}

func TestProgramCancellationPolicyAndEvidenceFenceRunningAnalysis(t *testing.T) {
	for _, boundary := range []string{"cancel", "policy", "evidence", "readonly"} {
		t.Run(boundary, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.backend.idle = true
			revision := f.importProgram(t, "analysis-fence", model.ProgramLimits{}, true)
			f.until(t, "running research", func() bool {
				active, err := f.db.Active(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return len(active) == 1 && active[0].ThreadID != "" && strings.HasSuffix(active[0].Stage, "_running")
			})
			switch boundary {
			case "cancel":
				if err := f.db.CancelProgram(context.Background(), "analysis-fence", revision.RevisionHash, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			case "policy":
				f.e.cfg.PolicyHash = "test-only-revoked-policy"
			case "evidence":
				current := f.status(t, "analysis-fence")
				if len(current.Steps) != 1 {
					t.Fatalf("expected the single active research input manifest, got %d steps", len(current.Steps))
				}
				if err := f.db.InvalidateProgramEvidence(context.Background(), "analysis-fence", current.Steps[0].InputManifestSHA, "test-only invalidated input", model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			case "readonly":
				f.backend.mutateReadOnly = true
			}
			f.backend.idle = false
			ticks(t, f.e, 12)
			if len(f.backend.requests) != 1 || len(f.runner.starts) != 0 {
				t.Fatal("revoked/read-only analysis advanced graph")
			}
			if boundary == "evidence" {
				for _, session := range f.backend.sessions {
					if !f.backend.stopped[session.ThreadID] {
						t.Fatal("evidence invalidation did not cancel the running model session")
					}
				}
				active, err := f.db.Active(context.Background())
				if err != nil || len(active) != 0 {
					t.Fatalf("invalidated analysis retained active authority: %+v %v", active, err)
				}
			}
			page, err := f.db.ProgramArtifactPage(context.Background(), "analysis-fence", store.ProgramQuery{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("unauthorized completion published artifacts: %+v %v", page, err)
			}
			if boundary == "cancel" && f.status(t, "analysis-fence").Program.State != model.ProgramCancelled {
				t.Fatal("program cancellation was not durable")
			}
		})
	}
}

func TestProgramPublishedEvidenceInvalidationFencesProductApproval(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "publication-fence", model.ProgramLimits{}, true)
	f.approvalGate(t, "publication-fence", 0)
	f.approveIteration(t, "publication-fence", 0)
	f.publish(t, "publication-fence")
	product := state(t, f.db, "program-product-publication-fence")
	if err := f.db.InvalidateProgramEvidence(context.Background(), "publication-fence", product.ProgramEvidenceGate.ManifestSHA, "Operator invalidated evaluated evidence", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	product = state(t, f.db, product.ID)
	if product.State != "blocked" || product.Reason != "evidence_invalid" || product.Attempts != 0 || product.PolicyHash != "" {
		t.Fatalf("invalidation did not immediately fence product authority: %+v", product)
	}
	grants := f.status(t, "publication-fence").Grants
	if err := f.db.Approve(context.Background(), product.ID, product.Revision, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err == nil {
		t.Fatal("invalidated publication evidence approved a product feature")
	}
	ticks(t, f.e, 5)
	if after := state(t, f.db, product.ID); !reflect.DeepEqual(after, product) {
		t.Fatalf("rejected approval mutated blocked product: before=%+v after=%+v", product, after)
	}
	if after := f.status(t, "publication-fence").Grants; !reflect.DeepEqual(after, grants) {
		t.Fatalf("rejected approval changed program grants: before=%+v after=%+v", grants, after)
	}
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 0 {
		t.Fatalf("invalidated publication admitted active authority: %+v %v", active, err)
	}
	if len(f.backend.requests) != 6 || f.repos.integrations != 0 {
		t.Fatal("invalidated discovery output launched product execution")
	}
}

var _ model.Backend = (*programTestBackend)(nil)
var _ model.DiscoveryRepositories = (*programTestRepositories)(nil)
var _ model.ExperimentRunner = (*programTestRunner)(nil)

func TestProgramExperimentAndIterationCapsDoNotResetOnResume(t *testing.T) {
	for _, boundary := range []string{"experiments", "iterations"} {
		t.Run(boundary, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.backend.reviseAll, f.runner.failAll = true, true
			limits := model.ProgramLimits{MaxIterations: 1}
			if boundary == "experiments" {
				limits.MaxExperiments = 1
			}
			revision := f.importProgram(t, "bounded-revision", limits, true)
			f.approvalGate(t, "bounded-revision", 0)
			f.approveIteration(t, "bounded-revision", 0)
			f.approvalGate(t, "bounded-revision", 1)
			f.approveIteration(t, "bounded-revision", 1)
			if boundary == "iterations" {
				f.until(t, "lifetime revision limit", func() bool {
					work, err := f.db.ProgramWork(context.Background(), f.e.cfg)
					if err != nil {
						t.Fatal(err)
					}
					for _, item := range work {
						if item.Program.ID == "bounded-revision" && item.Wait != nil && item.Wait.Code == "iteration_limit" {
							return true
						}
					}
					return false
				})
			}
			ticks(t, f.e, 12)
			before := f.status(t, "bounded-revision")
			wantExperiments := 2
			if boundary == "experiments" {
				wantExperiments = 1
			}
			if len(f.runner.starts) != wantExperiments || before.Budget.Experiments != wantExperiments {
				t.Fatalf("%s bound exceeded\n%s", boundary, f.diagnostics())
			}
			for _, step := range before.Steps {
				if step.Iteration > 1 {
					t.Fatalf("revision cap created iteration %d", step.Iteration)
				}
			}
			if _, err := f.db.Get(context.Background(), "program-product-bounded-revision"); err == nil {
				t.Fatal("bounded failed hypotheses became product proposals")
			}
			if err := f.db.ResumeProgram(context.Background(), "bounded-revision", revision.RevisionHash, f.e.cfg, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
			f.restart(t)
			ticks(t, f.e, 10)
			if after := f.status(t, "bounded-revision"); after.Budget != before.Budget || len(f.runner.starts) != wantExperiments {
				t.Fatalf("resume/restart reset %s bound: before=%+v after=%+v\n%s", boundary, before.Budget, after.Budget, f.diagnostics())
			}
		})
	}
}

func TestProgramMissingExperimentProfileWaitsWithoutOwnedCapacity(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "missing-profile", model.ProgramLimits{}, true)
	f.approvalGate(t, "missing-profile", 0)
	f.approveIteration(t, "missing-profile", 0)
	delete(f.e.cfg.Discovery.ExperimentProfiles, "test-only-deterministic")
	ticks(t, f.e, 10)
	work, err := f.db.ProgramWork(context.Background(), f.e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	waiting := false
	for _, item := range work {
		if item.Program.ID == "missing-profile" && item.Wait != nil && item.Wait.Code == "prerequisite_missing" {
			waiting = true
		}
	}
	active, err := f.db.Active(context.Background())
	if err != nil || !waiting || len(active) != 0 || len(f.runner.starts) != 0 {
		t.Fatalf("missing profile consumed capacity or manufactured execution: wait=%t active=%+v err=%v", waiting, active, err)
	}
	if f.status(t, "missing-profile").Budget.Experiments != 0 {
		t.Fatal("missing prerequisite consumed an experiment")
	}
}

func TestProgramRunningExperimentCancellationSettlesRunnerFirst(t *testing.T) {
	f := newProgramTestFixture(t)
	f.runner.hold = true
	revision := f.importProgram(t, "cancel-experiment", model.ProgramLimits{}, true)
	f.approvalGate(t, "cancel-experiment", 0)
	f.approveIteration(t, "cancel-experiment", 0)
	f.until(t, "running experiment", func() bool {
		active, err := f.db.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, attempt := range active {
			if attempt.Stage == "experiment_running" {
				return true
			}
		}
		return false
	})
	if err := f.db.CancelProgram(context.Background(), "cancel-experiment", revision.RevisionHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f.until(t, "runner cancellation settlement", func() bool {
		active, err := f.db.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(active) == 0
	})
	if len(f.runner.cancelled) != 1 || f.runner.cancelled[0] != f.runner.starts[0].RunID {
		t.Fatal("controller released experiment ownership without cancelling the exact runner identity")
	}
	if len(f.runner.cancelIntents) != 1 || !reflect.DeepEqual(f.runner.cancelIntents[0], f.runner.starts[0]) {
		t.Fatal("running cancellation omitted exact persisted execution policy and request")
	}
	if len(f.backend.requests) != 3 || len(f.runner.starts) != 1 {
		t.Fatal("cancellation advanced downstream analysis or replayed execution")
	}
	page, err := f.db.ProgramExperimentPage(context.Background(), "cancel-experiment", store.ProgramQuery{})
	if err != nil || len(page.Items) != 1 || len(page.Items[0].Receipts) != 1 || page.Items[0].Receipts[0].State != "cancelled" || page.Items[0].Receipts[0].Conclusive {
		t.Fatalf("cancellation was not persisted as non-evidence: %+v %v", page, err)
	}
	f.restart(t)
	ticks(t, f.e, 5)
	if len(f.runner.starts) != 1 || f.status(t, "cancel-experiment").Program.State != model.ProgramCancelled {
		t.Fatal("restart revived cancelled experiment")
	}
}

func TestProgramRevokedArtifactApprovalsCannotLaunchExperiment(t *testing.T) {
	for _, subject := range []string{"architecture", "contract", "policy"} {
		t.Run(subject, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.importProgram(t, "revoked-approval", model.ProgramLimits{}, true)
			f.approvalGate(t, "revoked-approval", 0)
			f.approveIteration(t, "revoked-approval", 0)
			switch subject {
			case "policy":
				f.e.cfg.PolicyHash = "test-only-new-policy"
			case "architecture":
				page, err := f.db.ProgramArtifactPage(context.Background(), "revoked-approval", store.ProgramQuery{Kind: model.ArtifactArchitecture})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.InvalidateProgramEvidence(context.Background(), "revoked-approval", page.Items[0].ContentSHA, "Architecture assumption invalidated", model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			case "contract":
				page, err := f.db.ProgramExperimentPage(context.Background(), "revoked-approval", store.ProgramQuery{})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.InvalidateProgramEvidence(context.Background(), "revoked-approval", page.Items[0].ContractSHA, "Frozen experiment invalidated", model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			}
			f.restart(t)
			ticks(t, f.e, 10)
			if len(f.runner.starts) != 0 || len(f.backend.requests) != 3 {
				t.Fatal("revoked approval launched experiment or downstream analysis")
			}
			active, err := f.db.Active(context.Background())
			if err != nil || len(active) != 0 {
				t.Fatalf("revoked approval consumed capacity: %+v %v", active, err)
			}
		})
	}
}

func TestProgramEvaluationRequiresFreshThreadAndExactManifest(t *testing.T) {
	for _, boundary := range []string{"author-thread", "stale-manifest"} {
		t.Run(boundary, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.backend.reuseEvaluator = boundary == "author-thread"
			f.backend.staleEvaluation = boundary == "stale-manifest"
			f.importProgram(t, "independent-evaluation", model.ProgramLimits{}, true)
			ticks(t, f.e, 24)
			if len(f.backend.requests) != 3 || len(f.runner.starts) != 0 {
				t.Fatal("invalid independent evaluation advanced graph")
			}
			page, err := f.db.ProgramArtifactPage(context.Background(), "independent-evaluation", store.ProgramQuery{Kind: model.ArtifactEvaluation})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("unreviewed/self-authored evaluation became persisted proof: %+v %v", page, err)
			}
			active, err := f.db.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "author-thread" && (len(active) != 1 || active[0].Stage != "uncertain") {
				t.Fatalf("reused evaluator session lost uncertain ownership: %+v", active)
			}
		})
	}
}

func TestProgramRevocationCancelsRunningExperimentBeforeReleasingLane(t *testing.T) {
	for _, boundary := range []string{"policy", "evidence"} {
		t.Run(boundary, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.runner.hold = true
			f.importProgram(t, "running-revocation", model.ProgramLimits{}, true)
			f.approvalGate(t, "running-revocation", 0)
			f.approveIteration(t, "running-revocation", 0)
			f.until(t, "running experiment before revocation", func() bool {
				active, err := f.db.Active(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, attempt := range active {
					if attempt.Stage == "experiment_running" {
						return true
					}
				}
				return false
			})
			if boundary == "policy" {
				f.e.cfg.PolicyHash = "test-only-revoked-running-policy"
			} else {
				if err := f.db.InvalidateProgramEvidence(context.Background(), "running-revocation", f.runner.starts[0].ContractSHA, "Observed evidence invalidated frozen experiment", model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			}
			ticks(t, f.e, 12)
			active, err := f.db.Active(context.Background())
			if err != nil || len(active) != 0 {
				t.Fatalf("settled revoked runner retained capacity: %+v %v", active, err)
			}
			if len(f.runner.cancelled) != 1 || f.runner.cancelled[0] != f.runner.starts[0].RunID {
				t.Fatal("revocation released owned lane before cancelling exact runner")
			}
			if len(f.runner.cancelIntents) != 1 || !reflect.DeepEqual(f.runner.cancelIntents[0], f.runner.starts[0]) {
				t.Fatal("revocation regenerated the request during cancellation")
			}
			if len(f.backend.requests) != 3 || len(f.runner.starts) != 1 {
				t.Fatal("revoked experiment spawned downstream work or replayed execution")
			}
			if _, err := f.db.Get(context.Background(), "program-product-running-revocation"); err == nil {
				t.Fatal("revoked execution became product evidence")
			}
		})
	}
}

func TestProgramRestartAfterModelReservationCannotBlindlySpawn(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "reserved-model", model.ProgramLimits{}, true)
	var attempt model.Attempt
	f.until(t, "prepared research before model reservation", func() bool {
		active, err := f.db.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range active {
			if current.Stage == model.StepResearch+"_pending" {
				attempt = current
				return true
			}
		}
		return false
	})
	step, err := f.db.ProgramStepForFeature(context.Background(), attempt.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	key := attempt.ID + ":" + step.Kind + ":0"
	if err := f.db.ReserveCall(context.Background(), attempt, step.Kind, key, f.e.cfg.MaxCallsPerDay); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReserveCall(context.Background(), attempt, step.Kind, key, f.e.cfg.MaxCallsPerDay); err != nil {
		t.Fatal(err)
	}
	if f.status(t, "reserved-model").Budget.ModelCalls != 1 {
		t.Fatal("idempotent reservation spent program allowance twice")
	}
	f.restart(t)
	ticks(t, f.e, 8)
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 1 || active[0].Stage != "uncertain" || active[0].DispatchKey != key {
		t.Fatalf("restart discarded an unreconciled reserved dispatch: %+v %v", active, err)
	}
	if len(f.backend.requests) != 0 || f.status(t, "reserved-model").Budget.ModelCalls != 1 {
		t.Fatal("restart blindly sent/refunded an already-reserved model operation")
	}
}

func TestProgramRestartAfterExperimentIntentReplaysOnlyExactAbsentRequest(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "reserved-experiment", model.ProgramLimits{}, true)
	f.approvalGate(t, "reserved-experiment", 0)
	f.approveIteration(t, "reserved-experiment", 0)
	var attempt model.Attempt
	f.until(t, "experiment before durable launch intent", func() bool {
		active, err := f.db.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range active {
			if current.Stage == "experiment_pending" {
				attempt = current
				return true
			}
		}
		return false
	})
	step, err := f.db.ProgramStepForFeature(context.Background(), attempt.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.e.programExperimentRequest(context.Background(), attempt, step)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordExperimentIntent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	attempt.Stage, attempt.DispatchKey = "experiment_starting", request.RunID
	if err := f.db.BeginOperation(context.Background(), attempt, f.e.cfg.PolicyHash); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.publish(t, "reserved-experiment")
	if len(f.runner.starts) == 0 || len(f.runner.statuses) == 0 || f.runner.processes[request.RunID] != 1 || len(f.runner.processes) != 1 {
		t.Fatal("healthy absent journal did not recover the stored request with exactly one process execution")
	}
	for _, replay := range f.runner.starts {
		if !reflect.DeepEqual(replay, request) {
			t.Fatal("recovery changed stored RunID, RequestSHA, execution policy, or frozen request")
		}
	}
	for _, runID := range f.runner.statuses {
		if runID != request.RunID {
			t.Fatal("restart queried a different durable runner identity")
		}
	}
	if budget := f.status(t, "reserved-experiment").Budget; budget.ModelCalls != 6 || budget.Experiments != 1 {
		t.Fatalf("runner recovery consumed model calls or a second experiment: %+v", budget)
	}
}

func TestProgramApprovedExperimentUsesFrozenBundleNotMutablePreparation(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "frozen-input", model.ProgramLimits{}, true)
	f.approvalGate(t, "frozen-input", 0)
	experiments, err := f.db.ProgramExperimentPage(context.Background(), "frozen-input", store.ProgramQuery{})
	if err != nil || len(experiments.Items) != 1 {
		t.Fatalf("missing frozen contract: %+v %v", experiments, err)
	}
	experiment := experiments.Items[0]
	bundle, err := f.db.ProgramBundle(context.Background(), experiment.Contract.BundleManifestSHA)
	if err != nil || len(bundle.Files) != 1 {
		t.Fatalf("missing actual frozen input: %+v %v", bundle, err)
	}
	frozen, err := f.repos.blobs.Read(context.Background(), bundle.Files[0].ContentSHA, bundle.Files[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	workspace := f.repos.workspaces[experiment.Artifact.ProducerStepID]
	mutable := filepath.Join(workspace.Path, bundle.Files[0].Path)
	if err := os.WriteFile(mutable, []byte("test-only mutation after frozen contract creation"), 0600); err != nil {
		t.Fatal(err)
	}
	f.approveIteration(t, "frozen-input", 0)
	f.until(t, "launch of approved immutable experiment", func() bool { return len(f.runner.starts) == 1 })
	request := f.runner.starts[0]
	if request.ContractSHA != experiment.ContractSHA || request.Bundle.ManifestSHA != experiment.Contract.BundleManifestSHA || !reflect.DeepEqual(request.Bundle, bundle) {
		t.Fatal("runner received mutable or unapproved experiment identity")
	}
	actual, err := f.repos.blobs.Read(context.Background(), request.Bundle.Files[0].ContentSHA, request.Bundle.Files[0].Bytes)
	if err != nil || !reflect.DeepEqual(actual, frozen) {
		t.Fatalf("frozen experiment content changed with author workspace: %q %v", actual, err)
	}
}

func TestProgramLostStartBeforeRunnerIntentRecoversExactRequestOnce(t *testing.T) {
	f := newProgramTestFixture(t)
	f.runner.loseStartBeforeIntent = true
	f.importProgram(t, "start-before-intent", model.ProgramLimits{}, true)
	f.approvalGate(t, "start-before-intent", 0)
	f.approveIteration(t, "start-before-intent", 0)
	f.until(t, "transport loss before runner journal acceptance", func() bool { return len(f.runner.starts) == 1 })
	original := f.runner.starts[0]
	if len(f.runner.processes) != 0 || len(f.runner.receipts) != 0 {
		t.Fatal("pre-intent transport fixture executed an experiment")
	}
	persisted, err := f.db.ProgramExperimentIntent(context.Background(), original.RunID)
	if err != nil || !reflect.DeepEqual(persisted, original) {
		t.Fatalf("lost Start had no exact controller-owned durable request: %+v %v", persisted, err)
	}
	f.restart(t)
	f.publish(t, "start-before-intent")
	if len(f.runner.starts) < 2 || len(f.runner.statuses) == 0 || len(f.runner.processes) != 1 || f.runner.processes[original.RunID] != 1 {
		t.Fatal("certified absence did not recover exactly one experiment process")
	}
	for _, request := range f.runner.starts {
		if !reflect.DeepEqual(request, original) {
			t.Fatal("recovery changed canonical request identity or policy")
		}
	}
	budget := f.status(t, "start-before-intent").Budget
	if budget.ModelCalls != 6 || budget.Experiments != 1 || budget.Steps != 7 {
		t.Fatalf("idempotent runner recovery duplicated program work: %+v", budget)
	}
}

func TestProgramExecutorPolicyMismatchBeforeRunnerIntentReleasesLane(t *testing.T) {
	f := newProgramTestFixture(t)
	f.importProgram(t, "runner-policy-mismatch", model.ProgramLimits{}, true)
	f.approvalGate(t, "runner-policy-mismatch", 0)
	f.approveIteration(t, "runner-policy-mismatch", 0)
	f.runner.executionPolicySHA = strings.Repeat("f", 64)
	f.until(t, "executor policy rejection before process intent", func() bool { return len(f.runner.starts) == 1 })
	ticks(t, f.e, 6)
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 0 {
		t.Fatalf("definitive pre-intent policy rejection retained owned capacity: %+v %v", active, err)
	}
	if len(f.runner.processes) != 0 || len(f.runner.receipts) != 0 || len(f.runner.statuses) != 0 || len(f.backend.requests) != 3 {
		t.Fatal("rejected executor policy launched, polled, or advanced an experiment")
	}
	for _, step := range f.status(t, "runner-policy-mismatch").Steps {
		if step.Kind == model.StepExperimentRun {
			feature := state(t, f.db, step.FeatureID)
			if feature.State != "blocked" || !strings.Contains(feature.Reason, "policy_changed") {
				t.Fatalf("policy rejection did not remain a visible prerequisite failure: %+v", feature)
			}
		}
	}
	f.restart(t)
	ticks(t, f.e, 6)
	if len(f.runner.starts) != 1 || len(f.runner.processes) != 0 {
		t.Fatal("restart retried definitive executor policy rejection")
	}
}

func TestProgramRunningExperimentMissingJournalNeverRelaunchesProcess(t *testing.T) {
	f := newProgramTestFixture(t)
	f.runner.hold = true
	f.importProgram(t, "lost-running-journal", model.ProgramLimits{}, true)
	f.approvalGate(t, "lost-running-journal", 0)
	f.approveIteration(t, "lost-running-journal", 0)
	f.until(t, "durably running experiment", func() bool {
		active, err := f.db.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, attempt := range active {
			if attempt.Stage == "experiment_running" {
				return true
			}
		}
		return false
	})
	request := f.runner.starts[0]
	delete(f.runner.receipts, request.RunID)
	f.restart(t)
	ticks(t, f.e, 8)
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 1 || active[0].Stage != "uncertain" {
		t.Fatalf("missing running journal discarded possible external owner: %+v %v", active, err)
	}
	if f.runner.processes[request.RunID] != 1 || len(f.runner.processes) != 1 || len(f.runner.starts) != 1 {
		t.Fatal("running404 relaunched a process instead of fencing ownership")
	}
	if len(f.backend.requests) != 3 {
		t.Fatal("lost running experiment journal advanced downstream evaluation")
	}
}

func TestProgramUncertifiedRunnerAbsenceNeverReplaysStart(t *testing.T) {
	for _, failure := range []*executor.ExperimentError{
		{Code: "not_found", Message: "uncertified transport or proxy404", Status: 404},
		{Code: "operation_uncertain", Message: "runner journal cannot be verified healthy", Status: 503},
	} {
		t.Run(failure.Code, func(t *testing.T) {
			f := newProgramTestFixture(t)
			f.runner.loseStartBeforeIntent, f.runner.statusError = true, failure
			f.importProgram(t, "uncertified-journal", model.ProgramLimits{}, true)
			f.approvalGate(t, "uncertified-journal", 0)
			f.approveIteration(t, "uncertified-journal", 0)
			f.until(t, "unknown runner acceptance", func() bool { return len(f.runner.starts) == 1 })
			f.restart(t)
			ticks(t, f.e, 8)
			active, err := f.db.Active(context.Background())
			if err != nil || len(active) != 1 {
				t.Fatalf("unverified journal absence freed possibly owned capacity: %+v %v", active, err)
			}
			if len(f.runner.processes) != 0 || len(f.runner.starts) != 1 || len(f.runner.statuses) == 0 {
				t.Fatal("uncertified absence authorized an experiment process")
			}
			if budget := f.status(t, "uncertified-journal").Budget; budget.ModelCalls != 3 || budget.Experiments != 1 {
				t.Fatalf("journal uncertainty duplicated work: %+v", budget)
			}
		})
	}
}

func TestProgramPausedRecoveryDoesNotReplayAbsentRunnerIntent(t *testing.T) {
	f := newProgramTestFixture(t)
	f.runner.loseStartBeforeIntent = true
	f.importProgram(t, "paused-runner-recovery", model.ProgramLimits{}, true)
	f.approvalGate(t, "paused-runner-recovery", 0)
	f.approveIteration(t, "paused-runner-recovery", 0)
	f.until(t, "pending runner acceptance before pause", func() bool { return len(f.runner.starts) == 1 })
	original := f.runner.starts[0]
	if err := f.db.SetPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	ticks(t, f.e, 6)
	if len(f.runner.starts) != 1 || len(f.runner.processes) != 0 {
		t.Fatal("certified absence bypassed OpenFactory pause during recovery")
	}
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 1 || active[0].Stage != "experiment_starting" {
		t.Fatalf("paused recovery discarded the exact durable intent: %+v %v", active, err)
	}
	if err := f.db.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "paused-runner-recovery")
	if f.runner.processes[original.RunID] != 1 || len(f.runner.processes) != 1 {
		t.Fatal("unpaused recovery did not execute exactly one process")
	}
	for _, request := range f.runner.starts {
		if !reflect.DeepEqual(request, original) {
			t.Fatal("pause/resume regenerated the request identity")
		}
	}
}

func TestProgramCancellationBeforeRunnerIntentPersistsNeverStartedTombstone(t *testing.T) {
	f := newProgramTestFixture(t)
	f.runner.loseStartBeforeIntent = true
	revision := f.importProgram(t, "cancel-before-runner-intent", model.ProgramLimits{}, true)
	f.approvalGate(t, "cancel-before-runner-intent", 0)
	f.approveIteration(t, "cancel-before-runner-intent", 0)
	f.until(t, "lost pre-journal runner Start", func() bool { return len(f.runner.starts) == 1 })
	request := f.runner.starts[0]
	if len(f.runner.processes) != 0 {
		t.Fatal("pre-journal transport fixture executed a process")
	}
	if err := f.db.CancelProgram(context.Background(), "cancel-before-runner-intent", revision.RevisionHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.until(t, "durable never-started cancellation", func() bool {
		active, err := f.db.Active(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(active) == 0
	})
	if len(f.runner.cancelIntents) != 1 || !reflect.DeepEqual(f.runner.cancelIntents[0], request) {
		t.Fatal("cancellation regenerated or omitted exact pending runner request")
	}
	receipt, err := f.db.ProgramExperimentReceipt(context.Background(), request.RunID)
	if err != nil || receipt.RunID != request.RunID || receipt.RequestSHA != request.RequestSHA || receipt.State != "cancelled" || !receipt.StartedAt.IsZero() || receipt.FinishedAt == nil || receipt.Conclusive || receipt.Accepted {
		t.Fatalf("never-started cancellation was not durably recorded: %+v %v", receipt, err)
	}
	late, err := f.runner.Start(context.Background(), request)
	if err != nil || late.State != "cancelled" || len(f.runner.processes) != 0 {
		t.Fatalf("late exact Start escaped cancelled journal tombstone: %+v %v", late, err)
	}
	f.restart(t)
	ticks(t, f.e, 6)
	if len(f.runner.processes) != 0 || len(f.backend.requests) != 3 || f.status(t, "cancel-before-runner-intent").Program.State != model.ProgramCancelled {
		t.Fatal("restart revived cancelled pre-journal execution")
	}
}

func TestProgramUnhealthyJournalCancellationRetainsLaneWithoutTombstone(t *testing.T) {
	f := newProgramTestFixture(t)
	f.runner.loseStartBeforeIntent = true
	revision := f.importProgram(t, "cancel-unhealthy-journal", model.ProgramLimits{}, true)
	f.approvalGate(t, "cancel-unhealthy-journal", 0)
	f.approveIteration(t, "cancel-unhealthy-journal", 0)
	f.until(t, "unknown runner acceptance before journal failure", func() bool { return len(f.runner.starts) == 1 })
	f.runner.statusError = &executor.ExperimentError{Code: "operation_uncertain", Message: "test-only journal corruption prevents certifying absence", Status: 503}
	if err := f.db.CancelProgram(context.Background(), "cancel-unhealthy-journal", revision.RevisionHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	ticks(t, f.e, 8)
	active, err := f.db.Active(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("unhealthy journal cancellation released possible external ownership: %+v %v", active, err)
	}
	if len(f.runner.cancelIntents) == 0 || len(f.runner.receipts) != 0 || len(f.runner.acceptedRequests) != 0 || len(f.runner.processes) != 0 || len(f.runner.starts) != 1 {
		t.Fatal("unhealthy journal certified a never-started tombstone or launched a process")
	}
}

func TestProgramDomainWaitDoesNotPreventOrdinaryFeatureAdmission(t *testing.T) {
	for _, code := range []string{"budget_exhausted", "invalid_contract"} {
		t.Run(code, func(t *testing.T) {
			f := newProgramTestFixture(t)
			limits := model.ProgramLimits{}
			if code == "budget_exhausted" {
				limits.MaxSteps = 1
			} else {
				f.backend.invalidImplementationCheck = true
			}
			revision := f.importProgram(t, "nonfatal-program-wait", limits, true)
			if code == "invalid_contract" {
				f.approvalGate(t, "nonfatal-program-wait", 0)
				f.approveIteration(t, "nonfatal-program-wait", 0)
			}
			f.until(t, "persisted program-only "+code+" wait", func() bool {
				status := f.status(t, "nonfatal-program-wait")
				return status.Program.Wait != nil && status.Program.Wait.Code == code
			})
			events, err := f.db.ProgramEventPage(context.Background(), "nonfatal-program-wait", store.ProgramQuery{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			recorded := false
			for _, event := range events.Items {
				if event.Kind != "waiting" {
					continue
				}
				var detail struct {
					RevisionHash string `json:"revision_hash"`
					Code         string `json:"code"`
					Detail       string `json:"detail"`
				}
				if err := json.Unmarshal([]byte(event.Detail), &detail); err != nil {
					t.Fatal(err)
				}
				if detail.Code == code && detail.RevisionHash == revision.RevisionHash && detail.Detail != "" {
					recorded = true
				}
			}
			if !recorded {
				t.Fatalf("program %s wait had no exact-revision persisted event", code)
			}
			if code == "invalid_contract" {
				if _, err := f.db.Get(context.Background(), "program-product-nonfatal-program-wait"); err == nil {
					t.Fatal("malformed implementation contract partially published")
				}
			}
			ordinary, err := f.db.Import(context.Background(), contract("ordinary-beside-program-wait"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.Approve(context.Background(), ordinary.ID, ordinary.Revision, f.e.cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
			if err := f.e.Tick(context.Background()); err != nil {
				t.Fatalf("program domain wait aborted ordinary OpenFactory cycle: %v", err)
			}
			active, err := f.db.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			admitted := false
			for _, attempt := range active {
				if attempt.FeatureID == ordinary.ID && attempt.Stage == "prepare" {
					admitted = true
				}
			}
			if !admitted || state(t, f.db, ordinary.ID).State != "running" {
				t.Fatalf("healthy ordinarily approved feature was not admitted beside program %s: %+v", code, active)
			}
			if wait := f.status(t, "nonfatal-program-wait").Program.Wait; wait == nil || wait.Code != code {
				t.Fatalf("ordinary admission erased program-only diagnostic: %+v", wait)
			}
		})
	}
}

func TestProgramLargeImmutableInputsArePagedWithoutBlockingIndependentEvaluation(t *testing.T) {
	f := newProgramTestFixture(t)
	f.e.cfg.MaxPromptBytes = 32 << 10
	f.backend.largePayloadBytes, f.runner.largeOutputBytes = 48<<10, 48<<10
	f.importProgram(t, "paged-evidence", model.ProgramLimits{}, true)
	f.approvalGate(t, "paged-evidence", 0)
	if len(f.runner.starts) != 0 || len(f.backend.requests) != 3 {
		t.Fatal("large frozen preparation skipped plan evaluation or approval")
	}
	f.approveIteration(t, "paged-evidence", 0)
	f.publish(t, "paged-evidence")
	status := f.status(t, "paged-evidence")
	if status.Budget.ModelCalls != 6 || status.Budget.Steps != 7 || len(f.runner.processes) != 1 {
		t.Fatalf("large authorized evidence blocked or duplicated fixed graph: %+v", status)
	}
	for _, request := range f.backend.requests {
		if len(request.Prompt) > f.e.cfg.MaxPromptBytes {
			t.Fatal("materialized input index exceeded compact prompt limit")
		}
		for _, marker := range []string{"TEST_ONLY_LARGE_ARTIFACT", "TEST_ONLY_LARGE_FROZEN_INPUT", "TEST_ONLY_LARGE_STDOUT", "TEST_ONLY_LARGE_OBSERVATION"} {
			if strings.Contains(request.Prompt, marker) {
				t.Fatalf("large immutable %s body was inlined into the model prompt", marker)
			}
		}
		contextBytes, err := os.ReadFile(filepath.Join(request.Workspace, "inputs", "context.json"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(contextBytes)
		if !strings.Contains(request.Prompt, "inputs/context.json") || !strings.Contains(request.Prompt, hex.EncodeToString(sum[:])) {
			t.Fatal("compact prompt did not address the exact materialized context bytes")
		}
	}
	request := f.runner.starts[0]
	receipt, err := f.db.ProgramExperimentReceipt(context.Background(), request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Bundle.Files[0].Bytes <= 24<<10 || receipt.Commands[0].StdoutBytes <= 24<<10 || receipt.Artifacts.Files[0].Bytes <= 24<<10 {
		t.Fatal("large-evidence fixture did not exceed the former inline prompt threshold")
	}
	required := map[string]string{
		"frozen preparation":   request.Bundle.Files[0].ContentSHA,
		"captured stdout":      receipt.Commands[0].StdoutSHA,
		"captured observation": receipt.Artifacts.Files[0].ContentSHA,
	}
	var planSession, resultSession *model.DiscoverySession
	for index := range f.repos.sessions {
		session := &f.repos.sessions[index]
		if session.Role == model.StepPlanEvaluation {
			planSession = session
		}
		if session.Role == model.StepResultEvaluation {
			resultSession = session
		}
	}
	if planSession == nil || resultSession == nil {
		t.Fatal("large evidence bypassed independent plan/result sessions")
	}
	for label, sha := range required {
		session := resultSession
		if label == "frozen preparation" {
			session = planSession
		}
		found := false
		for _, input := range session.InputFiles {
			if input.Path != "inputs/blobs/"+sha+".txt" {
				continue
			}
			actual, err := os.ReadFile(filepath.Join(f.repos.workspaces[session.StepID].Path, filepath.FromSlash(input.Path)))
			if err != nil {
				t.Fatal(err)
			}
			expected, err := f.repos.blobs.Read(context.Background(), sha, 1<<20)
			if err != nil || !reflect.DeepEqual(actual, expected) || input.ContentSHA != sha {
				t.Fatalf("%s was not fully materialized with its exact CAS identity: %v", label, err)
			}
			info, err := os.Stat(filepath.Join(f.repos.workspaces[session.StepID].Path, filepath.FromSlash(input.Path)))
			if err != nil || info.Mode().Perm()&0222 != 0 {
				t.Fatalf("%s materialized input was writable: %v", label, err)
			}
			found = true
		}
		if !found {
			t.Fatalf("independent evaluator could not page %s by immutable hash", label)
		}
	}
	contextBytes, err := os.ReadFile(filepath.Join(f.repos.workspaces[planSession.StepID].Path, "inputs", "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var planContext struct {
		Inputs []struct {
			Ref     model.ProgramInputRef `json:"ref"`
			Payload json.RawMessage       `json:"payload"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(contextBytes, &planContext); err != nil {
		t.Fatal(err)
	}
	foundRequirements := false
	for _, input := range planContext.Inputs {
		if input.Ref.Kind != model.ArtifactRequirements {
			continue
		}
		var requirements model.RequirementsPayload
		if err := model.StrictDecode(input.Payload, &requirements); err != nil {
			t.Fatal(err)
		}
		if len(requirements.Requirements[0].Rationale) <= 32<<10 {
			t.Fatal("materialized typed requirement artifact was truncated to prompt size")
		}
		foundRequirements = true
	}
	if !foundRequirements {
		t.Fatal("materialized evaluation context omitted typed requirement evidence")
	}
}
