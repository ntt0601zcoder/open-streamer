//go:build !libav

package main

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStubExits2 builds the package without -tags libav and checks that the
// binary refuses to run: exit status 2 and one line on stderr.
func TestStubExits2(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "ostranscoder")
	build := exec.CommandContext(t.Context(), gobin, "build", "-o", bin, ".") //nolint:gosec // the test's own package
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build: %s", out)

	for _, args := range [][]string{{"-version"}, {"-probe", "font", "x"}, nil} {
		cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // built above
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		var exit *exec.ExitError
		require.True(t, errors.As(err, &exit), "args %q: want an exit error, got %v", args, err)
		assert.Equal(t, 2, exit.ExitCode(), "args %q", args)
		assert.Equal(t, "ostranscoder: built without -tags libav\n", stderr.String(), "args %q", args)
		assert.Empty(t, stdout.String(), "args %q", args)
	}
}
