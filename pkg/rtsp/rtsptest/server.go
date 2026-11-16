// Package rtsptest provides a scripted RTSP server for tests. It serves the
// given access units over TCP interleaved RTP in real time, with switches for
// the authentication, control-URL, channel, keepalive and framing quirks of
// real cameras. It imports nothing from internal/.
package rtsptest

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // MD5 is the HTTP Digest hash defined by RFC 2617; no security claim rests on it.
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

const (
	realm        = "rtsptest"
	mtu          = 1200
	writeTimeout = 2 * time.Second
	// garbageAfter is the number of packets sent before the Garbage
	// injection; earlyFrames is how many go out before the PLAY response.
	garbageAfter = 40
	earlyFrames  = 3
	unknownChan  = 99
)

// TestAU is one access unit of a track: NAL units (no start codes) for video,
// AAC frames (no ADTS headers) or raw payloads for audio.
type TestAU struct {
	// TS is the RTP timestamp in the track clock, counted from 0.
	TS     uint32
	NALUs  [][]byte
	Frames [][]byte
}

// TrackSource is one media the server offers. Its AUs are looped forever.
// CodecOther is offered as PCMA/8000, each frame one packet.
type TrackSource struct {
	Codec     rtsp.Codec
	ParamSets [][]byte
	AAC       *rtsp.AACFormat
	ClockRate uint32
	AUs       []TestAU
}

// ServerOptions selects the server's behaviour.
type ServerOptions struct {
	// Auth is "", "basic", "digest" (qop=auth) or "digest-noqop". Every
	// request must be authorised.
	Auth       string
	User, Pass string
	// NonceTTL > 0 expires a Digest nonce that long after it was issued:
	// later requests with it get 401 with stale=true.
	NonceTTL time.Duration
	// ControlStyle is "hik" (Content-Base with a trailing slash, trackID=1..),
	// "dahua" (the same with trackID=0..) or "absolute" (no Content-Base,
	// absolute controls under /media). "" means "hik".
	ControlStyle string
	// ContentBaseUserinfo puts User:Pass@ into the Content-Base.
	ContentBaseUserinfo bool
	// Channels overrides the interleaved RTP channel of the i-th SETUP
	// (RTCP is the next one).
	Channels []int
	// SessionTimeout > 0 is announced in the Session header; a session with
	// no RTSP request for 1.5 times it is dropped.
	SessionTimeout time.Duration
	// NoGetParameter leaves GET_PARAMETER out of Public and answers it 501.
	NoGetParameter bool
	// SendRTPInfo adds RTP-Info to the PLAY response.
	SendRTPInfo bool
	// Garbage sends the first frames before the PLAY response, then after a
	// while a stray CRLF, junk bytes, a frame on an unknown channel and three
	// requests (GET_PARAMETER, SET_PARAMETER, ANNOUNCE).
	Garbage bool
	// SRAfter > 0 sends an RTCP sender report per track every SRAfter, the
	// first SRAfter after PLAY.
	SRAfter time.Duration
	// SRJump shifts the NTP time of the last track's sender reports by
	// SRJump from the second report on.
	SRJump time.Duration
	// SSRCChangeAfter > 0 restarts every track once that long after PLAY:
	// a new SSRC and a new sequence base, as a rebooting camera does.
	SSRCChangeAfter time.Duration
	// KeepaliveStatus, when set, answers keepalives (GET_PARAMETER, and
	// OPTIONS carrying a Session) with this status.
	KeepaliveStatus int
	// UDP options; UDP is not served yet (SETUP over UDP is answered 461).
	DropUDP                  bool
	UDPLoss, UDPSwap, UDPDup float64
	// Seed seeds the session's random sequence numbers, timestamps and SSRCs.
	Seed  int64
	Media []TrackSource
}

// Frame is an interleaved frame received from a client.
type Frame struct {
	Channel int
	Data    []byte
}

// ClientResponse is a client's answer to a server-initiated request.
type ClientResponse struct {
	Method     string
	StatusCode int
}

// Server is a running test server.
type Server struct {
	// Addr is the host:port the server listens on.
	Addr string

	t      testing.TB
	opt    ServerOptions
	ln     net.Listener
	tracks []*track
	wg     sync.WaitGroup
	done   chan struct{}

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	requests  []rtsp.Request
	frames    []Frame
	responses []ClientResponse
	nonces    map[string]time.Time
	closed    bool
	sessions  int
	closeOnce sync.Once
}

