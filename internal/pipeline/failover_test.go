package pipeline

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

var (
	videoOnly = []media.Kind{media.KindVideo}
	videoAud  = []media.Kind{media.KindVideo, media.KindAudio}
)

// idxGOP is the GOP of idxFrames, in frames.
const idxGOP = 30

// idxFrames returns the video frames of source indexes [from, to) on reader
// track 0: DTS idx×vDur, a key every idxGOP indexes, Data {marker, idx},
// arriving at base plus their DTS.
func idxFrames(from, to int, marker byte, base time.Time) []step {
	out := make([]step, 0, to-from)
	for i := from; i < to; i++ {
		d := int64(i) * vDur
		f := media.Frame{Track: 0, Config: videoCfg, DTS: d, PTS: d, Key: i%idxGOP == 0, Data: [][]byte{{marker, byte(i >> 8), byte(i)}}}
		out = append(out, step{f: ingest.Frame{Frame: f, Arrival: base.Add(media.ToDuration(d))}})
	}
	return out
}

func frameIdx(f media.Frame) int { return int(f.Data[0][1])<<8 | int(f.Data[0][2]) }

// standby is a backup input: its first open, made at start-up while the
// primary becomes active, returns a reader that never delivers; later opens
// return next(n), n from 1.
func standby(kinds []media.Kind, next func(n int) (ingest.Reader, error)) *opener {
	return &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
		if n == 0 {
			return newFakeReader(kinds, ingest.Spec{}, nil, false), nil
		}
		return next(n)
	}}
}

// noReopen fails the test when a standby input is opened again.
func noReopen(t *testing.T) func(int) (ingest.Reader, error) {
	return func(n int) (ingest.Reader, error) {
		t.Errorf("input reopened (open %d)", n)
		return nil, errors.New("unexpected open")
	}
}

func (o *opener) reader(i int) *fakeReader {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.readers[i].(*fakeReader)
}

func (o *opener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens
}

// multiOptions runs the inputs of m, by priority urls, with a fake clock.
func multiOptions(ft *fakeTime, m byURL, ev *events, urls ...string) Options {
	return Options{
		Inputs:  urls,
		Open:    m.open,
		Parse:   noParse,
		Now:     ft.Now,
		Sleep:   ft.Sleep,
		After:   ft.After,
		OnEvent: ev.on,
	}
}

func frames(ds []hub.Delivery) []media.Frame {
	out := make([]media.Frame, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Frame)
	}
	return out
}

// waitCall waits for an After(d) call: once it is reported, its timer is
// registered, so advancing the time cannot overtake the arming.
func waitCall(t *testing.T, ft *fakeTime, d time.Duration) {
	t.Helper()
	for {
		got, ok := ft.nextCall()
		require.True(t, ok, "no After(%v)", d)
		if got == d {
			return
		}
	}
}

// drainCalls forgets the After calls made so far.
func drainCalls(ft *fakeTime) {
	for len(ft.calls) > 0 {
		<-ft.calls
	}
}

func timeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFailoverOneEpoch(t *testing.T) {
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		steps0 []step // input 0's, then boom
		from1  int    // input 1's first source index
		events []EventKind
		lost0  int // input 0's frames read but not written
	}{
		// Input 0 ends cleanly; the backup carries the same source from the
		// frame after input 0's last, which is mid-GOP: its first key is
		// index 60.
		"clean end": {
			steps0: idxFrames(0, 50, 0, fakeT0),
			from1:  50,
			events: []EventKind{EventActive, EventFailed, EventActive},
		},
		// Input 0 breaks its timeline mid-GOP and fails before its next key:
		// the Clock drops its frames 50-54; the backup starts at 55.
		"lost tail": {
			steps0: append(append(idxFrames(0, 50, 0, fakeT0), step{err: ingest.ErrDiscontinuity}), idxFrames(50, 55, 0, fakeT0)...),
			from1:  55,
			events: []EventKind{EventActive, EventDiscontinuity, EventFailed, EventActive},
			lost0:  5,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ft, ev := newFakeTime(), newEvents()
			r0 := newFakeReader(videoOnly, ingest.Spec{}, append(append([]step{}, tc.steps0...), step{err: boom}), false)
			m := byURL{
				"fake://0": one(r0),
				"fake://1": standby(videoOnly, func(n int) (ingest.Reader, error) {
					if n > 1 {
						return nil, errors.New("reopened")
					}
					return newFakeReader(videoOnly, ingest.Spec{}, idxFrames(tc.from1, 150, 1, fakeT0), true), nil
				}),
			}
			h := hub.New(hub.Options{})
			sub := h.Subscribe("t", 1<<16)
			opt := multiOptions(ft, m, ev, "fake://0", "fake://1")
			opt.EndOnEOF = true
			var p *Pipeline
			var atFailure clock.Stats // the Clock's drops up to input 0's failure
			opt.OnEvent = func(e Event) {
				if e.Kind == EventFailed {
					atFailure = p.Stats().Clock
				}
				ev.on(e)
			}
			p = New(h, opt)
			require.NoError(t, p.Run(timeout(t)))

			require.Equal(t, tc.events, ev.kinds())
			failed := ev.evs[len(ev.evs)-2]
			assert.Equal(t, 0, failed.Input)
			assert.ErrorIs(t, failed.Err, boom)
			assert.Equal(t, 1, ev.evs[len(ev.evs)-1].Input)
			st := p.Stats()
			assert.Equal(t, uint64(1), st.Switches)
			assert.True(t, m["fake://1"].reader(0).isClosed(), "the start-up standby reader is closed")
			assert.True(t, r0.isClosed())

			ds := drain(sub)
			assert.Len(t, epochsOf(ds), 2, "exactly one more epoch")
			assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
			seen := map[int]bool{}
			written0 := 0
			for _, d := range ds {
				seen[frameIdx(d.Frame)] = true
				if d.Frame.Data[0][0] == 0 {
					written0++
				}
			}
			gap := 0
			for i := range 150 {
				if !seen[i] {
					gap++
				}
			}
			unwritten0 := r0.framesRead() - written0
			dropped1 := int(st.Clock.DroppedBeforeKey - atFailure.DroppedBeforeKey)
			assert.Equal(t, tc.lost0, unwritten0, "input 0's frames read but not written")
			assert.Equal(t, gap, unwritten0+dropped1, "the gap is input 0's unwritten frames plus input 1's frames before its key")
			assert.Equal(t, 10, gap, "indexes 50-59")
		})
	}
}

func TestFailoverTrackMismatch(t *testing.T) {
	boom := errors.New("boom")
	primary := func() *fakeReader {
		steps := av{video: 0, audio: 1, gop: 25, span: 2 * media.ClockRate, arrival: realTime}.steps()
		return newFakeReader(videoAud, ingest.Spec{}, append(steps, step{err: boom}), false)
	}
	run := func(t *testing.T, backup *fakeReader) (*Pipeline, []hub.Delivery, *events) {
		t.Helper()
		ft, ev := newFakeTime(), newEvents()
		m := byURL{
			"fake://0": one(primary()),
			"fake://1": standby(backup.kinds, func(int) (ingest.Reader, error) { return backup, nil }),
		}
		h := hub.New(hub.Options{})
		sub := h.Subscribe("t", 1<<16)
		opt := multiOptions(ft, m, ev, "fake://0", "fake://1")
		opt.Layout = videoAud
		opt.EndOnEOF = true
		p := New(h, opt)
		require.NoError(t, p.Run(timeout(t)))
		require.Equal(t, []EventKind{EventActive, EventFailed, EventActive}, ev.kinds())
		assert.Equal(t, uint64(1), p.Stats().Switches)
		ds := drain(sub)
		assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
		return p, ds, ev
	}
	backupAV := av{video: 0, audio: 1, gop: 25, start: 2 * media.ClockRate, span: 3 * media.ClockRate, arrival: realTime}

	t.Run("video only", func(t *testing.T) {
		steps := av{video: 0, audio: -1, gop: 25, start: 2 * media.ClockRate, span: 3 * media.ClockRate, arrival: realTime}.steps()
		p, ds, _ := run(t, newFakeReader(videoOnly, ingest.Spec{}, steps, true))
		epochs := epochsOf(ds)
		require.Len(t, epochs, 2, "one epoch more")
		var first, second uint32 = 1 << 31, 0
		for e := range epochs {
			first, second = min(first, e), max(second, e)
		}
		n := 0
		for _, d := range ds {
			if d.Frame.Epoch == second {
				assert.Equal(t, 0, d.Frame.Track, "audio stops after the switch")
				n++
			}
		}
		assert.Equal(t, len(steps), n, "every backup frame on layout track 0")
		assert.Zero(t, p.Stats().Unmapped)
	})
	t.Run("two audio", func(t *testing.T) {
		var steps []step
		extra := 0
		for _, s := range backupAV.steps() {
			steps = append(steps, s)
			if s.f.Track == 1 {
				e := s
				e.f.Track, e.f.Config, e.f.Data = 2, audioCfg2, [][]byte{{3}}
				steps = append(steps, e)
				extra++
			}
		}
		backup := newFakeReader([]media.Kind{media.KindVideo, media.KindAudio, media.KindAudio}, ingest.Spec{}, steps, true)
		p, ds, _ := run(t, backup)
		require.Len(t, epochsOf(ds), 2)
		for _, d := range ds {
			assert.NotEqual(t, byte(3), d.Frame.Data[0][0], "the second audio track is dropped")
		}
		assert.Equal(t, uint64(extra), p.Stats().Unmapped)
	})
}

