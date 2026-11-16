// Package clock turns source timestamps into stream time and assigns epochs.
//
// One Clock sits between a stream's demuxer and its hub. Source timestamps
// come in already unwrapped (int64, 90 kHz, possibly negative). Within an
// epoch, stream = source − base + Start, so deltas are kept exactly. An epoch
// starts on a video keyframe with audio aligned to it, after the previous
// epoch's presentation end, and Clock is the only place time is rewritten.
//
// A track that was not in an epoch when it was released and shows up later
// opens the next epoch at the next video keyframe (a late join), at most once
// per input; an epoch opened by a configuration change holds its whole align
// window, so staggered changes on several tracks are one epoch change.
//
// For a live source, the Clock also compares each epoch's decode timing with
// the frames' arrival times, and re-anchors the wall when the source's clock
// drifts from arrival by more than Options.MaxDrift (see Epoch).
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
	defaultDuration    = 3600                // lastDuration when a track has no DTS delta yet
	lateMargin         = media.ClockRate / 5 // added to a late track's lateness for its hold
	maxEpochs          = 64
	driftWindow        = 10 // seconds of arrivals in the drift baseline and in each drift sample, at least
	driftSamples       = 3  // distinct arrival instants a drift sample needs
)

// Options tunes a Clock. Zero values select the defaults.
type Options struct {
	Kinds       []media.Kind     // kind of each expected track, by index (from TrackKinds)
	AlignWindow int64            // source ticks held at an epoch start (raised by late-join holds); 0 means 90000
	MaxForward  int64            // forward jump past the high-water mark that opens an epoch; 0 means 5*90000
	MaxBackJump int64            // backward jump below the high-water mark that opens an epoch; 0 means 2*90000
	MaxBackStep int64            // per-track continuity tolerance while an epoch is pending; 0 means 45000
	VideoWait   int64            // held audio span before releasing without a video keyframe; 0 means 5*90000
	Now         func() time.Time // nil means time.Now

	// MaxDrift re-anchors the wall when the source's clock drifts from
	// arrival by more than it (see Epoch); 0 disables. It must exceed
	// media.MaxAnchorLead: a value in (0, MaxAnchorLead] is raised to
	// 2 × MaxAnchorLead.
	MaxDrift time.Duration
	// DriftPositiveOnly reacts only to a source falling behind arrival, for
	// paced inputs, which cannot run ahead.
	DriftPositiveOnly bool
	// ArrivalGranularity is the step between the source's arrivals when it
	// delivers in chunks (an HLS segment arrives whole); 0 means per frame.
	// The drift window is max(10 s, 4 × ArrivalGranularity) (see Epoch).
	ArrivalGranularity time.Duration
}

