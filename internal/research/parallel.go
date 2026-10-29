package research

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"openfactory/internal/model"
)

// Protocol pinned to can1357/oh-my-pi commit
// 81e1d8927a3c16cfaff2e9456baa8c060e1e0e9b:
// packages/coding-agent/src/web/search/providers/parallel.ts and mcp/json-rpc.ts.
// That keyless path calls tools/call directly, without initialize, API keys,
// credential resolution, retries, or any paid fallback.
const parallelMCP = "https://search.parallel.ai/mcp"

func (s *Service) search(ctx context.Context, a model.ResearchAuthorization) (model.ResearchOperationResult, error) {
	if a.Query == "" || len(a.Query) > 8192 {
		return model.ResearchOperationResult{}, rejected("source_rejected")
	}
	request := map[string]any{"jsonrpc": "2.0", "id": a.ID, "method": "tools/call", "params": map[string]any{"name": "web_search", "arguments": map[string]any{"objective": a.Query, "search_queries": []string{a.Query}}}}
	body, _ := json.Marshal(request)
	response, err := s.fetcher(a).request(ctx, "POST", parallelMCP, body, "application/json", a.ID)
	if err != nil {
		return model.ResearchOperationResult{}, rejected("search_unavailable")
	}
	if response.Status < 200 || response.Status >= 300 {
		return model.ResearchOperationResult{}, rejected("search_unavailable")
	}
	payload, err := parseMCP(response.Body, response.Header.Get("Content-Type"), a.ID)
	if err != nil {
		return model.ResearchOperationResult{}, err
	}
	var tool struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(payload, &tool) != nil || tool.IsError {
		return model.ResearchOperationResult{}, rejected("search_unavailable")
	}
	data := tool.StructuredContent
	if len(data) == 0 {
		for _, item := range tool.Content {
			if item.Type == "text" && json.Valid([]byte(item.Text)) {
				data = []byte(item.Text)
				break
			}
		}
	}
	if len(data) == 0 {
		return model.ResearchOperationResult{}, rejected("search_unavailable")
	}
	var result struct {
		SearchID string `json:"search_id"`
		Results  []struct {
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Excerpts []string `json:"excerpts"`
		} `json:"results"`
	}
	if json.Unmarshal(data, &result) != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return model.ResearchOperationResult{}, rejected("search_unavailable")
	}
	receipt := model.SearchReceipt{QueryID: a.Request.TopicID, Query: a.Query, Provider: "parallel_keyless_mcp", Status: "completed", RequestID: a.Request.RequestID}
	seen := map[string]bool{}
	for _, item := range result.Results {
		if len(receipt.Leads) >= 40 {
			break
		}
		u, err := PublicURL(item.URL)
		if err != nil || seen[u.String()] {
			continue
		}
		seen[u.String()] = true
		receipt.Leads = append(receipt.Leads, model.SearchLead{URL: u.String(), Title: boundedString(item.Title, 1024), Snippet: boundedString(strings.Join(item.Excerpts, "\n\n"), 8192), Origin: "parallel_search_lead"})
	}
	return model.ResearchOperationResult{Search: &receipt}, nil
}
func boundedString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && s[max]&0xc0 == 0x80 {
		max--
	}
	return s[:max]
}

// parseMCP selects only the exact request ID, never a notification or another
// caller's result. SSE supports comments, CRLF, and multi-line data fields.
func selectMCPMessage(data []byte, id string) (json.RawMessage, bool, error) {
	var messages []json.RawMessage
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if json.Unmarshal(trimmed, &messages) != nil {
			return nil, false, rejected("search_unavailable")
		}
	} else {
		messages = []json.RawMessage{trimmed}
	}
	for _, message := range messages {
		var m struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(message, &m) != nil || m.JSONRPC != "2.0" {
			return nil, false, rejected("search_unavailable")
		}
		if m.Method != "" {
			if len(m.Result) > 0 || len(m.Error) > 0 {
				return nil, false, rejected("search_unavailable")
			}
			continue
		}
		if len(m.ID) == 0 || (len(m.Result) > 0) == (len(m.Error) > 0) {
			return nil, false, rejected("search_unavailable")
		}
		var got string
		if json.Unmarshal(m.ID, &got) != nil {
			continue
		}
		if got != id {
			continue
		}
		if len(m.Error) > 0 {
			return nil, false, rejected("search_unavailable")
		}
		return m.Result, true, nil
	}
	return nil, false, nil
}
func parseMCP(body []byte, contentType, id string) (json.RawMessage, error) {
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		result, ok, err := selectMCPMessage(body, id)
		if err != nil {
			return nil, err
		}
		if ok {
			return result, nil
		}
		return nil, rejected("search_unavailable")
	}
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	for _, event := range strings.Split(normalized, "\n\n") {
		var data []string
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				value := strings.TrimPrefix(line, "data:")
				value = strings.TrimPrefix(value, " ")
				data = append(data, value)
			}
		}
		if len(data) == 0 {
			continue
		}
		joined := strings.Join(data, "\n")
		if joined == "[DONE]" {
			break
		}
		result, ok, err := selectMCPMessage([]byte(joined), id)
		if err != nil {
			return nil, err
		}
		if ok {
			return result, nil
		}
	}
	return nil, rejected("search_unavailable")
}

// Search is a protocol exchange, not source capture: an SSE server may leave
// its stream open after the matching response. Validate each event once and
// stop only at a complete matching result. Captures always read the full body.
func readMCPBody(response *http.Response, max int64, id string) ([]byte, error) {
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return readCompleteBody(response, max)
	}
	if response.ContentLength > max {
		return nil, rejected("search_unavailable")
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, rejected("search_unavailable")
	}
	reader := bufio.NewReader(io.LimitReader(response.Body, max+1))
	var event bytes.Buffer
	var data []string
	var total int64
	for {
		line, readErr := reader.ReadString('\n')
		total += int64(len(line))
		event.WriteString(line)
		if total > max {
			return nil, rejected("search_unavailable")
		}
		text := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(text, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " "))
		}
		if text == "" || readErr == io.EOF {
			if len(data) > 0 {
				joined := strings.Join(data, "\n")
				if joined == "[DONE]" {
					return nil, rejected("search_unavailable")
				}
				_, matched, parseErr := selectMCPMessage([]byte(joined), id)
				if parseErr != nil {
					return nil, parseErr
				}
				if matched {
					return event.Bytes(), nil
				}
			}
			event.Reset()
			data = nil
		}
		if readErr != nil {
			return nil, rejected("search_unavailable")
		}
	}
}
