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
	"testing"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

func TestServeResearchBindFailureStartsNoWorkersAndReleasesDashboard(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	dashboardAddr := availableDashboardAddress(t)
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	c := model.Config{StateDir: t.TempDir(), PollSeconds: 3600}
	// A worker or recovery transaction would dereference the nil store.
	err = serve(context.Background(), "unused.yaml", c, nil, io.Discard, dashboardAddr, false, occupied.Addr().String(), false, "", false)
	var networkErr *net.OpError
	if !errors.As(err, &networkErr) || networkErr.Op != "listen" {
		t.Fatalf("expected prebind failure, got %v", err)
	}
	replacement, err := net.Listen("tcp", dashboardAddr)
	if err != nil {
		t.Fatalf("dashboard listener survived broker prebind failure: %v", err)
	}
	replacement.Close()
	assertRuntimeLockReleased(t, c.StateDir)
}

func TestServeResearchRejectsPublicBindWithoutExplicitOptIn(t *testing.T) {
	for _, address := range []string{"0.0.0.0:38888", "[::]:38888", "192.0.2.1:38888", "localhost:38888", "127.0.0.1:0"} {
		t.Run(address, func(t *testing.T) {
			c := model.Config{StateDir: t.TempDir(), PollSeconds: 3600}
			if err := serve(context.Background(), "unused.yaml", c, nil, io.Discard, "", false, address, false, "", false); err == nil {
				t.Fatal("unsafe broker bind accepted")
			}
			if _, err := os.Stat(filepath.Join(c.StateDir, "daemon.lock")); !os.IsNotExist(err) {
				t.Fatalf("bind validation must precede daemon ownership: %v", err)
			}
		})
	}
}

func TestServePrivateResearchAuthAndCoordinatedDrain(t *testing.T) {
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	c := model.Config{StateDir: t.TempDir(), PollSeconds: 3600, Discovery: model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{})}
	db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	logs := make(runtimeLog, 16)
	done := make(chan error, 1)
	listenAddress := availableDashboardAddress(t)
	go func() { done <- serve(ctx, "unused.yaml", c, db, logs, "", false, listenAddress, false, "", false) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("broker/engine did not drain")
		}
	})
	address := waitRuntimeLog(t, logs, "openfactory private research broker listening on ")
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for _, path := range []string{"/v1/research/search", "/v1/research/capture", "/v1/research/github", "/v1/research/sources/not-a-source"} {
		req, err := http.NewRequest(http.MethodPost, "http://"+address+path, strings.NewReader(`{"request_id":"no-capability"}`))
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "approval_required") {
			t.Fatalf("unauthenticated broker route %s: %d %s", path, response.StatusCode, body)
		}
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("cancellation returned error: %v", err)
		}
		done <- nil
	case <-time.After(8 * time.Second):
		t.Fatal("private broker did not stop")
	}
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("private broker listener survived daemon shutdown")
	}
	assertRuntimeLockReleased(t, c.StateDir)
	if _, err = db.List(context.Background()); err != nil {
		t.Fatalf("runtime closed caller-owned DB before handoff: %v", err)
	}
}

func TestServeResearchComponentCancelsAndDrainsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	done := make(chan error, 1)
	go func() {
		done <- serveHTTPComponent(ctx, listener, handler, "private research broker", 35*time.Second, 30*time.Second)
	}()
	clientDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 8 * time.Second}
		resp, e := client.Get("http://" + listener.Addr().String())
		if e == nil {
			resp.Body.Close()
		}
		clientDone <- e
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Error("broker failed to drain active request")
		}
		select {
		case <-clientDone:
		case <-time.After(8 * time.Second):
			t.Error("broker client did not finish")
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("broker request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("broker request context was not canceled")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("broker released service ownership before request returned: %v", err)
	default:
	}
}
