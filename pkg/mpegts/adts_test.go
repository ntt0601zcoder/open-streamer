package mpegts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstAudioPES returns the payload of the first AAC (stream type 0x0F) PES in a fixture.
func firstAudioPES(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	dmx := NewDemux(bytes.NewReader(raw))
	audio := map[uint16]bool{}
	for {
		ev, err := dmx.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("no audio PES in %s", name)
		}
		require.NoError(t, err)
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				if s.Type == 0x0F {
					audio[s.PID] = true
				}
			}
		}
		if ev.PES != nil && audio[ev.PES.PID] {
			return ev.PES.Data
		}
	}
}

// adtsFrame builds a frame with a 7-byte header and n payload bytes
// (48 kHz stereo LC).
func adtsFrame(n int) []byte {
	fl := n + 7
	h := []byte{0xFF, 0xF1, 0x4C, 0x80, 0, 0, 0xFC}
	h[3] = 0x80 | byte(fl>>11)
	h[4] = byte(fl >> 3)
	h[5] = byte(fl<<5) | 0x1F
	out := make([]byte, 0, len(h)+n)
	out = append(out, h...)
	return append(out, bytes.Repeat([]byte{0xAB}, n)...)
}

func TestParseADTS(t *testing.T) {
	frames, err := ParseADTS(firstAudioPES(t, "h264_aac.ts"))
	require.NoError(t, err)
	require.NotEmpty(t, frames)
	for _, f := range frames {
		assert.Equal(t, []byte{0x11, 0x90}, f.AudioSpecificConfig())
		assert.Equal(t, 48000, f.SampleRate)
		assert.NotEmpty(t, f.Payload)
	}

	frames, err = ParseADTS(firstAudioPES(t, "h265_aac51.ts"))
	require.NoError(t, err)
	require.NotEmpty(t, frames)
	assert.Equal(t, uint8(6), frames[0].ChannelConfig)
}

func TestParseADTSSynthetic(t *testing.T) {
	buf := append(adtsFrame(10), adtsFrame(20)...)
	frames, err := ParseADTS(buf)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Len(t, frames[0].Payload, 10)
	assert.Len(t, frames[1].Payload, 20)
	assert.Equal(t, []byte{0x11, 0x90}, frames[0].AudioSpecificConfig())

	// 9-byte header (protection_absent = 0, CRC present).
	f := adtsFrame(5)
	crc := append(append([]byte{}, f[:7]...), 0x00, 0x00)
	crc[1] = 0xF0
	fl := 5 + 9
	crc[3] = 0x80 | byte(fl>>11)
	crc[4] = byte(fl >> 3)
	crc[5] = byte(fl<<5) | 0x1F
	crc = append(crc, f[7:]...)
	frames, err = ParseADTS(append(crc, adtsFrame(3)...))
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Len(t, frames[0].Payload, 5)
	assert.Len(t, frames[1].Payload, 3)
}

func TestParseADTSTruncated(t *testing.T) {
	buf := append(adtsFrame(10), adtsFrame(20)...)
	frames, err := ParseADTS(buf[:len(buf)-5])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")
	assert.Len(t, frames, 1)
}

