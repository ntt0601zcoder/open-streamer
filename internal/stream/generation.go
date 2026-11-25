package stream

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// packQueue is a packager's subscription queue; the ABR packager's holds
// one per rendition and its own.
const packQueue = 1024

// errEnded is the failure of a part that returned without an error while
// its generation was live.
var errEnded = errors.New("ended")

// packSpec is one packager a generation runs while HLS or DASH is on.
type packSpec struct {
	name  string // the served name
	hub   *hub.Hub
	abr   bool
	queue int
}

// packRun is a running packager.
type packRun struct {
	p      *live.Packager
	sub    *hub.Sub
	cancel context.CancelFunc
	done   chan struct{}
}

// pushRun is a running pusher.
type pushRun struct {
	id     string // pushID of its URL
	p      *rtmppush.Pusher
	cancel context.CancelFunc
	done   chan struct{}
}

// pushID is the label of a push target: the first 8 hex digits of
// redact.Tag of its URL. The tag is keyed with a key that is random per
// process, since the URL holds a stream key and the label is in the
// metrics and the status: it shows nothing of the URL and no guess can be
// tested against it. It is the same for the same URL within one process,
// and changes when the server restarts.
func pushID(url string) string {
	return hex.EncodeToString(redact.Tag([]byte(url))[:4])
}

// generation is one build of a stream: the source hub, the pipeline, the
// transcoder or thumbnailer, and the attached outputs. Everything but the
// outputs is fixed once build returns.
type generation struct {
	n       uint64
	started time.Time // by the supervisor's clock, for the 60 s rule
	since   time.Time // by the wall clock, for Status
	ctx     context.Context
	cancel  context.CancelFunc

	source *hub.Hub
	pipe   *pipeline.Pipeline
	tr     *transcode.Transcoder  // nil without a ladder
	nailer *transcode.Thumbnailer // nil unless the spec has a thumbnail and no child encodes
	shot   func() (picture.Shot, time.Time, bool)
	labels []string // the ladder's labels, in order

	single    map[string]*hub.Hub // served name → hub; never changed
	hubs      []*hub.Hub          // the distinct hubs of single
	packSpecs []packSpec
	active    atomic.Bool // an input has been active

	failed   chan struct{} // closed when a part ended while the generation was live
	failOnce sync.Once
	failPfx  string // set before failed is closed
	failErr  error

	rmu      sync.Mutex
	runners  []chan struct{} // one per run, closed when its function has returned
	pipeDone <-chan struct{}

	// The outputs: Run goroutine only.
	packs  map[string]*packRun         // by served name
	pushes map[string]*pushRun         // by URL
	ready  map[string]*rtmppush.Pusher // built with the generation, started at its first attach
}

func newGeneration(ctx context.Context, n uint64, started time.Time) *generation {
	g := &generation{
		n: n, started: started, since: time.Now(),
		single: map[string]*hub.Hub{},
		failed: make(chan struct{}),
		packs:  map[string]*packRun{},
		pushes: map[string]*pushRun{},
		ready:  map[string]*rtmppush.Pusher{},
	}
	g.ctx, g.cancel = context.WithCancel(ctx)
	return g
}

// run starts f, one long-lived part of the generation, under the name it
// fails by. When f returns while the generation's context is live, whatever
// it returns, the generation has failed with "<name>: <err>": each part is
// meant to run for the generation's life. A return after the cancel is the
// part stopping as asked. The returned channel is closed when f has
// returned; teardown waits for every one.
func (g *generation) run(name string, f func(context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	g.rmu.Lock()
	g.runners = append(g.runners, done)
	g.rmu.Unlock()
	go func() {
		defer close(done)
		err := f(g.ctx)
		if g.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errEnded
		}
		g.failOnce.Do(func() {
			g.failPfx, g.failErr = name+": ", err
			close(g.failed)
		})
	}()
	return done
}

