package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// The M2a scenarios run each TS-family input, fed by ffmpeg or by our own
// outputs, through a pipeline into a hub, and check the frames that a hub
// subscriber collects.

const (
	// clipPasses is how many times the live clip repeats h264_aac.ts: 36 s,
	// so that a row joining 3.5 s late still reads more than 30 s.
	clipPasses = 6
	gopVideo   = 25          // h264_aac.ts has closed GOPs of 25 frames at 25 fps
	gopAudio   = 47          // AAC frames in one GOP: 1 s at 48 kHz, 1024 samples each
	gopSpan    = time.Second // one GOP of h264_aac.ts
	videoRate  = 25.0
	audioRate  = 48000.0 / 1024
	// failoverSlack covers the demuxer's hold-back and scheduling in a
	// failover's gap, beyond the stall timeout and the wait for a key.
	failoverSlack = 500 * time.Millisecond
	// liveSpan is how long a live row must read, so that the Clock's drift
	// evaluation, which starts 10 s into an epoch, has 20 s to fire.
	liveSpan = 30 * time.Second
	// lateJoin is when the rows fed by our outputs connect: mid-GOP, so the
	// output starts them with a burst of the GOP so far.
	lateJoin = 3500 * time.Millisecond
	runLimit = 2 * time.Minute // a run that has not ended by then fails
)

// liveClip writes h264_aac.ts looped clipPasses times into a temporary file
// and returns its path. ffmpeg's concat demuxer keeps the timestamps
// continuous and every keyframe; -stream_loop drops the first keyframe of
// each loop.
func liveClip(t *testing.T, ffmpeg string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	dir := t.TempDir()
	var list strings.Builder
	for range clipPasses {
		fmt.Fprintf(&list, "file '%s'\n", src)
	}
	listPath := filepath.Join(dir, "list.txt")
	require.NoError(t, os.WriteFile(listPath, []byte(list.String()), 0o600))
	out := filepath.Join(dir, "clip.ts")
	b, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", "-f", "mpegts", out).CombinedOutput()
	require.NoError(t, err, string(b))
	return out
}

// demuxCounts counts the frames per kind that the ingest TS demuxer reads
// from data, as epoch 1.
func demuxCounts(t *testing.T, data []byte) kindMap {
	t.Helper()
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	out := kindMap{}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out[epochKind{1, f.Kind()}]++
	}
}

// assertWithinGOP checks that got has, per kind, as many frames as want
// within one GOP's worth.
func assertWithinGOP(t *testing.T, want, got kindMap) {
	t.Helper()
	for k, tol := range map[media.Kind]int{media.KindVideo: gopVideo, media.KindAudio: gopAudio} {
		key := epochKind{1, k}
		require.Positive(t, want[key], "source %v frames", k)
		assert.InDelta(t, want[key], got[key], float64(tol), "%v frames: want %s, got %s", k, want, got)
	}
}

// kindTol is a number of frames per kind.
type kindTol struct{ video, audio int }

// within is the frames of each kind in d, rounded up.
func within(d time.Duration) kindTol {
	return kindTol{
		video: int(math.Ceil(d.Seconds() * videoRate)),
		audio: int(math.Ceil(d.Seconds() * audioRate)),
	}
}

// rowBounds bounds the frames a row delivers against its source's, per
// kind. A source frame is accounted for when it is delivered or dropped by a
// Clock start rule (video before the first keyframe or RASL, audio presented
// before the keyframe): at most short frames may be unaccounted for, and at
// most over more frames than the source's may be accounted for (the burst of
// a row joining mid-GOP, from before the join).
type rowBounds struct{ short, over kindTol }

// assertRowCounts checks got against want within b.
func assertRowCounts(t *testing.T, want, got kindMap, st clock.Stats, b rowBounds) {
	t.Helper()
	for _, k := range []struct {
		kind        media.Kind
		dropped     uint64
		short, over int
	}{
		{media.KindVideo, st.DroppedBeforeKey + st.DroppedRASL, b.short.video, b.over.video},
		{media.KindAudio, st.DroppedEarlyAudio, b.short.audio, b.over.audio},
	} {
		key := epochKind{1, k.kind}
		require.Positive(t, want[key], "source %v frames", k.kind)
		accounted := got[key] + int(k.dropped)
		assert.GreaterOrEqual(t, accounted, want[key]-k.short, "%v frames: want %s, got %s, dropped %d", k.kind, want, got, k.dropped)
		assert.LessOrEqual(t, accounted, want[key]+k.over, "%v frames: want %s, got %s, dropped %d", k.kind, want, got, k.dropped)
	}
}

