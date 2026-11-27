package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/server"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// The M5a scenarios run the whole server (internal/server) from one config
// file and drive it over its API, with Basic credentials: they prove M5's
// done criterion, "a spec created over the API serves every output; editing
// outputs does not interrupt other outputs". The only in-process access is
// the tap (a lossless subscriber of the hub a stream serves, which the
// captures are compared with) and the packager's identity; a hand edit is a
// write to the file.
//
// The source of every stream is testdata/fixtures/h264_aac.ts as a looping
// file input: it is paced in real time, and each 6 s loop is one epoch
// change, which every reader must survive too.

const (
	// m5aIter is the iteration count of the scenarios' password hashes; the
	// servers are started with Options.RequiredIterations set to it.
	m5aIter = 1000
	// The API users of every scenario's config.
	m5aEditor   = "ops"
	m5aEditorPW = "ops-password-for-e2e"
	m5aViewer   = "eye"
	m5aViewerPW = "eye-password-for-e2e"
	// m5aSinkKey is the stream key of the push from cam to sink.
	m5aSinkKey = "k0123456789"
	// m5aWait bounds every wait for the server to reach a state.
	m5aWait = 20 * time.Second
	// m5aRead bounds every set-up and every stall of a reader.
	m5aRead = 15 * time.Second
	// m5aSegmented is the media a segmented reader of a row captures. The
	// edits come 5 s and 10 s after the readers start and the window may
	// already list 12 s, so a reader returns 14 s to 26 s after its start:
	// always after the second edit. The rows do not rely on that: they
	// record when each reader returned.
	m5aSegmented = 26 * time.Second
	// m5aEdit is the time from the readers' start to the first edit, and
	// from the first edit to the second.
	m5aEdit = 5 * time.Second
	// m5aOwnEnds is how long a session reader of an output has to end
	// after the edit that turned the output off.
	m5aOwnEnds = 2 * time.Second
)

// The three segmented formats, by the name the rows give them.
const (
	m5aHLSTS   = "hls-ts"
	m5aHLSFMP4 = "hls-fmp4"
	m5aDASH    = "dash"
)

// m5aFormats are the segmented formats in the order they are checked, with
// the output each belongs to and its file under /live/<name>/.
var m5aFormats = []struct {
	name string
	out  stream.Output
	file string
}{
	{m5aHLSTS, stream.OutHLS, "hls/index.m3u8"},
	{m5aHLSFMP4, stream.OutHLS, "cmaf/master.m3u8"},
	{m5aDASH, stream.OutDASH, "cmaf/index.mpd"},
}

// m5aLog is a server's captured log.
type m5aLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *m5aLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *m5aLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// m5a is a running server and the credentials its helpers use.
type m5a struct {
	srv            *server.Server
	addrs          server.Addrs
	path           string // the config file
	logs           *m5aLog
	user, password string
	stop           func() // cancels Run and waits for it; idempotent
	// base is the API's origin and hc the client of the API requests; ""
	// and nil mean the plain listener and m5aClient (the M5c1 scenarios
	// set them to the TLS listener and a client that trusts its CA).
	base string
	hc   *http.Client
}

// m5aHash is the hash of password at the scenarios' iteration count.
func m5aHash(password string) string {
	return auth.NewHash(password, []byte("0123456789abcdef"), m5aIter).String()
}

// m5aConfig is the scenarios' config: a commented template with every
// listener on 127.0.0.1:0, the users ops (editor) and eye (viewer), the
// transcoder when one is given, and the streams section as given ("" means
// an empty list).
func m5aConfig(streams, transcoder string) string {
	if streams == "" {
		streams = "streams: []\n"
	}
	tr := ""
	if transcoder != "" {
		tr = "  # The transcoder of the streams with a ladder.\n  transcoder: {binary: \"" + transcoder + "\"}\n"
	}
	return "# The config of an M5a scenario. Every listener is on an ephemeral\n" +
		"# loopback port; the server rewrites this file when the API edits it.\n" +
		"version: 1\n\n" +
		"# The listeners.\n" +
		"server:\n" +
		"  # HLS, DASH and HTTP-TS.\n" +
		"  http: {listen: \"127.0.0.1:0\"}\n" +
		"  # The API, the metrics and pprof.\n" +
		"  api: {listen: \"127.0.0.1:0\"}\n" +
		"  rtmp: {listen: \"127.0.0.1:0\"}\n" +
		"  rtsp: {listen: \"127.0.0.1:0\"}\n" +
		"  srt: {listen: \"127.0.0.1:0\"}\n" +
		tr +
		"  log: {level: info, format: json}\n\n" +
		"# The API's users.\n" +
		"users:\n" +
		"  - name: " + m5aEditor + "\n    role: editor\n    password: \"" + m5aHash(m5aEditorPW) + "\"\n" +
		"  - name: " + m5aViewer + "\n    role: viewer\n    password: \"" + m5aHash(m5aViewerPW) + "\"\n\n" +
		"# The streams.\n" +
		streams
}

// m5aServer writes the scenarios' config with the given streams section
// into a new directory, mode 0600, starts a server from it and stops it
// when the test ends. The helpers of the result act as the editor.
func m5aServer(t *testing.T, streams string) *m5a {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(m5aConfig(streams, "")), 0o600))
	return m5aStart(t, path)
}

// m5aStart starts a server from the config file path and stops it when the
// test ends, or earlier through its stop.
func m5aStart(t *testing.T, path string) *m5a {
	t.Helper()
	return m5aStartWith(t, path, server.Options{})
}

// m5aStartWith is m5aStart with the options a scenario adds (the
// transcoder's environment, say): Version, LogOut and RequiredIterations
// are the scenarios' own whatever opt holds.
func m5aStartWith(t *testing.T, path string, opt server.Options) *m5a {
	t.Helper()
	logs := &m5aLog{}
	opt.Version, opt.LogOut, opt.RequiredIterations = "e2e", logs, m5aIter
	srv, err := server.New(path, opt)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				assert.NoError(t, err, "Run")
			case <-time.After(m5aWait):
				assert.Fail(t, "Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	return &m5a{srv: srv, addrs: srv.Addrs(), path: path, logs: logs, user: m5aEditor, password: m5aEditorPW, stop: stop}
}

// as returns the same server with other credentials; an empty user sends
// none.
func (m *m5a) as(user, password string) *m5a {
	c := *m
	c.user, c.password = user, password
	return &c
}

// m5aResp is one HTTP answer.
type m5aResp struct {
	code   int
	body   []byte
	header http.Header
}

// m5aClient makes the API and the media requests. A request that waits for
// a rebuild, a pprof profile of 1 s and a segment all fit in its timeout.
var m5aClient = &http.Client{Timeout: time.Minute}

// m5aDo sends one request. hdr are header pairs; user "" sends no
// credentials.
func m5aDo(t *testing.T, method, u, user, password string, body []byte, hdr ...string) m5aResp {
	t.Helper()
	return m5aDoWith(t, m5aClient, method, u, user, password, body, hdr...)
}

// m5aDoWith is m5aDo through the client c.
func m5aDoWith(t *testing.T, c *http.Client, method, u, user, password string, body []byte, hdr ...string) m5aResp {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, u, bytes.NewReader(body))
	require.NoError(t, err)
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return m5aResp{code: res.StatusCode, body: b, header: res.Header}
}

// do sends one API request with the helper's credentials, and
// X-Requested-By on a write.
func (m *m5a) do(t *testing.T, method, path string, body []byte, hdr ...string) m5aResp {
	t.Helper()
	if method != http.MethodGet {
		hdr = append([]string{"X-Requested-By", "e2e"}, hdr...)
	}
	base, c := m.base, m.hc
	if base == "" {
		base = "http://" + m.addrs.API
	}
	if c == nil {
		c = m5aClient
	}
	return m5aDoWith(t, c, method, base+path, m.user, m.password, body, hdr...)
}

// get reads an API path.
func (m *m5a) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	r := m.do(t, http.MethodGet, path, nil)
	return r.code, r.body
}

// put is the per-stream PUT; applied is "" unless the answer is 200 or 201.
func (m *m5a) put(t *testing.T, sp spec.Stream) (code int, applied string) {
	t.Helper()
	body, err := json.Marshal(sp)
	require.NoError(t, err)
	r := m.do(t, http.MethodPut, "/api/v1/streams/"+sp.Name, body, "Content-Type", "application/json")
	if r.code != http.StatusOK && r.code != http.StatusCreated {
		t.Logf("PUT %s: %d %s", sp.Name, r.code, r.body)
		return r.code, ""
	}
	var res struct {
		Applied string `json:"applied"`
	}
	require.NoError(t, json.Unmarshal(r.body, &res), "%s", r.body)
	return r.code, res.Applied
}

// del deletes a stream, which must succeed.
func (m *m5a) del(t *testing.T, name string) {
	t.Helper()
	r := m.do(t, http.MethodDelete, "/api/v1/streams/"+name, nil)
	require.Equal(t, http.StatusNoContent, r.code, "DELETE %s: %s", name, r.body)
}

// configText is the config in force as the editor's GET returns it, and its
// ETag.
func (m *m5a) configText(t *testing.T) (text, etag string) {
	t.Helper()
	r := m.do(t, http.MethodGet, "/api/v1/config", nil)
	require.Equal(t, http.StatusOK, r.code, "%s", r.body)
	return string(r.body), r.header.Get("ETag")
}

// putConfig is the whole-config PUT; etag "" sends no If-Match. res is
// decoded from a 200 only.
func (m *m5a) putConfig(t *testing.T, text, etag string) (code int, res api.ApplyResult) {
	t.Helper()
	r := m.putConfigRaw(t, text, etag)
	if r.code == http.StatusOK {
		require.NoError(t, json.Unmarshal(r.body, &res), "%s", r.body)
	} else {
		t.Logf("PUT config: %d %s", r.code, r.body)
	}
	return r.code, res
}

func (m *m5a) putConfigRaw(t *testing.T, text, etag string) m5aResp {
	t.Helper()
	hdr := []string{"Content-Type", "application/yaml"}
	if etag != "" {
		hdr = append(hdr, "If-Match", etag)
	}
	return m.do(t, http.MethodPut, "/api/v1/config", []byte(text), hdr...)
}

// fileText is the config file as it is on disk.
func (m *m5a) fileText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(m.path)
	require.NoError(t, err)
	return string(b)
}

// handEdit rewrites the config file on disk, as an operator's editor would.
func (m *m5a) handEdit(t *testing.T, edit func(string) string) {
	t.Helper()
	require.NoError(t, os.WriteFile(m.path, []byte(edit(m.fileText(t))), 0o600))
}

// reload is POST /api/v1/reload, which must succeed.
func (m *m5a) reload(t *testing.T) api.ApplyResult {
	t.Helper()
	r := m.do(t, http.MethodPost, "/api/v1/reload", nil)
	require.Equal(t, http.StatusOK, r.code, "reload: %s", r.body)
	var res api.ApplyResult
	require.NoError(t, json.Unmarshal(r.body, &res), "%s", r.body)
	return res
}

// configStatus is GET /api/v1/config/status.
func (m *m5a) configStatus(t *testing.T) api.ConfigStatus {
	t.Helper()
	code, body := m.get(t, "/api/v1/config/status")
	require.Equal(t, http.StatusOK, code, "%s", body)
	var st api.ConfigStatus
	require.NoError(t, json.Unmarshal(body, &st), "%s", body)
	return st
}

// status is GET /api/v1/streams/<name>/status.
func (m *m5a) status(t *testing.T, name string) stream.Status {
	t.Helper()
	code, body := m.get(t, "/api/v1/streams/"+name+"/status")
	require.Equal(t, http.StatusOK, code, "status of %s: %s", name, body)
	var st stream.Status
	require.NoError(t, json.Unmarshal(body, &st), "%s", body)
	return st
}

// scrape is one parsed scrape of /metrics.
func (m *m5a) scrape(t *testing.T) []metricstest.Sample {
	t.Helper()
	code, body := m.get(t, "/metrics")
	require.Equal(t, http.StatusOK, code, "%s", body)
	samples, err := metricstest.Parse(bytes.NewReader(body))
	require.NoError(t, err)
	return samples
}

