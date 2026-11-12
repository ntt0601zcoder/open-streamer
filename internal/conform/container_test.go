package conform

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
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
	b, err := os.ReadFile("../../testdata/fixtures/" + name)
	require.NoError(t, err)
	return b
}

// runClip runs the unpaced pipeline over a fixture into h, anchors at t0.
func runClip(t *testing.T, h *hub.Hub, name string) {
	t.Helper()
	b := fixture(t, name)
	require.NoError(t, pipeline.Run(context.Background(), h, pipeline.Options{
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
		Now:  func() time.Time { return t0 },
	}))
}

// packClip packages a whole fixture; the packager clock is now.
func packClip(t *testing.T, name string, window int, now func() time.Time) *live.Packager {
	t.Helper()
	h := hub.New(hub.Options{})
	sub := h.Subscribe("pkg", 1<<16)
	runClip(t, h, name)
	h.Close()
	p := live.NewPackager(h, live.Options{Window: window, Now: now})
	p.Run(context.Background(), sub)
	return p
}

func farFuture() time.Time { return t0.Add(time.Hour) }

// serveLive serves a packager as stream "s", through wrap when non-nil.
func serveLive(t *testing.T, p *live.Packager, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	h := live.Handler(func(name string) (*live.Packager, bool) { return p, name == "s" })
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/live/s/"
}

// rewrite returns a middleware that passes path's body through edit; n
// counts the requests to path, from 1.
func rewrite(path string, edit func(body []byte, n int) []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		var mu sync.Mutex
		n := 0
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, path) {
				next.ServeHTTP(w, r)
				return
			}
			mu.Lock()
			n++
			k := n
			mu.Unlock()
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			for key, v := range rec.Header() {
				if key != "Content-Length" {
					w.Header()[key] = v
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(edit(rec.Body.Bytes(), k))
		})
	}
}

// notFound answers 404 for path.
func notFound(path string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, path) {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rules returns the distinct rule names of the violations, sorted.
func rules(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		if !slices.Contains(out, v.Rule) {
			out = append(out, v.Rule)
		}
	}
	slices.Sort(out)
	return out
}

func once() ReadOptions { return ReadOptions{Once: true} }

func countKind(frames []media.Frame, k media.Kind) int {
	n := 0
	for _, f := range frames {
		if f.Kind() == k {
			n++
		}
	}
	return n
}

// readHTTPTS serves a fixture's pipeline output over HTTP-TS and reads it
// back to EOF.
func readHTTPTS(t *testing.T, name string) Capture {
	t.Helper()
	h := hub.New(hub.Options{})
	srv := httptest.NewServer(httpts.Handler(func(string) (*hub.Hub, bool) { return h, true }, 1<<16))
	t.Cleanup(srv.Close)
	type result struct {
		c   Capture
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ReadTS(context.Background(), srv.URL+"/live/s/stream.ts", ReadOptions{})
		ch <- result{c, err}
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)
	runClip(t, h, name)
	h.Close()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.c
	case <-time.After(10 * time.Second):
		t.Fatal("ReadTS did not finish")
		return Capture{}
	}
}

func TestReadPackagerOutputsClean(t *testing.T) {
	for _, name := range []string{
		"h264_aac.ts", "h264_aac_then_640.ts", "h264_aac_loop.ts", "h265_aac51.ts",
		"h264_aacmono_wrap.ts", "h264_aac_midgop.ts", "h264_640.ts",
	} {
		t.Run(name, func(t *testing.T) {
			p := packClip(t, name, 100, farFuture)
			snap := p.Snapshot()
			require.NotZero(t, snap.Listed)
			base := serveLive(t, p, nil)
			ctx := context.Background()
			var video int
			for _, seg := range snap.Segments {
				for _, part := range seg.Parts {
					if et, ok := epochTrack(snap, seg.Epoch, part.Track); ok && et.Config.Codec.Kind() == media.KindVideo {
						video += countFrames(t, part.Data, et.Init)
					}
				}
			}

			for _, tc := range []struct {
				name string
				read func() (Capture, error)
			}{
				{"hls-ts", func() (Capture, error) { return ReadHLS(ctx, base+"hls/index.m3u8", once()) }},
				{"hls-fmp4", func() (Capture, error) { return ReadHLS(ctx, base+"cmaf/master.m3u8", once()) }},
				{"dash", func() (Capture, error) { return ReadDASH(ctx, base+"cmaf/index.mpd", once()) }},
			} {
				c, err := tc.read()
				require.NoError(t, err, tc.name)
				assert.Empty(t, c.Violations, tc.name)
				assert.NotZero(t, countKind(c.Frames, media.KindAudio)+countKind(c.Frames, media.KindVideo), tc.name)
				if video > 0 {
					assert.Equal(t, video, countKind(c.Frames, media.KindVideo), tc.name)
				}
			}
			c := readHTTPTS(t, name)
			assert.Empty(t, c.Violations, "http-ts")
			assert.NotEmpty(t, c.Frames, "http-ts")
		})
	}
}

func epochTrack(s live.Snapshot, epoch uint32, track int) (live.EpochTrack, bool) {
	e, ok := s.Epoch(epoch)
	if !ok {
		return live.EpochTrack{}, false
	}
	return e.Track(track)
}

// countFrames counts the samples of a CMAF part.
func countFrames(t *testing.T, part, init []byte) int {
	t.Helper()
	in, err := parseInit(init)
	require.NoError(t, err)
	seg, err := parseSegment(part, in)
	require.NoError(t, err)
	return seg.samples
}

