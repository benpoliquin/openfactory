package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"openfactory/internal/model"
)

func loadDiscoveryTestConfig(t *testing.T, path, discovery string) (model.Config, error) {
	t.Helper()
	base := `astra_model: operator/astra
fast_model: operator/fast
max_concurrent: 3
max_calls_per_day: 27
projects:
  app:
    trusted: true
    repository: https://github.com/example/app.git
    auth:
      mode: none
`
	if err := os.WriteFile(path, []byte(base+discovery), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestDiscoveryDefaultsNormalizeApprovalPolicyWithoutChangingOrdinaryAuthority(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	t.Setenv("GITHUB_TOKEN", "must-not-be-used")
	t.Setenv("PARALLEL_API_KEY", "must-not-be-used")
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	implicit, err := loadDiscoveryTestConfig(t, path, "")
	if err != nil {
		t.Fatal(err)
	}
	p := implicit.Discovery
	if p.Limits != (model.ProgramLimits{MaxIterations: 2, MaxModelCalls: 20, MaxSteps: 32, MaxExperiments: 8, MaxSearches: 20, MaxSources: 100, MaxTotalCaptureBytes: 128 << 20}) ||
		p.MaxCaptureBytes != 8<<20 || p.MaxTextBytes != 1<<20 || p.BundleMaxFiles != 128 || p.BundleMaxFileBytes != 8<<20 || p.BundleMaxTotalBytes != 64<<20 ||
		p.ResearchTimeoutSeconds != 30 || p.MaxRedirects != 3 || p.SourceFreshnessSeconds != 30*24*60*60 ||
		p.ExperimentLimits.WallSeconds != 300 || p.ExperimentLimits.MaxOutputBytes != 1<<20 || p.ExperimentLimits.MaxArtifactBytes != 16<<20 || p.ExperimentLimits.MaxArtifactFiles != 128 {
		t.Fatalf("incorrect discovery bounds: %+v", p)
	}
	if p.BrokerURL != "http://127.0.0.1:38888" || p.BrokerBind != "127.0.0.1:38888" || p.AllowNonLoopback || len(p.ExperimentProfiles) != 0 {
		t.Fatalf("discovery acquired implicit external authority: %+v", p)
	}
	data, err := yaml.Marshal(map[string]any{"discovery": p})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := loadDiscoveryTestConfig(t, path, string(data))
	if err != nil {
		t.Fatal(err)
	}
	if explicit.PolicyHash != implicit.PolicyHash || !reflect.DeepEqual(explicit.Discovery, implicit.Discovery) {
		t.Fatal("omitted and explicit discovery defaults must share one authorization policy")
	}
	if implicit.MaxConcurrent != 3 || implicit.MaxCallsPerDay != 27 || implicit.MaxAttempts != 3 || implicit.AstraModel != "operator/astra" || implicit.FastModel != "operator/fast" ||
		implicit.Projects["app"].Auth.Mode != "none" || implicit.Projects["app"].Auth.TokenFile != "" || implicit.Projects["app"].Auth.SSHKeyFile != "" {
		t.Fatal("discovery changed ordinary scheduler/model/auth policy")
	}
}

func TestDiscoveryEveryCapIsLowerOnlyAndChangesPolicyHash(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	base, err := loadDiscoveryTestConfig(t, path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		field func(*model.DiscoveryPolicy) reflect.Value
	}{
		{"iterations", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.Limits.MaxIterations).Elem() }},
		{"model_calls", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.Limits.MaxModelCalls).Elem() }},
		{"steps", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.Limits.MaxSteps).Elem() }},
		{"experiments", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.Limits.MaxExperiments).Elem() }},
		{"searches", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.Limits.MaxSearches).Elem() }},
		{"sources", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.Limits.MaxSources).Elem() }},
		{"total_capture", func(p *model.DiscoveryPolicy) reflect.Value {
			return reflect.ValueOf(&p.Limits.MaxTotalCaptureBytes).Elem()
		}},
		{"capture", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.MaxCaptureBytes).Elem() }},
		{"text", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.MaxTextBytes).Elem() }},
		{"bundle_files", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.BundleMaxFiles).Elem() }},
		{"bundle_file_bytes", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.BundleMaxFileBytes).Elem() }},
		{"bundle_total", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.BundleMaxTotalBytes).Elem() }},
		{"research_timeout", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.ResearchTimeoutSeconds).Elem() }},
		{"redirects", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.MaxRedirects).Elem() }},
		{"freshness", func(p *model.DiscoveryPolicy) reflect.Value { return reflect.ValueOf(&p.SourceFreshnessSeconds).Elem() }},
		{"experiment_wall", func(p *model.DiscoveryPolicy) reflect.Value {
			return reflect.ValueOf(&p.ExperimentLimits.WallSeconds).Elem()
		}},
		{"experiment_output", func(p *model.DiscoveryPolicy) reflect.Value {
			return reflect.ValueOf(&p.ExperimentLimits.MaxOutputBytes).Elem()
		}},
		{"experiment_artifacts", func(p *model.DiscoveryPolicy) reflect.Value {
			return reflect.ValueOf(&p.ExperimentLimits.MaxArtifactBytes).Elem()
		}},
		{"experiment_files", func(p *model.DiscoveryPolicy) reflect.Value {
			return reflect.ValueOf(&p.ExperimentLimits.MaxArtifactFiles).Elem()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, value := range []int64{-1, 0, 1, tc.field(&base.Discovery).Int() + 1} {
				p := base.Discovery
				tc.field(&p).SetInt(value)
				data, err := yaml.Marshal(map[string]any{"discovery": p})
				if err != nil {
					t.Fatal(err)
				}
				got, err := loadDiscoveryTestConfig(t, path, string(data))
				if value < 0 || value > tc.field(&base.Discovery).Int() {
					if err == nil {
						t.Fatalf("accepted authority expansion %d", value)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if value == 0 && got.PolicyHash != base.PolicyHash {
					t.Fatal("zero omission did not normalize before hashing")
				}
				if value == 1 && (tc.field(&got.Discovery).Int() != 1 || got.PolicyHash == base.PolicyHash) {
					t.Fatal("lower limit was lost or omitted from approval hash")
				}
				if got.MaxConcurrent != base.MaxConcurrent || got.MaxCallsPerDay != base.MaxCallsPerDay || got.AstraModel != base.AstraModel || !reflect.DeepEqual(got.Projects, base.Projects) {
					t.Fatal("program cap altered ordinary authority")
				}
			}
		})
	}
}

