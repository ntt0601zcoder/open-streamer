package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/asset"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
)

// fixtureFont is a font the store and the transcoder accept.
const fixtureFont = "../../testdata/fixtures/fonts/DejaVuSansMono.ttf"

// fontBytes returns the fixture font.
func fontBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(fixtureFont)
	require.NoError(t, err)
	return b
}

// pngOf is a valid PNG of side×side pixels.
func pngOf(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for i := range side {
		img.Set(i, i, color.NRGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

// testExe is the test binary: the transcoder of these tests.
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return exe
}

// fakeEnv is the TranscoderEnv that runs the test binary as the fake child
// and as a probe in the given mode, with vars added.
func fakeEnv(probe string, vars ...string) []string {
	// A race-built child sleeps 1 s at exit unless told otherwise.
	return append([]string{"OS_TRANSCODE_FAKE=1", "GORACE=atexit_sleep_ms=0", envFakeProbe + "=" + probe}, vars...)
}

// withAssets is a whole config with the given streams whose server section
// also has the transcoder bin ("" for none) and the assets directory dir
// ("" for none).
func withAssets(bin, dir string, streams ...string) string {
	extra := ""
	if bin != "" {
		extra += "  transcoder: {binary: " + strconv.Quote(bin) + "}\n"
	}
	if dir != "" {
		extra += "  assets_dir: " + strconv.Quote(dir) + "\n"
	}
	return strings.Replace(baseText(streams...), "server:\n", "server:\n"+extra, 1)
}

// camInput is the input of the tests' stream cam.
const camInput = "rtmp://src.example/live/cam"

// ladderYAML is a stream entry reading input, with a one-rung ladder and
// the watermark image logo.png.
func ladderYAML(name, input string) string {
	return "  - name: " + name + "\n    inputs:\n      - url: " + input + "\n" +
		"    transcode:\n      renditions:\n        - {label: \"720\", height: 720, bitrate_kbps: 1000}\n" +
		"      watermark: {image: logo.png}\n    outputs: {hls: true}\n"
}

// camSpec is the spec of ladderYAML("cam", camInput) with the watermark wm
// (nil for none).
func camSpec(wm *spec.Watermark) spec.Stream {
	return spec.Stream{
		Name:   "cam",
		Inputs: []spec.Input{{URL: camInput}},
		Transcode: &spec.Transcode{
			Renditions: []spec.Rendition{{Label: "720", Height: 720, BitrateKbps: 1000}},
			Watermark:  wm,
		},
		Outputs: spec.Outputs{HLS: true},
	}
}

// put stores b under name through the server.
func (s *testServer) put(name string, b []byte) (api.AssetPut, error) {
	s.t.Helper()
	res, _, err := s.PutAsset(bg, name, bytes.NewReader(b), "", false)
	return res, err
}

// lines returns the lines of the file at path; none while it is absent.
func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// dirNames returns the names in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// An upload reaches the running stream that names it, without a rebuild.
func TestAssetReachesRunningStream(t *testing.T) {
	dir := t.TempDir()
	logo, other := pngOf(t, 4), pngOf(t, 16)
	require.NotEqual(t, len(logo), len(other))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "logo.png"), logo, 0o600))
	wmLog := filepath.Join(t.TempDir(), "watermark.log")
	s := testConfig(t, func(su *setup) {
		su.Text = withAssets(testExe(t), dir, ladderYAML("cam", camInput))
		su.Opt.TranscoderEnv = fakeEnv("ok", transcodetest.EnvWatermarkLog+"="+wmLog)
	})
	gen := s.generation("cam")
	require.Eventually(t, func() bool {
		return slices.Contains(lines(t, wmLog), fmt.Sprintf("configure image:%d", len(logo)))
	}, waitFor, time.Millisecond, "the child's watermark log: %q", lines(t, wmLog))

	res, err := s.put("logo.png", other)
	require.NoError(t, err)
	assert.Equal(t, []stream.Refreshed{{Stream: "cam", OK: true}}, res.Refreshed)
	assert.Empty(t, res.Retried)
	assert.Equal(t, []string{"cam"}, res.UsedBy)
	assert.Equal(t, int64(len(other)), res.Size)
	require.Eventually(t, func() bool {
		return slices.Contains(lines(t, wmLog), fmt.Sprintf("set image:%d", len(other)))
	}, waitFor, time.Millisecond, "the child's watermark log: %q", lines(t, wmLog))
	assert.Equal(t, gen, s.generation("cam"), "the upload rebuilt the stream")

	// Over the API too.
	r := s.do(http.MethodPut, "/api/v1/assets/logo.png", "ops", string(logo), "Content-Type", "application/octet-stream")
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Contains(t, r.body, `"refreshed":[{"stream":"cam","ok":true}]`)
	assert.Contains(t, r.body, `"retried":[]`)
}

