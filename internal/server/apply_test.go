package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

var bg = context.Background()

// testParse is the tests' strict parse.
func testParse(t *testing.T, text string) config.Parsed {
	t.Helper()
	p, err := config.Parse([]byte(text), config.Options{RequiredIterations: testIter})
	require.NoError(t, err)
	return p
}

// specOf is the spec of streamYAML(name, …) with the outputs out.
func specOf(name string, out spec.Outputs) spec.Stream {
	return spec.Stream{Name: name, Inputs: []spec.Input{{URL: "rtmp://src.example/live/" + name}}, Outputs: out}
}

// freeUDP is a UDP port that was free a moment ago.
func freeUDP(t *testing.T) string {
	t.Helper()
	pc := listenUDP(t, "127.0.0.1:0")
	defer pc.Close()
	return port(t, pc.LocalAddr().String())
}

func TestStartIsLenient(t *testing.T) {
	udp := freeUDP(t)
	good := streamYAML("good", "{hls: true}")
	bad := "  - name: bad\n    inputs:\n      - url: rtmp://src.example/live/bad\n    colour: blue\n    outputs: {hls: true}\n"
	oct := "  - name: oct\n    inputs:\n      - url: rtmp://src.example/live/oct\n    thumbnail: {height: 0720}\n    outputs: {hls: true}\n"
	src := "  - &src\n    name: src\n    inputs:\n      - url: rtmp://src.example/live/src\n    outputs: {hls: true}\n"
	ali := "  - {<<: *src, name: ali}\n"
	pair := func(name string) string {
		return "  - name: " + name + "\n    inputs:\n      - url: udp://127.0.0.1:" + udp + "\n    outputs: {hls: true}\n"
	}

	t.Run("start", func(t *testing.T) {
		s := testConfig(t, func(su *setup) {
			su.Text = baseText(good, bad, oct, src, ali, pair("u1"), pair("u2"))
		})
		s.generation("good")
		s.generation("u1")
		st, ok := s.Manager().Status("u2")
		require.True(t, ok)
		assert.Equal(t, "stopped", st.State)
		assert.NotEmpty(t, st.LastError)
		for _, name := range []string{"bad", "oct", "ali", "src"} {
			_, ok := s.Manager().Get(name)
			assert.False(t, ok, name)
		}

		status := s.status()
		require.Len(t, status.LoadErrors, 5, "%v", status.LoadErrors)
		all := strings.Join(status.LoadErrors, "\n")
		for _, name := range []string{"bad", "oct", "src", "ali"} {
			assert.Regexp(t, regexp.MustCompile(`(?m)^streams\[\d+\] \(`+name+`\): line \d+: `), all)
		}
		assert.Contains(t, all, `"u2"`)
		assert.NotContains(t, all, "pbkdf2")
		assert.InDelta(t, 5, s.metric("openstreamer_config_load_errors"), 0)
		for _, e := range status.LoadErrors {
			assert.Contains(t, s.logs.String(), strings.ReplaceAll(e, `"`, `\"`), "each is logged")
		}

		// Every way in is refused while the file holds those entries.
		res := s.do(http.MethodPut, "/api/v1/streams/other", "ops", streamJSON("other", "rtmp://src.example/live/other", `{"hls":true}`))
		assert.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		res = s.do(http.MethodPost, "/api/v1/reload", "ops", "")
		assert.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		for _, name := range []string{"bad", "oct"} {
			assert.Contains(t, res.body, "("+name+")")
		}
		res = s.putConfig(s.fileText())
		assert.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		_, ok = s.Manager().Get("other")
		assert.False(t, ok)
	})

	t.Run("refused starts", func(t *testing.T) {
		users := usersYAML()
		viewerOnly := "users:\n  - name: eye\n    role: viewer\n    password: \"" + hashOf(viewerPW) + "\"\n"
		for name, text := range map[string]string{
			"an anchor on the streams sequence": "version: 1\n" + serverYAML + users + "streams: &all\n" + good,
			"an octal number in server":         "version: 1\nserver:\n  srt: {latency_ms: 0120}\n" + users,
			"no editor":                         "version: 1\n" + serverYAML + viewerOnly,
			"a bad server section":              "version: 1\nserver:\n  http: {listen: \"nope\"}\n" + users,
			"an unknown server key":             "version: 1\nserver:\n  colour: blue\n" + users,
			"a transcoder that does not exist":  "version: 1\nserver:\n  http: {listen: \"127.0.0.1:0\"}\n  api: {listen: \"127.0.0.1:0\"}\n  transcoder: {binary: /nonexistent/ostranscoder}\n" + users,
			"a 599999-iteration hash":           "version: 1\n" + serverYAML + strings.ReplaceAll(users, "$1000$", "$999$"),
		} {
			_, err := New(writeConfig(t, text), Options{LogOut: io.Discard, RequiredIterations: testIter})
			require.Error(t, err, name)
			assert.NotContains(t, err.Error(), "pbkdf2-sha256$", name)
		}
	})

	// The repairs, each on a fresh server started from a file whose only
	// fault is the one it repairs.
	fileA := baseText(good, bad)
	startA := func(t *testing.T) *testServer {
		s := testConfig(t, func(su *setup) { su.Text = fileA })
		require.Len(t, s.status().LoadErrors, 1)
		res := s.do(http.MethodPut, "/api/v1/streams/good", "ops", streamJSON("good", "rtmp://src.example/live/good", `{"hls":true,"dash":true}`))
		require.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		require.Contains(t, res.body, "(bad)")
		return s
	}
	repaired := func(t *testing.T, s *testServer) {
		t.Helper()
		testParse(t, s.fileText())
		assert.Empty(t, s.status().LoadErrors)
		assert.InDelta(t, 0, s.metric("openstreamer_config_load_errors"), 0)
		res := s.do(http.MethodPut, "/api/v1/streams/good", "ops", streamJSON("good", "rtmp://src.example/live/good", `{"hls":true,"dash":true}`))
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `"applied":"outputs"`)
	}
	t.Run("repair by a stream PUT", func(t *testing.T) {
		s := startA(t)
		res := s.do(http.MethodPut, "/api/v1/streams/bad", "ops", streamJSON("bad", "rtmp://src.example/live/bad", `{"hls":true}`))
		require.Equal(t, http.StatusCreated, res.code, res.body)
		assert.Contains(t, res.body, `"applied":"created"`)
		repaired(t, s)
		s.generation("bad")
	})
	t.Run("repair by a stream DELETE", func(t *testing.T) {
		s := startA(t)
		res := s.do(http.MethodDelete, "/api/v1/streams/bad", "ops", "")
		require.Equal(t, http.StatusNoContent, res.code, res.body)
		repaired(t, s)
		assert.NotContains(t, s.fileText(), "name: bad")
		// A name neither the manager nor the text has.
		res = s.do(http.MethodDelete, "/api/v1/streams/bad", "ops", "")
		require.Equal(t, http.StatusNotFound, res.code, res.body)
	})
	t.Run("repair by a config PUT", func(t *testing.T) {
		s := startA(t)
		res := s.putConfig(strings.Replace(fileA, "    colour: blue\n", "", 1))
		require.Equal(t, http.StatusOK, res.code, res.body)
		repaired(t, s)
	})
	t.Run("anchors need a config PUT", func(t *testing.T) {
		s := testConfig(t, func(su *setup) { su.Text = baseText(good, src, ali) })
		require.Len(t, s.status().LoadErrors, 2)
		for _, name := range []string{"src", "ali", "good"} {
			res := s.do(http.MethodPut, "/api/v1/streams/"+name, "ops", streamJSON(name, "rtmp://src.example/live/"+name, `{"hls":true}`))
			require.Equal(t, http.StatusUnprocessableEntity, res.code, "%s: %s", name, res.body)
		}
		res := s.putConfig(baseText(good, streamYAML("src", "{hls: true}"), streamYAML("ali", "{hls: true}")))
		require.Equal(t, http.StatusOK, res.code, res.body)
		repaired(t, s)
		assert.Len(t, s.List(), 3)
	})
	t.Run("a stream left stopped is repaired by its own edit", func(t *testing.T) {
		s := testConfig(t, func(su *setup) { su.Text = baseText(pair("u1"), pair("u2")) })
		require.Len(t, s.status().LoadErrors, 1)
		// An edit of another stream leaves the record.
		_, err := s.PutStream(bg, specOf("good", spec.Outputs{HLS: true}), "")
		require.NoError(t, err)
		require.Len(t, s.status().LoadErrors, 1)
		u2 := specOf("u2", spec.Outputs{HLS: true})
		out, err := s.PutStream(bg, u2, "")
		require.NoError(t, err)
		assert.Equal(t, "started", out.Applied)
		assert.Empty(t, s.status().LoadErrors)
	})
}

