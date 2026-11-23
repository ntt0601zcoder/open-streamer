//go:build !unix

package testpeer

import "os/exec"

// ownProcessGroup leaves cmd as it is: process groups are a unix notion.
func ownProcessGroup(*exec.Cmd) {}
