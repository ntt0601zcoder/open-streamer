//go:build !linux

package transcoder

import "errors"

// readProcessStats always fails off Linux, which ends the sampler: per-process
// CPU and memory metrics are a Linux-only feature.
func readProcessStats(int) (processStats, error) {
	return processStats{}, errors.New("process stats are only available on Linux")
}
