package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// full is a valid document that uses every part of the spec.
func full() Stream {
	return Stream{
		Name:   "cam",
		Inputs: []Input{{URL: "rtsp://u:pw@h/a"}, {URL: "udp://:5000"}},
		Layout: []string{"video", "audio"},
		Transcode: &Transcode{
			Renditions: []Rendition{
				{Label: "src", Copy: true},
				{Label: "720", Height: 720, BitrateKbps: 3000, MaxBitrateKbps: 3600, BFrames: 2, Preset: "veryfast", Encoder: "x264"},
				{Label: "360", Height: 360, BitrateKbps: 800, Encoder: "nvenc"},
			},
			Audio:     Audio{BitrateKbps: 128, SampleRate: 48000, Channels: 2},
			Watermark: &Watermark{Text: "%{localtime}", Position: "br", OpacityPct: 80, SizePct: 5, TimeZone: "Asia/Ho_Chi_Minh"},
		},
		Thumbnail: &Thumbnail{IntervalSec: 10, Height: 180, Quality: 75},
		Outputs: Outputs{
			HLS: true, DASH: true, HTTPTS: true, RTMP: true, RTSP: true, SRT: true,
			Push: []Push{{URL: "rtmp://a/live/k1"}, {URL: "srt://b:9000?streamid=x"}},
		},
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"cam", "a", "cam-1", "con1", "null", strings.Repeat("a", 63)} {
		assert.True(t, ValidName(s), s)
	}
	for _, s := range []string{
		"", "Cam", "cam_1", "-cam", "cam.1", "..", "a/b", "a%2f", "cam ", strings.Repeat("a", 64),
		"con", "prn", "aux", "nul", "com1", "com9", "lpt1", "lpt9",
	} {
		assert.False(t, ValidName(s), s)
	}
}

func TestDecodeStrict(t *testing.T) {
	const minimal = `{"name":"cam","inputs":[{"url":"udp://:5000"}],"outputs":{"hls":true}}`
	s, err := Decode(strings.NewReader(minimal))
	require.NoError(t, err)
	assert.Equal(t, Stream{Name: "cam", Inputs: []Input{{URL: "udp://:5000"}}, Outputs: Outputs{HLS: true}}, s)

	_, err = Decode(strings.NewReader(minimal + "\n\t "))
	require.NoError(t, err, "trailing white space is not data")

	_, err = Decode(strings.NewReader(`{"name":"cam","inputs":[],"outputs":{},"extra":1}`))
	require.Error(t, err, "an unknown key")
	_, err = Decode(strings.NewReader(`{"name":"cam","inputs":[{"url":"x","user":"u"}],"outputs":{}}`))
	require.Error(t, err, "an unknown nested key")
	_, err = Decode(strings.NewReader(minimal + minimal))
	require.Error(t, err, "a second document")
	_, err = Decode(strings.NewReader(minimal + " x"))
	require.Error(t, err, "trailing garbage")

	big := []byte(minimal)
	big = append(big, bytes.Repeat([]byte(" "), MaxSpecBytes+1-len(big))...)
	require.Len(t, big, MaxSpecBytes+1)
	_, err = Decode(bytes.NewReader(big))
	require.Error(t, err, "a body of MaxSpecBytes+1")
	_, err = Decode(bytes.NewReader(big[:MaxSpecBytes]))
	require.NoError(t, err, "a body of MaxSpecBytes")
}

func TestRedacted(t *testing.T) {
	s := full()
	s.Inputs[0].URL = "rtsp://u:hunter2@h/a"
	s.Outputs.Push[0].URL = "rtmp://h/live/secretkey"
	before := full()
	before.Inputs[0].URL = s.Inputs[0].URL
	before.Outputs.Push[0].URL = s.Outputs.Push[0].URL

	r := s.Redacted()
	assert.NotContains(t, r.Inputs[0].URL, "hunter2")
	assert.NotContains(t, r.Outputs.Push[0].URL, "secretkey")
	assert.Equal(t, "udp://:5000", r.Inputs[1].URL)
	assert.Equal(t, "srt://b:9000?streamid=x", r.Outputs.Push[1].URL)
	assert.Equal(t, before, s, "the receiver is unchanged")

	// A deep copy: writing through the result does not reach the receiver.
	r.Layout[0] = "audio"
	r.Transcode.Renditions[1].Height = 1080
	r.Transcode.Watermark.Text = "x"
	r.Thumbnail.Height = 1
	assert.Equal(t, before, s)

	var zero Stream
	assert.Equal(t, zero, zero.Redacted())
}