// An upload tries again the streams left stopped that name the asset.
func TestPutAssetRetriesStopped(t *testing.T) {
	cam := ladderYAML("cam", camInput)

	t.Run("the stream left stopped for the asset starts", func(t *testing.T) {
		dir := t.TempDir()
		s := testConfig(t, func(su *setup) {
			su.Text = withAssets(testExe(t), dir, cam)
			su.Opt.TranscoderEnv = fakeEnv("ok")
		})
		st, ok := s.Status("cam")
		require.True(t, ok)
		require.Equal(t, "stopped", st.State)
		require.Len(t, s.status().LoadErrors, 1)
		file := s.fileText()

		res, err := s.put("logo.png", pngOf(t, 4))
		require.NoError(t, err)
		assert.Equal(t, []stream.Outcome{{Name: "cam", Applied: "started"}}, res.Retried)
		assert.Empty(t, res.Refreshed)
		s.generation("cam")
		assert.Empty(t, s.status().LoadErrors)
		assert.Equal(t, file, s.fileText(), "the retry rewrote the config file")
	})

	t.Run("a stream still in the way stays stopped", func(t *testing.T) {
		dir := t.TempDir()
		udp := "udp://127.0.0.1:" + freeUDP(t)
		own := "  - name: own\n    inputs:\n      - url: " + udp + "\n    outputs: {hls: true}\n"
		dup := ladderYAML("dup", udp)
		s := testConfig(t, func(su *setup) {
			su.Text = withAssets(testExe(t), dir, own, cam, dup)
			su.Opt.TranscoderEnv = fakeEnv("ok")
		})
		s.generation("own")
		require.Len(t, s.status().LoadErrors, 2)
		file := s.fileText()

		res, err := s.put("logo.png", pngOf(t, 4))
		require.NoError(t, err)
		assert.Equal(t, []stream.Outcome{{Name: "cam", Applied: "started"}, {Name: "dup", Applied: "unchanged"}}, res.Retried)
		s.generation("cam")
		errs := s.status().LoadErrors
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0], `"own"`)
		assert.Equal(t, file, s.fileText())
	})

	t.Run("not while the config holds skipped entries", func(t *testing.T) {
		dir := t.TempDir()
		bad := "  - name: bad\n    inputs:\n      - url: rtmp://src.example/live/bad\n    colour: blue\n    outputs: {hls: true}\n"
		s := testConfig(t, func(su *setup) {
			su.Text = withAssets(testExe(t), dir, cam, bad)
			su.Opt.TranscoderEnv = fakeEnv("ok")
		})
		require.Len(t, s.status().LoadErrors, 2)
		loaded := s.status().LoadedAt
		const errorLine = `"level":"ERROR"`
		errs := strings.Count(s.logs.String(), errorLine)
		res, err := s.put("logo.png", pngOf(t, 4))
		require.NoError(t, err)
		assert.Empty(t, res.Retried)
		assert.Equal(t, errs, strings.Count(s.logs.String(), errorLine), "the upload logged an error")
		assert.Equal(t, loaded, s.status().LoadedAt, "the text in force was applied again")
		assert.FileExists(t, filepath.Join(dir, "logo.png"))
		st, _ := s.Status("cam")
		assert.Equal(t, "stopped", st.State)
	})

	t.Run("no stream left stopped names it: no apply", func(t *testing.T) {
		dir := t.TempDir()
		s := testConfig(t, func(su *setup) {
			su.Text = withAssets(testExe(t), dir, cam)
			su.Opt.TranscoderEnv = fakeEnv("ok")
		})
		loaded := s.status().LoadedAt
		res, err := s.put("other.png", pngOf(t, 4))
		require.NoError(t, err)
		assert.Empty(t, res.Retried)
		assert.Empty(t, res.Refreshed)
		assert.Equal(t, loaded, s.status().LoadedAt, "the text in force was applied again")
		st, _ := s.Status("cam")
		assert.Equal(t, "stopped", st.State)
	})
}

