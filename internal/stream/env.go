package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

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
	// RTMPPlay is the server's RTMP play address, as bound, and RTMPApp its
	// app, for Check's publish-path rule; "" means off.
	RTMPPlay, RTMPApp string
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
}

// Claim is one listen address: a server listener's, or a stream's listen
// input's (R-D11).
type Claim struct {
	UDP   bool   // the family: UDP (UDP and SRT listen inputs, the SRT server) or TCP
	Host  string // "" is the wildcard (0.0.0.0, ::, or no host)
	Port  int
	RTMP  bool   // an RTMP listener: RTMP claims on the same Host and Port share it
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
		if err := checkPublishPath(in.URL, env); err != nil {
			return checkErr(fmt.Sprintf("inputs[%d]", i), err)
		}
	}
	if _, err := layoutOf(sp); err != nil {
		return checkErr("layout", err)
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
		if _, err := rtmppush.New(scratch, p.URL, rtmppush.Options{}); err != nil {
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

// checkPublishPath refuses an RTMP listen input on the server's play
// address that publishes under the play app: a publish path is never
// playable, so a publisher at <RTMPApp>/<anything> would shadow the play
// path of whichever stream has, or later gets, that name. Refusing the
// whole app is the rule that does not depend on other streams.
func checkPublishPath(input string, env Env) error {
	if env.RTMPPlay == "" {
		return nil
	}
	play, err := rtmpsrv.AddrKey(env.RTMPPlay)
	if err != nil {
		return nil //nolint:nilerr // no play listener to share: the server reports its own address
	}
	addr, path, ok := rtmpListenKey(input)
	if !ok || addr != play {
		return nil
	}
	app := ""
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		app = path[:i]
	}
	if playApp, _ := rtmp.NormalizePath(env.RTMPApp, ""); playApp != "" && app == playApp {
		return fmt.Errorf("an RTMP listen input on the play address may not publish under the play app %q: publish under another app", playApp)
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
// i is named <name>_<label>, the presentation hub is on, and an image
// watermark is read from env.AssetsDir, so the asset must exist now.
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
				BFrames: r.BFrames, Preset: r.Preset, Encoder: r.Encoder,
			},
		})
	}
	if encodes(sp) && env.Transcoder == "" {
		return opt, errors.New("encoding needs the transcoder, which is not configured")
	}
	if w := t.Watermark; w != nil {
		pos, ok := positions[w.Position]
		if !ok {
			return opt, fmt.Errorf("watermark: unknown position %q", w.Position)
		}
		pw := picture.Watermark{
			Text: w.Text, Position: pos, OpacityPct: w.OpacityPct, SizePct: w.SizePct, TimeZone: w.TimeZone,
		}
		if w.Image != "" {
			if env.AssetsDir == "" {
				return opt, errors.New("watermark: an image needs the assets directory, which is not configured")
			}
			img, err := picture.LoadAsset(env.AssetsDir, w.Image)
			if err != nil {
				return opt, fmt.Errorf("watermark: %w", err)
			}
			pw.Image = img
		}
		opt.Watermark = &pw
	}
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
