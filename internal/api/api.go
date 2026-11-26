package api

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// Backend is the server behind the API. Every write goes through its one
// apply path.
type Backend interface {
	Get(name string) (spec.Stream, bool)
	List() []spec.Stream
	Status(name string) (stream.Status, bool)
	Shot(name string) (picture.Shot, time.Time, bool)
	// ConfigText is the text last loaded, reloaded or written, and
	// config.Hash of it.
	ConfigText() (text []byte, etag string)
	ConfigStatus() ConfigStatus
	PutConfig(ctx context.Context, text []byte, ifMatch string) (ApplyResult, error)
	PutStream(ctx context.Context, sp spec.Stream, ifMatch string) (out stream.Outcome, err error)
	DeleteStream(ctx context.Context, name, ifMatch string) error
	Reload(ctx context.Context) (ApplyResult, error)
	Assets() ([]Asset, error)
	// Asset returns the asset as an open regular file (asset.Store.Open); the handler closes it.
	Asset(name string) (Asset, io.ReadSeekCloser, error)
	PutAsset(ctx context.Context, name string, body io.Reader, ifMatch string, ifNoneMatch bool) (res AssetPut, created bool, err error)
	DeleteAsset(ctx context.Context, name, ifMatch string) error
}

// ApplyResult is what a whole-config write or a reload did.
type ApplyResult struct {
	Streams         []stream.Outcome `json:"streams"`          // one per stream of the old and the new config, by name
	RestartRequired []string         `json:"restart_required"` // the server keys that differ from the running ones; [] when none
	ETag            string           `json:"etag"`             // of the config now in force
}

// ConfigStatus is the state of the config in force and of the file.
type ConfigStatus struct {
	ETag            string     `json:"etag"`
	LoadedAt        time.Time  `json:"loaded_at"`   // when the config in force was loaded, reloaded or written
	Stale           bool       `json:"stale"`       // the file on disk differs: a reload is pending, API writes are refused
	LoadErrors      []string   `json:"load_errors"` // what the start skipped, and the streams that cannot run
	LastReloadAt    *time.Time `json:"last_reload_at,omitempty"`
	LastReloadError string     `json:"last_reload_error,omitempty"`
	RestartRequired []string   `json:"restart_required"`
}

// The errors a Backend returns beside those of internal/config and
// internal/stream.
var (
	ErrNotFound             = errors.New("api: not found")
	ErrPrecondition         = errors.New("api: precondition failed")   // an If-Match that does not match
	ErrPreconditionRequired = errors.New("api: precondition required") // PUT /config without If-Match
)

// Options are what New needs.
type Options struct {
	Backend Backend
	Users   func() *auth.Users // the users in force; called per request, so a reload takes effect at once
	Limiter *auth.Limiter
	Metrics http.Handler // served at /metrics; nil is a 404
	Version string
	Logger  *slog.Logger // nil discards
	Now     func() time.Time
}

// Route is one route of the API. Role "" means open.
type Route struct {
	Method, Path string
	Role         auth.Role
}

// The paths that are used more than once.
const (
	pathStreams = "/api/v1/streams"
	pathStream  = "/api/v1/streams/{name}"
	pathConfig  = "/api/v1/config"
	pathPprof   = "/debug/pprof/"
)

// unmatched is the route label of a request no route of ours answers.
const unmatched = "unmatched"

