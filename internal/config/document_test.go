package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// testOpt is what every test parses with, so the hashes can be made at
// 1000 iterations.
var testOpt = Options{RequiredIterations: 1000}

// hashAt returns a well-formed hash string at iter iterations. Parse never
// verifies a password, so the key need not be a real derivation.
func hashAt(iter int) string {
	return auth.Hash{
		Iterations: iter,
		Salt:       []byte("0123456789abcdef"),
		Key:        bytes.Repeat([]byte{7}, auth.KeyBytes),
	}.String()
}

// head is a valid version and users section; tests append the rest.
func head() string {
	return "version: 1\nusers:\n  - name: ops\n    role: editor\n    password: \"" + hashAt(1000) + "\"\n"
}

// streamYAML is a valid stream entry named name, indented for the streams
// sequence, with extra lines (already indented by 4) added to it.
func streamYAML(name string, extra ...string) string {
	s := "  - name: " + name + "\n    inputs:\n      - url: rtmp://src.example/live/" + name + "\n"
	for _, e := range extra {
		s += e + "\n"
	}
	return s
}

func parseOK(t *testing.T, text string) Parsed {
	t.Helper()
	p, err := Parse([]byte(text), testOpt)
	require.NoError(t, err)
	return p
}

// errText is the message of err, which must be a *SyntaxError or Errors.
func errText(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	var se *SyntaxError
	var es Errors
	if !errors.As(err, &se) && !errors.As(err, &es) {
		t.Fatalf("error %T is neither *SyntaxError nor Errors: %v", err, err)
	}
	return err.Error()
}

func TestParseMinimal(t *testing.T) {
	p := parseOK(t, head())
	assert.Equal(t, 1, p.Doc.Version)
	assert.Empty(t, p.Doc.Streams)
	r := p.Doc.Server.Resolved()
	assert.Equal(t, ":8080", r.HTTP.Listen)
	assert.Equal(t, "127.0.0.1:8081", r.API.Listen)
	assert.Equal(t, "live", r.RTMP.App)
	assert.Equal(t, "live", r.RTSP.App)
	assert.Equal(t, "live", r.SRT.App)
	assert.Equal(t, 120, r.SRT.LatencyMS)
	assert.Equal(t, "info", r.Log.Level)
	assert.Equal(t, "json", r.Log.Format)
	assert.Empty(t, r.RTMP.Listen)
	assert.Empty(t, r.Transcoder.Binary)
	require.NotNil(t, p.Users)
	assert.Equal(t, 1, p.Users.Len())
	require.Len(t, p.UserList, 1)
	assert.True(t, p.Users.Equal(p.UserList))
	assert.Equal(t, auth.Editor, p.UserList[0].Role)
}

