package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// Default listener policy for RTMP listen inputs.
const (
	defaultMaxPrePublish      = 64
	defaultMaxPrePublishPerIP = 4
	// maxAcceptBackoff caps the pause after a failing TCP accept (for
	// example out of file descriptors).
	maxAcceptBackoff = time.Second
)

// errStreamNotAvailable rejects a publish to an unknown path and to a busy
// one alike, so a client cannot probe which stream keys exist.
var errStreamNotAvailable = errors.New("ingest: rtmp: stream not available")

// rtmpSink receives the publishers of one listen path.
//
// offer and busy may still be called after a non-last unregister of the
// sink's route has returned: a session that reserved the route before the
// unregister goes on to offer its conn, possibly to a sink that has since
// closed. A closed sink must therefore refuse offers. The sink must never
// call unregister while holding a lock that offer or busy takes, since
// unregister may wait for a session that is calling them.
type rtmpSink interface {
	// offer hands over a publisher whose publish was accepted. It returns
	// false when the sink is closed; the registry then closes conn. It must
	// not block for long: unregistering waits for it.
	offer(conn *rtmp.ServerConn) bool
	// busy reports whether the sink has a live publisher that sent within
	// the last 2 s, in which case a new publisher is rejected.
	busy(now time.Time) bool
}

// listenPolicy bounds the sessions of one TCP listener that have not
// published yet. Zero fields take the defaults (64 and 4). The per-IP bound
// counts an IPv6 peer by its /64.
type listenPolicy struct {
	MaxPrePublish, MaxPrePublishPerIP int
}

func (p listenPolicy) withDefaults() listenPolicy {
	if p.MaxPrePublish <= 0 {
		p.MaxPrePublish = defaultMaxPrePublish
	}
	if p.MaxPrePublishPerIP <= 0 {
		p.MaxPrePublishPerIP = defaultMaxPrePublishPerIP
	}
	return p
}

// acceptFunc is rtmp.Accept; tests replace it.
type acceptFunc func(ctx context.Context, c net.Conn, cfg rtmp.Config, decide func(rtmp.PublishRequest) error) (*rtmp.ServerConn, error)

// rtmpRegistry shares one TCP listener per address between every RTMP
// listen input on it and routes each publisher to the input of its path.
type rtmpRegistry struct {
	accept acceptFunc // nil means rtmp.Accept

	// mu guards listeners and every listener's routes. It is never held
	// while calling a sink.
	mu        sync.Mutex
	listeners map[string]*rtmpListener
}

// rtmpListeners is the process-wide registry.
var rtmpListeners = &rtmpRegistry{}

// rtmpRoute is a registered path.
type rtmpRoute struct {
	sink     rtmpSink
	reserved bool // a publisher passed decide and is not offered yet; registry mu
}

// rtmpListener is one TCP listener and its sessions.
type rtmpListener struct {
	reg    *rtmpRegistry
	key    string
	ln     net.Listener
	cfg    rtmp.Config
	pol    listenPolicy
	accept acceptFunc
	now    func() time.Time
	ctx    context.Context // ends every session's Accept on shutdown
	cancel context.CancelFunc
	wg     sync.WaitGroup // the accept loop and every session

	routes map[string]*rtmpRoute // registry mu

	mu      sync.Mutex     // guards pending and perIP
	pending int            // sessions that have not published
	perIP   map[string]int // by peerKey: an IPv4 address or an IPv6 /64

	rejectedUnknown, rejectedBusy, rejectedFlood atomic.Uint64
}

