//go:build libav

package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/native"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// envChild makes the test binary run main, for TestMainOverFD3 and
// TestProbeFlag; envArgs holds main's arguments, split on spaces.
const (
	envChild = "OSTX_TEST_CHILD"
	envArgs  = "OSTX_TEST_ARGS"
)

func TestMain(m *testing.M) {
	if os.Getenv(envChild) == "1" {
		os.Args = append(os.Args[:1], strings.Fields(os.Getenv(envArgs))...)
		main()
		os.Exit(0)
	}
	astiav.SetLogLevel(astiav.LogLevelError)
	os.Exit(m.Run())
}

// renditionDelay is D, added by the host before frames reach the child.
const renditionDelay = 45000

// childTimeout bounds every wait on the child.
const childTimeout = 2 * time.Minute

// fixture is the source every test feeds.
const fixture = "../../testdata/fixtures/h264_aac.ts"

// source returns the fixture's frames as the host sends them to the child:
// demuxed, through a Clock, and shifted by D.
func source(t *testing.T) []media.Frame {
	t.Helper()
	data, err := os.ReadFile(fixture)
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		out = append(out, c.Push(f)...)
	}
	out = append(out, c.Flush()...)
	for i := range out {
		out[i].PTS += renditionDelay
		out[i].DTS += renditionDelay
	}
	return out
}

// socketpair returns the two ends of a Unix stream socketpair.
func socketpair(t *testing.T) (a, b *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	return os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
}

func fileConn(t *testing.T, f *os.File) *net.UnixConn {
	t.Helper()
	defer f.Close()
	c, err := net.FileConn(f)
	require.NoError(t, err)
	return c.(*net.UnixConn)
}

// host is the test's side of the protocol. It collects every message the
// child writes until the stream ends.
type host struct {
	conn *net.UnixConn
	w    *wire.Writer

	mu      sync.Mutex
	msgs    []wire.Message
	readErr error
	done    chan struct{}
}

func newHost(t *testing.T, conn *net.UnixConn) *host {
	t.Helper()
	h := &host{conn: conn, w: wire.NewWriter(conn), done: make(chan struct{})}
	t.Cleanup(func() {
		_ = conn.Close()
		<-h.done
	})
	go func() {
		defer close(h.done)
		r := wire.NewReader(conn)
		for {
			m, err := r.Read()
			h.mu.Lock()
			if err != nil {
				h.readErr = err
				h.mu.Unlock()
				return
			}
			h.msgs = append(h.msgs, m)
			h.mu.Unlock()
		}
	}()
	return h
}

// startChild runs serve in-process on one end of a socketpair and returns
// the host on the other end and the channel serve's result arrives on.
func startChild(t *testing.T, serveFn func(io.ReadWriteCloser) error) (*host, <-chan error) {
	t.Helper()
	a, b := socketpair(t)
	child, hc := fileConn(t, a), fileConn(t, b)
	errc := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		errc <- serveFn(child)
	}()
	t.Cleanup(func() {
		_ = hc.Close()
		select {
		case <-exited:
		case <-time.After(childTimeout):
			t.Error("child did not return")
		}
	})
	return newHost(t, hc), errc
}

// send writes messages until the first error, which it returns.
func (h *host) send(ms ...wire.Message) error {
	for _, m := range ms {
		if err := h.w.Write(m); err != nil {
			return err
		}
	}
	return nil
}

// frameMsgs wraps frames as host-to-child Frame messages.
func frameMsgs(frames []media.Frame) []wire.Message {
	out := make([]wire.Message, len(frames))
	for i, f := range frames {
		out[i] = wire.Frame{Rung: -1, F: f}
	}
	return out
}

func (h *host) sendFrames(frames []media.Frame) error {
	for _, f := range frames {
		if err := h.w.Write(wire.Frame{Rung: -1, F: f}); err != nil {
			return err
		}
	}
	return nil
}

// ended is how a child's run ended: its result, every message it wrote, and
// the error that ended the host's read.
type ended struct {
	runErr  error
	msgs    []wire.Message
	readErr error
}

