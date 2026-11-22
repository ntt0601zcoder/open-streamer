package rtpmux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// Bases near the 32-bit wrap, so every session's timestamps wrap.
var tsBases = []uint32{0xFFFF0000, 0xFFFFFF00}

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return b
}

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

func ofKind(frames []media.Frame, k media.Kind) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Kind() == k {
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

// configs returns the first config of every track, in track order, as the
// hub lists them.
func configs(frames []media.Frame) []hub.TrackConfig {
	var out []hub.TrackConfig
	for _, f := range frames {
		if f.Config == nil || slices.ContainsFunc(out, func(c hub.TrackConfig) bool { return c.Track == f.Track }) {
			continue
		}
		out = append(out, hub.TrackConfig{Track: f.Track, Kind: f.Kind(), Config: f.Config})
	}
	slices.SortFunc(out, func(a, b hub.TrackConfig) int { return a.Track - b.Track })
	return out
}

// tracksFor returns the session tracks of the frames' SDP, all SETUP.
func tracksFor(t *testing.T, frames []media.Frame) []Track {
	t.Helper()
	descs, err := Tracks(configs(frames))
	require.NoError(t, err)
	out := make([]Track, len(descs))
	for i, d := range descs {
		out[i] = Track{
			SDPIndex: i, HubTrack: d.ID, Desc: d, Setup: true,
			SSRC: 0x1000 + uint32(i), TSBase: tsBases[i],
		}
	}
	return out
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// result is what a session produced, per SDP index.
type result struct {
	frames  map[int][]*Packets // one entry per frame that produced packets
	reports []Report
}

func (r result) packets(idx int) []*rtp.Packet {
	var out []*rtp.Packet
	for _, p := range r.frames[idx] {
		out = append(out, p.RTP...)
	}
	return out
}

func feed(t *testing.T, m *Muxer, frames []media.Frame) result {
	t.Helper()
	r := result{frames: map[int][]*Packets{}}
	for i, f := range frames {
		p, reps, err := m.Frame(f)
		require.NoError(t, err, "frame %d", i)
		if p != nil {
			require.NotEmpty(t, p.RTP)
			r.frames[p.SDPIndex] = append(r.frames[p.SDPIndex], p)
		}
		r.reports = append(r.reports, reps...)
	}
	return r
}

func media4(tr Track) rtsp.Media {
	return rtsp.Media{
		Index: tr.SDPIndex, Codec: tr.Desc.Codec, Payload: tr.Desc.Payload,
		ClockRate: tr.Desc.ClockRate, ParamSets: tr.Desc.ParamSets, AAC: tr.Desc.AAC,
	}
}

// depack runs a track's packets through our depacketiser.
func depack(t *testing.T, tr Track, pkts []*rtp.Packet) []rtsp.AU {
	t.Helper()
	d, err := rtsp.NewDepacketizer(media4(tr), 0)
	require.NoError(t, err)
	var out []rtsp.AU
	for _, p := range pkts {
		out = append(out, d.Push(p, time.Time{})...)
	}
	return append(out, d.Flush()...)
}

func paramSets(cfg *media.TrackConfig) [][]byte {
	var out [][]byte
	out = append(out, cfg.VPS...)
	out = append(out, cfg.SPS...)
	return append(out, cfg.PPS...)
}

// roundTicks converts a·rate 90 kHz ticks to the rate clock, rounding to
// the nearest integer, halves away from zero.
func roundTicks(a int64) int64 {
	const b = media.ClockRate
	if a < 0 {
		return -((-a + b/2) / b)
	}
	return (a + b/2) / b
}

// want is one expected access unit.
type want struct {
	ts    uint32
	nalus [][]byte
}

// wantVideo returns the AUs a session starting at base b sends for the video
// frames of hub track tr.HubTrack: parameter sets before every keyframe,
// timestamps TSBase + (PTS − b).
func wantVideo(frames []media.Frame, tr Track, b int64) []want {
	var out []want
	for _, f := range frames {
		if f.Track != tr.HubTrack || f.Kind() != media.KindVideo {
			continue
		}
		var n [][]byte
		if f.Key {
			n = append(n, paramSets(f.Config)...)
		}
		out = append(out, want{ts: tr.TSBase + uint32(f.PTS-b), nalus: append(n, f.Data...)})
	}
	return out
}

// wantAudio returns the AUs for the audio frames of tr.HubTrack with PTS at
// or above keyPTS (one epoch): anchored rounding from the first one, and a
// frame whose anchored value is within one sample of the previous plus 1024
// is stamped exactly 1024 after it; otherwise it re-anchors on its PTS.
func wantAudio(frames []media.Frame, tr Track, b, keyPTS int64) []want {
	var out []want
	rate := int64(tr.Desc.ClockRate)
	var a0, off0, last int64
	first := true
	for _, f := range frames {
		if f.Track != tr.HubTrack || f.Kind() != media.KindAudio || f.PTS < keyPTS {
			continue
		}
		off := int64(0)
		if !first {
			off = off0 + roundTicks((f.PTS-a0)*rate)
		}
		if next := last + 1024; !first && off >= next-1 && off <= next+1 {
			off = next
		} else {
			a0, off0, first = f.PTS, roundTicks((f.PTS-b)*rate), false
			off = off0
		}
		last = off
		out = append(out, want{ts: tr.TSBase + uint32(off), nalus: f.Data})
	}
	return out
}

func checkAUs(t *testing.T, tr Track, got []rtsp.AU, w []want) {
	t.Helper()
	require.Len(t, got, len(w), "track %d AUs", tr.SDPIndex)
	for i := range w {
		require.Equal(t, w[i].ts, got[i].Timestamp, "track %d AU %d timestamp", tr.SDPIndex, i)
		data := got[i].NALUs
		if tr.Desc.Codec == rtsp.CodecAAC {
			data = got[i].Frames
		}
		require.Equal(t, w[i].nalus, data, "track %d AU %d data", tr.SDPIndex, i)
		require.False(t, got[i].Lost)
	}
}

// checkPackets checks the header fields the muxer owns and the payload
// bound: the marker on the last packet of each frame only, the payload type,
// and payloads within the MTU.
func checkPackets(t *testing.T, tr Track, frames []*Packets, mtu int) {
	t.Helper()
	for i, p := range frames {
		require.Equal(t, tr.SDPIndex, p.SDPIndex)
		for j, pkt := range p.RTP {
			require.Equal(t, tr.Desc.Payload, pkt.PayloadType, "frame %d packet %d", i, j)
			require.Equal(t, j == len(p.RTP)-1, pkt.Marker, "frame %d packet %d marker", i, j)
			require.Equal(t, p.RTP[0].Timestamp, pkt.Timestamp, "one timestamp per frame")
			require.LessOrEqual(t, len(pkt.Payload), mtu)
		}
	}
}

func TestMuxH264AAC(t *testing.T) {
	const mtu = 300 // small: exercises FU-A and AAC fragmentation
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	require.Len(t, tracks, 2)
	require.Equal(t, rtsp.CodecH264, tracks[0].Desc.Codec)
	require.Equal(t, rtsp.CodecAAC, tracks[1].Desc.Codec)

	m := NewMuxer(tracks, mtu, (&fakeClock{t: t0}).now)
	r := feed(t, m, frames)
	b, keyPTS := frames[0].DTS, frames[0].PTS
	checkAUs(t, tracks[0], depack(t, tracks[0], r.packets(0)), wantVideo(frames, tracks[0], b))
	audio := wantAudio(frames, tracks[1], b, keyPTS)
	checkAUs(t, tracks[1], depack(t, tracks[1], r.packets(1)), audio)
	checkPackets(t, tracks[0], r.frames[0], mtu)
	checkPackets(t, tracks[1], r.frames[1], mtu)
	require.Len(t, r.frames[1], len(audio), "one AAC frame per Packets")

	var pkts, octets uint64
	for _, idx := range []int{0, 1} {
		for _, p := range r.packets(idx) {
			pkts++
			octets += uint64(len(p.Payload))
		}
	}
	st := m.Stats()
	assert.Equal(t, pkts, st.Packets)
	assert.Equal(t, octets, st.Octets)
	assert.Equal(t, uint64(len(ofKind(frames, media.KindAudio))-len(audio)), st.DroppedEarlyAudio)
	assert.Zero(t, st.IgnoredFrames)
	assert.Zero(t, st.DroppedBeforeKey)
}

func TestMuxBFrames(t *testing.T) {
	frames := fromKey(demux(t, "h264_pyramid.ts"))
	tracks := tracksFor(t, frames)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	r := feed(t, m, frames)
	b := frames[0].DTS
	checkAUs(t, tracks[0], depack(t, tracks[0], r.packets(0)), wantVideo(frames, tracks[0], b))

	backwards := 0
	vf := r.frames[0]
	for i := 1; i < len(vf); i++ {
		if int32(vf[i].RTP[0].Timestamp-vf[i-1].RTP[0].Timestamp) < 0 {
			backwards++
		}
	}
	assert.Positive(t, backwards, "B-frames: RTP timestamps follow PTS, not decode order")
}

func TestMuxH265(t *testing.T) {
	frames := fromKey(demux(t, "h265_aac51.ts"))
	tracks := tracksFor(t, frames)
	require.Equal(t, rtsp.CodecH265, tracks[0].Desc.Codec)
	require.Equal(t, uint8(97), tracks[0].Desc.Payload)
	require.Equal(t, 6, tracks[1].Desc.Channels)
	require.Equal(t, uint32(44100), tracks[1].Desc.ClockRate)

	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	r := feed(t, m, frames)
	b := frames[0].DTS
	got := depack(t, tracks[0], r.packets(0))
	checkAUs(t, tracks[0], got, wantVideo(frames, tracks[0], b))
	require.Len(t, paramSets(frames[0].Config), 3, "VPS, SPS, PPS")
	checkAUs(t, tracks[1], depack(t, tracks[1], r.packets(1)), wantAudio(frames, tracks[1], b, frames[0].PTS))
	checkPackets(t, tracks[0], r.frames[0], DefaultMTU)
}

// A second epoch at a new resolution sends its own parameter sets in-band
// before its first keyframe, with timestamps continuing from the first.
func TestMuxEpochNewParamSets(t *testing.T) {
	ep1 := withEpoch(fromKey(demux(t, "h264_aac.ts")), 1)
	next := fromKey(demux(t, "h264_aac_640.ts"))
	ep2 := withEpoch(shift(next, maxDTS(ep1)+3600-next[0].DTS), 2)
	require.False(t, ep1[0].Config.Equal(ep2[0].Config), "new parameter sets")

	tracks := tracksFor(t, ep1)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	all := append(append([]media.Frame(nil), ep1...), ep2...)
	r := feed(t, m, all)
	b := ep1[0].DTS
	got := depack(t, tracks[0], r.packets(0))
	checkAUs(t, tracks[0], got, wantVideo(all, tracks[0], b))
	first2 := len(ofKind(ep1, media.KindVideo))
	assert.Equal(t, paramSets(ep2[0].Config), got[first2].NALUs[:2], "the new SPS and PPS first")
	assert.Equal(t, tracks[0].TSBase+uint32(ep2[0].PTS-b), got[first2].Timestamp)

	// Audio: the anchor restarts at the epoch's first audio frame, so its
	// frames step by exactly 1024 from there, continuing the timeline.
	aud := depack(t, tracks[1], r.packets(1))
	a2 := ofKind(ep2, media.KindAudio)
	i2 := len(aud) - len(a2)
	off := roundTicks((a2[0].PTS - b) * 48000)
	assert.Equal(t, tracks[1].TSBase+uint32(off), aud[i2].Timestamp)
	for i := i2 + 1; i < len(aud); i++ {
		require.Equal(t, uint32(1024), aud[i].Timestamp-aud[i-1].Timestamp)
	}
}

// After Resync nothing is sent until the next video keyframe, which carries
// the parameter sets; audio presented before it is dropped; timestamps keep
// the session base.
func TestMuxResyncAfterLoss(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	b := frames[0].DTS

	feed(t, m, frames[:60])
	m.Resync()
	rest := frames[80:] // 20 frames lost
	require.False(t, rest[0].Kind() == media.KindVideo && rest[0].Key, "the gap ends mid-GOP")
	r := feed(t, m, rest)

	k := slices.IndexFunc(rest, func(f media.Frame) bool { return f.Kind() == media.KindVideo && f.Key })
	require.Positive(t, k)
	key := rest[k]
	got := depack(t, tracks[0], r.packets(0))
	checkAUs(t, tracks[0], got, wantVideo(rest[k:], tracks[0], b))
	assert.Equal(t, paramSets(key.Config), got[0].NALUs[:2], "parameter sets before the resume keyframe")

	early := 0
	for _, f := range rest[k:] {
		if f.Kind() == media.KindAudio && f.PTS < key.PTS {
			early++
		}
	}
	require.Positive(t, early, "the fixture has audio presented before the resume keyframe")
	aud := depack(t, tracks[1], r.packets(1))
	require.NotEmpty(t, aud)
	for _, a := range aud {
		require.GreaterOrEqual(t, int64(a.Timestamp-tracks[1].TSBase)*media.ClockRate/48000, key.PTS-b-1,
			"no audio before the resume keyframe")
	}

	st := m.Stats()
	assert.Equal(t, uint64(1), st.Resyncs)
	assert.Equal(t, uint64(k), st.DroppedBeforeKey)
	assert.GreaterOrEqual(t, st.DroppedEarlyAudio, uint64(early))
}

func TestMuxCodecChanged(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	feed(t, m, frames[:30])

	h265 := fromKey(demux(t, "h265_aac51.ts"))[0]
	h265.Track = tracks[0].HubTrack
	h265.DTS, h265.PTS = maxDTS(frames[:30])+3600, maxDTS(frames[:30])+3600
	h265.Epoch = 1
	p, _, err := m.Frame(h265)
	require.ErrorIs(t, err, ErrCodecChanged)
	assert.Nil(t, p)
}

// RFC 3640's config= and the rtpmap clock exist only in the SDP: a new ASC,
// sampling rate or channel count ends the session.
func TestMuxAACConfigChangeEndsSession(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	require.Equal(t, uint32(48000), tracks[1].Desc.ClockRate)
	require.Equal(t, 2, tracks[1].Desc.Channels)

	for name, asc := range map[string][]byte{
		"48 kHz 5.1":      {0x11, 0xB0},
		"44.1 kHz stereo": {0x12, 0x10},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := media.NewAACConfig(asc)
			require.NoError(t, err)
			m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
			feed(t, m, frames[:30])
			a := ofKind(frames, media.KindAudio)[20]
			a.Config = cfg
			_, _, err = m.Frame(a)
			require.ErrorIs(t, err, ErrCodecChanged)
		})
	}

	// An equal ASC in a new config pointer is not a change.
	same, err := media.NewAACConfig(tracks[1].Desc.AAC.Config)
	require.NoError(t, err)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	feed(t, m, frames[:30])
	a := ofKind(frames, media.KindAudio)[20]
	a.Config = same
	_, _, err = m.Frame(a)
	require.NoError(t, err)
}

