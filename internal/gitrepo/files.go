package gitrepo

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The configured volume root is operator-owned. Keep the descriptor rooted at
// that boundary, not at an agent-replaceable attempt directory.
func (m *Manager) sharedRoot(path string) (*os.Root, string, error) {
	root, err := os.OpenRoot(m.cfg.WorkspaceDir)
	if err != nil {
		return nil, "", err
	}
	base, err := filepath.EvalSymlinks(m.cfg.WorkspaceDir)
	if err == nil {
		base, err = filepath.Abs(base)
	}
	var relative string
	if err == nil {
		relative, err = filepath.Rel(base, path)
	}
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		root.Close()
		return nil, "", errors.New("workspace path escapes shared volume")
	}
	return root, relative, nil
}

// copyRootedTree never follows a filesystem race outside either opened root.
// Symlink values are copied as values; reads through escaped links are refused.
func copyRootedTree(ctx context.Context, source *os.Root, sourcePath string, destination *os.Root, destinationPath string, skipGit bool) error {
	var buffer []byte
	return fs.WalkDir(source.FS(), filepath.ToSlash(sourcePath), func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourcePath, name)
		if err != nil {
			return err
		}
		if relative == "." {
			if !entry.IsDir() {
				return errors.New("copy source is not a directory")
			}
			return nil
		}
		if skipGit && entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(destinationPath, relative)
		info, err := source.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return destination.Mkdir(target, 0700)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := source.Readlink(name)
			if err != nil {
				return err
			}
			return destination.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return errors.New("unsupported checkout file type")
		}
		input, err := source.Open(name)
		if err != nil {
			return err
		}
		output, err := destination.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			input.Close()
			return err
		}
		if buffer == nil {
			buffer = make([]byte, 32<<10)
		}
		_, copyErr := io.CopyBuffer(output, contextReader{ctx, input}, buffer)
		inputErr := input.Close()
		outputErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputErr != nil {
			return inputErr
		}
		return outputErr
	})
}

func (m *Manager) copyCheckout(ctx context.Context, source, destination string) error {
	shared, relative, err := m.sharedRoot(destination)
	if err != nil {
		return err
	}
	defer shared.Close()
	if err = shared.Mkdir(relative, 0700); err != nil {
		return err
	}
	private, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer private.Close()
	return copyRootedTree(ctx, private, ".", shared, relative, false)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