// wait returns how the child ended, after the stream has ended.
func (h *host) wait(t *testing.T, errc <-chan error) ended {
	t.Helper()
	var runErr error
	select {
	case runErr = <-errc:
	case <-time.After(childTimeout):
		t.Fatal("child did not return")
	}
	select {
	case <-h.done:
	case <-time.After(childTimeout):
		t.Fatal("stream did not end")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return ended{runErr, h.msgs, h.readErr}
}

func requireHello(t *testing.T, msgs []wire.Message, pid int) {
	t.Helper()
	require.NotEmpty(t, msgs)
	require.Equal(t, wire.Hello{Version: wire.Version, PID: pid}, msgs[0])
}

// collected splits the child's messages into frames by rung, stats and
// errors.
type collected struct {
	frames map[int][]media.Frame
	stats  []wire.Stats
	errs   []wire.Error
}

func collect(msgs []wire.Message) collected {
	c := collected{frames: map[int][]media.Frame{}}
	for _, m := range msgs {
		switch v := m.(type) {
		case wire.Frame:
			c.frames[v.Rung] = append(c.frames[v.Rung], v.F)
		case wire.Stats:
			c.stats = append(c.stats, v)
		case wire.Error:
			c.errs = append(c.errs, v)
		}
	}
	return c
}

// rendition merges a rung's video with the audio by DTS, as a rendition hub
// holds them.
func (c collected) rendition(rung int) []media.Frame {
	out := append(slices.Clone(c.frames[rung]), c.frames[-1]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DTS < out[j].DTS })
	return out
}

func ofKind(frames []media.Frame, k media.Kind) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Kind() == k {
			out = append(out, f)
		}
	}
	return out
}

func keys(frames []media.Frame) []int64 {
	var out []int64
	for _, f := range frames {
		if f.Key && f.Kind() == media.KindVideo {
			out = append(out, f.PTS)
		}
	}
	slices.Sort(out)
	return out
}

// presentedFrom returns the frames presented at or after pts.
func presentedFrom(frames []media.Frame, pts int64) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.PTS >= pts {
			out = append(out, f)
		}
	}
	return out
}

func requireConforms(t *testing.T, frames []media.Frame) {
	t.Helper()
	v := conform.CheckFrames(frames, conform.FrameOptions{})
	require.Empty(t, v, "violations: %v", v)
}

func TestChildOverSocketpair(t *testing.T) {
	src := source(t)
	h, errc := startChild(t, run)

	require.NoError(t, h.send(wire.Configure{
		Rungs: []wire.Rung{{Height: 240, BitrateKbps: 600}, {Height: 180, BitrateKbps: 300}},
		Audio: &wire.AudioEncode{BitrateKbps: 96},
	}))
	require.NoError(t, h.sendFrames(src))
	require.NoError(t, h.send(wire.Stop{}))

	e := h.wait(t, errc)
	runErr, msgs, readErr := e.runErr, e.msgs, e.readErr
	require.NoError(t, runErr)
	require.ErrorIs(t, readErr, io.EOF, "the child closes its end cleanly")
	requireHello(t, msgs, os.Getpid())

	c := collect(msgs)
	require.Empty(t, c.errs)
	srcVideo := ofKind(src, media.KindVideo)
	counts := make([]uint64, 2)
	for rung, want := range [][2]int{{320, 240}, {240, 180}} {
		v := c.frames[rung]
		require.Len(t, v, len(srcVideo), "rung %d", rung)
		assert.Equal(t, keys(srcVideo), keys(v), "rung %d keys", rung)
		assert.Equal(t, want, [2]int{v[0].Config.Width, v[0].Config.Height}, "rung %d dims", rung)
		for _, f := range v {
			require.Same(t, v[0].Config, f.Config, "rung %d config pointer", rung)
			require.Equal(t, 0, f.Track)
		}
		requireConforms(t, c.rendition(rung))
		counts[rung] = uint64(len(v))
	}
	a := c.frames[-1]
	require.NotEmpty(t, a)
	for i := 1; i < len(a); i++ {
		require.Equal(t, int64(1920), a[i].PTS-a[i-1].PTS, "audio frame %d", i)
	}

	require.NotEmpty(t, c.stats)
	last := c.stats[len(c.stats)-1]
	assert.Equal(t, uint64(len(src)), last.In)
	assert.Equal(t, counts, last.Out)
	require.Len(t, last.EncodeNanos, 2)
	assert.Positive(t, last.EncodeNanos[0])
	_, isStats := msgs[len(msgs)-1].(wire.Stats)
	assert.True(t, isStats, "the last message is the final Stats")
	t.Logf("rungs %v frames, audio %d frames, %d stats", counts, len(a), len(c.stats))
}

