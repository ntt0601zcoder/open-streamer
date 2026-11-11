package mpegts

// PacketSize is the size of an MPEG-TS packet in bytes.
const PacketSize = 188

const (
	syncByte = 0x47

	// maxAFLenWithPayload is the largest adaptation_field_length when a
	// payload follows: 188 - 4 header bytes - 1 length byte - 1 payload byte.
	maxAFLenWithPayload = 182
	// maxAFLenOnly is the largest adaptation_field_length without payload.
	maxAFLenOnly = 183
	// pcrFieldLen is the size of the AF flags byte plus the 6-byte PCR.
	pcrFieldLen = 7
)

// Packet is one parsed transport stream packet.
type Packet struct {
	PID           uint16
	PUSI          bool   // payload_unit_start_indicator
	CC            uint8  // continuity_counter
	HasPayload    bool   // adaptation_field_control has the payload bit set
	Discontinuity bool   // adaptation field discontinuity_indicator
	RandomAccess  bool   // adaptation field random_access_indicator
	PCR           int64  // PCR base (90 kHz, 33 bits); -1 when absent
	Payload       []byte // valid until the next Next call
}

// parsePacket parses the 4-byte header and the adaptation field of b, which
// is exactly PacketSize bytes starting with the sync byte. ok is false for a
// packet that must be dropped: transport_error_indicator set, the reserved
// adaptation_field_control value '00', or an adaptation field that does not
// fit in the packet.
func parsePacket(b []byte) (p Packet, ok bool) {
	if b[1]&0x80 != 0 {
		return Packet{}, false
	}
	p = Packet{
		PID:  uint16(b[1]&0x1F)<<8 | uint16(b[2]),
		PUSI: b[1]&0x40 != 0,
		CC:   b[3] & 0x0F,
		PCR:  -1,
	}
	afc := b[3] >> 4 & 0x03
	if afc == 0 {
		return Packet{}, false
	}
	p.HasPayload = afc&0x01 != 0
	start := 4
	if afc&0x02 != 0 {
		afLen := int(b[4])
		limit := maxAFLenOnly
		if p.HasPayload {
			limit = maxAFLenWithPayload
		}
		if afLen > limit {
			return Packet{}, false
		}
		if afLen > 0 {
			flags := b[5]
			p.Discontinuity = flags&0x80 != 0
			p.RandomAccess = flags&0x40 != 0
			if flags&0x10 != 0 {
				if afLen < pcrFieldLen {
					return Packet{}, false
				}
				p.PCR = int64(b[6])<<25 | int64(b[7])<<17 | int64(b[8])<<9 |
					int64(b[9])<<1 | int64(b[10])>>7
			}
		}
		start = 5 + afLen
	}
	if p.HasPayload {
		p.Payload = b[start:PacketSize]
	}
	return p, true
}
