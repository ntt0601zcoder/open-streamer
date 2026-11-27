package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// advSecret is the secret of the adversarial rows; advURL holds it as a
// password.
const (
	advSecret = "S3CR3Tzz"
	advURL    = "rtsp://admin:" + advSecret + "@192.0.2.1/s"
)

// u16 is s in UTF-16, big or little endian, with a byte order mark or
// without.
func u16(s string, be, bom bool) []byte {
	var b []byte
	put := func(v uint16) {
		if be {
			b = append(b, byte(v>>8), byte(v))
		} else {
			b = append(b, byte(v), byte(v>>8))
		}
	}
	if bom {
		put(0xFEFF)
	}
	for _, v := range utf16.Encode([]rune(s)) {
		put(v)
	}
	return b
}

// cfg is a config with the stream entries given.
func cfg(streams string) string { return head() + "\nstreams:\n" + streams }

// advVisible are the rows whose secret Mask shows, by the rules: it is no
// credential R-T2 knows, where it stands.
var advVisible = map[string]string{
	"glue outer":                 "a URL inside another URL's query value (u=) is that URL's text",
	"no slash":                   "rtsp:admin:… has no \"://\": it is no URL-shaped token",
	"pass elsewhere":             "a passphrase under a key that is not srt_passphrase is not known to be one",
	"plain url glued hash":       "in a scalar, a URL after '#' is the fragment of the URL before it (R-T8)",
	"comment rtmp glued":         "old.rtmp:// is no RTMP scheme, as in a log line",
	"comment rtmp dropped glued": "old.rtmp:// is no RTMP scheme, as in a log line",
	"pass old after flow":        "an old passphrase in a comment that does not repeat the key, after a flow mapping: nothing says what it is",
	"pass old dropped in list":   "the same after the bracket of a list under the key, in a comment the parser drops",
}

