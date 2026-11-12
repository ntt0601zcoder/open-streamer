package mpegts

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PIDs and stream types of the hand-built test streams.
const (
	tPMT   = 0x1000
	tVideo = 0x100
	tAudio = 0x101
	tH264  = 0x1B
	tAAC   = 0x0F
)

// tsBuilder assembles a transport stream from raw packets and keeps a
// continuity counter per PID.
type tsBuilder struct {
	buf bytes.Buffer
	cc  map[uint16]uint8
}

func newTS() *tsBuilder { return &tsBuilder{cc: map[uint16]uint8{}} }

// nextCC returns the PID's next continuity counter and advances it.
func (b *tsBuilder) nextCC(pid uint16) uint8 {
	c := b.cc[pid]
	b.cc[pid] = (c + 1) & 0x0F
	return c
}

func (b *tsBuilder) raw(pkt []byte) *tsBuilder {
	if len(pkt) != PacketSize {
		panic("bad packet size")
	}
	b.buf.Write(pkt)
	return b
}

// psi writes one packet holding a pointer field of 0, the sections and 0xFF
// stuffing.
func (b *tsBuilder) psi(pid uint16, sections ...[]byte) *tsBuilder {
	payload := []byte{0}
	for _, s := range sections {
		payload = append(payload, s...)
	}
	return b.raw(buildPacket(pid, b.nextCC(pid), true, nil, payload))
}

// pes splits a PES into packets; flags is the adaptation field flags byte of
// the first packet (0x40 random access, 0x80 discontinuity).
func (b *tsBuilder) pes(pid uint16, pes []byte, flags byte) *tsBuilder {
	for _, p := range splitPES(pid, pes, flags, func() uint8 { return b.nextCC(pid) }) {
		b.raw(p)
	}
	return b
}

// base writes a PAT for program 1 on tPMT and a PMT version 0 with H.264
// video and AAC audio.
func (b *tsBuilder) base() *tsBuilder {
	return b.psi(0, patSection(0, 1, tPMT)).
		psi(tPMT, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}, Stream{tAudio, tAAC}))
}

func (b *tsBuilder) bytes() []byte { return b.buf.Bytes() }

// splitPES cuts a PES into packets whose payload ends the packet, stuffing
// short payloads with the adaptation field.
func splitPES(pid uint16, pes []byte, flags byte, cc func() uint8) [][]byte {
	var out [][]byte
	first := true
	for len(pes) > 0 {
		limit := 184
		if first && flags != 0 {
			limit = 182
		}
		n := min(limit, len(pes))
		f := byte(0)
		if first {
			f = flags
		}
		out = append(out, esPacket(pid, cc(), first, f, pes[:n]))
		pes = pes[n:]
		first = false
	}
	return out
}

// esPacket builds a packet whose payload ends the packet. An adaptation field
// carries flags and the stuffing; flags != 0 needs len(payload) <= 182.
func esPacket(pid uint16, cc uint8, pusi bool, flags byte, payload []byte) []byte {
	b := make([]byte, PacketSize)
	b[0] = 0x47
	b[1] = byte(pid>>8) & 0x1F
	if pusi {
		b[1] |= 0x40
	}
	b[2] = byte(pid)
	space := 184 - len(payload)
	if flags != 0 && space < 2 {
		panic("no room for adaptation flags")
	}
	if space == 0 {
		b[3] = 0x10 | cc&0x0F
		copy(b[4:], payload)
		return b
	}
	b[3] = 0x30 | cc&0x0F
	b[4] = byte(space - 1)
	if space >= 2 {
		b[5] = flags
		for i := 6; i < 4+space; i++ {
			b[i] = 0xFF
		}
	}
	copy(b[4+space:], payload)
	return b
}

// afOnly builds an adaptation-field-only packet.
func afOnly(pid uint16, cc uint8, flags byte) []byte {
	return buildPacket(pid, cc, false, []byte{flags}, nil)
}

// section builds a long-form PSI section with a valid CRC.
func section(tableID uint8, idExt uint16, version uint8, current bool, body []byte) []byte {
	slen := 5 + len(body) + 4
	cni := byte(0)
	if current {
		cni = 1
	}
	s := []byte{
		tableID, 0xB0 | byte(slen>>8), byte(slen),
		byte(idExt >> 8), byte(idExt), 0xC0 | (version&0x1F)<<1 | cni, 0, 0,
	}
	s = append(s, body...)
	c := crc32MPEG2(s)
	return append(s, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

// patSection builds a PAT from (program number, PMT PID) pairs.
func patSection(version uint8, pairs ...uint16) []byte {
	var body []byte
	for i := 0; i+1 < len(pairs); i += 2 {
		body = append(body, byte(pairs[i]>>8), byte(pairs[i]), 0xE0|byte(pairs[i+1]>>8), byte(pairs[i+1]))
	}
	return section(0x00, 1, version, true, body)
}

func pmtBody(pcrPID uint16, streams ...Stream) []byte {
	body := make([]byte, 0, 4+5*len(streams))
	body = append(body, 0xE0|byte(pcrPID>>8), byte(pcrPID), 0xF0, 0)
	for _, s := range streams {
		body = append(body, s.Type, 0xE0|byte(s.PID>>8), byte(s.PID), 0xF0, 0)
	}
	return body
}

func pmtSection(program uint16, version uint8, pcrPID uint16, streams ...Stream) []byte {
	return section(0x02, program, version, true, pmtBody(pcrPID, streams...))
}

// encodeTS encodes a 33-bit timestamp with the 4-bit prefix.
func encodeTS(prefix byte, v int64) []byte {
	return []byte{
		prefix<<4 | byte(v>>29)&0x0E | 1,
		byte(v >> 22),
		byte(v>>14)&0xFE | 1,
		byte(v >> 7),
		byte(v<<1) | 1,
	}
}

// pesBytes builds a PES with stream_id 0xE0; pts or dts < 0 omits them, and
// sized selects a non-zero PES_packet_length.
func pesBytes(pts, dts int64, data []byte, sized bool) []byte {
	return pesWithID(0xE0, pts, dts, data, sized)
}

func pesWithID(sid byte, pts, dts int64, data []byte, sized bool) []byte {
	var ts []byte
	flags := byte(0)
	if pts >= 0 && dts >= 0 {
		flags = 0xC0
		ts = append(encodeTS(3, pts), encodeTS(1, dts)...)
	} else if pts >= 0 {
		flags = 0x80
		ts = encodeTS(2, pts)
	}
	opt := append([]byte{0x80, flags, byte(len(ts))}, ts...)
	plen := 0
	if sized {
		plen = len(opt) + len(data)
	}
	out := []byte{0, 0, 1, sid, byte(plen >> 8), byte(plen)}
	out = append(out, opt...)
	return append(out, data...)
}

// payloadOf returns n bytes of a pattern seeded with seed.
func payloadOf(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// demuxAll runs a Demux over r until it returns an error.
func demuxAll(t *testing.T, r io.Reader) ([]Event, DemuxStats, error) {
	t.Helper()
	d := NewDemux(r)
	var evs []Event
	for i := 0; ; i++ {
		require.Less(t, i, 1_000_000, "demux does not terminate")
		ev, err := d.Next()
		if err != nil {
			return evs, d.Stats(), err
		}
		require.True(t, (ev.PMT == nil) != (ev.PES == nil), "exactly one of PMT and PES is set")
		evs = append(evs, ev)
	}
}

// demuxBytes runs a Demux over data and requires that it ends with io.EOF.
func demuxBytes(t *testing.T, data []byte) ([]Event, DemuxStats) {
	t.Helper()
	evs, st, err := demuxAll(t, bytes.NewReader(data))
	require.ErrorIs(t, err, io.EOF)
	return evs, st
}

// manyStreams returns n H.264 streams from PID 0x200 on, enough for a PMT
// section longer than one packet.
func manyStreams(n int) []Stream {
	out := make([]Stream, n)
	for i := range out {
		out[i] = Stream{PID: uint16(0x200 + i), Type: tH264}
	}
	return out
}

// videoOf returns the PES events on tVideo.
func videoOf(evs []Event) []*PES {
	var out []*PES
	for _, e := range evs {
		if e.PES != nil && e.PES.PID == tVideo {
			out = append(out, e.PES)
		}
	}
	return out
}

func pmtsOf(evs []Event) []*PMT {
	var out []*PMT
	for _, e := range evs {
		if e.PMT != nil {
			out = append(out, e.PMT)
		}
	}
	return out
}

// kinds describes an event sequence as "pmt" or the PES PID in hex.
func kinds(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		if e.PMT != nil {
			out = append(out, "pmt")
			continue
		}
		out = append(out, map[uint16]string{tVideo: "video", tAudio: "audio"}[e.PES.PID])
	}
	return out
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return raw
}

// fixtureCounts demuxes a fixture and returns its video PES count, the total
// ADTS frames of its AAC PES and the stats.
func fixtureCounts(t *testing.T, name string) (pmts []*PMT, video, aacFrames int, st DemuxStats) {
	t.Helper()
	evs, st := demuxBytes(t, readFixture(t, name))
	pmts = pmtsOf(evs)
	require.NotEmpty(t, pmts)
	types := map[uint16]uint8{}
	for _, s := range pmts[0].Streams {
		types[s.PID] = s.Type
	}
	for _, e := range evs {
		if e.PES == nil {
			continue
		}
		switch types[e.PES.PID] {
		case tH264, 0x24:
			assert.True(t, e.PES.HasPTS)
			video++
		case tAAC:
			frames, err := ParseADTS(e.PES.Data)
			require.NoError(t, err)
			aacFrames += len(frames)
		}
	}
	return pmts, video, aacFrames, st
}

func TestDemuxFixturePES(t *testing.T) {
	pmts, video, aac, st := fixtureCounts(t, "h264_aac.ts")
	require.Len(t, pmts, 1)
	assert.Len(t, pmts[0].Streams, 2)
	assert.Equal(t, 150, video)
	assert.Equal(t, 283, aac)
	assert.Zero(t, st.CCErrors)
	assert.Zero(t, st.PSIErrors)
	assert.Zero(t, st.PESErrors)
	assert.Equal(t, uint64(1710), st.Reader.Packets)
}

func TestDemuxLoopCC(t *testing.T) {
	pmts, video, aac, st := fixtureCounts(t, "h264_aac_loop.ts")
	assert.Len(t, pmts, 1)
	assert.Equal(t, 299, video, "the pending length-0 video PES is dropped at the join")
	assert.Equal(t, 566, aac)
	// One gap on each of the PAT, PMT, video and audio PIDs.
	assert.Equal(t, uint64(4), st.CCErrors)
	assert.Zero(t, st.PSIErrors)
	assert.Zero(t, st.PESErrors)
}

func TestDemuxFixtureCounts(t *testing.T) {
	_, video, aac, st := fixtureCounts(t, "h264_aac_then_640.ts")
	assert.Equal(t, 199, video)
	assert.Equal(t, 283, aac)
	assert.Equal(t, uint64(3), st.CCErrors)

	// midgop starts 16 video PES before its first keyframe; M1 delivers the
	// 75 from the keyframe on.
	evs, st := demuxBytes(t, readFixture(t, "h264_aac_midgop.ts"))
	ps := videoOf(evs)
	assert.Len(t, ps, 91)
	first := slices.IndexFunc(ps, func(p *PES) bool { return p.RandomAccess })
	assert.Equal(t, 16, first)
	assert.Len(t, ps[first:], 75)
	assert.Zero(t, st.CCErrors)
	assert.Zero(t, st.PESErrors)
}

func TestCRC32MPEG2(t *testing.T) {
	assert.Equal(t, uint32(0x0376E6E7), crc32MPEG2([]byte("123456789")))
	assert.Equal(t, uint32(0xFFFFFFFF), crc32MPEG2(nil))
}

func TestDemuxSectionAcrossPackets(t *testing.T) {
	streams := manyStreams(40)
	v0 := pmtSection(1, 0, 0x200, streams...)
	v1 := pmtSection(1, 1, 0x200, streams[:2]...)
	require.Greater(t, len(v0), 183)

	b := newTS().psi(0, patSection(0, 1, tPMT))
	// Packet 1: pointer 0, the first 183 bytes of v0.
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), true, nil, append([]byte{0}, v0[:183]...)))
	// Packet 2: PUSI, the rest of v0 before the pointer, then v1.
	rest := v0[183:]
	p2 := append([]byte{byte(len(rest))}, rest...)
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), true, nil, append(p2, v1...)))
	// Packets 3 and 4: v0 again, split over a PUSI packet and a continuation.
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), true, nil, append([]byte{0}, v0[:183]...)))
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), false, nil, v0[183:]))

	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 3)
	assert.Equal(t, uint8(0), pmts[0].Version)
	assert.Len(t, pmts[0].Streams, 40)
	assert.Equal(t, uint8(1), pmts[1].Version)
	assert.Len(t, pmts[1].Streams, 2)
	assert.Equal(t, uint8(0), pmts[2].Version)
	assert.Zero(t, st.PSIErrors)
	assert.Zero(t, st.CCErrors)
}

func TestDemuxBadCRC(t *testing.T) {
	badPAT := patSection(0, 1, tPMT)
	badPAT[len(badPAT)-1] ^= 0x01
	badPMT := pmtSection(1, 0, tVideo, Stream{tVideo, tH264})
	badPMT[10] ^= 0x40

	b := newTS().psi(0, badPAT).psi(tPMT, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}))
	b.psi(0, patSection(0, 1, tPMT)).psi(tPMT, badPMT)
	b.psi(tPMT, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}))
	evs, st := demuxBytes(t, b.bytes())
	require.Len(t, pmtsOf(evs), 1, "the PMT before the PAT and the bad one are ignored")
	assert.Equal(t, uint64(2), st.PSIErrors)
}

func TestDemuxPMTVersionChange(t *testing.T) {
	v, a := Stream{tVideo, tH264}, Stream{tAudio, tAAC}
	b := newTS().psi(0, patSection(0, 1, tPMT))
	for range 3 {
		b.psi(tPMT, pmtSection(1, 0, tVideo, v, a))
	}
	b.psi(tPMT, pmtSection(1, 1, tVideo, v, a)).psi(tPMT, pmtSection(1, 1, tVideo, v, a))
	b.psi(tPMT, pmtSection(1, 1, tAudio, v, a)).psi(tPMT, pmtSection(1, 1, tAudio, v, a))
	b.psi(tPMT, pmtSection(1, 1, tAudio, a, v))
	b.psi(0, patSection(0, 1, tPMT))
	b.psi(tPMT, pmtSection(1, 1, tAudio, a, v))

	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 4)
	assert.Equal(t, &PMT{Program: 1, PID: tPMT, Version: 0, PCRPID: tVideo, Streams: []Stream{v, a}}, pmts[0])
	assert.Equal(t, uint8(1), pmts[1].Version)
	assert.Equal(t, uint16(tAudio), pmts[2].PCRPID)
	assert.Equal(t, []Stream{a, v}, pmts[3].Streams)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxSizedPESImmediate(t *testing.T) {
	boom := errors.New("boom")
	data := payloadOf(300, 1)
	b := newTS().base().pes(tVideo, pesBytes(9000, -1, data, true), 0x40)
	evs, _, err := demuxAll(t, io.MultiReader(bytes.NewReader(b.bytes()), iotest.ErrReader(boom)))
	require.ErrorIs(t, err, boom)
	require.Equal(t, []string{"pmt", "video"}, kinds(evs))
	p := evs[1].PES
	assert.Equal(t, data, p.Data)
	assert.True(t, p.RandomAccess)
	assert.False(t, p.Discontinuity)
}

func TestDemuxLengthZeroFlush(t *testing.T) {
	boom := errors.New("boom")
	b := newTS().base().
		pes(tVideo, pesBytes(1, -1, payloadOf(400, 1), false), 0).
		pes(tVideo, pesBytes(2, -1, payloadOf(50, 2), false), 0)
	evs, _, err := demuxAll(t, io.MultiReader(bytes.NewReader(b.bytes()), iotest.ErrReader(boom)))
	require.ErrorIs(t, err, boom)
	require.Equal(t, []string{"pmt", "video"}, kinds(evs), "the first PES completes at the next PUSI")
	assert.Equal(t, payloadOf(400, 1), evs[1].PES.Data)

	evs, st := demuxBytes(t, b.bytes())
	require.Equal(t, []string{"pmt", "video", "video"}, kinds(evs), "the second PES completes at EOF")
	assert.Equal(t, payloadOf(50, 2), evs[2].PES.Data)
	assert.Equal(t, int64(2), evs[2].PES.PTS)
	assert.Zero(t, st.PESErrors)

	// Pending PES are flushed in ascending PID order at EOF.
	b = newTS().base().
		pes(tAudio, pesBytes(3, -1, payloadOf(10, 3), false), 0).
		pes(tVideo, pesBytes(4, -1, payloadOf(10, 4), false), 0)
	evs, _ = demuxBytes(t, b.bytes())
	assert.Equal(t, []string{"pmt", "video", "audio"}, kinds(evs))
}