// A delete is refused while a stream names the asset, running or disabled.
func TestDeleteAssetInUse(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "logo.png"), pngOf(t, 4), 0o600))
	s := testConfig(t, func(su *setup) {
		su.Text = withAssets(testExe(t), dir, ladderYAML("cam", camInput))
		su.Opt.TranscoderEnv = fakeEnv("ok")
	})
	s.generation("cam")
	inUse := func() {
		t.Helper()
		err := s.DeleteAsset(bg, "logo.png", "")
		var e *api.AssetInUseError
		require.ErrorAs(t, err, &e)
		assert.Equal(t, []string{"cam"}, e.Streams)
	}
	inUse()
	r := s.do(http.MethodDelete, "/api/v1/assets/logo.png", "ops", "")
	assert.Equal(t, http.StatusConflict, r.code, r.body)
	assert.Contains(t, r.body, "asset_in_use")

	disabled := camSpec(&spec.Watermark{Image: "logo.png"})
	disabled.Disabled = true
	_, _, err := s.PutStream(bg, disabled, "")
	require.NoError(t, err)
	inUse()

	_, _, err = s.PutStream(bg, camSpec(nil), "")
	require.NoError(t, err)
	require.NoError(t, s.DeleteAsset(bg, "logo.png", ""))
	assert.NoFileExists(t, filepath.Join(dir, "logo.png"))
	require.ErrorIs(t, s.DeleteAsset(bg, "logo.png", ""), asset.ErrNotFound)

	// A stream that names the deleted asset is refused by Check.
	doc := `{"name":"cam","inputs":[{"url":"` + camInput + `"}],` +
		`"transcode":{"renditions":[{"label":"720","height":720,"bitrate_kbps":1000}],"audio":{},"watermark":{"image":"logo.png"}},` +
		`"outputs":{"hls":true}}`
	r = s.do(http.MethodPut, "/api/v1/streams/cam", "ops", doc)
	assert.Equal(t, http.StatusUnprocessableEntity, r.code, r.body)
	assert.Contains(t, r.body, "logo.png")
}

// probeLog is the API's log line of a font probe that did not run.
const probeLog = `"msg":"api: the font probe did not run"`

// apiProbeFails uploads font over the API and checks an ErrProbe row: a
// 503, logged once with the binary bin and the status, and nothing stored
// in dir.
func apiProbeFails(t *testing.T, s *testServer, dir, bin string, font []byte, row, status string) {
	t.Helper()
	before := strings.Count(s.logs.String(), probeLog)
	r := s.do(http.MethodPut, "/api/v1/assets/brand.ttf", "ops", string(font), "Content-Type", "application/octet-stream")
	assert.Equal(t, http.StatusServiceUnavailable, r.code, row+": "+r.body)
	assert.Contains(t, r.body, "probe_failed", row)
	logs := s.logs.String()
	assert.Equal(t, before+1, strings.Count(logs, probeLog), row)
	if i := strings.LastIndex(logs, probeLog); i >= 0 {
		last, _, _ := strings.Cut(logs[i:], "\n")
		assert.Contains(t, last, bin, row)
		assert.Contains(t, last, status, row)
	}
	assert.Empty(t, dirNames(t, dir), row)
}

