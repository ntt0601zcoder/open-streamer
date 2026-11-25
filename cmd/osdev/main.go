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
// To serve every stream to RTSP players (ffplay, VLC, ffmpeg), give
// -rtsp-play an address; a player plays rtsp://<host>:<port>/<app>/<name>,
// where app is -rtsp-play-app (default live). Players use TCP interleaved
// RTP; -rtsp-udp also serves UDP, from one RTP/RTCP port pair on the
// -rtsp-play host:
//
//	osdev -stream cam=in.ts -loop -rtsp-play :8554 -rtsp-udp
//	ffplay -rtsp_transport udp rtsp://<host>:8554/live/cam
//
// To serve every stream to SRT receivers (ffplay, VLC, ffmpeg), give
// -srt-play a UDP address, which must not overlap the address of an SRT
// listen input (the same port, on the same host or either one a
// wildcard); the stream ID names the stream, as <app>/<name> or in the
// access-control form #!::r=<app>/<name>, where app is -srt-play-app
// (default live). -srt-latency sets the SRT latency (default 120ms):
//
//	osdev -stream cam=in.ts -loop -srt-play :9000
//	ffplay 'srt://<host>:9000?streamid=live/cam'
//
// To push a stream to an RTMP server (a CDN ingest, a restreaming service),
// give -rtmp-push name=URL, once per target; the URL's query goes to the
// server as is. A target that fails is retried with a backoff of 1 s to
// 30 s. The URL is a secret: osdev shows it only redacted, the stream key
// cut to its first 2 characters:
//
//	osdev -stream cam=in.ts -loop -rtmp-push cam=rtmp://a.example.com/live2/<stream-key>
//
// To transcode a stream into a ladder, give -rung name=label:height:kbps
// once per encoded rendition, or name=label:copy for one that copies the
// source video; -audio name=copy|aac[:kbps], once per stream, sets the
// audio of every rendition of the stream (default aac at 128 kbps). Each
// rendition is served as its own stream, <name>_<label>, on every output.
// Encoding runs in the transcoder child, cmd/ostranscoder, whose path
// -ostranscoder gives (build it with make ostranscoder). A ladder that only
// copies, with -audio name=copy (the default audio is AAC), needs none.
// Renditions are presented 0.5 s behind the source. The ladder is also
// served as one adaptive presentation, <name>_abr, over HLS and DASH only
// (not RTMP, RTSP, SRT, HTTP-TS or copy://), so the label abr is reserved:
//
//	osdev -stream cam=in.ts -loop -ostranscoder dist/ostranscoder-amd64/ostranscoder \
//		-rung cam=720:720:3000 -rung cam=360:360:800 -rung cam=src:copy -audio cam=aac:96
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtspplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/srtout"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

