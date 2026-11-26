package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
)

// waitFor bounds every wait of these tests.
const waitFor = 30 * time.Second

const editorPW = "editor-password-1"

var realHash struct {
	once sync.Once
	hash string
	err  error
}

// editorHash is a hash of editorPW at the real iteration count: run has no
// way to lower it. It is made once: each costs about 0.2 s, more under
// -race.
func editorHash(t *testing.T) string {
	t.Helper()
	realHash.once.Do(func() { realHash.hash, realHash.err = auth.HashPassword(editorPW) })
	require.NoError(t, realHash.err)
	return realHash.hash
}

// lockedBuffer is an output of run that the test reads while it runs.
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

// streamEntry is a stream reading the looped fixture.
func streamEntry(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fixtures/h264_aac.ts")
	require.NoError(t, err)
	u := (&url.URL{Scheme: "file", Path: p, RawQuery: "loop=1"}).String()
	return "  - name: " + name + "\n    inputs:\n      - url: \"" + u + "\"\n    outputs: {hls: true}\n"
}

// configText is a config with every listener on an ephemeral loopback
// port, the editor ops with the hash, and the given stream entries.
func configText(hash string, streams ...string) string {
	text := `version: 1
server:
  http: {listen: "127.0.0.1:0"}
  api: {listen: "127.0.0.1:0"}
  rtmp: {listen: "127.0.0.1:0"}
  rtsp: {listen: "127.0.0.1:0"}
  srt: {listen: "127.0.0.1:0"}
users:
  - name: ops
    role: editor
    password: "` + hash + `"
`
	if len(streams) == 0 {
		return text + "streams: []\n"
	}
	return text + "streams:\n" + strings.Join(streams, "")
}

func writeConfig(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(text), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// running is a run of the command with a config.
type running struct {
	t              *testing.T
	path           string
	stdout, stderr *lockedBuffer
	cancel         context.CancelFunc
	done           chan int
	addrs          map[string]string
}

// startRun runs the command with the config text until the test ends, and
// returns once it has printed its listeners.
func startRun(t *testing.T, text string, hup <-chan os.Signal) *running {
	t.Helper()
	r := &running{
		t: t, path: writeConfig(t, text, 0o600),
		stdout: &lockedBuffer{}, stderr: &lockedBuffer{},
		done: make(chan int, 1), addrs: map[string]string{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		r.done <- run(ctx, []string{"-config", r.path}, strings.NewReader(""), r.stdout, r.stderr, hup)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(waitFor):
			assert.Fail(t, "run did not return")
		}
	})
	require.Eventually(t, func() bool { return strings.Contains(r.stdout.String(), "\nsrt ") }, waitFor, 5*time.Millisecond,
		"stderr: %s", r.stderr)
	for _, line := range strings.Split(strings.TrimSpace(r.stdout.String()), "\n") {
		name, addr, ok := strings.Cut(line, " ")
		require.True(t, ok, line)
		r.addrs[name] = addr
	}
	return r
}

// get sends an API GET as the editor and returns the status code and body.
func (r *running) get(path string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+r.addrs["api"]+path, nil)
	require.NoError(r.t, err)
	req.SetBasicAuth("ops", editorPW)
	res, err := (&http.Client{Timeout: waitFor}).Do(req)
	require.NoError(r.t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(r.t, err)
	return res.StatusCode, string(b)
}

// stop cancels the run and returns its exit code.
func (r *running) stop() int {
	r.t.Helper()
	r.cancel()
	select {
	case code := <-r.done:
		r.done <- code // for the cleanup
		return code
	case <-time.After(waitFor):
		require.Fail(r.t, "run did not return")
		return -1
	}
}

// reloads counts the reloads the server logged.
func (r *running) reloads() int {
	return strings.Count(r.stderr.String(), "config: reloaded")
}

// runOnce runs the command to its end.
func runOnce(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(""), &out, &errOut, nil)
	return code, out.String(), errOut.String()
}

