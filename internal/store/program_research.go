package store

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/research"
)

const programResearchSchema = `
CREATE TABLE IF NOT EXISTS program_capabilities(token_hash TEXT PRIMARY KEY,attempt_id TEXT NOT NULL,program_id TEXT NOT NULL,revision INTEGER NOT NULL,policy_hash TEXT NOT NULL,deadline TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_research_operations(id TEXT PRIMARY KEY,program_id TEXT NOT NULL,request_id TEXT NOT NULL,request_hash TEXT NOT NULL,state TEXT NOT NULL,reserved_bytes INTEGER NOT NULL,data BLOB NOT NULL,result BLOB,UNIQUE(program_id,request_id));
CREATE TABLE IF NOT EXISTS program_sources(id TEXT PRIMARY KEY,program_id TEXT NOT NULL,revision INTEGER NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_searches(id TEXT PRIMARY KEY,program_id TEXT NOT NULL,revision INTEGER NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_leads(id TEXT PRIMARY KEY,program_id TEXT NOT NULL,revision INTEGER NOT NULL,data BLOB NOT NULL);
`

var _ model.ResearchAuthority = (*Store)(nil)

type researchCapabilityRecord struct {
	Capability model.ResearchCapability `json:"capability"`
	PolicyHash string                   `json:"policy_hash"`
	Policy     model.DiscoveryPolicy    `json:"policy"`
}
type researchOperationRecord struct {
	Authorization model.ResearchAuthorization `json:"authorization"`
	TokenHash     string                      `json:"token_hash"`
}

func researchError(code, message string) error {
	return &model.ProgramError{Code: code, Message: message}
}
func researchHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func researchPositive(value, ceiling int64) int64 {
	if value > 0 && value < ceiling {
		return value
	}
	return ceiling
}
func researchPolicy(p model.DiscoveryPolicy) model.DiscoveryPolicy {
	p = model.NormalizeDiscoveryPolicy(p)
	p.MaxCaptureBytes = researchPositive(p.MaxCaptureBytes, 8<<20)
	p.MaxTextBytes = researchPositive(p.MaxTextBytes, 1<<20)
	p.ResearchTimeoutSeconds = int(researchPositive(int64(p.ResearchTimeoutSeconds), 30))
	p.MaxRedirects = int(researchPositive(int64(p.MaxRedirects), 3))
	return p
}

// IssueResearchCapability binds a secret to the stored attempt, never caller
// supplied association or deadline. Only its digest enters the database.
func (s *Store) IssueResearchCapability(ctx context.Context, a model.Attempt, cfg model.Config) (model.ResearchCapability, error) {
	var zero model.ResearchCapability
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	f, actual, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return zero, err
	}
	if cfg.PolicyHash == "" || cfg.PolicyHash != f.PolicyHash {
		return zero, researchError("policy_changed", "research policy changed")
	}
	if f.DiscoveryStepID == "" {
		return zero, researchError("source_rejected", "attempt is not a discovery step")
	}
	var step model.ProgramStep
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE id=? AND feature_id=?`, f.DiscoveryStepID, f.ID).Scan(&data); err != nil {
		return zero, err
	}
	if err = json.Unmarshal(data, &step); err != nil {
		return zero, err
	}
	if step.ID != f.DiscoveryStepID || step.FeatureID != f.ID || step.Kind == model.StepExperimentRun || step.State == "completed" {
		return zero, researchError("source_rejected", "invalid research step association")
	}
	cap := model.ResearchCapability{ProgramID: step.ProgramID, Revision: step.Revision, StepID: step.ID, AttemptID: actual.ID, Deadline: actual.Deadline}
	rec := researchCapabilityRecord{Capability: cap, PolicyHash: cfg.PolicyHash, Policy: researchPolicy(cfg.Discovery)}
	if _, err = s.researchCapabilityAuthorityTx(ctx, tx, rec); err != nil {
		return zero, err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return zero, err
	}
	token := hex.EncodeToString(secret[:])
	data, err = json.Marshal(rec)
	if err != nil {
		return zero, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO program_capabilities(token_hash,attempt_id,program_id,revision,policy_hash,deadline,data) VALUES(?,?,?,?,?,?,?)`, researchHash(token), cap.AttemptID, cap.ProgramID, cap.Revision, rec.PolicyHash, cap.Deadline.Format(time.RFC3339Nano), data)
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	cap.Token = token
	return cap, nil
}

