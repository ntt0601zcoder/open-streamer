package rtsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server defaults applied when a ServerConfig field is zero.
const (
	defaultServerReadTimeout    = 10 * time.Second
	defaultServerWriteTimeout   = 10 * time.Second
	defaultServerSessionTimeout = 60 * time.Second
	defaultServerBodyBytes      = 64 << 10
	defaultMaxPending           = 64
	defaultMaxPendingPerIP      = 4
	defaultMaxSessions          = 1024
	defaultMaxSessionsPerIP     = 64
	defaultMaxSessionsPerPath   = 256
	defaultPendingLifetime      = 30 * time.Second
)

const (
	// maxSessionTracks bounds the tracks one session may set up.
	maxSessionTracks = 8
	// byeWait bounds the BYE write of ServerSession.Close.
	byeWait = 500 * time.Millisecond
	// requestLinePeek is how far a line is examined to tell a request with
	// an unknown method from garbage on the interleaved stream.
	requestLinePeek  = 1024
	acceptBackoffMax = time.Second
	serverName       = "open-streamer"
	publicMethods    = "OPTIONS, DESCRIBE, SETUP, PLAY, GET_PARAMETER, TEARDOWN"
	reasonNotFound   = "Stream Not Found"
)

// ErrServerClosed is returned by Serve after Close.
var ErrServerClosed = errors.New("rtsp: server closed")

// ErrSessionClosed is the Err of a session ended by ServerSession.Close or
// Server.Close.
var ErrSessionClosed = errors.New("rtsp: session closed")

// ErrTeardown is the Err of a session ended by the client's TEARDOWN.
var ErrTeardown = errors.New("rtsp: session torn down")

// errStop ends a connection after its last response without an error.
var errStop = errors.New("rtsp: connection done")

// ServerConfig tunes a Server. A zero field means its default.
type ServerConfig struct {
	// ReadTimeout bounds each request read until PLAY (default 10 s).
	ReadTimeout time.Duration
	// WriteTimeout bounds every response and every WritePackets or
	// WriteRTCP call; a session that misses it ends (default 10 s).
	WriteTimeout time.Duration
	// SessionTimeout is announced in the Session header and ends a UDP
	// session that sends neither a request nor RTCP for that long; a TCP
	// session lives while its connection is open (default 60 s).
	SessionTimeout time.Duration
	// Limits bounds requests (default 64 KiB header, 64 KiB body).
	Limits Limits
	// MaxPending and MaxPendingPerIP cap the connections without a playing
	// session per listener and per IP (IPv6 by /64) (default 64 and 4).
	MaxPending, MaxPendingPerIP int
	// MaxSessions, MaxSessionsPerIP and MaxSessionsPerPath cap the playing
	// sessions per listener, per IP and per stream path, checked at PLAY
	// (default 1024, 64 and 256).
	MaxSessions, MaxSessionsPerIP, MaxSessionsPerPath int
	// PendingLifetime is how long a connection may live without a playing
	// session (default 30 s).
	PendingLifetime time.Duration
	// UDPHost, when non-empty, is where the server binds and owns one
	// RTP/RTCP port pair for UDP sessions.
	UDPHost string
	// ConnHook, when set, is called on every accepted connection before it
	// is served.
	ConnHook func(net.Conn)
	// Now is the clock of session liveness (default time.Now).
	Now func() time.Time
}

