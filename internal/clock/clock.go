// Package clock turns source timestamps into stream time and assigns epochs.
//
// One Clock sits between a stream's demuxer and its hub. Source timestamps
// come in already unwrapped (int64, 90 kHz, possibly negative). Within an
// epoch, stream = source − base + Start, so deltas are kept exactly. An epoch
// starts on a video keyframe with audio aligned to it, after the previous
// epoch's presentation end, and Clock is the only place time is rewritten.
package clock

import (
	"slices"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	defaultAlignWindow = media.ClockRate
	defaultMaxForward  = 5 * media.ClockRate
	defaultMaxBackJump = 2 * media.ClockRate
	defaultMaxBackStep = media.ClockRate / 2
	defaultVideoWait   = 5 * media.ClockRate
	defaultDuration    = 3600 // lastDuration when a track has no DTS delta yet
	maxEpochs          = 64
)

// Options tunes a Clock. Zero values select the defaults.
type Options struct {
	Kinds       []media.Kind     // kind of each expected track, by index (from TrackKinds)
	AlignWindow int64            // source ticks held at an epoch start; 0 means 90000
	MaxForward  int64            // forward jump past the high-water mark that opens an epoch; 0 means 5*90000
	MaxBackJump int64            // backward jump below the high-water mark that opens an epoch; 0 means 2*90000
	MaxBackStep int64            // per-track continuity tolerance while an epoch is pending; 0 means 45000
	VideoWait   int64            // held audio span before releasing without a video keyframe; 0 means 5*90000
	Now         func() time.Time // nil means time.Now
}

// Epoch is the anchor of one timeline. Its smallest DTS maps to stream time
// Start. Wall is the arrival time (Now() during its Push) of the first frame
// the epoch released, that is, the earliest-arriving held frame that survived
// the start gates and the early-audio drop.
type Epoch struct {
	ID    uint32
	Start int64
	Wall  time.Time
}

// Stats counts dropped frames by reason.
type Stats struct {
	DroppedBackStep, DroppedBeforeKey, DroppedRASL, DroppedEarlyAudio, DroppedStale, DroppedLate uint64
	// DroppedInvalid counts input frames with PTS below DTS, dropped on entry
	// so that every output frame has PTS >= DTS.
	DroppedInvalid uint64
}

// gate is a video track's start-of-epoch gate (rule 5).
type gate uint8

const (
	gateKey  gate = iota // dropping frames until the first keyframe
	gateRASL             // dropping RASL frames directly after that keyframe
	gateOpen
)

// trackEpoch is one track's state within one epoch.
type trackEpoch struct {
	joined   bool
	fresh    bool  // joined before a restart: the next accepted frame rejoins
	hasLast  bool  // last is set
	last     int64 // lastSourceDTS: source DTS of the previous accepted frame
	gate     gate
	held     int  // frames of this track held by the pending epoch
	audioOut bool // an audio frame of this track was emitted in this epoch
}

type track struct {
	cfg  *media.TrackConfig
	seen bool
	cur  trackEpoch // in the released epoch
	pend trackEpoch // in the pending epoch

	emitted       bool
	lastEmitEpoch uint32
	lastEmitDTS   int64 // stream time
	lastDur       int64 // last DTS delta within an epoch, 0 if unknown
	maxPTS        int64 // largest stream PTS emitted
}

func (t *track) duration() int64 {
	if t.lastDur > 0 {
		return t.lastDur
	}
	return defaultDuration
}

// epoch is a released epoch.
type epoch struct {
	id          uint32
	base, start int64
	hasKey      bool
	keyPTS      int64
	jump        bool // opened by a jump: unjoined tracks get rule 3's old test
	audioOnly   bool // released by VideoWait: the first keyframe opens the next epoch
}

// heldFrame is a frame held by a pending epoch, in source time.
type heldFrame struct {
	f  media.Frame
	at time.Time // arrival: Now() during its Push
}

// pending is an epoch that holds frames until it is released.
type pending struct {
	id        uint32
	jump      bool
	held      []heldFrame // arrival order
	hasKey    bool
	keyPTS    int64 // smallest first-keyframe PTS among video tracks
	hasAnchor bool
	anchor    int64 // DTS of the first video keyframe held
	hasHW     bool
	hw        int64 // largest source DTS accepted
}

// Clock maps one stream's source time to stream time. It is not safe for
// concurrent use.
type Clock struct {
	opt         Options
	expectVideo bool
	tracks      []*track
	cur         *epoch   // nil until epoch 1 is released
	hw          int64    // largest source DTS accepted in cur
	pend        *pending // nil when no epoch is pending
	epochs      []Epoch
	stats       Stats
	out         []media.Frame
}

