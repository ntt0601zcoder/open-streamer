//go:build unix

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkMode refuses a mode with any group or other bit.
func checkMode(name string, m os.FileMode) error {
	if m.Perm()&0o077 != 0 {
		return fmt.Errorf("config: %s must be mode 0600", name)
	}
	return nil
}

// syncDir fsyncs the directory, making a rename in it durable. A file
// system that cannot fsync a directory at all (EINVAL, ENOTSUP) is not a
// failure: there is nothing more to wait for, and treating it as one would
// leave every later write stale.
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
