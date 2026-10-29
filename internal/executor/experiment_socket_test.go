package executor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExperimentUnixClientReceiptsSurviveServerRestart(t *testing.T) {
	fixture, request, options, source := experimentFixture(t, fencedDelivery)
	fixture.close()
	socketDir, err := os.MkdirTemp("/tmp", "openfactory-exp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "runner.sock")
	startServer := func() (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ServeWithOptions(ctx, socket, filepath.Dir(source), options) }()
		client := NewClient(socket)
		defer client.Close()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			req, _ := http.NewRequest("GET", "http://checker/health", nil)
			response, requestErr := client.http.Do(req)
			if requestErr == nil {
				response.Body.Close()
				if response.StatusCode == 204 {
					return cancel, done
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		t.Fatal("executor did not become healthy")
		return cancel, done
	}
	cancel, done := startServer()
	client := NewClient(socket)
	defer client.Close()
	started, err := client.Start(context.Background(), request)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		receipt, statusErr := client.Status(context.Background(), started.RunID)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if terminal(receipt.State) {
			if !receipt.Accepted {
				t.Fatalf("real socket experiment failed: %+v", receipt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket experiment unfinished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	cancel, done = startServer()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	restored, err := client.Start(context.Background(), request)
	if err != nil || !restored.Accepted || restored.RequestSHA != started.RequestSHA || restored.RunID != started.RunID {
		t.Fatalf("completed run lost on restart: %+v %v", restored, err)
	}
	// Different request, identical derived run ID must be a visible 409 conflict.
	conflict := request
	conflict.ProgramID = "different-program"
	conflict.RequestSHA = ""
	_, err = client.Start(context.Background(), conflict)
	var failure *ExperimentError
	if !errors.As(err, &failure) || failure.Status != 409 || failure.Code != "idempotency_conflict" {
		t.Fatalf("client lost conflict authority: %v", err)
	}
	next := request
	next.AttemptID = "cancel-before-start"
	next.RunID = ""
	next.RequestSHA = ""
	next, err = NormalizeRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := client.CancelIntent(context.Background(), next)
	if err != nil || tombstone.State != "cancelled" {
		t.Fatalf("socket absent-intent cancellation: %+v %v", tombstone, err)
	}
	late, err := client.Start(context.Background(), next)
	if err != nil || late.State != "cancelled" || len(late.Commands) != 0 {
		t.Fatalf("socket late start escaped cancellation: %+v %v", late, err)
	}
}
func TestExperimentJournalRejectsWorkspaceMountAndConcurrentServer(t *testing.T) {
	root := t.TempDir()
	if _, err := openJournal(filepath.Join(root, "journal"), root); err == nil {
		t.Fatal("accepted journal in shared workspace")
	}
	private := t.TempDir()
	journal, err := openJournal(filepath.Join(private, "journal"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.close()
	if other, err := openJournal(filepath.Join(private, "journal"), root); err == nil {
		other.close()
		t.Fatal("two servers acquired one durable execution journal")
	}
}
