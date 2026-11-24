//go:build libav

package native

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/h26x"
)

func TestMain(m *testing.M) {
	astiav.SetLogLevel(astiav.LogLevelError)
	os.Exit(m.Run())
}

// renditionDelay is D, added by the host before frames reach the pipeline.
const renditionDelay = 45000

// aacStep is one AAC frame at 48 kHz in 90 kHz ticks.
const aacStep = 1920

// source returns a fixture's frames as the host hands them to the pipeline:
// demuxed, through a Clock, and shifted by D.
func source(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/fixtures/" + name)
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		out = append(out, c.Push(f)...)
	}
	out = append(out, c.Flush()...)
	for i := range out {
		out[i].PTS += renditionDelay
		out[i].DTS += renditionDelay
	}
	return out
}

func newPipeline(t *testing.T, rungs []Rung, audio *Audio) *Pipeline {
	t.Helper()
	p, err := New(rungs, audio)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

// push feeds frames and returns the outputs.
func push(t *testing.T, p *Pipeline, frames []media.Frame) []Output {
	t.Helper()
	var out []Output
	for _, f := range frames {
		o, err := p.Push(f)
		require.NoError(t, err)
		out = append(out, o...)
	}
	return out
}

// run feeds frames, flushes and returns the outputs.
func run(t *testing.T, p *Pipeline, frames []media.Frame) []Output {
	t.Helper()
	out := push(t, p, frames)
	o, err := p.Flush()
	require.NoError(t, err)
	return append(out, o...)
}

func ofKind(frames []media.Frame, k media.Kind) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Kind() == k {
			out = append(out, f)
		}
	}
	return out
}

// rungFrames returns the frames of one rung (−1: audio), in output order.
func rungFrames(outs []Output, rung int) []media.Frame {
	var out []media.Frame
	for _, o := range outs {
		if o.Rung == rung {
			out = append(out, o.F)
		}
	}
	return out
}

func trackFrames(frames []media.Frame, track int) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

// rendition returns what a rendition hub holds for a rung: its video and
// every audio frame, merged by DTS.
func rendition(outs []Output, rung int) []media.Frame {
	out := append(rungFrames(outs, rung), rungFrames(outs, -1)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DTS < out[j].DTS })
	return out
}

func keys(frames []media.Frame) []int64 {
	var out []int64
	for _, f := range frames {
		if f.Key && f.Kind() == media.KindVideo {
			out = append(out, f.PTS)
		}
	}
	slices.Sort(out)
	return out
}

func requireConforms(t *testing.T, frames []media.Frame) {
	t.Helper()
	v := conform.CheckFrames(frames, conform.FrameOptions{})
	require.Empty(t, v, "violations: %v", v)
}

// requireStep checks that consecutive PTS differ by one AAC frame at 48 kHz.
func requireStep(t *testing.T, frames []media.Frame) {
	t.Helper()
	require.NotEmpty(t, frames)
	for i := 1; i < len(frames); i++ {
		require.Equal(t, int64(aacStep), frames[i].PTS-frames[i-1].PTS, "frame %d", i)
	}
}

func hasIDR(f media.Frame) bool {
	for _, n := range f.Data {
		if len(n) > 0 && n[0]&0x1f == 5 {
			return true
		}
	}
	return false
}

// x264Params carries b-pyramid=none (ruling R-T7b'): with B-frames 2 the
// output SPS announces a reorder depth of 1, not the B-pyramid's 2, which
// VLC 3 failed to decode without a "buffer deadlock".
func TestPipelineNoBPyramid(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150, BFrames: 2}}, nil)
	v := rungFrames(run(t, p, src), 0)
	require.NotEmpty(t, v)
	require.NotEmpty(t, v[0].Config.SPS)
	sps, err := h26x.ParseSPS(h26x.H264, v[0].Config.SPS[0])
	require.NoError(t, err)
	assert.Equal(t, 1, sps.ReorderFrames, "max_num_reorder_frames")
	reordered := 0
	for _, f := range v {
		if f.PTS > f.DTS {
			reordered++
		}
	}
	assert.Positive(t, reordered, "B-frames are still used")
}

