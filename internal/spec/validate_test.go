package spec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	require.NoError(t, full().Validate(), "the full document")
	minimal := Stream{Name: "cam", Inputs: []Input{{URL: "udp://:5000"}}, Outputs: Outputs{HLS: true}}
	require.NoError(t, minimal.Validate(), "the minimal document")

	long := "udp://h/" + strings.Repeat("a", MaxURLBytes-len("udp://h/"))
	rung := func(label string) Rendition {
		return Rendition{Label: label, Height: 360, BitrateKbps: 800}
	}
	nRungs := func(n int) []Rendition {
		r := make([]Rendition, n)
		for i := range r {
			r[i] = rung("r" + string(rune('a'+i)))
		}
		return r
	}
	nInputs := func(n int) []Input {
		in := make([]Input, n)
		for i := range in {
			in[i] = Input{URL: "udp://:" + string(rune('0'+i))}
		}
		return in
	}
	nPushes := func(n int) []Push {
		p := make([]Push, n)
		for i := range p {
			p[i] = Push{URL: "rtmp://h/live/k" + string(rune('a'+i))}
		}
		return p
	}

	ok := map[string]func(*Stream){
		"8 inputs":          func(s *Stream) { s.Inputs = nInputs(MaxInputs) },
		"input of 2048":     func(s *Stream) { s.Inputs[0].URL = long },
		"no layout":         func(s *Stream) { s.Layout = nil },
		"layout audio":      func(s *Stream) { s.Layout = []string{"audio"} },
		"8 renditions":      func(s *Stream) { s.Transcode.Renditions = nRungs(MaxRenditions) },
		"height 2":          func(s *Stream) { s.Transcode.Renditions[1].Height = 2 },
		"height 4320":       func(s *Stream) { s.Transcode.Renditions[1].Height = 4320 },
		"default encoder":   func(s *Stream) { s.Transcode.Renditions[1].Encoder = "" },
		"no watermark":      func(s *Stream) { s.Transcode.Watermark = nil },
		"image watermark":   func(s *Stream) { s.Transcode.Watermark = &Watermark{Image: "logo_v-2.png"} },
		"jpeg watermark":    func(s *Stream) { s.Transcode.Watermark = &Watermark{Image: "a.jpeg", Position: "c"} },
		"default position":  func(s *Stream) { s.Transcode.Watermark.Position = "" },
		"no transcode":      func(s *Stream) { s.Transcode = nil },
		"8 pushes":          func(s *Stream) { s.Outputs.Push = nPushes(MaxPushes) },
		"push of 2048":      func(s *Stream) { s.Outputs.Push[0].URL = long },
		"duplicate inputs":  func(s *Stream) { s.Inputs[1] = s.Inputs[0] },
		"unmasked password": func(s *Stream) { s.Inputs[0].URL = "rtsp://user:pw@host/a" },
	}
	for name, edit := range ok {
		s := full()
		edit(&s)
		assert.NoError(t, s.Validate(), name)
	}

	for _, c := range []struct {
		name, field string
		edit        func(*Stream)
	}{
		{"bad name", "name", func(s *Stream) { s.Name = "Cam" }},
		{"device name", "name", func(s *Stream) { s.Name = "con" }},
		{"no inputs", "inputs", func(s *Stream) { s.Inputs = nil }},
		{"9 inputs", "inputs", func(s *Stream) { s.Inputs = nInputs(MaxInputs + 1) }},
		{"empty input", "inputs[1].url", func(s *Stream) { s.Inputs[1].URL = "" }},
		{"long input", "inputs[0].url", func(s *Stream) { s.Inputs[0].URL = long + "a" }},
		{"masked input", "inputs[1].url", func(s *Stream) { s.Inputs[1].URL = "rtsp://user:xxxxx@host/a" }},
		{"bad layout", "layout[1]", func(s *Stream) { s.Layout = []string{"video", "data"} }},
		{"repeated layout", "layout[1]", func(s *Stream) { s.Layout = []string{"audio", "audio"} }},
		{"no renditions", "transcode.renditions", func(s *Stream) { s.Transcode.Renditions = nil }},
		{"9 renditions", "transcode.renditions", func(s *Stream) { s.Transcode.Renditions = nRungs(MaxRenditions + 1) }},
		{"bad label", "transcode.renditions[2].label", func(s *Stream) { s.Transcode.Renditions[2].Label = "a_b" }},
		{"empty label", "transcode.renditions[0].label", func(s *Stream) { s.Transcode.Renditions[0].Label = "" }},
		{"device label", "transcode.renditions[0].label", func(s *Stream) { s.Transcode.Renditions[0].Label = "nul" }},
		{"repeated label", "transcode.renditions[2].label", func(s *Stream) { s.Transcode.Renditions[2].Label = "720" }},
		{"copy with height", "transcode.renditions[0]", func(s *Stream) { s.Transcode.Renditions[0].Height = 720 }},
		{"copy with bitrate", "transcode.renditions[0]", func(s *Stream) { s.Transcode.Renditions[0].BitrateKbps = 1 }},
		{"copy with max bitrate", "transcode.renditions[0]", func(s *Stream) { s.Transcode.Renditions[0].MaxBitrateKbps = 1 }},
		{"copy with b-frames", "transcode.renditions[0]", func(s *Stream) { s.Transcode.Renditions[0].BFrames = 1 }},
		{"copy with preset", "transcode.renditions[0]", func(s *Stream) { s.Transcode.Renditions[0].Preset = "fast" }},
		{"copy with encoder", "transcode.renditions[0]", func(s *Stream) { s.Transcode.Renditions[0].Encoder = "x264" }},
		{"odd height", "transcode.renditions[1].height", func(s *Stream) { s.Transcode.Renditions[1].Height = 719 }},
		{"no height", "transcode.renditions[1].height", func(s *Stream) { s.Transcode.Renditions[1].Height = 0 }},
		{"negative height", "transcode.renditions[1].height", func(s *Stream) { s.Transcode.Renditions[1].Height = -2 }},
		{"height 4322", "transcode.renditions[1].height", func(s *Stream) { s.Transcode.Renditions[1].Height = 4322 }},
		{"no bitrate", "transcode.renditions[2].bitrate_kbps", func(s *Stream) { s.Transcode.Renditions[2].BitrateKbps = 0 }},
		{"bad encoder", "transcode.renditions[2].encoder", func(s *Stream) { s.Transcode.Renditions[2].Encoder = "qsv" }},
		{"watermark both", "transcode.watermark", func(s *Stream) { s.Transcode.Watermark.Image = "logo.png" }},
		{"watermark neither", "transcode.watermark", func(s *Stream) { s.Transcode.Watermark = &Watermark{Position: "tl"} }},
		{"image path", "transcode.watermark.image", func(s *Stream) { s.Transcode.Watermark = &Watermark{Image: "../logo.png"} }},
		{"image type", "transcode.watermark.image", func(s *Stream) { s.Transcode.Watermark = &Watermark{Image: "logo.gif"} }},
		{"image case", "transcode.watermark.image", func(s *Stream) { s.Transcode.Watermark = &Watermark{Image: "Logo.png"} }},
		{"bad position", "transcode.watermark.position", func(s *Stream) { s.Transcode.Watermark.Position = "center" }},
		{"9 pushes", "outputs.push", func(s *Stream) { s.Outputs.Push = nPushes(MaxPushes + 1) }},
		{"empty push", "outputs.push[1].url", func(s *Stream) { s.Outputs.Push[1].URL = "" }},
		{"long push", "outputs.push[0].url", func(s *Stream) { s.Outputs.Push[0].URL = long + "a" }},
		{"repeated push", "outputs.push[1].url", func(s *Stream) { s.Outputs.Push[1] = s.Outputs.Push[0] }},
		{"masked push", "outputs.push[0].url", func(s *Stream) { s.Outputs.Push[0].URL = "rtmp://h/app/abxxxxx" }},
	} {
		s := full()
		c.edit(&s)
		err := s.Validate()
		if assert.Error(t, err, c.name) {
			assert.Contains(t, err.Error(), c.field+": ", c.name)
		}
	}
}

