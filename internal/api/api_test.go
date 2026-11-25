package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

const (
	edPass = "editor password 1"
	viPass = "viewer password 1"
)

// testHash hashes at 1000 iterations so the tests stay fast.
func testHash(pw string) auth.Hash {
	return auth.NewHash(pw, []byte("0123456789abcdef"), 1000)
}

func usersWith(t *testing.T, ed, vi string) *auth.Users {
	t.Helper()
	u, err := auth.NewUsers([]auth.User{
		{Name: "ops", Role: auth.Editor, Hash: testHash(ed)},
		{Name: "eye", Role: auth.Viewer, Hash: testHash(vi)},
	})
	require.NoError(t, err)
	return u
}

// fakeClock is a clock the tests move.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuffer is a log sink several requests write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lines returns the log's records with the given message.
func (s *syncBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

type shotAt struct {
	shot picture.Shot
	at   time.Time
}

// fakeBackend is a Backend that keeps streams in a map, records its calls
// and returns the errors a test sets.
type fakeBackend struct {
	mu       sync.Mutex
	streams  map[string]spec.Stream
	statuses map[string]stream.Status
	shots    map[string]shotAt
	text     []byte
	etag     string
	cstatus  ConfigStatus
	result   ApplyResult

	putErr, delErr, configErr, reloadErr error
	validate                             bool // PutStream runs Validate, as the real apply path does
	panics                               bool

	calls     []string // "Get cam", "PutStream cam", ...
	ifMatch   []string
	gotConfig []byte
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		streams:  map[string]spec.Stream{},
		statuses: map[string]stream.Status{},
		shots:    map[string]shotAt{},
		text:     []byte("version: 1\n"),
		etag:     config.Hash([]byte("version: 1\n")),
	}
}

func (f *fakeBackend) call(s string) {
	if f.panics {
		panic("the backend panics: rtsp://u:hunter2@h/a")
	}
	f.calls = append(f.calls, s)
}

func (f *fakeBackend) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBackend) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.ifMatch, f.gotConfig = nil, nil, nil
}

func (f *fakeBackend) Get(name string) (spec.Stream, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("Get " + name)
	sp, ok := f.streams[name]
	return sp, ok
}

func (f *fakeBackend) List() []spec.Stream {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("List")
	out := make([]spec.Stream, 0, len(f.streams))
	for _, sp := range f.streams { // unsorted on purpose
		out = append(out, sp)
	}
	return out
}

func (f *fakeBackend) Status(name string) (stream.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("Status " + name)
	st, ok := f.statuses[name]
	return st, ok
}

func (f *fakeBackend) Shot(name string) (picture.Shot, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("Shot " + name)
	s, ok := f.shots[name]
	return s.shot, s.at, ok
}

func (f *fakeBackend) ConfigText() ([]byte, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ConfigText")
	return f.text, f.etag
}

func (f *fakeBackend) ConfigStatus() ConfigStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ConfigStatus")
	return f.cstatus
}

func (f *fakeBackend) PutConfig(_ context.Context, text []byte, ifMatch string) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("PutConfig")
	f.ifMatch = append(f.ifMatch, ifMatch)
	f.gotConfig = append([]byte(nil), text...)
	return f.result, f.configErr
}

func (f *fakeBackend) PutStream(_ context.Context, sp spec.Stream, ifMatch string) (stream.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("PutStream " + sp.Name)
	f.ifMatch = append(f.ifMatch, ifMatch)
	if f.putErr != nil {
		return stream.Outcome{}, f.putErr
	}
	if f.validate {
		if err := sp.Validate(); err != nil {
			return stream.Outcome{}, config.Errors{err}
		}
	}
	applied := "created"
	if old, ok := f.streams[sp.Name]; ok {
		applied = "rebuild"
		if old.ETag() == sp.ETag() {
			applied = "unchanged"
		}
	}
	f.streams[sp.Name] = sp
	return stream.Outcome{Name: sp.Name, Applied: applied}, nil
}

