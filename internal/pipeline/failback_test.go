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

// frameDur is the duration of one video frame (vDur).
var frameDur = media.ToDuration(vDur)

// timedReader is a live reader on the fake clock: it returns each step once
// the fake time reaches the step's arrival (an error step's too), then blocks
// until Close.
type timedReader struct {
	ft     *fakeTime
	steps  []step
	closed chan struct{}
	once   sync.Once

	mu sync.Mutex
	i  int // steps returned
}

func (r *timedReader) Read() (ingest.Frame, error) {
	r.mu.Lock()
	i := r.i
	r.mu.Unlock()
	if i >= len(r.steps) {
		<-r.closed
		return ingest.Frame{}, errFakeClosed
	}
	s := r.steps[i]
	select {
	case <-r.ft.until(s.f.Arrival):
	case <-r.closed:
		return ingest.Frame{}, errFakeClosed
	}
	r.mu.Lock()
	r.i++
	r.mu.Unlock()
	return s.f, s.err
}

func (r *timedReader) Kinds() []media.Kind      { return videoOnly }
func (r *timedReader) Spec() ingest.Spec        { return ingest.Spec{} }
func (r *timedReader) Stats() map[string]uint64 { return nil }

func (r *timedReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (r *timedReader) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

// returned returns the steps r returned from Read.
func (r *timedReader) returned() []step {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steps[:r.i]
}

// due reports whether r, open, has a step whose arrival is at or before now
// that it has not returned yet.
func (r *timedReader) due(now time.Time) bool {
	if r.isClosed() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.i < len(r.steps) && !r.steps[r.i].f.Arrival.After(now)
}

// timedSet makes and tracks the timed readers of a test.
type timedSet struct {
	ft *fakeTime
	mu sync.Mutex
	rs []*timedReader
}

func (s *timedSet) reader(steps []step) *timedReader {
	r := &timedReader{ft: s.ft, steps: steps, closed: make(chan struct{})}
	s.mu.Lock()
	s.rs = append(s.rs, r)
	s.mu.Unlock()
	return r
}

func (s *timedSet) behind() bool {
	now := s.ft.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.rs, func(r *timedReader) bool { return r.due(now) })
}

// stepTo advances the fake time one frame duration at a time until at after
// fakeT0. After each step it waits until every open timed reader has
// returned its due steps, then yields, so Run keeps up with the time.
func stepTo(t *testing.T, s *timedSet, at time.Duration) {
	t.Helper()
	for s.ft.Now().Sub(fakeT0) < at {
		s.ft.Advance(frameDur)
		deadline := time.Now().Add(5 * time.Second)
		for s.behind() {
			require.True(t, time.Now().Before(deadline), "readers behind at %v", s.ft.Now().Sub(fakeT0))
			time.Sleep(20 * time.Microsecond)
		}
		time.Sleep(50 * time.Microsecond) // Run takes the timers and opens due now
	}
}

// source is a live video source on the fake clock: frame idx has DTS
// idx×vDur and arrives at fakeT0 plus its DTS; it is a key when idx ≡ phase
// (mod gop); its Data is {marker, idx} (frameIdx).
type source struct {
	marker     byte
	gop, phase int
}

// frames returns the frames arriving in [from, to) after fakeT0.
func (s source) frames(from, to time.Duration) []step {
	var out []step
	for i := int((from + frameDur - 1) / frameDur); time.Duration(i)*frameDur < to; i++ {
		d := int64(i) * vDur
		f := media.Frame{
			Track: 0, Config: videoCfg, DTS: d, PTS: d,
			Key:  ((i-s.phase)%s.gop+s.gop)%s.gop == 0,
			Data: [][]byte{{s.marker, byte(i >> 8), byte(i)}},
		}
		out = append(out, step{f: ingest.Frame{Frame: f, Arrival: fakeT0.Add(time.Duration(i) * frameDur)}})
	}
	return out
}

// failAt is a read error at at after fakeT0.
func failAt(at time.Duration, err error) step {
	return step{f: ingest.Frame{Arrival: fakeT0.Add(at)}, err: err}
}

