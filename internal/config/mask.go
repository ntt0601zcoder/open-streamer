package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// passphraseKey is the key whose value Mask hides whole and Unmask
// resolves.
const passphraseKey = "srt_passphrase"

// tokenTail are the bytes cut from the end of a raw token before it is
// judged (R-T8): in the text, the URL pattern's "everything to the next
// white space" takes YAML's own closing punctuation with it.
const tokenTail = "\"',]}"

// ETag is the config's entity tag (R-T12): `"` + the first 32 hex digits
// of redact.Tag(text) + `"`. It is the same for the same text within one
// process and changes when the server restarts; no guess of the text can
// be tested against it.
func ETag(text []byte) string {
	return `"` + hex.EncodeToString(redact.Tag(text))[:32] + `"`
}

// Mask returns text as an API response shows it (R-T8). In every scalar,
// keys and values, each URL-shaped token whose redact.URL form carries a
// mask is replaced by that form, a value that starts with a URL is also
// judged whole (maskValue), and a scalar holding a token redact.URL
// cannot parse becomes redact.Mask whole; every non-empty scalar under an
// srt_passphrase key, in any case, becomes redact.Mask, and so does every
// !!binary scalar; in every comment each URL-shaped token goes through
// redact.URL and whatever follows the passphrase key on a line becomes
// the mask. Everything else is copied byte for byte, and a text that needs
// nothing hidden is returned as it is, the same slice. When a scalar
// cannot be replaced in place, or the output's bytes still hold a
// URL-shaped token with something to hide (a comment the parser dropped),
// the document is re-encoded through the node tree, as SetStream does;
// when that text still holds such a token, once more with every scalar
// that holds a URL or a '#' in double quotes. A text in UTF-16 is always
// re-encoded, to UTF-8, also when it holds no secret. It returns an error,
// and no text, when the text is not YAML or when the re-encoded bytes
// still hold such a token.
//
// Whatever path it took, the text it returns was decoded again and holds
// nothing Mask would hide, so Mask of it is the same text. The work grows
// with the text's length and no faster than n log n (the changes are
// sorted): a token is read at most a fixed number of times, one longer
// than 4 KiB not at all, and a scalar's place on its line is found from
// an index of the line, also when the whole config is one line.
func Mask(text []byte) ([]byte, error) {
	root, err := decodeNode(text)
	if err != nil {
		return nil, &SyntaxError{Err: err}
	}
	seen := newScan()
	m := masker{scan: seen}
	m.node(root)
	if !m.changed() {
		if seen.rawClean(text, root) {
			return text, nil
		}
	} else if out, ok := m.patch(text); ok && seen.shown(out, root) {
		return out, nil
	}
	// The node tree now holds what must be shown: encode it. The second
	// round puts every URL in double quotes, where its bytes are judged
	// by its value and YAML's punctuation cannot touch it.
	for _, quote := range []bool{false, true} {
		if quote {
			quoteHidable(root)
		}
		if out, err := encode(root, "mask"); err == nil && seen.shown(out, nil) {
			return out, nil
		}
	}
	return nil, errors.New("config: mask: the text cannot be shown with its secrets hidden")
}

// shown reports whether out is fit for a response: it decodes, to want
// when that is given, the tree holds nothing Mask would hide and neither
// do the bytes.
func shown(out []byte, want *yaml.Node) bool {
	return (*scan)(nil).shown(out, want)
}

func (s *scan) shown(out []byte, want *yaml.Node) bool {
	root, err := decodeNode(out)
	if err != nil || (want != nil && !sameTree(root, want)) {
		return false
	}
	m := masker{scan: s}
	m.node(root)
	return !m.changed() && s.rawClean(out, root)
}

// change is one scalar whose value the tree now holds differently from
// the text: old is the value the text holds.
type change struct {
	n   *yaml.Node
	old string
}

// nextToken finds the first URL-shaped token of s at or after from: what
// logging.URLPattern matches in a log line, a scheme, "://" and everything
// to the next white space. With wide false that is ASCII's white space,
// as in a log line and as redact.URL and the stream API see a URL; with
// wide true the token also ends at white space that is not ASCII (NEL, a
// no-break space, U+3000, LS, PS and the other separators). A token is
// judged both ways (pieces). It is written out, not a regular expression,
// because it runs over the whole text several times.
func nextToken(s string, from int, wide bool) (span, bool) {
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
		for at < i && !letter(s[at]) {
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
			return span{at, end}, true
		}
		from = i + 1
	}
	return span{}, false
}

func letter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

func schemeByte(c byte) bool {
	return letter(c) || '0' <= c && c <= '9' || c == '+' || c == '.' || c == '-'
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

// pieces returns the URL-shaped tokens inside tok, a token of nextToken,
// when it is cut at white space that is not ASCII too, and whether it
// holds any such white space. The one URL that redact.URL reads in
// "a://b/<NBSP>rtsp://u:pw@h/s" has nothing to hide; the second of the
// two a reader sees has. ok is false when there are more than maxPieces.
func pieces(tok string) (ps []span, cut, ok bool) {
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
		t, found := nextToken(tok, at, true)
		if !found {
			return ps, true, true
		}
		if len(ps) == maxPieces {
			return nil, true, false
		}
		ps, at = append(ps, t), t.end
	}
}

// tokens returns where the URL-shaped tokens of s are.
func tokens(s string) []span {
	var out []span
	for at := 0; ; {
		t, ok := nextToken(s, at, false)
		if !ok {
			return out
		}
		out, at = append(out, t), t.end
	}
}

// replaceTokens returns s with each URL-shaped token replaced by f of it.
func replaceTokens(s string, f func(tok string) string) string {
	var b strings.Builder
	at := 0
	for _, t := range tokens(s) {
		b.WriteString(s[at:t.at])
		b.WriteString(f(s[t.at:t.end]))
		at = t.end
	}
	if at == 0 {
		return s
	}
	b.WriteString(s[at:])
	return b.String()
}