func (c ServerConfig) withDefaults() ServerConfig {
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	num := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	dur(&c.ReadTimeout, defaultServerReadTimeout)
	dur(&c.WriteTimeout, defaultServerWriteTimeout)
	dur(&c.SessionTimeout, defaultServerSessionTimeout)
	dur(&c.PendingLifetime, defaultPendingLifetime)
	num(&c.Limits.BodyBytes, defaultServerBodyBytes)
	num(&c.MaxPending, defaultMaxPending)
	num(&c.MaxPendingPerIP, defaultMaxPendingPerIP)
	num(&c.MaxSessions, defaultMaxSessions)
	num(&c.MaxSessionsPerIP, defaultMaxSessionsPerIP)
	num(&c.MaxSessionsPerPath, defaultMaxSessionsPerPath)
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Handler serves the streams of a Server.
type Handler interface {
	// Describe returns the tracks of the stream at path (normalised: no
	// leading or trailing slash, no query, no trackID suffix), or ok=false
	// (answered 404). It is called for DESCRIBE and at a session's first
	// SETUP, whose result is frozen for the session.
	Describe(path string) (tracks []TrackDesc, ok bool)
	// Play is called in its own goroutine once PLAY is answered. It runs
	// until ctx ends (the session ended) and sends with s.WritePackets and
	// s.WriteRTCP. When it returns the server closes the session.
	Play(ctx context.Context, s *ServerSession)
}

// ServerStats counts what a Server has seen.
type ServerStats struct {
	// Connections and Sessions are the open RTSP connections and the
	// playing sessions.
	Connections, Sessions int
	// Rejected counts connections refused by a pending cap and requests
	// answered 404 (unknown stream or track, or a session cap).
	Rejected uint64
	// ProtocolErrors counts malformed or oversized requests, bad CSeq
	// headers and garbage skipped on the interleaved stream.
	ProtocolErrors uint64
	// Timeouts counts connections ended by the read timeout or the pending
	// lifetime and sessions ended by the write or session timeout.
	Timeouts uint64
}

// listenerState counts one listener's connections; guarded by Server.mu.
type listenerState struct{ pending, sessions int }

// Server is an RTSP 1.0 server for playing clients (RFC 2326): one
// goroutine per connection, one session per connection.
type Server struct {
	h   Handler
	cfg ServerConfig

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[*serverConn]struct{}
	pendingIP map[string]int
	sessIP    map[string]int
	sessPath  map[string]int
	wg        sync.WaitGroup

	nRejected, nProtocol, nTimeouts atomic.Uint64
}

// NewServer returns a server for h; Serve starts it.
func NewServer(h Handler, cfg ServerConfig) *Server {
	return &Server{
		h: h, cfg: cfg.withDefaults(),
		listeners: map[net.Listener]struct{}{}, conns: map[*serverConn]struct{}{},
		pendingIP: map[string]int{}, sessIP: map[string]int{}, sessPath: map[string]int{},
	}
}

// Serve accepts connections on l until Close, then returns ErrServerClosed.
// Transient accept errors are retried with a backoff; a listener closed by
// someone else ends Serve with its error.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrServerClosed
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	ls := &listenerState{}
	var backoff time.Duration
	for {
		nc, err := l.Accept()
		if err == nil {
			backoff = 0
			s.accept(nc, ls)
			continue
		}
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		switch {
		case closed:
			return ErrServerClosed
		case errors.Is(err, net.ErrClosed):
			s.mu.Lock()
			delete(s.listeners, l)
			s.mu.Unlock()
			return fmt.Errorf("rtsp: serve: %w", err)
		}
		backoff = min(max(2*backoff, 5*time.Millisecond), acceptBackoffMax)
		time.Sleep(backoff)
	}
}

