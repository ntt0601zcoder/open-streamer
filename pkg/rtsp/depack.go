package rtsp

import (
	"fmt"
	"time"

	"github.com/pion/rtp"
)

// AU is one access unit: the NAL units of a video picture (Annex B start
// codes removed), or one AAC frame in Frames. All slices are fresh
// allocations owned by the caller.
type AU struct {
	Track     int
	Timestamp uint32 // raw RTP timestamp
	NALUs     [][]byte
	Frames    [][]byte
	// Lost marks an AU that is known to be damaged by packet loss.
	Lost bool
	// Arrival is when the AU's own last packet arrived.
	Arrival time.Time
}

// Depacketizer turns the RTP packets of one track into access units.
type Depacketizer interface {
	// Push adds a packet and returns the AUs it completed.
	Push(p *rtp.Packet, arrival time.Time) []AU
	// Flush returns the pending AU, at close.
	Flush() []AU
	// Gap reports a sequence gap: pending fragments are dropped, the
	// pending AU is marked Lost, and so is the AU the next packet starts
	// when that packet has a new timestamp.
	Gap()
}

// OversizeCounter is implemented by the depacketizers; it counts the AUs
// dropped for exceeding the size bound.
type OversizeCounter interface {
	Oversize() uint64
}

// NewDepacketizer returns the depacketizer for a media. maxAU bounds the
// bytes of one access unit.
func NewDepacketizer(m Media, maxAU int) (Depacketizer, error) {
	if m.Reason != "" {
		return nil, fmt.Errorf("rtsp: depacketizer: media %d unsupported: %s", m.Index, m.Reason)
	}
	if maxAU <= 0 {
		maxAU = 8 << 20
	}
	switch m.Codec {
	case CodecH264:
		return &h264Depack{asm: newAssembler(m.Index, maxAU, isSlice264, isAUD264)}, nil
	case CodecH265:
		return &h265Depack{asm: newAssembler(m.Index, maxAU, isSlice265, isAUD265)}, nil
	case CodecAAC:
		if m.AAC == nil || m.ClockRate == 0 {
			return nil, fmt.Errorf("rtsp: depacketizer: media %d: missing AAC format", m.Index)
		}
		return newAACDepack(m, maxAU), nil
	case CodecOther:
		return nil, fmt.Errorf("rtsp: depacketizer: media %d: codec not supported", m.Index)
	default:
		return nil, fmt.Errorf("rtsp: depacketizer: media %d: unknown codec %d", m.Index, m.Codec)
	}
}

// assembler builds video AUs by RTP timestamp. The marker bit is ignored.
type assembler struct {
	track, maxAU int
	isSlice      func(nal []byte) bool
	isAUD        func(nal []byte) bool

	started  bool
	ts       uint32
	nalus    [][]byte
	size     int
	hasSlice bool
	lost     bool
	over     bool
	arrival  time.Time

	carry     [][]byte // NAL units of slice-less AUs, prepended to the next AU
	carrySize int
	gapNext   bool

	frag    []byte
	fragOn  bool
	oversze uint64
}

func newAssembler(track, maxAU int, slice, aud func([]byte) bool) *assembler {
	return &assembler{track: track, maxAU: maxAU, isSlice: slice, isAUD: aud}
}

// Oversize returns the AUs dropped for exceeding the size bound.
func (a *assembler) Oversize() uint64 { return a.oversze }

// advance closes the pending AU when ts differs and opens the AU of ts.
func (a *assembler) advance(ts uint32, arrival time.Time) []AU {
	var out []AU
	if a.started && ts != a.ts {
		out = a.finish()
		a.started = false
	}
	if !a.started {
		a.started = true
		a.ts = ts
		a.nalus, a.size = a.carry, a.carrySize
		a.carry, a.carrySize = nil, 0
		a.hasSlice = false
		a.lost = a.gapNext
		a.over = false
	}
	a.gapNext = false
	a.arrival = arrival
	return out
}

func (a *assembler) finish() []AU {
	if a.fragOn {
		a.lost = true
		a.frag, a.fragOn = nil, false
	}
	if a.over {
		a.oversze++
		return nil
	}
	if !a.hasSlice {
		a.carry, a.carrySize = a.nalus, a.size
		return nil
	}
	return []AU{{Track: a.track, Timestamp: a.ts, NALUs: a.nalus, Lost: a.lost, Arrival: a.arrival}}
}

// Flush closes the pending AU.
func (a *assembler) Flush() []AU {
	if !a.started {
		return nil
	}
	a.started = false
	return a.finish()
}

// Gap implements Depacketizer.Gap.
func (a *assembler) Gap() {
	a.frag, a.fragOn = nil, false
	if a.started {
		a.lost = true
	}
	a.gapNext = true
}

// add appends a copy of nal to the pending AU.
func (a *assembler) add(nal []byte) {
	if len(nal) == 0 || a.isAUD(nal) {
		return
	}
	a.addOwned(append([]byte(nil), nal...))
}

// addOwned appends nal, which the assembler now owns.
func (a *assembler) addOwned(nal []byte) {
	if a.over || len(nal) == 0 || a.isAUD(nal) {
		return
	}
	if a.size+len(nal) > a.maxAU {
		a.over = true
		a.nalus, a.size = nil, 0
		return
	}
	if a.isSlice(nal) {
		a.hasSlice = true
	}
	a.nalus = append(a.nalus, nal)
	a.size += len(nal)
}

// fragStart begins a fragmented NAL unit made of prefix and data.
func (a *assembler) fragStart(prefix, data []byte) {
	if a.fragOn {
		a.lost = true
	}
	a.frag, a.fragOn = nil, false
	if a.over {
		return
	}
	if a.size+len(prefix)+len(data) > a.maxAU {
		a.over = true
		a.nalus, a.size = nil, 0
		return
	}
	a.frag = append(append(make([]byte, 0, len(prefix)+len(data)), prefix...), data...)
	a.fragOn = true
}

// fragNext appends a continuation fragment and completes the NAL unit when
// end is set.
func (a *assembler) fragNext(data []byte, end bool) {
	if !a.fragOn {
		a.lost = true
		return
	}
	if a.size+len(a.frag)+len(data) > a.maxAU {
		a.over = true
		a.nalus, a.size = nil, 0
		a.frag, a.fragOn = nil, false
		return
	}
	a.frag = append(a.frag, data...)
	if end {
		nal := a.frag
		a.frag, a.fragOn = nil, false
		a.addOwned(nal)
	}
}

// aggregate splits an aggregation packet body (16-bit size, NAL unit,
// repeated) into the assembler. A truncated unit marks the AU lost.
func (a *assembler) aggregate(b []byte) {
	for len(b) > 0 {
		if len(b) < 2 {
			a.lost = true
			return
		}
		n := int(b[0])<<8 | int(b[1])
		b = b[2:]
		if n > len(b) {
			a.lost = true
			return
		}
		a.add(b[:n])
		b = b[n:]
	}
}
