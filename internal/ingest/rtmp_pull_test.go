package ingest

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

// wait waits until the reading goroutine of l has ended.
func (l *readLog) wait(t *testing.T) {
	t.Helper()
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Read did not end; %d frames so far", l.count())
	}
}

// openPull opens rawURL and closes the reader when the test ends.
func openPull(t *testing.T, ctx context.Context, rawURL string, env Env) Reader {
	t.Helper()
	r, err := Open(ctx, rawURL, env)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// Frames pulled from a server come out exactly as flvFrames makes them from
// the source tags, aggregates or not, stamped by Env.Now; our mode
// parameter is not sent; the end of the stream is io.ErrUnexpectedEOF after
// the last frame, held video included; the open ctx bounds only the open.
func TestRTMPPullPlayServer(t *testing.T) {
	for _, c := range []struct {
		name      string
		fixture   string
		aggregate bool
		tags      int // the first tags of the fixture; 0 means all
	}{
		{"h264 aggregates", "h264_aac.flv", true, 0},
		{"h265 aggregates", "h265_aac.flv", true, 0},
		{"h264 messages", "h264_aac.flv", false, 0},
		{"h264 ends with video held", "h264_aac.flv", false, 40},
	} {
		t.Run(c.name, func(t *testing.T) {
			tags := fixtureTags(t, c.fixture)
			if c.tags > 0 {
				tags = tags[:c.tags]
				require.Positive(t, heldAtEnd(t, tags), "the stream must end with video held")
			}
			want := sourceFrames(t, tags)
			srv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{Aggregate: c.aggregate})
			at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
			ctx, cancel := context.WithCancel(context.Background())
			r := openPull(t, ctx, srv.URL("live/"+vStreamKey+"?mode=pull&token=x"), Env{Now: func() time.Time { return at }})
			cancel() // the reader outlives the open ctx
			assert.Equal(t, Spec{Live: true}, r.Spec())
			assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())

			log := readAll(r)
			log.wait(t)
			assert.Equal(t, want, mediaFrames(log.frames))
			assert.Empty(t, log.breaks)
			for _, f := range log.frames {
				require.Equal(t, at, f.Arrival)
			}
			require.ErrorIs(t, log.err, io.ErrUnexpectedEOF, "a live pull never ends cleanly")
			assert.NotContains(t, log.err.Error(), vStreamKey)
			_, err := r.Read()
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "the end is returned again")

			require.Len(t, srv.Requests(), 1)
			assert.Equal(t, vStreamKey+"?token=x", srv.Requests()[0].Stream, "our mode parameter is stripped")
			if c.aggregate {
				assert.Positive(t, srv.Aggregates())
			}
			st := r.Stats()
			assert.EqualValues(t, len(want), st["frames"])
			assert.Zero(t, st["bad_tags"])
			assert.Zero(t, st["bad_commands"])
			for k := range st {
				assert.NotContains(t, k, vStreamKey)
			}
		})
	}
}

// A refusal names the server's status, with the stream key and secrets
// redacted from the URL and from the server's text.
func TestRTMPPullStreamNotFound(t *testing.T) {
	srv := rtmptest.NewPlayServer(t, nil, rtmptest.PlayOptions{NotFound: true})
	_, err := Open(context.Background(), srv.URL("live/"+vStreamKey+"?token="+vSecret), Env{})
	require.Error(t, err)
	var se *rtmp.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "NetStream.Play.StreamNotFound", se.Code)
	assert.Contains(t, err.Error(), "NetStream.Play.StreamNotFound")
	assert.Contains(t, err.Error(), "live/skxxxxx")
	assert.NotContains(t, err.Error(), vStreamKey)
	assert.NotContains(t, err.Error(), vSecret)
	assert.Len(t, srv.Requests(), 1)
}

// Close unblocks a Read that waits on a silent server; Read then returns
// errRTMPClosed, Close is idempotent and no goroutine is left behind.
// Not parallel: it counts goroutines.
func TestRTMPPullCloseUnblocks(t *testing.T) {
	tags := fixtureTags(t, "h264_aac.flv")
	want := sourceFrames(t, tags)
	srv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{Hold: true})
	cycle := func(t *testing.T) {
		r, err := Open(context.Background(), srv.URL("live/s"), Env{})
		require.NoError(t, err)
		log := readAll(r)
		// The last held frames come out only at the end, so all but those.
		log.waitCount(t, len(want)-heldAtEnd(t, tags))
		time.Sleep(50 * time.Millisecond) // Read blocks
		log.stop(t, r)
		assert.ErrorIs(t, log.err, errRTMPClosed)
		_, err = r.Read()
		assert.ErrorIs(t, err, errRTMPClosed, "Read after Close")
		require.NoError(t, r.Close(), "Close is idempotent")
	}
	cycle(t) // warm up lazily started goroutines
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 3 {
		cycle(t)
	}
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		3*time.Second, 20*time.Millisecond, "goroutines before=%d now=%d", before, runtime.NumGoroutine())
}

