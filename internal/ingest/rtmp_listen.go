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
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
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

// frameQueue holds the frames ready for Read, in order, with the total size
// of their data kept as frames come and go.
type frameQueue struct {
	frames []Frame
	bytes  int
}

func frameBytes(f Frame) int {
	n := 0
	for _, b := range f.Data {
		n += len(b)
	}
	return n
}

func (q *frameQueue) push(frames []Frame) {
	for _, f := range frames {
		q.bytes += frameBytes(f)
	}
	q.frames = append(q.frames, frames...)
}

func (q *frameQueue) pop() (Frame, bool) {
	if len(q.frames) == 0 {
		return Frame{}, false
	}
	f := q.frames[0]
	q.frames[0] = Frame{}
	q.frames = q.frames[1:]
	q.bytes -= frameBytes(f)
	return f, true
}

func (q *frameQueue) len() int { return len(q.frames) }

// drop empties the queue and returns the number of frames dropped.
func (q *frameQueue) drop() int {
	n := len(q.frames)
	q.frames, q.bytes = nil, 0
	return n
}

// tracksKnown is Open's rule for an RTMP session, listen or pull: its
// tracks are known once both kinds are announced, or once one kind is
// announced and openSpan of source time is covered or the frames queued
// meanwhile reach maxOpenFrames or maxOpenBytes.
func tracksKnown(f *flvFrames, q *frameQueue) bool {
	v, a := f.announced()
	switch {
	case v && a:
		return true
	case v || a:
		return f.spanned() >= openSpan || q.len() >= maxOpenFrames || q.bytes >= maxOpenBytes
	}
	return false
}

// rtmpKinds is the track layout of an RTMP session: video on track 0,
// audio on track 1 (audio only is [0, audio]).
func rtmpKinds(video, audio bool) []media.Kind {
	switch {
	case audio:
		k := []media.Kind{0, media.KindAudio}
		if video {
			k[0] = media.KindVideo
		}
		return k
	case video:
		return []media.Kind{media.KindVideo}
	}
	return nil
}

// rtmpListenPolicy is the listener policy that Open passes to the registry;
// tests lower it.
var rtmpListenPolicy rtmpsrv.Policy

// rtmpListeners is the registry of the shared RTMP listeners.
var rtmpListeners = rtmpsrv.Default

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
// listener (see rtmpsrv.Registry). It has at most one publisher. A new one is
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
	queue  frameQueue   // ready for Read
	disc   bool         // Read returns ErrDiscontinuity once the queue is empty

	mu       sync.Mutex // guards the fields below
	closed   bool
	cur      *rtmpSession // the latest publisher offered and not ended
	active   *rtmpSession // the session the reader reads, for Kinds and Stats
	video    bool         // kinds announced by earlier publishers since Open
	audio    bool
	doneStat map[string]uint64 // counters of finished publishers

	// addr is the shared TCP listener's address, for its counters, and
	// lastListener its counters when Close began; guarded by mu.
	addr                  string
	lastListener          map[string]uint64
	closing               atomic.Bool // Close has begun
	publishers, takeovers atomic.Uint64
}

// newRTMPListen returns an unregistered listen input for addr.
func newRTMPListen(spec Spec, addr string, now func() time.Time) *rtmpListen {
	return &rtmpListen{
		spec: spec, now: now, addr: addr,
		wake: make(chan struct{}, 1), done: make(chan struct{}), doneStat: map[string]uint64{},
	}
}

func openRTMPListen(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	addr, path := rtmpListenTarget(p)
	l := newRTMPListen(p.spec, addr, env.now())
	//nolint:contextcheck // the shared listener outlives the open ctx, which bounds only the open
	unregister, err := rtmpListeners.RegisterPublish(addr, path, l, rtmp.Config{}, rtmpListenPolicy)
	if err != nil {
		return nil, fmt.Errorf("ingest: rtmp: open %s: %w", Redact(rawURL), err)
	}
	l.unregister = unregister

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

// awaitTracks reads the first publisher until its tracks are known (see
// tracksKnown). A publisher that ends before is dropped with its frames,
// and the wait goes on with the next.
func (l *rtmpListen) awaitTracks() error {
	for l.sess == nil || !tracksKnown(l.sess.flv, &l.queue) {
		if err := l.step(); err != nil {
			return err
		}
	}
	return nil
}

// Offer implements rtmpsrv.PublishSink. The new publisher replaces the
// current one, whose session is closed.
func (l *rtmpListen) Offer(conn *rtmp.ServerConn) bool {
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

// Busy implements rtmpsrv.PublishSink: the current publisher received a
// byte within takeoverIdle. A publisher just offered counts from its offer,
// so a second one cannot take it over before it has had time to send.
//
// LastInbound measures bytes read from the connection, and only the reader
// reads it: a consumer that stalls for takeoverIdle (and lets the TCP
// window fill) makes a live publisher look idle, so a new one may take it
// over, and the old session's unread data is lost with it.
func (l *rtmpListen) Busy(now time.Time) bool {
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
	l.queue.push(frames)
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
// are queued (dropped while Open waits, with the session's queued frames)
// and its counters and kinds are kept. The frames counter leaves out the
// frames dropped, so it counts the frames Read can return.
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
	l.doneStat["bad_commands"] += s.conn.BadCommands()
	if l.opened {
		l.video = l.video || v
		l.audio = l.audio || a
	} else {
		l.doneStat["frames"] -= uint64(len(frames) + l.queue.drop())
		frames = nil
	}
	l.mu.Unlock()
	_ = s.conn.Close() // after cur is cleared: a publisher that sees the close can come back at once
	l.queue.push(frames)
}

func (l *rtmpListen) isClosed() bool { return l.closing.Load() }

// Read implements Reader.
func (l *rtmpListen) Read() (Frame, error) {
	for {
		if l.isClosed() {
			return Frame{}, errRTMPClosed
		}
		if f, ok := l.queue.pop(); ok {
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
// publisher announces a kind the earlier ones did not, but the pipeline
// fixes its track layout from Kinds when the input opens: frames of a kind
// announced only after Open are dropped there.
func (l *rtmpListen) Kinds() []media.Kind {
	l.mu.Lock()
	v, a := l.video, l.audio
	if l.active != nil {
		av, aa := l.active.flv.announced()
		v, a = v || av, a || aa
	}
	l.mu.Unlock()
	return rtmpKinds(v, a)
}

// Spec implements Reader.
func (l *rtmpListen) Spec() Spec { return l.spec }

// Stats implements Reader: the flvFrames counters of every publisher, the
// commands its publishers sent that were unknown (unknown_commands) or did
// not decode (bad_commands), the publishers accepted and taken over, and the
// rejections of the shared TCP listener (for every path on it).
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
		out["bad_commands"] += s.conn.BadCommands()
	}
	listener := l.lastListener
	l.mu.Unlock()
	out["publishers"] = l.publishers.Load()
	out["takeovers"] = l.takeovers.Load()
	if listener == nil {
		listener = rtmpListeners.Stats(l.addr).Counters()
	}
	for k, v := range listener {
		out[k] = v
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
	// The listener's counters as they stand while the route is still ours;
	// after the unregister another listener may take the address.
	last := rtmpListeners.Stats(l.addr).Counters()
	l.mu.Lock()
	l.lastListener = last
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
