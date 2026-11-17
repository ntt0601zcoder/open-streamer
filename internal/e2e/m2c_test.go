package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp/rtsptest"
)

// The M2c scenarios feed the RTSP pull input from independent peers (ffmpeg
// publishing into MediaMTX, which our client then plays) and from the
// scripted rtsptest server, through a pipeline into a hub, and check the
// frames that a hub subscriber collects.

// rtspReady bounds the wait for a MediaMTX pull to deliver: a path is ready
// once its publisher's tracks are known, 2 to 2.5 s after it connects.
const (
	rtspReady = 8 * time.Second
	spanH264  = 32 * time.Second
)

// ffmpegRTSP returns ffmpeg once it is known to publish RTSP over TCP.
func ffmpegRTSP(t *testing.T) string {
	t.Helper()
	ffmpeg := testpeer.Require(t, "ffmpeg", "tcp")
	testpeer.RequireMuxer(t, ffmpeg, "rtsp")
	return ffmpeg
}

// loopClip writes the fixture name repeated passes times, cut to span when
// that is positive, into a temporary file and returns its path (see
// liveClip for why concat rather than -stream_loop).
func loopClip(t *testing.T, ffmpeg, name string, passes int, span time.Duration) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	dir := t.TempDir()
	var list strings.Builder
	for range passes {
		fmt.Fprintf(&list, "file '%s'\n", src)
	}
	listPath := filepath.Join(dir, "list.txt")
	require.NoError(t, os.WriteFile(listPath, []byte(list.String()), 0o600))
	out := filepath.Join(dir, "clip.ts")
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-f", "concat", "-safe", "0", "-i", listPath}
	if span > 0 {
		args = append(args, "-t", fmt.Sprintf("%.1f", span.Seconds()))
	}
	b, err := exec.CommandContext(t.Context(), ffmpeg, append(args, "-c", "copy", "-f", "mpegts", out)...).CombinedOutput()
	require.NoError(t, err, string(b))
	return out
}

var (
	helpOnce sync.Once
	helpText []byte
)

// ffmpegHelp returns ffmpeg's full help, read once (the tests use one
// ffmpeg); nil when it cannot be read.
func ffmpegHelp(ffmpeg string) []byte {
	helpOnce.Do(func() {
		out, err := exec.CommandContext(context.Background(), ffmpeg, "-hide_banner", "-h", "full").CombinedOutput()
		if err == nil {
			helpText = out
		}
	})
	return helpText
}

// rateArgs paces ffmpeg at real time with no initial burst, and, where ffmpeg
// has the option (7.1 and later), no catch-up: the default catch-up of 1.05
// would send the first seconds 5 % fast, which skews a relay's sender
// reports.
func rateArgs(t *testing.T, ffmpeg string) []string {
	t.Helper()
	args := []string{"-readrate", "1", "-readrate_initial_burst", "0"}
	if bytes.Contains(ffmpegHelp(ffmpeg), []byte("readrate_catchup")) {
		args = append(args, "-readrate_catchup", "1")
	}
	return args
}

// delayArgs returns the option that sets the muxer's maximum delay to 0
// (see rtspPublish).
func delayArgs(ffmpeg string) []string { return delayArgsFrom(ffmpegHelp(ffmpeg)) }

// delayArgsFrom picks "-max_delay 0" when ffmpeg's help lists it, else the
// older "-muxdelay 0", which sets the same field.
func delayArgsFrom(help []byte) []string {
	if bytes.Contains(help, []byte("-max_delay")) {
		return []string{"-max_delay", "0"}
	}
	return []string{"-muxdelay", "0"}
}