// track is a media's prepared packets.
type track struct {
	src    TrackSource
	pt     uint8
	period uint32 // loop length in track clock
	aus    []preparedAU
}

type preparedAU struct {
	ts   uint32
	pkts []prepared
}

type prepared struct {
	tsOff   uint32
	marker  bool
	payload []byte
}

// NewServer starts a server on 127.0.0.1 and closes it when the test ends.
func NewServer(t testing.TB, opt ServerOptions) *Server {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("rtsptest: listen: %v", err)
	}
	s := &Server{
		Addr: ln.Addr().String(), t: t, opt: opt, ln: ln, done: make(chan struct{}),
		conns: map[net.Conn]struct{}{}, nonces: map[string]time.Time{},
	}
	for i, m := range opt.Media {
		s.tracks = append(s.tracks, prepareTrack(t, i, m))
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// URL returns rtsp://Addr followed by path.
func (s *Server) URL(path string) string { return "rtsp://" + s.Addr + path }

// Close stops the server and waits for its goroutines. It is idempotent.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.ln.Close()
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
}

// Requests returns the requests received so far, in order.
func (s *Server) Requests() []rtsp.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rtsp.Request(nil), s.requests...)
}

// FromClient returns the interleaved frames received from clients.
func (s *Server) FromClient() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Frame(nil), s.frames...)
}

// ClientResponses returns the clients' answers to server-initiated requests.
func (s *Server) ClientResponses() []ClientResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ClientResponse(nil), s.responses...)
}

func prepareTrack(t testing.TB, i int, m TrackSource) *track {
	tr := &track{src: m, pt: uint8(96 + i)}
	if m.Codec == rtsp.CodecOther {
		tr.pt = 8
	}
	var h264 codecs.H264Payloader
	var h265 codecs.H265Payloader
	var maxTS uint32
	for _, au := range m.AUs {
		maxTS = max(maxTS, au.TS)
		p := preparedAU{ts: au.TS}
		switch m.Codec {
		case rtsp.CodecH264, rtsp.CodecH265:
			var ab []byte
			for _, n := range au.NALUs {
				ab = append(ab, 0, 0, 0, 1)
				ab = append(ab, n...)
			}
			var pls [][]byte
			if m.Codec == rtsp.CodecH264 {
				pls = h264.Payload(mtu, ab)
			} else {
				pls = h265.Payload(mtu, ab)
			}
			for j, pl := range pls {
				p.pkts = append(p.pkts, prepared{marker: j == len(pls)-1, payload: pl})
			}
		case rtsp.CodecAAC:
			if m.AAC == nil {
				t.Fatalf("rtsptest: track %d: AAC without a format", i)
			}
			for _, pk := range rtsp.PacketizeAAC(au.Frames, 0, 1024, mtu, *m.AAC) {
				p.pkts = append(p.pkts, prepared{tsOff: pk.Timestamp, marker: pk.Marker, payload: pk.Payload})
			}
		case rtsp.CodecOther:
			for j, fr := range au.Frames {
				p.pkts = append(p.pkts, prepared{tsOff: uint32(j) * uint32(len(fr)), marker: true, payload: fr})
			}
		}
		tr.aus = append(tr.aus, p)
	}
	switch {
	case len(m.AUs) > 1:
		tr.period = maxTS + maxTS/uint32(len(m.AUs)-1)
	case m.ClockRate > 0:
		tr.period = m.ClockRate / 25
	default:
		tr.period = 3600
	}
	return tr
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.sessions++
		n := s.sessions
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(c, n)
	}
}

// session is one client connection.
type session struct {
	s    *Server
	conn net.Conn
	br   *bufio.Reader
	rng  *rand.Rand

	wmu sync.Mutex

	id       string
	reqURL   string // DESCRIBE URL
	base     string // Content-Base, userinfo removed ("" for absolute)
	setup    []int  // track index per SETUP
	chans    []int  // RTP channel per track (-1 not set up)
	seq0     []uint16
	ts0      []uint32
	ssrc     []uint32
	playing  bool
	stop     chan struct{}
	stopOnce sync.Once
	lastReq  time.Time
	serverCS int
	pending  map[int]string // server request CSeq → method
	wg       sync.WaitGroup
}

