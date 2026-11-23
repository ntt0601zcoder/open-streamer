package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmpplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtspplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/srtout"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
)

// TestM3VLCClass plays every output with a VLC-class client, the one Task 7's
// probe chose per output (see the M3b3 report): VLC 3 for HLS fMP4, HLS TS,
// DASH, HTTP-TS, SRT and RTMP play, GStreamer's rtspsrc for RTSP play and
// for the RTMP push-out played back from MediaMTX's RTSP (Ubuntu's VLC has
// no live555 module). The peers run on the host, or in the throwaway image
// that OS_PEER_IMAGE names (testpeer).
//
// The VLC pass rule: VLC (-vv -I rc --rc-fake-tty --stats --vout dummy
// --aout dummy) answers rc's stats command, 6 s after it started, with
// decoded video and audio block counts both above 0, and is told to quit
// 4 s later; its log shows the input opened and no "decoder error" or
// "cannot" line; and a dump of what it received (--sout
// '#duplicate{dst=display,dst=std{access=file,mux=ts,dst=<f>}}') has a
// video and an audio stream of at least 4 s each. Ruling from the probe:
// with that --sout, VLC 3.0.20's rc stats report 0 decoded blocks (the
// display chain's decoders belong to the stream output, not the input), so
// the counts come from a display instance run side by side with the dump
// instance, both from the same URL; both logs are checked.
//
// The GStreamer pass rule: gst-launch-1.0 -v -e with fakesink name=v
// silent=false and name=a silent=false, stopped by SIGINT after 12 s:
// each sink logs a buffer (last-message = chain) within 10 s, and no ERROR
// message. The exit code is never a pass.
//
// The VLC rows run one after the other (ruling R-T7b): VLC under CPU
// starvation logs its own "no reference clock" decoder errors. The
// GStreamer rows run in parallel once they are done.
//
// http-ts- and srt-audio-only-stretch (A/V, then 6 s of audio only, then
// A/V) check that audio is decoded through the stretch, while the TS
// carries no video and so no PCR on the video PID, and video after it.
//
// HLS fMP4 across a failover (ruling R-T7a): hls-fmp4-epoch-change (A/V,
// then 6 s of h264_aac_640.ts, then A/V: two discontinuities with new
// EXT-X-MAPs, no gap) is played by GStreamer through both. Across the
// video-only stretch of gapSource, no VLC-class client plays (the stream
// is proven by ffmpeg in TestHLSFFmpegAcrossGap): hls-fmp4-across-gap
// skips both clients, citing the evidence, and so does
// hls-fmp4-epoch-change for VLC. With OS_PEER_LIMITS=1 those rows instead
// probe the limitation and fail if a client no longer has it, so that an
// upgrade that fixes it shows.
func TestM3VLCClass(t *testing.T) {
	t.Parallel()
	for _, out := range []struct{ name, path string }{
		{"hls-fmp4", "/cmaf/master.m3u8"},
		{"hls-ts", "/hls/index.m3u8"},
		{"dash", "/cmaf/index.mpd"},
		{"http-ts", "/stream.ts"},
	} {
		t.Run(out.name, func(t *testing.T) {
			vlc := testpeer.VLC(t)
			ffmpeg := testpeer.Require(t, "ffmpeg")
			ffprobe := testpeer.Require(t, "ffprobe")
			r, p := liveRun(t, []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 4, 0), "")}, nil)
			origin := serveHTTP(t, "", map[string]*hub.Hub{"s": r.h}, map[string]*live.Packager{"s": p}, 0, nil)
			waitAV(t, r.h)
			if out.name != "http-ts" {
				// A window of five segments (10 s): VLC's DASH client starts
				// suggestedPresentationDelay (6 s) behind the live edge.
				waitListed(t, p, 5)
			}
			vlcPlays(t, vlc, ffprobe, origin+"/live/s"+out.path)
		})
	}
	t.Run("srt", func(t *testing.T) {
		vlc := testpeer.VLC(t)
		ffmpeg := testpeer.Require(t, "ffmpeg")
		ffprobe := testpeer.Require(t, "ffprobe")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
		waitAV(t, r.h)
		vlcPlays(t, vlc, ffprobe, o.url())
	})
	t.Run("rtmp-play", func(t *testing.T) {
		vlc := testpeer.VLC(t)
		ffmpeg := testpeer.Require(t, "ffmpeg")
		ffprobe := testpeer.Require(t, "ffprobe")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		p := servePlay(t, "", rtmpplay.Options{}, nil, map[string]*hub.Hub{"s": r.h})
		waitLive(t, p.srv, "s")
		vlcPlays(t, vlc, ffprobe, p.url())
	})
	t.Run("rtsp-play", func(t *testing.T) {
		t.Parallel()
		gst := testpeer.GStreamer(t, rtspElements...)
		ffmpeg := testpeer.Require(t, "ffmpeg")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		o := serveRTSP(t, "", rtspplay.Options{}, map[string]*hub.Hub{"s": r.h})
		waitAV(t, r.h)
		gstPlays(t, gst, rtspPipeline(o.url()))
	})
	t.Run("push-out", func(t *testing.T) {
		t.Parallel()
		mtxRTMP := testpeer.MediaMTX(t)
		mtxRTSP := testpeer.MediaMTXPull(t)
		gst := testpeer.GStreamer(t, rtspElements...)
		ffmpeg := testpeer.Require(t, "ffmpeg")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 4, 0), "")}})
		push, err := rtmppush.New(r.h, "rtmp://"+mtxRTMP+"/live/"+m3b3PushPath, rtmppush.Options{})
		require.NoError(t, err)
		runPusher(t, push)
		u := "rtsp://" + mtxRTSP + "/" + m3b3PushPath
		settleRTSP(t, u+"?transport=tcp")
		gstPlays(t, gst, rtspPipeline(u))
		t.Logf("push %+v", push.Stats())
	})
	t.Run("http-ts-audio-only-stretch", func(t *testing.T) { testVLCAudioOnlyStretch(t, false) })
	t.Run("srt-audio-only-stretch", func(t *testing.T) { testVLCAudioOnlyStretch(t, true) })
	t.Run("hls-fmp4-across-gap/vlc", func(t *testing.T) {
		peerLimit(t, vlcDiscontinuityLimit)
		probeVLCLimit(t, "h264_640.ts", 3)
	})
	t.Run("hls-fmp4-epoch-change/vlc", func(t *testing.T) {
		peerLimit(t, vlcDiscontinuityLimit)
		probeVLCLimit(t, "h264_aac_640.ts", 1)
	})
	t.Run("hls-fmp4-across-gap/gstreamer", func(t *testing.T) {
		t.Parallel()
		peerLimit(t, gstGapLimit)
		res := gstAcross(t, "h264_640.ts", 3)
		if res.err == nil {
			t.Errorf("GStreamer now plays HLS fMP4 across EXT-X-GAP entries: lift the skip (ruling R-T7a)")
		}
		t.Logf("the known limitation: %v", res.err)
	})
	t.Run("hls-fmp4-epoch-change/gstreamer", func(t *testing.T) {
		t.Parallel()
		res := gstAcross(t, "h264_aac_640.ts", 1)
		require.NoError(t, res.err)
	})
}

