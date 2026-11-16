package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

// The M2b scenarios feed the RTMP inputs from independent peers (ffmpeg
// publishing to our listener, MediaMTX serving our pull, and ffmpeg-written
// enhanced-FLV H.265 replayed by our publisher) through a pipeline into a
// hub, and check the frames that a hub subscriber collects.

// mtxReady bounds the wait for a MediaMTX pull of the live clip to
// deliver: MediaMTX makes a path ready 2 to 2.5 s after its publisher
// connects, once it knows the tracks. mtxReadyHEVC is the tighter bound for
// the 6 s h265_aac.flv, so that the pull still reads its last two GOPs
// (keys every second from 0.08 s): a later join would leave the row's
// counts nearly unchecked.
const (
	mtxReady     = 6 * time.Second
	mtxReadyHEVC = 3500 * time.Millisecond
)

// ffmpegBurst is how far ahead of real time ffmpeg -re sends at its start
// (its -readrate_initial_burst default, 0.5 s since ffmpeg 6.1).
const ffmpegBurst = 500 * time.Millisecond

// readerLog records the readers a pipeline opens, so that their counters
// can be read once the run has ended.
type readerLog struct {
	mu      sync.Mutex
	readers []ingest.Reader
}

// wrap returns open, ingest.Open when nil, recording every reader it opens.
func (l *readerLog) wrap(open func(ctx context.Context, u string) (ingest.Reader, error)) func(ctx context.Context, u string) (ingest.Reader, error) {
	if open == nil {
		open = func(ctx context.Context, u string) (ingest.Reader, error) { return ingest.Open(ctx, u, ingest.Env{}) }
	}
	return func(ctx context.Context, u string) (ingest.Reader, error) {
		r, err := open(ctx, u)
		if err == nil {
			l.mu.Lock()
			l.readers = append(l.readers, r)
			l.mu.Unlock()
		}
		return r, err
	}
}

// stats sums the counters of every reader opened.
func (l *readerLog) stats() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]uint64{}
	for _, r := range l.readers {
		for k, v := range r.Stats() {
			out[k] += v
		}
	}
	return out
}

// assertRTMPCounts checks got against want within b, as assertRowCounts
// does, with the audio frames that the RTMP inputs' audio grid dropped as
// ahead of it (audio_dropped_ahead) accounted for, and checks that no tag
// was bad and no frame was dropped for want of a config.
func assertRTMPCounts(t *testing.T, want, got kindMap, r *m2aRun, st map[string]uint64, b rowBounds) {
	t.Helper()
	adjusted := kindMap{}
	for k, n := range got {
		adjusted[k] = n
	}
	adjusted[epochKind{1, media.KindAudio}] += int(st["audio_dropped_ahead"])
	assertRowCounts(t, want, adjusted, r.stats.Clock, b)
	assert.Zero(t, st["bad_tags"], "bad tags")
	assert.Zero(t, st["dropped_no_config"], "dropped for want of a config")
}

// freeTCPPort returns a TCP port that was free a moment ago and that no
// other test of this package was given (see freeUDPPort).
func freeTCPPort(t *testing.T) int {
	t.Helper()
	portsMu.Lock()
	defer portsMu.Unlock()
	for {
		l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		if !ports[port] {
			ports[port] = true
			return port
		}
	}
}

// tcpListening reports whether a TCP socket listens on port, from procfs;
// known is false where there is no procfs.
func tcpListening(port int) (listening, known bool) {
	suffix := fmt.Sprintf(":%04X", port)
	for _, name := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		known = true
		for line := range strings.SplitSeq(string(b), "\n") {
			// sl local_address rem_address st …; st 0A is LISTEN.
			if f := strings.Fields(line); len(f) > 3 && strings.HasSuffix(f[1], suffix) && f[3] == "0A" {
				return true, true
			}
		}
	}
	return false, known
}

