package api

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"fmt"
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
)

const realm = `Basic realm="openstreamer"`

// countHashes counts the PBKDF2 computations from now on. The dummy is made
// first, so the count never includes its making; the env is made before
// it, since hashing the test users' passwords goes through the hook too.
func countHashes(t *testing.T) *atomic.Int64 {
	t.Helper()
	auth.WarmDummy()
	var n atomic.Int64
	restore := auth.SetHashHook(func(next func() []byte) []byte {
		n.Add(1)
		return next()
	})
	t.Cleanup(restore)
	return &n
}

// holdHashes holds every PBKDF2 computation until release is called, and
// then answers with no key at all, which matches nothing: the tests that
// use it only send bad credentials. inside is how many are held now.
func holdHashes(t *testing.T) (inside *atomic.Int64, release func()) {
	t.Helper()
	auth.WarmDummy()
	inside = new(atomic.Int64)
	gate := make(chan struct{})
	restore := auth.SetHashHook(func(func() []byte) []byte {
		inside.Add(1)
		<-gate
		inside.Add(-1)
		return nil
	})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// The hook is removed after the held computations are let go.
	t.Cleanup(restore)
	t.Cleanup(release)
	return inside, release
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 10*time.Second, time.Millisecond, what)
}

// free reports how many logins can be reserved for the pair now, and gives
// them back. 5 means nothing is in flight and no failure is counted. It is
// about the pair's key only: each reservation holds its place of an address
// of its own, so the cap per address never ends the count.
func (e *env) free(addr, user string) int {
	key := e.lim.Key(addr, user)
	var held []*auth.Attempt
	for i := 0; ; i++ {
		a, ok := e.lim.Reserve(key, fmt.Sprintf("198.18.0.%d:1", i+1))
		if !ok {
			break
		}
		held = append(held, a)
	}
	for _, a := range held {
		a.Abandon()
	}
	return len(held)
}

func TestAuth(t *testing.T) {
	// No real PBKDF2: an unknown name costs 600000 iterations of the
	// dummy, and nothing here logs in.
	e := newEnv(t)
	e.seedCam()
	auth.WarmDummy()
	t.Cleanup(auth.SetHashHook(func(func() []byte) []byte { return nil }))

	n := 0
	addr := func() reqOpt { n++; return from(fmt.Sprintf("10.1.%d.%d:999", n/250, n%250+1)) }
	for _, rt := range Routes() {
		target, opts := routeRequest(rt)
		opts = append(opts, xrb)
		if rt.Role == "" {
			assert.Equal(t, http.StatusOK, e.do(rt.Method, target).Code, target)
			continue
		}
		for name, cred := range map[string]reqOpt{
			"no header":      func(*http.Request) {},
			"bearer":         hdr("Authorization", "Bearer x"),
			"not base64":     hdr("Authorization", "Basic !!!"),
			"no colon":       hdr("Authorization", "Basic b3Bz"),
			"unknown user":   basic("nobody", edPass),
			"wrong password": basic("ops", "wrong password 1"),
			"empty password": basic("ops", ""),
			"viewer's":       basic("ops", viPass),
		} {
			rec := e.do(rt.Method, target, with(opts, cred, addr())...)
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s: %s", rt.Method, target, name)
			assert.Equal(t, []string{realm}, rec.Header().Values("WWW-Authenticate"))
			assert.Equal(t, codeUnauthorized, errOf(t, rec).Code)
		}
	}
	assert.Empty(t, e.be.callList(), "nothing reached the backend")

	// An unmatched path is the mux's 404, with no challenge.
	rec := e.do(http.MethodGet, "/api/v2/streams")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.Equal(t, "404 page not found\n", rec.Body.String())
}

