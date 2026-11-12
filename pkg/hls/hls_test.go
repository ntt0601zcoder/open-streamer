package hls

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaEncodeGolden(t *testing.T) {
	pdt := time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	p := MediaPlaylist{
		Version: 7, TargetDuration: 2, MediaSequence: 10, DiscontinuitySequence: 3, Independent: true,
		Segments: []Segment{
			{URI: "a.m4s", Duration: 2 * time.Second, Map: "init-1.mp4", ProgramDateTime: pdt},
			{URI: "b.m4s", Duration: 1500 * time.Millisecond, Map: "init-1.mp4"},
			{URI: "c.m4s", Duration: 2 * time.Second, Map: "init-2.mp4", Discontinuity: true},
		},
	}
	want := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:10\n" +
		"#EXT-X-DISCONTINUITY-SEQUENCE:3\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-MAP:URI=\"init-1.mp4\"\n#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:04:05.678Z\n#EXTINF:2.000,\na.m4s\n" +
		"#EXTINF:1.500,\nb.m4s\n" +
		"#EXT-X-DISCONTINUITY\n#EXT-X-MAP:URI=\"init-2.mp4\"\n#EXTINF:2.000,\nc.m4s\n"
	assert.Equal(t, want, string(p.Encode()))

	p.Ended = true
	assert.True(t, strings.HasSuffix(string(p.Encode()), "#EXT-X-ENDLIST\n"))
}

func TestMediaRoundTrip(t *testing.T) {
	pdt := time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	in := MediaPlaylist{
		Version: 7, TargetDuration: 2, MediaSequence: 10, DiscontinuitySequence: 3, Independent: true, Ended: true,
		Segments: []Segment{
			{URI: "a.m4s", Duration: 2 * time.Second, Map: "i1.mp4", ProgramDateTime: pdt},
			{URI: "b.m4s", Duration: 1500 * time.Millisecond, Map: "i1.mp4"},
			{URI: "c.m4s", Duration: 2 * time.Second, Map: "i2.mp4", Discontinuity: true},
		},
	}
	out, err := ParseMedia(in.Encode())
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestParseMediaHandWritten(t *testing.T) {
	src := "\xef\xbb\xbf#EXTM3U\r\n\r\n#EXT-X-VERSION:3\r\n#EXT-X-TARGETDURATION:6\r\n#EXT-X-MEDIA-SEQUENCE:5\r\n" +
		"#EXT-X-UNKNOWN:FOO=\"bar\"\r\n# a comment\r\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-01-02T03:04:05+01:00\r\n#EXTINF:5.005,title\r\ns1.ts\r\n\r\n" +
		"#EXT-X-DISCONTINUITY\r\n#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"10@0\"\r\n#EXTINF:6,\r\ns2.ts\r\n" +
		"#EXTINF:4.0,\r\ns3.ts\r\n#EXT-X-ENDLIST\r\n"
	p, err := ParseMedia([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, 3, p.Version)
	assert.Equal(t, 6, p.TargetDuration)
	assert.Equal(t, uint64(5), p.MediaSequence)
	assert.True(t, p.Ended)
	require.Len(t, p.Segments, 3)
	assert.Equal(t, 5005*time.Millisecond, p.Segments[0].Duration)
	assert.True(t, p.Segments[0].ProgramDateTime.Equal(time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC)))
	assert.True(t, p.Segments[1].Discontinuity)
	assert.Equal(t, "init.mp4", p.Segments[1].Map)
	assert.Equal(t, "init.mp4", p.Segments[2].Map, "map is sticky")
	assert.False(t, p.Segments[2].Discontinuity)
	assert.Equal(t, "", p.Segments[0].Map)
}

func TestParseMediaErrors(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"no header":        "#EXT-X-VERSION:3\n",
		"bad version":      "#EXTM3U\n#EXT-X-VERSION:x\n",
		"bad target":       "#EXTM3U\n#EXT-X-TARGETDURATION:x\n",
		"bad sequence":     "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:-1\n",
		"bad disc seq":     "#EXTM3U\n#EXT-X-DISCONTINUITY-SEQUENCE:x\n",
		"bad extinf":       "#EXTM3U\n#EXTINF:abc,\na.ts\n",
		"negative extinf":  "#EXTM3U\n#EXTINF:-1,\na.ts\n",
		"bad pdt":          "#EXTM3U\n#EXT-X-PROGRAM-DATE-TIME:nope\n#EXTINF:1,\na.ts\n",
		"uri no extinf":    "#EXTM3U\na.ts\n",
		"map no uri":       "#EXTM3U\n#EXT-X-MAP:BYTERANGE=\"1@2\"\n",
		"unterminated map": "#EXTM3U\n#EXT-X-MAP:URI=\"x\n",
		"multivariant":     "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMedia([]byte(src))
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "hls: "), err.Error())
		})
	}
}

