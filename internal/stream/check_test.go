package stream

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// Secrets of the redaction tests, built so the source holds none whole.
var (
	vPass = "hun" + "ter2"
	vKey  = "sekrit" + "key123"
)

// camSpec is a stream "cam" with one input and every output on.
func camSpec() spec.Stream {
	return spec.Stream{
		Name:   "cam",
		Inputs: []spec.Input{{URL: "http://source.test/cam.ts"}},
		Outputs: spec.Outputs{
			HLS: true, DASH: true, HTTPTS: true, RTMP: true, RTSP: true, SRT: true,
		},
	}
}

// served is env with the server's three session servers on, at addresses
// nothing binds, as Check wants for a stream that turns their outputs on.
// An RTMP play address the caller set is kept.
func served(env Env) Env {
	if env.RTMPPlay == "" {
		env.RTMPPlay, env.RTMPApp = "192.0.2.1:19350", "play"
	}
	env.RTSPPlay, env.SRTPlay = "192.0.2.1:15554", "192.0.2.1:19999"
	return env
}

// rtmpOff is a row edit: the server has no RTMP listener, and the stream
// does not ask for the output.
func rtmpOff(sp *spec.Stream, env *Env) {
	env.RTMPPlay, env.RTMPApp = "", ""
	sp.Outputs.RTMP = false
}

// writeAsset writes a 1×1 PNG named name into a new assets directory.
func writeAsset(t testing.TB, name string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), onePixelPNG, 0o600))
	return dir
}

// onePixelPNG is a 1×1 opaque PNG.
var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
	0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x03, 0x01, 0x01, 0x00, 0xc9, 0xfe, 0x92, 0xef, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
	0x44, 0xae, 0x42, 0x60, 0x82,
}