func TestParseStrict(t *testing.T) {
	syntaxCases := []struct {
		name, text, want string
	}{
		{"unknown top-level key", head() + "colour: red\n", "colour"},
		{"unknown key in server.rtmp", head() + "server:\n  rtmp:\n    lisen: \":1935\"\n", "lisen"},
		{"unknown key in a user", "version: 1\nusers:\n  - name: ops\n    role: editor\n    nick: o\n    password: \"" + hashAt(1000) + "\"\n", "nick"},
		{"duplicate key", head() + "version: 1\n", "already defined"},
		{"two documents", head() + "---\nversion: 1\n", "more than one document"},
		{"tab-indented line", head() + "server:\n\trtmp: {}\n", "line 7"},
		{"too large", head() + "# " + strings.Repeat("x", MaxBytes) + "\n", "larger than 4194304 bytes"},
		{"empty", "", "no document"},
		{"not a mapping", "- 1\n", "cannot unmarshal"},
	}
	for _, c := range syntaxCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.text), testOpt)
			var se *SyntaxError
			require.ErrorAs(t, err, &se)
			assert.Contains(t, err.Error(), c.want)
		})
	}

	t.Run("4 MiB + 1 byte", func(t *testing.T) {
		text := head()
		text += "#" + strings.Repeat("x", MaxBytes+1-len(text)-2) + "\n"
		require.Len(t, text, MaxBytes+1)
		_, err := Parse([]byte(text), testOpt)
		var se *SyntaxError
		require.ErrorAs(t, err, &se)
		// One byte less is in bounds.
		_, err = Parse([]byte(text[:MaxBytes-1]+"\n"), testOpt)
		require.NoError(t, err)
	})

	t.Run("unknown key in a rendition", func(t *testing.T) {
		text := head() + "streams:\n" + streamYAML("cam",
			"    transcode:",
			"      renditions:",
			"        - label: hd",
			"          heigth: 720",
			"          bitrate_kbps: 800")
		_, err := Parse([]byte(text), testOpt)
		msg := errText(t, err)
		assert.Contains(t, msg, "streams[0] (cam): line 13: field heigth not found")
	})

	nodeCases := []struct {
		name, text string
		want       []string
	}{
		{
			"anchor and alias between streams",
			head() + "streams:\n" +
				"  - &cam {name: cam, inputs: [{url: rtmp://a/live/cam}]}\n" +
				"  - *cam\n",
			[]string{"line 7: anchors, aliases and merge keys are not supported", "line 8: anchors"},
		},
		{
			"merge key inside a stream",
			head() + "streams:\n" +
				streamYAML("cam", "    outputs: &defaults", "      hls: true") +
				streamYAML("other", "    outputs:", "      <<: *defaults", "      dash: true"),
			[]string{"line 10: anchors", "line 16: anchors"},
		},
		{
			"an anchor never used",
			head() + "streams:\n" + streamYAML("cam", "    outputs: &o {hls: true}"),
			[]string{"line 10: anchors"},
		},
	}
	for _, c := range nodeCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.text), testOpt)
			var es Errors
			require.ErrorAs(t, err, &es)
			for _, w := range c.want {
				assert.Contains(t, err.Error(), w)
			}
		})
	}

	ladder := func(height, bitrate string) string {
		return head() + "streams:\n" + streamYAML("cam",
			"    transcode:",
			"      renditions:",
			"        - label: hd",
			"          height: "+height,
			"          bitrate_kbps: "+bitrate)
	}
	for _, h := range []string{"0720", "0x2d0", "1_080", "720.0", "720.5", "7.2e2"} {
		t.Run("height "+h, func(t *testing.T) {
			_, err := Parse([]byte(ladder(h, "800")), testOpt)
			var es Errors
			require.ErrorAs(t, err, &es)
			assert.Contains(t, err.Error(), "streams[0] (cam): line 13: write the number as a plain decimal integer, or quote it if it is text")
		})
	}
	t.Run("bitrate as a string", func(t *testing.T) {
		_, err := Parse([]byte(ladder("720", `"800"`)), testOpt)
		msg := errText(t, err)
		assert.Contains(t, msg, "line 14: cannot unmarshal !!str `…` into int")
	})
	t.Run("plain decimals pass", func(t *testing.T) {
		p := parseOK(t, ladder("720", "800")+"server:\n  srt:\n    latency_ms: 120\n")
		assert.Equal(t, 720, p.Doc.Streams[0].Transcode.Renditions[0].Height)
		assert.Equal(t, 120, p.Doc.Server.SRT.LatencyMS)
	})
	t.Run("700000 iterations", func(t *testing.T) {
		h := hashAt(700000)
		text := "version: 1\nusers:\n  - name: ops\n    role: editor\n    password: \"" + h + "\"\n"
		_, err := Parse([]byte(text), Options{})
		msg := errText(t, err)
		assert.Contains(t, msg, "users[0].password")
		assert.NotContains(t, msg, h)
		assert.NotContains(t, msg, "MDEy") // the salt's base64
	})

	numberLike := func(name, label, text string) string {
		return head() + "streams:\n" + streamYAML(name,
			"    transcode:",
			"      renditions:",
			"        - label: "+label,
			"          height: 720",
			"          bitrate_kbps: 800",
			"      watermark:",
			"        text: "+text)
	}
	for _, c := range []struct{ name, text string }{
		{"name 007", numberLike("007", "hd", "live")},
		{"label 007", numberLike("cam", "007", "live")},
		{"text 24.7", numberLike("cam", "hd", "24.7")},
		{"text 1e3", numberLike("cam", "hd", "1e3")},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.text), testOpt)
			var es Errors
			require.ErrorAs(t, err, &es)
			assert.Contains(t, err.Error(), "write the number as a plain decimal integer, or quote it if it is text")
		})
	}
	t.Run("number-like text quoted", func(t *testing.T) {
		p := parseOK(t, numberLike(`"007"`, `"007"`, `"24.7"`))
		s := p.Doc.Streams[0]
		assert.Equal(t, "007", s.Name)
		assert.Equal(t, "007", s.Transcode.Renditions[0].Label)
		assert.Equal(t, "24.7", s.Transcode.Watermark.Text)
		p = parseOK(t, numberLike("cam", "hd", `'1e3'`))
		assert.Equal(t, "1e3", p.Doc.Streams[0].Transcode.Watermark.Text)
	})
	t.Run("a comment and a quoted hash", func(t *testing.T) {
		parseOK(t, "# the config\nversion: 1 # one\nusers:\n  - name: ops\n    role: editor\n    password: '"+hashAt(1000)+"'\n")
	})
}

