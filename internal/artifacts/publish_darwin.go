package artifacts

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func renameNoReplace(sourceDir, targetDir *os.File, oldName, newName string) error {
	err := unix.RenameatxNp(int(sourceDir.Fd()), oldName, int(targetDir.Fd()), newName, unix.RENAME_EXCL)
	runtime.KeepAlive(sourceDir)
	runtime.KeepAlive(targetDir)
	if err != nil {
		return &os.LinkError{Op: "renameatx_np", Old: oldName, New: newName, Err: err}
	}
	return nil
}
