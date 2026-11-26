package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// waitFor bounds every wait of these tests.
const waitFor = 15 * time.Second

const (
	editorPW = "editor-password-1"
	viewerPW = "viewer-password-1"
	testIter = 1000
)

// hashOf is the hash of password at the tests' iteration count.
func hashOf(password string) string {
	return auth.NewHash(password, []byte("0123456789abcdef"), testIter).String()
}

// serverYAML is the server section of the tests: every listener on an
// ephemeral loopback port.
const serverYAML = `server:
  http: {listen: "127.0.0.1:0"}
  api: {listen: "127.0.0.1:0"}
  rtmp: {listen: "127.0.0.1:0"}
  rtsp: {listen: "127.0.0.1:0"}
  srt: {listen: "127.0.0.1:0"}
`

// usersYAML is the users section of the tests.
func usersYAML() string {
	return "users:\n" +
		"  - name: ops\n    role: editor\n    password: \"" + hashOf(editorPW) + "\"\n" +
		"  - name: eye\n    role: viewer\n    password: \"" + hashOf(viewerPW) + "\"\n"
}

// streamYAML is a stream entry named name with the outputs outs (a YAML
// flow mapping, as "{hls: true}").
func streamYAML(name, outs string) string {
	return "  - name: " + name + "\n    inputs:\n      - url: rtmp://src.example/live/" + name + "\n    outputs: " + outs + "\n"
}

// baseText is a whole config with the given stream entries.
func baseText(streams ...string) string {
	text := "version: 1\n" + serverYAML + usersYAML()
	if len(streams) == 0 {
		return text + "streams: []\n"
	}
	return text + "streams:\n" + strings.Join(streams, "")
}

// setup is what testConfig starts a server from; mutate changes it.
type setup struct {
	Text string
	Opt  Options
}

// testServer is a started server and its config file.
type testServer struct {
	*Server
	t    *testing.T
	path string
	logs *lockedBuffer
	stop func() // cancels Run and waits for it; idempotent
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// writeConfig writes text as a config file of mode 0600 in a new directory.
func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

// testConfig writes a config file with every listener on 127.0.0.1:0 and
// the users ops (editor) and eye (viewer), starts a server from it with a
// scripted input for every stream, and stops it when the test ends. mutate
// may change the text and the options first.
func testConfig(t *testing.T, mutate func(*setup)) *testServer {
	t.Helper()
	logs := &lockedBuffer{}
	su := &setup{
		Text: baseText(),
		Opt:  Options{Version: "test", LogOut: logs, RequiredIterations: testIter, Open: newScript(t).Open},
	}
	if mutate != nil {
		mutate(su)
	}
	if su.Opt.LogOut == nil {
		su.Opt.LogOut = logs
	}
	path := writeConfig(t, su.Text)
	srv, err := New(path, su.Opt)
	require.NoError(t, err)
	return start(t, srv, path, logs)
}

// start runs srv until the test ends.
func start(t *testing.T, srv *Server, path string, logs *lockedBuffer) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				assert.NoError(t, err)
			case <-time.After(waitFor):
				assert.Fail(t, "Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	return &testServer{Server: srv, t: t, path: path, logs: logs, stop: stop}
}

// response is an API response.
type response struct {
	code   int
	body   string
	header http.Header
}

var client = &http.Client{Timeout: waitFor}

// do sends one API request as user ("ops", "eye" or "" for none). body is
// sent as JSON unless ctype names another type; hdr are extra header pairs.
func (s *testServer) do(method, path, user, body string, hdr ...string) response {
	s.t.Helper()
	return s.doCtx(context.Background(), method, path, user, body, hdr...)
}

func (s *testServer) doCtx(ctx context.Context, method, path, user, body string, hdr ...string) response {
	s.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+s.Addrs().API+path, strings.NewReader(body))
	require.NoError(s.t, err)
	switch user {
	case "ops":
		req.SetBasicAuth("ops", editorPW)
	case "eye":
		req.SetBasicAuth("eye", viewerPW)
	}
	req.Header.Set("X-Requested-By", "test")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := client.Do(req)
	require.NoError(s.t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(s.t, err)
	return response{code: res.StatusCode, body: string(b), header: res.Header}
}

// putConfig sends a whole-config PUT with the current ETag.
func (s *testServer) putConfig(text string) response {
	s.t.Helper()
	_, etag := s.ConfigText()
	return s.do(http.MethodPut, "/api/v1/config", "ops", text, "Content-Type", "application/yaml", "If-Match", etag)
}

// streamJSON is a stream document for a per-stream PUT.
func streamJSON(name, input, outputs string) string {
	return fmt.Sprintf(`{"name":%q,"inputs":[{"url":%q}],"outputs":%s}`, name, input, outputs)
}

// status is the config status as the API gives it.
func (s *testServer) status() api.ConfigStatus {
	s.t.Helper()
	res := s.do(http.MethodGet, "/api/v1/config/status", "ops", "")
	require.Equal(s.t, http.StatusOK, res.code, res.body)
	var st api.ConfigStatus
	require.NoError(s.t, json.Unmarshal([]byte(res.body), &st))
	return st
}

// metric returns one series of /metrics.
func (s *testServer) metric(name string, labels ...string) float64 {
	s.t.Helper()
	res := s.do(http.MethodGet, "/metrics", "ops", "")
	require.Equal(s.t, http.StatusOK, res.code, res.body)
	samples, err := metricstest.Parse(strings.NewReader(res.body))
	require.NoError(s.t, err)
	v, ok := metricstest.Find(samples, name, labels...)
	require.True(s.t, ok, "no series %s %v", name, labels)
	return v
}

// fileText is the config file as it is on disk.
func (s *testServer) fileText() string {
	s.t.Helper()
	b, err := os.ReadFile(s.path)
	require.NoError(s.t, err)
	return string(b)
}

// handEdit rewrites the config file, as an operator's editor would.
func (s *testServer) handEdit(text string) {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(s.path, []byte(text), 0o600))
}

