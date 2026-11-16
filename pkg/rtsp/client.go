package rtsp

import (
	"bufio"
	"context"
	"crypto/rand"
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

	"github.com/pion/rtp"
)

// Client defaults applied when a Config field is zero.
const (
	defaultTimeout        = 10 * time.Second
	defaultIdleTimeout    = 10 * time.Second
	defaultReportInterval = 5 * time.Second
	defaultSessionTimeout = 60 * time.Second
	defaultMaxAU          = 8 << 20
	defaultPort           = "554"
	packetQueue           = 1024
	teardownWait          = 100 * time.Millisecond
	userAgent             = "open-streamer"
)

// ErrClosed is returned by ReadPacket after Close.
var ErrClosed = errors.New("rtsp: client closed")

// ErrIdle is returned by ReadPacket when no RTP packet arrived on any track
// for the idle timeout.
var ErrIdle = errors.New("rtsp: read: no packet within the idle timeout")

// Config tunes a Client. A zero field means its default.
type Config struct {
	// Timeout bounds the connect and every request/response (default 10 s).
	Timeout time.Duration
	// IdleTimeout ends the session when no RTP packet arrives on any track
	// for that long (default 10 s).
	IdleTimeout time.Duration
	// Transport is "tcp" (the default, interleaved), "udp" or "auto".
	Transport string
	Limits    Limits
	// MaxAU is the access-unit bound for the depacketizers the caller
	// builds (default 8 MiB). The client itself bounds only packets.
	MaxAU int
	// ReportInterval is the RTCP receiver report period per track
	// (default 5 s).
	ReportInterval time.Duration
	// Now stamps packet arrivals (default time.Now).
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	c.Transport = strings.ToLower(strings.TrimSpace(c.Transport))
	if c.Transport == "" {
		c.Transport = "tcp"
	}
	if c.MaxAU <= 0 {
		c.MaxAU = defaultMaxAU
	}
	if c.ReportInterval <= 0 {
		c.ReportInterval = defaultReportInterval
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Packet is one RTP or RTCP packet of a track; exactly one of RTP and RTCP
// is set. Track is the Media.Index. All slices are owned by the caller.
type Packet struct {
	Track   int
	RTP     *rtp.Packet
	RTCP    []byte
	Arrival time.Time
}

// ClientStats counts what the client has seen. Packet loss is not counted
// here: the caller's SeqTracker sees the gaps.
type ClientStats struct {
	// Resyncs counts bytes on the connection that were neither an
	// interleaved frame nor an RTSP message, skipped to the next frame or line.
	Resyncs uint64
	// UnknownChannels counts interleaved frames on channels no track uses.
	UnknownChannels uint64
	// ServerRequests counts requests the server sent to the client.
	ServerRequests uint64
	// DupPackets and Reordered are UDP counters.
	DupPackets, Reordered uint64
	// Dropped counts malformed RTP packets and, over UDP, packets dropped
	// because the queue was full.
	Dropped uint64
	// FallbackTCP counts automatic switches from UDP to TCP.
	FallbackTCP uint64
	// Reauth counts re-challenges after the first challenge, such as a 401
	// mid-session or a stale nonce; the 401 that starts authentication is
	// not counted.
	Reauth uint64
}

// StatusError is a non-success RTSP status. It names the method and the
// code only, never a URL or credential.
type StatusError struct {
	Method string
	Code   int
}

func (e *StatusError) Error() string {
	return "rtsp: " + strings.ToLower(e.Method) + ": status " + strconv.Itoa(e.Code)
}

// clientTrack is one set-up media and its transport. index, rtpCh and
// rtcpCh are fixed once SETUP returns; rx is guarded by Client.rrMu.
type clientTrack struct {
	index         int // Media.Index
	rtpCh, rtcpCh int
	rx            rxState
}

// rxState is the reception state of a track for receiver reports.
type rxState struct {
	has                 bool
	ssrc                uint32
	baseSeq, maxSeq     uint16
	cycles              uint32
	received            uint32
	expPrior, recvPrior uint32
	lsr                 uint32
	lsrAt               time.Time
}

// chanEntry maps an interleaved channel to a track.
type chanEntry struct {
	t    *clientTrack
	rtcp bool
}

// Client is a playing RTSP session. ReadPacket must be called from one
// goroutine; Stats, Description, RTPInfo and Close may be called from any.
type Client struct {
	cfg     Config
	conn    net.Conn
	br      *bufio.Reader
	auth    *Auth
	base    string // request URL, userinfo removed
	ctlBase string // base the SDP controls resolve against
	desc    Description
	info    []RTPInfo
	tracks  []*clientTrack
	byChan  atomic.Pointer[[256]chanEntry]
	ssrc    uint32 // our RTCP SSRC

	sessionTimeout time.Duration
	getParam       bool

	wmu sync.Mutex // serialises writes to conn

	mu         sync.Mutex // guards cseq, pending, session, challenged
	cseq       int
	pending    map[int]chan *Response
	session    string
	challenged bool

	rrMu sync.Mutex

	pkts      chan Packet
	done      chan struct{}
	err       error // set before done is closed
	failOnce  sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup

	epoch      time.Time
	lastPacket atomic.Int64 // nanoseconds since epoch
	blocked    atomic.Bool  // the reader waits on a full packet queue
	closeBy    atomic.Int64 // close deadline in Unix nanoseconds, 0 before Close

	nResyncs, nUnknown, nServerReqs, nDropped, nReauth atomic.Uint64
}

// Dial connects to an rtsp:// URL and starts playing: OPTIONS, DESCRIBE,
// SETUP for every supported media and PLAY. Userinfo in the URL is used for
// authentication and removed from everything sent or stored. ctx bounds only
// Dial.
func Dial(ctx context.Context, rawURL string, cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	switch cfg.Transport {
	case "tcp":
	case "udp", "auto":
		return nil, fmt.Errorf("rtsp: dial: transport %s not implemented yet", cfg.Transport)
	default:
		return nil, errors.New("rtsp: dial: unknown transport")
	}
	clean, user, pass, err := StripUserinfo(rawURL)
	if err != nil {
		return nil, errors.New("rtsp: dial: invalid URL")
	}
	u, err := url.Parse(clean)
	if err != nil || u.Host == "" {
		return nil, errors.New("rtsp: dial: invalid URL")
	}
	if !strings.EqualFold(u.Scheme, "rtsp") {
		return nil, errors.New("rtsp: dial: unsupported scheme")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	cancel()
	if err != nil {
		return nil, fmt.Errorf("rtsp: dial: %w", err)
	}
	c := &Client{
		cfg: cfg, conn: conn, br: bufio.NewReaderSize(conn, 64<<10), auth: NewAuth(user, pass),
		base: clean, pending: map[int]chan *Response{}, pkts: make(chan Packet, packetQueue),
		done: make(chan struct{}), epoch: time.Now(), ssrc: randomSSRC(),
	}
	c.byChan.Store(&[256]chanEntry{})
	c.wg.Add(1)
	go c.readLoop()
	if err := c.handshake(ctx); err != nil {
		if c.sessionID() != "" {
			// ctx may be what ended the handshake; the teardown has its
			// own 100 ms bound.
			c.teardown(context.WithoutCancel(ctx))
		}
		c.fail(err)
		c.wg.Wait()
		return nil, err
	}
	c.touch()
	c.wg.Add(1)
	go c.timerLoop()
	return c, nil
}

func randomSSRC() uint32 {
	var b [4]byte
	// Since Go 1.24 crypto/rand.Read does not return an error.
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func (c *Client) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

func ok2xx(r *Response) bool { return r.StatusCode >= 200 && r.StatusCode < 300 }

// handshake runs OPTIONS, DESCRIBE, SETUP and PLAY. Its waits are bounded by
// ctx and Timeout and are not paused while the reader is blocked on a full
// packet queue: a server that sends more than 1024 interleaved frames before
// the PLAY response makes Dial time out.
func (c *Client) handshake(ctx context.Context) error {
	resp, err := c.request(ctx, "OPTIONS", c.base, nil)
	if err != nil {
		var se *StatusError
		if !errors.As(err, &se) {
			return err
		}
		// A second 401 on OPTIONS: DESCRIBE will fail the same way if it
		// matters; some servers only guard DESCRIBE.
	} else if ok2xx(resp) {
		c.getParam = publicHas(resp.Header, "GET_PARAMETER")
	}

	resp, err = c.request(ctx, "DESCRIBE", c.base, Header{"Accept": {"application/sdp"}})
	if err != nil {
		return err
	}
	if !ok2xx(resp) {
		return &StatusError{Method: "DESCRIBE", Code: resp.StatusCode}
	}
	c.ctlBase = c.base
	if cb := resp.Header.Get("Content-Base"); hasScheme(cb) {
		c.ctlBase = stripUserinfoString(cb)
	} else if cl := resp.Header.Get("Content-Location"); hasScheme(cl) {
		c.ctlBase = stripUserinfoString(cl)
	}
	desc, err := ParseSDP(resp.Body, c.ctlBase)
	if err != nil {
		return err
	}
	c.desc = desc
	for _, m := range desc.Media {
		if m.Reason != "" {
			continue
		}
		if err := c.setup(ctx, m); err != nil {
			return err
		}
	}
	if len(c.tracks) == 0 {
		return errors.New("rtsp: dial: no supported media")
	}

	resp, err = c.request(ctx, "PLAY", c.desc.Control, Header{"Range": {"npt=0.000-"}})
	if err != nil {
		return err
	}
	if !ok2xx(resp) {
		return &StatusError{Method: "PLAY", Code: resp.StatusCode}
	}
	c.info = mapRTPInfo(ParseRTPInfo(resp.Header.Get("RTP-Info")), c.desc, c.ctlBase)
	return nil
}

func publicHas(h Header, method string) bool {
	for _, v := range h["Public"] {
		for _, m := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(m), method) {
				return true
			}
		}
	}
	return false
}

// setup sets up one media over TCP interleaved; the channels come from the
// reply when it names them.
func (c *Client) setup(ctx context.Context, m Media) error {
	want := [2]int{2 * m.Index, 2*m.Index + 1}
	tr := Transport{Protocol: "RTP/AVP/TCP", Unicast: true, Interleaved: want}
	resp, err := c.request(ctx, "SETUP", m.Control, Header{"Transport": {tr.String()}})
	if err != nil {
		return err
	}
	if !ok2xx(resp) {
		return &StatusError{Method: "SETUP", Code: resp.StatusCode}
	}
	if c.sessionID() == "" {
		s, err := ParseSession(resp.Header.Get("Session"))
		if err != nil {
			return fmt.Errorf("rtsp: setup: %w", err)
		}
		c.mu.Lock()
		c.session = s.ID
		c.mu.Unlock()
		c.sessionTimeout = s.Timeout
	}
	ch := want
	if rt, err := ParseTransport(resp.Header.Get("Transport")); err == nil && rt.Interleaved != [2]int{} {
		ch = rt.Interleaved
	}
	table := *c.byChan.Load()
	if ch[0] == ch[1] || ch[0] > 255 || ch[1] > 255 || table[ch[0]].t != nil || table[ch[1]].t != nil {
		return errors.New("rtsp: setup: server chose unusable interleaved channels")
	}
	t := &clientTrack{index: m.Index, rtpCh: ch[0], rtcpCh: ch[1]}
	table[ch[0]] = chanEntry{t: t}
	table[ch[1]] = chanEntry{t: t, rtcp: true}
	c.byChan.Store(&table)
	c.tracks = append(c.tracks, t)
	return nil
}

// mapRTPInfo returns one RTPInfo per media, matched by URL (exactly, then by
// the last path segment), else by order when nothing matched and the counts
// agree. Unknown entries have HasSeq and HasTime false.
func mapRTPInfo(infos []RTPInfo, d Description, base string) []RTPInfo {
	out := make([]RTPInfo, len(d.Media))
	var setUp []int
	for i, m := range d.Media {
		out[i].URL = m.Control
		if m.Reason == "" {
			setUp = append(setUp, i)
		}
	}
	norm := func(s string) string {
		if !hasScheme(s) {
			s = ResolveControl(base, s)
		}
		return strings.TrimRight(stripUserinfoString(s), "/")
	}
	used := make([]bool, len(infos))
	matched := make([]bool, len(d.Media))
	assign := func(i, j int) {
		out[i] = infos[j]
		out[i].URL = d.Media[i].Control
		used[j], matched[i] = true, true
	}
	for _, rule := range []func(ctl, info string) bool{
		func(ctl, info string) bool { return norm(ctl) == norm(info) },
		func(ctl, info string) bool {
			info = strings.TrimRight(info, "/")
			seg := info[strings.LastIndex(info, "/")+1:]
			return seg != "" && strings.HasSuffix(norm(ctl), "/"+seg)
		},
	} {
		for _, i := range setUp {
			for j := range infos {
				if !matched[i] && !used[j] && rule(d.Media[i].Control, infos[j].URL) {
					assign(i, j)
				}
			}
		}
	}
	matchedAny := false
	for _, u := range used {
		matchedAny = matchedAny || u
	}
	if !matchedAny && len(infos) == len(setUp) {
		for j, i := range setUp {
			assign(i, j)
		}
	}
	return out
}

// request sends a request and waits for its response. A 401 re-challenges
// and retries once; a second 401 is a StatusError. Other statuses are
// returned to the caller.
func (c *Client) request(ctx context.Context, method, uri string, extra Header) (*Response, error) {
	for attempt := 0; ; attempt++ {
		ch, cs, err := c.send(method, uri, extra, time.Now().Add(c.cfg.Timeout))
		if err != nil {
			return nil, err
		}
		resp, err := c.await(ctx, method, ch, cs, c.cfg.Timeout)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 401 {
			return resp, nil
		}
		if attempt > 0 {
			return nil, &StatusError{Method: method, Code: resp.StatusCode}
		}
		if err := c.rechallenge(resp.Header); err != nil {
			return nil, fmt.Errorf("rtsp: %s: status 401: %w", strings.ToLower(method), err)
		}
	}
}

// rechallenge records a 401 challenge; every challenge after the first is
// counted as a re-authentication.
func (c *Client) rechallenge(h Header) error {
	if err := c.auth.Challenge(h); err != nil {
		return err
	}
	c.mu.Lock()
	again := c.challenged
	c.challenged = true
	c.mu.Unlock()
	if again {
		c.nReauth.Add(1)
	}
	return nil
}

// send writes a request and registers its CSeq; the response arrives on the
// returned channel.
func (c *Client) send(method, uri string, extra Header, deadline time.Time) (chan *Response, int, error) {
	req, ch, cs := c.prepare(method, uri, extra)
	if err := c.writeMsg(req, deadline); err != nil {
		c.forget(cs)
		return nil, 0, fmt.Errorf("rtsp: %s: %w", strings.ToLower(method), err)
	}
	return ch, cs, nil
}

// prepare builds a request with its CSeq, Session and Authorization and
// registers the CSeq for the response.
func (c *Client) prepare(method, uri string, extra Header) (*Request, chan *Response, int) {
	ch := make(chan *Response, 1)
	c.mu.Lock()
	c.cseq++
	cs := c.cseq
	c.pending[cs] = ch
	session := c.session
	c.mu.Unlock()

	h := Header{}
	for k, v := range extra {
		h[k] = append([]string(nil), v...)
	}
	h.Set("CSeq", strconv.Itoa(cs))
	h.Set("User-Agent", userAgent)
	if session != "" {
		h.Set("Session", session)
	}
	if a := c.auth.Authorize(method, uri); a != "" {
		h.Set("Authorization", a)
	}
	return &Request{Method: method, URL: uri, Header: h}, ch, cs
}

func (c *Client) forget(cs int) {
	c.mu.Lock()
	delete(c.pending, cs)
	c.mu.Unlock()
}

func (c *Client) await(ctx context.Context, method string, ch chan *Response, cs int, timeout time.Duration) (*Response, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		c.forget(cs)
		return nil, fmt.Errorf("rtsp: %s: %w", strings.ToLower(method), ctx.Err())
	case <-t.C:
		c.forget(cs)
		return nil, fmt.Errorf("rtsp: %s: no response within %v", strings.ToLower(method), timeout)
	case <-c.done:
		return nil, c.err
	}
}

// message is a Request or a Response.
type message interface{ Write(w io.Writer) error }

// writeMsg writes a request or a response under the write mutex.
func (c *Client) writeMsg(m message, deadline time.Time) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(c.clampDeadline(deadline))
	return m.Write(c.conn)
}

