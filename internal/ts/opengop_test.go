package ts_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// openGOPKeyPTS are the recovery-point PTS values of h264_opengop.ts (see
// testdata/fixtures/README.md); the first key is an IDR.
var openGOPKeyPTS = []int64{0, 90000, 180000, 270000, 360000, 450000}

// openGOPLeadingPTS are the PTS of the leading B frames, in decode order.
var openGOPLeadingPTS = []int64{86400, 172800, 176400, 262800, 266400}

func demuxFixture(t *testing.T, name string) (*ts.Demuxer, []media.Frame) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	var frames []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return d, frames
		}
		require.NoError(t, err)
		frames = append(frames, f)
	}
}

func TestOpenGOPKeys(t *testing.T) {
	_, frames := demuxFixture(t, "h264_opengop.ts")
	var keys []int64
	var keyPTS int64
	var leading []int64
	for _, f := range frames {
		if f.Kind() != media.KindVideo {
			continue
		}
		if f.Key {
			keys = append(keys, f.PTS)
			keyPTS = f.PTS
			assert.False(t, f.RASL)
			continue
		}
		want := f.PTS < keyPTS
		assert.Equal(t, want, f.RASL, "pts %d after key %d", f.PTS, keyPTS)
		if f.RASL {
			leading = append(leading, f.PTS)
		}
	}
	assert.Equal(t, openGOPKeyPTS, keys)
	assert.Equal(t, openGOPLeadingPTS, leading)
}

func TestOpenGOPMidStart(t *testing.T) {
	d, frames := demuxFixture(t, "h264_opengop_midgop.ts")
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	out := make([]media.Frame, 0, len(frames))
	for _, f := range frames {
		out = append(out, c.Push(f)...)
	}
	out = append(out, c.Flush()...)
	require.NotEmpty(t, out)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))

	var first *media.Frame
	for i := range out {
		if out[i].Kind() == media.KindVideo {
			first = &out[i]
			break
		}
	}
	require.NotNil(t, first)
	assert.True(t, first.Key)
	// Locate the released frame in the source to read its source PTS.
	var srcPTS int64 = -1
	for _, f := range frames {
		if f.Kind() == media.KindVideo && f.Key && bytes.Equal(f.Data[len(f.Data)-1], first.Data[len(first.Data)-1]) {
			srcPTS = f.PTS
			break
		}
	}
	assert.Equal(t, midGOPKeyPTS, srcPTS)
	assert.Equal(t, uint64(2), c.Stats().DroppedRASL, "the two leading B frames after the cut")
}

// midGOPKeyPTS is the first recovery point after the cut in
// h264_opengop_midgop.ts (see testdata/fixtures/README.md).
const midGOPKeyPTS int64 = 57600
