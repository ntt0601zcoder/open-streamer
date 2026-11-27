package rtsp

import "time"

// UDP reorder bounds per track: a packet waits behind a missing sequence
// number for at most reorderWait, and the buffer spans at most reorderSpan
// sequence numbers.
const (
	reorderSpan = 64
	reorderWait = 200 * time.Millisecond
)

// pushResult is what the reorder buffer did with a packet.
type pushResult int

const (
	pushQueued    pushResult = iota // buffered in order
	pushReordered                   // buffered after a later packet arrived
	pushDup                         // a duplicate of a buffered or released packet, dropped
	pushLate                        // older than a skipped sequence number, dropped
)

type reorderSlot struct {
	p  Packet
	at time.Time // when it entered the buffer
	ok bool
}

type releasedSlot struct {
	seq uint16
	ok  bool
}

// reorder puts the RTP packets of one UDP track back in sequence order. It
// never reports loss: a missing sequence number is skipped once the packet
// after it has waited reorderWait or the buffer would span more than
// reorderSpan, and the caller's SeqTracker sees the gap. It is used by
// ReadPacket only.
type reorder struct {
	started bool
	ssrc    uint32
	next    uint16 // the next sequence number to release
	top     uint16 // the highest sequence number accepted
	n       int    // buffered packets
	slots   [reorderSpan]reorderSlot
	// released remembers the sequence numbers released in the last span,
	// to tell a duplicate from a packet whose slot was skipped.
	released [reorderSpan]releasedSlot
	// hint is the first sequence number from RTP-Info, used when the first
	// packet received is at most a span after it (the first one sent may
	// have been reordered).
	hint    uint16
	hasHint bool
}

// push adds an RTP packet received at now. Packets that become releasable
// out of turn (a restart or a skip forward) are appended to out in order.
func (r *reorder) push(p Packet, now time.Time, out *[]Packet) pushResult {
	seq := p.RTP.SequenceNumber
	if !r.started || p.RTP.SSRC != r.ssrc {
		r.restart(seq, p.RTP.SSRC, out)
	}
	d := seq - r.next
	switch {
	case d < reorderSpan:
	case int16(d) < 0 && int16(d) >= -seqWindow:
		if rs := r.released[seq%reorderSpan]; rs.ok && rs.seq == seq {
			return pushDup
		}
		return pushLate
	case d < seqWindow:
		r.advance(seq-reorderSpan+1, out)
	default:
		r.restart(seq, p.RTP.SSRC, out)
	}
	s := &r.slots[seq%reorderSpan]
	if s.ok {
		return pushDup
	}
	*s = reorderSlot{p: p, at: now, ok: true}
	r.n++
	if int16(seq-r.top) < 0 {
		return pushReordered
	}
	r.top = seq
	return pushQueued
}

// restart flushes the buffer and starts over at seq: the first packet, an
// SSRC change or a jump beyond the sequence window.
func (r *reorder) restart(seq uint16, ssrc uint32, out *[]Packet) {
	r.flush(out)
	first := !r.started
	r.started, r.ssrc, r.next, r.top = true, ssrc, seq, seq
	r.released = [reorderSpan]releasedSlot{}
	if first && r.hasHint && seq-r.hint < reorderSpan {
		r.next = r.hint
	}
	r.hasHint = false
}

// advance releases every buffered packet before seq, in order, and moves
// next to seq.
func (r *reorder) advance(seq uint16, out *[]Packet) {
	for r.next != seq && r.n > 0 {
		if p, ok := r.take(r.next); ok {
			*out = append(*out, p)
		}
		r.next++
	}
	r.next = seq
}

// flush releases every buffered packet in order.
func (r *reorder) flush(out *[]Packet) {
	for r.n > 0 {
		if p, ok := r.take(r.next); ok {
			*out = append(*out, p)
		}
		r.next++
	}
}

// take removes the packet with sequence number seq, if buffered.
func (r *reorder) take(seq uint16) (Packet, bool) {
	s := &r.slots[seq%reorderSpan]
	if !s.ok {
		return Packet{}, false
	}
	p := s.p
	*s = reorderSlot{}
	r.n--
	r.released[seq%reorderSpan] = releasedSlot{seq: seq, ok: true}
	return p, true
}

// oldest returns when the longest-waiting buffered packet arrived.
func (r *reorder) oldest() time.Time {
	var t time.Time
	for i := range r.slots {
		if s := &r.slots[i]; s.ok && (t.IsZero() || s.at.Before(t)) {
			t = s.at
		}
	}
	return t
}

// pop releases the next packet: the expected one when buffered, else the
// first buffered one once the oldest has waited reorderWait.
func (r *reorder) pop(now time.Time) (Packet, bool) {
	if r.n == 0 {
		return Packet{}, false
	}
	if p, ok := r.take(r.next); ok {
		r.next++
		return p, true
	}
	if now.Sub(r.oldest()) < reorderWait {
		return Packet{}, false
	}
	for {
		r.next++
		if p, ok := r.take(r.next); ok {
			r.next++
			return p, true
		}
	}
}

// deadline returns when pop next releases a packet without a new one
// arriving; ok is false when the buffer is empty.
func (r *reorder) deadline() (time.Time, bool) {
	if r.n == 0 {
		return time.Time{}, false
	}
	if r.slots[r.next%reorderSpan].ok {
		return time.Time{}, true
	}
	return r.oldest().Add(reorderWait), true
}
