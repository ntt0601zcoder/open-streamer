package stream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/certs"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// Output names one output of a stream.
type Output string

// The outputs a stream can be served on.
const (
	OutHLS    Output = "hls"
	OutDASH   Output = "dash"
	OutHTTPTS Output = "httpts"
	OutRTMP   Output = "rtmp"
	OutRTSP   Output = "rtsp"
	OutSRT    Output = "srt"
)

// Env is what a supervisor needs from the server.
type Env struct {
	Lookup     func(name string) (*hub.Hub, bool) // copy:// inputs; the Manager sets it
	Transcoder string                             // path of ostranscoder; "" means none
	AssetsDir  string                             // "" means no assets
	// RTMPPlay is the server's RTMP play address, as bound; "" means off.
	// RTMPApp is the play app of both RTMP listeners, set whenever either is
	// on, for Check's publish-path rule.
	RTMPPlay, RTMPApp string
	// RTSPPlay and SRTPlay are the addresses of the server's RTSP and SRT
	// servers, as bound; "" means off. Check refuses a stream that turns on
	// an output whose servers are all off (RTMP and RTSP have two each, plain
	// and TLS): nobody could play it.
	RTSPPlay, SRTPlay string
	// RTMPSPlay and RTSPSPlay are the server's RTMPS and RTSPS addresses, as
	// bound; "" means off. An rtmps listen input must be on RTMPSPlay.
	RTMPSPlay, RTSPSPlay string
	// RootCAs are the roots of every TLS client of the streams (rtmps and
	// rtsps inputs, https inputs, rtmps pushes); nil means the system's.
	// Tests only; cmd/openstreamer never sets it.
	RootCAs *x509.CertPool
	// Reserved are the server's own listeners as bound (HTTP, API, RTMP,
	// RTSP, SRT); the Manager's claims start from them.
	Reserved []Claim
	Logger   *slog.Logger // nil discards
	// Now and After drive the supervisor's own timing and nothing else: the
	// restart backoff (After) and the 60 s rule that resets it (Now). They
	// are NOT passed to the pipeline, the transcoder or the thumbnailer,
	// which keep real timers: a fake clock must not run their stall, respawn
	// or pacing rules. nil means time.Now and time.After.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Open is for tests; nil means ingest.Open.
	Open func(ctx context.Context, url string) (ingest.Reader, error)
	// TranscoderArgs and TranscoderEnv are for tests: the fake child.
	TranscoderArgs, TranscoderEnv []string

	// clients are the TLS clients of RootCAs, built once by NewManager for
	// all of its supervisors (withClients); nil until then.
	clients *clients
}

// clients are the TLS clients of the streams, built from Env.RootCAs.
type clients struct {
	tls  *tls.Config  // the base config of rtmps and rtsps inputs and rtmps pushes; never modified
	http *http.Client // HTTP inputs; nil means ingest's default client (the system's roots)
}

// withClients returns env with its clients built from RootCAs, unless they
// already are.
func (e Env) withClients() Env {
	if e.clients == nil {
		c := &clients{tls: certs.Client(e.RootCAs, false)}
		if e.RootCAs != nil {
			c.http = ingest.NewHTTPClient(e.RootCAs)
		}
		e.clients = c
	}
	return e
}

// pushOptions returns the options of every pusher: the TLS client of an
// rtmps:// target, the one of withClients when env has it.
func (e Env) pushOptions() rtmppush.Options {
	if e.clients == nil {
		return rtmppush.Options{RTMP: rtmp.Config{TLS: certs.Client(e.RootCAs, false)}}
	}
	return rtmppush.Options{RTMP: rtmp.Config{TLS: e.clients.tls}}
}

// Claim is one listen address: a server listener's, or a stream's listen
// input's (R-D11).
type Claim struct {
	UDP   bool   // the family: UDP (UDP and SRT listen inputs, the SRT server) or TCP
	Host  string // "" is the wildcard (0.0.0.0, ::, or no host)
	Port  int
	RTMP  bool   // an RTMP listener: RTMP claims on the same Host and Port share it
	TLS   bool   // an RTMPS listener: shares only with RTMP claims that are TLS too
	Path  string // an RTMP publish path; "" for a play listener and for every other claim
	Owner string // a stream name, or "server"
}

// checkError is a Check failure: its text is scrubbed of URL secrets, and it
// still unwraps to its cause.
type checkError struct {
	msg   string
	cause error
}

func (e *checkError) Error() string { return e.msg }
func (e *checkError) Unwrap() error { return e.cause }

// checkErr returns "stream: check: <what>: <err>", scrubbed.
func checkErr(what string, err error) error {
	return &checkError{msg: logging.Scrub(fmt.Sprintf("stream: check: %s: %v", what, err)), cause: err}
}

