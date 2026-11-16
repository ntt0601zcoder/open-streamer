package ts

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// Synthetic H.264 NAL units. The byte after the header starts the slice
// header: a leading 1 bit is first_mb_in_slice == 0.
var nalAUD = []byte{0x09, 0xF0}

// slice returns a slice NAL unit with the given header byte, first or not,
// and size filler bytes.
func slice(header, filler byte, first bool, size int) []byte {
	b := make([]byte, 2, 2+size)
	b[0], b[1] = header, 0x9A
	if !first {
		b[1] = 0x42 // first_mb_in_slice == 1
	}
	return append(b, bytes.Repeat([]byte{filler}, size)...)
}

func idrSlice(first bool, size int) []byte { return slice(0x65, 0xAA, first, size) }

func pSlice(first bool, size int) []byte { return slice(0x41, 0xAB, first, size) }

func joinNALUs(nalus ...[]byte) []byte {
	var b []byte
	for _, n := range nalus {
		b = append(b, 0, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

// h264Params returns the SPS and PPS of h264_aac.ts.
func h264Params(t *testing.T) (sps, pps []byte) {
	t.Helper()
	_, frames := demuxAll(t, fixture(t, "h264_aac.ts"))
	cfg := frames[0].Config
	return cfg.SPS[0], cfg.PPS[0]
}

// videoPES writes a video PES with PTS and DTS.
func (b *tsBuilder) videoPES(pid uint16, pts, dts int64, data []byte) {
	b.t.Helper()
	b.pes(&mpegts.PES{PID: pid, PTS: pts, DTS: dts, Data: data})
}

// pesNoPTS writes a PES with PTS_DTS_flags '00': the writer's PTS field is
// turned into header stuffing bytes.
func (b *tsBuilder) pesNoPTS(pid uint16, data []byte) {
	b.t.Helper()
	start := b.buf.Len()
	b.pes(&mpegts.PES{PID: pid, Data: data})
	pkt := b.buf.Bytes()[start : start+mpegts.PacketSize]
	off := 4
	if pkt[3]&0x20 != 0 {
		off += 1 + int(pkt[4])
	}
	pes := pkt[off:]
	require.Equal(b.t, []byte{0, 0, 1}, pes[:3])
	require.Equal(b.t, byte(5), pes[8])
	pes[7] &^= 0xC0
	copy(pes[9:14], bytes.Repeat([]byte{0xFF}, 5))
}

// cutSecondPacket removes the second packet written from byte offset start,
// a continuity gap inside a multi-packet PES.
func (b *tsBuilder) cutSecondPacket(start int) {
	b.t.Helper()
	all := b.buf.Bytes()
	at := start + mpegts.PacketSize
	require.LessOrEqual(b.t, at+mpegts.PacketSize, len(all))
	out := append(append([]byte(nil), all[:at]...), all[at+mpegts.PacketSize:]...)
	b.buf.Reset()
	b.buf.Write(out)
}

func ptsOf(frames []media.Frame) []int64 {
	out := make([]int64, len(frames))
	for i, f := range frames {
		out[i] = f.PTS
	}
	return out
}

func TestVideoPESWithoutPTSContinues(t *testing.T) {
	sps, pps := h264Params(t)
	s1, s2, s3 := idrSlice(true, 300), idrSlice(false, 300), idrSlice(false, 300)
	b := newTSBuilder(t)
	b.program(h264Stream)
	b.videoPES(videoPID, 90000, 90000, joinNALUs(nalAUD, sps, pps, s1))
	b.pesNoPTS(videoPID, joinNALUs(s2))
	b.pesNoPTS(videoPID, joinNALUs(s3))

	d, frames := demuxAll(t, b.buf.Bytes())
	require.Len(t, frames, 1)
	f := frames[0]
	assert.Equal(t, [][]byte{s1, s2, s3}, f.Data)
	assert.True(t, f.Key)
	// The timestamp decoder starts the timeline at 0.
	assert.Equal(t, int64(0), f.PTS)
	assert.Equal(t, int64(0), f.DTS)
	st := d.Stats()
	assert.Equal(t, uint64(2), st.Continued)
	assert.Zero(t, st.DecodeErrors)
	assert.Zero(t, st.LostFrames)
}

func TestVideoPESManyAUs(t *testing.T) {
	sps, pps := h264Params(t)
	t.Run("aud", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
		b.videoPES(videoPID, 3000, 3000, joinNALUs(nalAUD, pSlice(true, 50)))
		p1, p2, p3 := pSlice(true, 40), pSlice(true, 41), pSlice(true, 42)
		b.videoPES(videoPID, 6000, 6000, joinNALUs(nalAUD, p1, nalAUD, p2, nalAUD, p3))

		d, frames := demuxAll(t, b.buf.Bytes())
		require.Len(t, frames, 5)
		// The delta is the last PES-to-PES DTS step per access unit.
		assert.Equal(t, []int64{0, 3000, 6000, 9000, 12000}, dtsOf(frames))
		assert.Equal(t, dtsOf(frames), ptsOf(frames))
		assert.Equal(t, [][]byte{p1}, frames[2].Data)
		assert.Equal(t, [][]byte{p2}, frames[3].Data)
		assert.Equal(t, [][]byte{p3}, frames[4].Data)
		assert.Equal(t, uint64(2), d.Stats().SplitAUs)
		assert.Zero(t, d.Stats().SplitAUsReordered)
	})
	t.Run("first slices", func(t *testing.T) {
		i1, i2 := idrSlice(true, 40), idrSlice(false, 40)
		p1, p2a, p2b := pSlice(true, 41), pSlice(true, 42), pSlice(false, 43)
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(sps, pps, i1, i2, p1, p2a, p2b))

		d, frames := demuxAll(t, b.buf.Bytes())
		require.Len(t, frames, 3)
		// No history: the step is 3600.
		assert.Equal(t, []int64{0, 3600, 7200}, dtsOf(frames))
		assert.Equal(t, dtsOf(frames), ptsOf(frames))
		assert.Equal(t, [][]byte{i1, i2}, frames[0].Data)
		assert.True(t, frames[0].Key)
		assert.Equal(t, [][]byte{p1}, frames[1].Data)
		assert.False(t, frames[1].Key)
		assert.Equal(t, [][]byte{p2a, p2b}, frames[2].Data)
		assert.Equal(t, uint64(2), d.Stats().SplitAUs)
	})
	t.Run("reordered", func(t *testing.T) {
		i1 := idrSlice(true, 40)
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 7200, 3600, joinNALUs(nalAUD, sps, pps, i1, nalAUD, pSlice(true, 41), nalAUD, pSlice(true, 42)))

		d, frames := demuxAll(t, b.buf.Bytes())
		require.Len(t, frames, 1)
		assert.Equal(t, [][]byte{i1}, frames[0].Data)
		assert.Equal(t, int64(3600), frames[0].PTS-frames[0].DTS)
		// One PES: nothing to calibrate POC timing against.
		assert.Equal(t, uint64(2), d.Stats().SplitAUsReordered)
		assert.Zero(t, d.Stats().SplitAUs)
		assert.Zero(t, d.Stats().SplitAUsPOC)
	})
}