func researchLoadCapabilityTx(ctx context.Context, tx *sql.Tx, hash string) (researchCapabilityRecord, error) {
	var rec researchCapabilityRecord
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM program_capabilities WHERE token_hash=?`, hash).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, researchError("source_rejected", "invalid research capability")
	}
	if err != nil {
		return rec, err
	}
	err = json.Unmarshal(data, &rec)
	return rec, err
}

func (s *Store) researchCapabilityAuthorityTx(ctx context.Context, tx *sql.Tx, rec researchCapabilityRecord) (model.ProgramRevision, error) {
	cap := rec.Capability
	if cap.Deadline.IsZero() || !time.Now().Before(cap.Deadline) {
		return model.ProgramRevision{}, researchError("source_rejected", "research capability expired")
	}
	actual, revision, policy, active, err := loadAttempt(ctx, tx, cap.AttemptID)
	if err != nil {
		return model.ProgramRevision{}, err
	}
	if active != 1 || actual.State != "running" || !actual.Deadline.Equal(cap.Deadline) || !time.Now().Before(actual.Deadline) {
		return model.ProgramRevision{}, researchError("source_rejected", "research attempt is no longer active")
	}
	f, err := get(ctx, tx, actual.FeatureID)
	if err != nil {
		return model.ProgramRevision{}, err
	}
	if f.State == "cancelling" || f.State == "cancelled" || f.DiscoveryStepID != cap.StepID {
		return model.ProgramRevision{}, researchError("source_rejected", "research attempt cancelled or unassociated")
	}
	if policy != rec.PolicyHash || f.PolicyHash != policy || f.Revision != revision {
		return model.ProgramRevision{}, researchError("policy_changed", "research attempt authority changed")
	}
	var step model.ProgramStep
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE id=? AND feature_id=? AND program_id=? AND revision=?`, cap.StepID, f.ID, cap.ProgramID, cap.Revision).Scan(&data); err != nil {
		return model.ProgramRevision{}, err
	}
	if err = json.Unmarshal(data, &step); err != nil {
		return model.ProgramRevision{}, err
	}
	if step.ID != cap.StepID || step.FeatureID != f.ID || step.ProgramID != cap.ProgramID || step.Revision != cap.Revision || step.Kind == model.StepExperimentRun || step.State == "completed" {
		return model.ProgramRevision{}, researchError("source_rejected", "invalid research step")
	}
	var paused int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='paused' AND value='true'`).Scan(&paused); err != nil {
		return model.ProgramRevision{}, err
	}
	if paused != 0 {
		return model.ProgramRevision{}, researchError("source_rejected", "OpenFactory paused")
	}
	_, rev, err := s.programAuthorityTx(ctx, tx, cap.ProgramID, cap.Revision, rec.PolicyHash)
	if err == nil {
		err = programInvalidTx(ctx, tx, cap.ProgramID, rev.RevisionHash)
	}
	if err == nil {
		err = programInvalidTx(ctx, tx, cap.ProgramID, cap.StepID)
	}
	if err == nil && step.InputManifestSHA != "" {
		err = programInvalidTx(ctx, tx, cap.ProgramID, step.InputManifestSHA)
	}
	return rev, err
}

// AuthorizeResearch durably reserves lifetime counts and the full IO allowance
// before the broker can perform any external operation.
func (s *Store) AuthorizeResearch(ctx context.Context, token string, req model.ResearchOperationRequest) (model.ResearchAuthorization, error) {
	var zero model.ResearchAuthorization
	if len(token) != 64 || req.RequestID == "" || len(req.RequestID) > 256 {
		return zero, researchError("invalid_contract", "invalid research request identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	hash := researchHash(token)
	rec, err := researchLoadCapabilityTx(ctx, tx, hash)
	if err != nil {
		return zero, err
	}
	rev, err := s.researchCapabilityAuthorityTx(ctx, tx, rec)
	if err != nil {
		return zero, err
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	requestHash := researchHash(string(encoded))
	var previousHash, state string
	var data, result []byte
	err = tx.QueryRowContext(ctx, `SELECT request_hash,state,data,result FROM program_research_operations WHERE program_id=? AND request_id=?`, rec.Capability.ProgramID, req.RequestID).Scan(&previousHash, &state, &data, &result)
	if err == nil {
		if previousHash != requestHash {
			return zero, researchError("idempotency_conflict", "request ID already names a different operation")
		}
		if state != "completed" {
			return zero, researchError("operation_uncertain", "research operation is already pending; do not repeat IO")
		}
		var old researchOperationRecord
		if err = json.Unmarshal(data, &old); err != nil {
			return zero, err
		}
		if old.Authorization.Revision != rec.Capability.Revision {
			return zero, researchError("stale_revision", "research operation belongs to an older revision")
		}
		var replay model.ResearchOperationResult
		if err = json.Unmarshal(result, &replay); err != nil {
			return zero, err
		}
		old.Authorization.Replay = &replay
		return old.Authorization, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	cap := rec.Capability
	p := rec.Policy
	auth := model.ResearchAuthorization{ID: ID(), ProgramID: cap.ProgramID, Revision: cap.Revision, StepID: cap.StepID, AttemptID: cap.AttemptID, Request: req, MaxCaptureBytes: p.MaxCaptureBytes, MaxTextBytes: p.MaxTextBytes, MaxRedirects: p.MaxRedirects, TimeoutSeconds: p.ResearchTimeoutSeconds, FreshnessSeconds: p.SourceFreshnessSeconds}
	if err = researchResolveRequestTx(ctx, tx, rev, req, &auth); err != nil {
		return zero, err
	}
	budget, err := getProgramBudgetTx(ctx, tx, cap.ProgramID)
	if err != nil {
		return zero, err
	}
	searches := researchPositive(int64(rev.Spec.Limits.MaxSearches), int64(p.Limits.MaxSearches))
	sources := researchPositive(int64(rev.Spec.Limits.MaxSources), int64(p.Limits.MaxSources))
	total := researchPositive(rev.Spec.Limits.MaxTotalCaptureBytes, p.Limits.MaxTotalCaptureBytes)
	var reserved int64
	if req.Kind == "search" {
		if int64(budget.Searches) >= searches {
			return zero, researchError("budget_exhausted", "program lifetime search limit reached")
		}
		budget.Searches++
	} else {
		if int64(budget.Sources) >= sources {
			return zero, researchError("budget_exhausted", "program lifetime source limit reached")
		}
		remaining := total - budget.CaptureBytes - budget.ReservedCaptureBytes
		// A one-byte oversize probe is itself observed IO and must be reserved.
		if remaining <= 1 {
			return zero, researchError("budget_exhausted", "program lifetime capture byte limit reached")
		}
		auth.MaxCaptureBytes = researchPositive(auth.MaxCaptureBytes, remaining-1)
		reserved = auth.MaxCaptureBytes + 1
		budget.Sources++
		budget.ReservedCaptureBytes += reserved
	}
	if err = putProgramBudgetTx(ctx, tx, cap.ProgramID, budget); err != nil {
		return zero, err
	}
	data, err = json.Marshal(researchOperationRecord{Authorization: auth, TokenHash: hash})
	if err != nil {
		return zero, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO program_research_operations(id,program_id,request_id,request_hash,state,reserved_bytes,data) VALUES(?,?,?,?,'pending',?,?)`, auth.ID, cap.ProgramID, req.RequestID, requestHash, reserved, data)
	if err != nil {
		return zero, err
	}
	return auth, tx.Commit()
}

