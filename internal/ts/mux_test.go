package ts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// muxClip is a demuxed fixture prepared for muxing.
type muxClip struct {
	frames []media.Frame
	tracks []*media.TrackConfig
}

// loadClip demuxes a fixture and moves every timestamp by a constant so that
// the smallest DTS becomes start. The demuxer reports source time starting
// at 0 with DTS before it, so the raw output can be negative; the muxer is fed
// stream times, which are never negative.
func loadClip(t *testing.T, name string, start int64) muxClip {
	t.Helper()
	d, frames := demuxAll(t, fixture(t, name))
	minDTS := frames[0].DTS
	for _, f := range frames {
		minDTS = min(minDTS, f.DTS)
	}
	c := muxClip{tracks: make([]*media.TrackConfig, d.NumTracks())}
	for i := range frames {
		frames[i].PTS += start - minDTS
		frames[i].DTS += start - minDTS
		if c.tracks[frames[i].Track] == nil {
			c.tracks[frames[i].Track] = frames[i].Config
		}
	}
	c.frames = frames
	return c
}

func muxFrames(t *testing.T, m *Muxer, frames []media.Frame) {
	t.Helper()
	for _, f := range frames {
		require.NoError(t, m.WriteFrame(f))
	}
}

// muxClipBytes muxes the whole clip into one buffer.
func muxClipBytes(t *testing.T, c muxClip) []byte {
	t.Helper()
	var buf bytes.Buffer
	m, err := NewMuxer(&buf, c.tracks)
	require.NoError(t, err)
	muxFrames(t, m, c.frames)
	return buf.Bytes()
}

func readPackets(t *testing.T, data []byte) []mpegts.Packet {
	t.Helper()
	r := mpegts.NewReader(bytes.NewReader(data))
	var out []mpegts.Packet
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		p.Payload = bytes.Clone(p.Payload)
		out = append(out, p)
	}
}

func TestMuxRoundTrip(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h265_aac51.ts"} {
		t.Run(name, func(t *testing.T) {
			c := loadClip(t, name, 1000)
			d, out := demuxAll(t, muxClipBytes(t, c))
			assert.Zero(t, d.Stats().DecodeErrors)
			for track := range c.tracks {
				in, got := byTrack(c.frames, track), byTrack(out, track)
				require.Len(t, got, len(in))
				for i := range in {
					assert.Equal(t, in[i].DTS-in[0].DTS, got[i].DTS-got[0].DTS, "dts %d/%d", track, i)
					assert.Equal(t, in[i].PTS-in[0].DTS, got[i].PTS-got[0].DTS, "pts %d/%d", track, i)
					assert.Equal(t, in[i].Key, got[i].Key, "key %d/%d", track, i)
					assert.Equal(t, in[i].Data, got[i].Data, "data %d/%d", track, i)
					assert.True(t, in[i].Config.Equal(got[i].Config), "config %d/%d", track, i)
				}
			}
		})
	}
}

// annexB returns the NAL units of an Annex-B payload.
func annexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 {
		out = append(out, b[start:])
	}
	return out
}

func TestMuxParamSetsOnEveryKey(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h265_aac51.ts", "h264_aac_then_640.ts"} {
		t.Run(name, func(t *testing.T) {
			c := loadClip(t, name, 0)
			var video []media.Frame
			for _, f := range c.frames {
				if f.Track == 0 {
					video = append(video, f)
				}
			}
			dmx := mpegts.NewDemux(bytes.NewReader(muxClipBytes(t, c)))
			i, keys := 0, 0
			for {
				ev, err := dmx.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				if ev.PES == nil || ev.PES.PID != firstPID {
					continue
				}
				f := video[i]
				i++
				nals := annexB(ev.PES.Data)
				var want [][]byte
				if f.Key {
					keys++
					for _, s := range [][][]byte{f.Config.VPS, f.Config.SPS, f.Config.PPS} {
						want = append(want, s...)
					}
				}
				want = append(want, f.Data...)
				require.GreaterOrEqual(t, len(nals), 1)
				assert.Equal(t, want, nals[1:], "frame %d", i-1) // nals[0] is the AUD
				assert.Equal(t, f.Key, ev.PES.RandomAccess)
			}
			assert.Equal(t, len(video), i)
			assert.Greater(t, keys, 0)
		})
	}
}

// pcrPESCount counts the PES starting on pid and returns the PCR of each.
func pcrPESCount(pkts []mpegts.Packet, pid uint16) (pes int, pcrs []int64) {
	for _, p := range pkts {
		if p.PID != pid || !p.PUSI {
			continue
		}
		pes++
		pcrs = append(pcrs, p.PCR)
	}
	return pes, pcrs
}

