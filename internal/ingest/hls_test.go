package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return b
}

// tsSegments remuxes h264_aac.ts into segments that each start with the
// tables and a video keyframe (one GOP each), as an HLS packager writes them.
func tsSegments(t *testing.T) [][]byte {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(httpFixture(t)))
	require.NoError(t, err)
	kinds := d.TrackKinds()
	var frames []media.Frame
	tracks := make([]*media.TrackConfig, len(kinds))
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if tracks[f.Track] == nil {
			tracks[f.Track] = f.Config
		}
		frames = append(frames, f)
	}
	m, err := ts.NewMuxer(io.Discard, tracks)
	require.NoError(t, err)
	var segs [][]byte
	var buf *bytes.Buffer
	for _, f := range frames {
		if buf == nil || (f.Key && kinds[f.Track] == media.KindVideo) {
			if buf != nil {
				segs = append(segs, buf.Bytes())
			}
			buf = &bytes.Buffer{}
			m.SetOutput(buf)
			require.NoError(t, m.WriteTables())
		}
		require.NoError(t, m.WriteFrame(f))
	}
	segs = append(segs, buf.Bytes())
	require.Greater(t, len(segs), 3)
	return segs
}

// splitPackets cuts data into n packet-aligned chunks of about equal size;
// only the first starts with tables.
func splitPackets(data []byte, n int) [][]byte {
	pkts := len(data) / 188
	out := make([][]byte, 0, n)
	for i := range n {
		out = append(out, data[pkts*i/n*188:pkts*(i+1)/n*188])
	}
	return out
}

// vodPlaylist lists uris (with a discontinuity before the indexes in disc)
// and ends.
func vodPlaylist(target int, uris []string, disc ...int) []byte {
	pl := hls.MediaPlaylist{Version: 3, TargetDuration: target, Ended: true}
	for i, u := range uris {
		pl.Segments = append(pl.Segments, hls.Segment{URI: u, Duration: time.Second, Discontinuity: slices.Contains(disc, i)})
	}
	return pl.Encode()
}

// pullResult is what a reader returned until its first error.
type pullResult struct {
	frames []ingest.Frame
	breaks []int // len(frames) at each ErrDiscontinuity
	err    error
}

func pullAll(r ingest.Reader) pullResult {
	var res pullResult
	for {
		f, err := r.Read()
		switch {
		case errors.Is(err, ingest.ErrDiscontinuity):
			res.breaks = append(res.breaks, len(res.frames))
		case err != nil:
			res.err = err
			return res
		default:
			res.frames = append(res.frames, f)
		}
	}
}

func trackCounts(frames []ingest.Frame) map[int]int {
	m := map[int]int{}
	for _, f := range frames {
		m[f.Track]++
	}
	return m
}

func TestHLSPullConfigChange(t *testing.T) {
	all := fixtureFile(t, "h264_aac_then_640.ts")
	cut := len(fixtureFile(t, "h264_aac.ts")) // the file is h264_aac.ts then h264_640.ts
	partA, partB := all[:cut], all[cut:]
	segs := append(splitPackets(partA, 3), splitPackets(partB, 2)...)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(vodPlaylist(3, []string{"s0.ts", "s1.ts", "s2.ts", "s3.ts", "s4.ts"}, 3))
	})
	for i, s := range segs {
		mux.HandleFunc(fmt.Sprintf("/s%d.ts", i), func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(s) })
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var calls atomic.Int64
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return t0.Add(time.Duration(calls.Add(1)) * time.Second) }
	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{Now: now})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())

	require.Len(t, res.breaks, 1, "one ErrDiscontinuity at the EXT-X-DISCONTINUITY")
	before, after := res.frames[:res.breaks[0]], res.frames[res.breaks[0]:]
	assert.Equal(t, fixtureTrackCounts(t, partA), trackCounts(before), "PES spanning segments are reassembled")
	assert.Equal(t, fixtureTrackCounts(t, partB), trackCounts(after))
	assert.Equal(t, []media.Kind{media.KindVideo}, r.Kinds(), "kinds of the current demuxer")

	videoConfigs := func(frames []ingest.Frame) map[*media.TrackConfig]bool {
		m := map[*media.TrackConfig]bool{}
		for _, f := range frames {
			if f.Track == 0 {
				m[f.Config] = true
			}
		}
		return m
	}
	ca, cb := videoConfigs(before), videoConfigs(after)
	require.Len(t, ca, 1, "one pointer across the three segments")
	require.Len(t, cb, 1)
	for a := range ca {
		for b := range cb {
			assert.NotSame(t, a, b, "a new resolution is a new Config")
			assert.Equal(t, 640, b.Width)
		}
	}

	// Arrival is when the segment completing the frame finished downloading:
	// one stamp per segment, in order.
	stamps := map[time.Time]bool{}
	for i, f := range res.frames {
		stamps[f.Arrival] = true
		if i > 0 {
			assert.False(t, f.Arrival.Before(res.frames[i-1].Arrival))
		}
	}
	assert.Len(t, stamps, len(segs))
	st := r.Stats()
	assert.EqualValues(t, len(segs), st["segments"])
	assert.Zero(t, st["fell_behind"])
	assert.Zero(t, st["skipped_segments"])
}