// generation is the generation of a stream, once it runs.
func (s *testServer) generation(name string) uint64 {
	s.t.Helper()
	var gen uint64
	require.Eventually(s.t, func() bool {
		st, ok := s.Manager().Status(name)
		gen = st.Generation
		return ok && st.State == "running"
	}, waitFor, 5*time.Millisecond, "stream %s does not run", name)
	return gen
}

// applied is the outcome of a stream in an apply result.
func applied(t *testing.T, res api.ApplyResult, name string) string {
	t.Helper()
	for _, o := range res.Streams {
		if o.Name == name {
			return o.Applied
		}
	}
	require.Fail(t, "no outcome for "+name)
	return ""
}

var fixture struct {
	once   sync.Once
	frames []media.Frame
	kinds  []media.Kind
	span   int64
	err    error
}

// script is an Options.Open whose readers emit the frames of
// testdata/fixtures/h264_aac.ts in a loop. While gate is set, a reader's
// Close blocks until it is closed: that is how a test holds a teardown.
type script struct {
	frames []media.Frame
	kinds  []media.Kind
	span   int64

	mu   sync.Mutex
	gate chan struct{}
	urls []string
}

func newScript(t testing.TB) *script {
	t.Helper()
	fixture.once.Do(func() {
		data, err := os.ReadFile("../../testdata/fixtures/h264_aac.ts")
		if err != nil {
			fixture.err = err
			return
		}
		d, err := ts.NewDemuxer(bytes.NewReader(data))
		if err != nil {
			fixture.err = err
			return
		}
		for {
			f, err := d.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fixture.err = err
				return
			}
			fixture.frames = append(fixture.frames, f)
		}
		fixture.kinds = d.TrackKinds()
		lo, hi := fixture.frames[0].DTS, fixture.frames[0].DTS
		for _, f := range fixture.frames {
			lo, hi = min(lo, f.DTS), max(hi, f.DTS)
		}
		fixture.span = hi - lo + 3600
	})
	require.NoError(t, fixture.err)
	return &script{frames: fixture.frames, kinds: fixture.kinds, span: fixture.span}
}

func (s *script) Open(_ context.Context, u string) (ingest.Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urls = append(s.urls, u)
	return &scriptReader{s: s, closed: make(chan struct{})}, nil
}

