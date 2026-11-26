package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
)

// mrig is a manager under test, running, with a scripted world.
type mrig struct {
	t       testing.TB
	sc      *script
	m       *Manager
	cancel  context.CancelFunc
	done    chan struct{}
	commits atomic.Int64
}

// newMrig returns a running manager whose streams read scripted inputs;
// tune changes its Env first. It is stopped and waited for when the test
// ends.
func newMrig(t testing.TB, tune ...func(*Env)) *mrig {
	t.Helper()
	r := &mrig{t: t, sc: newScript(t)}
	env := served(Env{Open: r.sc.Open})
	for _, f := range tune {
		f(&env)
	}
	r.m = NewManager(env)
	r.run()
	return r
}

// run starts the manager's Run.
func (r *mrig) run() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() {
		defer close(r.done)
		r.m.Run(ctx)
	}()
	r.t.Cleanup(r.stop)
}

// stop ends Run and waits for it.
func (r *mrig) stop() {
	r.t.Helper()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(waitFor):
		r.t.Error("the manager did not stop")
	}
}

// commit counts its calls.
func (r *mrig) commit() error {
	r.commits.Add(1)
	return nil
}

// replace makes specs the set, which must be accepted.
func (r *mrig) replace(specs ...spec.Stream) []Outcome {
	r.t.Helper()
	for _, sp := range specs {
		require.NoError(r.t, sp.Validate(), sp.Name)
	}
	out, err := r.m.Replace(context.Background(), specs, r.commit)
	require.NoError(r.t, err)
	return out
}

// refuse makes specs the set, which must be refused without a commit.
func (r *mrig) refuse(specs ...spec.Stream) error {
	r.t.Helper()
	before := r.commits.Load()
	out, err := r.m.Replace(context.Background(), specs, r.commit)
	require.Error(r.t, err)
	require.Nil(r.t, out)
	require.Equal(r.t, before, r.commits.Load(), "commit was called for a refused edit")
	return err
}

// sup returns the supervisor of name, nil without one.
func (r *mrig) sup(name string) *Supervisor {
	e := (*r.m.streams.Load())[name]
	if e == nil {
		return nil
	}
	return e.sup
}

// status returns the status of name, which must exist.
func (r *mrig) status(name string) Status {
	r.t.Helper()
	st, ok := r.m.Status(name)
	require.True(r.t, ok, "no stream %q", name)
	return st
}

// waitRunning waits until every stream of names has an active input and
// its supervisor's View is that of the running generation: Status says
// "running" a moment before the View is published.
func (r *mrig) waitRunning(names ...string) {
	r.t.Helper()
	for _, name := range names {
		require.Eventually(r.t, func() bool {
			st, s := r.status(name), r.sup(name)
			return st.State == "running" && s != nil && s.View().Gen != 0 && s.View().Gen == st.Generation
		}, waitFor, time.Millisecond,
			"%s: state %q, want running (last error %q)", name, r.status(name).State, r.status(name).LastError)
	}
}

// tails counts the manager's tails.
func (r *mrig) tails() int {
	r.m.edit.Lock()
	defer r.m.edit.Unlock()
	return len(r.m.tails)
}

// named is camSpec under another name, with an input of its own.
func named(name string) spec.Stream {
	sp := camSpec()
	sp.Name = name
	sp.Inputs = []spec.Input{{URL: "http://source.test/" + name + ".ts"}}
	return sp
}

// withInputs is sp with these inputs.
func withInputs(sp spec.Stream, urls ...string) spec.Stream {
	sp.Inputs = nil
	for _, u := range urls {
		sp.Inputs = append(sp.Inputs, spec.Input{URL: u})
	}
	return sp
}

// outcomes builds the expected outcomes from name, applied pairs.
func outcomes(pairs ...string) []Outcome {
	var out []Outcome
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Outcome{Name: pairs[i], Applied: pairs[i+1]})
	}
	return out
}

// scrapeManager returns what m.Collect writes, parsed strictly.
func scrapeManager(t testing.TB, m *Manager) []metricstest.Sample {
	t.Helper()
	var reg metrics.Registry
	reg.Register(m.Collect)
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	samples, err := metricstest.Parse(strings.NewReader(rec.Body.String()))
	require.NoError(t, err, rec.Body.String())
	return samples
}

// A stream goes through every outcome of an edit.
func TestManagerReplaceCreatesAndEdits(t *testing.T) {
	r := newMrig(t)
	cam := camSpec()
	assert.Equal(t, outcomes("cam", "created"), r.replace(cam))
	assert.Equal(t, int64(1), r.commits.Load())
	r.waitRunning("cam")
	got, ok := r.m.Get("cam")
	require.True(t, ok)
	assert.Equal(t, cam, got)

	// The same set: nothing changes, and the caller still decides whether
	// to write.
	assert.Equal(t, outcomes("cam", "unchanged"), r.replace(cam))
	assert.Equal(t, int64(2), r.commits.Load())

	cam.Outputs.RTSP = false
	assert.Equal(t, outcomes("cam", "outputs"), r.replace(cam))
	assert.Equal(t, uint64(1), r.status("cam").Generation)
	_, ok = r.m.Hub(OutRTSP, "cam")
	assert.False(t, ok)
	_, ok = r.m.Hub(OutRTMP, "cam")
	assert.True(t, ok)

	cam.Layout = []string{"audio", "video"}
	assert.Equal(t, outcomes("cam", "rebuild"), r.replace(cam))
	assert.Equal(t, uint64(2), r.status("cam").Generation)
	assert.Equal(t, uint64(1), r.status("cam").Rebuilds)

	first := r.sup("cam")
	cam.Disabled = true
	assert.Equal(t, outcomes("cam", "stopped"), r.replace(cam))
	st := r.status("cam")
	assert.Equal(t, "stopped", st.State)
	assert.Equal(t, -1, st.ActiveInput)
	assert.Equal(t, []string{"cam"}, st.Served)
	assert.Equal(t, []string{"http://source.test/cam.ts"}, st.Inputs)
	assert.Empty(t, st.LastError)
	_, ok = r.m.Hub(OutRTMP, "cam")
	assert.False(t, ok)
	_, ok = r.m.CopyHub("cam")
	assert.False(t, ok)
	got, _ = r.m.Get("cam")
	assert.True(t, got.Disabled)
	// A disabled stream that changes stays stopped.
	cam.Outputs.RTSP = true
	assert.Equal(t, outcomes("cam", "stopped"), r.replace(cam))

	cam.Disabled = false
	assert.Equal(t, outcomes("cam", "started"), r.replace(cam))
	r.waitRunning("cam")
	assert.NotSame(t, first, r.sup("cam"))
	select {
	case <-first.Done():
	case <-time.After(waitFor):
		require.FailNow(t, "the stopped supervisor is still running")
	}

	assert.Equal(t, outcomes("cam", "deleted"), r.replace())
	_, ok = r.m.Get("cam")
	assert.False(t, ok)
	_, ok = r.m.Status("cam")
	assert.False(t, ok)
	_, ok = r.m.Hub(OutRTMP, "cam")
	assert.False(t, ok)
	_, _, ok = r.m.Shot("cam")
	assert.False(t, ok)
	assert.Empty(t, r.m.List())
	assert.Equal(t, int64(8), r.commits.Load())
}

// An edit leaves the streams it does not name as they are.
func TestManagerReplaceTouchesOnlyWhatChanged(t *testing.T) {
	r := newMrig(t)
	a, b, c, d := named("a"), named("b"), named("c"), named("d")
	assert.Equal(t, outcomes("a", "created", "b", "created", "c", "created"), r.replace(c, a, b))
	r.waitRunning("a", "b", "c")
	hubA, ok := r.m.Hub(OutRTMP, "a")
	require.True(t, ok)
	hubB, ok := r.m.Hub(OutRTMP, "b")
	require.True(t, ok)
	tp := tap(t, hubA, "tap")
	tp.more(t, 10)
	supA, supB, supC := r.sup("a"), r.sup("b"), r.sup("c")

	b.Outputs.RTSP = false
	assert.Equal(t, outcomes("a", "unchanged", "b", "outputs", "c", "deleted", "d", "created"), r.replace(d, b, a))
	assert.Same(t, supA, r.sup("a"))
	assert.Same(t, supB, r.sup("b"))
	for _, name := range []string{"a", "b"} {
		assert.Equal(t, uint64(1), r.status(name).Generation, name)
		assert.Zero(t, r.status(name).Rebuilds, name)
	}
	again, _ := r.m.Hub(OutRTMP, "a")
	assert.Same(t, hubA, again)
	again, _ = r.m.Hub(OutRTMP, "b")
	assert.Same(t, hubB, again)
	tp.more(t, 10)
	assert.Zero(t, tp.lost.Load())
	assert.False(t, tp.closed())
	select {
	case <-supC.Done():
	case <-time.After(waitFor):
		require.FailNow(t, "the deleted stream's supervisor is still running")
	}
	r.waitRunning("d")
	names := make([]string, 0, 3)
	for _, sp := range r.m.List() {
		names = append(names, sp.Name)
	}
	assert.Equal(t, []string{"a", "b", "d"}, names)
}