// Check reports why sp cannot run under env (R-D11). sp has passed Validate.
// Listen-address overlaps are not checked here: they are the Manager's
// claims, which see every stream and the server's listeners.
func Check(sp spec.Stream, env Env) error {
	for i, in := range sp.Inputs {
		if env.Open == nil {
			if _, err := ingest.Parse(in.URL); err != nil {
				return checkErr(fmt.Sprintf("inputs[%d]", i), err)
			}
		}
		if err := checkListenInput(in.URL, env); err != nil {
			return checkErr(fmt.Sprintf("inputs[%d]", i), err)
		}
	}
	if _, err := layoutOf(sp); err != nil {
		return checkErr("layout", err)
	}
	for _, o := range []struct {
		on   bool
		off  bool // every listener of the output is off
		name string
		keys string // the keys that turn one on
	}{
		{sp.Outputs.RTMP, env.RTMPPlay == "" && env.RTMPSPlay == "", "rtmp", "server.rtmp.listen or server.rtmp.tls_listen"},
		{sp.Outputs.RTSP, env.RTSPPlay == "" && env.RTSPSPlay == "", "rtsp", "server.rtsp.listen or server.rtsp.tls_listen"},
		{sp.Outputs.SRT, env.SRTPlay == "", "srt", "server.srt.listen"},
	} {
		if o.on && o.off {
			return checkErr("outputs."+o.name, fmt.Errorf("the server's %s listener is off: set %s and restart, or turn the output off", o.name, o.keys))
		}
	}
	scratch := hub.New(hub.Options{})
	defer scratch.Close()
	if sp.Transcode != nil {
		opt, err := ladderOf(sp, env)
		if err != nil {
			return checkErr("transcode", err)
		}
		tr, err := transcode.New(scratch, opt)
		if err != nil {
			return checkErr("transcode", err)
		}
		closeHubs(tr)
	}
	if sp.Thumbnail != nil {
		if env.Transcoder == "" {
			return checkErr("thumbnail", errors.New("thumbnails need the transcoder, which is not configured"))
		}
		if err := thumbnailOf(sp).Validate(); err != nil {
			return checkErr("thumbnail", err)
		}
		if !encodes(sp) {
			if _, err := transcode.NewThumbnailer(scratch, thumbnailerOf(sp, env)); err != nil {
				return checkErr("thumbnail", err)
			}
		}
	}
	for i, p := range sp.Outputs.Push {
		if _, err := rtmppush.New(scratch, p.URL, env.pushOptions()); err != nil {
			return checkErr(fmt.Sprintf("outputs.push[%d]", i), err)
		}
	}
	return nil
}

// closeHubs closes the hubs of a transcoder that will never run.
func closeHubs(tr *transcode.Transcoder) {
	for _, h := range tr.Hubs() {
		h.Close()
	}
	if p := tr.Presentation(); p != nil {
		p.Close()
	}
}

// checkListenInput refuses an RTMP listen input that could never open, or
// that would shadow a play path. An rtmps listen input must be on the
// server's RTMPS listener, by its listener key (rtmpsrv.AddrKey), which is
// how the registry finds it: a stream cannot open an RTMPS listener of its
// own, because the certificate is the server's (R-S6). Then checkPublishPath
// applies to it on that listener, as it does to a plain one on RTMPPlay.
func checkListenInput(input string, env Env) error {
	addr, path, secure, ok := rtmpListenKey(input)
	if !ok {
		return nil
	}
	if !secure {
		return checkPublishPath(addr, path, env.RTMPPlay, env.RTMPApp)
	}
	if env.RTMPSPlay == "" {
		return errors.New("the server's RTMPS listener is off: set server.rtmp.tls_listen and server.tls, and restart")
	}
	key, err := rtmpsrv.AddrKey(env.RTMPSPlay)
	if err != nil {
		key = env.RTMPSPlay
	}
	if addr != key {
		return fmt.Errorf("an rtmps listen input must be on the server's RTMPS listener %s", key)
	}
	return checkPublishPath(addr, path, env.RTMPSPlay, env.RTMPApp)
}

// checkPublishPath refuses an RTMP listen input, of listener key addr and
// publish path path, on the server's play address play that publishes under
// the play app: a publish path is never playable, so a publisher at
// <playApp>/<anything> would shadow the play path of whichever stream has,
// or later gets, that name. Refusing the whole app is the rule that does not
// depend on other streams. play "" is off.
func checkPublishPath(addr, path, play, playApp string) error {
	if play == "" {
		return nil
	}
	key, err := rtmpsrv.AddrKey(play)
	if err != nil {
		return nil //nolint:nilerr // no play listener to share: the server reports its own address
	}
	if addr != key {
		return nil
	}
	app := ""
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		app = path[:i]
	}
	if norm, _ := rtmp.NormalizePath(playApp, ""); norm != "" && app == norm {
		return fmt.Errorf("an RTMP listen input on the play address may not publish under the play app %q: publish under another app", norm)
	}
	return nil
}