func researchResolveRequestTx(ctx context.Context, tx *sql.Tx, rev model.ProgramRevision, req model.ResearchOperationRequest, auth *model.ResearchAuthorization) error {
	if req.Kind == "search" {
		if req.TopicID == "" || req.SeedID != "" || req.LeadID != "" || req.URL != "" || req.Repository != "" || req.Ref != "" || req.Path != "" || !rev.Spec.Disclosure.PublicSearchAllowed {
			return researchError("source_rejected", "search requires an approved topic only")
		}
		for _, topic := range rev.Spec.Disclosure.PublicSearchTopics {
			if topic.ID == req.TopicID && topic.Query != "" {
				auth.Query = topic.Query
				return nil
			}
		}
		return researchError("source_rejected", "search topic was not approved")
	}
	if req.Kind != "capture" && req.Kind != "github" {
		return researchError("invalid_contract", "unknown research operation kind")
	}
	if req.TopicID != "" || (req.SeedID != "" && req.LeadID != "") {
		return researchError("source_rejected", "capture requires exactly one approved seed or recorded lead")
	}
	if req.SeedID != "" {
		for _, seed := range rev.Spec.Seeds {
			if seed.ID == req.SeedID && seed.Kind == "public_url" {
				auth.URL = seed.URL
				break
			}
		}
	} else if req.LeadID != "" {
		var data []byte
		var lead model.SearchLead
		if err := tx.QueryRowContext(ctx, `SELECT data FROM program_leads WHERE id=? AND program_id=? AND revision=?`, req.LeadID, rev.ProgramID, rev.Number).Scan(&data); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return researchError("source_rejected", "lead is outside capability scope")
			}
			return err
		}
		if err := json.Unmarshal(data, &lead); err != nil {
			return err
		}
		auth.URL = lead.URL
		if err := researchLeadAuthorityTx(ctx, tx, rev, lead); err != nil {
			return err
		}
	} else if req.URL != "" {
		// URL-only requests may select an exact approved seed, never modify it.
		for _, seed := range rev.Spec.Seeds {
			if seed.Kind == "public_url" && seed.URL == req.URL {
				auth.URL = seed.URL
				break
			}
		}
	}
	if auth.URL == "" || req.URL != "" && req.URL != auth.URL || !researchURL(auth.URL) {
		return researchError("source_rejected", "URL is not an exact approved seed or recorded lead")
	}
	if req.Kind == "capture" && (req.Repository != "" || req.Ref != "" || req.Path != "") {
		return researchError("source_rejected", "capture cannot append GitHub parameters")
	}
	if req.Kind == "github" {
		repository, ref, path, ok := researchGitHubLocation(auth.URL)
		if !ok {
			return researchError("source_rejected", "GitHub source requires an approved file URL")
		}
		if req.Repository != "" && req.Repository != repository || req.Ref != "" && req.Ref != ref || req.Path != "" && req.Path != path {
			return researchError("source_rejected", "GitHub parameters differ from approved URL")
		}
	}
	return nil
}

