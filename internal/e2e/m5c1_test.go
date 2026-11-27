package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/server"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

// The M5c1 scenarios run the whole server with one certificate pair and
// every TLS listener beside its plain one, and prove M5c1's exit criterion
// (R-S1): ffmpeg publishes over RTMPS and plays over RTMPS, RTSPS and HTTPS
// (HLS, DASH, HTTP-TS) against our server; our server pulls from and pushes
// to a peer over RTMPS and pulls RTSPS. They build on the M5a harness: the
// same config, users, source and helpers.

const (
	// m5c1Poll is how often the scenarios' servers look at the certificate
	// files.
	m5c1Poll = 100 * time.Millisecond
	// m5c1Hub is how long a stream that pulls or takes a push over TLS has
	// to get video and audio.
	m5c1Hub = 15 * time.Second
	// m5c1Key is the stream key of the scenarios' RTMPS publish paths.
	m5c1Key = "tlskey-0123456789"
	// m5c1Shared is the least a hub shares, frame for frame, with the hub
	// it was fed from: 2 s of the fixture's video, and as many audio frames.
	m5c1Shared = 50
)

// m5c1 is a running server with TLS on, and what a client needs to verify
// it.
type m5c1 struct {
	*m5a
	ca        *tlstest.CA
	caDir     string // holds only caFile: a trust directory
	caFile    string // the CA as PEM, for a peer's -ca_file
	cert, key string // the pair the server serves
	// client trusts only the CA and has no timeout (a session reader's);
	// api is the same with the API requests' timeout.
	client, api *http.Client
}

// m5c1Server is m5aServer with server.tls (a tlstest leaf for 127.0.0.1 and
// localhost), a tls_listen beside each of the four listeners that can have
// one, an assets directory, the test CA as the roots of the streams' TLS
// clients, and a certificate poll of m5c1Poll.
func m5c1Server(t *testing.T, streams string) *m5c1 {
	t.Helper()
	dir := t.TempDir()
	sub := func(name string) string {
		d := filepath.Join(dir, name)
		require.NoError(t, os.Mkdir(d, 0o755)) //nolint:gosec // a peer in the image reads the CA's directory as another user
		return d
	}
	ca := tlstest.NewCA(t)
	m := &m5c1{ca: ca, caDir: sub("ca")}
	m.caFile = ca.PEMFile(t, m.caDir)
	m.cert, m.key = ca.Files(t, sub("tls"), "127.0.0.1", "localhost")

	text := m5aConfig(streams, "")
	for _, l := range []string{"http", "api", "rtmp", "rtsp"} {
		text = m5aOnce(t, text, "  "+l+": {listen: \"127.0.0.1:0\"}\n",
			"  "+l+": {listen: \"127.0.0.1:0\", tls_listen: \"127.0.0.1:0\"}\n")
	}
	const before = "  log: {level: info, format: json}\n"
	text = m5aOnce(t, text, before,
		"  # The pair every TLS listener presents.\n"+
			"  tls: {cert_file: \""+m.cert+"\", key_file: \""+m.key+"\"}\n"+
			"  assets_dir: \""+sub("assets")+"\"\n"+before)
	path := filepath.Join(dir, "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	m.m5a = m5aStartWith(t, path, server.Options{RootCAs: ca.Pool(), CertPoll: m5c1Poll})
	for what, addr := range map[string]string{
		"https": m.addrs.HTTPS, "api": m.addrs.APITLS, "rtmps": m.addrs.RTMPS, "rtsps": m.addrs.RTSPS,
	} {
		require.NotEmpty(t, addr, "the %s listener", what)
	}
	tr := &http.Transport{TLSClientConfig: ca.Client()}
	t.Cleanup(tr.CloseIdleConnections)
	m.client = &http.Client{Transport: tr}
	m.api = &http.Client{Transport: tr, Timeout: time.Minute}
	return m
}

// secure is the server's helpers over the API's TLS listener.
func (m *m5c1) secure() *m5a {
	c := *m.m5a
	c.base, c.hc = "https://"+m.addrs.APITLS, m.api
	return &c
}

// tlsLive is live over the HTTPS listener.
func (m *m5c1) tlsLive(served string) string {
	return "https://" + m.addrs.HTTPS + "/live/" + served + "/"
}

// tlsURL is m5a.url over the TLS listeners; SRT has none.
func (m *m5c1) tlsURL(out stream.Output, served string) string {
	switch out {
	case stream.OutHLS:
		return m.tlsLive(served) + "hls/index.m3u8"
	case stream.OutDASH:
		return m.tlsLive(served) + "cmaf/index.mpd"
	case stream.OutHTTPTS:
		return m.tlsLive(served) + "stream.ts"
	case stream.OutRTMP:
		return "rtmps://" + m.addrs.RTMPS + "/live/" + served
	case stream.OutRTSP:
		return "rtsps://" + m.addrs.RTSPS + "/live/" + served
	case stream.OutSRT:
		return ""
	default:
		return ""
	}
}

// create is a per-stream PUT that must create the stream.
func (m *m5c1) create(t *testing.T, sp spec.Stream) {
	t.Helper()
	code, applied := m.put(t, sp)
	require.Equal(t, http.StatusCreated, code, "PUT %s", sp.Name)
	require.Equal(t, "created", applied, "PUT %s", sp.Name)
}

// hub waits until a stream that is fed over TLS runs with a video and an
// audio track, within m5c1Hub of since.
func (m *m5c1) hub(t *testing.T, name string, since time.Time) {
	t.Helper()
	var st stream.Status
	m5aUntil(t, max(time.Until(since.Add(m5c1Hub)), 0), func() bool {
		st = m.status(t, name)
		return st.State == "running" && len(st.Tracks) == 2
	}, "%s gets video and audio within %v: %+v", name, m5c1Hub, &st)
	t.Logf("%s: video and audio %v after its source was there", name, time.Since(since).Round(time.Millisecond))
}

// m5c1Budget fails a scenario that took longer than d from start.
func m5c1Budget(t *testing.T, start time.Time, d time.Duration) {
	t.Helper()
	took := time.Since(start)
	t.Logf("took %v of a budget of %v", took.Round(time.Millisecond), d)
	if took > d {
		t.Errorf("the scenario took %v, over its budget of %v", took.Round(time.Millisecond), d)
	}
}

// readPlayTLS is readPlay of an rtmps:// URL with the client config c.
func readPlayTLS(u string, c *tls.Config) <-chan capResult {
	ch := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		got, err := conform.ReadRTMP(ctx, u, conform.ReadOptions{Timeout: m3b1Read, TLS: c})
		ch <- capResult{got, err}
	}()
	return ch
}

// readRTSPTLS is readRTSP of an rtsps:// URL with the client config c.
func readRTSPTLS(u string, c *tls.Config) <-chan capResult {
	ch := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		got, err := conform.ReadRTSP(ctx, u, conform.ReadOptions{Timeout: m3b2Read, TLS: c})
		ch <- capResult{got, err}
	}()
	return ch
}

