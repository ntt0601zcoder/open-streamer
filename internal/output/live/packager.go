// Package live packages one stream for HLS (TS and fMP4) and DASH.
//
// A Packager subscribes to the stream's hub, cuts segments with one
// segment.Segmenter, muxes each closed segment once as TS (in ABR mode once
// per video track, see Options.ABR) and once per track as CMAF, and keeps
// the last Window segments. Handler serves the segment and init files; the
// playlists and the MPD are built from a Snapshot of the window on request.
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
	"fmt"
	"log/slog"
	"maps"
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

	// rungWarnEvery is the least time between two warnings about rungs
	// left out of segments.
	rungWarnEvery = 10 * time.Second
)

// Options configures a Packager.
type Options struct {
	Target int64            // segment target in 90 kHz ticks; 0 means 180000 (2 s)
	Window int              // segments kept; 0 means 6
	Now    func() time.Time // nil means time.Now
	// ABR marks the stream as an ABR presentation (ruling R-B1): every video
	// track is a rendition of the same content, keyed at the same PTS. The
	// segmenter then aligns the video tracks (segment.Options.AlignVideo),
	// cmaf/master.m3u8 lists every video track as a variant (R-B5) and the
	// MPD puts video tracks of one codec in one AdaptationSet (R-B6). HLS-TS
	// muxes one TS per video track instead of a combined one (R-B4):
	// hls/index.m3u8 becomes a multivariant playlist over hls/v{track}.m3u8.
	ABR bool
	// Logger receives the ABR warning about rungs left out of segments, at
	// most once every 10 s; nil discards.
	Logger *slog.Logger
	// Join is for a packager that starts on a hub that is already live: it
	// is given the cached GOP, and the audio presented before that GOP's
	// keyframe arrives behind it. Join drops that audio, so the first
	// segment does not start with audio only (segment.Options.Join).
	Join bool
	// AudioLanguage is the declared language of the audio (R-L2), the first
	// value SetAudioLanguage would set; "" or an invalid tag writes none.
	AudioLanguage string
}

// RungStats counts what an ABR presentation lost of one video track (a
// rung). Gaps is the number of segments packaged without a part of the
// track since the track was first seen (its variant lists a gap there, and
// its DASH timeline a hole). LeftOut is the number of the track's frames
// that the segmenter discarded to keep every part starting on a keyframe at
// its segment's start (ruling R-B8; segment.Segmenter.LeftOut). A rung that
// keeps arriving too late for the keyframe barrier shows as both rising
// steadily.
type RungStats struct {
	Gaps, LeftOut uint64
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
	abr      bool
	now      func() time.Time
	errors   atomic.Uint64
	fallback atomic.Uint64
	codecErr atomic.Uint64
	// lang is the declared audio language (SetAudioLanguage). NewPackager
	// always stores one, so it is never nil after it; the nil guard in
	// Snapshot only matters for a zero-value Packager.
	lang atomic.Pointer[string]

	// Writer state, guarded by work.
	work    sync.Mutex
	seg     *segment.Segmenter
	tsm     *ts.Muxer
	vtsm    map[int]*ts.Muxer      // ABR: per video track, its variant's muxer
	epochs  map[uint32]*epochState // epochs with a segment in the window, plus the newest
	prev    *epochState            // the last epoch placed on the wallclock
	ast     time.Time              // valid once prev is set
	nextN   map[int]uint64         // per track: N of its next CMAF part
	pending map[int]bool           // per track: a discontinuity since its last part
	tsSeq   uint64                 // TSSeq of the next segment with TS bytes
	tsGap   bool                   // a TS segment was dropped since the last good one

	// ABR rung losses. rungs is guarded by rungMu; the rest by work.
	log      *slog.Logger
	rungMu   sync.Mutex
	rungs    map[int]RungStats
	warned   map[int]RungStats // rungs at the last warning
	warnedAt time.Time

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
		abr:     opt.ABR,
		now:     opt.Now,
		epochs:  map[uint32]*epochState{},
		nextN:   map[int]uint64{},
		pending: map[int]bool{},
		vtsm:    map[int]*ts.Muxer{},
		log:     opt.Logger,
		rungs:   map[int]RungStats{},
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
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
	p.seg = segment.New(segment.Options{Target: p.target, AlignVideo: p.abr, Join: opt.Join})
	p.SetAudioLanguage(opt.AudioLanguage)
	return p
}

