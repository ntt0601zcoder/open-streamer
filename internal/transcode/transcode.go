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
// # Presentation
//
// With Options.Presentation, one more hub carries the whole ladder for an
// ABR packager (rulings R-B1, R-B3, R-B7): rendition i's video as track
// PresentationTrack(i) and each source audio track once, as
// PresentationAudioTrack. Its merger gets the same frames as the rendition
// mergers, relabelled, and the same anchors, so its epochs, anchors, PTS and
// DTS are the rendition hubs'. It orders them behind a keyframe barrier that
// the router announces at every source main-video keyframe K + Delay: every
// frame below K, of every track, goes out before the rungs' IDRs at K, and
// those go out together, rung 0's first, so a packager can cut every
// variant at the same keyframe and a late subscriber's GOP cache, anchored
// on track 0, holds every rung's IDR. A rung is waited for as long as the
// encoder's delay asks, between MergeHold and four times MergeHold (see
// merger); one later than that, or without an IDR at K, is not.
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
	"slices"
	"strings"
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
	defaultInputStall   = 10 * time.Second

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
// at Height, which must be even (width keeps the aspect ratio). BFrames -1 means none, 0 means
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
	// Presentation adds the presentation hub (see Presentation).
	Presentation bool

	Queue int // source subscription queue; 0 means 1024

	RespawnMin   time.Duration // 0 means 250 ms
	RespawnMax   time.Duration // 0 means 5 s
	HealthyAfter time.Duration // 0 means 30 s
	// MergeHold is how long a merger holds a frame for a quiet track; 0
	// means 2 s. The presentation's merger holds up to four times as long
	// when the encoded rungs trail further (see merger).
	MergeHold    time.Duration
	HelloTimeout time.Duration // a child without Hello by then is killed; 0 means 10 s
	// InputStallTimeout bounds how long Run waits to queue a frame for a
	// child that consumes none of its input. While a send to the child is
	// blocked, a child that reports no progress (a rising Stats.In, or any
	// output Frame) for that long is killed ("input stalled"); 0 means 10 s.
	// The child reports Stats every second, so it must exceed that.
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
// rendition: the frames written to its hub. PresentationOut and
// PresentationDropped count the presentation hub's merger alone: the frames
// it wrote, and those it dropped (they are not in FramesOut or
// MergeDropped). MaxQueued is the high-water
// mark of the child's input channel (at most 256); it excludes the
// pre-Hello backlog, which can hold up to 1500 frames. PID is the running
// child's, or 0. DroppedBeforeKey also counts the frames given up on
// because the child exited, stalled, or Run ended while they waited.
// StderrSuppressed counts the children's stderr lines not logged because
// they came faster than 10 a second. Oversize counts the frames dropped on
// their way to a child because the wire protocol cannot carry them
// (wire.ErrLimit). Child holds the media counters the children reported.
type Stats struct {
	Spawns, Exits, Losses, DroppedBeforeKey, MergeDropped uint64
	FramesIn, StderrSuppressed, Oversize                  uint64
	PresentationOut, PresentationDropped                  uint64
	FramesOut                                             []uint64
	MaxQueued                                             int
	PID                                                   int
	Child                                                 ChildStats
}

// ChildStats are the media counters the children report in their Stats
// messages, summed over every child of the run (each child counts from
// zero). DroppedTracks counts video tracks other than the first that a
// child dropped, Reanchors forward audio re-anchors, AudioDropped input
// audio samples dropped for backward drift, and AudioCorrupt and
// VideoCorrupt the frames the child's decoders rejected and skipped. A
// child's counts after its last Stats are not included.
type ChildStats struct {
	DroppedTracks, Reanchors, AudioDropped, AudioCorrupt, VideoCorrupt uint64
}

// max returns the per-counter maximum of s and o.
func (s ChildStats) max(o ChildStats) ChildStats {
	return ChildStats{
		DroppedTracks: max(s.DroppedTracks, o.DroppedTracks),
		Reanchors:     max(s.Reanchors, o.Reanchors),
		AudioDropped:  max(s.AudioDropped, o.AudioDropped),
		AudioCorrupt:  max(s.AudioCorrupt, o.AudioCorrupt),
		VideoCorrupt:  max(s.VideoCorrupt, o.VideoCorrupt),
	}
}