// A stream the start left stopped is tried again by every edit and reload
// that leaves it unchanged, and its record follows.
func TestStoppedStreamStartsLater(t *testing.T) {
	udp := freeUDP(t)
	pair := func(name string) string {
		return "  - name: " + name + "\n    inputs:\n      - url: udp://127.0.0.1:" + udp + "\n    outputs: {hls: true}\n"
	}
	u2JSON := streamJSON("u2", "udp://127.0.0.1:"+udp, `{"hls":true}`)
	// two streams on one UDP port: the second is left stopped, naming the
	// first.
	begin := func(t *testing.T) *testServer {
		s := testConfig(t, func(su *setup) { su.Text = baseText(pair("u1"), pair("u2")) })
		s.generation("u1")
		st, ok := s.Status("u2")
		require.True(t, ok)
		require.Equal(t, "stopped", st.State)
		require.Contains(t, st.LastError, `of stream "u1"`)
		require.Len(t, s.status().LoadErrors, 1)
		return s
	}
	runs := func(t *testing.T, s *testServer) {
		t.Helper()
		s.generation("u2")
		st, _ := s.Status("u2")
		assert.Empty(t, st.LastError)
		assert.Nil(t, st.LastErrorAt)
		assert.Empty(t, s.status().LoadErrors)
		assert.InDelta(t, 0, s.metric("openstreamer_config_load_errors"), 0)
	}

	t.Run("still in the way: unchanged, the reason refreshed", func(t *testing.T) {
		s := begin(t)
		before, _ := s.Status("u2")
		time.Sleep(2 * time.Millisecond)
		res := s.do(http.MethodPut, "/api/v1/streams/u2", "ops", u2JSON)
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `"applied":"unchanged"`)
		after, _ := s.Status("u2")
		assert.Equal(t, "stopped", after.State)
		assert.True(t, after.LastErrorAt.After(*before.LastErrorAt))
		require.Len(t, s.status().LoadErrors, 1)
		res = s.do(http.MethodPost, "/api/v1/reload", "ops", "")
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `{"name":"u2","applied":"unchanged"}`)
		require.Len(t, s.status().LoadErrors, 1)
	})
	t.Run("the delete of the stream in its way starts it", func(t *testing.T) {
		s := begin(t)
		res := s.do(http.MethodDelete, "/api/v1/streams/u1", "ops", "")
		require.Equal(t, http.StatusNoContent, res.code, res.body)
		runs(t, s)
	})
	t.Run("a reload that deletes the stream in its way starts it", func(t *testing.T) {
		s := begin(t)
		s.handEdit(baseText(pair("u2")))
		res := s.do(http.MethodPost, "/api/v1/reload", "ops", "")
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `{"name":"u1","applied":"deleted"}`)
		assert.Contains(t, res.body, `{"name":"u2","applied":"started"}`)
		runs(t, s)
	})

	// An edit that puts another stream in its way leaves it stopped, and
	// its reason no longer names the stream that is gone.
	blocked := func(t *testing.T) *testServer {
		s := testConfig(t, func(su *setup) { su.Text = baseText(pair("u1"), pair("u2")) })
		s.generation("u1")
		return s
	}
	t.Run("the reason names what is in the way now", func(t *testing.T) {
		s := blocked(t)
		u0 := spec.Stream{Name: "u0", Inputs: []spec.Input{{URL: "udp://127.0.0.1:" + udp}}, Outputs: spec.Outputs{HLS: true}}
		_, etag := s.ConfigText()
		res, err := s.PutConfig(bg, []byte(baseText(pair("u0"), pair("u2"))), etag)
		require.NoError(t, err)
		assert.Equal(t, "created", applied(t, res, u0.Name))
		assert.Equal(t, "deleted", applied(t, res, "u1"))
		assert.Equal(t, "unchanged", applied(t, res, "u2"))
		st, _ := s.Status("u2")
		assert.Contains(t, st.LastError, `of stream "u0"`)
		assert.NotContains(t, st.LastError, "u1", "the error names a deleted stream")
		errs := s.status().LoadErrors
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0], `of stream "u0"`)
	})

	// The asset a watermark needs is missing at the start and written
	// later: nothing in the config changes, and the same document starts
	// the stream.
	assets := func(t *testing.T) (*testServer, string, string) {
		dir := t.TempDir()
		bin := filepath.Join(t.TempDir(), "ostranscoder")
		require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o700))
		wm := "  - name: wm\n    inputs:\n      - url: rtmp://src.example/live/wm\n" +
			"    transcode:\n      renditions:\n        - {label: \"720\", height: 720, bitrate_kbps: 1000}\n" +
			"      watermark: {image: logo.png}\n    outputs: {hls: true}\n"
		s := testConfig(t, func(su *setup) {
			su.Text = strings.Replace(baseText(wm), "server:\n",
				"server:\n  transcoder: {binary: "+bin+"}\n  assets_dir: "+dir+"\n", 1)
		})
		st, ok := s.Status("wm")
		require.True(t, ok)
		require.Equal(t, "stopped", st.State)
		require.Contains(t, st.LastError, "watermark")
		require.Len(t, s.status().LoadErrors, 1)
		doc := `{"name":"wm","inputs":[{"url":"rtmp://src.example/live/wm"}],` +
			`"transcode":{"renditions":[{"label":"720","height":720,"bitrate_kbps":1000}],"audio":{},"watermark":{"image":"logo.png"}},` +
			`"outputs":{"hls":true}}`
		return s, filepath.Join(dir, "logo.png"), doc
	}
	logo := func(t *testing.T, path string) {
		t.Helper()
		var b bytes.Buffer
		require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))))
		require.NoError(t, os.WriteFile(path, b.Bytes(), 0o600))
	}
	started := func(t *testing.T, s *testServer) {
		t.Helper()
		st, ok := s.Status("wm")
		require.True(t, ok)
		assert.NotEqual(t, "stopped", st.State)
		assert.NotContains(t, st.LastError, "watermark")
		assert.Empty(t, s.status().LoadErrors)
	}
	t.Run("a PUT of the same document starts it", func(t *testing.T) {
		s, path, doc := assets(t)
		res := s.do(http.MethodPut, "/api/v1/streams/wm", "ops", doc)
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `"applied":"unchanged"`)
		logo(t, path)
		res = s.do(http.MethodPut, "/api/v1/streams/wm", "ops", doc)
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `"applied":"started"`)
		started(t, s)
		res = s.do(http.MethodPut, "/api/v1/streams/wm", "ops", doc)
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `"applied":"unchanged"`)
	})
	t.Run("a reload of the same file starts it", func(t *testing.T) {
		s, path, _ := assets(t)
		logo(t, path)
		res := s.do(http.MethodPost, "/api/v1/reload", "ops", "")
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `{"name":"wm","applied":"started"}`)
		started(t, s)
	})
	t.Run("a PUT of the same config starts it", func(t *testing.T) {
		s, path, _ := assets(t)
		logo(t, path)
		res := s.putConfig(s.fileText())
		require.Equal(t, http.StatusOK, res.code, res.body)
		assert.Contains(t, res.body, `{"name":"wm","applied":"started"}`)
		started(t, s)
	})
}

