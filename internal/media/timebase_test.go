package media

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRescale(t *testing.T) {
	assert.Equal(t, int64(48000), Rescale(90000, 48000))
	assert.Equal(t, int64(1), Rescale(1, 48000))
	assert.Equal(t, int64(-48000), Rescale(-90000, 48000))
	assert.Equal(t, int64(-1), Rescale(-1, 48000))
}

func TestSampleTime(t *testing.T) {
	assert.Equal(t, int64(1920), SampleTime(1024, 48000))
	assert.Equal(t, int64(2090), SampleTime(1024, 44100))
	assert.Equal(t, int64(2089796), SampleTime(1024*1000, 44100))
	assert.Equal(t, int64(90000), SampleTime(44100, 44100))
	assert.Equal(t, int64(-1920), SampleTime(-1024, 48000))
}

func TestDurationConversions(t *testing.T) {
	assert.Equal(t, time.Second, ToDuration(90000))
	assert.Equal(t, int64(30000), FromDuration(time.Second/3))
	assert.Equal(t, int64(-30000), FromDuration(-time.Second/3))
}