// routeTable is every route with what serves it. New registers exactly
// these on the mux, and Routes lists exactly these, so the wrapper's lookup
// and the mux cannot disagree about which patterns exist.
func (a *API) routeTable() []routeEntry {
	return []routeEntry{
		{Route{http.MethodGet, "/healthz", ""}, http.HandlerFunc(a.healthz)},
		{Route{http.MethodGet, "/api/v1/version", auth.Viewer}, http.HandlerFunc(a.version)},
		{Route{http.MethodGet, "/api/v1/openapi.json", auth.Viewer}, http.HandlerFunc(a.openAPI)},
		{Route{http.MethodGet, pathStreams, auth.Viewer}, http.HandlerFunc(a.listStreams)},
		{Route{http.MethodGet, pathStream, auth.Viewer}, http.HandlerFunc(a.getStream)},
		{Route{http.MethodPut, pathStream, auth.Editor}, http.HandlerFunc(a.putStream)},
		{Route{http.MethodDelete, pathStream, auth.Editor}, http.HandlerFunc(a.deleteStream)},
		{Route{http.MethodGet, pathStream + "/status", auth.Viewer}, http.HandlerFunc(a.streamStatus)},
		{Route{http.MethodGet, pathStream + "/thumbnail.jpg", auth.Viewer}, http.HandlerFunc(a.thumbnail)},
		{Route{http.MethodGet, pathAssets, auth.Viewer}, http.HandlerFunc(a.listAssets)},
		{Route{http.MethodGet, pathAsset, auth.Viewer}, http.HandlerFunc(a.getAsset)},
		{Route{http.MethodPut, pathAsset, auth.Editor}, http.HandlerFunc(a.putAsset)},
		{Route{http.MethodDelete, pathAsset, auth.Editor}, http.HandlerFunc(a.deleteAsset)},
		{Route{http.MethodGet, pathConfig, auth.Editor}, http.HandlerFunc(a.getConfig)},
		{Route{http.MethodPut, pathConfig, auth.Editor}, http.HandlerFunc(a.putConfig)},
		{Route{http.MethodGet, pathConfig + "/status", auth.Viewer}, http.HandlerFunc(a.configStatus)},
		{Route{http.MethodPost, "/api/v1/reload", auth.Editor}, http.HandlerFunc(a.reload)},
		{Route{http.MethodGet, "/metrics", auth.Viewer}, http.HandlerFunc(a.metrics)},
		{Route{http.MethodGet, pathPprof, auth.Editor}, http.HandlerFunc(pprof.Index)},
		{Route{http.MethodGet, pathPprof + "cmdline", auth.Editor}, http.HandlerFunc(pprof.Cmdline)},
		{Route{http.MethodGet, pathPprof + "profile", auth.Editor}, http.HandlerFunc(pprof.Profile)},
		{Route{http.MethodGet, pathPprof + "symbol", auth.Editor}, http.HandlerFunc(pprof.Symbol)},
		{Route{http.MethodGet, pathPprof + "trace", auth.Editor}, http.HandlerFunc(pprof.Trace)},
	}
}

type routeEntry struct {
	Route
	h http.Handler
}

// Routes returns every route, sorted by path and method.
func Routes() []Route {
	entries := (&API{}).routeTable()
	out := make([]Route, len(entries))
	for i, e := range entries {
		out[i] = e.Route
	}
	slices.SortFunc(out, func(x, y Route) int {
		return cmp.Or(cmp.Compare(x.Path, y.Path), cmp.Compare(x.Method, y.Method))
	})
	return out
}

// API is the handler of the API listener.
type API struct {
	opt    Options
	log    *slog.Logger
	now    func() time.Time
	mux    *http.ServeMux
	routes map[string]Route // by the mux's pattern, "METHOD path"
	slots  chan struct{}    // the asset uploads that hold a slot
	timing timing

	authFailures, authBlocked atomic.Uint64

	mu       sync.Mutex
	requests map[series]uint64
	lastWarn time.Time // the last failed-login warning
	warned   bool
}

// series is one series of the request counter. Every part is bounded: the
// method by methodLabel, the route by the table, the code by what the
// handlers write.
type series struct {
	method, route string
	code          int
}

// New returns the API. A nil Backend, Users or Limiter is an error.
func New(opt Options) (*API, error) {
	switch {
	case opt.Backend == nil:
		return nil, errors.New("api: new: no backend")
	case opt.Users == nil:
		return nil, errors.New("api: new: no users")
	case opt.Limiter == nil:
		return nil, errors.New("api: new: no limiter")
	}
	a := &API{
		opt:      opt,
		log:      opt.Logger,
		now:      opt.Now,
		mux:      http.NewServeMux(),
		routes:   make(map[string]Route),
		requests: make(map[series]uint64),
		slots:    make(chan struct{}, uploadSlots),
		timing:   defaultTiming,
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if a.now == nil {
		a.now = time.Now
	}
	for _, e := range a.routeTable() {
		pattern := e.Method + " " + e.Path
		a.routes[pattern] = e.Route
		h := e.h
		if e.Role != "" {
			h = guard(pattern, h)
		}
		a.mux.Handle(pattern, h)
	}
	return a, nil
}

// lookup returns the route of a pattern the mux returned. Only a pattern
// with its "METHOD " prefix is a route: the mux's CONNECT branch returns a
// bare path, which must never be taken for one.
func (a *API) lookup(pattern string) (Route, bool) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || path == "" {
		return Route{}, false
	}
	rt, ok := a.routes[pattern]
	return rt, ok
}

