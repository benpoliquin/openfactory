package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"openfactory/internal/model"
)

func TestSharedCheckoutCannotWriteThroughOutsideParent(t *testing.T) {
	shared, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	outside := t.TempDir()
	put(t, filepath.Join(source, "proof.txt"), "candidate bytes")
	manager := New(model.Config{WorkspaceDir: shared})
	if err = manager.copyCheckout(context.Background(), source, filepath.Join(shared, "safe")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(shared, "safe", "proof.txt")); err != nil || string(data) != "candidate bytes" {
		t.Fatalf("valid rooted checkout: %q %v", data, err)
	}
	if err = os.Symlink(outside, filepath.Join(shared, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err = manager.copyCheckout(context.Background(), source, filepath.Join(shared, "redirect", "escaped")); err == nil {
		t.Fatal("checkout followed a shared-volume symlink outside its root")
	}
	if _, err = os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("checkout wrote outside its boundary: %v", err)
	}
}

func TestCaptureRejectsWorkspaceSwapAfterValidation(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	receipt, err := f.m.workspace(ctx, f.a.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	put(t, filepath.Join(outside, "private-key"), "controller-secret-must-not-be-captured")
	if err = os.Rename(receipt.Path, receipt.Path+"-retired"); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, receipt.Path); err != nil {
		t.Fatal(err)
	}
	// Simulate the path swap after workspace validation but before Git capture.
	if _, err = f.m.capture(ctx, receipt); err == nil {
		t.Fatal("captured files outside shared volume after a root swap")
	}
}

func TestSnapshotDoesNotDistributeRejectedPrivateObjects(t *testing.T) {
	f := setup(t, nil)
	ctx := context.Background()
	put(t, filepath.Join(f.a.Workspace, "app.txt"), "rejected candidate content")
	rejected, err := f.m.Commit(ctx, f.a.Workspace, "rejected candidate")
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.a.Workspace, "app.txt"), "accepted candidate content")
	candidate, err := f.m.Commit(ctx, f.a.Workspace, "approved candidate")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.m.Snapshot(ctx, f.a.Workspace, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := testGit(t, snapshot.Path, "rev-parse", "HEAD"); got != candidate {
		t.Fatalf("snapshot candidate %s", got)
	}
	if _, err = runGit(ctx, snapshot.Path, gitArgs("cat-file", "-e", rejected+"^{commit}"), nil, nil, 120); err == nil {
		t.Fatal("snapshot distributed rejected private commit objects")
	}
}
