package rtmppush

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// smallBuf sets SO_RCVBUF and SO_SNDBUF to 4 KiB before connect or listen,
// so a peer that reads slowly blocks the writer early.
func smallBuf(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		for _, opt := range []int{syscall.SO_RCVBUF, syscall.SO_SNDBUF} {
			if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, opt, 4096); err != nil {
				serr = err
			}
		}
	}); err != nil {
		return err
	}
	return serr
}

// smallDial dials with small socket buffers.
func smallDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{Control: smallBuf}).DialContext(ctx, network, addr)
}

// slowServer is a publish server with small socket buffers that, on each
// connection, reads pauseAfter messages, stops reading for pause, then reads
// on until the stream ends, recording every message. A negative pause stops
// reading for good.
type slowServer struct {
	addr string
	ln   net.Listener
	wg   sync.WaitGroup

	mu    sync.Mutex
	conns []net.Conn
	msgs  []rtmp.Media
	ended int
}

func newSlowServer(t *testing.T, pauseAfter int, pause time.Duration) *slowServer {
	t.Helper()
	ln, err := (&net.ListenConfig{Control: smallBuf}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &slowServer{addr: ln.Addr().String(), ln: ln}
	stop := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				s.serve(c, pauseAfter, pause, stop)
			}()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *slowServer) serve(c net.Conn, pauseAfter int, pause time.Duration, stop <-chan struct{}) {
	sc, err := rtmp.Accept(context.Background(), c, rtmp.Config{}, func(rtmp.Request) error { return nil })
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	for n := 0; ; n++ {
		if n == pauseAfter {
			if pause < 0 {
				<-stop
				return
			}
			select {
			case <-time.After(pause):
			case <-stop:
				return
			}
		}
		m, err := sc.ReadMedia()
		if err != nil {
			s.mu.Lock()
			s.ended++
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, m)
		s.mu.Unlock()
	}
}

func (s *slowServer) media() []rtmp.Media {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rtmp.Media(nil), s.msgs...)
}

func (s *slowServer) endedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// A target that cannot keep up makes the pusher lose frames; it resumes at
// the next keyframe with onMetaData and headers again on the same
// connection, and the hub's writes never wait for it.
func TestPushSlowTarget(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := newSlowServer(t, 200, 1500*time.Millisecond)
	frames := looped(t, "h264_aac.ts", 2)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	p := newPusher(t, h, "rtmp://"+srv.addr+"/live/"+vKey, Options{Queue: 32, RTMP: rtmp.Config{Dial: smallDial}})
	_, done := run(t, p)
	require.Eventually(t, func() bool { return len(h.Stats()) == 1 }, waitFor, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	longest := <-feed(ctx, h, rest, 4)
	assert.Less(t, longest, 100*time.Millisecond, "hub writes never block")
	h.Close()
	require.NoError(t, waitRun(t, done))
	require.Eventually(t, func() bool { return srv.endedCount() == 1 }, waitFor, time.Millisecond)

	s := check(t, srv.media())
	st := p.Stats()
	require.Positive(t, st.Resyncs, "the pusher lost frames")
	assert.Equal(t, st.Resyncs, st.Mux.Resyncs)
	assert.GreaterOrEqual(t, s.metas, 2, "onMetaData again at a resume")
	assert.LessOrEqual(t, uint64(s.metas-1), st.Resyncs)
	assert.Equal(t, uint64(1), st.Connects)
	assert.Zero(t, st.Failures, "a slow target is not a failure")
}

// Canceling Run while it is blocked in a write to a target that stopped
// reading returns at once and unsubscribes; that is no failure.
func TestPushContextCancel(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := newSlowServer(t, 5, -1)
	frames := looped(t, "h264_aac.ts", 4)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	p := newPusher(t, h, "rtmp://"+srv.addr+"/live/"+vKey, Options{
		WriteTimeout: time.Minute, RTMP: rtmp.Config{Dial: smallDial},
	})
	cancelRun, done := run(t, p)
	require.Eventually(t, func() bool { return len(h.Stats()) == 1 }, waitFor, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fed := feed(ctx, h, rest, 16)
	require.Eventually(t, func() bool {
		st := h.Stats()
		return len(st) == 1 && st[0].Dropped > 0
	}, waitFor, time.Millisecond, "the pusher is stuck in a write")

	start := time.Now()
	cancelRun()
	err := waitRun(t, done)
	assert.Less(t, time.Since(start), time.Second, "Run returns promptly")
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, errors.Is(err, net.ErrClosed))
	assert.Empty(t, h.Stats(), "unsubscribed")
	st := p.Stats()
	assert.Zero(t, st.Failures)
	assert.Empty(t, st.LastError)
	assert.Equal(t, StateStopped, st.State)
	cancel()
	<-fed
}
