// Package pipeline runs a stream's inputs into its hub: each input is an
// ingest reader whose frames go through the stream's Clock, onto a fixed
// track layout, and into the hub.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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
	maxProbeBytes        = 32 << 20               // a probe drops its held frames over this, until its next key
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
	// 10 s of reading without failure. FailbackHold is how long an input above
	// the active one must read without a stall before Run fails back to it
	// (10 s).
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
	EventActive EventKind = iota + 1 // the input delivered its first frame and feeds the hub
	// EventFailed reports an input's failure (Err). The input was closed and
	// is retried after its backoff, except a passive input that timed out,
	// which keeps listening.
	EventFailed
	EventDiscontinuity // the active input reported a timeline break
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
	inputs      []*input
	act         *attempt   // the active input's attempt, nil when none
	search      bool       // the active input failed: other pull inputs are tried one at a time
	items       chan item  // from every attempt's goroutine
	layoutAt    time.Time  // when input 0's claim on the layout ends
	waiting     []*attempt // opened attempts waiting for the layout, in opening order
	timer       <-chan time.Time
	timerAt     time.Time // the deadline timer is armed for; zero when none
	rearm       bool      // arm a new timer even for an unchanged deadline
	reached     time.Time // the latest deadline whose timer fired
	last        int       // input active last, -1 before the first
	stall       time.Duration
	activeSince time.Time      // when the active input delivered its first frame
	lastFrame   time.Time      // when the active input last delivered a mapped frame
	wg          sync.WaitGroup // input goroutines

	mu        sync.Mutex // guards st, cur and probePeak
	st        Stats      // Input unset
	cur       ingest.Reader
	probePeak int // the most frames a probe has held, for tests
}