func TestSyntaxErrorHidesValues(t *testing.T) {
	check := func(t *testing.T, text string, absent ...string) string {
		t.Helper()
		_, err := Parse([]byte(text), testOpt)
		var se *SyntaxError
		require.ErrorAs(t, err, &se)
		msg := err.Error()
		for _, a := range absent {
			assert.NotContains(t, msg, a)
		}
		// Every backquoted piece reads `…`.
		for _, m := range regexp.MustCompile("`[^`]*`").FindAllString(msg, -1) {
			assert.Equal(t, "`…`", m)
		}
		return msg
	}
	t.Run("int field", func(t *testing.T) {
		msg := check(t, head()+"server:\n  srt:\n    latency_ms: hunter2\n", "hunter2", "hunter")
		assert.Contains(t, msg, "line 8: cannot unmarshal !!str `…` into int")
	})
	t.Run("a password under the wrong key", func(t *testing.T) {
		pw := "correct-horse-battery"
		msg := check(t, "version: "+pw+"\n"+head()[len("version: 1\n"):], pw, "correct")
		assert.Contains(t, msg, "line 1: cannot unmarshal !!str `…` into int")
		msg = check(t, head()+"server: hunter2\n", "hunter2")
		assert.Contains(t, msg, "line 6: cannot unmarshal !!str `…` into config.Server")
	})
	t.Run("a backquote inside the value", func(t *testing.T) {
		check(t, head()+"server:\n  srt:\n    latency_ms: hun`ter2`tail\n", "hun", "ter2", "tail")
	})
	t.Run("a newline inside the value", func(t *testing.T) {
		check(t, head()+"server:\n  srt:\n    latency_ms: \"abc\\nsecret\"\n", "abc", "secret")
	})
	t.Run("two bad fields", func(t *testing.T) {
		msg := check(t, head()+"server:\n  srt:\n    latency_ms: hunter2\n  rtsp:\n    udp: swordfish\n", "hunter2", "swordf")
		assert.Contains(t, msg, "line 8: cannot unmarshal !!str `…` into int")
		assert.Contains(t, msg, "line 10: cannot unmarshal !!str `…` into bool")
	})
	t.Run("not a type error", func(t *testing.T) {
		se := &SyntaxError{Err: errors.New("yaml: bad `secret\nvalue` here")}
		assert.Equal(t, "config: yaml: bad `…` here", se.Error())
	})
}

