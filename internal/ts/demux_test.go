package ts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/asticode/go-astits"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return b
}

func demuxAll(t *testing.T, data []byte) (*Demuxer, []media.Frame) {
	t.Helper()
	d, err := NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	var frames []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return d, frames
		}
		require.NoError(t, err)
		frames = append(frames, f)
	}
}

// checkRaw runs the frame checks on raw demuxer output. It skips
// conform.RuleConfigChange: the demuxer emits source-time frames with Epoch
// 0, so a config change (as in h264_aac_then_640.ts) is never at an epoch
// start until the Clock assigns epochs.
func checkRaw(frames []media.Frame) []conform.Violation {
	return conform.CheckFrames(frames, conform.FrameOptions{Skip: []string{conform.RuleConfigChange}})
}

func byTrack(frames []media.Frame, track int) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

func TestDemuxH264AAC(t *testing.T) {
	d, frames := demuxAll(t, fixture(t, "h264_aac.ts"))
	assert.Equal(t, 2, d.NumTracks())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	v, a := byTrack(frames, 0), byTrack(frames, 1)
	require.Len(t, v, 150)
	require.Len(t, a, 283)
	assert.True(t, v[0].Key)
	assert.Equal(t, media.CodecH264, v[0].Config.Codec)
	assert.Equal(t, 320, v[0].Config.Width)
	assert.Equal(t, 240, v[0].Config.Height)
	assert.Equal(t, 48000, a[0].Config.SampleRate)
	assert.Equal(t, 2, a[0].Config.Channels)
	for _, f := range v {
		for _, n := range f.Data {
			typ := n[0] & 0x1f
			assert.NotContains(t, []byte{7, 8, 9}, typ)
		}
	}
	for i := 1; i < len(a); i++ {
		assert.Equal(t, int64(1920), a[i].DTS-a[i-1].DTS)
		assert.True(t, a[i].Key)
		assert.False(t, a[i].RASL)
	}
	assert.Nil(t, checkRaw(frames))
}

func TestDemuxH265AAC51(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h265_aac51.ts"))
	v, a := byTrack(frames, 0), byTrack(frames, 1)
	require.NotEmpty(t, v)
	assert.Equal(t, media.CodecH265, v[0].Config.Codec)
	assert.Len(t, v[0].Config.VPS, 1)
	require.Len(t, a, 260)
	assert.Equal(t, 44100, a[0].Config.SampleRate)
	assert.Equal(t, 6, a[0].Config.Channels)
	want := media.SampleTime(1024, 44100)
	for i := 1; i < len(a); i++ {
		delta := a[i].DTS - a[i-1].DTS
		assert.InDelta(t, want, delta, 1)
	}
	for _, f := range v {
		for _, n := range f.Data {
			typ := (n[0] >> 1) & 0x3f
			assert.NotContains(t, []byte{32, 33, 34, 35}, typ)
		}
	}
	assert.Nil(t, checkRaw(frames))
}

func TestDemuxH265RASL(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h265_aac51.ts"))
	rasl := 0
	lastKeyCRA := false
	seenKey := false
	for _, f := range byTrack(frames, 0) {
		if f.Key {
			seenKey = true
			lastKeyCRA = false
			for _, n := range f.Data {
				if (n[0]>>1)&0x3f == 21 {
					lastKeyCRA = true
				}
			}
			assert.False(t, f.RASL)
			continue
		}
		if f.RASL {
			rasl++
			assert.True(t, seenKey && lastKeyCRA, "RASL frame must follow a CRA keyframe")
		}
	}
	assert.GreaterOrEqual(t, rasl, 1)
}

func TestDemuxWrap(t *testing.T) {
	data := fixture(t, "h264_aacmono_wrap.ts")
	_, frames := demuxAll(t, data)
	for tr := 0; tr < 2; tr++ {
		fs := byTrack(frames, tr)
		require.NotEmpty(t, fs)
		for i := 1; i < len(fs); i++ {
			assert.Greater(t, fs[i].DTS, fs[i-1].DTS)
		}
	}
	v := byTrack(frames, 0)
	span := v[len(v)-1].DTS - v[0].DTS
	assert.Greater(t, span, int64(5.8*90000))
	assert.Less(t, span, int64(6*90000))

	ad := astits.NewDemuxer(context.Background(), bytes.NewReader(data))
	for {
		dd, err := ad.NextData()
		require.NoError(t, err)
		if dd.PES != nil && dd.PES.Header.StreamID>>4 == 0xE {
			assert.Greater(t, dd.PES.Header.OptionalHeader.PTS.Base, int64(1<<33-10*90000))
			break
		}
	}
}

