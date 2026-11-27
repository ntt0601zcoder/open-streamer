package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectTransport(t *testing.T) {
	peer := net.ParseIP("192.0.2.7")
	udp := Transport{Protocol: "RTP/AVP/UDP", Unicast: true, ClientPort: [2]int{5000, 5001}}
	tcp := Transport{Protocol: "RTP/AVP/TCP", Unicast: true, Interleaved: [2]int{0, 1}}
	cases := []struct {
		name    string
		offers  []Transport
		udpOK   bool
		current transportKind
		want    transportKind
		idx     int
	}{
		{"UDP first when the server owns a pair", []Transport{udp, tcp}, true, transportNone, transportUDP, 0},
		{"UDP skipped without a pair", []Transport{udp, tcp}, false, transportNone, transportTCP, 1},
		{"plain RTP/AVP is UDP", []Transport{{Protocol: "rtp/avp", ClientPort: [2]int{6000, 6001}}}, true, transportNone, transportUDP, 0},
		{"UDP without client ports", []Transport{{Protocol: "RTP/AVP", Unicast: true}, tcp}, true, transportNone, transportTCP, 1},
		{"the session's transport wins", []Transport{udp, tcp}, true, transportTCP, transportTCP, 1},
		{"destination of the peer", []Transport{{Protocol: "RTP/AVP/TCP", Destination: "192.0.2.7"}}, false, transportNone, transportTCP, 0},
		{"mode PLAY", []Transport{{Protocol: "RTP/AVP/TCP", Mode: "play"}}, false, transportNone, transportTCP, 0},
	}
	for _, c := range cases {
		got, kind, ok := selectTransport(c.offers, peer, c.udpOK, c.current)
		require.True(t, ok, c.name)
		assert.Equal(t, c.want, kind, c.name)
		assert.Equal(t, c.offers[c.idx], got, c.name)
	}
	for _, bad := range [][]Transport{
		{{Protocol: "RTP/AVP/TCP", Multicast: true}},
		{{Protocol: "RTP/AVP/TCP", Mode: "RECORD"}},
		{{Protocol: "RTP/AVP/TCP", Destination: "192.0.2.8"}},
		{{Protocol: "RTP/AVP/TCP", Destination: "example.com"}},
		{{Protocol: "RTP/SAVP"}},
		nil,
	} {
		_, _, ok := selectTransport(bad, peer, true, transportNone)
		assert.False(t, ok, "%+v", bad)
	}
	_, _, ok := selectTransport([]Transport{udp}, peer, true, transportTCP)
	assert.False(t, ok, "another transport than the session's")
}

func TestTransportReply(t *testing.T) {
	assert.Equal(t, "RTP/AVP/TCP;unicast;interleaved=2-3;ssrc=DEADBEEF",
		transportReply(transportTCP, Transport{Protocol: "rtp/avp/tcp"}, [2]int{2, 3}, [2]int{}, 0xDEADBEEF))
	udp := Transport{Protocol: "rtp/avp/udp", Unicast: true, ClientPort: [2]int{5000, 5001}}
	assert.Equal(t, "RTP/AVP/UDP;unicast;client_port=5000-5001;server_port=6970-6971;ssrc=00000001",
		transportReply(transportUDP, udp, [2]int{}, [2]int{6970, 6971}, 1), "client_port echoed")
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in, path, base string
		track          int
		bad            bool
	}{
		{"rtsp://h:8554/live/cam", "live/cam", "rtsp://h:8554/live/cam/", -1, false},
		{"rtsp://h/live/cam/?q=1/2", "live/cam", "rtsp://h/live/cam/", -1, false},
		{"RTSP://u:p@H:1/live/cam/trackID=3/", "live/cam", "RTSP://H:1/live/cam/", 3, false},
		{"rtsp://h/live/cam/TRACKID=0?x", "live/cam", "rtsp://h/live/cam/", 0, false},
		{"rtsp://h/live/cam/trackID=zz", "live/cam", "rtsp://h/live/cam/", -1, true},
		{"rtsp://h/live/ca%20m", "live/ca m", "rtsp://h/live/ca%20m/", -1, false},
		{"rtsp://h", "", "rtsp://h/", -1, false},
		{"rtsp://h/trackID=1", "", "rtsp://h/", 1, false},
	}
	for _, c := range cases {
		tg, err := parseTarget(c.in)
		require.NoError(t, err, c.in)
		assert.Equal(t, c.path, tg.path, c.in)
		assert.Equal(t, c.base, tg.contentBase, c.in)
		assert.Equal(t, c.track, tg.track, c.in)
		assert.Equal(t, c.bad, tg.badTrack, c.in)
	}
	for _, bad := range []string{"*", "//h/live", "rtsp:///live", "rtsp://h/%zz", "/live/cam"} {
		_, err := parseTarget(bad)
		assert.Error(t, err, bad)
	}
}