// advRows are the review's adversarial texts: a secret URL or passphrase
// in every place and shape of YAML the reviewer tried.
func advRows() map[string]string {
	const (
		SEC = advSecret
		U   = advURL
	)
	return map[string]string{
		"block plain":                     cfg("  - name: cam\n    inputs:\n      - url: " + U + "\n"),
		"single":                          cfg("  - name: cam\n    inputs:\n      - url: '" + U + "'\n"),
		"double":                          cfg("  - name: cam\n    inputs:\n      - url: \"" + U + "\"\n"),
		"double esc x":                    cfg("  - name: cam\n    inputs:\n      - url: \"rtsp://admin:\\x53" + SEC[1:] + "@h/s\"\n"),
		"double esc u":                    cfg("  - name: cam\n    inputs:\n      - url: \"rtsp://admin:\\u0053" + SEC[1:] + "@h/s\"\n"),
		"double esc U":                    cfg("  - name: cam\n    inputs:\n      - url: \"rtsp://admin:\\U00000053" + SEC[1:] + "@h/s\"\n"),
		"double esc tab":                  cfg("  - name: cam\n    inputs:\n      - url: \"" + U + "\\t\"\n"),
		"double esc ://":                  cfg("  - name: cam\n    inputs:\n      - url: \"rtsp:\\x2f/admin:" + SEC + "@h/s\"\n"),
		"double esc :":                    cfg("  - name: cam\n    inputs:\n      - url: \"rtsp\\x3a//admin:" + SEC + "@h/s\"\n"),
		"double esc /":                    cfg("  - name: cam\n    inputs:\n      - url: \"rtsp:\\/\\/admin:" + SEC + "@h/s\"\n"),
		"double cont":                     cfg("  - name: cam\n    inputs:\n      - url: \"rtsp://admin:S3C\\\n          R3Tzz@h/s\"\n"),
		"double cont2":                    cfg("  - name: cam\n    inputs:\n      - url: \"rtsp:\\\n          //admin:" + SEC + "@h/s\"\n"),
		"double fold":                     cfg("  - name: cam\n    inputs:\n      - url: \"x\n          " + U + "\n          y\"\n"),
		"single fold":                     cfg("  - name: cam\n    inputs:\n      - url: 'x\n          " + U + "\n          y'\n"),
		"plain multi":                     cfg("  - name: cam\n    inputs:\n      - url: x\n          " + U + "\n          y\n"),
		"literal":                         cfg("  - name: cam\n    inputs:\n      - url: |\n          " + U + "\n"),
		"literal keep":                    cfg("  - name: cam\n    inputs:\n      - url: |+\n          a " + U + " b\n\n"),
		"folded":                          cfg("  - name: cam\n    inputs:\n      - url: >-\n          " + U + "\n"),
		"folded2":                         cfg("  - name: cam\n    inputs:\n      - url: >\n          see\n          " + U + "\n          now\n"),
		"flow":                            cfg("  - {name: cam, inputs: [{url: " + U + "}]}\n"),
		"flow glued":                      cfg("  - {name: cam, inputs: [{url: " + U + "},{url: " + U + "x}]}\n"),
		"flow seq scalars":                cfg("  - {name: cam, x: [" + U + "," + U + "]}\n"),
		"json":                            `{"version": 1, "server": {"http": {"listen": "127.0.0.1:8080"}}, "streams": [{"name": "cam", "inputs": [{"url": "` + U + `"}]}]}`,
		"json nospace":                    `{"version":1,"streams":[{"name":"cam","inputs":[{"url":"` + U + `"}]}]}`,
		"key url":                         cfg("  - name: cam\n    " + U + ": x\n"),
		"key url quoted":                  cfg("  - name: cam\n    \"" + U + "\": x\n"),
		"key url flow":                    cfg("  - {name: cam, " + U + ": x}\n"),
		"complex key":                     cfg("  - name: cam\n    ? " + U + "\n    : x\n"),
		"complex key map":                 cfg("  - name: cam\n    ? {a: " + U + "}\n    : x\n"),
		"complex key lit":                 cfg("  - name: cam\n    ? |\n      " + U + "\n    : x\n"),
		"anchor scalar":                   cfg("  - name: cam\n    inputs:\n      - url: &u " + U + "\n      - url: *u\n"),
		"anchor dq":                       cfg("  - name: cam\n    inputs:\n      - url: &u \"" + U + "\"\n      - url: *u\n"),
		"anchor map":                      cfg("  - name: cam\n    inputs:\n      - &i {url: " + U + "}\n      - *i\n"),
		"merge":                           cfg("  - &b\n    name: cam\n    inputs: [{url: " + U + "}]\n  - <<: *b\n    name: cam2\n"),
		"merge inline":                    cfg("  - name: cam\n    inputs:\n      - <<: {url: " + U + "}\n"),
		"tag str":                         cfg("  - name: cam\n    inputs:\n      - url: !!str " + U + "\n"),
		"tag custom":                      cfg("  - name: cam\n    inputs:\n      - url: !foo " + U + "\n"),
		"tag custom dq":                   cfg("  - name: cam\n    inputs:\n      - url: !foo \"" + U + "\"\n"),
		"tag binary":                      cfg("  - name: cam\n    inputs:\n      - url: !!binary cnRzcDovL2FkbWluOlMzQ1IzVHp6QDE5Mi4wLjIuMS9z\n"),
		"tag is url":                      cfg("  - name: cam\n    x: !" + U + " v\n"),
		"tag verbatim":                    cfg("  - name: cam\n    x: !<" + U + "> v\n"),
		"tag pct":                         cfg("  - name: cam\n    x: !rtsp://admin:%53" + SEC[1:] + "@h/s v\n"),
		"tag on map":                      cfg("  - name: cam\n    x: !" + U + "\n      a: b\n"),
		"tag on seq":                      cfg("  - name: cam\n    x: !" + U + " [a]\n"),
		"tag on key":                      cfg("  - name: cam\n    !" + U + " x: v\n"),
		"tag directive":                   "%TAG !e! rtsp://admin:" + SEC + "@h/\n---\n" + cfg("  - name: cam\n    x: !e!s v\n"),
		"tag directive unused":            "%TAG !e! rtsp://admin:" + SEC + "@h/\n---\n" + cfg("  - name: cam\n"),
		"tag directive glued":             "%TAG !e! tag:x,2000:" + U + "\n---\n" + cfg("  - name: cam\n    x: !e!s v\n"),
		"comment head":                    cfg("  # old " + U + "\n  - name: cam\n"),
		"comment line":                    cfg("  - name: cam # " + U + "\n"),
		"comment foot":                    cfg("  - name: cam\n# " + U + "\n"),
		"comment nospace":                 cfg("  - name: cam #" + U + "\n"),
		"comment glued q":                 cfg("  - name: 'cam'#" + U + "\n"),
		"comment glued dq":                cfg("  - name: \"cam\"#" + U + "\n"),
		"comment after [":                 cfg("  - name: cam\n    inputs: [ # " + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment after [ glued":           cfg("  - name: cam\n    inputs: [#" + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment after {":                 cfg("  - { # " + U + "\n    name: cam}\n"),
		"comment after ,":                 cfg("  - {name: cam, # " + U + "\n    inputs: [{url: udp://239.0.0.1:1234}]}\n"),
		"comment after ,glued":            cfg("  - {name: cam,#" + U + "\n    inputs: [{url: udp://239.0.0.1:1234}]}\n"),
		"comment after ]":                 cfg("  - {name: cam, inputs: [{url: udp://239.0.0.1:1234}]#" + U + "\n  }\n"),
		"comment after }":                 cfg("  - {name: cam}#" + U + "\n"),
		"comment after :":                 cfg("  - name: # " + U + "\n      cam\n"),
		"comment after -":                 cfg("  - # " + U + "\n    name: cam\n"),
		"comment in flow between":         cfg("  - {name: cam,\n    # " + U + "\n    inputs: [{url: udp://239.0.0.1:1234}]}\n"),
		"comment after ?":                 cfg("  - name: cam\n    ? # " + U + "\n      a\n    : b\n"),
		"comment after anchor":            cfg("  - name: &a # " + U + "\n      cam\n"),
		"comment after tag":               cfg("  - name: !!str # " + U + "\n      cam\n"),
		"comment after |":                 cfg("  - name: cam\n    x: | # " + U + "\n      text\n"),
		"comment after >":                 cfg("  - name: cam\n    x: >- # " + U + "\n      text\n"),
		"comment eof no nl":               cfg("  - name: cam\n") + "# " + U,
		"comment before doc":              "# " + U + "\n---\n" + cfg("  - name: cam\n"),
		"comment after ---":               "--- # " + U + "\n" + cfg("  - name: cam\n"),
		"comment after ...":               cfg("  - name: cam\n") + "... # " + U + "\n",
		"comment after ... nl":            cfg("  - name: cam\n") + "...\n# " + U + "\n",
		"comment after directive":         "%YAML 1.1 # " + U + "\n---\n" + cfg("  - name: cam\n"),
		"comment glued url":               cfg("  - name: cam # see:" + U + "\n"),
		"comment glued paren":             cfg("  - name: cam # (" + U + ")\n"),
		"comment glued quotes":            cfg("  - name: cam # \"" + U + "\",\n"),
		"comment rtmp glued":              cfg("  - name: cam # old.rtmp://h/live/" + SEC + "\n"),
		"comment rtmp dropped glued":      cfg("  - name: cam\n    inputs: [ # old.rtmp://h/live/" + SEC + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment tab":                     cfg("  - name: cam\t#\t" + U + "\n"),
		"comment NEL":                     cfg("  - name: cam\n    inputs: [ # x\u0085#" + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment NEL2":                    cfg("  - name: cam\n    inputs: [ # x\u0085 # " + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment LS":                      cfg("  - name: cam\n    inputs: [ # a://b/\u2028# " + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment LS glued":                cfg("  - name: cam\n    inputs: [ # a://b/\u2028#" + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment CR":                      cfg("  - name: cam\n    inputs: [ # a://b/\r# " + U + "\r      {url: udp://239.0.0.1:1234}]\n"),
		"comment nbsp":                    cfg("  - name: cam\n    inputs: [ # a://b/\u00a0" + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment vt":                      cfg("  - name: cam\n    inputs: [ # a://b/\v" + U + "\n      {url: udp://239.0.0.1:1234}]\n"),
		"comment dq then drop":            cfg("  - {name: \"k://h\", inputs: [#" + U + "\n  {url: udp://239.0.0.1:1234}]}\n"),
		"dq key then drop":                "{\"k://h\":[#" + U + "\n]}",
		"sq key then drop":                "{'k://h/':[#" + U + "\n]}",
		"plain url then drop":             "{k: [a://h/ #" + U + "\n]}",
		"plain url glued hash":            "{k: [a://h/#" + U + "\n]}",
		"crlf":                            strings.ReplaceAll(cfg("  # c "+U+"\n  - name: cam\n    inputs:\n      - url: "+U+" # "+U+"\n"), "\n", "\r\n"),
		"cr only":                         strings.ReplaceAll(cfg("  # c "+U+"\n  - name: cam\n    inputs:\n      - url: "+U+" # "+U+"\n"), "\n", "\r"),
		"bom":                             "\ufeff" + cfg("  # c "+U+"\n  - name: cam\n    inputs:\n      - url: \""+U+"\" # "+U+"\n"),
		"tabs":                            cfg("  - name: cam\n    inputs:\n      - url:\t" + U + "\t#\t" + U + "\n"),
		"docstart":                        "---\n" + cfg("  - name: cam\n    inputs:\n      - url: "+U+"\n") + "...\n",
		"docend trailing comment dropped": "---\n" + cfg("  - name: cam\n") + "...\n# " + U + "\n",
		"nonascii before":                 cfg("  - name: cam\n    x: \"ĐÀI 📺\"\n    inputs:\n      - {x: \"é📺\", url: \"" + U + "\"}\n"),
		"nonascii same line dq":           cfg("  - name: cam\n    inputs:\n      - {x: 📺é, url: \"" + U + "\\t\", y: [ #" + U + "\n ]}\n"),
		"pass plain":                      cfg("  - name: cam\n    outputs:\n      srt_passphrase: " + SEC + "0123\n"),
		"pass single":                     cfg("  - name: cam\n    outputs:\n      srt_passphrase: '" + SEC + "0123'\n"),
		"pass double":                     cfg("  - name: cam\n    outputs:\n      srt_passphrase: \"" + SEC + "0123\"\n"),
		"pass double esc":                 cfg("  - name: cam\n    outputs:\n      srt_passphrase: \"\\x53" + SEC[1:] + "0123\"\n"),
		"pass literal":                    cfg("  - name: cam\n    outputs:\n      srt_passphrase: |\n        " + SEC + "0123\n"),
		"pass folded":                     cfg("  - name: cam\n    outputs:\n      srt_passphrase: >-\n        " + SEC + "0123\n"),
		"pass multiline":                  cfg("  - name: cam\n    outputs:\n      srt_passphrase:\n        " + SEC + "0123\n"),
		"pass flow":                       cfg("  - {name: cam, outputs: {srt_passphrase: " + SEC + "0123}}\n"),
		"pass json":                       cfg("  - {\"name\": \"cam\", \"outputs\": {\"srt_passphrase\":\"" + SEC + "0123\"}}\n"),
		"pass quoted key":                 cfg("  - name: cam\n    outputs:\n      \"srt_passphrase\": " + SEC + "0123\n"),
		"pass esc key":                    cfg("  - name: cam\n    outputs:\n      \"srt\\x5fpassphrase\": " + SEC + "0123\n"),
		"pass complex key":                cfg("  - name: cam\n    outputs:\n      ? srt_passphrase\n      : " + SEC + "0123\n"),
		"pass anchor":                     cfg("  - name: cam\n    outputs:\n      srt_passphrase: &p " + SEC + "0123\n      x: *p\n"),
		"pass alias":                      cfg("  - name: cam\n    x: &p " + SEC + "0123\n    outputs:\n      srt_passphrase: *p\n"),
		"pass alias key":                  cfg("  - name: cam\n    x: &k srt_passphrase\n    outputs:\n      *k : " + SEC + "0123\n"),
		"pass merge":                      cfg("  - name: cam\n    x: &o {srt_passphrase: " + SEC + "0123}\n    outputs:\n      <<: *o\n"),
		"pass tagged":                     cfg("  - name: cam\n    outputs:\n      srt_passphrase: !!str " + SEC + "0123\n"),
		"pass tagged key":                 cfg("  - name: cam\n    outputs:\n      !!str srt_passphrase: " + SEC + "0123\n"),
		"pass null tag":                   cfg("  - name: cam\n    outputs:\n      srt_passphrase: !!null " + SEC + "0123\n"),
		"pass custom tag":                 cfg("  - name: cam\n    outputs:\n      srt_passphrase: !x " + SEC + "0123\n"),
		"pass seq":                        cfg("  - name: cam\n    outputs:\n      srt_passphrase: [" + SEC + "0123]\n"),
		"pass map":                        cfg("  - name: cam\n    outputs:\n      srt_passphrase: {a: " + SEC + "0123}\n"),
		"pass alias seq":                  cfg("  - name: cam\n    x: &p [" + SEC + "0123]\n    outputs:\n      srt_passphrase: *p\n"),
		"pass commented":                  cfg("  - name: cam\n    outputs:\n      # srt_passphrase: " + SEC + "0123\n      hls: true\n"),
		"pass line comment":               cfg("  - name: cam\n    outputs:\n      srt_passphrase: " + SEC + "0123 # was " + SEC + "0123\n"),
		"pass elsewhere":                  cfg("  - name: cam\n    note: " + SEC + "0123\n    outputs:\n      srt_passphrase: " + SEC + "0123\n"),
		"pass top level":                  head() + "\nsrt_passphrase: " + SEC + "0123\n",
		"pass in users":                   strings.Replace(head(), "users:", "users:\n  - srt_passphrase: "+SEC+"0123", 1),
		"pass case":                       cfg("  - name: cam\n    outputs:\n      SRT_Passphrase: " + SEC + "0123\n"),
		"pass dup":                        cfg("  - name: cam\n    outputs:\n      srt_passphrase: " + SEC + "0123\n      srt_passphrase: " + SEC + "4567\n"),
		"pass old after flow":             cfg("  - {name: cam, outputs: {srt: true, srt_passphrase: new-0123456789}} # was " + SEC + "0123\n"),
		"pass old dropped in list":        cfg("  - name: cam\n    outputs:\n      srt_passphrase: [ # " + SEC + "0123\n        new-0123456789]\n"),
		"pass number":                     cfg("  - name: cam\n    outputs:\n      srt_passphrase: 1234567890\n"),
		"unknown key url":                 cfg("  - name: cam\n    whatever: " + U + "\n"),
		"users url":                       strings.Replace(head(), "users:", "users:\n  - name: "+U, 1),
		"server url":                      strings.Replace(head(), "server:", "server:\n  x: "+U, 1),
		"seq scalar":                      cfg("  - " + U + "\n"),
		"top scalar":                      U + "\n",
		"top seq":                         "- " + U + "\n- [" + U + "]\n",
		"streams scalar":                  head() + "\nstreams: " + U + "\n",
		"empty":                           "",
		"glue outer":                      cfg("  - name: cam\n    inputs:\n      - url: http://p/x?u=" + U + "\n"),
		"rtmp key":                        cfg("  - name: cam\n    outputs:\n      push:\n        - url: rtmp://h/live/" + SEC + "\n"),
		"rtmp key }}":                     cfg("  - {name: cam, outputs: {push: [{url: rtmp://h/live/" + SEC + "}]}}\n"),
		"query end":                       cfg("  - {name: cam, inputs: [{url: \"https://cdn/x.m3u8?token=" + SEC + "\"}]}\n"),
		"upper scheme":                    cfg("  - name: cam\n    inputs:\n      - url: RTSP://admin:" + SEC + "@h/s\n"),
		"no slash":                        cfg("  - name: cam\n    inputs:\n      - url: rtsp:admin:" + SEC + "@h/s\n"),
	}
}

// advCheck is what holds of every adversarial text that decodes: Mask
// does not fail, the secret is not in its output in UTF-8 or UTF-16, Mask
// of the output is the output itself, and Unmask of it is the text.
func advCheck(t *testing.T, name string, in []byte) {
	t.Helper()
	if _, err := decodeNode(in); err != nil {
		_, err := Mask(in)
		require.Error(t, err)
		return
	}
	out, err := Mask(in)
	require.NoError(t, err)
	leaked := bytes.Contains(out, []byte(advSecret)) || bytes.Contains(out, u16(advSecret, false, false)) || bytes.Contains(out, u16(advSecret, true, false))
	if _, visible := advVisible[name]; visible {
		assert.True(t, leaked, "the row is listed as visible by the rules and is not: take it off the list")
	} else if leaked {
		t.Errorf("the secret is in the output\n--- text\n%s\n--- output\n%s", in, out)
	}
	again, err := Mask(out)
	require.NoError(t, err)
	assert.True(t, sameSlice(again, out), "Mask of the output is not the output")
	back, err := Unmask(out, in)
	require.NoError(t, err)
	assert.Equal(t, string(in), string(back), "Unmask(Mask(t), t)")
}

func TestMaskAdversarial(t *testing.T) {
	for name, text := range advRows() {
		t.Run(name, func(t *testing.T) { advCheck(t, name, []byte(text)) })
	}
	base := cfg("  # c " + advURL + "\n  - name: cam\n    inputs:\n      - url: " + advURL + "\n    outputs:\n      srt_passphrase: " + advSecret + "0123\n")
	for name, in := range map[string][]byte{
		"utf16le bom":          u16(base, false, true),
		"utf16be bom":          u16(base, true, true),
		"utf16le no bom":       u16(base, false, false),
		"utf16le bom, dropped": u16(cfg("  - name: cam\n    inputs: [ # "+advURL+"\n ]\n"), false, true),
	} {
		t.Run(name, func(t *testing.T) { advCheck(t, name, in) })
	}
}

// C1: a scalar tagged !!binary is base64 of what the decoder reads, so its
// text says nothing of what it holds.
func TestBinary(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	rows := map[string]string{
		"a url":              cfg("  - name: cam\n    inputs:\n      - url: !!binary " + b64(advURL) + "\n"),
		"the passphrase key": cfg("  - name: cam\n    inputs:\n      - url: udp://239.0.0.1:1234\n    outputs:\n      srt: true\n      !!binary " + b64("srt_passphrase") + ": " + advSecret + "0123\n"),
		"a passphrase":       cfg("  - name: cam\n    inputs:\n      - url: udp://239.0.0.1:1234\n    outputs:\n      srt: true\n      srt_passphrase: !!binary " + b64(advSecret+"0123") + "\n"),
		"a push":             cfg("  - name: cam\n    inputs:\n      - url: udp://239.0.0.1:1234\n    outputs:\n      push:\n        - url: !!binary " + b64("rtmp://h/live/"+advSecret+"0123") + "\n"),
		"a url in JSON":      cfg("  - {\"name\": \"cam\", \"inputs\": [{\"url\": !!binary \"" + b64(advURL) + "\"}]}\n"),
		"a name":             cfg("  - name: !!binary " + b64("cam") + "\n    inputs:\n      - url: " + advURL + "\n"),
		"the name key":       cfg("  - !!binary " + b64("name") + ": cam\n    inputs:\n      - url: " + advURL + "\n"),
		"a verbatim tag":     cfg("  - name: cam\n    inputs:\n      - url: !<tag:yaml.org,2002:binary> " + b64(advURL) + "\n"),
	}
	for name, text := range rows {
		t.Run(name, func(t *testing.T) {
			hidden := []string{advSecret, b64(advURL), b64(advSecret + "0123"), b64("rtmp://h/live/" + advSecret + "0123")}
			// (a) The strict parse refuses it, and the start skips the entry.
			_, err := Parse([]byte(text), testOpt)
			var es Errors
			require.ErrorAs(t, err, &es)
			require.Len(t, es, 1)
			assert.Regexp(t, `^streams\[0\]( \(cam\))?: line \d+: !!binary is not supported$`, es[0].Error())
			p, skipped, err := ParseLenient([]byte(text), testOpt)
			require.NoError(t, err)
			assert.Empty(t, p.Doc.Streams)
			require.Len(t, skipped, 1)
			assert.Contains(t, skipped[0].Error(), msgBinary)
			// (b) Mask hides it whole.
			out := checkMask(t, text, hidden...)
			assert.Contains(t, string(out), "xxxxx")
			assert.NotContains(t, string(out), "!!binary")
		})
	}

	t.Run("outside streams", func(t *testing.T) {
		text := strings.Replace(head(), "name: ops", "name: !!binary "+b64("ops"), 1)
		_, err := Parse([]byte(text), testOpt)
		var es Errors
		require.ErrorAs(t, err, &es)
		assert.Equal(t, "line 3: !!binary is not supported", es[0].Error())
		_, _, err = ParseLenient([]byte(text), testOpt)
		require.ErrorContains(t, err, "line 3: !!binary is not supported")
	})

	// (c) A name in base64 is no name: a mask is not resolved through it
	// against another stream, whatever its text or its decoded value is.
	t.Run("a mask under a binary name", func(t *testing.T) {
		stored := cfg("  - name: ai0w\n    inputs:\n      - url: " + advURL + "\n    outputs:\n      srt: true\n      srt_passphrase: PASS" + advSecret + "01\n" +
			"  - name: cam\n    inputs:\n      - url: udp://239.0.0.1:1234\n")
		masked := "    inputs:\n      - url: rtsp://admin:xxxxx@192.0.2.1/s\n    outputs:\n      srt: true\n      srt_passphrase: xxxxx\n"
		for _, second := range []string{
			"  - name: !!binary ai0w\n",                // the text is the other stream's name
			"  - name: !!binary " + b64("ai0w") + "\n", // the decoded value is
			"  - !!binary " + b64("name") + ": ai0w\n", // the key is written in base64
			"  - !!binary name: ai0w\n",                // a key that reads name and decodes to something else
			"  - name: &n ai0w\n",                      // not binary, and still resolved: it is a name
		} {
			sent := cfg("  - name: ai0w\n" + masked + second + masked)
			out, err := Unmask([]byte(sent), []byte(stored))
			if strings.Contains(second, "&n") {
				require.NoError(t, err)
				continue
			}
			assert.Nil(t, out)
			var es Errors
			require.ErrorAs(t, err, &es, second)
			texts := make([]string, len(es))
			for i, e := range es {
				texts[i] = e.Error()
			}
			assert.Equal(t, []string{refusal("streams[1]", "inputs[0].url"), refusal("streams[1]", "outputs.srt_passphrase")}, texts, second)
		}
		// A url or a passphrase tagged !!binary is not resolved either.
		for _, sent := range []string{
			cfg("  - name: ai0w\n    inputs:\n      - url: !!binary rtsp://admin:xxxxx@192.0.2.1/s\n"),
			cfg("  - name: ai0w\n    inputs:\n      - !!binary " + b64("url") + ": rtsp://admin:xxxxx@192.0.2.1/s\n"),
			cfg("  - name: ai0w\n    inputs:\n      - !!binary url: rtsp://admin:xxxxx@192.0.2.1/s\n"),
		} {
			assert.Equal(t, []string{refusal("streams[0] (ai0w)", "inputs[0]")}, unmaskErrors(t, sent, stored))
		}
		sent := cfg("  - name: ai0w\n    inputs:\n      - url: udp://239.0.0.1:1\n    outputs:\n      srt: true\n      srt_passphrase: !!binary xxxxx\n")
		out, err := Unmask([]byte(sent), []byte(stored))
		require.NoError(t, err)
		assert.Equal(t, sent, string(out), "left as sent: the strict parse refuses the tag")
		_, err = Parse(out, testOpt)
		require.ErrorContains(t, err, msgBinary)
	})
}

// A mask behind any tag: whatever Unmask does with it, neither it nor the
// parse after it says the secret.
func TestUnmaskTags(t *testing.T) {
	stored := cfg("  - name: ai0w\n    inputs:\n      - url: " + advURL + "\n    outputs:\n      srt: true\n      srt_passphrase: PASS" + advSecret + "01\n")
	for _, tag := range []string{"!!bool", "!!timestamp", "!!null", "!!map", "!!seq", "!!merge", "!!float", "!!int", "!x", "!!set", "!!binary", "!!str"} {
		sent := cfg("  - name: ai0w\n    inputs:\n      - url: " + tag + " rtsp://admin:xxxxx@192.0.2.1/s\n    outputs:\n      srt: true\n      srt_passphrase: " + tag + " xxxxx\n")
		out, err := Unmask([]byte(sent), []byte(stored))
		if err != nil {
			assert.NotContains(t, err.Error(), advSecret, tag)
			continue
		}
		if _, err := Parse(out, testOpt); err != nil {
			assert.NotContains(t, err.Error(), advSecret, tag)
		}
		if _, skipped, err := ParseLenient(out, testOpt); err != nil {
			assert.NotContains(t, err.Error(), advSecret, tag)
		} else {
			assert.NotContains(t, fmt.Sprint(skipped), advSecret, tag)
		}
	}
}

// I1: the work on one token is bounded, so Mask is linear in the text. A
// config of the largest size made of the worst tokens is masked in a time
// a request can wait for. The bound is loose: before the fix 40 KiB took
// half a minute.
func TestMaskIsLinear(t *testing.T) {
	// The race detector makes everything some ten times slower, and it
	// is not the timing it is there for: a quarter of the size.
	size, bound := MaxBytes, 8*time.Second
	if raceEnabled {
		size, bound = MaxBytes/4, 30*time.Second
	}
	fill := func(line string) string {
		text := head() + "streams:\n" + streamYAML("cam")
		return text + strings.Repeat(line, (size-len(text))/len(line))
	}
	for name, line := range map[string]string{
		"URLs after '#', a token just under the limit": "# " + strings.Repeat("a://b#", (maxToken-8)/6) + "\n",
		"URLs after '#', a token over the limit":       "# " + strings.Repeat("a://b#", 40<<10/6) + "\n",
		"URLs after '#', eight deep":                   "# " + strings.Repeat("a://b#", maxNested) + "x " + strings.Repeat("a://b#", maxNested+1) + "x\n",
		"closing bytes":                                "# a://h:1" + strings.Repeat(`"`, maxTail) + " a://h:1" + strings.Repeat(`"`, maxTail+1) + "\n",
		"a long tail of closing bytes":                 "# a://h:1" + strings.Repeat(`"]`, (maxToken-16)/2) + "\n",
		"passphrases in comments":                      "# srt_passphrase: " + advSecret + " # srt_passphrase= x\n",
		"secrets in comments":                          "# was " + advURL + "\n",
		"a scalar of URLs":                             "note: " + strings.Repeat("a://b#", (maxToken-8)/6) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			text := fill(line)
			if strings.HasPrefix(line, "note:") {
				text = head() + line // one key, once
			}
			start := time.Now()
			out, err := Mask([]byte(text))
			took := time.Since(start)
			require.NoError(t, err)
			t.Logf("%d bytes in %s", len(text), took)
			assert.Less(t, took, bound, "%d bytes", len(text))
			assert.NotContains(t, string(out), advSecret)
			again, err := Mask(out)
			require.NoError(t, err)
			assert.True(t, sameSlice(again, out))
		})
	}

	// A whole config on one line, at the spec's limits: 1000 streams of 8
	// inputs and 8 pushes, a secret in every URL. Every scalar's position
	// is a column on that one line.
	full := spec.MaxStreams
	if raceEnabled {
		full /= 10
	}
	filler := strings.Repeat("y", 100)
	oneLine := func(streams int, q, before, between string) string {
		var b strings.Builder
		b.WriteString("{" + q + "version" + q + ": 1, " + before + q + "streams" + q + ": [" + between)
		for i := range streams {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "{%[1]sname%[1]s: %[1]ss%[2]d%[1]s, %[1]sinputs%[1]s: [", q, i)
			for j := range 8 {
				if j > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "{%[1]surl%[1]s: %[1]srtsp://admin:%[4]s-%[2]d-%[3]d@192.0.2.%[3]d:554/user=admin_password=%[4]s%[2]d_channel=%[3]d/a/rather/long/path/to/the/stream/of/camera/number/%[2]d/input/%[3]d/%[5]s%[1]s}", q, i, j, advSecret, filler)
			}
			b.WriteString("], " + q + "outputs" + q + ": {" + q + "srt" + q + ": true, " + q + "srt_passphrase" + q + ": " + q + advSecret + "-0123456789" + q + ", " + q + "push" + q + ": [")
			for j := range 8 {
				if j > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "{%[1]surl%[1]s: %[1]srtmp://platform-%[3]d.example/live/%[4]s-%[2]d-%[3]d?token=%[4]s%[2]d&a=rather&long=query&to=make&the=line&as=long&as=it&can=be&%[5]s=1%[1]s}", q, i, j, advSecret, filler)
			}
			b.WriteString("]}}")
		}
		b.WriteString("]}\n")
		return b.String()
	}
	for name, c := range map[string]struct {
		streams int
		text    string
	}{
		"one line of JSON":                                {full, oneLine(full, `"`, "", "")},
		"one line of JSON and a dropped comment":          {full, oneLine(full, `"`, "", " # was "+advURL+"\n")},
		"one line of JSON, text that is not ASCII before": {full / 4, oneLine(full/4, `"`, `"note": "`+strings.Repeat("Trực tiếp 📺 ", 500)+`", `, "")},
		"one line in single quotes":                       {full / 4, oneLine(full/4, "'", "", "")},
	} {
		streams, text := c.streams, c.text
		t.Run(name, func(t *testing.T) {
			_, err := decodeNode([]byte(text))
			require.NoError(t, err)
			start := time.Now()
			out, err := Mask([]byte(text))
			took := time.Since(start)
			require.NoError(t, err)
			t.Logf("%d bytes in %s", len(text), took)
			assert.Less(t, took, bound, "%d bytes", len(text))
			assert.NotContains(t, string(out), advSecret)
			assert.Equal(t, streams*33, strings.Count(string(out), "xxxxx"), "8 inputs with two masks each, 8 pushes with two, a passphrase")
			// An edited text back: every mask is resolved.
			edited := strings.Replace(string(out), "{", "{ ", 1)
			start = time.Now()
			back, err := Unmask([]byte(edited), []byte(text))
			took = time.Since(start)
			require.NoError(t, err)
			t.Logf("Unmask in %s", took)
			assert.Less(t, took, bound)
			assert.Equal(t, streams*33, strings.Count(string(back), advSecret))
		})
	}

	// Unmask reads the stored entries once, however many there are and
	// whatever their names, and resolves no list longer than a stream has.
	t.Run("Unmask, entries of one name", func(t *testing.T) {
		n := 40000
		if raceEnabled {
			n /= 10
		}
		var b strings.Builder
		b.WriteString(head() + "streams:\n")
		for i := range n {
			fmt.Fprintf(&b, "  - {name: a, inputs: [{url: 'rtsp://u:%s%d@h/s'}], outputs: {srt_passphrase: p-%d}}\n", advSecret, i, i)
		}
		stored := b.String()
		shown, err := Mask([]byte(stored))
		require.NoError(t, err)
		start := time.Now()
		_, err = Unmask(append(shown, "# edited\n"...), []byte(stored))
		took := time.Since(start)
		t.Logf("%d bytes in %s", len(stored), took)
		assert.Less(t, took, bound)
		var es Errors
		require.ErrorAs(t, err, &es)
		require.Len(t, es, MaxErrors+1)
		assert.Equal(t, refusal("streams[0] (a)", "inputs[0].url"), es[0].Error())
		assert.Equal(t, refusal("streams[0] (a)", "outputs.srt_passphrase"), es[1].Error())
		assert.Equal(t, fmt.Sprintf("and %d more", 2*n-MaxErrors), es[MaxErrors].Error())
	})
	t.Run("Unmask, a list longer than a stream has", func(t *testing.T) {
		list := func(n int, password string) string {
			var b strings.Builder
			b.WriteString(head() + "streams:\n  - name: cam\n    inputs:\n")
			for i := range n {
				fmt.Fprintf(&b, "      - url: rtsp://u:%s@h/%d\n", password, i)
			}
			return b.String()
		}
		got, err := Unmask([]byte(list(maxList, "xxxxx")+"# edited\n"), []byte(list(maxList, advSecret)))
		require.NoError(t, err)
		assert.Equal(t, list(maxList, advSecret)+"# edited\n", string(got))
		texts := unmaskErrors(t, list(maxList+1, "xxxxx")+"# edited\n", list(maxList+1, advSecret))
		require.Len(t, texts, maxList+1)
		assert.Equal(t, refusal("streams[0] (cam)", "inputs[0].url"), texts[0])
		assert.Equal(t, refusal("streams[0] (cam)", fmt.Sprintf("inputs[%d].url", maxList)), texts[maxList])
		// The stored list is the long one.
		texts = unmaskErrors(t, list(1, "xxxxx")+"# edited\n", list(maxList+1, advSecret))
		assert.Equal(t, []string{refusal("streams[0] (cam)", "inputs[0].url")}, texts)
	})

	// Nothing is remembered between these tokens: every one is different.
	t.Run("different tokens", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(head())
		for i := 0; b.Len() < size-200; i++ {
			fmt.Fprintf(&b, "# a%d://h:1%s b%d://u:pw@h:1\"}] c%d://x#d://y#e://u:pw@h/%d\n", i, strings.Repeat("}", maxTail+1), i, i, i)
		}
		start := time.Now()
		out, err := Mask([]byte(b.String()))
		took := time.Since(start)
		require.NoError(t, err)
		t.Logf("%d bytes in %s", b.Len(), took)
		assert.Less(t, took, bound, "%d bytes", b.Len())
		assert.NotContains(t, string(out), ":pw@")
	})

	// What the bounds do to a token.
	long := "a://h/" + strings.Repeat("x", maxToken)
	assert.False(t, cleanToken(long))
	assert.False(t, cleanRaw(long))
	assert.Equal(t, "<invalid url>", maskCommentToken(long))
	assert.Equal(t, "xxxxx", maskValue("see "+long))
	assert.True(t, cleanToken("a://h/"+strings.Repeat("x", maxToken-6)))
	assert.True(t, cleanToken("udp://h:1"+strings.Repeat("}", maxTail)))
	assert.False(t, cleanToken("udp://h:1"+strings.Repeat("}", maxTail+1)))
	assert.True(t, cleanRaw(strings.Repeat("a://b#", maxNested)))
	assert.False(t, cleanRaw(strings.Repeat("a://b#", maxNested+1)))
	assert.Equal(t, "<invalid url>", maskCommentToken(strings.Repeat("a://b#", maxNested)+"rtsp://u:pw@h/s"))
	assert.Equal(t, "a://b#rtsp://u:xxxxx@h/s", maskCommentToken("a://b#rtsp://u:pw@h/s"))
	assert.NotContains(t, maskCommentToken(strings.Repeat("a://b#", maxNested-1)+"rtsp://u:pw@h/s"), ":pw@")
	// The prefixes that are tried: the whole, the shortest, and three more.
	assert.True(t, cleanToken(`http://[::1]"}]`))
	out := checkMask(t, head()+"# "+long+"\nnote: "+long+" rtsp://u:pw@h/s\n", ":pw@")
	assert.Equal(t, head()+"# <invalid url>\nnote: xxxxx\n", string(out))
}

// I2 and the review's minor 1: a passphrase in a comment, and everything
// under an srt_passphrase key.
func TestMaskPassphraseText(t *testing.T) {
	const pass = advSecret + "0123"
	entry := func(lines ...string) string {
		return cfg("  - name: cam\n    inputs:\n      - url: udp://239.0.0.1:1234\n    outputs:\n      srt: true\n" + strings.Join(lines, "\n") + "\n")
	}
	for _, c := range []struct{ name, text, want string }{
		{"commented out", entry("      # srt_passphrase: "+pass, "      hls: true"), entry("      # srt_passphrase: xxxxx", "      hls: true")},
		{"beside its replacement", entry("      srt_passphrase: new-0123456789 # was " + pass), entry("      srt_passphrase: xxxxx # xxxxx")},
		{"beside its replacement, quoted", entry(`      srt_passphrase: "new-0123456789"   # was ` + pass), entry(`      srt_passphrase: "xxxxx"   # xxxxx`)},
		{"with '=', in upper case", entry("      # SRT_PASSPHRASE = " + pass + " (old)"), entry("      # SRT_PASSPHRASE = xxxxx")},
		{"quoted, as JSON has it", entry(`      # "srt_passphrase": "` + pass + `",`), entry(`      # "srt_passphrase": xxxxx`)},
		{"twice on a line", entry("      hls: true # srt_passphrase: " + pass + " srt_passphrase: " + pass), entry("      hls: true # srt_passphrase: xxxxx")},
		{"after a URL", entry("      # rtsp://u:" + pass + "@h/s srt_passphrase=" + pass), entry("      # rtsp://u:xxxxx@h/s%20srt_passphrase=xxxxx")},
		{"nothing after it", entry("      # set srt_passphrase: ", "      # srt_passphrase= xxxxx"), entry("      # set srt_passphrase: ", "      # srt_passphrase= xxxxx")},
		{"a foot comment", entry("      hls: true") + "# srt_passphrase: " + pass + "\n", entry("      hls: true") + "# srt_passphrase: xxxxx\n"},
		{"a list", entry("      srt_passphrase: [" + pass + ", '" + pass + "']"), entry("      srt_passphrase: [xxxxx, 'xxxxx']")},
		{"a mapping", entry("      srt_passphrase: {" + pass + ": " + pass + "}"), entry("      srt_passphrase: {xxxxx: xxxxx}")},
		{"a block list", entry("      srt_passphrase:", "        - "+pass+" # "+pass, "        - ~"), entry("      srt_passphrase:", "        - xxxxx # xxxxx", "        - ~")},
		{"another case", entry("      SRT_Passphrase: " + pass), entry("      SRT_Passphrase: xxxxx")},
		{"a quoted key with an escape", entry(`      "srt\x5fpassphrase": ` + pass), entry(`      "srt\x5fpassphrase": xxxxx`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := checkMask(t, c.text, advSecret)
			assert.Equal(t, c.want, string(out))
		})
	}
	// CRLF line ends are kept.
	crlf := strings.ReplaceAll(entry("      # srt_passphrase: "+pass, "      srt_passphrase: "+pass+" # was "+pass), "\n", "\r\n")
	out := checkMask(t, crlf, advSecret)
	assert.Equal(t, strings.ReplaceAll(entry("      # srt_passphrase: xxxxx", "      srt_passphrase: xxxxx # xxxxx"), "\n", "\r\n"), string(out))

	// Shapes that cannot be written in place: hidden all the same.
	for name, text := range map[string]string{
		"a null tag":                 entry("      srt_passphrase: !!null " + pass),
		"a custom tag":               entry("      srt_passphrase: !x " + pass),
		"an aliased key":             cfg("  - name: cam\n    x: &k srt_passphrase\n    outputs:\n      *k : " + pass + "\n"),
		"an alias of a list":         cfg("  - name: cam\n    x: &p [" + pass + "]\n    outputs:\n      srt_passphrase: *p\n"),
		"an alias of a mapping":      cfg("  - name: cam\n    x: &p {a: {b: " + pass + "}}\n    outputs:\n      srt_passphrase: *p # " + pass + "\n"),
		"an alias of itself":         cfg("  - name: cam\n    outputs:\n      srt_passphrase: &p [" + pass + ", *p, [[[[" + pass + "]]]]]\n"),
		"a merge key":                cfg("  - name: cam\n    x: &o {srt_passphrase: " + pass + "}\n    outputs:\n      <<: *o\n"),
		"a literal":                  entry("      srt_passphrase: |", "        "+pass),
		"a comment the parser drops": entry("      push: [ # srt_passphrase: "+pass, "        {url: udp://239.0.0.9:1}]"),
		"a dropped comment, glued":   entry("      push: [ #SRT_PASSPHRASE="+pass, "        {url: udp://239.0.0.9:1}]"),
	} {
		t.Run(name, func(t *testing.T) { checkMask(t, text, advSecret) })
	}

	// A scalar that reads like such a comment is kept, and Mask does not
	// fail on it: its bytes go into double quotes, out of the scan's way.
	for name, text := range map[string]string{
		"single quotes": head() + "note: 'see # srt_passphrase: hello'\n",
		"a literal":     head() + "note: |\n  see # srt_passphrase: hello\n",
		"a plain '#'":   head() + "x: {note: a#b, srt_passphrase: [x, y]}\n",
		"a key":         head() + "'a # srt_passphrase= hello': 1\n",
	} {
		t.Run("in a scalar: "+name, func(t *testing.T) {
			out := checkMask(t, text)
			if name != "a plain '#'" {
				assert.Contains(t, string(out), "hello")
			}
		})
	}
	out = checkMask(t, head()+"note: \"see # srt_passphrase: hello\"\n")
	assert.Equal(t, head()+"note: \"see # srt_passphrase: hello\"\n", string(out), "in double quotes it is left where it is")
}

// The review's minor 2: a token ends at white space that is not ASCII.
func TestMaskUnicodeSpace(t *testing.T) {
	spaces := []string{"\u00a0", "\u3000", "\u2003", "\u202f"}
	// Two URLs glued by such white space: redact.URL reads one URL with
	// nothing to hide, a reader sees two. The second is hidden, and the
	// white space comes back escaped, as redact.URL writes a URL it
	// changed.
	for _, sp := range spaces {
		out := checkMask(t, head()+"# a://b/"+sp+"rtsp://u:pw@h/s\nnote: \"a://b/"+sp+"rtsp://u:pw@h/s\"\n", ":pw@")
		esc := url.PathEscape(sp)
		assert.Equal(t, head()+"# a://b/"+esc+"rtsp://u:xxxxx@h/s\nnote: \"a://b/"+esc+"rtsp://u:xxxxx@h/s\"\n", string(out))
		// Dropped by the parser, found by the scan.
		checkMask(t, head()+"x: [ # a://b/"+sp+"rtsp://u:pw@h/s\n  1]\n", ":pw@")
		assert.False(t, cleanRaw("a://b/"+sp+"rtsp://u:pw@h/s"))
		assert.True(t, cleanRaw("a://b/"+sp+"rtsp://h/s"))
		assert.True(t, cleanRaw("a://b/"+esc+"rtsp://u:xxxxx@h/s"))
	}

	// A secret that holds such white space is one secret: the token is
	// first redacted whole, as redact.URL and the stream API show it, so
	// the part after the white space is not left over. Each is a live
	// stream that passes Parse, and each mask comes back on an edit.
	for _, sp := range spaces {
		for _, c := range []struct{ line, want string }{
			{"      - url: rtsp://h/user=a_password=AA" + sp + advSecret, "      - url: rtsp://h/user=a_password=xxxxx"},
			{"      - url: \"https://c/x.m3u8?token=AA" + sp + advSecret + "\"", "      - url: \"https://c/x.m3u8?token=xxxxx\""},
			{"      - url: rtsp://h/user=a_password=AA" + sp + advSecret + "_channel=1", "      - url: rtsp://h/user=a_password=xxxxx_channel=1"},
			{"      - url: 'srt://h:9000?streamid=publish:cam:u:AA" + sp + advSecret + "'", "      - url: 'srt://h:9000?streamid=publish:cam:u:xxxxx'"},
		} {
			text := camDoc("    inputs:", c.line)
			parseOK(t, text)
			out := checkMask(t, text, advSecret, "AA")
			assert.Equal(t, strings.Replace(camDoc("    inputs:", c.want), "/live/other", "/live/otxxxxx", 1), string(out))
			got, err := Unmask([]byte(oddEdit(string(out), "\n")), []byte(text))
			require.NoError(t, err)
			assert.Equal(t, oddEdit(text, "\n"), string(got))

			// The same line in a comment.
			comment := head() + "# was" + strings.TrimPrefix(c.line, "      - url:") + "\n"
			out = checkMask(t, comment, advSecret)
			assert.NotContains(t, string(out), sp, "nothing of the secret is left after the white space")
		}
		// The whole and the pieces: both are redacted.
		for _, line := range []string{"# %s\n", "note: %s\n", "note: \"%s\"\n"} {
			both := "rtsp://h/password=AA" + sp + advSecret + "/a" + sp + "rtsp://u2:" + advSecret + "@h/b"
			out := checkMask(t, head()+fmt.Sprintf(line, both), advSecret)
			assert.Equal(t, head()+fmt.Sprintf(line, "rtsp://h/password=xxxxx/a"+url.PathEscape(sp)+"rtsp://u2:xxxxx@h/b"), string(out))
		}
	}

	// NEL, LS and PS end a line for the parser, and a piece here.
	for _, br := range []string{"\u0085", "\u2028", "\u2029"} {
		checkMask(t, head()+"x: [ # a://b/"+br+"# rtsp://u:pw@h/s\n  1]\nnote: \"a://b/"+br+"rtsp://u:pw@h/s\"\n", ":pw@")
		checkMask(t, head()+"x: [ # a://b/"+br+"rtsp://u:pw@h/s: 1\n  ]\n", ":pw@")
	}

	// More pieces than are judged: hidden whole.
	many := strings.Repeat("a://b/\u00a0", maxPieces) + "rtsp://u:pw@h/s"
	assert.False(t, cleanRaw(many))
	assert.Equal(t, "<invalid url>", maskCommentToken(many))
	assert.Equal(t, "xxxxx", maskValue(many))
	assert.True(t, cleanRaw(strings.Repeat("a://b/\u00a0", maxPieces-1)+"c://d"))
}

// Review round 3: a secret that holds an ASCII space. The server and the
// stream API read the whole scalar as one URL; so does Mask, after it has
// read its tokens.
func TestMaskWholeValue(t *testing.T) {
	for _, c := range []struct{ name, stored, shown string }{
		{"a query secret", "https://c/x.m3u8?token=AA " + advSecret, "https://c/x.m3u8?token=xxxxx"},
		{"a pair in the path", "rtsp://h/user=a_password=AA " + advSecret + "_channel=1", "rtsp://h/user=a_password=xxxxx_channel=1"},
		{"an RTMP key", "rtmp://h/live/AAAA " + advSecret, "rtmp://h/live/AAxxxxx"},
		{"an SRT passphrase", "srt://h:9000?passphrase=AA " + advSecret + "0123", "srt://h:9000?passphrase=xxxxx"},
		{"two spaces and a second token", "https://c/x.m3u8?token=AA  " + advSecret + " b://c", "https://c/x.m3u8?token=xxxxx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// What the stream API shows of it.
			require.Equal(t, c.shown, redact.URL(c.stored))
			for _, q := range []string{`"`, `'`, ``} {
				if q != `"` && strings.Contains(c.stored, "\t") {
					continue
				}
				value := strings.ReplaceAll(c.stored, "\t", `\t`)
				text := camDoc("    inputs:", "      - url: udp://239.0.0.1:1234", "    outputs:", "      push:", "        - url: "+q+value+q)
				want := stream(t, []byte(text), "cam")
				require.Equal(t, c.stored, want.Outputs.Push[0].URL, "the row is a live stream")
				assert.Equal(t, c.shown, want.Redacted().Outputs.Push[0].URL)

				out := checkMask(t, text, advSecret, "AA ")
				assert.Contains(t, string(out), "        - url: "+q+c.shown+q+"\n")

				// An edit elsewhere: the mask takes the stored value back,
				// through spec.KeepSecrets.
				got, err := Unmask([]byte(oddEdit(string(out), "\n")), []byte(text))
				require.NoError(t, err)
				assert.Equal(t, spec.ChangeNone, spec.Diff(want, stream(t, got, "cam")))
				if !strings.Contains(c.stored, "\t") {
					assert.Equal(t, oddEdit(text, "\n"), string(got), "byte for byte")
				}
			}

			// The line commented out, and the URL in a comment's prose.
			if strings.Contains(c.stored, "\t") {
				return
			}
			for _, line := range []string{"#       - url: \"%s\"", "      # - url: %s", "# was %s"} {
				text := head() + fmt.Sprintf(line, c.stored) + "\nstreams: []\n"
				out := checkMask(t, text, advSecret)
				assert.Contains(t, string(out), c.shown)
				assert.NotContains(t, string(out), "AA ")
			}
			// Where the parser drops the comment, the scan finds it.
			checkMask(t, head()+"x: [ # was "+c.stored+"\n  1]\n", advSecret)
		})
	}

	// Both ways: a second URL after the space is hidden by the tokens,
	// the first one's secret by the whole.
	both := "rtsp://h/password=AA " + advSecret + "/x rtsp://u:" + advSecret + "@h/b"
	out := checkMask(t, head()+"note: "+both+"\n# "+both+"\n", advSecret)
	assert.Equal(t, head()+"note: rtsp://h/password=xxxxx/x%20rtsp://u:xxxxx@h/b\n# rtsp://h/password=xxxxx/x%20rtsp://u:xxxxx@h/b\n", string(out))

	// A value with nothing to hide keeps its bytes, spaces and all, and
	// so does a comment.
	for _, v := range []string{"udp://239.0.0.1:1234 and more", "http://example.com/live now", "HTTP://h/a b", "see rtsp://h/s now"} {
		in := []byte(head() + "note: " + v + "\n# " + v + "\n")
		out, err := Mask(in)
		require.NoError(t, err)
		assert.True(t, sameSlice(in, out), v)
	}
	// A value that starts with a URL and does not parse as one whole is
	// the bare mask, as the stream API shows it.
	assert.Equal(t, "xxxxx", maskScalar("udp://h:1 udp://h:2", true))
	assert.Equal(t, "xxxxx", maskScalar("https://c/x?token=AA\tBB", true), "a tab is no part of a URL")
	out = checkMask(t, camDoc("    inputs:", "      - url: udp://h:1 udp://h:2", "    note: udp://h:1 udp://h:2"))
	assert.Contains(t, string(out), "      - url: xxxxx\n    note: udp://h:1 udp://h:2\n", "under another key it is text")
	assert.Equal(t, "xxxxx", spec.Stream{Inputs: []spec.Input{{URL: "udp://h:1 udp://h:2"}}}.Redacted().Inputs[0].URL)
	// White space around one URL is not more than the URL.
	assert.Equal(t, "udp://h:1234\t", maskValue("udp://h:1234\t"))
	assert.Equal(t, " rtsp://u:xxxxx@h/s \n", maskValue(" rtsp://u:pw@h/s \n"))
	// A value that does not start with a URL is judged by its tokens.
	assert.Equal(t, "see https://c/x?token=xxxxx BB now", maskValue("see https://c/x?token=AA BB now"))

	// hidesNew: an escaped space is not a secret.
	for v, want := range map[string]bool{
		"http://h/a b":               false,
		"rtsp://u:xxxxx@h/a b":       false,
		"https://c/x?token=xxxxx":    false,
		"https://c/x?token=xxxxx BB": true,
		"rtmp://h/live/abxxxxx":      false,
		"rtmp://h/live/abxxxxx tail": true,
		"rtsp://u:pw@h/s":            true,
		"udp://h:1 udp://h:2":        false,
	} {
		_, hid, _ := hidesNew(v)
		assert.Equal(t, want, hid, v)
	}
	// More tokens on a comment's line than are judged with its rest, or a
	// longer line: token by token.
	assert.Equal(t, "# a://b c://d e://f g://h https://c/x?token=xxxxx BB", restOfLine("# a://b c://d e://f g://h https://c/x?token=xxxxx BB"))
	assert.Equal(t, "# a://b c://d e://f https://c/x?token=xxxxx", restOfLine("# a://b c://d e://f https://c/x?token=xxxxx BB"))
}

// Review round 4: a comment's token that cannot be read takes the rest of
// its line with it, and a url value that is no URL as a whole is the bare
// mask whatever it starts with.
func TestMaskUnreadable(t *testing.T) {
	const tail = "R3Tzz"
	for _, c := range []struct{ name, line, want string }{
		{"a space in the password", "# - url: rtsp://admin:my " + tail + "@cam/s", "# - url: <invalid url>"},
		{"a space in the password, words after", "# - url: rtsp://admin:my " + tail + "@cam/s # the door", "# - url: <invalid url>"},
		{"a tab in the password", "# - url: rtsp://admin:my\t" + tail + "@cam/s", "# - url: <invalid url>"},
		{"a tab in a query secret", "# - url: https://c/x.m3u8?token=AA\t" + tail, "# - url: https://c/x.m3u8?token=xxxxx"},
		{"a tab in a streamid secret", "# - url: srt://h:9000?streamid=publish:cam:u:AA\t" + tail, "# - url: srt://h:9000?streamid=publish:cam:u:xxxxx"},
		{"a tab in a path secret", "# - url: rtsp://h/user=a_password=AA\t" + tail + "_channel=1", "# - url: rtsp://h/user=a_password=xxxxx_channel=1"},
		{"a bad escape, then words", "# was rtsp://admin:p%ss@cam/s and " + tail, "# was <invalid url>"},
		{"a bad port, then the path", "# rtsp://u:pw@h:port /" + tail, "# <invalid url>"},
		{"a token too long", "# a://h/" + strings.Repeat("y", maxToken) + " " + tail, "# <invalid url>"},
		{"a second token that cannot be read", "# see http://h/x and rtsp://admin:my " + tail + "@cam/s", "# see http://h/x and <invalid url>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// As a head comment, a line comment, a foot comment, and in a
			// comment the parser drops.
			out := checkMask(t, head()+c.line+"\nstreams: []\n", tail)
			assert.Equal(t, head()+c.want+"\nstreams: []\n", string(out))
			out = checkMask(t, head()+"streams: [] "+c.line+"\n", tail)
			assert.Equal(t, head()+"streams: [] "+c.want+"\n", string(out))
			checkMask(t, head()+"streams: []\n"+c.line+"\n", tail)
			checkMask(t, head()+"x: [ "+c.line+"\n  1]\n", tail)
			assert.Equal(t, c.want, (*scan)(nil).commentLine(c.line))
		})
	}
	// The line before and after it are kept.
	out := checkMask(t, head()+"# one\n# rtsp://admin:my "+tail+"@cam/s\n# three http://h/x\nstreams: []\n", tail)
	assert.Equal(t, head()+"# one\n# <invalid url>\n# three http://h/x\nstreams: []\n", string(out))

	// A url value that is no URL as a whole.
	for _, c := range []struct{ value, other string }{
		{"see https://c/x.m3u8?token=AA " + advSecret, "see https://c/x.m3u8?token=xxxxx " + advSecret},
		{"see rtsp://u:pw@h/s now", "see rtsp://u:xxxxx@h/s now"},
		{"udp://h:1 udp://h:2", "udp://h:1 udp://h:2"},
		{"https://c/x?token=AA\\t" + advSecret, ""},
	} {
		for _, q := range []string{`"`, ``} {
			if q == "" && strings.Contains(c.value, `\`) {
				continue
			}
			text := camDoc("    inputs:", "      - url: "+q+c.value+q, "    note: "+q+c.value+q)
			// The stream is stored and left stopped; the stream API shows
			// the bare mask for it.
			_, skipped, err := ParseLenient([]byte(text), testOpt)
			require.NoError(t, err)
			require.Len(t, skipped, 1, "note is no key of a stream: the row need not run")
			out, err := Mask([]byte(text))
			require.NoError(t, err)
			assert.Contains(t, string(out), "      - url: "+q+"xxxxx"+q+"\n")
			if c.other != "" {
				assert.Contains(t, string(out), "    note: "+q+c.other+q+"\n", "under another key it is text, judged by its tokens")
			}
			// The write-back of a bare mask is refused.
			assert.Equal(t, []string{refusal("streams[0] (cam)", "inputs[0].url")}, unmaskErrors(t, oddEdit(string(out), "\n"), text))
		}
	}
	assert.Equal(t, "xxxxx", maskScalar("see https://c/x.m3u8?token=AA BB", true))
	assert.Equal(t, "xxxxx", spec.Stream{Inputs: []spec.Input{{URL: "see https://c/x.m3u8?token=AA BB"}}}.Redacted().Inputs[0].URL)
	assert.Equal(t, "udp://h:1234\t", maskScalar("udp://h:1234\t", true), "white space around a URL is not in it")
	assert.Equal(t, "plain text", maskScalar("plain text", true))
}

// The token finder is written out by hand: it finds what the pattern it
// stands for finds. On ASCII white space that is the log scrub's pattern.
func TestTokens(t *testing.T) {
	narrow := regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s\x{85}\p{Z}]+`)
	check := func(s string) {
		t.Helper()
		// As a log line's: logging.URLPattern.
		got, want := tokens(s), logging.URLPattern.FindAllStringIndex(s, -1)
		require.Len(t, got, len(want), "%q", s)
		for i, m := range want {
			require.Equal(t, span{m[0], m[1]}, got[i], "%q", s)
		}
		// Cut at white space that is not ASCII too.
		want = narrow.FindAllStringIndex(s, -1)
		for i, at := 0, 0; ; i++ {
			p, ok := nextToken(s, at, true)
			if !ok {
				require.Len(t, want, i, "%q", s)
				break
			}
			require.Greater(t, len(want), i, "%q", s)
			require.Equal(t, span{want[i][0], want[i][1]}, p, "%q", s)
			at = p.end
		}
	}
	for _, s := range []string{
		"see rtsp://u:pw@h/s now", "a://b\tc://d\ne://f\rg://h\fi://j", "x a+b.c-1://h:1\"}], y", "no url", "", "://", "a://", "a:// b", "://x",
		"1a://x", "-a://x", "a1+.-://x", "9://x", "a://b://c", "a:b://c", "a://b a://b", "x://\u00a0y", "é://x", "aé://x b\xff://\xffz", "a://\vb",
		"a:://b", "a://://b", "+://a://b", "a:// a://b", "a://:// ", "ab://c\u3000de://f\u2028g://h\u0085i://j\u2003",
	} {
		check(s)
	}
	for _, text := range tableTexts() {
		check(text)
	}
	for _, text := range advRows() {
		check(text)
	}
	// Random strings over the bytes that matter.
	r := rand.New(rand.NewPCG(7, 11))
	const alphabet = "a:/ \n\t#b1+.-\"'\u00a0\u2028\xff"
	for range 20000 {
		b := make([]byte, r.IntN(24))
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		check(string(b))
	}

	// The pieces of a token.
	for _, c := range []struct {
		tok  string
		want []string
		cut  bool
	}{
		{"a://b/c", nil, false},
		{"a://b/é", nil, false},
		{"a://b/\u00a0c://d\u3000x\u2028e://f", []string{"a://b/", "c://d", "e://f"}, true},
		{"a://b/\u00a0", []string{"a://b/"}, true},
	} {
		ps, cut, ok := pieces(c.tok)
		require.True(t, ok)
		assert.Equal(t, c.cut, cut, "%q", c.tok)
		var got []string
		for _, p := range ps {
			got = append(got, c.tok[p.at:p.end])
		}
		assert.Equal(t, c.want, got, "%q", c.tok)
	}
}

// passRests is written out by hand too: it finds what these patterns
// find, in ASCII case.
func TestPassRests(t *testing.T) {
	for _, c := range []struct {
		s    string
		want []string
	}{
		{"srt_passphrase: a", []string{" a"}},
		{"x # y SRT_Passphrase = a b # c", []string{" a b # c"}},
		{"#\nsrt_passphrase: a\n# \"srt_passphrase\":\"b\",\r\n#srt_passphrase='c'", []string{" a", "\"b\",", "'c'"}},
		{"# srt_passphrase a, srt_passphras: b, rt_passphrase: c, srt-passphrase: d, srt_passphrase", nil},
		{"# srt_passphrase:", []string{""}},
		{"# x\u2028srt_passphrase: a\u0085# srt_passphrase= b\u2029c", []string{" a", " b"}},
		{"# \u017frt_passphrase: a", nil},
		{"_passphrase: a # srt_", nil},
	} {
		var got []string
		for _, r := range passRests(c.s) {
			got = append(got, c.s[r.at:r.end])
		}
		assert.Equal(t, c.want, got, "%q", c.s)
	}
}

// The review's minor 4: what Unmask checks of a text it encoded.
func TestUnresolved(t *testing.T) {
	stored := tree([]byte(secretText()))
	require.NotNil(t, stored)
	kept := streamsNode(stored)
	assert.False(t, unresolved([]byte(secretText()), kept))
	assert.True(t, unresolved([]byte(maskedText()), kept), "masks that would be resolved")
	assert.True(t, unresolved([]byte(cfg("  - name: new\n    inputs:\n      - url: rtsp://u:xxxxx@h/s\n")), kept), "a mask that would be refused")
	assert.True(t, unresolved([]byte("a: [\n"), kept), "not YAML")
	assert.False(t, unresolved([]byte(head()+"# rtsp://u:xxxxx@h/s\n"), kept), "a mask where Unmask does not look")
}

// Review round 5: a comment's token that reads, followed by text that does
// not read as the URL's rest. The line ends with the token when what
// follows holds an '@' or the token had something hidden in it; otherwise
// the words stay.
func TestMaskUnreadableRest(t *testing.T) {
	for _, c := range []struct {
		name, line, want string
		secrets          []string
	}{
		{
			"a space in a numeric password", "# - url: rtsp://admin:1234 5678@cam/s",
			"# - url: rtsp://admin:1234",
			[]string{"5678"},
		},
		{
			"a space in a password that starts as a port", "# - url: rtsp://admin:8554 R3Tzz@192.0.2.1:554/s",
			"# - url: rtsp://admin:8554",
			[]string{"R3Tzz"},
		},
		{
			"a path secret with a space, then words", "# rtsp://h/user=a_password=AA R3Tzz_channel=1 at 50% zoom",
			"# rtsp://h/user=a_password=xxxxx",
			[]string{"R3Tzz"},
		},
		{
			"an RTMP key with a space, then words", "# rtmp://h/live/AAAA R3Tzz 100%",
			"# rtmp://h/live/AAxxxxx",
			[]string{"R3Tzz"},
		},
		{
			"a tab in a numeric password", "# - url: rtsp://admin:1234\t5678@cam/s",
			"# - url: rtsp://admin:1234",
			[]string{"5678"},
		},
		{
			"a second token, its rest with an '@'", "# see http://h/x or rtsp://admin:1234 5678@cam/s",
			"# see http://h/x or rtsp://admin:1234",
			[]string{"5678"},
		},
		{
			"an already masked token, then words", "# rtmp://h/live/AAxxxxx R3Tzz 100%",
			"# rtmp://h/live/AAxxxxx",
			[]string{"R3Tzz"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			// As a head comment, a line comment, a foot comment, and in a
			// comment the parser drops.
			out := checkMask(t, head()+c.line+"\nstreams: []\n", c.secrets...)
			assert.Equal(t, head()+c.want+"\nstreams: []\n", string(out))
			out = checkMask(t, head()+"streams: [] "+c.line+"\n", c.secrets...)
			assert.Equal(t, head()+"streams: [] "+c.want+"\n", string(out))
			checkMask(t, head()+"streams: []\n"+c.line+"\n", c.secrets...)
			checkMask(t, head()+"x: [ "+c.line+"\n  1]\n", c.secrets...)
			assert.Equal(t, c.want, (*scan)(nil).commentLine(c.line))
			assert.Equal(t, c.want, (*scan)(nil).commentLine(c.want))
		})
	}

	// Nothing hidden and no '@': the words stay, and the text is the same slice.
	for _, line := range []string{
		"# see http://h/x for 100% details",
		"# udp://239.0.0.1:1234 at 50% load",
		"# mail ops@example.org about http://h/x at 100% load",
	} {
		assert.Equal(t, line, restOfLine(line))
		assert.Equal(t, line, (*scan)(nil).commentLine(line))
		text := []byte(head() + line + "\nstreams: []\n")
		out, err := Mask(text)
		require.NoError(t, err)
		assert.Same(t, &text[0], &out[0], line)
		assert.Equal(t, string(text), string(out))
	}

	// The bounds hold: more tokens than maxRests, or a longer line, is
	// judged token by token.
	many := "# a://b c://d e://f g://h rtmp://h/live/AAxxxxx tail 100%"
	assert.Equal(t, many, restOfLine(many))
	long := "# rtmp://h/live/AAxxxxx tail 100% " + strings.Repeat("y", maxToken)
	assert.Equal(t, long, restOfLine(long))
}
