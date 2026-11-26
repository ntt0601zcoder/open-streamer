package stream

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// The restart backoff: it starts at backoffMin, doubles up to backoffMax,
// and starts again after a generation that lived for healthyLife.
const (
	backoffMin  = time.Second
	backoffMax  = 30 * time.Second
	healthyLife = 60 * time.Second
)

// stop is why serve returned, or why there was nothing to serve.
type stop uint8

const (
	stopCtx    stop = iota // the supervisor's context ended
	stopEdit               // an edit asked for a rebuild; s.spec is the new one
	stopFailed             // a part of the generation ended, or it could not be built
)

// edit is one Submit: the new spec and where its answer goes.
type edit struct {
	sp  spec.Stream
	ans chan spec.Change // buffered: an answer never blocks
}

// answer sends the edit's Change and closes its channel.
func (e edit) answer(c spec.Change) {
	e.ans <- c
	close(e.ans)
}

// Supervisor runs one stream: it builds a generation from the spec, serves
// it, applies output edits in place, and rebuilds it for any other edit or
// after a failure.
type Supervisor struct {
	name  string
	env   Env
	log   *slog.Logger
	now   func() time.Time
	wait  func(time.Duration) <-chan time.Time
	after <-chan struct{}
	done  chan struct{}

	view atomic.Pointer[View]
	eofs atomic.Uint64 // inputs that ended with io.EOF and were restarted

	// box is the mailbox: Submit appends, the Run goroutine takes.
	box struct {
		sync.Mutex
		queue  []edit
		closed bool
	}
	signal chan struct{} // one slot: the mailbox may hold something

	// pending are the edits that asked for the generation being built. Run
	// goroutine only.
	pending []edit
	gens    uint64 // generations built; Run goroutine only
	// published, when set, is called by the Run goroutine right after a
	// View is swapped in, before reattach goes on. It is a seam for the
	// tests of reattach's order; nothing sets it outside them.
	published func(*View)

	// mu guards what Status, Shot and Collect read. Only the Run goroutine
	// writes spec, cur and pushes, so it reads them without mu.
	mu        sync.Mutex
	spec      spec.Stream
	cur       *generation // nil between generations
	pushes    []*pushRun  // cur's pushers, in the spec's order; replaced, never changed
	backoff   bool        // between generations after a failure
	stopped   bool        // Run has returned
	restarts  uint64
	rebuilds  uint64
	lastErr   string // "" when there was none, or an edit's rebuild cleared it
	lastErrAt time.Time
}

