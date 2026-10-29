package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/config"
)

func TestServeHTTPComponentClosedListenerCancellationBoundary(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled-%t", canceled), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			err = serveHTTPComponent(ctx, listener, http.NotFoundHandler(), "private research broker", time.Second, time.Second)
			if canceled && err != nil {
				t.Fatalf("canceled listener close is normal shutdown: %v", err)
			}
			if !canceled && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("unexpected listener closure must remain fatal: %v", err)
			}
		})
	}
}

func TestServeHTTPComponentDoesNotHideUnexpectedErrorDuringCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	failure := errors.New("injected unexpected accept failure")
	// Force Serve to report the actual failure before cancellation can cause its
	// listener registration to be rejected as an ordinary server shutdown.
	entered := make(chan struct{})
	failed := &notifyingFailedListener{Listener: listener, entered: entered, failure: failure}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveHTTPComponent(ctx, failed, http.NotFoundHandler(), "private research broker", time.Second, time.Second)
	}()
	select {
	case <-entered:
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not enter Accept")
	}
	select {
	case err = <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("unexpected failure was suppressed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("component failed to drain")
	}
}

type notifyingFailedListener struct {
	net.Listener
	entered chan struct{}
	failure error
}

func (l *notifyingFailedListener) Accept() (net.Conn, error) { close(l.entered); return nil, l.failure }

func TestWatchExperimentPolicyRejectsScopedChanges(t *testing.T) {
	for _, change := range []string{"profile", "limits", "invalid"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "openfactory.yaml")
			initial := fmt.Sprintf("state_dir: %q\nworkspace_dir: %q\npoll_seconds: 1\ndiscovery:\n  experiment_profiles:\n    reviewed:\n      argv: [/usr/bin/git, --version]\n      allowed_args: []\n", filepath.Join(root, "state"), filepath.Join(root, "workspaces"))
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			policy, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- watchExperimentPolicy(ctx, path, policy) }()
			changed := strings.Replace(initial, "--version", "status", 1)
			if change == "limits" {
				changed = initial + "  experiment_limits:\n    max_output_bytes: 524288\n"
			}
			if change == "invalid" {
				changed = "discovery: [invalid policy"
			}
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("changed/invalid experiment policy kept admission alive")
				}
			case <-time.After(4 * time.Second):
				t.Fatal("experiment policy watcher did not stop stale executor")
			}
		})
	}
}

func TestWatchExperimentPolicyIgnoresUnrelatedControllerIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "openfactory.yaml")
	body := fmt.Sprintf("state_dir: %q\nworkspace_dir: %q\npoll_seconds: 1\nastra_model: provider/first\ndiscovery:\n  broker_url: http://127.0.0.1:38888\n", filepath.Join(root, "state"), filepath.Join(root, "workspaces"))
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watchExperimentPolicy(ctx, path, policy) }()
	changed := strings.Replace(strings.Replace(body, "provider/first", "provider/second", 1), "127.0.0.1:38888", "openfactory:38888", 1)
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		t.Fatalf("unrelated model/broker identity stopped executor: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not drain on cancellation")
	}
}

func TestServeHTTPComponentShutdownCloseErrorBoundary(t *testing.T) {
	unexpected := errors.New("unexpected listener shutdown failure")
	for _, closeErr := range []error{net.ErrClosed, unexpected} {
		t.Run(closeErr.Error(), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			wrapped := &shutdownErrorListener{Listener: listener, closeErr: closeErr}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				w.WriteHeader(http.StatusNoContent)
			})
			done := make(chan error, 1)
			go func() {
				done <- serveHTTPComponent(ctx, wrapped, handler, "private research broker", time.Second, time.Second)
			}()
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				client := &http.Client{Timeout: 3 * time.Second}
				response, requestErr := client.Get("http://" + listener.Addr().String())
				if requestErr == nil {
					response.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("request never reached registered server listener")
			}
			cancel()
			select {
			case err = <-done:
				if errors.Is(closeErr, net.ErrClosed) && err != nil {
					t.Fatalf("canceled closed-listener shutdown should drain normally: %v", err)
				}
				if closeErr == unexpected && !errors.Is(err, unexpected) {
					t.Fatalf("unexpected Shutdown error was suppressed: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("component did not drain after listener shutdown")
			}
			select {
			case <-clientDone:
			case <-time.After(3 * time.Second):
				t.Fatal("client remained active after server drain")
			}
		})
	}
}

type shutdownErrorListener struct {
	net.Listener
	closeErr error
}

func (l *shutdownErrorListener) Close() error {
	l.Listener.Close()
	return l.closeErr
}
