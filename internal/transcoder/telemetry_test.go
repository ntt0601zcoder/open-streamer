package transcoder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	pb "github.com/ntt0601zcoder/open-streamer/internal/transcoder/native/proto"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func newProcessMetrics() *metrics.Metrics {
	labels := []string{"stream_code"}
	return &metrics.Metrics{
		TranscoderProcessCPUSeconds:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cpu_test"}, labels),
		TranscoderProcessResidentMemory: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "rss_test"}, labels),
		TranscoderProcessThreads:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "threads_test"}, labels),
	}
}

// TestProcessSampler_ReportsUsageFromSuccessiveSamples: a cumulative CPU figure
// must become counter growth (never added whole twice, never decreasing) while
// RSS and threads follow the latest sample.
func TestProcessSampler_ReportsUsageFromSuccessiveSamples(t *testing.T) {
	t.Parallel()
	m := newProcessMetrics()
	s := newProcessSampler(m, "s1")

	s.observe(processStats{cpuSeconds: 1.5, residentBytes: 100, threads: 4})
	s.observe(processStats{cpuSeconds: 2.0, residentBytes: 200, threads: 5})
	s.observe(processStats{cpuSeconds: 1.0, residentBytes: 300, threads: 6})

	if got := counterValue(t, m.TranscoderProcessCPUSeconds.WithLabelValues("s1")); got != 2.0 {
		t.Errorf("cpu seconds = %v, want 2", got)
	}
	if got := gaugeValue(t, m.TranscoderProcessResidentMemory.WithLabelValues("s1")); got != 300 {
		t.Errorf("rss = %v, want 300", got)
	}
	if got := gaugeValue(t, m.TranscoderProcessThreads.WithLabelValues("s1")); got != 6 {
		t.Errorf("threads = %v, want 6", got)
	}
}

func seriesCount(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	n := 0
	for _, f := range families {
		n += len(f.GetMetric())
	}
	return n
}

// TestProcessSampler_RunClearsGaugesWhenStopped: without the cleanup a dead
// subprocess keeps reporting its last RSS forever. CPU stays because counters
// must survive a respawn.
func TestProcessSampler_RunClearsGaugesWhenStopped(t *testing.T) {
	t.Parallel()
	m := newProcessMetrics()
	s := newProcessSampler(m, "s1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.run(ctx, time.Millisecond, func() (processStats, error) {
		cancel()
		return processStats{cpuSeconds: 1, residentBytes: 100, threads: 2}, nil
	})

	if n := seriesCount(t, m.TranscoderProcessResidentMemory); n != 0 {
		t.Errorf("rss series after stop = %d, want 0", n)
	}
	if n := seriesCount(t, m.TranscoderProcessThreads); n != 0 {
		t.Errorf("thread series after stop = %d, want 0", n)
	}
	if n := seriesCount(t, m.TranscoderProcessCPUSeconds); n != 1 {
		t.Errorf("cpu series after stop = %d, want 1", n)
	}
}

// TestProcessSampler_RunStopsWhenTheProcessCannotBeRead: the sampler must end
// by itself when the process is gone or the platform has no /proc, rather than
// spinning for the rest of the subprocess's life. The context is never
// cancelled here, so a retry loop hangs the test.
func TestProcessSampler_RunStopsWhenTheProcessCannotBeRead(t *testing.T) {
	t.Parallel()
	s := newProcessSampler(newProcessMetrics(), "s1")
	calls := 0

	s.run(context.Background(), time.Millisecond, func() (processStats, error) {
		calls++
		return processStats{}, errors.New("no such process")
	})

	if calls != 1 {
		t.Errorf("read calls = %d, want 1", calls)
	}
}

