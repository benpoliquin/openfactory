package bb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openfactory/internal/model"
)

func wireProject(id, name, path string) map[string]any {
	return map[string]any{
		"id": id, "name": name,
		"sources": []any{map[string]any{"type": "local_path", "hostId": "host", "path": path}},
	}
}

func TestEnsureProjectConcurrentCallsCreateOneRegistration(t *testing.T) {
	const callers = 12
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var posts atomic.Int32
	var registered atomic.Bool
	hostRequests := make(chan struct{}, callers)
	var registration *projectRegistration
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/hosts":
			hostRequests <- struct{}{}
			hosts(w)
		case "/api/v1/projects":
			// Assert protection at the actual HTTP boundary as well as the final
			// registration count, independent of goroutine scheduling.
			if len(registration.gate) != 1 {
				t.Error("project list/create is outside its keyed registration lock")
			}
			if r.Method == http.MethodGet {
				if registered.Load() {
					respond(w, []any{wireProject("project", "repo", "/workspaces/repo")})
				} else {
					respond(w, []any{})
				}
				return
			}
			posts.Add(1)
			registered.Store(true)
			respond(w, map[string]any{"id": "project"})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	})
	var err error
	registration, err = c.lockProject(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	releaseRegistration := func() { release.Do(func() { <-registration.gate }) }
	defer releaseRegistration()
	type result struct {
		id  string
		err error
	}
	results := make(chan result, callers)
	for range callers {
		go func() {
			id, err := c.EnsureProject(ctx, "repo", "/workspaces/repo/.")
			results <- result{id, err}
		}()
	}
	// All callers have entered EnsureProject before admitting any registration.
	for range callers {
		select {
		case <-hostRequests:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	releaseRegistration()
	for range callers {
		select {
		case result := <-results:
			if result.err != nil || result.id != "project" {
				t.Fatalf("registration %q: %v", result.id, result.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if posts.Load() != 1 {
		t.Fatalf("created %d registrations, want one", posts.Load())
	}
}

func TestEnsureProjectReconcilesAmbiguousCreateWithoutAnotherPost(t *testing.T) {
	for _, outcome := range []string{"server_error", "malformed_response", "missing_id", "lost_response"} {
		t.Run(outcome, func(t *testing.T) {
			var posts atomic.Int32
			var lists atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/hosts":
					hosts(w)
				case "/api/v1/projects":
					if r.Method == http.MethodGet {
						lists.Add(1)
						if posts.Load() > 0 {
							respond(w, []any{wireProject("project", "repo", "/workspaces/repo")})
						} else {
							respond(w, []any{})
						}
						return
					}
					posts.Add(1)
					switch outcome {
					case "server_error":
						w.WriteHeader(http.StatusBadGateway)
					case "malformed_response":
						_, _ = w.Write([]byte("{"))
					case "missing_id":
						respond(w, map[string]any{"name": "repo"})
					case "lost_response":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			})
			for range 2 {
				id, err := c.EnsureProject(context.Background(), "repo", "/workspaces/repo")
				if err != nil || id != "project" {
					t.Fatalf("reconciled registration %q: %v", id, err)
				}
			}
			if posts.Load() != 1 || lists.Load() != 3 {
				t.Fatalf("posts=%d lists=%d, want one POST and three listings", posts.Load(), lists.Load())
			}
		})
	}
}

func TestEnsureProjectRetainsUnresolvedCreateAndKeepsListing(t *testing.T) {
	for _, listing := range []string{"absent", "unavailable", "collision"} {
		t.Run(listing, func(t *testing.T) {
			var posts atomic.Int32
			var visible atomic.Bool
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/hosts":
					hosts(w)
				case "/api/v1/projects":
					if r.Method == http.MethodPost {
						posts.Add(1)
						w.WriteHeader(http.StatusGatewayTimeout)
						return
					}
					if posts.Load() == 0 {
						respond(w, []any{})
					} else if visible.Load() {
						respond(w, []any{wireProject("project", "repo", "/workspaces/repo")})
					} else {
						switch listing {
						case "absent":
							respond(w, []any{})
						case "unavailable":
							w.WriteHeader(http.StatusServiceUnavailable)
						case "collision":
							respond(w, []any{wireProject("other", "repo", "/other")})
						}
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			})
			for range 2 {
				id, err := c.EnsureProject(context.Background(), "repo", "/workspaces/repo")
				var requestErr *RequestError
				if id != "" || !errors.As(err, &requestErr) || !requestErr.Uncertain() {
					t.Fatalf("unresolved create %q: %v", id, err)
				}
			}
			visible.Store(true)
			id, err := c.EnsureProject(context.Background(), "repo", "/workspaces/repo")
			if err != nil || id != "project" || posts.Load() != 1 {
				t.Fatalf("late reconciliation %q: %v; posts=%d", id, err, posts.Load())
			}
		})
	}
}

func TestProjectRegistrationDoesNotBlockOtherProjectsOrIndependentSpawns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registrationStarted := make(chan struct{})
	releaseRegistration := make(chan struct{})
	var registrationRelease sync.Once
	release := func() { registrationRelease.Do(func() { close(releaseRegistration) }) }
	defer release()
	type spawnInput struct {
		workspace string
		title     string
	}
	spawnInputs := make(chan spawnInput, 2)
	releaseSpawns := make(chan struct{})
	var spawnRelease sync.Once
	unblockSpawns := func() { spawnRelease.Do(func() { close(releaseSpawns) }) }
	defer unblockSpawns()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/hosts":
			hosts(w)
		case "/api/v1/projects":
			if r.Method == http.MethodGet {
				respond(w, []any{})
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body.Name == "repo" {
				close(registrationStarted)
				select {
				case <-releaseRegistration:
				case <-r.Context().Done():
					return
				}
			}
			respond(w, map[string]any{"id": body.Name})
		case "/api/v1/threads":
			var body struct {
				ProjectID   string `json:"projectId"`
				Title       string `json:"title"`
				Environment struct {
					Type      string `json:"type"`
					HostID    string `json:"hostId"`
					Workspace struct {
						Type string `json:"type"`
						Path string `json:"path"`
					} `json:"workspace"`
				} `json:"environment"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body.ProjectID != "project" || body.Environment.Type != "host" || body.Environment.HostID != "host" || body.Environment.Workspace.Type != "unmanaged" {
				t.Errorf("incorrect independent environment binding: %#v", body)
			}
			spawnInputs <- spawnInput{body.Environment.Workspace.Path, body.Title}
			select {
			case <-releaseSpawns:
			case <-r.Context().Done():
				return
			}
			respond(w, map[string]any{"id": body.Title, "projectId": "project", "environmentId": "env-" + body.Title, "providerId": provider})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	})
	type projectResult struct {
		id  string
		err error
	}
	blockedProject := make(chan projectResult, 1)
	go func() {
		id, err := c.EnsureProject(ctx, "repo", "/workspaces/repo")
		blockedProject <- projectResult{id, err}
	}()
	select {
	case <-registrationStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	otherID, err := c.EnsureProject(ctx, "other", "/workspaces/other")
	if err != nil || otherID != "other" {
		t.Fatalf("unrelated registration blocked: %q %v", otherID, err)
	}
	type spawnResult struct {
		session model.Session
		err     error
	}
	results := make(chan spawnResult, 2)
	for i := range 2 {
		go func() {
			id := "attempt-" + strconv.Itoa(i)
			session, err := c.Spawn(ctx, model.SpawnRequest{AttemptID: id, ProjectID: "project", Workspace: "/workspaces/" + id, Model: "astra", Prompt: "Build it"})
			results <- spawnResult{session, err}
		}()
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case input := <-spawnInputs:
			for i := range 2 {
				id := "attempt-" + strconv.Itoa(i)
				if input.workspace == "/workspaces/"+id && input.title == "[openfactory-attempt:"+id+"]" {
					seen[id] = true
				}
			}
		case <-ctx.Done():
			t.Fatalf("independent spawns did not overlap: %v", ctx.Err())
		}
	}
	if len(seen) != 2 {
		t.Fatalf("attempt workspaces or markers were shared: %#v", seen)
	}
	unblockSpawns()
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil || result.session.ThreadID == "" || result.session.EnvironmentID != "env-"+result.session.ThreadID {
				t.Fatalf("independent spawn %#v: %v", result.session, result.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	release()
	select {
	case result := <-blockedProject:
		if result.err != nil || result.id != "repo" {
			t.Fatalf("blocked registration %q: %v", result.id, result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestProjectRegistrationLockHonorsCancellationAndIndependentNames(t *testing.T) {
	c := New(model.Config{})
	registration, err := c.lockProject(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { <-registration.gate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.lockProject(ctx, "repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting registration ignored cancellation: %v", err)
	}
	other, err := c.lockProject(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	<-other.gate
}
