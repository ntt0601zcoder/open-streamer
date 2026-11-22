package rtsp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp/rtsptest"
)

const testPublic = "OPTIONS, DESCRIBE, SETUP, PLAY, GET_PARAMETER, TEARDOWN"

// testTracks is an H.264 and an AAC (48 kHz stereo) descriptor.
func testTracks() []rtsp.TrackDesc {
	return []rtsp.TrackDesc{
		{
			Codec: rtsp.CodecH264, Payload: 96, ClockRate: 90000,
			ParamSets: [][]byte{{0x67, 0x42, 0xc0, 0x1e, 0x95}, {0x68, 0xce, 0x3c, 0x80}}, ID: 7,
		},
		{
			Codec: rtsp.CodecAAC, Payload: 98, ClockRate: 48000, Channels: 2,
			AAC: &rtsp.AACFormat{Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3}, ID: 9,
		},
	}
}

// testHandler serves fixed tracks per path. Every playing session is sent
// on sessions; play, when set, produces the media.
type testHandler struct {
	mu       sync.Mutex
	tracks   map[string][]rtsp.TrackDesc
	paths    []string
	play     func(ctx context.Context, s *rtsp.ServerSession)
	sessions chan *rtsp.ServerSession
}

func newHandler() *testHandler {
	return &testHandler{
		tracks:   map[string][]rtsp.TrackDesc{"live/cam": testTracks(), "live/other": testTracks()},
		sessions: make(chan *rtsp.ServerSession, 64),
	}
}

func (h *testHandler) Describe(path string) ([]rtsp.TrackDesc, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, path)
	t, ok := h.tracks[path]
	return t, ok
}

func (h *testHandler) Play(ctx context.Context, s *rtsp.ServerSession) {
	h.sessions <- s
	if h.play != nil {
		h.play(ctx, s)
		return
	}
	<-ctx.Done()
}

func (h *testHandler) describePaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

func (h *testHandler) session(t *testing.T) *rtsp.ServerSession {
	t.Helper()
	select {
	case s := <-h.sessions:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("no session started playing")
		return nil
	}
}

