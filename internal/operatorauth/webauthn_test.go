package operatorauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"openfactory/internal/model"
)

// These fixtures emulate assertion generation, not verification: every response
// passes through the maintained library's JSON, COSE, and signature verification.
// Fixed test-only key material makes credentials reproducible; signature entropy
// and ceremony challenges are deliberately not part of any expected value.
type assertionFixture struct {
	verifier *Verifier
	policy   model.IntakePolicy
	op       model.Operator
	old      model.OperatorCredential
	other    model.OperatorCredential
	key      *ecdsa.PrivateKey
	otherKey *ecdsa.PrivateKey
	now      time.Time
}

func assertionPolicy() model.IntakePolicy {
	return model.IntakePolicy{
		Enabled:              true,
		ExternalOrigin:       "https://openfactory.example.test",
		RPID:                 "openfactory.example.test",
		RPName:               "OpenFactory test",
		ChallengeTTLSeconds:  120,
		CredentialTTLSeconds: 86400,
	}
}

func assertionKey(label string) *ecdsa.PrivateKey {
	seed := sha256.Sum256([]byte(label))
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(seed[:])
	d.Mod(d, new(big.Int).Sub(curve.Params().N, big.NewInt(1)))
	d.Add(d, big.NewInt(1))
	x, y := curve.ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
}

func persistedAssertionCredential(t *testing.T, op model.Operator, key *ecdsa.PrivateKey, label string, count uint32, flags protocol.AuthenticatorFlags, now time.Time) model.OperatorCredential {
	t.Helper()
	encoding, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	// COSE EC2 / ES256 / P-256, with fixed-width unsigned coordinates.
	publicKey, err := encoding.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1,
		-2: key.X.FillBytes(make([]byte, 32)),
		-3: key.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256([]byte(label))
	credential := webauthn.Credential{
		ID: id[:], PublicKey: publicKey,
		Flags:         webauthn.NewCredentialFlags(flags),
		Authenticator: webauthn.Authenticator{SignCount: count},
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	return model.OperatorCredential{
		ID: base64.RawURLEncoding.EncodeToString(id[:]), OperatorID: op.ID,
		CredentialData: raw, PublicKey: publicKey, UserHandle: op.UserHandle,
		SignCount: count, BackupEligible: flags.HasBackupEligible(), BackupState: flags.HasBackupState(),
		Fingerprint: SHA256(publicKey), CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}
}

func newAssertionFixture(t *testing.T, count uint32, backup bool) assertionFixture {
	t.Helper()
	f := assertionFixture{policy: assertionPolicy(), now: time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)}
	var err error
	f.verifier, err = New(f.policy)
	if err != nil {
		t.Fatal(err)
	}
	f.op = model.Operator{ID: "operator-test", Label: "Test operator", Enabled: true, UserHandle: []byte("operator-test-user-handle"), ProjectScope: []string{"project-test"}}
	f.key = assertionKey("openfactory-test-signing-key")
	f.otherKey = assertionKey("openfactory-test-second-signing-key")
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified
	if backup {
		flags |= protocol.FlagBackupEligible | protocol.FlagBackupState
	}
	f.old = persistedAssertionCredential(t, f.op, f.key, "openfactory-test-credential", count, flags, f.now)
	f.other = persistedAssertionCredential(t, f.op, f.otherKey, "openfactory-test-second-credential", count, flags, f.now)
	return f
}

func (f assertionFixture) user(t *testing.T) User {
	t.Helper()
	u, err := LoadUser(f.op, []model.OperatorCredential{f.old, f.other}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Credentials) != 2 || !u.Credentials[0].Flags.UserVerified {
		t.Fatal("fixture must load two persisted credentials with remembered UV")
	}
	return u
}

