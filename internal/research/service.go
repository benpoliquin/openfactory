package research

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openfactory/internal/model"
)

type BlobStore interface {
	Put(context.Context, []byte) (model.BlobRef, error)
	Read(context.Context, string, int64) ([]byte, error)
}
type Service struct {
	cfg       model.Config
	authority model.ResearchAuthority
	blobs     BlobStore
}

func New(cfg model.Config, authority model.ResearchAuthority, blobs BlobStore) *Service {
	return &Service{cfg: cfg, authority: authority, blobs: blobs}
}
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func (s *Service) fetcher(a model.ResearchAuthorization) *Fetcher {
	return &Fetcher{MaxBytes: a.MaxCaptureBytes, Timeout: time.Duration(a.TimeoutSeconds) * time.Second, MaxRedirects: a.MaxRedirects}
}

func (s *Service) Execute(ctx context.Context, token string, req model.ResearchOperationRequest) (model.ResearchOperationResult, error) {
	if s.authority == nil || s.blobs == nil || token == "" || len(token) > 512 || req.RequestID == "" || len(req.RequestID) > 128 {
		return model.ResearchOperationResult{}, rejected("invalid_contract")
	}
	a, err := s.authority.AuthorizeResearch(ctx, token, req)
	if err != nil {
		return model.ResearchOperationResult{}, err
	}
	if a.Replay != nil {
		return *a.Replay, nil
	}
	timeout := time.Duration(a.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ioCtx, ioCancel := context.WithTimeout(ctx, timeout)
	defer ioCancel()
	var result model.ResearchOperationResult
	switch req.Kind {
	case "search":
		result, err = s.search(ioCtx, a)
	case "capture":
		var response FetchResult
		response, err = s.fetcher(a).Fetch(ioCtx, a.URL)
		result.ObservedBytes = int64(len(response.Body))
		if err == nil {
			result, err = s.captureResult(ioCtx, a, response)
		}
	case "github":
		result, err = s.github(ioCtx, a)
	default:
		err = rejected("invalid_contract")
	}
	if err != nil {
		result.Source = nil
		result.Search = nil
		result.Leads = nil
		result.ErrorCode = errorCode(err)
	}
	// Reservations must be settled even when a caller disconnects. Completion
	// revalidates current controller authority after IO; it is not a local grant.
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	completed, completeErr := s.authority.CompleteResearchOperation(completeCtx, a, result)
	if completeErr != nil {
		return model.ResearchOperationResult{}, completeErr
	}
	return completed, nil
}
func (s *Service) captureResult(ctx context.Context, a model.ResearchAuthorization, response FetchResult) (model.ResearchOperationResult, error) {
	result := model.ResearchOperationResult{ObservedBytes: int64(len(response.Body))}
	if response.Status < 200 || response.Status >= 300 {
		return result, statusError(response.Status)
	}
	if response.Status == http.StatusPartialContent || response.Header.Get("Content-Range") != "" {
		return result, rejected("source_incomplete")
	}
	extracted, err := Extract(response.Body, response.Header.Get("Content-Type"), response.FinalURL, a.MaxTextBytes)
	if err != nil {
		return result, err
	}
	body, err := s.blobs.Put(ctx, response.Body)
	if err != nil {
		return result, err
	}
	text, err := s.blobs.Put(ctx, []byte(extracted.Text))
	if err != nil {
		return result, err
	}
	result.Source = &model.SourceCapture{Kind: "capture", RequestedURL: a.URL, FinalURL: response.FinalURL, PublisherCanonicalURL: extracted.CanonicalURL, Status: response.Status, MediaType: extracted.MediaType, ContentEncoding: response.Header.Get("Content-Encoding"), ByteLength: int64(len(response.Body)), ContentSHA256: body.SHA, TextSHA256: text.SHA, ExtractorVersion: ExtractorVersion, RedirectChain: response.RedirectChain, Valid: true}
	for _, link := range extracted.Links {
		result.Leads = append(result.Leads, model.SearchLead{URL: link, Origin: "captured_source_href"})
	}
	return result, nil
}

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(r.RequestURI) > 16384 || len(r.Header.Get("Authorization")) > 520 {
		writeError(w, http.StatusBadRequest, "invalid_contract")
		return
	}
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || strings.TrimSpace(strings.TrimPrefix(auth[0], "Bearer ")) == "" {
		writeError(w, http.StatusUnauthorized, "approval_required")
		return
	}
	token := strings.TrimPrefix(auth[0], "Bearer ")
	if strings.ContainsAny(token, " \t\r\n") {
		writeError(w, http.StatusUnauthorized, "approval_required")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/research/sources/") {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_contract")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/research/sources/")
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\") {
			writeError(w, http.StatusNotFound, "source_rejected")
			return
		}
		source, err := s.authority.ResearchSource(r.Context(), token, id)
		if err != nil {
			handleError(w, err)
			return
		}
		offset, limit, err := page(r)
		if err != nil {
			handleError(w, err)
			return
		}
		text, err := s.blobs.Read(r.Context(), source.TextSHA256, MaxTextBytes)
		if err != nil {
			writeError(w, http.StatusConflict, "evidence_invalid")
			return
		}
		// Recheck authority after local IO as well: cancellation/invalidations may
		// race the first lookup. Never expose cached source text after revocation.
		if _, err = s.authority.ResearchSource(r.Context(), token, id); err != nil {
			handleError(w, err)
			return
		}
		lines := strings.Split(string(text), "\n")
		if offset > len(lines) {
			writeError(w, http.StatusBadRequest, "invalid_contract")
			return
		}
		end := offset
		size := 0
		for end < len(lines) && end-offset < limit {
			n := len(lines[end]) + 1
			if size+n > 64<<10 {
				if end == offset {
					writeError(w, http.StatusConflict, "source_too_large")
					return
				}
				break
			}
			size += n
			end++
		}
		next := 0
		if end < len(lines) {
			next = end + 1
		}
		writeJSON(w, http.StatusOK, struct {
			Source    model.SourceCapture `json:"source"`
			StartLine int                 `json:"start_line"`
			Text      string              `json:"text"`
			NextLine  int                 `json:"next_line,omitempty"`
		}{source, offset + 1, strings.Join(lines[offset:end], "\n"), next})
		return
	}
	kinds := map[string]string{"/v1/research/search": "search", "/v1/research/capture": "capture", "/v1/research/github": "github"}
	kind, ok := kinds[r.URL.Path]
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_contract")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_contract")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_contract")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_contract")
		return
	}
	var req model.ResearchOperationRequest
	if strictRequest(data, &req) != nil || req.Kind != "" && req.Kind != kind {
		writeError(w, http.StatusBadRequest, "invalid_contract")
		return
	}
	req.Kind = kind
	result, err := s.Execute(r.Context(), token, req)
	if err != nil {
		handleError(w, err)
		return
	}
	if result.ErrorCode != "" {
		writeError(w, http.StatusUnprocessableEntity, result.ErrorCode)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func page(r *http.Request) (int, int, error) {
	offset, limit := 0, 100
	for key, values := range r.URL.Query() {
		if (key != "start_line" && key != "limit") || len(values) != 1 {
			return 0, 0, rejected("invalid_contract")
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 {
			return 0, 0, rejected("invalid_contract")
		}
		if key == "start_line" {
			offset = n - 1
		} else {
			if n > 500 {
				return 0, 0, rejected("invalid_contract")
			}
			limit = n
		}
	}
	return offset, limit, nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
func handleError(w http.ResponseWriter, err error) {
	code := errorCode(err)
	var p *model.ProgramError
	if errors.As(err, &p) {
		code = p.Code
	}
	status := http.StatusUnprocessableEntity
	switch code {
	case "approval_required":
		status = http.StatusForbidden
	case "stale_revision", "policy_changed", "idempotency_conflict", "operation_uncertain":
		status = http.StatusConflict
	case "budget_exhausted":
		status = http.StatusTooManyRequests
	case "search_unavailable":
		status = http.StatusServiceUnavailable
	}
	writeError(w, status, code)
}

func strictRequest(data []byte, dest any) error {
	// Reject duplicate object members before typed decoding, including nested
	// objects, rather than silently accepting whichever grant fields came last.
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return rejected("invalid_contract")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return rejected("invalid_contract")
	}
	return nil
}
func uniqueValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return rejected("invalid_contract")
	}
	seen := map[string]bool{}
	for d.More() {
		if delimiter == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return rejected("invalid_contract")
			}
			seen[name] = true
		}
		if err := uniqueValue(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