func TestClassifyTranscoderError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ msg, want string }{
		{"pipeline init: pipeline: gpu rendition 0: encoder: open failed", "init"},
		{"process packet: pipeline: decode: native: SendPacket: Invalid data", "decode"},
		{"process packet: pipeline: rendition 1 scale: bad frame", "scale"},
		{"process packet: pipeline: rendition 0 encode: native: SendFrame: x", "encode"},
		{"process packet: pipeline: rendition 2 watermark: x", "watermark"},
		{"process packet: pipeline: audio reencode: x", "audio"},
		{"process packet: pipeline: ts feed: closed", "demux"},
		{"switch input: pipeline: alloc new decoder on switch: x", "switch"},
		{"flush: pipeline: flush decoder: x", "flush"},
		{"something unexpected", "other"},
	} {
		if got := classifyTranscoderError(tc.msg); got != tc.want {
			t.Errorf("classifyTranscoderError(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

// TestSupervisor_HandleSubprocessError_CountsByStage: decode and encode errors
// are terminal, so they can only be counted on the supervisor side.
func TestSupervisor_HandleSubprocessError_CountsByStage(t *testing.T) {
	t.Parallel()
	errs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "errors_test"}, []string{"stream_code", "stage"})
	sv := &supervisor{svc: &Service{m: &metrics.Metrics{TranscoderErrorsTotal: errs}}, streamID: "s1"}

	if err := sv.handleSubprocessError(&pb.Error{Message: "pipeline: decode: x", Terminal: true}); err == nil {
		t.Fatal("terminal error must still tear the run down")
	}
	if got := counterValue(t, errs.WithLabelValues("s1", "decode")); got != 1 {
		t.Errorf("errors{stage=decode} = %v, want 1", got)
	}
}

func newHealthMetrics() *metrics.Metrics {
	return &metrics.Metrics{
		TranscoderDecodedFramesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "decoded_test"},
			[]string{"stream_code"}),
		TranscoderEncoderInputFramesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "input_test"},
			[]string{"stream_code", "profile"}),
		TranscoderEncodeSecondsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "encode_test"},
			[]string{"stream_code", "profile"}),
		TranscoderDroppedFramesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dropped_test"},
			[]string{"stream_code", "kind", "reason"}),
		TranscoderQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "queue_test"},
			[]string{"stream_code", "queue"}),
	}
}

// TestHealthTracker_TurnsCumulativeReportsIntoCounterGrowth: reports carry
// running totals, so adding each report whole would inflate every counter.
func TestHealthTracker_TurnsCumulativeReportsIntoCounterGrowth(t *testing.T) {
	t.Parallel()
	m := newHealthMetrics()
	tr := newHealthTracker(m, "s1")

	tr.observe(&pb.Health{
		FramesDecoded: 100, DroppedVideoPreKeyframe: 5, DroppedAudioPreKeyframe: 3, DroppedAudioSwitch: 2,
		PendingAudioFrames: 7, DemuxQueueChunks: 4,
		Profiles: []*pb.Health_Profile{
			{Index: 0, FramesIn: 100, EncodeNanos: 1_000_000_000},
			{Index: 1, FramesIn: 90, EncodeNanos: 500_000_000},
		},
	})
	tr.observe(&pb.Health{
		FramesDecoded: 150, DroppedVideoPreKeyframe: 5, DroppedAudioPreKeyframe: 4, DroppedAudioSwitch: 2,
		PendingAudioFrames: 1,
		Profiles: []*pb.Health_Profile{
			{Index: 0, FramesIn: 150, EncodeNanos: 1_500_000_000},
			{Index: 1, FramesIn: 140, EncodeNanos: 750_000_000},
		},
	})

	for _, tc := range []struct {
		name string
		got  prometheus.Counter
		want float64
	}{
		{"decoded", m.TranscoderDecodedFramesTotal.WithLabelValues("s1"), 150},
		{"input track_1", m.TranscoderEncoderInputFramesTotal.WithLabelValues("s1", "track_1"), 150},
		{"input track_2", m.TranscoderEncoderInputFramesTotal.WithLabelValues("s1", "track_2"), 140},
		{"encode track_1", m.TranscoderEncodeSecondsTotal.WithLabelValues("s1", "track_1"), 1.5},
		{"encode track_2", m.TranscoderEncodeSecondsTotal.WithLabelValues("s1", "track_2"), 0.75},
		{"video pre_keyframe", m.TranscoderDroppedFramesTotal.WithLabelValues("s1", "video", "pre_keyframe"), 5},
		{"audio pre_keyframe", m.TranscoderDroppedFramesTotal.WithLabelValues("s1", "audio", "pre_keyframe"), 4},
		{"audio input_switch", m.TranscoderDroppedFramesTotal.WithLabelValues("s1", "audio", "input_switch"), 2},
	} {
		if v := counterValue(t, tc.got); v != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, v, tc.want)
		}
	}
	if v := gaugeValue(t, m.TranscoderQueueDepth.WithLabelValues("s1", "pending_audio")); v != 1 {
		t.Errorf("pending_audio = %v, want 1", v)
	}
	if v := gaugeValue(t, m.TranscoderQueueDepth.WithLabelValues("s1", "demux")); v != 0 {
		t.Errorf("demux = %v, want 0", v)
	}
}

