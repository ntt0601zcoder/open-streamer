package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
)

// The inputs of the live-edit tests.
const (
	urlA = "http://a.test/cam.ts"
	urlB = "http://b.test/cam.ts"
	urlC = "http://c.test/cam.ts"
)

// wmLog makes the rig's fake child log its watermarks, and returns the
// reader of the log's lines.
func wmLog(t testing.TB, r *rig) func() []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watermark.log")
	fakeChild(t, &r.env, transcodetest.EnvWatermarkLog+"="+path)
	return func() []string {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// waitLines waits until the log is exactly want.
func waitLines(t testing.TB, lines func() []string, want ...string) {
	t.Helper()
	if !assert.Eventually(t, func() bool { return slices.Equal(lines(), want) }, waitFor, time.Millisecond) {
		require.FailNow(t, "the child's watermark log", "%q, want %q", lines(), want)
	}
}

// textSpec is a one-rung ladder on urlA with a text watermark.
func textSpec(text string) spec.Stream {
	sp := withInputs(ladderSpec("720"), urlA)
	sp.Transcode.Watermark = &spec.Watermark{Text: text}
	return sp
}

// withWatermark is sp with the watermark w, nil for none; sp is not changed.
func withWatermark(sp spec.Stream, w *spec.Watermark) spec.Stream {
	sp = cloneSpec(sp)
	sp.Transcode.Watermark = w
	return sp
}

// otherPNG is a valid PNG that is not onePixelPNG.
func otherPNG(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2))))
	require.NotEqual(t, len(onePixelPNG), buf.Len())
	return buf.Bytes()
}

// epochOf waits for a frame on tp and returns its epoch.
func epochOf(t testing.TB, tp *tapper) uint32 {
	t.Helper()
	tp.more(t, 20)
	return tp.last.Load().Epoch
}

// liveInputs runs sp, whose one input is urlA, replaces the input by urlB,
// and checks that nothing but the reader changed. It returns the supervisor
// and its status before the edit.
func liveInputs(t *testing.T, r *rig, sp spec.Stream) (*Supervisor, Status) {
	t.Helper()
	s := r.start(sp)
	waitRunning(t, s)
	v := s.View()
	tp := tap(t, v.Single["cam"], "rtsp x")
	epoch := epochOf(t, tp)
	before := s.Status()

	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlB))))
	assert.Equal(t, 1, r.sc.closes(urlA), "reader a is closed when the edit is answered")
	require.Eventually(t, func() bool { return r.sc.opened(urlB) == 1 }, waitFor, time.Millisecond)
	require.Eventually(t, func() bool {
		st := s.Status()
		return st.State == "running" && st.Switches == before.Switches+1
	}, waitFor, time.Millisecond)
	require.Eventually(t, func() bool { return tp.last.Load().Epoch != epoch }, waitFor, time.Millisecond,
		"no frame of the new input")
	tp.more(t, 100)
	assert.Equal(t, epoch+1, tp.last.Load().Epoch, "exactly one more epoch")
	assert.False(t, tp.closed(), "the session ended")
	assert.Zero(t, tp.lost.Load())

	st := s.Status()
	assert.Equal(t, before.Generation, st.Generation)
	assert.Equal(t, before.Since, st.Since)
	assert.Equal(t, before.Rebuilds, st.Rebuilds)
	assert.Zero(t, st.Restarts)
	assert.Equal(t, []string{urlB}, st.Inputs)
	assert.Equal(t, 0, st.ActiveInput)
	assert.Equal(t, uint64(1), st.Switches)
	assert.Empty(t, st.LastError)
	assert.Same(t, v, s.View(), "the view was published again")
	assert.Equal(t, 1, r.sc.opened(urlA))
	assert.Equal(t, 1, r.sc.opened(urlB))
	assert.Equal(t, 0, r.sc.closes(urlB))
	assert.False(t, r.after.called())
	return s, before
}

// An input edit swaps the reader under a running stream: the generation and
// its sessions go on, with one epoch change.
func TestSupervisorLiveInputs(t *testing.T) {
	r := newRig(t)
	sp := withInputs(camSpec(), urlA)
	s, _ := liveInputs(t, r, sp)
	// A password in an input never shows.
	secret := "http://u:" + vPass + "@c.test/cam.ts"
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlB, secret))))
	st := s.Status()
	require.Len(t, st.Inputs, 2)
	assert.NotContains(t, st.Inputs[1], vPass)
	assert.Equal(t, 0, r.sc.closes(urlB), "an input that stays keeps its reader")
}