// videoFrames returns the video access units of a fixture as the demuxer
// emits them, in decode order.
func videoFrames(t *testing.T, name string) []media.Frame {
	t.Helper()
	_, frames := demuxAll(t, fixture(t, name))
	return byTrack(frames, 0)
}

// repackAUs writes access units per to a PES, without AUDs, with only the
// first unit's PTS and DTS in the PES header (offset to stay positive). The
// first PES also carries the parameter sets: frames hold none.
func repackAUs(t *testing.T, stype uint8, frames []media.Frame, per int) []byte {
	t.Helper()
	return repackAUsLate(t, stype, frames, per, 0)
}

// repackAUsLate is repackAUs with every PES DTS late by late ticks.
func repackAUsLate(t *testing.T, stype uint8, frames []media.Frame, per int, late int64) []byte {
	t.Helper()
	const off = 900000
	cfg := frames[0].Config
	b := newTSBuilder(t)
	b.program(mpegts.Stream{PID: videoPID, Type: stype})
	for i := 0; i < len(frames); i += per {
		var nalus [][]byte
		if i == 0 {
			nalus = slices.Concat(cfg.VPS, cfg.SPS, cfg.PPS)
		}
		for _, f := range frames[i:min(i+per, len(frames))] {
			nalus = append(nalus, f.Data...)
		}
		b.videoPES(videoPID, frames[i].PTS+off, frames[i].DTS+off+late, joinNALUs(nalus...))
	}
	return b.buf.Bytes()
}

