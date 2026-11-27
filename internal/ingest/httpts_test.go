package ingest_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
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

	_, err := ingest.Open(context.Background(), srv.URL+"/missing?token="+vHunter, ingest.Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
	assert.NotContains(t, err.Error(), vHunter)

	r, err := ingest.Open(context.Background(), srv.URL+"/old", ingest.Env{})
	require.NoError(t, err)
	frames, _ := readUntilErr(r)
	require.NoError(t, r.Close())
	assert.NotEmpty(t, frames)
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

func TestHTTPTSEnvHTTPUsed(t *testing.T) {
	data := httpFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "yes", r.Header.Get("X-Test"))
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	var used atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used.Add(1)
		r.Header.Set("X-Test", "yes")
		return http.DefaultTransport.RoundTrip(r)
	})}
	r, err := ingest.Open(context.Background(), srv.URL+"/x.ts", ingest.Env{HTTP: client})
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.EqualValues(t, 1, used.Load())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPTSUserinfoRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	u := "http://alice:" + vS3 + "@" + strings.TrimPrefix(srv.URL, "http://") + "/x.ts"
	_, err := ingest.Open(context.Background(), u, ingest.Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), vS3)
	// A request that cannot even be built must not leak the raw URL either.
	_, err = ingest.Open(context.Background(), "http://alice:"+vS3+"@127.0.0.1:1/\x7f", ingest.Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), vS3)
}

// A failing source retried in a loop must not accumulate connections.
// Not parallel: it counts goroutines.
func TestHTTPTSFailedOpensLeakNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	open := func() {
		_, err := ingest.Open(context.Background(), srv.URL+"/x.ts", ingest.Env{})
		require.Error(t, err)
	}
	open() // warm up lazily started goroutines
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 30 {
		open()
	}
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		2*time.Second, 20*time.Millisecond, "goroutines before=%d now=%d", before, runtime.NumGoroutine())
}

// NewHTTPClient trusts the roots it is given, and the system's when given
// none: an HTTP-TS body is read from a server whose certificate a test CA
// issued only with that CA as the roots.
func TestNewHTTPClient(t *testing.T) {
	ca := tlstest.NewCA(t)
	data := httpFixture(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(data)
	}))
	srv.TLS = ca.Server(t)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes
	srv.StartTLS()
	defer srv.Close()

	client := ingest.NewHTTPClient(ca.Pool())
	tr, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.True(t, tr.DisableKeepAlives)
	assert.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
	assert.Equal(t, 10*time.Second, tr.ResponseHeaderTimeout)
	assert.Zero(t, client.Timeout, "the body of a live pull never ends")
	assert.NotSame(t, client, ingest.NewHTTPClient(ca.Pool()), "each call returns its own client")

	r, err := ingest.Open(context.Background(), srv.URL+"/live.ts", ingest.Env{HTTP: client})
	require.NoError(t, err)
	frames, err := readUntilErr(r)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NoError(t, r.Close())
	got := map[int]int{}
	for _, f := range frames {
		got[f.Track]++
	}
	assert.Equal(t, fixtureTrackCounts(t, data), got)

	// The system roots do not hold the test CA: with them, and with no
	// Env.HTTP at all, the pull is refused with the x509 reason.
	for name, env := range map[string]ingest.Env{
		"NewHTTPClient(nil)": {HTTP: ingest.NewHTTPClient(nil)},
		"no Env.HTTP":        {},
		"another CA":         {HTTP: ingest.NewHTTPClient(tlstest.NewCA(t).Pool())},
	} {
		_, err := ingest.Open(context.Background(), srv.URL+"/live.ts?token="+vSecret, env)
		require.Error(t, err, name)
		var ua x509.UnknownAuthorityError
		assert.True(t, errors.As(err, &ua), "%s: %v", name, err)
		assert.NotContains(t, err.Error(), vSecret, name)
	}
}
