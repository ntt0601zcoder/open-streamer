package ingest_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

// cmafTracks is h264_aac.ts cut into single-track CMAF segments, one per
// GOP, as a packager with a separate audio rendition writes them.
type cmafTracks struct {
	vInit, aInit   []byte
	vSegs, aSegs   [][]byte
	vCount, aCount int   // frames over all segments
	aPer           []int // audio frames of each segment
}

func cmafFixture(t *testing.T) cmafTracks {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(httpFixture(t)))
	require.NoError(t, err)
	var video, audio []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		// Fixture DTS start negative; tfdt is unsigned.
		f.PTS += 900000
		f.DTS += 900000
		if f.Config.Codec.Kind() == media.KindVideo {
			video = append(video, f)
		} else {
			audio = append(audio, f)
		}
	}
	var c cmafTracks
	c.vInit, err = fmp4.Init(video[0].Config)
	require.NoError(t, err)
	c.aInit, err = fmp4.Init(audio[0].Config)
	require.NoError(t, err)
	var starts []int64 // DTS of each GOP's keyframe
	for _, f := range video {
		if f.Key {
			starts = append(starts, f.DTS)
		}
	}
	require.Greater(t, len(starts), 3)
	vg, ag := make([][]media.Frame, len(starts)), make([][]media.Frame, len(starts))
	gop := func(dts int64) int {
		k := 0
		for i, s := range starts {
			if dts >= s {
				k = i
			}
		}
		return k
	}
	for _, f := range video {
		vg[gop(f.DTS)] = append(vg[gop(f.DTS)], f)
	}
	for _, f := range audio {
		ag[gop(f.DTS)] = append(ag[gop(f.DTS)], f)
	}
	for k := range starts {
		vs, _, err := fmp4.Segment(vg[k], video[0].Config, uint32(k+1))
		require.NoError(t, err)
		as, _, err := fmp4.Segment(ag[k], audio[0].Config, uint32(k+1))
		require.NoError(t, err)
		c.vSegs, c.aSegs = append(c.vSegs, vs), append(c.aSegs, as)
		c.vCount += len(vg[k])
		c.aCount += len(ag[k])
		c.aPer = append(c.aPer, len(ag[k]))
	}
	return c
}

// fmp4Playlist lists n segments "<k>.m4s" under the map init.mp4, twice when
// twice is set, with a discontinuity before the second copy.
func fmp4Playlist(n int, twice, ended bool) []byte {
	pl := hls.MediaPlaylist{Version: 7, TargetDuration: 2, Ended: ended}
	copies := 1
	if twice {
		copies = 2
	}
	for c := range copies {
		for k := range n {
			pl.Segments = append(pl.Segments, hls.Segment{
				URI: fmt.Sprintf("%d.m4s", k), Duration: time.Second, Map: "init.mp4", Discontinuity: c > 0 && k == 0,
			})
		}
	}
	return pl.Encode()
}

// renditionMaster is a multivariant playlist with one video-only variant
// and a separate audio rendition: a non-default entry first, then the
// default one, which is the one to play.
func renditionMaster() []byte {
	return hls.MultivariantPlaylist{
		Version: 7,
		Renditions: []hls.Rendition{
			{Type: "AUDIO", GroupID: "aud", Name: "other", URI: "other.m3u8"},
			{Type: "AUDIO", GroupID: "aud", Name: "main", URI: "audio/index.m3u8", Default: true},
		},
		Variants: []hls.Variant{{URI: "video/index.m3u8", Bandwidth: 1000000, Audio: "aud"}},
	}.Encode()
}