func TestPipelineLadder(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600}, {Height: 180, BitrateKbps: 300}, {Height: 120, BitrateKbps: 150}},
		&Audio{BitrateKbps: 96})
	outs := run(t, p, src)

	srcVideo := ofKind(src, media.KindVideo)
	srcKeys := keys(srcVideo)
	t.Logf("source: %d video, %d audio, keys %v", len(srcVideo), len(ofKind(src, media.KindAudio)), srcKeys)
	dims := [][2]int{{320, 240}, {240, 180}, {160, 120}}
	for i, want := range dims {
		v := rungFrames(outs, i)
		requireConforms(t, rendition(outs, i))
		require.NotEmpty(t, v)
		assert.Equal(t, srcKeys, keys(v), "rung %d keys", i)
		assert.Len(t, v, len(srcVideo), "rung %d count", i)
		assert.Equal(t, want, [2]int{v[0].Config.Width, v[0].Config.Height}, "rung %d dims", i)
		for _, f := range v {
			require.Same(t, v[0].Config, f.Config)
			require.Equal(t, 0, f.Track)
		}
		t.Logf("rung %d: %dx%d, %d frames, %d keys", i, v[0].Config.Width, v[0].Config.Height, len(v), len(keys(v)))
	}
	a := rungFrames(outs, -1)
	requireStep(t, a)
	assert.Equal(t, 48000, a[0].Config.SampleRate)
	assert.Equal(t, 2, a[0].Config.Channels)
	t.Logf("audio: %d frames, first PTS %d (source first audio PTS %d)", len(a), a[0].PTS, ofKind(src, media.KindAudio)[0].PTS)
	assert.Equal(t, Stats{}, p.Stats())
}

func TestPipelineResolutionChange(t *testing.T) {
	src := source(t, "h264_aac_then_640.ts")
	p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600}, {Height: 180, BitrateKbps: 300}}, &Audio{})
	outs := run(t, p, src)

	requireConforms(t, rendition(outs, 0))
	requireConforms(t, rendition(outs, 1))
	v := rungFrames(outs, 1)
	require.NotEmpty(t, v)
	first := v[0].Epoch
	second := -1
	for i, f := range v {
		if f.Epoch != first {
			second = i
			break
		}
	}
	require.Positive(t, second, "no second epoch")
	assert.Equal(t, [2]int{240, 180}, [2]int{v[0].Config.Width, v[0].Config.Height})
	assert.Equal(t, [2]int{320, 180}, [2]int{v[second].Config.Width, v[second].Config.Height})
	assert.True(t, v[second].Key)
	for i := 1; i < len(v); i++ {
		if i == second {
			assert.NotSame(t, v[i-1].Config, v[i].Config)
			continue
		}
		require.Same(t, v[i-1].Config, v[i].Config, "config changed at frame %d", i)
	}
	srcVideo := ofKind(src, media.KindVideo)
	assert.Equal(t, keys(srcVideo), keys(v))
	assert.Len(t, v, len(srcVideo))
	t.Logf("180 rung: epoch %d %d frames at 240x180, epoch %d %d frames at 320x180",
		first, second, v[second].Epoch, len(v)-second)
}

