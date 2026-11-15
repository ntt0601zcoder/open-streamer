package ingest

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// A segment URI that does not parse is reported without the URI, which may
// carry a signed token.
func TestHLSSegmentURIErrorRedacted(t *testing.T) {
	base, err := url.Parse("http://example.com/live/index.m3u8")
	require.NoError(t, err)
	s := &hlsSource{base: base}
	_, err = s.fetch("seg%zz.ts?token=SECRET", nil, maxSegmentBytes)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")
	assert.NotContains(t, err.Error(), "seg%zz")
	assert.EqualValues(t, 1, s.segmentErrors.Load())
}

// closeSpy is an io.ReadCloser that records Close.
type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error {
	c.closed = true
	return nil
}

// A publisher whose PMT was read while Close ran is closed, not installed.
func TestSRTSetCurAfterClose(t *testing.T) {
	data, err := os.ReadFile(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	rc := &closeSpy{Reader: bytes.NewReader(data)}
	r, err := newTSReader(rc, nil, Spec{}, &media.ConfigCache{}, nil)
	require.NoError(t, err)
	l := &srtListener{closed: true, doneStat: map[string]uint64{}}
	assert.False(t, l.setCur(r))
	assert.True(t, rc.closed, "the late publisher is closed")
	assert.Nil(t, l.cur)
}
