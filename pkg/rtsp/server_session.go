package rtsp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

// sessionCNAME is the SDES CNAME of the server's BYE compounds.
const sessionCNAME = "open-streamer"

// serverTrack is one set-up track: its RTP origin, its channels or client
// addresses and its sending state.
type serverTrack struct {
	index  int
	url    string         // the SETUP request URL, byte for byte
	ch     [2]int         // interleaved RTP and RTCP channels (TCP)
	rtpTo  netip.AddrPort // the client's RTP address (UDP)
	rtcpTo netip.AddrPort // the client's RTCP address (UDP)
	ssrc   uint32
	seq0   uint16
	tsBase uint32

	// Guarded by serverConn.wmu.
	next   uint16
	lastSR []byte
}

// ServerSession is one client's session on a Server. Its tracks and
// transport are fixed once it plays; WritePackets, WriteRTCP and Close may
// be called from any goroutine.
type ServerSession struct {
	c      *serverConn
	id     string
	path   string
	descs  []TrackDesc
	tracks []*serverTrack // by SDP index; nil when not set up
	kind   transportKind
	used   [256]bool // interleaved channels in use
	pair   *udpPair  // the server's UDP pair (UDP)

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	endOnce   sync.Once
	closeOnce sync.Once
	closing   atomic.Bool
	errMu     sync.Mutex
	err       error
	lastSeen  atomic.Int64 // Unix nanoseconds of the last request or client RTCP
}

func newServerSession(c *serverConn, path string, descs []TrackDesc) *ServerSession {
	var id [8]byte
	// Since Go 1.24 crypto/rand.Read does not return an error.
	_, _ = rand.Read(id[:])
	ctx, cancel := context.WithCancel(context.Background())
	s := &ServerSession{
		c: c, id: hex.EncodeToString(id[:]), path: path, descs: copyDescs(descs),
		tracks: make([]*serverTrack, len(descs)), ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	s.touch()
	return s
}

func copyDescs(in []TrackDesc) []TrackDesc {
	out := make([]TrackDesc, len(in))
	for i, d := range in {
		if d.ParamSets != nil {
			ps := make([][]byte, len(d.ParamSets))
			for j, p := range d.ParamSets {
				ps[j] = append([]byte(nil), p...)
			}
			d.ParamSets = ps
		}
		if d.AAC != nil {
			a := *d.AAC
			a.Config = append([]byte(nil), a.Config...)
			d.AAC = &a
		}
		out[i] = d
	}
	return out
}

func random32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// addTrack sets up track index with a fresh random origin: an SSRC unique
// in the session, a first sequence number and a timestamp base. Over TCP
// it takes the client's channels when they are free, else the next free
// even pair; over UDP it sends to the client's ports on the peer IP.
func (s *ServerSession) addTrack(index int, rawURL string, kind transportKind, offer Transport) *serverTrack {
	t := &serverTrack{index: index, url: rawURL, tsBase: random32()}
	t.seq0 = uint16(random32())
	t.next = t.seq0
	for unique := false; !unique; {
		t.ssrc = random32()
		unique = true
		for _, o := range s.tracks {
			if o != nil && o.ssrc == t.ssrc {
				unique = false
			}
		}
	}
	switch kind {
	case transportTCP:
		t.ch = s.freeChannels(offer.Interleaved)
		s.used[t.ch[0]], s.used[t.ch[1]] = true, true
	case transportUDP:
		t.rtpTo, t.rtcpTo = udpAddr(s.c.peer, offer.ClientPort[0]), udpAddr(s.c.peer, offer.ClientPort[1])
	case transportNone:
	}
	s.kind = kind
	s.tracks[index] = t
	return t
}

// freeChannels returns want when both channels are distinct, in range and
// free, else the first free even pair. A session has at most 8 tracks, so
// a pair is always free.
func (s *ServerSession) freeChannels(want [2]int) [2]int {
	ok := func(a int) bool { return a >= 0 && a < len(s.used) && !s.used[a] }
	if want[0] != want[1] && ok(want[0]) && ok(want[1]) {
		return want
	}
	a := 0
	for !ok(a) || !ok(a+1) {
		a += 2
	}
	return [2]int{a, a + 1}
}

// matches reports whether a Session header value names this session.
func (s *ServerSession) matches(v string) bool {
	h, err := ParseSession(v)
	return err == nil && h.ID == s.id
}

// header is the Session header value: the ID and the timeout in seconds.
func (s *ServerSession) header() string {
	secs := int64(math.Ceil(s.c.srv.cfg.SessionTimeout.Seconds()))
	return s.id + ";timeout=" + strconv.FormatInt(max(secs, 1), 10)
}

// rtpInfo is the PLAY response's RTP-Info: per set-up track in SDP order
// its SETUP URL, first sequence number and timestamp base.
func (s *ServerSession) rtpInfo() string {
	var parts []string
	for _, t := range s.tracks {
		if t != nil {
			parts = append(parts, fmt.Sprintf("url=%s;seq=%d;rtptime=%d", t.url, t.seq0, t.tsBase))
		}
	}
	return strings.Join(parts, ",")
}

// rtcpChannel reports whether ch is the interleaved RTCP channel of a
// set-up track of a TCP session.
func (s *ServerSession) rtcpChannel(ch int) bool {
	if s.kind != transportTCP {
		return false
	}
	for _, t := range s.tracks {
		if t != nil && t.ch[1] == ch {
			return true
		}
	}
	return false
}

// touch records a sign of life from the client.
func (s *ServerSession) touch() { s.lastSeen.Store(s.c.srv.cfg.Now().UnixNano()) }

// idleLeft is how long the session may still stay silent.
func (s *ServerSession) idleLeft() time.Duration {
	return s.c.srv.cfg.SessionTimeout - s.c.srv.cfg.Now().Sub(time.Unix(0, s.lastSeen.Load()))
}

// end ends the session with err: the first call wins, cancels Play's
// context and closes the connection.
func (s *ServerSession) end(err error) {
	s.endOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.done)
		s.cancel()
		_ = s.c.nc.Close()
	})
}

