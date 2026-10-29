package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

type fakeReader struct {
	programReaderFake
	snapshot model.StatusSnapshot
	feature  model.FeatureStatus
	events   model.EventPage
	evidence model.EvidencePage
	err      error
	calls    int
	query    model.StatusQuery
}

func (f *fakeReader) Status(_ context.Context, q model.StatusQuery, _ model.Config) (model.StatusSnapshot, error) {
	f.calls++
	f.query = q
	return f.snapshot, f.err
}
func (f *fakeReader) FeatureStatus(context.Context, string, model.Config) (model.FeatureStatus, error) {
	f.calls++
	return f.feature, f.err
}
func (f *fakeReader) EventPage(context.Context, string, model.PageQuery) (model.EventPage, error) {
	f.calls++
	return f.events, f.err
}
func (f *fakeReader) EvidencePage(context.Context, string, model.PageQuery) (model.EvidencePage, error) {
	f.calls++
	return f.evidence, f.err
}
func request(h http.Handler, method, path, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReadOnlyAndHostProtection(t *testing.T) {
	f := &fakeReader{}
	h := New(model.Config{}, f)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		w := request(h, method, "/api/status", "localhost")
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s status %d", method, w.Code)
		}
	}
	for _, host := range []string{"evil.example", "localhost.evil.example", "localhost@evil.example", "127.0.0.1.evil.example"} {
		if w := request(h, "GET", "/api/status", host); w.Code != 400 {
			t.Fatalf("host %s status %d", host, w.Code)
		}
	}
	if f.calls != 0 {
		t.Fatal("rejected request reached store")
	}
	h = NewForAddress(model.Config{}, f, "127.0.0.1:38887")
	if w := request(h, "GET", "/healthz", "localhost:38888"); w.Code != 400 {
		t.Fatal("wrong authority accepted")
	}
	w := request(h, "HEAD", "/api/status", "localhost:38887")
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD status/body: %d %q", w.Code, w.Body.String())
	}
	for _, key := range []string{"Content-Security-Policy", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy"} {
		if w.Header().Get(key) == "" {
			t.Fatalf("missing %s", key)
		}
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unexpected CORS")
	}
}
func TestQueriesAndErrors(t *testing.T) {
	f := &fakeReader{}
	h := New(model.Config{}, f)
	for _, path := range []string{"/?limit=0", "/?limit=501", "/?limit=bad", "/?project=a&project=b", "/?secret=x", "/api/features/f/events?limit=-1", "/features/f?evidence_after=" + strings.Repeat("a", 2049)} {
		if w := request(h, "GET", path, "localhost"); w.Code != 400 {
			t.Fatalf("%s status %d", path, w.Code)
		}
	}
	w := request(h, "GET", "/api/status?project=p&state=running&limit=7&after=opaque", "localhost")
	if w.Code != 200 || f.query.Project != "p" || f.query.State != "running" || f.query.Limit != 7 || f.query.After != "opaque" {
		t.Fatalf("query not propagated: %+v status %d", f.query, w.Code)
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{store.ErrNotFound, 404}, {store.ErrInvalidQuery, 400}, {errors.New("secret database path"), 503}} {
		f.err = tc.err
		w = request(h, "GET", "/api/features/f", "localhost")
		if w.Code != tc.code || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("error response %d %s", w.Code, w.Body.String())
		}
	}
	f.err = nil
	for _, path := range []string{"/missing", "/api/features/f/mutate", "/features/f/events"} {
		if w := request(h, "GET", path, "localhost"); w.Code != 404 {
			t.Fatalf("unknown route %s: %d", path, w.Code)
		}
	}
}
func TestDetailEscapesBoundsAndRetainsHistory(t *testing.T) {
	f := &fakeReader{feature: model.FeatureStatus{Feature: model.Feature{ID: "f", Title: "<script>alert(1)</script>"}, Attempts: []model.AttemptStatus{{ID: "a", CandidateSHA: "new-sha", CandidateGeneration: 2}}, CurrentEvidence: []model.Evidence{{Kind: "review", CandidateSHA: "new-sha", CandidateGeneration: 2, Passed: true}}, Receipt: "javascript:alert(1)"}, evidence: model.EvidencePage{Items: []model.Evidence{{Kind: "review", CandidateSHA: "old-sha", CandidateGeneration: 1, Detail: "<script>bad()</script>" + strings.Repeat("z", 20000)}}}, events: model.EventPage{Items: []model.Event{{Kind: "event", Detail: "<img src=x onerror=bad()>"}}}}
	h := New(model.Config{}, f)
	w := request(h, "GET", "/features/f", "localhost")
	s := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("detail status %d %s", w.Code, s)
	}
	if strings.Contains(s, "<script>") || strings.Contains(s, "<img src=x") || strings.Contains(s, "href=\"javascript:") {
		t.Fatal("unsafe active content rendered")
	}
	for _, needle := range []string{"new-sha", "old-sha", "[truncated]", "No sessions recorded"} {
		if !strings.Contains(s, needle) {
			t.Fatalf("missing %s", needle)
		}
	}
	w = request(h, "GET", "/api/features/f/evidence", "localhost")
	var p model.EvidencePage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || len(p.Items[0].Detail) > 16500 {
		t.Fatal("evidence not bounded")
	}
}
func TestOverviewDoesNotLeakPrivateDetail(t *testing.T) {
	f := &fakeReader{snapshot: model.StatusSnapshot{Features: []model.FeatureStatus{{Feature: model.Feature{ID: "f", Title: "Lane", Body: "private prompt"}, Wait: &model.WaitReason{Code: "integration_wait", Detail: "private reason path", Resource: "private resource path"}, Integration: model.IntegrationStatus{Resources: []string{"private resource path"}, Token: "secret-token"}}}}}
	w := request(New(model.Config{}, f), "GET", "/", "localhost")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, s := range []string{"private prompt", "private reason path", "private resource path", "secret-token"} {
		if strings.Contains(w.Body.String(), s) {
			t.Fatalf("overview leaked %s", s)
		}
	}
}
func TestListenAddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		addr      string
		allow, ok bool
	}{{"127.0.0.1:38887", false, true}, {"[::1]:38887", false, true}, {"0.0.0.0:38887", false, false}, {"0.0.0.0:38887", true, true}, {"localhost:38887", true, false}, {"127.0.0.1:0", false, false}, {":38887", true, false}} {
		if err := ValidateListenAddress(tc.addr, tc.allow); (err == nil) != tc.ok {
			t.Fatalf("%s allow %v: %v", tc.addr, tc.allow, err)
		}
	}
}
func TestSafeReceipt(t *testing.T) {
	for _, s := range []string{"javascript:alert(1)", "file:///private/key", "https://user:password@example.com/pr/1", "//evil.example/x"} {
		if safeReceipt(s) != "" {
			t.Fatalf("unsafe receipt %s", s)
		}
	}
	if safeReceipt("https://github.com/org/repo/pull/1") == "" {
		t.Fatal("safe receipt rejected")
	}
}

