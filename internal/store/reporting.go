package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"openfactory/internal/model"
)

var ErrInvalidQuery = errors.New("invalid reporting query")
var ErrNotFound = errors.New("reporting feature not found")

const reportDefaultLimit = 100
const reportMaxLimit = 500
const reportDetailLimit = 2048

// Cursors bind the position and initial high-water mark to the endpoint and its
// filters. Sequence/rowid ordering does not depend on mutable display timestamps.
type reportCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Scope   string `json:"scope,omitempty"`
	Project string `json:"project,omitempty"`
	State   string `json:"state,omitempty"`
	After   string `json:"after"`
	Upper   string `json:"upper"`
}

func reportLimit(limit int) (int, error) {
	if limit == 0 {
		return reportDefaultLimit, nil
	}
	if limit < 0 || limit > reportMaxLimit {
		return 0, ErrInvalidQuery
	}
	return limit, nil
}

func encodeReportCursor(c reportCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeReportCursor(raw string, expected reportCursor) (reportCursor, error) {
	if raw == "" {
		return expected, nil
	}
	if len(raw) > 4096 {
		return expected, ErrInvalidQuery
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return expected, ErrInvalidQuery
	}
	var c reportCursor
	if json.Unmarshal(data, &c) != nil || encodeReportCursor(c) != raw || c.Version != 1 || c.Kind != expected.Kind || c.Scope != expected.Scope || c.Project != expected.Project || c.State != expected.State || c.After == "" || c.Upper == "" {
		return expected, ErrInvalidQuery
	}
	if c.Kind == "features" {
		if c.After > c.Upper {
			return expected, ErrInvalidQuery
		}
	} else {
		a, e1 := strconv.ParseInt(c.After, 10, 64)
		u, e2 := strconv.ParseInt(c.Upper, 10, 64)
		if e1 != nil || e2 != nil || a < 1 || u < a || strconv.FormatInt(a, 10) != c.After || strconv.FormatInt(u, 10) != c.Upper {
			return expected, ErrInvalidQuery
		}
	}
	return c, nil
}

func validReportState(state string) bool {
	switch state {
	case "", "proposed", "awaiting_approval", "ready", "running", "blocked", "uncertain", "failed", "accepted", "cancelling", "cancelled":
		return true
	default:
		return false
	}
}

// Status reads counters, admission gates and every feature detail from one SQLite
// snapshot. No Store method is called while the sole connection belongs to tx.
func (s *Store) Status(ctx context.Context, q model.StatusQuery, cfg model.Config) (model.StatusSnapshot, error) {
	out := model.StatusSnapshot{Features: []model.FeatureStatus{}, Limits: model.StatusLimits{Projects: []model.ProjectLimit{}}}
	limit, err := reportLimit(q.Limit)
	if err != nil || !validReportState(q.State) {
		return out, ErrInvalidQuery
	}
	if q.Project != "" {
		if _, ok := cfg.Projects[q.Project]; !ok {
			return out, ErrInvalidQuery
		}
	}
	cursor, err := decodeReportCursor(q.After, reportCursor{Version: 1, Kind: "features", Project: q.Project, State: q.State})
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out.ObservedAt = time.Now().UTC()
	out.Calls = model.CallStatus{Day: out.ObservedAt.Format("2006-01-02"), Limit: cfg.MaxCallsPerDay}
	var paused int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='paused' AND value='true'`).Scan(&paused); err != nil {
		return out, err
	}
	out.Paused = paused != 0
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM calls WHERE day=?`, out.Calls.Day).Scan(&out.Calls.Used); err != nil {
		return out, err
	}
	out.Limits.Global.Limit = cfg.MaxConcurrent
	active := map[string]int{}
	rows, err := tx.QueryContext(ctx, `SELECT f.data FROM attempts a JOIN features f ON f.id=a.feature_id WHERE a.active=1`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var data []byte
		var f model.Feature
		if err = rows.Scan(&data); err == nil {
			err = json.Unmarshal(data, &f)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		active[f.Project]++
		out.Limits.Global.Active++
		if internalIntakeFeature(f) {
			out.IntakeStepCount++
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	projects := make([]string, 0, len(cfg.Projects))
	for name := range cfg.Projects {
		projects = append(projects, name)
	}
	sort.Strings(projects)
	for _, name := range projects {
		cap := cfg.Projects[name].MaxConcurrent
		if cap == 0 {
			cap = 1
		}
		out.Limits.Projects = append(out.Limits.Projects, model.ProjectLimit{Name: name, Active: active[name], Limit: cap})
	}
	if q.After == "" {
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(id),'') FROM features`).Scan(&cursor.Upper); err != nil {
			return out, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT data FROM features WHERE id>? AND id<=? AND coalesce(json_extract(data,'$.intake_step_id'),'')='' AND coalesce(json_extract(data,'$.kind'),'')!='intake' AND id NOT GLOB 'openfactory-intake-*' AND (?='' OR json_extract(data,'$.project')=?) AND (?='' OR json_extract(data,'$.state')=?) ORDER BY id LIMIT ?`, cursor.After, cursor.Upper, q.Project, q.Project, q.State, q.State, limit+1)
	if err != nil {
		return out, err
	}
	features := []model.Feature{}
	for rows.Next() {
		var data []byte
		var f model.Feature
		if err = rows.Scan(&data); err == nil {
			err = json.Unmarshal(data, &f)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		features = append(features, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(features) > limit {
		features = features[:limit]
		cursor.After = features[len(features)-1].ID
		out.NextCursor = encodeReportCursor(cursor)
	}
	for _, f := range features {
		detail, e := s.reportFeature(ctx, tx, f, cfg)
		if e != nil {
			return out, e
		}
		out.Features = append(out.Features, detail)
	}
	return out, tx.Commit()
}

func (s *Store) FeatureStatus(ctx context.Context, id string, cfg model.Config) (model.FeatureStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.FeatureStatus{}, err
	}
	defer tx.Rollback()
	f, err := get(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FeatureStatus{}, ErrNotFound
	}
	if err != nil {
		return model.FeatureStatus{}, err
	}
	if internalIntakeFeature(f) {
		return model.FeatureStatus{}, ErrNotFound
	}
	out, err := s.reportFeature(ctx, tx, f, cfg)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

type reportAttempt struct {
	Attempt  model.Attempt
	Active   bool
	Revision string
	Policy   string
}

func reportAttempts(ctx context.Context, tx *sql.Tx, id string) ([]reportAttempt, error) {
	rows, err := tx.QueryContext(ctx, `SELECT data,active,revision,policy_hash FROM attempts WHERE feature_id=? ORDER BY rowid DESC LIMIT ?`, id, reportDefaultLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []reportAttempt{}
	for rows.Next() {
		var data []byte
		var a reportAttempt
		if err = rows.Scan(&data, &a.Active, &a.Revision, &a.Policy); err == nil {
			err = json.Unmarshal(data, &a.Attempt)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) reportFeature(ctx context.Context, tx *sql.Tx, f model.Feature, cfg model.Config) (model.FeatureStatus, error) {
	out := model.FeatureStatus{Attempts: []model.AttemptStatus{}, Sessions: []model.SessionStatus{}, Checks: []model.CheckStatus{}, CurrentEvidence: []model.Evidence{}, Integration: model.IntegrationStatus{Resources: []string{}}}
	attempts, err := reportAttempts(ctx, tx, f.ID)
	if err != nil {
		return out, err
	}
	clean := newReportSanitizer(f, attempts, cfg)
	out.Feature = model.Feature{ID: reportPublic(f.ID, 256), Project: reportPublic(f.Project, 256), Title: clean.text(f.Title, 512), Kind: reportPublic(f.Kind, 128), Risk: reportPublic(f.Risk, 128), Priority: f.Priority, DependsOn: clean.list(f.DependsOn), CompletionTarget: reportPublic(f.CompletionTarget, 128), Checks: clean.list(f.Checks), Acceptance: []model.Acceptance{}, Revision: reportPublic(f.Revision, 256), State: reportPublic(f.State, 128), Reason: clean.text(f.Reason, reportDetailLimit), Attempts: f.Attempts, Receipt: clean.receipt(f.Receipt), CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt, DiscoveryStepID: reportPublic(f.DiscoveryStepID, 256), ProgramEvidenceGate: programReadSafe(f.ProgramEvidenceGate)}
	out.Receipt = out.Feature.Receipt
	active := false
	for _, entry := range attempts {
		a := entry.Attempt
		out.Attempts = append(out.Attempts, model.AttemptStatus{ID: reportPublic(a.ID, 256), FeatureID: reportPublic(a.FeatureID, 256), Stage: reportPublic(a.Stage, 128), State: reportPublic(a.State, 128), Active: entry.Active, StartedAt: a.StartedAt, StageStartedAt: a.StageStartedAt, UpdatedAt: a.UpdatedAt, Deadline: a.Deadline, BaseSHA: reportPublic(a.BaseSHA, 256), CandidateSHA: reportPublic(a.CandidateSHA, 256), CandidateGeneration: a.CandidateGeneration, SourceBaseSHA: reportPublic(a.SourceBaseSHA, 256), SourceCandidateSHA: reportPublic(a.SourceCandidateSHA, 256)})
		if entry.Active {
			active = true
			out.Wait = clean.wait(a.Wait)
		}
	}
	if !active && f.State != "accepted" && f.State != "cancelled" {
		cap := cfg.Projects[f.Project].MaxConcurrent
		if cap == 0 {
			cap = 1
		}
		wait, e := s.admissionGate(ctx, tx, f, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, cap)
		if e != nil {
			return out, e
		}
		out.Wait = clean.wait(wait)
	}
	if len(attempts) > 0 {
		latest := attempts[0]
		out.Integration, err = integrationStatus(ctx, tx, latest.Attempt.ID)
		if errors.Is(err, sql.ErrNoRows) {
			out.Integration = model.IntegrationStatus{}
			err = nil
		}
		if err != nil {
			return out, err
		}
		out.Integration.Token = ""
		resources := make([]string, 0, len(out.Integration.Resources))
		for _, resource := range out.Integration.Resources {
			resources = append(resources, reportResource(resource))
		}
		out.Integration.Resources = resources
		if out.Integration.ContendedResource != "" {
			out.Integration.ContendedResource = reportResource(out.Integration.ContendedResource)
		}
		out.Integration.State = reportPublic(out.Integration.State, 128)
		out.Integration.OwnerAttemptID = reportPublic(out.Integration.OwnerAttemptID, 256)
		if latest.Revision == f.Revision && latest.Policy == f.PolicyHash && latest.Attempt.CandidateSHA != "" {
			rows, e := tx.QueryContext(ctx, `SELECT data FROM evidence WHERE attempt_id=? AND revision=? AND policy_hash=? AND coalesce(json_extract(data,'$.candidate_generation'),0)=? AND json_extract(data,'$.candidate_sha')=? ORDER BY rowid DESC LIMIT ?`, latest.Attempt.ID, latest.Revision, latest.Policy, latest.Attempt.CandidateGeneration, latest.Attempt.CandidateSHA, reportMaxLimit)
			if e != nil {
				return out, e
			}
			for rows.Next() {
				var data []byte
				var evidence model.Evidence
				if e = rows.Scan(&data); e == nil {
					e = json.Unmarshal(data, &evidence)
				}
				if e != nil {
					rows.Close()
					return out, e
				}
				out.CurrentEvidence = append(out.CurrentEvidence, clean.evidence(evidence))
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return out, e
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.attempt_id,s.stage,s.dispatch_key,s.thread_id,s.environment_id,s.created_at FROM sessions s JOIN attempts a ON a.id=s.attempt_id WHERE a.feature_id=? ORDER BY s.rowid DESC`, f.ID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var session model.SessionStatus
		var at string
		if err = rows.Scan(&session.AttemptID, &session.Stage, &session.DispatchKey, &session.ThreadID, &session.EnvironmentID, &at); err == nil {
			session.CreatedAt, err = reportTime(at)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		session.AttemptID = reportPublic(session.AttemptID, 256)
		session.Stage = reportPublic(session.Stage, 128)
		session.DispatchKey = reportPublic(session.DispatchKey, 256)
		session.ThreadID = reportPublic(session.ThreadID, 256)
		session.EnvironmentID = reportPublic(session.EnvironmentID, 256)
		out.Sessions = append(out.Sessions, session)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Sessions, err = reportLegacySessions(ctx, tx, f.ID, out.Sessions)
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT c.attempt_id,c.name,c.generation,c.sha,c.state,c.detail,c.started_at,c.updated_at,a.data,r.data FROM checks c JOIN attempts a ON a.id=c.attempt_id LEFT JOIN revisions r ON r.feature_id=a.feature_id AND r.revision=a.revision WHERE a.feature_id=? ORDER BY c.rowid DESC LIMIT ?`, f.ID, reportMaxLimit)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var check model.CheckStatus
		var started, updated string
		var attemptData, featureData []byte
		if err = rows.Scan(&check.AttemptID, &check.Name, &check.CandidateGeneration, &check.CandidateSHA, &check.State, &check.Detail, &started, &updated, &attemptData, &featureData); err == nil {
			check.StartedAt, err = reportTime(started)
		}
		if err == nil {
			check.UpdatedAt, err = reportTime(updated)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		history, e := clean.history(attemptData, featureData)
		if e != nil {
			rows.Close()
			return out, e
		}
		check.AttemptID = reportPublic(check.AttemptID, 256)
		check.Name = reportPublic(check.Name, 256)
		check.CandidateSHA = reportPublic(check.CandidateSHA, 256)
		check.State = reportPublic(check.State, 128)
		check.Detail = history.text(check.Detail, reportDetailLimit)
		out.Checks = append(out.Checks, check)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Dependencies, out.Timeline, err = reportFeatureHistory(ctx, tx, f)
	return out, err
}

func reportTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func (s *Store) EventPage(ctx context.Context, id string, q model.PageQuery) (model.EventPage, error) {
	out := model.EventPage{Items: []model.Event{}}
	limit, err := reportLimit(q.Limit)
	if err != nil {
		return out, err
	}
	cursor, err := decodeReportCursor(q.After, reportCursor{Version: 1, Kind: "events", Scope: id})
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	clean, err := reportPageSanitizer(ctx, tx, id)
	if err != nil {
		return out, err
	}
	if q.After == "" {
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM events WHERE feature_id=?`, id).Scan(&cursor.Upper); err != nil {
			return out, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.seq,e.id,e.feature_id,e.kind,e.detail,e.created_at,a.data,r.data FROM events e LEFT JOIN event_progress p ON p.event_id=e.id LEFT JOIN attempts a ON a.id=json_extract(p.data,'$.attempt_id') LEFT JOIN revisions r ON r.feature_id=a.feature_id AND r.revision=a.revision WHERE e.feature_id=? AND e.seq<=? AND (?='' OR e.seq<?) ORDER BY e.seq DESC LIMIT ?`, id, cursor.Upper, cursor.After, cursor.After, limit+1)
	if err != nil {
		return out, err
	}
	last := ""
	for rows.Next() {
		var seq int64
		var item model.Event
		var at string
		var attemptData, featureData []byte
		if err = rows.Scan(&seq, &item.ID, &item.FeatureID, &item.Kind, &item.Detail, &at, &attemptData, &featureData); err == nil {
			item.CreatedAt, err = reportTime(at)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Items) == limit {
			cursor.After = last
			out.NextCursor = encodeReportCursor(cursor)
			break
		}
		history, e := clean.history(attemptData, featureData)
		if e != nil {
			rows.Close()
			return out, e
		}
		item.ID = reportPublic(item.ID, 256)
		item.FeatureID = reportPublic(item.FeatureID, 256)
		item.Kind = reportPublic(item.Kind, 128)
		item.Detail = history.text(item.Detail, reportDetailLimit)
		out.Items = append(out.Items, item)
		last = strconv.FormatInt(seq, 10)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) EvidencePage(ctx context.Context, id string, q model.PageQuery) (model.EvidencePage, error) {
	out := model.EvidencePage{Items: []model.Evidence{}}
	limit, err := reportLimit(q.Limit)
	if err != nil {
		return out, err
	}
	cursor, err := decodeReportCursor(q.After, reportCursor{Version: 1, Kind: "evidence", Scope: id})
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	clean, err := reportPageSanitizer(ctx, tx, id)
	if err != nil {
		return out, err
	}
	if q.After == "" {
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(rowid),0) FROM evidence WHERE feature_id=?`, id).Scan(&cursor.Upper); err != nil {
			return out, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.rowid,e.data,a.data,r.data FROM evidence e JOIN attempts a ON a.id=e.attempt_id LEFT JOIN revisions r ON r.feature_id=e.feature_id AND r.revision=e.revision WHERE e.feature_id=? AND e.rowid<=? AND (?='' OR e.rowid<?) ORDER BY e.rowid DESC LIMIT ?`, id, cursor.Upper, cursor.After, cursor.After, limit+1)
	if err != nil {
		return out, err
	}
	last := ""
	for rows.Next() {
		var seq int64
		var data []byte
		var item model.Evidence
		var attemptData, featureData []byte
		if err = rows.Scan(&seq, &data, &attemptData, &featureData); err == nil {
			err = json.Unmarshal(data, &item)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Items) == limit {
			cursor.After = last
			out.NextCursor = encodeReportCursor(cursor)
			break
		}
		history, e := clean.history(attemptData, featureData)
		if e != nil {
			rows.Close()
			return out, e
		}
		out.Items = append(out.Items, history.evidence(item))
		last = strconv.FormatInt(seq, 10)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func reportPageSanitizer(ctx context.Context, tx *sql.Tx, id string) (reportSanitizer, error) {
	f, err := get(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return reportSanitizer{}, ErrNotFound
	}
	if err != nil {
		return reportSanitizer{}, err
	}
	if internalIntakeFeature(f) {
		return reportSanitizer{}, ErrNotFound
	}
	attempts, err := reportAttempts(ctx, tx, id)
	if err != nil {
		return reportSanitizer{}, err
	}
	return newReportSanitizer(f, attempts, model.Config{}), nil
}

// Reporting never serializes stored execution objects. Text is a final defense
// for error/output fields that may themselves contain controller-private values.
type reportSanitizer struct{ private []string }

// History pages may reach attempts outside the bounded detail history. Join only
// each page row's original authority instead of loading all prior execution data.
func (s reportSanitizer) history(attemptData, featureData []byte) (reportSanitizer, error) {
	if len(attemptData) == 0 && len(featureData) == 0 {
		return s, nil
	}
	var f model.Feature
	var a model.Attempt
	if len(featureData) > 0 {
		if err := json.Unmarshal(featureData, &f); err != nil {
			return s, err
		}
	}
	if len(attemptData) > 0 {
		if err := json.Unmarshal(attemptData, &a); err != nil {
			return s, err
		}
	}
	extra := newReportSanitizer(f, []reportAttempt{{Attempt: a}}, model.Config{})
	if len(extra.private) == 0 {
		return s, nil
	}
	combined := make([]string, 0, len(s.private)+len(extra.private))
	combined = append(combined, s.private...)
	combined = append(combined, extra.private...)
	sort.Slice(combined, func(i, j int) bool { return len(combined[i]) > len(combined[j]) })
	return reportSanitizer{private: combined}, nil
}

var reportPrivatePath = regexp.MustCompile(`(?i)https?://[^\s"'<>),;]+|(^|[\s("'=:])(?:file://)?(?:/|~/|[a-z]:\\)[^\s"'<>),;]+`)
var reportSecret = regexp.MustCompile(`(?i)\b(?:bearer\s+|(?:authorization|token|password|secret|prompt|summary|body|configuration|config)\s*[:=]\s*)[^\r\n]+`)

func newReportSanitizer(f model.Feature, attempts []reportAttempt, cfg model.Config) reportSanitizer {
	out := reportSanitizer{}
	add := func(value string) {
		if len(value) >= 4 {
			out.private = append(out.private, value)
		}
	}
	add(f.Body)
	for _, a := range f.Acceptance {
		add(a.Scenario)
		add(a.Expected)
	}
	for _, entry := range attempts {
		a := entry.Attempt
		add(a.Workspace)
		add(a.Cursor)
		add(a.Fingerprint)
		add(a.Summary)
		add(a.IntegrationToken)
	}
	add(cfg.StateDir)
	add(cfg.WorkspaceDir)
	add(cfg.ReleaseDir)
	add(cfg.CheckSocket)
	add(cfg.WebhookURL)
	add(cfg.WebhookSecretFile)
	add(cfg.BBURL)
	for _, p := range cfg.Projects {
		add(p.Repository)
		add(p.Auth.TokenFile)
		add(p.Auth.SSHKeyFile)
		add(p.Auth.KnownHostsFile)
	}
	// Longest-first replacement cannot leave the suffix of an overlapping secret.
	sort.Slice(out.private, func(i, j int) bool { return len(out.private[i]) > len(out.private[j]) })
	return out
}

func (s reportSanitizer) text(value string, limit int) string {
	for _, private := range s.private {
		value = strings.ReplaceAll(value, private, "[redacted]")
	}
	value = reportSecret.ReplaceAllString(value, "[redacted]")
	value = reportPrivatePath.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasPrefix(strings.ToLower(match), "http://") || strings.HasPrefix(strings.ToLower(match), "https://") {
			u, err := url.Parse(match)
			if err != nil {
				return "[redacted URL]"
			}
			u.User = nil
			u.RawQuery = ""
			u.Fragment = ""
			return u.String()
		}
		if strings.ContainsRune(" \t\r\n(\"'=:", rune(match[0])) {
			return match[:1] + "[private path]"
		}
		return "[private path]"
	})
	return reportPublic(value, limit)
}

// Identity fields must not be rewritten by substring matches against private
// free text: callers use these exact IDs to address the detail endpoints.
func reportPublic(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if len(value) <= limit {
		return value
	}
	// Truncate on UTF-8 boundaries, with the suffix inside the byte bound.
	end := limit - len("…")
	for end > 0 && value[end]&0xc0 == 0x80 {
		end--
	}
	return value[:end] + "…"
}

func (s reportSanitizer) list(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, reportPublic(value, 256))
	}
	return out
}

func (s reportSanitizer) receipt(value string) string {
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || u.User != nil {
			return ""
		}
		u.RawQuery = ""
		u.Fragment = ""
		return s.text(u.String(), 512)
	}
	if strings.ContainsAny(value, "/\\") {
		return ""
	}
	return s.text(value, 512)
}

func (s reportSanitizer) evidence(e model.Evidence) model.Evidence {
	return model.Evidence{ID: reportPublic(e.ID, 256), FeatureID: reportPublic(e.FeatureID, 256), AttemptID: reportPublic(e.AttemptID, 256), Kind: reportPublic(e.Kind, 256), CandidateSHA: reportPublic(e.CandidateSHA, 256), CandidateGeneration: e.CandidateGeneration, Passed: e.Passed, Detail: s.text(e.Detail, reportDetailLimit), CreatedAt: e.CreatedAt}
}

func (s reportSanitizer) wait(wait *model.WaitReason) *model.WaitReason {
	if wait == nil {
		return nil
	}
	out := &model.WaitReason{Code: reportPublic(wait.Code, 128), Detail: s.text(wait.Detail, reportDetailLimit), FeatureID: reportPublic(wait.FeatureID, 256), OwnerID: reportPublic(wait.OwnerID, 256)}
	if wait.Resource != "" {
		out.Resource = reportResource(wait.Resource)
	}
	return out
}

func reportResource(resource string) string {
	sum := sha256.Sum256([]byte(resource))
	return fmt.Sprintf("resource:%s", hex.EncodeToString(sum[:8]))
}
