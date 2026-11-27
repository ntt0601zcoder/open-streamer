package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/asset"
	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/certs"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmpplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtspplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/srtout"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

const (
	// rtmpTries is how often New resolves and registers an RTMP address
	// whose configured port is 0.
	rtmpTries = 5
	// shutdownWait is how long an HTTP server gets to finish its requests.
	shutdownWait = 2 * time.Second
	// rtspUDPWait is how long New waits for the RTSP server to bind its
	// UDP pair.
	rtspUDPWait = 5 * time.Second
	// headerTimeout is how long a client of an HTTP server gets to send its
	// request header and, on a TLS listener, to finish its handshake.
	headerTimeout = 10 * time.Second
	// transcoderName is the transcoder looked for next to the executable
	// when the config names none.
	transcoderName = "ostranscoder"
	// certPoll is how often Run looks at the certificate files (R-S3).
	certPoll = 30 * time.Second
)

// Options are what New takes beside the config file.
type Options struct {
	Version string
	LogOut  io.Writer // nil means os.Stderr
	// RequiredIterations is passed to config.Options.RequiredIterations; 0
	// means auth.Iterations. Tests only; cmd/openstreamer never sets it.
	RequiredIterations int
	// Open is passed to stream.Env.Open. Tests only (a reader whose Close
	// the test holds is how a teardown is held); nil means ingest.Open.
	// cmd/openstreamer never sets it.
	Open func(ctx context.Context, url string) (ingest.Reader, error)
	// TranscoderEnv is added to the environment of every transcoder child
	// and of the font probe. Tests only; cmd/openstreamer never sets it.
	TranscoderEnv []string
	// RootCAs is passed to stream.Env.RootCAs: the roots of the streams'
	// TLS clients. Tests only; cmd/openstreamer never sets it.
	RootCAs *x509.CertPool
	// HeaderTimeout is both HTTP servers' ReadHeaderTimeout, which is also
	// their TLS handshake timeout; 0 means 10 s. Tests only;
	// cmd/openstreamer never sets it.
	HeaderTimeout time.Duration
	// CertPoll is how often Run looks at the certificate files (R-S3); 0
	// means 30 s. Tests only; cmd/openstreamer never sets it.
	CertPoll time.Duration
}

// Addrs are the listeners' addresses, host:port as bound; "" when off. HTTPS,
// APITLS, RTMPS and RTSPS are the TLS listeners beside HTTP, API, RTMP and
// RTSP.
type Addrs struct{ HTTP, HTTPS, API, APITLS, RTMP, RTMPS, RTSP, RTSPS, SRT string }

// Server is the running process: the listeners, the manager and the config
// in force. It implements api.Backend.
type Server struct {
	log      *slog.Logger
	addrs    Addrs
	parseOpt config.Options
	started  config.Server // the resolved server section the process started with

	// manager is set once, in New; the output servers' lookups, which a
	// client can reach before New returns, read it through the pointer.
	manager atomic.Pointer[stream.Manager]
	file    *config.File
	users   atomic.Pointer[auth.Users]
	state   atomic.Pointer[state] // the text in force and its status

	// assets is the one Store of server.assets_dir, opened once in New and
	// never again (a second Open would sweep the temp files of the uploads
	// in flight); every method of the Store of no directory is
	// asset.ErrOff. assetsOn is whether there is a directory.
	assets   *asset.Store
	assetsOn bool

	// cfg serialises the writers and guards the fields below.
	cfg           sync.Mutex
	applied       []byte // the text in force
	loadedAt      time.Time
	loadErrs      []string // scrubbed: the entries the start skipped, and the streams that cannot run
	skipped       bool     // the text in force holds entries the start skipped: it does not parse strictly
	lastReloadAt  *time.Time
	lastReloadErr string
	restart       []string

	reloadsOK, reloadsFailed atomic.Uint64
	staleLogged              atomic.Int64 // when a Stale error was last logged, Unix nanoseconds

	// store is the certificate of every TLS listener; nil without
	// server.tls. certPoll is how often Run's watch looks at its files.
	store    *certs.Store
	certPoll time.Duration
	// certMu orders the certificate's log lines; certLogged is the text of
	// the reload error last logged, "" once a reload succeeded; certShown is
	// Info.Changed of the pair last logged as loaded. Never taken with cfg
	// held, nor cfg with it.
	certMu     sync.Mutex
	certLogged string
	certShown  uint64
	// httpsFailures and apiFailures count the failed TLS handshakes of the
	// media and the API listener; the RTMPS and RTSPS listeners count their
	// own.
	httpsFailures, apiFailures atomic.Uint64
	// stopping is set by Run before its first shutdown: a TLS handshake
	// that fails from then on was cut short by the stop, and is neither
	// counted nor logged.
	stopping atomic.Bool

	mediaLn, apiLn     net.Listener
	mediaTLS, apiTLS   *trackListener // the HTTPS listeners; nil when off
	mediaSrv, apiSrv   *http.Server
	apiCtx             context.Context //nolint:containedctx // the API requests' base context, ended at shutdown
	apiCancel          context.CancelFunc
	rtmp               *rtmpplay.Server // nil when both of its listeners are off
	rtsp               *rtspplay.Server // nil when both of its listeners are off
	srt                *srtout.Server   // nil when off
	stopRTMP, stopRTSP func()
	stopSRT            func()
	failed             chan error // a listener that failed after start-up
}

