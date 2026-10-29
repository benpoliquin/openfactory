package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
	"openfactory/internal/model"
)

// validateDiscoveryYAML checks scalar tags before yaml.v3 can truncate floats
// into integer spend caps. All other config decoding remains with KnownFields.
func validateDiscoveryYAML(n *yaml.Node, field string, active map[*yaml.Node]bool) error {
	if active[n] {
		return errors.New("cyclic YAML alias in discovery policy")
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		return validateDiscoveryYAML(n.Alias, field, active)
	}
	switch field {
	case "discovery.max_capture_bytes", "discovery.max_text_bytes",
		"discovery.bundle_max_files", "discovery.bundle_max_file_bytes", "discovery.bundle_max_total_bytes",
		"discovery.research_timeout_seconds", "discovery.max_redirects", "discovery.source_freshness_seconds",
		"discovery.limits.max_iterations", "discovery.limits.max_model_calls", "discovery.limits.max_steps",
		"discovery.limits.max_experiments", "discovery.limits.max_searches", "discovery.limits.max_sources",
		"discovery.limits.max_total_capture_bytes", "discovery.experiment_limits.wall_seconds",
		"discovery.experiment_limits.max_output_bytes", "discovery.experiment_limits.max_artifact_bytes",
		"discovery.experiment_limits.max_artifact_files":
		if n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
			return fmt.Errorf("%s must be an integer", field)
		}
		return nil
	case "", "discovery", "discovery.limits", "discovery.experiment_limits":
		// Only these mappings contain discovery integer fields.
	default:
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range n.Content {
			if err := validateDiscoveryYAML(child, field, active); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			childField := key.Value
			if field != "" {
				childField = field + "." + childField
			}
			if key.Tag == "!!merge" {
				childField = field
			}
			if err := validateDiscoveryYAML(value, childField, active); err != nil {
				return err
			}
		}
	}
	return nil
}

// normalizeDiscovery canonicalizes operator policy before its authorization hash
// is computed. Validation itself does not mutate the profile map.
func normalizeDiscovery(p model.DiscoveryPolicy) (model.DiscoveryPolicy, error) {
	p, err := discoverySettings(p)
	if err != nil {
		return p, err
	}
	if p.ExperimentProfiles == nil {
		p.ExperimentProfiles = map[string]model.ExperimentProfile{}
	}
	for name, profile := range p.ExperimentProfiles {
		if profile.AllowedArgs == nil {
			profile.AllowedArgs = []string{}
		}
		if profile.Hardware == nil {
			profile.Hardware = []string{}
		}
		p.ExperimentProfiles[name] = profile
	}
	return p, nil
}

func discoverySettings(p model.DiscoveryPolicy) (model.DiscoveryPolicy, error) {
	// Check before the model helper fills omitted values: negative limits must
	// never silently become defaults.
	if err := validateDiscoveryCaps(p); err != nil {
		return p, err
	}
	if p.BrokerURL == "" {
		p.BrokerURL = os.Getenv("OPENFACTORY_RESEARCH_BROKER_URL")
	}
	p.BrokerURL = os.Expand(p.BrokerURL, func(name string) string {
		if name == "OPENFACTORY_RESEARCH_BROKER_URL" {
			return os.Getenv(name)
		}
		return "${" + name + "}"
	})
	if p.BrokerURL == "" {
		p.BrokerURL = "http://127.0.0.1:38888"
	}
	p = model.NormalizeDiscoveryPolicy(p)
	if err := validateDiscovery(p); err != nil {
		return p, err
	}
	return p, nil
}

