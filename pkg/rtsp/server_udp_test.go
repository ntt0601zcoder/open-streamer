package rtsp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// startUDPServer serves h on 127.0.0.1:0 with a UDP pair on 127.0.0.1 and
// returns once the pair is bound.
func startUDPServer(t *testing.T, h rtsp.Handler, cfg rtsp.ServerConfig) (*rtsp.Server, string) {
	t.Helper()
	cfg.UDPHost = "127.0.0.1"
	srv, addr := startServer(t, h, cfg)
	require.Eventually(t, func() bool { return srv.UDPPorts()[0] != 0 }, 2*time.Second, 5*time.Millisecond)
	return srv, addr
}

// udpPeer is the RTP/RTCP socket pair of a raw UDP client.
type udpPeer struct {
	rtp, rtcp *net.UDPConn
}

func newUDPPeer(t *testing.T) *udpPeer {
	t.Helper()
	a, b, err := rtsp.ListenUDPPair("127.0.0.1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return &udpPeer{rtp: a, rtcp: b}
}

func (p *udpPeer) ports() [2]int {
	return [2]int{p.rtp.LocalAddr().(*net.UDPAddr).Port, p.rtcp.LocalAddr().(*net.UDPAddr).Port}
}

func (p *udpPeer) offer() string {
	ps := p.ports()
	return fmt.Sprintf("RTP/AVP;unicast;client_port=%d-%d", ps[0], ps[1])
}

// readDatagram returns the next datagram on c and its source port, or an error
// after d.
func readDatagram(c *net.UDPConn, d time.Duration) ([]byte, int, error) {
	buf := make([]byte, 64<<10)
	_ = c.SetReadDeadline(time.Now().Add(d))
	n, from, err := c.ReadFromUDP(buf)
	if err != nil {
		return nil, 0, err
	}
	return buf[:n], from.Port, nil
}

func loopback(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

func TestServerPlayUDP(t *testing.T) {
	h := newHandler()
	h.play = producer(5 * time.Millisecond)
	srv, addr := startUDPServer(t, h, rtsp.ServerConfig{SessionTimeout: 300 * time.Millisecond})
	ports := srv.UDPPorts()
	assert.Zero(t, ports[0]%2, "RTP port even")
	assert.Equal(t, ports[0]+1, ports[1], "RTCP port next")

	cfg := udpConfig()
	cfg.ReportInterval = 100 * time.Millisecond
	c := dial(t, "rtsp://"+addr+"/live/cam", cfg)
	s := h.session(t)
	assert.True(t, s.UDP())

	// The client's receiver reports keep the session alive past several
	// session timeouts.
	ps := readFor(t, c, 1200*time.Millisecond)
	select {
	case <-s.Done():
		t.Fatalf("session ended despite receiver reports: %v", s.Err())
	default:
	}
	info := c.RTPInfo()
	require.Len(t, info, 2)
	first := map[int]*rtp.Packet{}
	last := map[int]uint16{}
	count := map[int]int{}
	for _, p := range ps {
		if p.RTP == nil {
			continue
		}
		count[p.Track]++
		ssrc, _, _ := s.Origin(p.Track)
		assert.Equal(t, ssrc, p.RTP.SSRC, "track %d SSRC", p.Track)
		if _, ok := first[p.Track]; !ok {
			first[p.Track] = p.RTP
		} else {
			assert.Equal(t, last[p.Track]+1, p.RTP.SequenceNumber, "track %d: every packet in order", p.Track)
		}
		last[p.Track] = p.RTP.SequenceNumber
	}
	require.Contains(t, first, 0)
	require.Contains(t, first, 1)
	assert.Greater(t, count[0], 100)
	assert.Greater(t, count[1], 50)
	for tr := range 2 {
		_, seq, base := s.Origin(tr)
		assert.Equal(t, seq, first[tr].SequenceNumber, "track %d: the first packet arrived", tr)
		assert.Equal(t, seq, info[tr].Seq)
		assert.Equal(t, base, info[tr].RTPTime)
	}
	assert.Zero(t, c.Stats().Dropped)
	st := srv.Stats()
	assert.Zero(t, st.Timeouts)
	assert.Zero(t, st.UDPStrangers, "the client's punches and reports come from its ports")
	assert.Equal(t, 1, st.Sessions)
}

func TestServerUDPSessionTimeout(t *testing.T) {
	h := newHandler()
	h.play = producer(10 * time.Millisecond)
	srv, addr := startUDPServer(t, h, rtsp.ServerConfig{SessionTimeout: 300 * time.Millisecond})
	ports := srv.UDPPorts()

	// A silent client: no request, no RTCP, its TCP connection open.
	r := dialRaw(t, addr)
	p := newUDPPeer(t)
	resp := r.setup(0, p.offer())
	require.Equal(t, 200, resp.StatusCode)
	tr, err := rtsp.ParseTransport(resp.Header.Get("Transport"))
	require.NoError(t, err)
	assert.Equal(t, "RTP/AVP", tr.Protocol)
	assert.True(t, tr.Unicast)
	assert.Equal(t, p.ports(), tr.ClientPort, "client_port echoed")
	assert.Equal(t, ports, tr.ServerPort)
	require.Equal(t, 200, r.play().StatusCode)
	start := time.Now()
	s := h.session(t)
	ssrc, _, _ := s.Origin(0)
	assert.Equal(t, ssrc, tr.SSRC)
	assert.True(t, tr.HasSSRC)

	b, from, err := readDatagram(p.rtp, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, ports[0], from, "RTP comes from the server's RTP port")
	var pkt rtp.Packet
	require.NoError(t, pkt.Unmarshal(b))
	assert.Equal(t, ssrc, pkt.SSRC)

	require.True(t, ended(s, 2*time.Second), "a silent UDP session must time out")
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
	assert.Error(t, s.Err())
	assert.True(t, r.eof(time.Second), "the timeout closes the connection")
	assert.Equal(t, uint64(1), srv.Stats().Timeouts)

	// RTCP from the client's RTCP port keeps a session alive; when it
	// stops the session times out.
	r = dialRaw(t, addr)
	p = newUDPPeer(t)
	require.Equal(t, 200, r.setup(0, p.offer()).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s = h.session(t)
	rr := rtsp.ReceiverReport(1, nil)
	for range 10 {
		_, err := p.rtcp.WriteToUDP(rr, loopback(ports[1]))
		require.NoError(t, err)
		time.Sleep(80 * time.Millisecond)
	}
	select {
	case <-s.Done():
		t.Fatalf("session ended despite RTCP: %v", s.Err())
	default:
	}
	require.True(t, ended(s, 2*time.Second), "the session times out once RTCP stops")

	// Closing the TCP connection ends a UDP session at once.
	r = dialRaw(t, addr)
	p = newUDPPeer(t)
	require.Equal(t, 200, r.setup(0, p.offer()).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s = h.session(t)
	start = time.Now()
	_ = r.c.Close()
	require.True(t, ended(s, time.Second))
	assert.Less(t, time.Since(start), 200*time.Millisecond)
}

func TestServerUDPStrangersIgnored(t *testing.T) {
	h := newHandler()
	h.play = producer(10 * time.Millisecond)
	srv, addr := startUDPServer(t, h, rtsp.ServerConfig{SessionTimeout: 400 * time.Millisecond})
	ports := srv.UDPPorts()
	r := dialRaw(t, addr)
	p := newUDPPeer(t)
	require.Equal(t, 200, r.setup(0, p.offer()).StatusCode)
	require.Equal(t, 200, r.play().StatusCode)
	s := h.session(t)

	// A stranger on the same IP but another port: its RTCP neither keeps
	// the session alive nor gets it anything.
	stranger, err := net.ListenUDP("udp", loopback(0))
	require.NoError(t, err)
	defer stranger.Close()
	rr := rtsp.ReceiverReport(1, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tk := time.NewTicker(30 * time.Millisecond)
		defer tk.Stop()
		for {
			_, _ = stranger.WriteToUDP(rr, loopback(ports[1]))
			_, _ = stranger.WriteToUDP([]byte{0x80, 96, 0, 1}, loopback(ports[0]))
			select {
			case <-stop:
				return
			case <-tk.C:
			}
		}
	})
	defer func() { close(stop); wg.Wait() }()
	_, _, err = readDatagram(stranger, 200*time.Millisecond)
	var ne net.Error
	require.ErrorAs(t, err, &ne, "the stranger receives nothing")
	assert.True(t, ne.Timeout())
	require.True(t, ended(s, 2*time.Second), "a stranger's RTCP must not keep the session alive")
	assert.Greater(t, srv.Stats().UDPStrangers, uint64(3))
}

func TestServerUDPWithoutPair461(t *testing.T) {
	h := newHandler()
	h.play = producer(5 * time.Millisecond)
	srv, addr := startServer(t, h, rtsp.ServerConfig{})
	assert.Equal(t, [2]int{}, srv.UDPPorts())
	r := dialRaw(t, addr)
	assert.Equal(t, 461, r.setup(0, "RTP/AVP;unicast;client_port=5000-5001").StatusCode)
	assert.Equal(t, 461, r.setup(0, "RTP/AVP/UDP;unicast;client_port=5000-5001").StatusCode)

	_, err := rtsp.Dial(context.Background(), "rtsp://"+addr+"/live/cam", udpConfig())
	var se *rtsp.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, 461, se.Code)

	cfg := testConfig()
	cfg.Transport = "auto"
	c := dial(t, "rtsp://"+addr+"/live/cam", cfg)
	assert.Equal(t, uint64(1), c.Stats().FallbackTCP)
	readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
}

func TestServerUDPPortsInUse(t *testing.T) {
	h := newHandler()
	_, addr := startUDPServer(t, h, rtsp.ServerConfig{})
	p := newUDPPeer(t)
	r := dialRaw(t, addr)
	require.Equal(t, 200, r.setup(0, p.offer()).StatusCode)
	// Another session asking for the same client ports from the same IP.
	r2 := dialRaw(t, addr)
	assert.Equal(t, 461, r2.setup(0, p.offer()).StatusCode)
	// One session uses one transport for all its tracks.
	assert.Equal(t, 461, r.setup(1, tcpOffer(2, 3)).StatusCode)
	// The same session may reuse its own ports for another track.
	assert.Equal(t, 200, r.setup(1, p.offer()).StatusCode)
}

func TestServerUDPByeOnClose(t *testing.T) {
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
	srv, addr := startUDPServer(t, h, rtsp.ServerConfig{})
	ports := srv.UDPPorts()
	check := func(close func(s *rtsp.ServerSession)) {
		t.Helper()
		r := dialRaw(t, addr)
		p0, p1 := newUDPPeer(t), newUDPPeer(t)
		require.Equal(t, 200, r.setup(0, p0.offer()).StatusCode)
		require.Equal(t, 200, r.setup(1, p1.offer()).StatusCode)
		require.Equal(t, 200, r.play().StatusCode)
		s := h.session(t)
		sr := <-srs
		_, from, err := readDatagram(p0.rtp, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, ports[0], from)
		b, from, err := readDatagram(p0.rtcp, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, ports[1], from, "RTCP comes from the server's RTCP port")
		assert.Equal(t, sr, b)
		close(s)
		b, from, err = readDatagram(p0.rtcp, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, ports[1], from)
		assert.Equal(t, []byte{200, 202, 203}, rtcpTypes(b))
		assert.Equal(t, sr, b[:len(sr)], "the last SR heads the compound")
		b, _, err = readDatagram(p1.rtcp, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, []byte{201, 202, 203}, rtcpTypes(b))
		ssrc1, _, _ := s.Origin(1)
		assert.Equal(t, []uint32{ssrc1}, rtsp.ParseReceiverReports(b))
		for _, c := range []*net.UDPConn{p0.rtcp, p1.rtcp} {
			_, _, err = readDatagram(c, 100*time.Millisecond)
			assert.Error(t, err, "one BYE compound per track")
		}
		require.True(t, ended(s, time.Second))
		assert.ErrorIs(t, s.Err(), rtsp.ErrSessionClosed)
		assert.True(t, r.eof(time.Second))
		assert.Error(t, s.WriteRTCP(0, sr))
	}
	check(func(s *rtsp.ServerSession) { s.Close() })
	// Server.Close ends playing sessions the same way.
	check(func(*rtsp.ServerSession) { require.NoError(t, srv.Close()) })
}

// holdOpenProxy forwards TCP connections to target but keeps the client's
// side open after the server closes its side, so that only RTCP BYE can
// end a UDP player.
func holdOpenProxy(t *testing.T, target string) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			down, err := l.Accept()
			if err != nil {
				return
			}
			up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", target)
			if err != nil {
				_ = down.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, down, up)
			mu.Unlock()
			wg.Go(func() { _, _ = io.Copy(up, down) })
			wg.Go(func() { _, _ = io.Copy(down, up) })
		}
	})
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return l.Addr().String()
}

