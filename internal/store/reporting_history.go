package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"openfactory/internal/model"
)

const reportTimelineLimit = 1000

// reportIntervals is a bounded chronological ring. Event history is streamed in
// durable sequence order; old intervals can be discarded without losing the
// state needed to interpret later retries, revisions, or completion receipts.
type reportIntervals struct {
	items []model.StageInterval
	start int
}

func (b *reportIntervals) append(stage, state, attemptID string, at time.Time) {
	if at.IsZero() {
		return
	}
	if len(b.items) != 0 {
		last := &b.items[(b.start+len(b.items)-1)%len(b.items)]
		if last.Stage == stage && last.State == state && last.AttemptID == attemptID {
			return
		}
		if at.Before(last.StartedAt) {
			at = last.StartedAt
		}
		if last.FinishedAt == nil {
			finish := at
			last.FinishedAt = &finish
		}
	}
	next := model.StageInterval{Stage: stage, State: state, AttemptID: attemptID, StartedAt: at}
	if state == "failed" || state == "cancelled" || state == "accepted" && stage != "waiting_to_merge" {
		finish := at
		next.FinishedAt = &finish
	}
	if len(b.items) < reportTimelineLimit {
		b.items = append(b.items, next)
		return
	}
	b.items[b.start] = next
	b.start = (b.start + 1) % len(b.items)
}

func (b *reportIntervals) result() []model.StageInterval {
	if len(b.items) == 0 {
		return []model.StageInterval{}
	}
	out := b.items
	if b.start != 0 {
		out = make([]model.StageInterval, len(b.items))
		n := copy(out, b.items[b.start:])
		copy(out[n:], b.items[:b.start])
	}
	for i := range out {
		out[i].Stage = reportPublic(out[i].Stage, 128)
		out[i].State = reportPublic(out[i].State, 128)
		out[i].AttemptID = reportPublic(out[i].AttemptID, 256)
	}
	return out
}