func TestParseUsers(t *testing.T) {
	user := func(name, role, password string) string {
		return "  - name: " + name + "\n    role: " + role + "\n    password: \"" + password + "\"\n"
	}
	h := hashAt(1000)
	padded := strings.Replace(h, "$MDEyMzQ1Njc4OWFiY2RlZg$", "$MDEyMzQ1Njc4OWFiY2RlZg==$", 1)
	short := auth.Hash{Iterations: 1000, Salt: []byte("0123456789abcde"), Key: bytes.Repeat([]byte{7}, 32)}.String()
	var many strings.Builder
	for i := range 65 {
		many.WriteString(user("u"+strconv.Itoa(i), "editor", h))
	}
	cases := []struct {
		name, users string
		opt         Options
		want        string
		secrets     []string
	}{
		{"plaintext", user("ops", "editor", "hunter2-plaintext"), testOpt, `users[0].password: user "ops": auth: parse hash`, []string{"hunter2"}},
		{"bcrypt", user("ops", "editor", "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ01234"), testOpt, `users[0].password: user "ops": auth: parse hash`, []string{"$2a$", "abcdefgh"}},
		{"599999", user("ops", "editor", hashAt(599999)), Options{}, `users[0].password: user "ops": the hash has 599999 iterations, want 600000`, []string{hashAt(599999)}},
		{"700000", user("ops", "editor", hashAt(700000)), Options{}, `users[0].password: user "ops": the hash has 700000 iterations, want 600000`, []string{hashAt(700000)}},
		{"short salt", user("ops", "editor", short), testOpt, `users[0].password: user "ops": auth: parse hash: salt too short`, []string{short}},
		{"padded", user("ops", "editor", padded), testOpt, `users[0].password: user "ops": auth: parse hash`, []string{padded}},
		{"role admin", user("ops", "editor", h) + user("watch", "admin", h), testOpt, `users[1].role: user "watch": want viewer or editor`, nil},
		{"two ops", user("ops", "editor", h) + user("ops", "viewer", h), testOpt, `users[1].name: user "ops" appears twice`, nil},
		{"bad name", user("ops", "editor", h) + user("Hunter2", "viewer", h), testOpt, `users[1].name: must match ^[a-z0-9][a-z0-9._-]{0,31}$`, []string{"Hunter2"}},
		{"no editor", user("watch", "viewer", h), testOpt, "users: no editor", nil},
		{"65 users", many.String(), testOpt, "users: 65 users, want at most 64", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte("version: 1\nusers:\n"+c.users), c.opt)
			var es Errors
			require.ErrorAs(t, err, &es)
			msg := err.Error()
			assert.Contains(t, msg, c.want)
			assert.NotContains(t, msg, h)
			assert.NotContains(t, msg, "MDEy")
			for _, s := range c.secrets {
				assert.NotContains(t, msg, s)
			}
		})
	}
	t.Run("600000 with the default options", func(t *testing.T) {
		p, err := Parse([]byte("version: 1\nusers:\n"+user("ops", "editor", hashAt(600000))+user("watch", "viewer", hashAt(600000))), Options{})
		require.NoError(t, err)
		assert.Equal(t, 2, p.Users.Len())
	})
}

func TestRequiredIterations(t *testing.T) {
	doc := func(iter int) []byte {
		return []byte("version: 1\nusers:\n  - name: ops\n    role: editor\n    password: \"" + hashAt(iter) + "\"\n")
	}
	_, err := Parse(doc(auth.Iterations), Options{})
	require.NoError(t, err)
	for _, n := range []int{700000, 599999} {
		_, err := Parse(doc(n), Options{})
		var es Errors
		require.ErrorAs(t, err, &es)
		msg := err.Error()
		assert.Contains(t, msg, "users[0].password")
		assert.Contains(t, msg, `"ops"`)
		assert.Contains(t, msg, "600000")
		assert.Contains(t, msg, strconv.Itoa(n))
		assert.NotContains(t, msg, hashAt(n))
	}
	_, err = Parse(doc(1000), Options{RequiredIterations: 1000})
	require.NoError(t, err)
	_, err = Parse(doc(600000), Options{RequiredIterations: 1000})
	require.ErrorContains(t, err, `users[0].password: user "ops": the hash has 600000 iterations, want 1000`)
}

func TestParseCollectsEveryError(t *testing.T) {
	text := "version: 1\nserver:\n  rtmp:\n    listen: nowhere\nusers:\n" +
		"  - name: ops\n    role: editor\n    password: \"" + hashAt(1000) + "\"\n" +
		"  - name: watch\n    role: admin\n    password: \"" + hashAt(1000) + "\"\n" +
		"streams:\n  - name: Bad Name\n    inputs: [{url: rtmp://a/live/x}]\n  - name: noinput\n"
	_, err := Parse([]byte(text), testOpt)
	var es Errors
	require.ErrorAs(t, err, &es)
	require.Len(t, es, 4, err.Error())
	assert.Contains(t, es[0].Error(), "server.rtmp.listen")
	assert.Contains(t, es[1].Error(), "users[1].role")
	assert.Contains(t, es[2].Error(), `streams[0] ("Bad Name"): line 13: name:`)
	assert.Contains(t, es[3].Error(), "streams[1] (noinput): line 15: inputs: 0 inputs")
	assert.Equal(t, strings.Join([]string{es[0].Error(), es[1].Error(), es[2].Error(), es[3].Error()}, "; "), err.Error())
}

