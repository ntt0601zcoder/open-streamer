package transcode

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// pendingFrame is one frame waiting in a merger, with its arrival time
// and, in the presentation merger, its order key (b, phase).
type pendingFrame struct {
	f     media.Frame
	at    time.Time
	b     int64
	phase uint8
}

// merger writes one rendition hub in DTS order from frames that arrive from
// two sources, the child and the copy path.
//
// Frames are released in (epoch, DTS) order. The expected tracks are those
// seen in the hub's current epoch (before the first write, in the epoch of
// the next frame), and those announced for it with expect: the router
// announces an encoded rung's video when it sends the epoch's first video
// frame to the child, so the child's audio, which comes out up to the
// encoder's delay (about 1 s with x264) ahead of the rung's IDR, waits for
// it instead of being written before it. The next frame waits only while
// another expected track has nothing pending, and only for at most hold,
// measured as wall time since it arrived or, when the frame is in the
// current epoch, as stream time (the largest DTS pending in that epoch
// minus the frame's DTS), whichever ends first. A later epoch's frames are no measure of the
// current epoch's progress: a burst of new-epoch copy frames must not
// release the current epoch while the child's output for it is still on
// its way. A track that makes a hold run out leaves the expected set until
// it delivers again.
//
// A frame whose epoch is below the hub's current epoch, or whose DTS is not
// above the last one written on its track, is dropped and counted. Before
// an epoch's first frame the merger sets the epoch's anchor on the hub.
//
// The presentation merger (newPresentationMerger) writes every rendition's
// video on its own track behind a keyframe barrier; see presOrder. It
// differs from a rendition merger in two ways only: frames go out in order
// of their presOrder key instead of (epoch, DTS), and while the next frame
// opens an epoch above the hub's current one (or the hub has none yet), it
// waits for that epoch's expected tracks with only the wall-time hold, so
// that a burst of the copy rung's frames cannot release its IDR ahead of
// rung 0's.
//
// The presentation merger's hold also follows the encoder's delay. An
// encoded rung comes out a fixed number of frames behind the copy rung and
// the copied audio (about 30 with x264), which in stream time is 1.2 s at
// 25 fps but 2.5 s at 12 fps: a fixed hold below that releases the copy
// rung ahead of the barrier at every keyframe, for good. So the merger
// measures each track's lag, the largest DTS pushed in the frame's epoch
// minus the frame's DTS, as an envelope that rises at once and falls by an
// eighth of the difference per frame, and holds for one and a half times
// the largest lag: never less than hold, never more than presHoldFactor ×
// hold (8 s by default), in stream time and in wall time alike. A new
// epoch's first frame, for which no lag is known yet, waits presHoldFactor ×
// hold of wall time for the tracks announced for it. The costs: a silent
// rung stalls the presentation for up to that long, once, before it is
// given up; and when an encoder trails by more than the cap (x264's
// defaults below about 4 fps), the copy rung is released ahead of every
// barrier, which the packager reports as rungs left out.
//
// push, expect, barrier and flush are safe for concurrent use; run arms the
// wall-time holds.
type merger struct {
	h         *hub.Hub
	hold      time.Duration
	holdTicks int64
	now       func() time.Time
	anchor    func(epoch uint32) (media.Anchor, bool)
	wake      chan struct{}

	mu      sync.Mutex
	q       map[int][]pendingFrame
	seen    map[uint32]map[int]bool // epoch → tracks seen in it
	last    map[int]int64           // last DTS written per track
	epoch   uint32                  // current epoch of the hub
	written bool
	out     uint64
	dropped uint64
	pres    *presOrder // the presentation merger's barriers; nil otherwise

	// The presentation merger's adaptive hold; unused otherwise.
	maxHold      time.Duration
	maxHoldTicks int64
	lead         map[uint32]int64 // epoch → largest DTS pushed
	lag          map[int]int64    // track → lag envelope, in ticks
}

