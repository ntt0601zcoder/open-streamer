package live

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/segment"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeNow is a settable clock safe for concurrent use.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeNow) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/fixtures/" + name)
	require.NoError(t, err)
	return b
}

// clip runs the unpaced pipeline over a fixture into a closed hub, with
// anchors at t0, and returns the hub and the deliveries a subscriber with a
// large queue got.
func clip(t *testing.T, name string) (*hub.Hub, *hub.Sub, []hub.Delivery) {
	t.Helper()
	b := fixture(t, name)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("pkg", 1<<16)
	check := h.Subscribe("check", 1<<16)
	require.NoError(t, pipeline.Run(context.Background(), h, pipeline.Options{
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
		Now:  func() time.Time { return t0 },
	}))
	h.Close()
	var ds []hub.Delivery
	for d := range check.C() {
		ds = append(ds, d)
	}
	return h, sub, ds
}

// packClip packages a whole fixture through Run.
func packClip(t *testing.T, name string, opt Options) (*Packager, []hub.Delivery) {
	t.Helper()
	h, sub, ds := clip(t, name)
	p := NewPackager(h, opt)
	p.Run(context.Background(), sub)
	return p, ds
}

// segmentsOf cuts deliveries the way the packager does.
func segmentsOf(ds []hub.Delivery, target int64) []segment.Segment {
	s := segment.New(segment.Options{Target: target})
	var out []segment.Segment
	for _, d := range ds {
		out = append(out, s.Push(d)...)
	}
	return append(out, s.Flush()...)
}

func serve(p *Packager) http.Handler {
	return Handler(func(name string) (*Packager, bool) {
		if name == "s" {
			return p, true
		}
		return nil, false
	})
}

func get(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec.Code, rec.Body.Bytes()
}

func farFuture() time.Time { return t0.Add(time.Hour) }

func TestPackagerTSAndCMAF(t *testing.T) {
	p, ds := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	want := segmentsOf(ds, 0)
	require.Len(t, want, 3)

	snap := p.Snapshot()
	require.Len(t, snap.Segments, 3)
	assert.Equal(t, 3, snap.Listed)
	assert.EqualValues(t, 180000, snap.Target)
	assert.Equal(t, 100, snap.Window)
	assert.Equal(t, t0, snap.AST)
	assert.Equal(t, farFuture(), snap.Now)
	assert.Zero(t, p.Errors())
	assert.Zero(t, p.FallbackAnchors())

	require.Len(t, snap.Epochs, 1)
	ep := snap.Epochs[0]
	assert.EqualValues(t, 1, ep.Epoch)
	assert.Equal(t, t0, ep.Wall)
	assert.Zero(t, ep.Start)
	require.Len(t, ep.Tracks, 2)

	srv := serve(p)
	var all []byte
	perTrack := map[int]int{}
	for i, s := range snap.Segments {
		w := want[i]
		assert.Equal(t, w.Seq, s.Seq)
		assert.Equal(t, w.Epoch, s.Epoch)
		assert.Equal(t, w.Start, s.Start)
		assert.Equal(t, w.Duration, s.Duration)
		assert.Equal(t, w.End, s.End)
		assert.False(t, s.Discontinuity)

		code, body := get(t, srv, fmt.Sprintf("/live/s/hls/%d.ts", s.Seq))
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, s.TS, body)
		all = append(all, body...)

		frames := map[int]int{}
		for _, f := range w.Frames {
			frames[f.Track]++
		}
		require.Len(t, s.Parts, 2)
		for j, part := range s.Parts {
			assert.Equal(t, j, part.Track)
			assert.EqualValues(t, i, part.N)
			assert.False(t, part.Discontinuity)
			code, body := get(t, srv, fmt.Sprintf("/live/s/cmaf/%d-%d.m4s", part.Track, part.N))
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, part.Data, body)

			code, init := get(t, srv, fmt.Sprintf("/live/s/cmaf/init-%d-1.mp4", part.Track))
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, ep.Tracks[j].Init, init)
			f, err := mp4.DecodeFile(bytes.NewReader(append(append([]byte(nil), init...), body...)))
			require.NoError(t, err)
			require.Len(t, f.Segments, 1)
			n := int(f.Segments[0].Fragments[0].Moof.Traf.Trun.SampleCount())
			assert.Equal(t, frames[part.Track], n, "segment %d track %d", i, part.Track)
			perTrack[part.Track] += n
			assert.NotZero(t, part.Timing.Duration)
		}
	}
	for tr, n := range perTrack {
		assert.Positive(t, n, "track %d", tr)
	}

	// The TS segments join into one stream with no continuity error.
	d, err := ts.NewDemuxer(bytes.NewReader(all))
	require.NoError(t, err)
	n := 0
	for {
		_, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		n++
	}
	total := 0
	for _, w := range want {
		total += len(w.Frames)
	}
	assert.Equal(t, total, n)
	assert.Zero(t, d.Stats().DecodeErrors)
	assert.Zero(t, d.Stats().DroppedNoConfig)
}

