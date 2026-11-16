package rtsp

import (
	"time"

	"github.com/pion/rtp"
)

// H.264 NAL unit and packetization types (RFC 6184 section 5).
const (
	h264SliceMin = 1
	h264SliceMax = 5
	h264AUD      = 9
	h264STAPA    = 24
	h264FUA      = 28
)

func isSlice264(n []byte) bool {
	t := n[0] & 0x1f
	return t >= h264SliceMin && t <= h264SliceMax
}

func isAUD264(n []byte) bool { return n[0]&0x1f == h264AUD }

// h264Depack implements RFC 6184: single NAL unit, STAP-A and FU-A.
type h264Depack struct {
	asm *assembler
}

// Oversize implements OversizeCounter.
func (d *h264Depack) Oversize() uint64 { return d.asm.Oversize() }

// Flush implements Depacketizer.
func (d *h264Depack) Flush() []AU { return d.asm.Flush() }

// Gap implements Depacketizer.
func (d *h264Depack) Gap() { d.asm.Gap() }

// Push implements Depacketizer.
func (d *h264Depack) Push(p *rtp.Packet, arrival time.Time) []AU {
	out := d.asm.advance(p.Timestamp, arrival)
	pl := p.Payload
	if len(pl) == 0 {
		return out
	}
	switch t := pl[0] & 0x1f; {
	case t >= 1 && t < h264STAPA:
		d.asm.add(pl)
	case t == h264STAPA:
		d.asm.aggregate(pl[1:])
	case t == h264FUA:
		d.fu(pl)
	default:
		// STAP-B, MTAP, FU-B and reserved types are not used in mode 1.
	}
	return out
}

func (d *h264Depack) fu(pl []byte) {
	if len(pl) < 2 {
		d.asm.lost = true
		return
	}
	start, end := pl[1]&0x80 != 0, pl[1]&0x40 != 0
	switch {
	case start && end:
		d.asm.lost = true
	case start:
		d.asm.fragStart([]byte{pl[0]&0xe0 | pl[1]&0x1f}, pl[2:])
	default:
		d.asm.fragNext(pl[2:], end)
	}
}
