package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// These are persistence tests, not substitute WebAuthn signature verification.
// Opaque library data is deliberately inert; real ceremonies belong to operatorauth.
func authTestHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func authTestCredential(o model.Operator, now time.Time) model.OperatorCredential {
	return model.OperatorCredential{ID: "credential-" + o.ID, OperatorID: o.ID, CredentialData: json.RawMessage(`{"opaque":"library-credential"}`), PublicKey: []byte("public-key"), UserHandle: o.UserHandle, Fingerprint: authTestHash("fingerprint-" + o.ID), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
}
func authTestInsert(t *testing.T, s *Store, table, id string, value any) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = intakeAuthUpdate(context.Background(), tx, table, id, value); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func authTestActor(t *testing.T, s *Store, now time.Time, suffix string) (model.Operator, model.OperatorCredential, model.BrowserSession) {
	t.Helper()
	ctx := context.Background()
	o := model.Operator{ID: "operator-" + suffix, Label: "Operator " + suffix, Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("handle-" + suffix), CreatedAt: now}
	if err := s.CreateOperator(ctx, o); err != nil {
		t.Fatal(err)
	}
	c := authTestCredential(o, now)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?)`, c.ID, c.OperatorID, data); err != nil {
		t.Fatal(err)
	}
	generation, err := s.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := model.BrowserSession{ID: "session-" + suffix, TokenHash: authTestHash("token-" + suffix), OperatorID: o.ID, CredentialID: c.ID, CSRFHash: authTestHash("csrf-" + suffix), AuthGeneration: generation, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(6 * time.Hour)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = intakeCreateSessionTx(ctx, tx, b, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return o, c, b
}
func authTestAuthenticate(t *testing.T, s *Store, b model.BrowserSession, csrf, project string, now time.Time) error {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, _, _, err = intakeAuthenticateTx(context.Background(), tx, b.TokenHash, csrf, project, now)
	return err
}
func TestIntakeAuthSessionExactExpiryScopeAndCSRF(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, csrf, project string
		at                  time.Time
		want                error
	}{
		{"live", authTestHash("csrf-a"), "demo", now, nil},
		{"read skips csrf", "", "demo", now, nil},
		{"csrf mismatch", authTestHash("wrong"), "demo", now, ErrCSRF},
		{"scope exact", authTestHash("csrf-a"), "dem", now, ErrProjectForbidden},
		{"idle boundary", authTestHash("csrf-a"), "demo", now.Add(time.Hour), ErrAuthRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			_, _, b := authTestActor(t, s, now, "a")
			err := authTestAuthenticate(t, s, b, tc.csrf, tc.project, tc.at)
			if !errors.Is(err, tc.want) {
				t.Fatalf("authenticate: got %v want %v", err, tc.want)
			}
		})
	}
	t.Run("absolute boundary", func(t *testing.T) {
		s := openTest(t)
		_, _, b := authTestActor(t, s, now, "a")
		b.ExpiresAt = now.Add(8 * time.Hour)
		authTestInsert(t, s, "intake_browser_sessions", b.ID, b)
		if err := authTestAuthenticate(t, s, b, "", "demo", b.AbsoluteExpiresAt); !errors.Is(err, ErrAuthRequired) {
			t.Fatalf("absolute expiry: %v", err)
		}
	})
	t.Run("credential boundary", func(t *testing.T) {
		s := openTest(t)
		_, c, b := authTestActor(t, s, now, "a")
		c.ExpiresAt = now.Add(30 * time.Minute)
		authTestInsert(t, s, "intake_credentials", c.ID, c)
		if err := authTestAuthenticate(t, s, b, "", "demo", c.ExpiresAt); !errors.Is(err, ErrCredentialRevoked) {
			t.Fatalf("credential expiry: %v", err)
		}
	})
	t.Run("live scope removal", func(t *testing.T) {
		s := openTest(t)
		o, _, b := authTestActor(t, s, now, "a")
		o.ProjectScope = []string{"other"}
		authTestInsert(t, s, "intake_operators", o.ID, o)
		if err := authTestAuthenticate(t, s, b, "", "demo", now); !errors.Is(err, ErrProjectForbidden) {
			t.Fatalf("live scope: %v", err)
		}
	})
	t.Run("cross operator credential", func(t *testing.T) {
		s := openTest(t)
		_, _, b := authTestActor(t, s, now, "a")
		_, other, _ := authTestActor(t, s, now, "b")
		b.CredentialID = other.ID
		authTestInsert(t, s, "intake_browser_sessions", b.ID, b)
		if err := authTestAuthenticate(t, s, b, "", "demo", now); !errors.Is(err, ErrCredentialRevoked) {
			t.Fatalf("cross operator: %v", err)
		}
	})
}
func authTestLoginChallenge(t *testing.T, s *Store, id string, o model.Operator, c model.OperatorCredential, now time.Time) model.AuthChallenge {
	t.Helper()
	generation, err := s.AuthGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	challenge := model.AuthChallenge{ID: id, Purpose: "login", OperatorID: o.ID, CredentialID: c.ID, SessionData: json.RawMessage(`{"opaque":"library-session"}`), AuthGeneration: generation, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	data, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(context.Background(), `INSERT INTO intake_auth_challenges(id,operator_id,credential_id,session_id,data) VALUES(?,?,?,?,?)`, id, o.ID, c.ID, "", data); err != nil {
		t.Fatal(err)
	}
	return challenge
}
func TestIntakeAuthRevocationAndGeneration(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for _, kind := range []string{"operator", "credential", "session", "generation"} {
		t.Run(kind, func(t *testing.T) {
			s := openTest(t)
			o, c, b := authTestActor(t, s, now, "a")
			challenge := authTestLoginChallenge(t, s, "login", o, c, now)
			var err error
			switch kind {
			case "operator":
				err = s.RevokeOperator(ctx, o.ID)
			case "credential":
				err = s.RevokeCredential(ctx, c.ID)
			case "session":
				challenge.SessionID = b.ID
				authTestInsert(t, s, "intake_auth_challenges", challenge.ID, challenge)
				_, err = s.db.ExecContext(ctx, `UPDATE intake_auth_challenges SET session_id=? WHERE id=?`, b.ID, challenge.ID)
				if err == nil {
					err = s.RevokeBrowserSession(ctx, b.ID)
				}
			case "generation":
				_, err = s.RotateAuthGeneration(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = authTestAuthenticate(t, s, b, "", "demo", now); err == nil {
				t.Fatal("revoked authority authenticated")
			}
			if _, err = s.GetAuthChallenge(ctx, challenge.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("challenge survived %s: %v", kind, err)
			}
			if kind == "generation" {
				if _, err = s.GetCredential(ctx, c.ID); err != nil {
					t.Fatalf("generation deleted credential: %v", err)
				}
			}
		})
	}
	t.Run("mismatched persisted generation", func(t *testing.T) {
		s := openTest(t)
		_, _, b := authTestActor(t, s, now, "a")
		b.AuthGeneration = "restored-old-generation"
		authTestInsert(t, s, "intake_browser_sessions", b.ID, b)
		if err := authTestAuthenticate(t, s, b, "", "demo", now); !errors.Is(err, ErrAuthRequired) {
			t.Fatalf("old generation: %v", err)
		}
	})
}
func authTestPairing(t *testing.T, s *Store, id string, o model.Operator, c model.OperatorCredential, now time.Time) (model.OperatorPairing, model.AuthChallenge) {
	t.Helper()
	ctx := context.Background()
	p := model.OperatorPairing{ID: id, SecretHash: authTestHash("secret-" + id), OperatorID: o.ID, ProjectScope: o.ProjectScope, MaxCredentials: 10, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	if err := s.CreatePairing(ctx, p); err != nil {
		t.Fatal(err)
	}
	challenge := model.AuthChallenge{ID: "registration-" + id, Purpose: "registration", OperatorID: o.ID, PairingID: id, SessionData: json.RawMessage(`{"opaque":"registration"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := s.CreateAuthChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	return p, challenge
}
func TestIntakeAuthPendingRegistrationFingerprintAtomic(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o := model.Operator{ID: "pair-operator", Label: "Pair operator", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("operator-handle")}
	if err := s.CreateOperator(ctx, o); err != nil {
		t.Fatal(err)
	}
	c := authTestCredential(o, now)
	p, challenge := authTestPairing(t, s, "pair", o, c, now)
	if err := s.RegisterPendingCredential(ctx, p.ID, authTestHash("wrong"), challenge.ID, c); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := s.RegisterPendingCredential(ctx, p.ID, p.SecretHash, challenge.ID, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("pending credential activated: %v", err)
	}
	if err := s.RegisterPendingCredential(ctx, p.ID, p.SecretHash, challenge.ID, c); !errors.Is(err, ErrPairingState) {
		t.Fatalf("registration replay: %v", err)
	}
	if _, err := s.ConfirmPairing(ctx, p.ID, authTestHash("wrong fingerprint")); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("wrong fingerprint: %v", err)
	}
	rejected, err := s.GetPairing(ctx, p.ID)
	if err != nil || rejected.State != "revoked" || rejected.PendingCredential != nil {
		t.Fatalf("fingerprint mismatch did not revoke pending pairing: %+v %v", rejected, err)
	}
	if _, err = s.ConfirmPairing(ctx, p.ID, c.Fingerprint); !errors.Is(err, ErrPairingState) {
		t.Fatalf("correct fingerprint reused revoked pairing: %v", err)
	}
	if _, err = s.GetCredential(ctx, c.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("mismatched pairing activated credential: %v", err)
	}
	p, challenge = authTestPairing(t, s, "fresh-pair", o, c, now)
	if err = s.RegisterPendingCredential(ctx, p.ID, p.SecretHash, challenge.ID, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmPairing(ctx, p.ID, c.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetCredential(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmPairing(ctx, p.ID, c.Fingerprint); !errors.Is(err, ErrPairingState) {
		t.Fatalf("confirmation replay: %v", err)
	}
	if _, err = s.GetPairingBySecretHash(ctx, p.SecretHash); !errors.Is(err, ErrPairingState) {
		t.Fatalf("consumed pairing secret: %v", err)
	}
}
func TestIntakeAuthConfirmationInsertFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, existing, _ := authTestActor(t, s, now, "existing")
	c := authTestCredential(o, now)
	c.ID = "new-credential"
	p, challenge := authTestPairing(t, s, "pair", o, c, now)
	if err := s.RegisterPendingCredential(ctx, p.ID, p.SecretHash, challenge.ID, c); err != nil {
		t.Fatal(err)
	}
	// Force a constraint failure after registration but before confirmation.
	existing.ID = c.ID
	data, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?)`, existing.ID, existing.OperatorID, data); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmPairing(ctx, p.ID, c.Fingerprint); err == nil {
		t.Fatal("duplicate credential confirmation succeeded")
	}
	got, err := s.GetPairing(ctx, p.ID)
	if err != nil || got.State != "registered" {
		t.Fatalf("failed insert consumed pairing: %+v %v", got, err)
	}
}
func TestIntakeAuthPairingExactExpiryAndScope(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, _, _ := authTestActor(t, s, now, "a")
	c := authTestCredential(o, now)
	p, _ := authTestPairing(t, s, "pair", o, c, now)
	p, err := s.GetPairing(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = intakeAuthPairingLive(ctx, tx, p, p.ExpiresAt); !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("pairing exact expiry: %v", err)
	}
	tx.Rollback()
	o.ProjectScope = []string{"other"}
	authTestInsert(t, s, "intake_operators", o.ID, o)
	if _, err = s.GetPairingBySecretHash(ctx, p.SecretHash); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("scope changed pairing: %v", err)
	}
	if intakeAuthScopeEqual([]string{"a", "a"}, []string{"a", "b"}) {
		t.Fatal("duplicate pairing scope matched expanded operator scope")
	}
}
func TestIntakeAuthLoginAtomicConsumeCounterSession(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, c, b := authTestActor(t, s, now, "a")
	challenge := authTestLoginChallenge(t, s, "login", o, c, now)
	c.SignCount = 1
	c.CredentialData = json.RawMessage(`{"opaque":"updated-library-credential"}`)
	// Token uniqueness forces session insert failure; credential/challenge updates roll back.
	duplicate := b
	duplicate.ID = "duplicate-session"
	if err := s.CompleteLogin(ctx, challenge.ID, c, duplicate); err == nil {
		t.Fatal("duplicate session token accepted")
	}
	stored, err := s.GetCredential(ctx, c.ID)
	if err != nil || stored.SignCount != 0 {
		t.Fatalf("counter escaped rollback: %+v %v", stored, err)
	}
	saved, err := s.GetAuthChallenge(ctx, challenge.ID)
	if err != nil || saved.ConsumedAt != nil {
		t.Fatalf("challenge escaped rollback: %+v %v", saved, err)
	}
	duplicate.TokenHash = authTestHash("new-token")
	if err = s.CompleteLogin(ctx, challenge.ID, c, duplicate); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteLogin(ctx, challenge.ID, c, duplicate); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("login replay: %v", err)
	}
	stored, err = s.GetCredential(ctx, c.ID)
	if err != nil || stored.SignCount != 1 || string(stored.CredentialData) != string(c.CredentialData) {
		t.Fatalf("library state not persisted: %+v %v", stored, err)
	}
}
func TestIntakeAuthLoginCrossOperatorAndChallengeExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, c, _ := authTestActor(t, s, now, "a")
	_, other, b := authTestActor(t, s, now, "b")
	challenge := authTestLoginChallenge(t, s, "login", o, c, now)
	if err := s.CompleteLogin(ctx, challenge.ID, other, b); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("cross operator login: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = intakeAuthChallengeLive(ctx, tx, challenge.ID, "login", challenge.ExpiresAt); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("challenge exact expiry: %v", err)
	}
}
func authTestReview(t *testing.T, s *Store, o model.Operator, c model.OperatorCredential, b model.BrowserSession, now time.Time) (model.IntakeReview, model.AuthChallenge, string) {
	t.Helper()
	ctx := context.Background()
	command := model.TypedCommand{CommandID: "00000000-0000-4000-8000-000000000001", Action: model.ActionFeatureApprove, Payload: json.RawMessage(`{"kind":"feature","id":"feature-one","revision":"` + authTestHash("revision") + `"}`)}
	digest, err := intake.CommandDigest(command)
	if err != nil {
		t.Fatal(err)
	}
	claims := model.ReviewClaims{Version: 1, InstallationID: "installation", OperatorID: o.ID, CredentialID: c.ID, Action: command.Action, SubjectKind: "feature", SubjectID: "feature-one", SubjectRevision: authTestHash("revision"), CanonicalPayloadSHA: digest, ReviewManifestSHA: authTestHash("manifest"), PolicySHA: authTestHash("policy"), CommandID: command.CommandID, ChallengeID: "command-challenge", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	sha, err := intakeReviewSHA(claims)
	if err != nil {
		t.Fatal(err)
	}
	r := model.IntakeReview{ID: "review", Claims: claims, ClaimsSHA: sha, Command: command, CreatedAt: now}
	if err = s.SaveIntakeReview(ctx, r); err != nil {
		t.Fatal(err)
	}
	challenge := model.AuthChallenge{ID: claims.ChallengeID, Purpose: "command", OperatorID: o.ID, CredentialID: c.ID, SessionID: b.ID, CommandID: command.CommandID, CommandDigest: digest, ClaimDigest: sha, ReviewID: r.ID, SessionData: json.RawMessage(`{"opaque":"command-session"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err = s.CreateAuthChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	return r, challenge, digest
}
func TestIntakeAuthCommandConsumeApprovalAndRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, c, b := authTestActor(t, s, now, "a")
	review, challenge, digest := authTestReview(t, s, o, c, b, now)
	c.SignCount = 1
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = intakeConsumeCommandChallengeTx(ctx, tx, challenge.ID, b.ID, authTestHash("other-command"), c, now); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("wrong command digest: %v", err)
	}
	tx.Rollback()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = intakeConsumeCommandChallengeTx(ctx, tx, challenge.ID, b.ID, digest, c, now); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	saved, err := s.GetAuthChallenge(ctx, challenge.ID)
	if err != nil || saved.ConsumedAt != nil {
		t.Fatalf("rollback consumed command: %+v %v", saved, err)
	}
	receipt := model.ApprovalReceipt{ID: "approval", ReviewID: review.ID, Claims: review.Claims, ClaimsSHA: review.ClaimsSHA, Audit: model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: o.ID, CredentialID: c.ID, CommandID: review.Claims.CommandID, ApprovalID: "approval"}, Evidence: model.WebAuthnEvidence{ChallengeID: challenge.ID, CredentialID: c.ID, UserPresent: true, UserVerified: true, AssertionSHA: authTestHash("assertion"), SignCount: 1, VerifiedAt: now}, CreatedAt: now}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = intakeConsumeCommandChallengeTx(ctx, tx, challenge.ID, b.ID, digest, c, now); err != nil {
		t.Fatal(err)
	}
	if err = intakeSaveApprovalTx(ctx, tx, receipt); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = intakeConsumeCommandChallengeTx(ctx, tx, challenge.ID, b.ID, digest, c, now); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("command replay: %v", err)
	}
	if err = intakeSaveApprovalTx(ctx, tx, receipt); err == nil {
		t.Fatal("approval receipt overwritten")
	}
	tx.Rollback()
	got, err := s.GetApprovalReceipt(ctx, receipt.ID)
	if err != nil || got.ClaimsSHA != review.ClaimsSHA {
		t.Fatalf("immutable receipt missing: %+v %v", got, err)
	}
}
func TestIntakeAuthReviewCrossActorAndExactExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, c, b := authTestActor(t, s, now, "a")
	review, _, digest := authTestReview(t, s, o, c, b, now)
	other, otherCredential, _ := authTestActor(t, s, now, "b")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = intakeVerifyReviewTx(ctx, tx, review.ID, other.ID, otherCredential.ID, digest, now); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("cross actor review: %v", err)
	}
	if _, err = intakeVerifyReviewTx(ctx, tx, review.ID, o.ID, c.ID, digest, review.Claims.ExpiresAt); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("exact review expiry: %v", err)
	}
}
func TestIntakeAuthZeroCounterAndTouchAbsoluteCap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	s := openTest(t)
	_, c, b := authTestActor(t, s, now, "a")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = intakeAuthSaveCredential(ctx, tx, c, now); err != nil {
		t.Fatalf("valid zero counter rejected: %v", err)
	}
	b.AbsoluteExpiresAt = now.Add(90 * time.Minute)
	if err = intakeTouchSessionTx(ctx, tx, b, now.Add(45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var got model.BrowserSession
	if err = intakeAuthLoad(ctx, tx, "intake_browser_sessions", b.ID, &got); err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(b.AbsoluteExpiresAt) {
		t.Fatalf("touch extended absolute expiry: %v", got.ExpiresAt)
	}
}
func TestIntakeAuthRateLimitBounded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	key := authTestHash("ip+ceremony")
	for i := range 3 {
		ok, err := s.TakeAuthRateLimit(ctx, key, 3, 24*time.Hour)
		if err != nil || !ok {
			t.Fatalf("attempt %d: %v %v", i, ok, err)
		}
	}
	if ok, err := s.TakeAuthRateLimit(ctx, key, 3, 24*time.Hour); err != nil || ok {
		t.Fatalf("rate limit: %v %v", ok, err)
	}
	if _, err := s.TakeAuthRateLimit(ctx, "unbounded-client-key", 3, time.Minute); err == nil {
		t.Fatal("unhashed rate key accepted")
	}
}

