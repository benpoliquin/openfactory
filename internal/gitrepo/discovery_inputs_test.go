package gitrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"openfactory/internal/model"
)

func discoveryInput(path string, data []byte) model.DiscoveryInputFile {
	sum := sha256.Sum256(data)
	return model.DiscoveryInputFile{Path: path, ContentSHA: hex.EncodeToString(sum[:]), Data: data}
}

func discoveryInputSession(files ...model.DiscoveryInputFile) model.DiscoverySession {
	return model.DiscoverySession{
		AttemptID:       "discovery-attempt",
		Role:            "architecture",
		StepID:          "step-one",
		CapabilityToken: "private-capability-not-for-model-context",
		InputFiles:      files,
	}
}

func TestDiscoveryInputsMaterializePrivatelyAndRetryWithoutReplacement(t *testing.T) {
	f, w := discoveryFixture(t)
	ctx := context.Background()
	contextFile := discoveryInput("inputs/context.json", []byte(`{"objective":"complete canonical context, not a prompt prefix"}`))
	blob := discoveryInput("inputs/blobs/source.bin", []byte{0, 1, 2, 255, '\n'})
	session := discoveryInputSession(contextFile, blob)
	if err := f.m.ConfigureDiscoverySession(ctx, w, session); err != nil {
		t.Fatal(err)
	}
	for _, input := range session.InputFiles {
		path := filepath.Join(w.Path, filepath.FromSlash(input.Path))
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, input.Data) {
			t.Fatalf("input bytes changed: %s %v", input.Path, err)
		}
		if got := discoveryInput(input.Path, data).ContentSHA; got != input.ContentSHA {
			t.Fatalf("input SHA changed: %s", input.Path)
		}
		info, err := os.Lstat(path)
		if err != nil || !singleLinkDiscoveryInput(info) || info.Mode().Perm() != 0600 {
			t.Fatalf("input not private and single-linked: %s %v", input.Path, err)
		}
		if bytes.Contains(data, []byte(session.CapabilityToken)) {
			t.Fatal("private capability escaped into model-readable input")
		}
	}
	controlPath := filepath.Join(w.Path, ".openfactory", "discovery-session.json")
	control, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(control, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["input_files"]; exists {
		t.Fatal("private input transport serialized into the controlfile")
	}
	if _, exists := fields["InputFiles"]; exists {
		t.Fatal("private input transport serialized with Go field names")
	}
	for _, input := range session.InputFiles {
		if bytes.Contains(control, []byte(input.Path)) || bytes.Contains(control, []byte(input.ContentSHA)) || bytes.Contains(control, input.Data) {
			t.Fatal("input transport leaked into session JSON")
		}
	}
	var privateSession model.DiscoverySession
	if err = json.Unmarshal(control, &privateSession); err != nil || privateSession.CapabilityToken != session.CapabilityToken {
		t.Fatal("private controlfile lost its intended capability")
	}
	inputPath := filepath.Join(w.Path, "inputs", "context.json")
	before, err := os.Stat(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	session.CapabilityToken = "replacement-private-capability"
	if err = f.m.ConfigureDiscoverySession(ctx, w, session); err != nil {
		t.Fatalf("identical input retry failed: %v", err)
	}
	after, err := os.Stat(inputPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("idempotent retry replaced input identity: %v", err)
	}
	second, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil || first != second {
		t.Fatalf("token-only retry changed content fingerprint: %v", err)
	}
	control, err = os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	session.InputFiles[0] = discoveryInput(contextFile.Path, bytes.Repeat([]byte{'x'}, len(contextFile.Data)))
	session.CapabilityToken = "must-not-publish-on-failed-input"
	if err = f.m.ConfigureDiscoverySession(ctx, w, session); err == nil {
		t.Fatal("differing existing input was overwritten")
	}
	data, err := os.ReadFile(inputPath)
	if err != nil || !bytes.Equal(data, contextFile.Data) {
		t.Fatalf("failed replacement altered input bytes: %v", err)
	}
	unchanged, err := os.ReadFile(controlPath)
	if err != nil || !bytes.Equal(unchanged, control) {
		t.Fatalf("session was published before input rejection: %v", err)
	}
	put(t, inputPath, "agent mutation of controller input")
	changed, err := f.m.FingerprintDiscovery(ctx, w)
	if err != nil || changed == first {
		t.Fatalf("input mutation escaped workspace fingerprint: %v", err)
	}
	if err = f.m.ConfigureDiscoverySession(ctx, w, discoveryInputSession(contextFile, blob)); err == nil {
		t.Fatal("mutated input was silently repaired")
	}
	if err = filepath.WalkDir(w.Path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".input-") {
			t.Fatalf("temporary input file remains: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryInputsRejectInvalidBatchesBeforePublishing(t *testing.T) {
	f, w := discoveryFixture(t)
	valid := discoveryInput("inputs/valid.json", []byte("valid canonical input"))
	for _, path := range []string{
		"", "inputs", "../inputs/context.json", "/inputs/context.json", "other/context.json",
		"inputs/../.openfactory/discovery-session.json", "inputs/context.json/..", "inputs//context.json",
		"inputs\\context.json", "inputs/.git/config", "inputs/.openfactory/discovery-session.json",
		".openfactory/discovery-session.json", "inputs/\x00bad", "Inputs/context.json",
	} {
		t.Run("path="+path, func(t *testing.T) {
			bad := discoveryInput(path, []byte("rejected context"))
			if err := f.m.ConfigureDiscoverySession(context.Background(), w, discoveryInputSession(valid, bad)); err == nil {
				t.Fatalf("invalid input path accepted: %q", path)
			}
		})
	}
	badHash := valid
	badHash.Path = "inputs/bad-hash.json"
	badHash.ContentSHA = strings.Repeat("0", sha256.Size*2)
	uppercaseHash := valid
	uppercaseHash.Path = "inputs/uppercase-hash.json"
	uppercaseHash.ContentSHA = strings.ToUpper(uppercaseHash.ContentSHA)
	for name, files := range map[string][]model.DiscoveryInputFile{
		"hash":           {valid, badHash},
		"canonical_hash": {valid, uppercaseHash},
		"duplicate":      {valid, valid},
		"file_parent":    {valid, discoveryInput("inputs/valid.json/child", []byte("child"))},
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.m.ConfigureDiscoverySession(context.Background(), w, discoveryInputSession(files...)); err == nil {
				t.Fatal("invalid input batch accepted")
			}
		})
	}
	for _, name := range []string{"inputs", ".openfactory"} {
		if _, err := os.Lstat(filepath.Join(w.Path, name)); !os.IsNotExist(err) {
			t.Fatalf("invalid batch changed workspace namespace: %s %v", name, err)
		}
	}
}

func TestDiscoveryInputsRejectSymlinksHardlinksAndNonregularFiles(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, string, string)
	}{
		{"parent_symlink", func(t *testing.T, workspace, outside string) {
			if err := os.Symlink(outside, filepath.Join(workspace, "inputs")); err != nil {
				t.Fatal(err)
			}
		}},
		{"internal_parent_symlink", func(t *testing.T, workspace, outside string) {
			if err := os.Mkdir(filepath.Join(workspace, "real-inputs"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real-inputs", filepath.Join(workspace, "inputs")); err != nil {
				t.Fatal(err)
			}
		}},
		{"nested_parent_symlink", func(t *testing.T, workspace, outside string) {
			if err := os.Mkdir(filepath.Join(workspace, "inputs"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(workspace, "inputs", "nested")); err != nil {
				t.Fatal(err)
			}
		}},
		{"file_symlink", func(t *testing.T, workspace, outside string) {
			if err := os.MkdirAll(filepath.Join(workspace, "inputs", "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "context.json"), filepath.Join(workspace, "inputs", "nested", "context.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlinked_file", func(t *testing.T, workspace, outside string) {
			if err := os.MkdirAll(filepath.Join(workspace, "inputs", "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(outside, "context.json"), filepath.Join(workspace, "inputs", "nested", "context.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory_file", func(t *testing.T, workspace, outside string) {
			if err := os.MkdirAll(filepath.Join(workspace, "inputs", "nested", "context.json"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo_file", func(t *testing.T, workspace, outside string) {
			if err := os.MkdirAll(filepath.Join(workspace, "inputs", "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(workspace, "inputs", "nested", "context.json"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, w := discoveryFixture(t)
			outside := t.TempDir()
			data := []byte("identical bytes still cannot authorize links")
			put(t, filepath.Join(outside, "context.json"), string(data))
			tc.prepare(t, w.Path, outside)
			session := discoveryInputSession(discoveryInput("inputs/nested/context.json", data))
			if err := f.m.ConfigureDiscoverySession(context.Background(), w, session); err == nil {
				t.Fatal("unsafe input namespace accepted")
			}
			unchanged, err := os.ReadFile(filepath.Join(outside, "context.json"))
			if err != nil || !bytes.Equal(unchanged, data) {
				t.Fatalf("input materialization mutated external bytes: %v", err)
			}
			if _, err = os.Lstat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
				t.Fatalf("input materialization escaped through symlink: %v", err)
			}
			if _, err = os.Lstat(filepath.Join(w.Path, ".openfactory", "discovery-session.json")); !os.IsNotExist(err) {
				t.Fatalf("unsafe inputs still published a session: %v", err)
			}
		})
	}
}

func TestDiscoveryInputsEnforceExactFileByteLimit(t *testing.T) {
	f, w := discoveryFixture(t)
	data := make([]byte, discoveryInputMaxBytes+1)
	atLimit := discoveryInput("inputs/maximum.bin", data[:discoveryInputMaxBytes])
	if err := f.m.ConfigureDiscoverySession(context.Background(), w, discoveryInputSession(atLimit)); err != nil {
		t.Fatalf("exactly 64 MiB input rejected: %v", err)
	}
	tooLarge := model.DiscoveryInputFile{Path: atLimit.Path, ContentSHA: atLimit.ContentSHA, Data: data}
	if err := f.m.ConfigureDiscoverySession(context.Background(), w, discoveryInputSession(tooLarge)); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized input not rejected by byte cap: %v", err)
	}
	info, err := os.Stat(filepath.Join(w.Path, "inputs", "maximum.bin"))
	if err != nil || info.Size() != discoveryInputMaxBytes {
		t.Fatalf("oversized retry changed prior input: %v", err)
	}
}

func TestDiscoveryInputsKeepCapabilitiesOutOfContext(t *testing.T) {
	f, w := discoveryFixture(t)
	session := discoveryInputSession()
	data := []byte(`{"accidental_private_capability":"` + session.CapabilityToken + `"}`)
	session.InputFiles = []model.DiscoveryInputFile{discoveryInput("inputs/context.json", data)}
	err := f.m.ConfigureDiscoverySession(context.Background(), w, session)
	if err == nil {
		t.Fatal("model-readable context accepted private capability")
	}
	if strings.Contains(err.Error(), session.CapabilityToken) || bytes.Contains([]byte(err.Error()), data) {
		t.Fatal("input rejection exposed private bytes in its error")
	}
	for _, name := range []string{"inputs", ".openfactory"} {
		if _, err := os.Lstat(filepath.Join(w.Path, name)); !os.IsNotExist(err) {
			t.Fatalf("capability rejection still materialized inputs or controlfile: %v", err)
		}
	}
}