func TestAPIPrefixDoesNotAliasHTMLRoutes(t *testing.T) {
	f := &fakeReader{feature: model.FeatureStatus{Feature: model.Feature{ID: "f"}}}
	h := New(model.Config{}, f)
	for _, path := range []string{"/apifeatures/f", "/apifeatures/f/events", "/api/api/features/f"} {
		w := request(h, "GET", path, "localhost")
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d", path, w.Code)
		}
	}
	if f.calls != 0 {
		t.Fatal("unknown path reached reader")
	}
	for _, path := range []string{"/features/f", "/api/features/f"} {
		if w := request(h, "GET", path, "localhost"); w.Code != http.StatusOK {
			t.Fatalf("valid path %s returned %d", path, w.Code)
		}
	}
}

func TestFilterFormPolicyAllowsSameOriginOnly(t *testing.T) {
	h := New(model.Config{}, &fakeReader{})
	w := request(h, "GET", "/", "localhost")
	directives := strings.Split(w.Header().Get("Content-Security-Policy"), ";")
	found := false
	for _, directive := range directives {
		fields := strings.Fields(directive)
		if len(fields) > 0 && fields[0] == "form-action" {
			found = true
			if len(fields) != 2 || fields[1] != "'self'" {
				t.Fatalf("filter form cannot submit safely: %q", directive)
			}
		}
	}
	if !found {
		t.Fatal("form destinations unrestricted")
	}
}