func TestBinaryGoneLaterBlocksNoEdit(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ostranscoder")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700))
	s := testConfig(t, func(su *setup) {
		su.Text = strings.Replace(baseText(streamYAML("cam", "{hls: true}")), "server:\n", "server:\n  transcoder: {binary: "+bin+"}\n", 1)
	})
	require.NoError(t, os.Remove(bin))

	_, err := s.PutStream(bg, specOf("two", spec.Outputs{HLS: true}), "")
	require.NoError(t, err)
	_, etag := s.ConfigText()
	res, err := s.PutConfig(bg, []byte(s.fileText()+"# a comment\n"), etag)
	require.NoError(t, err)
	assert.Empty(t, res.RestartRequired)
	res, err = s.Reload(bg)
	require.NoError(t, err)
	assert.Empty(t, res.RestartRequired)
	assert.Empty(t, s.ConfigStatus().LastReloadError)
}

func TestApplyPathsAgree(t *testing.T) {
	before := baseText(streamYAML("cam", "{hls: true, rtsp: true}"), streamYAML("other", "{hls: true}"))
	after := baseText(streamYAML("cam", "{hls: true}"), streamYAML("other", "{hls: true}"))
	want := testParse(t, after).Doc

	check := func(t *testing.T, s *testServer, gen uint64, cam, other string) {
		t.Helper()
		assert.Equal(t, "outputs", cam)
		assert.Equal(t, "unchanged", other)
		assert.Equal(t, gen, s.generation("cam"))
		assert.Equal(t, want, testParse(t, s.fileText()).Doc)
		got, ok := s.Get("cam")
		require.True(t, ok)
		assert.False(t, got.Outputs.RTSP)
		text, etag := s.ConfigText()
		assert.Equal(t, s.fileText(), string(text))
		assert.Equal(t, config.Hash(text), etag)
		assert.False(t, s.ConfigStatus().Stale)
	}
	begin := func(t *testing.T) (*testServer, uint64) {
		s := testConfig(t, func(su *setup) { su.Text = before })
		return s, s.generation("cam")
	}
	t.Run("config PUT", func(t *testing.T) {
		s, gen := begin(t)
		_, etag := s.ConfigText()
		res, err := s.PutConfig(bg, []byte(after), etag)
		require.NoError(t, err)
		check(t, s, gen, applied(t, res, "cam"), applied(t, res, "other"))
		assert.Equal(t, config.Hash([]byte(after)), res.ETag)
	})
	t.Run("stream PUT", func(t *testing.T) {
		s, gen := begin(t)
		out, err := s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true}), "")
		require.NoError(t, err)
		assert.Equal(t, "cam", out.Name)
		check(t, s, gen, out.Applied, "unchanged")
	})
	t.Run("hand edit and reload", func(t *testing.T) {
		s, gen := begin(t)
		s.handEdit(after)
		res, err := s.Reload(bg)
		require.NoError(t, err)
		check(t, s, gen, applied(t, res, "cam"), applied(t, res, "other"))
	})
}

