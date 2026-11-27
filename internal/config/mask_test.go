package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// The secrets of secretText. They appear nowhere else, so a leak scan can
// search for them.
const (
	camPassword = "hunter2-pw"
	camPushKey  = "pushkey-0123456789"
	camSRTPass  = "srtpass-0123456789"
	dvrPassword = "tlJwpbo6"
)

// secretText is a valid config with three streams: cam holds a password in
// its input, a key in its push and a passphrase; dvr holds a password in
// its input's path; plain holds nothing to hide. It has comments above
// entries, blank lines and, in dvr, 4-space sequences.
func secretText() string {
	return head() + `
# the cameras
streams:
  # the front door
  - name: cam
    inputs:
      - url: rtsp://admin:hunter2-pw@192.0.2.1/s
    outputs:
      hls: true
      srt: true
      srt_passphrase: srtpass-0123456789
      push:
        - url: rtmp://h/live/pushkey-0123456789

  # the recorder
  - name: dvr
    inputs:
        - url: rtsp://192.0.2.2/user=admin_password=tlJwpbo6_channel=1
    outputs:
        hls: true

  - name: plain
    inputs:
      - url: udp://239.0.0.1:1234
`
}

// maskedText is secretText as Mask shows it.
func maskedText() string {
	r := strings.NewReplacer(
		"rtsp://admin:hunter2-pw@192.0.2.1/s", "rtsp://admin:xxxxx@192.0.2.1/s",
		"srt_passphrase: srtpass-0123456789", "srt_passphrase: xxxxx",
		"rtmp://h/live/pushkey-0123456789", "rtmp://h/live/puxxxxx",
		"user=admin_password=tlJwpbo6_channel=1", "user=admin_password=xxxxx_channel=1",
	)
	return r.Replace(secretText())
}

// refusal is R-T4's text for the field of a stream entry.
func refusal(ref, field string) string {
	return ref + ": " + field + ": holds the redaction mask and the stored stream has no one secret it stands for; send the real value"
}

// sameSlice reports whether a and b are one slice, not two equal ones.
func sameSlice(a, b []byte) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// leak fails when any secret is a substring of out.
func leak(t *testing.T, out []byte, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("the secret %q is in the output", s)
		}
	}
}

// checkMask is what holds for every text Mask accepts: no error, no
// secret, the postcondition on the bytes, Mask of the result is the
// result, and Unmask of the result against the text is the text itself.
func checkMask(t *testing.T, text string, secrets ...string) []byte {
	t.Helper()
	in := []byte(text)
	out, err := Mask(in)
	require.NoError(t, err)
	leak(t, out, secrets...)
	root, err := decodeNode(out)
	require.NoError(t, err)
	assert.True(t, rawClean(out, root), "the output holds a URL with something to hide:\n%s", out)
	again, err := Mask(out)
	require.NoError(t, err)
	assert.Equal(t, string(out), string(again), "Mask of Mask's output")
	back, err := Unmask(out, in)
	require.NoError(t, err)
	assert.True(t, sameSlice(back, in), "Unmask(Mask(t), t) is not t itself")
	return out
}

// unmaskErrors is the texts of the Errors that Unmask returns, with
// nothing else.
func unmaskErrors(t *testing.T, sent, stored string) []string {
	t.Helper()
	out, err := Unmask([]byte(sent), []byte(stored))
	require.Error(t, err)
	assert.Nil(t, out)
	var es Errors
	require.ErrorAs(t, err, &es)
	texts := make([]string, len(es))
	for i, e := range es {
		texts[i] = e.Error()
	}
	return texts
}

// stream is the parsed stream name of text, which must pass Parse.
func stream(t *testing.T, text []byte, name string) spec.Stream {
	t.Helper()
	p, err := Parse(text, testOpt)
	require.NoError(t, err)
	for _, s := range p.Doc.Streams {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no stream %q", name)
	return spec.Stream{}
}

func TestMask(t *testing.T) {
	in := []byte(secretText())
	parseOK(t, secretText())
	out := checkMask(t, secretText(), camPassword, camPushKey, camSRTPass, dvrPassword)
	assert.Equal(t, maskedText(), string(out))

	// It differs only inside the four scalars.
	a, b := strings.Split(secretText(), "\n"), strings.Split(string(out), "\n")
	require.Len(t, b, len(a))
	changed := 0
	for i := range a {
		if a[i] != b[i] {
			changed++
		}
	}
	assert.Equal(t, 4, changed)

	// It decodes to the same document but for those values.
	var want, got Document
	require.NoError(t, yaml.Unmarshal(in, &want))
	require.NoError(t, yaml.Unmarshal(out, &got))
	require.Len(t, got.Streams, 3)
	for i := range want.Streams {
		want.Streams[i] = want.Streams[i].Redacted()
	}
	assert.Equal(t, want, got)
	assert.Equal(t, "rtsp://admin:xxxxx@192.0.2.1/s", got.Streams[0].Inputs[0].URL)
	assert.Equal(t, "rtmp://h/live/puxxxxx", got.Streams[0].Outputs.Push[0].URL)
	assert.Equal(t, redact.Mask, got.Streams[0].Outputs.SRTPassphrase)
	assert.Equal(t, "rtsp://192.0.2.2/user=admin_password=xxxxx_channel=1", got.Streams[1].Inputs[0].URL)
	assert.Equal(t, "udp://239.0.0.1:1234", got.Streams[2].Inputs[0].URL)

	// The users section, the hash included, is untouched.
	assert.True(t, bytes.HasPrefix(out, []byte(head())))

	t.Run("nothing to hide", func(t *testing.T) {
		for name, text := range map[string]string{
			"no stream":      head(),
			"a udp input":    head() + "streams:\n  - name: plain\n    inputs:\n      - url: udp://239.0.0.1:1234\n",
			"a scheme upper": head() + "streams:\n  - name: plain\n    inputs:\n      - url: UDP://239.0.0.1:1234\n",
			"a comment":      head() + "# see https://example.com/docs#streams\n",
		} {
			in := []byte(text)
			out, err := Mask(in)
			require.NoError(t, err, name)
			assert.True(t, sameSlice(in, out), name)
		}
	})

	t.Run("passphrases", func(t *testing.T) {
		text := head() + "streams:\n" +
			streamYAML("a", "    outputs:", "      srt: true", `      srt_passphrase: ""`) +
			streamYAML("b", "    outputs:", "      srt: true", "      srt_passphrase:") +
			streamYAML("c", "    outputs:", "      srt: true", "      srt_passphrase: ~") +
			streamYAML("d", "    outputs:", "      srt: true", "      srt_passphrase: 1234567890") +
			streamYAML("e", "    outputs:", "      srt: true", `      srt_passphrase: "quoted-0123456789" # the key`) +
			streamYAML("f", "    outputs:", "      srt: true", "      srt_passphrase: 'single-0123456789'")
		out := string(checkMask(t, text, "1234567890", "quoted-0123456789", "single-0123456789"))
		assert.Contains(t, out, "      srt_passphrase: \"\"\n")
		assert.Contains(t, out, "      srt_passphrase:\n")
		assert.Contains(t, out, "      srt_passphrase: ~\n")
		assert.Contains(t, out, "      srt_passphrase: xxxxx\n")
		assert.Contains(t, out, "      srt_passphrase: \"xxxxx\" # xxxxx\n")
		assert.Contains(t, out, "      srt_passphrase: 'xxxxx'\n")
		for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
			text = strings.Replace(text, "/live/"+name+"\n", "/live/xxxxx\n", 1)
		}
		text = strings.NewReplacer(": 1234567890\n", ": xxxxx\n", "quoted-0123456789", "xxxxx", "single-0123456789", "xxxxx", "# the key", "# xxxxx").Replace(text)
		assert.Equal(t, text, out)
	})

	t.Run("a passphrase behind an alias", func(t *testing.T) {
		text := head() + "x: &p alias-0123456789\nstreams:\n" +
			streamYAML("a", "    outputs:", "      srt: true", "      srt_passphrase: *p")
		checkMask(t, text, "alias-0123456789")
	})

	t.Run("not YAML", func(t *testing.T) {
		for _, text := range []string{"", "# nothing\n", "a: [\n", "a: 1\n---\nb: 2\n", "a: \"rtsp://u:pw@h/s\n"} {
			out, err := Mask([]byte(text))
			require.Error(t, err, text)
			assert.Nil(t, out)
			assert.NotContains(t, err.Error(), ":pw@")
			var se *SyntaxError
			assert.ErrorAs(t, err, &se)
		}
	})
}