// hold makes every Close from now on block until the returned release.
func (s *script) hold() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.gate = nil
			s.mu.Unlock()
			close(gate)
		})
	}
}

// opened reports whether a reader was opened for the URL u.
func (s *script) opened(u string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.urls {
		if x == u {
			return true
		}
	}
	return false
}

type scriptReader struct {
	s      *script
	n      int
	closed chan struct{}
	once   sync.Once
}

func (r *scriptReader) Read() (ingest.Frame, error) {
	select {
	case <-r.closed:
		return ingest.Frame{}, errors.New("script: closed")
	case <-time.After(2 * time.Millisecond):
	}
	f := r.s.frames[r.n%len(r.s.frames)]
	shift := int64(r.n/len(r.s.frames)) * r.s.span
	f.PTS += shift
	f.DTS += shift
	r.n++
	return ingest.Frame{Frame: f, Arrival: time.Now()}, nil
}

func (r *scriptReader) Kinds() []media.Kind      { return r.s.kinds }
func (r *scriptReader) Spec() ingest.Spec        { return ingest.Spec{} }
func (r *scriptReader) Stats() map[string]uint64 { return nil }

func (r *scriptReader) Close() error {
	r.s.mu.Lock()
	gate := r.s.gate
	r.s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	r.once.Do(func() { close(r.closed) })
	return nil
}

// loopURL is the looped fixture as a file input.
func loopURL(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	return (&url.URL{Scheme: "file", Path: p, RawQuery: "loop=1"}).String()
}

// fileStream is a stream entry reading the looped fixture.
func fileStream(t *testing.T, name, outs string) string {
	t.Helper()
	return "  - name: " + name + "\n    inputs:\n      - url: \"" + loopURL(t) + "\"\n    outputs: " + outs + "\n"
}

// port is the port of a host:port.
func port(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	return p
}

// mediaGet fetches a path of the media listener.
func (s *testServer) mediaGet(path string) int {
	s.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+s.Addrs().HTTP+path, nil)
	require.NoError(s.t, err)
	res, err := client.Do(req)
	require.NoError(s.t, err)
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return res.StatusCode
}

func TestServerStartsAndStops(t *testing.T) {
	client.CloseIdleConnections()
	before := runtime.NumGoroutine()
	s := testConfig(t, nil)
	a := s.Addrs()
	ports := map[string]bool{}
	for _, addr := range []string{a.HTTP, a.API, a.RTMP, a.RTSP, a.SRT} {
		p := port(t, addr)
		require.NotEqual(t, "0", p)
		ports[p] = true
	}
	require.Len(t, ports, 5, "%+v", a)

	c, err := (&net.Dialer{Timeout: waitFor}).DialContext(context.Background(), "tcp", a.RTMP)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	require.Equal(t, http.StatusOK, s.do(http.MethodGet, "/healthz", "", "").code)
	require.Equal(t, http.StatusNotFound, s.mediaGet("/live/nope/hls/index.m3u8"))
	require.Equal(t, http.StatusNotFound, s.mediaGet("/live/nope/stream.ts"))

	stopped := make(chan struct{})
	go func() { s.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		require.Fail(t, "Run did not return within 5 s")
	}
	// Every address can be bound again. A refused connection would prove
	// nothing for UDP, so each one is bound.
	for _, addr := range []string{a.HTTP, a.API, a.RTMP, a.RTSP} {
		require.NoError(t, listenTCP(t, addr).Close())
	}
	require.NoError(t, listenUDP(t, a.SRT).Close())

	client.CloseIdleConnections()
	// Not require.Eventually: it polls from goroutines of its own.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), before)
}

func TestResolveRTMP(t *testing.T) {
	addr, err := resolveRTMP("127.0.0.1:0")
	require.NoError(t, err)
	host, p, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", p)
	require.NoError(t, listenTCP(t, addr).Close())

	for _, same := range []string{"127.0.0.1:19351", ":19351"} {
		got, err := resolveRTMP(same)
		require.NoError(t, err)
		assert.Equal(t, same, got)
	}
	_, err = resolveRTMP("nope")
	require.Error(t, err)
}

