package testpeer

import (
	"fmt"
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
