package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

// The NVENC scenarios (ruling R-C12) run ladders with NVENC rungs on the
// host's GPU, under make test-nvenc (scripts/test-nvenc.sh). Every one but
// TestM4cNVENCRecordedVLC starts with testpeer.NVENC, so they are opt-in:
// without OS_NVENC=1 or OS_REQUIRE_NVENC=1, which make test-nvenc sets once
// it has found the GPU free, they skip without opening an NVENC session
// (a plain go test or make test on the GPU host must not disturb other
// users of the GPU); opted in but without a device (CI, the peers
// container) they skip naming the probe's reason, or fail with
// OS_REQUIRE_NVENC=1. Transcoder runs go one after another: no
// top-level test here is parallel and no t.Parallel surrounds a run
// (serialRuns), so at most 3 NVENC sessions are open at once. The checks of
// the recorded runs may run in parallel.

// envNVENCMedia names the directory, relative to the repository root unless
// it is absolute, that TestM4cNVENCKeep writes the recorded NVENC
// presentation to and TestM4cNVENCRecordedVLC plays.
const envNVENCMedia = "OS_NVENC_MEDIA"

// envNVENCShared, set to "1", lets the NVENC scenarios run while other
// processes hold NVENC sessions: TestM4cNVENCSessions then asserts a rise
// by 3 over the count before the run instead of 0, 3, 0.
const envNVENCShared = "OS_NVENC_SHARED"

// nvencBinary returns the ostranscoder of the NVENC scenarios, after
// testpeer.NVENC has checked the opt-in and found a device for it.
func nvencBinary(t *testing.T) string {
	t.Helper()
	if !testpeer.NVENCOptedIn() {
		t.Skip(testpeer.NVENCOptIn) // before looking for the transcoder
	}
	bin := testpeer.OSTranscoder(t)
	testpeer.NVENC(t, bin)
	return bin
}

// nvencLadder is m4aLadder with its three encoded rungs (t240, t180, t120)
// on NVENC with bframes (-1 none, 0 the encoder default of 2), the copy rung
// kept, and the presentation hub on.
func nvencLadder(binary string, bframes int) transcode.Options {
	opt := m4aLadder(binary)
	for i := range opt.Renditions {
		v := &opt.Renditions[i].Video
		if !v.Copy {
			v.Encoder = transcode.EncoderNVENC
			v.BFrames = bframes
		}
	}
	opt.Presentation = true
	return opt
}

// serialRun runs one transcoder with opt (which must have the
// presentation) over src and checks the run as m4bRuns does: clean (one
// child, no exit, no loss, nothing dropped, an anchor before every epoch),
// nothing dropped by the presentation's merger, the rendition hubs (except
// for m4bLowFPS) and the presentation starting with video, every
// presentation epoch starting with rung 0's IDR; and, since m4bRun carries
// no stats, no rung fallen back from NVENC to x264 (EncoderFallbacks 0).
// name labels the logs.
func serialRun(t *testing.T, opt transcode.Options, name string, src feed) m4bRun {
	t.Helper()
	require.True(t, opt.Presentation, "serialRun needs the presentation")
	start := time.Now()
	run := recordTranscode(t, src, opt, nil)
	for i, s := range run.renditions {
		if !m4bLowFPS[name] {
			requireVideoFirst(t, stepFrames(s), opt.Renditions[i].Name)
		}
		t.Logf("%s: %s", opt.Renditions[i].Name, kindCounts(stepFrames(s)))
	}
	requireCleanRun(t, run.stats)
	require.Zero(t, run.stats.PresentationDropped, "%+v", run.stats)
	require.Zero(t, run.stats.Child.EncoderFallbacks, "no rung fell back from NVENC: %+v", run.stats)
	pres := stepFrames(run.presentation)
	requireVideoFirst(t, pres, "presentation")
	requireRungZeroFirst(t, pres, len(opt.Renditions))
	t.Logf("presentation: %s", trackCounts(pres))
	t.Logf("%s transcoded in %v; stats %+v", name, time.Since(start).Round(time.Millisecond), run.stats)
	return m4bRun{presentation: run.presentation, renditions: run.renditions}
}

// serialRuns transcodes each named fixture through opt, one after another
// and never in parallel (m4aRuns and m4bRuns run their fixtures in
// parallel and fix their ladder), each run checked by serialRun. A name
// "<fixture>*<n>" feeds the fixture n times as one timeline (loopSteps) at
// its pace; a plain fixture name feeds it once, as m4aSource does.
func serialRuns(t *testing.T, opt transcode.Options, names ...string) map[string]m4bRun {
	t.Helper()
	out := make(map[string]m4bRun, len(names))
	t.Run("transcode", func(t *testing.T) {
		for _, name := range names {
			t.Run(name, func(t *testing.T) {
				out[name] = serialRun(t, opt, name, serialSource(t, name))
			})
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	return out
}

// serialSource is the source feed of a serialRuns name.
func serialSource(t *testing.T, name string) feed {
	t.Helper()
	fixture, passes, looped := strings.Cut(name, "*")
	if !looped {
		return m4aSource(t, name)
	}
	n, err := strconv.Atoi(passes)
	require.NoError(t, err, "serialRuns name %q", name)
	return pacedSteps(loopedFixture(t, fixture, n))
}

// hasX264SEI reports whether f has an H.264 SEI NAL unit (type 6) that
// contains "x264": libx264 writes its options there, NVENC does not.
func hasX264SEI(f media.Frame) bool {
	for _, nal := range f.Data {
		if len(nal) > 0 && nal[0]&0x1f == 6 && bytes.Contains(nal, []byte("x264")) {
			return true
		}
	}
	return false
}

// assertNVENCFrames checks that no video frame of an NVENC rung carries
// x264's SEI.
func assertNVENCFrames(t *testing.T, frames []media.Frame, name string) {
	t.Helper()
	for j, f := range videoFrames(frames) {
		if hasX264SEI(f) {
			assert.Fail(t, "x264's SEI on an NVENC rung", "%s: video frame %d (PTS %d)", name, j, f.PTS)
			return
		}
	}
}

// assertNVENCLadder checks a serialRuns run of a fixture through opt: every
// encoded rung's keyframe PTS set is the source's + D, no NVENC rung frame
// carries x264's SEI, and the presentation, packaged in ABR mode and read
// back as hls-ts, hls-fmp4 and dash with AlignedSequences (feedABR), has no
// violation and carries every frame (assertCompleteABR).
func assertNVENCLadder(t *testing.T, opt transcode.Options, run m4bRun, fixture string) {
	t.Helper()
	want := keySet(shiftedSource(t, fixture))
	require.NotEmpty(t, want)
	for i, r := range opt.Renditions {
		if r.Video.Copy {
			continue
		}
		frames := stepFrames(run.renditions[i])
		assert.Equal(t, want, keySet(frames), "%s: the keyframe PTS set is the source's + D", r.Name)
		if r.Video.Encoder == transcode.EncoderNVENC {
			assertNVENCFrames(t, frames, r.Name)
		}
	}
	_, _, caps := feedABR(t, run.presentation, 0)
	assertCompleteABR(t, caps, stepFrames(run.presentation))
}

// sourcePTS returns the set of the fixture's video PTS + D.
func sourcePTS(t *testing.T, fixture string) map[int64]bool {
	t.Helper()
	want := map[int64]bool{}
	for _, f := range videoFrames(shiftedSource(t, fixture)) {
		want[f.PTS] = true
	}
	require.NotEmpty(t, want)
	return want
}

// assertHubTiming checks a rendition's video frames against the fixture's:
// the PTS set is the source video's + D, one frame per source picture, and
// the DTS strictly increase within each epoch. It returns how many frames
// have PTS > DTS, and fails on a DTS above its PTS.
func assertHubTiming(t *testing.T, frames []media.Frame, fixture string) (reordered int) {
	t.Helper()
	want := sourcePTS(t, fixture)
	video := videoFrames(frames)
	got := map[int64]bool{}
	last := map[uint32]int64{}
	for j, f := range video {
		require.True(t, want[f.PTS], "frame %d: PTS %d is a source PTS + D", j, f.PTS)
		got[f.PTS] = true
		require.LessOrEqual(t, f.DTS, f.PTS, "frame %d: DTS %d not above its PTS", j, f.DTS)
		if p, ok := last[f.Epoch]; ok {
			require.Greater(t, f.DTS, p, "frame %d: DTS increasing within epoch %d", j, f.Epoch)
		}
		last[f.Epoch] = f.DTS
		if f.PTS > f.DTS {
			reordered++
		}
	}
	assert.Len(t, video, len(want), "one frame per source picture")
	assert.Len(t, got, len(want), "the PTS set is the source's + D")
	return reordered
}

// assertNoReorder checks an NVENC rung of a ladder without B-frames:
// reordered == 0 in the hub (every frame's DTS is its PTS) with the PTS set
// and increasing DTS of assertHubTiming, and on the four HTTP outputs
// (bframeOutputs, packaged as probeSteps does) ffprobe's packets match the
// rung's own frames (assertReorder against hubTimes, aligned at the first
// packet) with no packet reordered. A rung with more than one epoch has a
// multi-period MPD, which ffprobe does not read (dashNoReorder): its DASH
// is read with conform instead.
func assertNoReorder(t *testing.T, ffprobe string, steps []step, fixture string) {
	t.Helper()
	frames := stepFrames(steps)
	assert.Zero(t, assertHubTiming(t, frames, fixture), "hub frames with PTS > DTS")
	ref := hubTimes(frames)
	base := probeSteps(t, steps)
	multiPeriod := len(epochs(frames)) > 1
	for _, out := range bframeOutputs {
		if out.name == "dash" && multiPeriod {
			dashNoReorder(t, base, frames)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		pkts, err := probeVideoTimes(ctx, ffprobe, base+out.path, out.extra...)
		cancel()
		require.NoError(t, err, out.name)
		require.GreaterOrEqual(t, len(pkts), len(ref)/2, "%s: ffprobe read most of the rung", out.name)
		assert.Zero(t, assertReorder(t, ref, pkts, true), "%s: packets with PTS > DTS", out.name)
		t.Logf("%s: %d packets of %d, none reordered", out.name, len(pkts), len(ref))
	}
}

// dashNoReorder reads the DASH output at base once with conform and checks
// that its video frames are the hub's frames, in order, with the same key
// flags, and that each has DTS == PTS. It stands in for ffprobe on an MPD
// with more than one period: ffprobe 8.0.1 did not finish reading the
// two-period MPD of h264_aac_then_640.ts's rungs within 60 s, even with
// -read_intervals %+5, on any of the three rungs.
func dashNoReorder(t *testing.T, base string, frames []media.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := conform.ReadDASH(ctx, base+"cmaf/index.mpd", conform.ReadOptions{Once: true})
	require.NoError(t, err, "dash")
	assert.Empty(t, c.Violations, "dash")
	want, got := videoFrames(frames), videoFrames(c.Frames)
	require.Len(t, got, len(want), "dash: every video frame")
	for j, f := range got {
		require.Equal(t, want[j].Key, f.Key, "dash: frame %d key", j)
		require.Equal(t, f.PTS, f.DTS, "dash: frame %d DTS == PTS", j)
	}
	t.Logf("dash (conform, %d periods): %d frames, none reordered", len(epochs(c.Frames)), len(got))
}

// assertM4aRows runs TestM4aMatrix's steady-state, epoch-change and
// no-bframes rows, with their assertions, on rendition i of runs (keyed by
// fixture): h264_aac.ts, h264_aac_then_640.ts and h264_aac_nob.ts each
// packaged and read back clean with every frame (feedScenario); one epoch,
// no discontinuity and one period for the first, epochs [1 2] in hls-ts,
// hls-fmp4 and dash for the second.
func assertM4aRows(t *testing.T, runs map[string]m4bRun, i int) {
	t.Helper()
	t.Run("steady-state", func(t *testing.T) {
		t.Parallel()
		p, base, _ := feedScenario(t, runs["h264_aac.ts"].renditions[i])
		assert.Len(t, p.Snapshot().Epochs, 1)
		assert.Zero(t, discontinuities(mediaPlaylist(t, base+"hls/index.m3u8")))
		assert.Len(t, mpd(t, base).Periods, 1)
	})
	t.Run("epoch-change", func(t *testing.T) {
		t.Parallel()
		p, _, caps := feedScenario(t, runs["h264_aac_then_640.ts"].renditions[i])
		assert.Len(t, p.Snapshot().Epochs, 2)
		for _, f := range []string{"hls-ts", "hls-fmp4", "dash"} {
			assert.Equal(t, []uint32{1, 2}, epochs(caps[f].Frames), f)
		}
	})
	t.Run("no-bframes", func(t *testing.T) {
		t.Parallel()
		feedScenario(t, runs["h264_aac_nob.ts"].renditions[i])
	})
}

// TestM4cNVENCMatrix runs two NVENC ladders over the M4a fixtures, one
// fixture after another (serialRuns: clean runs, EncoderFallbacks 0). For
// every fixture and ladder: the encoded rungs' keyframe PTS are the
// source's + D, no NVENC rung frame carries x264's SEI, and the
// presentation read back in ABR mode is clean and complete
// (assertNVENCLadder).
//   - noB, nvencLadder(bin, -1), over h264_aac.ts, h264_aac_then_640.ts,
//     h264_aac_nob.ts and h264_pyramid.ts: every rendition passes
//     assertM4aRows; every NVENC rung, on every fixture, has nothing
//     reordered in the hub or on the HTTP outputs (assertNoReorder); the
//     copy rung of h264_pyramid.ts passes testM4aBFrames(steps, true).
//   - B, nvencLadder(bin, 0) (2 B-frames): on h264_pyramid.ts every NVENC
//     rung passes testM4aBFrames(steps, false), reordered packets
//     included, and the copy rung (steps, true); on h264_aac.ts every
//     NVENC rung's hub frames have some PTS > DTS, strictly increasing DTS
//     and the source's PTS + D, and feedScenario reads the rung clean.
func TestM4cNVENCMatrix(t *testing.T) {
	bin := nvencBinary(t)
	ffprobe := testpeer.Require(t, "ffprobe")
	t.Run("noB", func(t *testing.T) {
		opt := nvencLadder(bin, -1)
		fixtures := []string{"h264_aac.ts", "h264_aac_then_640.ts", "h264_aac_nob.ts", "h264_pyramid.ts"}
		runs := serialRuns(t, opt, fixtures...)
		for _, name := range fixtures {
			t.Run("ladder/"+name, func(t *testing.T) {
				t.Parallel()
				assertNVENCLadder(t, opt, runs[name], name)
			})
		}
		for i, r := range opt.Renditions {
			t.Run(r.Name, func(t *testing.T) {
				t.Parallel()
				assertM4aRows(t, runs, i)
				if r.Video.Copy {
					t.Run("b-frames", func(t *testing.T) {
						t.Parallel()
						testM4aBFrames(t, runs["h264_pyramid.ts"].renditions[i], true)
					})
					return
				}
				for _, name := range fixtures {
					t.Run("no-reorder/"+name, func(t *testing.T) {
						t.Parallel()
						assertNoReorder(t, ffprobe, runs[name].renditions[i], name)
					})
				}
			})
		}
	})
	t.Run("B", func(t *testing.T) {
		opt := nvencLadder(bin, 0)
		fixtures := []string{"h264_pyramid.ts", "h264_aac.ts"}
		runs := serialRuns(t, opt, fixtures...)
		for _, name := range fixtures {
			t.Run("ladder/"+name, func(t *testing.T) {
				t.Parallel()
				assertNVENCLadder(t, opt, runs[name], name)
			})
		}
		for i, r := range opt.Renditions {
			t.Run(r.Name, func(t *testing.T) {
				t.Parallel()
				t.Run("b-frames", func(t *testing.T) {
					t.Parallel()
					testM4aBFrames(t, runs["h264_pyramid.ts"].renditions[i], r.Video.Copy)
				})
				if r.Video.Copy {
					return
				}
				t.Run("steady-state", func(t *testing.T) {
					t.Parallel()
					steps := runs["h264_aac.ts"].renditions[i]
					reordered := assertHubTiming(t, stepFrames(steps), "h264_aac.ts")
					assert.Positive(t, reordered, "hub frames with PTS > DTS")
					t.Logf("%d hub frames with PTS > DTS", reordered)
					feedScenario(t, steps)
				})
			})
		}
	})
}

// videoStreamInfo is what ffprobe reports of a video stream.
type videoStreamInfo struct {
	Codec      string `json:"codec_name"`
	Profile    string `json:"profile"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	HasBFrames int    `json:"has_b_frames"`
}

// probeVideoStream runs ffprobe on u and returns its first video stream.
func probeVideoStream(t *testing.T, ffprobe, u string) videoStreamInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,profile,width,height,has_b_frames", "-of", "json", u).Output()
	require.NoError(t, err, u)
	var v struct {
		Streams []videoStreamInfo `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &v), string(out))
	require.Len(t, v.Streams, 1, "%s: one video stream; %s", u, out)
	return v.Streams[0]
}

// videoKbps is the bitrate of frames' video payload over its presentation
// span (last PTS − first PTS + the smallest step between PTS), in kbps.
func videoKbps(frames []media.Frame) int {
	video := videoFrames(frames)
	var size int
	pts := make([]int64, 0, len(video))
	for _, f := range video {
		for _, nal := range f.Data {
			size += len(nal)
		}
		pts = append(pts, f.PTS)
	}
	if len(pts) < 2 {
		return 0
	}
	slices.Sort(pts)
	step := pts[len(pts)-1] - pts[0]
	for i := 1; i < len(pts); i++ {
		if d := pts[i] - pts[i-1]; d > 0 && d < step {
			step = d
		}
	}
	secs := float64(pts[len(pts)-1]-pts[0]+step) / media.ClockRate
	return int(float64(size) * 8 / secs / 1000)
}

// TestM4cNVENCProbe transcodes h264_aac.ts through both NVENC ladders and
// runs ffprobe on each rung's HLS TS (packaged as probeSteps does): every
// NVENC rung is h264, High profile, 320x240, 240x180 and 160x120 for t240,
// t180 and t120, with has_b_frames 0 on the noB ladder and 1 on the B
// ladder; the copy rung reads as the source fixture does. Each NVENC rung is
// logged as one line, "nvenc-rung <ladder> <rung> <codec> <profile> <w>x<h>
// has_b_frames=<n> kbps=<payload kbps>", which scripts/test-nvenc.sh
// collects.
func TestM4cNVENCProbe(t *testing.T) {
	bin := nvencBinary(t)
	ffprobe := testpeer.Require(t, "ffprobe")
	srcPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	src := probeVideoStream(t, ffprobe, srcPath)
	widths := map[int]int{240: 320, 180: 240, 120: 160}
	for _, l := range []struct {
		name        string
		bframes, hb int
	}{{"noB", -1, 0}, {"B", 0, 1}} {
		t.Run(l.name, func(t *testing.T) {
			opt := nvencLadder(bin, l.bframes)
			run := serialRuns(t, opt, "h264_aac.ts")["h264_aac.ts"]
			for i, r := range opt.Renditions {
				steps := run.renditions[i]
				got := probeVideoStream(t, ffprobe, probeSteps(t, steps)+"hls/index.m3u8")
				kbps := videoKbps(stepFrames(steps))
				if r.Video.Copy {
					assert.Equal(t, src, got, "%s: the copy rung reads as the source", r.Name)
					t.Logf("copy-rung %s %s %s %s %dx%d has_b_frames=%d kbps=%d", l.name, r.Name, got.Codec,
						got.Profile, got.Width, got.Height, got.HasBFrames, kbps)
					continue
				}
				want := videoStreamInfo{
					Codec: "h264", Profile: "High", Width: widths[r.Video.Height], Height: r.Video.Height,
					HasBFrames: l.hb,
				}
				assert.Equal(t, want, got, r.Name)
				t.Logf("nvenc-rung %s %s %s %s %dx%d has_b_frames=%d kbps=%d", l.name, r.Name, got.Codec,
					got.Profile, got.Width, got.Height, got.HasBFrames, kbps)
			}
		})
	}
}

// nvencSessions sums nvidia-smi's encoder.stats.sessionCount over the GPUs.
func nvencSessions(ctx context.Context, smi string) (int, error) {
	out, err := exec.CommandContext(ctx, smi, "--query-gpu=encoder.stats.sessionCount",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi: %w", err)
	}
	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		v, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return 0, fmt.Errorf("nvidia-smi: session count %q: %w", line, err)
		}
		n += v
	}
	return n, nil
}

// sessionPoll is one reading of the NVENC session count.
type sessionPoll struct {
	at time.Time
	n  int
}

// pollSessions reads the session count every 250 ms until ctx ends, and
// returns a function that waits for the poller to stop and returns its
// readings and how many failed.
func pollSessions(ctx context.Context, smi string) func() ([]sessionPoll, int) {
	var (
		mu     sync.Mutex
		polls  []sessionPoll
		failed int
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			n, err := nvencSessions(ctx, smi)
			mu.Lock()
			if err == nil {
				polls = append(polls, sessionPoll{time.Now(), n})
			} else if ctx.Err() == nil {
				failed++
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() ([]sessionPoll, int) {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return polls, failed
	}
}

// TestM4cNVENCSessions feeds h264_aac.ts twice at its pace (12 s) through
// nvencLadder(bin, -1) while nvidia-smi's encoder.stats.sessionCount is
// polled every 250 ms: some poll before the feed ends reads base + 3, where
// base is the count before the run (0 unless OS_NVENC_SHARED=1), no poll
// reads more, and once the transcoder has stopped the count is base again
// within 10 s. It logs "nvenc-sessions base=… peak=… after=…", which
// scripts/test-nvenc.sh collects. Without nvidia-smi on PATH it fails under
// OS_REQUIRE_NVENC=1 and skips otherwise.
func TestM4cNVENCSessions(t *testing.T) {
	bin := nvencBinary(t)
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		if os.Getenv(testpeer.EnvRequireNVENC) == "1" {
			t.Fatalf("nvidia-smi not found (%s=1): %v", testpeer.EnvRequireNVENC, err)
		}
		t.Skipf("nvidia-smi not found: %v", err)
	}
	base, err := nvencSessions(t.Context(), smi)
	require.NoError(t, err)
	if base != 0 && os.Getenv(envNVENCShared) != "1" {
		t.Fatalf("%d NVENC sessions before the run; %s=1 shares the GPU with them", base, envNVENCShared)
	}
	opt := nvencLadder(bin, -1)
	rungs := 0
	for _, r := range opt.Renditions {
		if r.Video.Encoder == transcode.EncoderNVENC {
			rungs++
		}
	}
	steps := loopedFixture(t, "h264_aac.ts", 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := pollSessions(ctx, smi)
	var fedAt time.Time
	serialRun(t, opt, "h264_aac.ts*2", func(ctx context.Context, h *hub.Hub) error {
		err := pacedSteps(steps)(ctx, h)
		fedAt = time.Now()
		return err
	})
	cancel()
	polls, failed := stop()
	require.Zero(t, failed, "failed nvidia-smi polls")
	require.NotEmpty(t, polls)
	peak, peakBeforeEnd := 0, 0
	for _, p := range polls {
		peak = max(peak, p.n)
		if p.at.Before(fedAt) {
			peakBeforeEnd = max(peakBeforeEnd, p.n)
		}
	}
	assert.Equal(t, base+rungs, peakBeforeEnd, "the count read base + %d before the feed ended", rungs)
	assert.LessOrEqual(t, peak, base+rungs, "never more than base + %d", rungs)
	assert.Eventually(t, func() bool {
		n, err := nvencSessions(t.Context(), smi)
		return err == nil && n == base
	}, 10*time.Second, 250*time.Millisecond, "the count back to base %d after the run", base)
	after, err := nvencSessions(t.Context(), smi)
	require.NoError(t, err)
	t.Logf("nvenc-sessions base=%d peak=%d after=%d (%d polls, %d before the feed ended at %v)", base, peak, after,
		len(polls), countBefore(polls, fedAt), fedAt.UTC().Format(time.RFC3339))
}

// countBefore counts the polls taken before at.
func countBefore(polls []sessionPoll, at time.Time) int {
	n := 0
	for _, p := range polls {
		if p.at.Before(at) {
			n++
		}
	}
	return n
}

// TestM4cNVENCMixed transcodes h264_pyramid.ts through a mixed ladder,
// {t240 nvenc BFrames −1, t180 x264 (2 B-frames), t120 nvenc (2 B-frames),
// src copy} with the presentation: a clean run with EncoderFallbacks 0
// (serialRuns), the encoded rungs' keyframes the source's + D, and the
// presentation read back in ABR mode with no violation and every frame.
func TestM4cNVENCMixed(t *testing.T) {
	bin := nvencBinary(t)
	opt := m4aLadder(bin)
	opt.Renditions[0].Video.Encoder, opt.Renditions[0].Video.BFrames = transcode.EncoderNVENC, -1
	opt.Renditions[1].Video.Encoder = transcode.EncoderX264
	opt.Renditions[2].Video.Encoder = transcode.EncoderNVENC
	opt.Presentation = true
	run := serialRuns(t, opt, "h264_pyramid.ts")["h264_pyramid.ts"]
	assertNVENCLadder(t, opt, run, "h264_pyramid.ts")
}

// TestM4cNVENCEpochChangeABR transcodes h264_aac_then_640.ts, whose
// resolution changes mid-stream, through nvencLadder with B-frames (copy +
// three NVENC rungs, the presentation on), so that every NVENC encoder is
// reopened at the epoch change while the copy rung runs on. It validates the
// presentation merger's 2 s learned-hold opener against NVENC's reopen
// time: the run is clean with PresentationDropped 0 and EncoderFallbacks 0
// (serialRuns), and the presentation read back in ABR mode has every frame,
// epochs [1 2] in every format, one aligned discontinuity, and no rung left
// out of any segment (Packager.Rungs all zero).
func TestM4cNVENCEpochChangeABR(t *testing.T) {
	bin := nvencBinary(t)
	opt := nvencLadder(bin, 0)
	const fixture = "h264_aac_then_640.ts"
	run := serialRuns(t, opt, fixture)[fixture]
	steps := run.presentation
	p, base, caps := feedABR(t, steps, 0)
	assertCompleteABR(t, caps, stepFrames(steps))
	assert.Len(t, p.Snapshot().Epochs, 2)
	for _, f := range abrFormats {
		assert.Equal(t, []uint32{1, 2}, epochs(caps[f.name].Frames), f.name)
	}
	assert.Len(t, assertDiscontinuitiesAligned(t, base), 1)
	rungs := p.Rungs()
	assert.Len(t, rungs, len(opt.Renditions))
	for track, r := range rungs {
		assert.Zero(t, r, "rung %d left out", track)
	}
	t.Logf("rungs %+v", rungs)
}

// envNVENCFFmpeg names the ffmpeg TestM4cNVENCLongGOP makes its clip with:
// scripts/test-nvenc.sh sets it to the pinned GPL FFmpeg build
// (<prefix>/bin/ffmpeg, whose shared libraries are in <prefix>/lib), so
// that the clip does not depend on the host's ffmpeg. Unset, the ffmpeg on
// PATH is used.
const envNVENCFFmpeg = "OS_NVENC_FFMPEG"

// clipFFmpeg returns the ffmpeg of the long-GOP clip and the environment to
// run it in (nil: this process's), and skips the test when that ffmpeg has
// no libx264.
func clipFFmpeg(t *testing.T) (bin string, env []string) {
	t.Helper()
	if bin = os.Getenv(envNVENCFFmpeg); bin != "" {
		env = append(os.Environ(), "LD_LIBRARY_PATH="+filepath.Join(filepath.Dir(filepath.Dir(bin)), "lib"))
	} else {
		bin = testpeer.Require(t, "ffmpeg")
	}
	cmd := exec.CommandContext(t.Context(), bin, "-hide_banner", "-encoders")
	cmd.Env = env
	out, err := cmd.Output()
	require.NoError(t, err, "%s -encoders (%s=%q)", bin, envNVENCFFmpeg, os.Getenv(envNVENCFFmpeg))
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == "libx264" {
			return bin, env
		}
	}
	t.Skipf("%s has no libx264 encoder: the long-GOP clip cannot be made (%s=%q)", bin, envNVENCFFmpeg,
		os.Getenv(envNVENCFFmpeg))
	return "", nil
}

// longGOPClip writes, with ffmpeg (run in env) and libx264, a 30 s clip of 320x240 at
// 24 fps with a closed 48-frame GOP (720 pictures, 15 keyframes) and AAC
// stereo, and returns its bytes. 48 does not divide 600, the encoders' GOP,
// so a GOP counter that a forced IDR did not reset would put an IDR at
// picture 600, inside a source GOP.
func longGOPClip(t *testing.T, ffmpeg string, env []string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "longgop.ts")
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000",
		"-t", "30", "-c:v", "libx264", "-profile:v", "main", "-bf", "2", "-g", "48", "-keyint_min", "48",
		"-sc_threshold", "0", "-c:a", "aac", "-ac", "2", "-b:a", "64k", "-f", "mpegts", out)
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	return data
}

// TestM4cNVENCLongGOP transcodes longGOPClip (720 pictures at 24 fps, a
// keyframe every 48, made by clipFFmpeg's ffmpeg) at its pace through a mixed ladder, {t240 nvenc
// (2 B-frames), t180 x264, t120 nvenc BFrames −1}, with the presentation:
// a clean run with EncoderFallbacks 0, and on every rung one frame per
// source picture and the keyframe PTS set exactly the source's + D. That
// pins NVENC's 600-picture GOP counter restarting at every forced IDR (and
// x264's keyint=600): picture 600 is mid-GOP here, where the 25-frame GOPs
// of the fixtures put a source keyframe.
func TestM4cNVENCLongGOP(t *testing.T) {
	bin := nvencBinary(t)
	ffmpeg, env := clipFFmpeg(t)
	t.Logf("clip made with %s", ffmpeg)
	steps := dataSteps(t, longGOPClip(t, ffmpeg, env), newFakeClock(m3aT0).Now)
	src := videoFrames(stepFrames(steps))
	require.GreaterOrEqual(t, len(src), 700)
	var want []int64
	for _, f := range src {
		if f.Key {
			want = append(want, f.PTS+transcode.Delay)
		}
	}
	slices.Sort(want)
	require.Len(t, want, 15, "a keyframe every 48 pictures")
	const frame = media.ClockRate / 24
	for i := 1; i < len(want); i++ {
		require.EqualValues(t, 48*frame, want[i]-want[i-1], "keyframe %d is 48 pictures after the one before", i)
	}
	opt := transcode.Options{
		Binary: bin,
		Renditions: []transcode.Rendition{
			{Name: "t240", Video: transcode.Video{Height: 240, BitrateKbps: 600, Encoder: transcode.EncoderNVENC}},
			{Name: "t180", Video: transcode.Video{Height: 180, BitrateKbps: 300, Encoder: transcode.EncoderX264}},
			{Name: "t120", Video: transcode.Video{Height: 120, BitrateKbps: 150, Encoder: transcode.EncoderNVENC, BFrames: -1}},
		},
		Audio:        transcode.Audio{BitrateKbps: 96},
		Queue:        4096,
		Presentation: true,
	}
	run := serialRun(t, opt, "longgop", pacedSteps(steps))
	for i, r := range opt.Renditions {
		frames := stepFrames(run.renditions[i])
		assert.Len(t, videoFrames(frames), len(src), "%s: one frame per source picture", r.Name)
		got := keySet(frames)
		assert.Equal(t, want, got, "%s: the keyframe PTS set is the source's + D", r.Name)
		if r.Video.Encoder == transcode.EncoderNVENC {
			assertNVENCFrames(t, frames, r.Name)
		}
		t.Logf("%s: %d pictures, %d keyframes", r.Name, len(videoFrames(frames)), len(got))
	}
}

