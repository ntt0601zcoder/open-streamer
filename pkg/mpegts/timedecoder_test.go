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

func TestTimeDecoderAnchorRaw(t *testing.T) {
	var d TimeDecoder
	d.AnchorRaw()
	assert.Equal(t, int64(5000), d.Decode(5000), "the first output is the raw value")
	assert.Equal(t, int64(8600), d.Decode(8600))

	var w TimeDecoder
	w.AnchorRaw()
	assert.Equal(t, int64(1<<33-1800), w.Decode(1<<33-1800))
	assert.Equal(t, int64(1<<33+1800), w.Decode(1800), "unwrapped past 2^33")
}

func TestTimeDecoderAnchorNear(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ref, in int64
		want    int64
	}{
		{"same lap", 1000, 4600, 4600},
		{"ref past the wrap", 1<<33 + 9000, 5000, 1<<33 + 5000},
		{"input before the wrap, ref after", 1<<33 + 900, 1<<33 - 900, 1<<33 - 900},
		{"ref before the wrap, input after", 1<<33 - 900, 900, 1<<33 + 900},
		{"second lap", 3<<33 + 100, 200, 3<<33 + 200},
		{"negative ref", -900, 1<<33 - 1800, -1800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d TimeDecoder
			d.AnchorNear(tc.ref)
			assert.Equal(t, tc.want, d.Decode(tc.in))
			assert.Equal(t, tc.want+3600, d.Decode((tc.in+3600)%(1<<33)), "then unwrapped as usual")
		})
	}
}

func TestTimeDecoderAnchorAfterStartIgnored(t *testing.T) {
	var d TimeDecoder
	assert.Equal(t, int64(0), d.Decode(5000))
	d.AnchorRaw()
	d.AnchorNear(1 << 34)
	assert.Equal(t, int64(3600), d.Decode(8600))
}
