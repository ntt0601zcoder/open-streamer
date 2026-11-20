package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/" + name)
	require.NoError(t, err)
	return b
}

// memOpen opens b as an in-memory TS input stamped by now (nil: time.Now);
// its Spec is zero (unpaced, not live).
func memOpen(b []byte, now func() time.Time) func(context.Context, string) (ingest.Reader, error) {
	return func(context.Context, string) (ingest.Reader, error) {
		return ingest.NewTSReader(io.NopCloser(bytes.NewReader(b)), now)
	}
}

// pacedReader overrides a reader's Spec with Pace.
type pacedReader struct{ ingest.Reader }

func (pacedReader) Spec() ingest.Spec { return ingest.Spec{Pace: true} }

func memPaced(b []byte, now func() time.Time) func(context.Context, string) (ingest.Reader, error) {
	open := memOpen(b, now)
	return func(ctx context.Context, u string) (ingest.Reader, error) {
		r, err := open(ctx, u)
		if err != nil {
			return nil, err
		}
		return pacedReader{r}, nil
	}
}

// memOptions runs b once from memory.
func memOptions(b []byte) Options {
	return Options{Inputs: []string{"mem"}, Open: memOpen(b, nil), Parse: noParse, EndOnEOF: true}
}

// loopURL writes b to a temporary file and returns its looping file URL.
func loopURL(t testing.TB, b []byte, query string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clip.ts")
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String()
}

func drain(s *hub.Sub) []hub.Delivery {
	var out []hub.Delivery
	for {
		select {
		case d, ok := <-s.C():
			if !ok {
				return out
			}
			out = append(out, d)
		default:
			return out
		}
	}
}

func TestRunWritesAnchorsBeforeFrames(t *testing.T) {
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	require.NoError(t, New(h, memOptions(fixture(t, "h264_aac_loop.ts"))).Run(context.Background()))
	seen := map[uint32]bool{}
	n := 0
	for _, d := range drain(sub) {
		n++
		if seen[d.Frame.Epoch] {
			continue
		}
		seen[d.Frame.Epoch] = true
		_, ok := h.Anchor(d.Frame.Epoch)
		assert.True(t, ok, "epoch %d", d.Frame.Epoch)
	}
	assert.Greater(t, n, 0)
	assert.NotEmpty(t, seen)
}

func TestRunAnchorExistsAtWrite(t *testing.T) {
	// A hub subscriber cannot run inside Write, so verify ordering by polling
	// concurrently: the first frame of each epoch must never arrive without it.
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	done := make(chan struct{})
	bad := make(chan uint32, 1)
	go func() {
		defer close(done)
		seen := map[uint32]bool{}
		for d := range sub.C() {
			if !seen[d.Frame.Epoch] {
				seen[d.Frame.Epoch] = true
				if _, ok := h.Anchor(d.Frame.Epoch); !ok {
					select {
					case bad <- d.Frame.Epoch:
					default:
					}
				}
			}
		}
	}()
	require.NoError(t, New(h, memOptions(fixture(t, "h264_aac_loop.ts"))).Run(context.Background()))
	h.Close()
	<-done
	assert.Empty(t, bad)
}

func TestRunFileLoop(t *testing.T) {
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loops := 0
	err := New(h, Options{
		Inputs: []string{loopURL(t, fixture(t, "h264_aac.ts"), "loop=1&pace=0")},
		OnEvent: func(ev Event) {
			if ev.Kind == EventDiscontinuity {
				if loops++; loops == 3 {
					cancel()
				}
			}
		},
	}).Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, map[uint32]bool{1: true, 2: true, 3: true}, func() map[uint32]bool {
		out := map[uint32]bool{}
		for e := range epochsOf(drain(sub)) {
			out[e] = true
		}
		return out
	}())
}