// elapsed is the fake time since fakeT0.
func (f *fakeTime) elapsed() time.Duration { return f.Now().Sub(fakeT0) }

// firstTimed returns the first reader o opened.
func (o *opener) firstTimed() *timedReader {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.readers[0].(*timedReader)
}

func (o *opener) lastTimed(t *testing.T) *timedReader {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	require.NotEmpty(t, o.readers, "never opened")
	return o.readers[len(o.readers)-1].(*timedReader)
}

// waitActive waits for EventActive, which must be input in's, skipping
// failures.
func waitActive(t *testing.T, ev *events, in int) {
	t.Helper()
	for {
		e, ok := ev.next()
		require.True(t, ok, "no EventActive")
		if e.Kind == EventActive {
			require.Equal(t, in, e.Input)
			return
		}
	}
}

// actives returns the inputs of the EventActive events, in order.
func (e *events) actives() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []int
	for _, ev := range e.evs {
		if ev.Kind == EventActive {
			out = append(out, ev.Input)
		}
	}
	return out
}

// byEpoch groups the delivered frames by epoch, in epoch order.
func byEpoch(ds []hub.Delivery) [][]media.Frame {
	var ids []uint32
	m := map[uint32][]media.Frame{}
	for _, d := range ds {
		if _, ok := m[d.Frame.Epoch]; !ok {
			ids = append(ids, d.Frame.Epoch)
		}
		m[d.Frame.Epoch] = append(m[d.Frame.Epoch], d.Frame)
	}
	slices.Sort(ids)
	out := make([][]media.Frame, 0, len(ids))
	for _, id := range ids {
		out = append(out, m[id])
	}
	return out
}

func (p *Pipeline) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probePeak
}

// startRun runs p in the background; stop cancels it and checks that Run
// returned the cancellation.
func startRun(t *testing.T, p *Pipeline) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-res:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
	t.Cleanup(stop)
	return stop
}

// checkFailback checks the hub after a fail-back from marker from to
// marker to at source index at: two epochs, the second starting with the
// key at, every frame from its input, its indexes consecutive.
func checkFailback(t *testing.T, ds []hub.Delivery, from, to byte, at int) {
	t.Helper()
	assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
	epochs := byEpoch(ds)
	require.Equal(t, 2, len(epochs), "the fail-back is one epoch")
	for _, f := range epochs[0] {
		require.Equal(t, from, f.Data[0][0], "the first epoch is the backup's")
	}
	second := epochs[1]
	require.NotEmpty(t, second)
	assert.True(t, second[0].Key, "the new epoch starts with a key")
	assert.Equal(t, at, frameIdx(second[0]), "the switch is at the key after the hold")
	for i, f := range second {
		require.Equal(t, to, f.Data[0][0], "the new epoch is the recovered input's")
		require.Equal(t, at+i, frameIdx(f), "no frame lost or repeated")
	}
}

func TestFailbackOnKeyframe(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	down := errors.New("down")
	src0, src1 := source{marker: 0, gop: idxGOP}, source{marker: 1, gop: idxGOP}
	m := byURL{
		// Input 0 is down until 3 s; it reopens at 3.5 s and its frames
		// resume at 5 s: its hold ends at 15 s, between its keys at 14.4 s
		// and 15.6 s (index 390).
		"fake://0": {next: func(int, context.Context) (ingest.Reader, error) {
			if ft.elapsed() < 3*time.Second {
				return nil, down
			}
			return set.reader(src0.frames(5*time.Second, time.Minute)), nil
		}},
		"fake://1": {next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n > 0 {
				return nil, errors.New("reopened")
			}
			return set.reader(src1.frames(0, time.Minute)), nil
		}},
	}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1")
	opt.Layout = videoOnly
	p := New(h, opt)
	stop := startRun(t, p)

	waitActive(t, ev, 1)
	stepTo(t, set, 15*time.Second)
	assert.Equal(t, 1, p.Stats().Active, "no switch before the hold and the key after it")
	stepTo(t, set, 18*time.Second)
	st := p.Stats()
	stop()

	assert.Equal(t, []int{1, 0}, ev.actives())
	assert.Equal(t, 0, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	assert.True(t, m["fake://1"].firstTimed().isClosed(), "the backup is closed")
	assert.Equal(t, 4, m["fake://0"].count(), "opens at 0, 0.5, 1.5 and 3.5 s")
	checkFailback(t, drain(sub), 1, 0, 390)
}

