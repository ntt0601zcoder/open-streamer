// Package redact hides credentials and stream keys in URLs and RTMP paths
// before they reach errors, logs, stats or responses: the userinfo, pairs
// with a secret name (SecretName) in the query, a path segment, the
// fragment or a streamid, and an RTMP stream key (R-T2).
package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// tagKey is the key of Tag: random, made once per process, never exported
// and never logged.
var tagKey = sync.OnceValue(func() []byte {
	k := make([]byte, sha256.Size)
	if _, err := rand.Read(k); err != nil {
		// crypto/rand never fails on a supported platform; without a key
		// a tag would be an unkeyed hash of a secret.
		panic("redact: crypto/rand failed: " + err.Error())
	}
	return k
})

// Tag returns the HMAC-SHA-256 of b under a key that is random per
// process: a tag for a value that may hold a secret (a stream document, a
// push URL) and that is shown to those who must not learn it. Within one
// process it is a pure function of b, so equal values have equal tags;
// without the key a guess of the secret cannot be tested against it. The
// key is new at every start, so tags do not survive a restart.
func Tag(b []byte) []byte {
	m := hmac.New(sha256.New, tagKey())
	m.Write(b)
	return m.Sum(nil)
}

// Mask replaces a hidden value in what URL returns, and is what a write
// sends to keep a stored secret (R-T3).
const Mask = "xxxxx"

// Invalid is what URL returns for a string that holds "://" and does not
// parse. It shows nothing of the string.
const Invalid = "<invalid url>"

// secretParts are the words whose presence anywhere in a name makes it a
// secret name.
var secretParts = []string{"pass", "pwd", "psw", "secret", "token", "signature", "credential", "hmac", "jwt"}

// secretNames are the whole names that are secret names.
var secretNames = []string{
	"key", "pw", "sig", "auth", "authbasic", "loginpas", "hash", "md5", "hdnts", "hdnea", "wmsauthsign",
}

// SecretName reports whether name names a credential (R-T2), in any case:
// it contains pass, pwd, psw, secret, token, signature, credential, hmac
// or jwt; or is key, pw, sig, auth, authbasic, loginpas, hash, md5, hdnts,
// hdnea or wmsauthsign; or ends in key. Percent-decoding is the caller's.
func SecretName(name string) bool {
	n := strings.ToLower(name)
	for _, p := range secretParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	return slices.Contains(secretNames, n) || strings.HasSuffix(n, "key")
}

// URL returns rawURL with the credentials of R-T2 hidden, on every
// scheme:
//
//  1. the userinfo's password, or a lone user;
//  2. in a query piece (the raw query cut at every '&'), the first '='
//     whose name is secret: everything after it to the end of the piece,
//     also when empty;
//  3. in a path segment or the fragment, after every '=' whose name is
//     secret: the value, which runs to the separator before the next pair
//     start or to the end. A pair starts at the start, or after one of
//     '&', ';', ',' or '_' (in the fragment also '/' and '?'), with a name
//     that decodes to [A-Za-z][A-Za-z0-9-]* followed by '=';
//  4. in the value of a streamid query pair, which runs to the end of its
//     piece and is walked once percent-decoded: after an optional "#!::",
//     the values of rule 3, with '&', ',', '_', '/' and '?' as separators
//     and, after "#!::", s as a secret name too; and, for "read:" or
//     "publish:" (MediaMTX's read|publish:<path>:<user>:<pass>),
//     everything after the third colon, also when it is empty;
//  5. for a scheme of the RTMP family (rtmpFamily: an RTMPScheme, or one of
//     them with "+insecure", such as rtmp+insecure:// which validation
//     refuses but an error may still quote) its stream name (the last path
//     segment, the stream key) cut to its first 2 characters (RTMPKey);
//     rule 3 runs before it and again on the path it writes, which no
//     longer holds the escapes it was given.
//
// A name is judged apart from the pair grammar: it is the text before the
// '=' back to the previous '=', '&', ';', ',' or '/', or to the start, and
// it is secret when, raw or percent-decoded, it is a SecretName or ends in
// one of SecretName's whole names after a byte that is no letter or digit. Rules 2
// and 3 walk the escaped text, so an escaped separator separates nothing;
// a name keeps its bytes. The grammar only decides where a value ends. A
// user name is never hidden. Everything else keeps its bytes, but the
// scheme comes back in lower case, as url.Parse leaves it. A string
// without "://" is returned as is; one that does not parse is Invalid.
func URL(rawURL string) string {
	out, _ := redactURL(rawURL)
	return out
}

