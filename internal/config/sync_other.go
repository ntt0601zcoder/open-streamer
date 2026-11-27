//go:build !unix

package config

import "os"

// checkMode accepts every mode: outside Unix the permission bits do not
// say who may read the file.
func checkMode(string, os.FileMode) error { return nil }

// syncDir does nothing: outside Unix a directory cannot be fsynced, and a
// rename is made durable by the file system.
func syncDir(string) error { return nil }
