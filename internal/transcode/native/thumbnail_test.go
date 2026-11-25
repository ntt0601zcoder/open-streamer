//go:build libav

package native

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"slices"
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// shotsOf returns the shots of outs, in output order, and checks that every
// shot Output has a zero F.
func shotsOf(t *testing.T, outs []Output) []picture.Shot {
	t.Helper()
	var out []picture.Shot
	for _, o := range outs {
		if o.Rung != RungShot {
			require.Nil(t, o.Shot, "a shot outside RungShot")
			continue
		}
		require.NotNil(t, o.Shot)
		require.Equal(t, media.Frame{}, o.F)
		out = append(out, *o.Shot)
	}
	return out
}

// decodeShot decodes a shot's JPEG and checks its size against the shot's.
func decodeShot(t *testing.T, s picture.Shot) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(s.JPEG))
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, s.Width, s.Height), img.Bounds())
	return img
}

// videoKeys returns the video key frames of frames.
func videoKeys(frames []media.Frame) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			out = append(out, f)
		}
	}
	return out
}

// pick returns the frames at the given indices.
func pick(frames []media.Frame, idx ...int) []media.Frame {
	out := make([]media.Frame, len(idx))
	for i, j := range idx {
		out[i] = frames[j]
	}
	return out
}

// framePTS returns the PTS of frames, in order.
func framePTS(frames []media.Frame) []int64 {
	out := make([]int64, len(frames))
	for i, f := range frames {
		out[i] = f.PTS
	}
	return out
}

// shotPTS returns the PTS of shots, in order.
func shotPTS(shots []picture.Shot) []int64 {
	out := make([]int64, len(shots))
	for i, s := range shots {
		out[i] = s.PTS
	}
	return out
}

// corruptKey returns a copy of the key frame f with every byte of its slice
// NAL units after the NAL header zeroed.
func corruptKey(f media.Frame) media.Frame {
	data := make([][]byte, len(f.Data))
	for i, n := range f.Data {
		n = bytes.Clone(n)
		if t := n[0] & 0x1f; t == 1 || t == 5 {
			clear(n[1:])
		}
		data[i] = n
	}
	f.Data = data
	return f
}

// filterFrames returns the frames of src that keep accepts, in order.
func filterFrames(src []media.Frame, keep func(media.Frame) bool) []media.Frame {
	var out []media.Frame
	for _, f := range src {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

var rung120 = []Rung{{Height: 120, BitrateKbps: 150}}

// rungNoise bounds the mean difference, per channel 0..255, between two
// encodes of the same picture.
const rungNoise = 4.0

// requireSameRung checks that rung 0 of outs is rung 0 of plain: the same
// timing and, picture by picture, the same image within the noise of the
// encoder. libx264's threads make two runs of the same input differ in a
// few bits, so the frames cannot be compared byte for byte.
func requireSameRung(t *testing.T, plain, outs []Output) {
	t.Helper()
	requireSameTiming(t, rungFrames(plain, 0), rungFrames(outs, 0), "rung 0")
	a, b := decodeRung(t, plain, 0), decodeRung(t, outs, 0)
	require.Len(t, b, len(a))
	worst := 0.0
	for i := range a {
		d := band(a[i], b[i], a[i].Bounds())
		worst = max(worst, d)
		require.Less(t, d, rungNoise, "picture %d", i)
	}
	t.Logf("rung 0: worst mean difference %.2f", worst)
}

func TestThumbnailWithRungs(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := run(t, newWMPipeline(t, Config{Rungs: rung120}), src)
	p := newWMPipeline(t, Config{Rungs: rung120, Thumbnail: &picture.Thumbnail{IntervalSec: 2, Height: 120, Quality: 80}})
	outs := run(t, p, src)

	shots := shotsOf(t, outs)
	pts := ptsSet(ofKind(src, media.KindVideo))
	first := pts[0]
	at := func(from int64) int64 {
		i, _ := slices.BinarySearch(pts, from)
		return pts[i]
	}
	assert.Equal(t, []int64{first, at(first + 180000), at(first + 360000)}, shotPTS(shots))
	for _, s := range shots {
		assert.Equal(t, uint32(1), s.Epoch)
		decodeShot(t, s)
		assert.Equal(t, [2]int{160, 120}, [2]int{s.Width, s.Height})
	}
	require.Len(t, shots, 3)
	assert.NotEqual(t, shots[0].JPEG, shots[2].JPEG, "the first and last shots differ")
	assert.Equal(t, uint64(3), p.Stats().Thumbnails)
	assert.Zero(t, p.Stats().ThumbnailsFailed)
	requireSameRung(t, plain, outs)
}

func TestThumbnailBeforeWatermark(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 25},
		Thumbnail: &picture.Thumbnail{IntervalSec: 2, Height: 240},
	})
	outs := run(t, p, src)
	shots := shotsOf(t, outs)
	require.Len(t, shots, 3)
	for i, s := range shots {
		img := decodeShot(t, s)
		got := region(img, picture.TopLeft, 25, squareHPct(img, 25))
		assert.False(t, near(got, magenta, 80), "shot %d: mark rectangle %v", i, got)
	}
	imgs := decodeRung(t, outs, 0)
	got := region(imgs[0], picture.TopLeft, 25, squareHPct(imgs[0], 25))
	assert.True(t, near(got, magenta, 40), "the rung carries the mark: %v", got)
}

