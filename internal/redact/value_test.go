package redact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The rules for a value that may hold several URLs came here from
// internal/config, whose tests still drive them through config.Mask; these
// are the rules by themselves.

func TestNextTokenAndTokens(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"no url here", nil},
		{"://x", nil},
		{"1://x", nil},
		{"a://", nil},
		{"udp://h:1", []string{"udp://h:1"}},
		{"see rtsp://u:pw@h/s now", []string{"rtsp://u:pw@h/s"}},
		{"url:rtsp://h/s", []string{"rtsp://h/s"}},
		{"9rtsp://h/s", []string{"rtsp://h/s"}},
		{"a+b.c-d://h x", []string{"a+b.c-d://h"}},
		{"a://b c://d\te://f\ng://h\ri://j\fk://l", []string{"a://b", "c://d", "e://f", "g://h", "i://j", "k://l"}},
		{"a://b/\u00a0c://d", []string{"a://b/\u00a0c://d"}},
		{":// a://b", []string{"a://b"}},
	} {
		var got []string
		for _, s := range Tokens(c.in) {
			got = append(got, c.in[s.At:s.End])
		}
		assert.Equal(t, c.want, got, "%q", c.in)
	}

	// Wide: white space that is not ASCII ends a token too.
	for _, sp := range []string{"\u00a0", "\u0085", "\u2028", "\u2029", "\u3000", "\u2003"} {
		in := "a://b/" + sp + "c://d"
		first, ok := NextToken(in, 0, true)
		assert.True(t, ok)
		assert.Equal(t, "a://b/", in[first.At:first.End], "%q", sp)
		second, ok := NextToken(in, first.End, true)
		assert.True(t, ok)
		assert.Equal(t, "c://d", in[second.At:second.End], "%q", sp)
		narrow, ok := NextToken(in, 0, false)
		assert.True(t, ok)
		assert.Equal(t, in, in[narrow.At:narrow.End], "%q", sp)
	}
	// A zero-width space and other bytes that are not ASCII are part of
	// the token.
	in := "a://b/\u200bc://d\u00e9"
	tok, ok := NextToken(in, 0, true)
	assert.True(t, ok)
	assert.Equal(t, in, in[tok.At:tok.End])
	_, ok = NextToken("a://b", 5, false)
	assert.False(t, ok)
	_, ok = NextToken("a://b", 1, false)
	assert.False(t, ok, "from inside the scheme's \"://\"")
}

func TestPieces(t *testing.T) {
	ps, cut, ok := Pieces("a://b/c")
	assert.Nil(t, ps)
	assert.False(t, cut)
	assert.True(t, ok)

	ps, cut, ok = Pieces("a://b/\u00e9") // not ASCII, and no white space
	assert.Nil(t, ps)
	assert.False(t, cut)
	assert.True(t, ok)

	tok := "a://b/\u00a0rtsp://u:pw@h/s\u2028x"
	ps, cut, ok = Pieces(tok)
	assert.True(t, cut)
	assert.True(t, ok)
	got := make([]string, 0, len(ps))
	for _, p := range ps {
		got = append(got, tok[p.At:p.End])
	}
	assert.Equal(t, []string{"a://b/", "rtsp://u:pw@h/s"}, got)

	// One URL and white space after it: cut, one piece.
	ps, cut, ok = Pieces("a://b/\u00a0")
	assert.True(t, cut)
	assert.True(t, ok)
	assert.Len(t, ps, 1)

	ps, cut, ok = Pieces(strings.Repeat("a://b/\u00a0", MaxPieces))
	assert.True(t, cut)
	assert.True(t, ok)
	assert.Len(t, ps, MaxPieces)
	ps, cut, ok = Pieces(strings.Repeat("a://b/\u00a0", MaxPieces+1))
	assert.Nil(t, ps)
	assert.True(t, cut)
	assert.False(t, ok, "more pieces than are judged")
}

