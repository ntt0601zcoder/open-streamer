//go:build libav

package native

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// rgb is a mean colour, per channel 0..255.
type rgb [3]float64

var magenta = rgb{255, 0, 255}

// solidPNG returns a w × h PNG of one straight-alpha colour per half: left
// for the left half, right for the right one.
func solidPNG(t *testing.T, w, h int, left, right color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := left
			if x >= w/2 {
				c = right
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

// magenta64 is a 64×64 opaque PNG of RGB 255,0,255.
func magenta64(t *testing.T) []byte {
	t.Helper()
	m := color.NRGBA{255, 0, 255, 255}
	return solidPNG(t, 64, 64, m, m)
}

// newWMPipeline builds a pipeline for c and closes it at the end of the test.
func newWMPipeline(t *testing.T, c Config) *Pipeline {
	t.Helper()
	p, err := NewWith(c)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

// toRGBA converts a decoded picture to straight RGBA.
func toRGBA(t *testing.T, fr *astiav.Frame) *image.NRGBA {
	t.Helper()
	w, h := fr.Width(), fr.Height()
	sws, err := astiav.CreateSoftwareScaleContext(w, h, fr.PixelFormat(), w, h, astiav.PixelFormatRgba,
		astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBicubic))
	require.NoError(t, err)
	defer sws.Free()
	dst := astiav.AllocFrame()
	defer dst.Free()
	dst.SetWidth(w)
	dst.SetHeight(h)
	dst.SetPixelFormat(astiav.PixelFormatRgba)
	require.NoError(t, dst.AllocBuffer(0))
	require.NoError(t, sws.ScaleFrame(fr, dst))
	img := &image.NRGBA{}
	require.NoError(t, dst.Data().ToImage(img))
	// ToImage aliases a buffer it allocated; copy to be safe.
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	copy(out.Pix, img.Pix)
	return out
}

// decodeRung decodes a rung's output frames with libavcodec and returns its
// pictures in presentation order.
func decodeRung(t *testing.T, outs []Output, rung int) []image.Image {
	t.Helper()
	codec := astiav.FindDecoderByName("h264")
	require.NotNil(t, codec)
	cc := astiav.AllocCodecContext(codec)
	require.NotNil(t, cc)
	defer cc.Free()
	cc.SetTimeBase(timeBase)
	require.NoError(t, cc.Open(codec, nil))
	pkt := newInPacket()
	defer pkt.free()
	fr := astiav.AllocFrame()
	defer fr.Free()
	var imgs []image.Image
	receive := func() {
		for {
			err := cc.ReceiveFrame(fr)
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				return
			}
			require.NoError(t, err)
			imgs = append(imgs, toRGBA(t, fr))
			fr.Unref()
		}
	}
	for _, f := range rungFrames(outs, rung) {
		require.NoError(t, pkt.fill(annexB(f), f.PTS, f.DTS))
		require.NoError(t, pkt.send(cc))
		receive()
	}
	require.NoError(t, cc.SendPacket(nil))
	receive()
	return imgs
}

// markRect is the rectangle a mark wPct % of the picture's width and hPct %
// of its height occupies at pos, with the 2 % margin.
func markRect(img image.Image, pos picture.Position, wPct, hPct float64) image.Rectangle {
	b := img.Bounds()
	bw, bh := b.Dx(), b.Dy()
	w := int(math.Round(float64(bw) * wPct / 100))
	h := int(math.Round(float64(bh) * hPct / 100))
	m := picture.Margin(bh)
	var x, y int
	switch pos {
	case picture.TopLeft:
		x, y = m, m
	case picture.TopRight:
		x, y = bw-w-m, m
	case picture.BottomLeft:
		x, y = m, bh-h-m
	case picture.BottomRight:
		x, y = bw-w-m, bh-h-m
	case picture.Center:
		x, y = (bw-w)/2, (bh-h)/2
	}
	return image.Rect(x, y, x+w, y+h)
}

// inner is the middle half of r in each dimension.
func inner(r image.Rectangle) image.Rectangle {
	dx, dy := r.Dx()/4, r.Dy()/4
	return image.Rect(r.Min.X+dx, r.Min.Y+dy, r.Max.X-dx, r.Max.Y-dy)
}

// meanRGB is the mean colour of r in img.
func meanRGB(img image.Image, r image.Rectangle) rgb {
	var s rgb
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			s[0] += float64(c.R)
			s[1] += float64(c.G)
			s[2] += float64(c.B)
			n++
		}
	}
	for i := range s {
		s[i] /= float64(n)
	}
	return s
}

// region is the mean RGB of the inner half of the rectangle a mark of that
// size would occupy at pos.
func region(img image.Image, pos picture.Position, wPct, hPct float64) rgb {
	return meanRGB(img, inner(markRect(img, pos, wPct, hPct)))
}

// near reports whether every channel of a is within tol of b.
func near(a, b rgb, tol float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > tol {
			return false
		}
	}
	return true
}