func TestPackagerMissingTrack(t *testing.T) {
	p, _ := packClip(t, "h264_aac_then_640.ts", Options{Window: 100, Now: farFuture})
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 2)
	assert.Zero(t, p.Errors())

	var audioN []uint64
	sawEpoch2 := false
	for _, s := range snap.Segments {
		if s.Epoch != 2 {
			continue
		}
		sawEpoch2 = true
		require.Len(t, s.Parts, 1)
		assert.Equal(t, 0, s.Parts[0].Track)
	}
	require.True(t, sawEpoch2)
	for _, s := range snap.Segments {
		for _, part := range s.Parts {
			if part.Track == 1 {
				audioN = append(audioN, part.N)
			}
		}
	}
	for i, n := range audioN {
		assert.EqualValues(t, i, n)
	}

	// The first epoch-2 segment is discontinuous in TS and in the video track.
	var first SegmentInfo
	for _, s := range snap.Segments {
		if s.Epoch == 2 {
			first = s
			break
		}
	}
	assert.True(t, first.Discontinuity)
	assert.True(t, first.Parts[0].Discontinuity)

	e2 := snap.Epochs[1]
	assert.EqualValues(t, 2, e2.Epoch)
	require.Len(t, e2.Tracks, 1)
	assert.Equal(t, 0, e2.Tracks[0].Track)
	assert.Equal(t, 640, e2.Tracks[0].Config.Width)
	assert.Equal(t, 360, e2.Tracks[0].Config.Height)
	e1 := snap.Epochs[0]
	require.Len(t, e1.Tracks, 2)
	assert.Equal(t, 320, e1.Tracks[0].Config.Width)
	assert.Equal(t, media.CodecAAC, e1.Tracks[1].Config.Codec)
	got, ok := snap.Epoch(2)
	require.True(t, ok)
	assert.Equal(t, e2.Epoch, got.Epoch)
	_, ok = snap.Epoch(9)
	assert.False(t, ok)

	srv := serve(p)
	code, _ := get(t, srv, "/live/s/cmaf/init-1-2.mp4")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = get(t, srv, "/live/s/cmaf/init-0-2.mp4")
	assert.Equal(t, http.StatusOK, code)
	code, _ = get(t, srv, "/live/s/cmaf/init-1-1.mp4")
	assert.Equal(t, http.StatusOK, code)
}

func TestWindowSlides(t *testing.T) {
	p, ds := packClip(t, "h264_aac_loop.ts", Options{Window: 2, Now: farFuture})
	want := segmentsOf(ds, 0)
	require.Len(t, want, 6)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 2)
	assert.EqualValues(t, 4, snap.Segments[0].Seq)
	assert.EqualValues(t, 5, snap.Segments[1].Seq)
	assert.Equal(t, 2, snap.Listed)
	// Seq 3 opened epoch 2 and has left the window.
	assert.EqualValues(t, 1, snap.EvictedDiscontinuities)
	assert.Equal(t, map[int]uint64{0: 1, 1: 1}, snap.EvictedTrackDiscontinuities)
	require.Len(t, snap.Epochs, 1)
	assert.EqualValues(t, 2, snap.Epochs[0].Epoch)
	for _, s := range snap.Segments {
		for _, part := range s.Parts {
			assert.EqualValues(t, s.Seq, part.N)
		}
	}

	srv := serve(p)
	for _, path := range []string{"/live/s/hls/3.ts", "/live/s/cmaf/0-3.m4s", "/live/s/cmaf/init-0-1.mp4"} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}
	for _, path := range []string{"/live/s/hls/4.ts", "/live/s/hls/5.ts", "/live/s/cmaf/1-4.m4s", "/live/s/cmaf/init-1-2.mp4"} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusOK, code, path)
	}
}