// rtspPublish starts ffmpeg publishing the TS clip, in real time, to
// MediaMTX at url. "-max_delay 0" (or "-muxdelay 0" on an ffmpeg without
// it, delayArgs) matters: ffmpeg's RTP muxer holds AAC
// frames back, aggregating them into one packet for up to the muxer's
// max_delay (0.7 s for RTSP), which makes the audio arrive 120 to 200 ms
// behind its video; a pull of the relay cannot know that, since MediaMTX
// builds its sender reports from arrival, and the tracks would start that
// far apart. ffmpeg's RTSP muxer refuses AAC without global headers,
// which a TS carries only as ADTS, so the clip is remuxed to MP4 first (a
// copy of every packet).
func rtspPublish(t *testing.T, ffmpeg, clip, url string) *ffmpegProc {
	t.Helper()
	mp4 := strings.TrimSuffix(clip, ".ts") + ".mp4"
	b, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", clip, "-c", "copy", "-f", "mp4", mp4).CombinedOutput()
	require.NoError(t, err, string(b))
	args := append(rateArgs(t, ffmpeg), "-i", mp4, "-c", "copy")
	args = append(args, delayArgs(ffmpeg)...)
	return startFFmpeg(t, ffmpeg, append(args, "-f", "rtsp", "-rtsp_transport", "tcp", url)...)
}

// checkRTSP waits for the run to end and checks that the frames are clean
// (conform's default rules, the 40 ms start offset included), in the epochs
// wantEpochs, with nothing lost in the hub and no drift epoch.
func checkRTSP(t *testing.T, r *m2aRun, wantEpochs ...uint32) {
	t.Helper()
	r.check(t, wantEpochs)
}

// mtxRTSP runs one MediaMTX row: ffmpeg publishes span of the fixture name
// (looped as needed) into MediaMTX over TCP, and our RTSP pull plays it over
// transport once the path is ready. It returns the run, the source's frame
// counts and the bounds on what the join may have missed.
func mtxRTSP(t *testing.T, name, transport string, span time.Duration) (r *m2aRun, src kindMap, rl *readerLog, b rowBounds, join time.Duration) {
	t.Helper()
	mtx := testpeer.MediaMTXRTSP(t)
	ffmpeg := ffmpegRTSP(t)
	passes := int(span/(6*time.Second)) + 1
	clip := loopClip(t, ffmpeg, name, passes, span)
	data, err := os.ReadFile(clip)
	require.NoError(t, err)
	src = demuxCounts(t, data)
	path := mtxPath(strings.TrimSuffix(name, ".ts"))

	f := rtspPublish(t, ffmpeg, clip, "rtsp://"+mtx+"/"+path)
	started := time.Now()
	ended := make(chan struct{})
	rl = &readerLog{}
	r = startRun(t, runOptions{
		inputs: []string{"rtsp://" + mtx + "/" + path + "?transport=" + transport},
		open:   rl.wrap(mtxOpen(ended)),
	})
	mtxEnd(r, f.done, ended)
	r.waitActive(t, 0, rtspReady)
	join = time.Since(started)
	checkRTSP(t, r, 1)
	f.requireExited(t)
	return r, src, rl, joinBounds(join, 0), join
}

// TestM2cRTSPMediaMTXH264 has ffmpeg publish h264_pyramid.ts (a B-pyramid
// with AAC), looped, into MediaMTX for 32 s, and plays it over TCP
// interleaved: one epoch of clean frames (the default 40 ms start-offset
// rule included), nothing lost over TCP, no offset jump, drift evaluated and
// no drift epoch. MediaMTX sends sender reports every 10 s, so 32 s covers
// three rounds.
func TestM2cRTSPMediaMTXH264(t *testing.T) {
	t.Parallel()
	r, src, rl, b, join := mtxRTSP(t, "h264_pyramid.ts", "tcp", spanH264)
	st := rl.stats()
	assertRowCounts(t, src, r.col.counts(), r.stats.Clock, b)
	assertCodecs(t, r.col.frames, media.CodecH264, media.CodecAAC)
	assert.Zero(t, r.stats.Switches)
	assert.Zero(t, st["lost_packets"], "lost packets over TCP")
	assert.Zero(t, st["lost_aus"], "lost AUs over TCP")
	assert.Zero(t, st["skipped_tracks"])
	assert.Zero(t, st["sync_jumps"], "offset jumps")
	assert.LessOrEqual(t, st["sync_corrected"], uint64(1), "first-pair corrections")
	assert.Positive(t, r.stats.Clock.DriftEvaluations, "drift evaluated")
	t.Logf("joined %v after the publish; source %s, delivered %s over %v; clock %+v; reader %v; failures %v",
		join.Round(time.Millisecond), src, r.col.counts(), r.col.span().Round(time.Millisecond),
		r.stats.Clock, st, r.failures())
}

