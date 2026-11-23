package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/flv"
	rtpmux "github.com/ntt0601zcoder/open-streamer/internal/mux/rtp"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/srtout"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// serve packages the h264_aac fixture and serves it as stream "s". edit,
// when non-nil, rewrites the file whose path ends in suffix.
func serve(t *testing.T, suffix string, edit func([]byte) []byte) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("pkg", 1<<16)
	now := func() time.Time { return t0 }
	// A failed input would be retried without end: fail at once instead.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, pipeline.New(h, pipeline.Options{
		Inputs: []string{"mem"},
		Open: func(context.Context, string) (ingest.Reader, error) {
			return ingest.NewTSReader(io.NopCloser(bytes.NewReader(b)), now)
		},
		Parse:    func(string) (ingest.Spec, error) { return ingest.Spec{}, nil },
		EndOnEOF: true,
		Now:      now,
		OnEvent: func(ev pipeline.Event) {
			if ev.Kind == pipeline.EventFailed {
				t.Errorf("pipeline input failed: %v", ev.Err)
				cancel()
			}
		},
	}).Run(ctx))
	h.Close()
	p := live.NewPackager(h, live.Options{Window: 100, Now: func() time.Time { return t0.Add(time.Hour) }})
	p.Run(context.Background(), sub)
	next := live.Handler(func(n string) (*live.Packager, bool) { return p, n == "s" })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if edit == nil || !strings.HasSuffix(r.URL.Path, suffix) {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		w.Header().Set("Content-Type", rec.Header().Get("Content-Type"))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(edit(rec.Body.Bytes()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/live/s/"
}

func runConform(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var sb strings.Builder
	code := run(context.Background(), args, &sb)
	return code, sb.String()
}

func TestOsconformExitCodes(t *testing.T) {
	base := serve(t, "", nil)
	for _, p := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd"} {
		code, out := runConform(t, "-url", base+p, "-once")
		assert.Equal(t, 0, code, p)
		assert.Empty(t, out, p)
	}

	bad := serve(t, "cmaf/index.mpd", func(b []byte) []byte {
		m, err := dash.Parse(b)
		require.NoError(t, err)
		m.Periods[0].AdaptationSets[0].Representations[0].Codecs = "avc1.4d401e"
		return m.Encode()
	})
	code, out := runConform(t, "-url", bad+"cmaf/index.mpd", "-once")
	assert.Equal(t, 1, code)
	assert.Equal(t, 1, strings.Count(out, "\n"), out)

	bad = serve(t, "cmaf/master.m3u8", func(b []byte) []byte {
		m, err := hls.ParseMultivariant(b)
		require.NoError(t, err)
		m.Variants[0].Codecs = "avc1.4d401e,mp4a.40.2"
		return m.Encode()
	})
	code, _ = runConform(t, "-url", bad+"cmaf/master.m3u8", "-once")
	assert.Equal(t, 1, code)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "../../testdata/fixtures/h264_aac.ts")
	}))
	defer srv.Close()
	code, _ = runConform(t, "-url", srv.URL+"/x.ts")
	assert.Equal(t, 0, code)

	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	for _, ext := range []string{"x.m3u8", "x.mpd", "x.ts"} {
		code, _ = runConform(t, "-url", url+"/"+ext, "-once")
		assert.Equal(t, 2, code, ext)
	}
}

// A server that never answers is a read error after -timeout, not a hang.
func TestOsconformTimeout(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stop)
	for _, ext := range []string{"x.m3u8", "x.ts"} {
		start := time.Now()
		code, _ := runConform(t, "-url", srv.URL+"/"+ext, "-once", "-timeout", "200ms")
		assert.Equal(t, 2, code, ext)
		assert.Less(t, time.Since(start), 5*time.Second, ext)
	}
}

func TestOsconformBadArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"no url":      {},
		"bad flag":    {"-nope"},
		"extra arg":   {"-url", "http://x/a.ts", "y"},
		"bad suffix":  {"-url", "http://x/a.txt"},
		"bad scheme":  {"-url", "rtmps://x/live/s"},
		"bad url":     {"-url", "http://a b/a.ts"},
		"bad duratio": {"-url", "http://x/a.ts", "-duration", "soon"},
		"bad timeout": {"-url", "http://x/a.ts", "-timeout", "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := runConform(t, args...)
			assert.Equal(t, 2, code)
			assert.Empty(t, out)
		})
	}
}

// rtmpTags returns the h264_aac fixture as our RTMP muxer sends it. When
// dropKey is set, the first video keyframe is left out.
func rtmpTags(t *testing.T, dropKey bool) []rtmp.FLVTag {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(b))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	m := flv.NewMuxer()
	var tags []rtmp.FLVTag
	dropped := false
	add := func(frames []media.Frame) {
		for _, f := range frames {
			msgs, err := m.Frame(f)
			require.NoError(t, err)
			for _, msg := range msgs {
				if dropKey && !dropped && msg.Type == rtmp.MsgVideo {
					v, err := rtmp.ParseVideoTag(msg.Payload)
					require.NoError(t, err)
					if v.Kind == rtmp.VideoCodedFrames && v.Key {
						dropped = true
						continue
					}
				}
				tags = append(tags, rtmp.FLVTag{Type: msg.Type, Timestamp: msg.Timestamp, Data: msg.Payload})
			}
		}
	}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		add(c.Push(f))
	}
	add(c.Flush())
	return tags
}

func TestOsconformRTMP(t *testing.T) {
	s := rtmptest.NewPlayServer(t, rtmpTags(t, false), rtmptest.PlayOptions{})
	code, out := runConform(t, "-url", s.URL("live/s"))
	assert.Equal(t, 0, code)
	assert.Empty(t, out)

	s = rtmptest.NewPlayServer(t, rtmpTags(t, true), rtmptest.PlayOptions{})
	code, out = runConform(t, "-url", s.URL("live/s"))
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "epoch-starts-on-key")

	s = rtmptest.NewPlayServer(t, nil, rtmptest.PlayOptions{NotFound: true})
	code, _ = runConform(t, "-url", s.URL("live/s"))
	assert.Equal(t, 2, code)

	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	code, _ = runConform(t, "-url", "rtmp://"+addr+"/live/s")
	assert.Equal(t, 2, code)
}

// rtspClip plays the h264_aac fixture through our RTP muxer; noSR withholds
// the sender reports.
type rtspClip struct {
	frames []media.Frame
	descs  []rtsp.TrackDesc
	noSR   bool
}

func newRTSPClip(t *testing.T, noSR bool) *rtspClip {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(b))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var all []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		all = append(all, c.Push(f)...)
	}
	all = append(all, c.Flush()...)
	clip := &rtspClip{noSR: noSR}
	var cfgs []hub.TrackConfig
	for _, f := range all {
		if len(clip.frames) == 0 && (f.Kind() != media.KindVideo || !f.Key) {
			continue
		}
		clip.frames = append(clip.frames, f)
		seen := false
		for _, c := range cfgs {
			seen = seen || c.Track == f.Track
		}
		if !seen {
			cfgs = append(cfgs, hub.TrackConfig{Track: f.Track, Kind: f.Kind(), Config: f.Config})
		}
	}
	clip.descs, err = rtpmux.Tracks(cfgs)
	require.NoError(t, err)
	return clip
}

func (c *rtspClip) Describe(path string) ([]rtsp.TrackDesc, bool) { return c.descs, path == "live/s" }