func TestThumbnailNewEpoch(t *testing.T) {
	src := source(t, "h264_aac_then_640.ts")
	p := newWMPipeline(t, Config{Rungs: rung120, Thumbnail: &picture.Thumbnail{IntervalSec: 3600, Height: 120}})
	shots := shotsOf(t, run(t, p, src))
	require.Len(t, shots, 2)
	assert.Equal(t, uint32(1), shots[0].Epoch)
	assert.Equal(t, uint32(2), shots[1].Epoch)
	assert.Equal(t, [2]int{160, 120}, [2]int{shots[0].Width, shots[0].Height})
	assert.Equal(t, [2]int{214, 120}, [2]int{shots[1].Width, shots[1].Height})
	for _, s := range shots {
		decodeShot(t, s)
	}
}

func TestThumbnailNeverUpscales(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newWMPipeline(t, Config{Rungs: rung120, Thumbnail: &picture.Thumbnail{IntervalSec: 3600, Height: 720}})
	shots := shotsOf(t, run(t, p, src))
	require.Len(t, shots, 1)
	assert.Equal(t, [2]int{320, 240}, [2]int{shots[0].Width, shots[0].Height})
	decodeShot(t, shots[0])
}

func TestThumbnailConfig(t *testing.T) {
	_, err := NewWith(Config{Thumbnail: &picture.Thumbnail{Height: 3}})
	require.Error(t, err, "an invalid thumbnail")
	_, err = NewWith(Config{Rungs: rung120, Thumbnail: &picture.Thumbnail{IntervalSec: -1}})
	require.Error(t, err, "an invalid thumbnail with rungs")
	p, err := NewWith(Config{Thumbnail: &picture.Thumbnail{}})
	require.NoError(t, err, "a thumbnail alone")
	p.Close()
	_, err = NewWith(Config{})
	require.Error(t, err, "nothing to do")
}

// keyOnly is a pipeline with no rungs, no audio and an hourly thumbnail
// 120 high: a key-only child ignores the interval.
func keyOnly(t *testing.T) *Pipeline {
	t.Helper()
	return newWMPipeline(t, Config{Thumbnail: &picture.Thumbnail{IntervalSec: 3600, Height: 120}})
}

func TestThumbnailKeyOnly(t *testing.T) {
	ks := pick(videoKeys(source(t, "h264_aac.ts")), 0, 2, 4)
	p := keyOnly(t)
	outs := run(t, p, ks)
	require.Len(t, outs, 3, "nothing but shots")
	shots := shotsOf(t, outs)
	assert.Equal(t, framePTS(ks), shotPTS(shots))
	for _, s := range shots {
		decodeShot(t, s)
		assert.Equal(t, [2]int{160, 120}, [2]int{s.Width, s.Height})
		assert.Equal(t, uint32(1), s.Epoch)
	}
	assert.Equal(t, uint64(3), p.Stats().Thumbnails)
	assert.Equal(t, Stats{Thumbnails: 3}, p.Stats())
}

func TestThumbnailKeyOnlyIgnoresRest(t *testing.T) {
	src := source(t, "h264_aac.ts")
	ks := videoKeys(src)
	require.Len(t, ks, 6)
	p := keyOnly(t)
	outs := run(t, p, src)
	require.Len(t, outs, 6, "nothing but shots")
	assert.Equal(t, framePTS(ks), shotPTS(shotsOf(t, outs)))
	assert.Equal(t, Stats{Thumbnails: 6}, p.Stats())
}