// checkPCR asserts that every PES on the video track carries a PCR of
// stream DTS + 63000 modulo 2^33.
func checkPCR(t *testing.T, c muxClip, data []byte) {
	t.Helper()
	var dts []int64
	for _, f := range c.frames {
		if f.Track == 0 {
			dts = append(dts, f.DTS)
		}
	}
	n, pcrs := pcrPESCount(readPackets(t, data), firstPID)
	require.Equal(t, len(dts), n)
	for i, p := range pcrs {
		assert.Equal(t, (dts[i]+63000)&ts33Mask, p, "pes %d", i)
	}
}

func TestMuxPCREveryPES(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", 5000)
	checkPCR(t, c, muxClipBytes(t, c))
}

func TestMuxWrap(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", ts33Mask+1-126000-90000)
	data := muxClipBytes(t, c)
	checkPCR(t, c, data)

	// The clip crosses the written DTS wrap (stream time 2^33 - 126000) and
	// the PCR wrap (2^33 - 63000).
	last := c.frames[len(c.frames)-1].DTS
	require.Greater(t, last, int64(ts33Mask+1-63000))

	d, out := demuxAll(t, data)
	assert.Zero(t, d.Stats().DecodeErrors)
	for track := range c.tracks {
		in, got := byTrack(c.frames, track), byTrack(out, track)
		require.Len(t, got, len(in))
		for i := range in {
			assert.Equal(t, in[i].DTS-in[0].DTS, got[i].DTS-got[0].DTS)
			assert.Equal(t, in[i].PTS-in[0].DTS, got[i].PTS-got[0].DTS)
		}
	}
}

func TestMuxTablesBeforeKeys(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", 0)
	pkts := readPackets(t, muxClipBytes(t, c))
	keys, pats := 0, 0
	for i, p := range pkts {
		if p.PID == 0 {
			pats++
		}
		if p.PID == firstPID && p.PUSI && p.RandomAccess {
			keys++
			require.GreaterOrEqual(t, i, 2)
			assert.Equal(t, uint16(0), pkts[i-2].PID)
			assert.Equal(t, uint16(4096), pkts[i-1].PID)
		}
	}
	assert.Equal(t, keys, pats)
	assert.Greater(t, keys, 1)
}

