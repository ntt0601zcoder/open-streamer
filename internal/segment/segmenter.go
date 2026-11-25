// Package segment cuts the hub's delivery stream into segments on keyframes
// of one anchor track, so every packager shares one set of segment timings.
//
// An epoch's anchor is its lowest-numbered video track, else its
// lowest-numbered audio track. A segment opens on an anchor keyframe (any
// anchor frame for audio) and closes at the first anchor keyframe whose PTS
// is at least Target past the segment's start, or before a delivery that
// changes epoch or reports lost frames.
//
// A segment is also cut when a frame of any track is presented MaxDuration
// or more past its start, even without a keyframe, so a source whose
// keyframes stop or are far apart cannot grow the open segment without
// bound. The segment that frame opens is Forced: its first anchor frame need
// not be a keyframe. It is not a discontinuity, because the media continues
// and a decoder must keep its references across the cut; outputs that
// promise segments starting on a keyframe (HLS EXT-X-INDEPENDENT-SEGMENTS)
// must not promise it for a Forced segment. Leading audio held for the
// video anchor's first keyframe is likewise kept to MaxDuration.
//
// Audio is cut at the anchor's cut PTS K, not by arrival order (rulings
// R-F3 and R-T1d), for the cuts made within an epoch: on a keyframe, at
// MaxDuration (K is the PTS of the frame that triggers it) and when the
// anchor changes. Audio frames of the closing segment, other than the
// anchor's, presented at or after K move to the next segment (to the carry
// when an anchor change opens none), and the closed segment is held until
// every other audio track of the epoch has delivered a frame at or after K,
// or some track of the epoch has reached 1 s past K. An audio track that
// makes a hold time out is not waited for again until it delivers. Audio
// frames below K that arrive while the segment is held go into it; after
// its release they go into the open segment and count as Late. Video tracks
// other than the anchor are never moved or waited for: they stay in arrival
// order, since their decode order differs from PTS order. Held segments
// come out in Seq order, all of them before the open segment closes at an
// epoch change, Lost > 0 or Flush; those closes neither move frames nor
// hold, so a segment they close may hold audio past its end.
//
// Options.AlignVideo (ruling R-B8) is for an ABR presentation, whose video
// tracks are rungs keyed at the same PTS: it keeps every rung's part of a
// segment starting on a keyframe at the segment's Start, with two rules.
// First, while the anchor is a video track, a lower video track takes the
// anchor only at a keyframe presented at or after the open segment's Start,
// or after the newest closed segment's Start when none is open (any
// keyframe before the epoch's first segment); its frames before that are
// dropped. At the open segment's Start that segment is taken back and
// reopened by the new anchor, with its frames presented at or after Start;
// after Start it is cut there. So no segment starts at or before its
// predecessor's Start. A leading audio anchor is superseded as without
// AlignVideo. Second, in a segment that is not Forced, a video track other
// than the anchor enters only at a keyframe presented at the segment's
// Start: its frames before its first keyframe in the segment are dropped,
// and if that keyframe has another PTS, the track is left out of the
// segment. Audio is not affected.
//
// The Segmenter starts no goroutines and is not safe for concurrent use.
package segment

import (
	"slices"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	defaultTarget = 2 * media.ClockRate
	videoDelta    = 3600 // fallback frame duration for a one-frame video track
	audioSamples  = 1024 // AAC frame size
	defaultRate   = 48000

	// holdTimeout is how far past a cut any track of the epoch may reach
	// before the held segment is released without the tracks it waits for.
	holdTimeout = media.ClockRate
)

// Options tunes a Segmenter.
type Options struct {
	Target      int64 // ticks; 0 means 180000 (2 s)
	MaxDuration int64 // ticks; 0 means 5 × Target; never below Target
	// AlignVideo keeps every video track's part of a segment starting on a
	// keyframe at the segment's Start (ruling R-B8; see the package doc).
	AlignVideo bool
}

