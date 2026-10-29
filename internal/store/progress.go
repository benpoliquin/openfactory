package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"openfactory/internal/model"
)

// Progress captures only the authority and execution facts needed to interpret an
// event later, after a retry or re-import has replaced the feature's current state.
// Older events deliberately have no Progress rather than fabricated history.
type Progress struct {
	State            string `json:"state"`
	Stage            string `json:"stage,omitempty"`
	AttemptID        string `json:"attempt_id,omitempty"`
	Revision         string `json:"revision"`
	CompletionTarget string `json:"completion_target"`
	Receipt          string `json:"receipt,omitempty"`
}

func progressOf(f model.Feature, attempts ...model.Attempt) Progress {
	p := Progress{State: f.State, Revision: f.Revision, CompletionTarget: f.CompletionTarget, Receipt: f.Receipt}
	if len(attempts) > 0 {
		p.Stage = attempts[0].Stage
		p.AttemptID = attempts[0].ID
	}
	return p
}

// DependencySatisfied is the same integrated-receipt gate used by Claim. A PR or
// independent review receipt is not proof that downstream code can use the change.
func DependencySatisfied(f model.Feature) bool {
	return f.State == "accepted" && (f.CompletionTarget == "integrated" || f.CompletionTarget == "staging" || f.CompletionTarget == "production") && f.Receipt != ""
}

func agentStage(stage string) string {
	for _, name := range []string{"build", "review", "scout", "plan"} {
		if stage == name || stage == name+"_pending" || stage == name+"_dispatching" || stage == name+"_running" {
			return name
		}
	}
	return ""
}

func rememberSession(ctx context.Context, q executor, a model.Attempt, previousStage string) error {
	if a.ThreadID == "" {
		return nil
	}
	stage := agentStage(a.Stage)
	if stage == "" {
		stage = agentStage(previousStage)
	}
	if stage == "" {
		stage = a.Stage
	}
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(attempt_id,thread_id,stage,dispatch_key,environment_id,created_at) VALUES(?,?,?,?,?,?)`, a.ID, a.ThreadID, stage, a.DispatchKey, a.EnvironmentID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// reportLegacySessions recovers only identities still present on old attempts.
// Missing earlier thread IDs and session start timestamps cannot be reconstructed.
func reportLegacySessions(ctx context.Context, tx *sql.Tx, featureID string, sessions []model.SessionStatus) ([]model.SessionStatus, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.data,coalesce((SELECT c.stage FROM calls c WHERE c.attempt_id=a.id AND (c.dispatch_key=json_extract(a.data,'$.dispatch_key') OR coalesce(json_extract(a.data,'$.dispatch_key'),'')='') ORDER BY c.rowid DESC LIMIT 1),'') FROM attempts a WHERE a.feature_id=? AND coalesce(json_extract(a.data,'$.thread_id'),'')!='' AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.attempt_id=a.id AND s.thread_id=json_extract(a.data,'$.thread_id')) ORDER BY a.rowid DESC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		var dispatched string
		var a model.Attempt
		if err = rows.Scan(&data, &dispatched); err == nil {
			err = json.Unmarshal(data, &a)
		}
		if err != nil {
			return nil, err
		}
		stage := agentStage(a.Stage)
		if stage == "" {
			stage = agentStage(dispatched)
		}
		if stage == "" {
			stage = a.Stage
		}
		sessions = append(sessions, model.SessionStatus{AttemptID: reportPublic(a.ID, 256), Stage: reportPublic(stage, 128), DispatchKey: reportPublic(a.DispatchKey, 256), ThreadID: reportPublic(a.ThreadID, 256), EnvironmentID: reportPublic(a.EnvironmentID, 256)})
	}
	return sessions, rows.Err()
}