// accept admits a connection as pending, or refuses it at a pending cap.
func (s *Server) accept(nc net.Conn, ls *listenerState) {
	if s.cfg.ConnHook != nil {
		s.cfg.ConnHook(nc)
	}
	peer, key := peerKey(nc.RemoteAddr())
	s.mu.Lock()
	if s.closed || ls.pending >= s.cfg.MaxPending || s.pendingIP[key] >= s.cfg.MaxPendingPerIP {
		if !s.closed {
			s.nRejected.Add(1)
		}
		s.mu.Unlock()
		_ = nc.Close()
		return
	}
	ls.pending++
	s.pendingIP[key]++
	c := &serverConn{
		srv: s, ls: ls, nc: nc, br: bufio.NewReaderSize(nc, 64<<10),
		peer: peer, ipKey: key, since: time.Now(),
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go c.serve()
}

// peerKey returns the peer IP and the key its caps are counted under: the
// address for IPv4, the /64 prefix for IPv6.
func peerKey(a net.Addr) (net.IP, string) {
	var ip net.IP
	if ta, ok := a.(*net.TCPAddr); ok {
		ip = ta.IP
	} else if host, _, err := net.SplitHostPort(a.String()); err == nil {
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return nil, a.String()
	}
	if v4 := ip.To4(); v4 != nil {
		return v4, v4.String()
	}
	return ip, ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Close stops every listener, closes every session (playing sessions with
// BYE, best effort) and waits for every connection's goroutines. It is
// idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.closed = true
	var ls []net.Listener
	for l := range s.listeners {
		ls = append(ls, l)
	}
	var playing []*ServerSession
	var idle []net.Conn
	for c := range s.conns {
		if c.playing {
			playing = append(playing, c.sess)
		} else {
			idle = append(idle, c.nc)
		}
	}
	s.mu.Unlock()
	for _, l := range ls {
		_ = l.Close()
	}
	for _, nc := range idle {
		_ = nc.Close()
	}
	var wg sync.WaitGroup
	for _, ss := range playing {
		wg.Go(ss.Close)
	}
	wg.Wait()
	s.wg.Wait()
	return nil
}

// Stats returns the counters.
func (s *Server) Stats() ServerStats {
	s.mu.Lock()
	st := ServerStats{Connections: len(s.conns)}
	for c := range s.conns {
		if c.playing {
			st.Sessions++
		}
	}
	s.mu.Unlock()
	st.Rejected = s.nRejected.Load()
	st.ProtocolErrors = s.nProtocol.Load()
	st.Timeouts = s.nTimeouts.Load()
	return st
}

// startPlaying moves c from pending to playing unless a session cap is
// reached or the server is closing.
func (s *Server) startPlaying(c *serverConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := c.sess.path
	if s.closed || c.ls.sessions >= s.cfg.MaxSessions || s.sessIP[c.ipKey] >= s.cfg.MaxSessionsPerIP ||
		s.sessPath[path] >= s.cfg.MaxSessionsPerPath {
		return false
	}
	dec(s.pendingIP, c.ipKey)
	c.ls.pending--
	c.ls.sessions++
	s.sessIP[c.ipKey]++
	s.sessPath[path]++
	c.playing = true
	return true
}

// release forgets a finished connection.
func (s *Server) release(c *serverConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
	if c.playing {
		c.ls.sessions--
		dec(s.sessIP, c.ipKey)
		dec(s.sessPath, c.sess.path)
		return
	}
	c.ls.pending--
	dec(s.pendingIP, c.ipKey)
}

func dec(m map[string]int, k string) {
	if m[k] <= 1 {
		delete(m, k)
		return
	}
	m[k]--
}

// serverConn is one RTSP connection. sess and playing are written by the
// connection's goroutine under Server.mu and read by it without the lock.
type serverConn struct {
	srv   *Server
	ls    *listenerState
	nc    net.Conn
	br    *bufio.Reader
	peer  net.IP
	ipKey string
	since time.Time

	wmu sync.Mutex // serialises writes to nc

	sess     *ServerSession
	playing  bool
	playDone chan struct{} // closed when Play returns; nil before PLAY
}

func (c *serverConn) serve() {
	defer c.srv.wg.Done()
	err := c.loop()
	if c.sess != nil {
		if errors.Is(err, errStop) {
			err = ErrTeardown
		} else {
			err = fmt.Errorf("rtsp: session: connection ended: %w", err)
		}
		c.sess.end(err)
	}
	_ = c.nc.Close()
	if c.playDone != nil {
		<-c.playDone
	}
	c.srv.release(c)
}

// loop reads requests and interleaved frames until the connection ends.
func (c *serverConn) loop() error {
	for {
		_ = c.nc.SetReadDeadline(c.readDeadline())
		err := c.readOne()
		if err == nil {
			continue
		}
		if isTimeout(err) {
			if c.liveUDP() {
				continue
			}
			c.srv.nTimeouts.Add(1)
		}
		return err
	}
}

// readDeadline is the read timeout and the pending lifetime until PLAY,
// none for a playing TCP session (the write deadline catches dead peers),
// and the session timeout from the last sign of life for a UDP session.
func (c *serverConn) readDeadline() time.Time {
	cfg := c.srv.cfg
	now := time.Now()
	if !c.playing {
		d := now.Add(cfg.ReadTimeout)
		if end := c.since.Add(cfg.PendingLifetime); end.Before(d) {
			d = end
		}
		return d
	}
	if c.sess.kind == transportUDP {
		return now.Add(c.sess.idleLeft())
	}
	return time.Time{}
}

// liveUDP reports whether a read timeout only means the UDP session's
// liveness came from its RTCP since the deadline was set.
func (c *serverConn) liveUDP() bool {
	return c.playing && c.sess.kind == transportUDP && c.sess.idleLeft() > 0
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readOne reads one unit: an interleaved frame, a stray line end, a
// request, or a run of garbage.
func (c *serverConn) readOne() error {
	b, err := c.br.Peek(1)
	if err != nil {
		return fmt.Errorf("rtsp: read: %w", err)
	}
	switch b[0] {
	case '$':
		return c.readFrame()
	case '\r', '\n':
		// RFC 2326 section 10: stray line ends between messages are ignored.
		_, _ = c.br.Discard(1)
		return nil
	}
	isReq, err := c.requestAhead()
	if err != nil {
		return fmt.Errorf("rtsp: read: %w", err)
	}
	if !isReq {
		c.srv.nProtocol.Add(1)
		return c.resync()
	}
	req, err := ReadRequest(c.br, c.srv.cfg.Limits)
	if err != nil {
		if !isTimeout(err) && !errors.Is(err, io.EOF) {
			c.srv.nProtocol.Add(1)
		}
		return err
	}
	if c.sess != nil {
		c.sess.touch()
	}
	return c.handle(req)
}

// requestAhead reports whether the next line is a request line: a known
// method, or any "<token> <url> RTSP/1.0" within the first 1024 bytes.
func (c *serverConn) requestAhead() (bool, error) {
	// A short peek ends in an error the next Peek reports again.
	head, _ := c.br.Peek(maxStartPeek)
	if isRequestStart(head) {
		return true, nil
	}
	line, err := c.peekLine()
	if err != nil {
		return false, err
	}
	f := strings.Fields(string(line))
	return len(f) == 3 && f[2] == protoVersion && validToken(f[0]), nil
}

// peekLine returns the next line without consuming it, or the first 1024
// bytes when no line end comes before.
func (c *serverConn) peekLine() ([]byte, error) {
	n := 1
	for {
		b, err := c.br.Peek(n)
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			return b[:i], nil
		}
		if err != nil {
			return nil, err
		}
		if n >= requestLinePeek {
			return b, nil
		}
		// Scan what is buffered; block for one more byte only when the
		// whole buffer has been seen.
		n = min(max(n+1, c.br.Buffered()), requestLinePeek)
	}
}

// resync skips to the next '$' or past the next line end.
func (c *serverConn) resync() error {
	for {
		b, err := c.br.Peek(1)
		if err != nil {
			return fmt.Errorf("rtsp: read: %w", err)
		}
		v := b[0]
		if v == '$' {
			return nil
		}
		_, _ = c.br.Discard(1)
		if v == '\n' {
			return nil
		}
	}
}

// readFrame reads one interleaved frame from the client. Its RTCP only
// proves the client alive.
func (c *serverConn) readFrame() error {
	var h [4]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return fmt.Errorf("rtsp: read frame: %w", err)
	}
	if _, err := c.br.Discard(int(binary.BigEndian.Uint16(h[2:]))); err != nil {
		return fmt.Errorf("rtsp: read frame: %w", err)
	}
	if c.sess != nil {
		c.sess.touch()
	}
	return nil
}

// writeResponse writes a response under the write mutex.
func (c *serverConn) writeResponse(r *Response) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.nc.SetWriteDeadline(time.Now().Add(c.srv.cfg.WriteTimeout))
	return r.Write(c.nc)
}

