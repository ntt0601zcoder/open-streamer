//go:build !unix

package main

import "os"

// notifyHUP returns nil: this system has no SIGHUP. POST /api/v1/reload
// reloads the config.
func notifyHUP() <-chan os.Signal { return nil }
