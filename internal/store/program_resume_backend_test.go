package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func resumeBackendFailure(t *testing.T, s *Store, cfg model.Config, step model.ProgramStep, reserve bool) model.Attempt {
	t.Helper()
	ctx := context.Background()
	a, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, 2, 2, 600)
	if err != nil {
		t.Fatal(err)
	}
	if reserve {
		if err = s.ReserveCall(ctx, a, step.Kind, a.ID+":"+step.Kind, 100); err != nil {
			t.Fatal(err)
		}
	}
	a.State, a.Stage = "blocked", "failed"
	a.Summary = "private worker failure /host/private-path token=not-review-data"
	if err = s.Save(ctx, a, "blocked", a.Summary, "", false); err != nil {
		t.Fatal(err)
	}
	return a
}

func resumeBackendSelection(t *testing.T, s *Store, cfg model.Config, r model.ProgramRevision) (*programResumeSelection, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p, err := programLoad(ctx, tx, r.ProgramID)
	if err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err = tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	selection, selectionErr := s.programResumeWorkTx(ctx, tx, p, r, cfg)
	if err = tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("resume review selection wrote to the database: before=%d after=%d", before, after)
	}
	return selection, selectionErr
}

func TestProgramResumeBackendReadOnlySelectionAndPerAttemptCallCharge(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	cfg.MaxAttempts = 3
	ctx := context.Background()
	step := firstProgramStep(t, s, cfg)
	first := resumeBackendFailure(t, s, cfg, step, true)
	if err := s.RecordProgramWait(ctx, r.ProgramID, r.RevisionHash, "prerequisite_missing", "persisted failure wait"); err != nil {
		t.Fatal(err)
	}
	selection, err := resumeBackendSelection(t, s, cfg, r)
	if err != nil || selection == nil || selection.Work == nil {
		t.Fatalf("missing failed current step: %+v %v", selection, err)
	}
	work := selection.Work
	output := sha256.Sum256([]byte(first.Summary))
	if work.StepID != step.ID || work.StepState != "ready" || work.FeatureState != "blocked" || work.AttemptID != first.ID || work.AttemptState != "blocked" || work.AttemptStage != "failed" || work.AttemptActive || work.FeatureAttempts != 1 || work.FeatureMaxAttempts != 3 || work.AttemptOutputSHA != hex.EncodeToString(output[:]) || work.AttemptUpdatedAt.IsZero() {
		t.Fatalf("review did not bind the exact persisted settled failure: %+v", work)
	}
	encoded, err := json.Marshal(work)
	if err != nil || strings.Contains(string(encoded), "private-path") || strings.Contains(string(encoded), "not-review-data") || selection.Experiment != nil || selection.Artifact != nil {
		t.Fatalf("resume binding leaked worker failure or execution material: %s %v", encoded, err)
	}
	again, err := resumeBackendSelection(t, s, cfg, r)
	if err != nil || again == nil || programHash(again.Work) != programHash(work) {
		t.Fatalf("read-only selection changed target: %+v %v", again, err)
	}
	if err = s.ResumeProgram(ctx, r.ProgramID, r.RevisionHash, cfg, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	items, err := s.ProgramWork(ctx, cfg)
	if err != nil || len(items) != 1 || items[0].Wait != nil || items[0].Step == nil || programStepIdentity(*items[0].Step) != step.ID {
		t.Fatalf("coordinator did not emit the same current step exactly once: %+v %v", items, err)
	}
	second, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, cfg.MaxAttempts, 2, 2, 600)
	if err != nil || second.ID == first.ID {
		t.Fatalf("retry did not use ordinary new-attempt admission: %+v %v", second, err)
	}
	key := second.ID + ":" + step.Kind
	if err = s.ReserveCall(ctx, second, step.Kind, key, 100); err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, second, step.Kind, key, 100); err != nil {
		t.Fatalf("identical dispatch reservation was not idempotent: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := getProgramBudgetTx(ctx, tx, r.ProgramID)
	tx.Rollback()
	if err != nil || budget.ModelCalls != 2 || budget.Steps != 1 || budget.Experiments != 0 {
		t.Fatalf("reused StepID bypassed lifetime charges or duplicated logical steps: %+v %v", budget, err)
	}
	f, err := s.Get(ctx, step.FeatureID)
	if err != nil || f.Attempts != 2 || f.State != "running" {
		t.Fatalf("retry bypassed feature attempt accounting: %+v %v", f, err)
	}
	var calls int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("dispatch idempotency charged the wrong call count: %d %v", calls, err)
	}
}