// input is the retry state of one entry of Options.Inputs.
type input struct {
	passive bool
	tried   bool          // failed or tried since the active input failed (search)
	a       *attempt      // the running attempt, nil when none
	backoff time.Duration // the last retry wait, 0 before the first
	retryAt time.Time     // the input is not reopened before this
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
// EndOnEOF, the active input ends (nil). Run returns after every goroutine it
// started has ended. It must be called at most once.
//
// Inputs are tried by priority. With no active input (at start-up, or once
// every input has failed), every input is open at once, a pull input after
// its backoff and a passive one listening, and the first to deliver a frame
// becomes active. When the active input fails, the other pull inputs that are
// not waiting out a backoff are tried one at a time, highest first, each
// once, until one delivers; when none is left to try, every pull input
// reopens after its backoff, as with no active input. The attempts of the
// pull inputs below the active one are closed at every activation. A failed
// input is closed, reported (EventFailed) and retried after its backoff,
// except a passive input failing by a timeout, which keeps listening: it
// becomes active again at its next frame when no input is active. Passive
// inputs listen whenever they are not waiting out a backoff, without a start
// or stall limit, while another input is active.
//
// While an input is active, every input above it is probed for fail-back: a
// pull input reopens after its backoff, a passive one keeps listening, and
// Run takes each probe's frames as they come. A probe's hold runs from its
// first frame and restarts after a gap over its stall limit or a timeline
// break; a pull probe also fails by its start and stall limits, and its next
// attempt starts a new hold. Once a probe has read for FailbackHold, it
// becomes active at its next video key (any frame when it maps no video),
// unless a higher-priority probe is eligible too: the active input's held
// frames are flushed, it is closed (a passive one keeps listening), and the
// probe's frames go to the hub from that key on. While it waits, a probe
// keeps only its frames from its latest key. The passive inputs below the
// active one drop their frames.
func (p *Pipeline) Run(ctx context.Context) error {
	if len(p.opt.Inputs) == 0 {
		return errors.New("pipeline: run: no input")
	}
	p.inputs = make([]*input, len(p.opt.Inputs))
	for i, u := range p.opt.Inputs {
		// Parse gives Passive, needed before open; the effective Spec comes
		// from the open reader.
		spec, err := p.opt.Parse(u)
		if err != nil {
			return fmt.Errorf("pipeline: parse: %w", err)
		}
		p.inputs[i] = &input{passive: spec.Passive}
	}
	p.items = make(chan item)
	if len(p.layout) == 0 {
		p.layoutAt = p.now().Add(p.opt.StartTimeout)
	}
	defer p.wg.Wait()
	defer p.stopAll()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.schedule(ctx)
		p.arm()
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.timer:
			p.reached = latest(p.reached, p.timerAt)
			p.timer, p.timerAt = nil, time.Time{}
			err = p.expire(ctx)
		case it := <-p.items:
			err = p.handle(ctx, it)
		}
		if errors.Is(err, errEnded) {
			p.end(ctx, p.act)
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// now is the time of the deadlines: the clock, or the last deadline whose
// timer fired when that is later, so that a fired timer is always due.
func (p *Pipeline) now() time.Time { return latest(p.opt.Now(), p.reached) }

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// schedule starts the attempts that are due: every passive input not waiting
// out a backoff; the pull inputs above the active one, or with no active
// input, the pull inputs as Run describes.
func (p *Pipeline) schedule(ctx context.Context) {
	now := p.now()
	due := func(in *input) bool { return !in.passive && in.a == nil && !now.Before(in.retryAt) }
	for i, in := range p.inputs {
		if in.passive && in.a == nil && !now.Before(in.retryAt) {
			p.start(ctx, i)
		}
	}
	if p.act != nil {
		// Probes: the pull inputs above the active one.
		for i, in := range p.inputs[:p.act.in] {
			if due(in) {
				p.start(ctx, i)
			}
		}
		return
	}
	if p.search {
		for _, in := range p.inputs {
			if !in.passive && in.a != nil {
				return // one pull input at a time
			}
		}
		if i := slices.IndexFunc(p.inputs, func(in *input) bool { return due(in) && !in.tried }); i >= 0 {
			p.inputs[i].tried = true
			p.start(ctx, i)
			return
		}
		// Every pull input failed since the active one, or waits out an
		// earlier backoff: each reopens after its backoff.
		p.search = false
	}
	for i, in := range p.inputs {
		if due(in) {
			p.start(ctx, i)
		}
	}
}

// arm sets the timer to the earliest deadline: an attempt's start or stall
// limit, a retry that schedule would start, or the end of the layout wait
// while attempts wait for it. Every deadline is absolute: a timer armed late
// fires late by the lag, and the deadline it was armed for counts as due
// when it fires (reached), whatever the clock reads.
func (p *Pipeline) arm() {
	now := p.now()
	var next time.Time
	consider := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for i, in := range p.inputs {
		switch {
		case in.a != nil:
			consider(in.a.deadline)
		case (in.passive || p.reopens(i)) && in.retryAt.After(now):
			consider(in.retryAt)
		}
	}
	if len(p.waiting) > 0 {
		consider(p.layoutAt)
	}
	if next.Equal(p.timerAt) && !p.rearm {
		return
	}
	p.rearm = false
	p.timerAt = next
	if next.IsZero() {
		p.timer = nil
		return
	}
	p.timer = p.opt.After(next.Sub(now))
}

// reopens reports whether schedule reopens pull input i when its backoff
// ends: as a probe above the active input, or with no active input and no
// search running. arm runs after schedule, so a search has a pull attempt
// running until it ends.
func (p *Pipeline) reopens(i int) bool {
	if p.act != nil {
		return i < p.act.in
	}
	return !p.search
}

// expire handles the deadlines that are due: the end of the layout wait,
// then the start and stall limits.
func (p *Pipeline) expire(ctx context.Context) error {
	now := p.now()
	if len(p.waiting) > 0 && !now.Before(p.layoutAt) {
		if len(p.layout) == 0 {
			p.layout = p.waiting[0].r.Kinds() // the first input that opened
		}
		p.resumeWaiting()
	}
	for _, in := range p.inputs {
		a := in.a
		if a == nil || a.deadline.IsZero() || now.Before(a.deadline) {
			continue
		}
		var err error
		switch {
		case a == p.act:
			err = fmt.Errorf("pipeline: read: %w (%v)", errStalled, p.stall)
		case !a.since.IsZero(): // a probe that delivered
			err = fmt.Errorf("pipeline: read: %w (%v)", errStalled, a.stall)
		default:
			err = fmt.Errorf("pipeline: start: %w (%v)", errNoStart, p.opt.StartTimeout)
		}
		if err := p.fail(ctx, a, err); err != nil {
			return err
		}
	}
	return nil
}

// itemKind says what an input goroutine delivered.
type itemKind uint8

const (
	itemOpened  itemKind = iota + 1 // r is the open reader
	itemOpenErr                     // err is the open's error
	itemRead                        // f, err is one Read result
)

type item struct {
	a    *attempt
	kind itemKind
	r    ingest.Reader
	f    ingest.Frame
	err  error
}

// attempt is one open of one input and its goroutine, which opens the input,
// then reads one item per request so that reads stay in step with Run. Run
// requests the next read of an input that is not active as soon as it takes
// a frame, so probes and background listeners read freely.
type attempt struct {
	in     int
	req    chan struct{} // capacity 1: Run asks for the next Read
	done   chan struct{} // closed at stop
	exited chan struct{} // closed when the goroutine returns
	cancel context.CancelFunc

	// Run goroutine only.
	r        ingest.Reader // set once the open reader is received
	tracks   []int         // reader track → layout track, -1 unmapped; nil to recompute
	deadline time.Time     // the start or stall limit; zero for none

	// As a probe.
	stall    time.Duration // the stall limit, set at the first frame
	since    time.Time     // when the hold started; zero before the first frame
	last     time.Time     // the last mapped frame
	held     []heldFrame   // the frames from the latest key
	heldSize int           // bytes of held
}

// heldFrame is a frame a probe holds, with its layout track.
type heldFrame struct {
	f ingest.Frame
	t int
}

// resetProbe drops a's held frames and restarts its hold at its next frame.
func (a *attempt) resetProbe() {
	a.since = time.Time{}
	a.held, a.heldSize = nil, 0
}

// start opens input in in a new goroutine; it delivers on p.items.
func (p *Pipeline) start(ctx context.Context, in int) {
	octx, cancel := context.WithCancel(ctx)
	a := &attempt{
		in:     in,
		req:    make(chan struct{}, 1),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
		cancel: cancel,
	}
	if !p.inputs[in].passive || p.act == nil {
		// A passive input listening in the background is not judged.
		a.deadline = p.now().Add(p.opt.StartTimeout)
	}
	p.inputs[in].a = a
	p.rearm = true
	url := p.opt.Inputs[in]
	items := p.items
	send := func(it item) bool {
		it.a = a
		select {
		case items <- it:
			return true
		case <-a.done:
			return false
		}
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(a.exited)
		r, err := p.opt.Open(octx, url)
		if err == nil && r == nil {
			err = errors.New("no reader")
		}
		if err != nil {
			send(item{kind: itemOpenErr, err: fmt.Errorf("pipeline: open: %w", err)})
			return
		}
		if !send(item{kind: itemOpened, r: r}) {
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
			if !send(item{kind: itemRead, f: f, err: err}) {
				return
			}
			if err != nil && !errors.Is(err, ingest.ErrDiscontinuity) {
				return
			}
		}
	}()
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

// stopAttempt stops a and forgets it.
func (p *Pipeline) stopAttempt(a *attempt) {
	a.stop()
	p.inputs[a.in].a = nil
	p.waiting = slices.DeleteFunc(p.waiting, func(w *attempt) bool { return w == a })
}

func (p *Pipeline) stopAll() {
	for _, in := range p.inputs {
		if in.a != nil {
			p.stopAttempt(in.a)
		}
	}
}

// handle processes one item of an attempt. Only frames that map onto the
// layout are progress: they activate an input and re-arm the stall limit,
// as does a discontinuity's flush when it wrote frames (it may have paced
// them); an input of unmapped tracks or bare discontinuities stalls. The
// stall limit runs from the end of each frame's processing, so pacing sleeps
// never count.
func (p *Pipeline) handle(ctx context.Context, it item) error {
	a := it.a
	switch {
	case it.kind == itemOpenErr:
		return p.fail(ctx, a, it.err)
	case it.kind == itemOpened:
		a.r = it.r
		p.opened(a)
		return nil
	case it.err == nil:
		return p.frame(ctx, a, it.f)
	case errors.Is(it.err, ingest.ErrDiscontinuity):
		a.tracks = nil // a remapped slot may change kind
		a.resetProbe() // a probe's hold and held frames do not span a break
		if a == p.act {
			wrote, err := p.breakClock(ctx, false)
			if err != nil {
				return err
			}
			if wrote > 0 {
				p.restartStall(a)
			}
			p.emit(Event{Kind: EventDiscontinuity, Input: a.in})
		}
		a.next()
		return nil
	case errors.Is(it.err, io.EOF) && a == p.act && p.opt.EndOnEOF:
		return errEnded
	default:
		return p.fail(ctx, a, fmt.Errorf("pipeline: read: %w", it.err))
	}
}

// opened starts reading an open attempt, once the layout is known: input
// 0's kinds while it may still open within StartTimeout of Run's start,
// else the first input's that opens.
func (p *Pipeline) opened(a *attempt) {
	if len(p.layout) == 0 {
		if a.in != 0 && p.now().Before(p.layoutAt) {
			p.waiting = append(p.waiting, a)
			return
		}
		p.layout = a.r.Kinds()
	}
	a.next()
	if len(p.layout) > 0 {
		p.resumeWaiting()
	}
}

// resumeWaiting starts reading the attempts that waited for the layout. Their
// start limit runs again from now: they could not deliver while waiting.
func (p *Pipeline) resumeWaiting() {
	for _, w := range p.waiting {
		if !w.deadline.IsZero() {
			w.deadline = p.now().Add(p.opt.StartTimeout)
			p.rearm = true
		}
		w.next()
	}
	p.waiting = nil
}

// frame processes a frame of a: the active input's goes to the hub; with no
// active input, a's first mapped frame makes it active; a probe takes its
// mapped frames; a passive input listening below the active one drops it.
func (p *Pipeline) frame(ctx context.Context, a *attempt, f ingest.Frame) error {
	if len(p.layout) == 0 {
		// The input opened without tracks; it may not be input 0 once the
		// layout wait is over.
		p.layout = a.r.Kinds()
		if len(p.layout) > 0 {
			p.resumeWaiting()
		}
	}
	if p.act != nil && a != p.act {
		if a.in < p.act.in {
			if t, ok := a.track(f.Track, p.layout); ok {
				return p.probe(ctx, a, f, t)
			}
		} else {
			a.deadline = time.Time{} // a passive input listening in the background is not judged
		}
		a.next()
		return nil
	}
	t, ok := a.track(f.Track, p.layout)
	if !ok {
		p.mu.Lock()
		p.st.Unmapped++
		p.mu.Unlock()
		a.next()
		return nil
	}
	if a != p.act {
		p.activate(a)
	}
	p.lastFrame = p.now()
	if err := p.push(ctx, f, t); err != nil {
		return err
	}
	p.restartStall(a)
	a.next()
	return nil
}

// restartStall restarts the active input's stall limit from now. The timer
// is armed before the next Read is requested, so it is in place by the time
// the reader blocks.
func (p *Pipeline) restartStall(a *attempt) {
	a.deadline = p.now().Add(p.stall)
	p.rearm = true
	p.arm()
}

// activate makes a the active input at its first frame. The attempts of the
// pull inputs below it are closed; those above it go on as probes; passive
// inputs keep listening. Every activation after the first breaks the Clock
// once for a new input, the same input reopened or resumed included; a
// different input than the last one counts as a switch.
func (p *Pipeline) activate(a *attempt) {
	p.act = a
	p.search = false
	a.resetProbe()
	for i, in := range p.inputs {
		o := in.a
		switch {
		case o == nil || o == a:
		case in.passive:
			o.deadline = time.Time{} // listening, not judged, while another input is active
			if i > a.in {
				o.resetProbe()
			}
		case i > a.in:
			p.stopAttempt(o)
		}
	}
	spec := a.r.Spec()
	p.stall = max(p.opt.StallTimeout, spec.StallTimeout)
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
	p.activeSince = p.now()
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

// probe takes frame f of a, an input above the active one, mapped onto
// layout track t. Its hold runs from its first frame and restarts after a gap
// over its stall limit; a pull probe's stall limit is armed. It holds the
// frames from its latest key, video when it maps video, else any frame; at a
// key, once its hold is over and no higher-priority probe is eligible, it
// fails back.
func (p *Pipeline) probe(ctx context.Context, a *attempt, f ingest.Frame, t int) error {
	now := p.now()
	if a.stall == 0 {
		a.stall = max(p.opt.StallTimeout, a.r.Spec().StallTimeout)
	}
	if a.since.IsZero() || now.Sub(a.last) > a.stall {
		a.resetProbe()
		a.since = now
	}
	a.last = now
	if !p.inputs[a.in].passive {
		a.deadline = now.Add(a.stall) // arm re-arms when it is the earliest
	}
	key := !a.mapsVideo(p.layout) || (p.layout[t] == media.KindVideo && f.Key)
	if key {
		a.held, a.heldSize = a.held[:0], 0
	}
	if size := frameSize(f.Frame); len(a.held) > 0 || key {
		if a.heldSize+size > maxProbeBytes {
			a.held, a.heldSize = nil, 0 // too much to hold: wait for the next key
		} else {
			a.held = append(a.held, heldFrame{f: f, t: t})
			a.heldSize += size
			if len(a.held) > p.probePeak {
				p.mu.Lock()
				p.probePeak = len(a.held)
				p.mu.Unlock()
			}
		}
	}
	if key && len(a.held) > 0 && p.eligible(a, now) &&
		!slices.ContainsFunc(p.inputs[:a.in], func(in *input) bool { return in.a != nil && p.eligible(in.a, now) }) {
		return p.failback(ctx, a)
	}
	a.next()
	return nil
}

// eligible reports whether probe a has read without a stall for
// FailbackHold up to now.
func (p *Pipeline) eligible(a *attempt, now time.Time) bool {
	return !a.since.IsZero() && now.Sub(a.last) <= a.stall && now.Sub(a.since) >= p.opt.FailbackHold
}

// failback makes probe a, at its key, the active input: the active input is
// made inactive, which flushes the Clock, and closed by the activation (a
// passive one keeps listening); a's held frames, from that key, go to the
// hub.
func (p *Pipeline) failback(ctx context.Context, a *attempt) error {
	old := p.act
	if p.deactivate(ctx, old, false) >= healthyReset {
		p.inputs[old.in].backoff = 0
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	held := a.held
	p.activate(a)
	for _, h := range held {
		p.lastFrame = p.now()
		if err := p.push(ctx, h.f, h.t); err != nil {
			return err
		}
	}
	p.restartStall(a)
	a.next()
	return nil
}

func frameSize(f media.Frame) int {
	n := 0
	for _, b := range f.Data {
		n += len(b)
	}
	return n
}

// fail handles a's failure by cause. The active input's held frames are
// flushed at once, so pacing never delays failover, and the other pull
// inputs are then tried. A passive input that timed out keeps listening;
// any other failed input is closed and waits out its backoff, which 10 s of
// healthy reading (from its first frame to its last) resets. EventFailed is
// not reported once ctx is done.
func (p *Pipeline) fail(ctx context.Context, a *attempt, cause error) error {
	in := p.inputs[a.in]
	if a == p.act {
		// The search tries each other pull input once, highest first.
		p.search = true
		for _, o := range p.inputs {
			o.tried = false
		}
		in.tried = true
		if p.deactivate(ctx, a, false) >= healthyReset {
			in.backoff = 0
		}
	}
	if !a.since.IsZero() && a.last.Sub(a.since) >= healthyReset {
		in.backoff = 0 // a probe that read long enough (only probes have since set)
	}
	if in.passive && (errors.Is(cause, errNoStart) || errors.Is(cause, errStalled)) {
		a.deadline = time.Time{}
	} else {
		p.stopAttempt(a)
		in.backoff = min(max(2*in.backoff, firstRetry), p.opt.RetryInterval)
		in.retryAt = p.now().Add(in.backoff)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.emit(Event{Kind: EventFailed, Input: a.in, Err: cause})
	return nil
}

// end stops the active attempt a, which ended, and flushes its held frames
// paced.
func (p *Pipeline) end(ctx context.Context, a *attempt) {
	p.stopAttempt(a)
	p.deactivate(ctx, a, true)
}

// deactivate makes a, the active input, inactive. It flushes the Clock
// (unless ctx is done) and returns how long a was healthy, from its first
// frame to its last. The flush is paced only when a ended (ended).
func (p *Pipeline) deactivate(ctx context.Context, a *attempt, ended bool) time.Duration {
	p.act = nil
	a.deadline = time.Time{}
	p.mu.Lock()
	p.st.Active = -1
	p.cur = nil
	p.mu.Unlock()
	if !ended {
		p.w.pace = false // the next activation sets it again
	}
	if ctx.Err() == nil {
		// Only a cancelled context fails the flush, and Run returns then.
		_, _ = p.flush(ctx)
	}
	return p.lastFrame.Sub(p.activeSince)
}

// push runs f, mapped onto layout track t, through the Clock into the hub.
func (p *Pipeline) push(ctx context.Context, f ingest.Frame, t int) error {
	mf := f.Frame
	mf.Track = t
	out := p.w.c.PushAt(mf, f.Arrival)
	p.syncClock()
	return p.w.write(ctx, out)
}

// breakClock releases what the Clock holds, so the frames from before the
// break are written rather than dropped as stale, then breaks it. It returns
// how many frames it wrote.
func (p *Pipeline) breakClock(ctx context.Context, newInput bool) (int, error) {
	n, err := p.flush(ctx)
	if err != nil {
		return n, err
	}
	p.w.c.Break(newInput)
	return n, nil
}

// flush writes the frames the Clock holds and returns how many it wrote.
func (p *Pipeline) flush(ctx context.Context) (int, error) {
	out := p.w.c.Flush()
	p.syncClock()
	return len(out), p.w.write(ctx, out)
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

// mapsVideo reports whether a reader track of a, among those mapped so far,
// maps onto a video track of the layout.
func (a *attempt) mapsVideo(layout []media.Kind) bool {
	return slices.ContainsFunc(a.tracks, func(t int) bool { return t >= 0 && layout[t] == media.KindVideo })
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
