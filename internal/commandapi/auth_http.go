package commandapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/operatorauth"
	"openfactory/internal/store"
)

type ceremonyOptions struct {
	ChallengeID   string          `json:"challenge_id"`
	PublicKey     json.RawMessage `json:"public_key"`
	ReviewID      string          `json:"review_id,omitempty"`
	ClaimsSHA     string          `json:"claims_sha,omitempty"`
	CommandDigest string          `json:"command_digest,omitempty"`
}

func challengeOptions(c model.AuthChallenge) ceremonyOptions {
	options := ceremonyOptions{ChallengeID: c.ID, PublicKey: c.OptionsData}
	if c.Purpose == "command" {
		options.ReviewID = c.ReviewID
		options.ClaimsSHA = c.ClaimDigest
		options.CommandDigest = c.CommandDigest
	}
	return options
}

type registrationOptionsRequest struct {
	PairingSecret string `json:"pairing_secret"`
}
type registrationFinishRequest struct {
	PairingSecret string          `json:"pairing_secret"`
	ChallengeID   string          `json:"challenge_id"`
	Credential    json.RawMessage `json:"credential"`
}
type loginFinishRequest struct {
	ChallengeID string          `json:"challenge_id"`
	Credential  json.RawMessage `json:"credential"`
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, notFound())
		return
	}
	if err := s.rate(r); err != nil {
		writeError(w, apiFailure(err))
		return
	}
	var err error
	switch r.URL.Path {
	case "/v1/auth/register/options":
		err = s.registerOptions(w, r)
	case "/v1/auth/register/finish":
		err = s.registerFinish(w, r)
	case "/v1/auth/login/options":
		err = s.loginOptions(w, r)
	case "/v1/auth/login/finish":
		err = s.loginFinish(w, r)
	default:
		err = notFound()
	}
	if err != nil {
		writeError(w, apiFailure(err))
	}
}
func (s *Server) challenge(r *http.Request, id, purpose string) (model.AuthChallenge, error) {
	c, err := s.db.GetAuthChallenge(r.Context(), id)
	if err != nil {
		if isMissing(err) || errors.Is(err, store.ErrNotFound) {
			return c, store.ErrAuthRequired
		}
		return c, err
	}
	if c.Purpose != purpose {
		return c, store.ErrAuthRequired
	}
	if c.ConsumedAt != nil {
		return c, store.ErrChallengeConsumed
	}
	if !time.Now().UTC().Before(c.ExpiresAt) {
		return c, store.ErrChallengeExpired
	}
	generation, err := s.db.AuthGeneration(r.Context())
	if err != nil {
		return c, err
	}
	if c.AuthGeneration != generation {
		return c, store.ErrAuthRequired
	}
	return c, nil
}
func (s *Server) saveChallenge(r *http.Request, c model.AuthChallenge, options any, session json.RawMessage) (ceremonyOptions, error) {
	raw, err := json.Marshal(options)
	if err != nil {
		return ceremonyOptions{}, err
	}
	c.OptionsData = raw
	c.SessionData = session
	if c.ID == "" {
		c.ID = store.ID()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = now.Add(time.Duration(s.cfg.Intake.ChallengeTTLSeconds) * time.Second)
	}
	if err = s.db.CreateAuthChallenge(r.Context(), c); err != nil {
		if c.Purpose == "command" {
			existing, e := s.challenge(r, c.ID, "command")
			if e == nil && existing.OperatorID == c.OperatorID && existing.CredentialID == c.CredentialID && existing.SessionID == c.SessionID && existing.ReviewID == c.ReviewID && existing.ClaimDigest == c.ClaimDigest && existing.CommandDigest == c.CommandDigest && len(existing.OptionsData) > 0 {
				return challengeOptions(existing), nil
			}
			if e == nil && existing.OperatorID == c.OperatorID && existing.CredentialID == c.CredentialID && existing.ReviewID == c.ReviewID && existing.ClaimDigest == c.ClaimDigest && existing.CommandDigest == c.CommandDigest && existing.SessionID != c.SessionID {
				return ceremonyOptions{}, apiError{Status: 409, Code: "stale_subject", Message: "This review is bound to another session. Review the command again."}
			}
		}
		return ceremonyOptions{}, err
	}
	return challengeOptions(c), nil
}
func (s *Server) pairingUser(r *http.Request, secret string) (model.OperatorPairing, operatorauth.User, error) {
	if !validSecret(secret) {
		return model.OperatorPairing{}, operatorauth.User{}, store.ErrAuthRequired
	}
	pair, err := s.db.GetPairingBySecretHash(r.Context(), sha256Hex([]byte(secret)))
	if err != nil {
		return pair, operatorauth.User{}, err
	}
	if pair.State != "issued" {
		return pair, operatorauth.User{}, store.ErrPairingState
	}
	op, err := s.db.GetOperator(r.Context(), pair.OperatorID)
	if err != nil {
		if isMissing(err) || errors.Is(err, store.ErrNotFound) {
			return pair, operatorauth.User{}, store.ErrAuthRequired
		}
		return pair, operatorauth.User{}, err
	}
	records, err := s.db.ListOperatorCredentials(r.Context(), op.ID)
	if err != nil {
		return pair, operatorauth.User{}, err
	}
	active := 0
	now := time.Now().UTC()
	for _, c := range records {
		if c.RevokedAt == nil && now.Before(c.ExpiresAt) {
			active++
		}
	}
	if active >= s.cfg.Intake.MaxCredentials {
		return pair, operatorauth.User{}, apiError{Status: 400, Code: "invalid_contract", Message: "The operator credential limit has been reached."}
	}
	u, err := operatorauth.LoadUser(op, records, now)
	return pair, u, err
}
func (s *Server) registerOptions(w http.ResponseWriter, r *http.Request) error {
	var req registrationOptionsRequest
	if err := decodeJSON(w, r, &req, 2048); err != nil {
		return err
	}
	pair, u, err := s.pairingUser(r, req.PairingSecret)
	if err != nil {
		return err
	}
	options, session, err := s.verifier.BeginRegistration(u)
	if err != nil {
		return err
	}
	out, err := s.saveChallenge(r, model.AuthChallenge{Purpose: "registration", OperatorID: u.Operator.ID, PairingID: pair.ID}, options, session)
	if err != nil {
		return err
	}
	writeJSON(w, 200, out)
	return nil
}
func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) error {
	var req registrationFinishRequest
	if err := decodeJSON(w, r, &req, 128<<10); err != nil {
		return err
	}
	pair, u, err := s.pairingUser(r, req.PairingSecret)
	if err != nil {
		return err
	}
	c, err := s.challenge(r, req.ChallengeID, "registration")
	if err != nil {
		return err
	}
	if c.PairingID != pair.ID || c.OperatorID != u.Operator.ID {
		return store.ErrAuthRequired
	}
	credential, err := s.verifier.FinishRegistration(u, c.SessionData, req.Credential, time.Now().UTC())
	if err != nil {
		return err
	}
	if err = s.db.RegisterPendingCredential(r.Context(), pair.ID, sha256Hex([]byte(req.PairingSecret)), c.ID, credential); err != nil {
		return err
	}
	// Registration does not mint a session. Only exact CLI fingerprint
	// confirmation activates the credential, then a fresh login proves possession.
	writeJSON(w, 200, struct {
		PairingID    string `json:"pairing_id"`
		CredentialID string `json:"credential_id"`
		Fingerprint  string `json:"fingerprint"`
		Pending      bool   `json:"pending_confirmation"`
	}{pair.ID, credential.ID, credential.Fingerprint, true})
	return nil
}
func (s *Server) loginOptions(w http.ResponseWriter, r *http.Request) error {
	var req struct{}
	if err := decodeJSON(w, r, &req, 1024); err != nil {
		return err
	}
	options, session, err := s.verifier.BeginLogin()
	if err != nil {
		return err
	}
	out, err := s.saveChallenge(r, model.AuthChallenge{Purpose: "login"}, options, session)
	if err != nil {
		return err
	}
	writeJSON(w, 200, out)
	return nil
}
func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) error {
	var req loginFinishRequest
	if err := decodeJSON(w, r, &req, 128<<10); err != nil {
		return err
	}
	challenge, err := s.challenge(r, req.ChallengeID, "login")
	if err != nil {
		return err
	}
	credential, err := s.verifier.FinishLogin(challenge.SessionData, req.Credential, func(id string) (model.Operator, []model.OperatorCredential, error) {
		c, e := s.db.GetCredential(r.Context(), id)
		if e != nil {
			if isMissing(e) || errors.Is(e, store.ErrNotFound) || errors.Is(e, store.ErrCredentialRevoked) || errors.Is(e, store.ErrAuthRequired) {
				return model.Operator{}, nil, operatorauth.ErrVerification
			}
			return model.Operator{}, nil, e
		}
		op, e := s.db.GetOperator(r.Context(), c.OperatorID)
		if e != nil {
			if isMissing(e) || errors.Is(e, store.ErrNotFound) || errors.Is(e, store.ErrAuthRequired) {
				return op, nil, operatorauth.ErrVerification
			}
			return op, nil, e
		}
		records, e := s.db.ListOperatorCredentials(r.Context(), op.ID)
		return op, records, e
	}, time.Now().UTC())
	if err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	session := model.BrowserSession{ID: store.ID(), TokenHash: sha256Hex([]byte(token)), OperatorID: credential.OperatorID, CredentialID: credential.ID, CSRFHash: sha256Hex([]byte(operatorauth.CSRFToken(token))), CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Duration(s.cfg.Intake.SessionIdleSeconds) * time.Second), AbsoluteExpiresAt: now.Add(time.Duration(s.cfg.Intake.SessionAbsoluteSeconds) * time.Second)}
	if err = s.db.CompleteLogin(r.Context(), challenge.ID, credential, session); err != nil {
		return err
	}
	stored, op, confirmed, err := s.db.BrowserSessionByTokenHash(r.Context(), session.TokenHash)
	if err != nil {
		return err
	}
	view, err := s.sessionView(r, principal{token: token, session: stored, operator: op, credential: confirmed})
	if err != nil {
		return err
	}
	s.setCookie(w, token, stored.AbsoluteExpiresAt)
	writeJSON(w, 200, view)
	return nil
}
func (s *Server) authorizationOptions(w http.ResponseWriter, r *http.Request, p principal) error {
	var req model.AuthorizationOptionsRequest
	if err := decodeJSON(w, r, &req, 2048); err != nil {
		return err
	}
	if err := s.rateAuthorization(r, p); err != nil {
		return err
	}
	review, err := s.db.GetIntakeReview(r.Context(), req.ReviewID)
	if err != nil {
		return err
	}
	if review.Claims.OperatorID != p.operator.ID || review.Claims.CredentialID != p.credential.ID {
		return notFound()
	}
	if !time.Now().UTC().Before(review.Claims.ExpiresAt) {
		return store.ErrChallengeExpired
	}
	if review.Claims.PolicySHA != s.cfg.PolicyHash {
		return apiError{Status: 409, Code: "stale_policy", Message: "The controller policy changed. Review the command again."}
	}
	digest, err := commandDigest(review.Command)
	if err != nil {
		return err
	}
	if existing, e := s.db.GetAuthChallenge(r.Context(), review.Claims.ChallengeID); e == nil {
		if existing.SessionID != p.session.ID {
			return apiError{Status: 409, Code: "stale_subject", Message: "This review is bound to another session. Review the command again."}
		}
		if existing.ReviewID != review.ID || existing.ClaimDigest != review.ClaimsSHA || existing.CommandDigest != digest {
			return store.ErrAuthRequired
		}
		if _, e = s.challenge(r, existing.ID, "command"); e != nil {
			return e
		}
		if len(existing.OptionsData) == 0 {
			return errors.New("missing persisted ceremony options")
		}
		writeJSON(w, 200, challengeOptions(existing))
		return nil
	} else if !isMissing(e) && !errors.Is(e, store.ErrNotFound) {
		return e
	}
	records, err := s.db.ListOperatorCredentials(r.Context(), p.operator.ID)
	if err != nil {
		return err
	}
	u, err := operatorauth.LoadUser(p.operator, records, time.Now().UTC())
	if err != nil {
		return err
	}
	options, session, err := s.verifier.BeginCommand(u, p.credential.ID)
	if err != nil {
		return err
	}
	challenge := model.AuthChallenge{ID: review.Claims.ChallengeID, Purpose: "command", OperatorID: p.operator.ID, CredentialID: p.credential.ID, SessionID: p.session.ID, CommandID: review.Command.CommandID, CommandDigest: digest, ClaimDigest: review.ClaimsSHA, ReviewID: review.ID, ExpiresAt: review.Claims.ExpiresAt}
	out, err := s.saveChallenge(r, challenge, options, session)
	if err != nil {
		return err
	}
	writeJSON(w, 200, out)
	return nil
}
func (s *Server) authorizationCommit(w http.ResponseWriter, r *http.Request, p principal) error {
	var req model.AuthorizationCommitRequest
	if err := decodeJSON(w, r, &req, 128<<10); err != nil {
		return err
	}
	if !validUUID(req.CommandID) || req.ChallengeID == "" || len(req.Assertion) == 0 {
		return invalidJSON()
	}
	// Current access is authenticated before permanent receipt replay. A lost
	// successful response must reconcile even after challenge/revision expiry.
	if receipt, e := s.db.GetCommandReceipt(r.Context(), p.operator.ID, req.CommandID); e == nil {
		if receipt.ApprovalID == "" {
			return apiError{Status: 409, Code: "idempotency_conflict", Message: "The command ID already belongs to a different command."}
		}
		approval, e := s.db.GetApprovalReceipt(r.Context(), receipt.ApprovalID)
		if e != nil {
			return e
		}
		if approval.Audit.OperatorID != p.operator.ID || approval.Evidence.ChallengeID != req.ChallengeID {
			return apiError{Status: 409, Code: "idempotency_conflict", Message: "The command ID already belongs to a different authorization."}
		}
		writeJSON(w, 200, receipt)
		return nil
	} else if !errors.Is(e, store.ErrNotFound) && !isMissing(e) {
		return e
	}
	challenge, err := s.db.GetAuthChallenge(r.Context(), req.ChallengeID)
	if err != nil {
		if isMissing(err) || errors.Is(err, store.ErrNotFound) {
			return notFound()
		}
		return err
	}
	if challenge.Purpose != "command" || challenge.OperatorID != p.operator.ID || challenge.CredentialID != p.credential.ID || challenge.CommandID != req.CommandID {
		return notFound()
	}
	if challenge.SessionID != p.session.ID {
		return apiError{Status: 409, Code: "stale_subject", Message: "This review is bound to another session. Review the command again."}
	}
	review, err := s.db.GetIntakeReview(r.Context(), challenge.ReviewID)
	if err != nil {
		return err
	}
	digest, err := commandDigest(review.Command)
	if err != nil {
		return err
	}
	if _, err = s.challenge(r, challenge.ID, "command"); err != nil {
		return err
	}
	if review.Claims.OperatorID != p.operator.ID || review.Claims.CredentialID != p.credential.ID || review.Command.CommandID != req.CommandID || challenge.ClaimDigest != review.ClaimsSHA || challenge.CommandDigest != digest {
		return store.ErrAuthRequired
	}
	if err = s.rateAuthorization(r, p); err != nil {
		return err
	}
	records, err := s.db.ListOperatorCredentials(r.Context(), p.operator.ID)
	if err != nil {
		return err
	}
	u, err := operatorauth.LoadUser(p.operator, records, time.Now().UTC())
	if err != nil {
		return err
	}
	updated, err := s.verifier.FinishCommand(u, p.credential, challenge.SessionData, req.Assertion, time.Now().UTC())
	if err != nil {
		return err
	}
	proof := &store.CommandAuthorization{ChallengeID: challenge.ID, Credential: updated, Evidence: model.WebAuthnEvidence{ChallengeID: challenge.ID, CredentialID: p.credential.ID, UserPresent: true, UserVerified: true, AssertionSHA: sha256Hex(req.Assertion), SignCount: updated.SignCount, VerifiedAt: time.Now().UTC()}}
	receipt, err := s.db.ExecuteCommand(r.Context(), p.tokenSHA, p.csrfSHA, review.Command, s.cfg, proof)
	if err != nil {
		return err
	}
	writeJSON(w, 200, receipt)
	return nil
}