// Path returns the session's normalised stream path.
func (s *ServerSession) Path() string { return s.path }

// Tracks returns the set-up track indexes in SDP order.
func (s *ServerSession) Tracks() []int {
	var out []int
	for i, t := range s.tracks {
		if t != nil {
			out = append(out, i)
		}
	}
	return out
}

// TrackDescs returns a deep copy of the descriptors frozen at the first
// SETUP.
func (s *ServerSession) TrackDescs() []TrackDesc { return copyDescs(s.descs) }

// UDP reports whether the session plays over UDP.
func (s *ServerSession) UDP() bool { return s.kind == transportUDP }

// Origin returns the SSRC, first sequence number and timestamp base of a
// set-up track, or zeros for any other.
func (s *ServerSession) Origin(track int) (ssrc uint32, seq uint16, tsBase uint32) {
	if t := s.track(track); t != nil {
		return t.ssrc, t.seq0, t.tsBase
	}
	return 0, 0, 0
}

func (s *ServerSession) track(i int) *serverTrack {
	if i < 0 || i >= len(s.tracks) {
		return nil
	}
	return s.tracks[i]
}

// Done is closed when the session ends.
func (s *ServerSession) Done() <-chan struct{} { return s.done }

// Err returns why the session ended, or nil while it runs.
func (s *ServerSession) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// WritePackets sends the packets of one frame on a set-up track: it stamps
// version 2, the track's SSRC and payload type and the next sequence number
// on every packet, and writes them under one WriteTimeout deadline, as
// interleaved frames in one write over TCP or as one datagram each to the
// client's RTP port over UDP. A missed deadline or a failed write ends the
// session; a batch that fails to marshal is refused whole and consumes no
// sequence numbers. The stamps modify the caller's packets in place, so do
// not share them across sessions or goroutines. The packets are not
// retained.
func (s *ServerSession) WritePackets(track int, ps []*rtp.Packet) error {
	t := s.track(track)
	if t == nil {
		return fmt.Errorf("rtsp: write packets: track %d not set up", track)
	}
	udp := s.kind == transportUDP
	hdr := 4 // the interleaved frame header
	if udp {
		hdr = 0
	}
	size := 0
	for _, p := range ps {
		n := p.MarshalSize()
		if n > math.MaxUint16 {
			return fmt.Errorf("rtsp: write packets: packet of %d bytes too large", n)
		}
		size += hdr + n
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	pt := s.descs[track].Payload
	c := s.c
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := s.Err(); err != nil {
		return fmt.Errorf("rtsp: write packets: %w", err)
	}
	off := 0
	seq := t.next
	var ends []int
	if udp {
		ends = make([]int, 0, len(ps))
	}
	for _, p := range ps {
		p.Version, p.SSRC, p.PayloadType, p.SequenceNumber = 2, t.ssrc, pt, seq
		n, err := p.MarshalTo(buf[off+hdr:])
		if err != nil {
			return fmt.Errorf("rtsp: write packets: %w", err)
		}
		seq++
		if udp {
			ends = append(ends, off+n)
		} else {
			buf[off], buf[off+1] = '$', byte(t.ch[0])
			binary.BigEndian.PutUint16(buf[off+2:], uint16(n))
		}
		off += hdr + n
	}
	// Sequence numbers are committed only once the whole batch marshalled.
	t.next = seq
	if udp {
		return s.sendUDPLocked(s.pair.rtp, t.rtpTo, buf, ends, "write packets")
	}
	return s.writeLocked(buf[:off], "write packets")
}

// WriteRTCP sends an RTCP packet on a set-up track's RTCP channel, or to
// the client's RTCP port over UDP, under one WriteTimeout deadline. A
// sender report is remembered for the BYE compound of Close. b is not
// retained.
func (s *ServerSession) WriteRTCP(track int, b []byte) error {
	t := s.track(track)
	switch {
	case t == nil:
		return fmt.Errorf("rtsp: write rtcp: track %d not set up", track)
	case len(b) > math.MaxUint16:
		return fmt.Errorf("rtsp: write rtcp: packet of %d bytes too large", len(b))
	}
	c := s.c
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := s.Err(); err != nil {
		return fmt.Errorf("rtsp: write rtcp: %w", err)
	}
	if sr := firstSR(b); sr != nil {
		t.lastSR = sr
	}
	if s.kind == transportUDP {
		return s.sendUDPLocked(s.pair.rtcp, t.rtcpTo, b, []int{len(b)}, "write rtcp")
	}
	return s.writeLocked(interleave(t.ch[1], b), "write rtcp")
}

// firstSR returns a copy of b's leading sender report, or nil.
func firstSR(b []byte) []byte {
	if len(b) < rtcpHeaderLen+srBodyLen || b[0]>>6 != rtcpVersionTop || b[1] != rtcpSR {
		return nil
	}
	n := (int(binary.BigEndian.Uint16(b[2:])) + 1) * 4
	if n > len(b) {
		return nil
	}
	return append([]byte(nil), b[:n]...)
}

func interleave(ch int, b []byte) []byte {
	f := make([]byte, 4+len(b))
	f[0], f[1] = '$', byte(ch)
	binary.BigEndian.PutUint16(f[2:], uint16(len(b)))
	copy(f[4:], b)
	return f
}

// writeLocked writes b under the WriteTimeout deadline; the caller holds
// the write mutex. A failure ends the session unless Close is ending it.
func (s *ServerSession) writeLocked(b []byte, op string) error {
	c := s.c
	_ = c.nc.SetWriteDeadline(time.Now().Add(c.srv.cfg.WriteTimeout))
	if _, err := c.nc.Write(b); err != nil {
		return s.writeFailed(fmt.Errorf("rtsp: %s: %w", op, err))
	}
	return nil
}

// writeFailed ends the session with a write error, counting a missed
// deadline as a timeout, unless Close is ending it; it returns err.
func (s *ServerSession) writeFailed(err error) error {
	if !s.closing.Load() {
		if isTimeout(err) {
			s.c.srv.nTimeouts.Add(1)
		}
		s.end(err)
	}
	return err
}

// Close ends the session: per set-up track it sends, best effort within
// 500 ms (or the WriteTimeout when shorter), one compound RTCP packet of
// the track's last sender report (or an empty receiver report) + SDES +
// BYE on the track's RTCP channel or to the client's RTCP port, then
// closes the connection; it returns within that budget. Over TCP a writer
// blocked on the connection's full send buffer is cut short. Over UDP the
// pair's sockets are shared by every UDP session, so a writer blocked on
// the pair's full send buffer is not cut (shortening the shared deadline
// would fail other sessions' writes): Close then skips the BYE and ends
// the session, and that write returns within the WriteTimeout (default
// 10 s) without ending anything further. It is idempotent.
func (s *ServerSession) Close() {
	s.closeOnce.Do(func() {
		if s.Err() != nil {
			return
		}
		s.closing.Store(true)
		c := s.c
		// A writer still busy is given half the budget, the BYE the rest.
		budget := min(byeWait, c.srv.cfg.WriteTimeout)
		now := time.Now()
		deadline := now.Add(budget)
		if c.lockWriteBy(now.Add(budget/2), deadline) {
			if s.Err() == nil {
				s.sendByeLocked(deadline)
			}
			c.wmu.Unlock()
		}
		s.end(ErrSessionClosed)
	})
}

// sendByeLocked sends the BYE compound of every set-up track by deadline;
// the caller holds the write mutex.
func (s *ServerSession) sendByeLocked(deadline time.Time) {
	var buf []byte
	for _, t := range s.tracks {
		if t == nil {
			continue
		}
		bye := BuildBye(t.ssrc, sessionCNAME, t.lastSR)
		if s.kind == transportUDP {
			_ = s.pair.rtcp.SetWriteDeadline(deadline)
			_, _ = s.pair.rtcp.WriteToUDPAddrPort(bye, t.rtcpTo)
			continue
		}
		buf = append(buf, interleave(t.ch[1], bye)...)
	}
	if buf != nil {
		_ = s.c.nc.SetWriteDeadline(deadline)
		_, _ = s.c.nc.Write(buf)
	}
}
