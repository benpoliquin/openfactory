// Package operatorauth verifies browser ceremonies with the maintained WebAuthn
// implementation. Persistent authority and one-time consumption belong to store.
package operatorauth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"openfactory/internal/model"
)

var ErrVerification = errors.New("human verification required")

type Verifier struct {
	web    *webauthn.WebAuthn
	policy model.IntakePolicy
}
type User struct {
	Operator    model.Operator
	Credentials []webauthn.Credential
}

func (u User) WebAuthnID() []byte                         { return u.Operator.UserHandle }
func (u User) WebAuthnName() string                       { return u.Operator.ID }
func (u User) WebAuthnDisplayName() string                { return u.Operator.Label }
func (u User) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

func New(p model.IntakePolicy) (*Verifier, error) {
	origin, err := url.Parse(p.ExternalOrigin)
	if err != nil || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Host == "" || strings.Contains(p.RPID, ":") || !(origin.Scheme == "https" || origin.Scheme == "http" && origin.Hostname() == "localhost") {
		return nil, errors.New("invalid WebAuthn origin")
	}
	if origin.Hostname() != p.RPID {
		return nil, errors.New("WebAuthn RP must equal origin hostname")
	}
	if p.ChallengeTTLSeconds <= 0 || p.ChallengeTTLSeconds > 120 || p.CredentialTTLSeconds <= 0 {
		return nil, errors.New("invalid WebAuthn lifetime")
	}
	ttl := time.Duration(p.ChallengeTTLSeconds) * time.Second
	w, err := webauthn.New(&webauthn.Config{RPID: p.RPID, RPDisplayName: p.RPName, RPOrigins: []string{p.ExternalOrigin}, RPAllowCrossOrigin: false, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}, Timeouts: webauthn.TimeoutsConfig{Login: webauthn.TimeoutConfig{Enforce: true, Timeout: ttl}, Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ttl}}})
	if err != nil {
		return nil, err
	}
	return &Verifier{web: w, policy: p}, nil
}
func LoadUser(op model.Operator, records []model.OperatorCredential, now time.Time) (User, error) {
	if !op.Enabled || op.RevokedAt != nil || len(op.UserHandle) == 0 || len(op.UserHandle) > 64 {
		return User{}, ErrVerification
	}
	u := User{Operator: op, Credentials: make([]webauthn.Credential, 0, len(records))}
	for _, r := range records {
		if r.OperatorID != op.ID || r.RevokedAt != nil || !now.Before(r.ExpiresAt) {
			continue
		}
		var c webauthn.Credential
		if json.Unmarshal(r.CredentialData, &c) != nil || len(c.ID) == 0 || len(c.PublicKey) == 0 || base64.RawURLEncoding.EncodeToString(c.ID) != r.ID || !bytes.Equal(c.PublicKey, r.PublicKey) || !bytes.Equal(r.UserHandle, op.UserHandle) {
			return User{}, ErrVerification
		}
		u.Credentials = append(u.Credentials, c)
	}
	return u, nil
}
func (v *Verifier) BeginRegistration(u User) (any, json.RawMessage, error) {
	options, session, err := v.web.BeginRegistration(u, webauthn.WithRegistrationOrigin(v.policy.ExternalOrigin), webauthn.WithExclusions(webauthn.Credentials(u.Credentials).CredentialDescriptors()), webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired))
	if err != nil {
		return nil, nil, ErrVerification
	}
	raw, err := json.Marshal(session)
	return options.Response, raw, err
}
func (v *Verifier) BeginLogin() (any, json.RawMessage, error) {
	options, session, err := v.web.BeginDiscoverableLogin(webauthn.WithLoginOrigin(v.policy.ExternalOrigin), webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, nil, ErrVerification
	}
	raw, err := json.Marshal(session)
	return options.Response, raw, err
}
func (v *Verifier) BeginCommand(u User, credentialID string) (any, json.RawMessage, error) {
	var allowed []protocol.CredentialDescriptor
	for _, c := range u.Credentials {
		if base64.RawURLEncoding.EncodeToString(c.ID) == credentialID {
			allowed = append(allowed, c.Descriptor())
		}
	}
	if len(allowed) != 1 {
		return nil, nil, ErrVerification
	}
	options, session, err := v.web.BeginLogin(u, webauthn.WithLoginOrigin(v.policy.ExternalOrigin), webauthn.WithAllowedCredentials(allowed), webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, nil, ErrVerification
	}
	raw, err := json.Marshal(session)
	return options.Response, raw, err
}
func sessionData(raw json.RawMessage) (webauthn.SessionData, error) {
	var s webauthn.SessionData
	if json.Unmarshal(raw, &s) != nil || s.Challenge == "" || s.UserVerification != protocol.VerificationRequired {
		return s, ErrVerification
	}
	return s, nil
}
func request(raw json.RawMessage) *http.Request {
	return &http.Request{Body: io.NopCloser(bytes.NewReader(raw)), Header: http.Header{"Content-Type": []string{"application/json"}}}
}
func (v *Verifier) FinishRegistration(u User, session, credential json.RawMessage, now time.Time) (model.OperatorCredential, error) {
	s, err := sessionData(session)
	if err != nil {
		return model.OperatorCredential{}, err
	}
	c, err := v.web.FinishRegistration(u, s, request(credential))
	if err != nil || !c.Flags.UserPresent || !c.Flags.UserVerified {
		return model.OperatorCredential{}, ErrVerification
	}
	return v.record(u.Operator, c, model.OperatorCredential{}, now)
}

