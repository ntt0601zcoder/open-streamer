package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/server"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

// The M5b1 scenarios prove R-E10 on the whole server, over its API: an
// input replaced under a running stream (passthrough, and a ladder with the
// real child), the asset lifecycle with a watermark refreshed while the
// stream runs, and no_fallback. They are on M5a's harness and M4c's picture
// helpers.
//
// They count epochs, discontinuities and encoder opens, so their sources
// make none of their own: every stream reads an m5b1Clip, one paced pass of
// a file without a loop, and every scenario ends with a check that its hubs
// hold the epochs its own edits account for and no other.

const (
	// m5b1Passes is how many times an m5b1Clip repeats h264_aac.ts: 120 s,
	// longer than any scenario.
	m5b1Passes = 20
	// m5b1ClipLimit is how far into a clip a scenario may run: past it the
	// source would end under the test, which is a new epoch.
	m5b1ClipLimit = 100 * time.Second
	// m5b1Hold is how long an epoch count is watched after it was reached,
	// to see that it stays.
	m5b1Hold = 3 * time.Second
	// A pixel is lit by a text watermark, whose glyphs are white in a black
	// border, when every channel is at least m5b1Lit and its row has a
	// pixel with no channel above m5b1Dark within m5b1Stroke pixels on
	// either side. The fixture's picture has bright pixels of its own
	// (where two colour bars meet, in its squares), but none between two
	// dark ones: no pixel of its 150 pictures is lit by this rule at
	// 240x180 or at 160x120.
	m5b1Lit    = 160
	m5b1Dark   = 90
	m5b1Stroke = 4
)

// m5b1Clip writes h264_aac.ts m5b1Passes times back to back into a file of
// its own in the test's temporary directory, with ffmpeg's concat demuxer
// and stream copy as liveClip does (continuous timestamps, every keyframe
// kept), and returns it as a file input without a loop: one paced pass,
// one epoch. Two calls are the same media under two paths, so two URL
// texts. The test fails if it is still running m5b1ClipLimit after the call.
func m5b1Clip(t *testing.T, ffmpeg string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	dir := t.TempDir()
	var list strings.Builder
	for range m5b1Passes {
		fmt.Fprintf(&list, "file '%s'\n", src)
	}
	listPath := filepath.Join(dir, "list.txt")
	require.NoError(t, os.WriteFile(listPath, []byte(list.String()), 0o600))
	out := filepath.Join(dir, "clip.ts")
	b, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", "-f", "mpegts", out).CombinedOutput()
	require.NoError(t, err, string(b))
	written := time.Now()
	t.Cleanup(func() {
		if d := time.Since(written); d > m5b1ClipLimit {
			t.Errorf("the test ran %v past the writing of %s, a clip of %d s: its source may have ended under it, "+
				"so its epoch counts prove nothing", d.Round(time.Second), out, 6*m5b1Passes)
		}
	})
	return fileURL(out, "")
}

// m5b1Server starts a server with the transcoder bin, an assets directory
// next to its config file when assets is set, and opt.
func m5b1Server(t *testing.T, bin string, assets bool, opt server.Options) (m *m5a, assetsDir string) {
	t.Helper()
	dir := t.TempDir()
	text := m5aConfig("", bin)
	if assets {
		assetsDir = filepath.Join(dir, "assets")
		require.NoError(t, os.Mkdir(assetsDir, 0o700))
		const before = "  log: {level: info, format: json}\n"
		require.Contains(t, text, before)
		text = strings.Replace(text, before, "  assets_dir: \""+assetsDir+"\"\n"+before, 1)
	}
	path := filepath.Join(dir, "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return m5aStartWith(t, path, opt), assetsDir
}

// m5b1Frames are the frames a tap has recorded so far.
func m5b1Frames(r *hubRecorder) []media.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return stepFrames(r.steps)
}

// m5b1Video is how many video frames a tap has recorded so far.
func m5b1Video(r *hubRecorder) int {
	return len(videoFrames(m5b1Frames(r)))
}

// m5b1Epochs are the epochs of the frames a tap has recorded so far, in
// the order they came.
func m5b1Epochs(r *hubRecorder) []uint32 {
	return epochs(m5b1Frames(r))
}

// m5b1EpochsStay requires that the tap holds frames of exactly n epochs
// within d, and still exactly n after m5b1Hold more.
func m5b1EpochsStay(t *testing.T, r *hubRecorder, n int, d time.Duration, what string) {
	t.Helper()
	var got []uint32
	m5aUntil(t, d, func() bool {
		got = m5b1Epochs(r)
		return len(got) >= n
	}, "%s: frames of %d epochs; the tap has %v", what, n, &got)
	require.Len(t, m5b1Epochs(r), n, "%s: epochs", what)
	time.Sleep(m5b1Hold)
	require.Len(t, m5b1Epochs(r), n, "%s: epochs, %v later", what, m5b1Hold)
}

