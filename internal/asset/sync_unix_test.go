//go:build unix

package asset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// syncDir reports a directory it cannot open, and syncs one it can.
func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syncDir(dir))
	require.ErrorIs(t, syncDir(filepath.Join(dir, "gone")), os.ErrNotExist)
}
