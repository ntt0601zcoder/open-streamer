package rtsp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/pion/rtp"
)

// methods are the RTSP 1.0 request methods (RFC 2326 section 10) a server
// may send on the control connection.
var methods = []string{
	"OPTIONS", "DESCRIBE", "ANNOUNCE", "SETUP", "PLAY", "PAUSE", "RECORD",
	"TEARDOWN", "GET_PARAMETER", "SET_PARAMETER", "REDIRECT",
}

// maxStartPeek covers "RTSP/1.0" and the longest method plus its space.
const maxStartPeek = len("GET_PARAMETER ")

// readLoop owns the reading side of the connection until it fails.
func (c *Client) readLoop() {
	defer c.wg.Done()
	for {
		if err := c.readOne(); err != nil {
			c.fail(err)
			return
		}
	}
}

// readOne reads one unit: an interleaved frame, a response, a server request,
// a stray line end, or a run of garbage.
func (c *Client) readOne() error {
	b, err := c.br.Peek(1)
	if err != nil {
		return fmt.Errorf("rtsp: read: %w", err)
	}
	switch b[0] {
	case '$':
		return c.readFrame()
	case '\r', '\n':
		// RFC 2326 section 10: stray line ends between messages are ignored.
		_, _ = c.br.Discard(1)
		return nil
	}
	// A short peek ends in an error the next Peek reports again.
	head, _ := c.br.Peek(maxStartPeek)
	switch {
	case bytes.HasPrefix(head, []byte(protoVersion)):
		resp, err := ReadResponse(c.br, c.cfg.Limits)
		if err != nil {
			return err
		}
		c.deliver(resp)
		return nil
	case isRequestStart(head):
		req, err := ReadRequest(c.br, c.cfg.Limits)
		if err != nil {
			return err
		}
		return c.answer(req)
	}
	return c.resync()
}

func isRequestStart(head []byte) bool {
	for _, m := range methods {
		if len(head) > len(m) && string(head[:len(m)]) == m && head[len(m)] == ' ' {
			return true
		}
	}
	return false
}

// resync skips to the next '$' or past the next line end.
func (c *Client) resync() error {
	c.nResyncs.Add(1)
	for {
		b, err := c.br.Peek(1)
		if err != nil {
			return fmt.Errorf("rtsp: read: %w", err)
		}
		v := b[0]
		if v == '$' {
			return nil
		}
		_, _ = c.br.Discard(1)
		if v == '\n' {
			return nil
		}
	}
}

// deliver hands a response to the request waiting on its CSeq.
func (c *Client) deliver(r *Response) {
	cs, err := strconv.Atoi(r.Header.Get("CSeq"))
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[cs]
	delete(c.pending, cs)
	c.mu.Unlock()
	if ch != nil {
		ch <- r // buffered, and each CSeq is delivered once
	}
}

// answer replies to a server request: OPTIONS and GET_PARAMETER with 200,
// anything else with 501.
func (c *Client) answer(req *Request) error {
	c.nServerReqs.Add(1)
	resp := &Response{StatusCode: 501, Reason: "Not Implemented", Header: Header{}}
	switch req.Method {
	case "OPTIONS":
		resp.StatusCode, resp.Reason = 200, "OK"
		resp.Header.Set("Public", "OPTIONS, GET_PARAMETER")
	case "GET_PARAMETER":
		resp.StatusCode, resp.Reason = 200, "OK"
	}
	if cs := req.Header.Get("CSeq"); validText(cs) {
		resp.Header.Set("CSeq", cs)
	}
	if s := c.sessionID(); s != "" {
		resp.Header.Set("Session", s)
	}
	if err := c.writeMsg(resp, time.Now().Add(c.cfg.Timeout)); err != nil {
		return err
	}
	return nil
}

// readFrame reads one interleaved frame and queues it as a packet of its
// track.
func (c *Client) readFrame() error {
	var h [4]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return fmt.Errorf("rtsp: read frame: %w", err)
	}
	data := make([]byte, binary.BigEndian.Uint16(h[2:]))
	if _, err := io.ReadFull(c.br, data); err != nil {
		return fmt.Errorf("rtsp: read frame: %w", err)
	}
	arrival := c.cfg.Now()
	e := c.byChan.Load()[h[1]]
	switch {
	case e.t == nil:
		c.nUnknown.Add(1)
		return nil
	case e.rtcp:
		c.onRTCP(e.t, data)
		return c.queue(Packet{Track: e.t.index, RTCP: data, Arrival: arrival})
	}
	p, ok := parseRTP(data)
	if !ok {
		c.nDropped.Add(1)
		return nil
	}
	c.onRTP(e.t, p)
	c.touch()
	return c.queue(Packet{Track: e.t.index, RTP: p, Arrival: arrival})
}

