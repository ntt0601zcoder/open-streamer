package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// heaacASC is an HE-AAC AudioSpecificConfig with explicit hierarchical
// signalling: object type 5, 24 kHz core, stereo, 48 kHz output, AAC-LC core.
var heaacASC = []byte{0x2B, 0x11, 0x88, 0x00}

// probePackets returns pts and duration of every audio packet ffprobe reads.
func probePackets(t *testing.T, ffprobe, path string) (pts, dur []int64) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-select_streams", "a:0",
		"-show_entries", "packet=pts,duration", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	for _, line := range strings.Fields(string(out)) {
		f := strings.Split(strings.TrimSuffix(line, ","), ",")
		require.Len(t, f, 2, line)
		p, err := strconv.ParseInt(f[0], 10, 64)
		require.NoError(t, err, line)
		d, err := strconv.ParseInt(f[1], 10, 64)
		require.NoError(t, err, line)
		pts, dur = append(pts, p), append(dur, d)
	}
	return pts, dur
}

// HE-AAC frames (1024 samples at the 24 kHz core rate) go through the fMP4
// writer and the TS muxer: durations are the sample-count values and ffprobe
// reads both outputs.
func TestHEAACThroughOutputs(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
	// An AAC-LC encode at the core rate stands in for the HE-AAC core layer
	// (ffmpeg has no HE-AAC encoder); the frames carry no SBR payload.
	adts, err := exec.CommandContext(t.Context(), ffmpeg, "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=24000", "-t", "2",
		"-c:a", "aac", "-ac", "2", "-b:a", "32k", "-fflags", "+bitexact", "-flags:a", "+bitexact",
		"-f", "adts", "-").Output()
	require.NoError(t, err)
	raw, err := mpegts.ParseADTS(adts)
	require.NoError(t, err)
	require.Greater(t, len(raw), 40)
	require.Equal(t, 24000, raw[0].SampleRate)

	cfg, err := media.NewAACConfig(heaacASC)
	require.NoError(t, err)
	require.Equal(t, 24000, cfg.SampleRate, "the config rate is the core rate")
	frames := make([]media.Frame, len(raw))
	for k, r := range raw {
		ts := media.SampleTime(int64(k)*1024, cfg.SampleRate)
		frames[k] = media.Frame{Config: cfg, PTS: ts, DTS: ts, Key: true, Data: [][]byte{r.Payload}}
	}
	n := int64(len(frames))
	dir := t.TempDir()

	t.Run("fmp4", func(t *testing.T) {
		init, err := fmp4.Init(cfg)
		require.NoError(t, err)
		assert.Equal(t, uint32(24000), fmp4.Timescale(cfg))
		seg, tm, err := fmp4.Segment(frames, cfg, 1)
		require.NoError(t, err)
		assert.Equal(t, uint64(n*1024), tm.Duration)
		p := filepath.Join(dir, "heaac.mp4")
		require.NoError(t, os.WriteFile(p, append(init, seg...), 0o600))
		pts, dur := probePackets(t, ffprobe, p)
		require.Len(t, pts, len(frames))
		for k := range pts {
			assert.Equal(t, int64(k)*1024, pts[k], "packet %d pts", k)
			assert.Equal(t, int64(1024), dur[k], "packet %d duration", k)
		}

		// And back through the demuxer.
		d, err := fmp4.NewDemuxer(init)
		require.NoError(t, err)
		out, err := d.Segment(seg)
		require.NoError(t, err)
		require.Len(t, out, len(frames))
		for k, f := range out {
			assert.Equal(t, frames[k].PTS, f.PTS)
			assert.Equal(t, frames[k].Data, f.Data)
		}
	})

	t.Run("ts", func(t *testing.T) {
		var buf bytes.Buffer
		m, err := ts.NewMuxer(&buf, []*media.TrackConfig{cfg})
		require.NoError(t, err)
		for _, f := range frames {
			require.NoError(t, m.WriteFrame(f))
		}
		p := filepath.Join(dir, "heaac.ts")
		require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o600))
		pts, _ := probePackets(t, ffprobe, p)
		require.Len(t, pts, len(frames))
		for k := range pts {
			assert.Equal(t, pts[0]+media.SampleTime(int64(k)*1024, 24000), pts[k], "packet %d pts", k)
		}
		// Every ADTS header is AAC-LC at the 24 kHz core rate, stereo.
		dmx, err := ts.NewDemuxer(bytes.NewReader(buf.Bytes()))
		require.NoError(t, err)
		f, err := dmx.Read()
		require.NoError(t, err)
		assert.Equal(t, []byte{0x13, 0x10}, f.Config.ASC)
		assert.Equal(t, 24000, f.Config.SampleRate)
	})
}
