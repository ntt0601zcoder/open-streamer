package live

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

func media3(t *testing.T, srv http.Handler, path string) hls.MediaPlaylist {
	t.Helper()
	code, body := get(t, srv, path)
	require.Equal(t, http.StatusOK, code, path)
	pl, err := hls.ParseMedia(body)
	require.NoError(t, err, string(body))
	return pl
}

func master(t *testing.T, srv http.Handler) hls.MultivariantPlaylist {
	t.Helper()
	code, body := get(t, srv, "/live/s/cmaf/master.m3u8")
	require.Equal(t, http.StatusOK, code)
	pl, err := hls.ParseMultivariant(body)
	require.NoError(t, err, string(body))
	return pl
}

func TestHLSTSPlaylist(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	snap := p.Snapshot()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live/s/hls/index.m3u8", nil))
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))

	pl := media3(t, srv, "/live/s/hls/index.m3u8")
	assert.Equal(t, 3, pl.Version)
	assert.True(t, pl.Independent)
	assert.EqualValues(t, 0, pl.MediaSequence)
	assert.Zero(t, pl.DiscontinuitySequence)
	require.Len(t, pl.Segments, 3)
	assert.Equal(t, 2, pl.TargetDuration)
	ep := snap.Epochs[0]
	for i, s := range pl.Segments {
		seg := snap.Segments[i]
		assert.Equal(t, itoa(seg.Seq)+".ts", s.URI)
		assert.Equal(t, media.ToDuration(seg.Duration).Round(time.Millisecond), s.Duration)
		assert.False(t, s.Discontinuity)
		assert.Empty(t, s.Map)
		if i == 0 {
			assert.True(t, s.ProgramDateTime.Equal(ep.WallAt(seg.Start).Truncate(time.Millisecond)), "%v", s.ProgramDateTime)
		} else {
			assert.True(t, s.ProgramDateTime.IsZero())
		}
	}
}

func itoa(u uint64) string { return strconv.FormatUint(u, 10) }

func TestHLSFMP4Playlists(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	snap := p.Snapshot()

	for _, tc := range []struct {
		path  string
		track int
	}{{"/live/s/cmaf/v0.m3u8", 0}, {"/live/s/cmaf/a1.m3u8", 1}} {
		pl := media3(t, srv, tc.path)
		assert.Equal(t, 7, pl.Version, tc.path)
		assert.True(t, pl.Independent)
		assert.EqualValues(t, 0, pl.MediaSequence)
		require.Len(t, pl.Segments, 3)
		et, _ := snap.Epochs[0].Track(tc.track)
		ts := float64(timescaleOf(et.Config))
		for i, s := range pl.Segments {
			part := snap.Segments[i].Parts[tc.track]
			require.Equal(t, tc.track, part.Track)
			assert.Equal(t, itoa(uint64(tc.track))+"-"+itoa(part.N)+".m4s", s.URI)
			assert.Equal(t, "init-"+itoa(uint64(tc.track))+"-1.mp4", s.Map)
			assert.InDelta(t, float64(part.Timing.Duration)/ts, s.Duration.Seconds(), 0.001)
			if i == 0 {
				first := int64(float64(part.Timing.Earliest) / ts * 90000)
				want := snap.Epochs[0].WallAt(first)
				assert.WithinDuration(t, want, s.ProgramDateTime, 2*time.Millisecond)
			}
		}
	}

	m := master(t, srv)
	assert.Equal(t, 7, m.Version)
	assert.True(t, m.Independent)
	require.Len(t, m.Variants, 1)
	v := m.Variants[0]
	assert.Equal(t, "v0.m3u8", v.URI)
	assert.Equal(t, "aud", v.Audio)
	assert.Equal(t, 320, v.Width)
	peak := 1.0
	for _, seg := range snap.Segments {
		sum := 0.0
		for _, part := range seg.Parts {
			et, _ := snap.Epochs[0].Track(part.Track)
			sum += float64(len(part.Data)) * 8 * float64(timescaleOf(et.Config)) / float64(part.Timing.Duration)
		}
		peak = max(peak, sum)
	}
	assert.Equal(t, int(math.Ceil(peak)), v.Bandwidth)
	assert.Contains(t, v.Codecs, "avc1.")
	assert.Contains(t, v.Codecs, "mp4a.40.2")
	require.Len(t, m.Renditions, 1)
	assert.Equal(t, hls.Rendition{Type: "AUDIO", GroupID: "aud", Name: "audio", URI: "a1.m3u8", Default: true}, m.Renditions[0])

	for _, path := range []string{
		"/live/s/cmaf/v1.m3u8", "/live/s/cmaf/a0.m3u8", "/live/s/cmaf/v7.m3u8",
		"/live/s/cmaf/v.m3u8", "/live/s/cmaf/x0.m3u8", "/live/s/cmaf/v00.m3u8", "/live/s/cmaf/v-1.m3u8",
	} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}
}

