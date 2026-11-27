package redact

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TextSpan is the bytes [At, End) of a text.
type TextSpan struct{ At, End int }

// The bounds that keep the work on one token small and fixed: a token
// longer than MaxToken is not read at all and hidden whole, and at most
// MaxPieces URLs between white space that is not ASCII are judged in one
// token.
const (
	MaxToken  = 4 << 10
	MaxPieces = 8
)

// NextToken finds the first URL-shaped token of s at or after from: what
// logging.URLPattern matches in a log line, a scheme, "://" and everything
// to the next white space. With wide false that is ASCII's white space,
// as in a log line and as URL sees a URL; with wide true the token also
// ends at white space that is not ASCII (NEL, a no-break space, U+3000,
// LS, PS and the other separators). A token is judged both ways (Pieces).
// It is written out, not a regular expression, because it runs over a
// whole config text several times.
func NextToken(s string, from int, wide bool) (TextSpan, bool) {
	for from < len(s) {
		i := strings.Index(s[from:], "://")
		if i < 0 {
			break
		}
		i += from
		// The scheme: the run of scheme bytes before "://", from its
		// first letter.
		at := i
		for at > from && schemeByte(s[at-1]) {
			at--
		}
		for at < i && !isLetter(s[at]) {
			at++
		}
		end := i + 3
		for end < len(s) {
			r, w := rune(s[end]), 1
			if r >= utf8.RuneSelf {
				if !wide {
					end++
					continue
				}
				r, w = utf8.DecodeRuneInString(s[end:])
			}
			if tokenSpace(r) {
				break
			}
			end += w
		}
		if at < i && end > i+3 {
			return TextSpan{at, end}, true
		}
		from = i + 1
	}
	return TextSpan{}, false
}

// tokenSpace reports whether r ends a token: the white space of a regular
// expression's \s and, beyond ASCII, NEL and every Unicode separator.
func tokenSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\f', '\r':
		return true
	}
	return r >= utf8.RuneSelf && (r == 0x85 || unicode.Is(unicode.Z, r))
}

// Pieces returns the URL-shaped tokens inside tok, a token of NextToken,
// when it is cut at white space that is not ASCII too, and whether it
// holds any such white space. ok is false when there are more than
// MaxPieces.
func Pieces(tok string) (ps []TextSpan, cut, ok bool) {
	for i := 0; i < len(tok) && !cut; i++ {
		if tok[i] >= utf8.RuneSelf {
			r, w := utf8.DecodeRuneInString(tok[i:])
			cut = tokenSpace(r)
			i += w - 1
		}
	}
	if !cut {
		return nil, false, true
	}
	for at := 0; ; {
		t, found := NextToken(tok, at, true)
		if !found {
			return ps, true, true
		}
		if len(ps) == MaxPieces {
			return nil, true, false
		}
		ps, at = append(ps, t), t.End
	}
}

// Tokens returns where the URL-shaped tokens of s are, between ASCII white
// space.
func Tokens(s string) []TextSpan {
	var out []TextSpan
	for at := 0; ; {
		t, ok := NextToken(s, at, false)
		if !ok {
			return out
		}
		out, at = append(out, t), t.End
	}
}

// replaceTokens returns s with each URL-shaped token replaced by f of it.
func replaceTokens(s string, f func(tok string) string) string {
	var b strings.Builder
	at := 0
	for _, t := range Tokens(s) {
		b.WriteString(s[at:t.At])
		b.WriteString(f(s[t.At:t.End]))
		at = t.End
	}
	if at == 0 {
		return s
	}
	b.WriteString(s[at:])
	return b.String()
}

// HidesNew returns URL of v, whether it hid something there that was not
// hidden yet (its form differs from v as net/url alone writes it;
// comparing with v itself would take every escaped space for a secret),
// and whether v could be read as a URL at all. A v that cannot be read
// hides nothing here.
func HidesNew(v string) (r string, hid, read bool) {
	u, err := url.Parse(v)
	if err != nil {
		return v, false, false
	}
	if r = URL(v); r == Invalid {
		return v, false, false
	}
	return r, r != u.String(), true
}

