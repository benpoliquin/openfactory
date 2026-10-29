package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func streamAssertNoTemporary(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("stream staging leaked: %s", entry.Name())
		}
	}
}
func TestStreamCASExactBoundedRoundTripAndReplay(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	ctx := context.Background()
	data := bytes.Repeat([]byte("bounded immutable contents\n"), 100000)
	sha := digest(data)
	ref, err := s.PutReader(ctx, bytes.NewReader(data), sha, int64(len(data)), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if ref.SHA != sha || ref.Bytes != int64(len(data)) {
		t.Fatal("stream ref omitted exact identity")
	}
	hash := sha256.New()
	read, err := s.ReadTo(ctx, sha, ref.Bytes, ref.Bytes, hash)
	if err != nil || read != ref || hex.EncodeToString(hash.Sum(nil)) != sha {
		t.Fatalf("stream round trip failed: %+v %v", read, err)
	}
	replay, err := s.PutReader(ctx, bytes.NewReader(data), sha, ref.Bytes, ref.Bytes)
	if err != nil || replay != ref {
		t.Fatalf("identical stream replay failed: %+v %v", replay, err)
	}
	if _, err = s.ReadTo(ctx, sha, ref.Bytes, ref.Bytes-1, io.Discard); !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("stream reader ignored max: %v", err)
	}
	streamAssertNoTemporary(t, root)
}

type streamCountReader struct{ count int64 }

func (r *streamCountReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.count += int64(len(p))
	return len(p), nil
}
func TestStreamCASRejectsHashSizeAndUnboundedInput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     string
		declared int64
		max      int64
		sha      string
	}{{"short", "abc", 4, 4, digest([]byte("abcd"))}, {"long", "abcde", 4, 4, digest([]byte("abcd"))}, {"hash", "abce", 4, 4, digest([]byte("abcd"))}, {"limit", "abcd", 4, 3, digest([]byte("abcd"))}, {"path", "abcd", 4, 4, "../outside"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			_, err := New(root).PutReader(context.Background(), strings.NewReader(tc.data), tc.sha, tc.declared, tc.max)
			if err == nil {
				t.Fatal("invalid stream produced a reference")
			}
			if validSHA(tc.sha) {
				if _, e := os.Stat(filepath.Join(root, tc.sha)); !os.IsNotExist(e) {
					t.Fatalf("failed stream published blob: %v", e)
				}
			}
			streamAssertNoTemporary(t, root)
		})
	}
	root := t.TempDir()
	reader := &streamCountReader{}
	_, err := New(root).PutReader(context.Background(), reader, digest([]byte("xxxx")), 4, 4)
	if !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("unbounded stream error: %v", err)
	}
	if reader.count != 5 {
		t.Fatalf("read %d bytes, want exact limit plus one", reader.count)
	}
	streamAssertNoTemporary(t, root)
}
func TestStreamCASPreservesExistingCorruptionAndRejectsLinks(t *testing.T) {
	ctx := context.Background()
	data := []byte("declared immutable bytes")
	sha := digest(data)
	t.Run("corrupt", func(t *testing.T) {
		root := t.TempDir()
		corrupt := bytes.Repeat([]byte("x"), len(data))
		if err := os.WriteFile(filepath.Join(root, sha), corrupt, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(root).PutReader(ctx, bytes.NewReader(data), sha, int64(len(data)), 100); err == nil {
			t.Fatal("corrupt preexisting blob overwritten")
		}
		got, err := os.ReadFile(filepath.Join(root, sha))
		if err != nil || !bytes.Equal(got, corrupt) {
			t.Fatal("failed stream modified existing CAS object")
		}
		streamAssertNoTemporary(t, root)
	})
	for _, hard := range []bool{false, true} {
		name := "symlink"
		if hard {
			name = "hardlink"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(outside, data, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if hard {
				err = os.Link(outside, filepath.Join(root, sha))
			} else {
				err = os.Symlink(outside, filepath.Join(root, sha))
			}
			if err != nil {
				t.Fatal(err)
			}
			s := New(root)
			if _, err = s.PutReader(ctx, bytes.NewReader(data), sha, int64(len(data)), 100); err == nil {
				t.Fatal("linked CAS destination accepted")
			}
			if _, err = s.ReadTo(ctx, sha, int64(len(data)), 100, io.Discard); err == nil {
				t.Fatal("linked CAS read accepted")
			}
			got, e := os.ReadFile(outside)
			if e != nil || !bytes.Equal(got, data) {
				t.Fatal("external link target changed")
			}
			streamAssertNoTemporary(t, root)
		})
	}
	root := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := New(alias).PutReader(ctx, bytes.NewReader(data), sha, int64(len(data)), 100); err == nil {
		t.Fatal("symlink CAS root accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink root caused external writes")
	}
}

type streamHookReader struct {
	reader  io.Reader
	hook    func()
	invoked bool
}

func (r *streamHookReader) Read(p []byte) (int, error) {
	if !r.invoked {
		r.invoked = true
		r.hook()
	}
	return r.reader.Read(p)
}
func TestStreamCASRejectsStagingSubstitutionAndCleansCancellation(t *testing.T) {
	ctx := context.Background()
	data := []byte("bounded race contents")
	sha := digest(data)
	t.Run("staging-link-race", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "private")
		if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		reader := &streamHookReader{reader: bytes.NewReader(data), hook: func() {
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".tmp-") {
					name := filepath.Join(root, entry.Name())
					if err = os.Remove(name); err != nil {
						t.Fatal(err)
					}
					if err = os.Symlink(outside, name); err != nil {
						t.Fatal(err)
					}
					return
				}
			}
			t.Fatal("no actual secure staging file was created")
		}}
		if _, err := New(root).PutReader(ctx, reader, sha, int64(len(data)), 100); err == nil {
			t.Fatal("substituted staging path published")
		}
		got, err := os.ReadFile(outside)
		if err != nil || string(got) != "untouched" {
			t.Fatal("staging link race wrote outside CAS")
		}
		if _, err = os.Stat(filepath.Join(root, sha)); !os.IsNotExist(err) {
			t.Fatal("raced stream published hash object")
		}
		streamAssertNoTemporary(t, root)
	})
	t.Run("cancel", func(t *testing.T) {
		root := t.TempDir()
		canceled, cancel := context.WithCancel(ctx)
		reader := &streamHookReader{reader: bytes.NewReader(data), hook: cancel}
		if _, err := New(root).PutReader(canceled, reader, sha, int64(len(data)), 100); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, sha)); !os.IsNotExist(err) {
			t.Fatal("cancelled stream published")
		}
		streamAssertNoTemporary(t, root)
	})
}

type streamHookWriter struct {
	hook    func()
	invoked bool
}

func (w *streamHookWriter) Write(p []byte) (int, error) {
	if !w.invoked {
		w.invoked = true
		w.hook()
	}
	return len(p), nil
}
func TestStreamCASReadRejectsNameReplacementAfterOpen(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	data := []byte("pinned read contents")
	sha := digest(data)
	ctx := context.Background()
	if _, err := s.PutReader(ctx, bytes.NewReader(data), sha, int64(len(data)), 100); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	writer := &streamHookWriter{hook: func() {
		if err := os.Remove(filepath.Join(root, sha)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, sha)); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := s.ReadTo(ctx, sha, int64(len(data)), 100, writer); err == nil {
		t.Fatal("CAS name replacement during pinned read accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "external" {
		t.Fatal("raced read touched external target")
	}
}
