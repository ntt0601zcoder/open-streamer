package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
)

// TestMain runs the fake transcoder child when the test binary is started as
// one: the tests pass it as -ostranscoder.
func TestMain(m *testing.M) {
	if os.Getenv("OS_TRANSCODE_FAKE") == "1" {
		transcodetest.Main()
	}
	os.Exit(m.Run())
}

func TestParseRungEncoder(t *testing.T) {
	_, _, v, err := parseRung("cam=a:360:800:nvenc")
	require.NoError(t, err)
	assert.Equal(t, "nvenc", v.Encoder)
	assert.Equal(t, 360, v.Height)
	_, _, v, err = parseRung("cam=a:360:800:x264")
	require.NoError(t, err)
	assert.Equal(t, "x264", v.Encoder)
	_, _, v, err = parseRung("cam=a:360:800")
	require.NoError(t, err)
	assert.Empty(t, v.Encoder)
	for _, bad := range []string{"cam=a:360:800:qsv", "cam=a:360:800:", "cam=a:360:800:nvenc:x", "cam=a:copy:nvenc"} {
		_, _, _, err = parseRung(bad)
		assert.Error(t, err, bad)
	}
}

func writePNG(t *testing.T, dir, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 8, 8))))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o600))
	return b.Bytes()
}

func TestParseWatermark(t *testing.T) {
	stream, w, err := parseWatermark("cam=pos=br,opacity=80,size=6,tz=Asia/Ho_Chi_Minh,text=%H:%M:%S, live", "")
	require.NoError(t, err)
	assert.Equal(t, "cam", stream)
	assert.Equal(t, picture.Watermark{
		Text: "%H:%M:%S, live", Position: picture.BottomRight, OpacityPct: 80, SizePct: 6, TimeZone: "Asia/Ho_Chi_Minh",
	}, w)

	_, w, err = parseWatermark("cam=text=a,pos=br", "")
	require.NoError(t, err)
	assert.Equal(t, "a,pos=br", w.Text)
	assert.Equal(t, picture.TopLeft, w.Position)

	for in, want := range map[string]picture.Position{
		"tl": picture.TopLeft, "tr": picture.TopRight, "bl": picture.BottomLeft, "br": picture.BottomRight, "c": picture.Center,
	} {
		_, w, err = parseWatermark("cam=pos="+in+",text=x", "")
		require.NoError(t, err, in)
		assert.Equal(t, want, w.Position, in)
	}

	dir := t.TempDir()
	png := writePNG(t, dir, "logo.png")
	_, _, err = parseWatermark("cam=image=logo.png", "")
	require.Error(t, err, "an image needs -assets")
	assert.Contains(t, err.Error(), "-assets")
	_, w, err = parseWatermark("cam=image=logo.png,pos=c", dir)
	require.NoError(t, err)
	assert.Equal(t, png, w.Image)
	assert.Equal(t, picture.Center, w.Position)
	_, _, err = parseWatermark("cam=image=../logo.png", dir)
	assert.Error(t, err)
	_, _, err = parseWatermark("cam=image=missing.png", dir)
	assert.Error(t, err)

	for _, bad := range []string{
		"cam", "cam=", "cam=pos=br", "cam=bogus=1,text=x", "cam=pos=middle,text=x", "cam=opacity=x,text=x",
		"cam=size=x,text=x", "cam=pos=br,pos=tl,text=x", "cam=noequals,text=x", "=text=x",
		"cam=opacity=101,text=x", "cam=tz=Nowhere/Land,text=x", "cam=text=%q",
	} {
		_, _, err = parseWatermark(bad, dir)
		assert.Error(t, err, bad)
	}
	_, _, err = parseWatermark("cam=image=logo.png,text=x", dir)
	assert.Error(t, err, "both text and image")
}

func TestParseThumbnail(t *testing.T) {
	for in, want := range map[string]picture.Thumbnail{
		"cam=10":        {IntervalSec: 10},
		"cam=5:240:70":  {IntervalSec: 5, Height: 240, Quality: 70},
		"cam=3:240":     {IntervalSec: 3, Height: 240},
		"cam=3600:2:1":  {IntervalSec: 3600, Height: 2, Quality: 1},
		"cam=7:0:0":     {IntervalSec: 7},
		"cam=7:720:100": {IntervalSec: 7, Height: 720, Quality: 100},
	} {
		stream, th, err := parseThumbnail(in)
		require.NoError(t, err, in)
		assert.Equal(t, "cam", stream)
		assert.Equal(t, want, th, in)
	}
	for _, bad := range []string{
		"cam=0", "cam=x", "cam", "cam=", "=5", "cam=-1", "cam=5:x", "cam=5:240:x", "cam=5:3", "cam=5:240:101",
		"cam=3601", "cam=5:240:70:1", "cam=5:721",
	} {
		_, _, err := parseThumbnail(bad)
		assert.Error(t, err, bad)
	}
}