// passRests finds the passphrase key in the lines of s, in any ASCII case,
// with an optional closing quote and a ':' or '=' after it, and returns
// what follows each on its line: the first on a line, since what follows
// it runs to the line's end. A line ends at any of YAML's line breaks.
func passRests(s string) []span {
	var out []span
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			continue
		}
		const before, after = "srt", "passphrase"
		if i < len(before) || len(s)-i-1 < len(after) ||
			!strings.EqualFold(s[i-len(before):i], before) || !strings.EqualFold(s[i+1:i+1+len(after)], after) {
			continue
		}
		j := i + 1 + len(after)
		if j < len(s) && (s[j] == '"' || s[j] == '\'') {
			j++
		}
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j == len(s) || s[j] != ':' && s[j] != '=' {
			continue
		}
		end := j + 1
		for end < len(s) && s[end] != '\n' && s[end] != '\r' && wideBreak(s[end:]) == 0 {
			end++
		}
		out = append(out, span{j + 1, end})
		i = end - 1
	}
	return out
}

// wideBreak is the length of the NEL, LS or PS that s starts with, or 0.
func wideBreak(s string) int {
	for _, b := range wideBreaks {
		if strings.HasPrefix(s, string(b)) {
			return len(b)
		}
	}
	return 0
}

// The bounds that keep the work on one token small and fixed: a token
// longer than maxToken is not read at all and hidden whole; at most
// maxTail closing bytes are cut from a token's end, and at most tailTries
// of the prefixes that leaves are tried; and at most maxNested URLs, each
// after a '#' inside the one before, are judged in one token, and at most
// maxPieces URLs between white space that is not ASCII.
const (
	maxToken  = 4 << 10
	maxTail   = 12
	tailTries = 4
	maxNested = 8
	maxPieces = 8
)

// maskedComment is what a comment that is hidden whole reads.
const maskedComment = "# " + redact.Mask

// commentChange is a line comment the tree now holds as maskedComment.
type commentChange struct {
	n   *yaml.Node
	old string
}

// masker hides the secrets of a node tree and records what it changed.
type masker struct {
	changes []change
	// comments is set when a comment's text was changed (commentLine).
	comments bool
	// lines are the line comments that were hidden whole.
	lines []commentChange
	// scan remembers the tokens of the text being masked; it may be nil.
	scan *scan
	// url is the value of the url key being walked.
	url *yaml.Node
	// aliased are the alias targets already hidden under a passphrase
	// key.
	aliased map[*yaml.Node]bool
	// recoded is set when something changed that no edit of the bytes
	// can follow: a tag was dropped, or a value was reached through an
	// alias.
	recoded bool
}

func (m *masker) changed() bool {
	return len(m.changes) > 0 || len(m.lines) > 0 || m.comments || m.recoded
}

// node hides the secrets under n.
func (m *masker) node(n *yaml.Node) {
	n.HeadComment = m.comment(n.HeadComment)
	n.LineComment = m.comment(n.LineComment)
	n.FootComment = m.comment(n.FootComment)
	if strings.Contains(n.Tag, "://") && maskValue(n.Tag) != n.Tag {
		// A tag that is a URL with a secret: nothing reads it, it goes.
		n.Tag, n.Style, m.recoded = "", n.Style&^yaml.TaggedStyle, true
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.ShortTag() == binaryTag {
			// Its value is base64 of what the decoder hands on: what it
			// holds cannot be judged by its text, so it is hidden whole.
			if n.Value != "" {
				m.hide(n)
			}
			return
		}
		m.set(n, maskScalar(n.Value, n == m.url))
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if passKey(k) {
				m.lineComment(k)
				m.under(v)
			}
			m.node(k)
			if k.Kind == yaml.ScalarNode && k.Value == "url" {
				m.url = v
			}
			m.node(v)
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			m.node(c)
		}
	case yaml.AliasNode:
		// Its target is hidden where it is written.
	}
}

// binaryTag is the tag of a scalar written in base64.
const binaryTag = "!!binary"

// passKey reports whether the mapping key k is srt_passphrase as the
// decoder reads it: in any case, also behind an alias or written in
// base64.
func passKey(k *yaml.Node) bool {
	for range 8 {
		if k.Kind != yaml.AliasNode || k.Alias == nil {
			break
		}
		k = k.Alias
	}
	if k.Kind != yaml.ScalarNode {
		return false
	}
	name := k.Value
	if k.ShortTag() == binaryTag {
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(name), ""))
		if err != nil {
			return false
		}
		name = string(b)
	}
	return strings.EqualFold(name, passphraseKey)
}

// under hides everything under the value v of an srt_passphrase key: every
// scalar that is not empty, keys included, whatever v's shape (a scalar, a
// list, a mapping, an alias of any of them), and the line comments beside
// them, where an old passphrase is often left. A plain null stays.
func (m *masker) under(v *yaml.Node) {
	if v.Kind == yaml.AliasNode && v.Alias != nil {
		m.lineComment(v)
		if m.aliased[v.Alias] {
			return // already hidden, or an alias of what holds it
		}
		if m.aliased == nil {
			m.aliased = make(map[*yaml.Node]bool)
		}
		m.aliased[v.Alias] = true
		before := len(m.changes) + len(m.lines)
		m.under(v.Alias)
		m.recoded = m.recoded || len(m.changes)+len(m.lines) > before
		return
	}
	m.lineComment(v)
	if v.Kind == yaml.ScalarNode {
		null := v.ShortTag() == "!!null" && v.Style&yaml.TaggedStyle == 0
		if v.Value != "" && v.Value != redact.Mask && !null {
			m.hide(v)
		}
		return
	}
	for _, c := range v.Content {
		m.under(c)
	}
}

// hide makes the scalar n the bare mask, a string: a written tag goes.
func (m *masker) hide(n *yaml.Node) {
	m.changes = append(m.changes, change{n, n.Value})
	if n.Style&yaml.TaggedStyle != 0 {
		n.Style, m.recoded = n.Style&^yaml.TaggedStyle, true
	}
	n.Value, n.Tag = redact.Mask, "!!str"
}

// lineComment hides n's line comment whole.
func (m *masker) lineComment(n *yaml.Node) {
	if n.LineComment != "" && n.LineComment != maskedComment {
		m.lines = append(m.lines, commentChange{n, n.LineComment})
		n.LineComment = maskedComment
	}
}

// set gives the scalar n the value v, which is text.
func (m *masker) set(n *yaml.Node, v string) {
	if v == n.Value {
		return
	}
	m.changes = append(m.changes, change{n, n.Value})
	setText(n, v)
}

// setText gives the scalar n the string v: a scalar that was a number
// without a written tag is a string now.
func setText(n *yaml.Node, v string) {
	n.Value = v
	if n.Style&yaml.TaggedStyle == 0 {
		n.Tag = "!!str"
	}
}

