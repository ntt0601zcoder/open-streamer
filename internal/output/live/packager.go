// Package live packages one stream for HLS (TS and fMP4) and DASH.
//
// A Packager subscribes to the stream's hub, cuts segments with one
// segment.Segmenter, muxes each closed segment once as TS and once per track
// as CMAF, and keeps the last Window segments. Handler serves the segment
// and init files; the playlists and the MPD are built from a Snapshot of the
// window on request.
//
// # Epoch walls
//
// When an epoch's first segment is packaged, the packager places the epoch
// on the wallclock once. AST is the wall of the first anchor ever packaged.
// The hub's anchor wall is already the epoch's effective wall (the Clock
// keeps epochs from overlapping without letting the lead accumulate; see
// clock.Epoch), so the epoch's start relative to AST is
//
//	start_n = max(ceil_ms(anchor_n.Wall − AST), start_{n−1} + 1 ms)
//
// and its effective wall is W_n = AST + start_n. Clock walls are whole
// milliseconds, so W_n equals the anchor wall and a pipeline pacing on the
// anchor releases frames exactly at the wall the outputs advertise. Every
// wallclock value the outputs derive (HLS program date-time, DASH period
// start, segment availability) goes through W_n, never a raw arrival time.
//
// An epoch without a hub anchor (see FallbackAnchors) is placed from its
// arrival A = Now − the first segment's span instead, by the Clock's rule
// with R = A: start_n = ceil_ms(max(A − AST, min(start_{n−1} + Δ, A − AST +
// media.MaxAnchorLead))), Δ being the stream time between the two epochs'
// anchors.
package live

import (
	"bytes"
	"context"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/segment"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

const (
	defaultTarget = 2 * media.ClockRate
	defaultWindow = 6
)

// Options configures a Packager.
type Options struct {
	Target int64            // segment target in 90 kHz ticks; 0 means 180000 (2 s)
	Window int              // segments kept; 0 means 6
	Now    func() time.Time // nil means time.Now
}

// epochState is the writer's working copy of one epoch.
type epochState struct {
	info   EpochInfo
	tracks map[int]*EpochTrack
}

// publish returns an immutable copy of the epoch for the store.
func (e *epochState) publish() EpochInfo {
	info := e.info
	info.Tracks = make([]EpochTrack, 0, len(e.tracks)) // full capacity: appends copy
	for _, t := range e.tracks {
		info.Tracks = append(info.Tracks, *t)
	}
	sort.Slice(info.Tracks, func(i, j int) bool { return info.Tracks[i].Track < info.Tracks[j].Track })
	return info
}

// Packager packages one stream. Handle, Flush and Run serialise with each
// other; Snapshot and the HTTP handler may run concurrently with them.
type Packager struct {
	h        *hub.Hub
	target   int64
	window   int
	now      func() time.Time
	errors   atomic.Uint64
	fallback atomic.Uint64
	codecErr atomic.Uint64

	// Writer state, guarded by work.
	work    sync.Mutex
	seg     *segment.Segmenter
	tsm     *ts.Muxer
	epochs  map[uint32]*epochState // epochs with a segment in the window, plus the newest
	prev    *epochState            // the last epoch placed on the wallclock
	ast     time.Time              // valid once prev is set
	nextN   map[int]uint64         // per track: N of its next CMAF part
	pending map[int]bool           // per track: a discontinuity since its last part
	tsSeq   uint64                 // TSSeq of the next segment with TS bytes
	tsGap   bool                   // a TS segment was dropped since the last good one

	// TARGETDURATION per playlist; it never shrinks. Guarded by tdMu.
	tdMu sync.Mutex
	td   map[string]int

	// Shared state. The writer changes it only while holding both work and
	// mu, so it may read it holding work alone.
	mu sync.Mutex
	st store
}

// NewPackager returns a packager reading epoch anchors from h.
func NewPackager(h *hub.Hub, opt Options) *Packager {
	p := &Packager{
		h:       h,
		target:  opt.Target,
		window:  opt.Window,
		now:     opt.Now,
		epochs:  map[uint32]*epochState{},
		nextN:   map[int]uint64{},
		pending: map[int]bool{},
	}
	if p.target <= 0 {
		p.target = defaultTarget
	}
	if p.window <= 0 {
		p.window = defaultWindow
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.seg = segment.New(segment.Options{Target: p.target})
	return p
}

// Errors returns the number of mux errors so far. Each one dropped one
// segment's output in one format.
func (p *Packager) Errors() uint64 { return p.errors.Load() }

// FallbackAnchors returns the number of epochs placed without a hub anchor.
func (p *Packager) FallbackAnchors() uint64 { return p.fallback.Load() }

// CodecErrors returns the number of epoch tracks whose codec string could
// not be built, counted once each when the track's first part is packaged.
// The DASH MPD leaves such a track out, and the HLS master's CODECS lacks it.
func (p *Packager) CodecErrors() uint64 { return p.codecErr.Load() }

// Handle feeds one delivery and packages the segments it completes. It
// returns when they are in the window.
func (p *Packager) Handle(d hub.Delivery) {
	p.work.Lock()
	defer p.work.Unlock()
	for _, s := range p.seg.Push(d) {
		p.pack(s)
	}
}

// Flush packages the open segment, if any.
func (p *Packager) Flush() {
	p.work.Lock()
	defer p.work.Unlock()
	for _, s := range p.seg.Flush() {
		p.pack(s)
	}
}

// End packages the open segment, if any, and marks the stream ended: no
// segment follows, and once every segment is listed the HLS playlists carry
// EXT-X-ENDLIST. Handle must not be called after End.
func (p *Packager) End() {
	p.work.Lock()
	defer p.work.Unlock()
	for _, s := range p.seg.Flush() {
		p.pack(s)
	}
	p.mu.Lock()
	p.st.ended = true
	p.mu.Unlock()
}

// Run hands every delivery of sub to Handle. It returns when ctx is done, or
// after End when sub's channel closes (the hub closed). It does not close
// sub.
func (p *Packager) Run(ctx context.Context, sub *hub.Sub) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-sub.C():
			if !ok {
				p.End()
				return
			}
			p.Handle(d)
		}
	}
}

