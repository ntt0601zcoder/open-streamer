package srtout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

const waitFor = 10 * time.Second

// source returns a fixture's demuxed frames, in source time, and its track
// kinds.
func source(t *testing.T, name string) ([]media.Frame, []media.Kind) {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/fixtures/" + name)
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out, d.TrackKinds()
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

// looped returns n back-to-back copies of a fixture in stream time, through
// one Clock.
func looped(t *testing.T, name string, n int) []media.Frame {
	t.Helper()
	src, kinds := source(t, name)
	lo, hi := src[0].DTS, src[0].DTS
	for _, f := range src {
		lo, hi = min(lo, f.DTS), max(hi, f.DTS)
	}
	span := hi - lo + 3600
	c := clock.New(clock.Options{Kinds: kinds})
	var out []media.Frame
	for i := range n {
		for _, f := range src {
			f.PTS += int64(i) * span
			f.DTS += int64(i) * span
			out = append(out, c.Push(f)...)
		}
	}
	return append(out, c.Flush()...)
}

// configOf returns the first config of the given codec in a fixture.
func configOf(t *testing.T, name string, codec media.Codec) *media.TrackConfig {
	t.Helper()
	src, _ := source(t, name)
	for _, f := range src {
		if f.Config != nil && f.Config.Codec == codec {
			return f.Config
		}
	}
	t.Fatalf("%s has no %s track", name, codec)
	return nil
}

// splitAtKey returns the frames before the n-th video keyframe (1-based)
// and the rest.
func splitAtKey(frames []media.Frame, n int) (head, rest []media.Frame) {
	keys := 0
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			if keys++; keys == n {
				return frames[:i], frames[i:]
			}
		}
	}
	return frames, nil
}

// feed writes frames to h in a goroutine, paced at speed times real time
// by DTS, until they run out or ctx ends. The channel yields the longest
// Write, then closes.
func feed(ctx context.Context, h *hub.Hub, frames []media.Frame, speed float64) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	go func() {
		defer close(out)
		var longest time.Duration
		defer func() { out <- longest }()
		if len(frames) == 0 {
			return
		}
		start, base := time.Now(), frames[0].DTS
		for _, f := range frames {
			due := start.Add(time.Duration(float64(f.DTS-base) / media.ClockRate / speed * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				tm := time.NewTimer(d)
				select {
				case <-ctx.Done():
					tm.Stop()
					return
				case <-tm.C:
				}
			}
			if ctx.Err() != nil {
				return
			}
			t0 := time.Now()
			h.Write(f)
			longest = max(longest, time.Since(t0))
		}
	}()
	return out
}

func countKind(frames []media.Frame, k media.Kind) int {
	n := 0
	for _, f := range frames {
		if f.Kind() == k {
			n++
		}
	}
	return n
}

// env is a Server on 127.0.0.1 for stream "s" (h) and stream "q", whose hub
// never goes live.
type env struct {
	h    *hub.Hub
	srv  *Server
	addr string
}

func start(t *testing.T, opt Options) *env {
	t.Helper()
	return startWrapped(t, opt, nil)
}

// startWrapped is start with the Server's wrap hook set, before it serves.
func startWrapped(t *testing.T, opt Options, wrap func(io.Writer) io.Writer) *env {
	t.Helper()
	h, q := hub.New(hub.Options{}), hub.New(hub.Options{})
	srv := New(func(name string) (*hub.Hub, bool) {
		switch name {
		case "s":
			return h, true
		case "q":
			return q, true
		case "nil":
			return nil, true
		}
		return nil, false
	}, opt)
	srv.wrap = wrap
	require.NoError(t, srv.Listen("127.0.0.1:0"))
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		select {
		case err := <-served:
			assert.ErrorIs(t, err, ErrServerClosed)
		case <-time.After(waitFor):
			t.Error("Serve did not return after Close")
		}
		h.Close()
		q.Close()
	})
	return &env{h: h, srv: srv, addr: srv.Addr().String()}
}

// dial connects a gosrt caller with stream ID id; mod, when set, edits the
// config.
func dial(addr, id string, mod func(*srt.Config)) (srt.Conn, error) {
	cfg := srt.DefaultConfig()
	cfg.StreamId = id
	if mod != nil {
		mod(&cfg)
	}
	return srt.Dial("srt", addr, cfg)
}

// mustDial connects a receiver of live/s that closes with the test.
func mustDial(t *testing.T, addr string) srt.Conn {
	t.Helper()
	c, err := dial(addr, "live/s", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// capture is what a receiver read before its Read failed or it stopped,
// and the largest single Read: gosrt returns one SRT packet per Read.
type capture struct {
	data []byte
	most int
	err  error
}

// receive reads c in a goroutine until Read fails or stop (when set)
// returns true for the bytes so far; n, when set, tracks the byte count.
func receive(c srt.Conn, n *atomic.Int64, stop func([]byte) bool) <-chan capture {
	out := make(chan capture, 1)
	go func() {
		// Room for the largest test stream up front: a receiver that
		// reallocates while a burst arrives (slowed by the race detector)
		// would overflow its own gosrt read queue.
		data := make([]byte, 0, 8<<20)
		buf := make([]byte, 2048)
		most := 0
		for {
			k, err := c.Read(buf)
			data = append(data, buf[:k]...)
			most = max(most, k)
			if n != nil {
				n.Store(int64(len(data)))
			}
			if err != nil {
				out <- capture{data: data, most: most, err: err}
				return
			}
			if stop != nil && stop(data) {
				out <- capture{data: data, most: most}
				return
			}
		}
	}()
	return out
}

func waitCapture(t *testing.T, ch <-chan capture) capture {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(waitFor):
		t.Fatal("receiver did not finish")
		return capture{}
	}
}

// atLeast stops a receiver at n bytes.
func atLeast(n int) func([]byte) bool { return func(b []byte) bool { return len(b) >= n } }

// ccErrors counts continuity-counter breaks in a TS byte stream: a packet
// with payload whose counter is not its PID's previous one plus one, unless
// it carries the discontinuity indicator. The stream must be whole packets.
func ccErrors(t *testing.T, data []byte) int {
	t.Helper()
	require.Zero(t, len(data)%mpegts.PacketSize, "whole TS packets")
	r := mpegts.NewReader(bytes.NewReader(data))
	last := map[uint16]uint8{}
	errs := 0
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return errs
		}
		require.NoError(t, err)
		if !p.HasPayload {
			continue
		}
		if prev, ok := last[p.PID]; ok && !p.Discontinuity && p.CC != (prev+1)&0x0F {
			errs++
		}
		last[p.PID] = p.CC
	}
}