// The same under a ladder: the child goes on.
func TestSupervisorLiveInputsKeepsTranscoder(t *testing.T) {
	r := newRig(t)
	fakeChild(t, &r.env)
	s, before := liveInputs(t, r, withInputs(ladderSpec("720"), urlA))
	require.NotNil(t, before.Transcoder)
	require.NotZero(t, before.Transcoder.PID)
	now := s.Status().Transcoder
	require.NotNil(t, now)
	assert.Equal(t, before.Transcoder.PID, now.PID)
	assert.Equal(t, before.Transcoder.Spawns, now.Spawns)
	assert.Zero(t, now.Exits)
}

// A watermark edit reaches the running child: changed, removed and added,
// with no rebuild, no respawn and no epoch change.
func TestSupervisorLiveWatermark(t *testing.T) {
	r := newRig(t)
	lines := wmLog(t, r)
	r.env.AssetsDir = writeAsset(t, "mark.png")
	sp := withInputs(ladderSpec("720", "360"), urlA)
	sp.Transcode.Watermark = &spec.Watermark{Text: "one"}
	s := r.start(sp)
	waitRunning(t, s)
	v := s.View()
	taps := map[string]*tapper{}
	epochs := map[string]uint32{}
	for _, name := range []string{"cam_720", "cam_360"} {
		taps[name] = tap(t, v.Single[name], "tap")
		epochs[name] = epochOf(t, taps[name])
	}
	before := s.Status() // the renditions deliver: the child is up
	require.NotZero(t, before.Transcoder.PID)
	want := append(make([]string, 0, 5), "configure text:one")
	for _, step := range []struct {
		w    *spec.Watermark
		line string
	}{
		{&spec.Watermark{Text: "two"}, "set text:two"},
		{nil, "set none"},
		{&spec.Watermark{Image: "mark.png"}, fmt.Sprintf("set image:%d", len(onePixelPNG))},
		{&spec.Watermark{Text: "three", Position: "br"}, "set text:three"},
	} {
		next := withWatermark(sp, step.w)
		require.NoError(t, Check(next, r.env), step.line)
		require.Equal(t, spec.ChangeLive, answer(t, s.Submit(next)), step.line)
		want = append(want, step.line)
		waitLines(t, lines, want...)
		// The same again changes nothing.
		require.Equal(t, spec.ChangeNone, answer(t, s.Submit(next)), step.line)
	}
	for name, tp := range taps {
		assert.Equal(t, epochs[name], epochOf(t, tp), "a new epoch on %s", name)
		assert.False(t, tp.closed())
	}
	st := s.Status()
	assert.Equal(t, before.Generation, st.Generation)
	assert.Equal(t, before.Since, st.Since)
	assert.Zero(t, st.Rebuilds)
	assert.Zero(t, st.Restarts)
	assert.Equal(t, before.Transcoder.PID, st.Transcoder.PID)
	assert.Equal(t, before.Transcoder.Spawns, st.Transcoder.Spawns)
	assert.Empty(t, st.LastError)
	assert.Same(t, v, s.View())
	assert.Equal(t, 1, r.sc.Opens(), "a watermark edit touched the inputs")
}

// One edit of the inputs, the watermark and an output is one live edit, and
// each part takes effect.
func TestSupervisorLiveEditAllParts(t *testing.T) {
	r := newRig(t)
	lines := wmLog(t, r)
	sp := textSpec("one")
	s := r.start(sp)
	waitRunning(t, s)
	h := s.View().Single["cam"]
	rtsp, rtmp := tap(t, h, "rtsp x"), tap(t, h, "rtmp x")
	rtsp.more(t, 20)
	before := s.Status()

	next := outputEdit(withWatermark(withInputs(sp, urlB), &spec.Watermark{Text: "two"}), OutRTSP, false)
	c := s.Submit(next)
	require.Equal(t, spec.ChangeLive, answer(t, c))
	_, open := <-c
	assert.False(t, open, "one answer")
	assert.Equal(t, 1, r.sc.closes(urlA))
	require.Eventually(t, func() bool { return r.sc.opened(urlB) == 1 }, waitFor, time.Millisecond)
	waitLines(t, lines, "configure text:one", "set text:two")
	require.Eventually(t, rtsp.closed, waitFor, time.Millisecond, "the rtsp session did not end")
	assert.False(t, s.View().Outputs.RTSP)
	sub := h.Subscribe("rtsp y", 8)
	select {
	case _, open := <-sub.C():
		assert.False(t, open, "an rtsp subscriber was served")
	case <-time.After(waitFor):
		assert.Fail(t, "an rtsp subscriber was not refused")
	}
	sub.Close()
	rtmp.more(t, 50)
	assert.False(t, rtmp.closed(), "the rtmp session ended")

	st := s.Status()
	assert.Equal(t, before.Generation, st.Generation)
	assert.Equal(t, before.Since, st.Since)
	assert.Zero(t, st.Rebuilds)
	assert.Equal(t, before.Transcoder.PID, st.Transcoder.PID)
	assert.Equal(t, []string{urlB}, st.Inputs)
	assert.False(t, st.Outputs.RTSP)
}