func TestValidateRejectsMaskedURL(t *testing.T) {
	s := full()
	s.Inputs[0].URL = "rtsp://user:xxxxx@host/a"
	err := s.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inputs[0].url: ")

	s = full()
	s.Outputs.Push = []Push{{URL: "rtmp://h/app/abxxxxx"}}
	err = s.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outputs.push[0].url: ")

	s = full()
	s.Inputs[0].URL = "rtsp://user:pw@host/a"
	require.NoError(t, s.Validate())
}

// A message quotes at most 64 bytes of a value, on a rune boundary.
func TestQuote(t *testing.T) {
	assert.Equal(t, `"cam"`, Quote("cam"))
	assert.Equal(t, `"a\nb"`, Quote("a\nb"))
	exact := strings.Repeat("x", 64)
	assert.Equal(t, `"`+exact+`"`, Quote(exact))
	assert.Equal(t, `"`+exact+`"…`, Quote(exact+"y"))
	assert.Equal(t, `"`+strings.Repeat("x", 63)+`"…`, Quote(strings.Repeat("x", 63)+"é!"), "no half rune")

	named := func(name string) Stream {
		return Stream{Name: name, Inputs: []Input{{URL: "udp://:5000"}}, Outputs: Outputs{HLS: true}}
	}
	long := named("cam")
	long.Name = strings.Repeat("x", 100_000)
	err := long.Validate()
	require.Error(t, err)
	assert.Less(t, len(err.Error()), 200)
	for _, edit := range []func(*Stream){
		func(s *Stream) { s.Layout = []string{long.Name} },
		func(s *Stream) { s.Transcode = &Transcode{Renditions: []Rendition{{Label: long.Name}}} },
		func(s *Stream) {
			s.Transcode = &Transcode{Renditions: []Rendition{{Label: "a", Height: 720, BitrateKbps: 800, Encoder: long.Name}}}
		},
		func(s *Stream) {
			s.Transcode = &Transcode{
				Renditions: []Rendition{{Label: "a", Height: 720, BitrateKbps: 800}},
				Watermark:  &Watermark{Image: long.Name},
			}
		},
		func(s *Stream) {
			s.Transcode = &Transcode{
				Renditions: []Rendition{{Label: "a", Height: 720, BitrateKbps: 800}},
				Watermark:  &Watermark{Text: "t", Position: long.Name},
			}
		},
	} {
		sp := named("cam")
		edit(&sp)
		err := sp.Validate()
		require.Error(t, err)
		assert.Less(t, len(err.Error()), 250, err.Error()[:100])
	}
}