// readBack demuxes a TS byte stream, following PMT changes.
func readBack(t *testing.T, data []byte) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{FollowPMT: true})
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

func waitStats(t *testing.T, srv *Server, cond func(Stats) bool, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return cond(srv.Stats()) }, waitFor, 5*time.Millisecond, msg)
}

// play writes pre to the hub, connects a receiver through addr (the
// server's or a proxy's), feeds rest at speed, closes the hub and returns
// everything the receiver read.
func play(t *testing.T, e *env, addr, id string, pre, rest []media.Frame, speed float64, connected func(srt.Conn)) []byte {
	t.Helper()
	return playWith(t, e, addr, id, nil, pre, rest, speed, connected)
}

// playWith is play with the receiver's config edited by mod.
func playWith(t *testing.T, e *env, addr, id string, mod func(*srt.Config), pre, rest []media.Frame, speed float64, connected func(srt.Conn)) []byte {
	t.Helper()
	return playCapture(t, e, addr, id, mod, pre, rest, speed, connected).data
}

// playCapture is playWith returning the receiver's whole capture.
func playCapture(t *testing.T, e *env, addr, id string, mod func(*srt.Config), pre, rest []media.Frame, speed float64, connected func(srt.Conn)) capture {
	t.Helper()
	for _, f := range pre {
		e.h.Write(f)
	}
	subs := len(e.h.Stats())
	c, err := dial(addr, id, mod)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	// The connection subscribes in its own goroutine, after the handshake:
	// feeding before would lose the cached GOP to the next keyframe.
	require.Eventually(t, func() bool { return len(e.h.Stats()) > subs }, waitFor, time.Millisecond, "subscribed")
	if connected != nil {
		connected(c)
	}
	ch := receive(c, nil, nil)
	<-feed(context.Background(), e.h, rest, speed)
	e.h.Close()
	got := waitCapture(t, ch)
	require.Error(t, got.err, "the server closed the connection")
	return got
}

// raceLatency gives the receiver latency d under the race detector, and
// leaves gosrt's 120 ms otherwise. gosrt's sender tick sends what is due
// every 10 ms, so even a paced burst can overflow a receiver's UDP buffer
// on loopback; the losses are retransmitted, but a receiver slowed by the
// race detector needs more than 120 ms for those repairs.
func raceLatency(d time.Duration) func(*srt.Config) {
	if !raceEnabled {
		return nil
	}
	return func(c *srt.Config) { c.Latency = d }
}

// The stream reads back clean: every frame from the cached keyframe on,
// conformant, with unbroken continuity counters; the hub closing ends the
// connection.
func TestSRTPlay(t *testing.T) {
	e := start(t, Options{})
	frames := looped(t, "h264_aac.ts", 2)
	gop, rest := splitAtKey(frames, 2)
	data := play(t, e, e.addr, "live/s", gop, rest, 4, func(srt.Conn) {
		waitStats(t, e.srv, func(s Stats) bool { return s.Conns == 1 }, "connection counted")
	})
	got := readBack(t, data)
	require.NotEmpty(t, got)
	assert.Nil(t, conform.CheckFrames(got, conform.FrameOptions{}))
	assert.Zero(t, ccErrors(t, data))
	assert.Equal(t, countKind(frames, media.KindVideo), countKind(got, media.KindVideo))
	assert.Equal(t, countKind(frames, media.KindAudio), countKind(got, media.KindAudio))
	waitStats(t, e.srv, func(s Stats) bool { return s.EndedHubClosed == 1 }, "ended by the hub")
	st := e.srv.Stats()
	assert.Zero(t, st.Conns)
	assert.Equal(t, uint64(len(frames)), st.Frames)
	assert.Zero(t, st.Rejected)
}

// Both stream ID forms play, with or without the optional keys; unknown keys
// are ignored and the app follows Options.App.
func TestSRTStreamID(t *testing.T) {
	for _, tc := range []struct {
		app string
		ids []string
	}{
		{"", []string{
			"live/s", "#!::r=live/s", "#!::r=live/s,m=request", "#!::m=request,t=stream,r=live/s",
			"#!::u=bob,r=live/s,h=example.com,s=abc",
		}},
		{"/tv/", []string{"tv/s", "#!::r=tv/s,m=request"}},
	} {
		t.Run("app "+tc.app, func(t *testing.T) {
			e := start(t, Options{App: tc.app})
			frames := looped(t, "h264_aac.ts", 8)
			gop, rest := splitAtKey(frames, 2)
			for _, f := range gop {
				e.h.Write(f)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := feed(ctx, e.h, rest, 4)
			for _, id := range tc.ids {
				c, err := dial(e.addr, id, nil)
				require.NoError(t, err, id)
				got := waitCapture(t, receive(c, nil, atLeast(20*mpegts.PacketSize)))
				require.NoError(t, got.err, id)
				assert.Equal(t, byte(0x47), got.data[0], id)
				_ = c.Close()
			}
			cancel()
			<-done
		})
	}
}

func TestParseStreamID(t *testing.T) {
	for id, want := range map[string]struct {
		res string
		ok  bool
	}{
		"live/s":                     {"live/s", true},
		"#!::r=live/s":               {"live/s", true},
		"#!::r=live/s,m=request":     {"live/s", true},
		"#!::t=stream,r=a/b,x=y":     {"a/b", true},
		"#!::r=live/s,m=publish":     {"", false},
		"#!::m=bidirectional,r=a/b":  {"", false},
		"#!::r=live/s,t=file":        {"", false},
		"#!::r=live/s,t=auth":        {"", false},
		"":                           {"", false},
		"#!::m=request":              {"", true},
		"#!::":                       {"", true},
		"#!::r=live/s,junk,u=a=b":    {"live/s", true},
		"#!::r=live/s,M=publish":     {"live/s", true}, // keys are case-sensitive: M is unknown
		"#!:r=live/s":                {"#!:r=live/s", true},
		"#!::r=live/s,m=request,m=x": {"", false},
	} {
		res, ok := parseStreamID(id)
		assert.Equal(t, want.ok, ok, "%q", id)
		if want.ok {
			assert.Equal(t, want.res, res, "%q", id)
		}
	}
}

// Every refusal looks the same to the caller: "connection rejected" with
// one reason, whatever the cause.
func TestSRTRejects(t *testing.T) {
	e := start(t, Options{})
	gop, _ := splitAtKey(looped(t, "h264_aac.ts", 1), 2)
	for _, f := range gop {
		e.h.Write(f)
	}
	require.True(t, e.h.Live())
	encrypted := func(c *srt.Config) { c.Passphrase = "0123456789abcdef" }
	cases := []struct {
		name, id string
		mod      func(*srt.Config)
		mode     bool // counted as a mode refusal, else an unknown stream
	}{
		{"publish", "#!::r=live/s,m=publish", nil, true},
		{"file", "#!::r=live/s,t=file", nil, true},
		{"empty", "", nil, true},
		{"encrypted", "live/s", encrypted, true},
		{"unknown", "live/nope", nil, false},
		{"unknown access control", "#!::r=live/nope,m=request", nil, false},
		{"not live", "live/q", nil, false},
		{"nil hub", "live/nil", nil, false},
		{"other app", "tv/s", nil, false},
		{"no resource", "#!::m=request", nil, false},
		{"nested name", "live/s/x", nil, false},
		{"no name", "live/", nil, false},
	}
	var first string
	for _, tc := range cases {
		_, err := dial(e.addr, tc.id, tc.mod)
		require.Error(t, err, tc.name)
		assert.Contains(t, err.Error(), "connection rejected", tc.name)
		if first == "" {
			first = err.Error()
		}
		assert.Equal(t, first, err.Error(), "%s: the same reason for every refusal", tc.name)
	}
	st := e.srv.Stats()
	// A caller that retransmits its request after a refusal may be counted
	// twice.
	assert.GreaterOrEqual(t, st.RejectedMode, uint64(4))
	assert.GreaterOrEqual(t, st.RejectedUnknown, uint64(len(cases)-4))
	assert.Equal(t, st.Rejected, st.RejectedMode+st.RejectedUnknown+st.RejectedCaps+st.RejectedMSS)
	assert.Zero(t, st.RejectedCaps)
	assert.Zero(t, st.RejectedMSS)
	assert.Zero(t, st.Conns)
}

func TestSRTCaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Options
		n    int
	}{
		{"per stream", Options{MaxPerStream: 2}, 2},
		{"per listener", Options{MaxPerListener: 1}, 1},
		{"per IP", Options{MaxPerIP: 3}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := start(t, tc.opt)
			gop, _ := splitAtKey(looped(t, "h264_aac.ts", 1), 2)
			for _, f := range gop {
				e.h.Write(f)
			}
			conns := make([]srt.Conn, tc.n)
			for i := range conns {
				conns[i] = mustDial(t, e.addr)
			}
			_, err := dial(e.addr, "live/s", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "connection rejected")
			st := e.srv.Stats()
			assert.Positive(t, st.RejectedCaps)
			assert.Equal(t, st.Rejected, st.RejectedCaps)
			assert.Equal(t, uint64(tc.n), st.Conns)

			// A receiver that leaves frees its slot.
			_ = conns[0].Close()
			waitStats(t, e.srv, func(s Stats) bool { return s.EndedGone == 1 && s.Conns == uint64(tc.n-1) }, "slot freed")
			mustDial(t, e.addr)
		})
	}
}

