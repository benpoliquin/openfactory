//go:build !linux && !darwin

package artifacts

import (
	"errors"
	"os"
)

func renameNoReplace(_, _ *os.File, oldName, newName string) error {
	return &os.LinkError{Op: "atomic no-replace publication (requires Linux or Darwin)", Old: oldName, New: newName, Err: errors.ErrUnsupported}
}
