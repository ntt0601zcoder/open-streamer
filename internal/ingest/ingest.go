// Package ingest reads media from input URLs and hands it to the pipeline as
// frames in source time.
//
// A Reader emits frames at 90 kHz, unwrapped, with per-track DTS
// non-decreasing within one source timeline; only the Clock maps them to
// stream time. A timeline break the reader knows about is reported once as
// ErrDiscontinuity.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// ErrDiscontinuity is returned once by Reader.Read at a timeline break; the
// next Read continues.
var ErrDiscontinuity = errors.New("ingest: discontinuity")

// Spec describes how the pipeline must treat an input.
type Spec struct {
	Pace         bool          // release at real time against epoch anchors (file, HLS pull)
	Live         bool          // the source runs in real time: Clock drift re-anchoring
	Passive      bool          // the source connects to us (UDP, SRT listener)
	StallTimeout time.Duration // raises the pipeline's stall limit for this input; 0 means none
}

// Frame is a media frame with its arrival time.
type Frame struct {
	media.Frame
	Arrival time.Time // when the reader received the bytes completing it
}

// Reader reads one input.
type Reader interface {
	// Read returns the next frame in source time. It returns ErrDiscontinuity
	// once per timeline break and io.EOF at the end.
	Read() (Frame, error)
	// Kinds returns the reader's tracks by index; it may grow after a PMT
	// change.
	Kinds() []media.Kind
	// Spec returns the effective spec after open.
	Spec() Spec
	// Stats returns named snake_case counters. It is safe concurrently with
	// Read.
	Stats() map[string]uint64
	// Close releases the input and unblocks a pending Read. It is safe
	// concurrently with Read.
	Close() error
}

// Env carries what readers need from the process.
type Env struct {
	// Lookup finds another stream's hub for copy://; nil disables it.
	Lookup func(stream string) (*hub.Hub, bool)
	// HTTP is the client for HTTP inputs; nil means a 10 s dial and
	// ResponseHeaderTimeout.
	HTTP *http.Client
	// Now stamps arrivals; nil means time.Now.
	Now func() time.Time
}

func (e Env) now() func() time.Time {
	if e.Now != nil {
		return e.Now
	}
	return time.Now
}

// Input URL schemes.
const (
	schemeFile  = "file"
	schemeUDP   = "udp"
	schemeSRT   = "srt"
	schemeHTTP  = "http"
	schemeHTTPS = "https"
	schemeCopy  = "copy"
)

// parsed is a URL checked and classified by parse.
type parsed struct {
	scheme string
	u      *url.URL // nil for a bare path
	path   string   // file inputs
	spec   Spec
	loop   bool // file inputs
}

// Parse returns the Spec of an input URL without any I/O. A bare path is a
// file.
func Parse(rawURL string) (Spec, error) {
	p, err := parse(rawURL)
	if err != nil {
		return Spec{}, err
	}
	return p.spec, nil
}

func parse(rawURL string) (parsed, error) {
	if rawURL == "" {
		return parsed{}, errors.New("ingest: parse: empty url")
	}
	if !strings.Contains(rawURL, "://") {
		return parsed{scheme: schemeFile, path: rawURL, spec: Spec{Pace: true}}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return parsed{}, fmt.Errorf("ingest: parse: invalid url %s", Redact(rawURL))
	}
	p := parsed{scheme: strings.ToLower(u.Scheme), u: u}
	q := u.Query()
	fail := func(format string, args ...any) (parsed, error) {
		return parsed{}, fmt.Errorf("ingest: parse %s: %s", Redact(rawURL), fmt.Sprintf(format, args...))
	}
	switch p.scheme {
	case schemeFile:
		if u.Host != "" && u.Host != "localhost" {
			return fail("file url has host %q", u.Host)
		}
		if u.Path == "" {
			return fail("file url has no path")
		}
		p.path = u.Path
		pace, err := queryBool(q, "pace", true)
		if err != nil {
			return fail("%v", err)
		}
		if p.loop, err = queryBool(q, "loop", false); err != nil {
			return fail("%v", err)
		}
		p.spec = Spec{Pace: pace}
	case schemeUDP:
		if err := checkPort(u.Port()); err != nil {
			return fail("%v", err)
		}
		p.spec = Spec{Live: true, Passive: true}
	case schemeSRT:
		if err := checkPort(u.Port()); err != nil {
			return fail("%v", err)
		}
		mode := q.Get("mode")
		if mode == "" && u.Hostname() != "" {
			mode = "caller"
		}
		switch mode {
		case "caller":
			if u.Hostname() == "" {
				return fail("caller mode needs a host")
			}
			p.spec = Spec{Live: true}
		case "listener":
			p.spec = Spec{Live: true, Passive: true}
		case "":
			return fail("no host and no mode")
		default:
			return fail("unsupported mode %q", mode)
		}
	case schemeHTTP, schemeHTTPS:
		if u.Hostname() == "" {
			return fail("no host")
		}
		p.spec = Spec{Live: true}
		if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
			p.spec.Pace = true
		}
	case schemeCopy:
		if u.Host == "" {
			return fail("no stream name")
		}
		p.spec = Spec{Live: true}
	default:
		return fail("unsupported scheme %q", u.Scheme)
	}
	return p, nil
}

func checkPort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func queryBool(q url.Values, key string, def bool) (bool, error) {
	v, ok := q[key]
	if !ok {
		return def, nil
	}
	switch v[0] {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	}
	return false, fmt.Errorf("invalid %s=%q", key, v[0])
}

// Open opens the input. ctx bounds only the open and handshake (up to the
// first PMT); the returned reader lives until Close.
func Open(ctx context.Context, rawURL string, env Env) (Reader, error) {
	p, err := parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ingest: open %s: %w", Redact(rawURL), err)
	}
	switch p.scheme {
	case schemeFile:
		return openFile(ctx, p, env)
	default:
		return nil, fmt.Errorf("ingest: open %s: %s inputs are not available yet", Redact(rawURL), p.scheme)
	}
}

// redactedKeys are the query parameters whose values never appear in errors
// or logs.
var redactedKeys = []string{"passphrase", "password", "token", "secret", "key"}

// Redact returns rawURL with its password and secret query values hidden.
func Redact(rawURL string) string {
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
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		case u.User.Username() != "" || pw != "":
			u.User = url.User("xxxxx")
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
			if slices.Contains(redactedKeys, strings.ToLower(k)) {
				pairs[i] = pair[:strings.IndexByte(pair, '=')+1] + "xxxxx"
			}
		}
		u.RawQuery = strings.Join(pairs, "&")
	}
	return u.String()
}