func researchLeadAuthorityTx(ctx context.Context, tx *sql.Tx, rev model.ProgramRevision, lead model.SearchLead) error {
	if err := programInvalidTx(ctx, tx, rev.ProgramID, lead.ID); err != nil {
		return err
	}
	if lead.Origin == "" {
		return researchError("evidence_invalid", "lead provenance is missing")
	}
	if err := programInvalidTx(ctx, tx, rev.ProgramID, lead.Origin); err != nil {
		return err
	}
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM program_sources WHERE id=? AND program_id=? AND revision=?`, lead.Origin, rev.ProgramID, rev.Number).Scan(&data)
	if err == nil {
		var source model.SourceCapture
		if err = json.Unmarshal(data, &source); err != nil {
			return err
		}
		if !source.Valid || source.FreshUntil == nil || !time.Now().Before(*source.FreshUntil) {
			return researchError("evidence_invalid", "lead source is invalid or expired")
		}
		if err = programInvalidTx(ctx, tx, rev.ProgramID, source.ContentSHA256); err != nil {
			return err
		}
		return programInvalidTx(ctx, tx, rev.ProgramID, source.TextSHA256)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_searches WHERE id=? AND program_id=? AND revision=?`, lead.Origin, rev.ProgramID, rev.Number).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return researchError("evidence_invalid", "lead provenance is outside capability scope")
	}
	return nil
}

// Network validation remains the broker's responsibility. This ledger check
// excludes alternate authority/credentials/fragments before any IO is granted.
func researchURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.Opaque == "" && (u.Port() == "" || u.Port() == "443")
}

