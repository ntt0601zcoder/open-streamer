package wire

import (
	"bytes"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

func FuzzRead(f *testing.F) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	cfg, err := media.NewAACConfig([]byte{0x11, 0x90})
	if err != nil {
		f.Fatal(err)
	}
	for _, m := range []Message{
		Hello{Version: Version, PID: 7},
		Configure{Rungs: []Rung{{Height: 720, BitrateKbps: 2500, BFrames: 2, Preset: "fast"}}, Audio: &AudioEncode{128, 48000, 2}},
		Frame{Rung: -1, F: media.Frame{Config: cfg, PTS: 5, DTS: 5, Key: true, Data: [][]byte{{1, 2, 3}}}},
		Discontinuity{},
		Stats{In: 3, Out: []uint64{1}, EncodeNanos: []int64{9}},
		Error{Rung: 1, Message: "x", Fatal: true},
		Stop{},
	} {
		if err := w.Write(m); err != nil {
			f.Fatal(err)
		}
	}
	f.Add(buf.Bytes())
	f.Add([]byte{0, 0, 0, 2, 0x22, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data))
		for range 64 {
			if _, err := r.Read(); err != nil {
				return
			}
		}
	})
}
