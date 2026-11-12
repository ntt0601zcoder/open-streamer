package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
)

func fixturePath(name string) string {
	return filepath.Join("..", "..", "..", "testdata", "fixtures", name)
}

// topBoxes splits b into its top-level boxes (32-bit sizes only).
func topBoxes(t testing.TB, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 8)
		n := int(binary.BigEndian.Uint32(b))
		require.GreaterOrEqual(t, n, 8)
		require.LessOrEqual(t, n, len(b))
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

// splitFile returns the init segment of a fragmented file (every box up to
// and including moov) and its fragments (each moof with the boxes after it).
func splitFile(t testing.TB, b []byte) (init []byte, frags [][]byte) {
	t.Helper()
	boxes := topBoxes(t, b)
	i := 0
	for ; i < len(boxes); i++ {
		init = append(init, boxes[i]...)
		if string(boxes[i][4:8]) == "moov" {
			i++
			break
		}
	}
	for ; i < len(boxes); i++ {
		if string(boxes[i][4:8]) == "moof" || len(frags) == 0 {
			frags = append(frags, nil)
		}
		frags[len(frags)-1] = append(frags[len(frags)-1], boxes[i]...)
	}
	return init, frags
}

func encodeInit(t *testing.T, init *mp4.InitSegment) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, init.Encode(&buf))
	return buf.Bytes()
}

func encodeFrag(t *testing.T, frag *mp4.Fragment) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, frag.Encode(&buf))
	return buf.Bytes()
}

// muxInit merges single-track inits from fmp4.Init into one init: the
// track of inits[i] gets ID i+1.
func muxInit(t *testing.T, inits ...[]byte) []byte {
	t.Helper()
	base := decode(t, inits[0]).Init
	for i, b := range inits[1:] {
		in := decode(t, b).Init
		id := uint32(i + 2)
		in.Moov.Trak.Tkhd.TrackID = id
		in.Moov.Mvex.Trex.TrackID = id
		base.Moov.AddChild(in.Moov.Trak)
		base.Moov.Mvex.AddChild(in.Moov.Mvex.Trex)
	}
	return encodeInit(t, base)
}

// retrack rewrites the track ID of every fragment of a segment and returns
// the fragments without the styp.
func retrack(t *testing.T, seg []byte, id uint32) []byte {
	t.Helper()
	var out []byte
	for _, s := range decode(t, seg).Segments {
		for _, frag := range s.Fragments {
			for _, traf := range frag.Moof.Trafs {
				traf.Tfhd.TrackID = id
			}
			out = append(out, encodeFrag(t, frag)...)
		}
	}
	return out
}

// retimescale sets the timescale of every track of an init.
func retimescale(t *testing.T, init []byte, ts uint32) []byte {
	t.Helper()
	in := decode(t, init).Init
	for _, trak := range in.Moov.Traks {
		trak.Mdia.Mdhd.Timescale = ts
	}
	return encodeInit(t, in)
}

func lengthPrefixed(nalus [][]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

// ctoSegment writes video frames as one fragment of track 1 with the given
// composition offsets (trun version 1) and durations from the DTS steps.
func ctoSegment(t *testing.T, frames []media.Frame, seq uint32, cto func(media.Frame) int32) []byte {
	t.Helper()
	frag, err := mp4.CreateFragment(seq, 1)
	require.NoError(t, err)
	for i, f := range frames {
		dur := uint32(3600)
		if i+1 < len(frames) {
			dur = uint32(frames[i+1].DTS - f.DTS)
		}
		data := lengthPrefixed(f.Data)
		flags := mp4.NonSyncSampleFlags
		if f.Key {
			flags = mp4.SyncSampleFlags
		}
		frag.AddFullSample(mp4.FullSample{
			Sample:     mp4.Sample{Flags: flags, Dur: dur, Size: uint32(len(data)), CompositionTimeOffset: cto(f)},
			DecodeTime: uint64(f.DTS),
			Data:       data,
		})
	}
	require.Equal(t, byte(1), frag.Moof.Traf.Trun.Version)
	return encodeFrag(t, frag)
}

func byTrack(frames []media.Frame, track int) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

// requireDTSOrder checks that frames are in DTS order across tracks.
func requireDTSOrder(t *testing.T, frames []media.Frame) {
	t.Helper()
	for i := 1; i < len(frames); i++ {
		require.GreaterOrEqual(t, frames[i].DTS, frames[i-1].DTS, "frame %d (track %d) before frame %d (track %d)",
			i, frames[i].Track, i-1, frames[i-1].Track)
	}
}

// sameFrames checks a round trip: timestamps, flags and data equal, configs
// equal, one config pointer for the whole track.
func sameFrames(t *testing.T, want, got []media.Frame) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		w, g := want[i], got[i]
		require.Equal(t, w.PTS, g.PTS, "frame %d PTS", i)
		require.Equal(t, w.DTS, g.DTS, "frame %d DTS", i)
		require.Equal(t, w.Key, g.Key, "frame %d Key", i)
		require.Equal(t, w.RASL, g.RASL, "frame %d RASL", i)
		require.Equal(t, w.Data, g.Data, "frame %d Data", i)
		require.True(t, w.Config.Equal(g.Config), "frame %d Config", i)
		require.Same(t, got[0].Config, g.Config, "frame %d Config pointer", i)
		require.Zero(t, g.Epoch)
	}
}