func TestFailedLoginWarning(t *testing.T) {
	e := newEnv(t)
	auth.WarmDummy()
	t.Cleanup(auth.SetHashHook(func(func() []byte) []byte { return nil }))
	const pw = "the wrong password 1"
	for i := range 100 {
		rec := e.do(http.MethodGet, "/api/v1/streams", basic("ops", pw), from(fmt.Sprintf("10.2.0.%d:1", i+1)))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		e.clock.Advance(50 * time.Millisecond)
	}
	warns := e.logs.lines(t, "api: failed login")
	require.Len(t, warns, 1)
	assert.Equal(t, "WARN", warns[0]["level"])
	assert.Equal(t, "ops", warns[0]["user"])
	assert.Equal(t, "10.2.0.1:1", warns[0]["remote"])
	assert.NotContains(t, e.logs.String(), pw)

	// After 10 s the next one is logged: the name cut to 32 bytes, and
	// still text. (A Basic user name has no colon, so it is never a URL
	// with a password; it is scrubbed all the same.)
	e.clock.Advance(10 * time.Second)
	e.do(http.MethodGet, "/api/v1/streams", basic("x"+strings.Repeat("é", 40), pw), from("10.2.1.1:1"))
	e.do(http.MethodGet, "/api/v1/streams", basic("ops", pw), from("10.2.1.2:1"))
	warns = e.logs.lines(t, "api: failed login")
	require.Len(t, warns, 2)
	user, _ := warns[1]["user"].(string)
	assert.Equal(t, "x"+strings.Repeat("é", 15)+"?", user)
	// Requests without credentials are not failed logins.
	e.clock.Advance(10 * time.Second)
	e.do(http.MethodGet, "/api/v1/streams")
	assert.Len(t, e.logs.lines(t, "api: failed login"), 2)
}

func TestRoles(t *testing.T) {
	e := newEnv(t)
	want := map[string]auth.Role{
		"GET /healthz":                             "",
		"GET /api/v1/version":                      auth.Viewer,
		"GET /api/v1/openapi.json":                 auth.Viewer,
		"GET /api/v1/streams":                      auth.Viewer,
		"GET /api/v1/streams/{name}":               auth.Viewer,
		"PUT /api/v1/streams/{name}":               auth.Editor,
		"DELETE /api/v1/streams/{name}":            auth.Editor,
		"GET /api/v1/streams/{name}/status":        auth.Viewer,
		"GET /api/v1/streams/{name}/thumbnail.jpg": auth.Viewer,
		"GET /api/v1/assets":                       auth.Viewer,
		"GET /api/v1/assets/{name}":                auth.Viewer,
		"PUT /api/v1/assets/{name}":                auth.Editor,
		"DELETE /api/v1/assets/{name}":             auth.Editor,
		"GET /api/v1/config":                       auth.Editor,
		"PUT /api/v1/config":                       auth.Editor,
		"GET /api/v1/config/status":                auth.Viewer,
		"POST /api/v1/reload":                      auth.Editor,
		"GET /metrics":                             auth.Viewer,
		"GET /debug/pprof/":                        auth.Editor,
		"GET /debug/pprof/cmdline":                 auth.Editor,
		"GET /debug/pprof/profile":                 auth.Editor,
		"GET /debug/pprof/symbol":                  auth.Editor,
		"GET /debug/pprof/trace":                   auth.Editor,
	}
	got := map[string]auth.Role{}
	routes := Routes()
	for i, rt := range routes {
		got[rt.Method+" "+rt.Path] = rt.Role
		if i > 0 {
			prev := routes[i-1]
			assert.True(t, prev.Path < rt.Path || (prev.Path == rt.Path && prev.Method < rt.Method), "sorted by path and method")
		}
	}
	assert.Equal(t, want, got)

	for _, rt := range routes {
		target, opts := routeRequest(rt)
		e.seedCam()
		e.be.reset()
		rec := e.do(rt.Method, target, with(viewer, opts...)...)
		switch rt.Role {
		case auth.Editor:
			assert.Equal(t, http.StatusForbidden, rec.Code, target)
			assert.Equal(t, codeForbidden, errOf(t, rec).Code)
			assert.Empty(t, e.be.callList(), target)
		case auth.Viewer, "":
			assert.Equal(t, http.StatusOK, rec.Code, target)
		}
		e.seedCam()
		rec = e.do(rt.Method, target, with(editor, opts...)...)
		assert.Less(t, rec.Code, 300, "%s %s: %s", rt.Method, target, rec.Body.String())
	}

	// ?secrets=1 is an editor's, on any route.
	e.seedCam()
	e.be.reset()
	for _, target := range []string{"/api/v1/streams?secrets=1", "/api/v1/streams/cam?secrets=1", "/api/v1/version?secrets=1"} {
		rec := e.do(http.MethodGet, target, viewer...)
		assert.Equal(t, http.StatusForbidden, rec.Code, target)
		assert.Equal(t, http.StatusOK, e.do(http.MethodGet, target, editor...).Code, target)
	}
}

