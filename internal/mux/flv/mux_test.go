package flv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// demux returns every frame of a fixture in delivery order.
func demux(t *testing.T, name string) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixture(t, name)))
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

// fromKey drops the frames before the first video keyframe, as a fresh hub
// subscription does.
func fromKey(frames []media.Frame) []media.Frame {
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			return append([]media.Frame(nil), frames[i:]...)
		}
	}
	return nil
}

func audioOf(frames []media.Frame) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Kind() == media.KindAudio {
			out = append(out, f)
		}
	}
	return out
}

func shift(frames []media.Frame, by int64) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		out[i].PTS += by
		out[i].DTS += by
	}
	return out
}

func withEpoch(frames []media.Frame, ep uint32) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		out[i].Epoch = ep
	}
	return out
}

func maxDTS(frames []media.Frame) int64 {
	m := frames[0].DTS
	for _, f := range frames {
		m = max(m, f.DTS)
	}
	return m
}

// mergeByDTS merges two decode-ordered lists by DTS, a first on ties.
func mergeByDTS(a, b []media.Frame) []media.Frame {
	out := make([]media.Frame, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		if a[0].DTS <= b[0].DTS {
			out, a = append(out, a[0]), a[1:]
		} else {
			out, b = append(out, b[0]), b[1:]
		}
	}
	return append(append(out, a...), b...)
}

func feed(t *testing.T, m *Muxer, frames []media.Frame) []rtmp.Media {
	t.Helper()
	var out []rtmp.Media
	for _, f := range frames {
		msgs, err := m.Frame(f)
		require.NoError(t, err)
		out = append(out, msgs...)
	}
	return out
}

// msg is one parsed RTMP message.
type msg struct {
	typ   uint8
	ts    uint32
	video rtmp.VideoTag
	audio rtmp.AudioTag
	meta  map[string]any
	raw   []byte
}

func (p msg) header() bool {
	return p.typ == rtmp.MsgVideo && p.video.Kind == rtmp.VideoSequenceStart ||
		p.typ == rtmp.MsgAudio && p.audio.SequenceHeader
}

func (p msg) coded() bool {
	return p.typ == rtmp.MsgVideo && p.video.Kind == rtmp.VideoCodedFrames ||
		p.typ == rtmp.MsgAudio && !p.audio.SequenceHeader
}

func parse(t *testing.T, msgs []rtmp.Media) []msg {
	t.Helper()
	out := make([]msg, 0, len(msgs))
	for _, m := range msgs {
		p := msg{typ: m.Type, ts: m.Timestamp, raw: m.Payload}
		var err error
		switch m.Type {
		case rtmp.MsgVideo:
			p.video, err = rtmp.ParseVideoTag(m.Payload)
			require.NoError(t, err)
			require.True(t, p.video.Kind == rtmp.VideoSequenceStart || p.video.Kind == rtmp.VideoCodedFrames)
		case rtmp.MsgAudio:
			p.audio, err = rtmp.ParseAudioTag(m.Payload)
			require.NoError(t, err)
		case rtmp.MsgDataAMF0:
			vals, err := rtmp.DecodeAMF0(m.Payload)
			require.NoError(t, err)
			require.Len(t, vals, 2)
			require.Equal(t, "onMetaData", vals[0])
			arr, ok := vals[1].(rtmp.ECMAArray)
			require.True(t, ok, "onMetaData carries an ECMA array")
			p.meta = map[string]any{}
			for _, prop := range arr {
				p.meta[prop.Key] = prop.Value
			}
		default:
			t.Fatalf("unexpected message type %d", m.Type)
		}
		out = append(out, p)
	}
	return out
}

func lengthPrefixed(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		require.GreaterOrEqual(t, len(b), 4)
		n := int(binary.BigEndian.Uint32(b))
		require.LessOrEqual(t, n, len(b)-4)
		out = append(out, b[4:4+n])
		b = b[4+n:]
	}
	return out
}

