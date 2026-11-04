package publisher

import (
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// TestHLSSegmenter_NoCutOfAudioOnlyBuffer covers the state left behind by a
// force-flush: video is dropped until the next IDR while audio keeps filling
// the empty buffer. When that IDR arrives after segDur of audio, the
// audio-only buffer must not be cut as a segment of its own (it would have
// no IDR/SPS); the segment has to be extended to start with the IDR's GOP.
func TestHLSSegmenter_NoCutOfAudioOnlyBuffer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: dir + "/index.m3u8",
		segSec:       2,
		window:       5,
	}
	segDur := 2 * time.Second

	// Audio-only prefix spanning more than segDur (video is being discarded).
	for pts := uint64(20000); pts <= 22400; pts += 200 {
		p.recordMediaTime(&domain.AVPacket{Codec: domain.AVCodecAAC, PTSms: pts})
		p.segBuf = append(p.segBuf, 0x47, 0x00)
	}

	idr := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 22500, KeyFrame: true}
	p.recordMediaTime(idr)
	p.maybeFlushAtKeyframe(idr, segDur)
	if p.segN != 0 {
		t.Fatalf("audio-only buffer was flushed as a segment; segN = %d, want 0", p.segN)
	}

	// Once the buffer holds video, the next IDR past segDur cuts normally.
	p.recordMediaTime(&domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 22540})
	idr2 := &domain.AVPacket{Codec: domain.AVCodecH264, PTSms: 24600, KeyFrame: true}
	p.recordMediaTime(idr2)
	p.maybeFlushAtKeyframe(idr2, segDur)
	if p.segN != 1 {
		t.Fatalf("segment with video should cut at next IDR; segN = %d, want 1", p.segN)
	}
}
