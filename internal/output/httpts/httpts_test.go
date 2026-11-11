package httpts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

const waitFor = 5 * time.Second

// timed returns a fixture's frames in stream time: demuxed, then through the
// Clock, which shifts source time to non-negative stream time with epochs.
func timed(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/fixtures/" + name)
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
	return append(out, c.Flush()...)
}

// configOf returns the first config of the given codec in a fixture.
func configOf(t *testing.T, name string, codec media.Codec) *media.TrackConfig {
	t.Helper()
	for _, f := range timed(t, name) {
		if f.Config != nil && f.Config.Codec == codec {
			return f.Config
		}
	}
	t.Fatalf("%s has no %s track", name, codec)
	return nil
}

// synth returns n video frames on track 0, one every 40 ms from start, every
// one a keyframe carrying payload bytes.
func synth(cfg *media.TrackConfig, start int64, n, payload int) []media.Frame {
	data := make([]byte, payload)
	data[0] = 0x65 // H.264 IDR slice header byte; the muxer does not parse it
	out := make([]media.Frame, n)
	for i := range out {
		dts := start + int64(i)*3600
		out[i] = media.Frame{
			Track: 0, Config: cfg, PTS: dts, DTS: dts, Key: true,
			Data: [][]byte{data}, Epoch: 1,
		}
	}
	return out
}

// server serves h as stream "s".
func server(t *testing.T, h *hub.Hub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(lookupOne(h), 0))
	t.Cleanup(srv.Close)
	return srv
}

func request(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	require.NoError(t, err)
	return req
}

func lookupOne(h *hub.Hub) func(string) (*hub.Hub, bool) {
	return func(name string) (*hub.Hub, bool) {
		if name == "s" {
			return h, true
		}
		return nil, false
	}
}

// result is a finished GET: status, header and the whole body.
type result struct {
	status int
	header http.Header
	body   []byte
	err    error
}

// startGet issues the GET in a goroutine and reads the body to the end.
func startGet(ctx context.Context, client *http.Client, url string) <-chan result {
	out := make(chan result, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			out <- result{err: err}
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			out <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		out <- result{status: resp.StatusCode, header: resp.Header, body: body, err: err}
	}()
	return out
}

// waitSubscribed waits until the handler has subscribed to h.
func waitSubscribed(t *testing.T, h *hub.Hub) {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, waitFor, time.Millisecond)
}

// waitUnsubscribed waits until no subscriber is left on h.
func waitUnsubscribed(t *testing.T, h *hub.Hub) {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.Stats()) == 0 }, waitFor, time.Millisecond)
}

func wait(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitFor):
		t.Fatal("GET did not finish")
		return result{}
	}
}

// stream serves frames through a fresh hub and returns the whole response.
func stream(t *testing.T, frames []media.Frame) result {
	t.Helper()
	h := hub.New(hub.Options{})
	srv := server(t, h)
	ch := startGet(context.Background(), srv.Client(), srv.URL+"/live/s/stream.ts")
	waitSubscribed(t, h)
	for _, f := range frames {
		h.Write(f)
	}
	h.Close()
	r := wait(t, ch)
	require.NoError(t, r.err)
	require.Equal(t, http.StatusOK, r.status)
	return r
}

// readBack demuxes a response body, following PMT changes.
func readBack(t *testing.T, body []byte) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(body), ts.DemuxOptions{FollowPMT: true})
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

func countKind(frames []media.Frame, k media.Kind) int {
	n := 0
	for _, f := range frames {
		if f.Kind() == k {
			n++
		}
	}
	return n
}

func TestHTTPTSStreams(t *testing.T) {
	frames := timed(t, "h264_aac.ts")
	r := stream(t, frames)
	assert.Equal(t, "video/mp2t", r.header.Get("Content-Type"))

	got := readBack(t, r.body)
	require.NotEmpty(t, got)
	assert.Nil(t, conform.CheckFrames(got, conform.FrameOptions{}))
	assert.Equal(t, countKind(frames, media.KindVideo), countKind(got, media.KindVideo))
	assert.Equal(t, countKind(frames, media.KindAudio), countKind(got, media.KindAudio))

	// The demuxer reports source time on its own origin, so compare the
	// video DTS steps frame for frame.
	steps := func(frames []media.Frame) []int64 {
		var out []int64
		first := int64(-1 << 62)
		for _, f := range frames {
			if f.Track != 0 {
				continue
			}
			if first == -1<<62 {
				first = f.DTS
			}
			out = append(out, f.DTS-first)
		}
		return out
	}
	assert.Equal(t, steps(frames), steps(got))
}