// failure returns what closed g.failed. Call it after failed is closed.
func (g *generation) failure() (prefix string, err error) { return g.failPfx, g.failErr }

// waitRunners waits for every function started by run.
func (g *generation) waitRunners() {
	g.rmu.Lock()
	runners := slices.Clone(g.runners)
	g.rmu.Unlock()
	for _, done := range runners {
		<-done
	}
}

// build makes the next generation from s.spec and starts its long-lived
// parts. On an error nothing is left running. It fails when something the
// spec names is no longer there (a watermark's asset is read again for
// every generation); any error of the constructors is handled the same way.
func (s *Supervisor) build(ctx context.Context) (*generation, error) {
	sp := s.spec
	g := newGeneration(ctx, s.gens+1, s.now())
	g.source = hub.New(hub.Options{})
	abandon := func(err error) (*generation, error) {
		g.cancel()
		g.source.Close()
		if g.tr != nil {
			closeHubs(g.tr)
		}
		return nil, err
	}
	layout, err := layoutOf(sp)
	if err != nil {
		return abandon(fmt.Errorf("layout: %w", err))
	}
	// alive reports whether an event of this generation still counts: a
	// part that is being stopped reports its child's end too.
	alive := func() bool { return g.ctx.Err() == nil }

	// The served hubs (R-D3).
	if sp.Transcode == nil {
		g.single[sp.Name] = g.source
		g.hubs = []*hub.Hub{g.source}
		g.packSpecs = []packSpec{{name: sp.Name, hub: g.source, queue: packQueue}}
	} else {
		opt, err := ladderOf(sp, s.env)
		if err != nil {
			return abandon(fmt.Errorf("transcode: %w", err))
		}
		opt.OnEvent = func(ev transcode.Event) {
			if ev.Kind == transcode.EventExited && ev.Err != nil && alive() {
				s.fail("transcoder: ", ev.Err)
			}
		}
		if g.tr, err = transcode.New(g.source, opt); err != nil {
			return abandon(err)
		}
		hubs := g.tr.Hubs()
		g.hubs = hubs
		g.single[sp.Name] = hubs[0]
		g.packSpecs = []packSpec{{name: sp.Name, hub: g.tr.Presentation(), abr: true, queue: packQueue * (len(hubs) + 1)}}
		for i, r := range sp.Transcode.Renditions {
			name := sp.Name + "_" + r.Label
			g.labels = append(g.labels, r.Label)
			g.single[name] = hubs[i]
			g.packSpecs = append(g.packSpecs, packSpec{name: name, hub: hubs[i], queue: packQueue})
		}
		if opt.Thumbnail != nil {
			g.shot = g.tr.Shot
		}
	}
	if sp.Thumbnail != nil && g.shot == nil {
		opt := thumbnailerOf(sp, s.env)
		opt.OnEvent = func(ev transcode.Event) {
			if ev.Kind == transcode.EventExited && ev.Err != nil && alive() {
				s.fail("thumbnailer: ", ev.Err)
			}
		}
		if g.nailer, err = transcode.NewThumbnailer(g.source, opt); err != nil {
			return abandon(err)
		}
		g.shot = g.nailer.Shot
	}
	for _, p := range sp.Outputs.Push {
		pusher, err := rtmppush.New(g.single[sp.Name], p.URL, rtmppush.Options{})
		if err != nil {
			return abandon(fmt.Errorf("push %s: %w", pushID(p.URL), err))
		}
		g.ready[p.URL] = pusher
	}
	popt := pipeline.Options{
		Inputs:   inputsOf(sp),
		Env:      ingest.Env{Lookup: s.env.Lookup},
		Open:     s.env.Open,
		Layout:   layout,
		EndOnEOF: false,
		OnEvent: func(ev pipeline.Event) {
			switch ev.Kind {
			case pipeline.EventActive:
				g.active.Store(true)
			case pipeline.EventFailed:
				switch {
				case !alive():
				case errors.Is(ev.Err, io.EOF):
					// With EndOnEOF false, an input that reaches its end is
					// restarted by the pipeline: the stream working as
					// ruled (R-D5), counted and not shown as an error.
					s.eofs.Add(1)
				case ev.Err != nil:
					s.fail(fmt.Sprintf("input %d: ", ev.Input), ev.Err)
					s.log.Warn("stream: input failed", "input", ev.Input, "err", ev.Err)
				}
			case pipeline.EventDiscontinuity:
			}
		},
	}
	if s.env.Open != nil {
		// A test's reader is opened whatever its URL says.
		popt.Parse = func(url string) (ingest.Spec, error) {
			parsed, err := ingest.Parse(url)
			if err != nil {
				return ingest.Spec{}, nil //nolint:nilerr // Env.Open takes any URL
			}
			return parsed, nil
		}
	}
	g.pipe = pipeline.New(g.source, popt)

	// Nothing can fail from here on.
	s.gens = g.n
	if g.tr != nil {
		g.run("transcoder", g.tr.Run)
	}
	if g.nailer != nil {
		g.run("thumbnailer", g.nailer.Run)
	}
	g.pipeDone = g.run("pipeline", g.pipe.Run)
	s.mu.Lock()
	s.cur, s.pushes, s.backoff = g, nil, false
	s.mu.Unlock()
	return g, nil
}

