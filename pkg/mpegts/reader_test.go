package mpegts

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readH264AAC returns h264_aac.ts, 321480 bytes = 1710 packets.
func readH264AAC(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	return raw
}

// gotPacket is the part of a Packet the tests compare, with the payload copied
// out of the reader's buffer.
type gotPacket struct {
	PID     uint16
	CC      uint8
	PUSI    bool
	Payload []byte
}

func readAll(t *testing.T, r *Reader) []gotPacket {
	t.Helper()
	var out []gotPacket
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, gotPacket{
			PID:     p.PID,
			CC:      p.CC,
			PUSI:    p.PUSI,
			Payload: append([]byte(nil), p.Payload...),
		})
	}
}

// buildPacket hand-builds one 188-byte packet. af, when non-nil, is the
// adaptation field body after the length byte; payload fills the rest
// (padded with 0xFF).
func buildPacket(pid uint16, cc uint8, pusi bool, af []byte, payload []byte) []byte {
	b := make([]byte, PacketSize)
	for i := range b {
		b[i] = 0xFF
	}
	b[0] = 0x47
	b[1] = byte(pid>>8) & 0x1F
	if pusi {
		b[1] |= 0x40
	}
	b[2] = byte(pid)
	afc := byte(0)
	if payload != nil {
		afc |= 1
	}
	i := 4
	if af != nil {
		afc |= 2
		b[4] = byte(len(af))
		copy(b[5:], af)
		i = 5 + len(af)
	}
	b[3] = afc<<4 | cc&0x0F
	copy(b[i:], payload)
	return b
}

func TestReaderFixture(t *testing.T) {
	raw := readH264AAC(t)
	require.Equal(t, 321480, len(raw))
	r := NewReader(bytes.NewReader(raw))
	got := readAll(t, r)
	assert.Len(t, got, len(raw)/PacketSize)
	st := r.Stats()
	assert.Equal(t, uint64(len(raw)/PacketSize), st.Packets)
	assert.Zero(t, st.Resyncs)
	assert.Zero(t, st.SkippedBytes)
	assert.Zero(t, st.Errored)
}

func TestReaderOneByte(t *testing.T) {
	raw := readH264AAC(t)
	want := readAll(t, NewReader(bytes.NewReader(raw)))
	r := NewReader(iotest.OneByteReader(bytes.NewReader(raw)))
	got := readAll(t, r)
	assert.Equal(t, want, got)
	assert.Zero(t, r.Stats().Resyncs)
}

func TestReaderResyncAfterGarbage(t *testing.T) {
	raw := readH264AAC(t)
	want := readAll(t, NewReader(bytes.NewReader(raw)))

	in := make([]byte, 1000, 1000+len(raw))
	rng := rand.New(rand.NewSource(1))
	_, _ = rng.Read(in)
	for i, b := range in {
		if b == 0x47 {
			in[i] = 0x00
		}
	}
	r := NewReader(bytes.NewReader(append(in, raw...)))
	got := readAll(t, r)
	assert.Equal(t, want, got)
	st := r.Stats()
	assert.Equal(t, uint64(1), st.Resyncs)
	assert.Equal(t, uint64(1000), st.SkippedBytes)
}

func TestReaderFalseSync(t *testing.T) {
	raw := readH264AAC(t)
	want := readAll(t, NewReader(bytes.NewReader(raw)))

	// A lone 0x47 at 10, and a pair at 20 and 208 whose third byte (396)
	// is not 0x47: neither may be taken as the packet start.
	in := make([]byte, 500, 500+len(raw))
	in[10] = 0x47
	in[20] = 0x47
	in[208] = 0x47
	r := NewReader(bytes.NewReader(append(in, raw...)))
	got := readAll(t, r)
	assert.Equal(t, want, got)
	st := r.Stats()
	assert.Equal(t, uint64(1), st.Resyncs)
	assert.Equal(t, uint64(500), st.SkippedBytes)
}

func TestReaderLostBytes(t *testing.T) {
	raw := readH264AAC(t)
	n := len(raw) / PacketSize
	cut := 100*PacketSize + 60
	lossy := append(append([]byte(nil), raw[:cut]...), raw[cut+50:]...)

	r := NewReader(bytes.NewReader(lossy))
	got := readAll(t, r)
	require.Len(t, got, n-1)
	st := r.Stats()
	assert.Equal(t, uint64(1), st.Resyncs)
	assert.Equal(t, uint64(PacketSize-50), st.SkippedBytes)

	// Packets after the damage are intact.
	want := readAll(t, NewReader(bytes.NewReader(raw)))
	assert.Equal(t, want[:100], got[:100])
	assert.Equal(t, want[102:], got[101:])
}

func TestReaderTruncatedTail(t *testing.T) {
	raw := readH264AAC(t)
	n := len(raw) / PacketSize
	for _, cut := range []int{1, 100, 187} {
		r := NewReader(bytes.NewReader(raw[:len(raw)-cut]))
		got := readAll(t, r)
		assert.Len(t, got, n-1, "cut %d", cut)
		_, err := r.Next()
		assert.ErrorIs(t, err, io.EOF)
	}
}

func TestReaderShortInput(t *testing.T) {
	// Fewer than three packets: the lock accepts positions past EOF.
	p := buildPacket(0x100, 0, true, nil, []byte{1, 2, 3})
	for _, in := range [][]byte{nil, {0x47}, p, append(p, p...), append(append([]byte{0, 0}, p...), 0x47)} {
		r := NewReader(bytes.NewReader(in))
		got := readAll(t, r)
		assert.Len(t, got, len(in)/PacketSize)
	}
}

