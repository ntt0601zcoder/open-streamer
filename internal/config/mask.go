package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/ntt0601zcoder/open-streamer/internal/logging"
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
// mask is replaced by that form, and a scalar holding a token redact.URL
// cannot parse becomes redact.Mask whole; every non-empty srt_passphrase
// value becomes redact.Mask; in every comment each URL-shaped token goes
// through redact.URL. Everything else is copied byte for byte, and a text
// that needs nothing hidden is returned as it is, the same slice. When a
// scalar cannot be replaced in place, or the output's bytes still hold a
// URL-shaped token with something to hide (a comment the parser dropped),
// the document is re-encoded through the node tree, as SetStream does;
// when that text still holds such a token, once more with every scalar
// that holds a URL in double quotes. It returns an error, and no text,
// when the text is not YAML or when the re-encoded bytes still hold such
// a token.
//
// Whatever path it took, the text it returns was decoded again and holds
// nothing Mask would hide, so Mask of it is the same text.
func Mask(text []byte) ([]byte, error) {
	root, err := decodeNode(text)
	if err != nil {
		return nil, &SyntaxError{Err: err}
	}
	var m masker
	m.node(root)
	if !m.changed() {
		if rawClean(text, root) {
			return text, nil
		}
	} else if out, ok := m.patch(text); ok && shown(out, root) {
		return out, nil
	}
	// The node tree now holds what must be shown: encode it. The second
	// round puts every URL in double quotes, where its bytes are judged
	// by its value and YAML's punctuation cannot touch it.
	for _, quote := range []bool{false, true} {
		if quote {
			quoteURLs(root)
		}
		if out, err := encode(root, "mask"); err == nil && shown(out, nil) {
			return out, nil
		}
	}
	return nil, errors.New("config: mask: the text cannot be shown with its secrets hidden")
}

// shown reports whether out is fit for a response: it decodes, to want
// when that is given, the tree holds nothing Mask would hide and neither
// do the bytes.
func shown(out []byte, want *yaml.Node) bool {
	root, err := decodeNode(out)
	if err != nil || (want != nil && !sameTree(root, want)) {
		return false
	}
	var m masker
	m.node(root)
	return !m.changed() && rawClean(out, root)
}

// change is one scalar whose value the tree now holds differently from
// the text: old is the value the text holds.
type change struct {
	n   *yaml.Node
	old string
}

// masker hides the secrets of a node tree and records what it changed.
type masker struct {
	changes []change
	// tokens maps each comment token that was redacted to its redacted
	// form.
	tokens map[string]string
	// recoded is set when something changed that no edit of the bytes
	// can follow: a tag was dropped, or a value was reached through an
	// alias.
	recoded bool
}

