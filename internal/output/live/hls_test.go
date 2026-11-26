package live

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
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
	assert.Equal(t, 3, n)
	// Epoch 2 has no audio: one gap entry per segment, the discontinuity on
	// the first, the epoch-1 map kept.
	require.Len(t, a.Segments, len(snap.Segments))
	for i, s := range a.Segments {
		seg := snap.Segments[i]
		assert.Equal(t, "init-1-1.mp4", s.Map)
		assert.Equal(t, seg.Epoch == 2, s.Gap, "entry %d", i)
		assert.Equal(t, seg.Discontinuity, s.Discontinuity, "entry %d", i)
	}
	assert.Equal(t, 1, discontinuities(a))

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

// gapFeed returns the deliveries of epoch 1 A/V, epoch 2 video only and
// epoch 3 A/V, two 2 s segments each, with their anchors set on h. Segment
// Seq 2 and 3 have no audio.
func gapFeed(t *testing.T, h *hub.Hub) []hub.Delivery {
	t.Helper()
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(4 * time.Second)})
	h.SetAnchor(media.Anchor{Epoch: 3, Start: 720000, Wall: t0.Add(8 * time.Second)})
	ds := synthAV(t, 1, 0)
	ds = append(ds, synth(h264Config(t), 2, 360000, 100)...)
	return append(ds, synthAV(t, 3, 720000)...)
}

// gapScenario packages gapFeed whole.
func gapScenario(t *testing.T, opt Options) *Packager {
	t.Helper()
	h := hub.New(hub.Options{})
	ds := gapFeed(t, h)
	p := NewPackager(h, opt)
	handleAll(p, ds)
	p.Flush()
	return p
}

// entry is what must never change about a listed playlist entry.
type entry struct {
	uri      string
	duration time.Duration
	disc     bool
	gap      bool
	init     string // the EXT-X-MAP in effect
}

func entryOf(s hls.Segment) entry {
	return entry{s.URI, s.Duration, s.Discontinuity, s.Gap, s.Map}
}

// discSeqs returns each entry's discontinuity sequence number by media
// sequence number.
func discSeqs(pl hls.MediaPlaylist) map[uint64]uint64 {
	out := map[uint64]uint64{}
	d := pl.DiscontinuitySequence
	for i, s := range pl.Segments {
		if s.Discontinuity {
			d++
		}
		out[pl.MediaSequence+uint64(i)] = d
	}
	return out
}

// renditionWatch follows the CMAF playlists across refreshes and checks
// they stay aligned and that listed entries never change.
type renditionWatch struct {
	t       *testing.T
	srv     http.Handler
	paths   []string
	seen    map[string]map[uint64]entry // path -> MSN -> entry
	uriMSN  map[string]uint64
	lastDSN map[string]uint64
}

func newWatch(t *testing.T, srv http.Handler, paths ...string) *renditionWatch {
	return &renditionWatch{
		t: t, srv: srv, paths: paths,
		seen: map[string]map[uint64]entry{}, uriMSN: map[string]uint64{}, lastDSN: map[string]uint64{},
	}
}

// check refreshes every playlist once and returns them by path.
func (w *renditionWatch) check(step string) map[string]hls.MediaPlaylist {
	t := w.t
	t.Helper()
	out := map[string]hls.MediaPlaylist{}
	var ref hls.MediaPlaylist
	for i, path := range w.paths {
		pl := media3(t, w.srv, path)
		out[path] = pl
		assert.Equal(t, 7, pl.Version, "%s %s", step, path)
		if i == 0 {
			ref = pl
		} else {
			assert.Equal(t, ref.MediaSequence, pl.MediaSequence, "%s %s: MEDIA-SEQUENCE", step, path)
			assert.Equal(t, ref.DiscontinuitySequence, pl.DiscontinuitySequence, "%s %s: DISCONTINUITY-SEQUENCE", step, path)
			assert.Equal(t, discSeqs(ref), discSeqs(pl), "%s %s: discontinuity sequence numbers", step, path)
			assert.Len(t, pl.Segments, len(ref.Segments), "%s %s", step, path)
			assert.Equal(t, ref.TargetDuration, pl.TargetDuration, "%s %s: TARGETDURATION", step, path)
		}
		assert.GreaterOrEqual(t, pl.DiscontinuitySequence, w.lastDSN[path], "%s %s: DISCONTINUITY-SEQUENCE decreased", step, path)
		w.lastDSN[path] = pl.DiscontinuitySequence
		if w.seen[path] == nil {
			w.seen[path] = map[uint64]entry{}
		}
		for j, s := range pl.Segments {
			msn := pl.MediaSequence + uint64(j)
			if old, ok := w.seen[path][msn]; ok {
				assert.Equal(t, old, entryOf(s), "%s %s: entry %d changed", step, path, msn)
			}
			w.seen[path][msn] = entryOf(s)
			if old, ok := w.uriMSN[s.URI]; ok {
				assert.Equal(t, old, msn, "%s %s: %s moved", step, path, s.URI)
			}
			w.uriMSN[s.URI] = msn
			if s.Map != "" {
				code, _ := get(t, w.srv, "/live/s/cmaf/"+s.Map)
				assert.Equal(t, http.StatusOK, code, "%s %s: map %s", step, path, s.Map)
			}
			if s.Gap {
				code, _ := get(t, w.srv, "/live/s/cmaf/"+s.URI)
				assert.Equal(t, http.StatusNotFound, code, "%s %s: gap %s", step, path, s.URI)
			}
		}
	}
	return out
}

func TestHLSPerSegmentEntries(t *testing.T) {
	clk := &fakeNow{t: farFuture()}
	p := gapScenario(t, Options{Window: 10, Now: clk.Now})
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 6)
	srv := serve(p)

	a := media3(t, srv, "/live/s/cmaf/a1.m3u8")
	require.Len(t, a.Segments, 6)
	assert.EqualValues(t, 0, a.MediaSequence)
	for i, s := range a.Segments {
		seg := snap.Segments[i]
		assert.Equal(t, seg.Discontinuity, s.Discontinuity, "entry %d", i)
		if seg.Epoch != 2 {
			assert.False(t, s.Gap, "entry %d", i)
			continue
		}
		assert.True(t, s.Gap, "entry %d", i)
		assert.Equal(t, "gap-1-"+itoa(seg.Seq)+".m4s", s.URI)
		assert.Equal(t, media.ToDuration(seg.Duration).Round(time.Millisecond), s.Duration)
	}
	assert.True(t, a.Segments[2].Discontinuity)
	assert.True(t, a.Segments[4].Discontinuity)
	assert.Equal(t, "1-2.m4s", a.Segments[4].URI, "parts keep their own numbering in the URI")
	v := media3(t, srv, "/live/s/cmaf/v0.m3u8")
	assert.Equal(t, discontinuities(v), discontinuities(a))
	assert.Equal(t, 2, discontinuities(a))

	// List one more segment on every refresh.
	w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8")
	for i, seg := range snap.Segments {
		e, _ := snap.Epoch(seg.Epoch)
		clk.Set(e.WallAt(seg.End))
		pls := w.check("listed " + itoa(uint64(i+1)))
		assert.Len(t, pls["/live/s/cmaf/a1.m3u8"].Segments, i+1)
	}
}

