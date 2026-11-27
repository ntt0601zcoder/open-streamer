package rtsp

import (
	"crypto/md5" //nolint:gosec // MD5 is the HTTP Digest hash defined by RFC 2617; no security claim rests on it.
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"sync"
)

// Auth answers RTSP authentication challenges (Basic per RFC 7617, Digest per
// RFC 2069, RFC 2617 and RFC 7616). It never includes the password or an
// Authorization value in an error. It is safe for concurrent use.
type Auth struct {
	user, pass string

	// cnonce returns a fresh client nonce; tests replace it for known answers.
	cnonce func() string

	mu        sync.Mutex
	scheme    string // "", "basic" or "digest"
	realm     string
	nonce     string
	opaque    string
	qop       string // "auth" or "" (RFC 2069)
	algorithm string // as sent by the server, "" when absent
	nc        uint32
}

// NewAuth returns an Auth for the given credentials.
func NewAuth(user, pass string) *Auth {
	return &Auth{user: user, pass: pass, cnonce: randomCnonce}
}

func randomCnonce() string {
	var b [16]byte
	// Since Go 1.24 crypto/rand.Read does not return an error.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type authChallenge struct {
	scheme string // lower case
	params map[string]string
}

// parseChallenges splits a WWW-Authenticate value into its challenges. A token
// followed by '=' is a parameter of the current challenge, any other token
// starts a new one.
func parseChallenges(v string) ([]authChallenge, error) {
	var out []authChallenge
	i := 0
	skip := func() {
		for i < len(v) && (v[i] == ' ' || v[i] == '\t' || v[i] == ',') {
			i++
		}
	}
	for {
		skip()
		if i >= len(v) {
			return out, nil
		}
		start := i
		for i < len(v) && v[i] != ' ' && v[i] != '\t' && v[i] != ',' && v[i] != '=' {
			i++
		}
		tok := v[start:i]
		j := i
		for j < len(v) && (v[j] == ' ' || v[j] == '\t') {
			j++
		}
		if j >= len(v) || v[j] != '=' {
			if tok == "" {
				return nil, errors.New("rtsp: auth: malformed challenge")
			}
			out = append(out, authChallenge{scheme: strings.ToLower(tok), params: map[string]string{}})
			continue
		}
		if tok == "" || len(out) == 0 {
			return nil, errors.New("rtsp: auth: malformed challenge")
		}
		i = j + 1
		for i < len(v) && (v[i] == ' ' || v[i] == '\t') {
			i++
		}
		var val string
		if i < len(v) && v[i] == '"' {
			i++
			var sb strings.Builder
			closed := false
			for i < len(v) {
				c := v[i]
				i++
				if c == '\\' && i < len(v) {
					sb.WriteByte(v[i])
					i++
					continue
				}
				if c == '"' {
					closed = true
					break
				}
				sb.WriteByte(c)
			}
			if !closed {
				return nil, errors.New("rtsp: auth: unterminated quoted string in challenge")
			}
			val = sb.String()
		} else {
			s := i
			for i < len(v) && v[i] != ',' {
				i++
			}
			val = strings.TrimSpace(v[s:i])
		}
		out[len(out)-1].params[strings.ToLower(tok)] = val
	}
}

// digestHash returns the hash constructor for a Digest algorithm name and
// whether it is a "-sess" variant.
func digestHash(alg string) (func() hash.Hash, bool, bool) {
	switch strings.ToUpper(alg) {
	case "", "MD5":
		return md5.New, false, true
	case "MD5-SESS":
		return md5.New, true, true
	case "SHA-256":
		return sha256.New, false, true
	case "SHA-256-SESS":
		return sha256.New, true, true
	}
	return nil, false, false
}

