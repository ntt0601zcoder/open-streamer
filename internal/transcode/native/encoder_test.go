//go:build libav

package native

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/h26x"
)

// nvencOrSkip skips the test unless OS_NVENC=1 or OS_REQUIRE_NVENC=1
// (testpeer.NVENCOptIn: the probe opens an NVENC session, which an
// ordinary run on the GPU host must not do); opted in, it skips when no
// NVENC device can be opened, or fails when OS_REQUIRE_NVENC=1.
func nvencOrSkip(t *testing.T) {
	t.Helper()
	if !testpeer.NVENCOptedIn() {
		t.Skip(testpeer.NVENCOptIn)
	}
	if err := ProbeNVENC(); err != nil {
		if os.Getenv(testpeer.EnvRequireNVENC) == "1" {
			t.Fatalf("OS_REQUIRE_NVENC=1 but no NVENC device: %v", err)
		}
		t.Skipf("no NVENC device: %v", err)
	}
}

// setGPU makes every NVENC open in the test ask for GPU n. The test must
// not be parallel.
func setGPU(t *testing.T, n int) {
	t.Helper()
	old := nvencGPU
	nvencGPU = n
	t.Cleanup(func() { nvencGPU = old })
}

// warnings records the slog warnings logged while the test runs. The test
// must not be parallel.
type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }

func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		w.mu.Lock()
		w.msgs = append(w.msgs, r.Message)
		w.mu.Unlock()
	}
	return nil
}

func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }

// matching returns the recorded warnings that contain s.
func (w *warnings) matching(s string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, m := range w.msgs {
		if strings.Contains(m, s) {
			out = append(out, m)
		}
	}
	return out
}

func recordWarnings(t *testing.T) *warnings {
	t.Helper()
	w := &warnings{}
	old := slog.Default()
	slog.SetDefault(slog.New(w))
	t.Cleanup(func() { slog.SetDefault(old) })
	return w
}

// hasX264SEI reports whether the frame has a SEI NAL unit naming x264, as
// libx264 writes in its first keyframe.
func hasX264SEI(f media.Frame) bool {
	for _, n := range f.Data {
		if len(n) > 0 && n[0]&0x1f == 6 && bytes.Contains(n, []byte("x264")) {
			return true
		}
	}
	return false
}

// spsOf parses the first SPS of the frame's Config.
func spsOf(t *testing.T, f media.Frame) *h26x.SPS {
	t.Helper()
	require.NotNil(t, f.Config)
	require.NotEmpty(t, f.Config.SPS)
	sps, err := h26x.ParseSPS(h26x.H264, f.Config.SPS[0])
	require.NoError(t, err)
	return sps
}

func ptsSet(frames []media.Frame) []int64 {
	out := make([]int64, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.PTS)
	}
	slices.Sort(out)
	return out
}