func TestThumbnailRepeatRefused(t *testing.T) {
	var errw strings.Builder
	cfg, code := parseArgs([]string{
		"-stream", "cam=" + fixture, "-ostranscoder", "/bin/true", "-thumbnail", "cam=5", "-thumbnail", "cam=6",
	}, &errw)
	assert.Nil(t, cfg)
	assert.Equal(t, 2, code)
	assert.Contains(t, errw.String(), "twice")
}

func TestPictureFlagErrors(t *testing.T) {
	base := []string{"-stream", "a=" + fixture, "-ostranscoder", "/bin/true"}
	for name, extra := range map[string][]string{
		"watermark no ladder":    {"-watermark", "a=text=x"},
		"watermark unknown":      {"-rung", "a=x:240:600", "-watermark", "b=text=x"},
		"watermark twice":        {"-rung", "a=x:240:600", "-watermark", "a=text=x", "-watermark", "a=text=y"},
		"watermark on rendition": {"-rung", "a=x:240:600", "-watermark", "a_x=text=x"},
		"thumbnail unknown":      {"-thumbnail", "b=5"},
		"thumbnail on rendition": {"-rung", "a=x:240:600", "-thumbnail", "a_x=5"},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(append(slicesClone(base), extra...), &errw)
			assert.Nil(t, cfg)
			assert.Equal(t, 2, code)
			assert.NotEmpty(t, errw.String())
		})
	}
	var errw strings.Builder
	cfg, code := parseArgs([]string{"-stream", "a=" + fixture, "-thumbnail", "a=5"}, &errw)
	assert.Nil(t, cfg)
	assert.Equal(t, 2, code)
	assert.Contains(t, errw.String(), "-ostranscoder")
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func TestThumbnailChildChoice(t *testing.T) {
	const bin = "/bin/true"
	for name, c := range map[string]struct {
		args        []string
		wantNailer  bool
		wantInChild bool
	}{
		"no ladder":      {[]string{"-thumbnail", "v=5"}, true, false},
		"copies all":     {[]string{"-rung", "v=src:copy", "-audio", "v=copy", "-thumbnail", "v=5"}, true, false},
		"copy, aac":      {[]string{"-rung", "v=src:copy", "-thumbnail", "v=5"}, false, true},
		"encoded rung":   {[]string{"-rung", "v=a:120:150", "-audio", "v=copy", "-thumbnail", "v=5"}, false, true},
		"encoded + aac":  {[]string{"-rung", "v=a:120:150", "-thumbnail", "v=5:240:70"}, false, true},
		"no -thumbnail":  {[]string{"-rung", "v=a:120:150"}, false, false},
		"other stream":   {[]string{"-stream", "w=" + fixture, "-rung", "v=a:120:150", "-thumbnail", "w=5"}, true, false},
		"encoded nvenc*": {[]string{"-rung", "v=a:120:150:x264", "-thumbnail", "v=5"}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			var errw strings.Builder
			cfg, code := parseArgs(append([]string{"-stream", "v=" + fixture, "-ostranscoder", bin}, c.args...), &errw)
			require.NotNil(t, cfg, "%d %s", code, errw.String())
			nailers := 0
			for _, ts := range cfg.thumbs {
				if ts.nailer != nil {
					nailers++
				}
			}
			assert.Equal(t, c.wantNailer, nailers == 1, "a Thumbnailer")
			inChild := false
			for _, x := range cfg.transcoders {
				inChild = inChild || x.thumbnail != nil
			}
			assert.Equal(t, c.wantInChild, inChild, "child thumbnail")
		})
	}
	// A thumbnailer needs the transcoder binary even when the ladder copies.
	var errw strings.Builder
	cfg, _ := parseArgs([]string{"-stream", "v=" + fixture, "-rung", "v=src:copy", "-audio", "v=copy", "-thumbnail", "v=5"}, &errw)
	assert.Nil(t, cfg)
	assert.Contains(t, errw.String(), "-ostranscoder")
}