// parseRTP parses an RTP packet; the payload aliases data.
func parseRTP(data []byte) (*rtp.Packet, bool) {
	p := &rtp.Packet{}
	return p, p.Unmarshal(data) == nil
}

// queue hands a packet to ReadPacket. Over TCP a full queue blocks the
// reader (backpressure); the timers pause while it does.
func (c *Client) queue(p Packet) error {
	select {
	case c.pkts <- p:
		return nil
	default:
	}
	c.blocked.Store(true)
	defer func() {
		c.blocked.Store(false)
		c.touch()
	}()
	select {
	case c.pkts <- p:
		return nil
	case <-c.done:
		return net.ErrClosed
	}
}

// onRTP updates the reception state of a track (RFC 3550 appendix A.1,
// simplified: a jump beyond the sequence window or an SSRC change restarts
// it).
func (c *Client) onRTP(t *clientTrack, p *rtp.Packet) {
	c.rrMu.Lock()
	defer c.rrMu.Unlock()
	seq := p.SequenceNumber
	if !t.rx.has || p.SSRC != t.rx.ssrc {
		t.rx = rxState{has: true, ssrc: p.SSRC, baseSeq: seq, maxSeq: seq, received: 1}
		return
	}
	t.rx.received++
	switch d := seq - t.rx.maxSeq; {
	case d == 0 || d >= 1<<15:
		// duplicate or late
	case d < seqWindow:
		if seq < t.rx.maxSeq {
			t.rx.cycles += 1 << 16
		}
		t.rx.maxSeq = seq
	default:
		t.rx.baseSeq, t.rx.maxSeq, t.rx.cycles, t.rx.received, t.rx.expPrior, t.rx.recvPrior = seq, seq, 0, 1, 0, 0
	}
}

// onRTCP records the last sender report of a track for LSR and DLSR.
func (c *Client) onRTCP(t *clientTrack, data []byte) {
	srs := ParseSenderReports(data)
	if len(srs) == 0 {
		return
	}
	sr := srs[len(srs)-1]
	c.rrMu.Lock()
	t.rx.lsr = ntpMiddle(sr.NTP)
	t.rx.lsrAt = time.Now()
	c.rrMu.Unlock()
}

// ntpMiddle returns the middle 32 bits of the 64-bit NTP timestamp of t.
func ntpMiddle(t time.Time) uint32 {
	sec := uint64(t.Unix() + ntpToUnixSecs)
	frac := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	return uint32(sec<<16 | frac>>16)
}

// reportBlock builds the reception report of a track and starts the next
// interval.
func (c *Client) reportBlock(t *clientTrack) (ReportBlock, bool) {
	c.rrMu.Lock()
	defer c.rrMu.Unlock()
	if !t.rx.has {
		return ReportBlock{}, false
	}
	ext := t.rx.cycles + uint32(t.rx.maxSeq)
	expected := ext - uint32(t.rx.baseSeq) + 1
	lost := max(int64(expected)-int64(t.rx.received), 0)
	expInt, recvInt := expected-t.rx.expPrior, t.rx.received-t.rx.recvPrior
	t.rx.expPrior, t.rx.recvPrior = expected, t.rx.received
	var frac uint8
	if lostInt := int64(expInt) - int64(recvInt); expInt > 0 && lostInt > 0 {
		frac = uint8(min(lostInt<<8/int64(expInt), 255))
	}
	b := ReportBlock{SSRC: t.rx.ssrc, FractionLost: frac, Lost: uint32(min(lost, maxLost24)), HighestSeq: ext, LSR: t.rx.lsr}
	if !t.rx.lsrAt.IsZero() {
		b.DLSR = uint32(time.Since(t.rx.lsrAt).Seconds() * 65536)
	}
	return b, true
}

// keepalive is a keepalive request awaiting its response.
type keepalive struct {
	cs       int
	ch       chan *Response
	deadline time.Time
	retried  bool
}