func fileURL(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}

var (
	portsMu sync.Mutex
	ports   = map[int]bool{}
)

// freeUDPPort returns a UDP port that was free a moment ago and that no
// other test of this package was given. Another process can still bind it
// between its release here and the bind by its user; that rare flake is
// accepted.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	portsMu.Lock()
	defer portsMu.Unlock()
	for {
		pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
		require.NoError(t, err)
		port := pc.LocalAddr().(*net.UDPAddr).Port
		require.NoError(t, pc.Close())
		if !ports[port] {
			ports[port] = true
			return port
		}
	}
}

// udpBound reports whether a UDP socket is bound to port. It reads procfs
// where there is one, so that it never takes the port from the program about
// to bind it; elsewhere it tries to bind the port.
func udpBound(port int) bool {
	suffix := fmt.Sprintf(":%04X", port)
	proc := false
	for _, name := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		proc = true
		for line := range strings.SplitSeq(string(b), "\n") {
			if f := strings.Fields(line); len(f) > 1 && strings.HasSuffix(f[1], suffix) {
				return true
			}
		}
	}
	if proc {
		return false
	}
	pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	_ = pc.Close()
	return false
}

func waitBound(t *testing.T, port int) {
	t.Helper()
	require.Eventually(t, func() bool { return udpBound(port) }, 10*time.Second, 20*time.Millisecond,
		"udp port %d bound", port)
}

// waitFiles waits until every named file exists in dir.
func waitFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
				return false
			}
		}
		return true
	}, 15*time.Second, 50*time.Millisecond, "files %v in %s", names, dir)
}

// ffmpegProc is a running ffmpeg. done is closed when it has exited; err
// and stderr are set by then.
type ffmpegProc struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	stderr bytes.Buffer
}

// startFFmpeg runs ffmpeg with args until it exits or the test ends.
func startFFmpeg(t *testing.T, ffmpeg string, args ...string) *ffmpegProc {
	t.Helper()
	p := &ffmpegProc{done: make(chan struct{})}
	p.cmd = exec.CommandContext(t.Context(), ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-nostdin"}, args...)...)
	p.cmd.Stderr = &p.stderr
	require.NoError(t, p.cmd.Start())
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.done
	})
	return p
}

// requireExited checks that ffmpeg exits, within 10 s, without error.
func (p *ffmpegProc) requireExited(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("ffmpeg still running")
	}
	require.NoError(t, p.err, p.stderr.String())
}

// collector takes every delivery of a hub subscriber, with the time it took
// it, until the hub closes.
type collector struct {
	done chan struct{}

	mu     sync.Mutex
	frames []media.Frame
	recv   []time.Time
	lost   uint64
}

func collect(h *hub.Hub, name string) *collector {
	c := &collector{done: make(chan struct{})}
	sub := h.Subscribe(name, bigQueue)
	go func() {
		defer close(c.done)
		for dl := range sub.C() {
			now := time.Now()
			c.mu.Lock()
			c.frames = append(c.frames, dl.Frame)
			c.recv = append(c.recv, now)
			c.lost += dl.Lost
			c.mu.Unlock()
		}
	}()
	return c
}

// counts returns the frames taken so far per kind, as epoch 1.
func (c *collector) counts() kindMap {
	c.mu.Lock()
	defer c.mu.Unlock()
	return flat(kindCounts(c.frames))
}

// sinceKey returns the video frames taken from the last video keyframe on,
// that keyframe included; 0 before the first.
func (c *collector) sinceKey() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for i := len(c.frames) - 1; i >= 0; i-- {
		f := c.frames[i]
		if f.Kind() != media.KindVideo {
			continue
		}
		n++
		if f.Key {
			return n
		}
	}
	return 0
}

// span is the time from the first frame taken to the last.
func (c *collector) span() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recv) == 0 {
		return 0
	}
	return c.recv[len(c.recv)-1].Sub(c.recv[0])
}

// m2aRun is a pipeline running into a hub with a collector. Once ended is
// closed, the first input failure (a passive input's stall, a pull input
// whose source has gone) cancels the run; an input that reaches its end
// ends the run by itself.
type m2aRun struct {
	p      *pipeline.Pipeline
	h      *hub.Hub
	col    *collector
	cancel context.CancelFunc
	done   chan error
	ended  chan struct{}
	endMu  sync.Once
	stop   chan struct{} // closed once the run has ended
	active chan int      // the input of each activation

	mu     sync.Mutex
	events []pipeline.Event

	waitOnce sync.Once
	err      error
	stats    pipeline.Stats
}

