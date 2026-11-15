package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

const (
	// ts33 is the modulus of MPEG-TS timestamps.
	ts33 = int64(1) << 33
	// muxOffset is what ts.Muxer adds to every stream time it writes.
	muxOffset = 126000
	// fixtureLoop is where the second copy of h264_aac.ts starts: past the
	// end of the first on both the video (3600) and audio (1920) grid.
	fixtureLoop = 547200
)

// tsRendition is h264_aac.ts twice in a row, cut into a video-only and an
// audio-only TS stream of equal segments, as a packager with a separate TS
// audio rendition writes them.
type tsRendition struct {
	vSegs, aSegs [][]byte
	// vSrc and aSrc are the frames of each segment, with the timestamps the
	// muxer was given: the raw 33-bit value is (DTS + muxOffset) mod 2^33.
	vSrc, aSrc [][]media.Frame
}

// tsRenditionFixture builds a tsRendition of gops GOPs (seconds) per
// segment. base is added to every timestamp, audioDelay to the audio's.
func tsRenditionFixture(t *testing.T, base, audioDelay int64, gops int) tsRendition {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(httpFixture(t)))
	require.NoError(t, err)
	var once, video, audio []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		once = append(once, f)
	}
	for c := range 2 {
		for _, f := range once {
			f.DTS += base + int64(c)*fixtureLoop
			f.PTS += base + int64(c)*fixtureLoop
			if f.Config.Codec.Kind() == media.KindVideo {
				video = append(video, f)
				continue
			}
			f.DTS += audioDelay
			f.PTS += audioDelay
			audio = append(audio, f)
		}
	}
	var starts []int64 // DTS where each segment starts: every gops-th keyframe
	keys := 0
	for _, f := range video {
		if f.Key {
			if keys%gops == 0 {
				starts = append(starts, f.DTS)
			}
			keys++
		}
	}
	seg := func(dts int64) int {
		k := 0
		for i, s := range starts {
			if dts >= s {
				k = i
			}
		}
		return k
	}
	var c tsRendition
	c.vSrc, c.aSrc = make([][]media.Frame, len(starts)), make([][]media.Frame, len(starts))
	for _, f := range video {
		c.vSrc[seg(f.DTS)] = append(c.vSrc[seg(f.DTS)], f)
	}
	for _, f := range audio {
		c.aSrc[seg(f.DTS)] = append(c.aSrc[seg(f.DTS)], f)
	}
	c.vSegs = muxSegments(t, c.vSrc)
	c.aSegs = muxSegments(t, c.aSrc)
	return c
}

// muxSegments writes each group of one-track frames as a TS segment that
// starts with tables; continuity counters run on across segments.
func muxSegments(t *testing.T, groups [][]media.Frame) [][]byte {
	t.Helper()
	m, err := ts.NewMuxer(io.Discard, []*media.TrackConfig{groups[0][0].Config})
	require.NoError(t, err)
	out := make([][]byte, len(groups))
	for k, g := range groups {
		var buf bytes.Buffer
		m.SetOutput(&buf)
		require.NoError(t, m.WriteTables())
		for _, f := range g {
			f.Track = 0
			require.NoError(t, m.WriteFrame(f))
		}
		out[k] = buf.Bytes()
	}
	return out
}

// tsPlaylist lists the segments "<k>.ts" for k in [from, n), each gops
// seconds long, and ends.
func tsPlaylist(from, n, gops int) []byte {
	pl := hls.MediaPlaylist{Version: 3, TargetDuration: gops, Ended: true}
	for k := from; k < n; k++ {
		pl.Segments = append(pl.Segments, hls.Segment{URI: fmt.Sprintf("%d.ts", k), Duration: time.Duration(gops) * time.Second})
	}
	return pl.Encode()
}