func TestPeerKey(t *testing.T) {
	udp := func(s string) net.Addr { return net.UDPAddrFromAddrPort(netip.MustParseAddrPort(s)) }
	assert.Equal(t, "192.0.2.1", peerKey(udp("192.0.2.1:9000")))
	assert.Equal(t, "192.0.2.1", peerKey(udp("[::ffff:192.0.2.1]:9000")))
	assert.Equal(t, peerKey(udp("[2001:db8:1:2::1]:1")), peerKey(udp("[2001:db8:1:2:ffff::9]:2")), "one /64")
	assert.NotEqual(t, peerKey(udp("[2001:db8:1:2::1]:1")), peerKey(udp("[2001:db8:1:3::1]:1")))
	assert.Equal(t, "2001:db8:1:2::/64", peerKey(udp("[2001:db8:1:2::1]:1")))
	assert.Equal(t, "host", peerKey(fakeAddr("host:1")))
	assert.Equal(t, "nonsense", peerKey(fakeAddr("nonsense")))
}

type fakeAddr string

func (a fakeAddr) Network() string { return "udp" }
func (a fakeAddr) String() string  { return string(a) }

// A receiver that joins mid-GOP starts at the cached keyframe, after the
// tables.
func TestSRTJoinsAtKeyframe(t *testing.T) {
	e := start(t, Options{})
	frames := looped(t, "h264_aac.ts", 2)
	head, rest := splitAtKey(frames, 3)
	_, second := splitAtKey(head, 2) // the second GOP, cached when we join
	half := len(second) / 2
	pre := head[:len(head)-len(second)+half]
	data := play(t, e, e.addr, "live/s", pre, append(head[len(pre):len(head):len(head)], rest...), 4, nil)
	pkts := mpegts.NewReader(bytes.NewReader(data))
	p, err := pkts.Next()
	require.NoError(t, err)
	assert.Equal(t, uint16(0), p.PID, "PAT first")
	got := readBack(t, data)
	var video []media.Frame
	for _, f := range got {
		if f.Kind() == media.KindVideo {
			video = append(video, f)
		}
	}
	require.NotEmpty(t, video)
	assert.True(t, video[0].Key)
	assert.Equal(t, countKind(second, media.KindVideo)+countKind(rest, media.KindVideo), len(video),
		"every video frame from the cached keyframe on")
	assert.Zero(t, ccErrors(t, data))
}

