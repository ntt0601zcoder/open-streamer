//go:build unix

package transcode

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

func TestMain(m *testing.M) {
	if os.Getenv("OS_TRANSCODE_FAKE") == "1" {
		transcodetest.Main()
	}
	os.Exit(m.Run())
}

// fakeOptions runs the test binary as the fake child.
func fakeOptions(t *testing.T, env ...string) Options {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return Options{
		Binary: exe,
		// A race-built child sleeps 1 s at exit unless told otherwise.
		Env:        append([]string{"OS_TRANSCODE_FAKE=1", "GORACE=atexit_sleep_ms=0"}, env...),
		RespawnMin: 20 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
}

var (
	encode240 = Rendition{Name: "t240", Video: Video{Height: 240, BitrateKbps: 600}}
	encode180 = Rendition{Name: "t180", Video: Video{Height: 180, BitrateKbps: 300}}
	copySrc   = Rendition{Name: "src", Video: Video{Copy: true}}
)

// events records a transcoder's events.
type events struct {
	mu  sync.Mutex
	evs []Event
	at  []time.Time
}

func (e *events) add(ev Event) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	e.at = append(e.at, time.Now())
	e.mu.Unlock()
}

// firstAt returns when the first event of kind k was recorded.
func (e *events) firstAt(k EventKind) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, ev := range e.evs {
		if ev.Kind == k {
			return e.at[i]
		}
	}
	return time.Time{}
}

func (e *events) of(k EventKind) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Event
	for _, ev := range e.evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

// session is one transcoder run over a fed source hub.
type session struct {
	tr   *Transcoder
	src  *hub.Hub
	cols []*collector
	ev   *events
	errc chan error
}

// start builds a transcoder over a new source hub and runs it.
func start(t *testing.T, opt Options) *session {
	t.Helper()
	return startWith(t, opt, nil)
}

// startWith is start, calling prepare, if set, on the transcoder before
// Run.
func startWith(t *testing.T, opt Options, prepare func(*Transcoder)) *session {
	t.Helper()
	s := &session{src: hub.New(hub.Options{}), ev: &events{}, errc: make(chan error, 1)}
	opt.OnEvent = s.ev.add
	tr, err := New(s.src, opt)
	require.NoError(t, err)
	if prepare != nil {
		prepare(tr)
	}
	s.tr = tr
	for _, h := range tr.Hubs() {
		s.cols = append(s.cols, collect(h))
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { s.errc <- tr.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.errc:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})
	waitSub(t, s.src, "transcode")
	return s
}

// feed plays steps into the source, closes it and waits for Run.
func (s *session) feed(t *testing.T, steps []step, speed float64) [][]media.Frame {
	t.Helper()
	<-play(t.Context(), s.src, steps, speed, true)
	select {
	case err := <-s.errc:
		require.NoError(t, err)
		s.errc <- nil
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the source closed")
	}
	out := make([][]media.Frame, len(s.cols))
	for i, c := range s.cols {
		out[i] = c.wait(t)
		c.mu.Lock()
		assert.Empty(t, c.missing, "hub %d: epochs without an anchor before their first frame", i)
		c.mu.Unlock()
	}
	return out
}

// shifted returns frames shifted by Delay, sorted by track then DTS.
func shifted(fs []media.Frame) []media.Frame {
	out := slices.Clone(fs)
	for i := range out {
		out[i].PTS += Delay
		out[i].DTS += Delay
	}
	return sortTD(out)
}

func sortTD(fs []media.Frame) []media.Frame {
	out := slices.Clone(fs)
	slices.SortStableFunc(out, func(a, b media.Frame) int {
		if c := cmp.Compare(a.Track, b.Track); c != 0 {
			return c
		}
		return cmp.Compare(a.DTS, b.DTS)
	})
	return out
}

// assertSameFrames compares frames field by field, configs by Equal.
func assertSameFrames(t *testing.T, want, got []media.Frame, msg string) {
	t.Helper()
	require.Len(t, got, len(want), msg)
	for i := range want {
		w, g := want[i], got[i]
		g2 := g
		g2.Config = w.Config
		if !assert.Equal(t, w, g2, "%s: frame %d", msg, i) {
			return
		}
		assert.True(t, w.Config.Equal(g.Config), "%s: frame %d config", msg, i)
	}
}

func check(t *testing.T, fs []media.Frame, msg string) {
	t.Helper()
	assert.Empty(t, conform.CheckFrames(fs, conform.FrameOptions{}), msg)
}

func srcEpochs(steps []step) []uint32 { return epochs(frames(steps)) }

func TestSupervisorLadderFake(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := fakeOptions(t)
	opt.Renditions = []Rendition{encode240, encode180, copySrc}
	s := start(t, opt)
	out := s.feed(t, steps, 0)
	want := shifted(frames(steps))
	for i, fs := range out {
		assertSameFrames(t, want, sortTD(fs), opt.Renditions[i].Name)
		check(t, fs, opt.Renditions[i].Name)
		assert.Equal(t, srcEpochs(steps), epochs(fs), opt.Renditions[i].Name)
	}
	st := s.tr.Stats()
	assert.EqualValues(t, 1, st.Spawns)
	assert.EqualValues(t, len(want), st.FramesIn)
	for i := range out {
		assert.EqualValues(t, len(want), st.FramesOut[i])
	}
	assert.Zero(t, st.MergeDropped)
	assert.Zero(t, st.DroppedBeforeKey)
	assert.LessOrEqual(t, st.MaxQueued, childQueue)
}

// assertOneExtraEpoch checks that every hub got exactly one epoch beyond
// the source's, starting on a video keyframe with its anchor at K + D.
func assertOneExtraEpoch(t *testing.T, s *session, out [][]media.Frame, steps []step) {
	t.Helper()
	want := len(srcEpochs(steps)) + 1
	off := len(s.cols) - len(out) // out may be a tail of the hubs
	for j, fs := range out {
		i := j + off
		eps := epochs(fs)
		require.Len(t, eps, want, "hub %d epochs %v", i, eps)
		last := eps[len(eps)-1]
		first := firstOfEpoch(fs, last)
		assert.Equal(t, media.KindVideo, first.Kind(), "hub %d", i)
		assert.True(t, first.Key, "hub %d", i)
		s.cols[i].mu.Lock()
		a, ok := s.cols[i].anchors[last]
		s.cols[i].mu.Unlock()
		require.True(t, ok, "hub %d: anchor of epoch %d", i, last)
		assert.Equal(t, first.PTS, a.Start, "hub %d: bump anchor Start is K + D", i)
	}
}