// TestM4cNVENCWatermarkThumbnails transcodes h264_aac.ts fed four times as
// one timeline (24 s) at its pace through m4cLadder(bin, "nvenc") with the
// image mark and Thumbnail{2, 120, 80}: a clean run with EncoderFallbacks
// 0 and no x264 SEI on any rung; on every rung the first picture's mark
// region is within 40 of magenta and its bottom right is not
// (TestM4cWatermarkEveryRung's rule); at least 11 shots, 2 s apart or
// more, each a 160×120 JPEG, as many as the children counted, none failed.
func TestM4cNVENCWatermarkThumbnails(t *testing.T) {
	bin := nvencBinary(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	var log shotLog
	opt := m4cLadder(bin, transcode.EncoderNVENC)
	opt.Watermark = imageMark()
	opt.Thumbnail = &picture.Thumbnail{IntervalSec: 2, Height: 120, Quality: 80}
	opt.OnShot = log.add
	run := recordTranscode(t, pacedSteps(loopedFixture(t, "h264_aac.ts", 4)), opt, nil)
	requireCleanRun(t, run.stats)
	require.Zero(t, run.stats.Child.EncoderFallbacks, "no rung fell back from NVENC: %+v", run.stats)
	t.Logf("stats %+v", run.stats)
	for i, r := range opt.Renditions {
		frames := stepFrames(run.renditions[i])
		requireVideoFirst(t, frames, r.Name)
		assertNVENCFrames(t, frames, r.Name)
		img := firstPicture(t, ffmpeg, frames)
		ok, c := marked(img, picture.TopLeft)
		assert.True(t, ok, "%s: the mark region is magenta: %v", r.Name, c)
		other, oc := marked(img, picture.BottomRight)
		assert.False(t, other, "%s: the bottom right is not: %v", r.Name, oc)
		t.Logf("%s: %v, mark %v, bottom right %v", r.Name, img.Bounds().Size(), c, oc)
	}
	shots := log.get()
	require.GreaterOrEqual(t, len(shots), 11)
	checkShots(t, shots, 160, 120, 0)
	assert.EqualValues(t, len(shots), run.stats.Child.Thumbnails, "the children's count is the shots OnShot saw")
	assert.Zero(t, run.stats.Child.ThumbnailsFailed)
}

// nvencMedia returns the absolute directory OS_NVENC_MEDIA names, or skips
// the test naming the variable when it is unset.
func nvencMedia(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(envNVENCMedia)
	if dir == "" {
		t.Skipf("%s is not set: no recorded NVENC presentation", envNVENCMedia)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join("..", "..", dir) // the tests run in internal/e2e
	}
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	return abs
}

// mediaFiles serves dir with the HLS content types for .m3u8 and .ts.
func mediaFiles(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Ext(r.URL.Path) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		case ".ts":
			w.Header().Set("Content-Type", "video/mp2t")
		}
		files.ServeHTTP(w, r)
	})
}

