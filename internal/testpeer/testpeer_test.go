package testpeer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

// recorder is a testing.TB whose Skipf and Fatalf record instead of ending
// the test.
type recorder struct {
	testing.TB
	skipped, failed bool
	skipMsg         string
}

func (r *recorder) Skipf(format string, args ...any) {
	r.skipped = true
	r.skipMsg = fmt.Sprintf(format, args...)
}
func (r *recorder) Fatalf(string, ...any) { r.failed = true }
func (r *recorder) Helper()               {}

func TestRequire(t *testing.T) {
	for _, tt := range []struct {
		env                  string
		binary               string
		protocols            []string
		wantSkip, wantFail   bool
		wantPathNonEmptyOnOK bool
	}{
		{"", "no-such-binary-xyz", nil, true, false, false},
		{"1", "no-such-binary-xyz", nil, false, true, false},
		{"", "sh", nil, false, false, true},
		{"", "false", []string{"srt"}, true, false, false},
		{"1", "false", []string{"srt"}, false, true, false},
	} {
		t.Run(fmt.Sprintf("%s/%s/%q", tt.env, tt.binary, tt.protocols), func(t *testing.T) {
			t.Setenv(EnvRequire, tt.env)
			rec := &recorder{TB: t}
			path := Require(rec, tt.binary, tt.protocols...)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
			assert.Equal(t, tt.wantPathNonEmptyOnOK, path != "")
		})
	}
}

func TestRequireProtocols(t *testing.T) {
	path := Require(t, "ffmpeg", "file")
	assert.NotEmpty(t, path)

	t.Setenv(EnvRequire, "")
	rec := &recorder{TB: t}
	Require(rec, "ffmpeg", "no-such-protocol")
	assert.True(t, rec.skipped)
}

func TestMediaMTX(t *testing.T) {
	for _, tt := range []struct {
		addr, require      string
		wantSkip, wantFail bool
	}{
		{"", "", true, false},
		{"", "1", false, true},
		{"127.0.0.1:1935", "", false, false},
		{"127.0.0.1:1935", "1", false, false},
	} {
		t.Run(fmt.Sprintf("%q/%q", tt.addr, tt.require), func(t *testing.T) {
			t.Setenv(EnvMediaMTX, tt.addr)
			t.Setenv(EnvRequireMediaMTX, tt.require)
			rec := &recorder{TB: t}
			got := MediaMTX(rec)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
			assert.Equal(t, tt.addr, got)
		})
	}
}

func TestMediaMTXRTSP(t *testing.T) {
	for _, tt := range []struct {
		addr, require      string
		wantSkip, wantFail bool
	}{
		{"", "", true, false},
		{"", "1", false, true},
		{"127.0.0.1:18554", "", false, false},
		{"127.0.0.1:18554", "1", false, false},
	} {
		t.Run(fmt.Sprintf("%q/%q", tt.addr, tt.require), func(t *testing.T) {
			t.Setenv(EnvMediaMTXRTSP, tt.addr)
			t.Setenv(EnvRequireMediaMTX, tt.require)
			rec := &recorder{TB: t}
			got := MediaMTXRTSP(rec)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
			assert.Equal(t, tt.addr, got)
		})
	}
}

