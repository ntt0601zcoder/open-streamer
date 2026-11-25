package e2e

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtspplay"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
)

// The M4a scenarios run the software ladder of the real transcoder child
// (cmd/ostranscoder, found by testpeer.OSTranscoder) under the M3
// scenarios. Each rendition is its own stream (ruling R-M4a): a transcoder
// run over a fixture is recorded per rendition hub as steps, which are then
// replayed into the M3 machinery like any source.

// envTranscodeArm is set to 1 by the arm64 CI job, which has no SRT or RTSP
// peers: the srt and rtsp rows skip there.
const envTranscodeArm = "OS_TRANSCODE_E2E_ARM"

// m4aLadder is the ladder of every M4a scenario: three encoded rungs and a
// copy of the source video, with the audio re-encoded to AAC at 96 kbps.
func m4aLadder(binary string) transcode.Options {
	return transcode.Options{
		Binary: binary,
		Renditions: []transcode.Rendition{
			{Name: "t240", Video: transcode.Video{Height: 240, BitrateKbps: 600}},
			{Name: "t180", Video: transcode.Video{Height: 180, BitrateKbps: 300}},
			{Name: "t120", Video: transcode.Video{Height: 120, BitrateKbps: 150}},
			{Name: "src", Video: transcode.Video{Copy: true}},
		},
		Audio: transcode.Audio{BitrateKbps: 96},
		// The subscription holds a whole fixture, so the transcoder itself
		// never loses frames.
		Queue: 2048,
	}
}

// m4aLogger logs the transcoder's warnings into the test's output.
func m4aLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// hubRecorder records a rendition hub's deliveries as steps: each epoch's
// anchor, as the hub holds it when the epoch's first frame arrives, then
// the frames in delivery order.
type hubRecorder struct {
	done chan struct{}

	mu      sync.Mutex
	steps   []step
	lost    uint64
	missing []uint32 // epochs without an anchor at their first frame
}

func recordHub(h *hub.Hub) *hubRecorder {
	r := &hubRecorder{done: make(chan struct{})}
	sub := h.Subscribe("record", bigQueue)
	go func() {
		defer close(r.done)
		seen := map[uint32]bool{}
		for dl := range sub.C() {
			s := step{f: dl.Frame}
			if e := dl.Frame.Epoch; !seen[e] {
				seen[e] = true
				if a, ok := h.Anchor(e); ok {
					s.anchors = []media.Anchor{a}
				} else {
					r.mu.Lock()
					r.missing = append(r.missing, e)
					r.mu.Unlock()
				}
			}
			r.mu.Lock()
			r.steps = append(r.steps, s)
			r.lost += dl.Lost
			r.mu.Unlock()
		}
	}()
	return r
}

// transcodeRun is the recording of one transcoder run.
type transcodeRun struct {
	renditions   [][]step // per rendition, in opt.Renditions order
	presentation []step   // the presentation hub's; nil without opt.Presentation
	stats        transcode.Stats
}

// recordTranscode runs one transcoder with opt over src, written into the
// source hub, and records every rendition hub's deliveries as steps, and
// the presentation hub's too with opt.Presentation. hook, when not nil, is
// called with the transcoder before it runs, so it can subscribe to its
// hubs. Every recording must be whole: no loss and an anchor before every
// epoch; the presentation's must also hold every frame its merger wrote.
// The final stats are returned unchecked.
func recordTranscode(t *testing.T, src feed, opt transcode.Options, hook func(tr *transcode.Transcoder)) transcodeRun {
	t.Helper()
	h := hub.New(hub.Options{})
	if opt.Logger == nil {
		opt.Logger = m4aLogger(t)
	}
	tr, err := transcode.New(h, opt)
	require.NoError(t, err)
	recs := make([]*hubRecorder, 0, len(opt.Renditions))
	for _, rh := range tr.Hubs() {
		recs = append(recs, recordHub(rh))
	}
	var pres *hubRecorder
	if ph := tr.Presentation(); ph != nil {
		pres = recordHub(ph)
	}
	if hook != nil {
		hook(tr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- tr.Run(ctx) }()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 10*time.Second, time.Millisecond,
		"the transcoder subscribed")
	ferr := src(ctx, h)
	h.Close()
	require.NoError(t, <-runErr)
	require.NoError(t, ferr)
	out := transcodeRun{renditions: make([][]step, len(recs))}
	for i, r := range recs {
		out.renditions[i] = r.wait(t, opt.Renditions[i].Name)
	}
	out.stats = tr.Stats()
	if pres != nil {
		out.presentation = pres.wait(t, "presentation")
		require.EqualValues(t, out.stats.PresentationOut, len(out.presentation),
			"the presentation's recording holds every frame its merger wrote")
	}
	return out
}

