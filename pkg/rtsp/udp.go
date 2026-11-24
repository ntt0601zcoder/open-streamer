package rtsp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ErrNotUDP reports that the server answered a UDP SETUP with a transport
// other than UDP, such as TCP interleaved.
var ErrNotUDP = errors.New("rtsp: setup: server did not choose UDP")

// pairAttempts bounds the tries to bind an even/odd UDP port pair.
const pairAttempts = 64

// maxDatagram is the read buffer of a UDP socket; it also bounds an RTP
// packet to 64 KiB.
const maxDatagram = 64 << 10

// udpReadBuffer is the kernel receive buffer the client asks for on its
// UDP sockets, so that a burst does not overflow it.
const udpReadBuffer = 4 << 20

// udpDrainWait bounds how long each UDP socket reader keeps reading after
// the control connection ended, and udpDrainIdle is how long a socket must
// stay quiet to count as dry. A server sends its last RTP and its BYEs
// before it closes the connection, so on loopback and on a LAN they are
// already queued on the sockets when the EOF arrives, and are read at once;
// over a WAN they can trail the EOF by the jitter between the two paths,
// usually a few milliseconds. 100 ms covers that with room while keeping
// the end of a session, and a failover it starts, prompt; 20 ms of quiet
// ends the drain early in the common case.
const (
	udpDrainWait = 100 * time.Millisecond
	udpDrainIdle = 20 * time.Millisecond
)

// udpLink is the UDP transport of a track, fixed once SETUP returns.
type udpLink struct {
	rtp, rtcp *net.UDPConn
	srvIP     net.IP
	// srvPorts are the server's RTP and RTCP ports; zero when the SETUP
	// reply named none, in which case any port of srvIP is accepted and no
	// punch or receiver report is sent.
	srvPorts [2]int
}

// accepts reports whether a datagram from a comes from the server.
func (l *udpLink) accepts(a *net.UDPAddr) bool {
	if a == nil || !a.IP.Equal(l.srvIP) {
		return false
	}
	return l.srvPorts[0] == 0 || a.Port == l.srvPorts[0] || a.Port == l.srvPorts[1]
}

func (l *udpLink) srvAddr(i int) *net.UDPAddr {
	return &net.UDPAddr{IP: l.srvIP, Port: l.srvPorts[i]}
}

// ListenUDPPair binds an even RTP port and the odd RTCP port after it on
// host (an IP address or a name; "" means every interface), retrying on
// conflicts.
func ListenUDPPair(host string) (rtp, rtcp *net.UDPConn, err error) {
	rtp, rtcp, err = listenUDPPair(host)
	if err != nil {
		return nil, nil, fmt.Errorf("rtsp: listen udp pair: %w", err)
	}
	return rtp, rtcp, nil
}

// listenUDPPair is ListenUDPPair with its errors unwrapped, for callers
// that add their own prefix.
func listenUDPPair(host string) (*net.UDPConn, *net.UDPConn, error) {
	a, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, nil, err
	}
	return listenPair(context.Background(), a.IP, a.Zone)
}

// listenPair binds an even RTP port and the odd RTCP port after it on ip,
// retrying on conflicts.
func listenPair(ctx context.Context, ip net.IP, zone string) (*net.UDPConn, *net.UDPConn, error) {
	bind := func(port int) (*net.UDPConn, error) {
		pc, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", (&net.UDPAddr{IP: ip, Port: port, Zone: zone}).String())
		if err != nil {
			return nil, err
		}
		return pc.(*net.UDPConn), nil
	}
	for range pairAttempts {
		a, err := bind(0)
		if err != nil {
			return nil, nil, err
		}
		port := a.LocalAddr().(*net.UDPAddr).Port
		if port%2 == 1 {
			// a is a free RTCP port; try the even port before it.
			if b, err := bind(port - 1); err == nil {
				return b, a, nil
			}
		} else if b, err := bind(port + 1); err == nil {
			return a, b, nil
		}
		_ = a.Close()
	}
	return nil, nil, errors.New("no free port pair")
}