func TestHandlerNotFound(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	for _, path := range []string{
		"/live/x/hls/0.ts",
		"/live/x/cmaf/0-0.m4s",
		"/live/s/hls/3.ts",
		"/live/s/hls/00.ts",
		"/live/s/hls/+1.ts",
		"/live/s/hls/a.ts",
		"/live/s/hls/0.m4s",
		"/live/s/hls/18446744073709551616.ts",
		"/live/s/cmaf/0.ts",
		"/live/s/cmaf/2-0.m4s",
		"/live/s/cmaf/0-3.m4s",
		"/live/s/cmaf/0-x.m4s",
		"/live/s/cmaf/-1-0.m4s",
		"/live/s/cmaf/00-0.m4s",
		"/live/s/cmaf/0-0-0.m4s",
		"/live/s/cmaf/init-0-2.mp4",
		"/live/s/cmaf/init-2-1.mp4",
		"/live/s/cmaf/init-0.mp4",
		"/live/s/cmaf/init-a-1.mp4",
		"/live/s/cmaf/init-0-4294967296.mp4",
		"/live/s/cmaf/foo.mp4",
		"/live/s/other/0.ts",
		"/live/s/cmaf/other.mpd",
	} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}
	code, _ := get(t, srv, "/live/s/hls/0.ts")
	assert.Equal(t, http.StatusOK, code)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/live/s/cmaf/0-0.m4s", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "video/iso.segment", rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Body.Bytes())
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live/s/hls/0.ts", nil))
	assert.Equal(t, "video/mp2t", rec.Header().Get("Content-Type"))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live/s/cmaf/init-0-1.mp4", nil))
	assert.Equal(t, "video/mp4", rec.Header().Get("Content-Type"))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/live/s/hls/0.ts", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandlerEmptyPackager(t *testing.T) {
	p := NewPackager(hub.New(hub.Options{}), Options{})
	snap := p.Snapshot()
	assert.Empty(t, snap.Segments)
	assert.Zero(t, snap.Listed)
	assert.True(t, snap.AST.IsZero())
	assert.Equal(t, defaultWindow, snap.Window)
	assert.EqualValues(t, defaultTarget, snap.Target)
	assert.WithinDuration(t, time.Now(), snap.Now, time.Minute)
	srv := serve(p)
	for _, path := range []string{"/live/s/hls/0.ts", "/live/s/cmaf/0-0.m4s", "/live/s/cmaf/init-0-1.mp4", "/live/s/hls/index.m3u8"} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}
	p.Flush() // nothing open
	assert.Empty(t, p.Snapshot().Segments)
}

func TestUnlistedUntilEnded(t *testing.T) {
	clk := &fakeNow{t: farFuture()}
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: clk.Now})
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 3)
	ep := snap.Epochs[0]
	end1 := ep.WallAt(snap.Segments[1].End)
	assert.Equal(t, t0.Add(media.ToDuration(snap.Segments[1].End-ep.Anchor.Start)), end1)

	clk.Set(end1)
	snap = p.Snapshot()
	assert.Equal(t, 2, snap.Listed)
	assert.True(t, snap.IsListed(snap.Segments[1]))
	assert.False(t, snap.IsListed(snap.Segments[2]))
	clk.Set(end1.Add(-time.Nanosecond))
	assert.Equal(t, 1, p.Snapshot().Listed)

	srv := serve(p)
	code, _ := get(t, srv, "/live/s/hls/2.ts")
	assert.Equal(t, http.StatusOK, code, "an unlisted segment is still served")
	code, _ = get(t, srv, "/live/s/cmaf/0-2.m4s")
	assert.Equal(t, http.StatusOK, code)

	clk.Set(t0.Add(-time.Second))
	snap = p.Snapshot()
	assert.Zero(t, snap.Listed)
	code, _ = get(t, srv, "/live/s/hls/index.m3u8")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = get(t, srv, "/live/s/hls/0.ts")
	assert.Equal(t, http.StatusOK, code)
}

// h264Config returns the video config of the h264_aac.ts fixture.
func h264Config(t *testing.T) *media.TrackConfig {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixture(t, "h264_aac.ts")))
	require.NoError(t, err)
	for {
		f, err := d.Read()
		require.NoError(t, err)
		if f.Kind() == media.KindVideo && f.Config != nil {
			return f.Config
		}
	}
}

// synth returns n 25 fps video frames on track 0 from start, a keyframe every
// 50 frames (2 s).
func synth(cfg *media.TrackConfig, epoch uint32, start int64, n int) []hub.Delivery {
	out := make([]hub.Delivery, 0, n)
	for i := 0; i < n; i++ {
		key := i%50 == 0
		data := []byte{0x41, 0x9a, byte(i)}
		if key {
			data = []byte{0x65, 0x88, byte(i)}
		}
		ts := start + int64(i)*3600
		out = append(out, hub.Delivery{Frame: media.Frame{
			Track: 0, Config: cfg, PTS: ts, DTS: ts, Key: key, Data: [][]byte{data}, Epoch: epoch,
		}})
	}
	return out
}

func handleAll(p *Packager, ds []hub.Delivery) {
	for _, d := range ds {
		p.Handle(d)
	}
}

