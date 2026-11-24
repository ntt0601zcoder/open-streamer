package transcode

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// pendingFrame is one frame waiting in a merger, with its arrival time.
type pendingFrame struct {
	f  media.Frame
	at time.Time
}

// merger writes one rendition hub in DTS order from frames that arrive from
// two sources, the child and the copy path.
//
// Frames are released in (epoch, DTS) order. The expected tracks are those
// seen in the hub's current epoch (before the first write, in the epoch of
// the next frame). The next frame waits only while another expected track
// has nothing pending, and only for at most hold, measured as stream time
// (the largest pending DTS minus the frame's DTS) or as wall time since it
// arrived, whichever ends first. A track that makes a hold run out leaves
// the expected set until it delivers again.
//
// A frame whose epoch is below the hub's current epoch, or whose DTS is not
// above the last one written on its track, is dropped and counted. Before
// an epoch's first frame the merger sets the epoch's anchor on the hub.
//
// push and flush are safe for concurrent use; run arms the wall-time holds.
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
}

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
	m.q[f.Track] = append(m.q[f.Track], pendingFrame{f: f, at: m.now()})
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
// (epoch, DTS), video before audio on a tie, then the lowest track. Called
// with m.mu held.
func (m *merger) candidate() (int, bool) {
	best, found := 0, false
	for t, q := range m.q {
		if len(q) == 0 {
			continue
		}
		if !found || before(q[0].f, t, m.q[best][0].f, best) {
			best, found = t, true
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

// missing returns the expected tracks other than c with nothing pending.
// Called with m.mu held.
func (m *merger) missing(c int) []int {
	e := m.epoch
	if !m.written {
		e = m.q[c][0].f.Epoch
	}
	var out []int
	for t := range m.seen[e] {
		if t != c && len(m.q[t]) == 0 {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// maxPendingDTS returns the largest DTS pending. Called with m.mu held.
func (m *merger) maxPendingDTS() int64 {
	var hi int64
	found := false
	for _, q := range m.q {
		for _, p := range q {
			if !found || p.f.DTS > hi {
				hi, found = p.f.DTS, true
			}
		}
	}
	return hi
}

// deadline returns when the waiting head frame's wall-time hold ends.
// Called with m.mu held.
func (m *merger) deadline() (time.Time, bool) {
	c, ok := m.candidate()
	if !ok {
		return time.Time{}, false
	}
	return m.q[c][0].at.Add(m.hold), true
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
			if m.maxPendingDTS()-p.f.DTS < m.holdTicks && now.Sub(p.at) < m.hold {
				return
			}
			e := m.epoch
			if !m.written {
				e = p.f.Epoch
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
	}
	m.last[f.Track] = f.DTS
	m.h.Write(f)
	m.out++
}
