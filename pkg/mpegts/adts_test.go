package mpegts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/asticode/go-astits"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstAudioPES returns the payload of the first AAC (stream type 0x0F) PES in a fixture.
func firstAudioPES(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(raw))
	audio := map[uint16]bool{}
	for {
		d, err := dmx.NextData()
		if errors.Is(err, io.EOF) || errors.Is(err, astits.ErrNoMorePackets) {
			t.Fatalf("no audio PES in %s", name)
		}
		require.NoError(t, err)
		if d.PMT != nil {
			for _, es := range d.PMT.ElementaryStreams {
				if es.StreamType == astits.StreamTypeAACAudio {
					audio[es.ElementaryPID] = true
				}
			}
		}
		if d.PES != nil && audio[d.FirstPacket.Header.PID] {
			return d.PES.Data
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
	out := append(h, bytes.Repeat([]byte{0xAB}, n)...)
	return out
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