func (f *fakeBackend) DeleteStream(_ context.Context, name, ifMatch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("DeleteStream " + name)
	f.ifMatch = append(f.ifMatch, ifMatch)
	if f.delErr != nil {
		return f.delErr
	}
	if _, ok := f.streams[name]; !ok {
		return config.ErrNoStream
	}
	delete(f.streams, name)
	return nil
}

func (f *fakeBackend) Reload(context.Context) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("Reload")
	return f.result, f.reloadErr
}

// env is an API over a fake backend, with the users ops (editor) and eye
// (viewer), a limiter on a fake clock and a captured log.
type env struct {
	t          *testing.T
	api        *API
	be         *fakeBackend
	lim        *auth.Limiter
	clock      *fakeClock
	logs       *syncBuffer
	users      atomic.Pointer[auth.Users]
	usersCalls atomic.Int64
	metricsHit atomic.Int64
}

const camSpec = `{"inputs":[{"url":"udp://239.0.0.1:5000"}],"outputs":{"hls":true}}`

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t:     t,
		be:    newFakeBackend(),
		clock: &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)},
		logs:  &syncBuffer{},
	}
	e.lim = auth.NewLimiter(e.clock.Now)
	e.users.Store(usersWith(t, edPass, viPass))
	a, err := New(Options{
		Backend: e.be,
		Users: func() *auth.Users {
			e.usersCalls.Add(1)
			return e.users.Load()
		},
		Limiter: e.lim,
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			e.metricsHit.Add(1)
			_, _ = w.Write([]byte("# the metrics\n"))
		}),
		Version: "v2.0.0-test",
		Logger:  slog.New(slog.NewJSONHandler(e.logs, nil)),
		Now:     e.clock.Now,
	})
	require.NoError(t, err)
	e.api = a
	return e
}

// seedCam stores the stream cam with a status and a shot.
func (e *env) seedCam() {
	sp, err := spec.Decode(strings.NewReader(camSpec))
	require.NoError(e.t, err)
	sp.Name = "cam"
	e.be.streams["cam"] = sp
	e.be.statuses["cam"] = stream.Status{Name: "cam", State: "running", Generation: 2, ActiveInput: 0}
	e.be.shots["cam"] = shotAt{
		shot: picture.Shot{Epoch: 3, PTS: 90000, Width: 2, Height: 2, JPEG: []byte("\xff\xd8 a jpeg \xff\xd9")},
		at:   time.Date(2026, 11, 26, 11, 59, 0, 0, time.UTC),
	}
}

type reqOpt func(*http.Request)

func basic(user, pass string) reqOpt { return func(r *http.Request) { r.SetBasicAuth(user, pass) } }
func from(addr string) reqOpt        { return func(r *http.Request) { r.RemoteAddr = addr } }
func hdr(k, v string) reqOpt         { return func(r *http.Request) { r.Header.Set(k, v) } }

func body(contentType string, b []byte) reqOpt {
	return func(r *http.Request) {
		r.Header.Set("Content-Type", contentType)
		r.Body = http.NoBody
		if len(b) > 0 {
			r.Body = readCloser{bytes.NewReader(b)}
		}
		r.ContentLength = int64(len(b))
	}
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

var (
	xrb    = hdr("X-Requested-By", "test")
	editor = []reqOpt{basic("ops", edPass), xrb}
	viewer = []reqOpt{basic("eye", viPass), xrb}
)

func with(base []reqOpt, more ...reqOpt) []reqOpt {
	return append(append([]reqOpt(nil), base...), more...)
}

func (e *env) do(method, target string, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doCtx(context.Background(), method, target, opts...)
}

func (e *env) doCtx(ctx context.Context, method, target string, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequestWithContext(ctx, method, target, nil)
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, r)
	return rec
}

// errOf decodes the error document of a response.
func errOf(t *testing.T, rec *httptest.ResponseRecorder) errorDoc {
	t.Helper()
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"), rec.Body.String())
	var b errorBody
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&b), rec.Body.String())
	return b.Error
}

