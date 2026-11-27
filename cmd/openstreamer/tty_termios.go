//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// getTermios reads the terminal settings of fd.
func getTermios(fd uintptr) (syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return t, errno
	}
	return t, nil
}

// setTermios writes the terminal settings of fd.
func setTermios(fd uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}

// isTerminal reports whether f is a terminal: one whose settings can be
// read.
func isTerminal(f *os.File) bool {
	_, err := getTermios(f.Fd())
	return err == nil
}

// readNoEcho reads one line from the terminal f with echo off, and returns
// it without its line end. Echo is restored on every path: a normal return,
// an error, and a SIGINT or SIGTERM while it waits, which it catches for the
// length of the read and returns as a *signalError.
func readNoEcho(f *os.File) (string, error) {
	fd := f.Fd()
	old, err := getTermios(fd)
	if err != nil {
		return "", fmt.Errorf("read the terminal settings: %w", err)
	}
	// Caught before echo goes off: from then on no signal may end the
	// process with the terminal left silent.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	quiet := old
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ISIG
	if err := setTermios(fd, &quiet); err != nil {
		return "", fmt.Errorf("turn echo off: %w", err)
	}
	defer func() { _ = setTermios(fd, &old) }()

	type result struct {
		line string
		err  error
	}
	// The read cannot be interrupted, so it has its own goroutine; after a
	// signal the process exits, which ends it.
	done := make(chan result, 1)
	go func() {
		line, err := readLine(f)
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case s := <-sig:
		code := 128
		if n, ok := s.(syscall.Signal); ok {
			code += int(n)
		}
		return "", &signalError{code: code}
	}
}

// readLine reads up to the next "\n", byte by byte so that nothing after it
// is taken, and returns the line without its line end.
func readLine(r io.Reader) (string, error) {
	var (
		line []byte
		b    [1]byte
	)
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return trimLineEnd(string(line)), nil
			}
			if line = append(line, b[0]); len(line) > maxPasswordBytes {
				return "", errTooLong
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return trimLineEnd(string(line)), nil
			}
			return "", fmt.Errorf("read the terminal: %w", err)
		}
	}
}
