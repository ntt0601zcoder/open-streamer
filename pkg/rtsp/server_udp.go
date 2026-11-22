package rtsp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// udpPair is the server's RTP/RTCP socket pair, shared by every UDP
// session: media leaves from it, and one goroutine per socket reads what
// clients send to it.
type udpPair struct {
	rtp, rtcp *net.UDPConn
	ports     [2]int
	local     net.IP
	wg        sync.WaitGroup
}

// udpSendBuffer is the send buffer asked for the pair's RTP socket, best
// effort, so that keyframe bursts to many clients are not dropped by the
// server's own socket.
const udpSendBuffer = 4 << 20

// bindUDP binds the pair on UDPHost and starts its readers; the caller
// holds Server.mu and has checked that no pair is bound.
func (s *Server) bindUDP() error {
	rtpC, rtcpC, err := listenUDPPair(s.cfg.UDPHost)
	if err != nil {
		return fmt.Errorf("rtsp: serve: listen udp pair: %w", err)
	}
	// Best effort: the kernel caps it (net.core.wmem_max on Linux).
	_ = rtpC.SetWriteBuffer(udpSendBuffer)
	a := rtpC.LocalAddr().(*net.UDPAddr)
	p := &udpPair{rtp: rtpC, rtcp: rtcpC, ports: [2]int{a.Port, rtcpC.LocalAddr().(*net.UDPAddr).Port}, local: a.IP}
	p.wg.Add(2)
	go s.readRTCP(p)
	go p.drainRTP()
	s.udp = p
	return nil
}

// UDPPorts returns the server's RTP and RTCP ports, or zeros while it owns
// no pair (no UDPHost, before Serve, or after Close).
func (s *Server) UDPPorts() [2]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udp == nil {
		return [2]int{}
	}
	return s.udp.ports
}

// closeUDP closes the pair and waits for its readers.
func (s *Server) closeUDP() {
	s.mu.Lock()
	p := s.udp
	s.udp = nil
	s.mu.Unlock()
	if p == nil {
		return
	}
	_ = p.rtp.Close()
	_ = p.rtcp.Close()
	p.wg.Wait()
}

// readRTCP reads the RTCP socket until it is closed. A client report (an
// RTCP compound led by an SR or RR) from a session's client RTCP port
// proves that session alive; anything else from that port is counted as a
// protocol error, and a datagram from any other address as a stranger.
func (s *Server) readRTCP(p *udpPair) {
	defer p.wg.Done()
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := p.rtcp.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Unconnected UDP sockets report no peer errors; back off
			// anyway rather than spin.
			time.Sleep(time.Millisecond)
			continue
		}
		key := netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		s.mu.Lock()
		sess := s.udpPeers[key]
		s.mu.Unlock()
		switch {
		case sess == nil:
			s.nStrangers.Add(1)
		case isClientReport(buf[:n]):
			sess.touch()
		default:
			s.nProtocol.Add(1)
		}
	}
}

// drainRTP reads and discards what clients send to the RTP socket (NAT
// punches) until it is closed.
func (p *udpPair) drainRTP() {
	defer p.wg.Done()
	buf := make([]byte, maxDatagram)
	for {
		if _, _, err := p.rtp.ReadFromUDPAddrPort(buf); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// udpFor returns the pair when it can reach peer, else nil.
func (s *Server) udpFor(peer net.IP) *udpPair {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udp == nil || !udpReaches(s.udp.local, peer) {
		return nil
	}
	return s.udp
}

// udpReaches reports whether a socket bound on local can send to peer: an
// unspecified address is dual-stack, any other reaches its own family.
func udpReaches(local, peer net.IP) bool {
	switch {
	case peer == nil:
		return false
	case local == nil || local.IsUnspecified():
		return true
	}
	return (local.To4() != nil) == (peer.To4() != nil)
}

// udpAddr is the address of a client port on the peer IP.
func udpAddr(ip net.IP, port int) netip.AddrPort {
	a, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(a.Unmap(), uint16(port))
}

// claimUDP registers the client RTCP address of a UDP track for sess. It
// fails when another session uses that address.
func (s *Server) claimUDP(key netip.AddrPort, sess *ServerSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.udpPeers[key]; o != nil && o != sess {
		return false
	}
	s.udpPeers[key] = sess
	return true
}

// unclaimUDPLocked forgets the client addresses of sess; the caller holds
// Server.mu.
func (s *Server) unclaimUDPLocked(sess *ServerSession) {
	if sess == nil || sess.kind != transportUDP {
		return
	}
	for _, t := range sess.tracks {
		if t != nil && s.udpPeers[t.rtcpTo] == sess {
			delete(s.udpPeers, t.rtcpTo)
		}
	}
}

// sendUDPLocked sends the datagrams b[ends[i-1]:ends[i]] to to from conn
// under one WriteTimeout deadline; the caller holds the write mutex. A
// failure ends the session unless Close is ending it.
func (s *ServerSession) sendUDPLocked(conn *net.UDPConn, to netip.AddrPort, b []byte, ends []int, op string) error {
	_ = conn.SetWriteDeadline(time.Now().Add(s.c.srv.cfg.WriteTimeout))
	start := 0
	for _, end := range ends {
		if _, err := conn.WriteToUDPAddrPort(b[start:end], to); err != nil {
			return s.writeFailed(fmt.Errorf("rtsp: %s: %w", op, err))
		}
		start = end
	}
	return nil
}