func TestPipelineHEVCOpenGOP(t *testing.T) {
	src := source(t, "h265_aac51.ts")
	p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600}, {Height: 120, BitrateKbps: 150}}, &Audio{})
	outs := run(t, p, src)

	srcVideo := ofKind(src, media.KindVideo)
	for i := range 2 {
		requireConforms(t, rendition(outs, i))
		v := rungFrames(outs, i)
		assert.Equal(t, keys(srcVideo), keys(v), "rung %d keys", i)
		assert.Len(t, v, len(srcVideo))
	}
	a := rungFrames(outs, -1)
	require.NotEmpty(t, a)
	assert.Equal(t, 48000, a[0].Config.SampleRate)
	assert.Equal(t, 2, a[0].Config.Channels)
	srcAudio := ofKind(src, media.KindAudio)
	t.Logf("source %d video (keys %v), %d audio at %d Hz x %d; out %d video, %d audio",
		len(srcVideo), keys(srcVideo), len(srcAudio), srcAudio[0].Config.SampleRate, srcAudio[0].Config.Channels,
		len(rungFrames(outs, 0)), len(a))

	// Starting the pipeline at a later CRA: its RASL pictures are not sent
	// to the decoder, so the rung starts on that CRA's PTS.
	t.Run("start at a CRA", func(t *testing.T) {
		video := ofKind(src, media.KindVideo)
		k := slices.IndexFunc(video[1:], func(f media.Frame) bool { return f.Key }) + 1
		require.Positive(t, k)
		rasl := 0
		for _, f := range video[k+1:] {
			if !f.RASL {
				break
			}
			rasl++
		}
		require.Positive(t, rasl, "no RASL after the second CRA")
		p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150}}, nil)
		v := rungFrames(run(t, p, video[k:]), 0)
		requireConforms(t, v)
		require.NotEmpty(t, v)
		assert.Equal(t, video[k].PTS, v[0].PTS)
		assert.True(t, v[0].Key)
		assert.Len(t, v, len(video)-k-rasl)
		assert.Equal(t, keys(video[k:]), keys(v))
		t.Logf("start at CRA %d (decode index %d), %d RASL skipped, %d out", video[k].PTS, k, rasl, len(v))
	})
}

func TestPipelineOpenGOPH264(t *testing.T) {
	src := source(t, "h264_opengop.ts")
	p := newPipeline(t, []Rung{{Height: 180, BitrateKbps: 300}}, nil)
	outs := run(t, p, src)

	requireConforms(t, rendition(outs, 0))
	v := rungFrames(outs, 0)
	srcVideo := ofKind(src, media.KindVideo)
	srcKeys := keys(srcVideo)
	require.Len(t, srcKeys, 6)
	assert.Equal(t, srcKeys, keys(v))
	for _, f := range v {
		if f.Key {
			assert.True(t, hasIDR(f), "key at %d is not an IDR", f.PTS)
		}
	}
	rasl := 0
	for _, f := range srcVideo {
		if f.RASL {
			rasl++
		}
	}
	assert.Len(t, v, len(srcVideo))
	t.Logf("keys %v; %d source frames (%d RASL), %d out", srcKeys, len(srcVideo), rasl, len(v))
}

func TestPipelinePyramid(t *testing.T) {
	src := source(t, "h264_pyramid.ts")
	p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600, BFrames: 3}, {Height: 120, BitrateKbps: 150}}, &Audio{})
	outs := run(t, p, src)

	for i := range 2 {
		v := rungFrames(outs, i)
		require.NotEmpty(t, v)
		for j, f := range v {
			require.GreaterOrEqual(t, f.PTS, f.DTS, "rung %d frame %d", i, j)
			if j > 0 {
				require.Greater(t, f.DTS, v[j-1].DTS, "rung %d frame %d", i, j)
			}
		}
		requireConforms(t, rendition(outs, i))
		assert.Equal(t, keys(ofKind(src, media.KindVideo)), keys(v))
	}
}

func TestPipelineNoBFrames(t *testing.T) {
	src := source(t, "h264_aac_nob.ts")
	srcVideo := ofKind(src, media.KindVideo)
	for _, f := range srcVideo {
		require.Equal(t, f.PTS, f.DTS, "fixture has B-frames")
	}
	p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600}}, &Audio{})
	outs := run(t, p, src)

	requireConforms(t, rendition(outs, 0))
	v := rungFrames(outs, 0)
	minDTS, reordered := v[0].DTS, 0
	for _, f := range v {
		minDTS = min(minDTS, f.DTS)
		if f.PTS > f.DTS {
			reordered++
		}
	}
	assert.GreaterOrEqual(t, minDTS, int64(0))
	assert.Positive(t, reordered, "the encoder made no B-frames")
	assert.Equal(t, keys(srcVideo), keys(v))
	t.Logf("source first DTS %d; out min DTS %d, %d of %d frames with PTS > DTS", srcVideo[0].DTS, minDTS, reordered, len(v))
}