// envPeerLimits, set to "1", turns the skips of the VLC-class clients'
// known limitations into probes that fail once a client no longer has
// the limitation.
const envPeerLimits = "OS_PEER_LIMITS"

// The known limitations of the VLC-class clients (ruling R-T7a, evidence in
// the M3b3 Task 7 report, section 5.1).
const (
	vlcDiscontinuityLimit = "VLC 3.0.20 cannot play fMP4 HLS past any EXT-X-DISCONTINUITY: it drains, " +
		"recreates its MP4 demuxer without the init segment (adaptive demux error: Failed to create demuxer " +
		"(nil) MP4) and reaches EOF; reproduced with an ffmpeg-made fMP4 playlist with one discontinuity, " +
		"a VLC defect not specific to gaps (ruling R-T7a)"
	gstGapLimit = "GStreamer's hlsdemux2 and hlsdemux (1.24, 1.28) load consecutive EXT-X-GAP URIs, get the " +
		"404 the server answers for them and stop: 1.28 with Couldn't download fragments (http-status-code " +
		"404), 1.24 with no buffer after the gap (ruling R-T7a)"
)

// peerLimit skips the test, naming the limitation, unless OS_PEER_LIMITS=1.
func peerLimit(t *testing.T, limit string) {
	t.Helper()
	if os.Getenv(envPeerLimits) != "1" {
		t.Skipf("%s; %s=1 probes it", limit, envPeerLimits)
	}
}