func TestProgramResumeBackendLifetimeCapsRejectWithoutMutation(t *testing.T) {
	for _, cap := range []string{"attempt", "model_call"} {
		t.Run(cap, func(t *testing.T) {
			s, cfg, r := programStepsFixture(t)
			cfg.MaxAttempts = 3
			if cap == "attempt" {
				cfg.MaxAttempts = 1
			} else {
				cfg.Discovery.Limits.MaxModelCalls = 1
			}
			ctx := context.Background()
			step := firstProgramStep(t, s, cfg)
			resumeBackendFailure(t, s, cfg, step, true)
			before, err := s.Get(ctx, step.FeatureID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resumeBackendSelection(t, s, cfg, r)
			var domain *model.ProgramError
			if !errors.As(err, &domain) || domain.Code != "budget_exhausted" {
				t.Fatalf("exhausted lifetime %s allowance was accepted: %v", cap, err)
			}
			if err = s.ResumeProgram(ctx, r.ProgramID, r.RevisionHash, cfg, model.TrustedLocalAudit()); !errors.As(err, &domain) || domain.Code != "budget_exhausted" {
				t.Fatalf("resume bypassed lifetime %s allowance: %v", cap, err)
			}
			after, err := s.Get(ctx, step.FeatureID)
			if err != nil || programHash(before) != programHash(after) {
				t.Fatalf("rejected resume mutated feature: before=%+v after=%+v error=%v", before, after, err)
			}
			var active, retries int
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE active!=0`).Scan(&active); err != nil || active != 0 {
				t.Fatalf("rejection changed ownership: %d %v", active, err)
			}
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE feature_id=? AND kind='retry_requested'`, step.FeatureID).Scan(&retries); err != nil || retries != 0 {
				t.Fatalf("rejection emitted a retry: %d %v", retries, err)
			}
		})
	}
}

func TestProgramResumeBackendSourceCorruptionRemainsFatal(t *testing.T) {
	s, cfg, r := programStepsFixture(t)
	cfg.MaxAttempts = 3
	step := firstProgramStep(t, s, cfg)
	resumeBackendFailure(t, s, cfg, step, false)
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, "corrupt-source", r.ProgramID, r.Number, []byte(`{"invalid":`)); err != nil {
		t.Fatal(err)
	}
	_, err := resumeBackendSelection(t, s, cfg, r)
	var domain *model.ProgramError
	var syntax *json.SyntaxError
	if errors.As(err, &domain) || !errors.As(err, &syntax) {
		t.Fatalf("source corruption was downgraded to a resumable wait: %v", err)
	}
}

func TestProgramResumeBackendRejectsZeroAttemptAndOlderActiveOwner(t *testing.T) {
	t.Run("zero_attempt", func(t *testing.T) {
		s, cfg, r := programStepsFixture(t)
		cfg.MaxAttempts = 3
		step := firstProgramStep(t, s, cfg)
		if err := s.Block(context.Background(), step.FeatureID, "pre-attempt prerequisite"); err != nil {
			t.Fatal(err)
		}
		selection, err := resumeBackendSelection(t, s, cfg, r)
		var domain *model.ProgramError
		if selection != nil || !errors.As(err, &domain) || domain.Code != "stale_subject" {
			t.Fatalf("synthetic retry without a persisted attempt was accepted: %+v %v", selection, err)
		}
		var attempts int
		if err = s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM attempts WHERE feature_id=?`, step.FeatureID).Scan(&attempts); err != nil || attempts != 0 {
			t.Fatalf("zero-attempt rejection created ownership: %d %v", attempts, err)
		}
	})
	t.Run("older_active_owner", func(t *testing.T) {
		s, cfg, r := programStepsFixture(t)
		cfg.MaxAttempts = 4
		ctx := context.Background()
		step := firstProgramStep(t, s, cfg)
		first := resumeBackendFailure(t, s, cfg, step, false)
		if err := s.ResumeProgram(ctx, r.ProgramID, r.RevisionHash, cfg, model.TrustedLocalAudit()); err != nil {
			t.Fatal(err)
		}
		latest := resumeBackendFailure(t, s, cfg, step, false)
		if first.ID == latest.ID {
			t.Fatal("fixture did not create distinct persisted attempts")
		}
		// Deliberately corrupt only the older row's ownership marker. A latest
		// inactive failure must never hide another attempt retaining capacity.
		if _, err := s.db.ExecContext(ctx, `UPDATE attempts SET active=1 WHERE id=?`, first.ID); err != nil {
			t.Fatal(err)
		}
		before, err := s.Get(ctx, step.FeatureID)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := resumeBackendSelection(t, s, cfg, r)
		var domain *model.ProgramError
		if selection != nil || !errors.As(err, &domain) || domain.Code != "attempt_unsettled" {
			t.Fatalf("older active owner was ignored: %+v %v", selection, err)
		}
		if err = s.ResumeProgram(ctx, r.ProgramID, r.RevisionHash, cfg, model.TrustedLocalAudit()); !errors.As(err, &domain) || domain.Code != "attempt_unsettled" {
			t.Fatalf("resume cleared an older active owner: %v", err)
		}
		after, err := s.Get(ctx, step.FeatureID)
		if err != nil || programHash(after) != programHash(before) {
			t.Fatalf("ownership rejection mutated feature: %+v %+v %v", before, after, err)
		}
		var active int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active!=0`, step.FeatureID).Scan(&active); err != nil || active != 1 {
			t.Fatalf("rejection released retained capacity: %d %v", active, err)
		}
	})
}