func TestReadMidEpoch(t *testing.T) {
	p := packClip(t, "h265_aac51.ts", 2, farFuture)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 2)
	require.NotZero(t, snap.Segments[0].Seq, "the capture starts after the epoch's first segment")
	require.False(t, snap.Segments[0].Discontinuity)
	require.Equal(t, media.CodecH265, snap.Epochs[0].Tracks[0].Config.Codec)
	base := serveLive(t, p, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		read func() (Capture, error)
	}{
		{"hls-ts", func() (Capture, error) { return ReadHLS(ctx, base+"hls/index.m3u8", once()) }},
		{"hls-fmp4", func() (Capture, error) { return ReadHLS(ctx, base+"cmaf/master.m3u8", once()) }},
		{"dash", func() (Capture, error) { return ReadDASH(ctx, base+"cmaf/index.mpd", once()) }},
	} {
		c, err := tc.read()
		require.NoError(t, err, tc.name)
		assert.Empty(t, c.Violations, tc.name)
		assert.NotZero(t, countKind(c.Frames, media.KindVideo), tc.name)
	}
}

// TestReadMidEpochStartRules shows the start rules still run on the epochs
// after a mid-epoch first one: the loop fixture's second epoch is checked.
func TestReadMidEpochStartRules(t *testing.T) {
	p := packClip(t, "h264_aac_loop.ts", 4, farFuture)
	snap := p.Snapshot()
	require.NotZero(t, snap.Segments[0].Seq)
	require.Len(t, snap.Epochs, 2)
	// Drop the first audio part of epoch 2, so the audio starts a segment
	// late in that epoch.
	var drop string
	for _, seg := range snap.Segments {
		if seg.Epoch == 2 {
			for _, part := range seg.Parts {
				if part.Track == 1 {
					drop = fmt.Sprintf("cmaf/1-%d.m4s", part.N)
				}
			}
			break
		}
	}
	require.NotEmpty(t, drop)
	base := serveLive(t, p, notFound(drop))
	c, err := ReadHLS(context.Background(), base+"cmaf/master.m3u8", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RuleSegmentAvailable, RuleStartOffset}, rules(c.Violations))
}

// timedClip returns the frames of h264_aac.ts in stream time.
func timedClip(t *testing.T) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixture(t, "h264_aac.ts")))
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

// files serves fixed bodies by path; missing paths are 404.
func files(t *testing.T, m map[string][]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := m[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// fmp4Playlist hand-builds an HLS-fMP4 video playlist from the video frames
// of h264_aac.ts cut at the given frame indexes; extinf adjusts each
// segment's EXTINF in ticks.
func fmp4Playlist(t *testing.T, cuts []int, extinf map[int]int64) map[string][]byte {
	t.Helper()
	var video []media.Frame
	for _, f := range timedClip(t) {
		if f.Kind() == media.KindVideo {
			video = append(video, f)
		}
	}
	cfg := video[0].Config
	init, err := fmp4.Init(cfg)
	require.NoError(t, err)
	m := map[string][]byte{"/init.mp4": init}
	pl := hls.MediaPlaylist{Version: 7, TargetDuration: 4, Ended: true}
	for i := 0; i+1 < len(cuts); i++ {
		data, tm, err := fmp4.Segment(video[cuts[i]:cuts[i+1]], cfg, uint32(i+1))
		require.NoError(t, err)
		name := strconv.Itoa(i) + ".m4s"
		m["/"+name] = data
		pl.Segments = append(pl.Segments, hls.Segment{
			URI: name, Map: "init.mp4", Duration: media.ToDuration(int64(tm.Duration) + extinf[i]),
		})
	}
	m["/v.m3u8"] = pl.Encode()
	return m
}

// keyframes returns the indexes of the video keyframes of h264_aac.ts.
func keyframes(t *testing.T) []int {
	t.Helper()
	var out []int
	i := 0
	for _, f := range timedClip(t) {
		if f.Kind() != media.KindVideo {
			continue
		}
		if f.Key {
			out = append(out, i)
		}
		i++
	}
	return out
}

func TestReadSegmentStartsOnPFrame(t *testing.T) {
	keys := keyframes(t)
	require.GreaterOrEqual(t, len(keys), 3)
	good := fmp4Playlist(t, []int{0, keys[1], keys[2]}, nil)
	c, err := ReadHLS(context.Background(), files(t, good)+"/v.m3u8", once())
	require.NoError(t, err)
	assert.Empty(t, c.Violations)

	bad := fmp4Playlist(t, []int{0, keys[1] + 3, keys[2]}, nil)
	c, err = ReadHLS(context.Background(), files(t, bad)+"/v.m3u8", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RuleSegmentKey}, rules(c.Violations))
	require.Len(t, c.Violations, 1)
}

func TestReadExtinfOff(t *testing.T) {
	keys := keyframes(t)
	m := fmp4Playlist(t, []int{0, keys[1], keys[2]}, map[int]int64{0: 2 * 3600})
	c, err := ReadHLS(context.Background(), files(t, m)+"/v.m3u8", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RuleExtinf}, rules(c.Violations))
	require.Len(t, c.Violations, 1)

	// One frame off is within tolerance.
	m = fmp4Playlist(t, []int{0, keys[1], keys[2]}, map[int]int64{0: 3600})
	c, err = ReadHLS(context.Background(), files(t, m)+"/v.m3u8", once())
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
}