// probeVLCLimit plays the HLS fMP4 master of failoverSource with mid (VLC's
// display instance, stats as for the gap rows: when the stream switches back
// to A/V and 16 s later) and expects vlcDiscontinuityLimit: no answer after
// the switch back, and the demuxer failure in the log. A VLC that plays on
// fails the test.
func probeVLCLimit(t *testing.T, mid string, midPasses int) {
	t.Helper()
	vlc := testpeer.VLC(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	r, p, origin := failoverSource(t, ffmpeg, mid, midPasses, 4)
	r.waitActive(t, 0, rtspReady)
	waitListed(t, p, 3)
	v := startVLC(t, vlc, nil, origin+"/live/s/cmaf/master.m3u8")
	r.waitActive(t, 1, 30*time.Second)
	r.waitActive(t, 2, 30*time.Second)
	s1, ok1 := v.stats(t)
	time.Sleep(16 * time.Second)
	s2, ok2 := v.stats(t)
	v.quit(t)
	log := v.log.String()
	t.Logf("%s: stats back to A/V %+v (%v), 16 s later %+v (%v); log:\n%s", vlc, s1, ok1, s2, ok2, vlcErrors(log))
	if ok2 && s2.video > s1.video && s2.audio > s1.audio {
		t.Errorf("%s now plays HLS fMP4 across discontinuities: lift the skip (ruling R-T7a)", vlc)
		return
	}
	assert.Contains(t, log, "Failed to create demuxer", "the known failure mode")
}

// hlsElements are the GStreamer elements of hlsPipeline.
var hlsElements = []string{"uridecodebin3", "hlsdemux2", "qtdemux", "avdec_h264", "avdec_aac", "queue", "fakesink"}

// hlsPipeline plays an HLS (or any) URL with uridecodebin3 into the sinks
// v and a.
func hlsPipeline(u string) []string {
	return []string{
		"uridecodebin3", "uri=" + u, "name=d",
		"d.", "!", "video/x-raw", "!", "queue", "!", "fakesink", "name=v", "silent=false",
		"d.", "!", "audio/x-raw", "!", "queue", "!", "fakesink", "name=a", "silent=false",
	}
}

// acrossResult is how GStreamer played across a failover; err says how it
// failed the pass rule, nil when it passed.
type acrossResult struct{ err error }

// gstAcross plays the HLS fMP4 master of failoverSource with mid with
// GStreamer, for 40 s from its start, and applies the GStreamer pass rule
// plus: each sink gets buffers later than 12 s after the stream switched
// back to A/V, when GStreamer, joined at the live edge, plays what came
// after the second failover.
func gstAcross(t *testing.T, mid string, midPasses int) acrossResult {
	t.Helper()
	gst := testpeer.GStreamer(t, hlsElements...)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	r, p, origin := failoverSource(t, ffmpeg, mid, midPasses, 4)
	r.waitActive(t, 0, rtspReady)
	waitListed(t, p, 3)
	ch := runGst(t, gst, hlsPipeline(origin+"/live/s/cmaf/master.m3u8"), 40*time.Second)
	r.waitActive(t, 1, 30*time.Second)
	r.waitActive(t, 2, 30*time.Second)
	back := time.Now()
	var res gstResult
	select {
	case res = <-ch:
	case <-time.After(runLimit):
		t.Fatal("gst-launch-1.0 did not end")
	}
	after := map[string]int{}
	for sink, ts := range res.times {
		for _, at := range ts {
			if at.After(back.Add(12 * time.Second)) {
				after[sink]++
			}
		}
	}
	t.Logf("%s: buffers %v, %v later than 12 s after the switch back to A/V; errors %q", gst,
		res.counts(), after, res.errLines)
	var errs []error
	if err := res.check(); err != nil {
		errs = append(errs, err)
	}
	for _, sink := range []string{"v", "a"} {
		if after[sink] == 0 {
			errs = append(errs, fmt.Errorf("sink %s: no buffer after the second failover", sink))
		}
	}
	return acrossResult{err: errors.Join(errs...)}
}

// gstResult is what one gst-launch-1.0 run logged.
type gstResult struct {
	start    time.Time
	times    map[string][]time.Time // when each sink logged a buffer
	errLines []string
	exit     error
}

func (r gstResult) counts() map[string]int {
	n := map[string]int{}
	for sink, ts := range r.times {
		n[sink] = len(ts)
	}
	return n
}

// check applies the GStreamer pass rule.
func (r gstResult) check() error {
	var errs []error
	for _, sink := range []string{"v", "a"} {
		ts := r.times[sink]
		switch {
		case len(ts) == 0:
			errs = append(errs, fmt.Errorf("sink %s got no buffer", sink))
		case ts[0].Sub(r.start) > gstWithin:
			errs = append(errs, fmt.Errorf("sink %s: first buffer after %v, over %v", sink, ts[0].Sub(r.start), gstWithin))
		}
	}
	if len(r.errLines) > 0 {
		errs = append(errs, fmt.Errorf("ERROR messages: %q", r.errLines))
	}
	return errors.Join(errs...)
}

// runGst runs gst-launch-1.0 -v -e with pipeline, stopped by SIGINT after
// run, in the background.
func runGst(t *testing.T, gst testpeer.Peer, pipeline []string, run time.Duration) <-chan gstResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	cmd := gst.Command(ctx, t, testpeer.Run{Interrupt: run}, append([]string{"-v", "-e"}, pipeline...)...)
	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = pw, pw
	res := gstResult{start: time.Now(), times: map[string][]time.Time{}}
	require.NoError(t, cmd.Start())
	_ = pw.Close()
	ch := make(chan gstResult, 1)
	go func() {
		defer cancel()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			for _, sink := range []string{"v", "a"} {
				if strings.Contains(line, "GstFakeSink:"+sink+": last-message = chain") {
					res.times[sink] = append(res.times[sink], time.Now())
				}
			}
			if strings.Contains(line, "ERROR") {
				res.errLines = append(res.errLines, line)
			}
		}
		_ = pr.Close()
		res.exit = cmd.Wait()
		ch <- res
	}()
	return ch
}