// routeRequest returns a request for rt that its handler accepts: the
// stream cam, a body where one is needed, and one second for the profiles
// (the default is 30 s).
func routeRequest(rt Route) (target string, opts []reqOpt) {
	target = strings.ReplaceAll(rt.Path, "{name}", "cam")
	switch {
	case rt.Method == http.MethodPut && rt.Path == pathStream:
		opts = append(opts, body("application/json", []byte(camSpec)))
	case rt.Method == http.MethodPut && rt.Path == pathConfig:
		opts = append(opts, body("application/yaml", []byte("version: 1\n")), hdr("If-Match", `"x"`))
	case rt.Path == pathPprof+"profile", rt.Path == pathPprof+"trace":
		target += "?seconds=1"
	}
	return target, opts
}

func (e *env) samples() []metricstest.Sample {
	e.t.Helper()
	var reg metrics.Registry
	reg.Register(e.api.Collect)
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	s, err := metricstest.Parse(rec.Body)
	require.NoError(e.t, err)
	return s
}

func (e *env) counter(name string, labels ...string) float64 {
	e.t.Helper()
	v, _ := metricstest.Find(e.samples(), name, labels...)
	return v
}

const requestsTotal = "openstreamer_api_requests_total"

func TestNew(t *testing.T) {
	e := newEnv(t)
	users := func() *auth.Users { return e.users.Load() }
	_, err := New(Options{Users: users, Limiter: e.lim})
	require.Error(t, err)
	_, err = New(Options{Backend: e.be, Limiter: e.lim})
	require.Error(t, err)
	_, err = New(Options{Backend: e.be, Users: users})
	require.Error(t, err)

	// The defaults: no logger, no clock, no metrics handler.
	a, err := New(Options{Backend: e.be, Users: users, Limiter: e.lim})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	r.SetBasicAuth("eye", viPass)
	a.ServeHTTP(rec, r)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, codeNotFound, errOf(t, rec).Code)

	// A Users that returns nothing lets nobody in.
	a, err = New(Options{Backend: e.be, Users: func() *auth.Users { return nil }, Limiter: e.lim})
	require.NoError(t, err)
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestHeaders(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	for _, c := range []struct {
		target string
		opts   []reqOpt
	}{
		{"/healthz", nil},
		{"/api/v1/streams", viewer},
		{"/api/v1/streams", nil}, // 401
		{"/api/v1/nope", nil},    // the mux's 404
		{"/api/v1/config", editor},
		{"/metrics", viewer},
	} {
		rec := e.do(http.MethodGet, c.target, c.opts...)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), c.target)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), c.target)
	}
	rec := e.do(http.MethodGet, "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
	rec = e.do(http.MethodGet, "/api/v1/version", viewer...)
	assert.JSONEq(t, `{"version":"v2.0.0-test"}`, rec.Body.String())
}

