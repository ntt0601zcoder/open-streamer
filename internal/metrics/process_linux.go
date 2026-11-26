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
	cpu, rssPages, ok := parseStat(string(raw))
	if !ok {
		return
	}
	w.Gauge("process_resident_memory_bytes", "Resident memory size in bytes.", rssPages*float64(os.Getpagesize()))
	w.Counter("process_cpu_seconds_total", "Total user and system CPU time in seconds.", cpu)
}

// parseStat reads the text of /proc/self/stat: the user and system CPU time
// in seconds, and the resident set in pages. ok is false when the text does
// not have those fields.
func parseStat(s string) (cpu, rssPages float64, ok bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	// After the command name the fields start at number 3 (state).
	f := strings.Fields(s[i+1:])
	if len(f) < 22 {
		return 0, 0, false
	}
	utime, e1 := strconv.ParseFloat(f[11], 64)
	stime, e2 := strconv.ParseFloat(f[12], 64)
	rss, e3 := strconv.ParseFloat(f[21], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, false
	}
	return (utime + stime) / clockTicks, rss, true
}