// startServer serves h on 127.0.0.1:0 and closes the server at cleanup.
func startServer(t *testing.T, h rtsp.Handler, cfg rtsp.ServerConfig) (*rtsp.Server, string) {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := rtsp.NewServer(h, cfg)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		select {
		case err := <-served:
			assert.ErrorIs(t, err, rtsp.ErrServerClosed)
		case <-time.After(3 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return srv, l.Addr().String()
}

func ended(s *rtsp.ServerSession, d time.Duration) bool {
	select {
	case <-s.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// rawFrame is an interleaved frame read by a raw client.
type rawFrame struct {
	ch   byte
	data []byte
}

// raw is a hand-driven RTSP client.
type raw struct {
	t       *testing.T
	c       net.Conn
	br      *bufio.Reader
	cseq    int
	base    string
	session string
	frames  []rawFrame
}

func dialRaw(t *testing.T, addr string) *raw {
	t.Helper()
	c, err := (&net.Dialer{Timeout: 2 * time.Second}).Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return &raw{t: t, c: c, br: bufio.NewReaderSize(c, 1<<16), base: "rtsp://" + addr + "/live/cam"}
}

func (r *raw) writeRaw(s string) {
	r.t.Helper()
	_ = r.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := r.c.Write([]byte(s))
	require.NoError(r.t, err)
}

// send writes a request with the next CSeq and the given header pairs.
func (r *raw) send(method, url string, kv ...string) {
	r.t.Helper()
	r.cseq++
	h := rtsp.Header{"CSeq": {strconv.Itoa(r.cseq)}}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	_ = r.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	require.NoError(r.t, (&rtsp.Request{Method: method, URL: url, Header: h}).Write(r.c))
}

// frame reads one interleaved frame, failing on anything else.
func (r *raw) frame() (rawFrame, error) {
	_ = r.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var h [4]byte
	if _, err := io.ReadFull(r.br, h[:]); err != nil {
		return rawFrame{}, err
	}
	if h[0] != '$' {
		return rawFrame{}, fmt.Errorf("not a frame: %q", h[:])
	}
	data := make([]byte, binary.BigEndian.Uint16(h[2:]))
	if _, err := io.ReadFull(r.br, data); err != nil {
		return rawFrame{}, err
	}
	return rawFrame{ch: h[1], data: data}, nil
}

// resp reads the next response, keeping the frames in front of it.
func (r *raw) resp() *rtsp.Response {
	r.t.Helper()
	for {
		_ = r.c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b, err := r.br.Peek(1)
		require.NoError(r.t, err)
		if b[0] == '$' {
			f, err := r.frame()
			require.NoError(r.t, err)
			r.frames = append(r.frames, f)
			continue
		}
		resp, err := rtsp.ReadResponse(r.br, rtsp.Limits{})
		require.NoError(r.t, err)
		return resp
	}
}

func (r *raw) do(method, url string, kv ...string) *rtsp.Response {
	r.t.Helper()
	if r.session != "" && !hasKey(kv, "Session") {
		kv = append(kv, "Session", r.session)
	}
	r.send(method, url, kv...)
	resp := r.resp()
	assert.Equal(r.t, strconv.Itoa(r.cseq), resp.Header.Get("CSeq"), "%s CSeq", method)
	return resp
}

func hasKey(kv []string, k string) bool {
	for i := 0; i < len(kv); i += 2 {
		if strings.EqualFold(kv[i], k) {
			return true
		}
	}
	return false
}

// setup sets up one track and adopts the session on success.
func (r *raw) setup(track int, transport string) *rtsp.Response {
	r.t.Helper()
	resp := r.do("SETUP", r.base+"/trackID="+strconv.Itoa(track), "Transport", transport)
	if resp.StatusCode == 200 && r.session == "" {
		s, err := rtsp.ParseSession(resp.Header.Get("Session"))
		require.NoError(r.t, err)
		r.session = s.ID
	}
	return resp
}

func (r *raw) play() *rtsp.Response {
	r.t.Helper()
	return r.do("PLAY", r.base+"/", "Range", "npt=0.000-")
}

// eof reports whether the server closes the connection within d, reading
// and dropping whatever arrives before.
func (r *raw) eof(d time.Duration) bool {
	_ = r.c.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, r.br)
	var ne net.Error
	return !errors.As(err, &ne) || !ne.Timeout()
}

func tcpOffer(a, b int) string {
	return fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", a, b)
}

// producer writes one two-packet video frame and one audio frame per tick
// on every set-up track. The first video PTS is b, the first audio PTS
// b+3000.
func producer(tick time.Duration) func(ctx context.Context, s *rtsp.ServerSession) {
	return func(ctx context.Context, s *rtsp.ServerSession) {
		const b = 900000
		_, _, vbase := s.Origin(0)
		_, _, abase := s.Origin(1)
		t := time.NewTicker(tick)
		defer t.Stop()
		for i := 0; ; i++ {
			vpts, apts := int64(b+i*3600), int64(b+3000+i*3600)
			vts := vbase + uint32(vpts-b)
			ats := abase + uint32((apts-b)*48000/90000)
			for _, tr := range s.Tracks() {
				var ps []*rtp.Packet
				if tr == 0 {
					ps = []*rtp.Packet{
						{Header: rtp.Header{Timestamp: vts}, Payload: []byte{0x7c, 0x85, byte(i)}},
						{Header: rtp.Header{Timestamp: vts, Marker: true}, Payload: []byte{0x7c, 0x45, byte(i)}},
					}
				} else {
					ps = []*rtp.Packet{{Header: rtp.Header{Timestamp: ats, Marker: true}, Payload: []byte{0, 16, 0, 8, byte(i)}}}
				}
				if s.WritePackets(tr, ps) != nil {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}
}

func TestServerPlayTCP(t *testing.T) {
	h := newHandler()
	h.play = producer(5 * time.Millisecond)
	srv, addr := startServer(t, h, rtsp.ServerConfig{})
	c := dial(t, "rtsp://"+addr+"/live/cam", testConfig())
	s := h.session(t)

	n := map[int]int{}
	ps := readUntil(t, c, 3*time.Second, func(p rtsp.Packet) bool {
		if p.RTP != nil {
			n[p.Track]++
		}
		return n[0] >= 20 && n[1] >= 10
	})
	info := c.RTPInfo()
	require.Len(t, info, 2)
	first := map[int]*rtp.Packet{}
	last := map[int]uint16{}
	for _, p := range ps {
		if p.RTP == nil {
			continue
		}
		ssrc, _, _ := s.Origin(p.Track)
		assert.Equal(t, ssrc, p.RTP.SSRC, "track %d SSRC", p.Track)
		if _, ok := first[p.Track]; !ok {
			first[p.Track] = p.RTP
		} else {
			assert.Equal(t, last[p.Track]+1, p.RTP.SequenceNumber, "track %d sequence", p.Track)
		}
		last[p.Track] = p.RTP.SequenceNumber
	}
	require.Contains(t, first, 0)
	require.Contains(t, first, 1)
	for tr := range 2 {
		require.True(t, info[tr].HasSeq && info[tr].HasTime, "track %d RTP-Info", tr)
		assert.Equal(t, info[tr].Seq, first[tr].SequenceNumber, "track %d first seq", tr)
		_, seq, base := s.Origin(tr)
		assert.Equal(t, seq, info[tr].Seq)
		assert.Equal(t, base, info[tr].RTPTime)
	}
	assert.Equal(t, uint8(96), first[0].PayloadType)
	assert.Equal(t, uint8(98), first[1].PayloadType)
	// (PTS_first − b)·clock/90000: 0 for video, 3000·48000/90000 for audio.
	assert.Equal(t, info[0].RTPTime, first[0].Timestamp)
	assert.Equal(t, info[1].RTPTime+1600, first[1].Timestamp)

	assert.Equal(t, "live/cam", s.Path())
	assert.Equal(t, []int{0, 1}, s.Tracks())
	assert.False(t, s.UDP())
	descs := s.TrackDescs()
	require.Len(t, descs, 2)
	assert.Equal(t, 7, descs[0].ID)
	descs[0].ParamSets[0][0] = 0
	assert.Equal(t, byte(0x67), s.TrackDescs()[0].ParamSets[0][0], "TrackDescs must return a copy")
	st := srv.Stats()
	assert.Equal(t, 1, st.Sessions)
	assert.Equal(t, 1, st.Connections)
}

func TestServerKeepaliveTCP(t *testing.T) {
	h := newHandler()
	_, addr := startServer(t, h, rtsp.ServerConfig{SessionTimeout: 200 * time.Millisecond})
	r := dialRaw(t, addr)
	resp := r.setup(0, tcpOffer(0, 1))
	require.Equal(t, 200, resp.StatusCode)
	assert.Regexp(t, `^[0-9a-f]{16,};timeout=1$`, resp.Header.Get("Session"))
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)

	resp = r.do("GET_PARAMETER", r.base+"/")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, r.session+";timeout=1", resp.Header.Get("Session"))
	assert.Equal(t, 200, r.do("OPTIONS", r.base).StatusCode)
	assert.False(t, ended(s, 700*time.Millisecond), "a connected TCP session must outlive the session timeout")

	start := time.Now()
	_ = r.c.Close()
	require.True(t, ended(s, time.Second), "closing the connection must end the session")
	assert.Less(t, time.Since(start), 500*time.Millisecond)
	assert.Error(t, s.Err())
}

func TestServerTeardown(t *testing.T) {
	h := newHandler()
	srv, addr := startServer(t, h, rtsp.ServerConfig{})

	c, err := rtsp.Dial(context.Background(), "rtsp://"+addr+"/live/cam", testConfig())
	require.NoError(t, err)
	s := h.session(t)
	require.NoError(t, c.Close())
	require.True(t, ended(s, time.Second))
	assert.ErrorIs(t, s.Err(), rtsp.ErrTeardown)

	r := dialRaw(t, addr)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s = h.session(t)
	assert.Equal(t, 200, r.do("TEARDOWN", r.base+"/").StatusCode)
	assert.True(t, r.eof(time.Second), "TEARDOWN ends the connection")
	require.True(t, ended(s, time.Second))
	assert.ErrorIs(t, s.Err(), rtsp.ErrTeardown)

	// TEARDOWN of a session that never played.
	r = dialRaw(t, addr)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	assert.Equal(t, 200, r.do("TEARDOWN", r.base+"/").StatusCode)
	assert.True(t, r.eof(time.Second))
	require.Eventually(t, func() bool { st := srv.Stats(); return st.Sessions == 0 && st.Connections == 0 },
		2*time.Second, 10*time.Millisecond)
}

func TestServerProtocolErrors(t *testing.T) {
	h := newHandler()
	h.tracks["live/bad"] = []rtsp.TrackDesc{{Codec: rtsp.CodecH264, Payload: 96, ClockRate: 90000}}
	srv, addr := startServer(t, h, rtsp.ServerConfig{})
	r := dialRaw(t, addr)

	r.writeRaw("OPTIONS * RTSP/1.0\r\nCSeq: abc\r\n\r\n")
	assert.Equal(t, 400, r.resp().StatusCode, "bad CSeq")
	r.writeRaw("OPTIONS * RTSP/1.0\r\n\r\n")
	assert.Equal(t, 400, r.resp().StatusCode, "missing CSeq")

	resp := r.do("OPTIONS", "*")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, testPublic, resp.Header.Get("Public"))

	assert.Equal(t, 455, r.do("PLAY", r.base).StatusCode, "PLAY before SETUP")
	assert.Equal(t, 454, r.do("TEARDOWN", r.base).StatusCode, "TEARDOWN without a session")
	assert.Equal(t, 501, r.do("FOO", r.base).StatusCode, "unknown method")
	assert.Equal(t, 501, r.do("SET_PARAMETER", r.base).StatusCode)
	for _, m := range []string{"RECORD", "ANNOUNCE"} {
		resp = r.do(m, r.base)
		assert.Equal(t, 405, resp.StatusCode, m)
		assert.Equal(t, testPublic, resp.Header.Get("Allow"), m)
	}
	assert.Equal(t, 455, r.do("PAUSE", r.base).StatusCode)
	assert.Equal(t, 200, r.do("GET_PARAMETER", r.base).StatusCode, "GET_PARAMETER without a session")

	resp = r.do("DESCRIBE", "rtsp://"+addr+"/live/nope")
	assert.Equal(t, 404, resp.StatusCode)
	assert.Equal(t, "Stream Not Found", resp.Reason)
	assert.Equal(t, 500, r.do("DESCRIBE", "rtsp://"+addr+"/live/bad").StatusCode)
	assert.Equal(t, 400, r.do("DESCRIBE", "rtsp://"+addr+"/%zz").StatusCode)
	resp = r.setup(9, tcpOffer(0, 1))
	assert.Equal(t, 404, resp.StatusCode, "unknown track")
	assert.Equal(t, "Stream Not Found", resp.Reason)
	assert.Equal(t, 404, r.do("SETUP", r.base+"/trackID=x", "Transport", tcpOffer(0, 1)).StatusCode)
	assert.Equal(t, 404, r.do("SETUP", "rtsp://"+addr+"/live/nope/trackID=0", "Transport", tcpOffer(0, 1)).StatusCode)
	assert.Equal(t, 459, r.do("SETUP", r.base, "Transport", tcpOffer(0, 1)).StatusCode, "aggregate SETUP")
	assert.Equal(t, 461, r.setup(0, tcpOffer(0, 1)+";mode=record").StatusCode, "mode=record")
	assert.Equal(t, 461, r.setup(0, "RTP/AVP;multicast").StatusCode, "multicast")
	assert.Equal(t, 461, r.setup(0, "RTP/AVP/TCP;unicast;destination=10.9.9.9").StatusCode, "foreign destination")
	assert.Equal(t, 461, r.setup(0, "RTP/AVP;unicast;client_port=5000-5001").StatusCode, "UDP without a pair")
	assert.Equal(t, 461, r.setup(0, "RAW/RAW/UDP;unicast").StatusCode)
	assert.Equal(t, 461, r.do("SETUP", r.base+"/trackID=0").StatusCode, "no Transport")
	assert.Equal(t, 0, srv.Stats().Sessions)

	resp = r.setup(0, tcpOffer(0, 1)+";destination=127.0.0.1;mode=\"PLAY\"")
	require.Equal(t, 200, resp.StatusCode)
	assert.Regexp(t, `^[0-9a-f]{16,};timeout=60$`, resp.Header.Get("Session"))
	assert.Equal(t, 455, r.setup(0, tcpOffer(2, 3)).StatusCode, "track set up twice")
	assert.Equal(t, 461, r.setup(1, "RTP/AVP;unicast;client_port=5000-5001").StatusCode, "another transport")
	assert.Equal(t, 454, r.do("SETUP", r.base+"/trackID=1", "Transport", tcpOffer(2, 3), "Session", "bogus").StatusCode)
	id := r.session
	r.session = ""
	assert.Equal(t, 455, r.do("SETUP", r.base+"/trackID=1", "Transport", tcpOffer(2, 3)).StatusCode, "second session")
	assert.Equal(t, 454, r.do("PLAY", r.base).StatusCode, "PLAY without Session")
	r.session = id
	assert.Equal(t, 404, r.do("SETUP", "rtsp://"+addr+"/live/other/trackID=1", "Transport", tcpOffer(2, 3)).StatusCode,
		"SETUP of another stream")
	assert.Equal(t, 454, r.do("PLAY", r.base, "Session", "bogus").StatusCode, "unknown Session")
	assert.Equal(t, 454, r.do("GET_PARAMETER", r.base, "Session", "bogus").StatusCode)
	assert.Equal(t, 404, r.do("PLAY", "rtsp://"+addr+"/live/other").StatusCode, "PLAY of another stream")
	assert.Equal(t, 200, r.play().StatusCode)
	assert.Equal(t, 200, r.play().StatusCode, "repeated PLAY")
	assert.Equal(t, 455, r.setup(1, tcpOffer(2, 3)).StatusCode, "SETUP after PLAY")
	assert.Equal(t, 455, r.do("PAUSE", r.base).StatusCode)
	assert.Equal(t, 454, r.do("TEARDOWN", r.base, "Session", "bogus").StatusCode)
	assert.Equal(t, 200, r.do("GET_PARAMETER", r.base).StatusCode)
	assert.Equal(t, 1, srv.Stats().Sessions)
	assert.Positive(t, srv.Stats().ProtocolErrors)
	assert.Positive(t, srv.Stats().Rejected)
}

func TestServerTransportOffers(t *testing.T) {
	h := newHandler()
	_, addr := startServer(t, h, rtsp.ServerConfig{})
	r := dialRaw(t, addr)
	resp := r.setup(0, "RTP/AVP/UDP;unicast;client_port=5000-5001, "+tcpOffer(4, 5))
	require.Equal(t, 200, resp.StatusCode)
	tr0, err := rtsp.ParseTransport(resp.Header.Get("Transport"))
	require.NoError(t, err)
	assert.Equal(t, "RTP/AVP/TCP", tr0.Protocol)
	assert.True(t, tr0.Unicast)
	assert.Equal(t, [2]int{4, 5}, tr0.Interleaved)
	require.True(t, tr0.HasSSRC)

	resp = r.setup(1, "rtp/avp/tcp;unicast")
	require.Equal(t, 200, resp.StatusCode, "profiles match case-insensitively")
	tr1, err := rtsp.ParseTransport(resp.Header.Get("Transport"))
	require.NoError(t, err)
	assert.Equal(t, [2]int{0, 1}, tr1.Interleaved, "no channels asked: the first free pair")
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)
	ssrc0, _, _ := s.Origin(0)
	ssrc1, _, _ := s.Origin(1)
	assert.Equal(t, ssrc0, tr0.SSRC)
	assert.Equal(t, ssrc1, tr1.SSRC)
	assert.NotEqual(t, ssrc0, ssrc1)

	r2 := dialRaw(t, addr)
	assert.Equal(t, 461, r2.setup(0, "RTP/AVP/UDP;unicast;client_port=5000-5001").StatusCode)
	assert.Equal(t, 461, r2.setup(0, "RTP/AVP/TCP;interleaved=x").StatusCode, "malformed offer")
	assert.Equal(t, 200, r2.setup(0, "RTP/AVP/TCP;interleaved=0-1").StatusCode, "no unicast or multicast: unicast")
}

func TestServerChannelsChosen(t *testing.T) {
	h := newHandler()
	h.play = producer(5 * time.Millisecond)
	_, addr := startServer(t, h, rtsp.ServerConfig{})
	channels := func(resp *rtsp.Response) [2]int {
		t.Helper()
		require.Equal(t, 200, resp.StatusCode)
		tr, err := rtsp.ParseTransport(resp.Header.Get("Transport"))
		require.NoError(t, err)
		return tr.Interleaved
	}
	r := dialRaw(t, addr)
	assert.Equal(t, [2]int{0, 1}, channels(r.setup(0, tcpOffer(0, 1))))
	assert.Equal(t, [2]int{2, 3}, channels(r.setup(1, tcpOffer(0, 1))), "taken channels: the next free pair")
	require.Equal(t, 200, r.play().StatusCode)
	seen := map[byte]int{}
	for len(seen) < 2 || seen[0] < 3 || seen[2] < 3 {
		f, err := r.frame()
		require.NoError(t, err)
		seen[f.ch]++
	}
	assert.Len(t, seen, 2, "media only on the chosen RTP channels: %v", seen)

	r = dialRaw(t, addr)
	assert.Equal(t, [2]int{1, 2}, channels(r.setup(0, tcpOffer(1, 2))))
	assert.Equal(t, [2]int{4, 5}, channels(r.setup(1, tcpOffer(2, 3))))
	for _, offer := range []string{"RTP/AVP/TCP;unicast;interleaved=255", tcpOffer(7, 7)} {
		r = dialRaw(t, addr)
		assert.Equal(t, [2]int{0, 1}, channels(r.setup(0, offer)), offer)
	}
	r = dialRaw(t, addr)
	assert.Equal(t, [2]int{7, 8}, channels(r.setup(0, "RTP/AVP/TCP;unicast;interleaved=7")))
}

func TestServerPathNormalisation(t *testing.T) {
	h := newHandler()
	_, addr := startServer(t, h, rtsp.ServerConfig{})
	r := dialRaw(t, addr)
	_, port, _ := net.SplitHostPort(addr)

	resp := r.do("DESCRIBE", "rtsp://"+addr+"/live/cam?token=1", "Accept", "application/sdp")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "rtsp://"+addr+"/live/cam/", resp.Header.Get("Content-Base"))
	assert.Equal(t, "application/sdp", resp.Header.Get("Content-Type"))
	assert.Contains(t, string(resp.Body), "a=control:trackID=1")
	resp = r.do("DESCRIBE", "rtsp://user:pw@localhost:"+port+"/live/cam/")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "rtsp://localhost:"+port+"/live/cam/", resp.Header.Get("Content-Base"))
	assert.Equal(t, []string{"live/cam", "live/cam"}, h.describePaths())

	u0 := "rtsp://" + addr + "/live/cam/trackID=0?x=1"
	u1 := "rtsp://localhost:" + port + "/live/cam/trackID=1/"
	resp = r.do("SETUP", u0, "Transport", tcpOffer(0, 1))
	require.Equal(t, 200, resp.StatusCode)
	sess, err := rtsp.ParseSession(resp.Header.Get("Session"))
	require.NoError(t, err)
	r.session = sess.ID
	require.Equal(t, 200, r.do("SETUP", u1, "Transport", tcpOffer(2, 3)).StatusCode)
	assert.Equal(t, []string{"live/cam", "live/cam", "live/cam"}, h.describePaths(), "Describe once more, at the first SETUP")
	resp = r.do("PLAY", "rtsp://"+addr+"/live/cam/trackID=0")
	require.Equal(t, 200, resp.StatusCode, "PLAY on a track URL is aggregate")
	assert.Equal(t, "npt=now-", resp.Header.Get("Range"))
	ss := h.session(t)
	_, seq0, ts0 := ss.Origin(0)
	_, seq1, ts1 := ss.Origin(1)
	assert.Equal(t, fmt.Sprintf("url=%s;seq=%d;rtptime=%d,url=%s;seq=%d;rtptime=%d", u0, seq0, ts0, u1, seq1, ts1),
		resp.Header.Get("RTP-Info"))
	assert.Equal(t, "live/cam", ss.Path())
}