// halfTickShift returns the shift in [0, 200) ticks that puts
// (from + shift − to)·rate/90000 closest to a half RTP tick, where plain
// rounding of jittered 90 kHz PTS flips between neighbours.
func halfTickShift(from, to, rate int64) int64 {
	best, bestDist := int64(0), 1.0
	for d := range int64(200) {
		x := float64(from+d-to) * float64(rate) / media.ClockRate
		frac := x - float64(int64(x))
		if dist := abs(frac - 0.5); dist < bestDist {
			best, bestDist = d, dist
		}
	}
	return best
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func shiftAudio(frames []media.Frame, by int64) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		if out[i].Kind() == media.KindAudio {
			out[i].PTS += by
			out[i].DTS += by
		}
	}
	return out
}

// At 44.1 kHz the 90 kHz PTS of consecutive AAC frames differ by 2089 or
// 2090 ticks; anchored rounding still steps the RTP timestamp by exactly
// 1024 within each epoch, even when the audio's phase against the base, or
// against the previous epoch, sits on a half tick.
func TestMuxAudioRoundingAnchored(t *testing.T) {
	src := fromKey(demux(t, "h265_aac51.ts"))
	b := src[0].DTS
	firstAudio := func(fs []media.Frame) int64 {
		for _, f := range fs {
			if f.Kind() == media.KindAudio && f.PTS >= src[0].PTS {
				return f.PTS
			}
		}
		t.Fatal("no audio")
		return 0
	}
	ep1 := withEpoch(shiftAudio(src, halfTickShift(firstAudio(src), b, 44100)), 1)
	a1 := firstAudio(ep1)
	// The second epoch is the whole clip again, early audio included, so it
	// anchors on the clip's first audio frame: its PTS are off the sample
	// grid by up to about a tick against that frame, which plain anchored
	// rounding turns into a 1025 step at its 16th frame.
	next := shift(src, maxDTS(ep1)+3600-src[0].DTS)
	ep2 := withEpoch(shiftAudio(next, halfTickShift(firstAudio(next), a1, 44100)), 2)
	all := append(append([]media.Frame(nil), ep1...), ep2...)

	steps := map[int64]bool{}
	af := ofKind(ep1, media.KindAudio)
	for i := 1; i < len(af); i++ {
		steps[af[i].PTS-af[i-1].PTS] = true
	}
	require.Greater(t, len(steps), 1, "the PTS steps are not uniform")

	tracks := tracksFor(t, all)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	r := feed(t, m, all)
	pf := r.frames[1]
	n1 := len(wantAudio(ep1, tracks[1], b, ep1[0].PTS))
	require.Greater(t, n1, 100)
	require.Len(t, pf, n1+len(ofKind(ep2, media.KindAudio)))
	for i := 1; i < len(pf); i++ {
		if i == n1 {
			// The second epoch re-anchors on its first frame.
			a2 := ofKind(ep2, media.KindAudio)[0].PTS
			require.Equal(t, tracks[1].TSBase+uint32(roundTicks((a2-b)*44100)), pf[i].RTP[0].Timestamp)
			continue
		}
		require.Equal(t, uint32(1024), pf[i].RTP[0].Timestamp-pf[i-1].RTP[0].Timestamp, "frame %d", i)
	}

	t.Run("audio only, anchored on the first frame", func(t *testing.T) {
		audio := ofKind(demux(t, "h265_aac51.ts"), media.KindAudio)
		tracks := tracksFor(t, audio)
		r := feed(t, NewMuxer(tracks, 0, (&fakeClock{t: t0}).now), audio)
		pf := r.frames[0]
		require.Len(t, pf, len(audio))
		for i := 1; i < len(pf); i++ {
			require.Equal(t, uint32(1024), pf[i].RTP[0].Timestamp-pf[i-1].RTP[0].Timestamp, "frame %d", i)
		}
	})

	// A new epoch re-anchors even when it lands within one sample of the
	// next AU's slot.
	t.Run("epoch within a sample", func(t *testing.T) {
		audio := ofKind(demux(t, "h265_aac51.ts"), media.KindAudio)
		ep1 := withEpoch(audio[:100], 1)
		tracks := tracksFor(t, ep1)
		m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
		r := feed(t, m, ep1)
		b := ep1[0].DTS
		last := int64(r.frames[0][99].RTP[0].Timestamp - tracks[0].TSBase)
		lastPTS := ep1[99].PTS
		// The PTS step that puts the epoch's anchored value one sample past
		// the next AU's slot.
		d := int64(-1)
		for i := int64(1900); i < 2300 && d < 0; i++ {
			if roundTicks((lastPTS+i-b)*44100) == last+1025 {
				d = i
			}
		}
		require.Positive(t, d)
		ep2 := withEpoch(shift(audio[100:110], lastPTS+d-audio[100].PTS), 2)
		r = feed(t, m, ep2)
		assert.Equal(t, tracks[0].TSBase+uint32(last+1025), r.frames[0][0].RTP[0].Timestamp,
			"re-anchored on the new epoch's PTS, not snapped to the next slot")
	})

	// A gap of several AUs is not snapped: the frame after it re-anchors on
	// its PTS, and the steps are exact again from there.
	t.Run("gap", func(t *testing.T) {
		audio := ofKind(demux(t, "h265_aac51.ts"), media.KindAudio)
		const at, lost = 50, 5
		fed := append(append([]media.Frame(nil), audio[:at]...), audio[at+lost:]...)
		tracks := tracksFor(t, fed)
		r := feed(t, NewMuxer(tracks, 0, (&fakeClock{t: t0}).now), fed)
		pf := r.frames[0]
		require.Len(t, pf, len(fed))
		b := fed[0].DTS
		want := tracks[0].TSBase + uint32(roundTicks((fed[at].PTS-b)*44100))
		assert.Equal(t, want, pf[at].RTP[0].Timestamp, "re-anchored on the PTS after the gap")
		step := pf[at].RTP[0].Timestamp - pf[at-1].RTP[0].Timestamp
		assert.InDelta(t, (lost+1)*1024, step, 2)
		for i := 1; i < len(pf); i++ {
			if i != at {
				require.Equal(t, uint32(1024), pf[i].RTP[0].Timestamp-pf[i-1].RTP[0].Timestamp, "frame %d", i)
			}
		}
	})
}