// assertRecovered checks the frames demuxed from a repacked stream against
// the source units: every source unit but the dropped ones (by index) is
// emitted with the source's PTS, DTS, data and Key; the dropped ones count
// as SplitAUsReordered, and every split unit emitted was timed by POC.
func assertRecovered(t *testing.T, d *Demuxer, src, out []media.Frame, per int, dropped []int) {
	t.Helper()
	require.NotEmpty(t, out)
	// The timestamp decoder starts each timeline at its first PTS.
	shift := out[0].PTS - src[0].PTS
	byDTS := map[int64]int{}
	for i, f := range src {
		byDTS[f.DTS] = i
	}
	got := make([]int, 0, len(out))
	for k, f := range out {
		i, ok := byDTS[f.DTS-shift]
		require.True(t, ok, "frame %d: DTS %d is no source DTS", k, f.DTS-shift)
		assert.Equal(t, src[i].PTS, f.PTS-shift, "unit %d", i)
		assert.Equal(t, src[i].Data, f.Data, "unit %d", i)
		assert.Equal(t, src[i].Key, f.Key, "unit %d", i)
		got = append(got, i)
	}
	var want []int
	for i := range src {
		if !slices.Contains(dropped, i) {
			want = append(want, i)
		}
	}
	assert.Equal(t, want, got)
	st := d.Stats()
	groups := (len(src) + per - 1) / per
	assert.Equal(t, uint64(len(dropped)), st.SplitAUsReordered)
	assert.Equal(t, uint64(len(src)-len(dropped)-groups), st.SplitAUsPOC)
	assert.Equal(t, st.SplitAUsPOC, st.SplitAUs)
	assert.Zero(t, st.SplitAUsNoRoom)
	assert.Zero(t, st.DroppedNoConfig)
}

// TestVideoPESManyAUsReordered packs three fixture access units with
// B-frames into one PES carrying only the first unit's PTS and DTS. Once
// ticks per POC unit are calibrated (from the first units of PES 0 and 1),
// every unit is recovered with the source's timestamps, except the rest of
// a group from an IDR inside it (POC reset): the IDRs are units 25, 50, 100
// and 125 (75 starts a group).
func TestVideoPESManyAUsReordered(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h264_pyramid.ts"} {
		t.Run(name, func(t *testing.T) {
			src := videoFrames(t, name)
			require.Len(t, src, 150)
			d, out := demuxAll(t, repackAUs(t, typeH264, src, 3))
			assertRecovered(t, d, src, out, 3, []int{1, 2, 25, 26, 50, 100, 101, 125})
		})
	}
}

// TestVideoPESManyAUsFixture packs pairs of fixture access units into one PES
// without AUDs, timestamped as their first unit, and expects the source
// frames. h265_aac51.ts has no POC reset after its first picture (its later
// keys are CRA); the IDRs of h264_aac.ts at 25, 75 and 125 are second in a
// pair.
func TestVideoPESManyAUsFixture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stype   uint8
		dropped []int
	}{
		{"h264_aac.ts", typeH264, []int{1, 25, 75, 125}},
		{"h265_aac51.ts", typeH265, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := videoFrames(t, tc.name)
			d, out := demuxAll(t, repackAUs(t, tc.stype, src, 2))
			assertRecovered(t, d, src, out, 2, tc.dropped)
		})
	}
}

// TestVideoPESManyAUsOpenGOP repacks a feed that starts at an open-GOP
// recovery point (a non-IDR I picture): the counter starts there without an
// IDR, and every unit after calibration is recovered.
func TestVideoPESManyAUsOpenGOP(t *testing.T) {
	src := videoFrames(t, "h264_opengop_midgop.ts")
	require.True(t, src[0].Key)
	d, out := demuxAll(t, repackAUs(t, typeH264, src, 3))
	assertRecovered(t, d, src, out, 3, []int{1, 2})
}

// TestVideoPESManyAUsPOCReset puts IDRs inside groups: the rest of such a
// group is dropped, and the next group is timed by POC in the new period
// with the calibration kept. The stream is h264_aac.ts up to its IDR at unit
// 75, which starts a group, then from its IDR at unit 100 on, moved one
// frame after 75: unit 75 is a one-picture GOP. The IDR then sits second in
// the group of unit 75 (POC 0), where the POC alone would give it and the
// next unit a PTS above their DTS, but the wrong one. The real IDRs at 25
// and 50, and 125 (now 101), sit inside groups too.
func TestVideoPESManyAUsPOCReset(t *testing.T) {
	all := videoFrames(t, "h264_aac.ts")
	require.True(t, all[75].Key)
	require.True(t, all[100].Key)
	src := slices.Clone(all[:76])
	move := all[100].PTS - all[75].PTS - 3600
	for _, f := range all[100:] {
		f.PTS -= move
		f.DTS -= move
		src = append(src, f)
	}
	d, out := demuxAll(t, repackAUs(t, typeH264, src, 3))
	assertRecovered(t, d, src, out, 3, []int{1, 2, 25, 26, 50, 76, 77, 101})
}

