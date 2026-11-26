package stream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

// errBoom fails a generation's runner.
var errBoom = errors.New("boom")

// failGen makes the generation s serves fail, through a runner named name.
func failGen(t testing.TB, s *Supervisor, name string) {
	t.Helper()
	g := currentGen(s)
	require.NotNil(t, g, "no generation to fail")
	g.run(name, func(context.Context) error { return errBoom })
}

// A stream without a ladder serves its source hub under its name, on every
// output, from generation 1, and lets go of everything when it stops.
func TestSupervisorServes(t *testing.T) {
	r := newRig(t)
	r.sc.set(func(s *script) { s.openErr = errors.New("not yet") })
	s := r.start(camSpec())
	waitGen(t, s, 1)
	assert.Equal(t, "starting", s.Status().State, "no input has been active")
	v := s.View()
	assert.Equal(t, []string{"cam"}, slices.Collect(maps.Keys(v.Single)))
	assert.Equal(t, []string{"cam"}, slices.Collect(maps.Keys(v.Packs)))
	assert.Same(t, v.Source, v.Single["cam"])
	assert.Equal(t, camSpec().Outputs, v.Outputs)

	r.sc.set(func(s *script) { s.openErr = nil })
	waitState(t, s, "running")
	tp := tap(t, v.Source, "tap")
	tp.more(t, 20)
	st := s.Status()
	assert.Equal(t, "cam", st.Name)
	assert.Equal(t, uint64(1), st.Generation)
	assert.Equal(t, []string{"cam"}, st.Served)
	assert.Equal(t, 0, st.ActiveInput)
	assert.False(t, st.Since.IsZero())
	require.Len(t, st.Tracks, 2)
	assert.Equal(t, Track{Track: 0, Kind: "video", Codec: "h264"}, st.Tracks[0])
	assert.Equal(t, Track{Track: 1, Kind: "audio", Codec: "aac"}, st.Tracks[1])
	assert.Nil(t, st.Transcoder)
	_, _, ok := s.Shot()
	assert.False(t, ok, "no thumbnail without a thumbnail spec")

	r.stop()
	select {
	case <-v.Source.Done():
	default:
		assert.Fail(t, "the source hub is still open after Run returned")
	}
	end := s.View()
	require.NotNil(t, end)
	assert.Zero(t, end.Gen)
	assert.Empty(t, end.Single)
	assert.Empty(t, end.Packs)
	assert.Nil(t, end.Source)
	assert.Equal(t, "stopped", s.Status().State)
	assert.Equal(t, -1, s.Status().ActiveInput)
}

// A ladder serves rendition 0 under the stream's name, each rendition under
// <name>_<label>, and the presentation as the ABR packager of the name.
func TestSupervisorLadderNames(t *testing.T) {
	r := newRig(t)
	fakeChild(t, &r.env)
	s := r.start(ladderSpec("720", "360"))
	waitRunning(t, s)
	v := s.View()
	names := []string{"cam", "cam_360", "cam_720"}
	assert.Equal(t, names, slices.Sorted(maps.Keys(v.Single)))
	assert.Equal(t, names, slices.Sorted(maps.Keys(v.Packs)))
	assert.Same(t, v.Single["cam"], v.Single["cam_720"])
	assert.NotSame(t, v.Single["cam"], v.Single["cam_360"])
	assert.NotSame(t, v.Single["cam"], v.Source)
	assert.NotSame(t, v.Single["cam_360"], v.Source)
	assert.Equal(t, []string{"cam", "cam_720", "cam_360"}, s.Status().Served)

	// Packs["cam"] reads the presentation hub: every rung's video on its
	// own track.
	pres := currentGen(s).tr.Presentation()
	require.Eventually(t, func() bool { return slices.Contains(subscribers(pres), "packager cam") }, waitFor, time.Millisecond)
	tp := tap(t, pres, "tap")
	want := transcode.PresentationTrack(1)
	require.Eventually(t, func() bool {
		f := tp.last.Load()
		return f != nil && f.Track == want
	}, waitFor, time.Millisecond, "no frame of rung 1's presentation track %d", want)
	for _, name := range []string{"cam_720", "cam_360"} {
		assert.Contains(t, subscribers(v.Single[name]), "packager "+name)
	}
	require.NotNil(t, s.Status().Transcoder)
	assert.NotZero(t, s.Status().Transcoder.PID)
}

// outputEdit returns sp with one session output switched.
func outputEdit(sp spec.Stream, out Output, on bool) spec.Stream {
	switch out {
	case OutHTTPTS:
		sp.Outputs.HTTPTS = on
	case OutRTMP:
		sp.Outputs.RTMP = on
	case OutRTSP:
		sp.Outputs.RTSP = on
	case OutSRT:
		sp.Outputs.SRT = on
	case OutHLS:
		sp.Outputs.HLS = on
	case OutDASH:
		sp.Outputs.DASH = on
	}
	return sp
}

// pushStats returns the status of the push to url.
func pushStats(t testing.TB, s *Supervisor, url string) Push {
	t.Helper()
	id := pushID(url)
	for _, p := range s.Status().Pushes {
		if p.ID == id {
			return p
		}
	}
	require.FailNow(t, "no push "+id)
	return Push{}
}