func TestUsersReload(t *testing.T) {
	e := newEnv(t)
	hashes := countHashes(t)
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/version", basic("ops", edPass)).Code)

	const newPass = "another password 2"
	next := usersWith(t, newPass, viPass)
	e.users.Store(next)
	assert.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/api/v1/version", basic("ops", edPass)).Code)
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/version", basic("ops", newPass)).Code)

	// The same set again: a remembered login runs no PBKDF2.
	e.users.Store(next)
	before := hashes.Load()
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/version", basic("ops", newPass)).Code)
	assert.Equal(t, before, hashes.Load())
}

func TestLoginBackoff(t *testing.T) {
	e := newEnv(t)
	hashes := countHashes(t)
	const a1, a2 = "10.0.0.1:5000", "10.0.0.2:5000"
	get := func(addr, user, pass string) *httptest.ResponseRecorder {
		return e.do(http.MethodGet, "/api/v1/version", basic(user, pass), from(addr))
	}
	key := e.lim.Key(a1, "ops")

	// "ops" has a remembered login from 10.0.0.2.
	require.Equal(t, http.StatusOK, get(a2, "ops", edPass).Code)
	// Requests without credentials never count.
	for range 10 {
		require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/api/v1/version", from(a1)).Code)
	}
	for i := range 5 {
		_, blocked := e.lim.Blocked(key)
		require.False(t, blocked, "after %d failures", i)
		require.Equal(t, http.StatusUnauthorized, get(a1, "ops", "wrong password 1").Code)
	}

	// The 6th, with the right password: refused, and nothing was compared.
	h, u := hashes.Load(), e.usersCalls.Load()
	rec := get(a1, "ops", edPass)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Equal(t, codeTooMany, errOf(t, rec).Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.Equal(t, h, hashes.Load(), "no PBKDF2")
	assert.Equal(t, u, e.usersCalls.Load(), "no Remembered")

	// Never affected: the user from another address, and another user from
	// the same address.
	assert.Equal(t, http.StatusOK, get(a2, "ops", edPass).Code)
	assert.Equal(t, http.StatusOK, get(a1, "eye", viPass).Code)
	// A part of a second later the pair is still blocked, for 1 s rounded up.
	e.clock.Advance(300 * time.Millisecond)
	rec = get(a1, "ops", edPass)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))

	// After 1 s the right password gets in. With a new set of users it is
	// a real verification, and that clears the pair: 4 more failures do
	// not block it.
	e.clock.Advance(700 * time.Millisecond)
	e.users.Store(usersWith(t, edPass, viPass))
	h = hashes.Load()
	require.Equal(t, http.StatusOK, get(a1, "ops", edPass).Code)
	assert.Equal(t, h+1, hashes.Load())
	for range 4 {
		require.Equal(t, http.StatusUnauthorized, get(a1, "ops", "wrong password 1").Code)
	}
	_, blocked := e.lim.Blocked(key)
	assert.False(t, blocked)

	// A longer block rounds its Retry-After up: 7 failures are 4 s.
	for range 3 {
		e.clock.Advance(time.Minute)
		get(a1, "ops", "wrong password 1")
	}
	e.clock.Advance(2500 * time.Millisecond)
	rec = get(a1, "ops", edPass)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))

	assert.InDelta(t, 3, e.counter("openstreamer_api_auth_blocked_total"), 0)
	assert.InDelta(t, 12, e.counter("openstreamer_api_auth_failures_total"), 0)
	assert.InDelta(t, 3, e.counter(requestsTotal, "code", "429"), 0)
}

