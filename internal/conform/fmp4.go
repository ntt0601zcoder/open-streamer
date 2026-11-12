package conform

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// sampleNonSync is the sample_is_non_sync_sample bit of ISO/IEC 14496-12
// sample flags.
const sampleNonSync = 0x00010000

// fmp4Init is what the reader needs from a CMAF init segment.
type fmp4Init struct {
	cfg       *media.TrackConfig
	timescale uint32
	trex      *mp4.TrexBox
	nalLen    int // NAL unit length field size, video only
}

// fmp4Seg is one media segment read back into frames. Frames carry the
// init's config, track 0 and epoch 0; the caller sets them. Times are in the
// track timescale.
type fmp4Seg struct {
	frames   []media.Frame
	baseTime uint64 // tfdt of the first fragment
	earliest uint64 // smallest decode time + composition offset
	duration uint64 // summed sample durations
	samples  int
}

// decodeMP4 runs the mp4ff decoder, turning a panic on hostile input into an
// error.
func decodeMP4(b []byte) (f *mp4.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, fmt.Errorf("decoder panic: %v", r)
		}
	}()
	return mp4.DecodeFile(bytes.NewReader(b))
}

// parseInit reads the single track of an init segment: its codec config
// (rebuilt with the media constructors), timescale and trex defaults.
func parseInit(b []byte) (fmp4Init, error) {
	f, err := decodeMP4(b)
	if err != nil {
		return fmp4Init{}, fmt.Errorf("conform: init: %w", err)
	}
	if f.Init == nil || f.Init.Moov == nil || f.Init.Moov.Trak == nil {
		return fmp4Init{}, errors.New("conform: init: no moov track")
	}
	trak := f.Init.Moov.Trak
	if trak.Mdia == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil ||
		trak.Mdia.Minf.Stbl.Stsd == nil {
		return fmp4Init{}, errors.New("conform: init: incomplete track box")
	}
	in := fmp4Init{timescale: trak.Mdia.Mdhd.Timescale}
	if in.timescale == 0 {
		return fmp4Init{}, errors.New("conform: init: zero timescale")
	}
	if mvex := f.Init.Moov.Mvex; mvex != nil {
		in.trex = mvex.Trex
	}
	stsd := trak.Mdia.Minf.Stbl.Stsd
	switch {
	case stsd.AvcX != nil && stsd.AvcX.AvcC != nil:
		in.cfg, err = media.NewH264Config(stsd.AvcX.AvcC.SPSnalus, stsd.AvcX.AvcC.PPSnalus)
		in.nalLen = 4 // mp4ff decodes only 4-byte AVC lengths
	case stsd.HvcX != nil && stsd.HvcX.HvcC != nil:
		rec := &stsd.HvcX.HvcC.DecConfRec
		in.cfg, err = media.NewH265Config(rec.GetNalusForType(hevc.NALU_VPS),
			rec.GetNalusForType(hevc.NALU_SPS), rec.GetNalusForType(hevc.NALU_PPS))
		in.nalLen = int(rec.LengthSizeMinusOne) + 1
	case stsd.Mp4a != nil && stsd.Mp4a.Esds != nil && stsd.Mp4a.Esds.DecConfigDescriptor != nil &&
		stsd.Mp4a.Esds.DecConfigDescriptor.DecSpecificInfo != nil:
		in.cfg, err = media.NewAACConfig(stsd.Mp4a.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig)
	default:
		return fmp4Init{}, errors.New("conform: init: unsupported sample entry")
	}
	if err != nil {
		return fmp4Init{}, fmt.Errorf("conform: init: %w", err)
	}
	return in, nil
}

// splitNALUs splits length-prefixed sample data into NAL units.
func splitNALUs(data []byte, size int) ([][]byte, error) {
	var out [][]byte
	for len(data) > 0 {
		if len(data) < size {
			return nil, errors.New("truncated NAL length")
		}
		n := 0
		for _, c := range data[:size] {
			n = n<<8 | int(c)
		}
		data = data[size:]
		if n > len(data) {
			return nil, fmt.Errorf("NAL length %d beyond sample end", n)
		}
		if n > 0 {
			out = append(out, data[:n])
		}
		data = data[n:]
	}
	return out, nil
}

// isRASL reports whether a video frame holds an H.265 RASL picture.
func isRASL(codec media.Codec, nalus [][]byte) bool {
	if codec != media.CodecH265 {
		return false
	}
	for _, n := range nalus {
		if typ := (n[0] >> 1) & 0x3f; typ == 8 || typ == 9 { // RASL_N, RASL_R
			return true
		}
	}
	return false
}

// parseSegment reads a media segment of the init's track into frames in
// 90 kHz ticks.
func parseSegment(b []byte, in fmp4Init) (fmp4Seg, error) {
	f, err := decodeMP4(b)
	if err != nil {
		return fmp4Seg{}, fmt.Errorf("conform: segment: %w", err)
	}
	var seg fmp4Seg
	video := in.cfg.Codec.Kind() == media.KindVideo
	for _, ms := range f.Segments {
		for _, frag := range ms.Fragments {
			if frag.Moof == nil || frag.Moof.Traf == nil || frag.Mdat == nil {
				return fmp4Seg{}, errors.New("conform: segment: fragment without moof, traf or mdat")
			}
			if seg.samples == 0 && frag.Moof.Traf.Tfdt != nil {
				seg.baseTime = frag.Moof.Traf.Tfdt.BaseMediaDecodeTime()
			}
			samples, err := fullSamples(frag, in.trex)
			if err != nil {
				return fmp4Seg{}, fmt.Errorf("conform: segment: %w", err)
			}
			for _, s := range samples {
				fr, err := sampleFrame(s, in, video)
				if err != nil {
					return fmp4Seg{}, fmt.Errorf("conform: segment: sample %d: %w", seg.samples, err)
				}
				pt := uint64(max(s.PresentationTime(), 0))
				if seg.samples == 0 || pt < seg.earliest {
					seg.earliest = pt
				}
				seg.duration += uint64(s.Dur)
				seg.samples++
				seg.frames = append(seg.frames, fr)
			}
		}
	}
	if seg.samples == 0 {
		return fmp4Seg{}, errors.New("conform: segment: no samples")
	}
	return seg, nil
}

// fullSamples returns a fragment's samples, turning a decoder panic into an
// error.
func fullSamples(frag *mp4.Fragment, trex *mp4.TrexBox) (s []mp4.FullSample, err error) {
	defer func() {
		if r := recover(); r != nil {
			s, err = nil, fmt.Errorf("samples: %v", r)
		}
	}()
	return frag.GetFullSamples(trex)
}

func sampleFrame(s mp4.FullSample, in fmp4Init, video bool) (media.Frame, error) {
	ts := int(in.timescale)
	dts := media.SampleTime(int64(s.DecodeTime), ts)
	fr := media.Frame{Config: in.cfg, DTS: dts, PTS: dts, Key: true}
	if !video {
		fr.Data = [][]byte{s.Data}
		return fr, nil
	}
	nalus, err := splitNALUs(s.Data, in.nalLen)
	if err != nil {
		return media.Frame{}, err
	}
	fr.PTS = media.SampleTime(s.PresentationTime(), ts)
	fr.Key = s.Flags&sampleNonSync == 0
	fr.RASL = isRASL(in.cfg.Codec, nalus)
	fr.Data = nalus
	return fr, nil
}
