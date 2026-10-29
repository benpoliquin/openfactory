// Package bb implements BB's public HTTP API. Wire contracts below are taken
// from bb-app@0.47.1's published dist/index.js, dist/index.d.ts and packaged
// server/dist/start-server.js (npm gitHead 5e1acd765039e874643f34c4d35409e58484e408),
// not the moving main branch. The SDK's threads.spawn sends a nonempty input;
// it does not expose an empty-thread creation primitive.
package bb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"openfactory/internal/model"
)

const Version = "0.47.1"
const provider = "acp-omp"
const pageSize = 100
const responseLimit = 9 << 20
const outputLimit = 1 << 20

// RequestError distinguishes a rejection from an uncertain mutation. Callers
// must reconcile uncertain Spawn results through Find, never blindly retry.
// Read failures also use this type, but have no mutation to reconcile.
type RequestError struct {
	Operation string
	Status    int
	Cause     error
	uncertain bool
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("BB %s (HTTP %d): %v", e.Operation, e.Status, e.Cause)
}
func (e *RequestError) Unwrap() error   { return e.Cause }
func (e *RequestError) Uncertain() bool { return e.uncertain }

// Client does not retry HTTP requests or follow redirects (especially POSTs).
type Client struct {
	cfg       model.Config
	http      *http.Client
	projectMu sync.Mutex
	projects  map[string]*projectRegistration
}

type projectRegistration struct {
	gate      chan struct{}
	uncertain error
}

var _ model.Backend = (*Client)(nil)

func New(cfg model.Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func reject(op, message string) error {
	return &RequestError{Operation: op, Cause: errors.New(message)}
}
func uncertain(op, message string) error {
	return &RequestError{Operation: op, Cause: errors.New(message), uncertain: true}
}

func (c *Client) request(ctx context.Context, method, route string, query url.Values, body, result any) error {
	mutation := method != http.MethodGet
	u, err := url.Parse(strings.TrimRight(c.cfg.BBURL, "/") + route)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return reject(method+" "+route, "invalid BB URL")
	}
	u.RawQuery = query.Encode()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return reject(method+" "+route, "invalid request body")
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return reject(method+" "+route, "invalid HTTP request")
	}
	if c.cfg.BBHostAuthority != "" {
		// Keep URL, connection address, TLS SNI and certificate verification
		// bound to BBURL. Only the explicitly configured HTTP authority differs.
		req.Host = c.cfg.BBHostAuthority
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &RequestError{Operation: method + " " + route, Cause: err, uncertain: mutation}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Error bodies can contain prompts, environment data or credentials: do
		// not persist them in openfactory events. Status is sufficient for routing.
		return &RequestError{Operation: method + " " + route, Status: resp.StatusCode, Cause: errors.New(http.StatusText(resp.StatusCode)), uncertain: mutation && (resp.StatusCode >= 500 || resp.StatusCode < 400 || resp.StatusCode == 408)}
	}
	if result == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err == nil && len(data) > responseLimit {
		err = errors.New("response exceeds size limit")
	}
	if err == nil {
		err = json.Unmarshal(data, result)
	}
	if err != nil {
		return &RequestError{Operation: method + " " + route, Status: resp.StatusCode, Cause: errors.New("invalid or oversized JSON response"), uncertain: mutation}
	}
	return nil
}

type host struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Lifecycle struct {
		Phase string `json:"phase"`
	} `json:"lifecycle"`
}

