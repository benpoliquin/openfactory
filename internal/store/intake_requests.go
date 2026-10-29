package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// Request content and immutable history live separately: progress never changes a
// content version, and superseded jobs retain their exact inputs and outputs.
const intakeRequestSchema = `
CREATE TABLE IF NOT EXISTS intake_requests(id TEXT PRIMARY KEY,operator_id TEXT NOT NULL,project TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS intake_turns(request_id TEXT NOT NULL REFERENCES intake_requests(id),sequence INTEGER NOT NULL,client_message_id TEXT,data BLOB NOT NULL,PRIMARY KEY(request_id,sequence));
CREATE UNIQUE INDEX IF NOT EXISTS intake_message_dedup ON intake_turns(request_id,client_message_id) WHERE client_message_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS intake_drafts(request_id TEXT NOT NULL REFERENCES intake_requests(id),revision INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(request_id,revision));
CREATE TABLE IF NOT EXISTS intake_questions(id TEXT PRIMARY KEY,request_id TEXT NOT NULL REFERENCES intake_requests(id),data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS intake_question_answers(question_id TEXT PRIMARY KEY REFERENCES intake_questions(id),sequence INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS intake_steps(id TEXT PRIMARY KEY,request_id TEXT NOT NULL REFERENCES intake_requests(id),feature_id TEXT UNIQUE NOT NULL,active INTEGER NOT NULL,data BLOB NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS intake_one_active_step ON intake_steps(request_id) WHERE active=1;
CREATE TABLE IF NOT EXISTS intake_links(request_id TEXT NOT NULL REFERENCES intake_requests(id),draft_revision INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(request_id,draft_revision));
CREATE TABLE IF NOT EXISTS intake_events(request_id TEXT NOT NULL REFERENCES intake_requests(id),sequence INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(request_id,sequence));
CREATE TABLE IF NOT EXISTS intake_dispatches(request_id TEXT NOT NULL REFERENCES intake_requests(id),attempt_id TEXT NOT NULL,dispatch_key TEXT NOT NULL,PRIMARY KEY(attempt_id,dispatch_key));
CREATE TABLE IF NOT EXISTS intake_draft_authorizations(request_id TEXT NOT NULL REFERENCES intake_requests(id),content_version INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(request_id,content_version));
`

