package gitrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"openfactory/internal/model"
	"openfactory/internal/policy"
)

type reconciliationReceipt struct {
	OperationID, AttemptID, FeatureID, Project, SourceBase, SourceCandidate string
	Generation                                                              int
	RequiredAncestors                                                       []string
	Result                                                                  record
	Fingerprint                                                             string
	Changed                                                                 bool
	Failure                                                                 *model.DeliveryError
}

func (m *Manager) Reconcile(ctx context.Context, name string, p model.Project, request model.ReconcileRequest) (model.ReconciledCandidate, error) {
	defer m.lockRepository(p)()
	a, f := request.Attempt, request.Feature
	reject := func(err error) (model.ReconciledCandidate, error) {
		return model.ReconciledCandidate{}, deliveryError("rejected", err.Error(), "", nil)
	}
	if request.OperationID == "" || request.Generation < 1 || a.ID == "" || f.ID == "" || f.ID != a.FeatureID || f.Project != name {
		return reject(errors.New("reconciliation operation, generation and feature identity are required"))
	}
	if err := CheckCapabilities(ctx); err != nil {
		return reject(err)
	}
	r, err := m.validateCandidate(ctx, name, p, a)
	if err != nil {
		return reject(err)
	}
	sourceBase, sourceCandidate := r.SourceBase, r.SourceCandidate
	if sourceBase == "" && sourceCandidate == "" {
		// Receipts captured before lane support still bind their original commit.
		sourceBase, sourceCandidate = r.Base, r.Candidate
	}
	if !shaPattern.MatchString(a.SourceBaseSHA) || !shaPattern.MatchString(a.SourceCandidateSHA) || a.SourceBaseSHA != sourceBase || a.SourceCandidateSHA != sourceCandidate {
		return reject(errors.New("immutable source delta does not match the private capture receipt"))
	}
	parents, err := m.git(ctx, r.Common, "rev-list", "--parents", "-n", "1", sourceCandidate)
	if err != nil || strings.TrimSpace(parents) != sourceCandidate+" "+sourceBase {
		return reject(errors.New("source delta is not one controller-authored commit on its original baseline"))
	}
	ancestors := append([]string{}, request.RequiredAncestors...)
	sort.Strings(ancestors)
	for _, ancestor := range ancestors {
		if !shaPattern.MatchString(ancestor) {
			return reject(errors.New("dependency receipt is not an exact commit SHA"))
		}
	}
	identity := reconciliationReceipt{OperationID: request.OperationID, AttemptID: a.ID, FeatureID: f.ID, Project: name, SourceBase: sourceBase, SourceCandidate: sourceCandidate, Generation: request.Generation, RequiredAncestors: ancestors}
	key := digest(name + "\x00" + a.ID + "\x00" + request.OperationID)
	path := filepath.Join(m.records(), "reconciliation-"+key+".json")
	if data, readErr := os.ReadFile(path); readErr == nil {
		var saved reconciliationReceipt
		if err = json.Unmarshal(data, &saved); err != nil {
			return reject(err)
		}
		binding := saved
		binding.Result, binding.Fingerprint, binding.Changed, binding.Failure = record{}, "", false, nil
		if !reflect.DeepEqual(binding, identity) {
			return reject(errors.New("reconciliation operation identity changed"))
		}
		return m.finishReconciliation(ctx, saved, key)
	} else if !os.IsNotExist(readErr) {
		return reject(readErr)
	}
	target, err := m.fetchTarget(ctx, r, p)
	if err != nil {
		return reject(err)
	}
	if err = m.requireAncestor(ctx, r.Common, sourceBase, target); err != nil {
		identity.Failure = deliveryError("rejected", "target history no longer contains the original source baseline", target, nil)
	}
	if identity.Failure == nil {
		for _, ancestor := range ancestors {
			if err = m.requireAncestor(ctx, r.Common, ancestor, target); err != nil {
				identity.Failure = deliveryError("rejected", "target history does not contain dependency receipt "+ancestor, target, nil)
				break
			}
		}
	}
	if identity.Failure == nil {
		identity.Result = r
		tree, treeErr := m.git(ctx, r.Common, "rev-parse", "--verify", a.CandidateSHA+"^{tree}")
		if treeErr != nil {
			return reject(treeErr)
		}
		tree = strings.TrimSpace(tree)
		if target != a.BaseSHA && target != a.CandidateSHA {
			// The original candidate has exactly one parent, sourceBase; requiring
			// sourceBase in target prevents rewritten or unrelated target history.
			// Never merge the previously reconciled candidate (which would replay
			// unrelated changes from an earlier delivery head).
			var conflict *model.DeliveryError
			tree, conflict, err = m.mergeSource(ctx, r.Common, target, sourceCandidate)
			if err != nil {
				return reject(err)
			}
			if conflict != nil {
				conflict.ObservedSHA = target
				identity.Failure = conflict
			} else {
				name, email := m.cfg.GitName, m.cfg.GitEmail
				if name == "" {
					name = "OpenFactory"
				}
				if email == "" {
					email = "openfactory@localhost"
				}
				commit, commitErr := m.git(ctx, r.Common, "-c", "user.name="+name, "-c", "user.email="+email, "commit-tree", tree, "-p", target, "-m", "OpenFactory reconciliation "+a.ID+" generation "+fmt.Sprint(request.Generation))
				if commitErr != nil {
					return reject(commitErr)
				}
				identity.Changed = true
				identity.Result.Base = target
				identity.Result.Candidate = strings.TrimSpace(commit)
				identity.Result.SourceBase, identity.Result.SourceCandidate = sourceBase, sourceCandidate
				identity.Result.Path = filepath.Join(filepath.Dir(r.Path), "reconciled-"+key)
				identity.Result.Branch = "openfactory/" + digest(r.Project+"\x00"+a.ID+"\x00"+request.OperationID)
			}
		}
		identity.Fingerprint = digest(tree)
	}
	// Persist the chosen target, commit (or deterministic conflict) before
	// exporting a workspace. A crash cannot silently select a newer target for
	// an already issued operation ID.
	if err = saveExclusive(path, identity); err != nil {
		return reject(err)
	}
	return m.finishReconciliation(ctx, identity, key)
}