// clampDeadline shortens a write deadline to the close deadline once Close
// has started, so no write outlives the 100 ms teardown bound.
func (c *Client) clampDeadline(d time.Time) time.Time {
	if n := c.closeBy.Load(); n != 0 {
		if cb := time.Unix(0, n); cb.Before(d) {
			return cb
		}
	}
	return d
}

// lockWriteBy takes the write mutex by the deadline. While it waits it
// keeps cutting the current writer's deadline to it, which ends a write
// stuck on a full send buffer.
func (c *Client) lockWriteBy(deadline time.Time) bool {
	for !c.wmu.TryLock() {
		if !time.Now().Before(deadline) {
			return false
		}
		_ = c.conn.SetWriteDeadline(deadline)
		time.Sleep(time.Millisecond)
	}
	return true
}

// writeFrame writes one interleaved frame under the write mutex.
func (c *Client) writeFrame(ch int, data []byte) error {
	b := make([]byte, 4+len(data))
	b[0], b[1] = '$', byte(ch)
	binary.BigEndian.PutUint16(b[2:], uint16(len(data)))
	copy(b[4:], data)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(c.clampDeadline(time.Now().Add(c.cfg.Timeout)))
	if _, err := c.conn.Write(b); err != nil {
		return fmt.Errorf("rtsp: write rtcp: %w", err)
	}
	return nil
}

