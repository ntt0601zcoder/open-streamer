//go:build linux

package transcoder

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestReadProcessStats_ReadsARealProcess guards the /proc parsing: a wrong
// field or unit shows up as zero memory or zero threads for a live process.
func TestReadProcessStats_ReadsARealProcess(t *testing.T) {
	t.Parallel()
	st, err := readProcessStats(os.Getpid())
	if err != nil {
		t.Fatalf("readProcessStats: %v", err)
	}
	if st.residentBytes < 1<<20 {
		t.Errorf("residentBytes = %d, want at least 1 MiB for a Go test binary", st.residentBytes)
	}
	if st.threads < 1 {
		t.Errorf("threads = %d, want at least 1", st.threads)
	}
	if st.cpuSeconds < 0 {
		t.Errorf("cpuSeconds = %v, want non-negative", st.cpuSeconds)
	}
}

// TestSupervisor_StartProcessSampler_PublishesThenStops covers the wiring the
// supervisor relies on: samples appear for a real pid, and the done channel
// closes after cancel. runOnce waits on that channel, so a missing close would
// hang every stream stop.
func TestSupervisor_StartProcessSampler_PublishesThenStops(t *testing.T) {
	t.Parallel()
	m := newProcessMetrics()
	sv := &supervisor{svc: &Service{m: m}, streamID: "s1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := sv.startProcessSampler(ctx, os.Getpid())

	deadline := time.Now().Add(2 * time.Second)
	for gaugeValue(t, m.TranscoderProcessThreads.WithLabelValues("s1")) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no sample published within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not stop after cancel")
	}
}

func TestSupervisor_StartProcessSampler_WithoutMetricsIsDoneImmediately(t *testing.T) {
	t.Parallel()
	sv := &supervisor{svc: &Service{}, streamID: "s1"}
	select {
	case <-sv.startProcessSampler(context.Background(), os.Getpid()):
	case <-time.After(time.Second):
		t.Fatal("done channel not closed; runOnce would block forever")
	}
}

func TestReadProcessStats_FailsForAMissingProcess(t *testing.T) {
	t.Parallel()
	if _, err := readProcessStats(-1); err == nil {
		t.Fatal("readProcessStats(-1) returned no error")
	}
}
