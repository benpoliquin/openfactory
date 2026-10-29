package config

import (
	"path/filepath"
	"testing"
)

func TestBBHostAuthorityRejectsNonAuthorityAndMalformedPorts(t *testing.T) {
	for _, authority := range []string{"", "bb", "localhost:38886", "remote.example:9443", "127.0.0.1:38886", "[::1]", "[::1]:38886"} {
		if err := validateBBHostAuthority(authority); err != nil {
			t.Fatalf("valid authority %q rejected: %v", authority, err)
		}
	}
	for _, authority := range []string{"http://localhost:38886", "https://remote.example", "user@localhost:38886", "localhost:38886/path", "localhost:38886?query", "localhost:38886#fragment", "localhost:38886\r\nX-Actor: yes", "localhost:38886\n", "localhost:", "localhost:0", "localhost:65536", "localhost:038886", "localhost:port", "::1", "[localhost]:38886", "local host:38886", "[::1]:", "-bad:38886", "bb..example:38886"} {
		if err := validateBBHostAuthority(authority); err == nil {
			t.Fatalf("unsafe or ambiguous authority accepted: %q", authority)
		}
	}
}

func TestBBHostAuthorityEnvironmentIsExplicitAndPolicyHashed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.yaml")
	t.Setenv("OPENFACTORY_BB_HOST_AUTHORITY", "")
	base, err := loadIntakeFixture(t, path, "bb_url: https://remote.example:9443\n")
	if err != nil {
		t.Fatal(err)
	}
	if base.BBHostAuthority != "" {
		t.Fatal("empty override did not preserve URL authority")
	}
	t.Setenv("OPENFACTORY_BB_HOST_AUTHORITY", "localhost:38886")
	override, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if override.BBHostAuthority != "localhost:38886" || override.BBURL != base.BBURL || override.PolicyHash == base.PolicyHash {
		t.Fatal("explicit Host override changed destination or retained stale approval policy")
	}
	explicit, err := loadIntakeFixture(t, path, "bb_url: https://remote.example:9443\nbb_host_authority: remote.example:9443\n")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.BBHostAuthority != "remote.example:9443" {
		t.Fatal("ambient Host override replaced explicit operator config")
	}
	t.Setenv("OPENFACTORY_BB_HOST_AUTHORITY", "http://wrong.example")
	if _, err := loadIntakeFixture(t, path, "bb_url: http://bb:38886\n"); err == nil {
		t.Fatal("malformed ambient authority reached transport")
	}
}
