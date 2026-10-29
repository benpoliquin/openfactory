// Package dashboard provides a read-only view of durable openfactory state.
package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

// Reader is implemented by the durable store; the dashboard never mutates it.
type Reader interface {
	Status(context.Context, model.StatusQuery, model.Config) (model.StatusSnapshot, error)
	FeatureStatus(context.Context, string, model.Config) (model.FeatureStatus, error)
	EventPage(context.Context, string, model.PageQuery) (model.EventPage, error)
	EvidencePage(context.Context, string, model.PageQuery) (model.EvidencePage, error)
	ProgramPage(context.Context, store.ProgramQuery) (store.ProgramPage, error)
	ProgramStatus(context.Context, string) (store.ProgramStatus, error)
	ProgramArtifactPage(context.Context, string, store.ProgramQuery) (store.ProgramArtifactPage, error)
	ProgramArtifactDetail(context.Context, string, string) (store.ProgramArtifactDetail, error)
	ProgramQuestionPage(context.Context, string, store.ProgramQuery) (store.ProgramQuestionPage, error)
	ProgramExperimentPage(context.Context, string, store.ProgramQuery) (store.ProgramExperimentPage, error)
	ProgramSourcePage(context.Context, string, store.ProgramQuery) (store.ProgramSourcePage, error)
	ProgramFeaturePage(context.Context, string, store.ProgramQuery, model.Config) (store.ProgramFeaturePage, error)
	ProgramEventPage(context.Context, string, store.ProgramQuery) (store.ProgramEventPage, error)
}

type handler struct {
	cfg       model.Config
	reader    Reader
	authority string
}

// New serves local requests. NewForAddress additionally pins the bound authority.
func New(cfg model.Config, reader Reader) http.Handler { return &handler{cfg: cfg, reader: reader} }

// NewForAddress pins the request authority to an already validated listen address.
func NewForAddress(cfg model.Config, reader Reader, addr string) http.Handler {
	return &handler{cfg: cfg, reader: reader, authority: addr}
}

// ValidateListenAddress requires explicit IP binding to prevent accidental exposure.
func ValidateListenAddress(addr string, allowNonLoopback bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("dashboard listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("dashboard listen host must be a literal IP")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return errors.New("dashboard listen port must be 1..65535")
	}
	if !allowNonLoopback && !ip.IsLoopback() {
		return errors.New("non-loopback dashboard binding requires explicit permission")
	}
	return nil
}

func (h *handler) validHost(authority string) bool {
	if strings.ContainsAny(authority, "/\\@ \t\r\n") {
		return false
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
		port = ""
		if strings.Contains(host, ":") {
			return false
		}
	}
	if h.authority != "" {
		boundHost, boundPort, _ := net.SplitHostPort(h.authority)
		if port != boundPort {
			return false
		}
		ip := net.ParseIP(boundHost)
		if ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() {
			return strings.EqualFold(host, boundHost)
		}
	}
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w = &headWriter{ResponseWriter: w}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !h.validHost(r.Host) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "read-only endpoint", http.StatusMethodNotAllowed)
		return
	}
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		http.Error(w, "invalid query", 400)
		return
	}
	h.route(w, r)
}

type headWriter struct{ http.ResponseWriter }

func (w *headWriter) Write(p []byte) (int, error) { return len(p), nil }