// m5c1Probe runs ffprobe with args on u and returns its streams as
// type:codec; an error carries what ffprobe printed.
func m5c1Probe(ctx context.Context, ffprobe string, args []string, u string) ([]string, error) {
	argv := slices.Concat([]string{"-v", "error"}, args, []string{"-show_entries", "stream=codec_type,codec_name", "-of", "json", u})
	cmd := exec.CommandContext(ctx, ffprobe, argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %v: %w: %s", args, err, strings.TrimSpace(stderr.String()))
	}
	var v struct {
		Streams []struct {
			Type  string `json:"codec_type"`
			Codec string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	s := make([]string, 0, len(v.Streams))
	for _, st := range v.Streams {
		s = append(s, st.Type+":"+st.Codec)
	}
	return s, nil
}

// m5c1AV requires that a probe found one H.264 video stream and one AAC
// audio stream. ffmpeg's FLV demuxer shows an onMetaData that is not at
// timestamp 0 as a text stream of its own (see TestM5aPeers): that one
// stream, on an RTMP URL, is left out.
func m5c1AV(t *testing.T, what, u string, streams []string, err error) {
	t.Helper()
	if !assert.NoError(t, err, "%s: %s", what, u) {
		return
	}
	got := streams
	if strings.HasPrefix(u, "rtmp") {
		got = slices.DeleteFunc(slices.Clone(streams), func(s string) bool { return s == "subtitle:text" })
	}
	assert.ElementsMatch(t, []string{"video:h264", "audio:aac"}, got, "%s: %s: streams %v", what, u, streams)
}

// m5c1RTSPVerifies reports whether ffprobe's RTSP demuxer has the options
// that verify an rtsps:// server: ffmpeg 8 has them, ffmpeg 6.1 has not.
func m5c1RTSPVerifies(t *testing.T, ffprobe string) bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobe, "-hide_banner", "-h", "demuxer=rtsp").CombinedOutput()
	require.NoError(t, err, "%s", out)
	return bytes.Contains(out, []byte("tls_verify")) && bytes.Contains(out, []byte("ca_file"))
}

// m5c1Publish starts ffmpeg with args, which publish to the listen input of
// the stream name, and waits until the stream runs with both tracks. The
// publish route is there only once the stream has opened its input, so an
// ffmpeg that was refused is started again.
func m5c1Publish(t *testing.T, m *m5c1, ffmpeg, name string, args ...string) *ffmpegProc {
	t.Helper()
	deadline := time.Now().Add(m5aWait)
	for {
		p := startFFmpeg(t, ffmpeg, args...)
		for exited := false; !exited; {
			select {
			case <-p.done:
				exited = true
			case <-time.After(50 * time.Millisecond):
				if st := m.status(t, name); st.State == "running" && len(st.Tracks) == 2 {
					return p
				}
			}
		}
		require.True(t, time.Now().Before(deadline), "ffmpeg did not publish to %s: %v: %s", name, p.err, p.stderr.String())
		time.Sleep(100 * time.Millisecond)
	}
}

// TestM5c1FFmpeg is the exit criterion's ffmpeg half. cam is a passthrough
// stream whose input is an RTMPS listen input on the server's RTMPS
// listener, with every output on. ffmpeg publishes the M5a source to it
// over TLS, verifying the server against the CA; with another CA it fails
// with "Peer certificate failed verification" and nothing is published.
// Then, at the same time, ffprobe reads one H.264 and one AAC stream from
// RTMPS, from HLS-TS, HLS fMP4, DASH and HTTP-TS over HTTPS, and from RTSPS
// with and without -rtsp_transport tcp, verifying the server wherever it
// can (its RTSP demuxer can from ffmpeg 8 on); conform reads HLS and DASH
// over HTTPS without a violation; and the plain RTMP, RTSP and HTTP URLs of
// the same stream still play. Budget 120 s. ffmpeg only: it runs in the
// arm64 job too.
func TestM5c1FFmpeg(t *testing.T) {
	t.Parallel()
	defer m5c1Budget(t, time.Now(), 120*time.Second)
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmps")
	ffprobe := testpeer.Require(t, "ffprobe", "rtmps", "https", "rtmp", "http", "tcp")
	m := m5c1Server(t, "")
	pub := "rtmps://" + m.addrs.RTMPS + "/pub/" + m5c1Key
	m.create(t, m5aSpec("cam", pub+"?mode=listen"))
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 20, 0)
	other := tlstest.NewCA(t).PEMFile(t, t.TempDir())
	failures := func() float64 {
		return m.metric(t, "openstreamer_tls_handshake_failures_total", "listener", "rtmps")
	}

	// Another CA: ffmpeg refuses the server, and nothing is published.
	before := failures()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	out, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-re", "-i", clip,
		"-t", "2", "-c", "copy", "-f", "flv", "-tls_verify", "1", "-ca_file", other, pub).CombinedOutput()
	cancel()
	require.Error(t, err, "ffmpeg published to a server it cannot verify: %s", out)
	assert.Contains(t, string(out), "Peer certificate failed verification")
	st := m.status(t, "cam")
	assert.NotEqual(t, "running", st.State, "nothing was published")
	assert.Empty(t, st.Tracks, "nothing was published")
	m5aUntil(t, 5*time.Second, func() bool { return failures() > before }, "the refused handshake is counted")

	// The CA: ffmpeg publishes.
	p := m5c1Publish(t, m, ffmpeg, "cam", "-re", "-i", clip, "-c", "copy", "-f", "flv",
		"-tls_verify", "1", "-ca_file", m.caFile, pub)
	m.listed(t, "cam", 3)

	verify := []string{"-tls_verify", "1", "-ca_file", m.caFile}
	wrong := []string{"-tls_verify", "1", "-ca_file", other}
	rtspVerifies := m5c1RTSPVerifies(t, ffprobe)
	rtsps := func(base []string, args ...string) []string {
		if !rtspVerifies {
			return args // ffmpeg 6.1: nothing verifies an rtsps:// server
		}
		return slices.Concat(base, args)
	}
	type row struct {
		what string
		args []string
		u    string
		// refused rows must fail: the same read with another CA.
		refused bool
		streams []string
		err     error
	}
	rows := []*row{
		{what: "rtmps", args: verify, u: m.tlsURL(stream.OutRTMP, "cam")},
		{what: "https hls-ts", args: verify, u: m.tlsURL(stream.OutHLS, "cam")},
		{what: "https hls-fmp4", args: verify, u: m.tlsLive("cam") + "cmaf/master.m3u8"},
		{what: "https dash", args: verify, u: m.tlsURL(stream.OutDASH, "cam")},
		{what: "https http-ts", args: verify, u: m.tlsURL(stream.OutHTTPTS, "cam")},
		{what: "rtsps over tcp", args: rtsps(verify, "-rtsp_transport", "tcp"), u: m.tlsURL(stream.OutRTSP, "cam")},
		{what: "rtsps, no transport named", args: rtsps(verify), u: m.tlsURL(stream.OutRTSP, "cam")},
		{what: "rtmp", u: m.url(stream.OutRTMP, "cam")},
		{what: "rtsp over tcp", args: []string{"-rtsp_transport", "tcp"}, u: m.url(stream.OutRTSP, "cam")},
		{what: "http hls-ts", u: m.url(stream.OutHLS, "cam")},
		{what: "http http-ts", u: m.url(stream.OutHTTPTS, "cam")},
		// The flags verify: with another CA the same reads fail.
		{what: "rtmps, another CA", args: wrong, u: m.tlsURL(stream.OutRTMP, "cam"), refused: true},
		{what: "https http-ts, another CA", args: wrong, u: m.tlsURL(stream.OutHTTPTS, "cam"), refused: true},
	}
	if rtspVerifies {
		rows = append(rows, &row{
			what: "rtsps over tcp, another CA", args: slices.Concat(wrong, []string{"-rtsp_transport", "tcp"}),
			u: m.tlsURL(stream.OutRTSP, "cam"), refused: true,
		})
	}
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			r.streams, r.err = m5c1Probe(ctx, ffprobe, r.args, r.u)
		})
	}
	// conform, with a client that trusts the CA.
	segmented := map[string]*m5aSeg{}
	var mu sync.Mutex
	for _, f := range m5aFormats {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), runLimit)
			defer cancel()
			opt := conform.ReadOptions{Duration: 4 * time.Second, Timeout: m5aRead, AlignedSequences: true, Client: m.client}
			var s m5aSeg
			if f.out == stream.OutDASH {
				s.c, s.err = conform.ReadDASH(ctx, m.tlsLive("cam")+f.file, opt)
			} else {
				s.c, s.err = conform.ReadHLS(ctx, m.tlsLive("cam")+f.file, opt)
			}
			mu.Lock()
			segmented[f.name] = &s
			mu.Unlock()
		})
	}
	wg.Wait()
	for _, r := range rows {
		if r.refused {
			if assert.Error(t, r.err, "%s: ffprobe read %v", r.what, r.streams) {
				assert.Contains(t, r.err.Error(), "Peer certificate failed verification", r.what)
			}
			continue
		}
		m5c1AV(t, r.what, r.u, r.streams, r.err)
		t.Logf("%-28s %v %v", r.what, r.args, r.streams)
	}
	for _, f := range m5aFormats {
		s := segmented[f.name]
		m5aAV(t, "conform over https: "+f.name, s.c, s.err)
		t.Logf("conform over https: %-8s %s", f.name, kindCounts(s.c.Frames))
	}
	select {
	case <-p.done:
		require.Fail(t, "the publisher ended", "%v: %s", p.err, p.stderr.String())
	default:
	}
	assert.Equal(t, "running", m.status(t, "cam").State)
}

