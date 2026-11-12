package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
)

const fixture = "../../testdata/fixtures/h264_aac.ts"

func httpGet(u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func TestOsdevServes(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, []string{"-listen", "127.0.0.1:0", "-stream", "demo=" + fixture, "-loop"}, pw, io.Discard)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	require.True(t, sc.Scan())
	first := sc.Text()
	require.True(t, strings.HasPrefix(first, "listening on 127.0.0.1:"), first)
	addr := strings.TrimPrefix(first, "listening on ")
	var urls []string
	for i := 0; i < 4 && sc.Scan(); i++ {
		urls = append(urls, sc.Text())
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	require.Len(t, urls, 4)
	assert.Equal(t, "http://"+addr+"/live/demo/hls/index.m3u8", urls[0])
	assert.True(t, strings.HasSuffix(urls[3], "/live/demo/stream.ts"))

	get := func(path string) (int, string) {
		resp, err := httpGet("http://" + addr + path)
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	require.Eventually(t, func() bool {
		st, body := get("/live/demo/hls/index.m3u8")
		return st == 200 && strings.Contains(body, ".ts")
	}, 15*time.Second, 50*time.Millisecond)
	st, _ := get("/live/nope/hls/index.m3u8")
	assert.Equal(t, 404, st)
	st, _ = get("/live/nope/stream.ts")
	assert.Equal(t, 404, st)

	// an open HTTP-TS response must not hold up shutdown
	resp, err := httpGet("http://" + addr + "/live/demo/stream.ts")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	cancel()
	select {
	case c := <-code:
		assert.Equal(t, 0, c)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

func TestOsdevBadArgs(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	for name, args := range map[string][]string{
		"bad flag":    {"-nope"},
		"extra arg":   {"-stream", "a=" + fixture, "x"},
		"no stream":   {},
		"no equals":   {"-stream", "demo"},
		"empty name":  {"-stream", "=x.ts"},
		"empty path":  {"-stream", "demo="},
		"slash name":  {"-stream", "a/b=x.ts"},
		"duplicate":   {"-stream", "a=x.ts", "-stream", "a=y.ts"},
		"port in use": {"-listen", ln.Addr().String(), "-stream", "a=" + fixture},
	} {
		t.Run(name, func(t *testing.T) {
			var sb strings.Builder
			assert.Equal(t, 2, run(context.Background(), args, &sb, io.Discard))
			assert.NotContains(t, sb.String(), "listening")
		})
	}
}

// A stream whose source fails is reported, and when every stream has
// failed osdev exits 1 without waiting for a signal.
func TestOsdevMissingFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, errw strings.Builder
	assert.Equal(t, 1, run(ctx, []string{"-listen", "127.0.0.1:0", "-stream", "a=/nonexistent.ts"}, &out, &errw))
	assert.NoError(t, ctx.Err(), "exited on its own")
	assert.Contains(t, errw.String(), "osdev: stream a stopped: pipeline: open:")
	assert.Contains(t, errw.String(), "every stream failed")
}

// startOsdev runs osdev with args and returns its address and a stop
// function that cancels it and returns its exit code.
func startOsdev(t *testing.T, args []string, stderr io.Writer) (string, func() int) {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, append([]string{"-listen", "127.0.0.1:0"}, args...), pw, stderr)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	require.True(t, sc.Scan())
	addr := strings.TrimPrefix(sc.Text(), "listening on ")
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	return addr, func() int {
		cancel()
		select {
		case c := <-code:
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return")
			return -1
		}
	}
}

// Without -loop the stream ends with its file: the last partial segment is
// packaged and the playlists end with ENDLIST, and the server keeps serving.
func TestOsdevEndsStream(t *testing.T) {
	var errw syncBuilder
	addr, stop := startOsdev(t, []string{"-stream", "v=../../testdata/fixtures/h264_640.ts"}, &errw)
	body := func(path string) string {
		resp, err := httpGet("http://" + addr + path)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	require.Eventually(t, func() bool {
		return strings.Contains(body("/live/v/hls/index.m3u8"), "#EXT-X-ENDLIST")
	}, 15*time.Second, 50*time.Millisecond)
	assert.Contains(t, body("/live/v/cmaf/v0.m3u8"), "#EXT-X-ENDLIST")
	assert.Contains(t, errw.String(), "osdev: stream v: source ended")
	assert.Equal(t, 0, stop())
}

// syncBuilder is a strings.Builder safe for concurrent use.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestOsdevReport(t *testing.T) {
	cfg := &media.TrackConfig{Codec: media.CodecH264}
	p := live.NewPackager(nil, live.Options{Now: func() time.Time { return time.Unix(1e9, 0) }})
	quiet := &source{name: "q", pack: live.NewPackager(nil, live.Options{})}
	loud := &source{name: "l", pack: p}
	srcs := []*source{quiet, loud}
	var sb strings.Builder
	report(&sb, srcs, false)
	report(&sb, srcs, true)
	assert.Empty(t, sb.String(), "nothing to report")

	// A config without parameter sets fails both muxers; no hub means no
	// anchor.
	for i := 0; i < 3; i++ {
		ts := int64(i) * 3600
		p.Handle(hub.Delivery{Frame: media.Frame{Track: 0, Config: cfg, PTS: ts, DTS: ts, Key: i == 0, Data: [][]byte{{0x65}}, Epoch: 1}})
	}
	p.Flush()
	require.Positive(t, p.Errors())
	require.EqualValues(t, 1, p.FallbackAnchors())
	require.EqualValues(t, 1, p.CodecErrors())
	report(&sb, srcs, false)
	assert.Equal(t, fmt.Sprintf("osdev: stream l: %d packager mux errors (dropped segment outputs), 1 tracks without a codec string, 1 epochs placed without a hub anchor\n", p.Errors()), sb.String())
	report(&sb, srcs, false)
	assert.Equal(t, 1, strings.Count(sb.String(), "\n"), "unchanged: not repeated")
	report(&sb, srcs, true)
	assert.Equal(t, 2, strings.Count(sb.String(), "\n"), "repeated at shutdown")
	assert.NotContains(t, sb.String(), "stream q")
}

func TestOsdevServerTimeouts(t *testing.T) {
	srv := newServer(http.NotFoundHandler())
	assert.Positive(t, srv.ReadHeaderTimeout)
	assert.Positive(t, srv.IdleTimeout)
	assert.Positive(t, srv.MaxHeaderBytes)
	// Long-lived HTTP-TS responses carry their own rolling write deadline; a
	// server-wide read or write timeout would cut them.
	assert.Zero(t, srv.ReadTimeout)
	assert.Zero(t, srv.WriteTimeout)
}