func (s *Server) serve(c net.Conn, n int) {
	defer s.wg.Done()
	seed := uint64(s.opt.Seed)*1000 + uint64(n)
	ss := &session{
		s: s, conn: c, br: bufio.NewReader(c), rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b9)),
		stop: make(chan struct{}), lastReq: time.Now(), pending: map[int]string{}, serverCS: 1000,
	}
	ss.chans = make([]int, len(s.tracks))
	for i := range ss.chans {
		ss.chans[i] = -1
		ss.seq0 = append(ss.seq0, uint16(ss.rng.Uint32()))
		ss.ts0 = append(ss.ts0, ss.rng.Uint32())
		ss.ssrc = append(ss.ssrc, ss.rng.Uint32())
	}
	ss.id = fmt.Sprintf("%08X", ss.rng.Uint32())
	ss.loop()
	ss.stopOnce.Do(func() { close(ss.stop) })
	_ = c.Close()
	ss.wg.Wait()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (ss *session) loop() {
	for {
		if t := ss.s.opt.SessionTimeout; t > 0 && ss.playing {
			_ = ss.conn.SetReadDeadline(ss.lastReq.Add(t * 3 / 2))
		}
		b, err := ss.br.Peek(1)
		if err != nil {
			return
		}
		switch b[0] {
		case '$':
			if !ss.readFrame() {
				return
			}
		case '\r', '\n':
			_, _ = ss.br.ReadByte()
		default:
			head, _ := ss.br.Peek(8)
			if string(head) == "RTSP/1.0" {
				resp, err := rtsp.ReadResponse(ss.br, rtsp.Limits{})
				if err != nil {
					return
				}
				cs, _ := strconv.Atoi(resp.Header.Get("CSeq"))
				ss.s.mu.Lock()
				ss.s.responses = append(ss.s.responses, ClientResponse{Method: ss.pending[cs], StatusCode: resp.StatusCode})
				ss.s.mu.Unlock()
				continue
			}
			req, err := rtsp.ReadRequest(ss.br, rtsp.Limits{})
			if err != nil {
				return
			}
			ss.lastReq = time.Now()
			if !ss.handle(req) {
				return
			}
		}
	}
}

func (ss *session) readFrame() bool {
	var h [4]byte
	if _, err := io.ReadFull(ss.br, h[:]); err != nil {
		return false
	}
	data := make([]byte, binary.BigEndian.Uint16(h[2:]))
	if _, err := io.ReadFull(ss.br, data); err != nil {
		return false
	}
	ss.s.mu.Lock()
	ss.s.frames = append(ss.s.frames, Frame{Channel: int(h[1]), Data: data})
	ss.s.mu.Unlock()
	return true
}

func (ss *session) write(b []byte) error {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	_ = ss.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := ss.conn.Write(b)
	return err
}

func (ss *session) respond(req *rtsp.Request, code int, h rtsp.Header, body []byte) bool {
	if h == nil {
		h = rtsp.Header{}
	}
	h.Set("CSeq", req.Header.Get("CSeq"))
	resp := rtsp.Response{StatusCode: code, Reason: reason(code), Header: h, Body: body}
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	_ = ss.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return resp.Write(ss.conn) == nil
}

func reason(code int) string {
	switch code {
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 454:
		return "Session Not Found"
	case 461:
		return "Unsupported Transport"
	case 501:
		return "Not Implemented"
	}
	return "Error"
}

// handle answers one request; false closes the connection.
func (ss *session) handle(req *rtsp.Request) bool {
	ss.s.mu.Lock()
	ss.s.requests = append(ss.s.requests, rtsp.Request{Method: req.Method, URL: req.URL, Header: req.Header})
	ss.s.mu.Unlock()
	if req.Header.Get("CSeq") == "" {
		return ss.respond(req, 400, nil, nil)
	}
	if h, ok := ss.s.authorize(req); !ok {
		return ss.respond(req, 401, h, nil)
	}
	if code := ss.s.opt.KeepaliveStatus; code != 0 &&
		(req.Method == "GET_PARAMETER" || req.Method == "OPTIONS" && req.Header.Get("Session") != "") {
		return ss.respond(req, code, nil, nil)
	}
	switch req.Method {
	case "OPTIONS":
		pub := "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN"
		if !ss.s.opt.NoGetParameter {
			pub += ", GET_PARAMETER"
		}
		return ss.respond(req, 200, rtsp.Header{"Public": {pub}}, nil)
	case "DESCRIBE":
		return ss.describe(req)
	case "SETUP":
		return ss.setupTrack(req)
	case "PLAY":
		return ss.play(req)
	case "GET_PARAMETER":
		if ss.s.opt.NoGetParameter {
			return ss.respond(req, 501, nil, nil)
		}
		if !ss.checkSession(req) {
			return ss.respond(req, 454, nil, nil)
		}
		return ss.respond(req, 200, ss.sessionHeader(), nil)
	case "TEARDOWN":
		ss.respond(req, 200, nil, nil)
		return false
	}
	return ss.respond(req, 501, nil, nil)
}