// squareHPct is the height in % of the picture of a square mark wPct % of
// its width.
func squareHPct(img image.Image, wPct float64) float64 {
	b := img.Bounds()
	return wPct * float64(b.Dx()) / float64(b.Dy())
}

// band is the mean absolute difference of two pictures over r, all
// channels.
func band(a, b image.Image, r image.Rectangle) float64 {
	var s float64
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			ca := color.NRGBAModel.Convert(a.At(x, y)).(color.NRGBA)
			cb := color.NRGBAModel.Convert(b.At(x, y)).(color.NRGBA)
			s += math.Abs(float64(ca.R)-float64(cb.R)) + math.Abs(float64(ca.G)-float64(cb.G)) +
				math.Abs(float64(ca.B)-float64(cb.B))
			n += 3
		}
	}
	return s / float64(n)
}

// textBand is the rectangle a short text watermark at the bottom left of a
// 320×240 picture covers, with the default size of 5 % (12 px).
var textBand = image.Rect(5, 240-5-16, 90, 240-5)

// topLeftBand is the same at the top left.
var topLeftBand = image.Rect(5, 5, 90, 5+16)

var ladder3 = []Rung{{Height: 240, BitrateKbps: 600}, {Height: 180, BitrateKbps: 300}, {Height: 120, BitrateKbps: 150}}

func TestWatermarkImageOnEveryRung(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newWMPipeline(t, Config{Rungs: ladder3, Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 25}})
	outs := run(t, p, src)
	n := len(ofKind(src, media.KindVideo))
	for rung := range ladder3 {
		imgs := decodeRung(t, outs, rung)
		require.Len(t, imgs, n, "rung %d", rung)
		for _, i := range []int{0, len(imgs) - 1} {
			img := imgs[i]
			hp := squareHPct(img, 25)
			got := region(img, picture.TopLeft, 25, hp)
			assert.True(t, near(got, magenta, 40), "rung %d picture %d: mark %v", rung, i, got)
			other := region(img, picture.BottomRight, 25, hp)
			assert.False(t, near(other, magenta, 80), "rung %d picture %d: bottom right %v", rung, i, other)
		}
	}
}

func TestWatermarkPositions(t *testing.T) {
	src := source(t, "h264_aac.ts")
	for _, pos := range []picture.Position{picture.TopLeft, picture.TopRight, picture.BottomLeft, picture.BottomRight, picture.Center} {
		p := newWMPipeline(t, Config{
			Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
			Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 20, Position: pos},
		})
		imgs := decodeRung(t, run(t, p, src), 0)
		require.NotEmpty(t, imgs)
		for _, i := range []int{0, len(imgs) - 1} {
			img := imgs[i]
			got := region(img, pos, 20, squareHPct(img, 20))
			assert.True(t, near(got, magenta, 40), "position %d picture %d: %v", pos, i, got)
		}
	}
}

// unmarked runs src through one 240 rung with no watermark.
func unmarked(t *testing.T, src []media.Frame) []image.Image {
	t.Helper()
	p := newWMPipeline(t, Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}})
	return decodeRung(t, run(t, p, src), 0)
}

func TestWatermarkOpacity(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := unmarked(t, src)
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 25, OpacityPct: 50},
	})
	imgs := decodeRung(t, run(t, p, src), 0)
	require.Len(t, imgs, len(plain))
	for _, i := range []int{0, len(imgs) / 2, len(imgs) - 1} {
		hp := squareHPct(imgs[i], 25)
		got := region(imgs[i], picture.TopLeft, 25, hp)
		base := region(plain[i], picture.TopLeft, 25, hp)
		strict := 0
		for c := range got {
			lo, hi := min(base[c], magenta[c]), max(base[c], magenta[c])
			if hi-lo < 60 {
				continue
			}
			strict++
			assert.Greater(t, got[c], lo+15, "picture %d channel %d: %v between %v and magenta", i, c, got, base)
			assert.Less(t, got[c], hi-15, "picture %d channel %d: %v between %v and magenta", i, c, got, base)
		}
		require.Positive(t, strict, "picture %d: the unmarked region %v is too close to magenta", i, base)
	}
}

func TestWatermarkImageAlpha(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := unmarked(t, src)
	img := solidPNG(t, 64, 64, color.NRGBA{255, 0, 255, 255}, color.NRGBA{255, 0, 255, 128})
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: img, SizePct: 25},
	})
	imgs := decodeRung(t, run(t, p, src), 0)
	require.Len(t, imgs, len(plain))
	for _, i := range []int{0, len(imgs) - 1} {
		r := markRect(imgs[i], picture.TopLeft, 25, squareHPct(imgs[i], 25))
		mid := (r.Min.X + r.Max.X) / 2
		left := inner(image.Rect(r.Min.X, r.Min.Y, mid, r.Max.Y))
		right := inner(image.Rect(mid, r.Min.Y, r.Max.X, r.Max.Y))
		gl := meanRGB(imgs[i], left)
		assert.True(t, near(gl, magenta, 40), "picture %d: left half %v", i, gl)
		base := meanRGB(plain[i], right)
		want := rgb{(base[0] + magenta[0]) / 2, (base[1] + magenta[1]) / 2, (base[2] + magenta[2]) / 2}
		gr := meanRGB(imgs[i], right)
		assert.True(t, near(gr, want, 40), "picture %d: right half %v, want about %v (unmarked %v)", i, gr, want, base)
	}
}

// requireSameTiming checks that b has a's timing: as many frames, the same
// DTS in order, the same PTS and the same keys. Which picture an encoder
// codes as a B-frame depends on the picture (libx264's adaptive B-frame
// placement), so the PTS are compared as a set: a watermark changes the
// picture, never a timestamp.
func requireSameTiming(t *testing.T, a, b []media.Frame, msg string) {
	t.Helper()
	require.Len(t, b, len(a), "%s: frames", msg)
	dts := func(fs []media.Frame) []int64 {
		out := make([]int64, len(fs))
		for i, f := range fs {
			out[i] = f.DTS
		}
		return out
	}
	assert.Equal(t, dts(a), dts(b), "%s: DTS", msg)
	assert.Equal(t, ptsSet(a), ptsSet(b), "%s: PTS", msg)
	assert.Equal(t, keys(a), keys(b), "%s: keys", msg)
}

func TestWatermarkKeepsTiming(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := run(t, newWMPipeline(t, Config{Rungs: ladder3, Audio: &Audio{}}), src)
	marked := run(t, newWMPipeline(t, Config{
		Rungs: ladder3, Audio: &Audio{},
		Watermark: &picture.Watermark{Text: "Open Streamer %H:%M:%S"},
	}), src)
	srcKeys := keys(ofKind(src, media.KindVideo))
	for rung := -1; rung < len(ladder3); rung++ {
		a, b := rungFrames(plain, rung), rungFrames(marked, rung)
		require.NotEmpty(t, a)
		if rung < 0 {
			assert.Equal(t, a, b, "audio")
			continue
		}
		requireSameTiming(t, a, b, fmt.Sprintf("rung %d", rung))
		assert.Equal(t, srcKeys, keys(b), "rung %d keys", rung)
	}
}

// fakeClock returns a clock that starts at 12:00:00 UTC and advances 1 s
// every perSecond calls.
func fakeClock(perSecond int) func() time.Time {
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n/perSecond) * time.Second)
		n++
		return t
	}
}

// The fixture's top left holds testsrc2's own frame counter, which changes
// on every picture, so the text is placed at the bottom left, over two
// still colour bars.
func TestWatermarkTextTicks(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := unmarked(t, src)
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Text: "%H:%M:%S", Position: picture.BottomLeft},
		Now:       fakeClock(25),
	})
	imgs := decodeRung(t, run(t, p, src), 0)
	require.Len(t, imgs, len(plain))
	assert.Less(t, band(imgs[0], imgs[24], textBand), 2.0, "pictures 0 and 24 show the same second")
	assert.Greater(t, band(imgs[24], imgs[25], textBand), 2.0, "picture 25 shows the next second")
	for i := range imgs {
		require.Greater(t, band(imgs[i], plain[i], textBand), 2.0, "picture %d has no text", i)
	}
}

func TestWatermarkTextIsLiteral(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := unmarked(t, src)
	cjk := strings.Repeat("字", 85) + "x"
	require.Len(t, cjk, 256)
	evil := ":'\\%%{localtime},[in]textfile=/etc/passwd"
	for _, text := range []string{evil, cjk} {
		p := newWMPipeline(t, Config{
			Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
			Watermark: &picture.Watermark{Text: text},
		})
		outs := push(t, p, src)
		desc := p.video.wm.desc
		dir := p.video.wm.dir
		o, err := p.Flush()
		require.NoError(t, err)
		outs = append(outs, o...)
		imgs := decodeRung(t, outs, 0)
		require.Len(t, imgs, len(plain))
		for _, i := range []int{0, len(imgs) - 1} {
			assert.Greater(t, band(imgs[i], plain[i], topLeftBand), 2.0, "picture %d has no text", i)
		}

		t.Logf("graph: %s", desc)
		require.NotEmpty(t, desc)
		assert.NotContains(t, desc, text)
		for _, s := range []string{"%{localtime}", "/etc/passwd", "[in]", "localtime", "字"} {
			assert.NotContains(t, desc, s)
		}
		// The only paths are the child's own: once they are taken out, no
		// '/' is left.
		for _, kv := range []string{"fontfile=" + dir + "/font.ttf:", "textfile=" + dir + "/text:"} {
			require.Equal(t, 1, strings.Count(desc, kv), "%q in %q", kv, desc)
			desc = strings.Replace(desc, kv, "", 1)
		}
		assert.NotContains(t, desc, "/", "a path is left in %q", desc)
		got, err := os.ReadFile(filepath.Join(dir, "text"))
		require.NoError(t, err)
		assert.Equal(t, picture.FormatText(text, time.Now()), string(got), "the text file holds the text verbatim")
	}
}

func TestWatermarkTextGraph(t *testing.T) {
	src := source(t, "h264_aac.ts")[:40]
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Text: "%H", OpacityPct: 50, SizePct: 10, Position: picture.BottomRight},
	})
	push(t, p, src)
	dir := p.video.wm.dir
	want := "drawtext=fontfile=" + dir + "/font.ttf:textfile=" + dir + "/text:expansion=none:reload=1" +
		":fontsize=24:fontcolor=white@0.50:borderw=1:bordercolor=black@0.50:x=w-tw-5:y=h-th-5,format=yuv420p"
	assert.Equal(t, want, p.video.wm.desc)

	q := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Text: "100%% on", SizePct: 50, Position: picture.Center},
	})
	push(t, q, src)
	dir = q.video.wm.dir
	want = "drawtext=fontfile=" + dir + "/font.ttf:textfile=" + dir + "/text:expansion=none" +
		":fontsize=120:fontcolor=white@1.00:borderw=7:bordercolor=black@1.00:x=(w-tw)/2:y=(h-th)/2,format=yuv420p"
	assert.Equal(t, want, q.video.wm.desc)
}

func TestWatermarkImageGraph(t *testing.T) {
	src := source(t, "h264_aac.ts")[:40]
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: magenta64(t), OpacityPct: 30, Position: picture.TopRight},
	})
	push(t, p, src)
	want := "[wm]scale=48:-1,format=yuva420p,colorchannelmixer=aa=0.30[o];[in][o]overlay=W-w-5:5:format=auto,format=yuv420p"
	assert.Equal(t, want, p.video.wm.desc)
}

func TestWatermarkAcrossConfigChange(t *testing.T) {
	src := source(t, "h264_aac_then_640.ts")
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 25, Position: picture.TopRight},
	})
	imgs := decodeRung(t, run(t, p, src), 0)
	plain := decodeRung(t, run(t, newWMPipeline(t, Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}}), src), 0)
	require.Len(t, plain, len(imgs))
	change := -1
	for i := 1; i < len(imgs); i++ {
		if imgs[i].Bounds() != imgs[i-1].Bounds() {
			change = i
			break
		}
	}
	require.Positive(t, change, "no size change")
	t.Logf("change at picture %d: %v to %v", change, imgs[change-1].Bounds(), imgs[change].Bounds())
	for _, i := range []int{change - 1, change} {
		img := imgs[i]
		got := region(img, picture.TopRight, 25, squareHPct(img, 25))
		assert.True(t, near(got, magenta, 40), "picture %d (%v): %v", i, img.Bounds(), got)
		// The mark is 25 % of the width: just outside it to the left the
		// picture is the unmarked one.
		r := markRect(img, picture.TopRight, 25, squareHPct(img, 25))
		outside := image.Rect(r.Min.X-r.Dx()/4, r.Min.Y+r.Dy()/4, r.Min.X-2, r.Max.Y-r.Dy()/4)
		assert.Less(t, band(img, plain[i], outside), 8.0, "picture %d: the mark is wider than 25 %%", i)
	}
}

// pushVideoCount feeds frames until n video frames have gone in, and returns
// the outputs and the frames left.
func pushVideoCount(t *testing.T, p *Pipeline, frames []media.Frame, n int) ([]Output, []media.Frame) {
	t.Helper()
	var out []Output
	seen := 0
	for i, f := range frames {
		if f.Kind() == media.KindVideo {
			if seen == n {
				return out, frames[i:]
			}
			seen++
		}
		o, err := p.Push(f)
		require.NoError(t, err)
		out = append(out, o...)
	}
	return out, nil
}

func TestSetWatermark(t *testing.T) {
	src := source(t, "h264_aac.ts")
	rungs := []Rung{{Height: 240, BitrateKbps: 600}, {Height: 120, BitrateKbps: 150}}
	plain := run(t, newWMPipeline(t, Config{Rungs: rungs, Audio: &Audio{}}), src)

	p := newWMPipeline(t, Config{Rungs: rungs, Audio: &Audio{}})
	outs, rest := pushVideoCount(t, p, src, 50)
	require.NoError(t, p.SetWatermark(&picture.Watermark{Image: magenta64(t), SizePct: 25}))
	o, rest := pushVideoCount(t, p, rest, 50)
	outs = append(outs, o...)
	require.NoError(t, p.SetWatermark(nil))
	outs = append(outs, run(t, p, rest)...)

	assert.Equal(t, rungFrames(plain, -1), rungFrames(outs, -1), "audio")
	for rung := range rungs {
		requireSameTiming(t, rungFrames(plain, rung), rungFrames(outs, rung), fmt.Sprintf("rung %d", rung))
	}
	// The decoder holds a few pictures (frame threads, reordering), so the
	// swaps land some pictures before the 50th and 100th decoded one; both
	// land the same number of pictures early, so the mark is on exactly 50
	// consecutive pictures, the same ones on every rung.
	var first []int
	for rung := range rungs {
		imgs := decodeRung(t, outs, rung)
		require.Len(t, imgs, 150)
		var on []int
		for i, img := range imgs {
			if near(region(img, picture.TopLeft, 25, squareHPct(img, 25)), magenta, 40) {
				on = append(on, i)
			}
		}
		require.Len(t, on, 50, "rung %d: marked pictures %v", rung, on)
		assert.Equal(t, on[0]+49, on[49], "rung %d: the marked pictures are not consecutive: %v", rung, on)
		assert.Greater(t, on[0], 20, "rung %d: marked from picture %d", rung, on[0])
		assert.LessOrEqual(t, on[0], 50, "rung %d: marked from picture %d", rung, on[0])
		if rung == 0 {
			first = on
		} else {
			assert.Equal(t, first, on, "rung %d", rung)
		}
		t.Logf("rung %d: marked pictures %d to %d", rung, on[0], on[49])
	}
	assert.Equal(t, Stats{}, p.Stats())
}

func TestSetWatermarkInvalid(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 25},
	})
	outs, rest := pushVideoCount(t, p, src, 50)
	for _, w := range []picture.Watermark{
		{},
		{Text: "a", Image: magenta64(t)},
		{Text: "%q"},
		{Image: []byte("not an image")},
		{Text: "a", TimeZone: "Nowhere/City"},
	} {
		require.Error(t, p.SetWatermark(&w), "%+v", w)
	}
	outs = append(outs, run(t, p, rest)...)
	imgs := decodeRung(t, outs, 0)
	require.Len(t, imgs, 150)
	last := imgs[len(imgs)-1]
	assert.True(t, near(region(last, picture.TopLeft, 25, squareHPct(last, 25)), magenta, 40), "the previous watermark stays")

	audio := newWMPipeline(t, Config{Audio: &Audio{}})
	require.Error(t, audio.SetWatermark(&picture.Watermark{Text: "a"}))
	require.NoError(t, audio.SetWatermark(nil))
}

func TestWatermarkNeedsRungs(t *testing.T) {
	_, err := NewWith(Config{Audio: &Audio{}, Watermark: &picture.Watermark{Text: "a"}})
	require.Error(t, err)
	_, err = NewWith(Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Text: "%q"}})
	require.Error(t, err, "an invalid watermark")
}

// ostxDirs returns the ostx-wm-* directories in the temp directory.
func ostxDirs(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(os.TempDir(), "ostx-wm-*"))
	require.NoError(t, err)
	return m
}

func TestWatermarkTempDirRemoved(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	p, err := NewWith(Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Text: "x"}})
	require.NoError(t, err)
	push(t, p, source(t, "h264_aac.ts")[:40])
	dirs := ostxDirs(t)
	require.Len(t, dirs, 1)
	assert.Equal(t, dirs[0], p.video.wm.dir)
	font, err := os.ReadFile(filepath.Join(dirs[0], "font.ttf"))
	require.NoError(t, err)
	assert.Equal(t, dejaVuSans, font)
	p.Close()
	_, err = os.Stat(dirs[0])
	require.ErrorIs(t, err, os.ErrNotExist)

	// An image watermark needs no directory; a text one set later makes it.
	q, err := NewWith(Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Image: magenta64(t)}})
	require.NoError(t, err)
	assert.Empty(t, ostxDirs(t))
	require.NoError(t, q.SetWatermark(&picture.Watermark{Text: "y"}))
	require.Len(t, ostxDirs(t), 1)
	q.Close()
	assert.Empty(t, ostxDirs(t))
}

// A temp directory whose path has a character outside [A-Za-z0-9_./-] is
// refused: it would be written into the filter-graph string.
func TestWatermarkTempDirChars(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "a b")
	require.NoError(t, os.Mkdir(tmp, 0o700))
	t.Setenv("TMPDIR", tmp)
	_, err := NewWith(Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Text: "x"}})
	require.ErrorContains(t, err, "character")
	m, err := filepath.Glob(filepath.Join(tmp, "*"))
	require.NoError(t, err)
	assert.Empty(t, m, "the refused directory is removed")
}

// deadPID returns the PID of a process that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "true")
	require.NoError(t, cmd.Run())
	pid := cmd.Process.Pid
	require.True(t, pidGone(pid), "PID %d was reused at once", pid)
	return pid
}