const (
	// vlcStatsAfter is when VLC's stats are read after it started, and
	// vlcQuitAfter when it is told to quit: VLC's DASH client starts about
	// 4 s in, and its dump of a 6 s run held 1.3 to 1.6 s of media.
	vlcStatsAfter = 6 * time.Second
	vlcQuitAfter  = 10 * time.Second
	// vlcMinDump is the least media of each kind a dump must hold.
	vlcMinDump = 4 * time.Second
	// gstRun is when gst-launch-1.0 gets SIGINT; gstWithin bounds the wait
	// for each sink's first buffer.
	gstRun    = 12 * time.Second
	gstWithin = 10 * time.Second
	// m3b3PushPath is the path of the RTSP MediaMTX that pulls, on demand,
	// the push-out's target on the RTMP MediaMTX (OS_MEDIAMTX_RTMP):
	// live/m3b3push (MTX_PATHS_M3B3PUSH_SOURCE).
	m3b3PushPath = "m3b3push"
	// aacPerSecond is the AAC frames of one second at 48 kHz.
	aacPerSecond = 48000.0 / 1024
)

// rtspElements are the GStreamer elements of rtspPipeline.
var rtspElements = []string{"rtspsrc", "rtph264depay", "rtpmp4gdepay", "avdec_h264", "avdec_aac", "fakesink"}

// rtspPipeline plays an H.264+AAC RTSP URL over TCP into the sinks v and a.
func rtspPipeline(u string) []string {
	return []string{
		"rtspsrc", "location=" + u, "protocols=tcp", "name=s",
		"s.", "!", "application/x-rtp,media=video", "!", "rtph264depay", "!", "avdec_h264", "!",
		"fakesink", "name=v", "silent=false",
		"s.", "!", "application/x-rtp,media=audio", "!", "rtpmp4gdepay", "!", "avdec_aac", "!",
		"fakesink", "name=a", "silent=false",
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p) //nolint:wrapcheck // bytes.Buffer never fails
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// vlcStats is one answer to rc's stats command.
type vlcStats struct{ video, audio int }

// vlcProc is a running cvlc with the rc interface on its stdin and stdout.
type vlcProc struct {
	stdin  io.WriteCloser
	blocks chan vlcStats
	log    lockedBuffer // stderr
	out    lockedBuffer // stdout
	done   chan struct{}
	err    error
}

// startVLC runs cvlc -vv -I rc --rc-fake-tty --stats --vout dummy --aout
// dummy with args until quit or the test's end; dirs are mounted for the
// image.
func startVLC(t *testing.T, vlc testpeer.Peer, dirs []string, args ...string) *vlcProc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	argv := make([]string, 0, 9+len(args))
	argv = append(argv, "-vv", "-I", "rc", "--rc-fake-tty", "--stats", "--vout", "dummy", "--aout", "dummy")
	cmd := vlc.Command(ctx, t, testpeer.Run{Dirs: dirs}, append(argv, args...)...)
	v := &vlcProc{blocks: make(chan vlcStats, 16), done: make(chan struct{})}
	var err error
	v.stdin, err = cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = &v.log
	require.NoError(t, cmd.Start())
	parsed := make(chan struct{})
	go func() {
		defer close(parsed)
		v.parse(stdout)
	}()
	go func() {
		<-parsed
		v.err = cmd.Wait()
		close(v.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-v.done
	})
	return v
}

// parse reads rc's output and sends each stats block to blocks.
func (v *vlcProc) parse(r io.Reader) {
	sc := bufio.NewScanner(r)
	var cur *vlcStats
	for sc.Scan() {
		line := sc.Text()
		_, _ = v.out.Write([]byte(line + "\n"))
		switch {
		case strings.Contains(line, "begin of statistical info"):
			cur = &vlcStats{}
		case cur != nil && strings.Contains(line, "end of statistical info"):
			select {
			case v.blocks <- *cur:
			default:
			}
			cur = nil
		case cur != nil && strings.Contains(line, "video decoded"):
			cur.video = statValue(line)
		case cur != nil && strings.Contains(line, "audio decoded"):
			cur.audio = statValue(line)
		}
	}
}

// statValue returns the number after the colon of a stats line, or -1.
func statValue(line string) int {
	_, v, ok := strings.Cut(line, ":")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return -1
	}
	return n
}

// stats sends rc's stats command and returns the answer; ok is false when
// none comes within 5 s (VLC answers nothing once its input has ended).
func (v *vlcProc) stats(t *testing.T) (vlcStats, bool) {
	t.Helper()
	if _, err := io.WriteString(v.stdin, "stats\n"); err != nil {
		return vlcStats{}, false
	}
	select {
	case s := <-v.blocks:
		return s, true
	case <-time.After(5 * time.Second):
		return vlcStats{}, false
	case <-v.done:
		return vlcStats{}, false
	}
}

// quit sends rc's quit command and waits for VLC to exit.
func (v *vlcProc) quit(t *testing.T) {
	t.Helper()
	_, _ = io.WriteString(v.stdin, "quit\n")
	_ = v.stdin.Close()
	select {
	case <-v.done:
	case <-time.After(20 * time.Second):
		t.Error("VLC did not quit")
	}
}

