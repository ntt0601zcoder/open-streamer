package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// waitFor bounds every wait of these tests.
const waitFor = 15 * time.Second

func TestMain(m *testing.M) {
	if os.Getenv("OS_TRANSCODE_FAKE") == "1" {
		transcodetest.Main()
	}
	os.Exit(m.Run())
}

var fixture struct {
	once   sync.Once
	frames []media.Frame
	kinds  []media.Kind
	span   int64
	err    error
}

// fixtureFrames returns the frames of testdata/fixtures/h264_aac.ts in
// source time, its kinds, and the span one loop of it advances time by.
func fixtureFrames(t testing.TB) ([]media.Frame, []media.Kind, int64) {
	t.Helper()
	fixture.once.Do(func() {
		data, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
		if err != nil {
			fixture.err = err
			return
		}
		d, err := ts.NewDemuxer(bytes.NewReader(data))
		if err != nil {
			fixture.err = err
			return
		}
		for {
			f, err := d.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fixture.err = err
				return
			}
			fixture.frames = append(fixture.frames, f)
		}
		fixture.kinds = d.TrackKinds()
		lo, hi := fixture.frames[0].DTS, fixture.frames[0].DTS
		for _, f := range fixture.frames {
			lo, hi = min(lo, f.DTS), max(hi, f.DTS)
		}
		fixture.span = hi - lo + 3600
	})
	require.NoError(t, fixture.err)
	return fixture.frames, fixture.kinds, fixture.span
}

// errScriptClosed is what a scripted reader's Read returns after Close.
var errScriptClosed = errors.New("script: closed")

// script is a scriptedOpen: an Env.Open whose readers emit the fixture's
// frames in a loop, under test control.
type script struct {
	frames []media.Frame
	kinds  []media.Kind
	span   int64
	delay  time.Duration // per frame

	mu        sync.Mutex
	opens     int
	openErr   error         // Open fails with it while set
	readErr   error         // every open reader's next Read fails with it while set
	eofAfter  int           // a reader returns io.EOF after that many frames; 0 never
	closeGate chan struct{} // a reader's Close blocks until it is closed; nil never blocks
	urls      []string
}

func newScript(t testing.TB) *script {
	frames, kinds, span := fixtureFrames(t)
	return &script{frames: frames, kinds: kinds, span: span, delay: 2 * time.Millisecond}
}

// Open is the Env.Open.
func (s *script) Open(_ context.Context, url string) (ingest.Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	s.urls = append(s.urls, url)
	if s.openErr != nil {
		return nil, s.openErr
	}
	return &scriptReader{s: s, closed: make(chan struct{})}, nil
}

// Opens counts the Open calls so far.
func (s *script) Opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

func (s *script) set(f func(*script)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type scriptReader struct {
	s      *script
	n      int // frames read; Read goroutine only
	closed chan struct{}
	once   sync.Once
}

func (r *scriptReader) Read() (ingest.Frame, error) {
	select {
	case <-r.closed:
		return ingest.Frame{}, errScriptClosed
	case <-time.After(r.s.delay):
	}
	r.s.mu.Lock()
	readErr, eofAfter := r.s.readErr, r.s.eofAfter
	r.s.mu.Unlock()
	if readErr != nil {
		return ingest.Frame{}, readErr
	}
	if eofAfter > 0 && r.n >= eofAfter {
		return ingest.Frame{}, io.EOF
	}
	f := r.s.frames[r.n%len(r.s.frames)]
	shift := int64(r.n/len(r.s.frames)) * r.s.span
	f.PTS += shift
	f.DTS += shift
	r.n++
	return ingest.Frame{Frame: f, Arrival: time.Now()}, nil
}

func (r *scriptReader) Kinds() []media.Kind      { return r.s.kinds }
func (r *scriptReader) Spec() ingest.Spec        { return ingest.Spec{} }
func (r *scriptReader) Stats() map[string]uint64 { return nil }

func (r *scriptReader) Close() error {
	r.s.mu.Lock()
	gate := r.s.closeGate
	r.s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	r.once.Do(func() { close(r.closed) })
	return nil
}

// afterCall is one call of a fakeAfter: its duration, and the channel that
// fires it.
type afterCall struct {
	d time.Duration
	c chan time.Time
}

// fakeAfter is an Env.After that reports every call and fires none by
// itself.
type fakeAfter struct{ calls chan afterCall }

func newFakeAfter() *fakeAfter { return &fakeAfter{calls: make(chan afterCall, 4096)} }

func (f *fakeAfter) After(d time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.calls <- afterCall{d: d, c: c}
	return c
}

// next returns the next call.
func (f *fakeAfter) next(t testing.TB) afterCall {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(waitFor):
		require.FailNow(t, "the supervisor did not call After")
		return afterCall{}
	}
}

// fire waits for the next call, fires it and returns its duration.
func (f *fakeAfter) fire(t testing.TB) time.Duration {
	t.Helper()
	c := f.next(t)
	c.c <- time.Time{}
	return c.d
}

// called reports whether After was called and not yet taken.
func (f *fakeAfter) called() bool { return len(f.calls) > 0 }

// fakeNow is an Env.Now that advances by step at every call.
type fakeNow struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (f *fakeNow) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(f.step)
	return f.t
}

func (f *fakeNow) setStep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.step = d
}

// tapper records what a subscriber of a hub gets.
type tapper struct {
	sub    *hub.Sub
	frames atomic.Uint64
	lost   atomic.Uint64
	last   atomic.Pointer[media.Frame]
	done   chan struct{}
}