func TestRequestCounter(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	e.be.streams["door"] = spec.Stream{Name: "door", Inputs: []spec.Input{{URL: "udp://239.0.0.2:5000"}}}

	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/streams/cam", viewer...).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/streams/door", viewer...).Code)
	require.Equal(t, http.StatusNotFound, e.do(http.MethodGet, "/api/v1/nope", viewer...).Code)
	assert.InDelta(t, 2, e.counter(requestsTotal, "method", "GET", "route", pathStream, "code", "200"), 0)
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "GET", "route", unmatched, "code", "404"), 0)

	// The route is resolved before authentication, and has no method in it.
	rec := e.do(http.MethodGet, "/api/v1/streams/x")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "GET", "route", pathStream, "code", "401"), 0)

	// Any method outside the known ones is "other".
	token := make([]byte, 20)
	_, err := rand.Read(token)
	require.NoError(t, err)
	for _, m := range []string{"BREW", "get", hex.EncodeToString(token)} {
		e.do(m, "/api/v1/streams")
	}
	assert.InDelta(t, 3, e.counter(requestsTotal, "method", "other", "route", unmatched, "code", "405"), 0)

	// Unmatched: the mux's own answers, without credentials.
	e.be.reset()
	rec = e.do(http.MethodPost, "/api/v1/streams/x")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	allow := rec.Header().Get("Allow")
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		assert.Contains(t, allow, m)
	}
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "POST", "route", unmatched, "code", "405"), 0)

	rec = e.do(http.MethodGet, "/api/v1/streams/x/")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "404 page not found\n", rec.Body.String())
	assert.InDelta(t, 2, e.counter(requestsTotal, "method", "GET", "route", unmatched, "code", "404"), 0)
	assert.Empty(t, e.be.callList())

	// A trailing-slash redirect is a request for its target's route.
	rec = e.do(http.MethodGet, "/debug/pprof")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = e.do(http.MethodGet, "/debug/pprof", viewer...)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = e.do(http.MethodGet, "/debug/pprof", editor...)
	assert.Equal(t, http.StatusTemporaryRedirect, rec.Code)
	assert.Equal(t, "/debug/pprof/", rec.Header().Get("Location"))
	for _, code := range []string{"401", "403", "307"} {
		assert.InDelta(t, 1, e.counter(requestsTotal, "method", "GET", "route", pathPprof, "code", code), 0, code)
	}

	// So is a path the mux cleans.
	rec = e.do(http.MethodGet, "/api/v1//streams")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `Basic realm="openstreamer"`, rec.Header().Get("WWW-Authenticate"))
	rec = e.do(http.MethodGet, "/api/v1//streams", viewer...)
	assert.Equal(t, http.StatusTemporaryRedirect, rec.Code)
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "GET", "route", pathStreams, "code", "401"), 0)
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "GET", "route", pathStreams, "code", "307"), 0)
	// A cleaned write is refused like its target, and redirected only for
	// an editor.
	assert.Equal(t, http.StatusBadRequest, e.do(http.MethodPost, "/api/v1//reload", basic("ops", edPass)).Code)
	assert.Equal(t, http.StatusForbidden, e.do(http.MethodPost, "/api/v1//reload", viewer...).Code)
	assert.Equal(t, http.StatusTemporaryRedirect, e.do(http.MethodPost, "/api/v1//reload", editor...).Code)
	assert.Empty(t, e.be.callList(), "a redirect never reaches a handler of ours")

	// No series holds a stream name, a user name or a method outside the
	// known ones.
	for _, s := range e.samples() {
		for k, v := range s.Labels {
			for _, secret := range []string{"cam", "door", "ops", "eye", "BREW"} {
				assert.NotContains(t, v, secret, k)
			}
		}
		if s.Name == requestsTotal {
			assert.Contains(t, []string{"GET", "PUT", "DELETE", "POST", "HEAD", "OPTIONS", "other"}, s.Labels["method"])
		}
	}
}

func TestLookupNeedsMethod(t *testing.T) {
	e := newEnv(t)
	_, ok := e.api.lookup("/api/v1/streams")
	assert.False(t, ok, "a bare path is never a route")
	rt, ok := e.api.lookup("GET /api/v1/streams")
	require.True(t, ok)
	assert.Equal(t, Route{Method: "GET", Path: pathStreams, Role: auth.Viewer}, rt)
	for _, p := range []string{"", " ", "GET ", " /api/v1/streams", "POST /api/v1/streams", "GET /nope"} {
		_, ok := e.api.lookup(p)
		assert.False(t, ok, p)
	}

	// CONNECT never reaches a handler, with or without credentials.
	for _, target := range []string{"/api/v1/streams", "/api/v1//streams", "/api/v1/config"} {
		rec := e.do(http.MethodConnect, target, editor...)
		assert.GreaterOrEqual(t, rec.Code, 400, target)
	}
	assert.Empty(t, e.be.callList())
}

