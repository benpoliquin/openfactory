package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
)

func experimentFixture(t *testing.T, script string) (*experimentService, model.ExperimentRunRequest, Options, string) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(shared, "author")
	member := "experiments/arrival-order/run.sh"
	if err := os.MkdirAll(filepath.Join(source, "experiments", "arrival-order"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, member), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	options := Options{JournalDir: filepath.Join(root, "journal"), ArtifactRoot: filepath.Join(root, "artifacts"), Profiles: map[string]model.ExperimentProfile{"cpu": {Argv: []string{"sh"}, AllowedArgs: []string{"experiments/*/run.sh"}}}, DeclaredImageIdentity: "test-image@sha256:operator-declaration"}
	cas := artifacts.New(options.ArtifactRoot)
	bundle, err := cas.ImportBundle(context.Background(), source, []string{member}, model.BundleLimits{MaxFiles: 128, MaxFileBytes: 8 << 20, MaxTotalBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	contract := model.ExperimentContract{ID: "arrival-order", Hypothesis: "version never decreases under duplicate reordered delivery", Falsification: "final version is less than maximum delivered version", Environment: model.ExperimentEnvironment{Profile: "cpu", ImageIdentity: options.DeclaredImageIdentity}, BundleManifestSHA: bundle.ManifestSHA, BundleFiles: []string{member}, Commands: []model.ExperimentCommand{{Profile: "cpu", Args: []string{member}}}, Observations: []model.ExperimentObservationSpec{{Name: "version", Type: "integer", Unit: "version", OutputPath: "result.json", JSONPointer: "/version"}}, Acceptance: []model.ExperimentPredicate{{Observation: "version", Operator: "eq", Value: json.RawMessage("2"), Unit: "version"}}, Limits: model.ExperimentLimits{WallSeconds: 5, MaxOutputBytes: 4096, MaxArtifactBytes: 4096, MaxArtifactFiles: 8}}
	request, err := NormalizeRequest(model.ExperimentRunRequest{ExecutionPolicySHA: model.ExperimentPolicySHA(options.Profiles, options.Limits), ProgramID: "program", Revision: 1, StepID: "experiment-step", AttemptID: "attempt", Contract: contract, Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	service, err := newExperimentService(context.Background(), shared, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.close)
	return service, request, options, source
}
func awaitExperiment(t *testing.T, service *experimentService, id string) model.ExperimentReceipt {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := service.status(id)
		if err != nil {
			t.Fatal(err)
		}
		if terminal(receipt.State) {
			return receipt
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("experiment did not finish")
	return model.ExperimentReceipt{}
}

const naiveDelivery = "state=0\nfor version in 2 1 2 1; do state=$version; done\nprintf '{\"version\":%s}' \"$state\" > result.json\n"
const fencedDelivery = "state=0\nfor version in 2 1 2 1; do if [ \"$version\" -gt \"$state\" ]; then state=$version; fi; done\nprintf '{\"version\":%s}' \"$state\" > result.json\n"

func TestExperimentReorderedDeliveryFalsifiesAndCorrectedBundlePasses(t *testing.T) {
	for _, test := range []struct {
		name, script string
		accepted     bool
	}{{"arrival-order", naiveDelivery, false}, {"version-fence", fencedDelivery, true}} {
		t.Run(test.name, func(t *testing.T) {
			service, request, _, _ := experimentFixture(t, test.script)
			started, err := service.start(request)
			if err != nil {
				t.Fatal(err)
			}
			receipt := awaitExperiment(t, service, started.RunID)
			if receipt.State != "completed" || !receipt.Conclusive || receipt.Accepted != test.accepted || receipt.ExecutionOutcome != "completed" || receipt.ObservationOutcome != "complete" {
				t.Fatalf("unexpected outcome: %+v", receipt)
			}
			if len(receipt.Commands) != 1 || receipt.Commands[0].BinarySHA == "" || receipt.Commands[0].ExitCode == nil || *receipt.Commands[0].ExitCode != 0 || receipt.Artifacts.ManifestSHA == "" {
				t.Fatalf("missing observed evidence: %+v", receipt)
			}
			if !strings.Contains(receipt.AttestationScope, "operator-declared") || !strings.Contains(receipt.AttestationScope, "shares checker UID") {
				t.Fatal("runtime scope overclaims isolation or observed image")
			}
		})
	}
}
func TestExperimentPrivateCopyAndIdempotence(t *testing.T) {
	service, request, _, source := experimentFixture(t, "sleep 0.1\n"+fencedDelivery)
	started, err := service.start(request)
	if err != nil {
		t.Fatal(err)
	}
	// The author workspace no longer has approved code. Only the CAS snapshot may
	// be executed, and the private copy must not be the author path.
	if err = os.WriteFile(filepath.Join(source, "experiments", "arrival-order", "run.sh"), []byte("exit 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.start(request)
	if err != nil || duplicate.RunID != started.RunID || duplicate.RequestSHA != started.RequestSHA {
		t.Fatalf("duplicate run: %+v %v", duplicate, err)
	}
	receipt := awaitExperiment(t, service, started.RunID)
	if !receipt.Accepted {
		t.Fatalf("author mutation changed approved execution: %+v", receipt)
	}
	private := filepath.Join(service.journal.root, "runs", request.RunID, "experiments", "arrival-order", "run.sh")
	data, err := os.ReadFile(private)
	if err != nil || bytes.Contains(data, []byte("exit 99")) {
		t.Fatalf("private execution copy: %q %v", data, err)
	}
	conflict := request
	conflict.ProgramID = "other-program"
	conflict.RequestSHA = ""
	_, err = service.start(conflict)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Status != 409 {
		t.Fatalf("conflict not 409: %v", err)
	}
}
func TestExperimentIntentRestartNeverReplaysAndResolutionBindsExactHash(t *testing.T) {
	service, request, options, source := experimentFixture(t, fencedDelivery)
	receipt := model.ExperimentReceipt{RunID: request.RunID, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID, Revision: request.Revision, StepID: request.StepID, AttemptID: request.AttemptID, ContractSHA: request.ContractSHA, BundleManifestSHA: request.Bundle.ManifestSHA, State: "intent"}
	if err := service.journal.save(request.RunID, experimentRecord{Request: request, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	service.close()
	restarted, err := newExperimentService(context.Background(), filepath.Dir(source), options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	duplicate, err := restarted.start(request)
	if err != nil || duplicate.State != "uncertain" || duplicate.Conclusive || duplicate.Accepted {
		t.Fatalf("restart replayed or concluded intent: %+v %v", duplicate, err)
	}
	if _, err = os.Stat(filepath.Join(options.JournalDir, "runs", request.RunID)); !os.IsNotExist(err) {
		t.Fatalf("uncertain intent executed: %v", err)
	}
	if _, err = restarted.resolve(request.RunID, resolveRequest{RequestSHA: strings.Repeat("0", 64), Attestation: "operator verified no owned process remains"}); err == nil {
		t.Fatal("accepted wrong request hash")
	}
	resolved, err := restarted.resolve(request.RunID, resolveRequest{RequestSHA: request.RequestSHA, Attestation: "operator verified no owned process remains"})
	if err != nil || resolved.State != "resolved_failed" || resolved.Conclusive || resolved.Accepted {
		t.Fatalf("bad failure resolution: %+v %v", resolved, err)
	}
}
func TestExperimentCancellationOnlyKillsOwnedRun(t *testing.T) {
	service, request, _, _ := experimentFixture(t, "(sleep 0.3; printf child-survived > child-marker) &\nprintf started > started\nwait\n"+fencedDelivery)
	first, err := service.start(request)
	if err != nil {
		t.Fatal(err)
	}
	second := request
	second.AttemptID = "another-attempt"
	second.RunID = ""
	second.RequestSHA = ""
	second, err = NormalizeRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.start(second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	startedPath := filepath.Join(service.journal.root, "runs", first.RunID, "started")
	for {
		if _, statErr := os.Stat(startedPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned process never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = service.cancelRun(first.RunID); err != nil {
		t.Fatal(err)
	}
	cancelled := awaitExperiment(t, service, first.RunID)
	other := awaitExperiment(t, service, second.RunID)
	if cancelled.State != "cancelled" || cancelled.Accepted || cancelled.Conclusive || !other.Accepted {
		t.Fatalf("cancel crossed run ownership: cancelled=%+v other=%+v", cancelled, other)
	}
	if _, err = os.Stat(filepath.Join(service.journal.root, "runs", first.RunID, "child-marker")); !os.IsNotExist(err) {
		t.Fatalf("cancelled descendant survived: %v", err)
	}
	if _, err = os.Stat(filepath.Join(service.journal.root, "runs", second.RunID, "child-marker")); err != nil {
		t.Fatalf("other run descendant was killed: %v", err)
	}
}
func TestExperimentTransportLossDoesNotCancelDurableStart(t *testing.T) {
	service, request, _, _ := experimentFixture(t, "sleep 0.05\n"+fencedDelivery)
	mux := http.NewServeMux()
	service.routes(mux)
	body, _ := json.Marshal(request)
	ctx, cancel := context.WithCancel(context.Background())
	httpRequest := httptest.NewRequest("POST", "/experiments", bytes.NewReader(body)).WithContext(ctx)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httpRequest)
	cancel()
	if response.Code != 200 {
		t.Fatalf("start HTTP %d: %s", response.Code, response.Body.String())
	}
	receipt := awaitExperiment(t, service, request.RunID)
	if !receipt.Accepted {
		t.Fatalf("request cancellation orphaned execution: %+v", receipt)
	}
}
func TestExperimentIncompleteEvidenceNeverPasses(t *testing.T) {
	tests := []struct {
		name, script, kind string
		mutate             func(*model.ExperimentRunRequest)
	}{
		{"missing-tool", fencedDelivery, "prerequisite_missing", func(request *model.ExperimentRunRequest) {
			request.Contract.Environment.Tools = []string{"openfactory-impossible-tool-7ccf1"}
		}},
		{"missing-gpu", fencedDelivery, "prerequisite_missing", func(request *model.ExperimentRunRequest) {
			request.Contract.ExternalAuthority.Hardware = []string{"gpu"}
		}},
		{"missing-output", "true\n", "observation_incomplete", nil},
		{"ambiguous-json", "printf '{\"version\":2,\"version\":1}' > result.json\n", "observation_incomplete", nil},
		{"truncated", "while :; do printf aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done\n", "output_truncated", nil},
		{"timeout", "sleep 20 & wait\n", "timeout", func(request *model.ExperimentRunRequest) { request.Contract.Limits.WallSeconds = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, request, _, _ := experimentFixture(t, test.script)
			if test.mutate != nil {
				test.mutate(&request)
				request.RunID = ""
				request.RequestSHA = ""
				request.ContractSHA = ""
				var err error
				request, err = NormalizeRequest(request)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.start(request); err != nil {
				t.Fatal(err)
			}
			receipt := awaitExperiment(t, service, request.RunID)
			if receipt.Accepted || receipt.Conclusive || receipt.FailureKind != test.kind || receipt.HypothesisOutcome != "inconclusive" {
				t.Fatalf("infrastructure/unknown treated as proof: %+v", receipt)
			}
		})
	}
}
func TestExperimentKnownNonzeroWithoutObservationsIsInconclusive(t *testing.T) {
	service, request, _, _ := experimentFixture(t, "exit 7\n")
	if _, err := service.start(request); err != nil {
		t.Fatal(err)
	}
	receipt := awaitExperiment(t, service, request.RunID)
	if receipt.State != "failed" || receipt.Conclusive || receipt.Accepted || receipt.ExecutionOutcome != "nonzero" || receipt.ObservationOutcome != "incomplete" || receipt.HypothesisOutcome != "inconclusive" {
		t.Fatalf("nonzero execution invented negative finding from missing evidence: %+v", receipt)
	}
}
func TestExperimentRejectsBadBundleBeforeExecution(t *testing.T) {
	service, request, _, _ := experimentFixture(t, fencedDelivery)
	request.Bundle.Files[0].ContentSHA = strings.Repeat("a", 64)
	request.RequestSHA = ""
	if _, err := service.start(request); err != nil {
		t.Fatal(err)
	}
	receipt := awaitExperiment(t, service, request.RunID)
	if receipt.Conclusive || receipt.Accepted || receipt.FailureKind != "evidence_invalid" || len(receipt.Commands) != 0 {
		t.Fatalf("altered manifest executed: %+v", receipt)
	}
}
func TestExperimentChecksOnlyModeHasClearCapabilityError(t *testing.T) {
	service, err := newExperimentService(context.Background(), t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.close()
	_, err = service.start(model.ExperimentRunRequest{})
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "prerequisite_missing" || !strings.Contains(failure.Message, "--journal-root") {
		t.Fatalf("missing journal capability unclear: %v", err)
	}
}
func TestExperimentOutputRejectsSymlinksAndTypedPredicatesBindUnits(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "result.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readOutput(root, "result.json", 1024); err == nil {
		t.Fatal("followed output symlink")
	}
	contract := model.ExperimentContract{Acceptance: []model.ExperimentPredicate{{Observation: "n", Operator: "ge", Value: json.RawMessage("2"), Unit: "seconds"}}}
	_, err := evaluatePredicates(contract, []model.ExperimentObservation{{Name: "n", Unit: "bytes", Value: json.RawMessage("3")}})
	if err == nil {
		t.Fatal("compared different units")
	}
}

func TestExperimentProfilePatternsOnlySelectFrozenMembers(t *testing.T) {
	bundle := model.ProgramBundle{Files: []model.ProgramBundleFile{{Path: "experiments/E01/main.py"}}}
	for _, test := range []struct {
		arg      string
		patterns []string
		allowed  bool
	}{
		{"experiments/E01/main.py", []string{"experiments/*/main.py"}, true},
		{"experiments/E02/main.py", []string{"experiments/*/main.py"}, false},
		{"experiments/E01/../../secret", []string{"*"}, false},
		{"/etc/passwd", []string{"*"}, false},
		{"-c", []string{"*"}, false},
		{"print('unapproved model code')", []string{"*"}, false},
		{"--strict", []string{"--strict"}, true},
		{"experiments/E01/main.py", nil, false},
	} {
		if got := approvedArgument(test.patterns, test.arg, bundle); got != test.allowed {
			t.Fatalf("argument %q patterns %v: got %v want %v", test.arg, test.patterns, got, test.allowed)
		}
	}
}

func TestExperimentRejectsDuplicateJSONRequestFields(t *testing.T) {
	service, _, _, _ := experimentFixture(t, fencedDelivery)
	mux := http.NewServeMux()
	service.routes(mux)
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/experiments", strings.NewReader(`{"program_id":"one","program_id":"two"}`))
	mux.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatalf("ambiguous request accepted: HTTP %d", response.Code)
	}
}

func TestExperimentRunnerEnforcesLoweredConfiguredLimitsWithoutRerunningKnownIdentity(t *testing.T) {
	service, request, _, _ := experimentFixture(t, fencedDelivery)
	service.options.Limits = model.ExperimentLimits{WallSeconds: 1}
	_, err := service.start(request)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "policy_changed" {
		t.Fatalf("lowered runner cap ignored: %v", err)
	}
	if _, err = os.Stat(filepath.Join(service.journal.root, "runs", request.RunID)); !os.IsNotExist(err) {
		t.Fatalf("over-cap request executed: %v", err)
	}
	service.options.Limits = model.ExperimentLimits{}
	if _, err = service.start(request); err != nil {
		t.Fatal(err)
	}
	receipt := awaitExperiment(t, service, request.RunID)
	if !receipt.Accepted {
		t.Fatalf("approved run failed: %+v", receipt)
	}
	service.options.Limits = model.ExperimentLimits{WallSeconds: 1}
	replay, err := service.start(request)
	if err != nil || replay.RequestSHA != request.RequestSHA || !replay.Accepted {
		t.Fatalf("lowered cap lost existing durable identity: %+v %v", replay, err)
	}
}
