package redact

import (
	"bytes"
	"crypto/sha256"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{"rtmp+insecure://u:pw@h/live/secretkey?token=t", "rtmp+insecure://u:xxxxx@h/live/sexxxxx?token=xxxxx"},
		{"RTMP+INSECURE://h/live/secretkey", "rtmp+insecure://h/live/sexxxxx"},
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
	assert.True(t, Masked("rtmp+insecure://h/live/abxxxxx"))
}

func TestRTMPFamily(t *testing.T) {
	for _, s := range []string{"rtmp", "rtmps", "rtmps+insecure", "rtmp+insecure", "RTMP+Insecure"} {
		assert.True(t, rtmpFamily(s), s)
	}
	for _, s := range []string{"", "rtsp", "rtsps+insecure", "http+insecure", "rtmpt", "rtmp+insecure+insecure", "+insecure"} {
		assert.False(t, rtmpFamily(s), s)
	}
}

func TestRTMPScheme(t *testing.T) {
	for _, s := range []string{"rtmp", "rtmps", "rtmps+insecure", "RTMP", "RTMPS", "Rtmps+Insecure"} {
		assert.True(t, RTMPScheme(s), s)
	}
	for _, s := range []string{"", "rtsp", "rtsps", "rtsps+insecure", "rtmpt", "rtmp+insecure", "http", "rtmps+"} {
		assert.False(t, RTMPScheme(s), s)
	}
}

func TestSecretName(t *testing.T) {
	for _, n := range []string{
		"password", "Password", "PASSWD", "pwd", "psw", "pw", "pass", "passphrase", "secret", "client_secret",
		"token", "access_token", "hdnts", "hdnea", "wmsauthsign", "sig", "signature", "X-Amz-Signature",
		"X-Amz-Credential", "X-Amz-Security-Token", "auth", "key", "apikey", "api_key", "authkey", "streamkey",
		"stream_key", "loginpas", "authbasic", "jwt", "id_jwt", "hmac", "hash", "md5",
	} {
		assert.True(t, SecretName(n), n)
	}
	name, err := url.PathUnescape("pass%77ord")
	require.NoError(t, err)
	assert.True(t, SecretName(name), "pass%77ord after the caller's unescape")
	for _, n := range []string{
		"user", "username", "usr", "channel", "stream", "subtype", "mode", "transport", "latency", "streamid",
		"iface", "loop", "pace", "pkt_size", "Policy", "Key-Pair-Id", "author", "keyframe", "design", "loginuse",
		"hashtag",
	} {
		assert.False(t, SecretName(n), n)
	}
}