// New opens and parses the config file (leniently for streams, R-D4), binds
// every listener (so Addrs is final when it returns), and loads the streams
// into a Manager whose Env carries the bound addresses. The streams the
// file holds start; Run stops them, so a Server that New returned must be
// Run.
func New(path string, opt Options) (*Server, error) {
	auth.WarmDummy()
	file, text, err := config.Open(path)
	if err != nil {
		return nil, err
	}
	parseOpt := config.Options{RequiredIterations: opt.RequiredIterations}
	parsed, skipped, err := config.ParseLenient(text, parseOpt)
	if err != nil {
		return nil, err
	}
	sec := parsed.Doc.Server.Resolved()
	if err := sec.CheckFiles(); err != nil {
		return nil, err
	}
	out := opt.LogOut
	if out == nil {
		out = os.Stderr
	}
	log, err := logging.New(out, sec.Log.Level, sec.Log.Format)
	if err != nil {
		return nil, fmt.Errorf("server: new: %w", err)
	}
	transcoder := sec.Transcoder.Binary
	if transcoder == "" {
		transcoder = defaultTranscoder()
	}
	// Without a transcoder no font is probed: there is no child to hurt.
	var assetOpt asset.Options
	if transcoder != "" {
		assetOpt.ProbeFont = fontProber(transcoder, opt.TranscoderEnv, fontProbeLimit)
	}
	assets, err := asset.Open(sec.AssetsDir, assetOpt)
	if err != nil {
		return nil, fmt.Errorf("server: new: %w", err)
	}
	s := &Server{
		log:      log,
		parseOpt: parseOpt,
		started:  sec,
		file:     file,
		assets:   assets,
		assetsOn: sec.AssetsDir != "",
		applied:  text,
		loadedAt: time.Now(),
		restart:  []string{},
		stopRTMP: func() {},
		stopRTSP: func() {},
		stopSRT:  func() {},
		failed:   make(chan error, 4),
		certPoll: opt.CertPoll,
	}
	if s.certPoll <= 0 {
		s.certPoll = certPoll
	}
	s.users.Store(parsed.Users)
	s.apiCtx, s.apiCancel = context.WithCancel(context.Background())
	if sec.TLS.CertFile != "" {
		if s.store, err = certs.Load(sec.TLS.CertFile, sec.TLS.KeyFile); err != nil {
			return nil, fmt.Errorf("server: tls: %w", err)
		}
		warnKeyMode(log, sec.TLS.KeyFile)
		if sec.HTTP.TLSListen == "" && sec.API.TLSListen == "" && sec.RTMP.TLSListen == "" && sec.RTSP.TLSListen == "" {
			log.Warn("server: tls: a certificate is set and no tls_listen is: nothing is served over TLS; set server.http.tls_listen, server.api.tls_listen, server.rtmp.tls_listen or server.rtsp.tls_listen")
		}
	}
	reserved, err := s.listen(sec)
	if err != nil {
		s.closeListeners()
		return nil, err
	}

	env := stream.Env{
		Transcoder:    transcoder,
		AssetsDir:     sec.AssetsDir,
		Reserved:      reserved,
		Logger:        log,
		Open:          opt.Open,
		TranscoderEnv: opt.TranscoderEnv,
		RootCAs:       opt.RootCAs,
	}
	// The play app is both RTMP listeners': with only the RTMPS one on, an
	// rtmps listen input must still stay out of it.
	if s.addrs.RTMP != "" || s.addrs.RTMPS != "" {
		env.RTMPApp = sec.RTMP.App
	}
	env.RTMPPlay, env.RTMPSPlay = s.addrs.RTMP, s.addrs.RTMPS
	env.RTSPPlay, env.RTSPSPlay = s.addrs.RTSP, s.addrs.RTSPS
	env.SRTPlay = s.addrs.SRT
	m := stream.NewManager(env)

	reg := &metrics.Registry{}
	a, err := api.New(api.Options{
		Backend: s,
		Users:   s.users.Load,
		Limiter: auth.NewLimiter(nil),
		Metrics: reg.Handler(),
		Version: opt.Version,
		Logger:  log,
	})
	if err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("server: new: %w", err)
	}
	reg.Register(metrics.Process(time.Now()))
	reg.Register(func(w *metrics.Writer) {
		w.Gauge("openstreamer_build_info", "The version of the server and of Go; always 1.", 1,
			metrics.L("version", opt.Version), metrics.L("goversion", runtime.Version()))
	})
	reg.Register(m.Collect)
	reg.Register(a.Collect)
	reg.Register(s.collect)
	header := opt.HeaderTimeout
	if header <= 0 {
		header = headerTimeout
	}
	s.apiSrv = &http.Server{
		Handler:           a,
		ReadHeaderTimeout: header,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return s.apiCtx },
		ErrorLog:          s.httpErrorLog("api", &s.apiFailures),
	}
	// No ReadTimeout or WriteTimeout: either would cut the long-lived
	// HTTP-TS responses, and each handler sets its own write deadline.
	s.mediaSrv = &http.Server{
		Handler:           s.mediaHandler(),
		ReadHeaderTimeout: header,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          s.httpErrorLog("https", &s.httpsFailures),
	}
	// Each server serves both of its listeners. The TLS ones speak HTTP/1.1
	// only (R-S4): the empty map is how net/http is told to offer no h2.
	if s.store != nil {
		for _, srv := range []*http.Server{s.apiSrv, s.mediaSrv} {
			srv.TLSConfig = s.store.ServerConfig()
			srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
		}
	}

	// Nothing fails from here on: Load starts the streams, which only Run
	// stops.
	for _, e := range skipped {
		s.loadErrs = append(s.loadErrs, logging.Scrub(e.Error()))
	}
	s.skipped = len(skipped) > 0
	// Load fails only on a manager that is closed or not empty, which this
	// new one is not.
	left, _ := m.Load(parsed.Doc.Streams)
	s.loadErrs = append(s.loadErrs, stoppedTexts(left)...)
	for _, e := range s.loadErrs {
		log.Error("server: config: " + e)
	}
	warnAPI(log, s.addrs.API)
	s.manager.Store(m)
	s.publish()
	return s, nil
}