// fail ends the session with err: the first call wins, closes the
// connection and wakes every waiter.
func (c *Client) fail(err error) {
	c.failOnce.Do(func() {
		c.err = err
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *Client) touch() { c.lastPacket.Store(int64(time.Since(c.epoch))) }

// teardown sends TEARDOWN and waits for the answer, all within 100 ms:
// later writes are clamped to that deadline and a writer holding the write
// mutex is cut short; if the mutex is still not free, TEARDOWN is skipped.
func (c *Client) teardown(ctx context.Context) {
	deadline := time.Now().Add(teardownWait)
	c.closeBy.Store(deadline.UnixNano())
	req, ch, cs := c.prepare("TEARDOWN", c.desc.Control, nil)
	if !c.lockWriteBy(deadline) {
		c.forget(cs)
		return
	}
	_ = c.conn.SetWriteDeadline(deadline)
	err := req.Write(c.conn)
	c.wmu.Unlock()
	if err != nil {
		c.forget(cs)
		return
	}
	_, _ = c.await(ctx, "TEARDOWN", ch, cs, time.Until(deadline))
}

// Description returns the parsed SDP. The result is a deep copy.
func (c *Client) Description() Description {
	d := Description{Control: c.desc.Control, Media: make([]Media, len(c.desc.Media))}
	for i, m := range c.desc.Media {
		if m.ParamSets != nil {
			ps := make([][]byte, len(m.ParamSets))
			for j, p := range m.ParamSets {
				ps[j] = append([]byte(nil), p...)
			}
			m.ParamSets = ps
		}
		if m.AAC != nil {
			a := *m.AAC
			a.Config = append([]byte(nil), a.Config...)
			m.AAC = &a
		}
		d.Media[i] = m
	}
	return d
}

// RTPInfo returns the PLAY response's RTP-Info, one entry per Media in
// Description order; entries the server did not send have HasSeq and
// HasTime false.
func (c *Client) RTPInfo() []RTPInfo { return append([]RTPInfo(nil), c.info...) }

// Stats returns the counters.
func (c *Client) Stats() ClientStats {
	return ClientStats{
		Resyncs:         c.nResyncs.Load(),
		UnknownChannels: c.nUnknown.Load(),
		ServerRequests:  c.nServerReqs.Load(),
		Dropped:         c.nDropped.Load(),
		Reauth:          c.nReauth.Load(),
	}
}

// ReadPacket returns the next packet. After the session ends it returns the
// packets still queued, then the error that ended it (ErrClosed after
// Close).
func (c *Client) ReadPacket() (Packet, error) {
	if c.closed.Load() {
		return Packet{}, ErrClosed
	}
	select {
	case p := <-c.pkts:
		return p, nil
	case <-c.done:
		select {
		case p := <-c.pkts:
			if !c.closed.Load() {
				return p, nil
			}
		default:
		}
		return Packet{}, c.err
	}
}

// Close sends TEARDOWN (waiting at most 100 ms), closes the connection and
// waits for the client's goroutines. It is idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		select {
		case <-c.done:
		default:
			c.teardown(context.Background())
		}
		c.fail(ErrClosed)
		c.wg.Wait()
	})
	return nil
}