// writeHLSTree copies the ended ABR HLS-TS output at hlsBase (".../hls/")
// into dir as a static tree: index.m3u8, each v{track}.m3u8 and each
// segment, under the names the playlists use. Every variant playlist must
// be ended and list no gap.
//
// Warning: it first deletes every *.m3u8 and *.ts file in dir (an earlier
// tree), whatever wrote them. Point it only at a directory kept for this
// tree, such as dist/nvenc-report/media.
func writeHLSTree(t *testing.T, hlsBase, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, pat := range []string{"*.m3u8", "*.ts"} {
		old, err := filepath.Glob(filepath.Join(dir, pat))
		require.NoError(t, err)
		for _, f := range old {
			require.NoError(t, os.Remove(f))
		}
	}
	write := func(name string) []byte {
		b := get(t, hlsBase+name)
		require.NotEmpty(t, b, name)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o644))
		return b
	}
	mv, err := hls.ParseMultivariant(write("index.m3u8"))
	require.NoError(t, err)
	require.NotEmpty(t, mv.Variants)
	segs := 0
	for _, v := range mv.Variants {
		pl, err := hls.ParseMedia(write(v.URI))
		require.NoError(t, err, v.URI)
		require.True(t, pl.Ended, "%s is ended", v.URI)
		for _, s := range pl.Segments {
			require.False(t, s.Gap, "%s lists a gap, %s", v.URI, s.URI)
			write(s.URI)
			segs++
		}
	}
	t.Logf("wrote index.m3u8, %d variant playlists and %d segments to %s", len(mv.Variants), segs, dir)
}