func TestResolveRungEncoder(t *testing.T) {
	cases := []struct {
		name    string
		in      Rung
		enc     string
		preset  string
		bframes int
		err     string
	}{
		{name: "empty is x264", in: Rung{}, enc: EncoderX264, preset: "veryfast", bframes: 2},
		{name: "x264 named", in: Rung{Encoder: "x264", Preset: "slow", BFrames: 3}, enc: EncoderX264, preset: "slow", bframes: 3},
		{name: "x264 no b-frames", in: Rung{BFrames: -1}, enc: EncoderX264, preset: "veryfast", bframes: 0},
		{name: "x264 any preset name", in: Rung{Preset: "no-such-preset"}, enc: EncoderX264, preset: "no-such-preset", bframes: 2},
		{name: "x264 b-frames 4", in: Rung{BFrames: 4}, err: "b-frames 4 is outside [-1, 3]"},
		{name: "nvenc default preset", in: Rung{Encoder: "nvenc"}, enc: EncoderNVENC, preset: "p4", bframes: 2},
		{name: "nvenc p1", in: Rung{Encoder: "nvenc", Preset: "p1"}, enc: EncoderNVENC, preset: "p1", bframes: 2},
		{name: "nvenc p7", in: Rung{Encoder: "nvenc", Preset: "p7"}, enc: EncoderNVENC, preset: "p7", bframes: 2},
		{name: "nvenc x264 preset", in: Rung{Encoder: "nvenc", Preset: "veryfast"}, err: `nvenc preset "veryfast" is not p1 to p7`},
		{name: "nvenc p8", in: Rung{Encoder: "nvenc", Preset: "p8"}, err: `nvenc preset "p8" is not p1 to p7`},
		{name: "nvenc p0", in: Rung{Encoder: "nvenc", Preset: "p0"}, err: `nvenc preset "p0" is not p1 to p7`},
		{name: "nvenc no b-frames", in: Rung{Encoder: "nvenc", BFrames: -1}, enc: EncoderNVENC, preset: "p4", bframes: 0},
		{name: "nvenc b-frames 3", in: Rung{Encoder: "nvenc", BFrames: 3}, enc: EncoderNVENC, preset: "p4", bframes: 3},
		{name: "nvenc b-frames 4", in: Rung{Encoder: "nvenc", BFrames: 4}, err: "b-frames 4 is outside [-1, 3]"},
		{name: "qsv", in: Rung{Encoder: "qsv"}, err: `encoder "qsv" is not x264 or nvenc`},
		{name: "upper case", in: Rung{Encoder: "NVENC"}, err: `encoder "NVENC" is not x264 or nvenc`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Height, in.BitrateKbps = 240, 600
			r, err := resolveRung(in)
			if tc.err != "" {
				require.EqualError(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Rung{
				Height: 240, BitrateKbps: 600, MaxBitrateKbps: 600,
				BFrames: tc.bframes, Preset: tc.preset, Encoder: tc.enc,
			}, r)
		})
	}

	// The prefix New puts before the message.
	_, err := New([]Rung{{Height: 240, BitrateKbps: 600}, {Height: 240, BitrateKbps: 600, Encoder: "qsv"}}, nil)
	require.EqualError(t, err, `transcode: new pipeline: rung 1: encoder "qsv" is not x264 or nvenc`)
	_, err = NewWith(Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600, Encoder: "nvenc", Preset: "slow"}}})
	require.EqualError(t, err, `transcode: new pipeline: rung 0: nvenc preset "slow" is not p1 to p7`)
}

func TestNVENCOptions(t *testing.T) {
	want := [][2]string{
		{"preset", "p4"},
		{"tune", "hq"},
		{"rc", "vbr"},
		{"profile", "high"},
		{"forced-idr", "1"},
		{"no-scenecut", "1"},
		{"rc-lookahead", "0"},
		{"strict_gop", "1"},
		{"delay", "0"},
		{"aud", "0"},
		{"b_ref_mode", "disabled"},
	}
	assert.Equal(t, want, nvencOptions(Rung{Preset: "p4"}))
	got := nvencOptions(Rung{Preset: "p7", BFrames: 3})
	assert.Equal(t, [2]string{"preset", "p7"}, got[0])
	assert.Equal(t, want[1:], got[1:])
}

