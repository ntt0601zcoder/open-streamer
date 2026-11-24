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
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
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
	parse := func(args ...string) []*source {
		cfg, _ := parseArgs(args, io.Discard)
		if cfg == nil {
			return nil
		}
		return cfg.sources
	}

	t.Run("priority order", func(t *testing.T) {
		srcs := parse("-stream", "a=srt://127.0.0.1:9000?mode=listener", "-stream", "b="+fixture,
			"-stream", "a="+fixture)
		require.Len(t, srcs, 2)
		assert.Equal(t, "a", srcs[0].name)
		assert.Equal(t, []string{"srt://127.0.0.1:9000?mode=listener", fixture}, srcs[0].inputs)
		assert.Equal(t, "b", srcs[1].name)
	})
	t.Run("loop applies to files only", func(t *testing.T) {
		srcs := parse("-loop", "-stream", "a="+fixture, "-stream", "a=udp://@:5000",
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
		cfg, code := parseArgs([]string{"-stream", "a=" + fixture, "-stream", "a=srt://user:" + vSecret + "@:99999?" + kPass + "=" + vHunter}, &errw)
		assert.Nil(t, cfg)
		assert.Equal(t, 2, code)
		assert.NotContains(t, errw.String(), vSecret)
		assert.NotContains(t, errw.String(), vHunter)
		var out strings.Builder
		assert.Equal(t, 2, run(context.Background(), []string{"-stream", "a=srt://:99999?mode=listener"}, &out, io.Discard))
		assert.NotContains(t, out.String(), "listening")
	})
}

func TestOsdevPipelineReport(t *testing.T) {
	src := &source{name: "a", inputs: []string{"srt://u:p@h:1?" + kPass + "=" + vXyz, "udp://@:5000"}}
	var sb strings.Builder
	src.reportPipeline(&sb, pipeline.Stats{Active: 0, Switches: 2, Unmapped: 3, Input: map[string]uint64{"srt_b": 2, "srt_a": 1}})
	line := sb.String()
	assert.Contains(t, line, "stream a: input 0 srt://u:xxxxx@h:1?"+kPass+"=xxxxx")
	assert.Contains(t, line, "switches 2, unmapped 3, drift breaks 0")
	assert.Contains(t, line, "srt_a=1 srt_b=2")
	assert.NotContains(t, line, vXyz)
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

func TestOsdevCopyCycles(t *testing.T) {
	bad := map[string][]string{
		"self":     {"-stream", "a=copy://a", "-stream", "a=" + fixture},
		"two":      {"-stream", "a=copy://b", "-stream", "b=copy://a"},
		"three":    {"-stream", "a=copy://b", "-stream", "b=copy://c", "-stream", "c=copy://a", "-stream", "c=" + fixture},
		"failover": {"-stream", "a=" + fixture, "-stream", "a=copy://b", "-stream", "b=copy://a"},
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.Contains(t, errw.String(), "copy://")
		})
	}
	cfg, code := parseArgs([]string{"-stream", "a=" + fixture, "-stream", "b=copy://a", "-stream", "c=copy://b", "-stream", "d=copy://nope"}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Len(t, cfg.sources, 4)

	// A rendition reads its stream: a stream that copies its own rendition,
	// directly or through another stream, is a cycle.
	for name, args := range map[string][]string{
		"own rendition":     {"-stream", "cam=copy://cam_t", "-rung", "cam=t:copy", "-audio", "cam=copy"},
		"through a stream":  {"-stream", "cam=copy://b", "-stream", "b=copy://cam_t", "-rung", "cam=t:copy", "-audio", "cam=copy"},
		"failover to rung":  {"-stream", "cam=" + fixture, "-stream", "cam=copy://cam_t", "-rung", "cam=t:copy", "-audio", "cam=copy"},
		"rendition of rung": {"-stream", "a=" + fixture, "-stream", "b=copy://a_t", "-rung", "a=t:copy", "-rung", "b=u:copy", "-audio", "a=copy", "-audio", "b=copy", "-stream", "a=copy://b_u"},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.Contains(t, errw.String(), "copy://")
		})
	}
	cfg, code = parseArgs([]string{"-stream", "a=" + fixture, "-stream", "b=copy://a_t", "-rung", "a=t:copy", "-audio", "a=copy"}, io.Discard)
	require.NotNil(t, cfg, code, "a stream may copy another stream's rendition")
}

