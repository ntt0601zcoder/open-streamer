package rtsp

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hbr is the common AAC-hbr format: 13-bit size, 3-bit index.
func hbr(config []byte) *AACFormat {
	return &AACFormat{Config: config, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3}
}

func aacMedia(f *AACFormat, clock uint32) Media { //nolint:unparam // clock varies in future tests.
	return Media{Index: 1, Codec: CodecAAC, Payload: 97, ClockRate: clock, AAC: f}
}

func frameOf(n int, v byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v + byte(i)
	}
	return b
}

// rawAAC builds a payload by hand: headers are (size) pairs with index 0.
func rawAAC(frames ...[]byte) []byte {
	hl := 16 * len(frames)
	out := []byte{byte(hl >> 8), byte(hl)}
	for _, f := range frames {
		v := len(f) << 3 // 13-bit size, 3-bit index 0
		out = append(out, byte(v>>8), byte(v))
	}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func TestAACOneAndThreePerPacket(t *testing.T) {
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x11, 0x90}), 48000), 1<<20)
	require.NoError(t, err)
	f1, f2, f3 := frameOf(10, 1), frameOf(20, 50), frameOf(30, 100)
	out := d.Push(pkt(1, 5000, true, rawAAC(f1)), testT0)
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].Track)
	assert.Equal(t, uint32(5000), out[0].Timestamp)
	assert.Equal(t, [][]byte{f1}, out[0].Frames)
	assert.Empty(t, out[0].NALUs)

	out = d.Push(pkt(2, 6024, true, rawAAC(f1, f2, f3)), testT0)
	require.Len(t, out, 3)
	for i, f := range [][]byte{f1, f2, f3} {
		assert.Equal(t, uint32(6024+i*1024), out[i].Timestamp)
		assert.Equal(t, [][]byte{f}, out[i].Frames)
	}
	assert.Empty(t, d.Flush())
}

func TestAACHESBRTimestamps(t *testing.T) {
	// AAC-LC core at 24 kHz signalled implicitly (config 0x1310 =
	// AOT 2, freq index 6 = 24000), RTP clock 48000: 2048 per AU.
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x13, 0x10}), 48000), 1<<20)
	require.NoError(t, err)
	out := d.Push(pkt(1, 0, true, rawAAC(frameOf(5, 1), frameOf(5, 2))), testT0)
	require.Len(t, out, 2)
	assert.Equal(t, uint32(2048), out[1].Timestamp)
	assert.Equal(t, uint32(2048), samplesPerAU(hbr([]byte{0x13, 0x10}), 48000))
	// explicit SBR signalling: AOT 5, core freq index 6, ext freq index 3 (48000), AOT 2.
	assert.Equal(t, uint32(2048), samplesPerAU(hbr([]byte{0x2b, 0x13, 0x08, 0x00}), 48000))
	// AAC-LC 44100 at its own clock.
	assert.Equal(t, uint32(1024), samplesPerAU(hbr([]byte{0x12, 0x10}), 44100))
	// unparseable config falls back to 1024.
	assert.Equal(t, uint32(1024), samplesPerAU(hbr([]byte{0x12}), 44100))
}

func TestAACFragmented(t *testing.T) {
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x11, 0x90}), 48000), 1<<20)
	require.NoError(t, err)
	frame := frameOf(100, 7)
	hdr := func(size int) []byte { v := size << 3; return []byte{0, 16, byte(v >> 8), byte(v)} }
	var out []AU
	out = append(out, d.Push(pkt(1, 900, false, append(hdr(100), frame[:40]...)), testT0)...)
	out = append(out, d.Push(pkt(2, 900, false, append(hdr(100), frame[40:80]...)), testT0)...)
	assert.Empty(t, out)
	out = append(out, d.Push(pkt(3, 900, true, append(hdr(100), frame[80:]...)), testT0)...)
	require.Len(t, out, 1)
	assert.Equal(t, frame, out[0].Frames[0])
	assert.Equal(t, uint32(900), out[0].Timestamp)
	assert.False(t, out[0].Lost)

	// a lost tail: the fragment is dropped on Gap; the next AU is complete,
	// hence self-delimiting and not Lost.
	out = d.Push(pkt(4, 2000, false, append(hdr(100), frame[:40]...)), testT0)
	d.Gap()
	out = append(out, d.Push(pkt(6, 3024, true, rawAAC(frameOf(9, 1))), testT0)...)
	require.Len(t, out, 1)
	assert.False(t, out[0].Lost)
	assert.Equal(t, frameOf(9, 1), out[0].Frames[0])
}