func (m *Manager) requireAncestor(ctx context.Context, common, ancestor, target string) error {
	base, err := m.git(ctx, common, "merge-base", ancestor, target)
	if err != nil {
		return err
	}
	if strings.TrimSpace(base) != ancestor {
		return errors.New("required commit is not an ancestor of target")
	}
	return nil
}

func (m *Manager) mergeSource(ctx context.Context, common, target, source string) (string, *model.DeliveryError, error) {
	out, err := runGit(ctx, common, gitArgs("merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", target, source), nil, m.secretValues(), 120)
	fields := strings.Split(out, "\x00")
	if err == nil {
		if len(fields) != 2 || fields[1] != "" || !shaPattern.MatchString(fields[0]) {
			return "", nil, errors.New("unexpected private merge-tree output")
		}
		return fields[0], nil, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(fields) < 2 || !shaPattern.MatchString(fields[0]) || fields[len(fields)-1] != "" {
		return "", nil, err
	}
	var paths []string
	seen := make(map[string]bool)
	for _, path := range fields[1 : len(fields)-1] {
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return "", deliveryError("conflict", "original feature delta conflicts with the current target", "", paths), nil
}

func (m *Manager) finishReconciliation(ctx context.Context, saved reconciliationReceipt, key string) (model.ReconciledCandidate, error) {
	if saved.Failure != nil {
		return model.ReconciledCandidate{}, saved.Failure
	}
	r := saved.Result
	reject := func(err error) (model.ReconciledCandidate, error) {
		return model.ReconciledCandidate{}, deliveryError("rejected", err.Error(), "", nil)
	}
	if saved.Changed {
		ref := "refs/openfactory/reconciliations/" + key
		current, err := m.git(ctx, r.Common, "rev-parse", "--verify", "--quiet", ref)
		if err == nil {
			if strings.TrimSpace(current) != r.Candidate {
				return reject(errors.New("private reconciliation ref changed"))
			}
		} else if _, err = m.git(ctx, r.Common, "update-ref", ref, r.Candidate, strings.Repeat("0", len(r.Candidate))); err != nil {
			return reject(err)
		}
		if data, err := os.ReadFile(m.recordPath(r.Path)); err == nil {
			var existing record
			if err = json.Unmarshal(data, &existing); err != nil {
				return reject(err)
			}
			if existing != r {
				return reject(errors.New("reconciliation workspace receipt changed"))
			}
		} else if !os.IsNotExist(err) {
			return reject(err)
		} else if err = saveExclusive(m.recordPath(r.Path), r); err != nil {
			return reject(err)
		}
		if _, err := os.Lstat(r.Path); os.IsNotExist(err) {
			if err = m.exportReconciliation(ctx, r); err != nil {
				return reject(err)
			}
		} else if err != nil {
			return reject(err)
		}
	}
	actual, err := m.Fingerprint(ctx, r.Path)
	if err != nil {
		return reject(err)
	}
	if actual != saved.Fingerprint {
		return reject(errors.New("reconciliation workspace changed after private capture"))
	}
	paths, err := m.ChangedPaths(ctx, r.Path, r.Base, r.Candidate)
	if err != nil {
		return reject(err)
	}
	for _, path := range paths {
		for _, pattern := range m.cfg.Projects[saved.Project].ProtectedPaths {
			if policy.Matches(pattern, path) {
				return reject(fmt.Errorf("protected path changed: %s", path))
			}
		}
	}
	return model.ReconciledCandidate{Workspace: model.Workspace{Path: r.Path, BaseSHA: r.Base}, CandidateSHA: r.Candidate, Fingerprint: saved.Fingerprint, Changed: saved.Changed}, nil
}

// Publish the complete checkout atomically, never repairing or deleting a
// previously returned agent workspace. Only this call's random staging tree is
// removed after interruption; its final operation-bound path is never replaced.
func (m *Manager) exportReconciliation(ctx context.Context, r record) error {
	shared, relative, err := m.sharedRoot(r.Path)
	if err != nil {
		return err
	}
	defer shared.Close()
	private, err := os.MkdirTemp(m.records(), "reconcile-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	source := filepath.Join(private, "source")
	if err = m.privateCheckout(ctx, r.Common, r.Candidate, source); err != nil {
		return err
	}
	staging := filepath.Join(filepath.Dir(r.Path), ".reconciliation-"+filepath.Base(private))
	stagingRelative := filepath.Join(filepath.Dir(relative), ".reconciliation-"+filepath.Base(private))
	defer shared.RemoveAll(stagingRelative)
	if err = m.copyCheckout(ctx, source, staging); err != nil {
		return err
	}
	if _, err = shared.Lstat(relative); err == nil {
		return errors.New("reconciliation workspace unexpectedly exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return shared.Rename(stagingRelative, relative)
}