// A receiver that vanishes (its socket gone, no SRT shutdown) while the hub
// is quiet is ended within the peer idle timeout, and its cap slot freed.
func TestSRTViewerGoneDuringOutage(t *testing.T) {
	// Above the 1 s at which an idle gosrt receiver repeats its ACK, its
	// only traffic until data comes.
	const idle = 2 * time.Second
	e := start(t, Options{PeerIdleTimeout: idle, MaxPerIP: 1})
	frames := looped(t, "h264_aac.ts", 2)
	gop, rest := splitAtKey(frames, 2)
	for _, f := range gop {
		e.h.Write(f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := feed(ctx, e.h, rest, 1)
	p := newProxy(t, e.addr)
	c := mustDial(t, p.Addr())
	got := waitCapture(t, receive(c, nil, atLeast(50*mpegts.PacketSize)))
	require.NoError(t, got.err)
	cancel() // the outage: the hub goes quiet
	<-done
	p.Close() // the receiver's socket is gone: its shutdown reaches no one
	_ = c.Close()
	t0 := time.Now()
	waitStats(t, e.srv, func(s Stats) bool { return s.EndedGone == 1 }, "ended as gone")
	assert.Less(t, time.Since(t0), idle+time.Second, "within the idle timeout")
	st := e.srv.Stats()
	assert.Zero(t, st.Conns)
	assert.Zero(t, st.EndedStalled)
	mustDial(t, e.addr) // the per-IP slot is free again
}

// A receiver behind a link slower than the stream is ended as stalled after
// StallTimeout; another receiver goes on untouched and the hub never waits.
func TestSRTStalledReceiverEnded(t *testing.T) {
	e := start(t, Options{StallTimeout: time.Second})
	frames := looped(t, "h264_aac.ts", 4)
	gop, rest := splitAtKey(frames, 2)
	for _, f := range gop {
		e.h.Write(f)
	}
	good := mustDial(t, e.addr)
	var goodN atomic.Int64
	goodCh := receive(good, &goodN, nil)
	p := newProxy(t, e.addr)
	p.Throttle(15_000, 4*time.Second) // about a seventh of the stream at speed 2
	bad := mustDial(t, p.Addr())
	receive(bad, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := feed(ctx, e.h, rest, 2)
	waitStats(t, e.srv, func(s Stats) bool { return s.EndedStalled == 1 }, "the slow receiver ended as stalled")
	st := e.srv.Stats()
	assert.Equal(t, uint64(1), st.Conns, "the other receiver remains")
	n0 := goodN.Load()
	require.Eventually(t, func() bool { return goodN.Load() > n0+100*mpegts.PacketSize }, waitFor, 10*time.Millisecond,
		"the other receiver goes on")
	cancel()
	assert.Less(t, <-done, 100*time.Millisecond, "hub writes never block")
	e.h.Close()
	g := waitCapture(t, goodCh)
	assert.Zero(t, ccErrors(t, g.data), "the other receiver lost nothing")
	assert.Nil(t, conform.CheckFrames(readBack(t, g.data), conform.FrameOptions{}))
	assert.Zero(t, e.srv.Stats().EndedGone)
}

// fakeConn copies gosrt's Write: bytes are appended to a buffer that is cut
// into payload-sized chunks (payload bytes, 0 meaning payloadSize), and a
// chunk that finds the send queue full is consumed and discarded (0,
// io.EOF), leaving any rest of the call's bytes in the buffer. Every
// every-th chunk finds the queue full.
type fakeConn struct {
	every, payload     int
	buf, got           bytes.Buffer
	chunks, fails      int
	calls, empty, most int
}

func (f *fakeConn) Write(b []byte) (int, error) {
	f.calls++
	if len(b) == 0 {
		f.empty++
	}
	f.most = max(f.most, len(b))
	f.buf.Write(b)
	for {
		size := f.payload
		if size == 0 {
			size = payloadSize
		}
		chunk := f.buf.Next(size)
		if len(chunk) == 0 {
			return 0, io.EOF // gosrt: reading an empty buffer
		}
		f.chunks++
		if f.every > 0 && f.chunks%f.every == 0 {
			f.fails++
			return 0, io.EOF
		}
		f.got.Write(chunk)
		if f.buf.Len() == 0 {
			return len(b), nil
		}
	}
}

// tsBytes returns n TS packets, each one distinct.
func tsBytes(n int) []byte {
	out := make([]byte, 0, n*mpegts.PacketSize)
	for i := range n {
		p := make([]byte, mpegts.PacketSize)
		p[0] = 0x47
		p[1], p[2] = 0x01, 0x00
		p[3] = 0x10 | byte(i&0x0F)
		p[4], p[5], p[6] = byte(i>>16), byte(i>>8), byte(i)
		out = append(out, p...)
	}
	return out
}

// The write wrapper retries the very chunk gosrt discarded, so the bytes
// that reach the queue are exactly the bytes written; it never hands gosrt
// more than one payload or an empty slice.
func TestSRTWriterRetryExact(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		for _, every := range []int{2, 3, 7} {
			ctx, cancel := context.WithCancelCause(context.Background())
			f := &fakeConn{every: every}
			var full atomic.Uint64
			w := newWriter(ctx, cancel, f, payloadSize, time.Second, &full)
			data := tsBytes(1500)
			sizes := []int{1, 7, 8, 13, 40, 2, 100, 3}
			for off, i := 0, 0; off < len(data); i++ {
				n := min(sizes[i%len(sizes)]*mpegts.PacketSize, len(data)-off)
				k, err := w.Write(data[off : off+n])
				require.NoError(t, err)
				require.Equal(t, n, k)
				off += n
			}
			require.True(t, bytes.Equal(data, f.got.Bytes()), "every %d: no duplicate or missing packet", every)
			assert.LessOrEqual(t, f.most, payloadSize)
			assert.Zero(t, f.empty)
			assert.Positive(t, f.fails)
			assert.Equal(t, uint64(f.fails), full.Load(), "every full queue counted")
			require.NoError(t, w.SetWriteDeadline(time.Now()))
			require.NoError(t, w.Flush())
			k, err := w.Write(nil)
			require.NoError(t, err)
			assert.Zero(t, k)
			assert.Zero(t, f.empty, "an empty write never reaches gosrt")
			cancel(nil)
		}
	})
	t.Run("stalled", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		f := &fakeConn{every: 1}
		var full atomic.Uint64
		w := newWriter(ctx, cancel, f, payloadSize, 50*time.Millisecond, &full)
		t0 := time.Now()
		_, err := w.Write(tsBytes(7))
		require.ErrorIs(t, err, errStalled)
		assert.GreaterOrEqual(t, time.Since(t0), 50*time.Millisecond)
		require.ErrorIs(t, context.Cause(ctx), errStalled)
		assert.Equal(t, uint64(1), full.Load(), "one full queue, retried")
		assert.Greater(t, f.calls, 2)
	})
	t.Run("connection done", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		f := &fakeConn{every: 1}
		var full atomic.Uint64
		w := newWriter(ctx, cancel, f, payloadSize, time.Minute, &full)
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel(errGone)
		}()
		_, err := w.Write(tsBytes(7))
		require.ErrorIs(t, err, errGone)
		_, err = w.Write(tsBytes(7))
		require.ErrorIs(t, err, errGone, "io.EOF once the connection is done is its end")
	})
	t.Run("other error", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		boom := errors.New("boom")
		var full atomic.Uint64
		w := newWriter(ctx, cancel, writerFunc(func([]byte) (int, error) { return 0, boom }), payloadSize, time.Minute, &full)
		_, err := w.Write(tsBytes(1))
		require.ErrorIs(t, err, boom)
	})
	t.Run("paced", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		f := &fakeConn{}
		var full atomic.Uint64
		w := newWriter(ctx, cancel, f, payloadSize, time.Minute, &full)
		data := bytes.Repeat(tsBytes(7), 4*pacePackets+1) // 4 windows full, one more packet
		t0 := time.Now()
		_, err := w.Write(data)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(t0), 4*paceWindow-time.Millisecond, "at most 256 packets per 5 ms")
		assert.True(t, bytes.Equal(data, f.got.Bytes()))
	})
	t.Run("no write after the connection ended", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		f := &fakeConn{}
		var full atomic.Uint64
		w := newWriter(ctx, cancel, f, payloadSize, time.Minute, &full)
		cancel(errShutdown)
		_, err := w.Write(tsBytes(7))
		require.ErrorIs(t, err, errShutdown)
		assert.Zero(t, f.calls)
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func TestStallMonitor(t *testing.T) {
	t0 := time.Unix(0, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	t.Run("drops above 5 percent", func(t *testing.T) {
		m := stallMonitor{window: time.Second}
		assert.False(t, m.add(at(0), sample{}))
		assert.False(t, m.add(at(500), sample{sent: 500, drop: 50, acks: 10}), "the window is not covered yet")
		assert.True(t, m.add(at(1000), sample{sent: 1000, drop: 51, acks: 20}))
	})
	t.Run("drops at 5 percent", func(t *testing.T) {
		m := stallMonitor{window: time.Second}
		m.add(at(0), sample{})
		assert.False(t, m.add(at(1000), sample{sent: 1000, drop: 50, acks: 20}))
	})
	t.Run("no ack while sending", func(t *testing.T) {
		m := stallMonitor{window: time.Second}
		m.add(at(0), sample{sent: 10, acks: 5})
		m.add(at(500), sample{sent: 20, acks: 5})
		assert.True(t, m.add(at(1000), sample{sent: 30, acks: 5}))
	})
	t.Run("nothing sent", func(t *testing.T) {
		m := stallMonitor{window: time.Second}
		m.add(at(0), sample{sent: 10, drop: 0, acks: 5})
		assert.False(t, m.add(at(1000), sample{sent: 10, drop: 9, acks: 5}), "an outage is not a stall")
	})
	t.Run("sliding window", func(t *testing.T) {
		m := stallMonitor{window: time.Second}
		m.add(at(0), sample{})
		m.add(at(500), sample{sent: 100, drop: 100, acks: 1})
		m.add(at(1000), sample{sent: 200, drop: 100, acks: 2}) // 50 % over the window: stalled
		m = stallMonitor{window: time.Second}
		m.add(at(0), sample{sent: 0, drop: 0, acks: 0})
		m.add(at(500), sample{sent: 100, drop: 100, acks: 1})
		assert.False(t, m.add(at(1000), sample{sent: 2100, drop: 100, acks: 2}), "5 % or less")
		assert.False(t, m.add(at(1500), sample{sent: 4100, drop: 100, acks: 3}), "the old drops left the window")
		assert.Len(t, m.samples, 3, "older samples are forgotten")
	})
}

// SRT repairs a lossy link: through 2 % loss each way the TS reads back
// clean, and the receiver saw retransmissions.
func TestSRTLossyLink(t *testing.T) {
	e := start(t, Options{})
	frames := looped(t, "h264_aac.ts", 2)
	gop, rest := splitAtKey(frames, 2)
	p := newProxy(t, e.addr)
	var rc srt.Conn
	data := playWith(t, e, p.Addr(), "#!::r=live/s", raceLatency(500*time.Millisecond), gop, rest, 3, func(c srt.Conn) {
		rc = c
		p.SetLoss(0.02)
		time.AfterFunc(5*time.Second, func() { p.SetLoss(0) }) // the shutdown gets through
	})
	assert.Zero(t, ccErrors(t, data))
	got := readBack(t, data)
	assert.Nil(t, conform.CheckFrames(got, conform.FrameOptions{}))
	assert.Equal(t, countKind(frames, media.KindVideo), countKind(got, media.KindVideo))
	var st srt.Statistics
	rc.Stats(&st)
	assert.Positive(t, st.Accumulated.PktRecvRetrans, "losses repaired by retransmission")
}

// A join on a GOP whose keyframe is over 2 MB arrives whole: the replay is
// paced, so the receiver's queue never overflows, and the viewer is not
// ended.
func TestSRTBurstJoin(t *testing.T) {
	e := start(t, Options{})
	pre, rest := burstFrames(t)
	data := playWith(t, e, e.addr, "live/s", raceLatency(time.Second), pre, rest, 2, nil)
	assert.Zero(t, ccErrors(t, data), "no continuity break at the receiver")
	got := readBack(t, data)
	require.Len(t, got, 100)
	size := 0
	for _, d := range got[0].Data {
		size += len(d)
	}
	assert.GreaterOrEqual(t, size, burstKeyframe)
	st := e.srv.Stats()
	assert.Zero(t, st.EndedStalled)
	assert.Equal(t, uint64(1), st.EndedHubClosed)
}

// chunkSize keeps an SRT packet's 7 TS packets while the connection's
// payload (gosrt's MSS less 44) holds them, else the whole TS packets that
// fit; fewer than 4 refuse the connection (0).
func TestChunkSize(t *testing.T) {
	for _, tc := range []struct {
		mss  uint64
		want int
	}{
		{1500, payloadSize},
		{1360, payloadSize},
		{1359, 6 * mpegts.PacketSize},
		{1200, 6 * mpegts.PacketSize},
		{796, 4 * mpegts.PacketSize},
		{795, 0},
		{76, 0},
		{0, 0},
	} {
		assert.Equal(t, tc.want, chunkSize(tc.mss), "MSS %d", tc.mss)
	}
}

// With a payload of 1156 bytes (a caller's MSS of 1200), gosrt splits a
// 1316-byte Write over two packets, and a full queue on the second makes
// the retry repeat the first; chunks of the whole TS packets that fit one
// payload keep every Write one packet, queued whole or not at all.
func TestSRTWriterLowMSS(t *testing.T) {
	size := chunkSize(1200)
	for _, every := range []int{2, 3, 7} {
		ctx, cancel := context.WithCancelCause(context.Background())
		f := &fakeConn{every: every, payload: 1200 - 44}
		var full atomic.Uint64
		w := newWriter(ctx, cancel, f, size, time.Second, &full)
		data := tsBytes(1500)
		sizes := []int{1, 7, 8, 13, 40, 2, 100, 3}
		for off, i := 0, 0; off < len(data); i++ {
			n := min(sizes[i%len(sizes)]*mpegts.PacketSize, len(data)-off)
			k, err := w.Write(data[off : off+n])
			require.NoError(t, err)
			require.Equal(t, n, k)
			off += n
		}
		require.True(t, bytes.Equal(data, f.got.Bytes()), "every %d: no duplicate or missing packet", every)
		assert.Equal(t, size, f.most)
		assert.Equal(t, f.calls, f.chunks, "every %d: one gosrt packet per Write", every)
		assert.Positive(t, f.fails)
		cancel(nil)
	}
}

// recorder stands between a connection's writer and gosrt: it records the
// bytes gosrt took and the largest Write, and answers every every-th Write
// as gosrt answers a full send queue (0, io.EOF, the bytes dropped) without
// passing it on, so that retries happen on demand.
type recorder struct {
	w     io.Writer
	every int

	mu                 sync.Mutex
	calls, most, fails int
	got                bytes.Buffer
}

func (r *recorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.most = max(r.most, len(b))
	if r.calls%r.every == 0 {
		r.fails++
		return 0, io.EOF
	}
	n, err := r.w.Write(b)
	if err == nil {
		r.got.Write(b[:n])
	}
	return n, err
}

// A burst join with full-queue retries reaches a receiver byte-exact, at the
// default MSS and at an MSS of 1200, where the connection's chunks shrink
// to 6 TS packets so that each Write stays one SRT packet.
func TestSRTLowMSS(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mss   uint32
		chunk int
	}{
		{"default MSS", 0, payloadSize},
		{"MSS 1200", 1200, 6 * mpegts.PacketSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := make(chan *recorder, 1)
			e := startWrapped(t, Options{}, func(w io.Writer) io.Writer {
				r := &recorder{w: w, every: 5}
				recs <- r
				return r
			})
			mod := func(c *srt.Config) {
				if tc.mss > 0 {
					c.MSS, c.PayloadSize = tc.mss, tc.mss-44
				}
				if raceEnabled {
					c.Latency = time.Second
				}
			}
			pre, rest := burstFrames(t)
			got := playCapture(t, e, e.addr, "live/s", mod, pre, rest, 2, nil)
			r := <-recs
			r.mu.Lock()
			defer r.mu.Unlock()
			assert.Positive(t, r.fails)
			st := e.srv.Stats()
			assert.GreaterOrEqual(t, st.SendQueueFull, uint64(r.fails), "every full queue retried")
			assert.Equal(t, tc.chunk, r.most, "the connection's chunk size")
			assert.Equal(t, tc.chunk, got.most, "one chunk per SRT packet")
			require.True(t, bytes.Equal(r.got.Bytes(), got.data), "byte-exact: no duplicated or missing bytes")
			assert.Zero(t, ccErrors(t, got.data), "no continuity break at the receiver")
			require.Len(t, readBack(t, got.data), 100)
			assert.Zero(t, st.EndedStalled)
			assert.Equal(t, uint64(1), st.EndedHubClosed)
		})
	}
}

