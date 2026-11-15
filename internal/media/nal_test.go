package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// packBits packs a string of 0/1 characters into bytes, padding with a stop bit.
func packBits(s string) []byte {
	s += "1"
	for len(s)%8 != 0 {
		s += "0"
	}
	out := make([]byte, len(s)/8)
	for i, c := range s {
		if c == '1' {
			out[i/8] |= 0x80 >> uint(i%8)
		}
	}
	return out
}

// slice builds a non-IDR H.264 slice: first_mb_in_slice 0 ("1"), then
// slice_type ue: 2 = "011", 7 = "0001000", 0 = "1", 5 = "00110".
func slice(typeBits string) []byte {
	return append([]byte{0x41}, packBits("1"+typeBits+"1")...)
}

func recoverySEI(cnt string) []byte {
	// payload type 6, size 1, recovery_frame_cnt ue + flags.
	return append([]byte{0x06, 0x06, 0x01}, packBits(cnt+"1000")...)
}

func TestClassifyH264OpenGOP(t *testing.T) {
	idr := []byte{0x65, 0x88, 0x84}
	sps := []byte{0x67, 0x42, 0x00, 0x1e}
	c := NewNALClassifier(CodecH264)

	k, r := c.Classify([][]byte{sps, idr}, 0)
	assert.True(t, k)
	assert.False(t, r)

	// P frame, then an I slice without SEI: not a key.
	k, r = c.Classify([][]byte{slice("1")}, 10)
	assert.False(t, k)
	assert.False(t, r)
	k, r = c.Classify([][]byte{slice("011")}, 20)
	assert.False(t, k, "I slice without recovery SEI")
	assert.False(t, r)

	// recovery_frame_cnt 2 ("011"): not a key.
	k, _ = c.Classify([][]byte{recoverySEI("011"), slice("011")}, 30)
	assert.False(t, k)

	// Recovery point, I slice (type 7), cnt 0: key.
	k, r = c.Classify([][]byte{recoverySEI("1"), slice("0001000")}, 100)
	assert.True(t, k)
	assert.False(t, r)

	// Leading B frames (lower PTS) are RASL; later frames are not.
	_, r = c.Classify([][]byte{slice("00110")}, 80)
	assert.True(t, r)
	_, r = c.Classify([][]byte{slice("00110")}, 90)
	assert.True(t, r)
	k, r = c.Classify([][]byte{slice("1")}, 130)
	assert.False(t, k)
	assert.False(t, r)

	// An IDR ends the recovery state.
	k, _ = c.Classify([][]byte{idr}, 200)
	assert.True(t, k)
	_, r = c.Classify([][]byte{slice("00110")}, 150)
	assert.False(t, r)

	// A mixed access unit (I and P slices) with SEI is not a key.
	k, _ = c.Classify([][]byte{recoverySEI("1"), slice("011"), slice("1")}, 300)
	assert.False(t, k)
}

func TestClassifyH264EmulationPrevention(t *testing.T) {
	// A user-data payload (type 5, 4 bytes 00 00 01 aa, escaped on the wire)
	// before the recovery point.
	sei := append([]byte{0x06, 0x05, 0x04, 0x00, 0x00, 0x03, 0x01, 0xaa, 0x06, 0x01}, packBits("1"+"1000")...)
	assert.True(t, h264RecoveryPoint0(sei))
	e := ebsp{b: []byte{0x01, 0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x03}}
	var got []byte
	for v, ok := e.next(); ok; v, ok = e.next() {
		got = append(got, v)
	}
	assert.Equal(t, []byte{0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x03}, got)
}

func TestClassifyH265(t *testing.T) {
	c := NewNALClassifier(CodecH265)
	k, r := c.Classify([][]byte{{21 << 1, 1, 0}}, 0)
	assert.True(t, k)
	assert.False(t, r)
	k, r = c.Classify([][]byte{{8 << 1, 1, 0}}, 0)
	assert.False(t, k)
	assert.True(t, r)
	k, r = c.Classify([][]byte{{1 << 1, 1, 0}}, 0)
	assert.False(t, k)
	assert.False(t, r)
}

// bigNAL returns a 1 MB NAL unit: head, then a body of zeros and
// emulation-prevention bytes, as an escaped slice body looks.
func bigNAL(head []byte) []byte {
	const size = 1 << 20
	n := make([]byte, size)
	copy(n, head)
	for i := len(head); i+2 < size; i += 3 {
		n[i], n[i+1], n[i+2] = 0, 0, 3
	}
	return n
}

// Classifying an access unit allocates nothing, however large its slices
// and SEI: only slice-header and SEI-header bytes are read.
func TestClassifyH264AllocsBounded(t *testing.T) {
	c := NewNALClassifier(CodecH264)
	p := bigNAL(slice("1"))
	i := bigNAL(slice("011"))
	userData := bigNAL([]byte{0x06, 0x05, 0xff, 0xff, 0xff, 0xff, 0x0f}) // a 1 kB+ payload header, then filler
	for name, au := range map[string][][]byte{
		"P frame":        {p},
		"I slices":       {i, i},
		"P after I":      {i, p, p, p},
		"large SEI":      {userData, i},
		"recovery point": {recoverySEI("1"), i},
	} {
		allocs := testing.AllocsPerRun(20, func() { c.Classify(au, 0) })
		assert.Zero(t, allocs, name)
	}
	k, _ := c.Classify([][]byte{recoverySEI("1"), i, i}, 1000)
	assert.True(t, k, "large I slices with a recovery point still classify")
}

func BenchmarkClassifyH264LargeSlice(b *testing.B) {
	c := NewNALClassifier(CodecH264)
	au := [][]byte{bigNAL(slice("1"))}
	b.ReportAllocs()
	for b.Loop() {
		c.Classify(au, 0)
	}
}