func TestDiscoveryBrokerOriginBindingAndEnvironmentBoundary(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "http://openfactory:38888")
	t.Setenv("OTHER_BROKER", "http://unapproved:38888")
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	implicit, err := loadDiscoveryTestConfig(t, path, "")
	if err != nil || implicit.Discovery.BrokerURL != "http://openfactory:38888" {
		t.Fatalf("compose broker override: %v, %q", err, implicit.Discovery.BrokerURL)
	}
	explicit, err := loadDiscoveryTestConfig(t, path, "discovery:\n  broker_url: ${OPENFACTORY_RESEARCH_BROKER_URL}\n")
	if err != nil || explicit.PolicyHash != implicit.PolicyHash {
		t.Fatalf("broker environment was not normalized before hashing: %v", err)
	}
	for _, raw := range []string{"http://user:secret@openfactory:38888", "http://openfactory:38888/", "http://openfactory:38888/path", "http://openfactory:38888?", "http://openfactory:38888?q=secret", "http://openfactory:38888#", "http://openfactory:38888#fragment", "http://openfactory:65536", "http://openfactory:", "file:///tmp/broker", "${OTHER_BROKER}"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := loadDiscoveryTestConfig(t, path, "discovery:\n  broker_url: '"+raw+"'\n"); err == nil {
				t.Fatal("unsafe broker URL accepted")
			}
		})
	}
	for _, address := range []string{"0.0.0.0:38888", "[::]:38888", "192.168.1.2:38888"} {
		if _, err := loadDiscoveryTestConfig(t, path, "discovery:\n  broker_bind: '"+address+"'\n"); err == nil {
			t.Fatalf("non-loopback %s accepted without opt-in", address)
		}
		cfg, err := loadDiscoveryTestConfig(t, path, "discovery:\n  broker_bind: '"+address+"'\n  allow_non_loopback: true\n")
		if err != nil || cfg.PolicyHash == implicit.PolicyHash {
			t.Fatalf("explicit binding authority missing from policy: %v", err)
		}
	}
	for _, address := range []string{"localhost:38888", ":38888", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:not-a-port"} {
		if _, err := loadDiscoveryTestConfig(t, path, "discovery:\n  broker_bind: '"+address+"'\n  allow_non_loopback: true\n"); err == nil {
			t.Fatalf("invalid broker binding %s accepted", address)
		}
	}
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "http://127.0.0.1:38888")
	native, err := loadDiscoveryTestConfig(t, path, "")
	if err != nil || native.PolicyHash == implicit.PolicyHash {
		t.Fatalf("advertised broker origin omitted from policy hash: %v", err)
	}
}