func TestHTTPTSInBandConfigChange(t *testing.T) {
	frames := timed(t, "h264_aac_then_640.ts")
	r := stream(t, frames)
	got := readBack(t, r.body)

	var in, out []media.Frame
	for _, f := range frames {
		if f.Kind() == media.KindVideo {
			in = append(in, f)
		}
	}
	for _, f := range got {
		if f.Kind() == media.KindVideo {
			out = append(out, f)
		}
	}
	require.Len(t, out, len(in), "no video frame lost across the SPS change")
	changed := false
	for i, f := range out {
		assert.Equal(t, in[i].Config.Width, f.Config.Width, "frame %d", i)
		assert.Equal(t, in[i].Config.Height, f.Config.Height, "frame %d", i)
		if in[i].Config.Width == 640 {
			changed = true
			assert.Equal(t, 360, f.Config.Height)
		}
	}
	assert.True(t, changed)
	assert.Equal(t, 320, out[0].Config.Width)
	assert.Equal(t, 640, out[len(out)-1].Config.Width)
}

func TestHTTPTSAudioJoinsLate(t *testing.T) {
	all := timed(t, "h264_aac.ts")
	var v0 int64 = -1
	for _, f := range all {
		if f.Track == 0 {
			v0 = f.DTS
			break
		}
	}
	require.GreaterOrEqual(t, v0, int64(0))
	var frames []media.Frame
	audio := 0
	for _, f := range all {
		if f.Track == 1 && f.DTS < v0+180000 {
			continue
		}
		f.Epoch = 1
		if f.Track == 1 {
			audio++
		}
		frames = append(frames, f)
	}
	require.Positive(t, audio)

	r := stream(t, frames)
	got := readBack(t, r.body)
	gotAudio := 0
	for _, f := range got {
		if f.Kind() == media.KindAudio {
			gotAudio++
			assert.Equal(t, 1, f.Track)
		}
	}
	assert.Equal(t, audio, gotAudio)
	assert.Equal(t, countKind(frames, media.KindVideo), countKind(got, media.KindVideo))
}

func TestHTTPTSClientGoneUnsubscribes(t *testing.T) {
	t.Run("while streaming", func(t *testing.T) {
		h := hub.New(hub.Options{})
		defer h.Close()
		srv := server(t, h)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/live/s/stream.ts", nil)
		require.NoError(t, err)
		type got struct {
			resp *http.Response
			err  error
		}
		ch := make(chan got, 1)
		go func() {
			resp, err := srv.Client().Do(req) //nolint:bodyclose // closed below
			ch <- got{resp, err}
		}()
		waitSubscribed(t, h)
		for _, f := range synth(configOf(t, "h264_aac.ts", media.CodecH264), 0, 50, 100) {
			h.Write(f)
		}
		var g got
		select {
		case g = <-ch:
		case <-time.After(waitFor):
			t.Fatal("no response")
		}
		require.NoError(t, g.err)
		defer func() { _ = g.resp.Body.Close() }()
		// Frame 49 is complete once frame 50 arrives, and frame 50 is sent
		// by its flush: the handler is then idle, waiting for deliveries.
		d, err := ts.NewDemuxer(g.resp.Body)
		require.NoError(t, err)
		for range 49 {
			_, err := d.Read()
			require.NoError(t, err)
		}
		cancel()
		waitUnsubscribed(t, h)
	})
	t.Run("while buffering", func(t *testing.T) {
		h := hub.New(hub.Options{})
		defer h.Close()
		srv := server(t, h)
		ctx, cancel := context.WithCancel(context.Background())
		ch := startGet(ctx, srv.Client(), srv.URL+"/live/s/stream.ts")
		waitSubscribed(t, h)
		for _, f := range synth(configOf(t, "h264_aac.ts", media.CodecH264), 0, 10, 100) {
			h.Write(f) // less than 1 s: still buffering
		}
		cancel()
		r := wait(t, ch)
		require.Error(t, r.err)
		waitUnsubscribed(t, h)
	})
}