// scalarText is the scalar's value as a string field takes it: empty for
// a null.
func scalarText(n *yaml.Node) string {
	if n.ShortTag() == "!!null" {
		return ""
	}
	return n.Value
}

// comment returns the comment c as a response shows it, line by line
// (commentLine).
func (m *masker) comment(c string) string {
	if !strings.Contains(c, "://") && !strings.Contains(c, "_") {
		return c
	}
	lines := strings.Split(c, "\n")
	for i, line := range lines {
		lines[i] = m.scan.commentLine(line)
	}
	if r := strings.Join(lines, "\n"); r != c {
		c, m.comments = r, true
	}
	return c
}

// commentLine is one line of a comment as a response shows it. Each
// URL-shaped token is redacted (commentToken); then, as a scalar's value
// is judged whole, what runs from a token to the end of the line is
// (restOfLine): a commented-out URL whose secret holds a space; then
// whatever follows the passphrase key is hidden (maskPassText). That is
// repeated until nothing changes, and a line that does not settle is
// hidden whole: what a comment shows is what the scan accepts of a line.
func (s *scan) commentLine(line string) string {
	if !strings.Contains(line, "://") && !strings.Contains(line, "_") {
		return line
	}
	if s != nil {
		if r, ok := s.lines[line]; ok {
			return r
		}
	}
	out := line
	settled := false
	for range 4 {
		was := out
		out = maskPassText(restOfLine(s.commentTokens(out)))
		if settled = out == was; settled {
			break
		}
	}
	if !settled {
		out = "# " + redact.Invalid
	}
	if s != nil {
		s.lines[line] = out
	}
	return out
}

// commentTokens returns a comment's line with each URL-shaped token
// redacted (commentToken). Where a token cannot be read, the line ends
// with redact.Invalid in its place: what follows may be the rest of a
// secret that holds a space ("rtsp://admin:my pw@cam/s"), and a scalar
// with such a token is hidden whole too.
func (s *scan) commentTokens(line string) string {
	var b strings.Builder
	at := 0
	for _, t := range tokens(line) {
		r := s.commentToken(line[t.at:t.end])
		b.WriteString(line[at:t.at])
		b.WriteString(r)
		at = t.end
		if r == redact.Invalid {
			return b.String()
		}
	}
	if at == 0 {
		return line
	}
	b.WriteString(line[at:])
	return b.String()
}

// maxRests is how many URL-shaped tokens of a comment's line are judged
// together with the rest of the line.
const maxRests = 4

// restOfLine judges a comment's line as a scalar's whole value is judged:
// from each of its URL-shaped tokens, the first maxRests, to the end of
// the line. When redact.URL hides something in that (hidesNew) which it
// did not hide in the token alone, the rest of the line is replaced by
// redact.URL's form. So "# - url: https://c/x?token=AA BB" loses BB, as
// the scalar it was does. When that text cannot be read as a URL, the line
// ends with the token if what follows it may be the rest of a secret: it
// holds an '@' ("rtsp://admin:12 34@cam/s", whose token reads as a host and
// a port), or the token had something hidden in it (it carries the mask),
// as an unreadable token ends its line (commentTokens). The cost is prose:
// the words after an RTMP URL or a query secret go with it. A line longer
// than maxToken, or with more tokens, is judged token by token only.
func restOfLine(line string) string {
	if len(line) > maxToken {
		return line
	}
	ts := tokens(line)
	if len(ts) > maxRests {
		return line
	}
	for _, t := range ts {
		rest := strings.TrimRight(line[t.at:], " \t\r")
		if len(rest) == t.end-t.at {
			continue
		}
		// A tab is no part of a URL, and may be part of a secret as a
		// space is: the rest is read with a space in its place.
		r, hid, read := hidesNew(strings.ReplaceAll(rest, "\t", " "))
		switch {
		case hid:
			return line[:t.at] + r
		case !read && (strings.Contains(line[t.end:], "@") || strings.Contains(line[t.at:t.end], redact.Mask)):
			return line[:t.end]
		}
	}
	return line
}

// hidesNew returns redact.URL of v, whether it hid something there that
// was not hidden yet (its form differs from v as net/url alone writes it;
// comparing with v itself would take every escaped space for a secret),
// and whether v could be read as a URL at all. A v that cannot be read
// hides nothing here.
func hidesNew(v string) (r string, hid, read bool) {
	u, err := url.Parse(v)
	if err != nil {
		return v, false, false
	}
	if r = redact.URL(v); r == redact.Invalid {
		return v, false, false
	}
	return r, r != u.String(), true
}

// maskPassText returns the comment c with whatever follows the passphrase
// key and its ':' or '=' on a line replaced by the mask: a passphrase
// commented out, or noted beside its replacement.
func maskPassText(c string) string {
	if !strings.Contains(c, "_") {
		return c
	}
	var b strings.Builder
	at := 0
	for _, rest := range passRests(c) {
		if passTextShown(c[rest.at:rest.end]) {
			continue
		}
		b.WriteString(c[at:rest.at])
		b.WriteString(" " + redact.Mask)
		at = rest.end
	}
	if at == 0 {
		return c
	}
	b.WriteString(c[at:])
	return b.String()
}

// passTextShown reports whether rest, what follows the passphrase key on
// a line, may be shown: nothing, or the mask.
func passTextShown(rest string) bool {
	rest = strings.Trim(rest, " \t")
	return rest == "" || rest == redact.Mask
}

// cleanURL reports whether redact.URL reads p and hides nothing new in it.
// Hiding always writes the mask, so a form without the mask's text hid
// nothing, and a form equal to p is redact.URL's own.
func cleanURL(p string) bool {
	r := redact.URL(p)
	return r != redact.Invalid && (r == p || !strings.Contains(r, redact.Mask))
}

// maskValue is a scalar's value as a response shows it, judged both ways.
// Each URL-shaped token with something to hide is replaced by its
// redacted form (maskToken) and the other tokens keep their bytes. Then a
// value that starts with a URL and runs on past that token's white space
// is judged whole, as the stream API shows it (spec.Stream.Redacted) and
// as spec.KeepSecrets compares it: when redact.URL hides something in the
// whole that the tokens did not (a secret that holds a space), the value
// is redact.URL's form of it, so its mask comes back on an edit. That is
// repeated until nothing changes. The value is the bare mask when a token
// cannot be read or when it does not settle.
func maskValue(v string) string { return maskScalar(v, false) }

