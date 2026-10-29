package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
)

// Options are operator configuration, not model-supplied authority. JournalDir
// must not be in the shared workspace. ArtifactRoot is the controller/checker
// CAS volume, never an agent workspace. Empty JournalDir keeps checks-only mode.
type Options struct {
	JournalDir            string
	ArtifactRoot          string
	Profiles              map[string]model.ExperimentProfile
	DeclaredImageIdentity string
	Prerequisites         []string
	Limits                model.ExperimentLimits
}

type experimentRecord struct {
	Request model.ExperimentRunRequest `json:"request"`
	Receipt model.ExperimentReceipt    `json:"receipt"`
}
type experimentService struct {
	mu              sync.Mutex
	journal         *journal
	blobs           *artifacts.Store
	options         Options
	records         map[string]*experimentRecord
	cancels         map[string]context.CancelFunc
	ctx             context.Context
	cancel          context.CancelFunc
	workers         sync.WaitGroup
	capabilityError error
}

// RunID is stable even when current approval authority has been invalidated;
// cancellation and reconciliation must address the original durable run.
func RunID(attemptID, contractSHA string, ordinal int) string {
	return hashJSON([]string{attemptID, contractSHA, strconv.Itoa(ordinal)})
}
func ContractSHA(contract model.ExperimentContract) string {
	data, err := model.ExperimentContractCanonical(contract)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func hashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NormalizeRequest binds exactly one durable run identity to one approved
// contract, bundle and attempt. RequestSHA excludes only its own field.
func NormalizeRequest(request model.ExperimentRunRequest) (model.ExperimentRunRequest, error) {
	if request.AttemptID == "" || request.ProgramID == "" || request.StepID == "" || request.Revision < 1 || request.Ordinal < 0 || request.Contract.ID == "" {
		return request, errors.New("experiment identity is incomplete")
	}
	if !validSHA(request.ExecutionPolicySHA) {
		return request, errors.New("experiment execution policy hash is required")
	}
	validationContract := request.Contract
	validationContract.Limits = experimentLimits(validationContract.Limits)
	validationBytes, err := json.Marshal(model.ExperimentContractsPayload{Experiments: []model.ExperimentContract{validationContract}})
	if err != nil {
		return request, errors.New("invalid experiment contract JSON")
	}
	if _, err = model.CanonicalProgramPayload(model.ArtifactExperimentContracts, validationBytes); err != nil {
		return request, fmt.Errorf("invalid experiment contract: %w", err)
	}
	contractSHA := ContractSHA(request.Contract)
	if request.ContractSHA != "" && request.ContractSHA != contractSHA {
		return request, errors.New("experiment contract hash mismatch")
	}
	request.ContractSHA = contractSHA
	id := RunID(request.AttemptID, contractSHA, request.Ordinal)
	if request.RunID != "" && request.RunID != id {
		return request, errors.New("experiment run ID mismatch")
	}
	request.RunID = id
	supplied := request.RequestSHA
	request.RequestSHA = ""
	sha := hashJSON(request)
	if supplied != "" && supplied != sha {
		return request, errors.New("experiment request hash mismatch")
	}
	request.RequestSHA = sha
	if !validSHA(request.Bundle.ManifestSHA) || request.Contract.BundleManifestSHA != request.Bundle.ManifestSHA {
		return request, errors.New("experiment bundle identity mismatch")
	}
	if len(request.Contract.BundleFiles) == 0 || len(request.Contract.BundleFiles) != len(request.Bundle.Files) || len(request.Bundle.Files) > 128 {
		return request, errors.New("experiment bundle must exactly match declared files")
	}
	members := map[string]bool{}
	prefix := "experiments/" + request.Contract.ID + "/"
	for _, member := range request.Contract.BundleFiles {
		if !safeMember(member) || !strings.HasPrefix(member, prefix) || members[member] {
			return request, errors.New("invalid or duplicate experiment bundle member")
		}
		members[member] = true
	}
	for _, file := range request.Bundle.Files {
		if !members[file.Path] {
			return request, errors.New("experiment bundle includes undeclared file")
		}
		delete(members, file.Path)
	}
	if len(request.Contract.Commands) == 0 || len(request.Contract.Commands) > 32 || len(request.Contract.Acceptance) == 0 || len(request.Contract.Observations) > 128 || len(request.Contract.Acceptance) > 128 {
		return request, errors.New("experiment requires bounded commands, observations and acceptance predicates")
	}
	limits := request.Contract.Limits
	if limits.WallSeconds < 0 || limits.WallSeconds > 300 || limits.MaxOutputBytes < 0 || limits.MaxOutputBytes > 1<<20 || limits.MaxArtifactBytes < 0 || limits.MaxArtifactBytes > 16<<20 || limits.MaxArtifactFiles < 0 || limits.MaxArtifactFiles > 128 {
		return request, errors.New("experiment limits exceed runner capability")
	}
	seen := map[string]bool{}
	for _, observation := range request.Contract.Observations {
		if observation.Name == "" || seen[observation.Name] || !safeMember(observation.OutputPath) {
			return request, errors.New("invalid or duplicate observation")
		}
		for _, member := range request.Contract.BundleFiles {
			if observation.OutputPath == member {
				return request, errors.New("observation output collides with frozen input")
			}
		}
		seen[observation.Name] = true
	}
	for _, predicate := range request.Contract.Acceptance {
		if !seen[predicate.Observation] || !json.Valid(predicate.Value) {
			return request, errors.New("invalid predicate observation or value")
		}
		switch predicate.Operator {
		case "eq", "ne", "lt", "le", "gt", "ge", "in":
		default:
			return request, errors.New("unsupported experiment predicate")
		}
	}
	return request, nil
}
func newExperimentService(parent context.Context, root string, options Options) (*experimentService, error) {
	ctx, cancel := context.WithCancel(parent)
	caps := experimentLimits(options.Limits)
	if caps.WallSeconds < 1 || caps.WallSeconds > 300 || caps.MaxOutputBytes < 1 || caps.MaxOutputBytes > 1<<20 || caps.MaxArtifactBytes < 1 || caps.MaxArtifactBytes > 16<<20 || caps.MaxArtifactFiles < 1 || caps.MaxArtifactFiles > 128 {
		cancel()
		return nil, errors.New("configured experiment limits exceed runner capability")
	}
	service := &experimentService{options: options, records: map[string]*experimentRecord{}, cancels: map[string]context.CancelFunc{}, ctx: ctx, cancel: cancel}
	if options.JournalDir == "" {
		return service, nil
	}
	journal, err := openJournal(options.JournalDir, root)
	if err != nil {
		service.capabilityError = unhealthyJournal()
		return service, nil
	}
	service.journal = journal
	if options.ArtifactRoot == "" {
		service.close()
		return nil, errors.New("experiment ArtifactRoot is required with JournalDir")
	}
	if !filepath.IsAbs(options.ArtifactRoot) {
		service.close()
		return nil, errors.New("experiment ArtifactRoot must be absolute")
	}
	if err = os.MkdirAll(options.ArtifactRoot, 0700); err != nil {
		service.close()
		return nil, err
	}
	artifactRoot, err := filepath.EvalSymlinks(options.ArtifactRoot)
	if err != nil {
		service.close()
		return nil, err
	}
	sharedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		service.close()
		return nil, err
	}
	if within(sharedRoot, artifactRoot) {
		service.close()
		return nil, errors.New("experiment artifacts must not be on the agent workspace volume")
	}
	service.blobs = artifacts.New(options.ArtifactRoot)
	entries, err := os.ReadDir(journal.root)
	if err != nil {
		service.capabilityError = unhealthyJournal()
		return service, nil
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validSHA(id) {
			service.capabilityError = unhealthyJournal()
			continue
		}
		var record experimentRecord
		if err = readJournal(filepath.Join(journal.root, entry.Name()), &record); err != nil {
			service.capabilityError = unhealthyJournal()
			continue
		}
		normalized, normalizeErr := NormalizeRequest(record.Request)
		if normalizeErr != nil || normalized.RunID != id || record.Receipt.RunID != id || record.Receipt.RequestSHA != normalized.RequestSHA || record.Receipt.ContractSHA != normalized.ContractSHA || record.Receipt.BundleManifestSHA != normalized.Bundle.ManifestSHA || record.Receipt.AttemptID != normalized.AttemptID || record.Receipt.ProgramID != normalized.ProgramID || record.Receipt.StepID != normalized.StepID || record.Receipt.Revision != normalized.Revision || record.Receipt.Accepted && (!record.Receipt.Conclusive || record.Receipt.State != "completed") {
			service.capabilityError = unhealthyJournal()
			continue
		}
		if !terminal(record.Receipt.State) {
			now := time.Now().UTC()
			record.Receipt.State = "uncertain"
			record.Receipt.ExecutionOutcome = "uncertain"
			record.Receipt.ObservationOutcome = "not_evaluated"
			record.Receipt.HypothesisOutcome = "inconclusive"
			record.Receipt.Conclusive = false
			record.Receipt.Accepted = false
			record.Receipt.FailureKind = "operation_uncertain"
			record.Receipt.Detail = "runner restarted with unfinished durable intent; execution is not replayed"
			record.Receipt.FinishedAt = &now
			if err = journal.save(id, record); err != nil {
				service.capabilityError = unhealthyJournal()
			}
		}
		service.records[id] = &record
	}
	return service, nil
}
func terminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "uncertain", "resolved_failed", "waiting_prerequisite":
		return true
	}
	return false
}
func (s *experimentService) close() {
	s.cancel()
	s.mu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	s.workers.Wait()
	s.journal.close()
}
func (s *experimentService) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /experiments", func(w http.ResponseWriter, r *http.Request) {
		var request model.ExperimentRunRequest
		if !decodeExperiment(w, r, &request) {
			return
		}
		receipt, err := s.start(request)
		writeExperiment(w, receipt, err)
	})
	mux.HandleFunc("GET /experiments/{id}", func(w http.ResponseWriter, r *http.Request) {
		receipt, err := s.status(r.PathValue("id"))
		writeExperiment(w, receipt, err)
	})
	mux.HandleFunc("POST /experiments/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if !decodeExperiment(w, r, &raw) {
			return
		}
		var body map[string]json.RawMessage
		if err := model.StrictDecode(raw, &body); err != nil || body == nil {
			writeExperiment(w, model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: "invalid cancellation body", Status: 400})
			return
		}
		if len(body) == 0 {
			receipt, err := s.cancelRun(r.PathValue("id"))
			writeExperiment(w, receipt, err)
			return
		}
		var request model.ExperimentRunRequest
		if err := model.StrictDecode(raw, &request); err != nil {
			writeExperiment(w, model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: "invalid cancellation intent", Status: 400})
			return
		}
		receipt, err := s.cancelIntent(r.PathValue("id"), request)
		writeExperiment(w, receipt, err)
	})
	mux.HandleFunc("POST /experiments/{id}/resolve-failed", func(w http.ResponseWriter, r *http.Request) {
		var body resolveRequest
		if !decodeExperiment(w, r, &body) {
			return
		}
		receipt, err := s.resolve(r.PathValue("id"), body)
		writeExperiment(w, receipt, err)
	})
}
func decodeExperiment(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	data, err := io.ReadAll(r.Body)
	if err != nil || model.StrictDecode(data, value) != nil {
		writeExperiment(w, model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: "invalid experiment request", Status: 400})
		return false
	}
	return true
}
func writeExperiment(w http.ResponseWriter, receipt model.ExperimentReceipt, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		var failure *ExperimentError
		if !errors.As(err, &failure) {
			failure = &ExperimentError{Code: "operation_uncertain", Message: "experiment journal operation failed", Status: 503}
		}
		w.WriteHeader(failure.Status)
		_ = json.NewEncoder(w).Encode(failure)
		return
	}
	_ = json.NewEncoder(w).Encode(receipt)
}
func (s *experimentService) capability() error {
	if s.capabilityError != nil {
		return s.capabilityError
	}
	if s.journal == nil {
		return &ExperimentError{Code: "prerequisite_missing", Message: "experiments require checker-private --journal-root and --artifacts-root; legacy checks remain available", Status: 503}
	}
	return nil
}
func unhealthyJournal() error {
	return &ExperimentError{Code: "operation_uncertain", Message: "experiment journal contains unavailable or invalid ownership records; experiment launches are disabled, ordinary checks remain available", Status: 503}
}
func copyReceipt(receipt model.ExperimentReceipt) model.ExperimentReceipt {
	data, _ := json.Marshal(receipt)
	var copy model.ExperimentReceipt
	_ = json.Unmarshal(data, &copy)
	return copy
}
func (s *experimentService) start(request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return model.ExperimentReceipt{}, s.capability()
	}
	normalized, err := NormalizeRequest(request)
	if err != nil {
		if s.capabilityError != nil {
			return model.ExperimentReceipt{}, s.capabilityError
		}
		if _, exists := s.records[request.RunID]; exists {
			return model.ExperimentReceipt{}, &ExperimentError{Code: "idempotency_conflict", Message: "run ID already names a different request", Status: 409}
		}
		return model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: err.Error(), Status: 400}
	}
	request = normalized
	if record, ok := s.records[request.RunID]; ok {
		if record.Request.RequestSHA != request.RequestSHA {
			return model.ExperimentReceipt{}, &ExperimentError{Code: "idempotency_conflict", Message: "run ID already names a different request", Status: 409}
		}
		return copyReceipt(record.Receipt), nil
	}
	if err := s.capability(); err != nil {
		return model.ExperimentReceipt{}, err
	}
	if request.ExecutionPolicySHA != model.ExperimentPolicySHA(s.options.Profiles, s.options.Limits) {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "policy_changed", Message: "approved execution profiles or limits differ from the currently configured runner policy", Status: 409}
	}
	limits := experimentLimits(request.Contract.Limits)
	caps := experimentLimits(s.options.Limits)
	if limits.WallSeconds > caps.WallSeconds || limits.MaxOutputBytes > caps.MaxOutputBytes || limits.MaxArtifactBytes > caps.MaxArtifactBytes || limits.MaxArtifactFiles > caps.MaxArtifactFiles {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "policy_changed", Message: "approved experiment limits exceed the currently configured runner limits", Status: 409}
	}
	receipt := model.ExperimentReceipt{RunID: request.RunID, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID, Revision: request.Revision, StepID: request.StepID, AttemptID: request.AttemptID, ContractSHA: request.ContractSHA, BundleManifestSHA: request.Bundle.ManifestSHA, State: "intent", ExecutionOutcome: "pending", ObservationOutcome: "pending", HypothesisOutcome: "pending", DeclaredImageIdentity: s.options.DeclaredImageIdentity, AttestationScope: "binary SHA256 observed before launch by runner; image identity is operator-declared, not observed OCI identity; resources are container-wide, not per-run guarantees; approved experiment code is trusted and shares checker UID; observations are experiment-produced and require independent evaluation"}
	record := &experimentRecord{Request: request, Receipt: receipt}
	if err = s.journal.save(request.RunID, record); err != nil {
		return model.ExperimentReceipt{}, err
	}
	s.records[request.RunID] = record
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancels[request.RunID] = cancel
	s.workers.Add(1)
	go func() { defer s.workers.Done(); defer cancel(); s.run(ctx, request.RunID) }()
	return copyReceipt(receipt), nil
}
func (s *experimentService) status(id string) (model.ExperimentReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if ok {
		return copyReceipt(record.Receipt), nil
	}
	if err := s.capability(); err != nil {
		return model.ExperimentReceipt{}, err
	}
	return model.ExperimentReceipt{}, &ExperimentError{Code: "experiment_not_found", Message: "healthy experiment journal has no record for this run", Status: 404}
}
func (s *experimentService) cancelRun(id string) (model.ExperimentReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.capability(); err != nil {
		return model.ExperimentReceipt{}, err
	}
	record, ok := s.records[id]
	if !ok {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "experiment_not_found", Message: "healthy experiment journal has no record for this run", Status: 404}
	}
	if cancel := s.cancels[id]; cancel != nil {
		cancel()
		record.Receipt.State = "cancelling"
		if err := s.journal.save(id, record); err != nil {
			return model.ExperimentReceipt{}, err
		}
	}
	return copyReceipt(record.Receipt), nil
}
func (s *experimentService) resolve(id string, request resolveRequest) (model.ExperimentReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.capability(); err != nil {
		return model.ExperimentReceipt{}, err
	}
	record, ok := s.records[id]
	if !ok {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "experiment_not_found", Message: "healthy experiment journal has no record for this run", Status: 404}
	}
	if record.Request.RequestSHA != request.RequestSHA {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "idempotency_conflict", Message: "resolution request hash mismatch", Status: 409}
	}
	if strings.TrimSpace(request.Attestation) == "" || len(request.Attestation) > 4096 {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: "exact operator failure attestation required", Status: 400}
	}
	if record.Receipt.State == "resolved_failed" && record.Receipt.ResolutionAttestation == request.Attestation {
		return copyReceipt(record.Receipt), nil
	}
	if record.Receipt.State != "uncertain" {
		return model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: "only uncertain execution can be resolved failed", Status: 409}
	}
	record.Receipt.State = "resolved_failed"
	record.Receipt.Accepted = false
	record.Receipt.Conclusive = false
	record.Receipt.ResolutionAttestation = request.Attestation
	record.Receipt.FailureKind = "operator_resolved_failed"
	if err := s.journal.save(id, record); err != nil {
		return model.ExperimentReceipt{}, err
	}
	return copyReceipt(record.Receipt), nil
}
func experimentLimits(limits model.ExperimentLimits) model.ExperimentLimits {
	if limits.WallSeconds == 0 {
		limits.WallSeconds = 300
	}
	if limits.MaxOutputBytes == 0 {
		limits.MaxOutputBytes = 1 << 20
	}
	if limits.MaxArtifactBytes == 0 {
		limits.MaxArtifactBytes = 16 << 20
	}
	if limits.MaxArtifactFiles == 0 {
		limits.MaxArtifactFiles = 128
	}
	return limits
}
func (s *experimentService) prerequisite(contract model.ExperimentContract, bundle model.ProgramBundle) error {
	if contract.ExternalAuthority.Network {
		return errors.New("network-capable isolated experiment runtime is not enrolled")
	}
	if len(contract.ExternalAuthority.Accounts) > 0 || len(contract.ExternalAuthority.Credentials) > 0 || len(contract.ExternalAuthority.Hardware) > 0 || contract.ExternalAuthority.Spend != 0 {
		return errors.New("accounts, credentials, hardware or spend authority is not configured")
	}
	if contract.Environment.ImageIdentity != "" && contract.Environment.ImageIdentity != s.options.DeclaredImageIdentity {
		return errors.New("declared runtime image identity differs from approved contract")
	}
	for _, tool := range contract.Environment.Tools {
		if tool == "" || strings.ContainsAny(tool, "\x00\r\n") {
			return errors.New("invalid required tool")
		}
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %q is unavailable", tool)
		}
	}
	for _, name := range contract.Environment.Prerequisites {
		if !contains(s.options.Prerequisites, name) {
			return fmt.Errorf("required prerequisite %q is not operator-attested", name)
		}
	}
	for _, command := range contract.Commands {
		name := command.Profile
		if name == "" {
			name = contract.Environment.Profile
		}
		profile, ok := s.options.Profiles[name]
		if !ok || len(profile.Argv) == 0 {
			return fmt.Errorf("experiment profile %q is not configured", name)
		}
		if profile.ImageIdentity != "" && profile.ImageIdentity != s.options.DeclaredImageIdentity {
			return errors.New("profile image identity differs from declared runner image")
		}
		if len(profile.Hardware) > 0 {
			return errors.New("profile requires hardware unavailable to this runner")
		}
		if profile.Network {
			return errors.New("network-enabled profile unavailable in credential-free runtime")
		}
		if len(command.Args) > 128 {
			return errors.New("too many profile arguments")
		}
		for _, arg := range command.Args {
			if !approvedArgument(profile.AllowedArgs, arg, bundle) {
				return fmt.Errorf("argument %q is not allowed by operator profile or approved bundle", arg)
			}
		}
	}
	return nil
}
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func safeMember(path string) bool {
	if path == "" || strings.ContainsAny(path, "\\\x00") || filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." || part == "." || part == ".git" || part == ".openfactory" || part == "" {
			return false
		}
	}
	return true
}