// An output edit is applied in place: only the sessions of the output turned
// off end, and nothing else is touched.
func TestSupervisorOutputEditInPlace(t *testing.T) {
	r := newRig(t)
	// At the very moment a view is published, a player of each output it
	// announces subscribes: none may be handed a closed subscription. This
	// runs on the supervisor's goroutine, inside reattach, so it does not
	// depend on winning a race.
	var announced, refusedAtPublish atomic.Uint64
	r.published = func(v *View) {
		for out, name := range map[Output]string{
			OutRTSP: "rtsp probe", OutHTTPTS: "httpts probe", OutRTMP: "rtmp probe", OutSRT: "srt probe",
		} {
			if !slices.Contains(viewOutputs(v), out) {
				continue
			}
			announced.Add(1)
			sub := v.Single["cam"].SubscribeLive(name, 8)
			select {
			case _, open := <-sub.C():
				if !open {
					refusedAtPublish.Add(1)
				}
			default:
			}
			sub.Close()
		}
	}
	sinkA := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	sinkB := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	sp := camSpec()
	urlA, urlB := sinkA.URL("live/"+vKey+"a"), sinkB.URL("live/"+vKey+"b")
	sp.Outputs.Push = []spec.Push{{URL: urlA}, {URL: urlB}}
	s := r.start(sp)
	waitRunning(t, s)
	v := s.View()
	h := v.Single["cam"]
	require.Eventually(t, func() bool {
		return pushStats(t, s, urlA).Connects == 1 && pushStats(t, s, urlB).Connects == 1
	}, waitFor, time.Millisecond)

	sessions := map[Output]string{
		OutRTSP: "rtsp cam", OutHTTPTS: "httpts 1.1.1.1:2", OutRTMP: "rtmp cam", OutSRT: "srt 1.1.1.1:1",
	}
	taps := map[Output]*tapper{}
	for out, name := range sessions {
		taps[out] = tap(t, h, name)
	}
	for _, out := range []Output{OutRTSP, OutHTTPTS, OutRTMP, OutSRT} {
		require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(outputEdit(sp, out, false))), "%s off", out)
		assert.False(t, slices.Contains(viewOutputs(s.View()), out))
		require.Eventually(t, taps[out].closed, waitFor, time.Millisecond, "the %s session did not end", out)
		for other, tp := range taps {
			if other == out {
				continue
			}
			tp.more(t, 50)
			assert.False(t, tp.closed(), "the %s session ended when %s was turned off", other, out)
			assert.Zero(t, tp.lost.Load(), "the %s session lost frames when %s was turned off", other, out)
		}
		again := h.Subscribe(sessions[out], 8)
		_, open := <-again.C()
		assert.False(t, open, "a new %s session was accepted while the output is off", out)

		require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(sp)), "%s on", out)
		taps[out] = tap(t, h, sessions[out])
		taps[out].more(t, 10)
		assert.False(t, taps[out].closed())

		now := s.View()
		assert.Equal(t, uint64(1), now.Gen)
		assert.Same(t, h, now.Single["cam"])
		assert.Same(t, v.Source, now.Source)
		assert.Same(t, v.Packs["cam"], now.Packs["cam"])
		assert.Equal(t, uint64(1), pushStats(t, s, urlA).Connects)
		assert.Equal(t, uint64(1), pushStats(t, s, urlB).Connects)
	}
	assert.Equal(t, uint64(0), s.Status().Rebuilds)
	assert.Equal(t, uint64(0), s.Status().Restarts)

	// The re-enable order: an output is subscribable before the view
	// announces it.
	const rounds = 200
	type seen struct{ on, closedSub bool }
	acks := make(chan seen)
	stop := make(chan struct{})
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		on := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			cur := s.View()
			if cur.Outputs.RTSP == on {
				continue
			}
			on = cur.Outputs.RTSP
			got := seen{on: on}
			if on {
				// The moment the view announces RTSP, a player subscribes.
				sub := cur.Single["cam"].Subscribe("rtsp cam", 8)
				select {
				case _, open := <-sub.C():
					got.closedSub = !open
				default:
				}
				sub.Close()
			}
			select {
			case acks <- got:
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-pollerDone
	}()
	ack := func() seen {
		t.Helper()
		select {
		case got := <-acks:
			return got
		case <-time.After(waitFor):
			require.FailNow(t, "the poller saw no change of the view")
			return seen{}
		}
	}
	off := outputEdit(sp, OutRTSP, false)
	for i := range rounds {
		require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(off)))
		require.False(t, ack().on, "round %d", i)
		onAns := s.Submit(sp)
		got := ack()
		require.True(t, got.on, "round %d", i)
		require.False(t, got.closedSub, "round %d: the view announced RTSP while its sessions were still refused", i)
		require.Equal(t, spec.ChangeOutputs, answer(t, onAns))
	}
	assert.Greater(t, announced.Load(), uint64(4*rounds))
	assert.Zero(t, refusedAtPublish.Load(), "a view announced an output whose sessions were still refused")
}

// viewOutputs lists the session outputs a view has on.
func viewOutputs(v *View) []Output {
	var out []Output
	for o, on := range map[Output]bool{
		OutHLS: v.Outputs.HLS, OutDASH: v.Outputs.DASH, OutHTTPTS: v.Outputs.HTTPTS,
		OutRTMP: v.Outputs.RTMP, OutRTSP: v.Outputs.RTSP, OutSRT: v.Outputs.SRT,
	} {
		if on {
			out = append(out, o)
		}
	}
	return out
}

// Any edit outside the outputs rebuilds the stream at once, without a
// backoff, and is answered when the new generation is served.
func TestSupervisorRebuildOnOtherEdits(t *testing.T) {
	r := newRig(t)
	sp := camSpec()
	s := r.start(sp)
	waitRunning(t, s)
	old := s.View()

	next := sp
	next.Inputs = append(slices.Clone(sp.Inputs), spec.Input{URL: "http://backup.test/cam.ts"})
	require.Equal(t, spec.ChangeRebuild, answer(t, s.Submit(next)))
	v := s.View()
	assert.Equal(t, uint64(2), v.Gen, "the answer came before the new generation was served")
	assert.NotSame(t, old.Source, v.Source)
	select {
	case <-old.Source.Done():
	default:
		assert.Fail(t, "the old source hub is still open")
	}
	st := s.Status()
	assert.Equal(t, uint64(1), st.Rebuilds)
	assert.Equal(t, uint64(0), st.Restarts)
	assert.Len(t, st.Inputs, 2)
	assert.False(t, r.after.called(), "an edit's rebuild waited a backoff")
	waitState(t, s, "running")

	// The same document again changes nothing.
	require.Equal(t, spec.ChangeNone, answer(t, s.Submit(next)))
	assert.Equal(t, uint64(2), s.View().Gen)
}

