package pipeline

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// liveInput is an input whose every open reads marker's source from the
// time of the open.
func liveInput(set *timedSet, marker byte) *opener {
	return &opener{next: func(int, context.Context) (ingest.Reader, error) {
		return set.reader(source{marker: marker, gop: idxGOP}.frames(set.ft.elapsed(), 3*time.Minute)), nil
	}}
}

// failingInput is an input whose first open reads marker's source until at,
// when it fails; its later opens are live.
func failingInput(set *timedSet, marker byte, at time.Duration) *opener {
	src := source{marker: marker, gop: idxGOP}
	return &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
		if n == 0 {
			return set.reader(append(src.frames(0, at), failAt(at, errors.New("dropped")))), nil
		}
		return set.reader(src.frames(set.ft.elapsed(), 3*time.Minute)), nil
	}}
}

// hangInput is an input whose opens never deliver.
func hangInput() *opener {
	return &opener{next: func(int, context.Context) (ingest.Reader, error) {
		return newFakeReader(videoOnly, ingest.Spec{}, nil, false), nil
	}}
}

// standbyLive is a standby input that is live from its second open.
func standbyLive(set *timedSet, marker byte) *opener {
	return standby(videoOnly, func(int) (ingest.Reader, error) {
		return set.reader(source{marker: marker, gop: idxGOP}.frames(set.ft.elapsed(), 3*time.Minute)), nil
	})
}

// closedReaders returns how many of the readers o opened are closed, and how
// many it opened.
func (o *opener) closedReaders() (closed, total int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range o.readers {
		if r.(interface{ isClosed() bool }).isClosed() {
			closed++
		}
	}
	return closed, len(o.readers)
}

// allClosed reports whether every reader o opened is closed.
func (o *opener) allClosed() bool {
	c, n := o.closedReaders()
	return c == n
}

// waitOpens waits until o was opened n times.
func waitOpens(t *testing.T, o *opener, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for o.count() < n {
		require.True(t, time.Now().Before(deadline), "opened %d times, want %d", o.count(), n)
		time.Sleep(50 * time.Microsecond)
	}
	time.Sleep(200 * time.Microsecond) // an open too many would show
	require.Equal(t, n, o.count())
}

// waitReaders waits until o recorded n readers: it counts an open before
// the open returns its reader.
func waitReaders(t *testing.T, o *opener, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, got := o.closedReaders(); got >= n {
			return
		}
		require.True(t, time.Now().Before(deadline), "fewer than %d readers", n)
		time.Sleep(50 * time.Microsecond)
	}
}

func setInputs(t *testing.T, p *Pipeline, urls ...string) {
	t.Helper()
	require.NoError(t, p.SetInputs(timeout(t), urls))
	assert.Equal(t, urls, p.Inputs())
}

// epochIDs returns the epochs of the delivered frames, sorted.
func epochIDs(ds []hub.Delivery) []uint32 {
	seen := epochsOf(ds)
	ids := make([]uint32, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// markers returns the distinct source markers of the frames, in order of
// first appearance.
func markers(fs []media.Frame) []byte {
	var out []byte
	for _, f := range fs {
		if !slices.Contains(out, f.Data[0][0]) {
			out = append(out, f.Data[0][0])
		}
	}
	return out
}

func failures(ev *events) int {
	n := 0
	for _, k := range ev.kinds() {
		if k == EventFailed {
			n++
		}
	}
	return n
}

// editRig is a pipeline on the fake clock with a hub subscriber.
type editRig struct {
	ft   *fakeTime
	ev   *events
	set  *timedSet
	m    byURL
	p    *Pipeline
	sub  *hub.Sub
	stop func()
}

// newEditRig runs urls; build makes the inputs, the added ones too.
func newEditRig(t *testing.T, build func(set *timedSet) byURL, urls ...string) *editRig {
	t.Helper()
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	m := build(set)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := multiOptions(ft, m, ev, urls...)
	opt.Parse = passiveParse
	opt.Layout = videoOnly
	p := New(h, opt)
	return &editRig{ft: ft, ev: ev, set: set, m: m, p: p, sub: sub, stop: startRun(t, p)}
}

func TestSetInputsReplaceActive(t *testing.T) {
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{"fake://a": liveInput(set, 0), "fake://b": liveInput(set, 1)}
	}, "fake://a")
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 2*time.Second)

	setInputs(t, r.p, "fake://b")
	assert.True(t, r.m["fake://a"].firstTimed().isClosed(), "the removed input is closed when SetInputs returns")
	assert.Equal(t, -1, r.p.Stats().Active)
	waitOpens(t, r.m["fake://b"], 1)
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 4*time.Second)
	st := r.p.Stats()
	r.stop()

	assert.Equal(t, 0, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	assert.Equal(t, []EventKind{EventActive, EventActive}, r.ev.kinds(), "a removed input does not fail")
	assert.Equal(t, 1, r.m["fake://a"].count())
	assert.Equal(t, 1, r.m["fake://b"].count())
	ds := drain(r.sub)
	ids := epochIDs(ds)
	require.Len(t, ids, 2, "one epoch change")
	assert.Equal(t, ids[0]+1, ids[1])
	epochs := byEpoch(ds)
	assert.Equal(t, []byte{0}, markers(epochs[0]))
	assert.Equal(t, []byte{1}, markers(epochs[1]))
	assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
}