// Segment is one run of frames from every track, cut on an anchor keyframe.
type Segment struct {
	Seq           uint64 // from 0, never reused
	Epoch         uint32
	Discontinuity bool          // first segment after an epoch change or Lost > 0
	Forced        bool          // opened by the MaxDuration cut: its first frame need not be a keyframe
	Start         int64         // PTS of the anchor track's first frame
	Duration      int64         // ticks, anchor track
	End           int64         // largest frame PTS + that track's last DTS delta, over all tracks
	Frames        []media.Frame // arrival order, all tracks
	// VideoDurations, under AlignVideo only, holds the span in ticks of
	// each video track whose span in the segment is not Duration; a track
	// it does not hold spans Duration, and it is nil when none differs. A
	// segment cut at K (by the next anchor keyframe or the MaxDuration cut)
	// ends every rung at K, so it is always nil there. A segment closed
	// without a cut (an epoch change, Lost > 0 or Flush) ends each rung
	// where its own frames end, at its largest PTS + last DTS delta, as
	// Duration does for the anchor: after a respawn the anchor rung can
	// stop well before the others.
	VideoDurations map[int]int64
}

// trackAcc tracks one track's extent inside the open segment.
type trackAcc struct {
	video   bool
	maxPTS  int64
	lastDTS int64
	delta   int64 // last DTS delta; 0 until the second frame
	frames  int
	dflt    int64 // delta used while the track has one frame
}

func (a *trackAcc) add(f media.Frame) {
	if a.frames > 0 {
		a.delta = f.DTS - a.lastDTS
	}
	a.lastDTS = f.DTS
	a.frames++
	if a.frames == 1 || f.PTS > a.maxPTS {
		a.maxPTS = f.PTS
	}
}

func (a *trackAcc) end() int64 {
	d := a.delta
	if a.frames < 2 {
		d = a.dflt
	}
	return a.maxPTS + d
}

// open is the segment being built.
type open struct {
	seg    Segment
	anchor int
	lost   bool // opened after Lost > 0: early non-anchor frames are dropped
	disc   bool // s.disc when it opened, restored if the segment is carried
	// leading marks an audio-anchored segment that opened before any segment
	// of its epoch closed: a video anchor supersedes it instead of cutting it.
	leading bool
	tracks  map[int]*trackAcc
	// rungs holds, under AlignVideo, the state of each video track other
	// than the anchor; a track not in it is waiting.
	rungs map[int]rung
}

// rung is a video track's state in an open segment under AlignVideo.
type rung uint8

const (
	rungWaiting rung = iota // no keyframe yet: its frames are dropped
	rungIn                  // entered at a keyframe at the segment's Start
	rungOut                 // first keyframe elsewhere: left out of the segment
)

// held is a segment closed by a cut at K, waiting for the other tracks to
// reach K.
type held struct {
	c        *open
	k        int64
	duration int64 // as for closeOpen
}

// Segmenter cuts deliveries into segments. Create one with New.
type Segmenter struct {
	target  int64
	maxDur  int64
	align   bool
	cur     *open
	held    []held // in Seq order
	seq     uint64
	dropped uint64
	late    uint64

	haveEpoch   bool
	epoch       uint32
	anchor      int // -1 when the epoch has no track yet
	anchorVideo bool
	disc        bool          // next segment is a discontinuity
	lost        bool          // next segment opens after Lost > 0
	closed      bool          // a segment of this epoch has closed
	lastStart   int64         // Start of the newest closed segment, when closed
	carry       []media.Frame // leading audio waiting for the video anchor's keyframe
	reached     map[int]int64 // per track of the epoch: its largest PTS so far
	audio       map[int]bool  // tracks of the epoch that carry audio
	timedOut    map[int]bool  // audio tracks a hold stopped waiting for
}

// New returns a Segmenter.
func New(opt Options) *Segmenter {
	t := opt.Target
	if t <= 0 {
		t = defaultTarget
	}
	m := opt.MaxDuration
	if m <= 0 {
		m = 5 * t
	}
	return &Segmenter{
		target: t, maxDur: max(m, t), anchor: -1, align: opt.AlignVideo,
		reached: map[int]int64{}, audio: map[int]bool{}, timedOut: map[int]bool{},
	}
}