// New returns a Clock with epoch 1 pending.
func New(opt Options) *Clock {
	opt.Kinds = slices.Clone(opt.Kinds)
	opt.AlignWindow = orDefault(opt.AlignWindow, defaultAlignWindow)
	opt.MaxForward = orDefault(opt.MaxForward, defaultMaxForward)
	opt.MaxBackJump = orDefault(opt.MaxBackJump, defaultMaxBackJump)
	opt.MaxBackStep = orDefault(opt.MaxBackStep, defaultMaxBackStep)
	opt.VideoWait = orDefault(opt.VideoWait, defaultVideoWait)
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Clock{
		opt:         opt,
		expectVideo: slices.Contains(opt.Kinds, media.KindVideo),
		pend:        &pending{id: 1},
	}
}

func orDefault(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// Push takes one frame in source time and returns the frames it releases, in
// stream time and arrival order. f.Track must not be negative. A frame with
// PTS below DTS is dropped and counted in Stats.DroppedInvalid.
func (c *Clock) Push(f media.Frame) []media.Frame {
	if f.PTS < f.DTS {
		c.stats.DroppedInvalid++
		return nil
	}
	for len(c.tracks) <= f.Track {
		c.tracks = append(c.tracks, &track{})
	}
	t := c.tracks[f.Track]
	changed := t.seen && f.Config != t.cfg
	t.cfg, t.seen = f.Config, true

	switch {
	case changed:
		c.configChange(f)
	case c.pend != nil:
		c.pushPending(f, t)
	default:
		c.pushReleased(f, t)
	}
	return c.takeOut()
}

// Break makes the next frames open a new epoch that every track joins with
// its next frame. It does nothing while an epoch is pending.
func (c *Clock) Break() {
	if c.pend == nil {
		c.openPending(false)
	}
}

// Flush releases the frames held by a pending epoch.
func (c *Clock) Flush() []media.Frame {
	if c.pend != nil && len(c.pend.held) > 0 {
		c.release(c.expectVideo && !c.pend.hasKey)
	}
	return c.takeOut()
}

// Epochs returns the anchors of the last 64 released epochs, oldest first.
func (c *Clock) Epochs() []Epoch { return slices.Clone(c.epochs) }

// Stats returns the drop counters.
func (c *Clock) Stats() Stats { return c.stats }

func (c *Clock) takeOut() []media.Frame {
	out := c.out
	c.out = nil
	return out
}

func (c *Clock) kindOf(f media.Frame) media.Kind {
	if k := f.Kind(); k != 0 {
		return k
	}
	if f.Track < len(c.opt.Kinds) {
		return c.opt.Kinds[f.Track]
	}
	return 0
}

// isOld is rule 3's test: dts continues the track's previous timeline.
func (c *Clock) isOld(dts, last int64) bool {
	d := dts - last
	return d > -c.opt.MaxBackStep && d <= c.opt.MaxForward
}

// isJump is rule 2's test against a high-water mark.
func (c *Clock) isJump(dts, hw int64) bool {
	return dts > hw+c.opt.MaxForward || dts < hw-c.opt.MaxBackJump
}

// jumpsHeld is rule 8's test against the pending epoch's held frames.
func (c *Clock) jumpsHeld(dts int64) bool {
	lo, hi, ok := c.heldSpan(false)
	return ok && (dts > hi+c.opt.MaxForward || dts < lo-c.opt.MaxBackJump)
}

// heldSpan returns the smallest and largest held DTS, of audio frames only
// when audioOnly is set.
func (c *Clock) heldSpan(audioOnly bool) (lo, hi int64, ok bool) {
	for _, h := range c.pend.held {
		if audioOnly && c.kindOf(h.f) != media.KindAudio {
			continue
		}
		if !ok {
			lo, hi, ok = h.f.DTS, h.f.DTS, true
			continue
		}
		lo, hi = min(lo, h.f.DTS), max(hi, h.f.DTS)
	}
	return lo, hi, ok
}

// configChange applies rule 8: a new Config pointer acts like Break(), and
// restarts an already pending epoch, with f as the first frame.
func (c *Clock) configChange(f media.Frame) {
	if c.pend == nil {
		c.Break()
	} else {
		c.restart()
	}
	c.join(f)
}

// pushReleased handles a frame while no epoch is pending.
func (c *Clock) pushReleased(f media.Frame, t *track) {
	if c.isStale(f, t) {
		c.stats.DroppedStale++
		return
	}
	if c.isJump(f.DTS, c.hw) {
		c.openPending(true)
		c.join(f)
		return
	}
	c.acceptReleased(f, t)
}

// isStale reports whether f, from a track that has not joined the released
// epoch, is still on an earlier timeline. A track that joined before a restart
// but had no frame held at release gets rule 8's lag test: a jump against the
// epoch that continues its kept lastSourceDTS. Any other unjoined track gets
// rule 3's old test, in a jump epoch only.
func (c *Clock) isStale(f media.Frame, t *track) bool {
	te := &t.cur
	switch {
	case te.joined || !te.hasLast:
		return false
	case te.fresh:
		return c.isJump(f.DTS, c.hw) && c.isOld(f.DTS, te.last)
	default:
		return c.cur.jump && c.isOld(f.DTS, te.last)
	}
}

// acceptReleased applies rule 4 in the released epoch, then gates and maps f.
// It also handles old-timeline frames while a new epoch is pending.
func (c *Clock) acceptReleased(f media.Frame, t *track) {
	te := &t.cur
	if te.joined && te.hasLast && f.DTS <= te.last {
		c.stats.DroppedBackStep++
		return
	}
	te.joined, te.hasLast, te.last = true, true, f.DTS
	c.hw = max(c.hw, f.DTS)

	ep := c.cur
	kind := c.kindOf(f)
	switch kind {
	case media.KindVideo:
		switch te.gate {
		case gateKey:
			if !f.Key {
				c.stats.DroppedBeforeKey++
				return
			}
			if ep.audioOnly { // rule 6: the first keyframe after an audio-only release
				c.Break()
				c.join(f)
				return
			}
			if f.DTS < ep.base {
				c.stats.DroppedLate++ // the gate stays closed
				return
			}
			te.gate = gateRASL
			if !ep.hasKey || f.PTS < ep.keyPTS {
				ep.hasKey, ep.keyPTS = true, f.PTS
			}
		case gateRASL:
			if f.RASL {
				c.stats.DroppedRASL++
				return
			}
			te.gate = gateOpen
		case gateOpen:
		}
	case media.KindAudio:
		if !te.audioOut && ep.hasKey && f.PTS < ep.keyPTS {
			c.stats.DroppedEarlyAudio++
			return
		}
	}
	if f.DTS < ep.base {
		c.stats.DroppedLate++
		return
	}
	if kind == media.KindAudio {
		te.audioOut = true
	}
	c.emit(f, t, ep)
}

// pushPending handles a frame while an epoch is pending.
func (c *Clock) pushPending(f media.Frame, t *track) {
	te := &t.pend
	if !te.joined {
		if c.pend.jump && t.cur.hasLast && c.isOld(f.DTS, t.cur.last) {
			c.acceptReleased(f, t) // rule 3: old timeline, emitted with the released epoch
			return
		}
		if c.jumpsHeld(f.DTS) {
			c.restart()
		}
		c.join(f)
		return
	}
	if c.jumpsHeld(f.DTS) {
		if c.isOld(f.DTS, te.last) {
			c.stats.DroppedStale++ // rule 8: a joined track lagging on an earlier timeline
			return
		}
		c.restart()
		c.join(f)
		return
	}
	if !te.fresh && f.DTS <= te.last {
		c.stats.DroppedBackStep++
		return
	}
	c.join(f)
}

// join accepts f into the pending epoch without the rule 4 check, then gates
// and holds it.
func (c *Clock) join(f media.Frame) {
	p := c.pend
	t := c.tracks[f.Track]
	te := &t.pend
	te.joined, te.fresh, te.hasLast, te.last = true, false, true, f.DTS
	if !p.hasHW || f.DTS > p.hw {
		p.hw, p.hasHW = f.DTS, true
	}

	switch c.kindOf(f) {
	case media.KindVideo:
		switch te.gate {
		case gateKey:
			if !f.Key {
				c.stats.DroppedBeforeKey++
				return
			}
			te.gate = gateRASL
			if !p.hasAnchor {
				p.anchor, p.hasAnchor = f.DTS, true
			}
			if !p.hasKey || f.PTS < p.keyPTS {
				p.hasKey, p.keyPTS = true, f.PTS
				c.dropEarlyAudio()
			}
		case gateRASL:
			if f.RASL {
				c.stats.DroppedRASL++
				return
			}
			te.gate = gateOpen
		case gateOpen:
		}
	case media.KindAudio:
		if p.hasKey && f.PTS < p.keyPTS {
			c.stats.DroppedEarlyAudio++
			return
		}
	}

	p.held = append(p.held, heldFrame{f: f, at: c.opt.Now()})
	te.held++
	c.tryRelease()
}

// dropEarlyAudio removes held audio presented before keyPTS (rule 5).
func (c *Clock) dropEarlyAudio() {
	p := c.pend
	kept := p.held[:0]
	for _, h := range p.held {
		if c.kindOf(h.f) == media.KindAudio && h.f.PTS < p.keyPTS {
			c.stats.DroppedEarlyAudio++
			c.tracks[h.f.Track].pend.held--
			continue
		}
		kept = append(kept, h)
	}
	clear(p.held[len(kept):])
	p.held = kept
}

// tryRelease releases the pending epoch when a rule 6 condition holds.
func (c *Clock) tryRelease() {
	p := c.pend
	if c.allTracksHeld() {
		c.release(false)
		return
	}
	_, hi, _ := c.heldSpan(false)
	switch {
	case p.hasAnchor:
		if hi-p.anchor >= c.opt.AlignWindow {
			c.release(false)
		}
	case !c.expectVideo:
		if hi-p.held[0].f.DTS >= c.opt.AlignWindow {
			c.release(false)
		}
	default: // video expected, no keyframe yet
		if lo, hi, ok := c.heldSpan(true); ok && hi-lo >= c.opt.VideoWait {
			c.release(true)
		}
	}
}

func (c *Clock) allTracksHeld() bool {
	if len(c.opt.Kinds) == 0 {
		return false
	}
	for i := range c.opt.Kinds {
		if i >= len(c.tracks) || c.tracks[i].pend.held == 0 {
			return false
		}
	}
	return true
}

// release turns the pending epoch into the released one and emits its held
// frames in arrival order (rule 6).
func (c *Clock) release(audioOnly bool) {
	p := c.pend
	base, _, _ := c.heldSpan(false)
	ep := &epoch{
		id: p.id, base: base, start: c.nextStart(p.id),
		hasKey: p.hasKey, keyPTS: p.keyPTS, jump: p.jump, audioOnly: audioOnly,
	}
	c.record(Epoch{ID: ep.id, Start: ep.start, Wall: p.held[0].at})
	for _, t := range c.tracks {
		switch {
		case t.pend.fresh:
			// Joined before a restart, nothing since: its next frame rejoins
			// without the rule 4 check, after the lag test against the kept
			// lastSourceDTS.
			t.cur = trackEpoch{fresh: true, hasLast: true, last: t.pend.last}
		case t.pend.joined:
			t.cur = t.pend
			t.cur.held = 0
		default:
			// Not joined: keep the previous lastSourceDTS for rule 3.
			t.cur = trackEpoch{hasLast: t.cur.hasLast, last: t.cur.last}
		}
		t.pend = trackEpoch{}
	}
	c.cur, c.hw, c.pend = ep, p.hw, nil
	for _, h := range p.held {
		t := c.tracks[h.f.Track]
		if c.kindOf(h.f) == media.KindAudio {
			t.cur.audioOut = true
		}
		c.emit(h.f, t, ep)
	}
}

// nextStart is rule 6's Start: 0 for epoch 1, otherwise the latest
// presentation end over all tracks. Taking each track's largest PTS ever
// equals taking it over the previous epoch, because every epoch starts after
// all earlier presentation ends.
func (c *Clock) nextStart(id uint32) int64 {
	if id == 1 {
		return 0
	}
	var start int64
	for _, t := range c.tracks {
		if t.emitted {
			start = max(start, t.maxPTS+t.duration())
		}
	}
	return start
}

func (c *Clock) record(e Epoch) {
	if len(c.epochs) == maxEpochs {
		copy(c.epochs, c.epochs[1:])
		c.epochs = c.epochs[:maxEpochs-1]
	}
	c.epochs = append(c.epochs, e)
}

// openPending opens the next epoch. Every track starts unjoined.
func (c *Clock) openPending(jump bool) {
	c.pend = &pending{id: c.cur.id + 1, jump: jump}
	for _, t := range c.tracks {
		t.pend = trackEpoch{}
	}
}

// restart drops the pending epoch's held frames and starts it again (rule 8).
// Joined tracks stay joined and keep lastSourceDTS; their next accepted frame
// rejoins the restarted epoch.
func (c *Clock) restart() {
	p := c.pend
	c.stats.DroppedStale += uint64(len(p.held))
	clear(p.held)
	*p = pending{id: p.id, jump: p.jump, held: p.held[:0]}
	for _, t := range c.tracks {
		t.pend.held, t.pend.gate, t.pend.audioOut = 0, gateKey, false
		if t.pend.joined {
			t.pend.fresh = true
		}
	}
}

// emit maps f into ep (rule 7) and appends it to the output.
func (c *Clock) emit(f media.Frame, t *track, ep *epoch) {
	off := ep.start - ep.base
	f.PTS += off
	f.DTS += off
	f.Epoch = ep.id
	if t.emitted && t.lastEmitEpoch == ep.id {
		t.lastDur = f.DTS - t.lastEmitDTS
	}
	if !t.emitted || f.PTS > t.maxPTS {
		t.maxPTS = f.PTS
	}
	t.emitted, t.lastEmitEpoch, t.lastEmitDTS = true, ep.id, f.DTS
	c.out = append(c.out, f)
}