// A caller whose MSS leaves room for fewer than 4 TS packets per SRT packet
// completes gosrt's handshake, which decides the MSS, and is then closed at
// once and counted as refused; a caller at 4 packets is served.
func TestSRTRefusesSmallMSS(t *testing.T) {
	e := start(t, Options{})
	gop, rest := splitAtKey(looped(t, "h264_aac.ts", 1), 2)
	for _, f := range gop {
		e.h.Write(f)
	}
	mss := func(n uint32) func(*srt.Config) {
		return func(c *srt.Config) { c.MSS, c.PayloadSize = n, n-44 }
	}
	c, err := dial(e.addr, "live/s", mss(795))
	require.NoError(t, err, "gosrt completes the handshake")
	t.Cleanup(func() { _ = c.Close() })
	got := waitCapture(t, receive(c, nil, nil))
	require.Error(t, got.err, "closed by the server")
	assert.Empty(t, got.data)
	waitStats(t, e.srv, func(s Stats) bool { return s.RejectedMSS == 1 && s.Conns == 0 }, "refused")
	st := e.srv.Stats()
	assert.Equal(t, uint64(1), st.Rejected)
	assert.Zero(t, st.EndedGone+st.EndedShutdown+st.EndedStalled+st.EndedError, "never served")

	more := playCapture(t, e, e.addr, "live/s", mss(796), nil, rest, 4, nil)
	assert.Equal(t, 4*mpegts.PacketSize, more.most, "4 TS packets per SRT packet")
	assert.Zero(t, ccErrors(t, more.data))
	assert.Equal(t, uint64(1), e.srv.Stats().RejectedMSS)
}