// maskScalar is maskValue; with url, v is the value of a url key, and one
// that does not parse as a whole is the bare mask, whatever it starts
// with, as the stream API shows it. Anywhere else such a value is text,
// judged by its tokens: a watermark text is not lost for holding a URL.
func maskScalar(v string, url bool) string {
	if !strings.Contains(v, "://") {
		return v
	}
	if url && redact.URL(strings.TrimSpace(v)) == redact.Invalid {
		return redact.Mask
	}
	for range 4 {
		was := v
		whole := false
		v = replaceTokens(v, func(tok string) string {
			r, ok := maskToken(tok)
			whole = whole || !ok
			return r
		})
		if whole {
			return redact.Mask
		}
		// The whole value, when it starts with a URL and is more than
		// that one token: the server and the stream API read it as one
		// URL, white space and all.
		if w := strings.TrimSpace(v); w != "" {
			if t, ok := nextToken(w, 0, false); ok && t.at == 0 && t.end < len(w) {
				if r, hid, _ := hidesNew(w); hid {
					v = r
				}
			}
		}
		if v == was {
			return v
		}
	}
	return redact.Mask
}

// hides reports whether r, redact.URL of tok, hid something in it. Hiding
// always writes the mask, so a form without the mask's text hid nothing,
// and a form equal to tok is redact.URL's own.
func hides(r, tok string) bool {
	return r != tok && strings.Contains(r, redact.Mask)
}

// maskToken is a scalar's URL-shaped token as a response shows it, judged
// both ways (bothWays): each URL between white space that is not ASCII
// goes through redact.URL (pieces), so a URL glued to another by such
// white space is hidden; and the whole token does, as the stream API shows
// the URL and as spec.KeepSecrets compares it, so a secret that holds such
// white space is hidden whole and not up to it. That is repeated until
// nothing changes: redact.URL escapes that white space in a URL it
// changed. ok is false when the token is longer than maxToken, when
// redact.URL cannot parse it or one of its pieces, when it has more than
// maxPieces, or when it does not settle.
func maskToken(tok string) (out string, ok bool) {
	if len(tok) > maxToken {
		return tok, false
	}
	strict := func(t string) (string, bool) {
		r := redact.URL(t)
		if r == redact.Invalid {
			return "", false
		}
		if !hides(r, t) {
			r = t
		}
		return r, true
	}
	for range 4 {
		was := tok
		if tok, ok = bothWays(tok, strict); !ok {
			return was, false
		}
		if tok == was {
			return tok, true
		}
	}
	return tok, false
}

// bothWays redacts tok by f in each of its pieces, then as a whole. The
// pieces come first: redacting the whole escapes the white space between
// them, and what was a URL of its own after it would no longer be seen as
// one. ok is false when f fails or there are more than maxPieces.
func bothWays(tok string, f func(string) (string, bool)) (out string, ok bool) {
	ps, cut, ok := pieces(tok)
	if !ok {
		return "", false
	}
	if cut {
		var b strings.Builder
		at := 0
		for _, p := range ps {
			r, ok := f(tok[p.at:p.end])
			if !ok {
				return "", false
			}
			b.WriteString(tok[at:p.at])
			b.WriteString(r)
			at = p.end
		}
		b.WriteString(tok[at:])
		tok = b.String()
	}
	return f(tok)
}

// nested returns where the URL-shaped tokens of tok start that the scan
// judges: tok itself at 0, then the one after the first '#' of the one
// before. ok is false when there are more than maxNested.
func nested(tok string) (starts []int, ok bool) {
	starts = append(starts, 0)
	for {
		at := starts[len(starts)-1]
		i := strings.IndexByte(tok[at:], '#')
		if i < 0 {
			return starts, true
		}
		m, found := nextToken(tok, at+i+1, false)
		if !found {
			return starts, true
		}
		if len(starts) == maxNested {
			return nil, false
		}
		starts = append(starts, m.at)
	}
}

// prefixes calls try with the prefixes of tok the tail rule allows, until
// it returns true: tok itself, then tok without its trailing bytes of
// tokenTail (at most maxTail of them), then that with one closing byte
// back, and so on, tailTries prefixes after the first. The shortest comes
// before the ones between because it is the URL without YAML's
// punctuation; the next ones are a URL that ends in such a byte itself
// (http://[::1]).
func prefixes(tok string, try func(p string) bool) bool {
	if try(tok) {
		return true
	}
	end := len(tok)
	for end > 0 && len(tok)-end < maxTail && strings.IndexByte(tokenTail, tok[end-1]) >= 0 {
		end--
	}
	for n := 0; end < len(tok) && n < tailTries; end, n = end+1, n+1 {
		if try(tok[:end]) {
			return true
		}
	}
	return false
}

// redactTail returns tok with the first of its prefixes that redact.URL
// parses redacted and the cut tail kept; ok is false when none parses.
func redactTail(tok string) (out string, ok bool) {
	ok = prefixes(tok, func(p string) bool {
		r := redact.URL(p)
		if r == redact.Invalid {
			return false
		}
		out = tok
		if r != p && strings.Contains(r, redact.Mask) {
			out = r + tok[len(p):]
		}
		return true
	})
	return out, ok
}

// maskCommentToken is a comment's URL-shaped token as a response shows it.
// A token the scan finds clean (cleanRaw) keeps its bytes. Otherwise it
// is redacted both ways, as a scalar's token is (maskToken): as it is,
// and in each URL between white space that is not ASCII, each time by
// redactNested, until nothing changes. It is redact.Invalid when the
// token is longer than maxToken, holds more than maxNested URLs or
// maxPieces pieces, when one of them does not parse, or when the scan
// would still not find the result clean: what a comment shows is always
// what the scan accepts.
func maskCommentToken(tok string) string {
	return (*scan)(nil).commentToken(tok)
}

// cleanToken is the scan's rule for a raw token (R-T8): it is clean when
// the token, or a prefix of it made by cutting trailing bytes of tokenTail
// (prefixes), parses and gets no new mask from redact.URL. Only the tail
// is ever cut, so a secret is inside every prefix. A token longer than
// maxToken is never clean.
func cleanToken(tok string) bool {
	return len(tok) <= maxToken && prefixes(tok, cleanURL)
}