// encodedSpec is a stream that needs the transcoder.
func encodedSpec(name string) spec.Stream {
	sp := ladderSpec("720")
	sp.Name = name
	return sp
}

// One bad stream refuses the whole edit, and every error is reported.
func TestManagerReplaceAllOrNothing(t *testing.T) {
	r := newMrig(t, func(env *Env) {
		env.Reserved = []Claim{{UDP: true, Host: "0.0.0.0", Port: 19000, Owner: "server"}}
	})
	a := named("a")
	r.replace(a)
	r.waitRunning("a")

	edited := a
	edited.Layout = []string{"video"}
	err := r.refuse(edited, named("good"), encodedSpec("bad1"), withInputs(named("bad2"), "udp://:19000"))
	assert.ErrorIs(t, err, ErrInvalid)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `stream "bad1": check: transcode:`)
	assert.Contains(t, err.Error(), `stream "bad2": listen address udp :19000 conflicts with udp :19000 of the server`)
	assert.NotContains(t, err.Error(), "good")
	// The reasons are joined: a caller reads them one by one.
	var list interface{ Unwrap() []error }
	require.ErrorAs(t, err, &list)
	require.Len(t, list.Unwrap(), 2)
	assert.ErrorIs(t, list.Unwrap()[0], ErrInvalid)
	assert.NotErrorIs(t, list.Unwrap()[0], ErrConflict)
	assert.ErrorIs(t, list.Unwrap()[1], ErrConflict)
	_, ok := r.m.Get("good")
	assert.False(t, ok)
	got, _ := r.m.Get("a")
	assert.Equal(t, a, got)
	assert.Zero(t, r.status("a").Rebuilds)
	assert.Equal(t, 1, len(r.m.List()))

	// One kind alone.
	err = r.refuse(a, encodedSpec("bad1"))
	assert.ErrorIs(t, err, ErrInvalid)
	assert.NotErrorIs(t, err, ErrConflict)
	// A bad stream that is disabled is not judged.
	off := encodedSpec("bad1")
	off.Disabled = true
	assert.Equal(t, outcomes("a", "unchanged", "bad1", "created"), r.replace(a, off))
	assert.Equal(t, "stopped", r.status("bad1").State)
	off.Disabled = false
	assert.ErrorIs(t, r.refuse(a, off), ErrInvalid)
}

// A commit that fails changes nothing.
func TestManagerCommitFails(t *testing.T) {
	r := newMrig(t)
	cam := camSpec()
	r.replace(cam)
	r.waitRunning("cam")
	sup := r.sup("cam")

	errDisk := errors.New("disk full")
	edited := cam
	edited.Layout = []string{"video"}
	calls := 0
	out, err := r.m.Replace(context.Background(), []spec.Stream{edited, named("new")}, func() error {
		calls++
		return errDisk
	})
	assert.Same(t, errDisk, err)
	assert.Nil(t, out)
	assert.Equal(t, 1, calls)
	got, _ := r.m.Get("cam")
	assert.Equal(t, cam, got)
	_, ok := r.m.Get("new")
	assert.False(t, ok)
	time.Sleep(50 * time.Millisecond)
	assert.Same(t, sup, r.sup("cam"))
	assert.Equal(t, uint64(1), r.status("cam").Generation)
	assert.Zero(t, r.status("cam").Rebuilds)
	assert.Equal(t, 1, r.tails(), "a supervisor was created for a refused edit")
}

// Once commit has succeeded the edit is in force and Replace says so,
// whatever happens to its context.
func TestManagerReplaceCtxEndsAfterCommit(t *testing.T) {
	r := newMrig(t)
	cam := camSpec()
	r.replace(cam)
	r.waitRunning("cam")
	sup := r.sup("cam")

	edit := func(ctx context.Context, sp spec.Stream, commit func() error, gen uint64) {
		t.Helper()
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		r.sc.set(func(s *script) { s.closeGate = gate })
		type result struct {
			out []Outcome
			err error
		}
		res := make(chan result, 1)
		go func() {
			out, err := r.m.Replace(ctx, []spec.Stream{sp}, commit)
			res <- result{out, err}
		}()
		select {
		case got := <-res:
			require.NoError(t, got.err)
			assert.Equal(t, outcomes("cam", "rebuild"), got.out)
		case <-time.After(time.Second):
			require.FailNow(t, "Replace waited for a rebuild its context had given up on")
		}
		got, _ := r.m.Get("cam")
		assert.Equal(t, sp, got, "the edit is not in force")
		assert.Less(t, sup.View().Gen, gen, "the teardown was not held")
		release()
		waitGen(t, sup, gen)
		assert.Equal(t, sp.Layout, sup.snapshot().sp.Layout)
		assert.Same(t, sup, r.sup("cam"))
	}

	// The client hangs up once the edit is committed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next := cam
	next.Layout = []string{"video"}
	edit(ctx, next, func() error {
		r.commits.Add(1)
		cancel()
		return nil
	}, 2)
	assert.Equal(t, int64(2), r.commits.Load())

	// It had hung up before the call: Replace does not look at ctx before
	// the wait.
	next.Layout = []string{"audio"}
	edit(ctx, next, r.commit, 3)
	assert.Equal(t, int64(3), r.commits.Load())
}

// The listen claims of the streams and the server (R-D11).
func TestManagerListenClaim(t *testing.T) {
	// Numbers only: the scripted Open binds nothing.
	const (
		portHTTP, portAPI, portRTMP, portRTSP, portSRT = 18080, 18081, 11935, 18554, 19000
		p, q, p2                                       = 15000, 15001, 15002
	)
	r := newMrig(t, func(env *Env) {
		env.RTMPPlay, env.RTMPApp = "127.0.0.1:"+strconv.Itoa(portRTMP), "live"
		env.Reserved = []Claim{
			{Host: "127.0.0.1", Port: portHTTP, Owner: "server"},
			{Host: "127.0.0.1", Port: portAPI, Owner: "server"},
			{Host: "127.0.0.1", Port: portRTMP, RTMP: true, Owner: "server"},
			{Host: "127.0.0.1", Port: portRTSP, Owner: "server"},
			{Host: "127.0.0.1", Port: portSRT, UDP: true, Owner: "server"},
		}
	})
	rtmpIn := func(host string, port int, path string) string {
		return fmt.Sprintf("rtmp://%s:%d/%s?mode=listen", host, port, path)
	}
	udp := func(host string, port int) string { return fmt.Sprintf("udp://%s:%d", host, port) }
	aRTMP, aUDP := rtmpIn("127.0.0.1", portRTMP, "in/"+vKey), udp("", p)
	a := withInputs(named("a"), aRTMP, aUDP)
	r.replace(a)
	r.waitRunning("a")
	b, c := named("b"), named("c")

	refused := []struct {
		name   string
		set    []spec.Stream
		holds  []string // parts of the error: the address and both owners
		twoErr bool
	}{
		{
			"a's RTMP URL",
			[]spec.Stream{a, withInputs(b, aRTMP)},
			[]string{`stream "b"`, "tcp 127.0.0.1:11935", `stream "a"`},
			false,
		},
		{
			"the wildcard of a's RTMP port",
			[]spec.Stream{a, withInputs(b, rtmpIn("", portRTMP, "in/other"))},
			[]string{`stream "b"`, "tcp :11935", "tcp 127.0.0.1:11935", "the server"},
			false,
		},
		{
			"a's UDP port on a host",
			[]spec.Stream{a, withInputs(b, udp("127.0.0.1", p))},
			[]string{`stream "b"`, "udp 127.0.0.1:15000", "udp :15000", `stream "a"`},
			false,
		},
		{
			"SRT on a's UDP port",
			[]spec.Stream{a, withInputs(b, fmt.Sprintf("srt://127.0.0.1:%d?mode=listener", p))},
			[]string{`stream "b"`, "udp 127.0.0.1:15000", `stream "a"`},
			false,
		},
		{
			"the SRT server's port",
			[]spec.Stream{a, withInputs(b, udp("", portSRT))},
			[]string{`stream "b"`, "udp :19000", "udp 127.0.0.1:19000", "the server"},
			false,
		},
		{
			"the HTTP port",
			[]spec.Stream{a, withInputs(b, rtmpIn("127.0.0.1", portHTTP, "x/y"))},
			[]string{`stream "b"`, "tcp 127.0.0.1:18080", "the server"},
			false,
		},
		{
			"the API port",
			[]spec.Stream{a, withInputs(b, rtmpIn("127.0.0.1", portAPI, "x/y"))},
			[]string{`stream "b"`, "tcp 127.0.0.1:18081", "the server"},
			false,
		},
		{
			"the RTSP port",
			[]spec.Stream{a, withInputs(b, rtmpIn("127.0.0.1", portRTSP, "x/y"))},
			[]string{`stream "b"`, "tcp 127.0.0.1:18554", "the server"},
			false,
		},
		{
			"inside one spec",
			[]spec.Stream{a, withInputs(b, udp("", q), fmt.Sprintf("srt://:%d?mode=listener", q))},
			[]string{`stream "b": listen address udp :15001 conflicts with udp :15001 of stream "b"`},
			false,
		},
		{
			"two new streams",
			[]spec.Stream{a, withInputs(b, udp("", q)), withInputs(c, udp("", q))},
			[]string{`stream "c"`, "udp :15001", `stream "b"`},
			false,
		},
		{
			"a changed onto the server",
			[]spec.Stream{withInputs(a, aRTMP, udp("", portSRT))},
			[]string{`stream "a"`, "udp :19000", "the server"},
			false,
		},
	}
	for _, row := range refused {
		err := r.refuse(row.set...)
		assert.ErrorIs(t, err, ErrConflict, row.name)
		assert.NotErrorIs(t, err, ErrInvalid, row.name)
		for _, part := range row.holds {
			assert.Contains(t, err.Error(), part, row.name)
		}
		assert.NotContains(t, err.Error(), "key", row.name)
		assert.NotContains(t, err.Error(), "in/", row.name)
		assert.Equal(t, uint64(1), r.status("a").Generation, row.name)
		assert.Zero(t, r.status("a").Rebuilds, row.name)
		assert.Len(t, r.m.List(), 1, row.name)
	}
	// The play app on the play address is Check's to refuse.
	err := r.refuse(a, withInputs(b, rtmpIn("127.0.0.1", portRTMP, "live/x")))
	assert.ErrorIs(t, err, ErrInvalid)

	accepted := []struct {
		name string
		set  []spec.Stream
		want []Outcome
	}{
		{
			"the shared listener, another path",
			[]spec.Stream{a, withInputs(b, rtmpIn("127.0.0.1", portRTMP, "in/other"))},
			outcomes("a", "unchanged", "b", "created"),
		},
		{
			"a free UDP port",
			[]spec.Stream{a, withInputs(b, udp("", q))},
			outcomes("a", "unchanged", "b", "created"),
		},
		{
			"one multicast group twice",
			[]spec.Stream{a, withInputs(b, udp("239.1.1.1", p2)), withInputs(c, udp("239.1.1.1", p2))},
			outcomes("a", "unchanged", "b", "created", "c", "created"),
		},
		{"a keeping its inputs", []spec.Stream{outputEdit(a, OutRTSP, false)}, outcomes("a", "outputs")},
		{"a edited in place with its inputs", []spec.Stream{withInputs(a, aUDP, aRTMP)}, outcomes("a", "live")},
		{"a deleted, b taking its URLs", []spec.Stream{withInputs(b, aRTMP, aUDP)}, outcomes("a", "deleted", "b", "created")},
	}
	for _, row := range accepted {
		r.replace(a) // back to a alone
		require.Len(t, r.m.List(), 1, row.name)
		assert.Equal(t, row.want, r.replace(row.set...), row.name)
	}

	// A disabled stream holds nothing, and is judged when it is enabled.
	r.replace(a)
	off := withInputs(b, aRTMP, aUDP)
	off.Disabled = true
	assert.Equal(t, outcomes("a", "unchanged", "b", "created"), r.replace(a, off))
	assert.Equal(t, "stopped", r.status("b").State)
	off.Disabled = false
	err = r.refuse(a, off)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `stream "a"`)
	// And what a disabled stream names is free for the others.
	off.Disabled = true
	assert.Equal(t, outcomes("a", "unchanged", "b", "unchanged", "c", "created"),
		r.replace(a, off, withInputs(c, udp("", q))))
}