// loopback reports whether the bound address addr is on a loopback
// interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// warnAPI logs once, at the start, that the plain API listener is beyond
// loopback: Basic credentials, the config and the stream keys in it cross
// the network in clear there. The API for the network is
// server.api.tls_listen (R-S5).
func warnAPI(log *slog.Logger, addr string) {
	if loopback(addr) {
		return
	}
	log.Warn("server: api.listen is not a loopback address: that listener has no TLS, so passwords and stream keys travel in clear; keep it on loopback and serve the network from server.api.tls_listen",
		"listen", addr)
}

// stoppedTexts returns the reasons of the streams that cannot run, scrubbed.
func stoppedTexts(left []stream.Stopped) []string {
	out := make([]string, len(left))
	for i, st := range left {
		out[i] = logging.Scrub(st.Err.Error())
	}
	return out
}

// defaultTranscoder returns the ostranscoder next to this executable, or ""
// when there is none.
func defaultTranscoder() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return transcoderBeside(exe)
}

// transcoderBeside returns the ostranscoder in the directory of exe, or ""
// when there is none.
func transcoderBeside(exe string) string {
	name := transcoderName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

// lookup returns the hub lookup of one output for its server.
func (s *Server) lookup(out stream.Output) func(string) (*hub.Hub, bool) {
	return func(name string) (*hub.Hub, bool) {
		m := s.manager.Load()
		if m == nil {
			return nil, false
		}
		return m.Hub(out, name)
	}
}

// secureSRT asks the manager in force for the hub of a served name and the
// passphrase its SRT callers must present, from one reading of the stream's
// state. It answers ok == false while there is no manager.
func (s *Server) secureSRT(name string) (*hub.Hub, string, bool) {
	m := s.manager.Load()
	if m == nil {
		return nil, "", false
	}
	return m.SRT(name)
}

// listen binds every listener of sec, fills s.addrs and returns the claims
// of what was bound. On an error the caller closes what was bound so far.
func (s *Server) listen(sec config.Server) ([]stream.Claim, error) {
	var (
		lc       net.ListenConfig
		ctx      = context.Background()
		reserved []stream.Claim
		err      error
	)
	reserve := func(addr string, c stream.Claim) error {
		host, port, err := hostPort(addr)
		if err != nil {
			return fmt.Errorf("server: listen: %w", err)
		}
		c.Host, c.Port, c.Owner = host, port, "server"
		reserved = append(reserved, c)
		return nil
	}

	if s.mediaLn, err = lc.Listen(ctx, "tcp", sec.HTTP.Listen); err != nil {
		return nil, fmt.Errorf("server: http.listen %s: %w", sec.HTTP.Listen, err)
	}
	s.addrs.HTTP = s.mediaLn.Addr().String()
	if err := reserve(s.addrs.HTTP, stream.Claim{}); err != nil {
		return nil, err
	}
	if sec.HTTP.TLSListen != "" {
		ln, err := lc.Listen(ctx, "tcp", sec.HTTP.TLSListen)
		if err != nil {
			return nil, fmt.Errorf("server: http.tls_listen %s: %w", sec.HTTP.TLSListen, err)
		}
		s.mediaTLS = newTrackListener(ln)
		s.addrs.HTTPS = ln.Addr().String()
		if err := reserve(s.addrs.HTTPS, stream.Claim{}); err != nil {
			return nil, err
		}
	}

	if s.apiLn, err = lc.Listen(ctx, "tcp", sec.API.Listen); err != nil {
		return nil, fmt.Errorf("server: api.listen %s: %w", sec.API.Listen, err)
	}
	s.addrs.API = s.apiLn.Addr().String()
	if err := reserve(s.addrs.API, stream.Claim{}); err != nil {
		return nil, err
	}
	if sec.API.TLSListen != "" {
		ln, err := lc.Listen(ctx, "tcp", sec.API.TLSListen)
		if err != nil {
			return nil, fmt.Errorf("server: api.tls_listen %s: %w", sec.API.TLSListen, err)
		}
		s.apiTLS = newTrackListener(ln)
		s.addrs.APITLS = ln.Addr().String()
		if err := reserve(s.addrs.APITLS, stream.Claim{}); err != nil {
			return nil, err
		}
	}

	// One play server for both RTMP listeners, made when either is on.
	if sec.RTMP.Listen != "" || sec.RTMP.TLSListen != "" {
		s.rtmp = rtmpplay.New(s.lookup(stream.OutRTMP), rtmpplay.Options{})
	}
	if sec.RTMP.Listen != "" {
		addr, stop, err := registerRTMP(sec.RTMP.Listen, sec.RTMP.App, s.rtmp, nil)
		if err != nil {
			s.rtmp = nil
			return nil, fmt.Errorf("server: rtmp.listen %s: %w", sec.RTMP.Listen, err)
		}
		s.addrs.RTMP, s.stopRTMP = addr, stop
		// An RTMP claim without a path: the listen inputs on this address
		// share the listener.
		if err := reserve(addr, stream.Claim{RTMP: true}); err != nil {
			return nil, err
		}
	}
	if sec.RTMP.TLSListen != "" {
		// The hook runs inside the listener's wait group: it only logs.
		addr, stop, err := registerRTMP(sec.RTMP.TLSListen, sec.RTMP.App, s.rtmp, &rtmpsrv.TLSOptions{
			Config:  s.store.ServerConfig(),
			OnError: func(remote net.Addr, err error) { s.tlsFailed("rtmps", remote.String(), err.Error()) },
		})
		if err != nil {
			if s.addrs.RTMP == "" {
				s.rtmp = nil
			}
			return nil, fmt.Errorf("server: rtmp.tls_listen %s: %w", sec.RTMP.TLSListen, err)
		}
		plain := s.stopRTMP
		s.addrs.RTMPS, s.stopRTMP = addr, func() { plain(); stop() }
		// The rtmps listen inputs on this address share the listener.
		if err := reserve(addr, stream.Claim{RTMP: true, TLS: true}); err != nil {
			return nil, err
		}
	}

	// One play server for both RTSP listeners, made when either is on.
	if sec.RTSP.Listen != "" || sec.RTSP.TLSListen != "" {
		var ln, tlsLn net.Listener
		if sec.RTSP.Listen != "" {
			if ln, err = lc.Listen(ctx, "tcp", sec.RTSP.Listen); err != nil {
				return nil, fmt.Errorf("server: rtsp.listen %s: %w", sec.RTSP.Listen, err)
			}
		}
		if sec.RTSP.TLSListen != "" {
			if tlsLn, err = lc.Listen(ctx, "tcp", sec.RTSP.TLSListen); err != nil {
				if ln != nil {
					_ = ln.Close()
				}
				return nil, fmt.Errorf("server: rtsp.tls_listen %s: %w", sec.RTSP.TLSListen, err)
			}
		}
		if err := s.startRTSPBoth(ln, tlsLn, sec); err != nil {
			if ln == nil {
				return nil, fmt.Errorf("server: rtsp.tls_listen %s: %w", sec.RTSP.TLSListen, err)
			}
			return nil, fmt.Errorf("server: rtsp.listen %s: %w", sec.RTSP.Listen, err)
		}
		if ln != nil {
			s.addrs.RTSP = ln.Addr().String()
			if err := reserve(s.addrs.RTSP, stream.Claim{}); err != nil {
				return nil, err
			}
		}
		if tlsLn != nil {
			s.addrs.RTSPS = tlsLn.Addr().String()
			if err := reserve(s.addrs.RTSPS, stream.Claim{}); err != nil {
				return nil, err
			}
		}
	}

	if sec.SRT.Listen != "" {
		if err := s.startSRT(sec); err != nil {
			return nil, fmt.Errorf("server: srt.listen %s: %w", sec.SRT.Listen, err)
		}
		s.addrs.SRT = s.srt.Addr().String()
		if err := reserve(s.addrs.SRT, stream.Claim{UDP: true}); err != nil {
			return nil, err
		}
	}
	return reserved, nil
}

// hostPort splits a bound address into the host as a claim has it (a
// wildcard is "", an IP address canonical) and the port.
func hostPort(addr string) (string, int, error) {
	key, err := rtmpsrv.AddrKey(addr)
	if err != nil {
		return "", 0, err
	}
	host, p, err := net.SplitHostPort(key)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(p)
	return host, port, err
}

// resolveRTMP returns addr unchanged when its port is not 0. For port 0 it
// binds a TCP listener on the host, reads the port and closes it (R-D13):
// the RTMP registry keys its listeners by the requested address and takes
// no port 0.
func resolveRTMP(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("server: resolve: %w", err)
	}
	if n, err := strconv.Atoi(port); err != nil || n != 0 {
		return addr, nil //nolint:nilerr // not port 0: the registry judges the address
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", fmt.Errorf("server: resolve: %w", err)
	}
	bound := ln.Addr().(*net.TCPAddr).Port //nolint:errcheck,forcetypeassert // a TCP listener's address
	if err := ln.Close(); err != nil {
		return "", fmt.Errorf("server: resolve: %w", err)
	}
	return net.JoinHostPort(host, strconv.Itoa(bound)), nil
}

