package rtmpsrv

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
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

// tlsErrors collects the calls of a listener's TLSOptions.OnError.
type tlsErrors struct {
	mu    sync.Mutex
	addrs []string
	errs  []error
}

func (e *tlsErrors) onError(remote net.Addr, err error) {
	e.mu.Lock()
	e.addrs = append(e.addrs, remote.String())
	e.errs = append(e.errs, err)
	e.mu.Unlock()
}

func (e *tlsErrors) remotes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.addrs...)
}

// wait waits for n calls and returns their addresses: a failure is counted
// just before it is reported.
func (e *tlsErrors) wait(t *testing.T, n int) []string {
	t.Helper()
	require.Eventually(t, func() bool { return len(e.remotes()) >= n }, 5*time.Second, 5*time.Millisecond,
		"want %d OnError calls", n)
	return e.remotes()
}

// mustRegisterPlayTLS registers h for app on an RTMPS listener and
// unregisters it when the test ends.
func mustRegisterPlayTLS(t *testing.T, r *Registry, addr, app string, h PlayHandler, cfg rtmp.Config, pol Policy, opt TLSOptions) func() {
	t.Helper()
	unregister, err := r.RegisterPlayTLS(addr, app, h, cfg, pol, opt)
	require.NoError(t, err)
	t.Cleanup(unregister)
	return unregister
}

// mustRegisterTLS registers sink for path on the RTMPS listener of addr and
// unregisters it when the test ends.
func mustRegisterTLS(t *testing.T, r *Registry, addr, path string, sink PublishSink) func() {
	t.Helper()
	unregister, err := r.RegisterPublishTLS(addr, path, sink)
	require.NoError(t, err)
	t.Cleanup(unregister)
	return unregister
}

// recordingConn keeps every byte read from the conn: the peer's TLS records.
type recordingConn struct {
	net.Conn
	mu sync.Mutex
	in []byte
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.in = append(c.in, p[:n]...)
	c.mu.Unlock()
	return n, err
}

// lastRecord is the content type of the last whole TLS record received, or
// 0 when there is none or the bytes do not end on a record boundary.
func (c *recordingConn) lastRecord() byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var typ byte
	for b := c.in; len(b) > 0; {
		if len(b) < 5 || len(b) < 5+int(binary.BigEndian.Uint16(b[3:5])) {
			return 0
		}
		typ = b[0]
		b = b[5+int(binary.BigEndian.Uint16(b[3:5])):]
	}
	return typ
}

// recordAlert is the TLS record content type of an alert, close_notify
// included. Up to TLS 1.2 it is visible in the record header.
const recordAlert = 21

// dialer is an rtmp.Config.Dial that keeps the connections it opened, each
// wrapped by wrap when that is set.
type dialer struct {
	local string // the local IP to dial from, when set
	wrap  func(net.Conn) net.Conn

	mu    sync.Mutex
	conns []net.Conn
}

func (d *dialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var nd net.Dialer
	if d.local != "" {
		nd.LocalAddr = &net.TCPAddr{IP: net.ParseIP(d.local)}
	}
	c, err := nd.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if d.wrap != nil {
		c = d.wrap(c)
	}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

// last is the connection opened last.
func (d *dialer) last(t *testing.T) net.Conn {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotEmpty(t, d.conns, "nothing was dialled")
	return d.conns[len(d.conns)-1]
}

func (d *dialer) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
}

// endsWithin reports whether the peer ended c within d: a read fails with
// anything but a timeout. What the peer sends before is dropped.
func endsWithin(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 4096)
	for {
		if _, err := c.Read(buf); err != nil {
			var ne net.Error
			return !errors.As(err, &ne) || !ne.Timeout()
		}
	}
}

// clientHello returns the first flight of a TLS client: one ClientHello
// record.
func clientHello(t *testing.T) []byte {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() {
		// The handshake fails once the pipe closes.
		_ = tls.Client(client, &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS12}).HandshakeContext(context.Background())
	}()
	hello := make([]byte, 5, 1024)
	_, err := io.ReadFull(server, hello)
	require.NoError(t, err)
	hello = append(hello, make([]byte, binary.BigEndian.Uint16(hello[3:5]))...)
	_, err = io.ReadFull(server, hello[5:])
	require.NoError(t, err)
	return hello
}