func TestSetInputsKeepsActive(t *testing.T) {
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{"fake://a": liveInput(set, 0), "fake://b": standby(videoOnly, noReopen(t)), "fake://c": liveInput(set, 2)}
	}, "fake://a", "fake://b")
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 2*time.Second)

	setInputs(t, r.p, "fake://c", "fake://a")
	assert.Equal(t, 1, r.p.Stats().Active, "the active input moved")
	waitOpens(t, r.m["fake://c"], 1)
	// C reads from 2 s: its hold ends at 12 s and its next key is at 13.2 s.
	stepTo(t, r.set, 12*time.Second)
	st := r.p.Stats()
	assert.Equal(t, 1, st.Active)
	assert.Zero(t, st.Switches)
	ra := r.m["fake://a"].firstTimed()
	assert.False(t, ra.isClosed(), "the kept input keeps its reader")
	before := drain(r.sub)
	assert.Len(t, epochIDs(before), 1, "no new epoch")
	assert.Equal(t, []byte{0}, markers(frames(before)))
	assert.False(t, r.m["fake://c"].firstTimed().isClosed(), "the added input is probed")

	stepTo(t, r.set, 14*time.Second)
	st = r.p.Stats()
	r.stop()
	assert.Equal(t, 0, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	assert.Equal(t, []int{0, 0}, r.ev.actives())
	assert.Equal(t, 1, r.m["fake://a"].count(), "the kept input was opened once in all")
	assert.Equal(t, 1, r.m["fake://c"].count())
	assert.Zero(t, failures(r.ev))
	assert.True(t, ra.isClosed(), "the fail-back closes the input below")
	epochs := byEpoch(append(before, drain(r.sub)...))
	require.Len(t, epochs, 2, "the fail-back is exactly one new epoch")
	assert.Equal(t, []byte{0}, markers(epochs[0]))
	assert.Equal(t, []byte{2}, markers(epochs[1]))
	assert.Equal(t, 330, frameIdx(epochs[1][0]), "the fail-back is at C's key after its hold")
}

func TestSetInputsReorderBelow(t *testing.T) {
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{"fake://a": failingInput(set, 0, 2*time.Second), "fake://b": standbyLive(set, 1)}
	}, "fake://a", "fake://b")
	waitActive(t, r.ev, 0)
	// A fails at 2 s and B becomes active; A reopens at 2.5 s as a probe.
	stepTo(t, r.set, 2*time.Second)
	waitActive(t, r.ev, 1)
	stepTo(t, r.set, 4*time.Second)
	a := r.m["fake://a"]
	waitOpens(t, a, 2)
	waitReaders(t, a, 2)
	probe := a.lastTimed(t)
	require.False(t, probe.isClosed())
	require.NotEmpty(t, probe.returned(), "A is probing above B")
	require.Equal(t, 1, r.p.Stats().Active)
	require.Less(t, r.ft.elapsed(), 12*time.Second, "inside A's hold")
	evs := len(r.ev.kinds())

	setInputs(t, r.p, "fake://b", "fake://a")
	assert.Equal(t, 0, r.p.Stats().Active)
	assert.True(t, probe.isClosed(), "a pull attempt below the active input is closed")
	stepTo(t, r.set, 30*time.Second)
	st := r.p.Stats()
	r.stop()

	assert.Equal(t, 0, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	assert.Equal(t, 2, a.count(), "A is not reopened while B is active")
	assert.Equal(t, 2, r.m["fake://b"].count())
	assert.Len(t, r.ev.kinds(), evs, "no event after the edit")
	ds := drain(r.sub)
	assert.Len(t, epochIDs(ds), 2, "A's epoch and B's: none for the edit")
	assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
}