// TestNVENCFallback: an NVENC rung that cannot open (GPU 63 does not exist)
// is encoded by libx264, counted once per rung, with one warning per
// build. Runs with and without a device.
func TestNVENCFallback(t *testing.T) {
	setGPU(t, 63)
	src := source(t, "h264_aac.ts")
	srcKeys := keys(ofKind(src, media.KindVideo))

	t.Run("one rung", func(t *testing.T) {
		w := recordWarnings(t)
		p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600, Encoder: EncoderNVENC}, {Height: 180, BitrateKbps: 300}}, nil)
		outs := run(t, p, src)
		assert.Equal(t, uint64(1), p.Stats().EncoderFallbacks)
		assert.Len(t, w.matching("nvenc open failed, using libx264"), 1)
		for i := range 2 {
			v := rungFrames(outs, i)
			require.NotEmpty(t, v, "rung %d", i)
			assert.Equal(t, srcKeys, keys(v), "rung %d keys", i)
			requireConforms(t, v)
		}
		v := rungFrames(outs, 0)
		k := slices.IndexFunc(v, func(f media.Frame) bool { return f.Key })
		require.GreaterOrEqual(t, k, 0)
		assert.True(t, hasX264SEI(v[k]), "rung 0's first keyframe has no x264 SEI")
		assert.Equal(t, [2]int{320, 240}, [2]int{v[0].Config.Width, v[0].Config.Height})
	})

	t.Run("two rungs", func(t *testing.T) {
		w := recordWarnings(t)
		p := newPipeline(t, []Rung{
			{Height: 240, BitrateKbps: 600, Encoder: EncoderNVENC},
			{Height: 180, BitrateKbps: 300, Encoder: EncoderNVENC, BFrames: -1},
		}, nil)
		outs := run(t, p, src)
		assert.Equal(t, uint64(2), p.Stats().EncoderFallbacks)
		assert.Len(t, w.matching("nvenc open failed, using libx264"), 1)
		for i := range 2 {
			v := rungFrames(outs, i)
			require.NotEmpty(t, v, "rung %d", i)
			assert.Equal(t, srcKeys, keys(v), "rung %d keys", i)
			assert.True(t, hasX264SEI(v[slices.IndexFunc(v, func(f media.Frame) bool { return f.Key })]), "rung %d", i)
		}
		// The fallback keeps the rung's B-frames: none on rung 1.
		for _, f := range rungFrames(outs, 1) {
			require.Equal(t, f.PTS, f.DTS)
		}
	})
}

// TestNVENCMissingEncoder: a libavcodec without h264_nvenc takes the same
// fallback, and the probe says so.
func TestNVENCMissingEncoder(t *testing.T) {
	old := nvencCodec
	nvencCodec = "no_such_nvenc"
	t.Cleanup(func() { nvencCodec = old })

	err := ProbeNVENC()
	require.ErrorContains(t, err, "no_such_nvenc not in libavcodec")
	p := newPipeline(t, []Rung{{Height: 120, BitrateKbps: 150, Encoder: EncoderNVENC}}, nil)
	v := rungFrames(run(t, p, source(t, "h264_aac.ts")), 0)
	require.NotEmpty(t, v)
	assert.Equal(t, uint64(1), p.Stats().EncoderFallbacks)
	assert.True(t, hasX264SEI(v[0]))
}

func TestNVENCRung(t *testing.T) {
	nvencOrSkip(t)
	src := source(t, "h264_aac.ts")
	srcVideo := ofKind(src, media.KindVideo)
	w := recordWarnings(t)
	p := newPipeline(t, []Rung{
		{Height: 240, BitrateKbps: 600, BFrames: -1, Encoder: EncoderNVENC},
		{Height: 120, BitrateKbps: 150, BFrames: -1, Encoder: EncoderNVENC},
	}, nil)
	outs := run(t, p, src)
	assert.Equal(t, uint64(0), p.Stats().EncoderFallbacks)
	assert.Empty(t, w.matching("nvenc open failed"))
	for i, dims := range [][2]int{{320, 240}, {160, 120}} {
		v := rungFrames(outs, i)
		require.NotEmpty(t, v, "rung %d", i)
		assert.Equal(t, dims, [2]int{v[0].Config.Width, v[0].Config.Height}, "rung %d", i)
		assert.Equal(t, keys(srcVideo), keys(v), "rung %d keys", i)
		assert.Equal(t, ptsSet(srcVideo), ptsSet(v), "rung %d PTS", i)
		for j, f := range v {
			require.False(t, hasX264SEI(f), "rung %d frame %d", i, j)
			require.Equal(t, f.PTS, f.DTS, "rung %d frame %d", i, j)
			require.Same(t, v[0].Config, f.Config, "rung %d frame %d", i, j)
		}
		requireConforms(t, v)
		sps := spsOf(t, v[0])
		assert.Equal(t, uint8(100), sps.ProfileIDC, "rung %d: High profile", i)
		r, assumed := h26x.ReorderOf(h26x.H264, sps)
		assert.Equal(t, 0, r, "rung %d reorder", i)
		assert.False(t, assumed, "rung %d reorder assumed", i)
		t.Logf("rung %d: %dx%d, %d frames, keys %v, reorder %d", i, dims[0], dims[1], len(v), keys(v), r)
	}
}