// addSockets registers sockets to be closed when the session ends; if it
// has already ended they are closed at once.
func (c *Client) addSockets(socks ...*net.UDPConn) error {
	c.sockMu.Lock()
	defer c.sockMu.Unlock()
	if c.socksClosed {
		for _, s := range socks {
			_ = s.Close()
		}
		return net.ErrClosed
	}
	c.socks = append(c.socks, socks...)
	return nil
}

func (c *Client) closeSockets() {
	c.sockMu.Lock()
	defer c.sockMu.Unlock()
	c.socksClosed = true
	for _, s := range c.socks {
		_ = s.Close()
	}
	c.socks = nil
}

// setupUDP sets up one media over UDP: a socket pair on the local address
// of the control connection, SETUP with its ports, a punch packet to each
// server port and a reader goroutine per socket.
func (c *Client) setupUDP(ctx context.Context, m Media) error {
	local, ok1 := c.conn.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.conn.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return errors.New("rtsp: setup: UDP needs a TCP control connection")
	}
	rtpC, rtcpC, err := listenPair(ctx, local.IP, local.Zone)
	if err != nil {
		return fmt.Errorf("rtsp: setup: listen udp: %w", err)
	}
	if err := c.addSockets(rtpC, rtcpC); err != nil {
		return fmt.Errorf("rtsp: setup: %w", err)
	}
	// Best effort: net.core.rmem_max may cap it.
	_ = rtpC.SetReadBuffer(udpReadBuffer)
	_ = rtcpC.SetReadBuffer(udpReadBuffer)
	port := rtpC.LocalAddr().(*net.UDPAddr).Port
	tr := Transport{Protocol: "RTP/AVP", Unicast: true, ClientPort: [2]int{port, port + 1}}
	resp, err := c.request(ctx, "SETUP", m.Control, Header{"Transport": {tr.String()}})
	if err != nil {
		return err
	}
	if !ok2xx(resp) {
		return &StatusError{Method: "SETUP", Code: resp.StatusCode}
	}
	if err := c.adoptSession(resp); err != nil {
		return err
	}
	rt, err := ParseTransport(resp.Header.Get("Transport"))
	if err != nil {
		return fmt.Errorf("rtsp: setup: %w", err)
	}
	if p := strings.ToUpper(rt.Protocol); p != "RTP/AVP" && p != "RTP/AVP/UDP" || rt.Interleaved != [2]int{} {
		return ErrNotUDP
	}
	link := &udpLink{rtp: rtpC, rtcp: rtcpC, srvIP: remote.IP, srvPorts: rt.ServerPort}
	if ip := net.ParseIP(rt.Source); ip != nil {
		link.srvIP = ip
	}
	t := &clientTrack{index: m.Index, link: link}
	c.tracks = append(c.tracks, t)
	if link.srvPorts[0] != 0 {
		// Punches open NAT and firewall state toward the server; they are
		// best effort.
		_ = c.writeUDP(link.rtp, link.srvAddr(0), nil)
		_ = c.writeUDP(link.rtcp, link.srvAddr(1), nil)
	}
	// Under sockMu, so that a drain begun meanwhile (the server closed the
	// connection right after this SETUP) never waits while readers start.
	c.sockMu.Lock()
	if c.draining.Load() {
		c.sockMu.Unlock()
		return fmt.Errorf("rtsp: setup: %w", net.ErrClosed)
	}
	c.wg.Add(2)
	c.sockWG.Add(2)
	c.sockMu.Unlock()
	go c.readSocket(t, rtpC, false)
	go c.readSocket(t, rtcpC, true)
	return nil
}