// rtmpListenKey returns the listener key and the publish path of an RTMP
// listen input, rtmp:// or rtmps://, and whether it is rtmps:// (on the
// server's RTMPS listener; ingest.TLSListen); ok is false for any other
// input, an rtmps+insecure:// one included, which ingest refuses.
func rtmpListenKey(input string) (addr, path string, secure, ok bool) {
	if addr, path, ok := ingest.TLSListen(input); ok {
		return addr, path, true, true
	}
	u, err := url.Parse(input)
	if err != nil || !strings.EqualFold(u.Scheme, "rtmp") {
		return "", "", false, false
	}
	if mode := u.Query().Get("mode"); mode != "listen" && mode != "listener" {
		return "", "", false, false
	}
	port := u.Port()
	if port == "" {
		port = "1935"
	}
	addr, err = rtmpsrv.AddrKey(net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", "", false, false
	}
	path, _ = rtmp.NormalizePath(u.Path, "")
	return addr, path, false, true
}

// The functions below turn a spec into options. Check and the generation's
// build share them, so they cannot disagree on the spec.

// layoutOf maps the spec's layout to track kinds; an empty layout is video,
// audio.
func layoutOf(sp spec.Stream) ([]media.Kind, error) {
	if len(sp.Layout) == 0 {
		return []media.Kind{media.KindVideo, media.KindAudio}, nil
	}
	out := make([]media.Kind, 0, len(sp.Layout))
	for _, l := range sp.Layout {
		switch l {
		case "video":
			out = append(out, media.KindVideo)
		case "audio":
			out = append(out, media.KindAudio)
		default:
			return nil, fmt.Errorf("unknown kind %q, want video or audio", l)
		}
	}
	return out, nil
}

// inputsOf returns the spec's input URLs, by priority.
func inputsOf(sp spec.Stream) []string {
	out := make([]string, len(sp.Inputs))
	for i, in := range sp.Inputs {
		out[i] = in.URL
	}
	return out
}

// encodes reports whether the spec's ladder needs the transcoder child: a
// rendition or the audio is encoded.
func encodes(sp spec.Stream) bool {
	if sp.Transcode == nil {
		return false
	}
	enc := !sp.Transcode.Audio.Copy
	for _, r := range sp.Transcode.Renditions {
		enc = enc || !r.Copy
	}
	return enc
}

var positions = map[string]picture.Position{
	"": picture.TopLeft, "tl": picture.TopLeft, "tr": picture.TopRight,
	"bl": picture.BottomLeft, "br": picture.BottomRight, "c": picture.Center,
}

// ladderOf returns the transcoder options of a spec with a ladder: rendition
// i is named <name>_<label>, the presentation hub is on, and a watermark's
// image and font are read from env.AssetsDir, so the assets must exist now.
func ladderOf(sp spec.Stream, env Env) (transcode.Options, error) {
	t := sp.Transcode
	opt := transcode.Options{
		Binary: env.Transcoder, Args: env.TranscoderArgs, Env: env.TranscoderEnv,
		Audio: transcode.Audio{
			Copy: t.Audio.Copy, BitrateKbps: t.Audio.BitrateKbps,
			SampleRate: t.Audio.SampleRate, Channels: t.Audio.Channels,
		},
		Presentation: true,
		Logger:       env.Logger,
	}
	for _, r := range t.Renditions {
		opt.Renditions = append(opt.Renditions, transcode.Rendition{
			Name: sp.Name + "_" + r.Label,
			Video: transcode.Video{
				Copy: r.Copy, Height: r.Height, BitrateKbps: r.BitrateKbps, MaxBitrateKbps: r.MaxBitrateKbps,
				BFrames: r.BFrames, Preset: r.Preset, Encoder: r.Encoder, NoFallback: r.NoFallback,
			},
		})
	}
	if encodes(sp) && env.Transcoder == "" {
		return opt, errors.New("encoding needs the transcoder, which is not configured")
	}
	w, err := watermarkOf(sp, env)
	if err != nil {
		return opt, fmt.Errorf("watermark: %w", err)
	}
	opt.Watermark = w
	if sp.Thumbnail != nil && encodes(sp) {
		th := thumbnailOf(sp)
		opt.Thumbnail = &th
	}
	return opt, nil
}

// thumbnailOf returns the thumbnail setting of a spec that has one.
func thumbnailOf(sp spec.Stream) picture.Thumbnail {
	return picture.Thumbnail{
		IntervalSec: sp.Thumbnail.IntervalSec, Height: sp.Thumbnail.Height, Quality: sp.Thumbnail.Quality,
	}
}

// thumbnailerOf returns the options of the Thumbnailer of a spec with a
// thumbnail and no child that encodes.
func thumbnailerOf(sp spec.Stream, env Env) transcode.ThumbnailerOptions {
	return transcode.ThumbnailerOptions{
		Binary: env.Transcoder, Args: env.TranscoderArgs, Env: env.TranscoderEnv,
		Thumbnail: thumbnailOf(sp), Logger: env.Logger,
	}
}
