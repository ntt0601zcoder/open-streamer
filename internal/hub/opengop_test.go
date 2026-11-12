package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// avcOpenGOP builds an H.264 frame flagged as the NAL classifier would flag
// an open-GOP feed: Key on a recovery-point I frame, RASL on leading pictures.
func avcOpenGOP(dts, pts int64, key, rasl bool) media.Frame {
	return media.Frame{Track: 0, Config: h264, DTS: dts, PTS: pts, Key: key, RASL: rasl}
}

func TestOpenGOPHubResume(t *testing.T) {
	h := New(Options{})
	s := h.Subscribe("s", 1)

	h.Write(avcOpenGOP(0, 3*vDur, true, false))
	h.Write(avcOpenGOP(vDur, vDur, false, false)) // queue full: skip mode
	recv(t, s)
	h.Write(avcOpenGOP(2*vDur, 2*vDur, false, false)) // dropped: before the key

	// Recovery-point key with PTS above two leading B frames.
	h.Write(avcOpenGOP(10*vDur, 13*vDur, true, false))
	d := recv(t, s)
	assert.Equal(t, int64(10*vDur), d.Frame.DTS)
	assert.Equal(t, uint64(2), d.Lost)

	h.Write(avcOpenGOP(11*vDur, 11*vDur, false, true)) // RASL: dropped
	h.Write(avcOpenGOP(12*vDur, 12*vDur, false, true)) // RASL: dropped
	requireEmpty(t, s)
	h.Write(avcOpenGOP(13*vDur, 16*vDur, false, false))
	d = recv(t, s)
	assert.Equal(t, int64(13*vDur), d.Frame.DTS)
	assert.Zero(t, d.Lost)
}