// The native pipeline's counters reach the host in Stats (ruling R-T6a): a
// second video track is dropped, and an audio gap re-anchors once.
func TestChildReportsMediaCounters(t *testing.T) {
	var src []media.Frame
	na := 0
	for _, f := range source(t) {
		if f.Kind() == media.KindAudio {
			na++
			if na > 100 && na <= 110 { // a 10-frame audio gap
				continue
			}
		}
		src = append(src, f)
		if f.Kind() == media.KindVideo {
			f.Track = 2
			src = append(src, f)
		}
	}
	h, errc := startChild(t, run)
	require.NoError(t, h.send(wire.Configure{
		Rungs: []wire.Rung{{Height: 120, BitrateKbps: 150}},
		Audio: &wire.AudioEncode{BitrateKbps: 96},
	}))
	require.NoError(t, h.sendFrames(src))
	require.NoError(t, h.send(wire.Stop{}))

	e := h.wait(t, errc)
	require.NoError(t, e.runErr)
	c := collect(e.msgs)
	require.Empty(t, c.errs)
	require.NotEmpty(t, c.stats)
	last := c.stats[len(c.stats)-1]
	assert.Equal(t, wire.Stats{
		In: uint64(len(src)), Out: last.Out, EncodeNanos: last.EncodeNanos,
		DroppedTracks: 1, Reanchors: 1,
	}, last)
	assert.Equal(t, []uint64{uint64(len(c.frames[0]))}, last.Out)
}

func TestChildBadConfigure(t *testing.T) {
	cases := map[string]wire.Configure{
		"no rungs and no audio": {},
		"odd height":            {Rungs: []wire.Rung{{Height: 241, BitrateKbps: 300}}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			h, errc := startChild(t, run)
			require.NoError(t, h.send(cfg))

			end := h.wait(t, errc)
			runErr, msgs, readErr := end.runErr, end.msgs, end.readErr
			require.Error(t, runErr)
			require.ErrorIs(t, readErr, io.EOF)
			requireHello(t, msgs, os.Getpid())
			require.Len(t, msgs, 2)
			e, ok := msgs[1].(wire.Error)
			require.True(t, ok, "got %T", msgs[1])
			assert.True(t, e.Fatal)
			assert.Equal(t, -1, e.Rung)
			assert.Equal(t, runErr.Error(), e.Message)
		})
	}
}

// TestChildNotConfigureFirst: any message before Configure is fatal, except
// Stop and EOF, which end the child cleanly without an Error.
func TestChildNotConfigureFirst(t *testing.T) {
	t.Run("frame is fatal", func(t *testing.T) {
		h, errc := startChild(t, run)
		require.NoError(t, h.sendFrames(source(t)[:1]))

		end := h.wait(t, errc)
		require.ErrorContains(t, end.runErr, "wire.Frame")
		requireHello(t, end.msgs, os.Getpid())
		c := collect(end.msgs)
		require.Len(t, c.errs, 1)
		assert.True(t, c.errs[0].Fatal)
	})
	clean := map[string]func(h *host) error{
		"stop": func(h *host) error { return h.send(wire.Stop{}) },
		"eof":  func(h *host) error { return h.conn.CloseWrite() },
	}
	for name, end := range clean {
		t.Run(name+" is clean", func(t *testing.T) {
			h, errc := startChild(t, run)
			require.NoError(t, end(h))

			e := h.wait(t, errc)
			require.NoError(t, e.runErr)
			require.ErrorIs(t, e.readErr, io.EOF)
			requireHello(t, e.msgs, os.Getpid())
			assert.Len(t, e.msgs, 1, "only Hello, no Error")
		})
	}
}

