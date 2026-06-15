// Package mediaauth authorizes playback (media-plane) requests across every
// delivery protocol (HLS, DASH, HTTP-MPEGTS, RTMP, SRT, RTSP). It is the
// counterpart to the control-plane (admin) auth in internal/api: that gates
// who can CONFIGURE the server; this gates who can WATCH a stream.
//
// One Authorizer is shared by all protocol handlers. Each handler builds an
// AuthRequest from its connection and calls Authorize before allocating any
// streaming state. Evaluation is a Flussonic-style chain — deny wins, allow
// lists restrict, then a per-stream token-policy gate:
//
//  1. ClientIP / Country / User-Agent on a Deny* list      → DENY
//  2. any non-empty Allow* list the request value misses    → DENY
//  3. AllowedDomains set and the Referer host isn't covered → DENY
//  4. effective policy == "token" and the signed token is missing/invalid → DENY
//  5. otherwise                                             → ALLOW
//
// Disabled (Enabled=false, the default) short-circuits to ALLOW so existing
// deployments are unaffected until an operator turns it on. Config is swapped
// atomically for hot-reload.
package mediaauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Policy values for a stream's effective playback policy.
const (
	PolicyPublic = "public" // no token required
	PolicyToken  = "token"  // a valid signed token is required

	// clockSkew tolerates small clock differences when checking token expiry.
	clockSkew = 60 * time.Second
)

// AuthRequest is the per-request context a protocol handler hands to Authorize.
type AuthRequest struct {
	Code      domain.StreamCode
	ClientIP  net.IP // may be nil if unparseable
	Token     string // playback token (?token= / streamid / Authorization)
	UserAgent string
	Referer   string
}

// Decision is the outcome. Allow=false carries a short, non-leaky Reason for
// logs (never returned to the client verbatim).
type Decision struct {
	Allow  bool
	Reason string
}

func allow() Decision             { return Decision{Allow: true} }
func deny(reason string) Decision { return Decision{Allow: false, Reason: reason} }

// GeoResolver maps a client IP to an ISO 3166-1 alpha-2 country code, or "" if
// unknown / unavailable. Backed by the sessions GeoIP database.
type GeoResolver func(net.IP) string

// PolicyResolver returns a stream's own playback policy ("public"/"token") or
// "" to inherit the global default. Backed by the publisher's in-memory stream
// table so it costs an O(1) map lookup, not a store read per segment.
type PolicyResolver func(domain.StreamCode) string

// Authorizer evaluates the chain. Safe for concurrent use.
type Authorizer struct {
	state  atomic.Pointer[state]
	geo    GeoResolver
	policy PolicyResolver
}

// state is an immutable, pre-parsed snapshot of MediaAuthConfig.
type state struct {
	enabled       bool
	defaultPolicy string
	secret        []byte

	allowNets, denyNets []*net.IPNet
	allowIPs, denyIPs   map[string]struct{} // exact IPs (canonical string)
	allowCountries      map[string]struct{}
	denyCountries       map[string]struct{}
	allowUAs, denyUAs   []string // lower-cased substrings
	allowedDomains      []string // lower-cased hosts
	hasIPAllow          bool
	hasCountryAllow     bool
	hasUAAllow          bool
	hasDomainAllow      bool
}

// New builds an Authorizer. geo and policy may be nil (country rules then never
// match an allow-list, and every stream uses the global default policy).
func New(cfg config.MediaAuthConfig, geo GeoResolver, policy PolicyResolver) *Authorizer {
	a := &Authorizer{geo: geo, policy: policy}
	a.SetConfig(cfg)
	return a
}

// SetConfig swaps in a freshly-parsed snapshot (config hot-reload).
func (a *Authorizer) SetConfig(cfg config.MediaAuthConfig) {
	a.state.Store(buildState(cfg))
}

func buildState(cfg config.MediaAuthConfig) *state {
	st := &state{
		enabled:        cfg.Enabled,
		defaultPolicy:  strings.ToLower(strings.TrimSpace(cfg.DefaultPolicy)),
		secret:         []byte(cfg.TokenSecret),
		allowIPs:       map[string]struct{}{},
		denyIPs:        map[string]struct{}{},
		allowCountries: upperSet(cfg.AllowCountries),
		denyCountries:  upperSet(cfg.DenyCountries),
		allowUAs:       lowerList(cfg.AllowUserAgents),
		denyUAs:        lowerList(cfg.DenyUserAgents),
		allowedDomains: lowerList(cfg.AllowedDomains),
	}
	st.allowNets, st.allowIPs = parseIPRules(cfg.AllowIPs)
	st.denyNets, st.denyIPs = parseIPRules(cfg.DenyIPs)
	st.hasIPAllow = len(st.allowNets) > 0 || len(st.allowIPs) > 0
	st.hasCountryAllow = len(st.allowCountries) > 0
	st.hasUAAllow = len(st.allowUAs) > 0
	st.hasDomainAllow = len(st.allowedDomains) > 0
	return st
}