func TestSetInputsRemovesIdle(t *testing.T) {
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{"fake://a": liveInput(set, 0), "passive://l": hangInput()}
	}, "fake://a", "passive://l")
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 2*time.Second)
	l := r.m["passive://l"]
	waitOpens(t, l, 1)
	waitReaders(t, l, 1)
	require.False(t, l.reader(0).isClosed(), "the passive input listens")

	setInputs(t, r.p, "fake://a")
	assert.True(t, l.reader(0).isClosed(), "the removed listener is closed when SetInputs returns")
	stepTo(t, r.set, 4*time.Second)
	st := r.p.Stats()
	r.stop()

	assert.Equal(t, 0, st.Active)
	assert.Zero(t, st.Switches)
	assert.Equal(t, 1, r.m["fake://a"].count())
	assert.Equal(t, 1, l.count())
	assert.Len(t, epochIDs(drain(r.sub)), 1, "no new epoch")
	assert.Equal(t, []EventKind{EventActive}, r.ev.kinds())
}

func TestSetInputsKeepsListener(t *testing.T) {
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{
			"fake://a": liveInput(set, 0),
			// The publisher of L starts at 3 s.
			"passive://l": {next: func(int, context.Context) (ingest.Reader, error) {
				return set.reader(source{marker: 7, gop: idxGOP}.frames(3*time.Second, time.Minute)), nil
			}},
			"fake://b": hangInput(),
		}
	}, "fake://a", "passive://l")
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 2*time.Second)
	l := r.m["passive://l"]
	waitOpens(t, l, 1)
	waitReaders(t, l, 1)
	lr := l.firstTimed()

	setInputs(t, r.p, "passive://l", "fake://b")
	assert.True(t, r.m["fake://a"].firstTimed().isClosed())
	assert.False(t, lr.isClosed(), "the kept listener keeps its socket")
	stepTo(t, r.set, 5*time.Second)
	st := r.p.Stats()
	assert.False(t, lr.isClosed())
	r.stop()

	assert.Equal(t, 0, st.Active, "the listener is active at its first frame")
	assert.Equal(t, uint64(1), st.Switches)
	assert.Equal(t, 1, l.count(), "the listener was opened once in all")
	assert.NotEmpty(t, lr.returned(), "the same reader delivers after the edit")
	assert.Equal(t, []int{0, 0}, r.ev.actives())
	ds := drain(r.sub)
	epochs := byEpoch(ds)
	require.Len(t, epochs, 2)
	assert.Equal(t, []byte{0}, markers(epochs[0]))
	assert.Equal(t, []byte{7}, markers(epochs[1]))
}