func TestLoginBurst(t *testing.T) {
	e := newEnv(t)
	hashes := countHashes(t)
	burst := func(n int, addr, pass string) map[int]int {
		codes := make(chan int, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				<-start
				codes <- e.do(http.MethodGet, "/api/v1/version", basic("ops", pass), from(addr)).Code
			})
		}
		close(start)
		wg.Wait()
		close(codes)
		got := map[int]int{}
		for c := range codes {
			got[c]++
		}
		return got
	}

	// The pair's key admits 5 in all, and the address 4 of them at a time.
	got := burst(200, "10.0.0.1:1", "wrong password 1")
	n := int(hashes.Load())
	assert.LessOrEqual(t, n, 5)
	assert.Positive(t, n)
	assert.Equal(t, map[int]int{http.StatusUnauthorized: n, http.StatusTooManyRequests: 200 - n}, got)

	// A remembered login reserves nothing: no 429, no PBKDF2.
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/version", basic("ops", edPass), from("10.0.0.2:1")).Code)
	before := hashes.Load()
	got = burst(100, "10.0.0.2:1", edPass)
	assert.Equal(t, map[int]int{http.StatusOK: 100}, got)
	assert.Equal(t, before, hashes.Load())
}

type answer struct {
	i     int
	code  int
	retry string
}

func TestLoginBusy(t *testing.T) {
	e := newEnv(t)
	inside, release := holdHashes(t)

	answers := make(chan answer, 64)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(release)
	login := func(i int, ctx context.Context) {
		wg.Go(func() {
			rec := e.doCtx(ctx, http.MethodGet, "/api/v1/version",
				basic(fmt.Sprintf("ghost%02d", i), "some password 1"), from(fmt.Sprintf("10.3.0.%d:1", i+1)))
			answers <- answer{i, rec.Code, rec.Header().Get("Retry-After")}
		})
	}
	take := func(n int) []answer {
		out := make([]answer, 0, n)
		for range n {
			select {
			case a := <-answers:
				out = append(out, a)
			case <-time.After(10 * time.Second):
				require.FailNow(t, "an answer is missing")
			}
		}
		return out
	}
	none := func() {
		select {
		case a := <-answers:
			require.FailNow(t, "an unexpected answer", "%+v", a)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// 30 first logins: 2 inside PBKDF2, 16 waiting, 12 refused at once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range 30 {
		login(i, ctx)
	}
	busy := take(12)
	for _, a := range busy {
		assert.Equal(t, http.StatusServiceUnavailable, a.code)
		assert.Equal(t, "1", a.retry)
	}
	waitFor(t, "2 inside", func() bool { return inside.Load() == 2 })
	none()
	// Counted against nobody.
	for _, a := range busy {
		assert.Equal(t, 5, e.free(fmt.Sprintf("10.3.0.%d:1", a.i+1), fmt.Sprintf("ghost%02d", a.i)))
	}
	assert.InDelta(t, 0, e.counter("openstreamer_api_auth_failures_total"), 0)
	assert.InDelta(t, 0, e.counter("openstreamer_api_auth_blocked_total"), 0)

	// The clients hang up: the 16 that wait leave the queue, the 2 inside
	// stay where they are.
	cancel()
	for _, a := range take(16) {
		assert.Equal(t, http.StatusServiceUnavailable, a.code)
		assert.Empty(t, a.retry)
		assert.Equal(t, 5, e.free(fmt.Sprintf("10.3.0.%d:1", a.i+1), fmt.Sprintf("ghost%02d", a.i)))
	}
	none()
	assert.EqualValues(t, 2, inside.Load())

	// Their places are free: of 17 more, 16 wait and one is refused.
	for i := 30; i < 47; i++ {
		login(i, context.Background())
	}
	a := take(1)[0]
	assert.Equal(t, http.StatusServiceUnavailable, a.code)
	assert.Equal(t, "1", a.retry)
	none()

	release()
	for _, a := range take(18) {
		assert.Equal(t, http.StatusUnauthorized, a.code)
	}
	assert.InDelta(t, 18, e.counter("openstreamer_api_auth_failures_total"), 0)
}

func TestAttemptAlwaysResolved(t *testing.T) {
	t.Run("a handler panic", func(t *testing.T) {
		e := newEnv(t)
		hashes := countHashes(t)
		e.be.panics = true
		const addr = "10.4.0.1:1"
		rec := e.do(http.MethodGet, "/api/v1/streams", basic("ops", edPass), from(addr))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.EqualValues(t, 1, hashes.Load(), "a first login")
		assert.Equal(t, 5, e.free(addr, "ops"))
		assert.InDelta(t, 0, e.counter("openstreamer_api_auth_failures_total"), 0)
	})

	t.Run("a cancelled request", func(t *testing.T) {
		e := newEnv(t)
		inside, release := holdHashes(t)
		var wg sync.WaitGroup
		t.Cleanup(wg.Wait)
		t.Cleanup(release)
		// Two logins fill PBKDF2's two places.
		for i := range 2 {
			wg.Go(func() {
				e.do(http.MethodGet, "/api/v1/version", basic(fmt.Sprintf("ghost%d", i), "x"), from("10.4.1.1:1"))
			})
		}
		waitFor(t, "2 inside", func() bool { return inside.Load() == 2 })

		// A first login of ops waits behind them, and its client hangs up.
		const addr = "10.4.0.2:1"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		before := e.usersCalls.Load()
		wg.Go(func() {
			done <- e.doCtx(ctx, http.MethodGet, "/api/v1/streams", basic("ops", edPass), from(addr)).Code
		})
		waitFor(t, "the login is in Verify", func() bool { return e.usersCalls.Load() == before+1 })
		waitFor(t, "its place is taken", func() bool { return e.free(addr, "ops") == 4 })
		time.Sleep(10 * time.Millisecond)
		cancel()
		select {
		case code := <-done:
			assert.NotEqual(t, http.StatusOK, code)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "the cancelled login did not return")
		}
		assert.Equal(t, 5, e.free(addr, "ops"), "nothing in flight, no failure counted")
		assert.Empty(t, e.be.callList(), "a cancelled login reaches nothing")
		assert.InDelta(t, 0, e.counter("openstreamer_api_auth_failures_total"), 0)
		release()
	})
}

