//go:build unix

package asset

import (
	"errors"
	"os"
	"syscall"
)

// syncDir fsyncs the directory, making a rename or a removal in it durable.
// A file system that cannot fsync a directory at all (EINVAL, ENOTSUP) is
// not a failure: there is nothing more to wait for. This is a copy of
// syncDir in internal/config/sync_unix.go; change both together.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read only: a close error loses nothing
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