func (c *Client) hostID(ctx context.Context) (string, error) {
	var hosts []host
	if err := c.request(ctx, "GET", "/api/v1/hosts", nil, nil, &hosts); err != nil {
		return "", err
	}
	var matches []string
	for _, h := range hosts {
		if c.cfg.BBHostID != "" && h.ID != c.cfg.BBHostID {
			continue
		}
		if h.ID != "" && h.Status == "connected" && h.Lifecycle.Phase == "active" {
			matches = append(matches, h.ID)
		}
	}
	if len(matches) != 1 {
		return "", reject("host selection", "expected exactly one connected active BB host; configure bb_host_id when multiple hosts exist")
	}
	return matches[0], nil
}
func (c *Client) Doctor(ctx context.Context) error {
	var health struct {
		OK bool `json:"ok"`
	}
	if err := c.request(ctx, "GET", "/health", nil, nil, &health); err != nil {
		return err
	}
	if !health.OK {
		return reject("doctor", "BB health is not OK")
	}
	var version struct {
		Current string `json:"currentVersion"`
	}
	if err := c.request(ctx, "GET", "/api/v1/system/version", nil, nil, &version); err != nil {
		return err
	}
	if version.Current != Version {
		return reject("doctor", fmt.Sprintf("expected BB %s; server reports %q", Version, version.Current))
	}
	hostID, err := c.hostID(ctx)
	if err != nil {
		return err
	}
	var catalog struct {
		Providers []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
		} `json:"providers"`
		Models []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"models"`
		LoadError json.RawMessage `json:"modelLoadError"`
	}
	if err = c.request(ctx, "GET", "/api/v1/system/execution-options", url.Values{"hostId": {hostID}, "providerId": {provider}}, nil, &catalog); err != nil {
		return err
	}
	available := false
	for _, p := range catalog.Providers {
		if p.ID == provider && p.Available {
			available = true
		}
	}
	if !available {
		return reject("doctor", "acp-omp is not available on the selected BB host")
	}
	if len(catalog.LoadError) > 0 && string(catalog.LoadError) != "null" {
		return reject("doctor", "acp-omp model catalog could not be loaded; check BB provider authentication")
	}
	for _, wanted := range []string{c.cfg.AstraModel, c.cfg.FastModel} {
		if wanted == "" {
			if wanted == c.cfg.AstraModel {
				return reject("doctor", "astra_model must be explicit")
			}
			continue
		}
		found := false
		for _, m := range catalog.Models {
			if m.Model == wanted {
				found = true
			}
		}
		if !found {
			return reject("doctor", fmt.Sprintf("configured model %q is absent from acp-omp catalog", wanted))
		}
	}
	return nil
}

type project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Sources []struct {
		HostID string `json:"hostId"`
		Path   string `json:"path"`
		Type   string `json:"type"`
	} `json:"sources"`
}

// EnsureProject serializes registrations by name, not source: competing sources
// must observe the existing name collision rather than each creating a project.
// This coordination never covers thread creation or an agent's execution.
func (c *Client) EnsureProject(ctx context.Context, name, path string) (string, error) {
	if strings.TrimSpace(name) == "" || !filepath.IsAbs(path) {
		return "", reject("ensure project", "name and absolute shared project path are required")
	}
	hostID, err := c.hostID(ctx)
	if err != nil {
		return "", err
	}
	registration, err := c.lockProject(ctx, name)
	if err != nil {
		return "", err
	}
	defer func() { <-registration.gate }()
	id, err := c.findProject(ctx, name, hostID, path)
	if err != nil {
		return "", registration.reconcileError(err)
	}
	if id != "" {
		registration.uncertain = nil
		return id, nil
	}
	if registration.uncertain != nil {
		// Absence in a later listing does not establish that a timed-out create
		// cannot still finish. Keep reconciling; never submit another POST.
		return "", registration.uncertain
	}
	var created project
	err = c.request(ctx, "POST", "/api/v1/projects", nil, map[string]any{"name": name, "source": map[string]any{"type": "local_path", "hostId": hostID, "path": path}}, &created)
	if err == nil && created.ID != "" {
		return created.ID, nil
	}
	if err == nil {
		err = uncertain("create project", "response has no project ID")
	}
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || !requestErr.Uncertain() {
		return "", err
	}
	registration.uncertain = err
	id, err = c.findProject(ctx, name, hostID, path)
	if err != nil {
		return "", registration.reconcileError(err)
	}
	if id == "" {
		return "", registration.uncertain
	}
	registration.uncertain = nil
	return id, nil
}

func (c *Client) lockProject(ctx context.Context, name string) (*projectRegistration, error) {
	c.projectMu.Lock()
	if c.projects == nil {
		c.projects = make(map[string]*projectRegistration)
	}
	registration := c.projects[name]
	if registration == nil {
		registration = &projectRegistration{gate: make(chan struct{}, 1)}
		c.projects[name] = registration
	}
	c.projectMu.Unlock()
	select {
	case registration.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-registration.gate
			return nil, err
		}
		return registration, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *projectRegistration) reconcileError(err error) error {
	if r.uncertain == nil {
		return err
	}
	return &RequestError{Operation: "create project reconciliation", Cause: errors.Join(r.uncertain, err), uncertain: true}
}

func (c *Client) findProject(ctx context.Context, name, hostID, path string) (string, error) {
	var projects []project
	if err := c.request(ctx, "GET", "/api/v1/projects", nil, nil, &projects); err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	var match string
	for _, p := range projects {
		if p.Name != name {
			continue
		}
		compatible := false
		for _, s := range p.Sources {
			if s.Type == "local_path" && s.HostID == hostID && filepath.Clean(s.Path) == path {
				compatible = true
			}
		}
		if !compatible || p.ID == "" || match != "" {
			return "", reject("ensure project", "project name is ambiguous or registered to another source")
		}
		match = p.ID
	}
	return match, nil
}

func marker(attemptID string) (string, error) {
	if attemptID == "" || strings.ContainsAny(attemptID, "\r\n[]") {
		return "", reject("attempt marker", "attempt ID must be nonempty and contain no brackets or newlines")
	}
	return "[openfactory-attempt:" + attemptID + "]", nil
}

type thread struct {
	ID                         string `json:"id"`
	ProjectID                  string `json:"projectId"`
	EnvironmentID              string `json:"environmentId"`
	ProviderID                 string `json:"providerId"`
	Status                     string `json:"status"`
	ParentID                   string `json:"parentThreadId"`
	QueuedMessageCount         int    `json:"queuedMessageCount"`
	ActiveBackgroundAgentCount int    `json:"activeBackgroundAgentCount"`
}

func (c *Client) Spawn(ctx context.Context, r model.SpawnRequest) (model.Session, error) {
	m, err := marker(r.AttemptID)
	if err != nil {
		return model.Session{}, err
	}
	if r.ProjectID == "" || r.Model == "" || strings.TrimSpace(r.Prompt) == "" || !filepath.IsAbs(r.Workspace) {
		return model.Session{}, reject("spawn", "project, explicit model, nonempty prompt and absolute workspace are required")
	}
	if c.cfg.MaxPromptBytes > 0 && len(r.Prompt)+len(m)+2 > c.cfg.MaxPromptBytes {
		return model.Session{}, reject("spawn", "prompt exceeds configured byte limit")
	}
	if r.EnvironmentID != "" {
		return model.Session{}, reject("spawn", "existing environments are not accepted: each attempt binds its prepared unmanaged workspace")
	}
	hostID, err := c.hostID(ctx)
	if err != nil {
		return model.Session{}, err
	}
	body := map[string]any{
		"projectId": r.ProjectID, "providerId": provider, "model": r.Model, "title": m,
		"origin": "sdk", "startedOnBehalfOf": nil, "originKind": nil,
		"input":                 []any{map[string]any{"type": "text", "text": m + "\n\n" + r.Prompt, "mentions": []any{}}},
		"executionInputSources": map[string]string{"providerId": "explicit", "model": "explicit"},
		"environment":           map[string]any{"type": "host", "hostId": hostID, "workspace": map[string]any{"type": "unmanaged", "path": r.Workspace}},
	}
	var t thread
	if err = c.request(ctx, "POST", "/api/v1/threads", nil, body, &t); err != nil {
		return model.Session{}, err
	}
	if t.ID == "" || t.ProjectID != r.ProjectID || t.ProviderID != provider {
		return model.Session{}, uncertain("spawn", "created thread response is missing or inconsistent identity")
	}
	return model.Session{ThreadID: t.ID, EnvironmentID: t.EnvironmentID}, nil
}
func (c *Client) Stop(ctx context.Context, id string) error {
	if id == "" {
		return reject("stop", "thread ID is required")
	}
	return c.request(ctx, "POST", "/api/v1/threads/"+url.PathEscape(id)+"/stop", nil, nil, nil)
}

type event struct {
	Seq      int64  `json:"seq"`
	ThreadID string `json:"threadId"`
	Type     string `json:"type"`
	Scope    struct {
		Kind   string `json:"kind"`
		TurnID string `json:"turnId"`
	} `json:"scope"`
	Data struct {
		Parent          string `json:"parentToolCallId"`
		Status          string `json:"status"`
		Message         string `json:"message"`
		Source          string `json:"source"`
		RequestID       string `json:"requestId"`
		ClientRequestID string `json:"clientRequestId"`
		Input           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Parent string `json:"parentToolCallId"`
		} `json:"item"`
	} `json:"data"`
}

func (c *Client) events(ctx context.Context, id string, after int64) ([]event, error) {
	var rows []event
	err := c.request(ctx, "GET", "/api/v1/threads/"+url.PathEscape(id)+"/events", url.Values{"afterSeq": {strconv.FormatInt(after, 10)}, "order": {"asc"}, "limit": {strconv.Itoa(pageSize)}}, nil, &rows)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		if e.ThreadID != id || e.Seq <= after {
			return nil, reject("events", "non-monotonic event cursor or wrong thread identity")
		}
		after = e.Seq
	}
	return rows, nil
}

// Find checks durable dispatch inputs, not mutable titles or output text. BB's
// list response is an array paginated by limit/offset; archived is a separate
// filter, and hidden threads must be explicitly included. Multiple matches are
// returned to the engine so it can block rather than choose a duplicate spawn.
func (c *Client) Find(ctx context.Context, projectID, attemptID string) ([]model.Session, error) {
	m, err := marker(attemptID)
	if err != nil {
		return nil, err
	}
	if projectID == "" {
		return nil, reject("find", "project ID is required")
	}
	var result []model.Session
	seen := map[string]bool{}
	for _, archived := range []string{"false", "true"} {
		for offset := 0; ; offset += pageSize {
			var threads []thread
			if err = c.request(ctx, "GET", "/api/v1/threads", url.Values{"projectId": {projectID}, "archived": {archived}, "includeHidden": {"true"}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}, nil, &threads); err != nil {
				return nil, err
			}
			unseen := 0
			for _, t := range threads {
				if t.ID == "" || t.ProjectID != projectID {
					return nil, reject("find", "thread list returned inconsistent identity")
				}
				if seen[t.ID] {
					continue
				}
				seen[t.ID] = true
				unseen++
				if t.ParentID != "" || t.ProviderID != provider {
					continue
				}
				matched := false
				// Read the queue before events so queue-to-dispatch movement cannot
				// disappear between the two durable sources during reconciliation.
				var queued []struct {
					ThreadID string `json:"threadId"`
					Content  []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				if err = c.request(ctx, "GET", "/api/v1/threads/"+url.PathEscape(t.ID)+"/queued-messages", nil, nil, &queued); err != nil {
					return nil, err
				}
				for _, q := range queued {
					if q.ThreadID != t.ID {
						return nil, reject("find", "queued input has wrong thread identity")
					}
					for _, input := range q.Content {
						if input.Type == "text" && strings.HasPrefix(input.Text, m+"\n\n") {
							matched = true
						}
					}
				}
				for after := int64(0); ; {
					rows, err := c.events(ctx, t.ID, after)
					if err != nil {
						return nil, err
					}
					for _, e := range rows {
						if e.Type == "client/turn/requested" && e.Data.Source == "spawn" {
							for _, input := range e.Data.Input {
								if input.Type == "text" && strings.HasPrefix(input.Text, m+"\n\n") {
									matched = true
								}
							}
						}
						after = e.Seq
					}
					if matched || len(rows) < pageSize {
						break
					}
				}
				if matched {
					result = append(result, model.Session{ThreadID: t.ID, EnvironmentID: t.EnvironmentID})
				}
			}
			if len(threads) < pageSize {
				break
			}
			if unseen == 0 {
				return nil, reject("find", "thread pagination made no progress")
			}
		}
	}
	return result, nil
}

// A request is owned only by its immutable spawn marker and requestId. The
// accepted event, not turn ordering, binds that request to a provider turn.
// Starts can precede acceptance (including across polls).
type unboundTurn struct {
	Started   bool   `json:"started,omitempty"`
	Parent    string `json:"parent,omitempty"`
	RequestID string `json:"request,omitempty"`
	Completed bool   `json:"completed,omitempty"`
}

type cursor struct {
	Version     int                    `json:"v"`
	ThreadID    string                 `json:"thread"`
	Seq         int64                  `json:"seq"`
	Marker      string                 `json:"marker,omitempty"`
	RequestID   string                 `json:"request,omitempty"`
	TurnID      string                 `json:"turn,omitempty"`
	RootStarted bool                   `json:"root_started,omitempty"`
	Unbound     map[string]unboundTurn `json:"unbound,omitempty"`
	State       string                 `json:"state,omitempty"`
	Output      string                 `json:"output,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
}

