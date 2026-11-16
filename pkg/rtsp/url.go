package rtsp

import (
	"errors"
	"net/url"
	"strings"
)

// stripUserinfoString removes "user:pass@" from the authority of raw without
// re-serialising anything else, so queries with slashes survive unchanged.
func stripUserinfoString(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return raw
	}
	return raw[:i+3] + rest[at+1:]
}

// StripUserinfo returns raw without its userinfo, plus the decoded user name
// and password. The error never contains the URL, so no credential leaks.
func StripUserinfo(raw string) (clean, user, pass string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", "", errors.New("rtsp: strip userinfo: invalid URL")
	}
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return stripUserinfoString(raw), user, pass, nil
}

// ResolveControl turns an SDP control attribute into an absolute URL. An empty
// or "*" control is the base itself; an absolute rtsp:// control stands alone;
// anything else is appended to the base with a single slash (the de facto
// camera rule, which also keeps a query in the base intact). Userinfo is
// removed from the result.
func ResolveControl(base, control string) string {
	base = stripUserinfoString(base)
	control = strings.TrimSpace(control)
	switch {
	case control == "" || control == "*":
		return base
	case hasScheme(control):
		return stripUserinfoString(control)
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(control, "/")
}

// hasScheme reports whether s starts with an RFC 3986 scheme followed by "://".
func hasScheme(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case j > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}
