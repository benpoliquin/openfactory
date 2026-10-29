package executor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"openfactory/internal/model"
)

// journal is private checker state, never mounted in agent workspaces. A held
// advisory lock prevents two servers from executing one durable intent.
type journal struct {
	root string
	lock *os.File
}

func openJournal(path, workspaceRoot string) (*journal, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("experiment journal must be absolute")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("experiment journal must be a private directory (0700)")
	}
	private, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	shared, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if within(shared, private) || within(private, shared) {
		return nil, errors.New("experiment journal must be separate from shared workspace root")
	}
	lock, err := os.OpenFile(filepath.Join(private, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("experiment journal already in use")
	}
	return &journal{root: private, lock: lock}, nil
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (j *journal) close() {
	if j != nil && j.lock != nil {
		_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
		_ = j.lock.Close()
	}
}
func (j *journal) save(id string, value any) error {
	if !validSHA(id) {
		return errors.New("invalid experiment run identity")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(j.root, ".intent-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temp.Name(), filepath.Join(j.root, id+".json")); err != nil {
		return err
	}
	dir, err := os.Open(j.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func readJournal(path string, value any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return errors.New("invalid experiment journal record")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("experiment journal record exceeds limit")
	}
	return model.StrictDecode(data, value)
}
func validSHA(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