func crashOnce(t *testing.T, after int) []string {
	t.Helper()
	return []string{
		transcodetest.EnvCrashAfter + "=" + strconv.Itoa(after),
		transcodetest.EnvCrashOnce + "=" + filepath.Join(t.TempDir(), "crashed"),
	}
}

func TestSupervisorRespawnOneEpoch(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := fakeOptions(t, crashOnce(t, 50)...)
	opt.Renditions = []Rendition{encode240, encode180, copySrc}
	s := start(t, opt)
	out := s.feed(t, steps, 4)
	assertOneExtraEpoch(t, s, out, steps)
	for i, fs := range out {
		check(t, fs, opt.Renditions[i].Name)
	}
	st := s.tr.Stats()
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, st.Exits)
	assert.Len(t, s.ev.of(EventSpawned), 2)
	require.Len(t, s.ev.of(EventExited), 1)
	assert.NotZero(t, s.ev.of(EventExited)[0].PID)
}

// A child whose video comes out 1 s (25 frames) after its audio, as x264's
// delay makes it, still gives every rendition hub an epoch that starts on
// its video keyframe, with no audio written ahead of it: the merger waits
// for the rung's video in each new epoch, at the start and after a respawn.
// Before, the child's first second of audio went out ahead of the IDR, and
// a subscriber that joined on the IDR (the GOP cache) never got it.
func TestSupervisorVideoLag(t *testing.T) {
	lag := transcodetest.EnvVideoLag + "=25"
	// assertVideoFirst checks each epoch of each hub that has video; it
	// returns how many epochs had none.
	assertVideoFirst := func(t *testing.T, out [][]media.Frame, names []string) (noVideo int) {
		t.Helper()
		for i, fs := range out {
			for _, e := range epochs(fs) {
				if !slices.ContainsFunc(fs, func(f media.Frame) bool { return f.Epoch == e && f.Kind() == media.KindVideo }) {
					noVideo++
					continue
				}
				f := firstOfEpoch(fs, e)
				assert.Equal(t, media.KindVideo, f.Kind(), "%s: epoch %d starts with video", names[i], e)
				assert.True(t, f.Key, "%s: epoch %d starts on a keyframe", names[i], e)
			}
		}
		return noVideo
	}
	names := []string{encode240.Name, encode180.Name, copySrc.Name}
	t.Run("start", func(t *testing.T) {
		steps := fixtureSteps(t, "h264_aac.ts")
		opt := fakeOptions(t, lag)
		opt.Renditions = []Rendition{encode240, encode180, copySrc}
		s := start(t, opt)
		out := s.feed(t, steps, 0)
		assert.Zero(t, assertVideoFirst(t, out, names))
		want := shifted(frames(steps))
		for i, fs := range out {
			assertSameFrames(t, want, sortTD(fs), names[i])
			check(t, fs, names[i])
		}
		assert.Zero(t, s.tr.Stats().MergeDropped)
	})
	t.Run("respawn", func(t *testing.T) {
		steps := fixtureSteps(t, "h264_aac.ts")
		opt := fakeOptions(t, append(crashOnce(t, 60), lag)...)
		opt.Renditions = []Rendition{encode240, encode180, copySrc}
		s := start(t, opt)
		out := s.feed(t, steps, 4)
		assertOneExtraEpoch(t, s, out, steps)
		// After 60 input frames the child has put out some epoch 1 video
		// (the rest dies with it): every epoch of every hub has video.
		assert.Zero(t, assertVideoFirst(t, out, names))
		for i, fs := range out {
			check(t, fs, names[i])
		}
	})
}

// The media counters each child reports in Stats are summed over the
// children of the run (ruling R-T6a): the fake reports Counters once after
// Configure and again on every tick, and crashes once.
func TestSupervisorChildCounters(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := fakeOptions(t, append(crashOnce(t, 50),
		transcodetest.EnvCounters+"=1", transcodetest.EnvStatsInterval+"=10ms")...)
	opt.Renditions = []Rendition{encode240, copySrc}
	s := start(t, opt)
	s.feed(t, steps, 4)
	st := s.tr.Stats()
	require.EqualValues(t, 2, st.Spawns)
	c := transcodetest.Counters
	assert.Equal(t, ChildStats{
		DroppedTracks: 2 * c.DroppedTracks, Reanchors: 2 * c.Reanchors, AudioDropped: 2 * c.AudioDropped,
		AudioCorrupt: 2 * c.AudioCorrupt, VideoCorrupt: 2 * c.VideoCorrupt,
	}, st.Child)
}

func TestChildStatsAdd(t *testing.T) {
	var s ChildStats
	s.add(ChildStats{}, ChildStats{Reanchors: 2, AudioDropped: 10})
	s.add(ChildStats{Reanchors: 2, AudioDropped: 10}, ChildStats{Reanchors: 3, AudioDropped: 4, VideoCorrupt: 1})
	assert.Equal(t, ChildStats{Reanchors: 3, AudioDropped: 10, VideoCorrupt: 1}, s, "a falling counter adds nothing")
	// The reader keeps the per-counter maximum: a counter that fell (10 to
	// 4) adds again only above 10.
	prev := ChildStats{Reanchors: 2, AudioDropped: 10}.max(ChildStats{Reanchors: 3, AudioDropped: 4, VideoCorrupt: 1})
	assert.Equal(t, ChildStats{Reanchors: 3, AudioDropped: 10, VideoCorrupt: 1}, prev)
	s.add(prev, ChildStats{Reanchors: 3, AudioDropped: 12, VideoCorrupt: 1})
	assert.Equal(t, ChildStats{Reanchors: 3, AudioDropped: 12, VideoCorrupt: 1}, s)
}

