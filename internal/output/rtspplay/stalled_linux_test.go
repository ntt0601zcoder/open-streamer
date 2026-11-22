package rtspplay

import (
	"context"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// shrinkSend shrinks the send buffer of an accepted connection, so a player
// that reads slowly blocks the server's writes early.
func shrinkSend(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
}

// smallRcvBuf sets SO_RCVBUF before connect, so the window stays small.
func smallRcvBuf(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
	}); err != nil {
		return err
	}
	return serr
}

// smallRcvDial dials a player with a small receive buffer.
func smallRcvDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Control: smallRcvBuf}).DialContext(ctx, network, addr)
}

// hubDropped reports whether the hub dropped frames for any subscriber.
func hubDropped(e *env) bool {
	for _, s := range e.h.Stats() {
		if s.Dropped > 0 {
			return true
		}
	}
	return false
}

// A TCP player that cannot keep up loses frames and resumes at the next
// keyframe with its parameter sets, aligned audio, and never a delta frame
// after the gap; sequence numbers stay unbroken.
func TestPlaySlowConsumer(t *testing.T) {
	e := start(t, Options{Queue: 32, Server: rtsp.ServerConfig{ConnHook: shrinkSend}}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := tagged(looped(t, "h264_aac.ts", 6))
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		e.h.Write(f)
	}
	c := play(t, e.url, "tcp", smallRcvDial)
	s := sessionOf(c)

	paused := make(chan struct{})
	pkts := collect(c, func(p []rtsp.Packet) bool {
		if len(p) == 200 {
			close(paused)
			// Stop reading until the hub drops frames for this player.
			deadline := time.Now().Add(waitFor)
			for !hubDropped(e) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		return false
	})
	done := feed(ctx, e.h, rest, 8, nil)
	select {
	case <-paused:
	case <-time.After(waitFor):
		t.Fatal("player read too little")
	}
	<-done
	e.h.Close()
	r := wait(t, pkts)
	sum := check(t, s, r.pkts, true)
	require.Eventually(t, func() bool { return e.srv.Stats().EndedHubClosed == 1 }, waitFor, time.Millisecond)
	st := e.srv.Stats()
	require.Positive(t, st.Resyncs, "the player lost frames")
	assert.Equal(t, st.Resyncs, st.Mux.Resyncs)
	assert.LessOrEqual(t, uint64(sum.resumes), st.Resyncs)
	_, jumps := tagRuns(t, s, frames, r.pkts) // a keyframe after every gap, however small
	assert.Positive(t, jumps, "the player saw the gap")
	assert.LessOrEqual(t, uint64(jumps), st.Resyncs, "a jump only where the hub reported a loss")
	assert.Zero(t, sum.seqGaps, "dropped frames consume no sequence numbers")
	assert.Equal(t, 2, sum.byes)
}

// A TCP player that stops reading is ended by the write deadline; another
// player goes on, and the hub's writes never wait for either.
func TestPlayStalledPlayerDropped(t *testing.T) {
	e := start(t, Options{Server: rtsp.ServerConfig{WriteTimeout: 500 * time.Millisecond, ConnHook: shrinkSend}}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := tagged(looped(t, "h264_aac.ts", 4))
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		e.h.Write(f)
	}
	rawSetup(t, e.addr, e.url, "RTP/AVP/TCP;unicast;interleaved=0-1", smallRcvDial) // never read
	good := play(t, e.url, "tcp", nil)
	s := sessionOf(good)
	var count atomic.Int64
	pkts := collect(good, func(p []rtsp.Packet) bool {
		count.Store(int64(len(p)))
		return false
	})
	done := feed(ctx, e.h, rest, 2, nil)
	require.Eventually(t, func() bool { return e.srv.Stats().EndedDeadline == 1 },
		waitFor, 10*time.Millisecond, "stalled player ended")
	n0 := count.Load()
	require.Eventually(t, func() bool { return count.Load() > n0+50 }, waitFor, 10*time.Millisecond, "the other player goes on")
	st := e.srv.Stats()
	assert.Equal(t, 1, st.Players)
	assert.Positive(t, st.Server.Timeouts, "the server counted the missed deadline")
	cancel()
	longest := <-done
	assert.Less(t, longest, 100*time.Millisecond, "hub writes never block")
	e.h.Close()
	r := wait(t, pkts)
	sum := check(t, s, r.pkts, true)
	assert.Zero(t, sum.seqGaps)
	assert.Zero(t, sum.resumes)
	_, jumps := tagRuns(t, s, frames, r.pkts)
	assert.Zero(t, jumps, "the other player lost nothing")
}