func TestPipelineStallFailover(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	// Input 0 sends 2 s, then goes silent with its connection open.
	r0 := newFakeReader(videoOnly, ingest.Spec{}, vFrames(50, 25, 0), false)
	r1 := newFakeReader(videoOnly, ingest.Spec{}, vFrames(50, 25, 50*vDur), false)
	m := byURL{
		"fake://0": one(r0),
		"fake://1": standby(videoOnly, func(int) (ingest.Reader, error) { return r1, nil }),
	}
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1")
	var mu sync.Mutex
	var switched time.Time
	opt.OnEvent = func(e Event) {
		if e.Kind == EventActive && e.Input == 1 {
			mu.Lock()
			switched = ft.Now()
			mu.Unlock()
		}
		ev.on(e)
	}
	p := New(hub.New(hub.Options{}), opt)
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, Event{Kind: EventActive, Input: 0}, e)
	<-r0.blocked
	silent := ft.Now()
	ft.Advance(defaultStallTimeout)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	require.Equal(t, 0, e.Input)
	assert.ErrorIs(t, e.Err, errStalled)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, Event{Kind: EventActive, Input: 1}, e)
	mu.Lock()
	assert.LessOrEqual(t, switched.Sub(silent), defaultStallTimeout+time.Second)
	mu.Unlock()
	assert.True(t, r0.isClosed(), "the stalled reader is closed")
	<-r1.blocked
	goroutinesBack(t, base+2) // Run and input 1's read goroutine: input 0's is gone
	st := p.Stats()
	assert.Equal(t, 1, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	goroutinesBack(t, base)
}

func TestAllInputsDown(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	var mu sync.Mutex
	opens := map[string][]time.Duration{}
	opt := multiOptions(ft, nil, ev, "fake://0", "fake://1")
	opt.Open = func(_ context.Context, u string) (ingest.Reader, error) {
		mu.Lock()
		opens[u] = append(opens[u], ft.Now().Sub(fakeT0))
		mu.Unlock()
		return nil, errors.New("down")
	}
	p := New(hub.New(hub.Options{}), opt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	want := []time.Duration{0, ms(500), ms(1500), ms(3500), ms(7500), ms(15500), ms(25500), ms(35500), ms(45500), ms(55500)}
	for k, at := range want {
		if k > 0 {
			ft.Advance(at - time.Millisecond - ft.Now().Sub(fakeT0))
			ft.Advance(time.Millisecond)
		}
		for range 2 { // both inputs fail at each round
			e, ok := ev.next()
			require.True(t, ok, "round %d", k)
			require.Equal(t, EventFailed, e.Kind)
		}
	}
	ft.Advance(60*time.Second - ft.Now().Sub(fakeT0))
	time.Sleep(50 * time.Millisecond) // an open due by 60 s would happen now
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, want, opens["fake://0"])
	assert.Equal(t, want, opens["fake://1"])
	assert.Equal(t, 20, len(opens["fake://0"])+len(opens["fake://1"]), "at most 10 opens per input in 60 s")
}