func TestReadExtinfOffTS(t *testing.T) {
	p := packClip(t, "h264_aac.ts", 100, farFuture)
	base := serveLive(t, p, rewrite("hls/index.m3u8", func(b []byte, _ int) []byte {
		pl, err := hls.ParseMedia(b)
		require.NoError(t, err)
		pl.Segments[1].Duration += 80 * time.Millisecond
		return pl.Encode()
	}))
	c, err := ReadHLS(context.Background(), base+"hls/index.m3u8", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RuleExtinf}, rules(c.Violations))
	require.Len(t, c.Violations, 1)
	assert.Contains(t, c.Violations[0].Detail, "/1.ts")
}

func TestReadWrongCodecString(t *testing.T) {
	p := packClip(t, "h264_aac.ts", 100, farFuture)
	t.Run("dash", func(t *testing.T) {
		base := serveLive(t, p, rewrite("cmaf/index.mpd", func(b []byte, _ int) []byte {
			m, err := dash.Parse(b)
			require.NoError(t, err)
			m.Periods[0].AdaptationSets[0].Representations[0].Codecs = "avc1.4d401e"
			return m.Encode()
		}))
		c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleCodecString}, rules(c.Violations))
		require.Len(t, c.Violations, 1)
	})
	t.Run("hls", func(t *testing.T) {
		base := serveLive(t, p, rewrite("cmaf/master.m3u8", func(b []byte, _ int) []byte {
			m, err := hls.ParseMultivariant(b)
			require.NoError(t, err)
			m.Variants[0].Codecs = "avc1.4d401e,mp4a.40.2"
			return m.Encode()
		}))
		c, err := ReadHLS(context.Background(), base+"cmaf/master.m3u8", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleCodecString}, rules(c.Violations))
		require.Len(t, c.Violations, 1)
	})
}

func TestReadConfigChangeWithoutDiscontinuity(t *testing.T) {
	p := packClip(t, "h264_aac_then_640.ts", 100, farFuture)
	strip := func(b []byte, _ int) []byte {
		require.Contains(t, string(b), "#EXT-X-DISCONTINUITY\n")
		return bytes.ReplaceAll(b, []byte("#EXT-X-DISCONTINUITY\n"), nil)
	}
	want := []string{RuleConfigChange, RuleConfigSignalled}

	// fMP4: EXTINF is the summed sample durations, unaffected by the epoch.
	base := serveLive(t, p, rewrite("cmaf/v0.m3u8", strip))
	c, err := ReadHLS(context.Background(), base+"cmaf/v0.m3u8", once())
	require.NoError(t, err)
	assert.Equal(t, want, rules(c.Violations))

	// TS: without the discontinuity the segment before the change is
	// measured up to the next segment's keyframe, across the gap the Clock
	// leaves between epochs, so extinf-matches fires too.
	base = serveLive(t, p, rewrite("hls/index.m3u8", strip))
	c, err = ReadHLS(context.Background(), base+"hls/index.m3u8", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RuleConfigChange, RuleConfigSignalled, RuleExtinf}, rules(c.Violations))
}

// liveMPD serves a packager whose clock lists one more segment on every MPD
// request, with minimumUpdatePeriod cut to 100 ms; edit may alter the MPD.
func liveMPD(t *testing.T, edit func(m *dash.MPD, n int), extra ...func(http.Handler) http.Handler) (string, time.Duration) {
	t.Helper()
	clk := &fakeNow{t: farFuture()}
	p := packClip(t, "h264_aac.ts", 100, clk.Now)
	snap := p.Snapshot()
	ep := snap.Epochs[0]
	var total int64
	for _, seg := range snap.Segments {
		total += seg.Duration
	}
	ends := make([]time.Time, len(snap.Segments))
	for i, seg := range snap.Segments {
		ends[i] = ep.WallAt(seg.End)
	}
	clk.Set(ends[0])
	step := 0
	var mu sync.Mutex
	mpd := rewrite("cmaf/index.mpd", func(b []byte, n int) []byte {
		m, err := dash.Parse(b)
		require.NoError(t, err)
		m.MinimumUpdatePeriod = dash.Duration(100 * time.Millisecond)
		if edit != nil {
			edit(&m, n)
		}
		mu.Lock()
		if step+1 < len(ends) {
			step++
			clk.Set(ends[step])
		}
		mu.Unlock()
		return m.Encode()
	})
	base := serveLive(t, p, func(h http.Handler) http.Handler {
		for _, w := range extra {
			h = w(h)
		}
		return mpd(h)
	})
	return base, media.ToDuration(total)
}

func TestReadDASHPolling(t *testing.T) {
	base, total := liveMPD(t, nil)
	c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", ReadOptions{Duration: total - 100*time.Millisecond})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	assert.NotZero(t, countKind(c.Frames, media.KindVideo))
}

func TestReadASTChanges(t *testing.T) {
	base, total := liveMPD(t, func(m *dash.MPD, n int) {
		if n >= 2 {
			ast := m.AvailabilityStartTime.Add(time.Second)
			m.AvailabilityStartTime = &ast
		}
	})
	c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", ReadOptions{Duration: total - 100*time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, []string{RuleASTStable}, rules(c.Violations))
}