func TestSetInputsDuringSearch(t *testing.T) {
	t.Run("search", func(t *testing.T) {
		// A fails at 2 s (retry at 2.5 s); the search tries B, which hangs.
		r := newEditRig(t, func(set *timedSet) byURL {
			return byURL{
				"fake://a": failingInput(set, 0, 2*time.Second),
				"fake://b": hangInput(),
				"fake://c": standby(videoOnly, noReopen(t)),
				"fake://d": liveInput(set, 3),
			}
		}, "fake://a", "fake://b", "fake://c")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, 2*time.Second)
		a, b, d := r.m["fake://a"], r.m["fake://b"], r.m["fake://d"]
		waitOpens(t, b, 2) // at start-up and by the search
		waitReaders(t, b, 2)
		stepTo(t, r.set, 2200*time.Millisecond)
		require.False(t, b.reader(1).isClosed(), "the search is trying B")
		require.Equal(t, -1, r.p.Stats().Active)

		setInputs(t, r.p, "fake://a", "fake://d")
		assert.True(t, b.allClosed(), "B is closed when SetInputs returns")
		assert.True(t, r.m["fake://c"].allClosed())
		waitOpens(t, d, 1)     // at once: the time has not moved
		waitActive(t, r.ev, 1) // D delivers first
		assert.Equal(t, 1, a.count(), "A waits out its backoff")
		stepTo(t, r.set, 2440*time.Millisecond)
		assert.Equal(t, 1, a.count(), "A is not reopened before its retry time")
		assert.Equal(t, 1, r.p.Stats().Active)
		stepTo(t, r.set, 4*time.Second)
		waitOpens(t, a, 2)
		st := r.p.Stats()
		r.stop()

		assert.Equal(t, 2, a.count(), "A reopens at its retry time, as a probe")
		assert.Equal(t, 1, st.Active)
		assert.Equal(t, uint64(1), st.Switches)
		assert.Equal(t, []int{0, 1}, r.ev.actives())
		assert.Equal(t, 1, failures(r.ev), "A's failure alone")
		assert.Equal(t, 2, b.count())
		assert.Equal(t, 1, r.m["fake://c"].count())
		ds := drain(r.sub)
		epochs := byEpoch(ds)
		require.Len(t, epochs, 2, "exactly one new epoch after the edit")
		assert.Equal(t, []byte{0}, markers(epochs[0]))
		assert.Equal(t, []byte{3}, markers(epochs[1]))
	})
	t.Run("kept input in backoff", func(t *testing.T) {
		// A fails at 2 s (retry at 2.5 s) and B becomes active, at a key
		// (index 50) so that its epoch has frames.
		r := newEditRig(t, func(set *timedSet) byURL {
			return byURL{
				"fake://a": failingInput(set, 0, 2*time.Second),
				"fake://b": standby(videoOnly, func(int) (ingest.Reader, error) {
					return set.reader(source{marker: 1, gop: idxGOP, phase: 20}.frames(set.ft.elapsed(), time.Minute)), nil
				}),
			}
		}, "fake://a", "fake://b")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, 2*time.Second)
		waitActive(t, r.ev, 1)
		stepTo(t, r.set, 2200*time.Millisecond)
		a, b := r.m["fake://a"], r.m["fake://b"]
		require.Equal(t, 1, a.count())

		setInputs(t, r.p, "fake://a")
		assert.True(t, b.allClosed())
		assert.Equal(t, -1, r.p.Stats().Active)
		stepTo(t, r.set, 2440*time.Millisecond)
		assert.Equal(t, 1, a.count(), "nothing is opened until A's retry time")
		assert.Equal(t, 2, b.count())
		assert.Equal(t, -1, r.p.Stats().Active)
		stepTo(t, r.set, 4*time.Second)
		waitActive(t, r.ev, 0)
		st := r.p.Stats()
		r.stop()

		assert.Equal(t, 2, a.count(), "A reopens at its retry time")
		assert.Equal(t, 2, b.count())
		assert.Equal(t, 0, st.Active)
		assert.Equal(t, uint64(2), st.Switches, "A to B, and B to A through the removed input")
		assert.Equal(t, []int{0, 1, 0}, r.ev.actives())
		epochs := byEpoch(drain(r.sub))
		require.Len(t, epochs, 3, "one new epoch after the edit")
		assert.Equal(t, []byte{1}, markers(epochs[1]))
		assert.Equal(t, []byte{0}, markers(epochs[2]))
	})
	t.Run("added inputs open together", func(t *testing.T) {
		// B's open fails at start-up, before A delivers (from 0.2 s); A fails
		// at 2 s and the search tries B, which hangs: both have failed and no
		// input is active.
		down := errors.New("down")
		r := newEditRig(t, func(set *timedSet) byURL {
			return byURL{
				"fake://a": {next: func(int, context.Context) (ingest.Reader, error) {
					fs := source{marker: 0, gop: idxGOP}.frames(200*time.Millisecond, 2*time.Second)
					return set.reader(append(fs, failAt(2*time.Second, errors.New("dropped")))), nil
				}},
				"fake://b": {next: func(n int, _ context.Context) (ingest.Reader, error) {
					if n == 0 {
						return nil, down
					}
					return newFakeReader(videoOnly, ingest.Spec{}, nil, false), nil
				}},
				"fake://c": hangInput(),
				"fake://d": hangInput(),
			}
		}, "fake://a", "fake://b")
		e, ok := r.ev.next()
		require.True(t, ok)
		require.Equal(t, Event{Kind: EventFailed, Input: 1, Err: e.Err}, e)
		stepTo(t, r.set, 200*time.Millisecond)
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, 2*time.Second)
		a, b := r.m["fake://a"], r.m["fake://b"]
		waitOpens(t, b, 2)
		waitReaders(t, b, 1) // the failed open recorded none
		stepTo(t, r.set, 2200*time.Millisecond)
		require.Equal(t, -1, r.p.Stats().Active)
		require.Equal(t, 2, failures(r.ev))

		setInputs(t, r.p, "fake://a", "fake://b", "fake://c", "fake://d")
		waitOpens(t, r.m["fake://c"], 1)
		waitOpens(t, r.m["fake://d"], 1)
		assert.Equal(t, 1, a.count(), "A waits out its backoff")
		assert.Equal(t, 2, b.count(), "B's attempt goes on")
		assert.False(t, b.reader(0).isClosed())
		r.stop()
	})
}

