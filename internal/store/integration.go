package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"openfactory/internal/config"
	"openfactory/internal/model"
)

func canonicalResources(resources []string) ([]string, error) {
	out := append([]string(nil), resources...)
	sort.Strings(out)
	if len(out) == 0 {
		return nil, errors.New("integration resources required")
	}
	for i, r := range out {
		if strings.TrimSpace(r) == "" || (i > 0 && out[i-1] == r) {
			return nil, errors.New("invalid integration resources")
		}
	}
	return out, nil
}

func enqueueIntegration(ctx context.Context, tx *sql.Tx, a model.Attempt, resources []string) error {
	resources, err := canonicalResources(resources)
	if err != nil {
		return err
	}
	_, revision, policy, active, err := loadAttempt(ctx, tx, a.ID)
	if err != nil {
		return err
	}
	if active != 1 {
		return errors.New("integration attempt already settled")
	}
	var oldResources []byte
	var oldRev, oldPolicy, state string
	err = tx.QueryRowContext(ctx, `SELECT revision,policy_hash,resources,state FROM integration_requests WHERE attempt_id=?`, a.ID).Scan(&oldRev, &oldPolicy, &oldResources, &state)
	if err == nil {
		var old []string
		if err = json.Unmarshal(oldResources, &old); err != nil {
			return err
		}
		if oldRev != revision || oldPolicy != policy || !slices.Equal(old, resources) || state == "settled" {
			return errors.New("integration queue authority or resources changed")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	b, _ := json.Marshal(resources)
	_, err = tx.ExecContext(ctx, `INSERT INTO integration_requests(attempt_id,revision,policy_hash,resources,state) VALUES(?,?,?,?,'queued')`, a.ID, revision, policy, b)
	return err
}

func (s *Store) EnqueueIntegration(ctx context.Context, a model.Attempt, resources []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, err = validateAttempt(ctx, tx, a); err != nil {
		return err
	}
	if err = enqueueIntegration(ctx, tx, a, resources); err != nil {
		return err
	}
	return tx.Commit()
}

func integrationStatus(ctx context.Context, tx *sql.Tx, id string) (model.IntegrationStatus, error) {
	out := model.IntegrationStatus{Resources: []string{}, State: "none"}
	var b []byte
	err := tx.QueryRowContext(ctx, `SELECT seq,resources,state,token FROM integration_requests WHERE attempt_id=?`, id).Scan(&out.Sequence, &b, &out.State, &out.Token)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(b, &out.Resources); err != nil {
		return out, err
	}
	if out.State == "owned" {
		out.OwnerAttemptID = id
		return out, nil
	}
	if out.State == "settled" {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT attempt_id,resources,state FROM integration_requests WHERE seq<? AND state!='settled' ORDER BY seq`, out.Sequence)
	if err != nil {
		return out, err
	}
	out.Position = 1
	for rows.Next() {
		var other, state string
		var data []byte
		var resources []string
		if err = rows.Scan(&other, &data, &state); err == nil {
			err = json.Unmarshal(data, &resources)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		if resource := sharedResource(out.Resources, resources); resource != "" {
			if out.ContendedResource == "" {
				out.ContendedResource = resource
			}
			out.Position++
			if state == "owned" && out.OwnerAttemptID == "" {
				out.OwnerAttemptID = other
			}
		}
	}
	err = rows.Err()
	rows.Close()
	return out, err
}

func (s *Store) IntegrationStatus(ctx context.Context, id string) (model.IntegrationStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.IntegrationStatus{}, err
	}
	defer tx.Rollback()
	out, err := integrationStatus(ctx, tx, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func sharedResource(a, b []string) string {
	for _, x := range a {
		if slices.Contains(b, x) {
			return x
		}
	}
	return ""
}

func (s *Store) AcquireIntegration(ctx context.Context, id, policy string) (model.IntegrationLease, error) {
	lease := model.IntegrationLease{}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return lease, err
	}
	defer tx.Rollback()
	a, revision, hash, active, err := loadAttempt(ctx, tx, id)
	if err != nil {
		return lease, err
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return lease, err
	}
	if active != 1 || revision != f.Revision || policy == "" || hash != policy || f.PolicyHash != policy {
		return lease, errors.New("integration authority mismatch")
	}
	if f.State == "cancelling" {
		return lease, waiting("cancellation pending")
	}
	var paused int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='paused' AND value='true'`).Scan(&paused); err != nil {
		return lease, err
	}
	if paused != 0 {
		return lease, waiting("OpenFactory paused")
	}
	st, err := integrationStatus(ctx, tx, id)
	if err != nil {
		return lease, err
	}
	if st.State == "none" || st.State == "settled" {
		return lease, errors.New("integration attempt is not queued")
	}
	if st.State == "owned" {
		if err = validateIntegrationOwner(ctx, tx, a); err != nil {
			return lease, err
		}
		return model.IntegrationLease{AttemptID: id, Token: st.Token, Resources: st.Resources, Sequence: st.Sequence}, tx.Commit()
	}
	if st.Position > 1 {
		return lease, waiting("earlier integration lane owns or awaits a shared resource")
	}
	token := ID()
	for _, resource := range st.Resources {
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT attempt_id FROM integration_owners WHERE resource=?`, resource).Scan(&owner)
		if err == nil {
			return lease, waiting("integration resource owned by " + owner)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return lease, err
		}
	}
	for _, resource := range st.Resources {
		if _, err = tx.ExecContext(ctx, `INSERT INTO integration_owners(resource,attempt_id,token) VALUES(?,?,?)`, resource, id, token); err != nil {
			return lease, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE integration_requests SET state='owned',token=? WHERE attempt_id=? AND state='queued'`, token, id); err != nil {
		return lease, err
	}
	old := a
	a.IntegrationToken = token
	a.Wait = nil
	if err = saveAttempt(ctx, tx, a, old); err != nil {
		return lease, err
	}
	if err = event(ctx, tx, f.ID, "integration_acquired", id, progressOf(f, a)); err != nil {
		return lease, err
	}
	lease = model.IntegrationLease{AttemptID: id, Token: token, Resources: st.Resources, Sequence: st.Sequence}
	return lease, tx.Commit()
}

func validateIntegrationOwner(ctx context.Context, tx *sql.Tx, a model.Attempt) error {
	st, err := integrationStatus(ctx, tx, a.ID)
	if err != nil {
		return err
	}
	if a.IntegrationToken == "" || st.State != "owned" || st.Token != a.IntegrationToken {
		return errors.New("integration ownership token mismatch")
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM integration_owners WHERE attempt_id=? AND token=?`, a.ID, a.IntegrationToken).Scan(&count); err != nil {
		return err
	}
	if count != len(st.Resources) {
		return errors.New("integration ownership resources incomplete")
	}
	for _, resource := range st.Resources {
		var owner, token string
		if err = tx.QueryRowContext(ctx, `SELECT attempt_id,token FROM integration_owners WHERE resource=?`, resource).Scan(&owner, &token); err != nil {
			return err
		}
		if owner != a.ID || token != a.IntegrationToken {
			return errors.New("integration resource ownership mismatch")
		}
	}
	return nil
}

func releaseIntegration(ctx context.Context, tx *sql.Tx, a model.Attempt) error {
	st, err := integrationStatus(ctx, tx, a.ID)
	if err != nil {
		return err
	}
	if st.State == "none" || st.State == "settled" {
		if a.IntegrationToken != "" {
			return errors.New("stale integration token cannot release ownership")
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM legacy_integration_recovery WHERE attempt_id=?`, a.ID)
		return err
	}
	if st.State == "owned" {
		if err = validateIntegrationOwner(ctx, tx, a); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM integration_owners WHERE attempt_id=? AND token=?`, a.ID, a.IntegrationToken); err != nil {
			return err
		}
	} else if a.IntegrationToken != "" {
		return errors.New("queued integration token mismatch")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE integration_requests SET state='settled',token='' WHERE attempt_id=?`, a.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM legacy_integration_recovery WHERE attempt_id=?`, a.ID)
	return err
}

func protectedStage(a model.Attempt) bool {
	if a.IntegrationToken != "" {
		return true
	}
	switch a.Stage {
	case "reconciliation_pending", "reconciling", "opening_pr", "integrating", "integrated", "releasing", "rolling_back":
		return true
	}
	return false
}

// PrepareIntegrationRecovery cannot steal or time out ownership. Legacy
// potentially-mutating work receives canonical ownership atomically, or startup
// refuses the entire ambiguous set while CLI inspection/resolve remains usable.
func (s *Store) PrepareIntegrationRecovery(ctx context.Context, cfg model.Config) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT a.data,r.attempt_id IS NOT NULL FROM attempts a LEFT JOIN legacy_integration_recovery r ON r.attempt_id=a.id WHERE a.active=1 ORDER BY a.rowid`)
	if err != nil {
		return err
	}
	type recoveryAttempt struct {
		model.Attempt
		legacyUncertain bool
	}
	var attempts []recoveryAttempt
	for rows.Next() {
		var b []byte
		var a recoveryAttempt
		if err = rows.Scan(&b, &a.legacyUncertain); err == nil {
			err = json.Unmarshal(b, &a.Attempt)
		}
		if err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, recovery := range attempts {
		a := recovery.Attempt
		st, e := integrationStatus(ctx, tx, a.ID)
		if e != nil {
			return e
		}
		delivery := protectedStage(a) || (recovery.legacyUncertain && a.Stage == "uncertain")
		needsQueue := delivery || a.Stage == "integration_queued" || a.SourceCandidateSHA != ""
		if st.State == "none" && !needsQueue {
			continue
		}
		f, e := get(ctx, tx, a.FeatureID)
		if e != nil {
			return e
		}
		if f.CompletionTarget == "reviewed" && st.State == "none" {
			continue
		}
		project, ok := cfg.Projects[f.Project]
		if !ok {
			return fmt.Errorf("recovery project %s is missing", f.Project)
		}
		resources, e := config.IntegrationResources(project, f.CompletionTarget)
		if e != nil {
			return e
		}
		if e = enqueueIntegration(ctx, tx, a, resources); e != nil {
			return fmt.Errorf("integration recovery %s: %w", a.ID, e)
		}
		st, e = integrationStatus(ctx, tx, a.ID)
		if e != nil {
			return e
		}
		if st.State == "owned" {
			if e = validateIntegrationOwner(ctx, tx, a); e != nil {
				return e
			}
			continue
		}
		if !delivery {
			continue
		}
		// A legacy effect could already be in flight, so it must own rather than
		// queue behind another uncertain attempt. Any collision is operator work.
		token := ID()
		if a.IntegrationToken != "" {
			token = a.IntegrationToken
		}
		for _, resource := range resources {
			var owner string
			e = tx.QueryRowContext(ctx, `SELECT attempt_id FROM integration_owners WHERE resource=?`, resource).Scan(&owner)
			if e == nil {
				return fmt.Errorf("conflicting unresolved integration ownership: %s and %s", owner, a.ID)
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		for _, resource := range resources {
			if _, e = tx.ExecContext(ctx, `INSERT INTO integration_owners(resource,attempt_id,token) VALUES(?,?,?)`, resource, a.ID, token); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(ctx, `UPDATE integration_requests SET state='owned',token=? WHERE attempt_id=?`, token, a.ID); e != nil {
			return e
		}
		old := a
		a.IntegrationToken = token
		if e = saveAttempt(ctx, tx, a, old); e != nil {
			return e
		}
	}
	return tx.Commit()
}