func TestFailoverFlappingPrimary(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	down, dropped := errors.New("down"), errors.New("dropped")
	const (
		period = 6 * time.Second
		up     = 3 * time.Second
	)
	src0, src1 := source{marker: 0, gop: idxGOP}, source{marker: 1, gop: idxGOP}
	m := byURL{
		// The primary is up for the first 3 s of every 6 s: an open in that
		// window reads until its end, when the connection drops.
		"fake://0": {next: func(int, context.Context) (ingest.Reader, error) {
			now := ft.elapsed()
			start := now - now%period
			if now-start >= up {
				return nil, down
			}
			return set.reader(append(src0.frames(now, start+up), failAt(start+up, dropped))), nil
		}},
		"fake://1": standby(videoOnly, func(int) (ingest.Reader, error) {
			return set.reader(src1.frames(ft.elapsed(), 3*time.Minute)), nil
		}),
	}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1")
	opt.Layout = videoOnly
	p := New(h, opt)
	stop := startRun(t, p)

	waitActive(t, ev, 0)
	stepTo(t, set, 120*time.Second)
	st := p.Stats()
	stop()

	assert.Equal(t, []int{0, 1}, ev.actives(), "one switch to the backup, no fail-back")
	assert.Equal(t, 1, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	assert.GreaterOrEqual(t, m["fake://0"].count(), 10, "the primary is probed while the backup is active")
	assert.Positive(t, p.peak(), "the probes buffered frames")
	assert.LessOrEqual(t, p.peak(), idxGOP, "a probe buffers at most one GOP")
	ds := drain(sub)
	assert.Len(t, epochsOf(ds), 2, "the switch is one epoch")
	assert.Nil(t, conform.CheckFrames(frames(ds), conform.FrameOptions{}))
}

func TestFailbackPrefersHighest(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	down := errors.New("down")
	// Inputs 0 and 1 are down until 3 s, reopen at 3.5 s and resume at 5 s:
	// both are eligible from 15 s. Input 1's next key comes first (15.2 s,
	// index 380), input 0's at 15.6 s (index 390).
	recovering := func(src source) *opener {
		return &opener{next: func(int, context.Context) (ingest.Reader, error) {
			if ft.elapsed() < 3*time.Second {
				return nil, down
			}
			return set.reader(src.frames(5*time.Second, time.Minute)), nil
		}}
	}
	m := byURL{
		"fake://0": recovering(source{marker: 0, gop: idxGOP}),
		"fake://1": recovering(source{marker: 1, gop: idxGOP, phase: 20}),
		"fake://2": {next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n > 0 {
				return nil, errors.New("reopened")
			}
			return set.reader(source{marker: 2, gop: idxGOP}.frames(0, time.Minute)), nil
		}},
	}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1", "fake://2")
	opt.Layout = videoOnly
	p := New(h, opt)
	stop := startRun(t, p)

	waitActive(t, ev, 2)
	stepTo(t, set, 18*time.Second)
	st := p.Stats()
	stop()

	assert.Equal(t, []int{2, 0}, ev.actives(), "straight to the highest eligible input")
	assert.Equal(t, 0, st.Active)
	assert.Equal(t, uint64(1), st.Switches)
	r1 := m["fake://1"].lastTimed(t)
	assert.True(t, slices.ContainsFunc(r1.returned(), func(s step) bool { return frameIdx(s.f.Frame) == 380 }),
		"input 1 delivered its key while eligible")
	assert.True(t, r1.isClosed(), "the other probe is closed")
	assert.True(t, m["fake://2"].firstTimed().isClosed(), "the backup is closed")
	checkFailback(t, drain(sub), 2, 0, 390)
}