// A listed track that starts sending after the session started is carried
// from its first frame, anchored to the session base, with its first sender
// report right after its first packet. A video track the SDP does not carry
// is ignored.
func TestMuxTrackAppears(t *testing.T) {
	all := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, all)
	b := all[0].DTS
	late := b + 2*media.ClockRate
	var frames []media.Frame
	for _, f := range all {
		if f.Kind() == media.KindAudio && f.DTS < late {
			continue
		}
		frames = append(frames, f)
		if f.Kind() == media.KindVideo && f.DTS >= late {
			extra := f
			extra.Track = 9
			frames = append(frames, extra)
		}
	}

	clk := &fakeClock{t: t0}
	m := NewMuxer(tracks, 0, clk.now)
	var firstAudio int
	r := result{frames: map[int][]*Packets{}}
	for i, f := range frames {
		p, reps, err := m.Frame(f)
		require.NoError(t, err)
		if p != nil {
			r.frames[p.SDPIndex] = append(r.frames[p.SDPIndex], p)
			if p.SDPIndex == 1 && len(r.frames[1]) == 1 {
				firstAudio = i
				require.True(t, slices.ContainsFunc(reps, func(rp Report) bool { return rp.SDPIndex == 1 }),
					"first audio SR with the first audio packet")
			}
		}
		if len(r.frames[1]) == 0 {
			require.False(t, slices.ContainsFunc(reps, func(rp Report) bool { return rp.SDPIndex == 1 }),
				"no audio SR before an audio packet")
		}
	}
	require.Positive(t, firstAudio)
	checkAUs(t, tracks[1], depack(t, tracks[1], r.packets(1)), wantAudio(frames, tracks[1], b, all[0].PTS))
	checkAUs(t, tracks[0], depack(t, tracks[0], r.packets(0)), wantVideo(frames, tracks[0], b))
	assert.Equal(t, uint64(len(ofKind(frames, media.KindVideo))-len(ofKind(all, media.KindVideo))),
		m.Stats().IgnoredFrames)
}