// A URL that redact.URL cannot parse is shown as the bare mask, which says
// a value is stored (R-T8); redact.URL itself keeps "<invalid url>", so
// logs and error texts do.
func TestRedactedUnparseable(t *testing.T) {
	s := Stream{
		Name:    "cam",
		Inputs:  []Input{{URL: "rtsp://admin:p%ss@cam/s"}, {URL: "rtsp://u:pw@h:port/s"}, {URL: "udp://:5000"}},
		Outputs: Outputs{HLS: true, Push: []Push{{URL: "rtmp://h:port/live/k"}}},
	}
	for _, u := range []string{s.Inputs[0].URL, s.Inputs[1].URL, s.Outputs.Push[0].URL} {
		assert.Equal(t, "<invalid url>", redact.URL(u), u)
	}

	r := s.Redacted()
	assert.Equal(t, []Input{{URL: "xxxxx"}, {URL: "xxxxx"}, {URL: "udp://:5000"}}, r.Inputs)
	assert.Equal(t, []Push{{URL: "xxxxx"}}, r.Outputs.Push)
	b, err := json.Marshal(r)
	require.NoError(t, err)
	for _, secret := range []string{"p%ss", "pw", "admin", "port", "/live/k", "invalid"} {
		assert.NotContains(t, string(b), secret)
	}
	assert.Equal(t, "rtsp://admin:p%ss@cam/s", s.Inputs[0].URL, "the receiver is unchanged")

	// What is shown is refused when it is written back.
	require.ErrorContains(t, r.Validate(), "inputs[0].url: holds the redaction mask")
}

