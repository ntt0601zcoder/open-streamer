// Command osconform reads an HLS playlist, a DASH MPD or an MPEG-TS stream
// over HTTP and prints the conformance violations it finds, one per line.
//
// Exit status: 0 clean, 1 violations found, 2 read error or bad arguments.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("osconform", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	u := fs.String("url", "", "URL of a .m3u8, .mpd or .ts output")
	dur := fs.Duration("duration", 30*time.Second, "media time to capture")
	once := fs.Bool("once", false, "read the playlist or MPD once instead of polling")
	timeout := fs.Duration("timeout", 30*time.Second, "limit on each request, and on a stall of the .ts stream")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "osconform: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "osconform: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	pu, err := url.Parse(*u)
	if err != nil || *u == "" {
		fmt.Fprintln(os.Stderr, "osconform: -url is required and must be a valid URL")
		return 2
	}
	opt := conform.ReadOptions{Duration: *dur, Once: *once, Timeout: *timeout}
	var read func(context.Context, string, conform.ReadOptions) (conform.Capture, error)
	switch p := strings.ToLower(pu.Path); {
	case strings.HasSuffix(p, ".m3u8"):
		read = conform.ReadHLS
	case strings.HasSuffix(p, ".mpd"):
		read = conform.ReadDASH
	case strings.HasSuffix(p, ".ts"):
		read = conform.ReadTS
	default:
		fmt.Fprintln(os.Stderr, "osconform: -url must end in .m3u8, .mpd or .ts")
		return 2
	}
	c, err := read(ctx, *u, opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "osconform: %v\n", err)
		return 2
	}
	for _, v := range c.Violations {
		_, _ = fmt.Fprintln(stdout, v.String())
	}
	if len(c.Violations) > 0 {
		return 1
	}
	return 0
}