func TestSupervisorMixedRespawn(t *testing.T) {
	plain := fixtureSteps(t, "h264_aac.ts")
	copyVideo := []Rendition{copySrc}
	encodeVideo := []Rendition{encode240}
	for _, tc := range []struct {
		name       string
		steps      []step
		renditions []Rendition
		copyAudio  bool
	}{
		{"plain", plain, copyVideo, false},
		{"audio-leads-300ms", audioLead(plain, 27000), copyVideo, false},
		{"hevc-open-gop", fixtureSteps(t, "h265_aac51.ts"), copyVideo, false},
		{"encoded-video-copied-audio", plain, encodeVideo, true},
		{"encoded-video-copied-audio-leads", audioLead(plain, 27000), encodeVideo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := fakeOptions(t, crashOnce(t, 50)...)
			opt.Renditions = tc.renditions
			opt.Audio.Copy = tc.copyAudio
			s := start(t, opt)
			out := s.feed(t, tc.steps, 4)
			assertOneExtraEpoch(t, s, out, tc.steps)
			v := conform.CheckFrames(out[0], conform.FrameOptions{})
			assert.Empty(t, v)
			for _, x := range v {
				assert.NotEqual(t, conform.RuleRASL, x.Rule)
			}
			assert.NotZero(t, s.tr.Stats().DroppedBeforeKey)
			assert.EqualValues(t, 2, s.tr.Stats().Spawns)
			if !tc.renditions[0].Video.Copy {
				// The new epoch starts on the new child's IDR: a video
				// keyframe whose config came over the wire, not the source's.
				eps := epochs(out[0])
				first := firstOfEpoch(out[0], eps[len(eps)-1])
				for _, f := range frames(tc.steps) {
					require.NotSame(t, f.Config, first.Config)
				}
			}
		})
	}
}

// recordAfter fires the first n backoff timers at once and blocks the next.
type recordAfter struct {
	mu     sync.Mutex
	n      int
	delays []time.Duration
	full   chan struct{}
}

func (r *recordAfter) After(d time.Duration) <-chan time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delays = append(r.delays, d)
	if len(r.delays) > r.n {
		if len(r.delays) == r.n+1 {
			close(r.full)
		}
		return nil
	}
	c := make(chan time.Time, 1)
	c <- time.Now()
	return c
}

func TestSupervisorCrashLoopBacksOff(t *testing.T) {
	ra := &recordAfter{n: 7, full: make(chan struct{})}
	opt := fakeOptions(t, transcodetest.EnvCrashAfter+"=0")
	opt.RespawnMin = 0 // default 250 ms
	opt.After = ra.After
	opt.Renditions = []Rendition{encode240}
	s := start(t, opt)
	select {
	case <-ra.full:
	case <-time.After(30 * time.Second):
		t.Fatal("no eighth backoff")
	}
	ra.mu.Lock()
	ms := time.Millisecond
	assert.Equal(t, []time.Duration{250 * ms, 500 * ms, 1000 * ms, 2000 * ms, 4000 * ms, 5000 * ms, 5000 * ms}, ra.delays[:7])
	ra.mu.Unlock()
	assert.EqualValues(t, 8, s.tr.Stats().Spawns)
	assert.EqualValues(t, 8, s.tr.Stats().Exits)
}

func TestSupervisorBackoffResetsWhenHealthy(t *testing.T) {
	ra := &recordAfter{n: 3, full: make(chan struct{})}
	opt := fakeOptions(t, transcodetest.EnvCrashAfter+"=0")
	opt.RespawnMin = 0
	opt.After = ra.After
	var mu sync.Mutex
	now := time.Unix(0, 0)
	opt.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(40 * time.Second) // every child looks healthy
		return now
	}
	opt.Renditions = []Rendition{encode240}
	start(t, opt)
	<-ra.full
	ra.mu.Lock()
	defer ra.mu.Unlock()
	for _, d := range ra.delays {
		assert.Equal(t, 250*time.Millisecond, d)
	}
}

func TestSupervisorMissingBinary(t *testing.T) {
	ra := &recordAfter{n: 3, full: make(chan struct{})}
	opt := fakeOptions(t)
	opt.Binary = "/nonexistent"
	opt.RespawnMin = 0
	opt.After = ra.After
	opt.Renditions = []Rendition{encode240, copySrc}
	s := &session{src: hub.New(hub.Options{}), ev: &events{}}
	opt.OnEvent = s.ev.add
	tr, err := New(s.src, opt)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- tr.Run(ctx) }()
	<-ra.full
	exits := s.ev.of(EventExited)
	require.NotEmpty(t, exits)
	err = exits[0].Err
	assert.True(t, errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist), "%v", err)
	ra.mu.Lock()
	assert.Equal(t, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}, ra.delays[:3])
	ra.mu.Unlock()
	for _, h := range tr.Hubs() {
		select {
		case <-h.Done():
			t.Fatal("rendition hub closed before cancel")
		default:
		}
	}
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)
	for _, h := range tr.Hubs() {
		<-h.Done()
	}
}

func TestSupervisorVersionMismatch(t *testing.T) {
	opt := fakeOptions(t, transcodetest.EnvVersion+"=99")
	opt.Renditions = []Rendition{encode240}
	tr, err := New(hub.New(hub.Options{}), opt)
	require.NoError(t, err)
	errc := make(chan error, 1)
	go func() { errc <- tr.Run(t.Context()) }()
	select {
	case err := <-errc:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not fail")
	}
	assert.EqualValues(t, 1, tr.Stats().Spawns)
}

func TestSupervisorAudioOnly(t *testing.T) {
	steps := audioOnly(fixtureSteps(t, "h264_aac.ts"))
	opt := fakeOptions(t)
	opt.Renditions = []Rendition{encode240}
	s := start(t, opt)
	out := s.feed(t, steps, 0)
	want := shifted(frames(steps))
	assertSameFrames(t, want, sortTD(out[0]), "audio")
	for _, f := range out[0] {
		require.Equal(t, media.KindAudio, f.Kind())
	}
	check(t, out[0], "audio")
	assert.EqualValues(t, len(want), s.tr.childOut.Load(), "every frame came from the child")
}

