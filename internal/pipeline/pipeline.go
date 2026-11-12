// Package pipeline runs a stream's inputs into its hub: each input is an
// ingest reader whose frames go through the stream's Clock, onto a fixed
// track layout, and into the hub.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	defaultStartTimeout  = 10 * time.Second
	defaultStallTimeout  = 4 * time.Second
	defaultRetryInterval = 10 * time.Second
	defaultFailbackHold  = 10 * time.Second
	firstRetry           = 500 * time.Millisecond // the first retry wait, doubled up to RetryInterval
	healthyReset         = 10 * time.Second       // reading this long without failure resets the backoff
	liveMaxDrift         = 2 * time.Second        // Clock MaxDrift for live inputs
)

var (
	errNoStart = errors.New("no frame within the start timeout")
	errStalled = errors.New("no frame within the stall timeout")
	errEnded   = errors.New("pipeline: input ended")
)

// Options configures a Pipeline. Zero durations select the defaults.
type Options struct {
	Inputs   []string // URLs by priority, first highest; at least one
	Env      ingest.Env
	Open     func(ctx context.Context, url string) (ingest.Reader, error) // nil means ingest.Open with Env
	Parse    func(url string) (ingest.Spec, error)                        // nil means ingest.Parse
	Layout   []media.Kind                                                 // nil: input 0's kinds
	EndOnEOF bool                                                         // io.EOF from the active input ends Run with nil

	// StartTimeout bounds an input's open until its first frame (10 s).
	// StallTimeout bounds Run's wait for the next frame, excluding its pacing
	// sleeps (4 s); a reader's Spec().StallTimeout raises it. A failed input
	// is retried after 0.5 s, doubling up to RetryInterval (10 s), reset by
	// 10 s of reading without failure. FailbackHold is for fail-back (10 s).
	StartTimeout, StallTimeout, RetryInterval, FailbackHold time.Duration

	Now     func() time.Time                                 // nil means time.Now; also the Clock's and, when Env.Now is nil, the readers'
	Sleep   func(ctx context.Context, d time.Duration) error // pacing waits; nil means a timer that honours ctx
	After   func(d time.Duration) <-chan time.Time           // every timeout and retry wait; nil means time.After
	OnEvent func(Event)                                      // optional, called from the Run goroutine
}

// EventKind identifies an Event.
type EventKind uint8

// Event kinds.
const (
	EventActive        EventKind = iota + 1 // the input delivered its first frame and feeds the hub
	EventFailed                             // the input failed (Err) and was closed
	EventDiscontinuity                      // the active input reported a timeline break
)

// Event reports a change of an input's state.
type Event struct {
	Kind  EventKind
	Input int // index in Options.Inputs
	Err   error
}

// Stats is a snapshot of a pipeline's counters.
type Stats struct {
	Active   int // index of the active input, -1 when none
	Switches uint64
	Unmapped uint64 // frames of reader tracks outside the layout
	Clock    clock.Stats
	Input    map[string]uint64 // the active reader's Stats()
}

// Pipeline runs a stream's inputs into its hub.
type Pipeline struct {
	opt Options
	w   writer

	// Run goroutine only.
	layout      []media.Kind
	last        int           // input active last, -1 before the first
	stall       time.Duration // the active input's stall limit
	activeSince time.Time
	wg          sync.WaitGroup // input goroutines

	mu  sync.Mutex // guards st and cur
	st  Stats      // Input unset
	cur ingest.Reader
}

