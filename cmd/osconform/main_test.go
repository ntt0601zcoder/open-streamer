package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// serve packages the h264_aac fixture and serves it as stream "s". edit,
// when non-nil, rewrites the file whose path ends in suffix.
func serve(t *testing.T, suffix string, edit func([]byte) []byte) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("pkg", 1<<16)
	require.NoError(t, pipeline.Run(context.Background(), h, pipeline.Options{
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
		Now:  func() time.Time { return t0 },
	}))
	h.Close()
	p := live.NewPackager(h, live.Options{Window: 100, Now: func() time.Time { return t0.Add(time.Hour) }})
	p.Run(context.Background(), sub)
	next := live.Handler(func(n string) (*live.Packager, bool) { return p, n == "s" })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if edit == nil || !strings.HasSuffix(r.URL.Path, suffix) {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		w.Header().Set("Content-Type", rec.Header().Get("Content-Type"))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(edit(rec.Body.Bytes()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/live/s/"
}

func runConform(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var sb strings.Builder
	code := run(context.Background(), args, &sb)
	return code, sb.String()
}

func TestOsconformExitCodes(t *testing.T) {
	base := serve(t, "", nil)
	for _, p := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd"} {
		code, out := runConform(t, "-url", base+p, "-once")
		assert.Equal(t, 0, code, p)
		assert.Empty(t, out, p)
	}

	bad := serve(t, "cmaf/index.mpd", func(b []byte) []byte {
		m, err := dash.Parse(b)
		require.NoError(t, err)
		m.Periods[0].AdaptationSets[0].Representations[0].Codecs = "avc1.4d401e"
		return m.Encode()
	})
	code, out := runConform(t, "-url", bad+"cmaf/index.mpd", "-once")
	assert.Equal(t, 1, code)
	assert.Equal(t, 1, strings.Count(out, "\n"), out)

	bad = serve(t, "cmaf/master.m3u8", func(b []byte) []byte {
		m, err := hls.ParseMultivariant(b)
		require.NoError(t, err)
		m.Variants[0].Codecs = "avc1.4d401e,mp4a.40.2"
		return m.Encode()
	})
	code, _ = runConform(t, "-url", bad+"cmaf/master.m3u8", "-once")
	assert.Equal(t, 1, code)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "../../testdata/fixtures/h264_aac.ts")
	}))
	defer srv.Close()
	code, _ = runConform(t, "-url", srv.URL+"/x.ts")
	assert.Equal(t, 0, code)

	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	for _, ext := range []string{"x.m3u8", "x.mpd", "x.ts"} {
		code, _ = runConform(t, "-url", url+"/"+ext, "-once")
		assert.Equal(t, 2, code, ext)
	}
}

func TestOsconformBadArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"no url":      {},
		"bad flag":    {"-nope"},
		"extra arg":   {"-url", "http://x/a.ts", "y"},
		"bad suffix":  {"-url", "http://x/a.txt"},
		"bad url":     {"-url", "http://a b/a.ts"},
		"bad duratio": {"-url", "http://x/a.ts", "-duration", "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := runConform(t, args...)
			assert.Equal(t, 2, code)
			assert.Empty(t, out)
		})
	}
}