// A generation that fails is rebuilt after a backoff of 1 s doubling to
// 30 s, which a generation that lived 60 s resets.
func TestSupervisorRestartBackoff(t *testing.T) {
	r := newRig(t)
	clock := &fakeNow{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	r.env.Now = clock.Now
	fakeChild(t, &r.env, transcodetest.EnvVersion+"=99")
	s := r.start(ladderSpec("720"))
	var restarts uint64
	// step checks the next backoff, then lets the next generation start
	// with the clock jumping by jump at every reading.
	step := func(want, jump time.Duration) {
		t.Helper()
		call := r.after.next(t)
		restarts++
		assert.Equal(t, want, call.d, "backoff %d", restarts)
		st := s.Status()
		assert.Equal(t, "backoff", st.State)
		assert.Equal(t, restarts, st.Restarts)
		assert.Regexp(t, `^transcoder: `, st.LastError)
		assert.NotNil(t, st.LastErrorAt)
		assert.Zero(t, s.View().Gen)
		assert.Zero(t, metric(t, s, "openstreamer_stream_up", "stream", "cam"))
		clock.setStep(jump)
		call.c <- time.Time{}
	}
	for _, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		step(want*time.Second, 0)
	}
	// A generation that lived 60 s resets the backoff: the clock jumps 61 s
	// at every reading, so between a generation's start and its end.
	step(30*time.Second, 61*time.Second)
	step(time.Second, 61*time.Second)
	step(time.Second, 0)
	step(2*time.Second, 0)
	step(4*time.Second, 0)
	assert.Equal(t, float64(restarts), metric(t, s, "openstreamer_stream_restarts_total", "stream", "cam", "reason", "failure"))
	assert.Zero(t, s.Status().Rebuilds)
}

// An input that cannot be opened leaves the stream in its generation,
// starting, with the input's failure as its last error, scrubbed.
func TestSupervisorDeadInput(t *testing.T) {
	r := newRig(t)
	url := "rtsp://u:" + vPass + "@h/x"
	r.sc.set(func(s *script) { s.openErr = fmt.Errorf("dial %s: connection refused", url) })
	sp := camSpec()
	sp.Inputs = []spec.Input{{URL: url}}
	s := r.start(sp)
	require.Eventually(t, func() bool { return s.Status().LastError != "" }, waitFor, time.Millisecond)
	st := s.Status()
	assert.Equal(t, "starting", st.State)
	assert.Equal(t, uint64(1), st.Generation)
	assert.Equal(t, -1, st.ActiveInput)
	assert.Regexp(t, `^input 0: .*connection refused`, st.LastError)
	assert.NotContains(t, st.LastError, vPass)
	require.NotNil(t, st.LastErrorAt)
	assert.WithinDuration(t, time.Now(), *st.LastErrorAt, time.Minute)
	assert.Zero(t, metric(t, s, "openstreamer_stream_up", "stream", "cam"))
	assert.Equal(t, float64(-1), metric(t, s, "openstreamer_stream_active_input", "stream", "cam"))

	r.sc.set(func(s *script) { s.openErr = nil })
	waitState(t, s, "running")
	assert.Equal(t, float64(1), metric(t, s, "openstreamer_stream_up", "stream", "cam"))
	now := s.Status()
	assert.Equal(t, st.LastError, now.LastError, "the last error stays after recovery")
	assert.Equal(t, st.LastErrorAt, now.LastErrorAt)
	assert.Equal(t, uint64(1), now.Generation)
	assert.Zero(t, now.Restarts)
	assert.False(t, r.after.called())
}

// Losing every input does not rebuild the stream: it is no_input, and down,
// until an input is back, whatever the hub says.
func TestSupervisorInputLoss(t *testing.T) {
	r := newRig(t)
	s := r.start(camSpec())
	waitState(t, s, "running")
	assert.Equal(t, float64(1), metric(t, s, "openstreamer_stream_up", "stream", "cam"))
	gone := errors.New("connection reset")
	r.sc.set(func(s *script) { s.readErr, s.openErr = gone, gone })
	waitState(t, s, "no_input")
	assert.Zero(t, metric(t, s, "openstreamer_stream_up", "stream", "cam"))
	assert.True(t, s.View().Source.Live(), "the hub stays live once it had a keyframe")
	assert.Equal(t, uint64(1), s.View().Gen)
	assert.Regexp(t, `^input 0: .*connection reset`, s.Status().LastError)

	r.sc.set(func(s *script) { s.readErr, s.openErr = nil, nil })
	waitRunning(t, s)
	assert.Equal(t, float64(1), metric(t, s, "openstreamer_stream_up", "stream", "cam"))
	assert.Equal(t, uint64(1), s.View().Gen)
	assert.Zero(t, s.Status().Restarts)
}

// An input that ends is restarted by the pipeline; that is the stream
// working, counted and not shown as an error.
func TestSupervisorEOFIsNotAnError(t *testing.T) {
	r := newRig(t)
	r.sc.set(func(s *script) { s.eofAfter = 50 })
	s := r.start(camSpec())
	eofs := func() float64 { return metric(t, s, "openstreamer_stream_input_eof_total", "stream", "cam") }
	require.Eventually(t, func() bool { return eofs() >= 3 }, waitFor, 5*time.Millisecond)
	r.sc.set(func(s *script) { s.eofAfter = 0 })
	assert.Equal(t, float64(3), eofs())
	st := s.Status()
	assert.Empty(t, st.LastError)
	assert.Nil(t, st.LastErrorAt)
	assert.Zero(t, st.Restarts)
	assert.Equal(t, uint64(1), st.Generation)
	assert.GreaterOrEqual(t, r.sc.Opens(), 3)
}