// presHoldFactor is how many times its hold the presentation merger may
// wait: the cap of the hold that follows the encoder's delay, and the wait
// for a new epoch's announced tracks. With the default hold of 2 s it is
// 8 s: x264's delay of about 30 frames is 6 s at 5 fps, which leaves a
// third to spare, and reaches the cap at about 4 fps.
const presHoldFactor = 4

func newMerger(h *hub.Hub, hold time.Duration, now func() time.Time, anchor func(uint32) (media.Anchor, bool)) *merger {
	return &merger{
		h:         h,
		hold:      hold,
		holdTicks: media.FromDuration(hold),
		now:       now,
		anchor:    anchor,
		wake:      make(chan struct{}, 1),
		q:         map[int][]pendingFrame{},
		seen:      map[uint32]map[int]bool{},
		last:      map[int]int64{},
	}
}

// newPresentationMerger returns a merger for the presentation hub: a
// rendition merger that orders its frames behind keyframe barriers.
func newPresentationMerger(h *hub.Hub, hold time.Duration, now func() time.Time, anchor func(uint32) (media.Anchor, bool)) *merger {
	m := newMerger(h, hold, now, anchor)
	m.pres = newPresOrder(maxBarriers)
	m.maxHold = presHoldFactor * hold
	m.maxHoldTicks = media.FromDuration(m.maxHold)
	m.lead = map[uint32]int64{}
	m.lag = map[int]int64{}
	return m
}

// push adds f and writes every frame that no longer has to wait.
func (m *merger) push(f media.Frame) {
	m.mu.Lock()
	if m.written && f.Epoch < m.epoch {
		m.dropped++
		m.mu.Unlock()
		return
	}
	s := m.seen[f.Epoch]
	if s == nil {
		s = map[int]bool{}
		m.seen[f.Epoch] = s
	}
	s[f.Track] = true
	p := pendingFrame{f: f, at: m.now()}
	if m.pres != nil {
		p.b, p.phase = m.pres.key(f)
		m.noteLag(f)
	}
	m.q[f.Track] = append(m.q[f.Track], p)
	m.release(m.now(), false)
	waiting := m.pending()
	m.mu.Unlock()
	if waiting {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

// expect makes track an expected track of epoch e before any frame of it
// arrives. An epoch below the hub's current one is ignored.
func (m *merger) expect(e uint32, track int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.written && e < m.epoch {
		return
	}
	s := m.seen[e]
	if s == nil {
		s = map[int]bool{}
		m.seen[e] = s
	}
	s[track] = true
}

// barrier records a barrier of epoch e at pts, the PTS of a source
// main-video keyframe after the Delay shift, in the presentation merger. A
// rendition merger keeps no barrier, and an epoch below the hub's current
// one is ignored.
func (m *merger) barrier(e uint32, pts int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pres == nil || m.written && e < m.epoch {
		return
	}
	m.pres.addBarrier(e, pts)
}

// flush writes every pending frame, in order, without holding.
func (m *merger) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.release(time.Time{}, true)
}

// counts returns the frames written and dropped.
func (m *merger) counts() (out, dropped uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out, m.dropped
}

// run arms a timer, with after, for the wall-time hold of the frame that
// waits, and releases it when the timer fires. It returns when ctx ends.
func (m *merger) run(ctx context.Context, after func(time.Duration) <-chan time.Time) {
	var (
		tc    <-chan time.Time
		armed time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tc:
			tc, armed = nil, time.Time{}
			m.mu.Lock()
			m.release(m.now(), false)
			m.mu.Unlock()
		}
		m.mu.Lock()
		dl, ok := m.deadline()
		now := m.now()
		for ok && !now.Before(dl) {
			m.release(now, false)
			dl, ok = m.deadline()
		}
		m.mu.Unlock()
		switch {
		case !ok:
			tc, armed = nil, time.Time{}
		case tc == nil || !dl.Equal(armed):
			armed = dl
			tc = after(dl.Sub(now))
		}
	}
}