func TestParseADTSInvalid(t *testing.T) {
	// Sampling index 13.
	bad := adtsFrame(4)
	bad[2] = bad[2]&^0x3C | 13<<2
	_, err := ParseADTS(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")

	// Frame length 0.
	zero := adtsFrame(4)
	zero[3] &^= 0x03
	zero[4], zero[5] = 0, zero[5]&0x1F
	_, err = ParseADTS(zero)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")

	// One junk byte between two valid frames.
	buf := append(adtsFrame(6), 0x00)
	buf = append(buf, adtsFrame(6)...)
	frames, err := ParseADTS(buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")
	assert.Len(t, frames, 1)
}

func TestParseADTSChannelConfigZero(t *testing.T) {
	f := adtsFrame(4)
	f[2] &^= 0x01
	f[3] &^= 0xC0
	frames, err := ParseADTS(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")
	assert.Empty(t, frames)
}

func TestParseADTSFrameShorterThanCRCHeader(t *testing.T) {
	// protection_absent = 0 (9-byte header) with frame length 8.
	f := []byte{0xFF, 0xF0, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0, 0, 0xAA}
	_, err := ParseADTS(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")
}

func TestParseADTSPayloadCapacityLimited(t *testing.T) {
	frames, err := ParseADTS(append(adtsFrame(4), adtsFrame(4)...))
	require.NoError(t, err)
	assert.Equal(t, len(frames[0].Payload), cap(frames[0].Payload))
}

func TestAppendADTS(t *testing.T) {
	tests := []struct {
		name string
		asc  []byte
		rate int
		ch   uint8
	}{
		{"mono", []byte{0x11, 0x88}, 48000, 1},
		{"stereo", []byte{0x12, 0x10}, 44100, 2},
		{"5.1", []byte{0x12, 0x30}, 44100, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p1, p2 := bytes.Repeat([]byte{0x5A}, 100), bytes.Repeat([]byte{0xA5}, 3000)
			buf, err := AppendADTS([]byte{}, tt.asc, p1)
			require.NoError(t, err)
			buf, err = AppendADTS(buf, tt.asc, p2)
			require.NoError(t, err)
			frames, err := ParseADTS(buf)
			require.NoError(t, err)
			require.Len(t, frames, 2)
			assert.Equal(t, p1, frames[0].Payload)
			assert.Equal(t, p2, frames[1].Payload)
			for _, f := range frames {
				assert.Equal(t, tt.asc, f.AudioSpecificConfig())
				assert.Equal(t, tt.rate, f.SampleRate)
				assert.Equal(t, tt.ch, f.ChannelConfig)
			}
		})
	}
}

// heASC builds an HE-AAC AudioSpecificConfig with explicit hierarchical
// signalling: aot, core sampling index, channels, extension sampling index,
// core object type. An index of 15 is followed by a 24-bit frequency.
func heASC(aot, core, chans, ext, coreAOT uint64) []byte {
	var v uint64
	n := 0
	put := func(x uint64, bits int) { v, n = v<<bits|x, n+bits }
	put(aot, 5)
	put(core, 4)
	if core == 15 {
		put(24000, 24)
	}
	put(chans, 4)
	put(ext, 4)
	if ext == 15 {
		put(48000, 24)
	}
	put(coreAOT, 5)
	put(0, 3) // GASpecificConfig
	pad := (8 - n%8) % 8
	v <<= pad
	n += pad
	out := make([]byte, n/8)
	for i := range out {
		out[i] = byte(v >> (8 * (len(out) - 1 - i)))
	}
	return out
}

// HE-AAC ASCs are written as an AAC-LC ADTS header at the core sampling
// frequency and channel configuration (implicit SBR/PS signalling).
func TestAppendADTSHEAAC(t *testing.T) {
	tests := []struct {
		name  string
		asc   []byte
		rate  int
		srIdx uint8
		chans uint8
	}{
		{"aot5 24k/48k stereo", heASC(5, 6, 2, 3, 2), 24000, 6, 2},
		{"aot29 24k/48k mono core", heASC(29, 6, 1, 3, 2), 24000, 6, 1},
		// PS always has a mono core; an ASC that says 2 is written as 1.
		{"aot29 wrongly stereo", heASC(29, 6, 2, 3, 2), 24000, 6, 1},
		{"aot5 22.05k/44.1k explicit ext fs", heASC(5, 7, 2, 15, 2), 22050, 7, 2},
		{"aot5 bytes 2B 11 88 00", []byte{0x2B, 0x11, 0x88, 0x00}, 24000, 6, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x5A}, 200)
			buf, err := AppendADTS([]byte{0xEE}, tt.asc, payload)
			require.NoError(t, err)
			require.Equal(t, byte(0xEE), buf[0], "dst prefix kept")
			frames, err := ParseADTS(buf[1:])
			require.NoError(t, err)
			require.Len(t, frames, 1)
			f := frames[0]
			assert.Equal(t, uint8(2), f.ObjectType, "ADTS profile LC")
			assert.Equal(t, tt.srIdx, f.SampleRateIndex)
			assert.Equal(t, tt.rate, f.SampleRate)
			assert.Equal(t, tt.chans, f.ChannelConfig)
			assert.Equal(t, payload, f.Payload)
		})
	}
}

func TestAppendADTSErrors(t *testing.T) {
	tests := map[string][]byte{
		"short asc":     {0x12},
		"object type 0": {0x02, 0x10},
		"object type 6": {0x32, 0x10},
		"escape 31":     {0xF8, 0x00, 0x00},
		"sample index":  {0x17, 0x90},
		"channels 0":    {0x12, 0x00},
		"channels 8":    {0x12, 0x40},
		// HE-AAC: aot 5 needs the extension index and the core object type.
		"he-aac truncated":     {0x2B, 0x10},
		"he-aac core index":    heASC(5, 13, 2, 3, 2),
		"he-aac explicit fs":   heASC(5, 15, 2, 3, 2),
		"he-aac core not lc":   heASC(5, 6, 2, 3, 1),
		"he-aac channels 0":    heASC(5, 6, 0, 3, 2),
		"he-aac ext truncated": {0x2B, 0x17, 0x80},
	}
	for name, asc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := AppendADTS(nil, asc, []byte{1})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mpegts: adts:")
		})
	}
	_, err := AppendADTS(nil, []byte{0x12, 0x10}, make([]byte, 8185))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts: adts:")
	buf, err := AppendADTS(nil, []byte{0x12, 0x10}, make([]byte, 8184))
	require.NoError(t, err)
	_, err = ParseADTS(buf)
	assert.NoError(t, err)
}