// A child that cannot be started is retried by its own supervisor, on real
// timers, inside the one generation; the stream shows why.
func TestSupervisorSpawnFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-ostranscoder")
	rows := map[string]struct {
		sp     spec.Stream
		prefix string
	}{
		"ladder": {ladderSpec("720"), "transcoder: "},
		"thumbnail": {func() spec.Stream {
			sp := camSpec()
			sp.Thumbnail = &spec.Thumbnail{IntervalSec: 1}
			return sp
		}(), "thumbnailer: "},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.env.Transcoder = missing
			s := r.start(row.sp)
			start := time.Now()
			require.Eventually(t, func() bool {
				tr := s.Status().Transcoder
				return tr != nil && tr.Spawns >= 2 && tr.Exits >= 2
			}, 5*time.Second, 5*time.Millisecond, "the child was not respawned on real timers")
			assert.Less(t, time.Since(start), 5*time.Second)
			st := s.Status()
			assert.True(t, strings.HasPrefix(st.LastError, row.prefix), st.LastError)
			assert.Contains(t, st.LastError, "no-such-ostranscoder")
			assert.Equal(t, uint64(1), st.Generation)
			assert.Zero(t, st.Restarts)
			assert.GreaterOrEqual(t, st.Transcoder.Exits, uint64(2))
			assert.Zero(t, st.Transcoder.PID)
			assert.False(t, r.after.called(), "nothing rebuilds: the supervisor's backoff is not used")
			assert.Zero(t, metric(t, s, "openstreamer_transcoder_up", "stream", "cam"))
		})
	}
}

// Every long-lived part of a generation runs through g.run, which fails the
// generation when the part returns while the generation is live.
func TestGenerationRunnerFailure(t *testing.T) {
	for _, name := range []string{"pipeline", "transcoder", "thumbnailer"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			s := r.start(camSpec())
			waitState(t, s, "running")
			failGen(t, s, name)
			waitState(t, s, "backoff")
			st := s.Status()
			assert.Equal(t, uint64(1), st.Restarts)
			assert.Equal(t, name+": boom", st.LastError)
			assert.Equal(t, time.Second, r.after.next(t).d)
		})
	}
	t.Run("a return without an error fails it too", func(t *testing.T) {
		g := newGeneration(context.Background(), 1, time.Now())
		defer g.cancel()
		g.run("transcoder", func(context.Context) error { return nil })
		select {
		case <-g.failed:
		case <-time.After(waitFor):
			require.FailNow(t, "the generation did not fail")
		}
		prefix, err := g.failure()
		assert.Equal(t, "transcoder: ", prefix)
		require.Error(t, err)
		g.waitRunners()
	})
	t.Run("a return after the cancel does not", func(t *testing.T) {
		g := newGeneration(context.Background(), 1, time.Now())
		g.run("pipeline", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		g.run("transcoder", func(ctx context.Context) error {
			<-ctx.Done()
			return errBoom
		})
		g.cancel()
		g.waitRunners()
		select {
		case <-g.failed:
			assert.Fail(t, "a runner that returned after the cancel failed the generation")
		default:
		}
	})
	t.Run("the first failure is kept", func(t *testing.T) {
		g := newGeneration(context.Background(), 1, time.Now())
		defer g.cancel()
		g.run("pipeline", func(context.Context) error { return errBoom })
		<-g.failed
		g.run("transcoder", func(context.Context) error { return io.ErrUnexpectedEOF })
		g.waitRunners()
		prefix, err := g.failure()
		assert.Equal(t, "pipeline: ", prefix)
		assert.ErrorIs(t, err, errBoom)
	})
	t.Run("a thumbnailer whose Run returns", func(t *testing.T) {
		r := newRig(t)
		fakeChild(t, &r.env, transcodetest.EnvVersion+"=99")
		sp := camSpec()
		sp.Thumbnail = &spec.Thumbnail{IntervalSec: 1}
		s := r.start(sp)
		call := r.after.next(t)
		assert.Equal(t, time.Second, call.d)
		st := s.Status()
		assert.Equal(t, "backoff", st.State)
		assert.Equal(t, uint64(1), st.Restarts)
		assert.Regexp(t, `^thumbnailer: `, st.LastError)
	})
}

// A build that fails, because the world changed since Check, is a failed
// generation: backoff, and an edit is still answered.
func TestSupervisorAssetGone(t *testing.T) {
	r := newRig(t)
	fakeChild(t, &r.env)
	r.env.AssetsDir = writeAsset(t, "logo.png")
	asset := filepath.Join(r.env.AssetsDir, "logo.png")
	sp := ladderSpec("720")
	sp.Transcode.Watermark = &spec.Watermark{Image: "logo.png"}
	s := r.start(sp)
	waitRunning(t, s)
	first := s.View()
	g := currentGen(s)

	require.NoError(t, os.Remove(asset))
	failGen(t, s, "transcoder")
	assert.Equal(t, time.Second, r.after.fire(t), "the failed generation's backoff")
	// The build fails now.
	call := r.after.next(t)
	assert.Equal(t, 2*time.Second, call.d)
	st := s.Status()
	assert.Equal(t, "backoff", st.State)
	assert.Regexp(t, `^build: .*logo\.png`, st.LastError)
	assert.Equal(t, uint64(2), st.Restarts)
	v := s.View()
	assert.Zero(t, v.Gen)
	assert.Empty(t, v.Single)
	assert.Empty(t, v.Packs)
	assert.Nil(t, currentGen(s))
	for name, h := range first.Single {
		assert.Empty(t, h.Stats(), "subscribers left on %s", name)
	}
	assert.Empty(t, first.Source.Stats())
	assert.Empty(t, g.tr.Presentation().Stats())
	call.c <- time.Time{}
	assert.Equal(t, 4*time.Second, r.after.next(t).d, "the backoff keeps doubling over failed builds")

	// An edit that asks for a rebuild is answered when its build fails.
	next := sp
	next.Layout = []string{"video"}
	require.Equal(t, spec.ChangeRebuild, answer(t, s.Submit(next)))
	call = r.after.next(t)
	assert.Equal(t, 8*time.Second, call.d)
	assert.Regexp(t, `^build: `, s.Status().LastError, "the edit cleared the error and its build set it again")
	assert.Zero(t, s.View().Gen)

	require.NoError(t, os.WriteFile(asset, onePixelPNG, 0o600))
	call.c <- time.Time{}
	waitGen(t, s, 2)
	waitState(t, s, "running")
	require.Eventually(t, func() bool { return len(s.Status().Tracks) == 1 }, waitFor, time.Millisecond)
	assert.Equal(t, "video", s.Status().Tracks[0].Kind)
}