// TestAACGapMarksPartialOnly checks that after a Gap only an AU started by
// a partial fragment is Lost (its head may be missing); complete AUs are
// self-delimiting.
func TestAACGapMarksPartialOnly(t *testing.T) {
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x11, 0x90}), 48000), 1<<20)
	require.NoError(t, err)
	frame := frameOf(100, 7)
	hdr := func(size int) []byte { v := size << 3; return []byte{0, 16, byte(v >> 8), byte(v)} }

	d.Gap()
	out := d.Push(pkt(3, 1000, true, rawAAC(frameOf(9, 1), frameOf(9, 2))), testT0)
	require.Len(t, out, 2)
	assert.False(t, out[0].Lost, "complete AUs after a gap are intact")
	assert.False(t, out[1].Lost)

	d.Gap()
	assert.Empty(t, d.Push(pkt(5, 3048, false, append(hdr(100), frame[40:80]...)), testT0))
	out = d.Push(pkt(6, 3048, true, append(hdr(100), frame[80:]...)), testT0)
	assert.Empty(t, out, "a fragment shorter than its header cannot complete from the middle")
	out = d.Push(pkt(7, 4072, true, rawAAC(frameOf(9, 3))), testT0)
	require.Len(t, out, 1)
	assert.True(t, out[0].Lost, "the AU after a partial start carries the loss mark")

	d.Gap()
	out = d.Push(pkt(9, 6000, false, append(hdr(100), frame[:40]...)), testT0)
	out = append(out, d.Push(pkt(10, 6000, false, append(hdr(100), frame[40:80]...)), testT0)...)
	out = append(out, d.Push(pkt(11, 6000, true, append(hdr(100), frame[80:]...)), testT0)...)
	require.Len(t, out, 1)
	assert.True(t, out[0].Lost, "a partial start after a gap may lack its head")
}

func TestAACInterleavedDropped(t *testing.T) {
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x11, 0x90}), 48000), 1<<20)
	require.NoError(t, err)
	// two AUs: first with AU-Index 0, second with index-delta 1 (interleaved).
	f1, f2 := frameOf(8, 1), frameOf(9, 2)
	h1 := len(f1) << 3
	h2 := (len(f2) << 3) | 1
	pl := []byte{0, 32, byte(h1 >> 8), byte(h1), byte(h2 >> 8), byte(h2)}
	pl = append(pl, f1...)
	pl = append(pl, f2...)
	out := d.Push(pkt(1, 0, true, pl), testT0)
	require.Len(t, out, 1)
	assert.Equal(t, [][]byte{f1}, out[0].Frames)
	// a non-zero first AU-Index drops that AU.
	h := (len(f1) << 3) | 2
	pl = append([]byte{0, 16, byte(h >> 8), byte(h)}, f1...)
	assert.Empty(t, d.Push(pkt(2, 1024, true, pl), testT0))
}

func TestAACMalformed(t *testing.T) {
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x11, 0x90}), 48000), 1<<20)
	require.NoError(t, err)
	assert.Empty(t, d.Push(pkt(1, 0, true, nil), testT0))
	assert.Empty(t, d.Push(pkt(2, 0, true, []byte{0, 16}), testT0))
	assert.Empty(t, d.Push(pkt(3, 0, true, []byte{0xff, 0xff, 1, 2}), testT0))
	// two headers but short data.
	pl := rawAAC(frameOf(10, 1), frameOf(10, 1))
	assert.Empty(t, d.Push(pkt(4, 0, true, pl[:len(pl)-5]), testT0))
}

