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

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// These checks live in the external test package: conform imports ts for
// its readers, so ts's internal tests cannot import conform.

// TestDemuxConforms runs the frame checks on raw demuxer output. It skips
// conform.RuleConfigChange: the demuxer emits source-time frames with Epoch
// 0, so a config change (as in h264_aac_then_640.ts) is never at an epoch
// start until the Clock assigns epochs.
func TestDemuxConforms(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  int // bytes kept, 0 for all
	}{
		{"h264_aac.ts", 0},
		{"h265_aac51.ts", 0},
		{"h264_aac.ts", 100077}, // truncated mid-packet
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", tc.name))
		require.NoError(t, err)
		if tc.cut > 0 {
			data = data[:tc.cut]
		}
		d, err := ts.NewDemuxer(bytes.NewReader(data))
		require.NoError(t, err)
		var frames []media.Frame
		for {
			f, err := d.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			frames = append(frames, f)
		}
		require.NotEmpty(t, frames)
		assert.Nil(t, conform.CheckFrames(frames, conform.FrameOptions{Skip: []string{conform.RuleConfigChange}}), "%s cut %d", tc.name, tc.cut)
	}
}