func discontinuities(pl hls.MediaPlaylist) int {
	n := 0
	for _, s := range pl.Segments {
		if s.Discontinuity {
			n++
		}
	}
	return n
}

func TestHLSGapEviction(t *testing.T) {
	h := hub.New(hub.Options{})
	ds := gapFeed(t, h)
	// Epoch 3 goes on for two more segments, so the gaps leave the window.
	ds = append(ds, synthAV(t, 3, 1080000)...)
	p := NewPackager(h, Options{Window: 3, Now: farFuture})
	srv := serve(p)
	w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8")
	lastSeq := int64(-1)
	sawEvictedRef := false
	for _, d := range ds {
		p.Handle(d)
		s := p.Snapshot()
		if len(s.Segments) == 0 {
			continue
		}
		seq := int64(s.Segments[len(s.Segments)-1].Seq)
		if seq == lastSeq {
			continue
		}
		lastSeq = seq
		a := w.check("after " + itoa(uint64(seq)))["/live/s/cmaf/a1.m3u8"]
		referenced := false
		for _, e := range a.Segments {
			if e.Gap {
				assert.Equal(t, "init-1-1.mp4", e.Map, "after %d: a gap keeps the map of the track's last part", seq)
				referenced = true
			}
		}
		_, inWindow := s.Epoch(1)
		code, _ := get(t, srv, "/live/s/cmaf/init-1-1.mp4")
		switch {
		case referenced:
			assert.Equal(t, http.StatusOK, code, "after %d: referenced by a gap", seq)
			sawEvictedRef = sawEvictedRef || !inWindow
		case !inWindow:
			assert.Equal(t, http.StatusNotFound, code, "after %d: referenced by nothing", seq)
		}
	}
	assert.True(t, sawEvictedRef, "a gap referenced init-1-1 after epoch 1 left the window")
	p.Flush()
	pls := w.check("flushed")
	a := pls["/live/s/cmaf/a1.m3u8"]
	assert.EqualValues(t, 5, a.MediaSequence)
	assert.EqualValues(t, 2, a.DiscontinuitySequence, "the gap at Seq 2 and Seq 4 carried the discontinuities")
	for _, e := range a.Segments {
		assert.False(t, e.Gap)
	}
	code, _ := get(t, srv, "/live/s/cmaf/init-1-1.mp4")
	assert.Equal(t, http.StatusNotFound, code)
}

// A playlist that holds only gaps, then gets real entries, never changes
// the map of a listed gap.
func TestHLSGapOnlyThenReal(t *testing.T) {
	h := hub.New(hub.Options{})
	ds := gapFeed(t, h)
	p := NewPackager(h, Options{Window: 2, Now: farFuture})
	srv := serve(p)
	w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8")
	lastSeq := int64(-1)
	gapOnly := false
	for _, d := range ds {
		p.Handle(d)
		s := p.Snapshot()
		if len(s.Segments) == 0 || int64(s.Segments[len(s.Segments)-1].Seq) == lastSeq {
			continue
		}
		lastSeq = int64(s.Segments[len(s.Segments)-1].Seq)
		a := w.check("after " + itoa(uint64(lastSeq)))["/live/s/cmaf/a1.m3u8"]
		only := true
		for _, e := range a.Segments {
			only = only && e.Gap
		}
		gapOnly = gapOnly || only
	}
	p.Flush()
	w.check("flushed")
	assert.True(t, gapOnly, "the window held only gaps at some refresh")
}

// R7: two discontinuities merged by loss in the audio track's part flags
// still count twice in its playlist.
func TestHLSLossMergedDiscontinuities(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	p := NewPackager(h, Options{Window: 10, Now: farFuture})
	handleAll(p, synthAV(t, 1, 0))
	video := synth(h264Config(t), 1, 360000, 50)
	video[0].Lost = 1
	handleAll(p, video)
	av := synthAV(t, 1, 540000)
	require.Equal(t, media.KindVideo, av[0].Frame.Kind())
	av[0].Lost = 1
	handleAll(p, av)
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	require.True(t, snap.Segments[2].Discontinuity)
	require.True(t, snap.Segments[3].Discontinuity)
	require.Len(t, snap.Segments[2].Parts, 1, "the segment between the losses has no audio")

	srv := serve(p)
	v := media3(t, srv, "/live/s/cmaf/v0.m3u8")
	a := media3(t, srv, "/live/s/cmaf/a1.m3u8")
	assert.Equal(t, 2, discontinuities(v))
	assert.Equal(t, 2, discontinuities(a))
	assert.Equal(t, discSeqs(v), discSeqs(a))
	assert.True(t, a.Segments[2].Gap)
}

func TestHLSGapOnlyPlaylistServed(t *testing.T) {
	h := hub.New(hub.Options{})
	ds := gapFeed(t, h)
	p := NewPackager(h, Options{Window: 2, Now: farFuture})
	// Stop after epoch 2: the window holds its two video-only segments.
	for _, d := range ds {
		if d.Frame.Epoch == 3 {
			break
		}
		p.Handle(d)
	}
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 2)
	require.Len(t, snap.Epochs, 1)
	srv := serve(p)

	m := master(t, srv)
	require.Len(t, m.Variants, 1)
	assert.Contains(t, m.Variants[0].Codecs, "mp4a.40.2")
	assert.Contains(t, m.Variants[0].Codecs, "avc1.")
	require.Len(t, m.Renditions, 1)
	assert.Equal(t, "a1.m3u8", m.Renditions[0].URI)

	code, _ := get(t, srv, "/live/s/cmaf/a1.m3u8")
	require.Equal(t, http.StatusOK, code)
	w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8")
	a := w.check("gap only")["/live/s/cmaf/a1.m3u8"]
	require.Len(t, a.Segments, 2)
	for _, s := range a.Segments {
		assert.True(t, s.Gap)
		assert.Equal(t, "init-1-1.mp4", s.Map, "the retained init of the track's last part")
	}
	code, _ = get(t, srv, "/live/s/cmaf/init-1-1.mp4")
	assert.Equal(t, http.StatusOK, code, "retained while a gap references it")

	// The video's resolution still comes from its parts in the window.
	assert.Equal(t, 320, m.Variants[0].Width)
}