// serveCMAF serves renditionMaster with the given video and audio media
// playlists over the fixture's segments.
func serveCMAF(t *testing.T, c cmafTracks, video, audio func() []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(renditionMaster()) })
	mux.HandleFunc("/video/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(video()) })
	mux.HandleFunc("/audio/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(audio()) })
	mux.HandleFunc("/video/init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(c.vInit) })
	mux.HandleFunc("/audio/init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(c.aInit) })
	seg := func(segs [][]byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var k int
			_, err := fmt.Sscanf(r.PathValue("n"), "%d.m4s", &k)
			if err != nil || k >= len(segs) || segs[k] == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(segs[k])
		}
	}
	mux.HandleFunc("/video/{n}", seg(c.vSegs))
	mux.HandleFunc("/audio/{n}", seg(c.aSegs))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

// requireDTSOrder checks that the frames between breaks are in DTS order
// across tracks.
func requireDTSOrder(t *testing.T, res pullResult) {
	t.Helper()
	cuts := append(append([]int{0}, res.breaks...), len(res.frames))
	for s := 1; s < len(cuts); s++ {
		part := res.frames[cuts[s-1]:cuts[s]]
		for i := 1; i < len(part); i++ {
			require.GreaterOrEqual(t, part[i].DTS, part[i-1].DTS, "section %d frame %d (track %d) after track %d",
				s-1, i, part[i].Track, part[i-1].Track)
		}
	}
}

func TestHLSPullVariantSelection(t *testing.T) {
	segs := tsSegments(t)
	master := hls.MultivariantPlaylist{Version: 3, Variants: []hls.Variant{
		{URI: "low/index.m3u8", Bandwidth: 500000},
		{URI: "high/index.m3u8", Bandwidth: 2000000},
		{URI: "mid/index.m3u8", Bandwidth: 1000000},
	}}.Encode()
	var (
		mu    sync.Mutex
		paths []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		_, _ = w.Write(master)
	})
	mux.HandleFunc("/{name}/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		_, _ = w.Write(vodPlaylist(2, []string{"0.ts", "1.ts"}))
	})
	mux.HandleFunc("/{name}/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		_, _ = w.Write(segs[n])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, tc := range []struct{ query, want string }{
		{"", "/high/index.m3u8"},
		{"?variant=0", "/low/index.m3u8"},
		{"?variant=2&token=abc", "/mid/index.m3u8"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			mu.Lock()
			paths = nil
			mu.Unlock()
			r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8"+tc.query, ingest.Env{})
			require.NoError(t, err)
			res := pullAll(r)
			require.ErrorIs(t, res.err, io.EOF)
			require.NoError(t, r.Close())
			assert.Equal(t, fixtureTrackCounts(t, bytes.Join(segs[:2], nil)), trackCounts(res.frames))
			assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, paths, 2)
			assert.NotContains(t, paths[0], "variant=", "the selector is ours, not the server's")
			assert.Equal(t, tc.want, paths[1])
		})
	}
	for _, q := range []string{"?variant=3", "?variant=-1", "?variant=x"} {
		_, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8"+q, ingest.Env{})
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), "variant", q)
	}
}

func TestHLSPullMuxedFMP4(t *testing.T) {
	file := fixtureFile(t, "h264_aac_frag.mp4")
	init, frags := splitFMP4(t, file)
	require.Greater(t, len(frags), 3)

	// The reference: one demuxer over every fragment.
	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	want := map[int]int{}
	for _, f := range frags {
		out, err := d.Segment(f)
		require.NoError(t, err)
		for _, fr := range out {
			want[fr.Track]++
		}
	}
	require.Len(t, want, 2)

	// The fragments twice: the second copy under another map URI, after a
	// discontinuity.
	pl := hls.MediaPlaylist{Version: 7, TargetDuration: 2, Ended: true}
	for c, m := range []string{"init.mp4", "init-b.mp4"} {
		for k := range frags {
			pl.Segments = append(pl.Segments, hls.Segment{
				URI: fmt.Sprintf("f%d.m4s", k), Duration: time.Second, Map: m, Discontinuity: c > 0 && k == 0,
			})
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pl.Encode()) })
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(init) })
	mux.HandleFunc("/init-b.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(init) })
	mux.HandleFunc("/{n}", func(w http.ResponseWriter, r *http.Request) {
		var k int
		_, _ = fmt.Sscanf(r.PathValue("n"), "f%d.m4s", &k)
		_, _ = w.Write(frags[k])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1, "one ErrDiscontinuity at the new map")
	before, after := res.frames[:res.breaks[0]], res.frames[res.breaks[0]:]
	assert.Equal(t, want, trackCounts(before))
	assert.Equal(t, want, trackCounts(after))
	requireDTSOrder(t, res)

	// One Config pointer per track across both demuxers.
	configs := map[int]map[*media.TrackConfig]bool{0: {}, 1: {}}
	for _, f := range res.frames {
		configs[f.Track][f.Config] = true
	}
	assert.Len(t, configs[0], 1)
	assert.Len(t, configs[1], 1)
	assert.EqualValues(t, 2*len(frags), r.Stats()["segments"])
}

// splitFMP4 returns the init segment of a fragmented file (every box up to
// and including moov) and its fragments (each moof with the boxes after it).
func splitFMP4(t *testing.T, b []byte) (init []byte, frags [][]byte) {
	t.Helper()
	inInit := true
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 8)
		n := int(binary.BigEndian.Uint32(b))
		require.LessOrEqual(t, n, len(b))
		box, typ := b[:n], string(b[4:8])
		b = b[n:]
		if inInit {
			init = append(init, box...)
			inInit = typ != "moov"
			continue
		}
		if typ == "moof" || len(frags) == 0 {
			frags = append(frags, nil)
		}
		frags[len(frags)-1] = append(frags[len(frags)-1], box...)
	}
	return init, frags
}