// m5b1Put is a stream PUT that must answer 200 and the given applied.
func m5b1Put(t *testing.T, m *m5a, sp spec.Stream, applied string) {
	t.Helper()
	code, got := m.put(t, sp)
	require.Equal(t, http.StatusOK, code, "PUT %s", sp.Name)
	require.Equal(t, applied, got, "PUT %s: applied", sp.Name)
}

// m5b1Create is a stream PUT that must create the stream.
func m5b1Create(t *testing.T, m *m5a, sp spec.Stream) {
	t.Helper()
	code, got := m.put(t, sp)
	require.Equal(t, http.StatusCreated, code, "PUT %s", sp.Name)
	require.Equal(t, "created", got, "PUT %s: applied", sp.Name)
}

// m5b1Ended are the openstreamer_output_sessions_ended_total series that
// are not zero, by their labels.
func m5b1Ended(t *testing.T, m *m5a) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range m.scrape(t) {
		if s.Name != "openstreamer_output_sessions_ended_total" || s.Value == 0 {
			continue
		}
		var key []string
		for _, k := range slices.Sorted(maps.Keys(s.Labels)) {
			key = append(key, k+"="+s.Labels[k])
		}
		out[strings.Join(key, ",")] = s.Value
	}
	return out
}

// m5b1Playlist polls a media playlist every 200 ms, from its start to end.
type m5b1Playlist struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	polls int
	errs  []string
	msn   []uint64        // the media sequence of every poll
	discs map[uint64]bool // the media sequence numbers of the segments that carry EXT-X-DISCONTINUITY
	gone  uint64          // the last EXT-X-DISCONTINUITY-SEQUENCE: the discontinuities that left the window
}

func m5b1WatchPlaylist(u string) *m5b1Playlist {
	ctx, cancel := context.WithCancel(context.Background())
	w := &m5b1Playlist{cancel: cancel, done: make(chan struct{}), discs: map[uint64]bool{}}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			w.poll(ctx, u)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

func (w *m5b1Playlist) poll(ctx context.Context, u string) {
	pl, err := m5b1ReadPlaylist(ctx, u)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			w.errs = append(w.errs, err.Error())
		}
		return
	}
	w.polls++
	w.msn = append(w.msn, pl.MediaSequence)
	w.gone = pl.DiscontinuitySequence
	for i, s := range pl.Segments {
		if s.Discontinuity {
			w.discs[pl.MediaSequence+uint64(i)] = true //nolint:gosec // an index
		}
	}
}

// m5b1ReadPlaylist fetches and parses a media playlist.
func m5b1ReadPlaylist(ctx context.Context, u string) (hls.MediaPlaylist, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return hls.MediaPlaylist{}, err
	}
	res, err := m5aClient.Do(req)
	if err != nil {
		return hls.MediaPlaylist{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return hls.MediaPlaylist{}, err
	}
	if res.StatusCode != http.StatusOK {
		return hls.MediaPlaylist{}, fmt.Errorf("GET %s: status %d", u, res.StatusCode)
	}
	return hls.ParseMedia(body)
}

// slid reports whether the window has slid since the first poll: the media
// sequence of the last poll is beyond the first one's.
func (w *m5b1Playlist) slid() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.msn) > 0 && w.msn[len(w.msn)-1] > w.msn[0]
}

// end stops the polling.
func (w *m5b1Playlist) end() {
	w.cancel()
	<-w.done
}

// check requires that every poll answered, that the media sequence went on
// and never back, and that the polls saw n discontinuities in all, none of
// which left the window unseen.
func (w *m5b1Playlist) check(t *testing.T, n int) {
	t.Helper()
	assert.Empty(t, w.errs, "polls that failed, beside %d that answered", w.polls)
	require.Greater(t, w.polls, 10, "polls")
	for i := 1; i < len(w.msn); i++ {
		require.GreaterOrEqual(t, w.msn[i], w.msn[i-1], "the media sequence at poll %d", i)
	}
	assert.Greater(t, w.msn[len(w.msn)-1], w.msn[0], "the media sequence went on")
	assert.Len(t, w.discs, n, "the segments with EXT-X-DISCONTINUITY that the polls saw: %v", slices.Sorted(maps.Keys(w.discs)))
	assert.LessOrEqual(t, w.gone, uint64(n), "EXT-X-DISCONTINUITY-SEQUENCE at the last poll") //nolint:gosec // a small count
}