// Snapshot returns a copy of the window; see Snapshot for the contract.
func (p *Packager) Snapshot() Snapshot {
	now := p.now()
	p.mu.Lock()
	s := p.st.snapshot()
	p.mu.Unlock()
	s.Target, s.Window, s.Now = p.target, p.window, now
	for s.Listed < len(s.Segments) && s.IsListed(s.Segments[s.Listed]) {
		s.Listed++
	}
	return s
}

// pack muxes one closed segment and adds it to the window. Called with work
// held.
func (p *Packager) pack(s segment.Segment) {
	ep := p.epochs[s.Epoch]
	if ep == nil {
		ep = p.placeEpoch(s)
		p.epochs[s.Epoch] = ep
	}
	info := SegmentInfo{
		Seq:           s.Seq,
		Epoch:         s.Epoch,
		Discontinuity: s.Discontinuity,
		Forced:        s.Forced,
		Start:         s.Start,
		Duration:      s.Duration,
		End:           s.End,
		TS:            p.muxTS(s.Frames),
	}
	if info.TS == nil {
		p.tsGap = true
	} else {
		// The first segment with TS bytes has nothing before it to be
		// discontinuous with, like the segmenter's first segment.
		info.TSSeq = p.tsSeq
		info.TSDiscontinuity = p.tsSeq > 0 && (s.Discontinuity || p.tsGap)
		p.tsSeq++
		p.tsGap = false
	}
	if s.Discontinuity {
		for track := range p.nextN {
			p.pending[track] = true
		}
	}
	for _, track := range splitTracks(s.Frames) {
		if part, ok := p.muxCMAF(ep, track, s.Discontinuity); ok {
			info.Parts = append(info.Parts, part)
		}
	}

	info.Parts = slices.Clip(info.Parts)
	p.mu.Lock()
	p.st.ast = p.ast
	p.st.add(info, ep.publish(), p.window)
	p.mu.Unlock()

	for id := range p.epochs {
		if id != s.Epoch && !p.inWindow(id) {
			delete(p.epochs, id)
		}
	}
}

// inWindow reports whether the store holds a segment of epoch id. Called with
// work held.
func (p *Packager) inWindow(id uint32) bool {
	for _, seg := range p.st.segs {
		if seg.Epoch == id {
			return true
		}
	}
	return false
}

// placeEpoch computes a new epoch's start and effective wall from its hub
// anchor. Without an anchor (the hub keeps only the last 64) the epoch is
// placed so that its first segment ends at the packager's clock, close to
// the segment's arrival: Start is the segment's Start and Wall is Now minus
// the segment's span, then pushed later as the package doc describes.
// FallbackAnchors counts it. Called with work held.
func (p *Packager) placeEpoch(s segment.Segment) *epochState {
	var a media.Anchor
	ok := false
	if p.h != nil {
		a, ok = p.h.Anchor(s.Epoch)
	}
	if !ok {
		a = media.Anchor{Epoch: s.Epoch, Start: s.Start, Wall: p.now().Add(-media.ToDuration(s.End - s.Start))}
		p.fallback.Add(1)
	}
	if p.prev == nil {
		p.ast = a.Wall
	}
	start := a.Wall.Sub(p.ast)
	if p.prev != nil && !ok {
		chain := p.prev.info.Start + media.ToDuration(a.Start-p.prev.info.Anchor.Start)
		start = max(start, min(chain, start+media.MaxAnchorLead))
	}
	start = ceilMS(start)
	if p.prev != nil {
		start = max(start, p.prev.info.Start+time.Millisecond)
	}
	ep := &epochState{
		info:   EpochInfo{Epoch: s.Epoch, Anchor: a, Start: start, Wall: p.ast.Add(start)},
		tracks: map[int]*EpochTrack{},
	}
	p.prev = ep
	return ep
}