func TestRun(t *testing.T) {
	code, out, _ := runOnce(t, "-version")
	assert.Equal(t, 0, code)
	assert.Equal(t, version+"\n", out)

	code, out, errOut := runOnce(t)
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errOut, "usage: openstreamer -config FILE")

	code, _, errOut = runOnce(t, "-config", filepath.Join(t.TempDir(), "missing.yaml"))
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "missing.yaml")

	// A stray argument is refused without being echoed.
	code, _, errOut = runOnce(t, "-config", "x.yaml", "hunter2-secret")
	assert.Equal(t, 2, code)
	assert.NotContains(t, errOut, "hunter2-secret")
	code, _, errOut = runOnce(t, "-nope")
	assert.Equal(t, 2, code)
	assert.Equal(t, "openstreamer: unknown or malformed flag\n"+usageText, errOut)
	// Neither is a flag the command does not have, nor a value it cannot
	// parse.
	for _, args := range [][]string{
		{"-password=hunter2-secret"}, {"--hunter2-secret"}, {"-version=hunter2-secret"}, {"-config"},
	} {
		code, out, errOut = runOnce(t, args...)
		assert.Equal(t, 2, code, args)
		assert.Empty(t, out)
		assert.Equal(t, "openstreamer: unknown or malformed flag\n"+usageText, errOut, args)
	}
	code, out, errOut = runOnce(t, "-h")
	assert.Equal(t, 0, code)
	assert.Empty(t, out)
	assert.Equal(t, usageText, errOut)

	hash := editorHash(t)
	if runtime.GOOS != "windows" {
		code, _, errOut = runOnce(t, "-config", writeConfig(t, configText(hash), 0o644))
		assert.Equal(t, 2, code)
		assert.Contains(t, errOut, "must be mode 0600")
		assert.NotContains(t, errOut, "pbkdf2")
	}
	for _, bad := range []string{
		configText(hash) + "colour: blue\n",
		strings.Replace(configText(hash), "editor", "viewer", 1), // no editor
		configText(hash, "  - name: cam\n    inputs: 7\n") + "  - {name: [\n",
		strings.Replace(configText(hash), hash, "hunter2-plaintext", 1),
	} {
		code, out, errOut = runOnce(t, "-config", writeConfig(t, bad, 0o600))
		assert.Equal(t, 2, code)
		assert.Empty(t, out, "no listener is announced")
		assert.NotEmpty(t, errOut)
		assert.NotContains(t, errOut, "pbkdf2")
		assert.NotContains(t, errOut, "hunter2")
	}

	// A good config: the five listeners, the API, a reload, a clean stop.
	hup := make(chan os.Signal, 1)
	text := configText(hash, streamEntry(t, "cam"))
	r := startRun(t, text, hup)
	assert.Regexp(t, regexp.MustCompile(`^http 127\.0\.0\.1:\d+\napi 127\.0\.0\.1:\d+\nrtmp 127\.0\.0\.1:\d+\nrtsp 127\.0\.0\.1:\d+\nsrt 127\.0\.0\.1:\d+\n$`), r.stdout.String())
	res, err := http.Get("http://" + r.addrs["api"] + "/healthz") //nolint:noctx // a test's one-line probe
	require.NoError(t, err)
	res.Body.Close()
	assert.Equal(t, http.StatusOK, res.StatusCode)
	status, body := r.get("/api/v1/version")
	require.Equal(t, http.StatusOK, status, body)
	assert.Contains(t, body, version)
	status, _ = r.get("/api/v1/streams/two")
	require.Equal(t, http.StatusNotFound, status)

	require.NoError(t, os.WriteFile(r.path, []byte(text+streamEntry(t, "two")), 0o600))
	hup <- os.Interrupt // any value reloads
	require.Eventually(t, func() bool {
		status, _ := r.get("/api/v1/streams/two")
		return status == http.StatusOK
	}, waitFor, 20*time.Millisecond)
	assert.Equal(t, 1, r.reloads())
	assert.Equal(t, 0, r.stop())
	assert.NotContains(t, r.stderr.String(), "pbkdf2")
}

func TestRealIterations(t *testing.T) {
	// 600000 iterations, as hash-password makes them, load and log in.
	r := startRun(t, configText(editorHash(t)), nil)
	status, body := r.get("/api/v1/streams")
	assert.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, 0, r.stop())

	// One fewer is refused: run has no way to lower the floor.
	weak := auth.NewHash(editorPW, []byte("0123456789abcdef"), auth.MinIterations-1).String()
	code, out, errOut := runOnce(t, "-config", writeConfig(t, configText(weak), 0o600))
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errOut, "users[0]")
	assert.NotContains(t, errOut, "pbkdf2")
}

func TestHupLoop(t *testing.T) {
	// A signal pending when ctx has ended starts no reload: select picks
	// at random between the two ready cases, so this is tried many times.
	for range 200 {
		hup := make(chan os.Signal, 1)
		hup <- os.Interrupt
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		hupLoop(ctx, hup, func(context.Context) { assert.Fail(t, "a reload after ctx ended") })
	}
	// A nil channel, as on a system without SIGHUP, waits for ctx.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hupLoop(ctx, nil, func(context.Context) { assert.Fail(t, "a reload without a signal") })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(waitFor):
		require.Fail(t, "hupLoop did not return")
	}
}

// TestExampleConfig checks the shipped example without starting a server
// from it: it does not load as shipped, parses once its placeholders are
// replaced by hashes, and names none of the ports the soak owns.
func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile("openstreamer.example.yaml")
	require.NoError(t, err)
	text := string(b)
	const placeholder = "REPLACE-WITH-OUTPUT-OF-openstreamer-hash-password"
	require.Equal(t, 2, strings.Count(text, placeholder))
	assert.NotContains(t, text, "pbkdf2-sha256$", "the example ships no credential")
	// As whole numbers: the example's RTMP port, 19350, is not 1935.
	assert.NotRegexp(t, regexp.MustCompile(`\b(1935|554|8554)\b`), text)
	_, err = config.Parse(b, config.Options{})
	require.Error(t, err, "the example loads with its placeholders")
	assert.Contains(t, err.Error(), "users[0].password")

	hash := auth.NewHash(editorPW, []byte("0123456789abcdef"), 1000).String()
	p, err := config.Parse([]byte(strings.ReplaceAll(text, placeholder, hash)), config.Options{RequiredIterations: 1000})
	require.NoError(t, err)
	sec := p.Doc.Server
	assert.Equal(t, ":8080", sec.HTTP.Listen)
	assert.Equal(t, "127.0.0.1:8081", sec.API.Listen)
	assert.Equal(t, ":19350", sec.RTMP.Listen)
	assert.Equal(t, ":5554", sec.RTSP.Listen)
	assert.Equal(t, ":9999", sec.SRT.Listen)
	require.Len(t, p.Doc.Users, 2)
	assert.Equal(t, "editor", p.Doc.Users[0].Role)
	assert.Equal(t, "viewer", p.Doc.Users[1].Role)
	require.Len(t, p.Doc.Streams, 1)
}
