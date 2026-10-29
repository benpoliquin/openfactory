package frontdoor_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"openfactory/internal/commandapi"
	"openfactory/internal/frontdoor"
	"openfactory/internal/model"
)

const socketTimeout = 5 * time.Second

func frontdoorConfig(target string) model.Config {
	cfg := model.Config{BBURL: target}
	cfg.Intake.Enabled = true
	cfg.Intake.PublicPath = "/openfactory-intake"
	return cfg
}

func serveFrontdoor(t *testing.T, cfg model.Config, commands http.Handler) *httptest.Server {
	t.Helper()
	handler, err := frontdoor.New(cfg, commands)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func socketRequest(t *testing.T, server *httptest.Server, method, path string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), socketTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(socketTimeout):
		t.Fatal("timed out waiting for socket peer")
		var zero T
		return zero
	}
}

func TestFrontdoorRejectsTargetQuery(t *testing.T) {
	cfg := frontdoorConfig("http://127.0.0.1:38886/controller-base?upstream=value")
	if _, err := frontdoor.New(cfg, http.NotFoundHandler()); err == nil {
		t.Fatal("frontdoor accepted a BB target query forbidden by configuration policy")
	}
}

func TestFrontdoorRoutesOnlyFullPrivatePrefixToCommands(t *testing.T) {
	type observed struct{ route, uri string }
	seen := make(chan observed, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{"bb", r.URL.RequestURI()}
		w.Header().Set("X-Route", "bb")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "native BB")
	}))
	t.Cleanup(upstream.Close)
	commands := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{"commands", r.URL.RequestURI()}
		w.Header().Set("X-Route", "commands")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "private commands")
	})
	server := serveFrontdoor(t, frontdoorConfig(upstream.URL), commands)
	for _, tc := range []struct {
		path, route, uri, body string
		status                 int
	}{
		{"/openfactory-intake/v1/bootstrap?x=a%2Fb&x=c", "commands", "/v1/bootstrap?x=a%2Fb&x=c", "private commands", http.StatusCreated},
		{"/openfactory-intake/", "commands", "/", "private commands", http.StatusCreated},
		{"/", "bb", "/", "native BB", http.StatusAccepted},
		{"/api/v1/projects?limit=2", "bb", "/api/v1/projects?limit=2", "native BB", http.StatusAccepted},
		{"/openfactory-intake-other/v1/bootstrap", "bb", "/openfactory-intake-other/v1/bootstrap", "native BB", http.StatusAccepted},
		{"/v1", "", "", "", http.StatusNotFound},
		{"/v1/bootstrap", "", "", "", http.StatusNotFound},
		{"/openfactory-intake", "", "", "", http.StatusNotFound},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response, body := socketRequest(t, server, http.MethodGet, tc.path, nil)
			if response.StatusCode != tc.status || response.Header.Get("X-Route") != tc.route {
				t.Fatalf("status=%d route=%q; want %d %q", response.StatusCode, response.Header.Get("X-Route"), tc.status, tc.route)
			}
			if tc.route == "" {
				select {
				case got := <-seen:
					t.Fatalf("private root/bare API reached handler: %+v", got)
				default:
				}
				return
			}
			if got := receive(t, seen); got != (observed{tc.route, tc.uri}) || string(body) != tc.body {
				t.Fatalf("request=%+v body=%q", got, body)
			}
		})
	}
}

func TestFrontdoorRetainsOperatorCredentialsOnlyForCommands(t *testing.T) {
	type observed struct {
		uri, cookie, csrf, origin string
	}
	seen := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("private command reached BB")
	}))
	t.Cleanup(upstream.Close)
	commands := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(commandapi.SessionCookieName)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seen <- observed{r.URL.RequestURI(), cookie.Value, r.Header.Get(commandapi.CSRFHeaderName), r.Header.Get("Origin")}
		w.WriteHeader(http.StatusNoContent)
	})
	server := serveFrontdoor(t, frontdoorConfig(upstream.URL), commands)
	response, _ := socketRequest(t, server, http.MethodPost, "/openfactory-intake/v1/commands?native=value", http.Header{
		"Cookie":                  {commandapi.SessionCookieName + "=operator-session"},
		commandapi.CSRFHeaderName: {"operator-proof"},
		"Origin":                  {"https://operator.example"},
	})
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("private command status=%d", response.StatusCode)
	}
	want := observed{"/v1/commands?native=value", "operator-session", "operator-proof", "https://operator.example"}
	if got := receive(t, seen); got != want {
		t.Fatalf("private request credentials changed: got %+v want %+v", got, want)
	}
}