// New returns a pipeline that writes to h.
func New(h *hub.Hub, opt Options) *Pipeline {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Env.Now == nil {
		opt.Env.Now = opt.Now
	}
	if opt.Sleep == nil {
		opt.Sleep = sleep
	}
	if opt.After == nil {
		opt.After = time.After
	}
	if opt.Parse == nil {
		opt.Parse = ingest.Parse
	}
	if opt.Open == nil {
		env := opt.Env
		opt.Open = func(ctx context.Context, url string) (ingest.Reader, error) { return ingest.Open(ctx, url, env) }
	}
	opt.StartTimeout = orDefault(opt.StartTimeout, defaultStartTimeout)
	opt.StallTimeout = orDefault(opt.StallTimeout, defaultStallTimeout)
	opt.RetryInterval = orDefault(opt.RetryInterval, defaultRetryInterval)
	opt.FailbackHold = orDefault(opt.FailbackHold, defaultFailbackHold)
	return &Pipeline{
		opt:    opt,
		w:      writer{h: h, now: opt.Now, sleep: opt.Sleep, anchors: map[uint32]media.Anchor{}},
		layout: append([]media.Kind(nil), opt.Layout...),
		last:   -1,
		st:     Stats{Active: -1},
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Stats returns a snapshot of the counters. It is safe from any goroutine.
func (p *Pipeline) Stats() Stats {
	p.mu.Lock()
	st, cur := p.st, p.cur
	p.mu.Unlock()
	if cur != nil {
		st.Input = cur.Stats()
	}
	return st
}

// Run reads the inputs into the hub until ctx is done (ctx.Err()) or, with
// EndOnEOF, the active input ends (nil). A failed input is closed, reported
// (EventFailed) and retried after its backoff. Run returns after every
// goroutine it started has ended. It must be called at most once.
func (p *Pipeline) Run(ctx context.Context) error {
	if len(p.opt.Inputs) == 0 {
		return errors.New("pipeline: run: no input")
	}
	for _, u := range p.opt.Inputs {
		// Parse gives Passive, needed before open by failover; the
		// effective Spec comes from the open reader.
		if _, err := p.opt.Parse(u); err != nil {
			return fmt.Errorf("pipeline: parse: %w", err)
		}
	}
	defer p.wg.Wait()
	var backoff time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a := p.start(ctx, 0)
		cause := p.serve(ctx, a)
		healthy := p.end(ctx, a)
		if errors.Is(cause, errEnded) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.emit(Event{Kind: EventFailed, Input: a.in, Err: cause})
		if healthy >= healthyReset {
			backoff = 0
		}
		backoff = min(max(2*backoff, firstRetry), p.opt.RetryInterval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.opt.After(backoff):
		}
	}
}

// itemKind says what an input goroutine delivered.
type itemKind uint8

const (
	itemOpened  itemKind = iota + 1 // r is the open reader
	itemOpenErr                     // err is the open's error
	itemRead                        // f, err is one Read result
)

type item struct {
	kind itemKind
	r    ingest.Reader
	f    ingest.Frame
	err  error
}

// attempt is one open of one input and its goroutine, which opens the input,
// then reads one item per request so that reads stay in step with Run.
type attempt struct {
	in     int
	ch     chan item     // unbuffered: an item is either taken by Run or dropped at stop
	req    chan struct{} // capacity 1: Run asks for the next Read
	done   chan struct{} // closed at stop
	exited chan struct{} // closed when the goroutine returns
	cancel context.CancelFunc

	// Run goroutine only.
	r      ingest.Reader // set once the open reader is received
	active bool          // delivered its first frame
	tracks []int         // reader track → layout track, -1 unmapped; nil to recompute
}

// start opens input in in a new goroutine.
func (p *Pipeline) start(ctx context.Context, in int) *attempt {
	octx, cancel := context.WithCancel(ctx)
	a := &attempt{
		in:     in,
		ch:     make(chan item),
		req:    make(chan struct{}, 1),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
		cancel: cancel,
	}
	url := p.opt.Inputs[in]
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(a.exited)
		r, err := p.opt.Open(octx, url)
		if err == nil && r == nil {
			err = errors.New("no reader")
		}
		if err != nil {
			a.send(item{kind: itemOpenErr, err: fmt.Errorf("pipeline: open: %w", err)})
			return
		}
		if !a.send(item{kind: itemOpened, r: r}) {
			_ = r.Close()
			return
		}
		for {
			select {
			case <-a.done:
				return
			case <-a.req:
			}
			f, err := r.Read()
			if !a.send(item{kind: itemRead, f: f, err: err}) {
				return
			}
			if err != nil && !errors.Is(err, ingest.ErrDiscontinuity) {
				return
			}
		}
	}()
	return a
}

