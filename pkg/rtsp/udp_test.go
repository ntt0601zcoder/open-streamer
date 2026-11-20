package rtsp_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp/rtsptest"
)

func udpConfig() rtsp.Config {
	cfg := testConfig()
	cfg.Transport = "udp"
	return cfg
}

// setupTransports returns the parsed Transport headers of the SETUP requests
// the server received, in order.
func setupTransports(t *testing.T, s *rtsptest.Server) []rtsp.Transport {
	t.Helper()
	reqs := requestsOf(s, "SETUP")
	out := make([]rtsp.Transport, 0, len(reqs))
	for _, r := range reqs {
		tr, err := rtsp.ParseTransport(r.Header.Get("Transport"))
		require.NoError(t, err)
		out = append(out, tr)
	}
	return out
}

func TestClientPlayUDP(t *testing.T) {
	src := fixture(t)
	s := newServer(t, rtsptest.ServerOptions{SendRTPInfo: true, SRAfter: 100 * time.Millisecond, Seed: 1})
	cfg := udpConfig()
	cfg.ReportInterval = 200 * time.Millisecond
	c := dial(t, s.URL("/live/stream"), cfg)

	trs := setupTransports(t, s)
	require.Len(t, trs, 2)
	for i, tr := range trs {
		assert.Equal(t, "RTP/AVP", tr.Protocol, "setup %d", i)
		assert.True(t, tr.Unicast)
		assert.Zero(t, tr.ClientPort[0]%2, "RTP port even")
		assert.Equal(t, tr.ClientPort[0]+1, tr.ClientPort[1], "RTCP port next")
	}

	d := c.Description()
	info := c.RTPInfo()
	video, err := rtsp.NewDepacketizer(d.Media[0], 0)
	require.NoError(t, err)
	var aus []rtsp.AU
	var seqs [2]rtsp.SeqTracker
	first := [2]bool{}
	srs := 0
	pkts := readUntil(t, c, 3*time.Second, func(p rtsp.Packet) bool {
		if p.RTCP != nil {
			srs += len(rtsp.ParseSenderReports(p.RTCP))
			return false
		}
		if !first[p.Track] {
			first[p.Track] = true
			assert.Equal(t, info[p.Track].Seq, p.RTP.SequenceNumber, "track %d first seq", p.Track)
		}
		gap, reset, dup := seqs[p.Track].Next(p.RTP.SequenceNumber, p.RTP.SSRC)
		assert.Zero(t, gap)
		assert.False(t, reset || dup)
		if p.Track == 0 {
			aus = append(aus, video.Push(p.RTP, p.Arrival)...)
		}
		return len(aus) >= 10 && srs >= 2
	})
	assert.Positive(t, rtpCount(pkts, 1), "audio packets")
	for i := range aus {
		want := src[0].AUs[i].NALUs
		require.Len(t, aus[i].NALUs, len(want), "au %d", i)
		for j := range want {
			assert.True(t, bytes.Equal(want[j], aus[i].NALUs[j]), "au %d nal %d", i, j)
		}
	}
	readFor(t, c, 300*time.Millisecond)

	// Receiver reports go to the server's RTCP ports.
	rrs := map[int]int{}
	for _, f := range s.FromClient() {
		require.Equal(t, 1, f.Channel%2, "RTCP socket")
		require.GreaterOrEqual(t, len(f.Data), 8)
		require.Equal(t, byte(201), f.Data[1])
		rrs[f.Channel/2]++
	}
	assert.Positive(t, rrs[0])
	assert.Positive(t, rrs[1])
	assert.Equal(t, 4, s.Punches(), "one punch per server port")
	st := c.Stats()
	assert.Zero(t, st.DupPackets)
	assert.Zero(t, st.Dropped)
	assert.Zero(t, st.FallbackTCP)
}

