package artifacts

import (
	"context"
	"errors"
	"os"
	"strings"
)

// openRootDirectory obtains the syscall directory descriptor from the pinned
// root itself, never from a pathname that could have been renamed or replaced.
func openRootDirectory(root *os.Root) (*os.File, os.FileInfo, error) {
	before, err := root.Lstat(".")
	if err != nil {
		return nil, nil, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	opened, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, nil, err
	}
	if !before.IsDir() || !opened.IsDir() || !os.SameFile(before, opened) {
		dir.Close()
		return nil, nil, errors.New("publication root identity changed while opening directory")
	}
	return dir, opened, nil
}

func syncDir(root *os.Root) error {
	dir, _, err := openRootDirectory(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func publicationName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// PublishStaged is the only file publication convention for CAS, bundle, and
// workspace writers. It atomically moves a regular, single-link staging file
// between pinned roots without replacing any destination, including a symlink.
// Both names must be direct children of their roots. The roots must be on the
// same filesystem; unsupported kernels/filesystems fail without any fallback.
// Unlike link+unlink, no instant or crash boundary has two names for the inode.
// File data and both affected directory entries are synced before success.
// The caller owns the open file and removes the temporary name on every exit.
// An existing destination returns os.ErrExist for caller-side winner validation.
func PublishStaged(ctx context.Context, sourceRoot, targetRoot *os.Root, temp, name string, file *os.File, original os.FileInfo, size int64) error {
	if !publicationName(temp) || !publicationName(name) {
		return errors.New("publication requires direct-child names")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourceDir, sourceInfo, err := openRootDirectory(sourceRoot)
	if err != nil {
		return err
	}
	defer sourceDir.Close()
	targetDir, targetInfo := sourceDir, sourceInfo
	if sourceRoot != targetRoot {
		targetDir, targetInfo, err = openRootDirectory(targetRoot)
		if err != nil {
			return err
		}
		defer targetDir.Close()
	}
	sameDirectory := os.SameFile(sourceInfo, targetInfo)
	if sameDirectory && temp == name {
		return errors.New("publication requires distinct source and destination")
	}
	if err = file.Sync(); err != nil {
		return err
	}
	staged, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := sourceRoot.Lstat(temp)
	if err != nil {
		return err
	}
	if !streamRegular(original) || !streamRegular(staged) || !streamRegular(named) || !os.SameFile(original, staged) || !os.SameFile(staged, named) || staged.Size() != size {
		return errors.New("publication staging identity changed")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = renameNoReplace(sourceDir, targetDir, temp, name); err != nil {
		// An existing concurrent winner may not yet have synced its directory.
		// Make that entry durable before the caller verifies and adopts it.
		if errors.Is(err, os.ErrExist) {
			if syncErr := targetDir.Sync(); syncErr != nil {
				return syncErr
			}
		}
		return err
	}
	installed, err := targetRoot.Lstat(name)
	if err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if !streamRegular(installed) || !streamRegular(after) || !os.SameFile(staged, installed) || !os.SameFile(installed, after) || after.Size() != size || !after.ModTime().Equal(staged.ModTime()) {
		return errors.New("publication identity changed")
	}
	if err = targetDir.Sync(); err != nil {
		return err
	}
	if !sameDirectory {
		if err = sourceDir.Sync(); err != nil {
			return err
		}
	}
	return ctx.Err()
}