// redactURL is URL, and reports whether it wrote the mask anywhere.
func redactURL(rawURL string) (string, bool) {
	if !strings.Contains(rawURL, "://") {
		return rawURL, false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Invalid, false
	}
	hid := false
	if u.User != nil {
		pw, hasPW := u.User.Password()
		switch {
		case hasPW:
			u.User, hid = url.UserPassword(u.User.Username(), Mask), true
		case u.User.Username() != "" || pw != "":
			u.User, hid = url.User(Mask), true
		}
	}
	if q, h := hide(u.RawQuery, querySpans(u.RawQuery)); h {
		u.RawQuery, hid = q, true
	}
	h, ok := hidePath(u)
	if !ok {
		return Invalid, false
	}
	hid = hid || h
	if f := u.EscapedFragment(); f != "" {
		if f, h := hide(f, spans(f, fragSeps, false, secretText)); h {
			frag, err := url.PathUnescape(f)
			if err != nil {
				return Invalid, false
			}
			u.Fragment, u.RawFragment, hid = frag, f, true
		}
	}
	if rtmpFamily(u.Scheme) {
		changed := false
		if p := strings.TrimRight(u.Path, "/"); p != "" {
			u.Path, u.RawPath, changed = streamKey(p), "", true
		}
		if u.Opaque != "" {
			u.Opaque, changed = streamKey(u.Opaque), true
		}
		// RTMPKey wrote the decoded path: an escaped '=', '&' or '/' is
		// literal now, so rule 3 runs again on what it wrote.
		if changed {
			if _, ok := hidePath(u); !ok {
				return Invalid, false
			}
		}
		hid = hid || changed
	}
	out := u.String()
	if hid && schemeURL(rawURL, u) && !strings.Contains(out, "://") {
		// String drops an empty authority before an empty path
		// ("srt://?token=t" is "srt:?token=…"); put it back, so the
		// result is still a URL that Masked and URL read.
		out = u.Scheme + "://" + strings.TrimPrefix(out, u.Scheme+":")
	}
	return out, hid
}

// hidePath applies rule 3 to the path of u (its escaped path, or its
// opaque part), writes it back and reports whether it hid something. ok
// is false only when the walked path does not unescape, which cannot
// happen: the walk cuts only at literal bytes that are no hex digits, so
// it never splits an escape; should it ever, the caller shows nothing.
func hidePath(u *url.URL) (hid, ok bool) {
	rtmp := rtmpFamily(u.Scheme)
	if u.Opaque != "" {
		o, h := hide(u.Opaque, pathSpans(u.Opaque, rtmp))
		u.Opaque = o
		return h, true
	}
	p := u.EscapedPath()
	np, h := hide(p, pathSpans(p, rtmp))
	if !h {
		return false, true
	}
	path, err := url.PathUnescape(np)
	if err != nil {
		return false, false
	}
	u.Path, u.RawPath = path, np
	return true, true
}

// streamKey is RTMPKey, but a stream name that is the mask, which RTMPKey
// writes for a name of up to 2 characters, stays as it is, so that URL of
// its own result changes nothing.
func streamKey(p string) string {
	if p[strings.LastIndexByte(p, '/')+1:] == Mask {
		return p
	}
	return RTMPKey(p)
}

// schemeURL reports whether rawURL, parsed as u, starts with its scheme and
// "://": a URL with an authority, even an empty one.
func schemeURL(rawURL string, u *url.URL) bool {
	n := len(u.Scheme)
	return n > 0 && u.Opaque == "" && strings.HasPrefix(rawURL[n:], "://")
}

// span is a range [a, b) of a string whose text R-T2 hides.
type span struct{ a, b int }

// stops are the bytes a name runs back to from its '='.
const stops = "=&;,/"

// The bytes after which a pair may start, which end the value before it.
const (
	pathSeps   = "&;,_"
	fragSeps   = "&;,_/?"
	streamSeps = "&,_/?" // ';' ends no value: a streamid is in the query (rule 2)
)

