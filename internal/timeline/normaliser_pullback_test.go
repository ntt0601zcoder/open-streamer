package timeline

import (
	"fmt"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// TestSingleTrackPullbackPreservesAVSync: after a late burst on one track both
// tracks run ahead of wallclock (the forward re-anchor mirrors onto the
// partner). A later input gap on one track, with natural timestamps, makes that
// track's expected output look more than the jump threshold ahead of wallclock.
// It is still exactly where the recorded V/A relationship puts it relative to
// the partner, so it must NOT be pulled back on its own: that would turn the
// shared lead into a permanent A/V skew.
//
// Covered for every late-track / gap-track combination and for every way the
// V/A relationship is established: lazy seed (no joint seed, or joint seed
// timed out), joint seed with zero, positive and negative source delta, and a
// lazy source_dts_delta seed whose offset exceeds the jump threshold.
//
// At step s (wallclock s ms) the source carries video DTS base+s+vOff and audio
// DTS base+s, so the seeded relationship video_out - audio_out is vOff.
func TestSingleTrackPullbackPreservesAVSync(t *testing.T) {
	const base int64 = 1_000_000
	for _, mode := range []struct {
		name  string
		joint bool
		vOff  int64
	}{
		{"lazy_offset_0", false, 0},
		{"joint_offset_0", true, 0},
		{"joint_offset_+500", true, 500},
		{"joint_offset_-500", true, -500},
		{"lazy_source_delta_+4800", false, 4800},
		{"lazy_source_delta_-4800", false, -4800},
	} {
		for _, tc := range []struct {
			name      string
			lateIsVid bool
			gapIsVid  bool
		}{
			{"late_video_gap_video", true, true},
			{"late_video_gap_audio", true, false},
			{"late_audio_gap_video", false, true},
			{"late_audio_gap_audio", false, false},
		} {
			t.Run(fmt.Sprintf("%s/%s", mode.name, tc.name), func(t *testing.T) {
				cfg := DefaultConfig()
				if mode.joint {
					cfg = jointSeedConfig(1000)
				}
				n := New(cfg)
				at := func(ms int64) time.Time { return startWall.Add(time.Duration(ms) * time.Millisecond) }
				feed := func(isVid bool, s, wall int64) *domain.AVPacket {
					var p *domain.AVPacket
					if isVid {
						d := uint64(base + s + mode.vOff) //nolint:gosec
						p = vPkt(d, d)
					} else {
						d := uint64(base + s) //nolint:gosec
						p = aPkt(d, d)
					}
					n.Apply(p, at(wall))
					return p
				}

				// (1) Seed both at step 0 / wall 0 (the earlier-source track
				// first, so a lazy seed keeps the source delta), then on-time
				// pairs to 1000.
				videoFirst := mode.vOff <= 0
				feed(videoFirst, 0, 0)
				feed(!videoFirst, 0, 0)
				for s := int64(40); s <= 1000; s += 40 {
					feed(true, s, s)
					feed(false, s, s)
				}

				// (2) Late burst: the late track sends nothing until wall 3500
				// while the other keeps arriving on time, then delivers its
				// backlog back to back.
				for s := int64(1040); s <= 3500; s += 40 {
					feed(!tc.lateIsVid, s, s)
				}
				for s := int64(1040); s <= 3540; s += 40 {
					feed(tc.lateIsVid, s, 3500)
				}

				// (3) On-time pairs to step and wall 5000.
				for s := int64(3580); s <= 5000; s += 40 {
					feed(true, s, s)
					feed(false, s, s)
				}

				// (4) The gap track loses steps 5040..7960 (timestamps stay
				// natural) while the other track keeps arriving on time.
				for s := int64(5040); s <= 7960; s += 40 {
					feed(!tc.gapIsVid, s, s)
				}
				gapPkt := feed(tc.gapIsVid, 8000, 8000)
				otherPkt := feed(!tc.gapIsVid, 8000, 8000)

				vPTS, aPTS := int64(gapPkt.PTSms), int64(otherPkt.PTSms) //nolint:gosec
				if !tc.gapIsVid {
					vPTS, aPTS = aPTS, vPTS
				}
				rel := vPTS - aPTS
				if d := rel - mode.vOff; d < -100 || d > 100 {
					t.Fatalf("single-track pull-back broke A/V sync: video_pts=%d audio_pts=%d relationship=%+d ms, want %+d ms",
						vPTS, aPTS, rel, mode.vOff)
				}
			})
		}
	}
}