func TestServerBounds(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		srv, addr := startServer(t, newHandler(), rtsp.ServerConfig{})
		r := dialRaw(t, addr)
		r.writeRaw("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\nX-Pad: " + strings.Repeat("a", 65<<10) + "\r\n\r\n")
		assert.True(t, r.eof(2*time.Second), "oversized header")
		r = dialRaw(t, addr)
		r.writeRaw("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\nContent-Length: 70000\r\n\r\n")
		assert.True(t, r.eof(2*time.Second), "oversized body")
		assert.GreaterOrEqual(t, srv.Stats().ProtocolErrors, uint64(2))
	})
	t.Run("pending per IP", func(t *testing.T) {
		srv, addr := startServer(t, newHandler(), rtsp.ServerConfig{MaxPendingPerIP: 2})
		a, b := dialRaw(t, addr), dialRaw(t, addr)
		assert.Equal(t, 200, a.do("OPTIONS", "*").StatusCode)
		assert.Equal(t, 200, b.do("OPTIONS", "*").StatusCode)
		c := dialRaw(t, addr)
		assert.True(t, c.eof(time.Second), "third pending connection from one IP refused")
		assert.Positive(t, srv.Stats().Rejected)
		_ = a.c.Close()
		require.Eventually(t, func() bool { return srv.Stats().Connections == 1 }, 2*time.Second, 5*time.Millisecond)
		d := dialRaw(t, addr)
		assert.Equal(t, 200, d.do("OPTIONS", "*").StatusCode)
		// A playing connection no longer counts as pending.
		require.Equal(t, 200, d.setup(0, tcpOffer(0, 1)).StatusCode)
		require.Equal(t, 200, d.play().StatusCode)
		e := dialRaw(t, addr)
		assert.Equal(t, 200, e.do("OPTIONS", "*").StatusCode)
	})
	t.Run("pending per listener", func(t *testing.T) {
		_, addr := startServer(t, newHandler(), rtsp.ServerConfig{MaxPending: 2, MaxPendingPerIP: 10})
		a, b := dialRaw(t, addr), dialRaw(t, addr)
		assert.Equal(t, 200, a.do("OPTIONS", "*").StatusCode)
		assert.Equal(t, 200, b.do("OPTIONS", "*").StatusCode)
		assert.True(t, dialRaw(t, addr).eof(time.Second))
	})
	t.Run("pending lifetime", func(t *testing.T) {
		srv, addr := startServer(t, newHandler(), rtsp.ServerConfig{PendingLifetime: 300 * time.Millisecond})
		r := dialRaw(t, addr)
		start := time.Now()
		for time.Since(start) < 150*time.Millisecond {
			require.Equal(t, 200, r.do("OPTIONS", "*").StatusCode)
			time.Sleep(30 * time.Millisecond)
		}
		require.True(t, r.eof(2*time.Second))
		el := time.Since(start)
		assert.GreaterOrEqual(t, el, 250*time.Millisecond)
		assert.Less(t, el, time.Second)
		assert.Positive(t, srv.Stats().Timeouts)
	})
	t.Run("read timeout", func(t *testing.T) {
		_, addr := startServer(t, newHandler(), rtsp.ServerConfig{ReadTimeout: 200 * time.Millisecond})
		r := dialRaw(t, addr)
		start := time.Now()
		require.True(t, r.eof(2*time.Second))
		assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond)
	})
	sessionCap := func(t *testing.T, cfg rtsp.ServerConfig, otherPath bool) {
		t.Helper()
		_, addr := startServer(t, newHandler(), cfg)
		a := dialRaw(t, addr)
		require.Equal(t, 200, a.setup(0, tcpOffer(0, 1)).StatusCode)
		require.Equal(t, 200, a.play().StatusCode)
		b := dialRaw(t, addr)
		if otherPath {
			b.base = "rtsp://" + addr + "/live/other"
		}
		require.Equal(t, 200, b.setup(0, tcpOffer(0, 1)).StatusCode)
		resp := b.play()
		assert.Equal(t, 404, resp.StatusCode)
		assert.Equal(t, "Stream Not Found", resp.Reason)
		_ = a.c.Close()
		// Polled by hand: play uses require, which must run on the test
		// goroutine.
		deadline := time.Now().Add(2 * time.Second)
		for b.play().StatusCode != 200 {
			require.True(t, time.Now().Before(deadline), "the cap was not released")
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Run("sessions per path", func(t *testing.T) {
		sessionCap(t, rtsp.ServerConfig{MaxSessionsPerPath: 1}, false)
		_, addr := startServer(t, newHandler(), rtsp.ServerConfig{MaxSessionsPerPath: 1})
		a := dialRaw(t, addr)
		require.Equal(t, 200, a.setup(0, tcpOffer(0, 1)).StatusCode)
		require.Equal(t, 200, a.play().StatusCode)
		b := dialRaw(t, addr)
		b.base = "rtsp://" + addr + "/live/other"
		require.Equal(t, 200, b.setup(0, tcpOffer(0, 1)).StatusCode)
		assert.Equal(t, 200, b.play().StatusCode, "another path is not capped")
	})
	t.Run("sessions per IP", func(t *testing.T) { sessionCap(t, rtsp.ServerConfig{MaxSessionsPerIP: 1}, true) })
	t.Run("sessions per listener", func(t *testing.T) { sessionCap(t, rtsp.ServerConfig{MaxSessions: 1}, true) })
	t.Run("tracks per session", func(t *testing.T) {
		h := newHandler()
		many := make([]rtsp.TrackDesc, 9)
		for i := range many {
			many[i] = testTracks()[0]
		}
		h.tracks["live/many"] = many
		_, addr := startServer(t, h, rtsp.ServerConfig{})
		r := dialRaw(t, addr)
		r.base = "rtsp://" + addr + "/live/many"
		assert.Equal(t, 404, r.setup(8, tcpOffer(0, 1)).StatusCode)
		assert.Equal(t, 200, r.setup(7, tcpOffer(0, 1)).StatusCode)
	})
}

func TestServerInterleavedGarbage(t *testing.T) {
	h := newHandler()
	srv, addr := startServer(t, h, rtsp.ServerConfig{})
	r := dialRaw(t, addr)
	rr := rtsp.ReceiverReport(0x1234, nil)
	frame := func(ch byte, b []byte) string {
		return string(append([]byte{'$', ch, byte(len(b) >> 8), byte(len(b))}, b...))
	}
	r.writeRaw("\r\n\r\nhello garbage\r\n" + frame(1, rr) + "xyz" + frame(0, nil) + "RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n")
	assert.Equal(t, 200, r.do("OPTIONS", "*").StatusCode)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)
	r.writeRaw("junk junk\n" + frame(1, rr) + strings.Repeat("z", 3000) + "\n")
	assert.Equal(t, 200, r.do("GET_PARAMETER", r.base+"/").StatusCode)
	assert.False(t, ended(s, 50*time.Millisecond))
	assert.Positive(t, srv.Stats().ProtocolErrors)
}