// TestVideoPESManyAUsPTSBelowDTS makes every PES DTS 5400 ticks late: a
// split unit whose PTS from its POC falls below its DTS is dropped. In
// h264_aac.ts those are the units with PTS - DTS == 3600.
func TestVideoPESManyAUsPTSBelowDTS(t *testing.T) {
	const late = 5400
	src := videoFrames(t, "h264_aac.ts")[:33]
	d, out := demuxAll(t, repackAUsLate(t, typeH264, src, 3, late))
	shift := out[0].PTS - src[0].PTS
	want, got := make([]int, 0, len(src)), make([]int, 0, len(out))
	below := 0
	for i, f := range src {
		switch {
		case i%3 == 0:
		case i < 3 || i == 25 || i == 26: // before calibration; POC reset
			continue
		case f.PTS < f.DTS+late:
			below++
			continue
		}
		want = append(want, i)
	}
	require.Positive(t, below)
	for _, f := range out {
		i := slices.IndexFunc(src, func(s media.Frame) bool { return s.PTS == f.PTS-shift })
		require.GreaterOrEqual(t, i, 0)
		assert.Equal(t, src[i].DTS+late, f.DTS-shift, "unit %d", i)
		if i%3 != 0 { // unit 0 keeps its PES timestamps, made PTS < DTS here
			assert.GreaterOrEqual(t, f.PTS, f.DTS, "unit %d", i)
		}
		got = append(got, i)
	}
	assert.Equal(t, want, got)
	assert.Equal(t, uint64(4+below), d.Stats().SplitAUsReordered)
}

// withPPSID1 returns an H.264 PPS rewritten with pic_parameter_set_id 1 in
// place of 0.
func withPPSID1(t *testing.T, pps []byte) []byte {
	t.Helper()
	require.NotContains(t, string(pps), "\x00\x00\x03", "emulation prevention")
	var bits []byte
	for _, b := range pps[1:] {
		for i := 7; i >= 0; i-- {
			bits = append(bits, b>>i&1)
		}
	}
	require.Equal(t, byte(1), bits[0], "pic_parameter_set_id 0")
	// Drop the trailing alignment zeros, keeping rbsp_stop_one_bit.
	end := len(bits)
	for bits[end-1] == 0 {
		end--
	}
	bits = append([]byte{0, 1, 0}, bits[1:end]...) // ue(v) 1
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	out := []byte{pps[0]}
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for _, bit := range bits[i : i+8] {
			b = b<<1 | bit
		}
		out = append(out, b)
	}
	require.NotContains(t, string(out[1:]), "\x00\x00", "needs emulation prevention")
	return out
}

// TestVideoPESManyAUsNoPOC repacks h264_aac.ts with a PPS whose ID the
// slices do not reference: no POC is available, so the later units of every
// PES with PTS != DTS are dropped as before.
func TestVideoPESManyAUsNoPOC(t *testing.T) {
	src := videoFrames(t, "h264_aac.ts")[:30]
	cfg := *src[0].Config
	cfg.PPS = [][]byte{withPPSID1(t, cfg.PPS[0])}
	for i := range src {
		src[i].Config = &cfg
	}
	d, out := demuxAll(t, repackAUs(t, typeH264, src, 3))
	require.Len(t, out, 10)
	for k, f := range out {
		assert.Equal(t, src[3*k].Data, f.Data, "frame %d", k)
	}
	st := d.Stats()
	assert.Equal(t, uint64(20), st.SplitAUsReordered)
	assert.Zero(t, st.SplitAUsPOC)
	assert.Zero(t, st.SplitAUs)
}

func TestAudioPESWithoutPTS(t *testing.T) {
	asc := []byte{0x11, 0x90} // AAC-LC, 48 kHz, stereo
	adts := func(n int) []byte {
		var b []byte
		for i := range n {
			var err error
			b, err = mpegts.AppendADTS(b, asc, bytes.Repeat([]byte{byte(0x21 + i)}, 20))
			require.NoError(t, err)
		}
		return b
	}
	b := newTSBuilder(t)
	b.program(aacStream)
	b.pes(&mpegts.PES{PID: audioPID, PTS: 9000, DTS: 9000, Data: adts(2)})
	b.pesNoPTS(audioPID, adts(3))

	d, frames := demuxAll(t, b.buf.Bytes())
	require.Len(t, frames, 5)
	assert.Equal(t, []int64{0, 1920, 3840, 5760, 7680}, ptsOf(frames))
	assert.Equal(t, ptsOf(frames), dtsOf(frames))
	assert.Zero(t, d.Stats().DecodeErrors)
}

