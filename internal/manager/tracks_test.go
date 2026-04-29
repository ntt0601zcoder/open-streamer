package manager

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// TestInputTrackStats_BitrateEWMA verifies that bytes accumulated over the
// trackBitrateWindow flush into a bitrate value, and that subsequent windows
// blend (EWMA) rather than fully replace.
func TestInputTrackStats_BitrateEWMA(t *testing.T) {
	st := newInputTrackStats()
	now := time.Unix(0, 0)

	// 100 KB over 4s ≈ 200 kbps. One observation per ms wouldn't trip the
	// flush — a single observation just past the window does.
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: make([]byte, 100_000)}, now)
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: make([]byte, 0)}, now.Add(4*time.Second))

	got := st.totalBitrateKbps()
	if got < 150 || got > 250 {
		t.Fatalf("expected ~200 kbps, got %d", got)
	}

	// Second window same volume — EWMA stays in-range.
	base := now.Add(4 * time.Second)
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: make([]byte, 100_000)}, base)
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: make([]byte, 0)}, base.Add(4*time.Second))
	got = st.totalBitrateKbps()
	if got < 150 || got > 250 {
		t.Fatalf("expected ~200 kbps after second window, got %d", got)
	}
}

// TestInputTrackStats_PerCodecBuckets ensures distinct codecs accumulate into
// their own counters and snapshot returns video-then-audio order.
func TestInputTrackStats_PerCodecBuckets(t *testing.T) {
	st := newInputTrackStats()
	now := time.Unix(0, 0)

	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: make([]byte, 50_000)}, now)
	st.observe(&domain.AVPacket{Codec: domain.AVCodecAAC, Data: make([]byte, 20_000)}, now)
	// Flush window
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: nil}, now.Add(4*time.Second))
	st.observe(&domain.AVPacket{Codec: domain.AVCodecAAC, Data: nil}, now.Add(4*time.Second))

	tracks := st.snapshot()
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d (%+v)", len(tracks), tracks)
	}
	if tracks[0].Kind != domain.MediaTrackVideo || tracks[0].Codec != "h264" {
		t.Fatalf("expected video first, got %+v", tracks[0])
	}
	if tracks[1].Kind != domain.MediaTrackAudio || tracks[1].Codec != "aac" {
		t.Fatalf("expected audio second, got %+v", tracks[1])
	}
}

// TestInputTrackStats_NilAndUnknown verifies hot-path safety on garbage input.
func TestInputTrackStats_NilAndUnknown(t *testing.T) {
	st := newInputTrackStats()
	st.observe(nil, time.Now())
	st.observe(&domain.AVPacket{Codec: domain.AVCodecUnknown}, time.Now())
	if got := st.snapshot(); got != nil {
		t.Fatalf("expected no tracks, got %+v", got)
	}
}

// TestInputTrackStats_Reset verifies switch-time housekeeping clears stats.
func TestInputTrackStats_Reset(t *testing.T) {
	st := newInputTrackStats()
	now := time.Unix(0, 0)
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: make([]byte, 50_000)}, now)
	st.observe(&domain.AVPacket{Codec: domain.AVCodecH264, Data: nil}, now.Add(4*time.Second))
	if st.totalBitrateKbps() == 0 {
		t.Fatal("expected non-zero bitrate before reset")
	}
	st.reset()
	if got := st.totalBitrateKbps(); got != 0 {
		t.Fatalf("expected 0 kbps after reset, got %d", got)
	}
	if got := st.snapshot(); got != nil {
		t.Fatalf("expected no tracks after reset, got %+v", got)
	}
}