// serveTSRendition serves renditionMaster with the given video and audio
// media playlists over the fixture's segments; a nil segment is a 404.
func serveTSRendition(t *testing.T, c tsRendition, video, audio []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(renditionMaster()) })
	mux.HandleFunc("/video/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(video) })
	mux.HandleFunc("/audio/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(audio) })
	seg := func(segs [][]byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var k int
			_, err := fmt.Sscanf(r.PathValue("n"), "%d.ts", &k)
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

// requireSourceTimeline checks that every delivered frame carries exactly
// its source timestamp on the raw timeline, unwrapped past 2^33 (DTS +
// muxOffset): the two playlists were neither rebased nor shifted against
// each other. Frames come in source order per track, with gaps.
func requireSourceTimeline(t *testing.T, frames []ingest.Frame, c tsRendition) {
	t.Helper()
	src := [2][]media.Frame{}
	for k := range c.vSrc {
		src[0] = append(src[0], c.vSrc[k]...)
		src[1] = append(src[1], c.aSrc[k]...)
	}
	pos := [2]int{}
	for i, f := range frames {
		require.Less(t, f.Track, 2)
		s := src[f.Track]
		for pos[f.Track] < len(s) && s[pos[f.Track]].DTS+muxOffset != f.DTS {
			pos[f.Track]++
		}
		require.Less(t, pos[f.Track], len(s), "frame %d (track %d, DTS %d) is not on the source timeline", i, f.Track, f.DTS)
		assert.Equal(t, s[pos[f.Track]].PTS+muxOffset, f.PTS, "frame %d PTS", i)
		pos[f.Track]++
	}
}

func countFrames(groups [][]media.Frame) int {
	n := 0
	for _, g := range groups {
		n += len(g)
	}
	return n
}

// A TS audio rendition stays on the variant's timeline: audio that starts
// 40 ms after the video still does after the merge.
func TestHLSPullTSRenditionSync(t *testing.T) {
	c := tsRenditionFixture(t, 0, 3600, 1)
	n := len(c.vSegs)
	srv := serveTSRendition(t, c, tsPlaylist(0, n, 1), tsPlaylist(0, n, 1))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.Empty(t, res.breaks)
	requireDTSOrder(t, res)
	requireSourceTimeline(t, res.frames, c)
	assert.Equal(t, map[int]int{0: countFrames(c.vSrc), 1: countFrames(c.aSrc)}, trackCounts(res.frames))
	assert.Zero(t, r.Stats()["dropped_unaligned"])

	first := map[int]int64{}
	for _, f := range res.frames {
		if _, ok := first[f.Track]; !ok {
			first[f.Track] = f.DTS
		}
	}
	assert.Equal(t, c.aSrc[0][0].DTS-c.vSrc[0][0].DTS, first[1]-first[0], "the source A/V offset")
}

// TS playlists joined a segment (2 s) apart are aligned on their common
// timeline: the video's head that starts more than a second before the
// audio is dropped, and nothing else.
func TestHLSPullTSRenditionAlignment(t *testing.T) {
	c := tsRenditionFixture(t, 0, 0, 2)
	n := len(c.vSegs)
	srv := serveTSRendition(t, c, tsPlaylist(0, n, 2), tsPlaylist(1, n, 2))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.Empty(t, res.breaks)
	requireDTSOrder(t, res)
	requireSourceTimeline(t, res.frames, c)

	cut := c.aSrc[1][0].DTS - int64(media.ClockRate) // the alignment slack before the audio's first frame
	wantDropped := 0
	for _, f := range c.vSrc[0] {
		if f.DTS < cut {
			wantDropped++
		}
	}
	require.Positive(t, wantDropped)
	assert.EqualValues(t, wantDropped, r.Stats()["dropped_unaligned"], "the video before the slack, only")
	got := trackCounts(res.frames)
	assert.Equal(t, countFrames(c.vSrc)-wantDropped, got[0])
	assert.Equal(t, countFrames(c.aSrc[1:]), got[1])
}

// Across a wrap of the 33-bit timestamps, a break on the audio playlist
// alone (a lost segment after the wrap) restarts the audio on the video's
// unwrapped timeline, not 2^33 away from it.
func TestHLSPullTSRenditionWrap(t *testing.T) {
	// The raw timestamps wrap 5 s in, inside segment 2.
	c := tsRenditionFixture(t, ts33-muxOffset-5*int64(media.ClockRate), 3600, 2)
	n := len(c.vSegs)
	lost := 3
	require.Greater(t, c.aSrc[lost][0].DTS+muxOffset, ts33, "the lost segment is past the wrap")
	c.aSegs[lost] = nil
	srv := serveTSRendition(t, c, tsPlaylist(0, n, 2), tsPlaylist(0, n, 2))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/master.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1)
	requireDTSOrder(t, res)
	requireSourceTimeline(t, res.frames, c)

	st := r.Stats()
	assert.EqualValues(t, 1, st["skipped_segments"])
	got := trackCounts(res.frames)
	assert.Equal(t, countFrames(c.aSrc)-len(c.aSrc[lost]), got[1], "every audio frame but the lost segment's")
	assert.EqualValues(t, countFrames(c.vSrc), got[0]+int(st["dropped_unaligned"]), "video delivered or dropped to realign")
	assert.Less(t, int(st["dropped_unaligned"]), len(c.vSrc[lost]), "at most a segment of video dropped")
}
