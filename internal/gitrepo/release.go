package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/policy"
)

func commandBudget(command model.Command) time.Duration {
	seconds := command.TimeoutSeconds
	if seconds == 0 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

// releaseCommand admits only operator-owned executables, never candidate scripts
// or a shell selected by repository content. ReleaseDir is read-only in production.
func (m *Manager) releaseCommand(command model.Command) error {
	if len(command.Argv) == 0 || !filepath.IsAbs(command.Argv[0]) || m.cfg.ReleaseDir == "" {
		return errors.New("release command must be an absolute operator executable")
	}
	if command.TimeoutSeconds < 0 || command.TimeoutSeconds > 3600 {
		return errors.New("release command timeout exceeds admitted budget")
	}
	root, err := filepath.EvalSymlinks(m.cfg.ReleaseDir)
	if err != nil {
		return err
	}
	shared, err := filepath.EvalSymlinks(m.cfg.WorkspaceDir)
	if err != nil {
		return err
	}
	if root == shared || strings.HasPrefix(root, shared+string(filepath.Separator)) || strings.HasPrefix(shared, root+string(filepath.Separator)) {
		return errors.New("operator release directory overlaps shared agent workspace")
	}
	path, err := filepath.EvalSymlinks(command.Argv[0])
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("release executable is outside operator release directory")
	}
	info, err := os.Lstat(command.Argv[0])
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("release executable must be regular, executable and not group/world writable")
	}
	for _, arg := range command.Argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("release argument contains NUL")
		}
	}
	return nil
}

func releaseDeadline(ctx context.Context, a model.Attempt, profile model.Release) (time.Time, error) {
	if a.Deadline.IsZero() {
		return time.Time{}, errors.New("release requires a bounded attempt deadline")
	}
	deadline := a.Deadline
	if profile.ExpiresAt.Before(deadline) {
		deadline = profile.ExpiresAt
	}
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return deadline, nil
}

