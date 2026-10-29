package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"openfactory/internal/model"
)

// ProgramQuery selects a bounded, revision-specific read. Cursors are opaque and
// fail closed if the program or its evidence changed between requests.
type ProgramQuery struct {
	Project  string
	State    string
	After    string
	Kind     string
	Limit    int
	Revision int
}
type ProgramPage struct {
	Items      []model.Program `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Snapshot   string          `json:"snapshot"`
}
type ProgramStatus struct {
	Program       model.Program               `json:"program"`
	Revision      model.ProgramRevision       `json:"revision"`
	Budget        model.ProgramBudget         `json:"budget"`
	Grants        []model.ProgramGrant        `json:"grants"`
	Invalidations []model.ProgramInvalidation `json:"invalidations"`
	Steps         []model.ProgramStep         `json:"steps"`
	Snapshot      string                      `json:"snapshot"`
	Truncated     bool                        `json:"truncated"`
}

// ProgramArtifactOutputState describes history, not approval or evidence validity.
// Evaluator stages are distinct producer kinds even when payload kind is evaluation.
type ProgramArtifactOutputState struct {
	ArtifactID         string   `json:"artifact_id"`
	ProducerStepKind   string   `json:"producer_step_kind"`
	ProducerSubjectID  string   `json:"producer_subject_id"`
	SupersededBy       []string `json:"superseded_by"`
	CurrentStageOutput bool     `json:"current_stage_output"`
	HistoricalRevision bool     `json:"historical_revision"`
}
type ProgramArtifactPage struct {
	Items        []model.ProgramArtifact      `json:"items"`
	OutputStates []ProgramArtifactOutputState `json:"output_states"`
	NextCursor   string                       `json:"next_cursor,omitempty"`
	Snapshot     string                       `json:"snapshot"`
	RevisionHash string                       `json:"revision_hash"`
}
type ProgramArtifactDetail struct {
	Artifact    model.ProgramArtifact      `json:"artifact"`
	Payload     json.RawMessage            `json:"payload"`
	Snapshot    string                     `json:"snapshot"`
	OutputState ProgramArtifactOutputState `json:"output_state"`
}
type ProgramQuestionView struct {
	Artifact model.ProgramArtifact  `json:"artifact"`
	Question model.ProgramQuestion  `json:"question"`
	Answers  []model.QuestionAnswer `json:"answers"`
}
type ProgramQuestionPage struct {
	Items        []ProgramQuestionView `json:"items"`
	NextCursor   string                `json:"next_cursor,omitempty"`
	Snapshot     string                `json:"snapshot"`
	RevisionHash string                `json:"revision_hash"`
}
type ProgramExperimentView struct {
	Artifact    model.ProgramArtifact     `json:"artifact"`
	Contract    model.ExperimentContract  `json:"contract"`
	ContractSHA string                    `json:"contract_sha"`
	Grants      []model.ProgramGrant      `json:"grants"`
	Receipts    []model.ExperimentReceipt `json:"receipts"`
}
type ProgramExperimentPage struct {
	Items        []ProgramExperimentView `json:"items"`
	NextCursor   string                  `json:"next_cursor,omitempty"`
	Snapshot     string                  `json:"snapshot"`
	RevisionHash string                  `json:"revision_hash"`
}
type ProgramSourcePage struct {
	Items        []model.SourceCapture `json:"items"`
	NextCursor   string                `json:"next_cursor,omitempty"`
	Snapshot     string                `json:"snapshot"`
	RevisionHash string                `json:"revision_hash"`
}
type ProgramFeatureView struct {
	Link    model.ProgramFeatureLink `json:"link"`
	Feature model.FeatureStatus      `json:"feature"`
}
type ProgramFeaturePage struct {
	Items        []ProgramFeatureView `json:"items"`
	NextCursor   string               `json:"next_cursor,omitempty"`
	Snapshot     string               `json:"snapshot"`
	RevisionHash string               `json:"revision_hash"`
}
type ProgramEventPage struct {
	Items        []model.ProgramEvent `json:"items"`
	NextCursor   string               `json:"next_cursor,omitempty"`
	Snapshot     string               `json:"snapshot"`
	RevisionHash string               `json:"revision_hash"`
}

type programReadCursor struct {
	Version  int    `json:"v"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Project  string `json:"project"`
	State    string `json:"state"`
	Filter   string `json:"filter"`
	Revision int    `json:"revision"`
	Snapshot string `json:"snapshot"`
	After    string `json:"after"`
}