// Epoch is the anchor of one timeline. Its smallest DTS maps to stream time
// Start, presented at wallclock Wall.
//
// Wall is the epoch's effective wall, a whole millisecond. Epoch 1's is its
// arrival A_1, rounded up: the arrival time (Now() during its Push) of the
// first frame the epoch released, that is, the earliest-arriving held frame
// that survived the start gates and the early-audio drop. For a later epoch
// n, with Δ = Start_n − Start_{n−1} (the previous epoch's presentation span)
// and L = media.MaxAnchorLead,
//
//	R_n    = max(A_n, R_{n−1} + D_{n−1})     R_1 = A_1
//	Wall_n = ceil_ms(max(A_n, min(Wall_{n−1} + Δ, R_n + L)))
//
// then raised to Wall_{n−1} + 1 ms if below it. D_{n−1} is the previous
// epoch's DTS span, its largest stream DTS minus its Start, so R_n is where
// the source's own decode timing puts the epoch: it runs ahead of arrival
// only when the source delivers faster than real time (a file read unpaced,
// or encoder drift, which is left as it is). Wall_{n−1} + Δ is the earliest
// wall at which the epoch does not overlap the previous one's presentation;
// it exceeds R_n by the previous epoch's presentation-end excess (B-frame
// reordering and the last frame's duration). Taking it, up to L past R_n,
// keeps epochs from overlapping, and a pipeline that paces on Wall waits the
// excess out, so the next arrival starts from Wall again and nothing
// accumulates. An unpaced live source cannot wait: its excess would add up
// epoch after epoch, so the lead over R_n is capped at L, and beyond it the
// epoch overlaps the previous one's tail instead. Walls are rounded on their
// own values, so rounding never accumulates either.
//
// A live source whose clock drifts from arrival is re-anchored by a drift
// epoch (Options.MaxDrift, Clock.SetDrift). Within each epoch, a released
// frame's offset is off = arrival − (DTS − Start). The baseline is the
// smallest offset over the epoch's first window of arrivals: bursts and
// network delay only raise an offset above the true one, and a chain lead is
// constant within the epoch. Once per second of arrivals after that, the
// drift is the smallest offset over the last window minus the baseline,
// provided the window holds at least three distinct arrival instants. The
// window is max(10 s, 4 × Options.ArrivalGranularity): a source delivering
// in chunks (6 s HLS segments) has few arrivals per window, and one slow
// fetch must not be the only one in it. Four steps, not three, so that the
// window strictly holds three arrivals even when a source falling behind
// spaces them by more than a step (the current, unclosed second is not in
// the window). When the drift
// exceeds MaxDrift, or falls below −MaxDrift and DriftPositiveOnly is unset,
// the next video keyframe (any frame for a stream without video) opens a
// drift epoch. With b = baseline − Wall_{n−1}, the previous epoch's constant
// offset (burst, chain lead, first-frame delay), the drift epoch drops its
// frames with arrival − b before Wall_{n−1} + Δ (Stats.DroppedDrift), so
// epochs never overlap, and
//
//	Wall_n = R_n = ceil_ms(arrival_key − b)
//
// raised to ceil_ms(Wall_{n−1} + Δ) if below it, where arrival_key is the
// arrival of its first held video keyframe (its first held frame without
// video). The constant offset is kept and only the drift is removed. A source
// that fell behind (positive drift) leaves a wall gap of about the drift
// between the two epochs; one that ran ahead drops about the drift, plus the
// frames up to its next keyframe (Stats.DroppedBeforeKey).
type Epoch struct {
	ID    uint32
	Start int64
	Wall  time.Time
}

// Stats counts dropped frames by reason, and late tracks.
type Stats struct {
	DroppedBackStep, DroppedBeforeKey, DroppedRASL, DroppedEarlyAudio, DroppedStale, DroppedLate uint64
	// DroppedInvalid counts input frames with PTS below DTS, dropped on entry
	// so that every output frame has PTS >= DTS.
	DroppedInvalid uint64
	// LateJoins counts epochs opened by a late-joining track. LateTracks
	// counts late tracks that joined their epoch late instead, because they
	// had already opened a late-join epoch on this input.
	LateJoins, LateTracks uint64
	// DriftBreaks counts drift epochs. DroppedDrift counts the frames a drift
	// epoch dropped so as not to overlap the previous epoch.
	DriftBreaks, DroppedDrift uint64
	// DriftEvaluations counts drift evaluations (see Epoch): zero when drift
	// is disabled or no window held enough arrivals to judge.
	DriftEvaluations uint64
}

// gate is a video track's start-of-epoch gate (rule 5).
type gate uint8

const (
	gateKey  gate = iota // dropping frames until the first keyframe
	gateRASL             // dropping RASL frames until the first trailing picture
	gateOpen
)

// cause is why a pending epoch was opened.
type cause uint8

const (
	causeBreak  cause = iota // New, Break, or a keyframe after an audio-only release
	causeJump                // a jump against the high-water mark (rule 2)
	causeLate                // a late-joining track
	causeConfig              // a configuration change (rule 8)
	causeDrift               // the released epoch's drift exceeded MaxDrift
)

// reanchors reports whether tracks running in the released epoch keep
// emitting there until they join the pending epoch at their next video
// keyframe (any frame for audio-only streams): the epoch re-anchors a
// timeline that continues.
func (w cause) reanchors() bool {
	switch w {
	case causeLate, causeDrift:
		return true
	case causeBreak, causeJump, causeConfig:
	}
	return false
}

// driftCfg is the drift setting of one epoch.
type driftCfg struct {
	max          time.Duration // 0 disables
	positiveOnly bool
	window       int64 // seconds of arrivals in the baseline and in each sample
}

