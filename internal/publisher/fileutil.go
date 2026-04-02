package publisher

import (
	"os"
	"path/filepath"
)

// windowTail returns the last n elements of vals.
// If len(vals) <= n, the whole slice is returned unchanged.
func windowTail(vals []string, n int) []string {
	if n <= 0 || len(vals) == 0 {
		return vals
	}
	if len(vals) <= n {
		return vals
	}
	return vals[len(vals)-n:]
}

// writeFileAtomic writes data to path atomically by writing to a temp file in the
// same directory and renaming, so readers never see a partial write.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// resetOutputDir removes dir entirely then recreates it, giving each publisher run
// a clean slate so stale segments from a previous run are not served.
func resetOutputDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}