func TestRunPace(t *testing.T) {
	b := fixture(t, "h264_aac.ts")
	// Unpaced run to learn the DTS span.
	h0 := hub.New(hub.Options{})
	s0 := h0.Subscribe("t", 1<<16)
	require.NoError(t, New(h0, memOptions(b)).Run(context.Background()))
	var lo, hi int64
	first := true
	for _, d := range drain(s0) {
		if first || d.Frame.DTS < lo {
			lo = d.Frame.DTS
		}
		if first || d.Frame.DTS > hi {
			hi = d.Frame.DTS
		}
		first = false
	}
	require.False(t, first)

	now := time.Unix(1000, 0)
	var slept time.Duration
	h := hub.New(hub.Options{})
	opt := memOptions(b)
	opt.Now = func() time.Time { return now }
	opt.Open = memPaced(b, opt.Now)
	opt.Sleep = func(_ context.Context, d time.Duration) error {
		slept += d
		now = now.Add(d)
		return nil
	}
	require.NoError(t, New(h, opt).Run(context.Background()))
	sleptTicks := int64(slept) * media.ClockRate / int64(time.Second)
	assert.InDelta(t, hi-lo, sleptTicks, 3600)
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	b := fixture(t, "h264_aac.ts")
	opt := memOptions(b)
	opt.Open = memPaced(b, nil)
	opt.Sleep = func(ctx context.Context, _ time.Duration) error {
		calls++
		if calls == 3 {
			cancel()
		}
		return ctx.Err()
	}
	assert.ErrorIs(t, New(hub.New(hub.Options{}), opt).Run(ctx), context.Canceled)
}

func TestRunCancelDefaultSleep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := New(hub.New(hub.Options{}), Options{
		Inputs: []string{loopURL(t, fixture(t, "h264_aac.ts"), "loop=1")},
	}).Run(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunBadInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failed error
	opt := memOptions(bytes.Repeat([]byte("garbage!"), 500))
	opt.OnEvent = func(ev Event) {
		if ev.Kind == EventFailed && failed == nil {
			failed = ev.Err
			cancel()
		}
	}
	require.ErrorIs(t, New(hub.New(hub.Options{}), opt).Run(ctx), context.Canceled)
	require.Error(t, failed)
	assert.ErrorContains(t, failed, "pipeline: open:")
}

func TestRunNoInput(t *testing.T) {
	err := New(hub.New(hub.Options{}), Options{}).Run(context.Background())
	assert.ErrorContains(t, err, "pipeline:")
}

func TestRunBadURL(t *testing.T) {
	err := New(hub.New(hub.Options{}), Options{Inputs: []string{"gopher://x"}}).Run(context.Background())
	assert.ErrorContains(t, err, "pipeline: parse:")
}

func TestRunCancelBlockedRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{}, nil, false)
	res := make(chan error, 1)
	go func() {
		res <- New(hub.New(hub.Options{}), Options{
			Inputs: []string{"fake"},
			Parse:  noParse,
			Open:   func(context.Context, string) (ingest.Reader, error) { return r, nil },
		}).Run(ctx)
	}()
	select {
	case <-r.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Read never blocked")
	}
	cancel()
	select {
	case err := <-res:
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, r.isClosed())
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestPaceLongEpochNoOverflow(t *testing.T) {
	now := time.Unix(1000, 0)
	var slept time.Duration
	const span = 30 * 24 * 3600 * media.ClockRate
	w := &writer{
		h:    hub.New(hub.Options{}),
		pace: true,
		now:  func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error {
			slept = d
			return nil
		},
		anchored: 1,
		anchors:  map[uint32]media.Anchor{1: {Epoch: 1, Start: 0, Wall: now}, 0: {Epoch: 0}},
	}
	require.NoError(t, w.write(context.Background(), []media.Frame{{Epoch: 1, DTS: span, PTS: span}}))
	assert.Equal(t, 30*24*time.Hour, slept)
	assert.Len(t, w.anchors, 1, "older epochs are pruned")
}

// fakeOptions runs readers from o with a fake clock.
func fakeOptions(ft *fakeTime, o *opener, ev *events) Options {
	return Options{
		Inputs:  []string{"fake://0"},
		Open:    o.open,
		Parse:   noParse,
		Now:     ft.Now,
		Sleep:   ft.Sleep,
		After:   ft.After,
		OnEvent: ev.on,
	}
}

func one(r ingest.Reader) *opener {
	return &opener{next: func(int, context.Context) (ingest.Reader, error) { return r, nil }}
}