// A discontinuity at the same place in the video and the audio playlist is
// one break for the reader.
func TestHLSPullRenditionDiscontinuity(t *testing.T) {
	c := cmafFixture(t)
	n := len(c.vSegs)
	srv := serveCMAF(t, c,
		func() []byte { return fmp4Playlist(n, true, true) },
		func() []byte { return fmp4Playlist(n, true, true) })
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds(), "the audio rendition is the next track")
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1)
	want := map[int]int{0: c.vCount, 1: c.aCount}
	assert.Equal(t, want, trackCounts(res.frames[:res.breaks[0]]))
	assert.Equal(t, want, trackCounts(res.frames[res.breaks[0]:]))
	requireDTSOrder(t, res)
	st := r.Stats()
	assert.EqualValues(t, 4*n, st["segments"], "both playlists")
	assert.Zero(t, st["dropped_unaligned"])
}

// pumps counts the goroutines running a playlist loop.
func pumps() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "ingest.(*hlsSource).pump(")
}

// Not parallel: it inspects goroutines.
func TestHLSPullRenditionGoroutineExit(t *testing.T) {
	c := cmafFixture(t)
	// Live playlists that never grow: both loops end up waiting on reloads.
	srv := serveCMAF(t, c,
		func() []byte { return fmp4Playlist(3, false, false) },
		func() []byte { return fmp4Playlist(3, false, false) })
	defer srv.Close()

	require.Zero(t, pumps())
	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	for range 10 {
		_, err := r.Read()
		require.NoError(t, err)
	}
	assert.Equal(t, 2, pumps(), "one loop per playlist")
	done := make(chan pullResult, 1)
	go func() { done <- pullAll(r) }()
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, r.Close())
	select {
	case res := <-done:
		require.Error(t, res.err)
		assert.NotErrorIs(t, res.err, io.EOF)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
	assert.Eventually(t, func() bool { return pumps() == 0 }, time.Second, 10*time.Millisecond, "both loops gone")
}

func TestHLSPullOurCMAF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	h := hub.New(hub.Options{})
	sub := h.Subscribe("packager", 4096)
	p := live.NewPackager(h, live.Options{Target: media.ClockRate})
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.Run(ctx, sub)
	}()
	go func() {
		defer wg.Done()
		defer h.Close()
		err := pipeline.New(h, pipeline.Options{
			Inputs: []string{"file://" + mustAbs(t, httpFixturePath()) + "?loop=1"},
		}).Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("feed: %v", err)
		}
	}()
	srv := httptest.NewServer(live.Handler(func(name string) (*live.Packager, bool) { return p, name == "s" }))
	defer srv.Close()

	require.Eventually(t, func() bool { return p.Snapshot().Listed >= 3 }, 15*time.Second, 50*time.Millisecond)
	r, err := ingest.Open(context.Background(), srv.URL+"/live/s/cmaf/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, ingest.Spec{Pace: true, Live: true, StallTimeout: 1500 * time.Millisecond}, r.Spec())
	require.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())

	// Read past the loop point (6 s): the packager's discontinuity reaches
	// both playlists, and the reader reports it once.
	c := clock.New(clock.Options{Kinds: r.Kinds()})
	var (
		out    []media.Frame
		res    pullResult
		counts = map[int]int{}
	)
	deadline := time.Now().Add(20 * time.Second)
	for counts[0] < 7*25 && time.Now().Before(deadline) {
		f, err := r.Read()
		if errors.Is(err, ingest.ErrDiscontinuity) {
			res.breaks = append(res.breaks, len(res.frames))
			c.Break(false)
			continue
		}
		require.NoError(t, err)
		res.frames = append(res.frames, f)
		counts[f.Track]++
		out = append(out, c.PushAt(f.Frame, f.Arrival)...)
	}
	require.NoError(t, r.Close())
	out = append(out, c.Flush()...)
	require.GreaterOrEqual(t, counts[0], 7*25, "read seven seconds of video in time")
	assert.Greater(t, counts[1], 6*40, "and the audio rendition alongside")
	assert.NotEmpty(t, res.breaks, "the loop is an EXT-X-DISCONTINUITY")
	requireDTSOrder(t, res)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
	st := r.Stats()
	assert.Zero(t, st["fell_behind"])
	assert.Zero(t, st["skipped_segments"])
}