// newDriftCfg applies the MaxDrift clamp and sizes the window from the
// arrival granularity: max(10 s, 4 × granularity), rounded up to whole
// seconds.
func newDriftCfg(maxDrift time.Duration, positiveOnly bool, granularity time.Duration) driftCfg {
	switch {
	case maxDrift <= 0:
		maxDrift = 0
	case maxDrift <= media.MaxAnchorLead:
		maxDrift = 2 * media.MaxAnchorLead
	}
	window := int64(driftWindow)
	if g := max(granularity, 0); g > 0 {
		window = max(window, int64((4*g+time.Second-1)/time.Second))
	}
	return driftCfg{max: maxDrift, positiveOnly: positiveOnly, window: window}
}

// driftSec is the smallest offset among the frames released in one second of
// arrivals, and the number of distinct arrival instants among them.
type driftSec struct {
	sec int64
	min time.Time
	n   int
	ok  bool
}

// driftState measures a released epoch's drift (see Epoch). Arrivals are
// counted in whole seconds from the epoch's first released frame; an arrival
// earlier than the current second counts in it.
type driftState struct {
	driftCfg
	started  bool
	t0       time.Time  // arrival of the epoch's first released frame
	sec      int64      // the current second
	secMin   time.Time  // smallest offset in it
	secN     int        // distinct arrival instants in it
	latest   time.Time  // the latest arrival: an instant counts once, when first seen
	recent   []driftSec // ring of window seconds
	hasBase  bool
	baseline time.Time     // smallest offset over the first driftWindow seconds
	evals    int           // drift evaluations so far
	last     time.Duration // the last drift evaluated
	due      bool          // the drift exceeded max: the next keyframe opens a drift epoch
}

// observe records the offset of a frame released at arrival at.
func (d *driftState) observe(at, off time.Time) {
	if d.max == 0 || d.due {
		return
	}
	if !d.started {
		d.started, d.t0, d.sec, d.secMin, d.secN, d.latest = true, at, 0, off, 1, at
		d.recent = make([]driftSec, d.window)
		return
	}
	fresh := at.After(d.latest)
	if fresh {
		d.latest = at
	}
	sec := int64(at.Sub(d.t0) / time.Second)
	if sec <= d.sec {
		if off.Before(d.secMin) {
			d.secMin = off
		}
		if fresh {
			d.secN++
		}
		return
	}
	d.closeSec()
	d.sec, d.secMin, d.secN = sec, off, 1
	if sec > d.window {
		d.evaluate()
	}
}

// closeSec stores the current second, in the baseline too while it is one of
// the first window seconds.
func (d *driftState) closeSec() {
	if d.sec < d.window && (!d.hasBase || d.secMin.Before(d.baseline)) {
		d.hasBase, d.baseline = true, d.secMin
	}
	d.recent[d.sec%d.window] = driftSec{sec: d.sec, min: d.secMin, n: d.secN, ok: true}
}

// evaluate computes the drift over the window seconds before the current
// one, and sets due when it is out of bounds. A window with fewer than
// driftSamples distinct arrivals is not evaluated.
func (d *driftState) evaluate() {
	var lo time.Time
	found, n := false, 0
	for _, s := range d.recent {
		if !s.ok || s.sec < d.sec-d.window {
			continue
		}
		n += s.n
		if !found || s.min.Before(lo) {
			lo, found = s.min, true
		}
	}
	if !found || !d.hasBase || n < driftSamples {
		return
	}
	drift := lo.Sub(d.baseline)
	d.evals++
	d.last = drift
	if drift > d.max || (drift < -d.max && !d.positiveOnly) {
		d.due = true
	}
}

// trackEpoch is one track's state within one epoch.
type trackEpoch struct {
	joined   bool
	atStart  bool  // in the released epoch only: joined its pending epoch before release
	fresh    bool  // joined before a restart: the next accepted frame rejoins
	hasLast  bool  // last is set
	last     int64 // lastSourceDTS: source DTS of the previous accepted frame
	gate     gate
	keyPTS   int64 // source PTS of the keyframe that passed gateKey
	held     int   // frames of this track held by the pending epoch
	audioOut bool  // an audio frame of this track was emitted in this epoch
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

	// Late join: the hold a pending epoch keeps for this track, raised by
	// each late join and never lowered; and whether it opened a late-join
	// epoch since the last Break(true).
	lateHold int64
	lateUsed bool
}

