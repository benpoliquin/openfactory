package store

import (
	"context"
	"database/sql"

	"openfactory/internal/model"
)

// Intake progress is a projection of ordinary admission and attempt ownership.
// It never creates work, reserves capacity, changes content, or spends a budget.
func intakeProgressCurrent(r model.IntakeRequest, step model.IntakeStep, f model.Feature) bool {
	return (r.State == model.IntakeDraftQueued || r.State == model.IntakeDrafting) &&
		r.ActiveStepID == step.ID && r.ContentVersion == step.ContentVersion &&
		r.ContextManifest.SHA == step.InputManifestSHA && !step.Superseded &&
		step.FeatureID == f.ID && f.IntakeStepID == step.ID
}

func intakeSetProgressTx(ctx context.Context, tx *sql.Tx, r *model.IntakeRequest, state string, wait *model.WaitReason) error {
	sameWait := r.Wait == nil && wait == nil || r.Wait != nil && wait != nil && *r.Wait == *wait
	if r.State == state && sameWait {
		return nil
	}
	r.State = state
	r.Wait = wait
	return intakeEventTx(ctx, tx, r, "progress", r.ActiveStepID)
}

// A rejected Claim must commit the canonical gate without making the queued
// step look admitted. Stale/cancelling requests keep their newer progress.
func intakeClaimWaitTx(ctx context.Context, tx *sql.Tx, f model.Feature, wait *model.WaitReason) error {
	if f.IntakeStepID == "" {
		return nil
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	if !intakeProgressCurrent(r, step, f) || f.State != "ready" || step.AttemptID != "" {
		return nil
	}
	return intakeSetProgressTx(ctx, tx, &r, model.IntakeDraftQueued, wait)
}

// Claim binds the step and publishes drafting in the attempt's own transaction,
// before a dispatch reservation or an external worker session exists.
func intakeClaimStartedTx(ctx context.Context, tx *sql.Tx, f model.Feature, a model.Attempt) error {
	if f.IntakeStepID == "" {
		return nil
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	if !intakeProgressCurrent(r, step, f) || step.AttemptID != "" && step.AttemptID != a.ID {
		return intakeError("attempt_unsettled", "intake step is no longer available for admission")
	}
	step.AttemptID = a.ID
	step.State = "running"
	if err = intakePutStepTx(ctx, tx, step, true); err != nil {
		return err
	}
	return intakeSetProgressTx(ctx, tx, &r, model.IntakeDrafting, nil)
}

// Active attempt waits (including model-day waits) are already canonical. Copy
// the complete value, including resource/owner fields, without reinterpreting it.
func intakeAttemptProgressTx(ctx context.Context, tx *sql.Tx, f model.Feature, a model.Attempt) error {
	if f.IntakeStepID == "" {
		return nil
	}
	step, err := intakeStepLoad(ctx, tx, "id", f.IntakeStepID)
	if err != nil {
		return err
	}
	r, err := intakeRequestTx(ctx, tx, step.RequestID)
	if err != nil {
		return err
	}
	if !intakeProgressCurrent(r, step, f) || step.AttemptID != "" && step.AttemptID != a.ID {
		return nil
	}
	return intakeSetProgressTx(ctx, tx, &r, model.IntakeDrafting, a.Wait)
}
