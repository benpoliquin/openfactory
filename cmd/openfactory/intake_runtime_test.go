package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/config"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

func intakeRuntimeConfig(t *testing.T) model.Config {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "openfactory.yaml")
	data := "state_dir: " + filepath.Join(root, "state") + "\nworkspace_dir: " + filepath.Join(root, "workspaces") + "\npoll_seconds: 300\nintake:\n  enabled: true\n  external_origin: http://localhost\n  rp_id: localhost\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServeCommandValidatesBeforeStoreOrDaemonOwnership(t *testing.T) {
	for _, address := range []string{"0.0.0.0:38889", "[::]:38889", "192.0.2.1:38889", "localhost:38889", "127.0.0.1:0"} {
		t.Run(address, func(t *testing.T) {
			c := intakeRuntimeConfig(t)
			// Any recovery or worker access would dereference the nil store.
			if err := serve(context.Background(), "unused.yaml", c, nil, io.Discard, "", false, "", false, address, false); err == nil {
				t.Fatal("unsafe command listener accepted")
			}
			if _, err := os.Stat(filepath.Join(c.StateDir, "daemon.lock")); !os.IsNotExist(err) {
				t.Fatalf("validation acquired daemon ownership: %v", err)
			}
		})
	}
	t.Run("invalid policy", func(t *testing.T) {
		c := intakeRuntimeConfig(t)
		c.Intake.ExternalOrigin = "http://example.com"
		if err := serve(context.Background(), "unused.yaml", c, nil, io.Discard, "", false, "", false, availableDashboardAddress(t), false); err == nil {
			t.Fatal("invalid WebAuthn origin accepted")
		}
		if _, err := os.Stat(filepath.Join(c.StateDir, "daemon.lock")); !os.IsNotExist(err) {
			t.Fatalf("policy validation acquired daemon ownership: %v", err)
		}
	})
}

func TestServeCommandPrebindFailureStartsNoStoreWorkAndReleasesListeners(t *testing.T) {
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	c := intakeRuntimeConfig(t)
	dashboardAddr, researchAddr := availableDashboardAddress(t), availableDashboardAddress(t)
	// A recovery transaction, handler store access, or worker would panic.
	err = serve(context.Background(), "unused.yaml", c, nil, io.Discard, dashboardAddr, false, researchAddr, false, occupied.Addr().String(), false)
	var networkErr *net.OpError
	if !errors.As(err, &networkErr) || networkErr.Op != "listen" {
		t.Fatalf("expected command prebind failure, got %v", err)
	}
	for _, address := range []string{dashboardAddr, researchAddr} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("listener survived command prebind failure: %v", err)
		}
		listener.Close()
	}
	assertRuntimeLockReleased(t, c.StateDir)
}

func TestServeDisabledIntakeDoesNotOpenCommandListener(t *testing.T) {
	t.Setenv("OPENFACTORY_REQUIRE_CHECK_SOCKET", "")
	c := intakeRuntimeConfig(t)
	c.Intake.Enabled = false
	db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup is LIFO: the runtime cancellation/drain registered below must
	// finish before the caller-owned database is closed.
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	logs := make(runtimeLog, 32)
	done := make(chan error, 1)
	commandAddr := availableDashboardAddress(t)
	dashboardAddr := availableDashboardAddress(t)
	go func() {
		done <- serve(ctx, "unused.yaml", c, db, logs, dashboardAddr, false, "", false, commandAddr, false)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
			if _, err := db.List(context.Background()); err != nil {
				t.Errorf("runtime shutdown did not preserve caller-owned database: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("runtime did not drain")
		}
	})
	waitRuntimeLog(t, logs, "openfactory dashboard listening on ")
	listener, err := net.Listen("tcp", commandAddr)
	if err != nil {
		t.Fatalf("disabled intake occupied command listener: %v", err)
	}
	listener.Close()
}

func TestServeCommandComponentCancelsAndDrainsActiveRequest(t *testing.T) {
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
		done <- serveHTTPComponent(ctx, listener, handler, "intake command API", 35*time.Second, 30*time.Second)
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
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
			t.Error("command API did not drain")
		}
		select {
		case <-clientDone:
		case <-time.After(8 * time.Second):
			t.Error("command client did not finish")
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("command request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("command request context not canceled")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("command API returned before request drained: %v", err)
	default:
	}
}