// Challenge records the server's WWW-Authenticate challenge. Digest is
// preferred over Basic; among Digest challenges SHA-256 is preferred over MD5.
// A fresh challenge (including stale=true) replaces the previous nonce and
// restarts the nonce count. On error the previous state is kept.
func (a *Auth) Challenge(h Header) error {
	var all []authChallenge
	for _, v := range h["WWW-Authenticate"] {
		cs, err := parseChallenges(v)
		if err != nil {
			return err
		}
		all = append(all, cs...)
	}
	if len(all) == 0 {
		return errors.New("rtsp: auth: no WWW-Authenticate challenge")
	}

	var best *authChallenge
	bestRank := -1
	var basic *authChallenge
	var digestErr error
	for i := range all {
		c := &all[i]
		switch c.scheme {
		case "basic":
			if basic == nil {
				basic = c
			}
		case "digest":
			rank, err := digestRank(c)
			if err != nil {
				if digestErr == nil {
					digestErr = err
				}
				continue
			}
			if rank > bestRank {
				best, bestRank = c, rank
			}
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case best != nil:
		a.scheme = "digest"
		a.realm = best.params["realm"]
		a.nonce = best.params["nonce"]
		a.opaque = best.params["opaque"]
		a.algorithm = best.params["algorithm"]
		a.qop = "auth"
		if _, hasQop := best.params["qop"]; !hasQop {
			a.qop = ""
		}
		a.nc = 0
	case basic != nil:
		a.scheme = "basic"
		a.realm = basic.params["realm"]
	case digestErr != nil:
		return digestErr
	default:
		return errors.New("rtsp: auth: no supported authentication scheme")
	}
	return nil
}

// digestRank validates a Digest challenge and scores it (higher is better).
func digestRank(c *authChallenge) (int, error) {
	if c.params["nonce"] == "" {
		return 0, errors.New("rtsp: auth: digest challenge without nonce")
	}
	if _, _, ok := digestHash(c.params["algorithm"]); !ok {
		return 0, errors.New("rtsp: auth: unsupported digest algorithm")
	}
	if q, ok := c.params["qop"]; ok {
		found := false
		for _, o := range strings.Split(q, ",") {
			if strings.EqualFold(strings.TrimSpace(o), "auth") {
				found = true
			}
		}
		if !found {
			return 0, errors.New("rtsp: auth: unsupported digest qop")
		}
	}
	if strings.HasPrefix(strings.ToUpper(c.params["algorithm"]), "SHA-256") {
		return 2, nil
	}
	return 1, nil
}

func quoteParam(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// Authorize returns the Authorization header value for a request, or "" when
// no challenge has been accepted yet. uri must be the request URI exactly as
// sent (without credentials). Each call with qop=auth increments the nonce
// count and uses a fresh random cnonce.
func (a *Auth) Authorize(method, uri string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.scheme {
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.user+":"+a.pass))
	case "digest":
		return a.digest(method, uri)
	}
	return ""
}

func (a *Auth) digest(method, uri string) string {
	newHash, sess, _ := digestHash(a.algorithm)
	hashHex := func(s string) string {
		h := newHash()
		_, _ = h.Write([]byte(s))
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	var cnonce, nc string
	if a.qop != "" {
		a.nc++
		nc = fmt.Sprintf("%08x", a.nc)
		cnonce = a.cnonce()
	}
	ha1 := hashHex(a.user + ":" + a.realm + ":" + a.pass)
	if sess {
		if cnonce == "" {
			cnonce = a.cnonce()
		}
		ha1 = hashHex(ha1 + ":" + a.nonce + ":" + cnonce)
	}
	ha2 := hashHex(method + ":" + uri)
	var resp string
	if a.qop != "" {
		resp = hashHex(ha1 + ":" + a.nonce + ":" + nc + ":" + cnonce + ":" + a.qop + ":" + ha2)
	} else {
		resp = hashHex(ha1 + ":" + a.nonce + ":" + ha2)
	}

	var sb strings.Builder
	sb.WriteString("Digest username=" + quoteParam(a.user))
	sb.WriteString(", realm=" + quoteParam(a.realm))
	sb.WriteString(", nonce=" + quoteParam(a.nonce))
	sb.WriteString(", uri=" + quoteParam(uri))
	sb.WriteString(", response=\"" + resp + "\"")
	if a.algorithm != "" {
		sb.WriteString(", algorithm=" + a.algorithm)
	}
	if a.opaque != "" {
		sb.WriteString(", opaque=" + quoteParam(a.opaque))
	}
	if a.qop != "" {
		sb.WriteString(", qop=" + a.qop + ", nc=" + nc + ", cnonce=" + quoteParam(cnonce))
	}
	return sb.String()
}
