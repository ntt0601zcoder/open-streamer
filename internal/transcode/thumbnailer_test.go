//go:build unix

package transcode

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// thumbOptions runs the test binary as the fake child of a Thumbnailer.
func thumbOptions(t *testing.T, intervalSec int, env ...string) ThumbnailerOptions {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return ThumbnailerOptions{
		Binary:     exe,
		Env:        append([]string{"OS_TRANSCODE_FAKE=1", "GORACE=atexit_sleep_ms=0"}, env...),
		Thumbnail:  picture.Thumbnail{IntervalSec: intervalSec},
		RespawnMin: 20 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
}

// thumbSession is one Thumbnailer run over a source hub.
type thumbSession struct {
	th     *Thumbnailer
	src    *hub.Hub
	errc   chan error
	hellos chan int // the pid of each child whose Hello Run has handled
	cancel context.CancelFunc
}

func startThumb(t *testing.T, opt ThumbnailerOptions) *thumbSession {
	t.Helper()
	s := &thumbSession{src: hub.New(hub.Options{}), errc: make(chan error, 1), hellos: make(chan int, 64)}
	th, err := NewThumbnailer(s.src, opt)
	require.NoError(t, err)
	th.onHello = func(pid int) { s.hellos <- pid }
	s.th = th
	ctx, cancel := context.WithCancel(t.Context())
	s.cancel = cancel
	go func() { s.errc <- th.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-s.errc:
			s.errc <- err
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})
	waitSub(t, s.src, "thumbnailer")
	return s
}

// hello waits until Run has handled a child's Hello, and returns its pid.
func (s *thumbSession) hello(t *testing.T) int {
	t.Helper()
	select {
	case pid := <-s.hellos:
		return pid
	case <-time.After(10 * time.Second):
		t.Fatal("no Hello")
		return 0
	}
}

// settled waits until Run has nothing left to do with what was written to
// the source: its counters stand still, with a shot for every key sent.
func (s *thumbSession) settled(t *testing.T) ThumbnailerStats {
	t.Helper()
	var last ThumbnailerStats
	still := 0
	require.Eventually(t, func() bool {
		st := s.th.Stats()
		if st != last || st.Shots != st.KeysSent {
			last, still = st, 0
			return false
		}
		still++
		return still >= 10
	}, 10*time.Second, 5*time.Millisecond)
	return last
}

func TestNewThumbnailer(t *testing.T) {
	src := hub.New(hub.Options{})
	_, err := NewThumbnailer(nil, ThumbnailerOptions{Binary: "x"})
	require.Error(t, err)
	_, err = NewThumbnailer(src, ThumbnailerOptions{})
	require.EqualError(t, err, "transcode: new thumbnailer: no transcoder binary")
	_, err = NewThumbnailer(src, ThumbnailerOptions{Binary: "x", Thumbnail: picture.Thumbnail{Height: 3}})
	require.EqualError(t, err, "transcode: new thumbnailer: thumbnail: height 3 must be even, 2..720")
	th, err := NewThumbnailer(src, ThumbnailerOptions{Binary: "x"})
	require.NoError(t, err)
	_, _, ok := th.Shot()
	assert.False(t, ok)
	assert.Zero(t, th.Stats())
}