// registerRTMP registers the play handler on the process's RTMP registry,
// which the listen inputs share, and returns the concrete address: on an
// RTMPS listener when tlsOpt is set, on a plain one otherwise. When the
// configured port is 0 the pair resolve-and-register is tried up to
// rtmpTries times, on any error: another process may take the port in
// between, and another server of this process may be given the same one,
// which the registry refuses as already registered.
func registerRTMP(listen, app string, h rtmpsrv.PlayHandler, tlsOpt *rtmpsrv.TLSOptions) (addr string, stop func(), err error) {
	tries := 1
	if _, port, e := net.SplitHostPort(listen); e == nil && port == "0" {
		tries = rtmpTries
	}
	for range tries {
		if addr, err = resolveRTMP(listen); err != nil {
			continue
		}
		if tlsOpt != nil {
			stop, err = rtmpsrv.Default.RegisterPlayTLS(addr, app, h, rtmp.Config{}, rtmpsrv.Policy{}, *tlsOpt)
		} else {
			stop, err = rtmpsrv.Default.RegisterPlay(addr, app, h, rtmp.Config{}, rtmpsrv.Policy{})
		}
		if err == nil {
			return addr, stop, nil
		}
	}
	return "", nil, err
}

// startRTSP serves RTSP players on the plain listener ln alone.
func (s *Server) startRTSP(ln net.Listener, sec config.Server) error {
	return s.startRTSPBoth(ln, nil, sec)
}