func TestFrontdoorPreservesDestinationPathQueryAndNativeCredentials(t *testing.T) {
	type observed struct {
		host, uri, origin string
		headers           http.Header
		cookies           []*http.Cookie
	}
	seen := make(chan observed, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{r.Host, r.URL.RequestURI(), r.Header.Get("Origin"), r.Header.Clone(), r.Cookies()}
		w.Header().Add("Set-Cookie", "bb_session=renewed; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "bb_theme=dark; Path=/")
		w.Header().Set("Location", "/native-login")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "native redirect")
	}))
	t.Cleanup(upstream.Close)
	for _, authority := range []string{"", "bb-virtual.example:9443"} {
		t.Run(authority, func(t *testing.T) {
			cfg := frontdoorConfig(upstream.URL + "/controller-base")
			cfg.BBHostAuthority = authority
			server := serveFrontdoor(t, cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("BB request reached commands") }))
			headers := http.Header{
				"Cookie":                  {"bb_session=native; " + commandapi.SessionCookieName + "=private-one; bb_theme=light", commandapi.SessionCookieName + "=private-two; bb_other=kept; " + commandapi.SessionCookieName + "=private-three"},
				commandapi.CSRFHeaderName: {"private-csrf", "duplicate-csrf"},
				"Origin":                  {"https://operator.example"},
				"Forwarded":               {"for=attacker;host=forged.example;proto=https"},
				"X-Forwarded-For":         {"203.0.113.9"},
				"X-Forwarded-Host":        {"forged.example"},
				"X-Forwarded-Proto":       {"https"},
			}
			response, body := socketRequest(t, server, http.MethodGet, "/api/v1/projects/project%2Fid?x=a%2Fb&x=c+z", headers)
			got := receive(t, seen)
			wantHost := authority
			if wantHost == "" {
				target, err := url.Parse(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				wantHost = target.Host
			}
			if got.host != wantHost || got.uri != "/controller-base/api/v1/projects/project%2Fid?x=a%2Fb&x=c+z" || got.origin != "https://operator.example" {
				t.Fatalf("HTTP authority changed destination/path/query/origin: %+v", got)
			}
			for _, name := range []string{commandapi.CSRFHeaderName, "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if values := got.headers.Values(name); len(values) != 0 {
					t.Errorf("upstream received private/forged header %s=%q", name, values)
				}
			}
			cookies := make(map[string]string)
			for _, cookie := range got.cookies {
				if cookie.Name == commandapi.SessionCookieName {
					t.Error("operator cookie leaked upstream")
				}
				cookies[cookie.Name] = cookie.Value
			}
			if !reflect.DeepEqual(cookies, map[string]string{"bb_session": "native", "bb_theme": "light", "bb_other": "kept"}) {
				t.Fatalf("native cookies changed: %v", cookies)
			}
			if response.StatusCode != http.StatusTemporaryRedirect || response.Header.Get("Location") != "/native-login" || string(body) != "native redirect" || !reflect.DeepEqual(response.Header.Values("Set-Cookie"), []string{"bb_session=renewed; Path=/; HttpOnly", "bb_theme=dark; Path=/"}) {
				t.Fatalf("native response changed: status=%d headers=%v body=%q", response.StatusCode, response.Header, body)
			}
		})
	}
}