// The child is sent the main video's due keyframes and nothing else,
// unshifted, and makes a shot of each.
func TestThumbnailerForwardsKeysOnly(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	log := filepath.Join(t.TempDir(), "frames.log")
	opt := thumbOptions(t, 2, transcodetest.EnvShots+"=1", transcodetest.EnvFrameLog+"="+log)
	var shots shotLog
	opt.OnShot = shots.add
	const ahead = 1000 * time.Hour
	opt.Now = func() time.Time { return time.Now().Add(ahead) }
	s := startThumb(t, opt)
	s.hello(t)
	before := time.Now().Add(ahead)
	// Unpaced: the one-slot channel may skip a due key, and the next one
	// is then offered.
	<-play(t.Context(), s.src, steps, 0, false)
	st := s.settled(t)
	after := time.Now().Add(ahead)

	require.GreaterOrEqual(t, st.KeysSent, uint64(1))
	require.LessOrEqual(t, st.KeysSent, uint64(3))
	assert.Equal(t, st.KeysSent, st.Shots)
	assert.EqualValues(t, 1, st.Spawns)
	assert.Zero(t, st.Exits)
	assert.Zero(t, st.Failed)

	video, audio := frameLogVideo(t, log)
	assert.Zero(t, audio, "no audio goes to the child")
	require.Len(t, video, int(st.KeysSent), "exactly the keys sent")
	keys := videoKeys(frames(steps))
	for i, f := range video {
		assert.Contains(t, keys, f.PTS, "a source keyframe, unshifted")
		assert.Equal(t, uint32(1), f.Epoch)
		if i > 0 {
			assert.GreaterOrEqual(t, f.PTS-video[i-1].PTS, int64(180000))
		}
	}
	all := shots.all()
	require.Len(t, all, len(video))
	for i, sh := range all {
		assert.Equal(t, video[i].PTS, sh.PTS)
	}
	last, at, ok := s.th.Shot()
	require.True(t, ok)
	assert.Equal(t, all[len(all)-1], last, "Shot is the last")
	assert.False(t, at.Before(before) || at.After(after), "at %v outside [%v, %v]", at, before, after)
}

// fixtureVideoConfig is a video config the wire protocol carries: the
// fixture's.
func fixtureVideoConfig(t *testing.T) *media.TrackConfig {
	t.Helper()
	for _, f := range frames(fixtureSteps(t, "h264_aac.ts")) {
		if f.Kind() == media.KindVideo {
			return f.Config
		}
	}
	t.Fatal("no video in the fixture")
	return nil
}

// The main video track is the lowest-numbered one seen so far; the keys of
// any other are not offered.
func TestThumbnailerMainVideoTrack(t *testing.T) {
	log := filepath.Join(t.TempDir(), "frames.log")
	opt := thumbOptions(t, 1, transcodetest.EnvShots+"=1", transcodetest.EnvFrameLog+"="+log)
	cfg := fixtureVideoConfig(t)
	s := startThumb(t, opt)
	s.hello(t)
	key := func(track int, sec int64) media.Frame {
		return media.Frame{Track: track, Config: cfg, PTS: sec * media.ClockRate, DTS: sec * media.ClockRate, Key: true, Epoch: 1, Data: [][]byte{{0x65, 1, 2}}}
	}
	s.src.SetAnchor(media.Anchor{Epoch: 1, Wall: time.Now()})
	s.src.Write(key(2, 0)) // the only track so far
	require.Eventually(t, func() bool { return s.th.Stats().Shots == 1 }, 10*time.Second, time.Millisecond)
	s.src.Write(key(1, 10)) // a lower one: it is the main track now
	require.Eventually(t, func() bool { return s.th.Stats().Shots == 2 }, 10*time.Second, time.Millisecond)
	s.src.Write(key(2, 20)) // no longer the main track
	s.src.Write(aframe(1, 25*media.ClockRate))
	s.src.Write(key(1, 30))
	require.Eventually(t, func() bool { return s.th.Stats().Shots == 3 }, 10*time.Second, time.Millisecond)
	video, _ := frameLogVideo(t, log)
	require.Len(t, video, 3)
	assert.Equal(t, []int{2, 1, 1}, []int{video[0].Track, video[1].Track, video[2].Track})
	assert.Equal(t, ThumbnailerStats{Spawns: 1, KeysSent: 3, Shots: 3}, s.th.Stats())
}