// startRTSPBoth serves RTSP players on ln and RTSPS players on tlsLn with
// one play server; either may be nil, and it owns both from here on. It
// returns once the UDP pair, if any, is bound: server.rtsp.udp is the plain
// listener's, and RTSPS sessions are interleaved whatever it says.
func (s *Server) startRTSPBoth(ln, tlsLn net.Listener, sec config.Server) error {
	opt := rtspplay.Options{App: sec.RTSP.App}
	if tlsLn != nil {
		// The hook runs in the connection's goroutine: it only logs.
		opt.Server.TLSError = func(remote net.Addr, err error) { s.tlsFailed("rtsps", remote.String(), err.Error()) }
	}
	if sec.RTSP.UDP && ln != nil {
		host, _, _ := net.SplitHostPort(sec.RTSP.Listen)
		if host == "" {
			host = "0.0.0.0" // every interface
		}
		opt.Server.UDPHost = host
	}
	srv := rtspplay.New(s.lookup(stream.OutRTSP), opt)
	var wg sync.WaitGroup
	if ln != nil {
		served := make(chan error, 1)
		go func() { served <- srv.Serve(ln) }()
		if sec.RTSP.UDP {
			// Serve binds the pair before it accepts; wait for it so that a
			// bind failure is a start-up error.
			if ended, err := waitUDP(srv.UDPPorts, served, rtspUDPWait); err != nil {
				// Serve has returned, or returns for this Close.
				_ = srv.Close()
				_ = ln.Close()
				if tlsLn != nil {
					_ = tlsLn.Close()
				}
				if !ended {
					<-served
				}
				return err
			}
		}
		wg.Go(func() {
			if err := serveErr(<-served); err != nil {
				s.fail(fmt.Errorf("server: rtsp: %w", err))
			}
		})
	}
	if tlsLn != nil {
		// After the plain Serve, which is the one that binds the UDP pair.
		cfg := s.store.ServerConfig()
		wg.Go(func() {
			if err := serveErr(srv.ServeTLS(tlsLn, cfg)); err != nil {
				s.fail(fmt.Errorf("server: rtsps: %w", err))
			}
		})
	}
	s.rtsp = srv
	s.stopRTSP = func() {
		_ = srv.Close()
		wg.Wait()
	}
	return nil
}