func TestTwoServersOneProcess(t *testing.T) {
	for i := range 10 {
		var (
			wg   sync.WaitGroup
			srvs [2]*Server
			errs [2]error
			path [2]string
		)
		for j := range srvs {
			path[j] = writeConfig(t, baseText())
		}
		for j := range srvs {
			wg.Go(func() {
				srvs[j], errs[j] = New(path[j], Options{LogOut: io.Discard, RequiredIterations: testIter})
			})
		}
		wg.Wait()
		started := make([]*testServer, 0, len(srvs))
		for j, srv := range srvs {
			require.NoError(t, errs[j], "round %d", i)
			started = append(started, start(t, srv, path[j], nil))
		}
		assert.NotEqual(t, port(t, srvs[0].Addrs().RTMP), port(t, srvs[1].Addrs().RTMP), "round %d", i)
		for _, s := range started {
			s.stop()
		}
	}
}

func TestServerListenersOff(t *testing.T) {
	s := testConfig(t, func(su *setup) {
		su.Opt.Open = nil
		su.Text = "version: 1\nserver:\n  http: {listen: \"127.0.0.1:0\"}\n  api: {listen: \"127.0.0.1:0\"}\n" + usersYAML() +
			"streams:\n" + fileStream(t, "cam", "{hls: true, httpts: true}") + fileStream(t, "off", "{hls: true, srt: true}")
	})
	a := s.Addrs()
	assert.Empty(t, a.RTMP)
	assert.Empty(t, a.RTSP)
	assert.Empty(t, a.SRT)
	assert.NotEmpty(t, a.HTTP)
	require.Eventually(t, func() bool { return s.mediaGet("/live/cam/hls/index.m3u8") == http.StatusOK },
		waitFor, 50*time.Millisecond)
	// A stream that turns on an output whose listener is off is left
	// stopped at the start: nobody could play that output.
	errs := s.ConfigStatus().LoadErrors
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], `stream "off": check: outputs.srt: the server's srt listener is off`)
	st, ok := s.Status("off")
	require.True(t, ok)
	assert.Equal(t, "stopped", st.State)
	// And it is refused over the API, one output at a time.
	for _, out := range []string{"rtmp", "rtsp", "srt"} {
		res := s.do(http.MethodPut, "/api/v1/streams/two", "ops", streamJSON("two", loopURL(t), `{"hls":true,"`+out+`":true}`))
		require.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		assert.Contains(t, res.body, `"code":"invalid_spec"`)
		assert.Contains(t, res.body, "outputs."+out+": the server's "+out+" listener is off: set server."+out+".listen")
	}
	res := s.do(http.MethodPut, "/api/v1/streams/two", "ops", streamJSON("two", loopURL(t), `{"hls":true,"httpts":true}`))
	require.Equal(t, http.StatusCreated, res.code, res.body)
	// Turning the output off is the repair.
	res = s.do(http.MethodPut, "/api/v1/streams/off", "ops", streamJSON("off", loopURL(t), `{"hls":true}`))
	require.Equal(t, http.StatusOK, res.code, res.body)
	assert.Contains(t, res.body, `"applied":"started"`)
	assert.Empty(t, s.ConfigStatus().LoadErrors)
	// Without an output server its series are absent, and the scrape parses.
	out := s.do(http.MethodGet, "/metrics", "eye", "")
	require.Equal(t, http.StatusOK, out.code)
	_, err := metricstest.Parse(strings.NewReader(out.body))
	require.NoError(t, err)
	assert.NotContains(t, out.body, "openstreamer_output_sessions{")
}

// An API beyond loopback is warned about once, at the start; one on
// loopback is not.
func TestWarnAPI(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8081": true, "127.8.9.1:1": true, "[::1]:8081": true, "[::ffff:127.0.0.1]:8081": true,
		"0.0.0.0:8081": false, "[::]:8081": false, "192.0.2.7:8081": false, "[2001:db8::1]:8081": false,
		"nonsense": false, "localhost:1": false, // a bound address is never a name
	} {
		assert.Equal(t, want, loopback(addr), addr)
		var b bytes.Buffer
		log, err := logging.New(&b, "info", "json")
		require.NoError(t, err)
		warnAPI(log, addr)
		if want {
			assert.Empty(t, b.String(), addr)
			continue
		}
		assert.Equal(t, 1, strings.Count(b.String(), "\n"), addr)
		assert.Contains(t, b.String(), `"level":"WARN"`)
		assert.Contains(t, b.String(), "api.listen is not a loopback address")
		assert.Contains(t, b.String(), "travel in clear")
	}
	s := testConfig(t, nil)
	assert.True(t, loopback(s.Addrs().API))
	assert.NotContains(t, s.logs.String(), "api.listen is not a loopback")
}