func TestPackagerErrors(t *testing.T) {
	h, _, ds := clip(t, "h264_aac.ts")
	// 96 kHz AAC: the TS muxer writes it, fmp4.Init rejects the rate.
	hi, err := media.NewAACConfig([]byte{0x10, 0x10})
	require.NoError(t, err)
	require.Equal(t, 96000, hi.SampleRate)
	for i := range ds {
		if ds[i].Frame.Kind() == media.KindAudio {
			ds[i].Frame.Config = hi
		}
	}
	p := NewPackager(h, Options{Window: 100, Now: farFuture})
	handleAll(p, ds)
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 3)
	assert.EqualValues(t, 3, p.Errors(), "one failed init per segment")
	for _, s := range snap.Segments {
		assert.NotEmpty(t, s.TS)
		require.Len(t, s.Parts, 1)
		assert.Equal(t, 0, s.Parts[0].Track)
	}
	require.Len(t, snap.Epochs[0].Tracks, 2)
	assert.Nil(t, snap.Epochs[0].Tracks[1].Init)
	assert.Equal(t, hi, snap.Epochs[0].Tracks[1].Config)
	srv := serve(p)
	code, _ := get(t, srv, "/live/s/cmaf/init-1-1.mp4")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = get(t, srv, "/live/s/cmaf/1-0.m4s")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestPackagerMuxErrorsPerFormat(t *testing.T) {
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})

	// Segment 0: a P-frame without data fails the TS muxer only.
	ds := synth(cfg, 1, 0, 100)
	ds[3].Frame.Data = nil
	// Segment 1: a repeated DTS fails the CMAF writer only.
	ds[60].Frame.DTS = ds[59].Frame.DTS
	ds[60].Frame.PTS = ds[59].Frame.PTS
	handleAll(p, ds)
	// Segment 2: a track without a config fails both.
	tail := synth(cfg, 1, 100*3600, 50)
	tail = append(tail, hub.Delivery{Frame: media.Frame{Track: 3, PTS: 100 * 3600, DTS: 100 * 3600, Key: true, Data: [][]byte{{1}}, Epoch: 1}})
	handleAll(p, tail)
	p.Flush()

	snap := p.Snapshot()
	require.Len(t, snap.Segments, 3)
	assert.Nil(t, snap.Segments[0].TS)
	require.Len(t, snap.Segments[0].Parts, 1)
	assert.EqualValues(t, 0, snap.Segments[0].Parts[0].N)
	assert.False(t, snap.Segments[0].Parts[0].Discontinuity)
	assert.NotNil(t, snap.Segments[1].TS)
	assert.False(t, snap.Segments[1].TSDiscontinuity, "the first segment with TS bytes")
	assert.Zero(t, snap.Segments[1].TSSeq)
	assert.Empty(t, snap.Segments[1].Parts)
	assert.Nil(t, snap.Segments[2].TS)
	require.Len(t, snap.Segments[2].Parts, 1)
	assert.EqualValues(t, 1, snap.Segments[2].Parts[0].N, "n has no hole after a failed segment")
	assert.False(t, snap.Segments[2].Discontinuity)
	assert.True(t, snap.Segments[2].Parts[0].Discontinuity, "the part before it was dropped")
	assert.EqualValues(t, 4, p.Errors())

	srv := serve(p)
	code, _ := get(t, srv, "/live/s/hls/0.ts")
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = get(t, srv, "/live/s/hls/1.ts")
	assert.Equal(t, http.StatusOK, code)
}

func TestPackagerNoAnchorFallsBackToNow(t *testing.T) {
	cfg := h264Config(t)
	clk := &fakeNow{t: t0.Add(5 * time.Second)}
	p := NewPackager(hub.New(hub.Options{}), Options{Now: clk.Now})
	handleAll(p, synth(cfg, 7, 900, 60))
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 1)
	ep := snap.Epochs[0]
	// Anchored at the segment's start, placed so that it ends at Now.
	assert.Equal(t, media.Anchor{Epoch: 7, Start: 900, Wall: t0.Add(3 * time.Second)}, ep.Anchor)
	assert.Equal(t, clk.Now(), ep.WallAt(snap.Segments[0].End))
	assert.Equal(t, ep.Anchor.Wall, snap.AST)
	assert.EqualValues(t, 1, p.FallbackAnchors())
	assert.True(t, snap.IsListed(snap.Segments[0]))
}

