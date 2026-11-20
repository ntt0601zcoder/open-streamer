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

func remapDemux(t *testing.T, data []byte) (*ts.Demuxer, []media.Frame) {
	t.Helper()
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	frames, tracks, changeAt := readFrames(t, d)
	for _, n := range tracks {
		assert.Equal(t, 2, n)
	}
	require.Len(t, changeAt, 1)
	return d, frames
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
	d, frames := remapDemux(t, fixtureBytes(t, "h264_aac_pmtchange.ts"))
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
	out := make([]media.Frame, 0, len(frames))
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
	d, frames := remapDemux(t, fixtureBytes(t, "h264_aac_audiopid.ts"))
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

// fixturePES returns the PES packets of h264_aac.ts: video on PID 256,
// audio on PID 257.
func fixturePES(t *testing.T) []*mpegts.PES {
	t.Helper()
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
}

func TestRemapIdenticalPMT(t *testing.T) {
	pes := fixturePES(t)
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

// Fixture PIDs and the stream types the remap tests use.
const (
	fixVideo = uint16(256)
	fixAudio = uint16(257)
	typeH264 = uint8(0x1B)
	typeAAC  = uint8(0x0F)
)

// pmtPhase is one PMT and the fixture PES written under it: route maps a
// fixture PID to the PIDs it is written on (none: dropped).
type pmtPhase struct {
	streams []mpegts.Stream
	route   map[uint16][]uint16
}

// writePhases writes the fixture's PES split evenly over the phases, each
// phase opening with its PMT.
func writePhases(t *testing.T, phases ...pmtPhase) []byte {
	t.Helper()
	pes := fixturePES(t)
	var buf bytes.Buffer
	w := mpegts.NewWriter(&buf)
	per := (len(pes) + len(phases) - 1) / len(phases)
	for i, p := range pes {
		ph := phases[i/per]
		if i%per == 0 {
			writeProgram(t, w, ph.streams[0].PID, ph.streams...)
		}
		for _, pid := range ph.route[p.PID] {
			video := false
			for _, s := range ph.streams {
				if s.PID == pid {
					video = s.Type == typeH264
				}
			}
			require.NoError(t, w.WritePES(pid, p.Data, mpegts.PESOptions{
				PTS: p.PTS, DTS: p.DTS, HasDTS: p.DTS != p.PTS,
				RandomAccess: p.RandomAccess, PCR: -1, Video: video,
			}))
		}
	}
	return buf.Bytes()
}

// genFrame is a frame and the PMT generation it was read under.
type genFrame struct {
	media.Frame
	gen uint32
}

func readGens(t *testing.T, d *ts.Demuxer) []genFrame {
	t.Helper()
	var out []genFrame
	for {
		f, g, err := d.ReadGen()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, genFrame{f, g})
	}
}

// genConfigs returns the distinct Configs of a track per generation.
func genConfigs(frames []genFrame, track int) map[uint32][]*media.TrackConfig {
	out := map[uint32][]*media.TrackConfig{}
	for _, f := range frames {
		if f.Track != track {
			continue
		}
		if c := out[f.gen]; len(c) == 0 || c[len(c)-1] != f.Config {
			out[f.gen] = append(c, f.Config)
		}
	}
	return out
}

func genCounts(frames []genFrame) map[uint32]map[int]int {
	out := map[uint32]map[int]int{}
	for _, f := range frames {
		if out[f.gen] == nil {
			out[f.gen] = map[int]int{}
		}
		out[f.gen][f.Track]++
	}
	return out
}

var (
	streamVideo = mpegts.Stream{PID: fixVideo, Type: typeH264}
	streamAudio = mpegts.Stream{PID: fixAudio, Type: typeAAC}
	routeBoth   = map[uint16][]uint16{fixVideo: {fixVideo}, fixAudio: {fixAudio}}
	routeVideo  = map[uint16][]uint16{fixVideo: {fixVideo}}
)

// A PMT that gains a stream appends a slot: the existing slot keeps its
// state, nothing is remapped, and the new slot delivers.
func TestRemapPMTGainsStream(t *testing.T) {
	data := writePhases(t,
		pmtPhase{streams: []mpegts.Stream{streamVideo}, route: routeVideo},
		pmtPhase{streams: []mpegts.Stream{streamVideo, streamAudio}, route: routeBoth})
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	assert.Equal(t, 1, d.NumTracks())
	frames := readGens(t, d)
	assert.EqualValues(t, 1, d.Stats().PMTChanges)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	assert.Nil(t, d.TakeRemapped(1), "an appended slot is no remap")
	counts := genCounts(frames)
	assert.Zero(t, counts[0][1], "no audio before the PMT has it")
	assert.Positive(t, counts[1][1], "the appended slot delivers")
	video := genConfigs(frames, 0)
	require.Len(t, video[0], 1)
	require.Len(t, video[1], 1)
	assert.Same(t, video[0][0], video[1][0], "video keeps its state")
}

// The track cap applies to slots a later PMT appends: the streams over it
// are skipped and reported, the others delivered.
func TestRemapCapHitByLaterAppend(t *testing.T) {
	audio2 := mpegts.Stream{PID: 0x300, Type: typeAAC}
	data := writePhases(t,
		pmtPhase{streams: []mpegts.Stream{streamVideo}, route: routeVideo},
		pmtPhase{
			streams: []mpegts.Stream{streamVideo, streamAudio, audio2},
			route:   map[uint16][]uint16{fixVideo: {fixVideo}, fixAudio: {fixAudio, audio2.PID}},
		})
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true, MaxTracks: 2})
	require.NoError(t, err)
	assert.Empty(t, d.Skipped(), "under the cap at the first PMT")
	frames := readGens(t, d)
	assert.Equal(t, 2, d.NumTracks())
	assert.Equal(t, []uint16{fixVideo, fixAudio}, d.TrackPIDs())
	require.Len(t, d.Skipped(), 1)
	assert.Contains(t, d.Skipped()[0], "track limit 2")
	assert.Contains(t, d.Skipped()[0], "pid 768")
	counts := genCounts(frames)
	assert.Positive(t, counts[1][1], "the appended slot under the cap delivers")
	for _, f := range frames {
		assert.Less(t, f.Track, 2)
	}
}