// PreflightRelease has no remote or deployment effects. It admits the entire
// bounded deployment/observation/rollback envelope before integration is allowed.
func (m *Manager) PreflightRelease(ctx context.Context, name string, p model.Project, a model.Attempt, f model.Feature) error {
	if _, err := m.validateCandidate(ctx, name, p, a); err != nil {
		return err
	}
	if f.Project != name || f.ID != a.FeatureID {
		return errors.New("release feature identity mismatch")
	}
	profile, ok := p.Releases[f.CompletionTarget]
	if !ok || !profile.Allowed || !profile.Qualified {
		return errors.New("release target is not allowed and qualified")
	}
	if profile.ExpiresAt.IsZero() || !time.Now().Before(profile.ExpiresAt) {
		return errors.New("release qualification expired")
	}
	allowed := false
	for _, risk := range profile.AllowedRisks {
		if f.Risk == risk {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("feature risk not allowed for release")
	}
	paths, err := m.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
	if err != nil {
		return err
	}
	if len(profile.AllowedPaths) == 0 {
		return errors.New("release has no path allowlist")
	}
	for _, path := range paths {
		allowed = false
		for _, pattern := range profile.AllowedPaths {
			if policy.Matches(pattern, path) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("release path is not allowlisted: %s", path)
		}
	}
	if profile.ObservationSeconds <= 0 || profile.MinimumObservations <= 0 {
		return errors.New("release requires an observation window and observations")
	}
	for _, command := range []model.Command{profile.Deploy, profile.Observe, profile.Rollback} {
		if err = m.releaseCommand(command); err != nil {
			return err
		}
	}
	deadline, err := releaseDeadline(ctx, a, profile)
	if err != nil {
		return err
	}
	remaining := time.Until(deadline)
	// Divide before multiplying so arbitrarily large configuration integers cannot
	// wrap durations and make an impossible release appear admissible.
	if remaining <= 0 || int64(profile.ObservationSeconds) > int64(remaining/time.Second) {
		return errors.New("observation window exceeds release budget")
	}
	remaining -= time.Duration(profile.ObservationSeconds) * time.Second
	remaining -= commandBudget(profile.Deploy) + commandBudget(profile.Rollback)
	if remaining <= 0 || int64(profile.MinimumObservations) > int64(remaining/commandBudget(profile.Observe)) {
		return errors.New("deployment, observation and rollback envelope exceeds release budget")
	}
	return nil
}

// Release repeats admission and checks remote identity only after integration.
// No shared Git lock is held during deployment or observation.
func (m *Manager) Release(ctx context.Context, name string, p model.Project, a model.Attempt, f model.Feature) ([]model.Evidence, error) {
	if err := m.PreflightRelease(ctx, name, p, a, f); err != nil {
		return nil, err
	}
	r, err := m.workspace(ctx, a.Workspace)
	if err != nil {
		return nil, err
	}
	actual, err := m.remoteHead(ctx, r, p, p.Branch)
	if err != nil {
		return nil, err
	}
	if actual != a.CandidateSHA {
		return nil, errors.New("release candidate is not the current integrated remote head")
	}
	profile := p.Releases[f.CompletionTarget]
	deadline, err := releaseDeadline(ctx, a, profile)
	if err != nil {
		return nil, err
	}
	private, err := os.MkdirTemp(m.records(), "release-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(private)
	cwd := filepath.Join(private, "source")
	if err = m.privateCheckout(ctx, r.Common, a.CandidateSHA, cwd); err != nil {
		return nil, err
	}
	// Export/network latency must not consume the previously reserved envelope.
	if err = m.PreflightRelease(ctx, name, p, a, f); err != nil {
		return nil, err
	}
	env := []string{"OPENFACTORY_PROJECT=" + name, "OPENFACTORY_FEATURE_ID=" + f.ID, "OPENFACTORY_ATTEMPT_ID=" + a.ID, "OPENFACTORY_SHA=" + a.CandidateSHA, "OPENFACTORY_TARGET=" + f.CompletionTarget, "OPENFACTORY_POLICY_HASH=" + m.cfg.PolicyHash}
	var evidence []model.Evidence
	execute := func(runctx context.Context, kind string, command model.Command) error {
		out, e := run(runctx, cwd, command.Argv, env, m.secretValues(), command.TimeoutSeconds)
		detail := "exit status 0\n" + out
		if e != nil {
			detail = e.Error() + "\n" + out
		}
		evidence = append(evidence, model.Evidence{ID: digest(a.ID + kind + time.Now().String()), FeatureID: f.ID, AttemptID: a.ID, Kind: kind, CandidateSHA: a.CandidateSHA, CandidateGeneration: a.CandidateGeneration, Passed: e == nil, Detail: detail, CreatedAt: time.Now().UTC()})
		return e
	}
	rollback := func(cause error) ([]model.Evidence, error) {
		// Cancellation does not remove preapproved containment authority, but cannot
		// create a fresh attempt budget: rollback remains within the original bound.
		rbctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer cancel()
		if e := execute(rbctx, "release.rollback", profile.Rollback); e != nil {
			return evidence, fmt.Errorf("release failed (%v); rollback failed: %w", cause, e)
		}
		return evidence, fmt.Errorf("release failed; preapproved rollback completed: %w", cause)
	}
	// Reserve containment time even when deployment/observation exhausts its bound.
	releasectx, cancel := context.WithDeadline(ctx, deadline.Add(-commandBudget(profile.Rollback)))
	defer cancel()
	if err = execute(releasectx, "release.deploy", profile.Deploy); err != nil {
		return rollback(err)
	}
	start := time.Now()
	window := time.Duration(profile.ObservationSeconds) * time.Second
	for i := range profile.MinimumObservations {
		when := start.Add((window / time.Duration(profile.MinimumObservations)) * time.Duration(i+1))
		if i == profile.MinimumObservations-1 {
			when = start.Add(window)
		}
		timer := time.NewTimer(time.Until(when))
		select {
		case <-releasectx.Done():
			timer.Stop()
			return rollback(releasectx.Err())
		case <-timer.C:
		}
		if err = execute(releasectx, "release.observe", profile.Observe); err != nil {
			return rollback(err)
		}
	}
	if err = releasectx.Err(); err != nil {
		return rollback(err)
	}
	return evidence, nil
}