// hide returns s with every span replaced by the mask, and whether there
// was one.
func hide(s string, ss []span) (string, bool) {
	if len(ss) == 0 {
		return s, false
	}
	var b strings.Builder
	last := 0
	for _, sp := range ss {
		b.WriteString(s[last:sp.a])
		b.WriteString(Mask)
		last = sp.b
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// masked reports whether any span of s holds exactly the mask.
func masked(s string, ss []span) bool {
	for _, sp := range ss {
		if s[sp.a:sp.b] == Mask {
			return true
		}
	}
	return false
}

// pieceSpans cuts s at every sep and returns the spans f finds in the
// pieces, as offsets into s.
func pieceSpans(s string, sep byte, f func(string) []span) []span {
	var out []span
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != sep {
			continue
		}
		for _, sp := range f(s[start:i]) {
			out = append(out, span{start + sp.a, start + sp.b})
		}
		start = i + 1
	}
	return out
}

// pathSpans returns the spans of rule 3 in each '/'-separated segment of
// the escaped path p. For the RTMP family (decoded), whose rule 5 writes
// the path decoded, each segment is also walked once decoded and the two
// walks' spans are joined, so that an escaped '=', '&' or '/' that rule 5
// makes literal cannot cut a value short.
func pathSpans(p string, decoded bool) []span {
	return pieceSpans(p, '/', func(seg string) []span {
		ss := spans(seg, pathSeps, false, secretText)
		if decoded {
			ss = join(ss, decodedSpans(seg, func(d string) []span { return spans(d, pathSeps, true, secretText) }))
		}
		return ss
	})
}

// decodedSpans decodes v once, gives it to f and returns f's spans as
// offsets into v.
func decodedSpans(v string, f func(d string) []span) []span {
	var d []byte
	at := make([]int, 0, len(v)+1)
	for i := 0; i < len(v); {
		c, w := unit(v, i)
		d, at = append(d, c), append(at, i)
		i += w
	}
	at = append(at, len(v))
	ss := f(string(d))
	for k, sp := range ss {
		ss[k] = span{at[sp.a], at[sp.b]}
	}
	return ss
}

// join returns the union of two sorted lists of spans, sorted, with spans
// that overlap or touch made one.
func join(x, y []span) []span {
	all := append(slices.Clone(x), y...)
	slices.SortFunc(all, func(a, b span) int { return a.a - b.a })
	var out []span
	for _, sp := range all {
		if n := len(out); n > 0 && sp.a <= out[n-1].b {
			out[n-1].b = max(out[n-1].b, sp.b)
			continue
		}
		out = append(out, sp)
	}
	return out
}

// querySpans returns the spans of rules 2 and 4 in each '&'-separated piece
// of the raw query q.
func querySpans(q string) []span {
	return pieceSpans(q, '&', func(piece string) []span {
		for e := 0; e < len(piece); e++ {
			if piece[e] != '=' {
				continue
			}
			n := nameBefore(piece, e)
			switch {
			case secretText(n):
				return []span{{e + 1, len(piece)}}
			case strings.EqualFold(decode(n), "streamid"):
				var out []span
				for _, sp := range streamIDSpans(piece[e+1:]) {
					out = append(out, span{e + 1 + sp.a, e + 1 + sp.b})
				}
				return out
			}
		}
		return nil
	})
}

// streamIDSpans returns the spans of rule 4 in the raw value v of a
// streamid pair. The value is walked once percent-decoded; the spans are
// offsets into v.
func streamIDSpans(v string) []span {
	return decodedSpans(v, streamIDDecoded)
}

// streamIDDecoded returns the spans of rule 4 in d, a streamid's value
// once decoded.
func streamIDDecoded(d string) []span {
	n, secret := 0, secretText
	if strings.HasPrefix(d, "#!::") {
		n = 4
		secret = func(s string) bool {
			return secretText(s) || endsIn(decode(s), []string{"s"}, func(n string) bool { return strings.EqualFold(n, "s") })
		}
	}
	body := d[n:]
	ss := spans(body, streamSeps, true, secret)
	if hasPrefixFold(body, "read:") || hasPrefixFold(body, "publish:") {
		if i := afterColons(body, 3); i >= 0 {
			// The tail and any value that reaches it become one span, so
			// that URL of the result changes nothing.
			k := 0
			for k < len(ss) && ss[k].b < i {
				k++
			}
			if k < len(ss) {
				i = min(i, ss[k].a)
			}
			ss = append(ss[:k], span{i, len(body)})
		}
	}
	for k, sp := range ss {
		ss[k] = span{n + sp.a, n + sp.b}
	}
	return ss
}

// spans returns the values of s that rule 3 hides: for each '=' outside a
// value already found whose name (nameBefore) secret accepts, the text
// after it to the separator before the next pair start, or to the end of
// s. A pair starts at 0 or after a byte of seps, where the text up to the
// next '=' holds no byte of seps and, percent-decoded unless s is decoded
// already, matches [A-Za-z][A-Za-z0-9-]*.
func spans(s, seps string, decoded bool, secret func(string) bool) []span {
	var starts []int
	for i := 0; i < len(s); i++ {
		if i > 0 && strings.IndexByte(seps, s[i-1]) < 0 {
			continue
		}
		j := i
		for j < len(s) && s[j] != '=' && strings.IndexByte(seps, s[j]) < 0 {
			j++
		}
		if j == len(s) || s[j] != '=' {
			continue
		}
		name := s[i:j]
		if !decoded {
			name = decode(name)
		}
		if pairName(name) {
			starts = append(starts, i)
		}
	}
	var out []span
	for e := 0; e < len(s); e++ {
		if s[e] != '=' || !secret(nameBefore(s, e)) {
			continue
		}
		end := len(s)
		for _, p := range starts {
			if p-1 > e {
				end = p - 1
				break
			}
		}
		out = append(out, span{e + 1, end})
		e = end
	}
	return out
}

// nameBefore returns the name of the '=' at eq in s: the text before it
// back to the previous byte of stops, or to the start.
func nameBefore(s string, eq int) string {
	return s[strings.LastIndexAny(s[:eq], stops)+1 : eq]
}

// secretText reports whether n, raw or percent-decoded, is a SecretName,
// or ends in one of the whole secret names after a byte that is no letter
// or digit (admin_pw, cam?hdnts): a name that the pair grammar would
// start there.
func secretText(n string) bool {
	return endsIn(n, secretNames, SecretName) || endsIn(decode(n), secretNames, SecretName)
}

// endsIn reports whether whole(n) holds, or n has a suffix in names (in any
// case) that starts after a byte that is no letter or digit.
func endsIn(n string, names []string, whole func(string) bool) bool {
	if whole(n) {
		return true
	}
	for i := 0; i < len(n); i++ {
		if c := n[i]; !isLetter(c) && (c < '0' || c > '9') && slices.Contains(names, strings.ToLower(n[i+1:])) {
			return true
		}
	}
	return false
}

// pairName reports whether n matches [A-Za-z][A-Za-z0-9-]*.
func pairName(n string) bool {
	if n == "" || !isLetter(n[0]) {
		return false
	}
	for i := 1; i < len(n); i++ {
		if c := n[i]; !isLetter(c) && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

// decode returns s with every valid %XX escape decoded; anything else is
// kept as it is.
func decode(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c, w := unit(s, i)
		b = append(b, c)
		i += w
	}
	return string(b)
}

// unit returns the byte that s[i:] starts with, a %XX escape decoded, and
// how many bytes of s it takes.
func unit(s string, i int) (byte, int) {
	if s[i] == '%' && i+2 < len(s) {
		if h, l := unhex(s[i+1]), unhex(s[i+2]); h >= 0 && l >= 0 {
			return byte(h<<4 | l), 3
		}
	}
	return s[i], 1
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c|0x20 && c|0x20 <= 'f':
		return int(c|0x20-'a') + 10
	}
	return -1
}

// hasPrefixFold reports whether s starts with p, ignoring case.
func hasPrefixFold(s, p string) bool {
	return len(s) >= len(p) && strings.EqualFold(s[:len(p)], p)
}

// afterColons returns the index in s just after its n-th colon, or -1 when
// s has fewer.
func afterColons(s string, n int) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			if n--; n == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// RTMPScheme reports whether scheme (any case) is rtmp, rtmps or
// rtmps+insecure: the schemes whose last path segment is a stream key.
func RTMPScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "rtmp", "rtmps", "rtmps+insecure":
		return true
	}
	return false
}

