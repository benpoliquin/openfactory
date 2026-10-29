package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestCheckRejectsEscapedWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, outside, link} {
		_, err := execute(context.Background(), root, request{path, model.Command{Argv: []string{"sh", "-c", "touch escaped"}}})
		if err == nil {
			t.Fatalf("accepted outside/root workspace %s", path)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("escaped execution occurred: %v", err)
	}
}

func TestCheckEnvironmentAndExitFailure(t *testing.T) {
	t.Setenv("OPENFACTORY_TEST_PRIVATE_KEY", "never-pass-me")
	root := t.TempDir()
	workspace := filepath.Join(root, "attempt")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	out, err := execute(context.Background(), root, request{workspace, model.Command{Argv: []string{"sh", "-c", "test -z \"${OPENFACTORY_TEST_PRIVATE_KEY:-}\" && test -d \"$HOME\" && printf clean"}}})
	if err != nil || out != "clean" {
		t.Fatalf("isolated check: %q %v", out, err)
	}
	out, err = execute(context.Background(), root, request{workspace, model.Command{Argv: []string{"sh", "-c", "printf failure-receipt; exit 7"}}})
	if err == nil || out != "failure-receipt" {
		t.Fatalf("lost failure: %q %v", out, err)
	}
}

func TestCheckTimeoutKillsChildGroup(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "attempt")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := execute(context.Background(), root, request{workspace, model.Command{Argv: []string{"sh", "-c", "sleep 20 & wait"}, TimeoutSeconds: 1}})
	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("timeout did not terminate group: %v", err)
	}
}

func TestExecutorSocketTransfersRealCommandResult(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "openfactory-executor-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	workspace := filepath.Join(dir, "workspaces", "attempt")
	if err = os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "check.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socket, filepath.Dir(workspace)) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("executor did not bind")
		}
		time.Sleep(5 * time.Millisecond)
	}
	output, err := Run(ctx, socket, workspace, model.Command{Argv: []string{"sh", "-c", "printf observed > proof.txt; cat proof.txt"}})
	if err != nil || output != "observed" {
		t.Fatalf("command result: %q %v", output, err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "proof.txt"))
	if err != nil || string(data) != "observed" {
		t.Fatalf("side effect: %q %v", data, err)
	}
	output, err = Run(ctx, socket, workspace, model.Command{Argv: []string{"sh", "-c", "printf rejected; exit 1"}})
	if err == nil || output != "rejected" {
		t.Fatalf("failed command accepted: %q %v", output, err)
	}
	output, err = Run(ctx, socket, workspace, model.Command{Argv: []string{"sh", "-c", "dd if=/dev/zero bs=4096 count=128 2>/dev/null"}})
	if err != nil || len(output) != 524288 || strings.Trim(output, "\x00") != "" {
		t.Fatalf("escaped response corrupted: length=%d error=%v", len(output), err)
	}
	_, err = Run(ctx, socket, filepath.Join(dir, "missing"), model.Command{Argv: []string{"true"}})
	var infra interface{ Infrastructure() bool }
	if !errors.As(err, &infra) || !infra.Infrastructure() {
		t.Fatalf("lost executor infrastructure classification: %v", err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCheckOutputBounded(t *testing.T) {
	var output capped
	data := []byte(strings.Repeat("x", outputLimit+17))
	if n, err := output.Write(data); n != len(data) || err != nil {
		t.Fatalf("write %d %v", n, err)
	}
	got := output.String()
	if len(got) > outputLimit+128 || !strings.HasSuffix(got, "[output truncated at 1 MiB]") {
		t.Fatal("unbounded or unmarked truncated output")
	}
}

func TestUnavailableExecutorDoesNotReportCandidateFailure(t *testing.T) {
	_, err := Run(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), "/workspaces/attempt", model.Command{Argv: []string{"true"}})
	var infra interface{ Infrastructure() bool }
	if !errors.As(err, &infra) || !infra.Infrastructure() {
		t.Fatalf("unavailable executor misclassified: %v", err)
	}
}