// SetAudioLanguage replaces the declared language of the audio (R-L2). The
// next Snapshot, and so every playlist and MPD built after the call returns,
// carries it. A value failing media.ValidLanguage is stored as "", so
// nothing is written for it. It is safe to call concurrently with every
// other method.
func (p *Packager) SetAudioLanguage(lang string) {
	if !media.ValidLanguage(lang) {
		lang = ""
	}
	p.lang.Store(&lang)
}

// Errors returns the number of mux errors so far. Each one dropped one
// segment's output in one format: its TS, one variant's TS in ABR mode, or
// one track's CMAF part.
func (p *Packager) Errors() uint64 { return p.errors.Load() }

// FallbackAnchors returns the number of epochs placed without a hub anchor.
func (p *Packager) FallbackAnchors() uint64 { return p.fallback.Load() }

// CodecErrors returns the number of epoch tracks whose codec string could
// not be built, counted once each when the track's first part is packaged.
// The DASH MPD leaves such a track out, and the HLS master's CODECS lacks it.
func (p *Packager) CodecErrors() uint64 { return p.codecErr.Load() }

// Rungs returns, for an ABR presentation, what each video track lost: the
// tracks that had a part in some segment or had frames left out. It is
// empty when not in ABR mode. The counts are as of the last segment
// packaged.
func (p *Packager) Rungs() map[int]RungStats {
	p.rungMu.Lock()
	defer p.rungMu.Unlock()
	return maps.Clone(p.rungs)
}

// noteRungs updates the rung counters after segment seq was packaged with
// parts: a gap for every known video track without a part, and the
// segmenter's left-out frames. When a counter rose, it warns, at most once
// every rungWarnEvery. Called with work held.
func (p *Packager) noteRungs(ep *epochState, seq uint64, parts []CMAFPart) {
	leftOut := p.seg.LeftOut()
	p.rungMu.Lock()
	in := map[int]bool{}
	for _, part := range parts {
		et := ep.tracks[part.Track]
		if et == nil || et.Config == nil || et.Config.Codec.Kind() != media.KindVideo {
			continue
		}
		in[part.Track] = true
		if _, ok := p.rungs[part.Track]; !ok {
			p.rungs[part.Track] = RungStats{}
		}
	}
	for track, n := range leftOut {
		r := p.rungs[track]
		r.LeftOut = n
		p.rungs[track] = r
	}
	for track, r := range p.rungs {
		if !in[track] {
			r.Gaps++
			p.rungs[track] = r
		}
	}
	cur := maps.Clone(p.rungs)
	p.rungMu.Unlock()
	if sameLosses(cur, p.warned) {
		return
	}
	t := p.now()
	if !p.warnedAt.IsZero() && t.Sub(p.warnedAt) < rungWarnEvery {
		return
	}
	tracks := slices.Sorted(maps.Keys(cur))
	list := make([]string, 0, len(tracks))
	for _, track := range tracks {
		r, w := cur[track], p.warned[track]
		if r != w {
			list = append(list, fmt.Sprintf("track %d: %d segments without it (+%d), %d frames left out (+%d)",
				track, r.Gaps, r.Gaps-w.Gaps, r.LeftOut, r.LeftOut-w.LeftOut))
		}
	}
	p.log.Warn("live: abr: rungs left out of segments", "seq", seq, "rungs", list)
	p.warned, p.warnedAt = cur, t
}

