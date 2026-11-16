package h26x

import "container/heap"

// maxReorder bounds the reorder depth, as ReorderOf does.
const maxReorder = 16

// defaultFrameDuration is the start-window spacing in 90 kHz units when the
// SPS carries no timing information. Any positive value keeps the DTS valid.
const defaultFrameDuration = 3000

// minBreak is the smallest backward PTS jump, in 90 kHz units, that is a
// timeline break rather than a late frame.
const minBreak = 90000

// DTSStatus is the outcome of DTSExtractor.Extract.
type DTSStatus uint8

// Extract outcomes.
const (
	// DTSOK: the returned DTS belongs to the frame.
	DTSOK DTSStatus = iota
	// DTSDrop: the frame has no valid DTS and must be dropped.
	DTSDrop
	// DTSBreak: the PTS jumped backwards past the reorder window. The
	// extractor has reset itself without consuming the frame.
	DTSBreak
)

// DTSStats counts what a DTSExtractor has dropped or adjusted.
type DTSStats struct {
	Leading       uint64 // dropped before the first keyframe, or below its PTS
	Late          uint64 // dropped because the PTS was at or below the last DTS
	Breaks        uint64 // DTSBreak results
	ReorderRaised uint64 // times a late frame raised the reorder depth
}

// DTSExtractor derives decode timestamps from presentation timestamps of a
// video track in decode order, for sources that carry only the PTS (RTP).
//
// With R frames of reorder, a frame is decoded at most R frames before it
// is shown, so the DTS of the k-th decoded frame is the PTS of the (k−R)-th
// displayed frame. The extractor keeps the pending PTS in a min-heap across
// GOPs and, once it holds more than R entries, pops the smallest as the DTS.
// The first R frames after the start or Reset get synthetic DTS spaced one
// frame duration apart below the smallest pending PTS. The emitted DTS are
// strictly increasing and never above the frame's PTS.
//
// A DTSExtractor is not safe for concurrent use.
type DTSExtractor struct {
	reorder  int   // R
	pending  int   // a lower R to apply at the next Reset; -1 when none
	frameDur int64 // from the SPS; 0 when unknown

	pts     ptsHeap
	started bool  // the first keyframe since the start or Reset was seen
	window  bool  // still in the start window (nothing popped yet)
	firstPT int64 // PTS of that keyframe
	dur     int64 // frame duration fixed at the window's first frame
	lastDTS int64
	hasLast bool
	raised  bool // R was raised in this late episode; cleared by the next DTS

	stats DTSStats
}

// NewDTSExtractor returns an extractor for reorder depth reorder (from
// ReorderOf, clamped to [0, 16]) and frameDuration in 90 kHz units (from
// SPS.FrameDuration; 0 when unknown).
func NewDTSExtractor(reorder int, frameDuration int64) *DTSExtractor {
	return &DTSExtractor{
		reorder:  clampReorder(reorder),
		pending:  -1,
		frameDur: frameDuration,
	}
}

func clampReorder(r int) int {
	return min(max(r, 0), maxReorder)
}

// Extract returns the DTS for the next access unit in decode order, given its
// PTS and whether it is a keyframe (IDR, CRA, BLA or recovery point).
//
// DTSDrop: drop the frame. It comes before the first keyframe since the start
// or Reset, has a PTS below that keyframe's (a leading picture), or has a PTS
// at or below the last DTS (a late frame: R was underestimated, so R is
// raised by one, at most once until the next DTS is emitted).
//
// DTSBreak: the PTS is more than max(90000, (R+2)·frame duration) below the
// last DTS. The extractor has called Reset on itself (R is kept unless
// SetReorder left a lower value pending) and has not consumed the frame: the
// caller emits its discontinuity and calls Extract again with the same frame,
// which is then handled as the first frame after Reset.
func (d *DTSExtractor) Extract(pts int64, key bool) (dts int64, st DTSStatus) {
	if d.hasLast && pts < d.lastDTS-max(minBreak, int64(d.reorder+2)*d.dur) {
		d.stats.Breaks++
		d.Reset()
		return 0, DTSBreak
	}
	if !d.started {
		if !key {
			d.stats.Leading++
			return 0, DTSDrop
		}
		d.started, d.window, d.firstPT = true, true, pts
		d.dur = d.frameDur
		if d.dur <= 0 {
			d.dur = defaultFrameDuration
		}
	}
	if pts < d.firstPT {
		d.stats.Leading++
		return 0, DTSDrop
	}
	if d.hasLast && pts <= d.lastDTS {
		d.stats.Late++
		if !d.raised && d.reorder < maxReorder {
			d.reorder++
			d.raised = true
			d.stats.ReorderRaised++
		}
		return 0, DTSDrop
	}

	heap.Push(&d.pts, pts)
	switch {
	case d.pts.Len() > d.reorder:
		dts = heap.Pop(&d.pts).(int64)
		d.window = false
	case d.window:
		k := d.pts.Len() - 1
		dts = d.pts[0] - int64(d.reorder-k)*d.dur
	default:
		// R was raised after the start window: the heap refills first.
		dts = d.lastDTS + 1
	}
	if d.hasLast {
		// Pending PTS are above the last DTS when pushed, but a DTS of
		// lastDTS+1 can reach a duplicate of one; this keeps the DTS
		// strictly increasing and still at or below this frame's PTS.
		dts = max(dts, d.lastDTS+1)
	}
	d.lastDTS, d.hasLast, d.raised = dts, true, false
	return dts, DTSOK
}

// Reset starts a new timeline (new SSRC, timestamp jump): frames are dropped
// until the next keyframe, and the old timeline no longer bounds the DTS. A
// lower R from SetReorder takes effect here.
func (d *DTSExtractor) Reset() {
	if d.pending >= 0 {
		d.reorder, d.pending = d.pending, -1
	}
	d.pts = d.pts[:0]
	d.started, d.window, d.hasLast, d.raised = false, false, false, false
	d.firstPT, d.dur, d.lastDTS = 0, 0, 0
}

// SetReorder sets R from a new SPS, clamped to [0, 16]. A higher R applies at
// once; a lower R applies at the next Reset, since lowering it mid-timeline
// would emit pending PTS out of order.
func (d *DTSExtractor) SetReorder(r int) {
	r = clampReorder(r)
	switch {
	case r > d.reorder:
		d.reorder, d.pending = r, -1
	case r < d.reorder:
		d.pending = r
	default:
		d.pending = -1
	}
}

// Stats returns the counters.
func (d *DTSExtractor) Stats() DTSStats { return d.stats }

// ptsHeap is a min-heap of PTS for container/heap.
type ptsHeap []int64

func (h ptsHeap) Len() int           { return len(h) }
func (h ptsHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h ptsHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

// Push implements heap.Interface.
func (h *ptsHeap) Push(x any) { *h = append(*h, x.(int64)) }

// Pop implements heap.Interface.
func (h *ptsHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
