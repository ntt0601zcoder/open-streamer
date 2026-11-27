package metrics_test

import (
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
)

func processSamples(t *testing.T, start time.Time) []metricstest.Sample {
	t.Helper()
	var r metrics.Registry
	r.Register(metrics.Process(start))
	s, err := metricstest.Parse(scrape(t, &r, http.MethodGet).Body)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProcess(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	s := processSamples(t, start)
	if v, ok := metricstest.Find(s, "go_goroutines"); !ok || v < 1 {
		t.Errorf("go_goroutines %v %v", v, ok)
	}
	if v, ok := metricstest.Find(s, "process_start_time_seconds"); !ok || v != float64(start.UnixNano())/1e9 {
		t.Errorf("start %v %v", v, ok)
	}
	for _, n := range []string{"go_memstats_heap_alloc_bytes", "go_memstats_heap_inuse_bytes", "go_memstats_sys_bytes", "go_gc_cycles_total"} {
		if v, ok := metricstest.Find(s, n); !ok || v < 0 {
			t.Errorf("%s %v %v", n, v, ok)
		}
	}
	if runtime.GOOS != "linux" {
		for _, n := range []string{"process_open_fds", "process_resident_memory_bytes", "process_cpu_seconds_total"} {
			if _, ok := metricstest.Find(s, n); ok {
				t.Errorf("%s present", n)
			}
		}
		return
	}
	fds, ok := metricstest.Find(s, "process_open_fds")
	if !ok || fds < 3 {
		t.Fatalf("fds %v %v", fds, ok)
	}
	if v, ok := metricstest.Find(s, "process_resident_memory_bytes"); !ok || v <= 0 {
		t.Errorf("rss %v %v", v, ok)
	}
	if v, ok := metricstest.Find(s, "process_cpu_seconds_total"); !ok || v < 0 {
		t.Errorf("cpu %v %v", v, ok)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if v, _ := metricstest.Find(processSamples(t, start), "process_open_fds"); v != fds+1 {
		t.Errorf("fds after open %v, before %v", v, fds)
	}
}
