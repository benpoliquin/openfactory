package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestLoadPreservesCommandVariablesAndRejectsUnknownPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openfactory.yaml")
	t.Setenv("TEST_OPENFACTORY_MODEL", "openai-codex/test-astra")
	data := `state_dir: state
workspace_dir: workspaces
astra_model: ${TEST_OPENFACTORY_MODEL}
projects:
  app:
    trusted: true
    repository: https://github.com/example/app.git
    checks:
      smoke:
        argv: [sh, -c, 'test "$OPENFACTORY_CANDIDATE_SHA" != ""']
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AstraModel != "openai-codex/test-astra" {
		t.Fatal("model variable not resolved")
	}
	if !strings.Contains(c.Projects["app"].Checks["smoke"].Argv[2], "$OPENFACTORY_CANDIDATE_SHA") {
		t.Fatal("runtime variable expanded during config load")
	}
	original := c.PolicyHash
	if err := os.WriteFile(path, []byte(strings.Replace(data, "trusted: true", "trusted: false", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(path); err == nil {
		t.Fatal("unenrolled repository accepted")
	}
	if err := os.WriteFile(path, []byte(data+"unlimited_spend: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(path); err == nil {
		t.Fatal("unknown policy silently ignored")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(data, "example/app.git", "example/other.git", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if next.PolicyHash == original {
		t.Fatal("changed repository retained old authorization hash")
	}
}

func TestLoadRejectsCredentialURLAndOverlappingState(t *testing.T) {
	for _, data := range []string{
		"state_dir: /data\nworkspace_dir: /data/workspaces\n",
		"state_dir: /data/private\nworkspace_dir: /data\n",
		"projects:\n  app:\n    trusted: true\n    repository: https://secret@github.com/example/app.git\n",
		"projects:\n  app:\n    trusted: true\n    repository: https://github.com/example/app.git\n    auth:\n      mode: token\n",
	} {
		t.Run(data, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestReleaseRejectsCandidateOwnedCommandsAndOversizedEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	if err := os.WriteFile(path, []byte("state_dir: /state\nworkspace_dir: /workspaces\nrelease_dir: /config/releases\nprojects:\n  app:\n    trusted: true\n    repository: https://github.com/example/app.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "empty", "relative", "candidate", "outside", "timeout", "missing-rollback"} {
		t.Run(scenario, func(t *testing.T) {
			command := model.Command{Argv: []string{"/config/releases/operator-command"}, TimeoutSeconds: 30}
			release := model.Release{Allowed: true, Qualified: true, ExpiresAt: time.Now().Add(time.Hour), AllowedRisks: []string{"normal"}, AllowedPaths: []string{"src/**"}, Deploy: command, Observe: command, Rollback: command, ObservationSeconds: 1, MinimumObservations: 1}
			switch scenario {
			case "empty":
				release.Deploy.Argv = nil
			case "relative":
				release.Deploy.Argv = []string{"scripts/deploy"}
			case "candidate":
				release.Deploy.Argv = []string{"/workspaces/attempt/deploy"}
			case "outside":
				release.Deploy.Argv = []string{"/usr/bin/sh"}
			case "timeout":
				release.Deploy.TimeoutSeconds = 3601
			case "missing-rollback":
				release.Rollback.Argv = nil
			}
			project := cfg.Projects["app"]
			project.Releases = map[string]model.Release{"production": release}
			cfg.Projects["app"] = project
			err := Validate(cfg)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("release %s: %v", scenario, err)
			}
		})
	}
}

func TestProjectConcurrencyNormalizesApprovalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	base := "projects:\n  app:\n    trusted: true\n    repository: https://github.com/example/app.git\n"
	load := func(suffix string) (model.Config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(base+suffix), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	implicit, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := load("    max_concurrent: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if implicit.Projects["app"].MaxConcurrent != 1 || implicit.PolicyHash != explicit.PolicyHash {
		t.Fatal("omitted project cap must normalize to the same approval policy as one lane")
	}
	parallel, err := load("    max_concurrent: 2\n")
	if err != nil {
		t.Fatal(err)
	}
	if parallel.PolicyHash == explicit.PolicyHash || parallel.MaxConcurrent != implicit.MaxConcurrent || parallel.MaxCallsPerDay != implicit.MaxCallsPerDay {
		t.Fatal("project concurrency must change approval policy without increasing global/call limits")
	}
	for _, value := range []string{"-1", "33"} {
		if _, err := load("    max_concurrent: " + value + "\n"); err == nil {
			t.Fatalf("accepted invalid project cap %s", value)
		}
	}
}

func TestCanonicalAliasesShareDeliveryResources(t *testing.T) {
	aliases := []string{
		"https://github.com/Example/App.git",
		"https://GITHUB.com:443/example/app/",
		"git@github.com:example/app.git",
		"ssh://git@github.com:22/EXAMPLE/APP.git",
	}
	var expected []string
	for _, repository := range aliases {
		resources, err := IntegrationResources(model.Project{Repository: repository, Branch: "release/main"}, "production")
		if err != nil {
			t.Fatal(err)
		}
		if expected == nil {
			expected = resources
		}
		if strings.Join(resources, "\n") != strings.Join(expected, "\n") {
			t.Fatalf("alias %q escaped shared ownership: %v vs %v", repository, resources, expected)
		}
	}
	if len(expected) != 2 || expected[0] != "git:github.com/example/app:refs/heads/release/main" || expected[1] != "release:github.com/example/app:production" {
		t.Fatalf("wrong delivery resource scope: %v", expected)
	}
	dir := t.TempDir()
	repository := filepath.Join(dir, "repo.git")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(repository, alias); err != nil {
		t.Fatal(err)
	}
	expected = nil
	for _, path := range []string{repository, alias, "file://" + alias} {
		resources, err := IntegrationResources(model.Project{Repository: path, Branch: "main"}, "integrated")
		if err != nil {
			t.Fatal(err)
		}
		if expected == nil {
			expected = resources
		}
		if strings.Join(resources, "\n") != strings.Join(expected, "\n") || len(resources) != 1 {
			t.Fatalf("local alias %q escaped shared ownership: %v vs %v", path, resources, expected)
		}
	}
}

func TestCustomRemoteNormalizationKeepsRepositoryAndEnvironmentScopes(t *testing.T) {
	a := model.Project{Repository: "ssh://git@GIT.example:22/team/App.git", Branch: "main"}
	b := model.Project{Repository: "git@git.example:team/App", Branch: "main"}
	first, err := IntegrationResources(a, "staging")
	if err != nil {
		t.Fatal(err)
	}
	second, err := IntegrationResources(b, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("custom remote aliases differ: %v vs %v", first, second)
	}
	b.Branch = "other"
	second, err = IntegrationResources(b, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if first[0] == second[0] || first[1] != second[1] {
		t.Fatalf("branches must differ but share staging ownership: %v vs %v", first, second)
	}
	third, err := IntegrationResources(b, "production")
	if err != nil {
		t.Fatal(err)
	}
	if second[0] != third[0] || second[1] == third[1] {
		t.Fatalf("same branch shares Git ownership but environments differ: %v vs %v", second, third)
	}
}

func TestGitHubAPIRepositoryMustMatchTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	for _, repository := range []string{"example/app", "EXAMPLE/APP", "example/other"} {
		data := "projects:\n  app:\n    trusted: true\n    repository: git@github.com:example/app.git\n    github_repository: " + repository + "\n"
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if (err == nil) != strings.EqualFold(repository, "example/app") {
			t.Fatalf("API repository %s: %v", repository, err)
		}
	}
}