func TestValidateNoFallback(t *testing.T) {
	encoded := func(encoder string) Stream {
		s := full()
		s.Transcode.Renditions = []Rendition{
			{Label: "720", Height: 720, BitrateKbps: 3000, Encoder: encoder, NoFallback: true},
		}
		return s
	}
	require.NoError(t, encoded("nvenc").Validate())
	for _, enc := range []string{"", "x264"} {
		err := encoded(enc).Validate()
		if assert.Error(t, err, enc) {
			assert.Contains(t, err.Error(), "transcode.renditions[0].no_fallback: needs encoder nvenc", enc)
		}
	}

	s := full()
	s.Transcode.Renditions[0].NoFallback = true
	err := s.Validate()
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "transcode.renditions[0]: a copy rendition sets nothing but its label")
	}
}

func TestValidateFont(t *testing.T) {
	text := func(font string) Stream {
		s := full()
		s.Transcode.Watermark = &Watermark{Text: "on air", Font: font}
		return s
	}
	for _, font := range []string{"noto-sans.ttf", "a_b-c.otf", strings.Repeat("a", 64) + ".ttf"} {
		assert.NoError(t, text(font).Validate(), font)
	}
	for _, font := range []string{
		"Noto.ttf", "x.ttc", "x.woff", "../x.ttf", "x.png", "_x.ttf", ".ttf", "x.TTF",
		strings.Repeat("a", 65) + ".ttf",
	} {
		err := text(font).Validate()
		if assert.Error(t, err, font) {
			assert.Contains(t, err.Error(), "transcode.watermark.font: "+Quote(font)+" is not a ttf or otf asset name", font)
		}
	}

	s := full()
	s.Transcode.Watermark = &Watermark{Image: "logo.png", Font: "noto-sans.ttf"}
	err := s.Validate()
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "transcode.watermark.font: needs text")
	}
}