func TestHLSGapOnlyVideoKeepsResolution(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(4 * time.Second)})
	p := NewPackager(h, Options{Window: 2, Now: farFuture})
	handleAll(p, synthAV(t, 1, 0))
	// Epoch 2 is audio only.
	for _, d := range synthAV(t, 2, 360000) {
		if d.Frame.Kind() == media.KindAudio {
			p.Handle(d)
		}
	}
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 1)
	srv := serve(p)
	m := master(t, srv)
	require.Len(t, m.Variants, 1)
	assert.Equal(t, "v0.m3u8", m.Variants[0].URI)
	assert.Equal(t, 320, m.Variants[0].Width)
	assert.Equal(t, 240, m.Variants[0].Height)
	assert.Contains(t, m.Variants[0].Codecs, "avc1.")
	v := media3(t, srv, "/live/s/cmaf/v0.m3u8")
	for _, s := range v.Segments {
		assert.True(t, s.Gap)
		assert.Equal(t, "init-0-1.mp4", s.Map)
	}
	code, _ := get(t, srv, "/live/s/cmaf/init-0-1.mp4")
	assert.Equal(t, http.StatusOK, code)
}

func TestHLSGapKeepsMap(t *testing.T) {
	t.Run("previous", func(t *testing.T) {
		p := gapScenario(t, Options{Window: 10, Now: farFuture})
		a := media3(t, serve(p), "/live/s/cmaf/a1.m3u8")
		want := []string{"init-1-1.mp4", "init-1-1.mp4", "init-1-1.mp4", "init-1-1.mp4", "init-1-3.mp4", "init-1-3.mp4"}
		got := make([]string, 0, len(a.Segments))
		for _, s := range a.Segments {
			got = append(got, s.Map)
		}
		assert.Equal(t, want, got)
	})
	t.Run("leading", func(t *testing.T) {
		// The gaps lead the window but keep the map of the track's last
		// part before them, retained after epoch 1 left the window.
		p := gapScenario(t, Options{Window: 4, Now: farFuture})
		srv := serve(p)
		a := media3(t, srv, "/live/s/cmaf/a1.m3u8")
		require.Len(t, a.Segments, 4)
		for i, s := range a.Segments {
			assert.Equal(t, i < 2, s.Gap, "entry %d", i)
			want := "init-1-3.mp4"
			if s.Gap {
				want = "init-1-1.mp4"
			}
			assert.Equal(t, want, s.Map, "entry %d", i)
		}
		_, ok := p.Snapshot().Epoch(1)
		require.False(t, ok)
		code, _ := get(t, srv, "/live/s/cmaf/init-1-1.mp4")
		assert.Equal(t, http.StatusOK, code)
	})
	t.Run("late track", func(t *testing.T) {
		// Audio first appears in epoch 2.
		h := hub.New(hub.Options{})
		h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
		h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(4 * time.Second)})
		p := NewPackager(h, Options{Window: 10, Now: farFuture})
		handleAll(p, synth(h264Config(t), 1, 0, 100))
		handleAll(p, synthAV(t, 2, 360000))
		p.Flush()
		srv := serve(p)
		a := media3(t, srv, "/live/s/cmaf/a1.m3u8")
		require.Len(t, a.Segments, 4)
		assert.EqualValues(t, 0, a.MediaSequence)
		for i, s := range a.Segments {
			assert.Equal(t, i < 2, s.Gap, "entry %d", i)
			if s.Gap {
				assert.Empty(t, s.Map, "entry %d: before the track's first part, no map", i)
			} else {
				assert.Equal(t, "init-1-2.mp4", s.Map, "entry %d", i)
			}
		}
		newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8").check("late track")
	})
	t.Run("late track refreshed", func(t *testing.T) {
		// Listed one segment per refresh, the leading gaps never gain a map.
		h := hub.New(hub.Options{})
		h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
		h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(4 * time.Second)})
		p := NewPackager(h, Options{Window: 10, Now: farFuture})
		srv := serve(p)
		w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8")
		aw := newWatch(t, srv, "/live/s/cmaf/a1.m3u8")
		segs := 0
		for _, d := range append(synth(h264Config(t), 1, 0, 100), synthAV(t, 2, 360000)...) {
			p.Handle(d)
			if n := len(p.Snapshot().Segments); n != segs {
				segs = n
				w.check("v " + itoa(uint64(n)))
				if code, _ := get(t, srv, "/live/s/cmaf/a1.m3u8"); code == http.StatusOK {
					aw.check("a " + itoa(uint64(n)))
				}
			}
		}
		p.Flush()
		a := aw.check("flushed")["/live/s/cmaf/a1.m3u8"]
		require.Len(t, a.Segments, 4)
		assert.Empty(t, a.Segments[0].Map)
		assert.Empty(t, a.Segments[1].Map)
	})
}

func TestHLSGapProgramDateTime(t *testing.T) {
	p := gapScenario(t, Options{Window: 10, Now: farFuture})
	snap := p.Snapshot()
	a := media3(t, serve(p), "/live/s/cmaf/a1.m3u8")
	require.Len(t, a.Segments, 6)
	e2, _ := snap.Epoch(2)
	seg2 := snap.Segments[2]
	assert.Equal(t, e2.WallAt(seg2.Start).Truncate(time.Millisecond).UnixMilli(), a.Segments[2].ProgramDateTime.UnixMilli(),
		"a gap at a discontinuity carries the segment's start")
	assert.True(t, a.Segments[3].ProgramDateTime.IsZero())

	// A gap as the first entry carries it too.
	p = gapScenario(t, Options{Window: 3, Now: farFuture})
	snap = p.Snapshot()
	a = media3(t, serve(p), "/live/s/cmaf/a1.m3u8")
	require.True(t, a.Segments[0].Gap)
	seg3 := snap.Segments[0]
	require.EqualValues(t, 3, seg3.Seq)
	e2, _ = snap.Epoch(2)
	assert.Equal(t, e2.WallAt(seg3.Start).Truncate(time.Millisecond).UnixMilli(), a.Segments[0].ProgramDateTime.UnixMilli())
	assert.False(t, a.Segments[1].ProgramDateTime.IsZero(), "epoch 3's first part")
}

// Every CMAF playlist's TARGETDURATION is computed like the HLS-TS one's,
// over the whole window, so renditions agree even when only one track has
// the long segment.
func TestHLSTargetDurationLikeTS(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(4 * time.Second)})
	h.SetAnchor(media.Anchor{Epoch: 3, Start: 720000, Wall: t0.Add(8 * time.Second)})
	p := NewPackager(h, Options{Window: 10, Now: farFuture})
	handleAll(p, synthAV(t, 1, 0))
	long := synth(h264Config(t), 2, 360000, 65) // one 2.6 s video-only segment
	long[50].Frame.Key, long[50].Frame.Data = false, [][]byte{{0x41, 0x9a, 50}}
	handleAll(p, long)
	handleAll(p, synthAV(t, 3, 720000))
	p.Flush()
	srv := serve(p)
	ts := media3(t, srv, "/live/s/hls/index.m3u8")
	assert.Equal(t, 3, ts.TargetDuration)
	for _, path := range []string{"/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8"} {
		pl := media3(t, srv, path)
		assert.Equal(t, ts.TargetDuration, pl.TargetDuration, path)
		for _, s := range pl.Segments {
			assert.LessOrEqual(t, int(s.Duration.Round(time.Second)/time.Second), pl.TargetDuration, path)
		}
	}
	assert.True(t, strings.HasPrefix(media3(t, srv, "/live/s/cmaf/a1.m3u8").Segments[2].URI, "gap-1-"))
}