// Between generations a live edit replaces the spec, clears the error of
// the generation that is gone and ends the wait.
func TestSupervisorLiveEditInBackoff(t *testing.T) {
	r := newRig(t)
	lines := wmLog(t, r)
	r.env.AssetsDir = writeAsset(t, "logo.png")
	sp := withWatermark(withInputs(ladderSpec("720"), urlA), &spec.Watermark{Image: "logo.png"})
	s := r.start(sp)
	waitRunning(t, s)
	waitLines(t, lines, fmt.Sprintf("configure image:%d", len(onePixelPNG)))
	require.NoError(t, os.Remove(filepath.Join(r.env.AssetsDir, "logo.png")))
	failGen(t, s, "transcoder")
	assert.Equal(t, time.Second, r.after.fire(t))
	call := r.after.next(t) // the build failed; this wait is never fired
	assert.Equal(t, 2*time.Second, call.d)
	st := s.Status()
	require.Equal(t, "backoff", st.State)
	require.Regexp(t, `^build: .*logo\.png`, st.LastError)

	next := withWatermark(withInputs(sp, urlB), &spec.Watermark{Text: "two"})
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(next)))
	// The answer comes before the build: nothing is recorded in between.
	waitGen(t, s, 2)
	waitRunning(t, s)
	st = s.Status()
	assert.Empty(t, st.LastError)
	assert.Nil(t, st.LastErrorAt)
	assert.Equal(t, []string{urlB}, st.Inputs)
	assert.Zero(t, st.Rebuilds, "a live edit is not a rebuild")
	assert.Equal(t, uint64(2), st.Restarts)
	assert.Equal(t, 1, r.sc.opened(urlB))
	assert.Equal(t, 1, r.sc.opened(urlA))
	waitLines(t, lines, fmt.Sprintf("configure image:%d", len(onePixelPNG)), "configure text:two")
	assert.False(t, r.after.called(), "the wait did not end with the edit")
}

// The error is cleared when the edit is answered, not only by the build.
func TestSupervisorLiveEditInBackoffClearsAtOnce(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	var held atomic.Bool
	// The second build's first Open is held, so the status is read between
	// the answer and anything the new generation could record.
	r.env.Open = func(ctx context.Context, url string) (ingest.Reader, error) {
		if held.Load() {
			<-gate
		}
		return r.sc.Open(ctx, url)
	}
	defer close(gate)
	s := r.start(withInputs(camSpec(), urlA))
	waitRunning(t, s)
	failGen(t, s, "pipeline")
	r.after.next(t)
	require.Regexp(t, `^pipeline: boom`, s.Status().LastError)
	held.Store(true)
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(camSpec(), urlB))))
	st := s.Status()
	assert.Empty(t, st.LastError)
	assert.Equal(t, []string{urlB}, st.Inputs)
}