// waitListening waits until a TCP socket listens on port: procfs where
// there is one, else a connection that our RTMP listener drops at once.
func waitListening(t *testing.T, port int) {
	t.Helper()
	require.Eventually(t, func() bool {
		if up, known := tcpListening(port); known {
			return up
		}
		c, err := new(net.Dialer).DialContext(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 10*time.Second, 20*time.Millisecond, "tcp port %d listening", port)
}

// rtmpListen is the URL of an RTMP listen input for live/s on port, and
// rtmpPublish the URL a publisher pushes to.
func rtmpListen(port int) string  { return fmt.Sprintf("rtmp://127.0.0.1:%d/live/s?mode=listen", port) }
func rtmpPublish(port int) string { return fmt.Sprintf("rtmp://127.0.0.1:%d/live/s", port) }

// flvTags reads a fixture's FLV tags.
func flvTags(t *testing.T, name string) []rtmp.FLVTag {
	t.Helper()
	tags, err := rtmp.ReadFLV(bytes.NewReader(fixtureBytes(t, name)))
	require.NoError(t, err)
	require.NotEmpty(t, tags)
	return tags
}

// tagCounts counts the coded video frames and the AAC frames in tags, as
// epoch 1.
func tagCounts(t *testing.T, tags []rtmp.FLVTag) kindMap {
	t.Helper()
	out := kindMap{}
	for i, tg := range tags {
		switch tg.Type {
		case rtmp.FLVVideo:
			v, err := rtmp.ParseVideoTag(tg.Data)
			require.NoError(t, err, "tag %d", i)
			if v.Kind == rtmp.VideoCodedFrames {
				out[epochKind{1, media.KindVideo}]++
			}
		case rtmp.FLVAudio:
			a, err := rtmp.ParseAudioTag(tg.Data)
			if errors.Is(err, rtmp.ErrUnsupportedCodec) {
				continue // h265_aac.flv's enhanced-audio config tags
			}
			require.NoError(t, err, "tag %d", i)
			if !a.SequenceHeader {
				out[epochKind{1, media.KindAudio}]++
			}
		}
	}
	return out
}

// publishTags publishes tags with our publisher, paced, in the background;
// the returned channel is closed once the publish has ended, and err holds
// its result by then.
func publishTags(t *testing.T, url string, tags []rtmp.FLVTag) (done <-chan struct{}, err *error) {
	t.Helper()
	ch := make(chan struct{})
	var perr error
	go func() {
		defer close(ch)
		perr = rtmptest.Publish(t.Context(), url, tags, true)
	}()
	t.Cleanup(func() { <-ch })
	return ch, &perr
}

// assertCodecs checks that frames has both kinds, and that every frame of
// each kind carries the codec.
func assertCodecs(t *testing.T, frames []media.Frame, video, audio media.Codec) {
	t.Helper()
	want := map[media.Kind]media.Codec{media.KindVideo: video, media.KindAudio: audio}
	seen := map[media.Kind]bool{}
	for i, f := range frames {
		require.Equal(t, want[f.Kind()], f.Config.Codec, "frame %d", i)
		seen[f.Kind()] = true
	}
	assert.True(t, seen[media.KindVideo], "video delivered")
	assert.True(t, seen[media.KindAudio], "audio delivered")
}

// mtxPath is a MediaMTX path unique to this test run, so that tests sharing
// one MediaMTX never meet.
func mtxPath(name string) string {
	return "osv2/" + name + "-" + rand.Text()[:10]
}

// mtxEnd closes ended and marks the run's source ended once done is closed,
// unless the run ends first.
func mtxEnd(r *m2aRun, done <-chan struct{}, ended chan<- struct{}) {
	go func() {
		select {
		case <-done:
			close(ended)
			r.end()
		case <-r.stop:
		}
	}()
}

// mtxOpen opens the pull input, retrying until MediaMTX has the path ready:
// a path exists only once its publisher's tracks are known. Once ended is
// closed it stops retrying, so that the run's end is not delayed.
func mtxOpen(ended <-chan struct{}) func(ctx context.Context, u string) (ingest.Reader, error) {
	return func(ctx context.Context, u string) (ingest.Reader, error) {
		for {
			r, err := ingest.Open(ctx, u, ingest.Env{})
			if err == nil {
				return r, nil
			}
			select {
			case <-ended:
				return nil, err //nolint:wrapcheck // the pipeline wraps it
			case <-ctx.Done():
				return nil, err //nolint:wrapcheck // the pipeline wraps it
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// joinBounds bounds a pull row that joins a live source after join: the
// frames the source sent before then (burst is how far ahead of real time
// the publisher started: ffmpegBurst for ffmpeg, 0 for our publisher) and up
// to one GOP more, while MediaMTX waits for a key, may be missing.
func joinBounds(join, burst time.Duration) rowBounds {
	return rowBounds{short: within(join + burst + gopSpan + failoverSlack)}
}

// videoFromKey counts the coded video frames of tags from the first
// keyframe at or after the source time after (from the first tag) on, and
// the keyframes among them.
func videoFromKey(t *testing.T, tags []rtmp.FLVTag, after time.Duration) (frames, keys int) {
	t.Helper()
	from := false
	for _, tg := range tags {
		if tg.Type != rtmp.FLVVideo {
			continue
		}
		v, err := rtmp.ParseVideoTag(tg.Data)
		require.NoError(t, err)
		if v.Kind != rtmp.VideoCodedFrames {
			continue
		}
		at := time.Duration(tg.Timestamp-tags[0].Timestamp) * time.Millisecond
		if v.Key && at >= after {
			from = true
		}
		if from {
			frames++
			if v.Key {
				keys++
			}
		}
	}
	return frames, keys
}

// TestM2bRTMPListenFFmpeg has ffmpeg publish the 36 s live clip into our
// RTMP listener in real time. The run reads for liveSpan, with drift
// evaluated and no drift epoch, and delivers one epoch of clean frames,
// every frame of the clip accounted for. The audio grid drops a few audio
// frames as ahead of it (3 on every run): each of the clip's 5
// concatenation points overlaps audio by 16 ms (two frames 5.3 ms apart,
// the same in the TS clip and the FLV ffmpeg makes of it), and the grid
// drops a frame whenever the lead passes one frame, so at most one per
// concatenation point.
func TestM2bRTMPListenFFmpeg(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	clip := liveClip(t, ffmpeg)
	data, err := os.ReadFile(clip)
	require.NoError(t, err)
	src := demuxCounts(t, data)
	port := freeTCPPort(t)
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{rtmpListen(port)}, open: rl.wrap(nil)})
	waitListening(t, port)
	f := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "flv", rtmpPublish(port))
	r.endWith(f.done)

	r.check(t, []uint32{1})
	f.requireExited(t)
	got, st := r.col.counts(), rl.stats()
	assertRTMPCounts(t, src, got, r, st, rowBounds{})
	assert.LessOrEqual(t, st["audio_dropped_ahead"], uint64(clipPasses-1), "audio dropped as ahead of the grid")
	assert.Zero(t, r.stats.Switches)
	assert.GreaterOrEqual(t, r.col.span(), liveSpan, "read time")
	assert.Positive(t, r.stats.Clock.DriftEvaluations, "drift evaluated")
	t.Logf("source %s, delivered %s over %v; clock %+v; reader %v; failures %v",
		src, got, r.col.span().Round(time.Millisecond), r.stats.Clock, st, r.failures())
}

// TestM2bRTMPListenHEVC replays the tags of h265_aac.flv, which ffmpeg
// wrote as enhanced FLV, into our RTMP listener with our publisher, in real
// time: one epoch of clean H.265 and AAC frames, every coded frame of the
// file accounted for.
func TestM2bRTMPListenHEVC(t *testing.T) {
	t.Parallel()
	tags := flvTags(t, "h265_aac.flv")
	src := tagCounts(t, tags)
	port := freeTCPPort(t)
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{rtmpListen(port)}, open: rl.wrap(nil)})
	waitListening(t, port)
	done, perr := publishTags(t, rtmpPublish(port), tags)
	r.endWith(done)

	r.check(t, []uint32{1})
	<-done
	require.NoError(t, *perr)
	got, st := r.col.counts(), rl.stats()
	assertRTMPCounts(t, src, got, r, st, rowBounds{})
	assertCodecs(t, r.col.frames, media.CodecH265, media.CodecAAC)
	t.Logf("source %s, delivered %s; clock %+v; reader %v", src, got, r.stats.Clock, st)
}

// TestRTMPMediaMTX has ffmpeg publish the live clip to MediaMTX, and our
// RTMP pull play it from there once the path is ready: one epoch of clean
// frames, read for liveSpan with drift evaluated, short of the clip by at
// most what was sent before the join (joinBounds).
func TestRTMPMediaMTX(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTX(t)
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	clip := liveClip(t, ffmpeg)
	data, err := os.ReadFile(clip)
	require.NoError(t, err)
	src := demuxCounts(t, data)
	url := "rtmp://" + mtx + "/" + mtxPath("h264")

	f := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "flv", url)
	started := time.Now()
	ended := make(chan struct{})
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{url}, open: rl.wrap(mtxOpen(ended))})
	mtxEnd(r, f.done, ended)
	r.waitActive(t, 0, mtxReady)
	join := time.Since(started)

	r.check(t, []uint32{1})
	f.requireExited(t)
	got, st := r.col.counts(), rl.stats()
	b := joinBounds(join, ffmpegBurst)
	assertRTMPCounts(t, src, got, r, st, b)
	assertCodecs(t, r.col.frames, media.CodecH264, media.CodecAAC)
	assert.Zero(t, r.stats.Switches)
	assert.GreaterOrEqual(t, r.col.span(), liveSpan, "read time")
	assert.Positive(t, r.stats.Clock.DriftEvaluations, "drift evaluated")
	t.Logf("joined %v after the publish (bound %+v); source %s, delivered %s over %v; clock %+v; reader %v; failures %v",
		join.Round(time.Millisecond), b, src, got, r.col.span().Round(time.Millisecond), r.stats.Clock,
		st, r.failures())
}