func TestSinglePassiveStartsLate(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	const late = 30 * time.Second
	steps := av{video: 0, audio: 1, gop: 25, span: 3 * media.ClockRate, arrival: func(dts int64) time.Time {
		return fakeT0.Add(late + media.ToDuration(dts))
	}}.steps()
	r := newFakeReader(videoAud, ingest.Spec{Live: true}, steps, true)
	start := make(chan struct{})
	r.gates = map[int]chan struct{}{0: start} // the publisher connects 30 s after Run
	o := one(r)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, o, ev)
	opt.Inputs = []string{"passive://0"}
	opt.Parse = passiveParse
	opt.EndOnEOF = true
	var activeAt time.Time
	opt.OnEvent = func(e Event) {
		if e.Kind == EventActive {
			activeAt = ft.Now()
		}
		ev.on(e)
	}
	p := New(h, opt)
	res := make(chan error, 1)
	go func() { res <- p.Run(timeout(t)) }()

	<-r.blocked
	ft.Advance(late)
	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind, "no frame within the start timeout")
	assert.ErrorIs(t, e.Err, errNoStart)
	assert.False(t, r.isClosed(), "the passive input keeps listening")
	assert.Zero(t, p.Stats().Switches)
	close(start)
	require.NoError(t, <-res)

	assert.Equal(t, []EventKind{EventFailed, EventActive}, ev.kinds())
	assert.Equal(t, late, activeAt.Sub(fakeT0), "active at its first frame")
	assert.Equal(t, 1, o.count(), "never reopened")
	assert.Zero(t, p.Stats().Switches)
	ds := drain(sub)
	assert.Len(t, epochsOf(ds), 1)
	assert.Len(t, ds, len(steps))
}

func TestSinglePassiveAfterStall(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	const pause = 6 * time.Second
	steps := append(vFrames(50, 25, 0), vFrames(50, 25, 50*vDur)...)
	for i := 50; i < len(steps); i++ {
		steps[i].f.Arrival = steps[i].f.Arrival.Add(pause)
	}
	r := newFakeReader(videoOnly, ingest.Spec{Live: true}, steps, true)
	resume := make(chan struct{})
	r.gates = map[int]chan struct{}{50: resume}
	o := one(r)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, o, ev)
	opt.Inputs = []string{"passive://0"}
	opt.Parse = passiveParse
	opt.EndOnEOF = true
	p := New(h, opt)
	res := make(chan error, 1)
	go func() { res <- p.Run(timeout(t)) }()

	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, EventActive, e.Kind)
	<-r.blocked
	ft.Advance(defaultStallTimeout)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	assert.ErrorIs(t, e.Err, errStalled)
	assert.False(t, r.isClosed(), "the passive input keeps listening")
	assert.Equal(t, -1, p.Stats().Active)
	ft.Advance(pause - defaultStallTimeout)
	close(resume)
	require.NoError(t, <-res)

	assert.Equal(t, []EventKind{EventActive, EventFailed, EventActive}, ev.kinds())
	assert.Equal(t, 1, o.count(), "never reopened")
	assert.Zero(t, p.Stats().Switches)
	ds := drain(sub)
	assert.Len(t, epochsOf(ds), 2, "one epoch at the resume")
	assert.Len(t, ds, len(steps))
}

func TestFailoverDriftFollowsInput(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	boom := errors.New("boom")
	const skew = 0.1 // arrival runs 10 % slower than the source on both inputs
	slow := func(base time.Time, start int64) func(int64) time.Time {
		return func(dts int64) time.Time {
			return base.Add(time.Duration(float64(media.ToDuration(dts-start)) * (1 + skew)))
		}
	}
	const span0 = 30 * media.ClockRate
	steps0 := av{video: 0, audio: -1, gop: 25, span: span0, arrival: slow(fakeT0, 0)}.steps()
	r0 := newFakeReader(videoOnly, ingest.Spec{}, append(steps0, step{err: boom}), false)
	base1 := fakeT0.Add(33 * time.Second)
	steps1 := av{video: 0, audio: -1, gop: 25, start: span0, span: 60 * media.ClockRate, arrival: slow(base1, span0)}.steps()
	m := byURL{
		"fake://0": one(r0),
		"fake://1": standby(videoOnly, func(int) (ingest.Reader, error) {
			return newFakeReader(videoOnly, ingest.Spec{Live: true}, steps1, true), nil
		}),
	}
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1")
	opt.EndOnEOF = true
	var p *Pipeline
	var atFailure uint64
	opt.OnEvent = func(e Event) {
		if e.Kind == EventFailed {
			atFailure = p.Stats().Clock.DriftBreaks
		}
		ev.on(e)
	}
	p = New(hub.New(hub.Options{}), opt)
	require.NoError(t, p.Run(timeout(t)))
	require.Equal(t, []EventKind{EventActive, EventFailed, EventActive}, ev.kinds())
	assert.Zero(t, atFailure, "no drift re-anchoring on the not-live primary")
	assert.Positive(t, p.Stats().Clock.DriftBreaks, "drift re-anchoring on the live backup")
}