func TestAACExtraFields(t *testing.T) {
	// CTS (5), DTS (5), RAP (1) and an 8-bit aux length field.
	f := &AACFormat{
		Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
		CTSDeltaLength: 5, DTSDeltaLength: 5, RAPLength: 1, AuxLength: 8,
	}
	d, err := NewDepacketizer(aacMedia(f, 48000), 1<<20)
	require.NoError(t, err)
	fr := frameOf(12, 3)
	w := &bitWriter{}
	w.write(uint32(len(fr)), 13)
	w.write(0, 3)
	w.write(1, 1) // CTS flag set
	w.write(9, 5)
	w.write(1, 1) // DTS flag set
	w.write(3, 5)
	w.write(1, 1) // RAP
	hbits := w.n
	hdr := append([]byte{byte(hbits >> 8), byte(hbits)}, w.bytes()...)
	aux := &bitWriter{}
	aux.write(16, 8) // 16 bits of aux data
	aux.pad()
	pl := append(hdr, aux.bytes()...)
	pl = append(pl, 0xaa, 0xbb)
	pl = append(pl, fr...)
	out := d.Push(pkt(1, 10, true, pl), testT0)
	require.Len(t, out, 1)
	assert.Equal(t, fr, out[0].Frames[0])
}

func TestPacketizeAACRoundTrip(t *testing.T) {
	for _, f := range []*AACFormat{
		hbr([]byte{0x11, 0x90}),
		{
			Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
			CTSDeltaLength: 5, DTSDeltaLength: 5, RAPLength: 1, AuxLength: 8,
		},
	} {
		d, err := NewDepacketizer(aacMedia(f, 48000), 1<<20)
		require.NoError(t, err)
		frames := [][]byte{frameOf(300, 1), frameOf(2500, 5), frameOf(120, 9), frameOf(1, 3), frameOf(400, 4)}
		pkts := PacketizeAAC(frames, 1000, 1024, 1200, *f)
		require.Greater(t, len(pkts), 3)
		var got []AU
		for i, p := range pkts {
			assert.LessOrEqual(t, len(p.Payload)+12, 1200)
			p.SequenceNumber = uint16(i)
			got = append(got, d.Push(p, testT0)...)
		}
		require.Len(t, got, len(frames))
		for i := range frames {
			assert.Equal(t, frames[i], got[i].Frames[0], "frame %d", i)
			assert.Equal(t, uint32(1000+i*1024), got[i].Timestamp, "frame %d", i)
			assert.False(t, got[i].Lost)
		}
		last := pkts[len(pkts)-1]
		assert.True(t, last.Marker)
	}
}

var _ = rtp.Packet{}

func TestAACTailFragmentAfterGap(t *testing.T) {
	d, err := NewDepacketizer(aacMedia(hbr([]byte{0x11, 0x90}), 48000), 1<<20)
	require.NoError(t, err)
	frame := frameOf(100, 7)
	hdr := func(size int) []byte { v := size << 3; return []byte{0, 16, byte(v >> 8), byte(v)} }
	d.Push(pkt(1, 900, false, append(hdr(100), frame[:40]...)), testT0)
	d.Gap()
	// the tail has the dropped AU's timestamp: ignored, and the next AU is clean.
	assert.Empty(t, d.Push(pkt(3, 900, true, append(hdr(100), frame[80:]...)), testT0))
	out := d.Push(pkt(4, 1924, true, rawAAC(frameOf(9, 1))), testT0)
	require.Len(t, out, 1)
	assert.False(t, out[0].Lost)
}

func TestNewDepacketizerAACFormat(t *testing.T) {
	for _, f := range []*AACFormat{
		{Config: []byte{0x11, 0x90}},
		{Config: []byte{0x11, 0x90}, SizeLength: 33},
		{Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 33},
		{Config: []byte{0x11, 0x90}, SizeLength: 13, AuxLength: -1},
	} {
		_, err := NewDepacketizer(aacMedia(f, 48000), 1)
		require.Error(t, err)
	}
}

func TestAACAuxBounds(t *testing.T) {
	f := &AACFormat{Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, AuxLength: 32}
	d, err := NewDepacketizer(aacMedia(f, 48000), 1<<20)
	require.NoError(t, err)
	pl := []byte{0, 16, 0x00, 0x08, 0xff, 0xff, 0xff, 0xff, 1}
	assert.NotPanics(t, func() { assert.Empty(t, d.Push(pkt(1, 0, true, pl), testT0)) })
}

func TestSamplesPerAUHighClock(t *testing.T) {
	assert.Equal(t, uint32(1024*8000000/44100), samplesPerAU(hbr([]byte{0x12, 0x10}), 8000000))
}