func intakeError(code, message string) error {
	return &model.ProgramError{Code: code, Message: message}
}
func intakeLoad[T any](ctx context.Context, q programQuerier, query string, args ...any) (T, error) {
	var out T
	var raw []byte
	err := q.QueryRowContext(ctx, query, args...).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
func intakeList[T any](ctx context.Context, q programQuerier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw []byte
		var item T
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func intakeRequestTx(ctx context.Context, q programQuerier, id string) (model.IntakeRequest, error) {
	return intakeLoad[model.IntakeRequest](ctx, q, `SELECT data FROM intake_requests WHERE id=?`, id)
}
func intakePutRequestTx(ctx context.Context, tx *sql.Tx, r model.IntakeRequest) error {
	r.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO intake_requests(id,operator_id,project,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data WHERE intake_requests.operator_id=excluded.operator_id AND intake_requests.project=excluded.project`, r.ID, r.OperatorID, r.Project, raw)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return intakeError("stale_subject", "request owner and project are immutable")
	}
	return nil
}
func intakeEventTx(ctx context.Context, tx *sql.Tx, r *model.IntakeRequest, kind, detail string) error {
	r.EventSequence++
	e := model.IntakeEvent{RequestID: r.ID, Sequence: r.EventSequence, Kind: kind, Detail: detail, CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_events(request_id,sequence,data) VALUES(?,?,?)`, r.ID, e.Sequence, raw); err != nil {
		return err
	}
	return intakePutRequestTx(ctx, tx, *r)
}
func intakeHistoryLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}
func (s *Store) IntakeRequest(ctx context.Context, id string) (model.IntakeRequest, error) {
	return intakeRequestTx(ctx, s.db, id)
}
func (s *Store) ListIntakeRequests(ctx context.Context, operatorID, project, after string, limit int) ([]model.IntakeRequest, error) {
	return intakeList[model.IntakeRequest](ctx, s.db, `SELECT data FROM intake_requests WHERE (?='' OR operator_id=?) AND (?='' OR project=?) AND id>? ORDER BY id LIMIT ?`, operatorID, operatorID, project, project, after, intakeHistoryLimit(limit))
}
func (s *Store) IntakeTurns(ctx context.Context, id string, after int64, limit int) ([]model.IntakeTurn, error) {
	return intakeList[model.IntakeTurn](ctx, s.db, `SELECT data FROM intake_turns WHERE request_id=? AND sequence>? ORDER BY sequence LIMIT ?`, id, after, intakeHistoryLimit(limit))
}
func (s *Store) IntakeEvents(ctx context.Context, id string, after int64, limit int) ([]model.IntakeEvent, error) {
	return intakeList[model.IntakeEvent](ctx, s.db, `SELECT data FROM intake_events WHERE request_id=? AND sequence>? ORDER BY sequence LIMIT ?`, id, after, intakeHistoryLimit(limit))
}
func (s *Store) IntakeDraft(ctx context.Context, id string, revision int) (model.IntakeDraft, error) {
	return intakeLoad[model.IntakeDraft](ctx, s.db, `SELECT data FROM intake_drafts WHERE request_id=? AND revision=?`, id, revision)
}
func (s *Store) IntakeDrafts(ctx context.Context, id string) ([]model.IntakeDraft, error) {
	return intakeList[model.IntakeDraft](ctx, s.db, `SELECT data FROM intake_drafts WHERE request_id=? ORDER BY revision`, id)
}
func (s *Store) IntakeQuestions(ctx context.Context, id string) ([]model.IntakeQuestion, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := intakeList[model.IntakeQuestion](ctx, tx, `SELECT data FROM intake_questions WHERE request_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for i := range out {
		err = tx.QueryRowContext(ctx, `SELECT sequence FROM intake_question_answers WHERE question_id=?`, out[i].ID).Scan(&out[i].AnsweredBySequence)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return out, tx.Commit()
}
func (s *Store) IntakeLinks(ctx context.Context, id string) ([]model.IntakeLink, error) {
	return intakeList[model.IntakeLink](ctx, s.db, `SELECT data FROM intake_links WHERE request_id=? ORDER BY draft_revision`, id)
}
func (s *Store) IntakeContextBlob(ctx context.Context, sha string, maxBytes int64) ([]byte, error) {
	return s.blobs.Read(ctx, sha, maxBytes)
}
func (s *Store) IntakeSteps(ctx context.Context, id string) ([]model.IntakeStep, error) {
	return intakeList[model.IntakeStep](ctx, s.db, `SELECT data FROM intake_steps WHERE request_id=? ORDER BY rowid`, id)
}

func intakeLimits(c model.Config) model.IntakePolicy {
	p := c.Intake
	if p.MaxTurnBytes <= 0 {
		p.MaxTurnBytes = 16 << 10
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = 256
	}
	if p.MaxTextBytes <= 0 {
		p.MaxTextBytes = 2 << 20
	}
	if p.MaxCanonicalBytes <= 0 {
		p.MaxCanonicalBytes = 256 << 10
	}
	if p.MaxOutputBytes <= 0 {
		p.MaxOutputBytes = 256 << 10
	}
	if p.MaxDraftCalls <= 0 {
		p.MaxDraftCalls = 12
	}
	return p
}
func intakeWritable(r model.IntakeRequest, expected *int, operator string) error {
	if r.OperatorID != operator {
		return intakeError("project_forbidden", "request belongs to another operator")
	}
	if r.State == model.IntakeSubmitted {
		return intakeError("stale_subject", "submitted work requires a new amendment request")
	}
	if r.State == model.IntakeCancelled || r.State == model.IntakeCancelling {
		return intakeError("stale_subject", "request is cancelled or cancelling")
	}
	if expected == nil || *expected != r.ContentVersion {
		return intakeError("stale_revision", "request content version changed")
	}
	return nil
}
func intakeValidateText(turns []model.IntakeTurn, text string, c model.Config) error {
	p := intakeLimits(c)
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return intakeError("invalid_request", "nonempty UTF-8 text is required")
	}
	if len(text) > p.MaxTurnBytes || len(turns) >= p.MaxTurns {
		return intakeError("input_limit", "request turn limit exceeded")
	}
	total := len(text)
	for _, t := range turns {
		total += len(t.Text)
	}
	if total > p.MaxTextBytes {
		return intakeError("input_limit", "request text limit exceeded")
	}
	return nil
}
func intakeTurnsTx(ctx context.Context, q programQuerier, id string) ([]model.IntakeTurn, error) {
	return intakeList[model.IntakeTurn](ctx, q, `SELECT data FROM intake_turns WHERE request_id=? ORDER BY sequence`, id)
}
func intakeAppendTurnTx(ctx context.Context, tx *sql.Tx, t model.IntakeTurn) error {
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM intake_turns WHERE request_id=?`, t.RequestID).Scan(&t.Sequence); err != nil {
		return err
	}
	t.CreatedAt = time.Now().UTC()
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	var client any
	if t.ClientMessageID != "" {
		client = t.ClientMessageID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_turns(request_id,sequence,client_message_id,data) VALUES(?,?,?,?)`, t.RequestID, t.Sequence, client, raw)
	return err
}

// Context IDs contain no timestamps or generated IDs, so a preview and its exact
// later command bind identical bytes. No transcript or draft is silently trimmed.
func (s *Store) intakeManifestTx(ctx context.Context, tx *sql.Tx, c model.Config, r model.IntakeRequest, bundles []string) (model.IntakeManifest, error) {
	m := model.IntakeManifest{RequestID: r.ID, ContentVersion: r.ContentVersion, Project: r.Project, PolicySHA: c.PolicyHash, Inputs: []model.IntakeInputRef{}}
	turns, err := intakeTurnsTx(ctx, tx, r.ID)
	if err != nil {
		return m, err
	}
	for _, t := range turns {
		raw, err := json.Marshal(intakeSafeTurn(t))
		if err != nil {
			return m, err
		}
		ref, err := s.blobs.Put(ctx, raw)
		if err != nil {
			return m, err
		}
		m.Inputs = append(m.Inputs, model.IntakeInputRef{Kind: "turn", ID: strconv.FormatInt(t.Sequence, 10), SHA: ref.SHA, Bytes: ref.Bytes})
	}
	producers, err := intakeList[model.IntakeStep](ctx, tx, `SELECT data FROM intake_steps WHERE request_id=? ORDER BY rowid`, r.ID)
	if err != nil {
		return m, err
	}
	for _, producer := range producers {
		if producer.OutputSHA == "" {
			continue
		}
		raw, err := s.blobs.Read(ctx, producer.OutputSHA, 256<<10)
		if err != nil {
			return m, err
		}
		m.Inputs = append(m.Inputs, model.IntakeInputRef{Kind: "output", ID: producer.ID, SHA: producer.OutputSHA, Bytes: int64(len(raw))})
	}
	drafts, err := intakeList[model.IntakeDraft](ctx, tx, `SELECT data FROM intake_drafts WHERE request_id=? ORDER BY revision`, r.ID)
	if err != nil {
		return m, err
	}
	for _, d := range drafts {
		ref, err := s.blobs.Put(ctx, []byte(d.CanonicalSource))
		if err != nil {
			return m, err
		}
		m.Inputs = append(m.Inputs, model.IntakeInputRef{Kind: "draft", ID: strconv.Itoa(d.Revision), SHA: ref.SHA, Bytes: ref.Bytes})
		metadata := d
		metadata.CanonicalSource = ""
		raw, err := json.Marshal(metadata)
		if err != nil {
			return m, err
		}
		ref, err = s.blobs.Put(ctx, raw)
		if err != nil {
			return m, err
		}
		m.Inputs = append(m.Inputs, model.IntakeInputRef{Kind: "draft_metadata", ID: strconv.Itoa(d.Revision), SHA: ref.SHA, Bytes: ref.Bytes})
	}
	seen := map[string]bool{}
	for _, sha := range bundles {
		if seen[sha] {
			return m, intakeError("invalid_request", "duplicate bundle disclosure")
		}
		seen[sha] = true
		bundle, err := s.intakeAuthorizeBundleTx(ctx, tx, r.OperatorID, r.Project, sha)
		if err != nil {
			return m, err
		}
		for _, f := range bundle.Files {
			m.Inputs = append(m.Inputs, model.IntakeInputRef{Kind: "bundle", ID: sha + "/" + f.Path, SHA: f.ContentSHA, Bytes: f.Bytes})
		}
	}
	capabilities, err := intakeCapabilities(c, r.Project)
	if err != nil {
		return m, err
	}
	capRef, err := s.blobs.Put(ctx, capabilities)
	if err != nil {
		return m, err
	}
	m.Inputs = append(m.Inputs, model.IntakeInputRef{Kind: "capabilities", ID: r.Project, SHA: capRef.SHA, Bytes: capRef.Bytes})
	if c.AstraModel == "" {
		return m, intakeError("invalid_contract", "intake drafting model is not configured")
	}
	m.Disclosure = model.IntakeDisclosure{PolicySHA: c.PolicyHash, ModelProviderIDs: []string{c.AstraModel}, BundleManifestSHAs: append([]string(nil), bundles...), PrivateMaterialToModels: true}
	m.Disclosure.ContextSHA = intakeManifestContextSHA(m)
	m.SHA = programHash(m)
	return m, nil
}

// Preserve the historical digest shape at both production and resolution.
// ContentVersion belongs to the enclosing manifest identity: an exact-context
// retry changes that version, not the disclosed inputs, providers or bundles.
func intakeManifestContextSHA(m model.IntakeManifest) string {
	return programHash(struct {
		Project   string
		Policy    string
		Inputs    []model.IntakeInputRef
		Providers []string
		Bundles   []string
	}{m.Project, m.PolicySHA, m.Inputs, m.Disclosure.ModelProviderIDs, m.Disclosure.BundleManifestSHAs})
}

// IntakeDisclosure offers only already-saved bytes. Saving text never requires
// model consent and never creates a step; intake.draft consumes this offer.
func (s *Store) IntakeDisclosure(ctx context.Context, c model.Config, operatorID, project, requestID string, bundles []string) (model.IntakeDisclosure, error) {
	if !c.Intake.Enabled {
		return model.IntakeDisclosure{}, intakeError("unsupported_action", "intake is disabled")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.IntakeDisclosure{}, err
	}
	defer tx.Rollback()
	r, err := intakeRequestTx(ctx, tx, requestID)
	if err != nil {
		return model.IntakeDisclosure{}, err
	}
	if r.OperatorID != operatorID || r.Project != project {
		return model.IntakeDisclosure{}, intakeError("project_forbidden", "request scope differs")
	}
	if err = operatorProjectTx(ctx, tx, operatorID, project); err != nil {
		return model.IntakeDisclosure{}, err
	}
	m, err := s.intakeManifestTx(ctx, tx, c, r, bundles)
	return m.Disclosure, err
}
func intakeConsent(m model.IntakeManifest, d model.IntakeDisclosure) error {
	if !d.PrivateMaterialToModels || programHash(m.Disclosure) != programHash(d) {
		return intakeError("disclosure_required", "exact current context and provider consent is required")
	}
	return nil
}

type intakeContextTurn struct {
	Sequence       int64                    `json:"sequence"`
	Role           string                   `json:"role"`
	Text           string                   `json:"text"`
	ProducerStepID string                   `json:"producer_step_id,omitempty"`
	Question       *model.IntakeQuestionRef `json:"question,omitempty"`
}

func intakeSafeTurn(t model.IntakeTurn) intakeContextTurn {
	return intakeContextTurn{t.Sequence, t.Role, t.Text, t.ProducerStepID, t.Question}
}
func intakeStepLoad(ctx context.Context, q programQuerier, column, id string) (model.IntakeStep, error) {
	return intakeLoad[model.IntakeStep](ctx, q, `SELECT data FROM intake_steps WHERE `+column+`=?`, id)
}
func intakePutStepTx(ctx context.Context, tx *sql.Tx, step model.IntakeStep, active bool) error {
	step.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(step)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE intake_steps SET data=?,active=? WHERE id=?`, raw, active, step.ID)
	return err
}
func intakeSupersedeTx(ctx context.Context, tx *sql.Tx, r model.IntakeRequest) error {
	if r.ActiveStepID == "" {
		return nil
	}
	step, err := intakeStepLoad(ctx, tx, "id", r.ActiveStepID)
	if err != nil {
		return err
	}
	step.Superseded = true
	step.State = "cancelling"
	if err = intakePutStepTx(ctx, tx, step, true); err != nil {
		return err
	}
	f, err := get(ctx, tx, step.FeatureID)
	if err != nil {
		return err
	}
	f.State = "cancelling"
	f.Reason = "intake content superseded"
	return put(ctx, tx, f)
}

func (s *Store) applyIntakeCommandTx(ctx context.Context, tx *sql.Tx, command model.TypedCommand, c model.Config, audit model.AuditContext) (any, error) {
	if !c.Intake.Enabled {
		return nil, intakeError("unsupported_action", "intake is disabled")
	}
	if err := validateAuditContext(audit); err != nil {
		return nil, err
	}
	payload, err := intake.CommandPayload(command)
	if err != nil {
		return nil, err
	}
	if command.Action == model.ActionRequestCreate {
		p := payload.(*model.RequestCreatePayload)
		if project, ok := c.Projects[p.Project]; !ok || !project.Trusted {
			return nil, intakeError("project_forbidden", "project is not enrolled")
		}
		if err = operatorProjectTx(ctx, tx, audit.OperatorID, p.Project); err != nil {
			return nil, err
		}
		if err = intakeValidateText(nil, p.Text, c); err != nil {
			return nil, err
		}
		if p.ParentRequestID != "" {
			parent, err := intakeRequestTx(ctx, tx, p.ParentRequestID)
			if err != nil {
				return nil, err
			}
			if parent.OperatorID != audit.OperatorID {
				return nil, intakeError("project_forbidden", "amendment parent belongs to another operator")
			}
		}
		now := time.Now().UTC()
		r := model.IntakeRequest{ID: ID(), OperatorID: audit.OperatorID, Project: p.Project, State: model.IntakeCollecting, ContentVersion: 1, CreationPolicySHA: c.PolicyHash, MaxDraftDispatches: intakeLimits(c).MaxDraftCalls, ParentRequestID: p.ParentRequestID, CreatedAt: now, UpdatedAt: now}
		if err = intakePutRequestTx(ctx, tx, r); err != nil {
			return nil, err
		}
		if err = intakeAppendTurnTx(ctx, tx, model.IntakeTurn{RequestID: r.ID, ClientMessageID: p.ClientMessageID, Role: "user", Text: p.Text, Audit: audit}); err != nil {
			return nil, err
		}
		if err = intakeEventTx(ctx, tx, &r, "created", "saved without model authority"); err != nil {
			return nil, err
		}
		return r, nil
	}
	r, err := intakeRequestTx(ctx, tx, command.RequestID)
	if err != nil {
		return nil, err
	}
	if err = intakeWritable(r, command.ExpectedContentVersion, audit.OperatorID); err != nil {
		return nil, err
	}
	if err = operatorProjectTx(ctx, tx, r.OperatorID, r.Project); err != nil {
		return nil, err
	}
	if project, ok := c.Projects[r.Project]; !ok || !project.Trusted {
		return nil, intakeError("project_forbidden", "project is not enrolled")
	}
	switch command.Action {
	case model.ActionTurnAppend:
		p := payload.(*model.TurnAppendPayload)
		previous, err := intakeLoad[model.IntakeTurn](ctx, tx, `SELECT data FROM intake_turns WHERE request_id=? AND client_message_id=?`, r.ID, p.ClientMessageID)
		if err == nil {
			if previous.Text != p.Text || programHash(previous.Question) != programHash(p.Question) {
				return nil, intakeError("idempotency_conflict", "client message ID already has different content")
			}
			return r, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		turns, err := intakeTurnsTx(ctx, tx, r.ID)
		if err != nil {
			return nil, err
		}
		if err = intakeValidateText(turns, p.Text, c); err != nil {
			return nil, err
		}
		waitingQuestions := r.State == model.IntakeNeedsAnswers
		if p.Question != nil {
			q, err := intakeLoad[model.IntakeQuestion](ctx, tx, `SELECT data FROM intake_questions WHERE id=? AND request_id=?`, p.Question.QuestionID, r.ID)
			if err != nil {
				return nil, err
			}
			producerID := ""
			for i := len(turns) - 1; i >= 0; i-- {
				if turns[i].ProducerStepID != "" {
					producerID = turns[i].ProducerStepID
					break
				}
			}
			if producerID == "" {
				return nil, intakeError("stale_revision", "question producer is no longer current")
			}
			producer, err := intakeStepLoad(ctx, tx, "id", producerID)
			if err != nil {
				return nil, err
			}
			if !waitingQuestions || q.ContentVersion != p.Question.ContentVersion || q.InputManifestSHA != p.Question.InputManifestSHA || q.OutputSHA != p.Question.OutputSHA || q.OutputSHA != producer.OutputSHA {
				return nil, intakeError("stale_revision", "question no longer belongs to current input")
			}
			var answered int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM intake_question_answers WHERE question_id=?`, q.ID).Scan(&answered); err != nil {
				return nil, err
			}
			if answered != 0 {
				return nil, intakeError("stale_revision", "question was already answered")
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO intake_question_answers(question_id,sequence) VALUES(?,?)`, q.ID, len(turns)+1); err != nil {
				return nil, err
			}
			waitingQuestions = false
			questions, err := intakeList[model.IntakeQuestion](ctx, tx, `SELECT q.data FROM intake_questions q LEFT JOIN intake_question_answers a ON a.question_id=q.id WHERE q.request_id=? AND a.question_id IS NULL`, r.ID)
			if err != nil {
				return nil, err
			}
			for _, question := range questions {
				if question.OutputSHA == q.OutputSHA {
					waitingQuestions = true
					break
				}
			}
		}
		if err = intakeSupersedeTx(ctx, tx, r); err != nil {
			return nil, err
		}
		r.ContentVersion++
		r.State = model.IntakeCollecting
		r.Wait = nil
		r.ContextManifest = model.IntakeManifest{}
		if waitingQuestions {
			r.State = model.IntakeNeedsAnswers
		}
		if err = intakeAppendTurnTx(ctx, tx, model.IntakeTurn{RequestID: r.ID, ClientMessageID: p.ClientMessageID, Role: "user", Text: p.Text, Question: p.Question, Audit: audit}); err != nil {
			return nil, err
		}
		if err = intakeEventTx(ctx, tx, &r, "turn_appended", "saved without model authority"); err != nil {
			return nil, err
		}
	case model.ActionIntakeDraft:
		p := payload.(*model.IntakeDraftPayload)
		if r.State == model.IntakeNeedsAnswers {
			return nil, intakeError("question_unresolved", "answer all current consequential questions before drafting")
		}
		if r.State != model.IntakeCollecting {
			return nil, intakeError("stale_subject", "save new content before requesting a new draft")
		}
		if r.LifetimeDispatches >= r.MaxDraftDispatches {
			return nil, intakeError("input_limit", "request lifetime draft budget exhausted")
		}
		manifest, err := s.intakeManifestTx(ctx, tx, c, r, p.Disclosure.BundleManifestSHAs)
		if err != nil {
			return nil, err
		}
		if err = intakeConsent(manifest, p.Disclosure); err != nil {
			return nil, err
		}
		r.ContextManifest = manifest
		r.State = model.IntakeDraftQueued
		r.Wait = nil
		if err = intakeGrantDraftTx(ctx, tx, r, audit); err != nil {
			return nil, err
		}
		if err = intakeEventTx(ctx, tx, &r, "draft_requested", "exact saved context consent committed"); err != nil {
			return nil, err
		}
	case model.ActionDraftReplace:
		if r.State == model.IntakeNeedsAnswers {
			return nil, intakeError("question_unresolved", "answer all current consequential questions before replacing the draft")
		}
		p := payload.(*model.DraftReplacePayload)
		if len(p.CanonicalSource) > intakeLimits(c).MaxCanonicalBytes {
			return nil, intakeError("input_limit", "canonical source exceeds limit")
		}
		canonical, err := s.intakeCanonicalTx(ctx, tx, p.Route, p.Format, []byte(p.CanonicalSource), r, p.BundleManifestSHA, c)
		if err != nil {
			return nil, err
		}
		if err = intakeSupersedeTx(ctx, tx, r); err != nil {
			return nil, err
		}
		r.ContentVersion++
		draft := intakeDraftFromCanonical(r, canonical, "", "", "")
		draft.PolicySHA = c.PolicyHash
		draft.BundleManifestSHA = p.BundleManifestSHA
		if err = s.intakeInsertDraftTx(ctx, tx, &r, &draft, true); err != nil {
			return nil, err
		}
		r.State = model.IntakeReviewReady
		r.Wait = nil
		if err = intakeEventTx(ctx, tx, &r, "draft_replaced", ""); err != nil {
			return nil, err
		}
	case model.ActionIntakeCancel:
		r.ContentVersion++
		r.State = model.IntakeCancelled
		if r.ActiveStepID != "" {
			r.State = model.IntakeCancelling
			if err = intakeSupersedeTx(ctx, tx, r); err != nil {
				return nil, err
			}
		}
		if err = intakeEventTx(ctx, tx, &r, "cancel_requested", ""); err != nil {
			return nil, err
		}
	case model.ActionIntakeRetry:
		if r.ActiveStepID != "" {
			return nil, intakeError("attempt_unsettled", "previous draft dispatch is not settled")
		}
		if r.State != model.IntakeBlocked {
			return nil, intakeError("stale_subject", "request is not blocked")
		}
		if r.LifetimeDispatches >= r.MaxDraftDispatches {
			return nil, intakeError("input_limit", "request lifetime draft budget exhausted")
		}
		if r.ContextManifest.PolicySHA != c.PolicyHash {
			return nil, intakeError("stale_policy", "save new content and disclose it after a policy change")
		}
		r.ContentVersion++
		r.ContextManifest.ContentVersion = r.ContentVersion
		r.ContextManifest.SHA = ""
		r.ContextManifest.SHA = programHash(r.ContextManifest)
		r.State = model.IntakeDraftQueued
		r.Wait = nil
		if err = intakeGrantDraftTx(ctx, tx, r, audit); err != nil {
			return nil, err
		}
		if err = intakeEventTx(ctx, tx, &r, "retry_requested", ""); err != nil {
			return nil, err
		}
	case model.ActionIntakeApproveQueue:
		p := payload.(*model.IntakeApprovePayload)
		if r.State != model.IntakeReviewReady || r.ActiveStepID != "" || p.DraftRevision != r.LatestDraftRevision {
			return nil, intakeError("stale_revision", "current settled draft review is required")
		}
		draft, err := intakeLoad[model.IntakeDraft](ctx, tx, `SELECT data FROM intake_drafts WHERE request_id=? AND revision=?`, r.ID, p.DraftRevision)
		if err != nil {
			return nil, err
		}
		if draft.ContentVersion != r.ContentVersion || draft.SourceSHA != p.SourceSHA || draft.DomainRevision != p.DomainRevision {
			return nil, intakeError("stale_revision", "draft content changed")
		}
		if draft.PolicySHA != c.PolicyHash {
			return nil, intakeError("stale_policy", "draft policy changed")
		}
		canonical, err := s.intakeCanonicalTx(ctx, tx, draft.Route, draft.Format, []byte(draft.CanonicalSource), r, draft.BundleManifestSHA, c)
		if err != nil {
			return nil, err
		}
		if canonical.SourceSHA != draft.SourceSHA || canonical.DomainRevision != draft.DomainRevision {
			return nil, intakeError("stale_revision", "canonical draft changed")
		}
		link := model.IntakeLink{RequestID: r.ID, DraftRevision: draft.Revision, CommandID: audit.CommandID, CreatedAt: time.Now().UTC()}
		if canonical.Feature != nil {
			existing, lookupErr := get(ctx, tx, canonical.Feature.ID)
			if lookupErr == nil {
				if existing.Project != r.Project || existing.Revision != canonical.DomainRevision {
					return nil, intakeError("stale_subject", "feature ID already belongs to another immutable contract")
				}
				if existing.State == "cancelled" || existing.State == "cancelling" {
					return nil, intakeError("stale_subject", "cancelled work requires a new contract ID")
				}
			} else if !errors.Is(lookupErr, sql.ErrNoRows) {
				return nil, lookupErr
			}
			feature, err := importFeatureTx(ctx, tx, *canonical.Feature, true)
			if err != nil {
				return nil, err
			}
			// Linking identical work never resurrects it, replaces its current revision,
			// or creates a second attempt. Only a proposed source receives a new grant.
			if feature.State == "proposed" {
				if err = s.approveFeatureTx(ctx, tx, feature.ID, feature.Revision, c.PolicyHash, audit); err != nil {
					return nil, err
				}
			}
			link.SubjectKind = draft.Route
			link.SubjectID = feature.ID
			link.SubjectRevision = feature.Revision
		} else if canonical.Program != nil {
			var bundle model.ProgramBundle
			if draft.BundleManifestSHA != "" {
				bundle, err = s.intakeAuthorizeBundleTx(ctx, tx, r.OperatorID, r.Project, draft.BundleManifestSHA)
				if err != nil {
					return nil, err
				}
			}
			revision, err := s.importProgramTx(ctx, tx, *canonical.Program, bundle)
			if err != nil {
				return nil, err
			}
			if revision.RevisionHash != draft.DomainRevision {
				return nil, intakeError("stale_revision", "program revision changed")
			}
			if err = s.approveProgramTx(ctx, tx, revision.ProgramID, revision.RevisionHash, c.PolicyHash, audit); err != nil {
				return nil, err
			}
			link.SubjectKind = "program"
			link.SubjectID = revision.ProgramID
			link.SubjectRevision = revision.RevisionHash
		} else {
			return nil, intakeError("invalid_contract", "draft has no canonical subject")
		}
		raw, err := json.Marshal(link)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO intake_links(request_id,draft_revision,data) VALUES(?,?,?)`, r.ID, draft.Revision, raw); err != nil {
			return nil, err
		}
		r.ContentVersion++
		r.State = model.IntakeSubmitted
		if err = intakeEventTx(ctx, tx, &r, "submitted", link.SubjectID); err != nil {
			return nil, err
		}
	default:
		return nil, intakeError("unsupported_action", "unsupported intake command")
	}
	return r, nil
}

func intakeDraftFromCanonical(r model.IntakeRequest, c intake.CanonicalDraft, stepID, inputSHA, outputSHA string) model.IntakeDraft {
	return model.IntakeDraft{RequestID: r.ID, ContentVersion: r.ContentVersion, Route: c.Route, Format: c.Format, CanonicalSource: c.CanonicalSource, SourceSHA: c.SourceSHA, CanonicalSHA: c.CanonicalSHA, DomainRevision: c.DomainRevision, InputManifestSHA: inputSHA, OutputSHA: outputSHA, PolicySHA: r.ContextManifest.PolicySHA, StepID: stepID, Assumptions: []string{}, UnresolvedRisks: []string{}, Findings: []model.IntakeFinding{}, CreatedAt: time.Now().UTC()}
}
func (s *Store) intakeInsertDraftTx(ctx context.Context, tx *sql.Tx, r *model.IntakeRequest, d *model.IntakeDraft, current bool) error {
	blob, err := s.blobs.Put(ctx, []byte(d.CanonicalSource))
	if err != nil {
		return err
	}
	if blob.SHA != d.SourceSHA {
		return errors.New("intake source digest differs from exact canonical bytes")
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM intake_drafts WHERE request_id=?`, r.ID).Scan(&d.Revision); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_drafts(request_id,revision,data) VALUES(?,?,?)`, r.ID, d.Revision, raw); err != nil {
		return err
	}
	if current {
		r.LatestDraftRevision = d.Revision
	}
	return nil
}

func (s *Store) GetIntakeStep(ctx context.Context, id string) (model.IntakeStep, error) {
	return intakeStepLoad(ctx, s.db, "id", id)
}
func (s *Store) IntakeStepForFeature(ctx context.Context, id string) (model.IntakeStep, error) {
	return intakeStepLoad(ctx, s.db, "feature_id", id)
}
func (s *Store) CreateIntakeStep(ctx context.Context, id string, version int, c model.Config) (model.IntakeStep, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.IntakeStep{}, err
	}
	defer tx.Rollback()
	step, err := s.createIntakeStepTx(ctx, tx, id, version, c)
	if err != nil {
		return step, err
	}
	return step, tx.Commit()
}
func (s *Store) createIntakeStepTx(ctx context.Context, tx *sql.Tx, id string, version int, c model.Config) (model.IntakeStep, error) {
	r, err := intakeRequestTx(ctx, tx, id)
	if err != nil {
		return model.IntakeStep{}, err
	}
	if !c.Intake.Enabled {
		return model.IntakeStep{}, intakeError("unsupported_action", "intake is disabled")
	}
	if r.ContentVersion != version || r.State != model.IntakeDraftQueued {
		return model.IntakeStep{}, intakeError("stale_revision", "request is no longer queued at this content version")
	}
	if r.ActiveStepID != "" {
		return model.IntakeStep{}, intakeError("attempt_unsettled", "previous intake step still owns execution")
	}
	if project, ok := c.Projects[r.Project]; !ok || !project.Trusted {
		return model.IntakeStep{}, intakeError("project_forbidden", "project is no longer enrolled")
	}
	if err = intakeDraftAuthorityTx(ctx, tx, r, r.ContextManifest); err != nil {
		return model.IntakeStep{}, err
	}
	if r.ContextManifest.PolicySHA != c.PolicyHash || len(r.ContextManifest.Disclosure.ModelProviderIDs) != 1 || r.ContextManifest.Disclosure.ModelProviderIDs[0] != c.AstraModel {
		return model.IntakeStep{}, intakeError("stale_policy", "draft context or provider consent changed")
	}
	if r.LifetimeDispatches >= r.MaxDraftDispatches {
		return model.IntakeStep{}, intakeError("input_limit", "request lifetime draft budget exhausted")
	}
	now := time.Now().UTC()
	step := model.IntakeStep{ID: ID(), RequestID: r.ID, ContentVersion: r.ContentVersion, Manifest: r.ContextManifest, InputManifestSHA: r.ContextManifest.SHA, PolicySHA: c.PolicyHash, Model: c.AstraModel, State: "ready", CreatedAt: now, UpdatedAt: now}
	step.FeatureID = model.IntakeFeaturePrefix + step.ID
	feature := model.Feature{ID: step.FeatureID, Project: r.Project, Title: "Draft intake " + r.ID, Kind: model.IntakeFeatureKind, Risk: "normal", CompletionTarget: "reviewed", IntakeStepID: step.ID, PolicyHash: c.PolicyHash, State: "ready", CreatedAt: now, Body: "Draft the immutable disclosed intake context " + step.InputManifestSHA, Acceptance: []model.Acceptance{{ID: "intake-output", Scenario: "Read only the exact disclosed context manifest " + step.InputManifestSHA, Expected: "Return a complete typed questions, canonical draft, or blocked result; the controller validates its request, version, project, policy, producer attempt and output before publishing a current review."}}}
	feature.Revision = intake.Revision(feature)
	if err = intake.Validate(feature); err != nil {
		return step, err
	}
	raw, err := json.Marshal(step)
	if err != nil {
		return step, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_steps(id,request_id,feature_id,active,data) VALUES(?,?,?,1,?)`, step.ID, r.ID, feature.ID, raw); err != nil {
		return step, err
	}
	if err = put(ctx, tx, feature); err != nil {
		return step, err
	}
	raw, err = json.Marshal(feature)
	if err != nil {
		return step, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, feature.ID, feature.Revision, raw); err != nil {
		return step, err
	}
	r.ActiveStepID = step.ID
	r.State = model.IntakeDraftQueued
	r.Wait = nil
	if err = intakeEventTx(ctx, tx, &r, "step_queued", step.ID); err != nil {
		return step, err
	}
	wait, err := s.admissionGate(ctx, tx, feature, c.PolicyHash, c.MaxAttempts, c.MaxConcurrent, c.Projects[r.Project].MaxConcurrent)
	if err != nil {
		return step, err
	}
	if err = intakeSetProgressTx(ctx, tx, &r, model.IntakeDraftQueued, wait); err != nil {
		return step, err
	}
	return step, nil
}
func (s *Store) IntakeStepInputs(ctx context.Context, id string) (model.IntakeRequest, []model.IntakeTurn, []model.IntakeDraft, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.IntakeRequest{}, nil, nil, err
	}
	defer tx.Rollback()
	step, err := intakeStepLoad(ctx, tx, "id", id)
	if err != nil {
		return model.IntakeRequest{}, nil, nil, err
	}
	live, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return live, nil, nil, err
	}
	r := model.IntakeRequest{ID: step.RequestID, OperatorID: live.OperatorID, Project: live.Project, ContentVersion: step.ContentVersion, ContextManifest: step.Manifest, CreationPolicySHA: step.PolicySHA, MaxDraftDispatches: live.MaxDraftDispatches}
	if step.Manifest.ContentVersion != step.ContentVersion || step.Manifest.SHA != step.InputManifestSHA || step.Manifest.PolicySHA != step.PolicySHA {
		return r, nil, nil, errors.New("step context association differs")
	}
	turns, drafts, err := s.intakeResolveManifestTx(ctx, tx, r, step.Manifest, nil)
	if err != nil {
		return r, nil, nil, err
	}
	for _, d := range drafts {
		if d.Revision > r.LatestDraftRevision {
			r.LatestDraftRevision = d.Revision
		}
	}
	return r, turns, drafts, tx.Commit()
}