func TestFrontdoorAttachmentConfinementCannotBeBypassedByRepresentation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Security-Policy", "default-src 'self'; img-src data:")
		w.Header().Add("Content-Security-Policy", "object-src 'none'")
		w.Header().Set("X-Content-Type-Options", "upstream-value")
		w.Header().Set("X-Native-BB", "untouched")
		w.Header().Set("ETag", `"native-etag"`)
		kind := r.URL.Query().Get("kind")
		if kind == "image" {
			w.Header().Set("Content-Type", "image/png")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		if kind == "304" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if kind == "error" {
			w.WriteHeader(http.StatusForbidden)
		}
		_, _ = io.WriteString(w, "<script>parent.document.body.textContent='unsafe'</script>")
	}))
	t.Cleanup(upstream.Close)
	server := serveFrontdoor(t, frontdoorConfig(upstream.URL+"/bb-base"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("attachment reached commands") }))
	for _, path := range []string{
		"/api/v1/projects/project/attachments/content",
		"/api/v1/projects/project/attachments/content/",
		"/api/v1/projects/project%20id/attachments/content",
		"/api/v1/projects/project%2Fid/attachments/content",
		"/api/v1/projects/project%2fid/attachments/content/",
		"/%61pi/v1/projects/project/attachments/%63ontent",
		"/api/v1/projects/project/attachments/./content",
		"/api/v1/projects/project/attachments/unused/../content",
	} {
		for _, kind := range []string{"html", "image", "304", "error"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				response, body := socketRequest(t, server, http.MethodGet, path+"?kind="+kind+"&file=x%2Fy", nil)
				wantStatus := http.StatusOK
				if kind == "304" {
					wantStatus = http.StatusNotModified
				}
				if kind == "error" {
					wantStatus = http.StatusForbidden
				}
				if response.StatusCode != wantStatus {
					t.Fatalf("status=%d want %d", response.StatusCode, wantStatus)
				}
				policies := response.Header.Values("Content-Security-Policy")
				if !reflect.DeepEqual(policies, []string{"default-src 'self'; img-src data:", "object-src 'none'", "sandbox allow-scripts"}) {
					t.Fatalf("attachment must preserve native policies and append separate opaque-origin sandbox: %q", policies)
				}
				if !reflect.DeepEqual(response.Header.Values("X-Content-Type-Options"), []string{"nosniff"}) || response.Header.Get("X-Native-BB") != "untouched" || response.Header.Get("ETag") != `"native-etag"` {
					t.Fatalf("attachment headers=%v", response.Header)
				}
				if kind == "304" {
					if len(body) != 0 {
						t.Fatalf("304 body=%q", body)
					}
				} else if string(body) != "<script>parent.document.body.textContent='unsafe'</script>" {
					t.Fatalf("body rewritten: %q", body)
				}
				if kind == "image" && response.Header.Get("Content-Type") != "image/png" {
					t.Fatalf("image type changed: %v", response.Header)
				}
			})
		}
	}
	for _, path := range []string{"/", "/api/v1/projects/project/attachments", "/api/v1/projects/project/attachments/content-other", "/api/v1/projects/project/attachments/content/child", "/api/v1/projects/one/two/attachments/content"} {
		t.Run("native:"+path, func(t *testing.T) {
			response, _ := socketRequest(t, server, http.MethodGet, path, nil)
			if !reflect.DeepEqual(response.Header.Values("Content-Security-Policy"), []string{"default-src 'self'; img-src data:", "object-src 'none'"}) || response.Header.Get("X-Content-Type-Options") != "upstream-value" {
				t.Fatalf("non-attachment headers changed: %v", response.Header)
			}
		})
	}
}

func TestFrontdoorFlushesSSEBeforeUpstreamCompletesAndCancelsStream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(upstream.Close)
	server := serveFrontdoor(t, frontdoorConfig(upstream.URL), http.NotFoundHandler())
	ctx, cancel := context.WithTimeout(context.Background(), socketTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	receive(t, started)
	chunk := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, chunk); err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "data: first\n\n" || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE changed: headers=%v chunk=%q", response.Header, chunk)
	}
	// The upstream is still blocked: receiving this chunk proves it was not buffered to completion.
	cancel()
	_ = response.Body.Close()
	receive(t, canceled)
}