func (c *rtspClip) Play(ctx context.Context, s *rtsp.ServerSession) {
	trs := make([]rtpmux.Track, 0, len(c.descs))
	for i, d := range c.descs {
		ssrc, _, base := s.Origin(i)
		trs = append(trs, rtpmux.Track{SDPIndex: i, HubTrack: d.ID, Desc: d, Setup: true, SSRC: ssrc, TSBase: base})
	}
	var now time.Time
	m := rtpmux.NewMuxer(trs, 0, func() time.Time { return now })
	for _, f := range c.frames {
		if ctx.Err() != nil {
			return
		}
		now = t0.Add(time.Duration(f.DTS-c.frames[0].DTS) * time.Second / media.ClockRate)
		p, reps, err := m.Frame(f)
		if err != nil || (p != nil && s.WritePackets(p.SDPIndex, p.RTP) != nil) {
			return
		}
		for _, r := range reps {
			if !c.noSR && s.WriteRTCP(r.SDPIndex, r.RTCP) != nil {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	s.Close()
}

func serveRTSP(t *testing.T, h rtsp.Handler) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := rtsp.NewServer(h, rtsp.ServerConfig{})
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return "rtsp://" + ln.Addr().String() + "/live/s"
}

func TestOsconformRTSP(t *testing.T) {
	code, out := runConform(t, "-url", serveRTSP(t, newRTSPClip(t, false)), "-timeout", "10s")
	assert.Equal(t, 0, code, out)
	assert.Empty(t, out)

	code, out = runConform(t, "-url", serveRTSP(t, newRTSPClip(t, true)), "-timeout", "10s")
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "rtsp-sr-timely")

	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	code, _ = runConform(t, "-url", "rtsp://"+addr+"/live/s", "-timeout", "2s")
	assert.Equal(t, 2, code)
}

// srtPlay serves the h264_aac fixture's first GOP on an SRT output as
// stream "s", and the rest paced once a viewer is subscribed. It returns the
// listener address.
func srtPlay(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(b))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var frames []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		frames = append(frames, c.Push(f)...)
	}
	frames = append(frames, c.Flush()...)
	split, keys := len(frames), 0
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			if keys++; keys == 2 {
				split = i
				break
			}
		}
	}
	h := hub.New(hub.Options{})
	srv := srtout.New(func(name string) (*hub.Hub, bool) { return h, name == "s" }, srtout.Options{})
	require.NoError(t, srv.Listen("127.0.0.1:0"))
	go func() { _ = srv.Serve() }()
	for _, f := range frames[:split] {
		h.Write(f)
	}
	go func() {
		// Feed once the viewer is subscribed, paced by DTS at 4x.
		for len(h.Stats()) == 0 {
			time.Sleep(time.Millisecond)
		}
		start, base := time.Now(), frames[split].DTS
		for _, f := range frames[split:] {
			time.Sleep(time.Until(start.Add(time.Duration(float64(f.DTS-base) / media.ClockRate / 4 * float64(time.Second)))))
			h.Write(f)
		}
		h.Close()
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		h.Close()
	})
	return srv.Addr().String()
}

func TestOsconformSRT(t *testing.T) {
	addr := srtPlay(t)
	code, out := runConform(t, "-url", "srt://"+addr+"?streamid=live/s&latency=500", "-timeout", "10s")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)

	// A refused stream ID.
	code, _ = runConform(t, "-url", "srt://"+addr+"?streamid=live/nope", "-timeout", "10s")
	assert.Equal(t, 2, code)

	// A stream with packets missing: continuity gaps.
	b, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	const pkt = 188
	require.Greater(t, len(b), 110*pkt)
	b = append(b[:100*pkt:100*pkt], b[103*pkt:]...)
	cfg := srt.DefaultConfig()
	ln, err := srt.Listen("srt", "127.0.0.1:0", cfg)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		conn, err := req.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for data := b; len(data) > 0; data = data[min(len(data), 1316):] {
			if _, err := conn.Write(data[:min(len(data), 1316)]); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
	}()
	code, out = runConform(t, "-url", "srt://"+ln.Addr().String()+"?streamid=x", "-timeout", "10s")
	ln.Close()
	<-done
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "ts-cc-continuous")

	// Nothing listens.
	l2, err := srt.Listen("srt", "127.0.0.1:0", srt.DefaultConfig())
	require.NoError(t, err)
	dead := l2.Addr().String()
	l2.Close()
	code, _ = runConform(t, "-url", "srt://"+dead+"?streamid=x", "-timeout", "1s")
	assert.Equal(t, 2, code)
}