func researchGitHubLocation(raw string) (repository, ref, path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" {
		return
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if u.Hostname() == "github.com" {
		if len(parts) < 5 || parts[2] != "blob" {
			return
		}
		parts = append(parts[:2], parts[3:]...)
	} else if u.Hostname() != "raw.githubusercontent.com" {
		return
	}
	if len(parts) < 4 {
		return
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return
		}
	}
	return parts[0] + "/" + parts[1], parts[2], strings.Join(parts[3:], "/"), true
}

func researchEscapedPath(path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// CompleteResearchOperation accepts only the exact durable authorization. CAS
// reads verify full bytes; no remote source access occurs inside the transaction.
func (s *Store) CompleteResearchOperation(ctx context.Context, auth model.ResearchAuthorization, result model.ResearchOperationResult) (model.ResearchOperationResult, error) {
	var zero model.ResearchOperationResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var record researchOperationRecord
	var data, terminal []byte
	var state string
	var reserved int64
	if err = tx.QueryRowContext(ctx, `SELECT data,state,reserved_bytes,result FROM program_research_operations WHERE id=?`, auth.ID).Scan(&data, &state, &reserved, &terminal); err != nil {
		return zero, err
	}
	if err = json.Unmarshal(data, &record); err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(auth, record.Authorization) {
		return zero, researchError("source_rejected", "research authorization was modified")
	}
	if state == "completed" {
		if err = json.Unmarshal(terminal, &zero); err != nil {
			return zero, err
		}
		return zero, tx.Commit()
	}
	if state != "pending" {
		return zero, researchError("operation_uncertain", "research operation has unknown ownership")
	}
	if result.ObservedBytes < 0 || result.ObservedBytes > reserved {
		return zero, researchError("source_too_large", "observed bytes exceed the durable reservation")
	}
	rec, err := researchLoadCapabilityTx(ctx, tx, record.TokenHash)
	if err != nil {
		return zero, err
	}
	rev, authorityErr := s.researchCapabilityAuthorityTx(ctx, tx, rec)
	if authorityErr == nil {
		resolved := auth
		authorityErr = researchResolveRequestTx(ctx, tx, rev, auth.Request, &resolved)
		if authorityErr == nil && (resolved.URL != auth.URL || resolved.Query != auth.Query) {
			authorityErr = researchError("source_rejected", "research scope changed during IO")
		}
	}
	if authorityErr == nil {
		err = s.researchValidateResult(ctx, auth, &result)
	} else {
		err = authorityErr
	}
	if err != nil {
		code := "source_rejected"
		var pe *model.ProgramError
		if errors.As(err, &pe) {
			code = pe.Code
		}
		result = model.ResearchOperationResult{ObservedBytes: result.ObservedBytes, ErrorCode: code, ErrorMessage: "research result rejected by controller authority"}
	}
	budget, budgetErr := getProgramBudgetTx(ctx, tx, auth.ProgramID)
	if budgetErr != nil {
		return zero, budgetErr
	}
	if budget.ReservedCaptureBytes < reserved {
		return zero, researchError("operation_uncertain", "capture reservation ledger is inconsistent")
	}
	budget.ReservedCaptureBytes -= reserved
	budget.CaptureBytes += result.ObservedBytes
	if budgetErr = putProgramBudgetTx(ctx, tx, auth.ProgramID, budget); budgetErr != nil {
		return zero, budgetErr
	}
	if result.ErrorCode == "" {
		if result.Source != nil {
			data, budgetErr = json.Marshal(result.Source)
			if budgetErr == nil {
				_, budgetErr = tx.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, result.Source.ID, auth.ProgramID, auth.Revision, data)
			}
			if budgetErr != nil {
				return zero, budgetErr
			}
		}
		if result.Search != nil {
			data, budgetErr = json.Marshal(result.Search)
			if budgetErr == nil {
				_, budgetErr = tx.ExecContext(ctx, `INSERT INTO program_searches(id,program_id,revision,data) VALUES(?,?,?,?)`, result.Search.ID, auth.ProgramID, auth.Revision, data)
			}
			if budgetErr != nil {
				return zero, budgetErr
			}
		}
		for _, lead := range result.Leads {
			data, budgetErr = json.Marshal(lead)
			if budgetErr == nil {
				_, budgetErr = tx.ExecContext(ctx, `INSERT INTO program_leads(id,program_id,revision,data) VALUES(?,?,?,?)`, lead.ID, auth.ProgramID, auth.Revision, data)
			}
			if budgetErr != nil {
				return zero, budgetErr
			}
		}
	}
	terminal, budgetErr = json.Marshal(result)
	if budgetErr != nil {
		return zero, budgetErr
	}
	if _, budgetErr = tx.ExecContext(ctx, `UPDATE program_research_operations SET state='completed',reserved_bytes=0,result=? WHERE id=? AND state='pending'`, terminal, auth.ID); budgetErr != nil {
		return zero, budgetErr
	}
	if budgetErr = tx.Commit(); budgetErr != nil {
		return zero, budgetErr
	}
	if err != nil {
		return zero, err
	}
	return result, nil
}