// timerLoop sends keepalives and receiver reports, matches keepalive
// responses asynchronously, and enforces the idle and response timeouts.
// The clocks pause while the reader is blocked on a full packet queue.
func (c *Client) timerLoop() {
	defer c.wg.Done()
	keepEvery := c.sessionTimeout / 2
	if keepEvery <= 0 {
		keepEvery = defaultSessionTimeout / 2
	}
	tick := min(c.cfg.IdleTimeout, c.cfg.Timeout, keepEvery, c.cfg.ReportInterval) / 4
	tick = min(max(tick, 5*time.Millisecond), time.Second)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	last := time.Now()
	nextKeep, nextReport := last.Add(keepEvery), last.Add(c.cfg.ReportInterval)
	var waiting []*keepalive
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		now := time.Now()
		if c.blocked.Load() {
			for _, k := range waiting {
				k.deadline = k.deadline.Add(now.Sub(last))
			}
			c.touch()
		}
		last = now
		if time.Since(c.epoch)-time.Duration(c.lastPacket.Load()) > c.cfg.IdleTimeout {
			c.fail(ErrIdle)
			return
		}
		var err error
		if waiting, err = c.checkKeepalives(waiting, now); err != nil {
			c.fail(err)
			return
		}
		if !now.Before(nextKeep) {
			k, err := c.sendKeepalive(false)
			if err != nil {
				c.fail(err)
				return
			}
			waiting = append(waiting, k)
			nextKeep = now.Add(keepEvery)
		}
		if !now.Before(nextReport) {
			if err := c.sendReports(); err != nil {
				c.fail(err)
				return
			}
			nextReport = now.Add(c.cfg.ReportInterval)
		}
	}
}

// keepaliveMethod is GET_PARAMETER when the server lists it, else OPTIONS.
func (c *Client) keepaliveMethod() string {
	if c.getParam {
		return "GET_PARAMETER"
	}
	return "OPTIONS"
}

func (c *Client) sendKeepalive(retried bool) (*keepalive, error) {
	ch, cs, err := c.send(c.keepaliveMethod(), c.desc.Control, nil, time.Now().Add(c.cfg.Timeout))
	if err != nil {
		return nil, err
	}
	return &keepalive{cs: cs, ch: ch, deadline: time.Now().Add(c.cfg.Timeout), retried: retried}, nil
}

// checkKeepalives consumes answered keepalives. A 454 (Session Not Found)
// ends the session; a 401 re-challenges and retries once; other statuses are
// accepted, since any answer proves the connection alive.
func (c *Client) checkKeepalives(waiting []*keepalive, now time.Time) ([]*keepalive, error) {
	var keep []*keepalive
	for _, k := range waiting {
		select {
		case r := <-k.ch:
			if r.StatusCode == 454 {
				// Session Not Found: the session is gone.
				return nil, &StatusError{Method: c.keepaliveMethod(), Code: r.StatusCode}
			}
			if r.StatusCode != 401 {
				continue
			}
			if k.retried {
				return nil, &StatusError{Method: c.keepaliveMethod(), Code: r.StatusCode}
			}
			if err := c.rechallenge(r.Header); err != nil {
				return nil, fmt.Errorf("rtsp: keepalive: status 401: %w", err)
			}
			nk, err := c.sendKeepalive(true)
			if err != nil {
				return nil, err
			}
			keep = append(keep, nk)
			continue
		default:
		}
		if now.After(k.deadline) {
			c.forget(k.cs)
			return nil, errors.New("rtsp: keepalive: no response within the timeout")
		}
		keep = append(keep, k)
	}
	return keep, nil
}

// sendReports sends a receiver report on the RTCP channel or to the RTCP
// port of every track that has received RTP. Over UDP a track whose server
// named no ports gets none.
func (c *Client) sendReports() error {
	for _, t := range c.tracks {
		b, ok := c.reportBlock(t)
		if !ok {
			continue
		}
		rr := ReceiverReport(c.ssrc, []ReportBlock{b})
		var err error
		switch {
		case t.link == nil:
			err = c.writeFrame(t.rtcpCh, rr)
		case t.link.srvPorts[1] != 0:
			err = c.writeUDP(t.link.rtcp, t.link.srvAddr(1), rr)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
