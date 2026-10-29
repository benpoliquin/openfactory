package commandapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"openfactory/internal/artifacts"
	"openfactory/internal/intake"
	"openfactory/internal/model"
	"openfactory/internal/operatorauth"
	"openfactory/internal/store"
)

// The fixture provisions enrollment through trusted Store APIs, not an HTTP
// actor override. Assertions below are signed by a test-only P-256 authenticator
// and verified by the production maintained WebAuthn library, never a mock.
type httpActor struct {
	operator   model.Operator
	credential model.OperatorCredential
	key        *ecdsa.PrivateKey
	token      string
}

type httpFixture struct {
	t       *testing.T
	cfg     model.Config
	db      *store.Store
	cas     *artifacts.Store
	handler http.Handler
	path    string
	serial  int
	owner   httpActor
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	root := t.TempDir()
	f := &httpFixture{t: t, path: filepath.Join(root, "state.sqlite")}
	f.cfg = model.Config{
		PolicyHash: intake.SHA256([]byte("http-security-policy")), AstraModel: "test-provider/test-model",
		MaxAttempts: 3, MaxConcurrent: 1, MaxCallsPerDay: 10, AttemptTimeoutSeconds: 3600,
		Intake: model.IntakePolicy{
			Enabled: true, ExternalOrigin: "https://openfactory.example.test", RPID: "openfactory.example.test", RPName: "OpenFactory security test", PublicPath: "/openfactory-intake",
			MaxTurnBytes: 16 << 10, MaxTurns: 256, MaxTextBytes: 2 << 20, MaxCanonicalBytes: 256 << 10, MaxOutputBytes: 256 << 10,
			MaxDraftCalls: 3, HistoryDefault: 50, HistoryMax: 200, PairingTTLSeconds: 600, ChallengeTTLSeconds: 120,
			SessionIdleSeconds: 3600, SessionAbsoluteSeconds: 86400, CredentialTTLSeconds: 86400, MaxCredentials: 5, AuthRateLimit: 100,
		},
		Discovery: model.DiscoveryPolicy{BundleMaxFiles: 4, BundleMaxFileBytes: 64, BundleMaxTotalBytes: 96},
		Projects: map[string]model.Project{
			"demo":  {Trusted: true, MaxConcurrent: 1, Repository: "https://repository-secret@example.test/private.git", Auth: model.GitAuth{Mode: "token", TokenFile: "/private/git-token"}, Checks: map[string]model.Command{"unit": {Argv: []string{"secret-check-executable", "--secret-argument"}}}},
			"other": {Trusted: true, MaxConcurrent: 1, Checks: map[string]model.Command{"unit": {}}},
		},
	}
	var err error
	f.db, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.db.Close(); err != nil {
			t.Error(err)
		}
	})
	f.cas = artifacts.New(filepath.Join(root, "artifacts"))
	f.handler, err = New(f.cfg, f.db)
	if err != nil {
		t.Fatal(err)
	}
	f.owner = f.actor("owner", []string{"demo"})
	return f
}

func (f *httpFixture) id() string {
	f.serial++
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", f.serial)
}

func httpJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func httpDecode[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode HTTP %d: %v: %s", response.Code, err, response.Body.String())
	}
	return value
}

func httpStatus(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	if code != "" {
		body := httpDecode[model.IntakeErrorResponse](t, response)
		if body.Error.Code != code {
			t.Fatalf("error code = %q, want %q: %s", body.Error.Code, code, response.Body.String())
		}
	}
}