// The font probe's verdicts: only exit 3 and a crash refuse a font;
// anything else says nothing about it.
func TestFontProbe(t *testing.T) {
	font := fontBytes(t)
	saved := fontProbeLimit
	fontProbeLimit = 2 * time.Second
	t.Cleanup(func() { fontProbeLimit = saved })
	exe := testExe(t)
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), "probe-mode")
	s := testConfig(t, func(su *setup) {
		su.Text = withAssets(exe, dir)
		su.Opt.TranscoderEnv = fakeEnv("file:" + script)
	})
	setMode := func(mode string) {
		t.Helper()
		require.NoError(t, os.WriteFile(script, []byte(mode), 0o600))
	}
	// Nothing is left in the directory: no asset, no temp file.
	empty := func(row string) {
		t.Helper()
		assert.Empty(t, dirNames(t, dir), row)
	}
	apiFails := func(row, status string) {
		t.Helper()
		apiProbeFails(t, s, dir, exe, font, row, status)
	}
	// probeFails checks an ErrProbe row: the error and nothing stored, then
	// the same over the API.
	probeFails := func(row, status string) {
		t.Helper()
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrProbe, row)
		assert.NotErrorIs(t, err, asset.ErrContent, row)
		empty(row)
		apiFails(row, status)
	}

	t.Run("exit 0 stores it", func(t *testing.T) {
		setMode("ok")
		res, err := s.put("brand.ttf", font)
		require.NoError(t, err)
		assert.Equal(t, "font", res.Kind)
		assert.Equal(t, []string{"brand.ttf"}, dirNames(t, dir))
		require.NoError(t, s.DeleteAsset(bg, "brand.ttf", ""))
	})
	t.Run("exit 3 refuses it with the stdout reason, never stderr", func(t *testing.T) {
		setMode("bad")
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrContent)
		assert.Contains(t, err.Error(), "bad tables")
		assert.NotContains(t, err.Error(), "/tmp")
		assert.NotContains(t, err.Error(), dir)
		assert.Equal(t, "bad tables", asset.Reason(err))
		empty("bad")
		r := s.do(http.MethodPut, "/api/v1/assets/brand.ttf", "ops", string(font), "Content-Type", "application/octet-stream")
		assert.Equal(t, http.StatusUnprocessableEntity, r.code, r.body)
		assert.Contains(t, r.body, "bad tables")
		assert.NotContains(t, r.body, "/tmp")
	})
	t.Run("exit 3 without a verdict line", func(t *testing.T) {
		setMode("empty")
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrContent)
		assert.Equal(t, "the transcoder could not load the font", asset.Reason(err))
		empty("empty")
	})
	t.Run("exit 3 with a long reason and more output", func(t *testing.T) {
		setMode("long")
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrContent)
		reason := asset.Reason(err)
		// 85 three-byte runes are 255 bytes: the cut at 256 falls inside
		// the 86th, which is dropped whole.
		assert.Equal(t, strings.Repeat("€", 85), reason)
		assert.True(t, utf8.ValidString(reason))
		empty("long")
	})
	t.Run("exit 3 with control bytes and invalid UTF-8 in the reason", func(t *testing.T) {
		setMode("dirty")
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrContent)
		assert.Equal(t, "bad tables", asset.Reason(err))
		empty("dirty")
	})
	t.Run("killed by SIGSEGV refuses it", func(t *testing.T) {
		if !canSegv {
			t.Skip("no signals here")
		}
		setMode("segv")
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrContent)
		assert.Equal(t, "the transcoder crashed on the font", asset.Reason(err))
		empty("segv")
	})
	t.Run("exit 2 from a stale bundle", func(t *testing.T) {
		setMode("usage")
		probeFails("usage", "exit status 2")
	})
	t.Run("exit 2 from the stub", func(t *testing.T) {
		setMode("stub")
		probeFails("stub", "exit status 2")
	})
	t.Run("exit 1 with a path on stderr", func(t *testing.T) {
		setMode("exit1")
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrProbe)
		assert.NotContains(t, err.Error(), "/tmp/secret", "stderr reached the error")
		probeFails("exit1", "exit status 1")
		r := s.do(http.MethodPut, "/api/v1/assets/brand.ttf", "ops", string(font), "Content-Type", "application/octet-stream")
		assert.Equal(t, http.StatusServiceUnavailable, r.code, r.body)
		assert.NotContains(t, r.body, "/tmp/secret", "stderr reached the client")
		assert.NotContains(t, r.body, dir, "the assets dir reached the client")
		assert.NotContains(t, s.logs.String(), "/tmp/secret", "stderr reached the log")
	})
	t.Run("past the limit", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		setMode("sleep:" + pidFile)
		start := time.Now()
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrProbe)
		assert.NotErrorIs(t, err, asset.ErrContent, "a context kill read as a crash")
		assert.Less(t, time.Since(start), fontProbeLimit+5*time.Second)
		b, err := os.ReadFile(pidFile)
		require.NoError(t, err)
		pid, err := strconv.Atoi(string(b))
		require.NoError(t, err)
		assert.True(t, processGone(pid), "the probe outlived its limit")
		empty("sleep")
		apiFails("sleep", "no verdict within")
	})
	// A client that goes away during the probe is not a probe that did not
	// run: its error is its ctx's, and the API does not log it.
	t.Run("the client goes away during the probe", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		setMode("sleep:" + pidFile)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		go func() {
			for {
				if _, err := os.Stat(pidFile); err == nil || ctx.Err() != nil {
					cancel()
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		start := time.Now()
		_, _, err := s.PutAsset(ctx, "brand.ttf", bytes.NewReader(font), "", false)
		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, asset.ErrProbe)
		assert.Less(t, time.Since(start), fontProbeLimit, "the probe ran to its limit")
		empty("hang-up")

		// Over the API: the request ends, and no probe failure is logged.
		require.NoError(t, os.Remove(pidFile))
		const reqLog = `"msg":"api: request"`
		probes, requests := strings.Count(s.logs.String(), probeLog), strings.Count(s.logs.String(), reqLog)
		hctx, hcancel := context.WithCancel(bg)
		defer hcancel()
		done := make(chan error, 1)
		go func() {
			req, err := http.NewRequestWithContext(hctx, http.MethodPut, "http://"+s.Addrs().API+"/api/v1/assets/brand.ttf", bytes.NewReader(font))
			if err != nil {
				done <- err
				return
			}
			req.SetBasicAuth("ops", editorPW)
			req.Header.Set("X-Requested-By", "test")
			req.Header.Set("Content-Type", "application/octet-stream")
			res, err := client.Do(req)
			if err == nil {
				_ = res.Body.Close()
			}
			done <- err
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(pidFile); return err == nil }, waitFor, 5*time.Millisecond)
		hcancel()
		require.Error(t, <-done)
		require.Eventually(t, func() bool { return strings.Count(s.logs.String(), reqLog) > requests },
			waitFor, 5*time.Millisecond, "the request did not end")
		assert.Equal(t, probes, strings.Count(s.logs.String(), probeLog), "a hang-up logged as a probe failure")
		empty("hang-up over the API")
	})

	t.Run("a binary that does not exist", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "ostranscoder")
		require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700))
		dir := t.TempDir()
		s := testConfig(t, func(su *setup) { su.Text = withAssets(bin, dir) })
		require.NoError(t, os.Remove(bin))
		_, err := s.put("brand.ttf", font)
		require.ErrorIs(t, err, asset.ErrProbe)
		assert.Contains(t, err.Error(), bin)
		assert.Empty(t, dirNames(t, dir))
		apiProbeFails(t, s, dir, bin, font, "no binary", "no such file")
	})
	t.Run("without a transcoder: no probe", func(t *testing.T) {
		dir := t.TempDir()
		s := testConfig(t, func(su *setup) { su.Text = withAssets("", dir) })
		_, err := s.put("brand.ttf", font)
		require.NoError(t, err)
		assert.Equal(t, []string{"brand.ttf"}, dirNames(t, dir))
	})
}