func TestEffectiveWall(t *testing.T) {
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	clk := &fakeNow{t: t0.Add(time.Hour)}
	p := NewPackager(h, Options{Window: 3, Now: clk.Now})

	// Epoch 1: stream time 0..4 s at t0.
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	handleAll(p, synth(cfg, 1, 0, 100))
	// Epoch 2: continuous stream time, but 10 s of wallclock pause (plus
	// 300 µs, which rounds up to the millisecond).
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(14*time.Second + 300*time.Microsecond)})
	handleAll(p, synth(cfg, 2, 360000, 100))
	snap := p.Snapshot()
	e2, ok := snap.Epoch(2)
	require.True(t, ok, "epoch 2's first segment is packaged")
	assert.Equal(t, 14001*time.Millisecond, e2.Start)
	assert.Equal(t, t0.Add(14001*time.Millisecond), e2.Wall)

	// Epoch 3: the anchor wall is the effective wall (the Clock already kept
	// epochs apart, or chose to overlap to bound the lead), so the packager
	// takes it as it is and adds no stream-time chain of its own: a chain
	// here would compound the Clock's and let the lead accumulate.
	h.SetAnchor(media.Anchor{Epoch: 3, Start: 720045, Wall: t0.Add(15 * time.Second)})
	// A later anchor update for epoch 2 must not move its stored start.
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(50 * time.Second)})
	handleAll(p, synth(cfg, 3, 720045, 100))
	p.Flush()

	snap = p.Snapshot()
	assert.Equal(t, t0, snap.AST)
	e3, ok := snap.Epoch(3)
	require.True(t, ok)
	assert.Equal(t, 15*time.Second, e3.Start)
	assert.Equal(t, t0.Add(15*time.Second), e3.Wall)
	assert.Equal(t, media.Anchor{Epoch: 3, Start: 720045, Wall: t0.Add(15 * time.Second)}, e3.Anchor)
	// Epoch 1 left the window; epoch 2 kept the start computed when its first
	// segment was packaged.
	_, ok = snap.Epoch(1)
	assert.False(t, ok)
	e2b, ok := snap.Epoch(2)
	require.True(t, ok)
	assert.Equal(t, e2.Start, e2b.Start)
	assert.Equal(t, e2.Wall, e2b.Wall)
	assert.Equal(t, e2.Anchor, e2b.Anchor)

	// Listing uses W_n.
	last := snap.Segments[len(snap.Segments)-1]
	require.EqualValues(t, 3, last.Epoch)
	endWall := t0.Add(15*time.Second + media.ToDuration(last.End-720045))
	clk.Set(endWall.Add(-time.Nanosecond))
	assert.Equal(t, len(snap.Segments)-1, p.Snapshot().Listed)
	clk.Set(endWall)
	assert.Len(t, p.Snapshot().Segments, p.Snapshot().Listed)
}

// Period starts strictly increase even when anchor walls do not.
func TestEffectiveWallMonotonic(t *testing.T) {
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	p := NewPackager(h, Options{Window: 10, Now: farFuture})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0.Add(time.Second)})
	handleAll(p, synth(cfg, 1, 0, 50))
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 180000, Wall: t0})
	handleAll(p, synth(cfg, 2, 180000, 50))
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 2)
	assert.Equal(t, time.Duration(0), snap.Epochs[0].Start)
	assert.Equal(t, time.Millisecond, snap.Epochs[1].Start)
}

// Without hub anchors, epochs are placed from their arrival by the Clock's
// rule: chained by stream time to avoid overlap, but at most MaxAnchorLead
// past the arrival.
func TestFallbackWallLeadBounded(t *testing.T) {
	cfg := h264Config(t)
	clk := &fakeNow{t: t0}
	p := NewPackager(nil, Options{Window: 10, Now: clk.Now})
	// Epoch 1: 2 s of stream time arriving by t0+2s.
	clk.Set(t0.Add(2 * time.Second))
	handleAll(p, synth(cfg, 1, 0, 51))
	// Epoch 2 arrives 500 ms early against the chain: chained.
	clk.Set(t0.Add(3500 * time.Millisecond))
	handleAll(p, synth(cfg, 2, 180000, 51))
	// Epoch 3 arrives 2 s early against the chain: capped at 1 s of lead.
	clk.Set(t0.Add(4 * time.Second))
	handleAll(p, synth(cfg, 3, 360000, 51))
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 3)
	assert.EqualValues(t, 3, p.FallbackAnchors())
	assert.Equal(t, time.Duration(0), snap.Epochs[0].Start)
	assert.Equal(t, 2*time.Second, snap.Epochs[1].Start, "chained: no overlap with epoch 1")
	assert.Equal(t, t0.Add(2*time.Second), snap.Epochs[2].Anchor.Wall, "arrival: Now minus the segment's span")
	assert.Equal(t, 3*time.Second, snap.Epochs[2].Start, "capped: 1 s past the arrival, not the 4 s chain")
}

