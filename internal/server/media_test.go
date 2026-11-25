package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

func TestLiveOutputs(t *testing.T) {
	hls, dash := []stream.Output{stream.OutHLS}, []stream.Output{stream.OutDASH}
	both := []stream.Output{stream.OutHLS, stream.OutDASH}
	for _, c := range []struct {
		dir, file string
		want      []stream.Output
	}{
		{"hls", "index.m3u8", hls},
		{"hls", "3.ts", hls},
		{"hls", "v1.m3u8", hls},
		{"cmaf", "master.m3u8", hls},
		{"cmaf", "a1.m3u8", hls},
		{"cmaf", "index.mpd", dash},
		{"cmaf", "init-0-1.mp4", both},
		{"cmaf", "0-7.m4s", both},
		{"x", "y", nil},
	} {
		assert.Equal(t, c.want, LiveOutputs(c.dir, c.file), "%s/%s", c.dir, c.file)
	}
}

func TestServerLiveGate(t *testing.T) {
	s := testConfig(t, func(su *setup) {
		su.Opt.Open = nil
		su.Text = baseText(
			fileStream(t, "h", "{hls: true}"),
			fileStream(t, "d", "{dash: true}"),
			fileStream(t, "n", "{rtmp: true}"),
			fileStream(t, "t", "{httpts: true}"),
		)
	})
	get := func(path string) int { return s.mediaGet(path) }
	// The window lists a segment once the playlists answer.
	require.Eventually(t, func() bool { return get("/live/h/hls/index.m3u8") == http.StatusOK }, waitFor, 50*time.Millisecond)
	require.Eventually(t, func() bool { return get("/live/h/cmaf/master.m3u8") == http.StatusOK }, waitFor, 50*time.Millisecond)
	require.Eventually(t, func() bool { return get("/live/d/cmaf/index.mpd") == http.StatusOK }, waitFor, 50*time.Millisecond)

	// segment returns the paths of the init and of a media segment that
	// the CMAF window of a stream lists now.
	segment := func(name string) (string, string) {
		t.Helper()
		p, ok := s.Manager().Packager(stream.OutHLS, name)
		if !ok {
			p, ok = s.Manager().Packager(stream.OutDASH, name)
		}
		require.True(t, ok)
		var initFile, seg string
		require.Eventually(t, func() bool {
			for _, f := range cmafFiles(p) {
				switch {
				case strings.HasSuffix(f, ".mp4"):
					initFile = f
				case strings.HasSuffix(f, ".m4s"):
					seg = f
				}
			}
			return initFile != "" && seg != ""
		}, waitFor, 50*time.Millisecond)
		return "/live/" + name + "/cmaf/" + initFile, "/live/" + name + "/cmaf/" + seg
	}

	// hls on, dash off.
	assert.Equal(t, http.StatusNotFound, get("/live/h/cmaf/index.mpd"))
	initPath, segPath := segment("h")
	assert.Equal(t, http.StatusOK, get(initPath))
	assert.Equal(t, http.StatusOK, get(segPath))
	// dash on, hls off: the reverse, the segments still served.
	assert.Equal(t, http.StatusNotFound, get("/live/d/hls/index.m3u8"))
	assert.Equal(t, http.StatusNotFound, get("/live/d/cmaf/master.m3u8"))
	initPath, segPath = segment("d")
	assert.Equal(t, http.StatusOK, get(initPath))
	assert.Equal(t, http.StatusOK, get(segPath))
	// Neither: all 404.
	s.generation("n")
	for _, p := range []string{"/live/n/hls/index.m3u8", "/live/n/cmaf/master.m3u8", "/live/n/cmaf/index.mpd", "/live/n/cmaf/init-0-1.mp4"} {
		assert.Equal(t, http.StatusNotFound, get(p), p)
	}
	// Paths that are no file of a stream.
	for _, p := range []string{"/live/h", "/live/h/hls", "/live/h/x/y", "/live/h/hls/a/b"} {
		assert.Equal(t, http.StatusNotFound, get(p), p)
	}

	// stream.ts is 404 while httpts is off, and streams while it is on.
	assert.Equal(t, http.StatusNotFound, get("/live/h/stream.ts"))
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.Addrs().HTTP+"/live/t/stream.ts", nil)
		require.NoError(t, err)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return false
		}
		buf := make([]byte, 188*20)
		_, err = io.ReadFull(res.Body, buf)
		return err == nil && buf[0] == 0x47
	}, waitFor, 50*time.Millisecond)
}

// cmafFiles returns the init and the media segment of the first track of
// the newest segment in p's window.
func cmafFiles(p *live.Packager) []string {
	s := p.Snapshot()
	if len(s.Segments) == 0 {
		return nil
	}
	seg := s.Segments[len(s.Segments)-1]
	if len(seg.Parts) == 0 {
		return nil
	}
	part := seg.Parts[0]
	return []string{
		fmt.Sprintf("init-%d-%d.mp4", part.Track, seg.Epoch),
		fmt.Sprintf("%d-%d.m4s", part.Track, part.N),
	}
}
