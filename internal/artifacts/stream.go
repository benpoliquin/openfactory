package artifacts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"syscall"

	"openfactory/internal/model"
)

var (
	ErrContentMismatch = errors.New("artifact content does not match declared hash or size")
	ErrSizeLimit       = errors.New("artifact exceeds declared byte limit")
)

type streamContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r streamContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}
func streamRegular(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
func streamBounds(sha string, expectedBytes, max int64) error {
	if !validSHA(sha) {
		return errors.New("invalid blob SHA")
	}
	if expectedBytes < 0 || max < 0 || expectedBytes == 1<<63-1 || expectedBytes > max {
		return ErrSizeLimit
	}
	return nil
}

// ReadTo verifies a pinned, regular, single-link CAS object while streaming it
// into a bounded consumer. A caller must not act on partial bytes before success.
func (s *Store) ReadTo(ctx context.Context, sha string, expectedBytes, max int64, w io.Writer) (model.BlobRef, error) {
	if err := streamBounds(sha, expectedBytes, max); err != nil {
		return model.BlobRef{}, err
	}
	root, err := openDir(s.root, false)
	if err != nil {
		return model.BlobRef{}, err
	}
	defer root.Close()
	return streamReadTo(ctx, root, sha, expectedBytes, w)
}
func streamReadTo(ctx context.Context, root *os.Root, sha string, expectedBytes int64, w io.Writer) (model.BlobRef, error) {
	var out model.BlobRef
	before, err := root.Lstat(sha)
	if err != nil {
		return out, err
	}
	if !streamRegular(before) {
		return out, errors.New("CAS blob must be a regular single-link file")
	}
	file, err := root.OpenFile(sha, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return out, err
	}
	if !streamRegular(opened) || !os.SameFile(before, opened) {
		return out, errors.New("CAS blob identity changed while opening")
	}
	if opened.Size() != expectedBytes {
		return out, ErrContentMismatch
	}
	if w == nil {
		w = io.Discard
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(streamContextReader{ctx: ctx, reader: file}, expectedBytes+1))
	if err != nil {
		return out, err
	}
	if count != expectedBytes || hex.EncodeToString(hash.Sum(nil)) != sha {
		return out, ErrContentMismatch
	}
	after, err := file.Stat()
	if err != nil {
		return out, err
	}
	named, err := root.Lstat(sha)
	if err != nil {
		return out, err
	}
	if !streamRegular(after) || !streamRegular(named) || !os.SameFile(opened, after) || !os.SameFile(after, named) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return out, errors.New("CAS blob changed during read")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	return model.BlobRef{SHA: sha, Bytes: count}, nil
}

// PutReader stages only declaredBytes+1 bytes in the pinned CAS root, verifies
// hash/size before publication, and installs without replacing an existing path.
// It accepts no staging pathname, follows no links, and allocates no file-sized
// buffer. Failed or cancelled input never produces a usable new CAS reference.
func (s *Store) PutReader(ctx context.Context, reader io.Reader, expectedSHA string, expectedBytes, max int64) (model.BlobRef, error) {
	var out model.BlobRef
	if err := streamBounds(expectedSHA, expectedBytes, max); err != nil {
		return out, err
	}
	if reader == nil {
		return out, errors.New("artifact reader is required")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	root, err := openDir(s.root, true)
	if err != nil {
		return out, err
	}
	defer root.Close()
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return out, err
	}
	temp := ".tmp-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return out, err
	}
	defer root.Remove(temp)
	defer file.Close()
	original, err := file.Stat()
	if err != nil {
		return out, err
	}
	if !streamRegular(original) {
		return out, errors.New("CAS staging file must be regular and single-link")
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(streamContextReader{ctx: ctx, reader: reader}, expectedBytes+1))
	if err != nil {
		return out, err
	}
	if count != expectedBytes || hex.EncodeToString(hash.Sum(nil)) != expectedSHA {
		return out, ErrContentMismatch
	}
	if err = PublishStaged(ctx, root, root, temp, expectedSHA, file, original, expectedBytes); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return out, err
		}
		return streamReadTo(ctx, root, expectedSHA, expectedBytes, io.Discard)
	}
	if err = file.Close(); err != nil {
		return out, err
	}
	return model.BlobRef{SHA: expectedSHA, Bytes: count}, nil
}