func TestUDPReorderAndLoss(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{
		Media: []rtsptest.TrackSource{fastSource()}, SendRTPInfo: true, Seed: 7,
		UDPLoss: 0.01, UDPSwap: 0.01, UDPDup: 0.01,
		UDPGapAfter: 800 * time.Millisecond, UDPGap: 2 * time.Second,
	})
	cfg := udpConfig()
	cfg.IdleTimeout = 3 * time.Second
	c := dial(t, s.URL("/stream"), cfg)
	seq0 := c.RTPInfo()[0].Seq

	var tr rtsp.SeqTracker
	lost, n := 0, 0
	var maxWait time.Duration
	var last uint16
	end := time.Now().Add(3800 * time.Millisecond)
	for time.Now().Before(end) {
		p, err := c.ReadPacket()
		require.NoError(t, err)
		if p.RTP == nil {
			continue
		}
		maxWait = max(maxWait, time.Since(p.Arrival))
		if n == 0 {
			tr.Next(seq0-1, p.RTP.SSRC)
		}
		n++
		gap, reset, dup := tr.Next(p.RTP.SequenceNumber, p.RTP.SSRC)
		require.False(t, reset, "packet %d", n)
		require.False(t, dup, "packet %d out of order or duplicated", n)
		lost += gap
		last = p.RTP.SequenceNumber
	}
	upTo := last - seq0
	var wantLost, wantDup, swaps, gapped int
	for _, in := range s.Injected() {
		if in.Seq-seq0 > upTo {
			continue
		}
		switch in.Kind {
		case rtsptest.InjectLoss:
			wantLost++
		case rtsptest.InjectGap:
			wantLost++
			gapped++
		case rtsptest.InjectSwap:
			swaps++
		case rtsptest.InjectDup:
			wantDup++
		}
	}
	t.Logf("read %d, lost %d (injected %d, gap %d), dups %d (injected %d), swaps %d, reordered %d, max wait %v",
		n, lost, wantLost, gapped, c.Stats().DupPackets, wantDup, swaps, c.Stats().Reordered, maxWait)
	require.Greater(t, gapped, 1000, "the 2 s gap")
	require.Positive(t, wantDup)
	require.Positive(t, swaps)
	assert.InEpsilon(t, wantLost, lost, 0.1, "loss")
	assert.InEpsilon(t, wantDup, int(c.Stats().DupPackets), 0.1, "duplicates")
	assert.Positive(t, c.Stats().Reordered)
	assert.LessOrEqual(t, maxWait, 260*time.Millisecond, "stall")
}

func TestUDPIgnoresStrangers(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{Media: []rtsptest.TrackSource{fastSource()}})
	c := dial(t, s.URL("/stream"), udpConfig())
	ports := setupTransports(t, s)[0].ClientPort
	const stranger = 0xdeadbeef

	forge := func(from string) {
		pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", from+":0")
		if err != nil {
			t.Logf("cannot bind %s: %v", from, err)
			return
		}
		defer pc.Close()
		for i := 0; i < 50; i++ {
			raw, err := (&rtp.Packet{Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: uint16(i), SSRC: stranger,
			}, Payload: []byte{0x41, 0x9a}}).Marshal()
			require.NoError(t, err)
			_, err = pc.WriteTo(raw, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports[0]})
			require.NoError(t, err)
			sr := make([]byte, 28)
			sr[0], sr[1] = 0x80, 200
			binary.BigEndian.PutUint16(sr[2:], 6)
			binary.BigEndian.PutUint32(sr[4:], stranger)
			_, err = pc.WriteTo(sr, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports[1]})
			require.NoError(t, err)
		}
	}
	forge("127.0.0.1") // the server's address, another port
	forge("127.0.0.2") // another address
	pkts := readFor(t, c, 500*time.Millisecond)
	require.Positive(t, rtpCount(pkts, 0))
	for _, p := range pkts {
		require.Nil(t, p.RTCP, "no RTCP from the server, so any is a stranger's")
		require.NotEqual(t, uint32(stranger), p.RTP.SSRC)
	}
}

