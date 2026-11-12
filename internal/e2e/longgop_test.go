package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// noKeySteps returns one keyframe and then 60 s of non-key 25 fps video,
// with the AAC audio of h264_aac.ts repeated alongside, all in epoch 1.
func noKeySteps(t *testing.T) []step {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixtureBytes(t, "h264_aac.ts")))
	require.NoError(t, err)
	var key, au *media.Frame
	for key == nil || au == nil {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		switch {
		case key == nil && f.Kind() == media.KindVideo && f.Key:
			key = &f
		case au == nil && f.Kind() == media.KindAudio:
			au = &f
		}
	}
	require.NotNil(t, key)
	require.NotNil(t, au)
	const aDur = 1920 // 1024 samples at 48 kHz
	var out []step
	next := int64(0)
	for i := 0; i < 1500; i++ {
		dts := int64(i) * 3600
		for ; next <= dts; next += aDur {
			a := *au
			a.PTS, a.DTS, a.Epoch = next, next, 1
			out = append(out, step{f: a})
		}
		v := *key
		v.PTS, v.DTS, v.Epoch = dts, dts, 1
		if i > 0 {
			v.Key = false
			v.Data = [][]byte{{0x41, 0x9a, byte(i), byte(i >> 8)}} // non-IDR slice
		}
		out = append(out, step{f: v})
	}
	out[0].anchors = []media.Anchor{{Epoch: 1, Start: 0, Wall: m3aT0}}
	return out
}

// A source whose keyframes stop: segments are still cut, at 5 × Target, on
// non-key frames. The only violations any output shows are the expected
// segment-starts-on-key, once per forced segment, and no HLS playlist
// claims EXT-X-INDEPENDENT-SEGMENTS.
func TestM3aKeyframesStop(t *testing.T) {
	steps := noKeySteps(t)
	clk := newFakeClock(m3aT0)
	fd := stepsFeed(steps)
	p, written := packFeed(t, fd, clk.Now)
	advance(t, p, clk)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 6)
	forced := 0
	for i, sg := range snap.Segments {
		assert.LessOrEqual(t, sg.Duration, int64(5*2*media.ClockRate), "segment %d", i)
		assert.False(t, sg.Discontinuity, "segment %d", i)
		if sg.Forced {
			forced++
		}
	}
	require.Equal(t, 5, forced)

	base := serve(t, p, fd)
	assert.False(t, mediaPlaylist(t, base+"hls/index.m3u8").Independent, "hls-ts")
	for _, u := range cmafPlaylists(t, base) {
		assert.False(t, mediaPlaylist(t, u).Independent, u)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	once := conform.ReadOptions{Once: true}
	caps := map[string]conform.Capture{}
	for _, f := range []string{"hls-ts", "hls-fmp4", "dash"} {
		var (
			c   conform.Capture
			err error
		)
		switch f {
		case "hls-ts":
			c, err = conform.ReadHLS(ctx, base+"hls/index.m3u8", once)
		case "hls-fmp4":
			c, err = conform.ReadHLS(ctx, base+"cmaf/master.m3u8", once)
		case "dash":
			c, err = conform.ReadDASH(ctx, base+"cmaf/index.mpd", once)
		}
		require.NoError(t, err, f)
		n := 0
		for _, v := range c.Violations {
			if assert.Equal(t, conform.RuleSegmentKey, v.Rule, "%s: %v", f, v) {
				n++
			}
		}
		assert.Equal(t, forced, n, "%s: one segment-starts-on-key per forced segment: %v", f, c.Violations)
		caps[f] = c
	}
	caps["http-ts"] = conform.Capture{Frames: written} // no segments: nothing to check
	assertComplete(t, caps, written)
}