func (ss *session) style() string {
	if ss.s.opt.ControlStyle == "" {
		return "hik"
	}
	return ss.s.opt.ControlStyle
}

// controls returns the session control attribute, each track's control
// attribute and the absolute URLs they resolve to.
func (ss *session) controls() (sessionAttr string, trackAttrs, trackURLs []string, playURLs []string) {
	switch ss.style() {
	case "absolute":
		root := "rtsp://" + ss.s.Addr + "/media"
		for i := range ss.s.tracks {
			u := root + "/track" + strconv.Itoa(i)
			trackAttrs = append(trackAttrs, u)
			trackURLs = append(trackURLs, u)
		}
		return root, trackAttrs, trackURLs, []string{root}
	default:
		first := 1
		if ss.style() == "dahua" {
			first = 0
		}
		for i := range ss.s.tracks {
			a := "trackID=" + strconv.Itoa(i+first)
			trackAttrs = append(trackAttrs, a)
			trackURLs = append(trackURLs, ss.base+a)
		}
		return "*", trackAttrs, trackURLs, []string{ss.base, strings.TrimSuffix(ss.base, "/"), ss.reqURL}
	}
}

func (ss *session) describe(req *rtsp.Request) bool {
	ss.reqURL = req.URL
	h := rtsp.Header{"Content-Type": {"application/sdp"}}
	if ss.style() != "absolute" {
		ss.base = strings.TrimSuffix(req.URL, "/") + "/"
		cb := ss.base
		if ss.s.opt.ContentBaseUserinfo {
			cb = strings.Replace(cb, "rtsp://", "rtsp://"+ss.s.opt.User+":"+ss.s.opt.Pass+"@", 1)
		}
		h.Set("Content-Base", cb)
	}
	sessAttr, trackAttrs, _, _ := ss.controls()
	var sb strings.Builder
	sb.WriteString("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=rtsptest\r\nc=IN IP4 0.0.0.0\r\nt=0 0\r\n")
	sb.WriteString("a=control:" + sessAttr + "\r\n")
	for i, tr := range ss.s.tracks {
		sb.WriteString(mediaSDP(tr))
		sb.WriteString("a=control:" + trackAttrs[i] + "\r\n")
	}
	return ss.respond(req, 200, h, []byte(sb.String()))
}

func mediaSDP(tr *track) string {
	pt := strconv.Itoa(int(tr.pt))
	m := tr.src
	switch m.Codec {
	case rtsp.CodecH264:
		sets := make([]string, 0, len(m.ParamSets))
		for _, p := range m.ParamSets {
			sets = append(sets, base64.StdEncoding.EncodeToString(p))
		}
		return "m=video 0 RTP/AVP " + pt + "\r\na=rtpmap:" + pt + " H264/90000\r\n" +
			"a=fmtp:" + pt + " packetization-mode=1;sprop-parameter-sets=" + strings.Join(sets, ",") + "\r\n"
	case rtsp.CodecH265:
		var vps, sps, pps []string
		for _, p := range m.ParamSets {
			if len(p) == 0 {
				continue
			}
			e := base64.StdEncoding.EncodeToString(p)
			switch (p[0] >> 1) & 0x3f {
			case 32:
				vps = append(vps, e)
			case 33:
				sps = append(sps, e)
			case 34:
				pps = append(pps, e)
			}
		}
		return "m=video 0 RTP/AVP " + pt + "\r\na=rtpmap:" + pt + " H265/90000\r\n" +
			"a=fmtp:" + pt + " sprop-vps=" + strings.Join(vps, ",") + ";sprop-sps=" + strings.Join(sps, ",") +
			";sprop-pps=" + strings.Join(pps, ",") + "\r\n"
	case rtsp.CodecAAC:
		f := m.AAC
		return "m=audio 0 RTP/AVP " + pt + "\r\na=rtpmap:" + pt + " mpeg4-generic/" + strconv.Itoa(int(m.ClockRate)) + "\r\n" +
			"a=fmtp:" + pt + " streamtype=5;profile-level-id=1;mode=AAC-hbr;sizelength=" + strconv.Itoa(f.SizeLength) +
			";indexlength=" + strconv.Itoa(f.IndexLength) + ";indexdeltalength=" + strconv.Itoa(f.IndexDeltaLength) +
			";config=" + hex.EncodeToString(f.Config) + "\r\n"
	case rtsp.CodecOther:
		return "m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"
	}
	return ""
}