func paramSets(cfg *media.TrackConfig) [][]byte {
	var out [][]byte
	out = append(out, cfg.VPS...)
	out = append(out, cfg.SPS...)
	return append(out, cfg.PPS...)
}

func record(t *testing.T, cfg *media.TrackConfig) []byte {
	t.Helper()
	var (
		rec []byte
		err error
	)
	switch cfg.Codec {
	case media.CodecH264:
		rec, err = AVCRecord(cfg)
	case media.CodecH265:
		rec, err = HEVCRecord(cfg)
	case media.CodecAAC:
		rec = cfg.ASC
	default:
		t.Fatalf("codec %v", cfg.Codec)
	}
	require.NoError(t, err)
	return rec
}

// checkMux checks a whole connection: frames start at a video keyframe; the
// messages are one onMetaData, then for each track its header just before its
// first frame, then every frame not dropped as early audio, with exact
// timestamps, composition times and payloads.
func checkMux(t *testing.T, frames []media.Frame, msgs []rtmp.Media) {
	t.Helper()
	require.Equal(t, media.KindVideo, frames[0].Kind())
	require.True(t, frames[0].Key)
	b, keyPTS := frames[0].DTS, frames[0].PTS
	var want []media.Frame
	for _, f := range frames {
		if f.Kind() == media.KindAudio && f.PTS < keyPTS {
			continue
		}
		want = append(want, f)
	}
	ps := parse(t, msgs)
	require.NotEmpty(t, ps)
	require.Equal(t, uint8(rtmp.MsgDataAMF0), ps[0].typ)
	require.Equal(t, uint32(0), ps[0].ts)
	headed := map[uint8]bool{}
	i := 0
	for k, p := range ps[1:] {
		require.NotEqual(t, uint8(rtmp.MsgDataAMF0), p.typ, "one onMetaData only")
		if p.header() {
			require.False(t, headed[p.typ], "one header per track")
			headed[p.typ] = true
			require.Less(t, k+1, len(ps)-1)
			require.True(t, ps[k+2].coded() && ps[k+2].typ == p.typ, "header just before the track's first frame")
			continue
		}
		require.True(t, headed[p.typ], "frame before its header")
		require.Less(t, i, len(want))
		f := want[i]
		i++
		require.Equal(t, uint32((f.DTS-b)/90), p.ts, "frame %d", i)
		if f.Kind() == media.KindVideo {
			require.Equal(t, uint8(rtmp.MsgVideo), p.typ)
			require.Equal(t, f.Key, p.video.Key)
			require.Equal(t, int32((f.PTS-b)/90-(f.DTS-b)/90), p.video.CompTime)
			nalus := f.Data
			if f.Key {
				nalus = append(paramSets(f.Config), f.Data...)
			}
			require.Equal(t, nalus, lengthPrefixed(t, p.video.Data))
		} else {
			require.Equal(t, uint8(rtmp.MsgAudio), p.typ)
			require.Equal(t, bytes.Join(f.Data, nil), p.audio.Data)
		}
	}
	require.Equal(t, len(want), i, "every frame sent")
	for _, p := range ps {
		if p.header() && p.typ == rtmp.MsgVideo {
			require.Equal(t, record(t, frames[0].Config), p.video.Data)
		}
	}
}

func firstHeader(ps []msg, typ uint8) (msg, bool) {
	for _, p := range ps {
		if p.typ == typ && p.header() {
			return p, true
		}
	}
	return msg{}, false
}

func TestMuxH264AAC(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	m := NewMuxer()
	msgs := feed(t, m, frames)
	checkMux(t, frames, msgs)
	ps := parse(t, msgs)
	v, ok := firstHeader(ps, rtmp.MsgVideo)
	require.True(t, ok)
	require.Equal(t, rtmp.VideoAVC, v.video.Codec)
	require.True(t, v.video.Key)
	require.Equal(t, byte(0x17), v.raw[0], "legacy AVC key")
	a, ok := firstHeader(ps, rtmp.MsgAudio)
	require.True(t, ok)
	require.Equal(t, audioOf(frames)[0].Config.ASC, a.audio.Data)
	require.Equal(t, uint64(0), m.Stats().SkippedTracks)
	require.Positive(t, m.Stats().DroppedEarlyAudio, "the fixture's audio before the keyframe")
}