// lockWriteBy takes the write mutex, giving up at giveUp. While it waits
// it keeps cutting the current writer's deadline to cut, which ends a
// write stuck on a full send buffer.
func (c *serverConn) lockWriteBy(cut, giveUp time.Time) bool {
	for !c.wmu.TryLock() {
		if !time.Now().Before(giveUp) {
			return false
		}
		_ = c.nc.SetWriteDeadline(cut)
		time.Sleep(time.Millisecond)
	}
	return true
}

func reasonFor(code int) string {
	switch code {
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 404:
		return reasonNotFound
	case 405:
		return "Method Not Allowed"
	case 454:
		return "Session Not Found"
	case 455:
		return "Method Not Valid in This State"
	case 459:
		return "Aggregate Operation Not Allowed"
	case 461:
		return "Unsupported Transport"
	case 500:
		return "Internal Server Error"
	default:
		return "Not Implemented"
	}
}

func setStatus(r *Response, code int) {
	r.StatusCode, r.Reason = code, reasonFor(code)
}

// handle answers one request; after runs once the response is written.
func (c *serverConn) handle(req *Request) error {
	resp := &Response{Header: Header{}}
	var after func()
	stop := false
	cseq := strings.TrimSpace(req.Header.Get("CSeq"))
	if _, err := strconv.ParseUint(cseq, 10, 31); err != nil || len(req.Header["CSeq"]) != 1 {
		c.srv.nProtocol.Add(1)
		setStatus(resp, 400)
	} else {
		resp.Header.Set("CSeq", cseq)
		switch req.Method {
		case "OPTIONS":
			setStatus(resp, 200)
			resp.Header.Set("Public", publicMethods)
		case "DESCRIBE":
			c.describe(req, resp)
		case "SETUP":
			c.setup(req, resp)
		case "PLAY":
			after = c.play(req, resp)
		case "GET_PARAMETER":
			c.getParameter(req, resp)
		case "TEARDOWN":
			after = c.teardown(req, resp)
			stop = after != nil
		case "PAUSE":
			setStatus(resp, 455)
		case "RECORD", "ANNOUNCE":
			setStatus(resp, 405)
			resp.Header.Set("Allow", publicMethods)
		default:
			setStatus(resp, 501)
		}
	}
	resp.Header.Set("Server", serverName)
	if resp.StatusCode == 404 {
		c.srv.nRejected.Add(1)
	}
	if err := c.writeResponse(resp); err != nil {
		return err
	}
	if after != nil {
		after()
	}
	if stop {
		return errStop
	}
	return nil
}