func (ss *session) sessionHeader() rtsp.Header {
	v := ss.id
	if t := ss.s.opt.SessionTimeout; t > 0 {
		v += ";timeout=" + strconv.Itoa(int(t/time.Second))
	}
	return rtsp.Header{"Session": {v}}
}

func (ss *session) checkSession(req *rtsp.Request) bool {
	id, _, _ := strings.Cut(req.Header.Get("Session"), ";")
	return strings.TrimSpace(id) == ss.id
}

func (ss *session) setupTrack(req *rtsp.Request) bool {
	_, _, urls, _ := ss.controls()
	idx := -1
	for i, u := range urls {
		if u == req.URL {
			idx = i
		}
	}
	if idx < 0 || ss.chans[idx] >= 0 || ss.playing {
		return ss.respond(req, 404, nil, nil)
	}
	if len(ss.setup) > 0 && !ss.checkSession(req) {
		return ss.respond(req, 454, nil, nil)
	}
	tr, err := rtsp.ParseTransport(req.Header.Get("Transport"))
	if err != nil || tr.Protocol != "RTP/AVP/TCP" {
		return ss.respond(req, 461, nil, nil)
	}
	ch := tr.Interleaved[0]
	if n := len(ss.setup); n < len(ss.s.opt.Channels) {
		ch = ss.s.opt.Channels[n]
	}
	ss.chans[idx] = ch
	ss.setup = append(ss.setup, idx)
	reply := rtsp.Transport{Protocol: "RTP/AVP/TCP", Unicast: true, Interleaved: [2]int{ch, ch + 1}, SSRC: ss.ssrc[idx], HasSSRC: true}
	h := ss.sessionHeader()
	h.Set("Transport", reply.String())
	return ss.respond(req, 200, h, nil)
}

func (ss *session) play(req *rtsp.Request) bool {
	_, _, urls, plays := ss.controls()
	ok := false
	for _, u := range plays {
		ok = ok || u == req.URL
	}
	switch {
	case !ok:
		return ss.respond(req, 404, nil, nil)
	case !ss.checkSession(req) || len(ss.setup) == 0:
		return ss.respond(req, 454, nil, nil)
	case ss.playing:
		return ss.respond(req, 200, ss.sessionHeader(), nil)
	}
	h := ss.sessionHeader()
	h.Set("Range", "npt=0.000-")
	if ss.s.opt.SendRTPInfo {
		var parts []string
		for _, i := range ss.setup {
			parts = append(parts, fmt.Sprintf("url=%s;seq=%d;rtptime=%d", urls[i], ss.seq0[i], ss.ts0[i]))
		}
		h.Set("RTP-Info", strings.Join(parts, ","))
	}
	ss.playing = true
	resp := func() bool { return ss.respond(req, 200, h, nil) }
	if !ss.s.opt.Garbage {
		if !resp() {
			return false
		}
		resp = nil
	}
	ss.wg.Add(1)
	go ss.stream(resp)
	return true
}

// sendState is one track's position in the stream.
type sendState struct {
	track  int
	au     int
	loop   uint32
	seq    uint16
	at     time.Duration // send time of the next AU from the start
	srSent int
	octets uint32
	pkts   uint32
}