func (t *track) duration() int64 {
	if t.lastDur > 0 {
		return t.lastDur
	}
	return defaultDuration
}

// emittedIn reports whether the track has emitted a frame in ep.
func (t *track) emittedIn(ep *epoch) bool {
	return t.emitted && t.lastEmitEpoch == ep.id
}

// epoch is a released epoch.
type epoch struct {
	id          uint32
	base, start int64
	wall        time.Time // effective wall (Epoch.Wall)
	raw         time.Time // R_n: arrival carried by the source's decode timing
	hasDTS      bool
	maxDTS      int64 // largest stream DTS emitted in the epoch
	hasKey      bool
	keyPTS      int64
	anchor      int64 // DTS of its first video keyframe, or base without one
	jump        bool  // opened by a jump: unjoined tracks get rule 3's old test
	audioOnly   bool  // released by VideoWait: the first keyframe opens the next epoch
	drift       driftState
}

// heldFrame is a frame held by a pending epoch, in source time.
type heldFrame struct {
	f  media.Frame
	at time.Time // arrival, as given to PushAt
}

// pending is an epoch that holds frames until it is released.
type pending struct {
	id        uint32
	cause     cause
	held      []heldFrame // arrival order
	hasKey    bool
	keyPTS    int64 // smallest first-keyframe PTS among video tracks
	hasAnchor bool
	anchor    int64     // DTS of the first video keyframe held
	anchorAt  time.Time // its arrival
	hasHW     bool
	hw        int64 // largest source DTS accepted
	drift     driftCfg

	// A drift epoch's reset (see Epoch), cleared by a restart: the PTS of
	// the frame that opened it, before which audio stays in the released
	// epoch, and b, the released epoch's constant offset.
	reset  bool
	cut    int64
	offset time.Duration
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
	drift       driftCfg  // for epochs opened from now on
	now         time.Time // arrival of the frame being pushed
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
	drift := newDriftCfg(opt.MaxDrift, opt.DriftPositiveOnly, opt.ArrivalGranularity)
	opt.MaxDrift = drift.max
	return &Clock{
		opt:         opt,
		expectVideo: slices.Contains(opt.Kinds, media.KindVideo),
		pend:        &pending{id: 1, drift: drift},
		drift:       drift,
	}
}

func orDefault(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// Push is PushAt(f, Now()).
func (c *Clock) Push(f media.Frame) []media.Frame {
	return c.PushAt(f, c.opt.Now())
}

// PushAt takes one frame in source time, which arrived at arrival, and
// returns the frames it releases, in stream time and arrival order. f.Track
// must not be negative. A frame with PTS below DTS is dropped and counted in
// Stats.DroppedInvalid.
func (c *Clock) PushAt(f media.Frame, arrival time.Time) []media.Frame {
	c.now = arrival
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
		c.configChange(f, t)
	case c.pend != nil:
		c.pushPending(f, t)
	default:
		c.pushReleased(f, t)
	}
	return c.takeOut()
}

// Break makes the next frames open a new epoch that every track joins with
// its next frame. While an epoch is pending, Break restarts it: the frames it
// holds came from before the break and are dropped (Stats.DroppedStale).
// newInput reports an input switch: it lets every track open one late-join
// epoch again. A loop or a reader's discontinuity passes false.
func (c *Clock) Break(newInput bool) {
	if newInput {
		for _, t := range c.tracks {
			t.lateUsed = false
		}
	}
	if c.pend == nil {
		c.openPending(causeBreak)
		return
	}
	c.restart()
	c.pend.cause = causeBreak
}