// TestRTMPMediaMTXHEVC publishes the tags of h265_aac.flv to MediaMTX with
// our publisher, and our RTMP pull plays them back once the path is ready:
// MediaMTX's enhanced-RTMP writer is the independent peer. One epoch of
// clean H.265 and AAC frames, short of the file by at most what was sent
// before the join, which comes within mtxReadyHEVC; so at least the file's
// GOPs from its first keyframe after mtxReadyHEVC on (two) are delivered.
func TestRTMPMediaMTXHEVC(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTX(t)
	tags := flvTags(t, "h265_aac.flv")
	src := tagCounts(t, tags)
	minVideo, minKeys := videoFromKey(t, tags, mtxReadyHEVC)
	require.GreaterOrEqual(t, minKeys, 2, "GOPs after the join limit")
	url := "rtmp://" + mtx + "/" + mtxPath("h265")

	done, perr := publishTags(t, url, tags)
	started := time.Now()
	ended := make(chan struct{})
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{url}, open: rl.wrap(mtxOpen(ended))})
	mtxEnd(r, done, ended)
	r.waitActive(t, 0, mtxReadyHEVC)
	join := time.Since(started)

	r.check(t, []uint32{1})
	<-done
	require.NoError(t, *perr)
	got, st := r.col.counts(), rl.stats()
	b := joinBounds(join, 0)
	assertRTMPCounts(t, src, got, r, st, b)
	assert.GreaterOrEqual(t, got[epochKind{1, media.KindVideo}], minVideo, "video frames from the first key after %v", mtxReadyHEVC)
	assertCodecs(t, r.col.frames, media.CodecH265, media.CodecAAC)
	t.Logf("joined %v after the publish (bound %+v); source %s, delivered %s; clock %+v; reader %v",
		join.Round(time.Millisecond), b, src, got, r.stats.Clock, st)
}

