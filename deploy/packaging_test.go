package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"examples", "fake-bin"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("../start.sh")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"start.sh":                  string(script),
		".env.example":              "OPENFACTORY_ASTRA_MODEL=\nCOMPOSE_FILE=compose.yaml\n",
		"examples/openfactory.yaml": "state_dir: /state\nworkspace_dir: /workspaces\nprojects: {}\n",
		"fake-bin/docker":           "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CALLS\"\nif [ \"$1\" = version ]; then printf '1.41\\n'; fi\nexit 0\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Exclude operator overrides from the developer machine; no real Docker
	// command is executed by these starter behavior tests.
	environment := []string{
		"PATH=" + filepath.Join(root, "fake-bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + root,
		"DOCKER_CALLS=" + filepath.Join(root, "docker-calls"),
	}
	return root, environment
}

func runStarter(t *testing.T, root string, environment []string, args ...string) ([]byte, error) {
	t.Helper()
	command := exec.Command("bash", append([]string{filepath.Join(root, "start.sh")}, args...)...)
	command.Env = environment
	command.Dir = t.TempDir() // Invocation need not happen in the repository.
	return command.CombinedOutput()
}

func TestStarterCreatesIdleConfigAndPreservesExistingFiles(t *testing.T) {
	root, environment := fixture(t)
	output, err := runStarter(t, root, environment)
	if err != nil {
		t.Fatalf("start: %v\n%s", err, output)
	}
	config := filepath.Join(root, "config/openfactory.yaml")
	data, err := os.ReadFile(config)
	if err != nil || !strings.Contains(string(data), "projects: {}") {
		t.Fatalf("fresh config must remain unenrolled: %q, %v", data, err)
	}
	info, err := os.Stat(config)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("new config must be readable by the container UID: %v, %v", info, err)
	}
	for name, body := range map[string]string{
		"config/openfactory.yaml": "operator-owned-config\n",
		".env":                    "operator-owned-environment\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err = runStarter(t, root, environment); err != nil {
		t.Fatalf("restart: %v\n%s", err, output)
	}
	for name, want := range map[string]string{
		"config/openfactory.yaml": "operator-owned-config\n",
		".env":                    "operator-owned-environment\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("overwrote %s: %q, %v", name, got, err)
		}
	}
	calls, err := os.ReadFile(filepath.Join(root, "docker-calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{" approve ", " enqueue ", "down --volumes", "volume rm"} {
		if strings.Contains(string(calls), forbidden) {
			t.Fatalf("unsafe startup operation %q: %s", forbidden, calls)
		}
	}
}

func TestStarterHonorsCustomFilesWithoutExecutingDotenv(t *testing.T) {
	root, environment := fixture(t)
	envPath := filepath.Join(root, "operator.env")
	configPath := filepath.Join(root, "operator.yaml")
	marker := filepath.Join(root, "must-not-exist")
	if err := os.WriteFile(envPath, []byte("MALICIOUS=$(touch "+marker+")\n"), 0600); err != nil {
		t.Fatal(err)
	}
	environment = append(environment, "OPENFACTORY_ENV_FILE="+envPath, "OPENFACTORY_CONFIG_FILE="+configPath)
	output, err := runStarter(t, root, environment)
	if err != nil {
		t.Fatalf("custom start: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("dotenv was executed as shell code: %v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "config")); !os.IsNotExist(err) {
		t.Fatalf("custom config must not create a default config directory: %v", err)
	}
}

func TestStarterRejectsMissingConfigExampleBeforeStartup(t *testing.T) {
	root, environment := fixture(t)
	if err := os.Remove(filepath.Join(root, "examples/openfactory.yaml")); err != nil {
		t.Fatal(err)
	}
	output, err := runStarter(t, root, environment)
	if err == nil || !strings.Contains(string(output), "Missing packaged example") {
		t.Fatalf("missing example must fail closed: %v\n%s", err, output)
	}
	calls, err := os.ReadFile(filepath.Join(root, "docker-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), " up ") {
		t.Fatalf("started without a config: %s", calls)
	}
}

func TestBBEntrypointRejectsMissingOrEmptyExplicitSecret(t *testing.T) {
	script, err := filepath.Abs("bb-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing", "empty", "valid"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "openrouter")
			if mode != "missing" {
				value := ""
				if mode == "valid" {
					value = "test-only-not-a-real-provider-key\n"
				}
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("sh", script, "sh", "-c", `test "$OPENROUTER_API_KEY" = test-only-not-a-real-provider-key`)
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "OPENROUTER_API_KEY_FILE=" + path}
			output, err := command.CombinedOutput()
			if mode == "valid" && err != nil {
				t.Fatalf("valid secret did not reach child: %v\n%s", err, output)
			}
			if mode != "valid" && err == nil {
				t.Fatal("explicit unavailable secret must fail rather than use ambient credentials")
			}
			if strings.Contains(string(output), "test-only-not-a-real-provider-key") {
				t.Fatal("entrypoint disclosed the secret")
			}
		})
	}
}