func TestLostFramesCounted(t *testing.T) {
	sps, pps := h264Params(t)
	t.Run("pass through", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
		start := b.buf.Len()
		b.videoPES(videoPID, 3600, 3600, joinNALUs(nalAUD, pSlice(true, 600)))
		b.cutSecondPacket(start)
		next := pSlice(true, 50)
		b.videoPES(videoPID, 7200, 7200, joinNALUs(nalAUD, next))
		b.videoPES(videoPID, 10800, 10800, joinNALUs(nalAUD, pSlice(true, 50)))

		d, frames := demuxAll(t, b.buf.Bytes())
		// Every PES has a PTS, so the pending unit is complete; the lost one
		// never arrives; the next ones pass through.
		assert.Equal(t, []int64{0, 7200, 10800}, dtsOf(frames))
		require.Len(t, frames, 3)
		assert.Equal(t, [][]byte{next}, frames[1].Data)
		assert.Equal(t, uint64(1), d.Stats().LostFrames)
	})
	t.Run("continuation after loss", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
		b.pesNoPTS(videoPID, joinNALUs(idrSlice(false, 50)))
		start := b.buf.Len()
		b.videoPES(videoPID, 3600, 3600, joinNALUs(nalAUD, pSlice(true, 600)))
		b.cutSecondPacket(start)
		b.pesNoPTS(videoPID, joinNALUs(pSlice(false, 50)))
		b.videoPES(videoPID, 7200, 7200, joinNALUs(nalAUD, pSlice(true, 50)))

		d, frames := demuxAll(t, b.buf.Bytes())
		// The track carries PES without PTS, so the pending unit may be
		// missing data: it is dropped, and so is the continuation after the
		// loss. LostFrames: the lost PES and the dropped pending unit.
		assert.Equal(t, []int64{7200}, dtsOf(frames))
		assert.Equal(t, uint64(2), d.Stats().LostFrames)
		assert.Equal(t, uint64(1), d.Stats().Continued)
	})
}

func TestPMTFlushesPending(t *testing.T) {
	sps, pps := h264Params(t)
	t.Run("remap", func(t *testing.T) {
		const newPID = 0x300
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
		b.videoPES(videoPID, 3600, 3600, joinNALUs(nalAUD, pSlice(true, 50)))
		b.program(mpegts.Stream{PID: newPID, Type: typeH264})
		b.videoPES(newPID, 7200, 7200, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
		b.videoPES(newPID, 10800, 10800, joinNALUs(nalAUD, pSlice(true, 50)))

		d, frames := demuxAllOpts(t, b.buf.Bytes(), DemuxOptions{Remap: true})
		assert.Equal(t, uint64(1), d.Stats().PMTChanges)
		require.Equal(t, []int64{0, 3600, 7200, 10800}, dtsOf(frames))
		assert.Same(t, frames[0].Config, frames[1].Config)
		assert.NotSame(t, frames[1].Config, frames[2].Config)
		assert.Same(t, frames[2].Config, frames[3].Config)
	})
	t.Run("follow", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream, aacStream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
		b.videoPES(videoPID, 3600, 3600, joinNALUs(nalAUD, pSlice(true, 50)))
		b.program(mpegts.Stream{PID: videoPID, Type: 0x03}, aacStream)
		adts, err := mpegts.AppendADTS(nil, []byte{0x11, 0x90}, bytes.Repeat([]byte{0x21}, 20))
		require.NoError(t, err)
		b.pes(&mpegts.PES{PID: audioPID, PTS: 7200, DTS: 7200, Data: adts})

		_, frames := demuxAllOpts(t, b.buf.Bytes(), DemuxOptions{FollowPMT: true})
		require.Len(t, frames, 3)
		assert.Equal(t, []int{0, 0, 1}, []int{frames[0].Track, frames[1].Track, frames[2].Track})
		assert.Equal(t, []int64{0, 3600, 7200}, dtsOf(frames))
	})
}

func TestVideoAccessUnitBounded(t *testing.T) {
	sps, pps := h264Params(t)
	chunk := pSlice(false, 60000)
	b := newTSBuilder(t)
	b.program(h264Stream)
	b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
	// Continuations with no access-unit start, past maxAUSize in total.
	for range maxAUSize/len(chunk) + 1 {
		b.pesNoPTS(videoPID, joinNALUs(chunk))
	}
	next := pSlice(true, 50)
	b.videoPES(videoPID, 3600, 3600, joinNALUs(nalAUD, next))

	d, frames := demuxAll(t, b.buf.Bytes())
	require.Len(t, frames, 1)
	assert.Equal(t, [][]byte{next}, frames[0].Data)
	assert.Equal(t, uint64(1), d.Stats().DecodeErrors)
}

// fieldSPS is a Main-profile SPS with frame_mbs_only_flag == 0 (PAFF),
// sps_id 0, 336x288.
var fieldSPS = []byte{0x67, 0x4D, 0x00, 0x1E, 0xF4, 0x0A, 0x89, 0x24}