func TestMuxBFrames(t *testing.T) {
	frames := fromKey(demux(t, "h264_pyramid.ts"))
	msgs := feed(t, NewMuxer(), frames)
	checkMux(t, frames, msgs)
	cts := map[int32]bool{}
	for _, p := range parse(t, msgs) {
		if p.typ == rtmp.MsgVideo && p.coded() {
			require.GreaterOrEqual(t, p.video.CompTime, int32(0))
			cts[p.video.CompTime] = true
		}
	}
	require.Greater(t, len(cts), 2, "b-pyramid has several composition times")
}

func TestMuxH265(t *testing.T) {
	frames := fromKey(demux(t, "h265_aac51.ts"))
	msgs := feed(t, NewMuxer(), frames)
	checkMux(t, frames, msgs)
	ps := parse(t, msgs)
	v, ok := firstHeader(ps, rtmp.MsgVideo)
	require.True(t, ok)
	require.Equal(t, rtmp.VideoHEVC, v.video.Codec)
	require.Equal(t, byte(0x80|1<<4), v.raw[0], "enhanced key SequenceStart")
	require.Equal(t, []byte("hvc1"), v.raw[1:5])
	var x, coded int
	for _, p := range ps {
		if p.typ != rtmp.MsgVideo || !p.coded() {
			continue
		}
		require.Equal(t, []byte("hvc1"), p.raw[1:5])
		if p.video.CompTime == 0 {
			require.Equal(t, byte(3), p.raw[0]&0x0f, "CodedFramesX")
			x++
		} else {
			require.Equal(t, byte(1), p.raw[0]&0x0f, "CodedFrames")
			coded++
		}
	}
	require.Positive(t, x)
	require.Positive(t, coded)
}

// twoEpochs returns h264_aac.ts as epoch 1 and, as epoch 2, h264_640.ts's
// video (640x360) with h264_aac.ts's audio, continuing after epoch 1.
func twoEpochs(t *testing.T) (ep1, ep2 []media.Frame) {
	t.Helper()
	ep1 = withEpoch(fromKey(demux(t, "h264_aac.ts")), 1)
	v := fromKey(demux(t, "h264_640.ts"))
	d := maxDTS(ep1) + 3600 - v[0].DTS
	v = shift(v, d)
	a := audioOf(demux(t, "h264_aac.ts"))
	var aligned []media.Frame
	for _, f := range shift(a, d) {
		if f.PTS >= v[0].PTS && f.DTS <= maxDTS(v) {
			aligned = append(aligned, f)
		}
	}
	return ep1, withEpoch(mergeByDTS(v, aligned), 2)
}