const (
	subQueue       = 1024
	abrLabel       = "abr" // the presentation of stream <name> is <name>_abr
	reportInterval = 30 * time.Second
	// maxSRTLatency is the largest SRT latency: the handshake carries it
	// in 16-bit milliseconds.
	maxSRTLatency = 65535 * time.Millisecond
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usageText = `usage: osdev -stream name=URL [-stream ...] [-listen addr] [-layout video,audio] [-loop]
             [-rtmp-play addr [-rtmp-play-app live]] [-rtmp-push name=rtmp://host/app/key ...]
             [-rtsp-play addr [-rtsp-play-app live] [-rtsp-udp]]
             [-srt-play addr [-srt-play-app live] [-srt-latency 120ms]]
             [-ostranscoder path] [-rung name=label:height:kbps|name=label:copy ...] [-audio name=copy|aac[:kbps]]

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

Serve every stream to RTSP players (ffplay, VLC, ffmpeg) with -rtsp-play,
over TCP interleaved RTP, and also over UDP with -rtsp-udp (one RTP/RTCP
port pair on the -rtsp-play host). The play URL is
rtsp://<host>:<port>/<app>/<name>, app from -rtsp-play-app (default live):
  osdev -stream cam=in.ts -loop -rtsp-play :8554 -rtsp-udp
  ffplay -rtsp_transport udp rtsp://<host>:8554/live/<name>

Serve every stream to SRT receivers (ffplay, VLC, ffmpeg) with -srt-play, a
UDP address whose port is not that of an SRT listen input on the same host
or a wildcard. The stream ID is <app>/<name> or #!::r=<app>/<name>
(taken literally, never percent-decoded: with ffmpeg pass it raw,
?streamid=#!::r=live/<name>, or with -streamid), app from -srt-play-app
(default live);
-srt-latency sets the latency:
  osdev -stream cam=in.ts -loop -srt-play :9000
  ffplay 'srt://<host>:9000?streamid=live/<name>'

Push a stream to an RTMP server (repeat -rtmp-push for more targets; the
URL is shown only redacted, and a failing target is retried):
  osdev -stream cam=in.ts -loop -rtmp-push cam=rtmp://<host>/<app>/<stream-key>

Transcode a stream into renditions, each served as the stream <name>_<label>
(-rung per rendition: label:height:kbps encodes, label:copy copies the
video; the label abr is reserved; -audio sets the audio of every rendition,
default aac at 128 kbps; encoding needs the transcoder, built with make
ostranscoder, so a ladder without it must copy both video and audio,
-audio name=copy). The whole ladder is also served as one adaptive
presentation, <name>_abr, over HLS and DASH only (hls/index.m3u8,
cmaf/master.m3u8, cmaf/index.mpd), not over RTMP, RTSP, SRT, HTTP-TS or
copy://:
  osdev -stream cam=in.ts -loop -ostranscoder dist/ostranscoder-amd64/ostranscoder \
    -rung cam=360:360:800 -rung cam=src:copy -audio cam=aac:96
`

// streamFlags collects repeated -stream name=path values.
type streamFlags []string

func (s *streamFlags) String() string     { return strings.Join(*s, ",") }
func (s *streamFlags) Set(v string) error { *s = append(*s, v); return nil }

type source struct {
	name     string
	inputs   []string // input URLs by priority, first highest
	from     string   // a rendition's stream, which it is transcoded from
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
	// rtspPlay is the RTSP play address, empty when RTSP play is off;
	// rtspPlayApp the play app, slashes trimmed; rtspUDP enables UDP
	// sessions.
	rtspPlay, rtspPlayApp string
	rtspUDP               bool
	// srtPlay is the SRT play address, empty when SRT play is off;
	// srtPlayApp the play app, slashes trimmed; srtLatency the SRT latency.
	srtPlay, srtPlayApp string
	srtLatency          time.Duration
	pushes              []*push // in flag order
	// renditions are the transcoders' rendition streams, in -rung order;
	// transcoders the transcoders, in order of their stream's first -rung.
	renditions  []*source
	transcoders []*transcoder
	// presentations are the ladders' ABR presentations, <name>_abr, in
	// the order of transcoders. They are served by packLookup only.
	presentations []*source
}

// transcoder is the transcoder of one stream's -rung ladder.
type transcoder struct {
	stream   string
	tr       *transcode.Transcoder
	lastLine string // the last report line
}

// report writes the transcoder's counters when they differ from the last
// report.
func (x *transcoder) report(w io.Writer) {
	st := x.tr.Stats()
	c := st.Child
	line := fmt.Sprintf("osdev: stream %s: transcoder pid %d, spawns %d, exits %d, losses %d, dropped before key %d, merge dropped %d, "+
		"child dropped tracks %d, reanchors %d, audio samples dropped %d, corrupt audio %d, corrupt video %d",
		x.stream, st.PID, st.Spawns, st.Exits, st.Losses, st.DroppedBeforeKey, st.MergeDropped,
		c.DroppedTracks, c.Reanchors, c.AudioDropped, c.AudioCorrupt, c.VideoCorrupt)
	if line == x.lastLine {
		return
	}
	x.lastLine = line
	_, _ = fmt.Fprintln(w, line)
}

// parseRung parses a -rung value, name=label:height:kbps or name=label:copy.
func parseRung(v string) (stream, label string, video transcode.Video, err error) {
	stream, spec, ok := strings.Cut(v, "=")
	if !ok || !validName(stream) {
		return "", "", video, fmt.Errorf("malformed -rung %q, want name=label:height:kbps or name=label:copy", v)
	}
	f := strings.Split(spec, ":")
	label = f[0]
	if !validName(label) {
		return "", "", video, fmt.Errorf("-rung %q: invalid label %q", v, label)
	}
	if label == abrLabel {
		return "", "", video, fmt.Errorf("-rung %q: the label %q is reserved for the ABR presentation", v, label)
	}
	switch {
	case len(f) == 2 && f[1] == "copy":
		return stream, label, transcode.Video{Copy: true}, nil
	case len(f) > 1 && f[1] == "copy":
		return "", "", video, fmt.Errorf("-rung %q: label:copy takes nothing more", v)
	case len(f) == 3:
		height, herr := strconv.Atoi(f[1])
		kbps, kerr := strconv.Atoi(f[2])
		if herr != nil || kerr != nil || height <= 0 || kbps <= 0 {
			return "", "", video, fmt.Errorf("-rung %q: height and kbps must be positive integers", v)
		}
		return stream, label, transcode.Video{Height: height, BitrateKbps: kbps}, nil
	default:
		return "", "", video, fmt.Errorf("malformed -rung %q, want name=label:height:kbps or name=label:copy", v)
	}
}

// parseAudio parses an -audio value, name=copy, name=aac or name=aac:kbps.
func parseAudio(v string) (stream string, audio transcode.Audio, err error) {
	stream, spec, ok := strings.Cut(v, "=")
	if !ok || !validName(stream) {
		return "", audio, fmt.Errorf("malformed -audio %q, want name=copy or name=aac[:kbps]", v)
	}
	switch codec, kbps, hasRate := strings.Cut(spec, ":"); {
	case spec == "copy":
		return stream, transcode.Audio{Copy: true}, nil
	case codec == "aac" && !hasRate:
		return stream, transcode.Audio{}, nil
	case codec == "aac":
		n, err := strconv.Atoi(kbps)
		if err != nil || n <= 0 {
			return "", audio, fmt.Errorf("-audio %q: kbps must be a positive integer", v)
		}
		return stream, transcode.Audio{BitrateKbps: n}, nil
	default:
		return "", audio, fmt.Errorf("malformed -audio %q, want name=copy or name=aac[:kbps]", v)
	}
}

// parseLadders builds one transcoder per stream with -rung values, and
// adds each rendition to byName as the stream <name>_<label>.
func parseLadders(cfg *config, byName map[string]*source, binary string, rungs, audios []string, log *slog.Logger) error {
	ladders := map[string][]transcode.Rendition{}
	var order []string
	for _, v := range rungs {
		stream, label, video, err := parseRung(v)
		if err != nil {
			return err
		}
		if !slices.Contains(cfg.sources, byName[stream]) {
			return fmt.Errorf("-rung: unknown stream %s", stream)
		}
		name := stream + "_" + label
		if byName[name] != nil {
			return fmt.Errorf("-rung: rendition %s: the name is already a stream", name)
		}
		if _, ok := ladders[stream]; !ok {
			order = append(order, stream)
		}
		ladders[stream] = append(ladders[stream], transcode.Rendition{Name: name, Video: video})
		byName[name] = &source{name: name, from: stream} // reserved; completed below
	}
	audio := map[string]transcode.Audio{}
	for _, v := range audios {
		stream, a, err := parseAudio(v)
		if err != nil {
			return err
		}
		if _, ok := ladders[stream]; !ok {
			return fmt.Errorf("-audio: stream %s has no -rung", stream)
		}
		if _, ok := audio[stream]; ok {
			return fmt.Errorf("-audio: stream %s given twice", stream)
		}
		audio[stream] = a
	}
	for _, stream := range order {
		encodes := !audio[stream].Copy
		for _, r := range ladders[stream] {
			encodes = encodes || !r.Video.Copy
		}
		abrName := stream + "_" + abrLabel
		if byName[abrName] != nil {
			return fmt.Errorf("-rung: stream %s: the name %s is already a stream", stream, abrName)
		}
		if encodes && binary == "" {
			return fmt.Errorf("stream %s: encoding needs -ostranscoder", stream)
		}
		tr, err := transcode.New(byName[stream].hub, transcode.Options{
			Binary: binary, Renditions: ladders[stream], Audio: audio[stream], Logger: log,
			Presentation: true,
		})
		if err != nil {
			return fmt.Errorf("stream %s: %w", stream, err)
		}
		cfg.transcoders = append(cfg.transcoders, &transcoder{stream: stream, tr: tr})
		for i, h := range tr.Hubs() {
			s := byName[ladders[stream][i].Name]
			s.hub, s.pack = h, live.NewPackager(h, live.Options{})
			cfg.renditions = append(cfg.renditions, s)
		}
		ph := tr.Presentation()
		cfg.presentations = append(cfg.presentations, &source{
			name: stream + "_" + abrLabel, from: stream, hub: ph,
			pack: live.NewPackager(ph, live.Options{ABR: true}),
		})
	}
	return nil
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
	rtspPlay := fs.String("rtsp-play", "", "serve every stream to RTSP players on this address (host:port)")
	rtspPlayApp := fs.String("rtsp-play-app", "live", "the app of RTSP play URLs: rtsp://host:port/<app>/<name>")
	rtspUDP := fs.Bool("rtsp-udp", false, "also serve RTSP players over UDP, from one RTP/RTCP port pair on the -rtsp-play host")
	srtPlay := fs.String("srt-play", "", "serve every stream to SRT receivers on this UDP address (host:port)")
	srtPlayApp := fs.String("srt-play-app", "live", "the app of SRT stream IDs: <app>/<name> or #!::r=<app>/<name>")
	srtLatency := fs.Duration("srt-latency", 120*time.Millisecond, "the SRT latency of -srt-play")
	var streams, pushes streamFlags
	fs.Var(&streams, "stream", "name=URL (or name=path.ts) to serve; repeat a name to add a lower-priority input")
	fs.Var(&pushes, "rtmp-push", "name=rtmp://host/app/key: push stream name to an RTMP server; repeatable")
	ostx := fs.String("ostranscoder", "", "path of the transcoder, cmd/ostranscoder: needed to encode a -rung or aac -audio")
	var rungs, audios streamFlags
	fs.Var(&rungs, "rung", "name=label:height:kbps (encode) or name=label:copy: a rendition of stream name, served as <name>_<label>; repeatable")
	fs.Var(&audios, "audio", "name=copy or name=aac[:kbps]: the audio of stream name's renditions (default aac, 128 kbps)")
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
	rtspApp := strings.Trim(*rtspPlayApp, "/")
	if *rtspPlay != "" {
		if err := checkHostPort(*rtspPlay); err != nil {
			_, _ = fmt.Fprintf(stderr, "osdev: -rtsp-play: %v\n", err)
			return nil, 2
		}
		if rtspApp == "" {
			_, _ = fmt.Fprintln(stderr, "osdev: -rtsp-play-app: empty app")
			return nil, 2
		}
	} else if *rtspUDP {
		_, _ = fmt.Fprintln(stderr, "osdev: -rtsp-udp needs -rtsp-play")
		return nil, 2
	}
	srtApp := strings.Trim(*srtPlayApp, "/")
	if *srtPlay != "" {
		if err := checkHostPort(*srtPlay); err != nil {
			_, _ = fmt.Fprintf(stderr, "osdev: -srt-play: %v\n", err)
			return nil, 2
		}
		if srtApp == "" {
			_, _ = fmt.Fprintln(stderr, "osdev: -srt-play-app: empty app")
			return nil, 2
		}
		if *srtLatency <= 0 || *srtLatency > maxSRTLatency {
			_, _ = fmt.Fprintf(stderr, "osdev: -srt-latency: %v out of range (0, %v]\n", *srtLatency, maxSRTLatency)
			return nil, 2
		}
	}
	cfg := &config{
		listen: *listen, layout: layout, rtmpPlay: *rtmpPlay, rtmpPlayApp: app,
		rtspPlay: *rtspPlay, rtspPlayApp: rtspApp, rtspUDP: *rtspUDP,
		srtPlay: *srtPlay, srtPlayApp: srtApp, srtLatency: *srtLatency,
	}
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
	if err := checkSRTPlayAddr(cfg); err != nil {
		_, _ = fmt.Fprintf(stderr, "osdev: -srt-play: %v\n", err)
		return nil, 2
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := parseLadders(cfg, byName, *ostx, rungs, audios, log); err != nil {
		_, _ = fmt.Fprintf(stderr, "osdev: %v\n", err)
		return nil, 2
	}
	renditionOf := map[string]string{}
	for _, r := range cfg.renditions {
		renditionOf[r.name] = r.from
	}
	if err := checkCopyCycles(cfg.sources, renditionOf); err != nil {
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

// checkHostPort accepts host:port with a port from 1 to 65535.
func checkHostPort(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

// srtHostKey normalises an SRT host for comparison: an unspecified host
// (empty, 0.0.0.0 or ::) is "", an IP is in canonical form (an IPv4-mapped
// address unmapped), and a name is lower-cased.
func srtHostKey(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsUnspecified() {
			return ""
		}
		return ip.String()
	}
	return strings.ToLower(host)
}

// srtAddrsCollide reports whether two SRT listen addresses overlap: the
// same port, and the same host or either host unspecified. gosrt sets
// SO_REUSEADDR, so a wildcard and a specific address on one port both bind
// and the kernel splits the traffic between them.
func srtAddrsCollide(hostA, portA, hostB, portB string) bool {
	pa, errA := strconv.Atoi(portA)
	pb, errB := strconv.Atoi(portB)
	if errA != nil || errB != nil || pa != pb {
		return false
	}
	a, b := srtHostKey(hostA), srtHostKey(hostB)
	return a == b || a == "" || b == ""
}

// checkSRTPlayAddr refuses an -srt-play address that overlaps the address
// of an SRT listen input (srtAddrsCollide). The input URL is not echoed,
// since it may hold a passphrase.
func checkSRTPlayAddr(cfg *config) error {
	if cfg.srtPlay == "" {
		return nil
	}
	host, port, _ := net.SplitHostPort(cfg.srtPlay) // checked by checkHostPort
	for _, s := range cfg.sources {
		for _, in := range s.inputs {
			u, err := url.Parse(in)
			if err != nil || !strings.EqualFold(u.Scheme, "srt") {
				continue
			}
			if spec, err := ingest.Parse(in); err != nil || !spec.Passive {
				continue
			}
			if srtAddrsCollide(host, port, u.Hostname(), u.Port()) {
				return fmt.Errorf("%s overlaps the address of an SRT listen input of stream %s", cfg.srtPlay, s.name)
			}
		}
	}
	return nil
}

// startSRT serves every stream to SRT receivers on cfg.srtPlay. It returns
// once the listener is bound; stop closes the server and waits for it.
// Serve errors after that go to stderr.
func startSRT(cfg *config, lookup func(string) (*hub.Hub, bool), stderr io.Writer) (stop func(), err error) {
	srv := srtout.New(lookup, srtout.Options{App: cfg.srtPlayApp, Latency: cfg.srtLatency})
	if err := srv.Listen(cfg.srtPlay); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, srtout.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "osdev: -srt-play: %v\n", err)
		}
	})
	return func() {
		_ = srv.Close()
		wg.Wait()
	}, nil
}