func TestPutConfigWritesExactBytes(t *testing.T) {
	s := testConfig(t, func(su *setup) { su.Text = baseText(streamYAML("cam", "{hls: true}")) })
	text := "# the header\n\nversion:   1\n\n" + serverYAML + "\n" + usersYAML() + "\n\n# the streams\nstreams:\n" +
		"      - name: cam   # the camera\n        inputs:\n              - url: rtmp://src.example/live/cam\n        outputs: {hls: true}\n\n"
	_, etag := s.ConfigText()
	res, err := s.PutConfig(bg, []byte(text), etag)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", applied(t, res, "cam"))
	assert.Equal(t, text, s.fileText())
	got, etag := s.ConfigText()
	assert.Equal(t, text, string(got))
	assert.Equal(t, config.Hash([]byte(text)), etag)
	assert.Equal(t, res.ETag, etag)

	// The same bytes again are not written again.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(s.path, old, old))
	res, err = s.PutConfig(bg, []byte(text), etag)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", applied(t, res, "cam"))
	fi, err := os.Stat(s.path)
	require.NoError(t, err)
	assert.True(t, fi.ModTime().Equal(old), "the file was rewritten")

	// A text handed over is not kept by reference.
	b := []byte(text + "# more\n")
	_, err = s.PutConfig(bg, b, etag)
	require.NoError(t, err)
	b[0] = 'X'
	got, _ = s.ConfigText()
	assert.Equal(t, text+"# more\n", string(got))
	got[0] = 'X'
	again, _ := s.ConfigText()
	assert.Equal(t, text+"# more\n", string(again))
}

func TestPutConfigRefusesTheMask(t *testing.T) {
	// A document that came back redacted holds the mask where the stream
	// key was. The whole-config PUT is the one way such a text gets in
	// without the API looking at its streams, so apply refuses it.
	s := testConfig(t, func(su *setup) { su.Text = baseText(streamYAML("cam", "{hls: true}")) })
	before := s.fileText()
	for _, masked := range []string{
		"rtmp://src.example/live/caxxxxx",
		"srt://src.example:9000?passphrase=xxxxx",
	} {
		text := strings.Replace(before, "rtmp://src.example/live/cam", masked, 1)
		require.Equal(t, masked, spec.Stream{Inputs: []spec.Input{{URL: masked}}}.Redacted().Inputs[0].URL, "not a masked URL")
		res := s.putConfig(text)
		require.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		assert.Contains(t, res.body, "redaction mask")
		assert.Equal(t, before, s.fileText())
	}
	got, _ := s.Get("cam")
	assert.Equal(t, "rtmp://src.example/live/cam", got.Inputs[0].URL)
}

func TestPutStreamKeepsComments(t *testing.T) {
	text := "# the header comment\nversion: 1\n" + serverYAML + "users:\n" +
		"  # the operator\n  - name: ops\n    role: editor\n    password: \"" + hashOf(editorPW) + "\"\n" +
		"streams:\n" + streamYAML("cam", "{hls: true}") + "  # the other camera\n" + streamYAML("other", "{hls: true}")
	s := testConfig(t, func(su *setup) { su.Text = text })
	out, err := s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, DASH: true}), "")
	require.NoError(t, err)
	assert.Equal(t, "outputs", out.Applied)
	got := s.fileText()
	for _, c := range []string{"# the header comment", "# the operator", "# the other camera"} {
		assert.Contains(t, got, c)
	}
	assert.True(t, testParse(t, got).Doc.Streams[0].Outputs.DASH)
}

func TestPutConfigPreconditions(t *testing.T) {
	text := baseText(streamYAML("cam", "{hls: true}"))
	next := baseText(streamYAML("cam", "{hls: true, dash: true}"))
	s := testConfig(t, func(su *setup) { su.Text = text })
	gen := s.generation("cam")
	untouched := func(file string) {
		t.Helper()
		assert.Equal(t, file, s.fileText())
		got, _ := s.Get("cam")
		assert.False(t, got.Outputs.DASH)
		assert.Equal(t, gen, s.generation("cam"))
	}
	_, etag := s.ConfigText()
	old := config.Hash([]byte("something else"))

	_, err := s.PutConfig(bg, []byte(next), "")
	require.ErrorIs(t, err, api.ErrPreconditionRequired)
	untouched(text)
	_, err = s.PutConfig(bg, []byte(next), old)
	require.ErrorIs(t, err, api.ErrPrecondition)
	untouched(text)
	_, err = s.PutConfig(bg, []byte(next), "*")
	require.ErrorIs(t, err, api.ErrPrecondition)
	untouched(text)

	// After a hand edit the stale check comes first, whatever the ETag.
	edited := text + "# by hand\n"
	s.handEdit(edited)
	_, err = s.PutConfig(bg, []byte(next), etag)
	require.ErrorIs(t, err, config.ErrStale)
	untouched(edited)
	_, err = s.PutConfig(bg, []byte(next), old)
	require.ErrorIs(t, err, config.ErrStale)
	untouched(edited)
	// Over the API they are 428, 412 and 409.
	res := s.do(http.MethodPut, "/api/v1/config", "ops", next, "Content-Type", "application/yaml")
	assert.Equal(t, http.StatusPreconditionRequired, res.code)
	res = s.do(http.MethodPut, "/api/v1/config", "ops", next, "Content-Type", "application/yaml", "If-Match", etag)
	assert.Equal(t, http.StatusConflict, res.code)
	untouched(edited)

	// The per-stream preconditions.
	s.handEdit(text)
	cam, _ := s.Get("cam")
	_, err = s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, DASH: true}), `"0000000000000000"`)
	require.ErrorIs(t, err, api.ErrPrecondition)
	_, err = s.PutStream(bg, specOf("new", spec.Outputs{HLS: true}), "*")
	require.ErrorIs(t, err, api.ErrPrecondition)
	require.ErrorIs(t, s.DeleteStream(bg, "cam", `"0000000000000000"`), api.ErrPrecondition)
	require.ErrorIs(t, s.DeleteStream(bg, "new", "*"), api.ErrPrecondition)
	require.ErrorIs(t, s.DeleteStream(bg, "new", ""), config.ErrNoStream)
	untouched(text)
	_, err = s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, RTSP: true}), cam.ETag())
	require.NoError(t, err)
	_, err = s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true}), "*")
	require.NoError(t, err)
	cam, _ = s.Get("cam")
	require.NoError(t, s.DeleteStream(bg, "cam", cam.ETag()))
	assert.Empty(t, s.List())
}