func TestPipelineVideoOnly(t *testing.T) {
	src := source(t, "h264_640.ts")
	require.Empty(t, ofKind(src, media.KindAudio))
	p := newPipeline(t, []Rung{{Height: 180, BitrateKbps: 300}}, &Audio{})
	outs := run(t, p, src)

	assert.Empty(t, rungFrames(outs, -1))
	v := rungFrames(outs, 0)
	requireConforms(t, v)
	assert.Len(t, v, len(src))
	assert.Equal(t, [2]int{320, 180}, [2]int{v[0].Config.Width, v[0].Config.Height})
}

func TestPipelineAudioOnly(t *testing.T) {
	src := ofKind(source(t, "h264_aac.ts"), media.KindAudio)
	p := newPipeline(t, []Rung{{Height: 180, BitrateKbps: 300}}, &Audio{})
	half := len(src) / 2
	before := push(t, p, src[:half])
	drained, err := p.Discontinuity()
	require.NoError(t, err)
	before = append(before, drained...)
	after := run(t, p, src[half:])

	for _, o := range append(before, after...) {
		require.Equal(t, -1, o.Rung)
	}
	a1, a2 := rungFrames(before, -1), rungFrames(after, -1)
	require.NotEmpty(t, a1)
	require.NotEmpty(t, a2)
	requireStep(t, a1)
	requireStep(t, a2)
	assert.Len(t, a1, half)
	assert.Len(t, a2, len(src)-half)
	// Output resumes at the first frame after the discontinuity.
	assert.InDelta(t, src[half].PTS, a2[0].PTS, 1)
	assert.InDelta(t, src[half-1].PTS, a1[len(a1)-1].PTS, 1)
	requireConforms(t, append(a1, a2...))
	t.Logf("%d frames before, %d after; resumed at %d (input %d)", len(a1), len(a2), a2[0].PTS, src[half].PTS)
}

func TestPipelineTwoAudio(t *testing.T) {
	var src []media.Frame
	for _, f := range source(t, "h264_aac.ts") {
		src = append(src, f)
		if f.Kind() == media.KindAudio {
			f.Track = 2
			src = append(src, f)
		}
	}
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150}}, &Audio{})
	outs := run(t, p, src)

	a := rungFrames(outs, -1)
	a1, a2 := trackFrames(a, 1), trackFrames(a, 2)
	require.NotEmpty(t, a1)
	require.Len(t, a2, len(a1))
	requireStep(t, a1)
	requireStep(t, a2)
	require.NotNil(t, a1[0].Config)
	require.NotNil(t, a2[0].Config)
	assert.NotSame(t, a1[0].Config, a2[0].Config)
	for i := range a1 {
		require.Same(t, a1[0].Config, a1[i].Config)
		require.Same(t, a2[0].Config, a2[i].Config)
	}
	requireConforms(t, rendition(outs, 0))
}

func TestPipelineAudioGap(t *testing.T) {
	audio := ofKind(source(t, "h264_aac.ts"), media.KindAudio)
	const at = 100

	t.Run("gap", func(t *testing.T) {
		src := slices.Concat(audio[:at], audio[at+10:])
		p := newPipeline(t, nil, &Audio{})
		a := rungFrames(run(t, p, src), -1)
		assert.Equal(t, uint64(1), p.Stats().Reanchors)
		assert.Equal(t, uint64(0), p.Stats().AudioDropped)
		requireConforms(t, a)
		i := slices.IndexFunc(a, func(f media.Frame) bool { return f.PTS > audio[at-1].PTS+aacStep/2 })
		require.Positive(t, i)
		assert.InDelta(t, audio[at+10].PTS, a[i].PTS, aacStep)
		t.Logf("first after the gap: out %d, input %d", a[i].PTS, audio[at+10].PTS)
	})

	t.Run("backward", func(t *testing.T) {
		src := slices.Clone(audio)
		for i := at; i < len(src); i++ {
			src[i].PTS -= 3 * aacStep
			src[i].DTS -= 3 * aacStep
		}
		p := newPipeline(t, nil, &Audio{})
		a := rungFrames(run(t, p, src), -1)
		for i := 1; i < len(a); i++ {
			require.Greater(t, a[i].PTS, a[i-1].PTS, "frame %d", i)
		}
		assert.GreaterOrEqual(t, p.Stats().AudioDropped, uint64(3*1024))
		assert.Equal(t, uint64(0), p.Stats().Reanchors)
		t.Logf("dropped %d samples; %d in, %d out", p.Stats().AudioDropped, len(src), len(a))
	})

	t.Run("jitter", func(t *testing.T) {
		src := slices.Clone(audio)
		for i := range src {
			j := int64(90)
			if i%2 == 1 {
				j = -90
			}
			src[i].PTS += j
			src[i].DTS += j
		}
		p := newPipeline(t, nil, &Audio{})
		a := rungFrames(run(t, p, src), -1)
		assert.Equal(t, uint64(0), p.Stats().AudioDropped)
		assert.Equal(t, uint64(0), p.Stats().Reanchors)
		assert.Len(t, a, len(src))
		requireStep(t, a)
	})
}