// cleanNested is cleanToken for a token of the text or of a comment, where
// a '#' may start a comment: the parser takes one after a closing quote, a
// bracket or a comma without white space between, so the URL-shaped token
// after the first '#' is judged by itself as well, and so on inside it
// (nested). ('b://h'#rtsp://u:pw@h is, to redact.URL, one URL with a
// fragment and nothing to hide.) A token with more than maxNested of them
// is never clean.
func cleanNested(tok string) bool {
	starts, ok := nested(tok)
	if !ok {
		return false
	}
	for _, at := range starts {
		if !cleanToken(tok[at:]) {
			return false
		}
	}
	return true
}

// cleanRaw is the scan's whole rule for a token of the text or of a
// comment: it is clean when cleanNested finds it so both as it is and in
// each of the URLs between white space that is not ASCII (pieces). A token
// longer than maxToken or with more than maxPieces is never clean.
func cleanRaw(tok string) bool {
	if len(tok) > maxToken || !cleanNested(tok) {
		return false
	}
	ps, _, ok := pieces(tok)
	if !ok {
		return false
	}
	for _, p := range ps {
		if !cleanNested(tok[p.at:p.end]) {
			return false
		}
	}
	return true
}

// redactNested redacts each URL of tok that cleanNested judges by
// redactTail, the innermost first, while it can still be read as a URL:
// redacting the one around it may take its "://" away. ok is false when
// there are more than maxNested or one does not parse.
func redactNested(tok string) (out string, ok bool) {
	starts, ok := nested(tok)
	if !ok {
		return "", false
	}
	for _, at := range slices.Backward(starts) {
		r, ok := redactTail(tok[at:])
		if !ok {
			return "", false
		}
		tok = tok[:at] + r
	}
	return tok, true
}

// scan remembers what was found of each token while one text is masked:
// the tree is walked and the bytes are searched once per text Mask
// produces, and they hold the same tokens. A nil scan remembers nothing.
type scan struct {
	clean    map[string]bool
	redacted map[string]string
	lines    map[string]string
}

func newScan() *scan {
	return &scan{clean: make(map[string]bool), redacted: make(map[string]string), lines: make(map[string]string)}
}

// cleanRaw is cleanRaw, remembered.
func (s *scan) cleanRaw(tok string) bool {
	if s == nil {
		return cleanRaw(tok)
	}
	c, ok := s.clean[tok]
	if !ok {
		c = cleanRaw(tok)
		s.clean[tok] = c
	}
	return c
}

// commentToken is maskCommentToken, remembered.
func (s *scan) commentToken(tok string) string {
	if len(tok) > maxToken {
		return redact.Invalid
	}
	if s != nil {
		if r, ok := s.redacted[tok]; ok {
			return r
		}
	}
	r := s.redactToken(tok)
	if s != nil {
		s.redacted[tok] = r
	}
	return r
}

// redactToken does commentToken's work.
func (s *scan) redactToken(tok string) string {
	if s.cleanRaw(tok) {
		return tok
	}
	for range 4 {
		was := tok
		var ok bool
		if tok, ok = bothWays(tok, redactNested); !ok {
			return redact.Invalid
		}
		if tok == was {
			break
		}
	}
	if !s.cleanRaw(tok) {
		return redact.Invalid
	}
	return tok
}

// The line breaks of YAML that are not ASCII: NEL, LS and PS.
var wideBreaks = [][]byte{{0xC2, 0x85}, {0xE2, 0x80, 0xA8}, {0xE2, 0x80, 0xA9}}

// rawClean is the postcondition on the bytes (R-T8): no URL-shaped token
// of text has something to hide (cleanRaw), and no '#' is followed on its
// line by the passphrase key with anything but the mask after it. root is
// text decoded. The bytes of a double-quoted scalar are not judged: that
// scalar's value was, in the tree; what follows its closing quote is
// judged as a token of its own. A text in UTF-16 is never clean: its
// bytes cannot be searched, and encoding it again writes UTF-8.
func rawClean(text []byte, root *yaml.Node) bool {
	return (*scan)(nil).rawClean(text, root)
}

func (s *scan) rawClean(text []byte, root *yaml.Node) bool {
	if bytes.HasPrefix(text, []byte{0xFF, 0xFE}) || bytes.HasPrefix(text, []byte{0xFE, 0xFF}) {
		return false
	}
	urls, hash := bytes.Contains(text, []byte("://")), bytes.IndexByte(text, '#') >= 0
	if !urls && !hash {
		return true
	}
	if quoted := quotedSpans(text, root); len(quoted) > 0 {
		// The spans are blanked in a copy, line ends kept.
		text = bytes.Clone(text)
		for _, q := range quoted {
			for i := q.at; i < q.end; i++ {
				if text[i] != '\n' && text[i] != '\r' {
					text[i] = ' '
				}
			}
		}
	}
	str := string(text)
	if urls {
		for _, t := range tokens(str) {
			if t.end-t.at > maxToken || !s.cleanRaw(str[t.at:t.end]) {
				return false
			}
		}
	}
	if hash {
		// What follows a '#' on its line is, or may be, a comment: it
		// must read as a comment's line is shown.
		clean := true
		commentRegions(str, func(r span) {
			clean = clean && s.commentLine(str[r.at:r.end]) == str[r.at:r.end]
		})
		return clean
	}
	return true
}

// commentRegions calls f with each part of s that runs from the first
// '#' of a line to the line's end.
func commentRegions(s string, f func(span)) {
	for at := 0; at < len(s); {
		i := strings.IndexByte(s[at:], '#')
		if i < 0 {
			return
		}
		at += i
		end := at
		for end < len(s) && s[end] != '\n' && s[end] != '\r' {
			end++
		}
		f(span{at, end})
		at = end
	}
}

// span is the bytes [at, end) of a text.
type span struct{ at, end int }

// quotedSpans returns where the double-quoted scalars under root that
// hold "://" or a '#' are written in text, from the opening quote to the
// closing one, in order. A span is only returned when its bytes, decoded by
// themselves, are the node's value: then they hold nothing the value does
// not, wherever the node's position was taken from.
func quotedSpans(text []byte, root *yaml.Node) []span {
	at := newLines(text)
	var out []span
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && n.Style&yaml.DoubleQuotedStyle != 0 && hidable(n.Value) {
			if s, ok := quotedSpan(text, at.offset(n.Line, n.Column)); ok {
				var v string
				if yaml.Unmarshal(text[s.at:s.end], &v) == nil && v == n.Value {
					out = append(out, s)
				}
			}
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	visit(root)
	slices.SortFunc(out, func(a, b span) int { return a.at - b.at })
	return out
}

// quotedSpan finds the double-quoted scalar written at i, after its
// anchor and tag if it has them on the same line: from its opening quote
// to the first quote that no backslash escapes.
func quotedSpan(text []byte, i int) (span, bool) {
	if i < 0 {
		return span{}, false
	}
	for i < len(text) && (text[i] == '&' || text[i] == '!') {
		for i < len(text) && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' {
			i++
		}
		for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
	}
	if i >= len(text) || text[i] != '"' {
		return span{}, false
	}
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case '"':
			return span{i, j + 1}, true
		}
	}
	return span{}, false
}

