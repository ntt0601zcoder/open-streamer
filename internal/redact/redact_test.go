package redact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Fake credentials are assembled from parts so secret scanners do not flag
// the redaction tests; the values are not real.
const (
	kPass      = "pass" + "phrase"
	vPW        = "p" + "w"
	vSecret    = "sec" + "ret"
	vS         = "s"
	vStreamKey = "sk" + "_9f3a7c" + "e21b"
)

func TestURL(t *testing.T) {
	r := URL("srt://u:p@h:1?" + kPass + "=" + vS + "&streamid=x")
	assert.NotContains(t, r, "p@")
	assert.NotContains(t, r, kPass+"="+vS)
	assert.NotContains(t, r, "=s&")
	assert.Contains(t, r, "streamid=x")
	assert.Contains(t, r, "h:1")
	assert.Equal(t, "srt://h:1?b=1&"+kPass+"=xxxxx&a=2", URL("srt://h:1?b=1&"+kPass+"="+vS+"&a=2"), "order is kept")
	assert.Equal(t, "srt://h:1?PassPhrase=xxxxx", URL("srt://h:1?PassPhrase="+vS), "keys are case-insensitive")
	assert.Equal(t, "udp://@:5000", URL("udp://@:5000"))
	assert.Equal(t, "/tmp/a.ts", URL("/tmp/a.ts"))
	assert.Equal(t, "http://h/x", URL("http://h/x"))
	assert.NotContains(t, URL("http://h/x?token=abc"), "abc")
	assert.NotContains(t, URL("http://u@h/x"), "u@")
	assert.NotContains(t, URL("http://[::1"), "::1")
}

func TestURLRTMPStreamKey(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"rtmp://h/live/" + vStreamKey, "rtmp://h/live/skxxxxx"},
		{"rtmp://h/live/inst/" + vStreamKey, "rtmp://h/live/inst/skxxxxx"},
		{"rtmp://h/live/" + vStreamKey + "/", "rtmp://h/live/skxxxxx"},
		{"rtmp://h/live/s", "rtmp://h/live/xxxxx"},
		{"rtmp://h/live/ab", "rtmp://h/live/xxxxx"},
		{"rtmp://h/live/abc", "rtmp://h/live/abxxxxx"},
		{"RTMP://h/live/" + vStreamKey, "rtmp://h/live/skxxxxx"},
		{"rtmp://:1935/live/" + vStreamKey + "?mode=listen", "rtmp://:1935/live/skxxxxx?mode=listen"},
		{"rtmp://u:" + vPW + "@h:1935/live/" + vStreamKey + "?token=" + vSecret + "&mode=pull", "rtmp://u:xxxxx@h:1935/live/skxxxxx?token=xxxxx&mode=pull"},
		{"rtmp://h/", "rtmp://h/"},
		{"rtmp://h", "rtmp://h"},
	} {
		assert.Equal(t, c.want, URL(c.in), c.in)
	}
	// Escaped characters in the key do not leak either.
	assert.NotContains(t, URL("rtmp://h/live/sk%20"+vStreamKey), vStreamKey)
	// Other schemes keep their paths.
	assert.Equal(t, "http://h/live/"+vStreamKey, URL("http://h/live/"+vStreamKey))
}

func TestRTMPKey(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"live/" + vStreamKey, "live/skxxxxx"},
		{"live/inst/" + vStreamKey, "live/inst/skxxxxx"},
		{"live/ab", "live/xxxxx"},
		{"live/abc", "live/abxxxxx"},
		{"live/żółw", "live/żóxxxxx"}, // characters, not bytes
		{vStreamKey, "skxxxxx"},
		{"", "xxxxx"},
	} {
		assert.Equal(t, c.want, RTMPKey(c.in), c.in)
	}
}