// TestAPIAddressCap: one address inventing names has at most 4 logins being
// verified, the rest are 429s, and nobody else is kept out: not another
// address, not a remembered user of the same address, and not that address
// itself once the flood is over.
func TestAPIAddressCap(t *testing.T) {
	e := newEnv(t)
	auth.WarmDummy()
	const flood, elsewhere = "203.0.113.9", "198.51.100.7"
	port := new(atomic.Int64)
	// Each request from a port of its own, as each new connection has.
	fromFlood := func() reqOpt { return from(fmt.Sprintf("%s:%d", flood, 1024+port.Add(1)%60000)) }
	names := new(atomic.Int64)
	invented := func() reqOpt {
		n := names.Add(1)
		return basic(fmt.Sprintf("ghost%d", n), fmt.Sprintf("some password %d", n))
	}
	get := func(opts ...reqOpt) *httptest.ResponseRecorder {
		return e.do(http.MethodGet, "/api/v1/version", opts...)
	}

	// Before the flood, ops logs in from the address that will flood.
	require.Equal(t, http.StatusOK, get(basic("ops", edPass), fromFlood()).Code)

	// The hook counts the PBKDF2 computations, how many are inside, and how
	// long the slowest took; while gate is set it holds them. An invented
	// name is verified against the dummy hash, at 600000 iterations: so that
	// the flood stays fast, the hook computes a PBKDF2 at the test's
	// iteration count instead and answers with no key, which matches
	// nothing, unless a login of a real user is under way (realLogin): then
	// every computation is the real one.
	var (
		hashes, inside, real atomic.Int64
		slowest              atomic.Int64 // ns
		gate                 atomic.Pointer[chan struct{}]
	)
	t.Cleanup(auth.SetHashHook(func(next func() []byte) []byte {
		hashes.Add(1)
		inside.Add(1)
		defer inside.Add(-1)
		if g := gate.Load(); g != nil {
			<-*g
		}
		start := time.Now()
		var k []byte
		if real.Load() > 0 {
			k = next()
		} else {
			_, _ = pbkdf2.Key(sha256.New, "some password", []byte("0123456789abcdef"), 1000, auth.KeyBytes)
		}
		if d := int64(time.Since(start)); d > slowest.Load() {
			slowest.Store(d) // a lost update only lowers the bound below
		}
		return k
	}))
	// forget stores a new set of the same users, which remembers nobody.
	forget := func() {
		real.Add(1)
		defer real.Add(-1)
		n := hashes.Load()
		u, err := auth.NewUsers([]auth.User{
			{Name: "ops", Role: auth.Editor, Hash: testHash(edPass)},
			{Name: "eye", Role: auth.Viewer, Hash: testHash(viPass)},
		})
		require.NoError(t, err)
		e.users.Store(u)
		hashes.Store(n) // hashing their passwords is no login
	}
	realLogin := func(opts ...reqOpt) *httptest.ResponseRecorder {
		real.Add(1)
		defer real.Add(-1)
		return get(opts...)
	}
	held := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held) }) }
	gate.Store(&held)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(release)

	// 1. With the verifications held, the count is exact: of 20 logins at
	// once, 4 are inside Users.Verify (2 computing, 2 waiting for a turn)
	// and 16 are refused.
	answers := make(chan *httptest.ResponseRecorder, 20)
	for range 20 {
		cred, addr := invented(), fromFlood()
		wg.Go(func() { answers <- get(cred, addr) })
	}
	for range 16 {
		select {
		case rec := <-answers:
			require.Equal(t, http.StatusTooManyRequests, rec.Code)
			assert.Equal(t, "1", rec.Header().Get("Retry-After"))
			assert.Equal(t, codeTooMany, errOf(t, rec).Code)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "a refusal is missing")
		}
	}
	waitFor(t, "2 computing", func() bool { return inside.Load() == 2 })
	select {
	case rec := <-answers:
		require.FailNow(t, "a 17th answer", "%d", rec.Code)
	case <-time.After(50 * time.Millisecond):
	}
	assert.EqualValues(t, 2, hashes.Load(), "the other 2 wait inside Verify")
	assert.InDelta(t, 16, e.counter("openstreamer_api_auth_blocked_total"), 0)
	// More of them are refused the same way, whatever the name and the port.
	for range 20 {
		rec := get(invented(), fromFlood())
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	}
	// The remembered user of the address takes no place and is let in.
	require.Equal(t, http.StatusOK, get(basic("ops", edPass), fromFlood()).Code)
	// Another address has its own 4 places: its login is not refused, it
	// waits its turn and is verified.
	waiting := make(chan int, 1)
	cred := invented()
	wg.Go(func() { waiting <- get(cred, from(elsewhere+":1")).Code })
	release()
	gate.Store(nil)
	for range 4 {
		select {
		case rec := <-answers:
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		case <-time.After(30 * time.Second):
			require.FailNow(t, "a held login did not return")
		}
	}
	select {
	case code := <-waiting:
		assert.Equal(t, http.StatusUnauthorized, code, "a login from another address")
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the login from another address did not return")
	}
	assert.InDelta(t, 36, e.counter("openstreamer_api_auth_blocked_total"), 0)
	assert.InDelta(t, 5, e.counter("openstreamer_api_auth_failures_total"), 0)
	assert.EqualValues(t, 5, hashes.Load())

	// 2. The flood, with real PBKDF2: 20 clients of the address send
	// invented names one after another, 200 requests at least, until the
	// rows below are through.
	forget()
	require.Equal(t, http.StatusOK, realLogin(basic("ops", edPass), fromFlood()).Code)
	hashes.Store(0)
	slowest.Store(0)
	var (
		sent, got401, got429, gotOther atomic.Int64
		stop                           = make(chan struct{})
		flooders                       sync.WaitGroup
	)
	for range 20 {
		flooders.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := get(invented(), fromFlood())
				sent.Add(1)
				switch {
				case rec.Code == http.StatusUnauthorized:
					got401.Add(1)
				case rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "1":
					got429.Add(1)
					time.Sleep(time.Millisecond) // a refusal costs nothing: do not spin
				default:
					gotOther.Add(1)
				}
			}
		})
	}
	stopFlood := sync.OnceFunc(func() { close(stop); flooders.Wait() })
	t.Cleanup(stopFlood)
	waitFor(t, "the flood is verifying", func() bool { return got401.Load() >= 2 && got429.Load() > 0 })

	// A real user from another address logs in, within the time of four
	// verifications: at most 4 of the flood's are before it.
	start := time.Now()
	require.Equal(t, http.StatusOK, realLogin(basic("eye", viPass), from(elsewhere+":2")).Code)
	took := time.Since(start)
	// The remembered user of the flooding address keeps getting in.
	for range 20 {
		require.Equal(t, http.StatusOK, get(basic("ops", edPass), fromFlood()).Code)
	}
	waitFor(t, "200 requests", func() bool { return sent.Load() >= 200 })
	stopFlood()
	// The slack is for a loaded machine: the bound is 4 verifications, not 18.
	assert.LessOrEqual(t, took, 4*time.Duration(slowest.Load())+2*time.Second,
		"a login from another address waited behind more than 4 verifications")
	assert.Zero(t, gotOther.Load(), "an answer that is neither 401 nor 429 with Retry-After 1")
	assert.Positive(t, got429.Load())
	// Every login that was not refused was verified once: the flood's and eye's.
	assert.Equal(t, got401.Load()+1, hashes.Load())
	assert.InDelta(t, float64(36+got429.Load()), e.counter("openstreamer_api_auth_blocked_total"), 0)

	// 3. The flood is over: nothing is left behind. A user nobody remembers
	// logs in from that address at once, and its 4 places are all free.
	forget()
	before := hashes.Load()
	require.Equal(t, http.StatusOK, realLogin(basic("ops", edPass), fromFlood()).Code)
	assert.Equal(t, before+1, hashes.Load(), "a real verification")
	places := make([]*auth.Attempt, 0, 5)
	for i := range 5 {
		a, ok := e.lim.Reserve(e.lim.Key(flood+":1", fmt.Sprintf("after%d", i)), flood+":1")
		assert.Equal(t, i < 4, ok, "place %d", i+1)
		places = append(places, a)
	}
	for _, a := range places {
		a.Abandon()
	}
}