// tap subscribes to h under name and records its deliveries until the
// subscription closes; it is closed when the test ends.
func tap(t testing.TB, h *hub.Hub, name string) *tapper {
	t.Helper()
	tp := &tapper{sub: h.Subscribe(name, 4096), done: make(chan struct{})}
	go func() {
		defer close(tp.done)
		for d := range tp.sub.C() {
			tp.lost.Add(d.Lost)
			f := d.Frame
			tp.last.Store(&f)
			tp.frames.Add(1)
		}
	}()
	t.Cleanup(func() {
		tp.sub.Close()
		<-tp.done
	})
	return tp
}

// closed reports whether the subscription has ended.
func (tp *tapper) closed() bool {
	select {
	case <-tp.done:
		return true
	default:
		return false
	}
}

// more waits until n more frames have been delivered.
func (tp *tapper) more(t testing.TB, n uint64) {
	t.Helper()
	want := tp.frames.Load() + n
	require.Eventually(t, func() bool { return tp.frames.Load() >= want }, waitFor, time.Millisecond,
		"the subscriber did not get %d more frames", n)
}

// ladderSpec is camSpec with an encoded ladder of the given labels.
func ladderSpec(labels ...string) spec.Stream {
	sp := camSpec()
	sp.Transcode = &spec.Transcode{}
	for i, l := range labels {
		sp.Transcode.Renditions = append(sp.Transcode.Renditions,
			spec.Rendition{Label: l, Height: 720 - 180*i, BitrateKbps: 1000})
	}
	return sp
}

// fakeChild sets env to run the test binary as the fake transcoder child.
func fakeChild(t testing.TB, env *Env, vars ...string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	env.Transcoder = exe
	// A race-built child sleeps 1 s at exit unless told otherwise.
	env.TranscoderEnv = append([]string{"OS_TRANSCODE_FAKE=1", "GORACE=atexit_sleep_ms=0"}, vars...)
}

// rig is a supervisor under test with its scripted world.
type rig struct {
	t      testing.TB
	sc     *script
	after  *fakeAfter
	env    Env
	s      *Supervisor
	cancel context.CancelFunc
	// published becomes the supervisors' seam of the same name.
	published func(*View)
}

// newRig returns a scripted Env; tune r.env before start.
func newRig(t testing.TB) *rig {
	t.Helper()
	r := &rig{t: t, sc: newScript(t), after: newFakeAfter()}
	r.env = served(Env{Open: r.sc.Open, After: r.after.After})
	return r
}

// start runs a supervisor of sp; it is stopped and waited for when the test
// ends.
func (r *rig) start(sp spec.Stream) *Supervisor {
	r.t.Helper()
	require.NoError(r.t, sp.Validate())
	require.NoError(r.t, Check(sp, r.env))
	return r.startAfter(sp, nil)
}

// startAfter is start without the checks, chained on after.
func (r *rig) startAfter(sp spec.Stream, after <-chan struct{}) *Supervisor {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := NewSupervisor(sp, r.env, after)
	s.published = r.published
	go s.Run(ctx)
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-s.Done():
		case <-time.After(waitFor):
			r.t.Error("the supervisor did not stop")
		}
	})
	if r.s == nil {
		r.s, r.cancel = s, cancel
	}
	return s
}

// stop cancels the rig's first supervisor and waits for it.
func (r *rig) stop() {
	r.t.Helper()
	r.cancel()
	select {
	case <-r.s.Done():
	case <-time.After(waitFor):
		require.FailNow(r.t, "the supervisor did not stop")
	}
}

// waitState waits until s is in state.
func waitState(t testing.TB, s *Supervisor, state string) {
	t.Helper()
	require.Eventually(t, func() bool { return s.Status().State == state }, waitFor, time.Millisecond,
		"state %q, want %q (last error %q)", s.Status().State, state, s.Status().LastError)
}

// waitRunning waits until s is running and its View is that of the running
// generation. Status says "running" a moment before the View is published,
// so a test that reads the View next waits with this, not with waitState.
func waitRunning(t testing.TB, s *Supervisor) {
	t.Helper()
	require.Eventually(t, func() bool {
		st, v := s.Status(), s.View()
		return st.State == "running" && v.Gen != 0 && v.Gen == st.Generation
	}, waitFor, time.Millisecond,
		"state %q, generation %d, view %d (last error %q)", s.Status().State, s.Status().Generation, s.View().Gen, s.Status().LastError)
}

// waitGen waits until s serves generation gen.
func waitGen(t testing.TB, s *Supervisor, gen uint64) {
	t.Helper()
	require.Eventually(t, func() bool { return s.View().Gen == gen }, waitFor, time.Millisecond,
		"generation %d, want %d (last error %q)", s.View().Gen, gen, s.Status().LastError)
}

// answer waits for a Submit's answer.
func answer(t testing.TB, c <-chan spec.Change) spec.Change {
	t.Helper()
	select {
	case ch, ok := <-c:
		require.True(t, ok, "the edit was dropped: its channel closed without a value")
		return ch
	case <-time.After(waitFor):
		require.FailNow(t, "the edit was not answered")
		return 0
	}
}

// scrape returns what s.Collect writes, parsed strictly, and its text.
func scrape(t testing.TB, s *Supervisor) ([]metricstest.Sample, string) {
	t.Helper()
	var reg metrics.Registry
	reg.Register(s.Collect)
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	text := rec.Body.String()
	samples, err := metricstest.Parse(strings.NewReader(text))
	require.NoError(t, err, text)
	return samples, text
}

// metric returns the value of one series of s.
func metric(t testing.TB, s *Supervisor, name string, labels ...string) float64 {
	t.Helper()
	samples, text := scrape(t, s)
	v, ok := metricstest.Find(samples, name, labels...)
	require.True(t, ok, "no single series %s %v in:\n%s", name, labels, text)
	return v
}

// currentGen returns the generation s serves, nil between generations.
func currentGen(s *Supervisor) *generation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}