func TestMediaMTXPull(t *testing.T) {
	for _, tt := range []struct {
		pull, addr, require string
		wantSkip, wantFail  bool
		want                string
	}{
		{"", "127.0.0.1:18554", "", true, false, ""},
		{"", "127.0.0.1:18554", "1", false, true, ""},
		{"1", "", "", true, false, ""},
		{"1", "", "1", false, true, ""},
		{"1", "127.0.0.1:18554", "", false, false, "127.0.0.1:18554"},
		{"1", "127.0.0.1:18554", "1", false, false, "127.0.0.1:18554"},
	} {
		t.Run(fmt.Sprintf("%q/%q/%q", tt.pull, tt.addr, tt.require), func(t *testing.T) {
			t.Setenv(EnvMediaMTXPull, tt.pull)
			t.Setenv(EnvMediaMTXRTSP, tt.addr)
			t.Setenv(EnvRequireMediaMTX, tt.require)
			rec := &recorder{TB: t}
			got := MediaMTXPull(rec)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRequireMuxer(t *testing.T) {
	RequireMuxer(t, "ffmpeg", "mpegts") // every build has it

	for _, tt := range []struct {
		env, binary, muxer string
		wantSkip, wantFail bool
	}{
		{"", "ffmpeg", "no-such-muxer", true, false},
		{"1", "ffmpeg", "no-such-muxer", false, true},
		{"", "no-such-binary-xyz", "rtsp", true, false},
		{"1", "no-such-binary-xyz", "rtsp", false, true},
		{"", "false", "rtsp", true, false}, // exits with an error
		{"1", "false", "rtsp", false, true},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s", tt.env, tt.binary, tt.muxer), func(t *testing.T) {
			t.Setenv(EnvRequire, tt.env)
			rec := &recorder{TB: t}
			RequireMuxer(rec, tt.binary, tt.muxer)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
		})
	}
}

func TestParseFFmpegMajor(t *testing.T) {
	for line, want := range map[string]int{
		"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers": 6,
		"ffmpeg version 8.0.1-3ubuntu2 Copyright (c) 2000-2025 the FFmpeg developers": 8,
		"ffprobe version n7.1 Copyright (c) 2007-2024 the FFmpeg developers":          7,
		"ffmpeg version 7.0.2-static https://johnvansickle.com/ffmpeg/":               7,
		"ffmpeg version 10 Copyright":                                                 10,
		"ffmpeg version N-118000-g1234567 Copyright":                                  -1,
		"ffmpeg version 2024-01-01-git-abc":                                           -1,
		"ffmpeg version":                                                              -1,
		"version 6.1":                                                                 -1,
		"":                                                                            -1,
	} {
		got, ok := parseFFmpegMajor(line)
		if want < 0 {
			assert.False(t, ok, line)
			continue
		}
		assert.True(t, ok, line)
		assert.Equal(t, want, got, line)
	}
}

func TestFFmpegMajor(t *testing.T) {
	_, ok := FFmpegMajor(t, "false")
	assert.False(t, ok, "a binary that fails")
	dir := t.TempDir()
	bin := dir + "/fakeffmpeg"
	assert.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'ffmpeg version 6.1.1-3ubuntu5 Copyright'\necho second\n"), 0o755))
	got, ok := FFmpegMajor(t, bin)
	assert.True(t, ok)
	assert.Equal(t, 6, got)
}

// fakeFFmpeg writes an executable named ffmpeg in a new directory that
// prints version line, and returns its path.
func fakeFFmpeg(t *testing.T, version string) string {
	t.Helper()
	bin := t.TempDir() + "/ffmpeg"
	assert.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'ffmpeg version "+version+" Copyright'\n"), 0o755))
	return bin
}

func TestFFmpeg8(t *testing.T) {
	ff8 := fakeFFmpeg(t, "8.0.1-static")
	ff6 := fakeFFmpeg(t, "6.1.1-3ubuntu5")
	dir := func(p string) string { return p[:len(p)-len("/ffmpeg")] }
	for _, tt := range []struct {
		name               string
		env, path, onHost  string
		require            string
		want               string
		wantSkip, wantFail bool
	}{
		{name: "env 8", env: ff8, path: dir(ff6), want: ff8},
		{name: "env 6", env: ff6, path: dir(ff8), wantFail: true},
		{name: "env not runnable", env: "/no/such/ffmpeg", wantFail: true},
		{name: "env 8 wins over on-host", env: ff8, onHost: "1", want: ff8},
		{name: "on host", onHost: "1", path: dir(ff8), require: "1", wantSkip: true},
		{name: "PATH 8", path: dir(ff8), want: ff8},
		{name: "PATH 6", path: dir(ff6), wantSkip: true},
		{name: "PATH 6 required", path: dir(ff6), require: "1", wantFail: true},
		{name: "none", path: t.TempDir(), wantSkip: true},
		{name: "none required", path: t.TempDir(), require: "1", wantFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvFFmpeg8, tt.env)
			t.Setenv(EnvFFmpeg8OnHost, tt.onHost)
			t.Setenv(EnvRequire, tt.require)
			t.Setenv("PATH", tt.path)
			rec := &recorder{TB: t}
			got := FFmpeg8(rec)
			assert.Equal(t, tt.wantSkip, rec.skipped, "skipped")
			assert.Equal(t, tt.wantFail, rec.failed, "failed")
			if !tt.wantSkip && !tt.wantFail {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// fakeTranscoder writes an executable named ostranscoder in a new directory
// that runs script, and returns its path.
func fakeTranscoder(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir() + "/ostranscoder"
	assert.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	return bin
}

func TestOSTranscoderPeer(t *testing.T) {
	good := fakeTranscoder(t, "echo 'ostranscoder wire 1 libavcodec 62.28.103'")
	stub := fakeTranscoder(t, "echo 'ostranscoder: built without -tags libav' >&2; exit 2")
	other := fakeTranscoder(t, "echo 'something else'")
	dir := func(p string) string { return p[:len(p)-len("/ostranscoder")] }
	for _, tt := range []struct {
		name               string
		env, path, require string
		want               string
		wantSkip, wantFail bool
	}{
		{name: "env", env: good, path: dir(stub), want: good},
		{name: "env nonexistent", env: "/nonexistent", path: dir(good), wantFail: true},
		{name: "env stub", env: stub, path: dir(good), wantFail: true},
		{name: "env not ostranscoder", env: other, wantFail: true},
		{name: "PATH", path: dir(good), want: good},
		{name: "PATH stub", path: dir(stub), wantSkip: true},
		{name: "PATH stub required", path: dir(stub), require: "1", wantFail: true},
		{name: "none", path: t.TempDir(), wantSkip: true},
		{name: "none required", path: t.TempDir(), require: "1", wantFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvTranscoder, tt.env)
			t.Setenv(EnvRequire, tt.require)
			t.Setenv("PATH", tt.path)
			rec := &recorder{TB: t}
			got := OSTranscoder(rec)
			assert.Equal(t, tt.wantSkip, rec.skipped, "skipped")
			assert.Equal(t, tt.wantFail, rec.failed, "failed")
			if !tt.wantSkip && !tt.wantFail {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestNVENCProbeSkipsOrFails: opted in (OS_NVENC=1 or OS_REQUIRE_NVENC=1),
// a transcoder whose -probe nvenc exits 3 skips the test, or fails it with
// OS_REQUIRE_NVENC=1; one that exits 0 does neither.
func TestNVENCProbeSkipsOrFails(t *testing.T) {
	ok := fakeTranscoder(t, `[ "$1 $2" = "-probe nvenc" ] || exit 2; echo 'nvenc: ok'`)
	none := fakeTranscoder(t, `[ "$1 $2" = "-probe nvenc" ] || exit 2; echo 'nvenc: no device'; exit 3`)
	for _, tt := range []struct {
		name, bin, require string
		wantSkip, wantFail bool
	}{
		{"absent", none, "", true, false},
		{"absent required", none, "1", false, true},
		{"present", ok, "", false, false},
		{"present required", ok, "1", false, false},
		{"not runnable", "/nonexistent/ostranscoder", "", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvNVENC, "1")
			t.Setenv(EnvRequireNVENC, tt.require)
			t.Setenv(EnvRequire, "1") // OS_REQUIRE_PEERS has no say
			rec := &recorder{TB: t}
			NVENC(rec, tt.bin)
			assert.Equal(t, tt.wantSkip, rec.skipped, "skipped")
			assert.Equal(t, tt.wantFail, rec.failed, "failed")
			if tt.wantSkip {
				assert.True(t, strings.HasPrefix(rec.skipMsg, "no NVENC device: "), "skip %q", rec.skipMsg)
			}
		})
	}
}

// TestNVENCOptIn: without OS_NVENC=1 or OS_REQUIRE_NVENC=1 the test skips
// naming the opt-in, and the probe, which would open an NVENC session, is
// never run; OS_REQUIRE_NVENC=1 alone opts in.
func TestNVENCOptIn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probed")
	bin := fakeTranscoder(t, `touch '`+marker+`'; echo 'nvenc: ok'`)
	for _, nvenc := range []string{"", "0", "true"} {
		t.Run("OS_NVENC="+nvenc, func(t *testing.T) {
			t.Setenv(EnvNVENC, nvenc)
			t.Setenv(EnvRequireNVENC, "")
			assert.False(t, NVENCOptedIn())
			rec := &recorder{TB: t}
			NVENC(rec, bin)
			assert.True(t, rec.skipped, "skipped")
			assert.False(t, rec.failed, "failed")
			assert.Equal(t, "NVENC tests are opt-in: set OS_NVENC=1 (make test-nvenc does)", rec.skipMsg)
			assert.NoFileExists(t, marker, "the probe ran")
		})
	}
	for _, env := range []string{EnvNVENC, EnvRequireNVENC} {
		t.Run(env+"=1", func(t *testing.T) {
			t.Setenv(EnvNVENC, "")
			t.Setenv(EnvRequireNVENC, "")
			t.Setenv(env, "1")
			assert.True(t, NVENCOptedIn())
			rec := &recorder{TB: t}
			NVENC(rec, bin)
			assert.False(t, rec.skipped, "skipped: %s", rec.skipMsg)
			assert.False(t, rec.failed, "failed")
			assert.FileExists(t, marker, "the probe did not run")
			require.NoError(t, os.Remove(marker))
		})
	}
}

func TestProbeReason(t *testing.T) {
	err := errors.New("exit status 3")
	for _, tt := range []struct{ out, want string }{
		{
			"[h264_nvenc @ 0x1] Cannot load libcuda.so.1\nnvenc: open: Operation not permitted\n",
			"nvenc: open: Operation not permitted ([h264_nvenc @ 0x1] Cannot load libcuda.so.1)",
		},
		{"nvenc: no device\n", "nvenc: no device"},
		{"something\nelse\n", "something; else"},
		{"", "exit status 3"},
	} {
		assert.Equal(t, tt.want, probeReason(tt.out, err), "%q", tt.out)
	}
}

// echoConn is one connection an echoServer accepted; ended is closed when
// its client's side has ended.
type echoConn struct {
	net.Conn
	ended chan struct{}
}

// echoServer is a plain TCP server on 127.0.0.1:0 that echoes every
// connection; conns receives each accepted connection.
func echoServer(t *testing.T) (addr string, conns <-chan echoConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a test listener
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	ch := make(chan echoConn, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			ec := echoConn{c, make(chan struct{})}
			ch <- ec
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
				close(ec.ended)
			}()
		}
	}()
	return l.Addr().String(), ch
}

// closed reports whether a read of c ends, without data, within 5 s.
func closed(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(make([]byte, 1))
	var ne net.Error
	return n == 0 && err != nil && (!errors.As(err, &ne) || !ne.Timeout())
}

func TestTLSFront(t *testing.T) {
	ca := tlstest.NewCA(t)
	backend, conns := echoServer(t)
	accepted := func() echoConn {
		select {
		case c := <-conns:
			return c
		case <-time.After(5 * time.Second):
			require.Fail(t, "the backend got no connection")
			return echoConn{}
		}
	}
	dial := func(addr string, cfg *tls.Config) (*tls.Conn, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return conn.(*tls.Conn), nil
	}
	var (
		addr string
		left *tls.Conn // the connection the subtest leaves open
	)
	t.Run("serves", func(t *testing.T) {
		addr = TLSFront(t, backend, ca.Server(t))
		host, _, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", host)

		// Bytes go both ways: what the client writes comes back from the
		// echo, more than one TLS record of it.
		c, err := dial(addr, ca.Client())
		require.NoError(t, err)
		b := accepted()
		sent := make([]byte, 100<<10)
		for i := range sent {
			sent[i] = byte(i)
		}
		go func() { _, _ = c.Write(sent) }()
		got := make([]byte, len(sent))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(c, got)
		require.NoError(t, err)
		assert.Equal(t, sent, got)

		// The client closes: the backend's connection ends.
		require.NoError(t, c.Close())
		select {
		case <-b.ended:
		case <-time.After(5 * time.Second):
			assert.Fail(t, "the backend's connection did not end with the client's")
		}

		// The backend closes: the client's connection ends.
		c2, err := dial(addr, ca.Client())
		require.NoError(t, err)
		require.NoError(t, accepted().Close())
		assert.True(t, closed(c2), "the client's connection ends with the backend's")
		_ = c2.Close()

		// A client that does not trust the certificate never reaches the
		// backend.
		_, err = dial(addr, tlstest.NewCA(t).Client())
		require.Error(t, err)
		select {
		case <-conns:
			assert.Fail(t, "an unverified client reached the backend")
		case <-time.After(200 * time.Millisecond):
		}

		// A backend that is not there: the connection is closed.
		gone, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a test listener
		require.NoError(t, err)
		goneAddr := gone.Addr().String()
		require.NoError(t, gone.Close())
		c3, err := dial(TLSFront(t, goneAddr, ca.Server(t)), ca.Client())
		require.NoError(t, err)
		assert.True(t, closed(c3), "no backend: the connection is closed")
		_ = c3.Close()

		// One connection stays open to the subtest's end.
		left, err = dial(addr, ca.Client())
		require.NoError(t, err)
		_ = accepted()
	})
	// The subtest has ended: the connection it left open is closed, and so
	// is its listener.
	require.NotNil(t, left)
	assert.True(t, closed(left), "the test's end closes an open connection")
	_ = left.Close()
	_, err := net.DialTimeout("tcp", addr, time.Second) //nolint:noctx // a test dial
	require.Error(t, err, "the listener closes at the test's end")

	// A listener that cannot be made fails the test.
	rec := &recorder{TB: t}
	assert.Empty(t, TLSFront(rec, backend, &tls.Config{MinVersion: tls.VersionTLS12}))
	assert.True(t, rec.failed)
}