// waitTLSFailures waits for the listener on addr to have counted n failed
// TLS handshakes.
func waitTLSFailures(t *testing.T, r *Registry, addr string, n uint64) {
	t.Helper()
	require.Eventually(t, func() bool { return r.Stats(addr).TLSHandshakeFailures == n },
		5*time.Second, 5*time.Millisecond, "want %d failed TLS handshakes, have %d", n, r.Stats(addr).TLSHandshakeFailures)
}

// counterKeys are the keys Stats.Counters has had since before RTMPS: every
// RTMP listen input reports them, and must not gain one.
var counterKeys = []string{
	"rejected_busy", "rejected_flood", "rejected_play_caps",
	"rejected_play_publish_path", "rejected_play_unknown", "rejected_unknown",
}

// framePlayer is a PlayHandler that writes frames to each player, then
// waits for its end.
type framePlayer struct {
	frames []rtmp.Media
}

func (h *framePlayer) Live(string) bool { return true }

func (h *framePlayer) Serve(ctx context.Context, conn *rtmp.ServerConn, _ string) {
	for _, m := range h.frames {
		if conn.WriteMedia(m) != nil {
			return
		}
	}
	select {
	case <-ctx.Done():
	case <-conn.Done():
	}
}

// An RTMPS listener takes publishers and players as a plain one does: the
// sink and the player get the same frames over either, the handshake is the
// registry's own and Accept gets the bare *tls.Conn, while the connection
// hook keeps the TCP connection.
func TestRegistryTLS(t *testing.T) {
	ca := tlstest.NewCA(t)
	frames := []rtmp.Media{
		{Type: rtmp.MsgAudio, Timestamp: 0, Payload: []byte{0xAF, 0x00, 0x12, 0x10}},
		{Type: rtmp.MsgVideo, Timestamp: 10, Payload: []byte{0x17, 0x01, 0x00, 0x00, 0x00, 0xDE, 0xAD}},
		{Type: rtmp.MsgAudio, Timestamp: 23, Payload: []byte{0xAF, 0x01, 0x21, 0x00}},
	}
	type result struct {
		published, played []rtmp.Media
		hooked, accepted  []net.Conn
		playerIn          *recordingConn // what the player received, below TLS
		playerEnd         error
		stats             Stats
	}
	// run publishes frames to pub/<key> and plays live/s, on a listener of
	// scheme, then unregisters the play handler and reads the player to its
	// end.
	run := func(t *testing.T, scheme string) result {
		t.Helper()
		var (
			res result
			mu  sync.Mutex
		)
		r := &Registry{
			ConnHook: func(c net.Conn) {
				mu.Lock()
				res.hooked = append(res.hooked, c)
				mu.Unlock()
			},
			accept: func(ctx context.Context, c net.Conn, cfg rtmp.Config, decide func(rtmp.Request) error) (*rtmp.ServerConn, error) {
				mu.Lock()
				res.accepted = append(res.accepted, c)
				mu.Unlock()
				return rtmp.Accept(ctx, c, cfg, decide)
			},
		}
		addr := "127.0.0.1:" + freeTCPPort(t)
		sink := newFakeSink()
		t.Cleanup(sink.close)
		h := &framePlayer{frames: frames}
		var unplay func()
		if scheme == "rtmps" {
			unplay = mustRegisterPlayTLS(t, r, addr, "live", h, rtmp.Config{}, Policy{}, TLSOptions{Config: ca.Server(t)})
			mustRegisterTLS(t, r, addr, "pub/"+vKey, sink)
		} else {
			unplay = mustRegisterPlay(t, r, addr, "live", h, Policy{})
			mustRegister(t, r, addr, "pub/"+vKey, sink, Policy{})
		}
		// TLS 1.2 at most, so that an alert record can be told from data.
		d := &dialer{wrap: func(c net.Conn) net.Conn { return &recordingConn{Conn: c} }}
		t.Cleanup(d.closeAll)
		cfg := rtmp.Config{TLS: ca.Client(), Dial: d.dial}
		cfg.TLS.MaxVersion = tls.VersionTLS12
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		p, err := rtmp.Publish(ctx, scheme+"://"+addr+"/pub/"+vKey, cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })
		sc := sink.next(t)
		assert.Equal(t, "pub/"+vKey, sc.Request().Path())
		for _, m := range frames {
			require.NoError(t, p.WriteMedia(m))
		}
		for range frames {
			m, err := sc.ReadMedia()
			require.NoError(t, err)
			res.published = append(res.published, m)
		}

		c, err := rtmp.PlayWith(ctx, scheme+"://"+addr+"/live/s", cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		res.playerIn = d.last(t).(*recordingConn)
		for n := 0; n < len(frames); {
			m, err := c.ReadMedia()
			require.NoError(t, err)
			res.played = append(res.played, m)
			if m.Type != rtmp.MsgDataAMF0 {
				n++
			}
		}
		res.stats = r.Stats(addr)
		unplay()
		for err == nil {
			_, err = c.ReadMedia()
		}
		res.playerEnd = err
		mu.Lock()
		defer mu.Unlock()
		return res
	}
	plain := run(t, "rtmp")
	secure := run(t, "rtmps")

	assert.Equal(t, frames, plain.published, "the plain listener's publish")
	assert.Equal(t, plain.published, secure.published, "an RTMPS publisher's frames are a plain one's")
	assert.Equal(t, plain.played, secure.played, "an RTMPS player's frames are a plain one's")
	assert.GreaterOrEqual(t, len(secure.played), len(frames))

	// The existing fields count an RTMPS listener's sessions the same way.
	for name, st := range map[string]Stats{"rtmp": plain.stats, "rtmps": secure.stats} {
		assert.Equal(t, 1, st.Routes, name)
		assert.Equal(t, 1, st.Apps, name)
		assert.Equal(t, 1, st.Players, name)
		assert.Equal(t, map[string]int{"live/s": 1}, st.StreamPlayers, name)
		assert.Zero(t, st.PrePublish, name)
		assert.Zero(t, st.TLSHandshakeFailures, name)
		assert.Equal(t, counterKeys, slices.Sorted(maps.Keys(st.Counters())), "%s: Counters has the keys it had", name)
	}

	// R-S8: the hook has the TCP connection, whose close is immediate, and
	// Accept the *tls.Conn itself, the only type pkg/rtmp's close rule knows.
	require.Len(t, secure.hooked, 2)
	require.Len(t, secure.accepted, 2)
	for i := range secure.hooked {
		assert.IsType(t, (*net.TCPConn)(nil), secure.hooked[i], "the hook's conn")
		tc, ok := secure.accepted[i].(*tls.Conn)
		require.True(t, ok, "Accept got a %T, want a *tls.Conn", secure.accepted[i])
		assert.Same(t, secure.hooked[i], tc.NetConn(), "the TLS conn is over the hook's")
		assert.IsType(t, (*net.TCPConn)(nil), plain.accepted[i], "a plain listener's Accept")
	}

	// A healthy player is told the end: crypto/tls reports io.EOF for a
	// connection cut on a record boundary too, so the record is the proof.
	assert.ErrorIs(t, secure.playerEnd, io.EOF)
	assert.EqualValues(t, recordAlert, secure.playerIn.lastRecord(), "the last TLS record the player got is close_notify")

	t.Run("a failed handshake is no counter", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		mustRegisterPlayTLS(t, r, addr, "live", newFakePlayer(), rtmp.Config{}, Policy{}, TLSOptions{Config: ca.Server(t)})
		c := idleTCP(t, addr)
		_, err := c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
		require.NoError(t, err)
		waitTLSFailures(t, r, addr, 1)
		counters := r.Stats(addr).Counters()
		assert.Equal(t, counterKeys, slices.Sorted(maps.Keys(counters)))
		for k, v := range counters {
			assert.Zero(t, v, k)
		}
	})
	t.Run("options without a config", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		_, err := r.RegisterPlayTLS(addr, "live", newFakePlayer(), rtmp.Config{}, Policy{}, TLSOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rtmpsrv:")
		assert.Zero(t, r.Stats(addr).ID, "nothing was opened")
	})
}