// Dropped returns the number of frames discarded: those that arrived before
// a segment could open, early non-anchor frames after a loss, and, under
// AlignVideo, the video frames its rules leave out.
func (s *Segmenter) Dropped() uint64 { return s.dropped }

// Late returns the number of non-anchor audio frames put in a segment that
// starts after their PTS: those that arrived after the segment cut at their
// PTS was released, and those below the first segment of an epoch.
func (s *Segmenter) Late() uint64 { return s.late }

// Push feeds one delivery and returns the segments it completed.
func (s *Segmenter) Push(d hub.Delivery) []Segment {
	f := d.Frame
	var out []Segment
	if !s.haveEpoch || f.Epoch != s.epoch {
		out = s.closeAll(out)
		if s.haveEpoch {
			s.disc = true
		}
		s.haveEpoch, s.epoch = true, f.Epoch
		s.anchor, s.anchorVideo, s.closed = -1, false, false
		s.dropCarry()
		clear(s.reached)
		clear(s.audio)
		clear(s.timedOut)
	}
	if d.Lost > 0 {
		out = s.closeAll(out)
		s.dropCarry()
		s.disc, s.lost = true, true
	}
	if s.belowAnchor(f) {
		s.dropped++
		return s.release(out)
	}
	if r, ok := s.reached[f.Track]; !ok || f.PTS > r {
		s.reached[f.Track] = f.PTS
	}
	if f.Kind() == media.KindAudio {
		s.audio[f.Track] = true
	}
	delete(s.timedOut, f.Track)
	wasVideo := s.anchorVideo
	if s.promote(f) && s.cur != nil {
		// Under AlignVideo a lower video track reaches here only on a
		// keyframe at or after the open segment's Start.
		if s.cur.leading || (s.align && wasVideo && f.PTS == s.cur.seg.Start) {
			s.uncut()
		} else {
			s.cut(f.PTS, -1)
		}
	}

	isAnchor := f.Track == s.anchor
	opens := isAnchor && (f.Key || !s.anchorVideo)
	switch {
	case !isAnchor && f.Kind() == media.KindAudio && s.toHeld(f):
	case s.cur == nil && !opens && s.carry != nil && !isAnchor:
		s.carry = append(s.carry, f)
		s.trimCarry()
	case s.cur != nil && !opens && f.PTS >= s.cur.seg.Start+s.maxDur:
		s.cut(f.PTS, f.PTS-s.cur.seg.Start)
		s.openAt(f)
		s.cur.seg.Forced = true
	case s.cur == nil && !opens:
		s.dropped++
	case s.cur == nil:
		s.openAt(f)
	case isAnchor && opens && f.PTS >= s.cur.seg.Start+s.target:
		s.cut(f.PTS, f.PTS-s.cur.seg.Start)
		s.openAt(f)
	default:
		s.place(s.cur, f)
	}
	return s.release(out)
}

// cut closes the open segment at K within the epoch: its audio frames of
// tracks other than the epoch's anchor presented at or after K go to the
// carry, from where the next segment takes them, and it is held. Video
// frames never move. A K at or before the segment's start (an anchor change
// to a track presented earlier) moves nothing. duration is as for closeOpen.
func (s *Segmenter) cut(k, duration int64) {
	c := s.cur
	s.cur = nil
	s.closed, s.lastStart = true, c.seg.Start
	moved := []media.Frame{}
	kept := c.seg.Frames[:0]
	for _, f := range c.seg.Frames {
		if f.Track != s.anchor && f.Kind() == media.KindAudio && f.PTS >= k && k > c.seg.Start {
			moved = append(moved, f)
		} else {
			kept = append(kept, f)
		}
	}
	if len(moved) > 0 {
		// Rebuild the accumulators from the frames that stay.
		c.seg.Frames = nil
		c.tracks = map[int]*trackAcc{}
		for _, f := range slices.Clip(kept) {
			addTo(c, f)
		}
	}
	s.carry = moved
	s.held = append(s.held, held{c: c, k: k, duration: duration})
}

// toHeld puts a non-anchor audio frame presented before a held segment's
// cut into the first such segment, and reports whether it did.
func (s *Segmenter) toHeld(f media.Frame) bool {
	for i := range s.held {
		if f.PTS < s.held[i].k {
			s.place(s.held[i].c, f)
			return true
		}
	}
	return false
}

