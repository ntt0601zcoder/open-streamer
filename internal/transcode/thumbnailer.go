package transcode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// Defaults for zero ThumbnailerOptions fields.
const (
	defaultSilenceTimeout = 10 * time.Second
	// thumbnailerQueue is the Thumbnailer's source subscription queue.
	thumbnailerQueue = 64
)

// ThumbnailerOptions configures a Thumbnailer. Zero values select the
// defaults.
type ThumbnailerOptions struct {
	Binary string   // the transcoder child, cmd/ostranscoder
	Args   []string // arguments to Binary
	Env    []string // added to the server's environment for the child

	Thumbnail picture.Thumbnail

	RespawnMin time.Duration // 0 means 250 ms
	RespawnMax time.Duration // 0 means 5 s
	// HelloTimeout: a child without Hello by then is killed; 0 means 10 s.
	HelloTimeout time.Duration
	// SilenceTimeout: a child that has said Hello and then sends nothing
	// for this long is killed; 0 means 10 s. The child sends Stats every
	// second, so it must exceed that.
	SilenceTimeout time.Duration

	Now   func() time.Time                     // nil means time.Now
	After func(time.Duration) <-chan time.Time // nil means time.After

	Logger *slog.Logger // nil discards
	// OnShot is called with every thumbnail, from the child's reader
	// goroutine: it must not block. Optional.
	OnShot func(picture.Shot)
	// OnEvent is called from the Run goroutine with EventSpawned (PID set)
	// when a child starts and EventExited (Err set) when one exits, is
	// killed, or cannot be started, as the Transcoder's is. Optional.
	OnEvent func(Event)
}

// ThumbnailerStats counts a Thumbnailer's work: the children started and
// those that exited or failed to start, the keyframes sent to a child and
// the due ones that were not (the child had not said Hello, or still had
// the previous key to take), the shots received, and the shots the children
// reported they could not make (the sum of their Stats.ThumbnailsFailed; a
// child's count after its last Stats is not included).
type ThumbnailerStats struct{ Spawns, Exits, KeysSent, KeysSkipped, Shots, Failed uint64 }

// Thumbnailer makes the thumbnails of a stream that has no transcoding
// child (ruling R-C10a): it runs a child of its own that is configured for
// shots only, and sends it the keyframes to shoot.
//
// The thumbnail schedule is the host's. Of the source hub's main video
// track, the lowest-numbered video track seen so far, a keyframe that is
// due is offered to the child, unshifted, through a one-slot channel. The
// child takes it when it has said Hello and the slot is free; otherwise the
// key is skipped and the next one is offered. Nothing else is sent, and the
// child shoots every key it gets. A shot the child reports as failed is
// retried under shotRetry.
//
// A child that exits, or cannot be started (a binary that does not exist
// included), is respawned after a backoff, as the Transcoder's is, and the
// schedule starts over. A child that sends no Hello within
// HelloTimeout, nothing for SilenceTimeout after it, or anything the host
// cannot decode, is killed and counts as an exit. No hub is written.
type Thumbnailer struct {
	src       *hub.Hub
	opt       ThumbnailerOptions
	log       *slog.Logger
	configure wire.Configure
	every     int // the schedule's interval, in seconds

	running atomic.Bool
	// onHello, when set by a test, is called from the Run goroutine when it
	// has handled a child's Hello.
	onHello func(pid int)

	mu    sync.Mutex
	stats ThumbnailerStats
	// The schedule and its retry rule: Run asks them at every key, and the
	// child's reader resets them on the child's Stats.
	sched *picture.Schedule
	retry shotRetry
	// The latest thumbnail, and when it arrived.
	shot     picture.Shot
	shotAt   time.Time
	haveShot bool
}