// A listener is plain or TLS for as long as it is open, and a registration
// of the other kind is refused without changing anything. A TLS publish
// route never opens a listener.
func TestRegistryTLSKinds(t *testing.T) {
	ca := tlstest.NewCA(t)
	opt := TLSOptions{Config: ca.Server(t)}
	r := &Registry{}
	addr := "127.0.0.1:" + freeTCPPort(t)
	refused := func(t *testing.T) {
		t.Helper()
		var d net.Dialer
		c, err := d.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		require.Error(t, err, "something listens on %s", addr)
	}

	// No listener: a TLS publish route does not open one.
	_, err := r.RegisterPublishTLS(addr, "pub/"+vKey, newFakeSink())
	require.ErrorIs(t, err, ErrNoTLSListener)
	assert.NotContains(t, err.Error(), vKey)
	assert.Zero(t, r.Stats(addr).ID)
	refused(t)

	// A TLS listener refuses the plain calls.
	unplay := mustRegisterPlayTLS(t, r, addr, "live", newFakePlayer(), rtmp.Config{}, Policy{}, opt)
	before := r.Stats(addr)
	_, err = r.RegisterPublish(addr, "pub/"+vKey, newFakeSink(), rtmp.Config{}, Policy{})
	require.ErrorIs(t, err, ErrTLSListener)
	assert.NotContains(t, err.Error(), vKey)
	_, err = r.RegisterPlay(addr, "other", newFakePlayer(), rtmp.Config{}, Policy{})
	require.ErrorIs(t, err, ErrTLSListener)
	assert.Equal(t, before, r.Stats(addr), "a refused call changes nothing")
	// The TLS calls share it, as the plain ones share a plain listener.
	unpublish := mustRegisterTLS(t, r, addr, "pub/"+vKey, newFakeSink())
	unother := mustRegisterPlayTLS(t, r, addr, "other", newFakePlayer(), rtmp.Config{}, Policy{}, opt)
	st := r.Stats(addr)
	assert.Equal(t, before.ID, st.ID)
	assert.Equal(t, 1, st.Routes)
	assert.Equal(t, 2, st.Apps)
	_, err = r.RegisterPublishTLS(addr, "pub/"+vKey, newFakeSink())
	require.Error(t, err, "a duplicate path")
	assert.NotErrorIs(t, err, ErrNoTLSListener)

	// The last unregister closes it, and the address is free for either
	// kind: a plain play opens a plain listener.
	unother()
	unplay()
	assert.Equal(t, before.ID, r.Stats(addr).ID, "the publish route keeps the listener")
	unpublish()
	assert.Zero(t, r.Stats(addr).ID)
	refused(t)
	h := newFakePlayer()
	mustRegisterPlay(t, r, addr, "live", h, Policy{})
	mustPlay(t, "rtmp://"+addr+"/live/s", rtmp.Config{})
	h.next(t)

	// A plain listener refuses the TLS calls.
	before = r.Stats(addr)
	_, err = r.RegisterPlayTLS(addr, "other", newFakePlayer(), rtmp.Config{}, Policy{}, opt)
	require.ErrorIs(t, err, ErrPlainListener)
	_, err = r.RegisterPublishTLS(addr, "pub/"+vKey, newFakeSink())
	require.ErrorIs(t, err, ErrPlainListener)
	assert.NotContains(t, err.Error(), vKey)
	assert.Equal(t, before, r.Stats(addr), "a refused call changes nothing")
	mustPlay(t, "rtmp://"+addr+"/live/s", rtmp.Config{}) // still plain
	h.next(t)
}