// A child that exits is respawned, and the schedule starts over: the new
// child is sent the next key although the interval has not passed.
func TestThumbnailerRespawns(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := thumbOptions(t, 3600, append(crashOnce(t, 1), transcodetest.EnvShots+"=1")...)
	s := startThumb(t, opt)
	first := s.hello(t)
	played := play(t.Context(), s.src, steps, 2, false)
	second := s.hello(t)
	assert.NotEqual(t, first, second)
	require.Eventually(t, func() bool { return s.th.Stats().Shots == 2 }, 10*time.Second, time.Millisecond)
	<-played
	st := s.th.Stats()
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, st.Exits)
	assert.EqualValues(t, 2, st.KeysSent, "one key per child: the interval is an hour")
	assert.EqualValues(t, 2, st.Shots)
	select {
	case err := <-s.errc:
		t.Fatalf("Run returned: %v", err)
	default:
	}
}

// A shot the child could not make is retried at the next keyframe after the
// child reports it, once: a key that never decodes costs one more attempt,
// not one per keyframe.
func TestThumbnailerRetriesFailedShot(t *testing.T) {
	steps := fixtureSteps(t, "h264_aac.ts")
	opt := thumbOptions(t, 3600, transcodetest.EnvShotFail+"=1", transcodetest.EnvStatsInterval+"=5ms")
	s := startThumb(t, opt)
	s.hello(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	<-play(ctx, s.src, steps, 1, false) // six keyframes, one a second
	require.Eventually(t, func() bool { return s.th.Stats().Failed == 2 }, 10*time.Second, time.Millisecond)
	st := s.th.Stats()
	assert.EqualValues(t, 2, st.KeysSent, "the first key and one retry")
	assert.Zero(t, st.Shots)
	assert.Zero(t, st.KeysSkipped)
	assert.EqualValues(t, 1, st.Spawns)
	_, _, ok := s.th.Shot()
	assert.False(t, ok)
}

// A child that does not read: once its socket and the one-slot channel are
// full, due keys are skipped. Run does not wait for it, so the hub's writes
// are all taken.
func TestThumbnailerSkipsWhenChildBusy(t *testing.T) {
	opt := thumbOptions(t, 1, transcodetest.EnvStopReadingAfter+"=1", transcodetest.EnvStatsInterval+"=10ms")
	cfg := fixtureVideoConfig(t)
	s := startThumb(t, opt)
	s.hello(t)
	s.src.SetAnchor(media.Anchor{Epoch: 1, Wall: time.Now()})
	big := [][]byte{make([]byte, 1<<20)} // more than a socket buffer holds
	const n = 20
	start := time.Now()
	for i := range int64(n) {
		s.src.Write(media.Frame{Track: 0, Config: cfg, PTS: i * 2 * media.ClockRate, DTS: i * 2 * media.ClockRate, Key: true, Epoch: 1, Data: big})
	}
	assert.Less(t, time.Since(start), time.Second, "the hub's writes do not block")
	require.Eventually(t, func() bool {
		st := s.th.Stats()
		return st.KeysSent+st.KeysSkipped == n
	}, 5*time.Second, time.Millisecond, "Run took every frame")
	st := s.th.Stats()
	assert.GreaterOrEqual(t, st.KeysSent, uint64(1))
	assert.LessOrEqual(t, st.KeysSent, uint64(3), "one read, one stuck in the socket, one in the slot")
	assert.GreaterOrEqual(t, st.KeysSkipped, uint64(n-3))
	assert.EqualValues(t, 1, st.Spawns, "the child was not replaced: it is busy, not gone")
	assert.Zero(t, st.Exits)
	for _, sub := range s.src.Stats() {
		if sub.Name == "thumbnailer" {
			assert.Zero(t, sub.Dropped, "the subscription lost nothing")
		}
	}
	// Run still ends, by killing the child.
	s.cancel()
	select {
	case err := <-s.errc:
		require.ErrorIs(t, err, context.Canceled)
		s.errc <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A key the wire protocol cannot carry is dropped and the child lives; a
// source whose every key is too large logs that once per child, not at
// every interval.
func TestThumbnailerOversizeKeyLoggedOnce(t *testing.T) {
	opt := thumbOptions(t, 1)
	var logs syncBuffer
	opt.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := fixtureVideoConfig(t)
	s := startThumb(t, opt)
	s.hello(t)
	s.src.SetAnchor(media.Anchor{Epoch: 1, Wall: time.Now()})
	big := [][]byte{make([]byte, wire.MaxMessage+1)}
	const n = 5
	for i := range int64(n) {
		s.src.Write(media.Frame{Track: 0, Config: cfg, PTS: i * media.ClockRate, DTS: i * media.ClockRate, Key: true, Epoch: 1, Data: big})
		require.Eventually(t, func() bool {
			st := s.th.Stats()
			return st.KeysSent+st.KeysSkipped == uint64(i+1)
		}, 5*time.Second, time.Millisecond)
		time.Sleep(200 * time.Millisecond) // the writer takes the key from the slot
	}
	st := s.th.Stats()
	assert.GreaterOrEqual(t, st.KeysSent, uint64(2), "%+v", st)
	assert.Zero(t, st.Shots)
	assert.EqualValues(t, 1, st.Spawns, "the child lives")
	assert.Zero(t, st.Exits)
	assert.Equal(t, 1, strings.Count(logs.String(), "key dropped: the wire protocol cannot carry it"), logs.String())
}

// stubTime is a clock and a timer source for the silence timeout: delays of
// a second or more wait for fire, shorter ones (the respawn backoff) are
// real.
type stubTime struct {
	mu     sync.Mutex
	now    time.Time
	timers chan stubTimer
}

type stubTimer struct {
	d time.Duration
	c chan time.Time
}

func (f *stubTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *stubTime) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *stubTime) After(d time.Duration) <-chan time.Time {
	if d < time.Second {
		return time.After(d)
	}
	c := make(chan time.Time, 1)
	f.timers <- stubTimer{d: d, c: c}
	return c
}

// timer waits for the next long timer Run asked for.
func (f *stubTime) timer(t *testing.T) stubTimer {
	t.Helper()
	select {
	case tm := <-f.timers:
		return tm
	case <-time.After(10 * time.Second):
		t.Fatal("no timer")
		return stubTimer{}
	}
}

// A child that says Hello and then nothing is killed after SilenceTimeout,
// which counts as an exit, and respawned.
func TestThumbnailerKillsSilentChild(t *testing.T) {
	ft := &stubTime{now: time.Unix(1000, 0), timers: make(chan stubTimer, 16)}
	opt := thumbOptions(t, 2, transcodetest.EnvStatsInterval+"=1h")
	opt.SilenceTimeout = 7 * time.Second
	opt.Now, opt.After = ft.Now, ft.After
	s := startThumb(t, opt)
	first := s.hello(t)
	tm := ft.timer(t)
	assert.Equal(t, 7*time.Second, tm.d)
	// Fired early, 3 s in: the child is not silent for long enough yet.
	ft.advance(3 * time.Second)
	tm.c <- ft.Now()
	tm = ft.timer(t)
	assert.Equal(t, 4*time.Second, tm.d, "the rest of the timeout")
	assert.Zero(t, s.th.Stats().Exits)
	require.NoError(t, syscall.Kill(first, 0), "the child still runs")
	ft.advance(4 * time.Second)
	tm.c <- ft.Now()
	second := s.hello(t)
	assert.NotEqual(t, first, second)
	st := s.th.Stats()
	assert.EqualValues(t, 1, st.Exits)
	assert.EqualValues(t, 2, st.Spawns)
	assert.ErrorIs(t, syscall.Kill(first, 0), syscall.ESRCH, "the silent child is gone")
	assert.Equal(t, 7*time.Second, ft.timer(t).d, "the new child is watched too")
}

// A child that keeps sending Stats is not silent.
func TestThumbnailerTalkingChildNotKilled(t *testing.T) {
	opt := thumbOptions(t, 2, transcodetest.EnvStatsInterval+"=10ms")
	opt.SilenceTimeout = 400 * time.Millisecond
	s := startThumb(t, opt)
	s.hello(t)
	time.Sleep(1200 * time.Millisecond)
	assert.Equal(t, ThumbnailerStats{Spawns: 1}, s.th.Stats())
}

// When the hub closes Run returns nil, and when the context ends
// context.Canceled, as Transcoder.Run does; either way it has stopped its
// child.
func TestThumbnailerEndsWithHub(t *testing.T) {
	base := runtime.NumGoroutine()
	for _, how := range []string{"hub", "context"} {
		t.Run(how, func(t *testing.T) {
			s := startThumb(t, thumbOptions(t, 2))
			pid := s.hello(t)
			if how == "hub" {
				s.src.Close()
			} else {
				s.cancel()
			}
			select {
			case err := <-s.errc:
				if how == "hub" {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
				s.errc <- nil
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return")
			}
			assert.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "child %d not reaped", pid)
			assert.Error(t, s.th.Run(t.Context()), "Run may be called once")
		})
	}
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(5 * time.Second); n > base && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, n, base, "goroutines back to the baseline")
}

// Another wire version is fatal, also when the child exits right after its
// Hello: nothing about the timing is asserted.
func TestThumbnailerVersionMismatch(t *testing.T) {
	for name, env := range map[string][]string{
		"hangs": {transcodetest.EnvVersion + "=1"},
		"exits": {transcodetest.EnvVersion + "=1", transcodetest.EnvExitAfterHello + "=1"},
	} {
		t.Run(name, func(t *testing.T) {
			th, err := NewThumbnailer(hub.New(hub.Options{}), thumbOptions(t, 2, env...))
			require.NoError(t, err)
			errc := make(chan error, 1)
			go func() { errc <- th.Run(t.Context()) }()
			select {
			case err := <-errc:
				require.ErrorIs(t, err, errVersion)
				assert.ErrorContains(t, err, "1, want 3")
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not fail")
			}
			assert.Equal(t, ThumbnailerStats{Spawns: 1}, th.Stats())
		})
	}
}

// A binary that does not exist is retried with the backoff, as the
// Transcoder's is (TestSupervisorMissingBinary): every spawn counts as an
// exit, each failure is logged with startProc's error, and Run goes on
// until ctx ends.
func TestThumbnailerMissingBinary(t *testing.T) {
	ra := &recordAfter{n: 3, full: make(chan struct{})}
	opt := thumbOptions(t, 2)
	opt.Binary = "/nonexistent"
	opt.RespawnMin = 0 // default 250 ms
	opt.After = ra.After
	var logs syncBuffer
	opt.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	th, err := NewThumbnailer(hub.New(hub.Options{}), opt)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- th.Run(ctx) }()
	select {
	case <-ra.full:
	case err := <-errc:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no respawns")
	}
	ra.mu.Lock()
	// The 4th spawn's After returns nil: Run waits for ctx there.
	assert.Equal(t, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}, ra.delays)
	ra.mu.Unlock()
	assert.Equal(t, ThumbnailerStats{Spawns: 4, Exits: 4}, th.Stats())
	assert.Equal(t, 4, strings.Count(logs.String(), "transcode: thumbnailer: spawn failed"), logs.String())
	assert.Contains(t, logs.String(), "/nonexistent")
	cancel()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A child that never says Hello is killed after HelloTimeout and respawned.