// NewThumbnailer validates opt and returns a Thumbnailer for the source hub
// src. It returns errors.ErrUnsupported where the child cannot run.
func NewThumbnailer(src *hub.Hub, opt ThumbnailerOptions) (*Thumbnailer, error) {
	if src == nil {
		return nil, errors.New("transcode: new thumbnailer: no source hub")
	}
	if !spawnSupported {
		return nil, fmt.Errorf("transcode: new thumbnailer: %w", errors.ErrUnsupported)
	}
	if opt.Binary == "" {
		return nil, errors.New("transcode: new thumbnailer: no transcoder binary")
	}
	if err := opt.Thumbnail.Validate(); err != nil {
		return nil, fmt.Errorf("transcode: new thumbnailer: %w", err)
	}
	opt.RespawnMin = orDefault(opt.RespawnMin, defaultRespawnMin)
	opt.RespawnMax = max(orDefault(opt.RespawnMax, defaultRespawnMax), opt.RespawnMin)
	opt.HelloTimeout = orDefault(opt.HelloTimeout, defaultHelloTimeout)
	opt.SilenceTimeout = orDefault(opt.SilenceTimeout, defaultSilenceTimeout)
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.After == nil {
		opt.After = time.After
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	every := opt.Thumbnail.Resolved().IntervalSec
	th := opt.Thumbnail
	return &Thumbnailer{
		src: src, opt: opt, log: log, every: every,
		configure: wire.Configure{Thumbnail: &th},
		sched:     picture.NewSchedule(every),
		retry:     newShotRetry(),
	}, nil
}

// Shot returns the latest thumbnail and when it arrived, by
// ThumbnailerOptions.Now. ok is false while there is none. A shot's PTS is
// in source time. The caller must not change its JPEG.
func (t *Thumbnailer) Shot() (s picture.Shot, at time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.shot, t.shotAt, t.haveShot
}

// Stats returns the current counters.
func (t *Thumbnailer) Stats() ThumbnailerStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

func (t *Thumbnailer) emit(e Event) {
	if t.opt.OnEvent != nil {
		t.opt.OnEvent(e)
	}
}

func (t *Thumbnailer) update(fn func(s *ThumbnailerStats)) {
	t.mu.Lock()
	fn(&t.stats)
	t.mu.Unlock()
}

// thumbRun is the state of one Run call, owned by its goroutine.
type thumbRun struct {
	t         *Thumbnailer
	wg        sync.WaitGroup // child goroutines
	c         *thumbChild
	mainVideo int // lowest-numbered video track seen, or -1
	respawn   <-chan time.Time
	silence   <-chan time.Time // the current child's silence check
	backoff   backoff
}

// Run makes thumbnails until the source hub closes (it returns nil) or ctx
// ends (it returns ctx.Err()), as Transcoder.Run does, after it has stopped
// its child. A child that announces another wire version is fatal: Run
// returns that error. A binary that does not exist is not: like any spawn
// failure it is logged and retried after the backoff, so that a binary
// installed later is picked up. Run may be called once.
func (t *Thumbnailer) Run(ctx context.Context) error {
	if !t.running.CompareAndSwap(false, true) {
		return errors.New("transcode: thumbnailer: run: called twice")
	}
	sub := t.src.SubscribeLive("thumbnailer", thumbnailerQueue)
	defer sub.Close()
	s := &thumbRun{t: t, mainVideo: -1}
	s.backoff = backoff{min: t.opt.RespawnMin, max: t.opt.RespawnMax, healthy: defaultHealthyAfter}
	err := s.loop(ctx, sub)
	if s.c != nil {
		s.c.stop()
	}
	s.wg.Wait()
	return err
}

func (s *thumbRun) loop(ctx context.Context, sub *hub.Sub) error {
	t := s.t
	s.spawn()
	for {
		var done, hello <-chan struct{}
		if s.c != nil {
			done, hello = s.c.done, s.c.hello
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-sub.C():
			if !ok {
				return nil
			}
			// A Lost delivery needs nothing: the child holds no state
			// between keys.
			s.frame(d.Frame)
		case <-hello:
			s.c.ready = true
			s.silence = t.opt.After(t.opt.SilenceTimeout)
			if t.onHello != nil {
				t.onHello(s.c.p.pid)
			}
		case <-s.silence:
			s.checkSilence()
		case <-done:
			if err := s.exited(); err != nil {
				return err
			}
		case <-s.respawn:
			s.respawn = nil
			s.spawn()
		}
	}
}

// frame offers f to the child when it is a keyframe of the main video track
// that the schedule is due.
func (s *thumbRun) frame(f media.Frame) {
	if f.Kind() != media.KindVideo {
		return
	}
	if s.mainVideo < 0 || f.Track < s.mainVideo {
		s.mainVideo = f.Track
	}
	if f.Track != s.mainVideo || !f.Key {
		return
	}
	t := s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sched.Due(f.Epoch, f.PTS) {
		return
	}
	if c := s.c; c != nil && c.ready {
		select {
		case c.keys <- f:
			t.stats.KeysSent++
			t.sched.Took(f.Epoch, f.PTS)
			return
		default:
		}
	}
	// Not taken: the next key is offered.
	t.stats.KeysSkipped++
}

// checkSilence kills the current child when it has sent nothing for
// SilenceTimeout, and otherwise looks again when that would be.
func (s *thumbRun) checkSilence() {
	t, c := s.t, s.c
	s.silence = nil
	if c == nil || !c.ready {
		return
	}
	timeout := t.opt.SilenceTimeout
	if idle := t.opt.Now().Sub(time.Unix(0, c.lastMsg.Load())); idle < timeout {
		s.silence = t.opt.After(timeout - idle)
		return
	}
	c.silent = fmt.Errorf("transcode: thumbnailer: silent for %v", timeout)
	t.log.Warn("transcode: thumbnailer: child sends nothing", "pid", c.p.pid, "for", timeout)
	c.kill()
}

// spawn starts a child. A failure, a binary that does not exist included,
// counts as an exit and schedules a respawn after the backoff, as the
// Transcoder's spawn does.
func (s *thumbRun) spawn() {
	t := s.t
	t.update(func(st *ThumbnailerStats) { st.Spawns++ })
	c, err := t.startChild(&s.wg)
	if err != nil {
		t.update(func(st *ThumbnailerStats) { st.Exits++ })
		t.log.Warn("transcode: thumbnailer: spawn failed", "err", err)
		t.emit(Event{Kind: EventExited, Err: err})
		s.respawn = t.opt.After(s.backoff.next(0))
		return
	}
	s.c = c
	t.emit(Event{Kind: EventSpawned, PID: c.p.pid})
}

// exited handles the end of the current child: a fatal error ends Run, any
// other exit starts the schedule over and schedules a respawn.
func (s *thumbRun) exited() error {
	t, c := s.t, s.c
	s.c, s.silence = nil, nil
	if c.fatal != nil {
		return c.fatal
	}
	err := c.waitErr
	if err == nil {
		err = errors.New("exited")
	}
	cause := c.cause
	if c.silent != nil {
		cause = c.silent
	}
	if cause != nil {
		err = fmt.Errorf("%w: %w", cause, err)
	}
	t.log.Warn("transcode: thumbnailer: child exited", "pid", c.p.pid, "err", err)
	t.mu.Lock()
	t.stats.Exits++
	// The reader of c has ended: nothing of c touches these any more.
	t.sched = picture.NewSchedule(t.every)
	t.retry.childExited()
	t.mu.Unlock()
	t.emit(Event{Kind: EventExited, PID: c.p.pid, Err: fmt.Errorf("transcode: thumbnailer: child %d: %w", c.p.pid, err)})
	s.respawn = t.opt.After(s.backoff.next(t.opt.Now().Sub(c.started)))
	return nil
}

// thumbChild is one thumbnail child and its goroutines.
type thumbChild struct {
	p       *proc
	stderr  *logWriter
	w       *wire.Writer
	keys    chan media.Frame // the one slot
	quit    chan struct{}    // closed to stop
	helloOK chan struct{}    // closed by the reader on a valid Hello
	hello   chan struct{}    // receives once, on a valid Hello
	exited  chan struct{}    // closed once the process is reaped
	done    chan struct{}    // closed after exited, once the reader has ended
	started time.Time
	stopped sync.Once

	// Owned by the Run goroutine: Run has handled the child's Hello, and
	// why Run killed the child, if it did.
	ready  bool
	silent error

	reapMu sync.Mutex
	reaped bool // the waiter has reaped the process: its pgid may be reused

	// Written before done is closed.
	waitErr error
	fatal   error
	cause   error // why the reader gave up on the child, if it did

	// lastMsg is when the child last sent a message, by Options.Now, in
	// Unix nanoseconds; Run reads it to tell a silent child.
	lastMsg atomic.Int64
}

// startChild spawns the child and starts its reader, writer and waiter.
func (t *Thumbnailer) startChild(wg *sync.WaitGroup) (*thumbChild, error) {
	env := append(os.Environ(), t.opt.Env...)
	lw := newLogWriter(t.log, nil)
	p, err := startProc(t.opt.Binary, t.opt.Args, env, lw)
	if err != nil {
		return nil, err
	}
	lw.setPID(p.pid)
	c := &thumbChild{
		p:       p,
		stderr:  lw,
		w:       wire.NewWriter(p.conn),
		keys:    make(chan media.Frame, 1),
		quit:    make(chan struct{}),
		helloOK: make(chan struct{}),
		hello:   make(chan struct{}, 1),
		exited:  make(chan struct{}),
		done:    make(chan struct{}),
		started: t.opt.Now(),
	}
	readDone := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		defer close(readDone)
		c.cause = t.readChild(c)
		if c.cause == nil {
			// The child closed its socket: let it exit on its own, with
			// its exit status and last stderr lines, before killing it.
			timer := time.NewTimer(stopGrace)
			select {
			case <-c.exited:
			case <-timer.C:
			}
			timer.Stop()
		}
		c.kill()
	}()
	go func() {
		defer wg.Done()
		t.writeChild(c)
	}()
	go func() {
		defer wg.Done()
		c.wait(readDone)
	}()
	return c, nil
}

