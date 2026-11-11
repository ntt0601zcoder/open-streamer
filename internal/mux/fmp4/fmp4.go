// Package fmp4 writes single-track CMAF init and media segments.
package fmp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	videoTimescale  = 90000
	defaultVideoDur = 3600
	aacFrameSamples = 1024
	trackID         = 1
)

// Timing describes a segment in track timescale units.
type Timing struct {
	BaseTime, Earliest, Duration uint64
}

// Timescale returns the track timescale: 90000 for video, the sample rate for audio.
func Timescale(c *media.TrackConfig) uint32 {
	if c.Codec.Kind() == media.KindAudio {
		return uint32(c.SampleRate)
	}
	return videoTimescale
}

// Init writes the init segment of a single track with track ID 1.
func Init(c *media.TrackConfig) ([]byte, error) {
	init := mp4.CreateEmptyInit()
	switch c.Codec {
	case media.CodecH264, media.CodecH265:
		init.AddEmptyTrack(videoTimescale, "video", "und")
		trak := init.Moov.Trak
		var err error
		if c.Codec == media.CodecH264 {
			err = trak.SetAVCDescriptor("avc1", c.SPS, c.PPS, true)
		} else {
			err = trak.SetHEVCDescriptor("hvc1", c.VPS, c.SPS, c.PPS, nil, true)
		}
		if err != nil {
			return nil, fmt.Errorf("fmp4: init: video descriptor: %w", err)
		}
	case media.CodecAAC:
		if c.SampleRate <= 0 || c.SampleRate > 65535 {
			return nil, fmt.Errorf("fmp4: init: sample rate %d out of range", c.SampleRate)
		}
		if c.Channels <= 0 || c.Channels > 65535 {
			return nil, fmt.Errorf("fmp4: init: channel count %d out of range", c.Channels)
		}
		init.AddEmptyTrack(uint32(c.SampleRate), "audio", "und")
		entry := mp4.CreateAudioSampleEntryBox("mp4a", uint16(c.Channels), 16, uint16(c.SampleRate), mp4.CreateEsdsBox(c.ASC))
		init.Moov.Trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	default:
		return nil, fmt.Errorf("fmp4: init: unsupported codec %v", c.Codec)
	}
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, fmt.Errorf("fmp4: init: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// Segment writes one media segment of frames in decode order.
func Segment(frames []media.Frame, c *media.TrackConfig, seq uint32) ([]byte, Timing, error) {
	if len(frames) == 0 {
		return nil, Timing{}, errors.New("fmp4: segment: no frames")
	}
	audio := c.Codec.Kind() == media.KindAudio
	switch {
	case audio:
		if c.SampleRate <= 0 || c.SampleRate > 65535 {
			return nil, Timing{}, fmt.Errorf("fmp4: segment: sample rate %d out of range", c.SampleRate)
		}
	case c.Codec.Kind() == media.KindVideo:
	default:
		return nil, Timing{}, fmt.Errorf("fmp4: segment: unsupported codec %v", c.Codec)
	}
	frag, err := mp4.CreateFragment(seq, trackID)
	if err != nil {
		return nil, Timing{}, fmt.Errorf("fmp4: segment: fragment: %w", err)
	}
	decode := func(f media.Frame) int64 {
		if audio {
			return media.Rescale(f.DTS, c.SampleRate)
		}
		return f.DTS
	}
	var tm Timing
	var prevDur uint32
	for i, f := range frames {
		dt := decode(f)
		if dt < 0 {
			return nil, Timing{}, fmt.Errorf("fmp4: segment: negative decode time %d", dt)
		}
		var dur uint32
		switch {
		case i+1 < len(frames):
			next := decode(frames[i+1])
			if next <= dt {
				return nil, Timing{}, fmt.Errorf("fmp4: segment: decode time not increasing at frame %d", i)
			}
			dur = uint32(next - dt)
		case audio:
			dur = aacFrameSamples
		case prevDur != 0:
			dur = prevDur
		default:
			dur = defaultVideoDur
		}
		prevDur = dur

		var data []byte
		var cto int32
		flags := mp4.NonSyncSampleFlags
		if audio {
			if len(f.Data) != 1 {
				return nil, Timing{}, fmt.Errorf("fmp4: segment: audio frame %d has %d units", i, len(f.Data))
			}
			data = f.Data[0]
			flags = mp4.SyncSampleFlags
		} else {
			cto = int32(f.PTS - f.DTS)
			if f.Key {
				flags = mp4.SyncSampleFlags
			}
			n := 0
			for _, u := range f.Data {
				n += 4 + len(u)
			}
			data = make([]byte, 0, n)
			for _, u := range f.Data {
				data = binary.BigEndian.AppendUint32(data, uint32(len(u)))
				data = append(data, u...)
			}
		}
		frag.AddFullSample(mp4.FullSample{
			Sample:     mp4.Sample{Flags: flags, Dur: dur, Size: uint32(len(data)), CompositionTimeOffset: cto},
			DecodeTime: uint64(dt),
			Data:       data,
		})
		pt := uint64(int64(dt) + int64(cto))
		if i == 0 {
			tm.BaseTime, tm.Earliest = uint64(dt), pt
		} else {
			tm.Earliest = min(tm.Earliest, pt)
		}
		tm.Duration += uint64(dur)
	}
	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		return nil, Timing{}, fmt.Errorf("fmp4: segment: encode: %w", err)
	}
	return buf.Bytes(), tm, nil
}