func TestPathValuesReachHandlers(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	for _, c := range []struct{ method, target, call string }{
		{http.MethodGet, "/api/v1/streams/cam", "Get cam"},
		{http.MethodPut, "/api/v1/streams/cam", "PutStream cam"},
		{http.MethodGet, "/api/v1/streams/cam/status", "Status cam"},
		{http.MethodGet, "/api/v1/streams/cam/thumbnail.jpg", "Shot cam"},
		{http.MethodDelete, "/api/v1/streams/cam", "DeleteStream cam"},
	} {
		e.be.reset()
		rec := e.do(c.method, c.target, with(editor, body("application/json", []byte(camSpec)))...)
		assert.Less(t, rec.Code, 300, c.target)
		assert.Equal(t, []string{c.call}, e.be.callList())
	}

	e.seedCam()
	e.be.reset()
	rec := e.do(http.MethodHead, "/api/v1/streams/cam")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = e.do(http.MethodHead, "/api/v1/streams/cam", viewer...)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"Get cam"}, e.be.callList())
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "HEAD", "route", pathStream, "code", "200"), 0)
	assert.InDelta(t, 1, e.counter(requestsTotal, "method", "HEAD", "route", pathStream, "code", "401"), 0)
}

func TestGuard(t *testing.T) {
	// A handler of ours refuses a request the wrapper did not pass for its
	// own route, whoever calls it.
	e := newEnv(t)
	e.seedCam()
	for _, target := range []string{"/api/v1/streams", "/api/v1/config", "/api/v1/streams/cam"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		e.api.mux.ServeHTTP(rec, r)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)

		// A session for another route does not open this one.
		rec = httptest.NewRecorder()
		s := &session{pattern: "GET /api/v1/version", user: "ops", role: auth.Editor}
		e.api.mux.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	}
	assert.Empty(t, e.be.callList())
}

func TestAccessLog(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	e.clock.Advance(0)
	rec := e.do(http.MethodGet, "/api/v1/streams/cam?secrets=1", with(editor, from("203.0.113.9:4000"))...)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = e.do(http.MethodGet, "/api/v1/streams/cam", from("203.0.113.9:4001"))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	lines := e.logs.lines(t, "api: request")
	require.Len(t, lines, 2)
	first := lines[0]
	assert.Equal(t, "INFO", first["level"])
	assert.Equal(t, "GET", first["method"])
	assert.Equal(t, pathStream, first["route"])
	assert.InDelta(t, 200, first["status"], 0)
	assert.Contains(t, first, "duration")
	assert.Equal(t, "203.0.113.9:4000", first["remote"])
	assert.Equal(t, "ops", first["user"])
	assert.NotContains(t, lines[1], "user", "no user before authentication")
	assert.InDelta(t, 401, lines[1]["status"], 0)

	logs := e.logs.String()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.SetBasicAuth("ops", edPass)
	for _, secret := range []string{edPass, "secrets", "Authorization", r.Header.Get("Authorization"), "Basic", "239.0.0.1"} {
		assert.NotContains(t, logs, secret)
	}
}

func TestMetricsAndPprof(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/metrics", viewer...)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "# the metrics\n", rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/metrics").Code)
	assert.EqualValues(t, 1, e.metricsHit.Load())

	assert.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/debug/pprof/").Code)
	assert.Equal(t, http.StatusForbidden, e.do(http.MethodGet, "/debug/pprof/", viewer...).Code)
	rec = e.do(http.MethodGet, "/debug/pprof/", editor...)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "goroutine")
	// A named profile is under the index's route.
	assert.Equal(t, http.StatusForbidden, e.do(http.MethodGet, "/debug/pprof/heap", viewer...).Code)
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/debug/pprof/heap", editor...).Code)
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/debug/pprof/cmdline", editor...).Code)
}