func TestPipelinePassiveReconnect(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	const pause = 2 * time.Second
	// The publisher sends 2 s, pauses 2 s, and a new publisher starts its
	// own timeline from zero: the reader reports one discontinuity.
	second := av{video: 0, audio: -1, gop: 25, span: 3 * media.ClockRate, arrival: func(dts int64) time.Time {
		return fakeT0.Add(pause + media.ToDuration(dts))
	}}.steps()
	first := vFrames(50, 25, 0)
	steps := append(append(append([]step{}, first...), step{err: ingest.ErrDiscontinuity}), second...)
	r := newFakeReader(videoOnly, ingest.Spec{Live: true}, steps, true)
	r.onRead = func(i int) {
		if i == len(first) {
			ft.Advance(pause)
		}
	}
	m := byURL{
		"passive://0": one(r),
		"fake://1":    standby(videoOnly, noReopen(t)),
	}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := multiOptions(ft, m, ev, "passive://0", "fake://1")
	opt.Parse = passiveParse
	opt.EndOnEOF = true
	p := New(h, opt)
	require.NoError(t, p.Run(timeout(t)))

	assert.Equal(t, []EventKind{EventActive, EventDiscontinuity}, ev.kinds(), "no failure, no switch")
	assert.Zero(t, p.Stats().Switches)
	ds := drain(sub)
	assert.Len(t, epochsOf(ds), 2, "the reconnect is one epoch")
	assert.Len(t, ds, len(first)+len(second))
	assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
}

// deadThenHealthy returns an opener whose first open is the start-up standby,
// whose opens up to dead fail as given (a reader that never delivers when
// err is nil) and whose later opens return a healthy, never-ending reader.
func deadThenHealthy(dead int, err error, healthy func() ingest.Reader) *opener {
	return standby(videoOnly, func(n int) (ingest.Reader, error) {
		if n <= dead {
			if err != nil {
				return nil, err
			}
			return newFakeReader(videoOnly, ingest.Spec{}, nil, false), nil
		}
		return healthy(), nil
	})
}

func TestFailoverSearchReachesLowest(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	boom := errors.New("boom")
	// Input 0 delivers 2 s, fails, and its reopens never deliver; input 1's
	// open succeeds but never delivers; input 2 is healthy.
	r0 := newFakeReader(videoOnly, ingest.Spec{}, append(vFrames(50, 25, 0), step{err: boom}), false)
	o0 := &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
		if n == 0 {
			return r0, nil
		}
		return newFakeReader(videoOnly, ingest.Spec{}, nil, false), nil
	}}
	m := byURL{
		"fake://0": o0,
		"fake://1": deadThenHealthy(1<<30, nil, nil),
		"fake://2": deadThenHealthy(0, nil, func() ingest.Reader {
			return newFakeReader(videoOnly, ingest.Spec{}, vFrames(50, 25, 50*vDur), false)
		}),
	}
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1", "fake://2")
	var mu sync.Mutex
	var activeAt time.Time
	var searchCalls []time.Duration // the After calls left when input 2 activates
	var opens0 int                  // input 0's opens when input 2 activates
	opt.OnEvent = func(e Event) {
		if e.Kind == EventActive && e.Input == 0 {
			// The start-up timers were armed before the activation. Drained
			// here, on the Run goroutine, the search's timers cannot be.
			drainCalls(ft)
		}
		if e.Kind == EventActive && e.Input == 2 {
			mu.Lock()
			activeAt = ft.Now()
			opens0 = o0.count()
			for len(ft.calls) > 0 {
				searchCalls = append(searchCalls, <-ft.calls)
			}
			mu.Unlock()
		}
		ev.on(e)
	}
	p := New(hub.New(hub.Options{}), opt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, Event{Kind: EventActive, Input: 0}, e)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	require.Equal(t, 0, e.Input)
	failedAt := ft.Now()
	// Input 1's try ends at its start timeout: advance once its timer is
	// armed. The calls checked at the end are the ones from here on.
	var calls []time.Duration
	for {
		d, ok := ft.nextCall()
		require.True(t, ok, "input 1's start timer")
		calls = append(calls, d)
		if d == defaultStartTimeout {
			break
		}
	}
	require.Eventually(t, func() bool { return m["fake://1"].count() == 2 }, 5*time.Second, time.Millisecond, "input 1 tried")
	ft.Advance(defaultStartTimeout)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	require.Equal(t, 1, e.Input, "input 1 fails its start")
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, Event{Kind: EventActive, Input: 2}, e, "the search goes on to input 2, not back to input 0")
	mu.Lock()
	assert.LessOrEqual(t, activeAt.Sub(failedAt), 2*defaultStartTimeout)
	// Once input 2 is active, input 0 is reopened as a probe.
	assert.Equal(t, 1, opens0, "input 0 not retried before input 2 was tried")
	mu.Unlock()
	assert.Equal(t, uint64(1), p.Stats().Switches)
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	// No timer was armed during the search for a retry it would not start
	// (input 0's and input 1's 0.5 s backoffs). Once input 2 is active, both
	// are probes and their retries are armed.
	mu.Lock()
	calls = append(calls, searchCalls...)
	mu.Unlock()
	assert.NotContains(t, calls, firstRetry)
}