// startRTSP serves every stream to RTSP players on cfg.rtspPlay. It returns
// once the listener (and, with -rtsp-udp, the UDP pair) is bound; stop
// closes the server and waits for it. Serve errors after that go to stderr.
func startRTSP(ctx context.Context, cfg *config, lookup func(string) (*hub.Hub, bool), stderr io.Writer) (stop func(), err error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.rtspPlay)
	if err != nil {
		return nil, err
	}
	var opt rtspplay.Options
	opt.App = cfg.rtspPlayApp
	if cfg.rtspUDP {
		host, _, _ := net.SplitHostPort(cfg.rtspPlay)
		if host == "" {
			// Every interface. Go binds the unspecified 0.0.0.0 as a
			// dual-stack [::] socket where the system allows it, so the
			// pair then serves IPv4 and IPv6 players; on a host without
			// IPv6 (or with IPv4-mapped addresses disabled) it binds IPv4
			// only.
			host = "0.0.0.0"
		}
		opt.Server.UDPHost = host
	}
	srv := rtspplay.New(lookup, opt)
	served := make(chan error, 1)
	//nolint:contextcheck // the server lives until stop, not with ctx
	go func() { served <- srv.Serve(ln) }()
	if cfg.rtspUDP {
		// Serve binds the pair before it accepts; wait for it so that a
		// bind failure is a startup error.
		for srv.UDPPorts()[0] == 0 {
			select {
			case err := <-served:
				_ = ln.Close()
				return nil, err
			case <-time.After(time.Millisecond):
			}
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := <-served; err != nil && !errors.Is(err, rtsp.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "osdev: -rtsp-play: %v\n", err)
		}
	})
	return func() {
		_ = srv.Close()
		wg.Wait()
	}, nil
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
// or through other streams or renditions: such a stream would re-ingest its
// own output. renditionOf maps each rendition stream to the stream it is
// transcoded from, which it reads.
func checkCopyCycles(srcs []*source, renditionOf map[string]string) error {
	reads := map[string][]string{} // stream → the streams it reads
	names := make([]string, 0, len(srcs)+len(renditionOf))
	for _, s := range srcs {
		names = append(names, s.name)
		for _, in := range s.inputs {
			if t, ok := copyTarget(in); ok {
				reads[s.name] = append(reads[s.name], t)
			}
		}
	}
	for r, from := range renditionOf {
		names = append(names, r)
		reads[r] = []string{from}
	}
	slices.Sort(names[len(srcs):]) // renditions in a stable order
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		state[name] = visiting
		path = append(path, name)
		for _, t := range reads[name] {
			if !known[t] {
				continue
			}
			switch state[t] {
			case visiting:
				if t == name {
					return fmt.Errorf("stream %s: copy://%s reads its own stream", name, t)
				}
				return fmt.Errorf("copy:// cycle between streams: %s -> %s", strings.Join(path, " -> "), t)
			case 0:
				if err := visit(t, path); err != nil {
					return err
				}
			}
		}
		state[name] = done
		return nil
	}
	for _, n := range names {
		if state[n] == 0 {
			if err := visit(n, nil); err != nil {
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
// warning prints the play path, which in this case equals that publish
// path: it is <app>/<stream name>, built from the public stream name, so the
// operator already chose to make it public. No other publish path is
// printed.
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
	// every stream: the inputs' then the renditions'
	all := slices.Concat(order, cfg.renditions)
	srcs := map[string]*source{}
	for _, s := range all {
		srcs[s.name] = s
	}
	// The presentations are served over HLS and DASH only: packLookup finds
	// them, no other lookup does.
	pres := map[string]*source{}
	for _, s := range cfg.presentations {
		pres[s.name] = s
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
	stopRTSP := func() {}
	if cfg.rtspPlay != "" {
		stopRTSP, err = startRTSP(ctx, cfg, hubLookup, stderr)
		if err != nil {
			unregisterPlay()
			_ = ln.Close()
			_, _ = fmt.Fprintf(stderr, "osdev: -rtsp-play: %v\n", err)
			return 2
		}
	}
	defer func() { stopRTSP() }() // idempotent; shutdown calls it first
	stopSRT := func() {}
	if cfg.srtPlay != "" {
		//nolint:contextcheck // the server lives until stopSRT, not with ctx
		stopSRT, err = startSRT(cfg, hubLookup, stderr)
		if err != nil {
			stopRTSP()
			unregisterPlay()
			_ = ln.Close()
			_, _ = fmt.Fprintf(stderr, "osdev: -srt-play: %v\n", err)
			return 2
		}
	}
	defer func() { stopSRT() }() // idempotent; shutdown calls it first
	addr := ln.Addr().String()
	_, _ = fmt.Fprintf(stdout, "listening on %s\n", addr)
	base := "http://" + addr
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && tcp.IP.IsUnspecified() {
		base = fmt.Sprintf("http://127.0.0.1:%d", tcp.Port)
	}
	for _, s := range all {
		for _, p := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd", "stream.ts"} {
			_, _ = fmt.Fprintf(stdout, "%s/live/%s/%s\n", base, s.name, p)
		}
	}
	for _, s := range cfg.presentations {
		for _, p := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd"} {
			_, _ = fmt.Fprintf(stdout, "%s/live/%s/%s\n", base, s.name, p)
		}
	}
	if cfg.rtmpPlay != "" {
		host, port, _ := net.SplitHostPort(cfg.rtmpPlay)
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			host = "127.0.0.1"
		}
		for _, s := range all {
			_, _ = fmt.Fprintf(stdout, "rtmp://%s/%s/%s\n", net.JoinHostPort(host, port), cfg.rtmpPlayApp, s.name)
		}
	}
	if cfg.rtspPlay != "" {
		host, port, _ := net.SplitHostPort(cfg.rtspPlay)
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			host = "127.0.0.1"
		}
		for _, s := range all {
			_, _ = fmt.Fprintf(stdout, "rtsp://%s/%s/%s\n", net.JoinHostPort(host, port), cfg.rtspPlayApp, s.name)
		}
	}
	if cfg.srtPlay != "" {
		host, port, _ := net.SplitHostPort(cfg.srtPlay)
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			host = "127.0.0.1"
		}
		for _, s := range all {
			_, _ = fmt.Fprintf(stdout, "srt://%s?streamid=%s/%s\n", net.JoinHostPort(host, port), cfg.srtPlayApp, s.name)
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

	// The transcoders start after their sources, and close their rendition
	// hubs when they return.
	var twg sync.WaitGroup
	var transcodeFailed atomic.Bool
	for _, s := range cfg.renditions {
		sub := s.hub.Subscribe(s.name, subQueue)
		wg.Go(func() { s.pack.Run(ctx, sub) })
	}
	// A presentation's subscription holds a frame queue per rendition and
	// its own.
	for i, s := range cfg.presentations {
		sub := s.hub.Subscribe(s.name, subQueue*(len(cfg.transcoders[i].tr.Hubs())+1))
		wg.Go(func() { s.pack.Run(ctx, sub) })
	}
	for _, x := range cfg.transcoders {
		twg.Go(func() {
			err := x.tr.Run(ctx)
			switch {
			case ctx.Err() != nil:
			case err == nil:
				_, _ = fmt.Fprintf(stderr, "osdev: stream %s: renditions ended with the stream\n", x.stream)
			default:
				transcodeFailed.Store(true)
				_, _ = fmt.Fprintf(stderr, "osdev: stream %s: transcoder stopped: %v\n", x.stream, err)
			}
		})
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
				report(stderr, slices.Concat(all, cfg.presentations), false)
				for _, x := range cfg.transcoders {
					x.report(stderr)
				}
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
		if s := pres[n]; s != nil {
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
	twg.Wait()       // the transcoders closed their rendition hubs
	unregisterPlay() // the players got end of stream; this waits for them
	stopRTSP()       // the RTSP players got their BYEs; this waits for them
	stopSRT()        // ends the SRT receivers and waits for them
	sd, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer done()
	if err := srv.Shutdown(sd); err != nil {
		_ = srv.Close()
	}
	wg.Wait()
	report(stderr, slices.Concat(all, cfg.presentations), true)
	for _, x := range cfg.transcoders {
		x.report(stderr)
	}
	for _, p := range cfg.pushes {
		p.report(stderr)
	}
	if code == 0 && (failed.Load() > 0 || transcodeFailed.Load()) {
		code = 1
	}
	return code
}