func TestMuxEpochResendsHeaders(t *testing.T) {
	ep1, ep2 := twoEpochs(t)
	m := NewMuxer()
	out1 := parse(t, feed(t, m, ep1))
	out2 := parse(t, feed(t, m, ep2))
	require.Equal(t, uint64(1), m.Stats().Epochs)

	count := func(ps []msg) (n int) {
		for _, p := range ps {
			if p.typ == rtmp.MsgDataAMF0 {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, count(out1))
	require.Equal(t, 1, count(out2))
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out2[0].typ)
	require.InDelta(t, 640, out2[0].meta["width"], 0)
	require.InDelta(t, 360, out2[0].meta["height"], 0)

	b := ep1[0].DTS
	for _, typ := range []uint8{rtmp.MsgVideo, rtmp.MsgAudio} {
		first := -1
		for i, p := range out2 {
			if p.typ == typ {
				first = i
				break
			}
		}
		require.GreaterOrEqual(t, first, 0)
		require.True(t, out2[first].header(), "type %d: header first", typ)
		require.True(t, out2[first+1].coded() && out2[first+1].typ == typ)
		for _, p := range out2[first+1:] {
			require.False(t, p.typ == typ && p.header(), "one header per track")
		}
	}
	v, _ := firstHeader(out2, rtmp.MsgVideo)
	require.Equal(t, record(t, ep2[0].Config), v.video.Data)
	require.Equal(t, uint32((ep2[0].DTS-b)/90), out2[0].ts, "metadata at the epoch's first frame")
	for _, p := range out2 {
		if p.typ == rtmp.MsgVideo && p.coded() {
			require.True(t, p.video.Key, "epoch starts at a keyframe")
			break
		}
	}
}

func TestMuxCodecChanged(t *testing.T) {
	ep1 := withEpoch(fromKey(demux(t, "h264_aac.ts")), 1)
	h265 := fromKey(demux(t, "h265_aac51.ts"))
	ep2 := withEpoch(shift(h265, maxDTS(ep1)+3600-h265[0].DTS), 2)
	m := NewMuxer()
	feed(t, m, ep1)
	_, err := m.Frame(ep2[0])
	require.ErrorIs(t, err, ErrCodecChanged)
}

func TestMuxResyncAfterGap(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	m := NewMuxer()
	feed(t, m, frames[:100])
	early := m.Stats().DroppedEarlyAudio
	m.Resync()
	rest := frames[110:]
	out := parse(t, feed(t, m, rest))
	require.Equal(t, uint64(1), m.Stats().Resyncs)
	require.Positive(t, m.Stats().DroppedBeforeKey)
	require.Greater(t, m.Stats().DroppedEarlyAudio, early, "audio below the resume keyframe")

	var key media.Frame
	for _, f := range rest {
		if f.Kind() == media.KindVideo && f.Key {
			key = f
			break
		}
	}
	b := frames[0].DTS
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	sawVideo := false
	headed := map[uint8]bool{}
	for _, p := range out[1:] {
		require.NotEqual(t, uint8(rtmp.MsgDataAMF0), p.typ)
		if p.header() {
			headed[p.typ] = true
			continue
		}
		require.True(t, headed[p.typ], "header before frames after a resync")
		if p.typ == rtmp.MsgVideo && !sawVideo {
			sawVideo = true
			require.True(t, p.video.Key, "no delta after the gap")
			require.Equal(t, uint32((key.DTS-b)/90), p.ts)
		}
		if p.typ == rtmp.MsgAudio {
			require.GreaterOrEqual(t, p.ts, uint32((key.PTS-b)/90), "early audio dropped")
		}
	}
	require.True(t, sawVideo)
}

func TestMuxStartDropsEarlyAudio(t *testing.T) {
	src := demux(t, "h264_aac.ts")
	key := fromKey(src)[0]
	a := audioOf(src)
	early, late := a[0], a[1]
	early.PTS, early.DTS = key.DTS-900, key.DTS-900
	late.PTS, late.DTS = key.PTS, key.PTS
	m := NewMuxer()
	out := parse(t, feed(t, m, []media.Frame{key, early, late}))
	require.Equal(t, uint64(1), m.Stats().DroppedEarlyAudio)
	var audio []msg
	for _, p := range out {
		require.Less(t, p.ts, uint32(1<<24), "no negative timestamp")
		if p.typ == rtmp.MsgAudio && p.coded() {
			audio = append(audio, p)
		}
	}
	require.Len(t, audio, 1)
	require.Equal(t, uint32((key.PTS-key.DTS)/90), audio[0].ts)
}

func TestMuxStaggeredConfigChange(t *testing.T) {
	ep1 := withEpoch(fromKey(demux(t, "h264_aac.ts")), 1)
	ep2 := withEpoch(shift(ep1, maxDTS(ep1)+3600-ep1[0].DTS), 2)
	asc, err := media.NewAACConfig([]byte{0x12, 0x10})
	require.NoError(t, err)
	for i := range ep2 {
		if ep2[i].Kind() == media.KindAudio {
			ep2[i].Config = asc
		}
	}
	require.Equal(t, media.KindVideo, ep2[1].Kind(), "video leads the epoch")
	m := NewMuxer()
	feed(t, m, ep1)
	out := parse(t, feed(t, m, ep2))
	firstAudio := -1
	for i, p := range out {
		if p.typ == rtmp.MsgAudio {
			firstAudio = i
			break
		}
	}
	require.Greater(t, firstAudio, 3, "audio's first frame comes after video frames")
	require.True(t, out[firstAudio].header())
	require.Equal(t, asc.ASC, out[firstAudio].audio.Data)
	require.True(t, out[firstAudio+1].coded())
}

func TestMuxTrackAppears(t *testing.T) {
	v := fromKey(demux(t, "h264_640.ts"))
	a := audioOf(demux(t, "h264_aac.ts"))
	audio := a[len(a)-1]
	audio.Track = 1
	audio.PTS, audio.DTS = maxDTS(v)+90, maxDTS(v)+90

	m := NewMuxer()
	out := parse(t, feed(t, m, v))
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	require.NotContains(t, out[0].meta, "audiocodecid")
	_, err := m.Frame(audio)
	require.ErrorIs(t, err, ErrCodecChanged)

	m = NewMuxer()
	feed(t, m, withEpoch(v, 1))
	audio.Epoch = 2
	_, err = m.Frame(audio)
	require.ErrorIs(t, err, ErrCodecChanged, "audio appearing at a new epoch")
}

func TestMuxAudioOnly(t *testing.T) {
	src := demux(t, "h264_aac.ts")
	a := audioOf(src)
	m := NewMuxer()
	out := parse(t, feed(t, m, a[:10]))
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	require.NotContains(t, out[0].meta, "videocodecid")
	require.InDelta(t, 10, out[0].meta["audiocodecid"], 0)
	require.True(t, out[1].header())
	require.Equal(t, uint32(0), out[2].ts)
	require.Len(t, out, 12)

	m.Resync()
	out = parse(t, feed(t, m, a[20:21]))
	require.Len(t, out, 3, "metadata, header, frame after a resync")
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	require.True(t, out[1].header())
	require.Equal(t, uint32((a[20].DTS-a[0].DTS)/90), out[2].ts)

	key := fromKey(src)[0]
	key.PTS, key.DTS = a[21].PTS, a[21].DTS
	_, err := m.Frame(key)
	require.ErrorIs(t, err, ErrCodecChanged)
}

func TestMuxRebasePerConnection(t *testing.T) {
	frames := shift(fromKey(demux(t, "h264_aac.ts")), 5*3600*media.ClockRate)
	msgs := feed(t, NewMuxer(), frames)
	checkMux(t, frames, msgs)
	require.Equal(t, uint32(0), msgs[0].Timestamp)
	for _, p := range parse(t, msgs) {
		require.Less(t, p.ts, uint32(1<<24))
		if p.typ == rtmp.MsgVideo && p.coded() {
			require.Equal(t, uint32(0), p.ts, "first timestamp 0")
			break
		}
	}
}

func TestMuxTimestampWrap(t *testing.T) {
	v := fromKey(demux(t, "h264_640.ts"))
	key, delta := v[0], v[1]
	key.PTS, key.DTS = 0, 0
	frames := make([]media.Frame, 0, 9)
	frames = append(frames, key)
	start := int64(1<<32-100) * 90
	for i := range 8 {
		f := delta
		f.DTS = start + int64(i)*3000
		f.PTS = f.DTS
		frames = append(frames, f)
	}
	var prev uint32
	wrapped := false
	for i, p := range parse(t, feed(t, NewMuxer(), frames)) {
		if p.typ != rtmp.MsgVideo || !p.coded() {
			continue
		}
		if i > 0 && prev != 0 {
			d := p.ts - prev
			require.True(t, d == 33 || d == 34, "monotonic mod 2^32: %d -> %d", prev, p.ts)
			wrapped = wrapped || p.ts < prev
		}
		prev = p.ts
	}
	require.True(t, wrapped)
}

func TestMuxDropsBeforeKeyframe(t *testing.T) {
	src := demux(t, "h264_aac.ts")
	midGOP := src[1:]
	m := NewMuxer()
	got := feed(t, m, midGOP)
	want := feed(t, NewMuxer(), fromKey(midGOP))
	require.Equal(t, want, got)
	require.Positive(t, m.Stats().DroppedBeforeKey)
	require.Equal(t, rtmp.MsgVideo, int(got[1].Type))
	checkMux(t, fromKey(midGOP), got)
}

func TestMuxExtraTracksSkipped(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	doubled := make([]media.Frame, 0, 2*len(frames))
	for _, f := range frames {
		extra := f
		extra.Track += 2
		doubled = append(doubled, f, extra)
	}
	m := NewMuxer()
	got := feed(t, m, doubled)
	require.Equal(t, feed(t, NewMuxer(), frames), got)
	require.Equal(t, uint64(2), m.Stats().SkippedTracks)
}

func TestMuxMetadataCodecIDs(t *testing.T) {
	meta := func(frames []media.Frame) map[string]any {
		t.Helper()
		ps := parse(t, feed(t, NewMuxer(), frames))
		require.Equal(t, uint8(rtmp.MsgDataAMF0), ps[0].typ)
		return ps[0].meta
	}
	h264 := fromKey(demux(t, "h264_aac.ts"))
	md := meta(h264)
	require.InDelta(t, 7, md["videocodecid"], 0)
	require.InDelta(t, 10, md["audiocodecid"], 0)
	require.InDelta(t, h264[0].Config.Width, md["width"], 0)
	require.InDelta(t, h264[0].Config.Height, md["height"], 0)
	require.InDelta(t, 48000, md["audiosamplerate"], 0)
	require.InDelta(t, 2, md["audiochannels"], 0)
	require.Equal(t, true, md["stereo"])

	md = meta(fromKey(demux(t, "h265_aac51.ts")))
	require.InDelta(t, 1752589105, md["videocodecid"], 0)
	require.InDelta(t, 10, md["audiocodecid"], 0)
	require.InDelta(t, 6, md["audiochannels"], 0)

	md = meta(fromKey(demux(t, "h264_640.ts")))
	require.InDelta(t, 7, md["videocodecid"], 0)
	for _, k := range []string{"audiocodecid", "audiosamplerate", "audiochannels", "stereo"} {
		require.NotContains(t, md, k)
	}

	md = meta(audioOf(demux(t, "h264_aac.ts")))
	for _, k := range []string{"videocodecid", "width", "height", "framerate"} {
		require.NotContains(t, md, k)
	}
}

func TestMuxRecordFailureSkipsTrack(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	bad := &media.TrackConfig{Codec: media.CodecH264, SPS: [][]byte{{0x67, 0x00}}, PPS: [][]byte{{0x68}}}
	noVideo := append([]media.Frame(nil), frames...)
	for i := range noVideo {
		if noVideo[i].Kind() == media.KindVideo {
			noVideo[i].Config = bad
		}
	}
	m := NewMuxer()
	ps := parse(t, feed(t, m, noVideo))
	require.Equal(t, uint64(1), m.Stats().SkippedTracks)
	require.Equal(t, uint8(rtmp.MsgDataAMF0), ps[0].typ)
	require.NotContains(t, ps[0].meta, "videocodecid")
	require.Contains(t, ps[0].meta, "audiocodecid")
	require.Equal(t, uint32(0), ps[2].ts, "audio-only connection based on its first audio frame")
	for _, p := range ps {
		require.NotEqual(t, uint8(rtmp.MsgVideo), p.typ)
	}

	badAudio := &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 48000, Channels: 2}
	noAudio := append([]media.Frame(nil), frames...)
	for i := range noAudio {
		if noAudio[i].Kind() == media.KindAudio {
			noAudio[i].Config = badAudio
		}
	}
	m = NewMuxer()
	ps = parse(t, feed(t, m, noAudio))
	require.Equal(t, uint64(1), m.Stats().SkippedTracks)
	require.NotContains(t, ps[0].meta, "audiocodecid")
	require.Contains(t, ps[0].meta, "videocodecid")
	for _, p := range ps {
		require.NotEqual(t, uint8(rtmp.MsgAudio), p.typ)
	}
}

