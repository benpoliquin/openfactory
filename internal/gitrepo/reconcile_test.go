package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func reconcileRequest(a model.Attempt, operation string, generation int) model.ReconcileRequest {
	a.SourceBaseSHA, a.SourceCandidateSHA = a.BaseSHA, a.CandidateSHA
	return model.ReconcileRequest{OperationID: operation, Generation: generation, Attempt: a, Feature: model.Feature{ID: a.FeatureID, Project: "sample"}}
}

func requireDeliveryKind(t *testing.T, err error, kind string) *model.DeliveryError {
	t.Helper()
	var delivery *model.DeliveryError
	if !errors.As(err, &delivery) || delivery.Kind != kind || delivery.Uncertain() != (kind == "uncertain") {
		t.Fatalf("wanted %s delivery error, got %v", kind, err)
	}
	return delivery
}

func TestThreeIndependentSourcesReconcileOntoSuccessiveHeads(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	var requests []model.ReconcileRequest
	var results []model.ReconciledCandidate
	// All three features capture the same original baseline before any delivery.
	for i := range 3 {
		id := fmt.Sprintf("parallel-%d", i)
		w, err := f.m.Prepare(ctx, "sample", f.p, id)
		if err != nil {
			t.Fatal(err)
		}
		put(t, filepath.Join(w.Path, id+".txt"), id+"\n")
		sha, err := f.m.Commit(ctx, w.Path, id)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.m.Snapshot(ctx, w.Path, sha)
		if err != nil {
			t.Fatal(err)
		}
		a := model.Attempt{ID: id, FeatureID: id, BaseSHA: w.BaseSHA, CandidateSHA: sha, Workspace: snapshot.Path}
		requests = append(requests, reconcileRequest(a, "reconcile-"+id, 2))
	}
	previous := requests[0].Attempt.BaseSHA
	for i, request := range requests {
		if i > 0 {
			request.RequiredAncestors = []string{previous}
			requests[i] = request
		}
		before, err := f.m.Fingerprint(ctx, request.Attempt.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.m.Reconcile(ctx, "sample", f.p, request)
		if err != nil {
			t.Fatalf("feature %d: %v", i, err)
		}
		if got.Changed != (i > 0) || got.Workspace.BaseSHA != previous || got.Fingerprint == "" {
			t.Fatalf("feature %d reconciliation: %+v", i, got)
		}
		if i == 0 && (got.Workspace.Path != request.Attempt.Workspace || got.CandidateSHA != request.Attempt.CandidateSHA || got.Fingerprint != before) {
			t.Fatal("unchanged base discarded existing evidence identity")
		}
		if i > 0 && (got.Workspace.Path == request.Attempt.Workspace || filepath.Dir(got.Workspace.Path) != filepath.Dir(request.Attempt.Workspace) || got.CandidateSHA == request.Attempt.CandidateSHA) {
			t.Fatal("reconciliation did not create a fresh project-local candidate")
		}
		original, err := f.m.Fingerprint(ctx, request.Attempt.Workspace)
		if err != nil || original != before {
			t.Fatalf("reconciliation edited original agent workspace: %v", err)
		}
		a := request.Attempt
		a.Workspace, a.BaseSHA, a.CandidateSHA = got.Workspace.Path, got.Workspace.BaseSHA, got.CandidateSHA
		paths, err := f.m.ChangedPaths(ctx, a.Workspace, a.BaseSHA, a.CandidateSHA)
		if err != nil || !reflect.DeepEqual(paths, []string{a.ID + ".txt"}) {
			t.Fatalf("feature delta included earlier delivery: %v %v", paths, err)
		}
		r, err := f.m.workspace(ctx, a.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		if parents := testGit(t, r.Common, "rev-list", "--parents", "-n", "1", a.CandidateSHA); parents != a.CandidateSHA+" "+previous {
			t.Fatalf("not single-parent exact target: %s", parents)
		}
		if i > 0 {
			if _, err = f.m.Commit(ctx, a.Workspace, "must not repair original delta"); err == nil {
				t.Fatal("recaptured an immutable reconciled source")
			}
		}
		receipt, err := f.m.Integrate(ctx, "sample", f.p, a, a.BaseSHA)
		if err != nil || receipt != got.CandidateSHA {
			t.Fatalf("delivery %d: %s %v", i, receipt, err)
		}
		previous = receipt
		results = append(results, got)
	}
	for i, request := range requests {
		// A fresh manager after subsequent deliveries must replay the chosen
		// operation, not silently manufacture a candidate on the newest target.
		got, err := New(f.m.cfg).Reconcile(ctx, "sample", f.p, request)
		if err != nil || !reflect.DeepEqual(got, results[i]) {
			t.Fatalf("operation replay %d changed: %+v %v", i, got, err)
		}
		if content := testGit(t, f.remote, "show", "refs/heads/main:"+request.Attempt.ID+".txt"); content != request.Attempt.ID {
			t.Fatalf("lost feature %d: %q", i, content)
		}
	}
}

func TestReconciliationConflictIsDeterministicAndNeverPublishes(t *testing.T) {
	f := setup(t, nil)
	f.candidate(t)
	put(t, filepath.Join(f.seed, "app.txt"), "external conflicting update\n")
	testGit(t, f.seed, "add", "app.txt")
	testGit(t, f.seed, "commit", "-m", "conflicting target")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	before := testGit(t, f.seed, "ls-remote", f.remote)
	request := reconcileRequest(f.a, "conflict", 2)
	_, err := f.m.Reconcile(context.Background(), "sample", f.p, request)
	first := requireDeliveryKind(t, err, "conflict")
	if !reflect.DeepEqual(first.Paths, []string{"app.txt"}) || first.ObservedSHA == "" {
		t.Fatalf("conflict paths not exact: %+v", first)
	}
	_, err = New(f.m.cfg).Reconcile(context.Background(), "sample", f.p, request)
	second := requireDeliveryKind(t, err, "conflict")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("conflict changed across recovery: %+v %+v", first, second)
	}
	if after := testGit(t, f.seed, "ls-remote", f.remote); after != before {
		t.Fatal("conflicting reconciliation changed remote refs")
	}
	if content, err := os.ReadFile(filepath.Join(f.a.Workspace, "app.txt")); err != nil || string(content) != "candidate\n" {
		t.Fatalf("conflict edited agent source: %q %v", content, err)
	}
}

func TestReconciliationRejectsForgedSourceDependenciesAndRewrittenHistory(t *testing.T) {
	for _, scenario := range []string{"source", "dependency", "history", "generation", "private-config"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t, nil)
			f.candidate(t)
			request := reconcileRequest(f.a, "guard", 2)
			switch scenario {
			case "source":
				request.Attempt.SourceCandidateSHA = request.Attempt.BaseSHA
			case "dependency":
				request.RequiredAncestors = []string{request.Attempt.CandidateSHA}
			case "history":
				testGit(t, f.seed, "checkout", "--orphan", "rewritten")
				testGit(t, f.seed, "add", "app.txt")
				testGit(t, f.seed, "commit", "-m", "rewritten root")
				testGit(t, f.seed, "push", "--force", f.remote, "HEAD:refs/heads/main")
			case "generation":
				if _, err := f.m.Reconcile(context.Background(), "sample", f.p, request); err != nil {
					t.Fatal(err)
				}
				request.Generation++
			case "private-config":
				common := filepath.Join(f.m.cfg.StateDir, "git-repositories", digest("sample")+".git")
				put(t, filepath.Join(common, "config"), "[core]\n bare=true\n[merge]\n tool=untrusted\n")
			}
			before := testGit(t, f.seed, "ls-remote", f.remote)
			_, err := f.m.Reconcile(context.Background(), "sample", f.p, request)
			requireDeliveryKind(t, err, "rejected")
			if after := testGit(t, f.seed, "ls-remote", f.remote); after != before {
				t.Fatal("rejected reconciliation mutated remote")
			}
		})
	}
}