func TestNVENCBFrames(t *testing.T) {
	nvencOrSkip(t)
	src := source(t, "h264_aac.ts")
	srcVideo := ofKind(src, media.KindVideo)
	for _, bf := range []int{0, 3} {
		t.Run(map[int]string{0: "default 2", 3: "3"}[bf], func(t *testing.T) {
			p := newPipeline(t, []Rung{{Height: 240, BitrateKbps: 600, BFrames: bf, Encoder: EncoderNVENC}}, nil)
			v := rungFrames(run(t, p, src), 0)
			assert.Equal(t, uint64(0), p.Stats().EncoderFallbacks)
			require.NotEmpty(t, v)
			reordered := 0
			for j, f := range v {
				require.LessOrEqual(t, f.DTS, f.PTS, "frame %d", j)
				if j > 0 {
					require.Greater(t, f.DTS, v[j-1].DTS, "frame %d", j)
				}
				if f.PTS > f.DTS {
					reordered++
				}
			}
			assert.Positive(t, reordered, "no frame has PTS > DTS")
			assert.Equal(t, ptsSet(srcVideo), ptsSet(v))
			assert.Equal(t, keys(srcVideo), keys(v))
			requireConforms(t, v)
			sps := spsOf(t, v[0])
			r, assumed := h26x.ReorderOf(h26x.H264, sps)
			assert.Equal(t, 1, r, "reorder")
			assert.False(t, assumed, "reorder assumed")
			t.Logf("b-frames %d: %d frames, %d with PTS > DTS, SPS reorder %d", bf, len(v), reordered, r)
		})
	}
}

func TestNVENCMixedLadder(t *testing.T) {
	nvencOrSkip(t)
	src := source(t, "h264_pyramid.ts")
	p := newPipeline(t, []Rung{
		{Height: 240, BitrateKbps: 600, Encoder: EncoderNVENC},
		{Height: 180, BitrateKbps: 300, BFrames: 2},
		{Height: 120, BitrateKbps: 150, Encoder: EncoderNVENC},
	}, &Audio{})
	outs := run(t, p, src)
	assert.Equal(t, uint64(0), p.Stats().EncoderFallbacks)
	srcKeys := keys(ofKind(src, media.KindVideo))
	for i := range 3 {
		v := rungFrames(outs, i)
		require.NotEmpty(t, v, "rung %d", i)
		assert.Equal(t, srcKeys, keys(v), "rung %d keys", i)
		assert.Equal(t, i == 1, hasX264SEI(v[0]), "rung %d x264 SEI", i)
		requireConforms(t, rendition(outs, i))
	}
}

func TestNVENCAcrossConfigChange(t *testing.T) {
	nvencOrSkip(t)
	src := source(t, "h264_aac_then_640.ts")
	p := newPipeline(t, []Rung{{Height: 180, BitrateKbps: 300, Encoder: EncoderNVENC}}, &Audio{})
	outs := run(t, p, src)
	assert.Equal(t, uint64(0), p.Stats().EncoderFallbacks)

	requireConforms(t, rendition(outs, 0))
	v := rungFrames(outs, 0)
	require.NotEmpty(t, v)
	changes := []int{}
	for i := 1; i < len(v); i++ {
		if v[i].Config != v[i-1].Config {
			changes = append(changes, i)
		}
	}
	require.Len(t, changes, 1, "config pointer changes")
	second := changes[0]
	assert.NotEqual(t, v[0].Epoch, v[second].Epoch, "the change is at the second build")
	assert.Equal(t, v[second-1].Epoch, v[0].Epoch)
	assert.True(t, v[second].Key)
	assert.Equal(t, [2]int{240, 180}, [2]int{v[0].Config.Width, v[0].Config.Height})
	assert.Equal(t, [2]int{320, 180}, [2]int{v[second].Config.Width, v[second].Config.Height})
	assert.Equal(t, keys(ofKind(src, media.KindVideo)), keys(v))
}

func TestProbeNVENC(t *testing.T) {
	err := ProbeNVENC()
	t.Logf("probe: %v", err)
	setGPU(t, 63)
	require.Error(t, ProbeNVENC())
}