func TestMuxEpochStartsAtKeyframe(t *testing.T) {
	ep1, ep2 := twoEpochs(t)
	m := NewMuxer()
	feed(t, m, ep1)
	before := m.Stats().DroppedBeforeKey
	out := parse(t, feed(t, m, ep2[1:]))
	require.Greater(t, m.Stats().DroppedBeforeKey, before)
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	for _, p := range out {
		if p.typ == rtmp.MsgVideo && p.coded() {
			require.True(t, p.video.Key, "no delta frame at an epoch start")
			return
		}
	}
	t.Fatal("no video in the epoch")
}

// audioRun returns n audio frames copied from src[0], 1024 samples at 48 kHz
// apart, starting at DTS from.
func audioRun(src []media.Frame, from int64, n int) []media.Frame {
	out := make([]media.Frame, n)
	for i := range out {
		f := src[0]
		f.DTS = from + int64(i)*1920
		f.PTS = f.DTS
		out[i] = f
	}
	return out
}

// A connection carrying video whose input lost its video (failover to an
// audio-only source) resumes on audio once no video keyframe came within
// hub.LiveWindow of a Resync, and a later video keyframe resumes video with
// its header.
func TestMuxResyncAfterVideoEnds(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	m := NewMuxer()
	feed(t, m, frames)
	m.Resync()
	from := maxDTS(frames) + 3600
	audio := audioRun(audioOf(frames), from, 600)
	out := make([]msg, 0, len(audio)+2)
	firstOut := -1
	for i, f := range audio {
		msgs, err := m.Frame(f)
		require.NoError(t, err)
		if len(msgs) > 0 && firstOut < 0 {
			firstOut = i
		}
		out = append(out, parse(t, msgs)...)
	}
	require.GreaterOrEqual(t, firstOut, 0, "audio resumes")
	require.GreaterOrEqual(t, audio[firstOut].DTS-from, int64(hub.LiveWindow), "not before the live window")
	require.Less(t, audio[firstOut].DTS-from, int64(hub.LiveWindow)+1920)
	require.Equal(t, dropsBefore(t, frames)+uint64(firstOut), m.Stats().DroppedBeforeKey)
	b := frames[0].DTS
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	require.True(t, out[1].header() && out[1].typ == rtmp.MsgAudio)
	require.Equal(t, uint32((audio[firstOut].DTS-b)/90), out[2].ts)
	require.Len(t, out, 2+len(audio)-firstOut)

	// Video comes back: a delta frame is dropped, a keyframe resumes video
	// with its header.
	last := audio[len(audio)-1].DTS
	src := fromKey(demux(t, "h264_aac.ts"))
	key, delta := src[0], src[1]
	require.False(t, delta.Key)
	delta.DTS, delta.PTS = last+100, last+100
	key.DTS, key.PTS = last+3100, last+3100
	before := m.Stats().DroppedBeforeKey
	require.Empty(t, feed(t, m, []media.Frame{delta}))
	require.Equal(t, before+1, m.Stats().DroppedBeforeKey)
	vid := parse(t, feed(t, m, []media.Frame{key}))
	require.Len(t, vid, 2)
	require.True(t, vid[0].header() && vid[0].typ == rtmp.MsgVideo)
	require.True(t, vid[1].coded() && vid[1].video.Key)
}

