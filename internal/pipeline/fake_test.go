package pipeline

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

var fakeT0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// fakeTime is a manual clock. After registers a timer that fires when the
// time is advanced to its deadline, and reports every call on calls; Sleep
// advances the time.
type fakeTime struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
	calls  chan time.Duration
}

type fakeTimer struct {
	at time.Time
	c  chan time.Time
}

func newFakeTime() *fakeTime {
	return &fakeTime{now: fakeT0, calls: make(chan time.Duration, 1<<16)}
}

func (f *fakeTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeTime) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	t := fakeTimer{at: f.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- f.now
	} else {
		f.timers = append(f.timers, t)
	}
	f.mu.Unlock()
	select {
	case f.calls <- d:
	default:
	}
	return t.c
}

// Advance moves the time forward by d and fires the timers that are due.
func (f *fakeTime) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	kept := f.timers[:0]
	for _, t := range f.timers {
		if t.at.After(f.now) {
			kept = append(kept, t)
			continue
		}
		t.c <- f.now
	}
	f.timers = kept
}

func (f *fakeTime) Sleep(ctx context.Context, d time.Duration) error {
	f.Advance(d)
	return ctx.Err()
}

// nextCall returns the duration of the next After call, failing after 5 s of
// real time.
func (f *fakeTime) nextCall() (time.Duration, bool) {
	select {
	case d := <-f.calls:
		return d, true
	case <-time.After(5 * time.Second):
		return 0, false
	}
}

// errFakeClosed is returned by a fake reader's Read after Close.
var errFakeClosed = errors.New("fake: closed")

// step is one Read result of a fake reader.
type step struct {
	f   ingest.Frame
	err error
}

// fakeReader returns its steps in order, then io.EOF when eof is set, else
// blocks until Close.
type fakeReader struct {
	kinds []media.Kind
	spec  ingest.Spec
	steps []step
	eof   bool

	i       int // Read goroutine only
	blocked chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newFakeReader(kinds []media.Kind, spec ingest.Spec, steps []step, eof bool) *fakeReader {
	return &fakeReader{
		kinds: kinds, spec: spec, steps: steps, eof: eof,
		blocked: make(chan struct{}, 1), closed: make(chan struct{}),
	}
}

func (r *fakeReader) Read() (ingest.Frame, error) {
	select {
	case <-r.closed:
		return ingest.Frame{}, errFakeClosed
	default:
	}
	if r.i < len(r.steps) {
		s := r.steps[r.i]
		r.i++
		return s.f, s.err
	}
	if r.eof {
		return ingest.Frame{}, io.EOF
	}
	select {
	case r.blocked <- struct{}{}:
	default:
	}
	<-r.closed
	return ingest.Frame{}, errFakeClosed
}

func (r *fakeReader) Kinds() []media.Kind { return append([]media.Kind(nil), r.kinds...) }
func (r *fakeReader) Spec() ingest.Spec   { return r.spec }
func (r *fakeReader) Stats() map[string]uint64 {
	return map[string]uint64{"fake_steps": uint64(len(r.steps))}
}

func (r *fakeReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (r *fakeReader) isClosed() bool {
	select {
	case <-r.closed:
		return true
	default:
		return false
	}
}

var (
	videoCfg  = &media.TrackConfig{Codec: media.CodecH264, Width: 640, Height: 360}
	audioCfg  = &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 48000, Channels: 2}
	audioCfg2 = &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 44100, Channels: 1}
)

const (
	vDur = media.ClockRate / 25 // 25 fps
	aDur = 1920                 // AAC at 48 kHz
)

// av builds frames of a video track (25 fps, a key every gop frames) and,
// unless audio < 0, an audio track, over span ticks from DTS start, ordered
// by DTS, video first on a tie. arrival gives each frame's arrival.
type av struct {
	video, audio int // reader track indexes; audio < 0 for none
	gop          int
	start, span  int64
	arrival      func(dts int64) time.Time
}

func (a av) steps() []step {
	var out []step
	vi, ai := int64(0), int64(0)
	for {
		vd, ad := a.start+vi*vDur, a.start+ai*aDur
		if vd >= a.start+a.span && (a.audio < 0 || ad >= a.start+a.span) {
			return out
		}
		if a.audio < 0 || (vd < a.start+a.span && vd <= ad) {
			f := media.Frame{Track: a.video, Config: videoCfg, DTS: vd, PTS: vd, Key: vi%int64(a.gop) == 0, Data: [][]byte{{1}}}
			out = append(out, step{f: ingest.Frame{Frame: f, Arrival: a.arrival(vd)}})
			vi++
			continue
		}
		f := media.Frame{Track: a.audio, Config: audioCfg, DTS: ad, PTS: ad, Key: true, Data: [][]byte{{2}}}
		out = append(out, step{f: ingest.Frame{Frame: f, Arrival: a.arrival(ad)}})
		ai++
	}
}

// realTime is an arrival that follows the source clock from fakeT0.
func realTime(dts int64) time.Time { return fakeT0.Add(media.ToDuration(dts)) }

// opener serves the readers from next in turn and records the contexts and
// readers of each Open.
type opener struct {
	mu      sync.Mutex
	next    func(n int, ctx context.Context) (ingest.Reader, error)
	opens   int
	readers []ingest.Reader
}

func (o *opener) open(ctx context.Context, _ string) (ingest.Reader, error) {
	o.mu.Lock()
	n := o.opens
	o.opens++
	o.mu.Unlock()
	r, err := o.next(n, ctx)
	if r != nil {
		o.mu.Lock()
		o.readers = append(o.readers, r)
		o.mu.Unlock()
	}
	return r, err
}

func noParse(string) (ingest.Spec, error) { return ingest.Spec{}, nil }

// events collects pipeline events; it is used from the Run goroutine and read
// after Run returns, or under its lock.
type events struct {
	mu     sync.Mutex
	evs    []Event
	c      chan Event
	cancel context.CancelFunc
}

func newEvents() *events { return &events{c: make(chan Event, 1<<12)} }

func (e *events) on(ev Event) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	if ev.Kind == EventFailed && e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
	select {
	case e.c <- ev:
	default:
	}
}

// failCtx returns a context cancelled by the first EventFailed, so a test
// expecting its input to end fails instead of retrying without end.
func (e *events) failCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
	return ctx
}

func (e *events) kinds() []EventKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]EventKind, 0, len(e.evs))
	for _, ev := range e.evs {
		out = append(out, ev.Kind)
	}
	return out
}

// next returns the next event, failing after 5 s of real time.
func (e *events) next() (Event, bool) {
	select {
	case ev := <-e.c:
		return ev, true
	case <-time.After(5 * time.Second):
		return Event{}, false
	}
}

func epochsOf(ds []hub.Delivery) map[uint32]int {
	out := map[uint32]int{}
	for _, d := range ds {
		out[d.Frame.Epoch]++
	}
	return out
}