func TestSupervisorSlowChildLoss(t *testing.T) {
	loop := "h264_aac_loop.ts"
	steps := audioLead(fixtureSteps(t, loop, loop, loop, loop), 27000)
	opt := fakeOptions(t, transcodetest.EnvDelay+"=20ms")
	opt.Queue = 64
	opt.Renditions = []Rendition{encode240, copySrc}
	s := start(t, opt)
	out := s.feed(t, steps, 8)
	st := s.tr.Stats()
	t.Logf("losses %d, bumps %d, epochs %d, dropped before key %d, merge dropped %d, max queued %d",
		st.Losses, s.tr.bumps.Load(), len(s.ev.of(EventEpoch)), st.DroppedBeforeKey, st.MergeDropped, st.MaxQueued)
	losses := s.ev.of(EventLoss)
	require.NotEmpty(t, losses)
	assert.EqualValues(t, len(losses), st.Losses)
	// One bump per loss, absorbed while one is pending: at least one bump
	// took effect, and at most one per loss.
	bumps := s.tr.bumps.Load()
	assert.GreaterOrEqual(t, bumps, uint64(1))
	assert.LessOrEqual(t, bumps, uint64(len(losses)))
	assert.LessOrEqual(t, st.MaxQueued, childQueue)
	for i, fs := range out {
		check(t, fs, opt.Renditions[i].Name)
	}
}

func TestSupervisorStopsChildOnCancel(t *testing.T) {
	base := runtime.NumGoroutine()
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := fakeOptions(t)
	opt.Renditions = []Rendition{encode240, copySrc}
	ev := &events{}
	opt.OnEvent = ev.add
	src := hub.New(hub.Options{})
	tr, err := New(src, opt)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- tr.Run(ctx) }()
	waitSub(t, src, "transcode")
	pctx, pcancel := context.WithCancel(t.Context())
	played := play(pctx, src, steps, 2, false)
	require.Eventually(t, func() bool { return tr.Stats().FramesOut[0] > 20 }, 10*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-errc, context.Canceled)
	pcancel()
	<-played
	spawned := ev.of(EventSpawned)
	require.Len(t, spawned, 1)
	pid := spawned[0].PID
	assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "child %d not reaped", pid)
	assert.Zero(t, tr.Stats().PID)
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(5 * time.Second); n > base && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, n, base, "goroutines back to the baseline")
}

func TestNewRejects(t *testing.T) {
	src := hub.New(hub.Options{})
	for name, opt := range map[string]Options{
		"no renditions": {Binary: "x"},
		"bframes 4":     {Binary: "x", Renditions: []Rendition{{Name: "a", Video: Video{Height: 240, BitrateKbps: 1, BFrames: 4}}}},
		"no height":     {Binary: "x", Renditions: []Rendition{{Name: "a", Video: Video{BitrateKbps: 1}}}},
		"odd height":    {Binary: "x", Renditions: []Rendition{{Name: "a", Video: Video{Height: 181, BitrateKbps: 1}}}},
		"no bitrate":    {Binary: "x", Renditions: []Rendition{{Name: "a", Video: Video{Height: 240}}}},
		"duplicate":     {Binary: "x", Renditions: []Rendition{copySrc, copySrc}},
		"no binary":     {Renditions: []Rendition{encode240}},
		"bad audio":     {Binary: "x", Renditions: []Rendition{copySrc}, Audio: Audio{BitrateKbps: -1}},
	} {
		_, err := New(src, opt)
		assert.Error(t, err, name)
	}
	_, err := New(nil, Options{})
	assert.Error(t, err)
	// Copy-only needs no binary.
	_, err = New(src, Options{Renditions: []Rendition{copySrc}, Audio: Audio{Copy: true}})
	assert.NoError(t, err)
}

// New takes at most wire.MaxRungs encoded renditions, the most one
// Configure carries; copied renditions do not count.
func TestNewEncodedRenditionLimit(t *testing.T) {
	ladder := func(n int) []Rendition {
		out := make([]Rendition, 0, 1+n)
		out = append(out, copySrc)
		for i := range n {
			out = append(out, Rendition{Name: "r" + strconv.Itoa(i), Video: Video{Height: 240, BitrateKbps: 300}})
		}
		return out
	}
	src := hub.New(hub.Options{})
	_, err := New(src, Options{Binary: "x", Renditions: ladder(wire.MaxRungs)})
	require.NoError(t, err)
	_, err = New(src, Options{Binary: "x", Renditions: ladder(wire.MaxRungs + 1)})
	require.ErrorContains(t, err, "65 encoded renditions, at most 64")
}

// A copy-only transcoder spawns no child and passes every frame through.
func TestSupervisorCopyOnly(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	s := start(t, Options{Renditions: []Rendition{copySrc}, Audio: Audio{Copy: true}})
	out := s.feed(t, steps, 0)
	assertSameFrames(t, shifted(frames(steps)), sortTD(out[0]), "copy")
	check(t, out[0], "copy")
	assert.Zero(t, s.tr.Stats().Spawns)
	require.Error(t, s.tr.Run(t.Context()), "second Run")
}

func TestLogWriterLines(t *testing.T) {
	var buf strings.Builder
	w := newLogWriter(slog.New(slog.NewTextHandler(&buf, nil)), nil)
	w.setPID(7)
	n, err := w.Write([]byte("one\ntw"))
	require.NoError(t, err)
	assert.Equal(t, 6, n)
	_, _ = w.Write([]byte("o\n"))
	_, _ = w.Write(bytes.Repeat([]byte("x"), 5000))
	out := buf.String()
	assert.Contains(t, out, "line=one")
	assert.Contains(t, out, "line=two")
	assert.Equal(t, 3, strings.Count(out, "pid=7"))
}

// recordHandler is a slog.Handler that keeps every record.
type recordHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

// text renders r as its message followed by its attributes.
func recordText(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.String())
		return true
	})
	return b.String()
}

// texts returns the rendered records that contain sub.
func (h *recordHandler) texts(sub string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		if txt := recordText(r); strings.Contains(txt, sub) {
			out = append(out, txt)
		}
	}
	return out
}

// count returns the records whose message contains sub.
func (h *recordHandler) count(sub string) int {
	return len(h.matching(sub))
}

// matching returns the messages of the records that contain sub.
func (h *recordHandler) matching(sub string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		if strings.Contains(r.Message, sub) {
			out = append(out, r.Message)
		}
	}
	return out
}

