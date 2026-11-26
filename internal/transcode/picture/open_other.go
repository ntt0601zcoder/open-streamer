//go:build !unix

package picture

import (
	"fmt"
	"os"
)

// OpenRegular opens path for reading if it is a regular file and not a
// symbolic link. On Unix it opens with O_NOFOLLOW|O_NONBLOCK (so a FIFO does
// not block and a link is not followed), then checks the descriptor with
// Stat: there is no window between a check and the open. Elsewhere it is
// os.Open and the same Stat check, after an Lstat that refuses a symbolic
// link (os.Open would follow it; here a link replaced between the two calls
// is not seen). LoadAsset (images, M4c) now opens through it too.
func OpenRegular(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("picture: open: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, &notRegularError{path: path, what: symlink}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("picture: open: %w", err)
	}
	return checkRegular(f)
}