func (f assertionFixture) begin(t *testing.T, ceremony string) json.RawMessage {
	t.Helper()
	var options any
	var session json.RawMessage
	var err error
	if ceremony == "login" {
		options, session, err = f.verifier.BeginLogin()
	} else {
		options, session, err = f.verifier.BeginCommand(f.user(t), f.old.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := options.(protocol.PublicKeyCredentialRequestOptions)
	if !ok {
		t.Fatalf("unexpected assertion options type %T", options)
	}
	var stored webauthn.SessionData
	if err := json.Unmarshal(session, &stored); err != nil {
		t.Fatal(err)
	}
	if opts.UserVerification != protocol.VerificationRequired || stored.UserVerification != protocol.VerificationRequired {
		t.Fatal("fresh ceremony must require UV in both browser options and stored session")
	}
	if opts.RelyingPartyID != f.policy.RPID || len(opts.Challenge) < 32 || base64.RawURLEncoding.EncodeToString(opts.Challenge) != stored.Challenge {
		t.Fatal("browser options and stored RP challenge disagree")
	}
	if ceremony == "login" {
		if len(opts.AllowedCredentials) != 0 || len(stored.UserID) != 0 {
			t.Fatal("login must be discoverable")
		}
	} else {
		id, err := base64.RawURLEncoding.DecodeString(f.old.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(opts.AllowedCredentials) != 1 || !bytes.Equal(opts.AllowedCredentials[0].CredentialID, id) || len(stored.AllowedCredentialIDs) != 1 || !bytes.Equal(stored.AllowedCredentialIDs[0], id) || !bytes.Equal(stored.UserID, f.op.UserHandle) {
			t.Fatal("command ceremony must bind exactly the current session credential and operator")
		}
	}
	return session
}

type signedAssertion struct {
	challenge    string
	origin       string
	rpID         string
	credentialID string
	userHandle   []byte
	flags        protocol.AuthenticatorFlags
	count        uint32
	crossOrigin  bool
	key          *ecdsa.PrivateKey
}

func (f assertionFixture) assertionSpec(t *testing.T, session json.RawMessage, count uint32) signedAssertion {
	t.Helper()
	var stored webauthn.SessionData
	if err := json.Unmarshal(session, &stored); err != nil {
		t.Fatal(err)
	}
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified
	if f.old.BackupEligible {
		flags |= protocol.FlagBackupEligible | protocol.FlagBackupState
	}
	return signedAssertion{challenge: stored.Challenge, origin: f.policy.ExternalOrigin, rpID: f.policy.RPID, credentialID: f.old.ID, userHandle: f.op.UserHandle, flags: flags, count: count, key: f.key}
}

func (s signedAssertion) marshal(t *testing.T) json.RawMessage {
	t.Helper()
	clientData, err := json.Marshal(struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}{Type: "webauthn.get", Challenge: s.challenge, Origin: s.origin, CrossOrigin: s.crossOrigin})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(s.rpID))
	authData := make([]byte, 37)
	copy(authData[:32], rpHash[:])
	authData[32] = byte(s.flags)
	binary.BigEndian.PutUint32(authData[33:], s.count)
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	rawID, err := base64.RawURLEncoding.DecodeString(s.credentialID)
	if err != nil {
		t.Fatal(err)
	}
	response := protocol.CredentialAssertionResponse{
		PublicKeyCredential: protocol.PublicKeyCredential{
			Credential: protocol.Credential{ID: s.credentialID, Type: "public-key"},
			RawID:      rawID,
		},
		AssertionResponse: protocol.AuthenticatorAssertionResponse{
			AuthenticatorResponse: protocol.AuthenticatorResponse{ClientDataJSON: clientData},
			AuthenticatorData:     authData, Signature: signature, UserHandle: s.userHandle,
		},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f assertionFixture) finish(t *testing.T, ceremony string, session, response json.RawMessage) (model.OperatorCredential, error) {
	t.Helper()
	if ceremony == "command" {
		return f.verifier.FinishCommand(f.user(t), f.old, session, response, f.now)
	}
	return f.verifier.FinishLogin(session, response, func(id string) (model.Operator, []model.OperatorCredential, error) {
		if id != f.old.ID && id != f.other.ID {
			return model.Operator{}, nil, ErrVerification
		}
		return f.op, []model.OperatorCredential{f.old, f.other}, nil
	}, f.now)
}

func TestVerifierSignedAssertions(t *testing.T) {
	cases := []struct {
		name   string
		count  uint32
		backup bool
		wantOK bool
		mutate func(*signedAssertion, assertionFixture)
	}{
		{name: "fresh_UP_and_UV", count: 8, wantOK: true},
		{name: "synced_passkey_zero_counter", count: 0, backup: true, wantOK: true},
		{name: "UV_missing_despite_remembered_UV", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.flags &^= protocol.FlagUserVerified }},
		{name: "UP_missing", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.flags &^= protocol.FlagUserPresent }},
		{name: "wrong_origin", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.origin = "https://attacker.example.test" }},
		{name: "cross_origin", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.crossOrigin = true }},
		{name: "wrong_RP_hash", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.rpID = "attacker.example.test" }},
		{name: "credential_ID_substitution", count: 8, mutate: func(s *signedAssertion, f assertionFixture) { s.credentialID = f.other.ID }},
		{name: "wrong_signing_key", count: 8, mutate: func(s *signedAssertion, f assertionFixture) { s.key = f.otherKey }},
		{name: "wrong_user_handle", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.userHandle = []byte("another-operator") }},
		{name: "nonzero_counter_rollback", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.count = 7 }},
		{name: "nonzero_counter_reuse", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.count = 8 }},
		{name: "nonzero_counter_reset_to_zero", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) { s.count = 0 }},
		{name: "challenge_mismatch", count: 8, mutate: func(s *signedAssertion, _ assertionFixture) {
			s.challenge = base64.RawURLEncoding.EncodeToString([]byte("unrelated-32-byte-challenge-value"))
		}},
	}
	for _, ceremony := range []string{"login", "command"} {
		t.Run(ceremony, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					f := newAssertionFixture(t, tc.count, tc.backup)
					session := f.begin(t, ceremony)
					next := tc.count + 1
					if tc.backup {
						next = 0
					}
					spec := f.assertionSpec(t, session, next)
					if tc.mutate != nil {
						tc.mutate(&spec, f)
					}
					got, err := f.finish(t, ceremony, session, spec.marshal(t))
					if !tc.wantOK {
						if !errors.Is(err, ErrVerification) {
							t.Fatalf("invalid signed assertion accepted or wrong error: credential=%q err=%v", got.ID, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("valid signed assertion rejected: %v", err)
					}
					if got.ID != f.old.ID || got.OperatorID != f.op.ID || got.SignCount != next || !got.LastUsedAt.Equal(f.now) || !got.CreatedAt.Equal(f.old.CreatedAt) || !got.ExpiresAt.Equal(f.old.ExpiresAt) || !bytes.Equal(got.PublicKey, f.old.PublicKey) || !bytes.Equal(got.UserHandle, f.op.UserHandle) || got.Fingerprint != f.old.Fingerprint {
						t.Fatal("verified assertion did not preserve identity/lifetime and update usage state")
					}
					var persisted webauthn.Credential
					if err := json.Unmarshal(got.CredentialData, &persisted); err != nil {
						t.Fatal(err)
					}
					if persisted.Authenticator.SignCount != next || persisted.Authenticator.CloneWarning || !persisted.Flags.UserPresent || !persisted.Flags.UserVerified || got.BackupEligible != tc.backup || got.BackupState != tc.backup || persisted.Flags.BackupEligible != tc.backup || persisted.Flags.BackupState != tc.backup {
						t.Fatal("persisted library credential does not match the successful ceremony")
					}
				})
			}
		})
	}
}