// metric is the one series of name whose labels include the given pairs,
// in a new scrape.
func (m *m5a) metric(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	return m5aFind(t, m.scrape(t), name, labels...)
}

// m5aFind is metricstest.Find, which must find exactly one series.
func m5aFind(t *testing.T, samples []metricstest.Sample, name string, labels ...string) float64 {
	t.Helper()
	v, ok := metricstest.Find(samples, name, labels...)
	require.True(t, ok, "one series %s %v", name, labels)
	return v
}

// live is the base of a served name's files on the media listener.
func (m *m5a) live(served string) string { return "http://" + m.addrs.HTTP + "/live/" + served + "/" }

// url is the play URL of an output of a served name; HLS is the HLS-TS
// playlist (the fMP4 master is live(served) + "cmaf/master.m3u8").
func (m *m5a) url(out stream.Output, served string) string {
	switch out {
	case stream.OutHLS:
		return m.live(served) + "hls/index.m3u8"
	case stream.OutDASH:
		return m.live(served) + "cmaf/index.mpd"
	case stream.OutHTTPTS:
		return m.live(served) + "stream.ts"
	case stream.OutRTMP:
		return "rtmp://" + m.addrs.RTMP + "/live/" + served
	case stream.OutRTSP:
		return "rtsp://" + m.addrs.RTSP + "/live/" + served
	case stream.OutSRT:
		return "srt://" + m.addrs.SRT + "?streamid=live/" + served
	default:
		return ""
	}
}

// mediaGet fetches a URL of the media listener, without credentials.
func (m *m5a) mediaGet(t *testing.T, u string) m5aResp {
	t.Helper()
	return m5aDo(t, http.MethodGet, u, "", "", nil)
}

// m5aUntil polls ok every 20 ms until it holds, for at most d.
func m5aUntil(t *testing.T, d time.Duration, ok func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			require.Failf(t, "not reached in "+d.String(), format, args...)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// m5aSleepUntil sleeps until at.
func m5aSleepUntil(at time.Time) {
	if d := time.Until(at); d > 0 {
		time.Sleep(d)
	}
}

// tap records the hub that copy://served reads (the source hub of a stream,
// or a rendition's hub) without loss, from now to its end. The hub is
// polled for: a "created" answer comes back before the supervisor has
// published its first View.
func (m *m5a) tap(t *testing.T, served string) *hubRecorder {
	t.Helper()
	var h *hub.Hub
	deadline := time.Now().Add(5 * time.Second)
	for {
		var ok bool
		if h, ok = m.srv.Manager().CopyHub(served); ok {
			break
		}
		require.True(t, time.Now().Before(deadline), "no hub for %s within 5 s", served)
		time.Sleep(5 * time.Millisecond)
	}
	return recordHub(h)
}

// running waits until the stream is running with a video and an audio
// track, and returns its status.
func (m *m5a) running(t *testing.T, name string) stream.Status {
	t.Helper()
	var st stream.Status
	m5aUntil(t, m5aWait, func() bool {
		st = m.status(t, name)
		return st.State == "running" && len(st.Tracks) == 2
	}, "%s running with two tracks", name)
	return st
}

// listed waits until the segmented files of the served name's outputs
// answer and its HLS-TS playlist lists n segments (the playlist of its
// first variant, for a ladder); for a stream without HLS, until its MPD
// answers.
func (m *m5a) listed(t *testing.T, served string, n int) {
	t.Helper()
	// A served name is a stream's, or <stream>_<label>. A stream of the
	// whole name comes first, so a name that holds a "_" is not cut.
	name := served
	if _, ok := m.srv.Manager().Get(served); !ok {
		if i := strings.LastIndexByte(served, '_'); i >= 0 {
			name = served[:i]
		}
	}
	m5aUntil(t, m5aWait, func() bool {
		outs := m.status(t, name).Outputs
		for _, f := range m5aFormats {
			on := outs.HLS
			if f.out == stream.OutDASH {
				on = outs.DASH
			}
			if on && m.mediaGet(t, m.live(served)+f.file).code != http.StatusOK {
				return false
			}
		}
		if !outs.HLS {
			return true
		}
		body := m.mediaGet(t, m.url(stream.OutHLS, served)).body
		if bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
			body = m.mediaGet(t, m.live(served)+"hls/"+m5aFirstURI(body)).body
		}
		return bytes.Count(body, []byte("#EXTINF")) >= n
	}, "%s lists %d segments", served, n)
}

// m5aFirstURI is the first URI line of a playlist.
func m5aFirstURI(body []byte) string {
	for line := range strings.SplitSeq(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// m5aSource is the looping fixture as a file input.
func m5aSource(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	return fileURL(p, "loop=1")
}

// m5aSpec is a stream with every output on and no push.
func m5aSpec(name, input string) spec.Stream {
	return spec.Stream{
		Name:    name,
		Inputs:  []spec.Input{{URL: input}},
		Outputs: spec.Outputs{HLS: true, DASH: true, HTTPTS: true, RTMP: true, RTSP: true, SRT: true},
	}
}

// sinkPush is the URL cam pushes to, and sinkInput the listen input of
// sink that takes it, on the server's shared RTMP listener.
func (m *m5a) sinkPush() string  { return "rtmp://" + m.addrs.RTMP + "/in/" + m5aSinkKey }
func (m *m5a) sinkInput() string { return m.sinkPush() + "?mode=listen" }

// camAndSink creates sink (an RTMP listen input, served over HLS) and cam
// (every output, pushed to sink) with per-stream PUTs, and returns cam's
// spec.
func (m *m5a) camAndSink(t *testing.T) spec.Stream {
	t.Helper()
	sink := spec.Stream{Name: "sink", Inputs: []spec.Input{{URL: m.sinkInput()}}, Outputs: spec.Outputs{HLS: true}}
	code, applied := m.put(t, sink)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "created", applied)
	cam := m5aSpec("cam", m5aSource(t))
	cam.Outputs.Push = []spec.Push{{URL: m.sinkPush()}}
	code, applied = m.put(t, cam)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "created", applied)
	return cam
}

// editorStreams is the streams section the config-editor scenarios start
// from, with its comments: cam (every output, pushed to sink), sink, and
// other, which no edit of the scenario touches. Only cam writes its
// outputs one per line, so "      rtsp: true\n" and "      srt: true\n" are
// cam's.
func (m *m5a) editorStreams(t *testing.T) string {
	t.Helper()
	src := m5aSource(t)
	return "streams:\n" +
		"  # cam: every output, and a push to sink.\n" +
		"  - name: cam\n" +
		"    inputs:\n" +
		"      - url: \"" + src + "\"\n" +
		"    outputs:\n" +
		"      hls: true\n" +
		"      dash: true\n" +
		"      httpts: true\n" +
		"      rtmp: true\n" +
		"      # The players of rtsp://host/live/cam.\n" +
		"      rtsp: true\n" +
		"      srt: true\n" +
		"      push:\n" +
		"        - url: \"" + m.sinkPush() + "\"\n" +
		"  # sink: takes cam's push.\n" +
		"  - name: sink\n" +
		"    inputs:\n" +
		"      - url: \"" + m.sinkInput() + "\"\n" +
		"    outputs: {hls: true}\n" +
		"  # other: no edit touches it.\n" +
		"  - name: other\n" +
		"    inputs:\n" +
		"      - url: \"" + src + "\"\n" +
		"    outputs: {hls: true, rtmp: true, rtsp: true}\n"
}

// m5aSeg is what one segmented reader returned, and when.
type m5aSeg struct {
	c   conform.Capture
	err error
	at  time.Time
}

// readSegmented reads one segmented format of a served name for d of
// media (0 reads the window once), in the background.
func (m *m5a) readSegmented(format, served string, d time.Duration) <-chan m5aSeg {
	ch := make(chan m5aSeg, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		opt := conform.ReadOptions{Duration: d, Once: d == 0, Timeout: m5aRead, AlignedSequences: true}
		var (
			c   conform.Capture
			err error
		)
		switch format {
		case m5aHLSTS:
			c, err = conform.ReadHLS(ctx, m.live(served)+"hls/index.m3u8", opt)
		case m5aHLSFMP4:
			c, err = conform.ReadHLS(ctx, m.live(served)+"cmaf/master.m3u8", opt)
		case m5aDASH:
			c, err = conform.ReadDASH(ctx, m.live(served)+"cmaf/index.mpd", opt)
		default:
			err = fmt.Errorf("e2e: no segmented format %q", format)
		}
		ch <- m5aSeg{c, err, time.Now()}
	}()
	return ch
}

// readSession reads a session output of a served name for d of media
// with conform, in the background.
func (m *m5a) readSession(out stream.Output, served string, d time.Duration) <-chan capResult {
	ch := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		opt := conform.ReadOptions{Duration: d, Timeout: m5aRead}
		var (
			c   conform.Capture
			err error
		)
		switch out {
		case stream.OutRTMP:
			c, err = conform.ReadRTMP(ctx, m.url(out, served), opt)
		case stream.OutRTSP:
			c, err = conform.ReadRTSP(ctx, m.url(out, served), opt)
		case stream.OutHTTPTS, stream.OutSRT:
			c, err = conform.ReadTS(ctx, m.url(out, served), opt)
		case stream.OutHLS, stream.OutDASH:
			err = fmt.Errorf("e2e: %s is not a session output", out)
		}
		ch <- capResult{c, err}
	}()
	return ch
}

// m5aAV requires a capture without an error and without a violation that
// holds video and audio frames, and returns the number of capture epochs it holds. (A segmented read ends when one
// playlist has its span, and the audio playlist of an fMP4 master can be a
// segment ahead of the video's, so the count is over every track.)
func m5aAV(t *testing.T, what string, c conform.Capture, err error) int {
	t.Helper()
	require.NoError(t, err, what)
	for _, v := range c.Violations {
		assert.Fail(t, "a conform violation", "%s: %s", what, v)
	}
	v, _ := byKind(c.Frames, media.KindVideo)
	a, _ := byKind(c.Frames, media.KindAudio)
	require.NotEmpty(t, v, "%s: video frames", what)
	require.NotEmpty(t, a, "%s: audio frames", what)
	seen := map[uint32]bool{}
	for _, f := range c.Frames {
		seen[f.Epoch] = true
	}
	return len(seen)
}

// fresh reads 2 s of an output of a served name with a new reader, which
// must get video and audio without a violation. For HLS it reads both the
// HLS-TS playlist and the fMP4 master.
func (m *m5a) fresh(t *testing.T, out stream.Output, served string) {
	t.Helper()
	const span = 2 * time.Second
	session := func() {
		t.Helper()
		select {
		case r := <-m.readSession(out, served, span):
			m5aAV(t, "a new reader of "+string(out), r.c, r.err)
		case <-time.After(span + 2*m5aRead):
			require.Fail(t, "a new reader of "+string(out)+" did not return")
		}
	}
	segmented := func(format string) {
		t.Helper()
		select {
		case r := <-m.readSegmented(format, served, span):
			m5aAV(t, "a new reader of "+format, r.c, r.err)
		case <-time.After(span + 2*m5aRead):
			require.Fail(t, "a new reader of "+format+" did not return")
		}
	}
	switch out {
	case stream.OutHLS:
		segmented(m5aHLSTS)
		segmented(m5aHLSFMP4)
	case stream.OutDASH:
		segmented(m5aDASH)
	case stream.OutHTTPTS, stream.OutRTMP, stream.OutRTSP, stream.OutSRT:
		session()
	}
}