// send hands it to Run, or reports false once the attempt is stopped.
func (a *attempt) send(it item) bool {
	select {
	case a.ch <- it:
		return true
	case <-a.done:
		return false
	}
}

// next asks the goroutine for the next Read.
func (a *attempt) next() {
	select {
	case a.req <- struct{}{}:
	default:
	}
}

// stop aborts a pending open, closes the reader, which unblocks a pending
// Read, and waits for the goroutine to exit, so the input is closed when stop
// returns. A reader the goroutine opened but Run never received is closed by
// the goroutine.
func (a *attempt) stop() {
	a.cancel()
	close(a.done)
	if a.r != nil {
		_ = a.r.Close()
	}
	<-a.exited
}

// serve feeds the hub from a until it fails, ends (errEnded) or ctx is done.
// The timeout runs only while Run waits on the input: StartTimeout from the
// open to the first frame, then the stall limit from the end of each frame's
// processing, so pacing sleeps never count.
func (p *Pipeline) serve(ctx context.Context, a *attempt) error {
	wait := p.opt.After(p.opt.StartTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var it item
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
			if a.active {
				return fmt.Errorf("pipeline: read: %w (%v)", errStalled, p.stall)
			}
			return fmt.Errorf("pipeline: start: %w (%v)", errNoStart, p.opt.StartTimeout)
		case it = <-a.ch:
		}
		switch {
		case it.kind == itemOpenErr:
			return it.err
		case it.kind == itemOpened:
			a.r = it.r
			if len(p.layout) == 0 && a.in == 0 {
				p.layout = a.r.Kinds()
			}
		case it.err == nil:
			if !a.active {
				p.activate(a)
			}
			if err := p.push(ctx, a, it.f); err != nil {
				return err
			}
			wait = p.opt.After(p.stall)
		case errors.Is(it.err, ingest.ErrDiscontinuity):
			if a.active {
				if err := p.breakClock(ctx, false); err != nil {
					return err
				}
				a.tracks = nil // a remapped slot may change kind
				p.emit(Event{Kind: EventDiscontinuity, Input: a.in})
			}
		case errors.Is(it.err, io.EOF) && a.active && p.opt.EndOnEOF:
			return errEnded
		default:
			return fmt.Errorf("pipeline: read: %w", it.err)
		}
		a.next()
	}
}

// activate makes a the active input at its first frame. Every activation
// after the first breaks the Clock for a new input, the same input reopened
// included; a different input than the last one counts as a switch.
func (p *Pipeline) activate(a *attempt) {
	spec := a.r.Spec()
	p.stall = max(p.opt.StallTimeout, spec.StallTimeout)
	if len(p.layout) == 0 {
		p.layout = a.r.Kinds()
	}
	if p.w.c == nil {
		p.w.c = clock.New(clock.Options{Kinds: p.layout, Now: p.opt.Now})
	} else {
		// The previous input's end flushed the Clock, so this Break drops
		// nothing.
		p.w.c.Break(true)
	}
	if spec.Live {
		p.w.c.SetDrift(liveMaxDrift, spec.Pace)
	} else {
		p.w.c.SetDrift(0, false)
	}
	p.w.pace = spec.Pace
	a.active = true
	p.activeSince = p.opt.Now()
	p.mu.Lock()
	if p.last >= 0 && p.last != a.in {
		p.st.Switches++
	}
	p.st.Active = a.in
	p.cur = a.r
	p.mu.Unlock()
	p.last = a.in
	p.emit(Event{Kind: EventActive, Input: a.in})
}

// end stops a after serve returned. When a was active, it flushes the Clock
// (unless ctx is done) and returns how long a was active.
func (p *Pipeline) end(ctx context.Context, a *attempt) time.Duration {
	a.stop()
	if !a.active {
		return 0
	}
	p.mu.Lock()
	p.st.Active = -1
	p.cur = nil
	p.mu.Unlock()
	if ctx.Err() == nil {
		// Only a cancelled context fails the flush, and Run returns then.
		_ = p.flush(ctx)
	}
	return p.opt.Now().Sub(p.activeSince)
}

