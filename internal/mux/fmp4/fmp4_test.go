package fmp4_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

func demux(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

// shift moves every timestamp forward so fixture frames, which start at a
// negative DTS, fit the unsigned tfdt.
func shift(frames []media.Frame, by int64) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		out[i].PTS += by
		out[i].DTS += by
	}
	return out
}

func byCodec(frames []media.Frame, c media.Codec) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Config != nil && f.Config.Codec == c {
			out = append(out, f)
		}
	}
	return out
}

func decode(t *testing.T, parts ...[]byte) *mp4.File {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(bytes.Join(parts, nil)))
	require.NoError(t, err)
	return f
}

func TestInitVideo(t *testing.T) {
	for _, tt := range []struct {
		fixture string
		codec   media.Codec
		w, h    int
	}{
		{"h264_aac.ts", media.CodecH264, 320, 240},
		{"h265_aac51.ts", media.CodecH265, 320, 240},
	} {
		t.Run(tt.codec.String(), func(t *testing.T) {
			cfg := byCodec(demux(t, tt.fixture), tt.codec)[0].Config
			assert.Equal(t, uint32(90000), fmp4.Timescale(cfg))
			b, err := fmp4.Init(cfg)
			require.NoError(t, err)
			f := decode(t, b)
			require.NotNil(t, f.Init)
			trak := f.Init.Moov.Trak
			assert.Equal(t, uint32(1), trak.Tkhd.TrackID)
			stsd := trak.Mdia.Minf.Stbl.Stsd
			if tt.codec == media.CodecH264 {
				require.NotNil(t, stsd.AvcX)
				assert.Equal(t, uint16(tt.w), stsd.AvcX.Width)
				assert.Equal(t, uint16(tt.h), stsd.AvcX.Height)
				assert.Equal(t, cfg.SPS, stsd.AvcX.AvcC.SPSnalus)
				assert.Equal(t, cfg.PPS, stsd.AvcX.AvcC.PPSnalus)
			} else {
				require.NotNil(t, stsd.HvcX)
				assert.Equal(t, uint16(tt.w), stsd.HvcX.Width)
				assert.Equal(t, uint16(tt.h), stsd.HvcX.Height)
				arrays := map[int][][]byte{}
				for _, a := range stsd.HvcX.HvcC.NaluArrays {
					arrays[int(a.NaluType())] = a.Nalus
				}
				assert.Equal(t, cfg.VPS, arrays[32])
				assert.Equal(t, cfg.SPS, arrays[33])
				assert.Equal(t, cfg.PPS, arrays[34])
			}
		})
	}
}

func audioCfg(rate, ch int, asc []byte) *media.TrackConfig {
	return &media.TrackConfig{Codec: media.CodecAAC, SampleRate: rate, Channels: ch, ASC: asc}
}

func TestInitAACChannels(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fixture  string
		rate, ch int
	}{
		{"mono44k", "h264_aacmono_wrap.ts", 44100, 1},
		{"5.1", "h265_aac51.ts", 44100, 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := byCodec(demux(t, tt.fixture), media.CodecAAC)[0].Config
			assert.Equal(t, tt.ch, cfg.Channels)
			assert.Equal(t, uint32(tt.rate), fmp4.Timescale(cfg))
			b, err := fmp4.Init(cfg)
			require.NoError(t, err)
			f := decode(t, b)
			trak := f.Init.Moov.Trak
			assert.Equal(t, uint32(tt.rate), trak.Mdia.Mdhd.Timescale)
			mp4a := trak.Mdia.Minf.Stbl.Stsd.Mp4a
			require.NotNil(t, mp4a)
			assert.Equal(t, uint16(tt.ch), mp4a.ChannelCount)
			assert.Equal(t, uint16(tt.rate), mp4a.SampleRate)
			assert.Equal(t, cfg.ASC, mp4a.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig)
		})
	}
}

