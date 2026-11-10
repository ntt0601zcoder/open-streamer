package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodecKind(t *testing.T) {
	assert.Equal(t, KindVideo, CodecH264.Kind())
	assert.Equal(t, KindVideo, CodecH265.Kind())
	assert.Equal(t, KindAudio, CodecAAC.Kind())
	assert.Equal(t, "h264", CodecH264.String())
	assert.Equal(t, "h265", CodecH265.String())
	assert.Equal(t, "aac", CodecAAC.String())
}

func TestFrameKind(t *testing.T) {
	assert.Equal(t, KindVideo, Frame{Config: &TrackConfig{Codec: CodecH264}}.Kind())
	assert.Equal(t, KindAudio, Frame{Config: &TrackConfig{Codec: CodecAAC}}.Kind())
}