func TestSetInputsSwitchCount(t *testing.T) {
	build := func(set *timedSet) byURL {
		return byURL{
			"fake://a": liveInput(set, 0),
			"fake://b": liveInput(set, 1),
			"fake://c": liveInput(set, 2),
			"fake://s": hangInput(),
		}
	}
	t.Run("replaced twice", func(t *testing.T) {
		r := newEditRig(t, build, "fake://a")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, time.Second)
		setInputs(t, r.p, "fake://b")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, 2*time.Second)
		require.Equal(t, uint64(1), r.p.Stats().Switches)
		setInputs(t, r.p, "fake://c")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, 3*time.Second)
		st := r.p.Stats()
		r.stop()
		assert.Equal(t, 0, st.Active)
		assert.Equal(t, uint64(2), st.Switches, "an activation after a removed active input is a switch")
		assert.Equal(t, []int{0, 0, 0}, r.ev.actives())
		assert.Len(t, epochIDs(drain(r.sub)), 3)
	})
	t.Run("one input", func(t *testing.T) {
		r := newEditRig(t, build, "fake://a")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, 3*time.Second)
		st := r.p.Stats()
		r.stop()
		assert.Zero(t, st.Switches)
	})
	t.Run("reordered", func(t *testing.T) {
		r := newEditRig(t, build, "fake://a", "fake://s")
		waitActive(t, r.ev, 0)
		stepTo(t, r.set, time.Second)
		setInputs(t, r.p, "fake://s", "fake://a")
		require.Equal(t, 1, r.p.Stats().Active)
		stepTo(t, r.set, 3*time.Second)
		st := r.p.Stats()
		r.stop()
		assert.Equal(t, 1, st.Active)
		assert.Zero(t, st.Switches)
		assert.Equal(t, 1, r.m["fake://a"].count())
		assert.Len(t, epochIDs(drain(r.sub)), 1)
	})
}

func TestSetInputsDuplicates(t *testing.T) {
	// No input delivers, so the added duplicate opens at once.
	r := newEditRig(t, func(*timedSet) byURL { return byURL{"fake://a": hangInput()} }, "fake://a")
	a := r.m["fake://a"]
	waitOpens(t, a, 1)
	waitReaders(t, a, 1)

	setInputs(t, r.p, "fake://a", "fake://a")
	waitOpens(t, a, 2) // one more open
	waitReaders(t, a, 2)
	first, second := a.reader(0), a.reader(1)
	assert.False(t, first.isClosed())

	setInputs(t, r.p, "fake://a")
	assert.False(t, first.isClosed(), "the first duplicate stays")
	assert.True(t, second.isClosed(), "the second duplicate is closed")
	waitOpens(t, a, 2)
	r.stop()
	assert.Empty(t, r.ev.kinds())
}

func TestSetInputsSameList(t *testing.T) {
	t.Run("active", sameListActive)
	t.Run("idle", sameListIdle)
}