func TestVerifierCommandRejectsAnotherOwnedCredential(t *testing.T) {
	f := newAssertionFixture(t, 0, false)
	session := f.begin(t, "command")
	spec := f.assertionSpec(t, session, 1)
	// This response is genuinely signed by another live credential belonging to
	// the same operator, but not the browser session's selected credential.
	spec.credentialID, spec.key = f.other.ID, f.otherKey
	if _, err := f.finish(t, "command", session, spec.marshal(t)); !errors.Is(err, ErrVerification) {
		t.Fatalf("command accepted a different owned credential: %v", err)
	}
	if _, _, err := f.verifier.BeginCommand(f.user(t), "unknown-credential"); !errors.Is(err, ErrVerification) {
		t.Fatalf("command options accepted an unknown credential: %v", err)
	}
}

func TestVerifierRejectsWeakenedOrExpiredSession(t *testing.T) {
	for _, ceremony := range []string{"login", "command"} {
		for _, change := range []string{"UV_preferred", "missing_challenge", "expired"} {
			t.Run(ceremony+"/"+change, func(t *testing.T) {
				f := newAssertionFixture(t, 0, false)
				session := f.begin(t, ceremony)
				response := f.assertionSpec(t, session, 1).marshal(t)
				var stored webauthn.SessionData
				if err := json.Unmarshal(session, &stored); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "UV_preferred":
					stored.UserVerification = protocol.VerificationPreferred
				case "missing_challenge":
					stored.Challenge = ""
				case "expired":
					stored.Expires = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
				}
				session, err := json.Marshal(stored)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.finish(t, ceremony, session, response); !errors.Is(err, ErrVerification) {
					t.Fatalf("invalid server session accepted: %v", err)
				}
			})
		}
	}
}