// readChild reads the child's messages until its socket ends; the caller
// then kills the child if it is still running. It returns why the host gave
// up on the child, as Transcoder.readChild does, and sets c.fatal for a
// child that speaks another wire version.
func (t *Thumbnailer) readChild(c *thumbChild) error {
	r := wire.NewReader(bufio.NewReaderSize(c.p.conn, 64<<10))
	c.readDeadline(time.Now().Add(t.opt.HelloTimeout))
	if _, err := readHello(r); err != nil {
		// The version first: a child that exits right after a Hello with
		// another version must not lose its fatal error to its exit.
		if errors.Is(err, errVersion) {
			c.fatal = err
			c.kill()
			return nil
		}
		return helloFailed(err, c.isExited(), t.opt.HelloTimeout)
	}
	c.readDeadline(time.Time{})
	c.lastMsg.Store(t.opt.Now().UnixNano())
	close(c.helloOK)
	c.hello <- struct{}{}
	var failed uint64 // the child's ThumbnailsFailed, at its highest
	for {
		m, err := r.Read()
		if err != nil {
			if socketEnded(err) || c.isExited() {
				return nil
			}
			return fmt.Errorf("transcode: read: %w", err)
		}
		c.lastMsg.Store(t.opt.Now().UnixNano())
		switch v := m.(type) {
		case wire.Shot:
			t.gotShot(v.S)
		case wire.Stats:
			t.mu.Lock()
			if v.ThumbnailsFailed > failed {
				t.stats.Failed += v.ThumbnailsFailed - failed
				failed = v.ThumbnailsFailed
			}
			if t.retry.look(v.Thumbnails, v.ThumbnailsFailed) {
				t.sched = picture.NewSchedule(t.every)
			}
			t.mu.Unlock()
		case wire.Error:
			t.log.Warn("transcode: thumbnailer: child error", "pid", c.p.pid, "fatal", v.Fatal, "msg", v.Message)
		default:
		}
	}
}