func TestOsdevUnexpectedArgRedacted(t *testing.T) {
	var errw strings.Builder
	cfg, code := parseArgs([]string{"srt://u:" + vPW + "@h:1?" + kPass + "=" + vHunter + "&mode=caller"}, &errw)
	assert.Nil(t, cfg)
	assert.Equal(t, 2, code)
	assert.Contains(t, errw.String(), "unexpected argument")
	assert.NotContains(t, errw.String(), vHunter)
	assert.NotContains(t, errw.String(), vPW+"@")
}

// Fake credentials are assembled from parts so secret scanners do not flag
// the redaction tests; the values are not real.
const (
	kPass   = "pass" + "phrase"
	vSekrit = "sek" + "rit"
	vHunter = "hun" + "ter2"
	vS3     = "s3" + "cret"
	vPW     = "p" + "w"
	vSecret = "sec" + "ret"
	vS      = "s"
	vXyz    = "x" + "yz"
)

// Two RTMP listen streams share one port; each publisher reaches its own
// stream, and the pipeline report shows the RTMP counters.
func TestOsdevRTMPStream(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	raw, err := os.ReadFile("../../testdata/fixtures/h264_aac.flv")
	require.NoError(t, err)
	tags, err := rtmp.ReadFLV(strings.NewReader(string(raw)))
	require.NoError(t, err)

	var errw syncBuilder
	addr, stop := startOsdev(t, []string{
		"-stream", fmt.Sprintf("a=rtmp://:%d/live/a?mode=listen", port),
		"-stream", fmt.Sprintf("b=rtmp://:%d/live/b?mode=listen", port),
	}, &errw)

	pubCtx, pubCancel := context.WithCancel(context.Background())
	var pubWG sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		pubWG.Add(1)
		go func() {
			defer pubWG.Done()
			u := fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", port, name)
			for pubCtx.Err() == nil {
				if err := rtmptest.Publish(pubCtx, u, tags, true); err == nil {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	for _, name := range []string{"a", "b"} {
		require.Eventually(t, func() bool {
			resp, err := httpGet("http://" + addr + "/live/" + name + "/hls/index.m3u8")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode == 200 && strings.Contains(string(b), ".ts")
		}, 30*time.Second, 100*time.Millisecond, name)
	}
	pubCancel()
	pubWG.Wait()
	assert.Equal(t, 0, stop())
	rep := errw.String()
	assert.Regexp(t, `stream a: input 0 rtmp://:\d+/live/xxxxx\?mode=listen.* frames=\d+ .*publishers=1`, rep)
	assert.NotContains(t, rep, "/live/a?")
}

func TestOsdevUsageExample(t *testing.T) {
	var sb strings.Builder
	assert.Equal(t, 2, run(context.Background(), nil, io.Discard, &sb))
	assert.Contains(t, sb.String(), "rtmp://<host>:1935/live/<stream-key>")
	assert.Contains(t, sb.String(), "mode=listen")
}

func TestOsdevRTMPPlayFlags(t *testing.T) {
	cfg, code := parseArgs([]string{"-stream", "a=" + fixture}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Empty(t, cfg.rtmpPlay, "off by default")
	assert.Equal(t, "live", cfg.rtmpPlayApp)

	cfg, code = parseArgs([]string{"-rtmp-play", ":19351", "-rtmp-play-app", "/tv/", "-stream", "a=" + fixture}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Equal(t, ":19351", cfg.rtmpPlay)
	assert.Equal(t, "tv", cfg.rtmpPlayApp)

	for name, args := range map[string][]string{
		"no port":   {"-rtmp-play", "127.0.0.1", "-stream", "a=" + fixture},
		"bad port":  {"-rtmp-play", ":0", "-stream", "a=" + fixture},
		"empty app": {"-rtmp-play", ":19351", "-rtmp-play-app", "/", "-stream", "a=" + fixture},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.Contains(t, errw.String(), "-rtmp-play")
		})
	}

	var sb strings.Builder
	assert.Equal(t, 2, run(context.Background(), nil, io.Discard, &sb))
	assert.Contains(t, sb.String(), "-rtmp-play")
	assert.Contains(t, sb.String(), "rtmp://<host>:1935/live/<name>")
}

// A stream whose RTMP listen path is its own play path is reported: the
// registry never plays a publish path.
func TestOsdevRTMPPlayPathWarning(t *testing.T) {
	var errw strings.Builder
	warnPublishPaths(&errw, &config{
		rtmpPlay: ":19351", rtmpPlayApp: "live",
		sources: []*source{
			{name: "cam", inputs: []string{"rtmp://:19351/live/cam?mode=listen"}},
			{name: "ok", inputs: []string{"rtmp://:19351/live/" + vSekrit + "?mode=listen"}},
			{name: "other", inputs: []string{"rtmp://:19352/live/other?mode=listen"}},
			{name: "pull", inputs: []string{"rtmp://example.com/live/pull"}},
		},
	})
	out := errw.String()
	assert.Contains(t, out, "osdev: warning: stream cam cannot be played over RTMP")
	assert.Equal(t, 1, strings.Count(out, "\n"), out)
	assert.NotContains(t, out, vSekrit)
}

// Every stream is served to RTMP players on -rtmp-play, on the port of the
// RTMP listen inputs too, and a publish path is never playable.
func TestOsdevRTMPPlay(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	var errw syncBuilder
	_, stop := startOsdev(t, []string{
		"-rtmp-play", fmt.Sprintf(":%d", port),
		"-stream", "demo=" + fixture, "-loop",
		"-stream", fmt.Sprintf("cam=rtmp://:%d/live/cam?mode=listen", port),
	}, &errw)
	assert.Contains(t, errw.String(), "osdev: warning: stream cam cannot be played over RTMP")

	var c *rtmp.ClientConn
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err = rtmp.Play(ctx, fmt.Sprintf("rtmp://127.0.0.1:%d/live/demo", port))
		return err == nil
	}, 15*time.Second, 100*time.Millisecond)
	video := 0
	for video < 10 {
		m, err := c.ReadMedia()
		require.NoError(t, err)
		if m.Type == rtmp.MsgVideo {
			video++
		}
	}
	_ = c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = rtmp.Play(ctx, fmt.Sprintf("rtmp://127.0.0.1:%d/live/cam", port))
	var st *rtmp.StatusError
	require.ErrorAs(t, err, &st)
	assert.Equal(t, "NetStream.Play.StreamNotFound", st.Code)
	assert.Equal(t, 0, stop())
}

func TestOsdevRTSPPlayFlags(t *testing.T) {
	cfg, code := parseArgs([]string{"-stream", "a=" + fixture}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Empty(t, cfg.rtspPlay, "off by default")
	assert.Equal(t, "live", cfg.rtspPlayApp)
	assert.False(t, cfg.rtspUDP)

	cfg, code = parseArgs([]string{"-rtsp-play", ":19354", "-rtsp-play-app", "/tv/", "-rtsp-udp", "-stream", "a=" + fixture}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Equal(t, ":19354", cfg.rtspPlay)
	assert.Equal(t, "tv", cfg.rtspPlayApp)
	assert.True(t, cfg.rtspUDP)

	for name, tc := range map[string]struct {
		args []string
		flag string
	}{
		"no port":          {[]string{"-rtsp-play", "127.0.0.1", "-stream", "a=" + fixture}, "-rtsp-play"},
		"bad port":         {[]string{"-rtsp-play", ":0", "-stream", "a=" + fixture}, "-rtsp-play"},
		"empty app":        {[]string{"-rtsp-play", ":19354", "-rtsp-play-app", "/", "-stream", "a=" + fixture}, "-rtsp-play-app"},
		"udp without play": {[]string{"-rtsp-udp", "-stream", "a=" + fixture}, "-rtsp-udp"},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(tc.args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.Contains(t, errw.String(), tc.flag)
		})
	}

	var sb strings.Builder
	assert.Equal(t, 2, run(context.Background(), nil, io.Discard, &sb))
	assert.Contains(t, sb.String(), "-rtsp-play")
	assert.Contains(t, sb.String(), "-rtsp-udp")
	assert.Contains(t, sb.String(), "rtsp://<host>:8554/live/<name>")
}

// Every stream is served to RTSP players on -rtsp-play, over TCP and, with
// -rtsp-udp, over UDP; an unknown name is answered 404.
func TestOsdevRTSPPlay(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	_, stop := startOsdev(t, []string{
		"-rtsp-play", fmt.Sprintf("127.0.0.1:%d", port), "-rtsp-udp",
		"-stream", "demo=" + fixture, "-loop",
	}, io.Discard)
	url := fmt.Sprintf("rtsp://127.0.0.1:%d/live/demo", port)
	for _, transport := range []string{"tcp", "udp"} {
		var c *rtsp.Client
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err = rtsp.Dial(ctx, url, rtsp.Config{Transport: transport})
			return err == nil
		}, 15*time.Second, 100*time.Millisecond, transport)
		// Even right at the start, DESCRIBE waits for the audio.
		assert.Len(t, c.Description().Media, 2, transport)
		for rtpPkts := 0; rtpPkts < 50; {
			p, err := c.ReadPacket()
			require.NoError(t, err, transport)
			if p.RTP != nil {
				rtpPkts++
			}
		}
		_ = c.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = rtsp.Dial(ctx, fmt.Sprintf("rtsp://127.0.0.1:%d/live/nope", port), rtsp.Config{})
	var st *rtsp.StatusError
	require.ErrorAs(t, err, &st)
	assert.Equal(t, 404, st.Code)
	assert.Equal(t, 0, stop())
}

func TestOsdevRTMPPushFlags(t *testing.T) {
	key := vSekrit + "pushkey"
	cfg, code := parseArgs([]string{
		"-stream", "a=" + fixture, "-stream", "b=" + fixture,
		"-rtmp-push", "a=rtmp://127.0.0.1:19352/live/" + key + "?key=" + vHunter,
		"-rtmp-push", "a=rtmp://127.0.0.1:19353/app/" + key,
		"-rtmp-push", "b=rtmp://127.0.0.1:19352/live/" + key,
	}, io.Discard)
	require.NotNil(t, cfg, code)
	require.Len(t, cfg.pushes, 3)
	assert.Equal(t, "a", cfg.pushes[0].stream)
	assert.Equal(t, "rtmp://127.0.0.1:19352/live/sexxxxx?key=xxxxx", cfg.pushes[0].pusher.String())
	assert.Equal(t, "b", cfg.pushes[2].stream)

	for name, args := range map[string][]string{
		"unknown stream": {"-stream", "a=" + fixture, "-rtmp-push", "nope=rtmp://h/live/" + key},
		"no stream":      {"-stream", "a=" + fixture, "-rtmp-push", "rtmp://h/live/" + key},
		"not rtmp":       {"-stream", "a=" + fixture, "-rtmp-push", "a=srt://h:1?passphrase=" + key},
		"no key":         {"-stream", "a=" + fixture, "-rtmp-push", "a=rtmp://h/" + key},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.Contains(t, errw.String(), "-rtmp-push")
			assert.NotContains(t, errw.String(), key)
		})
	}

	var sb strings.Builder
	assert.Equal(t, 2, run(context.Background(), nil, io.Discard, &sb))
	assert.Contains(t, sb.String(), "-rtmp-push")
}

