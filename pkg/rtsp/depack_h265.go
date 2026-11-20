package rtsp

import (
	"time"

	"github.com/pion/rtp"
)

// H.265 NAL unit types (RFC 7798 section 4.4).
const (
	h265VCLMax = 31
	h265AUD    = 35
	h265AP     = 48
	h265FU     = 49
)

func isSlice265(n []byte) bool {
	return len(n) >= 2 && (n[0]>>1)&0x3f <= h265VCLMax
}

// valid265 accepts NAL unit types 0..47 with a full two-byte header.
func valid265(n []byte) bool {
	return len(n) >= 2 && (n[0]>>1)&0x3f < h265AP
}

func isAUD265(n []byte) bool { return (n[0]>>1)&0x3f == h265AUD }

// h265Depack implements RFC 7798 without DONL: single NAL unit, AP and FU.
// PACI packets are ignored.
type h265Depack struct {
	asm *assembler
}

// Oversize implements OversizeCounter.
func (d *h265Depack) Oversize() uint64 { return d.asm.Oversize() }

// Flush implements Depacketizer.
func (d *h265Depack) Flush() []AU { return d.asm.Flush() }

// Gap implements Depacketizer.
func (d *h265Depack) Gap() { d.asm.Gap() }

// Push implements Depacketizer.
func (d *h265Depack) Push(p *rtp.Packet, arrival time.Time) []AU {
	out := d.asm.advance(p.Timestamp, arrival)
	pl := p.Payload
	if len(pl) < 2 {
		return out
	}
	switch t := (pl[0] >> 1) & 0x3f; {
	case t < h265AP:
		d.asm.add(pl)
	case t == h265AP:
		d.asm.aggregate(pl[2:])
	case t == h265FU:
		d.fu(pl)
	default:
		// PACI (50) and reserved types are ignored.
	}
	return out
}

func (d *h265Depack) fu(pl []byte) {
	if len(pl) < 3 {
		d.asm.lost = true
		return
	}
	start, end := pl[2]&0x80 != 0, pl[2]&0x40 != 0
	switch {
	case start && end:
		d.asm.lost = true
	case start:
		hdr := []byte{pl[0]&0x81 | (pl[2]&0x3f)<<1, pl[1]}
		d.asm.fragStart(hdr, pl[3:])
	default:
		d.asm.fragNext(pl[3:], end)
	}
}