// gotShot keeps a child's thumbnail as the latest and passes it on. Called
// from the child's reader goroutine.
func (t *Thumbnailer) gotShot(s picture.Shot) {
	at := t.opt.Now()
	t.mu.Lock()
	t.shot, t.shotAt, t.haveShot = s, at, true
	t.stats.Shots++
	t.mu.Unlock()
	if t.opt.OnShot != nil {
		t.opt.OnShot(s)
	}
}

// writeChild sends Configure after the child's Hello, then each key from
// the slot, and Stop when the child is stopped.
func (t *Thumbnailer) writeChild(c *thumbChild) {
	select {
	case <-c.helloOK:
	case <-c.quit:
		// The child reads its socket once it has sent Hello: Stop then
		// ends it without waiting out the kill.
		_ = c.w.Write(wire.Stop{})
		return
	case <-c.exited:
		return
	}
	if err := c.w.Write(t.configure); err != nil {
		c.kill()
		return
	}
	oversizeLogged := false // once per child, as the Transcoder's send does
	for {
		select {
		case f := <-c.keys:
			err := c.w.Write(wire.Frame{Rung: -1, F: f})
			switch {
			case err == nil:
			case errors.Is(err, wire.ErrLimit):
				// Nothing was written: the child lives, without this key.
				// A source whose every key is too large would otherwise
				// log at every interval, forever.
				if !oversizeLogged {
					oversizeLogged = true
					t.log.Warn("transcode: thumbnailer: key dropped: the wire protocol cannot carry it",
						"pid", c.p.pid, "err", err)
				}
			default:
				c.kill()
				return
			}
		case <-c.quit:
			_ = c.w.Write(wire.Stop{})
			return
		case <-c.exited:
			return
		}
	}
}