// writeUDP sends one datagram under the client's write mutex.
func (c *Client) writeUDP(conn *net.UDPConn, to *net.UDPAddr, b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = conn.SetWriteDeadline(c.clampDeadline(time.Now().Add(c.cfg.Timeout)))
	if _, err := conn.WriteToUDP(b, to); err != nil {
		return fmt.Errorf("rtsp: write rtcp: %w", err)
	}
	return nil
}

// readSocket reads one UDP socket of a track until the session ends.
// Datagrams from anyone but the server are ignored. Once drainUDP wakes it,
// it reads what is queued and returns when the socket has been quiet for
// udpDrainIdle, after udpDrainWait, or when the socket is closed. Each read
// of the drain sets its own deadline just before it, so a reader that runs
// late still reads what is queued.
func (c *Client) readSocket(t *clientTrack, conn *net.UDPConn, rtcp bool) {
	defer c.wg.Done()
	defer c.sockWG.Done()
	buf := make([]byte, maxDatagram)
	var draining bool
	var until, idle time.Time
	for {
		if draining {
			if !time.Now().Before(until) {
				return
			}
			idle = time.Now().Add(udpDrainIdle)
			if idle.After(until) {
				idle = until
			}
			_ = conn.SetReadDeadline(idle)
		}
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			switch {
			case draining:
				// A deadline drainUDP set late can end a read early: only
				// a quiet udpDrainIdle ends the drain.
				if isTimeout(err) && time.Now().Before(idle) {
					continue
				}
				return
			case c.draining.Load():
				if errors.Is(err, net.ErrClosed) {
					return // Close cut the drain short
				}
				draining, until = true, time.Now().Add(udpDrainWait)
				continue
			}
			select {
			case <-c.done:
			default:
				c.fail(fmt.Errorf("rtsp: read udp: %w", err))
			}
			return
		}
		if n == 0 || !t.link.accepts(from) {
			continue
		}
		data := append([]byte(nil), buf[:n]...)
		arrival := c.cfg.Now()
		if rtcp {
			c.onRTCP(t, data)
			c.queueUDP(Packet{Track: t.index, RTCP: data, Arrival: arrival})
			continue
		}
		p, ok := parseRTP(data)
		if !ok {
			c.nDropped.Add(1)
			continue
		}
		c.onRTP(t, p)
		c.touch()
		c.firstOnce.Do(func() { close(c.firstRTP) })
		c.queueUDP(Packet{Track: t.index, RTP: p, Arrival: arrival})
	}
}

// drainUDP reads the UDP sockets dry after the control connection ended
// on its own: the server sent its last packets and BYEs before it closed
// the connection, but the EOF can overtake them while they wait on the
// sockets, and closing the sockets would discard them. It wakes every
// socket reader into its drain (see readSocket) and waits for them, so
// that the packets are queued before the session ends; Close and any
// failure still close the sockets at once and cut the drain short.
func (c *Client) drainUDP() {
	c.sockMu.Lock()
	c.draining.Store(true)
	now := time.Now()
	for _, s := range c.socks {
		_ = s.SetReadDeadline(now)
	}
	c.sockMu.Unlock()
	c.sockWG.Wait()
}

// queueUDP hands a packet to ReadPacket, dropping it (counted) when the
// queue is full.
func (c *Client) queueUDP(p Packet) {
	select {
	case c.pkts <- p:
	default:
		c.nDropped.Add(1)
	}
}

// initReorder creates the reorder buffers once the session plays, seeded
// with the RTP-Info sequence numbers.
func (c *Client) initReorder() {
	c.ro = make(map[int]*reorder, len(c.tracks))
	for _, t := range c.tracks {
		c.ro[t.index] = &reorder{}
	}
	for i, m := range c.desc.Media {
		if r := c.ro[m.Index]; r != nil && i < len(c.info) && c.info[i].HasSeq {
			r.hint, r.hasHint = c.info[i].Seq, true
		}
	}
}