// Authorize runs the chain and returns the decision.
func (a *Authorizer) Authorize(req AuthRequest) Decision {
	st := a.state.Load()
	if st == nil || !st.enabled {
		return allow()
	}

	// 1. Deny lists (hard block, evaluated first).
	if ipInRules(req.ClientIP, st.denyNets, st.denyIPs) {
		return deny("ip on deny list")
	}
	// Resolve country only when a rule needs it.
	country := ""
	if (len(st.denyCountries) > 0 || st.hasCountryAllow) && a.geo != nil {
		country = strings.ToUpper(a.geo(req.ClientIP))
	}
	if country != "" {
		if _, bad := st.denyCountries[country]; bad {
			return deny("country on deny list")
		}
	}
	if uaMatches(req.UserAgent, st.denyUAs) {
		return deny("user-agent on deny list")
	}

	// 2. Allow gates: each configured list must be satisfied.
	if st.hasIPAllow && !ipInRules(req.ClientIP, st.allowNets, st.allowIPs) {
		return deny("ip not on allow list")
	}
	if st.hasCountryAllow {
		if _, ok := st.allowCountries[country]; !ok || country == "" {
			return deny("country not on allow list")
		}
	}
	if st.hasUAAllow && !uaMatches(req.UserAgent, st.allowUAs) {
		return deny("user-agent not on allow list")
	}
	if st.hasDomainAllow && !domainAllowed(req.Referer, st.allowedDomains) {
		return deny("referer domain not allowed")
	}

	// 3. Policy gate: per-stream override, else global default.
	policy := st.defaultPolicy
	if a.policy != nil {
		if p := strings.ToLower(strings.TrimSpace(a.policy(req.Code))); p != "" {
			policy = p
		}
	}
	if policy == PolicyToken {
		if len(st.secret) == 0 {
			return deny("token policy but no secret configured")
		}
		if !st.verify(req.Code, req.Token) {
			return deny("missing or invalid token")
		}
	}
	return allow()
}

// Enabled reports whether media auth is on (for handlers / status).
func (a *Authorizer) Enabled() bool {
	st := a.state.Load()
	return st != nil && st.enabled
}

// ── playback token ──

// SignToken is the Go reference implementation of the playback-token format.
// The server only VERIFIES tokens; clients mint them with the shared secret —
// reproduce this in any language:
//
//	exp   = future expiry, unix seconds (decimal string)
//	msg   = "<stream_code>|<exp>"
//	sig   = HMAC-SHA256(secret, msg)               // raw 32 bytes
//	token = "<exp>." + base64url-nopad(sig)
//	URL   = .../<code>/index.m3u8?token=<token>     // (or SRT streamid / RTSP query)
func SignToken(secret []byte, code domain.StreamCode, exp int64) string {
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(tokenMAC(secret, code, exp))
}

func (st *state) verify(code domain.StreamCode, token string) bool {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		return false
	}
	exp, err := strconv.ParseInt(token[:dot], 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Add(-clockSkew).Unix() > exp {
		return false // expired
	}
	got, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return false
	}
	want := tokenMAC(st.secret, code, exp)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func tokenMAC(secret []byte, code domain.StreamCode, exp int64) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(string(code)))
	m.Write([]byte{'|'})
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return m.Sum(nil)
}

// ── helpers ──

func parseIPRules(rules []string) (nets []*net.IPNet, exact map[string]struct{}) {
	exact = map[string]struct{}{}
	for _, raw := range rules {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			exact[ip.String()] = struct{}{}
		}
	}
	return nets, exact
}

func ipInRules(ip net.IP, nets []*net.IPNet, exact map[string]struct{}) bool {
	if ip == nil {
		return false
	}
	if _, ok := exact[ip.String()]; ok {
		return true
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func uaMatches(ua string, patterns []string) bool {
	if ua == "" || len(patterns) == 0 {
		return false
	}
	lc := strings.ToLower(ua)
	for _, p := range patterns {
		if p != "" && strings.Contains(lc, p) {
			return true
		}
	}
	return false
}

// domainAllowed parses the Referer and checks its host against the allow-list
// (exact match or a parent domain, e.g. "example.com" covers "play.example.com").
func domainAllowed(referer string, domains []string) bool {
	if referer == "" {
		return false
	}
	u, err := url.Parse(referer)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func upperSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

func lowerList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
