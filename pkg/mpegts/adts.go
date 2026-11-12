package mpegts

import (
	"bytes"
	"errors"
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
	// Payload is the raw AAC data without the ADTS header. It aliases the
	// input buffer passed to ParseADTS; its capacity is limited to its length.
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
		if hdr.ChannelConfig == 0 {
			return frames, errors.New("mpegts: adts: channel configuration 0 (PCE) is not supported")
		}
		if hdr.PayloadLength > maxADTSPayload {
			return frames, fmt.Errorf("mpegts: adts: invalid frame length (payload length %d)", hdr.PayloadLength)
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
			Payload:         buf[hl : hl+pl : hl+pl],
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

// AppendADTS appends one ADTS frame (7-byte header, no CRC) carrying payload
// to dst. The header comes from asc, an AAC AudioSpecificConfig: the object
// type must be 1 to 4 (the ADTS profile is the object type minus one), the
// sampling frequency index 0 to 12 and the channel configuration 1 to 7.
func AppendADTS(dst, asc, payload []byte) ([]byte, error) {
	if len(asc) < 2 {
		return dst, fmt.Errorf("mpegts: adts: audio specific config has %d bytes, need 2", len(asc))
	}
	v := uint16(asc[0])<<8 | uint16(asc[1])
	objType, srIdx, chans := v>>11, (v>>7)&0x0F, (v>>3)&0x0F
	if objType < 1 || objType > 4 {
		return dst, fmt.Errorf("mpegts: adts: object type %d cannot be written in an ADTS header", objType)
	}
	if srIdx >= 13 {
		return dst, fmt.Errorf("mpegts: adts: invalid sampling frequency index %d", srIdx)
	}
	if chans < 1 || chans > 7 {
		return dst, fmt.Errorf("mpegts: adts: channel configuration %d is not supported", chans)
	}
	if len(payload) > maxADTSPayload {
		return dst, fmt.Errorf("mpegts: adts: payload of %d bytes does not fit one frame", len(payload))
	}
	fl := 7 + len(payload)
	dst = append(dst,
		0xFF, 0xF1, // sync word, MPEG-4, layer 0, protection absent
		byte(objType-1)<<6|byte(srIdx)<<2|byte(chans>>2),
		byte(chans&3)<<6|byte(fl>>11),
		byte(fl>>3),
		byte(fl<<5)|0x1F,
		0xFC,
	)
	return append(dst, payload...), nil
}