func (c *serverConn) describe(req *Request, resp *Response) {
	tg, err := parseTarget(req.URL)
	if err != nil {
		setStatus(resp, 400)
		return
	}
	tracks, ok := c.srv.h.Describe(tg.path)
	if !ok || len(tracks) == 0 {
		setStatus(resp, 404)
		return
	}
	sdp, err := BuildSDP(originHost(c.nc.LocalAddr()), tracks)
	if err != nil {
		setStatus(resp, 500)
		return
	}
	setStatus(resp, 200)
	resp.Header.Set("Content-Base", tg.contentBase)
	resp.Header.Set("Content-Type", "application/sdp")
	resp.Body = sdp
}

// originHost is the SDP o= address: the local IP of the connection, or
// 0.0.0.0 when it has none.
func originHost(a net.Addr) string {
	if ip, _ := peerKey(a); ip != nil {
		return ip.String()
	}
	return "0.0.0.0"
}

// sessionOK reports whether the request's Session header names this
// connection's session.
func (c *serverConn) sessionOK(req *Request) bool {
	return c.sess != nil && c.sess.matches(req.Header.Get("Session"))
}

func (c *serverConn) setup(req *Request, resp *Response) {
	if req.Header.Get("Session") != "" {
		if !c.sessionOK(req) {
			setStatus(resp, 454)
			return
		}
	} else if c.sess != nil {
		// One session per connection.
		setStatus(resp, 455)
		return
	}
	if c.playing {
		setStatus(resp, 455)
		return
	}
	tg, err := parseTarget(req.URL)
	switch {
	case err != nil:
		setStatus(resp, 400)
		return
	case tg.track < 0 && !tg.badTrack:
		setStatus(resp, 459)
		return
	}
	sess := c.sess
	var descs []TrackDesc
	if sess == nil {
		var ok bool
		if descs, ok = c.srv.h.Describe(tg.path); !ok {
			descs = nil
		}
	} else if tg.path == sess.path {
		descs = sess.descs
	}
	if tg.track < 0 || tg.track >= len(descs) || tg.track >= maxSessionTracks {
		setStatus(resp, 404)
		return
	}
	if sess != nil && sess.tracks[tg.track] != nil {
		setStatus(resp, 455)
		return
	}
	current := transportNone
	if sess != nil {
		current = sess.kind
	}
	offers, err := ParseTransports(req.Header.Get("Transport"))
	if err != nil {
		setStatus(resp, 461)
		return
	}
	// UDP is offered only once the server owns an RTP/RTCP pair.
	offer, kind, ok := selectTransport(offers, c.peer, false, current)
	if !ok {
		setStatus(resp, 461)
		return
	}
	if sess == nil {
		sess = newServerSession(c, tg.path, descs)
	}
	t := sess.addTrack(tg.track, req.URL, kind, offer.Interleaved)
	if c.sess == nil {
		c.srv.mu.Lock()
		c.sess = sess
		c.srv.mu.Unlock()
	}
	setStatus(resp, 200)
	resp.Header.Set("Transport", transportReply(kind, offer, t.ch, [2]int{}, t.ssrc))
	resp.Header.Set("Session", sess.header())
}