func TestCeilMillisecond(t *testing.T) {
	assert.Equal(t, time.Duration(0), ceilMS(0))
	assert.Equal(t, time.Millisecond, ceilMS(1))
	assert.Equal(t, time.Millisecond, ceilMS(time.Millisecond))
	assert.Equal(t, time.Duration(0), ceilMS(-1))
	assert.Equal(t, -time.Millisecond, ceilMS(-time.Millisecond-1))
}

func TestRunStopsOnCancel(t *testing.T) {
	h := hub.New(hub.Options{})
	sub := h.Subscribe("pkg", 0)
	defer sub.Close()
	p := NewPackager(h, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, sub)
	}()
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	for _, d := range synth(h264Config(t), 1, 0, 10) {
		h.Write(d.Frame)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestConcurrentReads(t *testing.T) {
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	sub := h.Subscribe("pkg", 1<<12)
	p := NewPackager(h, Options{Window: 3, Now: farFuture})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, sub)
	}()
	srv := serve(p)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := p.Snapshot()
			for _, s := range snap.Segments {
				get(t, srv, fmt.Sprintf("/live/s/hls/%d.ts", s.Seq))
			}
		}
	}()
	for _, d := range synth(cfg, 1, 0, 1000) {
		h.Write(d.Frame)
	}
	h.Close()
	<-done
	close(stop)
	wg.Wait()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 3)
	assert.EqualValues(t, 19, snap.Segments[2].Seq)
}

func TestPlaylistRoutes(t *testing.T) {
	clk := &fakeNow{t: farFuture()}
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: clk.Now})
	var built []Snapshot
	routes := []playlistRoute{
		{
			dir:   dirHLS,
			match: func(name string) bool { return name == "index.m3u8" },
			build: func(got *Packager, s Snapshot, name string) ([]byte, string, bool) {
				assert.Same(t, p, got)
				assert.Equal(t, "index.m3u8", name)
				built = append(built, s)
				return []byte("#EXTM3U\n"), "application/vnd.apple.mpegurl", true
			},
		},
		{
			dir:   dirCMAF,
			match: func(name string) bool { return name == "index.mpd" },
			build: func(*Packager, Snapshot, string) ([]byte, string, bool) { return nil, "", false },
		},
	}
	srv := newHandler(func(name string) (*Packager, bool) { return p, name == "s" }, routes, time.Second)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live/s/hls/index.m3u8", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "#EXTM3U\n", rec.Body.String())
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))
	require.Len(t, built, 1)
	assert.Equal(t, 3, built[0].Listed)

	for _, path := range []string{
		"/live/s/cmaf/index.m3u8", // the route is for hls/ only
		"/live/s/cmaf/index.mpd",  // the builder declines
		"/live/s/hls/other.m3u8",  // no route
	} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}

	clk.Set(t0.Add(-time.Second)) // nothing listed yet
	code, _ := get(t, srv, "/live/s/hls/index.m3u8")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Len(t, built, 1, "the builder is not called before a segment is listed")
}

func TestPackagerSetupErrors(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})

	// Segment 0: an unknown codec fails the TS muxer's creation and the init.
	bad := &media.TrackConfig{Codec: 99}
	for i := 0; i < 50; i++ {
		ts := int64(i) * 3600
		p.Handle(hub.Delivery{Frame: media.Frame{Track: 0, Config: bad, PTS: ts, DTS: ts, Key: true, Data: [][]byte{{1}}, Epoch: 1}})
	}

	// Segment 1 (epoch 2): an audio frame on a negative track fails TS and is
	// not packaged as CMAF; the video track is.
	aac, err := media.NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 50 * 3600, Wall: t0})
	ds := synth(h264Config(t), 2, 50*3600, 10)
	neg := hub.Delivery{Frame: media.Frame{Track: -1, Config: aac, PTS: 51 * 3600, DTS: 51 * 3600, Key: true, Data: [][]byte{{1}}, Epoch: 2}}
	ds = slices.Insert(ds, 2, neg)
	handleAll(p, ds)
	p.Flush()

	snap := p.Snapshot()
	require.Len(t, snap.Segments, 2)
	assert.Nil(t, snap.Segments[0].TS)
	assert.Empty(t, snap.Segments[0].Parts)
	assert.Nil(t, snap.Segments[1].TS)
	require.Len(t, snap.Segments[1].Parts, 1)
	assert.Equal(t, 0, snap.Segments[1].Parts[0].Track)
	assert.False(t, snap.Segments[1].Parts[0].Discontinuity, "the track's first part")
	assert.EqualValues(t, 4, p.Errors())
}

