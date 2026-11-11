package ts

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return b
}

func demuxAll(t *testing.T, data []byte) (*Demuxer, []media.Frame) {
	t.Helper()
	return demuxAllOpts(t, data, DemuxOptions{})
}

func demuxAllOpts(t *testing.T, data []byte, opt DemuxOptions) (*Demuxer, []media.Frame) {
	t.Helper()
	d, err := NewDemuxerWithOptions(bytes.NewReader(data), opt)
	require.NoError(t, err)
	return d, readAll(t, d)
}

func readAll(t *testing.T, d *Demuxer) []media.Frame {
	t.Helper()
	var frames []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return frames
		}
		require.NoError(t, err)
		frames = append(frames, f)
	}
}

// checkRaw runs the frame checks on raw demuxer output. It skips
// conform.RuleConfigChange: the demuxer emits source-time frames with Epoch
// 0, so a config change (as in h264_aac_then_640.ts) is never at an epoch
// start until the Clock assigns epochs.
func checkRaw(frames []media.Frame) []conform.Violation {
	return conform.CheckFrames(frames, conform.FrameOptions{Skip: []string{conform.RuleConfigChange}})
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

func TestDemuxH264AAC(t *testing.T) {
	d, frames := demuxAll(t, fixture(t, "h264_aac.ts"))
	assert.Equal(t, 2, d.NumTracks())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	v, a := byTrack(frames, 0), byTrack(frames, 1)
	require.Len(t, v, 150)
	require.Len(t, a, 283)
	assert.True(t, v[0].Key)
	assert.Equal(t, media.CodecH264, v[0].Config.Codec)
	assert.Equal(t, 320, v[0].Config.Width)
	assert.Equal(t, 240, v[0].Config.Height)
	assert.Equal(t, 48000, a[0].Config.SampleRate)
	assert.Equal(t, 2, a[0].Config.Channels)
	for _, f := range v {
		for _, n := range f.Data {
			typ := n[0] & 0x1f
			assert.NotContains(t, []byte{7, 8, 9}, typ)
		}
	}
	for i := 1; i < len(a); i++ {
		assert.Equal(t, int64(1920), a[i].DTS-a[i-1].DTS)
		assert.True(t, a[i].Key)
		assert.False(t, a[i].RASL)
	}
	assert.Nil(t, checkRaw(frames))
}

func TestDemuxH265AAC51(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h265_aac51.ts"))
	v, a := byTrack(frames, 0), byTrack(frames, 1)
	require.NotEmpty(t, v)
	assert.Equal(t, media.CodecH265, v[0].Config.Codec)
	assert.Len(t, v[0].Config.VPS, 1)
	require.Len(t, a, 260)
	assert.Equal(t, 44100, a[0].Config.SampleRate)
	assert.Equal(t, 6, a[0].Config.Channels)
	want := media.SampleTime(1024, 44100)
	for i := 1; i < len(a); i++ {
		delta := a[i].DTS - a[i-1].DTS
		assert.InDelta(t, want, delta, 1)
	}
	for _, f := range v {
		for _, n := range f.Data {
			typ := (n[0] >> 1) & 0x3f
			assert.NotContains(t, []byte{32, 33, 34, 35}, typ)
		}
	}
	assert.Nil(t, checkRaw(frames))
}

func TestDemuxH265RASL(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h265_aac51.ts"))
	rasl := 0
	lastKeyCRA := false
	seenKey := false
	for _, f := range byTrack(frames, 0) {
		if f.Key {
			seenKey = true
			lastKeyCRA = false
			for _, n := range f.Data {
				if (n[0]>>1)&0x3f == 21 {
					lastKeyCRA = true
				}
			}
			assert.False(t, f.RASL)
			continue
		}
		if f.RASL {
			rasl++
			assert.True(t, seenKey && lastKeyCRA, "RASL frame must follow a CRA keyframe")
		}
	}
	assert.GreaterOrEqual(t, rasl, 1)
}

func TestDemuxWrap(t *testing.T) {
	data := fixture(t, "h264_aacmono_wrap.ts")
	_, frames := demuxAll(t, data)
	for tr := 0; tr < 2; tr++ {
		fs := byTrack(frames, tr)
		require.NotEmpty(t, fs)
		for i := 1; i < len(fs); i++ {
			assert.Greater(t, fs[i].DTS, fs[i-1].DTS)
		}
	}
	v := byTrack(frames, 0)
	span := v[len(v)-1].DTS - v[0].DTS
	assert.Greater(t, span, int64(5.8*90000))
	assert.Less(t, span, int64(6*90000))

	md := mpegts.NewDemux(bytes.NewReader(data))
	video := map[uint16]bool{}
	for {
		ev, err := md.Next()
		require.NoError(t, err)
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				video[s.PID] = s.Type == typeH264
			}
		}
		if ev.PES != nil && video[ev.PES.PID] {
			assert.Greater(t, ev.PES.PTS, int64(1<<33-10*90000))
			break
		}
	}
}