func TestHLSPullFFmpegVOD(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg")
	dir := t.TempDir()
	src, err := filepath.Abs(httpFixturePath())
	require.NoError(t, err)
	out, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-i", src,
		"-c", "copy", "-f", "hls", "-hls_time", "2", "-hls_list_size", "0", filepath.Join(dir, "index.m3u8")).CombinedOutput()
	require.NoError(t, err, string(out))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.Empty(t, res.breaks)
	assert.Equal(t, fixtureTrackCounts(t, httpFixture(t)), trackCounts(res.frames))
}

func TestHLSPullFellBehind(t *testing.T) {
	segs := tsSegments(t)
	require.GreaterOrEqual(t, len(segs), 6)
	var (
		mu     sync.Mutex
		phase  int
		served []int
	)
	playlist := func(first uint64, ended bool) []byte {
		pl := hls.MediaPlaylist{Version: 3, TargetDuration: 1, MediaSequence: first, Ended: ended}
		for i := range 6 {
			pl.Segments = append(pl.Segments, hls.Segment{URI: fmt.Sprintf("seg/%d.ts", first+uint64(i)), Duration: time.Second})
		}
		return pl.Encode()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		p := phase
		mu.Unlock()
		if p == 0 {
			_, _ = w.Write(playlist(0, false))
			return
		}
		// The window moved on past the reader: 6..9 were never fetched.
		_, _ = w.Write(playlist(10, true))
	})
	mux.HandleFunc("/seg/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, err := fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		require.NoError(t, err)
		mu.Lock()
		served = append(served, n)
		if n == 5 {
			phase = 1
		}
		mu.Unlock()
		_, _ = w.Write(segs[n%10])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{3, 4, 5, 13, 14, 15}, served, "three from the end, then three from the end again")
	require.Len(t, res.breaks, 1)
	assert.Positive(t, res.breaks[0])
	assert.Greater(t, len(res.frames), res.breaks[0])
	st := r.Stats()
	assert.EqualValues(t, 1, st["fell_behind"])
	assert.Positive(t, st["playlist_reloads"])
}

func TestHLSPullRedirectRelative(t *testing.T) {
	segs := tsSegments(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/old/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new/dir/index.m3u8?token=abc", http.StatusFound)
	})
	mux.HandleFunc("/new/dir/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(vodPlaylist(2, []string{"s0.ts", "sub/s1.ts", "../dir/s2.ts"}))
	})
	var got []string
	var mu sync.Mutex
	for i, p := range []string{"/new/dir/s0.ts", "/new/dir/sub/s1.ts", "/new/dir/s2.ts"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, r.URL.Path)
			mu.Unlock()
			_, _ = w.Write(segs[i])
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/old/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.NotEmpty(t, res.frames)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"/new/dir/s0.ts", "/new/dir/sub/s1.ts", "/new/dir/s2.ts"}, got)
}

// byteRangePlaylist lists segs as consecutive ranges of all.ts: the first
// with its offset, the others defaulting to the previous end.
func byteRangePlaylist(segs [][]byte) (text string, want []string) {
	pl := hls.MediaPlaylist{Version: 4, TargetDuration: 2, Ended: true}
	off := 0
	for _, s := range segs {
		want = append(want, fmt.Sprintf("bytes=%d-%d", off, off+len(s)-1))
		pl.Segments = append(pl.Segments, hls.Segment{URI: "all.ts", Duration: time.Second})
		off += len(s)
	}
	parts := strings.Split(string(pl.Encode()), "#EXTINF")
	for i := 1; i < len(parts); i++ {
		r := fmt.Sprintf("#EXT-X-BYTERANGE:%d", len(segs[i-1]))
		if i == 1 {
			r += "@0"
		}
		parts[i-1] += r + "\n"
	}
	return strings.Join(parts, "#EXTINF"), want
}

