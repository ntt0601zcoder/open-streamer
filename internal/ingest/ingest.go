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
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
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
	// ArrivalGranularity is the step between arrivals when the source
	// delivers in chunks (HLS: the target duration, set after open); 0 means
	// per frame. It widens the Clock's drift window.
	ArrivalGranularity time.Duration
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
	// HTTP is the client for HTTP inputs; nil means NewHTTPClient(nil): a
	// 10 s dial and ResponseHeaderTimeout, and the system roots.
	HTTP *http.Client
	// TLS is the base client config of rtmps://, rtsps:// and their +insecure
	// forms; nil means the system roots.
	TLS *tls.Config
	// Now stamps arrivals and times HLS playlist reloads and retries (the
	// waits themselves are real timers); nil means time.Now.
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
	schemeRTMP  = "rtmp"
	schemeRTMPS = "rtmps"

	// insecureSuffix on rtmps or rtsps turns certificate verification off
	// for that one URL: rtmps+insecure://, rtsps+insecure://.
	insecureSuffix = "+insecure"
)

// splitScheme splits a lower-case scheme into its base and whether it
// carries the +insecure suffix.
func splitScheme(scheme string) (base string, insecure bool) {
	return strings.CutSuffix(scheme, insecureSuffix)
}

// parsed is a URL checked and classified by parse.
type parsed struct {
	scheme   string   // lower case, without +insecure
	insecure bool     // rtmps+insecure or rtsps+insecure: the certificate is not verified
	u        *url.URL // nil for a bare path
	path     string   // file inputs
	spec     Spec
	loop     bool // file inputs
}

// tlsDial translates a pull URL for its library, pkg/rtmp or pkg/rtsp, which
// know only rtmps:// and rtsps:// and a *tls.Config: the URL, and base as the
// client config. For a +insecure URL it is the same URL without the suffix,
// byte for byte after the scheme, and a clone of base (a new config when
// nil) with InsecureSkipVerify set, for this URL only; base itself is not
// modified. The suffix never reaches the peer.
//
// rtmppush's insecureTarget is the twin of this translation for
// rtmps+insecure:// push targets; the two change together.
func (p parsed) tlsDial(rawURL string, base *tls.Config) (string, *tls.Config) {
	if !p.insecure {
		return rawURL, base
	}
	// The scheme ends at the first ':' (RFC 3986), as url.Parse reads it.
	dial := p.scheme + rawURL[strings.IndexByte(rawURL, ':'):]
	c := base.Clone()
	if c == nil {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	c.InsecureSkipVerify = true
	return dial, c
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
	p := parsed{u: u}
	p.scheme, p.insecure = splitScheme(strings.ToLower(u.Scheme))
	q := u.Query()
	fail := func(format string, args ...any) (parsed, error) {
		return parsed{}, fmt.Errorf("ingest: parse %s: %s", Redact(rawURL), fmt.Sprintf(format, args...))
	}
	if p.insecure && p.scheme != schemeRTMPS && p.scheme != schemeRTSPS {
		return fail("unsupported scheme %q", u.Scheme)
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
	case schemeRTMP, schemeRTMPS:
		spec, err := parseRTMP(u, q)
		if err != nil {
			return fail("%v", err)
		}
		if p.insecure && spec.Passive {
			return fail("+insecure is for a pull: a listener presents the server's certificate")
		}
		p.spec = spec
	case schemeRTSP, schemeRTSPS:
		spec, err := parseRTSP(u, q, p.scheme == schemeRTSPS)
		if err != nil {
			return fail("%v", err)
		}
		p.spec = spec
	default:
		return fail("unsupported scheme %q", u.Scheme)
	}
	return p, nil
}

// parseRTMP checks an rtmp:// or rtmps:// URL: a pull (the default with a host) or a
// listen (mode=listen or listener, any host) for app[/…]/stream.
func parseRTMP(u *url.URL, q url.Values) (Spec, error) {
	if u.Port() != "" {
		if err := checkPort(u.Port()); err != nil {
			return Spec{}, err
		}
	}
	if path, _ := rtmp.NormalizePath(u.Path, ""); !strings.Contains(path, "/") {
		return Spec{}, errors.New("the path needs an app and a stream name")
	}
	mode := q.Get("mode")
	if mode == "" && u.Hostname() != "" {
		mode = "pull"
	}
	switch mode {
	case "pull":
		if u.Hostname() == "" {
			return Spec{}, errors.New("pull mode needs a host")
		}
		return Spec{Live: true}, nil
	case "listen", "listener":
		return Spec{Live: true, Passive: true}, nil
	case "":
		return Spec{}, errors.New("no host and no mode")
	}
	return Spec{}, fmt.Errorf("unsupported mode %q", mode)
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
	case schemeSRT:
		return openSRT(ctx, rawURL, p, env)
	case schemeUDP:
		return openUDP(ctx, rawURL, p, env)
	case schemeHTTP, schemeHTTPS:
		return openHTTP(ctx, rawURL, p, env)
	case schemeCopy:
		return openCopy(ctx, rawURL, p, env)
	case schemeRTMP, schemeRTMPS:
		if p.spec.Passive {
			return openRTMPListen(ctx, rawURL, p, env)
		}
		return openRTMPPull(ctx, rawURL, p, env)
	case schemeRTSP, schemeRTSPS:
		return openRTSP(ctx, rawURL, p, env)
	default:
		return nil, fmt.Errorf("ingest: open %s: %s inputs are not available yet", Redact(rawURL), p.scheme)
	}
}

// Redact returns rawURL with its password and secret query values hidden,
// and for rtmp://, rtmps:// and their +insecure spellings its stream name
// (the last path segment, the stream key) cut to its first 2 characters. The
// scheme is kept as written. It is redact.URL.
func Redact(rawURL string) string {
	return redact.URL(rawURL)
}