// ceilMS rounds d up to a whole number of milliseconds.
func ceilMS(d time.Duration) time.Duration {
	q := d / time.Millisecond
	if d%time.Millisecond > 0 {
		q++
	}
	return q * time.Millisecond
}

// muxTS writes the frames as one TS segment, tables first, on the packager's
// muxer so continuity counters run on across segments. It returns nil and
// counts an error when muxing fails. Called with work held.
func (p *Packager) muxTS(frames []media.Frame) []byte {
	var tracks []*media.TrackConfig
	for _, f := range frames {
		if f.Track < 0 {
			p.errors.Add(1)
			return nil
		}
		if f.Track >= len(tracks) {
			tracks = append(tracks, make([]*media.TrackConfig, f.Track+1-len(tracks))...)
		}
		if tracks[f.Track] == nil {
			tracks[f.Track] = f.Config
		}
	}
	var buf bytes.Buffer
	if err := p.writeTS(&buf, tracks, frames); err != nil {
		p.errors.Add(1)
		return nil
	}
	return buf.Bytes()
}

func (p *Packager) writeTS(buf *bytes.Buffer, tracks []*media.TrackConfig, frames []media.Frame) error {
	if p.tsm == nil {
		m, err := ts.NewMuxer(buf, tracks)
		if err != nil {
			return err
		}
		p.tsm = m
	} else {
		p.tsm.SetOutput(buf)
		if err := p.tsm.SetTracks(tracks); err != nil {
			return err
		}
	}
	if err := p.tsm.WriteTables(); err != nil {
		return err
	}
	for _, f := range frames {
		if err := p.tsm.WriteFrame(f); err != nil {
			return err
		}
	}
	return nil
}

// trackFrames is one track's frames of a segment, in decode order.
type trackFrames struct {
	track  int
	frames []media.Frame
}

// splitTracks groups a segment's frames by track, ascending track.
func splitTracks(frames []media.Frame) []trackFrames {
	var out []trackFrames
	idx := map[int]int{}
	for _, f := range frames {
		i, ok := idx[f.Track]
		if !ok {
			i = len(out)
			idx[f.Track] = i
			out = append(out, trackFrames{track: f.Track})
		}
		out[i].frames = append(out[i].frames, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].track < out[j].track })
	return out
}

// muxCMAF writes one track's CMAF part, writing the epoch's init for the
// track first if it has none. Init and media segment both use the epoch's
// config for the track. On failure it counts an error, leaves the track's
// numbering alone and marks the track discontinuous, since its timeline now
// has a gap. A track's first part is never discontinuous. Called with work held.
func (p *Packager) muxCMAF(ep *epochState, tf trackFrames, disc bool) (CMAFPart, bool) {
	var cfg *media.TrackConfig
	for _, f := range tf.frames {
		if f.Config != nil {
			cfg = f.Config
			break
		}
	}
	n, had := p.nextN[tf.track]
	fail := func() (CMAFPart, bool) {
		p.errors.Add(1)
		p.pending[tf.track] = true
		return CMAFPart{}, false
	}
	if cfg == nil || tf.track < 0 {
		return fail()
	}
	et := ep.tracks[tf.track]
	if et == nil {
		et = &EpochTrack{Track: tf.track, Config: cfg}
		if c, err := media.CodecString(cfg); err == nil {
			et.Codec = c
		} else {
			p.codecErr.Add(1)
		}
		ep.tracks[tf.track] = et
	}
	if et.Init == nil {
		init, err := fmp4.Init(et.Config)
		if err != nil {
			return fail()
		}
		et.Init = init
	}
	data, tm, err := fmp4.Segment(tf.frames, et.Config, uint32(n+1))
	if err != nil {
		return fail()
	}
	part := CMAFPart{
		Track:         tf.track,
		N:             n,
		Discontinuity: had && (disc || p.pending[tf.track]),
		Timing:        tm,
		Data:          data,
	}
	p.nextN[tf.track] = n + 1
	delete(p.pending, tf.track)
	return part, true
}