// add adds the rise from prev to cur, per counter, to s. Counters never
// fall within one child; one that does adds nothing, and the reader keeps
// the per-counter maximum as prev, so the counter adds again only once it
// is above its old high.
func (s *ChildStats) add(prev, cur ChildStats) {
	rise := func(p, c uint64) uint64 {
		if c < p {
			return 0
		}
		return c - p
	}
	s.DroppedTracks += rise(prev.DroppedTracks, cur.DroppedTracks)
	s.Reanchors += rise(prev.Reanchors, cur.Reanchors)
	s.AudioDropped += rise(prev.AudioDropped, cur.AudioDropped)
	s.AudioCorrupt += rise(prev.AudioCorrupt, cur.AudioCorrupt)
	s.VideoCorrupt += rise(prev.VideoCorrupt, cur.VideoCorrupt)
}

// Transcoder turns one source hub into one hub per rendition.
type Transcoder struct {
	src     *hub.Hub
	opt     Options
	log     *slog.Logger
	hubs    []*hub.Hub
	mergers []*merger
	pres    *merger // the presentation hub's, or nil
	anchors anchorStore

	needChild  bool
	encodeIdx  []int // rung index → rendition index
	copyVideo  []int // renditions that copy video
	configure  wire.Configure
	childVideo bool

	// writeMsg writes m to c; nil means c.w.Write. Tests replace it.
	writeMsg func(c *child, m wire.Message) error

	running  atomic.Bool
	childOut atomic.Uint64 // frames received from children
	bumps    atomic.Uint64 // bumps that took effect

	mu    sync.Mutex
	stats Stats
}

// New validates opt and returns a Transcoder whose rendition hubs are open.
// At most wire.MaxRungs renditions may be encoded.
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
		case v.Height <= 0 || v.Height%2 != 0:
			// YUV 4:2:0 needs an even height: the child would fail at
			// Configure and respawn forever.
			return nil, fmt.Errorf("transcode: new: rendition %q: height %d is not positive and even", r.Name, v.Height)
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
	if n := len(t.encodeIdx); n > wire.MaxRungs {
		// The child's Configure could not carry them: it would never start.
		return nil, fmt.Errorf("transcode: new: %d encoded renditions, at most %d", n, wire.MaxRungs)
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
	if opt.Presentation {
		h := hub.New(hub.Options{GOPCacheFrames: presentationCacheFrames(len(opt.Renditions))})
		t.pres = newPresentationMerger(h, opt.MergeHold, opt.Now, t.anchors.get)
	}
	return t, nil
}

// presentationCacheFrames is the presentation hub's GOP cache for n
// renditions (ruling R-B7): every rung's GOP and the audio.
func presentationCacheFrames(n int) int { return 1500 * (n + 1) }

// PresentationTrack is the presentation's track for the video of
// rendition i, in Options.Renditions order.
func PresentationTrack(rendition int) int { return rendition }

// PresentationAudioTrack is the presentation's track for source audio
// track srcTrack, with renditions renditions.
func PresentationAudioTrack(renditions, srcTrack int) int { return renditions + srcTrack }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Hubs returns the rendition hubs, in Options.Renditions order. Run closes
// them when it returns.
func (t *Transcoder) Hubs() []*hub.Hub { return append([]*hub.Hub(nil), t.hubs...) }

// Presentation returns the presentation hub, or nil without
// Options.Presentation. Run closes it when it returns.
func (t *Transcoder) Presentation() *hub.Hub {
	if t.pres == nil {
		return nil
	}
	return t.pres.h
}

// everyMerger returns the rendition mergers and the presentation's.
func (t *Transcoder) everyMerger() []*merger {
	if t.pres == nil {
		return t.mergers
	}
	return append(slices.Clip(t.mergers), t.pres)
}

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
	if t.pres != nil {
		s.PresentationOut, s.PresentationDropped = t.pres.counts()
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
	backoff backoff
}