func TestPipelineLayoutMapping(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	steps := av{video: 1, audio: 0, gop: 25, span: 2 * media.ClockRate, arrival: realTime}.steps()
	// A second audio track (reader track 2) beside each first-track audio frame.
	var all []step
	extra := 0
	for _, s := range steps {
		all = append(all, s)
		if s.f.Track == 0 {
			e := s
			e.f.Track, e.f.Config, e.f.Data = 2, audioCfg2, [][]byte{{3}}
			all = append(all, e)
			extra++
		}
	}
	r := newFakeReader([]media.Kind{media.KindAudio, media.KindVideo, media.KindAudio}, ingest.Spec{}, all, true)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, one(r), ev)
	opt.Layout = []media.Kind{media.KindVideo, media.KindAudio}
	opt.EndOnEOF = true
	p := New(h, opt)
	require.NoError(t, p.Run(ev.failCtx(t)))

	ds := drain(sub)
	count := map[int]int{}
	for _, d := range ds {
		switch d.Frame.Data[0][0] {
		case 1:
			assert.Equal(t, 0, d.Frame.Track, "video → layout 0")
		case 2:
			assert.Equal(t, 1, d.Frame.Track, "first audio → layout 1")
		default:
			t.Fatalf("unmapped frame delivered: %+v", d.Frame)
		}
		count[d.Frame.Track]++
	}
	assert.Equal(t, len(steps), len(ds), "every mapped frame delivered")
	assert.Positive(t, count[0])
	assert.Positive(t, count[1])
	assert.Equal(t, uint64(extra), p.Stats().Unmapped)
}

func TestPipelineLayoutWaitsForInput0(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	steps := av{video: 1, audio: 0, gop: 25, span: 2 * media.ClockRate, arrival: realTime}.steps()
	r := newFakeReader([]media.Kind{media.KindAudio, media.KindVideo}, ingest.Spec{}, steps, true)
	release := make(chan struct{})
	var openErr error
	o := &opener{next: func(_ int, ctx context.Context) (ingest.Reader, error) {
		<-release
		openErr = ctx.Err()
		return r, nil
	}}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, o, ev)
	opt.EndOnEOF = true
	p := New(h, opt)
	ctx := ev.failCtx(t)
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	d, ok := ft.nextCall()
	require.True(t, ok)
	require.Equal(t, defaultStartTimeout, d, "start timer")
	ft.Advance(3 * time.Second) // input 0 opens 3 s in, within StartTimeout
	close(release)
	require.NoError(t, <-res)
	require.NoError(t, openErr, "the open was not aborted")

	ds := drain(sub)
	require.Len(t, ds, len(steps))
	for _, d := range ds {
		// The layout is input 0's kinds: audio is track 0, video track 1.
		assert.Equal(t, d.Frame.Kind() == media.KindVideo, d.Frame.Track == 1, "%+v", d.Frame)
	}
	assert.Equal(t, []EventKind{EventActive}, ev.kinds())
	assert.Zero(t, p.Stats().Unmapped)
}

func TestPipelineDiscontinuity(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	first := av{video: 0, audio: 1, gop: 25, span: 3 * media.ClockRate, arrival: realTime}.steps()
	// The timeline continues: only the reported break opens an epoch.
	second := av{video: 0, audio: 1, gop: 25, start: 3 * media.ClockRate, span: 3 * media.ClockRate, arrival: realTime}.steps()
	steps := append(append(append([]step{}, first...), step{err: ingest.ErrDiscontinuity}), second...)
	r := newFakeReader([]media.Kind{media.KindVideo, media.KindAudio}, ingest.Spec{}, steps, true)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, one(r), ev)
	opt.EndOnEOF = true
	p := New(h, opt)
	require.NoError(t, p.Run(ev.failCtx(t)))
	epochs := epochsOf(drain(sub))
	assert.Len(t, epochs, 2, "exactly one extra epoch")
	assert.Equal(t, []EventKind{EventActive, EventDiscontinuity}, ev.kinds())
	assert.Equal(t, 0, ev.evs[1].Input)
}

// goroutinesBack waits up to 1 s for the goroutine count to fall to base.
// It polls in the test goroutine: assert.Eventually would add its own.
func goroutinesBack(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), base, "goroutines over the baseline")
}

// expectRetries checks the After calls of attempts that each fail on their
// timer of length timeout, and the backoff waits between them, firing each
// but the last backoff wait, which Run is left waiting out.
func expectRetries(t *testing.T, ft *fakeTime, ev *events, timeout time.Duration, waits []time.Duration) {
	t.Helper()
	for i, w := range waits {
		d, ok := ft.nextCall()
		require.True(t, ok, "attempt %d timer", i+2)
		require.Equal(t, timeout, d, "attempt %d timer", i+2)
		ft.Advance(d)
		e, ok := ev.next()
		require.True(t, ok)
		require.Equal(t, EventFailed, e.Kind)
		d, ok = ft.nextCall()
		require.True(t, ok, "retry %d", i+1)
		require.Equal(t, w, d, "retry %d", i+1)
		if i < len(waits)-1 {
			ft.Advance(d)
		}
	}
}