func TestServerStalledWriterDropped(t *testing.T) {
	h := newHandler()
	h.play = func(ctx context.Context, s *rtsp.ServerSession) {
		payload := bytes.Repeat([]byte{0x41}, 1000)
		for ctx.Err() == nil {
			ps := make([]*rtp.Packet, 8)
			for i := range ps {
				ps[i] = &rtp.Packet{Payload: payload}
			}
			if s.WritePackets(0, ps) != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	var hooked atomic.Int32
	srv, addr := startServer(t, h, rtsp.ServerConfig{
		WriteTimeout: 300 * time.Millisecond,
		// Only the stalled player's connection (the first) gets a small
		// send buffer.
		ConnHook: func(c net.Conn) {
			if tc, ok := c.(*net.TCPConn); ok && hooked.Add(1) == 1 {
				_ = tc.SetWriteBuffer(4096)
			}
		},
	})
	stalled := dialRaw(t, addr)
	_ = stalled.c.(*net.TCPConn).SetReadBuffer(4096)
	require.Equal(t, 200, stalled.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, stalled.play().StatusCode)
	sa := h.session(t)

	c := dial(t, "rtsp://"+addr+"/live/cam", testConfig())
	sb := h.session(t)
	start := time.Now()
	readFor(t, c, 100*time.Millisecond)
	require.True(t, ended(sa, 3*time.Second), "a stalled player must be dropped")
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Error(t, sa.Err())
	assert.Positive(t, srv.Stats().Timeouts)
	ps := readFor(t, c, 200*time.Millisecond)
	assert.Greater(t, rtpCount(ps, 0), 20, "the other player keeps receiving")
	assert.False(t, ended(sb, 0))
}

func TestServerCloseCutsStalledWriter(t *testing.T) {
	h := newHandler()
	writing := make(chan struct{})
	h.play = func(ctx context.Context, s *rtsp.ServerSession) {
		payload := bytes.Repeat([]byte{0x41}, 1000)
		var once sync.Once
		for ctx.Err() == nil {
			ps := make([]*rtp.Packet, 8)
			for i := range ps {
				ps[i] = &rtp.Packet{Payload: payload}
			}
			once.Do(func() { close(writing) })
			if s.WritePackets(0, ps) != nil {
				return
			}
		}
	}
	srv, addr := startServer(t, h, rtsp.ServerConfig{
		ConnHook: func(c net.Conn) { _ = c.(*net.TCPConn).SetWriteBuffer(4096) },
	})
	r := dialRaw(t, addr)
	_ = r.c.(*net.TCPConn).SetReadBuffer(4096)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)
	<-writing
	time.Sleep(100 * time.Millisecond) // the writer is now blocked on a full buffer
	start := time.Now()
	s.Close()
	assert.Less(t, time.Since(start), 800*time.Millisecond, "Close must cut a blocked writer")
	assert.ErrorIs(t, s.Err(), rtsp.ErrSessionClosed)
	assert.Zero(t, srv.Stats().Timeouts, "a write cut by Close is not a timeout")
}

// rtcpTypes returns the packet types of a compound RTCP packet.
func rtcpTypes(b []byte) []byte {
	var out []byte
	for len(b) >= 4 {
		n := (int(binary.BigEndian.Uint16(b[2:])) + 1) * 4
		if n > len(b) {
			break
		}
		out = append(out, b[1])
		b = b[n:]
	}
	return out
}

func TestServerByeOnClose(t *testing.T) {
	srs := make(chan []byte, 2)
	h := newHandler()
	h.play = func(ctx context.Context, s *rtsp.ServerSession) {
		ssrc, _, ts := s.Origin(0)
		sr := rtsp.BuildSenderReport(ssrc, time.Now(), ts, 1, 3)
		srs <- sr
		_ = s.WritePackets(0, []*rtp.Packet{{Header: rtp.Header{Timestamp: ts, Marker: true}, Payload: []byte{0x65, 1, 2}}})
		_ = s.WriteRTCP(0, sr)
		<-ctx.Done()
	}
	srv, addr := startServer(t, h, rtsp.ServerConfig{})
	check := func(r *raw, s *rtsp.ServerSession, close func()) []byte {
		t.Helper()
		sr := <-srs
		f, err := r.frame()
		require.NoError(t, err)
		require.Equal(t, byte(0), f.ch)
		f, err = r.frame()
		require.NoError(t, err)
		require.Equal(t, byte(1), f.ch)
		assert.Equal(t, sr, f.data)
		close()
		byes := map[byte][]byte{}
		for {
			f, err := r.frame()
			if err != nil {
				break
			}
			_, dup := byes[f.ch]
			assert.False(t, dup, "one BYE compound per track")
			byes[f.ch] = f.data
		}
		require.Len(t, byes, 2)
		assert.Equal(t, []byte{200, 202, 203}, rtcpTypes(byes[1]))
		assert.Equal(t, sr, byes[1][:len(sr)], "the last SR heads the compound")
		assert.Equal(t, []byte{201, 202, 203}, rtcpTypes(byes[3]))
		ssrc1, _, _ := s.Origin(1)
		assert.Equal(t, []uint32{ssrc1}, rtsp.ParseReceiverReports(byes[3]))
		require.True(t, ended(s, time.Second))
		return sr
	}
	r := dialRaw(t, addr)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, r.setup(1, tcpOffer(2, 3)).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)
	sr := check(r, s, s.Close)
	assert.ErrorIs(t, s.Err(), rtsp.ErrSessionClosed)
	assert.Error(t, s.WritePackets(0, []*rtp.Packet{{Payload: []byte{1}}}))
	assert.Error(t, s.WriteRTCP(0, sr))
	s.Close()

	// Server.Close ends playing sessions the same way.
	r = dialRaw(t, addr)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, r.setup(1, tcpOffer(2, 3)).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s = h.session(t)
	check(r, s, func() { require.NoError(t, srv.Close()) })
}