func TestPipelineSecondVideoDropped(t *testing.T) {
	var src []media.Frame
	for _, f := range source(t, "h264_aac.ts") {
		src = append(src, f)
		if f.Kind() == media.KindVideo {
			f.Track = 2
			src = append(src, f)
		}
	}
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150}}, &Audio{})
	outs := run(t, p, src)

	assert.Equal(t, uint64(1), p.Stats().DroppedTracks)
	v := rungFrames(outs, 0)
	assert.Len(t, v, len(ofKind(src, media.KindVideo))/2)
	for _, f := range v {
		require.Equal(t, 0, f.Track)
	}
	requireConforms(t, rendition(outs, 0))
}

func TestPipelineDiscontinuity(t *testing.T) {
	src := source(t, "h264_aac.ts")
	// Cut after video frame 35 (mid-GOP), and resume at video frame 40.
	var cut, resume, nv int
	for i, f := range src {
		if f.Kind() != media.KindVideo {
			continue
		}
		switch nv {
		case 35:
			cut = i + 1
		case 40:
			resume = i
		}
		nv++
	}
	srcVideo := ofKind(src, media.KindVideo)
	nextKey := srcVideo[50]
	require.True(t, nextKey.Key)
	require.False(t, srcVideo[40].Key)

	p := newPipeline(t, []Rung{{Height: 180, BitrateKbps: 300}}, &Audio{})
	before := push(t, p, src[:cut])
	drained, err := p.Discontinuity()
	require.NoError(t, err)
	before = append(before, drained...)
	after := run(t, p, src[resume:])

	assert.Len(t, rungFrames(before, 0), 36)
	v := rungFrames(after, 0)
	require.NotEmpty(t, v)
	assert.True(t, v[0].Key)
	assert.Equal(t, nextKey.PTS, v[0].PTS)
	assert.Len(t, v, len(srcVideo)-50)
	for _, f := range rungFrames(after, -1) {
		require.GreaterOrEqual(t, f.PTS, nextKey.PTS)
	}
	all := append(before, after...)
	requireConforms(t, rendition(all, 0))
	t.Logf("before: %d video; after: first video PTS %d key %v (source key %d), %d video, first audio PTS %d",
		len(rungFrames(before, 0)), v[0].PTS, v[0].Key, nextKey.PTS, len(v), rungFrames(after, -1)[0].PTS)
}

// TestPacketPadding checks that decoder input packets carry
// AV_INPUT_BUFFER_PADDING_SIZE (64 in FFmpeg 8) zeroed bytes after the
// payload, which bitstream readers rely on.
func TestPacketPadding(t *testing.T) {
	require.GreaterOrEqual(t, inputPadding, 64)
	p := newInPacket()
	defer p.free()
	for _, n := range []int{0, 1, 7, 100, 4096, 3, 65536, 33} {
		data := bytes.Repeat([]byte{0xff}, n)
		require.NoError(t, p.fill(data, 1, 1))
		pad := p.padding()
		require.Len(t, pad, inputPadding, "size %d", n)
		require.Equal(t, make([]byte, inputPadding), pad, "size %d", n)
	}
}

