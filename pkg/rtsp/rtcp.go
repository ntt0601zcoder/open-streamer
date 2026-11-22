package rtsp

import (
	"encoding/binary"
	"time"
)

const (
	rtcpSR = 200
	rtcpRR = 201

	rtcpHeaderLen  = 4
	srBodyLen      = 24 // SSRC and sender info
	rrBlockLen     = 24
	maxRRBlocks    = 31
	ntpToUnixSecs  = 2208988800
	maxLost24      = 1<<24 - 1
	rtcpVersionTop = 2
)

// SenderReport is the part of an RTCP sender report used for alignment.
type SenderReport struct {
	SSRC uint32
	NTP  time.Time
	RTP  uint32
}

// ParseSenderReports returns the sender reports in a compound RTCP packet
// (RFC 3550 section 6.4.1). The walk stops at the first element that is not
// version 2 or runs past the buffer; elements that are too short to hold a
// sender report are skipped.
func ParseSenderReports(rtcp []byte) []SenderReport {
	var out []SenderReport
	for len(rtcp) >= rtcpHeaderLen {
		if rtcp[0]>>6 != rtcpVersionTop {
			break
		}
		n := (int(binary.BigEndian.Uint16(rtcp[2:])) + 1) * 4
		if n > len(rtcp) {
			break
		}
		if rtcp[1] == rtcpSR && n >= rtcpHeaderLen+srBodyLen {
			b := rtcp[rtcpHeaderLen:]
			sec := int64(binary.BigEndian.Uint32(b[4:])) - ntpToUnixSecs
			frac := uint64(binary.BigEndian.Uint32(b[8:]))
			nsec := int64((frac * 1_000_000_000) >> 32)
			out = append(out, SenderReport{
				SSRC: binary.BigEndian.Uint32(b[0:]),
				NTP:  time.Unix(sec, nsec),
				RTP:  binary.BigEndian.Uint32(b[12:]),
			})
		}
		rtcp = rtcp[n:]
	}
	return out
}

// ReportBlock is one reception report block (RFC 3550 section 6.4.1).
type ReportBlock struct {
	SSRC         uint32
	FractionLost uint8
	Lost         uint32 // cumulative, clamped to 24 bits
	HighestSeq   uint32 // extended highest sequence number
	Jitter       uint32
	LSR, DLSR    uint32
}

// ReceiverReport builds an RTCP receiver report from ssrc with the given
// blocks; blocks beyond 31 are ignored.
func ReceiverReport(ssrc uint32, blocks []ReportBlock) []byte {
	if len(blocks) > maxRRBlocks {
		blocks = blocks[:maxRRBlocks]
	}
	n := 8 + rrBlockLen*len(blocks)
	b := make([]byte, n)
	b[0] = rtcpVersionTop<<6 | byte(len(blocks))
	b[1] = rtcpRR
	binary.BigEndian.PutUint16(b[2:], uint16(n/4-1))
	binary.BigEndian.PutUint32(b[4:], ssrc)
	for i, bl := range blocks {
		o := b[8+i*rrBlockLen:]
		lost := min(bl.Lost, maxLost24)
		binary.BigEndian.PutUint32(o[0:], bl.SSRC)
		binary.BigEndian.PutUint32(o[4:], uint32(bl.FractionLost)<<24|lost)
		binary.BigEndian.PutUint32(o[8:], bl.HighestSeq)
		binary.BigEndian.PutUint32(o[12:], bl.Jitter)
		binary.BigEndian.PutUint32(o[16:], bl.LSR)
		binary.BigEndian.PutUint32(o[20:], bl.DLSR)
	}
	return b
}

const (
	rtcpSDES = 202
	rtcpBye  = 203

	sdesCNAME  = 1
	maxCNAME   = 255
	ntpFracOne = 1 << 32
)

// BuildSenderReport builds an RTCP sender report without report blocks
// (RFC 3550 section 6.4.1). ntp is converted to 32.32 fixed point seconds
// since 1900.
func BuildSenderReport(ssrc uint32, ntp time.Time, rtpTS, packets, octets uint32) []byte {
	b := make([]byte, rtcpHeaderLen+srBodyLen)
	b[0] = rtcpVersionTop << 6
	b[1] = rtcpSR
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)/4-1))
	binary.BigEndian.PutUint32(b[4:], ssrc)
	sec := uint32(ntp.Unix() + ntpToUnixSecs)
	frac := uint32((uint64(ntp.Nanosecond()) * ntpFracOne) / 1_000_000_000)
	binary.BigEndian.PutUint32(b[8:], sec)
	binary.BigEndian.PutUint32(b[12:], frac)
	binary.BigEndian.PutUint32(b[16:], rtpTS)
	binary.BigEndian.PutUint32(b[20:], packets)
	binary.BigEndian.PutUint32(b[24:], octets)
	return b
}

// BuildBye builds the compound packet that ends a session: sr when non-nil
// (else an empty receiver report from ssrc), an SDES chunk with one CNAME
// item padded to 32 bits, and a BYE for ssrc. A cname longer than 255 bytes
// is truncated.
func BuildBye(ssrc uint32, cname string, sr []byte) []byte {
	if len(cname) > maxCNAME {
		cname = cname[:maxCNAME]
	}
	b := make([]byte, 0, len(sr)+8+16+len(cname)+8)
	if sr != nil {
		b = append(b, sr...)
	} else {
		b = append(b, ReceiverReport(ssrc, nil)...)
	}
	// SDES: header, SSRC, CNAME item, a null item end, padding to 32 bits.
	chunk := 4 + 2 + len(cname) + 1
	chunk += (4 - chunk%4) % 4
	start := len(b)
	b = append(b, make([]byte, 4+chunk)...)
	s := b[start:]
	s[0] = rtcpVersionTop<<6 | 1
	s[1] = rtcpSDES
	binary.BigEndian.PutUint16(s[2:], uint16((4+chunk)/4-1))
	binary.BigEndian.PutUint32(s[4:], ssrc)
	s[8] = sdesCNAME
	s[9] = byte(len(cname))
	copy(s[10:], cname)
	// BYE.
	b = append(b, rtcpVersionTop<<6|1, rtcpBye, 0, 1, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[len(b)-4:], ssrc)
	return b
}

// ParseReceiverReports returns the sender SSRC of every SR and RR in a
// compound RTCP packet. The walk stops at the first element that is not
// version 2 or runs past the buffer.
func ParseReceiverReports(rtcp []byte) []uint32 {
	var out []uint32
	for len(rtcp) >= rtcpHeaderLen {
		if rtcp[0]>>6 != rtcpVersionTop {
			break
		}
		n := (int(binary.BigEndian.Uint16(rtcp[2:])) + 1) * 4
		if n > len(rtcp) {
			break
		}
		if (rtcp[1] == rtcpSR || rtcp[1] == rtcpRR) && n >= rtcpHeaderLen+4 {
			out = append(out, binary.BigEndian.Uint32(rtcp[rtcpHeaderLen:]))
		}
		rtcp = rtcp[n:]
	}
	return out
}