// runOptions configures startRun.
type runOptions struct {
	inputs []string
	env    ingest.Env
	open   func(ctx context.Context, url string) (ingest.Reader, error) // nil means ingest.Open with env
	// prepare subscribes to the hub before the pipeline runs; the hub is
	// closed after the run, before the test's cleanups.
	prepare func(h *hub.Hub)
}

// startRun starts a pipeline that ends at an input's end (EndOnEOF).
func startRun(t *testing.T, o runOptions) *m2aRun {
	t.Helper()
	h := hub.New(hub.Options{})
	r := &m2aRun{
		h:      h,
		col:    collect(h, "e2e"),
		done:   make(chan error, 1),
		ended:  make(chan struct{}),
		stop:   make(chan struct{}),
		active: make(chan int, 64),
	}
	if o.prepare != nil {
		o.prepare(h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	r.cancel = cancel
	r.p = pipeline.New(h, pipeline.Options{
		Inputs:   o.inputs,
		Env:      o.env,
		Open:     o.open,
		EndOnEOF: true,
		OnEvent:  r.onEvent,
	})
	go func() { r.done <- r.p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		r.wait()
	})
	return r
}

// startFeed starts fd writing into a hub with a collector, as startRun
// starts a pipeline: the returned run has no pipeline, ends when fd returns,
// and its check skips the pipeline's stats. prepare, when given, subscribes
// to the hub before the first write, as runOptions.prepare.
func startFeed(t *testing.T, fd feed, prepare ...func(h *hub.Hub)) *m2aRun {
	t.Helper()
	h := hub.New(hub.Options{})
	r := &m2aRun{
		h:      h,
		col:    collect(h, "e2e"),
		done:   make(chan error, 1),
		ended:  make(chan struct{}),
		stop:   make(chan struct{}),
		active: make(chan int, 64),
	}
	for _, p := range prepare {
		p(h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	r.cancel = cancel
	go func() { r.done <- fd(ctx, h) }()
	t.Cleanup(func() {
		cancel()
		r.wait()
	})
	return r
}

func (r *m2aRun) onEvent(ev pipeline.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	switch ev.Kind {
	case pipeline.EventActive:
		select {
		case r.active <- ev.Input:
		default:
		}
	case pipeline.EventFailed:
		select {
		case <-r.ended:
			r.cancel()
		default:
		}
	case pipeline.EventDiscontinuity:
	}
}

// end marks the source ended.
func (r *m2aRun) end() { r.endMu.Do(func() { close(r.ended) }) }

// endWith marks the source ended once done is closed, unless the run ends
// first (a failure path where the source never ends).
func (r *m2aRun) endWith(done <-chan struct{}) {
	go func() {
		select {
		case <-done:
			r.end()
		case <-r.stop:
		}
	}()
}

// wait waits for Run to return, then closes the hub and waits for the
// collector.
func (r *m2aRun) wait() {
	r.waitOnce.Do(func() {
		r.err = <-r.done
		close(r.stop)
		if r.p != nil {
			r.stats = r.p.Stats()
		}
		r.h.Close()
		<-r.col.done
	})
}

// waitActive waits for the activation of input in.
func (r *m2aRun) waitActive(t *testing.T, in int, d time.Duration) {
	t.Helper()
	select {
	case got := <-r.active:
		require.Equal(t, in, got, "activated input")
	case <-time.After(d):
		t.Fatalf("input %d not active within %v", in, d)
	}
}

// failures describes the EventFailed events.
func (r *m2aRun) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.events {
		if ev.Kind == pipeline.EventFailed {
			out = append(out, fmt.Sprintf("input %d: %v", ev.Input, ev.Err))
		}
	}
	return out
}

// check waits for the run to end and checks that the frames are clean, in
// the given epochs, with nothing lost in the hub and, for a pipeline, no
// drift epoch.
func (r *m2aRun) check(t *testing.T, wantEpochs []uint32) {
	t.Helper()
	r.checkWith(t, wantEpochs, conform.FrameOptions{})
}

// checkWith is check with the frame rules tuned by opt.
func (r *m2aRun) checkWith(t *testing.T, wantEpochs []uint32, opt conform.FrameOptions) {
	t.Helper()
	r.wait()
	require.NotErrorIs(t, r.err, context.DeadlineExceeded, "the run did not end within %v", runLimit)
	if r.err != nil {
		require.ErrorIs(t, r.err, context.Canceled)
	}
	require.NotEmpty(t, r.col.frames)
	assert.Zero(t, r.col.lost)
	assert.Nil(t, conform.CheckFrames(r.col.frames, opt))
	assert.Equal(t, wantEpochs, epochs(r.col.frames))
	if r.p == nil {
		return // a feed: no pipeline, no Clock
	}
	assert.Zero(t, r.stats.Clock.DriftBreaks, "drift breaks")
	assert.Zero(t, r.stats.Clock.DroppedDrift, "dropped by drift")
}

// ourSource runs the live clip, paced, through a pipeline into a hub. At
// the clip's end it closes ended, then the hub, so that a reader failing on
// the hub's end finds the source ended. tap collects every frame written.
func ourSource(t *testing.T, clip string) (h *hub.Hub, tap *collector, ended <-chan struct{}) {
	t.Helper()
	h = hub.New(hub.Options{})
	tap = collect(h, "tap")
	end, exited := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(exited)
		err := pipeline.New(h, pipeline.Options{Inputs: []string{fileURL(clip, "")}, EndOnEOF: true}).Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("source pipeline: %v", err)
		}
		close(end)
		h.Close()
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
		<-tap.done
	})
	return h, tap, end
}