// A supervisor builds nothing before its predecessor has let go, and its
// Done never closes before its predecessor's.
func TestSupervisorWaitsForPredecessor(t *testing.T) {
	t.Run("builds after", func(t *testing.T) {
		r := newRig(t)
		after := make(chan struct{})
		s := r.startAfter(camSpec(), after)
		time.Sleep(100 * time.Millisecond)
		assert.Zero(t, r.sc.Opens(), "the supervisor built before its predecessor stopped")
		assert.Zero(t, s.View().Gen)
		assert.Equal(t, "starting", s.Status().State)
		close(after)
		waitState(t, s, "running")
	})
	t.Run("cancelled while waiting", func(t *testing.T) {
		r := newRig(t)
		after := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		s := NewSupervisor(camSpec(), r.env, after)
		go s.Run(ctx)
		edit := s.Submit(outputEdit(camSpec(), OutRTSP, false))
		cancel()
		select {
		case <-s.Done():
			require.FailNow(t, "Done closed before the predecessor's")
		case <-time.After(100 * time.Millisecond):
		}
		close(after)
		select {
		case <-s.Done():
		case <-time.After(waitFor):
			require.FailNow(t, "Done did not close")
		}
		assert.Zero(t, r.sc.Opens())
		select {
		case _, ok := <-edit:
			assert.False(t, ok, "an edit was answered by a supervisor that never ran")
		case <-time.After(waitFor):
			require.FailNow(t, "the pending edit's channel was not closed")
		}
		assert.Equal(t, "stopped", s.Status().State)
	})
	t.Run("a chain of three", func(t *testing.T) {
		r1, r3 := newRig(t), newRig(t)
		gate := make(chan struct{})
		ctx1, cancel1 := context.WithCancel(context.Background())
		s1 := NewSupervisor(camSpec(), r1.env, nil)
		go s1.Run(ctx1)
		waitState(t, s1, "running")
		r1.sc.set(func(s *script) { s.closeGate = gate })
		cancel1()

		ctx2, cancel2 := context.WithCancel(context.Background())
		s2 := NewSupervisor(camSpec(), r1.env, s1.Done())
		go s2.Run(ctx2)
		cancel2()
		s3 := r3.startAfter(camSpec(), s2.Done())

		time.Sleep(150 * time.Millisecond)
		assert.Zero(t, r3.sc.Opens(), "the third built while the first still held its input")
		for i, s := range []*Supervisor{s1, s2} {
			select {
			case <-s.Done():
				assert.Fail(t, "done early", "supervisor %d", i+1)
			default:
			}
		}
		close(gate)
		waitState(t, s3, "running")
		<-s1.Done()
		<-s2.Done()
		assert.Equal(t, 1, r1.sc.Opens(), "the second, cancelled while waiting, built something")
	})
	t.Run("an RTMP listen input is free for the successor", func(t *testing.T) {
		ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())
		sp := camSpec()
		sp.Inputs = []spec.Input{{URL: "rtmp://" + addr + "/in/" + vKey + "?mode=listen"}}
		sp.Outputs = spec.Outputs{HTTPTS: true}
		env := Env{}
		require.NoError(t, Check(sp, env))

		ctx1, cancel1 := context.WithCancel(context.Background())
		s1 := NewSupervisor(sp, env, nil)
		go s1.Run(ctx1)
		// The first holds the listener and the publish route.
		require.Eventually(t, func() bool {
			c, err := new(net.Dialer).DialContext(context.Background(), "tcp", addr)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		}, waitFor, 5*time.Millisecond)

		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		s2 := NewSupervisor(sp, env, s1.Done())
		cancel1()
		go s2.Run(ctx2)
		defer func() {
			cancel2()
			<-s2.Done()
		}()
		waitGen(t, s2, 1)

		pubCtx, pubCancel := context.WithCancel(context.Background())
		defer pubCancel()
		pubDone := make(chan struct{})
		go func() {
			defer close(pubDone)
			// The route may take a moment to register: retry the publish.
			for pubCtx.Err() == nil {
				_ = rtmptest.Publish(pubCtx, "rtmp://"+addr+"/in/"+vKey, flvFixture(t), true)
				select {
				case <-pubCtx.Done():
				case <-time.After(50 * time.Millisecond):
				}
			}
		}()
		defer func() {
			pubCancel()
			<-pubDone
		}()
		require.Eventually(t, func() bool {
			st := s2.Status()
			require.NotContains(t, st.LastError, "already registered")
			require.NotContains(t, st.LastError, "address already in use")
			return st.State == "running"
		}, waitFor, 5*time.Millisecond, "last error %q", s2.Status().LastError)
		assert.NotContains(t, s2.Status().LastError, vKey)
	})
}

// flvFixture returns the tags of testdata/fixtures/h264_aac.flv.
func flvFixture(t testing.TB) []rtmp.FLVTag {
	t.Helper()
	data, err := os.ReadFile("../../testdata/fixtures/h264_aac.flv")
	require.NoError(t, err)
	require.Greater(t, len(data), 13)
	var tags []rtmp.FLVTag
	for b := data[9+4:]; len(b) >= 11; {
		size := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		tsMs := uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6]) | uint32(b[7])<<24
		require.GreaterOrEqual(t, len(b), 11+size+4)
		tags = append(tags, rtmp.FLVTag{Type: b[0], Timestamp: tsMs, Data: slices.Clone(b[11 : 11+size])})
		b = b[11+size+4:]
	}
	require.NotEmpty(t, tags)
	return tags
}

