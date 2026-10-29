package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfactory/internal/model"
)

const intakePlanningSchema = `
CREATE TABLE IF NOT EXISTS planning_questions(id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),revision TEXT NOT NULL,attempt_id TEXT NOT NULL REFERENCES attempts(id),output_sha TEXT NOT NULL,data BLOB NOT NULL,UNIQUE(feature_id,revision,attempt_id,output_sha));
CREATE TABLE IF NOT EXISTS planning_answers(id TEXT PRIMARY KEY,question_id TEXT NOT NULL REFERENCES planning_questions(id),feature_id TEXT NOT NULL,predecessor_revision TEXT NOT NULL,revision TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS planning_lineage(feature_id TEXT NOT NULL,revision TEXT NOT NULL,predecessor_revision TEXT NOT NULL,original_goal TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(feature_id,revision));
`

type planningLineage struct {
	OriginalGoal        string                 `json:"original_goal"`
	PredecessorRevision string                 `json:"predecessor_revision"`
	Answers             []model.PlanningAnswer `json:"answers"`
}

func planningOutputSHA(output string) string {
	h := sha256.Sum256([]byte(output))
	return hex.EncodeToString(h[:])
}

func planningQuestionHash(q model.PlanningQuestion) string {
	q.ID = ""
	q.CreatedAt = time.Time{}
	return programHash(q)
}

