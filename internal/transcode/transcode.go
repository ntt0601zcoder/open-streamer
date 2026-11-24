// Package transcode supervises the per-stream transcoder subprocess and turns
// one source hub into one hub per rendition.
//
// The host subscribes once to the source hub for the whole run. Every
// delivery gets its rendition epoch from an epoch map and is shifted by
// Delay (ruling R-M4c), then goes to the child when its track is encoded, or
// straight to the renditions' mergers when it is copied. The child
// (cmd/ostranscoder) is reached over a socketpair passed as fd 3 and speaks
// the wire protocol; its outputs go by rung to the mergers, audio (rung -1)
// to every rendition. A merger per rendition writes its hub in DTS order and
// sets each epoch's anchor before the epoch's first frame.
//
// # Rendition epochs
//
// Upstream epoch e maps to e + offset. A child exit or a loss on the source
// subscription requests a bump: a new rendition epoch, numbered (largest
// issued) + 1, that takes effect at K, the next source video keyframe (the
// next frame without live video) whose PTS is above everything already sent
// in the old epoch. After a respawn, K is searched for only once the new
// child has said Hello, and the child is fed from K. A bump requested while
// one is pending is absorbed, and an upstream epoch that starts exactly at K
// is the bump's epoch. Each rendition epoch has one anchor, shared by every
// rendition hub: an upstream epoch's anchor shifted by Delay, or for a bump
// Start = K + Delay and Wall = the upstream Wall + (K − upstream Start) +
// Delay.
//
// # Delay
//
// Every frame and anchor of every rendition is presented at source time +
// Delay, so that the encoder's reorder delay never makes a DTS negative.
// The bound holds for B-frames ≤ 3 at 8 fps or more (B-frames 2: 6 fps); a
// slower source should use BFrames -1. New rejects BFrames above 3.
//
// # Respawn
//
// A child that exits for any reason other than the end of Run is respawned
// after a backoff: RespawnMin, doubling up to RespawnMax, back to RespawnMin
// once a child has run for HealthyAfter. A child that sends no Hello within
// HelloTimeout, or anything the host cannot decode, is killed and counts as
// an exit. A child whose Hello announces another wire version is a fatal
// error that Run returns. Stopping a child
// sends Stop, waits 2 s, then kills its process group.
package transcode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// Delay is the rendition delay D in 90 kHz ticks (ruling R-M4c): every
// rendition frame and anchor is presented at source time + Delay.
const Delay = 45000

// Defaults for zero Options fields.
const (
	defaultQueue        = 1024
	defaultRespawnMin   = 250 * time.Millisecond
	defaultRespawnMax   = 5 * time.Second
	defaultHealthyAfter = 30 * time.Second
	defaultMergeHold    = 2 * time.Second
	defaultHelloTimeout = 10 * time.Second
	defaultInputStall   = 5 * time.Second

	// childQueue is the length of the child's input channel.
	childQueue = 256
	// maxBacklog bounds the frames held for a child before its Hello: the
	// hub's GOP cache, which Subscribe replays in one burst.
	maxBacklog = 1500
	// stopGrace is how long a stopping child has before it is killed.
	stopGrace = 2 * time.Second
	// maxBFrames is the largest BFrames that Delay covers.
	maxBFrames = 3
)

// Video describes a rendition's video: copied from the source, or encoded
// at Height (width keeps the aspect ratio). BFrames -1 means none, 0 means
// the encoder default (2); at most 3.
type Video struct {
	Copy                                         bool
	Height, BitrateKbps, MaxBitrateKbps, BFrames int
	Preset                                       string
}

// Audio describes the audio of every rendition: copied from the source, or
// re-encoded to AAC by the child (zero values: 128 kbps, 48 kHz, stereo).
type Audio struct {
	Copy                              bool
	BitrateKbps, SampleRate, Channels int
}

// Rendition is one output hub.
type Rendition struct {
	Name  string
	Video Video
}

