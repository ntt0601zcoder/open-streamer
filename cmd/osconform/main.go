// Command osconform reads an HLS playlist, a DASH MPD or an MPEG-TS stream
// over HTTP, an RTMP stream (rtmp:// URL, played), or an RTSP stream (rtsp:// URL, played; ?transport=udp selects UDP), or an SRT stream (srt://host:port?streamid=...&latency=ms, called), and prints the conformance violations it finds, one per line.
//
// With -aligned, the media playlists of an HLS master must number their
// segments alike (equal MEDIA-SEQUENCE and discontinuity sequence numbers,
// matching gap durations), which also turns on the ABR rules that need it:
// variants aligned and BANDWIDTH honest. Our servers promise that; RFC 8216
// does not require it, so it is off by default.
//
// With -audio-language, which applies to HLS and DASH only, the HLS master
// or the MPD must carry that language on its audio (a media playlist has no
// master to carry it). The languages read are listed on stderr either way.
//
// Exit status: 0 clean, 1 violations found, 2 read error (an MPEG-TS stream
// that ends before its first byte is one) or bad arguments.
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
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("osconform", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	u := fs.String("url", "", "URL of a .m3u8, .mpd or .ts output, an rtmp:// stream, an rtsp:// stream, or an srt:// stream (streamid and latency in the query)")
	dur := fs.Duration("duration", 30*time.Second, "media time to capture")
	once := fs.Bool("once", false, "read the playlist or MPD once instead of polling")
	timeout := fs.Duration("timeout", 30*time.Second, "limit on each request, and on a stall of the .ts, RTMP, RTSP or SRT stream")
	aligned := fs.Bool("aligned", false, "HLS: require the master's media playlists to number their segments alike, and check that variants are aligned and BANDWIDTH is honest")
	mtu := fs.Int("mtu", 0, "largest RTP payload accepted from an RTSP stream (0: 1200)")
	lang := fs.String("audio-language", "", "HLS and DASH only (.m3u8, .mpd): the BCP 47 language the master's audio renditions or the MPD's audio sets must carry")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "osconform: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "osconform: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *lang != "" && !media.ValidLanguageSyntax(*lang) {
		_, _ = fmt.Fprintf(stderr, "osconform: -audio-language %q is not a BCP 47 tag\n", *lang)
		return 2
	}
	pu, err := url.Parse(*u)
	if err != nil || *u == "" {
		_, _ = fmt.Fprintln(stderr, "osconform: -url is required and must be a valid URL")
		return 2
	}
	opt := conform.ReadOptions{Duration: *dur, Once: *once, Timeout: *timeout, MTU: *mtu, AlignedSequences: *aligned, AudioLanguage: *lang}
	var read func(context.Context, string, conform.ReadOptions) (conform.Capture, error)
	langOK := false
	switch p := strings.ToLower(pu.Path); {
	case strings.EqualFold(pu.Scheme, "rtmp"):
		read = conform.ReadRTMP
	case strings.EqualFold(pu.Scheme, "rtsp"):
		read = conform.ReadRTSP
	case strings.EqualFold(pu.Scheme, "srt"):
		read = conform.ReadTS
	case strings.HasSuffix(p, ".m3u8"):
		read, langOK = conform.ReadHLS, true
	case strings.HasSuffix(p, ".mpd"):
		read, langOK = conform.ReadDASH, true
	case strings.HasSuffix(p, ".ts"):
		read = conform.ReadTS
	default:
		_, _ = fmt.Fprintln(stderr, "osconform: -url must end in .m3u8, .mpd or .ts, or be an rtmp://, rtsp:// or srt:// URL")
		return 2
	}
	if *lang != "" && !langOK {
		_, _ = fmt.Fprintln(stderr, "osconform: -audio-language applies to HLS and DASH only")
		return 2
	}
	c, err := read(ctx, *u, opt)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "osconform: %v\n", err)
		return 2
	}
	if len(c.AudioLanguages) > 0 {
		_, _ = fmt.Fprintf(stderr, "osconform: audio languages: %s\n", strings.Join(c.AudioLanguages, ", "))
	}
	for _, v := range c.Violations {
		_, _ = fmt.Fprintln(stdout, v.String())
	}
	if c.RTPLost > 0 {
		_, _ = fmt.Fprintf(stderr, "osconform: %d RTP packets lost\n", c.RTPLost)
	}
	if len(c.Violations) > 0 {
		return 1
	}
	return 0
}