func TestEditKeepsRememberedLogin(t *testing.T) {
	text := baseText(streamYAML("cam", "{hls: true}"))
	s := testConfig(t, func(su *setup) { su.Text = text })
	auth.WarmDummy()
	var hashes atomic.Int64
	t.Cleanup(auth.SetHashHook(func(next func() []byte) []byte {
		hashes.Add(1)
		return next()
	}))
	users := s.users.Load()

	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "ops", "").code)
	require.EqualValues(t, 1, hashes.Load())

	res := s.do(http.MethodPut, "/api/v1/streams/cam", "ops", streamJSON("cam", "rtmp://src.example/live/cam", `{"hls":true,"dash":true}`))
	require.Equal(t, http.StatusOK, res.code, res.body)
	res = s.putConfig(strings.Replace(s.fileText(), "dash: true", "dash: false", 1))
	require.Equal(t, http.StatusOK, res.code, res.body)
	s.handEdit(s.fileText() + "# by hand\n")
	res = s.do(http.MethodPost, "/api/v1/reload", "ops", "")
	require.Equal(t, http.StatusOK, res.code, res.body)
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "ops", "").code)
	assert.EqualValues(t, 1, hashes.Load(), "an edit that leaves the users alone costs no PBKDF2")
	assert.Same(t, users, s.users.Load())

	// A changed users section replaces the set.
	other := auth.NewHash(viewerPW, []byte("fedcba9876543210"), testIter).String()
	next := strings.Replace(s.fileText(), hashOf(viewerPW), other, 1)
	base := hashes.Load() // making the two hashes above went through the hook too
	res = s.putConfig(next)
	require.Equal(t, http.StatusOK, res.code, res.body)
	assert.NotSame(t, users, s.users.Load())
	assert.Equal(t, base, hashes.Load(), "the PUT itself was a remembered login")
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "ops", "").code)
	assert.Equal(t, base+1, hashes.Load())
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "ops", "").code)
	assert.Equal(t, base+1, hashes.Load())
}

// heldRebuild starts a server with the stream cam, holds its teardown, and
// sends a per-stream PUT that rebuilds it. It returns once the edit is in
// the file, with the PUT still waiting for the rebuild.
// hangUp cancels the PUT's context, as its client leaving does.
func heldRebuild(t *testing.T) (s *testServer, sc *script, release, hangUp func(), done chan error) {
	t.Helper()
	sc = newScript(t)
	s = testConfig(t, func(su *setup) {
		su.Opt.Open = sc.Open
		su.Text = baseText(streamYAML("cam", "{hls: true}"), streamYAML("other", "{hls: true}"))
	})
	s.generation("cam")
	release = sc.hold()
	t.Cleanup(release)
	edit := specOf("cam", spec.Outputs{HLS: true})
	edit.Inputs[0].URL = "rtmp://src.example/live/cam2"
	done = make(chan error, 1)
	ctx, hangUp := context.WithCancel(context.Background())
	t.Cleanup(hangUp)
	go func() {
		_, err := s.PutStream(ctx, edit, "")
		done <- err
	}()
	require.Eventually(t, func() bool { return strings.Contains(s.fileText(), "live/cam2") }, waitFor, time.Millisecond)
	select {
	case err := <-done:
		require.Fail(t, "the PUT did not wait for the rebuild", "%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	return s, sc, release, hangUp, done
}

func TestClientGoneDuringRebuild(t *testing.T) {
	s, sc, release, hangUp, done := heldRebuild(t)
	hangUp() // the client leaves after the file was written
	select {
	case err := <-done:
		require.NoError(t, err, "the edit is in the file and in force")
	case <-time.After(waitFor):
		require.Fail(t, "the PUT did not return when its client left")
	}
	file := s.fileText()
	text, etag := s.ConfigText()
	assert.Equal(t, file, string(text))
	assert.Equal(t, config.Hash([]byte(file)), etag)
	assert.False(t, s.ConfigStatus().Stale)
	cam := func(streams []spec.Stream) string {
		for _, sp := range streams {
			if sp.Name == "cam" {
				return sp.Inputs[0].URL
			}
		}
		return ""
	}
	assert.Equal(t, "rtmp://src.example/live/cam2", cam(testParse(t, file).Doc.Streams))
	got, _ := s.Get("cam")
	assert.Equal(t, "rtmp://src.example/live/cam2", got.Inputs[0].URL)

	// A later edit of another stream keeps the first: nothing is reverted.
	out, err := s.PutStream(bg, specOf("other", spec.Outputs{HLS: true, DASH: true}), "")
	require.NoError(t, err)
	assert.Equal(t, "outputs", out.Applied)
	assert.Equal(t, "rtmp://src.example/live/cam2", cam(testParse(t, s.fileText()).Doc.Streams))
	assert.False(t, sc.opened("rtmp://src.example/live/cam2"), "the teardown is still held")

	release()
	require.Eventually(t, func() bool { return sc.opened("rtmp://src.example/live/cam2") }, waitFor, 5*time.Millisecond)
	s.generation("cam")
}

func TestStatusDoesNotWaitForApply(t *testing.T) {
	s, _, release, _, done := heldRebuild(t)
	t0 := time.Now()
	st := s.ConfigStatus()
	text, etag := s.ConfigText()
	assert.Less(t, time.Since(t0), 100*time.Millisecond)
	// The apply has not finished: the text in force is still the old one,
	// and the file, which is ahead of it, reads as stale until it has.
	assert.NotContains(t, string(text), "live/cam2")
	assert.Equal(t, etag, st.ETag)
	// The streams, the users and the metrics do not wait either.
	t0 = time.Now()
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/config/status", "eye", "").code)
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "eye", "").code)
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/metrics", "eye", "").code)
	assert.Less(t, time.Since(t0), 2*time.Second)

	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitFor):
		require.Fail(t, "the PUT did not return")
	}
	text, _ = s.ConfigText()
	assert.Contains(t, string(text), "live/cam2")
	assert.False(t, s.ConfigStatus().Stale)
}