// Review Focus 1.
func TestMaskUnmaskRoundTrip(t *testing.T) {
	stored := []byte(secretText())
	shown, err := Mask(stored)
	require.NoError(t, err)

	back, err := Unmask(shown, stored)
	require.NoError(t, err)
	assert.True(t, sameSlice(back, stored))

	// An edit of the masked text: a fourth stream, and plain's input.
	const fourth = "\n  - name: four\n    inputs:\n      - url: udp://239.0.0.4:1234\n"
	edit := func(s string) string {
		s = strings.Replace(s, "udp://239.0.0.1:1234", "udp://239.0.0.2:5000", 1)
		return s + fourth
	}
	sent := edit(string(shown))
	require.NotEqual(t, string(shown), sent)
	got, err := Unmask([]byte(sent), stored)
	require.NoError(t, err)
	assert.Equal(t, edit(secretText()), string(got))
	parseOK(t, string(got))

	// Without a mask the text is the sender's, whatever is stored.
	for _, text := range []string{secretText(), head(), "not: [yaml"} {
		in := []byte(text)
		out, err := Unmask(in, stored)
		require.NoError(t, err)
		assert.True(t, sameSlice(in, out))
	}

	// A text that is not YAML is the sender's too: Parse reports it.
	bad := []byte("streams: [ xxxxx\n")
	out, err := Unmask(bad, stored)
	require.NoError(t, err)
	assert.True(t, sameSlice(bad, out))

	// A mask where Unmask does not look is stored as sent.
	note := []byte(head() + "# was rtsp://u:xxxxx@h/s\nstreams: []\n")
	out, err = Unmask(note, stored)
	require.NoError(t, err)
	assert.True(t, sameSlice(note, out))
}

// camDoc is a config whose streams are cam, with the lines given (indented
// by 4), and other.
func camDoc(lines ...string) string {
	return head() + "streams:\n  - name: cam\n" + strings.Join(lines, "\n") + "\n" + streamYAML("other")
}

// oddEdit is an edit of another stream than cam: other gains an output.
// nl is the text's line end.
func oddEdit(text, nl string) string {
	return text + "    outputs:" + nl + "      hls: true" + nl
}

