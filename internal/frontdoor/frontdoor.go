// Package frontdoor is the single private browser origin for BB and the
// authenticated controller. Worker-controlled attachment bytes are confined to
// an opaque sandbox origin rather than inheriting operator approval authority.
package frontdoor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"openfactory/internal/commandapi"
	"openfactory/internal/model"
)

type attachmentContextKey struct{}

// New serves full external paths. The command component remains internal /v1;
// there is no second public alias and no identity derived from proxy headers.
func New(cfg model.Config, commands http.Handler) (http.Handler, error) {
	if !cfg.Intake.Enabled || cfg.Intake.PublicPath != "/openfactory-intake" || commands == nil {
		return nil, errors.New("private frontdoor requires enabled intake, its fixed public path, and the authenticated command handler")
	}
	target, err := url.Parse(cfg.BBURL)
	if err != nil || target.Host == "" || target.User != nil || target.Opaque != "" || target.RawQuery != "" || target.Fragment != "" || target.ForceQuery || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, errors.New("private frontdoor requires the configured HTTP(S) BB base URL without credentials, query or fragment")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			// SetURL preserves target DNS/TLS identity and joins only its base path.
			// Rewrite removes incoming Forwarded/X-Forwarded-* trust headers;
			// do not regenerate them or derive a human identity from them.
			request.SetURL(target)
			if cfg.BBHostAuthority != "" {
				request.Out.Host = cfg.BBHostAuthority
			}
			request.Out.Header.Del(commandapi.CSRFHeaderName)
			stripOperatorCookie(request.Out)
		},
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if response.Request.Context().Value(attachmentContextKey{}) == true {
				// Additional enforcing CSP policies intersect with the original:
				// never replace an upstream stricter policy or permit same-origin.
				response.Header.Add("Content-Security-Policy", "sandbox allow-scripts")
				response.Header.Set("X-Content-Type-Options", "nosniff")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, request *http.Request, _ error) {
			if request.Context().Value(attachmentContextKey{}) == true {
				w.Header().Add("Content-Security-Policy", "sandbox allow-scripts")
				w.Header().Set("X-Content-Type-Options", "nosniff")
			}
			// Do not log upstream errors with private paths, cookies or queries.
			http.Error(w, "OpenFactory BB upstream is unavailable", http.StatusServiceUnavailable)
		},
	}
	commandPath := cfg.Intake.PublicPath
	commandHandler := http.StripPrefix(commandPath, commands)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, commandPath+"/") {
			commandHandler.ServeHTTP(w, request)
			return
		}
		if request.URL.Path == commandPath || request.URL.Path == "/v1" || strings.HasPrefix(request.URL.Path, "/v1/") {
			http.NotFound(w, request)
			return
		}
		if attachmentContent(request.URL) {
			// Bind confinement to the incoming route before SetURL joins any
			// configured BB base path. It also covers proxy error responses.
			request = request.WithContext(context.WithValue(request.Context(), attachmentContextKey{}, true))
		}
		// Native BB websocket/SSE streams outlive bounded command requests.
		// Keep the server's header and idle limits, but let the proxy stream
		// until peer close or supervised request-context cancellation.
		controller := http.NewResponseController(w)
		controller.SetReadDeadline(time.Time{})
		controller.SetWriteDeadline(time.Time{})
		proxy.ServeHTTP(w, request)
	}), nil
}

func stripOperatorCookie(request *http.Request) {
	containsOperator := false
	for _, value := range request.Header.Values("Cookie") {
		if strings.Contains(value, commandapi.SessionCookieName) {
			containsOperator = true
			break
		}
	}
	if !containsOperator {
		return
	}
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != commandapi.SessionCookieName {
			request.AddCookie(cookie)
		}
	}
}

// Inspect both forms: a backend router can treat an escaped slash within the
// raw project segment differently from net/http's decoded Path. Queries never
// affect this boundary, nor may escaped IDs/fixed segments bypass confinement.
func attachmentContent(u *url.URL) bool {
	return attachmentPath(path.Clean(u.Path), false) || u.RawPath != "" && attachmentPath(strings.TrimRight(u.EscapedPath(), "/"), true)
}

func attachmentPath(value string, escaped bool) bool {
	if !strings.HasPrefix(value, "/") {
		return false
	}
	value = value[1:]
	expected := [...]string{"", "api", "v1", "projects", "", "attachments", "content"}
	for index := 1; index < len(expected); index++ {
		part, rest, more := strings.Cut(value, "/")
		if index < len(expected)-1 && !more || index == len(expected)-1 && more {
			return false
		}
		value = rest
		if index == 4 {
			if part == "" {
				return false
			}
			continue
		}
		if escaped {
			var err error
			part, err = url.PathUnescape(part)
			if err != nil {
				return false
			}
		}
		if part != expected[index] {
			return false
		}
	}
	return true
}