// A child that floods its stderr and then panics (exit 2) has its panic
// line logged although the burst was spent.
func TestSupervisorCrashAfterStderrFlood(t *testing.T) {
	h := &recordHandler{}
	opt := fakeOptions(t, transcodetest.EnvStderrLines+"=1000", transcodetest.EnvStderrPanic+"=1")
	opt.Logger = slog.New(h)
	opt.Renditions = []Rendition{encode240}
	opt.RespawnMin = time.Hour // one child is enough
	start(t, opt)
	require.Eventually(t, func() bool { return len(h.texts("panic: boom")) > 0 }, 10*time.Second, time.Millisecond)
	got := h.texts("panic: boom")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "last suppressed stderr lines")
	assert.LessOrEqual(t, h.count("transcode: child stderr"), 3*stderrBurst)
}

// The child's stderr is logged at most stderrBurst lines per second; the
// rest is counted, and summarised once per summary period and at the end.
func TestLogWriterRateLimit(t *testing.T) {
	h := &recordHandler{}
	var suppressed atomic.Uint64
	w := newLogWriter(slog.New(h), func(n uint64) { suppressed.Add(n) })
	now := time.Unix(100, 0)
	w.now = func() time.Time { return now }
	w.every = 50 * time.Millisecond
	w.setPID(7)
	lines := func(n int) {
		for i := range n {
			_, err := w.Write([]byte("line " + strconv.Itoa(i) + "\n"))
			require.NoError(t, err)
		}
	}
	lines(25)
	assert.Equal(t, stderrBurst, h.count("child stderr"))
	assert.EqualValues(t, 15, suppressed.Load())
	require.Eventually(t, func() bool { return h.count("suppressed") == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, []string{"transcode: child 7: suppressed 15 stderr lines"}, h.matching("suppressed"))

	// A new second allows a new burst. The next summary is left to close.
	w.every = time.Hour
	now = now.Add(time.Second)
	lines(5)
	assert.Equal(t, stderrBurst+5, h.count("child stderr"))
	lines(20)
	assert.Equal(t, 2*stderrBurst, h.count("child stderr"))
	assert.EqualValues(t, 30, suppressed.Load())

	// close logs a partial last line if the burst allows it, and the
	// pending summary at once.
	_, _ = w.Write([]byte("partial"))
	w.close(false)
	assert.Equal(t, 2*stderrBurst, h.count("child stderr"))
	assert.EqualValues(t, 31, suppressed.Load())
	assert.Equal(t, []string{
		"transcode: child 7: suppressed 15 stderr lines",
		"transcode: child 7: suppressed 16 stderr lines",
	}, h.matching("suppressed"))
	assert.Zero(t, h.count("last suppressed"), "a clean exit logs no suppressed lines")
}

// A child that exits abnormally gets its last suppressed stderr lines
// (at most stderrKeep) logged in one record, so a panic or a crash report
// behind a burst of noise is not lost.
func TestLogWriterKeepsLastLinesOnCrash(t *testing.T) {
	h := &recordHandler{}
	w := newLogWriter(slog.New(h), nil)
	w.setPID(9)
	for i := range 100 {
		_, _ = w.Write([]byte("noise " + strconv.Itoa(i) + "\n"))
	}
	_, _ = w.Write([]byte("panic: boom\ngoroutine 1 [running]:"))
	w.close(true)
	got := h.texts("last suppressed")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "panic: boom")
	assert.Contains(t, got[0], "goroutine 1 [running]:")
	assert.Contains(t, got[0], "noise 82")
	assert.NotContains(t, got[0], "noise 81", "only the last stderrKeep lines")
	assert.Equal(t, stderrBurst, h.count("child stderr"))
}

// A child that floods its stderr (1000 lines at once) gets a burst of
// records, not one per line; the rest is counted in Stats.StderrSuppressed
// and summarised when the child ends.
func TestSupervisorStderrRateLimited(t *testing.T) {
	h := &recordHandler{}
	opt := fakeOptions(t, transcodetest.EnvStderrLines+"=1000")
	opt.Logger = slog.New(h)
	opt.Renditions = []Rendition{encode240}
	s := start(t, opt)
	s.feed(t, fixtureSteps(t, "h264_aac.ts"), 0)
	logged := h.count("transcode: child stderr")
	st := s.tr.Stats()
	t.Logf("logged %d, suppressed %d", logged, st.StderrSuppressed)
	assert.GreaterOrEqual(t, logged, stderrBurst)
	assert.LessOrEqual(t, logged, 3*stderrBurst)
	assert.EqualValues(t, 1000, uint64(logged)+st.StderrSuppressed)
	var summed uint64
	for _, m := range h.matching("stderr lines") {
		var pid int
		var n uint64
		_, err := fmt.Sscanf(m, "transcode: child %d: suppressed %d stderr lines", &pid, &n)
		require.NoError(t, err, m)
		summed += n
	}
	assert.Equal(t, st.StderrSuppressed, summed, "every suppressed line is in a summary")
}

// A frame the wire protocol cannot carry (more NAL units than the child's
// reader takes, or more than wire.MaxMessage bytes) is dropped on its way
// to the child and counted in Stats.Oversize; the child is not killed, and
// the copy rendition still gets the frame.
func TestSupervisorOversizeFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		data [][]byte
	}{
		{"70000 NALs", make([][]byte, 70000)},
		{"33 MiB", [][]byte{make([]byte, 33<<20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := fixtureSteps(t, "h264_aac.ts")
			var at int
			for i, st := range steps {
				if st.anchor == nil && st.f.Kind() == media.KindVideo && !st.f.Key && i > len(steps)/2 {
					at = i
					break
				}
			}
			require.NotZero(t, at)
			steps[at].f.Data = tc.data
			opt := fakeOptions(t)
			opt.Renditions = []Rendition{encode240, copySrc}
			opt.Audio.Copy = true
			s := start(t, opt)
			out := s.feed(t, steps, 0)
			st := s.tr.Stats()
			assert.EqualValues(t, 1, st.Oversize)
			assert.EqualValues(t, 1, st.Spawns)
			assert.Zero(t, st.Exits, "the child is not killed")
			assert.Empty(t, s.ev.of(EventExited))
			var srcVideo, encVideo, copyVideo int
			for _, f := range frames(steps) {
				if f.Kind() == media.KindVideo {
					srcVideo++
				}
			}
			for _, f := range out[0] {
				if f.Kind() == media.KindVideo {
					encVideo++
				}
			}
			for _, f := range out[1] {
				if f.Kind() == media.KindVideo {
					copyVideo++
				}
			}
			assert.Equal(t, srcVideo-1, encVideo, "the encoded rendition lacks only the oversize frame")
			assert.Equal(t, srcVideo, copyVideo, "the copy rendition has every frame")
		})
	}
}