// TestChildDiscontinuityEOF: a Discontinuity mid-GOP resumes at the next
// key, EOF from the host flushes and ends cleanly, and Stats arrive on the
// interval.
func TestChildDiscontinuityEOF(t *testing.T) {
	src := source(t)
	h, errc := startChild(t, func(c io.ReadWriteCloser) error { return serve(c, 5*time.Millisecond) })

	// Cut after video 35 and resume at video 40, as in the native test.
	var cut, resume int
	nv := 0
	for i, f := range src {
		if f.Kind() != media.KindVideo {
			continue
		}
		if nv == 36 {
			cut = i
		}
		if nv == 40 {
			resume = i
			break
		}
		nv++
	}
	sent := append(slices.Clone(src[:cut]), src[resume:]...)

	require.NoError(t, h.send(wire.Configure{Rungs: []wire.Rung{{Height: 180, BitrateKbps: 300}}, Audio: &wire.AudioEncode{}}))
	require.NoError(t, h.sendFrames(src[:cut]))
	require.NoError(t, h.send(wire.Discontinuity{}))
	for _, f := range src[resume:] {
		require.NoError(t, h.w.Write(wire.Frame{Rung: -1, F: f}))
		time.Sleep(100 * time.Microsecond) // let a few Stats ticks happen
	}
	require.NoError(t, h.conn.CloseWrite())

	e := h.wait(t, errc)
	runErr, msgs, readErr := e.runErr, e.msgs, e.readErr
	require.NoError(t, runErr)
	require.ErrorIs(t, readErr, io.EOF)
	requireHello(t, msgs, os.Getpid())

	c := collect(msgs)
	require.Empty(t, c.errs)
	v := c.frames[0]
	require.NotEmpty(t, v)
	requireConforms(t, c.rendition(0))
	// Video before the cut comes out whole; after it, output resumes at
	// the first source key at or after the resume point.
	var resumeKey int64 = -1
	for _, f := range src[resume:] {
		if f.Kind() == media.KindVideo && f.Key {
			resumeKey = f.PTS
			break
		}
	}
	require.NotEqual(t, int64(-1), resumeKey)
	var after []media.Frame
	for _, f := range v {
		if f.PTS >= resumeKey {
			after = append(after, f)
		}
	}
	require.NotEmpty(t, after)
	assert.True(t, after[0].Key, "first frame after the discontinuity is a key")
	assert.Equal(t, 36, len(v)-len(after), "every frame before the cut, none between it and the key")
	assert.Len(t, after, len(presentedFrom(ofKind(src, media.KindVideo), resumeKey)))

	require.GreaterOrEqual(t, len(c.stats), 2, "stats on the interval and at the end")
	for i := 1; i < len(c.stats); i++ {
		require.GreaterOrEqual(t, c.stats[i].In, c.stats[i-1].In)
		require.Len(t, c.stats[i].Out, 1)
		require.Len(t, c.stats[i].EncodeNanos, 1)
	}
	last := c.stats[len(c.stats)-1]
	assert.Equal(t, uint64(len(sent)), last.In)
	assert.Equal(t, []uint64{uint64(len(v))}, last.Out)
	t.Logf("video %d frames, %d after the cut, %d stats", len(v), len(after), len(c.stats))
}