func pageQuery(v url.Values, key string) (model.PageQuery, error) {
	q := model.PageQuery{After: v.Get(key), Limit: 100}
	if len(q.After) > 2048 {
		return q, errors.New("invalid cursor")
	}
	if s := v.Get("limit"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 || n > 500 {
			return q, errors.New("invalid limit")
		}
		q.Limit = n
	}
	return q, nil
}
func validateKeys(v url.Values, allowed ...string) error {
	for k, values := range v {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found || len(values) != 1 {
			return errors.New("invalid query")
		}
	}
	return nil
}
func (h *handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", 404)
	case errors.Is(err, store.ErrInvalidQuery):
		http.Error(w, "invalid query", 400)
	default:
		http.Error(w, "status temporarily unavailable", 503)
	}
}
func writeJSON(w http.ResponseWriter, v any) {
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(v); err != nil {
		http.Error(w, "response unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
func (h *handler) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if r.URL.Path == "/assets/dashboard.css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(styles))
		return
	}
	api := strings.HasPrefix(r.URL.Path, "/api/")
	path := r.URL.Path
	if api {
		path = strings.TrimPrefix(path, "/api")
	}
	if path == "/programs" || strings.HasPrefix(path, "/programs/") {
		h.programRoute(w, r, path, api)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/api/status" {
		v := r.URL.Query()
		if validateKeys(v, "project", "state", "after", "limit") != nil {
			http.Error(w, "invalid query", 400)
			return
		}
		p, err := pageQuery(v, "after")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s, err := h.reader.Status(r.Context(), model.StatusQuery{Project: v.Get("project"), State: v.Get("state"), After: p.After, Limit: p.Limit}, h.cfg)
		if err != nil {
			h.fail(w, err)
			return
		}
		if api {
			writeJSON(w, s)
			return
		}
		h.render(w, "overview", view{Status: s, Project: v.Get("project"), State: v.Get("state"), Next: nextURL(r.URL, "after", s.NextCursor)})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "features" || parts[1] == "" || len(parts) > 3 {
		http.NotFound(w, r)
		return
	}
	id := parts[1]
	if !api && len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	if api && len(parts) == 3 {
		v := r.URL.Query()
		if validateKeys(v, "after", "limit") != nil {
			http.Error(w, "invalid query", 400)
			return
		}
		q, err := pageQuery(v, "after")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		switch parts[2] {
		case "events":
			p, e := h.reader.EventPage(r.Context(), id, q)
			if e != nil {
				h.fail(w, e)
				return
			}
			boundEvents(&p)
			writeJSON(w, p)
		case "evidence":
			p, e := h.reader.EvidencePage(r.Context(), id, q)
			if e != nil {
				h.fail(w, e)
				return
			}
			boundEvidence(&p)
			writeJSON(w, p)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if validateKeys(r.URL.Query(), "events_after", "evidence_after", "limit") != nil {
		http.Error(w, "invalid query", 400)
		return
	}
	f, err := h.reader.FeatureStatus(r.Context(), id, h.cfg)
	if err != nil {
		h.fail(w, err)
		return
	}
	for i := range f.CurrentEvidence {
		f.CurrentEvidence[i].Detail = bounded(f.CurrentEvidence[i].Detail)
	}
	if api {
		if len(r.URL.Query()) != 0 {
			http.Error(w, "invalid query", 400)
			return
		}
		writeJSON(w, f)
		return
	}
	eq, err := pageQuery(r.URL.Query(), "events_after")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pq, err := pageQuery(r.URL.Query(), "evidence_after")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	events, err := h.reader.EventPage(r.Context(), id, eq)
	if err != nil {
		h.fail(w, err)
		return
	}
	evidence, err := h.reader.EvidencePage(r.Context(), id, pq)
	if err != nil {
		h.fail(w, err)
		return
	}
	boundEvents(&events)
	boundEvidence(&evidence)
	h.render(w, "detail", view{Feature: f, Events: events, Evidence: evidence, NextEvents: nextURL(r.URL, "events_after", events.NextCursor), NextEvidence: nextURL(r.URL, "evidence_after", evidence.NextCursor)})
}
func bounded(s string) string {
	const n = 16 << 10
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "�") + "\n[truncated]"
	}
	return s
}
func boundEvents(p *model.EventPage) {
	for i := range p.Items {
		p.Items[i].Detail = bounded(p.Items[i].Detail)
	}
}
func boundEvidence(p *model.EvidencePage) {
	for i := range p.Items {
		p.Items[i].Detail = bounded(p.Items[i].Detail)
	}
}
func nextURL(u *url.URL, key, cursor string) string {
	if cursor == "" {
		return ""
	}
	v := u.Query()
	v.Set(key, cursor)
	return u.Path + "?" + v.Encode()
}
func safeReceipt(s string) string {
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return u.String()
}

type view struct {
	Status                                         model.StatusSnapshot
	Feature                                        model.FeatureStatus
	Events                                         model.EventPage
	Evidence                                       model.EvidencePage
	Project, State, Next, NextEvents, NextEvidence string
}

var templates = template.Must(template.New("dashboard").Funcs(template.FuncMap{"receiptURL": safeReceipt, "featureURL": func(id string) string { return "/features/" + url.PathEscape(id) }, "short": func(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}, "bounded": bounded}).Parse(pages))

func (h *handler) render(w http.ResponseWriter, name string, v view) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, v); err != nil {
		http.Error(w, "view temporarily unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
