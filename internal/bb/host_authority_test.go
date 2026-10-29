package bb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"openfactory/internal/model"
)

func TestExplicitHostAuthorityPreservesHTTPDestinationAndRequest(t *testing.T) {
	for _, authority := range []string{"", "localhost:38886", "remote-vhost.example:9443"} {
		t.Run(authority, func(t *testing.T) {
			seen := make(chan struct{ host, uri string }, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- struct{ host, uri string }{r.Host, r.URL.RequestURI()}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			client := New(model.Config{BBURL: server.URL + "/controller-base", BBHostAuthority: authority})
			var result struct {
				OK bool `json:"ok"`
			}
			query := url.Values{"hostId": {"host /+?"}, "providerId": {"acp-omp"}}
			if err := client.request(context.Background(), http.MethodGet, "/api/v1/system/execution-options", query, nil, &result); err != nil {
				t.Fatal(err)
			}
			got := <-seen
			wantHost := authority
			if wantHost == "" {
				destination, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				wantHost = destination.Host
			}
			if !result.OK || got.host != wantHost || got.uri != "/controller-base/api/v1/system/execution-options?"+query.Encode() {
				t.Fatalf("request changed destination/path/query or authority: %+v result=%+v", got, result)
			}
		})
	}
}

func TestExplicitHostAuthorityDoesNotChangeTLSIdentity(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "separate-http-authority.example:38886" {
			t.Errorf("Host override lost: %s", r.Host)
		}
		seen <- r.TLS.ServerName
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := New(model.Config{BBURL: server.URL, BBHostAuthority: "separate-http-authority.example:38886"})
	// This trusts the test server certificate for the URL's IP identity. The
	// unrelated HTTP authority must not become DNS, TLS SNI, or certificate name.
	client.http.Transport = server.Client().Transport
	if err := client.request(context.Background(), http.MethodGet, "/health", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if name := <-seen; name != "" {
		t.Fatalf("HTTP authority changed IP-target TLS SNI: %q", name)
	}
}

func TestExplicitHostAuthorityKeepsRedirectAndUncertainMutationRules(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Host != "localhost:38886" {
					t.Errorf("configured authority absent: %q", r.Host)
				}
				w.Header().Set("Location", target.URL+"/must-not-follow")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := New(model.Config{BBURL: server.URL, BBHostAuthority: "localhost:38886"})
			err := client.request(context.Background(), http.MethodPost, "/api/v1/threads", nil, map[string]string{"input": "bounded"}, nil)
			var requestErr *RequestError
			wantUncertain := status != http.StatusForbidden
			if !errors.As(err, &requestErr) || requestErr.Status != status || requestErr.Uncertain() != wantUncertain || attempts.Load() != 1 {
				t.Fatalf("mutation routing semantics changed: err=%v attempts=%d", err, attempts.Load())
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect leaked authority/mutation to another destination: %d", redirected.Load())
	}
}