func (s *Store) researchValidateResult(ctx context.Context, auth model.ResearchAuthorization, result *model.ResearchOperationResult) error {
	reject := func() error { return researchError("source_rejected", "invalid research result shape or provenance") }
	if result.ErrorCode != "" {
		if result.Source != nil || result.Search != nil || len(result.Leads) != 0 || len(result.ErrorCode) > 128 || len(result.ErrorMessage) > 4096 {
			return reject()
		}
		return nil
	}
	if result.ErrorMessage != "" || len(result.Leads) > 100 {
		return reject()
	}
	now := time.Now().UTC()
	if auth.Request.Kind == "search" {
		if result.Search == nil || result.Source != nil || result.ObservedBytes != 0 {
			return reject()
		}
		search := result.Search
		if search.Provider != "parallel_keyless_mcp" || search.Status != "completed" {
			return reject()
		}
		if len(result.Leads) == 0 {
			result.Leads = search.Leads
		} else if len(search.Leads) > 0 && !reflect.DeepEqual(result.Leads, search.Leads) {
			return reject()
		}
		if len(result.Leads) > 100 {
			return reject()
		}
		search.ID = ID()
		search.ProgramID = auth.ProgramID
		search.Revision = auth.Revision
		search.QueryID = auth.Request.TopicID
		search.Query = auth.Query
		search.RequestID = auth.Request.RequestID
		search.RetrievedAt = now
	} else {
		source := result.Source
		if source == nil || result.Search != nil || source.Kind != auth.Request.Kind || source.ByteLength < 0 || source.ByteLength > auth.MaxCaptureBytes || result.ObservedBytes < source.ByteLength || source.Status < 200 || source.Status >= 300 || !researchURL(source.FinalURL) || source.ExtractorVersion == "" || len(source.RedirectChain) > auth.MaxRedirects || source.Bundle != nil {
			return reject()
		}
		if source.RequestedURL != "" && source.RequestedURL != auth.URL {
			return reject()
		}
		for _, hop := range source.RedirectChain {
			if !researchURL(hop) {
				return reject()
			}
		}
		if s.blobs == nil {
			return researchError("prerequisite_missing", "source CAS is unavailable")
		}
		body, err := s.blobs.Read(ctx, source.ContentSHA256, auth.MaxCaptureBytes)
		if err != nil || int64(len(body)) != source.ByteLength {
			return reject()
		}
		text, err := s.blobs.Read(ctx, source.TextSHA256, auth.MaxTextBytes)
		if err != nil {
			return reject()
		}
		extracted, err := research.Extract(body, source.MediaType, source.FinalURL, auth.MaxTextBytes)
		if err != nil || extracted.Text != string(text) || source.ExtractorVersion != research.ExtractorVersion || source.PublisherCanonicalURL != extracted.CanonicalURL {
			return reject()
		}
		links := make(map[string]bool, len(extracted.Links))
		for _, link := range extracted.Links {
			links[link] = true
		}
		for _, lead := range result.Leads {
			if !links[lead.URL] {
				return reject()
			}
		}
		if auth.Request.Kind == "github" {
			gh := source.GitHub
			if gh == nil || len(gh.Commit) != 40 || len(gh.BlobSHA) != 40 || gh.Path == "" || gh.Repository == "" || gh.BlobIdentityScope == "" || gh.CommitResolutionSHA256 == "" || !researchURL(gh.CommitResolutionURL) {
				return reject()
			}
			if _, err = hex.DecodeString(gh.Commit); err != nil {
				return reject()
			}
			if _, err = hex.DecodeString(gh.BlobSHA); err != nil {
				return reject()
			}
			resolution, readErr := s.blobs.Read(ctx, gh.CommitResolutionSHA256, auth.MaxCaptureBytes)
			if readErr != nil || int64(len(resolution))+source.ByteLength > result.ObservedBytes {
				return reject()
			}
			var resolved struct {
				SHA string `json:"sha"`
			}
			if json.Unmarshal(resolution, &resolved) != nil || resolved.SHA != gh.Commit {
				return reject()
			}
			repository, ref, path, ok := researchGitHubLocation(auth.URL)
			if !ok {
				return reject()
			}
			pinned := "https://raw.githubusercontent.com/" + repository + "/" + gh.Commit + "/" + researchEscapedPath(path)
			if gh.Repository != repository || gh.Path != path || source.FinalURL != pinned || gh.CommitResolutionURL != "https://api.github.com/repos/"+repository+"/commits/"+url.PathEscape(ref) || len(ref) == 40 && ref != gh.Commit {
				return reject()
			}
			blobHash := sha1.New()
			fmt.Fprintf(blobHash, "blob %d\x00", len(body))
			blobHash.Write(body)
			if gh.BlobSHA != hex.EncodeToString(blobHash.Sum(nil)) || gh.BlobIdentityScope != "computed_git_blob_sha1_from_complete_public_raw_body" {
				return reject()
			}
		} else if source.GitHub != nil {
			return reject()
		}
		source.ID = ID()
		source.ProgramID = auth.ProgramID
		source.Revision = auth.Revision
		source.RequestKey = auth.Request.RequestID
		source.RequestedURL = auth.URL
		source.RetrievedAt = now
		source.Valid = true
		fresh := now.Add(time.Duration(auth.FreshnessSeconds) * time.Second)
		source.FreshUntil = &fresh
	}
	for i := range result.Leads {
		lead := &result.Leads[i]
		if !researchURL(lead.URL) || len(lead.Title) > 4096 || len(lead.Snippet) > 16384 {
			return reject()
		}
		lead.ID = ID()
		if result.Source != nil {
			lead.Origin = result.Source.ID
		} else {
			lead.Origin = result.Search.ID
		}
	}
	if result.Search != nil {
		result.Search.Leads = result.Leads
	}
	return nil
}