// checkHLSTree reads the tree in dir from disk and checks that every URI
// its playlists list exists as a non-empty file. It returns the number of
// variants.
func checkHLSTree(t *testing.T, dir string) int {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, name)
		require.NotEmpty(t, b, name)
		return b
	}
	mv, err := hls.ParseMultivariant(read("index.m3u8"))
	require.NoError(t, err)
	for _, v := range mv.Variants {
		pl, err := hls.ParseMedia(read(v.URI))
		require.NoError(t, err, v.URI)
		require.NotEmpty(t, pl.Segments, v.URI)
		for _, s := range pl.Segments {
			read(s.URI)
		}
	}
	return len(mv.Variants)
}

// TestM4cNVENCKeep, when OS_NVENC_MEDIA names a directory, records the B
// ladder (nvencLadder(bin, 0)) over h264_aac.ts fed four times as one
// timeline (24 s; vlcPlays samples at 6 s, quits at 10 s and wants 4 s of
// video and audio) through serialRuns, and writes its presentation,
// packaged in ABR mode (feedABR), as a static ended HLS-TS tree
// (index.m3u8, v{track}.m3u8, segments) into the directory. Served from
// an http.FileServer, the tree read back with conform.ReadHLS
// (AlignedSequences, Once) has no violation, 4 video tracks and every
// frame, and every URI its playlists list is a non-empty file. Otherwise
// it skips.
func TestM4cNVENCKeep(t *testing.T) {
	bin := nvencBinary(t)
	dir := nvencMedia(t)
	const name = "h264_aac.ts*4"
	run := serialRuns(t, nvencLadder(bin, 0), name)[name]
	_, base, _ := feedABR(t, run.presentation, 0)
	writeHLSTree(t, base+"hls/", dir)
	variants := checkHLSTree(t, dir)
	assert.Equal(t, 4, variants)
	srv := httptest.NewServer(mediaFiles(dir))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := conform.ReadHLS(ctx, srv.URL+"/index.m3u8", conform.ReadOptions{AlignedSequences: true, Once: true})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	video, _ := byTrack(c.Frames)
	assert.Len(t, video, 4, "video tracks")
	assertCompleteABR(t, map[string]conform.Capture{"hls-ts": c}, stepFrames(run.presentation))
	t.Logf("read back %d frames, per epoch/track %s", len(c.Frames), trackCounts(c.Frames))
}

