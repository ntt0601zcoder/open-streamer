package ingest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// udpReadBuffer is the requested socket receive buffer.
const udpReadBuffer = 4 << 20

const (
	tsSyncByte     = 0x47
	rtpHeaderLen   = 12
	maxUDPDatagram = 64 << 10
)

// udpErr wraps err with the redacted URL.
func udpErr(rawURL, op string, err error) error {
	return fmt.Errorf("ingest: udp: %s %s: %w", op, Redact(rawURL), err)
}

// udpConn adapts a datagram socket to an io.ReadCloser of TS bytes: each
// datagram is unwrapped (raw TS or RTP) and handed out through Read. Read is
// called from one goroutine; Close and the counters are concurrent-safe.
type udpConn struct {
	pc  *net.UDPConn
	buf []byte
	rem []byte // unread payload of the current datagram

	haveSeq bool
	lastSeq uint16

	rtpGaps  atomic.Uint64
	nonTS    atomic.Uint64
	datagram atomic.Uint64
}

// Read implements io.Reader.
func (c *udpConn) Read(p []byte) (int, error) {
	for len(c.rem) == 0 {
		n, err := c.pc.Read(c.buf)
		if err != nil {
			return 0, err //nolint:wrapcheck // io.Reader contract: errors pass through unwrapped.
		}
		c.datagram.Add(1)
		c.rem = c.unwrap(c.buf[:n])
	}
	n := copy(p, c.rem)
	c.rem = c.rem[n:]
	return n, nil
}

// Close implements io.Closer.
func (c *udpConn) Close() error {
	err := c.pc.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err //nolint:wrapcheck // wrapped by the reader.
	}
	return nil
}

// unwrap returns the TS payload of one datagram, or nil for one that is
// neither raw TS nor RTP-wrapped TS.
func (c *udpConn) unwrap(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if b[0] == tsSyncByte {
		return b
	}
	if b[0]>>6 != 2 || len(b) < rtpHeaderLen {
		c.nonTS.Add(1)
		return nil
	}
	off := rtpHeaderLen + 4*int(b[0]&0x0f)
	if b[0]&0x10 != 0 { // header extension
		if len(b) < off+4 {
			c.nonTS.Add(1)
			return nil
		}
		off += 4 + 4*int(binary.BigEndian.Uint16(b[off+2:]))
	}
	end := len(b)
	if b[0]&0x20 != 0 { // padding
		end -= int(b[end-1])
	}
	if off >= end || b[off] != tsSyncByte {
		c.nonTS.Add(1)
		return nil
	}
	c.countSeq(binary.BigEndian.Uint16(b[2:]))
	return b[off:end]
}

// countSeq counts forward sequence gaps; a duplicate or an older number
// (reordering) neither counts nor moves the expectation.
func (c *udpConn) countSeq(seq uint16) {
	if !c.haveSeq {
		c.haveSeq, c.lastSeq = true, seq
		return
	}
	d := seq - c.lastSeq
	switch {
	case d == 1:
		c.lastSeq = seq
	case d > 1 && d < 0x8000:
		c.rtpGaps.Add(uint64(d - 1))
		c.lastSeq = seq
	}
}

// udpReader reads TS over UDP.
type udpReader struct {
	ts     *tsReader
	conn   *udpConn
	rawURL string
	rcvBuf uint64
	lowBuf uint64
}

func openUDP(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	pc, err := listenUDP(p)
	if err != nil {
		return nil, udpErr(rawURL, "listen", err)
	}
	var low uint64
	if err := pc.SetReadBuffer(udpReadBuffer); err != nil {
		low = 1
	}
	eff := effectiveReadBuffer(pc)
	if eff < udpReadBuffer {
		low = 1 // net.core.rmem_max caps it
	}
	conn := &udpConn{pc: pc, buf: make([]byte, maxUDPDatagram)}
	r, err := openTS(ctx, conn, env.now(), p.spec, &media.ConfigCache{})
	if err != nil {
		return nil, udpErr(rawURL, "open", err)
	}
	return &udpReader{ts: r, conn: conn, rawURL: rawURL, rcvBuf: eff, lowBuf: low}, nil
}

// listenUDP binds the socket: a multicast group is joined on the named
// interface (or the system default), anything else is a plain listen.
func listenUDP(p parsed) (*net.UDPConn, error) {
	host, port := p.u.Hostname(), p.u.Port()
	if ip := net.ParseIP(host); ip != nil && ip.IsMulticast() {
		var ifi *net.Interface
		if name := p.u.Query().Get("iface"); name != "" {
			var err error
			if ifi, err = net.InterfaceByName(name); err != nil {
				return nil, fmt.Errorf("interface %q: %w", name, err)
			}
		}
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, fmt.Errorf("resolve: %w", err)
		}
		pc, err := net.ListenMulticastUDP("udp", ifi, addr)
		if err != nil {
			return nil, fmt.Errorf("join: %w", err)
		}
		return pc, nil
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	pc, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return pc, nil
}

// effectiveReadBuffer returns the socket's receive buffer as the kernel
// reports it, or 0 when it cannot be read.
func effectiveReadBuffer(pc *net.UDPConn) uint64 {
	rc, err := pc.SyscallConn()
	if err != nil {
		return 0
	}
	var v int
	_ = rc.Control(func(fd uintptr) {
		v, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	})
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// Read implements Reader. A closed socket is an error: a live listener never
// ends cleanly.
func (u *udpReader) Read() (Frame, error) {
	f, err := u.ts.Read()
	if err == nil || errors.Is(err, ErrDiscontinuity) {
		return f, err
	}
	return Frame{}, udpErr(u.rawURL, "read", err)
}

// Kinds implements Reader.
func (u *udpReader) Kinds() []media.Kind { return u.ts.Kinds() }

// Spec implements Reader.
func (u *udpReader) Spec() Spec { return u.ts.Spec() }

// Stats implements Reader.
func (u *udpReader) Stats() map[string]uint64 {
	m := u.ts.Stats()
	m["udp_read_buffer"] = u.rcvBuf
	m["udp_read_buffer_low"] = u.lowBuf
	m["rtp_gaps"] = u.conn.rtpGaps.Load()
	m["non_ts_datagrams"] = u.conn.nonTS.Load()
	m["datagrams"] = u.conn.datagram.Load()
	return m
}

// Close implements Reader.
func (u *udpReader) Close() error {
	if err := u.ts.Close(); err != nil {
		return udpErr(u.rawURL, "close", err)
	}
	return nil
}