// rtmpFamily reports whether scheme (any case) is an RTMPScheme, with or
// without "+insecure": every scheme whose stream key URL and Masked hide,
// including rtmp+insecure, which RTMPScheme does not accept.
func rtmpFamily(scheme string) bool {
	return RTMPScheme(strings.TrimSuffix(strings.ToLower(scheme), "+insecure"))
}

// RTMPKey masks the stream name of an RTMP path, its last segment, keeping
// its first 2 characters; a name of up to 2 characters is masked whole.
func RTMPKey(path string) string {
	i := strings.LastIndexByte(path, '/')
	name := []rune(path[i+1:])
	if len(name) > 2 {
		name = name[:2]
	} else {
		name = nil
	}
	return path[:i+1] + string(name) + Mask
}

// Masked reports whether rawURL carries the mask in any place URL writes
// it: as the password, as a lone user, as a value URL hides in the query,
// a path segment, the fragment or a streamid, or, for a scheme of the RTMP
// family (rtmpFamily), at the end of the stream name. Such a URL came back
// from URL, so it no longer holds the secret and must not be stored.
func Masked(rawURL string) bool {
	if !strings.Contains(rawURL, "://") {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.User != nil {
		if pw, hasPW := u.User.Password(); hasPW {
			if pw == Mask {
				return true
			}
		} else if u.User.Username() == Mask {
			return true
		}
	}
	p := u.Opaque
	if p == "" {
		p = u.EscapedPath()
	}
	f := u.EscapedFragment()
	if masked(u.RawQuery, querySpans(u.RawQuery)) || masked(p, pathSpans(p, rtmpFamily(u.Scheme))) ||
		masked(f, spans(f, fragSeps, false, secretText)) {
		return true
	}
	if rtmpFamily(u.Scheme) {
		p := strings.TrimRight(u.Path, "/")
		if u.Opaque != "" {
			p = u.Opaque
		}
		return p != "" && strings.HasSuffix(p[strings.LastIndexByte(p, '/')+1:], Mask)
	}
	return false
}
