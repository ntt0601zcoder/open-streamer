// Package rtmpsrv shares one RTMP TCP listener per address between the
// inputs that take publishers on it and the outputs that serve players from
// it. A publisher is routed by its path, which is its stream key and stays
// secret; a player by the app its handler registered, with the rest of its
// path as the stream name. Every rejection reads the same to the client.
package rtmpsrv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// Default listener policy.
const (
	defaultMaxPrePublish       = 64
	defaultMaxPrePublishPerIP  = 4
	defaultMaxPlayersPerStream = 256
	defaultMaxPlayers          = 1024
	defaultMaxPlayersPerIP     = 64
	// maxAcceptBackoff caps the pause after a failing TCP accept (for
	// example out of file descriptors).
	maxAcceptBackoff = time.Second
)

// errStreamNotAvailable rejects every request the registry turns away: an
// unknown or busy publish path, an unknown play, a publish path played, a
// stream that is not live and every cap, so a client cannot probe which
// stream keys exist.
var errStreamNotAvailable = fmt.Errorf("rtmpsrv: %w", rtmp.ErrStreamNotAvailable)

// PublishSink receives the publishers of one listen path.
//
// Offer and Busy may still be called after a non-last unregister of the
// sink's route has returned: a session that reserved the route before the
// unregister goes on to offer its conn, possibly to a sink that has since
// closed. A closed sink must therefore refuse offers. The sink must never
// call unregister while holding a lock that Offer or Busy takes, since
// unregister may wait for a session that is calling them.
type PublishSink interface {
	// Offer hands over a publisher whose publish was accepted. It returns
	// false when the sink is closed; the registry then closes conn. It must
	// not block for long: unregistering waits for it.
	Offer(conn *rtmp.ServerConn) bool
	// Busy reports whether the sink has a live publisher that sent within
	// the last 2 s, in which case a new publisher is rejected.
	Busy(now time.Time) bool
}

// PlayHandler serves the players of one app. A player's request names the
// app exactly (rtmp://host/<app>/<name>); the stream name is the rest,
// conn.Request().Stream. Neither method is called after the handler's
// unregister has returned, and the handler must never call unregister while
// holding a lock that Live or Serve needs, since unregister waits for them.
type PlayHandler interface {
	// Live reports whether name can be played now; false rejects the
	// player. It is part of the play decision and must not block for long:
	// the session waits for it, and so does unregistering.
	Live(name string) bool
	// Serve plays the stream to conn, in the session's goroutine. It must
	// return once ctx ends or conn.Done closes; the registry then closes
	// conn. ctx ends when the handler is unregistered.
	Serve(ctx context.Context, conn *rtmp.ServerConn)
}

// Policy bounds the sessions of one TCP listener. MaxPrePublish and
// MaxPrePublishPerIP bound the sessions that have neither published nor
// started to play (default 64 and 4); MaxPlayersPerStream, MaxPlayers and
// MaxPlayersPerIP bound the players of one stream, of the listener and of
// one peer (default 256, 1024 and 64). A zero field takes its default. A
// per-IP bound counts an IPv6 peer by its /64.
type Policy struct {
	MaxPrePublish, MaxPrePublishPerIP                int
	MaxPlayersPerStream, MaxPlayers, MaxPlayersPerIP int
}

func (p Policy) withDefaults() Policy {
	for _, f := range []struct {
		v   *int
		def int
	}{
		{&p.MaxPrePublish, defaultMaxPrePublish},
		{&p.MaxPrePublishPerIP, defaultMaxPrePublishPerIP},
		{&p.MaxPlayersPerStream, defaultMaxPlayersPerStream},
		{&p.MaxPlayers, defaultMaxPlayers},
		{&p.MaxPlayersPerIP, defaultMaxPlayersPerIP},
	} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	return p
}

// acceptFunc is rtmp.Accept; tests replace it.
type acceptFunc func(ctx context.Context, c net.Conn, cfg rtmp.Config, decide func(rtmp.Request) error) (*rtmp.ServerConn, error)