func TestVerifierOriginPolicy(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		rpID   string
		wantOK bool
	}{
		{"private_HTTPS", "https://openfactory.example.test", "openfactory.example.test", true},
		{"parent_domain_RP_outside_exact_host_policy", "https://openfactory.example.test", "example.test", false},
		{"localhost_development", "http://localhost:38889", "localhost", true},
		{"remote_HTTP", "http://openfactory.example.test", "openfactory.example.test", false},
		{"IPv4", "https://192.0.2.10", "192.0.2.10", false},
		{"IPv6", "https://[2001:db8::1]", "2001:db8::1", false},
		{"loopback_IP_not_localhost", "http://127.0.0.1:38889", "127.0.0.1", false},
		{"path_prefix", "https://openfactory.example.test/openfactory-intake", "openfactory.example.test", false},
		{"trailing_slash", "https://openfactory.example.test/", "openfactory.example.test", false},
		{"wrong_RP", "https://openfactory.example.test", "other.example.test", false},
		{"RP_suffix_without_domain_boundary", "https://notexample.test", "example.test", false},
		{"RP_contains_port", "https://openfactory.example.test:443", "openfactory.example.test:443", false},
		{"userinfo", "https://operator@openfactory.example.test", "openfactory.example.test", false},
		{"query", "https://openfactory.example.test?token=secret", "openfactory.example.test", false},
		{"fragment", "https://openfactory.example.test#pairing", "openfactory.example.test", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := assertionPolicy()
			p.ExternalOrigin, p.RPID = tc.origin, tc.rpID
			v, err := New(p)
			if tc.wantOK {
				if err != nil || v == nil {
					t.Fatalf("valid origin rejected: %v", err)
				}
			} else if err == nil || v != nil {
				t.Fatalf("invalid origin/RP accepted: verifier=%v err=%v", v, err)
			}
		})
	}
}