// joinedSource is ourSource with a lookup for the stream "src" that records
// the frames the tap had taken at the first lookup: the subscriber's join.
type joinedSource struct {
	h       *hub.Hub
	tap     *collector
	ended   <-chan struct{}
	started time.Time

	mu    sync.Mutex
	since kindMap
}

func newJoinedSource(t *testing.T, clip string) *joinedSource {
	t.Helper()
	s := &joinedSource{started: time.Now()}
	s.h, s.tap, s.ended = ourSource(t, clip)
	return s
}

// waitMidGOP waits until lateJoin has passed since the source started and
// the tap is in the middle of a GOP, so that a reader joining now starts
// with a burst of part of a GOP however loaded the machine is: a join near a
// keyframe could subscribe after the next one, missing frames counted from
// the lookup on.
func (s *joinedSource) waitMidGOP(t *testing.T) {
	t.Helper()
	time.Sleep(time.Until(s.started.Add(lateJoin)))
	require.Eventually(t, func() bool {
		n := s.tap.sinceKey()
		return n >= gopVideo/4 && n <= gopVideo/2
	}, 10*time.Second, time.Millisecond, "the source mid-GOP")
}

func (s *joinedSource) lookup(name string) (*hub.Hub, bool) {
	if name != "src" {
		return nil, false
	}
	s.mu.Lock()
	if s.since == nil {
		s.since = s.tap.counts()
	}
	s.mu.Unlock()
	return s.h, true
}

// afterJoin returns the frames per kind written from the join to the end.
func (s *joinedSource) afterJoin(t *testing.T) kindMap {
	t.Helper()
	<-s.ended
	<-s.tap.done
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotNil(t, s.since, "the source was looked up")
	out := s.tap.counts()
	for k, n := range s.since {
		out[k] -= n
	}
	return out
}

// flushWriter flushes each write to the client, and records whether a
// write or flush failed: the client went away.
type flushWriter struct {
	w      io.Writer
	rc     *http.ResponseController
	failed atomic.Bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	if err != nil {
		f.failed.Store(true)
	}
	return n, err //nolint:wrapcheck // io.Writer contract
}

// everyRow is one input of TestM2aEveryInput. prepare starts the row's
// source, or its run when the run listens; the start it returns is called
// once every row has prepared, so that the rows' waits overlap, and starts
// whatever remains.
type everyRow struct {
	name    string
	live    bool
	burst   bool // the source starts the input with a burst from its GOP cache
	bounds  rowBounds
	prepare func() (start func() rowRun)
}

// rowRun is a started row: its run, and a function that waits for the
// source's end and returns the source's frame counts.
type rowRun struct {
	r      *m2aRun
	source func(t *testing.T) kindMap
}