// The wrong protocol, or silence, on an RTMPS listener: each such client is
// ended by the handshake timeout, holds a pre-publish place meanwhile, and
// is one failure of its own listener.
func TestRegistryTLSWrongProtocol(t *testing.T) {
	const (
		handshakeTimeout = 200 * time.Millisecond
		limit            = 1200 * time.Millisecond
	)
	ca := tlstest.NewCA(t)

	t.Run("bad clients", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		errs := &tlsErrors{}
		mustRegisterPlayTLS(t, r, addr, "live", newFakePlayer(), rtmp.Config{HandshakeTimeout: handshakeTimeout}, Policy{},
			TLSOptions{Config: ca.Server(t), OnError: errs.onError})
		want := make([]string, 0, 3)

		// A plain RTMP player.
		d := &dialer{}
		t.Cleanup(d.closeAll)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := rtmp.PlayWith(ctx, "rtmp://"+addr+"/live/s", rtmp.Config{Dial: d.dial})
		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err)
		assert.Less(t, time.Since(start), limit, "a plain player")
		want = append(want, d.last(t).LocalAddr().String())
		waitTLSFailures(t, r, addr, 1)

		// A client that sends nothing.
		silent := idleTCP(t, addr)
		assert.True(t, endsWithin(silent, limit), "a silent client was not ended")
		want = append(want, silent.LocalAddr().String())
		waitTLSFailures(t, r, addr, 2)

		// A client that stops in the middle of its ClientHello.
		hello := clientHello(t)
		half := idleTCP(t, addr)
		_, err = half.Write(hello[:len(hello)/2])
		require.NoError(t, err)
		assert.True(t, endsWithin(half, limit), "a client mid-handshake was not ended")
		want = append(want, half.LocalAddr().String())
		waitTLSFailures(t, r, addr, 3)

		assert.Equal(t, want, errs.wait(t, 3), "one OnError call for each, with its address")
		errs.mu.Lock()
		for _, err := range errs.errs {
			assert.ErrorContains(t, err, "rtmpsrv: tls handshake:")
		}
		errs.mu.Unlock()
		st := r.Stats(addr)
		assert.Equal(t, counterKeys, slices.Sorted(maps.Keys(st.Counters())))
		assert.Zero(t, st.RejectedFlood)

		// Each gave its pre-publish place back: as many clients as one
		// address may have are admitted again, to fail in their turn.
		waitPrePublish(t, r, addr, 0)
		for range defaultMaxPrePublishPerIP {
			idleTCP(t, addr)
		}
		waitTLSFailures(t, r, addr, 3+defaultMaxPrePublishPerIP)
		assert.Zero(t, r.Stats(addr).RejectedFlood, "a client was turned away: a place was not given back")
		waitPrePublish(t, r, addr, 0)
	})

	t.Run("silent clients fill the pre-publish cap", func(t *testing.T) {
		// The default handshake timeout, so that the silent clients hold
		// their places for the whole row.
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		errs := &tlsErrors{}
		mustRegisterPlayTLS(t, r, addr, "live", newFakePlayer(), rtmp.Config{}, Policy{},
			TLSOptions{Config: ca.Server(t), OnError: errs.onError})
		sink := newFakeSink()
		t.Cleanup(sink.close)
		mustRegisterTLS(t, r, addr, "pub/"+vKey, sink)
		for range defaultMaxPrePublishPerIP {
			idleTCP(t, addr)
		}
		waitPrePublish(t, r, addr, defaultMaxPrePublishPerIP)
		start := time.Now()
		assert.True(t, endsWithin(idleTCP(t, addr), 2*time.Second), "the client over the cap was not closed")
		assert.Less(t, time.Since(start), time.Second, "the client over the cap waited for a handshake")
		assert.EqualValues(t, 1, r.Stats(addr).RejectedFlood)

		// The accept loop is not held up: another address publishes.
		d := &dialer{local: "127.0.0.2"}
		t.Cleanup(d.closeAll)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := rtmp.Publish(ctx, "rtmps://"+addr+"/pub/"+vKey, rtmp.Config{TLS: ca.Client(), Dial: d.dial})
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })
		sink.next(t)
		st := r.Stats(addr)
		assert.Equal(t, defaultMaxPrePublishPerIP, st.PrePublish)
		assert.Zero(t, st.TLSHandshakeFailures, "a connection closed over the cap is no failed handshake")
		assert.Empty(t, errs.remotes())
	})

	t.Run("a TLS client on a plain listener", func(t *testing.T) {
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		h := newFakePlayer()
		unregister, err := r.RegisterPlay(addr, "live", h, rtmp.Config{HandshakeTimeout: handshakeTimeout}, Policy{})
		require.NoError(t, err)
		t.Cleanup(unregister)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := rtmp.PlayWith(ctx, "rtmps://"+addr+"/live/s", rtmp.Config{TLS: ca.Client()})
		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err)
		assert.Less(t, time.Since(start), limit)
		waitPrePublish(t, r, addr, 0)
		assert.Zero(t, r.Stats(addr).TLSHandshakeFailures, "a plain listener has no TLS handshake")
		mustPlay(t, "rtmp://"+addr+"/live/s", rtmp.Config{}) // the listener goes on
		h.next(t)
	})

	t.Run("unregister during handshakes", func(t *testing.T) {
		// The default handshake timeout: only the unregister ends them.
		r := &Registry{}
		addr := "127.0.0.1:" + freeTCPPort(t)
		errs := &tlsErrors{}
		unregister := mustRegisterPlayTLS(t, r, addr, "live", newFakePlayer(), rtmp.Config{}, Policy{MaxPrePublishPerIP: 8},
			TLSOptions{Config: ca.Server(t), OnError: errs.onError})
		hello := clientHello(t)
		conns := make([]net.Conn, 5)
		for i := range conns {
			conns[i] = idleTCP(t, addr)
			if i%2 == 1 { // some silent, some mid-ClientHello
				_, err := conns[i].Write(hello[:len(hello)/2])
				require.NoError(t, err)
			}
		}
		waitPrePublish(t, r, addr, len(conns))
		l := r.lookup(addr)
		require.NotNil(t, l)
		start := time.Now()
		unregister()
		assert.Less(t, time.Since(start), time.Second, "the unregister waited for the handshakes")
		for _, c := range conns {
			assert.True(t, endsWithin(c, time.Second), "a client mid-handshake was not closed")
		}
		assert.Zero(t, l.tlsFailures.Load(), "a handshake that shutdown cut short is no failure")
		assert.Empty(t, errs.remotes(), "nor is it reported")
		l.mu.Lock()
		assert.Zero(t, l.pending, "every session gave its pre-publish place back")
		assert.Empty(t, l.perIP)
		l.mu.Unlock()
	})

	t.Run("two registries", func(t *testing.T) {
		var (
			regs  [2]*Registry
			addrs [2]string
			errs  [2]*tlsErrors
		)
		for i := range regs {
			regs[i], addrs[i], errs[i] = &Registry{}, "127.0.0.1:"+freeTCPPort(t), &tlsErrors{}
			mustRegisterPlayTLS(t, regs[i], addrs[i], "live", newFakePlayer(), rtmp.Config{HandshakeTimeout: handshakeTimeout}, Policy{},
				TLSOptions{Config: ca.Server(t), OnError: errs[i].onError})
		}
		bad := func(i int) string {
			c := idleTCP(t, addrs[i])
			_, err := c.Write([]byte("not a ClientHello"))
			require.NoError(t, err)
			return c.LocalAddr().String()
		}
		first := bad(0)
		waitTLSFailures(t, regs[0], addrs[0], 1)
		assert.Equal(t, []string{first}, errs[0].wait(t, 1))
		assert.Empty(t, errs[1].remotes(), "the other registry's listener was told")
		assert.Zero(t, regs[1].Stats(addrs[1]).TLSHandshakeFailures)
		second := bad(1)
		waitTLSFailures(t, regs[1], addrs[1], 1)
		assert.Equal(t, []string{second}, errs[1].wait(t, 1))
		assert.Equal(t, []string{first}, errs[0].remotes())
		assert.EqualValues(t, 1, regs[0].Stats(addrs[0]).TLSHandshakeFailures)
	})
}

