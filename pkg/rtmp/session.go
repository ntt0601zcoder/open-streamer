package rtmp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Session defaults applied to zero Config fields.
const (
	defaultHandshakeTimeout     = 5 * time.Second
	defaultCommandTimeout       = 10 * time.Second
	defaultIdleTimeout          = 30 * time.Second
	defaultPingInterval         = 10 * time.Second
	defaultWriteTimeout         = 10 * time.Second
	defaultMaxPrePublishMessage = 64 << 10
	defaultMaxPrePublishStreams = 8
	defaultMaxPrePublishPending = 256 << 10

	// idleGrace is the shortest read deadline: a read issued after the idle
	// limit has passed still collects the bytes already waiting.
	idleGrace = 10 * time.Millisecond
)

// Config bounds an RTMP session, server or client. A zero field takes its
// default.
type Config struct {
	// HandshakeTimeout bounds the handshake. Default 5 s.
	HandshakeTimeout time.Duration
	// CommandTimeout bounds the command exchange before media flows
	// (connect to publish, or connect to play). Default 10 s.
	CommandTimeout time.Duration
	// IdleTimeout fails a session that receives no byte for this long.
	// Default 30 s. A publishing client has no idle limit once it
	// publishes: servers send only acknowledgements, once per window.
	IdleTimeout time.Duration
	// PingInterval is how often a server sends PingRequest. Clients never
	// ping. Default 10 s.
	PingInterval time.Duration
	// WriteTimeout bounds each media write, and every write once a reader
	// goroutine runs (a play conn being served, a publish being sent). A
	// write that misses it ends the session. Default 10 s.
	WriteTimeout time.Duration
	// MaxMessage is the largest message accepted once publish is accepted,
	// and by clients. Default 8 MiB. Once publish is accepted a server also
	// allows 64 chunk streams and 16 MiB of partial messages, as clients do.
	MaxMessage int
	// MaxPrePublishMessage is the largest message a server accepts before
	// publish is accepted. Default 64 KiB.
	MaxPrePublishMessage int
	// MaxPrePublishStreams is the most chunk stream ids a server lets the
	// peer use before publish is accepted. Default 8.
	MaxPrePublishStreams int
	// MaxPrePublishPending bounds the bytes of partial messages a server
	// holds for the peer before publish is accepted. Default 256 KiB.
	MaxPrePublishPending int
	// Now is the clock for timeouts, pings and LastInbound. Default time.Now.
	// It must advance with real time: it stamps LastInbound and decides when
	// a phase, the idle limit or a ping is due, while the deadlines set on
	// the conn are durations on the real clock.
	Now func() time.Time
	// Dial opens a client's connection (Play, PlayWith, Publish), network
	// "tcp". Default: a net.Dialer, with its TCP keepalive, which is a
	// publishing client's liveness check besides its write deadlines. Tests
	// use it to shrink socket buffers.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// KeepQuery makes Publish send the URL's query verbatim, a "mode"
	// parameter included, for a target whose query is its own (a push
	// to a third-party server). By default Publish, like PlayWith, drops
	// mode, which only means something to our input URLs. PlayWith and
	// servers ignore it.
	KeepQuery bool
}