func TestServerReserved(t *testing.T) {
	s := testConfig(t, func(su *setup) { su.Opt.Open = nil })
	a := s.Addrs()
	put := func(name, input string) response {
		return s.do(http.MethodPut, "/api/v1/streams/"+name, "ops", streamJSON(name, input, `{"hls":true}`))
	}
	res := put("u", "udp://127.0.0.1:"+port(t, a.SRT))
	assert.Equal(t, http.StatusConflict, res.code, res.body)
	for _, addr := range []string{a.HTTP, a.API, a.RTSP} {
		res := put("r", "rtmp://"+addr+"/in/key?mode=listen")
		assert.Equal(t, http.StatusConflict, res.code, "%s: %s", addr, res.body)
	}
	// On the play address a listen input shares the listener.
	res = put("ok", "rtmp://"+a.RTMP+"/in/key?mode=listen")
	assert.Equal(t, http.StatusCreated, res.code, res.body)
	// But not under the play app, whose paths are the players'.
	res = put("shadow", "rtmp://"+a.RTMP+"/live/anything?mode=listen")
	assert.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
	assert.Len(t, s.List(), 1)
}

func TestServerMetricsParse(t *testing.T) {
	s := testConfig(t, func(su *setup) {
		su.Opt.Open = nil
		su.Text = baseText(fileStream(t, "cam", "{rtmp: true}"))
	})
	s.generation("cam")
	var capture conform.Capture
	require.Eventually(t, func() bool {
		var err error
		capture, err = conform.ReadRTMP(context.Background(), "rtmp://"+s.Addrs().RTMP+"/live/cam",
			conform.ReadOptions{Duration: time.Second, Timeout: 5 * time.Second})
		return err == nil && len(capture.Frames) > 0
	}, waitFor, 100*time.Millisecond)

	res := s.do(http.MethodGet, "/metrics", "eye", "")
	require.Equal(t, http.StatusOK, res.code, res.body)
	samples, err := metricstest.Parse(strings.NewReader(res.body))
	require.NoError(t, err)
	find := func(name string, labels ...string) float64 {
		v, ok := metricstest.Find(samples, name, labels...)
		require.True(t, ok, "%s %v", name, labels)
		return v
	}
	assert.InDelta(t, 1, find("openstreamer_build_info", "version", "test", "goversion", runtime.Version()), 0)
	assert.Positive(t, find("go_goroutines"))
	assert.InDelta(t, 1, find("openstreamer_stream_up", "stream", "cam"), 0)
	assert.InDelta(t, 1, find("openstreamer_output_sessions_started_total", "output", "rtmp"), 0)
	assert.InDelta(t, 0, find("openstreamer_config_load_errors"), 0)
	assert.InDelta(t, 0, find("openstreamer_config_restart_required"), 0)
	assert.InDelta(t, 0, find("openstreamer_config_reloads_total", "result", "ok"), 0)
	assert.InDelta(t, 0, find("openstreamer_output_sessions", "output", "srt"), 0)
	assert.InDelta(t, 0, find("openstreamer_output_rejected_total", "output", "srt", "reason", "mss"), 0)
	assert.InDelta(t, 0, find("openstreamer_output_sessions_ended_total", "output", "rtsp", "reason", "tracks_gone"), 0)
	assert.InDelta(t, 0, find("openstreamer_output_resyncs_total", "output", "rtmp"), 0)
	// The metrics are a viewer's, and nobody else's.
	assert.Equal(t, http.StatusUnauthorized, s.do(http.MethodGet, "/metrics", "", "").code)
}