// sameLosses reports whether every track of a has the same counts in b, a
// track that b lacks counting as zero there.
func sameLosses(a, b map[int]RungStats) bool {
	for track, r := range a {
		if b[track] != r {
			return false
		}
	}
	return true
}

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
	s.Target, s.Window, s.Now, s.ABR = p.target, p.window, now, p.abr
	if lang := p.lang.Load(); lang != nil {
		s.AudioLanguage = *lang
	}
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
	}
	if p.abr {
		info.VariantTS = p.muxVariants(s)
	} else {
		p.packTS(&info, s)
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
	if p.abr {
		p.noteRungs(ep, s.Seq, info.Parts)
	}
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

// packTS muxes the segment's combined TS into info and numbers it for the
// HLS-TS playlist. Called with work held.
func (p *Packager) packTS(info *SegmentInfo, s segment.Segment) {
	info.TS = p.muxTS(s.Frames)
	if info.TS == nil {
		p.tsGap = true
		return
	}
	// The first segment with TS bytes has nothing before it to be
	// discontinuous with, like the segmenter's first segment.
	info.TSSeq = p.tsSeq
	info.TSDiscontinuity = p.tsSeq > 0 && (s.Discontinuity || p.tsGap)
	p.tsSeq++
	p.tsGap = false
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
	if err := writeTS(&p.tsm, &buf, tracks, frames); err != nil {
		p.errors.Add(1)
		return nil
	}
	return buf.Bytes()
}

// muxVariants writes the segment's ABR variants (ruling R-B4): one TS per
// video track with frames in the segment, ascending, holding that track's
// frames at track index 0 and every audio frame at 1 + its track's rank
// among the known audio tracks and the segment's, so a variant's PIDs stay
// put across segments. Each variant has its own muxer, kept across segments
// for its continuity counters. A variant that fails to mux counts an error
// and is left out. A frame no variant can place (a negative track, or a
// track with no config in the segment or of neither kind) fails every
// variant, as it fails the combined TS. Each variant's Duration is its
// video's span: seg.VideoDurations', else seg.Duration. Called with work
// held.
func (p *Packager) muxVariants(seg segment.Segment) []VariantTS {
	frames := seg.Frames
	cfgs := map[int]*media.TrackConfig{} // per track: its first config in the segment
	for _, f := range frames {
		if _, ok := cfgs[f.Track]; !ok && f.Config != nil {
			cfgs[f.Track] = f.Config
		}
	}
	kind := func(track int) media.Kind {
		if c := cfgs[track]; c != nil {
			return c.Codec.Kind()
		}
		return 0
	}
	var videos, audios []int
	bad := false
	for _, f := range frames {
		k := kind(f.Track)
		switch {
		case f.Track < 0 || (k != media.KindVideo && k != media.KindAudio):
			bad = true
		case k == media.KindVideo && !slices.Contains(videos, f.Track):
			videos = append(videos, f.Track)
		case k == media.KindAudio && !slices.Contains(audios, f.Track):
			audios = append(audios, f.Track)
		}
	}
	slices.Sort(videos)
	if bad {
		p.errors.Add(uint64(len(videos)))
		return nil
	}
	ranked := slices.Clone(audios)
	for _, kt := range p.st.known {
		if kt.Config != nil && kt.Config.Codec.Kind() == media.KindAudio && !slices.Contains(ranked, kt.Track) {
			ranked = append(ranked, kt.Track)
		}
	}
	slices.Sort(ranked)
	slot := map[int]int{} // audio track -> its track index in every variant
	for i, track := range ranked {
		slot[track] = 1 + i
	}

	var out []VariantTS
	for _, v := range videos {
		tracks := make([]*media.TrackConfig, 1+len(ranked))
		tracks[0] = cfgs[v]
		for _, a := range audios {
			tracks[slot[a]] = cfgs[a]
		}
		fs := make([]media.Frame, 0, len(frames))
		for _, f := range frames {
			switch {
			case f.Track == v:
				f.Track = 0
			case kind(f.Track) == media.KindAudio:
				f.Track = slot[f.Track]
			default:
				continue // another variant's video
			}
			fs = append(fs, f)
		}
		m := p.vtsm[v]
		var buf bytes.Buffer
		err := writeTS(&m, &buf, tracks, fs)
		if m != nil {
			p.vtsm[v] = m
		}
		if err != nil {
			p.errors.Add(1)
			continue
		}
		d, ok := seg.VideoDurations[v]
		if !ok {
			d = seg.Duration
		}
		out = append(out, VariantTS{Track: v, Data: buf.Bytes(), Duration: d})
	}
	return slices.Clip(out)
}

// writeTS writes the frames to buf on *m, tables first, creating the muxer
// if *m is nil.
func writeTS(m **ts.Muxer, buf *bytes.Buffer, tracks []*media.TrackConfig, frames []media.Frame) error {
	if *m == nil {
		mx, err := ts.NewMuxer(buf, tracks)
		if err != nil {
			return err
		}
		*m = mx
	} else {
		(*m).SetOutput(buf)
		if err := (*m).SetTracks(tracks); err != nil {
			return err
		}
	}
	if err := (*m).WriteTables(); err != nil {
		return err
	}
	for _, f := range frames {
		if err := (*m).WriteFrame(f); err != nil {
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