// A stream may not read itself through copy:// inputs.
func TestManagerCopyCycle(t *testing.T) {
	r := newMrig(t)
	a := withInputs(named("a"), "copy://b")
	assert.Equal(t, outcomes("a", "created"), r.replace(a), "a copy of an unknown stream waits for it")
	err := r.refuse(a, withInputs(named("b"), "copy://a"))
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `stream "b": copy cycle b -> a -> b`)
	// Both new: each is on the cycle.
	r.replace()
	err = r.refuse(a, withInputs(named("b"), "http://source.test/b.ts", "copy://a_720"))
	assert.Contains(t, err.Error(), "a -> b -> a")
	assert.Contains(t, err.Error(), "b -> a -> b")

	err = r.refuse(withInputs(named("a"), "copy://a_720"))
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "copy cycle a -> a")

	// A disabled stream is no edge.
	b := withInputs(named("b"), "copy://a")
	b.Disabled = true
	assert.Equal(t, outcomes("a", "created", "b", "created"), r.replace(a, b))
	b.Disabled = false
	assert.ErrorIs(t, r.refuse(a, b), ErrConflict)

	// A chain is no cycle, and a copy reads the stream it names.
	r.replace()
	src := named("src")
	assert.Equal(t, outcomes("mid", "created", "src", "created", "top", "created"),
		r.replace(withInputs(named("top"), "copy://mid"), withInputs(named("mid"), "copy://src"), src))
	r.waitRunning("src")
	h, ok := r.m.CopyHub("src")
	require.True(t, ok)
	assert.Same(t, r.sup("src").View().Source, h)
}

// The output servers find a served name only on the outputs that are on.
func TestManagerLookups(t *testing.T) {
	r := newMrig(t, func(env *Env) { fakeChild(t, env) })
	cam := ladderSpec("720", "360")
	cam.Outputs.RTSP = false
	r.replace(cam)
	r.waitRunning("cam")
	v := r.sup("cam").View()

	for _, name := range []string{"cam", "cam_720", "cam_360"} {
		for _, out := range []Output{OutHTTPTS, OutRTMP, OutSRT} {
			h, ok := r.m.Hub(out, name)
			assert.True(t, ok, "%s %s", out, name)
			assert.Same(t, v.Single[name], h, "%s %s", out, name)
		}
		_, ok := r.m.Hub(OutRTSP, name)
		assert.False(t, ok, "rtsp is off: %s", name)
		for _, out := range []Output{OutHLS, OutDASH} {
			p, ok := r.m.Packager(out, name)
			assert.True(t, ok, "%s %s", out, name)
			assert.Same(t, v.Packs[name], p, "%s %s", out, name)
			_, ok = r.m.Hub(out, name)
			assert.False(t, ok, "Hub does not serve %s", out)
		}
		_, ok = r.m.Packager(OutRTMP, name)
		assert.False(t, ok, "Packager does not serve rtmp")
	}
	for _, name := range []string{"cam_x", "nope", "", "_", "cam_", "_720", "nope_720"} {
		_, ok := r.m.Hub(OutRTMP, name)
		assert.False(t, ok, "Hub %q", name)
		_, ok = r.m.Packager(OutHLS, name)
		assert.False(t, ok, "Packager %q", name)
		_, ok = r.m.CopyHub(name)
		assert.False(t, ok, "CopyHub %q", name)
	}
	_, _, ok := r.m.Shot("cam")
	assert.False(t, ok)
	_, _, ok = r.m.Shot("nope")
	assert.False(t, ok)

	// The ABR packager is the one of the stream's name.
	abr, _ := r.m.Packager(OutHLS, "cam")
	rung, _ := r.m.Packager(OutHLS, "cam_720")
	assert.NotSame(t, abr, rung)

	h, ok := r.m.CopyHub("cam")
	require.True(t, ok)
	assert.Same(t, v.Source, h)
	h, ok = r.m.CopyHub("cam_360")
	require.True(t, ok)
	assert.Same(t, v.Single["cam_360"], h)
	assert.NotSame(t, v.Source, h)

	// HLS off, DASH on: the same packagers, found for DASH only.
	cam.Outputs.HLS = false
	assert.Equal(t, outcomes("cam", "outputs"), r.replace(cam))
	_, ok = r.m.Packager(OutHLS, "cam")
	assert.False(t, ok)
	p, ok := r.m.Packager(OutDASH, "cam")
	assert.True(t, ok)
	assert.Same(t, abr, p)
	cam.Outputs.DASH = false
	r.replace(cam)
	_, ok = r.m.Packager(OutDASH, "cam")
	assert.False(t, ok)
}