// An error of SetInputs is reported, and the spec in force is the new one;
// a pipeline that has ended is not an error.
func TestSupervisorLiveInputsError(t *testing.T) {
	r := newRig(t)
	var ret atomic.Pointer[error]
	var calls atomic.Int64
	var got atomic.Pointer[[]string]
	r.tune = func(s *Supervisor) {
		s.setInputs = func(_ context.Context, urls []string) error {
			calls.Add(1)
			got.Store(&urls)
			return *ret.Load()
		}
	}
	sp := withInputs(camSpec(), urlA)
	s := r.start(sp)
	waitRunning(t, s)

	boom := fmt.Errorf("pipeline: set inputs: parse: bad url rtsp://u:%s@h/x", vPass)
	ret.Store(&boom)
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlB))))
	st := s.Status()
	assert.Regexp(t, `^inputs: pipeline: set inputs: parse: bad url`, st.LastError)
	assert.NotContains(t, st.LastError, vPass)
	assert.NotNil(t, st.LastErrorAt)
	assert.Equal(t, []string{urlB}, st.Inputs)
	assert.Equal(t, []string{urlB}, *got.Load())
	assert.Equal(t, uint64(1), st.Generation)

	// The same spec again is no edit: the pipeline is not asked again.
	require.Equal(t, spec.ChangeNone, answer(t, s.Submit(withInputs(sp, urlB))))
	assert.Equal(t, int64(1), calls.Load())

	for _, gone := range []error{pipeline.ErrNotRunning, context.Canceled} {
		s.clearErr(prefixInputs)
		ret.Store(&gone)
		require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlC))), gone)
		st = s.Status()
		assert.Empty(t, st.LastError, gone)
		assert.Equal(t, []string{urlC}, st.Inputs)
		// And a success keeps what other parts recorded.
		var none error
		ret.Store(&none)
		s.fail("transcoder: ", errBoom)
		require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlB))))
		assert.Equal(t, "transcoder: boom", s.Status().LastError)
		s.clearErr("transcoder: ")
	}
	assert.Equal(t, int64(5), calls.Load())
}

// Status does not wait behind a SetInputs that waits for a reader's Close.
func TestSupervisorStatusDuringSetInputs(t *testing.T) {
	r := newRig(t)
	sp := withInputs(camSpec(), urlA)
	s := r.start(sp)
	waitRunning(t, s)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	r.sc.set(func(sc *script) { sc.closeGate = gate })

	c := s.Submit(withInputs(sp, urlB))
	require.Eventually(t, func() bool { return slices.Equal(s.Status().Inputs, []string{urlB}) }, waitFor, time.Millisecond)
	time.Sleep(50 * time.Millisecond) // SetInputs is in the reader's Close
	for range 20 {
		start := time.Now()
		st := s.Status()
		assert.Less(t, time.Since(start), 100*time.Millisecond, "Status waited")
		assert.Equal(t, []string{urlB}, st.Inputs)
		assert.Equal(t, uint64(1), st.Generation)
		_, _ = scrape(t, s)
	}
	select {
	case <-c:
		require.FailNow(t, "the edit was answered while the reader's Close was held")
	default:
	}
	assert.Zero(t, r.sc.closes(urlA))
	r.sc.set(func(sc *script) { sc.closeGate = nil })
	release()
	require.Equal(t, spec.ChangeLive, answer(t, c))
	assert.Equal(t, 1, r.sc.closes(urlA))
	waitState(t, s, "running")
}

// Inputs that fail while live edits replace them: the pipeline's OnEvent
// records each failure under the supervisor's lock while a SetInputs waits
// for the pipeline's loop. Nothing deadlocks, and Status stays responsive.
func TestSupervisorLiveEditInputFails(t *testing.T) {
	r := newRig(t)
	sp := withInputs(camSpec(), urlA)
	s := r.start(sp)
	waitRunning(t, s)
	// Every reader fails at its first read from here on: each new URL is
	// opened at once and fails 2 ms later, in the middle of the next edits.
	r.sc.set(func(sc *script) { sc.readErr = errors.New("connection reset") })

	stop := make(chan struct{})
	polled := make(chan time.Duration)
	var failed atomic.Int64 // statuses that showed an input's failure
	go func() {
		var worst time.Duration
		for {
			select {
			case <-stop:
				polled <- worst
				return
			default:
			}
			start := time.Now()
			st := s.Status()
			worst = max(worst, time.Since(start))
			if strings.HasPrefix(st.LastError, "input ") {
				failed.Add(1)
			}
		}
	}()
	const rounds = 300
	for i := range rounds {
		next := withInputs(sp, fmt.Sprintf("http://h%d.test/a.ts", i), fmt.Sprintf("http://h%d.test/b.ts", i))
		require.Equal(t, spec.ChangeLive, answer(t, s.Submit(next)), "edit %d", i)
		if i%10 == 0 {
			time.Sleep(5 * time.Millisecond) // let the new readers fail
		}
	}
	close(stop)
	select {
	case worst := <-polled:
		assert.Less(t, worst, time.Second, "Status waited behind an edit")
	case <-time.After(waitFor):
		require.FailNow(t, "Status did not return")
	}
	assert.NotZero(t, failed.Load(), "no input failed during the edits: the test tested nothing")
	st := s.Status()
	assert.Equal(t, uint64(1), st.Generation)
	assert.Zero(t, st.Restarts)
	assert.Zero(t, st.Rebuilds)

	r.sc.set(func(sc *script) { sc.readErr = nil })
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlB))))
	waitState(t, s, "running")
}

