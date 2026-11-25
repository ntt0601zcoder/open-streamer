// Package redact hides credentials and stream keys in URLs and RTMP paths
// before they reach errors, logs or stats.
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

// mask replaces a hidden value.
const mask = "xxxxx"

// secretKeys are the query parameters whose values never appear in errors
// or logs.
var secretKeys = []string{"passphrase", "password", "token", "secret", "key"}

// URL returns rawURL with its password and secret query values hidden, and
// for rtmp:// its stream name (the last path segment, the stream key) cut
// to its first 2 characters (RTMPKey). A string without "://" is returned
// as is; one that does not parse is "<invalid url>".
func URL(rawURL string) string {
	if !strings.Contains(rawURL, "://") {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid url>"
	}
	if u.User != nil {
		pw, hasPW := u.User.Password()
		switch {
		case hasPW:
			u.User = url.UserPassword(u.User.Username(), mask)
		case u.User.Username() != "" || pw != "":
			u.User = url.User(mask)
		}
	}
	if u.RawQuery != "" {
		pairs := strings.Split(u.RawQuery, "&")
		for i, pair := range pairs {
			k, _, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			if name, err := url.QueryUnescape(k); err == nil {
				k = name
			}
			if slices.Contains(secretKeys, strings.ToLower(k)) {
				pairs[i] = pair[:strings.IndexByte(pair, '=')+1] + mask
			}
		}
		u.RawQuery = strings.Join(pairs, "&")
	}
	if strings.EqualFold(u.Scheme, "rtmp") {
		u.Scheme = "rtmp"
		if p := strings.TrimRight(u.Path, "/"); p != "" {
			u.Path, u.RawPath = RTMPKey(p), ""
		}
		if u.Opaque != "" {
			u.Opaque = RTMPKey(u.Opaque)
		}
	}
	return u.String()
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
	return path[:i+1] + string(name) + mask
}

// Masked reports whether rawURL carries the mask where URL writes it: as
// the password, as a lone user, as the value of a secret query parameter,
// or, for rtmp://, at the end of the stream name. Such a URL came back
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
			if pw == mask {
				return true
			}
		} else if u.User.Username() == mask {
			return true
		}
	}
	for pair := range strings.SplitSeq(u.RawQuery, "&") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || v != mask {
			continue
		}
		if name, err := url.QueryUnescape(k); err == nil {
			k = name
		}
		if slices.Contains(secretKeys, strings.ToLower(k)) {
			return true
		}
	}
	if strings.EqualFold(u.Scheme, "rtmp") {
		p := strings.TrimRight(u.Path, "/")
		if u.Opaque != "" {
			p = u.Opaque
		}
		return p != "" && strings.HasSuffix(p[strings.LastIndexByte(p, '/')+1:], mask)
	}
	return false
}