// Check refuses, with an error that starts "stream: check:" and holds no
// password, what cannot run under the Env; and accepts what can.
func TestCheck(t *testing.T) {
	assets := writeAsset(t, "logo.png")
	const play = "127.0.0.1:1935"
	in := func(u string) func(*spec.Stream, *Env) {
		return func(sp *spec.Stream, _ *Env) { sp.Inputs = []spec.Input{{URL: u}} }
	}
	encoded := func(sp *spec.Stream, _ *Env) {
		sp.Transcode = &spec.Transcode{Renditions: []spec.Rendition{{Label: "720", Height: 720, BitrateKbps: 1000}}}
	}
	binary := func(_ *spec.Stream, env *Env) { env.Transcoder = "/opt/ostranscoder" }
	rtmpPlay := func(_ *spec.Stream, env *Env) { env.RTMPPlay, env.RTMPApp = play, "live" }
	watermark := func(w spec.Watermark) func(*spec.Stream, *Env) {
		return func(sp *spec.Stream, _ *Env) { sp.Transcode.Watermark = &w }
	}
	rows := []struct {
		name string
		edit []func(*spec.Stream, *Env)
		want string // "" passes; else a part of the error
	}{
		{"plain", nil, ""},
		{"a URL ingest refuses", []func(*spec.Stream, *Env){in("gopher://u:" + vPass + "@h/x")}, "inputs[0]"},
		{"a second input ingest refuses", []func(*spec.Stream, *Env){func(sp *spec.Stream, _ *Env) {
			sp.Inputs = append(sp.Inputs, spec.Input{URL: "rtmp://u:" + vPass + "@h/onlyapp"})
		}}, "inputs[1]"},
		{"an encoded rung without the binary", []func(*spec.Stream, *Env){encoded}, "encoding needs the transcoder"},
		{"an encoded rung with the binary", []func(*spec.Stream, *Env){encoded, binary}, ""},
		{"encoded audio without the binary", []func(*spec.Stream, *Env){func(sp *spec.Stream, _ *Env) {
			sp.Transcode = &spec.Transcode{Renditions: []spec.Rendition{{Label: "src", Copy: true}}}
		}}, "encoding needs the transcoder"},
		{"a copy-only ladder with copy audio and no binary", []func(*spec.Stream, *Env){func(sp *spec.Stream, _ *Env) {
			sp.Transcode = &spec.Transcode{Renditions: []spec.Rendition{{Label: "src", Copy: true}}, Audio: spec.Audio{Copy: true}}
		}}, ""},
		{"a height the transcoder refuses", []func(*spec.Stream, *Env){encoded, binary, func(sp *spec.Stream, _ *Env) {
			sp.Transcode.Renditions[0].BFrames = 9
		}}, "transcode"},
		{"an image watermark without the assets directory", []func(*spec.Stream, *Env){
			encoded, binary, watermark(spec.Watermark{Image: "logo.png"}),
		}, "assets directory"},
		{"an asset that does not exist", []func(*spec.Stream, *Env){
			encoded, binary, watermark(spec.Watermark{Image: "nologo.png"}),
			func(_ *spec.Stream, env *Env) { env.AssetsDir = assets },
		}, "nologo.png"},
		{"an image watermark", []func(*spec.Stream, *Env){
			encoded, binary, watermark(spec.Watermark{Image: "logo.png", Position: "br"}),
			func(_ *spec.Stream, env *Env) { env.AssetsDir = assets },
		}, ""},
		{"rtmp on, the RTMP listener off", []func(*spec.Stream, *Env){func(_ *spec.Stream, env *Env) {
			env.RTMPPlay, env.RTMPApp = "", ""
		}}, "outputs.rtmp: the server's rtmp listener is off: set server.rtmp.listen"},
		{"rtsp on, the RTSP listener off", []func(*spec.Stream, *Env){func(_ *spec.Stream, env *Env) {
			env.RTSPPlay = ""
		}}, "outputs.rtsp: the server's rtsp listener is off: set server.rtsp.listen"},
		{"srt on, the SRT listener off", []func(*spec.Stream, *Env){func(_ *spec.Stream, env *Env) {
			env.SRTPlay = ""
		}}, "outputs.srt: the server's srt listener is off: set server.srt.listen"},
		{"every listener off, and no such output on", []func(*spec.Stream, *Env){func(sp *spec.Stream, env *Env) {
			*env = Env{}
			sp.Outputs = spec.Outputs{HLS: true, DASH: true, HTTPTS: true}
		}}, ""},
		{"a text watermark", []func(*spec.Stream, *Env){encoded, binary, watermark(spec.Watermark{Text: "live %H:%M"})}, ""},
		{"a watermark next to a copy rung", []func(*spec.Stream, *Env){encoded, binary, func(sp *spec.Stream, _ *Env) {
			sp.Transcode.Renditions = append(sp.Transcode.Renditions, spec.Rendition{Label: "src", Copy: true})
			sp.Transcode.Watermark = &spec.Watermark{Text: "x"}
		}}, "transcode"},
		{"a thumbnail without the binary", []func(*spec.Stream, *Env){func(sp *spec.Stream, _ *Env) {
			sp.Thumbnail = &spec.Thumbnail{}
		}}, "thumbnail"},
		{"a thumbnail out of range", []func(*spec.Stream, *Env){binary, func(sp *spec.Stream, _ *Env) {
			sp.Thumbnail = &spec.Thumbnail{Quality: 1000}
		}}, "thumbnail"},
		{"a thumbnail", []func(*spec.Stream, *Env){binary, func(sp *spec.Stream, _ *Env) {
			sp.Thumbnail = &spec.Thumbnail{IntervalSec: 5}
		}}, ""},
		{"a thumbnail on an encoded ladder", []func(*spec.Stream, *Env){encoded, binary, func(sp *spec.Stream, _ *Env) {
			sp.Thumbnail = &spec.Thumbnail{}
		}}, ""},
		{"a push that is not rtmp", []func(*spec.Stream, *Env){func(sp *spec.Stream, _ *Env) {
			sp.Outputs.Push = []spec.Push{{URL: "rtmp://a.test/live/" + vKey}, {URL: "http://u:" + vPass + "@h/live/" + vKey}}
		}}, "outputs.push[1]"},
		{"a push", []func(*spec.Stream, *Env){func(sp *spec.Stream, _ *Env) {
			sp.Outputs.Push = []spec.Push{{URL: "rtmp://a.test/live/" + vKey}}
		}}, ""},
		{"publishing at live/cam on the play address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://" + play + "/live/cam?mode=listen")}, "play app"},
		{"publishing at live/other on the play address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://" + play + "/live/other?mode=listen")}, "play app"},
		{"publishing at live/other, listener spelling", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://" + play + "/live/other?mode=listener")}, "play app"},
		{"publishing at live/a/b on the play address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://" + play + "/live/a/b?mode=listen")}, ""},
		{"publishing at in/key on the play address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://" + play + "/in/key?mode=listen")}, ""},
		{"publishing at live/cam on another address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://127.0.0.1:1936/live/cam?mode=listen")}, ""},
		{"publishing at live/other on another address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://127.0.0.1:1936/live/other?mode=listen")}, ""},
		{"publishing at live/cam with RTMP play off", []func(*spec.Stream, *Env){rtmpOff, in("rtmp://" + play + "/live/cam?mode=listen")}, ""},
		{"pulling live/cam from the play address", []func(*spec.Stream, *Env){rtmpPlay, in("rtmp://" + play + "/live/cam")}, ""},
		{"the play app with slashes", []func(*spec.Stream, *Env){
			func(_ *spec.Stream, env *Env) { env.RTMPPlay, env.RTMPApp = ":1935", "/live/" },
			in("rtmp://0.0.0.0/live/cam?mode=listen"),
		}, "play app"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			sp, env := camSpec(), served(Env{})
			for _, e := range row.edit {
				e(&sp, &env)
			}
			require.NoError(t, sp.Validate(), "the row must pass Validate: Check runs after it")
			err := Check(sp, env)
			if row.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), row.want)
			assert.Regexp(t, `^stream: check: `, err.Error())
			assert.NotContains(t, err.Error(), vPass)
			assert.NotContains(t, err.Error(), vKey)
			assert.Error(t, errors.Unwrap(err), "the cause is kept")
		})
	}
}