func TestFailoverAllDeadThenRecover(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	boom := errors.New("boom")
	down := errors.New("down")
	r0 := newFakeReader(videoOnly, ingest.Spec{}, append(vFrames(50, 25, 0), step{err: boom}), false)
	var mu sync.Mutex
	opens := map[string][]time.Duration{}
	record := func(u string, o *opener) *opener {
		next := o.next
		o.next = func(n int, ctx context.Context) (ingest.Reader, error) {
			mu.Lock()
			opens[u] = append(opens[u], ft.Now().Sub(fakeT0))
			mu.Unlock()
			return next(n, ctx)
		}
		return o
	}
	m := byURL{
		"fake://0": record("0", &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n == 0 {
				return r0, nil
			}
			return nil, down
		}}),
		// Input 1 is down for its search try, then recovers.
		"fake://1": record("1", deadThenHealthy(1, down, func() ingest.Reader {
			return newFakeReader(videoOnly, ingest.Spec{}, vFrames(50, 25, 50*vDur), false)
		})),
		"fake://2": record("2", deadThenHealthy(1<<30, down, nil)),
	}
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1", "fake://2")
	p := New(hub.New(hub.Options{}), opt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	// Input 0 fails at 0 s; the search tries 1, then 2: all three are dead.
	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, Event{Kind: EventActive, Input: 0}, e)
	for _, in := range []int{0, 1, 2} {
		e, ok = ev.next()
		require.True(t, ok)
		require.Equal(t, EventFailed, e.Kind)
		require.Equal(t, in, e.Input)
	}
	// Every input waits 0.5 s; then all three reopen at once and input 1,
	// recovered, becomes active. Inputs 0 and 2 may be closed by that
	// activation before their open errors are taken, so they may not report
	// EventFailed.
	waitCall(t, ft, firstRetry)
	ft.Advance(firstRetry)
	for {
		e, ok = ev.next()
		require.True(t, ok)
		if e.Kind == EventActive {
			assert.Equal(t, Event{Kind: EventActive, Input: 1}, e)
			break
		}
		require.Equal(t, EventFailed, e.Kind)
	}
	assert.Equal(t, uint64(1), p.Stats().Switches)
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	mu.Lock()
	defer mu.Unlock()
	half := 500 * time.Millisecond
	assert.Equal(t, []time.Duration{0, half}, opens["0"])
	assert.Equal(t, []time.Duration{0, 0, half}, opens["1"])
	assert.Equal(t, []time.Duration{0, 0, half}, opens["2"])
}

func TestBackgroundPassiveNotJudged(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	// Input 0 is active throughout; the passive input 1 fails its first open,
	// then listens again after its backoff, with frames that are dropped.
	r0 := newFakeReader(videoOnly, ingest.Spec{}, vFrames(50, 25, 0), false)
	r1 := newFakeReader(videoOnly, ingest.Spec{Live: true}, vFrames(25, 25, 0), false)
	m := byURL{
		"fake://0": one(r0),
		"passive://1": &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n == 0 {
				return nil, errors.New("port busy")
			}
			return r1, nil
		}},
	}
	opt := multiOptions(ft, m, ev, "fake://0", "passive://1")
	opt.Parse = passiveParse
	opt.StallTimeout = time.Minute // input 0 never stalls in this test
	p := New(hub.New(hub.Options{}), opt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	got := map[EventKind]int{}
	for range 2 {
		e, ok := ev.next()
		require.True(t, ok)
		got[e.Kind] = e.Input
	}
	require.Equal(t, map[EventKind]int{EventActive: 0, EventFailed: 1}, got)
	waitCall(t, ft, firstRetry) // the listener's retry timer is armed
	ft.Advance(firstRetry)
	<-r1.blocked // the listener's frames were all read
	ft.Advance(defaultStartTimeout + time.Second)
	select {
	case e := <-ev.c:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	assert.False(t, r1.isClosed(), "the background listener keeps listening")
	assert.Equal(t, 0, p.Stats().Active)
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
}