// stallPlayer is a PlayHandler for players that stop reading. Once gate
// closes, and when write is set, it writes to its player until a WriteMedia
// fails and reports that call; then it waits for the route's end, whatever
// the conn does, and lingers.
type stallPlayer struct {
	write   bool
	linger  time.Duration
	gate    chan struct{}
	results chan stallResult
}

type stallResult struct {
	err  error
	took time.Duration
}

func (h *stallPlayer) Live(string) bool { return true }

func (h *stallPlayer) Serve(ctx context.Context, conn *rtmp.ServerConn, _ string) {
	if h.write {
		select {
		case <-h.gate:
		case <-ctx.Done():
			return
		}
		// Small messages, so that the call that stalls does little else:
		// the players' buffers fill over the calls before it.
		frame := rtmp.Media{Type: rtmp.MsgVideo, Payload: make([]byte, 16<<10)}
		var res stallResult
		for range 4096 {
			start := time.Now()
			if res.err = conn.WriteMedia(frame); res.err != nil {
				res.took = time.Since(start)
				break
			}
		}
		h.results <- res
	}
	<-ctx.Done()
	time.Sleep(h.linger)
}

// R-S8: unregistering a play handler closes its players at once, however
// many have stopped reading, and a route's end closes a player still in its
// decision by its TCP connection.
func TestRegistryTLSUnregisterStalled(t *testing.T) {
	const (
		players      = 100
		writeTimeout = 200 * time.Millisecond
		slack        = 300 * time.Millisecond
	)
	ca := tlstest.NewCA(t)
	pol := Policy{
		MaxPrePublish: 2 * players, MaxPrePublishPerIP: 2 * players,
		MaxPlayersPerStream: 2 * players, MaxPlayersPerIP: 2 * players,
	}
	// shrink makes the buffers between the server and a player that stops
	// reading small.
	shrink := func(c net.Conn) net.Conn {
		_ = c.(*net.TCPConn).SetReadBuffer(4096)
		_ = c.(*net.TCPConn).SetWriteBuffer(4096)
		return c
	}
	// play connects the players, which read nothing after their play
	// started.
	play := func(t *testing.T, r *Registry, addr string) {
		t.Helper()
		d := &dialer{wrap: shrink}
		t.Cleanup(d.closeAll)
		cfg := rtmp.Config{TLS: ca.Client(), Dial: d.dial}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		errc := make(chan error, players)
		for range players {
			go func() {
				_, err := rtmp.PlayWith(ctx, "rtmps://"+addr+"/live/s", cfg)
				errc <- err
			}()
		}
		for range players {
			require.NoError(t, <-errc)
		}
		require.Eventually(t, func() bool { return r.Stats(addr).Players == players }, 5*time.Second, 5*time.Millisecond)
	}

	t.Run("after the write timeout", func(t *testing.T) {
		r := &Registry{ConnHook: func(c net.Conn) { shrink(c) }}
		addr := "127.0.0.1:" + freeTCPPort(t)
		h := &stallPlayer{write: true, gate: make(chan struct{}), results: make(chan stallResult, players)}
		unplay := mustRegisterPlayTLS(t, r, addr, "live", h, rtmp.Config{WriteTimeout: writeTimeout}, pol, TLSOptions{Config: ca.Server(t)})
		play(t, r, addr)
		close(h.gate)
		for range players {
			select {
			case res := <-h.results:
				require.ErrorIs(t, res.err, os.ErrDeadlineExceeded, "WriteMedia to a stalled player")
				assert.LessOrEqual(t, res.took, writeTimeout+slack, "the WriteMedia that timed out")
			case <-time.After(30 * time.Second):
				t.Fatal("a WriteMedia to a stalled player never failed")
			}
		}
		// Every player's buffer is still full: nothing was read.
		start := time.Now()
		unplay()
		assert.Less(t, time.Since(start), time.Second, "unregistering %d stalled players", players)
		assert.Zero(t, r.Stats(addr).ID)
	})

	t.Run("no write failed before", func(t *testing.T) {
		// The unregister's Close is the first of each session: with no
		// write in flight, tls.Conn.Close sends close_notify into a full
		// buffer, which costs 100 ms a session. The handler is slow to
		// return, so that it is the unregister that closes: one after the
		// other, the closes would last until the handlers returned and
		// closed the rest themselves.
		var (
			mu   sync.Mutex
			raws []net.Conn
		)
		r := &Registry{ConnHook: func(c net.Conn) {
			shrink(c)
			mu.Lock()
			raws = append(raws, c)
			mu.Unlock()
		}}
		addr := "127.0.0.1:" + freeTCPPort(t)
		const linger = 600 * time.Millisecond
		h := &stallPlayer{linger: linger}
		unplay := mustRegisterPlayTLS(t, r, addr, "live", h, rtmp.Config{PingInterval: time.Minute}, pol, TLSOptions{Config: ca.Server(t)})
		play(t, r, addr)
		// Fill each player's buffers below TLS, on the TCP connection the
		// hook was given, and keep them full: no write of the session fails
		// or is in flight, and a close_notify has nowhere to go. A buffer
		// filled once would not do: the kernel takes a little more after a
		// while. The writers end when their connection closes.
		mu.Lock()
		conns := append([]net.Conn{}, raws...)
		mu.Unlock()
		require.Len(t, conns, players)
		var (
			written atomic.Int64
			fill    sync.WaitGroup
		)
		for _, c := range conns {
			fill.Go(func() {
				junk := make([]byte, 64<<10)
				for {
					n, err := c.Write(junk)
					written.Add(int64(n))
					if err != nil {
						return
					}
				}
			})
		}
		// Every writer is stalled once none has finished a write for a while.
		for last, since := int64(-1), time.Now(); time.Since(since) < 150*time.Millisecond; time.Sleep(10 * time.Millisecond) {
			if n := written.Load(); n != last {
				last, since = n, time.Now()
			}
		}
		start := time.Now()
		done := make(chan time.Duration, 1)
		go func() {
			unplay()
			done <- time.Since(start)
		}()
		fill.Wait() // every connection is closed
		assert.Less(t, time.Since(start), linger-200*time.Millisecond, "closing %d stalled players", players)
		took := <-done
		assert.Less(t, took, time.Second, "unregistering %d stalled players", players)
		assert.GreaterOrEqual(t, took, linger, "the unregister did not wait for the handlers")
		assert.Zero(t, r.Stats(addr).ID)
	})

	t.Run("a route's end closes the raw connection", func(t *testing.T) {
		// Accept is replaced by one that asks for a play and then waits, as
		// a player whose command exchange stalls after the decision would:
		// only admit's AfterFunc can end the connection.
		release := make(chan struct{})
		decided := make(chan error, 1)
		r := &Registry{accept: func(_ context.Context, c net.Conn, _ rtmp.Config, decide func(rtmp.Request) error) (*rtmp.ServerConn, error) {
			decided <- decide(rtmp.Request{Play: true, App: "live", Stream: "s"})
			<-release
			_ = c.Close()
			return nil, errors.New("stalled after the decision")
		}}
		addr := "127.0.0.1:" + freeTCPPort(t)
		unplay := mustRegisterPlayTLS(t, r, addr, "live", newFakePlayer(), rtmp.Config{}, Policy{}, TLSOptions{Config: ca.Server(t)})
		// TLS 1.2 at most, so that an alert record can be told from data.
		in := &recordingConn{Conn: idleTCP(t, addr)}
		cfg := ca.Client()
		cfg.ServerName, cfg.MaxVersion = "127.0.0.1", tls.VersionTLS12
		tc := tls.Client(in, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, tc.HandshakeContext(ctx))
		require.NoError(t, <-decided)
		require.Equal(t, 1, r.Stats(addr).Players)

		done := make(chan struct{})
		go func() {
			defer close(done)
			unplay() // waits for the session, which waits for release
		}()
		start := time.Now()
		_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := tc.Read(make([]byte, 1))
		assert.Less(t, time.Since(start), time.Second, "the route's end did not close the connection at once")
		// The TCP connection was closed, not the TLS one: no close_notify.
		// crypto/tls reports io.EOF for a connection cut on a record
		// boundary too, so the records are the proof: the handshake's
		// (type 22) are all there is.
		assert.Error(t, err)
		assert.EqualValues(t, 22, in.lastRecord(), "the connection was closed through TLS")
		select {
		case <-done:
			t.Fatal("the unregister did not wait for the session")
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the unregister did not return")
		}
	})
}