func TestThumbnailerNoHello(t *testing.T) {
	opt := thumbOptions(t, 2, transcodetest.EnvNoHello+"=1", transcodetest.EnvCrashOnce+"="+filepath.Join(t.TempDir(), "once"))
	opt.HelloTimeout = 100 * time.Millisecond
	s := startThumb(t, opt)
	s.hello(t) // the second child's
	st := s.th.Stats()
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, st.Exits)
}

// OnEvent reports each child's start and end from the Run goroutine, as the
// Transcoder's does: EventSpawned with the child's PID, and exactly one
// EventExited per child, whether it cannot be started, is killed, or is
// killed by Run for silence. Stats and the retries are as without OnEvent
// (the tests above, which leave it nil).
func TestThumbnailerEvents(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		// The first attempt (made before any wait) and one fired retry give
		// two events; the second wait is pending and is not fired.
		ra := &recordAfter{n: 1, full: make(chan struct{})}
		opt := thumbOptions(t, 2)
		opt.Binary = "/nonexistent"
		opt.After = ra.After
		ev := &events{}
		opt.OnEvent = ev.add
		th, err := NewThumbnailer(hub.New(hub.Options{}), opt)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { errc <- th.Run(ctx) }()
		select {
		case <-ra.full:
		case err := <-errc:
			t.Fatalf("Run returned: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("no retry")
		}
		ra.mu.Lock()
		assert.Len(t, ra.delays, 2, "After called twice")
		ra.mu.Unlock()
		exits := ev.of(EventExited)
		require.Len(t, exits, 2)
		for _, e := range exits {
			assert.ErrorContains(t, e.Err, "/nonexistent")
			assert.Zero(t, e.PID)
		}
		assert.Empty(t, ev.of(EventSpawned))
		assert.Equal(t, ThumbnailerStats{Spawns: 2, Exits: 2}, th.Stats())
		cancel()
		select {
		case err := <-errc:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return")
		}
		ev.mu.Lock()
		assert.Len(t, ev.evs, 2, "nothing more at the end of Run")
		ev.mu.Unlock()
	})

	t.Run("killed", func(t *testing.T) {
		opt := thumbOptions(t, 2)
		ev := &events{}
		opt.OnEvent = ev.add
		s := startThumb(t, opt)
		first := s.hello(t)
		require.NoError(t, syscall.Kill(first, syscall.SIGKILL))
		second := s.hello(t)
		ev.mu.Lock()
		evs := slices.Clone(ev.evs)
		ev.mu.Unlock()
		require.Len(t, evs, 3, "%+v", evs)
		assert.Equal(t, Event{Kind: EventSpawned, PID: first}, evs[0])
		assert.Equal(t, EventExited, evs[1].Kind)
		assert.Equal(t, first, evs[1].PID)
		assert.Error(t, evs[1].Err)
		assert.Equal(t, Event{Kind: EventSpawned, PID: second}, evs[2])
		assert.Equal(t, ThumbnailerStats{Spawns: 2, Exits: 1}, s.th.Stats())
	})

	t.Run("silent", func(t *testing.T) {
		ft := &stubTime{now: time.Unix(1000, 0), timers: make(chan stubTimer, 16)}
		opt := thumbOptions(t, 2, transcodetest.EnvStatsInterval+"=1h")
		opt.SilenceTimeout = 7 * time.Second
		opt.Now, opt.After = ft.Now, ft.After
		ev := &events{}
		opt.OnEvent = ev.add
		s := startThumb(t, opt)
		first := s.hello(t)
		tm := ft.timer(t)
		ft.advance(7 * time.Second)
		tm.c <- ft.Now()
		second := s.hello(t)
		ev.mu.Lock()
		evs := slices.Clone(ev.evs)
		ev.mu.Unlock()
		require.Len(t, evs, 3, "one EventExited for the silent child: %+v", evs)
		assert.Equal(t, Event{Kind: EventSpawned, PID: first}, evs[0])
		assert.Equal(t, EventExited, evs[1].Kind)
		assert.Equal(t, first, evs[1].PID)
		assert.ErrorContains(t, evs[1].Err, "silent for 7s")
		assert.Equal(t, Event{Kind: EventSpawned, PID: second}, evs[2])
		assert.Equal(t, ThumbnailerStats{Spawns: 2, Exits: 1}, s.th.Stats())
	})
}