// m5c1Payloads is the payload of every frame of kind, each as one string.
func m5c1Payloads(frames []media.Frame, kind media.Kind) []string {
	var out []string
	for _, f := range frames {
		if f.Kind() == kind {
			out = append(out, string(bytes.Join(f.Data, nil)))
		}
	}
	return out
}

// m5c1Overlap is the number of frames two recordings of one live stream
// share: the longest run on which one, laid over the other from some frame
// on, agrees with it frame for frame to the end of the shorter. Every
// alignment is tried and the best kept, since the fixture loops and its
// audio payloads repeat; 0 when none agrees.
func m5c1Overlap(a, b []string) int {
	best := 0
	for range 2 {
		for i := range a {
			n := min(len(a)-i, len(b))
			if n <= best {
				break // no later alignment is longer
			}
			same := true
			for j := 0; j < n && same; j++ {
				same = a[i+j] == b[j]
			}
			if same {
				best = n
			}
		}
		a, b = b, a
	}
	return best
}

// m5c1Packets counts the video and audio packets ffprobe reads from a file.
func m5c1Packets(t *testing.T, ffprobe, file string) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-count_packets",
		"-show_entries", "stream=codec_type,nb_read_packets", "-of", "json", file).Output()
	require.NoError(t, err, "ffprobe %s", file)
	var v struct {
		Streams []struct {
			Type    string `json:"codec_type"`
			Packets string `json:"nb_read_packets"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &v), "%s", out)
	counts := map[string]int{}
	for _, s := range v.Streams {
		n, err := strconv.Atoi(s.Packets)
		require.NoError(t, err, "%s", out)
		counts[s.Type] += n
	}
	return counts
}

// TestM5c1PullPush is the exit criterion's other half: our server pulls
// from and pushes to a peer over RTMPS, and pulls RTSPS.
//
//	(a) Our own listeners as the peer, fully verified: relay pulls
//	    rtmps://<RTMPS>/live/cam, viars pulls rtsps://<RTSPS>/live/cam, and
//	    cam pushes to rtmps://<RTMPS>/in/<key>, the listen input of sink.
//	    Each hub gets video and audio within 15 s, and each shares its
//	    frames with cam's, payload for payload.
//	(b) ffmpeg as the peer, behind testpeer.TLSFront as it would be behind a
//	    TLS terminator (ffmpeg cannot listen on rtmps): ffmpeg -listen 1
//	    serving the source as plain RTMP is pulled as rtmps://<front>/…, and
//	    ffmpeg -listen 1 recording to FLV takes cam's second push, whose
//	    file ffprobe counts video and audio packets in.
//	(c) MediaMTX as the RTSP peer, when OS_MEDIAMTX_RTSP names one: the
//	    source published to it is pulled as rtsps://<front>/<path> through a
//	    front of its RTSP port.
//	(d) Untrusted: through a front whose leaf is from another CA, the
//	    input's status carries an error with "x509" and neither the name it
//	    pulls nor the URL's password, the stream stays in its retry loop,
//	    and the same URL with +insecure plays.
//
// Budget 90 s.
func TestM5c1PullPush(t *testing.T) {
	t.Parallel()
	defer m5c1Budget(t, time.Now(), 90*time.Second)
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	ffprobe := testpeer.Require(t, "ffprobe")
	m := m5c1Server(t, "")
	src := m5aSource(t)
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 15, 0)
	hls := func(name, input string) spec.Stream {
		return spec.Stream{Name: name, Inputs: []spec.Input{{URL: input}}, Outputs: spec.Outputs{HLS: true}}
	}

	// (b)'s peers listen before a stream names them: each takes one
	// connection, which its front makes only for a client that verified it.
	const recKey = "reckey-0123456789"
	servePort, recPort := freeTCPPort(t), freeTCPPort(t)
	startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "flv", "-listen", "1",
		fmt.Sprintf("rtmp://127.0.0.1:%d/live/s", servePort))
	recFile := filepath.Join(t.TempDir(), "rec.flv")
	rec := startFFmpeg(t, ffmpeg, "-listen", "1", "-i", fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", recPort, recKey),
		"-c", "copy", "-f", "flv", recFile)
	waitListening(t, servePort)
	waitListening(t, recPort)
	serveFront := testpeer.TLSFront(t, fmt.Sprintf("127.0.0.1:%d", servePort), m.ca.Server(t))
	recFront := testpeer.TLSFront(t, fmt.Sprintf("127.0.0.1:%d", recPort), m.ca.Server(t))

	// (a) and (b): sink first, so that cam's push finds its route; relay,
	// viars and ffpull once cam is live.
	sinkPush := "rtmps://" + m.addrs.RTMPS + "/in/" + m5c1Key
	recPush := "rtmps://" + recFront + "/live/" + recKey
	m.create(t, hls("sink", sinkPush+"?mode=listen"))
	cam := m5aSpec("cam", src)
	cam.Outputs.Push = []spec.Push{{URL: sinkPush}, {URL: recPush}}
	m.create(t, cam)
	m.running(t, "cam")
	camTap := m.tap(t, "cam")
	live := time.Now()
	m.create(t, hls("relay", m.tlsURL(stream.OutRTMP, "cam")))
	m.create(t, hls("viars", m.tlsURL(stream.OutRTSP, "cam")))
	m.create(t, hls("ffpull", "rtmps://"+serveFront+"/live/s"))
	fed := []string{"relay", "viars", "sink"}
	taps := map[string]*hubRecorder{}
	for _, name := range slices.Concat(fed, []string{"ffpull"}) {
		m.hub(t, name, live)
		taps[name] = m.tap(t, name)
	}
	var pushes []stream.Push
	m5aUntil(t, m5aWait, func() bool {
		pushes = m.status(t, "cam").Pushes
		return len(pushes) == 2 && pushes[0].State == "publishing" && pushes[1].State == "publishing"
	}, "both of cam's pushes publish: %+v", &pushes)
	published := time.Now()

	// (c) MediaMTX, while the taps record.
	mtxDone := make(chan struct{})
	if mtx := os.Getenv(testpeer.EnvMediaMTXRTSP); mtx != "" {
		testpeer.RequireMuxer(t, ffmpeg, "rtsp")
		path := mtxPath("m5c1")
		rtspPublish(t, ffmpeg, loopClip(t, ffmpeg, "h264_aac.ts", 10, 0), "rtsp://"+mtx+"/"+path)
		settleRTSP(t, "rtsp://"+mtx+"/"+path+"?transport=tcp")
		front := testpeer.TLSFront(t, mtx, m.ca.Server(t))
		at := time.Now()
		m.create(t, hls("mtx", "rtsps://"+front+"/"+path))
		m.hub(t, "mtx", at)
		close(mtxDone)
	} else {
		t.Logf("(c) is not run: %s is not set", testpeer.EnvMediaMTXRTSP)
	}

	// (d) Untrusted: a front of the plain RTMP listener with a leaf of
	// another CA. The stream it plays is named like a key, so that the
	// redaction of the pull's URL can be seen.
	const feed, password = "feedkey-0123456789", "hunter2-pw"
	feedSpec := spec.Stream{Name: feed, Inputs: []spec.Input{{URL: src}}, Outputs: spec.Outputs{RTMP: true}}
	m.create(t, feedSpec)
	m.running(t, feed)
	badFront := testpeer.TLSFront(t, m.addrs.RTMP, tlstest.NewCA(t).Server(t))
	badURL := "rtmps://viewer:" + password + "@" + badFront + "/live/" + feed
	m.create(t, hls("bad", badURL))
	var bad stream.Status
	m5aUntil(t, m5aWait, func() bool {
		bad = m.status(t, "bad")
		return strings.Contains(bad.LastError, "x509")
	}, "bad's status carries the x509 reason: %+v", &bad)
	firstAt := bad.LastErrorAt
	require.NotNil(t, firstAt)
	// It stays in its retry loop: a later attempt fails the same way.
	m5aUntil(t, m5aWait, func() bool {
		bad = m.status(t, "bad")
		return bad.LastErrorAt != nil && bad.LastErrorAt.After(*firstAt)
	}, "bad tries again: %+v", &bad)
	assert.NotEqual(t, "running", bad.State)
	assert.Empty(t, bad.Tracks)
	assert.Contains(t, bad.LastError, "x509")
	code, body := m.get(t, "/api/v1/streams/bad/status")
	require.Equal(t, http.StatusOK, code)
	for _, secret := range []string{feed, password} {
		assert.NotContains(t, string(body), secret, "bad's status")
		assert.NotContains(t, m.logs.String(), password, "the log")
	}
	t.Logf("bad: %s", bad.LastError)
	// The same URL with +insecure plays.
	at := time.Now()
	code, _ = m.put(t, hls("bad", strings.Replace(badURL, "rtmps://", "rtmps+insecure://", 1)))
	require.Equal(t, http.StatusOK, code)
	m.hub(t, "bad", at)

	// (a): each hub shares its frames with cam's.
	time.Sleep(time.Until(live.Add(m5c1Hub / 2)))
	camFrames := m5b1Frames(camTap)
	for _, name := range fed {
		frames := m5b1Frames(taps[name])
		for _, kind := range []media.Kind{media.KindVideo, media.KindAudio} {
			want, got := m5c1Payloads(camFrames, kind), m5c1Payloads(frames, kind)
			assert.GreaterOrEqual(t, m5c1Overlap(want, got), m5c1Shared,
				"%s, kind %d: %d frames, cam has %d", name, kind, len(got), len(want))
		}
		t.Logf("%-6s %s; cam %s", name, kindCounts(frames), kindCounts(camFrames))
	}
	ff := m5b1Frames(taps["ffpull"])
	assert.GreaterOrEqual(t, len(m5c1Payloads(ff, media.KindVideo)), m5c1Shared, "ffpull's video")
	assert.GreaterOrEqual(t, len(m5c1Payloads(ff, media.KindAudio)), m5c1Shared, "ffpull's audio")

	// (b): the recorder took cam's push. Taking the push away ends its
	// connection, and ffmpeg closes its file.
	time.Sleep(time.Until(published.Add(4 * time.Second)))
	cam.Outputs.Push = cam.Outputs.Push[:1]
	code, applied := m.put(t, cam)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "outputs", applied)
	select {
	case <-rec.done:
	case <-time.After(m5aRead):
		require.Fail(t, "the recorder did not end with the push")
	}
	counts := m5c1Packets(t, ffprobe, recFile)
	assert.GreaterOrEqual(t, counts["video"], m5c1Shared, "video packets in the recording (ffmpeg: %v %s)", rec.err, rec.stderr.String())
	assert.GreaterOrEqual(t, counts["audio"], m5c1Shared, "audio packets in the recording")
	t.Logf("the recording: %v", counts)
	select {
	case <-mtxDone:
		assert.Equal(t, "running", m.status(t, "mtx").State)
	default:
	}
}

// m5c1Serial is the serial of the first certificate of a PEM file.
func m5c1Serial(t *testing.T, certFile string) *big.Int {
	t.Helper()
	return m5c1Leaf(t, certFile).SerialNumber
}

// m5c1Leaf is the first certificate of a PEM file.
func m5c1Leaf(t *testing.T, certFile string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(certFile)
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

// m5c1Replace writes data to a temporary name beside path and renames it
// over path, as a rotation tool does: no reader sees half a file.
func m5c1Replace(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, data, 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// m5c1Dial is a TLS connection to addr, its handshake done, within m5aRead.
func m5c1Dial(t *testing.T, addr string, c *tls.Config) (*tls.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), m5aRead)
	defer cancel()
	conn, err := (&tls.Dialer{Config: c}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller reports it
	}
	return conn.(*tls.Conn), nil
}

// serves reports whether a new handshake with each of the four TLS
// listeners is presented the certificate of serial.
func (m *m5c1) serves(t *testing.T, serial *big.Int) bool {
	t.Helper()
	all := true
	for what, addr := range map[string]string{
		"https": m.addrs.HTTPS, "api": m.addrs.APITLS, "rtmps": m.addrs.RTMPS, "rtsps": m.addrs.RTSPS,
	} {
		conn, err := m5c1Dial(t, addr, m.ca.Client())
		require.NoError(t, err, what)
		got := conn.ConnectionState().PeerCertificates[0].SerialNumber
		_ = conn.Close()
		all = all && got.Cmp(serial) == 0
	}
	return all
}

// m5c1Series are the series of one family in a scrape, by their labels.
func m5c1Series(samples []metricstest.Sample, name string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		keys := make([]string, 0, len(s.Labels))
		for k, v := range s.Labels {
			keys = append(keys, k+"="+v)
		}
		slices.Sort(keys)
		out[strings.Join(keys, ",")] = s.Value
	}
	return out
}

// TestM5c1Rotate rotates the certificate under a row of session readers
// that came through the TLS listeners: HTTP-TS over HTTPS, RTMPS and RTSPS.
// A new pair is written: within 5 s a new handshake with each of the four
// TLS listeners is presented the new serial, and the config status shows
// the new not_after and nothing to restart for. Then a certificate whose
// key is not there yet: the serial stays, certificate.last_error is set and
// reloads_total{result="error"} moves. Then the matching key and POST
// /api/v1/reload: the newest serial. Through all three, no
// openstreamer_output_sessions_ended_total series moved, and the row is not
// interrupted (assertUninterrupted); after it ffprobe, with the same CA,
// plays RTMPS from the certificate in force. Budget 45 s.
//
// The ffprobe row comes after the row's end, not between the rotations: a
// player that comes and goes moves the counters the row compares.
func TestM5c1Rotate(t *testing.T) {
	t.Parallel()
	defer m5c1Budget(t, time.Now(), 45*time.Second)
	ffprobe := testpeer.Require(t, "ffprobe", "rtmps")
	src := m5aSource(t)
	m := m5c1Server(t, "streams:\n"+
		"  - name: cam\n    inputs:\n      - url: \""+src+"\"\n"+
		"    outputs: {hls: true, dash: true, httpts: true, rtmp: true, rtsp: true, srt: true}\n"+
		"  - name: other\n    inputs:\n      - url: \""+src+"\"\n    outputs: {rtmp: true}\n")
	m.running(t, "other")
	row := m5aOpen(t, m.m5a, "cam", m5aRowOpt{
		sessions:  []stream.Output{stream.OutRTMP, stream.OutRTSP, stream.OutHTTPTS},
		segmented: []string{},
		tls:       m,
	})
	const ended = "openstreamer_output_sessions_ended_total"
	endedBefore := m5c1Series(m.scrape(t), ended)
	reloads := func(result string) float64 {
		return m.metric(t, "openstreamer_tls_certificate_reloads_total", "result", result)
	}
	first := m5c1Serial(t, m.cert)
	require.True(t, m.serves(t, first), "the start's pair")

	// A new pair: the poll takes it.
	certPEM, keyPEM := m.ca.Issue(t, 48*time.Hour, "127.0.0.1", "localhost")
	m5c1Replace(t, m.cert, certPEM)
	m5c1Replace(t, m.key, keyPEM)
	second := m5c1Serial(t, m.cert)
	require.NotEqual(t, first, second)
	began := time.Now()
	m5aUntil(t, 5*time.Second, func() bool { return m.serves(t, second) }, "every TLS listener presents the new serial")
	t.Logf("the new serial was served %v after it was written", time.Since(began).Round(time.Millisecond))
	st := m.configStatus(t)
	m5aUntil(t, 5*time.Second, func() bool {
		st = m.configStatus(t)
		return st.Certificate != nil && st.Certificate.LastError == ""
	}, "the certificate's status has no error: %+v", &st)
	assert.True(t, m5c1Leaf(t, m.cert).NotAfter.Equal(st.Certificate.NotAfter), "not_after is the new leaf's: %v", st.Certificate.NotAfter)
	assert.Empty(t, st.RestartRequired)

	// Only a new certificate: its key does not match, and the pair in
	// force stays.
	failed := reloads("error")
	certPEM, keyPEM = m.ca.Issue(t, 72*time.Hour, "127.0.0.1", "localhost")
	m5c1Replace(t, m.cert, certPEM)
	third := m5c1Serial(t, m.cert)
	m5aUntil(t, 5*time.Second, func() bool {
		st = m.configStatus(t)
		return st.Certificate.LastError != "" && reloads("error") > failed
	}, "a key that does not match is reported: %+v", &st)
	assert.True(t, m.serves(t, second), "the serial stays")
	for line := range strings.SplitSeq(strings.TrimSpace(string(keyPEM)), "\n") {
		if !strings.HasPrefix(line, "-----") {
			assert.NotContains(t, st.Certificate.LastError, line)
		}
	}

	// The matching key, then a reload: the newest serial at once.
	m5c1Replace(t, m.key, keyPEM)
	m.reload(t)
	assert.True(t, m.serves(t, third), "the newest serial, after POST /api/v1/reload")
	st = m.configStatus(t)
	assert.Empty(t, st.Certificate.LastError)
	assert.True(t, m5c1Leaf(t, m.cert).NotAfter.Equal(st.Certificate.NotAfter))
	assert.Empty(t, st.RestartRequired)

	// No session ended, and the row was not interrupted.
	assert.Equal(t, endedBefore, m5c1Series(m.scrape(t), ended), "no %s series moved", ended)
	row.edited()
	row.end(t)
	assertUninterrupted(t, row)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	u := m.tlsURL(stream.OutRTMP, "other")
	streams, err := m5c1Probe(ctx, ffprobe, []string{"-tls_verify", "1", "-ca_file", m.caFile}, u)
	m5c1AV(t, "ffprobe after the rotations", u, streams, err)
}

// m5c1Noise is a 64×64 PNG of pixels that do not compress: a few KiB.
func m5c1Noise() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	x := uint32(0x9e3779b9)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = byte(x)
	}
	for y := range 64 {
		for x := range 64 {
			c := img.NRGBAAt(x, y)
			img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, 0xff})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err) // an in-memory encode of a valid image does not fail
	}
	return b.Bytes()
}

// slowPut uploads an asset over the API's TLS listener as the editor,
// writing the body chunk bytes at a time with a pause after each, on a
// connection of its own.
func (m *m5c1) slowPut(t *testing.T, name string, body []byte, chunk int, pause time.Duration) m5aResp {
	t.Helper()
	conn, err := m5c1Dial(t, m.addrs.APITLS, m.ca.Client())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Minute)))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://"+m.addrs.APITLS+"/api/v1/assets/"+name, nil)
	require.NoError(t, err)
	req.SetBasicAuth(m5aEditor, m5aEditorPW)
	head := "PUT " + req.URL.Path + " HTTP/1.1\r\nHost: " + m.addrs.APITLS + "\r\nAuthorization: " + req.Header.Get("Authorization") +
		"\r\nX-Requested-By: e2e\r\nContent-Type: application/octet-stream\r\nContent-Length: " + strconv.Itoa(len(body)) +
		"\r\nConnection: close\r\n\r\n"
	_, err = conn.Write([]byte(head))
	require.NoError(t, err)
	for i := 0; i < len(body); i += chunk {
		_, err = conn.Write(body[i:min(i+chunk, len(body))])
		require.NoError(t, err)
		time.Sleep(pause)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	var b bytes.Buffer
	_, err = b.ReadFrom(res.Body)
	require.NoError(t, err)
	return m5aResp{code: res.StatusCode, body: b.Bytes(), header: res.Header}
}

// TestM5c1API drives the API over its TLS listener, and the same requests
// over the plain one on loopback: the editor creates a stream (201) and
// reads it back; the viewer's PUT is 403; no credentials is 401; a write
// without X-Requested-By is 400; GET /api/v1/config returns the text with
// the certificate's paths, and the editor's ETag round-trips a PUT. A plain
// HTTP request to the API's TLS port is 400, with net/http's "client sent
// an HTTP request to an HTTPS server". An image asset uploaded over HTTPS
// in small chunks with pauses, well inside its size-based deadline (M5b1's
// R-E6), is 201 and reads back the same bytes. (The stalled upload is
// TestAssetDeadlinesTLS, where the deadlines can be scaled.)
func TestM5c1API(t *testing.T) {
	t.Parallel()
	m := m5c1Server(t, "")
	src := m5aSource(t)
	for _, c := range []struct {
		what, name string
		ops        *m5a
	}{
		{"https", "overtls", m.secure()},
		{"plain", "overplain", m.m5a},
	} {
		t.Run(c.what, func(t *testing.T) {
			ops, name := c.ops, c.name
			sp := spec.Stream{Name: name, Inputs: []spec.Input{{URL: src}}, Outputs: spec.Outputs{HLS: true}}
			body, err := json.Marshal(sp)
			require.NoError(t, err)
			path := "/api/v1/streams/" + name
			json1 := []string{"Content-Type", "application/json"}

			// Who may do what, before the stream is there.
			eye, none := ops.as(m5aViewer, m5aViewerPW), ops.as("", "")
			assert.Equal(t, http.StatusForbidden, eye.do(t, http.MethodPut, path, body, json1...).code, "the viewer's PUT")
			resp := none.do(t, http.MethodPut, path, body, json1...)
			assert.Equal(t, http.StatusUnauthorized, resp.code, "no credentials")
			assert.Equal(t, `Basic realm="openstreamer"`, resp.header.Get("WWW-Authenticate"))
			assert.Equal(t, http.StatusUnauthorized, none.do(t, http.MethodGet, "/api/v1/streams", nil).code, "no credentials")
			base := ops.base
			if base == "" {
				base = "http://" + m.addrs.API
			}
			resp = m5aDoWith(t, m.api, http.MethodPut, base+path, m5aEditor, m5aEditorPW, body, json1...)
			assert.Equal(t, http.StatusBadRequest, resp.code, "a write without X-Requested-By: %s", resp.body)
			assert.Equal(t, http.StatusNotFound, ops.do(t, http.MethodGet, path, nil).code, "nothing was created")

			// The editor creates the stream and reads it back.
			code, applied := ops.put(t, sp)
			require.Equal(t, http.StatusCreated, code)
			assert.Equal(t, "created", applied)
			code, got := ops.get(t, path)
			require.Equal(t, http.StatusOK, code)
			var back spec.Stream
			require.NoError(t, json.Unmarshal(got, &back), "%s", got)
			assert.Equal(t, name, back.Name)
			require.Len(t, back.Inputs, 1)
			assert.Equal(t, src, back.Inputs[0].URL)
			assert.True(t, back.Outputs.HLS)
			assert.Equal(t, http.StatusOK, eye.do(t, http.MethodGet, path, nil).code, "the viewer reads it")
			ops.running(t, name)

			// The config's text names the pair, and its ETag round-trips.
			text, etag := ops.configText(t)
			assert.Contains(t, text, m.cert)
			assert.Contains(t, text, m.key)
			assert.Contains(t, text, "tls_listen")
			require.NotEmpty(t, etag)
			code, _ = ops.putConfig(t, text+"# An edit over "+c.what+".\n", etag)
			assert.Equal(t, http.StatusOK, code, "PUT config with the ETag")
			assert.Equal(t, http.StatusPreconditionFailed, ops.putConfigRaw(t, text, etag).code, "the old ETag")
			assert.Equal(t, http.StatusForbidden, eye.do(t, http.MethodGet, "/api/v1/config", nil).code, "the viewer's GET config")
			st := ops.configStatus(t)
			require.NotNil(t, st.Certificate)
			assert.ElementsMatch(t, []string{"127.0.0.1", "localhost"}, st.Certificate.Names)
			assert.Empty(t, st.RestartRequired)
		})
	}

	// Plain HTTP on the TLS port.
	resp := m5aDo(t, http.MethodGet, "http://"+m.addrs.APITLS+"/healthz", "", "", nil)
	assert.Equal(t, http.StatusBadRequest, resp.code)
	assert.Contains(t, strings.ToLower(string(resp.body)), "client sent an http request to an https server")

	// An asset over TLS, written slowly.
	img := m5c1Noise()
	require.Greater(t, len(img), 4<<10, "the image is several chunks")
	began := time.Now()
	resp = m.slowPut(t, "logo.png", img, 512, 20*time.Millisecond)
	require.Equal(t, http.StatusCreated, resp.code, "%s", resp.body)
	t.Logf("%d bytes uploaded in %v", len(img), time.Since(began).Round(time.Millisecond))
	ops := m.secure()
	back := m5b1Asset(t, ops, http.MethodGet, "logo.png", nil)
	require.Equal(t, http.StatusOK, back.code)
	assert.Equal(t, img, back.body, "GET returns the bytes that were sent")
	assert.Equal(t, img, m5b1Asset(t, m.m5a, http.MethodGet, "logo.png", nil).body, "over the plain listener too")
}

// TestM5c1Players plays cam over TLS with VLC and GStreamer (the amd64 test
// job only).
//
//   - VLC plays https://…/cam/hls/index.m3u8 and cmaf/master.m3u8, trusting
//     only the directory that holds the CA (--gnutls-dir-trust,
//     --no-gnutls-system-trust), by vlcPlays' rule.
//   - GStreamer plays rtsps://…/live/cam with rtspPipeline and
//     tls-validation-flags=0, by gstPlays' rule.
//   - GStreamer plays rtmps://…/live/cam with rtmp2src
//     (tls-validation-flags=0) ! flvdemux, by gstPlays' rule.
//   - GStreamer reads https://…/cam/stream.ts with souphttpsrc ! tsdemux,
//     video and audio to decoders and fakesinks, by gstPlays' rule, without
//     verifying the server (ssl-strict=false).
//
// Known limits of the peers, which leave rows out:
//   - VLC 3.0.20 has no RTSP, and gst-launch-1.0 cannot give GStreamer's HLS
//     client a CA.
//   - GStreamer cannot verify the HTTP-TS read against a CA file:
//     souphttpsrc's ssl-ca-file is read only with libsoup 2. With libsoup 3
//     (GStreamer 1.24.2 with libsoup 3.4.4 in the CI image) the element
//     verifies against the system's store alone, so ssl-ca-file=<the CA>
//     ssl-strict=true fails with "Unacceptable TLS certificate" for a chain
//     that openssl verifies, and plays once the CA is in the system's
//     store, which a test cannot write.
//
// The source is one pass of a clip, not a loop: VLC 3 cannot play past an
// fMP4 discontinuity (TestM3VLCClass, ruling R-T7a).
func TestM5c1Players(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	src := fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 30, 0), "")
	m := m5c1Server(t, "streams:\n  - name: cam\n    inputs:\n      - url: \""+src+"\"\n"+
		"    outputs: {hls: true, dash: true, httpts: true, rtmp: true, rtsp: true}\n")
	m.running(t, "cam")
	// A window of five segments, as TestM3VLCClass waits for.
	m.listed(t, "cam", 5)
	dirs := []string{m.caDir}

	t.Run("vlc", func(t *testing.T) {
		t.Parallel()
		ffprobe := testpeer.Require(t, "ffprobe")
		trust := []string{"--gnutls-dir-trust", m.caDir, "--no-gnutls-system-trust"}
		for _, row := range []struct {
			name, path string
			modules    []string
		}{
			{"hls-ts", "hls/index.m3u8", []string{"adaptive", "ts", "avcodec", "gnutls"}},
			{"hls-fmp4", "cmaf/master.m3u8", []string{"adaptive", "mp4", "avcodec", "gnutls"}},
		} {
			t.Run(row.name, func(t *testing.T) {
				vlc := testpeer.VLC(t, row.modules...)
				vlcPlaysWith(t, vlc, ffprobe, dirs, trust, m.tlsLive("cam")+row.path, true)
			})
		}
		// The trust directory is what verifies: with an empty one, and
		// without the system's store, VLC plays nothing.
		t.Run("hls-ts, an empty trust directory", func(t *testing.T) {
			vlc := testpeer.VLC(t, "adaptive", "ts", "avcodec", "gnutls")
			empty := filepath.Join(m.caDir, "empty")
			require.NoError(t, os.Mkdir(empty, 0o755)) //nolint:gosec // a peer in the image reads it as another user
			v := startVLC(t, vlc, dirs, "--gnutls-dir-trust", empty, "--no-gnutls-system-trust", m.tlsURL(stream.OutHLS, "cam"))
			time.Sleep(vlcStatsAfter)
			st, _ := v.stats(t)
			v.quit(t)
			assert.Zero(t, st.video, "video decoded from a server VLC cannot verify")
			assert.Zero(t, st.audio, "audio decoded from a server VLC cannot verify")
			assert.Contains(t, v.log.String(), "Certificate verification failure", "VLC names the refusal")
		})
	})

	t.Run("gstreamer", func(t *testing.T) {
		t.Parallel()
		t.Run("rtsps", func(t *testing.T) {
			gst := testpeer.GStreamer(t, rtspElements...)
			p := rtspPipeline(m.tlsURL(stream.OutRTSP, "cam"))
			gstPlays(t, gst, slices.Insert(p, 2, "tls-validation-flags=0"))
		})
		t.Run("rtmps", func(t *testing.T) {
			gst := testpeer.GStreamer(t, "rtmp2src", "flvdemux", "h264parse", "aacparse", "avdec_h264", "avdec_aac", "queue", "fakesink")
			gstPlays(t, gst, []string{
				"rtmp2src", "location=" + m.tlsURL(stream.OutRTMP, "cam"), "tls-validation-flags=0",
				"!", "flvdemux", "name=d",
				"d.video", "!", "h264parse", "!", "avdec_h264", "!", "queue", "!", "fakesink", "name=v", "silent=false",
				"d.audio", "!", "aacparse", "!", "avdec_aac", "!", "queue", "!", "fakesink", "name=a", "silent=false",
			})
		})
		t.Run("https http-ts, unverified", func(t *testing.T) {
			gst := testpeer.GStreamer(t, "souphttpsrc", "tsdemux", "h264parse", "aacparse", "avdec_h264", "avdec_aac", "queue", "fakesink")
			gstPlays(t, gst, []string{
				"souphttpsrc", "location=" + m.tlsURL(stream.OutHTTPTS, "cam"), "ssl-strict=false",
				"!", "tsdemux", "name=d",
				"d.", "!", "video/x-h264", "!", "h264parse", "!", "avdec_h264", "!", "queue", "!", "fakesink", "name=v", "silent=false",
				"d.", "!", "audio/mpeg", "!", "aacparse", "!", "avdec_aac", "!", "queue", "!", "fakesink", "name=a", "silent=false",
			})
		})
	})
}

// TestM5c1NoSecretsLeak is TestM5aNoSecretsLeak's scan with the TLS forms
// holding the secrets: an RTMPS listen input's key; an rtmps+insecure pull
// with user:password and ?token=; an rtsps pull with user:password; and an
// RTMPS push with a key and ?token= to a front whose leaf is from another
// CA, so that it fails (a push's failure is in its status; the inputs'
// failures are logged). The certificate is then left half rotated, so that
// its reload fails and is logged too. No secret is in the
// log, in /metrics, in any GET as the viewer over HTTPS or in any status,
// and neither is a PEM marker of a key ("PRIVATE KEY") or a line of either
// key file.
func TestM5c1NoSecretsLeak(t *testing.T) {
	t.Parallel()
	const (
		listenKey = "listenkey-0123456789"
		pullKey   = "pullkey-0123456789"
		pullPW    = "hunter2-pw"
		pullToken = "pulltoken-0123456789"
		rtspPW    = "rtspspass-0123456789"
		pushKey   = "pushkey-0123456789"
		pushToken = "pushtoken-0123456789"
	)
	m := m5c1Server(t, "")
	src := m5aSource(t)
	// Fronts that present a leaf of another CA: the push and the rtsps pull
	// reach TLS and fail verification, with their secrets in the URL.
	other := tlstest.NewCA(t)
	badFront := testpeer.TLSFront(t, m.addrs.RTMP, other.Server(t))
	badRTSPS := testpeer.TLSFront(t, m.addrs.RTSP, other.Server(t))
	listenInput := "rtmps://" + m.addrs.RTMPS + "/in/" + listenKey + "?mode=listen"
	pull := "rtmps+insecure://user:" + pullPW + "@127.0.0.1:1/live/" + pullKey + "?token=" + pullToken
	rtspPull := "rtsps://user:" + rtspPW + "@" + badRTSPS + "/live/cam3"
	push := "rtmps://" + badFront + "/live/" + pushKey + "?token=" + pushToken

	m.create(t, m5aSpec("lis", listenInput))
	cam2 := m5aSpec("cam2", pull)
	cam2.Inputs = append(cam2.Inputs, spec.Input{URL: src})
	cam2.Outputs.Push = []spec.Push{{URL: push}}
	m.create(t, cam2)
	cam3 := m5aSpec("cam3", rtspPull)
	cam3.Inputs = append(cam3.Inputs, spec.Input{URL: src})
	m.create(t, cam3)
	var st2, st3 stream.Status
	m5aUntil(t, m5aWait, func() bool {
		st2, st3 = m.status(t, "cam2"), m.status(t, "cam3")
		return st2.LastError != "" && st2.ActiveInput == 1 && len(st2.Pushes) == 1 && st2.Pushes[0].LastError != "" &&
			st3.LastError != "" && st3.ActiveInput == 1
	}, "cam2 and cam3 run on their second inputs, with their failures as last errors: %+v %+v", &st2, &st3)
	assert.Contains(t, st2.Pushes[0].LastError, "x509", "the push failed on the front's certificate")
	assert.Contains(t, st3.LastError, "x509", "the rtsps pull failed on the front's certificate")
	assert.NotContains(t, st3.LastError, rtspPW)

	// Half a rotation: a new certificate whose key is not there.
	oldKey, err := os.ReadFile(m.key)
	require.NoError(t, err)
	certPEM, newKey := m.ca.Issue(t, 48*time.Hour, "127.0.0.1", "localhost")
	m5c1Replace(t, m.cert, certPEM)
	cs := m.configStatus(t)
	m5aUntil(t, 5*time.Second, func() bool {
		cs = m.configStatus(t)
		return cs.Certificate != nil && cs.Certificate.LastError != ""
	}, "the certificate's reload failed: %+v", &cs)

	secrets := []string{listenKey, pullKey, pullPW, pullToken, rtspPW, pushKey, pushToken, "PRIVATE KEY"}
	for _, key := range [][]byte{oldKey, newKey} {
		for line := range strings.SplitSeq(strings.TrimSpace(string(key)), "\n") {
			if !strings.HasPrefix(line, "-----") {
				secrets = append(secrets, line)
			}
		}
	}
	userSecrets := []string{m5aEditorPW, m5aViewerPW, m5aHash(m5aEditorPW), m5aHash(m5aViewerPW)}
	all := slices.Concat(secrets, userSecrets)
	clean := func(what, text string, secrets []string) {
		t.Helper()
		for _, s := range secrets {
			assert.NotContains(t, text, s, what)
		}
	}
	ops := m.secure()
	eye := ops.as(m5aViewer, m5aViewerPW)
	// A second stream on lis's listen address and publish path: the
	// conflict's text names the address, never the path.
	dup, err := json.Marshal(m5aSpec("dup", listenInput))
	require.NoError(t, err)
	// A URL that cannot be an input, carrying a password: +insecure is
	// refused on a listen input.
	bad, err := json.Marshal(m5aSpec("bad", "rtmps+insecure://user:"+pullPW+"@"+m.addrs.RTMPS+"/in/"+pullKey+"?mode=listen"))
	require.NoError(t, err)
	names := []string{"lis", "cam2", "cam3"}
	for _, who := range []*m5a{eye, ops} {
		paths := make([]string, 0, 3+2*len(names))
		paths = append(paths, "/metrics", "/api/v1/streams", "/api/v1/config/status")
		for _, name := range names {
			paths = append(paths, "/api/v1/streams/"+name, "/api/v1/streams/"+name+"/status")
		}
		for _, path := range paths {
			code, body := who.get(t, path)
			require.Equal(t, http.StatusOK, code, "%s as %s", path, who.user)
			clean(path+" as "+who.user, string(body), all)
		}
		for _, c := range []struct {
			name string
			body []byte
			want int
		}{{"dup", dup, http.StatusConflict}, {"bad", bad, http.StatusUnprocessableEntity}} {
			resp := who.do(t, http.MethodPut, "/api/v1/streams/"+c.name, c.body, "Content-Type", "application/json")
			want := c.want
			if who == eye {
				want = http.StatusForbidden
			}
			assert.Equal(t, want, resp.code, "the PUT of %s as %s: %s", c.name, who.user, resp.body)
			clean("the answer to the PUT of "+c.name+" as "+who.user, string(resp.body), all)
		}
	}
	assert.NotEqual(t, "running", m.status(t, "lis").State, "lis waits for a publisher")

	logs := m.logs.String()
	clean("the log", logs, all)
	assert.Contains(t, logs, "rtsps://user:xxxxx@"+badRTSPS+"/live/cam3: rtsp: dial: tls: tls: failed to verify certificate: x509:",
		"the rtsps pull's x509 failure is in the log, its password redacted")
	assert.Contains(t, logs, "server: tls: reload failed", "the failed reload is in the log")

	// The deliberate exceptions, for the editor only: the stream secrets.
	for name, own := range map[string][]string{
		"lis": {listenKey}, "cam2": {pullKey, pullPW, pullToken, pushKey, pushToken}, "cam3": {rtspPW},
	} {
		code, body := ops.get(t, "/api/v1/streams/"+name+"?secrets=1")
		require.Equal(t, http.StatusOK, code)
		for _, s := range own {
			assert.Contains(t, string(body), s, "GET %s?secrets=1 as the editor", name)
		}
		clean("GET "+name+"?secrets=1", string(body), slices.Concat(userSecrets, secrets[7:]))
		assert.Equal(t, http.StatusForbidden, eye.do(t, http.MethodGet, "/api/v1/streams/"+name+"?secrets=1", nil).code)
	}
	// The config's text holds the key file's path, never what it holds.
	text, _ := ops.configText(t)
	assert.Contains(t, text, m.key)
	clean("GET /api/v1/config", text, secrets[7:])
}
