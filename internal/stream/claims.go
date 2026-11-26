package stream

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// The errors of an edit.
var (
	ErrInvalid  = errors.New("stream: invalid spec")   // wraps Check's errors
	ErrConflict = errors.New("stream: conflict")       // a listen claim or a copy:// cycle
	ErrClosed   = errors.New("stream: manager closed") // Run's context has ended
)

// serverOwner owns the claims of the server's own listeners.
const serverOwner = "server"

// editError is one reason a stream cannot run: kind is ErrInvalid or
// ErrConflict, and errors.Is finds it, and the cause when there is one. A
// refused edit joins one of these per reason, so an error may hold both
// kinds.
type editError struct {
	kind  error
	name  string // the stream
	msg   string // scrubbed; without the stream's name
	cause error  // Check's error; nil for a conflict
}

func (e *editError) Error() string { return fmt.Sprintf("stream %q: %s", e.name, e.msg) }
func (e *editError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// invalid is err, of Check, as an ErrInvalid of the stream name.
func invalid(name string, err error) *editError {
	msg := strings.TrimPrefix(logging.Scrub(err.Error()), "stream: ")
	return &editError{kind: ErrInvalid, name: name, msg: msg, cause: err}
}

// conflictErr is an ErrConflict of the stream name. Its text is built from
// addresses and stream names only: never from a URL or a publish path, which
// is a stream key.
func conflictErr(name, format string, args ...any) *editError {
	return &editError{kind: ErrConflict, name: name, msg: fmt.Sprintf(format, args...)}
}

// normalHost returns host as rtmpsrv.AddrKey writes it: "" for no host and
// for every unspecified address, an IP address canonically, a name as it is.
func normalHost(host string) string {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if ip = ip.Unmap(); ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

// splitKey splits a key of rtmpsrv.AddrKey into its host and port.
func splitKey(key string) (host string, port int, ok bool) {
	host, p, err := net.SplitHostPort(key)
	if err != nil {
		return "", 0, false
	}
	port, err = strconv.Atoi(p)
	return host, port, err == nil
}

// claimsOf returns the listen claims of sp, one per listen input, in the
// inputs' order.
func claimsOf(sp spec.Stream) []Claim {
	var out []Claim
	for _, in := range sp.Inputs {
		if c, ok := claimOf(in.URL); ok {
			c.Owner = sp.Name
			out = append(out, c)
		}
	}
	return out
}

// claimOf returns the listen claim of one input: an RTMP listen input claims
// its path on the listener of its address, an RTMPS listener for rtmps://;
// a UDP or an SRT listen input claims its UDP address. A UDP input on a
// multicast group claims nothing: any number of sockets join one group.
// Every other input, and one that does not parse (Check refuses it), claims
// nothing.
func claimOf(input string) (Claim, bool) {
	if key, path, secure, ok := rtmpListenKey(input); ok {
		host, port, ok := splitKey(key)
		return Claim{RTMP: true, TLS: secure, Host: host, Port: port, Path: path}, ok
	}
	parsed, err := ingest.Parse(input)
	if err != nil || !parsed.Passive {
		return Claim{}, false
	}
	u, err := url.Parse(input)
	if err != nil {
		return Claim{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "udp" && scheme != "srt" {
		return Claim{}, false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && ip.IsMulticast() && scheme == "udp" {
		return Claim{}, false
	}
	key, err := rtmpsrv.AddrKey(net.JoinHostPort(u.Hostname(), u.Port()))
	if err != nil {
		return Claim{}, false
	}
	host, port, ok := splitKey(key)
	return Claim{UDP: true, Host: host, Port: port}, ok
}

// conflict reports whether two claims cannot both be held (R-D11): the same
// family and port, and the same host or a wildcard on either side. Two RTMP
// claims on one address share its listener when both are TLS or neither is,
// and conflict only as two publishers of one path; a plain and a TLS one
// cannot share an address.
func conflict(a, b Claim) bool {
	if a.UDP != b.UDP || a.Port != b.Port {
		return false
	}
	if sharing(a, b) { // one shared RTMP listener
		// Two publishers of one path; a play listener ("" path) shares with all.
		return a.Path != "" && a.Path == b.Path
	}
	return a.Host == b.Host || a.Host == "" || b.Host == ""
}

// sharing reports whether two claims are on one shared RTMP listener: both
// RTMP, both TLS or both plain, on one host.
func sharing(a, b Claim) bool {
	return a.RTMP && b.RTMP && a.TLS == b.TLS && a.Host == b.Host
}

// taken returns the first claim of held that c conflicts with.
func taken(held []Claim, c Claim) (Claim, bool) {
	for _, h := range held {
		if conflict(h, c) {
			return h, true
		}
	}
	return Claim{}, false
}

// addr is the claim's address for an error: its family, host and port, and
// never its path.
func (c Claim) addr() string {
	family := "tcp"
	if c.UDP {
		family = "udp"
	}
	return family + " " + net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// holder names the claim's owner for an error.
func (c Claim) holder() string {
	if c.Owner == serverOwner || c.Owner == "" {
		return "the server"
	}
	return fmt.Sprintf("stream %q", c.Owner)
}

// claimErrors returns one ErrConflict for each claim of claims, a stream's,
// that conflicts with a claim of held or with an earlier one of claims.
func claimErrors(name string, claims, held []Claim) []*editError {
	var errs []*editError
	for i, c := range claims {
		other, ok := taken(held, c)
		if !ok {
			other, ok = taken(claims[:i], c)
		}
		if !ok {
			continue
		}
		if sharing(c, other) {
			errs = append(errs, conflictErr(name, "listen address %s has the RTMP publish path of an input of %s", c.addr(), other.holder()))
			continue
		}
		if c.RTMP && other.RTMP && c.TLS != other.TLS {
			kind := "RTMP"
			if other.TLS {
				kind = "RTMPS"
			}
			errs = append(errs, conflictErr(name, "listen address %s conflicts with the %s listener %s of %s", c.addr(), kind, other.addr(), other.holder()))
			continue
		}
		errs = append(errs, conflictErr(name, "listen address %s conflicts with %s of %s", c.addr(), other.addr(), other.holder()))
	}
	return errs
}

// copyTargets returns the streams that sp reads through its copy:// inputs:
// copy://x and copy://x_label both read x.
func copyTargets(sp spec.Stream) []string {
	var out []string
	for _, in := range sp.Inputs {
		u, err := url.Parse(in.URL)
		if err != nil || !strings.EqualFold(u.Scheme, "copy") || u.Host == "" {
			continue
		}
		name, _, _ := strings.Cut(u.Host, "_")
		out = append(out, name)
	}
	return out
}

// cyclePath returns the first path, depth first, that leads from start back
// to start over next's edges, as "a -> b -> a"; "" when there is none.
func cyclePath(start string, next func(string) []string) string {
	seen := map[string]bool{}
	var walk func(n string, path []string) []string
	walk = func(n string, path []string) []string {
		for _, t := range next(n) {
			if t == start {
				return append(path, t)
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			if p := walk(t, append(path, t)); p != nil {
				return p
			}
		}
		return nil
	}
	return strings.Join(walk(start, []string{start}), " -> ")
}

// cycleError returns an ErrConflict when the stream name reads itself
// through copy:// inputs, over the streams of running: a stream that is not
// there is no edge, so a copy of an unknown stream is accepted (it waits).
func cycleError(name string, running map[string]spec.Stream) *editError {
	path := cyclePath(name, func(n string) []string {
		var out []string
		for _, t := range copyTargets(running[n]) {
			if _, ok := running[t]; ok {
				out = append(out, t)
			}
		}
		return out
	})
	if path == "" {
		return nil
	}
	return conflictErr(name, "copy cycle %s", path)
}
