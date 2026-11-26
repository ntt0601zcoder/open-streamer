// Command openstreamer is the server: it reads one config file, serves its
// streams and the API, and reloads the file on SIGHUP or POST
// /api/v1/reload.
//
//	openstreamer -config /etc/openstreamer/openstreamer.yaml
//	openstreamer -version
//	openstreamer hash-password < password.txt
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/server"
)

// version is set by the build: -ldflags "-X main.version=…".
var version = "dev"

const usageText = `usage: openstreamer -config FILE     run the server
       openstreamer -version         print the version
       openstreamer hash-password    read a password from stdin, print its hash
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Registered before the server starts, and never stopped: a SIGHUP
	// during start-up or shutdown must not end the process.
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, notifyHUP())
	stop()
	os.Exit(code)
}

// run is the command. It returns the exit code: 0 after a clean stop, 1
// when the server ended on an error, 2 for a usage, config or start-up
// error.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, hup <-chan os.Signal) int {
	if len(args) > 0 && args[0] == "hash-password" {
		return hashPassword(args[1:], stdin, stdout, stderr)
	}
	fs := flag.NewFlagSet("openstreamer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, usageText) }
	path := fs.String("config", "", "the config file")
	showVersion := fs.Bool("version", false, "print the version")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch {
	case fs.NArg() > 0:
		// The arguments are not echoed: one may be a password meant for
		// hash-password.
		_, _ = io.WriteString(stderr, "openstreamer: unexpected arguments\n"+usageText)
		return 2
	case *showVersion:
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case *path == "":
		_, _ = io.WriteString(stderr, usageText)
		return 2
	}

	//nolint:contextcheck // the listeners live until Run returns, not with ctx
	srv, err := server.New(*path, server.Options{Version: version, LogOut: stderr})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "openstreamer: %s\n", logging.Scrub(err.Error()))
		return 2
	}
	a := srv.Addrs()
	for _, l := range []struct{ name, addr string }{
		{"http", a.HTTP}, {"api", a.API}, {"rtmp", a.RTMP}, {"rtsp", a.RTSP}, {"srt", a.SRT},
	} {
		if l.addr != "" {
			_, _ = fmt.Fprintf(stdout, "%s %s\n", l.name, l.addr)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() {
		// Reload logs its own outcome, and keeps it for the status.
		hupLoop(ctx, hup, func(ctx context.Context) { _, _ = srv.Reload(ctx) })
	})
	err = srv.Run(ctx)
	cancel()
	wg.Wait()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "openstreamer: %s\n", logging.Scrub(err.Error()))
		return 1
	}
	return 0
}

// hupLoop calls reload once for each value of hup, one at a time, until ctx
// ends. hup has a buffer of one, so the signals that arrive during a reload
// coalesce into one pending reload and none is lost. A nil hup (a system
// without SIGHUP) only waits for ctx.
func hupLoop(ctx context.Context, hup <-chan os.Signal, reload func(context.Context)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			// select picks at random when both are ready: a signal that
			// was pending at shutdown must not start a reload.
			if ctx.Err() != nil {
				return
			}
			reload(ctx)
		}
	}
}
