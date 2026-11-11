package media

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
)

// CodecString returns the RFC 6381 codec string of a track, as used in HLS
// CODECS and DASH @codecs. Video uses the lowest-ID SPS.
func CodecString(c *TrackConfig) (string, error) {
	s, err := codecString(c)
	if err != nil {
		return "", fmt.Errorf("media: codec string: %w", err)
	}
	return s, nil
}

func codecString(c *TrackConfig) (string, error) {
	if c == nil {
		return "", errors.New("nil config")
	}
	switch c.Codec {
	case CodecH264:
		if len(c.SPS) == 0 {
			return "", errors.New("h264: no sps")
		}
		sps, err := avc.ParseSPSNALUnit(c.SPS[0], false)
		if err != nil {
			return "", fmt.Errorf("h264: parse sps: %w", err)
		}
		return avc.CodecString("avc1", sps), nil
	case CodecH265:
		if len(c.SPS) == 0 {
			return "", errors.New("h265: no sps")
		}
		sps, err := hevc.ParseSPSNALUnit(c.SPS[0])
		if err != nil {
			return "", fmt.Errorf("h265: parse sps: %w", err)
		}
		return hevc.CodecString("hvc1", sps), nil
	case CodecAAC:
		asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(c.ASC))
		if err != nil {
			return "", fmt.Errorf("aac: parse asc: %w", err)
		}
		return fmt.Sprintf("mp4a.40.%d", asc.ObjectType), nil
	default:
		return "", fmt.Errorf("unsupported codec %d", c.Codec)
	}
}