// register routes publishers of path on addr to sink. The first register on
// an address opens its TCP listener with cfg and pol; later ones share it and
// their cfg and pol are ignored. The returned unregister removes the route;
// the last one closes the listener and waits for its sessions to end. It is
// idempotent. unregister must not be called while holding a lock that the
// sink's offer or busy takes (see rtmpSink).
func (r *rtmpRegistry) register(addr, path string, sink rtmpSink, cfg rtmp.Config, pol listenPolicy) (unregister func(), err error) {
	key, err := rtmpAddrKey(addr)
	if err != nil {
		return nil, err
	}
	path, _ = rtmp.NormalizePath(path, "")
	if app, stream, ok := strings.Cut(path, "/"); !ok || app == "" || stream == "" {
		return nil, fmt.Errorf("ingest: rtmp: listen %s: path %q needs an app and a stream name", key, redactRTMPPath(path))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.listeners[key]
	if l == nil {
		if l, err = r.listen(key, cfg, pol); err != nil {
			return nil, err
		}
		if r.listeners == nil {
			r.listeners = map[string]*rtmpListener{}
		}
		r.listeners[key] = l
	}
	if _, dup := l.routes[path]; dup {
		return nil, fmt.Errorf("ingest: rtmp: listen %s: path %s is already registered", key, redactRTMPPath(path))
	}
	l.routes[path] = &rtmpRoute{sink: sink}
	var once sync.Once
	return func() { once.Do(func() { r.unregister(l, path) }) }, nil
}

// listen opens the TCP listener for key and starts its accept loop. r.mu is
// held.
func (r *rtmpRegistry) listen(key string, cfg rtmp.Config, pol listenPolicy) (*rtmpListener, error) {
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", key)
	if err != nil {
		return nil, fmt.Errorf("ingest: rtmp: listen %s: %w", key, err)
	}
	accept := r.accept
	if accept == nil {
		accept = rtmp.Accept
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &rtmpListener{
		reg: r, key: key, ln: ln, cfg: cfg, pol: pol.withDefaults(), accept: accept, now: now,
		ctx: ctx, cancel: cancel, routes: map[string]*rtmpRoute{}, perIP: map[string]int{},
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

// unregister removes path from l. The last route removes l and closes its
// TCP listener under r.mu, so a register racing it binds the port afresh,
// then waits outside the lock for the accept loop and every session.
func (r *rtmpRegistry) unregister(l *rtmpListener, path string) {
	r.mu.Lock()
	delete(l.routes, path)
	last := len(l.routes) == 0
	if last {
		delete(r.listeners, l.key)
		_ = l.ln.Close()
	}
	r.mu.Unlock()
	if last {
		l.cancel()
		l.wg.Wait()
	}
}

// lookup returns the listener on addr, or nil.
func (r *rtmpRegistry) lookup(addr string) *rtmpListener {
	key, err := rtmpAddrKey(addr)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listeners[key]
}

// stats returns the counters of the listener on addr, or nil when there is
// none.
func (r *rtmpRegistry) stats(addr string) map[string]uint64 {
	l := r.lookup(addr)
	if l == nil {
		return nil
	}
	return l.counters()
}

// counters returns the listener's rejection counters.
func (l *rtmpListener) counters() map[string]uint64 {
	return map[string]uint64{
		"rejected_unknown": l.rejectedUnknown.Load(),
		"rejected_busy":    l.rejectedBusy.Load(),
		"rejected_flood":   l.rejectedFlood.Load(),
	}
}

// rtmpAddrKey normalises a listen address to one key per bind: the port is
// written as a number (1..65535); an empty host and every unspecified IP
// address (0.0.0.0, [::], their other spellings) become ":port"; any other
// IP address is written canonically, IPv4-mapped IPv6 as IPv4; a host name
// is kept.
func rtmpAddrKey(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("ingest: rtmp: listen address %q: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("ingest: rtmp: listen address %q: bad port", addr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
		if ip.Unmap().IsUnspecified() {
			host = ""
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// redactRTMPPath masks the stream name, the last path segment, keeping its
// first 2 characters.
func redactRTMPPath(path string) string {
	i := strings.LastIndexByte(path, '/')
	name := []rune(path[i+1:])
	if len(name) > 2 {
		name = name[:2]
	} else {
		name = nil
	}
	return path[:i+1] + string(name) + "xxxxx"
}

// acceptLoop accepts TCP connections until the listener closes, closing at
// once those over the pre-publish caps.
func (l *rtmpListener) acceptLoop() {
	defer l.wg.Done()
	var backoff time.Duration
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), maxAcceptBackoff)
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-l.ctx.Done():
				t.Stop()
				return
			}
			continue
		}
		backoff = 0
		ip := peerKey(c.RemoteAddr())
		l.mu.Lock()
		if l.pending >= l.pol.MaxPrePublish || l.perIP[ip] >= l.pol.MaxPrePublishPerIP {
			l.mu.Unlock()
			l.rejectedFlood.Add(1)
			_ = c.Close()
			continue
		}
		l.pending++
		l.perIP[ip]++
		l.mu.Unlock()
		l.wg.Add(1) // the loop's own count keeps wg above zero here
		go l.serve(c, ip)
	}
}

// peerKey is the key the per-IP pre-publish count uses for a remote address:
// an IPv4 address as is (an IPv4-mapped IPv6 address unmapped), and an IPv6
// address by its /64, which one host normally owns. An address that is not
// an IP falls back to its host part, or its whole string.
func peerKey(a net.Addr) string {
	var ip netip.Addr
	if t, ok := a.(*net.TCPAddr); ok {
		ip, _ = netip.AddrFromSlice(t.IP)
	} else if host, _, err := net.SplitHostPort(a.String()); err == nil {
		ip, _ = netip.ParseAddr(host)
		if !ip.IsValid() {
			return host
		}
	}
	if !ip.IsValid() {
		return a.String()
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip, 64).Masked().String()
}

// serve runs the publish exchange on c and offers the publisher to the sink
// of its path.
func (l *rtmpListener) serve(c net.Conn, ip string) {
	defer l.wg.Done()
	var rt *rtmpRoute
	sc, err := l.accept(l.ctx, c, l.cfg, func(req rtmp.PublishRequest) error {
		var err error
		rt, err = l.reserve(req)
		return err
	})
	if err != nil {
		if rt != nil {
			l.release(rt) // decide accepted, then the replies or ctx failed
		}
		l.leave(ip)
		return
	}
	l.leave(ip)
	if l.ctx.Err() != nil || !rt.sink.offer(sc) {
		_ = sc.Close()
	}
	l.release(rt)
}

// leave frees the pre-publish slot of a session from ip.
func (l *rtmpListener) leave(ip string) {
	l.mu.Lock()
	l.pending--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
	l.mu.Unlock()
}

// reserve is the publish decision: it finds the route of req and reserves
// it, or rejects an empty app or stream, an unknown path and a busy one with
// the same error. The route stays reserved until release, so two publishers
// cannot both pass before either is offered.
func (l *rtmpListener) reserve(req rtmp.PublishRequest) (*rtmpRoute, error) {
	path, _ := rtmp.NormalizePath(req.App, req.Stream)
	app, _ := rtmp.NormalizePath(req.App, "")
	stream, _ := rtmp.NormalizePath(req.Stream, "")
	if app == "" || stream == "" {
		l.rejectedUnknown.Add(1)
		return nil, errStreamNotAvailable
	}
	l.reg.mu.Lock()
	rt := l.routes[path]
	switch {
	case rt == nil:
		l.reg.mu.Unlock()
		l.rejectedUnknown.Add(1)
		return nil, errStreamNotAvailable
	case rt.reserved:
		l.reg.mu.Unlock()
		l.rejectedBusy.Add(1)
		return nil, errStreamNotAvailable
	}
	rt.reserved = true
	l.reg.mu.Unlock()
	if rt.sink.busy(l.now()) { // outside the lock: the sink takes its own
		l.release(rt)
		l.rejectedBusy.Add(1)
		return nil, errStreamNotAvailable
	}
	return rt, nil
}

// release ends the reservation of rt.
func (l *rtmpListener) release(rt *rtmpRoute) {
	l.reg.mu.Lock()
	rt.reserved = false
	l.reg.mu.Unlock()
}

// prePublish is the number of sessions that have not published.
func (l *rtmpListener) prePublish() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending
}