func TestDemuxLoop(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac_loop.ts"))
	for tr := 0; tr < 2; tr++ {
		fs := byTrack(frames, tr)
		back := 0
		for i := 1; i < len(fs); i++ {
			if fs[i].DTS < fs[i-1].DTS {
				back++
			}
		}
		assert.Equal(t, 1, back, "track %d", tr)
	}
}

func TestDemuxConfigChange(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac_then_640.ts"))
	var cfgs []*media.TrackConfig
	for _, f := range byTrack(frames, 0) {
		if len(cfgs) == 0 || cfgs[len(cfgs)-1] != f.Config {
			cfgs = append(cfgs, f.Config)
		}
	}
	require.Len(t, cfgs, 2)
	assert.Equal(t, 320, cfgs[0].Width)
	assert.Equal(t, 240, cfgs[0].Height)
	assert.Equal(t, 640, cfgs[1].Width)
	assert.Equal(t, 360, cfgs[1].Height)
}

func TestDemuxMidGOP(t *testing.T) {
	d, frames := demuxAll(t, fixture(t, "h264_aac_midgop.ts"))
	assert.Greater(t, d.Stats().DroppedNoConfig, uint64(0))
	firstVideo := -1
	for i, f := range frames {
		if f.Track == 0 {
			firstVideo = i
			assert.True(t, f.Key)
			break
		}
	}
	require.GreaterOrEqual(t, firstVideo, 1)
	assert.Equal(t, 1, frames[0].Track)
}

func TestDemuxSkipsUnsupported(t *testing.T) {
	d, _ := demuxAll(t, fixture(t, "h264_mp2.ts"))
	assert.Equal(t, 1, d.NumTracks())
	require.Len(t, d.Skipped(), 1)
	assert.Contains(t, d.Skipped()[0], "mpeg audio")
}

func TestDemuxTruncated(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac.ts")[:100077])
	assert.GreaterOrEqual(t, len(byTrack(frames, 0)), 40)
	assert.GreaterOrEqual(t, len(byTrack(frames, 1)), 70)
	for _, f := range frames {
		assert.NotEmpty(t, f.Data)
	}
	assert.Nil(t, checkRaw(frames))
}

func TestDemuxGarbage(t *testing.T) {
	b := make([]byte, 64<<10)
	rand.New(rand.NewSource(1)).Read(b)
	_, err := NewDemuxer(bytes.NewReader(b))
	assert.Error(t, err)
}

func TestDemuxEmpty(t *testing.T) {
	_, err := NewDemuxer(bytes.NewReader(nil))
	assert.Error(t, err)
	_, err = NewDemuxer(bytes.NewReader(fixture(t, "h264_aac.ts")[:100]))
	assert.Error(t, err)
}

