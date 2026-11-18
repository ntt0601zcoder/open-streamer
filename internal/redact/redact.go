// Package redact hides credentials and stream keys in URLs and RTMP paths
// before they reach errors, logs or stats.
package redact

import (
	"net/url"
	"slices"
	"strings"
)

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
