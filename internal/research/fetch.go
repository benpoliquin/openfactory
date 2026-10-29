// Package research implements the controller-owned public research broker.
package research

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const MaxBodyBytes int64 = 8 << 20
const MaxTextBytes int64 = 1 << 20

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Fetcher struct {
	Resolver     Resolver
	Timeout      time.Duration
	MaxBytes     int64
	MaxRedirects int
}
type FetchResult struct {
	RequestedURL, FinalURL string
	RedirectChain          []string
	Status                 int
	Header                 http.Header
	Body                   []byte
}
type brokerError struct{ Code string }

func (e *brokerError) Error() string { return e.Code }
func rejected(code string) error     { return &brokerError{Code: code} }

// PublicURL deliberately accepts no alternate ports, credentials, fragments or
// percent-encoded authority. Authorization is for the exact normalized URL.
func PublicURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\x00#") {
		return nil, rejected("source_rejected")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.Host == "" || strings.ContainsAny(u.Host, "%\\") {
		return nil, rejected("source_rejected")
	}
	if u.Port() != "" && u.Port() != "443" {
		return nil, rejected("source_rejected")
	}
	query, queryErr := url.ParseQuery(u.RawQuery)
	if queryErr != nil {
		return nil, rejected("source_rejected")
	}
	for name := range query {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") || lower == "key" || lower == "auth" || strings.Contains(lower, "signature") {
			return nil, rejected("source_rejected")
		}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.Contains(host, ":") && !strings.HasPrefix(u.Host, "[") {
		return nil, rejected("source_rejected")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !PublicIP(ip) {
			return nil, rejected("source_rejected")
		}
	} else {
		if !strings.Contains(host, ".") {
			return nil, rejected("source_rejected")
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, rejected("source_rejected")
			}
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					return nil, rejected("source_rejected")
				}
			}
		}
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("168.63.129.16/32"), // Azure virtual platform/metadata services.
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}
var publicIPv6 = netip.MustParsePrefix("2000::/3")

func PublicIP(ip netip.Addr) bool {
	// IPv4-mapped IPv6 is rejected, not unwrapped: it must never provide an
	// alternate spelling capable of bypassing either policy or dial validation.
	if !ip.IsValid() || ip.Is4In6() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !publicIPv6.Contains(ip) {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func (f *Fetcher) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !PublicIP(ip) {
			return nil, rejected("source_rejected")
		}
		return []netip.Addr{ip}, nil
	}
	r := f.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, rejected("source_rejected")
	}
	// Reject the entire answer when ANY record is unsafe. Never select a safe
	// member while ignoring a rebinding/private member of the same response.
	for _, ip := range ips {
		if !PublicIP(ip) {
			return nil, rejected("source_rejected")
		}
	}
	return ips, nil
}
func (f *Fetcher) Fetch(ctx context.Context, raw string) (FetchResult, error) {
	return f.request(ctx, http.MethodGet, raw, nil, "", "")
}
func (f *Fetcher) request(ctx context.Context, method, raw string, body []byte, contentType, mcpID string) (FetchResult, error) {
	max := f.MaxBytes
	if max <= 0 || max > MaxBodyBytes {
		max = MaxBodyBytes
	}
	timeout := f.Timeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	redirects := f.MaxRedirects
	if redirects <= 0 || redirects > 3 {
		redirects = 3
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := FetchResult{RequestedURL: raw}
	current := raw
	for hop := 0; ; hop++ {
		u, err := PublicURL(current)
		if err != nil {
			return result, err
		}
		ips, err := f.resolve(ctx, u.Hostname())
		if err != nil {
			return result, err
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		transport := &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr is NOT resolved again. All connection candidates were validated in
			// one DNS snapshot, while HTTP Host and TLS SNI retain the original name.
			var last error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		}}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
		if err != nil {
			transport.CloseIdleConnections()
			return result, rejected("source_rejected")
		}
		req.Header.Set("User-Agent", "openfactory-research/1")
		req.Header.Set("Accept-Encoding", "identity")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
		resp, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			return result, rejected("search_or_source_unavailable")
		}
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			transport.CloseIdleConnections()
			if method != http.MethodGet || location == "" || hop >= redirects {
				return result, rejected("source_rejected")
			}
			next, err := u.Parse(location)
			if err != nil {
				return result, rejected("source_rejected")
			}
			if _, err = PublicURL(next.String()); err != nil {
				return result, err
			}
			result.RedirectChain = append(result.RedirectChain, u.String())
			current = next.String()
			continue
		}
		var data []byte
		var readErr error
		if mcpID != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			data, readErr = readMCPBody(resp, max, mcpID)
		} else {
			data, readErr = readCompleteBody(resp, max)
		}
		resp.Body.Close()
		transport.CloseIdleConnections()
		result.Body = data
		if readErr != nil {
			return result, readErr
		}
		result.FinalURL = u.String()
		result.Status = resp.StatusCode
		result.Header = resp.Header.Clone()
		result.Body = data
		return result, nil
	}
}
func readCompleteBody(resp *http.Response, max int64) ([]byte, error) {
	if resp.ContentLength > max {
		return nil, rejected("source_too_large")
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, rejected("source_rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if int64(len(data)) > max {
		return data, rejected("source_too_large")
	}
	if err != nil || resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return data, rejected("source_incomplete")
	}
	return data, nil
}
func errorCode(err error) string {
	var e *brokerError
	if errors.As(err, &e) {
		return e.Code
	}
	return "source_rejected"
}
func statusError(status int) error {
	if status == 429 {
		return rejected("search_unavailable")
	}
	return fmt.Errorf("public source status %d: %w", status, rejected("source_rejected"))
}
