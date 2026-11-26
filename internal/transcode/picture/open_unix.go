//go:build unix

package picture

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// OpenRegular opens path for reading if it is a regular file and not a
// symbolic link. On Unix it opens with O_NOFOLLOW|O_NONBLOCK (so a FIFO does
// not block and a link is not followed), then checks the descriptor with
// Stat: there is no window between a check and the open. Elsewhere it is
// os.Open and the same Stat check. LoadAsset (images, M4c) now opens through
// it too.
func OpenRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, &notRegularError{path: path, what: symlink, err: err}
		}
		return nil, fmt.Errorf("picture: open: %w", err)
	}
	return checkRegular(f)
}