// readUDP is ReadPacket over UDP: packets go through the reorder buffers,
// and the wait for a new packet ends when the oldest buffered one is due.
func (c *Client) readUDP() (Packet, error) {
	for {
		now := time.Now()
		if p, ok := c.release(now); ok {
			return p, nil
		}
		var wake <-chan time.Time
		if d, ok := c.nextDeadline(); ok {
			if c.timer == nil {
				c.timer = time.NewTimer(d.Sub(now))
			} else {
				c.timer.Reset(d.Sub(now))
			}
			wake = c.timer.C
		}
		select {
		case p := <-c.pkts:
			c.accept(p, time.Now())
		case <-wake:
		case <-c.done:
			return c.drainEnded()
		}
	}
}

// drainEnded returns what is left after the session ended: queued packets
// and the buffers flushed in order, then the error that ended it.
func (c *Client) drainEnded() (Packet, error) {
	for more := true; more; {
		select {
		case p := <-c.pkts:
			c.accept(p, time.Now())
		default:
			more = false
		}
	}
	for _, t := range c.tracks {
		c.ro[t.index].flush(&c.out)
	}
	if p, ok := c.popOut(); ok && !c.closed.Load() {
		return p, nil
	}
	return Packet{}, c.err
}

// accept takes a packet from the queue: RTCP passes through, RTP enters its
// track's reorder buffer.
func (c *Client) accept(p Packet, now time.Time) {
	r := c.ro[p.Track]
	if p.RTP == nil || r == nil {
		c.out = append(c.out, p)
		return
	}
	switch r.push(p, now, &c.out) {
	case pushQueued:
	case pushReordered:
		c.nReordered.Add(1)
	case pushDup:
		c.nDup.Add(1)
	case pushLate:
		c.nDropped.Add(1)
	}
}

func (c *Client) popOut() (Packet, bool) {
	if len(c.out) == 0 {
		return Packet{}, false
	}
	p := c.out[0]
	c.out[0] = Packet{}
	c.out = c.out[1:]
	if len(c.out) == 0 {
		c.out = nil
	}
	return p, true
}

// release returns the next packet that is ready.
func (c *Client) release(now time.Time) (Packet, bool) {
	if p, ok := c.popOut(); ok {
		return p, true
	}
	for _, t := range c.tracks {
		if p, ok := c.ro[t.index].pop(now); ok {
			return p, true
		}
	}
	return Packet{}, false
}

// nextDeadline returns the earliest time a buffered packet becomes due.
func (c *Client) nextDeadline() (time.Time, bool) {
	var first time.Time
	have := false
	for _, t := range c.tracks {
		if d, ok := c.ro[t.index].deadline(); ok && (!have || d.Before(first)) {
			first, have = d, true
		}
	}
	return first, have
}

// awaitRTP waits up to d for the first RTP packet; false means none came,
// the session ended or ctx ended.
func (c *Client) awaitRTP(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.firstRTP:
		return true
	case <-t.C:
	case <-ctx.Done():
	case <-c.done:
	}
	return false
}

// dialAuto plays over UDP and falls back to a new session over TCP when the
// server refuses UDP (any non-2xx SETUP status), answers it with another
// transport (ErrNotUDP), or no RTP arrives within UDPFallback.
func dialAuto(ctx context.Context, cfg Config, tg target) (*Client, error) {
	c, err := dialSession(ctx, cfg, tg, true)
	if err == nil {
		if c.awaitRTP(ctx, cfg.UDPFallback) {
			return c, nil
		}
		// TEARDOWN goes out even when ctx is what ended the wait.
		c.shutdown(context.WithoutCancel(ctx))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("rtsp: dial: %w", ctx.Err())
		}
	} else if se := (*StatusError)(nil); !errors.Is(err, ErrNotUDP) && (!errors.As(err, &se) || se.Method != "SETUP") {
		return nil, err
	}
	c, err = dialSession(ctx, cfg, tg, false)
	if err != nil {
		return nil, err
	}
	c.nFallback.Add(1)
	return c, nil
}
