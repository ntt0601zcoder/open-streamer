package rtmpplay

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
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

// A player that cannot keep up loses frames and resumes at the next keyframe
// with onMetaData and headers again, aligned audio, and never a delta frame
// after the gap.
func TestPlaySlowConsumer(t *testing.T) {
	e := start(t, Options{Queue: 32}, shrinkSend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := looped(t, "h264_aac.ts", 2)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		e.h.Write(f)
	}
	c := play(t, e.url, rtmp.Config{Dial: smallRcvDial})

	paused := make(chan struct{})
	msgs := collect(c, func(m []rtmp.Media) bool {
		if len(m) == 200 {
			close(paused)
			time.Sleep(1500 * time.Millisecond) // stop reading: the queue overflows
		}
		return false
	})
	done := feed(ctx, e.h, rest, 4)
	select {
	case <-paused:
	case <-time.After(waitFor):
		t.Fatal("player read too little")
	}
	<-done
	e.h.Close()
	r := wait(t, msgs)
	require.ErrorIs(t, r.err, io.EOF, "hub closed: end of stream")
	s := check(t, r.msgs)
	require.Eventually(t, func() bool { return e.srv.Stats().EndedHubClosed == 1 }, waitFor, time.Millisecond)
	st := e.srv.Stats()
	require.Positive(t, st.Resyncs, "the player lost frames")
	assert.Equal(t, st.Resyncs, st.Mux.Resyncs)
	assert.Positive(t, s.gaps, "the player saw the gap")
	assert.Equal(t, uint64(s.metas-1), st.Resyncs, "onMetaData again at every resume")
}

// A player that stops reading is ended by the write deadline; another player
// goes on, and the hub's writes never wait for either.
func TestPlayStalledPlayerDropped(t *testing.T) {
	e := start(t, Options{WriteTimeout: 500 * time.Millisecond}, shrinkSend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := looped(t, "h264_aac.ts", 4)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		e.h.Write(f)
	}
	play(t, e.url, rtmp.Config{Dial: smallRcvDial}) // never read
	good := play(t, e.url, rtmp.Config{})
	var count atomic.Int64
	msgs := collect(good, func(m []rtmp.Media) bool {
		count.Store(int64(len(m)))
		return false
	})
	done := feed(ctx, e.h, rest, 2)
	require.Eventually(t, func() bool { return e.srv.Stats().EndedDeadline == 1 },
		waitFor, 10*time.Millisecond, "stalled player ended")
	n0 := count.Load()
	require.Eventually(t, func() bool { return count.Load() > n0+50 }, waitFor, 10*time.Millisecond, "the other player goes on")
	assert.Equal(t, 1, e.srv.Stats().Players)
	cancel()
	longest := <-done
	assert.Less(t, longest, 100*time.Millisecond, "hub writes never block")
	e.h.Close()
	r := wait(t, msgs)
	require.ErrorIs(t, r.err, io.EOF)
	check(t, r.msgs)
}