// A watermark whose asset went away after Check is not applied: the old one
// stays, the stream goes on, and the status says why.
func TestSupervisorWatermarkAssetGone(t *testing.T) {
	r := newRig(t)
	lines := wmLog(t, r)
	r.env.AssetsDir = writeAsset(t, "logo.png")
	sp := textSpec("one")
	s := r.start(sp)
	waitRunning(t, s)
	waitLines(t, lines, "configure text:one")
	next := withWatermark(sp, &spec.Watermark{Image: "logo.png"})
	require.NoError(t, Check(next, r.env))
	require.NoError(t, os.Remove(filepath.Join(r.env.AssetsDir, "logo.png")))

	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(next)))
	st := s.Status()
	// The asset's name and the cause, once each, never the directory.
	assert.Equal(t, `watermark: picture: asset "logo.png": no such file or directory`, st.LastError)
	assert.NotContains(t, st.LastError, r.env.AssetsDir)
	assert.Equal(t, "running", st.State)
	assert.Equal(t, uint64(1), st.Generation)
	assert.Zero(t, st.Restarts)
	assert.Zero(t, st.Transcoder.Exits)
	// The next watermark the child gets is the next edit's.
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withWatermark(sp, &spec.Watermark{Text: "two"}))))
	waitLines(t, lines, "configure text:one", "set text:two")
	assert.Empty(t, s.Status().LastError, "a watermark that was applied clears the watermark error")
	assert.False(t, r.after.called())
}

// A watermark the transcoder refuses is reported as "watermark: <why>",
// the word once, not with the transcoder's own "set watermark: " again.
func TestSupervisorWatermarkRefusedOnce(t *testing.T) {
	r := newRig(t)
	lines := wmLog(t, r)
	r.tune = func(s *Supervisor) {
		s.setWatermark = func(tr *transcode.Transcoder, _ *picture.Watermark) error {
			return tr.SetWatermark(&picture.Watermark{Text: "50%"})
		}
	}
	sp := textSpec("one")
	s := r.start(sp)
	waitRunning(t, s)
	waitLines(t, lines, "configure text:one")
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withWatermark(sp, &spec.Watermark{Text: "two"}))))
	assert.Equal(t, "watermark: text ends in a lone %", s.Status().LastError)
}

// refreshed waits for a Refresh's answer.
func refreshed(t testing.TB, c <-chan error) error {
	t.Helper()
	select {
	case err, ok := <-c:
		require.True(t, ok, "the refresh was dropped: its channel closed without a value")
		return err
	case <-time.After(waitFor):
		require.FailNow(t, "the refresh was not answered")
		return nil
	}
}