// RecordPlanningQuestion binds the exact model output to its authorized attempt
// and settles it in the same transaction. It does not retry or grant authority.
func (s *Store) RecordPlanningQuestion(ctx context.Context, a model.Attempt, question string) error {
	if strings.TrimSpace(question) == "" || a.Summary == "" {
		return programError("invalid_contract", "planning question and output required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	if f.Kind != "plan" || f.State == "cancelling" || f.State == "cancelled" {
		return programError("stale_subject", "planning question requires an active plan")
	}
	var revision, policy string
	var active bool
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT revision,policy_hash,active,data FROM attempts WHERE id=? AND feature_id=?`, a.ID, f.ID).Scan(&revision, &policy, &active, &raw); err != nil {
		return err
	}
	if revision != f.Revision || policy != f.PolicyHash {
		return programError("stale_revision", "planning attempt authority changed")
	}
	q := model.PlanningQuestion{FeatureID: f.ID, FeatureRevision: f.Revision, AttemptID: a.ID, OutputSHA: planningOutputSHA(a.Summary), Question: question, CreatedAt: time.Now().UTC()}
	q.ID = planningQuestionHash(q)
	if !active {
		var existing []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM planning_questions WHERE id=?`, q.ID).Scan(&existing); err != nil {
			return programError("attempt_unsettled", "planning attempt is already settled")
		}
		var old model.PlanningQuestion
		if err = json.Unmarshal(existing, &old); err != nil {
			return err
		}
		if planningQuestionHash(old) != q.ID {
			return programError("evidence_invalid", "planning question binding changed")
		}
		return nil
	}
	var old model.Attempt
	if err = json.Unmarshal(raw, &old); err != nil {
		return err
	}
	if old.ThreadID != a.ThreadID || old.DispatchKey != a.DispatchKey {
		return programError("stale_subject", "planning output is not the recorded attempt output")
	}
	raw, err = json.Marshal(q)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO planning_questions(id,feature_id,revision,attempt_id,output_sha,data) VALUES(?,?,?,?,?,?)`, q.ID, f.ID, f.Revision, a.ID, q.OutputSHA, raw); err != nil {
		return err
	}
	a.Stage, a.State, a.ThreadID = "blocked", "blocked", ""
	if err = s.saveTx(ctx, tx, a, "blocked", "planning question requires an answer", "", false); err != nil {
		return err
	}
	if err = event(ctx, tx, f.ID, "planning_question", q.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailPlanningQuestion settles a terminal planning worker whose question cannot
// be accepted. The engine must have observed that exact worker terminate; this
// method cannot resolve unknown worker ownership or stale ledger authority.
func (s *Store) FailPlanningQuestion(ctx context.Context, a model.Attempt, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return programError("invalid_contract", "planning failure reason required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	var revision, policy string
	var active bool
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT revision,policy_hash,active,data FROM attempts WHERE id=? AND feature_id=?`, a.ID, a.FeatureID).Scan(&revision, &policy, &active, &raw); err != nil {
		return err
	}
	var old model.Attempt
	if err = json.Unmarshal(raw, &old); err != nil {
		return err
	}
	if !active {
		return errors.New("planning attempt already settled")
	}
	if revision != f.Revision || policy != f.PolicyHash {
		return errors.New("planning attempt authority no longer matches feature")
	}
	if f.Kind != "plan" || old.ID != a.ID || old.FeatureID != a.FeatureID || old.Stage != "plan_running" || a.Stage != old.Stage || old.ThreadID == "" || old.ThreadID != a.ThreadID || old.DispatchKey != a.DispatchKey {
		return errors.New("terminal planning worker does not match recorded ownership")
	}
	settled := old
	settled.Cursor, settled.Summary = a.Cursor, a.Summary
	settled.Stage, settled.State, settled.ThreadID = "failed", "blocked", ""
	state := "blocked"
	if f.State == "cancelling" || f.State == "cancelled" {
		settled.Stage, settled.State, state = "cancelled", "cancelled", "cancelled"
	}
	if err = s.saveTx(ctx, tx, settled, state, reason, "", false); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PlanningQuestions(ctx context.Context, featureID string) ([]model.PlanningQuestion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM planning_questions WHERE feature_id=? ORDER BY rowid`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.PlanningQuestion{}
	for rows.Next() {
		var raw []byte
		var q model.PlanningQuestion
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) AnswerPlanningQuestion(ctx context.Context, answer model.PlanningAnswer) (model.Feature, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Feature{}, err
	}
	defer tx.Rollback()
	out, err := s.answerPlanningQuestionTx(ctx, tx, answer)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) answerPlanningQuestionTx(ctx context.Context, tx *sql.Tx, answer model.PlanningAnswer) (model.Feature, error) {
	var out model.Feature
	if err := validateAuditContext(answer.Audit); err != nil {
		return out, err
	}
	if strings.TrimSpace(answer.Text) == "" || answer.Question.ID == "" {
		return out, programError("invalid_contract", "bound planning question and answer required")
	}
	f, err := get(ctx, tx, answer.Question.FeatureID)
	if err != nil {
		return out, err
	}
	if f.Revision != answer.Question.FeatureRevision {
		return out, programError("stale_revision", "planning answer requires the current revision")
	}
	if f.Kind != "plan" || f.State == "cancelled" || f.State == "cancelling" {
		return out, programError("stale_subject", "planning answer requires a current plan")
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM planning_questions WHERE id=? AND feature_id=? AND revision=?`, answer.Question.ID, f.ID, f.Revision).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, programError("question_unresolved", "planning question is not current")
		}
		return out, err
	}
	var q model.PlanningQuestion
	if err = json.Unmarshal(raw, &q); err != nil {
		return out, err
	}
	if q.ID != planningQuestionHash(q) || q.ID != planningQuestionHash(answer.Question) {
		return out, programError("stale_revision", "planning question binding changed")
	}
	var revision string
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT revision,active,data FROM attempts WHERE id=? AND feature_id=?`, q.AttemptID, f.ID).Scan(&revision, &active, &raw); err != nil {
		return out, err
	}
	var a model.Attempt
	if err = json.Unmarshal(raw, &a); err != nil {
		return out, err
	}
	if active || revision != q.FeatureRevision || planningOutputSHA(a.Summary) != q.OutputSHA {
		return out, programError("attempt_unsettled", "planning question attempt is not settled with the exact output")
	}
	var latestAttempt string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM attempts WHERE feature_id=? AND revision=? ORDER BY rowid DESC LIMIT 1`, f.ID, f.Revision).Scan(&latestAttempt); err != nil {
		return out, err
	}
	if latestAttempt != q.AttemptID {
		return out, programError("stale_revision", "planning question was superseded by a newer attempt")
	}
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, f.ID).Scan(&n); err != nil {
		return out, err
	}
	if n != 0 {
		return out, programError("attempt_unsettled", "planning attempt must settle before answering")
	}
	lineage := planningLineage{OriginalGoal: f.Body, PredecessorRevision: f.Revision}
	err = tx.QueryRowContext(ctx, `SELECT data FROM planning_lineage WHERE feature_id=? AND revision=?`, f.ID, f.Revision).Scan(&raw)
	if err == nil {
		if err = json.Unmarshal(raw, &lineage); err != nil {
			return out, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	lineage.PredecessorRevision = f.Revision
	answer.Question, answer.CreatedAt = q, time.Now().UTC()
	lineage.Answers = append(lineage.Answers, answer)
	var body strings.Builder
	body.WriteString(lineage.OriginalGoal)
	body.WriteString("\n\n## Operator planning clarifications\n")
	for i, item := range lineage.Answers {
		fmt.Fprintf(&body, "\n### Clarification %d\n\nQuestion: %s\n\nAnswer: %s\n\nPlanning lineage: feature revision %s; attempt %s; output SHA256 %s; question %s.\n", i+1, item.Question.Question, item.Text, item.Question.FeatureRevision, item.Question.AttemptID, item.Question.OutputSHA, item.Question.ID)
	}
	f.Body = strings.TrimSpace(body.String())
	out, err = importFeatureTx(ctx, tx, f, false)
	if err != nil {
		return out, err
	}
	if out.Revision == q.FeatureRevision {
		return out, programError("invalid_contract", "planning answer must create a new revision")
	}
	raw, err = json.Marshal(lineage)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO planning_lineage(feature_id,revision,predecessor_revision,original_goal,data) VALUES(?,?,?,?,?)`, f.ID, out.Revision, q.FeatureRevision, lineage.OriginalGoal, raw); err != nil {
		return out, err
	}
	raw, err = json.Marshal(answer)
	if err != nil {
		return out, err
	}
	answerID := programHash(struct{ QuestionID, Revision, Text string }{q.ID, q.FeatureRevision, answer.Text})
	if _, err = tx.ExecContext(ctx, `INSERT INTO planning_answers(id,question_id,feature_id,predecessor_revision,revision,data) VALUES(?,?,?,?,?,?)`, answerID, q.ID, f.ID, q.FeatureRevision, out.Revision, raw); err != nil {
		return out, err
	}
	if err = recordAuditTx(ctx, tx, "feature", f.ID, "planning_answered", answer.Audit); err != nil {
		return out, err
	}
	if err = event(ctx, tx, f.ID, "planning_answered", answerID); err != nil {
		return out, err
	}
	return out, nil
}
