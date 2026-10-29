package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

func (e *Engine) intent(ctx context.Context, a model.Attempt) (bool, error) {
	err := e.db.BeginOperation(ctx, a, e.cfg.PolicyHash)
	if errors.Is(err, store.ErrWaiting) {
		return false, e.wait(ctx, a, "operation_gate", err)
	}
	return err == nil, err
}

func (e *Engine) wait(ctx context.Context, a model.Attempt, code string, err error) error {
	detail := err.Error()
	if strings.Contains(detail, "paused") {
		code = "paused"
	} else if strings.Contains(detail, "cancellation") {
		code = "cancellation"
	}
	return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: code, Detail: detail})
}
func (e *Engine) proven(ctx context.Context, a model.Attempt, f model.Feature) error {
	evidence, err := e.db.AttemptEvidence(ctx, a.ID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range evidence {
		if v.CandidateSHA == a.CandidateSHA && v.CandidateGeneration == a.CandidateGeneration && v.Passed && strings.TrimSpace(v.Detail) != "" {
			seen[v.Kind] = true
		}
	}
	for _, name := range f.Checks {
		if !seen["check:"+name] {
			return fmt.Errorf("candidate lacks current deterministic check evidence: %s", name)
		}
	}
	if !seen["review"] {
		return errors.New("candidate lacks current independent review evidence")
	}
	return nil
}
func (e *Engine) release(ctx context.Context, a model.Attempt, f model.Feature) error {
	if f.CompletionTarget != "staging" && f.CompletionTarget != "production" {
		return e.block(ctx, a, "unexpected release stage for target", true)
	}
	if err := e.validate(f); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if err := e.proven(ctx, a, f); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	files, err := e.repos.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if err = e.paths(f, files); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	fp, err := e.repos.Fingerprint(ctx, a.Workspace)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if fp != a.Fingerprint {
		return e.block(ctx, a, "candidate changed before release", false)
	}
	receipt := a.Summary
	if receipt == "" {
		return e.block(ctx, a, "release has no durable integration receipt", true)
	}
	a.Stage = "releasing"
	if proceed, err := e.intent(ctx, a); err != nil || !proceed {
		return err
	}
	evidence, err := e.repos.Release(ctx, f.Project, e.cfg.Projects[f.Project], a, f)
	for i := range evidence {
		evidence[i].CandidateGeneration = a.CandidateGeneration
	}
	if err != nil {
		return e.block(ctx, a, "release result uncertain: "+err.Error(), true, evidence...)
	}
	if len(evidence) == 0 {
		return e.block(ctx, a, "release returned no observation evidence", true)
	}
	for _, v := range evidence {
		if !v.Passed || v.CandidateSHA != a.CandidateSHA || strings.TrimSpace(v.Detail) == "" {
			return e.block(ctx, a, "release acceptance evidence is missing, failed, or for wrong candidate", true, evidence...)
		}
	}
	a.State = "accepted"
	a.Stage = "accepted"
	return e.db.Save(ctx, a, "accepted", "target "+f.CompletionTarget+" proven", receipt, false, evidence...)
}

// A concrete deterministic failure may consume another approved attempt, never
// another revision's authority. Questions, policy violations and uncertain effects
// deliberately use block instead.
func (e *Engine) repair(ctx context.Context, a model.Attempt, f model.Feature, reason string) error {
	if a.IntegrationToken != "" || a.SourceCandidateSHA != "" {
		return e.block(ctx, a, "reconciled candidate failed; fresh approval required: "+reason, false, model.Evidence{Kind: "failure", CandidateSHA: a.CandidateSHA, CandidateGeneration: a.CandidateGeneration, Passed: false, Detail: reason})
	}
	a.State = "failed"
	a.Stage = "failed"
	state := "ready"
	if f.Attempts >= e.cfg.MaxAttempts {
		state = "blocked"
		reason = "attempt allowance exhausted: " + reason
	}
	return e.db.Save(ctx, a, state, reason, "", false, model.Evidence{Kind: "failure", CandidateSHA: a.CandidateSHA, CandidateGeneration: a.CandidateGeneration, Passed: false, Detail: reason})
}