// wait waits for the recorded hub to close and returns its steps, which
// must be whole: no loss and an anchor before every epoch.
func (r *hubRecorder) wait(t *testing.T, name string) []step {
	t.Helper()
	<-r.done
	require.Zero(t, r.lost, "%s: lost frames", name)
	require.Empty(t, r.missing, "%s: epochs without an anchor", name)
	require.NotEmpty(t, r.steps, name)
	return r.steps
}

// requireCleanRun checks that a transcoder run was clean: one child, no
// exit, no loss, nothing dropped.
func requireCleanRun(t *testing.T, st transcode.Stats) {
	t.Helper()
	require.EqualValues(t, 1, st.Spawns, "%+v", st)
	require.Zero(t, st.Exits, "%+v", st)
	require.Zero(t, st.Losses, "%+v", st)
	require.Zero(t, st.DroppedBeforeKey, "%+v", st)
	require.Zero(t, st.MergeDropped, "%+v", st)
}

// transcodeSteps runs one transcoder with opt over src, written into the
// source hub, and returns every rendition hub's deliveries as steps, in
// opt.Renditions order, with the transcoder's final stats. The run must be
// clean: one child, no exit, no loss, nothing dropped, an anchor before
// every epoch.
func transcodeSteps(t *testing.T, src feed, opt transcode.Options) ([][]step, transcode.Stats) {
	t.Helper()
	run := recordTranscode(t, src, opt, nil)
	for i, steps := range run.renditions {
		requireVideoFirst(t, stepFrames(steps), opt.Renditions[i].Name)
	}
	requireCleanRun(t, run.stats)
	return run.renditions, run.stats
}

// requireVideoFirst checks that each epoch of a rendition hub that has
// video starts, in hub order, with its video keyframe: no audio is written
// ahead of the rung's IDR (the child's audio leads its video by the
// encoder's delay), so a subscriber that joins on the IDR, through the GOP
// cache, gets the epoch's audio from the start.
func requireVideoFirst(t *testing.T, frames []media.Frame, name string) {
	t.Helper()
	first := map[uint32]media.Frame{}
	video := map[uint32]bool{}
	for _, f := range frames {
		if _, ok := first[f.Epoch]; !ok {
			first[f.Epoch] = f
		}
		video[f.Epoch] = video[f.Epoch] || f.Kind() == media.KindVideo
	}
	for e, f := range first {
		if video[e] {
			require.True(t, f.Kind() == media.KindVideo && f.Key,
				"%s: epoch %d starts with a frame of kind %d, PTS %d, not the video keyframe", name, e, f.Kind(), f.PTS)
		}
	}
}

// transcodeFeed runs one transcoder over src, records every rendition hub's
// anchors and frames as steps, and returns one replayable feed per
// rendition.
func transcodeFeed(t *testing.T, src feed, opt transcode.Options) []feed {
	t.Helper()
	recorded, _ := transcodeSteps(t, src, opt)
	out := make([]feed, len(recorded))
	for i, steps := range recorded {
		out[i] = stepsFeed(steps)
	}
	return out
}

// stepFrames returns the frames of steps.
func stepFrames(steps []step) []media.Frame {
	out := make([]media.Frame, len(steps))
	for i, s := range steps {
		out[i] = s.f
	}
	return out
}

// passDelta is pacedFeed's shift between passes: the largest per-track
// (last PTS + frame duration) minus the first DTS, so that every track of
// a pass starts after every track of the one before has ended. A track's
// frame duration is the smallest step between its sorted PTS.
func passDelta(frames []media.Frame) int64 {
	pts := map[int][]int64{}
	first := frames[0].DTS
	for _, f := range frames {
		pts[f.Track] = append(pts[f.Track], f.PTS)
		first = min(first, f.DTS)
	}
	var end int64
	for _, p := range pts {
		slices.Sort(p)
		var dur int64
		for i := 1; i < len(p); i++ {
			if d := p[i] - p[i-1]; d > 0 && (dur == 0 || d < dur) {
				dur = d
			}
		}
		end = max(end, p[len(p)-1]+dur)
	}
	return end - first
}

