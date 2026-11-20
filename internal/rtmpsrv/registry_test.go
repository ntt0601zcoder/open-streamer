package rtmpsrv

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

// fakeSink is a PublishSink that keeps every conn it is offered.
type fakeSink struct {
	isBusy    atomic.Bool
	busyCalls atomic.Int32
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

func (f *fakeSink) Offer(conn *rtmp.ServerConn) bool {
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

func (f *fakeSink) Busy(time.Time) bool {
	f.busyCalls.Add(1)
	return f.isBusy.Load()
}

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
func mustRegister(t *testing.T, r *Registry, addr, path string, sink PublishSink, pol Policy) func() {
	t.Helper()
	unregister, err := r.RegisterPublish(addr, path, sink, rtmp.Config{}, pol)
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

func waitPrePublish(t *testing.T, r *Registry, addr string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		st := r.Stats(addr)
		return st.ID != 0 && st.PrePublish == n
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
func TestRegistrySharedPort(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	a, b := newFakeSink(), newFakeSink()
	a.read, b.read = true, true
	t.Cleanup(a.close)
	t.Cleanup(b.close)
	mustRegister(t, r, addr, "/live/a", a, Policy{})
	mustRegister(t, r, addr, "live/b?ignored=1", b, Policy{})

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
	mustRegister(t, r, addr, "live/inst/s", c, Policy{})
	mustPublish(t, "rtmp://"+addr+"/live/inst/s")
	c.next(t)

	// Unknown paths, and a stream name missing on either side, are rejected
	// with the same text as busy ones, and counted.
	mustReject(t, "rtmp://"+addr+"/live/zz")
	mustReject(t, "rtmp://"+addr+"/other/a")
	a.isBusy.Store(true)
	mustReject(t, "rtmp://"+addr+"/live/a")
	st := r.Stats(addr)
	assert.EqualValues(t, 2, st.RejectedUnknown)
	assert.EqualValues(t, 1, st.RejectedBusy)
	assert.Zero(t, st.RejectedFlood)
	// Published sessions do not count as pre-publish; a rejected one leaves
	// just after the client reads the rejection.
	waitPrePublish(t, r, addr, 0)
}

// The registry's decide rejects an empty app or stream instead of letting
// NormalizePath fold them into another route.
func TestRegistryEmptyAppOrStream(t *testing.T) {
	r := &Registry{}
	sink := newFakeSink()
	t.Cleanup(sink.close)
	l := &listener{reg: r, routes: map[string]*route{"live/s": {sink: sink}}, now: time.Now}
	for _, req := range []rtmp.PublishRequest{
		{App: "live/s", Stream: ""},
		{App: "", Stream: "live/s"},
		{App: "/", Stream: "live/s"},
	} {
		rt, err := l.reserve(req)
		assert.Nil(t, rt, "%+v", req)
		require.Error(t, err, "%+v", req)
		assert.Equal(t, "rtmpsrv: rtmp: stream not available", err.Error())
		assert.ErrorIs(t, err, rtmp.ErrStreamNotAvailable)
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

// Publishers are routed on the joined, normalised path: the query is
// dropped, slashes are trimmed, and an app of two segments with a stream of
// one meets an app of one with a stream of two. An empty app or stream
// never matches.
func TestRegistryRouting(t *testing.T) {
	r := &Registry{}
	routeS, routeInst := &route{sink: newFakeSink()}, &route{sink: newFakeSink()}
	l := &listener{reg: r, routes: map[string]*route{"live/s": routeS, "live/inst/s": routeInst}, now: time.Now}
	for _, c := range []struct {
		req  rtmp.Request
		want *route
	}{
		{rtmp.Request{App: "live", Stream: "s"}, routeS},
		{rtmp.Request{App: "live?token=x", Stream: "s"}, routeS},
		{rtmp.Request{App: "live", Stream: "s?token=x"}, routeS},
		{rtmp.Request{App: "/live/", Stream: "/s/"}, routeS},
		{rtmp.Request{App: "live/inst", Stream: "s"}, routeInst},
		{rtmp.Request{App: "live", Stream: "inst/s"}, routeInst},
		{rtmp.Request{App: "live/inst?token=x", Stream: "s"}, routeInst},
		{rtmp.Request{App: "live", Stream: ""}, nil},
		{rtmp.Request{App: "live/s", Stream: ""}, nil},
		{rtmp.Request{App: "live/s", Stream: "?token=x"}, nil},
		{rtmp.Request{App: "", Stream: "live/s"}, nil},
		{rtmp.Request{App: "live", Stream: "zz"}, nil},
	} {
		rt, err := l.reserve(c.req)
		if c.want == nil {
			assert.Error(t, err, "%+v", c.req)
			assert.Nil(t, rt, "%+v", c.req)
			continue
		}
		require.NoError(t, err, "%+v", c.req)
		assert.Same(t, c.want, rt, "%+v", c.req)
		l.release(rt)
	}
}

func TestRegistryDuplicatePath(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	mustRegister(t, r, addr, "live/secretkey", newFakeSink(), Policy{})
	_, err := r.RegisterPublish(addr, "/live/secretkey/", newFakeSink(), rtmp.Config{}, Policy{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rtmpsrv:")
	assert.NotContains(t, err.Error(), "secretkey", "the stream key is redacted")
	assert.Contains(t, err.Error(), "live/sexxxxx")

	// A path without both an app and a stream is refused.
	for _, p := range []string{"", "/", "live", "/live/"} {
		_, err := r.RegisterPublish(addr, p, newFakeSink(), rtmp.Config{}, Policy{})
		assert.Error(t, err, "path %q", p)
	}
	// A bad address is refused.
	_, err = r.RegisterPublish("nope", "live/s", newFakeSink(), rtmp.Config{}, Policy{})
	assert.Error(t, err)
}

// An empty host, 0.0.0.0 and [::] with the same port are one listener; a
// specific host is a bind of its own.
func TestRegistryAddrNormalised(t *testing.T) {
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
		got, err := AddrKey(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	for _, bad := range []string{"", "1935", ":", "host:", ":abc", ":70000", ":0", "0.0.0.0:0", ":-1"} {
		_, err := AddrKey(bad)
		assert.Error(t, err, "%q", bad)
	}

	r := &Registry{}
	port := freeTCPPort(t)
	sinks := []*fakeSink{newFakeSink(), newFakeSink(), newFakeSink()}
	for i, addr := range []string{":" + port, "0.0.0.0:" + port, "[::]:" + port} {
		t.Cleanup(sinks[i].close)
		mustRegister(t, r, addr, "live/s"+strconv.Itoa(i), sinks[i], Policy{})
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
	_, err := r.RegisterPublish("127.0.0.1:"+port, "live/x", newFakeSink(), rtmp.Config{}, Policy{})
	assert.Error(t, err)
}

// After the last unregister the port is free again at once, for the registry
// and for anyone else.
func TestRegistryReopenSameAddr(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	for range 20 {
		sink := newFakeSink()
		unregister, err := r.RegisterPublish(addr, "live/s", sink, rtmp.Config{}, Policy{})
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
		un1, err := r.RegisterPublish(addr, "live/a", newFakeSink(), rtmp.Config{}, Policy{})
		require.NoError(t, err)
		done := make(chan struct{})
		go func() {
			defer close(done)
			un1()
		}()
		un2, err := r.RegisterPublish(addr, "live/b", newFakeSink(), rtmp.Config{}, Policy{})
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
func TestRegistryCloseDuringAccept(t *testing.T) {
	addr := "127.0.0.1:" + freeTCPPort(t)
	cycle := func() {
		r := &Registry{}
		sink := newFakeSink()
		defer sink.close()
		unregister, err := r.RegisterPublish(addr, "live/s", sink, rtmp.Config{HandshakeTimeout: time.Minute}, Policy{})
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
func TestRegistryFloodCaps(t *testing.T) {
	t.Run("per listener", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		sink := newFakeSink()
		t.Cleanup(sink.close)
		mustRegister(t, r, addr, "live/s", sink, Policy{MaxPrePublish: 8, MaxPrePublishPerIP: 100})
		conns := make([]net.Conn, 8)
		for i := range conns {
			conns[i] = idleTCP(t, addr)
		}
		waitPrePublish(t, r, addr, 8)
		ninth := idleTCP(t, addr)
		assert.True(t, closedByPeer(ninth, 2*time.Second), "the 9th conn was not closed")
		assert.EqualValues(t, 1, r.Stats(addr).RejectedFlood)
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
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		mustRegister(t, r, addr, "live/s", newFakeSink(), Policy{MaxPrePublish: 100, MaxPrePublishPerIP: 4})
		conns := make([]net.Conn, 4)
		for i := range conns {
			conns[i] = idleTCP(t, addr)
		}
		waitPrePublish(t, r, addr, 4)
		fifth := idleTCP(t, addr)
		assert.True(t, closedByPeer(fifth, 2*time.Second), "the 5th conn was not closed")
		assert.EqualValues(t, 1, r.Stats(addr).RejectedFlood)
		for _, c := range conns {
			assert.False(t, closedByPeer(c, 10*time.Millisecond), "a conn under the cap was closed")
		}
	})
	t.Run("defaults", func(t *testing.T) {
		p := Policy{MaxPrePublish: 8, MaxPrePublishPerIP: 2}.withDefaults()
		assert.Equal(t, 8, p.MaxPrePublish, "set fields are kept")
		assert.Equal(t, 2, p.MaxPrePublishPerIP, "set fields are kept")
	})
}

// A publisher accepted by decide whose Accept then fails releases the route,
// and one offered to a closed sink is closed.
func TestRegistryReleaseAfterAcceptError(t *testing.T) {
	var failed atomic.Bool
	r := &Registry{accept: func(ctx context.Context, c net.Conn, cfg rtmp.Config, decide func(rtmp.PublishRequest) error) (*rtmp.ServerConn, error) {
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
	mustRegister(t, r, addr, "live/s", sink, Policy{})

	mustPublish(t, "rtmp://"+addr+"/live/s") // the client saw Publish.Start, the server failed
	require.Eventually(t, failed.Load, 5*time.Second, 5*time.Millisecond)
	waitPrePublish(t, r, addr, 0)
	mustPublish(t, "rtmp://"+addr+"/live/s") // not rejected as busy
	sink.next(t)
	assert.Zero(t, r.Stats(addr).RejectedBusy)

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

// fakePlayer is a PlayHandler. Live is true for the names in live, or for
// every name when live is nil. Serve writes one audio message, then waits
// for ctx or the conn to end, and lingers before returning.
type fakePlayer struct {
	live   map[string]bool
	block  chan struct{} // when set, Live waits until it is closed
	linger time.Duration

	mu     sync.Mutex
	asked  []string
	names  []string // the names Serve was given
	served chan *rtmp.ServerConn

	serving, ended atomic.Int32
}

func newFakePlayer(live ...string) *fakePlayer {
	h := &fakePlayer{served: make(chan *rtmp.ServerConn, 64)}
	if len(live) > 0 {
		h.live = map[string]bool{}
		for _, n := range live {
			h.live[n] = true
		}
	}
	return h
}

func (h *fakePlayer) Live(name string) bool {
	h.mu.Lock()
	h.asked = append(h.asked, name)
	h.mu.Unlock()
	if h.block != nil {
		<-h.block
	}
	return h.live == nil || h.live[name]
}

func (h *fakePlayer) Serve(ctx context.Context, conn *rtmp.ServerConn, name string) {
	h.mu.Lock()
	h.names = append(h.names, name)
	h.mu.Unlock()
	h.serving.Add(1)
	defer h.serving.Add(-1)
	defer h.ended.Add(1)
	h.served <- conn
	_ = conn.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Payload: []byte{0xAF, 0x00, 0x12, 0x10}})
	select {
	case <-ctx.Done():
	case <-conn.Done():
	}
	time.Sleep(h.linger)
}

func (h *fakePlayer) servedNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.names...)
}

func (h *fakePlayer) askedFor() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

func (h *fakePlayer) next(t *testing.T) *rtmp.ServerConn {
	t.Helper()
	select {
	case c := <-h.served:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no player served")
		return nil
	}
}

// mustRegisterPlay registers h for app and unregisters it when the test
// ends.
func mustRegisterPlay(t *testing.T, r *Registry, addr, app string, h PlayHandler, pol Policy) func() {
	t.Helper()
	unregister, err := r.RegisterPlay(addr, app, h, rtmp.Config{}, pol)
	require.NoError(t, err)
	t.Cleanup(unregister)
	return unregister
}

// fromIP dials from the local address ip (a loopback address other than
// 127.0.0.1 counts as another peer).
func fromIP(ip string) rtmp.Config {
	return rtmp.Config{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
		return d.DialContext(ctx, network, addr)
	}}
}

// mustPlay plays url, reads the message Serve writes and returns the
// player, which is closed when the test ends.
func mustPlay(t *testing.T, url string, cfg rtmp.Config) *rtmp.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := rtmp.PlayWith(ctx, url, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	for { // |RtmpSampleAccess comes first
		m, err := c.ReadMedia()
		require.NoError(t, err)
		if m.Type == rtmp.MsgAudio {
			return c
		}
	}
}

// mustNotPlay plays url and expects NetStream.Play.StreamNotFound with
// "stream not available"; it returns the client's error.
func mustNotPlay(t *testing.T, url string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := rtmp.Play(ctx, url)
	if c != nil {
		_ = c.Close()
	}
	var st *rtmp.StatusError
	require.ErrorAs(t, err, &st)
	assert.Equal(t, "NetStream.Play.StreamNotFound", st.Code)
	assert.Equal(t, "stream not available", st.Description)
	return err
}

// ended reports whether the server ended the player c within d.
func ended(c *rtmp.ClientConn, d time.Duration) bool {
	errc := make(chan error, 1)
	go func() {
		for {
			if _, err := c.ReadMedia(); err != nil {
				errc <- err
				return
			}
		}
	}()
	select {
	case <-errc:
		return true
	case <-time.After(d):
		_ = c.Close()
		<-errc
		return false
	}
}

// Publishers and players share one port: publishers reach the sink of their
// path, players the handler of their app with the stream name, and the
// connection hook sees every connection.
func TestServerPublishAndPlayRouting(t *testing.T) {
	var hooked atomic.Int32
	r := &Registry{ConnHook: func(net.Conn) { hooked.Add(1) }}
	addr := "127.0.0.1:" + freeTCPPort(t)
	sink := newFakeSink()
	t.Cleanup(sink.close)
	live, other := newFakePlayer(), newFakePlayer()
	mustRegisterPlay(t, r, addr, "/live/", live, Policy{})
	mustRegister(t, r, addr, "ingest/"+vKey, sink, Policy{})
	mustRegisterPlay(t, r, addr, "other", other, Policy{})
	r.mu.Lock()
	n := len(r.listeners)
	r.mu.Unlock()
	assert.Equal(t, 1, n, "one TCP listener for publish and play")

	mustPublish(t, "rtmp://"+addr+"/ingest/"+vKey)
	assert.Equal(t, "ingest/"+vKey, sink.next(t).Request().Path())

	mustPlay(t, "rtmp://"+addr+"/live/a?token=x", rtmp.Config{})
	c := live.next(t)
	assert.True(t, c.IsPlay())
	mustPlay(t, "rtmp://"+addr+"/live/a", rtmp.Config{})
	live.next(t)
	mustPlay(t, "rtmp://"+addr+"/other/b", rtmp.Config{})
	other.next(t)
	assert.Equal(t, []string{"a", "a"}, live.askedFor())
	assert.Equal(t, []string{"a", "a"}, live.servedNames(), "the handler gets the stream name")
	assert.Equal(t, []string{"b"}, other.askedFor())
	assert.Equal(t, []string{"b"}, other.servedNames())

	// An app nobody registered is rejected.
	mustNotPlay(t, "rtmp://"+addr+"/nope/a")
	st := r.Stats(addr)
	assert.Equal(t, 3, st.Players)
	assert.Equal(t, map[string]int{"live/a": 2, "other/b": 1}, st.StreamPlayers)
	assert.EqualValues(t, 1, st.RejectedPlayUnknown)
	assert.Zero(t, st.RejectedUnknown, "plays do not count as publishes")
	assert.EqualValues(t, 5, hooked.Load())
	waitPrePublish(t, r, addr, 0) // players are no longer pre-publish

	// Registration errors: no app, a duplicate app, a bad address.
	for _, app := range []string{"", "/", "?x=1"} {
		_, err := r.RegisterPlay(addr, app, newFakePlayer(), rtmp.Config{}, Policy{})
		assert.Error(t, err, "app %q", app)
	}
	_, err := r.RegisterPlay(addr, "live", newFakePlayer(), rtmp.Config{}, Policy{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rtmpsrv:")
	_, err = r.RegisterPlay("nope", "live", newFakePlayer(), rtmp.Config{}, Policy{})
	assert.Error(t, err)
}

// vKey is a fake stream key.
const vKey = "sk" + "_4b1d9e" + "07aa"

// A play of a publish path is rejected before any reservation: the
// publisher goes on, the sink is never asked, the route stays free, and the
// key reaches neither the handler nor the client's error.
func TestPlayPathIsNotPublishKey(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	sink := newFakeSink()
	sink.read = true
	t.Cleanup(sink.close)
	mustRegister(t, r, addr, "live/"+vKey, sink, Policy{})
	h := newFakePlayer() // every name is live
	mustRegisterPlay(t, r, addr, "live", h, Policy{})

	p := mustPublish(t, "rtmp://"+addr+"/live/"+vKey)
	sink.next(t)
	tags := registryTags()
	require.NoError(t, p.WriteTag(tags[0]))
	require.Eventually(t, func() bool { return sink.media.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	busy := sink.busyCalls.Load() // the publish's
	for _, url := range []string{
		"rtmp://" + addr + "/live/" + vKey,
		"rtmp://" + addr + "/live/" + vKey + "?token=x",
		"rtmp://" + addr + "/live/" + vKey + "/",
	} {
		err := mustNotPlay(t, url)
		assert.NotContains(t, err.Error(), vKey)
	}
	assert.Empty(t, h.askedFor(), "the handler never sees the key")
	assert.Equal(t, busy, sink.busyCalls.Load(), "a play never reserves the route")
	select {
	case <-sink.offered:
		t.Fatal("a player was offered to the sink")
	default:
	}
	st := r.Stats(addr)
	assert.EqualValues(t, 3, st.RejectedPlayPublishPath)
	assert.Zero(t, st.RejectedUnknown)
	assert.Zero(t, st.RejectedBusy)
	assert.Zero(t, st.Players)

	// The publisher is unaffected.
	for _, tag := range tags[1:] {
		require.NoError(t, p.WriteTag(tag))
	}
	require.Eventually(t, func() bool { return sink.media.Load() == 3 }, 5*time.Second, 5*time.Millisecond)

	// With no play handler on the address, the answer and the count are
	// the same.
	r2 := &Registry{}
	addr2 := "127.0.0.1:" + freeTCPPort(t)
	mustRegister(t, r2, addr2, "live/"+vKey, newFakeSink(), Policy{})
	mustNotPlay(t, "rtmp://"+addr2+"/live/"+vKey)
	assert.EqualValues(t, 1, r2.Stats(addr2).RejectedPlayPublishPath)
	assert.Zero(t, r2.Stats(addr2).RejectedUnknown)
}

// An unknown app, a stream that is not live, a publish path and a cap all
// get the same answer.
func TestPlayUnknownPath(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	pol := Policy{MaxPlayersPerStream: 1} // the first registrant's counts
	mustRegister(t, r, addr, "live/"+vKey, newFakeSink(), pol)
	h := newFakePlayer("on")
	mustRegisterPlay(t, r, addr, "live", h, Policy{})
	mustPlay(t, "rtmp://"+addr+"/live/on", rtmp.Config{})

	answers := make([]string, 0, 5)
	for _, path := range []string{"other/on", "live/off", "live/inst/on", "live/" + vKey, "live/on"} {
		err := mustNotPlay(t, "rtmp://"+addr+"/"+path)
		var st *rtmp.StatusError
		require.ErrorAs(t, err, &st)
		answers = append(answers, st.Error())
	}
	for _, a := range answers {
		assert.Equal(t, answers[0], a)
	}
	st := r.Stats(addr)
	// live/inst/on asks for app live/inst, which has no handler.
	assert.EqualValues(t, 3, st.RejectedPlayUnknown, "two unknown apps and a stream that is not live")
	assert.EqualValues(t, 1, st.RejectedPlayPublishPath)
	assert.EqualValues(t, 1, st.RejectedPlayCaps)
	assert.Equal(t, 1, st.Players)
	assert.Equal(t, []string{"on", "off", "on"}, h.askedFor())
}

// Plays are routed by the full play path, whichever of app and stream
// carries the stream name: ffmpeg sends app "live" and stream "cam",
// MediaMTX app "live/cam" and an empty stream. The handler whose app is the
// path up to its last segment gets that segment as the name. An app no
// handler serves, an empty remainder, a remainder containing "/" and a
// publish path in either form all get the same rejection.
func TestPlayRoutingByFullPath(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	mustRegister(t, r, addr, "live/"+vKey, newFakeSink(), Policy{})
	h := newFakePlayer() // every name is live
	mustRegisterPlay(t, r, addr, "live", h, Policy{})
	l := r.lookup(addr)
	require.NotNil(t, l)
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	for _, req := range []rtmp.Request{
		{Play: true, App: "live", Stream: "cam"},
		{Play: true, App: "live/cam", Stream: ""},
		{Play: true, App: "/live/cam/", Stream: ""},
		{Play: true, App: "live?token=x", Stream: "cam?x=1"},
		{Play: true, App: "", Stream: "live/cam"},
	} {
		slot, err := l.admit(c1, req, "127.0.0.1")
		require.NoError(t, err, "%+v", req)
		assert.Equal(t, "cam", slot.name, "%+v", req)
		assert.Same(t, h, slot.rt.h, "%+v", req)
		assert.Equal(t, "live/cam", slot.path, "%+v", req)
		l.endPlay(slot)
	}
	assert.Equal(t, []string{"cam", "cam", "cam", "cam", "cam"}, h.askedFor())

	answers := make([]string, 0, 9)
	for _, req := range []rtmp.Request{
		{Play: true, App: "other", Stream: "cam"},      // no handler
		{Play: true, App: "other/cam", Stream: ""},     // no handler, whole path in app
		{Play: true, App: "live", Stream: ""},          // empty remainder
		{Play: true, App: "live/", Stream: "/"},        // empty remainder
		{Play: true, App: "live", Stream: "inst/cam"},  // remainder with "/"
		{Play: true, App: "live/inst/cam", Stream: ""}, // remainder with "/"
		{Play: true, App: "live", Stream: vKey},        // publish path, split
		{Play: true, App: "live/" + vKey, Stream: ""},  // publish path, whole
		{Play: true, App: "live/" + vKey + "?x=1", Stream: ""},
	} {
		slot, err := l.admit(c1, req, "127.0.0.1")
		assert.Nil(t, slot, "%+v", req)
		require.Error(t, err, "%+v", req)
		assert.ErrorIs(t, err, rtmp.ErrStreamNotAvailable)
		answers = append(answers, err.Error())
	}
	for _, a := range answers {
		assert.Equal(t, answers[0], a)
	}
	st := r.Stats(addr)
	assert.EqualValues(t, 6, st.RejectedPlayUnknown)
	assert.EqualValues(t, 3, st.RejectedPlayPublishPath)
	assert.Equal(t, []string{"cam", "cam", "cam", "cam", "cam"}, h.askedFor(), "no rejected name reaches the handler")
	assert.Zero(t, st.Players)

	// Over the wire, a player that splits the path is served the name.
	mustPlay(t, "rtmp://"+addr+"/live/cam", rtmp.Config{})
	h.next(t)
	assert.Equal(t, []string{"cam"}, h.servedNames())
}

// A play app of several segments routes the same way: the name is the last
// segment of the path, whether the client splits it or sends it whole.
func TestPlayRoutingMultiSegmentApp(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	h := newFakePlayer()
	mustRegisterPlay(t, r, addr, "a/b", h, Policy{})
	l := r.lookup(addr)
	require.NotNil(t, l)
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	for _, req := range []rtmp.Request{
		{Play: true, App: "a/b", Stream: "cam"},
		{Play: true, App: "a/b/cam", Stream: ""},
	} {
		slot, err := l.admit(c1, req, "127.0.0.1")
		require.NoError(t, err, "%+v", req)
		assert.Equal(t, "cam", slot.name, "%+v", req)
		assert.Same(t, h, slot.rt.h, "%+v", req)
		assert.Equal(t, "a/b/cam", slot.path, "%+v", req)
		l.endPlay(slot)
	}
	assert.Equal(t, []string{"cam", "cam"}, h.askedFor())
}

// Players are capped per stream, per listener and per IP; a player that
// leaves frees its slots. Players connect one at a time, so the pre-publish
// cap of 4 per IP is never reached.
func TestPlayFloodCaps(t *testing.T) {
	t.Run("per stream and listener", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		mustRegisterPlay(t, r, addr, "live", newFakePlayer(), Policy{MaxPlayersPerStream: 3, MaxPlayers: 5})
		a := make([]*rtmp.ClientConn, 0, 3)
		for range 3 {
			a = append(a, mustPlay(t, "rtmp://"+addr+"/live/a", rtmp.Config{}))
		}
		mustNotPlay(t, "rtmp://"+addr+"/live/a")
		mustPlay(t, "rtmp://"+addr+"/live/b", rtmp.Config{})
		mustPlay(t, "rtmp://"+addr+"/live/b", rtmp.Config{})
		mustNotPlay(t, "rtmp://"+addr+"/live/c")
		st := r.Stats(addr)
		assert.EqualValues(t, 2, st.RejectedPlayCaps)
		assert.Equal(t, 5, st.Players)
		assert.Equal(t, map[string]int{"live/a": 3, "live/b": 2}, st.StreamPlayers)

		require.NoError(t, a[0].Close())
		require.Eventually(t, func() bool { return r.Stats(addr).Players == 4 }, 5*time.Second, 5*time.Millisecond)
		mustPlay(t, "rtmp://"+addr+"/live/c", rtmp.Config{})
		mustNotPlay(t, "rtmp://"+addr+"/live/a")
		assert.Equal(t, map[string]int{"live/a": 2, "live/b": 2, "live/c": 1}, r.Stats(addr).StreamPlayers)
	})
	t.Run("per IP", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		mustRegisterPlay(t, r, addr, "live", newFakePlayer(), Policy{MaxPlayersPerIP: 6})
		for range 6 {
			mustPlay(t, "rtmp://"+addr+"/live/a", rtmp.Config{})
		}
		mustNotPlay(t, "rtmp://"+addr+"/live/b")
		mustPlay(t, "rtmp://"+addr+"/live/a", fromIP("127.0.0.2")) // another peer
		st := r.Stats(addr)
		assert.EqualValues(t, 1, st.RejectedPlayCaps)
		assert.Zero(t, st.RejectedFlood, "established players are not pre-publish")
		assert.Equal(t, 7, st.Players)
	})
	t.Run("defaults", func(t *testing.T) {
		assert.Equal(t, Policy{
			MaxPrePublish: 64, MaxPrePublishPerIP: 4,
			MaxPlayersPerStream: 256, MaxPlayers: 1024, MaxPlayersPerIP: 64,
		}, Policy{}.withDefaults())
	})
}

// Unregistering a play handler ends its players and waits for Serve to
// return; the listener stays while a publish route is registered, and the
// last unregister closes it.
func TestPlayHandlerUnregister(t *testing.T) {
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	sink := newFakeSink()
	t.Cleanup(sink.close)
	unpublish := mustRegister(t, r, addr, "ingest/s", sink, Policy{})
	h := newFakePlayer()
	h.linger = 100 * time.Millisecond
	unplay := mustRegisterPlay(t, r, addr, "live", h, Policy{})
	players := make([]*rtmp.ClientConn, 0, 3)
	for _, name := range []string{"a", "a", "b"} {
		players = append(players, mustPlay(t, "rtmp://"+addr+"/live/"+name, rtmp.Config{}))
		h.next(t)
	}
	id := r.Stats(addr).ID

	unplay()
	assert.Zero(t, h.serving.Load(), "unregister returned before Serve did")
	assert.EqualValues(t, 3, h.ended.Load())
	for _, p := range players {
		assert.True(t, ended(p, 2*time.Second), "a player was not ended")
	}
	unplay() // idempotent
	st := r.Stats(addr)
	assert.Equal(t, id, st.ID, "the publish route keeps the listener")
	assert.Zero(t, st.Players)
	assert.Empty(t, st.StreamPlayers)
	mustNotPlay(t, "rtmp://"+addr+"/live/a")
	mustPublish(t, "rtmp://"+addr+"/ingest/s")
	sink.next(t)

	// A play in its decision when the handler leaves is waited for and
	// never served.
	h2 := newFakePlayer()
	h2.block = make(chan struct{})
	unplay2 := mustRegisterPlay(t, r, addr, "live", h2, Policy{})
	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := rtmp.Play(ctx, "rtmp://"+addr+"/live/x")
		if c != nil {
			_ = c.Close()
		}
		errc <- err
	}()
	require.Eventually(t, func() bool { return len(h2.askedFor()) == 1 }, 5*time.Second, 5*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		unplay2()
	}()
	select {
	case <-done:
		t.Fatal("unregister did not wait for Live")
	case <-time.After(100 * time.Millisecond):
	}
	close(h2.block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unregister did not return")
	}
	assert.Error(t, <-errc)
	assert.Zero(t, h2.ended.Load(), "served after unregister")
	assert.Zero(t, r.Stats(addr).Players)

	// The last unregister closes the listener and frees the port.
	unpublish()
	assert.Zero(t, r.Stats(addr).ID)
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	// A play handler alone holds the listener too.
	h3 := newFakePlayer()
	unplay3 := mustRegisterPlay(t, r, addr, "live", h3, Policy{})
	p := mustPlay(t, "rtmp://"+addr+"/live/a", rtmp.Config{})
	unplay3()
	assert.True(t, ended(p, 2*time.Second))
	assert.Zero(t, r.Stats(addr).ID)
	ln, err = new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	require.NoError(t, err)
	require.NoError(t, ln.Close())
}