func reportFeatureHistory(ctx context.Context, tx *sql.Tx, f model.Feature) ([]model.DependencyStatus, []model.StageInterval, error) {
	dependencies := make([]model.DependencyStatus, 0, len(f.DependsOn))
	for _, id := range f.DependsOn {
		dep, err := get(ctx, tx, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		state := "missing"
		if err == nil {
			state = dep.State
		}
		dependencies = append(dependencies, model.DependencyStatus{ID: reportPublic(id, 256), State: reportPublic(state, 128), Satisfied: err == nil && DependencySatisfied(dep)})
	}
	var latest reportAttempt
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT data,active,revision FROM attempts WHERE feature_id=? AND revision=? ORDER BY rowid DESC LIMIT 1`, f.ID, f.Revision).Scan(&data, &latest.Active, &latest.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, &latest.Attempt); err != nil {
			return nil, nil, err
		}
	}
	var intervals reportIntervals
	intervals.append("queued", "proposed", "", f.CreatedAt)
	state, rawStage, target, receipt, attemptID := "proposed", "", "", "", ""
	fallback := "queued"
	legacyIntegrated := false
	rows, err := tx.QueryContext(ctx, `SELECT e.kind,e.detail,e.created_at,p.data,json_extract(r.data,'$.completion_target') FROM events e LEFT JOIN event_progress p ON p.event_id=e.id LEFT JOIN revisions r ON r.feature_id=e.feature_id AND r.revision=e.detail AND e.kind='imported' WHERE e.feature_id=? ORDER BY e.seq`, f.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, detail, at string
		var progressData []byte
		var historicalTarget sql.NullString
		if err = rows.Scan(&kind, &detail, &at, &progressData, &historicalTarget); err != nil {
			return nil, nil, err
		}
		observedAt, e := reportTime(at)
		if e != nil {
			return nil, nil, e
		}
		switch kind {
		case "imported":
			state, rawStage, receipt, attemptID = "proposed", "", "", ""
			target = historicalTarget.String
			legacyIntegrated = false
		case "approved", "retry_requested":
			state, rawStage, receipt, attemptID = "ready", "", "", ""
			legacyIntegrated = false
		case "attempt_started":
			state, rawStage, receipt, attemptID = "running", "prepare", "", detail
			legacyIntegrated = false
		case "dispatch_reserved":
			rawStage = detail + "_dispatching"
		case "cancel_requested":
			state = "cancelling"
		case "cancelled":
			state = "cancelled"
		case "blocked", "uncertain", "operator_reconciled_failure":
			state = "blocked"
		case "failed":
			state = "failed"
		case "accepted":
			state, rawStage = "accepted", "accepted"
		default:
			if reportDisplayStage("running", kind, target, receipt, "") != "" {
				rawStage = kind
			}
		}
		var progress *Progress
		if len(progressData) != 0 {
			progress = &Progress{}
			if err = json.Unmarshal(progressData, progress); err != nil {
				return nil, nil, err
			}
			state, target, receipt = progress.State, progress.CompletionTarget, progress.Receipt
			if progress.Stage != "" {
				rawStage = progress.Stage
			}
			if progress.AttemptID != "" {
				attemptID = progress.AttemptID
			}
		}
		// Legacy rows lack receipts. Today's receipt is only evidence for the
		// same current attempt/revision, never a later re-import or retry.
		if progress == nil && state == "accepted" && f.State == "accepted" && target == f.CompletionTarget && latest.Attempt.ID == attemptID && latest.Revision == f.Revision {
			receipt = f.Receipt
		}
		if kind == "failed" && state == "ready" {
			intervals.append(fallback, "failed", attemptID, observedAt)
		}
		stage := reportDisplayStage(state, rawStage, target, receipt, fallback)
		if progress == nil && kind == "integrated" && reportIntegratedTarget(target) {
			legacyIntegrated = true
		}
		if legacyIntegrated && (rawStage == "integrated" || rawStage == "releasing") {
			stage = "integrated"
		}
		intervalAttempt := attemptID
		if state == "ready" || state == "proposed" || state == "awaiting_approval" {
			intervalAttempt = ""
		}
		intervals.append(stage, state, intervalAttempt, observedAt)
		fallback = stage
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	current := reportDisplayStage(f.State, latest.Attempt.Stage, f.CompletionTarget, f.Receipt, fallback)
	currentAttempt := latest.Attempt.ID
	if f.State == "proposed" || f.State == "ready" || f.State == "awaiting_approval" {
		currentAttempt = ""
	}
	currentState, observedAt := f.State, f.UpdatedAt
	if f.State == "running" && latest.Active && latest.Attempt.Wait != nil {
		currentState = "waiting"
		if !latest.Attempt.UpdatedAt.IsZero() {
			observedAt = latest.Attempt.UpdatedAt
		}
	}
	intervals.append(current, currentState, currentAttempt, observedAt)
	return dependencies, intervals.result(), nil
}

func reportIntegratedTarget(target string) bool {
	return target == "integrated" || target == "staging" || target == "production"
}

var reportPullPath = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[1-9][0-9]*$`)

func reportPullReceipt(receipt string) bool {
	u, err := url.Parse(receipt)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.RawPath == "" && reportPullPath.MatchString(u.Path) && !strings.Contains(u.Path, "/./") && !strings.Contains(u.Path, "/../")
}

func reportDisplayStage(state, stage, target, receipt, fallback string) string {
	if state == "proposed" || state == "ready" || state == "awaiting_approval" {
		return "queued"
	}
	if state == "accepted" {
		if target == "pull_request" {
			return "waiting_to_merge"
		}
		if target == "reviewed" && receipt != "" {
			return "reviewed"
		}
		if reportIntegratedTarget(target) && receipt != "" && !strings.HasPrefix(receipt, "reviewed:") && !reportPullReceipt(receipt) {
			return "integrated"
		}
		return "reviewing"
	}
	switch stage {
	case "prepare", "scout_pending", "scout_dispatching", "scout_running", "build_pending", "build_dispatching", "build_running", "plan_pending", "plan_dispatching", "plan_running":
		return "building"
	case "checks", "committing":
		return "checking"
	case "review_pending", "review_dispatching", "review_running":
		return "reviewing"
	case "reviewed":
		if target == "reviewed" {
			return "reviewing"
		}
		return "waiting_to_merge"
	case "opening_pr", "integrating", "integration_queued", "reconciliation_pending", "reconciling":
		return "waiting_to_merge"
	case "integrated", "releasing":
		if reportIntegratedTarget(target) && receipt != "" {
			return "integrated"
		}
		return "waiting_to_merge"
	default:
		return fallback
	}
}