// session is what the wrapper found out about a request it let through.
type session struct {
	pattern string // the route's pattern
	user    string
	role    auth.Role
}

type sessionKey struct{}

func sessionOf(r *http.Request) *session {
	s, _ := r.Context().Value(sessionKey{}).(*session)
	return s
}

// guard refuses a request the wrapper did not authenticate for exactly
// this pattern. The wrapper and the mux resolve a request the same way, so
// this never fires; it is here so that a mistake in either cannot serve a
// protected route to a stranger.
func guard(pattern string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := sessionOf(r); s == nil || s.pattern != pattern {
			writeError(w, http.StatusInternalServerError, codeInternal, msgInternal, nil)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// recorder remembers the status of a response.
type recorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

// WriteHeader remembers the first status.
func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.code, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write remembers the implied 200.
func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.code, r.wrote = http.StatusOK, true
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap is for http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush is for the handlers that stream (pprof). A flush sends the header,
// so it counts as written: a panic after it aborts the connection.
func (r *recorder) Flush() {
	if !r.wrote {
		r.code, r.wrote = http.StatusOK, true
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// methodLabel bounds the counter's method label.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodHead, http.MethodOptions:
		return m
	default:
		return "other"
	}
}

// ServeHTTP is the outer wrapper: it counts and logs every request, turns a
// panic into a 500, or, once a part of the answer is written, into an
// aborted connection (http.ErrAbortHandler), and runs the request in the
// order the package documentation gives.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := a.now()
	rec := &recorder{ResponseWriter: w, code: http.StatusOK}
	info := &session{pattern: unmatched}
	route := unmatched
	defer func() {
		v := recover()
		abort := v == http.ErrAbortHandler //nolint:errorlint // a panic value, compared as net/http does
		if v != nil && !abort {
			a.log.Error("api: panic", "route", route,
				"panic", logging.Scrub(fmt.Sprint(v)), "stack", string(debug.Stack()))
			if rec.wrote {
				// A part of an answer is out already: the connection is
				// aborted, so that it does not end as a whole answer.
				abort = true
			} else {
				writeError(rec, http.StatusInternalServerError, codeInternal, msgInternal, nil)
			}
		}
		method := methodLabel(r.Method)
		a.mu.Lock()
		a.requests[series{method, route, rec.code}]++
		a.mu.Unlock()
		attrs := []any{
			"method", method, "route", route, "status", rec.code,
			"duration", a.now().Sub(start), "remote", r.RemoteAddr,
		}
		if info.user != "" {
			attrs = append(attrs, "user", info.user)
		}
		a.log.Info("api: request", attrs...)
		if abort {
			panic(http.ErrAbortHandler)
		}
	}()

	h := rec.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")

	// 1. The route, from the pattern only: the handler Handler returns has
	// no path values and is never called.
	_, pattern := a.mux.Handler(r)
	rt, ok := a.lookup(pattern)
	if ok {
		route = rt.Path
	}
	// 2. Unmatched, or open: the mux answers.
	if !ok || rt.Role == "" {
		a.mux.ServeHTTP(rec, r)
		return
	}
	// 3. A write needs X-Requested-By, before any credential is looked at.
	if needsRequestedBy(rt.Method) && r.Header.Get("X-Requested-By") == "" {
		writeError(rec, http.StatusBadRequest, codeBadRequest, "the X-Requested-By header is required", nil)
		return
	}
	// 4. The credentials.
	name, password, ok := r.BasicAuth()
	if !ok {
		unauthorized(rec)
		return
	}
	// 5. The limiter and the users.
	key := a.opt.Limiter.Key(r.RemoteAddr, name)
	if left, blocked := a.opt.Limiter.Blocked(key); blocked {
		a.tooMany(rec, left)
		return
	}
	users := a.opt.Users()
	if users == nil {
		writeError(rec, http.StatusServiceUnavailable, codeUnavailable, "no users are loaded", nil)
		return
	}
	role, remembered := users.Remembered(name, password)
	if !remembered {
		// Only here, where a verification would start, does the address's
		// cap of verifications in flight count: a remembered login took no
		// place. Every refusal is answered alike.
		attempt, ok := a.opt.Limiter.Reserve(key, r.RemoteAddr)
		if !ok {
			a.tooMany(rec, time.Second)
			return
		}
		defer attempt.Abandon()
		var err error
		role, err = users.Verify(r.Context(), name, password)
		switch {
		case err == nil:
			attempt.Succeed()
		case errors.Is(err, auth.ErrBadCredentials):
			attempt.Fail()
			a.authFailures.Add(1)
			a.warnFailedLogin(r.RemoteAddr, name)
			unauthorized(rec)
			return
		case errors.Is(err, auth.ErrBusy):
			rec.Header().Set("Retry-After", "1")
			writeError(rec, http.StatusServiceUnavailable, codeUnavailable, "too many logins are waiting", nil)
			return
		default:
			// The request's context ended while it waited: the client is
			// gone, and whatever is written reaches nobody. It is never
			// let through.
			writeError(rec, http.StatusServiceUnavailable, codeUnavailable, "the request was cancelled", nil)
			return
		}
	}
	info.user, info.role = name, role
	// 6. The route's role, and an editor for ?secrets=1.
	if !role.Allows(rt.Role) || (asksSecrets(r) && !role.Allows(auth.Editor)) {
		writeError(rec, http.StatusForbidden, codeForbidden, "the "+string(auth.Editor)+" role is required", nil)
		return
	}
	// 7. The real dispatch, which fills the path values.
	info.pattern = pattern
	a.mux.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), sessionKey{}, info)))
}