func (m *masker) changed() bool {
	return len(m.changes) > 0 || len(m.tokens) > 0 || m.recoded
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
		m.set(n, maskValue(n.Value))
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			m.node(k)
			if k.Kind == yaml.ScalarNode && k.Value == passphraseKey {
				m.passphrase(v)
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

// passphrase hides the value v of an srt_passphrase key: a scalar that is
// not empty, also when v is an alias of it.
func (m *masker) passphrase(v *yaml.Node) {
	alias := false
	for range 8 {
		if v.Kind != yaml.AliasNode || v.Alias == nil {
			break
		}
		v, alias = v.Alias, true
	}
	if v.Kind == yaml.ScalarNode && scalarText(v) != "" && v.Value != redact.Mask {
		m.set(v, redact.Mask)
		m.recoded = m.recoded || alias
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

// comment returns the comment c with each URL-shaped token redacted.
func (m *masker) comment(c string) string {
	if !strings.Contains(c, "://") {
		return c
	}
	return logging.URLPattern.ReplaceAllStringFunc(c, func(tok string) string {
		r := maskCommentToken(tok)
		if r != tok {
			if m.tokens == nil {
				m.tokens = make(map[string]string)
			}
			m.tokens[tok] = r
		}
		return r
	})
}

// cleanURL reports whether redact.URL reads p and hides nothing new in it.
// Hiding always writes the mask, so a form without the mask's text hid
// nothing, and a form equal to p is redact.URL's own.
func cleanURL(p string) bool {
	r := redact.URL(p)
	return r != redact.Invalid && (r == p || !strings.Contains(r, redact.Mask))
}

// maskValue is a scalar's value as a response shows it: each URL-shaped
// token with something to hide replaced by its redact.URL form, the other
// tokens with the bytes they have, and the bare mask when redact.URL
// cannot parse one of them.
func maskValue(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	whole := false
	out := logging.URLPattern.ReplaceAllStringFunc(v, func(tok string) string {
		r := redact.URL(tok)
		switch {
		case r == redact.Invalid:
			whole = true
		case r != tok && strings.Contains(r, redact.Mask):
			return r
		}
		return tok
	})
	if whole {
		return redact.Mask
	}
	return out
}

// maskCommentToken is a comment's URL-shaped token as a response shows it.
// A token the scan finds clean (cleanRaw) keeps its bytes. Otherwise what
// follows its first '#' is redacted as a comment of its own, first, while
// a URL there can still be read as one; then the token's longest prefix
// that redact.URL parses, cutting only bytes of tokenTail, is redacted and
// the cut tail kept. It is redact.Invalid when no prefix parses, when a
// URL after the '#' does not, or when the scan would still not find the
// result clean: what a comment shows is always what the scan accepts.
func maskCommentToken(tok string) string {
	if cleanRaw(tok) {
		return tok
	}
	if i := strings.IndexByte(tok, '#'); i >= 0 {
		tok = tok[:i+1] + logging.URLPattern.ReplaceAllStringFunc(tok[i+1:], maskCommentToken)
		if strings.Contains(tok, redact.Invalid) {
			return redact.Invalid
		}
	}
	for p := tok; ; p = p[:len(p)-1] {
		if r := redact.URL(p); r != redact.Invalid {
			if r != p && strings.Contains(r, redact.Mask) {
				tok = r + tok[len(p):]
			}
			break
		}
		if strings.IndexByte(tokenTail, p[len(p)-1]) < 0 {
			return redact.Invalid
		}
	}
	if !cleanRaw(tok) {
		return redact.Invalid
	}
	return tok
}

// cleanToken is the scan's rule for a raw token (R-T8): it is clean when
// the token, or a prefix of it made by cutting trailing bytes of
// tokenTail, parses and gets no new mask from redact.URL. Only the tail is
// ever cut, so a secret is inside every prefix.
func cleanToken(tok string) bool {
	for p := tok; ; p = p[:len(p)-1] {
		if cleanURL(p) {
			return true
		}
		if p == "" || strings.IndexByte(tokenTail, p[len(p)-1]) < 0 {
			return false
		}
	}
}

// cleanRaw is cleanToken for a token of the text or of a comment, where a
// '#' may start a comment: the parser takes one after a closing quote, a
// bracket or a comma without white space between, so the URL-shaped token
// after the first '#' is judged by itself as well. ('b://h'#rtsp://u:pw@h
// is, to redact.URL, one URL with a fragment and nothing to hide.)
func cleanRaw(tok string) bool {
	if !cleanToken(tok) {
		return false
	}
	if i := strings.IndexByte(tok, '#'); i >= 0 {
		if at := logging.URLPattern.FindStringIndex(tok[i+1:]); at != nil {
			return cleanRaw(tok[i+1+at[0]:])
		}
	}
	return true
}

// The line breaks of YAML that are not ASCII: NEL, LS and PS. The URL
// pattern reads through them and the parser ends a line at them; a
// comment that starts a line after one is found by cleanRaw's '#' rule.
var wideBreaks = [][]byte{{0xC2, 0x85}, {0xE2, 0x80, 0xA8}, {0xE2, 0x80, 0xA9}}

// rawClean is the postcondition on the bytes (R-T8): no URL-shaped token
// of text has something to hide (cleanRaw). root is text decoded. The
// bytes of a double-quoted scalar are not judged: that scalar's value was,
// in the tree; what follows its closing quote is judged as a token of its
// own. A text in UTF-16 is never clean: its bytes cannot be searched, and
// encoding it again writes UTF-8.
func rawClean(text []byte, root *yaml.Node) bool {
	if bytes.HasPrefix(text, []byte{0xFF, 0xFE}) || bytes.HasPrefix(text, []byte{0xFE, 0xFF}) {
		return false
	}
	if !bytes.Contains(text, []byte("://")) {
		return true
	}
	clean := func(part []byte) bool {
		for _, at := range logging.URLPattern.FindAllIndex(part, -1) {
			if !cleanRaw(string(part[at[0]:at[1]])) {
				return false
			}
		}
		return true
	}
	at := 0
	for _, q := range quotedSpans(text, root) {
		if q.at < at {
			continue
		}
		if !clean(text[at:q.at]) {
			return false
		}
		at = q.end
	}
	return clean(text[at:])
}

// span is the bytes [at, end) of a text.
type span struct{ at, end int }

// quotedSpans returns where the double-quoted scalars under root that
// hold "://" are written in text, from the opening quote to the closing
// one, in order. A span is only returned when its bytes, decoded by
// themselves, are the node's value: then they hold nothing the value does
// not, wherever the node's position was taken from.
func quotedSpans(text []byte, root *yaml.Node) []span {
	starts := lineStarts(text)
	var out []span
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && n.Style&yaml.DoubleQuotedStyle != 0 && strings.Contains(n.Value, "://") {
			if s, ok := quotedSpan(text, offset(text, starts, n.Line, n.Column)); ok {
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

// offset returns where a node's Line and Column, which count characters
// from 1, point in text; -1 when they point nowhere.
func offset(text []byte, starts []int, line, column int) int {
	if line < 1 || line > len(starts) || column < 1 {
		return -1
	}
	i := starts[line-1]
	for range column - 1 {
		if i >= len(text) {
			return -1
		}
		_, w := utf8.DecodeRune(text[i:])
		i += w
	}
	if i >= len(text) {
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
// scalar at its position, each redacted comment token wherever the bytes
// hold it outside those scalars. ok is false when a scalar cannot be
// written in place. The caller decodes the result and compares it with
// the tree: that, not the replacement, is what the result rests on.
func (m *masker) patch(text []byte) (out []byte, ok bool) {
	if m.recoded {
		return nil, false
	}
	patches, ok := scalarPatches(text, m.changes)
	if !ok {
		return nil, false
	}
	if len(m.tokens) > 0 {
		n := len(patches)
		for _, at := range logging.URLPattern.FindAllIndex(text, -1) {
			r, ok := m.tokens[string(text[at[0]:at[1]])]
			if !ok {
				continue
			}
			i := sort.Search(n, func(i int) bool { return patches[i].end > at[0] })
			if i < n && patches[i].at < at[1] {
				continue // inside a scalar that is written anyway
			}
			patches = append(patches, patch{span{at[0], at[1]}, r})
		}
		slices.SortFunc(patches, func(a, b patch) int { return a.at - b.at })
	}
	return applyPatches(text, patches), true
}

// scalarPatches returns the patches that write every change in place, in
// the text's order; ok is false when one cannot be written in place or two
// overlap.
func scalarPatches(text []byte, changes []change) (patches []patch, ok bool) {
	starts := lineStarts(text)
	for _, c := range changes {
		p, ok := scalarPatch(text, offset(text, starts, c.n.Line, c.n.Column), c)
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

// quoteURLs puts every scalar under root that holds "://" in double
// quotes.
func quoteURLs(root *yaml.Node) {
	if root.Kind == yaml.ScalarNode && strings.Contains(root.Value, "://") {
		root.Style = root.Style&yaml.TaggedStyle | yaml.DoubleQuotedStyle
	}
	for _, c := range root.Content {
		quoteURLs(c)
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
	got := tree(out)
	if got == nil {
		return nil, errors.New("config: unmask: the resolved text does not decode")
	}
	if changes, refused := resolve(streamsNode(got), kept); len(changes) > 0 || len(refused) > 0 {
		return nil, errors.New("config: unmask: the resolved text still holds a mask")
	}
	return out, nil
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
	for i, e := range seq.Content {
		if e.Kind != yaml.MappingNode {
			continue
		}
		var old secrets
		if name, ok := entryName(e); ok && stored != nil {
			for _, s := range stored.Content {
				if n, ok := entryName(s); ok && n == name {
					old.add(s)
				}
			}
		}
		var now secrets
		now.add(e)
		refuse := func(field string) {
			refused = append(refused, errors.New(entryRef(i, e)+": "+spec.MaskRefusal(field)))
		}
		keep := func(list string, sent, stored []item) {
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
	if p := child(outputs, passphraseKey); p != nil && p.Kind == yaml.ScalarNode {
		if s.passphrase == nil {
			s.passphrase = p
		}
		if v := scalarText(p); v != "" && !slices.Contains(s.passphrases, v) {
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
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
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
		if u := child(e, "url"); u != nil && u.Kind == yaml.ScalarNode {
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
	return slices.ContainsFunc(logging.URLPattern.FindAllString(v, -1), redact.Masked)
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