// refused requires that a new session of a session output of a served name
// is refused: RTSP DESCRIBE 404, RTMP play refused, SRT rejected, GET
// stream.ts 404.
func (m *m5a) refused(t *testing.T, out stream.Output, served string) { //nolint:unparam // a helper for later milestones too, whose streams have other names
	t.Helper()
	switch out {
	case stream.OutRTSP:
		_, err := describe(t, m.url(out, served))
		var se *rtsp.StatusError
		require.ErrorAs(t, err, &se, "a new RTSP session is refused")
		assert.Equal(t, "DESCRIBE", se.Method)
		assert.Equal(t, 404, se.Code)
	case stream.OutRTMP:
		ctx, cancel := context.WithTimeout(context.Background(), m5aRead)
		defer cancel()
		_, err := conform.ReadRTMP(ctx, m.url(out, served), conform.ReadOptions{Duration: time.Second, Timeout: m5aRead})
		require.Error(t, err, "a new RTMP play is refused")
		assert.Contains(t, err.Error(), "NetStream.Play.StreamNotFound")
	case stream.OutSRT:
		select {
		case r := <-rawSRT(m.addrs.SRT, "live/"+served):
			require.Error(t, r.err, "a new SRT receiver is rejected (it got %d bytes)", len(r.b))
		case <-time.After(2 * m5aRead):
			require.Fail(t, "the SRT receiver was neither accepted nor rejected")
		}
	case stream.OutHTTPTS:
		require.Equal(t, http.StatusNotFound, m.mediaGet(t, m.url(out, served)).code, "GET stream.ts")
	case stream.OutHLS, stream.OutDASH:
		require.Fail(t, "not a session output: "+string(out))
	}
}

// m5aWatch samples a stream's packager every 200 ms: the pointer the
// manager answers for one output, and the Seq of every segment in its
// window.
type m5aWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	what   string

	packs   []*live.Packager // the distinct pointers, in the order seen
	seqs    map[uint64]bool  // every Seq seen in a snapshot's window
	missing int              // samples at which the manager had no packager
	samples int
}

// watchPackager starts sampling Packager(out, served).
func (m *m5a) watchPackager(out stream.Output, served string) *m5aWatch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &m5aWatch{
		cancel: cancel, done: make(chan struct{}), what: fmt.Sprintf("Packager(%s, %s)", out, served),
		seqs: map[uint64]bool{},
	}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			w.samples++
			if p, ok := m.srv.Manager().Packager(out, served); !ok {
				w.missing++
			} else {
				if len(w.packs) == 0 || w.packs[len(w.packs)-1] != p {
					w.packs = append(w.packs, p)
				}
				// The whole window, not its newest segment: two segments
				// may close between two samples.
				for _, seg := range p.Snapshot().Segments {
					w.seqs[seg.Seq] = true
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

// end stops the sampling.
func (w *m5aWatch) end() {
	w.cancel()
	<-w.done
}

// check is rule (d) of the segmented outputs: one packager for the whole
// row, and the Seqs of the segments its windows held, taken together, one
// run without a hole.
func (w *m5aWatch) check(t *testing.T) {
	t.Helper()
	assert.Zero(t, w.missing, "%s: samples without a packager, of %d", w.what, w.samples)
	assert.Len(t, w.packs, 1, "%s: the same packager at every sample", w.what)
	seqs := slices.Sorted(maps.Keys(w.seqs))
	require.GreaterOrEqual(t, len(seqs), 5, "%s: the segments seen: %v", w.what, seqs)
	for i := 1; i < len(seqs); i++ {
		require.Equal(t, seqs[i-1]+1, seqs[i], "%s: the Seqs seen are one run: %v", w.what, seqs)
	}
}

// m5aSessions are the session outputs, in the order they are checked.
var m5aSessions = []stream.Output{stream.OutRTMP, stream.OutRTSP, stream.OutHTTPTS, stream.OutSRT}

// m5aRowOpt is what a row has beside the stream it measures.
type m5aRowOpt struct {
	// otherRTMP is how many RTMP players of other streams run for the whole
	// row: openstreamer_output_sessions counts them too.
	otherRTMP int
	// sessions and segmented are the readers the row starts; nil means
	// every session output and every segmented format.
	sessions  []stream.Output
	segmented []string
	// taps are the served names whose hubs the segmented captures are
	// compared with, capture video track i with taps[i]; nil means the
	// stream's own name. The session readers are compared with taps[0].
	taps []string
	// watch is the output whose packager rule (d) samples; "" samples none.
	watch stream.Output
	// tls, when set, has the row's RTMP, RTSP and HTTP-TS readers come
	// through the server's TLS listeners, verifying it against its CA.
	tls *m5c1
}

// m5aRow measures "not interrupted" for one stream (R-D12): a tap, one
// long-lived reader per output, the counters before and after, and a
// packager sampler.
type m5aRow struct {
	m    *m5a
	name string
	opt  m5aRowOpt

	taps  []*hubRecorder
	start time.Time // when the readers started
	rtmp  <-chan capResult
	rtsp  <-chan capResult
	raw   map[stream.Output]<-chan rawResult // httpts and srt
	seg   map[string]<-chan m5aSeg
	watch *m5aWatch

	base, last map[string]float64     // the counters after the readers connected, and just before the DELETE
	edits      []time.Time            // when each edit's answer came
	fresh      map[stream.Output]int  // new sessions the row opened on an output, and closed, after the baseline
	taken      map[stream.Output]bool // session readers the scenario received itself

	// What the readers returned, filled by end.
	hubs   [][]media.Frame
	caps   map[stream.Output]capResult
	bytes  map[stream.Output][]byte
	segs   map[string]m5aSeg
	ended  bool
	endErr map[string]error
}

// m5aOpen starts a row on the running stream name: the taps (before any
// reader, so every reader joins at or after a tap's first keyframe), the
// readers, the wait until they are connected, then the baseline.
func m5aOpen(t *testing.T, m *m5a, name string, opt m5aRowOpt) *m5aRow { //nolint:unparam // a helper for later milestones too, whose streams have other names
	t.Helper()
	if opt.sessions == nil {
		opt.sessions = m5aSessions
	}
	if opt.segmented == nil {
		opt.segmented = []string{m5aHLSTS, m5aHLSFMP4, m5aDASH}
	}
	if opt.taps == nil {
		opt.taps = []string{name}
	}
	r := &m5aRow{
		m: m, name: name, opt: opt,
		raw: map[stream.Output]<-chan rawResult{}, seg: map[string]<-chan m5aSeg{},
		fresh: map[stream.Output]int{}, taken: map[stream.Output]bool{},
		caps: map[stream.Output]capResult{}, bytes: map[stream.Output][]byte{}, segs: map[string]m5aSeg{},
		endErr: map[string]error{},
	}
	st := m.running(t, name)
	for _, served := range opt.taps {
		r.taps = append(r.taps, m.tap(t, served))
	}
	m.listed(t, name, 1)
	// The hub's subscribers before the readers: the tap, the packagers and
	// the pushers that are there.
	if len(st.Pushes) > 0 {
		m5aUntil(t, m5aWait, func() bool {
			p := m.status(t, name).Pushes
			return len(p) > 0 && p[0].State == "publishing"
		}, "%s: the push is publishing", name)
	}
	before := m.metric(t, "openstreamer_hub_subscribers", "name", name)

	r.start = time.Now()
	for _, out := range opt.sessions {
		switch out {
		case stream.OutRTMP:
			if opt.tls != nil {
				r.rtmp = readPlayTLS(opt.tls.tlsURL(out, name), opt.tls.ca.Client())
			} else {
				r.rtmp = readPlay(m.url(out, name))
			}
		case stream.OutRTSP:
			if opt.tls != nil {
				r.rtsp = readRTSPTLS(opt.tls.tlsURL(out, name), opt.tls.ca.Client())
			} else {
				r.rtsp = readRTSP(m.url(out, name))
			}
		case stream.OutHTTPTS:
			if opt.tls != nil {
				r.raw[out] = rawHTTPWith(opt.tls.client, opt.tls.tlsURL(out, name))
			} else {
				r.raw[out] = rawHTTP(m.url(out, name))
			}
		case stream.OutSRT:
			r.raw[out] = rawSRT(m.addrs.SRT, "live/"+name)
		case stream.OutHLS, stream.OutDASH:
			require.Fail(t, "not a session output: "+string(out))
		}
	}
	for _, f := range opt.segmented {
		r.seg[f] = m.readSegmented(f, name, m5aSegmented)
	}

	// Connected: the session gauges, and the hub's subscribers (the only
	// sign of an HTTP-TS reader).
	want := map[string]float64{"rtmp": float64(opt.otherRTMP), "rtsp": 0, "srt": 0}
	for _, out := range opt.sessions {
		if out != stream.OutHTTPTS {
			want[string(out)]++
		}
	}
	m5aUntil(t, m5aWait, func() bool {
		s := m.scrape(t)
		for out, n := range want {
			if m5aFind(t, s, "openstreamer_output_sessions", "output", out) != n {
				return false
			}
		}
		return m5aFind(t, s, "openstreamer_hub_subscribers", "name", name) == before+float64(len(opt.sessions))
	}, "%s: the readers are connected: sessions %v, %v + %d hub subscribers", name, want, before, len(opt.sessions))
	r.base = r.counters(t)
	if opt.watch != "" {
		r.watch = m.watchPackager(opt.watch, name)
		t.Cleanup(r.watch.end)
	}
	t.Logf("%s: connected %v after the readers' start; baseline %v", name, time.Since(r.start).Round(time.Millisecond), r.base)
	return r
}

// counters reads what a row compares: the hub's subscribers, the session
// gauges and counters of every output, and the stream's status.
func (r *m5aRow) counters(t *testing.T) map[string]float64 {
	t.Helper()
	s := r.m.scrape(t)
	c := map[string]float64{
		"hub_subscribers": m5aFind(t, s, "openstreamer_hub_subscribers", "name", r.name),
	}
	for _, out := range []string{"rtmp", "rtsp", "srt"} {
		c["output_sessions "+out] = m5aFind(t, s, "openstreamer_output_sessions", "output", out)
		c["output_resyncs_total "+out] = m5aFind(t, s, "openstreamer_output_resyncs_total", "output", out)
	}
	for _, out := range []string{"rtmp", "rtsp"} {
		c["output_sessions_started_total "+out] = m5aFind(t, s, "openstreamer_output_sessions_started_total", "output", out)
	}
	st := r.m.status(t, r.name)
	c["generation"] = float64(st.Generation)
	c["input_switches"] = float64(st.Switches)
	c["restarts"] = float64(st.Restarts)
	c["rebuilds"] = float64(st.Rebuilds)
	if len(st.Pushes) > 0 {
		c["pushes[0].connects"] = float64(st.Pushes[0].Connects)
		// A push that was not touched lost no frame.
		c["pushes[0].resyncs"] = m5aFind(t, s, "openstreamer_push_resyncs_total", "target", st.Pushes[0].ID)
	}
	if st.Transcoder != nil {
		c["transcoder.pid"] = float64(st.Transcoder.PID)
		c["transcoder.spawns"] = float64(st.Transcoder.Spawns)
	}
	return c
}

// edited records that an edit's answer has just come.
func (r *m5aRow) edited() { r.edits = append(r.edits, time.Now()) }

// ownEnds requires that the row's reader of a session output ends within
// m5aOwnEnds of the last edit, and takes it.
func (r *m5aRow) ownEnds(t *testing.T, out stream.Output) {
	t.Helper()
	left := time.Until(r.edits[len(r.edits)-1].Add(m5aOwnEnds))
	timeout := time.After(max(left, 0))
	ended := func() {
		t.Logf("%s: the %s reader ended %v after the edit", r.name, out, time.Since(r.edits[len(r.edits)-1]).Round(time.Millisecond))
	}
	fail := func() {
		require.Failf(t, "the row's own reader did not end", "%s: the %s reader ends within %v of the edit", r.name, out, m5aOwnEnds)
	}
	r.taken[out] = true
	switch out {
	case stream.OutRTMP:
		select {
		case <-r.rtmp:
			ended()
		case <-timeout:
			fail()
		}
	case stream.OutRTSP:
		select {
		case <-r.rtsp:
			ended()
		case <-timeout:
			fail()
		}
	case stream.OutHTTPTS, stream.OutSRT:
		select {
		case <-r.raw[out]:
			ended()
		case <-timeout:
			fail()
		}
	case stream.OutHLS, stream.OutDASH:
		require.Fail(t, "not a session output: "+string(out))
	}
}

// ownSegmentedEnd requires that the row's readers of a segmented output
// that an edit turned off have returned by at, and takes them.
func (r *m5aRow) ownSegmentedEnd(t *testing.T, out stream.Output, at time.Time) {
	t.Helper()
	for _, f := range m5aFormats {
		ch := r.seg[f.name]
		if f.out != out || ch == nil {
			continue
		}
		select {
		case s := <-ch:
			r.segs[f.name] = s
			t.Logf("%s: the %s reader ended %v after the edit: %v", r.name, f.name,
				s.at.Sub(r.edits[len(r.edits)-1]).Round(time.Millisecond), s.err)
		case <-time.After(max(time.Until(at), 0)):
			require.Failf(t, "the row's own reader did not end", "%s: the %s reader ends once %s is off", r.name, f.name, out)
		}
	}
}

// settled waits until the stream's counters are the baseline changed by
// except's rule: the row's own sessions have come and gone on the server's
// side too.
func (r *m5aRow) settled(t *testing.T, except ...stream.Output) {
	t.Helper()
	want := r.expected(except)
	var got map[string]float64
	m5aUntil(t, m5aWait, func() bool {
		got = r.counters(t)
		return maps.Equal(want, got)
	}, "%s: the counters settle at %v; they are %v", r.name, want, &got)
}

// expected is the baseline with what the row's own outputs change: a
// session reader that ended is one session and one hub subscriber less,
// and every new session the row opened afterwards is one more started.
func (r *m5aRow) expected(except []stream.Output) map[string]float64 {
	want := maps.Clone(r.base)
	for _, out := range except {
		if !slices.Contains(r.opt.sessions, out) {
			continue
		}
		want["hub_subscribers"]--
		if out != stream.OutHTTPTS {
			want["output_sessions "+string(out)]--
		}
		if out == stream.OutRTMP || out == stream.OutRTSP {
			want["output_sessions_started_total "+string(out)] += float64(r.fresh[out])
		}
	}
	return want
}

// end ends the row. It waits for every segmented reader that is not of an
// output in except, each of which must return later than the last edit's
// answer; requires that every session reader the scenario did not take is
// still running; reads the counters; and only then ends the stream with a
// DELETE, which is what ends the session readers and the taps on the same
// frame. It then collects what they read.
func (r *m5aRow) end(t *testing.T, except ...stream.Output) {
	t.Helper()
	require.NotEmpty(t, r.edits, "a row has an edit")
	last := r.edits[len(r.edits)-1]
	for _, f := range m5aFormats {
		ch := r.seg[f.name]
		if ch == nil || slices.Contains(except, f.out) {
			continue
		}
		select {
		case s := <-ch:
			r.segs[f.name] = s
			require.True(t, s.at.After(last), "%s: the %s reader returned %v before the last edit's answer",
				r.name, f.name, last.Sub(s.at))
			t.Logf("%s: the %s reader returned %v after its start, %v after the last edit", r.name, f.name,
				s.at.Sub(r.start).Round(time.Millisecond), s.at.Sub(last).Round(time.Millisecond))
		case <-time.After(runLimit):
			require.Failf(t, "a segmented reader did not return", "%s: %s", r.name, f.name)
		}
	}
	if r.watch != nil {
		r.watch.end()
	}
	// Nothing ended by itself: ending the stream would hide a reader that
	// had ended earlier.
	for _, out := range r.opt.sessions {
		if r.taken[out] {
			continue
		}
		running := true
		switch out {
		case stream.OutRTMP:
			select {
			case c := <-r.rtmp:
				running, r.endErr[string(out)] = false, c.err
			default:
			}
		case stream.OutRTSP:
			select {
			case c := <-r.rtsp:
				running, r.endErr[string(out)] = false, c.err
			default:
			}
		case stream.OutHTTPTS, stream.OutSRT:
			select {
			case c := <-r.raw[out]:
				running, r.endErr[string(out)] = false, c.err
			default:
			}
		case stream.OutHLS, stream.OutDASH:
		}
		require.True(t, running, "%s: the %s reader ended by itself before the DELETE (error: %v)", r.name, out, r.endErr[string(out)])
	}
	r.last = r.counters(t)
	r.m.del(t, r.name)
	r.ended = true

	for _, out := range r.opt.sessions {
		if r.taken[out] {
			continue
		}
		switch out {
		case stream.OutRTMP:
			r.caps[out] = m5aWaitCap(t, r.name+": rtmp", r.rtmp)
		case stream.OutRTSP:
			r.caps[out] = m5aWaitCap(t, r.name+": rtsp", r.rtsp)
		case stream.OutHTTPTS, stream.OutSRT:
			select {
			case res := <-r.raw[out]:
				require.NoError(t, res.err, "%s: %s", r.name, out)
				r.bytes[out] = res.b
			case <-time.After(m5aWait):
				require.Failf(t, "a reader did not end with the stream", "%s: %s", r.name, out)
			}
		case stream.OutHLS, stream.OutDASH:
		}
	}
	for i, tap := range r.taps {
		select {
		case <-tap.done:
		case <-time.After(m5aWait):
			require.Failf(t, "a tap did not end with the stream", "%s: %s", r.name, r.opt.taps[i])
		}
		r.hubs = append(r.hubs, stepFrames(tap.wait(t, "the tap of "+r.opt.taps[i])))
	}
}

// m5aWaitCap waits for a session reader that the stream's end ends.
func m5aWaitCap(t *testing.T, what string, ch <-chan capResult) capResult {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(m5aWait):
		require.Failf(t, "a reader did not end with the stream", "%s", what)
		return capResult{}
	}
}

// assertUninterrupted applies the capture-to-tap table to every reader of
// the row that is not of an output in except, then compares the counters
// read just before the DELETE with the baseline: all equal, but for what
// except's own readers changed (see expected). The row has ended.
//
// A session reader's capture and the tap end on the same frame, so they
// are compared frame for frame. A segmented output has no session to end
// with the stream: its capture shares neither a start nor an end with the
// tap, and its timeline is the packager's, so it is held to the weaker
// rule of m5aAssertSegmented.
func assertUninterrupted(t *testing.T, r *m5aRow, except ...stream.Output) {
	t.Helper()
	require.True(t, r.ended, "the row has ended")
	held := func(out stream.Output) bool { return !slices.Contains(except, out) }
	hub := r.hubs[0]
	for _, out := range r.opt.sessions {
		if !held(out) {
			continue
		}
		t.Run(string(out), func(t *testing.T) {
			switch out {
			case stream.OutRTMP:
				c := r.caps[out]
				require.NoError(t, c.err)
				assert.Empty(t, c.c.Violations)
				assertMatchesHub(t, hub, c.c.Frames)
			case stream.OutRTSP:
				c := r.caps[out]
				require.NoError(t, c.err)
				assert.Empty(t, c.c.Violations)
				assert.Zero(t, c.c.RTPLost, "RTP packets lost")
				assert.Equal(t, 2, c.c.ByeTracks, "a BYE on both tracks")
				assertRTSPMatchesHub(t, m5aRTSPEpochs(hub), c.c.Frames)
			case stream.OutHTTPTS, stream.OutSRT:
				b := r.bytes[out]
				require.NotEmpty(t, b)
				assertTSMatchesHub(t, hub, tsFrames(t, b))
				assert.Zero(t, tsGaps(t, b), "resumes after loss")
				assert.Empty(t, conformBytes(t, b).Violations)
			case stream.OutHLS, stream.OutDASH:
			}
		})
	}
	for _, f := range m5aFormats {
		if r.seg[f.name] == nil || !held(f.out) {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			s := r.segs[f.name]
			require.NoError(t, s.err)
			m5aAssertSegmented(t, s.c, r.hubs)
		})
	}
	if r.watch != nil {
		t.Run("packager", r.watch.check)
	}
	assert.Equal(t, r.expected(except), r.last, "%s: the counters just before the DELETE, against the baseline", r.name)
	for out := range r.base {
		if strings.HasPrefix(out, "output_resyncs_total ") {
			assert.Equal(t, r.base[out], r.last[out], "%s: %s: no reader of any output lost a frame", r.name, out)
		}
	}
}

