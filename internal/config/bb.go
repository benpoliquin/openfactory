package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

var errBBHostAuthority = errors.New("bb_host_authority must be a plain hostname or IP authority with an optional canonical port (no scheme, credentials, path or controls)")

// bb_host_authority changes only the HTTP Host field. It is not a URL,
// credential, proxy actor identity, DNS target, or TLS certificate override.
func validateBBHostAuthority(authority string) error {
	if authority == "" {
		return nil
	}
	invalid := errBBHostAuthority
	if !discoveryText(authority, 320, false) || strings.ContainsAny(authority, "/\\?#@%") {
		return invalid
	}
	u, err := url.Parse("http://" + authority)
	if err != nil || u.Host != authority || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(authority, ":") {
		return invalid
	}
	host := u.Hostname()
	if host == "" {
		return invalid
	}
	expected := host
	if ip := net.ParseIP(host); ip != nil {
		if strings.Contains(host, ":") {
			expected = "[" + host + "]"
		}
	} else {
		if len(host) > 253 {
			return invalid
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return invalid
			}
			for _, c := range label {
				if c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
					return invalid
				}
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return invalid
		}
		expected += ":" + port
	}
	if authority != expected {
		return invalid
	}
	return nil
}
