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
