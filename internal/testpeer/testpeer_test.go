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
