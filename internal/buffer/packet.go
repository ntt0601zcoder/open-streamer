package buffer

import "github.com/ntt0601zcoder/open-streamer/internal/domain"

// Packet is the Buffer Hub wire format. Exactly one of TS or AV should be set per write:
//   - TS: raw MPEG-TS chunk (transcoder output, push ingest, or legacy passthrough).
//   - AV: one elementary-stream access unit (ingest pull after demux or native ES readers).
type Packet struct {
	TS []byte
	AV *domain.AVPacket
}

// TSPacket wraps a raw MPEG-TS chunk.
func TSPacket(b []byte) Packet { return Packet{TS: b} }

func clonePacket(p Packet) Packet {
	out := Packet{}
	if len(p.TS) > 0 {
		out.TS = append([]byte(nil), p.TS...)
	}
	if p.AV != nil {
		out.AV = p.AV.Clone()
	}
	return out
}

func (p Packet) empty() bool {
	return len(p.TS) == 0 && (p.AV == nil || len(p.AV.Data) == 0)
}