// TestM2cRTSPMediaMTXH265UDP has ffmpeg publish h265_aac51.ts (H.265 with an
// open GOP, and 5.1 AAC), looped, into MediaMTX, and plays it over UDP: one
// epoch of clean H.265 and AAC frames: nothing is delivered before the
// first keyframe, and the RASL pictures of the CRAs that follow it are
// (the conformance rules check that none follows a joining keyframe).
func TestM2cRTSPMediaMTXH265UDP(t *testing.T) {
	t.Parallel()
	r, src, rl, b, join := mtxRTSP(t, "h265_aac51.ts", "udp", 15*time.Second)
	st := rl.stats()
	assertRowCounts(t, src, r.col.counts(), r.stats.Clock, b)
	assertCodecs(t, r.col.frames, media.CodecH265, media.CodecAAC)
	assert.Zero(t, r.stats.Switches)
	assert.Zero(t, st["skipped_tracks"])
	assert.Zero(t, st["sync_jumps"], "offset jumps")
	assert.Equal(t, uint64(0), st["fallback_tcp"], "UDP carried the media")
	require.NotEmpty(t, r.col.frames)
	assert.True(t, r.col.frames[0].Key, "first frame is a keyframe")
	rasl := 0
	for _, f := range r.col.frames[1:] {
		if f.RASL {
			rasl++
		}
	}
	// The clip repeats every 6 s, each repeat joining its last GOP to a CRA
	// with leading pictures that reference it: those are delivered, marked.
	assert.Positive(t, rasl, "RASL frames after a mid-stream CRA delivered")
	t.Logf("joined %v after the publish; source %s, delivered %s (%d RASL) over %v; clock %+v; reader %v; failures %v",
		join.Round(time.Millisecond), src, r.col.counts(), rasl, r.col.span().Round(time.Millisecond),
		r.stats.Clock, st, r.failures())
}

// rtspFixtureSources turns a TS fixture into the H.264 and AAC tracks of a
// scripted RTSP server, both starting at RTP time 0.
func rtspFixtureSources(t *testing.T, name string) []rtsptest.TrackSource {
	t.Helper()
	d := mpegts.NewDemux(bytes.NewReader(fixtureBytes(t, name)))
	var videoPID, audioPID uint16
	video := rtsptest.TrackSource{Codec: rtsp.CodecH264, ClockRate: 90000}
	audio := rtsptest.TrackSource{Codec: rtsp.CodecAAC}
	var v0, a0 int64
	haveV, haveA := false, false
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				switch s.Type {
				case 0x1b:
					videoPID = s.PID
				case 0x0f:
					audioPID = s.PID
				}
			}
			continue
		}
		p := ev.PES
		switch {
		case p == nil:
		case p.PID == videoPID:
			if !haveV {
				v0, haveV = p.PTS, true
			}
			parts := splitAnnexB(p.Data)
			nalus := make([][]byte, 0, len(parts))
			for _, n := range parts {
				switch n[0] & 0x1f {
				case 9:
					continue
				case 7, 8:
					if len(video.ParamSets) < 2 {
						video.ParamSets = append(video.ParamSets, n)
					}
				}
				nalus = append(nalus, n)
			}
			video.AUs = append(video.AUs, rtsptest.TestAU{TS: uint32(p.PTS - v0), NALUs: nalus})
		case p.PID == audioPID:
			frames, err := mpegts.ParseADTS(p.Data)
			require.NoError(t, err)
			if len(frames) == 0 {
				continue
			}
			if !haveA {
				a0, haveA = p.PTS, true
				audio.ClockRate = uint32(frames[0].SampleRate)
				audio.AAC = &rtsp.AACFormat{
					Config: frames[0].AudioSpecificConfig(), SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
				}
			}
			au := rtsptest.TestAU{TS: uint32((p.PTS - a0) * int64(audio.ClockRate) / 90000)}
			for _, fr := range frames {
				au.Frames = append(au.Frames, fr.Payload)
			}
			audio.AUs = append(audio.AUs, au)
		}
	}
	require.NotEmpty(t, video.AUs)
	require.NotEmpty(t, audio.AUs)
	require.Len(t, video.ParamSets, 2)
	return []rtsptest.TrackSource{video, audio}
}