// push maps f onto the layout and runs it through the Clock into the hub.
func (p *Pipeline) push(ctx context.Context, a *attempt, f ingest.Frame) error {
	t, ok := a.track(f.Track, p.layout)
	if !ok {
		p.mu.Lock()
		p.st.Unmapped++
		p.mu.Unlock()
		return nil
	}
	mf := f.Frame
	mf.Track = t
	out := p.w.c.PushAt(mf, f.Arrival)
	p.syncClock()
	return p.w.write(ctx, out)
}

// breakClock releases what the Clock holds, so the frames from before the
// break are written rather than dropped as stale, then breaks it.
func (p *Pipeline) breakClock(ctx context.Context, newInput bool) error {
	if err := p.flush(ctx); err != nil {
		return err
	}
	p.w.c.Break(newInput)
	return nil
}

func (p *Pipeline) flush(ctx context.Context) error {
	out := p.w.c.Flush()
	p.syncClock()
	return p.w.write(ctx, out)
}

func (p *Pipeline) syncClock() {
	st := p.w.c.Stats()
	p.mu.Lock()
	p.st.Clock = st
	p.mu.Unlock()
}

func (p *Pipeline) emit(ev Event) {
	if p.opt.OnEvent != nil {
		p.opt.OnEvent(ev)
	}
}

// track maps reader track t onto the layout by (kind, ordinal): the k-th
// reader track of a kind is the layout's k-th track of that kind.
func (a *attempt) track(t int, layout []media.Kind) (int, bool) {
	if t < 0 {
		return 0, false
	}
	if t >= len(a.tracks) {
		a.tracks = mapTracks(a.r.Kinds(), layout)
	}
	if t >= len(a.tracks) || a.tracks[t] < 0 {
		return 0, false
	}
	return a.tracks[t], true
}

func mapTracks(kinds, layout []media.Kind) []int {
	out := make([]int, len(kinds))
	seen := map[media.Kind]int{}
	for i, k := range kinds {
		out[i] = nth(layout, k, seen[k])
		seen[k]++
	}
	return out
}

// nth returns the index of the n-th (from 0) entry of kind k in layout, or -1.
func nth(layout []media.Kind, k media.Kind, n int) int {
	for i, l := range layout {
		if l != k {
			continue
		}
		if n == 0 {
			return i
		}
		n--
	}
	return -1
}

// writer feeds clock output to the hub, recording each epoch's anchor before
// its first frame and, when pace is set, pacing frames against that anchor.
// The anchor's Wall is the Clock's effective wall, the same wall the packager
// advertises, so pacing waits out each epoch's presentation-end excess and
// the walls never run ahead of the frames (see clock.Epoch).
type writer struct {
	h        *hub.Hub
	c        *clock.Clock
	pace     bool // release frames at real time
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
	anchored uint32 // largest epoch whose anchor was set
	anchors  map[uint32]media.Anchor
}

func (w *writer) write(ctx context.Context, frames []media.Frame) error {
	for _, f := range frames {
		if f.Epoch > w.anchored {
			for _, e := range w.c.Epochs() {
				if e.ID > w.anchored {
					a := media.Anchor{Epoch: e.ID, Start: e.Start, Wall: e.Wall}
					w.h.SetAnchor(a)
					w.anchors[e.ID] = a
					w.anchored = e.ID
				}
			}
		}
		for id := range w.anchors {
			if id < f.Epoch {
				delete(w.anchors, id) // pacing needs only the current epoch
			}
		}
		if w.pace {
			if a, ok := w.anchors[f.Epoch]; ok {
				due := a.Wall.Add(media.ToDuration(f.DTS - a.Start))
				if d := due.Sub(w.now()); d > 0 {
					if err := w.sleep(ctx, d); err != nil {
						return err
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		w.h.Write(f)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