// TestM2aEveryInput runs every M2a input once. The live rows read the
// 36 s live clip from ffmpeg in real time, or join our outputs fed by it
// 3.5 s in, mid-GOP, and must read for liveSpan with drift evaluated and
// no drift epoch. Each row delivers one epoch of clean frames, counted
// against the source's (the whole clip for the rows fed by ffmpeg and the
// file row, what the source hub received from the join on for the rows fed
// by our outputs) within the row's bounds (rowBounds; the Clock drops one
// audio frame presented before the first keyframe, which is accounted for):
//   - file: exact.
//   - UDP, HTTP and HLS: at most 3 frames short. A TS demuxer holds back
//     the last video frames at a source that goes quiet (Task 12).
//   - UDP over RTP: at most 10 audio frames short. ffmpeg's rtp_mpegts
//     muxer never sends its last audio PES (about ten AAC frames; ffprobe
//     on the payloads it sends counts the same).
//   - SRT: at most 0.75 s short; 0.512 s (24 audio frames, 9 video) on
//     every run measured. ffmpeg closes the connection as soon as it has
//     sent the clip: the packets still inside the receiver's latency window
//     are not delivered, the demuxer's held-back video goes with them, and
//     audio loses whole PES of several frames. 0.75 s stays under one GOP,
//     so a lost GOP still fails.
//   - Our outputs: at most 3 frames short, and at most one GOP over: they
//     start with a burst of the GOP so far.
//
// The rows run at the same time (Ruling R-PF3). They are started from the
// test itself rather than as parallel subtests, so that -parallel, which
// defaults to the CPU count, does not queue rows that mostly wait on real
// time; each row's checks then run in its own subtest. So -run selecting
// one row still starts every row; that is accepted.
func TestM2aEveryInput(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg", "udp", "rtp", "srt")
	clip := liveClip(t, ffmpeg)
	data, err := os.ReadFile(clip)
	require.NoError(t, err)
	src := demuxCounts(t, data)
	whole := func(*testing.T) kindMap { return src }
	ffIn := []string{"-re", "-i", clip, "-c", "copy"}
	ffSource := func(f *ffmpegProc) func(t *testing.T) kindMap {
		return func(t *testing.T) kindMap {
			t.Helper()
			f.requireExited(t)
			return src
		}
	}

	udpRow := func(rtp bool) func() func() rowRun {
		return func() func() rowRun {
			port := freeUDPPort(t)
			r := startRun(t, runOptions{inputs: []string{fmt.Sprintf("udp://@:%d", port)}})
			return func() rowRun {
				waitBound(t, port)
				out := []string{"-f", "mpegts", fmt.Sprintf("udp://127.0.0.1:%d?pkt_size=1316", port)}
				if rtp {
					out = []string{"-f", "rtp_mpegts", fmt.Sprintf("rtp://127.0.0.1:%d?rtcpport=%d", port, freeUDPPort(t))}
				}
				f := startFFmpeg(t, ffmpeg, slices.Concat(ffIn, out)...)
				r.endWith(f.done)
				return rowRun{r, ffSource(f)}
			}
		}
	}
	// hlsRow has ffmpeg write a live HLS stream to out, waits for the files
	// listed, and pulls the playlist.
	hlsRow := func(playlist, out string, files []string, args ...string) func() func() rowRun {
		return func() func() rowRun {
			dir := t.TempDir()
			srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
			t.Cleanup(srv.Close)
			f := startFFmpeg(t, ffmpeg, slices.Concat([]string{"-re", "-i", clip}, args, []string{
				"-f", "hls", "-hls_time", "2", "-hls_list_size", "0", "-hls_flags", "temp_file",
				filepath.Join(dir, out),
			})...)
			return func() rowRun {
				waitFiles(t, dir, files...)
				r := startRun(t, runOptions{inputs: []string{srv.URL + "/" + playlist}})
				r.endWith(f.done)
				return rowRun{r, ffSource(f)}
			}
		}
	}
	// ourRow joins a source hub through the input that url and env make of
	// it, lateJoin after the source started.
	ourRow := func(input func(s *joinedSource) (string, ingest.Env)) func() func() rowRun {
		return func() func() rowRun {
			s := newJoinedSource(t, clip)
			return func() rowRun {
				s.waitMidGOP(t)
				u, env := input(s)
				r := startRun(t, runOptions{inputs: []string{u}, env: env})
				r.endWith(s.ended)
				return rowRun{r, s.afterJoin}
			}
		}
	}

	var (
		exact  rowBounds
		tsTail = rowBounds{short: kindTol{3, 3}}
		rtp    = rowBounds{short: kindTol{3, 10}}
		srt    = rowBounds{short: within(750 * time.Millisecond)}
		burst  = rowBounds{short: kindTol{3, 3}, over: kindTol{gopVideo, gopAudio}}
	)
	rows := []everyRow{
		{"file", false, false, exact, func() func() rowRun {
			return func() rowRun {
				return rowRun{startRun(t, runOptions{inputs: []string{fileURL(clip, "pace=0")}}), whole}
			}
		}},
		{"udp", true, false, tsTail, udpRow(false)},
		{"udp-rtp", true, false, rtp, udpRow(true)},
		{"srt-caller", true, false, srt, func() func() rowRun {
			port := freeUDPPort(t)
			f := startFFmpeg(t, ffmpeg, slices.Concat(ffIn, []string{
				"-f", "mpegts", fmt.Sprintf("srt://127.0.0.1:%d?mode=listener&latency=200000", port),
			})...)
			return func() rowRun {
				waitBound(t, port)
				r := startRun(t, runOptions{inputs: []string{fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&latency=200", port)}})
				r.endWith(f.done)
				return rowRun{r, ffSource(f)}
			}
		}},
		{"srt-listener", true, false, srt, func() func() rowRun {
			port := freeUDPPort(t)
			r := startRun(t, runOptions{inputs: []string{
				fmt.Sprintf("srt://127.0.0.1:%d?mode=listener&streamid=cam&latency=200", port),
			}})
			return func() rowRun {
				waitBound(t, port)
				f := startFFmpeg(t, ffmpeg, slices.Concat(ffIn, []string{
					"-f", "mpegts", fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&streamid=cam&latency=200000", port),
				})...)
				r.endWith(f.done)
				return rowRun{r, ffSource(f)}
			}
		}},
		{"http-ts", true, false, tsTail, func() func() rowRun {
			// The handler streams ffmpeg's output; ended closes before the
			// response ends. A client that goes away first (the run
			// cancelled on a failure path) kills ffmpeg: no error then.
			ended := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer once.Do(func() { close(ended) })
				w.Header().Set("Content-Type", "video/mp2t")
				rc := http.NewResponseController(w)
				w.WriteHeader(http.StatusOK)
				if err := rc.Flush(); err != nil {
					if req.Context().Err() == nil {
						t.Errorf("flush: %v", err)
					}
					return
				}
				cmd := exec.CommandContext(req.Context(), ffmpeg, slices.Concat(
					[]string{"-hide_banner", "-loglevel", "error", "-nostdin"}, ffIn,
					[]string{"-flush_packets", "1", "-f", "mpegts", "pipe:1"})...)
				var stderr bytes.Buffer
				out := &flushWriter{w: w, rc: rc}
				cmd.Stdout, cmd.Stderr = out, &stderr
				if err := cmd.Run(); err != nil && req.Context().Err() == nil && !out.failed.Load() {
					t.Errorf("ffmpeg: %v: %s", err, stderr.String())
				}
			}))
			t.Cleanup(srv.Close)
			return func() rowRun {
				r := startRun(t, runOptions{inputs: []string{srv.URL + "/live.ts"}})
				r.endWith(ended)
				return rowRun{r, func(t *testing.T) kindMap {
					t.Helper()
					select {
					case <-ended:
					case <-time.After(10 * time.Second):
						t.Fatal("the http-ts source did not end")
					}
					return src
				}}
			}
		}},
		{"http-ts-ours", true, true, burst, ourRow(func(s *joinedSource) (string, ingest.Env) {
			srv := httptest.NewServer(httpts.Handler(s.lookup, 0))
			t.Cleanup(srv.Close)
			return srv.URL + "/live/src/stream.ts", ingest.Env{}
		})},
		{"hls-ts", true, false, tsTail, hlsRow("index.m3u8", "index.m3u8", []string{"index.m3u8"}, "-c", "copy")},
		{"hls-fmp4-audio-rendition", true, false, tsTail, hlsRow("master.m3u8", "stream_%v.m3u8",
			[]string{"master.m3u8", "stream_0.m3u8", "stream_1.m3u8"},
			"-map", "0:v", "-map", "0:a", "-c", "copy", "-bsf:a", "aac_adtstoasc",
			"-hls_segment_type", "fmp4", "-master_pl_name", "master.m3u8",
			"-var_stream_map", "v:0,agroup:aud a:0,agroup:aud,default:yes")},
		{"copy", true, true, burst, ourRow(func(s *joinedSource) (string, ingest.Env) {
			return "copy://src", ingest.Env{Lookup: s.lookup}
		})},
	}
	begin := time.Now()
	starts := make([]func() rowRun, len(rows))
	for i, row := range rows {
		starts[i] = row.prepare()
	}
	runs := make([]rowRun, len(rows))
	for i, start := range starts {
		runs[i] = start()
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rr := runs[i]
			rr.r.check(t, []uint32{1})
			want := rr.source(t)
			got := rr.r.col.counts()
			assertRowCounts(t, want, got, rr.r.stats.Clock, row.bounds)
			assert.Zero(t, rr.r.stats.Switches)
			if row.live {
				assert.GreaterOrEqual(t, rr.r.col.span(), liveSpan, "read time")
				// DriftBreaks == 0 means something only if drift was judged.
				assert.Positive(t, rr.r.stats.Clock.DriftEvaluations, "drift evaluated")
			}
			if row.burst {
				video := epochKind{1, media.KindVideo}
				assert.Greater(t, got[video], want[video], "the GOP burst at the join is delivered")
			}
			t.Logf("source %s, delivered %s over %v; clock %+v; failures %v; done %v after the start",
				want, got, rr.r.col.span().Round(time.Millisecond), rr.r.stats.Clock, rr.r.failures(),
				time.Since(begin).Round(time.Millisecond))
		})
	}
}