// A child whose wire config table is full (wire.ErrConfigTable) is
// killed and respawned with a fresh table, with one bump; frames are not
// silently dropped as for wire.ErrLimit.
func TestSupervisorConfigTableFull(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := fakeOptions(t)
	opt.Renditions = []Rendition{encode240, copySrc}
	opt.Audio.Copy = true
	var sent atomic.Int64
	s := startWith(t, opt, func(tr *Transcoder) {
		tr.writeMsg = func(c *child, m wire.Message) error {
			if _, ok := m.(wire.Frame); ok && sent.Add(1) == 60 {
				return fmt.Errorf("wire: write: %w", wire.ErrConfigTable)
			}
			return c.w.Write(m)
		}
	})
	out := s.feed(t, steps, 8)
	st := s.tr.Stats()
	assert.EqualValues(t, 2, st.Spawns, "respawned")
	assert.EqualValues(t, 1, st.Exits)
	assert.Zero(t, st.Oversize)
	assert.EqualValues(t, 1, s.tr.bumps.Load())
	assertOneExtraEpoch(t, s, out, steps)
}

// A child output for a rung that does not exist is logged once per child,
// not once per frame, and dropped.
func TestFromChildUnknownRungLoggedOnce(t *testing.T) {
	h := &recordHandler{}
	tr, err := New(hub.New(hub.Options{}), Options{
		Renditions: []Rendition{copySrc}, Audio: Audio{Copy: true}, Logger: slog.New(h),
	})
	require.NoError(t, err)
	for i, c := range []*child{{p: &proc{pid: 1}}, {p: &proc{pid: 2}}} {
		for pts := range int64(5) {
			tr.fromChild(c, wire.Frame{Rung: 3, F: vframe(1, pts*3600, pts == 0)})
		}
		assert.Equal(t, i+1, h.count("unknown rung"), "child %d", c.p.pid)
	}
	out, _ := tr.mergers[0].counts()
	assert.Zero(t, out)
}

// tryChild never waits: a full queue, or no child, refuses the message.
func TestTryChild(t *testing.T) {
	tr, err := New(hub.New(hub.Options{}), Options{Renditions: []Rendition{copySrc}, Audio: Audio{Copy: true}})
	require.NoError(t, err)
	s := &run{t: tr}
	assert.False(t, s.tryChild(wire.Discontinuity{}))
	s.c = &child{in: make(chan wire.Message, 1), handed: true}
	assert.True(t, s.tryChild(wire.Discontinuity{}))
	assert.False(t, s.tryChild(wire.Discontinuity{}))
	assert.Equal(t, 1, tr.Stats().MaxQueued)
}

// syncBuffer is a strings.Builder safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// A child that writes stderr as it starts is logged with its pid, without a
// race against startChild (regression: exec's copy goroutine read the pid
// before it was set).
func TestSupervisorChildStderr(t *testing.T) {
	for range 3 {
		var buf syncBuffer
		opt := fakeOptions(t, transcodetest.EnvStderr+"=hello-from-child")
		opt.Logger = slog.New(slog.NewTextHandler(&buf, nil))
		opt.Renditions = []Rendition{encode240}
		s := start(t, opt)
		require.Eventually(t, func() bool { return strings.Contains(buf.String(), "line=hello-from-child") },
			10*time.Second, time.Millisecond)
		spawned := s.ev.of(EventSpawned)
		require.NotEmpty(t, spawned)
		assert.Contains(t, buf.String(), "pid="+strconv.Itoa(spawned[0].PID)+" line=hello-from-child")
	}
}

// A child that sends junk, or no Hello, while it keeps running is killed and
// counts as an exit: it is respawned, the renditions get one bump epoch, Run
// stays responsive and the copy rendition keeps every source video frame.
func TestSupervisorWedgedChild(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	for _, tc := range []struct {
		name, env, cause string
	}{
		{"junk", transcodetest.EnvJunk + "=1", "malformed"},
		{"no-hello", transcodetest.EnvNoHello + "=1", "none within"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := fakeOptions(t, tc.env,
				transcodetest.EnvCrashOnce+"="+filepath.Join(t.TempDir(), "once"))
			// The first child gets 1 s (4 s of media, over 256 frames) to
			// say Hello while the source queue holds only 64: feeding it
			// must not stall the copy rendition.
			opt.HelloTimeout = time.Second
			opt.Queue = 64
			opt.Renditions = []Rendition{encode240, copySrc}
			opt.Audio.Copy = true
			s := start(t, opt)
			out := s.feed(t, steps, 4)
			st := s.tr.Stats()
			assert.EqualValues(t, 2, st.Spawns)
			assert.EqualValues(t, 1, st.Exits)
			exits := s.ev.of(EventExited)
			require.Len(t, exits, 1)
			assert.ErrorContains(t, exits[0].Err, tc.cause)
			// The copy rendition gets the bump epoch on top of the source's.
			// The encoded one may have nothing in the first epoch: a junk
			// child dies before the first frame arrives.
			assertOneExtraEpoch(t, s, out[1:], steps)
			eps0, eps1 := epochs(out[0]), epochs(out[1])
			require.NotEmpty(t, eps0)
			assert.Equal(t, eps1[len(eps1)-1], eps0[len(eps0)-1])
			first := firstOfEpoch(out[0], eps0[len(eps0)-1])
			assert.True(t, first.Key && first.Kind() == media.KindVideo, "encoded rendition starts on a keyframe")
			for i, fs := range out {
				check(t, fs, opt.Renditions[i].Name)
			}
			var srcVideo, copyVideo int
			for _, f := range frames(steps) {
				if f.Kind() == media.KindVideo {
					srcVideo++
				}
			}
			for _, f := range out[1] {
				if f.Kind() == media.KindVideo {
					copyVideo++
				}
			}
			assert.Equal(t, srcVideo, copyVideo, "copy rendition video")
			assert.Zero(t, st.Losses, "the source subscription lost nothing")
		})
	}
}