func TestReadHLSPolling(t *testing.T) {
	clk := &fakeNow{t: farFuture()}
	p := packClip(t, "h264_aac.ts", 100, clk.Now)
	snap := p.Snapshot()
	ep := snap.Epochs[0]
	clk.Set(ep.WallAt(snap.Segments[0].End))
	var mu sync.Mutex
	step := 0
	base := serveLive(t, p, rewrite("hls/index.m3u8", func(b []byte, _ int) []byte {
		pl, err := hls.ParseMedia(b)
		require.NoError(t, err)
		pl.TargetDuration = 0 // poll at the minimum interval
		mu.Lock()
		if step+1 < len(snap.Segments) {
			step++
			clk.Set(ep.WallAt(snap.Segments[step].End))
		}
		mu.Unlock()
		return pl.Encode()
	}))
	var total int64
	for _, seg := range snap.Segments {
		total += seg.Duration
	}
	c, err := ReadHLS(context.Background(), base+"hls/index.m3u8", ReadOptions{Duration: media.ToDuration(total) - 100*time.Millisecond})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	once, err := ReadHLS(context.Background(), base+"hls/index.m3u8", once())
	require.NoError(t, err)
	assert.Len(t, c.Frames, len(once.Frames))
}

func TestReadSegmentMissing(t *testing.T) {
	p := packClip(t, "h264_aac.ts", 100, farFuture)
	t.Run("hls-ts", func(t *testing.T) {
		base := serveLive(t, p, notFound("hls/1.ts"))
		c, err := ReadHLS(context.Background(), base+"hls/index.m3u8", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleSegmentAvailable}, rules(c.Violations))
		require.Len(t, c.Violations, 1)
	})
	t.Run("dash", func(t *testing.T) {
		base := serveLive(t, p, notFound("cmaf/0-1.m4s"))
		c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleSegmentAvailable}, rules(c.Violations))
	})
	t.Run("dash-first-audio", func(t *testing.T) {
		// The capture starts at the period's first segment (tfdt equals
		// presentationTimeOffset), so the start rules apply: audio now starts
		// a segment late.
		base := serveLive(t, p, notFound("cmaf/1-0.m4s"))
		c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleSegmentAvailable, RuleStartOffset}, rules(c.Violations))
	})
	t.Run("init", func(t *testing.T) {
		base := serveLive(t, p, notFound("cmaf/init-1-1.mp4"))
		c, err := ReadHLS(context.Background(), base+"cmaf/master.m3u8", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleSegmentAvailable}, rules(c.Violations))
		assert.Len(t, c.Violations, 1, "a missing init is fetched and reported once")
		c, err = ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
		require.NoError(t, err)
		assert.Equal(t, []string{RuleSegmentAvailable}, rules(c.Violations))
	})
}

func TestReadTimelineAndPeriodOrder(t *testing.T) {
	p := packClip(t, "h264_aac_then_640.ts", 100, farFuture)
	base := serveLive(t, p, rewrite("cmaf/index.mpd", func(b []byte, _ int) []byte {
		m, err := dash.Parse(b)
		require.NoError(t, err)
		require.Len(t, m.Periods, 2)
		m.Periods[1].Start = m.Periods[0].Start + dash.Duration(time.Second)
		s := &m.Periods[0].AdaptationSets[0].SegmentTemplate.Timeline.S[1]
		s.D++
		return m.Encode()
	}))
	c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RulePeriodOrder, RuleTimeline}, rules(c.Violations))
}

func TestReadDroppedNoConfig(t *testing.T) {
	p := packClip(t, "h264_aac.ts", 100, farFuture)
	snap := p.Snapshot()
	// Turn every SPS of segment 1 into an unknown NAL type in place, so its
	// demuxer never has a video config and drops all its video frames.
	base := serveLive(t, p, rewrite("hls/1.ts", func(b []byte, _ int) []byte {
		require.Equal(t, snap.Segments[1].TS, b)
		out := bytes.ReplaceAll(b, []byte{0, 0, 0, 1, 0x67}, []byte{0, 0, 0, 1, 0x7f})
		require.NotEqual(t, b, out)
		return out
	}))
	c, err := ReadHLS(context.Background(), base+"hls/index.m3u8", once())
	require.NoError(t, err)
	// Without video, segment 1's anchor is its audio, which does not span
	// the video EXTINF: extinf-matches follows from the dropped frames.
	assert.Equal(t, []string{RuleExtinf, RuleSegmentKey}, rules(c.Violations))
}

func TestReadErrors(t *testing.T) {
	ctx := context.Background()
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(fail.Close)
	_, err := ReadHLS(ctx, fail.URL+"/x.m3u8", once())
	assert.ErrorContains(t, err, "status 500")
	_, err = ReadDASH(ctx, fail.URL+"/x.mpd", once())
	assert.ErrorContains(t, err, "status 500")
	_, err = ReadTS(ctx, fail.URL+"/x.ts", ReadOptions{})
	assert.ErrorContains(t, err, "status 500")

	keys := keyframes(t)
	good := fmp4Playlist(t, []int{0, keys[1]}, nil)
	trunc := func(b []byte) []byte { return b[:len(b)-10] }
	srv := files(t, map[string][]byte{
		"/bad.m3u8":         []byte("not a playlist"),
		"/badmaster.m3u8":   []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n"),
		"/emptymaster.m3u8": []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nnone.m3u8\n"),
		"/bad.mpd":          []byte("<MPD"),
		"/trunc.m3u8":       []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2,\n0.m4s\n"),
		"/truncinit.m3u8":   []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"tinit.mp4\"\n#EXTINF:2,\n0.m4s\n"),
		"/badts.m3u8":       []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:2,\n0.ts\n"),
		"/init.mp4":         good["/init.mp4"],
		"/tinit.mp4":        trunc(good["/init.mp4"]),
		"/0.m4s":            trunc(good["/0.m4s"]),
		"/0.ts":             []byte("garbage"),
		"/notimeline.mpd": dash.MPD{Periods: []dash.Period{{AdaptationSets: []dash.AdaptationSet{{
			Representations: []dash.Representation{{ID: "0"}},
		}}}}}.Encode(),
		"/huge.mpd": dash.MPD{Periods: []dash.Period{{AdaptationSets: []dash.AdaptationSet{{
			SegmentTemplate: &dash.SegmentTemplate{Timeline: &dash.SegmentTimeline{S: []dash.S{{D: 1, R: 60000}, {D: 1, R: 60000}}}},
			Representations: []dash.Representation{{ID: "0"}},
		}}}}}.Encode(),
		"/ts.ts": []byte("garbage"),
	})
	for _, u := range []string{"/bad.m3u8", "/badmaster.m3u8", "/trunc.m3u8", "/truncinit.m3u8", "/badts.m3u8"} {
		_, err := ReadHLS(ctx, srv+u, once())
		assert.Error(t, err, u)
	}
	_, err = ReadHLS(ctx, srv+"/emptymaster.m3u8", once())
	assert.ErrorContains(t, err, "status 404")
	for _, u := range []string{"/bad.mpd", "/notimeline.mpd", "/huge.mpd"} {
		_, err := ReadDASH(ctx, srv+u, once())
		assert.Error(t, err, u)
	}
	_, err = ReadTS(ctx, srv+"/ts.ts", ReadOptions{})
	assert.Error(t, err)
	_, err = ReadHLS(ctx, "http://[::1]:namedport/x", once())
	assert.Error(t, err)
	_, err = ReadTS(ctx, "http://[::1]:namedport/x", ReadOptions{})
	assert.Error(t, err)
	_, err = ReadHLS(ctx, srv+"\x7f", once())
	assert.Error(t, err)
	_, err = ReadTS(ctx, srv+"\x7f", ReadOptions{})
	assert.Error(t, err)
}