// dial opens a client's connection to addr with Dial, or a net.Dialer.
func (c Config) dial(ctx context.Context, addr string) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (c Config) withDefaults() Config {
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = defaultCommandTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.PingInterval <= 0 {
		c.PingInterval = defaultPingInterval
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaultWriteTimeout
	}
	if c.MaxMessage <= 0 {
		c.MaxMessage = defaultMaxMessage
	}
	if c.MaxPrePublishMessage <= 0 {
		c.MaxPrePublishMessage = defaultMaxPrePublishMessage
	}
	if c.MaxPrePublishStreams <= 0 {
		c.MaxPrePublishStreams = defaultMaxPrePublishStreams
	}
	if c.MaxPrePublishPending <= 0 {
		c.MaxPrePublishPending = defaultMaxPrePublishPending
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Media is one audio, video or data (metadata) message of a published or
// played stream. Type is MsgAudio, MsgVideo or MsgDataAMF0; Timestamp is in
// milliseconds as carried on the wire, so it wraps at 2^32. Payload is the FLV
// tag body, a fresh allocation owned by the caller.
type Media struct {
	Type      uint8
	Timestamp uint32
	Payload   []byte
}

// mediaOf returns m as Media when it is an audio, video or AMF0 data message.
func mediaOf(m Message) (Media, bool) {
	switch m.Type {
	case MsgAudio, MsgVideo, MsgDataAMF0:
		return Media{Type: m.Type, Timestamp: m.Timestamp, Payload: m.Payload}, true
	}
	return Media{}, false
}

// role is the side of the connection a session plays.
type role uint8

const (
	// roleServer answers the handshake, sends PingRequest every PingInterval
	// and starts with the pre-publish message limit.
	roleServer role = iota
	// roleClient starts the handshake, never pings and accepts MaxMessage
	// from the start.
	roleClient
)

// session is the message loop shared by the server and the client. It owns
// the buffered reader, the chunk reader and writer, acknowledgements, pings,
// timeouts, inbound protocol control and aggregate splitting.
//
// It runs in one of two modes. Until runReader, every method except Close,
// LastInbound, Done, Err and setWriteTimeout is called from one goroutine,
// which does all reads and all writes; phase and handshake state is set only
// then. After runReader, one reader goroutine owns next, and so every read
// and every write a read causes (acks, pings, ping responses, Window Ack
// Size); any other goroutine may call writeMedia and the control writes of
// its role. Every outbound message, media or control, is written whole while
// holding wmu, so a control message never lands inside a media message's
// chunks and the chunk writer's header state is never shared.
type session struct {
	conn  net.Conn
	cfg   Config
	role  role
	br    *bufio.Reader
	cr    *ChunkReader
	cw    *ChunkWriter
	start time.Time

	lastIn   time.Time // last inbound byte
	nextPing time.Time // zero until pings start
	phaseEnd time.Time // zero outside a bounded phase
	phase    string

	window      uint32 // peer's Window Ack Size; 0 until it sends one
	lastAck     uint64
	sentWindow  uint32 // last Window Ack Size we sent
	peerBW      uint32
	peerBWLimit uint8
	hasPeerBW   bool

	agg aggregate

	lastInbound atomic.Int64 // lastIn in Unix nanoseconds, for other goroutines
	closeOnce   sync.Once
	closeErr    error

	// writeTimeout bounds media writes, and every write once reading. It is
	// read before wmu is taken, so the wait for the mutex counts.
	writeTimeout atomic.Int64

	// reading is set once runReader starts, by the goroutine that ran the
	// exchange, before the reader exists.
	reading atomic.Bool

	// noIdle turns the idle limit off. It is set before runReader, by the
	// goroutine that ran the exchange, and read only by the reader.
	noIdle bool

	// wmu is held for every outbound message, around the chunk writer, and
	// guards writeDeadline.
	wmu sync.Mutex
	// writeDeadline is the absolute deadline, on the Config clock, of the
	// write in progress; zero gives each conn write IdleTimeout from its
	// start (the handshake and the control writes before runReader).
	writeDeadline time.Time

	readerOnce sync.Once
	done       chan struct{} // closed when the reader goroutine exits
	errMu      sync.Mutex
	err        error // first cause of the session's end
}

// newSession wraps conn. The caller runs handshake next, then the command
// exchange of its role.
func newSession(conn net.Conn, cfg Config, r role) *session {
	cfg = cfg.withDefaults()
	now := cfg.Now()
	s := &session{conn: conn, cfg: cfg, role: r, start: now, done: make(chan struct{})}
	s.writeTimeout.Store(int64(cfg.WriteTimeout))
	s.touch(now)
	s.br = bufio.NewReader(sessionReader{s})
	lim := s.postPublishLimits()
	if r == roleServer {
		// An unauthenticated peer may hold only a little memory.
		lim = ChunkLimits{
			MaxMessage: cfg.MaxPrePublishMessage,
			MaxStreams: cfg.MaxPrePublishStreams,
			MaxPending: cfg.MaxPrePublishPending,
		}
	}
	s.cr = NewChunkReader(s.br, lim)
	s.cw = NewChunkWriter(sessionWriter{s})
	return s
}

// handshake runs the role's handshake within HandshakeTimeout. A server
// starts pinging once it completes.
func (s *session) handshake() error {
	s.setPhase(s.cfg.HandshakeTimeout, "handshake")
	defer s.endPhase()
	// The handshake writes through sessionWriter, which runs under wmu.
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.writeDeadline = time.Time{}
	rw := struct {
		io.Reader
		io.Writer
	}{s.br, sessionWriter{s}}
	var err error
	if s.role == roleServer {
		err = ServerHandshake(rw, rand.Reader)
	} else {
		err = ClientHandshake(rw, rand.Reader)
	}
	if err != nil {
		return err
	}
	if s.role == roleServer {
		s.nextPing = s.cfg.Now().Add(s.cfg.PingInterval)
	}
	return nil
}

// setPhase bounds the reads and writes that follow by d, until endPhase;
// what names the phase in the timeout error.
func (s *session) setPhase(d time.Duration, what string) {
	s.phaseEnd = s.cfg.Now().Add(d)
	s.phase = what
}

// endPhase removes the phase bound.
func (s *session) endPhase() {
	s.phaseEnd = time.Time{}
	s.phase = ""
}

// raiseMessageLimit lifts the pre-publish limits to the post-publish ones,
// once a server has accepted publish.
func (s *session) raiseMessageLimit() {
	s.cr.SetLimits(s.postPublishLimits())
}

// postPublishLimits are the chunk limits of a client, and of a server once
// publish is accepted: MaxMessage, 64 chunk streams, 16 MiB pending.
func (s *session) postPublishLimits() ChunkLimits {
	return ChunkLimits{MaxMessage: s.cfg.MaxMessage, MaxStreams: defaultMaxStreams, MaxPending: defaultMaxPending}
}

// LastInbound is when the last byte arrived from the peer. It is safe to call
// from any goroutine.
func (s *session) LastInbound() time.Time {
	return time.Unix(0, s.lastInbound.Load())
}

// Close closes the connection, which unblocks a pending read or write and
// stops the reader goroutine. It never writes, so it is safe to call from any
// goroutine, and only the first call closes. When nothing ended the session
// before, Err reports net.ErrClosed.
func (s *session) Close() error {
	s.setErr(fmt.Errorf("rtmp: session: %w", net.ErrClosed))
	s.closeOnce.Do(func() { s.closeErr = s.conn.Close() })
	return s.closeErr
}

// fail ends the session with err as its cause, unless one was set before,
// and closes the connection.
func (s *session) fail(err error) {
	s.setErr(err)
	_ = s.Close()
}

func (s *session) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// Err is the first cause of the session's end: the reader's or a write's
// error, the handler's error, or net.ErrClosed after Close. It is nil while
// the session runs and is safe to call from any goroutine.
func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Done is closed once the reader goroutine started by runReader has
// exited; Err then reports why. Without runReader it never closes.
func (s *session) Done() <-chan struct{} {
	return s.done
}

// runReader starts the reader goroutine, once the command exchange is over
// and no phase is set: it reads messages with next, which answers pings and
// acknowledges as before, and passes every other message to handle. A read
// error or a handle error ends the session with that cause and closes the
// connection; Close stops the goroutine. From now on every write is bounded
// by the write timeout. It is called from the goroutine that ran the
// exchange; only the first call starts a reader, later ones do nothing.
//
// handle runs on the reader goroutine, which also answers pings and sends
// acknowledgements: it must not block, or the peer's pings go unanswered
// and the idle limit can end the session.
func (s *session) runReader(handle func(Message) error) {
	s.readerOnce.Do(func() {
		s.reading.Store(true)
		go func() {
			defer close(s.done)
			for {
				m, err := s.next()
				if err != nil {
					s.fail(fmt.Errorf("rtmp: session: read: %w", err))
					return
				}
				if err := handle(m); err != nil {
					s.fail(err)
					return
				}
			}
		}()
	})
}

// setWriteTimeout replaces Config.WriteTimeout for the writes that follow;
// d <= 0 restores Config.WriteTimeout. It is safe to call from any
// goroutine.
func (s *session) setWriteTimeout(d time.Duration) {
	if d <= 0 {
		d = s.cfg.WriteTimeout
	}
	s.writeTimeout.Store(int64(d))
}

// write runs f on the chunk writer holding wmu. A media write, and every
// write once the reader runs, must end within the write timeout of the call,
// the wait for wmu included: one whose deadline passed while waiting fails
// with os.ErrDeadlineExceeded before writing anything. A control write of
// the single-goroutine mode keeps IdleTimeout (or the phase end).
func (s *session) write(media bool, f func(*ChunkWriter) error) error {
	if !media && !s.reading.Load() {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		s.writeDeadline = time.Time{}
		return f(s.cw)
	}
	deadline := s.cfg.Now().Add(time.Duration(s.writeTimeout.Load()))
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if !s.cfg.Now().Before(deadline) {
		return fmt.Errorf("rtmp: session: write: waited out the write timeout: %w", os.ErrDeadlineExceeded)
	}
	s.writeDeadline = deadline
	return f(s.cw)
}

// writeMedia writes one audio, video or data message on chunk stream csid
// within the write timeout of the call. It may be called from any goroutine
// once runReader runs. A failed write may leave the chunk stream
// mid-message, and one that waited out its timeout means the peer is not
// keeping up, so either ends the session: the error is returned and becomes
// Err.
func (s *session) writeMedia(csid uint32, m Message) error {
	err := s.write(true, func(cw *ChunkWriter) error { return cw.WriteMessage(csid, m) })
	if err != nil {
		err = fmt.Errorf("rtmp: session: media: %w", err)
		s.fail(err)
	}
	return err
}

func (s *session) touch(now time.Time) {
	s.lastIn = now
	s.lastInbound.Store(now.UnixNano())
}

// next returns the next message the session does not consume itself:
// commands, data, audio, video, stream user control events (StreamBegin,
// StreamEOF, …) and other types it does not know. Protocol control messages
// and pings are handled here, and an aggregate is returned as its
// sub-messages one by one.
//
// An error is terminal: the chunk stream may be left mid-message, so the
// caller must Close the session and not call next again.
func (s *session) next() (Message, error) {
	for {
		m, ok, err := s.agg.next()
		if err != nil {
			return Message{}, err
		}
		if ok {
			return m, nil
		}
		m, err = s.cr.ReadMessage()
		if err != nil {
			return Message{}, err
		}
		if err := s.acknowledge(); err != nil {
			return Message{}, err
		}
		consumed, err := s.handleControl(m)
		if err != nil {
			return Message{}, err
		}
		if consumed {
			continue
		}
		if m.Type == MsgAggregate {
			s.agg = aggregate{p: m.Payload, ts: m.Timestamp, streamID: m.StreamID}
			continue
		}
		return m, nil
	}
}

// acknowledge sends Ack(BytesRead) once a window's worth of bytes has
// arrived since the last one. The sequence number wraps at 2^32.
func (s *session) acknowledge() error {
	if s.window == 0 {
		return nil
	}
	n := s.cr.BytesRead()
	if n-s.lastAck < uint64(s.window) {
		return nil
	}
	s.lastAck = n
	return s.writeControl(MsgAck, binary.BigEndian.AppendUint32(nil, uint32(n)))
}

// tick fails a timed-out phase and sends a due ping. The idle limit is left
// to checkIdle, after a read that found nothing.
func (s *session) tick(now time.Time) error {
	if !s.phaseEnd.IsZero() && !now.Before(s.phaseEnd) {
		return fmt.Errorf("rtmp: session: %s timed out: %w", s.phase, os.ErrDeadlineExceeded)
	}
	if !s.nextPing.IsZero() && !now.Before(s.nextPing) {
		s.nextPing = now.Add(s.cfg.PingInterval)
		return s.sendUserControl(ucPingRequest, uint32(now.Sub(s.start).Milliseconds()))
	}
	return nil
}

// checkIdle fails a session whose last inbound byte is IdleTimeout old,
// unless noIdle is set.
func (s *session) checkIdle(now time.Time) error {
	if !s.noIdle && !now.Before(s.lastIn.Add(s.cfg.IdleTimeout)) {
		return fmt.Errorf("rtmp: session: no inbound data for %v: %w", s.cfg.IdleTimeout, os.ErrDeadlineExceeded)
	}
	return nil
}

// readDeadline is the earliest of the idle limit, the next ping and the
// phase end, or zero when there is none of them (noIdle, no ping, no phase).
// The idle limit counts as at least idleGrace from now, so a consumer that
// comes back late still reads the bytes that are waiting.
func (s *session) readDeadline(now time.Time) time.Time {
	var d time.Time
	if !s.noIdle {
		d = s.lastIn.Add(s.cfg.IdleTimeout)
		if floor := now.Add(idleGrace); d.Before(floor) {
			d = floor
		}
	}
	if !s.nextPing.IsZero() && (d.IsZero() || s.nextPing.Before(d)) {
		d = s.nextPing
	}
	if !s.phaseEnd.IsZero() && (d.IsZero() || s.phaseEnd.Before(d)) {
		d = s.phaseEnd
	}
	return d
}

// connDeadline converts a time on the Config clock to one on the conn's.
// The zero time stays zero: no deadline.
func connDeadline(at, now time.Time) time.Time {
	if at.IsZero() {
		return time.Time{}
	}
	return time.Now().Add(at.Sub(now))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// sessionReader reads the conn for the session's bufio.Reader. A read
// deadline that expires on a ping slot sends PingRequest and reads on, so the
// chunk reader above never sees a timeout in the middle of a message; one
// past the phase end fails, and so does a read that times out with nothing
// received when the last inbound byte is IdleTimeout old (unless noIdle).
// With noIdle, no ping and no phase, a read waits without a deadline until
// data arrives or the conn is closed.
type sessionReader struct{ s *session }

func (r sessionReader) Read(p []byte) (int, error) {
	s := r.s
	for {
		now := s.cfg.Now()
		if err := s.tick(now); err != nil {
			return 0, err
		}
		// An error here (a closed conn) surfaces from the Read below.
		_ = s.conn.SetReadDeadline(connDeadline(s.readDeadline(now), now))
		n, err := s.conn.Read(p)
		if n > 0 {
			s.touch(s.cfg.Now())
			if isTimeout(err) {
				err = nil
			}
			return n, err
		}
		if err != nil && !isTimeout(err) {
			return 0, err
		}
		if err := s.checkIdle(s.cfg.Now()); err != nil {
			return 0, err
		}
	}
}

// sessionWriter writes to the conn with a deadline of writeDeadline (or
// IdleTimeout from now when that is zero), or the phase end when that is
// sooner. It runs only under wmu; the phase is set only before the reader
// goroutine starts, and is zero after.
type sessionWriter struct{ s *session }

func (w sessionWriter) Write(p []byte) (int, error) {
	s := w.s
	now := s.cfg.Now()
	d := s.writeDeadline
	if d.IsZero() {
		d = now.Add(s.cfg.IdleTimeout)
	}
	if !s.reading.Load() && !s.phaseEnd.IsZero() && s.phaseEnd.Before(d) {
		d = s.phaseEnd
	}
	_ = s.conn.SetWriteDeadline(connDeadline(d, now))
	return s.conn.Write(p)
}

// aggregate splits an aggregate message (type 22) into its sub-messages, one
// per call. Each sub-message is an FLV tag (11-byte header, data, 4-byte
// back-pointer); its timestamp is the aggregate's plus its offset from the
// first sub-message, mod 2^32.
type aggregate struct {
	p        []byte
	ts       uint32
	first    uint32
	started  bool
	streamID uint32
}

const flvTagHeaderLen = 11

func (a *aggregate) next() (Message, bool, error) {
	for len(a.p) > 0 {
		if len(a.p) < flvTagHeaderLen {
			a.p = nil
			return Message{}, false, errors.New("rtmp: session: truncated aggregate tag header")
		}
		typ := a.p[0] & 0x1F
		size := int(be24(a.p[1:4]))
		ts := be24(a.p[4:7]) | uint32(a.p[7])<<24
		if len(a.p)-flvTagHeaderLen < size {
			a.p = nil
			return Message{}, false, errors.New("rtmp: session: truncated aggregate tag")
		}
		data := a.p[flvTagHeaderLen : flvTagHeaderLen+size]
		rest := a.p[flvTagHeaderLen+size:]
		switch {
		case len(rest) >= 4:
			rest = rest[4:]
		case len(rest) > 0:
			a.p = nil
			return Message{}, false, errors.New("rtmp: session: truncated aggregate back-pointer")
		}
		// The last back-pointer may be missing.
		a.p = rest
		if !a.started {
			a.first, a.started = ts, true
		}
		switch typ {
		case MsgAudio, MsgVideo, MsgDataAMF0:
			return Message{
				Type:      typ,
				StreamID:  a.streamID,
				Timestamp: a.ts + (ts - a.first),
				Payload:   slices.Clone(data),
			}, true, nil
		}
	}
	return Message{}, false, nil
}