// silentTCP accepts connections and never answers; it returns host:port.
func silentTCP(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

func TestOpenRTMPPull(t *testing.T) {
	for _, u := range []string{
		"rtmp://host/live/s",
		"rtmp://host:1940/live/inst/s?mode=pull&token=x",
		"RTMP://host/live/s",
	} {
		got, err := Parse(u)
		require.NoError(t, err, u)
		assert.Equal(t, Spec{Live: true}, got, u)
	}

	// The open ctx bounds the play exchange: a server that never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Open(ctx, "rtmp://"+silentTCP(t)+"/live/"+vStreamKey, Env{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), vStreamKey)

	// And the wait for the tracks: a server that sends nothing after the
	// play start.
	srv := rtmptest.NewPlayServer(t, nil, rtmptest.PlayOptions{Hold: true})
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = Open(ctx, srv.URL("live/"+vStreamKey), Env{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), vStreamKey)

	// A stream that ends before its tracks are known fails the open.
	short := rtmptest.NewPlayServer(t, tagsOf(kindsSession(t, 0, -1, 500)), rtmptest.PlayOptions{})
	_, err = Open(context.Background(), short.URL("live/"+vStreamKey), Env{})
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.NotContains(t, err.Error(), vStreamKey)

	// A refused connection names the redacted URL only.
	port := freeTCPPort(t)
	_, err = Open(context.Background(), "rtmp://127.0.0.1:"+port+"/live/"+vStreamKey, Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "live/skxxxxx")
	assert.NotContains(t, err.Error(), vStreamKey)

	// The port defaults to 1935.
	t.Run("default port 1935", func(t *testing.T) {
		requireBind1935(t)
		ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:1935")
		require.NoError(t, err, "%s is set but port 1935 is not free", envBind1935)
		defer ln.Close()
		accepted := make(chan struct{})
		go func() {
			if c, err := ln.Accept(); err == nil {
				close(accepted)
				_ = c.Close()
			}
		}()
		_, err = Open(context.Background(), "rtmp://127.0.0.1/live/s", Env{})
		require.Error(t, err)
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("Open did not dial port 1935")
		}
	})
}

// envBind1935 allows tests to bind 127.0.0.1:1935. Only CI sets it: on a
// development machine that port may belong to a running server (or be
// forbidden), and a test must never take it.
const envBind1935 = "OS_TEST_BIND_1935"

// requireBind1935 skips the test unless envBind1935 is "1".
func requireBind1935(t *testing.T) {
	t.Helper()
	if os.Getenv(envBind1935) != "1" {
		t.Skipf("binds 127.0.0.1:1935, the RTMP default port; set %s=1 to run it (CI does)", envBind1935)
	}
}

// We pull from ffmpeg serving the fixture; the frames pass the Clock clean.
func TestRTMPFFmpegServerPull(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	url := "rtmp://127.0.0.1:" + stableTCPPort(t) + "/live/s"
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
		"-i", fixturePath("h264_aac.ts"), "-c", "copy", "-f", "flv", "-listen", "1", url)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})

	// Retry until ffmpeg listens.
	var r Reader
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var err error
		r, err = Open(ctx, url, Env{})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Open from ffmpeg: %v (ffmpeg: %s)", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { _ = r.Close() })
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())

	log := readAll(r)
	select {
	case <-log.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the pull did not end; %d frames", log.count())
	}
	t.Logf("end: %v", log.err)
	require.ErrorIs(t, log.err, io.ErrUnexpectedEOF, "the server ended the stream")
	want := map[int]int{}
	for _, f := range demuxFrames(t, "h264_aac.ts") {
		want[f.Track]++
	}
	assert.Len(t, byTrack(log.frames, 0), want[0])
	assert.InDelta(t, want[1], len(byTrack(log.frames, 1)), 2)
	assert.Empty(t, log.breaks)
	st := r.Stats()
	assert.Zero(t, st["bad_tags"])
	assert.Zero(t, st["dropped_no_config"])
	clockCheck(t, r, log.frames)
}

// An rtmps:// pull verified with Env.TLS reads the frames a plain pull
// reads; the server sees the scheme rtmps and nothing of ours.
func TestRTMPSPull(t *testing.T) {
	ca := tlstest.NewCA(t)
	tags := fixtureTags(t, "h264_aac.flv")
	read := func(t *testing.T, srv *rtmptest.PlayServer, env Env) []media.Frame {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		r := openPull(t, ctx, srv.URL("live/"+vStreamKey+"?mode=pull&token=x"), env)
		assert.Equal(t, Spec{Live: true}, r.Spec())
		assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
		log := readAll(r)
		log.wait(t)
		require.ErrorIs(t, log.err, io.ErrUnexpectedEOF)
		assert.NotContains(t, log.err.Error(), vStreamKey)
		return mediaFrames(log.frames)
	}
	plain := read(t, rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{}), Env{})
	require.Equal(t, sourceFrames(t, tags), plain)

	srv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{TLS: ca.Server(t)})
	client := ca.Client()
	assert.Equal(t, plain, read(t, srv, Env{TLS: client}), "the frames of the plain pull")
	assert.Empty(t, client.ServerName, "Env.TLS is not modified")

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "rtmps://"+srv.Addr+"/live", reqs[0].TCURL)
	assert.Equal(t, vStreamKey+"?token=x", reqs[0].Stream, "our mode parameter is stripped")
	for _, s := range []string{reqs[0].App, reqs[0].Stream, reqs[0].TCURL} {
		assert.NotContains(t, s, "insecure")
	}
}