// vlcLogLines returns the lines of a VLC log that contain any of words,
// case-insensitively.
func vlcLogLines(log string, words ...string) []string {
	var out []string
	for line := range strings.SplitSeq(log, "\n") {
		l := strings.ToLower(line)
		for _, w := range words {
			if strings.Contains(l, w) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

// checkVLCLog checks the log part of the VLC pass rule; a failure shows the
// 8 lines before each offending line, with HTTP header lines left out.
func checkVLCLog(t *testing.T, name, log string) {
	t.Helper()
	assert.Contains(t, log, "successfully opened", "%s: the input opened", name)
	bad := vlcLogLines(log, "decoder error", "cannot")
	if !assert.Empty(t, bad, "%s: decoder errors", name) {
		var lines []string
		for line := range strings.SplitSeq(log, "\n") {
			if strings.HasPrefix(line, "[") {
				lines = append(lines, line)
			}
		}
		for i, line := range lines {
			if slices.Contains(bad, line) {
				t.Logf("%s: context of %q:\n%s", name, line, strings.Join(lines[max(0, i-8):i], "\n"))
				break
			}
		}
	}
}

// vlcErrors returns at most 20 error lines of a VLC log, for failure
// messages.
func vlcErrors(log string) string {
	lines := vlcLogLines(log, " error", "warning")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// dumpSpans returns the media each kind of a TS dump covers: the sum of the
// steps between consecutive timestamps of a stream up to 1 s (a jump, such
// as VLC's own timestamps on the first packets of an RTMP dump, adds
// nothing).
func dumpSpans(t *testing.T, ffprobe, file string) map[string]time.Duration {
	t.Helper()
	out, err := execOutput(t, ffprobe, "-v", "error", "-show_entries",
		"stream=index,codec_type:packet=stream_index,pts_time,dts_time", "-of", "json", file)
	require.NoError(t, err)
	var v struct {
		Streams []struct {
			Index int    `json:"index"`
			Type  string `json:"codec_type"`
		} `json:"streams"`
		Packets []struct {
			Stream int    `json:"stream_index"`
			PTS    string `json:"pts_time"`
			DTS    string `json:"dts_time"`
		} `json:"packets"`
	}
	require.NoError(t, json.Unmarshal(out, &v))
	kind := map[int]string{}
	for _, s := range v.Streams {
		kind[s.Index] = s.Type
	}
	times := map[string][]float64{}
	for _, p := range v.Packets {
		ts := p.DTS
		if ts == "" {
			ts = p.PTS
		}
		f, err := strconv.ParseFloat(ts, 64)
		if err != nil {
			continue
		}
		times[kind[p.Stream]] = append(times[kind[p.Stream]], f)
	}
	spans := map[string]time.Duration{}
	for k, ts := range times {
		var sum float64
		for i := 1; i < len(ts); i++ {
			if d := ts[i] - ts[i-1]; d > 0 && d <= 1 {
				sum += d
			}
		}
		spans[k] = time.Duration(sum * float64(time.Second))
	}
	return spans
}

// execOutput runs a host program and returns its stdout.
func execOutput(t *testing.T, name string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output() //nolint:wrapcheck // the caller reports it
}

// vlcPair is the two VLC instances of the pass rule, from one URL: one
// displays (its stats count decoded blocks), one also dumps.
type vlcPair struct {
	disp, dump *vlcProc
	file       string
	started    time.Time
}

func startVLCPair(t *testing.T, vlc testpeer.Peer, u string) *vlcPair {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "dump.ts")
	return &vlcPair{
		disp:    startVLC(t, vlc, nil, u),
		dump:    startVLC(t, vlc, []string{dir}, "--sout", "#duplicate{dst=display,dst=std{access=file,mux=ts,dst="+file+"}}", u),
		file:    file,
		started: time.Now(),
	}
}

// finish quits both instances and checks the log and dump parts of the
// pass rule.
func (p *vlcPair) finish(t *testing.T, ffprobe string) {
	t.Helper()
	p.disp.quit(t)
	p.dump.quit(t)
	checkVLCLog(t, "display", p.disp.log.String())
	checkVLCLog(t, "dump", p.dump.log.String())
	_, err := os.Stat(p.file)
	require.NoError(t, err, "the dump exists; dump log:\n%s", vlcErrors(p.dump.log.String()))
	spans := dumpSpans(t, ffprobe, p.file)
	assert.GreaterOrEqual(t, spans["video"], vlcMinDump, "video in the dump")
	assert.GreaterOrEqual(t, spans["audio"], vlcMinDump, "audio in the dump")
	t.Logf("dump: video %v, audio %v", spans["video"].Round(time.Millisecond), spans["audio"].Round(time.Millisecond))
}

// vlcPlays applies the VLC pass rule to u.
func vlcPlays(t *testing.T, vlc testpeer.Peer, ffprobe, u string) {
	t.Helper()
	p := startVLCPair(t, vlc, u)
	time.Sleep(time.Until(p.started.Add(vlcStatsAfter)))
	st, ok := p.disp.stats(t)
	time.Sleep(time.Until(p.started.Add(vlcQuitAfter)))
	p.finish(t, ffprobe)
	require.True(t, ok, "VLC answered stats; log:\n%s", vlcErrors(p.disp.log.String()))
	assert.Positive(t, st.video, "video decoded")
	assert.Positive(t, st.audio, "audio decoded")
	t.Logf("%s (%s): stats %+v", u, vlc, st)
}

// gstPlays applies the GStreamer pass rule to a pipeline.
func gstPlays(t *testing.T, gst testpeer.Peer, pipeline []string) {
	t.Helper()
	res := <-runGst(t, gst, pipeline, gstRun)
	first := map[string]time.Duration{}
	for sink, ts := range res.times {
		first[sink] = ts[0].Sub(res.start)
	}
	t.Logf("%s: buffers %v, first after %v, exit %v", gst, res.counts(), first, res.exit)
	require.NoError(t, res.check())
}

// testVLCAudioOnlyStretch plays HTTP-TS, or SRT when srt is set, of A/V,
// then 6 s of audio only, then A/V, with the VLC pass rule: audio is
// decoded through the stretch (at least 3 s of it between the switch to
// audio only and the switch back), and video after it (at least 3 s of it
// in the 8 s after the switch back). conform.ReadTS reads the same output
// clean.
func testVLCAudioOnlyStretch(t *testing.T, srt bool) {
	vlc := testpeer.VLC(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
	inputs := []string{
		fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 2, 0), ""),
		fileURL(audioOnlyClip(t, ffmpeg, 1), ""),
		fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 2, 0), ""),
	}
	r := startRun(t, runOptions{inputs: inputs, open: chainAtEOF(inputs...)})
	var u string
	if srt {
		u = serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h}).url()
	} else {
		u = serveHTTP(t, "", map[string]*hub.Hub{"s": r.h}, nil, 0, nil) + "/live/s/stream.ts"
	}
	r.waitActive(t, 0, rtspReady)
	waitAV(t, r.h)
	capCh := readTS(u)
	pair := startVLCPair(t, vlc, u)
	r.waitActive(t, 1, 30*time.Second)
	s0, ok0 := pair.disp.stats(t)
	r.waitActive(t, 2, 30*time.Second)
	s1, ok1 := pair.disp.stats(t)
	time.Sleep(8 * time.Second)
	s2, ok2 := pair.disp.stats(t)
	t.Logf("stats at the switch to audio only %+v (%v), back to A/V %+v (%v), 8 s later %+v (%v)",
		s0, ok0, s1, ok1, s2, ok2)
	pair.finish(t, ffprobe)
	require.True(t, ok0 && ok1 && ok2, "VLC answered stats; log:\n%s", vlcErrors(pair.disp.log.String()))
	assert.Greater(t, float64(s1.audio-s0.audio), 3*aacPerSecond, "audio decoded through the audio-only stretch")
	assert.Greater(t, s2.video-s1.video, 3*gopVideo, "video decoded after the stretch")
	r.check(t, []uint32{1, 2, 3})
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	t.Logf("hub %s; conform %s; violations %v", kindCounts(r.col.frames), kindCounts(c.Frames), c.Violations)
}