// Registry shares one TCP listener per address between every publish route
// and play handler on it. The zero value is ready to use; ConnHook is set
// before the first register.
type Registry struct {
	// ConnHook, when set, is called with every connection a listener takes
	// a session for, before the handshake, in the session's goroutine.
	// Tests use it to shrink socket buffers.
	ConnHook func(net.Conn)

	accept acceptFunc // nil means rtmp.Accept

	// mu guards listeners, seq and every listener's routes and plays. It is
	// never held while calling a sink or a handler, nor while taking a
	// listener's mu.
	mu        sync.Mutex
	listeners map[string]*listener
	seq       uint64 // the last listener id
}

// Default is the process-wide registry.
var Default = &Registry{}

// route is a registered publish path.
type route struct {
	sink     PublishSink
	reserved bool // a publisher passed decide and is not offered yet; registry mu
}

// playRoute is a registered play handler.
type playRoute struct {
	h      PlayHandler
	ctx    context.Context // Serve's; ends at unregister
	cancel context.CancelFunc
	// wg counts the sessions that found the route in their decision, until
	// they end; unregister waits for it.
	wg sync.WaitGroup

	closed bool                          // unregistered; registry mu
	conns  map[*rtmp.ServerConn]struct{} // being served; registry mu
}

// playSlot is a player's admission: its route, and the slots it holds in
// the player counts.
type playSlot struct {
	rt       *playRoute
	path, ip string
	stop     func() bool // stops closing the raw conn when rt ends
}

// listener is one TCP listener and its sessions.
type listener struct {
	reg    *Registry
	key    string
	ln     net.Listener
	cfg    rtmp.Config
	pol    Policy
	accept acceptFunc
	now    func() time.Time
	ctx    context.Context // ends every session's Accept on shutdown
	cancel context.CancelFunc
	wg     sync.WaitGroup // the accept loop and every session

	id     uint64                // its number in the registry, from 1
	routes map[string]*route     // publish routes by path; registry mu
	plays  map[string]*playRoute // play handlers by app; registry mu

	mu      sync.Mutex     // guards the counts below
	pending int            // sessions that have neither published nor started to play
	perIP   map[string]int // pending by peerKey: an IPv4 address or an IPv6 /64
	players int
	streams map[string]int // players by play path
	ipPlay  map[string]int // players by peerKey

	rejectedUnknown, rejectedBusy, rejectedFlood                   atomic.Uint64
	rejectedPlayUnknown, rejectedPlayPublishPath, rejectedPlayCaps atomic.Uint64
}

// RegisterPublish routes publishers of path on addr to sink. The first
// register on an address, publish or play, opens its TCP listener with cfg
// and pol; later ones share it and their cfg and pol are ignored. The
// returned unregister removes the route; the last one on the address closes
// the listener and waits for its sessions to end. It is idempotent.
// unregister must not be called while holding a lock that the sink's Offer
// or Busy takes (see PublishSink).
func (r *Registry) RegisterPublish(addr, path string, sink PublishSink, cfg rtmp.Config, pol Policy) (unregister func(), err error) {
	key, err := AddrKey(addr)
	if err != nil {
		return nil, err
	}
	path, _ = rtmp.NormalizePath(path, "")
	if app, stream, ok := strings.Cut(path, "/"); !ok || app == "" || stream == "" {
		return nil, fmt.Errorf("rtmpsrv: listen %s: path %q needs an app and a stream name", key, redact.RTMPKey(path))
	}
	l, err := r.attach(key, cfg, pol, func(l *listener) error {
		if _, dup := l.routes[path]; dup {
			return fmt.Errorf("rtmpsrv: listen %s: path %s is already registered", key, redact.RTMPKey(path))
		}
		l.routes[path] = &route{sink: sink}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.detach(l, func() func() {
				delete(l.routes, path)
				return nil
			})
		})
	}, nil
}