// TestM5b1ReplaceInputs is clause 1 of R-E10 on a passthrough stream: cam,
// every output on, reads clip A with a reader on every session output, the
// packager's samplers and a tap. An edit that replaces its inputs with
// clip B is applied in place: the hub goes to its second epoch and stays
// there, the generation is the same, no session ended and every session
// reader holds every hub frame once across the epoch change; the HLS
// playlist went on with exactly one discontinuity. A second edit adds A
// below the active input and changes nothing a reader sees.
func TestM5b1ReplaceInputs(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	a, b := m5b1Clip(t, ffmpeg), m5b1Clip(t, ffmpeg)
	require.NotEqual(t, a, b, "two clips, two URLs")
	m := m5aServer(t, "")
	cam := m5aSpec("cam", a)
	m5b1Create(t, m, cam)

	// No segmented reader: m5aAssertSegmented needs a whole inner epoch of
	// a looping source. The playlist poller and the two samplers hold HLS
	// and DASH instead.
	r := m5aOpen(t, m, "cam", m5aRowOpt{segmented: []string{}, watch: stream.OutHLS})
	dash := m.watchPackager(stream.OutDASH, "cam")
	t.Cleanup(dash.end)
	playlist := m5b1WatchPlaylist(m.url(stream.OutHLS, "cam"))
	t.Cleanup(playlist.end)
	tap := r.taps[0]
	before := m.status(t, "cam")
	endedBefore := m5b1Ended(t, m)

	// The guard: the source made no epoch of its own.
	time.Sleep(3 * time.Second)
	require.Len(t, m5b1Epochs(tap), 1, "epochs before the edit")

	cam.Inputs = []spec.Input{{URL: b}}
	m5b1Put(t, m, cam, "live")
	r.edited()
	st := m.status(t, "cam")
	assert.Equal(t, before.Generation, st.Generation, "the generation")
	assert.True(t, before.Since.Equal(st.Since), "since: %v, then %v", before.Since, st.Since)
	assert.Zero(t, st.Rebuilds, "rebuilds")
	m5b1EpochsStay(t, tap, 2, 10*time.Second, "after the inputs were replaced")
	st = m.running(t, "cam")
	assert.EqualValues(t, 1, st.Switches, "input_switches")
	assert.Equal(t, 0, st.ActiveInput)
	text := m.fileText(t)
	assert.Contains(t, text, b, "the config file holds B")
	assert.NotContains(t, text, a, "the config file no longer holds A")

	// An input added below the active one.
	cam.Inputs = []spec.Input{{URL: b}, {URL: a}}
	m5b1Put(t, m, cam, "live")
	r.edited()
	time.Sleep(3 * time.Second)
	require.Len(t, m5b1Epochs(tap), 2, "epochs after an input was added below the active one")
	st = m.status(t, "cam")
	assert.Equal(t, before.Generation, st.Generation, "the generation")
	assert.True(t, before.Since.Equal(st.Since), "since: %v, then %v", before.Since, st.Since)
	assert.Zero(t, st.Rebuilds+st.Restarts, "rebuilds and restarts")
	assert.EqualValues(t, 1, st.Switches, "input_switches")
	assert.Len(t, st.Inputs, 2)

	// The media sequence goes on once the window is full.
	m5aUntil(t, m5aWait, playlist.slid, "the HLS window slides")

	// The one counter the first edit moves itself: the switch to B.
	r.base["input_switches"] = 1
	playlist.end()
	dash.end()
	// Read last, just before the row ends the stream: it covers the whole
	// run.
	assert.Equal(t, endedBefore, m5b1Ended(t, m), "no openstreamer_output_sessions_ended_total series moved")
	r.end(t)
	assertUninterrupted(t, r)
	t.Run("dash packager", dash.check)
	playlist.check(t, 1)
	assert.Len(t, epochs(r.hubs[0]), 2, "the hub's epochs from the tap's start to the stream's end: the one edit that changed the source")
}

// m5b1Ladder is cam reading input through a ladder of x264 rungs of the
// given heights, labelled by them, with the audio copied and HTTP-TS on.
func m5b1Ladder(input string, heights ...int) spec.Stream {
	sp := spec.Stream{
		Name:      "cam",
		Inputs:    []spec.Input{{URL: input}},
		Transcode: &spec.Transcode{Audio: spec.Audio{Copy: true}},
		Outputs:   spec.Outputs{HTTPTS: true},
	}
	for _, h := range heights {
		sp.Transcode.Renditions = append(sp.Transcode.Renditions,
			spec.Rendition{Label: fmt.Sprint(h), Height: h, BitrateKbps: 2 * h, Encoder: "x264"})
	}
	return sp
}

// m5b1Child requires a status with a running transcoder and returns its
// pid and spawns.
func m5b1Child(t *testing.T, st stream.Status) (pid int, spawns uint64) {
	t.Helper()
	require.NotNil(t, st.Transcoder, "the status has a transcoder")
	require.NotZero(t, st.Transcoder.PID, "the transcoder's pid")
	return st.Transcoder.PID, st.Transcoder.Spawns
}

