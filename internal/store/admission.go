package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"openfactory/internal/model"
)

// admissionGate is shared by queue mutation and reporting, so the dashboard never
// promises work that Claim would reject. Dependencies precede concurrency gates.
func (s *Store) admissionGate(ctx context.Context, tx *sql.Tx, f model.Feature, policy string, maxAttempts, maxConcurrent, projectMaxConcurrent int) (*model.WaitReason, error) {
	wait := func(code, detail string) *model.WaitReason { return &model.WaitReason{Code: code, Detail: detail} }
	var paused int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='paused' AND value='true'`).Scan(&paused); err != nil {
		return nil, err
	}
	if paused != 0 {
		return wait("paused", "OpenFactory paused"), nil
	}
	if f.State != "ready" {
		return wait("state", "feature is not ready"), nil
	}
	if policy == "" || f.PolicyHash != policy {
		return wait("approval", "approval required under current policy"), nil
	}
	if err := intakeFeatureGateTx(ctx, tx, f, policy); err != nil {
		if errors.Is(err, ErrWaiting) {
			return wait("intake", err.Error()), nil
		}
		return nil, err
	}
	if err := s.programFeatureGateTx(ctx, tx, f, policy); err != nil {
		var domain *model.ProgramError
		if errors.As(err, &domain) {
			return wait("program_evidence", err.Error()), nil
		}
		return nil, err
	}
	if maxAttempts < 1 || f.Attempts >= maxAttempts {
		return wait("attempt_limit", "attempt allowance exhausted"), nil
	}
	for _, id := range f.DependsOn {
		dep, err := get(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if dep.DiscoveryStepID != "" || strings.HasPrefix(id, "openfactory-discovery-") || dep.IntakeStepID != "" || dep.Kind == model.IntakeFeatureKind || strings.HasPrefix(id, model.IntakeFeaturePrefix) {
			return &model.WaitReason{Code: "dependency", Detail: fmt.Sprintf("dependency %s is internal controller work, not a code prerequisite", id), FeatureID: id}, nil
		}
		if !DependencySatisfied(dep) {
			return &model.WaitReason{Code: "dependency", Detail: fmt.Sprintf("dependency %s has no integrated receipt", id), FeatureID: id}, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.data FROM attempts a JOIN features f ON f.id=a.feature_id WHERE a.active=1`)
	if err != nil {
		return nil, err
	}
	global, project := 0, 0
	for rows.Next() {
		var b []byte
		var other model.Feature
		if err = rows.Scan(&b); err == nil {
			err = json.Unmarshal(b, &other)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		global++
		if other.Project == f.Project {
			project++
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if global >= maxConcurrent {
		return wait("global_limit", "global concurrency limit"), nil
	}
	if projectMaxConcurrent == 0 {
		projectMaxConcurrent = 1
	}
	if project >= projectMaxConcurrent {
		return wait("project_limit", "project concurrency limit"), nil
	}
	return nil, nil
}

// SetWait never accepts an attempt snapshot, and cannot clobber candidate state.
func (s *Store) SetWait(ctx context.Context, id string, w *model.WaitReason) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, _, _, active, err := loadAttempt(ctx, tx, id)
	if err != nil {
		return err
	}
	if active == 0 {
		return nil
	}
	old := a
	a.Wait = w
	if err = saveAttempt(ctx, tx, a, old); err != nil {
		return err
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	if err = intakeAttemptProgressTx(ctx, tx, f, a); err != nil {
		return err
	}
	return tx.Commit()
}
