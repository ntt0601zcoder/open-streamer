// Command osdev serves MPEG-TS files as live streams for development: each
// -stream is replayed in real time into a hub and packaged as HLS, CMAF and
// DASH, and also served as one continuous MPEG-TS response.
//
// Without -loop a stream ends with its file: its hub closes, the last partial
// segment is packaged and the HLS playlists end with EXT-X-ENDLIST, while
// the server keeps serving. A stream whose source fails is reported on
// stderr; osdev then exits 1 at shutdown, or at once when every stream has
// failed. Packager mux errors and fallback anchors are reported on stderr
// every 30 s while they grow, and once more at shutdown.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
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

// streamFlags collects repeated -stream name=path values.
type streamFlags []string

func (s *streamFlags) String() string     { return strings.Join(*s, ",") }
func (s *streamFlags) Set(v string) error { *s = append(*s, v); return nil }

type source struct {
	name, path string
	hub        *hub.Hub
	pack       *live.Packager
	reported   [3]uint64 // Errors, CodecErrors and FallbackAnchors at the last report
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

// report writes one line per stream whose packager counted mux errors,
// codec string errors or fallback anchors: on every call when final, else
// only when they grew since the last report.
func report(w io.Writer, srcs []*source, final bool) {
	for _, s := range srcs {
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	stderr = &lockedWriter{w: stderr}
	fs := flag.NewFlagSet("osdev", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", ":8080", "address to listen on")
	loop := fs.Bool("loop", false, "replay each file at its end")
	var streams streamFlags
	fs.Var(&streams, "stream", "name=path.ts to serve; repeatable")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "osdev: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "osdev: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if len(streams) == 0 {
		fmt.Fprintln(stderr, "osdev: at least one -stream name=path.ts is required")
		return 2
	}
	srcs := map[string]*source{}
	var order []*source
	for _, s := range streams {
		name, path, ok := strings.Cut(s, "=")
		if !ok || name == "" || path == "" || strings.ContainsAny(name, "/?#% ") || name == "." || name == ".." {
			fmt.Fprintf(stderr, "osdev: malformed -stream %q, want name=path.ts\n", s)
			return 2
		}
		if srcs[name] != nil {
			fmt.Fprintf(stderr, "osdev: duplicate stream %q\n", name)
			return 2
		}
		h := hub.New(hub.Options{})
		src := &source{name: name, path: path, hub: h, pack: live.NewPackager(h, live.Options{})}
		srcs[name] = src
		order = append(order, src)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "osdev: %v\n", err)
		return 2
	}
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg     sync.WaitGroup
		failed atomic.Int32
	)
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
			}
		}
	}()
	for _, s := range order {
		sub := s.hub.Subscribe(s.name, subQueue)
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.pack.Run(ctx, sub)
		}()
		go func() {
			defer wg.Done()
			opt := pipeline.Options{
				Open: func() (io.ReadCloser, error) { return os.Open(s.path) },
				Loop: *loop,
				Pace: true,
			}
			err := pipeline.Run(ctx, s.hub, opt)
			if ctx.Err() != nil {
				return // shutting down: run closes the hubs
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

	packLookup := func(n string) (*live.Packager, bool) {
		if s := srcs[n]; s != nil {
			return s.pack, true
		}
		return nil, false
	}
	hubLookup := func(n string) (*hub.Hub, bool) {
		if s := srcs[n]; s != nil {
			return s.hub, true
		}
		return nil, false
	}
	mux := http.NewServeMux()
	mux.Handle("GET /live/{stream}/stream.ts", httpts.Handler(hubLookup, 0))
	mux.Handle("/live/", live.Handler(packLookup))
	srv := newServer(mux)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	code := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		fmt.Fprintf(stderr, "osdev: %v\n", err)
		code = 2
	}
	cancel()
	for _, s := range order {
		s.hub.Close()
	}
	sd, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer done()
	if err := srv.Shutdown(sd); err != nil {
		_ = srv.Close()
	}
	wg.Wait()
	report(stderr, order, true)
	if code == 0 && failed.Load() > 0 {
		code = 1
	}
	return code
}