func TestDiscoveryProfilesAreExplicitBoundedAndHashed(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	t.Setenv("OPENFACTORY_PROFILE_EXECUTABLE", "/usr/bin/python3")
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	load := func(p model.DiscoveryPolicy) (model.Config, error) {
		t.Helper()
		data, err := yaml.Marshal(map[string]any{"discovery": p})
		if err != nil {
			t.Fatal(err)
		}
		return loadDiscoveryTestConfig(t, path, string(data))
	}
	profile := model.ExperimentProfile{Argv: []string{"/usr/bin/python3", "fixture.py", "$RUNTIME_ARGUMENT"}, AllowedArgs: []string{"--ordered"}, ImageIdentity: "sha256:operator-image"}
	base, err := load(model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": profile}})
	if err != nil {
		t.Fatal(err)
	}
	if base.Discovery.ExperimentProfiles["fixture"].Argv[2] != "$RUNTIME_ARGUMENT" {
		t.Fatal("operator command was subject to ambient environment expansion")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*model.ExperimentProfile)
	}{
		{"argv", func(p *model.ExperimentProfile) { p.Argv = []string{"/usr/bin/python3", "other.py"} }},
		{"allowed_args", func(p *model.ExperimentProfile) { p.AllowedArgs = []string{"--reordered"} }},
		{"image", func(p *model.ExperimentProfile) { p.ImageIdentity = "sha256:other-image" }},
		{"network", func(p *model.ExperimentProfile) { p.Network = true }},
		{"hardware", func(p *model.ExperimentProfile) { p.Hardware = []string{"gpu"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := profile
			tc.mutate(&p)
			got, err := load(model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": p}})
			if err != nil || got.PolicyHash == base.PolicyHash {
				t.Fatalf("operator profile authority absent from policy hash: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		profile model.ExperimentProfile
	}{
		{"empty", model.ExperimentProfile{}},
		{"relative", model.ExperimentProfile{Argv: []string{"python3"}}},
		{"environment_executable", model.ExperimentProfile{Argv: []string{"${OPENFACTORY_PROFILE_EXECUTABLE}"}}},
		{"unclean", model.ExperimentProfile{Argv: []string{"/usr/bin/../bin/python3"}}},
		{"root", model.ExperimentProfile{Argv: []string{"/"}}},
		{"newline", model.ExperimentProfile{Argv: []string{"/usr/bin/python3", "bad\nargument"}}},
		{"unicode_control", model.ExperimentProfile{Argv: []string{"/usr/bin/python3", "bad\u0085argument"}}},
		{"nul_allowlist", model.ExperimentProfile{Argv: []string{"/usr/bin/python3"}, AllowedArgs: []string{"bad\x00arg"}}},
		{"large_argument", model.ExperimentProfile{Argv: []string{"/usr/bin/python3", strings.Repeat("x", 4097)}}},
		{"large_total", model.ExperimentProfile{Argv: append([]string{"/usr/bin/python3"}, strings.Fields(strings.Repeat(strings.Repeat("x", 4096)+" ", 9))...)}},
		{"large_allowlist", model.ExperimentProfile{Argv: []string{"/usr/bin/python3"}, AllowedArgs: strings.Fields(strings.Repeat("arg ", 129))}},
		{"control_image", model.ExperimentProfile{Argv: []string{"/usr/bin/python3"}, ImageIdentity: "image\tidentity"}},
		{"empty_hardware", model.ExperimentProfile{Argv: []string{"/usr/bin/python3"}, Hardware: []string{""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := load(model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": tc.profile}}); err == nil {
				t.Fatal("invalid operator experiment profile accepted")
			}
		})
	}
	if _, err := load(model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"invalid/profile": profile}}); err == nil {
		t.Fatal("invalid profile name accepted")
	}
	cfg, err := load(model.DiscoveryPolicy{ExperimentLimits: model.ExperimentLimits{ResourceDescription: "container-wide CPU limit"}, ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": profile}})
	if err != nil || cfg.PolicyHash == base.PolicyHash {
		t.Fatalf("resource attestation description omitted from policy hash: %v", err)
	}
}

func TestDiscoveryAuthorityFlagsAndEmptyCollectionsNormalizeHash(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	base, err := loadDiscoveryTestConfig(t, path, "")
	if err != nil {
		t.Fatal(err)
	}
	flag, err := loadDiscoveryTestConfig(t, path, "discovery:\n  allow_non_loopback: true\n")
	if err != nil || flag.PolicyHash == base.PolicyHash {
		t.Fatalf("non-loopback authorization flag omitted from policy hash: %v", err)
	}
	bind, err := loadDiscoveryTestConfig(t, path, "discovery:\n  broker_bind: '127.0.0.1:38889'\n")
	if err != nil || bind.PolicyHash == base.PolicyHash {
		t.Fatalf("broker binding omitted from policy hash: %v", err)
	}
	nilCollections := "discovery:\n  experiment_profiles:\n    fixture:\n      argv: [/usr/bin/python3, fixture.py]\n"
	implicit, err := loadDiscoveryTestConfig(t, path, nilCollections)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := loadDiscoveryTestConfig(t, path, nilCollections+"      allowed_args: []\n      hardware: []\n")
	if err != nil || explicit.PolicyHash != implicit.PolicyHash {
		t.Fatalf("empty profile authority lists did not normalize: %v", err)
	}
	renamed, err := loadDiscoveryTestConfig(t, path, strings.Replace(nilCollections, "fixture:", "renamed:", 1))
	if err != nil || renamed.PolicyHash == implicit.PolicyHash {
		t.Fatalf("profile identity omitted from policy hash: %v", err)
	}
}

func TestDiscoveryCannotBorrowAmbientCredentialsOrMutateValidationInputs(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	t.Setenv("GITHUB_TOKEN", "ambient-secret")
	t.Setenv("GH_TOKEN", "ambient-secret")
	t.Setenv("PARALLEL_API_KEY", "ambient-secret")
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	cfg, err := loadDiscoveryTestConfig(t, path, "")
	if err != nil {
		t.Fatal(err)
	}
	project := cfg.Projects["app"]
	project.Auth.Mode = "token"
	cfg.Projects["app"] = project
	if err := Validate(cfg); err == nil {
		t.Fatal("ambient credentials satisfied missing operator token_file")
	}
	project.Auth.Mode = "none"
	cfg.Projects["app"] = project
	cfg.Discovery = model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{
		"fixture": {Argv: []string{"/usr/bin/python3", "fixture.py"}},
	}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Discovery.ExperimentProfiles["fixture"]
	if cfg.Discovery.BrokerURL != "" || cfg.Discovery.Limits.MaxSources != 0 || p.AllowedArgs != nil || p.Hardware != nil {
		t.Fatal("Validate mutated caller-owned policy")
	}
	for _, extra := range []string{
		"discovery:\n  api_key: ambient-secret\n",
		"discovery:\n  experiment_profiles:\n    fixture:\n      argv: [/usr/bin/python3]\n      credentials: [GITHUB_TOKEN]\n",
	} {
		if _, err := loadDiscoveryTestConfig(t, path, extra); err == nil {
			t.Fatal("unknown discovery authority silently accepted")
		}
	}
}

func TestDiscoveryIntegerCapsRejectNonIntegerYAMLScalars(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	configPath := filepath.Join(t.TempDir(), "openfactory.yaml")
	for _, field := range []string{
		"max_capture_bytes", "max_text_bytes", "bundle_max_files", "bundle_max_file_bytes",
		"bundle_max_total_bytes", "research_timeout_seconds", "max_redirects", "source_freshness_seconds",
		"limits.max_iterations", "limits.max_model_calls", "limits.max_steps", "limits.max_experiments",
		"limits.max_searches", "limits.max_sources", "limits.max_total_capture_bytes",
		"experiment_limits.wall_seconds", "experiment_limits.max_output_bytes",
		"experiment_limits.max_artifact_bytes", "experiment_limits.max_artifact_files",
	} {
		t.Run(field, func(t *testing.T) {
			yamlField := "  " + field + ": "
			if parent, child, nested := strings.Cut(field, "."); nested {
				yamlField = "  " + parent + ":\n    " + child + ": "
			}
			for _, value := range []string{"0.5", "-0.5", "5e-1", "-5e-1", "1e0", "!!float 0", "!!float 1", "'1'", "null"} {
				t.Run(value, func(t *testing.T) {
					_, err := loadDiscoveryTestConfig(t, configPath, "discovery:\n"+yamlField+value+"\n")
					if err == nil || !strings.Contains(err.Error(), "discovery."+field+" must be an integer") {
						t.Fatalf("non-integer cap %s was not rejected before normalization: %v", value, err)
					}
				})
			}
			for _, value := range []string{"0", "1", "!!int 1", "0x1"} {
				if _, err := loadDiscoveryTestConfig(t, configPath, "discovery:\n"+yamlField+value+"\n"); err != nil {
					t.Fatalf("ordinary integer cap %s rejected: %v", value, err)
				}
			}
		})
	}
}

func TestDiscoveryIntegerCapsCheckYAMLAliasesAndMerges(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	configPath := filepath.Join(t.TempDir(), "openfactory.yaml")
	for _, discovery := range []string{
		"discovery:\n  experiment_limits:\n    resource_description: &fraction 0.5\n  limits:\n    max_model_calls: *fraction\n",
		"discovery:\n  limits:\n    <<: {max_model_calls: 0.5}\n",
		"discovery:\n  experiment_limits:\n    <<: [{max_output_bytes: -0.5}]\n",
		"<<: {discovery: {limits: {max_model_calls: 0.5}}}\n",
	} {
		if _, err := loadDiscoveryTestConfig(t, configPath, discovery); err == nil || !strings.Contains(err.Error(), "must be an integer") {
			t.Fatalf("non-integer cap hidden by YAML alias or merge was not rejected before normalization: %s, error=%v", discovery, err)
		}
	}
	// Strict scalar tags are scoped to discovery, not ordinary scheduler fields.
	cfg, err := loadDiscoveryTestConfig(t, configPath, "poll_seconds: 10.5\n")
	if err != nil || cfg.PollSeconds != 10 {
		t.Fatalf("ordinary config compatibility changed: poll_seconds=%d, error=%v", cfg.PollSeconds, err)
	}
}

func TestDiscoveryAllowedArgumentGlobSyntaxAndLiteralEnvironment(t *testing.T) {
	t.Setenv("OPENFACTORY_RESEARCH_BROKER_URL", "")
	t.Setenv("OPENFACTORY_ALLOWED_ARG", "expanded-authority")
	configPath := filepath.Join(t.TempDir(), "openfactory.yaml")
	for _, pattern := range []string{"experiments/[", "experiments/[a-", "experiments/*\\"} {
		profile := model.ExperimentProfile{Argv: []string{"/usr/bin/python3"}, AllowedArgs: []string{pattern}}
		data, err := yaml.Marshal(map[string]any{"discovery": model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": profile}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loadDiscoveryTestConfig(t, configPath, string(data)); err == nil || !strings.Contains(err.Error(), "allowed_args pattern") {
			t.Fatalf("invalid glob %q accepted: %v", pattern, err)
		}
	}
	args := []string{"experiments/*", "experiments/?.py", "experiments/[ab].py", "--literal", "${OPENFACTORY_ALLOWED_ARG}", "literal\\argument"}
	profile := model.ExperimentProfile{Argv: []string{"/usr/bin/python3", "$OPENFACTORY_ALLOWED_ARG"}, AllowedArgs: args}
	data, err := yaml.Marshal(map[string]any{"discovery": model.DiscoveryPolicy{ExperimentProfiles: map[string]model.ExperimentProfile{"fixture": profile}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadDiscoveryTestConfig(t, configPath, string(data))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Discovery.ExperimentProfiles["fixture"]
	if !reflect.DeepEqual(got.AllowedArgs, args) || !reflect.DeepEqual(got.Argv, profile.Argv) {
		t.Fatalf("valid argument patterns or literal environment references changed: %+v", got)
	}
}