// Stopping never waits for a backoff. An edit that changes the stream ends
// one at once, with the edit applied to the generation it starts; the same
// document again changes nothing and leaves the wait as it is.
func TestSupervisorStopDuringBackoff(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		r := newRig(t)
		s := r.start(camSpec())
		for gen, want := range []time.Duration{1, 2, 4, 8, 16} {
			waitGen(t, s, uint64(gen+1))
			failGen(t, s, "pipeline")
			assert.Equal(t, want*time.Second, r.after.fire(t))
		}
		waitGen(t, s, 6)
		failGen(t, s, "pipeline")
		assert.Equal(t, 30*time.Second, r.after.next(t).d)
		assert.Equal(t, "backoff", s.Status().State)
		start := time.Now()
		r.stop()
		assert.Less(t, time.Since(start), time.Second)
		assert.Equal(t, "stopped", s.Status().State)
	})
	t.Run("edit", func(t *testing.T) {
		r := newRig(t)
		sp := camSpec()
		s := r.start(sp)
		waitGen(t, s, 1)
		failGen(t, s, "pipeline")
		call := r.after.next(t)
		assert.Equal(t, "backoff", s.Status().State)
		assert.NotEmpty(t, s.Status().LastError)

		next := sp
		next.Layout = []string{"audio"}
		require.Equal(t, spec.ChangeRebuild, answer(t, s.Submit(next)))
		assert.Equal(t, uint64(2), s.View().Gen)
		assert.Empty(t, call.c, "the backoff's timer was not fired")
		assert.Empty(t, s.Status().LastError, "a rebuild for an edit clears the last error")
		assert.Nil(t, s.Status().LastErrorAt)
		assert.Equal(t, uint64(1), s.Status().Restarts)

		// An output edit during a backoff is applied to the next generation.
		failGen(t, s, "pipeline")
		r.after.next(t)
		out := outputEdit(next, OutSRT, false)
		require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(out)))
		waitGen(t, s, 3)
		assert.False(t, s.View().Outputs.SRT)
		// The same document again, in a backoff, changes nothing and does
		// not end the wait.
		failGen(t, s, "pipeline")
		r.after.next(t)
		require.Equal(t, spec.ChangeNone, answer(t, s.Submit(out)))
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, "backoff", s.Status().State)
	})
}

// Edits are applied in the order they were submitted, and none blocks.
func TestSupervisorSubmitOrder(t *testing.T) {
	r := newRig(t)
	b := camSpec()
	a := outputEdit(outputEdit(b, OutRTSP, false), OutHLS, false)
	s := r.start(b)
	waitGen(t, s, 1)
	const n = 50
	answers := make([]<-chan spec.Change, n)
	for i := range answers {
		if i%2 == 0 {
			answers[i] = s.Submit(a)
		} else {
			answers[i] = s.Submit(b)
		}
	}
	// The last answer comes after every other.
	require.Equal(t, spec.ChangeOutputs, answer(t, answers[n-1]))
	for i, c := range answers[:n-1] {
		select {
		case ch, ok := <-c:
			require.True(t, ok)
			assert.Equal(t, spec.ChangeOutputs, ch, "edit %d", i)
		default:
			require.FailNow(t, "an earlier edit was answered after a later one", "edit %d", i)
		}
	}
	assert.Equal(t, b.Outputs, s.View().Outputs)
	assert.Equal(t, uint64(1), s.View().Gen)
	// An answered channel is closed after its value.
	_, ok := <-answers[0]
	assert.False(t, ok)

	// Edits queued behind a rebuild are applied to the new generation.
	rebuilt := b
	rebuilt.Layout = []string{"video"}
	first := s.Submit(rebuilt)
	second := s.Submit(outputEdit(rebuilt, OutRTMP, false))
	third := s.Submit(outputEdit(rebuilt, OutRTMP, false))
	require.Equal(t, spec.ChangeRebuild, answer(t, first))
	require.Equal(t, spec.ChangeOutputs, answer(t, second))
	require.Equal(t, spec.ChangeNone, answer(t, third))
	assert.Equal(t, uint64(2), s.View().Gen)
	assert.False(t, s.View().Outputs.RTMP)

	r.stop()
	select {
	case _, ok := <-s.Submit(a):
		assert.False(t, ok, "an edit was answered after Run returned")
	case <-time.After(waitFor):
		require.FailNow(t, "a Submit after Run returned was not closed")
	}
}

// A supervisor leaves no goroutine behind. Not parallel, like every test of
// this package: the count is this test's.
func TestSupervisorNoLeaks(t *testing.T) {
	sc := newScript(t)
	sinkURL := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{}).URL("live/" + vKey)
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	sp := camSpec()
	sp.Outputs.Push = []spec.Push{{URL: sinkURL}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewSupervisor(sp, Env{Open: sc.Open}, nil)
	go s.Run(ctx)
	waitState(t, s, "running")
	edits := []spec.Stream{
		outputEdit(outputEdit(sp, OutHLS, false), OutDASH, false),
		outputEdit(sp, OutRTSP, false),
		func() spec.Stream { e := sp; e.Outputs.Push = nil; return e }(),
	}
	for i, e := range edits {
		require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(e)), "edit %d", i)
	}
	rebuilt := sp
	rebuilt.Layout = []string{"video"}
	require.Equal(t, spec.ChangeRebuild, answer(t, s.Submit(rebuilt)))
	waitState(t, s, "running")
	cancel()
	<-s.Done()
	// The sink's connection handlers end after the pusher's connection.
	// The count is polled here, not in assert.Eventually, which runs its
	// condition in a goroutine of its own.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines: %d before, %d after\n%s", before, after, buf[:runtime.Stack(buf, true)])
	}
}