// stream sends the set-up tracks in real time. When playResp is set it is
// called after the first frames (the Garbage case).
func (ss *session) stream(playResp func() bool) {
	defer ss.wg.Done()
	start := time.Now()
	var states []*sendState
	for _, i := range ss.setup {
		if len(ss.s.tracks[i].aus) > 0 {
			states = append(states, &sendState{track: i, seq: ss.seq0[i]})
		}
	}
	sent := 0
	injected, restarted := false, false
	nextSR := ss.s.opt.SRAfter
	for {
		if playResp == nil && len(states) == 0 {
			select {
			case <-ss.stop:
				return
			case <-ss.s.done:
				return
			}
		}
		var next *sendState
		for _, st := range states {
			if next == nil || st.at < next.at {
				next = st
			}
		}
		wait := time.Duration(0)
		if next != nil {
			wait = time.Until(start.Add(next.at))
		}
		if nextSR > 0 && (next == nil || nextSR < next.at) {
			wait = time.Until(start.Add(nextSR))
		}
		if wait > 0 && playResp == nil {
			select {
			case <-ss.stop:
				return
			case <-ss.s.done:
				return
			case <-time.After(wait):
			}
		}
		if nextSR > 0 && time.Since(start) >= nextSR {
			if !ss.sendSRs(start, states) {
				return
			}
			nextSR += ss.s.opt.SRAfter
			continue
		}
		if next == nil {
			if playResp != nil && !playResp() {
				return
			}
			playResp = nil
			continue
		}
		if d := ss.s.opt.SSRCChangeAfter; d > 0 && !restarted && time.Since(start) >= d {
			restarted = true
			for _, st := range states {
				ss.ssrc[st.track] = ss.rng.Uint32()
				st.seq = uint16(ss.rng.Uint32())
			}
		}
		n, ok := ss.sendAU(next)
		if !ok {
			return
		}
		sent += n
		if playResp != nil && sent >= earlyFrames {
			if !playResp() {
				return
			}
			playResp = nil
		}
		if ss.s.opt.Garbage && !injected && sent >= garbageAfter {
			injected = true
			if !ss.inject() {
				return
			}
		}
	}
}

func (ss *session) frame(ch int, data []byte) []byte {
	b := make([]byte, 4, 4+len(data))
	b[0], b[1] = '$', byte(ch)
	binary.BigEndian.PutUint16(b[2:], uint16(len(data)))
	return append(b, data...)
}

// sendAU sends the next AU of st and advances it.
func (ss *session) sendAU(st *sendState) (int, bool) {
	tr := ss.s.tracks[st.track]
	au := tr.aus[st.au]
	base := ss.ts0[st.track] + st.loop*tr.period + au.ts
	var buf []byte
	for _, p := range au.pkts {
		pkt := rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: tr.pt, SequenceNumber: st.seq, Timestamp: base + p.tsOff,
			SSRC: ss.ssrc[st.track], Marker: p.marker,
		}, Payload: p.payload}
		raw, err := pkt.Marshal()
		if err != nil {
			return 0, false
		}
		buf = append(buf, ss.frame(ss.chans[st.track], raw)...)
		st.seq++
		st.pkts++
		st.octets += uint32(len(p.payload))
	}
	if err := ss.write(buf); err != nil {
		return 0, false
	}
	st.au++
	if st.au == len(tr.aus) {
		st.au = 0
		st.loop++
	}
	nxt := tr.aus[st.au]
	clock := uint64(max(tr.src.ClockRate, 1))
	units := uint64(st.loop)*uint64(tr.period) + uint64(nxt.ts)
	st.at = max(st.at, time.Duration(units*uint64(time.Second)/clock))
	return len(au.pkts), true
}

// sendSRs sends a sender report on every streaming track. The NTP time of RTP
// timestamp T is the session start plus (T - ts0)/clock.
func (ss *session) sendSRs(start time.Time, states []*sendState) bool {
	now := time.Now()
	for k, st := range states {
		tr := ss.s.tracks[st.track]
		clock := uint64(max(tr.src.ClockRate, 1))
		elapsed := now.Sub(start)
		rtpTS := ss.ts0[st.track] + uint32(uint64(elapsed)*clock/uint64(time.Second))
		ntp := now
		if ss.s.opt.SRJump != 0 && k == len(states)-1 && st.srSent >= 1 {
			ntp = ntp.Add(ss.s.opt.SRJump)
		}
		st.srSent++
		b := make([]byte, 28)
		b[0], b[1] = 0x80, 200
		binary.BigEndian.PutUint16(b[2:], 6)
		binary.BigEndian.PutUint32(b[4:], ss.ssrc[st.track])
		sec := uint64(ntp.Unix()) + 2208988800
		frac := uint64(ntp.Nanosecond()) << 32 / uint64(time.Second)
		binary.BigEndian.PutUint32(b[8:], uint32(sec))
		binary.BigEndian.PutUint32(b[12:], uint32(frac))
		binary.BigEndian.PutUint32(b[16:], rtpTS)
		binary.BigEndian.PutUint32(b[20:], st.pkts)
		binary.BigEndian.PutUint32(b[24:], st.octets)
		if err := ss.write(ss.frame(ss.chans[st.track]+1, b)); err != nil {
			return false
		}
	}
	return true
}