func TestRawIntegrationRefusesStaleBaseAndCandidateArgument(t *testing.T) {
	f := setup(t, nil)
	f.candidate(t)
	_, err := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.CandidateSHA)
	requireDeliveryKind(t, err, "rejected")
	put(t, filepath.Join(f.seed, "external.txt"), "external\n")
	testGit(t, f.seed, "add", "external.txt")
	testGit(t, f.seed, "commit", "-m", "external")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	before := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main")
	_, err = f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA)
	moved := requireDeliveryKind(t, err, "target_moved")
	if !strings.HasPrefix(before, moved.ObservedSHA) || moved.ObservedSHA == "" {
		t.Fatalf("missing exact observed head: %+v", moved)
	}
	if after := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/main"); after != before {
		t.Fatal("raw stale-base integration overwrote target")
	}
}

func installGitPushInterposition(t *testing.T, body string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "git")
	put(t, wrapper, "#!/bin/sh\nfor openfactory_arg in \"$@\"; do\n if [ \"$openfactory_arg\" = push ]; then\n"+body+"\n fi\ndone\nexec "+quote(realGit)+" \"$@\"\n")
	if err = os.Chmod(wrapper, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestIntegrationExactLeaseRejectsEvenFastForwardExternalRace(t *testing.T) {
	f := setup(t, nil)
	original := f.a.BaseSHA
	put(t, filepath.Join(f.seed, "checkpoint.txt"), "checkpoint\n")
	testGit(t, f.seed, "add", "checkpoint.txt")
	testGit(t, f.seed, "commit", "-m", "new baseline")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	w, err := f.m.Prepare(context.Background(), "sample", f.p, "newer-baseline")
	if err != nil {
		t.Fatal(err)
	}
	f.a.ID, f.a.Workspace, f.a.BaseSHA = "newer-baseline", w.Path, w.BaseSHA
	f.candidate(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Moving backwards would still permit a normal fast-forward push. Only an
	// exact expected-old lease prevents overwriting this external ref update.
	installGitPushInterposition(t, quote(realGit)+" --git-dir="+quote(f.remote)+" update-ref refs/heads/main "+quote(original)+" "+quote(f.a.BaseSHA)+" || exit 91")
	_, err = f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA)
	moved := requireDeliveryKind(t, err, "target_moved")
	if moved.ObservedSHA != original {
		t.Fatalf("race observation %q != %q", moved.ObservedSHA, original)
	}
	if head := testGit(t, f.remote, "rev-parse", "refs/heads/main"); head != original {
		t.Fatalf("external race was overwritten: %s", head)
	}
}

func TestAmbiguousPushRemainsUncertainWhenRemoteDiffers(t *testing.T) {
	f := setup(t, nil)
	f.candidate(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	body := quote(realGit) + " \"$@\" || exit $?\n" + quote(realGit) + " --git-dir=" + quote(f.remote) + " update-ref refs/heads/main " + quote(f.a.BaseSHA) + " " + quote(f.a.CandidateSHA) + " || exit 92\nexit 93"
	installGitPushInterposition(t, body)
	_, err = f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA)
	uncertain := requireDeliveryKind(t, err, "uncertain")
	if uncertain.ObservedSHA != f.a.BaseSHA {
		t.Fatalf("ambiguous push lost observation: %+v", uncertain)
	}
}

func TestEveryReconciliationUsesImmutableOriginalDelta(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	put(t, filepath.Join(f.a.Workspace, "feature.txt"), "feature\n")
	sha, err := f.m.Commit(ctx, f.a.Workspace, "independent feature")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.m.Snapshot(ctx, f.a.Workspace, sha)
	if err != nil {
		t.Fatal(err)
	}
	f.a.Workspace, f.a.CandidateSHA = snapshot.Path, sha
	request := reconcileRequest(f.a, "first-head", 2)
	put(t, filepath.Join(f.seed, "obsolete-external.txt"), "first target only\n")
	testGit(t, f.seed, "add", "obsolete-external.txt")
	testGit(t, f.seed, "commit", "-m", "first target")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	first, err := f.m.Reconcile(ctx, "sample", f.p, request)
	if err != nil || !first.Changed {
		t.Fatalf("first reconcile: %+v %v", first, err)
	}
	// Another writer replaces that intermediate head, but retains the original
	// approved source baseline. Replaying the first reconciliation would import
	// obsolete-external.txt; replaying the immutable feature delta must not.
	testGit(t, f.seed, "checkout", "-b", "replacement-head", f.a.BaseSHA)
	put(t, filepath.Join(f.seed, "replacement-external.txt"), "current target only\n")
	testGit(t, f.seed, "add", "replacement-external.txt")
	testGit(t, f.seed, "commit", "-m", "replacement target")
	testGit(t, f.seed, "push", "--force", f.remote, "HEAD:refs/heads/main")
	request.OperationID, request.Generation = "second-head", 3
	request.Attempt.Workspace, request.Attempt.BaseSHA, request.Attempt.CandidateSHA = first.Workspace.Path, first.Workspace.BaseSHA, first.CandidateSHA
	second, err := f.m.Reconcile(ctx, "sample", f.p, request)
	if err != nil || !second.Changed {
		t.Fatalf("second reconcile: %+v %v", second, err)
	}
	if _, err = os.Stat(filepath.Join(second.Workspace.Path, "obsolete-external.txt")); !os.IsNotExist(err) {
		t.Fatalf("previous target delta leaked into new candidate: %v", err)
	}
	for _, path := range []string{"feature.txt", "replacement-external.txt"} {
		if _, err = os.Stat(filepath.Join(second.Workspace.Path, path)); err != nil {
			t.Fatalf("missing %s in original-delta reconciliation: %v", path, err)
		}
	}
	r, err := f.m.workspace(ctx, second.Workspace.Path)
	if err != nil || r.SourceBase != f.a.BaseSHA || r.SourceCandidate != sha {
		t.Fatalf("original identities changed: %+v %v", r, err)
	}
}

func TestGitCapabilityMinimum(t *testing.T) {
	for _, version := range []string{"2.37.9", "2.38.0", "3.0.0"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			wrapper := filepath.Join(dir, "git")
			put(t, wrapper, "#!/bin/sh\nprintf '%s\\n' "+quote("git version "+version)+"\n")
			if err := os.Chmod(wrapper, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			err := CheckCapabilities(context.Background())
			if (version == "2.37.9") != (err != nil) {
				t.Fatalf("capability admission %s: %v", version, err)
			}
		})
	}
}