// TestChildPipelineError: an encoder that fails to open is a fatal Error,
// and run returns it.
func TestChildPipelineError(t *testing.T) {
	src := source(t)
	h, errc := startChild(t, run)

	require.NoError(t, h.send(wire.Configure{Rungs: []wire.Rung{{Height: 180, BitrateKbps: 300, Preset: "no-such-preset"}}}))
	// The child stops reading at the error, so later writes may fail.
	_ = h.sendFrames(src)
	_ = h.send(wire.Stop{})

	end := h.wait(t, errc)
	runErr, msgs := end.runErr, end.msgs
	require.Error(t, runErr)
	requireHello(t, msgs, os.Getpid())
	c := collect(msgs)
	require.Len(t, c.errs, 1)
	assert.True(t, c.errs[0].Fatal)
	assert.Contains(t, c.errs[0].Message, "rung 0")
	assert.Equal(t, runErr.Error(), c.errs[0].Message)
	assert.Empty(t, c.frames[0])
}

// TestMainOverFD3 runs main in a subprocess with the socketpair on fd 3, and
// checks its exit status.
func TestMainOverFD3(t *testing.T) {
	audio := ofKind(source(t), media.KindAudio)[:20]
	cases := []struct {
		name string
		send []wire.Message
		code int
		// clean: the child writes only Hello and at most one stderr line.
		clean bool
	}{
		{"stop", append(append([]wire.Message{wire.Configure{Audio: &wire.AudioEncode{}}}, frameMsgs(audio)...), wire.Stop{}), 0, false},
		{"bad configure", []wire.Message{wire.Configure{}}, 1, false},
		{"stop before configure", []wire.Message{wire.Stop{}}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := socketpair(t)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself
			cmd.Env = append(os.Environ(), envChild+"=1")
			cmd.ExtraFiles = []*os.File{a}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			require.NoError(t, cmd.Start())
			a.Close()
			h := newHost(t, fileConn(t, b))

			require.NoError(t, h.send(tc.send...))
			err := cmd.Wait()
			<-h.done
			h.mu.Lock()
			defer h.mu.Unlock()
			if tc.code == 0 {
				require.NoError(t, err, "stderr: %s", stderr.String())
			} else {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				assert.Equal(t, tc.code, exit.ExitCode(), "stderr: %s", stderr.String())
			}
			require.ErrorIs(t, h.readErr, io.EOF)
			requireHello(t, h.msgs, cmd.Process.Pid)
			c := collect(h.msgs)
			switch {
			case tc.clean:
				assert.Len(t, h.msgs, 1, "only Hello, no Error")
				assert.LessOrEqual(t, bytes.Count(stderr.Bytes(), []byte("\n")), 1, "stderr: %s", stderr.String())
			case tc.code == 0:
				assert.Empty(t, c.errs)
				assert.NotEmpty(t, c.frames[-1])
			default:
				require.Len(t, c.errs, 1)
				assert.True(t, c.errs[0].Fatal)
			}
		})
	}
}

// runMain runs main in a subprocess with args and returns its exit code,
// stdout and stderr.
func runMain(t *testing.T, args string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), envChild+"=1", envArgs+"="+args)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		require.NoError(t, err)
	}
	return code, out.String(), errb.String()
}

