package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"openfactory/internal/model"
)

// ExperimentError preserves authority/capability failures across the private
// socket. Transport loss means unknown outcome; callers must query the same ID.
type ExperimentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *ExperimentError) Error() string { return e.Code + ": " + e.Message }

type Client struct{ http *http.Client }

var _ model.ExperimentRunner = (*Client)(nil)

func NewClient(socket string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func (c *Client) Start(ctx context.Context, request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	return c.call(ctx, "POST", "/experiments", request)
}
func (c *Client) Status(ctx context.Context, id string) (model.ExperimentReceipt, error) {
	if !validSHA(id) {
		return model.ExperimentReceipt{}, errors.New("invalid experiment run ID")
	}
	return c.call(ctx, "GET", "/experiments/"+id, nil)
}
func (c *Client) Cancel(ctx context.Context, id string) (model.ExperimentReceipt, error) {
	if !validSHA(id) {
		return model.ExperimentReceipt{}, errors.New("invalid experiment run ID")
	}
	return c.call(ctx, "POST", "/experiments/"+id+"/cancel", struct{}{})
}
func (c *Client) CancelIntent(ctx context.Context, request model.ExperimentRunRequest) (model.ExperimentReceipt, error) {
	if !validSHA(request.RunID) || !validSHA(request.RequestSHA) {
		return model.ExperimentReceipt{}, errors.New("exact immutable experiment intent is required")
	}
	return c.call(ctx, "POST", "/experiments/"+request.RunID+"/cancel", request)
}
func (c *Client) ResolveFailed(ctx context.Context, id, requestSHA, attestation string) (model.ExperimentReceipt, error) {
	if !validSHA(id) {
		return model.ExperimentReceipt{}, errors.New("invalid experiment run ID")
	}
	return c.call(ctx, "POST", "/experiments/"+id+"/resolve-failed", resolveRequest{requestSHA, attestation})
}
func (c *Client) call(ctx context.Context, method, path string, value any) (model.ExperimentReceipt, error) {
	var receipt model.ExperimentReceipt
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			return receipt, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://checker"+path, body)
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return receipt, &ExperimentError{Code: "operation_uncertain", Message: "experiment executor unavailable or response interrupted; query the same run ID", Status: 503}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return receipt, &ExperimentError{Code: "operation_uncertain", Message: "invalid experiment executor response", Status: 503}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure ExperimentError
		if json.Unmarshal(data, &failure) != nil || strings.TrimSpace(failure.Code) == "" {
			failure = ExperimentError{Code: "operation_uncertain", Message: fmt.Sprintf("experiment executor HTTP %d", response.StatusCode)}
		}
		failure.Status = response.StatusCode
		return receipt, &failure
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&receipt); err != nil {
		return receipt, &ExperimentError{Code: "operation_uncertain", Message: "invalid experiment receipt", Status: 503}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return receipt, errors.New("trailing experiment receipt data")
	}
	return receipt, nil
}

type resolveRequest struct {
	RequestSHA  string `json:"request_sha"`
	Attestation string `json:"attestation"`
}
