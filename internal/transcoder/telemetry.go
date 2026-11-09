package transcoder

import (
	"context"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
)

// processSampleInterval is how often a subprocess's CPU and memory are read.
// Shorter than a typical Prometheus scrape so every scrape sees a fresh value.
const processSampleInterval = 5 * time.Second

// processStats is one reading of a subprocess's resource usage. cpuSeconds is
// cumulative since the process started.
type processStats struct {
	cpuSeconds    float64
	residentBytes int
	threads       int
}

// processSampler publishes the resource usage of one subprocess lifetime. The
// transcoder does its work in libav (C), so only the OS can report its real
// CPU and memory; none of it appears in the server's own process metrics.
type processSampler struct {
	m       *metrics.Metrics
	stream  string
	lastCPU float64
}

func newProcessSampler(m *metrics.Metrics, stream string) *processSampler {
	return &processSampler{m: m, stream: stream}
}

// observe applies one reading. Only CPU growth is added, so the counter keeps
// accumulating across subprocess respawns.
func (s *processSampler) observe(st processStats) {
	if d := st.cpuSeconds - s.lastCPU; d > 0 {
		s.m.TranscoderProcessCPUSeconds.WithLabelValues(s.stream).Add(d)
		s.lastCPU = st.cpuSeconds
	}
	s.m.TranscoderProcessResidentMemory.WithLabelValues(s.stream).Set(float64(st.residentBytes))
	s.m.TranscoderProcessThreads.WithLabelValues(s.stream).Set(float64(st.threads))
}

// run samples until ctx ends or the process can no longer be read, then drops
// the gauges so a dead subprocess doesn't keep reporting its last values.
func (s *processSampler) run(ctx context.Context, interval time.Duration, read func() (processStats, error)) {
	defer func() {
		s.m.TranscoderProcessResidentMemory.DeleteLabelValues(s.stream)
		s.m.TranscoderProcessThreads.DeleteLabelValues(s.stream)
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		st, err := read()
		if err != nil {
			return
		}
		s.observe(st)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startProcessSampler samples pid in the background. The returned channel
// closes once sampling has stopped, so a respawned subprocess can't have its
// fresh gauges deleted by the previous sampler's cleanup.
func (sv *supervisor) startProcessSampler(ctx context.Context, pid int) <-chan struct{} {
	done := make(chan struct{})
	if sv.svc.m == nil {
		close(done)
		return done
	}
	sampler := newProcessSampler(sv.svc.m, string(sv.streamID))
	go func() {
		defer close(done)
		sampler.run(ctx, processSampleInterval, func() (processStats, error) {
			return readProcessStats(pid)
		})
	}()
	return done
}

// transcoderErrorStages maps text from the subprocess's error chain to a stage
// label, keeping the metric's cardinality bounded. Order matters: an input
// switch or init failure can also mention the decoder or encoder it failed on.
var transcoderErrorStages = []struct{ needle, stage string }{
	{"pipeline init", "init"},
	{"switch input", "switch"},
	{"flush", "flush"},
	{"ts feed", "demux"},
	{"audio", "audio"},
	{"watermark", "watermark"},
	{" scale", "scale"},
	{"encode", "encode"},
	{"decode", "decode"},
}

// classifyTranscoderError returns the pipeline stage an error came from.
func classifyTranscoderError(msg string) string {
	for _, s := range transcoderErrorStages {
		if strings.Contains(msg, s.needle) {
			return s.stage
		}
	}
	return "other"
}
