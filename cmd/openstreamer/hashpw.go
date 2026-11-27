package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
)

// maxPasswordBytes is the longest password hash-password reads.
const maxPasswordBytes = 1024

// errTooLong is a password over maxPasswordBytes.
var errTooLong = fmt.Errorf("the password is longer than %d bytes", maxPasswordBytes)

// signalError is a SIGINT or SIGTERM that arrived while a password was
// read from the terminal: the exit code is 128 plus the signal's number.
type signalError struct{ code int }

func (e *signalError) Error() string { return "interrupted" }

// hashPassword is the hash-password subcommand: it reads a password from
// stdin and prints its hash. The password never comes from the command
// line, where every user of the machine could read it, so any argument is
// refused, and not echoed back: it may be one.
func hashPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = io.WriteString(stderr, "openstreamer: hash-password takes no argument: the password is read from stdin\n")
		return 2
	}
	password, err := readPassword(stdin, stderr)
	if err == nil {
		var hash string
		if hash, err = auth.HashPassword(password); err == nil {
			_, _ = fmt.Fprintln(stdout, hash)
			return 0
		}
	}
	var sig *signalError
	if errors.As(err, &sig) {
		return sig.code
	}
	// None of these errors holds the password.
	_, _ = fmt.Fprintf(stderr, "openstreamer: hash-password: %v\n", err)
	return 2
}

// readPassword reads the password: from a terminal, twice with echo off;
// from anything else, the whole input up to maxPasswordBytes, less one
// trailing line end.
func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		return promptPassword(f, stderr)
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxPasswordBytes+3))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	password := trimLineEnd(string(b))
	if len(password) > maxPasswordBytes {
		return "", errTooLong
	}
	return password, nil
}

// promptPassword asks for the password twice on the terminal f.
func promptPassword(f *os.File, stderr io.Writer) (string, error) {
	var lines [2]string
	for i, prompt := range []string{"Password: ", "Again: "} {
		_, _ = io.WriteString(stderr, prompt)
		line, err := readNoEcho(f)
		// The line end the user typed was not echoed.
		_, _ = io.WriteString(stderr, "\n")
		if err != nil {
			return "", err
		}
		lines[i] = line
	}
	if lines[0] != lines[1] {
		return "", errors.New("the two passwords differ")
	}
	return lines[0], nil
}

// trimLineEnd removes one trailing "\n" or "\r\n".
func trimLineEnd(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}
