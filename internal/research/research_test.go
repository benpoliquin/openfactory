package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func TestPublicURLRejectsCredentialAndSSRFSpellings(t *testing.T) {
	unsafe := []string{"http://example.com/", "https://user:pass@example.com/", "https://example.com:444/", "https://example.com/#fragment", "https://example.com/?api_key=secret", "https://example.com/?access_token=secret", "https://localhost/", "https://127.0.0.1/", "https://[::1]/", "https://[::ffff:8.8.8.8]/", "https://[::ffff:127.0.0.1]/", "https://169.254.169.254/", "https://100.64.0.1/", "https://192.0.2.1/", "https://198.18.0.1/", "https://240.0.0.1/", "https://[2001:db8::1]/", "https://[2002:0808:0808::1]/", "https://[64:ff9b::808:808]/", "https://example.com./", "https://example.com%2f.private/"}
	for _, raw := range unsafe {
		t.Run(raw, func(t *testing.T) {
			if _, err := PublicURL(raw); err == nil {
				t.Fatalf("accepted unsafe URL %q", raw)
			}
		})
	}
	for _, raw := range []string{"https://github.com/can1357/oh-my-pi", "https://example.com:443/a?q=public", "https://[2606:4700:4700::1111]/"} {
		if _, err := PublicURL(raw); err != nil {
			t.Fatalf("public URL %s: %v", raw, err)
		}
	}
}

type answerResolver struct {
	addresses []netip.Addr
	calls     int
}