func TestCSRFTokenStableAndDomainSeparated(t *testing.T) {
	const session = "fixed-high-entropy-test-session-secret"
	token := CSRFToken(session)
	if token == "" || token != CSRFToken(session) || token == CSRFToken(session+"-other") || token == session {
		t.Fatal("CSRF token must be stable per session and distinct across sessions")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != sha256.Size {
		t.Fatalf("CSRF token is not an unpadded base64url SHA-256 MAC: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(session))
	mac.Write([]byte("openfactory-intake-csrf-v1"))
	if !hmac.Equal(raw, mac.Sum(nil)) {
		t.Fatal("CSRF token changed its stable versioned derivation")
	}
	otherDomain := hmac.New(sha256.New, []byte(session))
	otherDomain.Write([]byte("openfactory-intake-command-v1"))
	plainHash := sha256.Sum256([]byte(session))
	if hmac.Equal(raw, otherDomain.Sum(nil)) || hmac.Equal(raw, plainHash[:]) {
		t.Fatal("CSRF derivation must be domain separated, not a reusable session digest")
	}
}

// A none attestation is a standard privacy-preserving WebAuthn registration,
// not a claim of physical authenticator attestation. The resulting public key is
// subsequently exercised by a signed login through the real verifier.
func (s signedAssertion) marshalRegistration(t *testing.T, publicKey []byte) json.RawMessage {
	t.Helper()
	clientData, err := json.Marshal(struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}{Type: "webauthn.create", Challenge: s.challenge, Origin: s.origin, CrossOrigin: s.crossOrigin})
	if err != nil {
		t.Fatal(err)
	}
	id, err := base64.RawURLEncoding.DecodeString(s.credentialID)
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(s.rpID))
	// RP hash, flags, counter, AAGUID, big-endian credential ID length, ID,
	// and the complete CBOR-encoded COSE key.
	authData := make([]byte, 37+16+2+len(id)+len(publicKey))
	copy(authData[:32], rpHash[:])
	authData[32] = byte(s.flags | protocol.FlagAttestedCredentialData)
	binary.BigEndian.PutUint32(authData[33:37], s.count)
	binary.BigEndian.PutUint16(authData[53:55], uint16(len(id)))
	copy(authData[55:], id)
	copy(authData[55+len(id):], publicKey)
	object, err := cbor.Marshal(map[string]any{
		"fmt":      "none",
		"authData": authData,
		"attStmt":  map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := protocol.CredentialCreationResponse{
		PublicKeyCredential: protocol.PublicKeyCredential{
			Credential: protocol.Credential{ID: s.credentialID, Type: "public-key"},
			RawID:      id,
		},
		AttestationResponse: protocol.AuthenticatorAttestationResponse{
			AuthenticatorResponse: protocol.AuthenticatorResponse{ClientDataJSON: clientData},
			AttestationObject:     object,
			Transports:            []string{"internal"},
		},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifierRegistrationAndSignedLogin(t *testing.T) {
	cases := []struct {
		name   string
		wantOK bool
		mutate func(*signedAssertion)
	}{
		{name: "valid_UP_and_UV", wantOK: true},
		{name: "UV_missing", mutate: func(s *signedAssertion) { s.flags &^= protocol.FlagUserVerified }},
		{name: "UP_missing", mutate: func(s *signedAssertion) { s.flags &^= protocol.FlagUserPresent }},
		{name: "wrong_origin", mutate: func(s *signedAssertion) { s.origin = "https://attacker.example.test" }},
		{name: "cross_origin", mutate: func(s *signedAssertion) { s.crossOrigin = true }},
		{name: "wrong_RP", mutate: func(s *signedAssertion) { s.rpID = "attacker.example.test" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAssertionFixture(t, 0, false)
			u := f.user(t)
			options, session, err := f.verifier.BeginRegistration(u)
			if err != nil {
				t.Fatal(err)
			}
			opts, ok := options.(protocol.PublicKeyCredentialCreationOptions)
			if !ok {
				t.Fatalf("unexpected registration options type %T", options)
			}
			var stored webauthn.SessionData
			if err := json.Unmarshal(session, &stored); err != nil {
				t.Fatal(err)
			}
			if opts.AuthenticatorSelection.UserVerification != protocol.VerificationRequired || opts.AuthenticatorSelection.ResidentKey != protocol.ResidentKeyRequirementRequired || stored.UserVerification != protocol.VerificationRequired || !bytes.Equal(stored.UserID, f.op.UserHandle) || len(opts.CredentialExcludeList) != len(u.Credentials) || base64.RawURLEncoding.EncodeToString(opts.Challenge) != stored.Challenge {
				t.Fatal("registration must bind the operator, require resident UP+UV credentials, and exclude enrolled credentials")
			}
			spec := f.assertionSpec(t, session, 0)
			newID := sha256.Sum256([]byte("newly-enrolled-test-credential"))
			spec.credentialID = base64.RawURLEncoding.EncodeToString(newID[:])
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			got, err := f.verifier.FinishRegistration(u, session, spec.marshalRegistration(t, f.old.PublicKey), f.now)
			if !tc.wantOK {
				if !errors.Is(err, ErrVerification) {
					t.Fatalf("invalid registration accepted or wrong error: credential=%q err=%v", got.ID, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid registration rejected: %v", err)
			}
			fingerprint := sha256.Sum256(f.old.PublicKey)
			if got.ID != spec.credentialID || got.OperatorID != f.op.ID || got.Fingerprint != hex.EncodeToString(fingerprint[:]) || !bytes.Equal(got.PublicKey, f.old.PublicKey) || !bytes.Equal(got.UserHandle, f.op.UserHandle) || got.SignCount != 0 || !got.CreatedAt.Equal(f.now) || !got.LastUsedAt.Equal(f.now) || !got.ExpiresAt.Equal(f.now.Add(time.Duration(f.policy.CredentialTTLSeconds)*time.Second)) {
				t.Fatal("registration record lost the verified key, identity, fingerprint, or policy lifetime")
			}
			var persisted webauthn.Credential
			if err := json.Unmarshal(got.CredentialData, &persisted); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(persisted.ID, newID[:]) || !bytes.Equal(persisted.PublicKey, got.PublicKey) || !persisted.Flags.UserPresent || !persisted.Flags.UserVerified || persisted.Authenticator.SignCount != 0 || persisted.AttestationFormat != "none" {
				t.Fatal("registration did not preserve the verified opaque library credential")
			}
			// Prove the actual registered key can subsequently authenticate; no
			// prebuilt credential serialization is substituted into this path.
			f.old = got
			loginSession := f.begin(t, "login")
			assertion := f.assertionSpec(t, loginSession, 1).marshal(t)
			loggedIn, err := f.finish(t, "login", loginSession, assertion)
			if err != nil || loggedIn.ID != got.ID || loggedIn.SignCount != 1 {
				t.Fatalf("newly registered key could not perform a signed login: id=%q count=%d err=%v", loggedIn.ID, loggedIn.SignCount, err)
			}
		})
	}
}

func TestVerifierLoginLookupErrorClassification(t *testing.T) {
	storageErr := errors.New("test credential storage unavailable")
	cases := []struct {
		name           string
		responseChange string
		lookupErr      error
		wantErr        error
		wantCalls      int
	}{
		{
			name:      "storage_failure_is_not_failed_human_proof",
			lookupErr: errors.Join(errors.New("credential lookup failed"), storageErr),
			wantErr:   storageErr,
			wantCalls: 1,
		},
		{
			name:      "unknown_credential_remains_verification_failure",
			lookupErr: ErrVerification,
			wantErr:   ErrVerification,
			wantCalls: 1,
		},
		{
			name:           "malformed_response_never_looks_up_credentials",
			responseChange: "malformed",
			lookupErr:      storageErr,
			wantErr:        ErrVerification,
			wantCalls:      0,
		},
		{
			name:           "signed_wrong_challenge_remains_verification_failure",
			responseChange: "wrong_challenge",
			wantErr:        ErrVerification,
			wantCalls:      1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAssertionFixture(t, 0, false)
			session := f.begin(t, "login")
			spec := f.assertionSpec(t, session, 1)
			if tc.responseChange == "wrong_challenge" {
				spec.challenge = base64.RawURLEncoding.EncodeToString([]byte("unrelated-32-byte-challenge-value"))
			}
			response := spec.marshal(t)
			if tc.responseChange == "malformed" {
				response = json.RawMessage("{")
			}
			calls := 0
			got, err := f.verifier.FinishLogin(session, response, func(id string) (model.Operator, []model.OperatorCredential, error) {
				calls++
				if id != f.old.ID {
					t.Fatalf("lookup received substituted credential ID %q", id)
				}
				if tc.lookupErr != nil {
					return model.Operator{}, nil, tc.lookupErr
				}
				return f.op, []model.OperatorCredential{f.old, f.other}, nil
			}, f.now)
			if calls != tc.wantCalls {
				t.Fatalf("credential lookup calls=%d, want %d", calls, tc.wantCalls)
			}
			if got.ID != "" || !errors.Is(err, tc.wantErr) {
				t.Fatalf("wrong lookup/verification error classification: id=%q err=%v want=%v", got.ID, err, tc.wantErr)
			}
			if errors.Is(tc.wantErr, storageErr) && errors.Is(err, ErrVerification) {
				t.Fatal("storage failure was misclassified as failed human verification")
			}
		})
	}
}
