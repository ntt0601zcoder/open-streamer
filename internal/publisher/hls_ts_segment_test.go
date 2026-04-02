package publisher

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHlsMpegTSSegmentCut_emptyOrShort(t *testing.T) {
	t.Parallel()
	require.Equal(t, 0, hlsMpegTSSegmentCut(nil))
	require.Equal(t, 0, hlsMpegTSSegmentCut([]byte{0x47}))
	require.Equal(t, 0, hlsMpegTSSegmentCut(make([]byte, 188)))
}

func TestHlsMpegTSForceCutLen(t *testing.T) {
	t.Parallel()
	require.Equal(t, 0, hlsMpegTSForceCutLen(nil))
	require.Equal(t, 376, hlsMpegTSForceCutLen(make([]byte, 400)))
}