func TestHTTPTSNotFound(t *testing.T) {
	srv := httptest.NewServer(Handler(func(name string) (*hub.Hub, bool) {
		if name == "nil" {
			return nil, true
		}
		return nil, false
	}, 0))
	defer srv.Close()
	for _, path := range []string{"/live/x/stream.ts", "/live/nil/stream.ts", "/live/x/other.ts"} {
		resp, err := srv.Client().Do(request(t, http.MethodGet, srv.URL+path))
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
}

func TestHTTPTSHead(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := server(t, h)
	resp, err := srv.Client().Do(request(t, http.MethodHead, srv.URL+"/live/s/stream.ts"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "video/mp2t", resp.Header.Get("Content-Type"))
	assert.Empty(t, h.Stats())
}

func TestHTTPTSClosedBeforeAnyFrame(t *testing.T) {
	r := stream(t, nil)
	assert.Empty(t, r.body)
}

func TestHTTPTSClosedWhileBuffering(t *testing.T) {
	// Half a second: the subscription closes before the 1 s of buffering.
	frames := synth(configOf(t, "h264_aac.ts", media.CodecH264), 0, 12, 100)
	got := readBack(t, stream(t, frames).body)
	assert.Len(t, got, len(frames))
}

func TestHTTPTSEndsResponse(t *testing.T) {
	h264 := configOf(t, "h264_aac.ts", media.CodecH264)
	h265 := configOf(t, "h265_aac51.ts", media.CodecH265)
	aac := configOf(t, "h264_aac.ts", media.CodecAAC)
	bogus := &media.TrackConfig{Codec: 99}

	for _, tc := range []struct {
		name  string
		after media.Frame // written after 1.2 s of H.264 on track 0
		want  int         // frames read back
	}{
		{"codec change", media.Frame{Track: 0, Config: h265, Key: true, Data: [][]byte{{0x26, 1}}}, 30},
		{"unsupported new track", media.Frame{Track: 1, Config: bogus, Key: true, Data: [][]byte{{1}}}, 30},
		{"bad frame", media.Frame{Track: 0, Config: h264, Key: true}, 30},
		{"skipped frames", media.Frame{Track: -1, Config: aac, Key: true, Data: [][]byte{{1}}}, 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := hub.New(hub.Options{})
			defer h.Close()
			srv := server(t, h)
			ch := startGet(context.Background(), srv.Client(), srv.URL+"/live/s/stream.ts")
			waitSubscribed(t, h)
			frames := synth(h264, 0, 30, 100)
			for _, f := range frames {
				h.Write(f)
			}
			f := tc.after
			f.DTS, f.PTS, f.Epoch = 30*3600, 30*3600, 1
			h.Write(f)
			if tc.name == "skipped frames" {
				// Neither a track index out of range nor a frame without
				// config on an unconfigured track ends the response.
				h.Write(media.Frame{Track: maxTracks, Config: aac, DTS: 30 * 3600, PTS: 30 * 3600, Key: true, Data: [][]byte{{1}}})
				h.Write(media.Frame{Track: 2, DTS: 30 * 3600, PTS: 30 * 3600, Data: [][]byte{{1}}})
				last := synth(h264, 31*3600, 1, 100)[0]
				h.Write(last)
				h.Close()
			} else {
				waitUnsubscribed(t, h)
			}
			r := wait(t, ch)
			require.NoError(t, r.err)
			assert.Len(t, readBack(t, r.body), tc.want)
		})
	}
}

func TestHTTPTSUnsupportedFirstTrack(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := server(t, h)
	ch := startGet(context.Background(), srv.Client(), srv.URL+"/live/s/stream.ts")
	waitSubscribed(t, h)
	for i := range 30 {
		dts := int64(i) * 3600
		h.Write(media.Frame{Track: 0, Config: &media.TrackConfig{Codec: 99}, DTS: dts, PTS: dts, Key: true, Data: [][]byte{{1}}})
	}
	waitUnsubscribed(t, h)
	r := wait(t, ch)
	require.NoError(t, r.err)
	assert.Empty(t, r.body)
}