// TestM5b1ReplaceInputsLadder is invariant 3 under the real child: cam
// reads clip A through one x264 rung. Replacing its inputs with clip B is
// applied in place and the child is the same process: the rendition's hub
// goes to its second epoch and stays there, and an HTTP-TS reader of the
// rendition that was reading before the edit reads on across it, without a
// violation.
func TestM5b1ReplaceInputsLadder(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	a, b := m5b1Clip(t, ffmpeg), m5b1Clip(t, ffmpeg)
	m, _ := m5b1Server(t, bin, false, server.Options{})
	cam := m5b1Ladder(a, 180)
	m5b1Create(t, m, cam)
	m.running(t, "cam")
	tap := m.tap(t, "cam_180")
	m5aUntil(t, m5aWait, func() bool { return m5b1Video(tap) > 0 }, "cam_180 has video")
	reader := rawHTTP(m.url(stream.OutHTTPTS, "cam_180"))
	subscribers := func() float64 { return m.metric(t, "openstreamer_hub_subscribers", "name", "cam_180") }
	m5aUntil(t, m5aWait, func() bool { return subscribers() == 2 }, "the tap and the reader are on cam_180's hub")
	time.Sleep(2 * time.Second)
	require.Len(t, m5b1Epochs(tap), 1, "epochs before the edit")
	before := m.status(t, "cam")
	pid, spawns := m5b1Child(t, before)

	cam.Inputs = []spec.Input{{URL: b}}
	m5b1Put(t, m, cam, "live")
	edited := time.Now()
	m5b1EpochsStay(t, tap, 2, 15*time.Second, "after the inputs were replaced")
	m5aSleepUntil(edited.Add(3 * time.Second))
	select {
	case res := <-reader:
		require.Fail(t, "the HTTP-TS reader of cam_180 ended", "%d bytes, error %v", len(res.b), res.err)
	default:
	}
	assert.EqualValues(t, 2, subscribers(), "the reader is still on the hub")
	st := m.running(t, "cam")
	nowPID, nowSpawns := m5b1Child(t, st)
	assert.Equal(t, pid, nowPID, "the child is the same process")
	assert.Equal(t, spawns, nowSpawns, "spawns")
	assert.Zero(t, st.Transcoder.Exits, "exits")
	assert.Equal(t, before.Generation, st.Generation, "the generation")
	assert.Zero(t, st.Rebuilds+st.Restarts, "rebuilds and restarts")
	assert.EqualValues(t, 1, st.Switches, "input_switches")

	m.del(t, "cam")
	got := waitRawResult(t, reader)
	c := conformBytes(t, got)
	assert.Empty(t, c.Violations, "the capture of cam_180's HTTP-TS")
	// TS never signals a hub epoch, for any stream: the hub's stream time
	// is continuous across one, and the output goes on in it. So the
	// capture is one run, whose largest video DTS step, the one at the
	// epoch change included, is under 1 s; it is held to the hub's two
	// epochs frame for frame below.
	assert.Zero(t, tsGaps(t, got), "resumes after loss")
	captured, _ := byKind(tsFrames(t, got), media.KindVideo)
	require.NotEmpty(t, captured, "video in the capture")
	var step int64
	for i := 1; i < len(captured); i++ {
		step = max(step, captured[i].DTS-captured[i-1].DTS)
	}
	t.Logf("the capture's largest video DTS step: %d ticks over %d frames", step, len(captured))
	assert.Less(t, step, int64(media.ClockRate), "the capture's largest video DTS step: continuous across the epoch change")
	select {
	case <-tap.done:
	case <-time.After(m5aWait):
		require.Fail(t, "the tap did not end with the stream")
	}
	frames := stepFrames(tap.wait(t, "the tap of cam_180"))
	assert.Len(t, epochs(frames), 2, "the rendition hub's epochs from the tap's start to the stream's end")
	assertTSMatchesHub(t, frames, tsFrames(t, got))
}

// m5b1PNG is a 64×64 opaque PNG of one colour, as magentaPNG is.
func m5b1PNG(c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err) // an in-memory encode of a valid image does not fail
	}
	return b.Bytes()
}

// m5b1LitWidth is the width of the bounding box of the pixels a text
// watermark lit (see m5b1Lit) in the upper 30 % of img, which a text of
// 20 % at the top left lies in, over img's whole width; 0 when none is lit.
func m5b1LitWidth(img image.Image) int {
	bd := img.Bounds()
	at := func(x, y int) color.NRGBA { return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA) }
	dark := func(x, y int) bool {
		if x < bd.Min.X || x >= bd.Max.X {
			return false
		}
		c := at(x, y)
		return max(c.R, c.G, c.B) <= m5b1Dark
	}
	lo, hi := bd.Max.X, bd.Min.X-1
	for y := bd.Min.Y; y < bd.Min.Y+bd.Dy()*30/100; y++ {
		for x := bd.Min.X; x < bd.Max.X; x++ {
			if c := at(x, y); min(c.R, c.G, c.B) < m5b1Lit {
				continue
			}
			var left, right bool
			for k := 1; k <= m5b1Stroke; k++ {
				left, right = left || dark(x-k, y), right || dark(x+k, y)
			}
			if left && right {
				lo, hi = min(lo, x), max(hi, x)
			}
		}
	}
	if hi < lo {
		return 0
	}
	return hi - lo + 1
}