func TestDemuxOurCMAF(t *testing.T) {
	all := shift(demux(t, "h264_aac.ts"), 900000)
	video, audio := byCodec(all, media.CodecH264), byCodec(all, media.CodecAAC)
	vcfg, acfg := video[0].Config, audio[0].Config
	vInit, err := fmp4.Init(vcfg)
	require.NoError(t, err)
	aInit, err := fmp4.Init(acfg)
	require.NoError(t, err)

	// One init per track, as our writer makes them.
	for _, in := range []struct {
		init []byte
		kind media.Kind
	}{{vInit, media.KindVideo}, {aInit, media.KindAudio}} {
		d, err := fmp4.NewDemuxer(in.init)
		require.NoError(t, err)
		assert.Equal(t, []media.Kind{in.kind}, d.Kinds())
	}

	d, err := fmp4.NewDemuxer(muxInit(t, vInit, aInit))
	require.NoError(t, err)
	require.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.Kinds())
	assert.Empty(t, d.Skipped())

	var keys []int
	for i, f := range video {
		if f.Key {
			keys = append(keys, i)
		}
	}
	require.Equal(t, 0, keys[0])
	require.Greater(t, len(keys), 3)

	// A segment per GOP: the video GOP's fragment, then the fragment of the
	// audio frames before the next GOP.
	var got []media.Frame
	ai := 0
	for k, start := range keys {
		end, limit := len(video), int64(math.MaxInt64)
		if k+1 < len(keys) {
			end = keys[k+1]
			limit = video[end].DTS
		}
		aj := ai
		for aj < len(audio) && audio[aj].DTS < limit {
			aj++
		}
		seg, _, err := fmp4.Segment(video[start:end], vcfg, uint32(k+1))
		require.NoError(t, err)
		if aj > ai {
			aseg, _, err := fmp4.Segment(audio[ai:aj], acfg, uint32(k+1))
			require.NoError(t, err)
			seg = append(seg, retrack(t, aseg, 2)...)
		}
		ai = aj
		out, err := d.Segment(seg)
		require.NoError(t, err)
		requireDTSOrder(t, out)
		got = append(got, out...)
	}
	sameFrames(t, video, byTrack(got, 0))
	sameFrames(t, audio, byTrack(got, 1))
	assert.Equal(t, fmp4.DemuxStats{}, d.Stats())
}