func TestServerBindFailure(t *testing.T) {
	held := listenTCP(t, "127.0.0.1:0")
	defer held.Close()
	heldUDP := listenUDP(t, "127.0.0.1:0")
	defer heldUDP.Close()
	// Every port is named, so that the test can see the others free again;
	// they are ports that were free a moment ago.
	free := func(network string) string {
		if network == "udp" {
			pc := listenUDP(t, "127.0.0.1:0")
			defer pc.Close()
			return pc.LocalAddr().String()
		}
		ln := listenTCP(t, "127.0.0.1:0")
		defer ln.Close()
		return ln.Addr().String()
	}
	keys := []string{"http", "api", "rtmp", "rtsp", "srt"}
	for _, failing := range keys {
		addrs := map[string]string{"http": free("tcp"), "api": free("tcp"), "rtmp": free("tcp"), "rtsp": free("tcp"), "srt": free("udp")}
		addrs[failing] = held.Addr().String()
		if failing == "srt" {
			addrs[failing] = heldUDP.LocalAddr().String()
		}
		text := "version: 1\nserver:\n"
		for _, k := range keys {
			text += "  " + k + ": {listen: \"" + addrs[k] + "\"}\n"
		}
		text += usersYAML()
		_, err := New(writeConfig(t, text), Options{LogOut: io.Discard, RequiredIterations: testIter})
		require.Error(t, err, failing)
		assert.Contains(t, err.Error(), addrs[failing], failing)
		assert.Contains(t, err.Error(), failing+".listen", failing)
		// No other listener is left bound.
		for _, k := range keys {
			switch k {
			case failing:
			case "srt":
				require.NoError(t, listenUDP(t, addrs[k]).Close(), "%s fails: %s is still bound", failing, k)
			default:
				require.NoError(t, listenTCP(t, addrs[k]).Close(), "%s fails: %s is still bound", failing, k)
			}
		}
	}
}

// listenTCP binds addr, which must be free.
func listenTCP(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	require.NoError(t, err, addr)
	return ln
}

// listenUDP binds addr, which must be free.
func listenUDP(t *testing.T, addr string) net.PacketConn {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", addr)
	require.NoError(t, err, addr)
	return pc
}

func TestTranscoderBeside(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "openstreamer")
	assert.Empty(t, transcoderBeside(exe))
	name := transcoderName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o700))
	assert.Empty(t, transcoderBeside(exe), "a directory is no transcoder")
	require.NoError(t, os.Remove(filepath.Join(dir, name)))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o700))
	assert.Equal(t, filepath.Join(dir, name), transcoderBeside(exe))
	// The test binary has none beside it, and the file names none: the
	// streams run without one, and the config in force still says "".
	s := testConfig(t, nil)
	assert.Empty(t, defaultTranscoder())
	assert.Empty(t, s.started.Transcoder.Binary)
}

func TestServerRTSPOverUDP(t *testing.T) {
	s := testConfig(t, func(su *setup) {
		su.Text = strings.Replace(baseText(), `rtsp: {listen: "127.0.0.1:0"}`, `rtsp: {listen: "127.0.0.1:0", udp: true}`, 1)
	})
	ports := s.rtsp.UDPPorts()
	assert.NotZero(t, ports[0])
	assert.NotZero(t, ports[1])
	s.stop()
	// The pair is free again.
	require.NoError(t, listenUDP(t, fmt.Sprintf("127.0.0.1:%d", ports[0])).Close())
}

// The wait for the RTSP server's UDP pair ends: with the pair, with Serve's
// error (a nil one too), or at the deadline.
func TestWaitUDP(t *testing.T) {
	bound := func() [2]int { return [2]int{5000, 5001} }
	never := func() [2]int { return [2]int{} }
	served := make(chan error, 1)

	ended, err := waitUDP(bound, served, time.Second)
	require.NoError(t, err)
	assert.False(t, ended)

	var calls atomic.Int64
	late := func() [2]int {
		if calls.Add(1) < 5 {
			return [2]int{}
		}
		return bound()
	}
	_, err = waitUDP(late, served, waitFor)
	require.NoError(t, err)

	boom := errors.New("listen udp 127.0.0.1:5000: address already in use")
	served <- boom
	ended, err = waitUDP(never, served, waitFor)
	require.ErrorIs(t, err, boom)
	assert.True(t, ended)

	served <- nil
	ended, err = waitUDP(never, served, waitFor)
	require.ErrorContains(t, err, "stopped before it bound its UDP ports")
	assert.True(t, ended)

	start := time.Now()
	ended, err = waitUDP(never, served, 30*time.Millisecond)
	require.ErrorContains(t, err, "the UDP ports were not bound within 30ms")
	assert.False(t, ended)
	assert.Less(t, time.Since(start), waitFor)

	require.NoError(t, serveErr(rtsp.ErrServerClosed))
	require.ErrorIs(t, serveErr(boom), boom)
	require.ErrorContains(t, serveErr(nil), "stopped serving")
}