func TestDemuxDuplicateCC(t *testing.T) {
	data := payloadOf(600, 7)
	pkts := splitPES(tVideo, pesBytes(1, -1, data, true), 0, counter(0))
	b := newTS().base()
	b.raw(pkts[0]).raw(pkts[1]).raw(pkts[1]).raw(pkts[2]).raw(pkts[3]).raw(pkts[3])
	evs, st := demuxBytes(t, b.bytes())
	ps := videoOf(evs)
	require.Len(t, ps, 1)
	assert.Equal(t, data, ps[0].Data)
	assert.Zero(t, st.CCErrors)
	assert.Zero(t, st.PESErrors)
}

// counter returns a continuity counter source starting at start.
func counter(start uint8) func() uint8 {
	c := start
	return func() uint8 {
		v := c & 0x0F
		c++
		return v
	}
}

func TestDemuxCCGap(t *testing.T) {
	t.Run("lost packet drops the PES", func(t *testing.T) {
		pkts := splitPES(tVideo, pesBytes(1, -1, payloadOf(600, 1), true), 0, counter(0))
		b := newTS().base()
		b.raw(pkts[0]).raw(pkts[1]).raw(pkts[3])
		next := counter(4)
		for _, p := range splitPES(tVideo, pesBytes(2, -1, payloadOf(10, 2), true), 0, next) {
			b.raw(p)
		}
		for _, p := range splitPES(tVideo, pesBytes(3, -1, payloadOf(10, 3), true), 0, next) {
			b.raw(p)
		}
		evs, st := demuxBytes(t, b.bytes())
		ps := videoOf(evs)
		require.Len(t, ps, 2)
		assert.Equal(t, int64(2), ps[0].PTS)
		assert.True(t, ps[0].Discontinuity)
		assert.False(t, ps[1].Discontinuity)
		assert.Equal(t, uint64(1), st.CCErrors)
		assert.Zero(t, st.PESErrors)
	})

	t.Run("gap on a PUSI packet drops the pending length-0 PES", func(t *testing.T) {
		b := newTS().base()
		next := counter(0)
		for _, p := range splitPES(tVideo, pesBytes(1, -1, payloadOf(300, 1), false), 0, next) {
			b.raw(p)
		}
		next() // the CC of a lost packet
		for _, p := range splitPES(tVideo, pesBytes(2, -1, payloadOf(30, 2), false), 0, next) {
			b.raw(p)
		}
		evs, st := demuxBytes(t, b.bytes())
		ps := videoOf(evs)
		require.Len(t, ps, 1)
		assert.Equal(t, int64(2), ps[0].PTS)
		assert.True(t, ps[0].Discontinuity)
		assert.Equal(t, uint64(1), st.CCErrors)
		assert.Zero(t, st.PESErrors)
	})

	t.Run("gap inside a PSI section", func(t *testing.T) {
		streams := manyStreams(40)
		sec := pmtSection(1, 0, 0x200, streams...)
		b := newTS().psi(0, patSection(0, 1, tPMT))
		b.raw(buildPacket(tPMT, 0, true, nil, append([]byte{0}, sec[:183]...)))
		b.raw(buildPacket(tPMT, 2, false, nil, sec[183:]))
		b.raw(buildPacket(tPMT, 3, true, nil, append([]byte{0}, sec[:183]...)))
		b.raw(buildPacket(tPMT, 4, false, nil, sec[183:]))
		evs, st := demuxBytes(t, b.bytes())
		assert.Len(t, pmtsOf(evs), 1)
		assert.Equal(t, uint64(1), st.CCErrors)
		assert.Zero(t, st.PSIErrors)
	})
}

func TestDemuxAdaptationOnlyCC(t *testing.T) {
	b := newTS().base()
	next := counter(0)
	for _, p := range splitPES(tVideo, pesBytes(1, -1, payloadOf(10, 1), true), 0, next) {
		b.raw(p) // CC 0
	}
	b.raw(afOnly(tVideo, 0, 0)) // same CC: no error
	b.raw(afOnly(tVideo, 0, 0))
	b.raw(afOnly(tVideo, 5, 0)) // a gap; 5 becomes the reference
	b.raw(afOnly(tVideo, 5, 0))
	for _, p := range splitPES(tVideo, pesBytes(2, -1, payloadOf(10, 2), true), 0, counter(6)) {
		b.raw(p)
	}
	evs, st := demuxBytes(t, b.bytes())
	ps := videoOf(evs)
	require.Len(t, ps, 2)
	assert.False(t, ps[0].Discontinuity)
	assert.True(t, ps[1].Discontinuity)
	assert.Equal(t, uint64(1), st.CCErrors)
}

func TestDemuxNullPackets(t *testing.T) {
	b := newTS().base()
	pkts := splitPES(tVideo, pesBytes(1, -1, payloadOf(600, 1), true), 0, counter(0))
	for i, p := range pkts {
		b.raw(p)
		b.raw(buildPacket(0x1FFF, uint8(i*7+3), false, nil, []byte{0xFF}))
		b.raw(afOnly(0x1FFF, uint8(i*5), 0))
	}
	evs, st := demuxBytes(t, b.bytes())
	require.Len(t, videoOf(evs), 1)
	assert.Zero(t, st.CCErrors)
}

