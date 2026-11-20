// Command osdev serves input URLs as live streams for development: each
// -stream name=URL is read into a hub and packaged as HLS, CMAF and DASH, and
// also served as one continuous MPEG-TS response. A URL is any input of the
// ingest package (a bare path is a file, replayed in real time); a name given
// again adds a lower-priority input for failover, and copy://name reads
// another osdev stream. -layout sets the track layout of every stream.
//
// Without -loop a stream ends with its file: its hub closes, the last partial
// segment is packaged and the HLS playlists end with EXT-X-ENDLIST, while
// the server keeps serving. A stream whose source fails is reported on
// stderr; osdev then exits 1 at shutdown, or at once when every stream has
// failed. Every 30 s, and at shutdown, osdev reports on stderr each stream's
// active input, switches, drift breaks and reader counters when they changed,
// and the packager's mux errors and fallback anchors while they grow.
//
// To accept an RTMP publisher (OBS, ffmpeg), serve a listen input and point
// the publisher at it, with a stream key of your choice:
//
//	osdev -stream cam=rtmp://:1935/live/<stream-key>?mode=listen
//	ffmpeg -re -i in.mp4 -c copy -f flv rtmp://<host>:1935/live/<stream-key>
//
// In OBS set the server to rtmp://<host>:1935/live and the stream key to
// <stream-key>. Several listen streams on one port share its listener.
//
// Set -layout explicitly to the tracks your publishers send, such as
// -layout audio for an audio-only encoder: the layout (video,audio by
// default) is fixed for the life of the stream, whatever a publisher sends,
// so a publisher's tracks outside it are dropped and layout tracks it lacks
// stay empty. Use a long random stream key: until authentication
// arrives, the key in the path is the only thing that keeps others from
// publishing to the stream.
//
// To serve every stream to RTMP players, give -rtmp-play an address (it may
// be the port of RTMP listen inputs, whose listener it then shares); a
// player plays rtmp://<host>:<port>/<app>/<name>, where app is
// -rtmp-play-app (default live) and name is the stream's -stream name:
//
//	osdev -stream cam=rtmp://:1935/live/<stream-key>?mode=listen -rtmp-play :1935
//	ffplay rtmp://<host>:1935/live/cam
//
// A publish path is never playable, so a stream whose listen path is its
// own play path (<app>/<name>) cannot be played over RTMP; osdev warns about
// it at startup. Publish under a long random key instead.
//
// To push a stream to an RTMP server (a CDN ingest, a restreaming service),
// give -rtmp-push name=URL, once per target; the URL's query goes to the
// server as is. A target that fails is retried with a backoff of 1 s to
// 30 s. The URL is a secret: osdev shows it only redacted, the stream key
// cut to its first 2 characters:
//
//	osdev -stream cam=in.ts -loop -rtmp-push cam=rtmp://a.example.com/live2/<stream-key>
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmpplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