// Each -rtmp-push publishes its stream to the target, which gets the key,
// while osdev's output shows the target only redacted.
func TestOsdevRTMPPush(t *testing.T) {
	key := vSekrit + "pushkey"
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	var errw syncBuilder
	_, stop := startOsdev(t, []string{
		"-stream", "demo=" + fixture, "-loop",
		"-rtmp-push", "demo=" + srv.URL("live/"+key+"?mode=x"),
	}, &errw)
	require.Eventually(t, func() bool {
		video := 0
		for _, m := range srv.Media() {
			if m.Type == rtmp.MsgVideo {
				video++
			}
		}
		return video >= 30
	}, 15*time.Second, 50*time.Millisecond)
	assert.Equal(t, 0, stop())
	reqs := srv.Requests()
	require.NotEmpty(t, reqs)
	assert.Equal(t, key, reqs[0].Stream)
	assert.Equal(t, "mode=x", reqs[0].Query)
	out := errw.String()
	assert.Contains(t, out, "osdev: stream demo: pushing to rtmp://"+srv.Addr+"/live/sexxxxx?mode=x")
	assert.Contains(t, out, "push rtmp://"+srv.Addr+"/live/sexxxxx?mode=x: stopped, connects 1")
	assert.NotContains(t, out, key)
}