func TestHLSPullByteRange(t *testing.T) {
	segs := tsSegments(t)
	file := bytes.Join(segs, nil)
	serve := map[string]http.HandlerFunc{
		"partial": func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "all.ts", time.Time{}, bytes.NewReader(file))
		},
		// A server that ignores Range: the reader cuts the range out.
		"ignored": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(file) },
		// A 206 whose Content-Range is not the requested one is refused.
		"wrong range": func(w http.ResponseWriter, r *http.Request) {
			var from, to int
			_, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to)
			require.NoError(t, err)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from+188, to+188, len(file)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(file[from+188 : min(to+189, len(file))])
		},
	}
	for name, h := range serve {
		t.Run(name, func(t *testing.T) {
			list := segs
			if name == "wrong range" {
				list = segs[:2]
			}
			text, want := byteRangePlaylist(list)
			var mu sync.Mutex
			var ranges []string
			mux := http.NewServeMux()
			mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(text)) })
			mux.HandleFunc("/all.ts", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				ranges = append(ranges, r.Header.Get("Range"))
				mu.Unlock()
				h(w, r)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
			if name == "wrong range" {
				require.Error(t, err)
				mu.Lock()
				defer mu.Unlock()
				assert.Len(t, ranges, 4, "each segment tried twice, then skipped")
				return
			}
			require.NoError(t, err)
			res := pullAll(r)
			require.ErrorIs(t, res.err, io.EOF)
			require.NoError(t, r.Close())
			assert.Empty(t, res.breaks)
			assert.Equal(t, fixtureTrackCounts(t, file), trackCounts(res.frames))
			assert.Zero(t, r.Stats()["segment_errors"])
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, want, ranges)
		})
	}
}

func TestHLSPullEncryptedRefused(t *testing.T) {
	segs := tsSegments(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/aes.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"\n#EXTINF:1,\ns0.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/none.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:1,\ns0.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/s0.ts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(segs[0]) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := ingest.Open(context.Background(), srv.URL+"/aes.m3u8?token="+vHunter, ingest.Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encrypted HLS is not supported")
	assert.NotContains(t, err.Error(), vHunter)

	r, err := ingest.Open(context.Background(), srv.URL+"/none.m3u8", ingest.Env{})
	require.NoError(t, err, "METHOD=NONE is clear")
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.NotEmpty(t, res.frames)
}

func TestHLSPullSegmentRetry(t *testing.T) {
	segs := tsSegments(t)
	var mu sync.Mutex
	tries := map[int]int{}
	at := map[int][]time.Time{}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(vodPlaylist(2, []string{"0.ts", "1.ts", "2.ts", "3.ts"}))
	})
	mux.HandleFunc("/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, err := fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		require.NoError(t, err)
		mu.Lock()
		tries[n]++
		try := tries[n]
		at[n] = append(at[n], time.Now())
		mu.Unlock()
		switch {
		case n == 1: // fails for good: skipped
			http.Error(w, "boom", http.StatusInternalServerError)
		case n == 2 && try == 1: // fails once: retried
			http.Error(w, "boom", http.StatusServiceUnavailable)
		default:
			_, _ = w.Write(segs[n])
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1, "the skipped segment is a discontinuity")
	mu.Lock()
	assert.Equal(t, map[int]int{0: 1, 1: 2, 2: 2, 3: 1}, tries)
	for _, n := range []int{1, 2} {
		// The retry waits a quarter target (2 s), at most 500 ms.
		assert.GreaterOrEqual(t, at[n][1].Sub(at[n][0]), 450*time.Millisecond, "segment %d", n)
	}
	mu.Unlock()
	st := r.Stats()
	assert.EqualValues(t, 1, st["skipped_segments"])
	assert.EqualValues(t, 3, st["segment_errors"])
	assert.EqualValues(t, 3, st["segments"])
}

func TestOpenHLS(t *testing.T) {
	segs := tsSegments(t)
	pl := vodPlaylist(2, []string{"/seg/0.ts", "/seg/1.ts"})
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(pl)
	})
	mux.HandleFunc("/type", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
		_, _ = w.Write(pl)
	})
	mux.HandleFunc("/sniff", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pl)
	})
	mux.HandleFunc("/bom", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(append([]byte("\xef\xbb\xbf \r\n"), pl...))
	})
	mux.HandleFunc("/seg/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		_, _ = w.Write(segs[n])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, p := range []string{"/index.m3u8", "/type", "/sniff", "/bom"} {
		t.Run(p, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			r, err := ingest.Open(ctx, srv.URL+p, ingest.Env{})
			require.NoError(t, err)
			cancel() // the open ctx ending does not end the reader
			assert.Equal(t, ingest.Spec{Pace: true, Live: true, StallTimeout: 3 * time.Second, ArrivalGranularity: 2 * time.Second}, r.Spec())
			assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
			res := pullAll(r)
			require.ErrorIs(t, res.err, io.EOF)
			require.NoError(t, r.Close())
			assert.Equal(t, fixtureTrackCounts(t, bytes.Join(segs[:2], nil)), trackCounts(res.frames))
			assert.Contains(t, r.Stats(), "fell_behind")
		})
	}
}

func TestHLSPullCloseUnblocks(t *testing.T) {
	segs := tsSegments(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		// A live playlist that never grows.
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:1,\n0.ts\n"))
	})
	mux.HandleFunc("/0.ts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(segs[0]) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, r.Spec().StallTimeout)
	done := make(chan pullResult, 1)
	go func() { done <- pullAll(r) }()
	select {
	case res := <-done:
		t.Fatalf("Read ended before Close: %v", res.err)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, r.Close())
	select {
	case res := <-done:
		require.Error(t, res.err)
		assert.NotErrorIs(t, res.err, io.EOF)
		assert.NotEmpty(t, res.frames)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestHLSOpenCtxAborts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		// A live playlist with no segment yet: the open waits for one.
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ingest.Open(ctx, srv.URL+"/index.m3u8?token="+vHunter, ingest.Env{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, 1, strings.Count(err.Error(), "ingest:"), "one prefix: %v", err)
	assert.Equal(t, 1, strings.Count(err.Error(), "/index.m3u8"), "one URL: %v", err)
	assert.NotContains(t, err.Error(), vHunter)
}

func TestHLSTargetDurationClamped(t *testing.T) {
	segs := tsSegments(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:9223372036\n#EXTINF:1,\n0.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/0.ts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(segs[0]) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	assert.Equal(t, 90*time.Second, r.Spec().StallTimeout, "1.5 x the 60 s cap")
}

// segmentWindow is a playlist of 1 s target listing first..first+5 as
// "<seq>.ts", with a discontinuity sequence.
func segmentWindow(first, disc uint64, ended bool) []byte {
	pl := hls.MediaPlaylist{Version: 3, TargetDuration: 1, MediaSequence: first, DiscontinuitySequence: disc, Ended: ended}
	for i := range 6 {
		pl.Segments = append(pl.Segments, hls.Segment{URI: fmt.Sprintf("%d.ts", first+uint64(i)), Duration: time.Second})
	}
	return pl.Encode()
}

// windowServer serves the playlists in order (the last one repeats) and
// segment n as segs[n%len(segs)], recording the playlist request times and
// the segments served.
type windowServer struct {
	mu        sync.Mutex
	playlists [][]byte
	loads     []time.Time
	served    []int
}

func (ws *windowServer) handler(t *testing.T, segs [][]byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		ws.mu.Lock()
		i := min(len(ws.loads), len(ws.playlists)-1)
		ws.loads = append(ws.loads, time.Now())
		pl := ws.playlists[i]
		ws.mu.Unlock()
		_, _ = w.Write(pl)
	})
	mux.HandleFunc("/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, err := fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		require.NoError(t, err)
		ws.mu.Lock()
		ws.served = append(ws.served, n)
		ws.mu.Unlock()
		_, _ = w.Write(segs[n%len(segs)])
	})
	return mux
}