func TestFrontdoorNativeWebSocketUpgradeAndFrames(t *testing.T) {
	finished := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") != "dGhlIHNhbXBsZSBub25jZQ==" {
			finished <- fmt.Errorf("upgrade headers lost: %v", r.Header)
			http.Error(w, "missing upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(socketTimeout))
		// RFC 6455's published example key/accept avoids any external WebSocket dependency.
		_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		if err == nil {
			err = rw.Flush()
		}
		if err != nil {
			finished <- err
			return
		}
		frame := make([]byte, 10)
		if _, err = io.ReadFull(rw, frame); err != nil {
			finished <- err
			return
		}
		if frame[0] != 0x81 || frame[1] != 0x84 {
			finished <- fmt.Errorf("not a masked text frame: %x", frame)
			return
		}
		for i := range 4 {
			frame[6+i] ^= frame[2+i]
		}
		if string(frame[6:]) != "ping" {
			finished <- fmt.Errorf("frame payload=%q", frame[6:])
			return
		}
		_, err = rw.Write([]byte{0x81, 4, 'p', 'i', 'n', 'g'})
		if err == nil {
			err = rw.Flush()
		}
		if err != nil {
			finished <- err
			return
		}
		// Closing the downstream socket must also terminate the owned upstream stream.
		_, err = rw.ReadByte()
		if err != io.EOF {
			finished <- fmt.Errorf("expected downstream close to reach upstream, got %v", err)
			return
		}
		finished <- nil
	}))
	t.Cleanup(upstream.Close)
	server := serveFrontdoor(t, frontdoorConfig(upstream.URL), http.NotFoundHandler())
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", target.Host, socketTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(socketTimeout)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "GET /api/v1/events/websocket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("native upgrade changed: status=%d headers=%v", response.StatusCode, response.Header)
	}
	if _, err := conn.Write([]byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'i' ^ 2, 'n' ^ 3, 'g' ^ 4}); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 6)
	if _, err := io.ReadFull(reader, frame); err != nil {
		t.Fatal(err)
	}
	if string(frame) != string([]byte{0x81, 4, 'p', 'i', 'n', 'g'}) {
		t.Fatalf("echo frame changed: %x", frame)
	}
	_ = conn.Close()
	if err := receive(t, finished); err != nil {
		t.Fatal(err)
	}
}

func TestFrontdoorNativeStreamSurvivesListenerWriteTimeout(t *testing.T) {
	secondChunk := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-secondChunk:
		case <-r.Context().Done():
			close(canceled)
			return
		case <-release:
			return
		}
		_, _ = io.WriteString(w, "data: second\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(upstream.Close)
	handler, err := frontdoor.New(frontdoorConfig(upstream.URL), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), socketTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "data: first\n\n" {
		t.Fatalf("first event=%q", first)
	}
	// Hold the upstream open past the listener's command-sized write deadline.
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(secondChunk)
	second := make([]byte, len("data: second\n\n"))
	if _, err := io.ReadFull(response.Body, second); err != nil {
		t.Fatalf("native stream inherited listener write timeout: %v", err)
	}
	if string(second) != "data: second\n\n" {
		t.Fatalf("second event=%q", second)
	}
	cancel()
	_ = response.Body.Close()
	receive(t, canceled)
}

func TestFrontdoorAttachmentGatewayErrorsAreConfined(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	target := upstream.URL
	upstream.Close()
	server := serveFrontdoor(t, frontdoorConfig(target+"/bb-base"), http.NotFoundHandler())
	for _, path := range []string{
		"/api/v1/projects/project/attachments/content?file=x",
		"/api/v1/projects/project%2Fid/attachments/content/?file=x",
	} {
		t.Run(path, func(t *testing.T) {
			response, _ := socketRequest(t, server, http.MethodGet, path, nil)
			if response.StatusCode < 500 || response.StatusCode > 599 {
				t.Fatalf("closed upstream returned non-gateway status %d", response.StatusCode)
			}
			if !reflect.DeepEqual(response.Header.Values("Content-Security-Policy"), []string{"sandbox allow-scripts"}) {
				t.Fatalf("gateway error escaped sandbox: %v", response.Header)
			}
			if !reflect.DeepEqual(response.Header.Values("X-Content-Type-Options"), []string{"nosniff"}) {
				t.Fatalf("gateway error lacks nosniff: %v", response.Header)
			}
		})
	}
}
