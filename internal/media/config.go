package media

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/hevc"
)

// TrackConfig describes a track's codec parameters. It is immutable once built.
type TrackConfig struct {
	Codec                Codec
	Width, Height        int      // from the SPS with the lowest ID
	VPS, SPS, PPS        [][]byte // video: every set in use, sorted by ID, one per ID
	SampleRate, Channels int      // audio
	ASC                  []byte   // AAC AudioSpecificConfig
}

type paramSet struct {
	id   uint32
	data []byte
}

// sortedByID parses the ID of every NAL unit, rejects duplicates and returns
// copies ordered by ID.
func sortedByID(codec Codec, name string, nalus [][]byte) ([]paramSet, error) {
	if len(nalus) == 0 {
		return nil, fmt.Errorf("no %s", name)
	}
	sets := make([]paramSet, 0, len(nalus))
	for _, n := range nalus {
		id, err := ParamSetID(codec, n)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		sets = append(sets, paramSet{id: id, data: bytes.Clone(n)})
	}
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].id < sets[j].id })
	for i := 1; i < len(sets); i++ {
		if sets[i].id == sets[i-1].id {
			return nil, fmt.Errorf("duplicate %s id %d", name, sets[i].id)
		}
	}
	return sets, nil
}

func datas(sets []paramSet) [][]byte {
	out := make([][]byte, len(sets))
	for i, s := range sets {
		out[i] = s.data
	}
	return out
}

// NewH264Config builds a config from SPS and PPS NAL units (without start codes).
func NewH264Config(sps, pps [][]byte) (*TrackConfig, error) {
	cfg, err := newH264Config(sps, pps)
	if err != nil {
		return nil, fmt.Errorf("media: h264 config: %w", err)
	}
	return cfg, nil
}

func newH264Config(sps, pps [][]byte) (*TrackConfig, error) {
	spsSets, err := sortedByID(CodecH264, "sps", sps)
	if err != nil {
		return nil, err
	}
	ppsSets, err := sortedByID(CodecH264, "pps", pps)
	if err != nil {
		return nil, err
	}
	parsed, err := avc.ParseSPSNALUnit(spsSets[0].data, false)
	if err != nil {
		return nil, fmt.Errorf("parse sps: %w", err)
	}
	return &TrackConfig{
		Codec:  CodecH264,
		Width:  int(parsed.Width),
		Height: int(parsed.Height),
		SPS:    datas(spsSets),
		PPS:    datas(ppsSets),
	}, nil
}

// NewH265Config builds a config from VPS, SPS and PPS NAL units (without start codes).
func NewH265Config(vps, sps, pps [][]byte) (*TrackConfig, error) {
	cfg, err := newH265Config(vps, sps, pps)
	if err != nil {
		return nil, fmt.Errorf("media: h265 config: %w", err)
	}
	return cfg, nil
}

func newH265Config(vps, sps, pps [][]byte) (*TrackConfig, error) {
	vpsSets, err := sortedByID(CodecH265, "vps", vps)
	if err != nil {
		return nil, err
	}
	spsSets, err := sortedByID(CodecH265, "sps", sps)
	if err != nil {
		return nil, err
	}
	ppsSets, err := sortedByID(CodecH265, "pps", pps)
	if err != nil {
		return nil, err
	}
	parsed, err := hevc.ParseSPSNALUnit(spsSets[0].data)
	if err != nil {
		return nil, fmt.Errorf("parse sps: %w", err)
	}
	w, h := parsed.ImageSize()
	return &TrackConfig{
		Codec:  CodecH265,
		Width:  int(w),
		Height: int(h),
		VPS:    datas(vpsSets),
		SPS:    datas(spsSets),
		PPS:    datas(ppsSets),
	}, nil
}