func TestDemuxDiscontinuityIndicator(t *testing.T) {
	b := newTS().base()
	for _, p := range splitPES(tVideo, pesBytes(1, -1, payloadOf(10, 1), true), 0, counter(0)) {
		b.raw(p)
	}
	// A payload packet that jumps CC with discontinuity_indicator set.
	for _, p := range splitPES(tVideo, pesBytes(2, -1, payloadOf(300, 2), true), 0x80, counter(9)) {
		b.raw(p) // CC 9, 10
	}
	// An adaptation-only packet with the indicator: the next CC is free.
	b.raw(afOnly(tVideo, 3, 0x80))
	for _, p := range splitPES(tVideo, pesBytes(3, -1, payloadOf(10, 3), true), 0, counter(14)) {
		b.raw(p)
	}
	evs, st := demuxBytes(t, b.bytes())
	ps := videoOf(evs)
	require.Len(t, ps, 3)
	for _, p := range ps {
		assert.False(t, p.Discontinuity)
	}
	assert.Equal(t, payloadOf(300, 2), ps[1].Data)
	assert.Zero(t, st.CCErrors)
}

func TestDemuxPIDRemoved(t *testing.T) {
	v, a := Stream{tVideo, tH264}, Stream{tAudio, tAAC}
	b := newTS().base()
	b.pes(tAudio, pesBytes(1, -1, payloadOf(20, 1), false), 0)
	b.psi(tPMT, pmtSection(1, 1, tVideo, v)) // removes audio: flush first
	// Audio is ignored while removed.
	b.pes(tAudio, pesBytes(2, -1, payloadOf(20, 2), true), 0)
	b.pes(tAudio, pesBytes(3, -1, payloadOf(20, 3), true), 0)
	b.psi(tPMT, pmtSection(1, 2, tVideo, v, a)) // listed again
	// A fresh CC reference: no false gap.
	for _, p := range splitPES(tAudio, pesBytes(4, -1, payloadOf(20, 4), false), 0, counter(11)) {
		b.raw(p)
	}
	b.psi(tPMT, pmtSection(1, 3, tVideo, v, Stream{tAudio, 0x11})) // type change
	for _, p := range splitPES(tAudio, pesBytes(5, -1, payloadOf(300, 5), true), 0, counter(12)) {
		b.raw(p) // CC 12, 13
	}
	// A sized PES left incomplete when audio is removed.
	b.raw(splitPES(tAudio, pesBytes(6, -1, payloadOf(300, 6), true), 0, counter(14))[0])
	b.psi(tPMT, pmtSection(1, 4, tVideo, v))

	evs, st := demuxBytes(t, b.bytes())
	require.Equal(t, []string{"pmt", "audio", "pmt", "pmt", "audio", "pmt", "audio", "pmt"}, kinds(evs))
	assert.Equal(t, int64(1), evs[1].PES.PTS)
	assert.Equal(t, int64(4), evs[4].PES.PTS)
	assert.Equal(t, uint8(0x11), evs[5].PMT.Streams[1].Type)
	assert.Equal(t, int64(5), evs[6].PES.PTS)
	assert.Zero(t, st.CCErrors)
	assert.Equal(t, uint64(1), st.PESErrors)
}

func TestDemuxSizedPESIncomplete(t *testing.T) {
	sized := pesBytes(1, -1, payloadOf(1000, 1), true)
	b := newTS().base()
	pkts := splitPES(tVideo, sized, 0, counter(0))
	b.raw(pkts[0]).raw(pkts[1])
	next := counter(2)
	for _, p := range splitPES(tVideo, pesBytes(2, -1, payloadOf(10, 2), true), 0, next) {
		b.raw(p)
	}
	for _, p := range splitPES(tVideo, sized, 0, next)[:3] {
		b.raw(p)
	}
	evs, st := demuxBytes(t, b.bytes())
	ps := videoOf(evs)
	require.Len(t, ps, 1)
	assert.Equal(t, int64(2), ps[0].PTS)
	assert.False(t, ps[0].Discontinuity)
	assert.Equal(t, uint64(2), st.PESErrors, "one at the next PUSI, one at EOF")
	assert.Zero(t, st.CCErrors)
}

func TestDemuxSizedPESTrailingBytes(t *testing.T) {
	// The packet carries bytes after the end PES_packet_length declares.
	pes := append(pesBytes(1, -1, []byte{1, 2, 3}, true), 0xAB, 0xCD, 0xEF)
	b := newTS().base().pes(tVideo, pes, 0)
	evs, st := demuxBytes(t, b.bytes())
	ps := videoOf(evs)
	require.Len(t, ps, 1)
	assert.Equal(t, []byte{1, 2, 3}, ps[0].Data)
	assert.Zero(t, st.PESErrors)
}

func TestDemuxPTSAndDTS(t *testing.T) {
	const big = int64(1)<<33 - 1
	b := newTS().base().
		pes(tVideo, pesBytes(big, 0x1_2345_6789, []byte{1}, true), 0x40).
		pes(tVideo, pesBytes(0x1_0000_0001, -1, []byte{2}, true), 0).
		pes(tVideo, pesBytes(-1, -1, []byte{3}, true), 0).
		pes(tVideo, []byte{0, 0, 1, 0xBF, 0, 2, 0xAA, 0xBB}, 0)
	evs, st := demuxBytes(t, b.bytes())
	ps := videoOf(evs)
	require.Len(t, ps, 4)
	assert.Equal(t, PES{PID: tVideo, PTS: big, DTS: 0x1_2345_6789, HasPTS: true, RandomAccess: true, Data: []byte{1}}, *ps[0])
	assert.Equal(t, PES{PID: tVideo, PTS: 0x1_0000_0001, DTS: 0x1_0000_0001, HasPTS: true, Data: []byte{2}}, *ps[1])
	assert.False(t, ps[2].HasPTS)
	assert.Equal(t, []byte{3}, ps[2].Data)
	assert.Equal(t, []byte{0xAA, 0xBB}, ps[3].Data, "a stream_id without the optional header")
	assert.False(t, ps[3].HasPTS)
	assert.Zero(t, st.PESErrors)
}