func programReadEncode(c programReadCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func programReadDecode(raw string, want programReadCursor) (programReadCursor, error) {
	if raw == "" {
		return want, nil
	}
	if len(raw) > 4096 {
		return want, ErrInvalidQuery
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(raw)
	if e != nil {
		return want, ErrInvalidQuery
	}
	var c programReadCursor
	if json.Unmarshal(b, &c) != nil || programReadEncode(c) != raw || c.After == "" {
		return want, ErrInvalidQuery
	}
	after := c.After
	c.After = ""
	if c != want {
		return want, ErrInvalidQuery
	}
	c.After = after
	return c, nil
}
func programReadError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func programReadState(s string) bool {
	switch s {
	case "", model.ProgramDraft, model.ProgramActive, model.ProgramWaiting, model.ProgramBlocked, model.ProgramProposed, model.ProgramCancelled:
		return true
	}
	return false
}

// Include state and mutable evidence rows, not merely timestamps: receipts and
// invalidations can advance without a phase transition. No open transaction or
// SQLite snapshot is retained across HTTP requests.
func programReadSnapshot(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	h := sha256.New()
	if id == "" {
		var count, last int64
		var updated string
		e := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(rowid),0),coalesce(max(json_extract(data,'$.updated_at')),'') FROM programs`).Scan(&count, &last, &updated)
		if e != nil {
			return "", e
		}
		fmt.Fprintf(h, "%d:%d:%s", count, last, updated)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	// Append-only tables are represented by their high-water marks. Mutable
	// bounded execution/state rows are hashed in full, detecting same-row updates.
	for _, table := range []string{"program_revisions", "program_grants", "program_artifacts", "program_answers", "program_invalidations", "program_feature_links", "program_events", "program_sources", "program_searches"} {
		var count, last int64
		e := tx.QueryRowContext(ctx, "SELECT count(*),coalesce(max(rowid),0) FROM "+table+" WHERE program_id=?", id).Scan(&count, &last)
		if e != nil {
			return "", e
		}
		fmt.Fprintf(h, "%s:%d:%d;", table, count, last)
	}
	for _, table := range []string{"programs", "program_budgets", "program_steps", "program_experiments"} {
		column := "program_id"
		if table == "programs" {
			column = "id"
		}
		rows, e := tx.QueryContext(ctx, "SELECT data FROM "+table+" WHERE "+column+"=? ORDER BY rowid LIMIT 501", id)
		if e != nil {
			return "", e
		}
		h.Write([]byte(table))
		count := 0
		for rows.Next() {
			count++
			if count > 500 {
				rows.Close()
				return "", fmt.Errorf("program reporting snapshot bound exceeded")
			}
			var b []byte
			if e = rows.Scan(&b); e != nil {
				rows.Close()
				return "", e
			}
			h.Write(b)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "", e
		}
	}
	rows, e := tx.QueryContext(ctx, `SELECT f.data FROM features f JOIN program_feature_links l ON f.id=l.feature_id WHERE l.program_id=? ORDER BY f.id LIMIT 501`, id)
	if e != nil {
		return "", e
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 500 {
			return "", fmt.Errorf("program feature snapshot bound exceeded")
		}
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return "", e
		}
		h.Write(b)
	}
	if e = rows.Err(); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func programReadStart(ctx context.Context, s *Store, id, kind string, q ProgramQuery) (*sql.Tx, model.ProgramRevision, programReadCursor, int, error) {
	var r model.ProgramRevision
	var c programReadCursor
	l, e := reportLimit(q.Limit)
	if e != nil || q.Revision < 0 || len(q.Project) > 256 || len(q.State) > 64 || len(q.Kind) > 64 || !programReadState(q.State) {
		return nil, r, c, 0, ErrInvalidQuery
	}
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return nil, r, c, 0, e
	}
	fail := func(e error) (*sql.Tx, model.ProgramRevision, programReadCursor, int, error) {
		tx.Rollback()
		return nil, r, c, 0, programReadError(e)
	}
	if id != "" {
		p, e := programLoad(ctx, tx, id)
		if e != nil {
			return fail(e)
		}
		n := q.Revision
		if n == 0 {
			n = p.CurrentRevision
		}
		r, e = programRevision(ctx, tx, id, n)
		if e != nil {
			return fail(e)
		}
	}
	snap, e := programReadSnapshot(ctx, tx, id)
	if e != nil {
		return fail(e)
	}
	c, e = programReadDecode(q.After, programReadCursor{Version: 1, Kind: kind, ID: id, Project: q.Project, State: q.State, Filter: q.Kind, Revision: r.Number, Snapshot: snap})
	if e != nil {
		return fail(e)
	}
	return tx, r, c, l, nil
}

// Keep canonical serialized types while removing operational secrets/host paths
// from free text. Hashes and identifiers remain addressable; source URLs have
// credentials, query strings and fragments removed by the shared sanitizer.
func programReadSafe[T any](v T) T {
	out := reflect.ValueOf(&v).Elem()
	programReadSanitize(out, "")
	return v
}
func programReadSanitize(v reflect.Value, name string) {
	if !v.IsValid() || !v.CanSet() {
		return
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		return
	}
	if v.Type() == reflect.TypeOf(json.RawMessage{}) {
		if v.Len() == 0 {
			return
		}
		var x any
		d := json.NewDecoder(bytes.NewReader(v.Bytes()))
		d.UseNumber()
		if d.Decode(&x) == nil {
			x = programReadJSONSafe(x)
			b, _ := json.Marshal(x)
			v.SetBytes(b)
		}
		return
	}
	switch v.Kind() {
	case reflect.String:
		lower := strings.ToLower(name)
		if name == "JSONPointer" {
			v.SetString(reportPublic(v.String(), 4096))
		} else if strings.HasSuffix(lower, "sha") || strings.HasSuffix(lower, "sha256") || strings.HasSuffix(lower, "hash") || strings.HasSuffix(lower, "id") || name == "Kind" || name == "State" || name == "Phase" {
			v.SetString(reportPublic(v.String(), 256))
		} else {
			v.SetString((reportSanitizer{}).text(v.String(), 4096))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			programReadSanitize(v.Elem(), name)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			programReadSanitize(v.Field(i), v.Type().Field(i).Name)
		}
	case reflect.Slice:
		for i := range v.Len() {
			programReadSanitize(v.Index(i), name)
		}
	}
}
func programReadJSONSafe(v any) any {
	switch x := v.(type) {
	case string:
		return (reportSanitizer{}).text(x, 4096)
	case []any:
		for i := range x {
			x[i] = programReadJSONSafe(x[i])
		}
	case map[string]any:
		for k, v := range x {
			if strings.Contains(strings.ToLower(k), "token") || strings.EqualFold(k, "prompt") || strings.EqualFold(k, "broker_url") {
				delete(x, k)
			} else if k == "json_pointer" {
				if pointer, ok := v.(string); ok {
					x[k] = reportPublic(pointer, 4096)
				} else {
					x[k] = programReadJSONSafe(v)
				}
			} else {
				x[k] = programReadJSONSafe(v)
			}
		}
	}
	return v
}
func programReadRows[T any](ctx context.Context, tx *sql.Tx, query string, args ...any) ([]T, error) {
	rows, e := tx.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var b []byte
		var v T
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) ProgramPage(ctx context.Context, q ProgramQuery) (ProgramPage, error) {
	out := ProgramPage{Items: []model.Program{}}
	if q.Revision != 0 || q.Kind != "" {
		return out, ErrInvalidQuery
	}
	tx, _, c, l, e := programReadStart(ctx, s, "", "programs", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	items, e := programReadRows[model.Program](ctx, tx, `SELECT data FROM programs WHERE id>? AND (?='' OR json_extract(data,'$.project')=?) AND (?='' OR json_extract(data,'$.state')=?) ORDER BY id LIMIT ?`, c.After, q.Project, q.Project, q.State, q.State, l+1)
	if e != nil {
		return out, e
	}
	if len(items) > l {
		items = items[:l]
		c.After = items[len(items)-1].ID
		out.NextCursor = programReadEncode(c)
	}
	out.Items = programReadSafe(items)
	return out, tx.Commit()
}
func (s *Store) ProgramStatus(ctx context.Context, id string) (ProgramStatus, error) {
	out := ProgramStatus{Grants: []model.ProgramGrant{}, Invalidations: []model.ProgramInvalidation{}, Steps: []model.ProgramStep{}}
	tx, r, c, _, e := programReadStart(ctx, s, id, "status", ProgramQuery{})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Revision = r
	out.Snapshot = c.Snapshot
	out.Program, e = programLoad(ctx, tx, id)
	if e != nil {
		return out, e
	}
	out.Budget, e = getProgramBudgetTx(ctx, tx, id)
	if e != nil {
		return out, e
	}
	out.Grants, e = programReadRows[model.ProgramGrant](ctx, tx, `SELECT data FROM program_grants WHERE program_id=? ORDER BY rowid DESC LIMIT 501`, id)
	if e != nil {
		return out, e
	}
	out.Invalidations, e = programReadRows[model.ProgramInvalidation](ctx, tx, `SELECT data FROM program_invalidations WHERE program_id=? ORDER BY rowid DESC LIMIT 501`, id)
	if e != nil {
		return out, e
	}
	out.Steps, e = programReadRows[model.ProgramStep](ctx, tx, `SELECT data FROM program_steps WHERE program_id=? ORDER BY rowid DESC LIMIT 501`, id)
	if e != nil {
		return out, e
	}
	if len(out.Grants) > 500 {
		out.Truncated = true
		out.Grants = out.Grants[:500]
	}
	if len(out.Invalidations) > 500 {
		out.Truncated = true
		out.Invalidations = out.Invalidations[:500]
	}
	if len(out.Steps) > 500 {
		out.Truncated = true
		out.Steps = out.Steps[:500]
	}
	out = programReadSafe(out)
	return out, tx.Commit()
}
func (s *Store) ProgramArtifactPage(ctx context.Context, id string, q ProgramQuery) (ProgramArtifactPage, error) {
	out := ProgramArtifactPage{Items: []model.ProgramArtifact{}, OutputStates: []ProgramArtifactOutputState{}}
	tx, r, c, l, e := programReadStart(ctx, s, id, "artifacts", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	after := int64(0)
	if c.After != "" {
		after, e = strconv.ParseInt(c.After, 10, 64)
		if e != nil || after < 1 || strconv.FormatInt(after, 10) != c.After {
			return out, ErrInvalidQuery
		}
	}
	rows, e := tx.QueryContext(ctx, `SELECT rowid,data FROM program_artifacts WHERE program_id=? AND revision=? AND (?=0 OR rowid<?) AND (?='' OR kind=?) ORDER BY rowid DESC LIMIT ?`, id, r.Number, after, after, q.Kind, q.Kind, l+1)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var seq int64
		var b []byte
		var a model.ProgramArtifact
		if e = rows.Scan(&seq, &b); e != nil {
			rows.Close()
			return out, e
		}
		if e = json.Unmarshal(b, &a); e != nil {
			rows.Close()
			return out, e
		}
		if len(out.Items) == l {
			out.NextCursor = programReadEncode(c)
			break
		}
		out.Items = append(out.Items, a)
		c.After = strconv.FormatInt(seq, 10)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	p, e := programLoad(ctx, tx, id)
	if e != nil {
		return out, e
	}
	for _, a := range out.Items {
		state, e := programArtifactOutputState(ctx, tx, p, a)
		if e != nil {
			return out, e
		}
		out.OutputStates = append(out.OutputStates, state)
	}
	out.Items = programReadSafe(out.Items)
	out.OutputStates = programReadSafe(out.OutputStates)
	return out, tx.Commit()
}

func programArtifactOutputState(ctx context.Context, tx *sql.Tx, p model.Program, a model.ProgramArtifact) (ProgramArtifactOutputState, error) {
	out := ProgramArtifactOutputState{ArtifactID: a.ID, SupersededBy: []string{}, HistoricalRevision: a.Revision != p.CurrentRevision}
	rows, e := tx.QueryContext(ctx, `SELECT id FROM program_artifacts WHERE program_id=? AND json_extract(data,'$.supersedes_id')=? ORDER BY rowid DESC LIMIT 501`, p.ID, a.ID)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return out, e
		}
		out.SupersededBy = append(out.SupersededBy, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(out.SupersededBy) > 500 {
		return out, fmt.Errorf("artifact supersession reporting bound exceeded")
	}
	var raw []byte
	e = tx.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE program_id=? AND id=?`, p.ID, a.ProducerStepID).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	var step model.ProgramStep
	if e = json.Unmarshal(raw, &step); e != nil {
		return out, e
	}
	out.ProducerStepKind = step.Kind
	out.ProducerSubjectID = step.SubjectID
	if out.HistoricalRevision || a.Iteration != p.Iteration || step.Revision != a.Revision || step.Iteration != a.Iteration || step.State != "completed" || step.OutputManifestSHA == "" || len(out.SupersededBy) > 0 {
		return out, nil
	}
	var latestStep string
	e = tx.QueryRowContext(ctx, `SELECT id FROM program_steps WHERE program_id=? AND revision=? AND json_extract(data,'$.iteration')=? AND json_extract(data,'$.kind')=? AND json_extract(data,'$.subject_id')=? ORDER BY rowid DESC LIMIT 1`, p.ID, a.Revision, a.Iteration, step.Kind, step.SubjectID).Scan(&latestStep)
	if e != nil {
		return out, e
	}
	if latestStep != step.ID {
		return out, nil
	}
	var latestArtifact string
	e = tx.QueryRowContext(ctx, `SELECT id FROM program_artifacts WHERE program_id=? AND revision=? AND kind=? AND json_extract(data,'$.producer_step_id')=? ORDER BY rowid DESC LIMIT 1`, p.ID, a.Revision, a.Kind, step.ID).Scan(&latestArtifact)
	if e != nil {
		return out, e
	}
	out.CurrentStageOutput = latestArtifact == a.ID
	return out, nil
}
func (s *Store) ProgramArtifactDetail(ctx context.Context, id, artifactID string) (ProgramArtifactDetail, error) {
	var out ProgramArtifactDetail
	tx, _, c, _, e := programReadStart(ctx, s, id, "artifact", ProgramQuery{})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	var b []byte
	e = tx.QueryRowContext(ctx, `SELECT data FROM program_artifacts WHERE program_id=? AND id=?`, id, artifactID).Scan(&b)
	if e != nil {
		return out, programReadError(e)
	}
	if e = json.Unmarshal(b, &out.Artifact); e != nil {
		return out, e
	}
	out.Payload, e = s.blobs.Read(ctx, out.Artifact.ContentSHA, 8<<20)
	if e != nil {
		return out, e
	}
	p, e := programLoad(ctx, tx, id)
	if e != nil {
		return out, e
	}
	out.OutputState, e = programArtifactOutputState(ctx, tx, p, out.Artifact)
	if e != nil {
		return out, e
	}
	out.Snapshot = c.Snapshot
	out = programReadSafe(out)
	return out, tx.Commit()
}

// Flatten just the requested payload kind, one immutable blob at a time. The
// cursor includes the artifact and element position, so an artifact containing
// more than a page of questions/experiments is never silently truncated.
func (s *Store) programReadPayloadPage(ctx context.Context, tx *sql.Tx, id string, revision int, kind string, c programReadCursor, limit int, visit func(model.ProgramArtifact, json.RawMessage) error) (string, error) {
	afterID := ""
	offset := 0
	if c.After != "" {
		i := strings.LastIndex(c.After, ":")
		if i < 1 {
			return "", ErrInvalidQuery
		}
		afterID = c.After[:i]
		var e error
		offset, e = strconv.Atoi(c.After[i+1:])
		if e != nil || offset < 0 {
			return "", ErrInvalidQuery
		}
	}
	rows, e := tx.QueryContext(ctx, `SELECT data FROM program_artifacts WHERE program_id=? AND revision=? AND kind=? AND id>=? ORDER BY id`, id, revision, kind, afterID)
	if e != nil {
		return "", e
	}
	defer rows.Close()
	count := 0
	bytes := 0
	for rows.Next() {
		var b []byte
		var a model.ProgramArtifact
		if e = rows.Scan(&b); e != nil {
			return "", e
		}
		if e = json.Unmarshal(b, &a); e != nil {
			return "", e
		}
		raw, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
		if e != nil {
			return "", e
		}
		var payload map[string]json.RawMessage
		if e = json.Unmarshal(raw, &payload); e != nil {
			return "", e
		}
		key := "questions"
		if kind == model.ArtifactExperimentContracts {
			key = "experiments"
		}
		var items []json.RawMessage
		if e = json.Unmarshal(payload[key], &items); e != nil {
			return "", e
		}
		start := 0
		if a.ID == afterID {
			start = offset
		}
		if start > len(items) {
			return "", ErrInvalidQuery
		}
		for j := start; j < len(items); j++ {
			if count == limit || (count > 0 && bytes+len(items[j]) > 8<<20) {
				c.After = a.ID + ":" + strconv.Itoa(j)
				return programReadEncode(c), nil
			}
			if e = visit(a, items[j]); e != nil {
				return "", e
			}
			count++
			bytes += len(items[j])
		}
	}
	return "", rows.Err()
}
func (s *Store) ProgramQuestionPage(ctx context.Context, id string, q ProgramQuery) (ProgramQuestionPage, error) {
	out := ProgramQuestionPage{Items: []ProgramQuestionView{}}
	tx, r, c, l, e := programReadStart(ctx, s, id, "questions", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	// Read answers before opening payload rows; SQLite is configured with a sole connection.
	answers, e := programReadRows[model.QuestionAnswer](ctx, tx, `SELECT data FROM program_answers WHERE program_id=? ORDER BY rowid DESC LIMIT 501`, id)
	if e != nil {
		return out, e
	}
	if len(answers) > 500 {
		return out, fmt.Errorf("program answer reporting bound exceeded")
	}
	out.NextCursor, e = s.programReadPayloadPage(ctx, tx, id, r.Number, model.ArtifactQuestions, c, l, func(a model.ProgramArtifact, b json.RawMessage) error {
		v := ProgramQuestionView{Artifact: a, Answers: []model.QuestionAnswer{}}
		if e := json.Unmarshal(b, &v.Question); e != nil {
			return e
		}
		for _, answer := range answers {
			if answer.QuestionArtifactID == a.ID && answer.QuestionArtifactSHA == a.ContentSHA && answer.QuestionID == v.Question.ID {
				v.Answers = append(v.Answers, answer)
			}
		}
		out.Items = append(out.Items, programReadSafe(v))
		return nil
	})
	if e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) ProgramExperimentPage(ctx context.Context, id string, q ProgramQuery) (ProgramExperimentPage, error) {
	out := ProgramExperimentPage{Items: []ProgramExperimentView{}}
	tx, r, c, l, e := programReadStart(ctx, s, id, "experiments", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	grants, e := programReadRows[model.ProgramGrant](ctx, tx, `SELECT data FROM program_grants WHERE program_id=? AND revision_hash=? ORDER BY rowid LIMIT 501`, id, r.RevisionHash)
	if e != nil {
		return out, e
	}
	receipts, e := programReadRows[model.ExperimentReceipt](ctx, tx, `SELECT data FROM program_experiments WHERE program_id=? AND json_extract(data,'$.revision')=? ORDER BY rowid LIMIT 501`, id, r.Number)
	if e != nil {
		return out, e
	}
	if len(grants) > 500 || len(receipts) > 500 {
		return out, fmt.Errorf("program experiment reporting bound exceeded")
	}
	out.NextCursor, e = s.programReadPayloadPage(ctx, tx, id, r.Number, model.ArtifactExperimentContracts, c, l, func(a model.ProgramArtifact, b json.RawMessage) error {
		v := ProgramExperimentView{Artifact: a, Grants: []model.ProgramGrant{}, Receipts: []model.ExperimentReceipt{}}
		if e := json.Unmarshal(b, &v.Contract); e != nil {
			return e
		}
		v.ContractSHA = programHash(v.Contract)
		for _, g := range grants {
			if g.SubjectHash == v.ContractSHA {
				v.Grants = append(v.Grants, g)
			}
		}
		for _, receipt := range receipts {
			if receipt.ContractSHA == v.ContractSHA {
				v.Receipts = append(v.Receipts, receipt)
			}
		}
		out.Items = append(out.Items, programReadSafe(v))
		return nil
	})
	if e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) ProgramFeaturePage(ctx context.Context, id string, q ProgramQuery, cfg model.Config) (ProgramFeaturePage, error) {
	out := ProgramFeaturePage{Items: []ProgramFeatureView{}}
	tx, r, c, l, e := programReadStart(ctx, s, id, "features", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	links, e := programReadRows[model.ProgramFeatureLink](ctx, tx, `SELECT data FROM program_feature_links WHERE program_id=? AND json_extract(data,'$.revision')=? AND feature_id>? ORDER BY feature_id LIMIT ?`, id, r.Number, c.After, l+1)
	if e != nil {
		return out, e
	}
	if len(links) > l {
		links = links[:l]
		c.After = links[len(links)-1].FeatureID
		out.NextCursor = programReadEncode(c)
	}
	for _, link := range links {
		f, e := get(ctx, tx, link.FeatureID)
		if e != nil {
			return out, e
		}
		status, e := s.reportFeature(ctx, tx, f, cfg)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, programReadSafe(ProgramFeatureView{Link: link, Feature: status}))
	}
	return out, tx.Commit()
}
func (s *Store) ProgramEventPage(ctx context.Context, id string, q ProgramQuery) (ProgramEventPage, error) {
	out := ProgramEventPage{Items: []model.ProgramEvent{}}
	tx, r, c, l, e := programReadStart(ctx, s, id, "events", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	after := int64(0)
	if c.After != "" {
		after, e = strconv.ParseInt(c.After, 10, 64)
		if e != nil || after < 1 {
			return out, ErrInvalidQuery
		}
	}
	rows, e := tx.QueryContext(ctx, `SELECT seq,data FROM program_events WHERE program_id=? AND seq>? ORDER BY seq LIMIT ?`, id, after, l+1)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var seq int64
		var b []byte
		var v model.ProgramEvent
		if e = rows.Scan(&seq, &b); e != nil {
			rows.Close()
			return out, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			rows.Close()
			return out, e
		}
		if len(out.Items) == l {
			out.NextCursor = programReadEncode(c)
			break
		}
		out.Items = append(out.Items, programReadSafe(v))
		c.After = strconv.FormatInt(seq, 10)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	return out, tx.Commit()
}

func (s *Store) ProgramSourcePage(ctx context.Context, id string, q ProgramQuery) (ProgramSourcePage, error) {
	out := ProgramSourcePage{Items: []model.SourceCapture{}}
	tx, r, c, l, e := programReadStart(ctx, s, id, "sources", q)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	items, e := programReadRows[model.SourceCapture](ctx, tx, `SELECT data FROM program_sources WHERE program_id=? AND revision=? AND id>? ORDER BY id LIMIT ?`, id, r.Number, c.After, l+1)
	if e != nil {
		return out, e
	}
	if len(items) > l {
		items = items[:l]
		c.After = items[len(items)-1].ID
		out.NextCursor = programReadEncode(c)
	}
	now := time.Now()
	for _, source := range items {
		var invalid int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_invalidations WHERE program_id=? AND subject IN ('*',?,?,?)`, id, source.ID, source.ContentSHA256, source.TextSHA256).Scan(&invalid); e != nil {
			return out, e
		}
		source.Valid = source.Valid && source.FreshUntil != nil && now.Before(*source.FreshUntil) && invalid == 0
		out.Items = append(out.Items, programReadSafe(source))
	}
	return out, tx.Commit()
}