// A child that sends junk after a valid Hello is killed at once, and the
// event names the decode error.
func TestSupervisorDecodeErrorAfterHello(t *testing.T) {
	opt := fakeOptions(t, transcodetest.EnvJunkAfterHello+"=1",
		transcodetest.EnvCrashOnce+"="+filepath.Join(t.TempDir(), "once"))
	opt.Renditions = []Rendition{encode240}
	s := start(t, opt)
	require.Eventually(t, func() bool { return len(s.ev.of(EventSpawned)) == 2 }, 10*time.Second, time.Millisecond)
	exits := s.ev.of(EventExited)
	require.Len(t, exits, 1)
	assert.ErrorIs(t, exits[0].Err, wire.ErrMalformed)
	assert.ErrorContains(t, exits[0].Err, "killed")
	// Killed at once, not after the grace given to a child that closes its
	// socket.
	took := s.ev.firstAt(EventExited).Sub(s.ev.firstAt(EventSpawned))
	assert.Less(t, took, stopGrace, "exit %v after the spawn", took)
}

// A child that closes its socket, then logs and exits on its own, is not
// killed: the event carries its exit status and its last stderr line is
// logged, and it is respawned.
func TestSupervisorClosingChildExits(t *testing.T) {
	var buf syncBuffer
	opt := fakeOptions(t, transcodetest.EnvCloseThenExit+"=1",
		transcodetest.EnvCrashOnce+"="+filepath.Join(t.TempDir(), "once"))
	opt.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	opt.Renditions = []Rendition{encode240}
	s := start(t, opt)
	require.Eventually(t, func() bool { return len(s.ev.of(EventSpawned)) == 2 }, 10*time.Second, time.Millisecond)
	exits := s.ev.of(EventExited)
	require.Len(t, exits, 1)
	assert.ErrorContains(t, exits[0].Err, "exit status 2")
	assert.NotContains(t, exits[0].Err.Error(), "killed")
	assert.Contains(t, buf.String(), "line=\"closing child exits\"")
	took := s.ev.firstAt(EventExited).Sub(s.ev.firstAt(EventSpawned))
	t.Logf("exit %v after the spawn", took)
}

// longGOP repeats a one-epoch clip n times on a continuous timeline, in
// epoch 1, and keeps the Key flag only on every gop-th video keyframe: a
// live source with a GOP of gop seconds (the fixture keys every 1 s).
func longGOP(steps []step, n, gop int) []step {
	var anchor *media.Anchor
	var clip []media.Frame
	for _, s := range steps {
		if s.anchor != nil {
			if anchor == nil {
				anchor = s.anchor
			}
			continue
		}
		clip = append(clip, s.f)
	}
	first, end := clip[0].DTS, clip[0].DTS
	for _, f := range clip {
		first = min(first, f.DTS)
		d := int64(3600)
		if f.Kind() == media.KindAudio {
			d = 1920
		}
		end = max(end, f.PTS+d)
	}
	span := end - first
	out := make([]step, 0, 1+len(clip)*n)
	out = append(out, step{anchor: anchor})
	keys := 0
	for k := range n {
		for _, f := range clip {
			f.PTS += int64(k) * span
			f.DTS += int64(k) * span
			f.Epoch = anchor.Epoch
			if f.Kind() == media.KindVideo && f.Key {
				f.Key = keys%gop == 0
				keys++
			}
			out = append(out, step{f: f, at: f.DTS})
		}
	}
	return out
}

// Joining a live hub whose GOP cache holds a long GOP (over the child's 256
// queue): the replayed GOP is held for the starting child, so there is no
// bump, nothing is dropped, and the encoded rendition starts at the cached
// keyframe.
func TestSupervisorJoinsLiveLongGOP(t *testing.T) {
	for _, gop := range []int{4, 8} {
		t.Run(strconv.Itoa(gop)+"s", func(t *testing.T) {
			steps := longGOP(fixtureSteps(t, "h264_aac.ts"), 3, gop)
			// Write up to the second keyframe: the cache holds one whole GOP.
			cut, keys := 0, 0
			for i, s := range steps {
				if s.anchor == nil && s.f.Kind() == media.KindVideo && s.f.Key {
					keys++
					if keys == 2 {
						cut = i
						break
					}
				}
			}
			require.NotZero(t, cut)
			src := hub.New(hub.Options{})
			<-play(t.Context(), src, steps[:cut], 0, false)
			var cachedKey int64
			for _, s := range steps[:cut] {
				if s.anchor == nil && s.f.Kind() == media.KindVideo && s.f.Key {
					cachedKey = s.f.PTS
				}
			}

			opt := fakeOptions(t)
			opt.Renditions = []Rendition{encode240, copySrc}
			ev := &events{}
			opt.OnEvent = ev.add
			tr, err := New(src, opt)
			require.NoError(t, err)
			cols := make([]*collector, 0, len(opt.Renditions))
			for _, h := range tr.Hubs() {
				cols = append(cols, collect(h))
			}
			errc := make(chan error, 1)
			go func() { errc <- tr.Run(t.Context()) }()
			waitSub(t, src, "transcode")
			// The rest arrives live (8× real time): an unpaced burst could
			// overflow the source subscription on its own.
			<-play(t.Context(), src, steps[cut:], 8, true)
			require.NoError(t, <-errc)

			enc := cols[0].wait(t)
			cols[1].wait(t)
			st := tr.Stats()
			assert.Equal(t, []uint32{1}, epochs(enc))
			assert.Zero(t, st.DroppedBeforeKey)
			assert.Zero(t, st.Losses)
			assert.Zero(t, tr.bumps.Load())
			var firstVideo media.Frame
			for _, f := range enc {
				if f.Kind() == media.KindVideo {
					firstVideo = f
					break
				}
			}
			assert.True(t, firstVideo.Key)
			assert.Equal(t, cachedKey+Delay, firstVideo.PTS, "starts at the cached GOP's keyframe")
			check(t, enc, "t240")
		})
	}
}