// splitAnnexB splits an Annex B byte stream into NAL units.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// TestM2cRTSPLossyUDP plays a scripted server that drops 1 % of its UDP RTP
// packets (and nothing else) for 10 s: one epoch of clean frames, every
// lost packet counted by the reader, the access units they hit dropped, and
// the frames after them delivered.
func TestM2cRTSPLossyUDP(t *testing.T) {
	t.Parallel()
	srv := rtsptest.NewServer(t, rtsptest.ServerOptions{
		Media: rtspFixtureSources(t, "h264_aac.ts"), UDPLoss: 0.01, SRAfter: 200 * time.Millisecond, Seed: 11,
	})
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{srv.URL("/cam?transport=udp")}, open: rl.wrap(nil)})
	r.waitActive(t, 0, 10*time.Second)
	time.Sleep(10 * time.Second)
	r.cancel()
	r.check(t, []uint32{1})

	injected := 0
	for _, in := range srv.Injected() {
		if in.Kind == rtsptest.InjectLoss {
			injected++
		}
	}
	st := rl.stats()
	require.Positive(t, injected, "packets the server dropped")
	assert.Positive(t, st["lost_packets"], "losses counted")
	assert.LessOrEqual(t, st["lost_packets"], uint64(injected))
	assert.Positive(t, st["lost_aus"], "AUs dropped for a missing packet")
	assert.Zero(t, st["resets"])
	got := r.col.counts()
	assert.Greater(t, got[epochKind{1, media.KindVideo}], 150, "video frames delivered")
	assert.Greater(t, got[epochKind{1, media.KindAudio}], 300, "audio frames delivered")
	assert.Zero(t, r.stats.Switches)
	t.Logf("server dropped %d packets; delivered %s over %v; clock %+v; reader %v; failures %v",
		injected, got, r.col.span().Round(time.Millisecond), r.stats.Clock, st, r.failures())
}