func TestBothWays(t *testing.T) {
	upper := func(s string) (string, bool) { return strings.ToUpper(s), true }
	out, ok := BothWays("a://b", upper)
	assert.True(t, ok)
	assert.Equal(t, "A://B", out)

	// The pieces first, then the whole.
	var seen []string
	note := func(s string) (string, bool) {
		seen = append(seen, s)
		return strings.ReplaceAll(s, "pw", Mask), true
	}
	out, ok = BothWays("a://b/\u00a0c://u:pw@d", note)
	assert.True(t, ok)
	assert.Equal(t, "a://b/\u00a0c://u:xxxxx@d", out)
	assert.Equal(t, []string{"a://b/", "c://u:pw@d", "a://b/\u00a0c://u:xxxxx@d"}, seen)

	fail := func(s string) (string, bool) { return "", !strings.Contains(s, "bad") }
	_, ok = BothWays("a://b/\u00a0bad://x", fail)
	assert.False(t, ok, "a piece fails")
	_, ok = BothWays("bad://x", fail)
	assert.False(t, ok, "the whole fails")
	_, ok = BothWays(strings.Repeat("a://b/\u00a0", MaxPieces+1), upper)
	assert.False(t, ok, "more pieces than are judged")
}

func TestHidesNew(t *testing.T) {
	for _, c := range []struct {
		in        string
		r         string
		hid, read bool
	}{
		{"http://h/a b", "http://h/a%20b", false, true},
		{"rtsp://u:xxxxx@h/a b", "rtsp://u:xxxxx@h/a%20b", false, true},
		{"https://c/x?token=xxxxx", "https://c/x?token=xxxxx", false, true},
		{"https://c/x?token=xxxxx BB", "https://c/x?token=xxxxx", true, true},
		{"rtmp://h/live/abxxxxx", "rtmp://h/live/abxxxxx", false, true},
		{"rtmp://h/live/abxxxxx tail", "rtmp://h/live/abxxxxx", true, true},
		{"rtsp://u:" + vPW + "@h/s", "rtsp://u:xxxxx@h/s", true, true},
		{"udp://h:1 udp://h:2", "udp://h:1 udp://h:2", false, false},
		{"http://h/%zz", "http://h/%zz", false, false},
	} {
		r, hid, read := HidesNew(c.in)
		assert.Equal(t, c.r, r, c.in)
		assert.Equal(t, c.hid, hid, c.in)
		assert.Equal(t, c.read, read, c.in)
	}
}