// ffprobeCounts returns the packet count of every stream of a file.
func ffprobeCounts(t *testing.T, path string) []int {
	t.Helper()
	bin := testpeer.Require(t, "ffprobe")
	out, err := exec.CommandContext(t.Context(), bin, "-v", "error", "-count_packets",
		"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	lines := strings.Fields(string(out))
	counts := make([]int, 0, len(lines))
	for _, line := range lines {
		n, err := strconv.Atoi(strings.TrimSuffix(line, ","))
		require.NoError(t, err)
		counts = append(counts, n)
	}
	return counts
}

// ffprobeTimes returns pts and dts, in stream timescale, of every packet of
// one stream.
func ffprobeTimes(t *testing.T, path, stream string) [][2]int64 {
	t.Helper()
	bin := testpeer.Require(t, "ffprobe")
	out, err := exec.CommandContext(t.Context(), bin, "-v", "error", "-select_streams", stream,
		"-show_entries", "packet=pts,dts", "-of", "csv=p=0", path).Output()
	require.NoError(t, err)
	lines := strings.Fields(string(out))
	times := make([][2]int64, 0, len(lines))
	for _, line := range lines {
		f := strings.Split(strings.TrimSuffix(line, ","), ",")
		require.Len(t, f, 2, line)
		p, err := strconv.ParseInt(f[0], 10, 64)
		require.NoError(t, err)
		d, err := strconv.ParseInt(f[1], 10, 64)
		require.NoError(t, err)
		times = append(times, [2]int64{p, d})
	}
	return times
}

func TestDemuxMuxedInit(t *testing.T) {
	path := fixturePath("h264_aac_frag.mp4")
	b := fixture(t, path)
	init, frags := splitFile(t, b)
	require.Greater(t, len(frags), 3)

	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	require.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.Kinds())
	frames, err := d.Segment(bytes.Join(frags, nil))
	require.NoError(t, err)
	requireDTSOrder(t, frames)

	v, a := byTrack(frames, 0), byTrack(frames, 1)
	assert.Equal(t, ffprobeCounts(t, path), []int{len(v), len(a)})
	assert.Len(t, v, 150)
	assert.Len(t, a, 283)

	// Interleaved by DTS, not one track after the other.
	switches := 0
	for i := 1; i < len(frames); i++ {
		if frames[i].Track != frames[i-1].Track {
			switches++
		}
	}
	assert.Greater(t, switches, 100)

	// Timestamps equal ffprobe's (video timescale 12800, audio 48000; no
	// edit list, so the first video DTS is 0). ffmpeg gives the first audio
	// sample a duration of 3840.
	for i, want := range ffprobeTimes(t, path, "v:0") {
		require.Equal(t, media.SampleTime(want[0], 12800), v[i].PTS, "video frame %d", i)
		require.Equal(t, media.SampleTime(want[1], 12800), v[i].DTS, "video frame %d", i)
	}
	for i, want := range ffprobeTimes(t, path, "a:0") {
		require.Equal(t, media.SampleTime(want[0], 48000), a[i].PTS, "audio frame %d", i)
		require.Equal(t, media.SampleTime(want[1], 48000), a[i].DTS, "audio frame %d", i)
	}
	assert.Equal(t, int64(0), v[0].DTS)
	assert.Equal(t, int64(7200), v[0].PTS)
	assert.Equal(t, int64(7200), a[1].DTS)
	keys := 0
	for _, f := range v {
		if f.Key {
			keys++
		}
		require.Equal(t, 320, f.Config.Width)
	}
	assert.True(t, v[0].Key)
	assert.Equal(t, 6, keys)
	for _, f := range a {
		require.True(t, f.Key)
		require.Len(t, f.Data, 1)
		require.Equal(t, 48000, f.Config.SampleRate)
	}

	// Fragment by fragment gives the same frames per track.
	d2, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	var per []media.Frame
	for _, frag := range frags {
		out, err := d2.Segment(frag)
		require.NoError(t, err)
		requireDTSOrder(t, out)
		per = append(per, out...)
	}
	for track := range 2 {
		want, got := byTrack(frames, track), byTrack(per, track)
		require.Len(t, got, len(want))
		for i := range want {
			require.Equal(t, want[i].PTS, got[i].PTS)
			require.Equal(t, want[i].DTS, got[i].DTS)
			require.Equal(t, want[i].Data, got[i].Data)
		}
	}
}

func fixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func gop2(t *testing.T) []media.Frame {
	t.Helper()
	video := shift(byCodec(demux(t, "h264_aac.ts"), media.CodecH264), 900000)[:50]
	require.True(t, video[0].Key)
	require.True(t, video[25].Key)
	return video
}

func TestDemuxNegativeCTO(t *testing.T) {
	video := gop2(t)
	cfg := video[0].Config
	init, err := fmp4.Init(cfg)
	require.NoError(t, err)
	off := video[0].PTS - video[0].DTS
	cto := func(f media.Frame) int32 { return int32(f.PTS - f.DTS - off) }
	minCTO := int64(0)
	for _, f := range video[:25] {
		minCTO = min(minCTO, int64(cto(f)))
	}
	require.Negative(t, minCTO)

	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	out1, err := d.Segment(ctoSegment(t, video[:25], 1, cto))
	require.NoError(t, err)
	out2, err := d.Segment(ctoSegment(t, video[25:], 2, cto))
	require.NoError(t, err)
	out := append(out1, out2...)
	require.Len(t, out, 50)
	assert.Zero(t, d.Stats().DroppedPTSBeforeDTS)
	for i, f := range out {
		require.GreaterOrEqual(t, f.PTS, f.DTS, "frame %d", i)
		// Presentation is unchanged; decode moves down by the most
		// negative offset of the first segment.
		require.Equal(t, video[i].PTS-off, f.PTS, "frame %d PTS", i)
		require.Equal(t, video[i].DTS+minCTO, f.DTS, "frame %d DTS", i)
		require.Equal(t, video[i].Key, f.Key)
		require.Equal(t, video[i].Data, f.Data)
	}
}