// pending reports whether a frame waits. Called with m.mu held.
func (m *merger) pending() bool {
	for _, q := range m.q {
		if len(q) > 0 {
			return true
		}
	}
	return false
}

// candidate returns the track whose head frame comes next: the smallest
// (epoch, DTS), video before audio on a tie, then the lowest track; in the
// presentation merger, the smallest presOrder key. Called with m.mu held.
func (m *merger) candidate() (int, bool) {
	best, found := 0, false
	for t, q := range m.q {
		if len(q) == 0 {
			continue
		}
		if !found {
			best, found = t, true
			continue
		}
		if m.pres != nil {
			if keyBefore(q[0], t, m.q[best][0], best) {
				best = t
			}
		} else if before(q[0].f, t, m.q[best][0].f, best) {
			best = t
		}
	}
	return best, found
}

func before(a media.Frame, at int, b media.Frame, bt int) bool {
	switch {
	case a.Epoch != b.Epoch:
		return a.Epoch < b.Epoch
	case a.DTS != b.DTS:
		return a.DTS < b.DTS
	case a.Kind() != b.Kind():
		return a.Kind() == media.KindVideo
	default:
		return at < bt
	}
}

// waitEpoch returns the epoch whose expected tracks f, the head frame that
// comes next, waits for: the hub's current epoch, or f's before the first
// write. In the presentation merger, a frame of an epoch above the hub's
// current one (or before the first write) waits for its own epoch's
// tracks, and newEpoch reports that only the wall-time hold applies.
// Called with m.mu held.
func (m *merger) waitEpoch(f media.Frame) (e uint32, newEpoch bool) {
	switch {
	case !m.written:
		return f.Epoch, m.pres != nil
	case m.pres != nil && f.Epoch > m.epoch:
		return f.Epoch, true
	default:
		return m.epoch, false
	}
}