// play answers PLAY; the returned func starts the handler once the
// response is out.
func (c *serverConn) play(req *Request, resp *Response) func() {
	if c.sess == nil {
		setStatus(resp, 455)
		return nil
	}
	if !c.sessionOK(req) {
		setStatus(resp, 454)
		return nil
	}
	if tg, err := parseTarget(req.URL); err != nil || tg.path != c.sess.path {
		setStatus(resp, 404)
		return nil
	}
	first := !c.playing
	if first && !c.srv.startPlaying(c) {
		setStatus(resp, 404)
		return nil
	}
	setStatus(resp, 200)
	resp.Header.Set("Session", c.sess.header())
	resp.Header.Set("Range", "npt=now-")
	resp.Header.Set("RTP-Info", c.sess.rtpInfo())
	if !first {
		return nil
	}
	return c.startPlay
}

// startPlay runs the handler's Play in its own goroutine; the session is
// closed when Play returns, and the connection's goroutine waits for it.
func (c *serverConn) startPlay() {
	c.playDone = make(chan struct{})
	sess := c.sess
	go func() {
		defer close(c.playDone)
		c.srv.h.Play(sess.ctx, sess)
		sess.Close()
	}()
}

func (c *serverConn) getParameter(req *Request, resp *Response) {
	if req.Header.Get("Session") == "" {
		setStatus(resp, 200)
		return
	}
	if !c.sessionOK(req) {
		setStatus(resp, 454)
		return
	}
	setStatus(resp, 200)
	resp.Header.Set("Session", c.sess.header())
}

// teardown answers TEARDOWN; the returned func ends the session once the
// response is out, and the connection ends with it.
func (c *serverConn) teardown(req *Request, resp *Response) func() {
	if !c.sessionOK(req) {
		setStatus(resp, 454)
		return nil
	}
	setStatus(resp, 200)
	sess := c.sess
	return func() { sess.end(ErrTeardown) }
}

