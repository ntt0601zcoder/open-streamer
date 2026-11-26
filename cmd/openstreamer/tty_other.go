//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

// isTerminal reports whether f is a character device, which is what a
// terminal is; this system's terminal settings are not read.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// readNoEcho refuses: a password typed here would be shown.
func readNoEcho(*os.File) (string, error) {
	return "", errors.New("echo cannot be turned off on this system; pipe the password on stdin")
}