// place adds a frame to c. A non-anchor frame presented before c's start is
// dropped after a loss, else kept, and counted late if it is audio. Under
// AlignVideo a video frame that admits rejects is dropped.
func (s *Segmenter) place(c *open, f media.Frame) {
	if f.Track != s.anchor && f.PTS < c.seg.Start {
		if c.lost {
			s.dropped++
			return
		}
		if f.Kind() == media.KindAudio {
			s.late++
		}
	}
	if !s.admits(c, f) {
		s.dropped++
		return
	}
	addTo(c, f)
}

// belowAnchor reports whether, under AlignVideo, f is a frame of a video
// track below the video anchor that may not take the anchor (ruling R-B8):
// only a keyframe presented at or after the open segment's Start may, or
// after the newest closed segment's Start when none is open, or any
// keyframe when the epoch has no segment yet.
func (s *Segmenter) belowAnchor(f media.Frame) bool {
	if !s.align || !s.anchorVideo || f.Kind() != media.KindVideo || f.Track >= s.anchor {
		return false
	}
	switch {
	case !f.Key:
		return true
	case s.cur != nil:
		return f.PTS < s.cur.seg.Start
	case s.closed:
		return f.PTS <= s.lastStart
	default:
		return false
	}
}

// admits reports whether frame f may go into c under AlignVideo's late-rung
// rule (ruling R-B8): in a segment that is not Forced, a video track other
// than the epoch's anchor enters only at a keyframe presented at c's Start,
// and is left out of c when its first keyframe in c has another PTS. It
// records the track's state on that keyframe.
func (s *Segmenter) admits(c *open, f media.Frame) bool {
	if !s.align || c.seg.Forced || f.Track == s.anchor || f.Kind() != media.KindVideo {
		return true
	}
	st := c.rungs[f.Track]
	if st == rungWaiting && f.Key {
		st = rungOut
		if f.PTS == c.seg.Start {
			st = rungIn
		}
		if c.rungs == nil {
			c.rungs = map[int]rung{}
		}
		c.rungs[f.Track] = st
	}
	return st == rungIn
}

// release emits the held segments, in order, while the first one no longer
// waits.
func (s *Segmenter) release(out []Segment) []Segment {
	for len(s.held) > 0 && s.releasable(s.held[0].k) {
		out = append(out, s.finish(s.held[0].c, s.held[0].duration))
		s.held[0] = held{}
		s.held = s.held[1:]
	}
	if len(s.held) == 0 {
		s.held = nil
	}
	return out
}

// releasable reports whether a segment cut at k may come out: every audio
// track of the epoch other than the anchor has reached k or was given up,
// or some track of the epoch has reached holdTimeout past k, in which case
// the audio tracks still short of k are given up until they deliver again.
// Timing out on any track, not only the anchor, bounds the held segments
// when the anchor stalls and forced cuts follow another track.
func (s *Segmenter) releasable(k int64) bool {
	waiting := false
	furthest := k
	for tr, r := range s.reached {
		furthest = max(furthest, r)
		if s.waitsFor(tr) && r < k {
			waiting = true
		}
	}
	if !waiting {
		return true
	}
	if furthest < k+holdTimeout {
		return false
	}
	for tr, r := range s.reached {
		if s.waitsFor(tr) && r < k {
			s.timedOut[tr] = true
		}
	}
	return true
}

// waitsFor reports whether a hold waits for track tr: an audio track of the
// epoch other than the anchor that has not timed out.
func (s *Segmenter) waitsFor(tr int) bool {
	return tr != s.anchor && s.audio[tr] && !s.timedOut[tr]
}

// closeAll emits every held segment, in order, then closes the open one,
// without moving frames.
func (s *Segmenter) closeAll(out []Segment) []Segment {
	for _, h := range s.held {
		out = append(out, s.finish(h.c, h.duration))
	}
	s.held = nil
	if s.cur != nil {
		out = append(out, s.closeOpen(-1))
	}
	return out
}