func TestOsdevSRTPlayFlags(t *testing.T) {
	cfg, code := parseArgs([]string{"-stream", "a=" + fixture}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Empty(t, cfg.srtPlay, "off by default")
	assert.Equal(t, "live", cfg.srtPlayApp)
	assert.Equal(t, 120*time.Millisecond, cfg.srtLatency)

	cfg, code = parseArgs([]string{"-srt-play", ":19355", "-srt-play-app", "/tv/", "-srt-latency", "300ms", "-stream", "a=" + fixture}, io.Discard)
	require.NotNil(t, cfg, code)
	assert.Equal(t, ":19355", cfg.srtPlay)
	assert.Equal(t, "tv", cfg.srtPlayApp)
	assert.Equal(t, 300*time.Millisecond, cfg.srtLatency)

	// Another port, or another specific host, does not collide with a
	// listen input; nor does an SRT caller input.
	for _, tc := range [][2]string{
		{":19356", "srt://:19355?mode=listener"},
		{"127.0.0.1:19355", "srt://127.0.0.2:19355?mode=listener"},
		{"[::1]:19355", "srt://127.0.0.1:19355?mode=listener"},
		{":19355", "srt://example.com:19355"},
	} {
		cfg, code = parseArgs([]string{"-srt-play", tc[0], "-stream", "a=" + tc[1]}, io.Discard)
		require.NotNil(t, cfg, "%s with %s: %d", tc[0], tc[1], code)
	}

	for name, tc := range map[string]struct {
		args []string
		flag string
	}{
		"no port":        {[]string{"-srt-play", "127.0.0.1", "-stream", "a=" + fixture}, "-srt-play"},
		"bad port":       {[]string{"-srt-play", ":0", "-stream", "a=" + fixture}, "-srt-play"},
		"empty app":      {[]string{"-srt-play", ":19355", "-srt-play-app", "/", "-stream", "a=" + fixture}, "-srt-play-app"},
		"zero latency":   {[]string{"-srt-play", ":19355", "-srt-latency", "0s", "-stream", "a=" + fixture}, "-srt-latency"},
		"huge latency":   {[]string{"-srt-play", ":19355", "-srt-latency", "70s", "-stream", "a=" + fixture}, "-srt-latency"},
		"same address":   {[]string{"-srt-play", ":19355", "-stream", "a=srt://:19355?mode=listener"}, "-srt-play"},
		"wildcard alias": {[]string{"-srt-play", "0.0.0.0:19355", "-stream", "b=" + fixture, "-stream", "a=srt://:19355?mode=listener"}, "-srt-play"},
		"ipv6 wildcard":  {[]string{"-srt-play", "[::]:19355", "-stream", "a=srt://0.0.0.0:19355?mode=listener"}, "-srt-play"},
		"same host":      {[]string{"-srt-play", "127.0.0.1:19355", "-stream", "a=srt://127.0.0.1:19355?mode=listener"}, "-srt-play"},
		// SO_REUSEADDR lets a wildcard and a specific address on one port
		// both bind, and the kernel then splits the traffic.
		"specific in wildcard": {[]string{"-srt-play", "127.0.0.1:19355", "-stream", "a=srt://:19355?mode=listener"}, "-srt-play"},
		"wildcard over host":   {[]string{"-srt-play", ":19355", "-stream", "a=srt://127.0.0.1:19355?mode=listener"}, "-srt-play"},
		"ipv6 loopback":        {[]string{"-srt-play", "[::1]:19355", "-stream", "a=srt://[::]:19355?mode=listener"}, "-srt-play"},
		"ipv6 under wildcard":  {[]string{"-srt-play", "0.0.0.0:19355", "-stream", "a=srt://[::1]:19355?mode=listener"}, "-srt-play"},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(tc.args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.Contains(t, errw.String(), tc.flag)
		})
	}

	var sb strings.Builder
	assert.Equal(t, 2, run(context.Background(), nil, io.Discard, &sb))
	assert.Contains(t, sb.String(), "-srt-play")
	assert.Contains(t, sb.String(), "-srt-latency")
	assert.Contains(t, sb.String(), "srt://<host>:9000?streamid=live/<name>")
}