func TestValue(t *testing.T) {
	const pw = vSecret + "-9zq"
	long := "a://h/" + strings.Repeat("x", MaxToken)
	for _, c := range []struct {
		name, in, text, url string // text: under any key; url: under a url key
	}{
		{"no URL", "just words", "just words", "just words"},
		{"nothing to hide", "udp://239.0.0.1:1234", "udp://239.0.0.1:1234", "udp://239.0.0.1:1234"},
		{"nothing to hide, not normalised", "HTTP://h/a b", "HTTP://h/a b", "HTTP://h/a b"},
		{"a password", "rtsp://u:" + pw + "@h/s", "rtsp://u:xxxxx@h/s", "rtsp://u:xxxxx@h/s"},
		{"white space around", " rtsp://u:" + pw + "@h/s \n", " rtsp://u:xxxxx@h/s \n", " rtsp://u:xxxxx@h/s \n"},
		{"prose", "see rtsp://u:" + pw + "@h/s now", "see rtsp://u:xxxxx@h/s now", "xxxxx"},
		{"two URLs", "rtsp://a/b rtsp://u:" + pw + "@h/s", "rtsp://a/b rtsp://u:xxxxx@h/s", "rtsp://a/b rtsp://u:xxxxx@h/s"},
		{"a secret that holds a space", "https://c/x?token=AA " + pw, "https://c/x?token=xxxxx", "https://c/x?token=xxxxx"},
		{"the same after prose", "see https://c/x?token=AA " + pw + " now", "see https://c/x?token=xxxxx " + pw + " now", "xxxxx"},
		{
			"a space in one secret and a second URL", "rtsp://h/password=AA " + pw + "/x rtsp://u:" + pw + "@h/b",
			"rtsp://h/password=xxxxx/x%20rtsp://u:xxxxx@h/b", "rtsp://h/password=xxxxx/x%20rtsp://u:xxxxx@h/b",
		},
		{"glued by a no-break space", "a://b/\u00a0rtsp://u:" + pw + "@h/s", "a://b/%C2%A0rtsp://u:xxxxx@h/s", "a://b/%C2%A0rtsp://u:xxxxx@h/s"},
		// The piece before the white space does not parse by itself.
		{"a password that holds a no-break space", "rtsp://u:AA\u00a0" + pw + "@h/s", "xxxxx", "xxxxx"},
		{"a query secret that holds a no-break space", "https://c/x?token=AA\u00a0" + pw, "https://c/x?token=xxxxx", "https://c/x?token=xxxxx"},
		{"a token that does not parse", "rtsp://admin:p%ss@cam/s", "xxxxx", "xxxxx"},
		{"one in prose", "see rtsp://u:" + pw + "@h:port/s now", "xxxxx", "xxxxx"},
		{"no URL as a whole", "udp://h:1 udp://h:2", "udp://h:1 udp://h:2", "xxxxx"},
		// As text the words after a tab stay (R-T8's residual for a key
		// other than url); a url key's value is no URL with a tab in it.
		{"a tab is no part of a URL", "https://c/x?token=AA\t" + pw, "https://c/x?token=xxxxx\t" + pw, "xxxxx"},
		{"a token longer than is read", "see " + long, "xxxxx", "xxxxx"},
		{"more pieces than are judged", strings.Repeat("a://b/\u00a0", MaxPieces) + "rtsp://u:" + pw + "@h/s", "xxxxx", "xxxxx"},
		{"the mask's letters of its own", "UDP://xxxxx:5000", "udp://xxxxx:5000", "udp://xxxxx:5000"},
		// Hiding the lone user leaves a whole that does not parse.
		{"a whole that no longer parses", "A://0@# A://00", "a://xxxxx@ A://00", "xxxxx"},
		// The nested URL does not parse and takes its value; the white
		// space after it is escaped: no rule would know the result for
		// a mask again.
		{"a result that is no known mask", "A://#A://\x7f\u00a00", "xxxxx", "xxxxx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			text, url := Value(c.in, false), Value(c.in, true)
			assert.Equal(t, c.text, text, "as text")
			assert.Equal(t, c.url, url, "under a url key")
			for _, out := range []string{text, url} {
				// Value of its own result is its result, and a result
				// with something hidden is a mask a write would know.
				assert.Equal(t, out, Value(out, false), "again, as text: %q", out)
				if out != c.in && masks(out) > masks(c.in) {
					assert.True(t, out == Mask || Masked(out) || HoldsMask(out), "%q is no known mask", out)
				}
			}
			if c.name != "the same after prose" && c.name != "a tab is no part of a URL" {
				assert.NotContains(t, text, pw)
			}
			assert.NotContains(t, url, pw)
		})
	}
}

func TestHoldsMask(t *testing.T) {
	for in, want := range map[string]bool{
		"":                                 false,
		"xxxxx":                            false,
		"words xxxxx words":                false,
		"rtsp://u:pw@h/s":                  false,
		"see rtsp://h/s now, xxxxx":        false,
		"rtsp://u:xxxxx@h/s":               true,
		"see rtsp://u:xxxxx@h/s now":       true,
		" rtsp://u:xxxxx@h/s":              true,
		"rtsp://a/b rtsp://u:xxxxx@h/s":    true,
		"udp://h:1 rtmp://h/live/abxxxxx":  true,
		"a://b/\u00a0rtsp://u:xxxxx@h/s":   true, // a piece
		"a://0%C2%A0a://xxxxx@0":           true, // a piece, its white space escaped
		"a://h/xxxxx/\u00a0b://c":          false,
		"http://h/x?mode=xxxxx tail":       true, // a whole value (frozen exception 15)
		"http://xxxxx.example/ rtsp://h/s": false,
	} {
		assert.Equal(t, want, HoldsMask(in), "%q", in)
	}
	many := strings.Repeat("a://b/\u00a0", MaxPieces+1)
	assert.False(t, HoldsMask(many+"c://d"))
	assert.True(t, HoldsMask(many+"c://xxxxx"), "more pieces than are judged, and the mask's text")
}