func TestServerFFprobeUDP(t *testing.T) {
	ffprobe := requirePeer(t, "ffprobe")
	_, addr := startUDPServer(t, newFixtureHandler(t), rtsp.ServerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-rtsp_transport", "udp",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", "rtsp://"+addr+"/live/cam")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "ffprobe: %s", stderr.String())
	assert.Equal(t, []string{"h264", "aac"}, strings.Fields(string(out)), "ffprobe: %s", stderr.String())
}

// TestServerFFmpegEOFOnByeUDP plays over UDP through a proxy that keeps
// ffmpeg's TCP connection open, so only the BYE compounds can end it.
func TestServerFFmpegEOFOnByeUDP(t *testing.T) {
	ffmpeg := requirePeer(t, "ffmpeg")
	run := func(closeAtEnd bool, wait time.Duration) (string, time.Duration, error) {
		f := newFixtureHandler(t)
		f.speed, f.closeAtEnd = 8, closeAtEnd
		_, addr := startUDPServer(t, f, rtsp.ServerConfig{})
		proxy := holdOpenProxy(t, addr)
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-rtsp_transport", "udp",
			"-i", "rtsp://"+proxy+"/live/cam", "-f", "null", "-")
		cmd.Stderr = &stderr
		start := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return stderr.String(), time.Since(start), err
	}
	stderr, took, err := run(true, 10*time.Second)
	require.NoError(t, err, "ffmpeg: %s", stderr)
	assert.Less(t, took, 5*time.Second)
	assert.Empty(t, stderr)

	// Control: without BYE ffmpeg is still playing well after the media
	// ended (0.75 s at 8x).
	_, _, err = run(false, 2500*time.Millisecond)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "without BYE ffmpeg must keep playing: %v", err)
}