func TestServeCommandClosedListenerCancellationBoundary(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unexpected", true: "cancelled"}[canceled], func(t *testing.T) {
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
			done := make(chan error, 1)
			go func() {
				done <- serveHTTPComponent(ctx, listener, http.NotFoundHandler(), "intake command API", time.Second, time.Second)
			}()
			select {
			case err := <-done:
				if canceled && err != nil {
					t.Fatalf("cancelled closure returned error: %v", err)
				}
				if !canceled && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("unexpected closure lost fatal error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("closed command listener did not return promptly")
			}
		})
	}
}

func TestOperatorCLIExplicitScopePrivatePairingAndRevocation(t *testing.T) {
	c := intakeRuntimeConfig(t)
	c.Projects = map[string]model.Project{"demo": {Trusted: true}, "untrusted": {}}
	c.Intake.PairingTTLSeconds = 90
	db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	invoke := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		err := runOperator(ctx, c, db, args, &out, io.Discard)
		return out.Bytes(), err
	}
	for _, args := range [][]string{
		{"create", "alice", "--label", "Alice"},
		{"create", "alice", "--label", "Alice", "--project", "unknown"},
		{"create", "alice", "--label", "Alice", "--project", "untrusted"},
	} {
		if _, err := invoke(args...); err == nil {
			t.Fatalf("invalid scope accepted: %v", args)
		}
	}
	if _, err := invoke("create", "alice", "--label", "Alice", "--project", "demo"); err != nil {
		t.Fatal(err)
	}
	operator, err := db.GetOperator(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(operator.ProjectScope) != 1 || operator.ProjectScope[0] != "demo" {
		t.Fatalf("scope expanded: %+v", operator)
	}
	devices, err := invoke("devices", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(devices)) != "[]" {
		t.Fatalf("creation implicitly enrolled a credential: %s", devices)
	}
	path := filepath.Join(t.TempDir(), "pairing.json")
	before := time.Now()
	out, err := invoke("pair", "alice", "--output", path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("pairing secret permissions: %v", info.Mode())
	}
	var pairing struct {
		PairingID  string    `json:"pairing_id"`
		URL        string    `json:"url"`
		ExpiresAt  time.Time `json:"expires_at"`
		TTLSeconds int       `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(raw, &pairing); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(pairing.URL, "#pairing=")
	if len(parts) != 2 || len(parts[1]) != 43 {
		t.Fatalf("missing enrollment secret: %s", raw)
	}
	if bytes.Contains(out, []byte(parts[1])) || bytes.Contains(out, []byte("#pairing=")) {
		t.Fatal("private pairing secret leaked to stdout")
	}
	if pairing.TTLSeconds != 90 || pairing.ExpiresAt.Before(before.Add(90*time.Second)) || pairing.ExpiresAt.After(time.Now().Add(90*time.Second)) {
		t.Fatalf("incorrect pairing TTL: %+v", pairing)
	}
	if _, err := invoke("pair", "alice", "--output", path); err == nil {
		t.Fatal("pairing overwrote an existing secret file")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, unchanged) {
		t.Fatal("exclusive pairing output changed existing file")
	}
	// Opaque verifier data exercises persistence/CLI boundaries, not WebAuthn crypto.
	now := time.Now().UTC()
	register := func(pairingID, credentialID string) model.OperatorCredential {
		t.Helper()
		credential := model.OperatorCredential{ID: credentialID, OperatorID: operator.ID, UserHandle: operator.UserHandle, PublicKey: []byte("fixture-public-key"), CredentialData: json.RawMessage(`{"opaque":"fixture"}`), Fingerprint: strings.Repeat("a", 64), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		challenge := model.AuthChallenge{ID: "registration-" + credentialID, Purpose: "registration", OperatorID: operator.ID, PairingID: pairingID, SessionData: json.RawMessage(`{"opaque":"registration"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
		if err := db.CreateAuthChallenge(ctx, challenge); err != nil {
			t.Fatal(err)
		}
		storedPairing, err := db.GetPairing(ctx, pairingID)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RegisterPendingCredential(ctx, pairingID, storedPairing.SecretHash, challenge.ID, credential); err != nil {
			t.Fatal(err)
		}
		status, err := invoke("pairing", pairingID)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(status, []byte(credential.Fingerprint)) || bytes.Contains(status, []byte("fingerprint")) {
			t.Fatalf("pairing status leaked confirmation value: %s", status)
		}
		return credential
	}
	newPairing := func() string {
		t.Helper()
		data, err := invoke("pair", "alice")
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			ID string `json:"pairing_id"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	credential := register(pairing.PairingID, "alice-wrong-device")
	if _, err := invoke("confirm", pairing.PairingID, "--fingerprint", strings.Repeat("f", 64)); err == nil {
		t.Fatal("mismatched credential was confirmed")
	}
	if _, err := db.GetCredential(ctx, credential.ID); err == nil {
		t.Fatal("wrong fingerprint activated credential")
	}
	rejected, err := db.GetPairing(ctx, pairing.PairingID)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != "revoked" {
		t.Fatalf("fingerprint mismatch did not revoke pairing: %s", rejected.State)
	}
	if _, err := invoke("confirm", pairing.PairingID, "--fingerprint", credential.Fingerprint); err == nil {
		t.Fatal("revoked pairing could be confirmed")
	}
	pairingID := newPairing()
	credential = register(pairingID, "alice-device")
	if _, err := invoke("confirm", pairingID, "--fingerprint", credential.Fingerprint); err != nil {
		t.Fatal(err)
	}
	generation, err := db.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := model.BrowserSession{ID: "cli-session", OperatorID: operator.ID, CredentialID: credential.ID, TokenHash: strings.Repeat("b", 64), CSRFHash: strings.Repeat("c", 64), AuthGeneration: generation, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(2 * time.Hour)}
	if err := db.CreateBrowserSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.BrowserSessionByTokenHash(ctx, session.TokenHash); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("revoke-device", credential.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.BrowserSessionByTokenHash(ctx, session.TokenHash); err == nil {
		t.Fatal("revoked device session authenticated")
	}
	pairingID = newPairing()
	credential = register(pairingID, "alice-restore-device")
	if _, err := invoke("confirm", pairingID, "--fingerprint", credential.Fingerprint); err != nil {
		t.Fatal(err)
	}
	session.ID, session.CredentialID = "cli-session-before-restore", credential.ID
	if err := db.CreateBrowserSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("restore-auth", "--confirm", "wrong"); err == nil {
		t.Fatal("restore accepted missing exact confirmation")
	}
	if _, err := invoke("restore-auth", "--confirm", "invalidate-all-browser-auth"); err != nil {
		t.Fatal(err)
	}
	updatedGeneration, err := db.AuthGeneration(ctx)
	if err != nil || generation == updatedGeneration {
		t.Fatalf("generation did not rotate: %v", err)
	}
	if _, _, _, err := db.BrowserSessionByTokenHash(ctx, session.TokenHash); err == nil {
		t.Fatal("old session survived auth restore")
	}
	oldCredential, err := db.GetCredential(ctx, credential.ID)
	if err != nil || oldCredential.RevokedAt == nil {
		t.Fatalf("restore did not revoke old credential: %v", err)
	}
	session.ID, session.AuthGeneration = "cli-session-after-restore", updatedGeneration
	if err := db.CreateBrowserSession(ctx, session); err == nil {
		t.Fatal("old credential created a fresh session after restore")
	}
	pairingID = newPairing()
	credential = register(pairingID, "alice-fresh-device")
	if _, err := invoke("confirm", pairingID, "--fingerprint", credential.Fingerprint); err != nil {
		t.Fatal(err)
	}
	session.CredentialID = credential.ID
	if err := db.CreateBrowserSession(ctx, session); err != nil {
		t.Fatalf("fresh CLI pairing could not restore access: %v", err)
	}
	if _, err := invoke("revoke", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("pair", "alice"); err == nil {
		t.Fatal("revoked operator received a new pairing")
	}
}

func TestIntakeCLIStatusTraversesDurableHistory(t *testing.T) {
	ctx := context.Background()
	c := intakeRuntimeConfig(t)
	c.Intake.HistoryDefault, c.Intake.HistoryMax = 2, 3
	c.Projects = map[string]model.Project{"demo": {Trusted: true}}
	db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	operator := model.Operator{ID: "history-operator", Label: "History", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("history-handle")}
	if err := db.CreateOperator(ctx, operator); err != nil {
		t.Fatal(err)
	}
	pairing := model.OperatorPairing{ID: "history-pairing", OperatorID: operator.ID, ProjectScope: operator.ProjectScope, MaxCredentials: c.Intake.MaxCredentials, SecretHash: strings.Repeat("d", 64), ExpiresAt: now.Add(time.Minute)}
	if err := db.CreatePairing(ctx, pairing); err != nil {
		t.Fatal(err)
	}
	challenge := model.AuthChallenge{ID: "history-registration", Purpose: "registration", OperatorID: operator.ID, PairingID: pairing.ID, SessionData: json.RawMessage(`{"opaque":"fixture"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := db.CreateAuthChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	// Inert verifier output tests real durable commands, not ceremony cryptography.
	credential := model.OperatorCredential{ID: "history-device", OperatorID: operator.ID, UserHandle: operator.UserHandle, PublicKey: []byte("fixture-key"), CredentialData: json.RawMessage(`{"opaque":"fixture"}`), Fingerprint: strings.Repeat("a", 64), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := db.RegisterPendingCredential(ctx, pairing.ID, pairing.SecretHash, challenge.ID, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConfirmPairing(ctx, pairing.ID, credential.Fingerprint); err != nil {
		t.Fatal(err)
	}
	generation, err := db.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := model.BrowserSession{ID: "history-session", OperatorID: operator.ID, CredentialID: credential.ID, TokenHash: strings.Repeat("b", 64), CSRFHash: strings.Repeat("c", 64), AuthGeneration: generation, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(2 * time.Hour)}
	if err := db.CreateBrowserSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
		"44444444-4444-4444-8444-444444444444",
		"55555555-5555-4555-8555-555555555555",
	} {
		payload, err := json.Marshal(model.RequestCreatePayload{Project: "demo", Text: "Saved history " + id, ClientMessageID: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecuteCommand(ctx, session.TokenHash, session.CSRFHash, model.TypedCommand{CommandID: id, Action: model.ActionRequestCreate, Payload: payload}, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for pageNumber := range 3 {
		args := []string{"status"}
		if cursor != "" {
			args = append(args, "--after", cursor)
		}
		var out bytes.Buffer
		if err := runIntake(ctx, c, db, args, &out); err != nil {
			t.Fatal(err)
		}
		var page struct {
			Items      []model.IntakeRequest `json:"items"`
			NextCursor string                `json:"next_cursor"`
			HasMore    bool                  `json:"has_more"`
			Limit      int                   `json:"limit"`
		}
		if err := json.Unmarshal(out.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		wantCount := 2
		if pageNumber == 2 {
			wantCount = 1
		}
		if page.Limit != 2 || len(page.Items) != wantCount {
			t.Fatalf("incorrect page size: %s", out.Bytes())
		}
		for _, request := range page.Items {
			if seen[request.ID] {
				t.Fatalf("duplicate request %s", request.ID)
			}
			seen[request.ID] = true
			if request.State != model.IntakeCollecting {
				t.Fatalf("saving history granted draft work: %s", request.State)
			}
		}
		if pageNumber < 2 {
			if !page.HasMore || page.NextCursor != page.Items[len(page.Items)-1].ID {
				t.Fatalf("missing continuation: %s", out.Bytes())
			}
		} else if page.HasMore || page.NextCursor != "" {
			t.Fatalf("final page advertises continuation: %s", out.Bytes())
		}
		cursor = page.NextCursor
	}
	var explicitPage bytes.Buffer
	if err := runIntake(ctx, c, db, []string{"status", "--limit", "3"}, &explicitPage); err != nil {
		t.Fatal(err)
	}
	var maximum struct {
		Items []model.IntakeRequest `json:"items"`
		Limit int                   `json:"limit"`
	}
	if err := json.Unmarshal(explicitPage.Bytes(), &maximum); err != nil {
		t.Fatal(err)
	}
	if maximum.Limit != 3 || len(maximum.Items) != 3 {
		t.Fatalf("explicit permitted limit ignored: %s", explicitPage.Bytes())
	}
	if len(seen) != 5 {
		t.Fatalf("history omitted requests: %d", len(seen))
	}
	for _, args := range [][]string{{"status", "--limit", "0"}, {"status", "--limit", "4"}, {"status", "request-id", "--limit", "2"}, {"status", "request-id", "--after", "cursor"}} {
		var out bytes.Buffer
		if err := runIntake(ctx, c, db, args, &out); err == nil {
			t.Fatalf("invalid pagination accepted: %v", args)
		}
	}
}