func TestDemuxBadConfigDropsFrames(t *testing.T) {
	_, frames := demuxAll(t, fixture(t, "h264_aac.ts"))
	cfg := frames[0].Config
	require.Equal(t, media.CodecH264, cfg.Codec)
	sc := []byte{0, 0, 0, 1}
	au := func(sps []byte) []byte {
		var b []byte
		for _, n := range [][]byte{sps, cfg.PPS[0], {0x65, 0x88, 0x84, 0x00}} {
			b = append(b, sc...)
			b = append(b, n...)
		}
		return b
	}
	tr := &track{
		codec: media.CodecH264, kind: media.KindVideo,
		vps: map[uint32][]byte{}, sps: map[uint32][]byte{}, pps: map[uint32][]byte{},
	}
	d := &Demuxer{tracks: []*track{tr}, byPID: map[uint16]*track{}}

	d.handleVideo(tr, au(cfg.SPS[0]), 0, 0)
	require.Len(t, d.queue, 1)
	good := d.queue[0].Config
	require.NotNil(t, good)

	// Truncated SPS: its ID cannot be read, so the config is invalidated.
	d.handleVideo(tr, au(cfg.SPS[0][:5]), 3600, 3600)
	assert.Len(t, d.queue, 1, "frame with unparsable SPS must not be emitted")
	assert.Equal(t, uint64(1), d.stats.DroppedNoConfig)

	// A repaired SPS restores output with the same config pointer as before,
	// so the Clock sees no configuration change.
	d.handleVideo(tr, au(cfg.SPS[0]), 7200, 7200)
	require.Len(t, d.queue, 2)
	assert.Same(t, good, d.queue[1].Config)
}

func dtsOf(frames []media.Frame) []int64 {
	out := make([]int64, len(frames))
	for i, f := range frames {
		out[i] = f.DTS
	}
	return out
}

func TestDemuxSharedDecoder(t *testing.T) {
	data := fixture(t, "h264_aac.ts")
	_, whole := demuxAll(t, data)

	// Each cut is a PAT packet directly preceding a keyframe PES.
	cuts := []int{0, 265, 538, 831, len(data) / mpegts.PacketSize}
	var dec mpegts.TimeDecoder
	var parts []media.Frame
	for i := 0; i+1 < len(cuts); i++ {
		seg := data[cuts[i]*mpegts.PacketSize : cuts[i+1]*mpegts.PacketSize]
		_, fs := demuxAllOpts(t, seg, DemuxOptions{Decoder: &dec})
		parts = append(parts, fs...)
	}
	for tr := 0; tr < 2; tr++ {
		require.NotEmpty(t, byTrack(whole, tr))
		assert.Equal(t, dtsOf(byTrack(whole, tr)), dtsOf(byTrack(parts, tr)), "track %d", tr)
	}
}

func TestDemuxNonSeekableReader(t *testing.T) {
	data := fixture(t, "h264_aac.ts")
	_, want := demuxAll(t, data)
	d, err := NewDemuxer(iotest.OneByteReader(bytes.NewReader(data)))
	require.NoError(t, err)
	got := readAll(t, d)
	assert.Equal(t, want, got)
}

const (
	videoPID = 256
	audioPID = 257
)

