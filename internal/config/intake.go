package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"openfactory/internal/model"
)

// Intake defaults bound the opt-in service; they never enable it or invent an
// external origin, RP ID, project scope, credential, or human approval.
func normalizeIntake(p model.IntakePolicy) (model.IntakePolicy, error) {
	defaults := []struct {
		value             *int
		fallback, maximum int
		name              string
	}{
		{&p.MaxTurnBytes, 16 << 10, 16 << 10, "max_turn_bytes"},
		{&p.MaxTurns, 256, 256, "max_turns"},
		{&p.MaxTextBytes, 2 << 20, 2 << 20, "max_text_bytes"},
		{&p.MaxCanonicalBytes, 256 << 10, 256 << 10, "max_canonical_bytes"},
		{&p.MaxOutputBytes, 256 << 10, 256 << 10, "max_output_bytes"},
		{&p.MaxDraftCalls, 12, 12, "max_draft_calls"},
		{&p.HistoryDefault, 50, 200, "history_default"},
		{&p.HistoryMax, 200, 200, "history_max"},
		{&p.PairingTTLSeconds, 600, 600, "pairing_ttl_seconds"},
		{&p.ChallengeTTLSeconds, 120, 120, "challenge_ttl_seconds"},
		{&p.SessionIdleSeconds, 12 * 60 * 60, 12 * 60 * 60, "session_idle_seconds"},
		{&p.SessionAbsoluteSeconds, 7 * 24 * 60 * 60, 7 * 24 * 60 * 60, "session_absolute_seconds"},
		{&p.CredentialTTLSeconds, 90 * 24 * 60 * 60, 365 * 24 * 60 * 60, "credential_ttl_seconds"},
		{&p.MaxCredentials, 10, 100, "max_credentials"},
		{&p.AuthRateLimit, 30, 120, "auth_rate_limit"},
	}
	for _, limit := range defaults {
		if *limit.value < 0 || *limit.value > limit.maximum {
			return p, fmt.Errorf("intake.%s must be 1..%d (zero selects the default)", limit.name, limit.maximum)
		}
		if *limit.value == 0 {
			*limit.value = limit.fallback
		}
	}
	if p.PublicPath == "" {
		p.PublicPath = "/openfactory-intake"
	}
	if p.Bind == "" {
		p.Bind = "127.0.0.1:38889"
	}
	if p.RPName == "" {
		p.RPName = "OpenFactory Intake"
	}
	return p, ValidateIntakePolicy(p)
}