// m5b1Rung is one rung of TestM5b1Assets: its tap, and the last GOP whose
// first picture was decoded.
type m5b1Rung struct {
	name   string
	tap    *hubRecorder
	ffmpeg string
	seen   int64 // the PTS of the last keyframe decoded, -1 before the first
	img    image.Image
}

// picture decodes the first picture of the rung's latest GOP; fresh is
// false when it is the GOP of the call before, whose picture is returned
// again.
func (r *m5b1Rung) picture(t *testing.T) (img image.Image, fresh bool) {
	t.Helper()
	video := videoFrames(m5b1Frames(r.tap))
	last := len(video) - 1
	for last >= 0 && !video[last].Key {
		last--
	}
	if last < 0 {
		return nil, false
	}
	if r.img != nil && video[last].PTS == r.seen {
		return r.img, false
	}
	r.seen, r.img = video[last].PTS, firstPicture(t, r.ffmpeg, video[last:])
	return r.img, true
}

// until waits, for at most d, for a GOP of the rung whose first picture
// satisfies ok, and returns the picture.
func (r *m5b1Rung) until(t *testing.T, d time.Duration, ok func(image.Image) bool, format string, args ...any) image.Image {
	t.Helper()
	var img image.Image
	m5aUntil(t, d, func() bool {
		var fresh bool
		img, fresh = r.picture(t)
		if img == nil {
			return false
		}
		if !ok(img) {
			if fresh {
				cr, cg, cb := markRegion(img, picture.TopLeft, m4cMarkPct)
				t.Logf("%s: the GOP at PTS %d: mark region %.0f,%.0f,%.0f, lit width %d", r.name, r.seen, cr, cg, cb, m5b1LitWidth(img))
			}
			return false
		}
		return true
	}, "%s: "+format, append([]any{r.name}, args...)...)
	return img
}

// m5b1Near reports whether a mark region's colour is within m4cMarkTol of
// want on every channel.
func m5b1Near(img image.Image, want [3]float64) bool {
	r, g, b := markRegion(img, picture.TopLeft, m4cMarkPct)
	return math.Abs(r-want[0]) <= m4cMarkTol && math.Abs(g-want[1]) <= m4cMarkTol && math.Abs(b-want[2]) <= m4cMarkTol
}

// m5b1Asset is an asset request as the editor, or as who m is.
func m5b1Asset(t *testing.T, m *m5a, method, name string, body []byte) m5aResp {
	t.Helper()
	var hdr []string
	if method == http.MethodPut {
		hdr = []string{"Content-Type", "application/octet-stream"}
	}
	return m.do(t, method, "/api/v1/assets/"+name, body, hdr...)
}

// m5b1AssetPut decodes the answer to an upload, whose three lists must be
// there: [] when empty, never null.
func m5b1AssetPut(t *testing.T, r m5aResp) api.AssetPut {
	t.Helper()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(r.body, &raw), "%s", r.body)
	for _, k := range []string{"used_by", "refreshed", "retried"} {
		require.Contains(t, raw, k, "%s", r.body)
		assert.True(t, bytes.HasPrefix(raw[k], []byte("[")), "%s is a list: %s", k, raw[k])
	}
	var res api.AssetPut
	require.NoError(t, json.Unmarshal(r.body, &res), "%s", r.body)
	return res
}