func TestMultivariantEncodeGolden(t *testing.T) {
	p := MultivariantPlaylist{
		Version: 7, Independent: true,
		Renditions: []Rendition{{Type: "AUDIO", GroupID: "aud", Name: "en", URI: "a1.m3u8", Default: true}},
		Variants: []Variant{
			{URI: "v1.m3u8", Codecs: "avc1.64001f,mp4a.40.2", Audio: "aud", Bandwidth: 2000000, Width: 1280, Height: 720},
			{URI: "v2.m3u8", Bandwidth: 500000},
		},
	}
	want := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"en\",DEFAULT=YES,AUTOSELECT=YES,URI=\"a1.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\",AUDIO=\"aud\"\nv1.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=500000\nv2.m3u8\n"
	assert.Equal(t, want, string(p.Encode()))
}

func TestMultivariantRoundTrip(t *testing.T) {
	in := MultivariantPlaylist{
		Version: 7, Independent: true,
		Renditions: []Rendition{
			{Type: "AUDIO", GroupID: "aud", Name: "en, main", URI: "a1.m3u8", Default: true},
			{Type: "AUDIO", GroupID: "aud", Name: "fr", URI: "a2.m3u8"},
		},
		Variants: []Variant{
			{URI: "v1.m3u8", Codecs: "avc1.64001f,mp4a.40.2", Audio: "aud", Bandwidth: 2000000, Width: 1280, Height: 720},
			{URI: "v2.m3u8", Bandwidth: 500000},
		},
	}
	out, err := ParseMultivariant(in.Encode())
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestParseMultivariantHandWritten(t *testing.T) {
	src := "#EXTM3U\r\n#EXT-X-VERSION:6\r\n#EXT-X-FOO:1\r\n\r\n" +
		"#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=800000,AVERAGE-BANDWIDTH=700000,CODECS=\"avc1.4d401e,mp4a.40.2\",RESOLUTION=640x360,FRAME-RATE=25\r\n" +
		"\r\nlow.m3u8\r\n"
	p, err := ParseMultivariant([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, 6, p.Version)
	require.Len(t, p.Variants, 1)
	v := p.Variants[0]
	assert.Equal(t, "low.m3u8", v.URI)
	assert.Equal(t, "avc1.4d401e,mp4a.40.2", v.Codecs)
	assert.Equal(t, 800000, v.Bandwidth)
	assert.Equal(t, 640, v.Width)
	assert.Equal(t, 360, v.Height)
}

func TestParseMultivariantErrors(t *testing.T) {
	cases := map[string]string{
		"no header":      "#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n",
		"bad version":    "#EXTM3U\n#EXT-X-VERSION:x\n",
		"bad bandwidth":  "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=x\nv.m3u8\n",
		"bad resolution": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=bad\nv.m3u8\n",
		"bad res nums":   "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=axb\nv.m3u8\n",
		"no uri":         "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n",
		"no uri 2":       "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n#EXT-X-STREAM-INF:BANDWIDTH=2\nv.m3u8\n",
		"bad attr":       "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,NAME=\"x\n",
		"media playlist": "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\na.ts\n",
		"media no type":  "#EXTM3U\n#EXT-X-MEDIA:NAME=\"x\"\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMultivariant([]byte(src))
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "hls: "), err.Error())
		})
	}
}

func TestParseAttrs(t *testing.T) {
	m, err := parseAttrs(`A=1, B="x,y=z" ,C=hello,D=""`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1", "B": "x,y=z", "C": "hello", "D": ""}, m)

	_, err = parseAttrs(`A`)
	require.Error(t, err)
	_, err = parseAttrs(`A="x"y`)
	require.Error(t, err)
}

func TestParseMediaColonlessOffset(t *testing.T) {
	p, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-PROGRAM-DATE-TIME:2026-11-11T23:17:11.457+0700\n#EXTINF:2,\na.ts\n"))
	require.NoError(t, err)
	require.Len(t, p.Segments, 1)
	assert.True(t, p.Segments[0].ProgramDateTime.Equal(time.Date(2026, 11, 11, 16, 17, 11, 457000000, time.UTC)))
}

func TestParseMediaMapErrorSinglePrefix(t *testing.T) {
	_, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-MAP:URI=\"x\n"))
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), "hls:"), err.Error())
}
