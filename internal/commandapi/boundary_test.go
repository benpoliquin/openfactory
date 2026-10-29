package commandapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserBoundary(t *testing.T) {
	const origin = "https://openfactory.example"
	for _, tc := range []struct {
		name, origin, contentType, site, mode string
		mutation, jsonBody, denied            bool
	}{
		{name: "CLI ceremony", origin: origin, contentType: "application/json", mutation: true, jsonBody: true},
		{name: "same origin", origin: origin, contentType: "application/json; charset=UTF-8", site: "same-origin", mode: "cors", mutation: true, jsonBody: true},
		{name: "read without origin"},
		{name: "read foreign origin", origin: "https://evil.example", denied: true},
		{name: "missing mutation origin", mutation: true, denied: true},
		{name: "null origin", origin: "null", mutation: true, denied: true},
		{name: "foreign origin", origin: "https://evil.example", mutation: true, denied: true},
		{name: "origin prefix", origin: origin + ".evil.example", mutation: true, denied: true},
		{name: "origin slash", origin: origin + "/", mutation: true, denied: true},
		{name: "origin list", origin: origin + " https://evil.example", mutation: true, denied: true},
		{name: "cross site read", site: "cross-site", denied: true},
		{name: "same site read", site: "same-site", denied: true},
		{name: "navigation", mode: "navigate", denied: true},
		{name: "websocket", mode: "websocket", denied: true},
		{name: "missing content type", origin: origin, mutation: true, jsonBody: true, denied: true},
		{name: "form content type", origin: origin, contentType: "application/x-www-form-urlencoded", mutation: true, jsonBody: true, denied: true},
		{name: "JSON suffix", origin: origin, contentType: "application/problem+json", mutation: true, jsonBody: true, denied: true},
		{name: "wrong charset", origin: origin, contentType: "application/json; charset=latin1", mutation: true, jsonBody: true, denied: true},
		{name: "extra parameter", origin: origin, contentType: "application/json; version=1", mutation: true, jsonBody: true, denied: true},
		{name: "binary upload boundary", origin: origin, contentType: "application/octet-stream", mutation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			for key, value := range map[string]string{"Origin": tc.origin, "Content-Type": tc.contentType, "Sec-Fetch-Site": tc.site, "Sec-Fetch-Mode": tc.mode} {
				if value != "" {
					r.Header.Set(key, value)
				}
			}
			if err := checkBrowserBoundary(r, origin, tc.mutation, tc.jsonBody); (err != nil) != tc.denied {
				t.Fatalf("denied = %v, want %v", err, tc.denied)
			}
		})
	}
	for _, header := range []string{"Origin", "Content-Type", "Sec-Fetch-Site", "Sec-Fetch-Mode"} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Mode", "cors")
		r.Header.Add(header, r.Header.Get(header))
		if err := checkBrowserBoundary(r, origin, true, true); err == nil {
			t.Errorf("accepted repeated %s", header)
		}
	}
}

func TestDecodeJSONStrict(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		max        int64
		code       string
	}{
		{"valid", `{"name":"ok","payload":{"items":[{"x":1}]}}`, 128, ""},
		{"duplicate root", `{"name":"a","name":"b"}`, 128, "invalid_request"},
		{"escaped duplicate", `{"name":"a","\u006eame":"b"}`, 128, "invalid_request"},
		{"duplicate raw payload", `{"payload":{"kind":"a","kind":"b"}}`, 128, "invalid_request"},
		{"duplicate nested array", `{"payload":[{"x":1,"x":2}]}`, 128, "invalid_request"},
		{"unknown field", `{"actor":"admin"}`, 128, "invalid_request"},
		{"two documents", `{"name":"a"} {"name":"b"}`, 128, "invalid_request"},
		{"trailing junk", `{"name":"a"} !`, 128, "invalid_request"},
		{"empty", ``, 128, "invalid_request"},
		{"malformed", `{"name":`, 128, "invalid_request"},
		{"over limit", `{"name":"too long"}`, 8, "input_limit"},
		{"exact limit", `{"name":"a"}`, 12, ""},
		{"disabled bound", `{}`, 0, "input_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst struct {
				Name    string          `json:"name"`
				Payload json.RawMessage `json:"payload"`
			}
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			err := decodeJSON(httptest.NewRecorder(), r, &dst, tc.max)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var api apiError
			if !errors.As(err, &api) || api.Code != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
		})
	}
}

func TestUUIDBoundaries(t *testing.T) {
	for _, value := range []string{"00000000-0000-0000-0000-000000000000", "123e4567-e89b-12d3-a456-426614174000"} {
		if !validUUID(value) {
			t.Errorf("rejected canonical UUID %q", value)
		}
	}
	for _, value := range []string{"", "123E4567-e89b-12d3-a456-426614174000", "123e4567e89b12d3a456426614174000", "urn:uuid:123e4567-e89b-12d3-a456-426614174000", "{123e4567-e89b-12d3-a456-426614174000}", "123e4567-e89b-12d3-a456-42661417400g", "123e4567_e89b-12d3-a456-426614174000", "123e4567-e89b-12d3-a456-426614174000\n"} {
		if validUUID(value) {
			t.Errorf("accepted noncanonical UUID %q", value)
		}
	}
}

func TestErrorSanitization(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, fmt.Errorf("database password=private"))
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), `"code":"storage_unavailable"`) {
		t.Fatalf("unsafe response: %d %s", w.Code, w.Body.String())
	}
	for _, err := range []error{apiError{Status: 409, Code: "stale_revision", Message: "Review is stale.", Current: 4}, fmt.Errorf("wrapped: %w", &apiError{Status: 409, Code: "stale_revision", Message: "Review is stale.", Current: 4})} {
		w := httptest.NewRecorder()
		writeError(w, err)
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"current":4`) {
			t.Fatalf("lost typed error: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatal("missing private JSON headers")
		}
	}
}

func TestTokenAndHash(t *testing.T) {
	if got := sha256Hex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("incorrect digest: %s", got)
	}
	first, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(decoded) != 32 || first == second {
		t.Fatal("invalid random token")
	}
}