// dropTS packages 5 segments of epoch 1 and 2 of epoch 2 with the TS output
// of segments 1 and 5 (the first of epoch 2) failing.
func dropTS(t *testing.T, window int) *Packager {
	t.Helper()
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 250 * 3600, Wall: t0})
	p := NewPackager(h, Options{Window: window, Now: farFuture})
	ds := synth(cfg, 1, 0, 250)
	ds[53].Frame.Data = nil
	e2 := synth(cfg, 2, 250*3600, 100)
	e2[2].Frame.Data = nil
	handleAll(p, append(ds, e2...))
	p.Flush()
	return p
}

func TestTSDiscontinuityAfterDroppedTS(t *testing.T) {
	p := dropTS(t, 100)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 7)
	assert.EqualValues(t, 2, p.Errors())
	n := len(snap.Segments)
	tsNil, tsDisc, disc := make([]bool, 0, n), make([]bool, 0, n), make([]bool, 0, n)
	for _, s := range snap.Segments {
		tsNil = append(tsNil, s.TS == nil)
		tsDisc = append(tsDisc, s.TSDiscontinuity)
		disc = append(disc, s.Discontinuity)
	}
	assert.Equal(t, []bool{false, true, false, false, false, true, false}, tsNil)
	assert.Equal(t, []bool{false, false, false, false, false, true, false}, disc)
	// Segment 2 follows a dropped TS; segment 6 inherits the discontinuity of
	// the dropped segment 5.
	assert.Equal(t, []bool{false, false, true, false, false, false, true}, tsDisc)

	// Window 1: segments 0..5 left, carrying one TS flag (segment 2) and
	// one segment flag (segment 5).
	snap = dropTS(t, 1).Snapshot()
	require.Len(t, snap.Segments, 1)
	assert.EqualValues(t, 6, snap.Segments[0].Seq)
	assert.EqualValues(t, 1, snap.EvictedTSDiscontinuities)
	assert.EqualValues(t, 1, snap.EvictedDiscontinuities)
	// Window 2: segment 5 is still in; its dropped TS has no flag.
	snap = dropTS(t, 2).Snapshot()
	assert.EqualValues(t, 1, snap.EvictedTSDiscontinuities)
	assert.Zero(t, snap.EvictedDiscontinuities)
}

func TestSnapshotSlicesClipped(t *testing.T) {
	aac, err := media.NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})
	// Three tracks: appending three parts leaves spare capacity unless clipped.
	for _, d := range synth(h264Config(t), 1, 0, 60) {
		p.Handle(d)
		if d.Frame.DTS%7200 == 0 {
			for track := 1; track <= 2; track++ {
				p.Handle(hub.Delivery{Frame: media.Frame{
					Track: track, Config: aac, PTS: d.Frame.DTS, DTS: d.Frame.DTS, Key: true, Data: [][]byte{{0x21, 0x00}}, Epoch: 1,
				}})
			}
		}
	}
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 2)
	for _, s := range snap.Segments {
		require.Len(t, s.Parts, 3)
		assert.Equal(t, len(s.Parts), cap(s.Parts))
	}
	for _, e := range snap.Epochs {
		require.Len(t, e.Tracks, 3)
		assert.Equal(t, len(e.Tracks), cap(e.Tracks))
	}
	assert.Zero(t, p.Errors())
}

func TestCMAFUsesEpochConfig(t *testing.T) {
	a48, err := media.NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	a44, err := media.NewAACConfig([]byte{0x12, 0x10})
	require.NoError(t, err)
	require.Equal(t, 44100, a44.SampleRate)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})
	var second int64 = -1
	for _, d := range synth(h264Config(t), 1, 0, 100) {
		p.Handle(d)
		if d.Frame.DTS%7200 != 0 {
			continue
		}
		cfg := a48
		if d.Frame.DTS >= 180000 {
			cfg = a44 // a stray config inside the epoch
			if second < 0 {
				second = d.Frame.DTS
			}
		}
		p.Handle(hub.Delivery{Frame: media.Frame{Track: 1, Config: cfg, PTS: d.Frame.DTS, DTS: d.Frame.DTS, Key: true, Data: [][]byte{{0x21, 0x00}}, Epoch: 1}})
	}
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 2)
	part := snap.Segments[1].Parts[1]
	require.Equal(t, 1, part.Track)
	assert.Equal(t, uint64(media.Rescale(second, 48000)), part.Timing.BaseTime, "timescale of the epoch's init")
	tr, ok := snap.Epochs[0].Track(1)
	require.True(t, ok)
	assert.Equal(t, a48, tr.Config)
}

// failTS packages 6 segments whose TS output fails at segments 0, 1 and 4.
func failTS(t *testing.T, window int) *Packager {
	t.Helper()
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Window: window, Now: farFuture})
	ds := synth(h264Config(t), 1, 0, 300)
	for _, i := range []int{3, 53, 203} {
		ds[i].Frame.Data = nil
	}
	handleAll(p, ds)
	p.Flush()
	return p
}