// TestM2aFailoverSRTtoUDP kills the ffmpeg feeding the primary SRT input
// mid-stream; the pipeline fails over to the UDP backup, fed by a second
// ffmpeg, in exactly one epoch change. The frames lost are bounded by
// StallTimeout worth of source frames plus the backup's wait for a key: the
// stall is detected StallTimeout after the primary's last frame, and the
// backup's epoch then starts at its next keyframe, up to one GOP later
// (Clock rule 5); 0.5 s more covers the demuxer's hold-back and scheduling.
// A live packager on the same hub lists one discontinuity and reads back
// clean.
func TestM2aFailoverSRTtoUDP(t *testing.T) {
	t.Parallel()
	start := time.Now()
	ffmpeg := testpeer.Require(t, "ffmpeg", "udp", "srt")
	clip := liveClip(t, ffmpeg)
	srtPort, udpPort := freeUDPPort(t), freeUDPPort(t)
	primary := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "mpegts",
		fmt.Sprintf("srt://127.0.0.1:%d?mode=listener&latency=200000", srtPort))
	waitBound(t, srtPort)

	var pk *live.Packager
	pkDone := make(chan struct{})
	r := startRun(t, runOptions{
		inputs: []string{
			fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&latency=200", srtPort),
			fmt.Sprintf("udp://@:%d", udpPort),
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
	r.waitActive(t, 0, 15*time.Second)
	waitBound(t, udpPort)
	backup := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "mpegts",
		fmt.Sprintf("udp://127.0.0.1:%d?pkt_size=1316", udpPort))

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

// pcrOf returns the PCR base (90 kHz) of a TS packet that carries one.
func pcrOf(p []byte) (int64, bool) {
	if len(p) < 12 || p[0] != 0x47 || p[3]&0x20 == 0 || p[4] < 7 || p[5]&0x10 == 0 {
		return 0, false
	}
	return int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7, true
}

// sendTS sends data to a local UDP port, seven TS packets per datagram, in
// real time by its PCRs. The returned channel is closed at the end.
func sendTS(t *testing.T, port int, data []byte) <-chan struct{} {
	t.Helper()
	ctx := t.Context()
	conn, err := new(net.Dialer).DialContext(ctx, "udp", "127.0.0.1:"+strconv.Itoa(port))
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		const datagram = 7 * mpegts.PacketSize
		start := time.Now()
		first, reached := int64(-1), int64(0)
		for off := 0; off < len(data); off += datagram {
			chunk := data[off:min(off+datagram, len(data))]
			for p := 0; p+mpegts.PacketSize <= len(chunk); p += mpegts.PacketSize {
				if pcr, ok := pcrOf(chunk[p : p+mpegts.PacketSize]); ok {
					if first < 0 {
						first = pcr
					}
					reached = max(reached, pcr-first)
				}
			}
			if d := time.Until(start.Add(media.ToDuration(reached))); d > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
				}
			}
			if _, err := conn.Write(chunk); err != nil {
				t.Errorf("send: %v", err)
				return
			}
		}
	}()
	t.Cleanup(func() { <-done })
	return done
}