func TestMuxAudioOnly(t *testing.T) {
	frames := ofKind(demux(t, "h264_aac.ts"), media.KindAudio)
	tracks := tracksFor(t, frames)
	require.Len(t, tracks, 1)
	require.Equal(t, rtsp.CodecAAC, tracks[0].Desc.Codec)
	require.Equal(t, uint8(98), tracks[0].Desc.Payload)

	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	r := feed(t, m, frames[:100])
	b := frames[0].DTS
	checkAUs(t, tracks[0], depack(t, tracks[0], r.packets(0)), wantAudio(frames[:100], tracks[0], b, b))

	// Resync on an audio-only session resumes at the next audio frame.
	m.Resync()
	r = feed(t, m, frames[110:])
	got := depack(t, tracks[0], r.packets(0))
	require.Len(t, got, len(frames)-110)
	assert.Equal(t, tracks[0].TSBase+uint32(roundTicks((frames[110].PTS-b)*48000)), got[0].Timestamp)
	assert.Zero(t, m.Stats().DroppedBeforeKey)
}

func srCounts(rtcp []byte) (packets, octets uint32) {
	return binary.BigEndian.Uint32(rtcp[20:]), binary.BigEndian.Uint32(rtcp[24:])
}

// Every sender report maps its RTP timestamp to NTP through one session-wide
// mapping from stream time, so both tracks agree within one tick; the first
// SR of a track follows its first packet, then one every 5 s.
func TestMuxSenderReportsAligned(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	clk := &fakeClock{t: t0}
	m := NewMuxer(tracks, 0, clk.now)
	b := frames[0].DTS

	type srAt struct {
		at time.Time
		sr rtsp.SenderReport
	}
	srs := map[int][]srAt{}
	sent := map[int]int{}
	lastTS := map[int]uint32{}
	var pkts, octets [2]uint32
	check := func(rp Report) {
		tr := tracks[rp.SDPIndex]
		parsed := rtsp.ParseSenderReports(rp.RTCP)
		require.Len(t, parsed, 1)
		sr := parsed[0]
		require.Equal(t, tr.SSRC, sr.SSRC)
		require.Equal(t, lastTS[rp.SDPIndex], sr.RTP, "the SR carries the last packet's timestamp")
		p, o := srCounts(rp.RTCP)
		require.Equal(t, pkts[rp.SDPIndex], p)
		require.Equal(t, octets[rp.SDPIndex], o)
		// NTP − NTP₀ in the track's clock equals RTP − TSBase within a tick.
		rate := float64(tr.Desc.ClockRate)
		ntpTicks := sr.NTP.Sub(t0).Seconds() * rate
		rtpTicks := float64(sr.RTP - tr.TSBase)
		require.InDelta(t, rtpTicks, ntpTicks, 1, "track %d", rp.SDPIndex)
		srs[rp.SDPIndex] = append(srs[rp.SDPIndex], srAt{at: clk.t, sr: sr})
	}
	for _, f := range frames {
		clk.t = t0.Add(time.Duration(f.DTS-b) * time.Second / media.ClockRate)
		p, reps, err := m.Frame(f)
		require.NoError(t, err)
		if p != nil {
			first := sent[p.SDPIndex] == 0
			sent[p.SDPIndex]++
			for _, pkt := range p.RTP {
				pkts[p.SDPIndex]++
				octets[p.SDPIndex] += uint32(len(pkt.Payload))
			}
			lastTS[p.SDPIndex] = p.RTP[len(p.RTP)-1].Timestamp
			if first {
				require.True(t, slices.ContainsFunc(reps, func(rp Report) bool { return rp.SDPIndex == p.SDPIndex }),
					"SR right after track %d's first packet", p.SDPIndex)
			}
		}
		for _, rp := range reps {
			check(rp)
		}
	}
	// 6 s of stream: the first SR and one 5 s later, per track.
	for idx := range 2 {
		require.Len(t, srs[idx], 2, "track %d", idx)
		assert.GreaterOrEqual(t, srs[idx][1].at.Sub(srs[idx][0].at), 5*time.Second)
		assert.Less(t, srs[idx][1].at.Sub(srs[idx][0].at), 5*time.Second+100*time.Millisecond)
	}
	// The timer path: both tracks are due 5 s later, then neither.
	clk.t = clk.t.Add(5 * time.Second)
	reps := m.Reports(clk.t)
	require.Len(t, reps, 2)
	for _, rp := range reps {
		check(rp)
	}
	assert.Empty(t, m.Reports(clk.t))

	// Equal stream times map to equal NTP instants across the tracks: the
	// first SR pair's NTP difference equals its stream-time difference.
	v, a := srs[0][0].sr, srs[1][0].sr
	vt := float64(v.RTP-tracks[0].TSBase) / 90000
	at := float64(a.RTP-tracks[1].TSBase) / 48000
	assert.InDelta(t, at-vt, a.NTP.Sub(v.NTP).Seconds(), 1.0/48000)
}

