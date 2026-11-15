package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

const (
	// defaultRTMPPort is the RTMP port when the URL has none.
	defaultRTMPPort = "1935"
	// takeoverIdle is how long the current publisher must have sent nothing
	// before a new publisher of the same path takes over.
	takeoverIdle = 2 * time.Second
	// openSpan is the source time after the first coded frame that Open
	// waits at most for both kinds to be announced.
	openSpan = media.ClockRate
	// maxOpenFrames and maxOpenBytes bound the frames queued while Open
	// waits, for a publisher whose timestamps do not advance.
	maxOpenFrames = 2048
	maxOpenBytes  = 32 << 20
)

// rtmpListenPolicy is the listener policy that Open passes to the registry;
// tests lower it.
var rtmpListenPolicy listenPolicy

// errRTMPClosed is returned by Read after Close.
var errRTMPClosed = errors.New("ingest: rtmp: closed")

// rtmpListenTarget returns the listen address and the path of a listen URL;
// the port defaults to 1935.
func rtmpListenTarget(p parsed) (addr, path string) {
	port := p.u.Port()
	if port == "" {
		port = defaultRTMPPort
	}
	return net.JoinHostPort(p.u.Hostname(), port), p.u.Path
}

// rtmpSession is one accepted publisher.
type rtmpSession struct {
	conn    *rtmp.ServerConn
	offered time.Time // when offer installed it; real time, like LastInbound
	flv     *flvFrames
	// superseded is set when a newer publisher took over; the reader then
	// stops reading this one.
	superseded atomic.Bool
}

// rtmpListen is an RTMP listen input: the sink of one path on a shared TCP
// listener (see rtmpRegistry). It has at most one publisher. A new one is
// accepted when there is none, or when the current one has received no byte
// for 2 s (a half-open peer): it takes over and the old session is closed.
// While the current one is sending, a new one is rejected with the same
// BadName as an unknown path.
//
// When a publisher ends or is taken over, Read returns its held video frames
// (flvFrames.flush), then ErrDiscontinuity before the next publisher's first
// frame. Each publisher gets a fresh flvFrames over the shared ConfigCache.
// A clean FCUnpublish ends the publisher like a disconnect: Read then waits
// for the next one, and the pipeline fails over after its stall limit, as
// with an SRT listener.
//
// Only the goroutine calling Read (Open's before it returns) reads a
// session and pushes into flvFrames, so the ConfigCache is never used
// concurrently, a takeover included.
type rtmpListen struct {
	spec       Spec
	now        func() time.Time
	unregister func()
	cache      media.ConfigCache
	wake       chan struct{} // a publisher was offered; capacity 1
	done       chan struct{} // closed by Close

	// The reading goroutine's own state.
	sess   *rtmpSession // being read; nil while waiting for a publisher
	opened bool         // Open has returned: a new publisher is a break
	queue  []Frame      // ready for Read, in order
	disc   bool         // Read returns ErrDiscontinuity once the queue is empty

	mu       sync.Mutex // guards the fields below
	closed   bool
	cur      *rtmpSession // the latest publisher offered and not ended
	active   *rtmpSession // the session the reader reads, for Kinds and Stats
	video    bool         // kinds announced by earlier publishers since Open
	audio    bool
	doneStat map[string]uint64 // counters of finished publishers

	// listener is the shared TCP listener, for its counters.
	listener              *rtmpListener
	closing               atomic.Bool // Close has begun
	publishers, takeovers atomic.Uint64
}

func openRTMPListen(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	addr, path := rtmpListenTarget(p)
	l := &rtmpListen{
		spec: p.spec, now: env.now(),
		wake: make(chan struct{}, 1), done: make(chan struct{}), doneStat: map[string]uint64{},
	}
	//nolint:contextcheck // the shared listener outlives the open ctx, which bounds only the open
	unregister, err := rtmpListeners.register(addr, path, l, rtmp.Config{}, rtmpListenPolicy)
	if err != nil {
		return nil, fmt.Errorf("ingest: rtmp: open %s: %w", Redact(rawURL), err)
	}
	l.unregister = unregister
	l.listener = rtmpListeners.lookup(addr) // ours while the route is registered

	// The first publisher and its tracks, bounded by ctx.
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	err = l.awaitTracks()
	if !stop() {
		err = fmt.Errorf("ingest: rtmp: open %s: %w", Redact(rawURL), ctx.Err())
	}
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	l.mu.Lock()
	l.video, l.audio = l.sess.flv.announced()
	l.mu.Unlock()
	l.opened = true
	return l, nil
}

// awaitTracks reads the first publisher until its tracks are known: both
// kinds announced, or one kind announced and openSpan of source time
// covered (or the queue bound reached). A publisher that ends before is
// dropped with its frames, and the wait goes on with the next.
func (l *rtmpListen) awaitTracks() error {
	for {
		if s := l.sess; s != nil {
			v, a := s.flv.announced()
			if v && a {
				return nil
			}
			if v || a {
				if s.flv.spanned() >= openSpan || len(l.queue) >= maxOpenFrames || queuedBytes(l.queue) >= maxOpenBytes {
					return nil
				}
			}
		}
		if err := l.step(); err != nil {
			return err
		}
	}
}

func queuedBytes(frames []Frame) int {
	n := 0
	for _, f := range frames {
		for _, b := range f.Data {
			n += len(b)
		}
	}
	return n
}

// offer implements rtmpSink. The new publisher replaces the current one,
// whose session is closed.
func (l *rtmpListen) offer(conn *rtmp.ServerConn) bool {
	s := &rtmpSession{conn: conn, offered: time.Now(), flv: newFLVFrames(&l.cache, l.now)}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return false
	}
	old := l.cur
	l.cur = s
	l.mu.Unlock()
	l.publishers.Add(1)
	if old != nil {
		l.takeovers.Add(1)
		old.superseded.Store(true)
		_ = old.conn.Close()
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return true
}