// Options configures a Transcoder. Zero values select the defaults.
type Options struct {
	Binary string   // the transcoder child, cmd/ostranscoder
	Args   []string // arguments to Binary
	Env    []string // added to the server's environment for the child

	Renditions []Rendition
	Audio      Audio

	Queue int // source subscription queue; 0 means 1024

	RespawnMin   time.Duration // 0 means 250 ms
	RespawnMax   time.Duration // 0 means 5 s
	HealthyAfter time.Duration // 0 means 30 s
	MergeHold    time.Duration // 0 means 2 s
	HelloTimeout time.Duration // a child without Hello by then is killed; 0 means 10 s
	// InputStallTimeout bounds how long Run waits to queue a frame for a
	// child that takes none of its input: a child whose writer makes no
	// progress for that long is killed ("input stalled"); 0 means 5 s.
	InputStallTimeout time.Duration

	Now   func() time.Time                     // nil means time.Now
	After func(time.Duration) <-chan time.Time // nil means time.After

	Logger  *slog.Logger // nil discards
	OnEvent func(Event)  // optional, called from the Run goroutine
}

// EventKind is the kind of an Event.
type EventKind uint8

// Event kinds.
const (
	EventSpawned EventKind = iota + 1 // a child started; PID is set
	EventExited                       // a child exited or failed to start; Err is set
	EventEpoch                        // a rendition epoch was issued; Epoch is set
	EventLoss                         // the source subscription lost frames
)

// Event reports a change in the transcoder.
type Event struct {
	Kind  EventKind
	PID   int
	Epoch uint32
	Err   error
}

// Stats counts the transcoder's work. FramesOut has one entry per
// rendition: the frames written to its hub. MaxQueued is the high-water
// mark of the child's input channel (at most 256); it excludes the
// pre-Hello backlog, which can hold up to 1500 frames. PID is the running
// child's, or 0. DroppedBeforeKey also counts the frames given up on
// because the child exited, stalled, or Run ended while they waited.
type Stats struct {
	Spawns, Exits, Losses, DroppedBeforeKey, MergeDropped uint64
	FramesIn                                              uint64
	FramesOut                                             []uint64
	MaxQueued                                             int
	PID                                                   int
}

// Transcoder turns one source hub into one hub per rendition.
type Transcoder struct {
	src     *hub.Hub
	opt     Options
	log     *slog.Logger
	hubs    []*hub.Hub
	mergers []*merger
	anchors anchorStore

	needChild  bool
	encodeIdx  []int // rung index → rendition index
	copyVideo  []int // renditions that copy video
	configure  wire.Configure
	childVideo bool

	running  atomic.Bool
	childOut atomic.Uint64 // frames received from children
	bumps    atomic.Uint64 // bumps that took effect

	mu    sync.Mutex
	stats Stats
}