// A delete is refused while the config holds an entry the start skipped.
func TestDeleteAssetWhileSkipped(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "logo.png"), pngOf(t, 4), 0o600))
	bad := "  - name: bad\n    inputs:\n      - url: rtmp://src.example/live/bad\n    colour: blue\n    outputs: {hls: true}\n"
	s := testConfig(t, func(su *setup) { su.Text = withAssets("", dir, bad) })
	require.ErrorIs(t, s.DeleteAsset(bg, "logo.png", ""), api.ErrSkippedEntries)
	r := s.do(http.MethodDelete, "/api/v1/assets/logo.png", "ops", "")
	assert.Equal(t, http.StatusConflict, r.code, r.body)
	assert.Contains(t, r.body, "config_invalid_entries")

	r = s.do(http.MethodPut, "/api/v1/streams/bad", "ops", streamJSON("bad", "rtmp://src.example/live/bad", `{"hls":true}`))
	require.Equal(t, http.StatusCreated, r.code, r.body)
	require.NoError(t, s.DeleteAsset(bg, "logo.png", ""))
	assert.NoFileExists(t, filepath.Join(dir, "logo.png"))
}

// Asset returns an open file: a replacement does not change what it reads.
func TestAssetOpenIsAFile(t *testing.T) {
	dir := t.TempDir()
	font := fontBytes(t)
	big := append(slices.Clone(font), make([]byte, 2<<20-len(font))...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.ttf"), big, 0o600))
	s := testConfig(t, func(su *setup) { su.Text = withAssets("", dir) })

	doc, r, err := s.Asset("big.ttf")
	require.NoError(t, err)
	defer r.Close()
	assert.IsType(t, &os.File{}, r)
	assert.Equal(t, int64(len(big)), doc.Size)
	assert.Equal(t, "font", doc.Kind)
	assert.Empty(t, doc.UsedBy)
	_, err = s.put("big.ttf", font)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(big, got), "the open asset changed under its reader")
	require.NoError(t, r.Close())

	require.NoError(t, os.Symlink(filepath.Join(dir, "big.ttf"), filepath.Join(dir, "link.ttf")))
	_, _, err = s.Asset("link.ttf")
	require.ErrorIs(t, err, asset.ErrNotFound)
	list, err := s.Assets()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "big.ttf", list[0].Name)
}

