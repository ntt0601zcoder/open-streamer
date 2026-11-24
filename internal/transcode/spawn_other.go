//go:build !unix

package transcode

import (
	"errors"
	"fmt"
	"io"
)

// spawnSupported reports whether this platform can run the child.
const spawnSupported = false

// startProc returns errors.ErrUnsupported: the child runs on Unix only.
func startProc(string, []string, []string, io.Writer) (*proc, error) {
	return nil, fmt.Errorf("transcode: spawn: %w", errors.ErrUnsupported)
}

// killGroup kills the child process.
func (p *proc) killGroup() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