func TestReaderErrorIndicator(t *testing.T) {
	good := buildPacket(0x100, 0, true, nil, []byte{1})
	bad := buildPacket(0x100, 1, false, nil, []byte{2})
	bad[1] |= 0x80 // transport_error_indicator
	last := buildPacket(0x100, 2, false, nil, []byte{3})
	in := make([]byte, 0, 3*PacketSize)
	in = append(in, good...)
	in = append(in, bad...)
	in = append(in, last...)

	r := NewReader(bytes.NewReader(in))
	got := readAll(t, r)
	require.Len(t, got, 2)
	assert.Equal(t, uint8(0), got[0].CC)
	assert.Equal(t, uint8(2), got[1].CC)
	st := r.Stats()
	assert.Equal(t, uint64(1), st.Errored)
	assert.Equal(t, uint64(2), st.Packets)
}

func TestReaderBadAdaptationField(t *testing.T) {
	ok := buildPacket(0x100, 0, false, nil, []byte{1})

	afTooLong := buildPacket(0x100, 1, false, []byte{0}, []byte{2})
	afTooLong[4] = 183 // AF + payload allows at most 182

	afOnlyTooLong := buildPacket(0x100, 2, false, []byte{0}, nil)
	afOnlyTooLong[4] = 184

	pcrShort := buildPacket(0x100, 3, false, []byte{0x10, 0, 0}, []byte{3}) // PCR flag, 2 bytes

	reserved := buildPacket(0x100, 4, false, nil, []byte{4})
	reserved[3] &^= 0x30 // adaptation_field_control '00'

	var in []byte
	for _, p := range [][]byte{ok, afTooLong, afOnlyTooLong, pcrShort, reserved, ok} {
		in = append(in, p...)
	}
	r := NewReader(bytes.NewReader(in))
	got := readAll(t, r)
	assert.Len(t, got, 2)
	assert.Equal(t, uint64(4), r.Stats().Errored)
}

func TestReaderAdaptationFlags(t *testing.T) {
	in := buildPacket(0x101, 7, true, []byte{0xC0}, []byte{9, 8})
	in = append(in, buildPacket(0x101, 8, false, []byte{}, []byte{7})...)    // zero-length AF
	in = append(in, buildPacket(0x1FFF, 0, false, nil, []byte{})...)         // payload only
	in = append(in, buildPacket(0x101, 9, false, make([]byte, 183), nil)...) // AF only
	r := NewReader(bytes.NewReader(in))

	p, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, uint16(0x101), p.PID)
	assert.True(t, p.PUSI)
	assert.Equal(t, uint8(7), p.CC)
	assert.True(t, p.HasPayload)
	assert.True(t, p.Discontinuity)
	assert.True(t, p.RandomAccess)
	assert.Equal(t, int64(-1), p.PCR)
	assert.Len(t, p.Payload, PacketSize-6)
	assert.Equal(t, []byte{9, 8}, p.Payload[:2])

	p, err = r.Next()
	require.NoError(t, err)
	assert.False(t, p.Discontinuity)
	assert.False(t, p.RandomAccess)
	assert.Len(t, p.Payload, PacketSize-5)

	p, err = r.Next()
	require.NoError(t, err)
	assert.Equal(t, uint16(0x1FFF), p.PID)
	assert.Len(t, p.Payload, PacketSize-4)

	p, err = r.Next()
	require.NoError(t, err)
	assert.False(t, p.HasPayload)
	assert.Empty(t, p.Payload)
}

func TestReaderPCR(t *testing.T) {
	for _, base := range []int64{0, 1, 123456789, 1<<33 - 1, 0x155555555} {
		af := make([]byte, 7)
		af[0] = 0x10 // PCR_flag
		af[1] = byte(base >> 25)
		af[2] = byte(base >> 17)
		af[3] = byte(base >> 9)
		af[4] = byte(base >> 1)
		af[5] = byte(base<<7) | 0x7F // reserved bits and extension MSB set
		af[6] = 0xFF                 // extension LSBs
		r := NewReader(bytes.NewReader(buildPacket(0x100, 0, false, af, []byte{1})))
		p, err := r.Next()
		require.NoError(t, err)
		assert.Equal(t, base, p.PCR)
		assert.Equal(t, PacketSize-4-8, len(p.Payload))
	}
}

func TestReaderReadError(t *testing.T) {
	boom := errors.New("boom")
	_, err := NewReader(iotest.ErrReader(boom)).Next()
	assert.Same(t, boom, err)

	raw := readH264AAC(t)
	r := NewReader(io.MultiReader(bytes.NewReader(raw[:5*PacketSize]), iotest.ErrReader(boom)))
	for i := 0; i < 5; i++ {
		_, err := r.Next()
		require.NoError(t, err)
	}
	_, err = r.Next()
	assert.Same(t, boom, err)
	_, err = r.Next()
	assert.Same(t, boom, err, "the error is sticky")

	// Data and an error from the same Read: the data is used first.
	r = NewReader(&dataErrReader{data: raw[:3*PacketSize], err: boom})
	for i := 0; i < 3; i++ {
		_, err := r.Next()
		require.NoError(t, err)
	}
	_, err = r.Next()
	assert.Same(t, boom, err)
}

func TestReaderNoProgress(t *testing.T) {
	_, err := NewReader(zeroReader{}).Next()
	assert.ErrorIs(t, err, io.ErrNoProgress)
}

type dataErrReader struct {
	data []byte
	err  error
}

func (d *dataErrReader) Read(p []byte) (int, error) {
	if len(d.data) == 0 {
		return 0, d.err
	}
	n := copy(p, d.data)
	d.data = d.data[n:]
	if len(d.data) == 0 {
		return n, d.err
	}
	return n, nil
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }
