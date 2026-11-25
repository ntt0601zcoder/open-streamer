package e2e

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// The M4c scenarios run the picture features of the real transcoder child
// with software encoders: the watermark on every encoded rung (image and
// text, set at the start, live, and across a respawn), the thumbnails of a
// transcoded ladder, of a passthrough stream (Thumbnailer) and of a ladder
// that copies its video, and the frozen behaviour without them. Pictures
// are read by muxing a rendition's GOP with internal/ts and decoding it with
// ffmpeg; shots are decoded with image/jpeg.

// m4cMarkPct is the image watermark's SizePct in every M4c scenario.
const m4cMarkPct = 25

// m4cMarkTol is how far, per channel, a mark region's mean colour may be
// from magenta to count as carrying the mark.
const m4cMarkTol = 40

// m4cSpeed is the pace of the thumbnail scenarios' paced feeds, in times
// the fixture's own: the passthrough child shoots keyframes alone, so it
// keeps up.
const m4cSpeed = 4

// m4cShotStep is the thumbnail interval of the M4c scenarios, 2 s, in
// 90 kHz ticks.
const m4cShotStep = 2 * media.ClockRate

// m4cLadder is the ladder of the M4c watermark scenarios: m4aLadder's three
// encoded rungs, each with Encoder set, and no copy rung, since a
// watermarked ladder may not copy its video (ruling R-C5).
//
//nolint:unparam // encoder is part of the helper's contract: Task 11 runs the NVENC ladder
func m4cLadder(binary, encoder string) transcode.Options {
	return transcode.Options{
		Binary: binary,
		Renditions: []transcode.Rendition{
			{Name: "t240", Video: transcode.Video{Height: 240, BitrateKbps: 600, Encoder: encoder}},
			{Name: "t180", Video: transcode.Video{Height: 180, BitrateKbps: 300, Encoder: encoder}},
			{Name: "t120", Video: transcode.Video{Height: 120, BitrateKbps: 150, Encoder: encoder}},
		},
		Audio: transcode.Audio{BitrateKbps: 96},
		Queue: 2048,
	}
}

// magentaPNG is a 64×64 opaque PNG of RGB 255,0,255: the image watermark.
func magentaPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.SetNRGBA(x, y, color.NRGBA{255, 0, 255, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err) // an in-memory encode of a valid image does not fail
	}
	return b.Bytes()
}

// imageMark is the image watermark of the M4c scenarios.
func imageMark() *picture.Watermark {
	return &picture.Watermark{Image: magentaPNG(), SizePct: m4cMarkPct, Position: picture.TopLeft}
}

// firstPicture muxes the first GOP of frames' video (from its first video
// keyframe to the next, in the order given) to a TS file with internal/ts,
// as track 0, and returns its first picture as ffmpeg decodes it (-frames:v
// 1 -f rawvideo -pix_fmt rgb24), at the keyframe's configured size.
func firstPicture(t *testing.T, ffmpeg string, frames []media.Frame) image.Image {
	t.Helper()
	video := videoFrames(frames)
	start := slices.IndexFunc(video, func(f media.Frame) bool { return f.Key })
	require.GreaterOrEqual(t, start, 0, "a video keyframe")
	gop := []media.Frame{video[start]}
	for _, f := range video[start+1:] {
		if f.Key {
			break
		}
		gop = append(gop, f)
	}
	return decodePictures(t, ffmpeg, gop, 1)[0]
}