func sameListActive(t *testing.T) {
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{"fake://a": liveInput(set, 0), "passive://l": hangInput()}
	}, "fake://a", "passive://l")
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 2*time.Second)
	a, l := r.m["fake://a"], r.m["passive://l"]
	waitOpens(t, l, 1)
	waitReaders(t, l, 1)

	urls := []string{"fake://a", "passive://l"}
	require.NoError(t, r.p.SetInputs(timeout(t), urls))
	urls[0] = "changed"
	assert.Equal(t, []string{"fake://a", "passive://l"}, r.p.Inputs(), "the caller's slice is not kept")
	stepTo(t, r.set, 4*time.Second)
	st := r.p.Stats()
	assert.False(t, a.firstTimed().isClosed(), "nothing closed")
	assert.False(t, l.reader(0).isClosed(), "nothing closed")
	r.stop()

	assert.Equal(t, 0, st.Active)
	assert.Zero(t, st.Switches)
	assert.Equal(t, 1, a.count(), "nothing opened")
	assert.Equal(t, 1, l.count(), "nothing opened")
	assert.Equal(t, []EventKind{EventActive}, r.ev.kinds(), "no event")
	assert.Len(t, epochIDs(drain(r.sub)), 1)
}

// sameListIdle is the list in force again while no input is active and a
// search runs: the search goes on, one input at a time.
func sameListIdle(t *testing.T) {
	// A fails at 2 s; the search tries B, which hangs; C waits its turn.
	r := newEditRig(t, func(set *timedSet) byURL {
		return byURL{
			"fake://a": failingInput(set, 0, 2*time.Second),
			"fake://b": hangInput(),
			"fake://c": hangInput(),
		}
	}, "fake://a", "fake://b", "fake://c")
	waitActive(t, r.ev, 0)
	stepTo(t, r.set, 2*time.Second)
	a, b, c := r.m["fake://a"], r.m["fake://b"], r.m["fake://c"]
	waitOpens(t, b, 2) // at start-up and by the search
	waitReaders(t, b, 2)
	waitOpens(t, c, 1)
	stepTo(t, r.set, 2200*time.Millisecond)
	require.Equal(t, -1, r.p.Stats().Active)
	evs := len(r.ev.kinds())

	setInputs(t, r.p, "fake://a", "fake://b", "fake://c")
	waitOpens(t, c, 1) // nothing opened: C is still after B in the search
	stepTo(t, r.set, 2440*time.Millisecond)
	assert.Equal(t, 1, a.count(), "nothing opened")
	assert.Equal(t, 2, b.count(), "nothing opened")
	assert.Equal(t, 1, c.count(), "the search is unchanged: one pull input at a time")
	assert.False(t, b.reader(1).isClosed(), "nothing closed")
	assert.Len(t, r.ev.kinds(), evs, "no event")
	r.stop()
}

func TestSetInputsErrors(t *testing.T) {
	refused := errors.New("unsupported scheme")
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	m := byURL{"fake://a": liveInput(set, 0), "fake://b": hangInput()}
	opt := multiOptions(ft, m, ev, "fake://a")
	opt.Layout = videoOnly
	opt.Parse = func(u string) (ingest.Spec, error) {
		if u == "bad://user:secret@host" {
			return ingest.Spec{}, refused
		}
		return ingest.Spec{}, nil
	}
	p := New(hub.New(hub.Options{}), opt)

	t.Run("cancelled before Run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, p.SetInputs(ctx, []string{"fake://b"}), context.Canceled)
		ctx, cancel = context.WithCancel(context.Background())
		res := make(chan error, 1)
		go func() { res <- p.SetInputs(ctx, []string{"fake://b"}) }()
		select {
		case err := <-res:
			t.Fatalf("SetInputs returned before Run: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		cancel()
		require.ErrorIs(t, <-res, context.Canceled)
		assert.Equal(t, []string{"fake://a"}, p.Inputs())
	})

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()
	waitActive(t, ev, 0)
	stepTo(t, set, time.Second)

	for name, urls := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, p.SetInputs(timeout(t), urls))
			assert.Equal(t, []string{"fake://a"}, p.Inputs())
		})
	}
	t.Run("parse", func(t *testing.T) {
		err := p.SetInputs(timeout(t), []string{"fake://b", "bad://user:secret@host"})
		require.ErrorIs(t, err, refused)
		assert.NotContains(t, err.Error(), "secret")
		assert.NotContains(t, err.Error(), "bad://")
		assert.Equal(t, []string{"fake://a"}, p.Inputs())
		stepTo(t, set, 2*time.Second)
		assert.Equal(t, 0, p.Stats().Active)
		assert.False(t, m["fake://a"].firstTimed().isClosed(), "no reader touched")
		assert.Equal(t, 1, m["fake://a"].count())
		assert.Zero(t, m["fake://b"].count())
	})
	urls := []string{"fake://a"}
	in := p.Inputs()
	in[0] = "changed"
	assert.Equal(t, urls, p.Inputs(), "Inputs returns a copy")

	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	t.Run("after Run", func(t *testing.T) {
		require.ErrorIs(t, p.SetInputs(timeout(t), []string{"fake://b"}), ErrNotRunning)
		assert.Equal(t, []string{"fake://a"}, p.Inputs())
	})
	assert.Equal(t, []EventKind{EventActive}, ev.kinds())
}