// TestProbeFlag: -probe nvenc exits 0 with "nvenc: ok" when a device opens,
// or 3 with the error; any other probe is bad usage.
func TestProbeFlag(t *testing.T) {
	code, stdout, stderr := runMain(t, "-probe nvenc")
	t.Logf("-probe nvenc: exit %d, stdout %q", code, stdout)
	switch code {
	case 0:
		assert.Equal(t, "nvenc: ok\n", stdout)
	case 3:
		require.True(t, strings.HasPrefix(stdout, "nvenc: "), "stdout %q", stdout)
		assert.NotEqual(t, "nvenc: ok\n", stdout)
		assert.Equal(t, 1, strings.Count(stdout, "\n"), "stdout %q", stdout)
		require.NotEqual(t, "1", os.Getenv("OS_REQUIRE_NVENC"), "OS_REQUIRE_NVENC=1 but the probe failed: %s", stdout)
	default:
		t.Fatalf("-probe nvenc: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	for _, args := range []string{"-probe x", "-probe", "-probe nvenc extra"} {
		code, stdout, stderr = runMain(t, args)
		assert.Equal(t, 2, code, "%q", args)
		assert.Empty(t, stdout, "%q", args)
		assert.Contains(t, stderr, "usage: ostranscoder", "%q", args)
	}
}

// TestChildEncoderFallbacks: the rung's Encoder reaches the pipeline, and
// its fallbacks reach the host in Stats: 0 when NVENC opens, 1 otherwise.
func TestChildEncoderFallbacks(t *testing.T) {
	want := uint64(0)
	if err := native.ProbeNVENC(); err != nil {
		want = 1
	}
	h, errc := startChild(t, run)
	require.NoError(t, h.send(wire.Configure{Rungs: []wire.Rung{{Height: 120, BitrateKbps: 150, Encoder: "nvenc"}}}))
	require.NoError(t, h.sendFrames(ofKind(source(t), media.KindVideo)))
	require.NoError(t, h.send(wire.Stop{}))

	e := h.wait(t, errc)
	require.NoError(t, e.runErr)
	c := collect(e.msgs)
	require.Empty(t, c.errs)
	require.NotEmpty(t, c.frames[0])
	require.NotEmpty(t, c.stats)
	assert.Equal(t, want, c.stats[len(c.stats)-1].EncoderFallbacks)
}

// TestServeSetWatermark: Configure with a watermark, frames, then
// SetWatermark{nil}, frames, Stop: the child exits cleanly and writes as
// many frames as without a watermark.
func TestServeSetWatermark(t *testing.T) {
	src := source(t)
	video := ofKind(src, media.KindVideo)
	h, errc := startChild(t, run)
	require.NoError(t, h.send(wire.Configure{
		Rungs:     []wire.Rung{{Height: 120, BitrateKbps: 150}},
		Audio:     &wire.AudioEncode{},
		Watermark: &picture.Watermark{Text: "live %H:%M:%S", Position: picture.BottomRight},
	}))
	half := len(src) / 2
	require.NoError(t, h.sendFrames(src[:half]))
	require.NoError(t, h.send(wire.SetWatermark{}))
	require.NoError(t, h.sendFrames(src[half:]))
	require.NoError(t, h.send(wire.Stop{}))

	e := h.wait(t, errc)
	require.NoError(t, e.runErr)
	require.ErrorIs(t, e.readErr, io.EOF)
	c := collect(e.msgs)
	require.Empty(t, c.errs)
	require.Len(t, c.frames[0], len(video))
	assert.Equal(t, keys(video), keys(c.frames[0]))
	requireConforms(t, c.rendition(0))
}

// TestServeSetWatermarkInvalid: a watermark the child cannot build is a
// fatal error (R-C9).
func TestServeSetWatermarkInvalid(t *testing.T) {
	src := source(t)
	h, errc := startChild(t, run)
	require.NoError(t, h.send(wire.Configure{Rungs: []wire.Rung{{Height: 120, BitrateKbps: 150}}}))
	require.NoError(t, h.sendFrames(src[:40]))
	require.NoError(t, h.send(wire.SetWatermark{W: &picture.Watermark{Text: "%q"}}))

	e := h.wait(t, errc)
	require.Error(t, e.runErr)
	c := collect(e.msgs)
	require.Len(t, c.errs, 1)
	assert.True(t, c.errs[0].Fatal)
	assert.Contains(t, c.errs[0].Message, "watermark")
}

// TestChildConfigureBadWatermark: Configure with an invalid watermark is a
// fatal error.
func TestChildConfigureBadWatermark(t *testing.T) {
	h, errc := startChild(t, run)
	require.NoError(t, h.send(wire.Configure{
		Rungs:     []wire.Rung{{Height: 120, BitrateKbps: 150}},
		Watermark: &picture.Watermark{Text: "%q"},
	}))
	e := h.wait(t, errc)
	require.Error(t, e.runErr)
	c := collect(e.msgs)
	require.Len(t, c.errs, 1)
	assert.True(t, c.errs[0].Fatal)
}