// ValidateIntakePolicy validates effective policy, including when run flags
// override its private bind. It does not mutate policy or grant authority.
func ValidateIntakePolicy(p model.IntakePolicy) error {
	if p.PublicPath != "/openfactory-intake" {
		return errors.New("intake.public_path must be /openfactory-intake")
	}
	if !discoveryText(p.RPName, 128, false) {
		return errors.New("intake.rp_name must be nonempty text of at most 128 bytes")
	}
	if p.HistoryDefault < 1 || p.HistoryMax < p.HistoryDefault || p.HistoryMax > 200 ||
		p.MaxTurnBytes < 1 || p.MaxTurnBytes > 16<<10 || p.MaxTurns < 1 || p.MaxTurns > 256 ||
		p.MaxTextBytes < p.MaxTurnBytes || p.MaxTextBytes > 2<<20 ||
		p.MaxCanonicalBytes < 1 || p.MaxCanonicalBytes > 256<<10 ||
		p.MaxOutputBytes < 1 || p.MaxOutputBytes > 256<<10 || p.MaxDraftCalls < 1 || p.MaxDraftCalls > 12 {
		return errors.New("invalid intake text, canonical, output, history or lifetime limit")
	}
	if p.PairingTTLSeconds < 1 || p.PairingTTLSeconds > 600 || p.ChallengeTTLSeconds < 1 || p.ChallengeTTLSeconds > 120 ||
		p.SessionIdleSeconds < 1 || p.SessionIdleSeconds > 43200 || p.SessionAbsoluteSeconds < p.SessionIdleSeconds || p.SessionAbsoluteSeconds > 604800 ||
		p.CredentialTTLSeconds < 1 || p.CredentialTTLSeconds > 31536000 || p.MaxCredentials < 1 || p.MaxCredentials > 100 || p.AuthRateLimit < 1 || p.AuthRateLimit > 120 {
		return errors.New("invalid intake authentication lifetime or rate limit")
	}
	host, port, err := net.SplitHostPort(p.Bind)
	if err != nil || net.ParseIP(host) == nil {
		return errors.New("intake.bind must be a literal IP:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("intake.bind port must be 1..65535")
	}
	if !net.ParseIP(host).IsLoopback() && !p.AllowNonLoopback {
		return errors.New("non-loopback intake.bind requires intake.allow_non_loopback")
	}
	if !p.Enabled && p.ExternalOrigin == "" && p.RPID == "" {
		return nil
	}
	if !discoveryText(p.ExternalOrigin, 2048, false) || strings.ContainsAny(p.ExternalOrigin, "?#$\\") {
		return errors.New("intake.external_origin must be an exact HTTPS origin (HTTP localhost is development-only)")
	}
	u, err := url.Parse(p.ExternalOrigin)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.HasSuffix(u.Host, ":") {
		return errors.New("intake.external_origin must not contain credentials, path, query or fragment")
	}
	host = u.Hostname()
	expectedHost := host
	if u.Port() != "" {
		expectedHost += ":" + u.Port()
	}
	if u.Host != expectedHost {
		return errors.New("intake.external_origin requires an unambiguous DNS hostname and optional port")
	}
	if p.ExternalOrigin != u.Scheme+"://"+expectedHost {
		return errors.New("intake.external_origin must use its exact browser-serialized lowercase scheme")
	}
	if host != strings.ToLower(host) || net.ParseIP(host) != nil || !validRPDomain(host) || (u.Scheme != "https" && !(u.Scheme == "http" && host == "localhost")) {
		return errors.New("intake.external_origin requires HTTPS and a lowercase stable DNS hostname; HTTP localhost is development-only")
	}
	if u.Port() != "" {
		n, err = strconv.Atoi(u.Port())
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != u.Port() {
			return errors.New("intake.external_origin port must be canonical decimal 1..65535")
		}
		if u.Scheme == "https" && n == 443 || u.Scheme == "http" && n == 80 {
			return errors.New("intake.external_origin must omit the scheme's default port")
		}
	}
	if p.RPID != host {
		return errors.New("intake.rp_id must exactly equal the external origin hostname, without a port")
	}
	return nil
}

func validRPDomain(host string) bool {
	if host == "localhost" {
		return true
	}
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

// yaml.v3 accepts fractional YAML numbers into Go ints by truncation. Reject
// those scalars before decoding, including policy supplied through YAML aliases.
func validateIntakeYAML(n *yaml.Node, field string, active map[*yaml.Node]bool) error {
	if active[n] {
		return errors.New("cyclic YAML alias in intake policy")
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		return validateIntakeYAML(n.Alias, field, active)
	}
	switch field {
	case "intake.enabled", "intake.allow_non_loopback":
		if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
			return fmt.Errorf("%s must be a YAML boolean", field)
		}
		return nil
	case "intake.max_turn_bytes", "intake.max_turns", "intake.max_text_bytes", "intake.max_canonical_bytes", "intake.max_output_bytes", "intake.max_draft_calls", "intake.history_default", "intake.history_max", "intake.pairing_ttl_seconds", "intake.challenge_ttl_seconds", "intake.session_idle_seconds", "intake.session_absolute_seconds", "intake.credential_ttl_seconds", "intake.max_credentials", "intake.auth_rate_limit":
		if n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
			return fmt.Errorf("%s must be an integer", field)
		}
		return nil
	case "", "intake":
	default:
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range n.Content {
			if err := validateIntakeYAML(child, field, active); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			child := key.Value
			if field != "" {
				child = field + "." + child
			}
			if key.Tag == "!!merge" {
				child = field
			}
			if err := validateIntakeYAML(value, child, active); err != nil {
				return err
			}
		}
	}
	return nil
}
