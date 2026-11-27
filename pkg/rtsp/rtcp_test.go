package rtsp

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rtcpPkt(pt byte, rc byte, body []byte) []byte {
	b := make([]byte, 0, 4+len(body))
	b = append(b, 0x80|rc, pt, 0, 0)
	b = append(b, body...)
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)/4-1))
	return b
}

func srBody(ssrc uint32, sec, frac, rtpTS uint32) []byte {
	b := make([]byte, 24)
	binary.BigEndian.PutUint32(b[0:], ssrc)
	binary.BigEndian.PutUint32(b[4:], sec)
	binary.BigEndian.PutUint32(b[8:], frac)
	binary.BigEndian.PutUint32(b[12:], rtpTS)
	binary.BigEndian.PutUint32(b[16:], 10) // packet count
	binary.BigEndian.PutUint32(b[20:], 99) // octet count
	return b
}

func TestParseSenderReports(t *testing.T) {
	const ntp1970 = 2208988800
	rr := rtcpPkt(201, 0, []byte{0, 0, 0, 5})
	sr := rtcpPkt(200, 0, srBody(0xdeadbeef, ntp1970+1700000000, 1<<31, 123456))
	sdes := rtcpPkt(202, 1, []byte{0, 0, 0, 1, 1, 3, 'a', 'b', 'c', 0, 0, 0})
	sr2 := rtcpPkt(200, 0, srBody(7, ntp1970+5, 0, 9))
	got := ParseSenderReports(append(append(append(append([]byte{}, rr...), sr...), sdes...), sr2...))
	require.Len(t, got, 2)
	assert.Equal(t, uint32(0xdeadbeef), got[0].SSRC)
	assert.Equal(t, uint32(123456), got[0].RTP)
	assert.True(t, got[0].NTP.Equal(time.Unix(1700000000, 500_000_000)))
	assert.Equal(t, uint32(7), got[1].SSRC)
	assert.True(t, got[1].NTP.Equal(time.Unix(5, 0)))

	// SR with a report block still parses.
	blk := append(srBody(1, ntp1970, 0, 1), make([]byte, 24)...)
	assert.Len(t, ParseSenderReports(rtcpPkt(200, 1, blk)), 1)
}

func TestParseSenderReportsMalformed(t *testing.T) {
	const ntp1970 = 2208988800
	sr := rtcpPkt(200, 0, srBody(1, ntp1970+1, 0, 2))
	assert.Empty(t, ParseSenderReports(nil))
	assert.Empty(t, ParseSenderReports([]byte{0x80, 200}))
	assert.Empty(t, ParseSenderReports(sr[:20])) // truncated
	bad := append([]byte{}, sr...)
	bad[0] = 0x40 // version 1
	assert.Empty(t, ParseSenderReports(bad))
	long := append([]byte{}, sr...)
	binary.BigEndian.PutUint16(long[2:], 200) // length beyond the buffer
	assert.Empty(t, ParseSenderReports(long))
	// too short for sender info: skipped, the next SR still found.
	short := rtcpPkt(200, 0, []byte{0, 0, 0, 1})
	got := ParseSenderReports(append(short, sr...))
	require.Len(t, got, 1)
	assert.Equal(t, uint32(1), got[0].SSRC)
	// a valid SR followed by garbage is kept.
	assert.Len(t, ParseSenderReports(append(append([]byte{}, sr...), 1, 2, 3)), 1)
}