// embeddedRows are R-T2's rows: an input and what URL returns for it.
var embeddedRows = []struct{ in, want string }{
	// Xiongmai, & form.
	{"rtsp://192.0.2.1:554/user=admin&password=s3cret&channel=1&stream=0.sdp?real_stream", "rtsp://192.0.2.1:554/user=admin&password=xxxxx&channel=1&stream=0.sdp?real_stream"},
	// Xiongmai, _ form.
	{"rtsp://192.0.2.1/user=admin_password=tlJwpbo6_channel=1_stream=0.sdp?real_stream", "rtsp://192.0.2.1/user=admin_password=xxxxx_channel=1_stream=0.sdp?real_stream"},
	// A password with _.
	{"rtsp://192.0.2.1/user=admin_password=a_b_channel=1", "rtsp://192.0.2.1/user=admin_password=xxxxx_channel=1"},
	// Empty.
	{"rtsp://192.0.2.1/user=admin&password=&channel=1", "rtsp://192.0.2.1/user=admin&password=xxxxx&channel=1"},
	{"rtsp://192.0.2.1/USER=admin&PASSWORD=x&channel=1", "rtsp://192.0.2.1/USER=admin&PASSWORD=xxxxx&channel=1"},
	// A name keeps its bytes, the value x%26y is hidden whole and channel=1 is kept.
	{"rtsp://192.0.2.1/user=admin&pass%77ord=x%26y&channel=1", "rtsp://192.0.2.1/user=admin&pass%77ord=xxxxx&channel=1"},
	// p%2Fq hidden whole: no "q" is left, channel=1 kept, one segment.
	{"rtsp://192.0.2.1/user=admin&password=p%2Fq&channel=1", "rtsp://192.0.2.1/user=admin&password=xxxxx&channel=1"},
	// x%26y=z hidden whole: the escaped & starts no pair "y".
	{"rtsp://192.0.2.1/user=admin&password=x%26y=z&channel=1", "rtsp://192.0.2.1/user=admin&password=xxxxx&channel=1"},
	// ; in a query: before a secret it separates pairs; a secret runs to the next & (review ruling I3).
	{"rtsp://cam/av_stream?user=admin;password=x;channel=1", "rtsp://cam/av_stream?user=admin;password=xxxxx"},
	{"http://cam/videostream.cgi?loginuse=admin&loginpas=12345", "http://cam/videostream.cgi?loginuse=admin&loginpas=xxxxx"},
	{"http://cam/snap?authbasic=YWRtaW46eA==", "http://cam/snap?authbasic=xxxxx"},
	{"https://cdn/x.m3u8?jwt=e30.e30.sig&md5=abc&hash=def", "https://cdn/x.m3u8?jwt=xxxxx&md5=xxxxx&hash=xxxxx"},
	// A query is not split at _ (base64url).
	{"https://cdn/x.m3u8?token=ab_cd=", "https://cdn/x.m3u8?token=xxxxx"},
	// cd starts no pair.
	{"https://cdn/x.m3u8?token=ab;cd", "https://cdn/x.m3u8?token=xxxxx"},
	// & is a hard split, as at HEAD.
	{"https://cdn/x.m3u8?key=x&flag", "https://cdn/x.m3u8?key=xxxxx&flag"},
	{"https://cdn/x.m3u8?user=u&real_stream&password=x", "https://cdn/x.m3u8?user=u&real_stream&password=xxxxx"},
	// Rule 3, then rule 5.
	{"rtmp://h/app/key=abcdef", "rtmp://h/app/kexxxxx"},
	// All after the third colon.
	{"srt://h:9000?streamid=publish:cam:user:pw:12:345", "srt://h:9000?streamid=publish:cam:user:xxxxx"},
	// Also when empty.
	{"srt://h:9000?streamid=publish:cam:user:", "srt://h:9000?streamid=publish:cam:user:xxxxx"},
	// Not the last segment.
	{"rtsp://192.0.2.1/live/user=admin&password=x/ch1", "rtsp://192.0.2.1/live/user=admin&password=xxxxx/ch1"},
	// The M2c note.
	{"rtsp://cam/av_stream?user=admin&password=x", "rtsp://cam/av_stream?user=admin&password=xxxxx"},
	{"rtsp://cam/h264/ch1/main/av_stream?username=admin&passwd=x", "rtsp://cam/h264/ch1/main/av_stream?username=admin&passwd=xxxxx"},
	// Foscam.
	{"http://cam/videostream.cgi?user=admin&pwd=x", "http://cam/videostream.cgi?user=admin&pwd=xxxxx"},
	{"rtsp://cam/videoMain?usr=admin&pwd=x", "rtsp://cam/videoMain?usr=admin&pwd=xxxxx"},
	{"http://cam/axis-cgi/mjpg/video.cgi?auth=YWRtaW46YWRtaW4=", "http://cam/axis-cgi/mjpg/video.cgi?auth=xxxxx"},
	{"https://cdn/live.m3u8?hdnts=exp=1~acl=/*~hmac=abc", "https://cdn/live.m3u8?hdnts=xxxxx"},
	{"https://cdn/live.m3u8?Policy=p&Signature=s&Key-Pair-Id=K1", "https://cdn/live.m3u8?Policy=p&Signature=xxxxx&Key-Pair-Id=K1"},
	{"https://s3/x.ts?X-Amz-Credential=c&X-Amz-Signature=s&X-Amz-Security-Token=t&X-Amz-Date=d", "https://s3/x.ts?X-Amz-Credential=xxxxx&X-Amz-Signature=xxxxx&X-Amz-Security-Token=xxxxx&X-Amz-Date=d"},
	{"http://h/live/token=abc123/index.m3u8", "http://h/live/token=xxxxx/index.m3u8"},
	// MediaMTX.
	{"srt://h:9000?streamid=publish:cam:user:pw12345", "srt://h:9000?streamid=publish:cam:user:xxxxx"},
	{"srt://h:9000?streamid=%23!::r=live/cam,token=abc", "srt://h:9000?streamid=%23!::r=live/cam,token=xxxxx"},
	{"srt://h:9000?streamid=live/cam&passphrase=0123456789", "srt://h:9000?streamid=live/cam&passphrase=xxxxx"},
	{"rtsp://h/x#password=abc", "rtsp://h/x#password=xxxxx"},
	// Kept from today.
	{"rtsp://u:pw@cam/s", "rtsp://u:xxxxx@cam/s"},
	{"rtmp://h/live/abcdef", "rtmp://h/live/abxxxxx"},
	{"rtmps://h/live/abcdef", "rtmps://h/live/abxxxxx"},
	{"rtmps+insecure://h/live/abcdef", "rtmps+insecure://h/live/abxxxxx"},
	// Over-masked, by R-T2.
	{"http://h/x?passthrough=1", "http://h/x?passthrough=xxxxx"},
	{"file:///media/pass=1/x.ts", "file:///media/pass=xxxxx/x.ts"},
}