func TestFailbackPassiveHoldRestarts(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	src0 := source{marker: 0, gop: idxGOP}
	// The passive input's publisher sends from 2 s to 8 s, pauses 5 s (over
	// the stall limit) and resumes at 13 s: the hold restarts then and ends at
	// 23 s, before the key at 24 s (index 600).
	m := byURL{
		"passive://0": {next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n > 0 {
				return nil, errors.New("reopened")
			}
			return set.reader(append(src0.frames(2*time.Second, 8*time.Second), src0.frames(13*time.Second, time.Minute)...)), nil
		}},
		"fake://1": {next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n > 0 {
				return nil, errors.New("reopened")
			}
			return set.reader(source{marker: 1, gop: idxGOP}.frames(0, time.Minute)), nil
		}},
	}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := multiOptions(ft, m, ev, "passive://0", "fake://1")
	opt.Parse = passiveParse
	opt.Layout = videoOnly
	p := New(h, opt)
	stop := startRun(t, p)

	waitActive(t, ev, 1)
	stepTo(t, set, 23*time.Second)
	assert.Equal(t, 1, p.Stats().Active, "the pause restarted the hold")
	stepTo(t, set, 26*time.Second)
	st := p.Stats()
	stop()

	assert.Equal(t, []int{1, 0}, ev.actives())
	assert.Equal(t, uint64(1), st.Switches)
	assert.Equal(t, 1, m["passive://0"].count(), "the listener is never reopened")
	assert.True(t, m["fake://1"].firstTimed().isClosed(), "the backup is closed")
	checkFailback(t, drain(sub), 1, 0, 600)
}

func TestProbeGoroutinesExit(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	set := &timedSet{ft: ft}
	opening := make(chan struct{}, 1)
	m := byURL{
		// Input 0's open hangs until cancelled.
		"fake://0": {next: func(_ int, ctx context.Context) (ingest.Reader, error) {
			opening <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		// Input 1's first open fails; it reopens at 0.5 s and reads.
		"fake://1": {next: func(n int, _ context.Context) (ingest.Reader, error) {
			if n == 0 {
				return nil, errors.New("down")
			}
			return set.reader(source{marker: 1, gop: idxGOP}.frames(ft.elapsed(), time.Minute)), nil
		}},
		// The passive input 2's publisher starts at 1 s.
		"passive://2": {next: func(int, context.Context) (ingest.Reader, error) {
			return set.reader(source{marker: 2, gop: idxGOP}.frames(time.Second, time.Minute)), nil
		}},
		"fake://3": {next: func(int, context.Context) (ingest.Reader, error) {
			return set.reader(source{marker: 3, gop: idxGOP}.frames(0, time.Minute)), nil
		}},
	}
	opt := multiOptions(ft, m, ev, "fake://0", "fake://1", "passive://2", "fake://3")
	opt.Parse = passiveParse
	opt.Layout = videoOnly
	p := New(hub.New(hub.Options{}), opt)
	base := runtime.NumGoroutine()
	stop := startRun(t, p)

	waitActive(t, ev, 3)
	stepTo(t, set, 3*time.Second)
	<-opening
	assert.Equal(t, 3, p.Stats().Active)
	require.Equal(t, 2, m["fake://1"].count())
	r1, r2 := m["fake://1"].firstTimed(), m["passive://2"].firstTimed() // the failed open recorded no reader
	assert.NotEmpty(t, r1.returned(), "input 1's probe reads")
	assert.False(t, r1.isClosed())
	assert.NotEmpty(t, r2.returned(), "input 2's probe reads")
	assert.Equal(t, 1, m["fake://0"].count(), "input 0's probe is still opening")
	stop()
	goroutinesBack(t, base)
	assert.True(t, r1.isClosed())
	assert.True(t, r2.isClosed())
}
