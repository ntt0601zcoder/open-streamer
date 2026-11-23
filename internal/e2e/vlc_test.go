package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
// The gap rows play a failover through a stretch without one kind:
// hls-fmp4-across-gap (A/V, then 6 s of video only, then A/V) samples VLC's
// stats when the stream switches back to A/V, when no audio after the gap
// exists yet, and 16 s later: the audio decoded count must grow by more
// than 6 s of audio, more than any audio from before the gap still queued
// in VLC could give. http-ts- and srt-audio-only-stretch (A/V, then 6 s of
// audio only, then A/V) check that audio is decoded through the stretch,
// while the TS carries no video and so no PCR on the video PID, and video
// after it.
func TestM3VLCClass(t *testing.T) {
	t.Parallel()
	for _, out := range []struct{ name, path string }{
		{"hls-fmp4", "/cmaf/master.m3u8"},
		{"hls-ts", "/hls/index.m3u8"},
		{"dash", "/cmaf/index.mpd"},
		{"http-ts", "/stream.ts"},
	} {
		t.Run(out.name, func(t *testing.T) {
			t.Parallel()
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
		t.Parallel()
		vlc := testpeer.VLC(t)
		ffmpeg := testpeer.Require(t, "ffmpeg")
		ffprobe := testpeer.Require(t, "ffprobe")
		r := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0), "")}})
		o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
		waitAV(t, r.h)
		vlcPlays(t, vlc, ffprobe, o.url())
	})
	t.Run("rtmp-play", func(t *testing.T) {
		t.Parallel()
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
	t.Run("hls-fmp4-across-gap", testVLCAcrossGap)
	t.Run("http-ts-audio-only-stretch", func(t *testing.T) { testVLCAudioOnlyStretch(t, false) })
	t.Run("srt-audio-only-stretch", func(t *testing.T) { testVLCAudioOnlyStretch(t, true) })
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
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	cmd := gst.Command(ctx, t, testpeer.Run{Interrupt: gstRun}, append([]string{"-v", "-e"}, pipeline...)...)
	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = pw, pw
	start := time.Now()
	require.NoError(t, cmd.Start())
	_ = pw.Close()
	n := map[string]int{}
	first := map[string]time.Duration{}
	var errLines []string
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, sink := range []string{"v", "a"} {
			if strings.Contains(line, "GstFakeSink:"+sink+": last-message = chain") {
				if n[sink] == 0 {
					first[sink] = time.Since(start)
				}
				n[sink]++
			}
		}
		if strings.Contains(line, "ERROR") {
			errLines = append(errLines, line)
		}
	}
	_ = pr.Close()
	werr := cmd.Wait()
	t.Logf("%s: buffers %v, first after %v, exit %v", gst, n, first, werr)
	for _, sink := range []string{"v", "a"} {
		require.Positive(t, n[sink], "sink %s got buffers", sink)
		assert.LessOrEqual(t, first[sink], gstWithin, "sink %s within %v", sink, gstWithin)
	}
	assert.Empty(t, errLines, "ERROR messages")
}

// testVLCAcrossGap plays the HLS fMP4 master of A/V, then 6 s of video
// only, then A/V, with the VLC pass rule, sampling stats as TestM3VLCClass
// describes.
func testVLCAcrossGap(t *testing.T) {
	t.Parallel()
	vlc := testpeer.VLC(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
	r, p, origin := gapSource(t, ffmpeg, 4)
	r.waitActive(t, 0, rtspReady)
	waitListed(t, p, 3)
	pair := startVLCPair(t, vlc, origin+"/live/s/cmaf/master.m3u8")
	r.waitActive(t, 1, 30*time.Second)
	s0, ok0 := pair.disp.stats(t)
	r.waitActive(t, 2, 30*time.Second)
	s1, ok1 := pair.disp.stats(t)
	time.Sleep(16 * time.Second)
	s2, ok2 := pair.disp.stats(t)
	t.Logf("stats at the switch to video only %+v (%v), back to A/V %+v (%v), 16 s later %+v (%v)",
		s0, ok0, s1, ok1, s2, ok2)
	pair.finish(t, ffprobe)
	require.True(t, ok1 && ok2, "VLC answered stats after the gap; log:\n%s", vlcErrors(pair.disp.log.String()))
	assert.Greater(t, float64(s2.audio-s1.audio), 6*aacPerSecond, "audio decoded after the gap")
	assert.Greater(t, s2.video, s1.video, "video decoded after the gap")
}

// testVLCAudioOnlyStretch plays HTTP-TS, or SRT when srt is set, of A/V,
// then 6 s of audio only, then A/V, with the VLC pass rule: audio is
// decoded through the stretch (at least 3 s of it between the switch to
// audio only and the switch back), and video after it (at least 3 s of it
// in the 8 s after the switch back). conform.ReadTS reads the same output
// clean.
func testVLCAudioOnlyStretch(t *testing.T, srt bool) {
	t.Parallel()
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