func timescaleOf(c *media.TrackConfig) uint32 {
	if c.Codec.Kind() == media.KindAudio {
		return uint32(c.SampleRate)
	}
	return 90000
}

func TestHLSConfigChange(t *testing.T) {
	p, _ := packClip(t, "h264_aac_then_640.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 2)

	ts := media3(t, srv, "/live/s/hls/index.m3u8")
	var first2 SegmentInfo
	for _, seg := range snap.Segments {
		if seg.Epoch == 2 {
			first2 = seg
			break
		}
	}
	require.NotZero(t, first2.Duration)
	e2 := snap.Epochs[1]
	var discs int
	for _, s := range ts.Segments {
		if s.Discontinuity {
			discs++
			assert.Equal(t, e2.WallAt(first2.Start).Truncate(time.Millisecond).UnixMilli(), s.ProgramDateTime.UnixMilli())
		}
	}
	assert.Equal(t, 1, discs)

	v := media3(t, srv, "/live/s/cmaf/v0.m3u8")
	maps := map[string]bool{}
	discs = 0
	for _, s := range v.Segments {
		maps[s.Map] = true
		if s.Discontinuity {
			discs++
			assert.Equal(t, "init-0-2.mp4", s.Map)
			earliest := int64(first2.Parts[0].Timing.Earliest) // video timescale is 90 kHz
			assert.Equal(t, e2.WallAt(earliest).Truncate(time.Millisecond).UnixMilli(), s.ProgramDateTime.UnixMilli())
		}
	}
	assert.Equal(t, map[string]bool{"init-0-1.mp4": true, "init-0-2.mp4": true}, maps)
	assert.Equal(t, 1, discs)

	a := media3(t, srv, "/live/s/cmaf/a1.m3u8")
	n := 0
	for _, seg := range snap.Segments {
		if seg.Epoch == 1 {
			n++
		}
	}
	assert.Len(t, a.Segments, n, "the audio playlist stops growing")
	assert.Equal(t, 3, n)
	for _, s := range a.Segments {
		assert.Equal(t, "init-1-1.mp4", s.Map)
		assert.False(t, s.Discontinuity)
	}

	m := master(t, srv)
	require.Len(t, m.Variants, 1)
	for _, c := range []string{"avc1.4D400D", "avc1.4D401E", "mp4a.40.2"} {
		assert.Contains(t, m.Variants[0].Codecs, c)
	}
	assert.Equal(t, 640, m.Variants[0].Width)
	assert.Equal(t, 360, m.Variants[0].Height)
}

func TestHLSWindowSlides(t *testing.T) {
	p, _ := packClip(t, "h264_aac_loop.ts", Options{Window: 2, Now: farFuture})
	srv := serve(p)
	ts := media3(t, srv, "/live/s/hls/index.m3u8")
	assert.EqualValues(t, 4, ts.MediaSequence)
	assert.EqualValues(t, 1, ts.DiscontinuitySequence)
	require.Len(t, ts.Segments, 2)
	assert.Equal(t, "4.ts", ts.Segments[0].URI)
	for _, path := range []string{"/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8"} {
		pl := media3(t, srv, path)
		assert.EqualValues(t, 4, pl.MediaSequence, path)
		assert.EqualValues(t, 1, pl.DiscontinuitySequence, path)
	}
}

func TestHLSTSDroppedSegments(t *testing.T) {
	p := dropTS(t, 100)
	srv := serve(p)
	snap := p.Snapshot()
	pl := media3(t, srv, "/live/s/hls/index.m3u8")
	var want []string
	var wantDisc []bool
	var seq uint64
	first := true
	for _, s := range snap.Segments {
		if s.TS == nil {
			continue
		}
		if first {
			seq, first = s.TSSeq, false
		}
		want = append(want, itoa(s.Seq)+".ts")
		wantDisc = append(wantDisc, s.TSDiscontinuity)
	}
	assert.Equal(t, seq, pl.MediaSequence)
	got := make([]string, 0, len(pl.Segments))
	gotDisc := make([]bool, 0, len(pl.Segments))
	for _, s := range pl.Segments {
		got = append(got, s.URI)
		gotDisc = append(gotDisc, s.Discontinuity)
	}
	assert.Equal(t, want, got)
	assert.Equal(t, wantDisc, gotDisc)
	assert.Contains(t, wantDisc, true)
}

func TestHLSTSPlaylistWithoutTS(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Now: farFuture})
	ds := synth(h264Config(t), 1, 0, 50)
	ds[3].Frame.Data = nil
	handleAll(p, ds)
	p.Flush()
	require.Equal(t, 1, p.Snapshot().Listed)
	srv := serve(p)
	code, _ := get(t, srv, "/live/s/hls/index.m3u8")
	assert.Equal(t, http.StatusNotFound, code)
	pl := media3(t, srv, "/live/s/cmaf/v0.m3u8")
	assert.Len(t, pl.Segments, 1)
	// Video only: the master has no audio rendition.
	m := master(t, srv)
	assert.Empty(t, m.Renditions)
	assert.Empty(t, m.Variants[0].Audio)
}