// abrMasterGolden is buildMaster's output for packLadder's hub with ABR off,
// captured with the packager before ABR mode existed (be38a47), with
// synthLadder's per-track padding.
const abrMasterGolden = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,URI="a3.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=132482,RESOLUTION=640x360,CODECS="avc1.4D400D,mp4a.40.2",AUDIO="aud"
v0.m3u8
`

func TestABRMaster(t *testing.T) {
	p := packLadder(t, Options{ABR: true}, nil)
	assert.Zero(t, p.Errors())
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	m := master(t, serve(p))
	assert.True(t, m.Independent)
	require.Len(t, m.Renditions, 1)
	assert.Equal(t, hls.Rendition{Type: "AUDIO", GroupID: "aud", Name: "audio", URI: "a3.m3u8", Default: true}, m.Renditions[0])

	aet, ok := snap.Track(3)
	require.True(t, ok)
	require.Len(t, m.Variants, len(ladderSizes))
	for i, v := range m.Variants {
		et, ok := snap.Track(i)
		require.True(t, ok)
		assert.Equal(t, "v"+strconv.Itoa(i)+".m3u8", v.URI)
		assert.Equal(t, ladderSizes[i][0], v.Width)
		assert.Equal(t, ladderSizes[i][1], v.Height)
		assert.Equal(t, et.Codec+","+aet.Codec, v.Codecs)
		assert.True(t, strings.HasPrefix(v.Codecs, "avc1."))
		assert.True(t, strings.HasSuffix(v.Codecs, ",mp4a.40.2"))
		assert.Equal(t, "aud", v.Audio)

		// R-B5: the peak over segments of the variant's video part rate plus
		// the default audio part rate.
		peak := 1.0
		for _, seg := range snap.Segments {
			bw := 0.0
			for _, track := range []int{i, 3} {
				e, ok := trackPart(snap, seg, track)
				require.True(t, ok)
				bw += float64(len(e.part.Data)) * 8 / partDuration(e).Seconds()
			}
			peak = max(peak, bw)
		}
		assert.Equal(t, int(math.Ceil(peak)), v.Bandwidth, "variant %d", i)
		media3(t, serve(p), "/live/s/cmaf/"+v.URI)
	}
}

func TestABRTrackPlaylistsAligned(t *testing.T) {
	ds := synthLadder(t, 1, 0, 250)
	ds = append(ds, synthLadder(t, 2, 900000, 150)...)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 900000, Wall: t0.Add(11 * time.Second)})
	p := NewPackager(h, Options{Window: 100, Now: farFuture, ABR: true})
	handleAll(p, ds)
	p.Flush()
	srv := serve(p)

	ref := media3(t, srv, "/live/s/cmaf/v0.m3u8")
	require.Len(t, ref.Segments, 8)
	assert.Equal(t, 1, discontinuities(ref), "the epoch change")
	for _, path := range []string{"v1.m3u8", "v2.m3u8", "a3.m3u8"} {
		pl := media3(t, srv, "/live/s/cmaf/"+path)
		assert.Equal(t, ref.MediaSequence, pl.MediaSequence, path)
		assert.Equal(t, ref.DiscontinuitySequence, pl.DiscontinuitySequence, path)
		require.Len(t, pl.Segments, len(ref.Segments), path)
		for i, seg := range pl.Segments {
			assert.Equal(t, ref.Segments[i].Discontinuity, seg.Discontinuity, "%s entry %d", path, i)
			assert.False(t, seg.Gap, "%s entry %d", path, i)
		}
	}
}

func TestABRMasterNonABRUnchanged(t *testing.T) {
	p := packLadder(t, Options{}, nil)
	code, body := get(t, serve(p), "/live/s/cmaf/master.m3u8")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, abrMasterGolden, string(body))
	assert.Equal(t, abrMasterGolden, string(buildMaster(p.Snapshot())))
}

// tsTimeOffset is the offset the TS muxer adds to every stream time.
const tsTimeOffset = 126000

// variantTS returns a segment's TS of a variant.
func variantTS(seg SegmentInfo, track int) ([]byte, bool) {
	for _, v := range seg.VariantTS {
		if v.Track == track {
			return v.Data, true
		}
	}
	return nil, false
}

func tsMaster(t *testing.T, srv http.Handler) hls.MultivariantPlaylist {
	t.Helper()
	code, body := get(t, srv, "/live/s/hls/index.m3u8")
	require.Equal(t, http.StatusOK, code)
	pl, err := hls.ParseMultivariant(body)
	require.NoError(t, err, string(body))
	return pl
}

func TestABRTSMaster(t *testing.T) {
	p := packLadder(t, Options{ABR: true}, nil)
	assert.Zero(t, p.Errors())
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	m := tsMaster(t, serve(p))
	assert.Equal(t, 3, m.Version)
	assert.True(t, m.Independent)
	assert.Empty(t, m.Renditions, "audio is muxed into every variant")
	aet, ok := snap.Track(3)
	require.True(t, ok)
	require.Len(t, m.Variants, len(ladderSizes))
	for i, v := range m.Variants {
		et, ok := snap.Track(i)
		require.True(t, ok)
		assert.Equal(t, "v"+strconv.Itoa(i)+".m3u8", v.URI)
		assert.Equal(t, ladderSizes[i][0], v.Width)
		assert.Equal(t, ladderSizes[i][1], v.Height)
		assert.Equal(t, et.Codec+","+aet.Codec, v.Codecs)
		assert.Empty(t, v.Audio)

		// R-B5: the peak over segments of the variant TS's bitrate.
		peak := 1.0
		for _, seg := range snap.Segments {
			data, ok := variantTS(seg, i)
			require.True(t, ok)
			peak = max(peak, float64(len(data))*8/media.ToDuration(seg.Duration).Seconds())
		}
		assert.Equal(t, int(math.Ceil(peak)), v.Bandwidth, "variant %d", i)
		if i > 0 {
			assert.Less(t, v.Bandwidth, m.Variants[i-1].Bandwidth, "rung %d is smaller than rung %d", i, i-1)
		}
	}
}

func TestABRTSVariants(t *testing.T) {
	ds := synthLadder(t, 1, 0, 250)
	ds = append(ds, synthLadder(t, 2, 900000, 150)...)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 900000, Wall: t0.Add(11 * time.Second)})
	p := NewPackager(h, Options{Window: 6, Now: farFuture, ABR: true})
	handleAll(p, ds)
	p.Flush()
	assert.Zero(t, p.Errors())
	srv := serve(p)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 6)
	require.Equal(t, 6, snap.Listed)

	ref := media3(t, srv, "/live/s/hls/v0.m3u8")
	assert.Equal(t, 1, discontinuities(ref), "the epoch change")
	assert.Equal(t, snap.Segments[0].Seq, ref.MediaSequence)
	assert.Equal(t, snap.EvictedDiscontinuities, ref.DiscontinuitySequence)
	for track := range ladderSizes {
		path := "/live/s/hls/v" + strconv.Itoa(track) + ".m3u8"
		pl := media3(t, srv, path)
		assert.Equal(t, 3, pl.Version, path)
		assert.True(t, pl.Independent, path)
		assert.Equal(t, ref.MediaSequence, pl.MediaSequence, path)
		assert.Equal(t, ref.DiscontinuitySequence, pl.DiscontinuitySequence, path)
		assert.Equal(t, ref.TargetDuration, pl.TargetDuration, path)
		require.Len(t, pl.Segments, len(snap.Segments), path)
		for i, e := range pl.Segments {
			seg := snap.Segments[i]
			assert.Equal(t, seg.Discontinuity, e.Discontinuity, "%s entry %d", path, i)
			assert.Equal(t, ref.Segments[i].Discontinuity, e.Discontinuity, "%s entry %d", path, i)
			assert.False(t, e.Gap, "%s entry %d", path, i)
			assert.Equal(t, strconv.Itoa(track)+"-"+itoa(seg.Seq)+".ts", e.URI)
			assert.Equal(t, media.ToDuration(seg.Duration).Round(time.Millisecond), e.Duration)
			if i == 0 || seg.Discontinuity {
				ep, ok := snap.Epoch(seg.Epoch)
				require.True(t, ok)
				assert.True(t, e.ProgramDateTime.Equal(ep.WallAt(seg.Start).Truncate(time.Millisecond)), "%s entry %d", path, i)
			} else {
				assert.True(t, e.ProgramDateTime.IsZero(), "%s entry %d", path, i)
			}

			code, body := get(t, srv, "/live/s/hls/"+e.URI)
			require.Equal(t, http.StatusOK, code, e.URI)
			want, _ := variantTS(seg, track)
			assert.Equal(t, want, body)
			require.GreaterOrEqual(t, len(body), 2*188)
			pid := func(pkt []byte) int { return int(pkt[1]&0x1f)<<8 | int(pkt[2]) }
			assert.Equal(t, 0, pid(body[:188]), "%s starts with the PAT", e.URI)
			assert.Equal(t, byte(0x02), body[188+5], "%s: the PMT follows", e.URI)

			var dec mpegts.TimeDecoder
			dec.AnchorRaw()
			d, err := ts.NewDemuxerWithOptions(bytes.NewReader(body), ts.DemuxOptions{Decoder: &dec})
			require.NoError(t, err)
			var first *media.Frame
			for {
				f, err := d.Read()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				if first == nil && f.Kind() == media.KindVideo {
					first = &f
				}
			}
			assert.Equal(t, []uint16{0x100, 0x101}, d.TrackPIDs(), e.URI)
			require.NotNil(t, first, e.URI)
			assert.True(t, first.Key, e.URI)
			assert.Equal(t, seg.Start, first.PTS-tsTimeOffset, e.URI)
			nal := first.Data[len(first.Data)-1]
			assert.Len(t, nal, 4+ladderPad*(len(ladderSizes)-track), "%s carries track %d's frame", e.URI, track)
		}
	}
}

func TestABRTSMuxFailureLeavesGap(t *testing.T) {
	p := packLadder(t, Options{ABR: true}, func(ds []hub.Delivery) {
		for i, d := range ds {
			if d.Frame.Track == 1 && d.Frame.PTS == 110*3600 {
				ds[i].Frame.Data = nil // segment 2; a P-frame
			}
		}
	})
	assert.EqualValues(t, 1, p.Errors())
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	srv := serve(p)
	for track := range ladderSizes {
		pl := media3(t, srv, "/live/s/hls/v"+strconv.Itoa(track)+".m3u8")
		require.Len(t, pl.Segments, 5)
		for i, e := range pl.Segments {
			seq := itoa(snap.Segments[i].Seq)
			if track == 1 && i == 2 {
				assert.True(t, e.Gap)
				assert.Equal(t, "gap-1-"+seq+".ts", e.URI)
				assert.Equal(t, media.ToDuration(snap.Segments[i].Duration).Round(time.Millisecond), e.Duration)
				code, _ := get(t, srv, "/live/s/hls/"+e.URI)
				assert.Equal(t, http.StatusNotFound, code)
				continue
			}
			assert.False(t, e.Gap, "v%d entry %d", track, i)
			assert.Equal(t, strconv.Itoa(track)+"-"+seq+".ts", e.URI)
		}
	}
	_, ok := variantTS(snap.Segments[2], 1)
	assert.False(t, ok)
	cmaf := media3(t, srv, "/live/s/cmaf/v1.m3u8")
	require.Len(t, cmaf.Segments, 5)
	assert.False(t, cmaf.Segments[2].Gap, "fMP4 takes the frame")
	// The master still lists the variant, from the segments it has.
	assert.Len(t, tsMaster(t, srv).Variants, len(ladderSizes))
}

func TestABRTSConform(t *testing.T) {
	ds := synthLadder(t, 1, 0, 250)
	ds = append(ds, synthLadder(t, 2, 900000, 150)...)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 900000, Wall: t0.Add(11 * time.Second)})
	p := NewPackager(h, Options{Window: 100, Now: farFuture, ABR: true})
	handleAll(p, ds)
	p.End()
	srv := httptest.NewServer(serve(p))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := conform.ReadHLS(ctx, srv.URL+"/live/s/hls/index.m3u8", conform.ReadOptions{AlignedSequences: true, Once: true})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	assert.Zero(t, c.HLSGaps)
	kinds := map[media.Kind]map[int]bool{media.KindVideo: {}, media.KindAudio: {}}
	for _, f := range c.Frames {
		kinds[f.Kind()][f.Track] = true
	}
	assert.Len(t, kinds[media.KindVideo], 3, "one video track per variant")
	assert.Len(t, kinds[media.KindAudio], 3, "one audio capture per variant")
}

// A rung with no frames in a segment lists a gap there without an error; a
// second audio track joining later takes the next PID in every variant
// while the first keeps its own; a frame no variant can place fails every
// variant of its segment.
func TestABRTSVariantGaps(t *testing.T) {
	ds := synthLadder(t, 1, 0, 250)
	var kept []hub.Delivery
	for _, d := range ds {
		f := d.Frame
		if f.Track == 2 && f.PTS >= 50*3600 && f.PTS < 100*3600 {
			continue // segment 1: rung 2 missing
		}
		kept = append(kept, d)
		if f.Track == 3 && f.PTS >= 100*3600 {
			f.Track = 4 // a second audio track from segment 2 on
			kept = append(kept, hub.Delivery{Frame: f})
		}
	}
	// Segment 3: a frame on a track without a config.
	i := slices.IndexFunc(kept, func(d hub.Delivery) bool { return d.Frame.Track == 0 && d.Frame.PTS == 160*3600 })
	require.GreaterOrEqual(t, i, 0)
	kept = slices.Insert(kept, i+1, hub.Delivery{Frame: media.Frame{Track: 5, PTS: 160 * 3600, DTS: 160 * 3600, Key: true, Data: [][]byte{{1}}, Epoch: 1}})

	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture, ABR: true})
	handleAll(p, kept)
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	assert.EqualValues(t, 3+1, p.Errors(), "segment 3: three variants and the CMAF part of track 5")
	for i, seg := range snap.Segments {
		var tracks []int
		for _, v := range seg.VariantTS {
			tracks = append(tracks, v.Track)
		}
		switch i {
		case 1:
			assert.Equal(t, []int{0, 1}, tracks)
		case 3:
			assert.Empty(t, tracks)
		default:
			assert.Equal(t, []int{0, 1, 2}, tracks, "segment %d", i)
		}
	}
	srv := serve(p)
	pl := media3(t, srv, "/live/s/hls/v2.m3u8")
	require.Len(t, pl.Segments, 5)
	assert.True(t, pl.Segments[1].Gap)
	assert.Equal(t, "gap-2-1.ts", pl.Segments[1].URI)
	assert.True(t, pl.Segments[3].Gap)
	assert.False(t, pl.Segments[4].Gap)

	pids := func(data []byte) []uint16 {
		d, err := ts.NewDemuxer(bytes.NewReader(data))
		require.NoError(t, err)
		for {
			if _, err := d.Read(); errors.Is(err, io.EOF) {
				break
			}
		}
		return d.TrackPIDs()
	}
	for track := range ladderSizes {
		data, ok := variantTS(snap.Segments[0], track)
		require.True(t, ok)
		assert.Equal(t, []uint16{0x100, 0x101}, pids(data), "segment 0, variant %d", track)
		data, ok = variantTS(snap.Segments[4], track)
		require.True(t, ok)
		assert.Equal(t, []uint16{0x100, 0x101, 0x102}, pids(data), "segment 4, variant %d", track)
	}
	m := tsMaster(t, srv)
	require.Len(t, m.Variants, 3)
	assert.True(t, strings.HasSuffix(m.Variants[0].Codecs, ",mp4a.40.2"), m.Variants[0].Codecs)
}

// A respawn's shape: rung 0, the anchor, stops right after its keyframe at
// 4 s while rungs 1 and 2 go on to 6 s, where epoch 2 starts. The segment
// at 4 s closes without a cut: its Duration is rung 0's one frame, and
// each TS variant's EXTINF and BANDWIDTH use its own video's span, so
// conform finds every EXTINF matching its media.
func TestABRTSVariantOwnDuration(t *testing.T) {
	var ds []hub.Delivery
	for _, d := range synthLadder(t, 1, 0, 150) {
		if d.Frame.Track == 0 && d.Frame.PTS > 100*3600 {
			continue
		}
		ds = append(ds, d)
	}
	ds = append(ds, synthLadder(t, 2, 150*3600, 100)...)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 150 * 3600, Wall: t0.Add(6 * time.Second)})
	p := NewPackager(h, Options{Window: 100, Now: farFuture, ABR: true})
	handleAll(p, ds)
	p.End()
	require.Zero(t, p.Errors())
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	seg := snap.Segments[2]
	require.EqualValues(t, 100*3600, seg.Start)
	require.EqualValues(t, 3600, seg.Duration, "rung 0's one frame")
	want := map[int]int64{0: 3600, 1: 50 * 3600, 2: 50 * 3600}
	require.Len(t, seg.VariantTS, len(want))
	for _, v := range seg.VariantTS {
		assert.Equal(t, want[v.Track], v.Duration, "variant %d", v.Track)
	}
	srv := serve(p)
	for track, d := range want {
		pl := media3(t, srv, "/live/s/hls/v"+strconv.Itoa(track)+".m3u8")
		require.Len(t, pl.Segments, 5)
		assert.Equal(t, media.ToDuration(d), pl.Segments[2].Duration, "v%d EXTINF", track)
		assert.Equal(t, 2*time.Second, pl.Segments[1].Duration, "v%d EXTINF of a cut segment", track)
	}
	m := tsMaster(t, srv)
	require.Len(t, m.Variants, len(ladderSizes))
	for i, v := range m.Variants {
		peak := 1.0
		for _, seg := range snap.Segments {
			for _, vt := range seg.VariantTS {
				if vt.Track == i {
					peak = max(peak, float64(len(vt.Data))*8/media.ToDuration(vt.Duration).Seconds())
				}
			}
		}
		assert.Equal(t, int(math.Ceil(peak)), v.Bandwidth, "variant %d", i)
	}

	hs := httptest.NewServer(srv)
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := conform.ReadHLS(ctx, hs.URL+"/live/s/hls/index.m3u8", conform.ReadOptions{AlignedSequences: true, Once: true})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
}

// Every TS variant shows one TARGETDURATION (RFC 8216 section 6.2.4),
// whichever is fetched first: rungs 0 and 1 stop right after their
// keyframe at 4 s while rung 2 goes on for 6 s, past the 2 s target, until
// epoch 2, so only v2 lists a 6 s EXTINF.
func TestABRTSVariantsShareTargetDuration(t *testing.T) {
	pack := func() http.Handler {
		var ds []hub.Delivery
		for _, d := range synthLadder(t, 1, 0, 250) {
			if d.Frame.Track < 2 && d.Frame.PTS > 100*3600 {
				continue
			}
			ds = append(ds, d)
		}
		ds = append(ds, synthLadder(t, 2, 250*3600, 100)...)
		h := hub.New(hub.Options{})
		h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
		h.SetAnchor(media.Anchor{Epoch: 2, Start: 250 * 3600, Wall: t0.Add(10 * time.Second)})
		p := NewPackager(h, Options{Window: 100, Now: farFuture, ABR: true})
		handleAll(p, ds)
		p.End()
		require.Zero(t, p.Errors())
		seg := p.Snapshot().Segments[2]
		require.EqualValues(t, 3600, seg.Duration)
		return serve(p)
	}
	for _, first := range []int{0, 2} {
		srv := pack()
		order := []int{first}
		for track := range ladderSizes {
			if track != first {
				order = append(order, track)
			}
		}
		for _, track := range order {
			pl := media3(t, srv, "/live/s/hls/v"+strconv.Itoa(track)+".m3u8")
			assert.Equal(t, 6, pl.TargetDuration, "v%d fetched after v%d", track, first)
			if track == 2 {
				assert.Equal(t, 6*time.Second, pl.Segments[2].Duration, "v2's long EXTINF")
			}
		}
	}
}

// packFeed packages ds with the given anchors set on the hub, Window 100
// and Now farFuture unless opt sets them.
func packFeed(t *testing.T, opt Options, ds []hub.Delivery, anchors ...media.Anchor) *Packager {
	t.Helper()
	h := hub.New(hub.Options{})
	for _, a := range anchors {
		h.SetAnchor(a)
	}
	if opt.Window == 0 {
		opt.Window = 100
	}
	if opt.Now == nil {
		opt.Now = farFuture
	}
	p := NewPackager(h, opt)
	handleAll(p, ds)
	p.Flush()
	require.Zero(t, p.Errors())
	return p
}

// keepTracks returns the deliveries of ds on the given tracks, in order.
func keepTracks(ds []hub.Delivery, tracks ...int) []hub.Delivery {
	var out []hub.Delivery
	for _, d := range ds {
		if slices.Contains(tracks, d.Frame.Track) {
			out = append(out, d)
		}
	}
	return out
}

// avFeed packages synthAV(t, 1, 0): video track 0 and audio track 1.
func avFeed(t *testing.T, opt Options) *Packager {
	t.Helper()
	return packFeed(t, opt, synthAV(t, 1, 0), media.Anchor{Epoch: 1, Wall: t0})
}

// audioOnlyFeed packages synthAV(t, 1, 0)'s audio track 1 alone.
func audioOnlyFeed(t *testing.T, opt Options) *Packager {
	t.Helper()
	return packFeed(t, opt, keepTracks(synthAV(t, 1, 0), 1), media.Anchor{Epoch: 1, Wall: t0})
}

// ladderFeed packages synthLadder(t, 1, 0, 250)'s given tracks in ABR mode.
func ladderFeed(t *testing.T, opt Options, tracks ...int) *Packager {
	t.Helper()
	opt.ABR = true
	return packFeed(t, opt, keepTracks(synthLadder(t, 1, 0, 250), tracks...), media.Anchor{Epoch: 1, Wall: t0})
}

// The goldens below are the base code's output (75eb774, before the audio
// language existed) for the feeds they name; with no language declared the
// playlists must stay byte for byte the same (ledger ruling F1).

// avMasterGolden is cmaf/master.m3u8 for avFeed.
const avMasterGolden = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,URI="a1.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=12277,RESOLUTION=320x240,CODECS="avc1.4D400D,mp4a.40.2",AUDIO="aud"
v0.m3u8
`