// A stored passphrase is shown as the mask, and none as nothing.
func TestRedactedPassphrase(t *testing.T) {
	s := full()
	s.Outputs.SRTPassphrase = "Zq7-Vb2_Kx9.Wm4~"
	r := s.Redacted()
	assert.Equal(t, "xxxxx", r.Outputs.SRTPassphrase)
	assert.Equal(t, "Zq7-Vb2_Kx9.Wm4~", s.Outputs.SRTPassphrase, "the receiver is unchanged")
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"srt_passphrase":"xxxxx"`)
	for n := 4; n <= len("Zq7-Vb2_Kx9.Wm4~"); n++ {
		for i := 0; i+n <= len("Zq7-Vb2_Kx9.Wm4~"); i++ {
			assert.NotContains(t, string(b), "Zq7-Vb2_Kx9.Wm4~"[i:i+n])
		}
	}

	r = full().Redacted()
	assert.Empty(t, r.Outputs.SRTPassphrase)
	b, err = json.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "srt_passphrase")
}

// The deep copy behind Redacted and ETag carries the M5b1 fields, and the
// tag follows them.
func TestCloneCopiesNewFields(t *testing.T) {
	s := full()
	s.Transcode.Renditions[2].NoFallback = true
	s.Transcode.Watermark.Font = "noto-sans.ttf"

	r := s.Redacted()
	assert.True(t, r.Transcode.Renditions[2].NoFallback)
	assert.Equal(t, "noto-sans.ttf", r.Transcode.Watermark.Font)
	r.Transcode.Watermark.Font = "other.otf"
	assert.Equal(t, "noto-sans.ttf", s.Transcode.Watermark.Font, "the receiver is unchanged")

	b, err := json.Marshal(s.normal())
	require.NoError(t, err)
	assert.Contains(t, string(b), `"no_fallback":true`)
	assert.Contains(t, string(b), `"font":"noto-sans.ttf"`)

	e := s.ETag()
	assert.Equal(t, e, s.clone().ETag())
	noFallback := s.clone()
	noFallback.Transcode.Renditions[2].NoFallback = false
	assert.NotEqual(t, e, noFallback.ETag(), "no_fallback")
	assert.NotEqual(t, s.Redacted().ETag(), noFallback.Redacted().ETag(), "no_fallback, redacted")
	noFont := s.clone()
	noFont.Transcode.Watermark.Font = ""
	assert.NotEqual(t, e, noFont.ETag(), "font")
	assert.NotEqual(t, s.Redacted().ETag(), noFont.Redacted().ETag(), "font, redacted")
}

func TestAssets(t *testing.T) {
	s := full()
	s.Transcode = nil
	assert.Nil(t, s.Assets(), "no ladder")

	s = full()
	s.Transcode.Watermark = nil
	assert.Nil(t, s.Assets(), "no watermark")

	s = full()
	assert.Nil(t, s.Assets(), "text with the embedded font")

	s.Transcode.Watermark = &Watermark{Image: "logo.png"}
	assert.Equal(t, []string{"logo.png"}, s.Assets(), "image only")

	s.Transcode.Watermark = &Watermark{Text: "on air", Font: "noto-sans.ttf"}
	assert.Equal(t, []string{"noto-sans.ttf"}, s.Assets(), "text and font")

	// Not valid, but Assets still answers: sorted, without repeats.
	s.Transcode.Watermark = &Watermark{Image: "z.png", Font: "a.ttf"}
	assert.Equal(t, []string{"a.ttf", "z.png"}, s.Assets())
	s.Transcode.Watermark = &Watermark{Image: "x", Font: "x"}
	assert.Equal(t, []string{"x"}, s.Assets())
}

func TestETag(t *testing.T) {
	a, b := full(), full()
	a.Outputs.Push, b.Outputs.Push = nil, []Push{}
	assert.Equal(t, a.ETag(), b.ETag(), "nil and empty Push")
	a.Layout, b.Layout = nil, []string{}
	assert.Equal(t, a.ETag(), b.ETag(), "nil and empty Layout")

	e := full().ETag()
	assert.Regexp(t, `^"[0-9a-f]{16}"$`, e)
	assert.Equal(t, e, full().ETag(), "stable")
	for name, edit := range map[string]func(*Stream){
		"name":      func(s *Stream) { s.Name = "cam2" },
		"disabled":  func(s *Stream) { s.Disabled = true },
		"input":     func(s *Stream) { s.Inputs[1].URL = "udp://:5001" },
		"layout":    func(s *Stream) { s.Layout = []string{"video"} },
		"bitrate":   func(s *Stream) { s.Transcode.Renditions[1].BitrateKbps++ },
		"audio":     func(s *Stream) { s.Transcode.Audio.Channels = 1 },
		"watermark": func(s *Stream) { s.Transcode.Watermark.Position = "tl" },
		"font":      func(s *Stream) { s.Transcode.Watermark.Font = "noto-sans.ttf" },
		"fallback":  func(s *Stream) { s.Transcode.Renditions[2].NoFallback = true },
		"thumbnail": func(s *Stream) { s.Thumbnail.Quality = 70 },
		"output":    func(s *Stream) { s.Outputs.SRT = false },
		"push":      func(s *Stream) { s.Outputs.Push[1].URL = "srt://c:9000" },
	} {
		s := full()
		edit(&s)
		assert.NotEqual(t, e, s.ETag(), name)
	}
}

func TestServed(t *testing.T) {
	s := Stream{Name: "cam"}
	assert.Equal(t, []string{"cam"}, s.Served())
	s.Transcode = &Transcode{}
	assert.Equal(t, []string{"cam"}, s.Served(), "a transcode without renditions")
	s.Transcode.Renditions = []Rendition{{Label: "720"}, {Label: "360"}}
	assert.Equal(t, []string{"cam", "cam_720", "cam_360"}, s.Served())
}

func TestYAMLTagsEqualJSONTags(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			j, okJ := f.Tag.Lookup("json")
			y, okY := f.Tag.Lookup("yaml")
			assert.True(t, okJ, "%s.%s has no json tag", typ.Name(), f.Name)
			assert.True(t, okY, "%s.%s has no yaml tag", typ.Name(), f.Name)
			assert.Equal(t, j, y, "%s.%s", typ.Name(), f.Name)
			walk(f.Type)
		}
	}
	walk(reflect.TypeFor[Stream]())
	for _, typ := range []reflect.Type{
		reflect.TypeFor[Stream](), reflect.TypeFor[Input](), reflect.TypeFor[Transcode](),
		reflect.TypeFor[Rendition](), reflect.TypeFor[Audio](), reflect.TypeFor[Watermark](),
		reflect.TypeFor[Thumbnail](), reflect.TypeFor[Outputs](), reflect.TypeFor[Push](),
	} {
		assert.True(t, seen[typ], "%s was not walked", typ.Name())
	}
}

// TestETagIsKeyedAndFollowsDiff: the tag is a pure function of the
// document within the process, equal exactly when Diff finds no change, and
// not the plain hash of the document, which a reader of the redacted
// document could test guesses of its secrets against.
func TestETagIsKeyedAndFollowsDiff(t *testing.T) {
	base := full()
	variants := map[string]Stream{"the same": full()}
	empty := full()
	empty.Layout, empty.Outputs.Push = []string{}, []Push{}
	none := full()
	none.Layout, none.Outputs.Push = nil, nil
	variants["empty slices"], variants["nil slices"] = empty, none
	reordered := full()
	slices.Reverse(reordered.Outputs.Push)
	variants["pushes reordered"] = reordered
	secret := full()
	secret.Inputs[0].URL = "rtsp://u:pw2@h/a"
	variants["another password"] = secret
	off := full()
	off.Outputs.HLS = !off.Outputs.HLS
	variants["an output"] = off

	for name, a := range variants {
		for other, b := range variants {
			same := Diff(a, b) == ChangeNone
			assert.Equal(t, same, a.ETag() == b.ETag(), "%s and %s", name, other)
		}
		assert.Equal(t, a.ETag(), a.ETag(), name)
	}
	assert.NotEqual(t, base.ETag(), secret.ETag(), "the secret is part of the document")

	b, err := json.Marshal(base.normal())
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	assert.NotEqual(t, `"`+hex.EncodeToString(sum[:8])+`"`, base.ETag(), "unkeyed")
	assert.Equal(t, `"`+hex.EncodeToString(redact.Tag(b)[:8])+`"`, base.ETag())
}
