package pull

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helpers ---------------------------------------------------------------------

// buildRTPPacket produces a minimal RTP packet (fixed 12-byte header) plus payload.
func buildRTPPacket(payload []byte) []byte {
	hdr := make([]byte, 0, 12+len(payload))
	hdr = append(hdr,
		0x80, 0x21, // V=2, P=0, X=0, CC=0, M=0, PT=33
		0x00, 0x01, // seq
		0x00, 0x00, 0x00, 0x00, // timestamp
		0x00, 0x00, 0x00, 0x00, // SSRC
	)
	return append(hdr, payload...)
}

// buildRTPPacketWithCSRC prepends cc CSRC identifiers (each 4 bytes).
func buildRTPPacketWithCSRC(cc int, payload []byte) []byte {
	b0 := byte(0x80 | (cc & 0x0F)) // V=2, CC=cc
	hdr := []byte{
		b0, 0x21,
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	for range cc {
		hdr = append(hdr, 0x00, 0x00, 0x00, 0x00)
	}
	return append(hdr, payload...)
}

// buildRTPPacketWithExt adds a 4-byte extension header + extWords × 4 bytes.
func buildRTPPacketWithExt(extWords int, payload []byte) []byte {
	hdr := []byte{
		0x90, 0x21, // V=2, X=1
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	hdr = append(hdr, 0xAB, 0xCD) // profile-defined
	hdr = append(hdr, byte(extWords>>8), byte(extWords&0xFF))
	for range extWords {
		hdr = append(hdr, 0x00, 0x00, 0x00, 0x00)
	}
	return append(hdr, payload...)
}

// Tests -----------------------------------------------------------------------

func TestStripRTPHeader_PlainRTP(t *testing.T) {
	t.Parallel()
	ts := []byte{0x47, 0x01, 0x02, 0x03, 0x04, 0x05}
	pkt := buildRTPPacket(ts)
	got := stripRTPHeader(pkt)
	require.NotNil(t, got)
	assert.Equal(t, ts, got)
}

func TestStripRTPHeader_TooShort(t *testing.T) {
	t.Parallel()
	assert.Nil(t, stripRTPHeader([]byte{0x80, 0x21, 0x00}))
	assert.Nil(t, stripRTPHeader(nil))
}

func TestStripRTPHeader_WrongVersion(t *testing.T) {
	t.Parallel()
	// Version 1 (0x40) instead of 2 (0x80) — must be rejected.
	pkt := []byte{0x40, 0x21, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x47}
	assert.Nil(t, stripRTPHeader(pkt))
}

func TestStripRTPHeader_NonMPEGTSPayload(t *testing.T) {
	t.Parallel()
	// Payload does not start with 0x47 sync byte → rejected.
	pkt := buildRTPPacket([]byte{0xFF, 0x01, 0x02})
	assert.Nil(t, stripRTPHeader(pkt))
}

func TestStripRTPHeader_WithCSRC(t *testing.T) {
	t.Parallel()
	ts := []byte{0x47, 0x0A, 0x0B}
	// CC=3 → header is 12 + 3*4 = 24 bytes.
	pkt := buildRTPPacketWithCSRC(3, ts)
	got := stripRTPHeader(pkt)
	require.NotNil(t, got)
	assert.Equal(t, ts, got)
}

func TestStripRTPHeader_WithExtension(t *testing.T) {
	t.Parallel()
	ts := []byte{0x47, 0xDE, 0xAD, 0xBE, 0xEF}
	// 2 extension words = 8 extra bytes + 4-byte ext header → 12+4+8 = 24.
	pkt := buildRTPPacketWithExt(2, ts)
	got := stripRTPHeader(pkt)
	require.NotNil(t, got)
	assert.Equal(t, ts, got)
}

func TestStripRTPHeader_ExtensionTruncated(t *testing.T) {
	t.Parallel()
	// X=1 but only 13 bytes total → extension header can't fit.
	pkt := []byte{0x90, 0x21, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0x47}
	assert.Nil(t, stripRTPHeader(pkt))
}

func TestStripRTPHeader_HeaderExceedsPacket(t *testing.T) {
	t.Parallel()
	// CC=5 → header needs 12+20=32 bytes; give only 14.
	pkt := make([]byte, 14)
	pkt[0] = 0x85 // V=2, CC=5
	assert.Nil(t, stripRTPHeader(pkt))
}