func (s *Store) ResearchSource(ctx context.Context, token, id string) (model.SourceCapture, error) {
	var source model.SourceCapture
	if len(token) != 64 || id == "" {
		return source, researchError("source_rejected", "invalid source capability")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return source, err
	}
	defer tx.Rollback()
	rec, err := researchLoadCapabilityTx(ctx, tx, researchHash(token))
	if err != nil {
		return source, err
	}
	if _, err = s.researchCapabilityAuthorityTx(ctx, tx, rec); err != nil {
		return source, err
	}
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_sources WHERE id=? AND program_id=? AND revision=?`, id, rec.Capability.ProgramID, rec.Capability.Revision).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return source, researchError("source_rejected", "source is outside capability scope")
		}
		return source, err
	}
	if err = json.Unmarshal(data, &source); err != nil {
		return source, err
	}
	if !source.Valid || source.FreshUntil == nil || !time.Now().Before(*source.FreshUntil) {
		return model.SourceCapture{}, researchError("evidence_invalid", "source is invalid or expired")
	}
	var invalid int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_invalidations WHERE program_id=? AND subject IN (?,?,?)`, rec.Capability.ProgramID, source.ID, source.ContentSHA256, source.TextSHA256).Scan(&invalid); err != nil {
		return model.SourceCapture{}, err
	}
	if invalid != 0 {
		return model.SourceCapture{}, researchError("evidence_invalid", "source has been invalidated")
	}
	return source, tx.Commit()
}