// A break on one playlist only (an audio segment that cannot be fetched) is
// one ErrDiscontinuity: the video up to where the audio stopped comes
// before it, and after it both start together again.
func TestHLSPullRenditionOneSidedBreak(t *testing.T) {
	c := cmafFixture(t)
	n := len(c.vSegs)
	lost := 2
	segs := slices.Clone(c.aSegs)
	segs[lost] = nil // served as 404
	c.aSegs = segs
	srv := serveCMAF(t, c,
		func() []byte { return fmp4Playlist(n, false, true) },
		func() []byte { return fmp4Playlist(n, false, true) })
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1)
	requireDTSOrder(t, res)
	st := r.Stats()
	assert.EqualValues(t, 1, st["skipped_segments"])
	got := trackCounts(res.frames)
	assert.Equal(t, c.aCount-c.aPer[lost], got[1], "every audio frame but the lost segment's")
	assert.EqualValues(t, c.vCount, got[0]+int(st["dropped_unaligned"]), "video delivered or dropped to realign")
	assert.Less(t, int(st["dropped_unaligned"]), c.vCount/n, "at most a segment of video dropped")
}

// Playlists joined at different points are aligned at the start: the one
// that starts more than a second earlier loses its head, with no break.
func TestHLSPullRenditionAlignment(t *testing.T) {
	c := cmafFixture(t)
	n := len(c.vSegs)
	video := hls.MediaPlaylist{Version: 7, TargetDuration: 2, Ended: true}
	for k := 2; k < n; k++ {
		video.Segments = append(video.Segments, hls.Segment{URI: fmt.Sprintf("%d.m4s", k), Duration: time.Second, Map: "init.mp4"})
	}
	srv := serveCMAF(t, c,
		func() []byte { return video.Encode() },
		func() []byte { return fmp4Playlist(n, false, true) })
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.Empty(t, res.breaks)
	requireDTSOrder(t, res)
	var firstVideo int64
	for _, f := range res.frames {
		if f.Track == 0 {
			firstVideo = f.DTS
			break
		}
	}
	for _, f := range res.frames {
		if f.Track == 1 {
			assert.GreaterOrEqual(t, f.DTS, firstVideo-90000, "audio starts at most a second before the video")
			break
		}
	}
	dropped := int(r.Stats()["dropped_unaligned"])
	assert.Positive(t, dropped)
	assert.Equal(t, c.aCount, trackCounts(res.frames)[1]+dropped, "only audio dropped")
}

// An fMP4 segment that does not demux is lost: one break, counted.
func TestHLSPullFMP4BadSegment(t *testing.T) {
	init, frags := splitFMP4(t, fixtureFile(t, "h264_aac_frag.mp4"))
	bad := 2
	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	want := map[int]int{}
	for k, f := range frags {
		out, err := d.Segment(f)
		require.NoError(t, err)
		if k != bad {
			for _, fr := range out {
				want[fr.Track]++
			}
		}
	}
	pl := hls.MediaPlaylist{Version: 7, TargetDuration: 2, Ended: true}
	for k := range frags {
		pl.Segments = append(pl.Segments, hls.Segment{URI: fmt.Sprintf("f%d.m4s", k), Duration: time.Second, Map: "init.mp4"})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pl.Encode()) })
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(init) })
	mux.HandleFunc("/{n}", func(w http.ResponseWriter, r *http.Request) {
		var k int
		_, _ = fmt.Sscanf(r.PathValue("n"), "f%d.m4s", &k)
		if k == bad {
			_, _ = w.Write(frags[k][:100]) // truncated
			return
		}
		_, _ = w.Write(frags[k])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1)
	assert.Equal(t, want, trackCounts(res.frames))
	assert.EqualValues(t, 1, r.Stats()["bad_segments"])
}