// decodePictures muxes video, video frames in decode order that start on a
// keyframe and keep its config, to a TS file with internal/ts, as track 0,
// and returns the first n pictures ffmpeg decodes of it (all when n is 0),
// as rgb24 at the keyframe's configured size.
func decodePictures(t *testing.T, ffmpeg string, video []media.Frame, n int) []image.Image {
	t.Helper()
	require.NotEmpty(t, video)
	require.True(t, video[0].Key, "the frames start on a keyframe")
	cfg := video[0].Config
	require.NotNil(t, cfg, "the keyframe's config")
	var buf bytes.Buffer
	m, err := ts.NewMuxer(&buf, []*media.TrackConfig{cfg})
	require.NoError(t, err)
	for _, f := range video {
		f.Track = 0
		require.NoError(t, m.WriteFrame(f))
	}
	path := filepath.Join(t.TempDir(), "video.ts")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-v", "error", "-i", path}
	if n > 0 {
		args = append(args, "-frames:v", strconv.Itoa(n))
	}
	args = append(args, "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, stderr.String())
	w, h := cfg.Width, cfg.Height
	size := w * h * 3
	require.NotZero(t, size)
	require.Zero(t, len(out)%size, "whole %dx%d rgb24 pictures; stderr %s", w, h, stderr.String())
	if n > 0 {
		require.Len(t, out, n*size, "%d pictures of %dx%d; stderr %s", n, w, h, stderr.String())
	}
	imgs := make([]image.Image, 0, len(out)/size)
	for ; len(out) > 0; out = out[size:] {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := range w * h {
			img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = out[3*i], out[3*i+1], out[3*i+2], 255
		}
		imgs = append(imgs, img)
	}
	return imgs
}

// markRegion is the mean RGB of the middle half, in each dimension, of the
// square a mark sizePct % of img's width occupies at pos, 2 % of img's
// height from its edges (picture.Margin), as the child places a square
// image watermark.
func markRegion(img image.Image, pos picture.Position, sizePct int) (r, g, b float64) {
	bd := img.Bounds()
	bw, bh := bd.Dx(), bd.Dy()
	side := int(math.Round(float64(bw) * float64(sizePct) / 100))
	m := picture.Margin(bh)
	var x, y int
	switch pos {
	case picture.TopLeft:
		x, y = m, m
	case picture.TopRight:
		x, y = bw-side-m, m
	case picture.BottomLeft:
		x, y = m, bh-side-m
	case picture.BottomRight:
		x, y = bw-side-m, bh-side-m
	case picture.Center:
		x, y = (bw-side)/2, (bh-side)/2
	}
	q := side / 4
	rect := image.Rect(x+q, y+q, x+side-q, y+side-q).Add(bd.Min)
	n := 0
	for py := rect.Min.Y; py < rect.Max.Y; py++ {
		for px := rect.Min.X; px < rect.Max.X; px++ {
			c := color.NRGBAModel.Convert(img.At(px, py)).(color.NRGBA)
			r += float64(c.R)
			g += float64(c.G)
			b += float64(c.B)
			n++
		}
	}
	return r / float64(n), g / float64(n), b / float64(n)
}

// marked reports whether img's mark region at pos is within m4cMarkTol of
// magenta on every channel, and returns the region's colour for messages.
func marked(img image.Image, pos picture.Position) (bool, [3]float64) {
	r, g, b := markRegion(img, pos, m4cMarkPct)
	c := [3]float64{r, g, b}
	return math.Abs(r-255) <= m4cMarkTol && g <= m4cMarkTol && math.Abs(b-255) <= m4cMarkTol, c
}

// topLeftBand is the rectangle of img that a short text watermark of the
// default size (5 %) covers at the top left, scaled from the 85 × 16 px band
// of a 320×240 picture 5 px from its corner.
func topLeftBand(img image.Image) image.Rectangle {
	bd := img.Bounds()
	sx := func(v int) int { return v * bd.Dx() / 320 }
	sy := func(v int) int { return v * bd.Dy() / 240 }
	return image.Rect(sx(5), sy(5), sx(90), sy(21)).Add(bd.Min)
}

// meanAbsDiff is the mean absolute difference of a and b over r, all three
// colour channels.
func meanAbsDiff(a, b image.Image, r image.Rectangle) float64 {
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

// m4cRuns runs transcodeSteps once per named Options, each over its own
// src(t), in parallel.
func m4cRuns(t *testing.T, src func(t *testing.T) feed, opts map[string]transcode.Options) map[string]m4aRun {
	t.Helper()
	out := map[string]m4aRun{}
	var mu sync.Mutex
	t.Run("transcode", func(t *testing.T) {
		for name, opt := range opts {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				steps, st := transcodeSteps(t, src(t), opt)
				for i, s := range steps {
					t.Logf("%s: %s", opt.Renditions[i].Name, kindCounts(stepFrames(s)))
				}
				t.Logf("stats %+v", st)
				mu.Lock()
				out[name] = m4aRun{steps, st}
				mu.Unlock()
			})
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	return out
}

// pacedFast writes steps at m4cSpeed times their own pace: each frame at
// (DTS − the first DTS) / m4cSpeed after the start.
func pacedFast(steps []step) feed {
	return func(ctx context.Context, h *hub.Hub) error {
		start := time.Now()
		first := steps[0].f.DTS
		for _, s := range steps {
			due := start.Add(media.ToDuration(s.f.DTS-first) / m4cSpeed)
			if d := time.Until(due); d > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(d):
				}
			}
			s.write(h)
		}
		return nil
	}
}

// m4cSettle is how long the thumbnail scenarios of a child that is offered
// keyframes wait after subscribing before they feed: time for the child to
// say Hello, so that the first key is normally taken and the bounds that
// hold when no key is skipped are exercised. Neither the Transcoder nor the
// Thumbnailer exports a Hello signal (EventSpawned comes before it), so the
// wait is fixed; a slower child only skips keys, which the bounds allow
// for, and the scenarios log a COVERAGE line when it does.
const m4cSettle = time.Second

// settled runs fd after m4cSettle.
func settled(fd feed) feed {
	return func(ctx context.Context, h *hub.Hub) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m4cSettle):
		}
		return fd(ctx, h)
	}
}

// loopedFixture is a fixture's Clock steps looped n times as one timeline
// (loopSteps).
func loopedFixture(t *testing.T, name string, n int) []step {
	t.Helper()
	return loopSteps(clockSteps(t, name, newFakeClock(m3aT0).Now), n)
}

// frameID is what the frozen comparisons compare of a frame: its timing,
// its config (configID) and its data.
type frameID struct {
	pts, dts int64
	key      bool
	epoch    uint32
	config   string
	data     string
}

// configID renders every field of c (codec, size, VPS, SPS, PPS, rate,
// channels, ASC) for comparison; "" for nil.
func configID(c *media.TrackConfig) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%v %dx%d vps %x sps %x pps %x %d Hz %d ch asc %x",
		c.Codec, c.Width, c.Height, c.VPS, c.SPS, c.PPS, c.SampleRate, c.Channels, c.ASC)
}

// trackIDs returns, per track, the frameIDs of frames in order.
func trackIDs(frames []media.Frame) map[int][]frameID {
	out := map[int][]frameID{}
	for _, f := range frames {
		out[f.Track] = append(out[f.Track],
			frameID{f.PTS, f.DTS, f.Key, f.Epoch, configID(f.Config), string(bytes.Join(f.Data, nil))})
	}
	return out
}

// x264Options returns the options string x264 writes in its
// user-data-unregistered SEI ("x264 - core ... - options: ..."), from the
// SEI NAL units of f's data, up to the string's NUL; ok is false when f has
// none.
func x264Options(f media.Frame) (string, bool) {
	for _, nal := range f.Data {
		if len(nal) == 0 || nal[0]&0x1f != 6 { // H.264 SEI
			continue
		}
		i := bytes.Index(nal, []byte("x264 - core"))
		if i < 0 {
			continue
		}
		opts := nal[i:]
		if end := bytes.IndexByte(opts, 0); end >= 0 {
			opts = opts[:end]
		}
		return string(opts), true
	}
	return "", false
}

// killAfterKeyframes kills tr's child once, after the n-th video keyframe
// has reached h (killAfterKeys, watching from a subscriber taken now), and
// returns a function to call once the run has ended: it fails the test
// unless the child was killed, and returns when it was.
func killAfterKeyframes(t *testing.T, tr *transcode.Transcoder, h *hub.Hub, n int) func() time.Time {
	t.Helper()
	killed := killAfterKeys(t, tr, h, n)
	return func() time.Time {
		t.Helper()
		at, ok := <-killed
		require.True(t, ok, "the child was killed after output keyframe %d", n)
		return at
	}
}

// shotLog collects the shots of an OnShot callback.
type shotLog struct {
	mu    sync.Mutex
	shots []picture.Shot
}

func (l *shotLog) add(s picture.Shot) {
	l.mu.Lock()
	l.shots = append(l.shots, s)
	l.mu.Unlock()
}

func (l *shotLog) get() []picture.Shot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.shots)
}

// decodeShot decodes s's JPEG, which must be s.Width × s.Height.
func decodeShot(t *testing.T, s picture.Shot) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(s.JPEG))
	require.NoError(t, err, "shot at PTS %d", s.PTS)
	require.Equal(t, image.Pt(s.Width, s.Height), img.Bounds().Size(), "shot at PTS %d: decoded size", s.PTS)
	return img
}

// checkShots checks that every shot decodes to w × h and that, within an
// epoch, consecutive shots are at least m4cShotStep apart and, when maxStep
// is positive, at most maxStep.
func checkShots(t *testing.T, shots []picture.Shot, w, h int, maxStep int64) {
	t.Helper()
	pts := make([]int64, 0, len(shots))
	for i, s := range shots {
		pts = append(pts, s.PTS)
		decodeShot(t, s)
		assert.Equal(t, image.Pt(w, h), image.Pt(s.Width, s.Height), "shot %d", i)
		if i == 0 || shots[i-1].Epoch != s.Epoch {
			continue
		}
		d := s.PTS - shots[i-1].PTS
		assert.GreaterOrEqual(t, d, int64(m4cShotStep), "shot %d: %d after shot %d", i, d, i-1)
		if maxStep > 0 {
			assert.LessOrEqual(t, d, maxStep, "shot %d: %d after shot %d", i, d, i-1)
		}
	}
	t.Logf("%d shots at PTS %v", len(shots), pts)
}

// TestM4cWatermarkEveryRung transcodes h264_aac.ts through m4cLadder three
// times: with the image mark (magentaPNG, SizePct 25, TopLeft), with the
// text mark "%Y-%m-%d %H:%M:%S" in Asia/Ho_Chi_Minh, and with none.
//   - image: on every rendition the first picture's mark region is within
//     40 of (255,0,255) and its BottomRight region is not; every rendition
//     packaged and read back as feedScenario does has no violation; every
//     rung's keyframe PTS set is the source's + D (as
//     TestM4aKeyframesAligned).
//   - text: on every rendition the top-left band of the first picture
//     differs from the unmarked run's by a mean absolute difference above 2.
func TestM4cWatermarkEveryRung(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	withMark := func(w *picture.Watermark) transcode.Options {
		opt := m4cLadder(bin, "")
		opt.Watermark = w
		return opt
	}
	runs := m4cRuns(t, func(t *testing.T) feed { return m4aSource(t, "h264_aac.ts") }, map[string]transcode.Options{
		"image": withMark(imageMark()),
		"text":  withMark(&picture.Watermark{Text: "%Y-%m-%d %H:%M:%S", TimeZone: "Asia/Ho_Chi_Minh"}),
		"plain": withMark(nil),
	})
	want := keySet(shiftedSource(t, "h264_aac.ts"))
	require.NotEmpty(t, want)
	for i, r := range m4cLadder(bin, "").Renditions {
		t.Run(r.Name, func(t *testing.T) {
			t.Parallel()
			steps := runs["image"].steps[i]
			frames := stepFrames(steps)
			img := firstPicture(t, ffmpeg, frames)
			ok, c := marked(img, picture.TopLeft)
			assert.True(t, ok, "the mark region is magenta: %v", c)
			other, oc := marked(img, picture.BottomRight)
			assert.False(t, other, "the bottom right is not: %v", oc)
			assert.Equal(t, want, keySet(frames), "the keyframe PTS set is the source's + D")

			text := firstPicture(t, ffmpeg, stepFrames(runs["text"].steps[i]))
			plain := firstPicture(t, ffmpeg, stepFrames(runs["plain"].steps[i]))
			require.Equal(t, plain.Bounds(), text.Bounds())
			band := meanAbsDiff(text, plain, topLeftBand(text))
			assert.Greater(t, band, 2.0, "the text band differs from the unmarked picture")
			t.Logf("%v: mark %v, bottom right %v, text band %.2f", img.Bounds().Size(), c, oc, band)

			feedScenario(t, steps)
		})
	}
}

// TestM4cWatermarkABR transcodes h264_aac.ts through m4cLadder with the
// image mark and the presentation: the presentation packaged in ABR mode
// (feedABR) has no violation and carries every frame, and every variant's
// first picture carries the mark.
func TestM4cWatermarkABR(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	opt := m4cLadder(bin, "")
	opt.Watermark = imageMark()
	opt.Presentation = true
	run := recordTranscode(t, m4aSource(t, "h264_aac.ts"), opt, nil)
	requireCleanRun(t, run.stats)
	require.Zero(t, run.stats.PresentationDropped, "%+v", run.stats)
	pres := stepFrames(run.presentation)
	requireRungZeroFirst(t, pres, len(opt.Renditions))
	t.Logf("presentation: %s", trackCounts(pres))
	_, _, caps := feedABR(t, run.presentation, 0)
	assertCompleteABR(t, caps, pres)
	for i, r := range opt.Renditions {
		img := firstPicture(t, ffmpeg, presentationTrackFrames(pres, transcode.PresentationTrack(i)))
		ok, c := marked(img, picture.TopLeft)
		assert.True(t, ok, "%s: the variant's first picture carries the mark: %v", r.Name, c)
		t.Logf("%s: %v, mark %v", r.Name, img.Bounds().Size(), c)
	}
}

// TestM4cWatermarkLiveUpdate feeds h264_aac.ts twice at its pace through
// m4cLadder with no watermark, and sets the image mark 4 s into the feed:
// every rendition has exactly one epoch, there is one child, and on every
// rendition the first picture has no mark and the first picture of the last
// GOP has it.
func TestM4cWatermarkLiveUpdate(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	opt := m4cLadder(bin, "")
	steps := loopedFixture(t, "h264_aac.ts", 2)
	var tr *transcode.Transcoder
	set := make(chan error, 1)
	src := func(ctx context.Context, h *hub.Hub) error {
		timer := time.AfterFunc(4*time.Second, func() { set <- tr.SetWatermark(imageMark()) })
		defer timer.Stop()
		return pacedSteps(steps)(ctx, h)
	}
	run := recordTranscode(t, src, opt, func(x *transcode.Transcoder) { tr = x })
	select {
	case err := <-set:
		require.NoError(t, err, "SetWatermark")
	default:
		require.Fail(t, "the watermark was not set during the feed")
	}
	requireCleanRun(t, run.stats)
	for i, r := range opt.Renditions {
		frames := stepFrames(run.renditions[i])
		assert.Equal(t, []uint32{1}, epochs(frames), "%s: one epoch", r.Name)
		first := firstPicture(t, ffmpeg, frames)
		ok, c := marked(first, picture.TopLeft)
		assert.False(t, ok, "%s: the first picture has no mark: %v", r.Name, c)
		video := videoFrames(frames)
		last := len(video) - 1
		for last > 0 && !video[last].Key {
			last--
		}
		lastImg := firstPicture(t, ffmpeg, video[last:])
		lok, lc := marked(lastImg, picture.TopLeft)
		assert.True(t, lok, "%s: the last GOP's first picture (PTS %d) has the mark: %v", r.Name, video[last].PTS, lc)
		t.Logf("%s: first picture %v, last GOP at PTS %d %v", r.Name, c, video[last].PTS, lc)
	}
}

// TestM4cWatermarkRespawn feeds h264_aac.ts twice at its pace through
// m4cLadder with the image mark, and kills the child after its 3rd output
// keyframe on t240 (killAfterKeyframes): there are two children, every
// rendition has epochs [1 2], and the first picture of epoch 2 on every
// rendition carries the mark.
func TestM4cWatermarkRespawn(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	opt := m4cLadder(bin, "")
	opt.Watermark = imageMark()
	var killed func() time.Time
	run := recordTranscode(t, pacedSteps(loopedFixture(t, "h264_aac.ts", 2)), opt, func(tr *transcode.Transcoder) {
		killed = killAfterKeyframes(t, tr, tr.Hubs()[0], 3)
	})
	at := killed()
	st := run.stats
	t.Logf("killed at %v; stats %+v", at.Format(time.TimeOnly), st)
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, st.Exits)
	assert.Zero(t, st.Losses)
	for i, r := range opt.Renditions {
		frames := stepFrames(run.renditions[i])
		require.Equal(t, []uint32{1, 2}, epochs(frames), r.Name)
		var e2 []media.Frame
		for _, f := range frames {
			if f.Epoch == 2 {
				e2 = append(e2, f)
			}
		}
		img := firstPicture(t, ffmpeg, e2)
		ok, c := marked(img, picture.TopLeft)
		assert.True(t, ok, "%s: epoch 2's first picture carries the mark: %v", r.Name, c)
		t.Logf("%s: epoch 2 first picture %v", r.Name, c)
	}
}

// requireFirstDue checks that each shot is the first picture due: an
// epoch's first shot is the epoch's first picture of frames (a
// rendition's), in PTS order, and for every later one no picture lies at
// or above the previous shot + m4cShotStep and below it. Within a pass of the
// fixture, whose pictures are 3600 apart, the shots are then at most
// 180000 + 3600 apart; across a loopSteps seam the gap between the passes
// adds to that (passDelta shifts a pass by the last PTS + one frame − the
// first DTS, beyond the first PTS by the B-frame delay).
func requireFirstDue(t *testing.T, shots []picture.Shot, frames []media.Frame) {
	t.Helper()
	pics := map[uint32][]int64{}
	for _, f := range videoFrames(frames) {
		pics[f.Epoch] = append(pics[f.Epoch], f.PTS)
	}
	for _, p := range pics {
		slices.Sort(p)
	}
	var steps []int64
	for i, b := range shots {
		p := pics[b.Epoch]
		require.NotEmpty(t, p, "shot %d: pictures of epoch %d", i, b.Epoch)
		if i == 0 || shots[i-1].Epoch != b.Epoch {
			// The epoch's first shot is due at once: its first picture.
			assert.Equal(t, p[0], b.PTS, "shot %d: the first picture of epoch %d", i, b.Epoch)
			continue
		}
		a := shots[i-1]
		j, found := slices.BinarySearch(p, b.PTS)
		require.True(t, found, "shot %d at PTS %d is a picture of the rendition", i, b.PTS)
		require.Positive(t, j, "shot %d", i)
		assert.Less(t, p[j-1], a.PTS+m4cShotStep,
			"shot %d at PTS %d: the picture at %d before it was already due", i, b.PTS, p[j-1])
		steps = append(steps, b.PTS-a.PTS)
	}
	t.Logf("steps between shots %v", steps)
}

// TestM4cThumbnailsTranscoded feeds h264_aac.ts four times as one timeline
// (24 s) at its pace through m4cLadder with Thumbnail{IntervalSec: 2,
// Height: 120}, the shots collected by OnShot: at least 11; within an
// epoch, consecutive PTS differ by at least 180000 and each shot is the
// first picture due (requireFirstDue: at most 180000 + 3600 apart within a
// pass); every JPEG decodes to 160×120; the first and the last differ;
// Shot returns the last one.
func TestM4cThumbnailsTranscoded(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	var log shotLog
	opt := m4cLadder(bin, "")
	opt.Thumbnail = &picture.Thumbnail{IntervalSec: 2, Height: 120}
	opt.OnShot = log.add
	var tr *transcode.Transcoder
	run := recordTranscode(t, pacedSteps(loopedFixture(t, "h264_aac.ts", 4)), opt,
		func(x *transcode.Transcoder) { tr = x })
	requireCleanRun(t, run.stats)
	t.Logf("stats %+v", run.stats)
	shots := log.get()
	require.GreaterOrEqual(t, len(shots), 11)
	checkShots(t, shots, 160, 120, 0)
	requireFirstDue(t, shots, stepFrames(run.renditions[0]))
	first, last := shots[0], shots[len(shots)-1]
	assert.NotEqual(t, first.JPEG, last.JPEG, "the first and the last shot differ")
	got, _, ok := tr.Shot()
	require.True(t, ok, "Shot has one")
	assert.Equal(t, last, got, "Shot returns the last shot")
}

// thumbnailerRun runs a Thumbnailer (interval 2 s, height 120) on a hub fed
// steps at m4cSpeed times their pace, m4cSettle after the Thumbnailer has
// subscribed.
// Once the feed has ended it waits for the shot of every key sent, then
// closes the hub; it returns the shots, in order, and the final stats.
func thumbnailerRun(t *testing.T, bin string, steps []step) ([]picture.Shot, transcode.ThumbnailerStats) {
	t.Helper()
	var log shotLog
	h := hub.New(hub.Options{})
	th, err := transcode.NewThumbnailer(h, transcode.ThumbnailerOptions{
		Binary:    bin,
		Thumbnail: picture.Thumbnail{IntervalSec: 2, Height: 120},
		Logger:    m4aLogger(t),
		OnShot:    log.add,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- th.Run(ctx) }()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 10*time.Second, time.Millisecond,
		"the thumbnailer subscribed")
	ferr := settled(pacedFast(steps))(ctx, h)
	assert.Eventually(t, func() bool {
		st := th.Stats()
		return st.Shots+st.Failed >= st.KeysSent
	}, 10*time.Second, 5*time.Millisecond, "a shot for every key sent")
	h.Close()
	require.NoError(t, <-runErr)
	require.NoError(t, ferr)
	st := th.Stats()
	t.Logf("thumbnailer stats %+v", st)
	return log.get(), st
}

// TestM4cThumbnailsPassthrough runs a hub with only a Thumbnailer
// (interval 2, height 120), fed at 4 times the fixture's pace so that the
// child keeps up:
//   - h264_aac.ts four times as one timeline (6 s): a shot for every key
//     sent; at least 11 − KeysSkipped shots; consecutive PTS at least 180000
//     apart and, when no key was skipped, less than 270000 (the fixture's
//     keyframes are 1 s apart); 160×120.
//   - h264_opengop.ts and h265_aac51.ts, twice each: a shot for every key
//     sent; at least 4 − KeysSkipped shots, all decodable.
func TestM4cThumbnailsPassthrough(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	for _, row := range []struct {
		name   string
		passes int
		min    int
	}{
		{"h264_aac.ts", 4, 11},
		{"h264_opengop.ts", 2, 4},
		{"h265_aac51.ts", 2, 4},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			shots, st := thumbnailerRun(t, bin, loopedFixture(t, row.name, row.passes))
			if st.KeysSkipped > 0 {
				t.Logf("COVERAGE: %d keys skipped (the child was not ready): the no-skip bounds are relaxed",
					st.KeysSkipped)
			}
			assert.EqualValues(t, 1, st.Spawns, "one child")
			assert.Zero(t, st.Failed, "no shot failed")
			assert.Equal(t, st.KeysSent, st.Shots, "a shot for every key sent")
			assert.GreaterOrEqual(t, len(shots), row.min-int(st.KeysSkipped))
			require.NotEmpty(t, shots)
			if row.name != "h264_aac.ts" {
				for _, s := range shots {
					decodeShot(t, s)
				}
				t.Logf("%d shots, the first %dx%d", len(shots), shots[0].Width, shots[0].Height)
				return
			}
			var maxStep int64
			if st.KeysSkipped == 0 {
				maxStep = 270000 - 1
			}
			checkShots(t, shots, 160, 120, maxStep)
		})
	}
}

// copyLadder is the ladder of the copy-video thumbnail scenarios: the
// source video copied, the audio re-encoded to AAC at 96 kbps.
func copyLadder(binary string, th *picture.Thumbnail, onShot func(picture.Shot)) transcode.Options {
	return transcode.Options{
		Binary:     binary,
		Renditions: []transcode.Rendition{{Name: "src", Video: transcode.Video{Copy: true}}},
		Audio:      transcode.Audio{BitrateKbps: 96},
		Queue:      2048,
		Thumbnail:  th,
		OnShot:     onShot,
	}
}

// TestM4cThumbnailsCopyVideo runs a ladder that copies its video, with the
// audio re-encoded to AAC, so its child decodes no video and is offered the
// keyframes to shoot (ruling R-C10a), fed at 4 times the fixture's pace
// (after m4cSettle):
//   - h264_aac.ts four times as one timeline with Thumbnail{2, 120, 80}:
//     one child; at least 11 − ShotKeysSkipped shots, at least 2 s apart,
//     each decoding to 160×120; Stats.Child.Thumbnails equals the shots
//     OnShot saw and ThumbnailsFailed is 0; the rendition's frames are a
//     run's without thumbnails, track by track.
//   - h264_aac_then_640.ts with interval 3600: one child and one shot per
//     epoch, at the epoch's first keyframe or one at most ShotKeysSkipped
//     keyframes after it; the second, of 640×360, 214×120.
func TestM4cThumbnailsCopyVideo(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	t.Run("h264_aac.ts", func(t *testing.T) {
		t.Parallel()
		steps := loopedFixture(t, "h264_aac.ts", 4)
		var log shotLog
		var with, without transcodeRun
		t.Run("transcode", func(t *testing.T) {
			t.Run("thumbnails", func(t *testing.T) {
				t.Parallel()
				with = recordTranscode(t, settled(pacedFast(steps)),
					copyLadder(bin, &picture.Thumbnail{IntervalSec: 2, Height: 120, Quality: 80}, log.add), nil)
				requireCleanRun(t, with.stats)
				t.Logf("stats %+v", with.stats)
			})
			t.Run("plain", func(t *testing.T) {
				t.Parallel()
				without = recordTranscode(t, settled(pacedFast(steps)), copyLadder(bin, nil, nil), nil)
				requireCleanRun(t, without.stats)
			})
		})
		if t.Failed() {
			t.FailNow()
		}
		st := with.stats
		if st.ShotKeysSkipped > 0 {
			t.Logf("COVERAGE: %d shot keys skipped (the child was not ready): the shot count bound is relaxed",
				st.ShotKeysSkipped)
		}
		shots := log.get()
		assert.GreaterOrEqual(t, len(shots), 11-int(st.ShotKeysSkipped))
		require.NotEmpty(t, shots)
		checkShots(t, shots, 160, 120, 0)
		assert.EqualValues(t, len(shots), st.Child.Thumbnails, "the children's count is the shots OnShot saw")
		assert.Zero(t, st.Child.ThumbnailsFailed)
		got, want := trackIDs(stepFrames(with.renditions[0])), trackIDs(stepFrames(without.renditions[0]))
		require.Len(t, got, len(want))
		for tr, ids := range want {
			assert.Equal(t, ids, got[tr], "track %d: the frames of a run without thumbnails", tr)
		}
	})
	t.Run("h264_aac_then_640.ts", func(t *testing.T) {
		t.Parallel()
		var log shotLog
		run := recordTranscode(t, settled(pacedFast(clockSteps(t, "h264_aac_then_640.ts", newFakeClock(m3aT0).Now))),
			copyLadder(bin, &picture.Thumbnail{IntervalSec: 3600, Height: 120}, log.add), nil)
		requireCleanRun(t, run.stats)
		st := run.stats
		t.Logf("stats %+v", st)
		if st.ShotKeysSkipped > 0 {
			t.Logf("COVERAGE: %d shot keys skipped (the child was not ready): a later keyframe is accepted",
				st.ShotKeysSkipped)
		}
		frames := stepFrames(run.renditions[0])
		keys := map[uint32][]int64{} // per epoch, the video keyframe PTS in order
		for _, f := range videoFrames(frames) {
			if f.Key {
				keys[f.Epoch] = append(keys[f.Epoch], f.PTS)
			}
		}
		eps := epochs(frames)
		require.Equal(t, []uint32{1, 2}, eps)
		shots := log.get()
		require.Len(t, shots, len(eps), "one shot per epoch")
		w2, h2 := picture.ShotSize(640, 360, 1, 1, 120)
		for i, s := range shots {
			require.Equal(t, eps[i], s.Epoch, "shot %d", i)
			k := keys[s.Epoch]
			require.NotEmpty(t, k, "epoch %d keys", s.Epoch)
			// The epoch's first keyframe, or a later one for each key
			// skipped.
			j := slices.Index(k, s.PTS)
			require.GreaterOrEqual(t, j, 0, "shot %d at PTS %d is a keyframe of epoch %d: %v", i, s.PTS, s.Epoch, k)
			assert.LessOrEqual(t, uint64(j), st.ShotKeysSkipped,
				"shot %d: keyframe %d of epoch %d, with %d keys skipped", i, j, s.Epoch, st.ShotKeysSkipped)
			decodeShot(t, s)
		}
		assert.Equal(t, image.Pt(160, 120), image.Pt(shots[0].Width, shots[0].Height))
		assert.Equal(t, image.Pt(w2, h2), image.Pt(shots[1].Width, shots[1].Height))
		t.Logf("shots %dx%d at %d, %dx%d at %d; keys %v", shots[0].Width, shots[0].Height, shots[0].PTS,
			shots[1].Width, shots[1].Height, shots[1].PTS, keys)
	})
}

// m4cNoise bounds the mean difference, per channel 0..255, between two
// encodes of the same picture: libx264's threads make two runs of the same
// input differ in a few bits (as the native tests' rungNoise).
const m4cNoise = 4.0

// TestM4cFrozen transcodes h264_aac.ts through m4aLadder twice, once as M4b
// ran it and once with Watermark nil, Thumbnail nil and Encoder "x264"
// spelled out on every encoded rung. Per rendition and track, the frames
// have the same PTS, DTS, Key, Epoch and Config (codec, size, VPS, SPS,
// PPS, rate, channels, ASC); the audio and the copied video the same Data.
// An encoded rung's Data cannot be compared byte for byte (libx264's
// threads): its first IDR's x264 options SEI must be the same, which pins
// the encoder's settings (preset, refs, profile, rate control), and its
// pictures, all decoded, must match within m4cNoise.
func TestM4cFrozen(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	spelled := m4aLadder(bin)
	spelled.Watermark, spelled.Thumbnail = nil, nil
	for i := range spelled.Renditions {
		if !spelled.Renditions[i].Video.Copy {
			spelled.Renditions[i].Video.Encoder = transcode.EncoderX264
		}
	}
	runs := m4cRuns(t, func(t *testing.T) feed { return m4aSource(t, "h264_aac.ts") }, map[string]transcode.Options{
		"m4b":     m4aLadder(bin),
		"spelled": spelled,
	})
	for i, r := range spelled.Renditions {
		t.Run(r.Name, func(t *testing.T) {
			t.Parallel()
			a, b := stepFrames(runs["m4b"].steps[i]), stepFrames(runs["spelled"].steps[i])
			want, got := trackIDs(a), trackIDs(b)
			require.Len(t, got, len(want))
			encoded := false
			for tr, ids := range want {
				g := got[tr]
				require.Len(t, g, len(ids), "track %d", tr)
				video := len(ids) > 0 && slices.ContainsFunc(a, func(f media.Frame) bool {
					return f.Track == tr && f.Kind() == media.KindVideo
				})
				if !video || r.Video.Copy {
					assert.Equal(t, ids, g, "track %d", tr)
					continue
				}
				encoded = true
				same := 0
				for j := range ids {
					w, x := ids[j], g[j]
					if w.data == x.data {
						same++
					}
					w.data, x.data = "", ""
					require.Equal(t, w, x, "track %d frame %d: PTS, DTS, Key, Epoch, config", tr, j)
				}
				t.Logf("track %d: %d of %d frames byte-identical; config %s", tr, same, len(ids), ids[0].config)
				// x264's settings, the preset among them, as its first IDR
				// states them.
				wo, ok := x264Options(videoFrames(a)[0])
				require.True(t, ok, "track %d: the first IDR has x264's options SEI", tr)
				go2, ok := x264Options(videoFrames(b)[0])
				require.True(t, ok, "track %d: the first IDR has x264's options SEI", tr)
				assert.Equal(t, wo, go2, "track %d: x264's options", tr)
				t.Logf("track %d: %s", tr, wo)
			}
			if !encoded {
				return
			}
			pa, pb := decodePictures(t, ffmpeg, videoFrames(a), 0), decodePictures(t, ffmpeg, videoFrames(b), 0)
			require.Len(t, pb, len(pa))
			require.Len(t, pa, len(videoFrames(a)), "every picture decoded")
			worst := 0.0
			for j := range pa {
				require.Equal(t, pa[j].Bounds(), pb[j].Bounds(), "picture %d", j)
				d := meanAbsDiff(pa[j], pb[j], pa[j].Bounds())
				worst = max(worst, d)
				assert.Less(t, d, m4cNoise, "picture %d", j)
			}
			t.Logf("%d pictures, worst mean difference %.2f", len(pa), worst)
		})
	}
}