// pacedFeed replays steps at wall-clock pace: each frame is written at its
// epoch anchor's Wall + (DTS − Start), and a loop clip is repeated n times
// with the epochs and anchors of each pass shifted, as loopClip does for
// inputs. Recorded anchor Walls are rebased to the replay start, and each
// pass is shifted by Δ = the largest per-track (last PTS + duration) − the
// first DTS. A clip with one epoch and one config stays in that epoch on
// every pass. Otherwise pass k shifts epoch numbers by k × (epochs per pass)
// and adds anchors shifted by k × Δ.
func pacedFeed(steps []step, n int) feed {
	frames := stepFrames(steps)
	delta := passDelta(frames)
	minE, maxE := frames[0].Epoch, frames[0].Epoch
	configs := map[int]*media.TrackConfig{}
	oneConfig := true
	for _, f := range frames {
		minE, maxE = min(minE, f.Epoch), max(maxE, f.Epoch)
		if c, ok := configs[f.Track]; ok && c != f.Config {
			oneConfig = false
		}
		configs[f.Track] = f.Config
	}
	var epochShift uint32
	if minE != maxE || !oneConfig {
		epochShift = maxE - minE + 1
	}
	return func(ctx context.Context, h *hub.Hub) error {
		start := time.Now()
		var rebase time.Duration
		if len(steps[0].anchors) > 0 {
			rebase = start.Sub(steps[0].anchors[0].Wall)
		}
		anchors := map[uint32]media.Anchor{}
		for k := range n {
			shift := int64(k) * delta
			for _, s := range steps {
				if k == 0 || epochShift > 0 {
					for _, a := range s.anchors {
						a.Epoch += uint32(k) * epochShift
						a.Start += shift
						a.Wall = a.Wall.Add(rebase + media.ToDuration(shift))
						h.SetAnchor(a)
						anchors[a.Epoch] = a
					}
				}
				f := s.f
				f.Epoch += uint32(k) * epochShift
				f.PTS += shift
				f.DTS += shift
				if a, ok := anchors[f.Epoch]; ok {
					due := a.Wall.Add(media.ToDuration(f.DTS - a.Start))
					if d := time.Until(due); d > 0 {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(d):
						}
					}
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				h.Write(f)
			}
		}
		return nil
	}
}