func TestPipelineStartTimeout(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	var readers []*fakeReader
	var mu sync.Mutex
	aborted := make(chan struct{})
	o := &opener{next: func(n int, ctx context.Context) (ingest.Reader, error) {
		if n == 0 {
			// The first open hangs until its context ends.
			<-ctx.Done()
			close(aborted)
			return nil, ctx.Err()
		}
		r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{}, nil, false)
		mu.Lock()
		readers = append(readers, r)
		mu.Unlock()
		return r, nil
	}}
	h := hub.New(hub.Options{})
	p := New(h, fakeOptions(ft, o, ev))
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()

	d, ok := ft.nextCall()
	require.True(t, ok)
	require.Equal(t, defaultStartTimeout, d)
	ft.Advance(d)
	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	assert.ErrorIs(t, e.Err, errNoStart)
	<-aborted
	d, ok = ft.nextCall()
	require.True(t, ok)
	require.Equal(t, 500*time.Millisecond, d)
	goroutinesBack(t, base+1) // only Run is left while it waits out the backoff
	assert.Equal(t, -1, p.Stats().Active)
	ft.Advance(d)

	// The next attempts open a reader that never delivers.
	expectRetries(t, ft, ev, defaultStartTimeout,
		[]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second})
	mu.Lock()
	for i, r := range readers {
		assert.True(t, r.isClosed(), "reader %d closed", i)
	}
	assert.Len(t, readers, 6)
	mu.Unlock()
	goroutinesBack(t, base+1)
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
	goroutinesBack(t, base)
}

func TestPipelineStallTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		readerStall, want time.Duration
	}{
		"default":      {0, defaultStallTimeout},
		"reader raise": {6 * time.Second, 6 * time.Second},
		"reader lower": {time.Second, defaultStallTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			ft, ev := newFakeTime(), newEvents()
			steps := av{video: 0, audio: -1, gop: 25, span: 3 * vDur, arrival: realTime}.steps()
			require.Len(t, steps, 3)
			var readers []*fakeReader
			var mu sync.Mutex
			o := &opener{next: func(int, context.Context) (ingest.Reader, error) {
				r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{StallTimeout: tc.readerStall}, steps, false)
				mu.Lock()
				readers = append(readers, r)
				mu.Unlock()
				return r, nil
			}}
			p := New(hub.New(hub.Options{}), fakeOptions(ft, o, ev))
			base := runtime.NumGoroutine()
			ctx, cancel := context.WithCancel(context.Background())
			res := make(chan error, 1)
			go func() { res <- p.Run(ctx) }()

			attempt := func(wait time.Duration) {
				d, ok := ft.nextCall()
				require.True(t, ok)
				require.Equal(t, defaultStartTimeout, d)
				for range steps {
					d, ok = ft.nextCall()
					require.True(t, ok)
					require.Equal(t, tc.want, d, "stall limit")
				}
				e, ok := ev.next()
				require.True(t, ok)
				require.Equal(t, EventActive, e.Kind)
				ft.Advance(tc.want)
				e, ok = ev.next()
				require.True(t, ok)
				require.Equal(t, EventFailed, e.Kind)
				assert.ErrorIs(t, e.Err, errStalled)
				mu.Lock()
				assert.True(t, readers[len(readers)-1].isClosed())
				mu.Unlock()
				d, ok = ft.nextCall()
				require.True(t, ok)
				require.Equal(t, wait, d, "backoff")
				goroutinesBack(t, base+1)
				ft.Advance(d)
			}
			attempt(500 * time.Millisecond)
			attempt(time.Second)
			attempt(2 * time.Second)
			cancel()
			require.ErrorIs(t, <-res, context.Canceled)
			assert.Zero(t, p.Stats().Switches, "reopening the same input is no switch")
		})
	}
}