// RegisterPlay hands the players of app on addr to h: a play whose app is
// app (rtmp://host/<app>/<name>) reaches h with the stream name <name>. app
// is the only source of the play app. A play whose path is a registered
// publish path is rejected before h is asked, so a publish key is never
// playable. The listener is shared as with RegisterPublish. The returned
// unregister cancels the ctx of every Serve, closes the players, waits for
// every session that reached h, and closes the listener when it was the
// address's last registration. It is idempotent.
func (r *Registry) RegisterPlay(addr, app string, h PlayHandler, cfg rtmp.Config, pol Policy) (unregister func(), err error) {
	key, err := AddrKey(addr)
	if err != nil {
		return nil, err
	}
	app, _ = rtmp.NormalizePath(app, "")
	if app == "" {
		return nil, fmt.Errorf("rtmpsrv: listen %s: play needs an app", key)
	}
	var pr *playRoute
	l, err := r.attach(key, cfg, pol, func(l *listener) error {
		if _, dup := l.plays[app]; dup {
			return fmt.Errorf("rtmpsrv: listen %s: play app %q is already registered", key, app)
		}
		ctx, cancel := context.WithCancel(l.ctx)
		pr = &playRoute{h: h, ctx: ctx, cancel: cancel, conns: map[*rtmp.ServerConn]struct{}{}}
		l.plays[app] = pr
		return nil
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.detach(l, func() func() {
				delete(l.plays, app)
				pr.closed = true
				conns := make([]*rtmp.ServerConn, 0, len(pr.conns))
				for c := range pr.conns {
					conns = append(conns, c)
				}
				return func() {
					pr.cancel()
					for _, c := range conns {
						_ = c.Close()
					}
					pr.wg.Wait()
				}
			})
		})
	}, nil
}

// attach runs add on the listener for key, opening it with cfg and pol when
// there is none. A listener opened for an add that fails is closed again.
func (r *Registry) attach(key string, cfg rtmp.Config, pol Policy, add func(*listener) error) (*listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.listeners[key]; l != nil {
		if err := add(l); err != nil {
			return nil, err
		}
		return l, nil
	}
	l, err := r.bind(key, cfg, pol)
	if err != nil {
		return nil, err
	}
	if err := add(l); err != nil {
		l.cancel()
		_ = l.ln.Close()
		return nil, err
	}
	if r.listeners == nil {
		r.listeners = map[string]*listener{}
	}
	r.listeners[key] = l
	l.wg.Add(1)
	go l.acceptLoop()
	return l, nil
}