func TestIntakeAuthCounterRejectsStaleZeroSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	s := openTest(t)
	_, credential, _ := authTestActor(t, s, now, "a")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	latest := credential
	latest.SignCount = 5
	if err = intakeAuthSaveCredential(ctx, tx, latest, now); err != nil {
		t.Fatal(err)
	}
	if err = intakeAuthSaveCredential(ctx, tx, credential, now); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("stale zero overwrote established counter: %v", err)
	}
	latest.SignCount = 4
	if err = intakeAuthSaveCredential(ctx, tx, latest, now); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("counter rollback accepted: %v", err)
	}
	var stored model.OperatorCredential
	if err = intakeAuthLoad(ctx, tx, "intake_credentials", credential.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.SignCount != 5 {
		t.Fatalf("counter changed after rejection: %d", stored.SignCount)
	}
}

func TestIntakeAuthConfirmationEnforcesCapturedCredentialCapacity(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, _, _ := authTestActor(t, s, now, "a")
	c := authTestCredential(o, now)
	c.ID = "pending-new-device"
	p, challenge := authTestPairing(t, s, "capacity-pair", o, c, now)
	if err := s.RegisterPendingCredential(ctx, p.ID, p.SecretHash, challenge.ID, c); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPairing(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.MaxCredentials = 1
	authTestInsert(t, s, "intake_pairings", p.ID, p)
	if _, err = s.ConfirmPairing(ctx, p.ID, c.Fingerprint); !errors.Is(err, ErrCredentialLimit) {
		t.Fatalf("credential capacity bypassed: %v", err)
	}
	got, err := s.GetPairing(ctx, p.ID)
	if err != nil || got.State != "registered" {
		t.Fatalf("capacity denial consumed pairing: %+v %v", got, err)
	}
	if err = s.RevokeCredential(ctx, "credential-"+o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmPairing(ctx, p.ID, c.Fingerprint); err != nil {
		t.Fatalf("revoked credential counted as active: %v", err)
	}
}

func TestIntakeAuthRestoreRevokesKeysRequiresFreshPairingAndPreservesHistory(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	o, oldCredential, oldSession := authTestActor(t, s, now, "restore")
	installation, err := s.InstallationID(ctx)
	if err != nil || installation == "" {
		t.Fatalf("installation: %q %v", installation, err)
	}
	previousGeneration, err := s.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	review, challenge, digest := authTestReview(t, s, o, oldCredential, oldSession, now)
	updated := oldCredential
	updated.SignCount = 1
	receipt := model.ApprovalReceipt{ID: "historical-approval", ReviewID: review.ID, Claims: review.Claims, ClaimsSHA: review.ClaimsSHA, Audit: model.AuditContext{Kind: model.AuditVerifiedOperator, OperatorID: o.ID, CredentialID: oldCredential.ID, CommandID: review.Claims.CommandID, ApprovalID: "historical-approval"}, Evidence: model.WebAuthnEvidence{ChallengeID: challenge.ID, CredentialID: oldCredential.ID, UserPresent: true, UserVerified: true, AssertionSHA: authTestHash("historical-assertion"), SignCount: 1, VerifiedAt: now}, CreatedAt: now}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = intakeConsumeCommandChallengeTx(ctx, tx, challenge.ID, oldSession.ID, digest, updated, now); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = intakeSaveApprovalTx(ctx, tx, receipt); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// An expired key in a restored backup is still permanently revoked by reset.
	expired := oldCredential
	expired.ID = "expired-restored-key"
	expired.ExpiresAt = now.Add(-time.Hour)
	data, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?)`, expired.ID, o.ID, data); err != nil {
		t.Fatal(err)
	}
	newGeneration, err := s.RotateAuthGeneration(ctx)
	if err != nil || newGeneration == previousGeneration {
		t.Fatalf("restore generation: %q %v", newGeneration, err)
	}
	restored, err := s.GetCredential(ctx, oldCredential.ID)
	if err != nil || restored.RevokedAt == nil || restored.SignCount != 1 || restored.Fingerprint != oldCredential.Fingerprint {
		t.Fatalf("old key not retained revoked: %+v %v", restored, err)
	}
	expired, err = s.GetCredential(ctx, expired.ID)
	if err != nil || expired.RevokedAt == nil {
		t.Fatalf("expired restored key escaped reset: %+v %v", expired, err)
	}
	currentOperator, err := s.GetOperator(ctx, o.ID)
	if err != nil || !currentOperator.Enabled || currentOperator.RevokedAt != nil {
		t.Fatalf("reset disabled enrolled operator: %+v %v", currentOperator, err)
	}
	if got, err := s.InstallationID(ctx); err != nil || got != installation {
		t.Fatalf("reset changed installation: %q %v", got, err)
	}
	if got, err := s.GetIntakeReview(ctx, review.ID); err != nil || got.ClaimsSHA != review.ClaimsSHA {
		t.Fatalf("review history lost: %+v %v", got, err)
	}
	if got, err := s.GetApprovalReceipt(ctx, receipt.ID); err != nil || got.ClaimsSHA != receipt.ClaimsSHA || got.Evidence.AssertionSHA != receipt.Evidence.AssertionSHA {
		t.Fatalf("approval history lost: %+v %v", got, err)
	}
	// A newly-issued challenge/session in the current epoch cannot reactivate an old key.
	login := model.AuthChallenge{ID: "after-restore-login", Purpose: "login", OperatorID: o.ID, CredentialID: oldCredential.ID, SessionData: json.RawMessage(`{"opaque":"new-login"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err = s.CreateAuthChallenge(ctx, login); err != nil {
		t.Fatal(err)
	}
	session := oldSession
	session.ID = "after-restore-session"
	session.TokenHash = authTestHash("after-restore-token")
	session.AuthGeneration = newGeneration
	if err = s.CompleteLogin(ctx, login.ID, updated, session); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("restored old key freshly logged in: %v", err)
	}
	if err = s.CreateBrowserSession(ctx, session); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("restored old key created new session: %v", err)
	}
	// Recovery succeeds only through a new CLI-authorized pairing and fresh login.
	fresh := authTestCredential(o, now)
	fresh.ID = "fresh-recovery-key"
	fresh.PublicKey = []byte("fresh-public-key")
	fresh.Fingerprint = authTestHash("fresh-recovery-fingerprint")
	pairing, registration := authTestPairing(t, s, "restore-recovery", o, fresh, now)
	if err = s.RegisterPendingCredential(ctx, pairing.ID, pairing.SecretHash, registration.ID, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmPairing(ctx, pairing.ID, fresh.Fingerprint); err != nil {
		t.Fatal(err)
	}
	freshLogin := model.AuthChallenge{ID: "fresh-recovery-login", Purpose: "login", OperatorID: o.ID, CredentialID: fresh.ID, SessionData: json.RawMessage(`{"opaque":"fresh-login"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err = s.CreateAuthChallenge(ctx, freshLogin); err != nil {
		t.Fatal(err)
	}
	session.CredentialID = fresh.ID
	if err = s.CompleteLogin(ctx, freshLogin.ID, fresh, session); err != nil {
		t.Fatalf("fresh paired key could not login: %v", err)
	}
	if _, _, _, err = s.BrowserSessionByTokenHash(ctx, session.TokenHash); err != nil {
		t.Fatalf("fresh recovery session: %v", err)
	}
}

func TestIntakeAuthOrdinaryRestartPreservesRegistrationAndEpoch(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	_, credential, session := authTestActor(t, s, now, "restart")
	generation, err := s.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := s.InstallationID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.AuthGeneration(ctx); err != nil || got != generation {
		t.Fatalf("ordinary restart rotated auth epoch: %q %v", got, err)
	}
	if got, err := s.InstallationID(ctx); err != nil || got != installation {
		t.Fatalf("ordinary restart changed installation: %q %v", got, err)
	}
	if got, err := s.GetCredential(ctx, credential.ID); err != nil || got.RevokedAt != nil {
		t.Fatalf("ordinary restart revoked registration: %+v %v", got, err)
	}
	if _, _, _, err = s.BrowserSessionByTokenHash(ctx, session.TokenHash); err != nil {
		t.Fatalf("ordinary restart lost valid session: %v", err)
	}
}

func authTestSeedChallenge(t *testing.T, s *Store, challenge model.AuthChallenge) {
	t.Helper()
	data, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(context.Background(), `INSERT INTO intake_auth_challenges(id,operator_id,credential_id,session_id,data) VALUES(?,?,?,?,?)`, challenge.ID, challenge.OperatorID, challenge.CredentialID, challenge.SessionID, data); err != nil {
		t.Fatal(err)
	}
}

func TestIntakeAuthChallengeCleanupPreservesLiveAndAuthorityHistory(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 1, 1, 12, 0, 2, 0, time.UTC)
	s := openTest(t)
	generation, err := s.AuthGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	consumed := now.Add(-time.Minute)
	cases := []struct {
		id, purpose string
		expires     time.Time
		consumed    *time.Time
		purged      bool
	}{
		{"expired-login-whole", "login", now.Add(-2 * time.Second), nil, true},
		{"expired-login-fractional", "login", now.Add(-1900 * time.Millisecond), nil, true},
		{"expired-registration", "registration", now.Add(-2 * time.Second), nil, true},
		{"expired-offset", "login", now.Add(-2 * time.Second).In(time.FixedZone("offset", 3600)), nil, true},
		{"active-whole", "login", now.Add(time.Second), nil, false},
		{"active-nanosecond", "login", now.Add(time.Nanosecond), nil, false},
		{"active-registration", "registration", now.Add(time.Minute), nil, false},
		{"exact-expiry", "login", now, nil, false},
		{"recent-expiry", "registration", now.Add(-time.Nanosecond), nil, false},
		{"cleanup-grace-boundary", "login", now.Add(-time.Second), nil, false},
		{"consumed-login", "login", now.Add(-time.Hour), &consumed, false},
		{"consumed-registration", "registration", now.Add(-time.Hour), &consumed, false},
		{"unused-command", "command", now.Add(-time.Hour), nil, false},
		{"consumed-command", "command", now.Add(-time.Hour), &consumed, false},
		{"unknown-purpose", "other", now.Add(-time.Hour), nil, false},
	}
	for _, tc := range cases {
		authTestSeedChallenge(t, s, model.AuthChallenge{ID: tc.id, Purpose: tc.purpose, SessionData: json.RawMessage(`{"opaque":"preserved-options"}`), AuthGeneration: generation, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: tc.expires, ConsumedAt: tc.consumed})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = intakeAuthCleanupChallenges(ctx, tx, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		var got model.AuthChallenge
		err := intakeAuthLoad(ctx, tx, "intake_auth_challenges", tc.id, &got)
		if tc.purged {
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("%s: expired unused ceremony retained: %v", tc.id, err)
			}
			continue
		}
		if err != nil || got.ID != tc.id || got.Purpose != tc.purpose || !got.ExpiresAt.Equal(tc.expires) || (got.ConsumedAt == nil) != (tc.consumed == nil) || string(got.SessionData) != `{"opaque":"preserved-options"}` {
			t.Fatalf("%s: challenge changed: %+v %v", tc.id, got, err)
		}
	}
	if _, err = intakeAuthChallengeLive(ctx, tx, "active-nanosecond", "login", now); err != nil {
		t.Fatalf("nanosecond-live options lost: %v", err)
	}
	if _, err = intakeAuthChallengeLive(ctx, tx, "exact-expiry", "login", now); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("retained exact expiry: %v", err)
	}
	if _, err = intakeAuthChallengeLive(ctx, tx, "consumed-login", "login", now); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("consumed history lost: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestIntakeAuthChallengeCleanupBatchBoundAndIndex(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 1, 1, 12, 0, 0, 123456789, time.UTC)
	s := openTest(t)
	for i := range intakeAuthChallengeCleanupBatch + 5 {
		purpose := "login"
		if i%2 == 0 {
			purpose = "registration"
		}
		authTestSeedChallenge(t, s, model.AuthChallenge{ID: fmt.Sprintf("expired-%03d", i), Purpose: purpose, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-2 * time.Second)})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM intake_auth_challenges
		WHERE json_extract(data,'$.purpose') IN ('login','registration')
			AND json_extract(data,'$.consumed_at') IS NULL
			AND julianday(json_extract(data,'$.expires_at')) < julianday(?)
		ORDER BY julianday(json_extract(data,'$.expires_at')) LIMIT ?`, now.Add(-time.Second).Format(time.RFC3339Nano), intakeAuthChallengeCleanupBatch)
	if err != nil {
		t.Fatal(err)
	}
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH") && strings.Contains(detail, "intake_auth_challenges_unused_ceremony_expiry") {
			indexed = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("cleanup expiry predicate did not search the partial time index")
	}
	for _, remaining := range []int{5, 0} {
		if err = intakeAuthCleanupChallenges(ctx, tx, now); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM intake_auth_challenges`).Scan(&count); err != nil || count != remaining {
			t.Fatalf("cleanup batch: got %d remaining, want %d: %v", count, remaining, err)
		}
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_auth_challenges`).Scan(&count); err != nil || count != intakeAuthChallengeCleanupBatch+5 {
		t.Fatalf("cleanup escaped transaction rollback: %d %v", count, err)
	}
}

func TestIntakeAuthChallengeCreationCleansExpiredWithoutChangingRetryOptions(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := openTest(t)
	expired := model.AuthChallenge{ID: "expired", Purpose: "login", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	authTestSeedChallenge(t, s, expired)
	active := model.AuthChallenge{ID: "retry-options", Purpose: "login", SessionData: json.RawMessage(`{"opaque":"same-options"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	authTestSeedChallenge(t, s, active)
	if err := s.CreateAuthChallenge(ctx, active); err == nil {
		t.Fatal("duplicate options challenge unexpectedly accepted")
	}
	if _, err := s.GetAuthChallenge(ctx, expired.ID); err != nil {
		t.Fatalf("failed creation cleaned up challenges: %v", err)
	}
	fresh := active
	fresh.ID = "fresh-options"
	if err := s.CreateAuthChallenge(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAuthChallenge(ctx, expired.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("successful creation did not purge expired ceremony: %v", err)
	}
	got, err := s.GetAuthChallenge(ctx, active.ID)
	if err != nil || !got.ExpiresAt.Equal(active.ExpiresAt) || string(got.SessionData) != string(active.SessionData) || got.ConsumedAt != nil {
		t.Fatalf("active retry options changed: %+v %v", got, err)
	}
	if _, err = s.GetAuthChallenge(ctx, fresh.ID); err != nil {
		t.Fatalf("new options lost: %v", err)
	}
}