// With Env.Open set, as in tests, the input URLs are not parsed.
func TestCheckSkipsParseWithOpen(t *testing.T) {
	sp := camSpec()
	sp.Inputs = []spec.Input{{URL: "scripted://anything"}}
	require.Error(t, Check(sp, served(Env{})))
	open := func(context.Context, string) (ingest.Reader, error) { return nil, errors.New("not opened by Check") }
	require.NoError(t, Check(sp, served(Env{Open: open})))
}

// fixtureFont is a font the picture package accepts.
const fixtureFont = "../../testdata/fixtures/fonts/DejaVuSansMono.ttf"

// Check reads a watermark's font from the assets directory.
func TestCheckWatermarkFont(t *testing.T) {
	font, err := os.ReadFile(fixtureFont)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mono.ttf"), font, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fake.ttf"), onePixelPNG, 0o600))
	env := served(Env{AssetsDir: dir})
	fakeChild(t, &env)
	with := func(name string) spec.Stream {
		sp := textSpec("hello")
		sp.Transcode.Watermark.Font = name
		require.NoError(t, sp.Validate())
		return sp
	}
	require.NoError(t, Check(with("mono.ttf"), env))
	opt, err := ladderOf(with("mono.ttf"), env)
	require.NoError(t, err)
	assert.Equal(t, font, opt.Watermark.Font)

	// The causes of a font or an image that cannot be read name the asset,
	// never the directory, and say "picture:" once.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "logo.png"), onePixelPNG, 0o600))
	for _, link := range []string{"link.ttf", "link.png"} {
		target := "mono.ttf"
		if filepath.Ext(link) == ".png" {
			target = "logo.png"
		}
		require.NoError(t, os.Symlink(filepath.Join(dir, target), filepath.Join(dir, link)))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub.ttf"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub.png"), 0o700))
	image := func(name string) spec.Stream {
		return withWatermark(textSpec("x"), &spec.Watermark{Image: name})
	}
	for _, row := range []struct {
		name, why string
		sp        spec.Stream
	}{
		{"gone.ttf", "no such file or directory", with("gone.ttf")},
		{"link.ttf", "is a symbolic link", with("link.ttf")},
		{"sub.ttf", "is a directory, not a regular file", with("sub.ttf")},
		{"gone.png", "no such file or directory", image("gone.png")},
		{"link.png", "is a symbolic link", image("link.png")},
		{"sub.png", "is a directory, not a regular file", image("sub.png")},
	} {
		err := Check(row.sp, env)
		require.Error(t, err, row.name)
		want := fmt.Sprintf("stream: check: transcode: watermark: picture: asset %q: %s", row.name, row.why)
		assert.Equal(t, want, err.Error(), row.name)
		assert.NotContains(t, err.Error(), dir, row.name)
		if strings.HasPrefix(row.name, "gone.") {
			assert.ErrorIs(t, err, fs.ErrNotExist, row.name)
		}
	}
	require.NoError(t, Check(image("logo.png"), env), "the image itself is fine")

	noDir := env
	noDir.AssetsDir = ""
	for name, row := range map[string]struct {
		sp   spec.Stream
		env  Env
		want string
	}{
		"missing":          {with("gone.ttf"), env, `gone\.ttf`},
		"not a font":       {with("fake.ttf"), env, `font`},
		"no assets dir":    {with("mono.ttf"), noDir, `^stream: check: transcode: watermark: a font needs the assets directory, which is not configured$`},
		"image, no dir":    {withWatermark(textSpec("x"), &spec.Watermark{Image: "logo.png"}), noDir, `^stream: check: transcode: watermark: an image needs the assets directory, which is not configured$`},
		"unknown position": {withWatermark(textSpec("x"), &spec.Watermark{Text: "x", Position: "zz"}), env, `^stream: check: transcode: watermark: unknown position "zz"$`},
	} {
		err := Check(row.sp, row.env)
		require.Error(t, err, name)
		assert.Regexp(t, `^stream: check: transcode: watermark: `, err.Error(), name)
		assert.Regexp(t, row.want, err.Error(), name)
	}
}