func TestPanicIsA500(t *testing.T) {
	e := newEnv(t)
	e.be.panics = true
	rec := e.do(http.MethodGet, "/api/v1/streams", viewer...)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	doc := errOf(t, rec)
	assert.Equal(t, codeInternal, doc.Code)
	assert.Equal(t, "internal error", doc.Message)
	assert.InDelta(t, 1, e.counter(requestsTotal, "route", pathStreams, "code", "500"), 0)
	logs := e.logs.String()
	assert.Contains(t, logs, "the backend panics")
	assert.NotContains(t, logs, "hunter2")

	// http.ErrAbortHandler is the server's, and passes through.
	a, err := New(Options{
		Backend: e.be, Users: func() *auth.Users { return e.users.Load() }, Limiter: e.lim,
		Metrics: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
	})
	require.NoError(t, err)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	r.SetBasicAuth("eye", viPass)
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { a.ServeHTTP(httptest.NewRecorder(), r) })
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) {
	return 0, errors.New("the connection broke: rtsp://u:hunter2@h/a")
}
func (brokenBody) Close() error { return nil }

func TestBrokenBody(t *testing.T) {
	e := newEnv(t)
	broken := func(r *http.Request) { r.Body, r.ContentLength = brokenBody{}, -1 }
	for _, c := range []struct {
		target string
		opts   []reqOpt
	}{
		{"/api/v1/streams/cam", []reqOpt{body("application/json", nil), broken}},
		{"/api/v1/config", []reqOpt{body("application/yaml", nil), hdr("If-Match", `"x"`), broken}},
	} {
		rec := e.do(http.MethodPut, c.target, with(editor, c.opts...)...)
		assert.Equal(t, http.StatusBadRequest, rec.Code, c.target)
		assert.Equal(t, codeBadRequest, errOf(t, rec).Code)
		assert.NotContains(t, rec.Body.String(), "hunter2")
	}
	assert.Empty(t, e.be.callList())
}

// TestOverHTTP runs the API behind a real server: the wire's view of the
// order, the headers and a streamed response.
func TestOverHTTP(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	do := func(method, path string, opts ...reqOpt) (int, http.Header, string) {
		t.Helper()
		r, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, nil)
		require.NoError(t, err)
		for _, o := range opts {
			o(r)
		}
		res, err := client.Do(r)
		require.NoError(t, err)
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, res.Header, string(b)
	}

	code, h, text := do(http.MethodGet, "/healthz")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "no-store", h.Get("Cache-Control"))
	assert.JSONEq(t, `{"status":"ok"}`, text)

	code, h, _ = do(http.MethodGet, "/api/v1/streams/cam")
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Equal(t, realm, h.Get("WWW-Authenticate"))

	code, _, text = do(http.MethodPut, "/api/v1/streams/door", with(editor, jsonBody(secretSpec))...)
	assert.Equal(t, http.StatusCreated, code, text)
	assert.NotContains(t, text, "hunter2")

	code, h, text = do(http.MethodHead, "/api/v1/streams/door", viewer...)
	assert.Equal(t, http.StatusOK, code)
	assert.Empty(t, text)
	assert.NotEmpty(t, h.Get("ETag"))

	code, _, _ = do(http.MethodGet, "/api/v1//config", viewer...)
	assert.Equal(t, http.StatusForbidden, code)
	code, h, _ = do(http.MethodGet, "/api/v1//config", editor...)
	assert.Equal(t, http.StatusTemporaryRedirect, code)
	assert.Equal(t, "/api/v1/config", h.Get("Location"))

	// A streamed answer goes through the wrapper's writer.
	code, _, text = do(http.MethodGet, "/debug/pprof/goroutine?debug=1", editor...)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, text, "goroutine profile")
}

func TestRecorder(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &recorder{ResponseWriter: inner, code: http.StatusOK}
	assert.Same(t, http.ResponseWriter(inner), rec.Unwrap())
	rec.WriteHeader(http.StatusTeapot)
	rec.WriteHeader(http.StatusOK)
	_, err := rec.Write([]byte("x"))
	require.NoError(t, err)
	rec.Flush()
	assert.Equal(t, http.StatusTeapot, rec.code, "the first status")
	assert.True(t, inner.Flushed)
	// A writer that cannot flush is left alone.
	(&recorder{ResponseWriter: noFlush{inner}}).Flush()
}

type noFlush struct{ http.ResponseWriter }