// TestWatermarkSweepsStaleDirs: building a text watermark first removes
// the private directories of dead children (a SIGKILL or a crash skips
// close), and nothing else: not a directory whose PID is alive (this
// process's, or PID 1's), not a name off the pattern, not a file or a
// symlink with a dead PID's name. The new directory carries this
// process's PID.
func TestWatermarkSweepsStaleDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dead := deadPID(t)
	mkdir := func(name string) string {
		d := filepath.Join(tmp, name)
		require.NoError(t, os.Mkdir(d, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(d, "font.ttf"), []byte("font"), 0o600))
		return d
	}
	stale := mkdir(fmt.Sprintf("ostx-wm-%d-123", dead))
	stale2 := mkdir(fmt.Sprintf("ostx-wm-%d-9", dead))
	file := filepath.Join(tmp, fmt.Sprintf("ostx-wm-%d-5", dead))
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	target := mkdir("target")
	link := filepath.Join(tmp, fmt.Sprintf("ostx-wm-%d-6", dead))
	require.NoError(t, os.Symlink(target, link))
	kept := []string{
		mkdir(fmt.Sprintf("ostx-wm-%d-456", os.Getpid())), // alive: this process
		mkdir("ostx-wm-1-77"),                             // alive: init
		mkdir(fmt.Sprintf("ostx-wm-%d", dead)),            // no random part
		mkdir(fmt.Sprintf("ostx-wm-%d-12x", dead)),        // not digits
		mkdir(fmt.Sprintf("ostx-wm-0-%d", dead)),          // PID 0 names a group
		mkdir(fmt.Sprintf("xostx-wm-%d-1", dead)),         // another prefix
		mkdir("ostx-wm-abc"),                              // a v1-era name
		mkdir("ostx-wm-2147483648-1"),                     // a 32-bit pid of -2^31: a group
		mkdir("ostx-wm-4294967297-1"),                     // wraps to pid 1 as 32 bits
		mkdir("ostx-wm-99999999-1"),                       // 8 digits: above pid_max
		file, target, link, filepath.Join(target, "font.ttf"),
	}

	p, err := NewWith(Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Text: "x"}})
	require.NoError(t, err)
	for _, d := range []string{stale, stale2} {
		_, err = os.Lstat(d)
		require.ErrorIs(t, err, os.ErrNotExist, "%s was not swept", d)
	}
	for _, k := range kept {
		_, err = os.Lstat(k)
		require.NoError(t, err, "%s was removed", k)
	}
	own := p.video.wm.dir
	assert.True(t, strings.HasPrefix(filepath.Base(own), fmt.Sprintf("ostx-wm-%d-", os.Getpid())), "own dir %s", own)
	assert.Regexp(t, wmDirName, filepath.Base(own))
	p.Close()
	_, err = os.Stat(own)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPIDGone(t *testing.T) {
	assert.False(t, pidGone(os.Getpid()))
	assert.False(t, pidGone(1), "init is alive (EPERM is alive)")
	assert.False(t, pidGone(0))
	assert.False(t, pidGone(-1))
	assert.True(t, pidGone(deadPID(t)))
}

// A watermark that cannot be built is fatal (R-C9): here the text file has
// been replaced by a directory when the next graph is built.
func TestWatermarkBuildFailureIsFatal(t *testing.T) {
	src := source(t, "h264_aac.ts")
	p := newWMPipeline(t, Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Text: "a"}})
	_, rest := pushVideoCount(t, p, src, 30)
	text := filepath.Join(p.video.wm.dir, "text")
	require.NoError(t, os.Remove(text))
	require.NoError(t, os.Mkdir(text, 0o700))
	require.NoError(t, p.SetWatermark(&picture.Watermark{Text: "a", Position: picture.Center}))
	var err error
	for _, f := range rest {
		if _, err = p.Push(f); err != nil {
			break
		}
	}
	require.ErrorContains(t, err, "watermark")
	_, err2 := p.Push(rest[len(rest)-1])
	assert.Equal(t, err, err2, "the error is permanent")
}

// A temp cleaner may remove the font, the text or the whole directory on a
// long stream: the next graph build writes them again, the font from the
// embedded copy, never falling back to fontconfig (R-C7).
func TestWatermarkFilesRestored(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := unmarked(t, src)
	for _, tc := range []struct {
		name   string
		remove func(dir string) error
	}{
		{"files", func(dir string) error {
			return errors.Join(os.Remove(filepath.Join(dir, "font.ttf")), os.Remove(filepath.Join(dir, "text")))
		}},
		{"directory", os.RemoveAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newWMPipeline(t, Config{Rungs: []Rung{{Height: 240, BitrateKbps: 600}}, Watermark: &picture.Watermark{Text: "a"}})
			outs, rest := pushVideoCount(t, p, src, 30)
			dir := p.video.wm.dir
			require.NoError(t, tc.remove(dir))
			// The same text: the text cache has nothing new to write, so only
			// the build can bring the file back.
			require.NoError(t, p.SetWatermark(&picture.Watermark{Text: "a", Position: picture.BottomLeft}))
			outs = append(outs, run(t, p, rest)...)
			font, err := os.ReadFile(filepath.Join(dir, "font.ttf"))
			require.NoError(t, err)
			assert.Equal(t, dejaVuSans, font)
			text, err := os.ReadFile(filepath.Join(dir, "text"))
			require.NoError(t, err)
			assert.Equal(t, "a", string(text))
			imgs := decodeRung(t, outs, 0)
			require.Len(t, imgs, len(plain))
			last := len(imgs) - 1
			assert.Greater(t, band(imgs[last], plain[last], textBand), 2.0, "the last picture has no text")
		})
	}
}

