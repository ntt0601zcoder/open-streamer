package mpegts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTimeDecoderStartsAtZero(t *testing.T) {
	var d TimeDecoder
	assert.Equal(t, int64(0), d.Decode(5000))
	assert.Equal(t, int64(3600), d.Decode(8600))
}

func TestTimeDecoderWrap(t *testing.T) {
	var d TimeDecoder
	d.Decode(1<<33 - 1800)
	assert.Equal(t, int64(3600), d.Decode(1800))
}

func TestTimeDecoderNegativeDiff(t *testing.T) {
	var d TimeDecoder
	assert.Equal(t, int64(0), d.Decode(10000))
	assert.Equal(t, int64(-3600), d.Decode(6400))
	assert.Equal(t, int64(3600), d.Decode(13600))
}

func TestTimeDecoderNegativeAcrossWrap(t *testing.T) {
	var d TimeDecoder
	d.Decode(1800)
	assert.Equal(t, int64(-3600), d.Decode(1<<33-1800))
}

func TestTimeDecoderHalfRangeBoundaries(t *testing.T) {
	var d TimeDecoder
	d.Decode(0)
	assert.Equal(t, -(int64(1) << 32), d.Decode(1<<32), "+2^32 maps to -2^32")

	var e TimeDecoder
	e.Decode(1 << 32)
	assert.Equal(t, -(int64(1) << 32), e.Decode(0), "-2^32 stays -2^32")
}
