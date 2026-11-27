//go:build unix

package asset

import (
	"errors"
	"syscall"
	"testing"
)

// mkfifo makes a FIFO at path. Its cleanup opens it read-write, which never
// blocks, so that an open stuck on it (a failed test) is released.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(fd)
		}
	})
}

// assertNoReader checks that nothing holds the FIFO at path open for
// reading: a writer that does not wait for a reader is refused with ENXIO.
func assertNoReader(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		_ = syscall.Close(fd)
		t.Fatal("the FIFO is still open for reading")
	}
	if !errors.Is(err, syscall.ENXIO) {
		t.Fatalf("open for writing: %v, want ENXIO", err)
	}
}
