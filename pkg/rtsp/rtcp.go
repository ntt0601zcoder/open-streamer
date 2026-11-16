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
