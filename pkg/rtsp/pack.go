package rtsp

const defaultPackMTU = 1200

// Packetizer turns the NAL units of one access unit into RTP payloads.
type Packetizer interface {
	// Packetize returns the RTP payloads of one access unit (NAL units
	// without start codes) in order; the caller sets the marker bit on the
	// last one. AUD and filler NAL units are skipped. Every payload is a
	// fresh allocation of at most the MTU.
	Packetize(nalus [][]byte) [][]byte
}

type h264Packetizer struct{ mtu int }

type h265Packetizer struct{ mtu int }

// NewH264Packetizer returns an RFC 6184 packetizer (packetization-mode 1:
// single NAL unit and FU-A). mtu is the RTP payload size.
func NewH264Packetizer(mtu int) Packetizer {
	if mtu < 4 {
		mtu = defaultPackMTU
	}
	return &h264Packetizer{mtu: mtu}
}

// NewH265Packetizer returns an RFC 7798 packetizer (single NAL unit and FU,
// no DONL). mtu is the RTP payload size.
func NewH265Packetizer(mtu int) Packetizer {
	if mtu < 4 {
		mtu = defaultPackMTU
	}
	return &h265Packetizer{mtu: mtu}
}

// Packetize implements Packetizer.
func (p *h264Packetizer) Packetize(nalus [][]byte) [][]byte {
	var out [][]byte
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		typ := n[0] & 0x1f
		if typ == 9 || typ == 12 {
			continue
		}
		if len(n) <= p.mtu {
			out = append(out, append([]byte(nil), n...))
			continue
		}
		ind := n[0]&0xe0 | 28
		body := n[1:]
		size := p.mtu - 2
		for first := true; len(body) > 0; first = false {
			c := min(size, len(body))
			h := typ
			if first {
				h |= 0x80
			}
			if c == len(body) {
				h |= 0x40
			}
			pl := make([]byte, 2+c)
			pl[0], pl[1] = ind, h
			copy(pl[2:], body[:c])
			out = append(out, pl)
			body = body[c:]
		}
	}
	return out
}

// Packetize implements Packetizer.
func (p *h265Packetizer) Packetize(nalus [][]byte) [][]byte {
	var out [][]byte
	for _, n := range nalus {
		if len(n) < 2 {
			continue
		}
		typ := (n[0] >> 1) & 0x3f
		if typ == 35 || typ == 38 {
			continue
		}
		if len(n) <= p.mtu {
			out = append(out, append([]byte(nil), n...))
			continue
		}
		b0 := n[0]&0x81 | 49<<1
		b1 := n[1]
		body := n[2:]
		size := p.mtu - 3
		for first := true; len(body) > 0; first = false {
			c := min(size, len(body))
			h := typ
			if first {
				h |= 0x80
			}
			if c == len(body) {
				h |= 0x40
			}
			pl := make([]byte, 3+c)
			pl[0], pl[1], pl[2] = b0, b1, h
			copy(pl[3:], body[:c])
			out = append(out, pl)
			body = body[c:]
		}
	}
	return out
}