func TestPipelineBackoffReset(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	boom := errors.New("boom")
	healthy := av{video: 0, audio: -1, gop: 25, span: 12 * media.ClockRate, arrival: realTime}.steps()
	// The reopened input continues the timeline: only Break(true) opens its epoch.
	short := av{video: 0, audio: -1, gop: 25, start: 12 * media.ClockRate, span: media.ClockRate, arrival: realTime}.steps()
	o := &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
		switch n {
		case 2: // reads 12 s, then fails
			return newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{Pace: true}, append(append([]step{}, healthy...), step{err: boom}), false), nil
		case 4: // reads 1 s, then fails
			return newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{Pace: true}, append(append([]step{}, short...), step{err: boom}), false), nil
		default:
			return nil, boom
		}
	}}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	opt := fakeOptions(ft, o, ev)
	// Retry waits fire at once; start and stall timeouts, set apart from any
	// backoff value, never fire.
	opt.StartTimeout, opt.StallTimeout = 77*time.Second, 33*time.Second
	opt.After = func(d time.Duration) <-chan time.Time {
		if d != opt.StartTimeout && d != opt.StallTimeout {
			waits = append(waits, d)
			if len(waits) == 5 {
				cancel()
			}
			return ft.After(0)
		}
		return nil
	}
	p := New(h, opt)
	require.ErrorIs(t, p.Run(ctx), context.Canceled)
	assert.Equal(t, []time.Duration{500 * time.Millisecond, time.Second, 500 * time.Millisecond, time.Second, 2 * time.Second}, waits,
		"12 s of healthy reading reset the backoff, 1 s did not")
	assert.Equal(t, []EventKind{EventFailed, EventFailed, EventActive, EventFailed, EventFailed, EventActive, EventFailed}, ev.kinds())
	assert.Zero(t, p.Stats().Switches, "reactivating the same input is no switch")
	assert.Len(t, epochsOf(drain(sub)), 2, "the reopened input starts one new epoch")
}

func TestPipelinePacingNotStall(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	// A video keyframe every 2 s: each pacing sleep is 2 s, over the 1 s limit.
	steps := make([]step, 0, 10)
	for i := range 10 {
		dts := int64(i) * 2 * media.ClockRate
		f := media.Frame{Track: 0, Config: videoCfg, DTS: dts, PTS: dts, Key: true, Data: [][]byte{{1}}}
		steps = append(steps, step{f: ingest.Frame{Frame: f, Arrival: fakeT0}})
	}
	r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{Pace: true}, steps, true)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, one(r), ev)
	opt.StallTimeout = time.Second
	opt.EndOnEOF = true
	require.NoError(t, New(h, opt).Run(ev.failCtx(t)))
	assert.Equal(t, []EventKind{EventActive}, ev.kinds(), "no stall")
	assert.Len(t, drain(sub), len(steps))
	assert.GreaterOrEqual(t, ft.Now().Sub(fakeT0), 18*time.Second, "paced")
}

func TestPipelineDriftSettings(t *testing.T) {
	const skew = 0.1
	for name, tc := range map[string]struct {
		spec       ingest.Spec
		slow, fast bool // drift breaks expected for a source slower / faster than arrival
	}{
		"live unpaced": {spec: ingest.Spec{Live: true}, slow: true, fast: true},
		"live paced":   {spec: ingest.Spec{Live: true, Pace: true}, slow: true, fast: false},
		"not live":     {spec: ingest.Spec{}, slow: false, fast: false},
	} {
		for dir, s := range map[string]float64{"slow": skew, "fast": -skew} {
			t.Run(name+"/"+dir, func(t *testing.T) {
				t.Parallel()
				ft, ev := newFakeTime(), newEvents()
				arrival := func(dts int64) time.Time {
					return fakeT0.Add(time.Duration(float64(media.ToDuration(dts)) * (1 + s)))
				}
				steps := av{video: 0, audio: -1, gop: 25, span: 60 * media.ClockRate, arrival: arrival}.steps()
				r := newFakeReader([]media.Kind{media.KindVideo}, tc.spec, steps, true)
				opt := fakeOptions(ft, one(r), ev)
				opt.EndOnEOF = true
				opt.Sleep = func(context.Context, time.Duration) error { return nil }
				p := New(hub.New(hub.Options{}), opt)
				require.NoError(t, p.Run(ev.failCtx(t)))
				want := tc.slow
				if s < 0 {
					want = tc.fast
				}
				breaks := p.Stats().Clock.DriftBreaks
				if want {
					assert.Positive(t, breaks)
				} else {
					assert.Zero(t, breaks)
				}
			})
		}
	}
}