// TestM2cFailoverRTSPtoRTMP kills the ffmpeg publishing the primary's
// MediaMTX path mid-stream; MediaMTX closes our RTSP session and the
// pipeline fails over to the RTMP listen backup, fed by our publisher, in
// exactly one epoch change. A live packager on the same hub lists one
// discontinuity and reads back clean.
func TestM2cFailoverRTSPtoRTMP(t *testing.T) {
	t.Parallel()
	start := time.Now()
	mtx := testpeer.MediaMTXRTSP(t)
	ffmpeg := ffmpegRTSP(t)
	clip := loopClip(t, ffmpeg, "h264_aac.ts", clipPasses, 0)
	tags := flvTags(t, "h264_aac.flv")
	rtmpPort := freeTCPPort(t)
	url := "rtsp://" + mtx + "/" + mtxPath("failover")

	var pk *live.Packager
	pkDone := make(chan struct{})
	var rl readerLog
	r := startRun(t, runOptions{
		inputs: []string{url + "?transport=tcp", rtmpListen(rtmpPort)},
		open:   rl.wrap(openWhenReady(url + "?transport=tcp")),
		prepare: func(h *hub.Hub) {
			pk = live.NewPackager(h, live.Options{Window: m3aWindow})
			sub := h.Subscribe("packager", bigQueue)
			go func() {
				defer close(pkDone)
				pk.Run(context.Background(), sub)
			}()
			// Registered before the run's cleanup, so it runs after the
			// run's cleanup has closed the hub.
			t.Cleanup(func() { <-pkDone })
		},
	})
	primary := rtspPublish(t, ffmpeg, clip, url)
	r.waitActive(t, 0, rtspReady)
	// The backup publishes as soon as its listener is up, so that it is
	// probed while the primary feeds the hub.
	waitListening(t, rtmpPort)
	done, perr := publishTags(t, rtmpPublish(rtmpPort), tags)
	r.endWith(done)
	time.Sleep(2 * time.Second)

	killed := time.Now()
	require.NoError(t, primary.cmd.Process.Kill())
	r.waitActive(t, 1, 15*time.Second)
	failover := time.Since(killed)
	checkRTSP(t, r, 1, 2)
	<-done
	require.NoError(t, *perr)
	assert.Equal(t, uint64(1), r.stats.Switches)
	for _, f := range r.col.frames {
		if f.Epoch == 2 && f.Kind() == media.KindVideo {
			assert.Equal(t, media.CodecH264, f.Config.Codec)
		}
	}

	// The packager lists every segment once their walls have passed.
	<-pkDone
	require.Zero(t, pk.Errors())
	require.Zero(t, pk.FallbackAnchors())
	require.Eventually(t, func() bool {
		s := pk.Snapshot()
		return len(s.Segments) > 0 && s.Listed == len(s.Segments)
	}, 10*time.Second, 50*time.Millisecond, "every segment listed")
	assert.Len(t, pk.Snapshot().Epochs, 2)
	mux := http.NewServeMux()
	mux.Handle("/live/", live.Handler(func(name string) (*live.Packager, bool) { return pk, name == "s" }))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL + "/live/s/"
	assert.Equal(t, 1, discontinuities(mediaPlaylist(t, base+"hls/index.m3u8")))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := conform.ReadHLS(ctx, base+"hls/index.m3u8", conform.ReadOptions{Once: true})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	assert.NotEmpty(t, c.Frames)
	assert.Equal(t, []uint32{1, 2}, epochs(c.Frames))
	t.Logf("failover %v after the kill; delivered %s; HLS read back %s; failures %v; took %v",
		failover.Round(time.Millisecond), kindCounts(r.col.frames), kindCounts(c.Frames), r.failures(),
		time.Since(start).Round(time.Millisecond))
}

