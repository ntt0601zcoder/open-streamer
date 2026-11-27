package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigCacheStable(t *testing.T) {
	var c ConfigCache
	a := &TrackConfig{Codec: CodecAAC, SampleRate: 48000, Channels: 2, ASC: []byte{0x11, 0x90}}
	a2 := &TrackConfig{Codec: CodecAAC, SampleRate: 48000, Channels: 2, ASC: []byte{0x11, 0x90}}
	b := &TrackConfig{Codec: CodecAAC, SampleRate: 44100, Channels: 2, ASC: []byte{0x12, 0x10}}

	assert.Same(t, a, c.Stable(1, a))
	assert.Same(t, a, c.Stable(1, a2), "equal config keeps the previous pointer")
	assert.Same(t, b, c.Stable(1, b), "different config is new")
	assert.Same(t, a2, c.Stable(0, a2), "another track has its own entry")

	assert.Nil(t, c.Stable(1, nil))
	assert.Same(t, b, c.Stable(1, &TrackConfig{Codec: CodecAAC, SampleRate: 44100, Channels: 2, ASC: []byte{0x12, 0x10}}), "nil does not clear")

	c.Reset(1)
	n := &TrackConfig{Codec: CodecAAC, SampleRate: 44100, Channels: 2, ASC: []byte{0x12, 0x10}}
	assert.Same(t, n, c.Stable(1, n), "Reset forgets the entry")
	c.Reset(-1)
	c.Reset(99)
	assert.Same(t, a, c.Stable(-1, a))
}