// reqTarget is a request URL split for lookup.
type reqTarget struct {
	// path is normalised: no leading or trailing slash, no query, no
	// trackID suffix.
	path string
	// track is the trackID suffix, or -1 when there is none or it is
	// invalid (badTrack).
	track    int
	badTrack bool
	// contentBase is scheme://host[:port] as sent, without userinfo, plus
	// "/" + path + "/".
	contentBase string
}

const trackPrefix = "trackid="

// parseTarget normalises a request URL: the query is dropped, then a
// trailing slash, then a /trackID=<n> suffix is split off.
func parseTarget(raw string) (reqTarget, error) {
	u, err := url.Parse(raw)
	i := strings.Index(raw, "://")
	if err != nil || u.Host == "" || i <= 0 {
		return reqTarget{}, errors.New("rtsp: parse url: invalid")
	}
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority := rest[:end]
	authority = authority[strings.LastIndex(authority, "@")+1:]

	tg := reqTarget{track: -1}
	p := strings.TrimRight(u.Path, "/")
	seg := p[strings.LastIndex(p, "/")+1:]
	if len(seg) >= len(trackPrefix) && strings.EqualFold(seg[:len(trackPrefix)], trackPrefix) {
		n, err := strconv.ParseUint(seg[len(trackPrefix):], 10, 16)
		if err != nil {
			tg.badTrack = true
		} else {
			tg.track = int(n)
		}
		p = strings.TrimRight(p[:len(p)-len(seg)], "/")
	}
	tg.path = strings.TrimLeft(p, "/")
	base := "/"
	if tg.path != "" {
		base = (&url.URL{Path: "/" + tg.path + "/"}).EscapedPath()
	}
	tg.contentBase = raw[:i+3] + authority + base
	return tg, nil
}

// transportKind is a session's transport.
type transportKind int

const (
	transportNone transportKind = iota
	transportTCP
	transportUDP
)

// profileKind classifies a transport profile, case-insensitively.
func profileKind(protocol string) transportKind {
	switch strings.ToUpper(protocol) {
	case "RTP/AVP/TCP":
		return transportTCP
	case "RTP/AVP", "RTP/AVP/UDP":
		return transportUDP
	}
	return transportNone
}

// selectTransport returns the first offer the server supports: a unicast
// RTP/AVP over TCP, or over UDP with client ports when udpOK, in mode PLAY,
// with no destination but the peer, and of the session's transport once
// it has one.
func selectTransport(offers []Transport, peer net.IP, udpOK bool, current transportKind) (Transport, transportKind, bool) {
	for _, o := range offers {
		kind := profileKind(o.Protocol)
		switch {
		case kind == transportNone, o.Multicast:
			continue
		case o.Mode != "" && !strings.EqualFold(o.Mode, "PLAY"):
			continue
		case o.Destination != "" && !net.ParseIP(o.Destination).Equal(peer):
			continue
		case current != transportNone && kind != current:
			continue
		case kind == transportUDP && (!udpOK || o.ClientPort[0] == 0):
			continue
		}
		return o, kind, true
	}
	return Transport{}, transportNone, false
}

// transportReply renders the SETUP reply's Transport: the interleaved
// channels for TCP; the client's ports echoed with the server's pair for
// UDP; ssrc= for both.
func transportReply(kind transportKind, offer Transport, ch, serverPorts [2]int, ssrc uint32) string {
	r := Transport{Unicast: true, SSRC: ssrc, HasSSRC: true}
	switch kind {
	case transportTCP:
		r.Protocol, r.Interleaved = "RTP/AVP/TCP", ch
	case transportUDP, transportNone:
		r.Protocol, r.ClientPort, r.ServerPort = strings.ToUpper(offer.Protocol), offer.ClientPort, serverPorts
	}
	return r.String()
}