func (r *answerResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return r.addresses, nil
}
func TestDNSRejectsWholeMixedAnswerAndEveryFreshResolution(t *testing.T) {
	r := &answerResolver{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}}
	f := Fetcher{Resolver: r}
	if _, err := f.resolve(context.Background(), "public.example"); err == nil {
		t.Fatal("mixed public/private DNS answer accepted")
	}
	r.addresses = []netip.Addr{netip.MustParseAddr("8.8.8.8")}
	if _, err := f.resolve(context.Background(), "public.example"); err != nil {
		t.Fatal(err)
	}
	r.addresses = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if _, err := f.resolve(context.Background(), "public.example"); err == nil {
		t.Fatal("rebound address accepted on next request")
	}
	if r.calls != 3 {
		t.Fatalf("cached DNS authority across requests: %d", r.calls)
	}
}
func TestCompleteBodyRejectsOversizeAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		length, max    int64
		encoding, code string
	}{
		{"complete", "complete", 8, 8, "", ""}, {"unknown_complete", "complete", -1, 8, "", ""}, {"declared_oversize", "0123456789", 10, 8, "", "source_too_large"}, {"stream_oversize", "0123456789", -1, 8, "", "source_too_large"}, {"truncated", "prefix", 20, 30, "", "source_incomplete"}, {"compressed_not_identity", "compressed", 10, 30, "gzip", "source_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{Header: http.Header{}, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.body))}
			response.Header.Set("Content-Encoding", tc.encoding)
			body, err := readCompleteBody(response, tc.max)
			if tc.code == "" {
				if err != nil || string(body) != tc.body {
					t.Fatalf("full body lost: %q %v", body, err)
				}
			} else if err == nil || errorCode(err) != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}
func TestExtractionProducesVerifiableQuotesNotScriptEvidence(t *testing.T) {
	html := []byte(`<html><head><link rel="canonical" href="https://example.com/original"></head><body><h1>Public statement</h1><script>invented secret proof</script><style>hidden proof</style><p>Complete &amp; bounded evidence.</p><a href="/next#part">Next</a><a href="http://127.0.0.1/private">bad</a></body></html>`)
	result, err := Extract(html, "text/html; charset=utf-8", "https://example.com/article", MaxTextBytes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "invented") || strings.Contains(result.Text, "hidden") {
		t.Fatalf("executable content became evidence: %s", result.Text)
	}
	if !VerifyQuote(result.Text, "Complete & bounded evidence.", 2, 2) || VerifyQuote(result.Text, "invented secret proof", 0, 0) || VerifyQuote(result.Text, "Public statement", 2, 2) {
		t.Fatalf("quote occurrence/range verification wrong: %q", result.Text)
	}
	if result.CanonicalURL != "https://example.com/original" || len(result.Links) != 1 || result.Links[0] != "https://example.com/next" {
		t.Fatalf("wrong extracted authority links: %+v", result)
	}
	if _, err := Extract([]byte(strings.Repeat("x", 100)), "text/plain", "https://example.com/", 10); err == nil {
		t.Fatal("silently truncated oversized extraction")
	}
	if _, err := Extract([]byte(`{"a":1} trailing`), "application/json", "https://example.com/", 100); err == nil {
		t.Fatal("accepted incomplete JSON representation")
	}
}
func TestMCPUsesExactRequestIDAndActualStructuredOrTextPayload(t *testing.T) {
	jsonBody := []byte(`{"jsonrpc":"2.0","id":"request","result":{"structuredContent":{"results":[{"url":"https://github.com/example/repo","title":"Repo","excerpts":["lead only"]}]}}}`)
	result, err := parseMCP(jsonBody, "application/json", "request")
	if err != nil || !strings.Contains(string(result), "structuredContent") {
		t.Fatalf("JSON response: %s %v", result, err)
	}
	sse := []byte(": keepalive\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"other\",\"result\":{}}\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"request\",\r\ndata: \"result\":{\"content\":[{\"type\":\"text\",\"text\":\"{\\\"results\\\":[]}\"}]}}\r\n\r\n")
	result, err = parseMCP(sse, "text/event-stream", "request")
	if err != nil || !strings.Contains(string(result), "content") {
		t.Fatalf("SSE response: %s %v", result, err)
	}
	for _, body := range []string{`{"jsonrpc":"2.0","id":"other","result":{}}`, `{"jsonrpc":"2.0","id":"request","error":{"code":429,"message":"rate limited"}}`, `{"jsonrpc":"2.0","id":"request","result":{},"error":{}}`, `{"id":"request","result":{}}`} {
		if _, err := parseMCP([]byte(body), "application/json", "request"); err == nil {
			t.Fatalf("accepted malformed/error/unmatched MCP result: %s", body)
		}
	}
}
func TestGitHubAuthorityComesFromApprovedURL(t *testing.T) {
	repo, ref, path, err := githubLocation("https://github.com/can1357/oh-my-pi/blob/main/docs/tools/web_search.md")
	if err != nil || repo != "can1357/oh-my-pi" || ref != "main" || path != "docs/tools/web_search.md" {
		t.Fatalf("wrong derived GitHub location: %q %q %q %v", repo, ref, path, err)
	}
	for _, raw := range []string{"https://github.com/private/repo/tree/main/path", "https://example.com/a/b/blob/main/c", "https://raw.githubusercontent.com/a/b/main/../secret", "https://github.com/a/b/blob/main/file?private=query"} {
		if _, _, _, err := githubLocation(raw); err == nil {
			t.Fatalf("accepted non-blob or appended query URL: %s", raw)
		}
	}
}

type testBlobs struct{ data map[string][]byte }

func (b *testBlobs) Put(_ context.Context, data []byte) (model.BlobRef, error) {
	hash := sha256.Sum256(data)
	sha := hex.EncodeToString(hash[:])
	if b.data == nil {
		b.data = map[string][]byte{}
	}
	b.data[sha] = append([]byte(nil), data...)
	return model.BlobRef{SHA: sha, Bytes: int64(len(data))}, nil
}
func (b *testBlobs) Read(_ context.Context, sha string, max int64) ([]byte, error) {
	data, ok := b.data[sha]
	if !ok || int64(len(data)) > max {
		return nil, errors.New("missing")
	}
	return data, nil
}

type testAuthority struct {
	authorization                              model.ResearchAuthorization
	result                                     model.ResearchOperationResult
	completeErr                                error
	source                                     model.SourceCapture
	sourceCalls, authorizeCalls, completeCalls int
	revokeSecondRead                           bool
}

func (*testAuthority) IssueResearchCapability(context.Context, model.Attempt, model.Config) (model.ResearchCapability, error) {
	return model.ResearchCapability{}, errors.New("not used")
}
func (a *testAuthority) AuthorizeResearch(_ context.Context, token string, request model.ResearchOperationRequest) (model.ResearchAuthorization, error) {
	a.authorizeCalls++
	if token != "opaque-attempt-capability" {
		return model.ResearchAuthorization{}, &model.ProgramError{Code: "approval_required"}
	}
	value := a.authorization
	value.Request = request
	return value, nil
}
func (a *testAuthority) CompleteResearchOperation(_ context.Context, _ model.ResearchAuthorization, result model.ResearchOperationResult) (model.ResearchOperationResult, error) {
	a.completeCalls++
	a.result = result
	return result, a.completeErr
}
func (a *testAuthority) ResearchSource(_ context.Context, token, id string) (model.SourceCapture, error) {
	a.sourceCalls++
	if token != "opaque-attempt-capability" || id != a.source.ID || a.revokeSecondRead && a.sourceCalls > 1 {
		return model.SourceCapture{}, &model.ProgramError{Code: "approval_required"}
	}
	return a.source, nil
}
func TestBrokerSettlesFailedIOAndDoesNotReturnRevokedResults(t *testing.T) {
	authority := &testAuthority{authorization: model.ResearchAuthorization{ID: "operation", URL: "https://127.0.0.1/", MaxCaptureBytes: 1024}, completeErr: &model.ProgramError{Code: "stale_revision"}}
	service := New(model.Config{}, authority, &testBlobs{})
	result, err := service.Execute(context.Background(), "opaque-attempt-capability", model.ResearchOperationRequest{RequestID: "request", Kind: "capture"})
	if err == nil || result.Source != nil || authority.completeCalls != 1 || authority.result.ErrorCode != "source_rejected" {
		t.Fatalf("failed IO not settled or post-IO revocation ignored: %+v %v %+v", result, err, authority)
	}
}
func TestBrokerTerminalReplayDoesNoIOAndNoCompletion(t *testing.T) {
	replay := model.ResearchOperationResult{Search: &model.SearchReceipt{Status: "completed", Query: "approved"}}
	authority := &testAuthority{authorization: model.ResearchAuthorization{Replay: &replay}}
	result, err := New(model.Config{}, authority, &testBlobs{}).Execute(context.Background(), "opaque-attempt-capability", model.ResearchOperationRequest{RequestID: "same", Kind: "search"})
	if err != nil || result.Search == nil || authority.completeCalls != 0 {
		t.Fatalf("replay changed operation: %+v %v", result, err)
	}
}
func TestPrivateHandlerRejectsMutationRoutesAndDuplicateJSON(t *testing.T) {
	authority := &testAuthority{}
	handler := New(model.Config{}, authority, &testBlobs{}).Handler()
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/programs/approve", `{}`, 404}, {"GET", "/v1/research/search", "", 405}, {"POST", "/v1/research/search", `{"request_id":"r","topic_id":"public","topic_id":"private"}`, 400}, {"POST", "/v1/research/search", `{"request_id":"r","query":"private text"}`, 400},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer opaque-attempt-capability")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s %s = %d: %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	if authority.authorizeCalls != 0 {
		t.Fatal("malformed/unknown route reached budget authority")
	}
	request := httptest.NewRequest("POST", "/v1/research/search", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("private handler accepted missing bearer")
	}
}
func TestSourcePagingAndPostReadRevocation(t *testing.T) {
	blobs := &testBlobs{}
	blob, _ := blobs.Put(context.Background(), []byte("one\ntwo\nthree"))
	authority := &testAuthority{source: model.SourceCapture{ID: "source", TextSHA256: blob.SHA, Valid: true}}
	handler := New(model.Config{}, authority, blobs).Handler()
	request := httptest.NewRequest("GET", "/v1/research/sources/source?start_line=2&limit=1", nil)
	request.Header.Set("Authorization", "Bearer opaque-attempt-capability")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var page struct {
		Text     string `json:"text"`
		NextLine int    `json:"next_line"`
	}
	if json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Text != "two" || page.NextLine != 3 {
		t.Fatalf("wrong bounded page: %s", response.Body.String())
	}
	authority.sourceCalls = 0
	authority.revokeSecondRead = true
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 || strings.Contains(response.Body.String(), "two") {
		t.Fatalf("revoked source text leaked: %d %s", response.Code, response.Body.String())
	}
}