func TestServerUDPPairClosed(t *testing.T) {
	before := runtime.NumGoroutine()
	// A pair that cannot be bound fails Serve.
	l0, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	bad := rtsp.NewServer(newHandler(), rtsp.ServerConfig{UDPHost: "192.0.2.1"})
	err = bad.Serve(l0)
	require.Error(t, err)
	assert.NotErrorIs(t, err, rtsp.ErrServerClosed)
	assert.Equal(t, [2]int{}, bad.UDPPorts())
	_ = l0.Close()
	require.NoError(t, bad.Close())

	h := newHandler()
	h.play = producer(5 * time.Millisecond)
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := rtsp.NewServer(h, rtsp.ServerConfig{UDPHost: "127.0.0.1"})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	c, err := rtsp.Dial(context.Background(), "rtsp://"+l.Addr().String()+"/live/cam", udpConfig())
	require.NoError(t, err)
	readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
	ports := srv.UDPPorts()
	require.NotZero(t, ports[0])

	require.NoError(t, srv.Close())
	assert.ErrorIs(t, <-served, rtsp.ErrServerClosed)
	assert.Equal(t, [2]int{}, srv.UDPPorts(), "Close releases the pair")
	_ = c.Close()
	for _, port := range ports {
		pc, err := net.ListenUDP("udp", loopback(port))
		require.NoError(t, err, "port %d released", port)
		_ = pc.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: before %d, now %d\n%s", before, n, stacks())
	}
}