// promote registers the frame's track and reports whether the anchor changed
// away from an earlier one.
func (s *Segmenter) promote(f media.Frame) bool {
	video := f.Kind() == media.KindVideo
	switch {
	case s.anchor < 0:
	case video && (!s.anchorVideo || f.Track < s.anchor):
	case !video && !s.anchorVideo && f.Track < s.anchor:
	default:
		return false
	}
	changed := s.anchor >= 0
	s.anchor, s.anchorVideo = f.Track, video
	return changed
}

// Flush emits the held segments and closes the open one, if any.
func (s *Segmenter) Flush() []Segment {
	s.dropCarry()
	return s.closeAll(nil)
}

func (s *Segmenter) openAt(f media.Frame) {
	s.cur = &open{
		seg: Segment{
			Seq:           s.seq,
			Epoch:         f.Epoch,
			Discontinuity: s.disc && s.seq > 0,
			Start:         f.PTS,
		},
		anchor:  f.Track,
		lost:    s.lost,
		disc:    s.disc,
		leading: !s.anchorVideo && !s.closed,
		tracks:  map[int]*trackAcc{},
	}
	s.seq++
	s.disc, s.lost = false, false
	carry := s.carry
	s.carry = nil
	for _, c := range carry {
		if c.PTS < f.PTS || !s.admits(s.cur, c) {
			s.dropped++
			continue
		}
		s.add(c)
	}
	s.add(f)
}

// uncut takes back the open segment, the newest Seq: its frames wait in
// carry for the next segment to open and its Seq is released. It serves a
// leading audio segment superseded by the video anchor, and under AlignVideo
// a segment whose Start a lower video track's keyframe takes over.
func (s *Segmenter) uncut() {
	c := s.cur
	s.cur = nil
	s.seq--
	s.disc, s.lost = c.disc, c.lost
	s.carry = c.seg.Frames
}

// trimCarry drops the oldest leading audio while the carry spans
// MaxDuration or more.
func (s *Segmenter) trimCarry() {
	n := 0
	last := s.carry[len(s.carry)-1].PTS
	for n < len(s.carry) && last-s.carry[n].PTS >= s.maxDur {
		n++
	}
	if n > 0 {
		s.dropped += uint64(n)
		s.carry = append(s.carry[:0], s.carry[n:]...)
	}
}

func (s *Segmenter) dropCarry() {
	s.dropped += uint64(len(s.carry))
	s.carry = nil
}

func (s *Segmenter) add(f media.Frame) { addTo(s.cur, f) }

func addTo(c *open, f media.Frame) {
	a := c.tracks[f.Track]
	if a == nil {
		a = &trackAcc{video: f.Kind() == media.KindVideo, dflt: defaultDelta(f)}
		c.tracks[f.Track] = a
	}
	a.add(f)
	c.seg.Frames = append(c.seg.Frames, f)
}

func defaultDelta(f media.Frame) int64 {
	if f.Kind() != media.KindAudio {
		return videoDelta
	}
	rate := defaultRate
	if f.Config != nil && f.Config.SampleRate > 0 {
		rate = f.Config.SampleRate
	}
	return media.SampleTime(audioSamples, rate)
}

// closeOpen finishes the open segment. duration is the anchor span when the
// next anchor keyframe closed it, or negative to derive it from the frames.
func (s *Segmenter) closeOpen(duration int64) Segment {
	c := s.cur
	s.cur = nil
	s.closed, s.lastStart = true, c.seg.Start
	return s.finish(c, duration)
}

// finish builds the closed segment c; duration is as for closeOpen.
func (s *Segmenter) finish(c *open, duration int64) Segment {
	seg := c.seg
	cut := duration >= 0
	if !cut {
		duration = c.tracks[c.anchor].end() - seg.Start
	}
	seg.Duration = duration
	for _, a := range c.tracks {
		seg.End = max(seg.End, a.end())
	}
	if s.align && !cut {
		for tr, a := range c.tracks {
			if d := a.end() - seg.Start; a.video && d != duration {
				if seg.VideoDurations == nil {
					seg.VideoDurations = map[int]int64{}
				}
				seg.VideoDurations[tr] = d
			}
		}
	}
	return seg
}
