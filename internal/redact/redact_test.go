package redact

import (
	"bytes"
	"crypto/sha256"
	"sync"
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

func TestMasked(t *testing.T) {
	// Each form URL masks: Masked(URL(x)) is true and Masked(x) is false.
	for _, x := range []string{
		"rtsp://u:" + vPW + "@h/a",
		"http://u@h/x",
		"srt://h:1?" + kPass + "=" + vS + "&streamid=x",
		"srt://h:1?PassPhrase=" + vS,
		"http://h/x?token=abc",
		"http://h/x?a=1&password=" + vPW,
		"http://h/x?secret=" + vSecret,
		"http://h/x?key=" + vS,
		"rtmp://h/live/" + vStreamKey,
		"rtmp://h/live/" + vStreamKey + "/",
		"rtmp://h/live/ab",
		"rtmp://h/live/abc",
		"RTMP://h/live/" + vStreamKey,
		"rtmp://:1935/live/" + vStreamKey + "?mode=listen",
	} {
		assert.True(t, Masked(URL(x)), "Masked(URL(%q)) = Masked(%q)", x, URL(x))
		assert.False(t, Masked(x), x)
	}
	// A URL without secrets is false both ways.
	for _, x := range []string{
		"udp://@:5000",
		"udp://:5000",
		"http://h/x",
		"http://h/x?streamid=x&mode=pull",
		"http://h/live/" + vStreamKey,
		"srt://h:1",
		"/tmp/a.ts",
		"file:///tmp/a.ts",
		"",
	} {
		assert.False(t, Masked(x), x)
		assert.False(t, Masked(URL(x)), "URL(%q) = %q", x, URL(x))
	}
	// The mask counts only where URL writes it.
	assert.False(t, Masked("http://h/xxxxx"), "a path segment of another scheme")
	assert.False(t, Masked("http://h/x?mode=xxxxx"), "a value of a key that is not secret")
	assert.False(t, Masked("http://xxxxx.example/x"), "a host")
	assert.True(t, Masked("rtsp://user:xxxxx@host/a"))
	assert.True(t, Masked("rtmp://h/app/abxxxxx"))
	assert.False(t, Masked("http://[::1"), "an invalid URL")
}

func TestTag(t *testing.T) {
	a := Tag([]byte("rtmp://h/live/secretkey"))
	if len(a) != sha256.Size {
		t.Fatalf("a tag of %d bytes", len(a))
	}
	if !bytes.Equal(a, Tag([]byte("rtmp://h/live/secretkey"))) {
		t.Error("not a pure function of its input within one process")
	}
	if bytes.Equal(a, Tag([]byte("rtmp://h/live/secretkez"))) {
		t.Error("two values, one tag")
	}
	// Keyed: not the plain hash, which a guess could be tested against.
	if plain := sha256.Sum256([]byte("rtmp://h/live/secretkey")); bytes.Equal(a, plain[:]) {
		t.Error("the tag is the unkeyed SHA-256")
	}
	if !bytes.Equal(Tag(nil), Tag([]byte{})) {
		t.Error("nil and empty differ")
	}
	// The key is made once: tags from many goroutines agree.
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if !bytes.Equal(a, Tag([]byte("rtmp://h/live/secretkey"))) {
				t.Error("a different key")
			}
		})
	}
	wg.Wait()
}

func TestURLTLSSchemes(t *testing.T) {
	masked := []struct{ in, want string }{
		{"rtmps://h/live/secretkey", "rtmps://h/live/sexxxxx"},
		{"rtmps+insecure://u:pw@h:443/live/secretkey?token=t0k", "rtmps+insecure://u:xxxxx@h:443/live/sexxxxx?token=xxxxx"},
		{"RTMPS://h/live/secretkey", "rtmps://h/live/sexxxxx"},
		{"RTMPS+Insecure://h/live/secretkey", "rtmps+insecure://h/live/sexxxxx"},
		{"rtsps://u:pw@cam/s", "rtsps://u:xxxxx@cam/s"},
		{"rtsps+insecure://u:pw@cam/s", "rtsps+insecure://u:xxxxx@cam/s"},
		{"rtmp://h/live/secretkey", "rtmp://h/live/sexxxxx"},
		{"rtmp://u:pw@h:1935/live/secretkey?token=t0k", "rtmp://u:xxxxx@h:1935/live/sexxxxx?token=xxxxx"},
		{"RTMP://h/live/secretkey", "rtmp://h/live/sexxxxx"},
	}
	for _, c := range masked {
		assert.Equal(t, c.want, URL(c.in), c.in)
		assert.True(t, Masked(c.want), c.want)
		assert.False(t, Masked(c.in), c.in)
	}
	// rtsps paths are not stream keys.
	for _, x := range []string{"rtsps://cam/live/secretkey", "rtsps+insecure://cam/live/secretkey"} {
		assert.Equal(t, x, URL(x))
		assert.False(t, Masked(x), x)
	}
	assert.False(t, Masked("rtsps://cam/live/abxxxxx"), "the mask in an rtsps path is not where URL writes it")
	assert.True(t, Masked("rtmps+insecure://h/live/abxxxxx"))
}

func TestRTMPScheme(t *testing.T) {
	for _, s := range []string{"rtmp", "rtmps", "rtmps+insecure", "RTMP", "RTMPS", "Rtmps+Insecure"} {
		assert.True(t, RTMPScheme(s), s)
	}
	for _, s := range []string{"", "rtsp", "rtsps", "rtsps+insecure", "rtmpt", "rtmp+insecure", "http", "rtmps+"} {
		assert.False(t, RTMPScheme(s), s)
	}
}