func decodeCursor(id, raw string) (cursor, error) {
	state := cursor{Version: 2, ThreadID: id, State: "running"}
	if raw == "" {
		return state, nil
	}
	if len(raw) > base64.RawURLEncoding.EncodedLen(7*outputLimit) {
		return state, reject("poll", "event cursor exceeds size limit")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) > 7*outputLimit {
		return state, reject("poll", "invalid event cursor")
	}
	if err = json.Unmarshal(data, &state); err != nil || state.Version != 2 || state.ThreadID != id || state.Seq < 0 {
		return state, reject("poll", "event cursor does not belong to thread")
	}
	if len(state.Output) > outputLimit || len(state.Unbound) > 4096 {
		return state, reject("poll", "event cursor exceeds correlation or output limit")
	}
	return state, nil
}

func openfactoryMarker(text string) string {
	line, _, ok := strings.Cut(text, "\n\n")
	if !ok || !strings.HasPrefix(line, "[openfactory-attempt:") || !strings.HasSuffix(line, "]") {
		return ""
	}
	id := strings.TrimSuffix(strings.TrimPrefix(line, "[openfactory-attempt:"), "]")
	m, err := marker(id)
	if err != nil || m != line {
		return ""
	}
	return m
}

func (s *cursor) terminalFailure() bool {
	return s.State == "failed" || s.State == "interrupted"
}