// burstKeyframe is the size of the keyframe burstFrames starts with.
const burstKeyframe = 2_300_000

// burstFrames returns a synthetic H.264 GOP whose keyframe is over 2 MB, to
// be cached before a join (pre), and 75 small frames after it (rest).
func burstFrames(t *testing.T) (pre, rest []media.Frame) {
	t.Helper()
	h264 := configOf(t, "h264_aac.ts", media.CodecH264)
	frame := func(i, size int, key bool) media.Frame {
		data := make([]byte, size)
		data[0] = 0x41
		if key {
			data[0] = 0x65
		}
		for j := 1; j < size; j++ {
			data[j] = byte(j*7 + i)
		}
		dts := int64(i) * 3600
		return media.Frame{Track: 0, Config: h264, PTS: dts, DTS: dts, Key: key, Data: [][]byte{data}, Epoch: 1}
	}
	pre = make([]media.Frame, 0, 25)
	rest = make([]media.Frame, 0, 75)
	for i := range 25 {
		size := 2000
		if i == 0 {
			size = burstKeyframe
		}
		pre = append(pre, frame(i, size, i == 0))
	}
	for i := 25; i < 100; i++ {
		rest = append(rest, frame(i, 2000, i%25 == 0))
	}
	return pre, rest
}

// A codec change on a configured track ends the connection.
func TestSRTCodecChangeEnds(t *testing.T) {
	e := start(t, Options{})
	frames := looped(t, "h264_aac.ts", 1)
	gop, rest := splitAtKey(frames, 2)
	for _, f := range gop {
		e.h.Write(f)
	}
	c := mustDial(t, e.addr)
	ch := receive(c, nil, nil)
	<-feed(context.Background(), e.h, rest[:len(rest)/2], 4)
	last := rest[len(rest)/2-1].DTS + 3600
	h265 := configOf(t, "h265_aac51.ts", media.CodecH265)
	e.h.Write(media.Frame{Track: 0, Config: h265, PTS: last, DTS: last, Key: true, Data: [][]byte{{0x26, 1}}, Epoch: 1})
	waitStats(t, e.srv, func(s Stats) bool { return s.EndedCodecChange == 1 }, "ended by the codec change")
	got := waitCapture(t, ch)
	require.Error(t, got.err, "the server closed the connection")
	assert.NotEmpty(t, readBack(t, got.data))
}