func (f *httpFixture) actor(label string, scope []string) httpActor {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Second)
	op := model.Operator{ID: "operator-" + label, Label: label, Enabled: true, UserHandle: []byte("user-handle-" + label), ProjectScope: scope, CreatedAt: now}
	if err := f.db.CreateOperator(ctx, op); err != nil {
		f.t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("test-only-p256-" + label))
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(seed[:])
	d.Mod(d, new(big.Int).Sub(curve.Params().N, big.NewInt(1)))
	d.Add(d, big.NewInt(1))
	x, y := curve.ScalarBaseMult(d.Bytes())
	key := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x.FillBytes(make([]byte, 32)), -3: y.FillBytes(make([]byte, 32))})
	if err != nil {
		f.t.Fatal(err)
	}
	id := sha256.Sum256([]byte("credential-" + label))
	libraryCredential := webauthn.Credential{ID: id[:], PublicKey: publicKey, Flags: webauthn.NewCredentialFlags(protocol.FlagUserPresent | protocol.FlagUserVerified)}
	credential := model.OperatorCredential{ID: base64.RawURLEncoding.EncodeToString(id[:]), OperatorID: op.ID, CredentialData: httpJSON(f.t, libraryCredential), PublicKey: publicKey, UserHandle: op.UserHandle, Fingerprint: intake.SHA256(publicKey), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	secretHash := intake.SHA256([]byte("trusted-pairing-" + label))
	pair := model.OperatorPairing{ID: "pairing-" + label, SecretHash: secretHash, OperatorID: op.ID, ProjectScope: scope, MaxCredentials: 5, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err = f.db.CreatePairing(ctx, pair); err != nil {
		f.t.Fatal(err)
	}
	challenge := model.AuthChallenge{ID: "registration-" + label, Purpose: "registration", OperatorID: op.ID, PairingID: pair.ID, SessionData: json.RawMessage(`{"fixture":"trusted registration provisioning"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err = f.db.CreateAuthChallenge(ctx, challenge); err != nil {
		f.t.Fatal(err)
	}
	if err = f.db.RegisterPendingCredential(ctx, pair.ID, secretHash, challenge.ID, credential); err != nil {
		f.t.Fatal(err)
	}
	credential, err = f.db.ConfirmPairing(ctx, pair.ID, credential.Fingerprint)
	if err != nil {
		f.t.Fatal(err)
	}
	tokenBytes := sha256.Sum256([]byte("trusted-session-" + label))
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	session := model.BrowserSession{ID: "session-" + label, TokenHash: intake.SHA256([]byte(token)), OperatorID: op.ID, CredentialID: credential.ID, CSRFHash: intake.SHA256([]byte(operatorauth.CSRFToken(token))), CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}
	if err = f.db.CreateBrowserSession(ctx, session); err != nil {
		f.t.Fatal(err)
	}
	return httpActor{operator: op, credential: credential, key: key, token: token}
}

func (f *httpFixture) request(actor *httpActor, method, path string, body []byte, change func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Origin", f.cfg.Intake.ExternalOrigin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	if method != http.MethodGet {
		r.Header.Set("Content-Type", "application/json")
	}
	if actor != nil {
		r.AddCookie(&http.Cookie{Name: "openfactory_intake_session", Value: actor.token})
		r.Header.Set("X-OpenFactory-CSRF", operatorauth.CSRFToken(actor.token))
	}
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *httpFixture) command(action string, request *model.IntakeRequest, payload any) model.TypedCommand {
	c := model.TypedCommand{CommandID: f.id(), Action: action, Payload: httpJSON(f.t, payload)}
	if request != nil {
		version := request.ContentVersion
		c.RequestID = request.ID
		c.ExpectedContentVersion = &version
	}
	return c
}

func (f *httpFixture) execute(actor *httpActor, command model.TypedCommand) *httptest.ResponseRecorder {
	return f.request(actor, http.MethodPost, "/v1/commands", httpJSON(f.t, command), nil)
}

func (f *httpFixture) save(actor *httpActor, text string) model.IntakeRequest {
	f.t.Helper()
	command := f.command(model.ActionRequestCreate, nil, model.RequestCreatePayload{Project: "demo", Text: text, ClientMessageID: f.id()})
	w := f.execute(actor, command)
	httpStatus(f.t, w, 200, "")
	receipt := httpDecode[model.CommandReceipt](f.t, w)
	var request model.IntakeRequest
	if err := json.Unmarshal(receipt.Result, &request); err != nil {
		f.t.Fatal(err)
	}
	if request.ID == "" || request.State != model.IntakeCollecting || request.OperatorID != actor.operator.ID || request.Project != "demo" {
		f.t.Fatalf("save did not persist an operator-owned, undispatched request: %+v", request)
	}
	return request
}

func (f *httpFixture) saved(id string) model.IntakeRequest {
	f.t.Helper()
	request, err := f.db.IntakeRequest(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return request
}

func (f *httpFixture) preview(actor *httpActor, request model.IntakeRequest, bundles []string) model.IntakeDisclosure {
	f.t.Helper()
	w := f.request(actor, http.MethodPost, "/v1/disclosures", httpJSON(f.t, map[string]any{"project": request.Project, "request_id": request.ID, "bundle_manifest_shas": bundles}), nil)
	httpStatus(f.t, w, 200, "")
	return httpDecode[model.IntakeDisclosure](f.t, w)
}

func (f *httpFixture) assertNoRequests() {
	f.t.Helper()
	requests, err := f.db.ListIntakeRequests(context.Background(), "", "", "", 200)
	if err != nil || len(requests) != 0 {
		f.t.Fatalf("denied HTTP request saved work: %+v, %v", requests, err)
	}
}

func TestServerWriteSecurityDoesNotMutate(t *testing.T) {
	f := newHTTPFixture(t)
	command := f.command(model.ActionRequestCreate, nil, model.RequestCreatePayload{Project: "demo", Text: "Private bounded request", ClientMessageID: f.id()})
	body := httpJSON(t, command)
	cases := []struct {
		name   string
		auth   bool
		status int
		code   string
		change func(*http.Request)
	}{
		{name: "no session", status: 401, code: "auth_required"},
		{name: "forged actor and service key", status: 401, code: "auth_required", change: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer bridge-service-key")
			r.Header.Set("X-Service-Key", "bridge-service-key")
			r.Header.Set("X-OpenFactory-Operator", f.owner.operator.ID)
			r.Header.Set("Tailscale-User-Login", "owner@example.test")
			r.Header.Set("X-Forwarded-User", f.owner.operator.ID)
		}},
		{name: "forged opaque cookie", status: 401, code: "auth_required", change: func(r *http.Request) {
			sum := sha256.Sum256([]byte("network-forged-token"))
			r.AddCookie(&http.Cookie{Name: "openfactory_intake_session", Value: base64.RawURLEncoding.EncodeToString(sum[:])})
		}},
		{name: "missing CSRF", auth: true, status: 403, code: "csrf_failed", change: func(r *http.Request) { r.Header.Del("X-OpenFactory-CSRF") }},
		{name: "wrong CSRF", auth: true, status: 403, code: "csrf_failed", change: func(r *http.Request) { r.Header.Set("X-OpenFactory-CSRF", "wrong") }},
		{name: "missing origin", auth: true, status: 403, code: "origin_denied", change: func(r *http.Request) { r.Header.Del("Origin") }},
		{name: "null origin", auth: true, status: 403, code: "origin_denied", change: func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{name: "cross origin", auth: true, status: 403, code: "origin_denied", change: func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example.test") }},
		{name: "cross site metadata", auth: true, status: 403, code: "origin_denied", change: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{name: "same site is not same origin", auth: true, status: 403, code: "origin_denied", change: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }},
		{name: "navigation metadata", auth: true, status: 403, code: "origin_denied", change: func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "navigate") }},
		{name: "non JSON write", auth: true, status: 400, code: "invalid_request", change: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var actor *httpActor
			if tc.auth {
				actor = &f.owner
			}
			w := f.request(actor, http.MethodPost, "/v1/commands", body, tc.change)
			httpStatus(t, w, tc.status, tc.code)
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("private command response enabled CORS")
			}
			f.assertNoRequests()
		})
	}
	// Even a valid session cannot smuggle a caller-selected audit identity or
	// authority through fields rejected by the typed command boundary.
	for _, field := range []string{`"actor":"owner"`, `"approved":true`, `"service_key":"bridge-key"`} {
		forged := append([]byte(nil), body[:len(body)-1]...)
		forged = append(forged, []byte(","+field+"}")...)
		httpStatus(t, f.request(&f.owner, http.MethodPost, "/v1/commands", forged, nil), 400, "invalid_request")
		f.assertNoRequests()
	}
	valid := f.execute(&f.owner, command)
	httpStatus(t, valid, 200, "")
	receipt := httpDecode[model.CommandReceipt](t, valid)
	if receipt.Audit.OperatorID != f.owner.operator.ID || receipt.Audit.CredentialID != f.owner.credential.ID || receipt.Audit.Kind != model.AuditVerifiedOperator {
		t.Fatalf("audit was not server attributed: %+v", receipt.Audit)
	}
}

func TestServerProjectMetadataIsScopedAndSanitized(t *testing.T) {
	f := newHTTPFixture(t)
	httpStatus(t, f.request(nil, http.MethodGet, "/v1/projects", nil, nil), 401, "auth_required")
	w := f.request(&f.owner, http.MethodGet, "/v1/projects", nil, nil)
	httpStatus(t, w, 200, "")
	var view struct {
		Items []model.IntakeProjectView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].ID != "demo" || !reflect.DeepEqual(view.Items[0].Checks, []string{"unit"}) || !reflect.DeepEqual(view.Items[0].ModelProviderIDs, []string{f.cfg.AstraModel}) {
		t.Fatalf("project capabilities missing or unscoped: %+v", view)
	}
	for _, private := range []string{"repository-secret", "private.git", "/private/git-token", "secret-check-executable", "secret-argument", `"auth"`, `"argv"`, `"repository"`, `"token_file"`} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("project metadata leaked %q: %s", private, w.Body.String())
		}
	}
}

func TestServerSavedTextAndPermanentReceiptReplay(t *testing.T) {
	f := newHTTPFixture(t)
	request := f.save(&f.owner, "Save this exact private request without invoking a model.")
	appendCommand := f.command(model.ActionTurnAppend, &request, model.TurnAppendPayload{ClientMessageID: f.id(), Text: "Acknowledge only after durable persistence."})
	first := f.execute(&f.owner, appendCommand)
	httpStatus(t, first, 200, "")
	firstReceipt := httpDecode[model.CommandReceipt](t, first)
	updated := f.saved(request.ID)
	second := f.command(model.ActionTurnAppend, &updated, model.TurnAppendPayload{ClientMessageID: f.id(), Text: "A later saved edit makes the original expected version stale."})
	httpStatus(t, f.execute(&f.owner, second), 200, "")
	// Close/reopen the actual database: a receipt is not process-local dedup.
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = New(f.cfg, f.db)
	if err != nil {
		t.Fatal(err)
	}
	replay := f.execute(&f.owner, appendCommand)
	httpStatus(t, replay, 200, "")
	if !reflect.DeepEqual(httpDecode[model.CommandReceipt](t, replay), firstReceipt) {
		t.Fatal("stale identical retry did not recover the original durable receipt")
	}
	lookup := f.request(&f.owner, http.MethodGet, "/v1/commands/"+appendCommand.CommandID, nil, nil)
	httpStatus(t, lookup, 200, "")
	if !reflect.DeepEqual(httpDecode[model.CommandReceipt](t, lookup), firstReceipt) {
		t.Fatal("command reconciliation returned a different receipt")
	}
	changed := appendCommand
	changed.Payload = httpJSON(t, model.TurnAppendPayload{ClientMessageID: f.id(), Text: "Different body cannot reuse committed command identity."})
	httpStatus(t, f.execute(&f.owner, changed), 409, "idempotency_conflict")
	freshID := appendCommand
	freshID.CommandID = f.id()
	httpStatus(t, f.execute(&f.owner, freshID), 409, "stale_revision")
	stored := f.saved(request.ID)
	turns, err := f.db.IntakeTurns(context.Background(), request.ID, 0, 50)
	if err != nil || len(turns) != 3 || turns[1].Text != "Acknowledge only after durable persistence." || stored.ContentVersion != request.ContentVersion+2 {
		t.Fatalf("retry mutated durable content: request=%+v turns=%+v err=%v", stored, turns, err)
	}
	if stored.State != model.IntakeCollecting || stored.ActiveStepID != "" || stored.LifetimeDispatches != 0 || stored.ContextManifest.Disclosure.PrivateMaterialToModels {
		t.Fatalf("saving or reading implicitly authorized model access: %+v", stored)
	}
	steps, err := f.db.IntakeSteps(context.Background(), request.ID)
	if err != nil || len(steps) != 0 {
		t.Fatalf("saving produced model work: %+v %v", steps, err)
	}
}

func TestServerDraftRequiresExactCurrentDisclosure(t *testing.T) {
	f := newHTTPFixture(t)
	request := f.save(&f.owner, "Draft a bounded durable acknowledgement feature.")
	disclosure := f.preview(&f.owner, request, nil)
	cases := []struct {
		name   string
		change func(*model.IntakeDisclosure)
	}{
		{"missing private consent", func(d *model.IntakeDisclosure) { d.PrivateMaterialToModels = false }},
		{"different context", func(d *model.IntakeDisclosure) { d.ContextSHA = intake.SHA256([]byte("other context")) }},
		{"different policy", func(d *model.IntakeDisclosure) { d.PolicySHA = intake.SHA256([]byte("other policy")) }},
		{"different provider", func(d *model.IntakeDisclosure) { d.ModelProviderIDs = []string{"unselected-provider/model"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := disclosure
			tc.change(&bad)
			command := f.command(model.ActionIntakeDraft, &request, model.IntakeDraftPayload{Disclosure: bad})
			httpStatus(t, f.execute(&f.owner, command), 403, "disclosure_required")
			if got := f.saved(request.ID); got.State != model.IntakeCollecting || got.ContentVersion != request.ContentVersion || got.ContextManifest.Disclosure.PrivateMaterialToModels {
				t.Fatalf("denied consent mutated request: %+v", got)
			}
		})
	}
	appendCommand := f.command(model.ActionTurnAppend, &request, model.TurnAppendPayload{ClientMessageID: f.id(), Text: "Now also preserve cancellation semantics."})
	httpStatus(t, f.execute(&f.owner, appendCommand), 200, "")
	updated := f.saved(request.ID)
	stale := f.command(model.ActionIntakeDraft, &updated, model.IntakeDraftPayload{Disclosure: disclosure})
	httpStatus(t, f.execute(&f.owner, stale), 403, "disclosure_required")
	current := f.preview(&f.owner, updated, nil)
	offer := f.request(&f.owner, http.MethodGet, "/v1/requests/"+request.ID+"/disclosure", nil, nil)
	httpStatus(t, offer, 200, "")
	var versioned struct {
		ContentVersion int                    `json:"content_version"`
		Disclosure     model.IntakeDisclosure `json:"disclosure"`
	}
	if err := json.Unmarshal(offer.Body.Bytes(), &versioned); err != nil {
		t.Fatal(err)
	}
	if versioned.ContentVersion != updated.ContentVersion || !reflect.DeepEqual(versioned.Disclosure, current) {
		t.Fatalf("review offer did not bind the current saved content version: %+v", versioned)
	}
	command := f.command(model.ActionIntakeDraft, &updated, model.IntakeDraftPayload{Disclosure: current})
	httpStatus(t, f.execute(&f.owner, command), 200, "")
	stored := f.saved(request.ID)
	if stored.State != model.IntakeDraftQueued || stored.ContextManifest.SHA == "" || !reflect.DeepEqual(stored.ContextManifest.Disclosure, current) || stored.LifetimeDispatches != 0 {
		t.Fatalf("explicit exact consent was not durably queued: %+v", stored)
	}
	// Neither a private upload nor a previous context silently enters this grant.
	for _, ref := range stored.ContextManifest.Inputs {
		if ref.Kind == "bundle" {
			t.Fatalf("undisclosed private bundle entered model context: %+v", ref)
		}
	}
}

func TestServerRequestOwnershipAndProjectScope(t *testing.T) {
	f := newHTTPFixture(t)
	request := f.save(&f.owner, "Only the creating operator may inspect or edit this transcript.")
	request = f.saved(request.ID)
	peer := f.actor("same-project-peer", []string{"demo"})
	outsider := f.actor("other-project", []string{"other"})
	for _, actor := range []*httpActor{&peer, &outsider} {
		for _, path := range []string{"/v1/requests/" + request.ID, "/v1/requests/" + request.ID + "/turns", "/v1/requests/" + request.ID + "/events", "/v1/requests/" + request.ID + "/drafts/1"} {
			httpStatus(t, f.request(actor, http.MethodGet, path, nil, nil), 404, "not_found")
		}
		page := f.request(actor, http.MethodGet, "/v1/requests", nil, nil)
		httpStatus(t, page, 200, "")
		if got := httpDecode[model.IntakeRequestPage](t, page); len(got.Items) != 0 {
			t.Fatalf("another operator listed private request: %+v", got)
		}
		for _, action := range []string{model.ActionTurnAppend, model.ActionIntakeCancel} {
			var payload any = model.EmptyCommandPayload{}
			if action == model.ActionTurnAppend {
				payload = model.TurnAppendPayload{ClientMessageID: f.id(), Text: "Unauthorized edit"}
			}
			httpStatus(t, f.execute(actor, f.command(action, &request, payload)), 403, "project_forbidden")
		}
		httpStatus(t, f.request(actor, http.MethodPost, "/v1/disclosures", httpJSON(t, map[string]any{"project": "demo", "request_id": request.ID}), nil), 404, "not_found")
		httpStatus(t, f.request(actor, http.MethodGet, "/v1/requests/"+request.ID+"/disclosure", nil, nil), 404, "not_found")
	}
	create := f.command(model.ActionRequestCreate, nil, model.RequestCreatePayload{Project: "demo", Text: "Wrong project", ClientMessageID: f.id()})
	httpStatus(t, f.execute(&outsider, create), 403, "project_forbidden")
	httpStatus(t, f.request(&outsider, http.MethodGet, "/v1/requests?project=demo", nil, nil), 403, "project_forbidden")
	if got := f.saved(request.ID); !reflect.DeepEqual(got, request) {
		t.Fatalf("unauthorized access changed request: %+v", got)
	}
}

func TestServerRevocationRejectsCookieAndReceiptReplay(t *testing.T) {
	for _, kind := range []string{"credential", "operator"} {
		t.Run(kind, func(t *testing.T) {
			f := newHTTPFixture(t)
			command := f.command(model.ActionRequestCreate, nil, model.RequestCreatePayload{Project: "demo", Text: "A committed receipt is not future access authority.", ClientMessageID: f.id()})
			first := f.execute(&f.owner, command)
			httpStatus(t, first, 200, "")
			var err error
			if kind == "credential" {
				err = f.db.RevokeCredential(context.Background(), f.owner.credential.ID)
			} else {
				err = f.db.RevokeOperator(context.Background(), f.owner.operator.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/v1/session", "/v1/projects", "/v1/commands/" + command.CommandID} {
				httpStatus(t, f.request(&f.owner, http.MethodGet, path, nil, nil), 401, "auth_required")
			}
			httpStatus(t, f.execute(&f.owner, command), 401, "auth_required")
			stored, err := f.db.ListIntakeRequests(context.Background(), f.owner.operator.ID, "demo", "", 50)
			if err != nil || len(stored) != 1 {
				t.Fatalf("revoked replay changed durable requests: %+v %v", stored, err)
			}
		})
	}
}

func (f *httpFixture) upload(actor *httpActor, data []byte) model.BundleUpload {
	f.t.Helper()
	manifest := model.BundleUploadManifest{Project: "demo", Files: []model.BundleUploadFile{{ID: "notes", Path: "notes/private.md", Bytes: int64(len(data)), ContentSHA: intake.SHA256(data)}}}
	w := f.request(actor, http.MethodPost, "/v1/bundles", httpJSON(f.t, manifest), nil)
	httpStatus(f.t, w, 201, "")
	return httpDecode[model.BundleUpload](f.t, w)
}

func (f *httpFixture) put(actor *httpActor, upload model.BundleUpload, data []byte, change func(*http.Request)) *httptest.ResponseRecorder {
	return f.request(actor, http.MethodPut, "/v1/bundles/"+upload.ID+"/files/notes", data, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/octet-stream")
		if change != nil {
			change(r)
		}
	})
}

func TestServerBundleManifestAndStreamingIntegrity(t *testing.T) {
	f := newHTTPFixture(t)
	base := model.BundleUploadFile{ID: "notes", Path: "notes.md", Bytes: 4, ContentSHA: intake.SHA256([]byte("safe"))}
	cases := []struct {
		name   string
		files  []model.BundleUploadFile
		status int
		code   string
	}{
		{"traversal", []model.BundleUploadFile{{ID: "notes", Path: "../notes.md", Bytes: 4, ContentSHA: base.ContentSHA}}, 400, "invalid_contract"},
		{"absolute path", []model.BundleUploadFile{{ID: "notes", Path: "/notes.md", Bytes: 4, ContentSHA: base.ContentSHA}}, 400, "invalid_contract"},
		{"unsupported extraction", []model.BundleUploadFile{{ID: "notes", Path: "notes.pdf", Bytes: 4, ContentSHA: base.ContentSHA}}, 400, "invalid_contract"},
		{"duplicate file ID", []model.BundleUploadFile{base, base}, 400, "invalid_contract"},
		{"duplicate logical path", []model.BundleUploadFile{base, {ID: "second", Path: base.Path, Bytes: 4, ContentSHA: base.ContentSHA}}, 400, "invalid_contract"},
		{"invalid hash", []model.BundleUploadFile{{ID: "notes", Path: "notes.md", Bytes: 4, ContentSHA: "not-a-sha256"}}, 400, "invalid_contract"},
		{"oversized file", []model.BundleUploadFile{{ID: "notes", Path: "notes.md", Bytes: 65, ContentSHA: base.ContentSHA}}, 413, "bundle_limit"},
		{"oversized total", []model.BundleUploadFile{{ID: "notes", Path: "one.md", Bytes: 64, ContentSHA: base.ContentSHA}, {ID: "second", Path: "two.md", Bytes: 64, ContentSHA: base.ContentSHA}}, 413, "bundle_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := model.BundleUploadManifest{Project: "demo", Files: tc.files}
			httpStatus(t, f.request(&f.owner, http.MethodPost, "/v1/bundles", httpJSON(t, manifest), nil), tc.status, tc.code)
		})
	}
	data := []byte("private bounded context\n")
	upload := f.upload(&f.owner, data)
	httpStatus(t, f.request(&f.owner, http.MethodPost, "/v1/bundles/"+upload.ID+"/finalize", []byte(`{}`), nil), 400, "invalid_contract")
	httpStatus(t, f.put(&f.owner, upload, bytes.Repeat([]byte("x"), len(data)), nil), 409, "evidence_invalid")
	httpStatus(t, f.put(&f.owner, upload, append(append([]byte(nil), data...), 'x'), nil), 400, "invalid_contract")
	// Unknown-length streaming must enforce the declared bound too, rather than
	// trusting Content-Length or silently accepting a matching prefix.
	httpStatus(t, f.put(&f.owner, upload, append(append([]byte(nil), data...), 'x'), func(r *http.Request) { r.ContentLength = -1 }), 409, "evidence_invalid")
	httpStatus(t, f.put(&f.owner, upload, data[:len(data)-1], func(r *http.Request) { r.ContentLength = -1 }), 409, "evidence_invalid")
	httpStatus(t, f.request(&f.owner, http.MethodPut, "/v1/bundles/"+upload.ID+"/files/undeclared", data, func(r *http.Request) { r.Header.Set("Content-Type", "application/octet-stream") }), 404, "not_found")
	httpStatus(t, f.request(&f.owner, http.MethodPost, "/v1/bundles/"+upload.ID+"/finalize", []byte(`{}`), nil), 400, "invalid_contract")
	httpStatus(t, f.put(&f.owner, upload, data, nil), 200, "")
	final := f.request(&f.owner, http.MethodPost, "/v1/bundles/"+upload.ID+"/finalize", []byte(`{}`), nil)
	httpStatus(t, final, 200, "")
	completed := httpDecode[model.BundleUpload](t, final)
	if completed.State != "finalized" || completed.Bundle == nil {
		t.Fatalf("upload not finalized into existing bundle: %+v", completed)
	}
	bundle, err := f.cas.LoadBundle(context.Background(), completed.Bundle.ManifestSHA)
	if err != nil || len(bundle.Files) != 1 || bundle.Files[0].Path != "notes/private.md" || bundle.Files[0].ContentSHA != intake.SHA256(data) {
		t.Fatalf("finalized bundle does not verify in real CAS: %+v %v", bundle, err)
	}
	stored, err := f.cas.Read(context.Background(), bundle.Files[0].ContentSHA, int64(len(data)))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("finalized bytes changed: %q %v", stored, err)
	}
}

func TestServerPrivateBundleRequiresOwnerAndExplicitDraftSelection(t *testing.T) {
	f := newHTTPFixture(t)
	data := []byte("Private seed material\n")
	upload := f.upload(&f.owner, data)
	httpStatus(t, f.put(&f.owner, upload, data, nil), 200, "")
	final := f.request(&f.owner, http.MethodPost, "/v1/bundles/"+upload.ID+"/finalize", []byte(`{}`), nil)
	httpStatus(t, final, 200, "")
	completed := httpDecode[model.BundleUpload](t, final)
	if completed.Bundle == nil {
		t.Fatal("finalization omitted bundle")
	}
	sha := completed.Bundle.ManifestSHA
	peer := f.actor("bundle-peer", []string{"demo"})
	for _, methodPath := range []struct{ method, path string }{{http.MethodGet, "/v1/bundles/" + upload.ID}, {http.MethodPost, "/v1/bundles/" + upload.ID + "/finalize"}} {
		httpStatus(t, f.request(&peer, methodPath.method, methodPath.path, []byte(`{}`), nil), 403, "project_forbidden")
	}
	httpStatus(t, f.put(&peer, upload, data, nil), 403, "project_forbidden")
	peerRequest := f.save(&peer, "Another operator cannot disclose the owner's bundle.")
	httpStatus(t, f.request(&peer, http.MethodPost, "/v1/disclosures", httpJSON(t, map[string]any{"project": "demo", "request_id": peerRequest.ID, "bundle_manifest_shas": []string{sha}}), nil), 403, "project_forbidden")
	request := f.save(&f.owner, "Upload is not model disclosure or draft authorization.")
	if request.ContextManifest.Disclosure.PrivateMaterialToModels || len(request.ContextManifest.Disclosure.BundleManifestSHAs) != 0 {
		t.Fatal("upload implicitly granted private model access")
	}
	none := f.preview(&f.owner, request, nil)
	selected := f.preview(&f.owner, request, []string{sha})
	if len(none.BundleManifestSHAs) != 0 || !reflect.DeepEqual(selected.BundleManifestSHAs, []string{sha}) || selected.ContextSHA == none.ContextSHA {
		t.Fatal("preview did not bind the exact explicit bundle selection")
	}
	// Merely copying a selected manifest into a different preview must not grant
	// it: all fields are bound together by the current context digest.
	forged := none
	forged.BundleManifestSHAs = []string{sha}
	httpStatus(t, f.execute(&f.owner, f.command(model.ActionIntakeDraft, &request, model.IntakeDraftPayload{Disclosure: forged})), 403, "disclosure_required")
	httpStatus(t, f.execute(&f.owner, f.command(model.ActionIntakeDraft, &request, model.IntakeDraftPayload{Disclosure: none})), 200, "")
	without := f.saved(request.ID)
	for _, input := range without.ContextManifest.Inputs {
		if input.Kind == "bundle" {
			t.Fatalf("unselected bundle entered context: %+v", input)
		}
	}
	second := f.save(&f.owner, "This request explicitly permits the exact uploaded seed.")
	consent := f.preview(&f.owner, second, []string{sha})
	httpStatus(t, f.execute(&f.owner, f.command(model.ActionIntakeDraft, &second, model.IntakeDraftPayload{Disclosure: consent})), 200, "")
	with := f.saved(second.ID)
	count := 0
	for _, input := range with.ContextManifest.Inputs {
		if input.Kind == "bundle" {
			count++
			if input.ID != sha+"/notes/private.md" || input.SHA != intake.SHA256(data) || input.Bytes != int64(len(data)) {
				t.Fatalf("disclosed bundle bytes not exact: %+v", input)
			}
		}
	}
	if count != 1 || !reflect.DeepEqual(with.ContextManifest.Disclosure, consent) {
		t.Fatalf("explicit selection not durably granted exactly once: %+v", with.ContextManifest)
	}
}

func (f *httpFixture) importFeature(id, body string) model.Feature {
	f.t.Helper()
	source := fmt.Sprintf("---\nid: %s\nproject: demo\ntitle: Durable acknowledgement\nkind: feature\nrisk: normal\ncompletion_target: reviewed\nchecks: [unit]\nacceptance:\n  - id: durable\n    scenario: A client retries after losing a response\n    expected: Recover one durable receipt without duplicate work\n---\n%s\n", id, body)
	feature, err := intake.Parse([]byte(source))
	if err != nil {
		f.t.Fatal(err)
	}
	feature, err = f.db.Import(context.Background(), feature)
	if err != nil {
		f.t.Fatal(err)
	}
	return feature
}

func (f *httpFixture) authorization(actor *httpActor, command model.TypedCommand) (model.IntakeReview, ceremonyOptions) {
	f.t.Helper()
	w := f.request(actor, http.MethodPost, "/v1/reviews", httpJSON(f.t, model.ReviewRequest{Command: command}), nil)
	httpStatus(f.t, w, 200, "")
	review := httpDecode[model.IntakeReview](f.t, w)
	w = f.request(actor, http.MethodPost, "/v1/authorizations/options", httpJSON(f.t, model.AuthorizationOptionsRequest{ReviewID: review.ID}), nil)
	httpStatus(f.t, w, 200, "")
	return review, httpDecode[ceremonyOptions](f.t, w)
}

func (f *httpFixture) assertion(actor httpActor, options ceremonyOptions, flags protocol.AuthenticatorFlags, origin string, count uint32) json.RawMessage {
	f.t.Helper()
	var publicKey protocol.PublicKeyCredentialRequestOptions
	if err := json.Unmarshal(options.PublicKey, &publicKey); err != nil {
		f.t.Fatal(err)
	}
	clientData := httpJSON(f.t, map[string]any{"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(publicKey.Challenge), "origin": origin, "crossOrigin": false})
	rpHash := sha256.Sum256([]byte(f.cfg.Intake.RPID))
	authData := make([]byte, 37)
	copy(authData, rpHash[:])
	authData[32] = byte(flags)
	binary.BigEndian.PutUint32(authData[33:], count)
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, actor.key, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	rawID, err := base64.RawURLEncoding.DecodeString(actor.credential.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return httpJSON(f.t, protocol.CredentialAssertionResponse{
		PublicKeyCredential: protocol.PublicKeyCredential{Credential: protocol.Credential{ID: actor.credential.ID, Type: "public-key"}, RawID: rawID},
		AssertionResponse:   protocol.AuthenticatorAssertionResponse{AuthenticatorResponse: protocol.AuthenticatorResponse{ClientDataJSON: clientData}, AuthenticatorData: authData, Signature: signature, UserHandle: actor.operator.UserHandle},
	})
}

func (f *httpFixture) commit(actor *httpActor, command model.TypedCommand, options ceremonyOptions, assertion json.RawMessage) *httptest.ResponseRecorder {
	return f.request(actor, http.MethodPost, "/v1/authorizations/commit", httpJSON(f.t, model.AuthorizationCommitRequest{CommandID: command.CommandID, ChallengeID: options.ChallengeID, Assertion: assertion}), nil)
}

func TestServerRealWebAuthnApprovalAndConsumedChallengeReplay(t *testing.T) {
	f := newHTTPFixture(t)
	feature := f.importFeature("signed-feature", "Implement only the exact reviewed acknowledgement contract.")
	command := f.command(model.ActionFeatureApprove, nil, model.SubjectCommandPayload{Kind: "feature", ID: feature.ID, Revision: feature.Revision})
	httpStatus(t, f.execute(&f.owner, command), 403, "human_verification_required")
	review, options := f.authorization(&f.owner, command)
	if review.Feature == nil || review.Feature.Revision != feature.Revision || review.CanonicalSource == "" {
		t.Fatal("authority review did not expose the real immutable feature")
	}
	for _, tc := range []struct {
		name   string
		flags  protocol.AuthenticatorFlags
		origin string
	}{
		{"no fresh UV", protocol.FlagUserPresent, f.cfg.Intake.ExternalOrigin},
		{"no fresh UP", protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin},
		{"wrong signed origin", protocol.FlagUserPresent | protocol.FlagUserVerified, "https://attacker.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertion := f.assertion(f.owner, options, tc.flags, tc.origin, 1)
			httpStatus(t, f.commit(&f.owner, command, options, assertion), 403, "human_verification_required")
			stored, err := f.db.Get(context.Background(), feature.ID)
			if err != nil || stored.State != "proposed" || stored.PolicyHash != "" {
				t.Fatalf("invalid signed assertion granted authority: %+v %v", stored, err)
			}
			challenge, err := f.db.GetAuthChallenge(context.Background(), options.ChallengeID)
			if err != nil || challenge.ConsumedAt != nil {
				t.Fatalf("rejected assertion consumed challenge: %+v %v", challenge, err)
			}
		})
	}
	assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	first := f.commit(&f.owner, command, options, assertion)
	httpStatus(t, first, 200, "")
	receipt := httpDecode[model.CommandReceipt](t, first)
	if receipt.ApprovalID == "" || receipt.Audit.OperatorID != f.owner.operator.ID || receipt.Audit.CommandID != command.CommandID {
		t.Fatalf("signed grant lacks immutable verified provenance: %+v", receipt)
	}
	stored, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || stored.State != "ready" || stored.PolicyHash != f.cfg.PolicyHash {
		t.Fatalf("valid library-verified assertion did not approve feature: %+v %v", stored, err)
	}
	approval, err := f.db.GetApprovalReceipt(context.Background(), receipt.ApprovalID)
	if err != nil || !approval.Evidence.UserPresent || !approval.Evidence.UserVerified || approval.Evidence.ChallengeID != options.ChallengeID || approval.ClaimsSHA != review.ClaimsSHA {
		t.Fatalf("durable approval evidence not bound to exact review: %+v %v", approval, err)
	}
	challenge, err := f.db.GetAuthChallenge(context.Background(), options.ChallengeID)
	if err != nil || challenge.ConsumedAt == nil {
		t.Fatalf("successful grant failed to consume challenge: %+v %v", challenge, err)
	}
	credential, err := f.db.GetCredential(context.Background(), f.owner.credential.ID)
	if err != nil || credential.SignCount != 1 {
		t.Fatalf("verified credential state not persisted: %+v %v", credential, err)
	}
	// Historical approval is not authority over a new revision. Its identical
	// lost-response retry still recovers the original receipt after consumption.
	changed := f.importFeature(feature.ID, "This is a new unapproved immutable contract.")
	if changed.Revision == feature.Revision || changed.State != "proposed" {
		t.Fatal("fixture edit did not create a new unapproved revision")
	}
	replay := f.commit(&f.owner, command, options, assertion)
	httpStatus(t, replay, 200, "")
	if !reflect.DeepEqual(httpDecode[model.CommandReceipt](t, replay), receipt) {
		t.Fatal("consumed-challenge retry did not recover original approval receipt")
	}
	newCommand := command
	newCommand.CommandID = f.id()
	httpStatus(t, f.commit(&f.owner, newCommand, options, assertion), 404, "not_found")
	current, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || current.State != "proposed" || current.PolicyHash != "" || current.Revision != changed.Revision {
		t.Fatalf("replayed proof approved changed contract: %+v %v", current, err)
	}
}

func TestServerRealWebAuthnRejectsStaleReviewAtomically(t *testing.T) {
	f := newHTTPFixture(t)
	feature := f.importFeature("stale-signed-feature", "The first exact reviewed contract.")
	command := f.command(model.ActionFeatureApprove, nil, model.SubjectCommandPayload{Kind: "feature", ID: feature.ID, Revision: feature.Revision})
	_, options := f.authorization(&f.owner, command)
	assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	changed := f.importFeature(feature.ID, "Changed after the authenticator challenge was issued.")
	httpStatus(t, f.commit(&f.owner, command, options, assertion), 409, "stale_revision")
	current, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || current.Revision != changed.Revision || current.State != "proposed" || current.PolicyHash != "" {
		t.Fatalf("stale signed review mutated subject: %+v %v", current, err)
	}
	challenge, err := f.db.GetAuthChallenge(context.Background(), options.ChallengeID)
	if err != nil || challenge.ConsumedAt != nil {
		t.Fatalf("stale review partially consumed authority: %+v %v", challenge, err)
	}
	credential, err := f.db.GetCredential(context.Background(), f.owner.credential.ID)
	if err != nil || credential.SignCount != 0 {
		t.Fatalf("stale review partially updated credential: %+v %v", credential, err)
	}
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/commands/"+command.CommandID, nil, nil), 404, "not_found")
}

func TestServerPreauthPeerExhaustionDoesNotBlockSignedSessionAuthorization(t *testing.T) {
	f := newHTTPFixture(t)
	f.cfg.Intake.AuthRateLimit = 4
	var err error
	f.handler, err = New(f.cfg, f.db)
	if err != nil {
		t.Fatal(err)
	}
	feature := f.importFeature("rate-isolated-feature", "A noisy unauthenticated transport peer must not prevent an existing operator from approving an exact review.")
	command := f.command(model.ActionFeatureApprove, nil, model.SubjectCommandPayload{Kind: "feature", ID: feature.ID, Revision: feature.Revision})
	peer := func(r *http.Request) {
		r.RemoteAddr = "192.0.2.44:54321"
	}
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/session", nil, peer), 200, "")
	for range f.cfg.Intake.AuthRateLimit {
		httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), peer), 200, "")
	}
	// A different source port and caller-selected forwarded identities do not
	// buy another preauthentication attempt for the same transport peer.
	w := f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), func(r *http.Request) {
		peer(r)
		r.RemoteAddr = "192.0.2.44:54322"
		r.Header.Set("X-Forwarded-For", "198.51.100.99")
		r.Header.Set("X-Forwarded-User", "another-operator")
		r.Header.Set("Tailscale-User-Login", "another@example.test")
	})
	httpStatus(t, w, 429, "auth_rate_limit")
	if !httpDecode[model.IntakeErrorResponse](t, w).Error.Retryable {
		t.Fatal("exhausted peer bucket was not retryable")
	}
	w = f.request(&f.owner, http.MethodPost, "/v1/reviews", httpJSON(t, model.ReviewRequest{Command: command}), peer)
	httpStatus(t, w, 200, "")
	review := httpDecode[model.IntakeReview](t, w)
	w = f.request(&f.owner, http.MethodPost, "/v1/authorizations/options", httpJSON(t, model.AuthorizationOptionsRequest{ReviewID: review.ID}), peer)
	httpStatus(t, w, 200, "")
	options := httpDecode[ceremonyOptions](t, w)
	assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	w = f.request(&f.owner, http.MethodPost, "/v1/authorizations/commit", httpJSON(t, model.AuthorizationCommitRequest{CommandID: command.CommandID, ChallengeID: options.ChallengeID, Assertion: assertion}), peer)
	httpStatus(t, w, 200, "")
	receipt := httpDecode[model.CommandReceipt](t, w)
	approval, err := f.db.GetApprovalReceipt(context.Background(), receipt.ApprovalID)
	if err != nil || approval.ClaimsSHA != review.ClaimsSHA || approval.Evidence.ChallengeID != options.ChallengeID || !approval.Evidence.UserPresent || !approval.Evidence.UserVerified {
		t.Fatalf("peer-isolated signed commit did not persist reviewed authority: %+v %v", approval, err)
	}
	stored, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || stored.State != "ready" || stored.PolicyHash != f.cfg.PolicyHash {
		t.Fatalf("peer-isolated signed commit did not approve feature: %+v %v", stored, err)
	}
	httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), peer), 429, "auth_rate_limit")
}

func TestServerOtherLiveSessionCannotRecoverIssuedAuthorizationChallenge(t *testing.T) {
	f := newHTTPFixture(t)
	feature := f.importFeature("session-bound-options-feature", "A review challenge belongs to the live browser session that first issued it.")
	command := f.command(model.ActionFeatureApprove, nil, model.SubjectCommandPayload{Kind: "feature", ID: feature.ID, Revision: feature.Revision})
	review, options := f.authorization(&f.owner, command)
	w := f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
	httpStatus(t, w, 200, "")
	loginOptions := httpDecode[ceremonyOptions](t, w)
	assertion := f.assertion(f.owner, loginOptions, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	w = f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: loginOptions.ChallengeID, Credential: assertion}), nil)
	httpStatus(t, w, 200, "")
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].Value == f.owner.token {
		t.Fatal("fresh signed login did not create a distinct session")
	}
	second := f.owner
	second.token = cookies[0].Value
	httpStatus(t, f.request(&second, http.MethodGet, "/v1/session", nil, nil), 200, "")
	beforeChallenge, err := f.db.GetAuthChallenge(context.Background(), options.ChallengeID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCredential, err := f.db.GetCredential(context.Background(), f.owner.credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeFeature, err := f.db.Get(context.Background(), feature.ID)
	if err != nil {
		t.Fatal(err)
	}
	w = f.request(&second, http.MethodPost, "/v1/authorizations/options", httpJSON(t, model.AuthorizationOptionsRequest{ReviewID: review.ID}), nil)
	httpStatus(t, w, 409, "stale_subject")
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("session-local review conflict changed the browser cookie")
	}
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/session", nil, nil), 200, "")
	httpStatus(t, f.request(&second, http.MethodGet, "/v1/session", nil, nil), 200, "")
	stored, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || !reflect.DeepEqual(stored, beforeFeature) {
		t.Fatalf("other-session options mutated reviewed feature: %+v %v", stored, err)
	}
	afterChallenge, err := f.db.GetAuthChallenge(context.Background(), options.ChallengeID)
	if err != nil || !reflect.DeepEqual(afterChallenge, beforeChallenge) {
		t.Fatalf("other-session options replaced or consumed first-session challenge: %+v %v", afterChallenge, err)
	}
	afterCredential, err := f.db.GetCredential(context.Background(), f.owner.credential.ID)
	if err != nil || !reflect.DeepEqual(afterCredential, beforeCredential) {
		t.Fatalf("other-session options mutated credential: %+v %v", afterCredential, err)
	}
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/commands/"+command.CommandID, nil, nil), 404, "not_found")
	// The first browser retains not only a valid cookie but its usable signed
	// ceremony after another valid session encounters the review conflict.
	assertion = f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 2)
	httpStatus(t, f.commit(&f.owner, command, options, assertion), 200, "")
}

func assertHTTPStorageUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	httpStatus(t, w, 503, "storage_unavailable")
	// Require the entire public failure, not just its code: raw SQL errors,
	// tokens, CSRF material and fabricated WebAuthn failures cannot hide in
	// extra response fields or a newly minted session cookie.
	if w.Body.String() != `{"error":{"code":"storage_unavailable","message":"Storage is unavailable.","retryable":true}}` {
		t.Fatalf("storage failure was not the sanitized retryable response: %s", w.Body.String())
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("storage failure minted or changed a session cookie")
	}
}

func (f *httpFixture) dropAuthTable(table string) {
	f.t.Helper()
	// A second real connection injects a schema failure without replacing the
	// store, verifier, HTTP handler or successful ceremony fixtures.
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(context.Background(), "DROP TABLE "+table); err != nil {
		f.t.Fatal(err)
	}
}

func TestServerLoginLookupStorageFailuresRemainRetryable(t *testing.T) {
	for _, table := range []string{"intake_auth_challenges", "intake_credentials", "intake_operators"} {
		t.Run(table, func(t *testing.T) {
			f := newHTTPFixture(t)
			w := f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
			httpStatus(t, w, 200, "")
			options := httpDecode[ceremonyOptions](t, w)
			assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
			f.dropAuthTable(table)
			w = f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: options.ChallengeID, Credential: assertion}), nil)
			assertHTTPStorageUnavailable(t, w)
			f.assertNoRequests()
			if table != "intake_auth_challenges" {
				challenge, err := f.db.GetAuthChallenge(context.Background(), options.ChallengeID)
				if err != nil || challenge.ConsumedAt != nil {
					t.Fatalf("failed login consumed challenge: %+v %v", challenge, err)
				}
			}
		})
	}
}

func TestServerCommitChallengeStorageFailureIsNotMissingAuthority(t *testing.T) {
	f := newHTTPFixture(t)
	feature := f.importFeature("storage-failed-commit-feature", "A challenge lookup outage must not be fabricated as missing authority or invalid verification.")
	command := f.command(model.ActionFeatureApprove, nil, model.SubjectCommandPayload{Kind: "feature", ID: feature.ID, Revision: feature.Revision})
	_, options := f.authorization(&f.owner, command)
	assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	before, err := f.db.Get(context.Background(), feature.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCredential, err := f.db.GetCredential(context.Background(), f.owner.credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.dropAuthTable("intake_auth_challenges")
	// Authentication still succeeds, so the commit must reach its own failed
	// challenge lookup rather than merely failing at the session boundary.
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/session", nil, nil), 200, "")
	assertHTTPStorageUnavailable(t, f.commit(&f.owner, command, options, assertion))
	stored, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || !reflect.DeepEqual(stored, before) {
		t.Fatalf("failed challenge lookup mutated subject: %+v %v", stored, err)
	}
	credential, err := f.db.GetCredential(context.Background(), f.owner.credential.ID)
	if err != nil || !reflect.DeepEqual(credential, beforeCredential) {
		t.Fatalf("failed challenge lookup updated credential: %+v %v", credential, err)
	}
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/commands/"+command.CommandID, nil, nil), 404, "not_found")
}

func TestServerRegistrationOperatorStorageFailureRemainsRetryable(t *testing.T) {
	f := newHTTPFixture(t)
	_, secret := f.invite(f.owner, "operator-storage-failure")
	body := httpJSON(t, registrationOptionsRequest{PairingSecret: secret})
	httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/register/options", body, nil), 200, "")
	f.dropAuthTable("intake_operators")
	assertHTTPStorageUnavailable(t, f.request(nil, http.MethodPost, "/v1/auth/register/options", body, nil))
	f.assertNoRequests()
}

func TestServerClosedStoreAuthHelpersPreserveStorageFailures(t *testing.T) {
	f := newHTTPFixture(t)
	_, secret := f.invite(f.owner, "closed-store")
	w := f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
	httpStatus(t, w, 200, "")
	options := httpDecode[ceremonyOptions](t, w)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	s := f.handler.(*Server)
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login/finish", nil)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"challenge", func() error {
			_, err := s.challenge(r, options.ChallengeID, "login")
			return err
		}},
		{"pairing user", func() error {
			_, _, err := s.pairingUser(r, secret)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("closed store unexpectedly allowed authentication lookup")
			}
			w := httptest.NewRecorder()
			writeError(w, apiFailure(err))
			assertHTTPStorageUnavailable(t, w)
		})
	}
}

func TestServerRealWebAuthnLoginCookieAndLogout(t *testing.T) {
	f := newHTTPFixture(t)
	w := f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
	httpStatus(t, w, 200, "")
	options := httpDecode[ceremonyOptions](t, w)
	assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	w = f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: options.ChallengeID, Credential: assertion}), nil)
	httpStatus(t, w, 200, "")
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %+v", cookies)
	}
	cookie := cookies[0]
	decoded, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(decoded) != 32 || cookie.Name != "openfactory_intake_session" || cookie.Path != "/openfactory-intake/" || cookie.Domain != "" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login did not mint a confined opaque secure session: %+v %v", cookie, err)
	}
	session := httpDecode[model.IntakeSessionView](t, w)
	if session.CSRFToken != operatorauth.CSRFToken(cookie.Value) {
		t.Fatal("login CSRF did not bind raw opaque session")
	}
	installation, err := f.db.InstallationID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	generation, err := f.db.AuthGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.InstallationID == "" || session.InstallationID != installation || session.AuthGeneration == "" || session.AuthGeneration != generation {
		t.Fatalf("login did not expose canonical session recovery fences: %+v", session)
	}
	stored, _, _, err := f.db.BrowserSessionByTokenHash(context.Background(), intake.SHA256([]byte(cookie.Value)))
	if err != nil || stored.TokenHash == cookie.Value || stored.CSRFHash != intake.SHA256([]byte(session.CSRFToken)) {
		t.Fatalf("login persisted raw bearer/CSRF or wrong binding: %+v %v", stored, err)
	}
	loggedIn := f.owner
	loggedIn.token = cookie.Value
	w = f.request(&loggedIn, http.MethodGet, "/v1/session", nil, nil)
	httpStatus(t, w, 200, "")
	if got := httpDecode[model.IntakeSessionView](t, w); !reflect.DeepEqual(got, session) {
		t.Fatalf("session refresh changed login recovery identity: %+v", got)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.handler, err = New(f.cfg, f.db)
	if err != nil {
		t.Fatal(err)
	}
	w = f.request(&loggedIn, http.MethodGet, "/v1/session", nil, nil)
	httpStatus(t, w, 200, "")
	if got := httpDecode[model.IntakeSessionView](t, w); !reflect.DeepEqual(got, session) {
		t.Fatalf("normal database restart changed session recovery identity: %+v", got)
	}
	httpStatus(t, f.request(&loggedIn, http.MethodPost, "/v1/auth/logout", []byte(`{}`), nil), 200, "")
	httpStatus(t, f.request(&loggedIn, http.MethodGet, "/v1/session", nil, nil), 401, "auth_required")
	// A consumed login assertion cannot mint another session after logout.
	httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: options.ChallengeID, Credential: assertion}), nil), 409, "challenge_consumed")
}

func TestServerDomainErrorsExposeStableSanitizedHTTPFailures(t *testing.T) {
	const secret = "synthetic-private-token=/private/operator/credential-secret"
	cases := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{"approval prerequisite", &model.ProgramError{Code: "approval_required", Message: secret}, 409, "stale_subject", false},
		{"budget exhausted", &model.ProgramError{Code: "budget_exhausted", Message: secret}, 409, "stale_subject", false},
		{"missing prerequisite", &model.ProgramError{Code: "prerequisite_missing", Message: secret}, 409, "stale_subject", false},
		{"uncertain operation", &model.ProgramError{Code: "operation_uncertain", Message: secret}, 409, "attempt_unsettled", false},
		{"rejected source", &model.ProgramError{Code: "source_rejected", Message: secret}, 409, "evidence_invalid", false},
		{"changed policy", &model.ProgramError{Code: "policy_changed", Message: secret}, 409, "stale_policy", false},
		{"waiting ownership", store.ErrWaiting, 409, "attempt_unsettled", false},
		{"unexpected storage failure", fmt.Errorf("database failed: %s", secret), 503, "storage_unavailable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapping reflects real handler call chains; neither the domain's
			// private detail nor an unexpected error may become response text.
			w := httptest.NewRecorder()
			writeError(w, apiFailure(fmt.Errorf("controller operation: %w", tc.err)))
			httpStatus(t, w, tc.status, tc.code)
			response := httpDecode[model.IntakeErrorResponse](t, w)
			if response.Error.Retryable != tc.retryable {
				t.Fatalf("retryable = %v, want %v", response.Error.Retryable, tc.retryable)
			}
			for _, private := range []string{secret, "credential-secret", "controller operation:", "database failed:"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatalf("HTTP failure leaked private error detail %q", private)
				}
			}
		})
	}
}

func TestServerPrivateProgramImportDoesNotRequireModelDraftConsent(t *testing.T) {
	f := newHTTPFixture(t)
	f.cfg.Discovery = model.NormalizeDiscoveryPolicy(f.cfg.Discovery)
	var err error
	f.handler, err = New(f.cfg, f.db)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("Private canonical Program seed\n")
	upload := f.upload(&f.owner, data)
	httpStatus(t, f.put(&f.owner, upload, data, nil), 200, "")
	final := f.request(&f.owner, http.MethodPost, "/v1/bundles/"+upload.ID+"/finalize", []byte(`{}`), nil)
	httpStatus(t, final, 200, "")
	completed := httpDecode[model.BundleUpload](t, final)
	if completed.Bundle == nil {
		t.Fatal("private upload omitted finalized seed manifest")
	}
	sha := completed.Bundle.ManifestSHA
	spec := model.ProgramSpec{
		ID: "private-canonical-program", Project: "demo", Title: "Review a private Program contract",
		Objective:  "Inspect only the exact approved seed and publish unapproved implementation proposals.",
		Seeds:      []model.ProgramSeed{{ID: "private-notes", Kind: "bundle", Path: "notes/private.md"}},
		Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{f.cfg.AstraModel}, PrivateMaterialToModels: true},
	}
	source := string(httpJSON(t, spec))
	request := f.save(&f.owner, "Import an existing canonical Program; do not invoke a drafting model.")
	payload := model.DraftReplacePayload{Route: model.IntakeRouteProgram, Format: model.IntakeFormatProgram, CanonicalSource: source, BundleManifestSHA: sha}
	// Referencing bundle paths without selecting their exact finalized manifest
	// is not an implicit selection of any uploaded private files.
	missingBundle := payload
	missingBundle.BundleManifestSHA = ""
	httpStatus(t, f.execute(&f.owner, f.command(model.ActionDraftReplace, &request, missingBundle)), 400, "invalid_contract")
	httpStatus(t, f.execute(&f.owner, f.command(model.ActionDraftReplace, &request, payload)), 200, "")
	imported := f.saved(request.ID)
	draft, err := f.db.IntakeDraft(context.Background(), request.ID, imported.LatestDraftRevision)
	if err != nil || draft.BundleManifestSHA != sha || draft.CanonicalSource != source || draft.SourceSHA != intake.SHA256([]byte(source)) || draft.DomainRevision == "" {
		t.Fatalf("private canonical import lost exact source or seed identity: %+v %v", draft, err)
	}
	if imported.State != model.IntakeReviewReady || imported.ContextManifest.Disclosure.PrivateMaterialToModels || len(imported.ContextManifest.Disclosure.BundleManifestSHAs) != 0 || imported.ActiveStepID != "" || imported.LifetimeDispatches != 0 {
		t.Fatalf("canonical import implicitly authorized a drafting model: %+v", imported)
	}
	steps, err := f.db.IntakeSteps(context.Background(), request.ID)
	if err != nil || len(steps) != 0 {
		t.Fatalf("canonical import created draft model work: %+v %v", steps, err)
	}
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/subjects/program/"+spec.ID, nil, nil), 404, "not_found")
	peer := f.actor("canonical-program-peer", []string{"demo"})
	peerRequest := f.save(&peer, "A same-project operator still cannot select another operator's private seed.")
	httpStatus(t, f.execute(&peer, f.command(model.ActionDraftReplace, &peerRequest, payload)), 403, "project_forbidden")
	if got := f.saved(peerRequest.ID); got.State != model.IntakeCollecting || got.LatestDraftRevision != 0 {
		t.Fatalf("foreign private bundle selection mutated canonical request: %+v", got)
	}
	approve := f.command(model.ActionIntakeApproveQueue, &imported, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
	httpStatus(t, f.execute(&f.owner, approve), 403, "human_verification_required")
	review, options := f.authorization(&f.owner, approve)
	if review.SeedManifest == nil || review.SeedManifest.ManifestSHA != sha || review.ProgramRevision == nil || review.ProgramRevision.RevisionHash != draft.DomainRevision || !reflect.DeepEqual(review.Claims.BundleManifestSHAs, []string{sha}) {
		t.Fatalf("exact Program review did not expose and bind selected private seed: %+v", review)
	}
	assertion := f.assertion(f.owner, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	httpStatus(t, f.commit(&f.owner, approve, options, assertion), 200, "")
	submitted := f.saved(request.ID)
	if submitted.State != model.IntakeSubmitted || submitted.ContextManifest.Disclosure.PrivateMaterialToModels || submitted.LifetimeDispatches != 0 {
		t.Fatalf("explicit Program approval invented an intake model grant: %+v", submitted)
	}
	w := f.request(&f.owner, http.MethodGet, "/v1/subjects/program/"+spec.ID, nil, nil)
	httpStatus(t, w, 200, "")
	subject := httpDecode[model.IntakeSubjectView](t, w)
	if subject.ProgramRevision == nil || subject.ProgramRevision.RevisionHash != draft.DomainRevision || subject.ProgramRevision.SeedManifestSHA != sha || subject.SeedManifest == nil || subject.SeedManifest.ManifestSHA != sha {
		t.Fatalf("approved Program did not durably retain exact reviewed private seed: %+v", subject)
	}
	httpStatus(t, f.request(&peer, http.MethodGet, "/v1/subjects/program/"+spec.ID, nil, nil), 403, "project_forbidden")
}

func TestServerConcurrentAuthorizationOptionsRecoverSameUsableChallenge(t *testing.T) {
	f := newHTTPFixture(t)
	feature := f.importFeature("concurrent-options-feature", "Approve only one exact immutable contract after a lost options response.")
	command := f.command(model.ActionFeatureApprove, nil, model.SubjectCommandPayload{Kind: "feature", ID: feature.ID, Revision: feature.Revision})
	w := f.request(&f.owner, http.MethodPost, "/v1/reviews", httpJSON(t, model.ReviewRequest{Command: command}), nil)
	httpStatus(t, w, 200, "")
	review := httpDecode[model.IntakeReview](t, w)
	body := httpJSON(t, model.AuthorizationOptionsRequest{ReviewID: review.ID})
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			responses <- f.request(&f.owner, http.MethodPost, "/v1/authorizations/options", body, nil)
		}()
	}
	close(start)
	// Drain both requests before reporting a failure, so neither handler can
	// outlive fixture cleanup or leave a blocked response sender.
	first, second := <-responses, <-responses
	httpStatus(t, first, 200, "")
	httpStatus(t, second, 200, "")
	one := httpDecode[ceremonyOptions](t, first)
	two := httpDecode[ceremonyOptions](t, second)
	if one.ChallengeID == "" || one.ChallengeID != two.ChallengeID || len(one.PublicKey) == 0 || !bytes.Equal(one.PublicKey, two.PublicKey) {
		t.Fatalf("simultaneous retries received incompatible authenticator challenges: first=%s second=%s", first.Body.String(), second.Body.String())
	}
	digest, err := intake.CommandDigest(review.Command)
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []ceremonyOptions{one, two} {
		if options.ReviewID != review.ID || options.ClaimsSHA != review.ClaimsSHA || options.CommandDigest != digest || options.ChallengeID != review.Claims.ChallengeID {
			t.Fatal("recovered authenticator options did not pin the displayed immutable review and command")
		}
	}
	// The recovered winner must actually verify and commit; identical-looking
	// options disconnected from the stored WebAuthn session are not sufficient.
	assertion := f.assertion(f.owner, two, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	httpStatus(t, f.commit(&f.owner, command, one, assertion), 200, "")
	stored, err := f.db.Get(context.Background(), feature.ID)
	if err != nil || stored.State != "ready" || stored.PolicyHash != f.cfg.PolicyHash {
		t.Fatalf("recovered concurrent options could not grant the reviewed subject: %+v %v", stored, err)
	}
}

func (f *httpFixture) registrationActor(operator model.Operator, label string) httpActor {
	f.t.Helper()
	seed := sha256.Sum256([]byte("test-only-registration-key-" + label))
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(seed[:])
	d.Mod(d, new(big.Int).Sub(curve.Params().N, big.NewInt(1)))
	d.Add(d, big.NewInt(1))
	x, y := curve.ScalarBaseMult(d.Bytes())
	key := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x.FillBytes(make([]byte, 32)), -3: y.FillBytes(make([]byte, 32))})
	if err != nil {
		f.t.Fatal(err)
	}
	id := sha256.Sum256([]byte("registration-credential-" + label))
	return httpActor{operator: operator, key: key, credential: model.OperatorCredential{ID: base64.RawURLEncoding.EncodeToString(id[:]), PublicKey: publicKey, UserHandle: operator.UserHandle}}
}

func (f *httpFixture) invite(actor httpActor, label string) (model.OperatorPairing, string) {
	f.t.Helper()
	secretBytes := sha256.Sum256([]byte("trusted-registration-invitation-" + label))
	secret := base64.RawURLEncoding.EncodeToString(secretBytes[:])
	now := time.Now().UTC().Add(-time.Second)
	pair := model.OperatorPairing{ID: f.id(), SecretHash: intake.SHA256([]byte(secret)), OperatorID: actor.operator.ID, ProjectScope: actor.operator.ProjectScope, MaxCredentials: f.cfg.Intake.MaxCredentials, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err := f.db.CreatePairing(context.Background(), pair); err != nil {
		f.t.Fatal(err)
	}
	return pair, secret
}

func (f *httpFixture) register(actor httpActor, secret string) (registrationFinishRequest, *httptest.ResponseRecorder) {
	f.t.Helper()
	w := f.request(nil, http.MethodPost, "/v1/auth/register/options", httpJSON(f.t, registrationOptionsRequest{PairingSecret: secret}), nil)
	httpStatus(f.t, w, 200, "")
	options := httpDecode[ceremonyOptions](f.t, w)
	var creation struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(options.PublicKey, &creation); err != nil {
		f.t.Fatal(err)
	}
	if creation.Challenge == "" {
		f.t.Fatal("registration did not provide authenticator challenge")
	}
	clientData := httpJSON(f.t, map[string]any{"type": "webauthn.create", "challenge": creation.Challenge, "origin": f.cfg.Intake.ExternalOrigin, "crossOrigin": false})
	id, err := base64.RawURLEncoding.DecodeString(actor.credential.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(f.cfg.Intake.RPID))
	authData := make([]byte, 55+len(id)+len(actor.credential.PublicKey))
	copy(authData[:32], rpHash[:])
	authData[32] = byte(protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagAttestedCredentialData)
	binary.BigEndian.PutUint16(authData[53:55], uint16(len(id)))
	copy(authData[55:], id)
	copy(authData[55+len(id):], actor.credential.PublicKey)
	// Standard privacy-preserving none attestation, followed by real signed
	// login below. This is not physical authenticator/hardware attestation.
	object, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": authData, "attStmt": map[string]any{}})
	if err != nil {
		f.t.Fatal(err)
	}
	credential := httpJSON(f.t, protocol.CredentialCreationResponse{
		PublicKeyCredential: protocol.PublicKeyCredential{Credential: protocol.Credential{ID: actor.credential.ID, Type: "public-key"}, RawID: id},
		AttestationResponse: protocol.AuthenticatorAttestationResponse{AuthenticatorResponse: protocol.AuthenticatorResponse{ClientDataJSON: clientData}, AttestationObject: object, Transports: []string{"internal"}},
	})
	finish := registrationFinishRequest{PairingSecret: secret, ChallengeID: options.ChallengeID, Credential: credential}
	return finish, f.request(nil, http.MethodPost, "/v1/auth/register/finish", httpJSON(f.t, finish), nil)
}

func TestServerRegisteredPairingCannotRevealPendingCredential(t *testing.T) {
	f := newHTTPFixture(t)
	device := f.registrationActor(f.owner.operator, "pending-device")
	pair, secret := f.invite(device, "pending-device")
	finish, success := f.register(device, secret)
	httpStatus(t, success, 200, "")
	var registered struct {
		PairingID    string `json:"pairing_id"`
		CredentialID string `json:"credential_id"`
		Fingerprint  string `json:"fingerprint"`
		Pending      bool   `json:"pending_confirmation"`
	}
	if err := json.Unmarshal(success.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if !registered.Pending || registered.PairingID != pair.ID || registered.CredentialID != device.credential.ID || registered.Fingerprint == "" {
		t.Fatalf("successful own registration did not expose exact CLI confirmation identity: %+v", registered)
	}
	if len(success.Result().Cookies()) != 0 {
		t.Fatal("registration minted a browser session before trusted confirmation and fresh login")
	}
	pending, err := f.db.GetPairing(context.Background(), pair.ID)
	if err != nil || pending.State != "registered" || pending.PendingCredential == nil || pending.PendingCredential.ID != device.credential.ID || pending.PendingCredential.Fingerprint != registered.Fingerprint {
		t.Fatalf("registration did not persist pending exact credential: %+v %v", pending, err)
	}
	if _, err := f.db.GetCredential(context.Background(), device.credential.ID); err == nil {
		t.Fatal("registration activated an unconfirmed credential")
	}
	for _, retry := range []struct {
		path string
		body []byte
	}{
		{"/v1/auth/register/options", httpJSON(t, registrationOptionsRequest{PairingSecret: secret})},
		{"/v1/auth/register/finish", httpJSON(t, finish)},
	} {
		w := f.request(nil, http.MethodPost, retry.path, retry.body, nil)
		httpStatus(t, w, 409, "challenge_consumed")
		for _, private := range []string{registered.Fingerprint, device.credential.ID, secret, `"fingerprint"`, `"credential_id"`, `"pending_confirmation"`} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("reused registered pairing disclosed pending confirmation material %q", private)
			}
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("registered-pairing replay minted a session")
		}
	}
	httpStatus(t, f.request(nil, http.MethodGet, "/v1/session", nil, nil), 401, "auth_required")
	// Even actual possession of the newly registered private key cannot log in
	// until the trusted CLI confirms the exact pending fingerprint.
	w := f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
	httpStatus(t, w, 200, "")
	options := httpDecode[ceremonyOptions](t, w)
	assertion := f.assertion(device, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: options.ChallengeID, Credential: assertion}), nil), 403, "human_verification_required")
}

func TestServerTrustedRestoreRotationRequiresFreshPairingAndPreservesHistory(t *testing.T) {
	f := newHTTPFixture(t)
	peer := f.actor("restore-peer", []string{"demo"})
	command := f.command(model.ActionRequestCreate, nil, model.RequestCreatePayload{Project: "demo", Text: "Keep the permanent ledger across an explicit trusted restore fence.", ClientMessageID: f.id()})
	w := f.execute(&f.owner, command)
	httpStatus(t, w, 200, "")
	receipt := httpDecode[model.CommandReceipt](t, w)
	w = f.request(&f.owner, http.MethodGet, "/v1/session", nil, nil)
	httpStatus(t, w, 200, "")
	before := httpDecode[model.IntakeSessionView](t, w)
	if before.InstallationID == "" || before.AuthGeneration == "" {
		t.Fatalf("session omitted recovery fences: %+v", before)
	}
	w = f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
	httpStatus(t, w, 200, "")
	oldOptions := httpDecode[ceremonyOptions](t, w)
	oldAssertion := f.assertion(f.owner, oldOptions, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	// This is the explicit trusted restore operation, not automatic detection
	// of an arbitrary rollback or a claim about external backup provenance.
	generation, err := f.db.RotateAuthGeneration(context.Background())
	if err != nil || generation == "" || generation == before.AuthGeneration {
		t.Fatalf("trusted restore did not rotate canonical session fence: %q %v", generation, err)
	}
	installation, err := f.db.InstallationID(context.Background())
	if err != nil || installation != before.InstallationID {
		t.Fatalf("restore fence changed installation identity: %q %v", installation, err)
	}
	for _, actor := range []httpActor{f.owner, peer} {
		credential, err := f.db.GetCredential(context.Background(), actor.credential.ID)
		if err != nil || credential.RevokedAt == nil {
			t.Fatalf("trusted restore left an existing credential live: %+v %v", credential, err)
		}
		httpStatus(t, f.request(&actor, http.MethodGet, "/v1/session", nil, nil), 401, "auth_required")
		// Fresh ceremony options must not re-activate a pre-restore credential.
		w = f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
		httpStatus(t, w, 200, "")
		options := httpDecode[ceremonyOptions](t, w)
		assertion := f.assertion(actor, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
		httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: options.ChallengeID, Credential: assertion}), nil), 403, "human_verification_required")
	}
	httpStatus(t, f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: oldOptions.ChallengeID, Credential: oldAssertion}), nil), 401, "auth_required")
	httpStatus(t, f.execute(&f.owner, command), 401, "auth_required")
	httpStatus(t, f.request(&f.owner, http.MethodGet, "/v1/commands/"+command.CommandID, nil, nil), 401, "auth_required")
	historical, err := f.db.GetCommandReceipt(context.Background(), f.owner.operator.ID, command.CommandID)
	if err != nil || !reflect.DeepEqual(historical, receipt) {
		t.Fatalf("restore fence rewrote historical command ledger: %+v %v", historical, err)
	}
	replacement := f.registrationActor(f.owner.operator, "post-restore-device")
	pair, secret := f.invite(replacement, "post-restore-device")
	_, registered := f.register(replacement, secret)
	httpStatus(t, registered, 200, "")
	var confirmation struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(registered.Body.Bytes(), &confirmation); err != nil {
		t.Fatal(err)
	}
	if confirmation.Fingerprint == "" {
		t.Fatal("fresh trusted pairing did not expose its own fingerprint")
	}
	replacement.credential, err = f.db.ConfirmPairing(context.Background(), pair.ID, confirmation.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	w = f.request(nil, http.MethodPost, "/v1/auth/login/options", []byte(`{}`), nil)
	httpStatus(t, w, 200, "")
	options := httpDecode[ceremonyOptions](t, w)
	assertion := f.assertion(replacement, options, protocol.FlagUserPresent|protocol.FlagUserVerified, f.cfg.Intake.ExternalOrigin, 1)
	w = f.request(nil, http.MethodPost, "/v1/auth/login/finish", httpJSON(t, loginFinishRequest{ChallengeID: options.ChallengeID, Credential: assertion}), nil)
	httpStatus(t, w, 200, "")
	session := httpDecode[model.IntakeSessionView](t, w)
	if session.AuthGeneration != generation || session.InstallationID != before.InstallationID || session.OperatorID != f.owner.operator.ID || session.CredentialID != replacement.credential.ID {
		t.Fatalf("fresh trusted pairing did not enter current restore fence: %+v", session)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("fresh login session cookies = %+v", cookies)
	}
	replacement.token = cookies[0].Value
	w = f.request(&replacement, http.MethodGet, "/v1/commands/"+command.CommandID, nil, nil)
	httpStatus(t, w, 200, "")
	if got := httpDecode[model.CommandReceipt](t, w); !reflect.DeepEqual(got, receipt) {
		t.Fatal("freshly paired operator could not reconcile immutable pre-restore command history")
	}
}