// A load-balanced edge can answer a reload with an older copy of the
// playlist: it is ignored, without replay or break, and the next reload
// comes after half a target.
func TestHLSPullStalePlaylist(t *testing.T) {
	segs := tsSegments(t)
	ws := &windowServer{playlists: [][]byte{
		segmentWindow(0, 0, false), // read 3, 4, 5
		segmentWindow(1, 0, false), // read 6
		segmentWindow(0, 0, false), // stale: ends at 5
		segmentWindow(2, 0, true),  // read 7, end
	}}
	srv := httptest.NewServer(ws.handler(t, segs))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	assert.Empty(t, res.breaks)
	st := r.Stats()
	assert.Zero(t, st["fell_behind"])
	assert.Zero(t, st["playlist_restarts"])
	assert.EqualValues(t, 1, st["stale_playlists"])

	ws.mu.Lock()
	defer ws.mu.Unlock()
	assert.Equal(t, []int{3, 4, 5, 6, 7}, ws.served, "nothing replayed")
	require.Len(t, ws.loads, 4)
	// A load with new segments waits a target (1 s); after the stale one,
	// half a target.
	for i, want := range []time.Duration{time.Second, time.Second, 500 * time.Millisecond} {
		gap := ws.loads[i+1].Sub(ws.loads[i])
		assert.GreaterOrEqual(t, gap, want-50*time.Millisecond, "reload %d", i+1)
		assert.Less(t, gap, want+400*time.Millisecond, "reload %d", i+1)
	}
}

// A playlist that restarts far below the reader's position is one break,
// then the reader rejoins three from the new end.
func TestHLSPullPlaylistRestart(t *testing.T) {
	segs := tsSegments(t)
	ws := &windowServer{playlists: [][]byte{
		segmentWindow(100, 0, false), // read 103, 104, 105
		segmentWindow(0, 0, true),    // restarted: read 3, 4, 5
	}}
	srv := httptest.NewServer(ws.handler(t, segs))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1)
	st := r.Stats()
	assert.EqualValues(t, 1, st["playlist_restarts"])
	assert.Zero(t, st["fell_behind"])
	assert.Zero(t, st["stale_playlists"])
	ws.mu.Lock()
	defer ws.mu.Unlock()
	assert.Equal(t, []int{103, 104, 105, 3, 4, 5}, ws.served)
}

// A restart that keeps the numbers close is told apart from a stale copy by
// a changed discontinuity sequence.
func TestHLSPullPlaylistRestartDiscSeq(t *testing.T) {
	segs := tsSegments(t)
	ws := &windowServer{playlists: [][]byte{
		segmentWindow(4, 3, false), // read 7, 8, 9
		segmentWindow(2, 0, true),  // ends at 7, discontinuity sequence reset: read 5, 6, 7
	}}
	srv := httptest.NewServer(ws.handler(t, segs))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	res := pullAll(r)
	require.ErrorIs(t, res.err, io.EOF)
	require.NoError(t, r.Close())
	require.Len(t, res.breaks, 1)
	assert.EqualValues(t, 1, r.Stats()["playlist_restarts"])
	ws.mu.Lock()
	defer ws.mu.Unlock()
	assert.Equal(t, []int{7, 8, 9, 5, 6, 7}, ws.served)
}

func TestHLSPullCloseAbortsSegment(t *testing.T) {
	segs := tsSegments(t)
	release := make(chan struct{})
	defer close(release)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1,\n0.ts\n#EXTINF:1,\n1.ts\n"))
	})
	mux.HandleFunc("/0.ts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(segs[0]) })
	var hung atomic.Bool
	mux.HandleFunc("/1.ts", func(w http.ResponseWriter, r *http.Request) {
		// Half the segment, then a stall.
		w.Header().Set("Content-Length", fmt.Sprint(len(segs[1])))
		_, _ = w.Write(segs[1][:len(segs[1])/2])
		w.(http.Flusher).Flush()
		hung.Store(true)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	done := make(chan pullResult, 1)
	go func() { done <- pullAll(r) }()
	require.Eventually(t, hung.Load, 2*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, r.Close())
	select {
	case res := <-done:
		require.Error(t, res.err)
		assert.NotErrorIs(t, res.err, io.EOF)
	case <-time.After(time.Second):
		t.Fatal("Close did not abort the segment download")
	}
	assert.Zero(t, r.Stats()["skipped_segments"], "a closed reader skips nothing")
}

// Readers opened and closed in a loop leave no connection behind.
// Not parallel: it counts goroutines.
func TestHLSOpenCloseLeaksNothing(t *testing.T) {
	segs := tsSegments(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(vodPlaylist(2, []string{"0.ts", "1.ts"}))
	})
	mux.HandleFunc("/aes.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:1,\n0.ts\n"))
	})
	mux.HandleFunc("/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		_, _ = w.Write(segs[n])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cycle := func() {
		r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{})
		require.NoError(t, err)
		_, err = r.Read()
		require.NoError(t, err)
		require.NoError(t, r.Close())
		_, err = ingest.Open(context.Background(), srv.URL+"/aes.m3u8", ingest.Env{})
		require.Error(t, err)
	}
	cycle() // warm up lazily started goroutines
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 20 {
		cycle()
	}
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		2*time.Second, 20*time.Millisecond, "goroutines before=%d now=%d", before, runtime.NumGoroutine())
}