// Refresh reads the watermark's asset again and hands it to the running
// child.
func TestSupervisorRefresh(t *testing.T) {
	for name, earlier := range map[string]string{"clean": "", "an earlier error": "transcoder: boom"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			lines := wmLog(t, r)
			r.env.AssetsDir = writeAsset(t, "logo.png")
			asset := filepath.Join(r.env.AssetsDir, "logo.png")
			sp := withWatermark(withInputs(ladderSpec("720"), urlA), &spec.Watermark{Image: "logo.png"})
			s := r.start(sp)
			waitRunning(t, s)
			if earlier != "" {
				s.fail("transcoder: ", errBoom)
			}
			want := append(make([]string, 0, 5), fmt.Sprintf("configure image:%d", len(onePixelPNG)))
			waitLines(t, lines, want...) // the child is up: what follows is set, not configured

			// The file replaced by other valid bytes.
			other := otherPNG(t)
			require.NoError(t, os.WriteFile(asset, other, 0o600))
			require.NoError(t, refreshed(t, s.Refresh()))
			want = append(want, fmt.Sprintf("set image:%d", len(other)))
			waitLines(t, lines, want...)
			assert.Equal(t, earlier, s.Status().LastError, "a refresh clears only a watermark error")

			// The file removed: the old watermark stays.
			require.NoError(t, os.Remove(asset))
			c := s.Refresh()
			err := refreshed(t, c)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "logo.png")
			_, open := <-c
			assert.False(t, open, "a refresh's channel is closed after its value")
			assert.Regexp(t, `^watermark: .*logo\.png`, s.Status().LastError)
			assert.Equal(t, "running", s.Status().State)

			// The file put back.
			require.NoError(t, os.WriteFile(asset, onePixelPNG, 0o600))
			require.NoError(t, refreshed(t, s.Refresh()))
			want = append(want, fmt.Sprintf("set image:%d", len(onePixelPNG)))
			waitLines(t, lines, want...) // and nothing for the failed refresh
			assert.Empty(t, s.Status().LastError)
			assert.Nil(t, s.Status().LastErrorAt)

			// Without a watermark there is nothing to refresh.
			require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withWatermark(sp, nil))))
			require.NoError(t, refreshed(t, s.Refresh()))
			// SetWatermark calls that pile up collapse to the latest: the
			// child has the first before the second is made.
			want = append(want, "set none")
			waitLines(t, lines, want...)
			require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withWatermark(sp, &spec.Watermark{Text: "t"}))))
			want = append(want, "set text:t")
			waitLines(t, lines, want...)
			st := s.Status()
			assert.Equal(t, uint64(1), st.Generation)
			assert.Equal(t, uint64(1), st.Transcoder.Spawns)

			// Between generations.
			failGen(t, s, "transcoder")
			r.after.next(t)
			require.Equal(t, "backoff", s.Status().State)
			require.ErrorIs(t, refreshed(t, s.Refresh()), ErrNoGeneration)
			assert.Equal(t, "backoff", s.Status().State, "a refresh ended the wait")

			// After Run.
			r.stop()
			select {
			case err, open := <-s.Refresh():
				assert.False(t, open, "a refresh was answered after Run returned: %v", err)
			case <-time.After(waitFor):
				require.FailNow(t, "a Refresh after Run returned was not closed")
			}
		})
	}
}

// A stream without a ladder has no watermark to refresh.
func TestSupervisorRefreshWithoutLadder(t *testing.T) {
	r := newRig(t)
	s := r.start(camSpec())
	waitRunning(t, s)
	require.NoError(t, refreshed(t, s.Refresh()))
	// A refresh queued when Run ends is dropped.
	r.sc.set(func(sc *script) { sc.closeGate = make(chan struct{}) })
	gate := r.sc.closeGate
	live := s.Submit(withInputs(camSpec(), urlB)) // holds the loop in SetInputs
	require.Eventually(t, func() bool { return slices.Equal(s.Status().Inputs, []string{urlB}) }, waitFor, time.Millisecond)
	c := s.Refresh()
	r.cancel()
	close(gate)
	select {
	case <-s.Done():
	case <-time.After(waitFor):
		require.FailNow(t, "the supervisor did not stop")
	}
	// The refresh was taken before the loop saw the end, or dropped with
	// the mailbox: answered nil or closed, never left open.
	select {
	case err, ok := <-c:
		if ok {
			assert.NoError(t, err)
		}
	default:
		assert.Fail(t, "a refresh was left unanswered when Run returned")
	}
	select {
	case <-live:
	default:
		assert.Fail(t, "an edit was left unanswered when Run returned")
	}
}