// The image is added at the first picture's PTS, so a picture before 0 is
// marked too.
func TestWatermarkNegativePTS(t *testing.T) {
	src := source(t, "h264_aac.ts")
	const shift = 1_000_000
	for i := range src {
		src[i].PTS -= shift
		src[i].DTS -= shift
	}
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Image: magenta64(t), SizePct: 25},
	})
	outs := run(t, p, src)
	v := rungFrames(outs, 0)
	require.NotEmpty(t, v)
	require.Negative(t, v[len(v)-1].PTS)
	imgs := decodeRung(t, outs, 0)
	require.Len(t, imgs, 150)
	for _, i := range []int{0, len(imgs) - 1} {
		got := region(imgs[i], picture.TopLeft, 25, squareHPct(imgs[i], 25))
		assert.True(t, near(got, magenta, 40), "picture %d: %v", i, got)
	}
}

// monoFont is the fixture DejaVu Sans Mono, an operator's font.
func monoFont(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/fixtures/fonts/DejaVuSansMono.ttf")
	require.NoError(t, err)
	return b
}

// badFont is 4 kB of zeros behind a valid table directory: CheckFont takes
// it, FreeType does not.
func badFont(t *testing.T) []byte {
	t.Helper()
	tags := []string{"cmap", "glyf", "head", "hhea", "hmtx", "loca", "maxp"}
	b := make([]byte, 4096)
	copy(b, "\x00\x01\x00\x00")
	b[5] = byte(len(tags))
	for i, tag := range tags {
		r := b[12+16*i:]
		copy(r, tag)
		r[10] = 0x02 // offset 512
		r[15] = 0x40 // length 64
	}
	require.NoError(t, picture.CheckFont(b))
	return b
}

// blackFrame is a 640×360 black yuv420p picture, freed at the end of the
// test.
func blackFrame(t *testing.T) *astiav.Frame {
	t.Helper()
	fr := astiav.AllocFrame()
	t.Cleanup(fr.Free)
	fr.SetWidth(640)
	fr.SetHeight(360)
	fr.SetPixelFormat(astiav.PixelFormatYuv420P)
	fr.SetSampleAspectRatio(astiav.NewRational(1, 1))
	require.NoError(t, fr.AllocBuffer(0))
	require.NoError(t, fr.ImageFillBlack())
	return fr
}

