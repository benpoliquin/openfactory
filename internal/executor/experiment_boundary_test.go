package executor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestExperimentArtifactFIFOIsRejectedWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readOutput(root, "fifo", 1024); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO artifact")
		}
	case <-time.After(time.Second):
		t.Fatal("artifact FIFO blocked beyond capture deadline")
	}
}
func TestExperimentArtifactNestedDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "result.json"), []byte(`{"value":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "output")); err != nil {
		t.Fatal(err)
	}
	if _, err := readOutput(root, "output/result.json", 1024); err == nil {
		t.Fatal("followed artifact parent symlink")
	}
	if err := os.Mkdir(filepath.Join(root, "safe"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "safe", "result.json"), []byte(`{"value":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readOutput(root, "safe/result.json", 1024); err != nil || string(data) != `{"value":true}` {
		t.Fatalf("descriptor-rooted nested output unavailable: %q %v", data, err)
	}
}