// Load is lenient: what cannot run is kept, stopped, and does not block
// later edits (R-D4).
func TestManagerLoad(t *testing.T) {
	r := &mrig{t: t, sc: newScript(t)}
	r.m = NewManager(served(Env{
		Open:     r.sc.Open,
		Reserved: []Claim{{UDP: true, Host: "127.0.0.1", Port: 19000}},
	}))
	r1, r2, y := named("r1"), named("r2"), withInputs(named("y"), "udp://:15000")
	z, x := withInputs(named("z"), "http://source.test/z.ts", "udp://:15000"), withInputs(named("x"), "udp://:19000")
	dis := named("dis")
	dis.Disabled = true
	bad := encodedSpec("bad")
	set := []spec.Stream{z, r2, bad, x, dis, y, r1}

	// Before Run: the supervisors start at once.
	left, err := r.m.Load(set)
	require.NoError(t, err)
	require.Len(t, left, 3)
	assert.Equal(t, "bad", left[0].Name)
	assert.ErrorIs(t, left[0].Err, ErrInvalid)
	assert.Contains(t, left[0].Err.Error(), `stream "bad": check: transcode:`)
	assert.Equal(t, "x", left[1].Name)
	assert.ErrorIs(t, left[1].Err, ErrConflict)
	assert.Contains(t, left[1].Err.Error(), `stream "x": listen address udp :19000 conflicts with udp 127.0.0.1:19000 of the server`)
	assert.Equal(t, "z", left[2].Name)
	assert.ErrorIs(t, left[2].Err, ErrConflict)
	assert.Contains(t, left[2].Err.Error(), `stream "z": listen address udp :15000 conflicts with udp :15000 of stream "y"`)
	assert.Equal(t, left, r.m.Stopped())
	_, err = r.m.Load(set)
	assert.ErrorContains(t, err, "not empty", "a second Load is refused")
	r.run()

	r.waitRunning("r1", "r2", "y")
	stopped := func(name, reason string) {
		t.Helper()
		st := r.status(name)
		assert.Equal(t, "stopped", st.State, name)
		assert.Contains(t, st.LastError, reason, name)
		if reason != "" {
			require.NotNil(t, st.LastErrorAt, name)
			assert.WithinDuration(t, time.Now(), *st.LastErrorAt, time.Minute, name)
		} else {
			assert.Empty(t, st.LastError, name)
			assert.Nil(t, st.LastErrorAt, name)
		}
		assert.Nil(t, r.sup(name), name)
	}
	check := func() {
		t.Helper()
		stopped("bad", "check: transcode:")
		stopped("x", "udp :19000 conflicts with udp 127.0.0.1:19000 of the server")
		stopped("dis", "")
	}
	check()
	stopped("z", `udp :15000 conflicts with udp :15000 of stream "y"`)
	assert.Equal(t, []spec.Stream{bad, dis, r1, r2, x, y, z}, r.m.List())
	samples := scrapeManager(t, r.m)
	assert.True(t, metricFound(samples, "openstreamer_streams", 3, "state", "running"))
	assert.True(t, metricFound(samples, "openstreamer_streams", 4, "state", "stopped"))

	// The conflict left in the set does not block edits.
	y.Outputs.RTSP = false
	assert.Equal(t, outcomes("bad", "unchanged", "dis", "unchanged", "r1", "unchanged", "r2", "unchanged",
		"x", "unchanged", "y", "outputs", "z", "unchanged"), r.replace(bad, dis, r1, r2, x, y, z))
	check()
	stopped("z", `of stream "y"`)

	w := named("w")
	out := r.replace(bad, dis, r1, r2, w, x, y, z)
	assert.Equal(t, Outcome{Name: "w", Applied: "created"}, out[4])
	r.waitRunning("w")
	check()
	stopped("z", `of stream "y"`)

	// A stream left stopped is judged when an edit changes it.
	z2 := withInputs(z, "http://source.test/z.ts", "udp://:15001")
	out = r.replace(bad, dis, r1, r2, w, x, y, z2)
	assert.Equal(t, Outcome{Name: "z", Applied: "started"}, out[7])
	r.waitRunning("z")
	assert.Empty(t, r.status("z").LastError)
	err = r.refuse(bad, dis, r1, r2, w, withInputs(x, "udp://:19000", "http://source.test/x.ts"), y, z2)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `stream "x"`)

	supZ := r.sup("z")
	out = r.replace(bad, dis, r1, r2, w, x, z2)
	assert.Equal(t, Outcome{Name: "x", Applied: "unchanged"}, out[5])
	assert.Equal(t, Outcome{Name: "y", Applied: "deleted"}, out[6])
	assert.Equal(t, Outcome{Name: "z", Applied: "unchanged"}, out[7])
	check()
	assert.Same(t, supZ, r.sup("z"))
	assert.Equal(t, "running", r.status("z").State)

	// The end: everything stops, and nothing can be edited.
	sups := []*Supervisor{r.sup("r1"), r.sup("r2"), r.sup("w"), supZ}
	r.stop()
	for _, s := range sups {
		select {
		case <-s.Done():
		default:
			assert.Fail(t, "Run returned before a supervisor had stopped")
		}
	}
	for _, sp := range r.m.List() {
		assert.Equal(t, "stopped", r.status(sp.Name).State, sp.Name)
	}
	_, ok := r.m.Hub(OutRTMP, "r1")
	assert.False(t, ok)
	out, err = r.m.Replace(context.Background(), nil, r.commit)
	assert.ErrorIs(t, err, ErrClosed)
	assert.Nil(t, out)
	_, err = r.m.Load(nil)
	assert.ErrorIs(t, err, ErrClosed)
	assert.Len(t, r.m.List(), 7, "a closed manager was edited")
}

// Load leaves the stream that closes a copy:// cycle stopped.
func TestManagerLoadCopyCycle(t *testing.T) {
	r := &mrig{t: t, sc: newScript(t)}
	r.m = NewManager(served(Env{Open: r.sc.Open}))
	left, err := r.m.Load([]spec.Stream{withInputs(named("b"), "copy://a"), withInputs(named("a"), "copy://b")})
	r.run()
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, "b", left[0].Name)
	assert.ErrorIs(t, left[0].Err, ErrConflict)
	assert.Contains(t, left[0].Err.Error(), `stream "b": copy cycle b -> a -> b`)
	assert.NotNil(t, r.sup("a"))
	assert.Equal(t, "copy cycle b -> a -> b", r.status("b").LastError)
}

// A stream left stopped at load is tried again by every edit that leaves it
// unchanged: it starts once nothing stands in its way, and until then keeps
// a reason that is true now, without failing the edit.
func TestManagerRetriesStopped(t *testing.T) {
	r := &mrig{t: t, sc: newScript(t)}
	r.m = NewManager(served(Env{Open: r.sc.Open}))
	a, b := withInputs(named("a"), "udp://:15000"), withInputs(named("b"), "udp://:15000")
	c := withInputs(named("c"), "udp://:15000")
	left, err := r.m.Load([]spec.Stream{a, b, c})
	require.NoError(t, err)
	require.Len(t, left, 2)
	r.run()
	r.waitRunning("a")
	first := r.status("b")
	require.NotNil(t, first.LastErrorAt)
	assert.Contains(t, first.LastError, `of stream "a"`)

	// Still in the way: unchanged, the edit is accepted, the reason is new.
	time.Sleep(2 * time.Millisecond)
	assert.Equal(t, outcomes("a", "unchanged", "b", "unchanged", "c", "unchanged"), r.replace(a, b, c))
	again := r.status("b")
	assert.Equal(t, "stopped", again.State)
	assert.Contains(t, again.LastError, `of stream "a"`)
	assert.True(t, again.LastErrorAt.After(*first.LastErrorAt), "the reason was not refreshed")
	assert.Nil(t, r.sup("b"))
	require.Len(t, r.m.Stopped(), 2)

	// A touched stream comes first: d takes the port a gives up, and b and
	// c, tried after it, name d now and do not fail the edit.
	d := withInputs(named("d"), "udp://:15000")
	assert.Equal(t, outcomes("a", "deleted", "b", "unchanged", "c", "unchanged", "d", "created"), r.replace(b, c, d))
	r.waitRunning("d")
	assert.Contains(t, r.status("b").LastError, `of stream "d"`)
	assert.NotContains(t, r.status("b").LastError, `"a"`)
	st := r.m.Stopped()
	require.Len(t, st, 2)
	assert.Equal(t, "b", st[0].Name)
	assert.ErrorIs(t, st[0].Err, ErrConflict)
	assert.Contains(t, st[0].Err.Error(), `stream "b": listen address udp :15000 conflicts with udp :15000 of stream "d"`)

	// The way is free: the first of them by name starts, the other one now
	// names it.
	assert.Equal(t, outcomes("b", "started", "c", "unchanged", "d", "deleted"), r.replace(b, c))
	r.waitRunning("b")
	assert.Empty(t, r.status("b").LastError)
	assert.Nil(t, r.status("b").LastErrorAt)
	assert.Contains(t, r.status("c").LastError, `of stream "b"`)
	st = r.m.Stopped()
	require.Len(t, st, 1)
	assert.Equal(t, "c", st[0].Name)

	// The same set again: b runs on, untouched.
	sup := r.sup("b")
	assert.Equal(t, outcomes("b", "unchanged", "c", "unchanged"), r.replace(b, c))
	assert.Same(t, sup, r.sup("b"))

	// Disabling a stopped stream forgets its reason; nothing tries it.
	c.Disabled = true
	assert.Equal(t, outcomes("b", "deleted", "c", "stopped"), r.replace(c))
	assert.Empty(t, r.status("c").LastError)
	assert.Empty(t, r.m.Stopped())
	assert.Equal(t, outcomes("c", "unchanged"), r.replace(c))
	assert.Nil(t, r.sup("c"))
}

// syncBuffer is a log sink for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// socketErrors are what a stream says when it built before its predecessor
// had let go of its sockets.
var socketErrors = []string{"already registered", "address already in use", "bind:"}

// assertNoSocketErrors fails when text holds one of socketErrors.
func assertNoSocketErrors(t testing.TB, what, text string) {
	t.Helper()
	for _, bad := range socketErrors {
		assert.NotContains(t, text, bad, what)
	}
}