// requireShotPerKey feeds the video keys of a fixture to a key-only
// pipeline and expects one decodable shot per key.
func requireShotPerKey(t *testing.T, fixture string) {
	t.Helper()
	ks := videoKeys(source(t, fixture))
	require.Greater(t, len(ks), 1)
	p := keyOnly(t)
	shots := shotsOf(t, run(t, p, ks))
	assert.Equal(t, framePTS(ks), shotPTS(shots))
	for _, s := range shots {
		decodeShot(t, s)
		assert.Equal(t, [2]int{160, 120}, [2]int{s.Width, s.Height})
	}
	assert.Equal(t, Stats{Thumbnails: uint64(len(ks))}, p.Stats())
}

// The keys after the first are non-IDR I pictures with a recovery point.
func TestThumbnailKeyOnlyOpenGOP(t *testing.T) { requireShotPerKey(t, "h264_opengop.ts") }

// The keys after the first are CRA pictures.
func TestThumbnailKeyOnlyHEVC(t *testing.T) { requireShotPerKey(t, "h265_aac51.ts") }

func TestThumbnailKeyOnlyEpochs(t *testing.T) {
	ks := videoKeys(source(t, "h264_aac_then_640.ts"))
	p := keyOnly(t)
	shots := shotsOf(t, run(t, p, ks))
	require.Len(t, shots, len(ks))
	second := 0
	for i, s := range shots {
		assert.Equal(t, ks[i].PTS, s.PTS)
		assert.Equal(t, ks[i].Epoch, s.Epoch, "shot %d", i)
		decodeShot(t, s)
		switch s.Epoch {
		case 1:
			assert.Equal(t, [2]int{160, 120}, [2]int{s.Width, s.Height}, "shot %d", i)
		case 2:
			second++
			assert.Equal(t, [2]int{214, 120}, [2]int{s.Width, s.Height}, "shot %d", i)
		default:
			t.Errorf("shot %d: epoch %d", i, s.Epoch)
		}
	}
	assert.Positive(t, second, "no shot in epoch 2")
}

// audioFrames returns the audio outputs of outs.
func audioFrames(outs []Output) []media.Frame { return rungFrames(outs, -1) }

func TestThumbnailKeyOnlyWithAudio(t *testing.T) {
	isAudio := func(f media.Frame) bool { return f.Kind() == media.KindAudio }
	isKey := func(f media.Frame, ks []media.Frame) bool {
		return f.Kind() == media.KindVideo && f.Key && slices.Contains(framePTS(ks), f.PTS)
	}

	t.Run("keys", func(t *testing.T) {
		src := source(t, "h264_aac.ts")
		ks := pick(videoKeys(src), 0, 2, 4)
		mixed := filterFrames(src, func(f media.Frame) bool { return isAudio(f) || isKey(f, ks) })
		require.Len(t, mixed, len(ofKind(src, media.KindAudio))+3)
		want := audioFrames(run(t, newWMPipeline(t, Config{Audio: &Audio{}}), ofKind(src, media.KindAudio)))
		p := newWMPipeline(t, Config{Audio: &Audio{}, Thumbnail: &picture.Thumbnail{IntervalSec: 3600}})
		outs := run(t, p, mixed)
		assert.Equal(t, want, audioFrames(outs))
		assert.Equal(t, framePTS(ks), shotPTS(shotsOf(t, outs)))
		assert.Len(t, outs, len(want)+3)
	})

	t.Run("discontinuity", func(t *testing.T) {
		src := source(t, "h264_aac.ts")
		ks := pick(videoKeys(src), 0, 2, 4)
		mixed := filterFrames(src, func(f media.Frame) bool { return isAudio(f) || isKey(f, ks) })
		cut := src[0].PTS + 3*media.ClockRate
		feed := func(p *Pipeline, frames []media.Frame) []Output {
			var out []Output
			done := false
			for _, f := range frames {
				if !done && f.PTS >= cut {
					o, err := p.Discontinuity()
					require.NoError(t, err)
					out = append(out, o...)
					done = true
				}
				o, err := p.Push(f)
				require.NoError(t, err)
				out = append(out, o...)
			}
			o, err := p.Flush()
			require.NoError(t, err)
			return append(out, o...)
		}
		want := audioFrames(feed(newWMPipeline(t, Config{Audio: &Audio{}}), ofKind(src, media.KindAudio)))
		outs := feed(newWMPipeline(t, Config{Audio: &Audio{}, Thumbnail: &picture.Thumbnail{}}), mixed)
		assert.Equal(t, want, audioFrames(outs))
		assert.Len(t, shotsOf(t, outs), 3)
	})

	t.Run("video key opens epoch 2", func(t *testing.T) {
		src := source(t, "h264_aac_then_640.ts")
		mixed := filterFrames(src, func(f media.Frame) bool { return isAudio(f) || f.Kind() == media.KindVideo && f.Key })
		// The first frame of epoch 2 is a video key.
		i := slices.IndexFunc(mixed, func(f media.Frame) bool { return f.Epoch == 2 })
		require.Positive(t, i)
		require.Equal(t, media.KindVideo, mixed[i].Kind())
		require.True(t, mixed[i].Key)
		want := audioFrames(run(t, newWMPipeline(t, Config{Audio: &Audio{}}), ofKind(src, media.KindAudio)))
		outs := run(t, newWMPipeline(t, Config{Audio: &Audio{}, Thumbnail: &picture.Thumbnail{}}), mixed)
		require.NotEmpty(t, want)
		assert.Equal(t, want, audioFrames(outs), "frames, PTS, epochs and order")
		assert.Len(t, shotsOf(t, outs), len(videoKeys(src)))
	})
}