// The reader's arrival granularity reaches the Clock. Paced HLS with 6 s
// segments: a single fetch 2.5 s slow is no drift, a source that keeps
// falling behind is. A 10 s window would hold at most two arrivals of 6 s
// segments, too few to judge, so the slow source's breaks show the window
// was widened.
func TestPipelineArrivalGranularity(t *testing.T) {
	const seg = 6 * time.Second
	segOf := func(dts int64) int { return int(media.ToDuration(dts) / seg) }
	for name, tc := range map[string]struct {
		skew   float64
		slow   int // the segment fetched 2.5 s late; -1 for none
		breaks bool
	}{
		"slow fetch":  {skew: 0, slow: 5, breaks: false},
		"slow source": {skew: 0.05, slow: -1, breaks: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ft, ev := newFakeTime(), newEvents()
			arrival := func(dts int64) time.Time {
				k := segOf(dts)
				at := fakeT0.Add(time.Duration(float64(time.Duration(k+1)*seg)*(1+tc.skew)) + 100*time.Millisecond)
				if k == tc.slow {
					at = at.Add(2500 * time.Millisecond)
				}
				return at
			}
			steps := av{video: 0, audio: -1, gop: 25, span: 120 * media.ClockRate, arrival: arrival}.steps()
			spec := ingest.Spec{Live: true, Pace: true, ArrivalGranularity: seg}
			r := newFakeReader([]media.Kind{media.KindVideo}, spec, steps, true)
			opt := fakeOptions(ft, one(r), ev)
			opt.EndOnEOF = true
			opt.Sleep = func(context.Context, time.Duration) error { return nil }
			p := New(hub.New(hub.Options{}), opt)
			require.NoError(t, p.Run(ev.failCtx(t)))
			if tc.breaks {
				assert.Positive(t, p.Stats().Clock.DriftBreaks)
			} else {
				assert.Zero(t, p.Stats().Clock.DriftBreaks)
			}
		})
	}
}

func TestPipelineSniffedSpec(t *testing.T) {
	const u = "http://example.com/live"
	parsed, err := ingest.Parse(u)
	require.NoError(t, err)
	require.False(t, parsed.Pace, "Parse says unpaced")

	ft, ev := newFakeTime(), newEvents()
	steps := av{video: 0, audio: 1, gop: 25, span: 3 * media.ClockRate, arrival: realTime}.steps()
	r := newFakeReader([]media.Kind{media.KindVideo, media.KindAudio}, ingest.Spec{Pace: true, Live: true}, steps, true)
	var slept time.Duration
	opt := fakeOptions(ft, one(r), ev)
	opt.Inputs = []string{u}
	opt.Parse = nil
	opt.EndOnEOF = true
	opt.Sleep = func(ctx context.Context, d time.Duration) error {
		slept += d
		return ft.Sleep(ctx, d)
	}
	require.NoError(t, New(hub.New(hub.Options{}), opt).Run(ev.failCtx(t)))
	assert.InDelta(t, float64(3*time.Second), float64(slept), float64(100*time.Millisecond), "paced by the reader's Spec")
}

func TestPipelineBurstNoDrift(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	const gop = 2 * media.ClockRate
	// The first GOP arrives at once; the rest in real time behind it.
	arrival := func(dts int64) time.Time {
		if dts < gop {
			return fakeT0
		}
		return fakeT0.Add(media.ToDuration(dts - gop))
	}
	steps := av{video: 0, audio: 1, gop: 50, span: gop + 30*media.ClockRate, arrival: arrival}.steps()
	r := newFakeReader([]media.Kind{media.KindVideo, media.KindAudio}, ingest.Spec{Live: true}, steps, true)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, one(r), ev)
	opt.EndOnEOF = true
	p := New(h, opt)

	// Stats is safe while Run runs.
	stop := make(chan struct{})
	var polled sync.WaitGroup
	polled.Add(1)
	go func() {
		defer polled.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.Stats()
			}
		}
	}()
	require.NoError(t, p.Run(ev.failCtx(t)))
	close(stop)
	polled.Wait()

	st := p.Stats()
	assert.Zero(t, st.Clock.DriftBreaks)
	assert.Positive(t, st.Clock.DriftEvaluations, "drift judged")
	st.Clock.DriftEvaluations = 0
	assert.Equal(t, clock.Stats{}, st.Clock, "no frame dropped")
	assert.Len(t, drain(sub), len(steps), "every frame delivered")
	assert.Equal(t, -1, st.Active, "the ended input is no longer active")
}

func TestPipelineStatsWhileActive(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	steps := av{video: 0, audio: -1, gop: 25, span: media.ClockRate, arrival: realTime}.steps()
	r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{}, steps, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := New(hub.New(hub.Options{}), fakeOptions(ft, one(r), ev))
	assert.Equal(t, -1, p.Stats().Active, "before Run")
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()
	<-r.blocked
	require.Eventually(t, func() bool { return p.Stats().Active == 0 }, 5*time.Second, time.Millisecond)
	st := p.Stats()
	assert.Equal(t, uint64(len(steps)), st.Input["fake_steps"], "the active reader's counters")
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
}