// Edits and refreshes are taken in the order they were made.
func TestSupervisorRefreshInOrder(t *testing.T) {
	r := newRig(t)
	lines := wmLog(t, r)
	entered, gate := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var texts []string
	r.tune = func(s *Supervisor) {
		s.setInputs = func(context.Context, []string) error {
			close(entered)
			<-gate
			return nil
		}
		s.setWatermark = func(tr *transcode.Transcoder, w *picture.Watermark) error {
			mu.Lock()
			texts = append(texts, w.Text)
			mu.Unlock()
			return tr.SetWatermark(w)
		}
	}
	sp := textSpec("zero")
	s := r.start(sp)
	waitRunning(t, s)
	waitLines(t, lines, "configure text:zero")

	// The loop is held in an input edit.
	moved := withInputs(sp, urlB)
	hold := s.Submit(moved)
	select {
	case <-entered:
	case <-time.After(waitFor):
		require.FailNow(t, "the input edit did not reach the pipeline")
	}
	a := s.Submit(withWatermark(moved, &spec.Watermark{Text: "A"}))
	ref := s.Refresh()
	b := s.Submit(withWatermark(moved, &spec.Watermark{Text: "B"}))
	close(gate)
	require.Equal(t, spec.ChangeLive, answer(t, b))
	for _, c := range []<-chan spec.Change{hold, a} {
		select {
		case ch := <-c:
			assert.Equal(t, spec.ChangeLive, ch)
		default:
			require.FailNow(t, "an earlier edit was answered after a later one")
		}
	}
	select {
	case err := <-ref:
		assert.NoError(t, err)
	default:
		require.FailNow(t, "the refresh was answered after a later edit")
	}
	mu.Lock()
	assert.Equal(t, []string{"A", "A", "B"}, texts)
	mu.Unlock()
	// The transcoder collapses calls that pile up: the child's log ends in
	// B, whatever it saw before.
	if !assert.Eventually(t, func() bool {
		l := lines()
		return l[len(l)-1] == "set text:B"
	}, waitFor, time.Millisecond) {
		require.FailNow(t, "the child's watermark log does not end in B", "%q", lines())
	}
}

// A live edit of the inputs clears an input's error, which may name an
// input that is gone, and no other part's.
func TestSupervisorClearsStaleInputError(t *testing.T) {
	r := newRig(t)
	sp := withInputs(camSpec(), urlA, urlB)
	s := r.start(sp)
	waitRunning(t, s)
	s.fail("input 1: ", errors.New("connection refused"))
	require.Regexp(t, `^input 1: `, s.Status().LastError)
	// An output edit leaves it.
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(outputEdit(sp, OutSRT, false))))
	require.Regexp(t, `^input 1: `, s.Status().LastError)
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlA))))
	st := s.Status()
	assert.Empty(t, st.LastError)
	assert.Nil(t, st.LastErrorAt)

	s.fail("transcoder: ", errBoom)
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(withInputs(sp, urlA, urlC))))
	assert.Equal(t, "transcoder: boom", s.Status().LastError)
	assert.Equal(t, uint64(1), s.Status().Generation)
}

// A child that ends because a no_fallback rendition could not open its
// encoder is counted, in the status and in the metrics, and shown.
func TestSupervisorRefusalStatus(t *testing.T) {
	r := newRig(t)
	fakeChild(t, &r.env, transcodetest.EnvRefuse+"=0")
	sp := ladderSpec("720")
	sp.Transcode.Renditions[0].Encoder = "nvenc"
	sp.Transcode.Renditions[0].NoFallback = true
	s := r.start(sp)
	require.Eventually(t, func() bool {
		st := s.Status()
		return st.Transcoder != nil && st.Transcoder.EncoderRefusals > 0 && st.LastError != ""
	}, waitFor, time.Millisecond)
	// The counter moves on while the child is respawned: read both in one
	// generation, and compare when they agree on a second look.
	require.Eventually(t, func() bool {
		st := s.Status()
		if st.Transcoder == nil {
			return false
		}
		samples, _ := scrape(t, s)
		again := s.Status()
		if again.Transcoder == nil || again.Transcoder.EncoderRefusals != st.Transcoder.EncoderRefusals {
			return false
		}
		return metricFound(samples, "openstreamer_transcoder_encoder_refusals_total",
			float64(st.Transcoder.EncoderRefusals), "stream", "cam")
	}, waitFor, time.Millisecond)
	st := s.Status()
	assert.Regexp(t, `^transcoder: `, st.LastError)
	assert.Contains(t, st.LastError, "no_fallback")
	_, text := scrape(t, s)
	assert.Contains(t, text, "# HELP openstreamer_transcoder_encoder_refusals_total Transcoder children that ended because a no_fallback rendition could not open its encoder.")
}