type sseThenOpen struct{ payload []byte }

func (r *sseThenOpen) Read(p []byte) (int, error) {
	if len(r.payload) == 0 {
		return 0, errors.New("reader was consumed after terminal matching SSE response")
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, nil
}
func TestMCPReturnsCompleteMatchingEventWithoutWaitingForStreamClose(t *testing.T) {
	stream := &sseThenOpen{payload: []byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"request\",\"result\":{\"structuredContent\":{\"results\":[]}}}\n\n")}
	response := &http.Response{ContentLength: -1, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(stream)}
	body, err := readMCPBody(response, 8192, "request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseMCP(body, "text/event-stream", "request"); err != nil {
		t.Fatalf("terminal event missing: %s %v", body, err)
	}
}

// BB 0.47.1 commit5e1acd765039e874643f34c4d35409e58484e408:
// provider-bridge-acp/bridge/bridge.ts passes the SAME params.cwd to
// createBridgeAgentConnection and session/new; known-agents.ts acp-omp has
// args ["acp"] and no cwd override. This subprocess fixture exercises the
// actual launcher branch/argument boundary without claiming LLM/tool proof.
func TestDiscoveryLauncherRestrictsToolsWithoutChangingOrdinaryLaunch(t *testing.T) {
	launcher, err := filepath.Abs(filepath.Join("..", "..", "deploy", "omp.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	digest := strings.Repeat("a", 32)
	discovery := filepath.Join(root, "discovery", digest, digest, digest, digest)
	if err := os.MkdirAll(filepath.Join(discovery, ".openfactory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discovery, ".openfactory", "discovery-session.json"), []byte(`{"capability_token":"must-not-appear"}`), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", launcher, "acp")
	command.Dir = discovery
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("discovery launch: %v %s", err, data)
	}
	output := string(data)
	if !strings.Contains(output, "research_search,research_capture,research_github,research_source,discovery_read,discovery_write") || !strings.Contains(output, "--no-extensions") || !strings.Contains(output, "--no-rules") || strings.Contains(output, "must-not-appear") || strings.Contains(output, "read,bash") {
		t.Fatalf("discovery tools/token boundary violated: %s", output)
	}
	command = exec.Command("/bin/sh", launcher, "acp", "--tools", "bash")
	command.Dir = discovery
	if data, err := command.CombinedOutput(); err == nil || !strings.Contains(string(data), "discovery_launch_arguments_rejected") {
		t.Fatalf("model caller can replace launch policy: %v %s", err, data)
	}
	ordinary := filepath.Join(root, "ordinary")
	if err := os.Mkdir(ordinary, 0700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("/bin/sh", launcher, "acp")
	command.Dir = ordinary
	data, err = command.CombinedOutput()
	if err != nil || !strings.Contains(string(data), "read,bash,edit,write,grep,find,glob,eval,todo,task") || strings.Contains(string(data), "research_search") {
		t.Fatalf("ordinary launch changed: %v %s", err, data)
	}
}

func TestCaptureStoresCompleteBodyAndDistinctExtractedText(t *testing.T) {
	blobs := &testBlobs{}
	service := New(model.Config{}, &testAuthority{}, blobs)
	body := []byte("<html><body><p>Actual captured quotation.</p></body></html>")
	response := FetchResult{FinalURL: "https://example.com/article", Status: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: body}
	result, err := service.captureResult(context.Background(), model.ResearchAuthorization{URL: response.FinalURL, MaxTextBytes: 1024}, response)
	if err != nil || result.Source == nil {
		t.Fatalf("capture: %+v %v", result, err)
	}
	source := result.Source
	if source.ByteLength != int64(len(body)) || string(blobs.data[source.ContentSHA256]) != string(body) || source.ContentSHA256 == source.TextSHA256 || !VerifyQuote(string(blobs.data[source.TextSHA256]), "Actual captured quotation.", 1, 1) {
		t.Fatalf("capture did not preserve complete raw and extracted provenance: %+v", source)
	}
	response.Status = http.StatusPartialContent
	result, err = service.captureResult(context.Background(), model.ResearchAuthorization{}, response)
	if err == nil || result.Source != nil {
		t.Fatal("partial response accepted as whole source")
	}
}