func TestReadNilClientUsesDefault(t *testing.T) {
	keys := keyframes(t)
	srv := files(t, fmp4Playlist(t, []int{0, keys[1]}, nil))
	c, err := ReadHLS(context.Background(), srv+"/v.m3u8", ReadOptions{Once: true, Client: nil})
	require.NoError(t, err)
	assert.NotEmpty(t, c.Frames)
	c, err = ReadHLS(context.Background(), srv+"/v.m3u8", ReadOptions{Once: true, Client: &http.Client{}})
	require.NoError(t, err)
	assert.NotEmpty(t, c.Frames)
}

func TestReadPollingStopsOnCancel(t *testing.T) {
	keys := keyframes(t)
	m := fmp4Playlist(t, []int{0, keys[1]}, nil)
	pl, err := hls.ParseMedia(m["/v.m3u8"])
	require.NoError(t, err)
	pl.Ended, pl.TargetDuration = false, 1
	m["/v.m3u8"] = pl.Encode()
	mpd := dash.MPD{Type: "dynamic", MinimumUpdatePeriod: dash.Duration(time.Second)}
	m["/x.mpd"] = mpd.Encode()
	srv := files(t, m)

	for _, read := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := ReadHLS(ctx, srv+"/v.m3u8", ReadOptions{Duration: time.Hour})
			return err
		},
		func(ctx context.Context) error {
			_, err := ReadDASH(ctx, srv+"/x.mpd", ReadOptions{Duration: time.Hour})
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		start := time.Now()
		err := read(ctx)
		cancel()
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(start), 2*time.Second)
	}
}

func TestReadTSDuration(t *testing.T) {
	h := hub.New(hub.Options{})
	srv := httptest.NewServer(httpts.Handler(func(string) (*hub.Hub, bool) { return h, true }, 1<<16))
	t.Cleanup(srv.Close)
	ch := make(chan Capture, 1)
	go func() {
		c, err := ReadTS(context.Background(), srv.URL+"/live/s/stream.ts", ReadOptions{Duration: time.Second})
		assert.NoError(t, err)
		ch <- c
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)
	runClip(t, h, "h264_aac.ts")
	c := <-ch
	h.Close()
	require.NotEmpty(t, c.Frames)
	span := c.Frames[len(c.Frames)-1].DTS - c.Frames[0].DTS
	assert.GreaterOrEqual(t, span, int64(media.ClockRate))
	assert.Less(t, span, int64(2*media.ClockRate))
}

func TestReadTSCancel(t *testing.T) {
	h := hub.New(hub.Options{})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(httpts.Handler(func(string) (*hub.Hub, bool) { return h, true }, 1<<16))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := ReadTS(ctx, srv.URL+"/live/s/stream.ts", ReadOptions{})
		ch <- err
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)
	for _, f := range timedClip(t) {
		h.Write(f)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-ch:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("ReadTS did not stop on cancel")
	}
}

func TestExpand(t *testing.T) {
	rep := dash.Representation{ID: "v1", Bandwidth: 5000}
	expandOK := func(tpl string, number, tm uint64) string {
		s, err := expand(tpl, rep, number, tm)
		require.NoError(t, err)
		return s
	}
	assert.Equal(t, "v1-0042-77-5000$.m4s", expandOK("$RepresentationID$-$Number%04d$-$Time$-$Bandwidth$$$.m4s", 42, 77))
	assert.Equal(t, "a$Other$b", expandOK("a$Other$b", 1, 1))
	assert.Equal(t, "open$tail", expandOK("open$tail", 1, 1))
	assert.Equal(t, strings.Repeat("0", 31)+"7", expandOK("$Number%032d$", 7, 0))
	assert.Equal(t, "123456", expandOK("$Number%02d$", 123456, 0))
	for _, bad := range []string{"$Number%033d$", "$Time%0200000d$", "$Number%0xd$", "$Bandwidth%d$"} {
		start := time.Now()
		_, err := expand(bad, rep, 1, 1)
		assert.Error(t, err, bad)
		assert.Less(t, time.Since(start), 100*time.Millisecond)
	}
	assert.EqualValues(t, 48000, rescale(90000, 90000, 48000))
	assert.EqualValues(t, 7, rescale(7, 1000, 1000))
}

