package commandapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"openfactory/internal/intake"
)

type apiError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
	Current   any
}

func (e apiError) Error() string { return e.Code }

func writeJSON(w http.ResponseWriter, status int, value any) {
	// Encode before committing headers so an encoding failure cannot leak a
	// partially encoded private response or produce a successful status.
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusServiceUnavailable
		body = []byte(`{"error":{"code":"storage_unavailable","message":"Storage is unavailable.","retryable":true}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, err error) {
	e := apiError{Status: http.StatusServiceUnavailable, Code: "storage_unavailable", Message: "Storage is unavailable.", Retryable: true}
	var pointer *apiError
	var value apiError
	if errors.As(err, &pointer) && pointer != nil {
		e = *pointer
	} else if errors.As(err, &value) {
		e = value
	}
	if e.Status < 400 || e.Status > 599 {
		e = apiError{Status: http.StatusServiceUnavailable, Code: "storage_unavailable", Message: "Storage is unavailable.", Retryable: true}
	}
	writeJSON(w, e.Status, struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			Current   any    `json:"current,omitempty"`
		} `json:"error"`
	}{Error: struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
		Current   any    `json:"current,omitempty"`
	}{e.Code, e.Message, e.Retryable, e.Current}})
}

func checkBrowserBoundary(r *http.Request, origin string, mutation, jsonBody bool) error {
	denied := apiError{Status: http.StatusForbidden, Code: "origin_denied", Message: "Request origin is not permitted."}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (mutation && len(origins) != 1) {
		return denied
	}
	if len(origins) == 1 && (origin == "" || origins[0] == "null" || origins[0] != origin) {
		return denied
	}
	for _, field := range []struct {
		name    string
		allowed func(string) bool
	}{
		{"Sec-Fetch-Site", func(v string) bool { return v == "same-origin" }},
		{"Sec-Fetch-Mode", func(v string) bool { return v == "cors" || v == "same-origin" || v == "no-cors" }},
	} {
		values := r.Header.Values(field.name)
		if len(values) > 1 || (len(values) == 1 && !field.allowed(values[0])) {
			return denied
		}
	}
	if jsonBody {
		values := r.Header.Values("Content-Type")
		if len(values) != 1 {
			return invalidJSON()
		}
		mediaType, params, err := mime.ParseMediaType(values[0])
		if err != nil || mediaType != "application/json" || len(params) > 1 {
			return invalidJSON()
		}
		for key, value := range params {
			if key != "charset" || !strings.EqualFold(value, "utf-8") {
				return invalidJSON()
			}
		}
	}
	return nil
}

func invalidJSON() error {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "Request must contain valid JSON matching the expected contract."}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, max int64) error {
	if max <= 0 {
		return apiError{Status: http.StatusRequestEntityTooLarge, Code: "input_limit", Message: "Request exceeds the input limit."}
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return apiError{Status: http.StatusRequestEntityTooLarge, Code: "input_limit", Message: "Request exceeds the input limit."}
		}
		return invalidJSON()
	}
	// Reuse the domain's strict decoder so HTTP and immutable command hashing
	// reject the same duplicate keys, unknown fields, null roots and UTF-8.
	if err := intake.DecodeStrict(body, dst); err != nil {
		return invalidJSON()
	}
	return nil
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

func sha256Hex(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func randomToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}