// dropsBefore returns the DroppedBeforeKey count of a fresh muxer fed frames.
func dropsBefore(t *testing.T, frames []media.Frame) uint64 {
	t.Helper()
	m := NewMuxer()
	feed(t, m, frames)
	return m.Stats().DroppedBeforeKey
}

// A Resync during the start hold discards the held frames: the connection
// starts again, based on the next keyframe.
func TestMuxResyncDuringHold(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	m := NewMuxer()
	require.Empty(t, feed(t, m, frames[:1]), "held")
	m.Resync()
	require.Equal(t, uint64(1), m.Stats().Resyncs)
	next := 0
	for i := 1; i < len(frames); i++ {
		if frames[i].Kind() == media.KindVideo && frames[i].Key {
			next = i
			break
		}
	}
	require.Positive(t, next, "the fixture has a second keyframe")
	msgs := feed(t, m, frames[1:])
	require.Equal(t, uint64(next-1), m.Stats().DroppedBeforeKey, "frames before the next keyframe dropped")
	checkMux(t, frames[next:], msgs)
}

// The start hold of a video-only connection ends at 256 held frames even
// within a second of decode time.
func TestMuxHoldFrameCap(t *testing.T) {
	v := fromKey(demux(t, "h264_640.ts"))
	key, delta := v[0], v[1]
	require.False(t, delta.Key)
	frames := []media.Frame{key}
	for i := 1; i <= holdFrames; i++ {
		f := delta
		f.DTS, f.PTS = key.DTS+int64(i), key.DTS+int64(i)
		frames = append(frames, f)
	}
	m := NewMuxer()
	for _, f := range frames[:holdFrames] {
		msgs, err := m.Frame(f)
		require.NoError(t, err)
		require.Empty(t, msgs, "held")
	}
	out := parse(t, feed(t, m, frames[holdFrames:]))
	require.Len(t, out, 2+holdFrames+1, "metadata, header and every frame")
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	require.True(t, out[1].header())
}