// readDeadline sets the socket's read deadline, unless the child has been
// reaped: the waiter then sets its own.
func (c *thumbChild) readDeadline(at time.Time) {
	c.reapMu.Lock()
	defer c.reapMu.Unlock()
	if !c.reaped {
		_ = c.p.conn.SetReadDeadline(at)
	}
}

// isExited reports whether the child has been reaped.
func (c *thumbChild) isExited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// kill kills the child's process group, unless the child has already been
// reaped, after which its pgid may belong to another group.
func (c *thumbChild) kill() {
	c.reapMu.Lock()
	defer c.reapMu.Unlock()
	if !c.reaped {
		c.p.killGroup()
	}
}

// wait reaps the process, then lets the reader drain what the child wrote.
func (c *thumbChild) wait(readDone <-chan struct{}) {
	err := c.p.cmd.Wait()
	c.stderr.close(err != nil) // Wait has copied all of the child's stderr
	c.reapMu.Lock()
	c.reaped = true
	c.waitErr = err
	_ = c.p.conn.SetReadDeadline(time.Now().Add(time.Second))
	c.reapMu.Unlock()
	close(c.exited)
	<-readDone
	_ = c.p.conn.Close()
	close(c.done)
}

// stop asks the child to stop at once: there is no queue to drain, so its
// writer sends Stop as soon as it is free. After stopGrace it kills the
// child's process group. It returns once the child is reaped and its reader
// has ended.
func (c *thumbChild) stop() {
	c.stopped.Do(func() {
		_ = c.p.conn.SetWriteDeadline(time.Now().Add(stopGrace))
		close(c.quit)
	})
	waitOrKill(c.done, c.kill)
	<-c.done
}
