package ingest

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

// fakeSink is an rtmpSink that keeps every conn it is offered.
type fakeSink struct {
	isBusy atomic.Bool
	// read makes offer read each conn's media in a goroutine until it ends,
	// counting it in media, then close the conn.
	read  bool
	media atomic.Int64

	mu      sync.Mutex
	closed  bool
	conns   []*rtmp.ServerConn
	refused []*rtmp.ServerConn
	wg      sync.WaitGroup
	offered chan *rtmp.ServerConn
}

func newFakeSink() *fakeSink {
	return &fakeSink{offered: make(chan *rtmp.ServerConn, 16)}
}

func (f *fakeSink) offer(conn *rtmp.ServerConn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		f.refused = append(f.refused, conn)
		return false
	}
	f.conns = append(f.conns, conn)
	if f.read {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer conn.Close()
			for {
				if _, err := conn.ReadMedia(); err != nil {
					return
				}
				f.media.Add(1)
			}
		}()
	}
	f.offered <- conn
	return true
}

func (f *fakeSink) busy(time.Time) bool { return f.isBusy.Load() }

// close refuses later offers and closes the conns taken so far.
func (f *fakeSink) close() {
	f.mu.Lock()
	f.closed = true
	conns := f.conns
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	f.wg.Wait()
}

func (f *fakeSink) next(t *testing.T) *rtmp.ServerConn {
	t.Helper()
	select {
	case c := <-f.offered:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no publisher offered to the sink")
		return nil
	}
}

// freeTCPPort returns a TCP port that was free a moment ago.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// mustRegister registers sink and unregisters it when the test ends.
func mustRegister(t *testing.T, r *rtmpRegistry, addr, path string, sink rtmpSink, pol listenPolicy) func() {
	t.Helper()
	unregister, err := r.register(addr, path, sink, rtmp.Config{}, pol)
	require.NoError(t, err)
	t.Cleanup(unregister)
	return unregister
}

// mustPublish publishes to url and returns the publisher, which is abandoned and
// closed when the test ends.
func mustPublish(t *testing.T, url string) *rtmptest.Publisher {
	t.Helper()
	p, code, err := rtmptest.Dial(context.Background(), url)
	require.NoError(t, err)
	require.Equal(t, "NetStream.Publish.Start", code)
	t.Cleanup(func() {
		p.Abandon()
		_ = p.Close()
	})
	return p
}

// mustReject publishes to url and expects BadName with "stream not
// available".
func mustReject(t *testing.T, url string) {
	t.Helper()
	p, code, err := rtmptest.Dial(context.Background(), url)
	require.Nil(t, p)
	require.Error(t, err)
	assert.Equal(t, "NetStream.Publish.BadName", code)
	assert.Contains(t, err.Error(), "stream not available")
}

// idleTCP opens a TCP connection that sends nothing.
func idleTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// closedByPeer reports whether the server closed c within d.
func closedByPeer(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.Is(err, io.EOF) || !errors.As(err, &ne) || !ne.Timeout()
}

func waitPrePublish(t *testing.T, r *rtmpRegistry, addr string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		l := r.lookup(addr)
		return l != nil && l.prePublish() == n
	}, 5*time.Second, 5*time.Millisecond, "want %d pre-publish sessions", n)
}