func TestServerWriteErrors(t *testing.T) {
	h := newHandler()
	got := make(chan []error, 1)
	h.play = func(ctx context.Context, s *rtsp.ServerSession) {
		var errs []error
		ssrc, seq, ts := s.Origin(1)
		errs = append(errs, s.WritePackets(1, []*rtp.Packet{{Payload: []byte{1}}}))
		errs = append(errs, s.WriteRTCP(1, []byte{1}))
		errs = append(errs, s.WritePackets(5, nil))
		errs = append(errs, s.WritePackets(0, []*rtp.Packet{{Payload: make([]byte, 70000)}}))
		errs = append(errs, s.WriteRTCP(0, make([]byte, 70000)))
		errs = append(errs, s.WritePackets(0, nil))
		if ssrc != 0 || seq != 0 || ts != 0 {
			errs = append(errs, errors.New("origin of a track not set up"))
		}
		got <- errs
		<-ctx.Done()
	}
	_, addr := startServer(t, h, rtsp.ServerConfig{})
	r := dialRaw(t, addr)
	require.Equal(t, 200, r.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)
	errs := <-got
	require.Len(t, errs, 6)
	for i, err := range errs[:5] {
		assert.Error(t, err, "call %d", i)
	}
	assert.NoError(t, errs[5], "an empty batch")
	assert.Equal(t, []int{0}, s.Tracks())
	assert.False(t, ended(s, 50*time.Millisecond), "rejected calls leave the session alone")
}