// Collect writes the families of a stream, parseable, with no URL in a
// label.
func TestCollect(t *testing.T) {
	r := newRig(t)
	fakeChild(t, &r.env, transcodetest.EnvCounters+"=1")
	sinkA := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	sinkB := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	urlA, urlB := sinkA.URL("live/"+vKey+"a"), sinkB.URL("live/"+vKey+"b")
	sp := ladderSpec("720", "360")
	sp.Outputs.Push = []spec.Push{{URL: urlA}, {URL: urlB}}
	s := r.start(sp)
	waitState(t, s, "running")
	up := func(url string) bool {
		samples, _ := scrape(t, s)
		v, ok := metricstest.Find(samples, "openstreamer_push_up", "target", pushID(url))
		return ok && v == 1
	}
	require.Eventually(t, func() bool { return up(urlA) && up(urlB) }, waitFor, 5*time.Millisecond)

	// Removing the first push leaves the second's series as they were.
	next := sp
	next.Outputs.Push = []spec.Push{{URL: urlB}}
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(next)))
	samples, text := scrape(t, s)
	_, ok := metricstest.Find(samples, "openstreamer_push_up", "target", pushID(urlA))
	assert.False(t, ok, "the removed push still has a series")
	assert.True(t, up(urlB))
	assert.Equal(t, float64(1), metric(t, s, "openstreamer_push_connects_total", "target", pushID(urlB)))
	assert.Equal(t, pushID(urlB), pushStats(t, s, urlB).ID)

	// Every family of the table, once the ABR packager has cut a segment.
	families := []string{
		"openstreamer_stream_up", "openstreamer_stream_generation", "openstreamer_stream_restarts_total",
		"openstreamer_stream_active_input", "openstreamer_stream_input_switches_total",
		"openstreamer_stream_input_eof_total", "openstreamer_stream_unmapped_frames_total",
		"openstreamer_stream_drift_breaks_total", "openstreamer_hub_subscribers",
		"openstreamer_packager_errors_total", "openstreamer_packager_rung_gaps_total",
		"openstreamer_transcoder_up", "openstreamer_transcoder_spawns_total", "openstreamer_transcoder_exits_total",
		"openstreamer_transcoder_losses_total", "openstreamer_transcoder_frames_in_total",
		"openstreamer_transcoder_frames_out_total", "openstreamer_transcoder_dropped_frames_total",
		"openstreamer_transcoder_encoder_fallbacks_total", "openstreamer_transcoder_media_errors_total",
		"openstreamer_transcoder_thumbnails_total",
		"openstreamer_push_up", "openstreamer_push_connects_total", "openstreamer_push_failures_total",
		"openstreamer_push_republishes_total", "openstreamer_push_messages_total",
		"openstreamer_push_resyncs_total",
	}
	has := func(samples []metricstest.Sample, family string) bool {
		return slices.ContainsFunc(samples, func(m metricstest.Sample) bool {
			return m.Name == family && m.Labels["stream"] == "cam"
		})
	}
	require.Eventually(t, func() bool {
		samples, text = scrape(t, s)
		return has(samples, "openstreamer_packager_rung_gaps_total") &&
			metricFound(samples, "openstreamer_transcoder_media_errors_total", 2, "kind", "reanchors")
	}, waitFor, 10*time.Millisecond)
	for _, f := range families {
		assert.True(t, has(samples, f), "no family %s with stream=\"cam\" in:\n%s", f, text)
	}
	for _, m := range samples {
		assert.Equal(t, "cam", m.Labels["stream"], "%s lacks the stream label", m.Name)
		assert.NotEmpty(t, m.Help, "%s has no help", m.Name)
		for k, v := range m.Labels {
			assert.NotContains(t, v, "://", "%s{%s}", m.Name, k)
			assert.NotContains(t, v, vKey, "%s{%s}", m.Name, k)
		}
	}
	find := func(name string, labels ...string) float64 {
		t.Helper()
		v, ok := metricstest.Find(samples, name, append([]string{"stream", "cam"}, labels...)...)
		require.True(t, ok, "%s %v in:\n%s", name, labels, text)
		return v
	}
	c := transcodetest.Counters
	assert.Equal(t, float64(c.EncoderFallbacks), find("openstreamer_transcoder_encoder_fallbacks_total"))
	assert.Equal(t, float64(c.AudioCorrupt), find("openstreamer_transcoder_media_errors_total", "kind", "audio_corrupt"))
	assert.Equal(t, float64(c.VideoCorrupt), find("openstreamer_transcoder_media_errors_total", "kind", "video_corrupt"))
	assert.Equal(t, float64(c.AudioDropped), find("openstreamer_transcoder_media_errors_total", "kind", "audio_dropped"))
	assert.Equal(t, float64(c.DroppedTracks), find("openstreamer_transcoder_media_errors_total", "kind", "dropped_tracks"))
	assert.Equal(t, float64(1), find("openstreamer_stream_up"))
	assert.Equal(t, float64(1), find("openstreamer_stream_generation"))
	assert.Equal(t, float64(1), find("openstreamer_transcoder_up"))
	assert.Equal(t, float64(1), find("openstreamer_transcoder_spawns_total"))
	assert.Positive(t, find("openstreamer_transcoder_frames_out_total", "rendition", "720"))
	assert.Positive(t, find("openstreamer_transcoder_frames_out_total", "rendition", "360"))
	assert.Zero(t, find("openstreamer_stream_restarts_total", "reason", "failure"))
	assert.Zero(t, find("openstreamer_stream_restarts_total", "reason", "edit"))
	for _, name := range []string{"cam", "cam_720", "cam_360"} {
		assert.Zero(t, find("openstreamer_packager_errors_total", "name", name, "kind", "mux"))
		assert.Positive(t, find("openstreamer_hub_subscribers", "name", name))
	}
	for _, reason := range []string{"before_key", "merge", "oversize", "presentation"} {
		find("openstreamer_transcoder_dropped_frames_total", "reason", reason)
	}
	for _, result := range []string{"ok", "failed"} {
		find("openstreamer_transcoder_thumbnails_total", "result", result)
	}

	// A stream without a ladder or a thumbnail has no transcoder family, and
	// a stopped one only its own.
	plain := newRig(t)
	ps := plain.start(camSpec())
	waitState(t, ps, "running")
	_, ptext := scrape(t, ps)
	assert.NotContains(t, ptext, "openstreamer_transcoder_")
	assert.NotContains(t, ptext, "openstreamer_push_")
	plain.stop()
	stopped, stext := scrape(t, ps)
	assert.True(t, metricFound(stopped, "openstreamer_stream_up", 0), stext)
	assert.True(t, metricFound(stopped, "openstreamer_stream_generation", 0), stext)
	assert.NotContains(t, stext, "openstreamer_hub_subscribers")
}

// metricFound reports whether the one series of name with labels is v.
func metricFound(samples []metricstest.Sample, name string, v float64, labels ...string) bool {
	got, ok := metricstest.Find(samples, name, labels...)
	return ok && got == v
}

