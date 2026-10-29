package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func TestPutReadIntegrityAndCancellation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := New(root)
	data := []byte("complete immutable contents")
	ref, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if ref.SHA != digest(data) || ref.Bytes != int64(len(data)) {
		t.Fatalf("incorrect reference: %+v", ref)
	}
	if _, err = s.Put(ctx, data); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(ctx, ref.SHA, ref.Bytes-1); err == nil {
		t.Fatal("oversized read accepted")
	}
	if err = os.WriteFile(filepath.Join(root, ref.SHA), []byte(strings.Repeat("x", len(data))), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(ctx, ref.SHA, ref.Bytes); err == nil {
		t.Fatal("corruption accepted")
	}
	if _, err = s.Put(ctx, data); err == nil {
		t.Fatal("existing corruption silently replaced")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.Put(canceled, []byte("cancelled")); err == nil {
		t.Fatal("cancelled write accepted")
	}
	if _, err = os.Stat(filepath.Join(root, digest([]byte("cancelled")))); !os.IsNotExist(err) {
		t.Fatalf("cancelled write published blob: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatal("temporary file leaked")
		}
	}
}
func TestBundleExplicitRoundTripAndLimits(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for p, b := range map[string]string{"nested/b.txt": "beta", "a.txt": "alpha", "secret": "not declared"} {
		if err := os.WriteFile(filepath.Join(source, p), []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(t.TempDir())
	limits := model.BundleLimits{MaxFiles: 2, MaxFileBytes: 8, MaxTotalBytes: 9}
	bundle, err := s.ImportBundle(ctx, source, []string{"nested/b.txt", "a.txt"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ImportBundle(ctx, source, []string{"a.txt", "nested/b.txt"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if second.ManifestSHA != bundle.ManifestSHA {
		t.Fatal("manifest depends on declaration ordering")
	}
	loaded, err := s.LoadBundle(ctx, bundle.ManifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "output")
	if err = s.MaterializeBundle(ctx, loaded, dest, limits); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "nested/b.txt"))
	if err != nil || string(b) != "beta" {
		t.Fatalf("round trip: %q %v", b, err)
	}
	if _, err = os.Stat(filepath.Join(dest, "secret")); !os.IsNotExist(err) {
		t.Fatal("undeclared file copied")
	}
	for _, declared := range [][]string{{"../secret"}, {"/etc/passwd"}, {"a.txt", "a.txt"}, {"a.txt", "nested/b.txt", "secret"}} {
		if _, err = s.ImportBundle(ctx, source, declared, limits); err == nil {
			t.Fatalf("unsafe declaration accepted: %v", declared)
		}
	}
	limits.MaxTotalBytes = 8
	if _, err = s.ImportBundle(ctx, source, []string{"a.txt", "nested/b.txt"}, limits); err == nil {
		t.Fatal("total size limit bypassed")
	}
	limits.MaxFileBytes = 3
	if _, err = s.ImportBundle(ctx, source, []string{"a.txt"}, limits); err == nil {
		t.Fatal("file size limit bypassed")
	}
	bundle.Files[0].Path = "altered"
	if err = s.MaterializeBundle(ctx, bundle, t.TempDir(), model.BundleLimits{MaxFiles: 2, MaxFileBytes: 8, MaxTotalBytes: 9}); err == nil {
		t.Fatal("altered manifest accepted")
	}
}
func TestSymlinksRejectedAtEveryBoundary(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(root, "linked")).Put(ctx, []byte("x")); err == nil {
		t.Fatal("symlink CAS root accepted")
	}
	aliased := New(filepath.Join(root, "linked", "cas"))
	ref, err := aliased.Put(ctx, []byte("ancestor alias"))
	if err != nil {
		t.Fatalf("trusted container alias rejected: %v", err)
	}
	if _, err = aliased.Read(ctx, ref.SHA, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.TempDir()).ImportBundle(ctx, filepath.Join(root, "linked"), []string{"file"}, model.BundleLimits{MaxFiles: 1, MaxFileBytes: 100, MaxTotalBytes: 100}); err == nil {
		t.Fatal("symlink source root accepted")
	}
	s := New(t.TempDir())
	limits := model.BundleLimits{MaxFiles: 4, MaxFileBytes: 100, MaxTotalBytes: 200}
	if err := os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"file", "linked/file"} {
		if _, err := s.ImportBundle(ctx, root, []string{p}, limits); err == nil {
			t.Fatalf("symlink input accepted: %s", p)
		}
	}
	bundle, err := s.ImportBundle(ctx, outside, []string{"file"}, limits)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err = os.Symlink(filepath.Join(outside, "file"), filepath.Join(dest, "file")); err != nil {
		t.Fatal(err)
	}
	if err = s.MaterializeBundle(ctx, bundle, dest, limits); err == nil {
		t.Fatal("symlink output accepted")
	}
	b, err := os.ReadFile(filepath.Join(outside, "file"))
	if err != nil || string(b) != "private" {
		t.Fatal("outside target was modified")
	}
}