// listenSpec is a stream "cam" with an RTMP listen input and a UDP listen
// input on loopback ports that were free a moment ago, and the UDP port.
func listenSpec(t testing.TB) (spec.Stream, int) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, pc.Close())
	sp := camSpec()
	sp.Inputs = []spec.Input{
		{URL: "rtmp://" + addr + "/in/" + vKey + "?mode=listen"},
		{URL: "udp://127.0.0.1:" + strconv.Itoa(port)},
	}
	sp.Outputs = spec.Outputs{HTTPTS: true, RTSP: true}
	require.NoError(t, sp.Validate())
	return sp, port
}

// sampleStatus reads the status of name every 5 ms until the returned
// function is called, which returns every LastError seen.
func sampleStatus(m *Manager, name string) (stop func() []string) {
	quit, done := make(chan struct{}), make(chan struct{})
	seen := map[string]bool{}
	go func() {
		defer close(done)
		for {
			if st, ok := m.Status(name); ok && st.LastError != "" {
				seen[st.LastError] = true
			}
			select {
			case <-quit:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return func() []string {
		close(quit)
		<-done
		out := make([]string, 0, len(seen))
		for e := range seen {
			out = append(out, e)
		}
		return out
	}
}

// sendFixture sends the TS fixture to a local UDP port, again and again,
// until the returned function is called.
func sendFixture(t testing.TB, port int) (stop func()) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	conn, err := new(net.Dialer).DialContext(context.Background(), "udp", "127.0.0.1:"+strconv.Itoa(port))
	require.NoError(t, err)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		const datagram = 7 * 188
		for {
			for off := 0; off < len(data); off += datagram {
				// Nobody listens between two supervisors: the write fails,
				// and the next one is tried.
				_, _ = conn.Write(data[off:min(off+datagram, len(data))])
				select {
				case <-quit:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}
	}()
	return sync.OnceFunc(func() {
		close(quit)
		<-done
	})
}

// heldOpen is an Env.Open over ingest.Open: the sockets are real, and its
// readers let go of them late. The readers opened while gate is set have a
// Close that blocks until the gate is closed, which holds their pipeline's
// Run and so its teardown; the others have a Close that takes delay.
type heldOpen struct {
	opens atomic.Int64
	live  atomic.Int64 // readers that hold their input
	gate  atomic.Pointer[chan struct{}]
	delay time.Duration
}

type heldReader struct {
	ingest.Reader
	h     *heldOpen
	gate  chan struct{} // nil: delay
	delay time.Duration
	once  sync.Once
}

func (r *heldReader) Close() error {
	if r.gate != nil {
		<-r.gate
	} else {
		time.Sleep(r.delay)
	}
	err := r.Reader.Close()
	r.once.Do(func() { r.h.live.Add(-1) })
	return err
}

func (h *heldOpen) Open(ctx context.Context, url string) (ingest.Reader, error) {
	h.opens.Add(1)
	r, err := ingest.Open(ctx, url, ingest.Env{})
	if err != nil {
		return nil, err
	}
	h.live.Add(1)
	if g := h.gate.Load(); g != nil {
		return &heldReader{Reader: r, h: h, gate: *g}, nil
	}
	return &heldReader{Reader: r, h: h, delay: h.delay}, nil
}

// The chain of a name's supervisors holds through one that was cancelled
// while it waited (R-D5): the third builds only when the first has let go.
func TestManagerChainSurvivesCancelledWaiter(t *testing.T) {
	sp, port := listenSpec(t)
	var logs syncBuffer
	logger, err := logging.New(&logs, "debug", "text")
	require.NoError(t, err)
	open := &heldOpen{}
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	open.gate.Store(&gate)
	r := &mrig{t: t}
	r.m = NewManager(served(Env{Open: open.Open, Logger: logger}))
	r.run()
	stopSend := sendFixture(t, port)
	defer stopSend()
	stopSample := sampleStatus(r.m, "cam")

	assert.Equal(t, outcomes("cam", "created"), r.replace(sp))
	r.waitRunning("cam")
	s1 := r.sup("cam")
	open.gate.Store(nil) // only the first supervisor's readers are held

	assert.Equal(t, outcomes("cam", "deleted"), r.replace())
	time.Sleep(50 * time.Millisecond)
	opened := open.opens.Load()
	assert.Equal(t, outcomes("cam", "created"), r.replace(sp))
	s2 := r.sup("cam")
	assert.Equal(t, outcomes("cam", "deleted"), r.replace()) // cancelled while it waits
	assert.Equal(t, outcomes("cam", "created"), r.replace(sp))
	s3 := r.sup("cam")
	require.NotSame(t, s1, s3)
	require.NotSame(t, s2, s3)

	// While the first supervisor's teardown is held, the third waits.
	for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		st := r.status("cam")
		require.Equal(t, "starting", st.State)
		require.Empty(t, st.LastError)
		require.Zero(t, st.Generation)
		require.Equal(t, opened, open.opens.Load(), "the third supervisor opened an input while the first held its sockets")
	}
	for i, s := range []*Supervisor{s1, s2} {
		select {
		case <-s.Done():
			require.FailNow(t, "done early", "supervisor %d", i+1)
		default:
		}
	}
	assert.Equal(t, 1, r.tails())
	// The scrape has one supervisor per name, whatever is still tearing down.
	samples := scrapeManager(t, r.m)
	assert.True(t, metricFound(samples, "openstreamer_streams", 1, "state", "starting"))
	assert.True(t, metricFound(samples, "openstreamer_stream_up", 0, "stream", "cam"))

	release()
	r.waitRunning("cam")
	assert.Same(t, s3, r.sup("cam"))
	assert.Greater(t, open.opens.Load(), opened)
	<-s1.Done()
	<-s2.Done()
	assert.Equal(t, 1, r.tails())

	// The tail goes once the name has no stream, its last supervisor has
	// stopped, and one more edit has run.
	assert.Equal(t, outcomes("cam", "deleted"), r.replace())
	select {
	case <-s3.Done():
	case <-time.After(waitFor):
		require.FailNow(t, "the third supervisor did not stop")
	}
	assert.Equal(t, 1, r.tails())
	assert.Empty(t, r.replace())
	assert.Zero(t, r.tails())

	for _, e := range stopSample() {
		assertNoSocketErrors(t, "a status", e)
	}
	stopSend()
	r.stop()
	assertNoSocketErrors(t, "the log", logs.String())
	assert.NotContains(t, logs.String(), vKey)
}

// The ledger of leases sees an overlap, and a reader's Close releases its
// lease only after the delay.
func TestLeases(t *testing.T) {
	l := newLeases(t, 10*time.Millisecond)
	udpURL, port := l.udp()
	idleURL := l.idle("rtmp://lease/in/" + vKey + "?mode=listen")
	stopSend := sendFixture(t, port)
	defer stopSend()

	r, err := l.Open(context.Background(), udpURL)
	require.NoError(t, err)
	f, err := r.Read()
	require.NoError(t, err)
	assert.NotEmpty(t, f.Data)
	idle, err := l.Open(context.Background(), idleURL)
	require.NoError(t, err)
	assert.Equal(t, int64(2), l.Live())

	_, err = l.Open(context.Background(), udpURL)
	require.ErrorIs(t, err, errOverlap)
	assert.EqualError(t, err, "lease: overlap")
	_, err = l.Open(context.Background(), idleURL)
	require.ErrorIs(t, err, errOverlap)
	require.Len(t, l.Overlaps(), 2)
	assert.Equal(t, udpURL+": open 3 while open 1 holds it", l.Overlaps()[0])
	assert.NotContains(t, l.Overlaps()[1], vKey)

	read := make(chan error, 1)
	go func() {
		_, err := idle.Read()
		read <- err
	}()
	begin := time.Now()
	require.NoError(t, r.Close())
	require.NoError(t, idle.Close())
	assert.GreaterOrEqual(t, time.Since(begin), 20*time.Millisecond, "each Close waits the delay")
	assert.ErrorIs(t, <-read, io.EOF)
	assert.Zero(t, l.Live())

	// Released: the next holder opens, and reads.
	r, err = l.Open(context.Background(), udpURL)
	require.NoError(t, err)
	_, err = r.Read()
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Len(t, l.Overlaps(), 2)
	assert.Equal(t, int64(5), l.Opens())

	// An Open waiting for its first PMT ends with its context, and lets go.
	quiet, _ := l.udp()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = l.Open(ctx, quiet)
	require.ErrorIs(t, err, errLeaseAborted)
	assert.Zero(t, l.Live())
}

// Concurrent edits of one stream neither deadlock nor let two generations of
// it hold an input at once: its inputs are leases of sockets the test holds,
// so no bind happens and no foreign process can take a port, and the ledger
// sees any overlap. The UDP input gets data, so it has a reader, which lets
// go of its lease 10 ms late. Not parallel: the goroutine count is this
// test's.
func TestManagerConcurrentReplaces(t *testing.T) {
	l := newLeases(t, 10*time.Millisecond)
	udpURL, port := l.udp()
	sp := camSpec()
	sp.Inputs = []spec.Input{{URL: l.idle("rtmp://lease/in/" + vKey + "?mode=listen")}, {URL: udpURL}}
	sp.Outputs = spec.Outputs{HTTPTS: true, RTSP: true}
	require.NoError(t, sp.Validate())
	var logs syncBuffer
	logger, err := logging.New(&logs, "debug", "text")
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	m := NewManager(served(Env{Open: l.Open, Logger: logger}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSend := sendFixture(t, port)
	defer stopSend()
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		m.Run(ctx)
	}()
	_, err = m.Replace(ctx, []spec.Stream{sp}, nil)
	require.NoError(t, err)
	stopSample := sampleStatus(m, "cam")
	// held waits until the stream has a reader of the UDP input, which lets
	// go of its lease late: the next edit meets a slow teardown. The idle
	// input holds one lease from the start; the UDP input's reader is the
	// second.
	held := func() {
		for end := time.Now().Add(waitFor); time.Now().Before(end); time.Sleep(time.Millisecond) {
			if l.Live() > 1 {
				return
			}
		}
		assert.Fail(t, "no input was opened")
	}
	held()

	var wg sync.WaitGroup
	var deleted atomic.Bool // the deleter has finished
	for i := range 20 {
		wg.Go(func() {
			for round := 0; round < 2 || !deleted.Load(); round++ {
				set := m.List()
				switch at := slices.IndexFunc(set, func(s spec.Stream) bool { return s.Name == "cam" }); {
				case at < 0:
					set = append(set, sp)
				case i%2 == 0: // an output edit
					set[at].Outputs.RTSP = !set[at].Outputs.RTSP
				case len(set[at].Layout) == 0: // a rebuild edit
					set[at].Layout = []string{"video"}
				default:
					set[at].Layout = nil
				}
				_, err := m.Replace(ctx, set, nil)
				assert.NoError(t, err)
				time.Sleep(time.Duration(i) * time.Millisecond)
			}
		})
	}
	wg.Go(func() {
		defer deleted.Store(true)
		for range 3 {
			_, err := m.Replace(ctx, nil, nil)
			assert.NoError(t, err)
			_, err = m.Replace(ctx, []spec.Stream{sp}, nil)
			assert.NoError(t, err)
			held()
		}
	})
	last := sp
	last.Layout = []string{"video"}
	last.Outputs.SRT = true
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		wg.Wait()
		_, err := m.Replace(ctx, []spec.Stream{last}, nil)
		assert.NoError(t, err)
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		buf := make([]byte, 1<<20)
		require.FailNow(t, "deadlock", "%s", buf[:runtime.Stack(buf, true)])
	}

	assert.Equal(t, []spec.Stream{last}, m.List())
	sup := (*m.streams.Load())["cam"].sup
	require.NotNil(t, sup)
	require.Eventually(t, func() bool { return sup.View().Gen > 0 }, waitFor, time.Millisecond)
	assert.Equal(t, last.Outputs, sup.View().Outputs)
	assert.Equal(t, last.Layout, sup.snapshot().sp.Layout)
	require.Eventually(t, func() bool { return sup.Status().State == "running" }, waitFor, time.Millisecond,
		"last error %q", sup.Status().LastError)
	samples := scrapeManager(t, m)
	assert.True(t, metricFound(samples, "openstreamer_stream_up", 1, "stream", "cam"))
	assert.Greater(t, l.Opens(), int64(2))

	for _, e := range stopSample() {
		assertNoSocketErrors(t, "a status", e)
	}
	cancel()
	select {
	case <-ran:
	case <-time.After(waitFor):
		require.FailNow(t, "Run did not return")
	}
	stopSend()
	assertNoSocketErrors(t, "the log", logs.String())
	assert.NotContains(t, logs.String(), vKey)
	assert.Empty(t, l.Overlaps(), "two generations of cam never hold an input at once")

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines: %d before, %d after\n%s", before, after, buf[:runtime.Stack(buf, true)])
	}
}

// inUse is what a status says when an input could not bind its port.
const inUse = "address already in use"

// A rebuild binds its inputs' real ports again: ten sequential rebuilds of a
// stream with an RTMP and a UDP listen input never meet their own previous
// generation on a port. A failed bind does not fail the build (the pipeline
// reports the input and retries it), so its sign is the status text. When it
// shows, the stream is deleted and waited for, so that nothing of ours holds
// the port, and the test binds it: still taken, the holder is foreign and the
// test starts over on new ports; free, our own previous generation held it
// when the next one bound, which is an overlap.
func TestManagerRebindsAfterRebuild(t *testing.T) {
	var logs syncBuffer
	logger, err := logging.New(&logs, "debug", "text")
	require.NoError(t, err)
	open := &heldOpen{delay: 10 * time.Millisecond}
	r := &mrig{t: t}
	r.m = NewManager(served(Env{Open: open.Open, Logger: logger}))
	r.run()

	// taken binds the ports of sp that lastErr names (both when it names
	// neither) and reports whether one of them is still taken.
	taken := func(sp spec.Stream, lastErr string) bool {
		networks := []string{"tcp", "udp"} // the RTMP input, then the UDP one
		var hosts [2]string
		var named [2]bool
		for i := range networks {
			u, err := url.Parse(sp.Inputs[i].URL)
			require.NoError(t, err)
			hosts[i], named[i] = u.Host, strings.Contains(lastErr, ":"+u.Port())
		}
		busy := false
		for i, network := range networks {
			if named[i] || named == [2]bool{} {
				busy = bound(t, network, hosts[i]) || busy
			}
		}
		return busy
	}

	// rebuilds runs the ten edits on sp; it returns the status text of a
	// failed bind, "" when there was none.
	rebuilds := func(sp spec.Stream) string {
		assert.Equal(t, outcomes("cam", "created"), r.replace(sp))
		r.waitRunning("cam")
		for i := range 10 {
			if i%2 == 0 {
				sp.Layout = []string{"audio", "video"}
			} else {
				sp.Layout = nil
			}
			gen := r.status("cam").Generation
			edit := time.Now()
			assert.Equal(t, outcomes("cam", "rebuild"), r.replace(sp), "edit %d", i)
			for {
				st := r.status("cam")
				if strings.Contains(st.LastError, inUse) {
					return st.LastError
				}
				if st.State == "running" && st.Generation > gen && time.Since(edit) >= 200*time.Millisecond {
					break
				}
				require.Less(t, time.Since(edit), waitFor, "edit %d: state %q, generation %d (last error %q)",
					i, st.State, st.Generation, st.LastError)
				time.Sleep(time.Millisecond)
			}
		}
		return ""
	}

	const restarts = 3
	for attempt := 0; ; attempt++ {
		sp, port := listenSpec(t)
		stopSend := sendFixture(t, port)
		lastErr := rebuilds(sp)
		st := r.status("cam")
		sup := r.sup("cam")
		assert.Equal(t, outcomes("cam", "deleted"), r.replace())
		select {
		case <-sup.Done():
		case <-time.After(waitFor):
			require.FailNow(t, "the supervisor did not stop")
		}
		stopSend()
		// Done means every reader has let go: one still open is ours, and
		// whatever holds the port then proves nothing about a foreigner.
		require.Zero(t, open.live.Load(), "an input reader outlived its supervisor (last error %q)", lastErr)
		if lastErr == "" {
			break
		}
		if !taken(sp, lastErr) {
			require.FailNow(t, "overlap: the port was free once our stream had stopped, so its previous generation held it when the next one bound",
				"status %+v\nlog:\n%s", st, logs.String())
		}
		require.Less(t, attempt, restarts, "a foreign process took our ports %d times: %s", attempt+1, lastErr)
		t.Logf("attempt %d: a foreign process holds a port (%s): starting over on new ports", attempt+1, lastErr)
	}
	assert.NotContains(t, logs.String(), vKey)
}

// bound binds addr on network ("tcp" or "udp") and reports whether it is
// taken; a bind that succeeds is closed at once.
func bound(t testing.TB, network, addr string) bool {
	t.Helper()
	var lc net.ListenConfig
	if network == "udp" {
		pc, err := lc.ListenPacket(context.Background(), network, addr)
		if err != nil {
			return true
		}
		require.NoError(t, pc.Close())
		return false
	}
	ln, err := lc.Listen(context.Background(), network, addr)
	if err != nil {
		return true
	}
	require.NoError(t, ln.Close())
	return false
}

// A lookup, a status request and a scrape do not wait for an edit that is
// writing the file.
func TestManagerReadersDoNotWaitForEdits(t *testing.T) {
	r := newMrig(t)
	a, other := named("a"), named("other")
	r.replace(a, other)
	r.waitRunning("a", "other")
	hubA, _ := r.m.Hub(OutRTSP, "a")
	require.NotNil(t, hubA)

	gate, called := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	edited := outputEdit(a, OutRTSP, false)
	replaced := make(chan error, 1)
	go func() {
		_, err := r.m.Replace(context.Background(), []spec.Stream{edited, other, named("new")}, func() error {
			close(called)
			<-gate
			return nil
		})
		replaced <- err
	}()
	select {
	case <-called:
	case <-time.After(waitFor):
		require.FailNow(t, "commit was not called")
	}

	// within runs one reader, which must return in 100 ms.
	within := func(what string, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f()
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			release()
			<-done
			assert.Fail(t, "a reader waited for the edit", what)
		}
	}
	readers := func(newSet bool) {
		t.Helper()
		for _, name := range []string{"a", "other"} {
			changed := newSet && name == "a"
			within("Hub "+name, func() {
				h, ok := r.m.Hub(OutRTSP, name)
				assert.Equal(t, !changed, ok, "Hub rtsp %s", name)
				if name == "a" && !changed {
					assert.Same(t, hubA, h)
				}
				_, ok = r.m.Hub(OutRTMP, name)
				assert.True(t, ok, "Hub rtmp %s", name)
			})
			within("Packager "+name, func() {
				_, ok := r.m.Packager(OutHLS, name)
				assert.True(t, ok, "Packager %s", name)
			})
			within("CopyHub "+name, func() {
				_, ok := r.m.CopyHub(name)
				assert.True(t, ok, "CopyHub %s", name)
			})
			within("Get "+name, func() {
				got, ok := r.m.Get(name)
				assert.True(t, ok)
				assert.Equal(t, !changed, got.Outputs.RTSP, "Get %s", name)
			})
			within("Status "+name, func() {
				st, ok := r.m.Status(name)
				assert.True(t, ok)
				assert.Equal(t, "running", st.State, "Status %s", name)
				assert.Equal(t, !changed, st.Outputs.RTSP, "Status %s", name)
			})
			within("Shot "+name, func() { r.m.Shot(name) })
		}
		within("List", func() {
			want := 2
			if newSet {
				want = 3
			}
			assert.Len(t, r.m.List(), want)
		})
		within("Collect", func() {
			samples := scrapeManager(t, r.m)
			total := 0.0
			for _, m := range samples {
				if m.Name == "openstreamer_streams" {
					total += m.Value
				}
			}
			want := 2.0
			if newSet {
				want = 3
			}
			assert.Equal(t, want, total)
		})
		within("the new stream", func() {
			_, ok := r.m.Get("new")
			assert.Equal(t, newSet, ok)
		})
	}
	readers(false)
	select {
	case err := <-replaced:
		require.FailNow(t, "Replace returned while commit was blocked", "%v", err)
	default:
	}
	release()
	select {
	case err := <-replaced:
		require.NoError(t, err)
	case <-time.After(waitFor):
		require.FailNow(t, "Replace did not return")
	}
	r.waitRunning("new")
	readers(true)
}