// Without an assets directory every asset method is asset.ErrOff, and the
// scrape has no asset series.
func TestAssetsOff(t *testing.T) {
	s := testConfig(t, nil)
	_, err := s.Assets()
	require.ErrorIs(t, err, asset.ErrOff)
	_, _, err = s.Asset("logo.png")
	require.ErrorIs(t, err, asset.ErrOff)
	_, err = s.put("logo.png", pngOf(t, 4))
	require.ErrorIs(t, err, asset.ErrOff)
	require.ErrorIs(t, s.DeleteAsset(bg, "logo.png", ""), asset.ErrOff)
	r := s.do(http.MethodGet, "/metrics", "ops", "")
	require.Equal(t, http.StatusOK, r.code)
	assert.NotContains(t, r.body, "openstreamer_assets")
}

func TestAssetMetrics(t *testing.T) {
	dir := t.TempDir()
	logo, font := pngOf(t, 4), fontBytes(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "logo.png"), logo, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.ttf"), font, 0o600))
	s := testConfig(t, func(su *setup) { su.Text = withAssets("", dir) })
	assert.InDelta(t, 1, s.metric("openstreamer_assets", "kind", "image"), 0)
	assert.InDelta(t, 1, s.metric("openstreamer_assets", "kind", "font"), 0)
	assert.InDelta(t, len(logo), s.metric("openstreamer_assets_bytes", "kind", "image"), 0)
	assert.InDelta(t, len(font), s.metric("openstreamer_assets_bytes", "kind", "font"), 0)
	r := s.do(http.MethodGet, "/metrics", "ops", "")
	assert.Contains(t, r.body, "# HELP openstreamer_assets Assets in the assets directory, by kind.\n")
}