// NewSupervisor returns the supervisor of sp, which has passed Check and is
// not disabled. after, when not nil, is the Done of the previous supervisor
// of the same name. Run waits for it before it builds anything, and Done is
// never closed before after is: a supervisor that is cancelled while it
// waits still waits for after before it closes Done. So the chain is
// transitive: whatever was started and cancelled in between, a supervisor
// builds only when every earlier one of its name has let go of its sockets.
func NewSupervisor(sp spec.Stream, env Env, after <-chan struct{}) *Supervisor {
	if env.Logger == nil {
		env.Logger = slog.New(slog.DiscardHandler)
	}
	env.Logger = env.Logger.With("stream", sp.Name)
	s := &Supervisor{
		name:   sp.Name,
		env:    env,
		log:    env.Logger,
		now:    env.Now,
		wait:   env.After,
		after:  after,
		done:   make(chan struct{}),
		signal: make(chan struct{}, 1),
		spec:   cloneSpec(sp),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.wait == nil {
		s.wait = time.After
	}
	s.view.Store(&View{})
	return s
}

// cloneSpec returns a deep copy of sp: the supervisor keeps no slice its
// caller can change. Redacted would copy too, but it hides the URLs.
func cloneSpec(sp spec.Stream) spec.Stream {
	c := sp
	c.Inputs = append([]spec.Input(nil), sp.Inputs...)
	c.Layout = append([]string(nil), sp.Layout...)
	if sp.Transcode != nil {
		t := *sp.Transcode
		t.Renditions = append([]spec.Rendition(nil), t.Renditions...)
		if t.Watermark != nil {
			w := *t.Watermark
			t.Watermark = &w
		}
		c.Transcode = &t
	}
	if sp.Thumbnail != nil {
		th := *sp.Thumbnail
		c.Thumbnail = &th
	}
	c.Outputs.Push = append([]spec.Push(nil), sp.Outputs.Push...)
	return c
}

// Done is closed when Run has returned.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// View returns what the current generation serves. It is never nil, and
// empty between generations.
func (s *Supervisor) View() *View { return s.view.Load() }

// Submit queues an edit and never blocks. The channel gets the Change once
// it is applied (for a rebuild: once the old generation is torn down and the
// new one's View is published, or its build has failed), and is closed
// without a value if Run ends first. sp has passed Check, has the
// supervisor's name and is not disabled.
func (s *Supervisor) Submit(sp spec.Stream) <-chan spec.Change {
	e := edit{sp: cloneSpec(sp), ans: make(chan spec.Change, 1)}
	s.box.Lock()
	if s.box.closed {
		s.box.Unlock()
		close(e.ans)
		return e.ans
	}
	s.box.queue = append(s.box.queue, e)
	s.box.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
	return e.ans
}

// take removes the oldest queued edit.
func (s *Supervisor) take() (edit, bool) {
	s.box.Lock()
	defer s.box.Unlock()
	if len(s.box.queue) == 0 {
		return edit{}, false
	}
	e := s.box.queue[0]
	s.box.queue[0] = edit{}
	s.box.queue = s.box.queue[1:]
	return e, true
}

// closeMailbox closes the channel of every edit not answered, and makes
// every later Submit return a closed channel.
func (s *Supervisor) closeMailbox() {
	s.box.Lock()
	queue := s.box.queue
	s.box.queue, s.box.closed = nil, true
	s.box.Unlock()
	for _, e := range append(s.pending, queue...) {
		close(e.ans)
	}
	s.pending = nil
	s.mu.Lock()
	s.stopped, s.backoff = true, false
	s.mu.Unlock()
}

// Run runs the stream until ctx ends, and returns with everything stopped
// and waited for. It must be called once.
func (s *Supervisor) Run(ctx context.Context) {
	defer close(s.done) // runs last: after the wait for s.after below, on every path
	defer s.closeMailbox()
	if s.after != nil {
		select {
		case <-s.after: // the previous supervisor of this name has stopped
		case <-ctx.Done():
			// Cancelled while waiting: still wait, so Done stays behind
			// after. Bounded: the manager cancels a predecessor before it
			// starts a successor.
			<-s.after
			return
		}
	}
	wait := backoffMin
	for ctx.Err() == nil {
		reason, lived := stopFailed, time.Duration(0)
		g, err := s.build(ctx)
		if err != nil {
			s.fail("build: ", err)
			s.log.Warn("stream: build failed", "err", err, "retry_in", wait)
			s.release(nil, stopFailed)
			s.answerRebuilds()
		} else {
			// Starts the outputs and publishes the View.
			s.reattach(g, spec.Outputs{}, s.spec.Outputs) //nolint:contextcheck // the outputs run under g's context, a child of ctx
			s.answerRebuilds()
			reason = s.serve(ctx, g)
			lived = s.now().Sub(g.started)
			s.teardown(g, reason)
		}
		switch reason {
		case stopCtx:
			return
		case stopEdit: // s.spec is already the new one; no wait
		case stopFailed:
			if lived >= healthyLife {
				wait = backoffMin
			}
			if !s.idle(ctx, wait) {
				return
			}
			wait = min(2*wait, backoffMax)
		}
	}
}

// serve applies the queued edits to g until ctx ends, an edit asks for a
// rebuild, or a part of g ends.
func (s *Supervisor) serve(ctx context.Context, g *generation) stop {
	for {
		if ctx.Err() != nil {
			return stopCtx
		}
		for {
			e, ok := s.take()
			if !ok {
				break
			}
			switch spec.Diff(s.spec, e.sp) {
			case spec.ChangeNone:
				e.answer(spec.ChangeNone)
			case spec.ChangeOutputs:
				old := s.spec.Outputs
				s.setSpec(e.sp)
				s.reattach(g, old, e.sp.Outputs) //nolint:contextcheck // the outputs run under g's context, a child of ctx
				e.answer(spec.ChangeOutputs)
			case spec.ChangeLive, spec.ChangeRebuild: // ChangeLive: applied in place from Task 8
				// Answered after the next generation's reattach, or its
				// failed build. The edits behind it stay queued for that
				// generation.
				s.rebuildFor(e)
				return stopEdit
			}
		}
		select {
		case <-ctx.Done():
			return stopCtx
		case <-s.signal:
		case <-g.failed:
			prefix, err := g.failure()
			s.fail(prefix, err)
			s.log.Warn("stream: generation failed", "generation", g.n, "part", prefix, "err", err)
			return stopFailed
		}
	}
}

// idle waits out a backoff of wait. It is false when ctx ended. An edit
// that changes the spec replaces it and ends the wait.
func (s *Supervisor) idle(ctx context.Context, wait time.Duration) bool {
	var timer <-chan time.Time
	for {
		changed := false
		for {
			e, ok := s.take()
			if !ok {
				break
			}
			switch spec.Diff(s.spec, e.sp) {
			case spec.ChangeNone:
				e.answer(spec.ChangeNone)
			case spec.ChangeOutputs:
				// Nothing is attached: the next generation starts with them.
				s.setSpec(e.sp)
				e.answer(spec.ChangeOutputs)
				changed = true
			case spec.ChangeLive, spec.ChangeRebuild: // ChangeLive: applied in place from Task 8
				s.rebuildFor(e)
				changed = true
			}
		}
		if changed {
			return true
		}
		if timer == nil {
			timer = s.wait(wait)
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer:
			return true
		case <-s.signal:
		}
	}
}

// setSpec replaces the spec.
func (s *Supervisor) setSpec(sp spec.Stream) {
	s.mu.Lock()
	s.spec = sp
	s.mu.Unlock()
}

// rebuildFor takes e's spec for the next generation, which answers e. A
// rebuild for an edit clears the last error.
func (s *Supervisor) rebuildFor(e edit) {
	s.mu.Lock()
	s.spec = e.sp
	s.lastErr, s.lastErrAt = "", time.Time{}
	s.mu.Unlock()
	s.pending = append(s.pending, e)
}

// answerRebuilds answers the edits that asked for the generation just
// built, or whose build just failed.
func (s *Supervisor) answerRebuilds() {
	for _, e := range s.pending {
		e.answer(spec.ChangeRebuild)
	}
	s.pending = nil
}

// fail records the stream's last error, scrubbed of URL secrets. It is safe
// from any goroutine.
func (s *Supervisor) fail(prefix string, err error) {
	msg := prefix + logging.Scrub(err.Error())
	s.mu.Lock()
	s.lastErr, s.lastErrAt = msg, time.Now()
	s.mu.Unlock()
}

// release ends g's time as the current generation (g is nil after a failed
// build) and counts why.
func (s *Supervisor) release(g *generation, reason stop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g != nil && s.cur == g {
		s.cur, s.pushes = nil, nil
	}
	switch reason {
	case stopCtx:
	case stopEdit:
		s.rebuilds++
	case stopFailed:
		s.restarts++
		s.backoff = true
	}
}
