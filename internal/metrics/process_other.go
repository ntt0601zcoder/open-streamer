//go:build !linux

package metrics

// platform has nothing to add: the fd, memory and CPU families are Linux only.
func platform(*Writer) {}
