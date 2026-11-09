//go:build linux

package transcoder

import (
	"fmt"

	"github.com/prometheus/procfs"
)

// readProcessStats reads a process's resource usage from /proc.
func readProcessStats(pid int) (processStats, error) {
	proc, err := procfs.NewProc(pid)
	if err != nil {
		return processStats{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	stat, err := proc.Stat()
	if err != nil {
		return processStats{}, fmt.Errorf("read process %d stat: %w", pid, err)
	}
	return processStats{
		cpuSeconds:    stat.CPUTime(),
		residentBytes: stat.ResidentMemory(),
		threads:       stat.NumThreads,
	}, nil
}
