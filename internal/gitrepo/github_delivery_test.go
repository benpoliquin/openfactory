package gitrepo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"openfactory/internal/model"
)

func pullFixture(t *testing.T) (*fixture, model.Feature) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	put(t, token, "test-token")
	f := setup(t, func(p *model.Project) { p.GitHubRepository = "owner/repo"; p.Auth.TokenFile = token })
	f.candidate(t)
	return f, model.Feature{ID: f.a.FeatureID, Project: "sample", Title: "Feature", Revision: "approved"}
}

func TestPRCreateOnlyLeaseDoesNotOverwriteConcurrentCandidateRef(t *testing.T) {
	f, feature := pullFixture(t)
	var mu sync.Mutex
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			posts++
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	f.m.apiURL = server.URL
	branch := "openfactory/" + digest("sample\x00"+f.a.ID)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// The unexpected ref is an ancestor of the candidate: ordinary push would
	// overwrite it successfully. Publication must use an absent-ref lease.
	installGitPushInterposition(t, quote(realGit)+" --git-dir="+quote(f.remote)+" update-ref "+quote("refs/heads/"+branch)+" "+quote(f.a.BaseSHA)+" '' || exit 94")
	_, err = f.m.PullRequest(context.Background(), "sample", f.p, f.a, feature)
	requireDeliveryKind(t, err, "rejected")
	if actual := testGit(t, f.remote, "rev-parse", "refs/heads/"+branch); actual != f.a.BaseSHA {
		t.Fatalf("overwrote concurrent candidate branch: %s", actual)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Fatalf("created PR after rejected branch publication: %d", posts)
	}
}

func TestPRStaleTargetAndUnexpectedRefNeverPublish(t *testing.T) {
	for _, scenario := range []string{"target", "candidate-ref"} {
		t.Run(scenario, func(t *testing.T) {
			f, feature := pullFixture(t)
			branch := "openfactory/" + digest("sample\x00"+f.a.ID)
			if scenario == "target" {
				put(t, filepath.Join(f.seed, "external.txt"), "external\n")
				testGit(t, f.seed, "add", "external.txt")
				testGit(t, f.seed, "commit", "-m", "external")
				testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
			} else {
				testGit(t, f.remote, "update-ref", "refs/heads/"+branch, f.a.BaseSHA)
			}
			before := testGit(t, f.remote, "show-ref")
			_, err := f.m.PullRequest(context.Background(), "sample", f.p, f.a, feature)
			kind := "rejected"
			if scenario == "target" {
				kind = "target_moved"
			}
			requireDeliveryKind(t, err, kind)
			if after := testGit(t, f.remote, "show-ref"); after != before {
				t.Fatal("PR admission changed remote refs")
			}
		})
	}
}

func TestPRPartialPublicationAndUnmatchedBaseRemainUncertain(t *testing.T) {
	for _, scenario := range []string{"lost-api", "wrong-base", "target-after-publication"} {
		t.Run(scenario, func(t *testing.T) {
			f, feature := pullFixture(t)
			var mu sync.Mutex
			created, posts := false, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodPost {
					posts++
					created = true
					if scenario == "target-after-publication" {
						// Simulate an external main update after publication. GitHub's
						// apparently matching response cannot make this proof current.
						testGit(t, f.remote, "update-ref", "refs/heads/main", f.a.CandidateSHA, f.a.BaseSHA)
					}
					http.Error(w, "lost acknowledgement", http.StatusInternalServerError)
					return
				}
				if !created {
					_, _ = w.Write([]byte("[]"))
					return
				}
				if scenario == "lost-api" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				base := f.a.BaseSHA
				if scenario == "wrong-base" {
					base = f.a.CandidateSHA
				}
				branch := "openfactory/" + digest("sample\x00"+f.a.ID)
				_ = json.NewEncoder(w).Encode([]any{map[string]any{"html_url": "https://github.com/owner/repo/pull/2", "state": "open", "head": map[string]string{"ref": branch, "sha": f.a.CandidateSHA}, "base": map[string]string{"ref": "main", "sha": base}}})
			}))
			defer server.Close()
			f.m.apiURL = server.URL
			_, err := f.m.PullRequest(context.Background(), "sample", f.p, f.a, feature)
			requireDeliveryKind(t, err, "uncertain")
			branch := "openfactory/" + digest("sample\x00"+f.a.ID)
			if got := testGit(t, f.seed, "ls-remote", f.remote, "refs/heads/"+branch); !strings.HasPrefix(got, f.a.CandidateSHA) {
				t.Fatalf("uncertain publication lost exact candidate identity: %s", got)
			}
			mu.Lock()
			if posts != 1 {
				t.Errorf("unexpected mutation count: %d", posts)
			}
			mu.Unlock()
		})
	}
}

func TestPRPublishedBranchWithMovedBaseIsNotSafeReconciliationRetry(t *testing.T) {
	f, feature := pullFixture(t)
	r, err := f.m.workspace(context.Background(), f.a.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.publishCandidate(context.Background(), r, f.p, r.Branch, f.a.CandidateSHA, ""); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.seed, "external.txt"), "external\n")
	testGit(t, f.seed, "add", "external.txt")
	testGit(t, f.seed, "commit", "-m", "external")
	testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/main")
	_, err = f.m.PullRequest(context.Background(), "sample", f.p, f.a, feature)
	requireDeliveryKind(t, err, "uncertain")
}
