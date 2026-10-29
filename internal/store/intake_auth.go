package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

const intakeAuthChallengeCleanupIndex = `CREATE INDEX IF NOT EXISTS intake_auth_challenges_unused_ceremony_expiry ON intake_auth_challenges(julianday(json_extract(data,'$.expires_at'))) WHERE json_extract(data,'$.purpose') IN ('login','registration') AND json_extract(data,'$.consumed_at') IS NULL;`

const intakeAuthSchema = `
CREATE TABLE IF NOT EXISTS intake_operators(id TEXT PRIMARY KEY,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS intake_credentials(id TEXT PRIMARY KEY,operator_id TEXT NOT NULL REFERENCES intake_operators(id),data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS intake_credentials_operator ON intake_credentials(operator_id);
CREATE TABLE IF NOT EXISTS intake_pairings(id TEXT PRIMARY KEY,secret_hash TEXT UNIQUE NOT NULL,operator_id TEXT NOT NULL REFERENCES intake_operators(id),data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS intake_browser_sessions(id TEXT PRIMARY KEY,token_hash TEXT UNIQUE NOT NULL,operator_id TEXT NOT NULL REFERENCES intake_operators(id),credential_id TEXT NOT NULL REFERENCES intake_credentials(id),data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS intake_browser_sessions_operator ON intake_browser_sessions(operator_id,credential_id);
CREATE TABLE IF NOT EXISTS intake_auth_challenges(id TEXT PRIMARY KEY,operator_id TEXT NOT NULL,credential_id TEXT NOT NULL,session_id TEXT NOT NULL,data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS intake_auth_challenges_actor ON intake_auth_challenges(operator_id,credential_id,session_id);
` + intakeAuthChallengeCleanupIndex + `
CREATE TABLE IF NOT EXISTS intake_reviews(id TEXT PRIMARY KEY,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS intake_approval_receipts(id TEXT PRIMARY KEY,review_id TEXT UNIQUE NOT NULL REFERENCES intake_reviews(id),challenge_id TEXT UNIQUE NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS intake_auth_rate_limits(key TEXT PRIMARY KEY,window_start INTEGER NOT NULL,count INTEGER NOT NULL);
INSERT OR IGNORE INTO settings(key,value) VALUES('intake_auth_generation',lower(hex(randomblob(32))));
`

var (
	ErrAuthRequired      = errors.New("auth_required")
	ErrCredentialRevoked = errors.New("credential_revoked")
	ErrCredentialLimit   = errors.New("credential_limit")
	ErrProjectForbidden  = errors.New("project_forbidden")
	ErrChallengeExpired  = errors.New("challenge_expired")
	ErrChallengeConsumed = errors.New("challenge_consumed")
	ErrPairingExpired    = errors.New("pairing_expired")
	ErrPairingState      = errors.New("pairing_state")
	ErrCSRF              = errors.New("csrf_failed")
)

type intakeAuthQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Table names are fixed internal literals, never caller-controlled identifiers.
func intakeAuthLoad(ctx context.Context, q intakeAuthQuery, table, id string, out any) error {
	var b []byte
	if err := q.QueryRowContext(ctx, `SELECT data FROM `+table+` WHERE id=?`, id).Scan(&b); err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func intakeAuthUpdate(ctx context.Context, tx *sql.Tx, table, id string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `UPDATE `+table+` SET data=? WHERE id=?`, b, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}
func intakeAuthGeneration(ctx context.Context, q intakeAuthQuery) (string, error) {
	var g string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='intake_auth_generation'`).Scan(&g)
	return g, err
}
func (s *Store) AuthGeneration(ctx context.Context) (string, error) {
	return intakeAuthGeneration(ctx, s.db)
}
func (s *Store) InstallationID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='installation_id'`).Scan(&id)
	return id, err
}
func (s *Store) RotateAuthGeneration(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	generation := ID() + ID()
	if _, err = tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key='intake_auth_generation'`, generation); err != nil {
		return "", err
	}
	// A restored backup may predate a key revocation. Revoke every existing key,
	// including expired keys, while retaining immutable registration/audit history.
	if _, err = tx.ExecContext(ctx, `UPDATE intake_credentials SET data=json_set(data,'$.revoked_at',?) WHERE json_extract(data,'$.revoked_at') IS NULL`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	// Operators remain enrolled, but only fresh trusted pairing can grant new keys.
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_browser_sessions; DELETE FROM intake_auth_challenges; DELETE FROM intake_pairings; DELETE FROM intake_auth_rate_limits;`); err != nil {
		return "", err
	}
	return generation, tx.Commit()
}
func intakeAuthGenerationMatches(ctx context.Context, tx *sql.Tx, g string) error {
	live, err := intakeAuthGeneration(ctx, tx)
	if err != nil {
		return err
	}
	if g == "" || g != live {
		return ErrAuthRequired
	}
	return nil
}
func intakeAuthLiveOperator(ctx context.Context, tx *sql.Tx, id, project string) (model.Operator, error) {
	var o model.Operator
	if err := intakeAuthLoad(ctx, tx, "intake_operators", id, &o); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return o, ErrAuthRequired
		}
		return o, err
	}
	if !o.Enabled || o.RevokedAt != nil {
		return o, ErrAuthRequired
	}
	if project != "" {
		for _, p := range o.ProjectScope {
			if p == project {
				return o, nil
			}
		}
		return o, ErrProjectForbidden
	}
	return o, nil
}
func intakeAuthLiveCredential(ctx context.Context, tx *sql.Tx, id, operatorID string, now time.Time) (model.OperatorCredential, error) {
	var c model.OperatorCredential
	if err := intakeAuthLoad(ctx, tx, "intake_credentials", id, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c, ErrCredentialRevoked
		}
		return c, err
	}
	if c.OperatorID != operatorID || c.RevokedAt != nil || !now.Before(c.ExpiresAt) {
		return c, ErrCredentialRevoked
	}
	return c, nil
}
func (s *Store) CreateOperator(ctx context.Context, o model.Operator) error {
	if o.ID == "" || o.Label == "" || len(o.UserHandle) == 0 || len(o.UserHandle) > 64 || len(o.ProjectScope) == 0 || o.RevokedAt != nil {
		return errors.New("invalid operator")
	}
	seen := map[string]bool{}
	for _, p := range o.ProjectScope {
		if p == "" || seen[p] {
			return errors.New("invalid project scope")
		}
		seen[p] = true
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO intake_operators(id,data) VALUES(?,?)`, o.ID, b)
	return err
}
func (s *Store) GetOperator(ctx context.Context, id string) (model.Operator, error) {
	var o model.Operator
	err := intakeAuthLoad(ctx, s.db, "intake_operators", id, &o)
	return o, err
}
func intakeAuthList[T any](ctx context.Context, db *sql.DB, query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var b []byte
		var v T
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) ListOperators(ctx context.Context) ([]model.Operator, error) {
	return intakeAuthList[model.Operator](ctx, s.db, `SELECT data FROM intake_operators ORDER BY id`)
}
func (s *Store) GetCredential(ctx context.Context, id string) (model.OperatorCredential, error) {
	var c model.OperatorCredential
	err := intakeAuthLoad(ctx, s.db, "intake_credentials", id, &c)
	return c, err
}
func (s *Store) ListOperatorCredentials(ctx context.Context, id string) ([]model.OperatorCredential, error) {
	return intakeAuthList[model.OperatorCredential](ctx, s.db, `SELECT data FROM intake_credentials WHERE operator_id=? ORDER BY id`, id)
}
func (s *Store) RevokeOperator(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var o model.Operator
	if err = intakeAuthLoad(ctx, tx, "intake_operators", id, &o); err != nil {
		return err
	}
	now := time.Now().UTC()
	o.Enabled = false
	if o.RevokedAt == nil {
		o.RevokedAt = &now
	}
	if err = intakeAuthUpdate(ctx, tx, "intake_operators", id, o); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_browser_sessions WHERE operator_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_auth_challenges WHERE operator_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_pairings WHERE operator_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) RevokeCredential(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var c model.OperatorCredential
	if err = intakeAuthLoad(ctx, tx, "intake_credentials", id, &c); err != nil {
		return err
	}
	now := time.Now().UTC()
	if c.RevokedAt == nil {
		c.RevokedAt = &now
	}
	if err = intakeAuthUpdate(ctx, tx, "intake_credentials", id, c); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_auth_challenges WHERE credential_id=? OR session_id IN (SELECT id FROM intake_browser_sessions WHERE credential_id=?)`, id, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_browser_sessions WHERE credential_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func intakeAuthHashValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}
func intakeAuthSame(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func intakeAuthScopeEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	scopes := make(map[string]bool, len(a))
	for _, scope := range a {
		if scope == "" || scopes[scope] {
			return false
		}
		scopes[scope] = true
	}
	for _, scope := range b {
		if !scopes[scope] {
			return false
		}
		delete(scopes, scope)
	}
	return len(scopes) == 0
}
func (s *Store) CreatePairing(ctx context.Context, p model.OperatorPairing) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.ID == "" || p.MaxCredentials < 1 || !intakeAuthHashValid(p.SecretHash) || !p.ExpiresAt.After(now) || p.ExpiresAt.After(p.CreatedAt.Add(10*time.Minute)) || p.CreatedAt.After(now) || p.PendingCredential != nil || (p.State != "" && p.State != "issued") {
		return errors.New("invalid pairing")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := intakeAuthLiveOperator(ctx, tx, p.OperatorID, "")
	if err != nil {
		return err
	}
	// Pairing scope is exact, never a browser-selected expansion or reduction.
	if !intakeAuthScopeEqual(p.ProjectScope, o.ProjectScope) {
		return ErrProjectForbidden
	}
	if p.AuthGeneration == "" {
		p.AuthGeneration, err = intakeAuthGeneration(ctx, tx)
	}
	if err != nil {
		return err
	}
	if err = intakeAuthGenerationMatches(ctx, tx, p.AuthGeneration); err != nil {
		return err
	}
	p.State = "issued"
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_pairings(id,secret_hash,operator_id,data) VALUES(?,?,?,?)`, p.ID, p.SecretHash, p.OperatorID, b); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) GetPairing(ctx context.Context, id string) (model.OperatorPairing, error) {
	var p model.OperatorPairing
	err := intakeAuthLoad(ctx, s.db, "intake_pairings", id, &p)
	return p, err
}
func (s *Store) GetPairingBySecretHash(ctx context.Context, hash string) (model.OperatorPairing, error) {
	var p model.OperatorPairing
	var b []byte
	if !intakeAuthHashValid(hash) {
		return p, ErrAuthRequired
	}
	err := s.db.QueryRowContext(ctx, `SELECT data FROM intake_pairings WHERE secret_hash=?`, hash).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	if err != nil {
		return p, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	err = intakeAuthPairingLive(ctx, tx, p, time.Now().UTC())
	return p, err
}
func intakeAuthPairingLive(ctx context.Context, tx *sql.Tx, p model.OperatorPairing, now time.Time) error {
	if !now.Before(p.ExpiresAt) {
		return ErrPairingExpired
	}
	if p.State != "issued" && p.State != "registered" {
		return ErrPairingState
	}
	if err := intakeAuthGenerationMatches(ctx, tx, p.AuthGeneration); err != nil {
		return err
	}
	o, err := intakeAuthLiveOperator(ctx, tx, p.OperatorID, "")
	if err != nil {
		return err
	}
	if !intakeAuthScopeEqual(p.ProjectScope, o.ProjectScope) {
		return ErrProjectForbidden
	}
	return nil
}
func intakeAuthCredentialShape(c model.OperatorCredential, o model.Operator, now time.Time) error {
	if c.ID == "" || c.OperatorID != o.ID || !bytes.Equal(c.UserHandle, o.UserHandle) || len(c.PublicKey) == 0 || len(c.CredentialData) == 0 || !json.Valid(c.CredentialData) || c.Fingerprint == "" || !c.ExpiresAt.After(now) || c.CreatedAt.After(now) || c.RevokedAt != nil {
		return ErrCredentialRevoked
	}
	return nil
}
func (s *Store) RegisterPendingCredential(ctx context.Context, pairingID, secretSHA, challengeID string, c model.OperatorCredential) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var p model.OperatorPairing
	if err = intakeAuthLoad(ctx, tx, "intake_pairings", pairingID, &p); err != nil {
		return err
	}
	if !intakeAuthSame(p.SecretHash, secretSHA) {
		return ErrAuthRequired
	}
	if err = intakeAuthPairingLive(ctx, tx, p, now); err != nil {
		return err
	}
	if p.State != "issued" || p.PendingCredential != nil {
		return ErrPairingState
	}
	challenge, err := intakeAuthChallengeLive(ctx, tx, challengeID, "registration", now)
	if err != nil {
		return err
	}
	if challenge.PairingID != p.ID || challenge.OperatorID != p.OperatorID {
		return ErrAuthRequired
	}
	o, err := intakeAuthLiveOperator(ctx, tx, p.OperatorID, "")
	if err != nil {
		return err
	}
	if err = intakeAuthCredentialShape(c, o, now); err != nil {
		return err
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intake_credentials WHERE id=?)`, c.ID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrPairingState
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	p.PendingCredential = &c
	p.State = "registered"
	challenge.ConsumedAt = &now
	if err = intakeAuthUpdate(ctx, tx, "intake_auth_challenges", challenge.ID, challenge); err != nil {
		return err
	}
	if err = intakeAuthUpdate(ctx, tx, "intake_pairings", p.ID, p); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ConfirmPairing(ctx context.Context, id, fingerprint string) (model.OperatorCredential, error) {
	var c model.OperatorCredential
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var p model.OperatorPairing
	now := time.Now().UTC()
	if err = intakeAuthLoad(ctx, tx, "intake_pairings", id, &p); err != nil {
		return c, err
	}
	if err = intakeAuthPairingLive(ctx, tx, p, now); err != nil {
		return c, err
	}
	if p.State != "registered" || p.PendingCredential == nil {
		return c, ErrPairingState
	}
	c = *p.PendingCredential
	if !intakeAuthSame(c.Fingerprint, fingerprint) {
		p.State = "revoked"
		p.PendingCredential = nil
		if err = intakeAuthUpdate(ctx, tx, "intake_pairings", p.ID, p); err != nil {
			return model.OperatorCredential{}, err
		}
		if err = tx.Commit(); err != nil {
			return model.OperatorCredential{}, err
		}
		return model.OperatorCredential{}, ErrAuthRequired
	}
	o, err := intakeAuthLiveOperator(ctx, tx, p.OperatorID, "")
	if err != nil {
		return c, err
	}
	if err = intakeAuthCredentialShape(c, o, now); err != nil {
		return c, err
	}
	if err = intakeAuthCredentialCapacityTx(ctx, tx, p.OperatorID, p.MaxCredentials, now); err != nil {
		return c, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?)`, c.ID, c.OperatorID, b); err != nil {
		return c, err
	}
	p.State = "consumed"
	if err = intakeAuthUpdate(ctx, tx, "intake_pairings", p.ID, p); err != nil {
		return c, err
	}
	return c, tx.Commit()
}

const intakeAuthChallengeCleanupBatch = 128

// Unused ceremony challenges have bounded, opportunistic retention: each successful
// challenge creation removes at most one batch. Retained expired ceremonies still
// report ErrChallengeExpired; a purged ceremony instead reports sql.ErrNoRows.
// The numeric date index handles fractional UTC timestamps (and UTC offsets),
// unlike RFC3339Nano lexical ordering. SQLite rounds sub-millisecond fractions,
// so a one-second grace prevents cleanup from touching any active ceremony.
// Consumed ceremonies and all command challenge authority history are excluded.
func intakeAuthCleanupChallenges(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM intake_auth_challenges WHERE id IN (
		SELECT id FROM intake_auth_challenges
		WHERE json_extract(data,'$.purpose') IN ('login','registration')
			AND json_extract(data,'$.consumed_at') IS NULL
			AND julianday(json_extract(data,'$.expires_at')) < julianday(?)
		ORDER BY julianday(json_extract(data,'$.expires_at'))
		LIMIT ?
	)`, now.Add(-time.Second).UTC().Format(time.RFC3339Nano), intakeAuthChallengeCleanupBatch)
	return err
}
func (s *Store) CreateAuthChallenge(ctx context.Context, c model.AuthChallenge) error {
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.ID == "" || len(c.SessionData) == 0 || !json.Valid(c.SessionData) || !c.ExpiresAt.After(now) || c.ExpiresAt.After(c.CreatedAt.Add(2*time.Minute)) || c.CreatedAt.After(now) || c.ConsumedAt != nil {
		return errors.New("invalid challenge")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if c.AuthGeneration == "" {
		c.AuthGeneration, err = intakeAuthGeneration(ctx, tx)
	}
	if err != nil {
		return err
	}
	if err = intakeAuthGenerationMatches(ctx, tx, c.AuthGeneration); err != nil {
		return err
	}
	switch c.Purpose {
	case "registration":
		var p model.OperatorPairing
		if err = intakeAuthLoad(ctx, tx, "intake_pairings", c.PairingID, &p); err != nil {
			return err
		}
		if err = intakeAuthPairingLive(ctx, tx, p, now); err != nil {
			return err
		}
		if p.State != "issued" || c.OperatorID != p.OperatorID {
			return ErrPairingState
		}
	case "login":
		if c.OperatorID != "" {
			if _, err = intakeAuthLiveOperator(ctx, tx, c.OperatorID, ""); err != nil {
				return err
			}
		}
	case "command":
		var session model.BrowserSession
		if err = intakeAuthLoad(ctx, tx, "intake_browser_sessions", c.SessionID, &session); err != nil {
			return err
		}
		_, o, credential, err := intakeAuthenticateTx(ctx, tx, session.TokenHash, "", "", now)
		if err != nil {
			return err
		}
		if c.OperatorID != o.ID || c.CredentialID != credential.ID || c.CommandID == "" || c.CommandDigest == "" || c.ReviewID == "" {
			return ErrAuthRequired
		}
		review, err := intakeVerifyReviewTx(ctx, tx, c.ReviewID, o.ID, credential.ID, c.CommandDigest, now)
		if err != nil {
			return err
		}
		if review.Claims.ChallengeID != c.ID || review.Claims.CommandID != c.CommandID || !intakeAuthSame(review.ClaimsSHA, c.ClaimDigest) {
			return ErrAuthRequired
		}
	default:
		return errors.New("invalid challenge purpose")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_auth_challenges(id,operator_id,credential_id,session_id,data) VALUES(?,?,?,?,?)`, c.ID, c.OperatorID, c.CredentialID, c.SessionID, b); err != nil {
		return err
	}
	if err = intakeAuthCleanupChallenges(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) GetAuthChallenge(ctx context.Context, id string) (model.AuthChallenge, error) {
	var c model.AuthChallenge
	err := intakeAuthLoad(ctx, s.db, "intake_auth_challenges", id, &c)
	return c, err
}
func intakeAuthChallengeLive(ctx context.Context, tx *sql.Tx, id, purpose string, now time.Time) (model.AuthChallenge, error) {
	var c model.AuthChallenge
	if err := intakeAuthLoad(ctx, tx, "intake_auth_challenges", id, &c); err != nil {
		return c, err
	}
	if c.ConsumedAt != nil {
		return c, ErrChallengeConsumed
	}
	if !now.Before(c.ExpiresAt) {
		return c, ErrChallengeExpired
	}
	if c.Purpose != purpose {
		return c, ErrAuthRequired
	}
	return c, intakeAuthGenerationMatches(ctx, tx, c.AuthGeneration)
}
func intakeAuthSaveCredential(ctx context.Context, tx *sql.Tx, c model.OperatorCredential, now time.Time) error {
	old, err := intakeAuthLiveCredential(ctx, tx, c.ID, c.OperatorID, now)
	if err != nil {
		return err
	}
	// Never let an assertion replace identity, expiry, keys, or revocation metadata.
	if !bytes.Equal(c.UserHandle, old.UserHandle) || !bytes.Equal(c.PublicKey, old.PublicKey) || c.Fingerprint != old.Fingerprint || c.BackupEligible != old.BackupEligible || !c.ExpiresAt.Equal(old.ExpiresAt) || !c.CreatedAt.Equal(old.CreatedAt) || c.RevokedAt != nil || !json.Valid(c.CredentialData) {
		return ErrCredentialRevoked
	}
	// The verifier owns signature/counter semantics, including valid zero counters.
	// A result verified against an older snapshot, including zero, cannot overwrite
	// an established nonzero counter. Zero/zero authenticators remain supported.
	if old.SignCount != 0 && c.SignCount <= old.SignCount {
		return ErrChallengeConsumed
	}
	c.LastUsedAt = now
	return intakeAuthUpdate(ctx, tx, "intake_credentials", c.ID, c)
}
func (s *Store) CompleteLogin(ctx context.Context, challengeID string, c model.OperatorCredential, session model.BrowserSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	challenge, err := intakeAuthChallengeLive(ctx, tx, challengeID, "login", now)
	if err != nil {
		return err
	}
	if (challenge.OperatorID != "" && challenge.OperatorID != c.OperatorID) || (challenge.CredentialID != "" && challenge.CredentialID != c.ID) || session.OperatorID != c.OperatorID || session.CredentialID != c.ID {
		return ErrAuthRequired
	}
	if _, err = intakeAuthLiveOperator(ctx, tx, c.OperatorID, ""); err != nil {
		return err
	}
	if err = intakeAuthSaveCredential(ctx, tx, c, now); err != nil {
		return err
	}
	if err = intakeCreateSessionTx(ctx, tx, session, now); err != nil {
		return err
	}
	challenge.ConsumedAt = &now
	if err = intakeAuthUpdate(ctx, tx, "intake_auth_challenges", challenge.ID, challenge); err != nil {
		return err
	}
	return tx.Commit()
}
func intakeCreateSessionTx(ctx context.Context, tx *sql.Tx, b model.BrowserSession, now time.Time) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	if b.LastUsedAt.IsZero() {
		b.LastUsedAt = now
	}
	if b.ID == "" || !intakeAuthHashValid(b.TokenHash) || !intakeAuthHashValid(b.CSRFHash) || b.CreatedAt.After(now) || b.LastUsedAt.After(now) || b.LastUsedAt.Before(b.CreatedAt) || !b.ExpiresAt.After(now) || b.ExpiresAt.After(b.LastUsedAt.Add(12*time.Hour)) || !b.AbsoluteExpiresAt.After(now) || b.AbsoluteExpiresAt.After(b.CreatedAt.Add(7*24*time.Hour)) || b.ExpiresAt.After(b.AbsoluteExpiresAt) || b.RevokedAt != nil {
		return errors.New("invalid browser session")
	}
	if _, err := intakeAuthLiveOperator(ctx, tx, b.OperatorID, ""); err != nil {
		return err
	}
	if _, err := intakeAuthLiveCredential(ctx, tx, b.CredentialID, b.OperatorID, now); err != nil {
		return err
	}
	var err error
	if b.AuthGeneration == "" {
		b.AuthGeneration, err = intakeAuthGeneration(ctx, tx)
	}
	if err != nil {
		return err
	}
	if err = intakeAuthGenerationMatches(ctx, tx, b.AuthGeneration); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_browser_sessions(id,token_hash,operator_id,credential_id,data) VALUES(?,?,?,?,?)`, b.ID, b.TokenHash, b.OperatorID, b.CredentialID, data)
	return err
}
func (s *Store) CreateBrowserSession(ctx context.Context, b model.BrowserSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = intakeCreateSessionTx(ctx, tx, b, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
func intakeAuthenticateTx(ctx context.Context, tx *sql.Tx, tokenSHA, csrfSHA, project string, now time.Time) (model.BrowserSession, model.Operator, model.OperatorCredential, error) {
	var b model.BrowserSession
	var o model.Operator
	var c model.OperatorCredential
	var data []byte
	if !intakeAuthHashValid(tokenSHA) {
		return b, o, c, ErrAuthRequired
	}
	err := tx.QueryRowContext(ctx, `SELECT data FROM intake_browser_sessions WHERE token_hash=?`, tokenSHA).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return b, o, c, ErrAuthRequired
	}
	if err != nil {
		return b, o, c, err
	}
	if err = json.Unmarshal(data, &b); err != nil {
		return b, o, c, err
	}
	if b.RevokedAt != nil || !now.Before(b.ExpiresAt) || !now.Before(b.AbsoluteExpiresAt) {
		return b, o, c, ErrAuthRequired
	}
	if err = intakeAuthGenerationMatches(ctx, tx, b.AuthGeneration); err != nil {
		return b, o, c, err
	}
	if csrfSHA != "" && !intakeAuthSame(csrfSHA, b.CSRFHash) {
		return b, o, c, ErrCSRF
	}
	o, err = intakeAuthLiveOperator(ctx, tx, b.OperatorID, project)
	if err != nil {
		return b, o, c, err
	}
	c, err = intakeAuthLiveCredential(ctx, tx, b.CredentialID, o.ID, now)
	return b, o, c, err
}
func intakeTouchSessionTx(ctx context.Context, tx *sql.Tx, b model.BrowserSession, now time.Time) error {
	idle := b.ExpiresAt.Sub(b.LastUsedAt)
	if idle <= 0 || idle > 12*time.Hour {
		return ErrAuthRequired
	}
	b.LastUsedAt = now
	b.ExpiresAt = now.Add(idle)
	if b.ExpiresAt.After(b.AbsoluteExpiresAt) {
		b.ExpiresAt = b.AbsoluteExpiresAt
	}
	return intakeAuthUpdate(ctx, tx, "intake_browser_sessions", b.ID, b)
}
func (s *Store) BrowserSessionByTokenHash(ctx context.Context, sha string) (model.BrowserSession, model.Operator, model.OperatorCredential, error) {
	var b model.BrowserSession
	var o model.Operator
	var c model.OperatorCredential
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return b, o, c, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	b, o, c, err = intakeAuthenticateTx(ctx, tx, sha, "", "", now)
	if err != nil {
		return b, o, c, err
	}
	if err = intakeTouchSessionTx(ctx, tx, b, now); err != nil {
		return b, o, c, err
	}
	if err = intakeAuthLoad(ctx, tx, "intake_browser_sessions", b.ID, &b); err != nil {
		return b, o, c, err
	}
	return b, o, c, tx.Commit()
}
func (s *Store) TouchBrowserSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var b model.BrowserSession
	if err = intakeAuthLoad(ctx, tx, "intake_browser_sessions", id, &b); err != nil {
		return err
	}
	now := time.Now().UTC()
	if b, _, _, err = intakeAuthenticateTx(ctx, tx, b.TokenHash, "", "", now); err != nil {
		return err
	}
	if err = intakeTouchSessionTx(ctx, tx, b, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) RevokeBrowserSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var b model.BrowserSession
	if err = intakeAuthLoad(ctx, tx, "intake_browser_sessions", id, &b); err != nil {
		return err
	}
	now := time.Now().UTC()
	if b.RevokedAt == nil {
		b.RevokedAt = &now
	}
	if err = intakeAuthUpdate(ctx, tx, "intake_browser_sessions", id, b); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_auth_challenges WHERE session_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func intakeConsumeCommandChallengeTx(ctx context.Context, tx *sql.Tx, challengeID, sessionID, commandDigest string, updated model.OperatorCredential, now time.Time) (model.AuthChallenge, error) {
	c, err := intakeAuthChallengeLive(ctx, tx, challengeID, "command", now)
	if err != nil {
		return c, err
	}
	if c.SessionID != sessionID || c.CommandDigest != commandDigest || c.OperatorID != updated.OperatorID || c.CredentialID != updated.ID {
		return c, ErrAuthRequired
	}
	var b model.BrowserSession
	if err = intakeAuthLoad(ctx, tx, "intake_browser_sessions", sessionID, &b); err != nil {
		return c, err
	}
	if _, _, _, err = intakeAuthenticateTx(ctx, tx, b.TokenHash, "", "", now); err != nil {
		return c, err
	}
	if b.OperatorID != c.OperatorID || b.CredentialID != c.CredentialID {
		return c, ErrAuthRequired
	}
	review, err := intakeVerifyReviewTx(ctx, tx, c.ReviewID, c.OperatorID, c.CredentialID, commandDigest, now)
	if err != nil {
		return c, err
	}
	if review.Claims.ChallengeID != c.ID || review.Claims.CommandID != c.CommandID || !intakeAuthSame(review.ClaimsSHA, c.ClaimDigest) {
		return c, ErrAuthRequired
	}
	if err = intakeAuthSaveCredential(ctx, tx, updated, now); err != nil {
		return c, err
	}
	c.ConsumedAt = &now
	return c, intakeAuthUpdate(ctx, tx, "intake_auth_challenges", c.ID, c)
}
func intakeReviewSHA(claims model.ReviewClaims) (string, error) {
	b, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func (s *Store) SaveIntakeReview(ctx context.Context, r model.IntakeReview) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = intakeSaveReviewTx(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit()
}
func intakeSaveReviewTx(ctx context.Context, tx *sql.Tx, r model.IntakeReview) error {
	now := time.Now().UTC()
	if r.ID == "" || r.Claims.Version != 1 || r.Claims.InstallationID == "" || r.Claims.CommandID == "" || r.Claims.ChallengeID == "" || !now.Before(r.Claims.ExpiresAt) || r.Claims.ExpiresAt.After(r.Claims.IssuedAt.Add(2*time.Minute)) || r.Claims.IssuedAt.After(now) {
		return errors.New("invalid review")
	}
	if _, err := intakeAuthLiveOperator(ctx, tx, r.Claims.OperatorID, ""); err != nil {
		return err
	}
	if _, err := intakeAuthLiveCredential(ctx, tx, r.Claims.CredentialID, r.Claims.OperatorID, now); err != nil {
		return err
	}
	sha, err := intakeReviewSHA(r.Claims)
	if err != nil {
		return err
	}
	if !intakeAuthSame(sha, r.ClaimsSHA) || r.Command.CommandID != r.Claims.CommandID || r.Command.Action != r.Claims.Action {
		return errors.New("invalid review binding")
	}
	if err = validateIntakeReviewMaterial(r); err != nil {
		return err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_reviews(id,data) VALUES(?,?)`, r.ID, b)
	return err
}
func (s *Store) GetIntakeReview(ctx context.Context, id string) (model.IntakeReview, error) {
	var r model.IntakeReview
	err := intakeAuthLoad(ctx, s.db, "intake_reviews", id, &r)
	return r, err
}
func intakeVerifyReviewTx(ctx context.Context, tx *sql.Tx, id, operatorID, credentialID, digest string, now time.Time) (model.IntakeReview, error) {
	var r model.IntakeReview
	if err := intakeAuthLoad(ctx, tx, "intake_reviews", id, &r); err != nil {
		return r, err
	}
	if r.Claims.OperatorID != operatorID || r.Claims.CredentialID != credentialID {
		return r, ErrAuthRequired
	}
	if !now.Before(r.Claims.ExpiresAt) {
		return r, ErrChallengeExpired
	}
	if _, err := intakeAuthLiveOperator(ctx, tx, operatorID, ""); err != nil {
		return r, err
	}
	if _, err := intakeAuthLiveCredential(ctx, tx, credentialID, operatorID, now); err != nil {
		return r, err
	}
	sha, err := intakeReviewSHA(r.Claims)
	if err != nil {
		return r, err
	}
	if !intakeAuthSame(sha, r.ClaimsSHA) {
		return r, ErrAuthRequired
	}
	if err = validateIntakeReviewMaterial(r); err != nil {
		return r, err
	}
	actual, err := intake.CommandDigest(r.Command)
	if err != nil {
		return r, err
	}
	if !intakeAuthSame(actual, digest) {
		return r, ErrAuthRequired
	}
	return r, nil
}
func intakeSaveApprovalTx(ctx context.Context, tx *sql.Tx, r model.ApprovalReceipt) error {
	if r.ID == "" || r.Audit.Kind != model.AuditVerifiedOperator || r.Audit.ApprovalID != r.ID || r.Audit.OperatorID != r.Claims.OperatorID || r.Audit.CredentialID != r.Claims.CredentialID || r.Audit.CommandID != r.Claims.CommandID || !r.Evidence.UserPresent || !r.Evidence.UserVerified || r.Evidence.CredentialID != r.Claims.CredentialID || r.Evidence.ChallengeID != r.Claims.ChallengeID || !intakeAuthHashValid(r.Evidence.AssertionSHA) {
		return ErrAuthRequired
	}
	var review model.IntakeReview
	if err := intakeAuthLoad(ctx, tx, "intake_reviews", r.ReviewID, &review); err != nil {
		return err
	}
	sha, err := intakeReviewSHA(r.Claims)
	if err != nil {
		return err
	}
	if !intakeAuthSame(sha, r.ClaimsSHA) || !intakeAuthSame(review.ClaimsSHA, r.ClaimsSHA) {
		return ErrAuthRequired
	}
	var challenge model.AuthChallenge
	if err = intakeAuthLoad(ctx, tx, "intake_auth_challenges", r.Claims.ChallengeID, &challenge); err != nil {
		return err
	}
	if challenge.ConsumedAt == nil || challenge.ReviewID != r.ReviewID || challenge.CredentialID != r.Claims.CredentialID || challenge.OperatorID != r.Claims.OperatorID || challenge.CommandID != r.Claims.CommandID || challenge.ClaimDigest != r.ClaimsSHA {
		return ErrAuthRequired
	}
	if r.Evidence.VerifiedAt.Before(challenge.CreatedAt) || !r.Evidence.VerifiedAt.Before(challenge.ExpiresAt) || r.Evidence.VerifiedAt.After(*challenge.ConsumedAt) {
		return ErrAuthRequired
	}
	credential, err := intakeAuthLiveCredential(ctx, tx, r.Claims.CredentialID, r.Claims.OperatorID, *challenge.ConsumedAt)
	if err != nil {
		return err
	}
	if r.Evidence.SignCount != credential.SignCount || !credential.LastUsedAt.Equal(*challenge.ConsumedAt) {
		return ErrAuthRequired
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = *challenge.ConsumedAt
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_approval_receipts(id,review_id,challenge_id,data) VALUES(?,?,?,?)`, r.ID, r.ReviewID, r.Claims.ChallengeID, b)
	return err
}
func (s *Store) GetApprovalReceipt(ctx context.Context, id string) (model.ApprovalReceipt, error) {
	var r model.ApprovalReceipt
	err := intakeAuthLoad(ctx, s.db, "intake_approval_receipts", id, &r)
	return r, err
}
func (s *Store) TakeAuthRateLimit(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if !intakeAuthHashValid(key) || limit < 1 || window < time.Second || window > 24*time.Hour {
		return false, fmt.Errorf("invalid auth rate limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	seconds := int64(window / time.Second)
	start := now - now%seconds
	if _, err = tx.ExecContext(ctx, `DELETE FROM intake_auth_rate_limits WHERE window_start<?`, now-86400); err != nil {
		return false, err
	}
	var count int
	var oldStart int64
	err = tx.QueryRowContext(ctx, `SELECT window_start,count FROM intake_auth_rate_limits WHERE key=?`, key).Scan(&oldStart, &count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if oldStart != start {
		count = 0
	}
	if count >= limit {
		return false, nil
	}
	var total int
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM intake_auth_rate_limits`).Scan(&total); err != nil {
			return false, err
		}
		if total >= 10000 {
			return false, nil
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_auth_rate_limits(key,window_start,count) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET window_start=excluded.window_start,count=excluded.count`, key, start, count+1); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) RevokePairing(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var p model.OperatorPairing
	if err = intakeAuthLoad(ctx, tx, "intake_pairings", id, &p); err != nil {
		return err
	}
	if p.State == "consumed" {
		return ErrPairingState
	}
	p.State = "revoked"
	p.PendingCredential = nil
	if err = intakeAuthUpdate(ctx, tx, "intake_pairings", id, p); err != nil {
		return err
	}
	// Registration challenge rows do not have a pairing index; their immutable JSON
	// binding is checked again by RegisterPendingCredential under this transaction.
	return tx.Commit()
}

func intakeAuthCredentialCapacityTx(ctx context.Context, tx *sql.Tx, operatorID string, limit int, now time.Time) error {
	if limit < 1 {
		return ErrCredentialLimit
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM intake_credentials WHERE operator_id=?`, operatorID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var data []byte
		var credential model.OperatorCredential
		if err = rows.Scan(&data); err != nil {
			return err
		}
		if err = json.Unmarshal(data, &credential); err != nil {
			return err
		}
		if credential.RevokedAt == nil && now.Before(credential.ExpiresAt) {
			count++
			if count >= limit {
				return ErrCredentialLimit
			}
		}
	}
	return rows.Err()
}