func TestFieldCodedH264(t *testing.T) {
	_, pps := h264Params(t)
	top, bottom := idrSlice(true, 40), pSlice(true, 41)
	b := newTSBuilder(t)
	b.program(h264Stream)
	// Both fields of a frame in one PES with PTS != DTS: each field's first
	// slice has first_mb_in_slice == 0, but field-coded streams split only
	// on AUD.
	b.videoPES(videoPID, 7200, 3600, joinNALUs(nalAUD, fieldSPS, pps, top, bottom))
	b.videoPES(videoPID, 10800, 7200, joinNALUs(nalAUD, pSlice(true, 42), pSlice(true, 43)))

	d, frames := demuxAll(t, b.buf.Bytes())
	require.Len(t, frames, 2)
	assert.Equal(t, [][]byte{top, bottom}, frames[0].Data)
	assert.Equal(t, 336, frames[0].Config.Width)
	st := d.Stats()
	assert.Zero(t, st.SplitAUsReordered)
	assert.Zero(t, st.SplitAUs)
	assert.Zero(t, st.DroppedNoConfig)
}

// assertIncreasing checks that DTS strictly increases.
func assertIncreasing(t *testing.T, frames []media.Frame) {
	t.Helper()
	for i := 1; i < len(frames); i++ {
		assert.Greater(t, frames[i].DTS, frames[i-1].DTS, "frame %d", i)
	}
}

func TestSplitAUsSpacedToNextPES(t *testing.T) {
	sps, pps := h264Params(t)
	// 60 fps, four units per PES, no step measured before the first PES.
	b := newTSBuilder(t)
	b.program(h264Stream)
	for p := range 4 {
		var nalus [][]byte
		for u := range 4 {
			nalus = append(nalus, nalAUD)
			if p == 0 && u == 0 {
				nalus = append(nalus, sps, pps, idrSlice(true, 30))
				continue
			}
			nalus = append(nalus, pSlice(true, 30+u))
		}
		dts := int64(p) * 6000
		b.videoPES(videoPID, dts, dts, joinNALUs(nalus...))
	}
	d, frames := demuxAll(t, b.buf.Bytes())
	require.Len(t, frames, 16)
	for i, f := range frames {
		assert.Equal(t, int64(i)*1500, f.DTS, "frame %d", i)
		assert.Equal(t, f.DTS, f.PTS, "frame %d", i)
	}
	assert.Equal(t, uint64(12), d.Stats().SplitAUs)
}

func TestSplitAUsAcrossJumps(t *testing.T) {
	sps, pps := h264Params(t)
	three := func() []byte {
		return joinNALUs(nalAUD, pSlice(true, 30), nalAUD, pSlice(true, 31), nalAUD, pSlice(true, 32))
	}
	build := func(next int64) []byte {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 30)))
		b.videoPES(videoPID, 9000, 9000, joinNALUs(nalAUD, pSlice(true, 30))) // step 9000
		b.videoPES(videoPID, 18000, 18000, three())
		b.videoPES(videoPID, next, next, joinNALUs(nalAUD, pSlice(true, 33)))
		return b.buf.Bytes()
	}
	t.Run("forward splice", func(t *testing.T) {
		// +10 s with continuous CC: the gap per unit is too large to space
		// evenly, so the last step is used, below the next PES DTS.
		d, frames := demuxAll(t, build(18000+900000))
		assert.Equal(t, []int64{0, 9000, 18000, 27000, 36000, 918000}, dtsOf(frames))
		assertIncreasing(t, frames)
		assert.Zero(t, d.Stats().SplitAUsNoRoom)
	})
	t.Run("backward jump", func(t *testing.T) {
		// No DTS between the PES and the next one: the split units have no
		// room and are dropped.
		d, frames := demuxAll(t, build(5000))
		assert.Equal(t, []int64{0, 9000, 18000, 5000}, dtsOf(frames))
		assert.Equal(t, uint64(2), d.Stats().SplitAUsNoRoom)
	})
	t.Run("loss clamps to the next PES", func(t *testing.T) {
		// A loss before the next PES: no even spacing; the 9000 step would
		// pass the next DTS, so the second unit has no room.
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 30)))
		b.videoPES(videoPID, 9000, 9000, joinNALUs(nalAUD, pSlice(true, 30)))
		b.videoPES(videoPID, 18000, 18000, three())
		start := b.buf.Len()
		b.videoPES(videoPID, 24000, 24000, joinNALUs(nalAUD, pSlice(true, 600)))
		b.cutSecondPacket(start)
		b.videoPES(videoPID, 30000, 30000, joinNALUs(nalAUD, pSlice(true, 34)))
		d, frames := demuxAll(t, b.buf.Bytes())
		assert.Equal(t, []int64{0, 9000, 18000, 27000, 30000}, dtsOf(frames))
		assert.Equal(t, uint64(1), d.Stats().SplitAUsNoRoom)
	})
}

func TestPTSLessPESStartsAU(t *testing.T) {
	sps, pps := h264Params(t)
	t.Run("spaced", func(t *testing.T) {
		p1 := pSlice(true, 31)
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 30)))
		b.pesNoPTS(videoPID, joinNALUs(nalAUD, p1))
		b.videoPES(videoPID, 6000, 6000, joinNALUs(nalAUD, pSlice(true, 32)))
		d, frames := demuxAll(t, b.buf.Bytes())
		assert.Equal(t, []int64{0, 3000, 6000}, dtsOf(frames))
		require.Len(t, frames, 3)
		assert.Equal(t, [][]byte{p1}, frames[1].Data)
		assert.Equal(t, uint64(1), d.Stats().SplitAUs)
		assert.Equal(t, uint64(1), d.Stats().Continued)
	})
	t.Run("reordered", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 3600, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 30)))
		b.pesNoPTS(videoPID, joinNALUs(nalAUD, pSlice(true, 31)))
		b.videoPES(videoPID, 10800, 7200, joinNALUs(nalAUD, pSlice(true, 32)))
		d, frames := demuxAll(t, b.buf.Bytes())
		assert.Len(t, frames, 2)
		assert.Equal(t, uint64(1), d.Stats().SplitAUsReordered)
	})
}

func TestAudioContinuationDroppedAfterLoss(t *testing.T) {
	asc := []byte{0x11, 0x90}
	adts := func(n, size int) []byte {
		var b []byte
		for i := range n {
			var err error
			b, err = mpegts.AppendADTS(b, asc, bytes.Repeat([]byte{byte(0x21 + i)}, size))
			require.NoError(t, err)
		}
		return b
	}
	b := newTSBuilder(t)
	b.program(aacStream)
	b.pes(&mpegts.PES{PID: audioPID, PTS: 0, DTS: 0, Data: adts(2, 20)})
	start := b.buf.Len()
	b.pes(&mpegts.PES{PID: audioPID, PTS: 3840, DTS: 3840, Data: adts(3, 200)})
	b.cutSecondPacket(start)
	b.pesNoPTS(audioPID, adts(3, 20))
	b.pes(&mpegts.PES{PID: audioPID, PTS: 17280, DTS: 17280, Data: adts(1, 20)})

	d, frames := demuxAll(t, b.buf.Bytes())
	assert.Equal(t, []int64{0, 1920, 17280}, ptsOf(frames))
	// The lost PES counts one; the dropped continuation its three frames.
	assert.Equal(t, uint64(4), d.Stats().LostFrames)
}

func TestFollowPMTFlushesAbsentPID(t *testing.T) {
	sps, pps := h264Params(t)
	b := newTSBuilder(t)
	b.program(h264Stream, aacStream)
	b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
	b.videoPES(videoPID, 3600, 3600, joinNALUs(nalAUD, pSlice(true, 50)))
	b.program(aacStream)
	adts, err := mpegts.AppendADTS(nil, []byte{0x11, 0x90}, bytes.Repeat([]byte{0x21}, 20))
	require.NoError(t, err)
	b.pes(&mpegts.PES{PID: audioPID, PTS: 7200, DTS: 7200, Data: adts})

	_, frames := demuxAllOpts(t, b.buf.Bytes(), DemuxOptions{FollowPMT: true})
	require.Len(t, frames, 3)
	assert.Equal(t, []int{0, 0, 1}, []int{frames[0].Track, frames[1].Track, frames[2].Track})
}

func TestRemapRebindResetsAssembly(t *testing.T) {
	sps, pps := h264Params(t)
	const newPID = 0x300
	b := newTSBuilder(t)
	b.program(h264Stream)
	b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
	b.pesNoPTS(videoPID, joinNALUs(idrSlice(false, 50))) // the old PID carries PES without PTS
	b.program(mpegts.Stream{PID: newPID, Type: typeH264})
	b.videoPES(newPID, 7200, 7200, joinNALUs(nalAUD, sps, pps, idrSlice(true, 50)))
	start := b.buf.Len()
	b.videoPES(newPID, 10800, 10800, joinNALUs(nalAUD, pSlice(true, 600)))
	b.cutSecondPacket(start)
	b.videoPES(newPID, 14400, 14400, joinNALUs(nalAUD, pSlice(true, 50)))

	d, frames := demuxAllOpts(t, b.buf.Bytes(), DemuxOptions{Remap: true})
	// The rebound slot does not inherit "carries PES without PTS", so the
	// loss keeps its complete pending unit.
	assert.Equal(t, []int64{0, 7200, 14400}, dtsOf(frames))
	assert.Equal(t, uint64(1), d.Stats().LostFrames)
	assert.False(t, d.tracks[0].ptsLess)
}

