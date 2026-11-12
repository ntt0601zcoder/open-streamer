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

func TestLongTimestampsDoNotOverflow(t *testing.T) {
	const day = 24 * 3600
	ticks := int64(30 * day * ClockRate) // 30 days
	d := 30 * 24 * time.Hour
	assert.Equal(t, d, ToDuration(ticks))
	assert.Equal(t, -d, ToDuration(-ticks))
	assert.Equal(t, ticks, FromDuration(d))
	assert.Equal(t, -ticks, FromDuration(-d))
	// Rounding is preserved: 1 tick = 11111.11 ns, 2 ticks = 22222.22 ns.
	assert.Equal(t, d+11111, ToDuration(ticks+1))
	assert.Equal(t, -(d + 22222), ToDuration(-(ticks + 2)))
	assert.Equal(t, ticks+30000, FromDuration(d+time.Second/3))
	assert.Equal(t, -(ticks + 30000), FromDuration(-(d + time.Second/3)))
	assert.Equal(t, int64(30*day*48000), Rescale(ticks, 48000))
	assert.Equal(t, int64(30*day*ClockRate), SampleTime(30*day*44100, 44100))
}
