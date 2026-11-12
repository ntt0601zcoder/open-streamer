package ts_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return b
}

// readFrames reads d to the end, recording the track count after every frame
// and the index of the first frame read after each PMT change.
func readFrames(t *testing.T, d *ts.Demuxer) (frames []media.Frame, tracks []int, changeAt []int) {
	t.Helper()
	changes := d.Stats().PMTChanges
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return frames, tracks, changeAt
		}
		require.NoError(t, err)
		if c := d.Stats().PMTChanges; c != changes {
			changes = c
			changeAt = append(changeAt, len(frames))
		}
		frames = append(frames, f)
		tracks = append(tracks, d.NumTracks())
	}
}

func remapDemux(t *testing.T, data []byte) (*ts.Demuxer, []media.Frame, []int) {
	t.Helper()
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	frames, tracks, changeAt := readFrames(t, d)
	for _, n := range tracks {
		assert.Equal(t, 2, n)
	}
	require.Len(t, changeAt, 1)
	return d, frames, []int{changeAt[0]}
}

func configs(frames []media.Frame, track int) (ptrs []*media.TrackConfig) {
	for _, f := range frames {
		if f.Track != track {
			continue
		}
		if len(ptrs) == 0 || ptrs[len(ptrs)-1] != f.Config {
			ptrs = append(ptrs, f.Config)
		}
	}
	return ptrs
}

func TestRemapPMTChange(t *testing.T) {
	d, frames, _ := remapDemux(t, fixtureBytes(t, "h264_aac_pmtchange.ts"))
	assert.Equal(t, uint64(1), d.Stats().PMTChanges)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	assert.Equal(t, []uint16{0x200, 0x201}, d.TrackPIDs())
	assert.Empty(t, d.Skipped())
	for tr := 0; tr < 2; tr++ {
		cfgs := configs(frames, tr)
		require.Len(t, cfgs, 2, "track %d", tr)
		assert.NotSame(t, cfgs[0], cfgs[1], "track %d", tr)
		assert.True(t, cfgs[0].Equal(cfgs[1]), "track %d", tr)
	}

	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var out []media.Frame
	for _, f := range frames {
		out = append(out, c.Push(f)...)
	}
	out = append(out, c.Flush()...)
	assert.Len(t, c.Epochs(), 2)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
}

func TestRemapAudioPIDOnly(t *testing.T) {
	_, ref := func() (*ts.Demuxer, []media.Frame) {
		d, err := ts.NewDemuxer(bytes.NewReader(fixtureBytes(t, "h264_aac_audiopid.ts")))
		require.NoError(t, err)
		return d, readOnly(t, d)
	}()
	d, frames, _ := remapDemux(t, fixtureBytes(t, "h264_aac_audiopid.ts"))
	assert.Equal(t, uint64(1), d.Stats().PMTChanges)
	assert.Equal(t, []uint16{0x100, 0x120}, d.TrackPIDs())
	// Video keeps its state: one Config, and every video frame of the
	// first-PMT-only demux (the video PID never changes) is delivered.
	assert.Len(t, configs(frames, 0), 1)
	assert.Len(t, byTrackOf(frames, 0), len(byTrackOf(ref, 0)))
	// Audio restarts with a new Config.
	assert.Len(t, configs(frames, 1), 2)
	assert.NotEmpty(t, byTrackOf(frames, 1))
}

func readOnly(t *testing.T, d *ts.Demuxer) []media.Frame {
	t.Helper()
	f, _, _ := readFrames(t, d)
	return f
}

func byTrackOf(frames []media.Frame, track int) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

// writeProgram sets the program and writes its tables.
func writeProgram(t *testing.T, w *mpegts.Writer, pcr uint16, streams ...mpegts.Stream) {
	t.Helper()
	require.NoError(t, w.SetProgram(mpegts.PMT{PCRPID: pcr, Streams: streams}))
	require.NoError(t, w.WriteTables())
}