// Value is a text value that may hold URLs (a config scalar, a stream's
// URL field) as a response shows it, judged both ways. Each URL-shaped
// token with something to hide is replaced by its redacted form (token)
// and the other tokens keep their bytes. Then a value that starts with a
// URL and runs on past that token's white space is judged whole, as the
// server reads it: when URL hides something in the whole that the tokens
// did not (a secret that holds a space), the value is URL's form of it.
// That is repeated until nothing changes. The value is the bare mask when
// a token cannot be read or when it does not settle.
//
// With isURL, v is a field that holds one URL (a url key), and one that
// does not parse as a whole is the bare mask, whatever it starts with.
// Anywhere else such a value is text, judged by its tokens: a watermark
// text is not lost for holding a URL.
func Value(v string, isURL bool) string {
	if !strings.Contains(v, "://") {
		return v
	}
	if isURL && URL(strings.TrimSpace(v)) == Invalid {
		return Mask
	}
	r := settle(v)
	if isURL && r != v && URL(strings.TrimSpace(r)) == Invalid {
		// Hiding a token left a whole that no longer parses: shown
		// again, it would be the bare mask.
		return Mask
	}
	return r
}

// settle does Value's rounds; it returns the bare mask when a token cannot
// be read or the value does not settle.
func settle(v string) string {
	for range 4 {
		was := v
		whole := false
		v = replaceTokens(v, func(tok string) string {
			r, ok := token(tok)
			whole = whole || !ok
			return r
		})
		if whole {
			return Mask
		}
		// The whole value, when it starts with a URL and is more than
		// that one token: the server and the stream API read it as one
		// URL, white space and all.
		if w := strings.TrimSpace(v); w != "" {
			if t, ok := NextToken(w, 0, false); ok && t.At == 0 && t.End < len(w) {
				if r, hid, _ := HidesNew(w); hid {
					v = r
				}
			}
		}
		if v == was {
			return v
		}
	}
	return Mask
}

// hides reports whether r, URL of tok, hid something in it. Hiding always
// writes the mask, so a form without the mask's text hid nothing, and a
// form equal to tok is URL's own.
func hides(r, tok string) bool {
	return r != tok && strings.Contains(r, Mask)
}

// token is a value's URL-shaped token as a response shows it, judged both
// ways (BothWays): each URL between white space that is not ASCII goes
// through URL (Pieces), so a URL glued to another by such white space is
// hidden; and the whole token does, so a secret that holds such white
// space is hidden whole and not up to it. That is repeated until nothing
// changes: URL escapes that white space in a URL it changed. ok is false
// when the token is longer than MaxToken, when URL cannot parse it or one
// of its pieces, when it has more than MaxPieces, or when it does not
// settle.
func token(tok string) (out string, ok bool) {
	if len(tok) > MaxToken {
		return tok, false
	}
	strict := func(t string) (string, bool) {
		r := URL(t)
		if r == Invalid {
			return "", false
		}
		if !hides(r, t) {
			r = t
		}
		return r, true
	}
	for range 4 {
		was := tok
		if tok, ok = BothWays(tok, strict); !ok {
			return was, false
		}
		if tok == was {
			return tok, true
		}
	}
	return tok, false
}

// BothWays redacts tok by f in each of its pieces, then as a whole. The
// pieces come first: redacting the whole escapes the white space between
// them, and what was a URL of its own after it would no longer be seen as
// one. ok is false when f fails or there are more than MaxPieces.
func BothWays(tok string, f func(string) (string, bool)) (out string, ok bool) {
	ps, cut, ok := Pieces(tok)
	if !ok {
		return "", false
	}
	if cut {
		var b strings.Builder
		at := 0
		for _, p := range ps {
			r, ok := f(tok[p.At:p.End])
			if !ok {
				return "", false
			}
			b.WriteString(tok[at:p.At])
			b.WriteString(r)
			at = p.End
		}
		b.WriteString(tok[at:])
		tok = b.String()
	}
	return f(tok)
}

// HoldsMask reports whether v holds a URL-shaped token that Masked
// reports, between white space of either kind: a value Value wrote a mask
// into. The white space that is not ASCII is also looked for escaped, as
// URL writes it in a token it changed. A token with more pieces than are
// judged counts when it holds the mask's text.
func HoldsMask(v string) bool {
	if !strings.Contains(v, Mask) {
		return false
	}
	for _, t := range Tokens(v) {
		tok := v[t.At:t.End]
		if Masked(tok) || pieceMasked(tok) || pieceMasked(decode(tok)) {
			return true
		}
	}
	return false
}

// pieceMasked reports whether one of tok's pieces is Masked, or tok has
// more pieces than are judged and holds the mask's text.
func pieceMasked(tok string) bool {
	ps, cut, ok := Pieces(tok)
	if !ok {
		return strings.Contains(tok, Mask)
	}
	for _, p := range ps {
		if cut && Masked(tok[p.At:p.End]) {
			return true
		}
	}
	return false
}