// missing returns the expected tracks other than c with nothing pending.
// Called with m.mu held.
func (m *merger) missing(c int) []int {
	e, _ := m.waitEpoch(m.q[c][0].f)
	var out []int
	for t := range m.seen[e] {
		if t != c && len(m.q[t]) == 0 {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// maxPendingDTS returns the largest DTS pending in epoch e. Called with
// m.mu held.
func (m *merger) maxPendingDTS(e uint32) int64 {
	var hi int64
	found := false
	for _, q := range m.q {
		for _, p := range q {
			if p.f.Epoch == e && (!found || p.f.DTS > hi) {
				hi, found = p.f.DTS, true
			}
		}
	}
	return hi
}

// noteLag records, in the presentation merger, how far f is behind the
// furthest frame pushed in its epoch, in its track's lag envelope: a larger
// lag replaces it, a smaller one pulls it down by an eighth of the
// difference. Called with m.mu held.
func (m *merger) noteLag(f media.Frame) {
	lead, ok := m.lead[f.Epoch]
	if !ok || f.DTS > lead {
		lead = f.DTS
		m.lead[f.Epoch] = lead
	}
	lag, env := lead-f.DTS, m.lag[f.Track]
	if lag >= env {
		env = lag
	} else {
		env -= (env - lag + 7) / 8
	}
	m.lag[f.Track] = env
}

// holds returns how long the head frame waits for a missing track, as wall
// time and as stream time. A rendition merger waits hold. The presentation
// merger waits one and a half times the largest lag envelope of its tracks,
// at least hold and at most presHoldFactor × hold; and presHoldFactor ×
// hold for a frame that opens an epoch (newEpoch), whose lag is not known
// yet. Called with m.mu held.
func (m *merger) holds(newEpoch bool) (time.Duration, int64) {
	switch {
	case m.pres == nil:
		return m.hold, m.holdTicks
	case newEpoch:
		return m.maxHold, m.maxHoldTicks
	}
	var lag int64
	for _, l := range m.lag {
		lag = max(lag, l)
	}
	ticks := min(max(lag+lag/2, m.holdTicks), m.maxHoldTicks)
	if ticks == m.holdTicks {
		return m.hold, ticks
	}
	return media.ToDuration(ticks), ticks
}

// deadline returns when the waiting head frame's wall-time hold ends.
// Called with m.mu held.
func (m *merger) deadline() (time.Time, bool) {
	c, ok := m.candidate()
	if !ok {
		return time.Time{}, false
	}
	_, newEpoch := m.waitEpoch(m.q[c][0].f)
	hold, _ := m.holds(newEpoch)
	return m.q[c][0].at.Add(hold), true
}

// release writes the frames that no longer wait; all writes every pending
// frame. Called with m.mu held.
func (m *merger) release(now time.Time, all bool) {
	for {
		c, ok := m.candidate()
		if !ok {
			return
		}
		p := m.q[c][0]
		if miss := m.missing(c); len(miss) > 0 && !all {
			e, newEpoch := m.waitEpoch(p.f)
			// The stream-time bound only holds within the current epoch.
			hold, holdTicks := m.holds(newEpoch)
			streamUp := !newEpoch && p.f.Epoch == e && m.maxPendingDTS(e)-p.f.DTS >= holdTicks
			if !streamUp && now.Sub(p.at) < hold {
				return
			}
			for _, t := range miss {
				delete(m.seen[e], t)
			}
		}
		m.q[c][0] = pendingFrame{}
		m.q[c] = m.q[c][1:]
		if len(m.q[c]) == 0 {
			delete(m.q, c)
		}
		m.write(p.f)
	}
}

// write sends f to the hub. Called with m.mu held.
func (m *merger) write(f media.Frame) {
	if m.written && f.Epoch < m.epoch {
		m.dropped++
		return
	}
	if last, ok := m.last[f.Track]; ok && f.DTS <= last {
		m.dropped++
		return
	}
	if !m.written || f.Epoch > m.epoch {
		if a, ok := m.anchor(f.Epoch); ok {
			m.h.SetAnchor(a)
		}
		m.epoch, m.written = f.Epoch, true
		for e := range m.seen {
			if e < m.epoch {
				delete(m.seen, e)
			}
		}
		if m.pres != nil {
			m.pres.dropBefore(m.epoch)
			for e := range m.lead {
				if e < m.epoch {
					delete(m.lead, e)
				}
			}
		}
	}
	m.last[f.Track] = f.DTS
	m.h.Write(f)
	m.out++
}

// maxBarriers is how many barriers the presentation merger keeps per epoch.
const maxBarriers = 64

// barrierMark is one barrier: its sequence number in its epoch (1, 2, …) and
// its PTS.
type barrierMark struct {
	seq, pts int64
}

// epochBarriers are one epoch's barriers.
type epochBarriers struct {
	list []barrierMark // the last cap barriers, oldest first
	next int64         // sequence number of the next barrier
	base barrierMark   // the newest pruned barrier; seq 0 when none
}

// trackPass is one track's progress through its epoch's barriers.
type trackPass struct {
	epoch  uint32
	passed int64 // the largest barrier sequence the track has passed
}

// presOrder gives each frame of the presentation merger its order key, so
// that every rendition's IDR at a source keyframe K goes out after every
// frame below K, of every track, with rung 0's IDR first (rulings R-B1,
// R-B3).
//
// The router announces a barrier at K (after the Delay shift) for every
// source main-video keyframe it routes, before the keyframe goes to the
// child or the copy path, so a barrier always precedes every rung's frame
// at K. Each epoch numbers its barriers 1, 2, … and keeps its last cap; the
// newest pruned one stays as the epoch's base, and a track that has not
// passed the base passes it with its first frame at or above the base's
// PTS. A frame's key is (epoch, b, phase, then the track for phase 0 or
// the DTS for phase 1), where b is:
//
//   - for a video keyframe whose PTS equals an unpassed barrier: that
//     barrier, which the track passes with every lower one, in phase 0;
//   - for another video keyframe: the highest unpassed barrier below its
//     PTS, if any, else the track's last; phase 1;
//   - for a video non-keyframe: the highest unpassed barrier at or below
//     its PTS, if any, else the track's last (a rung without an IDR at K
//     catches up this way); phase 1;
//   - for audio (and anything else): the largest barrier at or below its
//     PTS, never below the track's last; phase 1.
//
// A track's key never decreases, so the merger's wait for the expected
// tracks still finds the smallest key. It is used under the merger's lock.
type presOrder struct {
	cap      int
	barriers map[uint32]*epochBarriers
	tracks   map[int]trackPass
}

func newPresOrder(capacity int) *presOrder {
	return &presOrder{cap: capacity, barriers: map[uint32]*epochBarriers{}, tracks: map[int]trackPass{}}
}

// addBarrier records the next barrier of epoch e at pts.
func (p *presOrder) addBarrier(e uint32, pts int64) {
	eb := p.barriers[e]
	if eb == nil {
		eb = &epochBarriers{next: 1}
		p.barriers[e] = eb
	}
	eb.list = append(eb.list, barrierMark{seq: eb.next, pts: pts})
	eb.next++
	if len(eb.list) > p.cap {
		eb.base = eb.list[0]
		eb.list[0] = barrierMark{}
		eb.list = eb.list[1:]
	}
}

// dropBefore forgets the barriers and track progress of epochs below e.
func (p *presOrder) dropBefore(e uint32) {
	for id := range p.barriers {
		if id < e {
			delete(p.barriers, id)
		}
	}
	for t, tp := range p.tracks {
		if tp.epoch < e {
			delete(p.tracks, t)
		}
	}
}

// key returns f's barrier sequence b and phase, and records the track's
// progress.
func (p *presOrder) key(f media.Frame) (b int64, phase uint8) {
	tp := p.tracks[f.Track]
	if tp.epoch != f.Epoch {
		tp = trackPass{epoch: f.Epoch}
	}
	phase = 1
	if eb := p.barriers[f.Epoch]; eb != nil {
		if tp.passed < eb.base.seq && f.PTS >= eb.base.pts {
			tp.passed = eb.base.seq
		}
		video := f.Kind() == media.KindVideo
		var equal, below, atOrBelow int64 // unpassed barriers, 0 for none
		var reached int64                 // any barrier at or below f.PTS
		for _, br := range eb.list {
			if br.pts <= f.PTS {
				reached = br.seq
			}
			if br.seq <= tp.passed {
				continue
			}
			switch {
			case br.pts == f.PTS:
				equal = br.seq
				atOrBelow = br.seq
			case br.pts < f.PTS:
				below = br.seq
				atOrBelow = br.seq
			}
		}
		switch {
		case video && f.Key && equal > 0:
			tp.passed, phase = equal, 0
		case video && f.Key:
			tp.passed = max(tp.passed, below)
		case video:
			tp.passed = max(tp.passed, atOrBelow)
		default:
			tp.passed = max(tp.passed, reached)
		}
	}
	p.tracks[f.Track] = tp
	return tp.passed, phase
}

// keyBefore reports whether pending frame a, on track at, comes before b,
// on track bt, in the presentation merger's order: (epoch, b, phase), then
// the track in phase 0, or the DTS (video before audio on a tie, then the
// lowest track) in phase 1.
func keyBefore(a pendingFrame, at int, b pendingFrame, bt int) bool {
	switch {
	case a.f.Epoch != b.f.Epoch:
		return a.f.Epoch < b.f.Epoch
	case a.b != b.b:
		return a.b < b.b
	case a.phase != b.phase:
		return a.phase < b.phase
	case a.phase == 0:
		return at < bt
	default:
		return before(a.f, at, b.f, bt)
	}
}