// udpFixtureRun sends a fixture over UDP in real time into a pipeline
// listening for it, and returns the run once its source has ended; the run
// ends when the input stalls.
func udpFixtureRun(t *testing.T, name string, open func(context.Context, string) (ingest.Reader, error)) *m2aRun {
	t.Helper()
	port := freeUDPPort(t)
	r := startRun(t, runOptions{inputs: []string{fmt.Sprintf("udp://@:%d", port)}, open: open})
	waitBound(t, port)
	r.endWith(sendTS(t, port, fixtureBytes(t, name)))
	return r
}

// TestM2aPMTChange sends h264_aac_pmtchange.ts over UDP: the PMT change
// that moves every track to new PIDs is one epoch change, and every track
// carries on.
func TestM2aPMTChange(t *testing.T) {
	t.Parallel()
	start := time.Now()
	r := udpFixtureRun(t, "h264_aac_pmtchange.ts", nil)
	r.check(t, []uint32{1, 2})
	want := demuxCounts(t, fixtureBytes(t, "h264_aac_pmtchange.ts"))
	got := r.col.counts()
	assertWithinGOP(t, want, got)
	per := kindCounts(r.col.frames)
	for _, k := range []epochKind{{1, media.KindVideo}, {1, media.KindAudio}, {2, media.KindVideo}, {2, media.KindAudio}} {
		assert.Positive(t, per[k], "epoch %d %v", k.epoch, k.kind)
	}
	t.Logf("source %s, delivered %s; clock %+v; took %v", want, per, r.stats.Clock, time.Since(start).Round(time.Millisecond))
}