// Every stream is served to SRT receivers on -srt-play; an unknown name is
// refused.
func TestOsdevSRTPlay(t *testing.T) {
	pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, pc.Close())

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	_, stop := startOsdev(t, []string{"-srt-play", addr, "-stream", "demo=" + fixture, "-loop"}, io.Discard)
	var c srt.Conn
	require.Eventually(t, func() bool {
		cfg := srt.DefaultConfig()
		cfg.StreamId = "#!::r=live/demo,m=request"
		c, err = srt.Dial("srt", addr, cfg)
		return err == nil
	}, 15*time.Second, 100*time.Millisecond)
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Positive(t, n)
	assert.Equal(t, byte(0x47), buf[0])
	_ = c.Close()

	cfg := srt.DefaultConfig()
	cfg.StreamId = "live/nope"
	_, err = srt.Dial("srt", addr, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection rejected")
	assert.Equal(t, 0, stop())
}

func TestOsdevLadderFlags(t *testing.T) {
	cfg, code := parseArgs([]string{
		"-stream", "a=" + fixture, "-stream", "b=" + fixture, "-ostranscoder", "/bin/true",
		"-rung", "a=t240:240:600", "-rung", "a=src:copy", "-rung", "b=low:120:150",
		"-audio", "a=aac:96", "-audio", "b=copy",
		"-rtmp-push", "a_t240=rtmp://127.0.0.1:19352/live/key",
	}, io.Discard)
	require.NotNil(t, cfg, code)
	require.Len(t, cfg.transcoders, 2)
	assert.Equal(t, "a", cfg.transcoders[0].stream)
	assert.Equal(t, "b", cfg.transcoders[1].stream)
	names := make([]string, 0, len(cfg.renditions))
	for _, s := range cfg.renditions {
		names = append(names, s.name)
		assert.NotNil(t, s.hub)
		assert.NotNil(t, s.pack)
	}
	assert.Equal(t, []string{"a_t240", "a_src", "b_low"}, names)
	assert.Same(t, cfg.transcoders[0].tr.Hubs()[0], cfg.renditions[0].hub)
	require.Len(t, cfg.pushes, 1)
	assert.Equal(t, "a_t240", cfg.pushes[0].stream, "a rendition can be pushed")

	rung, label, video, err := parseRung("a=t240:240:600")
	require.NoError(t, err)
	assert.Equal(t, "a", rung)
	assert.Equal(t, "t240", label)
	assert.Equal(t, transcode.Video{Height: 240, BitrateKbps: 600}, video)
	for v, want := range map[string]transcode.Audio{
		"a=copy": {Copy: true}, "a=aac": {}, "a=aac:96": {BitrateKbps: 96},
	} {
		_, a, err := parseAudio(v)
		require.NoError(t, err, v)
		assert.Equal(t, want, a, v)
	}

	copyOnly, code := parseArgs([]string{"-stream", "a=" + fixture, "-rung", "a=src:copy", "-audio", "a=copy"}, io.Discard)
	require.NotNil(t, copyOnly, code, "copies need no transcoder")
	assert.Len(t, copyOnly.renditions, 1)

	for name, args := range map[string][]string{
		"unknown stream":   {"-stream", "a=" + fixture, "-ostranscoder", "/bin/true", "-rung", "b=x:240:600"},
		"rung of rung":     {"-stream", "a=" + fixture, "-ostranscoder", "/bin/true", "-rung", "a=x:240:600", "-rung", "a_x=y:120:100"},
		"no equals":        {"-stream", "a=" + fixture, "-rung", "x:240:600"},
		"bad label":        {"-stream", "a=" + fixture, "-rung", "a=x/y:copy"},
		"empty label":      {"-stream", "a=" + fixture, "-rung", "a=:copy"},
		"zero height":      {"-stream", "a=" + fixture, "-ostranscoder", "/bin/true", "-rung", "a=x:0:600"},
		"bad kbps":         {"-stream", "a=" + fixture, "-ostranscoder", "/bin/true", "-rung", "a=x:240:fast"},
		"two fields":       {"-stream", "a=" + fixture, "-rung", "a=x:240"},
		"duplicate label":  {"-stream", "a=" + fixture, "-rung", "a=x:copy", "-rung", "a=x:copy", "-audio", "a=copy"},
		"name collision":   {"-stream", "a=" + fixture, "-stream", "a_x=" + fixture, "-rung", "a=x:copy", "-audio", "a=copy"},
		"no transcoder":    {"-stream", "a=" + fixture, "-rung", "a=x:240:600"},
		"aac no binary":    {"-stream", "a=" + fixture, "-rung", "a=x:copy"},
		"audio no rung":    {"-stream", "a=" + fixture, "-audio", "a=copy"},
		"bad audio":        {"-stream", "a=" + fixture, "-rung", "a=x:copy", "-audio", "a=mp3"},
		"bad audio kbps":   {"-stream", "a=" + fixture, "-rung", "a=x:copy", "-audio", "a=aac:0"},
		"audio no equals":  {"-stream", "a=" + fixture, "-rung", "a=x:copy", "-audio", "copy"},
		"audio bad stream": {"-stream", "a=" + fixture, "-rung", "a=x:copy", "-audio", "b=copy"},
		"odd height":       {"-stream", "a=" + fixture, "-ostranscoder", "/bin/true", "-rung", "a=x:241:600"},
		"copy with kbps":   {"-stream", "a=" + fixture, "-rung", "a=x:copy:300", "-audio", "a=copy"},
		"audio twice":      {"-stream", "a=" + fixture, "-rung", "a=x:copy", "-audio", "a=copy", "-audio", "a=copy"},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(args, &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.NotEmpty(t, errw.String())
			t.Log(strings.TrimSpace(errw.String()))
		})
	}
}

