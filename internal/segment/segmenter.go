// Package segment cuts the hub's delivery stream into segments on keyframes
// of one anchor track, so every packager shares one set of segment timings.
//
// An epoch's anchor is its lowest-numbered video track, else its
// lowest-numbered audio track. A segment opens on an anchor keyframe (any
// anchor frame for audio) and closes at the first anchor keyframe whose PTS
// is at least Target past the segment's start, or before a delivery that
// changes epoch or reports lost frames. The Segmenter starts no goroutines
// and is not safe for concurrent use.
package segment

import (
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	defaultTarget = 2 * media.ClockRate
	videoDelta    = 3600 // fallback frame duration for a one-frame video track
	audioSamples  = 1024 // AAC frame size
	defaultRate   = 48000
)

// Options tunes a Segmenter.
type Options struct {
	Target int64 // ticks; 0 means 180000 (2 s)
}

// Segment is one run of frames from every track, cut on an anchor keyframe.
type Segment struct {
	Seq           uint64 // from 0, never reused
	Epoch         uint32
	Discontinuity bool          // first segment after an epoch change or Lost > 0
	Start         int64         // PTS of the anchor track's first frame
	Duration      int64         // ticks, anchor track
	End           int64         // largest frame PTS + that track's last DTS delta, over all tracks
	Frames        []media.Frame // arrival order, all tracks
}

// trackAcc tracks one track's extent inside the open segment.
type trackAcc struct {
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
}

// Segmenter cuts deliveries into segments. Create one with New.
type Segmenter struct {
	target  int64
	cur     *open
	seq     uint64
	dropped uint64

	haveEpoch   bool
	epoch       uint32
	anchor      int // -1 when the epoch has no track yet
	anchorVideo bool
	disc        bool          // next segment is a discontinuity
	lost        bool          // next segment opens after Lost > 0
	closed      bool          // a segment of this epoch has closed
	carry       []media.Frame // leading audio waiting for the video anchor's keyframe
}

// New returns a Segmenter.
func New(opt Options) *Segmenter {
	t := opt.Target
	if t <= 0 {
		t = defaultTarget
	}
	return &Segmenter{target: t, anchor: -1}
}

// Dropped returns the number of frames discarded: those that arrived before
// a segment could open, and early non-anchor frames after a loss.
func (s *Segmenter) Dropped() uint64 { return s.dropped }

// Push feeds one delivery and returns the segments it completed.
func (s *Segmenter) Push(d hub.Delivery) []Segment {
	f := d.Frame
	var out []Segment
	if !s.haveEpoch || f.Epoch != s.epoch {
		if s.cur != nil {
			out = append(out, s.closeOpen(-1))
		}
		if s.haveEpoch {
			s.disc = true
		}
		s.haveEpoch, s.epoch = true, f.Epoch
		s.anchor, s.anchorVideo, s.closed = -1, false, false
		s.dropCarry()
	}
	if d.Lost > 0 {
		if s.cur != nil {
			out = append(out, s.closeOpen(-1))
		}
		s.dropCarry()
		s.disc, s.lost = true, true
	}
	if s.promote(f) && s.cur != nil {
		if s.cur.leading {
			s.uncut()
		} else {
			out = append(out, s.closeOpen(-1))
		}
	}

	isAnchor := f.Track == s.anchor
	opens := isAnchor && (f.Key || !s.anchorVideo)
	switch {
	case s.cur == nil && !opens && s.carry != nil && !isAnchor:
		s.carry = append(s.carry, f)
	case s.cur == nil && !opens:
		s.dropped++
	case s.cur == nil:
		s.openAt(f)
	case isAnchor && opens && f.PTS >= s.cur.seg.Start+s.target:
		out = append(out, s.closeOpen(f.PTS-s.cur.seg.Start))
		s.openAt(f)
	case !isAnchor && s.cur.lost && f.PTS < s.cur.seg.Start:
		s.dropped++
	default:
		s.add(f)
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

// Flush closes the open segment, if any.
func (s *Segmenter) Flush() []Segment {
	s.dropCarry()
	if s.cur == nil {
		return nil
	}
	return []Segment{s.closeOpen(-1)}
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
		if c.PTS < f.PTS {
			s.dropped++
			continue
		}
		s.add(c)
	}
	s.add(f)
}

// uncut takes back the open leading audio segment: its frames wait in carry
// for the video anchor's first keyframe and its Seq is released.
func (s *Segmenter) uncut() {
	c := s.cur
	s.cur = nil
	s.seq--
	s.disc, s.lost = c.disc, c.lost
	s.carry = c.seg.Frames
}

func (s *Segmenter) dropCarry() {
	s.dropped += uint64(len(s.carry))
	s.carry = nil
}

func (s *Segmenter) add(f media.Frame) {
	a := s.cur.tracks[f.Track]
	if a == nil {
		a = &trackAcc{dflt: defaultDelta(f)}
		s.cur.tracks[f.Track] = a
	}
	a.add(f)
	s.cur.seg.Frames = append(s.cur.seg.Frames, f)
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
	s.closed = true
	seg := c.seg
	if duration < 0 {
		duration = c.tracks[c.anchor].end() - seg.Start
	}
	seg.Duration = duration
	for _, a := range c.tracks {
		seg.End = max(seg.End, a.end())
	}
	return seg
}
