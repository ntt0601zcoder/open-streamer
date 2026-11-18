package flv

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/h26x"
)

var errNoConfig = errors.New("no track config")

// hasChromaExt reports the AVC profiles whose record carries the chroma format
// and bit depth fields (ISO/IEC 14496-15 5.3.3.1.2, later editions).
func hasChromaExt(profile byte) bool {
	switch profile {
	case 100, 110, 122, 144, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

// AVCRecord builds the AVCDecoderConfigurationRecord of cfg: profile,
// compatibility and level from the first SPS, 4-byte NAL lengths, every SPS
// and PPS, and for High profiles the chroma format and bit depths of the SPS.
func AVCRecord(cfg *media.TrackConfig) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("flv: avc record: %w", errNoConfig)
	}
	if cfg.Codec != media.CodecH264 {
		return nil, fmt.Errorf("flv: avc record: codec %v is not H.264", cfg.Codec)
	}
	rec, err := avc.CreateAVCDecConfRec(cfg.SPS, cfg.PPS, true)
	if err != nil {
		return nil, fmt.Errorf("flv: avc record: %w", err)
	}
	if hasChromaExt(rec.AVCProfileIndication) {
		sps, err := h26x.ParseSPS(h26x.H264, cfg.SPS[0])
		if err != nil {
			return nil, fmt.Errorf("flv: avc record: %w", err)
		}
		rec.ChromaFormat = sps.ChromaFormatIDC
		rec.BitDepthLumaMinus1 = sps.BitDepthLuma - 8
		rec.BitDepthChromaMinus1 = sps.BitDepthChroma - 8
	}
	var buf bytes.Buffer
	if err := rec.Encode(&buf); err != nil {
		return nil, fmt.Errorf("flv: avc record: %w", err)
	}
	out := buf.Bytes()
	if hasChromaExt(rec.AVCProfileIndication) && len(out) == int(rec.Size())-4 {
		// mp4ff writes the extension only for profiles 100, 110, 122 and 144.
		out = append(out, 0xfc|rec.ChromaFormat, 0xf8|rec.BitDepthLumaMinus1,
			0xf8|rec.BitDepthChromaMinus1, 0)
	}
	return out, nil
}

// HEVCRecord builds the HEVCDecoderConfigurationRecord of cfg with the VPS,
// SPS and PPS arrays marked complete and 4-byte NAL lengths.
func HEVCRecord(cfg *media.TrackConfig) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("flv: hevc record: %w", errNoConfig)
	}
	if cfg.Codec != media.CodecH265 {
		return nil, fmt.Errorf("flv: hevc record: codec %v is not H.265", cfg.Codec)
	}
	rec, err := hevc.CreateHEVCDecConfRec(cfg.VPS, cfg.SPS, cfg.PPS, true, true, true, true)
	if err != nil {
		return nil, fmt.Errorf("flv: hevc record: %w", err)
	}
	var buf bytes.Buffer
	if err := rec.Encode(&buf); err != nil {
		return nil, fmt.Errorf("flv: hevc record: %w", err)
	}
	return buf.Bytes(), nil
}