func TestHLSH265NotIndependent(t *testing.T) {
	p, _ := packClip(t, "h265_aac51.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	require.NotZero(t, p.Snapshot().Listed)
	assert.False(t, media3(t, srv, "/live/s/hls/index.m3u8").Independent)
	assert.False(t, media3(t, srv, "/live/s/cmaf/v0.m3u8").Independent)
	assert.False(t, master(t, srv).Independent)
}

func TestHLSTargetDurationNeverShrinks(t *testing.T) {
	p := NewPackager(hub.New(hub.Options{}), Options{})
	s := Snapshot{Target: 180000}
	assert.Equal(t, 2, p.targetDuration("a", s, nil))
	assert.Equal(t, 2, p.targetDuration("a", s, []time.Duration{2400 * time.Millisecond}))
	assert.Equal(t, 3, p.targetDuration("a", s, []time.Duration{2600 * time.Millisecond}))
	assert.Equal(t, 3, p.targetDuration("a", s, []time.Duration{time.Second}), "never shrinks")
	assert.Equal(t, 2, p.targetDuration("b", s, []time.Duration{time.Second}), "per playlist")
	assert.Equal(t, 3, p.targetDuration("c", Snapshot{Target: 190000}, nil), "ceil of the target")
	// 2.4996 s prints as 2.500 in EXTINF, which clients round up to 3.
	assert.Equal(t, 3, p.targetDuration("d", s, []time.Duration{2499600 * time.Microsecond}))
	assert.Equal(t, 2, p.targetDuration("e", s, []time.Duration{2499400 * time.Microsecond}))
}

func TestHLSUnlistedNotAdvertised(t *testing.T) {
	clk := &fakeNow{t: farFuture()}
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: clk.Now})
	srv := serve(p)
	snap := p.Snapshot()
	// Now sits between the first and the second segment's end.
	clk.Set(snap.Epochs[0].WallAt(snap.Segments[0].End))
	assert.Len(t, media3(t, srv, "/live/s/hls/index.m3u8").Segments, 1)
	assert.Len(t, media3(t, srv, "/live/s/cmaf/v0.m3u8").Segments, 1)
	clk.Set(t0.Add(-time.Second))
	for _, path := range []string{"/live/s/hls/index.m3u8", "/live/s/cmaf/master.m3u8", "/live/s/cmaf/v0.m3u8"} {
		code, _ := get(t, srv, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}
}

// When the hub closes, Run flushes the last partial segment and the stream
// ends: once every segment is listed, the HLS playlists carry ENDLIST.
func TestHLSEndListAfterHubCloses(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	sub := h.Subscribe("pkg", 1<<12)
	clk := &fakeNow{t: t0}
	p := NewPackager(h, Options{Now: clk.Now})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background(), sub)
	}()
	for _, d := range synth(h264Config(t), 1, 0, 75) { // 1.5 segments
		h.Write(d.Frame)
	}
	clk.Set(farFuture())
	require.Eventually(t, func() bool { return p.Snapshot().Listed == 1 }, 5*time.Second, time.Millisecond)
	srv := serve(p)
	assert.False(t, media3(t, srv, "/live/s/hls/index.m3u8").Ended, "not before the end")
	assert.False(t, p.Snapshot().Ended)

	h.Close()
	<-done
	snap := p.Snapshot()
	assert.True(t, snap.Ended)
	require.Len(t, snap.Segments, 2, "the partial segment was flushed")
	assert.True(t, media3(t, srv, "/live/s/hls/index.m3u8").Ended)
	assert.True(t, media3(t, srv, "/live/s/cmaf/v0.m3u8").Ended)

	// Not every segment listed yet: no ENDLIST, or it would hide the rest.
	clk.Set(t0.Add(2 * time.Second))
	snap = p.Snapshot()
	require.Equal(t, 1, snap.Listed)
	assert.False(t, media3(t, srv, "/live/s/hls/index.m3u8").Ended)
}