func intakeFeatureGateTx(ctx context.Context, tx *sql.Tx, f model.Feature, policy string) error {
	if f.IntakeStepID == "" {
		if f.Kind == model.IntakeFeatureKind || strings.HasPrefix(f.ID, model.IntakeFeaturePrefix) {
			return intakeError("invalid_contract", "intake feature association is missing")
		}
		return nil
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	if err = intakeManifestIdentity(step.Manifest); err != nil {
		return err
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	queued := r.State == model.IntakeDraftQueued && step.State == "ready" && step.AttemptID == "" && f.State == "ready"
	if step.FeatureID != f.ID || step.PolicySHA != policy || step.PolicySHA != step.Manifest.PolicySHA || f.PolicyHash != policy || step.InputManifestSHA != step.Manifest.SHA || r.ContextManifest.SHA != step.InputManifestSHA || r.ActiveStepID != step.ID || r.ContentVersion != step.ContentVersion || step.Superseded || r.State != model.IntakeDrafting && !queued {
		return waiting("intake execution context is superseded")
	}
	if err = intakeDraftAuthorityTx(ctx, tx, r, step.Manifest); err != nil {
		var denied *model.ProgramError
		if errors.As(err, &denied) {
			return waiting(denied.Message)
		}
		return err
	}
	return nil
}
func reserveIntakeCallTx(ctx context.Context, tx *sql.Tx, f model.Feature, a model.Attempt, dispatchKey string) error {
	if f.IntakeStepID == "" {
		return nil
	}
	if err := intakeFeatureGateTx(ctx, tx, f, f.PolicyHash); err != nil {
		return err
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM intake_dispatches WHERE attempt_id=? AND dispatch_key=?`, a.ID, dispatchKey).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return nil
	}
	if r.LifetimeDispatches >= r.MaxDraftDispatches {
		return waiting("request lifetime draft budget exhausted")
	}
	if step.AttemptID != "" && step.AttemptID != a.ID {
		return intakeError("attempt_unsettled", "intake step belongs to another attempt")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_dispatches(request_id,attempt_id,dispatch_key) VALUES(?,?,?)`, r.ID, a.ID, dispatchKey); err != nil {
		return err
	}
	r.LifetimeDispatches++
	step.AttemptID = a.ID
	step.ThreadID = a.ThreadID
	step.State = "running"
	if err = intakePutStepTx(ctx, tx, step, true); err != nil {
		return err
	}
	return intakeEventTx(ctx, tx, &r, "dispatch_reserved", step.ID)
}
func (s *Store) BindIntakeAttempt(ctx context.Context, a model.Attempt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = intakeBindAttemptTx(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}
func intakeBindAttemptTx(ctx context.Context, tx *sql.Tx, a model.Attempt) error {
	feature, old, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if old.ThreadID != a.ThreadID {
		return intakeError("stale_subject", "thread must match the durable attempt before binding")
	}
	if feature.IntakeStepID == "" {
		return intakeError("invalid_contract", "attempt is not intake work")
	}
	step, err := intakeStepLoad(ctx, tx, "id", feature.IntakeStepID)
	if err != nil {
		return err
	}
	if step.AttemptID != "" && step.AttemptID != a.ID {
		return intakeError("attempt_unsettled", "step already bound to another attempt")
	}
	if step.ThreadID != "" && step.ThreadID != a.ThreadID {
		return intakeError("stale_subject", "step thread association changed")
	}
	step.AttemptID = a.ID
	step.ThreadID = a.ThreadID
	return intakePutStepTx(ctx, tx, step, true)
}
func (s *Store) ValidateIntakeAttempt(ctx context.Context, a model.Attempt, c model.Config) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, _, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if !c.Intake.Enabled || f.IntakeStepID == "" {
		return intakeError("stale_policy", "intake drafting is disabled")
	}
	if err = intakeFeatureGateTx(ctx, tx, f, c.PolicyHash); err != nil {
		return err
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	if step.Model != c.AstraModel {
		return intakeError("stale_policy", "draft provider changed")
	}
	if step.AttemptID != "" && step.AttemptID != a.ID {
		return intakeError("stale_subject", "step attempt changed")
	}
	return nil
}

// This hook runs only when the ordinary feature's external ownership is settled.
func settleIntakeFeatureTx(ctx context.Context, tx *sql.Tx, f model.Feature, a model.Attempt, state string) error {
	if f.IntakeStepID == "" {
		return nil
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	if step.AttemptID != "" && step.AttemptID != a.ID {
		return intakeError("stale_subject", "settlement attempt mismatch")
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	if step.State == "complete" || step.State == "cancelled" || step.State == "blocked" || step.State == "superseded" {
		return nil
	}
	step.AttemptID = a.ID
	step.ThreadID = a.ThreadID
	if step.Superseded {
		step.State = "superseded"
	} else if state == "cancelled" {
		step.State = "cancelled"
	} else {
		step.State = "blocked"
	}
	if err = intakePutStepTx(ctx, tx, step, false); err != nil {
		return err
	}
	if r.ActiveStepID == step.ID {
		r.ActiveStepID = ""
		if r.State == model.IntakeCancelling {
			r.State = model.IntakeCancelled
		} else if r.ContentVersion == step.ContentVersion {
			r.State = model.IntakeBlocked
			r.Wait = &model.WaitReason{Code: "draft_failed", Detail: "intake draft attempt settled without a valid current result"}
		}
	}
	return intakeEventTx(ctx, tx, &r, "step_settled", step.ID)
}

func (s *Store) intakeCompleteAttemptTx(ctx context.Context, tx *sql.Tx, f model.Feature, a model.Attempt, state, reason string) error {
	if a.Stage == "uncertain" {
		return intakeError("attempt_unsettled", "uncertain ownership cannot be settled")
	}
	a.State = "complete"
	a.Stage = "complete"
	a.Wait = nil
	return s.saveTx(ctx, tx, a, state, reason, "", false)
}
func (s *Store) ApplyIntakeResult(ctx context.Context, a model.Attempt, supplied model.IntakeStep, result model.IntakeModelOutput, c model.Config) error {
	raw := []byte(a.Summary)
	if len(raw) == 0 {
		return intakeError("invalid_contract", "exact worker output is required")
	}
	parsed, err := intake.ParseModelOutput(raw, intakeLimits(c).MaxOutputBytes)
	if err != nil {
		return err
	}
	if programHash(parsed) != programHash(result) {
		return intakeError("invalid_contract", "parsed result differs from exact worker output")
	}
	blob, err := s.blobs.Put(ctx, raw)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.applyIntakeResultTx(ctx, tx, a, supplied, parsed, blob.SHA, c); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) applyIntakeResultTx(ctx context.Context, tx *sql.Tx, a model.Attempt, supplied model.IntakeStep, parsed model.IntakeModelOutput, outputSHA string, c model.Config) error {
	step, err := intakeStepLoad(ctx, tx, "id", supplied.ID)
	if err != nil {
		return err
	}
	if step.RequestID != supplied.RequestID || step.ContentVersion != supplied.ContentVersion || step.InputManifestSHA != supplied.InputManifestSHA || step.FeatureID != a.FeatureID || step.AttemptID != a.ID || step.ThreadID != a.ThreadID {
		return intakeError("stale_subject", "result producer association mismatch")
	}
	if step.OutputSHA != "" {
		if step.OutputSHA == outputSHA {
			return nil
		}
		return intakeError("idempotency_conflict", "step already has another immutable output")
	}
	feature, old, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if old.ThreadID != a.ThreadID {
		return intakeError("stale_subject", "output thread differs from durable attempt")
	}
	if feature.IntakeStepID != step.ID {
		return intakeError("stale_subject", "feature relation changed")
	}
	if old.Stage == "uncertain" {
		return intakeError("attempt_unsettled", "uncertain attempt must be reconciled")
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	current := !step.Superseded && r.ActiveStepID == step.ID && r.ContentVersion == step.ContentVersion && r.State == model.IntakeDrafting && step.PolicySHA == c.PolicyHash && c.Intake.Enabled && step.Model == c.AstraModel
	if err = intakeDraftAuthorityTx(ctx, tx, r, step.Manifest); err != nil {
		var denied *model.ProgramError
		if !errors.As(err, &denied) {
			return err
		}
		current = false
	}
	step.OutputSHA = outputSHA
	step.ThreadID = a.ThreadID
	step.State = "complete"
	if !current {
		step.Superseded = true
		step.State = "superseded"
	}
	turns, err := intakeTurnsTx(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	outputLimitErr := intakeValidateText(turns, parsed.Message, c)
	if outputLimitErr == nil {
		if err = intakeAppendTurnTx(ctx, tx, model.IntakeTurn{RequestID: r.ID, Role: "model", Text: parsed.Message, ProducerStepID: step.ID}); err != nil {
			return err
		}
	}
	switch parsed.Type {
	case "draft":
		snapshot := r
		snapshot.ContentVersion = step.ContentVersion
		snapshot.ContextManifest = step.Manifest
		canonical, validationErr := s.intakeCanonicalCommittedTx(ctx, tx, parsed.Route, parsed.Format, []byte(parsed.CanonicalSource), snapshot, c)
		d := intakeDraftFromCanonical(snapshot, canonical, step.ID, step.InputManifestSHA, outputSHA)
		d.Assumptions = parsed.Assumptions
		d.UnresolvedRisks = parsed.UnresolvedRisks
		if parsed.Route == model.IntakeRouteProgram && len(step.Manifest.Disclosure.BundleManifestSHAs) == 1 {
			d.BundleManifestSHA = step.Manifest.Disclosure.BundleManifestSHAs[0]
		}
		if validationErr != nil {
			d.Findings = append(d.Findings, model.IntakeFinding{Code: "invalid_contract", Severity: "error", Detail: validationErr.Error()})
		}
		if validationErr != nil {
			var domain *model.ProgramError
			if !errors.As(validationErr, &domain) && !errors.Is(validationErr, ErrProjectForbidden) {
				return validationErr
			}
		}
		if outputLimitErr != nil {
			d.Findings = append(d.Findings, model.IntakeFinding{Code: "input_limit", Severity: "error", Detail: "model message exceeds request transcript limits"})
		}
		if err = s.intakeInsertDraftTx(ctx, tx, &r, &d, current); err != nil {
			return err
		}
		if current {
			r.State = model.IntakeReviewReady
			r.Wait = nil
			if validationErr != nil {
				r.State = model.IntakeBlocked
				r.Wait = &model.WaitReason{Code: "invalid_contract", Detail: "draft canonical contract failed validation; inspect draft findings"}
			}
		}
	case "questions":
		for _, item := range parsed.Questions {
			question := model.IntakeQuestion{ID: ID(), RequestID: r.ID, ContentVersion: step.ContentVersion, InputManifestSHA: step.InputManifestSHA, OutputSHA: outputSHA, LocalID: item.LocalID, Text: item.Text, Consequence: item.Consequence, AnswerType: item.AnswerType, Choices: item.Choices}
			data, err := json.Marshal(question)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO intake_questions(id,request_id,data) VALUES(?,?,?)`, question.ID, r.ID, data); err != nil {
				return err
			}
		}
		if current {
			r.State = model.IntakeNeedsAnswers
			r.Wait = nil
		}
	case "blocked":
		if current {
			r.State = model.IntakeBlocked
			r.Wait = &model.WaitReason{Code: "prerequisite_missing", Detail: strings.Join(parsed.MissingPrerequisites, "; ")}
		}
	}
	if current && outputLimitErr != nil {
		r.State = model.IntakeBlocked
		r.Wait = &model.WaitReason{Code: "input_limit", Detail: "model transcript exceeds request limits; exact output is retained in step history"}
	}
	if !current && r.ContentVersion == step.ContentVersion && r.State == model.IntakeDrafting {
		r.State = model.IntakeBlocked
		r.Wait = &model.WaitReason{Code: "stale_policy", Detail: "draft result is historical because current authority or policy changed"}
	}
	if r.ActiveStepID == step.ID {
		r.ActiveStepID = ""
		if r.State == model.IntakeCancelling {
			r.State = model.IntakeCancelled
		}
	}
	if err = intakePutStepTx(ctx, tx, step, false); err != nil {
		return err
	}
	state := "reviewed"
	if r.State == model.IntakeBlocked {
		state = "blocked"
	}
	if !current {
		state = "cancelled"
	}
	if err = s.intakeCompleteAttemptTx(ctx, tx, feature, a, state, ""); err != nil {
		return err
	}
	return intakeEventTx(ctx, tx, &r, "result_recorded", step.ID)
}
func (s *Store) FailIntakeStep(ctx context.Context, a model.Attempt, supplied model.IntakeStep, reason string, cancelled bool) error {
	var outputSHA string
	if a.Summary != "" {
		if len(a.Summary) > 256<<10 {
			return intakeError("input_limit", "intake output exceeds byte limit")
		}
		ref, err := s.blobs.Put(ctx, []byte(a.Summary))
		if err != nil {
			return err
		}
		outputSHA = ref.SHA
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.failIntakeStepTx(ctx, tx, a, supplied, reason, cancelled, outputSHA); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) failIntakeStepTx(ctx context.Context, tx *sql.Tx, a model.Attempt, supplied model.IntakeStep, reason string, cancelled bool, outputSHA string) error {
	step, err := intakeStepLoad(ctx, tx, "id", supplied.ID)
	if err != nil {
		return err
	}
	if step.FeatureID != a.FeatureID || (step.AttemptID != "" && step.AttemptID != a.ID) {
		return intakeError("stale_subject", "failure producer association mismatch")
	}
	if outputSHA != "" && step.OutputSHA != "" && step.OutputSHA != outputSHA {
		return intakeError("idempotency_conflict", "step output is immutable")
	}
	if step.State == "complete" || step.State == "blocked" || step.State == "cancelled" || step.State == "superseded" {
		return nil
	}
	f, old, err := validateAttempt(ctx, tx, a)
	if err != nil {
		return err
	}
	if old.ThreadID != a.ThreadID || (step.ThreadID != "" && step.ThreadID != a.ThreadID) {
		return intakeError("stale_subject", "failure thread differs from durable producer")
	}
	if old.Stage == "uncertain" {
		if a.Stage != "intake_running" || a.ThreadID == "" || a.ThreadID != old.ThreadID || a.ThreadID != step.ThreadID {
			return intakeError("attempt_unsettled", "uncertain attempt requires a certified terminal thread observation")
		}
		// The trusted engine calls this only after Stop/Poll proves this exact
		// durable thread terminal. Clear uncertainty inside the settlement Tx.
		old.Stage = "intake_running"
		old.State = "running"
		raw, err := json.Marshal(old)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE attempts SET data=? WHERE id=? AND active=1`, raw, a.ID); err != nil {
			return err
		}
	}
	if outputSHA != "" {
		if step.OutputSHA != "" && step.OutputSHA != outputSHA {
			return intakeError("idempotency_conflict", "step output is immutable")
		}
		step.OutputSHA = outputSHA
	}
	step.AttemptID = a.ID
	step.ThreadID = a.ThreadID
	if err = intakePutStepTx(ctx, tx, step, true); err != nil {
		return err
	}
	state := "blocked"
	if cancelled {
		state = "cancelled"
	}
	if err = s.intakeCompleteAttemptTx(ctx, tx, f, a, state, reason); err != nil {
		return err
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	if r.State == model.IntakeBlocked {
		r.Wait = &model.WaitReason{Code: "draft_failed", Detail: reason}
		if err = intakeEventTx(ctx, tx, &r, "draft_failed", reason); err != nil {
			return err
		}
	}
	return nil
}

// intakeResolveManifestTx is the single authority boundary for model inputs and
// review reconstruction. A nil config reads only an already-committed grant;
// a new review must instead re-establish live bundle access and capabilities.
func (s *Store) intakeResolveManifestTx(ctx context.Context, tx *sql.Tx, r model.IntakeRequest, m model.IntakeManifest, cfg *model.Config) ([]model.IntakeTurn, []model.IntakeDraft, error) {
	// These records are already persisted authority, not untrusted model source.
	// Keep integrity failures raw so settlement cannot turn corruption into a
	// domain finding and release the attempt's external ownership.
	invalid := func() error { return errors.New("immutable intake context differs") }
	if err := intakeManifestIdentity(m); err != nil {
		return nil, nil, err
	}
	if m.RequestID != r.ID || m.Project != r.Project || m.ContentVersion > r.ContentVersion || m.PolicySHA != m.Disclosure.PolicySHA || !m.Disclosure.PrivateMaterialToModels {
		return nil, nil, invalid()
	}
	if intakeManifestContextSHA(m) != m.Disclosure.ContextSHA || len(m.Disclosure.ModelProviderIDs) != 1 {
		return nil, nil, invalid()
	}
	if cfg == nil {
		if err := intakeDraftAuthorityTx(ctx, tx, r, m); err != nil {
			return nil, nil, err
		}
	} else if m.PolicySHA != cfg.PolicyHash || m.Disclosure.ModelProviderIDs[0] != cfg.AstraModel {
		return nil, nil, intakeError("stale_policy", "request disclosure policy or provider changed")
	}
	members := map[string]model.ProgramBundleFile{}
	bundles := map[string]bool{}
	for _, sha := range m.Disclosure.BundleManifestSHAs {
		if !intakeBundleSHAValid(sha) || bundles[sha] {
			return nil, nil, invalid()
		}
		bundles[sha] = true
		var bundle model.ProgramBundle
		var err error
		if cfg == nil {
			bundle, err = s.blobs.LoadBundle(ctx, sha)
		} else {
			bundle, err = s.intakeAuthorizeBundleTx(ctx, tx, r.OperatorID, r.Project, sha)
		}
		if err != nil {
			var domain *model.ProgramError
			if errors.As(err, &domain) && domain.Code == "evidence_invalid" {
				return nil, nil, fmt.Errorf("immutable intake bundle context differs: %v", err)
			}
			return nil, nil, err
		}
		if bundle.ManifestSHA != sha {
			return nil, nil, invalid()
		}
		for _, f := range bundle.Files {
			members[sha+"/"+f.Path] = f
		}
	}
	turns := []model.IntakeTurn{}
	drafts := []model.IntakeDraft{}
	seen := map[string]bool{}
	draftSources, draftMetadata := map[string]bool{}, map[string]bool{}
	for _, ref := range m.Inputs {
		key := ref.Kind + "/" + ref.ID
		if ref.ID == "" || seen[key] || ref.Bytes < 0 || !intakeBundleSHAValid(ref.SHA) {
			return nil, nil, invalid()
		}
		seen[key] = true
		if ref.Kind == "bundle" {
			f, ok := members[ref.ID]
			if !ok || f.ContentSHA != ref.SHA || f.Bytes != ref.Bytes {
				return nil, nil, invalid()
			}
			if err := s.intakeVerifyBundleFile(ctx, f, false); err != nil {
				return nil, nil, err
			}
			delete(members, ref.ID)
			continue
		}
		raw, err := s.blobs.Read(ctx, ref.SHA, ref.Bytes)
		if err != nil {
			return nil, nil, err
		}
		if int64(len(raw)) != ref.Bytes || intake.SHA256(raw) != ref.SHA {
			return nil, nil, invalid()
		}
		var exact []byte
		switch ref.Kind {
		case "turn":
			seq, e := strconv.ParseInt(ref.ID, 10, 64)
			if e != nil || seq <= 0 || strconv.FormatInt(seq, 10) != ref.ID {
				return nil, nil, invalid()
			}
			t, e := intakeLoad[model.IntakeTurn](ctx, tx, `SELECT data FROM intake_turns WHERE request_id=? AND sequence=?`, r.ID, seq)
			if e != nil {
				return nil, nil, e
			}
			if t.RequestID != r.ID || t.Sequence != seq {
				return nil, nil, invalid()
			}
			exact, err = json.Marshal(intakeSafeTurn(t))
			turns = append(turns, t)
		case "draft", "draft_metadata":
			revision, e := strconv.Atoi(ref.ID)
			if e != nil || revision <= 0 || strconv.Itoa(revision) != ref.ID {
				return nil, nil, invalid()
			}
			d, e := intakeLoad[model.IntakeDraft](ctx, tx, `SELECT data FROM intake_drafts WHERE request_id=? AND revision=?`, r.ID, revision)
			if e != nil {
				return nil, nil, e
			}
			if d.RequestID != r.ID || d.Revision != revision || intake.SHA256([]byte(d.CanonicalSource)) != d.SourceSHA {
				return nil, nil, invalid()
			}
			if ref.Kind == "draft" {
				exact = []byte(d.CanonicalSource)
				draftSources[ref.ID] = true
				drafts = append(drafts, d)
			} else {
				d.CanonicalSource = ""
				exact, err = json.Marshal(d)
				draftMetadata[ref.ID] = true
			}
		case "output":
			producer, e := intakeStepLoad(ctx, tx, "id", ref.ID)
			if e != nil {
				return nil, nil, e
			}
			if producer.ID != ref.ID || producer.RequestID != r.ID || producer.OutputSHA != ref.SHA || producer.InputManifestSHA != producer.Manifest.SHA || producer.Manifest.RequestID != r.ID || producer.Manifest.ContentVersion != producer.ContentVersion || producer.ContentVersion > m.ContentVersion {
				return nil, nil, invalid()
			}
			if e = intakeDraftAuthorityTx(ctx, tx, r, producer.Manifest); e != nil {
				return nil, nil, e
			}
			attempt, e := intakeLoad[model.Attempt](ctx, tx, `SELECT data FROM attempts WHERE id=?`, producer.AttemptID)
			if e != nil {
				return nil, nil, e
			}
			if attempt.ID != producer.AttemptID || attempt.FeatureID != producer.FeatureID || producer.ThreadID == "" || attempt.Summary != string(raw) {
				return nil, nil, invalid()
			}
			// Terminal cleanup may clear the attempt's current thread pointer.
			// The immutable session relation, not that pointer, binds provenance.
			var threadID string
			if e = tx.QueryRowContext(ctx, `SELECT thread_id FROM sessions WHERE attempt_id=? AND thread_id=?`, producer.AttemptID, producer.ThreadID).Scan(&threadID); e != nil {
				return nil, nil, e
			}
			if _, e = intake.ParseModelOutput(raw, 256<<10); e != nil {
				return nil, nil, e
			}
			exact = raw
		case "capabilities":
			if ref.ID != r.Project {
				return nil, nil, invalid()
			}
			if cfg != nil {
				exact, err = intakeCapabilities(*cfg, r.Project)
			} else {
				// The grant binds these exact persisted contract bytes, including
				// the policy/model applicable when this work was committed.
				exact = raw
			}
		default:
			return nil, nil, fmt.Errorf("unsupported immutable intake context reference %q", ref.Kind)
		}
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(exact, raw) {
			return nil, nil, invalid()
		}
	}
	if len(members) != 0 || !seen["capabilities/"+r.Project] || len(draftSources) != len(draftMetadata) {
		return nil, nil, invalid()
	}
	for id := range draftSources {
		if !draftMetadata[id] {
			return nil, nil, invalid()
		}
	}
	return turns, drafts, nil
}

// Validate persisted records before exporting manifests to a model worker.
func intakeManifestIdentity(m model.IntakeManifest) error {
	sha := m.SHA
	m.SHA = ""
	if sha == "" || sha != programHash(m) {
		return fmt.Errorf("intake manifest identity mismatch")
	}
	return nil
}

func intakeCapabilities(c model.Config, project string) ([]byte, error) {
	p, ok := c.Projects[project]
	if !ok || !p.Trusted {
		return nil, intakeError("project_forbidden", "project is not enrolled")
	}
	checks := make([]string, 0, len(p.Checks))
	for name := range p.Checks {
		checks = append(checks, name)
	}
	sort.Strings(checks)
	targets := []string{"reviewed", "integrated"}
	if p.GitHubRepository != "" {
		targets = append(targets, "pull_request")
	}
	for _, target := range []string{"staging", "production"} {
		release, ok := p.Releases[target]
		if ok && release.Allowed {
			targets = append(targets, target)
		}
	}
	limits := intakeLimits(c)
	return json.Marshal(struct {
		Project            string   `json:"project"`
		Checks             []string `json:"checks"`
		CompletionTargets  []string `json:"completion_targets"`
		Model              string   `json:"model"`
		MaxTurnBytes       int      `json:"max_turn_bytes"`
		MaxTurns           int      `json:"max_turns"`
		MaxTextBytes       int      `json:"max_text_bytes"`
		MaxCanonicalBytes  int      `json:"max_canonical_bytes"`
		MaxDraftDispatches int      `json:"max_draft_dispatches"`
	}{project, checks, targets, c.AstraModel, limits.MaxTurnBytes, limits.MaxTurns, limits.MaxTextBytes, limits.MaxCanonicalBytes, limits.MaxDraftCalls})
}

// Reconcile non-dispatched jobs and durable waits without releasing uncertain
// external ownership. Running attempts are stopped by the normal engine path.
func (s *Store) ReconcileIntakeRequests(ctx context.Context, c model.Config) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.reconcileIntakeRequestsTx(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) reconcileIntakeRequestsTx(ctx context.Context, tx *sql.Tx, c model.Config) error {
	requests, err := intakeList[model.IntakeRequest](ctx, tx, `SELECT data FROM intake_requests ORDER BY id`)
	if err != nil {
		return err
	}
	for _, r := range requests {
		if r.State == model.IntakeSubmitted || r.State == model.IntakeCancelled {
			continue
		}
		invalid := ""
		if !c.Intake.Enabled || r.ContextManifest.PolicySHA != c.PolicyHash {
			invalid = "intake policy changed; fresh disclosure required"
		}
		if len(r.ContextManifest.Disclosure.ModelProviderIDs) != 1 || r.ContextManifest.Disclosure.ModelProviderIDs[0] != c.AstraModel {
			invalid = "draft provider changed; fresh disclosure required"
		}
		if _, ok := c.Projects[r.Project]; !ok {
			invalid = "project is no longer configured"
		}
		if r.State == model.IntakeDraftQueued || r.State == model.IntakeDrafting {
			if err = intakeDraftAuthorityTx(ctx, tx, r, r.ContextManifest); err != nil {
				var domain *model.ProgramError
				if !errors.As(err, &domain) {
					return err
				}
				invalid = domain.Message
			}
		}
		if r.ActiveStepID == "" {
			if r.State == model.IntakeCancelling {
				r.State = model.IntakeCancelled
				if err = intakeEventTx(ctx, tx, &r, "cancelled", ""); err != nil {
					return err
				}
			} else if r.State == model.IntakeDraftQueued && (invalid != "" || r.LifetimeDispatches >= r.MaxDraftDispatches) {
				if invalid == "" {
					invalid = "request lifetime draft budget exhausted"
				}
				r.State = model.IntakeBlocked
				r.Wait = &model.WaitReason{Code: "draft_unavailable", Detail: invalid}
				if err = intakeEventTx(ctx, tx, &r, "blocked", invalid); err != nil {
					return err
				}
			}
			continue
		}
		step, err := intakeStepLoad(ctx, tx, "id", r.ActiveStepID)
		if err != nil {
			return err
		}
		f, err := get(ctx, tx, step.FeatureID)
		if err != nil {
			return err
		}
		if invalid != "" && !step.Superseded {
			step.Superseded = true
			step.State = "cancelling"
			f.State = "cancelling"
			f.Reason = invalid
			if err = intakePutStepTx(ctx, tx, step, true); err != nil {
				return err
			}
			if err = put(ctx, tx, f); err != nil {
				return err
			}
		}
		a, err := intakeLoad[model.Attempt](ctx, tx, `SELECT data FROM attempts WHERE feature_id=? AND active=1`, f.ID)
		if err == nil {
			if invalid == "" && intakeProgressCurrent(r, step, f) {
				if err = intakeSetProgressTx(ctx, tx, &r, model.IntakeDrafting, a.Wait); err != nil {
					return err
				}
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !step.Superseded && f.State != "blocked" && f.State != "cancelled" && f.State != "cancelling" {
			if f.State == "ready" && step.AttemptID == "" && intakeProgressCurrent(r, step, f) {
				wait, err := s.admissionGate(ctx, tx, f, c.PolicyHash, c.MaxAttempts, c.MaxConcurrent, c.Projects[r.Project].MaxConcurrent)
				if err != nil {
					return err
				}
				if err = intakeSetProgressTx(ctx, tx, &r, model.IntakeDraftQueued, wait); err != nil {
					return err
				}
			}
			continue
		}
		step.State = "blocked"
		if step.Superseded {
			step.State = "superseded"
		}
		if err = intakePutStepTx(ctx, tx, step, false); err != nil {
			return err
		}
		f.State = "cancelled"
		if err = put(ctx, tx, f); err != nil {
			return err
		}
		r.ActiveStepID = ""
		if r.State == model.IntakeCancelling {
			r.State = model.IntakeCancelled
		} else if r.ContentVersion == step.ContentVersion {
			r.State = model.IntakeBlocked
			r.Wait = &model.WaitReason{Code: "draft_unavailable", Detail: f.Reason}
		}
		if err = intakeEventTx(ctx, tx, &r, "step_settled", step.ID); err != nil {
			return err
		}
	}
	return nil
}