// A frame in a later segment whose offset is below the first segment's
// minimum still has PTS < DTS after the shift: it is dropped and counted.
func TestDemuxNegativeCTOLaterFrameDropped(t *testing.T) {
	video := gop2(t)
	init, err := fmp4.Init(video[0].Config)
	require.NoError(t, err)
	off := video[0].PTS - video[0].DTS
	minCTO := int32(0)
	for _, f := range video[:25] {
		minCTO = min(minCTO, int32(f.PTS-f.DTS-off))
	}
	bad := 30
	cto := func(f media.Frame) int32 {
		if f.DTS == video[bad].DTS {
			return minCTO - 3600
		}
		return int32(f.PTS - f.DTS - off)
	}
	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	out1, err := d.Segment(ctoSegment(t, video[:25], 1, cto))
	require.NoError(t, err)
	out2, err := d.Segment(ctoSegment(t, video[25:], 2, cto))
	require.NoError(t, err)
	out := append(out1, out2...)
	require.Len(t, out, 49)
	assert.Equal(t, uint64(1), d.Stats().DroppedPTSBeforeDTS)
	for _, f := range out {
		require.GreaterOrEqual(t, f.PTS, f.DTS)
		require.NotEqual(t, video[bad].DTS+int64(minCTO), f.DTS)
	}
}

// An edit list's media time is subtracted from PTS and DTS; empty edits are
// skipped.
func TestDemuxEditList(t *testing.T) {
	video := byCodec(demux(t, "h264_aac.ts"), media.CodecH264)[:25]
	video = shift(video, -video[0].DTS)
	cfg := video[0].Config
	base, err := fmp4.Init(cfg)
	require.NoError(t, err)
	in := decode(t, base).Init
	edts := &mp4.EdtsBox{}
	edts.AddChild(&mp4.ElstBox{Entries: []mp4.ElstEntry{
		{SegmentDuration: 1000, MediaTime: -1, MediaRateInteger: 1},
		{SegmentDuration: 0, MediaTime: 7200, MediaRateInteger: 1},
	}})
	in.Moov.Trak.AddChild(edts)
	d, err := fmp4.NewDemuxer(encodeInit(t, in))
	require.NoError(t, err)
	seg, _, err := fmp4.Segment(video, cfg, 1)
	require.NoError(t, err)
	out, err := d.Segment(seg)
	require.NoError(t, err)
	require.Len(t, out, len(video))
	assert.Negative(t, out[0].DTS)
	for i, f := range out {
		require.Equal(t, video[i].PTS-7200, f.PTS, "frame %d", i)
		require.Equal(t, video[i].DTS-7200, f.DTS, "frame %d", i)
	}
}

func TestDemuxTimescale(t *testing.T) {
	t.Run("44.1 kHz audio over 10 min", func(t *testing.T) {
		cfg, err := media.NewAACConfig([]byte{0x12, 0x10})
		require.NoError(t, err)
		require.Equal(t, 44100, cfg.SampleRate)
		init, err := fmp4.Init(cfg)
		require.NoError(t, err)
		n := 600*44100/1024 + 1
		frag, err := mp4.CreateFragment(1, 1)
		require.NoError(t, err)
		for k := range n {
			frag.AddFullSample(mp4.FullSample{
				Sample:     mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: 1024, Size: 1},
				DecodeTime: uint64(k) * 1024,
				Data:       []byte{byte(k)},
			})
		}
		d, err := fmp4.NewDemuxer(init)
		require.NoError(t, err)
		out, err := d.Segment(encodeFrag(t, frag))
		require.NoError(t, err)
		require.Len(t, out, n)
		for k, f := range out {
			want := media.SampleTime(int64(k)*1024, 44100)
			require.Equal(t, want, f.DTS, "frame %d", k)
			require.Equal(t, want, f.PTS, "frame %d", k)
			require.Equal(t, [][]byte{{byte(k)}}, f.Data)
		}
		assert.Equal(t, media.SampleTime(int64(n-1)*1024, 44100), out[n-1].PTS)
		assert.Equal(t, int64(53998237), out[n-1].PTS)
	})
	t.Run("tfdt 1.7e16 at 10 MHz", func(t *testing.T) {
		video := byCodec(demux(t, "h264_aac.ts"), media.CodecH264)[:3]
		init, err := fmp4.Init(video[0].Config)
		require.NoError(t, err)
		init = retimescale(t, init, 10_000_000)
		const base = uint64(17_000_000_000_000_000)
		frag, err := mp4.CreateFragment(1, 1)
		require.NoError(t, err)
		for i, f := range video {
			data := lengthPrefixed(f.Data)
			frag.AddFullSample(mp4.FullSample{
				Sample:     mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: 400_000, Size: uint32(len(data)), CompositionTimeOffset: 800_000},
				DecodeTime: base + uint64(i)*400_000,
				Data:       data,
			})
		}
		d, err := fmp4.NewDemuxer(init)
		require.NoError(t, err)
		out, err := d.Segment(encodeFrag(t, frag))
		require.NoError(t, err)
		require.Len(t, out, 3)
		for i, f := range out {
			dts := int64(153_000_000_000_000) + int64(i)*3600
			require.Equal(t, dts, f.DTS, "frame %d", i)
			require.Equal(t, dts+7200, f.PTS, "frame %d", i)
		}
	})
}