// inject sends the Garbage sequence.
func (ss *session) inject() bool {
	var b []byte
	b = append(b, "\r\n"...)
	b = append(b, "GARBAGE junk line\r\n"...)
	b = append(b, 0x01, 0x02, 0x03, 0xff)
	b = append(b, ss.frame(unknownChan, []byte{0x80, 0x60, 0, 1})...)
	for _, m := range []string{"GET_PARAMETER", "SET_PARAMETER", "ANNOUNCE"} {
		ss.serverCS++
		ss.s.mu.Lock()
		ss.pending[ss.serverCS] = m
		ss.s.mu.Unlock()
		b = fmt.Appendf(b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nSession: %s\r\n\r\n", m, ss.reqURL, ss.serverCS, ss.id)
	}
	return ss.write(b) == nil
}

// authorize checks req against the configured scheme. On failure it returns
// the WWW-Authenticate header to send with the 401.
func (s *Server) authorize(req *rtsp.Request) (rtsp.Header, bool) {
	a := req.Header.Get("Authorization")
	switch s.opt.Auth {
	case "":
		return nil, true
	case "basic":
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(s.opt.User+":"+s.opt.Pass))
		if a == want {
			return nil, true
		}
		return rtsp.Header{"WWW-Authenticate": {`Basic realm="` + realm + `"`}}, false
	case "digest", "digest-noqop":
		stale, ok := s.checkDigest(req.Method, req.URL, a)
		if ok {
			return nil, true
		}
		nonce := s.newNonce()
		v := `Digest realm="` + realm + `", nonce="` + nonce + `"`
		if s.opt.Auth == "digest" {
			v += `, qop="auth"`
		}
		if stale {
			v += ", stale=true"
		}
		return rtsp.Header{"WWW-Authenticate": {v}}, false
	}
	s.t.Errorf("rtsptest: unknown auth mode %q", s.opt.Auth)
	return nil, false
}

func (s *Server) newNonce() string {
	var b [12]byte
	for i := range b {
		b[i] = byte(rand.Uint32())
	}
	n := hex.EncodeToString(b[:])
	s.mu.Lock()
	s.nonces[n] = time.Now()
	s.mu.Unlock()
	return n
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s)) //nolint:gosec // Digest authentication hash.
	return hex.EncodeToString(h[:])
}

// checkDigest verifies a Digest Authorization value.
func (s *Server) checkDigest(method, uri, a string) (stale, ok bool) {
	scheme, rest, _ := strings.Cut(a, " ")
	if !strings.EqualFold(scheme, "Digest") {
		return false, false
	}
	p, err := parseParams(rest)
	if err != nil || p["username"] != s.opt.User || p["realm"] != realm || p["uri"] != uri {
		return false, false
	}
	s.mu.Lock()
	issued, known := s.nonces[p["nonce"]]
	s.mu.Unlock()
	if !known {
		return false, false
	}
	ha1 := md5hex(s.opt.User + ":" + realm + ":" + s.opt.Pass)
	ha2 := md5hex(method + ":" + uri)
	var want string
	if s.opt.Auth == "digest" {
		if p["qop"] != "auth" || p["nc"] == "" || p["cnonce"] == "" {
			return false, false
		}
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	} else {
		if _, has := p["qop"]; has {
			return false, false
		}
		want = md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
	}
	if p["response"] != want {
		return false, false
	}
	if s.opt.NonceTTL > 0 && time.Since(issued) > s.opt.NonceTTL {
		return true, false
	}
	return false, true
}

// parseParams parses comma-separated k=v / k="v" pairs.
func parseParams(v string) (map[string]string, error) {
	out := map[string]string{}
	for v = strings.TrimSpace(v); v != ""; v = strings.TrimLeft(v, ", \t") {
		k, rest, ok := strings.Cut(v, "=")
		if !ok {
			return nil, errors.New("rtsptest: malformed parameter")
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				return nil, errors.New("rtsptest: unterminated quote")
			}
			out[k] = rest[1 : end+1]
			v = rest[end+2:]
			continue
		}
		val, after, _ := strings.Cut(rest, ",")
		out[k] = strings.TrimSpace(val)
		v = after
	}
	return out, nil
}