func registryTags() []rtmp.FLVTag {
	return []rtmp.FLVTag{
		{Type: rtmp.FLVScript, Data: []byte{0x02, 0x00, 0x0A, 'o', 'n', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a', 0x05}},
		{Type: rtmp.FLVAudio, Timestamp: 0, Data: []byte{0xAF, 0x00, 0x12, 0x10}},
		{Type: rtmp.FLVAudio, Timestamp: 23, Data: []byte{0xAF, 0x01, 0x21, 0x00}},
	}
}

// Two paths on one port share one TCP listener; each publisher reaches its
// own sink.
func TestRTMPRegistrySharedPort(t *testing.T) {
	r := &rtmpRegistry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	a, b := newFakeSink(), newFakeSink()
	a.read, b.read = true, true
	t.Cleanup(a.close)
	t.Cleanup(b.close)
	mustRegister(t, r, addr, "/live/a", a, listenPolicy{})
	mustRegister(t, r, addr, "live/b?ignored=1", b, listenPolicy{})

	r.mu.Lock()
	n := len(r.listeners)
	r.mu.Unlock()
	assert.Equal(t, 1, n, "one TCP listener for both paths")

	// The publishers stay connected until the test ends: closing one with
	// FCUnpublish races the sink closing its conn on EOF.
	for url, tags := range map[string][]rtmp.FLVTag{
		"rtmp://" + addr + "/live/a":         registryTags(),
		"rtmp://" + addr + "/live/b?token=x": registryTags()[:2],
	} {
		p := mustPublish(t, url)
		for _, tag := range tags {
			require.NoError(t, p.WriteTag(tag))
		}
	}
	require.Eventually(t, func() bool { return a.media.Load() == 3 && b.media.Load() == 2 },
		5*time.Second, 5*time.Millisecond, "a=%d b=%d", a.media.Load(), b.media.Load())
	assert.Equal(t, "live/a", a.next(t).Request().Path())
	assert.Equal(t, "live/b", b.next(t).Request().Path())

	// An app of two segments and a stream of one reach the same route as
	// one and two.
	c := newFakeSink()
	t.Cleanup(c.close)
	mustRegister(t, r, addr, "live/inst/s", c, listenPolicy{})
	mustPublish(t, "rtmp://"+addr+"/live/inst/s")
	c.next(t)

	// Unknown paths, and a stream name missing on either side, are rejected
	// with the same text as busy ones, and counted.
	mustReject(t, "rtmp://"+addr+"/live/zz")
	mustReject(t, "rtmp://"+addr+"/other/a")
	a.isBusy.Store(true)
	mustReject(t, "rtmp://"+addr+"/live/a")
	st := r.stats(addr)
	assert.EqualValues(t, 2, st["rejected_unknown"])
	assert.EqualValues(t, 1, st["rejected_busy"])
	assert.Zero(t, st["rejected_flood"])
	// Published sessions do not count as pre-publish; a rejected one leaves
	// just after the client reads the rejection.
	waitPrePublish(t, r, addr, 0)
}

// The registry's decide rejects an empty app or stream instead of letting
// NormalizePath fold them into another route.
func TestRTMPRegistryEmptyAppOrStream(t *testing.T) {
	r := &rtmpRegistry{}
	sink := newFakeSink()
	t.Cleanup(sink.close)
	l := &rtmpListener{reg: r, routes: map[string]*rtmpRoute{"live/s": {sink: sink}}, now: time.Now}
	for _, req := range []rtmp.PublishRequest{
		{App: "live/s", Stream: ""},
		{App: "", Stream: "live/s"},
		{App: "/", Stream: "live/s"},
	} {
		rt, err := l.reserve(req)
		assert.Nil(t, rt, "%+v", req)
		require.Error(t, err, "%+v", req)
		assert.Equal(t, "ingest: rtmp: stream not available", err.Error())
	}
	assert.EqualValues(t, 3, l.rejectedUnknown.Load())
	rt, err := l.reserve(rtmp.PublishRequest{App: "live", Stream: "s?token=x"})
	require.NoError(t, err)
	assert.Same(t, sink, rt.sink)
	// The route is reserved until released.
	_, err = l.reserve(rtmp.PublishRequest{App: "live", Stream: "s"})
	require.Error(t, err)
	assert.EqualValues(t, 1, l.rejectedBusy.Load())
	l.release(rt)
	rt, err = l.reserve(rtmp.PublishRequest{App: "live", Stream: "s"})
	require.NoError(t, err)
	l.release(rt)
}

func TestRTMPRegistryDuplicatePath(t *testing.T) {
	r := &rtmpRegistry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	mustRegister(t, r, addr, "live/secretkey", newFakeSink(), listenPolicy{})
	_, err := r.register(addr, "/live/secretkey/", newFakeSink(), rtmp.Config{}, listenPolicy{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ingest: rtmp:")
	assert.NotContains(t, err.Error(), "secretkey", "the stream key is redacted")
	assert.Contains(t, err.Error(), "live/sexxxxx")

	// A path without both an app and a stream is refused.
	for _, p := range []string{"", "/", "live", "/live/"} {
		_, err := r.register(addr, p, newFakeSink(), rtmp.Config{}, listenPolicy{})
		assert.Error(t, err, "path %q", p)
	}
	// A bad address is refused.
	_, err = r.register("nope", "live/s", newFakeSink(), rtmp.Config{}, listenPolicy{})
	assert.Error(t, err)
}

// An empty host, 0.0.0.0 and [::] with the same port are one listener; a
// specific host is a bind of its own.
func TestRTMPRegistryAddrNormalised(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{":1935", ":1935"},
		{"0.0.0.0:1935", ":1935"},
		{"[::]:1935", ":1935"},
		{"127.0.0.1:1935", "127.0.0.1:1935"},
		{"[::1]:1935", "[::1]:1935"},
		{"localhost:1935", "localhost:1935"},
		// Canonical forms: the port as a number, IPv4-mapped IPv6 unmapped,
		// every unspecified address the wildcard.
		{":01935", ":1935"},
		{"[::ffff:127.0.0.1]:1935", "127.0.0.1:1935"},
		{"[::ffff:0.0.0.0]:1935", ":1935"},
		{"[0:0::0]:1935", ":1935"},
		{"[0:0::1]:1935", "[::1]:1935"},
	} {
		got, err := rtmpAddrKey(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	for _, bad := range []string{"", "1935", ":", "host:", ":abc", ":70000", ":0", "0.0.0.0:0", ":-1"} {
		_, err := rtmpAddrKey(bad)
		assert.Error(t, err, "%q", bad)
	}

	r := &rtmpRegistry{}
	port := freeTCPPort(t)
	sinks := []*fakeSink{newFakeSink(), newFakeSink(), newFakeSink()}
	for i, addr := range []string{":" + port, "0.0.0.0:" + port, "[::]:" + port} {
		t.Cleanup(sinks[i].close)
		mustRegister(t, r, addr, "live/s"+strconv.Itoa(i), sinks[i], listenPolicy{})
	}
	r.mu.Lock()
	n := len(r.listeners)
	r.mu.Unlock()
	assert.Equal(t, 1, n)
	for i := range sinks {
		mustPublish(t, "rtmp://127.0.0.1:"+port+"/live/s"+strconv.Itoa(i))
		sinks[i].next(t)
	}
	// The wildcard holds the port, so a specific host is its own bind and
	// fails.
	_, err := r.register("127.0.0.1:"+port, "live/x", newFakeSink(), rtmp.Config{}, listenPolicy{})
	assert.Error(t, err)
}

// After the last unregister the port is free again at once, for the registry
// and for anyone else.
func TestRTMPRegistryReopenSameAddr(t *testing.T) {
	r := &rtmpRegistry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	for range 20 {
		sink := newFakeSink()
		unregister, err := r.register(addr, "live/s", sink, rtmp.Config{}, listenPolicy{})
		require.NoError(t, err)
		idleTCP(t, addr) // a session in flight
		unregister()
		unregister() // idempotent
		sink.close()
	}
	r.mu.Lock()
	n := len(r.listeners)
	r.mu.Unlock()
	assert.Zero(t, n)

	// Unregister racing a register of another path never fails the register.
	for range 20 {
		un1, err := r.register(addr, "live/a", newFakeSink(), rtmp.Config{}, listenPolicy{})
		require.NoError(t, err)
		done := make(chan struct{})
		go func() {
			defer close(done)
			un1()
		}()
		un2, err := r.register(addr, "live/b", newFakeSink(), rtmp.Config{}, listenPolicy{})
		require.NoError(t, err)
		<-done
		un2()
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	require.NoError(t, err)
	require.NoError(t, ln.Close())
}

// Unregistering while a publisher is mid-handshake ends its session at once
// and leaves no goroutine behind.
// Not parallel: it counts goroutines.
func TestRTMPRegistryCloseDuringAccept(t *testing.T) {
	addr := "127.0.0.1:" + freeTCPPort(t)
	cycle := func() {
		r := &rtmpRegistry{}
		sink := newFakeSink()
		defer sink.close()
		unregister, err := r.register(addr, "live/s", sink, rtmp.Config{HandshakeTimeout: time.Minute}, listenPolicy{})
		require.NoError(t, err)
		c := idleTCP(t, addr)
		_, err = c.Write([]byte{0x03}) // C0, then nothing
		require.NoError(t, err)
		waitPrePublish(t, r, addr, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			unregister()
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("unregister waited for the handshake timeout")
		}
		assert.True(t, closedByPeer(c, time.Second), "the session was not closed")
		_ = c.Close()
	}
	cycle() // warm up lazily started goroutines
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 10 {
		cycle()
	}
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		2*time.Second, 20*time.Millisecond, "goroutines before=%d now=%d", before, runtime.NumGoroutine())
}

// Connections over the per-listener or per-IP pre-publish cap are closed
// right after accept and counted; a freed slot takes a publisher again.
func TestRTMPRegistryFloodCaps(t *testing.T) {
	t.Run("per listener", func(t *testing.T) {
		r := &rtmpRegistry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		sink := newFakeSink()
		t.Cleanup(sink.close)
		mustRegister(t, r, addr, "live/s", sink, listenPolicy{MaxPrePublish: 8, MaxPrePublishPerIP: 100})
		conns := make([]net.Conn, 8)
		for i := range conns {
			conns[i] = idleTCP(t, addr)
		}
		waitPrePublish(t, r, addr, 8)
		ninth := idleTCP(t, addr)
		assert.True(t, closedByPeer(ninth, 2*time.Second), "the 9th conn was not closed")
		assert.EqualValues(t, 1, r.stats(addr)["rejected_flood"])
		for _, c := range conns {
			assert.False(t, closedByPeer(c, 10*time.Millisecond), "a conn under the cap was closed")
		}

		// One idle conn leaves: its slot takes a publisher.
		require.NoError(t, conns[0].Close())
		waitPrePublish(t, r, addr, 7)
		mustPublish(t, "rtmp://"+addr+"/live/s")
		sink.next(t)
		waitPrePublish(t, r, addr, 7)
	})
	t.Run("per IP", func(t *testing.T) {
		r := &rtmpRegistry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		mustRegister(t, r, addr, "live/s", newFakeSink(), listenPolicy{MaxPrePublish: 100, MaxPrePublishPerIP: 4})
		conns := make([]net.Conn, 4)
		for i := range conns {
			conns[i] = idleTCP(t, addr)
		}
		waitPrePublish(t, r, addr, 4)
		fifth := idleTCP(t, addr)
		assert.True(t, closedByPeer(fifth, 2*time.Second), "the 5th conn was not closed")
		assert.EqualValues(t, 1, r.stats(addr)["rejected_flood"])
		for _, c := range conns {
			assert.False(t, closedByPeer(c, 10*time.Millisecond), "a conn under the cap was closed")
		}
	})
	t.Run("defaults", func(t *testing.T) {
		assert.Equal(t, listenPolicy{MaxPrePublish: 64, MaxPrePublishPerIP: 4}, listenPolicy{}.withDefaults())
		assert.Equal(t, listenPolicy{MaxPrePublish: 8, MaxPrePublishPerIP: 2}, listenPolicy{MaxPrePublish: 8, MaxPrePublishPerIP: 2}.withDefaults())
	})
}

// A publisher accepted by decide whose Accept then fails releases the route,
// and one offered to a closed sink is closed.
func TestRTMPRegistryReleaseAfterAcceptError(t *testing.T) {
	var failed atomic.Bool
	r := &rtmpRegistry{accept: func(ctx context.Context, c net.Conn, cfg rtmp.Config, decide func(rtmp.PublishRequest) error) (*rtmp.ServerConn, error) {
		sc, err := rtmp.Accept(ctx, c, cfg, decide)
		if err == nil && failed.CompareAndSwap(false, true) {
			_ = sc.Close()
			return nil, errors.New("write failed after decide")
		}
		return sc, err
	}}
	addr := "127.0.0.1:" + freeTCPPort(t)
	sink := newFakeSink()
	t.Cleanup(sink.close)
	mustRegister(t, r, addr, "live/s", sink, listenPolicy{})

	mustPublish(t, "rtmp://"+addr+"/live/s") // the client saw Publish.Start, the server failed
	require.Eventually(t, failed.Load, 5*time.Second, 5*time.Millisecond)
	waitPrePublish(t, r, addr, 0)
	mustPublish(t, "rtmp://"+addr+"/live/s") // not rejected as busy
	sink.next(t)
	assert.Zero(t, r.stats(addr)["rejected_busy"])

	// A sink closed between decide and offer refuses the conn, which the
	// registry closes.
	sink.close()
	mustPublish(t, "rtmp://"+addr+"/live/s")
	var refused *rtmp.ServerConn
	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		if len(sink.refused) == 1 {
			refused = sink.refused[0]
		}
		return refused != nil
	}, 5*time.Second, 5*time.Millisecond)
	errc := make(chan error, 1)
	go func() {
		_, err := refused.ReadMedia()
		errc <- err
	}()
	select {
	case err := <-errc:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the refused conn was not closed")
	}
	// The route is free again.
	mustPublish(t, "rtmp://"+addr+"/live/s")
}

// An IPv6 host normally owns a /64, so the per-IP pre-publish count keys it
// by that prefix; an IPv4-mapped address counts as its IPv4 address.
func TestPeerKey(t *testing.T) {
	key := func(s string) string {
		t.Helper()
		return peerKey(&net.TCPAddr{IP: net.ParseIP(s), Port: 1935})
	}
	same := [][2]string{
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff"},
		{"2001:db8:1:2::1", "2001:db8:1:2:abcd::9"},
		{"::ffff:1.2.3.4", "1.2.3.4"},
		{"fe80::1", "fe80::2"},
	}
	for _, p := range same {
		if a, b := key(p[0]), key(p[1]); a != b {
			t.Errorf("key(%s) = %q, key(%s) = %q, want equal", p[0], a, p[1], b)
		}
	}
	differ := [][2]string{
		{"2001:db8:1:2::1", "2001:db8:1:3::1"},
		{"1.2.3.4", "1.2.3.5"},
		{"::1.2.3.4", "1.2.3.4"}, // IPv4-compatible is not IPv4-mapped
	}
	for _, p := range differ {
		if a, b := key(p[0]), key(p[1]); a == b {
			t.Errorf("key(%s) = key(%s) = %q, want different", p[0], p[1], a)
		}
	}
	if k := key("1.2.3.4"); k != "1.2.3.4" {
		t.Errorf("IPv4 key %q, want 1.2.3.4", k)
	}
	if k := key("2001:db8:1:2::1"); k != "2001:db8:1:2::/64" {
		t.Errorf("IPv6 key %q, want 2001:db8:1:2::/64", k)
	}
	// A non-TCP address falls back to its host string.
	if k := peerKey(&net.UnixAddr{Name: "@x", Net: "unix"}); k == "" {
		t.Error("empty key for a unix address")
	}
}
