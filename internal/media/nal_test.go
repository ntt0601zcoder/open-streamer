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

// ueBits is the exp-Golomb code of v as a string of bits.
func ueBits(v uint) string {
	x := v + 1
	n := 0
	for y := x; y > 1; y >>= 1 {
		n++
	}
	s := ""
	for range n {
		s += "0"
	}
	for i := n; i >= 0; i-- {
		if x&(1<<uint(i)) != 0 {
			s += "1"
		} else {
			s += "0"
		}
	}
	return s
}

// Only slice types 2 and 7 (I) with a recovery point make a key; P, B, SP
// and SI slices do not, and a P or B slice is never RASL before a key.
func TestClassifyH264SliceTypes(t *testing.T) {
	assert.Equal(t, "1", ueBits(0))
	assert.Equal(t, "0001000", ueBits(7))
	for st := range uint(10) {
		c := NewNALClassifier(CodecH264)
		k, r := c.Classify([][]byte{recoverySEI("1"), slice(ueBits(st))}, 100)
		assert.Equal(t, st == 2 || st == 7, k, "slice_type %d", st)
		assert.False(t, r, "slice_type %d", st)
	}
}

// SEI payload types and sizes over 254 use 0xFF extension bytes: a payload
// with a long size is skipped whole, and a type whose last byte is 6 but
// whose value is 261 is not a recovery point.
func TestClassifyH264SEIExtension(t *testing.T) {
	long := make([]byte, 300) // user data, size 300 = FF 2D
	for i := range long {
		long[i] = 0x06 // bytes that would read as a recovery point if misparsed
	}
	recovery0 := append([]byte{0x06, 0x01}, packBits("1"+"1000")...)
	sei := append(append([]byte{0x06, 0x05, 0xff, 0x2d}, long...), recovery0...)
	assert.True(t, h264RecoveryPoint0(sei), "recovery point after a payload with an extended size")

	sei = append([]byte{0x06, 0xff, 0x06, 0x01}, packBits("1"+"1000")...)
	assert.False(t, h264RecoveryPoint0(sei), "payload type 261 is not a recovery point")

	sei = append([]byte{0x06, 0xff, 0x06, 0x01, 0x80}, recovery0...)
	assert.True(t, h264RecoveryPoint0(sei), "recovery point after a payload with an extended type")
}

// The recovery point need not be the first SEI message, nor in the first
// SEI NAL unit; its own recovery_frame_cnt decides.
func TestClassifyH264SEINotFirst(t *testing.T) {
	userData := append([]byte{0x05, 0x10}, make([]byte, 16)...)
	msg := func(cnt string) []byte { return append([]byte{0x06, 0x01}, packBits(cnt+"1000")...) }
	sei := func(msgs ...[]byte) []byte {
		out := []byte{0x06}
		for _, m := range msgs {
			out = append(out, m...)
		}
		return out
	}
	i := slice(ueBits(7))
	for name, tc := range map[string]struct {
		au  [][]byte
		key bool
	}{
		"second message, cnt 0":   {au: [][]byte{sei(userData, msg("1")), i}, key: true},
		"second message, cnt 2":   {au: [][]byte{sei(userData, msg("011")), i}, key: false},
		"second SEI NAL, cnt 0":   {au: [][]byte{sei(userData), sei(msg("1")), i}, key: true},
		"second SEI NAL, cnt 2":   {au: [][]byte{sei(userData), sei(msg("011")), i}, key: false},
		"third message after two": {au: [][]byte{sei(userData, userData, msg("1")), i}, key: true},
	} {
		c := NewNALClassifier(CodecH264)
		k, _ := c.Classify(tc.au, 0)
		assert.Equal(t, tc.key, k, name)
	}
}