func TestFieldCodedH264WithAUDs(t *testing.T) {
	_, pps := h264Params(t)
	top, bottom := idrSlice(true, 40), pSlice(true, 41)
	b := newTSBuilder(t)
	b.program(h264Stream)
	// Each field is preceded by an AUD, in one PES with PTS != DTS: a
	// field-coded track never splits a PES, so both fields stay one frame.
	b.videoPES(videoPID, 7200, 3600, joinNALUs(nalAUD, fieldSPS, pps, top, nalAUD, bottom))
	b.videoPES(videoPID, 10800, 7200, joinNALUs(nalAUD, pSlice(true, 42), nalAUD, pSlice(true, 43)))

	d, frames := demuxAll(t, b.buf.Bytes())
	require.Len(t, frames, 2)
	assert.Equal(t, [][]byte{top, bottom}, frames[0].Data)
	assert.Len(t, frames[1].Data, 2)
	st := d.Stats()
	assert.Zero(t, st.SplitAUsReordered)
	assert.Zero(t, st.SplitAUs)
	assert.Zero(t, st.SplitAUsNoRoom)
	assert.Zero(t, st.DroppedNoConfig)
}

func TestSplitAUsAfterPTSLessLoss(t *testing.T) {
	sps, pps := h264Params(t)
	three := joinNALUs(nalAUD, pSlice(true, 30), nalAUD, pSlice(true, 31), nalAUD, pSlice(true, 32))
	t.Run("group clamped to the next PES", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 30)))
		b.videoPES(videoPID, 9000, 9000, joinNALUs(nalAUD, pSlice(true, 30))) // step 9000
		b.videoPES(videoPID, 18000, 18000, three)
		start := b.buf.Len()
		b.pesNoPTS(videoPID, joinNALUs(pSlice(false, 600)))
		b.cutSecondPacket(start)                           // lost
		b.pesNoPTS(videoPID, joinNALUs(pSlice(false, 40))) // Discontinuity, dropped
		b.videoPES(videoPID, 24000, 24000, joinNALUs(nalAUD, pSlice(true, 33)))

		d, frames := demuxAll(t, b.buf.Bytes())
		// Unit 1 of the 18000 group would get 27000 with the 9000 step,
		// past the next PES: no room. Unit 2 was pending at the loss.
		assert.Equal(t, []int64{0, 9000, 18000, 24000}, dtsOf(frames))
		assertIncreasing(t, frames)
		st := d.Stats()
		assert.Equal(t, uint64(1), st.SplitAUsNoRoom)
		assert.Equal(t, uint64(2), st.LostFrames, "the lost PES and the dropped pending unit")
	})
	t.Run("no step measured across the loss", func(t *testing.T) {
		b := newTSBuilder(t)
		b.program(h264Stream)
		b.videoPES(videoPID, 0, 0, joinNALUs(nalAUD, sps, pps, idrSlice(true, 30)))
		b.videoPES(videoPID, 9000, 9000, joinNALUs(nalAUD, pSlice(true, 30))) // step 9000
		b.videoPES(videoPID, 18000, 18000, joinNALUs(nalAUD, pSlice(true, 30)))
		start := b.buf.Len()
		b.pesNoPTS(videoPID, joinNALUs(nalAUD, pSlice(true, 600)))
		b.cutSecondPacket(start)                                  // lost
		b.pesNoPTS(videoPID, joinNALUs(pSlice(false, 40)))        // Discontinuity, dropped
		b.pesNoPTS(videoPID, joinNALUs(nalAUD, pSlice(true, 41))) // dropped, no flag
		b.videoPES(videoPID, 54000, 54000, three)                 // no Discontinuity flag

		d, frames := demuxAll(t, b.buf.Bytes())
		// The 18000 unit was pending at the loss and is dropped.
		// 54000-18000 over one counted unit would measure 36000; the last
		// good step stays 9000 and times the final group at EOF.
		assert.Equal(t, []int64{0, 9000, 54000, 63000, 72000}, dtsOf(frames))
		assert.Equal(t, uint64(3), d.Stats().LostFrames,
			"the lost PES, the dropped pending unit and the unit starting in a dropped continuation")
		assert.Equal(t, int64(9000), d.tracks[0].step)
	})
}