// reviewRows are the rows of the Task 1 review's rulings (fix round 1).
var reviewRows = []struct{ in, want string }{
	// I1: rule 5 writes the decoded path, so rule 3 runs again on it.
	{"rtmp://h/app/password%3DS3CR3T/streamkey1", "rtmp://h/app/password=xxxxx/stxxxxx"},
	{"rtmp://h/app/user=a%26password=S3CR3T/key", "rtmp://h/app/user=a&password=xxxxx/kexxxxx"},
	{"rtmp://h/app/a%2Fpassword=S3CR3T/key", "rtmp://h/app/a/password=xxxxx/kexxxxx"},
	{"rtmp://h/token%3DS3CR3T/live/key", "rtmp://h/token=xxxxx/live/kexxxxx"},
	// I3: a secret value in a query runs to the next &.
	{"http://h/x?password=a;b=S3CR3T", "http://h/x?password=xxxxx"},
	{"http://h/x?user=admin;password=x", "http://h/x?user=admin;password=xxxxx"},
	{"srt://h:9000?streamid=publish:path:user:a;b=S3CR3T", "srt://h:9000?streamid=publish:path:user:xxxxx"},
	// I4: a name is judged apart from the pair grammar.
	{"rtsp://h/user=admin_api%5Fkey=S3CR3T", "rtsp://h/user=admin_api%5Fkey=xxxxx"},
	{"rtsp://h/user=admin&password_b64=S3CR3T", "rtsp://h/user=admin&password_b64=xxxxx"},
	{"rtsp://h/user=admin&token_v2=S3CR3T", "rtsp://h/user=admin&token_v2=xxxxx"},
	{"rtsp://h/user=admin&pwd_enc=S3CR3T&channel=1", "rtsp://h/user=admin&pwd_enc=xxxxx&channel=1"},
	{"rtsp://h/user=admin_secret_id=S3CR3T", "rtsp://h/user=admin_secret_id=xxxxx"},
	{"rtsp://h/user=admin&pass.word=S3CR3T", "rtsp://h/user=admin&pass.word=xxxxx"},
	{"rtsp://h/user=admin&my.password=S3CR3T", "rtsp://h/user=admin&my.password=xxxxx"},
	{"rtsp://h/user=admin&password%20=S3CR3T", "rtsp://h/user=admin&password%20=xxxxx"},
	{"rtsp://h/user=admin&2password=S3CR3T", "rtsp://h/user=admin&2password=xxxxx"},
	{"rtsp://h/user=admin&password[0]=S3CR3T", "rtsp://h/user=admin&password[0]=xxxxx"},
	{"rtsp://h/x%3Fpassword=S3CR3T", "rtsp://h/x%3Fpassword=xxxxx"},
	{"rtsp://h/user=admin:password=S3CR3T", "rtsp://h/user=admin:password=xxxxx"},
	{"rtsp://h/admin.password=S3CR3T", "rtsp://h/admin.password=xxxxx"},
	{"rtsp://h/admin+password=S3CR3T", "rtsp://h/admin+password=xxxxx"},
	// A whole secret name after a byte that is no letter or digit.
	{"rtsp://h/user=admin_pw=S3CR3T_channel=1", "rtsp://h/user=admin_pw=xxxxx_channel=1"},
	{"srt://h:9000?streamid=live%2Fcam%3Fhdnts%3DS3CR3T", "srt://h:9000?streamid=live%2Fcam%3Fhdnts%3Dxxxxx"},
	{"http://h/x#cb?sig=S3CR3T", "http://h/x#cb?sig=xxxxx"},
	// I4's accepted cost.
	{"rtsp://h/user=passenger_channel=1", "rtsp://h/user=passenger_channel=xxxxx"},
	// I5: in a streamid and the fragment, / and ? are pair boundaries; a streamid is walked once decoded.
	{"srt://h:9000?streamid=live/cam/token=S3CR3T", "srt://h:9000?streamid=live/cam/token=xxxxx"},
	{"srt://h:9000?streamid=live/cam?token=S3CR3T", "srt://h:9000?streamid=live/cam?token=xxxxx"},
	{"srt://h:9000?streamid=%23!::r=cam%2Ctoken%3DS3CR3T", "srt://h:9000?streamid=%23!::r=cam%2Ctoken%3Dxxxxx"},
	{"srt://h:9000?streamid=srt%3A%2F%2Fh%3A9999%2Fapp%2Fstream%3Fpolicy%3Dp%26signature%3DS3CR3T", "srt://h:9000?streamid=srt%3A%2F%2Fh%3A9999%2Fapp%2Fstream%3Fpolicy%3Dp%26signature%3Dxxxxx"},
	{"http://h/x#a/password=S3CR3T", "http://h/x#a/password=xxxxx"},
	{"http://h/x#?token=S3CR3T", "http://h/x#?token=xxxxx"},
	{"http://h/x#x?password=S3CR3T", "http://h/x#x?password=xxxxx"},
	// I6: MediaMTX's #!:: form, its s key and any secret name.
	{"srt://h:9000?streamid=%23!::m=publish,r=cam,u=user,s=S3CR3T", "srt://h:9000?streamid=%23!::m=publish,r=cam,u=user,s=xxxxx"},
	{"srt://h:9000?streamid=%23!::s=S3CR3T,r=cam", "srt://h:9000?streamid=%23!::s=xxxxx,r=cam"},
	{"srt://h:9000?streamid=%23!::r=cam,password=S3CR3T", "srt://h:9000?streamid=%23!::r=cam,password=xxxxx"},
}