func TestReceiverReport(t *testing.T) {
	b := ReceiverReport(0x01020304, []ReportBlock{
		{SSRC: 0xaabbccdd, FractionLost: 25, Lost: 0x123456, HighestSeq: 0x00010002, Jitter: 77, LSR: 0x11223344, DLSR: 0x55667788},
	})
	require.Len(t, b, 8+24)
	assert.Equal(t, byte(0x81), b[0]) // V=2, P=0, RC=1
	assert.Equal(t, byte(201), b[1])
	assert.Equal(t, uint16(7), binary.BigEndian.Uint16(b[2:])) // 32 bytes / 4 - 1
	assert.Equal(t, uint32(0x01020304), binary.BigEndian.Uint32(b[4:]))
	assert.Equal(t, uint32(0xaabbccdd), binary.BigEndian.Uint32(b[8:]))
	assert.Equal(t, byte(25), b[12])
	assert.Equal(t, []byte{0x12, 0x34, 0x56}, b[13:16])
	assert.Equal(t, uint32(0x00010002), binary.BigEndian.Uint32(b[16:]))
	assert.Equal(t, uint32(77), binary.BigEndian.Uint32(b[20:]))
	assert.Equal(t, uint32(0x11223344), binary.BigEndian.Uint32(b[24:]))
	assert.Equal(t, uint32(0x55667788), binary.BigEndian.Uint32(b[28:]))

	empty := ReceiverReport(9, nil)
	assert.Equal(t, []byte{0x80, 201, 0, 1, 0, 0, 0, 9}, empty)

	many := ReceiverReport(1, make([]ReportBlock, 40))
	assert.Equal(t, byte(0x80|31), many[0])
	assert.Len(t, many, 8+31*24)
	assert.Equal(t, uint16(1+6*31), binary.BigEndian.Uint16(many[2:]))

	// the cumulative loss is clamped to 24 bits.
	c := ReceiverReport(1, []ReportBlock{{Lost: 1 << 30}})
	assert.Equal(t, []byte{0, 0xff, 0xff, 0xff}, c[12:16])
}

func TestSenderReportRoundTrip(t *testing.T) {
	ntp := time.Unix(1_700_000_000, 250_000_000)
	b := BuildSenderReport(0xdeadbeef, ntp, 12345, 10, 2000)
	if len(b) != 28 {
		t.Fatalf("len %d", len(b))
	}
	srs := ParseSenderReports(b)
	if len(srs) != 1 || srs[0].SSRC != 0xdeadbeef || srs[0].RTP != 12345 {
		t.Fatalf("%+v", srs)
	}
	if d := srs[0].NTP.Sub(ntp); d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("ntp %v vs %v", srs[0].NTP, ntp)
	}
	if binary.BigEndian.Uint32(b[20:]) != 10 || binary.BigEndian.Uint32(b[24:]) != 2000 {
		t.Fatalf("counts % x", b)
	}
}

func TestByeCompound(t *testing.T) {
	sr := BuildSenderReport(1, time.Unix(1_700_000_000, 0), 5, 1, 2)
	b := BuildBye(1, "os", sr)
	if !bytes.HasPrefix(b, sr) {
		t.Fatalf("sr not first: % x", b)
	}
	rest := b[len(sr):]
	// SDES: header 4 + SSRC 4 + CNAME item (2+2) + terminator, padded to 4.
	if rest[0] != 0x81 || rest[1] != 202 {
		t.Fatalf("sdes % x", rest)
	}
	n := (int(binary.BigEndian.Uint16(rest[2:])) + 1) * 4
	if n%4 != 0 || rest[8] != 1 || rest[9] != 2 || string(rest[10:12]) != "os" || rest[12] != 0 {
		t.Fatalf("sdes body % x", rest[:n])
	}
	bye := rest[n:]
	if len(bye) != 8 || bye[0] != 0x81 || bye[1] != 203 || binary.BigEndian.Uint16(bye[2:]) != 1 ||
		binary.BigEndian.Uint32(bye[4:]) != 1 {
		t.Fatalf("bye % x", bye)
	}
	// No SR: empty RR first.
	b = BuildBye(9, "abc", nil)
	if b[0] != 0x80 || b[1] != 201 || binary.BigEndian.Uint32(b[4:]) != 9 || len(b) < 8 {
		t.Fatalf("rr % x", b)
	}
	if got := ParseReceiverReports(b); len(got) != 1 || got[0] != 9 {
		t.Fatalf("senders %v", got)
	}
	// Odd-length CNAME is padded to 32 bits.
	if len(b)%4 != 0 {
		t.Fatalf("len %d", len(b))
	}
}

func TestParseReceiverReports(t *testing.T) {
	sr := BuildSenderReport(11, time.Unix(1_700_000_000, 0), 5, 1, 2)
	rr := ReceiverReport(22, []ReportBlock{{SSRC: 1}})
	got := ParseReceiverReports(append(append([]byte{}, rr...), sr...))
	if len(got) != 2 || got[0] != 22 || got[1] != 11 {
		t.Fatalf("%v", got)
	}
	// Truncated and hostile inputs stop the walk without panicking.
	if got := ParseReceiverReports(rr[:6]); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := ParseReceiverReports([]byte{0x80, 201, 0xff, 0xff, 0, 0, 0, 1}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := ParseReceiverReports(nil); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
