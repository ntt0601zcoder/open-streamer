package ingest

import "github.com/ntt0601zcoder/open-streamer/internal/media"

// aacSamples is the number of samples in one AAC frame.
const aacSamples = 1024

// audioGrid places AAC frames on their sample grid. RTMP stamps audio in
// whole milliseconds, so an AAC frame (21.33 ms at 48 kHz) carries up to
// 1 ms of rounding plus the encoder's jitter; the grid restores the exact
// sample timing. Frame k after the anchor is presented at
//
//	PTS_k = anchor + floor(k · 1024 · 90000 / rate)
//
// With d one frame and dev the RTMP time minus the grid time:
//
//   - |dev| <= d: the frame stays on the grid;
//   - dev > d: re-anchor forward at the RTMP time (a gap in the source);
//   - −1 s < dev < −d: the frame is ahead of the grid and dropped;
//   - |dev| >= 1 s: re-anchor at the RTMP time (an encoder reset; the Clock
//     handles the jump).
//
// A dropped frame does not advance k, so a source whose clock runs slower
// than its sample clock loses one frame per d of drift, and one that runs
// faster gains a gap of about d instead; between corrections the output
// never steps backward. It is not safe for concurrent use.
type audioGrid struct {
	started bool
	anchor  int64 // 90 kHz time of frame 0
	k       int64 // frames placed since the anchor
	rate    int   // sample rate of the grid
}

// place returns the PTS of the frame stamped rtmpPTS (90 kHz) at sample rate
// rate. keep is false when the frame is dropped; reanchored reports a new
// anchor taken from rtmpPTS. The first frame anchors without reporting a
// re-anchor. A rate change continues from the end of the old grid.
func (g *audioGrid) place(rtmpPTS int64, rate int) (pts int64, keep bool, reanchored bool) {
	if !g.started {
		g.started = true
		g.anchor, g.k, g.rate = rtmpPTS, 1, rate
		return rtmpPTS, true, false
	}
	if rate != g.rate {
		g.anchor += g.offset(g.k)
		g.k, g.rate = 0, rate
	}
	grid := g.anchor + g.offset(g.k)
	dev := rtmpPTS - grid
	// d = 1024 · 90000 / rate, compared exactly: |dev| · rate against
	// 1024 · 90000.
	frame := int64(aacSamples * media.ClockRate)
	scaled := dev * int64(g.rate)
	switch {
	case dev >= media.ClockRate || dev <= -media.ClockRate, scaled > frame:
		g.anchor, g.k = rtmpPTS, 1
		return rtmpPTS, true, true
	case scaled < -frame:
		return grid, false, false
	default:
		g.k++
		return grid, true, false
	}
}

// offset is the grid time of frame k after the anchor.
func (g *audioGrid) offset(k int64) int64 {
	return k * aacSamples * media.ClockRate / int64(g.rate)
}