// The manager reports a live edit as "live", and keeps the supervisor.
func TestManagerLiveOutcome(t *testing.T) {
	r := newMrig(t, func(env *Env) { fakeChild(t, env) })
	cam := textSpec("one")
	other := named("other")
	assert.Equal(t, outcomes("cam", "created", "other", "created"), r.replace(cam, other))
	r.waitRunning("cam", "other")
	sup, gen := r.sup("cam"), r.status("cam").Generation

	cam = withInputs(cam, urlB)
	assert.Equal(t, outcomes("cam", "live", "other", "unchanged"), r.replace(cam, other))
	cam = withWatermark(cam, &spec.Watermark{Text: "two"})
	assert.Equal(t, outcomes("cam", "live", "other", "unchanged"), r.replace(cam, other))
	cam = withWatermark(cam, nil)
	assert.Equal(t, outcomes("cam", "live", "other", "unchanged"), r.replace(cam, other))
	assert.Same(t, sup, r.sup("cam"))
	assert.Equal(t, gen, r.status("cam").Generation)
	assert.Zero(t, r.status("cam").Rebuilds)
	got, ok := r.m.Get("cam")
	require.True(t, ok)
	assert.Equal(t, cam, got)
	assert.Equal(t, []string{urlB}, r.status("cam").Inputs)

	cam = outputEdit(cam, OutRTSP, false)
	assert.Equal(t, outcomes("cam", "outputs", "other", "unchanged"), r.replace(cam, other))
	// Inputs and outputs together are still live.
	cam = outputEdit(withInputs(cam, urlC), OutRTSP, true)
	assert.Equal(t, outcomes("cam", "live", "other", "unchanged"), r.replace(cam, other))
	assert.Same(t, sup, r.sup("cam"))
	assert.Equal(t, gen, r.status("cam").Generation)

	cam.Layout = []string{"audio", "video"}
	assert.Equal(t, outcomes("cam", "rebuild", "other", "unchanged"), r.replace(cam, other))
	assert.Same(t, sup, r.sup("cam"), "a rebuild keeps the supervisor too")
	assert.Equal(t, gen+1, r.status("cam").Generation)
	assert.Equal(t, uint64(1), r.status("cam").Rebuilds)
}

// langs is the audio language each packager of v reports, by served name.
func langs(v *View) map[string]string {
	out := make(map[string]string, len(v.Packs))
	for name, p := range v.Packs {
		out[name] = p.Snapshot().AudioLanguage
	}
	return out
}

// An audio language edit reaches every running packager without replacing
// one, and packagers started later start with the language in force.
func TestSupervisorLiveAudioLanguage(t *testing.T) {
	r := newRig(t)
	fakeChild(t, &r.env)
	sp := ladderSpec("720", "360")
	sp.AudioLanguage = "vi"
	s := r.start(sp)
	waitRunning(t, s)
	v := s.View()
	names := []string{"cam", "cam_360", "cam_720"}
	require.Equal(t, names, slices.Sorted(maps.Keys(v.Packs)))
	all := func(lang string) map[string]string {
		out := map[string]string{}
		for _, name := range names {
			out[name] = lang
		}
		return out
	}
	assert.Equal(t, all("vi"), langs(v))
	packs := maps.Clone(v.Packs)
	before := s.Status()

	for _, lang := range []string{"en", ""} {
		next := cloneSpec(sp)
		next.AudioLanguage = lang
		require.NoError(t, Check(next, r.env), lang)
		require.Equal(t, spec.ChangeLive, answer(t, s.Submit(next)), lang)
		now := s.View()
		assert.Equal(t, all(lang), langs(now), lang)
		require.Len(t, now.Packs, len(packs), lang)
		for name, p := range packs {
			assert.Same(t, p, now.Packs[name], "%s was replaced (%q)", name, lang)
		}
		st := s.Status()
		assert.Equal(t, before.Generation, st.Generation, lang)
		assert.Equal(t, before.Since, st.Since, lang)
		assert.Zero(t, st.Rebuilds, lang)
		sp = next
	}

	// With neither HLS nor DASH on there is no packager to tell; the ones
	// started later read the language in force.
	off := outputEdit(outputEdit(sp, OutHLS, false), OutDASH, false)
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(off)))
	require.Empty(t, s.View().Packs)
	fr := cloneSpec(off)
	fr.AudioLanguage = "fr"
	require.Equal(t, spec.ChangeLive, answer(t, s.Submit(fr)))
	assert.Empty(t, s.View().Packs)
	on := outputEdit(fr, OutHLS, true)
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(on)))
	now := s.View()
	require.Equal(t, names, slices.Sorted(maps.Keys(now.Packs)))
	assert.Equal(t, all("fr"), langs(now))
	for name, p := range packs {
		assert.NotSame(t, p, now.Packs[name], "%s was not started again", name)
	}
	assert.Equal(t, before.Generation, s.Status().Generation)
}
