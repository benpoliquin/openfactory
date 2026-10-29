package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestExperimentCorruptJournalLeavesOrdinarySocketChecksAvailable(t *testing.T) {
	fixture, request, options, source := experimentFixture(t, fencedDelivery)
	fixture.close()
	if err := os.WriteFile(filepath.Join(options.JournalDir, "broken.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "openfactory-exp-degraded-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "check.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeWithOptions(ctx, socket, filepath.Dir(source), options) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := NewClient(socket)
	defer client.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.http.Get("http://checker/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 204 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("corrupt experiment journal took down ordinary executor server")
		}
		time.Sleep(5 * time.Millisecond)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer checkCancel()
	output, err := Run(checkCtx, socket, source, model.Command{Argv: []string{"sh", "-c", "printf ordinary-check"}})
	if err != nil || output != "ordinary-check" {
		t.Fatalf("ordinary check unavailable: %q %v", output, err)
	}
	_, err = client.Status(checkCtx, request.RunID)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Code != "operation_uncertain" {
		t.Fatalf("corrupt journal certified absence: %v", err)
	}
	if _, err = os.Stat(filepath.Join(options.JournalDir, "broken.json")); err != nil {
		t.Fatal("corrupt ownership record removed")
	}
}
func TestExperimentMissingPythonModuleDoesNotFalsify(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	service, request, _, _ := experimentFixture(t, "python3 -c 'import openfactory_nonexistent_prerequisite_91d7bfe1'\n")
	if _, err := service.start(request); err != nil {
		t.Fatal(err)
	}
	receipt := awaitExperiment(t, service, request.RunID)
	if receipt.State != "failed" || receipt.Conclusive || receipt.Accepted || receipt.HypothesisOutcome != "inconclusive" || receipt.ObservationOutcome != "incomplete" || receipt.ExecutionOutcome != "nonzero" {
		t.Fatalf("missing module became negative finding: %+v", receipt)
	}
}