// waitUDP waits until ports reports the RTSP server's UDP pair bound. It
// fails when Serve returns first (ended is true; also when it returned nil,
// which is a server that stopped without a reason), and when the pair is
// not bound within wait.
func waitUDP(ports func() [2]int, served <-chan error, wait time.Duration) (ended bool, err error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for ports()[0] == 0 {
		select {
		case err := <-served:
			if err == nil {
				err = errors.New("the server stopped before it bound its UDP ports")
			}
			return true, err
		case <-deadline.C:
			return false, fmt.Errorf("the UDP ports were not bound within %s", wait)
		case <-tick.C:
		}
	}
	return false, nil
}

// serveErr is the error of an RTSP Serve that returned after start-up: nil
// for the return Close makes, an error for any other, also a nil one.
func serveErr(err error) error {
	switch {
	case errors.Is(err, rtsp.ErrServerClosed):
		return nil
	case err == nil:
		return errors.New("the server stopped serving")
	}
	return err
}

// startSRT serves SRT receivers on sec's address. It returns once the
// listener is bound.
func (s *Server) startSRT(sec config.Server) error {
	srv := srtout.New(nil, srtout.Options{
		App:     sec.SRT.App,
		Latency: time.Duration(sec.SRT.LatencyMS) * time.Millisecond,
		Secure:  s.secureSRT,
	})
	if err := srv.Listen(sec.SRT.Listen); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, srtout.ErrServerClosed) {
			s.fail(fmt.Errorf("server: srt: %w", err))
		}
	})
	s.srt = srv
	s.stopSRT = func() {
		_ = srv.Close()
		wg.Wait()
	}
	return nil
}

