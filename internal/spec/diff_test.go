package spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDiff edits one base document (a ladder, a text watermark, 2 inputs,
// 2 pushes) and checks the class of the edit both ways.
func TestDiff(t *testing.T) {
	assert.Equal(t, ChangeNone, Diff(full(), full()), "equal")
	assert.Equal(t, ChangeNone, Diff(Stream{}, Stream{}), "zero")

	empty := full()
	empty.Inputs, empty.Layout, empty.Outputs.Push = []Input{}, []string{}, []Push{}
	empty.Transcode.Renditions = []Rendition{}
	none := full()
	none.Inputs, none.Layout, none.Outputs.Push = nil, nil, nil
	none.Transcode.Renditions = nil
	assert.Equal(t, ChangeNone, Diff(empty, none), "nil and empty slices are equal")
	assert.Equal(t, ChangeNone, Diff(none, empty))

	ladder := func() *Transcode {
		return &Transcode{
			Renditions: []Rendition{{Label: "480", Height: 480, BitrateKbps: 1200}},
			Watermark:  &Watermark{Text: "on air"},
		}
	}

	for _, c := range []struct {
		name string
		want Change
		from func(*Stream) // the base's own edit, if any
		edit func(*Stream)
	}{
		{"layout nil vs video, audio", ChangeNone, nil, func(s *Stream) { s.Layout = nil }},
		{"layout audio, video", ChangeRebuild, nil, func(s *Stream) { s.Layout = []string{"audio", "video"} }},

		{"outputs.hls", ChangeOutputs, nil, func(s *Stream) { s.Outputs.HLS = false }},
		{"one push added", ChangeOutputs, nil, func(s *Stream) { s.Outputs.Push = append(s.Outputs.Push, Push{URL: "rtmp://c/live/k"}) }},
		{"a push reordered", ChangeOutputs, nil, func(s *Stream) {
			p := s.Outputs.Push
			p[0], p[1] = p[1], p[0]
		}},

		{"an input URL", ChangeLive, nil, func(s *Stream) { s.Inputs[1].URL = "udp://:5001" }},
		{"inputs reordered", ChangeLive, nil, func(s *Stream) {
			in := s.Inputs
			in[0], in[1] = in[1], in[0]
		}},
		{"an input added", ChangeLive, nil, func(s *Stream) { s.Inputs = append(s.Inputs, Input{URL: "udp://:6000"}) }},
		{"an input removed", ChangeLive, nil, func(s *Stream) { s.Inputs = s.Inputs[:1] }},
		{"watermark text", ChangeLive, nil, func(s *Stream) { s.Transcode.Watermark.Text = "live" }},
		{"watermark font", ChangeLive, nil, func(s *Stream) { s.Transcode.Watermark.Font = "noto-sans.ttf" }},
		{"watermark removed", ChangeLive, nil, func(s *Stream) { s.Transcode.Watermark = nil }},
		{"watermark added", ChangeLive, func(s *Stream) { s.Transcode.Watermark = nil }, func(s *Stream) {
			s.Transcode.Watermark = &Watermark{Image: "logo.png", Position: "tr"}
		}},
		{"inputs, watermark and outputs", ChangeLive, nil, func(s *Stream) {
			s.Inputs[0].URL = "udp://:7000"
			s.Transcode.Watermark.Position = "c"
			s.Outputs.RTSP = false
		}},

		{"inputs and layout", ChangeRebuild, nil, func(s *Stream) {
			s.Inputs[0].URL = "udp://:7000"
			s.Layout = []string{"video"}
		}},
		{"watermark and a rendition's bitrate", ChangeRebuild, nil, func(s *Stream) {
			s.Transcode.Watermark.Text = "live"
			s.Transcode.Renditions[1].BitrateKbps = 2500
		}},
		{"no_fallback", ChangeRebuild, nil, func(s *Stream) { s.Transcode.Renditions[2].NoFallback = true }},
		{"transcode removed", ChangeRebuild, nil, func(s *Stream) { s.Transcode = nil }},
		{"transcode added, with a watermark", ChangeRebuild, func(s *Stream) { s.Transcode = nil }, func(s *Stream) { s.Transcode = ladder() }},
		{"thumbnail", ChangeRebuild, nil, func(s *Stream) { s.Thumbnail.IntervalSec = 5 }},
		{"thumbnail removed", ChangeRebuild, nil, func(s *Stream) { s.Thumbnail = nil }},
		{"disabled", ChangeRebuild, nil, func(s *Stream) { s.Disabled = true }},
		{"name", ChangeRebuild, nil, func(s *Stream) { s.Name = "cam2" }},
		{"audio", ChangeRebuild, nil, func(s *Stream) { s.Transcode.Audio.Channels = 1 }},
	} {
		old := full()
		if c.from != nil {
			c.from(&old)
		}
		edited := old.clone()
		c.edit(&edited)
		assert.Equal(t, c.want, Diff(old, edited), c.name)
		assert.Equal(t, c.want, Diff(edited, old), c.name+", reversed")
	}

	// Diff does not change its arguments.
	a, b := full(), full()
	a.Layout = []string{}
	b.Inputs[0].URL = "udp://:7000"
	Diff(a, b)
	assert.NotNil(t, a.Layout)
	assert.NotNil(t, a.Transcode.Watermark)
	assert.NotNil(t, b.Transcode.Watermark)
	assert.Equal(t, "udp://:7000", b.Inputs[0].URL)
	assert.Equal(t, full().Inputs, a.Inputs)
}

// The classes are ordered by how much of the stream an edit touches.
func TestDiffOrder(t *testing.T) {
	assert.Less(t, ChangeNone, ChangeOutputs)
	assert.Less(t, ChangeOutputs, ChangeLive)
	assert.Less(t, ChangeLive, ChangeRebuild)
}