// A UDP pair that cannot be bound is a start-up error, and nothing is left
// listening.
func TestServerRTSPUDPBindFails(t *testing.T) {
	// The RTSP server binds an even/odd pair of its own choosing, so the
	// failure is made by a host that cannot be bound.
	text := strings.Replace(baseText(), `rtsp: {listen: "127.0.0.1:0"}`, `rtsp: {listen: "127.0.0.1:0", udp: true}`, 1)
	s := &Server{failed: make(chan error, 4), stopRTSP: func() {}}
	ln := listenTCP(t, "127.0.0.1:0")
	sec := testParse(t, text).Doc.Server.Resolved()
	sec.RTSP.Listen = "192.0.2.123:0" // TEST-NET-1: not an address of this host
	err := s.startRTSP(ln, sec)
	require.Error(t, err)
	assert.Nil(t, s.rtsp)
	_, err = ln.Accept()
	require.Error(t, err, "the listener is closed")
}

func TestListenerFailsAfterStart(t *testing.T) {
	srv, err := New(writeConfig(t, baseText(streamYAML("cam", "{hls: true}"))),
		Options{LogOut: io.Discard, RequiredIterations: testIter, Open: newScript(t).Open})
	require.NoError(t, err)
	a := srv.Addrs()
	done := make(chan error, 1)
	go func() { done <- srv.Run(context.Background()) }()
	// The API listener is closed under the server.
	require.NoError(t, srv.apiLn.Close())
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server: api: ")
	case <-time.After(waitFor):
		require.Fail(t, "Run did not end on the listener's failure")
	}
	// The same stop as for a cancelled ctx: everything is released.
	for _, addr := range []string{a.HTTP, a.API, a.RTMP, a.RTSP} {
		require.NoError(t, listenTCP(t, addr).Close())
	}
	require.NoError(t, listenUDP(t, a.SRT).Close())
	st, ok := srv.Status("cam")
	require.True(t, ok)
	assert.NotEqual(t, "running", st.State)
}

func TestStatusAndShotThroughTheAPI(t *testing.T) {
	s := testConfig(t, func(su *setup) {
		su.Text = baseText(streamYAML("cam", "{hls: true}"),
			"  - name: off\n    disabled: true\n    inputs:\n      - url: rtmp://src.example/live/off\n    outputs: {hls: true}\n")
	})
	s.generation("cam")
	for _, name := range []string{"cam", "off"} {
		res := s.do(http.MethodGet, "/api/v1/streams/"+name+"/status", "eye", "")
		require.Equal(t, http.StatusOK, res.code, res.body)
		// A stream that never ran has no zero time in its status.
		assert.NotContains(t, res.body, "0001-01-01", name)
	}
	assert.Contains(t, s.do(http.MethodGet, "/api/v1/streams/off/status", "eye", "").body, `"state":"stopped"`)
	res := s.do(http.MethodGet, "/api/v1/streams/cam/thumbnail.jpg", "eye", "")
	assert.Equal(t, http.StatusNotFound, res.code, "a stream without a thumbnail")
	// The config is an editor's.
	assert.Equal(t, http.StatusForbidden, s.do(http.MethodGet, "/api/v1/config", "eye", "").code)
	res = s.do(http.MethodGet, "/api/v1/config", "ops", "")
	require.Equal(t, http.StatusOK, res.code)
	assert.Equal(t, s.fileText(), res.body)
	_, etag := s.ConfigText()
	assert.Equal(t, etag, res.header.Get("ETag"))
}