// needsRequestedBy reports whether a route of this method changes
// something.
func needsRequestedBy(method string) bool {
	return method == http.MethodPut || method == http.MethodDelete || method == http.MethodPost
}

// asksSecrets reports whether the query is exactly secrets=1.
func asksSecrets(r *http.Request) bool { return r.URL.RawQuery == "secrets=1" }

// showSecrets reports whether the response may hold stream secrets: the
// query is exactly secrets=1 and the wrapper found an editor.
func showSecrets(r *http.Request) bool {
	s := sessionOf(r)
	return asksSecrets(r) && s != nil && s.role == auth.Editor
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="openstreamer"`)
	writeError(w, http.StatusUnauthorized, codeUnauthorized, "credentials are required", nil)
}

// tooMany answers 429 with Retry-After in whole seconds, rounded up.
func (a *API) tooMany(w http.ResponseWriter, left time.Duration) {
	a.authBlocked.Add(1)
	secs := max(int64((left+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	writeError(w, http.StatusTooManyRequests, codeTooMany, "too many login attempts", nil)
}

const (
	warnEvery    = 10 * time.Second
	warnNameSize = 32
)

// warnFailedLogin logs a failed login, at most once every 10 s: the remote
// address and the presented name, scrubbed and cut, never the password.
func (a *API) warnFailedLogin(remote, name string) {
	now := a.now()
	a.mu.Lock()
	quiet := a.warned && now.Sub(a.lastWarn) < warnEvery
	if !quiet {
		a.warned, a.lastWarn = true, now
	}
	a.mu.Unlock()
	if quiet {
		return
	}
	name = logging.Scrub(name)
	if len(name) > warnNameSize {
		name = name[:warnNameSize]
	}
	a.log.Warn("api: failed login", "remote", remote, "user", strings.ToValidUTF8(name, "?"))
}

// Collect writes openstreamer_api_requests_total{method,route,code},
// openstreamer_api_auth_failures_total and
// openstreamer_api_auth_blocked_total.
func (a *API) Collect(w *metrics.Writer) {
	a.mu.Lock()
	keys := make([]series, 0, len(a.requests))
	vals := make(map[series]uint64, len(a.requests))
	for k, v := range a.requests {
		keys = append(keys, k)
		vals[k] = v
	}
	a.mu.Unlock()
	slices.SortFunc(keys, func(x, y series) int {
		return cmp.Or(cmp.Compare(x.route, y.route), cmp.Compare(x.method, y.method), cmp.Compare(x.code, y.code))
	})
	for _, k := range keys {
		w.Counter("openstreamer_api_requests_total", "API requests by method, route and status code.",
			float64(vals[k]),
			metrics.L("method", k.method), metrics.L("route", k.route), metrics.L("code", strconv.Itoa(k.code)))
	}
	w.Counter("openstreamer_api_auth_failures_total", "Logins refused for bad credentials.",
		float64(a.authFailures.Load()))
	w.Counter("openstreamer_api_auth_blocked_total", "Requests refused by the login limiter.",
		float64(a.authBlocked.Load()))
}

//go:embed openapi.json
var openAPIDoc []byte

func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": a.opt.Version})
}

func (a *API) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openAPIDoc)
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	if a.opt.Metrics == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no metrics", nil)
		return
	}
	a.opt.Metrics.ServeHTTP(w, r)
}