func TestServerGoroutinesExit(t *testing.T) {
	before := runtime.NumGoroutine()
	h := newHandler()
	h.play = producer(5 * time.Millisecond)
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := rtsp.NewServer(h, rtsp.ServerConfig{})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	addr := l.Addr().String()

	c, err := rtsp.Dial(context.Background(), "rtsp://"+addr+"/live/cam", testConfig())
	require.NoError(t, err)
	readFor(t, c, 100*time.Millisecond)
	require.NoError(t, c.Close())
	pending := dialRaw(t, addr)
	assert.Equal(t, 200, pending.do("OPTIONS", "*").StatusCode)
	playing := dialRaw(t, addr)
	require.Equal(t, 200, playing.setup(0, tcpOffer(0, 1)).StatusCode)
	require.Equal(t, 200, playing.play().StatusCode)

	require.NoError(t, srv.Close())
	require.NoError(t, srv.Close(), "Close is idempotent")
	assert.ErrorIs(t, <-served, rtsp.ErrServerClosed)
	assert.ErrorIs(t, srv.Serve(l), rtsp.ErrServerClosed)
	assert.True(t, pending.eof(time.Second))
	assert.True(t, playing.eof(time.Second))
	_ = pending.c.Close()
	_ = playing.c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: before %d, now %d\n%s", before, n, stacks())
	}
}