// SetDrift sets MaxDrift, DriftPositiveOnly and ArrivalGranularity, with
// the same clamp. It applies to the pending epoch when one is pending (always
// after New and right after Break), else from the next epoch opened.
func (c *Clock) SetDrift(maxDrift time.Duration, positiveOnly bool, granularity time.Duration) {
	c.drift = newDriftCfg(maxDrift, positiveOnly, granularity)
	if c.pend != nil {
		c.pend.drift = c.drift
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

// configChange applies rule 8: a new Config pointer opens a pending epoch
// that f joins as its track's first frame. When an epoch is already pending,
// f joins it. If f is a jump against the held frames, the epoch restarts
// first. If f's track has frames held under its old config, an epoch opened
// by a configuration change drops just those (it holds its whole window, so
// a staggered change on another track lands inside it); any other pending
// epoch restarts. So a failover or PMT change that gives every track a new
// pointer is one epoch change, and the tracks that change later do not drop
// what the others hold.
func (c *Clock) configChange(f media.Frame, t *track) {
	switch {
	case c.pend == nil:
		c.openPending(causeConfig)
	case c.jumpsHeld(f.DTS):
		c.restart()
	case t.pend.held > 0 && c.pend.cause == causeConfig:
		c.dropTrack(f.Track)
	case t.pend.held > 0:
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
	if c.isJump(f.DTS, c.hw) && !c.continues(f, t) {
		c.openPending(causeJump)
		c.join(f)
		return
	}
	c.acceptReleased(f, t)
}

// continues reports whether f continues its track's own timeline in the
// released epoch (rule 2): such a frame is never a jump, so a constant skew
// between tracks above MaxBackJump does not open epochs. A real loop or jump
// breaks the track's own continuity too.
func (c *Clock) continues(f media.Frame, t *track) bool {
	return t.cur.joined && c.isOld(f.DTS, t.cur.last)
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
// It also handles old-timeline frames while a new epoch is pending, and the
// frames a running track keeps emitting while a late-join epoch is pending.
func (c *Clock) acceptReleased(f media.Frame, t *track) {
	te := &t.cur
	if te.joined && te.hasLast && f.DTS <= te.last {
		c.stats.DroppedBackStep++
		return
	}
	te.joined, te.hasLast, te.last = true, true, f.DTS
	c.hw = max(c.hw, f.DTS)

	ep := c.cur
	if c.pend == nil && ep.drift.due && c.opensDrift(f) {
		c.openDrift(f)
		c.join(f)
		return
	}
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
				c.Break(false)
				c.join(f)
				return
			}
			if f.DTS < ep.base {
				c.stats.DroppedLate++ // the gate stays closed
				return
			}
			te.gate, te.keyPTS = gateRASL, f.PTS
			if !ep.hasKey || f.PTS < ep.keyPTS {
				ep.hasKey, ep.keyPTS = true, f.PTS
			}
		case gateRASL:
			if c.dropRASL(f, te) {
				return
			}
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
	if c.pend == nil && !te.atStart && !t.emittedIn(ep) && c.lateJoin(f, t, ep) {
		return
	}
	if kind == media.KindAudio {
		te.audioOut = true
	}
	c.emit(f, t, ep, c.now)
}

// opensDrift reports whether f opens the drift epoch that the released epoch
// is due: a video keyframe, or any frame of a stream without video. After an
// audio-only release the first keyframe opens an epoch anyway (rule 6).
func (c *Clock) opensDrift(f media.Frame) bool {
	if !c.expectVideo {
		return true
	}
	return c.kindOf(f) == media.KindVideo && f.Key && !c.cur.audioOnly
}

// openDrift opens a drift epoch at f (see Epoch).
func (c *Clock) openDrift(f media.Frame) {
	prev := c.cur
	c.stats.DriftBreaks++
	c.openPending(causeDrift)
	p := c.pend
	p.reset, p.cut, p.offset = true, f.PTS, prev.drift.baseline.Sub(prev.wall)
}

// driftEnd is the previous epoch's presentation end on its wall,
// Wall_{n−1} + Δ, so far.
func (c *Clock) driftEnd() time.Time {
	return c.cur.wall.Add(media.ToDuration(c.nextStart(c.pend.id) - c.cur.start))
}

// lateJoin handles f, the first frame of a track that was not in the released
// epoch ep when it was released. Unless the track already opened a late-join
// epoch on this input, f opens a new pending epoch, which tracks running in
// ep join at their next video keyframe (any frame for audio-only streams), and
// lateJoin reports true. The track's hold is raised to its lateness, from the
// epoch's keyframe DTS to f's DTS, plus 200 ms, capped at VideoWait.
// Otherwise f joins ep late (Stats.LateTracks) and lateJoin reports false.
func (c *Clock) lateJoin(f media.Frame, t *track, ep *epoch) bool {
	if t.lateUsed {
		c.stats.LateTracks++
		return false
	}
	t.lateUsed = true
	t.lateHold = min(max(t.lateHold, f.DTS-ep.anchor+lateMargin), c.opt.VideoWait)
	c.stats.LateJoins++
	c.openPending(causeLate)
	c.join(f)
	return true
}

// pushPending handles a frame while an epoch is pending.
func (c *Clock) pushPending(f media.Frame, t *track) {
	te := &t.pend
	if !te.joined {
		if c.keepsReleased(f, t) {
			c.acceptReleased(f, t)
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

// keepsReleased reports whether f, from a track that has not joined the
// pending epoch, goes to the released epoch instead: in a jump epoch, a frame
// on the old timeline (rule 3); in a late-join epoch, a video frame before
// its track's keyframe, from a track running in the released epoch.
func (c *Clock) keepsReleased(f media.Frame, t *track) bool {
	switch c.pend.cause {
	case causeJump:
		return t.cur.hasLast && c.isOld(f.DTS, t.cur.last)
	case causeLate, causeDrift:
		return c.kindOf(f) == media.KindVideo && !f.Key && t.emittedIn(c.cur) && c.continues(f, t)
	case causeBreak, causeConfig:
	}
	return false
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
	if p.reset {
		if c.kindOf(f) == media.KindAudio && f.PTS < p.cut {
			c.earlyAudio(f)
			return
		}
		if c.now.Add(-p.offset).Before(c.driftEnd()) {
			c.stats.DroppedDrift++ // its gates stay as they were
			return
		}
	}

	switch c.kindOf(f) {
	case media.KindVideo:
		switch te.gate {
		case gateKey:
			if !f.Key {
				c.stats.DroppedBeforeKey++
				return
			}
			te.gate, te.keyPTS = gateRASL, f.PTS
			if !p.hasAnchor {
				p.anchor, p.anchorAt, p.hasAnchor = f.DTS, c.now, true
			}
			if !p.hasKey || f.PTS < p.keyPTS {
				p.hasKey, p.keyPTS = true, f.PTS
				c.dropEarlyAudio()
			}
		case gateRASL:
			if c.dropRASL(f, te) {
				return
			}
		case gateOpen:
		}
	case media.KindAudio:
		if p.hasKey && f.PTS < p.keyPTS {
			c.earlyAudio(f)
			return
		}
	}

	p.held = append(p.held, heldFrame{f: f, at: c.now})
	te.held++
	c.tryRelease()
}

// dropRASL applies rule 5's RASL gate to f, a video frame after its track's
// gating keyframe, and reports whether f is dropped. Leading pictures (RASL
// and RADL) are presented before that keyframe, so the gate ends at the first
// trailing picture: a non-RASL frame with PTS above the keyframe's. A RADL
// before it is kept and leaves the gate in place.
func (c *Clock) dropRASL(f media.Frame, te *trackEpoch) bool {
	if f.RASL {
		c.stats.DroppedRASL++
		return true
	}
	if f.PTS > te.keyPTS {
		te.gate = gateOpen
	}
	return false
}

// dropEarlyAudio removes held audio presented before keyPTS (rule 5).
func (c *Clock) dropEarlyAudio() {
	p := c.pend
	kept := p.held[:0]
	for _, h := range p.held {
		if c.kindOf(h.f) == media.KindAudio && h.f.PTS < p.keyPTS {
			c.tracks[h.f.Track].pend.held--
			c.earlyAudio(h.f)
			continue
		}
		kept = append(kept, h)
	}
	clear(p.held[len(kept):])
	p.held = kept
}

// earlyAudio handles an audio frame presented before the pending epoch's
// keyframe. In a late-join or drift epoch, a track running in the released
// epoch emits it there, since its frames up to that keyframe belong to it; any
// other is dropped (rule 5). A drift epoch whose reset holds keeps only audio
// before the frame that opened it: the frames after it are in the new epoch,
// even when the drop dropped that frame.
func (c *Clock) earlyAudio(f media.Frame) {
	t := c.tracks[f.Track]
	p := c.pend
	if p.cause.reanchors() && (!p.reset || f.PTS < p.cut) && t.emittedIn(c.cur) {
		c.acceptReleased(f, t)
		return
	}
	c.stats.DroppedEarlyAudio++
}

// dropTrack drops the frames of one track held by the pending epoch (its
// frames under an old config) and closes the track's start gates again.
func (c *Clock) dropTrack(track int) {
	p := c.pend
	kept := p.held[:0]
	for _, h := range p.held {
		if h.f.Track == track {
			c.stats.DroppedStale++
			continue
		}
		kept = append(kept, h)
	}
	clear(p.held[len(kept):])
	p.held = kept
	te := &c.tracks[track].pend
	te.held, te.gate, te.audioOut = 0, gateKey, false

	// The keyframe and anchor come from the video tracks still held: each
	// one's first held frame is its gating keyframe.
	p.hasKey, p.hasAnchor = false, false
	for _, h := range p.held {
		if c.kindOf(h.f) == media.KindVideo {
			p.anchor, p.anchorAt, p.hasAnchor = h.f.DTS, h.at, true
			break
		}
	}
	for _, t := range c.tracks {
		if pe := &t.pend; pe.held > 0 && pe.gate != gateKey && (!p.hasKey || pe.keyPTS < p.keyPTS) {
			p.hasKey, p.keyPTS = true, pe.keyPTS
		}
	}
	if p.hasKey {
		c.dropEarlyAudio()
	}
}

// tryRelease releases the pending epoch when a rule 6 condition holds. An
// epoch opened by a configuration change does not release early when every
// track has a frame: it holds its whole window. A late-join epoch also waits
// past its window for the tracks running in the released epoch, up to
// VideoWait from its keyframe, so a running video track whose keyframe is
// further than the window keeps its GOP.
func (c *Clock) tryRelease() {
	p := c.pend
	if p.cause != causeConfig && c.allTracksHeld() {
		c.release(false)
		return
	}
	_, hi, _ := c.heldSpan(false)
	w := c.window()
	switch {
	case p.hasAnchor:
		if span := hi - p.anchor; span >= w && (span >= c.opt.VideoWait || !c.awaitsRunning()) {
			c.release(false)
		}
	case !c.expectVideo:
		if hi-p.held[0].f.DTS >= w {
			c.release(false)
		}
	default: // video expected, no keyframe yet
		if lo, hi, ok := c.heldSpan(true); ok && hi-lo >= c.opt.VideoWait {
			c.release(true)
		}
	}
}

// awaitsRunning reports whether a late-join or drift epoch holds no frame yet
// of a track running in the released epoch.
func (c *Clock) awaitsRunning() bool {
	if !c.pend.cause.reanchors() {
		return false
	}
	for _, t := range c.tracks {
		if t.pend.held == 0 && t.emittedIn(c.cur) {
			return true
		}
	}
	return false
}

// window is the source span a pending epoch holds: AlignWindow, raised to
// the late-join hold of each track it holds no frame of.
func (c *Clock) window() int64 {
	w := c.opt.AlignWindow
	for _, t := range c.tracks {
		if t.pend.held == 0 {
			w = max(w, t.lateHold)
		}
	}
	return w
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
		id: p.id, base: base, start: c.nextStart(p.id), anchor: base,
		hasKey: p.hasKey, keyPTS: p.keyPTS, jump: p.cause == causeJump, audioOnly: audioOnly,
		drift: driftState{driftCfg: p.drift},
	}
	if p.hasAnchor {
		ep.anchor = p.anchor
	}
	if p.reset {
		ep.wall = c.placeDrift(p, ep.start)
		ep.raw = ep.wall
	} else {
		ep.wall, ep.raw = c.placeWall(p.held[0].at, ep.start)
	}
	c.record(Epoch{ID: ep.id, Start: ep.start, Wall: ep.wall})
	for _, t := range c.tracks {
		switch {
		case t.pend.fresh:
			// Joined before a restart, nothing since: its next frame rejoins
			// without the rule 4 check, after the lag test against the kept
			// lastSourceDTS.
			t.cur = trackEpoch{atStart: true, fresh: true, hasLast: true, last: t.pend.last}
		case t.pend.joined:
			t.cur = t.pend
			t.cur.held, t.cur.atStart = 0, true
		default:
			// Not joined: keep the previous lastSourceDTS for rule 3. Its
			// first frame in the epoch is a late join, unless it was running
			// in the previous epoch and a late-join or drift epoch released
			// (by VideoWait) before its keyframe: it is present, not late.
			running := p.cause.reanchors() && t.emittedIn(c.cur)
			t.cur = trackEpoch{atStart: running, hasLast: t.cur.hasLast, last: t.cur.last}
		}
		t.pend = trackEpoch{}
	}
	c.cur, c.hw, c.pend = ep, p.hw, nil
	for _, h := range p.held {
		t := c.tracks[h.f.Track]
		if c.kindOf(h.f) == media.KindAudio {
			t.cur.audioOut = true
		}
		c.emit(h.f, t, ep, h.at)
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

// placeWall returns the effective wall and R of an epoch that starts at
// stream time start and whose first frame arrived at arrival (see Epoch).
func (c *Clock) placeWall(arrival time.Time, start int64) (wall, raw time.Time) {
	prev := c.cur
	if prev == nil {
		return ceilMS(arrival), arrival
	}
	raw = arrival
	if prev.hasDTS {
		raw = later(raw, prev.raw.Add(media.ToDuration(prev.maxDTS-prev.start)))
	}
	chain := prev.wall.Add(media.ToDuration(start - prev.start))
	if limit := raw.Add(media.MaxAnchorLead); chain.After(limit) {
		chain = limit
	}
	wall = ceilMS(later(arrival, chain))
	return later(wall, prev.wall.Add(time.Millisecond)), raw
}

// placeDrift returns the wall of a drift epoch p that starts at stream time
// start (see Epoch); R is the same value.
func (c *Clock) placeDrift(p *pending, start int64) time.Time {
	prev := c.cur
	at := p.held[0].at
	if p.hasAnchor {
		at = p.anchorAt
	}
	end := ceilMS(prev.wall.Add(media.ToDuration(start - prev.start)))
	return later(ceilMS(at.Add(-p.offset)), end)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// ceilMS rounds t up to a whole millisecond.
func ceilMS(t time.Time) time.Time {
	r := t.Truncate(time.Millisecond)
	if r.Before(t) {
		r = r.Add(time.Millisecond)
	}
	return r
}

func (c *Clock) record(e Epoch) {
	if len(c.epochs) == maxEpochs {
		copy(c.epochs, c.epochs[1:])
		c.epochs = c.epochs[:maxEpochs-1]
	}
	c.epochs = append(c.epochs, e)
}

// openPending opens the next epoch. Every track starts unjoined.
func (c *Clock) openPending(why cause) {
	c.pend = &pending{id: c.cur.id + 1, cause: why, drift: c.drift}
	for _, t := range c.tracks {
		t.pend = trackEpoch{}
	}
}

// restart drops the pending epoch's held frames and starts it again (rule 8).
// Joined tracks stay joined and keep lastSourceDTS; their next accepted frame
// rejoins the restarted epoch. A restarted drift epoch loses its reset: the
// frames that restarted it are on another timeline, so it is placed by the
// ordinary rule.
func (c *Clock) restart() {
	p := c.pend
	c.stats.DroppedStale += uint64(len(p.held))
	clear(p.held)
	*p = pending{id: p.id, cause: p.cause, held: p.held[:0], drift: p.drift}
	for _, t := range c.tracks {
		t.pend.held, t.pend.gate, t.pend.audioOut = 0, gateKey, false
		if t.pend.joined {
			t.pend.fresh = true
		}
	}
}

// emit maps f, which arrived at at, into ep (rule 7), measures ep's drift,
// and appends f to the output.
func (c *Clock) emit(f media.Frame, t *track, ep *epoch, at time.Time) {
	off := ep.start - ep.base
	f.PTS += off
	f.DTS += off
	f.Epoch = ep.id
	evals := ep.drift.evals
	ep.drift.observe(at, at.Add(-media.ToDuration(f.DTS-ep.start)))
	if ep.drift.evals != evals { // observe evaluates at most once
		c.stats.DriftEvaluations++
	}
	if !ep.hasDTS || f.DTS > ep.maxDTS {
		ep.hasDTS, ep.maxDTS = true, f.DTS
	}
	if t.emitted && t.lastEmitEpoch == ep.id {
		t.lastDur = f.DTS - t.lastEmitDTS
	}
	if !t.emitted || f.PTS > t.maxPTS {
		t.maxPTS = f.PTS
	}
	t.emitted, t.lastEmitEpoch, t.lastEmitDTS = true, ep.id, f.DTS
	c.out = append(c.out, f)
}