func TestReadDASHBadTemplateWidth(t *testing.T) {
	m := dash.MPD{Periods: []dash.Period{{AdaptationSets: []dash.AdaptationSet{{
		SegmentTemplate: &dash.SegmentTemplate{
			Initialization: "init-$Number%0200000d$.mp4", Media: "$Number%0200000d$.m4s",
			Timeline: &dash.SegmentTimeline{S: []dash.S{{D: 1}}},
		},
		Representations: []dash.Representation{{ID: "0"}},
	}}}}}
	srv := files(t, map[string][]byte{"/x.mpd": m.Encode()})
	_, err := ReadDASH(context.Background(), srv+"/x.mpd", once())
	assert.ErrorContains(t, err, "width")
}

// timelineMPD returns an MPD with one adaptation set per timeline, each with
// its own init URL (not served).
func timelineMPD(timelines ...[]dash.S) []byte {
	sets := make([]dash.AdaptationSet, 0, len(timelines))
	for i, s := range timelines {
		sets = append(sets, dash.AdaptationSet{
			SegmentTemplate: &dash.SegmentTemplate{
				Initialization: "init-" + strconv.Itoa(i) + ".mp4", Media: strconv.Itoa(i) + "-$Number$.m4s",
				Timeline: &dash.SegmentTimeline{S: s},
			},
			Representations: []dash.Representation{{ID: strconv.Itoa(i)}},
		})
	}
	return dash.MPD{Periods: []dash.Period{{AdaptationSets: sets}}}.Encode()
}

func TestReadDASHSegmentCap(t *testing.T) {
	const maxR = int64(^uint64(0) >> 1)
	srv := files(t, map[string][]byte{
		// r = MaxInt64 must not overflow the count.
		"/overflow.mpd": timelineMPD([]dash.S{{D: 1, R: maxR}, {D: 1, R: maxR}, {D: 1, R: maxR}, {D: 1, R: maxR}}),
		// Each representation is under the cap; together they are not.
		"/many.mpd": timelineMPD(
			[]dash.S{{D: 1, R: 40000}}, []dash.S{{D: 1, R: 40000}}, []dash.S{{D: 1, R: 40000}},
		),
		"/under.mpd": timelineMPD([]dash.S{{D: 1, R: 40000}}, []dash.S{{D: 1, R: 40000}}),
	})
	for _, u := range []string{"/overflow.mpd", "/many.mpd"} {
		start := time.Now()
		_, err := ReadDASH(context.Background(), srv+u, once())
		assert.ErrorContains(t, err, "segments", u)
		assert.Less(t, time.Since(start), 2*time.Second, u)
	}
	c, err := ReadDASH(context.Background(), srv+"/under.mpd", once())
	require.NoError(t, err)
	assert.Equal(t, []string{RuleSegmentAvailable}, rules(c.Violations), "inits are not served")
}

// TestReadDASHCaptureCap: the cap also bounds the distinct segments of a
// whole capture, across representations and refreshes, even when each MPD
// lists few segments.
func TestReadDASHCaptureCap(t *testing.T) {
	old := maxSegments
	t.Cleanup(func() { maxSegments = old })
	p := packClip(t, "h264_aac.ts", 100, farFuture)
	base := serveLive(t, p, nil)
	maxSegments = 5 // the MPD lists 3 video + 3 audio segments
	_, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
	assert.ErrorContains(t, err, "MPD timelines above 5 segments")
	// The capture total counts 6 segments, 2 inits and 2 representations.
	maxSegments = 9
	_, err = ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
	assert.ErrorContains(t, err, "capture above 9")
	maxSegments = 10
	_, err = ReadDASH(context.Background(), base+"cmaf/index.mpd", once())
	require.NoError(t, err)

	// Each refresh lists only the newest segment per representation (2 per
	// MPD), but the capture fetches 6 distinct segments in all.
	for _, tc := range []struct {
		max  int
		want string
	}{{9, "capture above 9"}, {10, ""}} {
		sliding, total := liveMPD(t, func(m *dash.MPD, _ int) {
			for i := range m.Periods[0].AdaptationSets {
				st := m.Periods[0].AdaptationSets[i].SegmentTemplate
				n := uint64(len(st.Timeline.S) - 1)
				st.Timeline.S = st.Timeline.S[n:]
				first := *st.StartNumber + n
				st.StartNumber = &first
			}
		})
		maxSegments = tc.max
		c, err := ReadDASH(context.Background(), sliding+"cmaf/index.mpd", ReadOptions{Duration: total - 100*time.Millisecond})
		if tc.want != "" {
			assert.ErrorContains(t, err, tc.want)
			continue
		}
		require.NoError(t, err)
		assert.NotZero(t, countKind(c.Frames, media.KindVideo))
	}
}