// lineStarts returns the offset of the first byte of each line of text,
// with the parser's line breaks: LF, CR, CRLF, NEL, LS and PS. A byte
// order mark is before the first line.
func lineStarts(text []byte) []int {
	i := 0
	if bytes.HasPrefix(text, []byte{0xEF, 0xBB, 0xBF}) {
		i = 3
	}
	starts := []int{i}
	for i < len(text) {
		switch {
		case text[i] == '\n':
			i++
		case text[i] == '\r':
			i++
			if i < len(text) && text[i] == '\n' {
				i++
			}
		case bytes.HasPrefix(text[i:], wideBreaks[0]):
			i += 2
		case bytes.HasPrefix(text[i:], wideBreaks[1]), bytes.HasPrefix(text[i:], wideBreaks[2]):
			i += 3
		default:
			i++
			continue
		}
		starts = append(starts, i)
	}
	return starts
}

// lines finds where a node's Line and Column point in a text. A column
// counts characters, so a line that is not ASCII is indexed once, the
// first time a position on it is asked for: finding each of the thousands
// of scalars of a config written on one line by walking the line from its
// start would be quadratic.
type lines struct {
	text   []byte
	starts []int
	// marks holds, for each line asked about, the offset of every
	// markEvery-th character; nil for a line that is ASCII, where a
	// character is a byte.
	marks map[int][]int
}

const markEvery = 64

func newLines(text []byte) *lines {
	return &lines{text: text, starts: lineStarts(text), marks: make(map[int][]int)}
}

// offset returns where line and column, which count from 1, point in the
// text; -1 when they point nowhere.
func (l *lines) offset(line, column int) int {
	if line < 1 || line > len(l.starts) || column < 1 {
		return -1
	}
	at, end := l.starts[line-1], len(l.text)
	if line < len(l.starts) {
		end = l.starts[line]
	}
	marks, ok := l.marks[line]
	if !ok {
		ascii := true
		for i := at; i < end && ascii; i++ {
			ascii = l.text[i] < utf8.RuneSelf
		}
		if !ascii {
			for i, n := at, 0; i < end; n++ {
				if n%markEvery == 0 {
					marks = append(marks, i)
				}
				_, w := utf8.DecodeRune(l.text[i:end])
				i += w
			}
		}
		l.marks[line] = marks
	}
	i := at + column - 1
	if marks != nil {
		k := (column - 1) / markEvery
		if k >= len(marks) {
			return -1
		}
		i = marks[k]
		for n := k * markEvery; n < column-1 && i < end; n++ {
			_, w := utf8.DecodeRune(l.text[i:end])
			i += w
		}
	}
	if i >= end || i >= len(l.text) {
		return -1
	}
	return i
}

// patch is the bytes [at, end) of a text replaced by text.
type patch struct {
	span
	text string
}

// patch returns text with every change written in place: each changed
// scalar at its position, each line comment that was hidden whole on its
// node's line, and, when a comment's text changed, what follows the first
// '#' of each line as a comment's line is shown (commentLine). ok is false
// when one of the first two cannot be written in place. Where two overlap
// the earlier kind stands. The caller decodes the result and compares it
// with the tree: that, not the replacement, is what the result rests on.
func (m *masker) patch(text []byte) (out []byte, ok bool) {
	if m.recoded {
		return nil, false
	}
	patches, ok := scalarPatches(text, m.changes)
	if !ok {
		return nil, false
	}
	kinds := make([]int, len(patches)) // the kind of each patch; a lower one stands
	add := func(kind int, p patch) {
		patches, kinds = append(patches, p), append(kinds, kind)
	}
	if len(m.lines) > 0 {
		starts := lineStarts(text)
		for _, c := range m.lines {
			// The comment is on the line the node starts on, at its end.
			if c.n.Line < 1 || c.n.Line > len(starts) {
				return nil, false
			}
			at := starts[c.n.Line-1]
			end := len(text)
			if c.n.Line < len(starts) {
				end = starts[c.n.Line]
			}
			i := bytes.LastIndex(text[at:end], []byte(c.old))
			if i < 0 {
				return nil, false
			}
			add(1, patch{span{at + i, at + i + len(c.old)}, maskedComment})
		}
	}
	if m.comments {
		str := string(text)
		commentRegions(str, func(r span) {
			if shown := m.scan.commentLine(str[r.at:r.end]); shown != str[r.at:r.end] {
				add(2, patch{r, shown})
			}
		})
	}
	if len(patches) > len(m.changes) {
		order := make([]int, len(patches))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, b int) int {
			if c := patches[a].at - patches[b].at; c != 0 {
				return c
			}
			return kinds[a] - kinds[b]
		})
		kept := make([]patch, 0, len(patches))
		last := -1 // the kind of the last patch kept
		for _, i := range order {
			p := patches[i]
			if n := len(kept); n > 0 && p.at < kept[n-1].end {
				if kinds[i] >= last {
					continue
				}
				// A patch of an earlier kind starts inside a later
				// kind's: it must not reach back into the one before.
				if n > 1 && p.at < kept[n-2].end {
					return nil, false
				}
				kept = kept[:n-1]
			}
			kept, last = append(kept, p), kinds[i]
		}
		patches = kept
	}
	return applyPatches(text, patches), true
}

// scalarPatches returns the patches that write every change in place, in
// the text's order; ok is false when one cannot be written in place or two
// overlap.
func scalarPatches(text []byte, changes []change) (patches []patch, ok bool) {
	at := newLines(text)
	for _, c := range changes {
		p, ok := scalarPatch(text, at.offset(c.n.Line, c.n.Column), c)
		if !ok {
			return nil, false
		}
		patches = append(patches, p)
	}
	slices.SortFunc(patches, func(a, b patch) int { return a.at - b.at })
	for i := 1; i < len(patches); i++ {
		if patches[i].at < patches[i-1].end {
			return nil, false
		}
	}
	return patches, true
}