func TestPeerKey(t *testing.T) {
	ip, key := peerKey(&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1})
	assert.Equal(t, "192.0.2.1", ip.String())
	assert.Equal(t, "192.0.2.1", key)
	_, k1 := peerKey(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:aaaa::1")})
	_, k2 := peerKey(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:bbbb::9")})
	assert.Equal(t, "2001:db8:1:2::/64", k1)
	assert.Equal(t, k1, k2, "IPv6 peers are counted by /64")
	ip, key = peerKey(&net.UDPAddr{IP: net.ParseIP("192.0.2.9"), Port: 5})
	assert.Equal(t, "192.0.2.9", ip.String())
	assert.Equal(t, "192.0.2.9", key)
	ip, key = peerKey(&net.UnixAddr{Name: "@sock", Net: "unix"})
	assert.Nil(t, ip)
	assert.Equal(t, "@sock", key)
	assert.Equal(t, "0.0.0.0", originHost(&net.UnixAddr{Name: "@sock", Net: "unix"}))
}

func TestFirstSR(t *testing.T) {
	sr := BuildSenderReport(1, time.Unix(1, 0), 2, 3, 4)
	compound := append(append([]byte(nil), sr...), ReceiverReport(1, nil)...)
	got := firstSR(compound)
	assert.Equal(t, sr, got)
	got[0] = 0
	assert.NotEqual(t, byte(0), compound[0], "a copy")
	assert.Nil(t, firstSR(ReceiverReport(1, nil)))
	assert.Nil(t, firstSR(sr[:len(sr)-1]))
	long := append([]byte(nil), sr...)
	long[3] = 99 // claims more than there is
	assert.Nil(t, firstSR(long))
}

// flakyListener fails Accept with a transient error first, then reports
// itself closed.
type flakyListener struct {
	net.Listener
	calls atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.calls.Add(1) <= 2 {
		return nil, errors.New("accept: too many open files")
	}
	return nil, net.ErrClosed
}

func TestServeRetriesTransientErrors(t *testing.T) {
	l := &flakyListener{}
	err := NewServer(nil, ServerConfig{}).Serve(l)
	require.ErrorIs(t, err, net.ErrClosed)
	assert.NotErrorIs(t, err, ErrServerClosed)
	assert.Equal(t, int32(3), l.calls.Load())
}

func TestUDPLiveness(t *testing.T) {
	now := time.Unix(100, 0)
	srv := NewServer(nil, ServerConfig{SessionTimeout: time.Minute, Now: func() time.Time { return now }})
	c := &serverConn{srv: srv, playing: true}
	c.sess = &ServerSession{c: c, kind: transportUDP}
	c.sess.touch()
	assert.True(t, c.liveUDP())
	assert.WithinDuration(t, time.Now().Add(time.Minute), c.readDeadline(), time.Second)
	now = now.Add(61 * time.Second)
	assert.False(t, c.liveUDP())
	c.sess.kind = transportTCP
	assert.True(t, c.readDeadline().IsZero(), "a playing TCP session has no read deadline")
}

func TestUDPReaches(t *testing.T) {
	v4, v6 := net.IPv4(127, 0, 0, 1).To4(), net.ParseIP("::1")
	cases := []struct {
		local, peer net.IP
		want        bool
	}{
		{net.IPv4zero, v4, true},
		{net.IPv4zero, v6, true},
		{net.IPv6unspecified, v6, true},
		{nil, v4, true},
		{v4, v4, true},
		{v4, v6, false},
		{v6, v4, false},
		{v6, v6, true},
		{v4, nil, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, udpReaches(c.local, c.peer), "%v -> %v", c.local, c.peer)
	}
}

// TestCloseCutsResponseWrite checks that a response write cut by Close is
// neither a timeout nor a "connection ended": the session ends with
// ErrSessionClosed.
func TestCloseCutsResponseWrite(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	srv := NewServer(nil, ServerConfig{WriteTimeout: 400 * time.Millisecond})
	ls := &listenerState{sessions: 1}
	c := &serverConn{srv: srv, ls: ls, nc: a, br: bufio.NewReader(a), peer: net.IPv4(127, 0, 0, 1).To4(), ipKey: "127.0.0.1", since: time.Now()}
	sess := newServerSession(c, "live/cam", []TrackDesc{{Codec: CodecH264, Payload: 96, ClockRate: 90000}})
	sess.addTrack(0, "rtsp://x/live/cam/trackID=0", transportTCP, Transport{Interleaved: [2]int{0, 1}})
	c.sess, c.playing = sess, true
	srv.conns[c] = struct{}{}
	srv.sessIP[c.ipKey], srv.sessPath["live/cam"] = 1, 1
	srv.wg.Add(1)
	go c.serve()
	// The request is read; nobody reads the response, so its write blocks.
	_, err := b.Write([]byte("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"))
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	sess.Close()
	srv.wg.Wait()
	assert.ErrorIs(t, sess.Err(), ErrSessionClosed)
	assert.Zero(t, srv.nTimeouts.Load(), "a write cut by Close is not a timeout")
}

func TestIsClientReport(t *testing.T) {
	rr := ReceiverReport(7, nil)
	sr := BuildSenderReport(7, time.Unix(1, 0), 1, 1, 1)
	bye := BuildBye(7, "c", nil) // RR + SDES + BYE
	assert.True(t, isClientReport(rr))
	assert.True(t, isClientReport(sr))
	assert.True(t, isClientReport(bye))
	assert.True(t, isClientReport(append(append([]byte{}, rr...), 0xff)), "trailing bytes after a valid lead")
	for _, b := range [][]byte{nil, {0x80}, rr[:6], bye[8:], {0x80, 96, 0, 1, 0, 0, 0, 0}, {0x40, rtcpRR, 0, 1, 0, 0, 0, 7}} {
		assert.False(t, isClientReport(b), "% x", b)
	}
}

// TestInterleavedRTCPLiveness checks that only a client report on one of
// the session's RTCP channels counts as a sign of life on TCP.
func TestInterleavedRTCPLiveness(t *testing.T) {
	now := time.Unix(100, 0)
	srv := NewServer(nil, ServerConfig{Now: func() time.Time { return now }})
	frame := func(ch byte, b []byte) []byte {
		return append([]byte{'$', ch, byte(len(b) >> 8), byte(len(b))}, b...)
	}
	rr := ReceiverReport(7, nil)
	for _, tc := range []struct {
		name  string
		in    []byte
		alive bool
	}{
		{"report on the RTCP channel", frame(1, rr), true},
		{"garbage on the RTCP channel", frame(1, []byte{1, 2, 3, 4}), false},
		{"report on the RTP channel", frame(0, rr), false},
		{"report on another channel", frame(5, rr), false},
	} {
		c := &serverConn{srv: srv, br: bufio.NewReader(bytes.NewReader(tc.in))}
		c.sess = &ServerSession{c: c, tracks: make([]*serverTrack, 1)}
		c.sess.addTrack(0, "u", transportTCP, Transport{Interleaved: [2]int{0, 1}})
		c.sess.lastSeen.Store(0)
		require.NoError(t, c.readFrame(), tc.name)
		assert.Equal(t, tc.alive, c.sess.lastSeen.Load() == now.UnixNano(), tc.name)
	}
	assert.Equal(t, uint64(1), srv.nProtocol.Load(), "only the garbage report is a protocol error")
}

// TestServerUDPByeDeadline checks that the UDP BYE is written under the
// WriteTimeout deadline of every other UDP write, not under the short BYE
// deadline given for TCP: a deadline already past must not fail it.
func TestServerUDPByeDeadline(t *testing.T) {
	rtpC, rtcpC, err := listenUDPPair("127.0.0.1")
	require.NoError(t, err)
	defer rtpC.Close()
	defer rtcpC.Close()
	peerRTP, peerRTCP, err := listenUDPPair("127.0.0.1")
	require.NoError(t, err)
	defer peerRTP.Close()
	defer peerRTCP.Close()

	srv := NewServer(nil, ServerConfig{WriteTimeout: 5 * time.Second})
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &serverConn{srv: srv, nc: a, peer: net.IPv4(127, 0, 0, 1).To4(), ipKey: "127.0.0.1", since: time.Now()}
	sess := newServerSession(c, "live/cam", []TrackDesc{{Codec: CodecH264, Payload: 96, ClockRate: 90000}})
	sess.pair = &udpPair{rtp: rtpC, rtcp: rtcpC}
	ports := [2]int{peerRTP.LocalAddr().(*net.UDPAddr).Port, peerRTCP.LocalAddr().(*net.UDPAddr).Port}
	sess.addTrack(0, "rtsp://x/live/cam/trackID=0", transportUDP, Transport{ClientPort: ports})

	c.wmu.Lock()
	sess.sendByeLocked(time.Now().Add(-time.Second))
	c.wmu.Unlock()

	buf := make([]byte, 1500)
	require.NoError(t, peerRTCP.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, _, err := peerRTCP.ReadFromUDP(buf)
	require.NoError(t, err, "the BYE is sent under the WriteTimeout deadline")
	assert.Equal(t, []byte{201, 202, 203}, rtcpTypeBytes(buf[:n]))
}

// rtcpTypeBytes returns the packet type of each packet of a compound.
func rtcpTypeBytes(b []byte) []byte {
	var out []byte
	for len(b) >= 4 {
		out = append(out, b[1])
		b = b[(int(b[2])<<8|int(b[3])+1)*4:]
	}
	return out
}

// TestServerPlayingReaderSmall checks that a connection that plays reads
// requests through a 4 KiB buffer, keeping what was already buffered, and
// still reads a large interleaved frame whole.
func TestServerPlayingReaderSmall(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	srv := NewServer(nil, ServerConfig{})
	c := &serverConn{srv: srv, nc: a, br: bufio.NewReaderSize(a, 64<<10)}
	go func() {
		_, _ = b.Write([]byte("OPTIONS * RTSP/1.0\r\nCSeq: 9\r\n\r\n"))
	}()
	_, err := c.br.Peek(1)
	require.NoError(t, err)
	c.playing = true
	c.shrinkReader()
	assert.Equal(t, playingReaderBytes, c.br.Size())
	req, err := ReadRequest(c.br, c.requestLimits())
	require.NoError(t, err)
	assert.Equal(t, "9", req.Header.Get("CSeq"))
	assert.Equal(t, playingReaderBytes, c.requestLimits().HeaderBytes, "request headers are limited to the buffer size")

	// A 60000-byte frame on a session channel, read through the small buffer.
	rr := ReceiverReport(7, nil)
	payload := append(append([]byte{}, rr...), make([]byte, 60000-len(rr))...)
	frame := append([]byte{'$', 1, byte(len(payload) >> 8), byte(len(payload))}, payload...)
	c.sess = &ServerSession{c: c, tracks: make([]*serverTrack, 1)}
	c.sess.addTrack(0, "u", transportTCP, Transport{Interleaved: [2]int{0, 1}})
	go func() { _, _ = b.Write(frame) }()
	require.NoError(t, c.readFrame())
	assert.Zero(t, srv.nProtocol.Load())
}