// requirePeer returns the path of a peer tool, skipping the test when it is
// missing, or failing it when OS_REQUIRE_PEERS=1. pkg/ cannot import
// internal/testpeer, so this mirrors testpeer.Require.
func requirePeer(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("OS_REQUIRE_PEERS") == "1" {
			t.Fatalf("peer %s not found: %v", name, err)
		}
		t.Skipf("peer %s not found: %v", name, err)
	}
	return path
}

// fixtureHandler serves h264_aac.ts packetised with the package's H.264 and
// AAC packetisers at speed times real time; closeAtEnd ends each session
// with Close after the last frame.
type fixtureHandler struct {
	video, audio rtsptest.TrackSource
	speed        float64
	closeAtEnd   bool
}

func newFixtureHandler(t *testing.T) *fixtureHandler {
	t.Helper()
	src := fixture(t)
	return &fixtureHandler{video: src[0], audio: src[1], speed: 4}
}

func (f *fixtureHandler) Describe(path string) ([]rtsp.TrackDesc, bool) {
	if path != "live/cam" {
		return nil, false
	}
	return []rtsp.TrackDesc{
		{Codec: rtsp.CodecH264, Payload: 96, ClockRate: 90000, ParamSets: f.video.ParamSets},
		{
			Codec: rtsp.CodecAAC, Payload: 98, ClockRate: f.audio.ClockRate,
			Channels: int(f.audio.AAC.Config[1]>>3) & 0xf, AAC: f.audio.AAC,
		},
	}, true
}

