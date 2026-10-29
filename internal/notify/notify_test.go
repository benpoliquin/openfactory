package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestDeliverySignsMinimalPayloadAndRejectsReplay(t *testing.T) {
	secret := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, secret, 0600); err != nil {
		t.Fatal(err)
	}
	var body []byte
	var timestamp, signature string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		timestamp = r.Header.Get("X-OpenFactory-Timestamp")
		signature = r.Header.Get("X-OpenFactory-Signature")
		w.WriteHeader(204)
	}))
	defer server.Close()
	d := &Dispatcher{URL: server.URL, SecretFile: secretPath, Client: server.Client()}
	if err := d.Deliver(context.Background(), model.Event{ID: "event-1", FeatureID: "feature-1", Kind: "blocked", Detail: "private source and credential data", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private source") || strings.Contains(string(body), "detail") {
		t.Fatal("exported private detail")
	}
	if !Verify(secret, body, timestamp, signature, time.Now()) {
		t.Fatal("valid signature rejected")
	}
	if Verify(secret, append(append([]byte{}, body...), 'x'), timestamp, signature, time.Now()) {
		t.Fatal("tampered payload accepted")
	}
	if Verify(secret, body, timestamp, signature, time.Now().Add(10*time.Minute)) {
		t.Fatal("expired replay accepted")
	}
}

func TestDeliveryDoesNotFollowRedirect(t *testing.T) {
	reached := false
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer destination.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{URL: source.URL, SecretFile: path, Client: source.Client()}
	if err := d.Deliver(context.Background(), model.Event{ID: "private-event"}); err == nil {
		t.Fatal("redirect treated as successful delivery")
	}
	if reached {
		t.Fatal("signature or payload forwarded to redirect destination")
	}
}