const (
	subQueue       = 1024
	reportInterval = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usageText = `usage: osdev -stream name=URL [-stream ...] [-listen addr] [-layout video,audio] [-loop]
             [-rtmp-play addr [-rtmp-play-app live]] [-rtmp-push name=rtmp://host/app/key ...]

Accept an RTMP publisher (OBS or ffmpeg):
  osdev -stream cam=rtmp://:1935/live/<stream-key>?mode=listen
  ffmpeg -re -i in.mp4 -c copy -f flv rtmp://<host>:1935/live/<stream-key>
  OBS: server rtmp://<host>:1935/live, stream key <stream-key>
  Set -layout to the tracks the publisher sends (e.g. -layout audio), and
  use a long random stream key: it is the only gate on who may publish.

Pull from an IP camera or an RTSP server (TCP by default; UDP and auto
with ?transport=udp or ?transport=auto; the port defaults to 554):
  osdev -stream cam=rtsp://user:pass@<host>/<path>

Serve every stream to RTMP players (ffplay, VLC, ffmpeg) with -rtmp-play;
it may share the port of RTMP listen inputs. The play URL is
rtmp://<host>:<port>/<app>/<name>, app from -rtmp-play-app (default live):
  osdev -stream cam=rtmp://:1935/live/<stream-key>?mode=listen -rtmp-play :1935
  ffplay rtmp://<host>:1935/live/<name>
  A publish path is never playable: a stream published at <app>/<name>
  cannot be played (osdev warns), so publish under a long random key.

Push a stream to an RTMP server (repeat -rtmp-push for more targets; the
URL is shown only redacted, and a failing target is retried):
  osdev -stream cam=in.ts -loop -rtmp-push cam=rtmp://<host>/<app>/<stream-key>
`

// streamFlags collects repeated -stream name=path values.
type streamFlags []string

func (s *streamFlags) String() string     { return strings.Join(*s, ",") }
func (s *streamFlags) Set(v string) error { *s = append(*s, v); return nil }

type source struct {
	name     string
	inputs   []string // input URLs by priority, first highest
	hub      *hub.Hub
	pack     *live.Packager
	pipe     *pipeline.Pipeline
	reported [3]uint64 // Errors, CodecErrors and FallbackAnchors at the last report
	lastLine string    // the last pipeline report line
}

// reportPipeline writes the stream's input state when it differs from the
// last report: the active input (redacted), switches, unmapped frames, drift
// breaks and the active reader's counters.
func (s *source) reportPipeline(w io.Writer, st pipeline.Stats) {
	active := "none"
	if st.Active >= 0 && st.Active < len(s.inputs) {
		active = fmt.Sprintf("%d %s", st.Active, ingest.Redact(s.inputs[st.Active]))
	}
	line := fmt.Sprintf("osdev: stream %s: input %s, switches %d, unmapped %d, drift breaks %d",
		s.name, active, st.Switches, st.Unmapped, st.Clock.DriftBreaks)
	keys := make([]string, 0, len(st.Input))
	for k := range st.Input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line += fmt.Sprintf(" %s=%d", k, st.Input[k])
	}
	if line == s.lastLine {
		return
	}
	s.lastLine = line
	_, _ = fmt.Fprintln(w, line)
}

// lockedWriter serialises writes from several goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// report writes each stream's pipeline line when it changed, then one line
// per stream whose packager counted mux errors,
// codec string errors or fallback anchors: on every call when final, else
// only when they grew since the last report.
func report(w io.Writer, srcs []*source, final bool) {
	for _, s := range srcs {
		if s.pipe != nil {
			s.reportPipeline(w, s.pipe.Stats())
		}
		now := [3]uint64{s.pack.Errors(), s.pack.CodecErrors(), s.pack.FallbackAnchors()}
		if now == ([3]uint64{}) || (!final && now == s.reported) {
			continue
		}
		s.reported = now
		_, _ = fmt.Fprintf(w, "osdev: stream %s: %d packager mux errors (dropped segment outputs), %d tracks without a codec string, %d epochs placed without a hub anchor\n",
			s.name, now[0], now[1], now[2])
	}
}

// newServer returns the HTTP server. It sets no ReadTimeout or WriteTimeout:
// either would cut the long-lived HTTP-TS responses, and each handler sets
// its own per-write deadline instead (live and httpts, 10 s). The header,
// idle and header-size limits keep idle or slow clients from holding
// connections.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// push is one -rtmp-push: a stream and its pusher.
type push struct {
	stream   string
	pusher   *rtmppush.Pusher
	lastLine string // the last report line
}

// report writes the push's state when it differs from the last report: the
// target (redacted), state, counters and last error.
func (p *push) report(w io.Writer) {
	st := p.pusher.Stats()
	line := fmt.Sprintf("osdev: stream %s: push %s: %s, connects %d, failures %d, republishes %d, resyncs %d, messages %d",
		p.stream, p.pusher, st.State, st.Connects, st.Failures, st.Republishes, st.Resyncs, st.Messages)
	if st.LastError != "" {
		line += ", last error: " + st.LastError
	}
	if line == p.lastLine {
		return
	}
	p.lastLine = line
	_, _ = fmt.Fprintln(w, line)
}

// config is the parsed command line.
type config struct {
	listen  string
	sources []*source // in order of first appearance
	layout  []media.Kind
	// rtmpPlay is the RTMP play address, empty when RTMP play is off;
	// rtmpPlayApp the play app, normalised.
	rtmpPlay, rtmpPlayApp string
	pushes                []*push // in flag order
}

// parseLayout parses a comma-separated list of video and audio.
func parseLayout(v string) ([]media.Kind, error) {
	var out []media.Kind
	for _, f := range strings.Split(v, ",") {
		switch strings.TrimSpace(f) {
		case "video":
			out = append(out, media.KindVideo)
		case "audio":
			out = append(out, media.KindAudio)
		default:
			return nil, fmt.Errorf("unknown kind %q, want video or audio", f)
		}
	}
	return out, nil
}