func TestDemuxMultipleSections(t *testing.T) {
	other := section(0xC0, 1, 0, true, []byte{1, 2, 3})
	short := []byte{0x72, 0x00, 0x02, 0xAA, 0xBB} // a short-form section
	pmt := pmtSection(1, 0, tVideo, Stream{tVideo, tH264})
	pmt1 := pmtSection(1, 1, tVideo, Stream{tVideo, tH264})
	b := newTS().psi(0, patSection(0, 1, tPMT))
	// Sections follow each other until the 0xFF stuffing; what follows the
	// stuffing is ignored.
	payload := append([]byte{0}, other...)
	payload = append(payload, short...)
	payload = append(payload, pmt...)
	payload = append(payload, 0xFF)
	payload = append(payload, pmt1...)
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), true, nil, payload))
	b.psi(tPMT, pmt1, pmt1)
	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 2)
	assert.Equal(t, uint8(0), pmts[0].Version)
	assert.Equal(t, uint8(1), pmts[1].Version)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxCurrentNextZero(t *testing.T) {
	b := newTS().psi(0, patSection(0, 1, tPMT))
	b.psi(0, section(0x00, 1, 1, false, []byte{0, 1, 0xE0 | 0x10, 0x01})) // next PAT
	b.psi(0, section(0x40, 1, 0, true, []byte{1, 2}))                     // other table_id
	b.psi(tPMT, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}))
	b.psi(tPMT, section(0x02, 1, 1, false, pmtBody(tVideo, Stream{tAudio, tAAC})))
	b.psi(tPMT, pmtSection(1, 1, tVideo, Stream{tAudio, tAAC}))
	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 2)
	assert.Equal(t, uint16(tPMT), pmts[0].PID)
	assert.Equal(t, uint8(1), pmts[1].Version)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxPMTPIDChange(t *testing.T) {
	const newPMT = 0x1001
	v, a := Stream{tVideo, tH264}, Stream{tAudio, tAAC}
	b := newTS().base()
	b.pes(tVideo, pesBytes(1, -1, payloadOf(20, 1), false), 0)
	b.raw(splitPES(tAudio, pesBytes(2, -1, payloadOf(300, 2), true), 0, counter(0))[0])
	b.psi(0, patSection(1, 1, newPMT))
	b.psi(tPMT, pmtSection(1, 5, tVideo, v)) // the old PMT PID is ignored
	b.psi(newPMT, pmtSection(1, 0, tVideo, v, a))
	for _, p := range splitPES(tVideo, pesBytes(3, -1, payloadOf(20, 3), true), 0, counter(9)) {
		b.raw(p)
	}
	for _, p := range splitPES(tAudio, pesBytes(4, -1, payloadOf(20, 4), true), 0, counter(4)) {
		b.raw(p)
	}
	evs, st := demuxBytes(t, b.bytes())
	require.Equal(t, []string{"pmt", "video", "pmt", "video", "audio"}, kinds(evs))
	assert.Equal(t, int64(1), evs[1].PES.PTS)
	assert.Equal(t, &PMT{Program: 1, PID: newPMT, PCRPID: tVideo, Streams: []Stream{v, a}}, evs[2].PMT)
	assert.False(t, evs[3].PES.Discontinuity)
	assert.Zero(t, st.CCErrors)
	assert.Equal(t, uint64(1), st.PESErrors)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxProgramZeroSkipped(t *testing.T) {
	// A PAT with only the network PID selects nothing.
	evs, st := demuxBytes(t, newTS().psi(0, patSection(0, 0, 0x10)).
		psi(0x10, pmtSection(0, 0, tVideo, Stream{tVideo, tH264})).bytes())
	assert.Empty(t, evs)
	assert.Zero(t, st.PSIErrors)

	b := newTS().psi(0, patSection(0, 0, 0x10, 7, tPMT, 8, 0x1002))
	b.psi(0x10, pmtSection(0, 0, tVideo, Stream{tVideo, tH264}))
	b.psi(tPMT, pmtSection(7, 0, tVideo, Stream{tVideo, tH264}))
	evs, st = demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 1)
	assert.Equal(t, uint16(7), pmts[0].Program)
	assert.Equal(t, uint16(tPMT), pmts[0].PID)
	assert.Zero(t, st.PSIErrors)
}

// patSectionN builds section number num of last of a PAT from (program
// number, PMT PID) pairs.
func patSectionN(version, num, last uint8, pairs ...uint16) []byte {
	sec := patSection(version, pairs...)
	sec[6], sec[7] = num, last
	return fixSectionCRC(sec)
}

// fixSectionCRC recomputes the CRC_32 of a section after an edit.
func fixSectionCRC(sec []byte) []byte {
	end := len(sec) - 4
	c := crc32MPEG2(sec[:end])
	sec[end], sec[end+1], sec[end+2], sec[end+3] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
	return sec
}

func TestDemuxMultiSectionPAT(t *testing.T) {
	const pmt2 = 0x1002
	sec0 := patSectionN(0, 0, 1, 0, 0x10, 1, tPMT)
	sec1 := patSectionN(0, 1, 1, 2, pmt2)
	b := newTS()
	for i := range 4 {
		// Section 1 comes first in the first repetition.
		if i == 0 {
			b.psi(0, sec1, sec0)
		} else {
			b.psi(0, sec0).psi(0, sec1)
		}
		b.psi(tPMT, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}))
		b.psi(pmt2, pmtSection(2, 0, tAudio, Stream{tAudio, tAAC}))
		b.pes(tVideo, pesBytes(int64(i), -1, payloadOf(20, byte(i)), true), 0)
		b.pes(tAudio, pesBytes(int64(i), -1, payloadOf(20, byte(i)), true), 0)
	}
	// A repetition of section 0 that moves program 1's PMT is followed.
	b.psi(0, patSectionN(1, 0, 1, 1, 0x1003)).psi(0, patSectionN(1, 1, 1, 2, pmt2))
	b.psi(0x1003, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}))
	b.pes(tVideo, pesBytes(9, -1, payloadOf(20, 9), true), 0)

	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 2)
	assert.Equal(t, uint16(tPMT), pmts[0].PID)
	assert.Equal(t, uint16(1), pmts[0].Program)
	assert.Equal(t, uint16(0x1003), pmts[1].PID)
	ps := videoOf(evs)
	require.Len(t, ps, 5)
	for i, p := range ps[:4] {
		assert.Equal(t, int64(i), p.PTS)
	}
	assert.Equal(t, int64(9), ps[4].PTS)
	assert.Zero(t, st.CCErrors)
	assert.Zero(t, st.PSIErrors)
	assert.Zero(t, st.PESErrors)
}

