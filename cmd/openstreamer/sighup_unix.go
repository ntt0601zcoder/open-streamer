//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyHUP returns the channel SIGHUP arrives on. Its buffer is exactly
// one: a signal that arrives while a reload runs stays pending, and further
// ones coalesce into it. signal.Stop is never called on it: between that and
// the exit, a SIGHUP would kill a process that is shutting down cleanly.
func notifyHUP() <-chan os.Signal {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	return hup
}