// Nothing a Status holds shows a password or a stream key.
func TestStatusRedacts(t *testing.T) {
	r := newRig(t)
	sink := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{Reject: true})
	pushURL := sink.URL("live/" + vKey + "?token=" + vPass)
	in0 := "rtsp://user:" + vPass + "@cam.test/stream"
	in1 := "rtmp://ingest.test/live/" + vKey
	r.sc.set(func(s *script) { s.openErr = fmt.Errorf("open %s and %s: refused", in0, in1) })
	sp := camSpec()
	sp.Inputs = []spec.Input{{URL: in0}, {URL: in1}}
	sp.Outputs.Push = []spec.Push{{URL: pushURL}}
	s := r.start(sp)
	require.Eventually(t, func() bool { return s.Status().LastError != "" }, waitFor, time.Millisecond)
	r.sc.set(func(s *script) { s.openErr = nil })
	waitState(t, s, "running")
	require.Eventually(t, func() bool {
		p := s.Status().Pushes
		return len(p) == 1 && p[0].LastError != ""
	}, waitFor, time.Millisecond, "the rejected push reported no error")

	st := s.Status()
	require.Len(t, st.Inputs, 2)
	require.Len(t, st.Pushes, 1)
	require.Len(t, st.Outputs.Push, 1)
	// The ID is keyed per process: compared with this process's, and not
	// the plain hash of the URL.
	assert.Equal(t, pushID(pushURL), st.Pushes[0].ID)
	sum := sha256.Sum256([]byte(pushURL))
	assert.NotEqual(t, hex.EncodeToString(sum[:])[:8], st.Pushes[0].ID)
	assert.Regexp(t, `^[0-9a-f]{8}$`, st.Pushes[0].ID)
	assert.Contains(t, st.Pushes[0].Target, sink.Addr)
	assert.Contains(t, st.Inputs[0], "cam.test")
	assert.Regexp(t, `^input [01]: `, st.LastError)
	body, err := json.Marshal(st)
	require.NoError(t, err)
	for _, text := range []string{
		string(body), st.LastError, st.Inputs[0], st.Inputs[1], st.Pushes[0].Target, st.Pushes[0].LastError,
		st.Outputs.Push[0].URL, fmt.Sprint(st),
	} {
		assert.NotContains(t, text, vPass)
		assert.NotContains(t, text, vKey)
	}
	// The spec the supervisor runs is untouched by the redaction.
	assert.Equal(t, pushURL, s.View().Outputs.Push[0].URL)
	_, text := scrape(t, s)
	assert.NotContains(t, text, vPass)
	assert.NotContains(t, text, vKey)

	var back map[string]any
	require.NoError(t, json.Unmarshal(body, &back))
	for _, k := range []string{
		"name", "state", "generation", "since", "restarts", "rebuilds", "last_error", "last_error_at",
		"served", "outputs", "inputs", "active_input", "input_switches", "tracks", "pushes",
	} {
		assert.Contains(t, back, k)
	}
	assert.NotContains(t, back, "transcoder")
}

// A stream with a thumbnail serves its latest shot: from a thumbnailer of
// its own when no child encodes, else from the transcoder's child. It has
// none between generations.
func TestSupervisorShot(t *testing.T) {
	thumb := &spec.Thumbnail{IntervalSec: 1}
	rows := map[string]struct {
		sp         spec.Stream
		thumbnails bool // a Thumbnailer runs, next to no transcoder or a copying one
	}{
		"no ladder": {func() spec.Stream {
			sp := camSpec()
			sp.Thumbnail = thumb
			return sp
		}(), true},
		"a ladder that copies": {func() spec.Stream {
			sp := camSpec()
			sp.Transcode = &spec.Transcode{Renditions: []spec.Rendition{{Label: "src", Copy: true}}, Audio: spec.Audio{Copy: true}}
			sp.Thumbnail = thumb
			return sp
		}(), true},
		"a ladder that encodes": {func() spec.Stream {
			sp := ladderSpec("720")
			sp.Thumbnail = thumb
			return sp
		}(), false},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			fakeChild(t, &r.env, transcodetest.EnvShots+"=1", transcodetest.EnvStatsInterval+"=50ms")
			s := r.start(row.sp)
			waitRunning(t, s)
			g := currentGen(s)
			assert.Equal(t, row.thumbnails, g.nailer != nil)
			require.Eventually(t, func() bool {
				_, _, ok := s.Shot()
				return ok
			}, waitFor, 5*time.Millisecond, "no thumbnail")
			shot, at, _ := s.Shot()
			assert.NotEmpty(t, shot.JPEG)
			assert.WithinDuration(t, time.Now(), at, time.Minute)
			require.NotNil(t, s.Status().Transcoder)
			assert.Positive(t, s.Status().Transcoder.Spawns)
			// A transcoder's child reports its count in its next Stats.
			require.Eventually(t, func() bool { return s.Status().Transcoder.Thumbnails > 0 }, waitFor, 5*time.Millisecond)
			assert.Positive(t, metric(t, s, "openstreamer_transcoder_thumbnails_total", "result", "ok"))
			assert.Equal(t, float64(1), metric(t, s, "openstreamer_transcoder_up", "stream", "cam"))

			failGen(t, s, "pipeline")
			waitState(t, s, "backoff")
			_, _, ok := s.Shot()
			assert.False(t, ok, "a shot of a generation that is gone")
			assert.Nil(t, s.Status().Transcoder)
		})
	}
}

// TestPushIDIsKeyed: the label is the same for the same URL within the
// process, differs between URLs, and is not the plain hash of the URL,
// which holds a stream key.
func TestPushIDIsKeyed(t *testing.T) {
	const url = "rtmp://h/live/secretkey"
	id := pushID(url)
	assert.Regexp(t, `^[0-9a-f]{8}$`, id)
	assert.Equal(t, id, pushID(url))
	assert.NotEqual(t, id, pushID(url+"2"))
	sum := sha256.Sum256([]byte(url))
	assert.NotEqual(t, hex.EncodeToString(sum[:4]), id, "unkeyed")
	assert.Equal(t, hex.EncodeToString(redact.Tag([]byte(url))[:4]), id)
}
