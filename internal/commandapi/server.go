// Package commandapi serves the private, same-origin operator API. It is not a
// BB RPC bridge: only a live passkey-derived browser session is an actor.
package commandapi

import (
	"crypto/hmac"
	"database/sql"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/operatorauth"
	"openfactory/internal/store"
)

// SessionCookieName identifies operator authentication and must never be proxied
// to BB or another upstream service.
const SessionCookieName = "openfactory_intake_session"

// CSRFHeaderName carries the synchronizer proof only for the operator API.
const CSRFHeaderName = "X-OpenFactory-CSRF"

type Server struct {
	cfg      model.Config
	db       *store.Store
	verifier *operatorauth.Verifier
}
type principal struct {
	token, tokenSHA, csrfSHA string
	session                  model.BrowserSession
	operator                 model.Operator
	credential               model.OperatorCredential
}

// New returns an internal /v1 handler. The listener owner must drain its
// http.Server before closing the store. It owns no goroutines or listeners.
// A private same-origin HTTPS proxy strips /openfactory-intake from public paths.
func New(cfg model.Config, db *store.Store) (http.Handler, error) {
	if !cfg.Intake.Enabled {
		return nil, errors.New("intake is disabled")
	}
	if db == nil {
		return nil, errors.New("intake requires durable store")
	}
	if cfg.Intake.PublicPath != "/openfactory-intake" {
		return nil, errors.New("invalid intake public path")
	}
	verifier, err := operatorauth.New(cfg.Intake)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, db: db, verifier: verifier}, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.RawPath != "" || !strings.HasPrefix(r.URL.Path, "/v1/") || strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "\\") {
		writeError(w, notFound())
		return
	}
	for key := range r.URL.Query() {
		switch strings.ToLower(key) {
		case "token", "secret", "pairing_secret", "authorization", "access_token", "csrf_token":
			writeError(w, invalidJSON())
			return
		}
	}
	mutation := r.Method != "GET" && r.Method != "HEAD"
	upload := r.Method == "PUT" && strings.Contains(r.URL.Path, "/files/")
	if err := checkBrowserBoundary(r, s.cfg.Intake.ExternalOrigin, mutation, mutation && !upload); err != nil {
		writeError(w, err)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/bootstrap" {
		writeJSON(w, 200, model.IntakeBootstrap{Enabled: true, ExternalOrigin: s.cfg.Intake.ExternalOrigin, PublicPath: s.cfg.Intake.PublicPath, RPName: s.cfg.Intake.RPName})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/auth/") && r.URL.Path != "/v1/auth/logout" {
		s.auth(w, r)
		return
	}
	p, err := s.authenticate(r, mutation)
	if err != nil {
		writeError(w, apiFailure(err))
		return
	}
	switch {
	case r.Method == "GET":
		err = s.read(w, r, p)
	case r.Method == "POST" && r.URL.Path == "/v1/auth/logout":
		err = s.logout(w, r, p)
	case r.Method == "POST" && r.URL.Path == "/v1/commands":
		err = s.command(w, r, p)
	case r.Method == "POST" && r.URL.Path == "/v1/disclosures":
		err = s.disclosure(w, r, p)
	case r.Method == "POST" && r.URL.Path == "/v1/reviews":
		err = s.review(w, r, p)
	case r.Method == "POST" && r.URL.Path == "/v1/authorizations/options":
		err = s.authorizationOptions(w, r, p)
	case r.Method == "POST" && r.URL.Path == "/v1/authorizations/commit":
		err = s.authorizationCommit(w, r, p)
	case strings.HasPrefix(r.URL.Path, "/v1/bundles"):
		err = s.bundle(w, r, p)
	default:
		err = notFound()
	}
	if err != nil {
		writeError(w, apiFailure(err))
	}
}
func (s *Server) authenticate(r *http.Request, mutation bool) (principal, error) {
	var p principal
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == SessionCookieName {
			count++
			p.token = cookie.Value
		}
	}
	if count != 1 || !validSecret(p.token) {
		return p, store.ErrAuthRequired
	}
	p.tokenSHA = sha256Hex([]byte(p.token))
	var err error
	p.session, p.operator, p.credential, err = s.db.BrowserSessionByTokenHash(r.Context(), p.tokenSHA)
	if err != nil {
		return p, err
	}
	csrf := operatorauth.CSRFToken(p.token)
	if !hmac.Equal([]byte(p.session.CSRFHash), []byte(sha256Hex([]byte(csrf)))) {
		return p, store.ErrAuthRequired
	}
	if mutation {
		header := r.Header.Values(CSRFHeaderName)
		if len(header) != 1 || !hmac.Equal([]byte(header[0]), []byte(csrf)) {
			return p, store.ErrCSRF
		}
		p.csrfSHA = sha256Hex([]byte(header[0]))
	}
	return p, nil
}
func validSecret(token string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == token
}
func (s *Server) sessionView(r *http.Request, p principal) (model.IntakeSessionView, error) {
	installation, err := s.db.InstallationID(r.Context())
	if err != nil {
		return model.IntakeSessionView{}, err
	}
	generation, err := s.db.AuthGeneration(r.Context())
	if err != nil {
		return model.IntakeSessionView{}, err
	}
	if installation == "" || generation == "" || generation != p.session.AuthGeneration {
		return model.IntakeSessionView{}, store.ErrAuthRequired
	}
	return model.IntakeSessionView{InstallationID: installation, AuthGeneration: generation, OperatorID: p.operator.ID, Label: p.operator.Label, CredentialID: p.credential.ID, ProjectScope: p.operator.ProjectScope, CSRFToken: operatorauth.CSRFToken(p.token)}, nil
}
func (s *Server) setCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: token, Path: s.cfg.Intake.PublicPath + "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, p principal) error {
	var body struct{}
	if err := decodeJSON(w, r, &body, 1024); err != nil {
		return err
	}
	if err := s.db.RevokeBrowserSession(r.Context(), p.session.ID); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: "", Path: s.cfg.Intake.PublicPath + "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: time.Unix(1, 0), MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"logged_out": true})
	return nil
}
func (s *Server) rate(r *http.Request) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Forwarded headers, service keys and Tailscale identity are deliberately not
	// consulted. Only the transport peer bounds unauthenticated attempt volume.
	key := sha256Hex([]byte("auth-v1:" + host))
	return s.takeAuthRate(r, key)
}
func (s *Server) rateAuthorization(r *http.Request, p principal) error {
	// The router authenticates and checks CSRF before this bucket. A shared
	// loopback proxy peer's pre-authentication traffic cannot exhaust it.
	if p.session.ID == "" || p.operator.ID == "" {
		return store.ErrAuthRequired
	}
	return s.takeAuthRate(r, sha256Hex([]byte("command-v1:"+p.session.ID)))
}
func (s *Server) takeAuthRate(r *http.Request, key string) error {
	ok, err := s.db.TakeAuthRateLimit(r.Context(), key, s.cfg.Intake.AuthRateLimit, time.Minute)
	if err != nil {
		return err
	}
	if !ok {
		return apiError{Status: 429, Code: "auth_rate_limit", Message: "Too many authentication attempts. Try again later.", Retryable: true}
	}
	return nil
}
func (s *Server) checkProject(p principal, project string) error {
	if _, ok := s.cfg.Projects[project]; !ok {
		return store.ErrProjectForbidden
	}
	for _, allowed := range p.operator.ProjectScope {
		if project == allowed {
			return nil
		}
	}
	return store.ErrProjectForbidden
}
func notFound() error {
	return apiError{Status: 404, Code: "not_found", Message: "Resource was not found."}
}
func humanRequired() error {
	return apiError{Status: 403, Code: "human_verification_required", Message: "Fresh passkey user verification is required."}
}
func apiFailure(err error) error {
	var e apiError
	var ep *apiError
	if errors.As(err, &e) || errors.As(err, &ep) {
		return err
	}
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		return apiError{Status: 413, Code: "bundle_limit", Message: "Bundle upload exceeds the declared size."}
	}
	switch {
	case errors.Is(err, store.ErrAuthRequired):
		return apiError{Status: 401, Code: "auth_required", Message: "Sign in with your passkey."}
	case errors.Is(err, store.ErrCredentialRevoked):
		return apiError{Status: 401, Code: "credential_revoked", Message: "This credential no longer has access."}
	case errors.Is(err, store.ErrProjectForbidden):
		return apiError{Status: 403, Code: "project_forbidden", Message: "This project is not available to this operator."}
	case errors.Is(err, store.ErrCSRF):
		return apiError{Status: 403, Code: "csrf_failed", Message: "Refresh the authenticated session before trying again."}
	case errors.Is(err, store.ErrChallengeExpired):
		return apiError{Status: 410, Code: "challenge_expired", Message: "The verification challenge has expired."}
	case errors.Is(err, store.ErrChallengeConsumed):
		return apiError{Status: 409, Code: "challenge_consumed", Message: "The verification challenge has already been used."}
	case errors.Is(err, store.ErrPairingExpired):
		return apiError{Status: 410, Code: "pairing_expired", Message: "The pairing invitation has expired."}
	case errors.Is(err, store.ErrPairingState):
		return apiError{Status: 409, Code: "challenge_consumed", Message: "This pairing invitation is no longer available."}
	case errors.Is(err, store.ErrCredentialLimit):
		return apiError{Status: 400, Code: "invalid_contract", Message: "The operator credential limit has been reached."}
	case errors.Is(err, operatorauth.ErrVerification):
		return humanRequired()
	case errors.Is(err, store.ErrWaiting):
		return apiError{Status: 409, Code: "attempt_unsettled", Message: "Existing work must settle before this command can proceed."}
	case errors.Is(err, store.ErrInvalidQuery):
		return invalidJSON()
	case errors.Is(err, store.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return notFound()
	}
	var domain *model.ProgramError
	if errors.As(err, &domain) {
		code := domain.Code
		status := 409
		message := "The saved state changed. Refresh the exact review before trying again."
		switch code {
		case "invalid_request", "invalid_contract", "unsupported_action":
			status = 400
			message = "Request does not match the supported command contract."
		case "disclosure_required", "project_forbidden", "human_verification_required":
			status = 403
			message = "Explicit current operator authorization is required."
		case "input_limit", "bundle_limit":
			status = 413
			message = "Input exceeds the configured limit."
		case "not_found":
			return notFound()
		case "stale_revision", "stale_policy", "stale_subject", "idempotency_conflict", "challenge_consumed", "attempt_unsettled", "evidence_invalid", "question_unresolved":
		case "policy_changed":
			code = "stale_policy"
		case "approval_required", "budget_exhausted", "prerequisite_missing":
			code = "stale_subject"
		case "operation_uncertain":
			code = "attempt_unsettled"
		case "source_rejected":
			code = "evidence_invalid"
		default:
			return err
		}
		return apiError{Status: status, Code: code, Message: message}
	}
	return err
}