// withLoop adds loop=1 to a file input: a bare path becomes a file URL.
func withLoop(input string) (string, error) {
	if !strings.Contains(input, "://") {
		abs, err := filepath.Abs(input)
		if err != nil {
			return "", err
		}
		return (&url.URL{Scheme: "file", Path: abs, RawQuery: "loop=1"}).String(), nil
	}
	u, err := url.Parse(input)
	if err != nil || !strings.EqualFold(u.Scheme, "file") || u.Query().Has("loop") {
		return input, nil //nolint:nilerr // ingest.Parse reports a malformed URL
	}
	if u.RawQuery != "" {
		u.RawQuery = "loop=1&" + u.RawQuery
	} else {
		u.RawQuery = "loop=1"
	}
	return u.String(), nil
}

// parseArgs parses the command line. On failure it reports on stderr and
// returns a nil config and the exit code.
func parseArgs(args []string, stderr io.Writer) (*config, int) {
	fs := flag.NewFlagSet("osdev", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", ":8080", "address to listen on")
	loop := fs.Bool("loop", false, "replay each file at its end")
	layoutFlag := fs.String("layout", "video,audio", "track layout of every stream: video and audio, comma-separated")
	rtmpPlay := fs.String("rtmp-play", "", "serve every stream to RTMP players on this address (host:port)")
	rtmpPlayApp := fs.String("rtmp-play-app", "live", "the app of RTMP play URLs: rtmp://host:port/<app>/<name>")
	var streams, pushes streamFlags
	fs.Var(&streams, "stream", "name=URL (or name=path.ts) to serve; repeat a name to add a lower-priority input")
	fs.Var(&pushes, "rtmp-push", "name=rtmp://host/app/key: push stream name to an RTMP server; repeatable")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "osdev: %v\n", err)
		return nil, 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "osdev: unexpected argument %q\n", ingest.Redact(fs.Arg(0)))
		return nil, 2
	}
	layout, err := parseLayout(*layoutFlag)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "osdev: -layout: %v\n", err)
		return nil, 2
	}
	if len(streams) == 0 {
		_, _ = fmt.Fprintln(stderr, "osdev: at least one -stream name=URL is required")
		_, _ = fmt.Fprint(stderr, usageText)
		return nil, 2
	}
	app, _ := rtmp.NormalizePath(*rtmpPlayApp, "")
	if *rtmpPlay != "" {
		if _, err := rtmpsrv.AddrKey(*rtmpPlay); err != nil {
			_, _ = fmt.Fprintf(stderr, "osdev: -rtmp-play: %v\n", err)
			return nil, 2
		}
		if app == "" {
			_, _ = fmt.Fprintln(stderr, "osdev: -rtmp-play-app: empty app")
			return nil, 2
		}
	}
	cfg := &config{listen: *listen, layout: layout, rtmpPlay: *rtmpPlay, rtmpPlayApp: app}
	byName := map[string]*source{}
	for _, s := range streams {
		name, input, ok := strings.Cut(s, "=")
		if !ok || input == "" || !validName(name) {
			_, _ = fmt.Fprintf(stderr, "osdev: malformed -stream %q, want name=URL\n", ingest.Redact(s))
			return nil, 2
		}
		if *loop {
			var err error
			if input, err = withLoop(input); err != nil {
				_, _ = fmt.Fprintf(stderr, "osdev: stream %s: %v\n", name, err)
				return nil, 2
			}
		}
		if _, err := ingest.Parse(input); err != nil {
			_, _ = fmt.Fprintf(stderr, "osdev: stream %s: %v\n", name, err)
			return nil, 2
		}
		src := byName[name]
		if src == nil {
			h := hub.New(hub.Options{})
			src = &source{name: name, hub: h, pack: live.NewPackager(h, live.Options{})}
			byName[name] = src
			cfg.sources = append(cfg.sources, src)
		}
		src.inputs = append(src.inputs, input)
	}
	if err := checkCopyCycles(cfg.sources); err != nil {
		_, _ = fmt.Fprintf(stderr, "osdev: %v\n", err)
		return nil, 2
	}
	for _, v := range pushes {
		// The value holds a stream key: it is never echoed.
		name, target, ok := strings.Cut(v, "=")
		if !ok || target == "" || !validName(name) {
			_, _ = fmt.Fprintln(stderr, "osdev: malformed -rtmp-push, want name=rtmp://host/app/key")
			return nil, 2
		}
		src := byName[name]
		if src == nil {
			_, _ = fmt.Fprintf(stderr, "osdev: -rtmp-push: unknown stream %s\n", name)
			return nil, 2
		}
		p, err := rtmppush.New(src.hub, target, rtmppush.Options{})
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "osdev: -rtmp-push for stream %s: %v\n", name, err)
			return nil, 2
		}
		cfg.pushes = append(cfg.pushes, &push{stream: name, pusher: p})
	}
	return cfg, 0
}