func TestHLSPullFromOurOutput(t *testing.T) {
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

	// Wait for three segments, so the reader starts with a backlog.
	require.Eventually(t, func() bool { return p.Snapshot().Listed >= 3 }, 15*time.Second, 50*time.Millisecond)
	r, err := ingest.Open(context.Background(), srv.URL+"/live/s/hls/index.m3u8", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, ingest.Spec{Pace: true, Live: true, StallTimeout: 1500 * time.Millisecond, ArrivalGranularity: time.Second}, r.Spec())

	// Read past the loop point (6 s): the packager's discontinuity reaches us.
	c := clock.New(clock.Options{Kinds: r.Kinds()})
	var out []media.Frame
	breaks := 0
	deadline := time.Now().Add(20 * time.Second)
	var videoSeen int
	for videoSeen < 7*25 && time.Now().Before(deadline) {
		f, err := r.Read()
		if errors.Is(err, ingest.ErrDiscontinuity) {
			breaks++
			c.Break(false)
			continue
		}
		require.NoError(t, err)
		if r.Kinds()[f.Track] == media.KindVideo {
			videoSeen++
		}
		out = append(out, c.PushAt(f.Frame, f.Arrival)...)
	}
	require.NoError(t, r.Close())
	out = append(out, c.Flush()...)
	require.GreaterOrEqual(t, videoSeen, 7*25, "read seven seconds of video in time")
	assert.Positive(t, breaks, "the loop is an EXT-X-DISCONTINUITY")
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
	st := r.Stats()
	assert.Zero(t, st["fell_behind"])
	assert.Zero(t, st["skipped_segments"])
}

// The injected clock times the reloads as well as the arrivals: a clock an
// hour ahead of the system's neither stalls the reloads (an hour's wait) nor
// spins them.
func TestHLSPullInjectedClock(t *testing.T) {
	segs := tsSegments(t)
	var (
		mu      sync.Mutex
		start   = time.Now()
		reloads int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reloads++
		mu.Unlock()
		// One new 1 s segment per second, a window of three.
		n := int(time.Since(start)/time.Second) + 3
		pl := hls.MediaPlaylist{Version: 3, TargetDuration: 1, MediaSequence: uint64(n - 3)}
		for k := n - 3; k < n; k++ {
			pl.Segments = append(pl.Segments, hls.Segment{URI: fmt.Sprintf("seg/%d.ts", k), Duration: time.Second})
		}
		_, _ = w.Write(pl.Encode())
	})
	mux.HandleFunc("/seg/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.PathValue("n"), "%d.ts", &n)
		_, _ = w.Write(segs[n%len(segs)])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ahead := func() time.Time { return time.Now().Add(time.Hour) }
	r, err := ingest.Open(context.Background(), srv.URL+"/index.m3u8", ingest.Env{Now: ahead})
	require.NoError(t, err)
	defer r.Close()
	var last ingest.Frame
	deadline := time.Now().Add(10 * time.Second)
	for time.Since(start) < 4*time.Second {
		require.True(t, time.Now().Before(deadline), "reloads stalled")
		f, err := r.Read()
		if errors.Is(err, ingest.ErrDiscontinuity) {
			continue
		}
		require.NoError(t, err)
		last = f
	}
	assert.WithinDuration(t, ahead(), last.Arrival, 2*time.Second, "arrivals on the injected clock")
	mu.Lock()
	defer mu.Unlock()
	assert.Less(t, reloads, 15, "reloads paced, not spinning")
}
