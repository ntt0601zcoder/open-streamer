package testpeer

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// recorder is a testing.TB whose Skipf and Fatalf record instead of ending
// the test.
type recorder struct {
	testing.TB
	skipped, failed bool
}

func (r *recorder) Skipf(string, ...any)  { r.skipped = true }
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