// Review Focus 4.
func TestMaskOddYAML(t *testing.T) {
	const otherKey = "/live/other"

	t.Run("in place", func(t *testing.T) {
		crlf := strings.ReplaceAll(camDoc("    inputs:", "      - url: rtsp://u:pw@h/s"), "\n", "\r\n")
		for _, c := range []struct {
			name, text, nl string
			secrets        []string
		}{
			{"single quotes", camDoc("    inputs:", "      - url: 'rtsp://u:pw@h/s'"), "\n", []string{":pw@"}},
			{"a single quote inside single quotes", camDoc("    inputs:", "      - url: 'rtsp://u:it''s@h/s'"), "\n", []string{"it''s"}},
			{"double quotes", camDoc("    inputs:", `      - url: "rtsp://u:pw@h/s"`), "\n", []string{":pw@"}},
			{"two URLs on one line", camDoc("    inputs: [{url: rtsp://u:pw@h/a}, {url: rtsp://u:pw2@h/b}]"), "\n", []string{":pw@", ":pw2@"}},
			{"a line comment", camDoc("    inputs:", "      - url: rtsp://u:pw@h/s # the door"), "\n", []string{":pw@"}},
			{"CRLF", crlf, "\r\n", []string{":pw@"}},
			{
				"a non-ASCII text on the line before",
				camDoc("    transcode:", "      renditions:", "        - label: hd", "          height: 720", "          bitrate_kbps: 800",
					"      watermark:", `        text: "Trực tiếp ở cửa trước"`, "    inputs:", "      - url: rtsp://u:pw@h/s"),
				"\n",
				[]string{":pw@"},
			},
			{
				"a non-ASCII text on the same line",
				camDoc(`    transcode: {renditions: [{label: hd, height: 720, bitrate_kbps: 800}], watermark: {text: "Trực tiếp"}}`+
					", inputs: [{url: rtsp://u:pw@h/s}]") + "",
				"\n",
				[]string{":pw@"},
			},
			{"a byte order mark", "\xEF\xBB\xBF" + camDoc("    inputs:", "      - url: rtsp://u:pw@h/s"), "\n", []string{":pw@"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				if c.name == "a non-ASCII text on the same line" {
					// One flow mapping holds the whole entry.
					c.text = head() + "streams:\n" +
						`  - {name: cam, transcode: {renditions: [{label: hd, height: 720, bitrate_kbps: 800}], watermark: {text: "Trực tiếp"}}, inputs: [{url: rtsp://u:pw@h/s}]}` +
						"\n" + streamYAML("other")
				}
				out := checkMask(t, c.text, append(c.secrets, otherKey)...)
				// Only the secrets' bytes changed.
				assert.Len(t, strings.Split(string(out), "\n"), len(strings.Split(c.text, "\n")))
				assert.Equal(t, c.nl == "\r\n", bytes.Contains(out, []byte("\r\n")))

				got, err := Unmask([]byte(oddEdit(string(out), c.nl)), []byte(c.text))
				require.NoError(t, err)
				assert.Equal(t, oddEdit(c.text, c.nl), string(got), "only the edit's bytes differ")
				assert.Equal(t, spec.ChangeNone, spec.Diff(stream(t, []byte(c.text), "cam"), stream(t, got, "cam")))
			})
		}
	})

	t.Run("re-encoded", func(t *testing.T) {
		for _, c := range []struct {
			name, text string
			secrets    []string
		}{
			{"a hex escape in the password", camDoc("    inputs:", `      - url: "rtsp://u:p\x77@h/s"`), []string{":pw@", `p\x77`}},
			{"a folded scalar", camDoc("    inputs:", "      - url: >-", "          rtsp://u:pw@h/s"), []string{":pw@"}},
			{"a line continuation", camDoc("    inputs:", "      - url: \"rtsp://u:p\\", "          w@h/s\""), []string{":pw@", `u:p\`}},
			{"a literal scalar", camDoc("    inputs:", "      - url: |-", "          rtsp://u:pw@h/s"), []string{":pw@"}},
			{"a tag", camDoc("    inputs:", "      - url: !!str rtsp://u:pw@h/s"), []string{":pw@"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				want := stream(t, []byte(c.text), "cam")
				out := checkMask(t, c.text, append(c.secrets, otherKey)...)
				assert.Contains(t, string(out), "rtsp://u:xxxxx@h")

				got, err := Unmask([]byte(oddEdit(string(out), "\n")), []byte(c.text))
				require.NoError(t, err)
				assert.Equal(t, spec.ChangeNone, spec.Diff(want, stream(t, got, "cam")))
				assert.True(t, stream(t, got, "other").Outputs.HLS)
				assert.Equal(t, "rtmp://src.example/live/other", stream(t, got, "other").Inputs[0].URL)
			})
		}
	})

	// Finding 9: Mask does not depend on the parse refusing these.
	t.Run("anchors", func(t *testing.T) {
		for _, c := range []struct {
			name, text string
			want       string
		}{
			{
				"an anchored URL and its alias",
				head() + "streams:\n  - name: cam\n    inputs:\n      - url: &u rtsp://u:pw@h/s\n  - name: two\n    inputs:\n      - url: *u\n",
				"&u rtsp://u:xxxxx@h/s",
			},
			{
				"an anchored input and its alias",
				head() + "streams:\n  - name: cam\n    inputs:\n      - &in\n        url: rtsp://u:pw@h/s\n  - name: two\n    inputs:\n      - *in\n",
				"url: rtsp://u:xxxxx@h/s",
			},
			{
				"a merge key that carries a url",
				head() + "base: &base\n  url: rtsp://u:pw@h/s\nstreams:\n  - name: cam\n    inputs:\n      - <<: *base\n",
				"url: rtsp://u:xxxxx@h/s",
			},
			{
				"an anchored double-quoted URL with an escape",
				head() + "streams:\n  - name: cam\n    inputs:\n      - url: &u \"udp://h:1234\\t\"\n      - url: *u # was rtsp://u:pw@h/s\n",
				"# was rtsp://u:xxxxx@h/s",
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				_, err := Parse([]byte(c.text), testOpt)
				require.ErrorContains(t, err, "anchors, aliases and merge keys are not supported")
				out := checkMask(t, c.text, ":pw@")
				assert.Contains(t, string(out), c.want)
				assert.Contains(t, string(out), "*")
			})
		}
	})

	// Finding 2: a token inside a scalar is masked, and the scalar is
	// refused when the text comes back edited.
	t.Run("tokens inside scalars", func(t *testing.T) {
		for _, c := range []struct {
			name, line, want, field string
		}{
			// Under a url key a value that is no URL as a whole is the bare
			// mask, as the stream API shows it; under another key it is
			// text (TestMaskWholeValue).
			{"after other text", "      - url: see rtsp://u:pw@h/s now", "      - url: xxxxx\n", "inputs[0].url"},
			{"glued to its key", "      - url:rtsp://u:pw@h/s", "      - url:rtsp://u:xxxxx@h/s\n", "inputs[0]"},
			{"as a key", "      - rtsp://u:pw@h/s: x", "      - rtsp://u:xxxxx@h/s: x\n", "inputs[0]"},
		} {
			t.Run(c.name, func(t *testing.T) {
				text := camDoc("    inputs:", c.line)
				out := checkMask(t, text, ":pw@")
				assert.Contains(t, string(out), c.want)
				assert.Equal(t, []string{refusal("streams[0] (cam)", c.field)}, unmaskErrors(t, oddEdit(string(out), "\n"), text))
			})
		}
	})

	// Finding 2: a token redact.URL cannot parse hides its scalar.
	t.Run("tokens that do not parse", func(t *testing.T) {
		for _, c := range []struct{ line, want, secret string }{
			{"      - url: rtsp://admin:p%ss@cam/s", "      - url: xxxxx\n", "p%ss"},
			{`      - url: "rtsp://admin:pa#ss@cam/s"`, "      - url: \"xxxxx\"\n", "pa#ss"},
			{"      - url: rtsp://u:pw@h:port/s", "      - url: xxxxx\n", ":pw@"},
			{"      - url: 'see rtsp://u:pw@h:port/s now'", "      - url: 'xxxxx'\n", ":pw@"},
		} {
			t.Run(c.line, func(t *testing.T) {
				text := camDoc("    inputs:", c.line)
				out := checkMask(t, text, c.secret, "admin", "cam/s", "port")
				assert.Contains(t, string(out), c.want)
				assert.NotContains(t, string(out), redact.Invalid)
				assert.Equal(t, []string{refusal("streams[0] (cam)", "inputs[0].url")}, unmaskErrors(t, oddEdit(string(out), "\n"), text))
			})
		}
	})

	t.Run("a mistyped key", func(t *testing.T) {
		text := camDoc("    inptus: [{url: rtsp://u:pw@h/s}]")
		out := checkMask(t, text, ":pw@")
		assert.Contains(t, string(out), "    inptus: [{url: rtsp://u:xxxxx@h/s}]\n")
		// Not resolved on the way in: the text is stored as sent but for
		// other's input, and Parse refuses the key.
		got, err := Unmask([]byte(oddEdit(string(out), "\n")), []byte(text))
		require.NoError(t, err)
		assert.Contains(t, string(got), "    inptus: [{url: rtsp://u:xxxxx@h/s}]\n")
		assert.Contains(t, string(got), "rtmp://src.example/live/other\n")
		_, err = Parse(got, testOpt)
		require.ErrorContains(t, err, "inptus")
	})

	t.Run("an entry the lenient start skipped", func(t *testing.T) {
		text := camDoc("    layout: [video, bogus]", "    inputs:", "      - url: rtsp://u:pw@h/s", "    outputs:", "      push:", "        - url: rtmp://h/live/skipped-key")
		_, skipped, err := ParseLenient([]byte(text), testOpt)
		require.NoError(t, err)
		require.Len(t, skipped, 1)
		out := checkMask(t, text, ":pw@", "skipped-key")
		got, err := Unmask([]byte(oddEdit(string(out), "\n")), []byte(text))
		require.NoError(t, err)
		assert.Equal(t, oddEdit(text, "\n"), string(got))
	})

	t.Run("a key the raw scan cannot read", func(t *testing.T) {
		// As keys, these are followed by ':' in the text, which makes the
		// raw token another URL: they are written in double quotes.
		for _, key := range []string{"rtmp://h/live/pushkey-0123456789", "https://cdn/x.m3u8?token=abc123tok"} {
			text := camDoc("    inputs:", "      - "+key+": x")
			out := checkMask(t, text, camPushKey, "abc123tok")
			assert.Contains(t, string(out), `- "`+redact.URL(key)+`": x`)
		}
	})

	t.Run("a URL in a tag", func(t *testing.T) {
		text := camDoc("    inputs:", "      - url: !rtsp://u:pw@h/s x")
		out := checkMask(t, text, ":pw@")
		assert.Contains(t, string(out), "- url: x\n")
	})

	t.Run("UTF-16", func(t *testing.T) {
		for _, text := range []string{
			head() + "# was rtsp://u:pw@h/s\nstreams: []\n",
			head() + "streams: [ # was rtsp://u:pw@h/s\n  ]\n",
			camDoc("    inputs:", "      - url: rtsp://u:pw@h/s"),
			head(),
		} {
			in := []byte{0xFF, 0xFE}
			for _, u := range utf16.Encode([]rune(text)) {
				in = append(in, byte(u), byte(u>>8))
			}
			_, err := decodeNode(in)
			require.NoError(t, err)
			out, err := Mask(in)
			require.NoError(t, err)
			require.False(t, bytes.HasPrefix(out, []byte{0xFF, 0xFE}), "the output is UTF-8")
			leak(t, out, ":pw@")
			wide := make([]byte, 0, 8)
			for _, u := range utf16.Encode([]rune(":pw@")) {
				wide = append(wide, byte(u), byte(u>>8))
			}
			leak(t, out, string(wide))
			again, err := Mask(out)
			require.NoError(t, err)
			assert.Equal(t, string(out), string(again))
		}
	})

	t.Run("a byte order mark before the first line", func(t *testing.T) {
		out := checkMask(t, "\xEF\xBB\xBFnote: rtsp://u:pw@h/s\n"+head(), ":pw@")
		assert.Equal(t, "\xEF\xBB\xBFnote: rtsp://u:xxxxx@h/s\n"+head(), string(out))
	})

	t.Run("line breaks the URL pattern reads through", func(t *testing.T) {
		// After NEL, LS or PS a '#' starts a comment for the parser, and
		// the raw token before it runs on into it.
		for _, br := range []string{"\u0085", " ", " "} {
			text := head() + "note: udp://h:1/" + br + "#rtsp://u:pw@h/s\nstreams: [ # x" + br + "#rtsp://u:pw@h/s\n  ]\n"
			_, err := decodeNode([]byte(text))
			require.NoError(t, err)
			checkMask(t, text, ":pw@")
		}
	})

	for _, c := range punctuationRows() {
		t.Run("punctuation: "+c.name, func(t *testing.T) {
			checkMask(t, c.text, c.secrets...)
		})
	}
}

func TestMaskComments(t *testing.T) {
	text := head() + `streams:
  # old: rtmp://h/live/oldkey-0123456789 (the first platform)
  - name: cam
    inputs:
      - url: udp://239.0.0.1:1234   # was rtsp://admin:linepw-1@192.0.2.1/s, see https://wiki.example/cam
    # and "rtsp://admin:footpw-1@192.0.2.1/s", in quotes
# the end: srt://h:9000?passphrase=endpass-0123456789
`
	want := head() + `streams:
  # old: rtmp://h/live/olxxxxx
  - name: cam
    inputs:
      - url: udp://239.0.0.1:1234   # was rtsp://admin:xxxxx@192.0.2.1/s, see https://wiki.example/cam
    # and "rtsp://admin:xxxxx@192.0.2.1/s%22, in quotes
# the end: srt://h:9000?passphrase=xxxxx
`
	// The words after the RTMP URL go with its key: a stream name may hold
	// a space, and the rest of a comment's line is read as a scalar's
	// whole value is.
	// A token is redacted as a log line's is: the quote and the comma
	// after the third are part of it, and redact.URL escapes the quote.
	out := checkMask(t, text, "oldkey-0123456789", "linepw-1", "footpw-1", "endpass-0123456789")
	assert.Equal(t, want, string(out))

	// An edited round trip stores the comments as sent.
	edit := func(s string) string { return strings.Replace(s, "239.0.0.1", "239.0.0.2", 1) }
	got, err := Unmask([]byte(edit(string(out))), []byte(text))
	require.NoError(t, err)
	assert.Equal(t, edit(want), string(got))

	// Finding 3: comments the parser drops.
	t.Run("dropped", func(t *testing.T) {
		for name, text := range map[string]string{
			"after a flow sequence's bracket":  head() + "streams:\n  - name: cam\n    inputs: [ # old rtsp://u:s1@h/s\n      {url: udp://239.0.0.1:1234},\n      {url: udp://239.0.0.2:1234}]\n",
			"between a flow mapping's entries": head() + "streams:\n  - {name: cam, # old rtsp://u:s1@h/s\n     inputs: [{url: udp://239.0.0.1:1234}]}\n",
			"inside a flow sequence, quoted":   head() + "streams:\n  - name: cam\n    inputs: [ # old \"rtsp://u:s1@h/s\"\n      {url: \"udp://239.0.0.1:1234\"}]\n",
			"with a secret beside it":          head() + "streams:\n  - name: cam\n    inputs: [ # old rtsp://u:s1@h/s\n      {url: rtsp://u:s2@h/s}]\n",
			// A '#' needs no white space before it after a quote or a
			// bracket: to the URL pattern these comments are the tail of
			// the key's token, and to redact.URL its fragment.
			"glued to a double-quoted key": head() + "streams:\n  - {name: cam, \"k://h\":[#rtsp://u:s1@h/s\n      1]}\n",
			"glued to a single-quoted key": head() + "streams:\n  - {name: cam, 'k://h/':[#rtsp://u:s1@h/s\n      1]}\n",
		} {
			t.Run(name, func(t *testing.T) {
				out := checkMask(t, text, ":s1@", ":s2@")
				root, err := decodeNode(out)
				require.NoError(t, err)
				seq := streamsNode(root)
				require.NotNil(t, seq)
				require.Len(t, seq.Content, 1)
				name, _ := entryName(seq.Content[0])
				assert.Equal(t, "cam", name)
			})
		}
		// The first is one the parser drops: it is gone.
		out := checkMask(t, head()+"streams:\n  - name: cam\n    inputs: [ # old rtsp://u:s1@h/s\n      {url: udp://239.0.0.1:1234}]\n", ":s1@")
		assert.NotContains(t, string(out), "old")
	})

	t.Run("after a '#' inside a token", func(t *testing.T) {
		// A comment the parser reports, glued to a scalar.
		out := checkMask(t, head()+"note: 'b://h'#rtsp://u:s1@h/s\n", ":s1@")
		assert.Contains(t, string(out), "#rtsp://u:xxxxx@h/s\n")
		// A URL in a comment whose fragment is a URL.
		out = checkMask(t, head()+"# see https://docs.example/x#rtsp://u:s1@h/s and https://docs.example/x#top\n", ":s1@")
		assert.Equal(t, head()+"# see https://docs.example/x#rtsp://u:xxxxx@h/s and https://docs.example/x#top\n", string(out))
		// The URL after the '#' is redacted before the one it is in: here
		// redacting the outer one takes the inner one's "://" away, and
		// what is left of it would no longer be read as a URL.
		out = checkMask(t, head()+"# A://#psw=0000#srt://h:9000?streamid=publish:cam:u:inner-secret\n", "inner-secret", "0000")
		assert.Equal(t, head()+"# a://#psw=xxxxx?streamid=publish:cam:u:xxxxx\n", string(out))
		out = checkMask(t, head()+"# see https://docs.example/x#rtsp://admin:p%ss@cam/s\n", "p%ss")
		assert.Equal(t, head()+"# see <invalid url>\n", string(out))
	})

	t.Run("a token that does not parse", func(t *testing.T) {
		out := checkMask(t, head()+"# rtsp://admin:p%ss@cam/s\nstreams: [] # \"rtsp://u:pw@h:port/s\"\n", "p%ss", "cam/s", ":pw@", "port")
		assert.Equal(t, head()+"# <invalid url>\nstreams: [] # \"<invalid url>\n", string(out))
	})
}

// punctuationRow is a text in which a URL touches YAML's punctuation.
type punctuationRow struct {
	name, text string
	secrets    []string // none: the text has nothing to hide
}

// oneStream is a config with the stream cam alone, with the lines given.
func oneStream(lines ...string) string {
	return head() + "streams:\n  - name: cam\n" + strings.Join(lines, "\n") + "\n"
}

// jsonConfig is a whole config in JSON form with one input.
func jsonConfig(url string) string {
	return `{"version": 1, "users": [{"name": "ops", "role": "editor", "password": "` + hashAt(1000) +
		`"}], "streams": [{"name": "cam", "inputs": [{"url": "` + url + `"}], "outputs": {"hls": true}}]}` + "\n"
}

// punctuationRows are round 2's finding 1.
func punctuationRows() []punctuationRow {
	return []punctuationRow{
		{"double quotes, host:port", oneStream("    inputs:", `      - url: "udp://239.0.0.1:1234"`), nil},
		{"double quotes, host:port, a password", oneStream("    inputs:", `      - url: "rtsp://u:pw@h:554"`), []string{":pw@"}},
		{"single quotes", oneStream("    inputs:", "      - url: 'srt://h:9000'"), nil},
		{"single quotes, a password", oneStream("    inputs:", "      - url: 'srt://u:pw@h:9000'"), []string{":pw@"}},
		{"flow", oneStream("    inputs: [{url: udp://239.0.0.1:1234}]"), nil},
		{"flow, two passwords", oneStream("    inputs: [{url: rtsp://u:pw@h:554}, {url: rtsp://u:pw2@h:554}]"), []string{":pw@", ":pw2@"}},
		{"flow, no space after the comma", oneStream("    inputs: [{url: 'rtsp://u:pw@h:554'},{url: 'rtsp://u:pw2@h:554'}]"), []string{":pw@", ":pw2@"}},
		{"JSON", jsonConfig("udp://239.0.0.1:1234"), nil},
		{"JSON, a password", jsonConfig("rtsp://u:pw@h:554"), []string{":pw@"}},
		{"a query secret at the very end", oneStream("    inputs:", `      - url: "https://cdn/x.m3u8?token=abc123tok"`), []string{"abc123tok"}},
		{"an RTMP key at the very end", oneStream("    inputs:", "      - url: udp://239.0.0.1:1234", "    outputs:", "      push:", `        - url: "rtmp://h/live/pushkey-0123456789"`), []string{camPushKey}},
		{"an escape after the port", oneStream("    inputs:", `      - url: "udp://h:1234\t"`), nil},
		{"an escape after the port, a password", oneStream("    inputs:", `      - url: "rtsp://u:pw@h:554\t"`), []string{":pw@"}},
		{"a comment after an escape", oneStream("    inputs:", `      - url: "udp://h:1234\t" # was rtsp://u:s1@h/s`), []string{":s1@"}},
		{"a comment after an escaped quote", oneStream("    inputs:", `      - url: "udp://h:1234/\"" # was rtsp://u:s1@h/s`), []string{":s1@"}},
	}
}

func TestMaskPunctuation(t *testing.T) {
	for _, c := range punctuationRows() {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.text)
			checkMask(t, c.text, c.secrets...)
			out, err := Mask(in)
			require.NoError(t, err)
			if len(c.secrets) == 0 {
				assert.True(t, sameSlice(in, out), "a text with nothing to hide is returned as it is")
			} else {
				assert.Contains(t, string(out), redact.Mask)
			}
			_, _, err = ParseLenient(in, testOpt)
			require.NoError(t, err)
		})
	}

	// The comment after the escape is masked where it is.
	out := checkMask(t, oneStream("    inputs:", `      - url: "udp://h:1234\t" # was rtsp://u:s1@h/s`), ":s1@")
	assert.Equal(t, oneStream("    inputs:", `      - url: "udp://h:1234\t" # was rtsp://u:xxxxx@h/s`), string(out))

	// The scan's rule by itself.
	for tok, want := range map[string]bool{
		`udp://239.0.0.1:1234"`:      true,
		`srt://h:9000'`:              true,
		`udp://h:1}],`:               true,
		`udp://h:1}]`:                true,
		`rtsp://u:xxxxx@h:554"}`:     true,
		`http://h/x`:                 true,
		`HTTP://h/x`:                 true,
		`rtmp://h/live/puxxxxx'`:     true,
		`rtsp://u:s1@h/s"`:           false,
		`rtsp://u:pw@h:554"}]`:       false,
		`rtsp://admin:p%ss@cam/s`:    false,
		`udp://h:1234\t"`:            false,
		`https://h/x?token="`:        false,
		`rtmp://h/live/pushkey-01',`: false,
	} {
		assert.Equal(t, want, cleanToken(tok), tok)
		assert.Equal(t, want, cleanRaw(tok), tok)
	}
	// And what follows a '#' in a token, where a comment may start.
	for tok, want := range map[string]bool{
		`http://h/x#top`:                 true,
		`http://h/x#see-http://other/y"`: true,
		`a://h/#b://u:xxxxx@h',`:         true,
		`k://h/':[#rtsp://u:pw@h/s`:      false,
		`k://h/'#x#rtsp://u:pw@h/s`:      false,
		`k://h/'#a://b#rtsp://u:pw@c/`:   false,
	} {
		// A URL after the '#' is a URL nested in the fragment (redact.URL's
		// nested rule), so the token's own rule finds it too.
		assert.Equal(t, want, cleanToken(tok), tok)
		assert.Equal(t, want, cleanRaw(tok), tok)
	}
}

// tableTexts are the texts of the package's Parse, ParseLenient and edit
// tests: their fixtures, and their tables' rows written again here, since
// the rows live inside the test functions.
func tableTexts() map[string]string {
	ladder := head() + "streams:\n" + streamYAML("cam",
		"    transcode:",
		"      renditions:",
		"        - label: hd",
		"          height: 0720",
		"          bitrate_kbps: 800",
		"      watermark:",
		"        text: \"24.7\"")
	texts := map[string]string{
		"head":                head(),
		"one stream":          head() + "streams:\n" + streamYAML("cam"),
		"unknown top key":     head() + "colour: red\n",
		"unknown server key":  head() + "server:\n  rtmp:\n    lisen: \":1935\"\n",
		"duplicate key":       head() + "version: 1\n",
		"not a mapping":       "- 1\n",
		"anchor and alias":    head() + "streams:\n  - &cam {name: cam, inputs: [{url: rtmp://a/live/cam}]}\n  - *cam\n",
		"merge key":           head() + "streams:\n" + streamYAML("cam", "    outputs: &defaults", "      hls: true") + streamYAML("other", "    outputs:", "      <<: *defaults", "      dash: true"),
		"anchor never used":   head() + "streams:\n" + streamYAML("cam", "    outputs: &o {hls: true}"),
		"anchor on a user":    "version: 1\nusers:\n  - &u\n    name: ops\n    role: editor\n    password: \"" + hashAt(1000) + "\"\n",
		"anchor on the list":  head() + "streams: &s\n" + streamYAML("cam"),
		"anchor on the root":  "--- &r\n" + head(),
		"ladder":              ladder,
		"comment and quotes":  "# the config\nversion: 1 # one\nusers:\n  - name: ops\n    role: editor\n    password: '" + hashAt(1000) + "'\n",
		"invalid stream":      head() + "streams:\n  - name: cam\n",
		"duplicate stream":    head() + "streams:\n" + streamYAML("cam") + streamYAML("cam"),
		"streams not a list":  head() + "streams: {name: cam}\n",
		"lenient":             lenientText(),
		"lenient node checks": head() + "streams:\n" + streamYAML("cam") + "  - &x {name: anchored, inputs: [{url: rtmp://a/live/x}]}\n  - *x\n" + streamYAML("other"),
		"library":             fixtureLibrary(),
		"hand":                fixtureHand(),
		"secret":              secretText(),
		"full": head() + `streams:
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
      watermark:
        text: "On air %H:%M"
        position: br
    outputs:
      hls: true
      srt: true
      srt_passphrase: "full-0123456789"
      push:
        - url: rtmp://cdn.example/live/key1
        - url: rtmp://cdn2.example/live/key2
`,
	}
	for _, c := range punctuationRows() {
		texts["punctuation: "+c.name] = c.text
	}
	return texts
}

// leakString is a string with a secret, and the part of it no output may
// hold.
type leakString struct{ text, secret string }

// leakStrings are finding 2's five strings and four secret URLs that
// parse.
var leakStrings = []leakString{
	{"rtsp://admin:p%ss@cam/s", "p%ss"},
	{"rtsp://admin:pa#ss@cam/s", "pa#ss"},
	{"rtsp://u:pw@h:port/s", ":pw@"},
	{"see rtsp://u:pw@h/s now", ":pw@"},
	{"url:rtsp://u:pw@h/s", ":pw@"},
	{"rtsp://admin:hunter2-pw@192.0.2.1/s", camPassword},
	{"rtmp://h/live/pushkey-0123456789", camPushKey},
	{"rtsp://192.0.2.2/user=admin_password=tlJwpbo6_channel=1", dvrPassword},
	{"https://cdn/x.m3u8?token=abc123tok", "abc123tok"},
}

// neverLeaks is what TestMaskNeverLeaks asserts of one text that decodes.
func neverLeaks(t *testing.T, text []byte, secrets ...string) {
	t.Helper()
	out, err := Mask(text)
	if err != nil {
		t.Fatalf("Mask: %v\n%s", err, text)
	}
	for _, s := range secrets {
		if bytes.Contains(out, []byte(s)) {
			t.Fatalf("the secret %q is in the output\n--- text\n%s\n--- output\n%s", s, text, out)
		}
	}
	// Mask returns its own output as it is, the same slice, only when the
	// tree holds nothing to hide and the postcondition holds on the bytes.
	again, err := Mask(out)
	if err != nil || !sameSlice(again, out) {
		t.Fatalf("Mask of the output is not the output: %v\n--- output\n%s\n--- again\n%s", err, out, again)
	}
}

// mustMask is Mask of text, which must not fail.
func mustMask(t *testing.T, text string) []byte {
	t.Helper()
	out, err := Mask([]byte(text))
	require.NoError(t, err)
	return out
}

// scalarNodes are the scalars under n, keys and values, in order.
func scalarNodes(n *yaml.Node) []*yaml.Node {
	var out []*yaml.Node
	if n.Kind == yaml.ScalarNode {
		out = append(out, n)
	}
	for _, c := range n.Content {
		out = append(out, scalarNodes(c)...)
	}
	return out
}

// Review Focus 4.
func TestMaskNeverLeaks(t *testing.T) {
	styles := []yaml.Style{0, yaml.SingleQuotedStyle, yaml.DoubleQuotedStyle, yaml.FoldedStyle, yaml.LiteralStyle}

	t.Run("every scalar of the tables", func(t *testing.T) {
		var checked atomic.Int64
		t.Run("texts", func(t *testing.T) {
			for name, text := range tableTexts() {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					root, err := decodeNode([]byte(text))
					require.NoError(t, err)
					where := newLines([]byte(text))
					for k, at := range scalarNodes(root) {
						for j, s := range leakStrings {
							// Through the node tree, in every style.
							fresh, err := decodeNode([]byte(text))
							require.NoError(t, err)
							n := scalarNodes(fresh)[k]
							n.Value, n.Tag, n.Style = s.text, "!!str", styles[(k+j)%len(styles)]
							if enc, err := encode(fresh, "test"); err == nil {
								if _, err := decodeNode(enc); err == nil {
									neverLeaks(t, enc, s.secret)
									checked.Add(1)
								}
							}
							// In the bytes, where a plain scalar is written.
							if i := where.offset(at.Line, at.Column); at.Style == 0 && at.Value != "" && i >= 0 && strings.HasPrefix(text[i:], at.Value) {
								raw := []byte(text[:i] + s.text + text[i+len(at.Value):])
								if _, err := decodeNode(raw); err == nil {
									neverLeaks(t, raw, s.secret)
									checked.Add(1)
								}
							}
						}
					}
				})
			}
		})
		t.Logf("%d texts checked", checked.Load())
		assert.Greater(t, checked.Load(), int64(5000))
	})

	t.Run("generated", func(t *testing.T) {
		type one struct {
			text    string
			secrets []string
		}
		r := rand.New(rand.NewPCG(20261127, 5))
		var texts []one
		for range 2000 {
			text, secrets := generated(r)
			if _, err := decodeNode([]byte(text)); err == nil {
				texts = append(texts, one{text, secrets})
			}
		}
		require.Greater(t, len(texts), 1500, "most generated texts decode")
		const parts = 8
		for p := range parts {
			t.Run(fmt.Sprintf("part %d", p), func(t *testing.T) {
				t.Parallel()
				for i := p; i < len(texts); i += parts {
					c := texts[i]
					neverLeaks(t, []byte(c.text), c.secrets...)
					back, err := Unmask(mustMask(t, c.text), []byte(c.text))
					require.NoError(t, err)
					require.Equal(t, c.text, string(back))
				}
			})
		}
	})
}

// generated is one text of TestMaskNeverLeaks' generator and the secrets
// it holds: a stream whose input is a leakString in a picked quoting, in
// flow or block style, with an anchor or none, a comment that holds
// another leakString in a picked place, and a picked indentation.
func generated(r *rand.Rand) (string, []string) {
	val, note := leakStrings[r.IntN(len(leakStrings))], leakStrings[r.IntN(len(leakStrings))]
	secrets := []string{val.secret}
	url := val.text
	switch r.IntN(4) {
	case 1:
		url = "'" + strings.ReplaceAll(url, "'", "''") + "'"
	case 2:
		url = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(url) + `"`
	case 3:
		// One letter of it as an escape.
		i := strings.Index(url, "://") + 3 + r.IntN(len(url)-strings.Index(url, "://")-3)
		url = `"` + url[:i] + fmt.Sprintf(`\x%02x`, url[i]) + url[i+1:] + `"`
	}
	anchor := r.IntN(4) == 0
	if anchor {
		url = "&a " + url
	}
	ind := strings.Repeat(" ", 2+2*r.IntN(2))
	place := r.IntN(7)
	comment := ""
	if place > 0 {
		c := note.text
		if r.IntN(3) == 0 {
			c = `"` + c + `",`
		}
		comment = "# old " + c
		secrets = append(secrets, note.secret)
	}
	at := func(p int, before, after string) string {
		if place == p {
			return before + comment + after
		}
		return ""
	}
	var b strings.Builder
	b.WriteString(head() + at(1, "", "\n") + "streams:\n")
	b.WriteString(at(2, ind, "\n"))
	if r.IntN(2) == 0 {
		// Block style.
		b.WriteString(ind + "- name: cam\n")
		b.WriteString(ind + "  inputs:\n")
		b.WriteString(at(3, ind+ind+"  ", "\n"))
		b.WriteString(ind + ind + "- url: " + url + at(4, " ", "") + "\n")
		b.WriteString(at(5, ind+"  ", "\n"))
	} else {
		// Flow style; the comments inside it are ones the parser drops.
		b.WriteString(ind + "- name: cam\n")
		b.WriteString(ind + "  inputs: [" + at(3, " ", "\n"+ind+"    "))
		b.WriteString("{url: " + url + "}" + at(4, ", ", "\n"+ind+"    {url: udp://239.0.0.1:1234}"))
		b.WriteString("]" + at(5, " ", "") + "\n")
	}
	if anchor && r.IntN(2) == 0 {
		b.WriteString(ind + "- name: two\n" + ind + "  inputs:\n" + ind + ind + "- url: *a\n")
	}
	b.WriteString(at(6, "", "\n"))
	return b.String(), secrets
}

func TestMaskAcceptsWhatTheServerRuns(t *testing.T) {
	accepted := 0
	for name, text := range tableTexts() {
		in := []byte(text)
		if _, _, err := ParseLenient(in, testOpt); err != nil {
			continue
		}
		accepted++
		out, err := Mask(in)
		require.NoError(t, err, name)
		again, err := Mask(out)
		require.NoError(t, err, name)
		assert.Equal(t, string(out), string(again), name)
	}
	assert.GreaterOrEqual(t, accepted, 12+len(punctuationRows()))
}

// Review Focus 2.
func TestUnmaskErrors(t *testing.T) {
	stored := secretText()
	shown := maskedText()
	const (
		camRef   = "streams[0] (cam)"
		dvrRef   = "streams[1] (dvr)"
		plainRef = "streams[2] (plain)"
	)
	movedHost := func(s string) string {
		return strings.Replace(s, "rtsp://admin:xxxxx@192.0.2.1/s", "rtsp://admin:xxxxx@192.0.2.9/s", 1)
	}
	plainPassphrase := func(s string) string {
		return s + "    outputs:\n      srt: true\n      srt_passphrase: xxxxx\n"
	}

	for _, c := range []struct {
		name, sent string
		want       []string
	}{
		{
			"a masked URL in a stream that is not stored",
			head() + "streams:\n  - name: new\n    inputs:\n      - url: rtsp://admin:xxxxx@192.0.2.1/s\n",
			[]string{refusal("streams[0] (new)", "inputs[0].url")},
		},
		{
			"a masked URL in a stream without a name",
			head() + "streams:\n  - inputs:\n      - url: rtsp://admin:xxxxx@192.0.2.1/s\n",
			[]string{refusal("streams[0]", "inputs[0].url")},
		},
		{"cam's input with its host changed", movedHost(shown), []string{refusal(camRef, "inputs[0].url")}},
		{
			"cam's input with its user changed",
			strings.Replace(shown, "rtsp://admin:xxxxx@", "rtsp://root:xxxxx@", 1),
			[]string{refusal(camRef, "inputs[0].url")},
		},
		{
			"cam's push copied into dvr",
			strings.Replace(shown, "        hls: true\n", "        hls: true\n        push:\n            - url: rtmp://h/live/puxxxxx\n", 1),
			[]string{refusal(dvrRef, "outputs.push[0].url")},
		},
		{
			"cam's input copied into dvr, behind dvr's own",
			strings.Replace(shown, "_channel=1\n", "_channel=1\n        - url: rtsp://admin:xxxxx@192.0.2.1/s\n", 1),
			[]string{refusal(dvrRef, "inputs[1].url")},
		},
		{"a passphrase mask in plain", plainPassphrase(shown), []string{refusal(plainRef, "outputs.srt_passphrase")}},
		{
			"cam's input among its pushes",
			strings.Replace(shown, "        - url: rtmp://h/live/puxxxxx\n", "        - url: rtmp://h/live/puxxxxx\n        - url: rtsp://admin:xxxxx@192.0.2.1/s\n", 1),
			[]string{refusal(camRef, "outputs.push[1].url")},
		},
		{
			"two masks in two streams",
			plainPassphrase(movedHost(shown)),
			[]string{refusal(camRef, "inputs[0].url"), refusal(plainRef, "outputs.srt_passphrase")},
		},
		{
			"every place of one stream",
			strings.NewReplacer("192.0.2.1/s", "192.0.2.9/s", "rtmp://h/live/puxxxxx", "rtmp://other/live/puxxxxx").Replace(shown),
			[]string{refusal(camRef, "inputs[0].url"), refusal(camRef, "outputs.push[0].url")},
		},
		{
			"the bare mask",
			strings.Replace(shown, "rtsp://admin:xxxxx@192.0.2.1/s", "xxxxx", 1),
			[]string{refusal(camRef, "inputs[0].url")},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.NotEqual(t, shown, c.sent)
			got := unmaskErrors(t, c.sent, stored)
			assert.Equal(t, c.want, got)
			for _, e := range got {
				leak(t, []byte(e), camPassword, camPushKey, camSRTPass, dvrPassword, "192.0.2", "admin")
			}
		})
	}

	t.Run("nothing stored", func(t *testing.T) {
		for _, stored := range []string{"", "not: [yaml", head()} {
			assert.Equal(t,
				[]string{refusal(camRef, "inputs[0].url"), refusal(camRef, "outputs.push[0].url"), refusal(camRef, "outputs.srt_passphrase"), refusal(dvrRef, "inputs[0].url")},
				unmaskErrors(t, shown, stored))
		}
	})

	t.Run("the errors are bounded", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(head() + "streams:\n")
		for i := range MaxErrors + 20 {
			fmt.Fprintf(&b, "  - name: s%d\n    inputs:\n      - url: rtsp://u:xxxxx@h/s\n", i)
		}
		got := unmaskErrors(t, b.String(), stored)
		require.Len(t, got, MaxErrors+1)
		assert.Equal(t, "and 20 more", got[MaxErrors])
	})

	// Stored holds two entries named cam, which the lenient start allows:
	// the candidates are both entries'.
	t.Run("two stored entries of one name", func(t *testing.T) {
		two := func(first, second string) string {
			return head() + "streams:\n" +
				"  - name: cam\n    inputs:\n      - url: " + first + "\n" +
				"  - name: cam\n    inputs:\n      - url: " + second + "\n"
		}
		const (
			one   = "rtsp://u:first-secret@h/s"
			other = "rtsp://u:other-secret@h/s"
			form  = "rtsp://u:xxxxx@h/s"
			extra = "  - name: plain\n    inputs:\n      - url: udp://239.0.0.1:1234\n"
		)
		stored := two(one, other)
		shown, err := Mask([]byte(stored))
		require.NoError(t, err)
		require.Equal(t, two(form, form), string(shown))

		// Both entries sent back: each takes one of the two values, in
		// order, though each alone sees two candidates and one mask.
		assert.Equal(t,
			[]string{refusal(camRef, "inputs[0].url"), refusal("streams[1] (cam)", "inputs[0].url")},
			unmaskErrors(t, two(form, form)+extra, stored))
		// One entry sent back: one mask for two stored values.
		one1 := head() + "streams:\n  - name: cam\n    inputs:\n      - url: " + form + "\n"
		assert.Equal(t, []string{refusal(camRef, "inputs[0].url")}, unmaskErrors(t, one1+extra, stored))
		// One entry with two masks takes both, in order.
		got, err := Unmask([]byte(one1+"      - url: "+form+"\n"), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, head()+"streams:\n  - name: cam\n    inputs:\n      - url: "+one+"\n      - url: "+other+"\n", string(got))
		// When both entries hold the same value there is one candidate.
		got, err = Unmask([]byte(two(form, form)+extra), []byte(two(one, one)))
		require.NoError(t, err)
		assert.Equal(t, two(one, one)+extra, string(got))
	})
}

// The checks the results rest on, by themselves.
func TestMaskChecks(t *testing.T) {
	node := func(text string) *yaml.Node {
		n, err := decodeNode([]byte(text))
		require.NoError(t, err)
		return n
	}
	// sameTree: kinds, tags, values, anchors, comments and children.
	const base = "a: [x, y] # c\n"
	assert.True(t, sameTree(node(base), node("a:   [x,   'y']   # c\n")), "styles and positions are not compared")
	for _, other := range []string{
		"a: [x, z] # c\n", "a: [x, y] # d\n", "a: [x, y]\n", "# c\na: [x, y]\n", "a: [x, y, z] # c\n", "a: {x: y} # c\n",
		"a: [x, 1] # c\n", "a: &k [x, y] # c\n", "b: [x, y] # c\n", "a: [x, y] # c\n# foot\n",
	} {
		assert.False(t, sameTree(node(base), node(other)), other)
	}

	// shown: a text that hides everything is still refused when it is
	// not the wanted document.
	assert.True(t, shown([]byte("a: 1\n"), node("a: 1\n")))
	assert.True(t, shown([]byte("a: 1\n"), nil))
	assert.False(t, shown([]byte("a: 1\n"), node("a: 2\n")))
	assert.False(t, shown([]byte("a: [\n"), nil))
	assert.False(t, shown([]byte("a: rtsp://u:pw@h/s\n"), nil))
	assert.False(t, shown([]byte("a: [ # rtsp://u:pw@h/s\n  1]\n"), nil))

	// quotedSpans: a span is trusted only when its bytes decode to the
	// node's value. Here the tree is another text's, with a double-quoted
	// scalar at the same place.
	text := []byte("a: \"rtsp://u:pw@h/s\"\n")
	assert.Equal(t, []span{{3, 20}}, quotedSpans(text, node(string(text))))
	assert.Empty(t, quotedSpans(text, node("a: \"udp://239.0.0.1:1\"\n")))
	assert.False(t, rawClean(text, node("a: \"udp://239.0.0.1:1\"\n")))
	// An anchor or a tag before the quote is stepped over.
	text = []byte("a: &k !!str \"udp://h:1\\t\"\n")
	assert.Equal(t, []span{{12, 25}}, quotedSpans(text, node(string(text))))
	// Positions that point nowhere give no span.
	assert.Equal(t, -1, newLines(text).offset(3, 1))
	assert.Equal(t, -1, newLines(text).offset(1, 200))
	assert.Equal(t, -1, newLines(text).offset(0, 1))
	assert.Equal(t, -1, newLines(text).offset(1, 0))
	// A column counts characters, on a line of any length, asked in any
	// order.
	long := []byte("ab\n" + strings.Repeat("é", 200) + "x" + strings.Repeat("𝄞", 70) + "y\nz\n")
	where := newLines(long)
	for _, c := range []struct{ line, column, want int }{
		{2, 272, 3 + 400 + 1 + 280},
		{2, 1, 3},
		{2, 201, 403},
		{2, 64, 3 + 126},
		{2, 65, 3 + 128},
		{2, 66, 3 + 130},
		{2, 202, 404},
		{2, 203, 408},
		{1, 2, 1},
		{1, 3, 2},
		{1, 4, -1},
		{3, 1, 3 + 400 + 1 + 280 + 2},
		{2, 274, -1},
		{2, 5000, -1},
		{3, 3, -1},
	} {
		assert.Equal(t, c.want, where.offset(c.line, c.column), "line %d column %d", c.line, c.column)
	}
	_, ok := quotedSpan(text, -1)
	assert.False(t, ok)
	_, ok = quotedSpan([]byte(`a: "open`), 3)
	assert.False(t, ok)
	assert.Equal(t, []int{0, 2, 5, 7, 10, 14, 18}, lineStarts([]byte("a\nb\r\nc\rd\u0085e\u2028f\u2029g")))
}

// Finding 4.
func TestUnmaskOrder(t *testing.T) {
	doc := func(pushes ...string) string {
		s := head() + "streams:\n  - name: cam\n    inputs:\n      - url: udp://239.0.0.1:1234\n    outputs:\n      push:\n"
		for _, p := range pushes {
			s += "        - url: " + p + "\n"
		}
		return s
	}
	const (
		keyOne = "rtmp://h/live/secretkey"
		keyTwo = "rtmp://h/live/sesame"
		form   = "rtmp://h/live/sexxxxx"
		ref    = "streams[0] (cam)"
	)
	stored := doc(keyOne, keyTwo)
	shown, err := Mask([]byte(stored))
	require.NoError(t, err)
	require.Equal(t, doc(form, form), string(shown))

	t.Run("a third push added", func(t *testing.T) {
		got, err := Unmask([]byte(doc(form, form, "rtmp://third/live/newkey")), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, doc(keyOne, keyTwo, "rtmp://third/live/newkey"), string(got))
		parseOK(t, string(got))
	})
	t.Run("a new push between the masks", func(t *testing.T) {
		got, err := Unmask([]byte(doc(form, "rtmp://third/live/newkey", form)), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, doc(keyOne, "rtmp://third/live/newkey", keyTwo), string(got))
	})
	t.Run("one of the two masks deleted", func(t *testing.T) {
		assert.Equal(t, []string{refusal(ref, "outputs.push[0].url")}, unmaskErrors(t, doc(form), stored))
		assert.Equal(t, []string{refusal(ref, "outputs.push[1].url")}, unmaskErrors(t, doc("rtmp://third/live/newkey", form), stored))
	})
	t.Run("a third mask", func(t *testing.T) {
		assert.Equal(t,
			[]string{refusal(ref, "outputs.push[0].url"), refusal(ref, "outputs.push[1].url"), refusal(ref, "outputs.push[2].url")},
			unmaskErrors(t, doc(form, form, form), stored))
	})
	t.Run("a new real key of the same form beside the masks", func(t *testing.T) {
		const third = "rtmp://h/live/sextant"
		require.Equal(t, form, redact.URL(third))
		got, err := Unmask([]byte(doc(form, form, third)), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, doc(keyOne, keyTwo, third), string(got))
		parseOK(t, string(got))
	})
}

// A resolved value is written in the style the mask had; one that is no
// plain string is quoted, and one that cannot be written in place sends
// the document through the node tree.
func TestUnmaskWrites(t *testing.T) {
	doc := func(lines ...string) string {
		return head() + "streams:\n" + streamYAML("cam", append([]string{"    outputs:", "      srt: true"}, lines...)...)
	}
	edit := func(s string) string { return s + "      hls: true\n" }
	for _, c := range []struct{ name, stored, shown, want string }{
		{"digits unquoted", doc("      srt_passphrase: 1234567890"), doc("      srt_passphrase: xxxxx"), doc(`      srt_passphrase: "1234567890"`)},
		{"digits quoted", doc(`      srt_passphrase: "0123456789"`), doc(`      srt_passphrase: "xxxxx"`), doc(`      srt_passphrase: "0123456789"`)},
		{"single quotes", doc("      srt_passphrase: '1234567890.5'"), doc("      srt_passphrase: 'xxxxx'"), doc("      srt_passphrase: '1234567890.5'")},
		{"plain", doc("      srt_passphrase: plain-0123456789"), doc("      srt_passphrase: xxxxx"), doc("      srt_passphrase: plain-0123456789")},
	} {
		t.Run(c.name, func(t *testing.T) {
			stored := strings.ReplaceAll(c.stored, "rtmp://src.example/live/cam", "udp://239.0.0.1:1234")
			shown, err := Mask([]byte(stored))
			require.NoError(t, err)
			wantShown := strings.ReplaceAll(c.shown, "rtmp://src.example/live/cam", "udp://239.0.0.1:1234")
			require.Equal(t, wantShown, string(shown))
			got, err := Unmask([]byte(edit(string(shown))), []byte(stored))
			require.NoError(t, err)
			assert.Equal(t, edit(strings.ReplaceAll(c.want, "rtmp://src.example/live/cam", "udp://239.0.0.1:1234")), string(got))
			assert.Equal(t, stream(t, []byte(stored), "cam").Outputs.SRTPassphrase, stream(t, got, "cam").Outputs.SRTPassphrase)
		})
	}

	t.Run("two stored entries of one name", func(t *testing.T) {
		two := func(first, second string) string {
			return head() + "streams:\n" +
				streamYAML("cam", "    outputs:", "      srt: true", "      srt_passphrase: "+first) +
				streamYAML("cam", "    outputs:", "      srt: true", "      srt_passphrase: "+second)
		}
		sent := edit(head() + "streams:\n" + streamYAML("cam", "    outputs:", "      srt: true", "      srt_passphrase: xxxxx"))
		sent = strings.Replace(sent, "/live/cam", "/live/caxxxxx", 1)
		// Two different passphrases: the mask stands for no one of them.
		assert.Equal(t, []string{refusal("streams[0] (cam)", "outputs.srt_passphrase")},
			unmaskErrors(t, sent, two("first-0123456789", "other-0123456789")))
		got, err := Unmask([]byte(sent), []byte(two("first-0123456789", "first-0123456789")))
		require.NoError(t, err)
		assert.Equal(t, "first-0123456789", stream(t, got, "cam").Outputs.SRTPassphrase)
		got, err = Unmask([]byte(sent), []byte(two("first-0123456789", `""`)))
		require.NoError(t, err)
		assert.Equal(t, "first-0123456789", stream(t, got, "cam").Outputs.SRTPassphrase)
	})

	t.Run("re-encoded", func(t *testing.T) {
		// The sender put the mask in a folded scalar and behind a tag.
		stored := head() + "streams:\n  - name: cam\n    inputs:\n      - url: rtsp://u:first-secret@h/s\n      - url: rtsp://u:other-secret@h/t\n"
		sent := head() + "streams:\n  - name: cam\n    inputs:\n      - url: >-\n          rtsp://u:xxxxx@h/s\n      - url: !!str rtsp://u:xxxxx@h/t\n"
		got, err := Unmask([]byte(sent), []byte(stored))
		require.NoError(t, err)
		s := stream(t, got, "cam")
		assert.Equal(t, "rtsp://u:first-secret@h/s", s.Inputs[0].URL)
		assert.Equal(t, "rtsp://u:other-secret@h/t", s.Inputs[1].URL)
		assert.NotContains(t, string(got), redact.Mask)
	})

	t.Run("a secret no plain scalar holds", func(t *testing.T) {
		// Stored in quotes, sent back plain: the value is written in
		// quotes where the mask was.
		stored := head() + "streams:\n  - name: cam\n    inputs:\n      - url: \"https://h/x?token=abc:\"\n"
		sent := head() + "streams:\n  - name: cam\n    inputs:\n      - url: https://h/x?token=xxxxx\n    disabled: true\n"
		got, err := Unmask([]byte(sent), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, head()+"streams:\n  - name: cam\n    inputs:\n      - url: \"https://h/x?token=abc:\"\n    disabled: true\n", string(got))
	})

	t.Run("a secret no flow scalar holds", func(t *testing.T) {
		// In a flow mapping the comma ends a plain scalar: the result is
		// compared, found different and the document re-encoded.
		stored := head() + "streams:\n  - name: cam\n    inputs:\n      - url: \"rtsp://u:a,b@h/s\"\n"
		sent := head() + "streams:\n  - {name: cam, inputs: [{url: rtsp://u:xxxxx@h/s}], disabled: true}\n"
		_, err := decodeNode([]byte(sent))
		require.NoError(t, err)
		got, err := Unmask([]byte(sent), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, "rtsp://u:a,b@h/s", stream(t, got, "cam").Inputs[0].URL)
		assert.True(t, stream(t, got, "cam").Disabled)
	})
}

func TestPassphraseQuoting(t *testing.T) {
	doc := func(v string) string {
		return head() + "streams:\n" + streamYAML("cam", "    outputs:", "      srt: true", "      srt_passphrase: "+v)
	}
	for _, c := range []struct{ written, want string }{
		{"1234567890", "1234567890"},
		{`"0123456789"`, "0123456789"},
		{`"1234567890.5"`, "1234567890.5"},
		{`'0123456789'`, "0123456789"},
	} {
		assert.Equal(t, c.want, stream(t, []byte(doc(c.written)), "cam").Outputs.SRTPassphrase, c.written)
	}
	for _, v := range []string{"0123456789", "1234567890.5"} {
		_, err := Parse([]byte(doc(v)), testOpt)
		var es Errors
		require.ErrorAs(t, err, &es, v)
		require.Len(t, es, 1)
		assert.Equal(t, "streams[0] (cam): line 12: write the number as a plain decimal integer, or quote it if it is text", es[0].Error())
		assert.NotContains(t, err.Error(), v)
	}
}

func TestETag(t *testing.T) {
	text := []byte(secretText())
	tag := ETag(text)
	require.Len(t, tag, 34)
	assert.Regexp(t, regexp.MustCompile(`^"[0-9a-f]{32}"$`), tag)
	assert.Equal(t, tag, ETag([]byte(secretText())))
	assert.Equal(t, `"`+hex.EncodeToString(redact.Tag(text))[:32]+`"`, tag)

	// One byte of a secret changed: another tag, though Mask shows the
	// same text.
	other := []byte(strings.Replace(secretText(), "hunter2-pw", "hunter2-pW", 1))
	assert.NotEqual(t, tag, ETag(other))
	a, err := Mask(text)
	require.NoError(t, err)
	b, err := Mask(other)
	require.NoError(t, err)
	assert.Equal(t, string(a), string(b))
	assert.NotEqual(t, tag, ETag(append(bytes.Clone(text), '\n')))
	assert.NotEqual(t, ETag(nil), tag)

	// It is no hash of the text that a guess could be tested against.
	sum := sha256.Sum256(text)
	plain := hex.EncodeToString(sum[:])
	assert.NotEqual(t, `"`+plain+`"`, tag)
	for i := 0; i+32 <= len(plain); i++ {
		assert.NotContains(t, tag, plain[i:i+32])
	}
}

// fuzzSecret is the secret FuzzMask plants; fuzzURLs hold it where
// redact.URL hides it, or do not parse.
const fuzzSecret = "Zq7hunterZq7"

var fuzzURLs = []string{
	"rtsp://admin:" + fuzzSecret + "@192.0.2.1/s",
	"https://cdn/x.m3u8?token=" + fuzzSecret,
	"rtsp://192.0.2.2/user=admin_password=" + fuzzSecret + "_channel=1",
	"rtmp://h/live/" + fuzzSecret,
	"srt://h:9000?streamid=publish:cam:u:" + fuzzSecret,
	"rtsp://admin:p%ss" + fuzzSecret + "@cam/s",
	"rtsp://u:" + fuzzSecret + "@h:port/s",
	"rtsp://192.0.2.2/user=admin_password=AA\u00a0" + fuzzSecret + "_channel=1",
	"https://cdn/x.m3u8?token=AA\u3000" + fuzzSecret,
	"a://b/\u2003rtsp://admin:" + fuzzSecret + "@192.0.2.1/s",
	"https://cdn/x.m3u8?token=AA " + fuzzSecret,
	"rtmp://h/live/AAAA " + fuzzSecret,
	"rtsp://admin:my " + fuzzSecret + "@cam/s",
	"rtsp://admin:8554 " + fuzzSecret + "@192.0.2.1:554/s",
	"rtmp://h/live/AAAA " + fuzzSecret + " 100%",
	"rtsp://h/user=a_password=AA " + fuzzSecret + "_channel=1 at 50% zoom",
}

// FuzzMask plants a secret URL, between white space, somewhere in a text.
// Wherever it lands in a text that still decodes, in a scalar of any style
// or in a comment, reported or dropped, the secret is not in what Mask
// returns (but where YAML glues it to the text before it); Mask never fails on a text ParseLenient accepts; and Mask of
// its result is its result.
func FuzzMask(f *testing.F) {
	for _, text := range tableTexts() {
		for i := range 4 {
			f.Add([]byte(text), uint16(i*len(text)/4), uint8(i), uint8(i))
		}
	}
	i := 0
	for _, text := range advRows() {
		f.Add([]byte(text), uint16(len(text)), uint8(i), uint8(i))
		i++
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 64 {
		text, _ := generated(r)
		f.Add([]byte(text), uint16(i*7), uint8(i), uint8(i/7))
	}
	seps := []string{" ", "\n", "\n# ", " # ", "\r\n", "\t", "#", ",#"}
	f.Fuzz(func(t *testing.T, text []byte, pos uint16, which, sep uint8) {
		if len(text) > 1<<12 {
			return
		}
		i := 0
		if len(text) > 0 {
			i = int(pos) % (len(text) + 1)
		}
		before := seps[int(sep)%len(seps)]
		after := seps[int(sep/8)%2] // a space or a line end
		secret := fuzzURLs[int(which)%(len(fuzzURLs)+1)%len(fuzzURLs)]
		passphrase := int(which)%(len(fuzzURLs)+1) == len(fuzzURLs)
		if passphrase {
			// A passphrase commented out: hidden in a comment, kept in a
			// scalar that only reads like one.
			before, secret = []string{"\n# ", " # ", "\n#"}[int(sep)%3], []string{"srt_passphrase: ", "SRT_PASSPHRASE="}[int(sep/4)%2]+fuzzSecret
		}
		planted := slicesConcat(text[:i], []byte(before+secret+after), text[i:])
		// Where YAML glues the planted URL to the text before it, it is no
		// token of its own but the tail of another, as it would be in a
		// log line, and R-T2 may find no secret in it: in double quotes
		// after a backslash and a line end, and after a '#' that is part
		// of a scalar. Only a scalar's value may then hold the secret.
		pre := text[:i]
		if j := bytes.LastIndexAny(pre, " \t\r\n"); j >= 0 {
			pre = pre[j+1:]
		}
		glued := bytes.HasSuffix(bytes.TrimRight(text[:i], " \t\r\n"), []byte(`\`)) ||
			(strings.HasSuffix(before, "#") && bytes.Contains(pre, []byte("://"))) || passphrase
		for _, in := range [][]byte{planted, text} {
			out, err := Mask(in)
			if _, derr := decodeNode(in); derr != nil {
				if err == nil {
					t.Fatalf("Mask accepted a text that does not decode\n%q", in)
				}
				continue
			}
			if err != nil {
				if _, _, perr := ParseLenient(in, testOpt); perr == nil {
					t.Fatalf("Mask failed on a text ParseLenient accepts: %v\n%q", err, in)
				}
				continue
			}
			root, err := decodeNode(out)
			if err != nil {
				t.Fatalf("the output does not decode: %v\n%q", err, out)
			}
			if strings.Contains(secret, " ") && (before == "\n# " || before == " # ") && bytes.Contains(out, []byte(fuzzSecret)) && !bytes.Contains(text, []byte(fuzzSecret)) {
				// Planted as a comment: gone, unless the place is inside a
				// scalar after all.
				inScalar := false
				for _, n := range scalarNodes(root) {
					inScalar = inScalar || strings.Contains(n.Value, fuzzSecret)
				}
				if !inScalar && !strings.HasPrefix(secret, "rtmp") && !strings.HasPrefix(secret, "https") {
					t.Fatalf("the secret is in a comment\n--- text\n%q\n--- output\n%q", in, out)
				}
			}
			if strings.Contains(secret, " ") {
				// A secret that holds a space: hidden where a scalar's
				// value starts with the URL and reads as one whole. As
				// words after other text it is text.
				for _, n := range scalarNodes(root) {
					w := strings.TrimSpace(n.Value)
					if _, err := url.Parse(w); err == nil && strings.HasPrefix(w, secret) {
						t.Fatalf("the secret is in a value that starts with its URL\n--- text\n%q\n--- output\n%q", in, out)
					}
				}
			} else if bytes.Contains(out, []byte(fuzzSecret)) && !bytes.Contains(text, []byte(fuzzSecret)) {
				// In a scalar's value the planted URL may also follow a
				// '#' as the fragment of a URL the bytes do not show (an
				// escape in double quotes): the value tells.
				inScalar, fragment := false, false
				for _, n := range scalarNodes(root) {
					i := strings.Index(n.Value, "#"+secret)
					inScalar = inScalar || strings.Contains(n.Value, fuzzSecret)
					fragment = fragment || i >= 0 && strings.Contains(n.Value[strings.LastIndexAny(n.Value[:i], " \t\r\n")+1:i], "://")
				}
				if (!glued && !fragment) || !inScalar {
					t.Fatalf("the secret is in the output\n--- text\n%q\n--- output\n%q", in, out)
				}
			}
			if !rawClean(out, root) {
				t.Fatalf("the output holds a URL with something to hide\n%q", out)
			}
			again, err := Mask(out)
			if err != nil || !bytes.Equal(again, out) {
				t.Fatalf("Mask of the output: %v\n--- text\n%q\n--- output\n%q\n--- again\n%q", err, in, out, again)
			}
			back, err := Unmask(out, in)
			if err != nil || !sameSlice(back, in) {
				t.Fatalf("Unmask(Mask(t), t) is not t: %v\n%q", err, in)
			}
		}
	})
}

// slicesConcat is the parts joined in a new slice.
func slicesConcat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}
