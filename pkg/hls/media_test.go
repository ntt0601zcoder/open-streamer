package hls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHLSKeyAndByteRange(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:2\n" +
		"#EXT-X-BYTERANGE:100@0\n#EXTINF:2,\nall.ts\n" +
		"#EXT-X-BYTERANGE:200\n#EXTINF:2,\nall.ts\n" + // follows the previous range: 100
		"#EXT-X-KEY:METHOD=AES-128,URI=\"https://k/key?x=1\",IV=0x00000000000000000000000000000001\n" +
		"#EXT-X-BYTERANGE:50@1000\n#EXTINF:2,\nother.ts\n" +
		"#EXT-X-BYTERANGE:300\n#EXTINF:2,\nall.ts\n" + // the previous range of all.ts ended at 300
		"#EXTINF:2,\nplain.ts\n" +
		"#EXT-X-KEY:METHOD=NONE\n#EXTINF:2,\nclear.ts\n#EXT-X-ENDLIST\n"
	p, err := ParseMedia([]byte(src))
	require.NoError(t, err)
	require.Len(t, p.Segments, 6)
	s := p.Segments

	assert.Equal(t, &ByteRange{Length: 100, Offset: 0}, s[0].ByteRange)
	assert.Equal(t, &ByteRange{Length: 200, Offset: 100}, s[1].ByteRange)
	assert.Equal(t, &ByteRange{Length: 50, Offset: 1000}, s[2].ByteRange)
	assert.Equal(t, &ByteRange{Length: 300, Offset: 300}, s[3].ByteRange)
	assert.Nil(t, s[4].ByteRange)
	assert.Nil(t, s[5].ByteRange)

	assert.Equal(t, Key{}, s[0].Key, "no key tag yet")
	assert.Equal(t, Key{}, s[1].Key)
	aes := Key{Method: "AES-128", URI: "https://k/key?x=1"}
	assert.Equal(t, aes, s[2].Key)
	assert.Equal(t, aes, s[3].Key, "the key is sticky")
	assert.Equal(t, aes, s[4].Key)
	assert.Equal(t, Key{Method: "NONE"}, s[5].Key)
	assert.Equal(t, 2*time.Second, s[5].Duration)

	// Encode writes both tags back.
	out, err := ParseMedia(p.Encode())
	require.NoError(t, err)
	assert.Equal(t, p, out)
}

func TestHLSKeyAndByteRangeErrors(t *testing.T) {
	cases := map[string]string{
		"range without previous": "#EXTM3U\n#EXT-X-BYTERANGE:10\n#EXTINF:1,\na.ts\n",
		"range of another uri":   "#EXTM3U\n#EXT-X-BYTERANGE:10@0\n#EXTINF:1,\na.ts\n#EXT-X-BYTERANGE:10\n#EXTINF:1,\nb.ts\n",
		"bad range length":       "#EXTM3U\n#EXT-X-BYTERANGE:x@0\n#EXTINF:1,\na.ts\n",
		"zero range length":      "#EXTM3U\n#EXT-X-BYTERANGE:0@0\n#EXTINF:1,\na.ts\n",
		"bad range offset":       "#EXTM3U\n#EXT-X-BYTERANGE:10@-1\n#EXTINF:1,\na.ts\n",
		"key without method":     "#EXTM3U\n#EXT-X-KEY:URI=\"k\"\n#EXTINF:1,\na.ts\n",
		"key without uri":        "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128\n#EXTINF:1,\na.ts\n",
		"bad key attrs":          "#EXTM3U\n#EXT-X-KEY:METHOD=\"AES-128\n#EXTINF:1,\na.ts\n",
	}
	for name, src := range cases {
		_, err := ParseMedia([]byte(src))
		assert.Error(t, err, name)
	}
}

// Playlist lines can carry credentials (signed URIs, key URLs): errors must
// not quote them.
func TestParseErrorsQuoteNoLine(t *testing.T) {
	// Assembled from parts so secret scanners do not flag a literal.
	secret := "s3" + "cr" + "et"
	cases := []string{
		"#EXTM3U\nhttps://u:" + secret + "@h/a.ts\n",
		"#EXTM3U\n#EXT-X-BYTERANGE:10\n#EXTINF:1,\nhttps://h/a.ts?token=" + secret + "\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://h/k?token=" + secret + "\n#EXTINF:1,\na.ts\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://h/k?token=" + secret + "\"x\n#EXTINF:1,\na.ts\n",
		"#EXTM3U\n#EXT-X-MAP:" + secret + "\n#EXTINF:1,\na.ts\n",
	}
	for _, src := range cases {
		_, err := ParseMedia([]byte(src))
		require.Error(t, err, src)
		assert.NotContains(t, err.Error(), secret, src)
	}
	_, err := ParseMultivariant([]byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,URI=\"https://h/a?token=" + secret + "\n"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

// EXT-X-GAP (RFC 8216bis section 4.4.4.7) marks only the segment that
// follows it.
func TestParseGap(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:4\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2.000,\n1-0.m4s\n" +
		"#EXT-X-DISCONTINUITY\n#EXT-X-GAP\n#EXTINF:2.000,\ngap-1-5.m4s\n" +
		"#EXT-X-GAP\n#EXTINF:1.960,\ngap-1-6.m4s\n" +
		"#EXTINF:2.000,\n1-1.m4s\n"
	p, err := ParseMedia([]byte(src))
	require.NoError(t, err)
	require.Len(t, p.Segments, 4)
	assert.False(t, p.Segments[0].Gap)
	assert.True(t, p.Segments[1].Gap)
	assert.True(t, p.Segments[1].Discontinuity)
	assert.True(t, p.Segments[2].Gap)
	assert.Equal(t, 1960*time.Millisecond, p.Segments[2].Duration)
	assert.False(t, p.Segments[3].Gap, "the tag applies to one segment")
	for _, s := range p.Segments {
		assert.Equal(t, "init.mp4", s.Map)
	}

	out := p.Encode()
	assert.Contains(t, string(out), "#EXT-X-DISCONTINUITY\n#EXT-X-GAP\n#EXTINF:2.000,\ngap-1-5.m4s\n")
	back, err := ParseMedia(out)
	require.NoError(t, err)
	assert.Equal(t, p, back)
}