// ffprobe reads both codec pairs over SRT, with the access-control stream
// ID. The ID goes in libsrt's streamid option rather than the URL: ffmpeg
// 6.1 passes a URL's streamid query value on undecoded (a percent-encoded
// #!::r=live/s,m=request arrives as %23%21%3A%3A...), where ffmpeg 8
// decodes it, and the option is passed on verbatim by both.
func TestSRTFFprobe(t *testing.T) {
	ffprobe := testpeer.Require(t, "ffprobe", "srt")
	for _, tc := range []struct {
		fixture string
		want    map[string]string
	}{
		{"h264_aac.ts", map[string]string{"video": "h264 320x240", "audio": "aac 48000"}},
		{"h265_aac51.ts", map[string]string{"video": "hevc 320x240", "audio": "aac 44100"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			e := start(t, Options{})
			frames := looped(t, tc.fixture, 4)
			gop, rest := splitAtKey(frames, 2)
			for _, f := range gop {
				e.h.Write(f)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := feed(ctx, e.h, rest, 2)
			u := fmt.Sprintf("srt://%s", e.addr)
			pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
			defer pcancel()
			cmd := exec.CommandContext(pctx, ffprobe, "-v", "error", "-streamid", "#!::r=live/s,m=request", "-show_entries",
				"stream=codec_name,codec_type,width,height,sample_rate", "-of", "json", u)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			require.NoError(t, err, "ffprobe stdout %q stderr %q; server %+v", out, stderr.String(), e.srv.Stats())
			var probe struct {
				Streams []struct {
					CodecName  string `json:"codec_name"`
					CodecType  string `json:"codec_type"`
					Width      int    `json:"width"`
					Height     int    `json:"height"`
					SampleRate string `json:"sample_rate"`
				} `json:"streams"`
			}
			require.NoError(t, json.Unmarshal(out, &probe))
			got := map[string]string{}
			for _, s := range probe.Streams {
				switch s.CodecType {
				case "video":
					got["video"] = fmt.Sprintf("%s %dx%d", s.CodecName, s.Width, s.Height)
				case "audio":
					got["audio"] = s.CodecName + " " + s.SampleRate
				}
			}
			assert.Equal(t, tc.want, got)
			cancel()
			<-done
		})
	}
}

// The server's own errors and lifecycle.
func TestSRTServerLifecycle(t *testing.T) {
	srv := New(func(string) (*hub.Hub, bool) { return nil, false }, Options{})
	assert.Nil(t, srv.Addr())
	require.Error(t, srv.Serve(), "Serve before Listen")
	require.Error(t, srv.Listen("127.0.0.1:99999"))
	require.NoError(t, srv.Listen("127.0.0.1:0"))
	require.Error(t, srv.Listen("127.0.0.1:0"), "one listener per server")
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	require.NoError(t, srv.Close())
	require.ErrorIs(t, <-served, ErrServerClosed)
	require.NoError(t, srv.Close(), "idempotent")
	require.ErrorIs(t, srv.Serve(), ErrServerClosed)
	require.ErrorIs(t, srv.Listen("127.0.0.1:0"), ErrServerClosed)

	// Close ends the connections being served, as shutdown.
	e := start(t, Options{})
	gop, _ := splitAtKey(looped(t, "h264_aac.ts", 1), 2)
	for _, f := range gop {
		e.h.Write(f)
	}
	var wg sync.WaitGroup
	for range 3 {
		c := mustDial(t, e.addr)
		ch := receive(c, nil, nil)
		wg.Go(func() { <-ch })
	}
	waitStats(t, e.srv, func(s Stats) bool { return s.Conns == 3 }, "three connections")
	require.NoError(t, e.srv.Close())
	st := e.srv.Stats()
	assert.Equal(t, uint64(3), st.EndedShutdown)
	assert.Zero(t, st.Conns)
	wg.Wait()
	_, err := dial(e.addr, "live/s", func(c *srt.Config) { c.ConnectionTimeout = 300 * time.Millisecond })
	require.Error(t, err)
}

func TestBucketRate(t *testing.T) {
	const ceiling = float64(pacePackets*payloadSize) / 0.005 // bytes per second
	assert.InDelta(t, 2e6/8, bucketRate(0), 1e-6, "2 Mbit/s floor")
	assert.InDelta(t, 2e6/8, bucketRate(500e3), 1e-6, "2.5 x 0.5 Mbit/s is below the floor")
	assert.InDelta(t, 2.5*6e6/8, bucketRate(6e6), 1e-6)
	assert.InDelta(t, ceiling, bucketRate(1e9), 1e-6, "256 packets per 5 ms ceiling")
	assert.InDelta(t, 0.05*2.5*6e6/8, bucketDepth(bucketRate(6e6)), 1e-6, "50 ms at the rate")
	assert.InDelta(t, float64(32*payloadSize), bucketDepth(bucketRate(0)), 1e-6, "at least 32 packets")
}

// A 1.5 MB GOP replay on a 6 Mbit/s stream leaves at 15 Mbit/s: the bucket
// lets its depth go at once and the rest at the rate.
func TestBucketCatchUp(t *testing.T) {
	t0 := time.Unix(0, 0)
	b := newBucket(6e6, t0)
	now := t0
	for sent := 0; sent < 1_500_000; sent += payloadSize {
		now = now.Add(b.take(now, payloadSize))
	}
	rate := 2.5 * 6e6 / 8
	want := time.Duration((1_500_000 - 0.05*rate) / rate * float64(time.Second))
	assert.InDelta(t, want.Seconds(), now.Sub(t0).Seconds(), 0.01, "about 0.75 s")

	// Idle refills the bucket to its depth, no more.
	now = now.Add(time.Hour)
	assert.Zero(t, b.take(now, int(0.05*rate)))
	assert.Positive(t, b.take(now, 2*minSleepBytes(rate)))

	// A lower rate shrinks the depth.
	b.setRate(0, now.Add(time.Hour))
	assert.InDelta(t, float64(32*payloadSize), b.tokens, 1e-6)
}

// minSleepBytes is the debt the bucket allows before it asks for a sleep.
func minSleepBytes(rate float64) int { return int(rate*minSleep.Seconds()) + 1 }

func TestRateMeter(t *testing.T) {
	var m rateMeter
	assert.InDelta(t, 125000*8*188/184.0, m.prebuffered(125000, media.ClockRate), 1e-6, "TS overhead on the payload")
	assert.Zero(t, m.prebuffered(1000, 0), "no span: unknown")

	// 5000 TS bytes every 40 ms: 1 Mbit/s once 2 s of stream time are seen.
	for i := range 50 {
		_, ok := m.frame(int64(i)*3600, 5000)
		require.False(t, ok, "frame %d: less than 2 s", i)
	}
	r, ok := m.frame(50*3600, 5000)
	require.True(t, ok)
	assert.InDelta(t, 1e6, r, 1)
	for i := 51; i < 200; i++ {
		r, ok = m.frame(int64(i)*3600, 10000)
	}
	require.True(t, ok)
	assert.InDelta(t, 2e6, r, 1, "the window moves")
	assert.LessOrEqual(t, len(m.samples), 52)

	// An interleaved frame slightly behind does not move the clock back.
	_, ok = m.frame(198*3600, 100)
	assert.True(t, ok)
	// A new timeline starts a new window.
	_, ok = m.frame(0, 5000)
	assert.False(t, ok)
	_, ok = m.frame(200*3600+20*media.ClockRate, 5000)
	assert.False(t, ok, "a jump forward is a new timeline too")

	// 6 Mbit/s, then a 3 s gap: the window starts over.
	m = rateMeter{}
	for i := range 76 {
		r, ok = m.frame(int64(i)*3600, 30_000)
	}
	require.True(t, ok)
	assert.InDelta(t, 6e6, r, 1)
	_, ok = m.frame(75*3600+3*media.ClockRate, 30_000)
	assert.False(t, ok, "a gap over 1 s starts a new window")
	_, ok = m.frame(75*3600+4*media.ClockRate, 30_000)
	assert.False(t, ok, "not covered yet")
	m.reset()
	assert.Empty(t, m.samples)
}

// The writer paces by the bitrate it is told: before it knows one, only
// the ceiling applies.
func TestWriterMeter(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f := &fakeConn{}
	var full atomic.Uint64
	w := newWriter(ctx, cancel, f, payloadSize, time.Minute, &full)
	assert.Nil(t, w.bucket)
	w.Prebuffered(10_000, 0)
	assert.NotNil(t, w.bucket, "an unknown bitrate paces at the floor")
	w.Prebuffered(250_000, media.ClockRate) // about 2 Mbit/s: 5 Mbit/s pacing
	assert.InDelta(t, bucketRate(250_000*8*188/184.0), w.bucket.rate, 1e-6)
	for i := range 60 {
		w.Frame(int64(i)*3600, 25_000) // 5 Mbit/s: 12.5 Mbit/s pacing
	}
	assert.InDelta(t, bucketRate(5e6), w.bucket.rate, 1)
	data := bytes.Repeat(tsBytes(7), 300) // 395 kB: about 0.25 s at 12.5 Mbit/s less the depth
	t0 := time.Now()
	_, err := w.Write(data)
	require.NoError(t, err)
	assert.Greater(t, time.Since(t0), 150*time.Millisecond)
	assert.True(t, bytes.Equal(data, f.got.Bytes()))
}

// A receiver behind a link of about three times the stream's bitrate, at
// gosrt's default 120 ms latency, gets the GOP replay it joins on without a
// continuity break: the replay leaves at 2.5 times the bitrate, not at
// line rate into the link's queue.
func TestSRTThrottledJoin(t *testing.T) {
	e := start(t, Options{})
	h264 := configOf(t, "h264_aac.ts", media.CodecH264)
	frame := func(i, size int, key bool) media.Frame {
		data := make([]byte, size)
		data[0] = 0x41
		if key {
			data[0] = 0x65
		}
		for j := 1; j < size; j++ {
			data[j] = byte(j*3 + i)
		}
		dts := int64(i) * 3600
		return media.Frame{Track: 0, Config: h264, PTS: dts, DTS: dts, Key: key, Data: [][]byte{data}, Epoch: 1}
	}
	// 100 kB keyframes and 15 kB frames at 25 fps: about 3.7 Mbit/s.
	frames := make([]media.Frame, 0, 150)
	for i := range 150 {
		size := 15_000
		if i%25 == 0 {
			size = 100_000
		}
		frames = append(frames, frame(i, size, i%25 == 0))
	}
	pre, rest := frames[:25], frames[25:]
	p := newProxy(t, e.addr)
	p.Throttle(1_400_000, 100*time.Millisecond) // about 3 x; a 100 ms router queue
	data := play(t, e, p.Addr(), "live/s", pre, rest, 1, nil)
	assert.Zero(t, ccErrors(t, data), "no continuity break at the receiver")
	got := readBack(t, data)
	assert.Len(t, got, len(frames), "the replay and every frame after it")
	assert.Zero(t, e.srv.Stats().EndedStalled)
}

// feedMeter tells w about frames of size bytes every 40 ms over [from, to)
// seconds of stream time.
func feedMeter(w *writer, from, to float64, size int) {
	for t := from; t < to; t += 0.04 {
		w.Frame(int64(t*media.ClockRate), size)
	}
}

// A gap in the stream (an outage, a failover) does not drag the pacing
// rate below the stream's: the window starts over and the previous rate
// holds until it is covered again.
func TestWriterRateAfterGap(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	w := newWriter(ctx, cancel, &fakeConn{}, payloadSize, time.Minute, new(atomic.Uint64))
	w.Prebuffered(750_000, media.ClockRate)
	feedMeter(w, 0, 3, 30_000) // 30 kB every 40 ms: 6 Mbit/s
	want := bucketRate(6e6)
	require.InDelta(t, want, w.bucket.rate, want*0.01)
	for i, t0 := 0, 6.0; t0 < 10; i, t0 = i+1, t0+0.04 { // after a 3 s gap
		w.Frame(int64(t0*media.ClockRate), 30_000)
		require.InDelta(t, want, w.bucket.rate, want*0.01, "frame %d after the gap", i)
	}
}

// Frames the hub skipped for a slow connection (a resync) do not drag the
// pacing rate down either.
func TestWriterRateAfterResync(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	w := newWriter(ctx, cancel, &fakeConn{}, payloadSize, time.Minute, new(atomic.Uint64))
	w.Prebuffered(750_000, media.ClockRate)
	feedMeter(w, 0, 3, 30_000)
	want := bucketRate(6e6)
	require.InDelta(t, want, w.bucket.rate, want*0.01)
	w.Resync()
	// 0.8 s skipped, under the 1 s that counts as a gap on its own.
	for i, t0 := 0, 3.8; t0 < 8; i, t0 = i+1, t0+0.04 {
		w.Frame(int64(t0*media.ClockRate), 30_000)
		require.InDelta(t, want, w.bucket.rate, want*0.01, "frame %d after the resync", i)
	}
}