// TestReadHLSPollingAcrossConfigChange: the master's CODECS grows when the
// window reaches the new config; polling re-reads the master.
func TestReadHLSPollingAcrossConfigChange(t *testing.T) {
	for _, window := range []int{3, 100} {
		t.Run(strconv.Itoa(window), func(t *testing.T) {
			clk := &fakeNow{t: farFuture()}
			p := packClip(t, "h264_aac_then_640.ts", window, clk.Now)
			snap := p.Snapshot()
			require.Len(t, snap.Epochs, 2)
			at := func(i int) time.Time {
				e, _ := snap.Epoch(snap.Segments[i].Epoch)
				return e.WallAt(snap.Segments[i].End)
			}
			clk.Set(at(0))
			var mu sync.Mutex
			step := 0
			var masters []string
			fast := func(b []byte, _ int) []byte {
				pl, err := hls.ParseMedia(b)
				require.NoError(t, err)
				pl.TargetDuration = 0 // poll at the minimum interval
				return pl.Encode()
			}
			mw := rewrite("cmaf/master.m3u8", func(b []byte, _ int) []byte {
				mu.Lock()
				defer mu.Unlock()
				masters = append(masters, string(b))
				if step+1 < len(snap.Segments) {
					step++
					clk.Set(at(step))
				}
				return b
			})
			base := serveLive(t, p, func(h http.Handler) http.Handler {
				return mw(rewrite("cmaf/v0.m3u8", fast)(rewrite("cmaf/a1.m3u8", fast)(h)))
			})
			var total int64
			for _, seg := range snap.Segments {
				total += seg.Duration
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := ReadHLS(ctx, base+"cmaf/master.m3u8",
				ReadOptions{Duration: media.ToDuration(total) - 100*time.Millisecond})
			require.NoError(t, err)
			assert.Empty(t, c.Violations)
			mu.Lock()
			defer mu.Unlock()
			require.GreaterOrEqual(t, len(masters), 2)
			assert.NotEqual(t, masters[0], masters[len(masters)-1], "CODECS changed during the capture")
		})
	}
}

// TestReadTSTracksByPID: a TS segment without the video PID keeps the audio
// on the same capture track.
func TestReadTSTracksByPID(t *testing.T) {
	frames := timedClip(t)
	var vcfg, acfg *media.TrackConfig
	for _, f := range frames {
		if f.Track == 0 && vcfg == nil {
			vcfg = f.Config
		}
		if f.Track == 1 && acfg == nil {
			acfg = f.Config
		}
	}
	half := frames[len(frames)/2].DTS
	mux := func(tracks []*media.TrackConfig, keep func(media.Frame) bool) []byte {
		var buf bytes.Buffer
		m, err := ts.NewMuxer(&buf, tracks)
		require.NoError(t, err)
		require.NoError(t, m.WriteTables())
		for _, f := range frames {
			if keep(f) {
				require.NoError(t, m.WriteFrame(f))
			}
		}
		return buf.Bytes()
	}
	seg0 := mux([]*media.TrackConfig{vcfg, acfg}, func(f media.Frame) bool { return f.DTS < half })
	seg1 := mux([]*media.TrackConfig{nil, acfg}, func(f media.Frame) bool { return f.DTS >= half && f.Track == 1 })
	pl := hls.MediaPlaylist{Version: 3, TargetDuration: 4, Ended: true, Segments: []hls.Segment{
		{URI: "0.ts", Duration: time.Second}, {URI: "1.ts", Duration: time.Second},
	}}
	srv := files(t, map[string][]byte{"/i.m3u8": pl.Encode(), "/0.ts": seg0, "/1.ts": seg1})
	c, err := ReadHLS(context.Background(), srv+"/i.m3u8", once())
	require.NoError(t, err)
	tracks := map[media.Kind]map[int]bool{media.KindVideo: {}, media.KindAudio: {}}
	var lastAudio int64
	for _, f := range c.Frames {
		tracks[f.Kind()][f.Track] = true
		if f.Kind() == media.KindAudio {
			lastAudio = f.DTS
		}
	}
	assert.Len(t, tracks[media.KindVideo], 1)
	assert.Len(t, tracks[media.KindAudio], 1, "audio keeps one capture track")
	assert.NotEqual(t, tracks[media.KindVideo], tracks[media.KindAudio])
	assert.Greater(t, lastAudio, int64(0))
	for _, v := range c.Violations {
		assert.NotEqual(t, RuleDTS, v.Rule, v.String())
	}
}

func TestSplitNALUs(t *testing.T) {
	got, err := splitNALUs([]byte{0, 1, 0xaa, 0, 0, 0, 2, 0xbb, 0xcc}, 2)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{{0xaa}, {0xbb, 0xcc}}, got)
	_, err = splitNALUs([]byte{0, 9, 1}, 2)
	assert.Error(t, err)
	_, err = splitNALUs([]byte{0}, 2)
	assert.Error(t, err)
}