// waitBody waits until the body at u contains want.
func waitBody(t *testing.T, u, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := httpGet(u)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == 200 && strings.Contains(string(b), want)
	}, 20*time.Second, 50*time.Millisecond, u)
}

// A ladder of copies runs without the transcoder child: the rendition is
// served as <name>_<label> and ends with its stream.
func TestOsdevCopyLadder(t *testing.T) {
	var errw syncBuilder
	addr, stop := startOsdev(t, []string{
		"-stream", "v=../../testdata/fixtures/h264_640.ts", "-rung", "v=src:copy", "-audio", "v=copy",
	}, &errw)
	waitBody(t, "http://"+addr+"/live/v_src/hls/index.m3u8", "#EXT-X-ENDLIST")
	assert.Equal(t, 0, stop())
	out := errw.String()
	assert.Contains(t, out, "osdev: stream v: renditions ended with the stream")
	assert.Contains(t, out, "osdev: stream v: transcoder pid 0, spawns 0, exits 0")
}

// The real transcoder child encodes a rung, served on HTTP like any stream.
func TestOsdevLadder(t *testing.T) {
	bin := testpeer.OSTranscoder(t)
	var errw syncBuilder
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, []string{
			"-listen", "127.0.0.1:0", "-stream", "demo=" + fixture, "-loop", "-ostranscoder", bin,
			"-rung", "demo=t180:180:300", "-rung", "demo=src:copy", "-audio", "demo=aac:96",
		}, pw, &errw)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) == 13 {
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	require.Len(t, lines, 13)
	addr := strings.TrimPrefix(lines[0], "listening on ")
	assert.Equal(t, "http://"+addr+"/live/demo_t180/hls/index.m3u8", lines[5])
	assert.Equal(t, "http://"+addr+"/live/demo_src/stream.ts", lines[12])

	waitBody(t, "http://"+addr+"/live/demo_t180/hls/index.m3u8", ".ts")
	waitBody(t, "http://"+addr+"/live/demo_src/hls/index.m3u8", ".ts")
	cancel()
	select {
	case c := <-code:
		assert.Equal(t, 0, c, errw.String())
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
	out := errw.String()
	assert.Contains(t, out, "osdev: stream demo: transcoder pid 0, spawns 1, exits 0")
	assert.NotContains(t, out, "stopped")
}