// off returns the subscriber-name prefixes of the session outputs disabled
// in o, for hub.Refuse.
//
// Every prefix it returns must be non-empty: hub.Refuse("") matches every
// subscriber name, and would end the transcoder's, the packagers' and every
// player's subscription at once. Each ends in a space, so "rtmp " does not
// match "rtmppush …". TestOffPrefixesNonEmpty holds both.
func off(o spec.Outputs) []string {
	var out []string
	if !o.HTTPTS {
		out = append(out, "httpts ")
	}
	if !o.RTMP {
		out = append(out, "rtmp ")
	}
	if !o.RTSP {
		out = append(out, "rtsp ")
	}
	if !o.SRT {
		out = append(out, "srt ")
	}
	return out
}

// both returns the prefixes in a and in b.
func both(a, b []string) []string {
	var out []string
	for _, p := range a {
		if slices.Contains(b, p) {
			out = append(out, p)
		}
	}
	return out
}

func hasPush(o spec.Outputs, url string) bool {
	return slices.ContainsFunc(o.Push, func(p spec.Push) bool { return p.URL == url })
}

// reattach takes g's outputs from old to new with the smallest set of calls
// (R-D6), and publishes the View. The order matters; see each step.
//
// A field added to spec.Outputs must be handled here (and in off, when it is
// a session output): TestReattachCoversEveryOutput fails until it is.
func (s *Supervisor) reattach(g *generation, old, new spec.Outputs) {
	// 1. The packagers serve HLS and DASH alike: they run while either is
	// on, and an edit that keeps either touches none.
	want, have := new.HLS || new.DASH, old.HLS || old.DASH
	switch {
	case want && !have:
		s.startPackagers(g)
	case !want && have:
		g.stopPackagers()
	}
	// 2. The pushes, keyed by URL.
	for _, p := range old.Push {
		if !hasPush(new, p.URL) {
			g.stopPush(p.URL)
		}
	}
	for _, p := range new.Push {
		if hasPush(old, p.URL) {
			continue
		}
		if err := g.startPush(p.URL, g.single[s.name]); err != nil {
			// Check and build accepted this URL, so this does not happen;
			// it is reported rather than assumed away.
			s.fail("build: ", fmt.Errorf("push %s: %w", pushID(p.URL), err))
			s.log.Error("stream: push not started", "push", pushID(p.URL), "err", err)
		}
	}
	// 3. Relax only: what was refused and still is. An output being
	// re-enabled must be subscribable before the view announces it, or a
	// player that connects at once would be handed a closed subscription.
	// At the first attach old is the zero Outputs, whose off is every
	// prefix, so this refuses exactly off(new) before anything is announced.
	now, relaxed := off(new), both(off(old), off(new))
	for _, h := range g.hubs {
		h.Refuse(relaxed...)
	}
	// 4. Publish: the sessions of a re-enabled output are now found, and new
	// sessions of a disabled one find nothing.
	s.publish(g, new)
	// 5. End the sessions of the outputs just disabled, and only those.
	if len(now) != len(relaxed) {
		for _, h := range g.hubs {
			h.Refuse(now...)
		}
	}
}

