package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"openfactory/internal/config"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

func (e *Engine) enqueueIntegration(ctx context.Context, a model.Attempt, f model.Feature) error {
	resources, err := config.IntegrationResources(e.cfg.Projects[f.Project], f.CompletionTarget)
	if err != nil {
		return e.block(ctx, a, "delivery resources: "+err.Error(), false)
	}
	return e.db.EnqueueIntegration(ctx, a, resources)
}

func (e *Engine) acquireIntegration(ctx context.Context, a model.Attempt, f model.Feature) error {
	if err := e.enqueueIntegration(ctx, a, f); err != nil {
		return err
	}
	lease, err := e.db.AcquireIntegration(ctx, a.ID, e.cfg.PolicyHash)
	if errors.Is(err, store.ErrWaiting) {
		if strings.Contains(err.Error(), "paused") || strings.Contains(err.Error(), "cancellation") {
			return e.wait(ctx, a, "integration_gate", err)
		}
		status, statusErr := e.db.IntegrationStatus(ctx, a.ID)
		if statusErr != nil {
			return statusErr
		}
		return e.db.SetWait(ctx, a.ID, &model.WaitReason{Code: "integration_owner", Detail: fmt.Sprintf("delivery queue position %d", status.Position), Resource: status.ContendedResource, OwnerID: status.OwnerAttemptID})
	}
	if err != nil {
		return err
	}
	if lease.AttemptID != a.ID || lease.Token == "" {
		return errors.New("integration acquisition returned no matching durable ownership")
	}
	a.IntegrationToken = lease.Token
	a.Stage = "reconciliation_pending"
	return e.save(ctx, a)
}

// requiredAncestors preserves applicable accepted dependency candidates even if
// another actor rewrites the branch after dependency admission. Cross-repository
// and other-branch dependencies retain their ordinary admission gate only.
func (e *Engine) requiredAncestors(ctx context.Context, f model.Feature) ([]string, error) {
	project := e.cfg.Projects[f.Project]
	identity, err := config.RepositoryIdentity(project)
	if err != nil {
		return nil, err
	}
	ancestors := map[string]bool{}
	// Admission already checked the direct dependency receipts. Reconciliation
	// adds only the ancestry proof relevant to this exact target branch; the
	// direct candidate already implies its own same-branch ancestors.
	for _, id := range f.DependsOn {
		dep, err := e.db.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		other, ok := e.cfg.Projects[dep.Project]
		if !ok || project.Branch != other.Branch {
			continue
		}
		otherIdentity, err := config.RepositoryIdentity(other)
		if err != nil {
			return nil, err
		}
		if identity != otherIdentity {
			continue
		}
		sha, err := e.db.AcceptedCandidate(ctx, dep.ID)
		if err != nil {
			return nil, fmt.Errorf("dependency %s accepted candidate: %w", id, err)
		}
		ancestors[sha] = true
	}
	out := make([]string, 0, len(ancestors))
	for sha := range ancestors {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out, nil
}

func (e *Engine) reconcileCandidate(ctx context.Context, a model.Attempt, f model.Feature) error {
	if a.IntegrationToken == "" || a.SourceBaseSHA == "" || a.SourceCandidateSHA == "" {
		return e.block(ctx, a, "reconciliation lacks durable ownership or immutable source delta", true)
	}
	ancestors, err := e.requiredAncestors(ctx, f)
	if err != nil {
		return e.block(ctx, a, "dependency ancestry: "+err.Error(), false)
	}
	if a.Stage == "reconciliation_pending" {
		a.ReconcileGeneration++
		if a.ReconcileGeneration <= a.CandidateGeneration {
			a.ReconcileGeneration = a.CandidateGeneration + 1
		}
		a.ReconcileOperationID = fmt.Sprintf("%s:reconcile:%d", a.ID, a.ReconcileGeneration)
		a.Stage = "reconciling"
	}
	if a.ReconcileOperationID == "" || a.ReconcileGeneration < 1 {
		return e.block(ctx, a, "reconciliation intent lacks operation identity", true)
	}
	// Local reconciliation is receipt-idempotent. Persist and fence its identity
	// before work so restart resumes the same operation rather than recapturing.
	if proceed, err := e.intent(ctx, a); err != nil || !proceed {
		return err
	}
	candidate, err := e.repos.Reconcile(ctx, f.Project, e.cfg.Projects[f.Project], model.ReconcileRequest{OperationID: a.ReconcileOperationID, Generation: a.ReconcileGeneration, Attempt: a, Feature: f, RequiredAncestors: ancestors})
	if err != nil {
		return e.deliveryFailure(ctx, a, "reconciliation", err)
	}
	if candidate.Workspace.Path == "" || candidate.Workspace.BaseSHA == "" || candidate.CandidateSHA == "" || candidate.Fingerprint == "" {
		return e.block(ctx, a, "reconciliation returned an incomplete candidate snapshot", false)
	}
	changed := candidate.Changed || candidate.CandidateSHA != a.CandidateSHA || candidate.Workspace.BaseSHA != a.BaseSHA
	if changed && (candidate.CandidateSHA == a.CandidateSHA || candidate.Workspace.Path == a.Workspace) {
		return e.block(ctx, a, "reconciliation did not isolate a new candidate snapshot", false)
	}
	if !changed && candidate.Fingerprint != a.Fingerprint {
		return e.block(ctx, a, "unchanged reconciliation altered the reviewed fingerprint", false)
	}
	a.Workspace, a.BaseSHA = candidate.Workspace.Path, candidate.Workspace.BaseSHA
	a.CandidateSHA, a.Fingerprint = candidate.CandidateSHA, candidate.Fingerprint
	if changed {
		a.CandidateGeneration = a.ReconcileGeneration
	}
	files, err := e.repos.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
	if err != nil {
		return e.block(ctx, a, "reconciled paths: "+err.Error(), false)
	}
	if err := e.paths(f, files); err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	a.Stage = "reviewed"
	if changed {
		a.Stage = "checks"
		a.DispatchKey, a.ThreadID, a.EnvironmentID, a.Cursor, a.Summary = "", "", "", "", ""
	}
	return e.save(ctx, a)
}

func (e *Engine) deliveryFailure(ctx context.Context, a model.Attempt, operation string, err error) error {
	// The repository has classified the result after the worker may have been
	// cancelled or reached its deadline. Persist that knowledge before draining
	// the worker; otherwise a known no-write rejection becomes false uncertainty
	// on the next cycle. This is bounded local settlement, not renewed authority
	// to perform an external operation.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var delivery *model.DeliveryError
	if errors.As(err, &delivery) {
		switch delivery.Kind {
		case "target_moved":
			// Git positively established that no remote mutation occurred. Keep
			// our FIFO ownership and reconcile the immutable feature delta again.
			a.Stage = "reconciliation_pending"
			return e.save(persist, a)
		case "conflict", "rejected":
			detail := operation + " blocked: " + delivery.Error()
			if len(delivery.Paths) > 0 {
				detail += "; paths: " + strings.Join(delivery.Paths, ", ")
			}
			return e.block(persist, a, detail, false)
		}
	}
	// Unknown effects are never interpreted as safe rejection. In particular a
	// transport failure after push/PR/release must retain the durable lease.
	return e.block(persist, a, operation+" result uncertain: "+err.Error(), true)
}
