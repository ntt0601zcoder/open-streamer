package spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiff(t *testing.T) {
	assert.Equal(t, ChangeNone, Diff(full(), full()), "equal")
	assert.Equal(t, ChangeNone, Diff(Stream{}, Stream{}), "zero")

	empty := full()
	empty.Layout, empty.Outputs.Push = []string{}, []Push{}
	none := full()
	none.Layout, none.Outputs.Push = nil, nil
	assert.Equal(t, ChangeNone, Diff(empty, none), "nil and empty slices are equal")
	assert.Equal(t, ChangeNone, Diff(none, empty))

	for _, c := range []struct {
		name string
		want Change
		edit func(*Stream)
	}{
		{"only outputs.rtsp", ChangeOutputs, func(s *Stream) { s.Outputs.RTSP = false }},
		{"only hls", ChangeOutputs, func(s *Stream) { s.Outputs.HLS = false }},
		{"one push added", ChangeOutputs, func(s *Stream) { s.Outputs.Push = append(s.Outputs.Push, Push{URL: "rtmp://c/live/k"}) }},
		{"pushes reordered", ChangeOutputs, func(s *Stream) {
			p := s.Outputs.Push
			p[0], p[1] = p[1], p[0]
		}},
		{"an input added", ChangeRebuild, func(s *Stream) { s.Inputs = append(s.Inputs, Input{URL: "udp://:6000"}) }},
		{"a rung's bitrate", ChangeRebuild, func(s *Stream) { s.Transcode.Renditions[1].BitrateKbps = 2500 }},
		{"layout", ChangeRebuild, func(s *Stream) { s.Layout = []string{"video"} }},
		{"thumbnail", ChangeRebuild, func(s *Stream) { s.Thumbnail.IntervalSec = 5 }},
		{"thumbnail removed", ChangeRebuild, func(s *Stream) { s.Thumbnail = nil }},
		{"watermark text", ChangeRebuild, func(s *Stream) { s.Transcode.Watermark.Text = "live" }},
		{"disabled", ChangeRebuild, func(s *Stream) { s.Disabled = true }},
		{"name", ChangeRebuild, func(s *Stream) { s.Name = "cam2" }},
		{"outputs together with an input", ChangeRebuild, func(s *Stream) {
			s.Outputs.RTSP = false
			s.Inputs[0].URL = "udp://:7000"
		}},
	} {
		old, edited := full(), full()
		c.edit(&edited)
		assert.Equal(t, c.want, Diff(old, edited), c.name)
		assert.Equal(t, c.want, Diff(edited, old), c.name+", reversed")
	}

	// Diff does not change its arguments.
	a, b := full(), full()
	a.Layout = []string{}
	Diff(a, b)
	assert.NotNil(t, a.Layout)
}