func (f *fixtureHandler) Play(ctx context.Context, s *rtsp.ServerSession) {
	_, _, vbase := s.Origin(0)
	_, _, abase := s.Origin(1)
	setUp := map[int]bool{}
	for _, tr := range s.Tracks() {
		setUp[tr] = true
	}
	pk := rtsp.NewH264Packetizer(1200)
	start := time.Now()
	vi, ai := 0, 0
	for vi < len(f.video.AUs) || ai < len(f.audio.AUs) {
		vt, at := 1e9, 1e9
		if vi < len(f.video.AUs) {
			vt = float64(f.video.AUs[vi].TS) / 90000
		}
		if ai < len(f.audio.AUs) {
			at = float64(f.audio.AUs[ai].TS) / float64(f.audio.ClockRate)
		}
		var track int
		var ps []*rtp.Packet
		at0 := min(vt, at)
		if vt <= at {
			au := f.video.AUs[vi]
			vi++
			pls := pk.Packetize(au.NALUs)
			for i, pl := range pls {
				ps = append(ps, &rtp.Packet{Header: rtp.Header{Timestamp: vbase + au.TS, Marker: i == len(pls)-1}, Payload: pl})
			}
		} else {
			au := f.audio.AUs[ai]
			ai++
			track = 1
			ps = rtsp.PacketizeAAC(au.Frames, abase+au.TS, 1024, 1200+12, *f.audio.AAC)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(start.Add(time.Duration(at0 / f.speed * float64(time.Second))))):
		}
		if setUp[track] && s.WritePackets(track, ps) != nil {
			return
		}
	}
	if f.closeAtEnd {
		s.Close()
		return
	}
	<-ctx.Done()
}

func TestServerFFprobeTCP(t *testing.T) {
	ffprobe := requirePeer(t, "ffprobe")
	_, addr := startServer(t, newFixtureHandler(t), rtsp.ServerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-rtsp_transport", "tcp",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", "rtsp://"+addr+"/live/cam")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "ffprobe: %s", stderr.String())
	assert.Equal(t, []string{"h264", "aac"}, strings.Fields(string(out)), "ffprobe: %s", stderr.String())
}

func TestServerFFmpegEOFOnBye(t *testing.T) {
	ffmpeg := requirePeer(t, "ffmpeg")
	f := newFixtureHandler(t)
	f.speed, f.closeAtEnd = 8, true
	_, addr := startServer(t, f, rtsp.ServerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-rtsp_transport", "tcp",
		"-i", "rtsp://"+addr+"/live/cam", "-f", "null", "-")
	cmd.Stderr = &stderr
	start := time.Now()
	require.NoError(t, cmd.Run(), "ffmpeg: %s", stderr.String())
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Empty(t, stderr.String())
}
