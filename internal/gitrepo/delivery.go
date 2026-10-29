package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"openfactory/internal/config"
	"openfactory/internal/model"
)

// CheckCapabilities admits the merge-tree implementation used for private
// reconciliation. Older Git versions must not fall back to agent worktree merges.
func CheckCapabilities(ctx context.Context) error {
	out, err := runGit(ctx, "", gitArgs("--version"), nil, nil, 10)
	if err != nil {
		return fmt.Errorf("Git capability check: %w", err)
	}
	var major, minor int
	if _, err = fmt.Sscanf(strings.TrimSpace(out), "git version %d.%d", &major, &minor); err != nil || major < 2 || (major == 2 && minor < 38) {
		return fmt.Errorf("Git 2.38 or newer is required for private merge-tree reconciliation (found %q)", strings.TrimSpace(out))
	}
	return nil
}

func (m *Manager) lockRepository(p model.Project) func() {
	identity, err := config.RepositoryIdentity(p)
	if err != nil {
		// Enrollment validation reports invalid repositories before any operation.
		identity = p.Repository
	}
	return m.lock(identity)
}

func deliveryError(kind, message, observed string, paths []string) *model.DeliveryError {
	return &model.DeliveryError{Kind: kind, Message: message, ObservedSHA: observed, Paths: paths}
}

func (m *Manager) fetchTarget(ctx context.Context, r record, p model.Project) (string, error) {
	if _, err := m.network(ctx, r.Common, p, "fetch", "--no-tags", "--no-recurse-submodules", "--", p.Repository, "refs/heads/"+p.Branch); err != nil {
		return "", err
	}
	out, err := m.git(ctx, r.Common, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("unexpected fetched target commit")
	}
	return sha, nil
}

func (m *Manager) remoteHeadOptional(ctx context.Context, r record, p model.Project, branch string) (string, error) {
	ref := "refs/heads/" + branch
	out, err := m.network(ctx, r.Common, p, "ls-remote", "--heads", "--", p.Repository, ref)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != ref || !shaPattern.MatchString(fields[0]) {
		return "", fmt.Errorf("unexpected remote branch state")
	}
	return fields[0], nil
}

// publishCandidate updates exactly one ref with an explicit expected-old lease.
// An empty expected SHA is a create-only lease, never permission to overwrite.
func (m *Manager) publishCandidate(ctx context.Context, r record, p model.Project, branch, candidate, expected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", deliveryError("rejected", err.Error(), "", nil)
	}
	env, secrets, err := m.auth(p)
	if err != nil {
		return "", deliveryError("rejected", err.Error(), "", nil)
	}
	ref := "refs/heads/" + branch
	out, pushErr := runGit(ctx, r.Common, gitArgs("push", "--porcelain", "--force-with-lease="+ref+":"+expected, "--", p.Repository, candidate+":"+ref), env, append(secrets, m.secretValues()...), 120)
	actual, observeErr := m.remoteHeadOptional(ctx, r, p, branch)
	if observeErr == nil && actual == candidate {
		return actual, nil
	}
	var startError *exec.Error
	var pathError *os.PathError
	if errors.As(pushErr, &startError) || errors.As(pushErr, &pathError) {
		return "", deliveryError("rejected", "candidate publication could not start: "+pushErr.Error(), "", nil)
	}
	// Only explicit local/server refusals prove no mutation. In particular,
	// "[remote failure]" may mean the server applied the pack but lost its
	// per-ref report; the "!" flag alone is not a rejection receipt.
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 3 && fields[0] == "!" && strings.HasSuffix(fields[1], ":"+ref) && (strings.HasPrefix(fields[2], "[rejected]") || strings.HasPrefix(fields[2], "[remote rejected]")) {
			if expected != "" && observeErr == nil && actual != expected {
				return "", deliveryError("target_moved", "target changed before the exact ref lease could be applied", actual, nil)
			}
			return "", deliveryError("rejected", "remote rejected candidate publication: "+fields[2], actual, nil)
		}
	}
	message := "candidate publication outcome uncertain; do not retry"
	if pushErr != nil {
		message += ": " + pushErr.Error()
	} else if observeErr != nil {
		message += ": " + observeErr.Error()
	}
	return "", deliveryError("uncertain", message, actual, nil)
}