// TestM4cNVENCRecordedVLC needs no device: when OS_NVENC_MEDIA names a
// directory with index.m3u8 (TestM4cNVENCKeep's tree), it serves the
// directory from an http.FileServer and plays index.m3u8 and v0.m3u8 (the
// t240 NVENC rung, 2 B-frames) with VLC, by the pass rule of the HLS-TS
// rows (vlcPlays with tolerate true: R-T1c's start-up pair only; any
// decoder error, "buffer deadlock prevented" among them, fails); otherwise
// it skips naming OS_NVENC_MEDIA.
func TestM4cNVENCRecordedVLC(t *testing.T) {
	dir := nvencMedia(t)
	if _, err := os.Stat(filepath.Join(dir, "index.m3u8")); err != nil {
		t.Skipf("%s=%s has no recorded presentation: %v", envNVENCMedia, os.Getenv(envNVENCMedia), err)
	}
	vlc := testpeer.VLC(t, "adaptive", "ts", "avcodec")
	ffprobe := testpeer.Require(t, "ffprobe")
	t.Logf("VLC: %s", vlc.Version(t))
	srv := httptest.NewServer(mediaFiles(dir))
	t.Cleanup(srv.Close)
	for _, pl := range []string{"index.m3u8", "v0.m3u8"} {
		t.Run(pl, func(t *testing.T) {
			vlcPlays(t, vlc, ffprobe, srv.URL+"/"+pl, true)
		})
	}
}