// TestPipelineFrameRateAtCRA starts at a CRA followed by RASL pictures: the
// skipped pictures still count in the framerate estimate.
func TestPipelineFrameRateAtCRA(t *testing.T) {
	video := ofKind(source(t, "h265_aac51.ts"), media.KindVideo)
	k := slices.IndexFunc(video[1:], func(f media.Frame) bool { return f.Key }) + 1
	require.Positive(t, k)
	require.True(t, video[k+1].RASL)
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150}}, nil)
	run(t, p, video[k:])
	assert.Equal(t, [2]int{25, 1}, [2]int{p.video.fps.Num(), p.video.fps.Den()})

	// The estimate is the median delta, so a missing frame does not move it.
	fps := estimateFPS([]int64{0, 3600, 7200, 18000, 21600, 25200, 28800, 32400, 36000})
	assert.Equal(t, [2]int{25, 1}, [2]int{fps.Num(), fps.Den()})
	fps = estimateFPS([]int64{0, 3003, 6006, 9009})
	assert.Equal(t, [2]int{30000, 1001}, [2]int{fps.Num(), fps.Den()})
	fps = estimateFPS([]int64{0})
	assert.Equal(t, [2]int{25, 1}, [2]int{fps.Num(), fps.Den()})
}

// TestPipelineRungOpenFailure: a rung that fails to open fails the whole
// pipeline, permanently, with no rung left open.
func TestPipelineRungOpenFailure(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150}, {Height: 120, BitrateKbps: 150, Preset: "no-such-preset"}}, &Audio{})
	var (
		first error
		n     int
	)
	for i, f := range src {
		o, err := p.Push(f)
		assert.Empty(t, rungFrames(o, 0))
		if err != nil {
			first, n = err, i
			break
		}
	}
	require.Error(t, first)
	assert.Nil(t, p.video.encs)
	assert.Nil(t, p.video.dec)
	_, err := p.Push(src[n+1])
	require.Error(t, err)
	assert.Equal(t, first, err)
	_, err = p.Discontinuity()
	assert.Equal(t, first, err)
	_, err = p.Flush()
	assert.Equal(t, first, err)
	t.Logf("frame %d: %v", n, first)
}

// gainControlAAC is one raw AAC-LC stereo frame (a CPE without a common
// window, max_sfb 0) with the SSR gain_control_data_present bit set and
// zeros after it. FFmpeg 8.1 parses gain control (it only warns that it is
// not implemented), so the frame fails later, on the zeros, as
// AVERROR_INVALIDDATA.
var gainControlAAC = []byte{
	// 001 0000 0 | 01100100 | 0 00 0 0000|00 0 0 0 1 00 | zero padding
	0x20, 0x64, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00,
}

// invalidAAC is one raw AAC frame with an SCE of tag 5, which a stereo
// stream does not have: AVERROR_INVALIDDATA.
var invalidAAC = []byte{0x0a, 0x64, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00}

// minusOneAAC is a fuzz-found raw AAC frame (a fill element with a broken
// extension payload) that the decoder rejects with a bare -1, EPERM.
var minusOneAAC = []byte{0xa7, 0x10, 0x68, 0x92, 0xae, 0x7d}

// adtsAAC starts with the ADTS sync word, so the decoder parses an ADTS
// header and rejects it with a private AAC_PARSE_ERROR code.
var adtsAAC = []byte{0xff, 0xff, 0xff, 0xff}

func TestIsCorruptData(t *testing.T) {
	assert.True(t, isCorruptData(astiav.ErrInvaliddata))
	assert.True(t, isCorruptData(astiav.ErrPatchwelcome))
	assert.True(t, isCorruptData(astiav.Error(-1)))
	assert.True(t, isCorruptData(astiav.Error(-0x3030c0a)))
	assert.True(t, isCorruptData(fmt.Errorf("wrapped: %w", astiav.ErrPatchwelcome)))
	assert.False(t, isCorruptData(nil))
	assert.False(t, isCorruptData(errNoMem))
	assert.False(t, isCorruptData(fmt.Errorf("wrapped: %w", errNoMem)))
	assert.False(t, isCorruptData(astiav.ErrEagain))
	assert.False(t, isCorruptData(astiav.ErrEof))
}