// The scrape counts the streams by state and labels every series of a
// stream with a name of the set.
func TestManagerCollect(t *testing.T) {
	r := newMrig(t)
	a, b, c := named("a"), named("b"), named("c")
	c.Disabled = true
	r.replace(a, b, c)
	r.waitRunning("a", "b")
	samples := scrapeManager(t, r.m)
	for state, want := range map[string]float64{"running": 2, "stopped": 1, "starting": 0, "no_input": 0, "backoff": 0} {
		assert.True(t, metricFound(samples, "openstreamer_streams", want, "state", state), "state %s", state)
	}
	per := map[string]int{}
	for _, m := range samples {
		if m.Name == "openstreamer_streams" {
			assert.NotContains(t, m.Labels, "stream")
			continue
		}
		assert.Contains(t, []string{"a", "b"}, m.Labels["stream"], m.Name)
		per[m.Labels["stream"]]++
	}
	assert.Positive(t, per["a"])
	assert.Equal(t, per["a"], per["b"])

	// A name has one supervisor in the scrape, also while its predecessor
	// tears down: the same series twice would fail the whole scrape.
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	old := r.sup("a")
	r.sc.set(func(s *script) { s.closeGate = gate })
	r.replace(b, c)
	r.replace(a, b, c)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-old.Done():
		require.FailNow(t, "the predecessor's teardown was not held")
	default:
	}
	samples = scrapeManager(t, r.m)
	assert.True(t, metricFound(samples, "openstreamer_stream_up", 0, "stream", "a"))
	assert.True(t, metricFound(samples, "openstreamer_streams", 1, "state", "starting"))
	assert.True(t, metricFound(samples, "openstreamer_streams", 1, "state", "running"))
	release()
	r.sc.set(func(s *script) { s.closeGate = nil })
	r.waitRunning("a")
}