func TestDemuxPATVersionMovesProgram(t *testing.T) {
	// A new PAT version whose section 0 lists no program: section 1 chooses.
	b := newTS().psi(0, patSectionN(0, 0, 0, 1, tPMT))
	b.psi(tPMT, pmtSection(1, 0, tVideo, Stream{tVideo, tH264}))
	b.psi(0, patSectionN(1, 0, 1, 0, 0x10)).psi(0, patSectionN(1, 1, 1, 3, 0x1003))
	b.psi(0x1003, pmtSection(3, 0, tVideo, Stream{tVideo, tH264}))
	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 2)
	assert.Equal(t, uint16(3), pmts[1].Program)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxOtherTableCutShort(t *testing.T) {
	// A section of another table cut short by the next section start is
	// ignored; a cut PMT section counts.
	other := section(0xC0, 1, 0, true, make([]byte, 300))
	pmt := pmtSection(1, 0, tVideo, Stream{tVideo, tH264})
	b := newTS().psi(0, patSection(0, 1, tPMT))
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), true, nil, append([]byte{0}, other[:183]...)))
	b.psi(tPMT, pmt)
	evs, st := demuxBytes(t, b.bytes())
	assert.Len(t, pmtsOf(evs), 1)
	assert.Zero(t, st.PSIErrors)

	long := pmtSection(1, 1, tVideo, manyStreams(40)...)
	b.raw(buildPacket(tPMT, b.nextCC(tPMT), true, nil, append([]byte{0}, long[:183]...)))
	b.psi(tPMT, pmt)
	evs, st = demuxBytes(t, b.bytes())
	assert.Len(t, pmtsOf(evs), 1)
	assert.Equal(t, uint64(1), st.PSIErrors)
}

func TestDemuxProgramChangeSamePID(t *testing.T) {
	b := newTS().base()
	b.psi(tPMT, pmtSection(2, 0, tVideo, Stream{tVideo, tH264})) // another program: ignored
	b.psi(0, patSection(1, 2, tPMT))
	b.psi(tPMT, pmtSection(2, 0, tVideo, Stream{tVideo, tH264}))
	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 2)
	assert.Equal(t, uint16(2), pmts[1].Program)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxMalformedPSI(t *testing.T) {
	corruptSyntax := patSection(0, 1, tPMT)
	corruptSyntax[1] &^= 0x80
	tests := []struct {
		name string
		pid  uint16
		pkt  []byte
	}{
		{"pointer beyond payload", 0, esPacket(0, 0, true, 0, []byte{0x05})},
		{"syntax indicator 0", 0, nil},
		{"section too short", 0, nil},
		{"section too long", 0, nil},
		{"PAT loop not a multiple of 4", 0, nil},
		{"PMT program info overflow", tPMT, nil},
		{"PMT ES info overflow", tPMT, nil},
		{"PMT truncated ES entry", tPMT, nil},
		{"PMT without PCR PID", tPMT, nil},
	}
	payloads := map[string][]byte{
		"syntax indicator 0":           corruptSyntax,
		"section too short":            {0x00, 0xB0, 0x05, 0, 1, 0xC1, 0, 0},
		"section too long":             append([]byte{0x00, 0xB3, 0xFE}, make([]byte, 180)...),
		"PAT loop not a multiple of 4": section(0x00, 1, 0, true, []byte{0, 1, 0xF0}),
		"PMT program info overflow":    section(0x02, 1, 0, true, []byte{0xE1, 0x00, 0xF0, 0x09, 1, 2}),
		"PMT ES info overflow":         section(0x02, 1, 0, true, []byte{0xE1, 0x00, 0xF0, 0x00, tH264, 0xE1, 0, 0xF0, 0x05}),
		"PMT truncated ES entry":       section(0x02, 1, 0, true, []byte{0xE1, 0x00, 0xF0, 0x00, tH264, 0xE1}),
		"PMT without PCR PID":          section(0x02, 1, 0, true, []byte{0xE1}),
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTS()
			if tc.pid == tPMT {
				b.psi(0, patSection(0, 1, tPMT))
			}
			if tc.pkt != nil {
				b.raw(tc.pkt)
			} else {
				b.psi(tc.pid, payloads[tc.name])
			}
			evs, st := demuxBytes(t, b.bytes())
			assert.Empty(t, evs)
			assert.Equal(t, uint64(1), st.PSIErrors)
		})
	}
}

func TestDemuxInvalidPIDsFiltered(t *testing.T) {
	// A PAT entry with PMT PID 0x1FFF is skipped; PMT entries on PID 0, the
	// PMT PID, 0x1FFF or a repeated PID are dropped.
	b := newTS().psi(0, patSection(0, 1, 0x1FFF, 2, tPMT))
	b.psi(tPMT, pmtSection(2, 0, 0x1FFF,
		Stream{0, tH264}, Stream{tPMT, tH264}, Stream{0x1FFF, tH264},
		Stream{tVideo, tH264}, Stream{tVideo, tAAC}))
	evs, st := demuxBytes(t, b.bytes())
	pmts := pmtsOf(evs)
	require.Len(t, pmts, 1)
	assert.Equal(t, []Stream{{tVideo, tH264}}, pmts[0].Streams)
	assert.Equal(t, uint16(0x1FFF), pmts[0].PCRPID)
	assert.Zero(t, st.PSIErrors)
}

