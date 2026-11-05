package publisher

import (
	"math"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// After a wall-clock force-flush only video is dropped, so audio piles up in
// an empty segBuf. At the resume IDR that audio must be published as its own
// segment (no audio lost) and the next segment must be anchored at the IDR.
func TestHLSSegmenter_ResumeIDRFlushesAudioOnlyPrefix(t *testing.T) {
	t.Parallel()
	for name, vc := range map[string]domain.AVCodec{
		"h264": domain.AVCodecH264,
		"h265": domain.AVCodecH265,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := &hlsSegmenter{
				streamID:     "test",
				streamDir:    dir,
				manifestPath: dir + "/index.m3u8",
				segSec:       2,
				window:       5,
			}
			p.discardUntilIDR = true

			feed := func(a *domain.AVPacket) {
				if p.shouldDropAVPacket(a) {
					t.Fatalf("packet %d dropped", a.PTSms)
				}
				p.recordMediaTime(a)
				p.maybeFlushAtKeyframe(a, 2*time.Second)
				p.mu.Lock()
				p.segBuf = append(p.segBuf, 0x47, 0x00)
				if p.segStart.IsZero() {
					p.segStart = time.Now()
				}
				p.mu.Unlock()
			}

			// ~6.6 s of audio-only data buffered during the discard window.
			for pts := uint64(1000); pts <= 7600; pts += 100 {
				feed(&domain.AVPacket{Codec: domain.AVCodecAAC, PTSms: pts})
			}
			feed(&domain.AVPacket{Codec: vc, PTSms: 7640, KeyFrame: true})

			if p.segN != 1 || len(p.onDisk) != 1 {
				t.Fatalf("audio-only prefix not published as one segment: segN=%d onDisk=%d", p.segN, len(p.onDisk))
			}
			if d := p.onDisk[0].dur; math.Abs(d-6.6) > 0.15 {
				t.Fatalf("audio segment EXTINF = %.3f, want ~6.6", d)
			}
			if p.segStartMediaPTS != 7640 {
				t.Fatalf("segStartMediaPTS = %d, want 7640 (anchored at the IDR)", p.segStartMediaPTS)
			}
			if len(p.segBuf) != 2 {
				t.Fatalf("segBuf = %d bytes, want only the IDR's data", len(p.segBuf))
			}
			if p.segVideoPkts != 1 {
				t.Fatalf("segVideoPkts = %d, want 1", p.segVideoPkts)
			}
		})
	}
}