// m5aRTSPEpochs returns the tap's frames with the epochs an RTSP capture
// has: RTSP has no signal for an epoch change, so conform.ReadRTSP opens a
// capture epoch where the video parameter sets change, and the loops of
// one file, whose parameter sets are the same, are one capture epoch with
// one RTP timeline. assertRTSPMatchesHub numbers the capture's epochs from
// the hub's, which holds as it is only when every hub epoch changes the
// parameter sets; with the epochs counted this way it holds the capture to
// the tap frame for frame across the loops too.
func m5aRTSPEpochs(frames []media.Frame) []media.Frame {
	out := slices.Clone(frames)
	last, epoch := "", uint32(1)
	for i := range out {
		f := &out[i]
		if f.Kind() == media.KindVideo && f.Config != nil {
			if sets := m5aParameterSets(f.Config); sets != last {
				if last != "" {
					epoch++
				}
				last = sets
			}
		}
		f.Epoch = epoch
	}
	return out
}

// m5aParameterSets is the identity of a video config's parameter sets.
func m5aParameterSets(c *media.TrackConfig) string {
	var b strings.Builder
	for _, sets := range [][][]byte{c.VPS, c.SPS, c.PPS} {
		for _, s := range sets {
			fmt.Fprintf(&b, "%x.", s)
		}
		b.WriteByte('/')
	}
	return b.String()
}

// m5aSteps returns, per capture or hub epoch, the frame count of the
// frames, and the set of DTS steps between neighbours inside an epoch.
func m5aSteps(frames []media.Frame) (counts map[uint32]int, steps map[int64]int) {
	byEpoch := map[uint32][]int64{}
	for _, f := range frames {
		byEpoch[f.Epoch] = append(byEpoch[f.Epoch], f.DTS)
	}
	counts, steps = map[uint32]int{}, map[int64]int{}
	for e, dts := range byEpoch {
		slices.Sort(dts)
		counts[e] = len(dts)
		for i := 1; i < len(dts); i++ {
			steps[dts[i]-dts[i-1]]++
		}
	}
	return counts, steps
}

// m5aInner returns the counts of the epochs that are neither the first nor
// the last, in epoch order.
func m5aInner(counts map[uint32]int) []int {
	epochs := slices.Sorted(maps.Keys(counts))
	var out []int
	for i, e := range epochs {
		if i > 0 && i < len(epochs)-1 {
			out = append(out, counts[e])
		}
	}
	return out
}

// m5aAssertSegmented is the rule of a segmented output's capture, which
// shares neither a start nor an end with the tap: (a) no conform violation
// over the poll (segment continuity, durations, a discontinuity at every
// epoch change and nowhere else); (b) inside every capture epoch, every
// DTS step of a track is a step the tap has for that kind, so no frame is
// missing; (c) every capture epoch that is neither its first nor its last
// holds exactly the frames of a whole tap epoch, and there is at least one
// such epoch. A frame lost in the first or the last capture epoch is
// caught as a step, not by a count.
//
// Capture video track i is compared with hubs[i] (a ladder's rendition i),
// every audio track with hubs[0], whose audio every rendition shares.
func m5aAssertSegmented(t *testing.T, c conform.Capture, hubs [][]media.Frame) {
	t.Helper()
	assert.Empty(t, c.Violations, "(a) no violation over the poll")
	cv, ca := byTrack(c.Frames)
	require.Len(t, cv, len(hubs), "video tracks in the capture")
	require.NotEmpty(t, ca, "audio tracks in the capture")
	check := func(what string, got, tapped []media.Frame) {
		t.Helper()
		tapCounts, tapSteps := m5aSteps(tapped)
		whole := m5aInner(tapCounts)
		require.NotEmpty(t, whole, "%s: the tap has a whole epoch", what)
		require.NotEmpty(t, tapSteps, "%s: the tap has DTS steps", what)
		counts, steps := m5aSteps(got)
		for step, n := range steps {
			assert.Contains(t, tapSteps, step, "(b) %s: %d DTS steps of %d ticks inside an epoch; the tap's steps are %v",
				what, n, step, tapSteps)
		}
		inner := m5aInner(counts)
		require.NotEmpty(t, inner, "(c) %s: the capture holds a whole epoch; per epoch %v", what, counts)
		for _, n := range inner {
			assert.Contains(t, whole, n, "(c) %s: a whole capture epoch holds %d frames; the tap's whole epochs hold %v; per capture epoch %v",
				what, n, whole, counts)
		}
		t.Logf("%s: capture epochs %v, steps %v; the tap's whole epochs %v, steps %v", what, counts, steps, whole, tapSteps)
	}
	for i, tr := range sortedKeys(cv) {
		hv, _ := byKind(hubs[i], media.KindVideo)
		check(fmt.Sprintf("video track %d", tr), cv[tr], hv)
	}
	ha, _ := byKind(hubs[0], media.KindAudio)
	for _, tr := range sortedKeys(ca) {
		check(fmt.Sprintf("audio track %d", tr), ca[tr], ha)
	}
}