func TestParseStreams(t *testing.T) {
	t.Run("invalid", func(t *testing.T) {
		_, err := Parse([]byte(head()+"streams:\n  - name: cam\n"), testOpt)
		require.ErrorContains(t, err, "streams[0] (cam): line 7: inputs: 0 inputs, want 1 to 8")
	})
	t.Run("duplicate", func(t *testing.T) {
		_, err := Parse([]byte(head()+"streams:\n"+streamYAML("cam")+streamYAML("cam")), testOpt)
		require.ErrorContains(t, err, `streams[1] (cam): line 10: name: "cam" is repeated (streams[0])`)
	})
	t.Run("1001 streams", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(head() + "streams:\n")
		for i := range spec.MaxStreams + 1 {
			b.WriteString(streamYAML("s" + strconv.Itoa(i)))
		}
		_, err := Parse([]byte(b.String()), testOpt)
		var es Errors
		require.ErrorAs(t, err, &es)
		require.Len(t, es, 1)
		assert.Equal(t, "streams: 1001 streams, want at most 1000", es[0].Error())
	})
	t.Run("streams not a list", func(t *testing.T) {
		_, err := Parse([]byte(head()+"streams: {name: cam}\n"), testOpt)
		var se *SyntaxError
		require.ErrorAs(t, err, &se)
	})
	t.Run("full stream equals its JSON", func(t *testing.T) {
		y := head() + `streams:
  - name: cam
    disabled: true
    inputs:
      - url: rtmp://user:pw@src.example/live/cam
      - url: srt://backup.example:9000?streamid=cam
    layout: [video, audio]
    transcode:
      renditions:
        - label: src
          copy: true
        - label: hd
          height: 720
          bitrate_kbps: 2500
          max_bitrate_kbps: 3000
          b_frames: 2
          preset: veryfast
          encoder: x264
      audio:
        bitrate_kbps: 128
        sample_rate: 48000
        channels: 2
      watermark:
        text: "On air %H:%M"
        position: br
        opacity_pct: 80
        size_pct: 5
        time_zone: Asia/Ho_Chi_Minh
    thumbnail:
      interval_sec: 10
      height: 180
      quality: 5
    outputs:
      hls: true
      dash: true
      httpts: true
      rtmp: true
      rtsp: true
      srt: true
      push:
        - url: rtmp://cdn.example/live/key1
        - url: rtmp://cdn2.example/live/key2
`
		j := `{"name":"cam","disabled":true,
"inputs":[{"url":"rtmp://user:pw@src.example/live/cam"},{"url":"srt://backup.example:9000?streamid=cam"}],
"layout":["video","audio"],
"transcode":{"renditions":[{"label":"src","copy":true},
 {"label":"hd","height":720,"bitrate_kbps":2500,"max_bitrate_kbps":3000,"b_frames":2,"preset":"veryfast","encoder":"x264"}],
 "audio":{"bitrate_kbps":128,"sample_rate":48000,"channels":2},
 "watermark":{"text":"On air %H:%M","position":"br","opacity_pct":80,"size_pct":5,"time_zone":"Asia/Ho_Chi_Minh"}},
"thumbnail":{"interval_sec":10,"height":180,"quality":5},
"outputs":{"hls":true,"dash":true,"httpts":true,"rtmp":true,"rtsp":true,"srt":true,
 "push":[{"url":"rtmp://cdn.example/live/key1"},{"url":"rtmp://cdn2.example/live/key2"}]}}`
		p := parseOK(t, y)
		want, err := spec.Decode(strings.NewReader(j))
		require.NoError(t, err)
		require.Len(t, p.Doc.Streams, 1)
		assert.Equal(t, want, p.Doc.Streams[0])
	})
}

// lenientText is the fixture of TestParseLenientSkipsBadStreams: a padded
// header so the unknown-key entry is past line 40, then five streams, the
// unknown-key one in block style, in the library's own formatting, under a
// two-line head comment.
func lenientText() string {
	var b strings.Builder
	b.WriteString(head())
	for i := range 30 {
		fmt.Fprintf(&b, "# padding %d\n", i)
	}
	b.WriteString("streams:\n")                                                              // line 36
	b.WriteString(streamYAML("cam"))                                                         // 37-39
	b.WriteString("  # the odd one\n  # has a typo\n")                                       // 40-41
	b.WriteString(streamYAML("odd", "    outputs:", "      hls: true", "      colour: red")) // 42-47, colour on 47
	b.WriteString("  - name: empty\n")                                                       // 48
	b.WriteString(streamYAML("cam"))                                                         // 49-51
	b.WriteString(streamYAML("other"))                                                       // 52-54
	return b.String()
}