// NewAACConfig builds a config from an AudioSpecificConfig.
func NewAACConfig(asc []byte) (*TrackConfig, error) {
	cfg, err := newAACConfig(asc)
	if err != nil {
		return nil, fmt.Errorf("media: aac config: %w", err)
	}
	return cfg, nil
}

func newAACConfig(asc []byte) (*TrackConfig, error) {
	parsed, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(asc))
	if err != nil {
		return nil, err
	}
	var channels int
	switch c := int(parsed.ChannelConfiguration); {
	case c >= 1 && c <= 6:
		channels = c
	case c == 7:
		channels = 8
	default:
		return nil, fmt.Errorf("unsupported channel configuration %d", c)
	}
	if parsed.SamplingFrequency <= 0 {
		return nil, fmt.Errorf("invalid sample rate %d", parsed.SamplingFrequency)
	}
	return &TrackConfig{
		Codec:      CodecAAC,
		SampleRate: parsed.SamplingFrequency,
		Channels:   channels,
		ASC:        bytes.Clone(asc),
	}, nil
}

// Equal reports whether two configs describe the same codec parameters.
// A nil config equals only nil.
func (c *TrackConfig) Equal(o *TrackConfig) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.Codec == o.Codec &&
		c.Width == o.Width && c.Height == o.Height &&
		c.SampleRate == o.SampleRate && c.Channels == o.Channels &&
		bytes.Equal(c.ASC, o.ASC) &&
		equalSets(c.VPS, o.VPS) && equalSets(c.SPS, o.SPS) && equalSets(c.PPS, o.PPS)
}

func equalSets(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ParamSetID returns the ID of a VPS, SPS or PPS NAL unit (with its NAL header).
// The NAL type is not checked; the caller decides which kind of set nalu is.
func ParamSetID(codec Codec, nalu []byte) (uint32, error) {
	id, err := paramSetID(codec, nalu)
	if err != nil {
		return 0, fmt.Errorf("media: param set id: %w", err)
	}
	return id, nil
}

func paramSetID(codec Codec, nalu []byte) (uint32, error) {
	switch codec {
	case CodecH264:
		if len(nalu) < 2 {
			return 0, fmt.Errorf("h264 nal unit too short")
		}
		switch nalu[0] & 0x1f {
		case 7:
			sps, err := avc.ParseSPSNALUnit(nalu, false)
			if err != nil {
				return 0, err
			}
			return sps.ParameterID, nil
		case 8:
			return readFirstUE(nalu, 1)
		default:
			return 0, fmt.Errorf("h264 nal type %d is not a parameter set", nalu[0]&0x1f)
		}
	case CodecH265:
		if len(nalu) < 3 {
			return 0, fmt.Errorf("h265 nal unit too short")
		}
		switch (nalu[0] >> 1) & 0x3f {
		case 32:
			vps, err := hevc.ParseVPSNALUnit(nalu)
			if err != nil {
				return 0, err
			}
			return uint32(vps.VpsID), nil
		case 33:
			sps, err := hevc.ParseSPSNALUnit(nalu)
			if err != nil {
				return 0, err
			}
			return uint32(sps.SpsID), nil
		case 34:
			return readFirstUE(nalu, 2)
		default:
			return 0, fmt.Errorf("h265 nal type %d is not a parameter set", (nalu[0]>>1)&0x3f)
		}
	case CodecAAC:
		return 0, fmt.Errorf("aac has no parameter sets")
	default:
		return 0, fmt.Errorf("unknown codec %d", codec)
	}
}

// readFirstUE reads the first ue(v) after a header of hdr bytes, removing
// emulation prevention bytes.
func readFirstUE(nalu []byte, hdr int) (uint32, error) {
	r := bits.NewEBSPReader(bytes.NewReader(nalu))
	for i := 0; i < hdr; i++ {
		r.Read(8)
	}
	v := r.ReadExpGolomb()
	if err := r.AccError(); err != nil {
		return 0, fmt.Errorf("read pps id: %w", err)
	}
	return uint32(v), nil
}