// A composition time outside the signed 24-bit range is an error, not a
// truncated value.
func TestMuxCompositionTimeRange(t *testing.T) {
	src := demux(t, "h264_aac.ts")
	key := fromKey(src)[0]
	key.PTS = key.DTS + (1<<23)*90
	a := audioOf(src)[0]
	a.DTS, a.PTS = key.DTS+90, key.PTS
	m := NewMuxer()
	require.Empty(t, feed(t, m, []media.Frame{key}))
	_, err := m.Frame(a)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCodecChanged)
	require.Contains(t, err.Error(), "composition time")

	key.PTS = key.DTS + (1<<23-1)*90
	a.PTS = key.PTS
	out := parse(t, feed(t, NewMuxer(), []media.Frame{key, a}))
	require.Equal(t, int32(1<<23-1), out[2].video.CompTime)
}

// A new epoch whose first frame is audio gets onMetaData and the audio header
// at that frame; video resumes at the epoch's first keyframe, with its
// header.
func TestMuxEpochAudioFirst(t *testing.T) {
	ep1, ep2 := twoEpochs(t)
	var first int
	for i, f := range ep2 {
		if f.Kind() == media.KindAudio {
			first = i
			break
		}
	}
	a := ep2[first]
	a.DTS, a.PTS = ep2[0].DTS-1920, ep2[0].DTS-1920
	m := NewMuxer()
	feed(t, m, ep1)
	out := parse(t, feed(t, m, append([]media.Frame{a}, ep2...)))
	require.Equal(t, uint64(1), m.Stats().Epochs)
	require.Equal(t, uint8(rtmp.MsgDataAMF0), out[0].typ)
	require.True(t, out[1].header() && out[1].typ == rtmp.MsgAudio)
	require.True(t, out[2].coded() && out[2].typ == rtmp.MsgAudio)
	require.Equal(t, uint32((a.DTS-ep1[0].DTS)/90), out[0].ts)
	headers := map[uint8]int{}
	sawVideo := false
	for i, p := range out {
		require.False(t, i > 0 && p.typ == rtmp.MsgDataAMF0, "one onMetaData")
		if p.header() {
			headers[p.typ]++
			continue
		}
		if p.typ == rtmp.MsgVideo && !sawVideo {
			sawVideo = true
			require.True(t, p.video.Key)
			require.True(t, out[i-1].header() && out[i-1].typ == rtmp.MsgVideo, "video header just before")
		}
	}
	require.True(t, sawVideo)
	require.Equal(t, map[uint8]int{rtmp.MsgAudio: 1, rtmp.MsgVideo: 1}, headers)
}
