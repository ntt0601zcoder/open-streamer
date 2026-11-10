package mpegts

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/aac"
)

// maxADTSPayload is the largest payload a 13-bit frame length can describe.
const maxADTSPayload = 8191 - 7

// ADTS is one parsed ADTS frame.
type ADTS struct {
	// ObjectType is the MPEG-4 audio object type (ADTS profile + 1).
	ObjectType uint8
	// SampleRateIndex is the sampling frequency index from the header.
	SampleRateIndex uint8
	// ChannelConfig is the channel configuration from the header.
	ChannelConfig uint8
	// SampleRate is the sampling frequency in Hz.
	SampleRate int
	// Payload is the raw AAC data without the ADTS header.
	Payload []byte
}

// ParseADTS returns every ADTS frame in a PES payload. On a malformed or
// truncated frame it returns the frames parsed before it together with an
// error wrapping "mpegts: adts:".
func ParseADTS(buf []byte) ([]ADTS, error) {
	var frames []ADTS
	for len(buf) > 0 {
		hdr, offset, err := aac.DecodeADTSHeader(bytes.NewReader(buf))
		if err != nil {
			return frames, fmt.Errorf("mpegts: adts: header: %w", err)
		}
		if offset != 0 {
			return frames, fmt.Errorf("mpegts: adts: %d unexpected bytes before sync word", offset)
		}
		if hdr.SamplingFrequencyIndex >= 13 {
			return frames, fmt.Errorf("mpegts: adts: invalid sampling frequency index %d", hdr.SamplingFrequencyIndex)
		}
		if hdr.PayloadLength > maxADTSPayload {
			return frames, fmt.Errorf("mpegts: adts: invalid frame length")
		}
		hl, pl := int(hdr.HeaderLength), int(hdr.PayloadLength)
		if len(buf) < hl+pl {
			return frames, fmt.Errorf("mpegts: adts: truncated frame: need %d bytes, have %d", hl+pl, len(buf))
		}
		frames = append(frames, ADTS{
			ObjectType:      hdr.ObjectType,
			SampleRateIndex: hdr.SamplingFrequencyIndex,
			ChannelConfig:   hdr.ChannelConfig,
			SampleRate:      int(hdr.Frequency()),
			Payload:         buf[hl : hl+pl],
		})
		buf = buf[hl+pl:]
	}
	return frames, nil
}

// AudioSpecificConfig returns the 2-byte AAC AudioSpecificConfig for the frame:
// 5 bits object type, 4 bits sampling index, 4 bits channel configuration.
func (a ADTS) AudioSpecificConfig() []byte {
	v := uint16(a.ObjectType&0x1f)<<11 | uint16(a.SampleRateIndex&0x0f)<<7 | uint16(a.ChannelConfig&0x0f)<<3
	return []byte{byte(v >> 8), byte(v)}
}