// NoFallback reaches the transcoder's options.
func TestLadderNoFallback(t *testing.T) {
	env := served(Env{})
	fakeChild(t, &env)
	sp := ladderSpec("720", "360")
	sp.Transcode.Renditions[1].Encoder = "nvenc"
	sp.Transcode.Renditions[1].NoFallback = true
	require.NoError(t, sp.Validate())
	require.NoError(t, Check(sp, env))
	opt, err := ladderOf(sp, env)
	require.NoError(t, err)
	assert.False(t, opt.Renditions[0].Video.NoFallback)
	assert.True(t, opt.Renditions[1].Video.NoFallback)
}

// Check over TLS: an output needs either of its two listeners; an rtmps
// listen input must be on the server's RTMPS listener, by its listener key,
// and keeps out of the play app; a push may be rtmps:// or
// rtmps+insecure://, never rtmp+insecure://. No text holds a key.
func TestCheckTLS(t *testing.T) {
	const tlsPlay = "127.0.0.1:1936"
	type edit = func(*spec.Stream, *Env)
	in := func(u string) edit {
		return func(sp *spec.Stream, _ *Env) { sp.Inputs = []spec.Input{{URL: u}} }
	}
	rtmps := func(addr string) edit { return func(_ *spec.Stream, env *Env) { env.RTMPSPlay = addr } }
	rtmpOffKeepApp := func(_ *spec.Stream, env *Env) { env.RTMPPlay = "" }
	push := func(u string) edit {
		return func(sp *spec.Stream, _ *Env) { sp.Outputs.Push = []spec.Push{{URL: u}} }
	}
	listen := func(host, path string) string { return "rtmps://" + host + "/" + path + "?mode=listen" }
	rows := []struct {
		name string
		edit []edit
		want string // "" passes; else a part of the error
	}{
		{"rtmp on RTMPS alone", []edit{rtmps(tlsPlay), rtmpOffKeepApp}, ""},
		{"rtmp on, both RTMP listeners off", []edit{rtmpOffKeepApp}, "outputs.rtmp: the server's rtmp listener is off: set server.rtmp.listen or server.rtmp.tls_listen and restart, or turn the output off"},
		{"rtsp on RTSPS alone", []edit{func(_ *spec.Stream, env *Env) { env.RTSPPlay, env.RTSPSPlay = "", "127.0.0.1:3220" }}, ""},
		{"rtsp on, both RTSP listeners off", []edit{func(_ *spec.Stream, env *Env) { env.RTSPPlay = "" }}, "outputs.rtsp: the server's rtsp listener is off: set server.rtsp.listen or server.rtsp.tls_listen and restart, or turn the output off"},

		{"an rtmps listen input on the RTMPS listener", []edit{rtmps(tlsPlay), in(listen(tlsPlay, "pub/"+vKey))}, ""},
		{"0.0.0.0 against a wildcard bind", []edit{rtmps(":1936"), in(listen("0.0.0.0:1936", "pub/"+vKey))}, ""},
		{"[::] against a wildcard bind", []edit{rtmps(":1936"), in(listen("[::]:1936", "pub/"+vKey))}, ""},
		{"no host against a wildcard bind", []edit{rtmps(":1936"), in(listen(":1936", "pub/"+vKey))}, ""},
		{"no host against a 0.0.0.0 bind", []edit{rtmps("0.0.0.0:1936"), in(listen(":1936", "pub/"+vKey))}, ""},
		{"a host against a wildcard bind", []edit{rtmps(":1936"), in(listen(tlsPlay, "pub/"+vKey))}, "inputs[0]: an rtmps listen input must be on the server's RTMPS listener :1936"},
		{"the RTMPS listener off", []edit{in(listen(tlsPlay, "pub/"+vKey))}, "inputs[0]: the server's RTMPS listener is off: set server.rtmp.tls_listen and server.tls, and restart"},
		{"another port", []edit{rtmps(tlsPlay), in(listen("127.0.0.1:1937", "pub/"+vKey))}, "inputs[0]: an rtmps listen input must be on the server's RTMPS listener 127.0.0.1:1936"},
		{"the default port, 443", []edit{rtmps(tlsPlay), in(listen("127.0.0.1", "pub/"+vKey))}, "inputs[0]: an rtmps listen input must be on the server's RTMPS listener 127.0.0.1:1936"},
		{"the plain RTMP address", []edit{rtmps(tlsPlay), in(listen("192.0.2.1:19350", "pub/"+vKey))}, "inputs[0]: an rtmps listen input must be on the server's RTMPS listener 127.0.0.1:1936"},
		{"a second input", []edit{rtmps(tlsPlay), func(sp *spec.Stream, _ *Env) {
			sp.Inputs = append(sp.Inputs, spec.Input{URL: listen("127.0.0.1:1937", "pub/"+vKey)})
		}}, "inputs[1]: an rtmps listen input must be on the server's RTMPS listener"},
		{"under the play app", []edit{rtmps(tlsPlay), in(listen(tlsPlay, "play/"+vKey))}, "may not publish under the play app \"play\""},
		{"under the play app, RTMPS alone", []edit{rtmps(tlsPlay), rtmpOffKeepApp, in(listen(tlsPlay, "play/"+vKey))}, "may not publish under the play app \"play\""},
		{"under the play app, listener spelling", []edit{rtmps(tlsPlay), in("rtmps://" + tlsPlay + "/play/" + vKey + "?mode=listener")}, "may not publish under the play app"},
		{"under the play app, upper-case scheme", []edit{rtmps(tlsPlay), in("RTMPS://" + tlsPlay + "/play/" + vKey + "?mode=listen")}, "may not publish under the play app"},
		{"under another app", []edit{rtmps(tlsPlay), rtmpOffKeepApp, in(listen(tlsPlay, "in/"+vKey))}, ""},
		{"an rtmps+insecure listen input", []edit{rtmps(tlsPlay), in("rtmps+insecure://" + tlsPlay + "/pub/" + vKey + "?mode=listen")}, "inputs[0]"},
		{"an rtmps pull, the RTMPS listener off", []edit{in("rtmps://u:" + vPass + "@h/live/" + vKey)}, ""},
		{"an rtmps+insecure pull", []edit{in("rtmps+insecure://h/live/" + vKey)}, ""},
		{"an rtmp listen input on the RTMPS address", []edit{rtmps(tlsPlay), in("rtmp://" + tlsPlay + "/pub/" + vKey + "?mode=listen")}, ""}, // a claim conflict, not Check's

		{"an rtmps push", []edit{push("rtmps://h/live/" + vKey)}, ""},
		{"an rtmps+insecure push", []edit{push("rtmps+insecure://h/live/" + vKey)}, ""},
		{"an rtmp+insecure push", []edit{push("rtmp+insecure://h/live/" + vKey)}, "outputs.push[0]: "},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			sp, env := camSpec(), served(Env{})
			for _, e := range row.edit {
				e(&sp, &env)
			}
			require.NoError(t, sp.Validate(), "the row must pass Validate: Check runs after it")
			err := Check(sp, env)
			if row.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), row.want)
			assert.Regexp(t, `^stream: check: `, err.Error())
			assert.NotContains(t, err.Error(), vPass)
			assert.NotContains(t, err.Error(), vKey)
			assert.Error(t, errors.Unwrap(err), "the cause is kept")
		})
	}
}