func (s *cursor) block(reason string) {
	if !s.terminalFailure() && s.State != "blocked" {
		s.State, s.Reason = "blocked", reason
	}
}

func (s *cursor) observe(e event) error {
	if e.Type == "client/turn/requested" && e.Data.Source == "spawn" {
		for _, input := range e.Data.Input {
			m := openfactoryMarker(input.Text)
			if input.Type != "text" || m == "" {
				continue
			}
			if e.Scope.Kind != "thread" || e.Data.RequestID == "" {
				s.block("openfactory dispatch is missing request identity")
				continue
			}
			if s.RequestID != "" {
				// Even the same marker dispatched again is not permission to
				// replace the original request/turn or its terminal failure.
				s.block("ambiguous openfactory dispatch ownership")
				continue
			}
			s.Marker, s.RequestID = m, e.Data.RequestID
		}
	}
	inTurn := e.Scope.Kind == "turn" && e.Scope.TurnID != ""
	if e.Type == "turn/started" && inTurn {
		if s.TurnID == e.Scope.TurnID {
			if e.Data.Parent != "" || s.RootStarted {
				s.block("openfactory request does not own a unique root turn")
			} else {
				s.RootStarted = true
			}
		} else if s.TurnID == "" {
			if s.Unbound == nil {
				s.Unbound = map[string]unboundTurn{}
			}
			t := s.Unbound[e.Scope.TurnID]
			if t.Started {
				s.block("duplicate turn start before openfactory acceptance")
			}
			t.Started, t.Parent = true, e.Data.Parent
			s.Unbound[e.Scope.TurnID] = t
		}
	}
	if e.Type == "turn/input/accepted" {
		if e.Data.ClientRequestID == "" {
			s.block("accepted input is missing client request identity")
		} else if e.Data.ClientRequestID == s.RequestID {
			if !inTurn || s.TurnID != "" {
				s.block("openfactory request has ambiguous accepted turn")
			} else {
				t := s.Unbound[e.Scope.TurnID]
				if t.Parent != "" || t.Completed || (t.RequestID != "" && t.RequestID != s.RequestID) {
					s.block("openfactory request does not exclusively own a live root turn")
				} else {
					s.TurnID, s.RootStarted = e.Scope.TurnID, t.Started
					s.Unbound = nil
				}
			}
		} else if inTurn && e.Scope.TurnID == s.TurnID {
			s.block("another request was accepted into the openfactory turn")
		} else if inTurn && s.TurnID == "" {
			if s.Unbound == nil {
				s.Unbound = map[string]unboundTurn{}
			}
			t := s.Unbound[e.Scope.TurnID]
			if t.RequestID != "" && t.RequestID != e.Data.ClientRequestID {
				s.block("multiple requests share an unowned turn")
			}
			t.RequestID = e.Data.ClientRequestID
			s.Unbound[e.Scope.TurnID] = t
		}
	}
	if len(s.Unbound) > 4096 {
		return reject("poll", "too many turns before openfactory request acceptance")
	}
	root := inTurn && s.TurnID == e.Scope.TurnID && s.RootStarted
	if e.Type == "turn/completed" && inTurn && s.TurnID == "" {
		if s.Unbound == nil {
			s.Unbound = map[string]unboundTurn{}
		}
		t := s.Unbound[e.Scope.TurnID]
		t.Completed = true
		s.Unbound[e.Scope.TurnID] = t
	}
	// A terminal failure is irrevocable. Later operator activity, thread idle,
	// acceptance, or even a contradictory completion cannot erase it.
	if s.terminalFailure() || s.State == "blocked" {
		return nil
	}
	if root && e.Type == "item/completed" && e.Data.Item.Type == "agentMessage" && e.Data.Item.Parent == "" && s.State != "completed" {
		if len(e.Data.Item.Text) > outputLimit {
			return reject("poll", "root output exceeds size limit")
		}
		s.Output = e.Data.Item.Text
	}
	if root && e.Type == "turn/completed" {
		switch e.Data.Status {
		case "completed", "failed", "interrupted":
			s.State, s.Reason = e.Data.Status, e.Data.Error.Message
		default:
			return reject("poll", "unknown root completion status")
		}
	}
	// Thread-scoped interruption/error has no turn identity. Conservatively
	// apply it only while our owned request is unresolved, never to a later
	// operator request after the openfactory's explicit terminal completion.
	threadFailure := e.Scope.Kind == "thread" && s.RequestID != "" && s.State != "completed"
	if e.Type == "system/thread/interrupted" && (root || threadFailure) {
		s.State, s.Reason = "interrupted", "BB thread interrupted"
	}
	if (e.Type == "system/error" || e.Type == "provider/error") && (root || threadFailure) {
		s.State, s.Reason = "failed", e.Data.Message
	}
	if e.Type == "client/turn/rejected" && s.RequestID != "" && e.Data.RequestID == s.RequestID {
		s.State, s.Reason = "failed", e.Data.Message
	}
	return nil
}
func (c *Client) Poll(ctx context.Context, id, raw string) (model.Observation, error) {
	if id == "" {
		return model.Observation{}, reject("poll", "thread ID is required")
	}
	s, err := decodeCursor(id, raw)
	if err != nil {
		return model.Observation{}, err
	}
	var t thread
	if err = c.request(ctx, "GET", "/api/v1/threads/"+url.PathEscape(id), nil, nil, &t); err != nil {
		return model.Observation{}, err
	}
	if t.ID != id || t.ProviderID != provider {
		return model.Observation{}, reject("poll", "thread identity or provider mismatch")
	}
	for {
		rows, err := c.events(ctx, id, s.Seq)
		if err != nil {
			return model.Observation{}, err
		}
		for _, e := range rows {
			s.Seq = e.Seq
			if err = s.observe(e); err != nil {
				return model.Observation{}, err
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	// Pending approvals/questions are durable BB interactions, not model prose.
	var interactions []struct {
		Status string `json:"status"`
		Kind   string `json:"kind"`
	}
	if err = c.request(ctx, "GET", "/api/v1/threads/"+url.PathEscape(id)+"/interactions", nil, nil, &interactions); err != nil {
		return model.Observation{}, err
	}
	if t.Status == "error" && s.State == "running" {
		s.State, s.Reason = "failed", "BB thread reports error"
	}
	state, reason := s.State, s.Reason
	if state == "completed" && (!s.RootStarted || s.RequestID == "" || s.TurnID == "") {
		s.block("completion is missing openfactory turn ownership")
		state, reason = s.State, s.Reason
	}
	if state == "running" && (s.RequestID == "" || s.TurnID == "" || !s.RootStarted) && t.Status == "idle" {
		state, reason = "blocked", "idle thread is missing explicit openfactory request acceptance and root ownership"
	}
	if !s.terminalFailure() && state != "blocked" {
		for _, i := range interactions {
			if i.Status == "pending" {
				state, reason = "blocked", "BB is awaiting operator interaction"
				break
			}
		}
	}
	if state == "completed" && (t.Status != "idle" || t.QueuedMessageCount > 0 || t.ActiveBackgroundAgentCount > 0) {
		state, reason = "running", "awaiting quiescent snapshot after explicit openfactory root completion"
	}
	if len(s.Reason) > 4096 {
		s.Reason = s.Reason[:4096]
	}
	encoded, _ := json.Marshal(s)
	output := ""
	if state == "completed" {
		output = s.Output
	}
	return model.Observation{State: state, Cursor: base64.RawURLEncoding.EncodeToString(encoded), Output: output, Reason: reason}, nil
}