func TestTSSeqHasNoHoles(t *testing.T) {
	p := failTS(t, 100)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 6)
	assert.EqualValues(t, 3, p.Errors())
	type entry struct {
		seq, tsSeq uint64
		disc       bool
	}
	var got []entry
	for _, s := range snap.Segments[:snap.Listed] {
		if s.TS != nil {
			got = append(got, entry{s.Seq, s.TSSeq, s.TSDiscontinuity})
		} else {
			assert.False(t, s.TSDiscontinuity)
		}
	}
	// Segment 2 is the first with TS bytes (no flag despite the drops
	// before it); segment 5 follows the dropped segment 4.
	assert.Equal(t, []entry{{2, 0, false}, {3, 1, false}, {5, 2, true}}, got)

	for window, first := range map[int]uint64{3: 1, 2: 2, 1: 2} {
		snap := failTS(t, window).Snapshot()
		var tsSeqs []uint64
		for _, s := range snap.Segments[:snap.Listed] {
			if s.TS != nil {
				tsSeqs = append(tsSeqs, s.TSSeq)
			}
		}
		require.NotEmpty(t, tsSeqs, "window %d", window)
		assert.Equal(t, first, tsSeqs[0], "window %d", window)
		for i := range tsSeqs {
			assert.Equal(t, first+uint64(i), tsSeqs[i], "window %d", window)
		}
		assert.Zero(t, snap.EvictedTSDiscontinuities, "window %d: segment 5's flag is still in", window)
	}
}

func TestCMAFFailureFlags(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})
	ds := synth(h264Config(t), 1, 0, 250)
	// A repeated DTS fails the video part of segments 0, 2 and 3.
	for _, i := range []int{10, 110, 160} {
		ds[i].Frame.DTS, ds[i].Frame.PTS = ds[i-1].Frame.DTS, ds[i-1].Frame.PTS
	}
	handleAll(p, ds)
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	assert.EqualValues(t, 3, p.Errors())
	for _, i := range []int{0, 2, 3} {
		assert.Empty(t, snap.Segments[i].Parts, "segment %d", i)
	}
	// A failure before the track's first part does not flag it.
	require.Len(t, snap.Segments[1].Parts, 1)
	assert.EqualValues(t, 0, snap.Segments[1].Parts[0].N)
	assert.False(t, snap.Segments[1].Parts[0].Discontinuity)
	// Two consecutive failures after it flag the next part once.
	require.Len(t, snap.Segments[4].Parts, 1)
	assert.EqualValues(t, 1, snap.Segments[4].Parts[0].N)
	assert.True(t, snap.Segments[4].Parts[0].Discontinuity)
	for _, s := range snap.Segments {
		assert.False(t, s.Discontinuity)
	}
}

func TestFirstTSSegmentNeverDiscontinuous(t *testing.T) {
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 50 * 3600, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})
	// Epoch 1's only segment fails TS; epoch 2's first segment is the first
	// with TS bytes, and discontinuous for the segmenter.
	e1 := synth(cfg, 1, 0, 50)
	e1[3].Frame.Data = nil
	handleAll(p, e1)
	handleAll(p, synth(cfg, 2, 50*3600, 100))
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 3)
	assert.Nil(t, snap.Segments[0].TS)
	s1 := snap.Segments[1]
	assert.True(t, s1.Discontinuity)
	require.NotNil(t, s1.TS)
	assert.Zero(t, s1.TSSeq)
	assert.False(t, s1.TSDiscontinuity)
	assert.EqualValues(t, 1, snap.Segments[2].TSSeq)
	assert.False(t, snap.Segments[2].TSDiscontinuity)
	// The video track's parts: the first is never discontinuous.
	assert.False(t, snap.Segments[0].Parts[0].Discontinuity)
	assert.True(t, s1.Parts[0].Discontinuity)
}

// Players on another origin (hls.js, dash.js) can fetch every file: each
// response allows any origin, and a preflight is answered.
func TestHandlerCORS(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	for _, path := range []string{"/live/s/hls/index.m3u8", "/live/s/hls/0.ts", "/live/s/cmaf/index.mpd", "/live/s/cmaf/init-0-1.mp4", "/live/s/hls/99.ts"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://player.example")
		srv.ServeHTTP(rec, req)
		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"), path)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/live/s/cmaf/0-0.m4s", nil)
	req.Header.Set("Origin", "https://player.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "range")
	srv.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "GET")
	assert.Contains(t, strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")), "range")
	assert.Empty(t, rec.Body.Bytes())
}