// validName reports whether name can name a stream.
func validName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/?#% ") && name != "." && name != ".."
}

// copyTarget returns the stream a copy:// input reads.
func copyTarget(input string) (string, bool) {
	u, err := url.Parse(input)
	if err != nil || !strings.EqualFold(u.Scheme, "copy") {
		return "", false
	}
	return u.Host, true
}

// checkCopyCycles rejects a copy:// input that reads its own stream, directly
// or through other streams: such a stream would re-ingest its own output.
func checkCopyCycles(srcs []*source) error {
	byName := map[string]*source{}
	for _, s := range srcs {
		byName[s.name] = s
	}
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(s *source, path []string) error
	visit = func(s *source, path []string) error {
		state[s.name] = visiting
		path = append(path, s.name)
		for _, in := range s.inputs {
			t, ok := copyTarget(in)
			if !ok || byName[t] == nil {
				continue
			}
			switch state[t] {
			case visiting:
				if t == s.name {
					return fmt.Errorf("stream %s: copy://%s reads its own stream", s.name, t)
				}
				return fmt.Errorf("copy:// cycle between streams: %s -> %s", strings.Join(path, " -> "), t)
			case 0:
				if err := visit(byName[t], path); err != nil {
					return err
				}
			}
		}
		state[s.name] = done
		return nil
	}
	for _, s := range srcs {
		if state[s.name] == 0 {
			if err := visit(s, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// rtmpListenKey returns the listener key and the publish path of an RTMP
// listen input; ok is false for any other input.
func rtmpListenKey(input string) (addr, path string, ok bool) {
	u, err := url.Parse(input)
	if err != nil || !strings.EqualFold(u.Scheme, "rtmp") {
		return "", "", false
	}
	if mode := u.Query().Get("mode"); mode != "listen" && mode != "listener" {
		return "", "", false
	}
	port := u.Port()
	if port == "" {
		port = "1935"
	}
	addr, err = rtmpsrv.AddrKey(net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", "", false
	}
	path, _ = rtmp.NormalizePath(u.Path, "")
	return addr, path, true
}

// warnPublishPaths reports each stream whose RTMP play path is a publish
// path of an RTMP listen input on the play listener: the registry never
// plays a publish path, so that stream cannot be played over RTMP. The
// publish paths are not printed; the play path is public.
func warnPublishPaths(w io.Writer, cfg *config) {
	if cfg.rtmpPlay == "" {
		return
	}
	playAddr, err := rtmpsrv.AddrKey(cfg.rtmpPlay)
	if err != nil {
		return
	}
	publish := map[string]bool{}
	for _, s := range cfg.sources {
		for _, in := range s.inputs {
			if addr, path, ok := rtmpListenKey(in); ok && addr == playAddr {
				publish[path] = true
			}
		}
	}
	for _, s := range cfg.sources {
		if path, _ := rtmp.NormalizePath(cfg.rtmpPlayApp, s.name); publish[path] {
			_, _ = fmt.Fprintf(w, "osdev: warning: stream %s cannot be played over RTMP: its play path %s is a publish path on %s; publish under another key\n",
				s.name, path, playAddr)
		}
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	stderr = &lockedWriter{w: stderr}
	cfg, code := parseArgs(args, stderr)
	if cfg == nil {
		return code
	}
	warnPublishPaths(stderr, cfg)
	order := cfg.sources
	srcs := map[string]*source{}
	for _, s := range order {
		srcs[s.name] = s
	}
	hubLookup := func(n string) (*hub.Hub, bool) {
		if s := srcs[n]; s != nil {
			return s.hub, true
		}
		return nil, false
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "osdev: %v\n", err)
		return 2
	}
	unregisterPlay := func() {}
	if cfg.rtmpPlay != "" {
		players := rtmpplay.New(hubLookup, rtmpplay.Options{})
		//nolint:contextcheck // the shared listener lives until unregisterPlay, not with ctx
		unregisterPlay, err = rtmpsrv.Default.RegisterPlay(cfg.rtmpPlay, cfg.rtmpPlayApp, players, rtmp.Config{}, rtmpsrv.Policy{})
		if err != nil {
			_ = ln.Close()
			_, _ = fmt.Fprintf(stderr, "osdev: -rtmp-play: %v\n", err)
			return 2
		}
	}
	defer unregisterPlay() // idempotent; shutdown calls it first
	addr := ln.Addr().String()
	_, _ = fmt.Fprintf(stdout, "listening on %s\n", addr)
	base := "http://" + addr
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.IP.IsUnspecified() {
		base = fmt.Sprintf("http://127.0.0.1:%d", tcp.Port)
	}
	for _, s := range order {
		for _, p := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd", "stream.ts"} {
			_, _ = fmt.Fprintf(stdout, "%s/live/%s/%s\n", base, s.name, p)
		}
	}
	if cfg.rtmpPlay != "" {
		host, port, _ := net.SplitHostPort(cfg.rtmpPlay)
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			host = "127.0.0.1"
		}
		for _, s := range order {
			_, _ = fmt.Fprintf(stdout, "rtmp://%s/%s/%s\n", net.JoinHostPort(host, port), cfg.rtmpPlayApp, s.name)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg     sync.WaitGroup
		failed atomic.Int32
	)
	for _, s := range order {
		sub := s.hub.Subscribe(s.name, subQueue)
		// A stream of one file input stops at its failure; any other input
		// is retried, and its failures are reported.
		spec, _ := ingest.Parse(s.inputs[0])
		stopOnFail := len(s.inputs) == 1 && !spec.Live && !spec.Passive
		sctx, scancel := context.WithCancel(ctx)
		var failure error // set from the Run goroutine, read after Run
		s.pipe = pipeline.New(s.hub, pipeline.Options{
			Inputs:   s.inputs,
			Env:      ingest.Env{Lookup: hubLookup},
			Layout:   cfg.layout,
			EndOnEOF: true,
			OnEvent: func(ev pipeline.Event) {
				if ev.Kind != pipeline.EventFailed {
					return
				}
				if stopOnFail {
					if failure == nil {
						failure = ev.Err
						scancel()
					}
					return
				}
				_, _ = fmt.Fprintf(stderr, "osdev: stream %s: input %d %s failed: %v\n",
					s.name, ev.Input, ingest.Redact(s.inputs[ev.Input]), ev.Err)
			},
		})
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.pack.Run(ctx, sub)
		}()
		go func() {
			defer wg.Done()
			defer scancel()
			err := s.pipe.Run(sctx)
			if ctx.Err() != nil {
				return // shutting down: run closes the hubs
			}
			if failure != nil {
				err = failure
			}
			// The stream is over: closing the hub ends it in the packager.
			s.hub.Close()
			if err == nil {
				_, _ = fmt.Fprintf(stderr, "osdev: stream %s: source ended\n", s.name)
				return
			}
			_, _ = fmt.Fprintf(stderr, "osdev: stream %s stopped: %v\n", s.name, err)
			if int(failed.Add(1)) == len(order) {
				_, _ = fmt.Fprintln(stderr, "osdev: every stream failed")
				cancel()
			}
		}()
	}

	for _, p := range cfg.pushes {
		_, _ = fmt.Fprintf(stderr, "osdev: stream %s: pushing to %s\n", p.stream, p.pusher)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.pusher.Run(ctx) == nil {
				_, _ = fmt.Fprintf(stderr, "osdev: stream %s: push to %s ended with the stream\n", p.stream, p.pusher)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(reportInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				report(stderr, order, false)
				for _, p := range cfg.pushes {
					p.report(stderr)
				}
			}
		}
	}()
	packLookup := func(n string) (*live.Packager, bool) {
		if s := srcs[n]; s != nil {
			return s.pack, true
		}
		return nil, false
	}
	mux := http.NewServeMux()
	mux.Handle("GET /live/{stream}/stream.ts", httpts.Handler(hubLookup, 0))
	mux.Handle("/live/", live.Handler(packLookup))
	srv := newServer(mux)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	code = 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		_, _ = fmt.Fprintf(stderr, "osdev: %v\n", err)
		code = 2
	}
	cancel()
	for _, s := range order {
		s.hub.Close()
	}
	unregisterPlay() // the players got end of stream; this waits for them
	sd, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer done()
	if err := srv.Shutdown(sd); err != nil {
		_ = srv.Close()
	}
	wg.Wait()
	report(stderr, order, true)
	for _, p := range cfg.pushes {
		p.report(stderr)
	}
	if code == 0 && failed.Load() > 0 {
		code = 1
	}
	return code
}