// TestPipelineAudioCorrupt: frames the AAC decoder rejects, whatever the
// error, are skipped and counted, and the pipeline keeps going.
func TestPipelineAudioCorrupt(t *testing.T) {
	audio := ofKind(source(t, "h264_aac.ts"), media.KindAudio)
	src := slices.Clone(audio)
	bad := map[int][]byte{50: gainControlAAC, 120: invalidAAC, 180: minusOneAAC, 240: adtsAAC}
	for i, d := range bad {
		src[i].Data = [][]byte{d}
	}
	p := newPipeline(t, nil, &Audio{})
	a := rungFrames(run(t, p, src), -1)
	assert.Equal(t, uint64(len(bad)), p.Stats().AudioCorrupt)
	assert.Len(t, a, len(src)-len(bad))
	requireConforms(t, a)
	t.Logf("%d in, %d out, stats %+v", len(src), len(a), p.Stats())
}

// TestPipelineCorruptRunFails: 50 consecutive corrupt frames on one decoder
// fail the pipeline for good; 49 followed by a good frame do not.
func TestPipelineCorruptRunFails(t *testing.T) {
	audio := ofKind(source(t, "h264_aac.ts"), media.KindAudio)
	const at = 100
	corrupt := func(n int) []media.Frame {
		src := slices.Clone(audio)
		for i := at; i < at+n; i++ {
			src[i].Data = [][]byte{invalidAAC}
		}
		return src
	}

	t.Run("49 twice", func(t *testing.T) {
		// Two runs of 49, with one good frame between them that ends the
		// first run.
		src := corrupt(2*maxCorruptRun - 1)
		src[at+maxCorruptRun-1] = audio[at+maxCorruptRun-1]
		p := newPipeline(t, nil, &Audio{})
		a := rungFrames(run(t, p, src), -1)
		assert.Equal(t, uint64(2*(maxCorruptRun-1)), p.Stats().AudioCorrupt)
		assert.Len(t, a, len(audio)-2*(maxCorruptRun-1))
		requireConforms(t, a)
	})

	t.Run("50", func(t *testing.T) {
		p := newPipeline(t, nil, &Audio{})
		src := corrupt(maxCorruptRun)
		var (
			first error
			n     int
		)
		for i, f := range src {
			if _, err := p.Push(f); err != nil {
				first, n = err, i
				break
			}
		}
		require.Error(t, first)
		assert.Equal(t, at+maxCorruptRun-1, n)
		assert.Equal(t, uint64(maxCorruptRun), p.Stats().AudioCorrupt)
		_, err := p.Push(src[n+1])
		assert.Equal(t, first, err)
		_, err = p.Flush()
		assert.Equal(t, first, err)
		t.Logf("frame %d: %v", n, first)
	})
}

// TestPipelineVideoCorrupt: non-key frames with a broken slice header are
// rejected by the decoder, skipped and counted; the output goes on with
// every source key.
func TestPipelineVideoCorrupt(t *testing.T) {
	src := source(t, "h264_aac.ts")
	nv, broken := 0, 0
	for i, f := range src {
		if f.Kind() != media.KindVideo {
			continue
		}
		if nv >= 30 && nv < 34 && !f.Key {
			data := make([][]byte, len(f.Data))
			for j, n := range f.Data {
				n = bytes.Clone(n)
				if n[0]&0x1f == 1 { // a non-IDR slice: garble its header
					for k := 1; k < min(len(n), 6); k++ {
						n[k] = 0xff
					}
				}
				data[j] = n
			}
			src[i].Data = data
			broken++
		}
		nv++
	}
	require.Equal(t, 4, broken)
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150}}, &Audio{})
	outs := run(t, p, src)

	assert.Positive(t, p.Stats().VideoCorrupt)
	v := rungFrames(outs, 0)
	srcVideo := ofKind(src, media.KindVideo)
	assert.Equal(t, keys(srcVideo), keys(v))
	assert.GreaterOrEqual(t, len(v), len(srcVideo)-broken)
	assert.Equal(t, srcVideo[len(srcVideo)-1].PTS, slices.MaxFunc(v, func(a, b media.Frame) int { return int(a.PTS - b.PTS) }).PTS)
	requireConforms(t, rendition(outs, 0))
	t.Logf("%d broken, stats %+v, %d of %d frames out", broken, p.Stats(), len(v), len(srcVideo))
}