func TestMuxSetTracks(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", 0)
	half := len(c.frames) / 2

	t.Run("drop audio", func(t *testing.T) {
		var buf bytes.Buffer
		m, err := NewMuxer(&buf, c.tracks)
		require.NoError(t, err)
		muxFrames(t, m, c.frames[:half])
		require.NoError(t, m.SetTracks([]*media.TrackConfig{c.tracks[0], nil}))
		assert.Error(t, m.WriteFrame(byTrack(c.frames, 1)[0]))
		muxFrames(t, m, byTrack(c.frames[half:], 0))
		d, out := demuxAll(t, buf.Bytes())
		assert.Zero(t, d.Stats().DecodeErrors)
		assert.Len(t, byTrack(out, 0), len(byTrack(c.frames, 0)))
	})

	t.Run("drop video", func(t *testing.T) {
		var first, second bytes.Buffer
		m, err := NewMuxer(&first, c.tracks)
		require.NoError(t, err)
		muxFrames(t, m, c.frames[:half])
		m.SetOutput(&second)
		require.NoError(t, m.SetTracks([]*media.TrackConfig{nil, c.tracks[1]}))
		audio := byTrack(c.frames[half:], 1)
		muxFrames(t, m, audio)

		pkts := readPackets(t, second.Bytes())
		var tables []int64 // PCR of the PES after each table pair
		var pcrCount, pesCount int
		for i, p := range pkts {
			if p.PID == firstPID+1 && p.PUSI {
				pesCount++
				if p.PCR >= 0 {
					pcrCount++
				}
			}
			if p.PID == 0 {
				require.Less(t, i+2, len(pkts))
				tables = append(tables, pkts[i+2].PCR)
			}
		}
		assert.Equal(t, pesCount, pcrCount, "every audio PES carries a PCR")
		require.Greater(t, len(tables), 2)
		assert.Equal(t, (audio[0].DTS+63000)&ts33Mask, tables[0], "tables before the first audio frame")
		for i := 1; i < len(tables); i++ {
			gap := tables[i] - tables[i-1]
			assert.GreaterOrEqual(t, gap, int64(9000))
			assert.LessOrEqual(t, gap, int64(9000+1920))
		}
		dmx := mpegts.NewDemux(bytes.NewReader(second.Bytes()))
		ev, err := dmx.Next()
		require.NoError(t, err)
		require.NotNil(t, ev.PMT)
		assert.Equal(t, uint16(firstPID+1), ev.PMT.PCRPID)
	})

	t.Run("explicit tables", func(t *testing.T) {
		var buf bytes.Buffer
		m, err := NewMuxer(&buf, c.tracks)
		require.NoError(t, err)
		keys := 0
		explicit := false
		for _, f := range c.frames {
			if f.Track == 0 && f.Key {
				keys++
				if keys == 2 {
					require.NoError(t, m.WriteTables())
					explicit = true
				}
			}
			require.NoError(t, m.WriteFrame(f))
		}
		require.True(t, explicit)
		pats := 0
		for _, p := range readPackets(t, buf.Bytes()) {
			if p.PID == 0 {
				pats++
			}
		}
		assert.Equal(t, keys, pats)
	})

	t.Run("explicit tables on audio PCR", func(t *testing.T) {
		var buf bytes.Buffer
		m, err := NewMuxer(&buf, []*media.TrackConfig{nil, c.tracks[1]})
		require.NoError(t, err)
		audio := byTrack(c.frames, 1)
		require.NoError(t, m.WriteTables())
		muxFrames(t, m, audio[:4]) // 4 frames: under 9000 ticks since the explicit tables
		pats := 0
		for _, p := range readPackets(t, buf.Bytes()) {
			if p.PID == 0 {
				pats++
			}
		}
		assert.Equal(t, 1, pats)
	})

	t.Run("codec change", func(t *testing.T) {
		h265 := loadClip(t, "h265_aac51.ts", c.frames[half].DTS+90000)
		var first, second bytes.Buffer
		m, err := NewMuxer(&first, c.tracks)
		require.NoError(t, err)
		muxFrames(t, m, c.frames[:half])
		m.SetOutput(&second)
		require.NoError(t, m.SetTracks([]*media.TrackConfig{h265.tracks[0], c.tracks[1]}))
		muxFrames(t, m, h265.frames)

		version := func(b []byte) (uint8, uint8) {
			dmx := mpegts.NewDemux(bytes.NewReader(b))
			for {
				ev, err := dmx.Next()
				require.NoError(t, err)
				if ev.PMT != nil {
					return ev.PMT.Version, ev.PMT.Streams[0].Type
				}
			}
		}
		v1, t1 := version(first.Bytes())
		v2, t2 := version(second.Bytes())
		assert.Equal(t, uint8(0x1B), t1)
		assert.Equal(t, uint8(0x24), t2)
		assert.Equal(t, v1+1, v2)

		d, err := NewDemuxer(bytes.NewReader(second.Bytes()))
		require.NoError(t, err)
		video := byTrack(readAll(t, d), 0)
		require.NotEmpty(t, video)
		for _, f := range video {
			assert.Equal(t, media.CodecH265, f.Config.Codec)
		}

		// The continuity counter of the kept PID continued: the two outputs
		// demux as one stream without a gap.
		both := append(bytes.Clone(first.Bytes()), second.Bytes()...)
		dm := mpegts.NewDemux(bytes.NewReader(both))
		for {
			_, err := dm.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
		}
		assert.Zero(t, dm.Stats().CCErrors)
	})

	t.Run("errors", func(t *testing.T) {
		m, err := NewMuxer(io.Discard, c.tracks)
		require.NoError(t, err)
		assert.ErrorContains(t, m.SetTracks(nil), "ts: mux:")
		assert.ErrorContains(t, m.SetTracks([]*media.TrackConfig{nil, nil}), "ts: mux:")
		assert.ErrorContains(t, m.SetTracks([]*media.TrackConfig{{Codec: 99}}), "ts: mux:")
		muxFrames(t, m, c.frames[:3]) // still works after the failed calls
	})
}