// m5b1Error decodes an error document.
func m5b1Error(t *testing.T, r m5aResp) (code string, errs []string) {
	t.Helper()
	var doc struct {
		Error struct {
			Code   string   `json:"code"`
			Errors []string `json:"errors"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.body, &doc), "%s", r.body)
	return doc.Error.Code, doc.Error.Errors
}

// TestM5b1Assets is clause 2 of R-E10 and the live watermark edit, on one
// server with an assets directory and the real child: cam reads a clip
// through two x264 rungs with a text watermark. An image is uploaded, named
// by the watermark, replaced (which redraws it on the running stream with
// no stream PUT), refused a delete while it is named, and deleted once it
// is not; a font asset widens a text. Every picture is checked on both
// rungs, and from start to end the child, the generation and the epoch of
// both rendition hubs are the same.
func TestM5b1Assets(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	font, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "fonts", "DejaVuSansMono.ttf"))
	require.NoError(t, err)
	m, assetsDir := m5b1Server(t, bin, true, server.Options{})
	viewer := m.as(m5aViewer, m5aViewerPW)
	cam := m5b1Ladder(m5b1Clip(t, ffmpeg), 180, 120)
	cam.Transcode.Watermark = &spec.Watermark{Text: "M5B1"}
	m5b1Create(t, m, cam)
	first := m.running(t, "cam")
	rungs := []*m5b1Rung{
		{name: "cam_180", tap: m.tap(t, "cam_180"), ffmpeg: ffmpeg, seen: -1},
		{name: "cam_120", tap: m.tap(t, "cam_120"), ffmpeg: ffmpeg, seen: -1},
	}
	var pid int
	var spawns uint64
	// same requires the child, the generation and the rendition hubs'
	// epochs that the scenario started with.
	same := func(step string) {
		t.Helper()
		st := m.status(t, "cam")
		nowPID, nowSpawns := m5b1Child(t, st)
		require.Equal(t, pid, nowPID, "%s: the child is the same process", step)
		require.Equal(t, spawns, nowSpawns, "%s: spawns", step)
		require.Equal(t, first.Generation, st.Generation, "%s: the generation", step)
		require.Zero(t, st.Rebuilds+st.Restarts, "%s: rebuilds and restarts", step)
		require.Empty(t, st.LastError, "%s: last_error", step)
		for _, r := range rungs {
			require.Len(t, m5b1Epochs(r.tap), 1, "%s: %s: epochs", step, r.name)
		}
	}
	// each waits, for at most d in all, for a GOP of every rung whose
	// first picture satisfies ok, and returns the pictures.
	each := func(d time.Duration, ok func(i int, img image.Image) bool, format string, args ...any) []image.Image {
		t.Helper()
		by := time.Now().Add(d)
		out := make([]image.Image, len(rungs))
		for i, r := range rungs {
			out[i] = r.until(t, max(time.Until(by), 0), func(img image.Image) bool { return ok(i, img) }, format, args...)
		}
		return out
	}

	// 1. The plain text, in the embedded font.
	dejavu := each(m5aWait, func(_ int, img image.Image) bool { return m5b1LitWidth(img) > 0 }, "the text M5B1 is drawn")
	pid, spawns = m5b1Child(t, m.status(t, "cam"))
	for i, r := range rungs {
		ok, c := marked(dejavu[i], picture.TopLeft)
		require.False(t, ok, "%s: no image mark yet: %v", r.name, c)
		// Four glyphs of the default 5 %: narrower than eight i's of 20 %
		// in either font, which step 6 measures.
		t.Logf("%s: lit width of M5B1 in the embedded font: %d", r.name, m5b1LitWidth(dejavu[i]))
	}
	same("1")

	// 2. The upload: an editor's, and a viewer may read it.
	res := m5b1Asset(t, viewer, http.MethodPut, "logo.png", magentaPNG())
	require.Equal(t, http.StatusForbidden, res.code, "a viewer's upload: %s", res.body)
	res = m5b1Asset(t, m, http.MethodPut, "logo.png", magentaPNG())
	require.Equal(t, http.StatusCreated, res.code, "the editor's upload: %s", res.body)
	put := m5b1AssetPut(t, res)
	assert.Equal(t, "logo.png", put.Name)
	assert.Equal(t, "image", put.Kind)
	assert.Empty(t, put.UsedBy, "used_by")
	assert.Empty(t, put.Refreshed, "refreshed")
	assert.Empty(t, put.Retried, "retried")
	res = m5b1Asset(t, viewer, http.MethodGet, "logo.png", nil)
	require.Equal(t, http.StatusOK, res.code, "a viewer's download")
	assert.Equal(t, magentaPNG(), res.body, "the bytes")
	same("2")

	// 3. The watermark names the image.
	cam.Transcode.Watermark = &spec.Watermark{Image: "logo.png", SizePct: m4cMarkPct}
	m5b1Put(t, m, cam, "live")
	each(10*time.Second, func(_ int, img image.Image) bool { ok, _ := marked(img, picture.TopLeft); return ok },
		"the magenta mark is drawn within 10 s of the edit")
	same("3")

	// 4. New bytes under the same name: the running stream redraws, with
	// no stream PUT.
	res = m5b1Asset(t, m, http.MethodPut, "logo.png", m5b1PNG(color.NRGBA{0, 255, 0, 255}))
	require.Equal(t, http.StatusOK, res.code, "the second upload: %s", res.body)
	put = m5b1AssetPut(t, res)
	assert.Equal(t, []string{"cam"}, put.UsedBy, "used_by")
	assert.Equal(t, []stream.Refreshed{{Stream: "cam", OK: true}}, put.Refreshed, "refreshed")
	assert.Empty(t, put.Retried, "retried")
	each(10*time.Second, func(_ int, img image.Image) bool { return m5b1Near(img, [3]float64{0, 255, 0}) },
		"the mark is green within 10 s of the upload")
	same("4")

	// 5. An asset a stream names is not deleted.
	res = m5b1Asset(t, m, http.MethodDelete, "logo.png", nil)
	require.Equal(t, http.StatusConflict, res.code, "DELETE of an asset in use: %s", res.body)
	code, errs := m5b1Error(t, res)
	assert.Equal(t, "asset_in_use", code)
	assert.Equal(t, []string{"cam"}, errs)
	status, body := viewer.get(t, "/api/v1/assets")
	require.Equal(t, http.StatusOK, status, "%s", body)
	var list struct {
		Assets []api.Asset `json:"assets"`
	}
	require.NoError(t, json.Unmarshal(body, &list), "%s", body)
	require.Len(t, list.Assets, 1, "%s", body)
	assert.Equal(t, "logo.png", list.Assets[0].Name)
	assert.Equal(t, []string{"cam"}, list.Assets[0].UsedBy)
	same("5")

	// 6. A font asset: the transcoder probed it, and eight i's are wider
	// in a monospaced font.
	res = m5b1Asset(t, m, http.MethodPut, "mono.ttf", font)
	require.Equal(t, http.StatusCreated, res.code, "the font's upload: %s", res.body)
	assert.Equal(t, "font", m5b1AssetPut(t, res).Kind)
	cam.Transcode.Watermark = &spec.Watermark{Text: "iiiiiiii", Font: "mono.ttf", SizePct: 20}
	m5b1Put(t, m, cam, "live")
	// The first picture that has a text and no green mark is the mono one:
	// the watermark changes between two pictures.
	mono := each(10*time.Second, func(_ int, img image.Image) bool {
		return !m5b1Near(img, [3]float64{0, 255, 0}) && m5b1LitWidth(img) > 0
	}, "the text is drawn in the font within 10 s of the edit")
	same("6: the font")
	cam.Transcode.Watermark = &spec.Watermark{Text: "iiiiiiii", SizePct: 20}
	m5b1Put(t, m, cam, "live")
	sans := each(10*time.Second, func(i int, img image.Image) bool {
		w := m5b1LitWidth(img)
		return w > 0 && float64(m5b1LitWidth(mono[i])) >= 1.5*float64(w)
	}, "the text with the font is at least 1.5 times as wide as without it, within 10 s of the edit")
	for i, r := range rungs {
		t.Logf("%s: lit width: %d with mono.ttf, %d with the embedded font", r.name, m5b1LitWidth(mono[i]), m5b1LitWidth(sans[i]))
	}
	same("6: the embedded font")

	// 7. No stream names them now.
	for _, name := range []string{"logo.png", "mono.ttf"} {
		res = m5b1Asset(t, m, http.MethodDelete, name, nil)
		require.Equal(t, http.StatusNoContent, res.code, "DELETE %s: %s", name, res.body)
	}

	// 8. What is not an asset.
	res = m5b1Asset(t, m, http.MethodPut, "evil.png", []byte("<!DOCTYPE html><html><script>alert(1)</script></html>"))
	require.Equal(t, http.StatusUnprocessableEntity, res.code, "an HTML body as a PNG: %s", res.body)
	code, _ = m5b1Error(t, res)
	assert.Equal(t, "invalid_asset", code)
	res = m5b1Asset(t, m, http.MethodPut, "..%2Fx.png", magentaPNG())
	require.Equal(t, http.StatusNotFound, res.code, "a name with a path: %s", res.body)
	assert.Empty(t, m5b1Names(t, assetsDir), "the assets directory")
	assert.Equal(t, []string{"assets", "openstreamer.yaml"}, m5b1Names(t, filepath.Dir(assetsDir)), "the assets directory's parent")
	status, body = m.get(t, "/api/v1/assets")
	require.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `{"assets":[]}`, string(body))
	same("8")

	m.del(t, "cam")
	for _, r := range rungs {
		select {
		case <-r.tap.done:
		case <-time.After(m5aWait):
			require.Fail(t, "a tap did not end with the stream", r.name)
		}
		assert.Len(t, epochs(stepFrames(r.tap.wait(t, "the tap of "+r.name))), 1,
			"%s: the hub's epochs from the tap's start to the stream's end: no edit here changes the epoch", r.name)
	}
}

// m5b1Names are the names of a directory's entries, sorted.
func m5b1Names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestM5b1NoFallback is clause 3 of R-E10: cam has an NVENC rung with
// no_fallback and a second, software rung, on a server whose children
// cannot open NVENC. The ladder is refused as a whole and tried again: the
// refusals are counted and said, nothing falls back, neither rung delivers
// a video frame, and the source and a second stream of the server run on.
// Without no_fallback the stream is rebuilt, the NVENC rung falls back to
// x264 and both rungs deliver.
//
// NVENC is hidden from the children with CUDA_VISIBLE_DEVICES= in
// server.Options.TranscoderEnv (ruling F5, verified in Task 5): where there
// is no device it changes nothing, and on a GPU host no NVENC session is
// opened.
func TestM5b1NoFallback(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	m, _ := m5b1Server(t, bin, false, server.Options{TranscoderEnv: []string{"CUDA_VISIBLE_DEVICES="}})
	cam := spec.Stream{
		Name:   "cam",
		Inputs: []spec.Input{{URL: m5b1Clip(t, ffmpeg)}},
		Transcode: &spec.Transcode{
			Renditions: []spec.Rendition{
				{Label: "hi", Height: 180, BitrateKbps: 400, Encoder: "nvenc", NoFallback: true},
				{Label: "lo", Height: 120, BitrateKbps: 200},
			},
			Audio: spec.Audio{Copy: true},
		},
		Outputs: spec.Outputs{HTTPTS: true},
	}
	other := spec.Stream{Name: "other", Inputs: []spec.Input{{URL: m5b1Clip(t, ffmpeg)}}, Outputs: spec.Outputs{HTTPTS: true}}
	m5b1Create(t, m, cam)
	m5b1Create(t, m, other)
	m.running(t, "cam")
	source := m.tap(t, "cam")
	served := []string{"cam_hi", "cam_lo"}
	taps := map[string]*hubRecorder{}
	for _, name := range served {
		taps[name] = m.tap(t, name)
	}

	const (
		refusals  = "openstreamer_transcoder_encoder_refusals_total"
		fallbacks = "openstreamer_transcoder_encoder_fallbacks_total"
	)
	var st stream.Status
	m5aUntil(t, 20*time.Second, func() bool {
		n := m.metric(t, refusals, "stream", "cam")
		st = m.status(t, "cam")
		return n >= 2 && st.Transcoder != nil && float64(st.Transcoder.EncoderRefusals) == n &&
			m.metric(t, refusals, "stream", "cam") == n
	}, "at least 2 refusals, the same in the metric and the status")
	t.Logf("refused: %+v, last_error %q", *st.Transcoder, st.LastError)
	assert.Zero(t, m.metric(t, fallbacks, "stream", "cam"), "nothing fell back")
	assert.Zero(t, st.Transcoder.EncoderFallbacks, "the status' encoder_fallbacks")
	assert.True(t, strings.HasPrefix(st.LastError, "transcoder: "), "last_error: %q", st.LastError)
	assert.Contains(t, st.LastError, "no_fallback", "last_error")
	assert.Equal(t, "running", st.State, "the source is running")
	assert.EqualValues(t, 1, st.Generation, "the generation: a refusal does not rebuild the stream")
	for _, name := range served {
		assert.Zero(t, m5b1Video(taps[name]), "%s: video frames while the ladder is refused", name)
	}
	n := len(m5b1Frames(source))
	m5aUntil(t, 5*time.Second, func() bool { return len(m5b1Frames(source)) > n+25 }, "copy://cam delivers")
	m.running(t, "other")
	m.fresh(t, stream.OutHTTPTS, "other")
	require.Len(t, m5b1Epochs(source), 1, "the source hub's epochs while the ladder is refused")

	// Without no_fallback the rung falls back.
	cam.Transcode.Renditions[0].NoFallback = false
	m5b1Put(t, m, cam, "rebuild")
	select {
	case <-source.done:
	case <-time.After(m5aWait):
		require.Fail(t, "the tap of cam did not end with the generation")
	}
	assert.Len(t, epochs(stepFrames(source.wait(t, "the tap of cam"))), 1, "the first generation's source hub: epochs")
	for _, name := range served {
		select {
		case <-taps[name].done:
		case <-time.After(m5aWait):
			require.Fail(t, "a tap did not end with the generation", name)
		}
		assert.Zero(t, m5b1Video(taps[name]), "%s: video frames of the refused generation", name)
	}
	m.running(t, "cam")
	source = m.tap(t, "cam")
	for _, name := range served {
		taps[name] = m.tap(t, name)
	}
	m5aUntil(t, 20*time.Second, func() bool {
		for _, name := range served {
			if m5b1Video(taps[name]) == 0 {
				return false
			}
		}
		return m.metric(t, fallbacks, "stream", "cam") >= 1
	}, "both rungs deliver and the NVENC rung fell back")
	for _, name := range served {
		select {
		case <-taps[name].done:
			require.Fail(t, "a tap of the second generation ended", name)
		default:
		}
	}
	st = m.status(t, "cam")
	require.NotNil(t, st.Transcoder)
	t.Logf("rebuilt: %+v, last_error %q", *st.Transcoder, st.LastError)
	assert.EqualValues(t, 2, st.Generation)
	assert.EqualValues(t, 1, st.Rebuilds)
	assert.GreaterOrEqual(t, st.Transcoder.EncoderFallbacks, uint64(1), "the status' encoder_fallbacks")
	m.fresh(t, stream.OutHTTPTS, "cam_hi")
	m.fresh(t, stream.OutHTTPTS, "cam_lo")
	assert.Len(t, m5b1Epochs(source), 1, "the second generation's source hub: epochs")
	// A respawned child builds its encoders again, which is an epoch of
	// its own: the count is held only for a child that was not respawned.
	if st.Transcoder.Spawns == 1 && st.Transcoder.Exits == 0 {
		assert.Len(t, m5b1Epochs(taps["cam_hi"]), 1, "cam_hi: epochs, with a child that was not respawned")
	} else {
		t.Logf("cam_hi: epochs %v not held to 1: the child was respawned (%+v)", m5b1Epochs(taps["cam_hi"]), *st.Transcoder)
	}
}