func TestParseLenientSkipsBadStreams(t *testing.T) {
	text := lenientText()
	require.Equal(t, "      colour: red", strings.Split(text, "\n")[46])

	p, skipped, err := ParseLenient([]byte(text), testOpt)
	require.NoError(t, err)
	require.Len(t, p.Doc.Streams, 2)
	assert.Equal(t, "cam", p.Doc.Streams[0].Name)
	assert.Equal(t, "other", p.Doc.Streams[1].Name)
	assert.Equal(t, 1, p.Users.Len())
	require.Len(t, skipped, 3)
	assert.Equal(t, "streams[1] (odd): line 47: field colour not found in type spec.Outputs", skipped[0].Error())
	assert.Equal(t, "streams[2] (empty): line 48: inputs: 0 inputs, want 1 to 8", skipped[1].Error())
	assert.Equal(t, `streams[3] (cam): line 49: name: "cam" is repeated (streams[0])`, skipped[2].Error())

	_, err = Parse([]byte(text), testOpt)
	var es Errors
	require.ErrorAs(t, err, &es)
	require.Len(t, es, 3)
	for i := range es {
		assert.Equal(t, skipped[i].Error(), es[i].Error())
	}

	t.Run("a bad server or user is err", func(t *testing.T) {
		bad := strings.Replace(text, "version: 1\n", "version: 1\nserver:\n  srt:\n    latency_ms: -1\n", 1)
		p, skipped, err := ParseLenient([]byte(bad), testOpt)
		require.ErrorContains(t, err, "server.srt.latency_ms")
		assert.Empty(t, p.Doc.Streams)
		assert.Nil(t, p.Users)
		assert.Nil(t, skipped)
		bad = strings.Replace(text, "role: editor", "role: admin", 1)
		p, _, err = ParseLenient([]byte(bad), testOpt)
		require.ErrorContains(t, err, "users[0].role")
		assert.Empty(t, p.Doc.Streams)
	})

	t.Run("node checks skip entries", func(t *testing.T) {
		text := head() + "streams:\n" +
			streamYAML("cam") + // 7-9
			streamYAML("zero", // 10-17
				"    transcode:",
				"      renditions:",
				"        - label: hd",
				"          height: 0720",
				"          bitrate_kbps: 800") +
			"  - &x {name: anchored, inputs: [{url: rtmp://a/live/x}]}\n" + // 18
			"  - *x\n" + // 19
			streamYAML("other") // 20-22
		p, skipped, err := ParseLenient([]byte(text), testOpt)
		require.NoError(t, err)
		require.Len(t, p.Doc.Streams, 2)
		assert.Equal(t, "cam", p.Doc.Streams[0].Name)
		assert.Equal(t, "other", p.Doc.Streams[1].Name)
		require.Len(t, skipped, 3)
		assert.Equal(t, "streams[1] (zero): line 16: write the number as a plain decimal integer, or quote it if it is text", skipped[0].Error())
		assert.Equal(t, "streams[2] (anchored): line 18: anchors, aliases and merge keys are not supported", skipped[1].Error())
		assert.Equal(t, "streams[3]: line 19: anchors, aliases and merge keys are not supported", skipped[2].Error())
	})

	t.Run("node checks outside streams are err", func(t *testing.T) {
		for name, text := range map[string]string{
			"number in server":       head() + "server:\n  srt:\n    latency_ms: 0120\n",
			"anchor on a user":       "version: 1\nusers:\n  - &u\n    name: ops\n    role: editor\n    password: \"" + hashAt(1000) + "\"\n",
			"anchor on the sequence": head() + "streams: &s\n" + streamYAML("cam"),
			"anchor on the root":     "--- &r\n" + head(),
		} {
			t.Run(name, func(t *testing.T) {
				p, skipped, err := ParseLenient([]byte(text), testOpt)
				var es Errors
				require.ErrorAs(t, err, &es)
				assert.Contains(t, err.Error(), "line ")
				assert.Empty(t, p.Doc.Streams)
				assert.Nil(t, skipped)
			})
		}
	})
}

func TestHash(t *testing.T) {
	text := []byte(head())
	h := Hash(text)
	assert.Regexp(t, `^"[0-9a-f]{64}"$`, h)
	assert.Equal(t, h, Hash([]byte(head())))
	assert.NotEqual(t, h, Hash(append([]byte("#\n"), text...)))
}
