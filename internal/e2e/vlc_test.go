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
// The VLC rows run one after the other (ruling R-T7b); the GStreamer rows
// are TestM3GStreamerClass, which overlaps them. On the HLS TS, HLS fMP4
// and DASH rows, VLC 3.0.20 may reset its clock at the live start and then
// log two decoder errors ("Timestamp conversion failed for N: no reference
// clock", "Could not convert timestamp 0 for FFmpeg"). Measured in the
// 30-run A/B of R-F3: on HLS fMP4 in 7/30 runs before it and 6/30 after,
// on HLS TS in 0/30 before and 1/30 after (R-F3 added one reset there).
// DASH was not in that A/B; with R-F3 it showed the pair in 2 of 3 runs.
// Ruling R-T1c tolerates on those rows one such pair right after each
// reset, at most 4 per VLC process (vlcBadLines). Any other decoder error
// fails.
//
// http-ts- and srt-audio-only-stretch (A/V, then 6 s of audio only, then
// A/V) check that audio is decoded through the stretch, while the TS
// carries no video and so no PCR on the video PID, and video after it
// (testVLCAudioOnlyStretch).
//
// HLS fMP4 across a failover (ruling R-T7a): VLC 3 cannot play past any
// fMP4 discontinuity, so hls-fmp4-across-gap/vlc and
// hls-fmp4-epoch-change/vlc skip, citing the evidence; the GStreamer rows
// are in TestM3GStreamerClass. With OS_PEER_LIMITS=1 the skipped rows
// instead probe the limitation and fail if the client no longer has it, so
// that an upgrade that fixes it shows.
func TestM3VLCClass(t *testing.T) {
	t.Parallel()
	for _, out := range []struct {
		name, path string
		modules    []string
		tolerate   bool // VLC's start-up pair (ruling R-T1c)
	}{
		{"hls-fmp4", "/cmaf/master.m3u8", []string{"adaptive", "mp4", "avcodec"}, true},
		{"hls-ts", "/hls/index.m3u8", []string{"adaptive", "ts", "avcodec"}, true},
		{"dash", "/cmaf/index.mpd", []string{"adaptive", "mp4", "avcodec"}, true},
		{"http-ts", "/stream.ts", []string{"http", "ts", "avcodec"}, false},
	} {
		t.Run(out.name, func(t *testing.T) {
			vlc := testpeer.VLC(t, out.modules...)
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
			vlcPlays(t, vlc, ffprobe, origin+"/live/s"+out.path, out.tolerate)
		})
	}
	t.Run("srt", func(t *testing.T) {
		vlc := testpeer.VLC(t, "access_srt", "ts", "avcodec")
		ffmpeg := testpeer.Require(t, "ffmpeg")
		ffprobe := testpeer.Require(t, "ffprobe")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
		waitAV(t, r.h)
		vlcPlays(t, vlc, ffprobe, o.url(), false)
	})
	t.Run("rtmp-play", func(t *testing.T) {
		vlc := testpeer.VLC(t, "avio", "avcodec")
		ffmpeg := testpeer.Require(t, "ffmpeg")
		ffprobe := testpeer.Require(t, "ffprobe")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		p := servePlay(t, "", rtmpplay.Options{}, nil, map[string]*hub.Hub{"s": r.h})
		waitLive(t, p.srv, "s")
		vlcPlays(t, vlc, ffprobe, p.url(), false)
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
}

// TestM3GStreamerClass plays the outputs whose VLC-class client is
// GStreamer, with the GStreamer pass rule (see TestM3VLCClass): RTSP play
// over TCP, the RTMP push-out played back from MediaMTX's RTSP, and HLS
// fMP4 across an epoch change with no gap (A/V, then 6 s of
// h264_aac_640.ts, then A/V: two discontinuities with new EXT-X-MAPs),
// where each sink must also get buffers after the second failover. Across
// the video-only stretch of gapSource GStreamer loads the EXT-X-GAP URIs and
// stops (ruling R-T7a): that row skips, or with OS_PEER_LIMITS=1 probes the
// limitation. It runs alongside TestM3VLCClass's serial VLC rows.
func TestM3GStreamerClass(t *testing.T) {
	t.Parallel()
	t.Run("rtsp-play", func(t *testing.T) {
		t.Parallel()
		gst := testpeer.GStreamer(t, rtspElements...)
		ffmpeg := testpeer.Require(t, "ffmpeg")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		o := serveRTSP(t, "", rtspplay.Options{}, map[string]*hub.Hub{"s": r.h})
		waitAV(t, r.h)
		// Logged when the row fails too: whether a session played, and how
		// it ended, tells a stalled player from a silent server.
		defer func() { t.Logf("play %+v", o.srv.Stats()) }()
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
	t.Run("hls-fmp4-across-gap/gstreamer", func(t *testing.T) {
		t.Parallel()
		peerLimit(t, gstGapLimit)
		res := gstAcross(t, "h264_640.ts", 3)
		if res.err == nil {
			t.Errorf("GStreamer now plays HLS fMP4 across EXT-X-GAP entries: lift the skip (ruling R-T7a)")
			return
		}
		// 1.28 reports the failed gap download; 1.24 stops delivering.
		download := slices.ContainsFunc(res.errLines, func(l string) bool { return strings.Contains(l, "Couldn't download") })
		stalled := res.after["v"] == 0 && res.after["a"] == 0
		assert.True(t, download || stalled, "the known failure mode: %v", res.err)
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
	vlc := testpeer.VLC(t, "adaptive", "mp4", "avcodec")
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
// Its two caps-filtered delayed links may share rtspPipeline's race: check a
// "no buffers, killed" failure for "Delayed linking failed" first.
func hlsPipeline(u string) []string {
	return []string{
		"uridecodebin3", "uri=" + u, "name=d",
		"d.", "!", "video/x-raw", "!", "queue", "!", "fakesink", "name=v", "silent=false",
		"d.", "!", "audio/x-raw", "!", "queue", "!", "fakesink", "name=a", "silent=false",
	}
}

// acrossResult is how GStreamer played across a failover: err says how it
// failed the pass rule, nil when it passed; after counts each sink's
// buffers after the second failover; errLines are its ERROR lines.
type acrossResult struct {
	err      error
	after    map[string]int
	errLines []string
}

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
	t.Logf("%s: buffers %v, %v later than 12 s after the switch back to A/V; errors %q, link lines %q", gst,
		res.counts(), after, res.errLines, res.linkLines)
	var errs []error
	if err := res.check(); err != nil {
		errs = append(errs, err)
	}
	for _, sink := range []string{"v", "a"} {
		if after[sink] == 0 {
			errs = append(errs, fmt.Errorf("sink %s: no buffer after the second failover", sink))
		}
	}
	return acrossResult{err: errors.Join(errs...), after: after, errLines: res.errLines}
}

// gstResult is what one gst-launch-1.0 run logged.
type gstResult struct {
	start    time.Time
	times    map[string][]time.Time // when each sink logged a buffer
	errLines []string
	// linkLines are its lines that match gstLinkMarks: logged, never
	// judged.
	linkLines []string
	exit      error
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

// gstLinkMarks are the lines of gst-launch-1.0 worth logging with a
// failure, as diagnostics only: the pass rule does not look at them, since
// an SDP that lacks a track or names a wrong encoding, and a track whose
// RTP never comes, print them too.
//
// gst-launch's delayed links (the "s. ! ..." of a pipeline) are not safe
// against pads added from two threads at once, which rtspsrc does when both
// tracks' first packets come together, as in a TCP session's GOP replay:
// each pad-added runs every pending delayed link on its own thread. A
// delayed link with a caps filter is two links through a capsfilter of its
// own, the second made even when the first failed. The thread of the video
// pad tries the audio link before the audio pad is there: no pad for its
// filter, but its filter takes the depayloader's sink. The thread of the
// audio pad links that pad to its own filter, finds the depayloader's sink
// taken, and undoes; the first undoes too. No pad-added follows and the pad
// stays unlinked ("Delayed linking failed"): its sink never prerolls, which
// holds the other sink in preroll, so neither gets a buffer and gst-launch
// -e waits for an EOS that cannot come until it is killed. With GStreamer
// 1.24.2 that was 4 of 1440 runs of rtspPipeline when it had caps filters
// and none of 3360 without them, one of which crashed gst-launch instead
// ("Caught SIGSEGV").
var gstLinkMarks = []string{"Delayed linking failed", "Caught SIGSEGV"}

// gstLinkLine reports whether a line of gst-launch-1.0 matches gstLinkMarks.
func gstLinkLine(line string) bool {
	for _, m := range gstLinkMarks {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
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
			if gstLinkLine(line) {
				res.linkLines = append(res.linkLines, line)
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
)

// rtspElements are the GStreamer elements of rtspPipeline.
var rtspElements = []string{"rtspsrc", "rtph264depay", "rtpmp4gdepay", "avdec_h264", "avdec_aac", "fakesink"}

// rtspPipeline plays an H.264+AAC RTSP URL over TCP into the sinks v and a.
// It links rtspsrc straight to the depayloaders, whose sink templates pick
// the pads (encoding-name H264 for v, MPEG4-GENERIC for a), without caps
// filters: gst-launch's caps-filtered delayed links race when rtspsrc adds
// its pads from two threads (gstLinkMarks).
func rtspPipeline(u string) []string {
	return []string{
		"rtspsrc", "location=" + u, "protocols=tcp", "name=s",
		"s.", "!", "rtph264depay", "!", "avdec_h264", "!", "fakesink", "name=v", "silent=false",
		"s.", "!", "rtpmp4gdepay", "!", "avdec_aac", "!", "fakesink", "name=a", "silent=false",
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

// vlcStats is one answer to rc's stats command: decoded video and audio
// blocks, and audio buffers played and lost.
type vlcStats struct{ video, audio, played, lost int }

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
		case cur != nil && strings.Contains(line, "buffers played"):
			cur.played = statValue(line)
		case cur != nil && strings.Contains(line, "buffers lost"):
			cur.lost = statValue(line)
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
// none comes within 5 s (VLC answers nothing once its input has ended). An
// answer that came after an earlier call gave up is dropped first, so that
// it is not taken for this one.
func (v *vlcProc) stats(t *testing.T) (vlcStats, bool) {
	t.Helper()
	for drained := false; !drained; {
		select {
		case <-v.blocks:
		default:
			drained = true
		}
	}
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

// VLC's live-start clock reset and the decoder error pair that may follow
// it on an adaptive output, HLS or DASH (ruling R-T1c), as VLC 3.0.20 logs
// them after the "[<object>] " prefix. The reset is the PCR line, then the reset
// line; the decoder may log its first picture before the pair.
const (
	vlcPCRLate      = "main input error: ES_OUT_SET_(GROUP_)PCR  is called too late (pts_delay increased to "
	vlcPCRReset     = "main input debug: ES_OUT_RESET_PCR called"
	vlcFirstPicture = "main decoder debug: Received first picture"
	vlcNoClock      = "main decoder error: Timestamp conversion failed for "
	vlcNoClockEnd   = ": no reference clock"
	vlcNoConvert    = "main decoder error: Could not convert timestamp 0 for FFmpeg"
)

// vlcEntry is one line of a VLC log: msg is the line after its "[<object>] "
// prefix; a line without one (a continuation, such as an HTTP header) is
// not a message.
type vlcEntry struct {
	raw, msg string
	message  bool
}

func vlcEntries(log string) []vlcEntry {
	var out []vlcEntry
	for line := range strings.SplitSeq(log, "\n") {
		e := vlcEntry{raw: line, msg: line}
		if strings.HasPrefix(line, "[") {
			if _, msg, ok := strings.Cut(line, "] "); ok {
				e.msg, e.message = msg, true
			}
		}
		out = append(out, e)
	}
	return out
}

// isNoClock reports whether msg is the first line of the tolerated pair.
func isNoClock(msg string) bool {
	if !strings.HasPrefix(msg, vlcNoClock) || !strings.HasSuffix(msg, vlcNoClockEnd) {
		return false
	}
	_, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(msg, vlcNoClock), vlcNoClockEnd), 10, 64)
	return err == nil
}

// isPCRLate reports whether msg is VLC's PCR-late line, with any delay.
func isPCRLate(msg string) bool {
	if !strings.HasPrefix(msg, vlcPCRLate) || !strings.HasSuffix(msg, " ms)") {
		return false
	}
	_, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(msg, vlcPCRLate), " ms)"), 10, 64)
	return err == nil
}

// vlcMaxTolerated bounds the start-up pairs tolerated per VLC process: VLC
// 3.0.20 was seen to reset its clock up to 4 times in its first seconds.
const vlcMaxTolerated = 4

// vlcBadLines returns the lines of one VLC process's log that fail the
// log rule (that contain "decoder error" or "cannot", case-insensitively),
// and how many start-up pairs it tolerated. When tolerate is set, ruling
// R-T1c tolerates the pair vlcNoClock…vlcNoClockEnd, vlcNoConvert, as
// consecutive messages, right after each of the process's live-start clock
// resets: the messages before the pair are vlcPCRLate, vlcPCRReset and at
// most one vlcFirstPicture, so one pair per reset, and at most
// vlcMaxTolerated pairs. VLC logs nothing when it answers rc's stats, and
// its log and the rc output are separate streams, so no start-up window
// bounds the pairs beyond that count. A pair without its own reset, a
// second pair after one reset, a fifth pair, or any other error is bad.
func vlcBadLines(log string, tolerate bool) (bad []string, tolerated int) {
	es := vlcEntries(log)
	var msgs []int // indexes of the messages in es
	for i, e := range es {
		if e.message {
			msgs = append(msgs, i)
		}
	}
	at := func(k int) string {
		if k < 0 || k >= len(msgs) {
			return ""
		}
		return es[msgs[k]].msg
	}
	skip := map[int]bool{}
	for k := range msgs {
		if !tolerate || tolerated >= vlcMaxTolerated || !isNoClock(at(k)) || at(k+1) != vlcNoConvert {
			continue
		}
		j := k - 1
		if at(j) == vlcFirstPicture {
			j--
		}
		if at(j) == vlcPCRReset && isPCRLate(at(j-1)) {
			tolerated++
			skip[msgs[k]], skip[msgs[k+1]] = true, true
		}
	}
	for i, e := range es {
		l := strings.ToLower(e.raw)
		if !skip[i] && (strings.Contains(l, "decoder error") || strings.Contains(l, "cannot")) {
			bad = append(bad, e.raw)
		}
	}
	return bad, tolerated
}

// checkVLCLog checks the log part of the VLC pass rule on one process's
// log, tolerating its start-up pair when tolerate is set (vlcBadLines); a
// failure shows the 8 messages before the first offending line.
func checkVLCLog(t *testing.T, name, log string, tolerate bool) {
	t.Helper()
	assert.Contains(t, log, "successfully opened", "%s: the input opened", name)
	bad, tolerated := vlcBadLines(log, tolerate)
	if tolerated > 0 {
		t.Logf("R-T1c: %s: tolerated %d of VLC's start-up pairs, each after its own clock reset", name, tolerated)
	}
	if !assert.Empty(t, bad, "%s: decoder errors", name) {
		t.Logf("%s: clock lines of the log:\n%s", name, strings.Join(vlcLogLines(log, "es_out_reset_pcr", "called too late"), "\n"))
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
	// tolerate allows each instance a start-up pair after each of its
	// clock resets, at most vlcMaxTolerated (ruling R-T1c): set on the
	// HLS TS, HLS fMP4 and DASH rows only.
	tolerate bool
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
	checkVLCLog(t, "display", p.disp.log.String(), p.tolerate)
	checkVLCLog(t, "dump", p.dump.log.String(), p.tolerate)
	_, err := os.Stat(p.file)
	require.NoError(t, err, "the dump exists; dump log:\n%s", vlcErrors(p.dump.log.String()))
	spans := dumpSpans(t, ffprobe, p.file)
	assert.GreaterOrEqual(t, spans["video"], vlcMinDump, "video in the dump")
	assert.GreaterOrEqual(t, spans["audio"], vlcMinDump, "audio in the dump")
	t.Logf("dump: video %v, audio %v", spans["video"].Round(time.Millisecond), spans["audio"].Round(time.Millisecond))
}

// vlcPlays applies the VLC pass rule to u; tolerate is set on the HLS TS,
// HLS fMP4 and DASH rows (ruling R-T1c).
func vlcPlays(t *testing.T, vlc testpeer.Peer, ffprobe, u string, tolerate bool) {
	t.Helper()
	p := startVLCPair(t, vlc, u)
	p.tolerate = tolerate
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
	t.Logf("%s: buffers %v, first after %v, exit %v, link lines %q", gst, res.counts(), first, res.exit, res.linkLines)
	require.NoError(t, res.check())
}

// testVLCAudioOnlyStretch plays HTTP-TS, or SRT when srt is set, of A/V,
// then 6 s of audio only, then A/V, with the VLC pass rule, and samples
// VLC's stats 3 s after VLC started and when the stream switches to audio
// only (the A/V phase: its audio block rate), 2 s after that switch and when
// the stream switches back (the window: VLC, about 1 s behind the server,
// is past the A/V before the stretch and cannot have decoded the video
// after it), and 8 s after the switch back. In the window fewer video
// blocks than one GOP's frames are decoded, which proves it inside the
// stretch, while at least half the A/V phase's audio block rate is decoded
// and audio buffers are played; after the switch back, video is decoded
// again (at least 3 s of it). conform.ReadTS reads the same output clean.
func testVLCAudioOnlyStretch(t *testing.T, srt bool) {
	modules := []string{"http", "ts", "avcodec"}
	if srt {
		modules = []string{"access_srt", "ts", "avcodec"}
	}
	vlc := testpeer.VLC(t, modules...)
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
	type sample struct {
		at time.Time
		s  vlcStats
		ok bool
	}
	take := func() sample {
		st, ok := pair.disp.stats(t)
		return sample{time.Now(), st, ok}
	}
	time.Sleep(time.Until(pair.started.Add(3 * time.Second)))
	avStart := take()
	r.waitActive(t, 1, 30*time.Second)
	avEnd := take()
	time.Sleep(time.Until(avEnd.at.Add(2 * time.Second)))
	win0 := take()
	r.waitActive(t, 2, 30*time.Second)
	win1 := take()
	time.Sleep(8 * time.Second)
	after := take()
	t.Logf("stats: A/V phase %+v -> %+v; window %+v -> %+v (%v); 8 s after the switch back %+v",
		avStart.s, avEnd.s, win0.s, win1.s, win1.at.Sub(win0.at).Round(time.Millisecond), after.s)
	pair.finish(t, ffprobe)
	for _, smp := range []sample{avStart, avEnd, win0, win1, after} {
		require.True(t, smp.ok, "VLC answered stats; log:\n%s", vlcErrors(pair.disp.log.String()))
	}
	rate := float64(avEnd.s.audio-avStart.s.audio) / avEnd.at.Sub(avStart.at).Seconds()
	window := win1.at.Sub(win0.at).Seconds()
	require.Positive(t, rate, "audio decoded in the A/V phase")
	require.Greater(t, window, 2.0, "a window of more than 2 s inside the stretch")
	assert.Less(t, win1.s.video-win0.s.video, gopVideo, "no video decoded in the window: it is inside the stretch")
	assert.GreaterOrEqual(t, float64(win1.s.audio-win0.s.audio), 0.5*rate*window,
		"audio decoded through the stretch at half the A/V phase's rate (%.1f blocks/s) or more", rate)
	assert.Positive(t, win1.s.played-win0.s.played, "audio buffers played through the stretch")
	assert.Greater(t, after.s.video-win1.s.video, 3*gopVideo, "video decoded after the stretch")
	r.check(t, []uint32{1, 2, 3})
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	t.Logf("hub %s; conform %s; violations %v", kindCounts(r.col.frames), kindCounts(c.Frames), c.Violations)
}

// TestVLCLogTolerance checks the matcher of ruling R-T1c on log excerpts
// shaped like VLC 3.0.20's, one log per VLC process: a start-up pair is
// tolerated right after each clock reset, at most 4 per process, on the
// HLS TS, HLS fMP4 and DASH rows; a pair without its own reset, two pairs
// after one reset, a fifth pair, a pair on a strict row (HTTP-TS and every
// other non-adaptive row), or another error fails the log.
// VLC logs no marker for rc's stats answer, so no start-up window is
// checked.
func TestVLCLogTolerance(t *testing.T) {
	t.Parallel()
	const (
		opened  = "[00007a75bc000c90] main input debug: `http://127.0.0.1:1/live/s/cmaf/index.mpd' successfully opened"
		segment = "[00007a75b8001150] adaptive demux debug: Retrieving http://127.0.0.1:1/live/s/cmaf/0-6.m4s"
		header  = "Accept-Encoding: deflate, gzip"
		late    = "[00007aa760000c90] main input error: ES_OUT_SET_(GROUP_)PCR  is called too late (pts_delay increased to 1000 ms)"
		reset   = "[00007aa760000c90] main input debug: ES_OUT_RESET_PCR called"
		picture = "[00007aa75c0f0380] main decoder debug: Received first picture"
		noClock = "[00007aa75c0f0380] main decoder error: Timestamp conversion failed for 5520001: no reference clock"
		convert = "[00007aa75c0f0380] main decoder error: Could not convert timestamp 0 for FFmpeg"
		other   = "[00007aa75c0f0380] main decoder error: buffer deadlock prevented"
		cannot  = "[00007aa75c0f0380] main stream error: cannot pre fill buffer"
	)
	lateAt := func(ms string) string { return strings.Replace(late, "1000 ms", ms+" ms", 1) }
	startup := []string{late, reset, picture, noClock, convert}
	pair := []string{noClock, convert}
	join := func(parts ...[]string) string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return strings.Join(all, "\n")
	}
	// The four resets of the failing hls-fmp4 run (task 7 report, section
	// 12), with pairs after resets 1 and 3.
	fourResets := join(
		[]string{opened, late, reset, picture}, pair, []string{segment},
		[]string{lateAt("1029"), reset, segment},
		[]string{lateAt("1064"), reset}, pair, []string{segment},
		[]string{lateAt("1071"), reset, segment},
	)
	fivePairs := make([][]string, 0, 15)
	for range 5 {
		fivePairs = append(fivePairs, []string{late, reset}, pair, []string{segment})
	}
	for _, tc := range []struct {
		name      string
		log       string
		tolerate  bool
		bad       int
		tolerated int
	}{
		{"clean", join([]string{opened, segment, header}), true, 0, 0},
		{"after the reset", join([]string{opened, segment}, startup, []string{segment}), true, 0, 1},
		{"reset without first picture", join([]string{opened, late, reset}, pair), true, 0, 1},
		{"pcr-late line with another delay", join([]string{opened, lateAt("2033"), reset, picture}, pair), true, 0, 1},
		{"four resets, pairs after 1 and 3", fourResets, true, 0, 2},
		{"a pair after each of two resets", join([]string{opened}, startup, []string{segment}, startup), true, 0, 2},
		{"four pairs", join(append([][]string{{opened}}, fivePairs[:12]...)...), true, 0, 4},
		{"five pairs", join(append([][]string{{opened}}, fivePairs...)...), true, 2, 4},
		{"two pairs after one reset", join([]string{opened}, startup, []string{segment}, pair), true, 2, 1},
		{"a pair without its own reset", join([]string{opened}, startup, []string{segment, picture}, pair), true, 2, 1},
		{"without any reset", join([]string{opened, segment, picture}, pair), true, 2, 0},
		{"reset without the pcr-late line", join([]string{opened, segment, reset, picture}, pair), true, 2, 0},
		{"a message between the reset and the pair", join([]string{opened, late, reset, segment}, pair), true, 2, 0},
		{"a message inside the pair", join([]string{opened, late, reset, picture, noClock, segment, convert}), true, 2, 0},
		{"half the pair", join([]string{opened, late, reset, picture, noClock}), true, 1, 0},
		{"a strict row (http-ts)", join([]string{opened}, startup), false, 2, 0},
		{"with another error", join([]string{opened}, startup, []string{other}), true, 1, 1},
		{"with a cannot line", join([]string{opened}, startup, []string{cannot}), true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad, tolerated := vlcBadLines(tc.log, tc.tolerate)
			assert.Len(t, bad, tc.bad, "bad lines %q", bad)
			assert.Equal(t, tc.tolerated, tolerated)
		})
	}
	// A pair on each instance of a row (display and dump): each process
	// has its own allowance.
	t.Run("a pair on each instance", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"display", "dump"} {
			bad, tolerated := vlcBadLines(join([]string{opened, segment}, startup), true)
			assert.Empty(t, bad, name)
			assert.Equal(t, 1, tolerated, name)
		}
	})
}