// audioOnlyMasterGolden is cmaf/master.m3u8 for audioOnlyFeed.
const audioOnlyMasterGolden = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-STREAM-INF:BANDWIDTH=7213,CODECS="mp4a.40.2"
a1.m3u8
`

// tsMasterGolden is hls/index.m3u8 for ladderFeed's tracks 0, 1 and 3.
const tsMasterGolden = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=222592,RESOLUTION=640x360,CODECS="avc1.4D400D,mp4a.40.2"
v0.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=184992,RESOLUTION=426x240,CODECS="avc1.4D400D,mp4a.40.2"
v1.m3u8
`

// tsMasterNoAudioGolden is hls/index.m3u8 for ladderFeed's tracks 0 and 1.
const tsMasterNoAudioGolden = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=151904,RESOLUTION=640x360,CODECS="avc1.4D400D"
v0.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=114304,RESOLUTION=426x240,CODECS="avc1.4D400D"
v1.m3u8
`

// inVariantLine is R-L4's audio entry for audio carried in the variant.
func inVariantLine(lang string) string {
	return `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="audio",LANGUAGE="` + lang + `",DEFAULT=YES,AUTOSELECT=YES` + "\n"
}

// withInVariantAudio returns golden with R-L4's URI-less audio entry before
// the first variant and AUDIO="aud" on every variant, nothing else changed.
func withInVariantAudio(golden, lang string) string {
	var b strings.Builder
	added := false
	for _, line := range strings.SplitAfter(golden, "\n") {
		if rest, ok := strings.CutPrefix(line, "#EXT-X-STREAM-INF:"); ok {
			if !added {
				b.WriteString(inVariantLine(lang))
				added = true
			}
			line = "#EXT-X-STREAM-INF:" + strings.TrimSuffix(rest, "\n") + `,AUDIO="aud"` + "\n"
		}
		b.WriteString(line)
	}
	return b.String()
}

// withLanguage returns golden with LANGUAGE written after its rendition's NAME.
func withLanguage(golden, lang string) string {
	return strings.Replace(golden, `NAME="audio",`, `NAME="audio",LANGUAGE="`+lang+`",`, 1)
}

func TestMasterAudioLanguage(t *testing.T) {
	t.Run("video track 0, audio track 1", func(t *testing.T) {
		p := avFeed(t, Options{})
		srv := serve(p)
		_, body := get(t, srv, "/live/s/cmaf/master.m3u8")
		assert.Equal(t, avMasterGolden, string(body))

		p.SetAudioLanguage("vi")
		m := master(t, srv)
		require.Len(t, m.Renditions, 1)
		assert.Equal(t, hls.Rendition{Type: "AUDIO", GroupID: "aud", Name: "audio", Language: "vi", URI: "a1.m3u8", Default: true}, m.Renditions[0])
		_, body = get(t, srv, "/live/s/cmaf/master.m3u8")
		assert.Equal(t, withLanguage(avMasterGolden, "vi"), string(body))

		p.SetAudioLanguage("")
		_, body = get(t, srv, "/live/s/cmaf/master.m3u8")
		assert.Equal(t, avMasterGolden, string(body), "removed")
	})
	t.Run("ladder", func(t *testing.T) {
		_, body := get(t, serve(packLadder(t, Options{}, nil)), "/live/s/cmaf/master.m3u8")
		assert.Equal(t, abrMasterGolden, string(body))
		_, body = get(t, serve(packLadder(t, Options{AudioLanguage: "vi"}, nil)), "/live/s/cmaf/master.m3u8")
		assert.Equal(t, withLanguage(abrMasterGolden, "vi"), string(body))

		m := master(t, serve(packLadder(t, Options{ABR: true, AudioLanguage: "vi"}, nil)))
		require.Len(t, m.Renditions, 1)
		assert.Equal(t, "vi", m.Renditions[0].Language)
		assert.Equal(t, "a3.m3u8", m.Renditions[0].URI)
		require.Len(t, m.Variants, len(ladderSizes))
	})
	t.Run("an invalid snapshot value writes none", func(t *testing.T) {
		snap := avFeed(t, Options{}).Snapshot()
		snap.AudioLanguage = `vi",URI="x`
		assert.Equal(t, avMasterGolden, string(buildMaster(snap)))
	})
}

func TestMasterAudioOnlyLanguage(t *testing.T) {
	p := audioOnlyFeed(t, Options{})
	srv := serve(p)
	_, body := get(t, srv, "/live/s/cmaf/master.m3u8")
	assert.Equal(t, audioOnlyMasterGolden, string(body))
	assert.NotContains(t, string(body), "#EXT-X-MEDIA")
	assert.NotContains(t, string(body), "AUDIO=")

	p.SetAudioLanguage("vi")
	_, body = get(t, srv, "/live/s/cmaf/master.m3u8")
	assert.Equal(t, withInVariantAudio(audioOnlyMasterGolden, "vi"), string(body))
	assert.Contains(t, string(body), inVariantLine("vi"))
	m := master(t, srv)
	require.Len(t, m.Renditions, 1)
	assert.Equal(t, hls.Rendition{Type: "AUDIO", GroupID: "aud", Name: "audio", Language: "vi", Default: true}, m.Renditions[0])
	require.Len(t, m.Variants, 1)
	assert.Equal(t, "aud", m.Variants[0].Audio)
	assert.Equal(t, "a1.m3u8", m.Variants[0].URI)
}

func TestTSMasterAudioLanguage(t *testing.T) {
	t.Run("with audio", func(t *testing.T) {
		p := ladderFeed(t, Options{}, 0, 1, 3)
		srv := serve(p)
		_, body := get(t, srv, "/live/s/hls/index.m3u8")
		assert.Equal(t, tsMasterGolden, string(body))

		p.SetAudioLanguage("vi")
		_, body = get(t, srv, "/live/s/hls/index.m3u8")
		assert.Equal(t, withInVariantAudio(tsMasterGolden, "vi"), string(body))
		assert.Contains(t, string(body), inVariantLine("vi"))
		m := tsMaster(t, srv)
		assert.Equal(t, 3, m.Version)
		require.Len(t, m.Renditions, 1)
		assert.Equal(t, hls.Rendition{Type: "AUDIO", GroupID: "aud", Name: "audio", Language: "vi", Default: true}, m.Renditions[0])
		require.Len(t, m.Variants, 2)
		for _, v := range m.Variants {
			assert.Equal(t, "aud", v.Audio)
			assert.Equal(t, "avc1.4D400D,mp4a.40.2", v.Codecs)
		}
	})
	t.Run("without audio", func(t *testing.T) {
		p := ladderFeed(t, Options{AudioLanguage: "vi"}, 0, 1)
		_, body := get(t, serve(p), "/live/s/hls/index.m3u8")
		assert.Equal(t, tsMasterNoAudioGolden, string(body))
		assert.NotContains(t, string(body), "#EXT-X-MEDIA")
		assert.NotContains(t, string(body), "AUDIO=")
	})
	t.Run("without ABR there is no master", func(t *testing.T) {
		p := avFeed(t, Options{AudioLanguage: "vi"})
		_, body := get(t, serve(p), "/live/s/hls/index.m3u8")
		assert.NotContains(t, string(body), "LANGUAGE")
		assert.Contains(t, string(body), "#EXTINF:")
	})
}

// TestAudioLanguageConforms is the writer x conform matrix: every master and
// MPD the packager writes with a declared language reads back through
// conform, expecting that language, with no audio-language violation and
// exactly that language read.
func TestAudioLanguageConforms(t *testing.T) {
	const lang = "vi"
	// twoAudioLadder is synthLadder's ladder with its audio track copied onto
	// a second audio track, packaged in ABR mode.
	twoAudioLadder := func(t *testing.T, opt Options) *Packager {
		t.Helper()
		audio := len(ladderSizes)
		var ds []hub.Delivery
		for _, d := range synthLadder(t, 1, 0, 250) {
			ds = append(ds, d)
			if d.Frame.Track == audio {
				d.Frame.Track = audio + 1
				ds = append(ds, d)
			}
		}
		opt.ABR = true
		return packFeed(t, opt, ds, media.Anchor{Epoch: 1, Wall: t0})
	}
	const (
		cmafMaster = "/live/s/cmaf/master.m3u8"
		mpd        = "/live/s/cmaf/index.mpd"
		tsMaster   = "/live/s/hls/index.m3u8"
	)
	for _, tc := range []struct {
		name string
		pack func(t *testing.T, opt Options) *Packager
		abr  bool
		path string
	}{
		{"audio only, CMAF master", audioOnlyFeed, false, cmafMaster},
		{"audio only, MPD", audioOnlyFeed, false, mpd},
		{"audio only ABR, CMAF master", audioOnlyFeed, true, cmafMaster},
		{"audio only ABR, MPD", audioOnlyFeed, true, mpd},
		{"A/V, CMAF master", avFeed, false, cmafMaster},
		{"A/V, MPD", avFeed, false, mpd},
		{"ladder ABR, TS master", func(t *testing.T, opt Options) *Packager { return ladderFeed(t, opt, 0, 1, 2, 3) }, true, tsMaster},
		{"ladder ABR, CMAF master", func(t *testing.T, opt Options) *Packager { return ladderFeed(t, opt, 0, 1, 2, 3) }, true, cmafMaster},
		{"ladder ABR, MPD", func(t *testing.T, opt Options) *Packager { return ladderFeed(t, opt, 0, 1, 2, 3) }, true, mpd},
		{"two audio, CMAF master", twoAudioFeed, false, cmafMaster},
		{"two audio ABR, TS master", twoAudioLadder, true, tsMaster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pack(t, Options{ABR: tc.abr, AudioLanguage: lang})
			srv := httptest.NewServer(serve(p))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			opt := conform.ReadOptions{Once: true, AudioLanguage: lang}
			var c conform.Capture
			var err error
			if strings.HasSuffix(tc.path, ".mpd") {
				c, err = conform.ReadDASH(ctx, srv.URL+tc.path, opt)
			} else {
				c, err = conform.ReadHLS(ctx, srv.URL+tc.path, opt)
			}
			require.NoError(t, err)
			assert.Equal(t, []string{lang}, c.AudioLanguages)
			// Only rule audio-language is judged here: the synthetic
			// fixtures may break unrelated rules, which are filtered out by
			// rule name only.
			// No rule needs filtering out: these fixtures break none, so every
			// rule is judged, audio-language among them.
			assert.Empty(t, c.Violations)
			assert.True(t, slices.ContainsFunc(c.Frames, func(f media.Frame) bool { return f.Kind() == media.KindAudio }), "audio read")
		})
	}
}