// vFrames returns n video frames of reader track 0 from DTS start, a key
// every gop frames, arriving in real time.
func vFrames(n, gop int, start int64) []step {
	return av{video: 0, audio: -1, gop: gop, start: start, span: int64(n) * vDur, arrival: realTime}.steps()
}

// aFrames returns n audio frames of reader track 0 from DTS start, arriving
// in real time.
func aFrames(n int, start int64) []step {
	out := make([]step, 0, n)
	for i := range n {
		d := start + int64(i)*aDur
		f := media.Frame{Track: 0, Config: audioCfg, DTS: d, PTS: d, Key: true, Data: [][]byte{{2}}}
		out = append(out, step{f: ingest.Frame{Frame: f, Arrival: realTime(d)}})
	}
	return out
}

func TestPipelineDiscontinuityFlushNotStall(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	// Layout [video audio] with video alone: epoch 1 stays pending for its
	// 1 s window, so the discontinuity's flush paces 0.92 s of frames, over
	// the 0.5 s stall limit.
	held := vFrames(24, 250, 0)
	after := vFrames(50, 25, 24*vDur)
	steps := append(append(append([]step{}, held...), step{err: ingest.ErrDiscontinuity}), after...)
	r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{Pace: true}, steps, true)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, one(r), ev)
	opt.Layout = []media.Kind{media.KindVideo, media.KindAudio}
	opt.StallTimeout = 500 * time.Millisecond
	opt.EndOnEOF = true
	require.NoError(t, New(h, opt).Run(ev.failCtx(t)))
	assert.Equal(t, []EventKind{EventActive, EventDiscontinuity}, ev.kinds(), "no stall")
	assert.GreaterOrEqual(t, ft.Now().Sub(fakeT0), 900*time.Millisecond, "the flush was paced")
	assert.Len(t, drain(sub), len(held)+len(after))
}

func TestPipelineDiscontinuitiesOnlyStall(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	frames := vFrames(10, 25, 0)
	steps := append([]step{}, frames...)
	for range 1000 {
		steps = append(steps, step{err: ingest.ErrDiscontinuity})
	}
	r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{}, steps, true)
	// Each discontinuity takes 100 ms: they flush nothing, so the stall
	// limit runs from the last frame.
	r.onRead = func(i int) {
		if i >= len(frames) {
			ft.Advance(100 * time.Millisecond)
		}
	}
	opt := fakeOptions(ft, one(r), ev)
	p := New(hub.New(hub.Options{}), opt)
	require.ErrorIs(t, p.Run(ev.failCtx(t)), context.Canceled)
	e := ev.evs[len(ev.evs)-1]
	require.Equal(t, EventFailed, e.Kind)
	assert.ErrorIs(t, e.Err, errStalled)
	assert.Less(t, r.i, len(steps), "stalled before the reader ran out")
}

func TestPipelineUnmappedOnly(t *testing.T) {
	t.Run("never active", func(t *testing.T) {
		ft, ev := newFakeTime(), newEvents()
		r := newFakeReader([]media.Kind{media.KindAudio}, ingest.Spec{}, aFrames(25, 0), false)
		opt := fakeOptions(ft, one(r), ev)
		opt.Layout = []media.Kind{media.KindVideo}
		p := New(hub.New(hub.Options{}), opt)
		ctx := ev.failCtx(t)
		res := make(chan error, 1)
		go func() { res <- p.Run(ctx) }()
		<-r.blocked
		ft.Advance(defaultStartTimeout)
		require.ErrorIs(t, <-res, context.Canceled)
		require.Equal(t, []EventKind{EventFailed}, ev.kinds(), "unmapped frames do not activate")
		assert.ErrorIs(t, ev.evs[0].Err, errNoStart)
		assert.Equal(t, uint64(len(r.steps)), p.Stats().Unmapped)
	})
	t.Run("stalls", func(t *testing.T) {
		ft, ev := newFakeTime(), newEvents()
		video := vFrames(25, 25, 0)
		steps := append([]step{}, video...)
		for _, s := range aFrames(300, media.ClockRate) { // 11.5 s of audio outside the layout
			s.f.Track = 1
			steps = append(steps, s)
		}
		// io.EOF after the steps: an input that never stalls fails on it instead.
		r := newFakeReader([]media.Kind{media.KindVideo, media.KindAudio}, ingest.Spec{}, steps, true)
		r.onRead = func(i int) {
			if i >= len(video) {
				ft.Advance(media.ToDuration(aDur))
			}
		}
		opt := fakeOptions(ft, one(r), ev)
		opt.Layout = []media.Kind{media.KindVideo}
		p := New(hub.New(hub.Options{}), opt)
		require.ErrorIs(t, p.Run(ev.failCtx(t)), context.Canceled)
		require.Equal(t, []EventKind{EventActive, EventFailed}, ev.kinds())
		assert.ErrorIs(t, ev.evs[1].Err, errStalled)
		assert.Less(t, r.i, len(steps), "stalled while unmapped frames kept coming")
		assert.Positive(t, p.Stats().Unmapped)
	})
}