func TestInitRateTooHigh(t *testing.T) {
	_, err := fmp4.Init(audioCfg(96000, 2, []byte{0x11, 0x90}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fmp4:")
}

func TestInitUnknownCodec(t *testing.T) {
	_, err := fmp4.Init(&media.TrackConfig{})
	require.Error(t, err)
	_, _, err = fmp4.Segment([]media.Frame{{}}, &media.TrackConfig{}, 1)
	require.Error(t, err)
}

func TestSegmentVideo(t *testing.T) {
	frames := byCodec(demux(t, "h264_aac.ts"), media.CodecH264)
	require.Greater(t, len(frames), 10)
	frames = shift(frames[:30], 90000)
	cfg := frames[0].Config
	init, err := fmp4.Init(cfg)
	require.NoError(t, err)
	seg, tm, err := fmp4.Segment(frames, cfg, 7)
	require.NoError(t, err)

	f := decode(t, init, seg)
	require.Len(t, f.Segments, 1)
	frag := f.Segments[0].Fragments[0]
	assert.Equal(t, uint32(7), frag.Moof.Mfhd.SequenceNumber)
	samples, err := frag.GetFullSamples(f.Init.Moov.Mvex.Trex)
	require.NoError(t, err)
	require.Len(t, samples, len(frames))

	minPTS := frames[0].PTS
	var total uint64
	for i, s := range samples {
		fr := frames[i]
		assert.Equal(t, uint64(fr.DTS), s.DecodeTime, "dts %d", i)
		assert.Equal(t, fr.PTS, int64(s.DecodeTime)+int64(s.CompositionTimeOffset), "pts %d", i)
		assert.Equal(t, fr.Key, s.IsSync(), "sync %d", i)
		size := 0
		for _, n := range fr.Data {
			size += 4 + len(n)
		}
		assert.Equal(t, size, len(s.Data))
		if i+1 < len(frames) {
			assert.Equal(t, uint32(frames[i+1].DTS-fr.DTS), s.Dur)
		} else {
			assert.Equal(t, samples[i-1].Dur, s.Dur)
		}
		total += uint64(s.Dur)
		minPTS = min(minPTS, fr.PTS)
	}
	assert.Equal(t, uint64(frames[0].DTS), tm.BaseTime)
	assert.Equal(t, uint64(minPTS), tm.Earliest)
	assert.Equal(t, total, tm.Duration)
}

func TestSegmentSingleVideoFrameDuration(t *testing.T) {
	frames := byCodec(demux(t, "h264_aac.ts"), media.CodecH264)[:1]
	frames = shift(frames, 90000)
	_, tm, err := fmp4.Segment(frames, frames[0].Config, 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(3600), tm.Duration)
}

func aacFrames(cfg *media.TrackConfig, start int64, n int, skip func(i int) bool) []media.Frame {
	var out []media.Frame
	for i := 0; i < n; i++ {
		if skip != nil && skip(i) {
			continue
		}
		dts := start + media.SampleTime(int64(i)*1024, cfg.SampleRate)
		out = append(out, media.Frame{Config: cfg, PTS: dts, DTS: dts, Key: true, Data: [][]byte{{1, 2, 3, byte(i)}}})
	}
	return out
}

func TestSegmentAudio(t *testing.T) {
	for _, rate := range []int{48000, 44100} {
		asc := []byte{0x12, 0x10}
		if rate == 44100 {
			asc = []byte{0x12, 0x08}
		}
		cfg := audioCfg(rate, 2, asc)
		frames := aacFrames(cfg, 1_000_000, 50, nil)
		init, err := fmp4.Init(cfg)
		require.NoError(t, err)
		seg, tm, err := fmp4.Segment(frames, cfg, 1)
		require.NoError(t, err)
		f := decode(t, init, seg)
		samples, err := f.Segments[0].Fragments[0].GetFullSamples(f.Init.Moov.Mvex.Trex)
		require.NoError(t, err)
		require.Len(t, samples, 50)
		for i, s := range samples {
			assert.Equal(t, uint32(1024), s.Dur, "rate %d sample %d", rate, i)
			assert.Equal(t, uint32(4), s.Size)
			assert.Equal(t, int32(0), s.CompositionTimeOffset)
		}
		assert.Equal(t, uint64(50*1024), tm.Duration)
		assert.Equal(t, uint64(media.Rescale(1_000_000, rate)), tm.BaseTime)
		assert.Equal(t, tm.BaseTime, tm.Earliest)
	}
}

func TestSegmentAudioGap(t *testing.T) {
	cfg := audioCfg(48000, 2, []byte{0x11, 0x90})
	frames := aacFrames(cfg, 0, 40, func(i int) bool { return i >= 10 && i < 26 })
	require.Len(t, frames, 24)
	init, err := fmp4.Init(cfg)
	require.NoError(t, err)
	seg, _, err := fmp4.Segment(frames, cfg, 1)
	require.NoError(t, err)
	f := decode(t, init, seg)
	samples, err := f.Segments[0].Fragments[0].GetFullSamples(f.Init.Moov.Mvex.Trex)
	require.NoError(t, err)
	require.Len(t, samples, 24)
	// The sample before the gap spans it; the ones after keep their true time.
	assert.Equal(t, uint32(17*1024), samples[9].Dur)
	assert.Equal(t, uint64(26*1024), samples[10].DecodeTime)
	assert.Equal(t, uint32(1024), samples[10].Dur)
	assert.Equal(t, uint32(1024), samples[23].Dur)
}

func TestSegmentErrors(t *testing.T) {
	cfg := audioCfg(48000, 2, []byte{0x11, 0x90})
	mk := func(dts ...int64) []media.Frame {
		out := make([]media.Frame, 0, len(dts))
		for _, d := range dts {
			out = append(out, media.Frame{Config: cfg, PTS: d, DTS: d, Data: [][]byte{{1}}})
		}
		return out
	}
	_, _, err := fmp4.Segment(mk(-1920, 0), cfg, 1)
	require.Error(t, err, "negative decode time")
	_, _, err = fmp4.Segment(mk(1920, 1920), cfg, 1)
	require.Error(t, err, "non-increasing decode time")
	two := mk(0)
	two[0].Data = [][]byte{{1}, {2}}
	_, _, err = fmp4.Segment(two, cfg, 1)
	require.Error(t, err, "audio frame with several units")
	none := mk(0)
	none[0].Data = nil
	_, _, err = fmp4.Segment(none, cfg, 1)
	require.Error(t, err, "audio frame without data")
	_, _, err = fmp4.Segment(mk(0), audioCfg(96000, 2, nil), 1)
	require.Error(t, err)
	_, err = fmp4.Init(audioCfg(48000, 0, nil))
	require.Error(t, err)
}

func TestSegmentEmptyErrors(t *testing.T) {
	cfg := audioCfg(48000, 2, []byte{0x11, 0x90})
	_, _, err := fmp4.Segment(nil, cfg, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fmp4:")
}

func TestFFprobeReadsInitAndSegment(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	all := demux(t, "h264_aac.ts")
	for _, c := range []media.Codec{media.CodecH264, media.CodecAAC} {
		frames := byCodec(all, c)
		if c == media.CodecH264 {
			frames = frames[:30]
		}
		frames = shift(frames, 90000)
		cfg := frames[0].Config
		init, err := fmp4.Init(cfg)
		require.NoError(t, err)
		seg, _, err := fmp4.Segment(frames, cfg, 1)
		require.NoError(t, err)
		p := filepath.Join(t.TempDir(), "x.mp4")
		require.NoError(t, os.WriteFile(p, append(init, seg...), 0o600))
		out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", p).CombinedOutput()
		require.NoError(t, err, string(out))
		want := map[media.Codec]string{media.CodecH264: "h264", media.CodecAAC: "aac"}[c]
		assert.Contains(t, string(out), want)
	}
}