// scalarPatch writes the scalar c.n, which text holds at i with the value
// c.old, with its new value, in the style it has. It can when the scalar
// is written on one line with no escape: plain, in single quotes, or in
// double quotes with neither a backslash nor a quote in the old and the
// new value. A plain scalar whose new value is no plain string (a
// passphrase of digits) is written in double quotes. An anchored or
// tagged scalar's position is its anchor's or tag's, so it never matches.
func scalarPatch(text []byte, i int, c change) (patch, bool) {
	if i < 0 || c.old == "" || !printable(c.n.Value) {
		return patch{}, false
	}
	rest := text[i:]
	var was, now string
	switch c.n.Style {
	case 0:
		was, now = c.old, c.n.Value
		if !plain(now) {
			b, err := json.Marshal(now)
			if err != nil {
				return patch{}, false
			}
			now = string(b)
		}
	case yaml.SingleQuotedStyle:
		was = "'" + strings.ReplaceAll(c.old, "'", "''") + "'"
		now = "'" + strings.ReplaceAll(c.n.Value, "'", "''") + "'"
	case yaml.DoubleQuotedStyle:
		if strings.ContainsAny(c.old, `\"`) || strings.ContainsAny(c.n.Value, `\"`) {
			return patch{}, false
		}
		was, now = `"`+c.old+`"`, `"`+c.n.Value+`"`
	case yaml.TaggedStyle, yaml.LiteralStyle, yaml.FoldedStyle, yaml.FlowStyle:
		return patch{}, false
	default:
		return patch{}, false
	}
	if !bytes.HasPrefix(rest, []byte(was)) {
		return patch{}, false
	}
	return patch{span{i, i + len(was)}, now}, true
}

// printable reports whether v can be written between quotes as it is: no
// control character, no line break of any kind and valid UTF-8.
func printable(v string) bool {
	for _, r := range v {
		if r < 0x20 || r == 0x7F || r == 0x85 || r == 0x2028 || r == 0x2029 || r == 0xFEFF || r == utf8.RuneError {
			return false
		}
	}
	return v != ""
}

// plain reports whether v, written as a plain scalar after a key, is read
// back as the string v. A value that fails only inside a flow collection
// is found by the caller's comparison.
func plain(v string) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("k: "+v+"\n"), &doc); err != nil {
		return false
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return false
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode || len(m.Content) != 2 {
		return false
	}
	n := m.Content[1]
	return n.Kind == yaml.ScalarNode && n.Style == 0 && n.ShortTag() == "!!str" && n.Value == v &&
		n.HeadComment == "" && n.LineComment == "" && n.FootComment == "" && m.FootComment == ""
}

// applyPatches returns text with the patches, which are in order and do
// not overlap, written.
func applyPatches(text []byte, patches []patch) []byte {
	out := make([]byte, 0, len(text)+16*len(patches))
	at := 0
	for _, p := range patches {
		out = append(out, text[at:p.at]...)
		out = append(out, p.text...)
		at = p.end
	}
	return append(out, text[at:]...)
}