// bind opens the TCP listener for key; attach starts its accept loop. r.mu
// is held.
func (r *Registry) bind(key string, cfg rtmp.Config, pol Policy) (*listener, error) {
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", key)
	if err != nil {
		return nil, fmt.Errorf("rtmpsrv: listen %s: %w", key, err)
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
	r.seq++
	return &listener{
		reg: r, key: key, id: r.seq, ln: ln, cfg: cfg, pol: pol.withDefaults(), accept: accept, now: now,
		ctx: ctx, cancel: cancel, routes: map[string]*route{}, plays: map[string]*playRoute{},
		perIP: map[string]int{}, streams: map[string]int{}, ipPlay: map[string]int{},
	}, nil
}

// detach runs remove under r.mu, then the function it returns, if any,
// outside it. The last registration removes l and closes its TCP listener
// under r.mu, so a register racing it binds the port afresh, then waits
// outside the lock for the accept loop and every session.
func (r *Registry) detach(l *listener, remove func() (after func())) {
	r.mu.Lock()
	after := remove()
	last := len(l.routes) == 0 && len(l.plays) == 0
	if last {
		delete(r.listeners, l.key)
		_ = l.ln.Close()
	}
	r.mu.Unlock()
	if after != nil {
		after()
	}
	if last {
		l.cancel()
		l.wg.Wait()
	}
}

// lookup returns the listener on addr, or nil.
func (r *Registry) lookup(addr string) *listener {
	key, err := AddrKey(addr)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listeners[key]
}

// Stats describes the listener on one address.
type Stats struct {
	// ID is the listener's number in its registry, from 1, so a test can
	// tell a listener from its successor; 0 when nothing listens.
	ID uint64
	// Routes is the number of publish paths registered, Apps the number of
	// play handlers.
	Routes, Apps int
	// PrePublish is the number of sessions that have neither published nor
	// started to play.
	PrePublish int
	// RejectedUnknown counts publishes to an unknown path, RejectedBusy
	// publishes to a path whose publisher is live, and RejectedFlood
	// connections closed at accept over the pre-publish caps.
	RejectedUnknown, RejectedBusy, RejectedFlood uint64
	// RejectedPlayUnknown counts plays of an app no handler serves or of a
	// stream that is not live, RejectedPlayPublishPath plays of a publish
	// path, and RejectedPlayCaps plays over a player cap.
	RejectedPlayUnknown, RejectedPlayPublishPath, RejectedPlayCaps uint64
	// Players is the number of players admitted and not ended;
	// StreamPlayers counts them by play path (<app>/<name>).
	Players       int
	StreamPlayers map[string]int
}

// Counters returns the rejection counters by their stats names.
func (s Stats) Counters() map[string]uint64 {
	return map[string]uint64{
		"rejected_unknown":           s.RejectedUnknown,
		"rejected_busy":              s.RejectedBusy,
		"rejected_flood":             s.RejectedFlood,
		"rejected_play_unknown":      s.RejectedPlayUnknown,
		"rejected_play_publish_path": s.RejectedPlayPublishPath,
		"rejected_play_caps":         s.RejectedPlayCaps,
	}
}

// Stats returns the stats of the listener on addr; the zero Stats when
// nothing listens there.
func (r *Registry) Stats(addr string) Stats {
	l := r.lookup(addr)
	if l == nil {
		return Stats{}
	}
	r.mu.Lock()
	st := Stats{ID: l.id, Routes: len(l.routes), Apps: len(l.plays)}
	r.mu.Unlock()
	l.mu.Lock()
	st.PrePublish = l.pending
	st.Players = l.players
	st.StreamPlayers = maps.Clone(l.streams)
	l.mu.Unlock()
	st.RejectedUnknown = l.rejectedUnknown.Load()
	st.RejectedBusy = l.rejectedBusy.Load()
	st.RejectedFlood = l.rejectedFlood.Load()
	st.RejectedPlayUnknown = l.rejectedPlayUnknown.Load()
	st.RejectedPlayPublishPath = l.rejectedPlayPublishPath.Load()
	st.RejectedPlayCaps = l.rejectedPlayCaps.Load()
	return st
}

// AddrKey normalises a listen address to one key per bind: the port is
// written as a number (1..65535); an empty host and every unspecified IP
// address (0.0.0.0, [::], their other spellings) become ":port"; any other
// IP address is written canonically, IPv4-mapped IPv6 as IPv4; a host name
// is kept.
func AddrKey(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("rtmpsrv: listen address %q: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("rtmpsrv: listen address %q: bad port", addr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
		if ip.Unmap().IsUnspecified() {
			host = ""
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// acceptLoop accepts TCP connections until the listener closes, closing at
// once those over the pre-publish caps.
func (l *listener) acceptLoop() {
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

// serve runs the command exchange on c, then offers a publisher to the
// sink of its path or plays a player through the handler of its app.
func (l *listener) serve(c net.Conn, ip string) {
	defer l.wg.Done()
	if hook := l.reg.ConnHook; hook != nil {
		hook(c)
	}
	var (
		rt   *route
		slot *playSlot
	)
	sc, err := l.accept(l.ctx, c, l.cfg, func(req rtmp.Request) error {
		var err error
		if req.Play {
			slot, err = l.admit(c, req, ip)
		} else {
			rt, err = l.reserve(req)
		}
		return err
	})
	l.leave(ip)
	if err != nil {
		// decide may have accepted, then the replies or ctx failed.
		if rt != nil {
			l.release(rt)
		}
		if slot != nil {
			l.endPlay(slot)
		}
		return
	}
	if slot != nil {
		l.play(slot, sc)
		return
	}
	if l.ctx.Err() != nil || !rt.sink.Offer(sc) {
		_ = sc.Close()
	}
	l.release(rt)
}

// admit is the play decision. It rejects, all with the same error, a play
// of a publish path (before anything else, so the key reaches no handler),
// of an app with no handler or an empty name, of a stream that is not Live,
// and one over a player cap. An admitted player holds its route (the
// route's wg) and its player slots until endPlay, and its raw conn is
// closed if the route is unregistered meanwhile.
func (l *listener) admit(c net.Conn, req rtmp.Request, ip string) (*playSlot, error) {
	path, _ := rtmp.NormalizePath(req.App, req.Stream)
	app, _ := rtmp.NormalizePath(req.App, "")
	name, _ := rtmp.NormalizePath(req.Stream, "")
	l.reg.mu.Lock()
	if _, ok := l.routes[path]; ok {
		l.reg.mu.Unlock()
		l.rejectedPlayPublishPath.Add(1)
		return nil, errStreamNotAvailable
	}
	rt := l.plays[app]
	if rt == nil || name == "" {
		l.reg.mu.Unlock()
		l.rejectedPlayUnknown.Add(1)
		return nil, errStreamNotAvailable
	}
	rt.wg.Add(1) // before unregister's Wait: it marks rt closed under this lock
	l.reg.mu.Unlock()
	stop := context.AfterFunc(rt.ctx, func() { _ = c.Close() })
	if !rt.h.Live(name) { // outside the lock: the handler takes its own
		stop()
		rt.wg.Done()
		l.rejectedPlayUnknown.Add(1)
		return nil, errStreamNotAvailable
	}
	l.mu.Lock()
	if l.players >= l.pol.MaxPlayers || l.streams[path] >= l.pol.MaxPlayersPerStream ||
		l.ipPlay[ip] >= l.pol.MaxPlayersPerIP {
		l.mu.Unlock()
		stop()
		rt.wg.Done()
		l.rejectedPlayCaps.Add(1)
		return nil, errStreamNotAvailable
	}
	l.players++
	l.streams[path]++
	l.ipPlay[ip]++
	l.mu.Unlock()
	return &playSlot{rt: rt, path: path, ip: ip, stop: stop}, nil
}

// play serves an admitted player until Serve returns, unless its route was
// unregistered meanwhile, then closes it and frees its slots.
func (l *listener) play(s *playSlot, sc *rtmp.ServerConn) {
	defer l.endPlay(s)
	rt := s.rt
	s.stop() // the route's unregister now closes sc instead
	l.reg.mu.Lock()
	if rt.closed {
		l.reg.mu.Unlock()
		_ = sc.Close()
		return
	}
	rt.conns[sc] = struct{}{}
	l.reg.mu.Unlock()
	rt.h.Serve(rt.ctx, sc)
	_ = sc.Close()
	l.reg.mu.Lock()
	delete(rt.conns, sc)
	l.reg.mu.Unlock()
}

// endPlay frees the player slots of s and releases its route.
func (l *listener) endPlay(s *playSlot) {
	s.stop()
	l.mu.Lock()
	l.players--
	if l.streams[s.path]--; l.streams[s.path] <= 0 {
		delete(l.streams, s.path)
	}
	if l.ipPlay[s.ip]--; l.ipPlay[s.ip] <= 0 {
		delete(l.ipPlay, s.ip)
	}
	l.mu.Unlock()
	s.rt.wg.Done()
}

// leave frees the pre-publish slot of a session from ip.
func (l *listener) leave(ip string) {
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
func (l *listener) reserve(req rtmp.Request) (*route, error) {
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
	if rt.sink.Busy(l.now()) { // outside the lock: the sink takes its own
		l.release(rt)
		l.rejectedBusy.Add(1)
		return nil, errStreamNotAvailable
	}
	return rt, nil
}

// release ends the reservation of rt.
func (l *listener) release(rt *route) {
	l.reg.mu.Lock()
	rt.reserved = false
	l.reg.mu.Unlock()
}