func TestThumbnailKeyOnlyCorruptKey(t *testing.T) {
	ks := videoKeys(source(t, "h264_aac.ts"))
	p := keyOnly(t)
	outs := run(t, p, []media.Frame{corruptKey(ks[1]), ks[2]})
	assert.Equal(t, []int64{ks[2].PTS}, shotPTS(shotsOf(t, outs)))
	assert.Equal(t, Stats{Thumbnails: 1, ThumbnailsFailed: 1}, p.Stats())
}

func TestThumbnailKeyOnlyNeverFails(t *testing.T) {
	w := recordWarnings(t)
	src := source(t, "h264_aac.ts")
	ks := videoKeys(src)
	audio := ofKind(src, media.KindAudio)
	const n = 60
	every := len(audio) / n
	var mixed []media.Frame
	bad := 0
	for i, a := range audio {
		mixed = append(mixed, a)
		if i%every == 0 && bad < n {
			k := corruptKey(ks[bad%len(ks)])
			k.PTS, k.DTS = a.PTS, a.DTS
			mixed = append(mixed, k)
			bad++
		}
	}
	require.Equal(t, n, bad)
	want := audioFrames(run(t, newWMPipeline(t, Config{Audio: &Audio{}}), audio))
	p := newWMPipeline(t, Config{Audio: &Audio{}, Thumbnail: &picture.Thumbnail{}})
	outs := run(t, p, mixed)
	assert.Equal(t, want, audioFrames(outs))
	assert.Empty(t, shotsOf(t, outs))
	st := p.Stats()
	assert.Equal(t, uint64(n), st.ThumbnailsFailed)
	assert.Zero(t, st.Thumbnails)
	assert.Zero(t, st.VideoCorrupt)
	assert.Len(t, w.matching("thumbnail"), 1, "one warning per pipeline")
}

// failJPEG replaces the JPEG encoder with one that always fails.
func failJPEG(t *testing.T) {
	t.Helper()
	old := encodeJPEG
	encodeJPEG = func(image.Image, int) ([]byte, error) { return nil, errors.New("no jpeg today") }
	t.Cleanup(func() { encodeJPEG = old })
}

func TestThumbnailFailureWithRungs(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := run(t, newWMPipeline(t, Config{Rungs: rung120}), src)
	failJPEG(t)
	w := recordWarnings(t)
	p := newWMPipeline(t, Config{Rungs: rung120, Thumbnail: &picture.Thumbnail{IntervalSec: 2}})
	outs := run(t, p, src)
	assert.Empty(t, shotsOf(t, outs))
	st := p.Stats()
	assert.Equal(t, uint64(3), st.ThumbnailsFailed, "one attempt per interval")
	assert.Zero(t, st.Thumbnails)
	assert.Zero(t, st.VideoCorrupt)
	requireSameRung(t, plain, outs)
	assert.Len(t, w.matching("thumbnail"), 1, "one warning per pipeline")
}

// rangeLevels are the luma levels of rangeFrame's four 64-pixel columns.
var rangeLevels = [4]uint8{0, 16, 235, 255}