// FinishLogin resolves the credential only after parsing the assertion with the
// library. The lookup must return a live confirmed credential and its operator.
// Missing or denied identities return ErrVerification; operational lookup
// errors are preserved so callers can distinguish storage failure from proof.
func (v *Verifier) FinishLogin(session, assertion json.RawMessage, lookup func(string) (model.Operator, []model.OperatorCredential, error), now time.Time) (model.OperatorCredential, error) {
	s, err := sessionData(session)
	if err != nil {
		return model.OperatorCredential{}, err
	}
	var op model.Operator
	var old model.OperatorCredential
	var lookupErr error
	c, err := v.web.FinishDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		recordsOp, records, e := lookup(base64.RawURLEncoding.EncodeToString(rawID))
		if e != nil {
			if !errors.Is(e, ErrVerification) {
				lookupErr = e
			}
			return nil, e
		}
		if !bytes.Equal(userHandle, recordsOp.UserHandle) {
			return nil, ErrVerification
		}
		u, e := LoadUser(recordsOp, records, now)
		if e != nil {
			return nil, e
		}
		op = recordsOp
		for _, r := range records {
			if r.ID == base64.RawURLEncoding.EncodeToString(rawID) {
				old = r
			}
		}
		if old.ID == "" {
			return nil, ErrVerification
		}
		return u, nil
	}, s, request(assertion))
	// The verifier library wraps callback failures as ceremony failures. A
	// storage outage is not failed human proof: retain it for the HTTP layer's
	// retryable 503 classification, while missing identities use ErrVerification.
	if lookupErr != nil {
		return model.OperatorCredential{}, lookupErr
	}
	if err != nil || c.Authenticator.CloneWarning {
		return model.OperatorCredential{}, ErrVerification
	}
	return v.record(op, c, old, now)
}
func (v *Verifier) FinishCommand(u User, old model.OperatorCredential, session, assertion json.RawMessage, now time.Time) (model.OperatorCredential, error) {
	s, err := sessionData(session)
	if err != nil {
		return model.OperatorCredential{}, err
	}
	c, err := v.web.FinishLogin(u, s, request(assertion))
	if err != nil || c.Authenticator.CloneWarning || base64.RawURLEncoding.EncodeToString(c.ID) != old.ID {
		return model.OperatorCredential{}, ErrVerification
	}
	return v.record(u.Operator, c, old, now)
}
func (v *Verifier) record(op model.Operator, c *webauthn.Credential, old model.OperatorCredential, now time.Time) (model.OperatorCredential, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return old, err
	}
	if old.ID == "" {
		old = model.OperatorCredential{ID: base64.RawURLEncoding.EncodeToString(c.ID), OperatorID: op.ID, PublicKey: c.PublicKey, UserHandle: op.UserHandle, Fingerprint: SHA256(c.PublicKey), CreatedAt: now, ExpiresAt: now.Add(time.Duration(v.policy.CredentialTTLSeconds) * time.Second)}
	}
	old.CredentialData = raw
	old.SignCount = c.Authenticator.SignCount
	old.BackupEligible = c.Flags.BackupEligible
	old.BackupState = c.Flags.BackupState
	old.LastUsedAt = now
	return old, nil
}
func SHA256(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// CSRFToken is a domain-separated pseudorandom synchronizer token recoverable
// from the HttpOnly session secret. Only its hash is persisted by the store.
func CSRFToken(sessionToken string) string {
	h := hmac.New(sha256.New, []byte(sessionToken))
	h.Write([]byte("openfactory-intake-csrf-v1"))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