// fail reports a listener that failed after start-up; Run ends with the
// first one.
func (s *Server) fail(err error) {
	select {
	case s.failed <- err:
	default:
	}
}

// closeListeners closes what New bound, when it fails before any stream is
// loaded.
func (s *Server) closeListeners() {
	s.apiCancel()
	s.stopRTMP()
	s.stopRTSP()
	s.stopSRT()
	if s.apiLn != nil {
		_ = s.apiLn.Close()
	}
	if s.apiTLS != nil {
		_ = s.apiTLS.Close()
	}
	if s.mediaLn != nil {
		_ = s.mediaLn.Close()
	}
	if s.mediaTLS != nil {
		_ = s.mediaTLS.Close()
	}
}

// Addrs returns the listeners' addresses as bound.
func (s *Server) Addrs() Addrs { return s.addrs }

// Manager returns the manager of the streams.
func (s *Server) Manager() *stream.Manager { return s.manager.Load() }

// Run serves until ctx ends, then stops in order and returns nil, or the
// first listener error. It must be called once.
func (s *Server) Run(ctx context.Context) error {
	// The manager outlives ctx: it stops after the API did.
	mctx, stopManager := context.WithCancel(context.WithoutCancel(ctx))
	defer stopManager()
	var mwg, hwg sync.WaitGroup
	mwg.Go(func() { s.Manager().Run(mctx) })
	// The certificate's files are watched until the stop begins, which a
	// listener's failure starts too.
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if s.store != nil {
		s.certLoaded()
		hwg.Go(func() { s.store.Watch(wctx, s.certPoll, s.certReport) })
	}
	serve := func(name string, srv *http.Server, ln net.Listener) {
		hwg.Go(func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.fail(fmt.Errorf("server: %s: %w", name, err))
			}
		})
	}
	// The certificate is the TLSConfig's, so ServeTLS is given no files.
	serveTLS := func(name string, srv *http.Server, ln *trackListener) {
		if ln == nil {
			return
		}
		hwg.Go(func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.fail(fmt.Errorf("server: %s: %w", name, err))
			}
		})
	}
	serve("api", s.apiSrv, s.apiLn)
	serveTLS("api-tls", s.apiSrv, s.apiTLS)
	serve("http", s.mediaSrv, s.mediaLn)
	serveTLS("https", s.mediaSrv, s.mediaTLS)

	var err error
	select {
	case <-ctx.Done():
	case err = <-s.failed:
	}

	// No new edit comes in; then the streams stop, which closes every hub
	// and so ends every player; then the output servers, which wait for
	// their sessions; the media server last. Each stop covers both of its
	// server's listeners. A TLS handshake that fails from here on is the
	// stop's doing.
	s.stopping.Store(true)
	stopWatch()
	shutdown(ctx, s.apiSrv, s.apiTLS)
	s.apiCancel() // a request still waiting for a rebuild
	stopManager()
	mwg.Wait()
	s.stopRTMP()
	s.stopRTSP()
	s.stopSRT()
	shutdown(ctx, s.mediaSrv, s.mediaTLS)
	hwg.Wait()
	return err
}

// shutdown lets srv finish its requests for shutdownWait, then closes it.
// ctx has ended already; only its values are kept.
//
// tlsLn is srv's HTTPS listener, nil when off. net/http closes a TLS
// connection with a close_notify, which a peer that reads nothing holds up
// for seconds, in Close and in Shutdown's own close of an idle connection
// alike, and so does a connection that closes itself. So the raw connections
// still open are closed here, when Shutdown returns and at shutdownWait
// whatever Shutdown is doing then, and the stop takes shutdownWait at most
// (R-S8).
func shutdown(ctx context.Context, srv *http.Server, tlsLn *trackListener) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
	defer cancel()
	if tlsLn != nil {
		// For a Shutdown that is itself held up by a close_notify.
		defer time.AfterFunc(shutdownWait, tlsLn.closeConns).Stop()
	}
	err := srv.Shutdown(ctx)
	// Whatever Shutdown returned: a connection that was closing itself as
	// Shutdown began is no longer Shutdown's to wait for, so nil does not
	// mean that every raw connection is closed.
	if tlsLn != nil {
		tlsLn.closeConns()
	}
	if err != nil {
		_ = srv.Close()
	}
}