// m4aSource is the source feed of a fixture: its Clock steps, anchored at
// m3aT0, written at the fixture's own pace (each frame at its DTS after the
// first). Pacing matters to the copy renditions: their video goes straight
// to the merger while the child encodes the audio, and the merger holds a
// track for at most 2 s of stream time, which an unpaced feed spends at
// once.
func m4aSource(t *testing.T, name string) feed {
	t.Helper()
	steps := clockSteps(t, name, newFakeClock(m3aT0).Now)
	return func(ctx context.Context, h *hub.Hub) error {
		start := time.Now()
		first := steps[0].f.DTS
		for _, s := range steps {
			if d := time.Until(start.Add(media.ToDuration(s.f.DTS - first))); d > 0 {
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

// m4aRun is one transcoder run over a fixture, shared by the scenarios that
// use it.
type m4aRun struct {
	steps [][]step // per rendition
	stats transcode.Stats
}

// m4aRuns transcodes each fixture once, in parallel.
func m4aRuns(t *testing.T, bin string, names ...string) map[string]m4aRun {
	t.Helper()
	out := map[string]m4aRun{}
	var mu sync.Mutex
	t.Run("transcode", func(t *testing.T) {
		for _, name := range names {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				start := time.Now()
				steps, st := transcodeSteps(t, m4aSource(t, name), m4aLadder(bin))
				for i, s := range steps {
					t.Logf("%s: %s", m4aLadder(bin).Renditions[i].Name, kindCounts(stepFrames(s)))
				}
				t.Logf("transcoded in %v; stats %+v", time.Since(start).Round(time.Millisecond), st)
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

// skipOnArm skips a peer row that the arm64 CI job does not run.
func skipOnArm(t *testing.T) {
	t.Helper()
	if os.Getenv(envTranscodeArm) == "1" {
		t.Skip(envTranscodeArm + "=1: no SRT or RTSP peer rows on arm64")
	}
}

// feedScenario packages and serves a recorded rendition as pipelineScenario
// does a pipeline run, reads every output back, and checks that each
// carries every frame.
func feedScenario(t *testing.T, steps []step) (*live.Packager, string, map[string]conform.Capture) {
	t.Helper()
	clk := newFakeClock(m3aT0)
	fd := stepsFeed(steps)
	p, written := packFeed(t, fd, clk.Now)
	content := advance(t, p, clk)
	t.Logf("written %d frames %s, content %v, %d segments, %d epochs",
		len(written), kindCounts(written), content, len(p.Snapshot().Segments), len(p.Snapshot().Epochs))
	base := serve(t, p, fd)
	caps := readAll(t, base)
	assertComplete(t, caps, written)
	return p, base, caps
}

// videoFrames returns the video frames of frames.
func videoFrames(frames []media.Frame) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Kind() == media.KindVideo {
			out = append(out, f)
		}
	}
	return out
}

// keySet returns the sorted PTS of the video keyframes of frames.
func keySet(frames []media.Frame) []int64 {
	var out []int64
	for _, f := range videoFrames(frames) {
		if f.Key {
			out = append(out, f.PTS)
		}
	}
	slices.Sort(out)
	return out
}

// shiftedSource returns the source's frames as the source hub holds them,
// shifted by D.
func shiftedSource(t *testing.T, name string) []media.Frame {
	t.Helper()
	steps := clockSteps(t, name, newFakeClock(m3aT0).Now)
	out := make([]media.Frame, 0, len(steps))
	for _, s := range steps {
		f := s.f
		f.PTS += transcode.Delay
		f.DTS += transcode.Delay
		out = append(out, f)
	}
	return out
}

// TestM4aMatrix runs every rendition of the ladder (t240, t180, t120 and
// src, a copy) through the M3 scenarios:
//
//   - steady-state: h264_aac.ts packaged and read back from HLS-TS, HLS
//     fMP4, DASH and HTTP-TS clean, each with every frame written.
//   - epoch-change: h264_aac_then_640.ts, the same, in the source's two
//     epochs.
//   - slow-consumer: http-ts/slow-consumer of M3 on the rendition replayed
//     at its pace three times (pacedFeed).
//   - no-bframes: h264_aac_nob.ts, whose encoder delay D must cover: every
//     reader free of violations.
//   - b-frames: h264_pyramid.ts. On src, ffprobe reads the fixture's
//     packets shifted by D; on an encoded rung its PTS set is the source's
//     shifted by D, its DTS strictly increase and some packet has PTS > DTS.
//
// t180 also runs srt-steady (M3's srt/steady-state) and rtsp-steady
// (ReadRTSP clean) on its replayed rendition.
func TestM4aMatrix(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	runs := m4aRuns(t, bin, "h264_aac.ts", "h264_aac_then_640.ts", "h264_aac_nob.ts", "h264_pyramid.ts")
	ladder := m4aLadder(bin).Renditions
	for i, r := range ladder {
		t.Run(r.Name, func(t *testing.T) {
			t.Parallel()
			t.Run("steady-state", func(t *testing.T) {
				t.Parallel()
				p, base, _ := feedScenario(t, runs["h264_aac.ts"].steps[i])
				assert.Len(t, p.Snapshot().Epochs, 1)
				assert.Zero(t, discontinuities(mediaPlaylist(t, base+"hls/index.m3u8")))
				assert.Len(t, mpd(t, base).Periods, 1)
			})
			t.Run("epoch-change", func(t *testing.T) {
				t.Parallel()
				p, _, caps := feedScenario(t, runs["h264_aac_then_640.ts"].steps[i])
				assert.Len(t, p.Snapshot().Epochs, 2)
				for _, f := range []string{"hls-ts", "hls-fmp4", "dash"} {
					assert.Equal(t, []uint32{1, 2}, epochs(caps[f].Frames), f)
				}
			})
			t.Run("slow-consumer", func(t *testing.T) {
				steps := runs["h264_aac.ts"].steps[i]
				testHTTPTSSlowConsumer(t, func(t *testing.T) *m2aRun {
					t.Helper()
					return startFeed(t, pacedFeed(steps, 3))
				})
			})
			t.Run("no-bframes", func(t *testing.T) {
				t.Parallel()
				feedScenario(t, runs["h264_aac_nob.ts"].steps[i])
			})
			t.Run("b-frames", func(t *testing.T) {
				t.Parallel()
				testM4aBFrames(t, runs["h264_pyramid.ts"].steps[i], r.Video.Copy)
			})
			if r.Name != "t180" {
				return
			}
			t.Run("srt-steady", func(t *testing.T) {
				skipOnArm(t)
				steps := runs["h264_aac.ts"].steps[i]
				testSRTSteady(t, func(t *testing.T) *m2aRun {
					t.Helper()
					return startFeed(t, pacedFeed(steps, 3))
				}, media.CodecH264, []string{"video:h264", "audio:aac"})
			})
			t.Run("rtsp-steady", func(t *testing.T) {
				skipOnArm(t)
				t.Parallel()
				testM4aRTSPSteady(t, runs["h264_aac.ts"].steps[i])
			})
		})
	}
}

// bframeOutputs are the HTTP outputs ffprobe reads in the b-frames rows.
// Our MPD stays dynamic after the stream ends, so ffprobe reads the first
// 5 s of DASH (as in TestM3Matrix).
var bframeOutputs = []struct {
	name, path string
	extra      []string
}{
	{"hls-fmp4", "cmaf/master.m3u8", nil},
	{"hls-ts", "hls/index.m3u8", nil},
	{"dash", "cmaf/index.mpd", []string{"-read_intervals", "%+5"}},
	{"http-ts", "stream.ts", nil},
}

// testM4aBFrames reads a rendition of h264_pyramid.ts back from the four
// HTTP outputs with conform and ffprobe, against ffprobe's reading of the
// source stream packaged the same way (probeBase), whose time origin on
// each output it shares.
//   - src (copied): pyramidRef's key flags and composition offsets, and
//     every packet D after the source stream's.
//   - an encoded rung: the PTS set is the source's + D, the DTS strictly
//     increase, and some packet has PTS > DTS.
func testM4aBFrames(t *testing.T, steps []step, copied bool) {
	t.Helper()
	feedScenario(t, steps)
	ffprobe := testpeer.Require(t, "ffprobe")
	srcBase := probeBase(t, "h264_pyramid.ts")
	base := probeSteps(t, steps)
	ref := pyramidRef(t)
	srcVideo := videoFrames(stepFrames(clockSteps(t, "h264_pyramid.ts", time.Now)))
	want := map[int64]bool{}
	for _, f := range srcVideo {
		want[f.PTS+transcode.Delay] = true
	}
	const tol = 0.0015 // seconds, as assertReorder
	d := media.ToDuration(transcode.Delay).Seconds()
	for _, out := range bframeOutputs {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		srcPkts, err := probeVideoTimes(ctx, ffprobe, srcBase+out.path, out.extra...)
		require.NoError(t, err, out.name)
		pkts, err := probeVideoTimes(ctx, ffprobe, base+out.path, out.extra...)
		cancel()
		require.NoError(t, err, out.name)
		require.GreaterOrEqual(t, len(pkts), len(ref)/2, "%s: ffprobe read most of the clip", out.name)
		require.NotEmpty(t, srcPkts, out.name)
		if copied {
			reordered := assertReorder(t, ref, pkts, true)
			assert.Positive(t, reordered, out.name)
			require.LessOrEqual(t, len(pkts), len(srcPkts), out.name)
			for j, p := range pkts {
				require.InDelta(t, srcPkts[j].pts+d, p.pts, tol, "%s: packet %d pts is the source's + D", out.name, j)
				require.InDelta(t, srcPkts[j].dts+d, p.dts, tol, "%s: packet %d dts is the source's + D", out.name, j)
			}
			t.Logf("%s: %d packets, %d reordered, first at %.4f s, the source's at %.4f s", out.name, len(pkts),
				reordered, pkts[0].pts, srcPkts[0].pts)
			continue
		}
		// The output's time origin, from the source stream's first packet.
		origin := srcPkts[0].pts - float64(srcVideo[0].PTS)/90000
		got := map[int64]bool{}
		prev := math.Inf(-1)
		reordered := 0
		for j, p := range pkts {
			require.True(t, p.hasDTS, "%s: packet %d has a dts", out.name, j)
			tick := int64(math.Round((p.pts - origin) * 90000))
			require.True(t, want[tick], "%s: packet %d PTS %d is a source PTS + D", out.name, j, tick)
			got[tick] = true
			require.Greater(t, p.dts, prev, "%s: packet %d dts increasing", out.name, j)
			prev = p.dts
			if p.pts > p.dts+tol/3 {
				reordered++
			}
		}
		if len(pkts) == len(srcVideo) {
			assert.Len(t, got, len(want), "%s: the PTS set is the source's + D", out.name)
		}
		assert.Positive(t, reordered, "%s: packets with PTS > DTS", out.name)
		t.Logf("%s: %d packets, %d distinct PTS, %d with PTS > DTS", out.name, len(pkts), len(got), reordered)
	}
}

// probeSteps packages steps on the real clock with their epochs anchored in
// the past, as probeBase does, so that every segment is available, and
// serves them until the test ends. It returns the stream's base URL.
func probeSteps(t *testing.T, steps []step) string {
	t.Helper()
	var last int64
	for _, s := range steps {
		last = max(last, s.f.PTS)
	}
	t0 := time.Now().Add(-media.ToDuration(last) - 5*time.Second)
	re := slices.Clone(steps)
	shift := t0.Sub(steps[0].anchors[0].Wall)
	for i := range re {
		if len(re[i].anchors) == 0 {
			continue
		}
		re[i].anchors = slices.Clone(re[i].anchors)
		for j := range re[i].anchors {
			re[i].anchors[j].Wall = re[i].anchors[j].Wall.Add(shift)
		}
	}
	fd := stepsFeed(re)
	p, _ := packFeed(t, fd, time.Now)
	return serve(t, p, fd)
}

// testM4aRTSPSteady plays a rendition, replayed three times at its pace,
// over RTSP (TCP and UDP) and reads it back clean with exactly the hub's
// frames.
func testM4aRTSPSteady(t *testing.T, steps []step) {
	t.Helper()
	r := startFeed(t, pacedFeed(steps, 3))
	o := serveRTSP(t, "", rtspplay.Options{}, map[string]*hub.Hub{"s": r.h})
	waitAV(t, r.h)
	tcpCh := readRTSP(o.url())
	udpCh := readRTSP(o.url() + "?transport=udp")
	r.check(t, []uint32{1})
	for _, tr := range []struct {
		name string
		ch   <-chan capResult
	}{{"tcp", tcpCh}, {"udp", udpCh}} {
		c := waitCapture(t, tr.ch)
		assert.Empty(t, c.Violations, tr.name)
		assert.Equal(t, []uint32{1}, epochs(c.Frames), tr.name)
		assert.Zero(t, c.RTPLost, tr.name)
		assertCodecs(t, c.Frames, media.CodecH264, media.CodecAAC)
		assertRTSPMatchesHub(t, r.col.frames, c.Frames)
		t.Logf("%s read %s, %d RTP packets lost", tr.name, kindCounts(c.Frames), c.RTPLost)
	}
	ps := o.srv.Stats()
	assert.Zero(t, ps.Resyncs)
	assert.Zero(t, ps.EndedFrameError)
	t.Logf("hub %s; play %+v", kindCounts(r.col.frames), ps)
}

// TestM4aKeyframesAligned checks that the four renditions of h264_aac.ts
// have the same keyframe PTS set, the source's + D, and that their HLS-TS
// segments have the same boundaries.
func TestM4aKeyframesAligned(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	opt := m4aLadder(bin)
	feeds := transcodeFeed(t, m4aSource(t, "h264_aac.ts"), opt)
	want := keySet(shiftedSource(t, "h264_aac.ts"))
	require.NotEmpty(t, want)
	type bound struct {
		epoch           uint32
		start, duration int64
	}
	var ref []bound
	var refExtinf []time.Duration
	for i, fd := range feeds {
		name := opt.Renditions[i].Name
		clk := newFakeClock(m3aT0)
		p, written := packFeed(t, fd, clk.Now)
		assert.Equal(t, want, keySet(written), "%s: keyframe PTS set", name)
		advance(t, p, clk)
		var bounds []bound
		for _, s := range p.Snapshot().Segments {
			if s.TS != nil {
				bounds = append(bounds, bound{s.Epoch, s.Start, s.Duration})
			}
		}
		base := serve(t, p, fd)
		var extinf []time.Duration
		for _, s := range mediaPlaylist(t, base+"hls/index.m3u8").Segments {
			extinf = append(extinf, s.Duration)
		}
		require.NotEmpty(t, bounds, name)
		if i == 0 {
			ref, refExtinf = bounds, extinf
			t.Logf("keys %v; segments %v; EXTINF %v", want, bounds, extinf)
			continue
		}
		assert.Equal(t, ref, bounds, "%s: HLS-TS segment boundaries", name)
		assert.Equal(t, refExtinf, extinf, "%s: HLS-TS EXTINF", name)
	}
}

// arrivals records when each epoch's first frame reached a hub, and
// whether a video frame has.
type arrivals struct {
	mu    sync.Mutex
	first map[uint32]time.Time
	video bool
}

func watchEpochs(h *hub.Hub) *arrivals {
	a := &arrivals{first: map[uint32]time.Time{}}
	sub := h.Subscribe("epochs", bigQueue)
	go func() {
		for dl := range sub.C() {
			a.mu.Lock()
			if _, ok := a.first[dl.Frame.Epoch]; !ok {
				a.first[dl.Frame.Epoch] = time.Now()
			}
			a.video = a.video || dl.Frame.Kind() == media.KindVideo
			a.mu.Unlock()
		}
	}()
	return a
}

// hasVideo reports whether a video frame has reached the hub.
func (a *arrivals) hasVideo() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.video
}

func (a *arrivals) get() map[uint32]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.first)
}

// TestM4aRespawn kills the child with SIGKILL mid-stream. The transcoder is
// fed h264_aac_then_640.ts looped twice at its pace (upstream epochs 1 to
// 4, the second of each pass video-only at 640x360), and the child is
// killed at most 0.5 s after the first encoded video frame (t240), inside
// upstream epoch 1.
// Every rendition hub gets exactly one new epoch within 5 s of the kill,
// starting on a keyframe; the later upstream epochs map above it; and each
// rendition's HLS fMP4, read back, has exactly one discontinuity more than
// the source's epoch changes, and no violation.
func TestM4aRespawn(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	opt := m4aLadder(bin)
	opt.Logger = m4aLogger(t)
	src := hub.New(hub.Options{})
	tr, err := transcode.New(src, opt)
	require.NoError(t, err)
	n := len(opt.Renditions)
	cols := make([]*collector, 0, n)
	packs := make([]*live.Packager, 0, n)
	arr := make([]*arrivals, 0, n)
	var pwg sync.WaitGroup
	for _, h := range tr.Hubs() {
		cols = append(cols, collect(h, "e2e"))
		arr = append(arr, watchEpochs(h))
		p := live.NewPackager(h, live.Options{Window: m3aWindow})
		sub := h.Subscribe("packager", bigQueue)
		pwg.Go(func() { p.Run(context.Background(), sub) })
		packs = append(packs, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- tr.Run(ctx) }()
	require.Eventually(t, func() bool { return len(src.Stats()) > 0 }, 10*time.Second, time.Millisecond)

	srcSteps := clockSteps(t, "h264_aac_then_640.ts", time.Now)
	fed := make(chan error, 1)
	go func() {
		fed <- pacedFeed(srcSteps, 2)(ctx, src)
		src.Close()
	}()

	// Kill the child 0.5 s after the first encoded video frame (t240; the
	// copy rendition's video comes at once): mid-GOP, after the child has
	// output video in epoch 1, with K well inside upstream epoch 1 (its last
	// key is at about 5.08 s) even on a slow runner.
	require.Eventually(t, arr[0].hasVideo, 60*time.Second, 5*time.Millisecond, "an encoded video frame on t240")
	time.Sleep(500 * time.Millisecond)
	pid := tr.Stats().PID
	require.Positive(t, pid)
	killed := time.Now()
	proc, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, proc.Kill()) // SIGKILL on Unix

	require.NoError(t, <-fed)
	require.NoError(t, <-runErr)
	pwg.Wait()
	st := tr.Stats()
	t.Logf("stats %+v", st)
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, st.Exits)
	assert.Zero(t, st.Losses)

	// Upstream epochs 1..4, plus the bump after epoch 1's kill.
	want := []uint32{1, 2, 3, 4, 5}
	kd := int64(-1) // K + D
	for i, r := range opt.Renditions {
		<-cols[i].done
		frames := cols[i].frames
		assert.Zero(t, cols[i].lost, r.Name)
		assert.Empty(t, conform.CheckFrames(frames, conform.FrameOptions{}), r.Name)
		assert.Equal(t, want, epochs(frames), r.Name)
		if !r.Video.Copy {
			assert.Positive(t, kindCounts(frames)[epochKind{1, media.KindVideo}],
				"%s: the child output video in epoch 1 before the kill (else the kill came too early)", r.Name)
		}
		// The respawn epoch starts with K's keyframe, at K + D on every
		// rendition, where its anchor starts, and nothing in it is
		// presented before. (On an encoded rung the new child's audio
		// reaches the hub ahead of its first video, by the encoder's
		// delay: the old child's last video never came, so the merger no
		// longer waits for video.)
		var k media.Frame
		minPTS := int64(math.MaxInt64)
		audioFirst := false
		for _, f := range frames {
			if f.Epoch != 2 {
				continue
			}
			if minPTS == math.MaxInt64 {
				audioFirst = f.Kind() == media.KindAudio
			}
			minPTS = min(minPTS, f.PTS)
			if k.Config == nil && f.Kind() == media.KindVideo {
				k = f
			}
		}
		require.NotNil(t, k.Config, "%s: video in the respawn epoch", r.Name)
		assert.True(t, k.Key, "%s: the respawn epoch's video starts on a keyframe", r.Name)
		assert.GreaterOrEqual(t, minPTS, k.PTS, "%s: nothing in the respawn epoch before K + D", r.Name)
		a, ok := tr.Hubs()[i].Anchor(2)
		require.True(t, ok, r.Name)
		assert.Equal(t, k.PTS, a.Start, "%s: the respawn anchor starts at K + D", r.Name)
		if kd < 0 {
			kd = k.PTS
		}
		assert.Equal(t, kd, k.PTS, "%s: the same K on every rendition", r.Name)
		at := arr[i].get()[2]
		require.False(t, at.IsZero(), r.Name)
		assert.Less(t, at.Sub(killed), 5*time.Second, "%s: the new epoch within 5 s of the kill", r.Name)
		// The upstream epoch change after the kill maps above the respawn
		// epoch: epoch 3 is the source's second epoch, video only.
		for _, f := range frames {
			if f.Epoch == 3 {
				require.Equal(t, media.KindVideo, f.Kind(), "%s: epoch 3 is the video-only upstream epoch", r.Name)
				require.Greater(t, f.PTS, k.PTS, r.Name)
			}
		}
		t.Logf("%s: %s; respawn epoch at K + D = %d (audio first: %v), %v after the kill", r.Name, kindCounts(frames),
			k.PTS, audioFirst, at.Sub(killed).Round(time.Millisecond))
	}

	// HLS fMP4, read back once the stream has ended: 3 epoch changes from
	// the source and 1 from the respawn.
	for i, r := range opt.Renditions {
		p := packs[i]
		srv := serveHTTP(t, "", nil, map[string]*live.Packager{"s": p}, 0, nil)
		base := srv + "/live/s/"
		require.Eventually(t, func() bool {
			s := p.Snapshot()
			return s.Ended && s.Listed == len(s.Segments)
		}, 10*time.Second, 20*time.Millisecond, "%s: every segment listed", r.Name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := conform.ReadHLS(ctx, base+"cmaf/master.m3u8", conform.ReadOptions{Once: true, AlignedSequences: true})
		cancel()
		require.NoError(t, err, r.Name)
		assert.Empty(t, c.Violations, r.Name)
		assert.Equal(t, want, epochs(c.Frames), r.Name)
		for _, u := range cmafPlaylists(t, base) {
			pl := mediaPlaylist(t, u)
			assert.Equal(t, 4, discontinuities(pl), "%s: %s", r.Name, u)
		}
		t.Logf("%s: hls-fmp4 read %s", r.Name, kindCounts(c.Frames))
	}
}

// TestM4aVLC plays the t180 rendition's HLS TS in VLC with the pass rule of
// TestM3VLCClass's hls-ts row, the start-up pair's tolerance of ruling
// R-T1c included (ruling R-T7a'): the rendition of h264_aac.ts replayed four
// times at its pace, packaged live as the M3 row does.
func TestM4aVLC(t *testing.T) {
	bin := testpeer.OSTranscoder(t)
	vlc := testpeer.VLC(t, "adaptive", "ts", "avcodec")
	ffprobe := testpeer.Require(t, "ffprobe")
	opt := m4aLadder(bin)
	idx := slices.IndexFunc(opt.Renditions, func(r transcode.Rendition) bool { return r.Name == "t180" })
	recorded, _ := transcodeSteps(t, m4aSource(t, "h264_aac.ts"), opt)
	var p *live.Packager
	done := make(chan struct{})
	r := startFeed(t, pacedFeed(recorded[idx], 4), func(h *hub.Hub) {
		p = live.NewPackager(h, live.Options{})
		sub := h.Subscribe("packager", bigQueue)
		go func() {
			defer close(done)
			p.Run(context.Background(), sub)
		}()
	})
	t.Cleanup(func() {
		r.cancel()
		r.wait()
		<-done
	})
	origin := serveHTTP(t, "", map[string]*hub.Hub{"s": r.h}, map[string]*live.Packager{"s": p}, 0, nil)
	waitAV(t, r.h)
	waitListed(t, p, 5) // as the M3 row: a window of five segments
	vlcPlays(t, vlc, ffprobe, origin+"/live/s/hls/index.m3u8", true)
}
