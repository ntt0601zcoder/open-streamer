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

// SecretName reports whether name, percent-decoded and lower-cased by the
// caller or not, names a credential (R-T2): it contains pass, pwd, psw,
// secret, token, signature, credential, hmac or jwt; or is key, pw, sig,
// auth, authbasic, loginpas, hash, md5, hdnts, hdnea or wmsauthsign; or
// ends in key. Percent-decoding is the caller's.
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
//  2. the value of a query pair whose name is a SecretName, also when
//     empty: the raw query is cut at every '&', and inside one piece ';'
//     separates pairs by rule 3's "next pair start" rule, with any
//     non-empty run without '=', '&' or ';' as a name;
//  3. the value of a pair with a secret name inside a path segment or the
//     fragment: a pair starts at the start of the segment or after one of
//     '&', ';', ',' or '_', with a name matching [A-Za-z][A-Za-z0-9-]*
//     followed by '='; its value runs to the separator before the next
//     pair's start, or to the end of the segment;
//  4. in the value of a streamid query pair, after an optional "#!::", its
//     pairs by rule 3, and for "read:" or "publish:" (MediaMTX's
//     read|publish:<path>:<user>:<pass>) everything after the third colon,
//     also when it is empty;
//  5. for a scheme of the RTMP family (rtmpFamily: an RTMPScheme, or one of
//     them with "+insecure", such as rtmp+insecure:// which validation
//     refuses but an error may still quote) its stream name (the last path
//     segment, the stream key) cut to its first 2 characters (RTMPKey),
//     after rule 3.
//
// Rules 2 to 4 walk the escaped text: only a pair's name is
// percent-decoded, to be judged, and it keeps its bytes; an escaped
// separator separates nothing. A user name is never hidden. Everything
// else keeps its bytes, but the scheme comes back in lower case, as
// url.Parse leaves it. A string without "://" is returned as is; one that
// does not parse is Invalid.
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
	if u.RawQuery != "" {
		if q, h := query(u.RawQuery, hideQueryPair); h {
			u.RawQuery, hid = q, true
		}
	}
	if u.Opaque != "" {
		if o, h := segments(u.Opaque, hidePathPair); h {
			u.Opaque, hid = o, true
		}
	} else if p, h := segments(u.EscapedPath(), hidePathPair); h {
		// The walk cuts only at literal bytes that are no hex digits, so
		// it never splits an escape; should it ever, show nothing.
		path, err := url.PathUnescape(p)
		if err != nil {
			return Invalid, false
		}
		u.Path, u.RawPath, hid = path, p, true
	}
	if f := u.EscapedFragment(); f != "" {
		if f, h := walk(f, pathSeps, pathName, hidePathPair); h {
			frag, err := url.PathUnescape(f)
			if err != nil {
				return Invalid, false
			}
			u.Fragment, u.RawFragment, hid = frag, f, true
		}
	}
	if rtmpFamily(u.Scheme) {
		if p := strings.TrimRight(u.Path, "/"); p != "" {
			u.Path, u.RawPath, hid = streamKey(p), "", true
		}
		if u.Opaque != "" {
			u.Opaque, hid = streamKey(u.Opaque), true
		}
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

// pathSeps are the bytes after which a pair of rule 3 may start.
const pathSeps = "&;,_"

// pairFunc gets a pair's raw name and value and returns its new value and
// whether the pair holds, or now holds, the mask.
type pairFunc func(name, value string) (string, bool)

// walk returns s, escaped text, with the value of each of its pairs
// replaced by what f returns for it, and whether f reported any. A pair
// starts at the start of s or after a byte of seps, where the text up to
// the next '=' holds no byte of seps and isName accepts it; its value runs
// to the separator before the next pair's start, or to the end of s. Text
// before the first pair is kept.
func walk(s, seps string, isName func(string) bool, f pairFunc) (string, bool) {
	type span struct{ name, eq int }
	var starts []span
	for i := 0; i < len(s); i++ {
		if i > 0 && strings.IndexByte(seps, s[i-1]) < 0 {
			continue
		}
		j := i
		for j < len(s) && s[j] != '=' && strings.IndexByte(seps, s[j]) < 0 {
			j++
		}
		if j < len(s) && s[j] == '=' && j > i && isName(s[i:j]) {
			starts = append(starts, span{i, j})
		}
	}
	if len(starts) == 0 {
		return s, false
	}
	var b strings.Builder
	b.WriteString(s[:starts[0].name])
	found := false
	for k, p := range starts {
		end := len(s)
		if k+1 < len(starts) {
			end = starts[k+1].name - 1
		}
		v, ok := f(s[p.name:p.eq], s[p.eq+1:end])
		found = found || ok
		b.WriteString(s[p.name : p.eq+1])
		b.WriteString(v)
		if end < len(s) {
			b.WriteByte(s[end])
		}
	}
	return b.String(), found
}

// segments walks each '/'-separated segment of the escaped path p by
// rule 3.
func segments(p string, f pairFunc) (string, bool) {
	parts := strings.Split(p, "/")
	found := false
	for i, s := range parts {
		var ok bool
		parts[i], ok = walk(s, pathSeps, pathName, f)
		found = found || ok
	}
	return strings.Join(parts, "/"), found
}

// query walks the raw query q by rule 2: a hard split at '&', then pairs
// separated by ';' inside each piece.
func query(q string, f pairFunc) (string, bool) {
	pieces := strings.Split(q, "&")
	found := false
	for i, s := range pieces {
		var ok bool
		pieces[i], ok = walk(s, ";", queryName, f)
		found = found || ok
	}
	return strings.Join(pieces, "&"), found
}

// pathName reports whether the raw name n, percent-decoded, matches
// [A-Za-z][A-Za-z0-9-]*.
func pathName(n string) bool {
	d, err := url.PathUnescape(n)
	if err != nil || d == "" || !isLetter(d[0]) {
		return false
	}
	for i := 1; i < len(d); i++ {
		if c := d[i]; !isLetter(c) && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

// queryName accepts any name walk offers in a query: a non-empty run
// without '=', '&' or ';'.
func queryName(string) bool { return true }

// pathSecret reports whether the raw name of a pair of rule 3 is a
// SecretName.
func pathSecret(name string) bool {
	d, _ := url.PathUnescape(name) // pathName accepted it, so it decodes.
	return SecretName(d)
}

// querySecret reports whether the raw name of a query pair is a SecretName,
// judged on its decoded form when it decodes, as at HEAD.
func querySecret(name string) bool {
	if d, err := url.QueryUnescape(name); err == nil {
		name = d
	}
	return SecretName(name)
}

// isStreamID reports whether the raw name of a query pair is streamid.
func isStreamID(name string) bool {
	if d, err := url.QueryUnescape(name); err == nil {
		name = d
	}
	return strings.EqualFold(name, "streamid")
}

// hidePathPair hides the value of a pair of rule 3 with a secret name.
func hidePathPair(name, value string) (string, bool) {
	if pathSecret(name) {
		return Mask, true
	}
	return value, false
}

// hideQueryPair hides the value of a query pair with a secret name, and
// the credentials inside a streamid's value (rule 4).
func hideQueryPair(name, value string) (string, bool) {
	switch {
	case querySecret(name):
		return Mask, true
	case isStreamID(name):
		return streamID(value, hidePathPair, true)
	}
	return value, false
}

// streamID applies rule 4 to the raw value v of a streamid pair: f to the
// pairs after an optional "#!::", and, for MediaMTX's form, the mask after
// the third colon when hide is set. It reports what f reported, or, for
// MediaMTX's form, also whether the text after the third colon is the
// mask. The prefix and the colons are matched on the decoded text, so an
// escaped one counts.
func streamID(v string, f pairFunc, hide bool) (string, bool) {
	n := prefixLen(v, "#!::", false)
	body, found := walk(v[n:], pathSeps, pathName, f)
	if prefixLen(body, "read:", true) > 0 || prefixLen(body, "publish:", true) > 0 {
		if i := afterColons(body, 3); i >= 0 {
			switch {
			case hide:
				body, found = body[:i]+Mask, true
			case body[i:] == Mask:
				found = true
			}
		}
	}
	return v[:n] + body, found
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

// prefixLen returns how many bytes of s decode to p (ignoring the case of
// letters when fold is set), or 0 when s does not start with p.
func prefixLen(s, p string, fold bool) int {
	i := 0
	for k := 0; k < len(p); k++ {
		if i >= len(s) {
			return 0
		}
		c, w := unit(s, i)
		if c != p[k] && (!fold || !isLetter(c) || c|0x20 != p[k]|0x20) {
			return 0
		}
		i += w
	}
	return i
}

// afterColons returns the index in s just after its n-th colon, decoded,
// or -1 when s has fewer.
func afterColons(s string, n int) int {
	for i := 0; i < len(s); {
		c, w := unit(s, i)
		i += w
		if c == ':' {
			if n--; n == 0 {
				return i
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
// it: as the password, as a lone user, as the value of a pair with a
// secret name in the query, a path segment or the fragment, in a
// streamid's value, or, for a scheme of the RTMP family (rtmpFamily), at
// the end of the stream name. Such a URL came back from URL, so it no
// longer holds the secret and must not be stored.
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
	if _, ok := query(u.RawQuery, maskedQueryPair); ok {
		return true
	}
	p := u.Opaque
	if p == "" {
		p = u.EscapedPath()
	}
	if _, ok := segments(p, maskedPathPair); ok {
		return true
	}
	if _, ok := walk(u.EscapedFragment(), pathSeps, pathName, maskedPathPair); ok {
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

// maskedPathPair reports a pair of rule 3 whose name is secret and whose
// value is the mask.
func maskedPathPair(name, value string) (string, bool) {
	return value, value == Mask && pathSecret(name)
}

// maskedQueryPair reports a query pair whose name is secret and whose value
// is the mask, or a streamid whose value holds the mask where rule 4
// writes it.
func maskedQueryPair(name, value string) (string, bool) {
	switch {
	case querySecret(name):
		return value, value == Mask
	case isStreamID(name):
		return streamID(value, maskedPathPair, false)
	}
	return value, false
}
