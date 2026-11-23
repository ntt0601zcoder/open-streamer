package live

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
}

func entryOf(s hls.Segment) entry {
	return entry{s.URI, s.Duration, s.Discontinuity, s.Gap}
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
	p := NewPackager(h, Options{Window: 3, Now: farFuture})
	srv := serve(p)
	w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8")
	lastSeq := int64(-1)
	for _, d := range ds {
		p.Handle(d)
		s := p.Snapshot()
		if len(s.Segments) == 0 {
			continue
		}
		if seq := int64(s.Segments[len(s.Segments)-1].Seq); seq != lastSeq {
			lastSeq = seq
			w.check("after " + itoa(uint64(seq)))
		}
	}
	p.Flush()
	pls := w.check("flushed")
	a := pls["/live/s/cmaf/a1.m3u8"]
	assert.EqualValues(t, 3, a.MediaSequence)
	assert.EqualValues(t, 1, a.DiscontinuitySequence, "the gap at Seq 2 carried epoch 2's discontinuity")
	require.Len(t, a.Segments, 3)
	assert.True(t, a.Segments[0].Gap)
	assert.Equal(t, "init-1-3.mp4", a.Segments[0].Map, "a leading gap takes the first real entry's map")
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

	code, body := get(t, srv, "/live/s/cmaf/a1.m3u8")
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, string(body), "EXT-X-MAP", "gap entries are never loaded")
	w := newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8")
	a := w.check("gap only")["/live/s/cmaf/a1.m3u8"]
	require.Len(t, a.Segments, 2)
	for _, s := range a.Segments {
		assert.True(t, s.Gap)
		assert.Empty(t, s.Map)
	}

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
		assert.Empty(t, s.Map)
	}
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
		p := gapScenario(t, Options{Window: 4, Now: farFuture})
		a := media3(t, serve(p), "/live/s/cmaf/a1.m3u8")
		require.Len(t, a.Segments, 4)
		for i, s := range a.Segments {
			assert.Equal(t, i < 2, s.Gap, "entry %d", i)
			assert.Equal(t, "init-1-3.mp4", s.Map, "entry %d", i)
		}
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
			assert.Equal(t, "init-1-2.mp4", s.Map, "entry %d", i)
		}
		newWatch(t, srv, "/live/s/cmaf/v0.m3u8", "/live/s/cmaf/a1.m3u8").check("late track")
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