// litBox draws the watermark on fr and returns the bounding box of the
// pixels whose luma is over 128.
func litBox(t *testing.T, m *watermarker, fr *astiav.Frame) image.Rectangle {
	t.Helper()
	out, err := m.apply(fr)
	require.NoError(t, err)
	b, err := out.Data().Bytes(1)
	require.NoError(t, err)
	w, h := out.Width(), out.Height()
	require.GreaterOrEqual(t, len(b), w*h)
	box := image.Rectangle{}
	for y := range h {
		for x := range w {
			if b[y*w+x] > 128 {
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	require.False(t, box.Empty(), "no lit pixel")
	return box
}

// requireFontFile checks that <dir>/font.ttf holds want.
func requireFontFile(t *testing.T, m *watermarker, want []byte, msg string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(m.dir, "font.ttf"))
	require.NoError(t, err, msg)
	require.True(t, bytes.Equal(want, got), "%s: font.ttf holds %d bytes, not the %d expected", msg, len(got), len(want))
}

// newTestWatermarker returns a watermarker in a private TMPDIR, closed at
// the end of the test.
func newTestWatermarker(t *testing.T) *watermarker {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	m := newWatermarker(nil)
	t.Cleanup(m.close)
	return m
}

// TestWatermarkFont: the operator's font draws the text. Eight i's are at
// least 1.5 times as wide in a monospaced font, whose i is as wide as an m,
// and the graph string is the same: the font goes into the watermarker's
// own font.ttf, never into the string.
func TestWatermarkFont(t *testing.T) {
	mono := monoFont(t)
	fr := blackFrame(t)
	m := newTestWatermarker(t)

	wm := picture.Watermark{Text: "iiiiiiii", SizePct: 20, Position: picture.TopLeft}
	require.NoError(t, m.set(&wm))
	sans := litBox(t, m, fr)
	sansDesc := m.desc

	wm.Font = mono
	require.NoError(t, m.set(&wm))
	wide := litBox(t, m, fr)
	t.Logf("lit width: built-in %d, mono %d", sans.Dx(), wide.Dx())
	assert.GreaterOrEqual(t, float64(wide.Dx()), 1.5*float64(sans.Dx()))
	assert.Equal(t, sansDesc, m.desc, "the graph string does not change with the font")

	// The watermarker holds the font once: its own reference, the caller's
	// slice; the resolved watermark it keeps has none.
	assert.Nil(t, m.wm.Font)
	require.NotEmpty(t, m.font)
	assert.Same(t, &mono[0], &m.font[0])

	desc := m.desc
	for _, f := range [][]byte{dejaVuSans, mono} {
		assert.NotContains(t, desc, string(f[:16]))
	}
	assert.Less(t, len(desc), 1024)
	for _, kv := range []string{"fontfile=" + m.dir + "/font.ttf:", "textfile=" + m.dir + "/text:"} {
		require.Equal(t, 1, strings.Count(desc, kv), "%q in %q", kv, desc)
		desc = strings.Replace(desc, kv, "", 1)
	}
	assert.NotContains(t, desc, "/", "a path outside the watermarker's directory in %q", m.desc)
}

// TestWatermarkFontChange: changing the font on one watermarker rewrites
// font.ttf, and the text widens and narrows again.
func TestWatermarkFontChange(t *testing.T) {
	mono := monoFont(t)
	fr := blackFrame(t)
	m := newTestWatermarker(t)
	wm := picture.Watermark{Text: "iiiiiiii", SizePct: 20}

	require.NoError(t, m.set(&wm))
	first := litBox(t, m, fr)
	requireFontFile(t, m, dejaVuSans, "built-in")

	wm.Font = mono
	require.NoError(t, m.set(&wm))
	wide := litBox(t, m, fr)
	requireFontFile(t, m, mono, "mono")
	assert.GreaterOrEqual(t, float64(wide.Dx()), 1.5*float64(first.Dx()))

	wm.Font = nil
	require.NoError(t, m.set(&wm))
	again := litBox(t, m, fr)
	requireFontFile(t, m, dejaVuSans, "built-in again")
	assert.Equal(t, first, again)
	_, err := os.Stat(filepath.Join(m.dir, "font.tmp"))
	require.ErrorIs(t, err, os.ErrNotExist, "font.tmp is renamed away")
}

// TestWatermarkFontRestored: a temp cleaner removes font.ttf under an
// operator's font: the next build writes that font back, not DejaVu.
func TestWatermarkFontRestored(t *testing.T) {
	mono := monoFont(t)
	fr := blackFrame(t)
	m := newTestWatermarker(t)
	wm := picture.Watermark{Text: "iiiiiiii", SizePct: 20, Font: mono}
	require.NoError(t, m.set(&wm))
	wide := litBox(t, m, fr)

	require.NoError(t, os.Remove(filepath.Join(m.dir, "font.ttf")))
	// The same watermark: set has nothing new to write, so only the build
	// can bring the file back.
	require.NoError(t, m.set(&wm))
	assert.Equal(t, wide, litBox(t, m, fr))
	requireFontFile(t, m, mono, "restored")

	require.NoError(t, os.RemoveAll(m.dir))
	require.NoError(t, m.set(&wm))
	assert.Equal(t, wide, litBox(t, m, fr))
	requireFontFile(t, m, mono, "directory restored")
}

// TestWatermarkBadFont: a font CheckFont takes and FreeType refuses fails
// the build with an error about the font.
func TestWatermarkBadFont(t *testing.T) {
	fr := blackFrame(t)
	m := newTestWatermarker(t)
	require.NoError(t, m.set(&picture.Watermark{Text: "a", Font: badFont(t)}))
	_, err := m.apply(fr)
	require.ErrorContains(t, err, "font")
}

func TestProbeFont(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	require.NoError(t, ProbeFont(monoFont(t)))
	err := ProbeFont(badFont(t))
	require.ErrorContains(t, err, "font")
	require.Error(t, ProbeFont(nil))
	require.Error(t, ProbeFont([]byte("not a font")))
	assert.Empty(t, ostxDirs(t), "the probe removes its directory")
}

// TestPipelineWatermarkFont: a font in Config and in SetWatermark reaches
// the picture.
func TestPipelineWatermarkFont(t *testing.T) {
	src := source(t, "h264_aac.ts")
	plain := unmarked(t, src)
	mono := monoFont(t)
	p := newWMPipeline(t, Config{
		Rungs:     []Rung{{Height: 240, BitrateKbps: 600}},
		Watermark: &picture.Watermark{Text: "a", Position: picture.BottomLeft, Font: mono},
	})
	outs, rest := pushVideoCount(t, p, src, 30)
	requireFontFile(t, p.video.wm, mono, "configured")
	require.NoError(t, p.SetWatermark(&picture.Watermark{Text: "a", Position: picture.BottomLeft}))
	outs = append(outs, run(t, p, rest)...)
	requireFontFile(t, p.video.wm, dejaVuSans, "set without a font")
	imgs := decodeRung(t, outs, 0)
	require.Len(t, imgs, len(plain))
	for _, i := range []int{0, len(imgs) - 1} {
		assert.Greater(t, band(imgs[i], plain[i], textBand), 2.0, "picture %d has no text", i)
	}
}