func TestSetInputsBeforeRun(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	m := byURL{"fake://a": hangInput(), "fake://b": liveInput(set, 1)}
	opt := multiOptions(ft, m, ev, "fake://a")
	opt.Layout = videoOnly
	p := New(hub.New(hub.Options{}), opt)

	urls := []string{"fake://b"}
	res := make(chan error, 1)
	go func() { res <- p.SetInputs(timeout(t), urls) }()
	select {
	case err := <-res:
		t.Fatalf("SetInputs returned before Run: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	stop := startRun(t, p)
	select {
	case err := <-res:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("SetInputs did not return")
	}
	assert.Equal(t, []string{"fake://b"}, p.Inputs())
	assert.True(t, m["fake://a"].allClosed(), "the first list's input is closed")
	waitActive(t, ev, 0)
	stepTo(t, set, time.Second)
	st := p.Stats()
	stop()
	assert.Equal(t, 0, st.Active)
	assert.Zero(t, st.Switches)
	assert.Equal(t, 1, m["fake://b"].count())
	assert.LessOrEqual(t, m["fake://a"].count(), 1)
}

// tickReader is a live reader on the real clock: a video frame every
// millisecond, a key every ten, until Close.
type tickReader struct {
	n      int64
	closed chan struct{}
	once   sync.Once
}

func (r *tickReader) Read() (ingest.Frame, error) {
	select {
	case <-r.closed:
		return ingest.Frame{}, errFakeClosed
	case <-time.After(time.Millisecond):
	}
	d := r.n * media.ClockRate / 1000
	r.n++
	f := media.Frame{Track: 0, Config: videoCfg, DTS: d, PTS: d, Key: r.n%10 == 1, Data: [][]byte{{1}}}
	return ingest.Frame{Frame: f, Arrival: time.Now()}, nil
}

func (r *tickReader) Kinds() []media.Kind      { return videoOnly }
func (r *tickReader) Spec() ingest.Spec        { return ingest.Spec{Live: true} }
func (r *tickReader) Stats() map[string]uint64 { return nil }

func (r *tickReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (r *tickReader) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

func TestSetInputsRace(t *testing.T) {
	var mu sync.Mutex
	var readers []*tickReader
	opt := Options{
		Inputs: []string{"fake://a", "fake://b"},
		Parse:  passiveParse,
		Layout: videoOnly,
		Open: func(context.Context, string) (ingest.Reader, error) {
			r := &tickReader{closed: make(chan struct{})}
			mu.Lock()
			readers = append(readers, r)
			mu.Unlock()
			return r, nil
		},
		FailbackHold: 20 * time.Millisecond,
		OnEvent:      func(Event) {},
	}
	h := hub.New(hub.Options{})
	p := New(h, opt)
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	lists := [][]string{
		{"fake://a", "fake://b", "passive://c"},
		{"passive://c", "fake://b", "fake://a"},
		{"fake://b"},
		{"fake://d", "fake://a"},
		{"fake://a", "fake://a", "passive://c"},
		{"passive://c"},
		{"fake://b", "fake://d", "fake://a", "passive://c"},
	}
	end := time.Now().Add(200 * time.Millisecond)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := g; time.Now().Before(end); i++ {
				if err := p.SetInputs(ctx, lists[i%len(lists)]); err != nil {
					t.Errorf("SetInputs: %v", err)
					return
				}
				_ = p.Inputs()
				_ = p.Stats()
				time.Sleep(time.Duration(i%4) * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	assert.True(t, slices.ContainsFunc(lists, func(l []string) bool { return slices.Equal(l, p.Inputs()) }))
	cancel()
	select {
	case err := <-res:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	require.ErrorIs(t, p.SetInputs(context.Background(), lists[0]), ErrNotRunning)
	goroutinesBack(t, base)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, readers)
	for i, r := range readers {
		assert.True(t, r.isClosed(), "reader %d is open", i)
	}
}
