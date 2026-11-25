package metrics

import (
	"runtime"
	rm "runtime/metrics"
	"time"
)

// Process returns the collector of the go_* and process_* families.
func Process(start time.Time) func(*Writer) {
	return func(w *Writer) {
		w.Gauge("go_goroutines", "Number of goroutines.", float64(runtime.NumGoroutine()))
		samples := []rm.Sample{
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/memory/classes/heap/unused:bytes"},
			{Name: "/memory/classes/total:bytes"},
			{Name: "/gc/cycles/total:gc-cycles"},
		}
		rm.Read(samples)
		heap, unused := u64(samples[0]), u64(samples[1])
		w.Gauge("go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", heap)
		w.Gauge("go_memstats_heap_inuse_bytes", "Bytes in in-use heap spans.", heap+unused)
		w.Gauge("go_memstats_sys_bytes", "Bytes obtained from the system.", u64(samples[2]))
		w.Counter("go_gc_cycles_total", "Completed garbage collection cycles.", u64(samples[3]))
		w.Gauge("process_start_time_seconds", "Start time of the process since the Unix epoch in seconds.",
			float64(start.UnixNano())/1e9)
		platform(w)
	}
}

func u64(s rm.Sample) float64 {
	if s.Value.Kind() != rm.KindUint64 {
		return 0
	}
	return float64(s.Value.Uint64())
}