// AllowedArgs consists of explicit literals or path.Match patterns. Wildcards
// can select only explicitly frozen safe relative bundle members, never source
// code strings, arbitrary flags, host paths, or mutable workspace inputs.
func approvedArgument(patterns []string, arg string, bundle model.ProgramBundle) bool {
	if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
		return false
	}
	declared := false
	for _, file := range bundle.Files {
		if file.Path == arg {
			declared = true
			break
		}
	}
	for _, pattern := range patterns {
		wildcard := strings.ContainsAny(pattern, "*?[")
		match, err := path.Match(pattern, arg)
		if err != nil || !match {
			continue
		}
		if wildcard || strings.Contains(arg, "/") {
			if !safeMember(arg) || !declared {
				continue
			}
		}
		return true
	}
	return false
}
func sortedUnique(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	values = nil
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func (s *experimentService) cancelIntent(id string, request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	normalized, err := NormalizeRequest(request)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || request.RunID != id || request.ContractSHA != normalized.ContractSHA || request.RequestSHA == "" || normalized.RequestSHA != request.RequestSHA {
		if _, exists := s.records[id]; exists {
			return model.ExperimentReceipt{}, &ExperimentError{Code: "idempotency_conflict", Message: "cancellation request conflicts with durable run identity", Status: 409}
		}
		if s.capabilityError != nil {
			return model.ExperimentReceipt{}, s.capabilityError
		}
		return model.ExperimentReceipt{}, &ExperimentError{Code: "invalid_contract", Message: "cancellation requires the exact immutable run request", Status: 400}
	}
	if record, ok := s.records[id]; ok {
		if record.Request.RequestSHA != request.RequestSHA {
			return model.ExperimentReceipt{}, &ExperimentError{Code: "idempotency_conflict", Message: "cancellation request conflicts with durable run identity", Status: 409}
		}
		if cancel := s.cancels[id]; cancel != nil {
			cancel()
			record.Receipt.State = "cancelling"
			if err := s.journal.save(id, record); err != nil {
				return model.ExperimentReceipt{}, err
			}
		}
		return copyReceipt(record.Receipt), nil
	}
	if err := s.capability(); err != nil {
		return model.ExperimentReceipt{}, err
	}
	now := time.Now().UTC()
	receipt := model.ExperimentReceipt{RunID: id, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID, Revision: request.Revision, StepID: request.StepID, AttemptID: request.AttemptID, ContractSHA: request.ContractSHA, BundleManifestSHA: request.Bundle.ManifestSHA, State: "cancelled", FinishedAt: &now, ExecutionOutcome: "cancelled", ObservationOutcome: "not_evaluated", HypothesisOutcome: "inconclusive", FailureKind: "cancelled", Detail: "cancelled before executor launch; durable tombstone prevents any late start", DeclaredImageIdentity: s.options.DeclaredImageIdentity, AttestationScope: "runner durably recorded cancellation without launching a process"}
	record := &experimentRecord{Request: request, Receipt: receipt}
	if err := s.journal.save(id, record); err != nil {
		return model.ExperimentReceipt{}, err
	}
	s.records[id] = record
	return copyReceipt(receipt), nil
}