func TestDemuxLoop(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac_loop.ts"))
	for tr := 0; tr < 2; tr++ {
		fs := byTrack(frames, tr)
		back := 0
		for i := 1; i < len(fs); i++ {
			if fs[i].DTS < fs[i-1].DTS {
				back++
			}
		}
		assert.Equal(t, 1, back, "track %d", tr)
	}
}

func TestDemuxConfigChange(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac_then_640.ts"))
	var cfgs []*media.TrackConfig
	for _, f := range byTrack(frames, 0) {
		if len(cfgs) == 0 || cfgs[len(cfgs)-1] != f.Config {
			cfgs = append(cfgs, f.Config)
		}
	}
	require.Len(t, cfgs, 2)
	assert.Equal(t, 320, cfgs[0].Width)
	assert.Equal(t, 240, cfgs[0].Height)
	assert.Equal(t, 640, cfgs[1].Width)
	assert.Equal(t, 360, cfgs[1].Height)
}

func TestDemuxMidGOP(t *testing.T) {
	d, frames := demuxAll(t, fixture(t, "h264_aac_midgop.ts"))
	assert.Greater(t, d.Stats().DroppedNoConfig, uint64(0))
	firstVideo := -1
	for i, f := range frames {
		if f.Track == 0 {
			firstVideo = i
			assert.True(t, f.Key)
			break
		}
	}
	require.GreaterOrEqual(t, firstVideo, 1)
	assert.Equal(t, 1, frames[0].Track)
}

func TestDemuxSkipsUnsupported(t *testing.T) {
	d, _ := demuxAll(t, fixture(t, "h264_mp2.ts"))
	assert.Equal(t, 1, d.NumTracks())
	require.Len(t, d.Skipped(), 1)
	assert.Contains(t, d.Skipped()[0], "mpeg audio")
}

func TestDemuxTruncated(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac.ts")[:100077])
	assert.GreaterOrEqual(t, len(byTrack(frames, 0)), 40)
	assert.GreaterOrEqual(t, len(byTrack(frames, 1)), 70)
	for _, f := range frames {
		assert.NotEmpty(t, f.Data)
	}
	assert.Nil(t, checkRaw(frames))
}

func TestDemuxGarbage(t *testing.T) {
	b := make([]byte, 64<<10)
	rand.New(rand.NewSource(1)).Read(b)
	_, err := NewDemuxer(bytes.NewReader(b))
	assert.Error(t, err)
}

func TestDemuxEmpty(t *testing.T) {
	_, err := NewDemuxer(bytes.NewReader(nil))
	assert.Error(t, err)
	_, err = NewDemuxer(bytes.NewReader(fixture(t, "h264_aac.ts")[:100]))
	assert.Error(t, err)
}

func TestDemuxBadConfigDropsFrames(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac.ts"))
	cfg := frames[0].Config
	require.Equal(t, media.CodecH264, cfg.Codec)
	sc := []byte{0, 0, 0, 1}
	au := func(sps []byte) []byte {
		var b []byte
		for _, n := range [][]byte{sps, cfg.PPS[0], {0x65, 0x88, 0x84, 0x00}} {
			b = append(b, sc...)
			b = append(b, n...)
		}
		return b
	}
	tr := &track{
		codec: media.CodecH264, kind: media.KindVideo,
		vps: map[uint32][]byte{}, sps: map[uint32][]byte{}, pps: map[uint32][]byte{},
	}
	d := &Demuxer{tracks: []*track{tr}, byPID: map[uint16]*track{}}

	d.handleVideo(tr, au(cfg.SPS[0]), 0, 0)
	require.Len(t, d.queue, 1)
	good := d.queue[0].Config
	require.NotNil(t, good)

	// Truncated SPS: its ID cannot be read, so the config is invalidated.
	d.handleVideo(tr, au(cfg.SPS[0][:5]), 3600, 3600)
	assert.Len(t, d.queue, 1, "frame with unparsable SPS must not be emitted")
	assert.Equal(t, uint64(1), d.stats.DroppedNoConfig)

	// A repaired SPS restores output with the same config pointer as before,
	// so the Clock sees no configuration change.
	d.handleVideo(tr, au(cfg.SPS[0]), 7200, 7200)
	require.Len(t, d.queue, 2)
	assert.Same(t, good, d.queue[1].Config)
}