func TestDemuxMalformedPES(t *testing.T) {
	badFlags := pesBytes(1, -1, []byte{1}, true)
	badFlags[7] = 0x40 // PTS_DTS_flags '01'
	shortHdr := pesBytes(1, -1, []byte{1}, true)
	shortHdr[8] = 2 // header_data_length smaller than the PTS
	overHdr := pesBytes(1, -1, []byte{1}, true)
	overHdr[8] = 200
	badMarker := pesBytes(1, -1, []byte{1}, true)
	badMarker[6] = 0x40
	tooShort := []byte{0, 0, 1, 0xE0, 0, 2, 0x80, 0x80}
	tests := map[string][]byte{
		"start code":            append([]byte{0, 0, 2, 0xE0, 0, 0}, make([]byte, 20)...),
		"PTS_DTS_flags 01":      badFlags,
		"header data too short": shortHdr,
		"header data overflow":  overHdr,
		"marker bits":           badMarker,
		"optional header short": tooShort,
	}
	for name, pes := range tests {
		t.Run(name, func(t *testing.T) {
			b := newTS().base().pes(tVideo, pes, 0).pes(tVideo, pesBytes(7, -1, []byte{7}, true), 0)
			evs, st := demuxBytes(t, b.bytes())
			ps := videoOf(evs)
			require.Len(t, ps, 1)
			assert.Equal(t, int64(7), ps[0].PTS)
			assert.Equal(t, uint64(1), st.PESErrors)
		})
	}
	t.Run("header cut by the next PUSI", func(t *testing.T) {
		b := newTS().base()
		b.raw(esPacket(tVideo, b.nextCC(tVideo), true, 0, []byte{0, 0, 1}))
		b.pes(tVideo, pesBytes(7, -1, []byte{7}, true), 0)
		b.raw(esPacket(tVideo, b.nextCC(tVideo), true, 0, []byte{0, 0, 1, 0xE0}))
		evs, st := demuxBytes(t, b.bytes())
		require.Len(t, videoOf(evs), 1)
		assert.Equal(t, uint64(2), st.PESErrors)
	})
	t.Run("continuation without a start", func(t *testing.T) {
		b := newTS().base()
		b.raw(esPacket(tVideo, 0, false, 0, payloadOf(100, 1)))
		evs, st := demuxBytes(t, b.bytes())
		assert.Len(t, evs, 1)
		assert.Zero(t, st.PESErrors)
	})
}

func TestDemuxMaxPES(t *testing.T) {
	b := newTS().base().
		pes(tVideo, pesBytes(1, -1, payloadOf(5000, 1), false), 0).
		pes(tVideo, pesBytes(2, -1, payloadOf(100, 2), false), 0)
	d := NewDemux(bytes.NewReader(b.bytes()))
	d.maxPES = 1000
	var got []*PES
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if ev.PES != nil {
			got = append(got, ev.PES)
		}
	}
	require.Len(t, got, 1)
	assert.Equal(t, int64(2), got[0].PTS)
	assert.Equal(t, uint64(1), d.Stats().PESErrors)
}

func TestDemuxReadError(t *testing.T) {
	boom := errors.New("boom")
	d := NewDemux(iotest.ErrReader(boom))
	_, err := d.Next()
	require.ErrorIs(t, err, boom)
	_, err = d.Next()
	require.ErrorIs(t, err, boom)

	d = NewDemux(bytes.NewReader(nil))
	_, err = d.Next()
	require.ErrorIs(t, err, io.EOF)
	_, err = d.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestDemuxOneByteReads(t *testing.T) {
	raw := readFixture(t, "h264_aac.ts")
	want, _ := demuxBytes(t, raw)
	got, _, err := demuxAll(t, iotest.OneByteReader(bytes.NewReader(raw)))
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, want, got)
}

// TestDemuxGarbage feeds random bytes and random packets on the stream's PIDs
// after a valid PAT and PMT, and corrupts a fixture; the demuxer must neither
// panic nor hang.
func TestDemuxGarbage(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 200 {
		b := newTS().base()
		for range 40 {
			pkt := make([]byte, PacketSize)
			rng.Read(pkt)
			pkt[0] = 0x47
			switch rng.Intn(4) {
			case 0:
				pkt[1] = pkt[1]&0xE0 | byte(tVideo>>8)
				pkt[2] = byte(tVideo & 0xFF)
			case 1:
				pkt[1] = pkt[1]&0xE0 | byte(tPMT>>8)
				pkt[2] = byte(tPMT & 0xFF)
			case 2:
				pkt[1] &= 0xE0
				pkt[2] = 0
			}
			pkt[1] &^= 0x80
			b.raw(pkt)
		}
		junk := make([]byte, rng.Intn(2000))
		rng.Read(junk)
		b.buf.Write(junk)
		_, _ = demuxBytes(t, b.bytes())
	}

	raw := append([]byte(nil), readFixture(t, "h264_aac.ts")...)
	for range 3000 {
		raw[rng.Intn(len(raw))] = byte(rng.Intn(256))
	}
	_, _ = demuxBytes(t, raw)
}

func FuzzDemux(f *testing.F) {
	f.Add(newTS().base().pes(tVideo, pesBytes(1, 2, payloadOf(300, 1), false), 0x40).bytes())
	f.Add(newTS().base().pes(tAudio, pesBytes(1, -1, payloadOf(300, 1), true), 0).bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDemux(bytes.NewReader(data))
		for i := 0; i < 100_000; i++ {
			if _, err := d.Next(); err != nil {
				return
			}
		}
		t.Fatal("demux does not terminate")
	})
}