// rangeFrame returns a 256×64 picture of pixel format pf and colour range
// cr, with neutral chroma and one column per rangeLevels entry.
func rangeFrame(t *testing.T, pf astiav.PixelFormat, cr astiav.ColorRange) *astiav.Frame {
	t.Helper()
	const w, h = 256, 64
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := range h {
		for x := range w {
			img.Y[y*img.YStride+x] = rangeLevels[x/64]
		}
	}
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 128, 128
	}
	fr := astiav.AllocFrame()
	t.Cleanup(fr.Free)
	fr.SetWidth(w)
	fr.SetHeight(h)
	fr.SetPixelFormat(pf)
	fr.SetColorRange(cr)
	fr.SetSampleAspectRatio(astiav.NewRational(1, 1))
	require.NoError(t, fr.AllocBuffer(0))
	require.NoError(t, fr.Data().FromImage(img))
	return fr
}

// shotLevels shoots fr and returns the mean luma of the middle of each
// column, whatever the shot's size.
func shotLevels(t *testing.T, th *thumbnailer, fr *astiav.Frame) [4]float64 {
	t.Helper()
	s, err := th.make(fr)
	require.NoError(t, err)
	require.Equal(t, th.t.Height, s.Height)
	require.Equal(t, 4*s.Height, s.Width)
	img, err := jpeg.Decode(bytes.NewReader(s.JPEG))
	require.NoError(t, err)
	yc, ok := img.(*image.YCbCr)
	require.True(t, ok, "%T", img)
	var out [4]float64
	cw := s.Width / 4
	for c := range out {
		n := 0
		for y := s.Height / 4; y < s.Height*3/4; y++ {
			for x := c*cw + cw/4; x < c*cw+cw*3/4; x++ {
				out[c] += float64(yc.Y[yc.YOffset(x, y)])
				n++
			}
		}
		out[c] /= float64(n)
	}
	return out
}

// TestThumbnailColourRange: a full-range source keeps its levels in the
// full-range JPEG, black at 0 and white at 255 and nothing stretched; a
// limited-range source is stretched, 16 to 0 and 235 to 255. Both at the
// source's size (swscale's unscaled path) and scaled down.
func TestThumbnailColourRange(t *testing.T) {
	full := [4]float64{0, 16, 235, 255}
	limited := [4]float64{0, 0, 255, 255}
	for _, tc := range []struct {
		name string
		pf   astiav.PixelFormat
		cr   astiav.ColorRange
		want [4]float64
	}{
		{"yuv420p jpeg range", astiav.PixelFormatYuv420P, astiav.ColorRangeJpeg, full},
		{"yuvj420p", astiav.PixelFormatYuvj420P, astiav.ColorRangeUnspecified, full},
		{"yuvj420p jpeg range", astiav.PixelFormatYuvj420P, astiav.ColorRangeJpeg, full},
		{"yuv420p mpeg range", astiav.PixelFormatYuv420P, astiav.ColorRangeMpeg, limited},
		{"yuv420p unspecified", astiav.PixelFormatYuv420P, astiav.ColorRangeUnspecified, limited},
	} {
		for _, height := range []int{64, 32} {
			t.Run(fmt.Sprintf("%s %d", tc.name, height), func(t *testing.T) {
				var st Stats
				th := newThumbnailer(picture.Thumbnail{IntervalSec: 10, Height: height, Quality: 95}, &st, false)
				t.Cleanup(th.release)
				got := shotLevels(t, th, rangeFrame(t, tc.pf, tc.cr))
				for c := range got {
					assert.InDelta(t, tc.want[c], got[c], 3, "column %d (level %d): %v", c, rangeLevels[c], got)
				}
			})
		}
	}

	// The scaler follows a range change on the same size and format.
	var st Stats
	th := newThumbnailer(picture.Thumbnail{IntervalSec: 10, Height: 64, Quality: 95}, &st, false)
	t.Cleanup(th.release)
	for _, cr := range []astiav.ColorRange{astiav.ColorRangeMpeg, astiav.ColorRangeJpeg, astiav.ColorRangeMpeg} {
		want := limited
		if cr == astiav.ColorRangeJpeg {
			want = full
		}
		got := shotLevels(t, th, rangeFrame(t, astiav.PixelFormatYuv420P, cr))
		for c := range got {
			assert.InDelta(t, want[c], got[c], 3, "range %v column %d: %v", cr, c, got)
		}
	}
}