// A live input edit is judged as any other: its listen claims against the
// other streams', and its copy:// cycles.
func TestManagerLiveInputClaims(t *testing.T) {
	// Numbers only: the scripted Open binds nothing.
	const p, q, p2 = 16000, 16001, 16002
	udp := func(port int) string { return fmt.Sprintf("udp://:%d", port) }
	r := newMrig(t)
	a := withInputs(named("a"), udp(p))
	b := withInputs(named("b"), udp(q))
	c := withInputs(named("c"), "copy://a")
	r.replace(a, b, c)
	r.waitRunning("a", "b", "c")
	assert.Equal(t, 1, r.sc.opened(udp(p)))

	// Onto b's address: refused, and nothing changed.
	err := r.refuse(withInputs(a, udp(q)), b, c)
	assert.ErrorIs(t, err, ErrConflict)
	assert.NotErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), `stream "a"`)
	assert.Contains(t, err.Error(), "udp :16001")
	assert.Contains(t, err.Error(), `stream "b"`)
	got, ok := r.m.Get("a")
	require.True(t, ok)
	assert.Equal(t, a, got)
	st := r.status("a")
	assert.Equal(t, []string{udp(p)}, st.Inputs)
	assert.Equal(t, uint64(1), st.Generation)
	assert.Equal(t, 1, r.sc.opened(udp(q)), "b's address was opened for a")
	assert.Equal(t, 1, r.sc.opened(udp(p)))
	assert.Equal(t, 0, r.sc.closes(udp(p)), "a's reader was closed")

	// A second listen input beside the first: live, and the first reader
	// stays.
	a2 := withInputs(a, udp(p), udp(p2))
	assert.Equal(t, outcomes("a", "live", "b", "unchanged", "c", "unchanged"), r.replace(a2, b, c))
	require.Eventually(t, func() bool { return slices.Equal(r.status("a").Inputs, []string{udp(p), udp(p2)}) },
		waitFor, time.Millisecond)
	r.waitRunning("a")
	st = r.status("a")
	assert.Equal(t, uint64(1), st.Generation)
	assert.Equal(t, 0, st.ActiveInput)
	assert.Equal(t, 1, r.sc.opened(udp(p)), "the first reader was opened again")
	assert.Equal(t, 0, r.sc.closes(udp(p)), "the first reader was closed")

	// A copy of c, which copies a: a cycle. The claims and the cycles are
	// both ErrConflict (claims.go), whatever the input edit is.
	err = r.refuse(withInputs(a2, udp(p), "copy://c"), b, c)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), "copy cycle a -> c -> a")
	got, ok = r.m.Get("a")
	require.True(t, ok)
	assert.Equal(t, a2, got)
	assert.Equal(t, uint64(1), r.status("a").Generation)
	assert.Equal(t, 1, r.sc.opened(udp(p)))
}

// imageSpec is a one-rung ladder name, on an input of its own, with the
// watermark image logo.png.
func imageSpec(name string) spec.Stream {
	sp := ladderSpec("720")
	sp.Name = name
	sp.Inputs = []spec.Input{{URL: "http://source.test/" + name + ".ts"}}
	sp.Transcode.Watermark = &spec.Watermark{Image: "logo.png"}
	return sp
}

// AssetUsers finds the streams whose spec names the asset, with or without a
// supervisor.
func TestManagerAssetUsers(t *testing.T) {
	r := newMrig(t, func(env *Env) {
		fakeChild(t, env)
		env.AssetsDir = writeAsset(t, "logo.png")
	})
	a := imageSpec("a")
	b := ladderSpec("720")
	b.Name, b.Disabled = "b", true
	b.Transcode.Watermark = &spec.Watermark{Text: "t", Font: "f.ttf"}
	c := named("c")
	r.replace(a, b, c)
	assert.Equal(t, []string{"a"}, r.m.AssetUsers("logo.png"))
	assert.Equal(t, []string{"b"}, r.m.AssetUsers("f.ttf"))
	assert.Nil(t, r.m.AssetUsers("other.png"))
	assert.Nil(t, r.m.AssetUsers(""))
	// Both naming it, sorted.
	b2 := imageSpec("b")
	b2.Disabled = true
	r.replace(a, b2, c)
	assert.Equal(t, []string{"a", "b"}, r.m.AssetUsers("logo.png"))
	assert.Nil(t, r.m.AssetUsers("f.ttf"))
}

// countLines counts the lines of the log that are line.
func countLines(lines func() []string, line string) int {
	n := 0
	for _, l := range lines() {
		if l == line {
			n++
		}
	}
	return n
}