// The pre-Hello backlog holds maxBacklog messages; the next one is dropped
// and requests a bump that feeds the child again from K after its Hello.
func TestBacklogOverflow(t *testing.T) {
	tr, err := New(hub.New(hub.Options{}), Options{Renditions: []Rendition{copySrc}, Audio: Audio{Copy: true}})
	require.NoError(t, err)
	var rt routed
	s := &run{t: tr, ctx: t.Context()}
	s.r = newTestRouter(&rt, true, false, true)
	s.c = &child{in: make(chan wire.Message, childQueue), backlogCh: make(chan []wire.Message, 1)}
	for range maxBacklog {
		require.True(t, s.toChild(wire.Discontinuity{}))
	}
	assert.False(t, s.tryChild(wire.Discontinuity{}))
	assert.False(t, s.toChild(wire.Discontinuity{}))
	assert.True(t, s.r.em.isPending())
	assert.False(t, s.r.childFed)
	assert.EqualValues(t, 1, rt.dropped)
	s.c.handOver()
	assert.Len(t, <-s.c.backlogCh, maxBacklog)
	assert.True(t, s.toChild(wire.Discontinuity{}), "after the Hello, the queue")
	assert.Equal(t, 1, len(s.c.in))
}

// A child that stops reading its input (and reporting Stats) after its Hello
// is killed once Run has waited InputStallTimeout without the child
// reporting progress: the
// exit names the stall, the child is respawned with a bump, and Run never
// stalls long enough for the source subscription to lose frames.
func TestSupervisorStalledChild(t *testing.T) {
	steps := longGOP(fixtureSteps(t, "h264_aac.ts"), 4, 1)
	opt := fakeOptions(t, transcodetest.EnvStopReadingAfter+"=50",
		transcodetest.EnvStatsInterval+"=100ms",
		transcodetest.EnvCrashOnce+"="+filepath.Join(t.TempDir(), "once"))
	opt.InputStallTimeout = 300 * time.Millisecond
	opt.Renditions = []Rendition{encode240, copySrc}
	opt.Audio.Copy = true
	s := start(t, opt)
	out := s.feed(t, steps, 8)
	st := s.tr.Stats()
	exits := s.ev.of(EventExited)
	require.Len(t, exits, 1)
	assert.ErrorContains(t, exits[0].Err, "input stalled")
	assert.ErrorContains(t, exits[0].Err, "killed")
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, s.tr.bumps.Load())
	assert.Zero(t, st.Losses, "Run never stalled long enough to lose source frames")
	assertOneExtraEpoch(t, s, out, steps)
	for i, fs := range out {
		check(t, fs, opt.Renditions[i].Name)
	}
	var srcVideo, copyVideo int
	for _, f := range frames(steps) {
		if f.Kind() == media.KindVideo {
			srcVideo++
		}
	}
	for _, f := range out[1] {
		if f.Kind() == media.KindVideo {
			copyVideo++
		}
	}
	assert.Equal(t, srcVideo, copyVideo, "copy rendition video")
}

// A slow child that keeps consuming, and says so in its Stats, is never
// killed, even though the host's socket writes complete only in bursts
// seconds apart.
func TestSupervisorSlowChildNotKilled(t *testing.T) {
	loop := "h264_aac_loop.ts"
	steps := fixtureSteps(t, loop, loop)
	opt := fakeOptions(t, transcodetest.EnvDelay+"=20ms", transcodetest.EnvStatsInterval+"=100ms")
	opt.InputStallTimeout = 500 * time.Millisecond
	opt.Renditions = []Rendition{encode240, copySrc}
	s := start(t, opt)
	<-play(t.Context(), s.src, steps, 8, false)
	st := s.tr.Stats()
	t.Logf("losses %d, max queued %d", st.Losses, st.MaxQueued)
	assert.EqualValues(t, st.MaxQueued, childQueue, "the child's queue filled: sends blocked")
	assert.Zero(t, st.Exits, "a slow child that consumes is not killed")
	assert.Empty(t, s.ev.of(EventExited))
	assert.EqualValues(t, 1, st.Spawns)
	s.src.Close()
}

// The real child (cmd/ostranscoder) behind the supervisor: three encoded
// rungs and AAC on h264_aac.ts. Every rendition hub conforms, carries the
// source's video frame count in the source's epoch, and has an IDR at
// exactly every source keyframe PTS + D, so the rungs stay segment-aligned.
func TestSupervisorRealChild(t *testing.T) {
	bin := testpeer.OSTranscoder(t)
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := Options{
		Binary: bin,
		Renditions: []Rendition{
			{Name: "t240", Video: Video{Height: 240, BitrateKbps: 600}},
			{Name: "t180", Video: Video{Height: 180, BitrateKbps: 300}},
			{Name: "t120", Video: Video{Height: 120, BitrateKbps: 150}},
		},
		Audio:  Audio{BitrateKbps: 96},
		Queue:  2048,
		Logger: slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	s := start(t, opt)
	out := s.feed(t, steps, 0)

	var srcVideo []media.Frame
	for _, f := range shifted(frames(steps)) {
		if f.Kind() == media.KindVideo {
			srcVideo = append(srcVideo, f)
		}
	}
	srcKeys := keyPTS(srcVideo)
	require.NotEmpty(t, srcKeys)
	heights := []int{240, 180, 120}
	for i, fs := range out {
		name := opt.Renditions[i].Name
		check(t, fs, name)
		assert.Equal(t, srcEpochs(steps), epochs(fs), name)
		var video, audio []media.Frame
		for _, f := range fs {
			if f.Kind() == media.KindVideo {
				video = append(video, f)
			} else {
				audio = append(audio, f)
			}
		}
		require.Len(t, video, len(srcVideo), name)
		assert.Equal(t, srcKeys, keyPTS(video), "%s: keyframe PTS = source keyframe PTS + D", name)
		assert.Equal(t, heights[i], video[0].Config.Height, name)
		assert.NotEmpty(t, audio, name)
		t.Logf("%s: %d video (%d keys), %d audio, %dx%d", name, len(video), len(srcKeys), len(audio),
			video[0].Config.Width, video[0].Config.Height)
	}
	st := s.tr.Stats()
	assert.EqualValues(t, 1, st.Spawns)
	assert.Zero(t, st.Exits)
	assert.Zero(t, st.Losses)
	assert.Zero(t, st.DroppedBeforeKey)
	assert.Zero(t, st.MergeDropped)
	assert.Equal(t, ChildStats{}, st.Child)
}

// keyPTS returns the PTS of the video keyframes, sorted.
func keyPTS(fs []media.Frame) []int64 {
	var out []int64
	for _, f := range fs {
		if f.Key && f.Kind() == media.KindVideo {
			out = append(out, f.PTS)
		}
	}
	slices.Sort(out)
	return out
}
