package main

import "syscall"

// The termios ioctls of macOS; isTerminal and readNoEcho are in
// tty_termios.go.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
