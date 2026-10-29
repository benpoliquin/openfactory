// Package artifacts provides immutable, verified content-addressed storage.
// Publication requires atomic no-replace rename support: renameat2 with
// RENAME_NOREPLACE on Linux or renameatx_np with RENAME_EXCL on Darwin.
// Other platforms and unsupported kernels/filesystems fail without fallback.
package artifacts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"openfactory/internal/model"
)

type Store struct{ root string }

func New(root string) *Store { return &Store{root: root} }
func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// openDir resolves aliases in trusted container ancestors (such as macOS /var),
// but rejects a symbolic link at the requested root itself. After resolution,
// pinned roots and identity checks fence races; bundle members never use alias
// resolution and reject every symbolic-link component.
func openDir(path string, create bool) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if info, e := os.Lstat(absolute); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("root must not be a symbolic link")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	ancestor := filepath.Dir(absolute)
	for {
		_, e := os.Lstat(ancestor)
		if e == nil {
			break
		}
		if !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, e
		}
		ancestor = parent
	}
	physical, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(ancestor, absolute)
	if err != nil {
		return nil, err
	}
	absolute = filepath.Join(physical, relative)
	r, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		info, e := r.Lstat(part)
		if errors.Is(e, os.ErrNotExist) && create {
			if e = r.Mkdir(part, 0700); e != nil && !errors.Is(e, os.ErrExist) {
				r.Close()
				return nil, e
			}
			info, e = r.Lstat(part)
		}
		if e != nil {
			r.Close()
			return nil, e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			r.Close()
			return nil, fmt.Errorf("unsafe directory component %q", part)
		}
		next, e := r.OpenRoot(part)
		if e != nil {
			r.Close()
			return nil, e
		}
		actual, e := next.Stat(".")
		r.Close()
		if e != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, fmt.Errorf("directory changed while opening %q", part)
		}
		r = next
	}
	return r, nil
}
func readFile(ctx context.Context, r *os.Root, name string, max int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if max < 0 {
		return nil, errors.New("negative byte limit")
	}
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("artifact is not a regular file")
	}
	if info.Size() > max {
		return nil, errors.New("artifact exceeds byte limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("artifact exceeds byte limit")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return b, nil
}
func atomicWrite(ctx context.Context, r *os.Root, name string, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	temp := ".tmp-" + hex.EncodeToString(token[:])
	f, err := r.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(temp)
	defer f.Close()
	original, err := f.Stat()
	if err != nil {
		return err
	}
	if !streamRegular(original) {
		return errors.New("CAS staging file must be regular and single-link")
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = PublishStaged(ctx, r, r, temp, name, f, original, int64(len(b))); err != nil {
		return err
	}
	return f.Close()
}
func (s *Store) Put(ctx context.Context, b []byte) (model.BlobRef, error) {
	ref := model.BlobRef{SHA: digest(b), Bytes: int64(len(b))}
	r, err := openDir(s.root, true)
	if err != nil {
		return model.BlobRef{}, err
	}
	defer r.Close()
	if _, err = streamReadTo(ctx, r, ref.SHA, ref.Bytes, io.Discard); err == nil {
		if err = syncDir(r); err != nil {
			return model.BlobRef{}, err
		}
		return ref, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return model.BlobRef{}, err
	}
	if err = atomicWrite(ctx, r, ref.SHA, b); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return model.BlobRef{}, err
		}
		if _, err = streamReadTo(ctx, r, ref.SHA, ref.Bytes, io.Discard); err != nil {
			return model.BlobRef{}, err
		}
	}
	return ref, nil
}
func (s *Store) Read(ctx context.Context, sha string, max int64) ([]byte, error) {
	if !validSHA(sha) {
		return nil, errors.New("invalid blob SHA")
	}
	r, err := openDir(s.root, false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := readFile(ctx, r, sha, max)
	if err != nil {
		return nil, err
	}
	if digest(b) != sha {
		return nil, errors.New("CAS blob SHA mismatch")
	}
	return b, nil
}
func validPath(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\\x00") && !strings.HasPrefix(p, "/") && filepath.ToSlash(filepath.Clean(p)) == p && p != ".." && !strings.HasPrefix(p, "../")
}
func limitsOK(l model.BundleLimits) bool {
	return l.MaxFiles > 0 && l.MaxFileBytes > 0 && l.MaxTotalBytes > 0
}
func canonical(files []model.ProgramBundleFile) ([]byte, error) {
	return json.Marshal(struct {
		Files []model.ProgramBundleFile `json:"files"`
	}{files})
}
func validate(bundle model.ProgramBundle, l model.BundleLimits) ([]byte, error) {
	if !limitsOK(l) {
		return nil, errors.New("bundle limits must be positive")
	}
	if len(bundle.Files) > l.MaxFiles {
		return nil, errors.New("bundle file count exceeded")
	}
	var total int64
	previous := ""
	for _, f := range bundle.Files {
		if !validPath(f.Path) || f.Path <= previous || !validSHA(f.ContentSHA) || f.Bytes < 0 || f.Bytes > l.MaxFileBytes || f.Bytes > l.MaxTotalBytes-total {
			return nil, errors.New("invalid bundle member or exceeded limit")
		}
		previous = f.Path
		total += f.Bytes
	}
	if bundle.Files == nil {
		bundle.Files = []model.ProgramBundleFile{}
	}
	b, e := canonical(bundle.Files)
	if e != nil {
		return nil, e
	}
	if bundle.ManifestSHA != "" && digest(b) != bundle.ManifestSHA {
		return nil, errors.New("bundle manifest SHA mismatch")
	}
	return b, nil
}
func memberRoot(root *os.Root, path string, create bool) (*os.Root, string, error) {
	parts := strings.Split(path, "/")
	r, err := root.OpenRoot(".")
	if err != nil {
		return nil, "", err
	}
	for _, p := range parts[:len(parts)-1] {
		info, e := r.Lstat(p)
		if errors.Is(e, os.ErrNotExist) && create {
			if e = r.Mkdir(p, 0700); e != nil && !errors.Is(e, os.ErrExist) {
				r.Close()
				return nil, "", e
			}
			info, e = r.Lstat(p)
		}
		if e != nil {
			r.Close()
			return nil, "", e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			r.Close()
			return nil, "", errors.New("unsafe bundle directory")
		}
		next, e := r.OpenRoot(p)
		if e != nil {
			r.Close()
			return nil, "", e
		}
		actual, e := next.Stat(".")
		r.Close()
		if e != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, "", errors.New("bundle directory changed")
		}
		r = next
	}
	return r, parts[len(parts)-1], nil
}
func (s *Store) ImportBundle(ctx context.Context, root string, declared []string, l model.BundleLimits) (model.ProgramBundle, error) {
	bundle := model.ProgramBundle{Files: []model.ProgramBundleFile{}}
	if !limitsOK(l) || len(declared) > l.MaxFiles {
		return bundle, errors.New("invalid bundle limits or file count")
	}
	paths := append([]string(nil), declared...)
	sort.Strings(paths)
	r, err := openDir(root, false)
	if err != nil {
		return bundle, err
	}
	defer r.Close()
	var total int64
	for i, p := range paths {
		if !validPath(p) || (i > 0 && p == paths[i-1]) {
			return model.ProgramBundle{}, errors.New("invalid or duplicate declared path")
		}
		dir, name, e := memberRoot(r, p, false)
		if e != nil {
			return model.ProgramBundle{}, e
		}
		max := min(l.MaxFileBytes, l.MaxTotalBytes-total)
		b, e := readFile(ctx, dir, name, max)
		dir.Close()
		if e != nil {
			return model.ProgramBundle{}, e
		}
		ref, e := s.Put(ctx, b)
		if e != nil {
			return model.ProgramBundle{}, e
		}
		total += ref.Bytes
		bundle.Files = append(bundle.Files, model.ProgramBundleFile{Path: p, ContentSHA: ref.SHA, Bytes: ref.Bytes})
	}
	b, err := validate(bundle, l)
	if err != nil {
		return model.ProgramBundle{}, err
	}
	ref, err := s.Put(ctx, b)
	if err != nil {
		return model.ProgramBundle{}, err
	}
	bundle.ManifestSHA = ref.SHA
	return bundle, nil
}
func (s *Store) LoadBundle(ctx context.Context, sha string) (model.ProgramBundle, error) {
	b, err := s.Read(ctx, sha, 4<<20)
	if err != nil {
		return model.ProgramBundle{}, err
	}
	var payload struct {
		Files []model.ProgramBundleFile `json:"files"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&payload); err != nil {
		return model.ProgramBundle{}, err
	}
	bundle := model.ProgramBundle{ManifestSHA: sha, Files: payload.Files}
	if _, err = validate(bundle, model.BundleLimits{MaxFiles: 100000, MaxFileBytes: 1 << 62, MaxTotalBytes: 1 << 62}); err != nil {
		return model.ProgramBundle{}, err
	}
	return bundle, nil
}
func (s *Store) MaterializeBundle(ctx context.Context, bundle model.ProgramBundle, dest string, l model.BundleLimits) error {
	if !validSHA(bundle.ManifestSHA) {
		return errors.New("missing bundle manifest SHA")
	}
	if _, err := validate(bundle, l); err != nil {
		return err
	}
	stored, err := s.LoadBundle(ctx, bundle.ManifestSHA)
	if err != nil {
		return err
	}
	if _, err = validate(stored, l); err != nil {
		return err
	}
	r, err := openDir(dest, true)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range bundle.Files {
		b, e := s.Read(ctx, f.ContentSHA, f.Bytes)
		if e != nil {
			return e
		}
		if int64(len(b)) != f.Bytes {
			return errors.New("bundle member byte length mismatch")
		}
		dir, name, e := memberRoot(r, f.Path, true)
		if e != nil {
			return e
		}
		info, e := dir.Lstat(name)
		if e == nil {
			dir.Close()
			return fmt.Errorf("bundle destination already exists: %s (%s)", f.Path, info.Mode())
		}
		if !errors.Is(e, os.ErrNotExist) {
			dir.Close()
			return e
		}
		e = atomicWrite(ctx, dir, name, b)
		dir.Close()
		if e != nil {
			return e
		}
	}
	return nil
}