func TestStaleIsLive(t *testing.T) {
	text := baseText(streamYAML("cam", "{hls: true}"))
	s := testConfig(t, func(su *setup) { su.Text = text })
	assert.False(t, s.ConfigStatus().Stale)
	s.handEdit(text + "# by hand\n")
	assert.True(t, s.ConfigStatus().Stale)
	assert.True(t, s.status().Stale)
	s.handEdit(text)
	assert.False(t, s.ConfigStatus().Stale)
	// A chmod alone changes no byte.
	require.NoError(t, os.Chmod(s.path, 0o644))
	assert.False(t, s.ConfigStatus().Stale)
	require.NoError(t, os.Chmod(s.path, 0o600))
	// A file that cannot be read is stale, and said so in the log, once.
	require.NoError(t, os.Remove(s.path))
	for range 3 {
		assert.True(t, s.ConfigStatus().Stale)
	}
	assert.Equal(t, 1, strings.Count(s.logs.String(), "the file cannot be read"))
	_, err := s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, DASH: true}), "")
	require.ErrorIs(t, err, config.ErrStale)
	s.handEdit(text)
	assert.False(t, s.ConfigStatus().Stale)
}

func TestDirSyncFails(t *testing.T) {
	text := baseText(streamYAML("cam", "{hls: true}"))
	s := testConfig(t, func(su *setup) { su.Text = text })
	gen := s.generation("cam")
	// The directory fsync fails after the rename.
	s.file.SetHook(func(step string) error {
		if step == "renamed" {
			return errors.New("fsync: input/output error")
		}
		return nil
	})
	res := s.do(http.MethodPut, "/api/v1/streams/cam", "ops", streamJSON("cam", "rtmp://src.example/live/cam", `{"hls":true,"dash":true}`))
	require.Equal(t, http.StatusInternalServerError, res.code, res.body)
	s.file.SetHook(nil)

	// The new text is on disk and nothing was applied.
	assert.Contains(t, s.fileText(), "dash: true")
	got, _ := s.Get("cam")
	assert.False(t, got.Outputs.DASH)
	applied, _ := s.ConfigText()
	assert.Equal(t, text, string(applied))
	assert.True(t, s.status().Stale)
	_, err := s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, RTSP: true}), "")
	require.ErrorIs(t, err, config.ErrStale)
	_, etag := s.ConfigText()
	_, err = s.PutConfig(bg, []byte(text), etag)
	require.ErrorIs(t, err, config.ErrStale)

	// A reload takes the file as it is.
	out, err := s.Reload(bg)
	require.NoError(t, err)
	assert.Equal(t, "outputs", out.Streams[0].Applied)
	got, _ = s.Get("cam")
	assert.True(t, got.Outputs.DASH)
	assert.Equal(t, gen, s.generation("cam"))
	assert.False(t, s.status().Stale)
	_, err = s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true}), "")
	require.NoError(t, err)
}

func TestHandEditThenWrite(t *testing.T) {
	text := baseText(streamYAML("cam", "{hls: true}"), streamYAML("other", "{hls: true}"))
	s := testConfig(t, func(su *setup) { su.Text = text })
	edited := strings.Replace(text, "streams:\n", "# added by hand\nstreams:\n", 1)
	s.handEdit(edited)
	_, etag := s.ConfigText()

	_, err := s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, DASH: true}), "")
	require.ErrorIs(t, err, config.ErrStale)
	require.ErrorIs(t, s.DeleteStream(bg, "other", ""), config.ErrStale)
	_, err = s.PutConfig(bg, []byte(text), etag)
	require.ErrorIs(t, err, config.ErrStale)
	assert.Equal(t, edited, s.fileText(), "the hand edit is intact")
	assert.True(t, s.status().Stale)
	assert.Len(t, s.List(), 2)

	_, err = s.Reload(bg)
	require.NoError(t, err)
	assert.False(t, s.status().Stale)
	_, err = s.PutStream(bg, specOf("cam", spec.Outputs{HLS: true, DASH: true}), "")
	require.NoError(t, err)
	assert.Contains(t, s.fileText(), "# added by hand")
	require.NoError(t, s.DeleteStream(bg, "other", ""))
	_, etag = s.ConfigText()
	_, err = s.PutConfig(bg, []byte(text), etag)
	require.NoError(t, err)
	assert.Equal(t, text, s.fileText())
}

