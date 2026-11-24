//go:build unix

package transcode

import (
	"bytes"
	"cmp"
	"context"
	"errors"
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
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
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
}

func (e *events) add(ev Event) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	e.mu.Unlock()
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
	s := &session{src: hub.New(hub.Options{}), ev: &events{}, errc: make(chan error, 1)}
	opt.OnEvent = s.ev.add
	tr, err := New(s.src, opt)
	require.NoError(t, err)
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
	for i, fs := range out {
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

func TestSupervisorMixedRespawn(t *testing.T) {
	plain := fixtureSteps(t, "h264_aac.ts")
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"plain", plain},
		{"audio-leads-300ms", audioLead(plain, 27000)},
		{"hevc-open-gop", fixtureSteps(t, "h265_aac51.ts")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := fakeOptions(t, crashOnce(t, 50)...)
			opt.Renditions = []Rendition{copySrc}
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
	w := &logWriter{log: slog.New(slog.NewTextHandler(&buf, nil)), pid: 7}
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

// tryChild never waits: a full queue, or no child, refuses the message.
func TestTryChild(t *testing.T) {
	tr, err := New(hub.New(hub.Options{}), Options{Renditions: []Rendition{copySrc}, Audio: Audio{Copy: true}})
	require.NoError(t, err)
	s := &run{t: tr}
	assert.False(t, s.tryChild(wire.Discontinuity{}))
	s.c = &child{in: make(chan wire.Message, 1)}
	assert.True(t, s.tryChild(wire.Discontinuity{}))
	assert.False(t, s.tryChild(wire.Discontinuity{}))
	assert.Equal(t, 1, tr.Stats().MaxQueued)
}