func TestMuxDropsBeforeBase(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	feed(t, m, frames[:30])
	b := frames[0].DTS

	late := ofKind(frames, media.KindVideo)[3]
	late.DTS, late.PTS = b-3600, b+3600
	p, _, err := m.Frame(late)
	require.NoError(t, err)
	assert.Nil(t, p)
	assert.Equal(t, uint64(1), m.Stats().DroppedBeforeBase)

	// After a Resync, a keyframe below the base is not a resume point.
	m.Resync()
	key := frames[0]
	key.DTS, key.PTS = b-3600, b
	p, _, err = m.Frame(key)
	require.NoError(t, err)
	assert.Nil(t, p)
	assert.Equal(t, uint64(2), m.Stats().DroppedBeforeBase)
}

func TestMuxIgnoresUnsetupTracks(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	audio := ofKind(frames, media.KindAudio)

	t.Run("not SETUP", func(t *testing.T) {
		tracks := tracksFor(t, frames)
		tracks[1].Setup = false
		m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
		r := feed(t, m, frames)
		assert.Empty(t, r.frames[1])
		assert.NotEmpty(t, r.frames[0])
		for _, rp := range r.reports {
			assert.Equal(t, 0, rp.SDPIndex, "no SR for a track not SETUP")
		}
		assert.Equal(t, uint64(len(audio)), m.Stats().IgnoredFrames)
	})
	t.Run("second audio track", func(t *testing.T) {
		tracks := tracksFor(t, frames)
		m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
		feed(t, m, frames[:30])
		a := audio[20]
		a.Track = 7
		p, _, err := m.Frame(a)
		require.NoError(t, err)
		assert.Nil(t, p)
		assert.Equal(t, uint64(1), m.Stats().IgnoredFrames)
	})
	t.Run("audio absent from the SDP", func(t *testing.T) {
		video := ofKind(frames, media.KindVideo)
		tracks := tracksFor(t, video)
		require.Len(t, tracks, 1)
		m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
		feed(t, m, video[:10])
		_, _, err := m.Frame(audio[20])
		require.ErrorIs(t, err, ErrCodecChanged)
	})
}

