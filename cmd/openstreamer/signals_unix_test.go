//go:build unix

package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperEnv selects what TestSignalHelper does in the child process.
const helperEnv = "OPENSTREAMER_SIGNAL_HELPER"

// TestSignalHelper is not a test: it is the child process of the tests
// below, which run this test binary again with helperEnv set.
func TestSignalHelper(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "":
		t.Skip("the child process of the signal tests")
	case "hang":
		// A server whose orderly stop hangs.
		ctx, stop := signals()
		defer stop()
		_, _ = os.Stderr.WriteString("ready\n")
		<-ctx.Done()
		_, _ = os.Stderr.WriteString("stopping\n")
		select {}
	case "hash-password":
		// The command itself, reading a password from a pipe nobody
		// writes to.
		os.Args = []string{"openstreamer", "hash-password"}
		_, _ = os.Stderr.WriteString("ready\n")
		main()
	}
}

// helper starts the child in mode, with a stdin that stays open, and
// returns it once it has printed "ready".
func helper(t *testing.T, mode string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelper$") //nolint:gosec,noctx // this test binary
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
	})
	lines := bufio.NewScanner(stderr)
	require.True(t, lines.Scan(), "the child printed nothing")
	require.Equal(t, "ready", lines.Text())
	return cmd, lines
}

// killedBy waits for cmd and requires that sig ended it, sending sig every
// 20 ms meanwhile when again is set.
func killedBy(t *testing.T, cmd *exec.Cmd, sig syscall.Signal, again bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(waitFor)
	for {
		select {
		case err := <-done:
			var exit *exec.ExitError
			require.True(t, errors.As(err, &exit), "the child ended with %v", err)
			ws, ok := exit.Sys().(syscall.WaitStatus)
			require.True(t, ok)
			assert.True(t, ws.Signaled(), "the child exited by itself: %v", err)
			assert.Equal(t, sig, ws.Signal())
			return
		case <-tick.C:
			if again {
				_ = cmd.Process.Signal(sig)
			}
		case <-deadline:
			require.Fail(t, "the signal did not end the child")
		}
	}
}

// The first SIGINT or SIGTERM starts the stop; a second one ends a process
// whose stop hangs.
func TestSecondSignalKills(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, lines := helper(t, "hang")
			require.NoError(t, cmd.Process.Signal(sig))
			require.True(t, lines.Scan(), "the first signal ended the child")
			require.Equal(t, "stopping", lines.Text(), "the first signal starts the orderly stop")
			// The handler is removed a moment after the context ends: keep
			// sending until one gets through.
			killedBy(t, cmd, sig, true)
		})
	}
}

// hash-password reading from a pipe is ended by one SIGINT: nothing
// swallows it.
func TestHashPasswordPipeInterrupt(t *testing.T) {
	cmd, _ := helper(t, "hash-password")
	// Let main reach the read: were the signal caught as the server's is,
	// it would do nothing and the child would live on.
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, cmd.Process.Signal(syscall.SIGINT))
	killedBy(t, cmd, syscall.SIGINT, false)
}