func TestAutoFallsBackToTCP(t *testing.T) {
	autoConfig := func() rtsp.Config {
		cfg := testConfig()
		cfg.Transport = "auto"
		cfg.UDPFallback = 300 * time.Millisecond
		return cfg
	}
	protocols := func(s *rtsptest.Server) []string {
		trs := setupTransports(t, s)
		out := make([]string, 0, len(trs))
		for _, tr := range trs {
			out = append(out, tr.Protocol)
		}
		return out
	}
	t.Run("no RTP over UDP", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{DropUDP: true})
		start := time.Now()
		c := dial(t, s.URL("/stream"), autoConfig())
		assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
		readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
		assert.Equal(t, uint64(1), c.Stats().FallbackTCP)
		assert.Equal(t, []string{"RTP/AVP", "RTP/AVP", "RTP/AVP/TCP", "RTP/AVP/TCP"}, protocols(s))
		require.Eventually(t, func() bool { return len(requestsOf(s, "TEARDOWN")) == 1 }, time.Second, 10*time.Millisecond)
	})
	t.Run("UDP refused", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{NoUDP: true})
		start := time.Now()
		c := dial(t, s.URL("/stream"), autoConfig())
		assert.Less(t, time.Since(start), 300*time.Millisecond)
		readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
		assert.Equal(t, uint64(1), c.Stats().FallbackTCP)
		assert.Equal(t, []string{"RTP/AVP", "RTP/AVP/TCP", "RTP/AVP/TCP"}, protocols(s))
	})
	t.Run("server answers with interleaved TCP", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{UDPAsTCP: true})
		c := dial(t, s.URL("/stream"), autoConfig())
		readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
		assert.Equal(t, uint64(1), c.Stats().FallbackTCP)
		assert.Equal(t, []string{"RTP/AVP", "RTP/AVP/TCP", "RTP/AVP/TCP"}, protocols(s))
	})
	for _, code := range []int{400, 406, 500} {
		t.Run(fmt.Sprintf("UDP SETUP status %d", code), func(t *testing.T) {
			s := newServer(t, rtsptest.ServerOptions{UDPStatus: code})
			c := dial(t, s.URL("/stream"), autoConfig())
			readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
			assert.Equal(t, uint64(1), c.Stats().FallbackTCP)
			assert.Equal(t, []string{"RTP/AVP", "RTP/AVP/TCP", "RTP/AVP/TCP"}, protocols(s))
		})
	}
	t.Run("udp only reports the sentinel", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{UDPAsTCP: true})
		_, err := rtsp.Dial(context.Background(), s.URL("/stream"), udpConfig())
		require.ErrorIs(t, err, rtsp.ErrNotUDP)
	})
	t.Run("UDP works", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{})
		c := dial(t, s.URL("/stream"), autoConfig())
		readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
		assert.Zero(t, c.Stats().FallbackTCP)
		assert.Equal(t, []string{"RTP/AVP", "RTP/AVP"}, protocols(s))
	})
	t.Run("ctx ends the wait", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{DropUDP: true})
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		cfg := autoConfig()
		cfg.UDPFallback = 5 * time.Second
		start := time.Now()
		_, err := rtsp.Dial(ctx, s.URL("/stream"), cfg)
		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), time.Second)
	})
}

func TestUDPServerClose(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{})
	cfg := udpConfig()
	cfg.IdleTimeout = 5 * time.Second
	c := dial(t, s.URL("/stream"), cfg)
	readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
	s.Close()
	errc := make(chan error, 1)
	go func() {
		for {
			if _, err := c.ReadPacket(); err != nil {
				errc <- err
				return
			}
		}
	}()
	select {
	case err := <-errc:
		require.Error(t, err)
		assert.NotErrorIs(t, err, rtsp.ErrIdle)
		assert.NotErrorIs(t, err, rtsp.ErrClosed)
	case <-time.After(time.Second):
		t.Fatal("control connection EOF did not end the session")
	}
}

func TestUDPSocketsClosed(t *testing.T) {
	src := fixture(t)
	before := runtime.NumGoroutine()

	s := rtsptest.NewServer(t, rtsptest.ServerOptions{Media: src})
	cfg := udpConfig()
	cfg.ReportInterval = 100 * time.Millisecond
	c, err := rtsp.Dial(context.Background(), s.URL("/stream"), cfg)
	require.NoError(t, err)
	readFor(t, c, 300*time.Millisecond)
	trs := setupTransports(t, s)
	require.NoError(t, c.Close())
	_, err = c.ReadPacket()
	require.ErrorIs(t, err, rtsp.ErrClosed)
	for _, tr := range trs {
		for _, port := range tr.ClientPort {
			pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", (&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}).String())
			require.NoError(t, err, "port %d reusable", port)
			pc.Close()
		}
	}

	// A UDP Dial that fails at SETUP, after binding its sockets.
	bad := rtsptest.NewServer(t, rtsptest.ServerOptions{Media: src, NoUDP: true})
	_, err = rtsp.Dial(context.Background(), bad.URL("/stream"), cfg)
	var se *rtsp.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, 461, se.Code)
	s.Close()
	bad.Close()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: before %d, now %d\n%s", before, n, stacks())
	}
}

// TestUDPSingleServerPort checks that server_port=x means RTCP on x+1:
// sender reports from x+1 are accepted and receiver reports go there.
func TestUDPSingleServerPort(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{SingleServerPort: true, SRAfter: 100 * time.Millisecond, Seed: 3})
	cfg := udpConfig()
	cfg.ReportInterval = 100 * time.Millisecond
	c := dial(t, s.URL("/stream"), cfg)
	srs := 0
	readUntil(t, c, 3*time.Second, func(p rtsp.Packet) bool {
		if p.RTCP != nil {
			srs += len(rtsp.ParseSenderReports(p.RTCP))
		}
		return srs >= 2
	})
	require.Eventually(t, func() bool {
		rrs := 0
		for _, f := range s.FromClient() {
			if f.Channel%2 == 1 && len(f.Data) >= 8 && f.Data[1] == 201 {
				rrs++
			}
		}
		return rrs >= 2
	}, 2*time.Second, 20*time.Millisecond, "receiver reports reach the RTCP port x+1")
	assert.Equal(t, 4, s.Punches(), "one punch per server port")
}
