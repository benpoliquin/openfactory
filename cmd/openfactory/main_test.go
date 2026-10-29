package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

type runtimeLog chan string

func (l runtimeLog) Write(p []byte) (int, error) {
	l <- string(p)
	return len(p), nil
}

func waitRuntimeLog(t *testing.T, logs runtimeLog, prefix string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-logs:
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		case <-deadline:
			t.Fatalf("openfactory did not log %q", prefix)
		}
	}
}

func assertRuntimeLockReleased(t *testing.T, stateDir string) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(stateDir, "daemon.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("openfactory retained its process lock: %v", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}

func availableDashboardAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func TestServeOptionalDashboardAndCancellation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name, addr := "disabled", ""
		if enabled {
			name, addr = "enabled", availableDashboardAddress(t)
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
			c := model.Config{StateDir: t.TempDir(), PollSeconds: 3600}
			db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			logs := make(runtimeLog, 8)
			done := make(chan struct{})
			var runErr error
			go func() {
				runErr = serve(ctx, filepath.Join(c.StateDir, "unused.yaml"), c, db, logs, addr, false, "", false, "", false)
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(8 * time.Second):
					t.Error("openfactory did not drain after cancellation")
				}
			})
			var boundAddr string
			if enabled {
				boundAddr = waitRuntimeLog(t, logs, "openfactory dashboard listening on ")
				client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
				for _, path := range []string{"/healthz", "/api/status"} {
					response, err := client.Get("http://" + boundAddr + path)
					if err != nil {
						t.Fatalf("GET %s: %v", path, err)
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("GET %s returned %d", path, response.StatusCode)
					}
				}
			}
			line := waitRuntimeLog(t, logs, "")
			if !strings.HasPrefix(line, "openfactory running;") {
				t.Fatalf("unexpected startup log: %s", line)
			}
			cancel()
			select {
			case <-done:
				if runErr != nil {
					t.Fatalf("canceled openfactory returned an error: %v", runErr)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("openfactory did not drain after cancellation")
			}
			if enabled {
				conn, err := net.DialTimeout("tcp", boundAddr, time.Second)
				if err == nil {
					conn.Close()
					t.Fatal("view listener remained open after openfactory stopped")
				}
			}
			assertRuntimeLockReleased(t, c.StateDir)
			if _, err := db.List(context.Background()); err != nil {
				t.Fatalf("serve closed its caller-owned database: %v", err)
			}
		})
	}
}

func TestServeDashboardBindFailureStartsNoWorkers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	c := model.Config{StateDir: t.TempDir(), PollSeconds: 3600}
	// A worker started before the failed bind would dereference this nil store.
	err = serve(context.Background(), "unused.yaml", c, nil, io.Discard, listener.Addr().String(), false, "", false, "", false)
	var networkErr *net.OpError
	if !errors.As(err, &networkErr) || networkErr.Op != "listen" {
		t.Fatalf("expected fatal listener bind error, got %v", err)
	}
	assertRuntimeLockReleased(t, c.StateDir)
}

func TestServeComponentFailureStopsDashboard(t *testing.T) {
	dashboardAddr := availableDashboardAddress(t)
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	c := model.Config{StateDir: t.TempDir(), PollSeconds: 1}
	db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	logs := make(runtimeLog, 8)
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = serve(ctx, filepath.Join(c.StateDir, "missing.yaml"), c, db, logs, dashboardAddr, false, "", false, "", false)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("openfactory did not drain after configuration failure")
		}
	})
	addr := waitRuntimeLog(t, logs, "openfactory dashboard listening on ")
	select {
	case <-done:
		if runErr == nil || !strings.Contains(runErr.Error(), "operator configuration became invalid") {
			t.Fatalf("configuration failure was not propagated: %v", runErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("configuration failure did not stop openfactory")
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("view listener survived another supervised component's failure")
	}
	assertRuntimeLockReleased(t, c.StateDir)
}

func TestServeRejectsUnsafeDashboardBeforeStartingWork(t *testing.T) {
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	for _, address := range []string{"0.0.0.0:38887", "[::]:38887", "192.0.2.1:38887", "localhost:38887", "127.0.0.1:0", "127.0.0.1"} {
		t.Run(address, func(t *testing.T) {
			c := model.Config{StateDir: t.TempDir(), PollSeconds: 3600}
			// Any recovery or worker access would dereference this nil store.
			if err := serve(context.Background(), "unused.yaml", c, nil, io.Discard, address, false, "", false, "", false); err == nil {
				t.Fatal("unsafe dashboard listener accepted")
			}
			if _, err := os.Stat(filepath.Join(c.StateDir, "daemon.lock")); !os.IsNotExist(err) {
				t.Fatalf("listener validation must precede daemon ownership: %v", err)
			}
		})
	}
}

type failedDashboardListener struct {
	net.Listener
	err error
}

func (l failedDashboardListener) Accept() (net.Conn, error) { return nil, l.err }

func TestServeDashboardPropagatesServingFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	failure := errors.New("dashboard accept failed")
	err = serveDashboard(context.Background(), failedDashboardListener{Listener: listener, err: failure}, http.NotFoundHandler())
	if !errors.Is(err, failure) {
		t.Fatalf("HTTP serving failure was not propagated: %v", err)
	}
}

func TestServeDashboardCancelsAndDrainsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = serveDashboard(ctx, listener, handler)
		close(done)
	}()
	clientDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		clientDone <- err
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		select {
		case <-done:
			if serveErr != nil {
				t.Errorf("HTTP shutdown failed: %v", serveErr)
			}
		case <-time.After(8 * time.Second):
			t.Error("HTTP worker did not drain its active request")
		}
		select {
		case err := <-clientDone:
			if err != nil {
				t.Errorf("active HTTP request failed: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("active HTTP client did not finish")
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("openfactory cancellation did not reach the HTTP request context")
	}
	select {
	case <-done:
		t.Fatalf("HTTP worker exited before its active request drained: %v", serveErr)
	default:
	}
}