// unchangedRows come back from URL byte for byte.
var unchangedRows = []string{
	"rtsp://cam/Streaming/Channels/101",
	"rtsp://cam/cam/realmonitor?channel=1&subtype=0",
	"http://h/a=b/c",
	"udp://239.0.0.1:1234?pkt_size=1316",
	"srt://h:9000?streamid=live/cam&latency=200&mode=listen",
	"rtsp://h/user=admin_channel=1",
}

func TestURLEmbeddedCredentials(t *testing.T) {
	for _, r := range slices.Concat(embeddedRows, reviewRows) {
		assert.Equal(t, r.want, URL(r.in), r.in)
	}
	for _, x := range unchangedRows {
		assert.Equal(t, x, URL(x))
	}
	// Nothing of a hidden value is left.
	assert.NotContains(t, URL("rtsp://192.0.2.1/user=admin&password=p%2Fq&channel=1"), "q&")
	assert.NotContains(t, URL("rtsp://192.0.2.1/user=admin_password=tlJwpbo6_channel=1"), "tlJwpbo6")
}

func TestMaskedEmbeddedCredentials(t *testing.T) {
	for _, r := range slices.Concat(embeddedRows, reviewRows) {
		assert.False(t, Masked(r.in), r.in)
		assert.True(t, Masked(r.want), r.want)
		assert.Equal(t, r.want, URL(r.want), "URL of %q", r.want)
	}
	for _, x := range unchangedRows {
		assert.False(t, Masked(x), x)
		assert.Equal(t, x, URL(x))
	}
}

func TestConstants(t *testing.T) {
	assert.Equal(t, "xxxxx", Mask)
	assert.Equal(t, "<invalid url>", Invalid)
	assert.Equal(t, Invalid, URL("http://[::1"))
	assert.Equal(t, Invalid, URL(Invalid))
	assert.False(t, Masked(Invalid))
}

// FuzzURL checks R-T2's postcondition: for a URL (a string that starts with
// its scheme and "://") that Masked does not report, Masked(URL(u)) is
// true exactly when URL hid something, and URL of its own result changes
// nothing. A string whose only "://" sits elsewhere, such as inside a
// hidden value, is not a URL: hiding can take its "://" away.
func FuzzURL(f *testing.F) {
	for _, r := range slices.Concat(embeddedRows, reviewRows) {
		f.Add(r.in)
	}
	for _, x := range unchangedRows {
		f.Add(x)
	}
	f.Add("srt://h:9000?streamid=publish%3Acam%3Auser%3Apw")
	f.Add("srt://h:9000?streamid=%23!::token=abc,r=x")
	f.Add("rtmp://h/token=abc/key")
	f.Fuzz(func(t *testing.T, u string) {
		p, err := url.Parse(u)
		if err != nil || !schemeURL(u, p) || Masked(u) {
			return
		}
		out, hid := redactURL(u)
		if Masked(out) != hid {
			t.Errorf("URL(%q) = %q, hid %v, Masked %v", u, out, hid, Masked(out))
		}
		if again := URL(out); again != out {
			t.Errorf("URL(%q) = %q, URL of that = %q", u, out, again)
		}
	})
}

func TestURLShortStreamKeyIdempotent(t *testing.T) {
	for _, x := range []string{"rtmp://h/live/ab", "rtmp:///0", "rtmps://h/live/s"} {
		once := URL(x)
		assert.True(t, Masked(once), once)
		assert.Equal(t, once, URL(once), x)
	}
}

func TestURLEmptyAuthorityKeepsScheme(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"srt://?token=t", "srt://?token=xxxxx"},
		{"a://#pass=", "a://#pass=xxxxx"},
	} {
		got := URL(c.in)
		assert.Equal(t, c.want, got, c.in)
		assert.True(t, Masked(got), got)
		assert.Equal(t, got, URL(got))
	}
	assert.Equal(t, "srt:?mode=listener", URL("srt://?mode=listener"), "nothing hidden: as at HEAD")
}
