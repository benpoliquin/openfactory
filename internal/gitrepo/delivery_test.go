package gitrepo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRemoteFailurePushStatusRetainsUncertainPublication(t *testing.T) {
	for _, target := range []string{"main", "create-only-pr"} {
		for _, observation := range []string{"unavailable", "outside-head", "exact-candidate"} {
			t.Run(target+"/"+observation, func(t *testing.T) {
				f, feature := pullFixture(t)
				branch := f.p.Branch
				if target == "create-only-pr" {
					branch = "openfactory/" + digest("sample\x00"+f.a.ID)
				}
				ref := "refs/heads/" + branch
				// Make a third commit available without moving the delivery target.
				// It represents an outside writer moving the ref after our write.
				put(t, filepath.Join(f.seed, "outside.txt"), "outside writer\n")
				testGit(t, f.seed, "add", "outside.txt")
				testGit(t, f.seed, "commit", "-m", "outside writer")
				outside := testGit(t, f.seed, "rev-parse", "HEAD")
				testGit(t, f.seed, "push", f.remote, "HEAD:refs/heads/outside")
				var posts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						posts.Add(1)
					}
					_, _ = w.Write([]byte("[]"))
				}))
				defer server.Close()
				f.m.apiURL = server.URL
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				marker := filepath.Join(dir, "push-attempted")
				var script strings.Builder
				script.WriteString("#!/bin/sh\nfor openfactory_arg in \"$@\"; do\n case \"$openfactory_arg\" in\n push)\n")
				// Apply the real push, then hide the successful acknowledgement.
				script.WriteString(quote(realGit) + " \"$@\" >/dev/null || exit $?\n")
				script.WriteString("printf applied > " + quote(marker) + "\n")
				if observation == "outside-head" {
					script.WriteString(quote(realGit) + " --git-dir=" + quote(f.remote) + " update-ref " + quote(ref) + " " + quote(outside) + " " + quote(f.a.CandidateSHA) + " || exit 96\n")
				}
				script.WriteString("printf '%s\\n' " + quote("!\t"+f.a.CandidateSHA+":"+ref+"\t[remote failure] (remote failed to report status)") + "\nexit 97\n;;\n ls-remote)\n")
				if observation == "unavailable" {
					script.WriteString("if [ -f " + quote(marker) + " ]; then exit 98; fi\n")
				}
				script.WriteString(";;\n esac\ndone\nexec " + quote(realGit) + " \"$@\"\n")
				wrapper := filepath.Join(dir, "git")
				put(t, wrapper, script.String())
				if err = os.Chmod(wrapper, 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				if target == "main" {
					receipt, err := f.m.Integrate(context.Background(), "sample", f.p, f.a, f.a.BaseSHA)
					if observation == "exact-candidate" {
						if err != nil || receipt != f.a.CandidateSHA {
							t.Fatalf("exact observed candidate did not settle publication: %s %v", receipt, err)
						}
					} else {
						requireDeliveryKind(t, err, "uncertain")
					}
				} else if observation == "exact-candidate" {
					// Exercise the shared create-only publisher directly: an exact
					// observed ref settles the Git mutation, independent of PR API.
					r, err := f.m.workspace(context.Background(), f.a.Workspace)
					if err != nil {
						t.Fatal(err)
					}
					receipt, err := f.m.publishCandidate(context.Background(), r, f.p, branch, f.a.CandidateSHA, "")
					if err != nil || receipt != f.a.CandidateSHA {
						t.Fatalf("exact create-only candidate did not settle publication: %s %v", receipt, err)
					}
				} else {
					_, err := f.m.PullRequest(context.Background(), "sample", f.p, f.a, feature)
					requireDeliveryKind(t, err, "uncertain")
				}
				want := f.a.CandidateSHA
				if observation == "outside-head" {
					want = outside
				}
				if actual := testGit(t, f.remote, "rev-parse", ref); actual != want {
					t.Fatalf("test did not exercise an applied write: %s != %s", actual, want)
				}
				if posts.Load() != 0 {
					t.Fatal("PR creation proceeded after an unconfirmed branch publication")
				}
			})
		}
	}
}