// RefreshAsset asks the streams that run with the asset to load it again,
// and reports each one's answer.
func TestManagerRefreshAsset(t *testing.T) {
	after := newFakeAfter()
	path := filepath.Join(t.TempDir(), "watermark.log")
	r := newMrig(t, func(env *Env) {
		fakeChild(t, env, transcodetest.EnvWatermarkLog+"="+path)
		env.AssetsDir = writeAsset(t, "logo.png")
		env.After = after.After
	})
	lines := func() []string {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	asset := filepath.Join(r.m.env.AssetsDir, "logo.png")
	d := imageSpec("d")
	d.Disabled = true
	r.replace(imageSpec("a"), imageSpec("b"), imageSpec("c"), d)
	r.waitRunning("a", "b", "c")
	configured := fmt.Sprintf("configure image:%d", len(onePixelPNG))
	require.Eventually(t, func() bool { return countLines(lines, configured) == 3 }, waitFor, time.Millisecond,
		"the children's watermark log: %q", lines())
	// c between generations: its backoff waits for the fake clock.
	failGen(t, r.sup("c"), "transcoder")
	after.next(t)
	require.Equal(t, "backoff", r.status("c").State)

	// The file replaced by other valid bytes.
	other := otherPNG(t)
	require.NoError(t, os.WriteFile(asset, other, 0o600))
	assert.Equal(t, []Refreshed{
		{Stream: "a", OK: true},
		{Stream: "b", OK: true},
		{Stream: "c", Error: "between generations: taken at the next build"},
	}, r.m.RefreshAsset(context.Background(), "logo.png"))
	set := fmt.Sprintf("set image:%d", len(other))
	require.Eventually(t, func() bool { return countLines(lines, set) == 2 }, waitFor, time.Millisecond,
		"the children's watermark log: %q", lines())
	assert.Equal(t, "backoff", r.status("c").State, "a refresh ended the backoff")
	assert.False(t, after.called(), "a refresh started another backoff")

	// The file removed: each running stream keeps its picture and says why.
	require.NoError(t, os.Remove(asset))
	got := r.m.RefreshAsset(context.Background(), "logo.png")
	require.Len(t, got, 3)
	for _, g := range got[:2] {
		assert.False(t, g.OK, g.Stream)
		assert.Contains(t, g.Error, "logo.png", g.Stream)
	}
	assert.Equal(t, Refreshed{Stream: "c", Error: "between generations: taken at the next build"}, got[2])
	assert.Equal(t, []string{"a", "b"}, []string{got[0].Stream, got[1].Stream})
	assert.Equal(t, 2, countLines(lines, set), "a failed refresh set a watermark")
	for _, name := range []string{"a", "b"} {
		assert.Equal(t, "running", r.status(name).State, name)
		assert.Equal(t, uint64(1), r.status(name).Generation, name)
	}

	// An asset nobody uses.
	assert.Nil(t, r.m.RefreshAsset(context.Background(), "other.png"))

	// A cancelled ctx: no wait.
	require.NoError(t, os.WriteFile(asset, onePixelPNG, 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	assert.Equal(t, []Refreshed{
		{Stream: "a", Error: "cancelled"},
		{Stream: "b", Error: "cancelled"},
		{Stream: "c", Error: "cancelled"},
	}, r.m.RefreshAsset(ctx, "logo.png"))
	assert.Less(t, time.Since(start), time.Second)
}

// A supervisor that has stopped reports its refresh as not done, at once.
func TestManagerRefreshAssetStopped(t *testing.T) {
	r := newMrig(t, func(env *Env) {
		fakeChild(t, env)
		env.AssetsDir = writeAsset(t, "logo.png")
	})
	r.replace(imageSpec("a"))
	r.waitRunning("a")
	sup := r.sup("a")
	r.stop()
	select {
	case <-sup.Done():
	case <-time.After(waitFor):
		require.FailNow(t, "the supervisor did not stop")
	}
	got := r.m.RefreshAsset(context.Background(), "logo.png")
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].Stream)
	assert.False(t, got[0].OK)
	assert.NotEmpty(t, got[0].Error)
	assert.NotEqual(t, "cancelled", got[0].Error)
}

// Concurrent live edits and refreshes neither deadlock nor overlap two
// readers of an input, nor rebuild the stream.
func TestManagerConcurrentLiveEdits(t *testing.T) {
	l := newLeases(t, 10*time.Millisecond)
	u1, p1 := l.udp()
	u2, p2 := l.udp()
	assets := t.TempDir()
	font, err := os.ReadFile(fixtureFont)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(assets, "f.ttf"), font, 0o600))
	env := served(Env{Open: l.Open, AssetsDir: assets})
	fakeChild(t, &env)
	sp := ladderSpec("720")
	sp.Inputs = []spec.Input{{URL: u1}}
	sp.Outputs = spec.Outputs{HTTPTS: true, RTSP: true}
	sp.Transcode.Watermark = &spec.Watermark{Text: "w", Font: "f.ttf"}
	require.NoError(t, sp.Validate())

	m := NewManager(env)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop1, stop2 := sendFixture(t, p1), sendFixture(t, p2)
	defer stop1()
	defer stop2()
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		m.Run(ctx)
	}()
	_, err = m.Replace(ctx, []spec.Stream{sp}, nil)
	require.NoError(t, err)
	sup := (*m.streams.Load())["cam"].sup
	require.NotNil(t, sup)
	waitRunning(t, sup)

	deadline := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	var edits, refreshes atomic.Int64
	for i := range 20 {
		wg.Go(func() {
			for n := 0; time.Now().Before(deadline); n++ {
				if i%4 == 3 {
					assert.Equal(t, []Refreshed{{Stream: "cam", OK: true}}, m.RefreshAsset(ctx, "f.ttf"))
					refreshes.Add(1)
					continue
				}
				set := m.List()
				if !assert.Len(t, set, 1) {
					return
				}
				c := &set[0]
				switch i % 4 {
				case 0: // the input swapped for the other
					next := u1
					if c.Inputs[0].URL == u1 {
						next = u2
					}
					c.Inputs = []spec.Input{{URL: next}}
				case 1: // a new text
					c.Transcode.Watermark = &spec.Watermark{Text: fmt.Sprintf("w%d.%d", i, n), Font: "f.ttf"}
				case 2: // an output toggled
					c.Outputs.RTSP = !c.Outputs.RTSP
				}
				out, err := m.Replace(ctx, set, nil)
				assert.NoError(t, err)
				edits.Add(1)
				for _, o := range out {
					assert.Contains(t, []string{"live", "outputs", "unchanged"}, o.Applied)
				}
				time.Sleep(time.Duration(i%3) * time.Millisecond)
			}
		})
	}
	last := cloneSpec(sp)
	last.Inputs = []spec.Input{{URL: u2}}
	last.Transcode.Watermark = &spec.Watermark{Text: "last", Font: "f.ttf"}
	last.Outputs.SRT = true
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		wg.Wait()
		_, err := m.Replace(ctx, []spec.Stream{last}, nil)
		assert.NoError(t, err)
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		buf := make([]byte, 1<<20)
		require.FailNow(t, "deadlock", "%s", buf[:runtime.Stack(buf, true)])
	}

	t.Logf("%d edits, %d refreshes", edits.Load(), refreshes.Load())
	assert.Greater(t, edits.Load(), int64(40))
	assert.Greater(t, refreshes.Load(), int64(10))
	assert.Equal(t, []spec.Stream{last}, m.List())
	assert.Same(t, sup, (*m.streams.Load())["cam"].sup)
	assert.Equal(t, spec.ChangeNone, spec.Diff(last, sup.snapshot().sp))
	assert.Equal(t, last.Outputs, sup.View().Outputs)
	require.Eventually(t, func() bool { return sup.Status().State == "running" }, waitFor, time.Millisecond,
		"last error %q", sup.Status().LastError)
	st := sup.Status()
	assert.Equal(t, []string{u2}, st.Inputs)
	assert.Equal(t, uint64(1), st.Generation, "an edit rebuilt the stream")
	assert.Equal(t, uint64(1), sup.View().Gen)
	assert.Zero(t, st.Rebuilds)
	assert.Zero(t, st.Restarts)
	assert.Greater(t, l.Opens(), int64(2))

	cancel()
	select {
	case <-ran:
	case <-time.After(waitFor):
		require.FailNow(t, "Run did not return")
	}
	assert.Empty(t, l.Overlaps(), "two readers of one input at once")
}

// The manager reports an audio language edit as "live", and one that also
// changes the layout as "rebuild".
func TestManagerAudioLanguageOutcome(t *testing.T) {
	r := newMrig(t)
	cam := named("cam")
	assert.Equal(t, outcomes("cam", "created"), r.replace(cam))
	r.waitRunning("cam")
	sup, gen := r.sup("cam"), r.status("cam").Generation

	cam.AudioLanguage = "vi"
	assert.Equal(t, outcomes("cam", "live"), r.replace(cam))
	assert.Same(t, sup, r.sup("cam"))
	assert.Equal(t, gen, r.status("cam").Generation)
	assert.Zero(t, r.status("cam").Rebuilds)
	got, ok := r.m.Get("cam")
	require.True(t, ok)
	assert.Equal(t, "vi", got.AudioLanguage)

	cam.AudioLanguage = "en"
	cam.Layout = []string{"audio", "video"}
	assert.Equal(t, outcomes("cam", "rebuild"), r.replace(cam))
	assert.Same(t, sup, r.sup("cam"))
	assert.Equal(t, gen+1, r.status("cam").Generation)
	assert.Equal(t, uint64(1), r.status("cam").Rebuilds)
}