// publish swaps in the View of g with outputs o, and the pushers Status
// reads, in o's order.
func (s *Supervisor) publish(g *generation, o spec.Outputs) {
	v := &View{Gen: g.n, Outputs: o, Single: g.single, Packs: make(map[string]*live.Packager, len(g.packs)), Source: g.source}
	v.Outputs.Push = slices.Clone(o.Push)
	for name, r := range g.packs {
		v.Packs[name] = r.p
	}
	pushes := make([]*pushRun, 0, len(o.Push))
	for _, p := range o.Push {
		if r := g.pushes[p.URL]; r != nil {
			pushes = append(pushes, r)
		}
	}
	s.mu.Lock()
	s.pushes = pushes
	s.mu.Unlock()
	s.view.Store(v)
	if s.published != nil {
		s.published(v)
	}
}

// startPackagers starts every packager of g, each subscribed under
// "packager <served name>".
func (s *Supervisor) startPackagers(g *generation) {
	for _, ps := range g.packSpecs {
		opt := live.Options{}
		if ps.abr {
			opt = live.Options{ABR: true, Logger: s.log}
		}
		ctx, cancel := context.WithCancel(g.ctx)
		r := &packRun{
			p:      live.NewPackager(ps.hub, opt),
			sub:    ps.hub.Subscribe("packager "+ps.name, ps.queue),
			cancel: cancel,
			done:   make(chan struct{}),
		}
		g.packs[ps.name] = r
		go func() {
			defer close(r.done)
			r.p.Run(ctx, r.sub)
		}()
	}
}

// stopPackagers stops every packager of g and waits for it.
func (g *generation) stopPackagers() {
	for _, r := range g.packs {
		r.cancel()
		r.sub.Close()
	}
	for _, r := range g.packs {
		<-r.done
	}
	clear(g.packs)
}

// startPush starts the pusher of url on h.
func (g *generation) startPush(url string, h *hub.Hub) error {
	p := g.ready[url]
	delete(g.ready, url)
	if p == nil {
		var err error
		if p, err = rtmppush.New(h, url, rtmppush.Options{}); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(g.ctx)
	r := &pushRun{id: pushID(url), p: p, cancel: cancel, done: make(chan struct{})}
	g.pushes[url] = r
	go func() {
		defer close(r.done)
		_ = r.p.Run(ctx) // it ends with ctx or with the hub; its failures are in its Stats
	}()
	return nil
}

// stopPush stops the pusher of url and waits for it.
func (g *generation) stopPush(url string) {
	r := g.pushes[url]
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	delete(g.pushes, url)
}

// teardown stops g and waits for it, in osdev's shutdown order with the
// pipeline's wait made explicit. The empty View is published first, so
// nothing new is handed the generation's hubs. The pipeline is waited for
// first: its return is what closes the input readers, so the listen sockets
// and RTMP publish routes of this generation are free before the next one,
// or the next supervisor of this name, builds. The transcoder and the
// thumbnailer follow (they close the rendition and presentation hubs), then
// the packagers and the pushers.
func (s *Supervisor) teardown(g *generation, reason stop) {
	s.view.Store(&View{})
	s.release(g, reason)
	g.cancel()
	g.source.Close()
	<-g.pipeDone
	g.waitRunners()
	g.stopPackagers()
	for url := range g.pushes {
		g.stopPush(url)
	}
}