func TestMuxSetOutput(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", 0)
	var outs [3]bytes.Buffer
	m, err := NewMuxer(&outs[0], c.tracks)
	require.NoError(t, err)
	third := len(c.frames) / 3
	muxFrames(t, m, c.frames[:third])
	m.SetOutput(&outs[1])
	require.NoError(t, m.WriteTables())
	muxFrames(t, m, c.frames[third:2*third])
	m.SetOutput(&outs[2])
	muxFrames(t, m, c.frames[2*third:])

	var all []byte
	for i := range outs {
		all = append(all, outs[i].Bytes()...)
	}
	dm := mpegts.NewDemux(bytes.NewReader(all))
	for {
		_, err := dm.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	assert.Zero(t, dm.Stats().CCErrors)
	d, out := demuxAll(t, all)
	assert.Zero(t, d.Stats().DecodeErrors)
	assert.Len(t, out, len(c.frames))
}

func TestMuxFrameErrors(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", 0)
	h265 := loadClip(t, "h265_aac51.ts", 0)
	m, err := NewMuxer(io.Discard, []*media.TrackConfig{c.tracks[0], nil})
	require.NoError(t, err)

	v := byTrack(c.frames, 0)[0]
	a := byTrack(c.frames, 1)[0]
	assert.ErrorContains(t, m.WriteFrame(a), "ts: mux: track 1 is not configured")
	assert.ErrorContains(t, m.WriteFrame(media.Frame{Track: 7}), "ts: mux:")
	assert.ErrorContains(t, m.WriteFrame(media.Frame{Track: -1}), "ts: mux:")
	assert.ErrorContains(t, m.WriteFrame(byTrack(h265.frames, 0)[0]), "ts: mux:")
	noData := v
	noData.Data = nil
	assert.ErrorContains(t, m.WriteFrame(noData), "ts: mux:")
	noCfg := v
	noCfg.Config = nil
	assert.ErrorContains(t, m.WriteFrame(noCfg), "keyframe without config")
	assert.NoError(t, m.WriteFrame(v))

	m2, err := NewMuxer(io.Discard, c.tracks)
	require.NoError(t, err)
	wrongCodec := a
	wrongCodec.Config = v.Config
	assert.ErrorContains(t, m2.WriteFrame(wrongCodec), "ts: mux:")
	two := a
	two.Data = [][]byte{{1}, {2}}
	assert.ErrorContains(t, m2.WriteFrame(two), "ts: mux:")
	bad := a
	bad.Config = &media.TrackConfig{Codec: media.CodecAAC, ASC: []byte{0}}
	assert.ErrorContains(t, m2.WriteFrame(bad), "ts: mux:")
	noCfgAudio := a
	noCfgAudio.Config = nil
	assert.NoError(t, m2.WriteFrame(noCfgAudio))

	_, err = NewMuxer(nil, c.tracks)
	assert.ErrorContains(t, err, "ts: mux:")
	_, err = NewMuxer(io.Discard, nil)
	assert.ErrorContains(t, err, "ts: mux:")
	assert.ErrorContains(t, (&Muxer{w: mpegts.NewWriter(io.Discard)}).WriteTables(), "ts: mux:")
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestMuxWriteErrors(t *testing.T) {
	c := loadClip(t, "h264_aac.ts", 0)
	m, err := NewMuxer(failWriter{}, c.tracks)
	require.NoError(t, err)
	assert.ErrorContains(t, m.WriteTables(), "ts: mux:")
	assert.ErrorContains(t, m.WriteFrame(c.frames[0]), "ts: mux:") // tables fail
	m2, err := NewMuxer(failWriter{}, []*media.TrackConfig{nil, c.tracks[1]})
	require.NoError(t, err)
	m2.explicit = true
	assert.ErrorContains(t, m2.WriteFrame(byTrack(c.frames, 1)[0]), "ts: mux:") // PES fails
}

func TestMuxFFprobe(t *testing.T) {
	path, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	for name, codec := range map[string]string{"h264_aac.ts": "h264", "h265_aac51.ts": "hevc"} {
		t.Run(name, func(t *testing.T) {
			c := loadClip(t, name, 0)
			file := filepath.Join(t.TempDir(), "out.ts")
			require.NoError(t, os.WriteFile(file, muxClipBytes(t, c), 0o600))
			cmd := exec.CommandContext(t.Context(), path, "-v", "error", "-show_streams", "-show_format", "-of", "json", file)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			require.NoError(t, err)
			assert.Empty(t, stderr.String())
			var res struct {
				Streams []struct {
					CodecName string `json:"codec_name"`
				} `json:"streams"`
				Format struct {
					StartTime string `json:"start_time"`
				} `json:"format"`
			}
			require.NoError(t, json.Unmarshal(out, &res))
			require.Len(t, res.Streams, 2)
			assert.Equal(t, codec, res.Streams[0].CodecName)
			assert.Equal(t, "aac", res.Streams[1].CodecName)
			assert.NotEmpty(t, res.Format.StartTime)
		})
	}
}