// Uploads, deletes and stream edits that use the assets, all at once: each
// answer is one the API defines, nothing deadlocks, and the end state is
// consistent.
func TestConcurrentAssetAndStreamWrites(t *testing.T) {
	client.CloseIdleConnections()
	before := runtime.NumGoroutine()
	dir := t.TempDir()
	imgA, imgB, font := pngOf(t, 4), pngOf(t, 8), fontBytes(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "logo.png"), imgA, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.ttf"), font, 0o600))
	s := testConfig(t, func(su *setup) {
		su.Text = withAssets(testExe(t), dir, ladderYAML("cam", camInput))
		su.Opt.TranscoderEnv = fakeEnv("ok")
	})
	s.generation("cam")
	variants := []spec.Stream{
		camSpec(&spec.Watermark{Image: "logo.png"}),
		camSpec(&spec.Watermark{Text: "brand", Font: "f.ttf"}),
		camSpec(nil),
	}

	deadline := time.Now().Add(2 * time.Second)
	running := func() bool { return time.Now().Before(deadline) }
	var (
		wg                   sync.WaitGroup
		edits, puts, deletes atomic.Int64
		mu                   sync.Mutex
		unexpected           []error
	)
	allowed := func(err error) bool {
		var inUse *api.AssetInUseError
		switch {
		case err == nil, errors.As(err, &inUse), errors.Is(err, asset.ErrNotFound):
			return true
		case errors.Is(err, stream.ErrInvalid):
			return strings.Contains(err.Error(), "logo.png") || strings.Contains(err.Error(), "f.ttf")
		}
		return false
	}
	check := func(err error) {
		if !allowed(err) {
			mu.Lock()
			unexpected = append(unexpected, err)
			mu.Unlock()
		}
	}
	for i := range 20 {
		wg.Go(func() {
			for n := 0; running(); n++ {
				switch i % 4 {
				case 0:
					_, _, err := s.PutStream(bg, variants[(i/4+n)%len(variants)], "")
					check(err)
					edits.Add(1)
				case 1:
					img := imgA
					if n%2 == 1 {
						img = imgB
					}
					_, _, err := s.PutAsset(bg, "logo.png", bytes.NewReader(img), "", false)
					check(err)
					puts.Add(1)
				case 2:
					name := "logo.png"
					if n%2 == 1 {
						name = "f.ttf"
					}
					check(s.DeleteAsset(bg, name, ""))
					deletes.Add(1)
				case 3:
					_, _, err := s.PutAsset(bg, "f.ttf", bytes.NewReader(font), "", false)
					check(err)
					puts.Add(1)
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "deadlock")
	}
	assert.Empty(t, unexpected)
	assert.Positive(t, edits.Load())
	assert.Positive(t, puts.Load())
	assert.Positive(t, deletes.Load())

	final, ok := s.Get("cam")
	require.True(t, ok)
	file := testParse(t, s.fileText()).Doc.Streams
	require.Len(t, file, 1)
	assert.Equal(t, spec.ChangeNone, spec.Diff(file[0], final), "the file is not the stream in force")
	assert.True(t, slices.ContainsFunc(variants, func(v spec.Stream) bool { return spec.Diff(v, final) == spec.ChangeNone }))
	for _, name := range final.Assets() {
		assert.FileExists(t, filepath.Join(dir, name))
	}
	left, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, left)

	s.stop()
	client.CloseIdleConnections()
	// Not assert.Eventually: it polls from goroutines of its own.
	until := time.Now().Add(waitFor)
	for runtime.NumGoroutine() > before && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), before, "a goroutine is left")
}

// An input edit is applied live through every way of the apply path.
func TestLiveEditOverTheApplyPath(t *testing.T) {
	s := testConfig(t, func(su *setup) {
		su.Text = baseText(streamYAML("cam", "{hls: true}"), streamYAML("other", "{hls: true}"))
	})
	gen := s.generation("cam")
	sp := specOf("cam", spec.Outputs{HLS: true})
	sp.Inputs = append(sp.Inputs, spec.Input{URL: "rtmp://backup.example/live/cam"})
	out, _, err := s.PutStream(bg, sp, "")
	require.NoError(t, err)
	assert.Equal(t, stream.Outcome{Name: "cam", Applied: "live"}, out)
	assert.Contains(t, s.fileText(), "rtmp://backup.example/live/cam")
	assert.Equal(t, gen, s.generation("cam"))

	text := strings.Replace(s.fileText(), "rtmp://backup.example/live/cam", "rtmp://spare.example/live/cam", 1)
	r := s.putConfig(text)
	require.Equal(t, http.StatusOK, r.code, r.body)
	assert.Contains(t, r.body, `{"name":"cam","applied":"live"}`)
	assert.Contains(t, r.body, `{"name":"other","applied":"unchanged"}`)
	assert.Equal(t, text, s.fileText())
	assert.Equal(t, gen, s.generation("cam"))
}