// A slot whose stream leaves the PMT produces nothing; when the same PID and
// type come back, the slot starts fresh with a new Config, and neither
// change is a remap of an active slot.
func TestRemapOrdinalDisappearsAndReturns(t *testing.T) {
	data := writePhases(t,
		pmtPhase{streams: []mpegts.Stream{streamVideo, streamAudio}, route: routeBoth},
		pmtPhase{streams: []mpegts.Stream{streamVideo}, route: routeVideo},
		pmtPhase{streams: []mpegts.Stream{streamVideo, streamAudio}, route: routeBoth})
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	frames := readGens(t, d)
	assert.EqualValues(t, 2, d.Stats().PMTChanges)
	assert.Equal(t, 2, d.NumTracks(), "the slot stays while absent")
	assert.Nil(t, d.TakeRemapped(1), "dropping a slot is no remap")
	assert.Nil(t, d.TakeRemapped(2), "an absent slot returning is no remap")
	counts := genCounts(frames)
	assert.Positive(t, counts[0][1])
	assert.Zero(t, counts[1][1], "nothing from the absent slot")
	assert.Positive(t, counts[2][1])
	audio := genConfigs(frames, 1)
	require.Len(t, audio[0], 1)
	require.Len(t, audio[2], 1)
	assert.NotSame(t, audio[0][0], audio[2][0], "the returning slot starts fresh")
	assert.True(t, audio[0][0].Equal(audio[2][0]))
	video := genConfigs(frames, 0)
	assert.Same(t, video[0][0], video[2][0], "video keeps its state throughout")
}

// PIDs reused with new types (video and audio swap PIDs) remap both slots:
// each starts fresh and the data follows the new PMT.
func TestRemapPIDReusedWithNewType(t *testing.T) {
	data := writePhases(t,
		pmtPhase{streams: []mpegts.Stream{streamVideo, streamAudio}, route: routeBoth},
		pmtPhase{
			streams: []mpegts.Stream{{PID: fixAudio, Type: typeH264}, {PID: fixVideo, Type: typeAAC}},
			route:   map[uint16][]uint16{fixVideo: {fixAudio}, fixAudio: {fixVideo}},
		})
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(data), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	frames := readGens(t, d)
	assert.EqualValues(t, 1, d.Stats().PMTChanges)
	assert.Equal(t, []uint16{fixAudio, fixVideo}, d.TrackPIDs())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	assert.Equal(t, []int{0, 1}, d.TakeRemapped(1))
	for _, f := range frames {
		want := media.CodecH264
		if f.Track == 1 {
			want = media.CodecAAC
		}
		require.Equal(t, want, f.Config.Codec, "track %d gen %d", f.Track, f.gen)
	}
	counts := genCounts(frames)
	all := map[int]int{}
	for _, f := range demuxAll(t, fixtureBytes(t, "h264_aac.ts")) {
		all[f.Track]++
	}
	assert.Positive(t, counts[1][0])
	assert.Equal(t, all[1], counts[0][1]+counts[1][1], "every audio frame, on either PID")
	for tr := range 2 {
		c := genConfigs(frames, tr)
		require.Len(t, c[0], 1)
		require.Len(t, c[1], 1)
		assert.NotSame(t, c[0][0], c[1][0], "track %d starts fresh", tr)
	}
}

func demuxAll(t *testing.T, data []byte) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	return readOnly(t, d)
}