func TestReloadAppliesDiff(t *testing.T) {
	text := baseText(streamYAML("a", "{hls: true}"), streamYAML("b", "{hls: true}"), streamYAML("c", "{hls: true}"))
	s := testConfig(t, func(su *setup) { su.Text = text })
	gen := s.generation("a")
	s.generation("b")
	hubA, ok := s.Manager().CopyHub("a")
	require.True(t, ok)
	sub := hubA.Subscribe("tap", 4096)
	var lost, frames atomic.Uint64
	tapped := make(chan struct{})
	go func() {
		defer close(tapped)
		for d := range sub.C() {
			lost.Add(d.Lost)
			frames.Add(1)
		}
	}()
	t.Cleanup(func() { sub.Close(); <-tapped })
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "eye", "").code)

	newPW := "another-viewer-password"
	newHash := auth.NewHash(newPW, []byte("fedcba9876543210"), testIter).String()
	edited := "version: 1\n" + serverYAML + strings.Replace(usersYAML(), hashOf(viewerPW), newHash, 1) + "streams:\n" +
		streamYAML("a", "{hls: true}") + streamYAML("b", "{hls: true, rtsp: true}") + streamYAML("d", "{hls: true}")
	s.handEdit(edited)
	res, err := s.Reload(bg)
	require.NoError(t, err)
	assert.Equal(t, []stream.Outcome{{Name: "a", Applied: "unchanged"}, {Name: "b", Applied: "outputs"}, {Name: "c", Applied: "deleted"}, {Name: "d", Applied: "created"}}, res.Streams)
	assert.Empty(t, res.RestartRequired)
	assert.NotNil(t, res.RestartRequired)
	assert.Equal(t, config.Hash([]byte(edited)), res.ETag)

	assert.Equal(t, gen, s.generation("a"))
	again, ok := s.Manager().CopyHub("a")
	require.True(t, ok)
	assert.Same(t, hubA, again)
	n := frames.Load()
	require.Eventually(t, func() bool { return frames.Load() > n }, waitFor, 5*time.Millisecond)
	assert.Zero(t, lost.Load())
	s.generation("d")

	// The old password is refused, the new one accepted.
	assert.Equal(t, http.StatusUnauthorized, s.do(http.MethodGet, "/api/v1/streams", "eye", "").code)
	req, err := http.NewRequestWithContext(bg, http.MethodGet, "http://"+s.Addrs().API+"/api/v1/streams", nil)
	require.NoError(t, err)
	req.SetBasicAuth("eye", newPW)
	out, err := client.Do(req)
	require.NoError(t, err)
	out.Body.Close()
	assert.Equal(t, http.StatusOK, out.StatusCode)

	status := s.ConfigStatus()
	require.NotNil(t, status.LastReloadAt)
	assert.Empty(t, status.LastReloadError)
	assert.Equal(t, res.ETag, status.ETag)
	assert.InDelta(t, 1, s.metric("openstreamer_config_reloads_total", "result", "ok"), 0)

	// A reload of a file equal to the text in force changes nothing.
	res, err = s.Reload(bg)
	require.NoError(t, err)
	for _, o := range res.Streams {
		assert.Equal(t, "unchanged", o.Applied, o.Name)
	}
}

func TestReloadInvalidKeepsState(t *testing.T) {
	cam := streamYAML("cam", "{hls: true}")
	text := baseText(cam)
	s := testConfig(t, func(su *setup) { su.Text = text })
	gen := s.generation("cam")
	a := s.Addrs()
	entry := func(name, extra string) string {
		return "  - name: " + name + "\n    inputs:\n      - url: rtmp://src.example/live/" + name + "\n" + extra + "    outputs: {hls: true}\n"
	}
	viewerOnly := "users:\n  - name: eye\n    role: viewer\n    password: \"" + hashOf(viewerPW) + "\"\n"
	rows := []struct {
		name, text string
		chmod      bool
	}{
		{name: "not YAML", text: "version: 1\n\t{{{\n"},
		{name: "an unknown key", text: text + "colour: blue\n"},
		{name: "version 2", text: strings.Replace(text, "version: 1", "version: 2", 1)},
		{name: "no editor", text: "version: 1\n" + serverYAML + viewerOnly + "streams:\n" + cam},
		{name: "a plaintext password", text: strings.Replace(text, hashOf(editorPW), "hunter2-plaintext", 1)},
		{name: "a stream failing Validate", text: text + "  - name: Not_A_Name\n    inputs:\n      - url: rtmp://src.example/live/x\n    outputs: {hls: true}\n"},
		{name: "two streams of one name", text: text + cam},
		{name: "a stream whose Check fails", text: text + entry("thumb", "    thumbnail: {interval_sec: 5}\n")},
		{name: "a claim on a reserved port", text: text + "  - name: udp\n    inputs:\n      - url: udp://127.0.0.1:" + port(t, a.SRT) + "\n    outputs: {hls: true}\n"},
		{name: "an alias", text: strings.Replace(text, "  - name: cam", "  - &cam\n    name: cam", 1) + "  - *cam\n"},
		{name: "an octal number", text: text + entry("oct", "    thumbnail: {height: 0720}\n")},
		{name: "a secret on the wrong line", text: strings.Replace(text, "srt: {listen: \"127.0.0.1:0\"}", "srt: {listen: \"127.0.0.1:0\", latency_ms: hunter2}", 1)},
		{name: "mode 0644", chmod: true},
	}
	failed := 0.0
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.chmod {
				require.NoError(t, os.Chmod(s.path, 0o644))
			} else {
				require.NotEqual(t, text, row.text, "the row changes nothing")
				s.handEdit(row.text)
			}
			_, err := s.Reload(bg)
			require.Error(t, err)
			failed++

			assert.Equal(t, gen, s.generation("cam"))
			assert.Len(t, s.List(), 1)
			applied, _ := s.ConfigText()
			assert.Equal(t, text, string(applied))
			require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "ops", "").code, "the old users still log in")
			status := s.status()
			require.NotEmpty(t, status.LastReloadError)
			require.NotNil(t, status.LastReloadAt)
			for _, secret := range []string{"hunter2", "pbkdf2", hashOf(editorPW)} {
				assert.NotContains(t, status.LastReloadError, secret)
				assert.NotContains(t, err.Error(), secret)
				assert.NotContains(t, s.logs.String(), secret)
			}
			assert.Equal(t, !row.chmod, status.Stale)
			assert.InDelta(t, failed, s.metric("openstreamer_config_reloads_total", "result", "error"), 0)
			assert.Contains(t, s.logs.String(), "reload failed")

			// Restoring the old file and reloading clears the error.
			require.NoError(t, os.Chmod(s.path, 0o600))
			s.handEdit(text)
			_, err = s.Reload(bg)
			require.NoError(t, err)
			status = s.status()
			assert.Empty(t, status.LastReloadError)
			assert.False(t, status.Stale)
		})
	}
}