func TestTracksFromConfigs(t *testing.T) {
	h264 := fromKey(demux(t, "h264_aac.ts"))
	h265 := fromKey(demux(t, "h265_aac51.ts"))
	v264 := ofKind(h264, media.KindVideo)[0].Config
	v265 := ofKind(h265, media.KindVideo)[0].Config
	a51 := ofKind(h265, media.KindAudio)[0].Config
	a2 := ofKind(h264, media.KindAudio)[0].Config

	got, err := Tracks([]hub.TrackConfig{
		{Track: 0, Kind: media.KindAudio, Config: &media.TrackConfig{Codec: media.CodecAAC}},  // no ASC: skipped
		{Track: 1, Kind: media.KindVideo, Config: &media.TrackConfig{Codec: media.CodecH264}}, // no SPS: skipped
		{Track: 2, Kind: media.KindVideo, Config: v265},
		{Track: 3, Kind: media.KindAudio, Config: a51},
		{Track: 4, Kind: media.KindVideo, Config: v264},
		{Track: 5, Kind: media.KindAudio, Config: a2},
		{Track: 6, Kind: 0, Config: nil},
	})
	require.NoError(t, err)
	require.Equal(t, []rtsp.TrackDesc{
		{
			Codec: rtsp.CodecH265, Payload: 97, ClockRate: 90000,
			ParamSets: paramSets(v265), ID: 2,
		},
		{
			Codec: rtsp.CodecAAC, Payload: 98, ClockRate: 44100, Channels: 6,
			AAC: &rtsp.AACFormat{Config: a51.ASC, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3},
			ID:  3,
		},
	}, got)

	// Audio listed before video in track order still comes second.
	got, err = Tracks([]hub.TrackConfig{
		{Track: 0, Kind: media.KindAudio, Config: a2},
		{Track: 1, Kind: media.KindVideo, Config: v264},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, rtsp.CodecH264, got[0].Codec)
	assert.Equal(t, uint8(96), got[0].Payload)
	assert.Equal(t, paramSets(v264), got[0].ParamSets)
	assert.Equal(t, 1, got[0].ID)
	assert.Equal(t, 0, got[1].ID)
	_, err = rtsp.BuildSDP("127.0.0.1", got)
	require.NoError(t, err, "the descriptors build an SDP")

	_, err = Tracks(nil)
	require.Error(t, err)
	_, err = Tracks([]hub.TrackConfig{{Track: 0, Kind: media.KindVideo, Config: &media.TrackConfig{Codec: media.CodecH264}}})
	require.Error(t, err)
}

// A new epoch that starts mid-GOP refers to pictures of the old source: its
// video waits for its first keyframe, audio goes on.
func TestMuxEpochWaitsForKey(t *testing.T) {
	ep1 := withEpoch(fromKey(demux(t, "h264_aac.ts")), 1)
	src := fromKey(demux(t, "h264_aac_640.ts"))
	cut := slices.IndexFunc(src, func(f media.Frame) bool { return f.Kind() == media.KindVideo && !f.Key })
	ep2 := withEpoch(shift(src[cut:], maxDTS(ep1)+3600-src[cut].DTS), 2)
	k := slices.IndexFunc(ep2, func(f media.Frame) bool { return f.Kind() == media.KindVideo && f.Key })
	require.Positive(t, k)

	tracks := tracksFor(t, ep1)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	feed(t, m, ep1)
	before := m.Stats()
	r := feed(t, m, ep2)
	got := depack(t, tracks[0], r.packets(0))
	checkAUs(t, tracks[0], got, wantVideo(ep2[k:], tracks[0], ep1[0].DTS))
	assert.Equal(t, uint64(len(ofKind(ep2[:k], media.KindVideo))), m.Stats().DroppedBeforeKey-before.DroppedBeforeKey)
	assert.Len(t, r.frames[1], len(ofKind(ep2, media.KindAudio)), "audio is not held")
}

// After a Resync on a session carrying video, when no keyframe comes within
// hub.LiveWindow of stream time the video track has ended: audio resumes,
// and video at its next keyframe.
func TestMuxVideoEndsAfterResync(t *testing.T) {
	frames := fromKey(demux(t, "h264_aac.ts"))
	tracks := tracksFor(t, frames)
	m := NewMuxer(tracks, 0, (&fakeClock{t: t0}).now)
	feed(t, m, frames)
	m.Resync()

	src := ofKind(frames, media.KindAudio)
	start := maxDTS(frames) + 1920
	audio := make([]media.Frame, 0, 3*len(src))
	for i := range 3 {
		audio = append(audio, shift(src, start+int64(i)*6*media.ClockRate-src[0].DTS)...)
	}
	r := feed(t, m, audio)
	resumed := slices.IndexFunc(audio, func(f media.Frame) bool { return f.DTS-start >= hub.LiveWindow })
	require.Len(t, r.frames[1], len(audio)-resumed, "audio resumes once the video track has ended")
	assert.Equal(t, uint64(resumed), m.Stats().DroppedBeforeKey)

	// Video comes back: deltas wait for its keyframe.
	v := ofKind(frames, media.KindVideo)
	back := shift(v[1:30], maxDTS(audio)+3600-v[1].DTS)
	k := slices.IndexFunc(back, func(f media.Frame) bool { return f.Key })
	require.Positive(t, k)
	r = feed(t, m, back)
	require.Len(t, r.frames[0], len(back)-k)
	assert.Equal(t, paramSets(back[k].Config), depack(t, tracks[0], r.packets(0))[0].NALUs[:2])
}
