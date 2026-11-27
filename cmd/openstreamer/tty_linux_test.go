package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
)

// openPTY opens a pty pair through /dev/ptmx: the master, which plays the
// keyboard, and the slave, which is the command's terminal.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	require.Zero(t, errno)
	var n uint32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n)))
	require.Zero(t, errno)
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// echo reports whether the terminal f echoes.
func echo(t *testing.T, f *os.File) bool {
	t.Helper()
	tio, err := getTermios(f.Fd())
	require.NoError(t, err)
	return tio.Lflag&syscall.ECHO != 0
}

// ttyRun is hash-password running on a pty.
type ttyRun struct {
	t              *testing.T
	master, slave  *os.File
	stdout, stderr *lockedBuffer
	echoed         *lockedBuffer // what the terminal sent back to the keyboard's side
	done           chan int
}

func startTTY(t *testing.T) *ttyRun {
	t.Helper()
	r := &ttyRun{t: t, stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, echoed: &lockedBuffer{}, done: make(chan int, 1)}
	r.master, r.slave = openPTY(t)
	require.True(t, isTerminal(r.slave))
	require.True(t, echo(t, r.slave))
	// The master is read until the slave's last descriptor is closed. A
	// line end first lets go a read the command left behind on the slave
	// after a signal, which holds that descriptor.
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = io.Copy(r.echoed, r.master)
	}()
	t.Cleanup(func() {
		_, _ = r.master.WriteString("\n")
		_ = r.slave.Close()
		select {
		case <-read:
		case <-time.After(waitFor):
			assert.Fail(t, "the pty reader did not end")
		}
	})
	go func() { r.done <- hashPassword(nil, r.slave, r.stdout, r.stderr) }()
	return r
}

// typeLine waits for the prompt, checks that echo is off while the command
// waits, and types the line.
func (r *ttyRun) typeLine(prompt, line string) {
	r.t.Helper()
	r.waitPrompt(prompt)
	_, err := r.master.WriteString(line + "\n")
	require.NoError(r.t, err)
}

func (r *ttyRun) waitPrompt(prompt string) {
	r.t.Helper()
	require.Eventually(r.t, func() bool {
		return strings.HasSuffix(r.stderr.String(), prompt) && !echo(r.t, r.slave)
	}, waitFor, time.Millisecond, "no %q with echo off; stderr %q", prompt, r.stderr)
}

func (r *ttyRun) exit() int {
	r.t.Helper()
	select {
	case code := <-r.done:
		return code
	case <-time.After(waitFor):
		require.Fail(r.t, "hash-password did not return")
		return -1
	}
}

func TestHashPasswordTTY(t *testing.T) {
	t.Run("two equal lines", func(t *testing.T) {
		r := startTTY(t)
		r.typeLine("Password: ", "correct horse battery")
		r.typeLine("Again: ", "correct horse battery")
		require.Equal(t, 0, r.exit(), r.stderr.String())
		assert.True(t, echo(t, r.slave), "echo is on again")
		h, err := auth.ParseHash(strings.TrimSuffix(r.stdout.String(), "\n"), auth.MinIterations)
		require.NoError(t, err)
		assert.True(t, h.Verify("correct horse battery"))
		assert.NotContains(t, r.stderr.String(), "correct horse")
		// Nothing typed was echoed to the keyboard's side.
		time.Sleep(20 * time.Millisecond)
		assert.NotContains(t, r.echoed.String(), "correct")
	})

	t.Run("two different lines", func(t *testing.T) {
		r := startTTY(t)
		r.typeLine("Password: ", "correct horse battery")
		r.typeLine("Again: ", "correct horse bakery")
		require.Equal(t, 2, r.exit())
		assert.True(t, echo(t, r.slave), "echo is on again")
		assert.Empty(t, r.stdout.String())
		assert.Contains(t, r.stderr.String(), "differ")
		assert.NotContains(t, r.stderr.String(), "correct horse")
	})

	t.Run("a short password", func(t *testing.T) {
		r := startTTY(t)
		r.typeLine("Password: ", "short")
		r.typeLine("Again: ", "short")
		require.Equal(t, 2, r.exit())
		assert.True(t, echo(t, r.slave))
		assert.Empty(t, r.stdout.String())
	})

	for sig, code := range map[syscall.Signal]int{syscall.SIGINT: 130, syscall.SIGTERM: 143} {
		t.Run(sig.String(), func(t *testing.T) {
			r := startTTY(t)
			// Echo is off only after the handler is in place, so the
			// signal, sent to the test process, is the command's to catch.
			r.waitPrompt("Password: ")
			require.NoError(t, syscall.Kill(os.Getpid(), sig))
			require.Equal(t, code, r.exit())
			assert.True(t, echo(t, r.slave), "echo is on again")
			assert.Empty(t, r.stdout.String())
		})
	}
}

func TestReadLine(t *testing.T) {
	for in, want := range map[string]string{
		"a line\nand more": "a line",
		"a line\r\n":       "a line",
		"no line end":      "no line end",
		"\n":               "",
	} {
		got, err := readLine(strings.NewReader(in))
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
	// Nothing after the line end is taken.
	r := strings.NewReader("first\nsecond\n")
	_, err := readLine(r)
	require.NoError(t, err)
	rest, err := readLine(r)
	require.NoError(t, err)
	assert.Equal(t, "second", rest)

	_, err = readLine(strings.NewReader(""))
	require.Error(t, err)
	_, err = readLine(strings.NewReader(strings.Repeat("x", maxPasswordBytes+1) + "\n"))
	require.ErrorIs(t, err, errTooLong)
	// A file that is no terminal has no settings to change.
	f, err := os.Open(os.DevNull)
	require.NoError(t, err)
	defer f.Close()
	assert.False(t, isTerminal(f))
	_, err = readNoEcho(f)
	require.Error(t, err)
}