// busy implements rtmpSink: the current publisher received a byte within
// takeoverIdle. A publisher just offered counts from its offer, so a second
// one cannot take it over before it has had time to send.
func (l *rtmpListen) busy(now time.Time) bool {
	l.mu.Lock()
	s := l.cur
	l.mu.Unlock()
	if s == nil {
		return false
	}
	last := s.conn.LastInbound()
	if s.offered.After(last) {
		last = s.offered
	}
	return now.Sub(last) < takeoverIdle
}

// step does one unit of the reader's work: take the next publisher, read
// one message, or retire a publisher that ended or was taken over. It is
// called only with an empty queue and no pending discontinuity, except
// while Open waits.
func (l *rtmpListen) step() error {
	if l.isClosed() {
		return errRTMPClosed
	}
	s := l.sess
	if s == nil {
		next, err := l.next()
		if err != nil {
			return err
		}
		l.sess = next
		l.mu.Lock()
		l.active = next
		l.mu.Unlock()
		if l.opened {
			l.disc = true
		}
		return nil
	}
	if s.superseded.Load() {
		l.retire()
		return nil
	}
	m, err := s.conn.ReadMedia()
	if err != nil {
		if l.isClosed() {
			return errRTMPClosed
		}
		l.retire()
		return nil
	}
	// A bad tag is counted by flvFrames and skipped; the session goes on.
	frames, _ := s.flv.push(m)
	l.queue = append(l.queue, frames...)
	return nil
}

// next waits for a publisher newer than the last one read.
func (l *rtmpListen) next() (*rtmpSession, error) {
	for {
		l.mu.Lock()
		closed, cur := l.closed, l.cur
		l.mu.Unlock()
		if closed {
			return nil, errRTMPClosed
		}
		if cur != nil && !cur.superseded.Load() {
			return cur, nil
		}
		select {
		case <-l.wake:
		case <-l.done:
		}
	}
}

// retire ends the session being read: it is closed, its held video frames
// are queued (dropped while Open waits, with the session) and its counters
// and kinds are kept.
func (l *rtmpListen) retire() {
	s := l.sess
	l.sess = nil
	frames := s.flv.flush()
	v, a := s.flv.announced()
	l.mu.Lock()
	if l.cur == s {
		l.cur = nil
	}
	l.active = nil
	for k, n := range s.flv.stats() {
		l.doneStat[k] += n
	}
	l.doneStat["unknown_commands"] += s.conn.UnknownCommands()
	if l.opened {
		l.video = l.video || v
		l.audio = l.audio || a
	}
	l.mu.Unlock()
	_ = s.conn.Close() // after cur is cleared: a publisher that sees the close can come back at once
	if l.opened {
		l.queue = append(l.queue, frames...)
	} else {
		l.queue = nil
	}
}

func (l *rtmpListen) isClosed() bool { return l.closing.Load() }

// Read implements Reader.
func (l *rtmpListen) Read() (Frame, error) {
	for {
		if l.isClosed() {
			return Frame{}, errRTMPClosed
		}
		if len(l.queue) > 0 {
			f := l.queue[0]
			l.queue[0] = Frame{}
			l.queue = l.queue[1:]
			return f, nil
		}
		if l.disc {
			l.disc = false
			return Frame{}, ErrDiscontinuity
		}
		if err := l.step(); err != nil {
			return Frame{}, err
		}
	}
}

// Kinds implements Reader: video on track 0, audio on track 1, for every
// kind announced since Open (audio only is [0, audio]). It grows when a
// publisher announces a kind the earlier ones did not.
func (l *rtmpListen) Kinds() []media.Kind {
	l.mu.Lock()
	v, a := l.video, l.audio
	if l.active != nil {
		av, aa := l.active.flv.announced()
		v, a = v || av, a || aa
	}
	l.mu.Unlock()
	switch {
	case a:
		k := []media.Kind{0, media.KindAudio}
		if v {
			k[0] = media.KindVideo
		}
		return k
	case v:
		return []media.Kind{media.KindVideo}
	}
	return nil
}

// Spec implements Reader.
func (l *rtmpListen) Spec() Spec { return l.spec }

// Stats implements Reader: the flvFrames counters of every publisher, the
// publishers accepted and taken over, and the rejections of the shared TCP
// listener (for every path on it).
func (l *rtmpListen) Stats() map[string]uint64 {
	l.mu.Lock()
	out := make(map[string]uint64, len(l.doneStat)+8)
	for k, v := range l.doneStat {
		out[k] = v
	}
	if s := l.active; s != nil {
		for k, v := range s.flv.stats() {
			out[k] += v
		}
		out["unknown_commands"] += s.conn.UnknownCommands()
	}
	l.mu.Unlock()
	out["publishers"] = l.publishers.Load()
	out["takeovers"] = l.takeovers.Load()
	if l.listener != nil {
		for k, v := range l.listener.counters() {
			out[k] = v
		}
	}
	return out
}

// Close implements Reader. It unregisters the path, closing the TCP
// listener when this was its last path, and closes the publishers.
func (l *rtmpListen) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.closing.Store(true)
	close(l.done)
	cur, active := l.cur, l.active
	l.mu.Unlock()
	// Not under l.mu: unregister may wait for a session calling offer.
	l.unregister()
	for _, s := range []*rtmpSession{cur, active} {
		if s != nil {
			_ = s.conn.Close()
		}
	}
	return nil
}