func TestRememberedTouchesNoLimiter(t *testing.T) {
	e := newEnv(t)
	hashes := countHashes(t)
	const a1, a2 = "10.5.0.1:1", "10.5.0.2:1"
	get := func(addr, pass string) int {
		return e.do(http.MethodGet, "/api/v1/version", basic("ops", pass), from(addr)).Code
	}
	require.Equal(t, http.StatusOK, get(a2, edPass)) // the real verification, from elsewhere
	for range 4 {
		require.Equal(t, http.StatusUnauthorized, get(a1, "wrong password 1"))
	}
	before := hashes.Load()
	for range 50 {
		require.Equal(t, http.StatusOK, get(a1, edPass))
	}
	assert.Equal(t, before, hashes.Load(), "remembered: no PBKDF2")
	assert.Equal(t, 1, e.free(a1, "ops"), "the 4 failures are still counted")

	// So one more wrong password blocks the pair.
	require.Equal(t, http.StatusUnauthorized, get(a1, "wrong password 1"))
	assert.Equal(t, http.StatusTooManyRequests, get(a1, edPass))
	assert.Equal(t, http.StatusOK, get(a2, edPass))
}

func TestWritesNeedRequestedBy(t *testing.T) {
	e := newEnv(t)
	hashes := countHashes(t)
	writes := 0
	for _, rt := range Routes() {
		if strings.HasPrefix(rt.Path, pathPprof) && rt.Path != pathPprof {
			continue // the profiles take a second each, and are GETs like the index
		}
		target, opts := routeRequest(rt)
		e.seedCam()
		e.be.reset()
		before := hashes.Load()
		cred := basic("ops", edPass)
		rec := e.do(rt.Method, target, with(opts, cred, from("10.6.0.1:1"))...)
		if rt.Method == http.MethodGet {
			assert.Less(t, rec.Code, 300, target)
			continue
		}
		writes++
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s %s", rt.Method, target)
		doc := errOf(t, rec)
		assert.Equal(t, codeBadRequest, doc.Code)
		assert.Equal(t, "the X-Requested-By header is required", doc.Message)
		assert.Empty(t, e.be.callList(), "the backend is not called")
		assert.Equal(t, before, hashes.Load(), "no credential was looked at")
		// Nor without credentials: the header comes first.
		assert.Equal(t, http.StatusBadRequest, e.do(rt.Method, target, opts...).Code)

		rec = e.do(rt.Method, target, with(opts, cred, hdr("X-Requested-By", "anything"))...)
		assert.Less(t, rec.Code, 300, "%s %s: %s", rt.Method, target, rec.Body.String())
		assert.Len(t, e.be.callList(), 1)
	}
	assert.Equal(t, 6, writes, "PUT and DELETE of a stream and of an asset, PUT of the config, POST reload")

	// What a cross-site form could send: a bodyless POST with only an
	// Authorization header.
	e.be.reset()
	rec := e.do(http.MethodPost, "/api/v1/reload", basic("ops", edPass))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, e.be.callList())
}