func TestRemapIdenticalPMT(t *testing.T) {
	pes := func() []*mpegts.PES {
		d := mpegts.NewDemux(bytes.NewReader(fixtureBytes(t, "h264_aac.ts")))
		var out []*mpegts.PES
		for {
			ev, err := d.Next()
			if errors.Is(err, io.EOF) {
				return out
			}
			require.NoError(t, err)
			if ev.PES != nil {
				out = append(out, ev.PES)
			}
		}
	}()
	video := mpegts.Stream{PID: 256, Type: 0x1B}
	audio := mpegts.Stream{PID: 257, Type: 0x0F}
	var buf bytes.Buffer
	w := mpegts.NewWriter(&buf)
	writeProgram(t, w, 256, video, audio)
	for i, p := range pes {
		if i == len(pes)/2 {
			// A new PMT version (the PCR PID differs) with the same streams.
			writeProgram(t, w, 257, video, audio)
		}
		require.NoError(t, w.WritePES(p.PID, p.Data, mpegts.PESOptions{
			PTS: p.PTS, DTS: p.DTS, HasDTS: p.DTS != p.PTS,
			RandomAccess: p.RandomAccess, PCR: -1, Video: p.PID == 256,
		}))
	}
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(buf.Bytes()), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	frames, _, changeAt := readFrames(t, d)
	assert.Empty(t, changeAt)
	assert.Equal(t, uint64(0), d.Stats().PMTChanges)
	assert.Len(t, configs(frames, 0), 1)
	assert.Len(t, configs(frames, 1), 1)
	assert.Len(t, byTrackOf(frames, 0), 150)
}

func TestMaxTracks(t *testing.T) {
	var streams []mpegts.Stream
	for i := 0; i < 20; i++ {
		streams = append(streams, mpegts.Stream{PID: uint16(0x100 + i), Type: 0x0F})
	}
	var buf bytes.Buffer
	w := mpegts.NewWriter(&buf)
	writeProgram(t, w, streams[0].PID, streams...)
	for _, opt := range []ts.DemuxOptions{{}, {Remap: true}, {FollowPMT: true}} {
		d, err := ts.NewDemuxerWithOptions(bytes.NewReader(buf.Bytes()), opt)
		require.NoError(t, err)
		assert.Equal(t, 16, d.NumTracks())
		require.Len(t, d.Skipped(), 4)
		for _, s := range d.Skipped() {
			assert.Contains(t, s, "track limit 16")
		}
	}
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(buf.Bytes()), ts.DemuxOptions{MaxTracks: 3})
	require.NoError(t, err)
	assert.Equal(t, 3, d.NumTracks())
	assert.Len(t, d.Skipped(), 17)
}

func TestRemapFollowPMTExclusive(t *testing.T) {
	_, err := ts.NewDemuxerWithOptions(bytes.NewReader(fixtureBytes(t, "h264_aac.ts")), ts.DemuxOptions{FollowPMT: true, Remap: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exclusive")
}

func TestReadGenTagsFlushedFramesWithOldGeneration(t *testing.T) {
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(fixtureBytes(t, "h264_aac_pmtchange.ts")), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	var gens []uint32
	cfgs := map[uint32]map[*media.TrackConfig]bool{}
	for {
		f, g, err := d.ReadGen()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if len(gens) > 0 {
			assert.GreaterOrEqual(t, g, gens[len(gens)-1])
		}
		gens = append(gens, g)
		if cfgs[g] == nil {
			cfgs[g] = map[*media.TrackConfig]bool{}
		}
		cfgs[g][f.Config] = true
	}
	assert.Equal(t, uint32(1), gens[len(gens)-1])
	assert.Zero(t, gens[0])
	assert.Len(t, cfgs[0], 2, "generation 0 holds only old Configs: one per track")
	assert.Len(t, cfgs[1], 2)
	assert.Equal(t, []int{0, 1}, d.TakeRemapped(1))
	assert.Nil(t, d.TakeRemapped(1), "taken once")
	assert.Nil(t, d.TakeRemapped(5))
}