// withSets puts the config's parameter sets in front of every keyframe's
// NAL units.
func withSets(frames []media.Frame, c *media.TrackConfig) []media.Frame {
	out := make([]media.Frame, len(frames))
	for i, f := range frames {
		out[i] = f
		if f.Key {
			var d [][]byte
			d = append(d, c.SPS...)
			d = append(d, c.PPS...)
			out[i].Data = append(d, f.Data...)
		}
	}
	return out
}

// Parameter sets in samples move into the Config: unchanged sets keep the
// init's config pointer, new ones give a new config.
func TestDemuxInBandParamSets(t *testing.T) {
	v1 := shift(byCodec(demux(t, "h264_aac.ts"), media.CodecH264), 900000)[:25]
	v2 := demux(t, "h264_640.ts")[:25]
	v2 = shift(v2, v1[24].DTS+3600-v2[0].DTS)
	cfg1, cfg2 := v1[0].Config, v2[0].Config
	require.False(t, cfg1.Equal(cfg2))
	init, err := fmp4.Init(cfg1)
	require.NoError(t, err)
	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)

	seg1, _, err := fmp4.Segment(withSets(v1, cfg1), cfg1, 1)
	require.NoError(t, err)
	seg2, _, err := fmp4.Segment(withSets(v2, cfg2), cfg1, 2)
	require.NoError(t, err)
	out1, err := d.Segment(seg1)
	require.NoError(t, err)
	out2, err := d.Segment(seg2)
	require.NoError(t, err)
	sameFrames(t, v1, out1)
	sameFrames(t, v2, out2)
	assert.NotSame(t, out1[0].Config, out2[0].Config)
	assert.Equal(t, 640, out2[0].Config.Width)

	// Back to the first sets.
	out3, err := d.Segment(seg1)
	require.NoError(t, err)
	require.True(t, out3[0].Config.Equal(cfg1))
	assert.Equal(t, fmp4.DemuxStats{}, d.Stats())
}

// An avc3 track without parameter sets in its sample entry drops video until
// the first in-band sets.
func TestDemuxAVC3InBandOnly(t *testing.T) {
	video := gop2(t)
	cfg := video[0].Config
	in := mp4.CreateEmptyInit()
	trak := in.AddEmptyTrack(90000, "video", "und")
	require.NoError(t, trak.SetAVCDescriptor("avc3", cfg.SPS, cfg.PPS, false))
	d, err := fmp4.NewDemuxer(encodeInit(t, in))
	require.NoError(t, err)
	require.Equal(t, []media.Kind{media.KindVideo}, d.Kinds())

	// The first GOP without its keyframe, then the second GOP with sets.
	frames := append(append([]media.Frame(nil), video[1:25]...), withSets(video[25:], cfg)...)
	seg, _, err := fmp4.Segment(frames, cfg, 1)
	require.NoError(t, err)
	out, err := d.Segment(seg)
	require.NoError(t, err)
	assert.Equal(t, uint64(24), d.Stats().DroppedNoConfig)
	sameFrames(t, video[25:], out)
}

