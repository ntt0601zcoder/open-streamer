package main

import "syscall"

// The termios ioctls of Linux; isTerminal and readNoEcho are in
// tty_termios.go.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)