func TestWatermarkCopyRungRefused(t *testing.T) {
	var errw syncBuilder
	code := run(context.Background(), []string{
		"-listen", "127.0.0.1:0", "-stream", "v=" + fixture, "-ostranscoder", "/bin/true",
		"-rung", "v=src:copy", "-rung", "v=a:120:150", "-watermark", "v=text=x",
	}, io.Discard, &errw)
	assert.NotZero(t, code)
	assert.Contains(t, errw.String(), "v_src")
}

func TestWatermarkAccepted(t *testing.T) {
	cfg, code := parseArgs([]string{
		"-stream", "v=" + fixture, "-ostranscoder", "/bin/true", "-rung", "v=a:120:150:nvenc",
		"-watermark", "v=pos=br,text=%H:%M",
	}, io.Discard)
	require.NotNil(t, cfg, code)
	require.Len(t, cfg.transcoders, 1)
}

func TestThumbnailHTTP(t *testing.T) {
	t.Setenv("OS_TRANSCODE_FAKE", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv(transcodetest.EnvShots, "1")
	exe, err := os.Executable()
	require.NoError(t, err)
	var errw syncBuilder
	addr, stop := startOsdev(t, []string{
		"-stream", "v=" + fixture, "-stream", "w=" + fixture, "-loop", "-ostranscoder", exe, "-thumbnail", "v=1",
	}, &errw)
	type reply struct {
		code int
		hdr  http.Header
		body []byte
	}
	get := func(path string, hdr ...string) reply {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, http.NoBody)
		require.NoError(t, err)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return reply{resp.StatusCode, resp.Header, body}
	}
	var first reply
	require.Eventually(t, func() bool {
		first = get("/live/v/thumbnail.jpg")
		return first.code == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond, errw.String())
	assert.Equal(t, "image/jpeg", first.hdr.Get("Content-Type"))
	assert.Equal(t, "no-cache", first.hdr.Get("Cache-Control"))
	assert.NotEmpty(t, first.hdr.Get("Last-Modified"))
	etag := first.hdr.Get("ETag")
	assert.Regexp(t, `^"\d+-\d+"$`, etag)
	_, err = picture.DecodeImage(first.body)
	assert.NoError(t, err, "a JPEG")

	r2 := get("/live/v/thumbnail.jpg", "If-None-Match", etag)
	assert.Equal(t, http.StatusNotModified, r2.code)
	assert.Equal(t, etag, r2.hdr.Get("ETag"))
	assert.Equal(t, http.StatusOK, get("/live/v/thumbnail.jpg", "If-None-Match", `"0-0"`).code)
	assert.Equal(t, http.StatusNotFound, get("/live/w/thumbnail.jpg").code)
	assert.Equal(t, http.StatusNotFound, get("/live/nope/thumbnail.jpg").code)
	assert.Equal(t, http.StatusNotFound, get("/live/v_abr/thumbnail.jpg").code)
	assert.Equal(t, 0, stop(), errw.String())
	assert.Contains(t, errw.String(), "thumbnailer")
}

func TestThumbnailBeforeFirstShot(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /live/{stream}/thumbnail.jpg", thumbHandler(map[string]*thumbSrc{
		"v": {stream: "v", shot: func() (picture.Shot, time.Time, bool) { return picture.Shot{}, time.Time{}, false }},
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live/v/thumbnail.jpg", http.NoBody))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A transcoder's child makes the thumbnails of a ladder that encodes, and the
// report line counts them.
func TestThumbnailFromChild(t *testing.T) {
	t.Setenv("OS_TRANSCODE_FAKE", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv(transcodetest.EnvShots, "1")
	t.Setenv(transcodetest.EnvStatsInterval, "50ms")
	exe, err := os.Executable()
	require.NoError(t, err)
	var errw syncBuilder
	addr, stop := startOsdev(t, []string{
		"-stream", "v=" + fixture, "-loop", "-ostranscoder", exe,
		"-rung", "v=a:120:150", "-audio", "v=copy", "-thumbnail", "v=1",
	}, &errw)
	require.Eventually(t, func() bool {
		resp, err := httpGet("http://" + addr + "/live/v/thumbnail.jpg")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond, errw.String())
	time.Sleep(300 * time.Millisecond) // a Stats with the count
	assert.Equal(t, 0, stop(), errw.String())
	assert.Regexp(t, `transcoder pid .*, thumbnails \d+`, errw.String())
	assert.NotContains(t, errw.String(), "thumbnailer spawns")
}