func validateDiscoveryCaps(p model.DiscoveryPolicy) error {
	caps := model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{})
	for _, limit := range []struct {
		name           string
		value, maximum int64
	}{
		{"limits.max_iterations", int64(p.Limits.MaxIterations), int64(caps.Limits.MaxIterations)},
		{"limits.max_model_calls", int64(p.Limits.MaxModelCalls), int64(caps.Limits.MaxModelCalls)},
		{"limits.max_steps", int64(p.Limits.MaxSteps), int64(caps.Limits.MaxSteps)},
		{"limits.max_experiments", int64(p.Limits.MaxExperiments), int64(caps.Limits.MaxExperiments)},
		{"limits.max_searches", int64(p.Limits.MaxSearches), int64(caps.Limits.MaxSearches)},
		{"limits.max_sources", int64(p.Limits.MaxSources), int64(caps.Limits.MaxSources)},
		{"limits.max_total_capture_bytes", p.Limits.MaxTotalCaptureBytes, caps.Limits.MaxTotalCaptureBytes},
		{"max_capture_bytes", p.MaxCaptureBytes, caps.MaxCaptureBytes},
		{"max_text_bytes", p.MaxTextBytes, caps.MaxTextBytes},
		{"bundle_max_files", int64(p.BundleMaxFiles), int64(caps.BundleMaxFiles)},
		{"bundle_max_file_bytes", p.BundleMaxFileBytes, caps.BundleMaxFileBytes},
		{"bundle_max_total_bytes", p.BundleMaxTotalBytes, caps.BundleMaxTotalBytes},
		{"research_timeout_seconds", int64(p.ResearchTimeoutSeconds), int64(caps.ResearchTimeoutSeconds)},
		{"max_redirects", int64(p.MaxRedirects), int64(caps.MaxRedirects)},
		{"source_freshness_seconds", int64(p.SourceFreshnessSeconds), int64(caps.SourceFreshnessSeconds)},
		{"experiment_limits.wall_seconds", int64(p.ExperimentLimits.WallSeconds), int64(caps.ExperimentLimits.WallSeconds)},
		{"experiment_limits.max_output_bytes", p.ExperimentLimits.MaxOutputBytes, caps.ExperimentLimits.MaxOutputBytes},
		{"experiment_limits.max_artifact_bytes", p.ExperimentLimits.MaxArtifactBytes, caps.ExperimentLimits.MaxArtifactBytes},
		{"experiment_limits.max_artifact_files", int64(p.ExperimentLimits.MaxArtifactFiles), int64(caps.ExperimentLimits.MaxArtifactFiles)},
	} {
		if limit.value < 0 || limit.value > limit.maximum {
			return fmt.Errorf("discovery.%s must be positive and at most %d (zero selects the default)", limit.name, limit.maximum)
		}
	}
	return nil
}

func validateDiscovery(p model.DiscoveryPolicy) error {
	u, err := url.Parse(p.BrokerURL)
	if err != nil || !discoveryText(p.BrokerURL, 2048, false) || strings.ContainsAny(p.BrokerURL, "?$#") ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("discovery.broker_url must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("discovery.broker_url has an empty port")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("discovery.broker_url port must be 1..65535")
		}
	}
	host, port, err := net.SplitHostPort(p.BrokerBind)
	if err != nil {
		return fmt.Errorf("discovery.broker_bind: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("discovery.broker_bind host must be a literal IP")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("discovery.broker_bind port must be 1..65535")
	}
	if !ip.IsLoopback() && !p.AllowNonLoopback {
		return errors.New("non-loopback discovery.broker_bind requires discovery.allow_non_loopback")
	}
	if !discoveryText(p.ExperimentLimits.ResourceDescription, 4096, true) {
		return errors.New("discovery.experiment_limits.resource_description exceeds 4096 bytes or contains control characters")
	}
	if len(p.ExperimentProfiles) > 128 {
		return errors.New("discovery.experiment_profiles exceeds 128 profiles")
	}
	for name, profile := range p.ExperimentProfiles {
		if !identifier.MatchString(name) {
			return fmt.Errorf("invalid discovery experiment profile name %q", name)
		}
		if len(profile.Argv) == 0 || len(profile.Argv) > 128 || !filepath.IsAbs(profile.Argv[0]) || filepath.Clean(profile.Argv[0]) != profile.Argv[0] || profile.Argv[0] == string(filepath.Separator) {
			return fmt.Errorf("discovery experiment profile %s requires an absolute executable and 1..128 argv entries", name)
		}
		if err := validateDiscoveryArguments(profile.Argv); err != nil {
			return fmt.Errorf("discovery experiment profile %s argv: %w", name, err)
		}
		if err := validateDiscoveryArguments(profile.AllowedArgs); err != nil {
			return fmt.Errorf("discovery experiment profile %s allowed_args: %w", name, err)
		}
		for _, arg := range profile.AllowedArgs {
			if strings.ContainsAny(arg, "*?[") {
				if _, err := path.Match(arg, ""); err != nil {
					return fmt.Errorf("discovery experiment profile %s allowed_args pattern %q: %w", name, arg, err)
				}
			}
		}
		if !discoveryText(profile.ImageIdentity, 1024, true) || len(profile.Hardware) > 128 {
			return fmt.Errorf("discovery experiment profile %s has invalid image identity or hardware", name)
		}
		for _, hardware := range profile.Hardware {
			if !discoveryText(hardware, 256, false) {
				return fmt.Errorf("discovery experiment profile %s has invalid hardware", name)
			}
		}
	}
	return nil
}

func validateDiscoveryArguments(args []string) error {
	if len(args) > 128 {
		return errors.New("at most 128 arguments are permitted")
	}
	total := 0
	for _, arg := range args {
		if !discoveryText(arg, 4096, true) {
			return errors.New("arguments must be at most 4096 bytes and contain no control characters")
		}
		total += len(arg)
	}
	if total > 32768 {
		return errors.New("arguments exceed 32768 bytes in total")
	}
	return nil
}

func discoveryText(s string, max int, allowEmpty bool) bool {
	return (allowEmpty || s != "") && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}