func TestDemuxSkipsUnsupportedTracks(t *testing.T) {
	video := gop2(t)[:25]
	cfg := video[0].Config
	in := mp4.CreateEmptyInit()
	in.AddEmptyTrack(1000, "wvtt", "und").Mdia.Minf.Stbl.Stsd.AddChild(mp4.NewWvttBox())
	vt := in.AddEmptyTrack(90000, "video", "und")
	require.NoError(t, vt.SetAVCDescriptor("avc1", cfg.SPS, cfg.PPS, true))
	d, err := fmp4.NewDemuxer(encodeInit(t, in))
	require.NoError(t, err)
	require.Equal(t, []media.Kind{media.KindVideo}, d.Kinds())
	require.Len(t, d.Skipped(), 1)
	assert.Contains(t, d.Skipped()[0], "wvtt")

	text, err := mp4.CreateFragment(1, 1)
	require.NoError(t, err)
	text.AddFullSample(mp4.FullSample{Sample: mp4.Sample{Dur: 1000, Size: 4}, Data: []byte("vtte")})
	vseg, _, err := fmp4.Segment(video, cfg, 1)
	require.NoError(t, err)
	out, err := d.Segment(append(encodeFrag(t, text), retrack(t, vseg, 2)...))
	require.NoError(t, err)
	for _, f := range out {
		require.Equal(t, 0, f.Track)
	}
	sameFrames(t, video, out)
}

func TestDemuxHostile(t *testing.T) {
	b := fixture(t, fixturePath("h264_aac_frag.mp4"))
	init, frags := splitFile(t, b)
	for n := range len(init) {
		_, err := fmp4.NewDemuxer(init[:n])
		require.Error(t, err, "init truncated to %d bytes", n)
		assert.Contains(t, err.Error(), "fmp4: ")
	}
	d, err := fmp4.NewDemuxer(init)
	require.NoError(t, err)
	seg := frags[0]
	moofLen := int(binary.BigEndian.Uint32(seg))
	for n := 1; n < len(seg); n++ {
		if n > moofLen+64 && n%61 != 0 {
			continue
		}
		_, err := d.Segment(seg[:n])
		require.Error(t, err, "segment truncated to %d bytes", n)
		assert.Contains(t, err.Error(), "fmp4: ")
	}

	rng := rand.New(rand.NewPCG(1, 2))
	types := []string{"moof", "mfhd", "traf", "tfhd", "tfdt", "trun", "mdat", "styp", "free", "moov", "trak", "mdia"}
	var box func(depth int) []byte
	box = func(depth int) []byte {
		typ := types[rng.IntN(len(types))]
		var body []byte
		if depth < 3 && rng.IntN(2) == 0 {
			for range rng.IntN(4) {
				body = append(body, box(depth+1)...)
			}
		} else {
			body = make([]byte, rng.IntN(48))
			for i := range body {
				body[i] = byte(rng.Uint32())
			}
		}
		out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
		if rng.IntN(16) == 0 {
			out = binary.BigEndian.AppendUint32(nil, rng.Uint32())
		}
		return append(append(out, typ...), body...)
	}
	for range 1000 {
		r := box(0)
		for range rng.IntN(4) {
			r = append(r, box(0)...)
		}
		_, _ = d.Segment(r)
		_, _ = fmp4.NewDemuxer(r)

		m := bytes.Clone(seg)
		for range 1 + rng.IntN(4) {
			m[rng.IntN(moofLen+16)] = byte(rng.Uint32())
		}
		_, _ = d.Segment(m)
		mi := bytes.Clone(init)
		for range 1 + rng.IntN(4) {
			mi[rng.IntN(len(mi))] = byte(rng.Uint32())
		}
		if dm, err := fmp4.NewDemuxer(mi); err == nil {
			_, _ = dm.Segment(seg)
		}
	}
}

func FuzzDemuxSegment(f *testing.F) {
	b, err := os.ReadFile(fixturePath("h264_aac_frag.mp4"))
	if err != nil {
		f.Fatal(err)
	}
	init, frags := splitFile(f, b)
	f.Add(frags[0])
	f.Add(frags[1][:200])
	f.Fuzz(func(t *testing.T, seg []byte) {
		d, err := fmp4.NewDemuxer(init)
		require.NoError(t, err)
		frames, err := d.Segment(seg)
		if err != nil {
			return
		}
		for i := 1; i < len(frames); i++ {
			require.GreaterOrEqual(t, frames[i].DTS, frames[i-1].DTS)
		}
		for _, fr := range frames {
			require.GreaterOrEqual(t, fr.PTS, fr.DTS)
		}
	})
}