func TestParseInitErrors(t *testing.T) {
	_, err := parseInit([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'})
	assert.ErrorContains(t, err, "no moov")
	_, err = parseInit([]byte{0, 0, 0, 9, 'm', 'o', 'o'})
	assert.Error(t, err)
	_, err = parseSegment([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}, fmp4Init{cfg: &media.TrackConfig{Codec: media.CodecAAC}, timescale: 48000})
	assert.ErrorContains(t, err, "no samples")
}

// TestReadRenditionStartsMidEpoch: the video playlist starts at its epoch's
// first segment but the audio playlist does not, so the first epoch's start
// rules are skipped.
func TestReadRenditionStartsMidEpoch(t *testing.T) {
	p := packClip(t, "h264_aac.ts", 100, farFuture)
	base := serveLive(t, p, rewrite("cmaf/a1.m3u8", func(b []byte, _ int) []byte {
		pl, err := hls.ParseMedia(b)
		require.NoError(t, err)
		pl.Segments = pl.Segments[1:]
		pl.MediaSequence++
		return pl.Encode()
	}))
	c, err := ReadHLS(context.Background(), base+"cmaf/master.m3u8", once())
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
}

// TestReadDASHInitsCounted: representations with empty timelines and their
// own (missing) inits still count toward the capture cap.
func TestReadDASHInitsCounted(t *testing.T) {
	old := maxSegments
	t.Cleanup(func() { maxSegments = old })
	maxSegments = 10
	timelines := make([][]dash.S, 20)
	srv := files(t, map[string][]byte{"/x.mpd": timelineMPD(timelines...)})
	_, err := ReadDASH(context.Background(), srv+"/x.mpd", once())
	assert.ErrorContains(t, err, "capture above 10")
}

func TestReadDASHTemplateTooLong(t *testing.T) {
	long := strings.Repeat("a", 2049)
	for _, tpl := range []dash.SegmentTemplate{
		{Initialization: long, Media: "$Number$.m4s"},
		{Initialization: "init.mp4", Media: long},
	} {
		tpl.Timeline = &dash.SegmentTimeline{S: []dash.S{{D: 1}}}
		m := dash.MPD{Periods: []dash.Period{{AdaptationSets: []dash.AdaptationSet{{
			SegmentTemplate: &tpl, Representations: []dash.Representation{{ID: "0"}},
		}}}}}
		srv := files(t, map[string][]byte{"/x.mpd": m.Encode()})
		_, err := ReadDASH(context.Background(), srv+"/x.mpd", once())
		assert.ErrorContains(t, err, "template")
	}
}

// TestReadDASHInit404Cached: a missing init is fetched and reported once,
// not on every refresh.
func TestReadDASHInit404Cached(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "cmaf/init-1-1.mp4") {
				mu.Lock()
				gets++
				mu.Unlock()
			}
			next.ServeHTTP(w, r)
		})
	}
	base, total := liveMPD(t, nil, notFound("cmaf/init-1-1.mp4"), count)
	c, err := ReadDASH(context.Background(), base+"cmaf/index.mpd", ReadOptions{Duration: total - 100*time.Millisecond})
	require.NoError(t, err)
	assert.Equal(t, []string{RuleSegmentAvailable}, rules(c.Violations))
	assert.Len(t, c.Violations, 1)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, gets)
}

// masterServer serves a hand-built fMP4 capture under two media playlists,
// a.m3u8 (ended) and b.m3u8 (live, never ending), and a master whose answer
// to its n-th request (from 1) master chooses.
func masterServer(t *testing.T, master func(n int, both, onlyA []byte) (int, []byte)) (string, *int) {
	t.Helper()
	keys := keyframes(t)
	m := fmp4Playlist(t, []int{0, keys[1]}, nil)
	pl, err := hls.ParseMedia(m["/v.m3u8"])
	require.NoError(t, err)
	pl.TargetDuration = 1
	m["/a.m3u8"] = pl.Encode()
	pl.Ended = false
	m["/b.m3u8"] = pl.Encode()
	in, err := parseInit(m["/init.mp4"])
	require.NoError(t, err)
	codec, err := media.CodecString(in.cfg)
	require.NoError(t, err)
	variant := func(uri string) hls.Variant { return hls.Variant{URI: uri, Codecs: codec, Bandwidth: 1} }
	both := hls.MultivariantPlaylist{Version: 7, Variants: []hls.Variant{variant("a.m3u8"), variant("b.m3u8")}}.Encode()
	onlyA := hls.MultivariantPlaylist{Version: 7, Variants: []hls.Variant{variant("a.m3u8")}}.Encode()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			mu.Lock()
			n++
			code, body := master(n, both, onlyA)
			mu.Unlock()
			w.WriteHeader(code)
			_, _ = w.Write(body)
			return
		}
		b, ok := m[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/master.m3u8", &n
}

// TestReadMasterDropsRendition: once the master stops listing the live
// playlist and the rest have ended, polling stops.
func TestReadMasterDropsRendition(t *testing.T) {
	u, _ := masterServer(t, func(n int, both, onlyA []byte) (int, []byte) {
		if n == 1 {
			return http.StatusOK, both
		}
		return http.StatusOK, onlyA
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := ReadHLS(ctx, u, ReadOptions{Duration: time.Hour})
	require.NoError(t, err)
	assert.NotEmpty(t, c.Frames)
	assert.Empty(t, c.Violations)
}

// TestReadFinalMasterFails: a failing last master read keeps the capture,
// and Once reads the master only once.
func TestReadFinalMasterFails(t *testing.T) {
	u, n := masterServer(t, func(n int, _, onlyA []byte) (int, []byte) {
		if n == 1 {
			return http.StatusOK, onlyA
		}
		return http.StatusInternalServerError, nil
	})
	c, err := ReadHLS(context.Background(), u, ReadOptions{Duration: time.Hour})
	require.NoError(t, err)
	assert.NotEmpty(t, c.Frames)
	assert.Empty(t, c.Violations)
	assert.Equal(t, 2, *n, "the final master read was tried")

	u, n = masterServer(t, func(_ int, _, onlyA []byte) (int, []byte) { return http.StatusOK, onlyA })
	c, err = ReadHLS(context.Background(), u, once())
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	assert.Equal(t, 1, *n, "Once reads the master once")

	// A master that turns unparsable is ignored the same way.
	u, _ = masterServer(t, func(n int, _, onlyA []byte) (int, []byte) {
		if n == 1 {
			return http.StatusOK, onlyA
		}
		return http.StatusOK, []byte("garbage")
	})
	_, err = ReadHLS(context.Background(), u, ReadOptions{Duration: time.Hour})
	require.NoError(t, err)
}
