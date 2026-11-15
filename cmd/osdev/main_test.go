package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
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
		"bad url":     {"-stream", "a=srt://:99999?mode=listener"},
		"bad layout":  {"-layout", "video,text", "-stream", "a=" + fixture},
		"empty lay":   {"-layout", "", "-stream", "a=" + fixture},
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

func TestOsdevFailoverFlags(t *testing.T) {
	parse := func(args ...string) ([]*source, string, int) {
		var errw strings.Builder
		cfg, code := parseArgs(args, &errw)
		if cfg == nil {
			return nil, errw.String(), code
		}
		return cfg.sources, errw.String(), code
	}

	t.Run("priority order", func(t *testing.T) {
		srcs, _, _ := parse("-stream", "a=srt://127.0.0.1:9000?mode=listener", "-stream", "b="+fixture,
			"-stream", "a="+fixture)
		require.Len(t, srcs, 2)
		assert.Equal(t, "a", srcs[0].name)
		assert.Equal(t, []string{"srt://127.0.0.1:9000?mode=listener", fixture}, srcs[0].inputs)
		assert.Equal(t, "b", srcs[1].name)
	})
	t.Run("loop applies to files only", func(t *testing.T) {
		srcs, _, _ := parse("-loop", "-stream", "a="+fixture, "-stream", "a=udp://@:5000",
			"-stream", "a=file:///x.ts?pace=0")
		require.Len(t, srcs, 1)
		in := srcs[0].inputs
		require.Len(t, in, 3)
		assert.True(t, strings.HasPrefix(in[0], "file:///") && strings.HasSuffix(in[0], "h264_aac.ts?loop=1"), in[0])
		assert.Equal(t, "udp://@:5000", in[1])
		assert.Equal(t, "file:///x.ts?loop=1&pace=0", in[2])
	})
	t.Run("layout", func(t *testing.T) {
		cfg, code := parseArgs([]string{"-layout", "audio", "-stream", "a=" + fixture}, io.Discard)
		require.NotNil(t, cfg, code)
		assert.Equal(t, []media.Kind{media.KindAudio}, cfg.layout)
		cfg, _ = parseArgs([]string{"-stream", "a=" + fixture}, io.Discard)
		require.NotNil(t, cfg)
		assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, cfg.layout)
	})
	t.Run("malformed url", func(t *testing.T) {
		var errw strings.Builder
		cfg, code := parseArgs([]string{"-stream", "a=" + fixture, "-stream", "a=srt://user:secret@:99999?passphrase=hunter2"}, &errw)
		assert.Nil(t, cfg)
		assert.Equal(t, 2, code)
		assert.NotContains(t, errw.String(), "secret")
		assert.NotContains(t, errw.String(), "hunter2")
		var out strings.Builder
		assert.Equal(t, 2, run(context.Background(), []string{"-stream", "a=srt://:99999?mode=listener"}, &out, io.Discard))
		assert.NotContains(t, out.String(), "listening")
	})
}

func TestOsdevPipelineReport(t *testing.T) {
	src := &source{name: "a", inputs: []string{"srt://u:p@h:1?passphrase=xyz", "udp://@:5000"}}
	var sb strings.Builder
	src.reportPipeline(&sb, pipeline.Stats{Active: 0, Switches: 2, Unmapped: 3, Input: map[string]uint64{"srt_b": 2, "srt_a": 1}})
	line := sb.String()
	assert.Contains(t, line, "stream a: input 0 srt://u:xxxxx@h:1?passphrase=xxxxx")
	assert.Contains(t, line, "switches 2, unmapped 3, drift breaks 0")
	assert.Contains(t, line, "srt_a=1 srt_b=2")
	assert.NotContains(t, line, "xyz")
	src.reportPipeline(&sb, pipeline.Stats{Active: 0, Switches: 2, Unmapped: 3, Input: map[string]uint64{"srt_b": 2, "srt_a": 1}})
	assert.Equal(t, line, sb.String(), "unchanged: not repeated")
	src.reportPipeline(&sb, pipeline.Stats{Active: -1, Switches: 3})
	assert.Contains(t, strings.TrimPrefix(sb.String(), line), "input none")
}

func TestOsdevSRTStream(t *testing.T) {
	pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, pc.Close())

	var errw syncBuilder
	addr, stop := startOsdev(t, []string{"-stream", fmt.Sprintf("a=srt://:%d?mode=listener", port)}, &errw)

	data, err := os.ReadFile(fixture)
	require.NoError(t, err)
	pubCtx, pubCancel := context.WithCancel(context.Background())
	var pubWG sync.WaitGroup
	pubWG.Add(1)
	go func() {
		defer pubWG.Done()
		var conn srt.Conn
		for pubCtx.Err() == nil {
			conn, err = srt.Dial("srt", "127.0.0.1:"+strconv.Itoa(port), srt.DefaultConfig())
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if conn == nil {
			return
		}
		defer conn.Close()
		const chunk = 7 * 188
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for off := 0; off < len(data); off += chunk {
			select {
			case <-pubCtx.Done():
				return
			case <-tick.C:
			}
			if _, err := conn.Write(data[off:min(off+chunk, len(data))]); err != nil {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		resp, err := httpGet("http://" + addr + "/live/a/hls/index.m3u8")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == 200 && strings.Contains(string(b), ".ts")
	}, 30*time.Second, 100*time.Millisecond)
	pubCancel()
	pubWG.Wait()
	assert.Equal(t, 0, stop())
}