// fixturePES returns a fixture's PES packets in stream order.
func fixturePES(t *testing.T, name string) []*mpegts.PES {
	t.Helper()
	d := mpegts.NewDemux(bytes.NewReader(fixture(t, name)))
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

// tsBuilder writes a test stream with mpegts.Writer.
type tsBuilder struct {
	t   *testing.T
	buf bytes.Buffer
	w   *mpegts.Writer
}

func newTSBuilder(t *testing.T) *tsBuilder {
	b := &tsBuilder{t: t}
	b.w = mpegts.NewWriter(&b.buf)
	return b
}

// program sets the program, with PCR on the first stream, and writes the
// tables.
func (b *tsBuilder) program(streams ...mpegts.Stream) {
	b.t.Helper()
	require.NoError(b.t, b.w.SetProgram(mpegts.PMT{PCRPID: streams[0].PID, Streams: streams}))
	b.tables()
}

func (b *tsBuilder) tables() {
	b.t.Helper()
	require.NoError(b.t, b.w.WriteTables())
}

func (b *tsBuilder) pes(p *mpegts.PES) {
	b.t.Helper()
	require.NoError(b.t, b.w.WritePES(p.PID, p.Data, mpegts.PESOptions{
		PTS: p.PTS, DTS: p.DTS, HasDTS: p.DTS != p.PTS,
		RandomAccess: p.RandomAccess, PCR: -1, Video: p.PID == videoPID,
	}))
}

// adtsFrames counts the ADTS frames in audio PES.
func adtsFrames(t *testing.T, pes []*mpegts.PES) int {
	t.Helper()
	n := 0
	for _, p := range pes {
		fs, err := mpegts.ParseADTS(p.Data)
		require.NoError(t, err)
		n += len(fs)
	}
	return n
}

var (
	h264Stream = mpegts.Stream{PID: videoPID, Type: typeH264}
	aacStream  = mpegts.Stream{PID: audioPID, Type: typeAAC}
)

// addTrackStream is h264_aac.ts video-only under a video-only program for
// the first 30 video PES, then a program that adds the audio, then both.
func addTrackStream(t *testing.T) []byte {
	pes := fixturePES(t, "h264_aac.ts")
	b := newTSBuilder(t)
	b.program(h264Stream)
	nv, i := 0, 0
	for ; nv < 30; i++ {
		if pes[i].PID == videoPID {
			b.pes(pes[i])
			nv++
		}
	}
	b.program(h264Stream, aacStream)
	for _, p := range pes[i:] {
		b.pes(p)
	}
	return b.buf.Bytes()
}

func TestDemuxLaterPMTAddsTrack(t *testing.T) {
	d, frames := demuxAllOpts(t, addTrackStream(t), DemuxOptions{FollowPMT: true})
	assert.Equal(t, 2, d.NumTracks())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	assert.Len(t, byTrack(frames, 0), 150)
	a := byTrack(frames, 1)
	require.NotEmpty(t, a)
	for _, f := range a {
		assert.Equal(t, media.CodecAAC, f.Config.Codec)
	}
	assert.Len(t, frames, 150+len(a))
	assert.Empty(t, d.Skipped())
}

func TestDemuxLaterPMTIgnored(t *testing.T) {
	d, frames := demuxAll(t, addTrackStream(t))
	assert.Equal(t, 1, d.NumTracks())
	assert.Equal(t, []media.Kind{media.KindVideo}, d.TrackKinds())
	assert.Len(t, frames, 150)
	assert.Len(t, byTrack(frames, 0), 150)
}

// writeUntilVideo writes pes in order from index i until n video PES have
// been written, and returns the next index.
func writeUntilVideo(b *tsBuilder, pes []*mpegts.PES, i, n int) int {
	for nv := 0; nv < n; i++ {
		b.pes(pes[i])
		if pes[i].PID == videoPID {
			nv++
		}
	}
	return i
}

func TestDemuxLaterPMTChangesCodec(t *testing.T) {
	h264 := fixturePES(t, "h264_aac.ts")
	_, ref := demuxAll(t, fixture(t, "h264_aac.ts"))
	var h265Video []*mpegts.PES
	for _, p := range fixturePES(t, "h265_aac51.ts") {
		if p.PID == videoPID {
			h265Video = append(h265Video, p)
		}
	}
	b := newTSBuilder(t)
	b.program(h264Stream, aacStream)
	i := writeUntilVideo(b, h264, 0, 30)
	cut := b.buf.Len()
	b.program(mpegts.Stream{PID: videoPID, Type: typeH265}, aacStream)
	var audioAfter []*mpegts.PES
	for _, p := range h264[i:] {
		if p.PID == audioPID {
			audioAfter = append(audioAfter, p)
			b.pes(p)
		}
	}
	for _, p := range h265Video[:20] {
		b.pes(p)
	}
	data := b.buf.Bytes()

	before, _ := demuxAllOpts(t, data[:cut], DemuxOptions{FollowPMT: true})
	d, frames := demuxAllOpts(t, data, DemuxOptions{FollowPMT: true})
	assert.Equal(t, 2, d.NumTracks())
	v := byTrack(frames, 0)
	require.Len(t, v, 30)
	for k, f := range v {
		assert.Equal(t, media.CodecH264, f.Config.Codec)
		assert.Equal(t, byTrack(ref, 0)[k].Data, f.Data)
	}
	assert.Len(t, frames, 30+len(byTrack(frames, 1)))
	// Audio keeps flowing after the change.
	assert.GreaterOrEqual(t, len(byTrack(frames, 1)), adtsFrames(t, audioAfter))
	assert.Equal(t, before.Stats().DecodeErrors, d.Stats().DecodeErrors)
	assert.Empty(t, d.Skipped())
}

func TestDemuxLaterPMTUnsupportedCodec(t *testing.T) {
	pes := fixturePES(t, "h264_aac.ts")
	mp2 := mpegts.Stream{PID: videoPID, Type: 0x03}
	b := newTSBuilder(t)
	b.program(h264Stream, aacStream)
	i := writeUntilVideo(b, pes, 0, 30)
	b.program(aacStream, mp2)
	b.tables()
	b.program(aacStream, mpegts.Stream{PID: videoPID, Type: 0x04})
	// The PID returns as H.264, but a stopped PID is never re-added.
	b.program(h264Stream, aacStream)
	writeUntilVideo(b, pes, i, 30)

	d, frames := demuxAllOpts(t, b.buf.Bytes(), DemuxOptions{FollowPMT: true})
	assert.Equal(t, 2, d.NumTracks())
	assert.Len(t, byTrack(frames, 0), 30)
	assert.NotEmpty(t, byTrack(frames, 1))
	require.Len(t, d.Skipped(), 1)
	assert.Contains(t, d.Skipped()[0], "pid 256")
	assert.Contains(t, d.Skipped()[0], "mpeg audio")
}

func TestDemuxRepeatedPMT(t *testing.T) {
	pes := fixturePES(t, "h264_aac.ts")
	mp2 := mpegts.Stream{PID: 258, Type: 0x03}
	b := newTSBuilder(t)
	var audio []*mpegts.PES
	phase := func(withAudio bool, from, videos int) int {
		if withAudio {
			b.program(h264Stream, aacStream, mp2)
		} else {
			b.program(h264Stream, mp2)
		}
		nv := 0
		for ; nv < videos && from < len(pes); from++ {
			p := pes[from]
			b.tables() // The same PMT before every PES.
			switch {
			case p.PID == videoPID:
				b.pes(p)
				nv++
			case withAudio:
				b.pes(p)
				audio = append(audio, p)
			}
		}
		return from
	}
	i := phase(false, 0, 30)
	i = phase(true, i, 40)
	i = phase(false, i, 40)
	phase(true, i, 40)

	d, frames := demuxAllOpts(t, b.buf.Bytes(), DemuxOptions{FollowPMT: true})
	assert.Equal(t, 2, d.NumTracks())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, d.TrackKinds())
	assert.Len(t, byTrack(frames, 0), 150)
	assert.Len(t, byTrack(frames, 1), adtsFrames(t, audio))
	assert.Len(t, frames, 150+adtsFrames(t, audio))
	require.Len(t, d.Skipped(), 1)
	assert.Contains(t, d.Skipped()[0], "pid 258")
}

var errBoom = errors.New("boom")

// failingReader returns data and then errBoom.
type failingReader struct{ data []byte }

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errBoom
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestDemuxReadError(t *testing.T) {
	data := fixture(t, "h264_aac.ts")
	_, err := NewDemuxer(&failingReader{data: data[:mpegts.PacketSize]})
	require.ErrorIs(t, err, errBoom)
	assert.Contains(t, err.Error(), "ts: demux:")

	d, err := NewDemuxer(&failingReader{data: data[:100*mpegts.PacketSize]})
	require.NoError(t, err)
	var frames []media.Frame
	for {
		f, rerr := d.Read()
		if rerr != nil {
			err = rerr
			break
		}
		frames = append(frames, f)
	}
	require.ErrorIs(t, err, errBoom)
	assert.Contains(t, err.Error(), "ts: demux:")
	assert.NotEmpty(t, frames)
	_, err = d.Read()
	require.ErrorIs(t, err, errBoom, "the error repeats")
}

func TestDemuxNoSupportedTrack(t *testing.T) {
	b := newTSBuilder(t)
	b.program(mpegts.Stream{PID: audioPID, Type: 0x03})
	_, err := NewDemuxer(bytes.NewReader(b.buf.Bytes()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported track in PMT")
}