// openWhenReady is ingest.Open, retrying the first open of first until
// MediaMTX has its path ready: later opens fail at once, so that a failed
// primary fails over instead of being waited for.
func openWhenReady(first string) func(ctx context.Context, u string) (ingest.Reader, error) {
	var opened atomic.Bool
	return func(ctx context.Context, u string) (ingest.Reader, error) {
		for {
			r, err := ingest.Open(ctx, u, ingest.Env{})
			if err == nil {
				if u == first {
					opened.Store(true)
				}
				return r, nil
			}
			if u != first || opened.Load() {
				return nil, err //nolint:wrapcheck // the pipeline wraps it
			}
			select {
			case <-ctx.Done():
				return nil, err //nolint:wrapcheck // the pipeline wraps it
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// repackPES writes the video frames as MPEG-TS with per access units in each
// PES, carrying only the first unit's PTS and DTS and a PCR just before
// its DTS (so that sendTS paces it). The first PES also carries the
// parameter sets: frames hold none.
func repackPES(t *testing.T, stype uint8, frames []media.Frame, per int) []byte {
	t.Helper()
	const (
		pid = 0x100
		off = 900000
	)
	var buf bytes.Buffer
	w := mpegts.NewWriter(&buf)
	require.NoError(t, w.SetProgram(mpegts.PMT{PCRPID: pid, Streams: []mpegts.Stream{{PID: pid, Type: stype}}}))
	require.NoError(t, w.WriteTables())
	cfg := frames[0].Config
	for i := 0; i < len(frames); i += per {
		var data []byte
		add := func(n []byte) { data = append(append(data, 0, 0, 0, 1), n...) }
		if i == 0 {
			for _, set := range [][][]byte{cfg.VPS, cfg.SPS, cfg.PPS} {
				for _, n := range set {
					add(n)
				}
			}
		}
		for _, f := range frames[i:min(i+per, len(frames))] {
			for _, n := range f.Data {
				add(n)
			}
		}
		f := frames[i]
		require.NoError(t, w.WritePES(pid, data, mpegts.PESOptions{
			PTS: f.PTS + off, DTS: f.DTS + off, HasDTS: f.DTS != f.PTS,
			RandomAccess: f.Key, PCR: f.DTS + off - 4500, Video: true,
		}))
	}
	return buf.Bytes()
}

// TestM2cTSReorderedPES sends h264_pyramid.ts's video over UDP packed three
// access units to a PES, each PES carrying only its first unit's timestamps.
// The TS reader recovers the other units with the picture order counts, once
// it has calibrated them: of the 150 units, all but the 8 the calibration
// and the POC resets (IDRs at 25, 50, 100, 125) cost are delivered (the
// ts package's TestVideoPESManyAUsReordered pins which), and the frames
// are clean.
func TestM2cTSReorderedPES(t *testing.T) {
	t.Parallel()
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(fixtureBytes(t, "h264_pyramid.ts")), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	var src []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if f.Kind() == media.KindVideo {
			src = append(src, f)
		}
	}
	require.Len(t, src, 150)
	data := repackPES(t, 0x1b, src, 3)

	port := freeUDPPort(t)
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{fmt.Sprintf("udp://@:%d", port)}, open: rl.wrap(nil)})
	waitBound(t, port)
	r.endWith(sendTS(t, port, data))
	r.check(t, []uint32{1})

	st := rl.stats()
	got := r.col.counts()
	const (
		dropped   = 8
		heldAtEnd = 3 // the demuxer holds the last units back for reordering
	)
	assert.EqualValues(t, dropped, st["split_aus_reordered"], "units the calibration and POC resets drop")
	assert.EqualValues(t, len(src)-dropped-len(src)/3, st["split_aus_poc"], "units timed by POC")
	// Every delivered frame is a source unit with the source's timing (up
	// to a constant shift), and the units missing are the dropped ones and
	// the last frames the demuxer still holds when the source ends.
	out := r.col.frames
	require.NotEmpty(t, out)
	shift := out[0].PTS - src[0].PTS
	byPTS := map[int64]int{}
	for i, f := range src {
		byPTS[f.PTS] = i
	}
	idx := make([]int, 0, len(out))
	for k, f := range out {
		i, ok := byPTS[f.PTS-shift]
		require.True(t, ok, "frame %d: PTS %d is no source PTS", k, f.PTS-shift)
		assert.Equal(t, src[i].Data, f.Data, "unit %d", i)
		assert.Equal(t, src[i].Key, f.Key, "unit %d", i)
		idx = append(idx, i)
	}
	assert.True(t, slices.IsSorted(idx), "decode order")
	var missing []int
	for i := range src {
		if !slices.Contains(idx, i) {
			missing = append(missing, i)
		}
	}
	assert.Equal(t, []int{1, 2, 25, 26, 50, 100, 101, 125}, missing[:min(len(missing), dropped)], "dropped units")
	assert.LessOrEqual(t, len(missing), dropped+heldAtEnd, "units missing: %v", missing)
	assert.Equal(t, got[epochKind{1, media.KindVideo}], len(idx))
	t.Logf("source %d video units, delivered %s; clock %+v; reader %v", len(src), got, r.stats.Clock, st)
}

func TestDelayArgs(t *testing.T) {
	assert.Equal(t, []string{"-max_delay", "0"},
		delayArgsFrom([]byte("  -max_delay         <int>        ED......... maximum muxing or demuxing delay")))
	assert.Equal(t, []string{"-muxdelay", "0"},
		delayArgsFrom([]byte("-muxdelay <seconds>  set the maximum demux-decode delay")))
	assert.Equal(t, []string{"-muxdelay", "0"}, delayArgsFrom(nil))
}