// tapReader records the frames its reader returns before Close. A TS reader
// returns the frames its demuxer still holds once closed; the pipeline that
// closed it never takes them.
type tapReader struct {
	ingest.Reader

	mu     sync.Mutex
	closed bool
	frames []media.Frame
}

func (r *tapReader) Read() (ingest.Frame, error) {
	f, err := r.Reader.Read()
	r.mu.Lock()
	if err == nil && !r.closed {
		r.frames = append(r.frames, f.Frame)
	}
	r.mu.Unlock()
	return f, err //nolint:wrapcheck // the reader's errors pass through
}

func (r *tapReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return r.Reader.Close() //nolint:wrapcheck // the reader's errors pass through
}

// TestM2aOpenGOP sends h264_opengop_midgop.ts, an H.264 open-GOP feed cut
// mid-GOP, over UDP: video starts at its first recovery point (PTS 57600
// from the start of the file, testdata/fixtures/README.md), the leading
// pictures of that GOP are dropped, and every later video frame is
// delivered.
func TestM2aOpenGOP(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var taps []*tapReader
	r := udpFixtureRun(t, "h264_opengop_midgop.ts", func(ctx context.Context, u string) (ingest.Reader, error) {
		rd, err := ingest.Open(ctx, u, ingest.Env{})
		if err != nil {
			return nil, err //nolint:wrapcheck // the pipeline wraps it
		}
		tr := &tapReader{Reader: rd}
		mu.Lock()
		taps = append(taps, tr)
		mu.Unlock()
		return tr, nil
	})
	r.check(t, []uint32{1})
	require.Len(t, taps, 1, "one open")

	// The reader's video frames: the first is the recovery point.
	var src []media.Frame
	for _, f := range taps[0].frames {
		if f.Kind() == media.KindVideo {
			src = append(src, f)
		}
	}
	require.NotEmpty(t, src)
	key := src[0]
	require.True(t, key.Key, "the reader starts video at a key")
	assert.Equal(t, int64(57600), key.PTS, "the first recovery point")

	// Delivered video, in source time: the key, then every frame presented
	// after it; the leading pictures presented before it are dropped.
	var got []int64
	for _, f := range r.col.frames {
		if f.Kind() == media.KindVideo {
			got = append(got, f.PTS)
		}
	}
	require.NotEmpty(t, got)
	off := got[0] - key.PTS
	for i := range got {
		got[i] -= off
	}
	var want []int64
	leading := 0
	for _, f := range src {
		if f.PTS < key.PTS {
			leading++
			continue
		}
		want = append(want, f.PTS)
	}
	assert.Equal(t, want, got)
	assert.Equal(t, 2, leading, "leading pictures of the first GOP")
	assert.Equal(t, uint64(leading), r.stats.Clock.DroppedRASL)
	t.Logf("%d video frames read, %d delivered, %d leading dropped; clock %+v", len(src), len(got), leading, r.stats.Clock)
}