// sameTree reports whether two node trees are the same document: kinds,
// tags, values, anchors and comments, whatever their styles and positions.
func sameTree(a, b *yaml.Node) bool {
	if a.Kind != b.Kind || a.ShortTag() != b.ShortTag() || a.Value != b.Value || a.Anchor != b.Anchor ||
		a.HeadComment != b.HeadComment || a.LineComment != b.LineComment || a.FootComment != b.FootComment ||
		len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		if !sameTree(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// hidable reports whether the bytes of a scalar with the value v must be
// kept from the scan: it holds a URL, or a '#' that the scan could take
// for the start of a comment.
func hidable(v string) bool {
	return strings.Contains(v, "://") || strings.Contains(v, "#")
}

// quoteHidable puts every scalar under root that is hidable in double
// quotes.
func quoteHidable(root *yaml.Node) {
	if root.Kind == yaml.ScalarNode && hidable(root.Value) {
		root.Style = root.Style&yaml.TaggedStyle | yaml.DoubleQuotedStyle
	}
	for _, c := range root.Content {
		quoteHidable(c)
	}
}

// Unmask returns sent with every inputs[i].url, outputs.push[j].url and
// outputs.srt_passphrase of a stream entry that holds the mask replaced by
// the value the entries of the same name in stored hold for it (R-T4:
// spec.KeepSecrets per list, its order rule included). When sent equals
// Mask(stored) byte for byte it returns stored, the same slice. A sent
// text without a mask is returned as it is, the same slice. A sent text
// that is not YAML is returned as it is: Parse reports it. A mask that
// cannot be replaced is an Errors, one per place, as
// "streams[3] (cam): inputs[0].url: holds the redaction mask and the
// stored stream has no one secret it stands for; send the real value".
//
// A URL that holds a masked token beside other text is refused in the
// same words, and so is an entry of inputs or of outputs.push that holds
// a masked token anywhere but in its url, named "inputs[0]" (R-T8).
// Nothing else is looked at: a mask under any other key is stored as
// sent. The values are written in place, and the result is decoded and
// compared; when one cannot be written in place the document is
// re-encoded through the node tree, as SetStream does.
func Unmask(sent, stored []byte) ([]byte, error) {
	// First, and also for a text without a mask: Mask may have dropped a
	// comment from a text that holds no other secret.
	if shownStored, err := Mask(stored); err == nil && bytes.Equal(shownStored, sent) {
		return stored, nil
	}
	if !bytes.Contains(sent, []byte(redact.Mask)) {
		return sent, nil
	}
	root := tree(sent)
	if root == nil {
		return sent, nil
	}
	var kept *yaml.Node
	if r := tree(stored); r != nil {
		kept = streamsNode(r)
	}
	changes, refused := resolve(streamsNode(root), kept)
	if len(refused) > 0 {
		return nil, Errors(cappedErrors(refused))
	}
	if len(changes) == 0 {
		return sent, nil
	}
	if patches, ok := scalarPatches(sent, changes); ok {
		out := applyPatches(sent, patches)
		if got := tree(out); got != nil && sameTree(got, root) {
			return out, nil
		}
	}
	out, err := encode(root, "unmask")
	if err != nil {
		return nil, err
	}
	if unresolved(out, kept) {
		return nil, errors.New("config: unmask: the resolved text still holds a mask")
	}
	return out, nil
}

// unresolved reports whether text, which Unmask is about to return after
// encoding it, does not decode or still holds a mask in a place Unmask
// resolves, against the stored entries kept.
func unresolved(text []byte, kept *yaml.Node) bool {
	got := tree(text)
	if got == nil {
		return true
	}
	changes, refused := resolve(streamsNode(got), kept)
	return len(changes) > 0 || len(refused) > 0
}

// tree is text decoded, or nil when it is not one YAML document.
func tree(text []byte) *yaml.Node {
	root, err := decodeNode(text)
	if err != nil {
		return nil
	}
	return root
}

// resolve replaces the masks in the stream entries of seq by the values
// the entries of the same name in stored hold (either may be nil). It
// returns the scalars it changed and one error per mask it could not
// replace, in the file's order of entries.
func resolve(seq, stored *yaml.Node) (changes []change, refused []error) {
	if seq == nil {
		return nil, nil
	}
	// The stored entries' places by name, read once: a text may hold any
	// number of entries, of one name too.
	kept := make(map[string]*secrets)
	if stored != nil {
		for _, e := range stored.Content {
			if name, ok := entryName(e); ok {
				if kept[name] == nil {
					kept[name] = new(secrets)
				}
				kept[name].add(e)
			}
		}
	}
	for i, e := range seq.Content {
		if e.Kind != yaml.MappingNode {
			continue
		}
		var old secrets
		if name, ok := entryName(e); ok {
			if s := kept[name]; s != nil {
				old = *s
			}
		}
		var now secrets
		now.add(e)
		refuse := func(field string) {
			refused = append(refused, errors.New(entryRef(i, e)+": "+spec.MaskRefusal(field)))
		}
		keep := func(list string, sent, stored []item) {
			if len(sent) > maxList || len(stored) > maxList {
				// No stream has so many: nothing is resolved, and the
				// work stays linear in the text.
				for j, it := range sent {
					field := list + "[" + strconv.Itoa(j) + "]"
					switch {
					case it.stray:
						refuse(field)
					case it.url == redact.Mask || redact.Masked(it.url):
						refuse(field + ".url")
					}
				}
				return
			}
			urls := make([]string, len(sent))
			for j, it := range sent {
				urls[j] = it.url
			}
			olds := make([]string, len(stored))
			for j, it := range stored {
				olds[j] = it.url
			}
			out, bad := spec.KeepSecrets(urls, olds)
			for j, it := range sent {
				field := list + "[" + strconv.Itoa(j) + "]"
				switch {
				case it.stray:
					refuse(field)
				case slices.Contains(bad, j):
					refuse(field + ".url")
				case out[j] != it.url:
					changes = append(changes, change{it.n, it.n.Value})
					setText(it.n, out[j])
				}
			}
		}
		keep("inputs", now.inputs, old.inputs)
		keep("outputs.push", now.pushes, old.pushes)
		if p := now.passphrase; p != nil {
			kept := ""
			if len(old.passphrases) == 1 {
				kept = old.passphrases[0]
			}
			out, ok := spec.KeepPassphrase(scalarText(p), kept)
			switch {
			case !ok:
				refuse("outputs." + passphraseKey)
			case out != scalarText(p):
				changes = append(changes, change{p, p.Value})
				setText(p, out)
			}
		}
	}
	return changes, refused
}

// maxList is the longest inputs or outputs.push list, of a sent entry or
// of the stored entries of its name together, that Unmask resolves. The
// spec allows far fewer in a stream.
const maxList = 64

// item is one entry of an inputs or outputs.push list, as KeepSecrets
// takes it.
type item struct {
	n   *yaml.Node // the url scalar, or nil
	url string     // its value; the bare mask when it holds a masked token beside other text
	// stray is set when the entry holds a mask KeepSecrets never sees: a
	// masked token outside its url, or an entry that is a scalar.
	stray bool
}

// secrets are the places of stream entries that Unmask resolves.
type secrets struct {
	inputs, pushes []item
	passphrase     *yaml.Node
	passphrases    []string // the distinct passphrases of the entries added, none empty
}

// add adds the stream entry e's places.
func (s *secrets) add(e *yaml.Node) {
	s.inputs = append(s.inputs, items(child(e, "inputs"))...)
	outputs := child(e, "outputs")
	s.pushes = append(s.pushes, items(child(outputs, "push"))...)
	if p := child(outputs, passphraseKey); p != nil && p.Kind == yaml.ScalarNode && p.ShortTag() != binaryTag {
		if s.passphrase == nil {
			s.passphrase = p
		}
		// Two different ones are as good as any number: the mask then
		// stands for no one of them.
		if v := scalarText(p); v != "" && len(s.passphrases) < 2 && !slices.Contains(s.passphrases, v) {
			s.passphrases = append(s.passphrases, v)
		}
	}
}

// child returns the value of m's first key named key, or nil when m is
// nil, is no mapping or has no such key.
func child(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key && k.ShortTag() != binaryTag {
			return m.Content[i+1]
		}
	}
	return nil
}

// items returns the entries of the list seq (nil for anything else).
func items(seq *yaml.Node) []item {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]item, len(seq.Content))
	for i, e := range seq.Content {
		it := &out[i]
		if u := child(e, "url"); u != nil && u.Kind == yaml.ScalarNode && u.ShortTag() != binaryTag {
			it.n, it.url = u, u.Value
			if it.url != redact.Mask && !redact.Masked(it.url) && holdsMask(it.url) {
				it.url = redact.Mask
			}
		}
		it.stray = strayMask(e, it.n)
	}
	return out
}

// holdsMask reports whether v holds a URL-shaped token that redact.Masked
// reports.
func holdsMask(v string) bool {
	if !strings.Contains(v, redact.Mask) {
		return false
	}
	return slices.ContainsFunc(tokens(v), func(t span) bool { return redact.Masked(v[t.at:t.end]) })
}

// strayMask reports whether the list entry e holds a mask outside its url
// scalar: a scalar entry that is the bare mask, or any scalar under it
// but url, key or value, that holds a masked token.
func strayMask(e, url *yaml.Node) bool {
	if e == url {
		return false
	}
	if e.Kind == yaml.ScalarNode && (e.Value == redact.Mask || holdsMask(e.Value)) {
		return true
	}
	for _, c := range e.Content {
		if strayMask(c, url) {
			return true
		}
	}
	return false
}
