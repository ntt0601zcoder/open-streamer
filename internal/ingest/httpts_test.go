package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

func httpFixturePath() string {
	return filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts")
}

func httpFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(httpFixturePath())
	require.NoError(t, err)
	return b
}

// fixtureTrackCounts demuxes data and counts frames per track.
func fixtureTrackCounts(t *testing.T, data []byte) map[int]int {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	m := map[int]int{}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return m
		}
		require.NoError(t, err)
		m[f.Track]++
	}
}

// readUntilErr reads r until Read fails and returns the frames and the error.
func readUntilErr(r ingest.Reader) ([]ingest.Frame, error) {
	var out []ingest.Frame
	for {
		f, err := r.Read()
		switch {
		case errors.Is(err, ingest.ErrDiscontinuity):
		case err != nil:
			return out, err
		default:
			out = append(out, f)
		}
	}
}

func TestHTTPTSPull(t *testing.T) {
	data := httpFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		fl := w.(http.Flusher)
		const chunk = 7 * 188
		for off := 0; off < len(data); off += chunk {
			_, _ = w.Write(data[off:min(off+chunk, len(data))])
			fl.Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/live.ts", ingest.Env{})
	require.NoError(t, err)
	assert.Equal(t, ingest.Spec{Live: true}, r.Spec())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	frames, err := readUntilErr(r)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NoError(t, r.Close())
	got := map[int]int{}
	for _, f := range frames {
		got[f.Track]++
		assert.False(t, f.Arrival.IsZero())
	}
	assert.Equal(t, fixtureTrackCounts(t, data), got)
	assert.Contains(t, r.Stats(), "cc_errors")
}

func TestHTTPTSPullFromOurOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	lookup := func(name string) (*hub.Hub, bool) {
		if name != "s" {
			return nil, false
		}
		h := hub.New(hub.Options{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer h.Close()
			for len(h.Stats()) == 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Millisecond):
				}
			}
			err := pipeline.New(h, pipeline.Options{
				Inputs:   []string{"file://" + mustAbs(t, httpFixturePath()) + "?pace=0"},
				EndOnEOF: true,
			}).Run(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("feed: %v", err)
			}
		}()
		return h, true
	}
	srv := httptest.NewServer(httpts.Handler(lookup, 0))
	defer srv.Close()

	r, err := ingest.Open(context.Background(), srv.URL+"/live/s/stream.ts", ingest.Env{})
	require.NoError(t, err)
	frames, err := readUntilErr(r)
	require.Error(t, err)
	require.NoError(t, r.Close())
	require.NotEmpty(t, frames)

	c := clock.New(clock.Options{Kinds: r.Kinds()})
	out := make([]media.Frame, 0, len(frames))
	for _, f := range frames {
		out = append(out, c.PushAt(f.Frame, f.Arrival)...)
	}
	out = append(out, c.Flush()...)
	require.NotEmpty(t, out)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	require.NoError(t, err)
	return a
}

func TestHTTPTSStatus(t *testing.T) {
	data := httpFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "new.ts", http.StatusFound)
	})
	mux.HandleFunc("/new.ts", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := ingest.Open(context.Background(), srv.URL+"/missing?token=hunter2", ingest.Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
	assert.NotContains(t, err.Error(), "hunter2")

	r, err := ingest.Open(context.Background(), srv.URL+"/old", ingest.Env{})
	require.NoError(t, err)
	frames, _ := readUntilErr(r)
	require.NoError(t, r.Close())
	assert.NotEmpty(t, frames)
}

func TestHTTPTSHLSNotAvailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/type", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
		_, _ = w.Write([]byte("whatever"))
	})
	mux.HandleFunc("/body", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, p := range []string{"/type", "/body", "/index.m3u8"} {
		_, err := ingest.Open(context.Background(), srv.URL+p, ingest.Env{})
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "HLS input not available", p)
	}
}

func TestHTTPTSCloseUnblocks(t *testing.T) {
	data := httpFixture(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Enough for the PMT and some frames, then a stall.
		_, _ = w.Write(data[:20*188])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	r, err := ingest.Open(ctx, srv.URL+"/live.ts", ingest.Env{})
	require.NoError(t, err)
	cancel() // the open ctx ending does not end the reader
	time.Sleep(100 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := readUntilErr(r)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Read ended before Close: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, r.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestHTTPTSOpenCtxAborts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ingest.Open(ctx, srv.URL+"/live.ts", ingest.Env{})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}