// Run transcodes until the source hub closes (it returns nil) or ctx ends
// (it returns ctx.Err()). It stops the child, flushes the mergers and closes
// the rendition hubs and the presentation hub before returning. A child that announces another wire
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
	for _, m := range t.everyMerger() {
		mwg.Add(1)
		go func() {
			defer mwg.Done()
			m.run(mctx, t.opt.After)
		}()
	}

	s := &run{t: t, ctx: ctx}
	s.backoff = backoff{min: t.opt.RespawnMin, max: t.opt.RespawnMax, healthy: t.opt.HealthyAfter}
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
	for _, m := range t.everyMerger() {
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
	s.respawn = s.t.opt.After(s.backoff.next(uptime))
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
	// The queue is full: wait, but kill a child that reports consuming
	// nothing for InputStallTimeout, so Run never stalls on it for longer.
	// The host's own completed writes are no signal: a blocked socket writer
	// only wakes once the child has drained most of the buffer, seconds
	// later for a slow child.
	stall := s.t.opt.InputStallTimeout
	since := time.Now()
	timer := time.NewTimer(stall)
	defer timer.Stop()
	for {
		select {
		case c.in <- m:
			s.queued(c)
			return true
		case <-c.done:
		case <-s.ctx.Done():
		case now := <-timer.C:
			last := since
			if p := c.lastProgress.Load(); p > last.UnixNano() {
				last = time.Unix(0, p)
			}
			if idle := now.Sub(last); idle < stall {
				timer.Reset(stall - idle)
				continue
			}
			c.stalled = fmt.Errorf("transcode: input stalled for %v", stall)
			s.t.log.Warn("transcode: child stopped consuming its input", "pid", c.p.pid, "after", stall)
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
	var barrier func(uint32, int64)
	if t.pres != nil {
		barrier = t.pres.barrier
	}
	return &router{
		childVideo: t.childVideo,
		copyVideo:  len(t.copyVideo) > 0,
		childAudio: !t.opt.Audio.Copy,
		copyAudio:  t.opt.Audio.Copy,
		mainVideo:  -1,
		toChild:    s.toChild,
		tryChild:   s.tryChild,
		toCopy:     t.toCopy,
		expectVideo: func(epoch uint32, track int) {
			for _, i := range t.encodeIdx {
				t.mergers[i].expect(epoch, track)
				if t.pres != nil {
					t.pres.expect(epoch, PresentationTrack(i))
				}
			}
		},
		barrier:   barrier,
		setAnchor: t.anchors.set,
		srcAnchor: t.src.Anchor,
		now:       t.opt.Now,
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
// video, audio to every rendition; and to the presentation, relabelled.
func (t *Transcoder) toCopy(f media.Frame) {
	if f.Kind() == media.KindVideo {
		for _, i := range t.copyVideo {
			t.mergers[i].push(f)
			t.toPresentationVideo(i, f)
		}
		return
	}
	for _, m := range t.mergers {
		m.push(f)
	}
	t.toPresentationAudio(f)
}

// toPresentationVideo hands rendition i's video frame f to the
// presentation, as track PresentationTrack(i).
func (t *Transcoder) toPresentationVideo(i int, f media.Frame) {
	if t.pres == nil {
		return
	}
	f.Track = PresentationTrack(i)
	t.pres.push(f)
}

// toPresentationAudio hands audio frame f to the presentation, as track
// PresentationAudioTrack of its source track.
func (t *Transcoder) toPresentationAudio(f media.Frame) {
	if t.pres == nil {
		return
	}
	f.Track = PresentationAudioTrack(len(t.mergers), f.Track)
	t.pres.push(f)
}

// fromChild hands an output of child c to its renditions: a rung's video to
// that rendition, audio (rung -1) to every rendition; and to the
// presentation, relabelled. An output for a rung
// that does not exist is dropped, and logged once per child. Called from
// c's reader goroutine.
func (t *Transcoder) fromChild(c *child, v wire.Frame) {
	t.childOut.Add(1)
	switch {
	case v.Rung == -1:
		for _, m := range t.mergers {
			m.push(v.F)
		}
		t.toPresentationAudio(v.F)
	case v.Rung >= 0 && v.Rung < len(t.encodeIdx):
		i := t.encodeIdx[v.Rung]
		t.mergers[i].push(v.F)
		t.toPresentationVideo(i, v.F)
	default:
		if !c.unknownRungLogged {
			c.unknownRungLogged = true
			t.log.Warn("transcode: child output for an unknown rung", "pid", c.p.pid, "rung", v.Rung)
		}
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
	stderr  *logWriter
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

	// Owned by the writer goroutine: an oversize frame was logged.
	oversizeLogged bool
	// Owned by the reader goroutine: an output for an unknown rung was
	// logged.
	unknownRungLogged bool

	// lastProgress is when the child last reported consuming input (a
	// rising Stats.In, or an output Frame), in Unix nanoseconds; Run reads
	// it to tell a slow child from one that stopped.
	lastProgress atomic.Int64
}

// startChild spawns the child and starts its reader, writer and waiter.
func (t *Transcoder) startChild(wg *sync.WaitGroup) (*child, error) {
	env := append(os.Environ(), t.opt.Env...)
	lw := newLogWriter(t.log, func(n uint64) {
		t.update(func(st *Stats) { st.StderrSuppressed += n })
	})
	p, err := startProc(t.opt.Binary, t.opt.Args, env, lw)
	if err != nil {
		return nil, err
	}
	// exec copies stderr only once Start has succeeded; Write waits for the
	// pid.
	lw.setPID(p.pid)
	c := &child{
		p:         p,
		stderr:    lw,
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
	c.reapMu.Lock()
	if !c.reaped {
		// The waiter sets its own deadline once the child is reaped.
		_ = c.p.conn.SetReadDeadline(time.Time{})
	}
	c.reapMu.Unlock()
	close(c.helloOK)
	c.hello <- struct{}{}
	var in uint64
	var counted ChildStats // this child's media counters, as last reported
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
			c.lastProgress.Store(time.Now().UnixNano())
			t.fromChild(c, v)
		case wire.Stats:
			if v.In > in {
				in = v.In
				c.lastProgress.Store(time.Now().UnixNano())
			}
			cur := ChildStats{
				DroppedTracks: v.DroppedTracks, Reanchors: v.Reanchors, AudioDropped: v.AudioDropped,
				AudioCorrupt: v.AudioCorrupt, VideoCorrupt: v.VideoCorrupt,
			}
			if cur != counted {
				t.update(func(st *Stats) { st.Child.add(counted, cur) })
				counted = counted.max(cur)
			}
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
			if !t.send(c, m) {
				c.kill()
				return
			}
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
			if !t.send(c, m) {
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

// send writes m to the child and reports whether the child is still
// usable. A frame the protocol cannot carry (wire.ErrLimit) is dropped and
// counted in Stats.Oversize, and the child keeps running: nothing was
// written, and a child would only be killed over a frame it never saw. Any
// other error, wire.ErrConfigTable included (a respawn starts a fresh
// table), means the child must go. Called from the writer goroutine.
func (t *Transcoder) send(c *child, m wire.Message) bool {
	var err error
	if t.writeMsg != nil {
		err = t.writeMsg(c, m)
	} else {
		err = c.w.Write(m)
	}
	if err == nil {
		return true
	}
	if errors.Is(err, wire.ErrConfigTable) {
		t.log.Warn("transcode: child's config table is full; restarting it", "pid", c.p.pid, "err", err)
		return false
	}
	if _, frame := m.(wire.Frame); !frame || !errors.Is(err, wire.ErrLimit) {
		return false
	}
	t.update(func(st *Stats) { st.Oversize++ })
	if !c.oversizeLogged {
		c.oversizeLogged = true
		t.log.Warn("transcode: frame dropped: the wire protocol cannot carry it", "pid", c.p.pid, "err", err)
	}
	return true
}

// wait reaps the process, then lets the reader drain what the child wrote.
func (c *child) wait(readDone <-chan struct{}) {
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
	waitOrKill(c.done, c.kill)
	<-c.done
}

// Child stderr rate limit: a lossy source makes libav print about 10 lines
// a second per stream.
const (
	// stderrBurst is how many stderr lines of one child are logged per
	// second; the rest are counted and summarised.
	stderrBurst = 10
	// stderrSummaryEvery is how often suppressed lines are summarised.
	stderrSummaryEvery = 10 * time.Second
	// stderrKeep is how many of the last suppressed lines are kept, and
	// logged if the child exits abnormally: a Go panic or a crash report
	// goes only to stderr.
	stderrKeep = 20
)

// logWriter logs one child's stderr, one record per line, at most
// stderrBurst lines per second. Further lines are counted (onSuppress) and
// summarised in one record every summary period, and once more by close;
// the last stderrKeep of them are kept, and close logs them when the child
// exited abnormally.
// exec calls Write from its own goroutine, which may start before
// startChild knows the pid, so Write waits for ready, closed by setPID.
type logWriter struct {
	log        *slog.Logger
	ready      chan struct{}
	pid        int
	now        func() time.Time
	every      time.Duration
	onSuppress func(n uint64)

	mu         sync.Mutex
	buf        []byte
	window     time.Time // start of the current one-second window
	inWindow   int       // lines logged in it
	suppressed uint64    // lines suppressed since the last summary
	last       []string  // the last stderrKeep suppressed lines, oldest first
	timer      *time.Timer
	closed     bool
}

func newLogWriter(log *slog.Logger, onSuppress func(n uint64)) *logWriter {
	if onSuppress == nil {
		onSuppress = func(uint64) {}
	}
	return &logWriter{
		log: log, ready: make(chan struct{}), now: time.Now,
		every: stderrSummaryEvery, onSuppress: onSuppress,
	}
}

// setPID sets the child's pid and lets Write proceed.
func (w *logWriter) setPID(pid int) {
	w.pid = pid
	close(w.ready)
}

func (w *logWriter) Write(b []byte) (int, error) {
	<-w.ready
	w.mu.Lock()
	defer w.mu.Unlock()
	var n uint64
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if !w.line(w.buf[:i]) {
			n++
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 4096 {
		if !w.line(w.buf) {
			n++
		}
		w.buf = w.buf[:0]
	}
	if n > 0 {
		w.onSuppress(n)
	}
	return len(b), nil
}

// line logs l, or counts it once the window's burst is spent, and reports
// whether it was logged. Called with w.mu held.
func (w *logWriter) line(l []byte) bool {
	now := w.now()
	if now.Sub(w.window) >= time.Second {
		w.window, w.inWindow = now, 0
	}
	if w.inWindow < stderrBurst {
		w.inWindow++
		w.log.Warn("transcode: child stderr", "pid", w.pid, "line", string(l))
		return true
	}
	w.suppressed++
	if len(w.last) == stderrKeep {
		w.last = append(w.last[:0], w.last[1:]...)
	}
	w.last = append(w.last, string(l))
	if w.timer == nil && !w.closed {
		w.timer = time.AfterFunc(w.every, w.summary)
	}
	return false
}

// summary logs the lines suppressed since the last summary.
func (w *logWriter) summary() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer = nil
	w.report()
}

// report logs the suppressed count, if any. Called with w.mu held.
func (w *logWriter) report() {
	if w.suppressed == 0 {
		return
	}
	w.log.Warn(fmt.Sprintf("transcode: child %d: suppressed %d stderr lines", w.pid, w.suppressed),
		"pid", w.pid, "suppressed", w.suppressed)
	w.suppressed = 0
}

// close handles a last line without a newline, stops the summary timer and
// logs what is still suppressed; when the child exited abnormally (a
// non-zero status or a signal), it first logs the last suppressed lines in
// one record. It is called once the child is reaped, when exec has copied
// all of its stderr.
func (w *logWriter) close(abnormal bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if len(w.buf) > 0 {
		if !w.line(w.buf) {
			w.onSuppress(1)
		}
		w.buf = nil
	}
	if abnormal && len(w.last) > 0 {
		w.log.Warn(fmt.Sprintf("transcode: child %d exited abnormally; last suppressed stderr lines", w.pid),
			"pid", w.pid, "lines", strings.Join(w.last, "\n"))
	}
	w.last = nil
	w.report()
}