// TestHealthTracker_KeepsCountingAcrossRespawn: a new subprocess restarts its
// totals at zero, and the Prometheus counters must keep rising across that.
func TestHealthTracker_KeepsCountingAcrossRespawn(t *testing.T) {
	t.Parallel()
	m := newHealthMetrics()
	newHealthTracker(m, "s1").observe(&pb.Health{FramesDecoded: 100})
	newHealthTracker(m, "s1").observe(&pb.Health{FramesDecoded: 10})

	if v := counterValue(t, m.TranscoderDecodedFramesTotal.WithLabelValues("s1")); v != 110 {
		t.Errorf("decoded = %v, want 110", v)
	}
}

func TestHealthTracker_ClearRemovesQueueDepth(t *testing.T) {
	t.Parallel()
	m := newHealthMetrics()
	tr := newHealthTracker(m, "s1")
	tr.observe(&pb.Health{PendingAudioFrames: 3, DemuxQueueChunks: 2})

	tr.clear()

	if n := seriesCount(t, m.TranscoderQueueDepth); n != 0 {
		t.Errorf("queue series after clear = %d, want 0", n)
	}
}

// TestSupervisor_DispatchEvent_RoutesHealthToTracker covers the path from the
// gRPC event to the metrics.
func TestSupervisor_DispatchEvent_RoutesHealthToTracker(t *testing.T) {
	t.Parallel()
	m := newHealthMetrics()
	sv := &supervisor{svc: &Service{m: m}, streamID: "s1"}
	tr := newHealthTracker(m, "s1")

	err := sv.dispatchEvent(&pb.Event{Body: &pb.Event_Health{Health: &pb.Health{FramesDecoded: 7}}}, tr)
	if err != nil {
		t.Fatalf("dispatchEvent: %v", err)
	}

	if v := counterValue(t, m.TranscoderDecodedFramesTotal.WithLabelValues("s1")); v != 7 {
		t.Errorf("decoded = %v, want 7", v)
	}
}

func TestSupervisor_DispatchEvent_HealthWithoutMetricsIsIgnored(t *testing.T) {
	t.Parallel()
	sv := &supervisor{svc: &Service{}, streamID: "s1"}
	var tr *healthTracker
	if err := sv.dispatchEvent(&pb.Event{Body: &pb.Event_Health{Health: &pb.Health{FramesDecoded: 7}}}, tr); err != nil {
		t.Fatalf("dispatchEvent: %v", err)
	}
}

func newFramesVec() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "frames_test"},
		[]string{"stream_code", "profile", "kind"})
}

// TestSupervisor_WriteOutputPacket_CountsFramesPerRendition locks in that
// frames are counted where the transcoder hands them over, so the metric no
// longer depends on any output protocol being enabled. No publisher exists in
// this test.
func TestSupervisor_WriteOutputPacket_CountsFramesPerRendition(t *testing.T) {
	t.Parallel()
	buf := buffer.NewServiceForTesting(8)
	const stream domain.StreamCode = "test_frames"
	targets := []RenditionTarget{{BufferID: stream + "/t0"}, {BufferID: stream + "/t1"}}
	for _, tg := range targets {
		buf.Create(tg.BufferID)
	}
	frames := newFramesVec()
	sv := &supervisor{
		svc:      &Service{buf: buf, m: &metrics.Metrics{TranscoderFramesTotal: frames}},
		streamID: stream,
		targets:  targets,
	}

	write := func(idx int32, codec pb.Codec) {
		t.Helper()
		if err := sv.writeOutputPacket(&pb.OutputPacket{TargetIndex: idx, Data: []byte{1}, Codec: codec}); err != nil {
			t.Fatalf("writeOutputPacket(%d): %v", idx, err)
		}
	}
	write(0, pb.Codec_CODEC_H264)
	write(1, pb.Codec_CODEC_H265)
	write(1, pb.Codec_CODEC_H265)
	write(-1, pb.Codec_CODEC_AAC) // broadcast reaches both buffers but is one encoded frame
	write(7, pb.Codec_CODEC_H264) // unknown target is dropped, so it was not delivered
	write(0, pb.Codec_CODEC_UNSPECIFIED)

	for _, tc := range []struct {
		profile, kind string
		want          float64
	}{
		{"track_1", "video", 1},
		{"track_2", "video", 2},
		{"all", "audio", 1},
	} {
		got := counterValue(t, frames.WithLabelValues(string(stream), tc.profile, tc.kind))
		if got != tc.want {
			t.Errorf("frames{profile=%q,kind=%q} = %v, want %v", tc.profile, tc.kind, got, tc.want)
		}
	}
}