func TestPipelineHealthyUntilLastFrame(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	boom := errors.New("boom")
	var r *fakeReader
	o := &opener{next: func(n int, _ context.Context) (ingest.Reader, error) {
		if n == 2 {
			// Frames, then silence until an 11 s stall limit expires:
			// detected 11 s or more after activation, healthy for at most
			// the 2 s the frames span.
			r = newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{Pace: true, StallTimeout: 11 * time.Second}, vFrames(50, 25, 0), false)
			return r, nil
		}
		return nil, boom
	}}
	p := New(hub.New(hub.Options{}), fakeOptions(ft, o, ev))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()
	next := func(want time.Duration) {
		t.Helper()
		for {
			d, ok := ft.nextCall()
			require.True(t, ok)
			if d == want {
				return
			}
		}
	}
	next(500 * time.Millisecond)
	ft.Advance(500 * time.Millisecond)
	next(time.Second)
	ft.Advance(time.Second)
	e, ok := ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventActive, e.Kind)
	<-r.blocked
	for len(ft.calls) > 0 {
		<-ft.calls
	}
	ft.Advance(11 * time.Second)
	e, ok = ev.next()
	require.True(t, ok)
	require.Equal(t, EventFailed, e.Kind)
	require.ErrorIs(t, e.Err, errStalled)
	d, ok := ft.nextCall()
	require.True(t, ok)
	assert.Equal(t, 2*time.Second, d, "the silence before the stall is not healthy time")
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)
}

func TestPipelineFailureFlushUnpaced(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	boom := errors.New("boom")
	// Epoch 1 stays pending (video alone in a [video audio] layout), then
	// the input fails: its held frames are written at once, not paced.
	held := vFrames(24, 250, 0)
	r := newFakeReader([]media.Kind{media.KindVideo}, ingest.Spec{Pace: true}, append(append([]step{}, held...), step{err: boom}), false)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	sleeps := 0
	opt := fakeOptions(ft, one(r), ev)
	opt.Layout = []media.Kind{media.KindVideo, media.KindAudio}
	opt.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		return ft.Sleep(ctx, d)
	}
	require.ErrorIs(t, New(h, opt).Run(ev.failCtx(t)), context.Canceled)
	assert.Equal(t, []EventKind{EventActive, EventFailed}, ev.kinds())
	assert.Zero(t, sleeps, "EventFailed is not delayed by pacing")
	assert.Equal(t, fakeT0, ft.Now())
	assert.Len(t, drain(sub), len(held), "the held frames are written")
}

// A reader whose Kinds() has an unknown (zero) entry, such as a copy:// source
// with a skipped track index, gives the layout no phantom track.
func TestPipelineLayoutDropsUnknownKinds(t *testing.T) {
	ft, ev := newFakeTime(), newEvents()
	steps := av{video: 1, audio: 2, gop: 25, span: 2 * media.ClockRate, arrival: realTime}.steps()
	r := newFakeReader([]media.Kind{0, media.KindVideo, media.KindAudio}, ingest.Spec{}, steps, true)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opt := fakeOptions(ft, one(r), ev)
	opt.EndOnEOF = true
	p := New(h, opt)
	require.NoError(t, p.Run(ev.failCtx(t)))

	ds := drain(sub)
	require.Len(t, ds, len(steps), "every frame delivered")
	for _, d := range ds {
		switch d.Frame.Data[0][0] {
		case 1:
			assert.Equal(t, 0, d.Frame.Track, "video → layout 0")
		case 2:
			assert.Equal(t, 1, d.Frame.Track, "audio → layout 1")
		}
	}
	assert.Zero(t, p.Stats().Unmapped)
}
