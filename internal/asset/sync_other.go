//go:build !unix

package asset

// syncDir does nothing: outside Unix a directory cannot be fsynced, and a
// rename is made durable by the file system. This is a copy of syncDir in
// internal/config/sync_other.go; change both together.
func syncDir(string) error { return nil }
