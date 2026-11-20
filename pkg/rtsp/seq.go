package rtsp

// seqWindow is how far a sequence number may sit behind the last one and
// still count as a duplicate, and how far ahead it may jump and still count
// as a loss; anything further is a reset.
const seqWindow = 3000

// SeqTracker follows the 16-bit RTP sequence numbers of one track.
type SeqTracker struct {
	has  bool
	last uint16
	ssrc uint32
}

// Next classifies the next received packet. gap is the number of packets
// missing before it. reset reports a stream restart (an SSRC change, or a
// jump beyond 3000 packets either way); it is not a loss. dup reports a
// packet equal to or older than the last one, within 3000 behind.
func (s *SeqTracker) Next(seq uint16, ssrc uint32) (gap int, reset, dup bool) {
	if !s.has {
		s.has, s.last, s.ssrc = true, seq, ssrc
		return 0, false, false
	}
	if ssrc != s.ssrc {
		s.last, s.ssrc = seq, ssrc
		return 0, true, false
	}
	d := int(int16(seq - s.last))
	switch {
	case d <= 0 && d >= -seqWindow:
		return 0, false, true
	case d > 0 && d <= seqWindow:
		s.last = seq
		return d - 1, false, false
	default:
		s.last = seq
		return 0, true, false
	}
}