// TestM5aEveryOutput creates two streams over the API on a server that
// started with none: sink, an RTMP listen input, and cam, the looping
// fixture with every output on and a push to sink. Every output of cam
// plays for 8 s without a violation, across an epoch change, and sink's HLS
// carries what cam pushed.
func TestM5aEveryOutput(t *testing.T) {
	m := m5aServer(t, "")
	m.camAndSink(t)
	p, err := config.Parse([]byte(m.fileText(t)), config.Options{RequiredIterations: m5aIter})
	require.NoError(t, err)
	names := make([]string, 0, len(p.Doc.Streams))
	for _, sp := range p.Doc.Streams {
		names = append(names, sp.Name)
	}
	assert.ElementsMatch(t, []string{"cam", "sink"}, names, "both streams are in the config file")

	m.running(t, "cam")
	m.listed(t, "cam", 1)
	const span = 8 * time.Second
	segmented := map[string]<-chan m5aSeg{}
	for _, f := range m5aFormats {
		segmented[f.name] = m.readSegmented(f.name, "cam", span)
	}
	sessions := map[stream.Output]<-chan capResult{}
	for _, out := range m5aSessions {
		sessions[out] = m.readSession(out, "cam", span)
	}
	for _, f := range m5aFormats {
		select {
		case r := <-segmented[f.name]:
			epochs := m5aAV(t, f.name, r.c, r.err)
			assert.GreaterOrEqual(t, epochs, 2, "%s: an epoch change survived", f.name)
			t.Logf("%-8s %s", f.name, kindCounts(r.c.Frames))
		case <-time.After(span + 2*m5aRead):
			require.Fail(t, f.name+" did not return")
		}
	}
	for _, out := range m5aSessions {
		select {
		case r := <-sessions[out]:
			m5aAV(t, string(out), r.c, r.err)
			// The file loops every 6 s, so 7 s of video cross a loop: an
			// epoch change, which RTMP carries as a new capture epoch and
			// the others as continuing timestamps.
			v, _ := byKind(r.c.Frames, media.KindVideo)
			assert.GreaterOrEqual(t, len(v), 7*25, "%s: the capture crosses a loop of the file", out)
			t.Logf("%-8s %s", out, kindCounts(r.c.Frames))
		case <-time.After(span + 2*m5aRead):
			require.Fail(t, string(out)+" did not return")
		}
	}

	// sink has frames: the push arrived and the listen input took it.
	m.running(t, "sink")
	m.listed(t, "sink", 1)
	r := <-m.readSegmented(m5aHLSTS, "sink", 0)
	m5aAV(t, "sink's HLS", r.c, r.err)

	st := m.status(t, "cam")
	assert.Equal(t, "running", st.State)
	assert.Equal(t, 0, st.ActiveInput)
	assert.Len(t, st.Tracks, 2)
	require.Len(t, st.Pushes, 1)
	assert.Equal(t, "publishing", st.Pushes[0].State)
	assert.EqualValues(t, 1, st.Pushes[0].Connects)

	code, body := m.get(t, "/api/v1/streams")
	require.Equal(t, http.StatusOK, code)
	var list struct {
		Streams []spec.Stream `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(body, &list), "%s", body)
	require.Len(t, list.Streams, 2)
	assert.Equal(t, "cam", list.Streams[0].Name)
	assert.Equal(t, "sink", list.Streams[1].Name)
	assert.NotContains(t, string(body), m5aSinkKey, "the list is redacted")
}

// m5aEditRow is one row of TestM5aEditOutputs.
type m5aEditRow struct {
	name string
	outs []stream.Output       // the row's own outputs: what its edit turns off
	off  func(o *spec.Outputs) // the first edit
	// watch is the output whose packager stays for the whole row; "" when
	// the edit stops the packager.
	watch stream.Output
}

// TestM5aEditOutputs is the done criterion's second half, one output at a
// time: with a reader on every output of cam, an edit turns one output off
// and a second one turns it back on, as per-stream PUTs. The row's own
// output ends and comes back; every other reader is held to the
// capture-to-tap table of assertUninterrupted, and the counters say that
// nothing else was touched.
func TestM5aEditOutputs(t *testing.T) {
	rows := []m5aEditRow{
		{"rtsp", []stream.Output{stream.OutRTSP}, func(o *spec.Outputs) { o.RTSP = false }, stream.OutHLS},
		{"rtmp", []stream.Output{stream.OutRTMP}, func(o *spec.Outputs) { o.RTMP = false }, stream.OutHLS},
		{"srt", []stream.Output{stream.OutSRT}, func(o *spec.Outputs) { o.SRT = false }, stream.OutHLS},
		{"httpts", []stream.Output{stream.OutHTTPTS}, func(o *spec.Outputs) { o.HTTPTS = false }, stream.OutHLS},
		{"hls", []stream.Output{stream.OutHLS}, func(o *spec.Outputs) { o.HLS = false }, stream.OutDASH},
		{"dash", []stream.Output{stream.OutDASH}, func(o *spec.Outputs) { o.DASH = false }, stream.OutHLS},
		{"hls+dash", []stream.Output{stream.OutHLS, stream.OutDASH}, func(o *spec.Outputs) { o.HLS, o.DASH = false, false }, ""},
		{"push", nil, func(o *spec.Outputs) { o.Push = nil }, stream.OutHLS},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			m5aEditOutputs(t, row)
		})
	}
}

func m5aEditOutputs(t *testing.T, row m5aEditRow) {
	m := m5aServer(t, "")
	cam := m.camAndSink(t)
	r := m5aOpen(t, m, "cam", m5aRowOpt{watch: row.watch})
	session := len(row.outs) == 1 && slices.Contains(m5aSessions, row.outs[0])
	own := func(out stream.Output) bool { return slices.Contains(row.outs, out) }
	first, _ := m.srv.Manager().Packager(stream.OutHLS, "cam")
	require.NotNil(t, first)

	// The first edit: the row's output off.
	edited := cam
	row.off(&edited.Outputs)
	m5aSleepUntil(r.start.Add(m5aEdit))
	code, applied := m.put(t, edited)
	r.edited()
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "outputs", applied)
	second := r.start.Add(2 * m5aEdit)
	switch {
	case session:
		// The row's own reader ends, and a new session is refused until
		// the second edit.
		r.ownEnds(t, row.outs[0])
		m.refused(t, row.outs[0], "cam")
		m5aSleepUntil(second.Add(-time.Second))
		m.refused(t, row.outs[0], "cam")
	case row.name == "push":
		assert.Empty(t, m.status(t, "cam").Pushes, "no push")
		m5aUntil(t, 5*time.Second, func() bool { return m.status(t, "sink").ActiveInput < 0 }, "sink's input goes quiet within 5 s")
	default:
		// HLS is hls/* and cmaf/*.m3u8, DASH is cmaf/*.mpd: an output that
		// is off answers 404 at once, the other one still plays.
		for _, f := range m5aFormats {
			want := http.StatusOK
			if own(f.out) {
				want = http.StatusNotFound
			}
			assert.Equal(t, want, m.mediaGet(t, m.live("cam")+f.file).code, f.file)
		}
		if own(stream.OutHLS) {
			assert.Equal(t, http.StatusNotFound, m.mediaGet(t, m.live("cam")+"cmaf/video.m3u8").code, "any cmaf/*.m3u8")
		}
		for _, out := range row.outs {
			_, ok := m.srv.Manager().Packager(out, "cam")
			assert.False(t, ok, "no packager for %s", out)
			r.ownSegmentedEnd(t, out, second)
		}
	}

	// The second edit: back on, and the output works again.
	m5aSleepUntil(second)
	code, applied = m.put(t, cam)
	r.edited()
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "outputs", applied)
	switch {
	case session:
		m.fresh(t, row.outs[0], "cam")
		r.fresh[row.outs[0]]++
	case row.name == "push":
		m5aUntil(t, m5aWait, func() bool {
			p := m.status(t, "cam").Pushes
			return len(p) == 1 && p[0].State == "publishing" && m.status(t, "sink").ActiveInput == 0
		}, "the new pusher publishes and sink's input is back")
	default:
		m.listed(t, "cam", 2)
		// With both off and on again, a new packager started in the middle
		// of the stream, on the hub's cached GOP: it joins (live.Options.Join),
		// so its first segment starts every track at the keyframe.
		for _, out := range row.outs {
			m.fresh(t, out, "cam")
		}
		now, ok := m.srv.Manager().Packager(stream.OutHLS, "cam")
		require.True(t, ok)
		if len(row.outs) == 2 {
			assert.NotSame(t, first, now, "a new packager serves after both outputs were off")
		} else {
			assert.Same(t, first, now, "the packager stayed while one of HLS and DASH was on")
		}
	}
	r.settled(t, row.outs...)

	r.end(t, row.outs...)
	assertUninterrupted(t, r, row.outs...)
	// In every row but push the pusher kept its one connection for the
	// whole row; in push it is the new pusher's first.
	assert.EqualValues(t, 1, r.last["pushes[0].connects"], "pushes[0].connects at the end")
}

// TestM5aRebuildIsTheOtherRoute is the contrast that keeps
// TestM5aEditOutputs honest: an edit outside outputs rebuilds the stream,
// and then a reader does end. Up to the rebuild it lost nothing, and
// afterwards every output plays again from the new generation.
func TestM5aRebuildIsTheOtherRoute(t *testing.T) {
	m := m5aServer(t, "")
	cam := m5aSpec("cam", m5aSource(t))
	code, applied := m.put(t, cam)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "created", applied)
	m.running(t, "cam")
	tap := m.tap(t, "cam")
	got := readPlay(m.url(stream.OutRTMP, "cam"))
	m5aUntil(t, m5aWait, func() bool { return m.metric(t, "openstreamer_output_sessions", "output", "rtmp") == 1 }, "the player is connected")
	require.EqualValues(t, 1, m.status(t, "cam").Generation)
	time.Sleep(3 * time.Second)

	// An edit of the inputs alone is applied in place; this layout keeps
	// both kinds and is not the default, so it rebuilds.
	cam.Layout = []string{"audio", "video"}
	code, applied = m.put(t, cam)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "rebuild", applied)
	c := m5aWaitCap(t, "the RTMP player", got)
	require.NoError(t, c.err)
	assert.Empty(t, c.c.Violations)
	select {
	case <-tap.done:
	case <-time.After(m5aWait):
		require.Fail(t, "the tap did not end: the old hub was not closed")
	}
	assertMatchesHub(t, stepFrames(tap.wait(t, "the tap")), c.c.Frames)

	st := m.running(t, "cam")
	assert.EqualValues(t, 2, st.Generation)
	assert.EqualValues(t, 1, st.Rebuilds)
	m.listed(t, "cam", 2)
	for _, out := range []stream.Output{stream.OutHLS, stream.OutDASH, stream.OutHTTPTS, stream.OutRTMP, stream.OutRTSP, stream.OutSRT} {
		m.fresh(t, out, "cam")
	}
	assert.EqualValues(t, 2, m.status(t, "cam").Generation)
}

// m5aOther is the stream of the config-editor scenarios that no edit
// touches: a tap and an RTMP player for its whole life.
type m5aOther struct {
	m    *m5a
	tap  *hubRecorder
	rtmp <-chan capResult
}

func m5aOpenOther(t *testing.T, m *m5a) *m5aOther {
	t.Helper()
	m.running(t, "other")
	o := &m5aOther{m: m, tap: m.tap(t, "other")}
	o.rtmp = readPlay(m.url(stream.OutRTMP, "other"))
	m5aUntil(t, m5aWait, func() bool { return m.metric(t, "openstreamer_output_sessions", "output", "rtmp") == 1 }, "other's player is connected")
	return o
}

// end requires that other is still in its first generation with its player
// running, deletes it, and holds the player to the tap frame for frame.
func (o *m5aOther) end(t *testing.T) {
	t.Helper()
	st := o.m.status(t, "other")
	assert.EqualValues(t, 1, st.Generation, "other's generation never moved")
	assert.Zero(t, st.Restarts+st.Rebuilds)
	select {
	case c := <-o.rtmp:
		require.Fail(t, "other's player ended by itself", "%v", c.err)
	default:
	}
	o.m.del(t, "other")
	c := m5aWaitCap(t, "other's player", o.rtmp)
	require.NoError(t, c.err)
	assert.Empty(t, c.c.Violations)
	select {
	case <-o.tap.done:
	case <-time.After(m5aWait):
		require.Fail(t, "other's tap did not end")
	}
	assertMatchesHub(t, stepFrames(o.tap.wait(t, "other's tap")), c.c.Frames)
}

// m5aEditorSetup starts a server and gives it the commented streams of the
// config-editor scenarios through PUT /api/v1/config (the push and the
// listen input name the RTMP listener, whose port only a started server
// knows). It returns the text in force.
func m5aEditorSetup(t *testing.T) (*m5a, string) {
	t.Helper()
	m := m5aServer(t, "")
	text, etag := m.configText(t)
	require.Contains(t, text, "streams: []\n")
	text = strings.Replace(text, "streams: []\n", m.editorStreams(t), 1)
	code, res := m.putConfig(t, text, etag)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []stream.Outcome{{Name: "cam", Applied: "created"}, {Name: "other", Applied: "created"}, {Name: "sink", Applied: "created"}}, res.Streams)
	return m, text
}

// m5aOnce replaces the one occurrence of old in text.
func m5aOnce(t *testing.T, text, old, new string) string {
	t.Helper()
	require.Equal(t, 1, strings.Count(text, old), "%q occurs once", old)
	return strings.Replace(text, old, new, 1)
}

// TestM5aConfigPut is the done criterion through the config editor: one
// line of the YAML that GET /api/v1/config returned is changed (cam's
// rtsp) and the text is PUT back. Only cam's RTSP ends; cam's other
// outputs, and the other streams, are not interrupted. The text the GET
// returns is masked (R-T8): the stream keys in it are xxxxx, and the PUT
// resolves them, so the file is the text that was first written with the
// one line changed, every comment and every real key included.
func TestM5aConfigPut(t *testing.T) {
	m, sent := m5aEditorSetup(t)
	other := m5aOpenOther(t, m)
	r := m5aOpen(t, m, "cam", m5aRowOpt{otherRTMP: 1, watch: stream.OutHLS})

	text, etag := m.configText(t)
	require.Equal(t, sent, m.fileText(t), "the file holds the text that was sent, keys and all")
	require.Contains(t, sent, m5aSinkKey)
	shown, err := config.Mask([]byte(sent))
	require.NoError(t, err)
	assert.Equal(t, string(shown), text, "GET /api/v1/config returns the file masked")
	assert.NotContains(t, text, m5aSinkKey)
	assert.Contains(t, text, "xxxxx")
	for _, comment := range []string{
		"# The config of an M5a scenario.", "# cam: every output, and a push to sink.",
		"# The players of rtsp://host/live/cam.", "# sink: takes cam's push.", "# other: no edit touches it.",
	} {
		require.Contains(t, text, comment)
	}
	edited := m5aOnce(t, text, "      rtsp: true\n", "      rtsp: false\n")
	editedFile := m5aOnce(t, sent, "      rtsp: true\n", "      rtsp: false\n")
	m5aSleepUntil(r.start.Add(m5aEdit))
	resp := m.putConfigRaw(t, edited, etag)
	r.edited()
	require.Equal(t, http.StatusOK, resp.code, "%s", resp.body)
	var res api.ApplyResult
	require.NoError(t, json.Unmarshal(resp.body, &res))
	assert.Equal(t, []stream.Outcome{{Name: "cam", Applied: "outputs"}, {Name: "other", Applied: "unchanged"}, {Name: "sink", Applied: "unchanged"}}, res.Streams)
	assert.Equal(t, []string{}, res.RestartRequired)
	assert.NotEqual(t, etag, res.ETag, "a new ETag")
	assert.Equal(t, res.ETag, resp.header.Get("ETag"))
	assert.Contains(t, string(resp.body), `"restart_required":[]`)
	r.ownEnds(t, stream.OutRTSP)
	m.refused(t, stream.OutRTSP, "cam")
	assert.Equal(t, editedFile, m.fileText(t), "the file is the text sent with its masks resolved, byte for byte")
	assert.NotContains(t, m.fileText(t), "xxxxx")

	// The writes that are refused change nothing: the old ETag, no
	// If-Match, and a text with an unknown key.
	code, _ := m.putConfig(t, text, etag)
	assert.Equal(t, http.StatusPreconditionFailed, code, "the old ETag")
	code, _ = m.putConfig(t, text, "")
	assert.Equal(t, http.StatusPreconditionRequired, code, "no If-Match")
	bad := m5aOnce(t, edited, "      rtsp: false\n", "      rtsp: false\n      rtsps: true\n")
	resp = m.putConfigRaw(t, bad, res.ETag)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.code, "%s", resp.body)
	assert.Contains(t, string(resp.body), "rtsps", "the unknown key is listed")
	assert.Equal(t, editedFile, m.fileText(t), "the file is untouched")
	now, nowTag := m.configText(t)
	assert.Equal(t, edited, now)
	assert.Equal(t, res.ETag, nowTag)

	r.settled(t, stream.OutRTSP)
	r.end(t, stream.OutRTSP)
	assertUninterrupted(t, r, stream.OutRTSP)
	other.end(t)
}

// TestM5aReload applies a hand edit of the file by POST /api/v1/reload:
// cam's SRT off, a new stream, a comment. Nothing changes before the
// reload; after it only cam's SRT has ended. A file that does not parse
// changes nothing and is reported, and a change of the server section is
// reported as restart_required and not applied.
func TestM5aReload(t *testing.T) {
	m, _ := m5aEditorSetup(t)
	other := m5aOpenOther(t, m)
	r := m5aOpen(t, m, "cam", m5aRowOpt{otherRTMP: 1, watch: stream.OutHLS})

	extra := "  # extra: added by hand.\n" +
		"  - name: extra\n" +
		"    inputs:\n" +
		"      - url: \"" + m5aSource(t) + "\"\n" +
		"    outputs: {hls: true}\n"
	m5aSleepUntil(r.start.Add(m5aEdit - time.Second))
	m.handEdit(t, func(text string) string {
		return m5aOnce(t, text, "      srt: true\n", "      # Turned off by hand.\n      srt: false\n") + extra
	})
	byHand := m.fileText(t)
	// Before the reload nothing changed: the file is stale, and cam still
	// serves SRT.
	assert.True(t, m.configStatus(t).Stale, "the file is stale before the reload")
	time.Sleep(time.Second)
	select {
	case c := <-r.raw[stream.OutSRT]:
		require.Fail(t, "cam's SRT reader ended before the reload", "%v", c.err)
	default:
	}
	assert.True(t, m.status(t, "cam").Outputs.SRT, "cam still serves SRT before the reload")
	assert.Equal(t, http.StatusNotFound, m.do(t, http.MethodGet, "/api/v1/streams/extra", nil).code)

	res := m.reload(t)
	r.edited()
	assert.Equal(t, []stream.Outcome{
		{Name: "cam", Applied: "outputs"},
		{Name: "extra", Applied: "created"},
		{Name: "other", Applied: "unchanged"},
		{Name: "sink", Applied: "unchanged"},
	}, res.Streams)
	assert.Equal(t, []string{}, res.RestartRequired)
	r.ownEnds(t, stream.OutSRT)
	m.refused(t, stream.OutSRT, "cam")
	st := m.configStatus(t)
	assert.False(t, st.Stale)
	assert.Empty(t, st.LastReloadError)
	assert.Equal(t, byHand, m.fileText(t), "a reload does not write the file")

	// extra plays over HLS.
	m.running(t, "extra")
	m.listed(t, "extra", 1)
	seg := <-m.readSegmented(m5aHLSTS, "extra", 0)
	m5aAV(t, "extra's HLS", seg.c, seg.err)

	// A file that does not parse: the error, and nothing changes.
	gens := func() map[string]uint64 {
		out := map[string]uint64{}
		for _, name := range []string{"cam", "extra", "other", "sink"} {
			out[name] = m.status(t, name).Generation
		}
		return out
	}
	before := gens()
	m.handEdit(t, func(text string) string { return text + "streams: [\n" })
	resp := m.do(t, http.MethodPost, "/api/v1/reload", nil)
	assert.Equal(t, http.StatusBadRequest, resp.code, "%s", resp.body)
	assert.Equal(t, before, gens(), "every stream keeps its generation")
	st = m.configStatus(t)
	assert.NotEmpty(t, st.LastReloadError, "config/status has last_reload_error")
	assert.True(t, st.Stale)
	assert.EqualValues(t, 1, m.metric(t, "openstreamer_config_reloads_total", "result", "error"))
	m.handEdit(t, func(string) string { return byHand })
	res = m.reload(t)
	for _, o := range res.Streams {
		assert.Equal(t, "unchanged", o.Applied, o.Name)
	}
	assert.Empty(t, m.configStatus(t).LastReloadError, "restoring the file and reloading clears it")

	// A change of the server section is reported, not applied.
	m.handEdit(t, func(text string) string {
		return m5aOnce(t, text, "  rtsp: {listen: \"127.0.0.1:0\"}\n", "  rtsp: {listen: \"127.0.0.1:1\"}\n")
	})
	res = m.reload(t)
	assert.Equal(t, []string{"server.rtsp.listen"}, res.RestartRequired)
	assert.Equal(t, []string{"server.rtsp.listen"}, m.configStatus(t).RestartRequired)
	assert.EqualValues(t, 1, m.metric(t, "openstreamer_config_restart_required"))
	assert.Equal(t, before, gens(), "every stream keeps its generation")

	r.settled(t, stream.OutSRT)
	r.end(t, stream.OutSRT)
	assertUninterrupted(t, r, stream.OutSRT)
	// RTSP still answers on the address it was bound to. This is a new
	// session, so it comes after cam's counters were compared.
	_, err := describe(t, m.url(stream.OutRTSP, "other"))
	require.NoError(t, err, "RTSP still answers on the old address")
	other.end(t)
}

// TestM5aHandEditThenWrite checks that the API never overwrites a hand
// edit: while the file differs from the config in force every write is 409
// config_changed and changes nothing; after a reload the same writes
// succeed and keep what was written by hand.
func TestM5aHandEditThenWrite(t *testing.T) {
	src := m5aSource(t)
	m := m5aServer(t, "streams:\n  - name: cam\n    inputs:\n      - url: \""+src+"\"\n    outputs: {hls: true}\n")
	st := m.running(t, "cam")
	const comment = "# Written by hand: keep me.\n"
	m.handEdit(t, func(text string) string { return comment + text })
	byHand := m.fileText(t)

	b := m5aSpec("b", src)
	_, etag := m.configText(t)
	text, _ := m.configText(t)
	writes := func() []m5aResp {
		body, err := json.Marshal(b)
		require.NoError(t, err)
		return []m5aResp{
			m.do(t, http.MethodPut, "/api/v1/streams/b", body, "Content-Type", "application/json"),
			m.do(t, http.MethodDelete, "/api/v1/streams/cam", nil),
			m.putConfigRaw(t, text+"# Sent over the API.\n", etag),
		}
	}
	for i, resp := range writes() {
		assert.Equal(t, http.StatusConflict, resp.code, "write %d: %s", i, resp.body)
		assert.Contains(t, string(resp.body), `"config_changed"`, "write %d", i)
	}
	assert.Equal(t, byHand, m.fileText(t), "the file still holds the hand edit, byte for byte")
	code, body := m.get(t, "/api/v1/streams")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, string(body), `"name":"cam"`)
	assert.NotContains(t, string(body), `"name":"b"`)
	now := m.status(t, "cam")
	assert.Equal(t, st.Generation, now.Generation, "no stream moved")
	assert.Equal(t, "running", now.State)

	res := m.reload(t)
	assert.Equal(t, []stream.Outcome{{Name: "cam", Applied: "unchanged"}}, res.Streams)
	// The same three requests, now on the text the reload took.
	text, etag = m.configText(t)
	assert.Equal(t, byHand, text)
	body, err := json.Marshal(b)
	require.NoError(t, err)
	resp := m.do(t, http.MethodPut, "/api/v1/streams/b", body, "Content-Type", "application/json")
	assert.Equal(t, http.StatusCreated, resp.code, "%s", resp.body)
	resp = m.do(t, http.MethodDelete, "/api/v1/streams/cam", nil)
	assert.Equal(t, http.StatusNoContent, resp.code, "%s", resp.body)
	text, etag = m.configText(t)
	code, _ = m.putConfig(t, text+"# Sent over the API.\n", etag)
	assert.Equal(t, http.StatusOK, code)
	file := m.fileText(t)
	assert.True(t, strings.HasPrefix(file, comment), "the hand-made comment is still in the file")
	assert.True(t, strings.HasSuffix(file, "# Sent over the API.\n"))
	assert.Contains(t, file, "name: b")
	assert.NotContains(t, file, "name: cam")
}

// TestM5aRestartRestores stops the server and starts another one from the
// same file: the streams are the ones the API created, with their secrets.
// A third start, with an entry added by hand that does not parse, runs the
// good streams, reports the bad entry and leaves it in the file.
func TestM5aRestartRestores(t *testing.T) {
	src := m5aSource(t)
	m := m5aServer(t, "")
	off := m5aSpec("off", "rtsp://user:restart-secret@127.0.0.1:1/x")
	off.Disabled = true
	for _, sp := range []spec.Stream{m5aSpec("cam", src), off, m5aSpec("third", src)} {
		code, _ := m.put(t, sp)
		require.Equal(t, http.StatusCreated, code)
	}
	m.running(t, "cam")
	// Secrets are write-only: ?secrets=1 is a 400, the documents are
	// redacted, and the secret is read from the file.
	code, _ := m.get(t, "/api/v1/streams?secrets=1")
	require.Equal(t, http.StatusBadRequest, code)
	code, before := m.get(t, "/api/v1/streams")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(before), "restart-secret")
	require.Contains(t, string(before), "rtsp://user:xxxxx@127.0.0.1:1/x")
	file := m.fileText(t)
	require.Contains(t, file, "rtsp://user:restart-secret@127.0.0.1:1/x")
	m.stop()

	m2 := m5aStart(t, m.path)
	code, after := m2.get(t, "/api/v1/streams")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, string(before), string(after), "the streams after the restart")
	assert.Equal(t, file, m2.fileText(t), "the secret is still in the file")
	m2.running(t, "cam")
	m2.running(t, "third")
	m2.listed(t, "cam", 1)
	seg := <-m2.readSegmented(m5aHLSTS, "cam", 0)
	m5aAV(t, "cam's HLS after the restart", seg.c, seg.err)
	assert.Equal(t, "stopped", m2.status(t, "off").State)
	assert.Empty(t, m2.configStatus(t).LoadErrors)
	m2.stop()

	const bad = "  - name: bad\n    inputs:\n      - url: \"udp://127.0.0.1:1\"\n    outputs: {hls: true}\n    bogus_key: 1\n"
	m2.handEdit(t, func(text string) string { return text + bad })
	byHand := m2.fileText(t)
	m3 := m5aStart(t, m.path)
	m3.running(t, "cam")
	m3.running(t, "third")
	assert.Equal(t, "stopped", m3.status(t, "off").State)
	code, after = m3.get(t, "/api/v1/streams")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, string(before), string(after), "the three good streams")
	st := m3.configStatus(t)
	require.Len(t, st.LoadErrors, 1, "%v", st.LoadErrors)
	assert.Contains(t, st.LoadErrors[0], "bad")
	assert.Contains(t, st.LoadErrors[0], "bogus_key")
	assert.EqualValues(t, 1, m3.metric(t, "openstreamer_config_load_errors"))
	assert.Equal(t, byHand, m3.fileText(t), "the bad entry is still in the file, untouched")
}

// TestM5aLadder runs a stream with a ladder of two encoded rungs and a
// thumbnail through the server: the names it is served under, the
// thumbnail, the transcoder's metrics, and an edit of its outputs, which
// touches neither the players nor the child.
//
// It runs in the arm64 job too: its readers are our own (conform's RTSP,
// RTMP and segmented readers), with no external SRT or RTSP peer.
func TestM5aLadder(t *testing.T) {
	bin := testpeer.OSTranscoder(t)
	path := filepath.Join(t.TempDir(), "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(m5aConfig("", bin)), 0o600))
	m := m5aStart(t, path)
	cam := m5aSpec("cam", m5aSource(t))
	cam.Transcode = &spec.Transcode{
		Renditions: []spec.Rendition{
			{Label: "360", Height: 360, BitrateKbps: 800, Encoder: "x264"},
			{Label: "180", Height: 180, BitrateKbps: 300},
		},
		Audio: spec.Audio{BitrateKbps: 128},
	}
	cam.Thumbnail = &spec.Thumbnail{IntervalSec: 2}
	code, applied := m.put(t, cam)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "created", applied)
	m.running(t, "cam")
	m.listed(t, "cam", 2)

	// The names (R-D3): the ABR presentation under cam, each rendition
	// alone under cam_<label>, and nothing else.
	for _, f := range m5aFormats {
		seg := <-m.readSegmented(f.name, "cam", 0)
		m5aAV(t, f.name, seg.c, seg.err)
		video, _ := byTrack(seg.c.Frames)
		assert.Len(t, video, 2, "%s lists 2 video variants", f.name)
	}
	for _, served := range []string{"cam_360", "cam_180"} {
		m.listed(t, served, 1)
		seg := <-m.readSegmented(m5aHLSTS, served, 0)
		m5aAV(t, served+"'s HLS", seg.c, seg.err)
		video, _ := byTrack(seg.c.Frames)
		assert.Len(t, video, 1, "%s is a single-rendition stream", served)
		m.fresh(t, stream.OutRTSP, served)
	}
	for _, f := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd", "stream.ts"} {
		assert.Equal(t, http.StatusNotFound, m.mediaGet(t, m.live("cam_abr")+f).code, "/live/cam_abr/%s", f)
	}
	var shot m5aResp
	m5aUntil(t, m5aWait, func() bool {
		shot = m.do(t, http.MethodGet, "/api/v1/streams/cam/thumbnail.jpg", nil)
		return shot.code == http.StatusOK
	}, "the API has a thumbnail")
	assert.Equal(t, "image/jpeg", shot.header.Get("Content-Type"))
	assert.Equal(t, "image/jpeg", http.DetectContentType(shot.body), "the thumbnail is a JPEG")
	assert.Equal(t, http.StatusNotFound, m.mediaGet(t, m.live("cam")+"thumbnail.jpg").code, "the media listener has no thumbnail")
	assert.Zero(t, m.metric(t, "openstreamer_transcoder_encoder_fallbacks_total", "stream", "cam"))
	assert.EqualValues(t, 1, m.metric(t, "openstreamer_transcoder_up", "stream", "cam"))
	m5aUntil(t, m5aWait, func() bool { return m.metric(t, "openstreamer_output_sessions", "output", "rtsp") == 0 }, "the RTSP readers are gone")

	// The edit: DASH off. The RTMP player of live/cam (rendition 0) and the
	// ABR HLS-TS reader are not touched, nor is the child.
	r := m5aOpen(t, m, "cam", m5aRowOpt{
		sessions: []stream.Output{stream.OutRTMP}, segmented: []string{m5aHLSTS},
		taps: []string{"cam_360", "cam_180"}, watch: stream.OutHLS,
	})
	require.NotZero(t, r.base["transcoder.pid"])
	edited := cam
	edited.Outputs.DASH = false
	m5aSleepUntil(r.start.Add(m5aEdit))
	code, applied = m.put(t, edited)
	r.edited()
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "outputs", applied)
	assert.Equal(t, http.StatusNotFound, m.mediaGet(t, m.url(stream.OutDASH, "cam")).code, "DASH is off")
	r.end(t)
	assertUninterrupted(t, r)
	assert.Equal(t, r.base["transcoder.pid"], r.last["transcoder.pid"], "the child was not touched")
	assert.Equal(t, r.base["transcoder.spawns"], r.last["transcoder.spawns"])
}

// TestM5aPeers has ffprobe read each of cam's seven URLs: one h264 video
// stream and one aac audio stream on each.
func TestM5aPeers(t *testing.T) {
	ffprobe := testpeer.Require(t, "ffprobe", "http", "rtmp", "srt")
	m := m5aServer(t, "")
	code, _ := m.put(t, m5aSpec("cam", m5aSource(t)))
	require.Equal(t, http.StatusCreated, code)
	m.running(t, "cam")
	m.listed(t, "cam", 3)
	urls := []string{
		m.url(stream.OutHLS, "cam"), m.live("cam") + "cmaf/master.m3u8", m.url(stream.OutDASH, "cam"),
		m.url(stream.OutHTTPTS, "cam"), m.url(stream.OutRTMP, "cam"), m.url(stream.OutRTSP, "cam"), m.url(stream.OutSRT, "cam"),
	}
	for _, u := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var (
			streams []string
			err     error
		)
		if strings.HasPrefix(u, "rtsp://") {
			streams, err = probeRTSPStreams(ctx, ffprobe, u, "tcp")
		} else {
			streams, err = probeStreams(ctx, ffprobe, u)
		}
		cancel()
		if !assert.NoError(t, err, u) {
			continue
		}
		// ffmpeg's FLV demuxer shows an onMetaData that is not at
		// timestamp 0, which the RTMP output sends at every epoch change,
		// as a text stream of its own: it is there only when the probe
		// crosses a loop of the file. That one stream, on the RTMP URL, is
		// all that is left out: anything else on any URL fails.
		got := streams
		if strings.HasPrefix(u, "rtmp://") {
			got = slices.DeleteFunc(slices.Clone(streams), func(s string) bool { return s == "subtitle:text" })
		}
		assert.ElementsMatch(t, []string{"video:h264", "audio:aac"}, got, "%s: streams %v", u, streams)
		t.Logf("%s: %v", u, streams)
	}
}

// m5aSecrets are what must never leave the server but where an editor asks
// for it: the stream secrets and the API users' passwords and hashes.
func m5aSecrets() (streams, users []string) {
	return []string{"hunter2-pw", "pushkey-0123456789", "listenkey-0123456789", "srtpass-0123456789"},
		[]string{m5aEditorPW, m5aViewerPW, m5aHash(m5aEditorPW), m5aHash(m5aViewerPW)}
}

// TestM5aNoSecretsLeak runs a stream whose first input and whose push hold
// a secret and are refused, so that both end up in errors; a stream whose
// first input is an SRT caller with a passphrase that nobody answers; and a
// stream that waits on an RTMP listen input, whose publish path is its key.
// Then it looks for the secrets in everything the server says.
func TestM5aNoSecretsLeak(t *testing.T) {
	const input, push = "rtsp://user:hunter2-pw@127.0.0.1:1/x", "rtmp://127.0.0.1:1/live/pushkey-0123456789"
	const srtInput = "srt://127.0.0.1:1?passphrase=srtpass-0123456789"
	m := m5aServer(t, "")
	listenInput := "rtmp://" + m.addrs.RTMP + "/in/listenkey-0123456789?mode=listen"
	cam2 := m5aSpec("cam2", input)
	cam2.Inputs = append(cam2.Inputs, spec.Input{URL: m5aSource(t)})
	cam2.Outputs.Push = []spec.Push{{URL: push}}
	code, _ := m.put(t, cam2)
	require.Equal(t, http.StatusCreated, code)
	cam5 := m5aSpec("cam5", srtInput)
	cam5.Inputs = append(cam5.Inputs, spec.Input{URL: m5aSource(t)})
	code, _ = m.put(t, cam5)
	require.Equal(t, http.StatusCreated, code)
	code, _ = m.put(t, m5aSpec("cam6", listenInput))
	require.Equal(t, http.StatusCreated, code)
	// Both failures are in the statuses before anything is read.
	m5aUntil(t, m5aWait, func() bool {
		st2, st5 := m.status(t, "cam2"), m.status(t, "cam5")
		return st2.LastError != "" && len(st2.Pushes) == 1 && st2.Pushes[0].LastError != "" && st2.ActiveInput == 1 &&
			st5.LastError != "" && st5.ActiveInput == 1
	}, "cam2 and cam5 run on their second inputs, with their failures as last errors")

	streamSecrets, userSecrets := m5aSecrets()
	all := slices.Concat(streamSecrets, userSecrets)
	clean := func(what, text string, secrets []string) {
		t.Helper()
		for _, s := range secrets {
			assert.NotContains(t, text, s, what)
		}
	}
	// A PUT with a URL that cannot be an input, carrying the password.
	bad := m5aSpec("cam3", "gopher://user:hunter2-pw@127.0.0.1:1/x")
	badBody, err := json.Marshal(bad)
	require.NoError(t, err)
	// A second stream on cam6's listen address and publish path: the
	// conflict's text names the address, never the path.
	dup, err := json.Marshal(m5aSpec("cam7", listenInput))
	require.NoError(t, err)
	eye := m.as(m5aViewer, m5aViewerPW)
	for _, who := range []*m5a{eye, m} {
		names := []string{"cam2", "cam5", "cam6"}
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
		resp := who.do(t, http.MethodPut, "/api/v1/streams/cam7", dup, "Content-Type", "application/json")
		want := http.StatusConflict
		if who == eye {
			want = http.StatusForbidden
		}
		assert.Equal(t, want, resp.code, "the PUT of a taken listen input as %s: %s", who.user, resp.body)
		clean("the answer to a PUT of a taken listen input as "+who.user, string(resp.body), all)
		resp = who.do(t, http.MethodPut, "/api/v1/streams/cam3", badBody, "Content-Type", "application/json")
		want = http.StatusUnprocessableEntity
		if who == eye {
			want = http.StatusForbidden
		}
		assert.Equal(t, want, resp.code, "the PUT of a bad URL as %s: %s", who.user, resp.body)
		clean("the answer to a PUT of a bad URL as "+who.user, string(resp.body), all)
	}
	st := m.status(t, "cam2")
	assert.NotEmpty(t, st.LastError, "cam2's last_error")
	require.Len(t, st.Pushes, 1)
	assert.NotEmpty(t, st.Pushes[0].LastError, "pushes[0].last_error")
	assert.Equal(t, 1, st.ActiveInput, "cam2 runs on its second input")
	st5 := m.status(t, "cam5")
	assert.Contains(t, st5.LastError, "srt", "cam5's last_error is its SRT input's")
	assert.NotEqual(t, "running", m.status(t, "cam6").State, "cam6 waits for a publisher")

	logs := m.logs.String()
	clean("the log", logs, all)
	require.NotEmpty(t, logs)
	for line := range strings.SplitSeq(strings.TrimSuffix(logs, "\n"), "\n") {
		var rec map[string]any
		if !assert.NoError(t, json.Unmarshal([]byte(line), &rec), "a log line is one JSON object: %s", line) {
			continue
		}
		for _, key := range []string{"time", "level", "msg"} {
			assert.Contains(t, rec, key, line)
		}
	}

	// There is no exception any more: ?secrets=1 is a 400 for every role,
	// and the editor's config text is masked (R-T6, R-T8).
	for _, name := range []string{"cam2", "cam5", "cam6"} {
		for _, who := range []*m5a{eye, m} {
			code, body := who.get(t, "/api/v1/streams/"+name+"?secrets=1")
			assert.Equal(t, http.StatusBadRequest, code, "GET %s?secrets=1 as %s", name, who.user)
			clean("GET "+name+"?secrets=1 as "+who.user, string(body), all)
		}
	}
	text, _ := m.configText(t)
	shown, err := config.Mask([]byte(m.fileText(t)))
	require.NoError(t, err)
	assert.Equal(t, string(shown), text, "GET /api/v1/config is the file masked")
	clean("GET /api/v1/config", text, slices.Concat(streamSecrets, []string{m5aEditorPW, m5aViewerPW}))
	for _, s := range []string{m5aHash(m5aEditorPW), m5aHash(m5aViewerPW)} {
		assert.Contains(t, text, s, "the users' hashes are not masked")
	}
	assert.Equal(t, http.StatusForbidden, eye.do(t, http.MethodGet, "/api/v1/config", nil).code)

	// On disk the file holds the stream secrets verbatim, which is what
	// makes a restart possible.
	p, err := config.Parse([]byte(m.fileText(t)), config.Options{RequiredIterations: m5aIter})
	require.NoError(t, err)
	require.Len(t, p.Doc.Streams, 3)
	assert.Equal(t, input, p.Doc.Streams[0].Inputs[0].URL)
	assert.Equal(t, push, p.Doc.Streams[0].Outputs.Push[0].URL)
	assert.Equal(t, srtInput, p.Doc.Streams[1].Inputs[0].URL)
	assert.Equal(t, listenInput, p.Doc.Streams[2].Inputs[0].URL)
}

// TestM5aRoles checks who may do what: nothing without credentials but
// /healthz, reads for the viewer, everything for the editor, the header a
// write needs, the login limiter, a change of the users through the config
// editor, and the media listener, which has no API.
func TestM5aRoles(t *testing.T) {
	src := m5aSource(t)
	m := m5aServer(t, "streams:\n  - name: cam\n    inputs:\n      - url: \""+src+"\"\n    outputs: {hls: true}\n")
	api1 := "http://" + m.addrs.API
	camBody, err := json.Marshal(m5aSpec("cam", src))
	require.NoError(t, err)
	newBody, err := json.Marshal(m5aSpec("new", src))
	require.NoError(t, err)

	// Without credentials.
	none := m.as("", "")
	for _, rt := range api.Routes() {
		path := strings.ReplaceAll(rt.Path, "{name}", "cam")
		resp := none.do(t, rt.Method, path, nil)
		if rt.Role == "" {
			assert.Equal(t, http.StatusOK, resp.code, "%s %s is open", rt.Method, path)
			continue
		}
		assert.Equal(t, http.StatusUnauthorized, resp.code, "%s %s", rt.Method, path)
		assert.Equal(t, `Basic realm="openstreamer"`, resp.header.Get("WWW-Authenticate"), "%s %s", rt.Method, path)
	}
	assert.Equal(t, http.StatusOK, none.do(t, http.MethodGet, "/healthz", nil).code)

	// The viewer reads, as the soak's Prometheus will.
	eye := m.as(m5aViewer, m5aViewerPW)
	for _, path := range []string{
		"/api/v1/streams", "/api/v1/streams/cam", "/api/v1/streams/cam/status", "/api/v1/config/status", "/metrics",
		"/api/v1/version", "/api/v1/openapi.json",
	} {
		assert.Equal(t, http.StatusOK, eye.do(t, http.MethodGet, path, nil).code, "the viewer reads %s", path)
	}
	// What only the editor may do: the viewer gets 403, the editor does it.
	text, etag := m.configText(t)
	for _, c := range []struct {
		method, path string
		body         []byte
		hdr          []string
		want         int
	}{
		{http.MethodPut, "/api/v1/streams/new", newBody, []string{"Content-Type", "application/json"}, http.StatusCreated},
		{http.MethodDelete, "/api/v1/streams/new", nil, nil, http.StatusNoContent},
		{http.MethodGet, "/api/v1/config", nil, nil, http.StatusOK},
		{http.MethodPost, "/api/v1/reload", nil, nil, http.StatusOK},
		{http.MethodGet, "/debug/pprof/", nil, nil, http.StatusOK},
		{http.MethodGet, "/debug/pprof/cmdline", nil, nil, http.StatusOK},
		{http.MethodGet, "/debug/pprof/profile?seconds=1", nil, nil, http.StatusOK},
		{http.MethodGet, "/debug/pprof/symbol", nil, nil, http.StatusOK},
		{http.MethodGet, "/debug/pprof/trace?seconds=1", nil, nil, http.StatusOK},
		{http.MethodGet, "/debug/pprof/heap", nil, nil, http.StatusOK},
	} {
		assert.Equal(t, http.StatusForbidden, eye.do(t, c.method, c.path, c.body, c.hdr...).code, "the viewer: %s %s", c.method, c.path)
		resp := m.do(t, c.method, c.path, c.body, c.hdr...)
		assert.Equal(t, c.want, resp.code, "the editor: %s %s: %.200s", c.method, c.path, resp.body)
	}
	// ?secrets=1 is gone: a 400 for both.
	for _, path := range []string{"/api/v1/streams/cam?secrets=1", "/api/v1/streams?secrets=1"} {
		for _, who := range []*m5a{eye, m} {
			assert.Equal(t, http.StatusBadRequest, who.do(t, http.MethodGet, path, nil).code, "%s: GET %s", who.user, path)
		}
	}
	assert.Equal(t, http.StatusForbidden, eye.putConfigRaw(t, text, etag).code, "the viewer: PUT config")
	text, etag = m.configText(t)
	code, _ := m.putConfig(t, text+"# An editor's edit.\n", etag)
	assert.Equal(t, http.StatusOK, code, "the editor: PUT config")

	// A write without X-Requested-By, whoever sends it.
	for _, c := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPut, "/api/v1/streams/cam", camBody},
		{http.MethodDelete, "/api/v1/streams/cam", nil},
		{http.MethodPost, "/api/v1/reload", nil},
		{http.MethodPut, "/api/v1/config", []byte(text)},
	} {
		resp := m5aDo(t, c.method, api1+c.path, m5aEditor, m5aEditorPW, c.body, "Content-Type", "application/json")
		assert.Equal(t, http.StatusBadRequest, resp.code, "%s %s without X-Requested-By: %s", c.method, c.path, resp.body)
	}
	assert.Equal(t, "running", m.running(t, "cam").State)

	// A PUT of the config that replaces the editor's hash: the old password
	// is refused and the new one served on the next request.
	const newPW = "ops-second-password"
	text, etag = m.configText(t)
	code, _ = m.putConfig(t, m5aOnce(t, text, m5aHash(m5aEditorPW), m5aHash(newPW)), etag)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusUnauthorized, m.do(t, http.MethodGet, "/api/v1/streams", nil).code, "the old password")
	ops := m.as(m5aEditor, newPW)
	assert.Equal(t, http.StatusOK, ops.do(t, http.MethodGet, "/api/v1/streams", nil).code, "the new password")
	// One that leaves no editor is refused.
	text, etag = ops.configText(t)
	resp := ops.putConfigRaw(t, m5aOnce(t, text, "role: editor", "role: viewer"), etag)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.code, "%s", resp.body)
	assert.Equal(t, http.StatusOK, ops.do(t, http.MethodGet, "/api/v1/config", nil).code, "the editor is still one")

	// The media listener serves /live/ without credentials and has no API.
	ops.listed(t, "cam", 1)
	media1 := "http://" + m.addrs.HTTP
	assert.Equal(t, http.StatusOK, m.mediaGet(t, m.url(stream.OutHLS, "cam")).code)
	for _, path := range []string{"/api/v1/streams", "/api/v1/config", "/metrics", "/healthz", "/debug/pprof/"} {
		assert.Equal(t, http.StatusNotFound, m5aDo(t, http.MethodGet, media1+path, m5aEditor, newPW, nil).code, "the media listener: %s", path)
	}

	// Six wrong passwords for eye: the sixth is not even tried. ops, from
	// the same address, is still served.
	wrong := m.as(m5aViewer, "not-the-password")
	for i := 1; i <= 5; i++ {
		assert.Equal(t, http.StatusUnauthorized, wrong.do(t, http.MethodGet, "/api/v1/streams", nil).code, "wrong password %d", i)
	}
	resp = wrong.do(t, http.MethodGet, "/api/v1/streams", nil)
	assert.Equal(t, http.StatusTooManyRequests, resp.code, "the sixth wrong password")
	assert.NotEmpty(t, resp.header.Get("Retry-After"))
	assert.Equal(t, http.StatusOK, ops.do(t, http.MethodGet, "/api/v1/streams", nil).code, "ops is still served")
	assert.Equal(t, http.StatusTooManyRequests, eye.do(t, http.MethodGet, "/api/v1/streams", nil).code, "eye is blocked, right password or not")
}
