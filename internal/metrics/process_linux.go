package metrics

import (
	"os"
	"strconv"
	"strings"
)

const clockTicks = 100

func platform(w *Writer) {
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		w.Gauge("process_open_fds", "Number of open file descriptors.", float64(len(ents)))
	}
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return
	}
	// After the command name the fields start at number 3 (state).
	f := strings.Fields(s[i+1:])
	if len(f) < 22 {
		return
	}
	utime, e1 := strconv.ParseFloat(f[11], 64)
	stime, e2 := strconv.ParseFloat(f[12], 64)
	rss, e3 := strconv.ParseFloat(f[21], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return
	}
	w.Gauge("process_resident_memory_bytes", "Resident memory size in bytes.", rss*float64(os.Getpagesize()))
	w.Counter("process_cpu_seconds_total", "Total user and system CPU time in seconds.", (utime+stime)/clockTicks)
}