func TestRestartRequired(t *testing.T) {
	text := baseText(streamYAML("cam", "{hls: true}"))
	want := []string{"server.log.level", "server.rtmp.listen"}
	edit := func(text string) string {
		text = strings.Replace(text, "rtmp: {listen: \"127.0.0.1:0\"}", "rtmp: {listen: \"127.0.0.1:19359\"}\n  log: {level: debug}", 1)
		return strings.Replace(text, "{hls: true}", "{hls: true, dash: true}", 1)
	}
	check := func(t *testing.T, s *testServer, addrs Addrs, res api.ApplyResult) {
		t.Helper()
		assert.Equal(t, want, res.RestartRequired)
		assert.Equal(t, "outputs", applied(t, res, "cam"))
		got, _ := s.Get("cam")
		assert.True(t, got.Outputs.DASH, "the stream edit is applied")
		assert.Equal(t, addrs, s.Addrs())
		assert.Equal(t, want, s.status().RestartRequired)
		assert.InDelta(t, 1, s.metric("openstreamer_config_restart_required"), 0)
		assert.Equal(t, 1, strings.Count(s.logs.String(), "restart required: server.log.level, server.rtmp.listen"))
		// The level did not change either: no debug line is written.
		assert.NotContains(t, s.logs.String(), `"level":"DEBUG"`)
	}
	cleared := func(t *testing.T, s *testServer, res api.ApplyResult) {
		t.Helper()
		assert.Empty(t, res.RestartRequired)
		assert.NotNil(t, res.RestartRequired)
		assert.Empty(t, s.status().RestartRequired)
		assert.InDelta(t, 0, s.metric("openstreamer_config_restart_required"), 0)
	}
	t.Run("reload", func(t *testing.T) {
		s := testConfig(t, func(su *setup) { su.Text = text })
		addrs := s.Addrs()
		s.handEdit(edit(text))
		res, err := s.Reload(bg)
		require.NoError(t, err)
		check(t, s, addrs, res)
		s.handEdit(text)
		res, err = s.Reload(bg)
		require.NoError(t, err)
		cleared(t, s, res)
	})
	t.Run("config PUT", func(t *testing.T) {
		s := testConfig(t, func(su *setup) { su.Text = text })
		addrs := s.Addrs()
		_, etag := s.ConfigText()
		res, err := s.PutConfig(bg, []byte(edit(text)), etag)
		require.NoError(t, err)
		check(t, s, addrs, res)
		// A stream edit while it lasts does not say it again.
		_, err = s.PutStream(bg, specOf("two", spec.Outputs{HLS: true}), "")
		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(s.logs.String(), "restart required"))
		assert.Equal(t, want, s.status().RestartRequired)
		res, err = s.PutConfig(bg, []byte(text), res.ETag)
		require.ErrorIs(t, err, api.ErrPrecondition) // the stream edit moved the ETag
		_, etag = s.ConfigText()
		res, err = s.PutConfig(bg, []byte(text), etag)
		require.NoError(t, err)
		cleared(t, s, res)
	})
}

func TestConcurrentWriters(t *testing.T) {
	entries := make([]string, 0, 8)
	for i := range 8 {
		entries = append(entries, streamYAML(fmt.Sprintf("s%d", i), "{hls: true}"))
	}
	s := testConfig(t, func(su *setup) { su.Text = baseText(entries...) })
	deadline := time.Now().Add(2 * time.Second)
	running := func() bool { return time.Now().Before(deadline) }
	var (
		wg                     sync.WaitGroup
		puts, configs, reloads atomic.Int64
		parseOpt               = config.Options{RequiredIterations: testIter}
	)
	for i := range 8 {
		wg.Go(func() {
			name := fmt.Sprintf("s%d", i)
			for n := 0; running(); n++ {
				_, err := s.PutStream(bg, specOf(name, spec.Outputs{HLS: true, DASH: n%2 == 0}), "")
				if !assert.NoError(t, err) {
					return
				}
				puts.Add(1)
				time.Sleep(time.Millisecond) // leave the whole-config writers a turn
			}
		})
	}
	for i := range 2 {
		wg.Go(func() {
			for n := 0; running(); n++ {
				text, etag := s.ConfigText()
				// A comment at the end, replacing this writer's last one.
				lines := strings.Split(strings.TrimRight(string(text), "\n"), "\n")
				mark := fmt.Sprintf("# writer %d", i)
				if strings.HasPrefix(lines[len(lines)-1], mark) {
					lines = lines[:len(lines)-1]
				}
				next := strings.Join(lines, "\n") + fmt.Sprintf("\n%s edit %d\n", mark, n)
				_, err := s.PutConfig(bg, []byte(next), etag)
				if errors.Is(err, api.ErrPrecondition) {
					continue // another writer came first: read again
				}
				if !assert.NoError(t, err) {
					return
				}
				configs.Add(1)
			}
		})
	}
	wg.Go(func() {
		for running() {
			_, err := s.Reload(bg)
			if !assert.NoError(t, err) {
				return
			}
			reloads.Add(1)
			time.Sleep(time.Millisecond)
		}
	})
	// The file always parses: a reader never sees a partial write.
	wg.Go(func() {
		for running() {
			b, err := os.ReadFile(s.path)
			if !assert.NoError(t, err) {
				return
			}
			if _, err := config.Parse(b, parseOpt); !assert.NoError(t, err) {
				return
			}
			_ = s.ConfigStatus()
		}
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(waitFor + 2*time.Second):
		require.Fail(t, "deadlock")
	}
	assert.Positive(t, puts.Load())
	assert.Positive(t, configs.Load())
	assert.Positive(t, reloads.Load())

	file := testParse(t, s.fileText()).Doc.Streams
	list := s.List()
	require.Len(t, list, len(file))
	byName := map[string]spec.Stream{}
	for _, sp := range file {
		byName[sp.Name] = sp
	}
	for _, sp := range list {
		assert.Equal(t, spec.ChangeNone, spec.Diff(byName[sp.Name], sp), sp.Name)
	}
	text, _ := s.ConfigText()
	assert.Equal(t, s.fileText(), string(text))
	assert.False(t, s.ConfigStatus().Stale)
	left, err := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, left)
}