// New validates opt and returns a Transcoder whose rendition hubs are open.
func New(src *hub.Hub, opt Options) (*Transcoder, error) {
	if src == nil {
		return nil, errors.New("transcode: new: no source hub")
	}
	if len(opt.Renditions) == 0 {
		return nil, errors.New("transcode: new: no rendition")
	}
	t := &Transcoder{src: src}
	names := map[string]bool{}
	for i, r := range opt.Renditions {
		if r.Name == "" || names[r.Name] {
			return nil, fmt.Errorf("transcode: new: rendition %d: empty or duplicate name %q", i, r.Name)
		}
		names[r.Name] = true
		if r.Video.Copy {
			t.copyVideo = append(t.copyVideo, i)
			continue
		}
		v := r.Video
		switch {
		case v.Height <= 0:
			return nil, fmt.Errorf("transcode: new: rendition %q: height %d", r.Name, v.Height)
		case v.BitrateKbps <= 0 || v.MaxBitrateKbps < 0:
			return nil, fmt.Errorf("transcode: new: rendition %q: bitrate %d, max %d", r.Name, v.BitrateKbps, v.MaxBitrateKbps)
		case v.BFrames < -1 || v.BFrames > maxBFrames:
			return nil, fmt.Errorf("transcode: new: rendition %q: bframes %d outside [-1, %d]", r.Name, v.BFrames, maxBFrames)
		}
		t.encodeIdx = append(t.encodeIdx, i)
		t.configure.Rungs = append(t.configure.Rungs, wire.Rung{
			Height: v.Height, BitrateKbps: v.BitrateKbps, MaxBitrateKbps: v.MaxBitrateKbps,
			BFrames: v.BFrames, Preset: v.Preset,
		})
	}
	if a := opt.Audio; !a.Copy {
		if a.BitrateKbps < 0 || a.SampleRate < 0 || a.Channels < 0 {
			return nil, fmt.Errorf("transcode: new: audio: bitrate %d, rate %d, channels %d", a.BitrateKbps, a.SampleRate, a.Channels)
		}
		t.configure.Audio = &wire.AudioEncode{BitrateKbps: a.BitrateKbps, SampleRate: a.SampleRate, Channels: a.Channels}
	}
	t.childVideo = len(t.encodeIdx) > 0
	t.needChild = t.childVideo || !opt.Audio.Copy
	if t.needChild {
		if !spawnSupported {
			return nil, fmt.Errorf("transcode: new: %w", errors.ErrUnsupported)
		}
		if opt.Binary == "" {
			return nil, errors.New("transcode: new: no transcoder binary")
		}
	}

	if opt.Queue <= 0 {
		opt.Queue = defaultQueue
	}
	opt.RespawnMin = orDefault(opt.RespawnMin, defaultRespawnMin)
	opt.RespawnMax = max(orDefault(opt.RespawnMax, defaultRespawnMax), opt.RespawnMin)
	opt.HealthyAfter = orDefault(opt.HealthyAfter, defaultHealthyAfter)
	opt.MergeHold = orDefault(opt.MergeHold, defaultMergeHold)
	opt.HelloTimeout = orDefault(opt.HelloTimeout, defaultHelloTimeout)
	opt.InputStallTimeout = orDefault(opt.InputStallTimeout, defaultInputStall)
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.After == nil {
		opt.After = time.After
	}
	t.log = opt.Logger
	if t.log == nil {
		t.log = slog.New(slog.DiscardHandler)
	}
	t.opt = opt

	t.stats.FramesOut = make([]uint64, len(opt.Renditions))
	for range opt.Renditions {
		h := hub.New(hub.Options{})
		t.hubs = append(t.hubs, h)
		t.mergers = append(t.mergers, newMerger(h, opt.MergeHold, opt.Now, t.anchors.get))
	}
	return t, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Hubs returns the rendition hubs, in Options.Renditions order. Run closes
// them when it returns.
func (t *Transcoder) Hubs() []*hub.Hub { return append([]*hub.Hub(nil), t.hubs...) }

// Stats returns the current counters.
func (t *Transcoder) Stats() Stats {
	t.mu.Lock()
	s := t.stats
	t.mu.Unlock()
	s.FramesOut = make([]uint64, len(t.mergers))
	for i, m := range t.mergers {
		out, dropped := m.counts()
		s.FramesOut[i] = out
		s.MergeDropped += dropped
	}
	return s
}

func (t *Transcoder) update(fn func(s *Stats)) {
	t.mu.Lock()
	fn(&t.stats)
	t.mu.Unlock()
}

func (t *Transcoder) emit(e Event) {
	if t.opt.OnEvent != nil {
		t.opt.OnEvent(e)
	}
}

// run is the state of one Run call, owned by its goroutine.
type run struct {
	t       *Transcoder
	ctx     context.Context
	wg      sync.WaitGroup // child goroutines
	r       *router
	c       *child
	spawned bool
	respawn <-chan time.Time
	delay   time.Duration
}

// Run transcodes until the source hub closes (it returns nil) or ctx ends
// (it returns ctx.Err()). It stops the child, flushes the mergers and closes
// the rendition hubs before returning. A child that announces another wire
// version is fatal: Run returns that error. Run may be called once.
func (t *Transcoder) Run(ctx context.Context) error {
	if !t.running.CompareAndSwap(false, true) {
		return errors.New("transcode: run: called twice")
	}
	sub := t.src.Subscribe("transcode", t.opt.Queue)
	defer sub.Close()

	// The merger timers run until the child is stopped, after ctx ends.
	mctx, mcancel := context.WithCancel(context.WithoutCancel(ctx))
	var mwg sync.WaitGroup
	for _, m := range t.mergers {
		mwg.Add(1)
		go func() {
			defer mwg.Done()
			m.run(mctx, t.opt.After)
		}()
	}

	s := &run{t: t, ctx: ctx, delay: t.opt.RespawnMin}
	s.r = t.newRouter(s)
	if t.needChild {
		s.spawn()
	}
	err := s.loop(sub)
	if s.c != nil {
		s.c.stop(err == nil)
		t.update(func(st *Stats) { st.PID = 0 })
	}
	s.wg.Wait()
	mcancel()
	mwg.Wait()
	for _, m := range t.mergers {
		m.flush()
		m.h.Close()
	}
	return err
}

func (s *run) loop(sub *hub.Sub) error {
	t := s.t
	for {
		var done, hello <-chan struct{}
		if s.c != nil {
			done, hello = s.c.done, s.c.hello
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case d, ok := <-sub.C():
			if !ok {
				return nil
			}
			t.update(func(st *Stats) { st.FramesIn++ })
			s.r.deliver(d)
		case <-hello:
			s.c.handOver()
			s.r.childHello()
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

// spawn starts a child. The first child is fed from the start of the run;
// a later one from K.
func (s *run) spawn() {
	t := s.t
	first := !s.spawned
	s.spawned = true
	t.update(func(st *Stats) { st.Spawns++ })
	c, err := t.startChild(&s.wg)
	if err != nil {
		t.update(func(st *Stats) { st.Exits++ })
		t.log.Warn("transcode: spawn failed", "err", err)
		t.emit(Event{Kind: EventExited, Err: err})
		s.r.childExited()
		s.scheduleRespawn(0)
		return
	}
	s.c = c
	t.update(func(st *Stats) { st.PID = c.p.pid })
	t.emit(Event{Kind: EventSpawned, PID: c.p.pid})
	if first {
		s.r.childFed = true
	}
}

// exited handles the end of the current child.
func (s *run) exited() error {
	t, c := s.t, s.c
	s.c = nil
	t.update(func(st *Stats) { st.PID = 0 })
	if c.fatal != nil {
		return c.fatal
	}
	err := c.waitErr
	if err == nil {
		err = errors.New("exited")
	}
	cause := c.cause
	if c.stalled != nil {
		cause = c.stalled
	}
	if cause != nil {
		err = fmt.Errorf("%w: %w", cause, err)
	}
	err = fmt.Errorf("transcode: child %d: %w", c.p.pid, err)
	t.update(func(st *Stats) { st.Exits++ })
	t.log.Warn("transcode: child exited", "pid", c.p.pid, "err", err)
	t.emit(Event{Kind: EventExited, PID: c.p.pid, Err: err})
	s.r.childExited()
	s.scheduleRespawn(t.opt.Now().Sub(c.started))
	return nil
}

func (s *run) scheduleRespawn(uptime time.Duration) {
	o := s.t.opt
	if uptime >= o.HealthyAfter {
		s.delay = o.RespawnMin
	}
	s.respawn = o.After(s.delay)
	s.delay = min(2*s.delay, o.RespawnMax)
}

// toChild queues m for the current child and reports whether it was
// queued.
//
// Until Run has taken the child's Hello, m goes to the child's pre-Hello
// backlog, up to maxBacklog messages, and toChild never waits: Subscribe
// replays the source hub's GOP cache in one burst, and a child that is slow
// to start, or never says Hello, must not stall the copy renditions. On its
// Hello the backlog is handed to the writer, which sends it ahead of
// everything queued later. A message beyond maxBacklog is dropped, and the
// child is then fed again only from the K of a bump after its Hello, so its
// input never has a gap.
//
// Afterwards toChild waits while the child's queue is full.
func (s *run) toChild(m wire.Message) bool {
	c := s.c
	if c == nil {
		s.droppedFrame(m)
		return false
	}
	if !c.handed {
		if len(c.backlog) >= maxBacklog {
			s.r.overflowBeforeHello()
			return false
		}
		c.backlog = append(c.backlog, m)
		return true
	}
	select {
	case c.in <- m:
		s.queued(c)
		return true
	default:
	}
	// The queue is full: wait, but kill a child whose writer makes no
	// progress for InputStallTimeout, so Run never stalls on it for longer.
	stall := s.t.opt.InputStallTimeout
	timer := time.NewTimer(stall)
	defer timer.Stop()
	seen := c.progress.Load()
	for {
		select {
		case c.in <- m:
			s.queued(c)
			return true
		case <-c.done:
		case <-s.ctx.Done():
		case <-timer.C:
			if p := c.progress.Load(); p != seen {
				seen = p
				timer.Reset(stall)
				continue
			}
			c.stalled = fmt.Errorf("transcode: input stalled for %v", stall)
			s.t.log.Warn("transcode: child stopped reading its input", "pid", c.p.pid, "after", stall)
			c.kill()
		}
		s.droppedFrame(m)
		return false
	}
}

// queued records the queue's high-water mark after a send to c.in.
func (s *run) queued(c *child) {
	n := len(c.in)
	s.t.update(func(st *Stats) { st.MaxQueued = max(st.MaxQueued, n) })
}

// droppedFrame counts a frame that toChild gave up on.
func (s *run) droppedFrame(m wire.Message) {
	if _, ok := m.(wire.Frame); ok {
		s.t.update(func(st *Stats) { st.DroppedBeforeKey++ })
	}
}

// tryChild is toChild without waiting: it queues m only if there is room,
// in the backlog before the child's Hello or in its queue after.
func (s *run) tryChild(m wire.Message) bool {
	c := s.c
	if c == nil {
		return false
	}
	if !c.handed {
		if len(c.backlog) >= maxBacklog {
			return false
		}
		c.backlog = append(c.backlog, m)
		return true
	}
	select {
	case c.in <- m:
		s.queued(c)
		return true
	default:
		return false
	}
}

// handOver gives the child's pre-Hello backlog to its writer, once. From
// then on, messages go to its queue.
func (c *child) handOver() {
	if c.handed {
		return
	}
	c.handed = true
	c.backlogCh <- c.backlog
	c.backlog = nil
}

func (t *Transcoder) newRouter(s *run) *router {
	return &router{
		childVideo: t.childVideo,
		copyVideo:  len(t.copyVideo) > 0,
		childAudio: !t.opt.Audio.Copy,
		copyAudio:  t.opt.Audio.Copy,
		mainVideo:  -1,
		toChild:    s.toChild,
		tryChild:   s.tryChild,
		toCopy:     t.toCopy,
		setAnchor:  t.anchors.set,
		srcAnchor:  t.src.Anchor,
		now:        t.opt.Now,
		onEpoch: func(e uint32, bump bool) {
			if bump {
				t.bumps.Add(1)
			}
			t.emit(Event{Kind: EventEpoch, Epoch: e})
		},
		onLoss: func() {
			t.update(func(st *Stats) { st.Losses++ })
			t.emit(Event{Kind: EventLoss})
		},
		dropped: func(n uint64) {
			t.update(func(st *Stats) { st.DroppedBeforeKey += n })
		},
		warn: t.log.Warn,
	}
}

// toCopy hands a copied frame to its renditions: video to those that copy
// video, audio to every rendition.
func (t *Transcoder) toCopy(f media.Frame) {
	if f.Kind() == media.KindVideo {
		for _, i := range t.copyVideo {
			t.mergers[i].push(f)
		}
		return
	}
	for _, m := range t.mergers {
		m.push(f)
	}
}

// fromChild hands a child output to its renditions: a rung's video to that
// rendition, audio (rung -1) to every rendition.
func (t *Transcoder) fromChild(v wire.Frame) {
	t.childOut.Add(1)
	switch {
	case v.Rung == -1:
		for _, m := range t.mergers {
			m.push(v.F)
		}
	case v.Rung >= 0 && v.Rung < len(t.encodeIdx):
		t.mergers[t.encodeIdx[v.Rung]].push(v.F)
	default:
		t.log.Warn("transcode: child output for an unknown rung", "rung", v.Rung)
	}
}

// proc is a started child process and the host's end of its socketpair.
type proc struct {
	cmd  *exec.Cmd
	conn net.Conn
	pid  int
}

// child is one transcoder subprocess and its goroutines.
type child struct {
	p       *proc
	w       *wire.Writer
	in      chan wire.Message
	quit    chan struct{} // closed to stop at once
	helloOK chan struct{} // closed by the reader on a valid Hello
	hello   chan struct{} // receives once, on a valid Hello
	exited  chan struct{} // closed once the process is reaped
	done    chan struct{} // closed after exited, once the reader has ended
	started time.Time
	stopped sync.Once

	// Owned by the Run goroutine: the frames queued before Run took the
	// Hello, and whether they were handed to the writer through backlogCh.
	backlog   []wire.Message
	handed    bool
	backlogCh chan []wire.Message

	reapMu sync.Mutex
	reaped bool // the waiter has reaped the process: its pgid may be reused

	// Written before done is closed.
	waitErr error
	fatal   error
	cause   error // why the reader gave up on the child, if it did

	// Owned by the Run goroutine: set when Run killed the child for not
	// taking its input.
	stalled error

	// progress counts the writer's successful writes; Run reads it to tell
	// a slow child from one that stopped reading.
	progress atomic.Uint64
}

// startChild spawns the child and starts its reader, writer and waiter.
func (t *Transcoder) startChild(wg *sync.WaitGroup) (*child, error) {
	env := append(os.Environ(), t.opt.Env...)
	lw := &logWriter{log: t.log, ready: make(chan struct{})}
	p, err := startProc(t.opt.Binary, t.opt.Args, env, lw)
	if err != nil {
		return nil, err
	}
	// exec copies stderr only once Start has succeeded; Write waits for the
	// pid.
	lw.pid = p.pid
	close(lw.ready)
	c := &child{
		p:         p,
		w:         wire.NewWriter(p.conn),
		in:        make(chan wire.Message, childQueue),
		quit:      make(chan struct{}),
		helloOK:   make(chan struct{}),
		hello:     make(chan struct{}, 1),
		backlogCh: make(chan []wire.Message, 1),
		exited:    make(chan struct{}),
		done:      make(chan struct{}),
		started:   t.opt.Now(),
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
// up on the child: no Hello within HelloTimeout, or input that does not
// decode. An end of the socket (EOF, or ECONNRESET when the child exited
// with input unread) is not an error: the exit is the event.
func (t *Transcoder) readChild(c *child) error {
	r := wire.NewReader(bufio.NewReaderSize(c.p.conn, 64<<10))
	c.reapMu.Lock()
	if !c.reaped {
		_ = c.p.conn.SetReadDeadline(time.Now().Add(t.opt.HelloTimeout))
	}
	c.reapMu.Unlock()
	m, err := r.Read()
	if err != nil {
		if socketEnded(err) || c.isExited() {
			return nil
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("transcode: hello: none within %v", t.opt.HelloTimeout)
		}
		return fmt.Errorf("transcode: hello: %w", err)
	}
	h, ok := m.(wire.Hello)
	switch {
	case !ok:
		return fmt.Errorf("transcode: hello: first message is %T", m)
	case h.Version != wire.Version:
		c.fatal = fmt.Errorf("transcode: hello: child speaks wire version %d, want %d", h.Version, wire.Version)
		c.kill()
		return nil
	}
	c.reapMu.Lock()
	if !c.reaped {
		// The waiter sets its own deadline once the child is reaped.
		_ = c.p.conn.SetReadDeadline(time.Time{})
	}
	c.reapMu.Unlock()
	close(c.helloOK)
	c.hello <- struct{}{}
	for {
		m, err := r.Read()
		if err != nil {
			if socketEnded(err) || c.isExited() {
				return nil
			}
			return fmt.Errorf("transcode: read: %w", err)
		}
		switch v := m.(type) {
		case wire.Frame:
			t.fromChild(v)
		case wire.Error:
			t.log.Warn("transcode: child error", "pid", c.p.pid, "rung", v.Rung, "fatal", v.Fatal, "msg", v.Message)
		default:
		}
	}
}

// socketEnded reports a read error that only means the child closed its
// end.
func socketEnded(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// isExited reports whether the child has been reaped.
func (c *child) isExited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// kill kills the child's process group, unless the child has already been
// reaped, after which its pgid may belong to another group.
func (c *child) kill() {
	c.reapMu.Lock()
	defer c.reapMu.Unlock()
	if !c.reaped {
		c.p.killGroup()
	}
}

// writeChild sends Configure after the child's Hello, then the queued
// messages, then Stop.
func (t *Transcoder) writeChild(c *child) {
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
	// The backlog, handed over by Run on the Hello (or on stop), goes ahead
	// of everything in c.in.
	select {
	case backlog := <-c.backlogCh:
		for _, m := range backlog {
			if err := c.w.Write(m); err != nil {
				c.kill()
				return
			}
			c.progress.Add(1)
		}
	case <-c.quit:
		_ = c.w.Write(wire.Stop{})
		return
	case <-c.exited:
		return
	}
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				_ = c.w.Write(wire.Stop{})
				return
			}
			if err := c.w.Write(m); err != nil {
				c.kill()
				return
			}
			c.progress.Add(1)
		case <-c.quit:
			_ = c.w.Write(wire.Stop{})
			return
		case <-c.exited:
			return
		}
	}
}

// wait reaps the process, then lets the reader drain what the child wrote.
func (c *child) wait(readDone <-chan struct{}) {
	err := c.p.cmd.Wait()
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

// stop asks the child to stop: after the queued frames when graceful, at
// once otherwise. After stopGrace it kills the child's process group. It
// returns once the child is reaped and its reader has ended.
func (c *child) stop(graceful bool) {
	c.handOver() // a child that says Hello late still gets its backlog
	c.stopped.Do(func() {
		_ = c.p.conn.SetWriteDeadline(time.Now().Add(stopGrace))
		if graceful {
			close(c.in)
		} else {
			close(c.quit)
		}
	})
	timer := time.NewTimer(stopGrace)
	defer timer.Stop()
	select {
	case <-c.done:
		return
	case <-timer.C:
	}
	c.kill()
	<-c.done
}

// logWriter logs the child's stderr, one record per line. exec calls Write
// from its own goroutine, which may start before startChild knows the pid,
// so Write waits for ready, closed once pid is set.
type logWriter struct {
	log   *slog.Logger
	ready chan struct{}
	pid   int
	buf   []byte
}

func (w *logWriter) Write(b []byte) (int, error) {
	<-w.ready
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.log.Warn("transcode: child stderr", "pid", w.pid, "line", string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 4096 {
		w.log.Warn("transcode: child stderr", "pid", w.pid, "line", string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(b), nil
}