func TestReconciledReleasePreservesPathAdmissionAndExactCandidate(t *testing.T) {
	f := setup(t, func(p *model.Project) {
		p.Releases = map[string]model.Release{"staging": {
			Allowed: true, Qualified: true, AllowedRisks: []string{"low"}, AllowedPaths: []string{"app.txt"}, ExpiresAt: time.Now().Add(time.Hour),
			Deploy:  model.Command{Argv: []string{"sh", "-c", "test -f external.txt && test -n \"$OPENFACTORY_SHA\""}},
			Observe: model.Command{Argv: []string{"true"}}, Rollback: model.Command{Argv: []string{"true"}}, ObservationSeconds: 1, MinimumObservations: 1,
		}}
	})
	f.candidate(t)
	put(t, filepath.Join(f.seed, "external.txt"), "unrelated delivery\n")
	testGit(t, f.seed, "add", "external.txt")
	testGit(t, f.seed, "commit", "-m", "unrelated delivery")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	request := reconcileRequest(f.a, "release-current-head", 2)
	got, err := f.m.Reconcile(context.Background(), "sample", f.p, request)
	if err != nil || !got.Changed {
		t.Fatalf("release reconciliation: %+v %v", got, err)
	}
	a := request.Attempt
	a.Workspace, a.BaseSHA, a.CandidateSHA, a.CandidateGeneration = got.Workspace.Path, got.Workspace.BaseSHA, got.CandidateSHA, 2
	feature := request.Feature
	feature.Risk, feature.CompletionTarget = "low", "staging"
	if err = f.m.PreflightRelease(context.Background(), "sample", f.p, a, feature); err != nil {
		t.Fatalf("unrelated target path polluted release allowlist: %v", err)
	}
	if _, err = f.m.Integrate(context.Background(), "sample", f.p, a, a.BaseSHA); err != nil {
		t.Fatal(err)
	}
	evidence, err := f.m.Release(context.Background(), "sample", f.p, a, feature)
	if err != nil || len(evidence) != 2 {
		t.Fatalf("reconciled release: %+v %v", evidence, err)
	}
	for _, item := range evidence {
		if !item.Passed || item.CandidateSHA != a.CandidateSHA || item.CandidateGeneration != 2 {
			t.Fatalf("release lost reconciled candidate identity: %+v", item)
		}
	}
}
