package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"openfactory/internal/model"
)

func TestExperimentExecutionPolicyBindsApprovedProfileBeforeIntent(t *testing.T) {
	service, request, _, _ := experimentFixture(t, fencedDelivery)
	service.options.Profiles = map[string]model.ExperimentProfile{"cpu": {Argv: []string{"sh", "-c", "exit 99"}, AllowedArgs: []string{"experiments/*/run.sh"}}}
	_, err := service.start(request)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "policy_changed" {
		t.Fatalf("stale runner profile accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(service.journal.root, request.RunID+".json")); !os.IsNotExist(err) {
		t.Fatalf("mismatched execution policy created intent: %v", err)
	}
}
func TestExperimentCapturedPipeDeadlineIsNotPrerequisiteFailure(t *testing.T) {
	service, request, _, _ := experimentFixture(t, "sleep 20 &\nprintf '{\"version\":2}' > result.json\n")
	if _, err := service.start(request); err != nil {
		t.Fatal(err)
	}
	receipt := awaitExperiment(t, service, request.RunID)
	if receipt.State != "failed" || receipt.FailureKind != "capture_incomplete" || receipt.ObservationOutcome != "incomplete" || receipt.Conclusive || receipt.Accepted || len(receipt.Commands) != 1 || receipt.Commands[0].ExitCode == nil || *receipt.Commands[0].ExitCode != 0 {
		t.Fatalf("lost capture reported as startup failure or proof: %+v", receipt)
	}
}
func TestExperimentExecutesApprovedArgvZeroWhileHashingResolvedBinary(t *testing.T) {
	service, request, _, _ := experimentFixture(t, fencedDelivery)
	service.options.Profiles = map[string]model.ExperimentProfile{"cpu": {Argv: []string{"sh", "-c", `printf '{"version":2}' > result.json; printf '%s' "$0" > argv0.txt`}}}
	request.Contract.Commands[0].Args = nil
	request.ContractSHA = ""
	request.RunID = ""
	request.RequestSHA = ""
	request.ExecutionPolicySHA = model.ExperimentPolicySHA(service.options.Profiles, service.options.Limits)
	var err error
	request, err = NormalizeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.start(request); err != nil {
		t.Fatal(err)
	}
	receipt := awaitExperiment(t, service, request.RunID)
	argv0, err := os.ReadFile(filepath.Join(service.journal.root, "runs", request.RunID, "argv0.txt"))
	if err != nil || string(argv0) != "sh" || !receipt.Accepted || receipt.Commands[0].Argv[0] != "sh" || receipt.Commands[0].BinarySHA == "" {
		t.Fatalf("approved argv0 changed: argv0=%q receipt=%+v err=%v", argv0, receipt, err)
	}
}
func TestExperimentObservationCannotReuseFrozenInput(t *testing.T) {
	_, request, _, _ := experimentFixture(t, fencedDelivery)
	request.Contract.Observations[0].OutputPath = request.Contract.BundleFiles[0]
	request.ContractSHA = ""
	request.RunID = ""
	request.RequestSHA = ""
	if _, err := NormalizeRequest(request); err == nil {
		t.Fatal("accepted frozen input as experiment-produced observation")
	}
}
func TestExperimentCorruptJournalDisablesLaunchesButPreservesKnownStatus(t *testing.T) {
	service, request, options, source := experimentFixture(t, fencedDelivery)
	if _, err := service.start(request); err != nil {
		t.Fatal(err)
	}
	known := awaitExperiment(t, service, request.RunID)
	service.close()
	if err := os.WriteFile(filepath.Join(options.JournalDir, "broken.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := newExperimentService(context.Background(), filepath.Dir(source), options)
	if err != nil {
		t.Fatalf("corruption took down ordinary checks: %v", err)
	}
	defer restarted.close()
	restored, err := restarted.status(request.RunID)
	if err != nil || restored.RequestSHA != known.RequestSHA || !restored.Accepted {
		t.Fatalf("valid known identity lost to unrelated corruption: %+v %v", restored, err)
	}
	duplicate, err := restarted.start(request)
	if err != nil || duplicate.RequestSHA != known.RequestSHA {
		t.Fatalf("known idempotent identity unavailable: %+v %v", duplicate, err)
	}
	_, err = restarted.status(RunID("unknown", request.ContractSHA, 0))
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "operation_uncertain" {
		t.Fatalf("corrupt journal certified absence: %v", err)
	}
	next := request
	next.AttemptID = "next"
	next.RunID = ""
	next.RequestSHA = ""
	if _, err = restarted.start(next); !errors.As(err, &failure) || failure.Code != "operation_uncertain" {
		t.Fatalf("corrupt journal accepted new launch: %v", err)
	}
	if _, err = os.Stat(filepath.Join(options.JournalDir, "broken.json")); err != nil {
		t.Fatalf("corrupt ownership silently removed: %v", err)
	}
}
func TestExperimentOldMissingPolicyJournalFailsClosedWithoutCheckOutage(t *testing.T) {
	service, request, options, source := experimentFixture(t, fencedDelivery)
	request.ExecutionPolicySHA = ""
	receipt := model.ExperimentReceipt{RunID: request.RunID, RequestSHA: request.RequestSHA, ProgramID: request.ProgramID, Revision: request.Revision, StepID: request.StepID, AttemptID: request.AttemptID, ContractSHA: request.ContractSHA, BundleManifestSHA: request.Bundle.ManifestSHA, State: "intent"}
	if err := service.journal.save(request.RunID, experimentRecord{Request: request, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	service.close()
	restarted, err := newExperimentService(context.Background(), filepath.Dir(source), options)
	if err != nil {
		t.Fatalf("old record took down checks: %v", err)
	}
	defer restarted.close()
	_, err = restarted.status(request.RunID)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "operation_uncertain" {
		t.Fatalf("old policy-less record replayable or certified absent: %v", err)
	}
}

func TestExperimentCancelIntentDurablyFencesLateStartEvenWithChangedPolicy(t *testing.T) {
	service, request, options, source := experimentFixture(t, fencedDelivery)
	service.options.Profiles = nil
	cancelled, err := service.cancelIntent(request.RunID, request)
	if err != nil || cancelled.State != "cancelled" || cancelled.Conclusive || !cancelled.StartedAt.IsZero() {
		t.Fatalf("absent intent cancellation: %+v %v", cancelled, err)
	}
	late, err := service.start(request)
	if err != nil || late.State != "cancelled" {
		t.Fatalf("late start escaped cancellation tombstone: %+v %v", late, err)
	}
	service.close()
	restarted, err := newExperimentService(context.Background(), filepath.Dir(source), options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	late, err = restarted.start(request)
	if err != nil || late.State != "cancelled" {
		t.Fatalf("restart lost cancellation tombstone: %+v %v", late, err)
	}
	if _, err = os.Stat(filepath.Join(options.JournalDir, "runs", request.RunID)); !os.IsNotExist(err) {
		t.Fatalf("cancelled absent intent launched: %v", err)
	}
}

func TestExperimentCorruptJournalCannotCertifyAbsentCancellation(t *testing.T) {
	service, request, options, source := experimentFixture(t, fencedDelivery)
	service.close()
	if err := os.WriteFile(filepath.Join(options.JournalDir, "corrupt.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := newExperimentService(context.Background(), filepath.Dir(source), options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	_, err = restarted.cancelIntent(request.RunID, request)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "operation_uncertain" {
		t.Fatalf("corrupt journal certified no process: %v", err)
	}
	if _, err = os.Stat(filepath.Join(options.JournalDir, request.RunID+".json")); !os.IsNotExist(err) {
		t.Fatalf("corrupt ownership replaced with cancellation tombstone: %v", err)
	}
}

func TestExperimentNonzeroUsesOnlyCompleteDeclaredNegativeObservations(t *testing.T) {
	for _, test := range []struct {
		name, script string
		conclusive   bool
		hypothesis   string
	}{
		{"negative-observation", naiveDelivery + "exit 7\n", true, "falsified"},
		{"passing-observation", fencedDelivery + "exit 7\n", false, "inconclusive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, request, _, _ := experimentFixture(t, test.script)
			if _, err := service.start(request); err != nil {
				t.Fatal(err)
			}
			receipt := awaitExperiment(t, service, request.RunID)
			if receipt.Accepted || receipt.Conclusive != test.conclusive || receipt.HypothesisOutcome != test.hypothesis || receipt.ObservationOutcome != "complete" || receipt.ExecutionOutcome != "nonzero" {
				t.Fatalf("nonzero substituted for declared observation criteria: %+v", receipt)
			}
		})
	}
}
