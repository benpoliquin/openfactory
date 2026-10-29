package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func loadIntakeFixture(t *testing.T, path, policy string) (model.Config, error) {
	t.Helper()
	if err := os.WriteFile(path, []byte("state_dir: state\nworkspace_dir: workspaces\n"+policy), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestIntakeRequiresExplicitOriginAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	c, err := loadIntakeFixture(t, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Intake.Enabled || c.Intake.ExternalOrigin != "" || c.Intake.RPID != "" {
		t.Fatal("omitted intake policy invented browser authority")
	}
	for _, policy := range []string{
		"intake:\n  enabled: true\n",
		"intake:\n  enabled: true\n  external_origin: https://openfactory.example.ts.net\n",
		"intake:\n  enabled: true\n  rp_id: openfactory.example.ts.net\n",
	} {
		if _, err := loadIntakeFixture(t, path, policy); err == nil {
			t.Fatalf("incomplete browser authority accepted: %s", policy)
		}
	}
}

func TestIntakeOriginAndRPIDAreExact(t *testing.T) {
	for _, v := range []struct {
		origin, rp string
		valid      bool
	}{
		{"https://openfactory.example.ts.net", "openfactory.example.ts.net", true},
		{"https://openfactory.example.ts.net:9443", "openfactory.example.ts.net", true},
		{"http://localhost:40903", "localhost", true},
		{"https://localhost", "localhost", true},
		{"http://localhost", "localhost", true},
		{"https://openfactory.example.ts.net:443", "openfactory.example.ts.net", false},
		{"http://localhost:80", "localhost", false},
		{"HTTPS://openfactory.example.ts.net", "openfactory.example.ts.net", false},
		{"HTTP://localhost:40903", "localhost", false},
		{"https://openfactory.example.ts.net:09443", "openfactory.example.ts.net", false},
		{"http://localhost:040903", "localhost", false},
		{"http://openfactory.example.ts.net", "openfactory.example.ts.net", false},
		{"http://127.0.0.1:40903", "127.0.0.1", false},
		{"https://192.0.2.1", "192.0.2.1", false},
		{"https://[openfactory.example.ts.net]", "openfactory.example.ts.net", false},
		{"http://localhost.evil.example", "localhost.evil.example", false},
		{"https://openfactory.example.ts.net/", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net/path", "openfactory.example.ts.net", false},
		{"https://user@openfactory.example.ts.net", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net?", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net#fragment", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net:", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net:0", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net:65536", "openfactory.example.ts.net", false},
		{"https://OpenFactory.example.ts.net", "openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net.", "openfactory.example.ts.net.", false},
		{"https://openfactory.example.ts.net", "example.ts.net", false},
		{"https://openfactory.example.ts.net", "openfactory.example.ts.net:443", false},
		{"https://openfactory.example.ts.net", "https://openfactory.example.ts.net", false},
		{"https://openfactory.example.ts.net", "other.example.ts.net", false},
	} {
		t.Run(v.origin+"|"+v.rp, func(t *testing.T) {
			_, err := normalizeIntake(model.IntakePolicy{Enabled: true, ExternalOrigin: v.origin, RPID: v.rp})
			if (err == nil) != v.valid {
				t.Fatalf("origin/RP validation got %v, want valid=%v", err, v.valid)
			}
		})
	}
}

func TestIntakeRejectsFractionalAliasedAndUnboundedLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	fields := []string{"max_turn_bytes", "max_turns", "max_text_bytes", "max_canonical_bytes", "max_output_bytes", "max_draft_calls", "history_default", "history_max", "pairing_ttl_seconds", "challenge_ttl_seconds", "session_idle_seconds", "session_absolute_seconds", "credential_ttl_seconds", "max_credentials", "auth_rate_limit"}
	for _, field := range fields {
		for _, value := range []string{"1.5", "-1", "999999999", "\"1\""} {
			t.Run(field+"="+value, func(t *testing.T) {
				if _, err := loadIntakeFixture(t, path, "intake:\n  "+field+": "+value+"\n"); err == nil {
					t.Fatal("invalid bound accepted or fractional scalar truncated")
				}
			})
		}
		policy := "intake:\n  <<: &limits\n    " + field + ": 1.5\n"
		if _, err := loadIntakeFixture(t, path, policy); err == nil {
			t.Fatalf("merged fractional %s accepted", field)
		}
	}
	for _, policy := range []string{
		"intake:\n  enabled: \"true\"\n",
		"intake:\n  enabled: 1\n",
		"intake:\n  allow_non_loopback: \"true\"\n",
		"intake:\n  public_path: /other\n",
		"intake:\n  bind: localhost:38889\n",
		"intake:\n  bind: 127.0.0.1:0\n",
		"intake:\n  bind: 0.0.0.0:38889\n",
		"intake:\n  history_default: 51\n  history_max: 50\n",
		"intake:\n  max_turn_bytes: 100\n  max_text_bytes: 99\n",
		"intake:\n  session_idle_seconds: 60\n  session_absolute_seconds: 59\n",
	} {
		if _, err := loadIntakeFixture(t, path, policy); err == nil {
			t.Fatalf("invalid policy accepted: %s", policy)
		}
	}
}

func TestIntakePolicyChangesInvalidateApprovalHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	base := "intake:\n  enabled: true\n  external_origin: https://openfactory.example.ts.net\n  rp_id: openfactory.example.ts.net\n"
	original, err := loadIntakeFixture(t, path, base)
	if err != nil {
		t.Fatal(err)
	}
	// Every setting changes either authority, disclosed draft bounds, or auth
	// validity, so no persisted grant may silently retain its old policy hash.
	for _, suffix := range []string{
		"  max_turn_bytes: 100\n", "  max_turns: 10\n", "  max_text_bytes: 20000\n",
		"  max_canonical_bytes: 100\n", "  max_output_bytes: 100\n", "  max_draft_calls: 1\n",
		"  pairing_ttl_seconds: 60\n", "  challenge_ttl_seconds: 30\n", "  session_idle_seconds: 60\n",
		"  session_absolute_seconds: 43200\n", "  credential_ttl_seconds: 600\n", "  max_credentials: 1\n", "  auth_rate_limit: 1\n",
	} {
		c, err := loadIntakeFixture(t, path, base+suffix)
		if err != nil {
			t.Fatal(err)
		}
		if c.PolicyHash == original.PolicyHash {
			t.Fatalf("changed authority retained approval hash: %s", suffix)
		}
	}
	explicit, err := loadIntakeFixture(t, path, base+"  max_draft_calls: 12\n")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.PolicyHash != original.PolicyHash {
		t.Fatal("equivalent effective policy hashes differ")
	}
	disabled, err := loadIntakeFixture(t, path, strings.Replace(base, "enabled: true", "enabled: false", 1))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.PolicyHash == original.PolicyHash {
		t.Fatal("browser authority enablement did not affect policy hash")
	}
}