// TestM2bFailoverRTMPtoSRT kills the ffmpeg publishing into the primary
// RTMP listen input mid-stream; the pipeline fails over to the SRT listener
// backup, fed by a second ffmpeg, in exactly one epoch change, with the
// frames lost bounded as in TestM2aFailoverSRTtoUDP. A live packager on the
// same hub lists one discontinuity and reads back clean.
func TestM2bFailoverRTMPtoSRT(t *testing.T) {
	t.Parallel()
	start := time.Now()
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp", "srt")
	clip := liveClip(t, ffmpeg)
	rtmpPort, srtPort := freeTCPPort(t), freeUDPPort(t)

	var pk *live.Packager
	pkDone := make(chan struct{})
	r := startRun(t, runOptions{
		inputs: []string{
			rtmpListen(rtmpPort),
			fmt.Sprintf("srt://127.0.0.1:%d?mode=listener&streamid=cam&latency=200", srtPort),
		},
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
	waitListening(t, rtmpPort)
	primary := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "flv", rtmpPublish(rtmpPort))
	r.waitActive(t, 0, 15*time.Second)
	waitBound(t, srtPort)
	backup := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "mpegts",
		fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&streamid=cam&latency=200000", srtPort))

	time.Sleep(8 * time.Second)
	killed := time.Now()
	require.NoError(t, primary.cmd.Process.Kill())
	r.waitActive(t, 1, 15*time.Second)
	failover := time.Since(killed)
	time.Sleep(6 * time.Second)
	select {
	case <-backup.done:
		t.Fatalf("backup ffmpeg exited early: %v: %s", backup.err, backup.stderr.String())
	default:
	}
	r.cancel()
	r.check(t, []uint32{1, 2})
	assert.Equal(t, uint64(1), r.stats.Switches)

	// The gap in the hub between the primary's last frame and the backup's
	// first, in source frames.
	frames, recv := r.col.frames, r.col.recv
	first2 := -1
	for i, f := range frames {
		if f.Epoch == 2 {
			first2 = i
			break
		}
	}
	require.Positive(t, first2)
	gap := recv[first2].Sub(recv[first2-1])
	lostVideo := int(gap.Seconds()*videoRate) - 1
	bound := int((pipeline.DefaultStallTimeout + gopSpan + failoverSlack).Seconds() * videoRate)
	assert.LessOrEqual(t, lostVideo, bound, "video frames lost in a %v gap", gap)
	for _, f := range frames[first2:] {
		require.Equal(t, uint32(2), f.Epoch, "epoch 2 runs to the end")
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
	t.Logf("failover %v after the kill; hub gap %v (%d video frames lost, bound %d); delivered %s; "+
		"HLS read back %s; failures %v; took %v",
		failover.Round(time.Millisecond), gap.Round(time.Millisecond), lostVideo, bound, kindCounts(frames),
		kindCounts(c.Frames), r.failures(), time.Since(start).Round(time.Millisecond))
}

// TestM2bRTMPReconnect has ffmpeg publish h264_aac.ts into our RTMP
// listener twice, the second publisher starting as soon as the first has
// exited: one epoch change, every frame of both publishes accounted for
// (each epoch at most one frame short, dropped by the Clock before the
// epoch's keyframe), and the TCP listener up throughout, without a stall in
// between.
func TestM2bRTMPReconnect(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	clip, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	src := demuxCounts(t, fixtureBytes(t, "h264_aac.ts"))
	port := freeTCPPort(t)
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{rtmpListen(port)}, open: rl.wrap(nil)})
	waitListening(t, port)

	// Sample the listening socket until the second publisher has exited.
	var samples, down atomic.Int64
	stopSampling, sampled := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(stopSampling) })
		<-sampled
	}
	t.Cleanup(stop) // a publish that fails the test stops it too
	_, procfs := tcpListening(port)
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stopSampling:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if up, known := tcpListening(port); known {
				samples.Add(1)
				if !up {
					down.Add(1)
				}
			}
		}
	}()

	publish := func() {
		f := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "flv", rtmpPublish(port))
		f.requireExited(t)
	}
	publish()
	restart := time.Now()
	publish()
	second := time.Since(restart)
	stop()
	assert.Empty(t, r.failures(), "no stall between the publishers")
	r.end()

	r.check(t, []uint32{1, 2})
	if procfs {
		assert.Positive(t, samples.Load())
		assert.Zero(t, down.Load(), "samples with the listener down, of %d", samples.Load())
	} else {
		t.Log("no procfs: listener continuity not sampled")
	}
	assert.Zero(t, r.stats.Switches)
	got, st := kindCounts(r.col.frames), rl.stats()
	double := kindMap{}
	for k, n := range src {
		double[k] = 2 * n
	}
	assertRTMPCounts(t, double, flat(got), r, st, rowBounds{})
	assert.EqualValues(t, 2, st["publishers"])
	for _, e := range []uint32{1, 2} {
		for _, k := range []media.Kind{media.KindVideo, media.KindAudio} {
			want, n := src[epochKind{1, k}], got[epochKind{e, k}]
			assert.LessOrEqual(t, n, want, "epoch %d %v", e, k)
			assert.GreaterOrEqual(t, n, want-1, "epoch %d %v", e, k)
		}
	}
	t.Logf("source %s per publisher, delivered %s; second publish took %v; clock %+v; "+
		"listener samples %d (%d down); reader %v",
		src, got, second.Round(time.Millisecond), r.stats.Clock, samples.Load(), down.Load(), st)
}
