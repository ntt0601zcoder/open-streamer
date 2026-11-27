package h26x

import (
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureAU is one video PES of a fixture: its NAL units and PTS.
type fixtureAU struct {
	nals [][]byte
	pts  int64
}

// fixtureAUs returns the video PES of a fixture in decode order. The
// fixtures carry one access unit per PES.
func fixtureAUs(t *testing.T, name string) []fixtureAU {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	defer f.Close()
	d := mpegts.NewDemux(f)
	var video uint16
	var out []fixtureAU
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				if s.Type == 0x1b || s.Type == 0x24 {
					video = s.PID
				}
			}
			continue
		}
		if ev.PES == nil || ev.PES.PID != video || !ev.PES.HasPTS {
			continue
		}
		out = append(out, fixtureAU{nals: splitAnnexB(ev.PES.Data), pts: ev.PES.PTS})
	}
	require.NotEmpty(t, out, name)
	return out
}

// pocResult is the outcome of POCCounter.POC for one access unit.
type pocResult struct {
	poc       int32
	reset, ok bool
}

func countPOCs(c *POCCounter, aus []fixtureAU) []pocResult {
	out := make([]pocResult, len(aus))
	for i, au := range aus {
		poc, reset, ok := c.POC(au.nals)
		out[i] = pocResult{poc, reset, ok}
	}
	return out
}

// checkLinear asserts that within every POC period (a reset starts one)
// PTS = PTS0 + (POC − POC0)·k with one k for the whole fixture, which implies
// that POC order equals PTS order. It returns the number of periods and
// whether any picture was reordered (a POC below an earlier one of its
// period).
func checkLinear(t *testing.T, aus []fixtureAU, res []pocResult, k int64) (periods int, reordered bool) {
	t.Helper()
	var first int
	maxPOC := int32(-1 << 31)
	for i, r := range res {
		require.True(t, r.ok, "au %d", i)
		if i == 0 || r.reset {
			require.True(t, r.reset, "first au must reset")
			periods++
			first, maxPOC = i, r.poc
			continue
		}
		if r.poc < maxPOC {
			reordered = true
		}
		maxPOC = max(maxPOC, r.poc)
		want := aus[first].pts + int64(r.poc-res[first].poc)*k
		require.Equal(t, want, aus[i].pts, "au %d poc %d (period from au %d poc %d)", i, r.poc, first, res[first].poc)
	}
	return periods, reordered
}

func TestPOCH264FromFixture(t *testing.T) {
	aus := fixtureAUs(t, "h264_aac.ts")
	res := countPOCs(NewPOCCounter(H264, nil), aus)
	// x264 steps POC by 2 per frame: 3600 ticks per frame at 25 fps.
	periods, reordered := checkLinear(t, aus, res, 1800)
	assert.Equal(t, 6, periods, "one period per IDR (6 s, GOP 25)")
	assert.True(t, reordered, "the fixture has B-frames")
}

func TestPOCH265FromFixture(t *testing.T) {
	aus := fixtureAUs(t, "h265_aac51.ts")
	res := countPOCs(NewPOCCounter(H265, nil), aus)
	periods, reordered := checkLinear(t, aus, res, 3600)
	assert.True(t, reordered, "the fixture has B-frames")

	// Count IDRs: only they reset; the CRAs after the first picture do not.
	idrs, rasl := 0, 0
	craPOC := int32(0)
	sawCRA := false
	for i, au := range aus {
		switch vclType265(au.nals) {
		case 19, 20:
			idrs++
		case 21:
			assert.False(t, res[i].reset, "au %d: a CRA after the first picture keeps the POC period", i)
			craPOC, sawCRA = res[i].poc, true
		case 8, 9:
			require.True(t, sawCRA)
			rasl++
			assert.Less(t, res[i].poc, craPOC, "au %d: RASL precedes its CRA in output order", i)
		}
	}
	assert.Equal(t, idrs, periods)
	assert.Positive(t, rasl, "the fixture has RASL pictures")
}

// vclType265 returns the NAL unit type of the first VCL NAL unit, or -1.
func vclType265(au [][]byte) int {
	for _, n := range au {
		if len(n) >= 2 {
			if typ := int(n[0] >> 1 & 0x3f); typ < 32 {
				return typ
			}
		}
	}
	return -1
}

// sps264Opts selects the POC-related fields of a hand-built H.264 SPS
// (Main profile, so ChromaArrayType is 1; log2_max_frame_num and
// log2_max_pic_order_cnt_lsb are both 4).
type sps264Opts struct {
	pocType    uint64
	fields     bool // frame_mbs_only_flag 0
	alwaysZero bool
	nonRef     int64
	topBottom  int64
	offsets    []int64
}

func pocSPS264(o sps264Opts) []byte {
	w := &bitWriter{}
	w.u(8, 77).u(8, 0).u(8, 30) // profile_idc, constraint flags, level_idc
	w.ue(0)                     // seq_parameter_set_id
	w.ue(0)                     // log2_max_frame_num_minus4
	w.ue(o.pocType)
	switch o.pocType {
	case 0:
		w.ue(0) // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		w.flag(o.alwaysZero).se(o.nonRef).se(o.topBottom).ue(uint64(len(o.offsets)))
		for _, v := range o.offsets {
			w.se(v)
		}
	}
	w.ue(4).flag(false).ue(19).ue(14) // max_num_ref_frames, gaps, 320x240
	w.flag(!o.fields)
	if o.fields {
		w.flag(false) // mb_adaptive_frame_field_flag
	}
	w.flag(true).flag(false).flag(false) // direct_8x8_inference, cropping, vui
	return w.nal(0x67)
}

// pps264Opts selects the slice-header-related fields of a hand-built PPS.
type pps264Opts struct {
	bottomPresent bool
	weighted      bool
	bipred        uint64
	redundant     bool
}

func pocPPS264(o pps264Opts) []byte {
	w := &bitWriter{}
	w.ue(0).ue(0)                       // pic_parameter_set_id, seq_parameter_set_id
	w.flag(false).flag(o.bottomPresent) // entropy_coding_mode, bottom_field_pic_order_in_frame_present
	w.ue(0)                             // num_slice_groups_minus1
	w.ue(0).ue(0)                       // num_ref_idx_l0/l1_default_active_minus1
	w.flag(o.weighted).u(2, o.bipred)
	w.se(0).se(0).se(0)
	w.flag(true).flag(false).flag(o.redundant)
	return w.nal(0x68)
}

// slice264Opts describes one hand-built H.264 slice header.
type slice264Opts struct {
	idr       bool
	ref       bool
	sliceType uint64 // 0 P, 1 B, 2 I
	frameNum  uint64
	field     bool
	bottom    bool
	lsb       uint64
	deltaBot  int64
	delta     [2]int64
	numRefL0  *uint64    // num_ref_idx_active_override with this l0 value (l1 0)
	mods      [][]uint64 // list 0 modifications: idc and its value; 3 is appended
	mmco      [][]uint64 // operations with their arguments; 0 is appended
	ue36      bool       // first_mb_in_slice with 36 leading zeros
}

func u64(v uint64) *uint64 { return &v }

// slice264NAL builds the slice NAL unit and returns it with the bit position
// (NAL header included) after dec_ref_pic_marking.
func slice264NAL(s sps264Opts, p pps264Opts, o slice264Opts) (nalu []byte, end int) {
	w := &bitWriter{}
	if o.ue36 {
		w.u(36, 0).u(37, 1) // first_mb_in_slice with 36 leading zeros
	} else {
		w.ue(0) // first_mb_in_slice
	}
	w.ue(o.sliceType).ue(0) // slice_type, pic_parameter_set_id
	w.u(4, o.frameNum)
	if s.fields {
		w.flag(o.field)
		if o.field {
			w.flag(o.bottom)
		}
	}
	if o.idr {
		w.ue(7) // idr_pic_id
	}
	if s.pocType == 0 {
		w.u(4, o.lsb)
		if p.bottomPresent && !o.field {
			w.se(o.deltaBot)
		}
	}
	if s.pocType == 1 && !s.alwaysZero {
		w.se(o.delta[0])
		if p.bottomPresent && !o.field {
			w.se(o.delta[1])
		}
	}
	if p.redundant {
		w.ue(0)
	}
	st := o.sliceType % 5
	if st == 1 {
		w.flag(true) // direct_spatial_mv_pred_flag
	}
	numL0, numL1 := uint64(0), uint64(0)
	if st == 0 || st == 1 || st == 3 {
		w.flag(o.numRefL0 != nil)
		if o.numRefL0 != nil {
			numL0 = *o.numRefL0
			w.ue(numL0)
			if st == 1 {
				w.ue(0)
			}
		}
	}
	if st != 2 && st != 4 {
		w.flag(o.mods != nil)
		for _, m := range o.mods {
			w.ue(m[0]).ue(m[1])
		}
		if o.mods != nil {
			w.ue(3)
		}
		if st == 1 {
			w.flag(false)
		}
	}
	if (p.weighted && (st == 0 || st == 3)) || (p.bipred == 1 && st == 1) {
		w.ue(2).ue(1) // luma and chroma log2 weight denominators
		for i := uint64(0); i <= numL0 && i < 64; i++ {
			w.flag(true).se(3).se(-2)
			w.flag(true).se(1).se(0).se(-1).se(2)
		}
		if st == 1 {
			for i := uint64(0); i <= numL1; i++ {
				w.flag(false).flag(false)
			}
		}
	}
	if o.ref || o.idr {
		if o.idr {
			w.flag(false).flag(false)
		} else {
			w.flag(o.mmco != nil)
			for _, op := range o.mmco {
				for _, v := range op {
					w.ue(v)
				}
			}
			if o.mmco != nil {
				w.ue(0)
			}
		}
	}
	end = w.nbit + 8
	w.se(-3) // slice_qp_delta, as slice data stand-in
	hdr := byte(0x01)
	switch {
	case o.idr:
		hdr = 0x65
	case o.ref:
		hdr = 0x41
	}
	return w.nal(hdr), end
}

func slice264AU(s sps264Opts, p pps264Opts, o slice264Opts) [][]byte {
	n, _ := slice264NAL(s, p, o)
	return [][]byte{n}
}

// newCounter264 returns a counter whose parameter sets hold s and p.
func newCounter264(t *testing.T, s sps264Opts, p pps264Opts) *POCCounter {
	t.Helper()
	ps := &ParamSets{}
	require.NoError(t, ps.Update(H264, pocSPS264(s)))
	require.NoError(t, ps.Update(H264, pocPPS264(p)))
	return NewPOCCounter(H264, ps)
}

func runPOCs(t *testing.T, c *POCCounter, aus [][][]byte) []pocResult {
	t.Helper()
	out := make([]pocResult, len(aus))
	for i, au := range aus {
		poc, reset, ok := c.POC(au)
		out[i] = pocResult{poc, reset, ok}
	}
	return out
}

func TestPOCH264Types(t *testing.T) {
	t.Run("type0 lsb wrap and mmco5", func(t *testing.T) {
		s := sps264Opts{pocType: 0}
		p := pps264Opts{weighted: true, bipred: 1}
		mk := func(o slice264Opts) [][]byte { return slice264AU(s, p, o) }
		// MaxPicOrderCntLsb is 16. The P slices carry a weight table and
		// the B slices list modifications, so mmco 5 is found only when
		// every field before it is walked correctly.
		aus := [][][]byte{
			mk(slice264Opts{idr: true, sliceType: 7}),                       // 0
			mk(slice264Opts{ref: true, sliceType: 5, frameNum: 1, lsb: 6}),  // 6
			mk(slice264Opts{sliceType: 6, frameNum: 2, lsb: 2}),             // 2
			mk(slice264Opts{sliceType: 6, frameNum: 2, lsb: 4}),             // 4
			mk(slice264Opts{ref: true, sliceType: 0, frameNum: 2, lsb: 12}), // 12
			mk(slice264Opts{sliceType: 1, frameNum: 3, lsb: 8}),             // 8
			mk(slice264Opts{sliceType: 1, frameNum: 3, lsb: 10}),            // 10
			mk(slice264Opts{ref: true, sliceType: 0, frameNum: 3, lsb: 2}),  // wraps: 18
			mk(slice264Opts{sliceType: 1, frameNum: 4, lsb: 14}),            // 14
			mk(slice264Opts{sliceType: 1, frameNum: 4, lsb: 0}),             // 16
			mk(slice264Opts{ref: true, sliceType: 0, frameNum: 4, lsb: 8, // 24, then mmco 5
				numRefL0: u64(2), mmco: [][]uint64{{1, 0}, {2, 1}, {3, 2, 0}, {6, 1}, {4, 2}, {5}}}),
			mk(slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 4}), // 4
			mk(slice264Opts{sliceType: 1, frameNum: 2, lsb: 2, // 2
				mods: [][]uint64{{0, 0}}}),
			mk(slice264Opts{ref: true, sliceType: 1, frameNum: 2, lsb: 3, // B reference with mmco 5: 3 → 0
				numRefL0: u64(1), mods: [][]uint64{{1, 2}, {2, 0}}, mmco: [][]uint64{{5}}}),
		}
		want := []pocResult{
			{0, true, true},
			{6, false, true},
			{2, false, true},
			{4, false, true},
			{12, false, true},
			{8, false, true},
			{10, false, true},
			{18, false, true},
			{14, false, true},
			{16, false, true},
			{0, true, true},
			{4, false, true},
			{2, false, true},
			{0, true, true},
		}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})

	t.Run("type0 mmco5 keeps the top field order count", func(t *testing.T) {
		s := sps264Opts{pocType: 0}
		p := pps264Opts{bottomPresent: true}
		mk := func(o slice264Opts) [][]byte { return slice264AU(s, p, o) }
		aus := [][][]byte{
			mk(slice264Opts{idr: true, sliceType: 7}),
			// Top 8, bottom 7: POC 7; after mmco 5 top is 1 and bottom 0.
			mk(slice264Opts{ref: true, frameNum: 1, lsb: 8, deltaBot: -1, mmco: [][]uint64{{5}}}),
			// prevPicOrderCntLsb is the adjusted top (1): lsb 9 is 8 ahead,
			// not more than half the range, so the msb stays 0.
			mk(slice264Opts{ref: true, frameNum: 1, lsb: 9}),
			mk(slice264Opts{frameNum: 2, lsb: 11, deltaBot: -2}), // min(11, 9)
		}
		want := []pocResult{{0, true, true}, {0, true, true}, {9, false, true}, {9, false, true}}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})

	t.Run("type0 field pairs", func(t *testing.T) {
		s := sps264Opts{pocType: 0, fields: true}
		p := pps264Opts{bottomPresent: true}
		field := func(o slice264Opts) []byte {
			o.field = true
			n, _ := slice264NAL(s, p, o)
			return n
		}
		frame := func(o slice264Opts) []byte {
			n, _ := slice264NAL(s, p, o)
			return n
		}
		aus := [][][]byte{
			{field(slice264Opts{idr: true, sliceType: 7}), field(slice264Opts{ref: true, sliceType: 7, bottom: true, lsb: 1})},
			// The first field's POC: top 8 (bottom 9 follows).
			{field(slice264Opts{ref: true, frameNum: 1, lsb: 8}), field(slice264Opts{ref: true, frameNum: 1, bottom: true, lsb: 9})},
			// Bottom field first.
			{field(slice264Opts{frameNum: 2, bottom: true, lsb: 5}), field(slice264Opts{frameNum: 2, lsb: 4})},
			// A frame: lsb 1 is 8 below the second field's lsb 9 (the
			// previous reference picture), so the msb steps to 16; from the
			// first field's lsb 8 it would not. min(top 17, bottom 17 − 1).
			{frame(slice264Opts{ref: true, frameNum: 2, lsb: 1, deltaBot: -1})},
		}
		want := []pocResult{{0, true, true}, {8, false, true}, {5, false, true}, {16, false, true}}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})

	t.Run("type1 negative offsets", func(t *testing.T) {
		// MaxFrameNum 16; cycle [10, 6] (16 per cycle); non-reference −4;
		// top to bottom −1, so a frame's POC is its bottom field's.
		s := sps264Opts{pocType: 1, nonRef: -4, topBottom: -1, offsets: []int64{10, 6}}
		p := pps264Opts{}
		mk := func(o slice264Opts) [][]byte { return slice264AU(s, p, o) }
		aus := [][][]byte{
			mk(slice264Opts{idr: true, sliceType: 7, delta: [2]int64{1, 0}}), // top 1, bottom 0
			mk(slice264Opts{ref: true, frameNum: 1}),                         // abs 1: 10 → 9
			mk(slice264Opts{sliceType: 1, frameNum: 2}),                      // abs 2→1: 10−4 → 5
			mk(slice264Opts{ref: true, frameNum: 2, delta: [2]int64{-3, 0}}), // abs 2: 16−3 → 12
			mk(slice264Opts{ref: true, frameNum: 15}),                        // abs 15: 7·16+10 → 121
			mk(slice264Opts{ref: true, frameNum: 0}),                         // FrameNumOffset 16, abs 16: 7·16+16 → 127
			mk(slice264Opts{sliceType: 1, frameNum: 1}),                      // abs 17→16: 128−4 → 123
		}
		want := []pocResult{
			{0, true, true},
			{9, false, true},
			{5, false, true},
			{12, false, true},
			{121, false, true},
			{127, false, true},
			{123, false, true},
		}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})

	t.Run("type1 fields and delta always zero", func(t *testing.T) {
		s := sps264Opts{pocType: 1, fields: true, alwaysZero: true, nonRef: -4, topBottom: 3, offsets: []int64{10, 6}}
		p := pps264Opts{}
		field := func(o slice264Opts) []byte {
			o.field = true
			n, _ := slice264NAL(s, p, o)
			return n
		}
		aus := [][][]byte{
			{field(slice264Opts{idr: true, sliceType: 7}), field(slice264Opts{ref: true, sliceType: 7, bottom: true})},
			{field(slice264Opts{ref: true, frameNum: 1, bottom: true})}, // expected 10, bottom: 10+3
			{field(slice264Opts{frameNum: 2})},                          // abs 1: 10−4, top: 6
		}
		want := []pocResult{{0, true, true}, {13, false, true}, {6, false, true}}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})

	t.Run("type2 non-reference pictures", func(t *testing.T) {
		s := sps264Opts{pocType: 2}
		p := pps264Opts{}
		mk := func(o slice264Opts) [][]byte { return slice264AU(s, p, o) }
		aus := [][][]byte{
			mk(slice264Opts{idr: true, sliceType: 7}),
			mk(slice264Opts{ref: true, frameNum: 1}),                        // 2
			mk(slice264Opts{frameNum: 2}),                                   // 3
			mk(slice264Opts{ref: true, frameNum: 2}),                        // 4
			mk(slice264Opts{frameNum: 3}),                                   // 5
			mk(slice264Opts{ref: true, frameNum: 15}),                       // 30
			mk(slice264Opts{ref: true, frameNum: 0}),                        // wraps: 32
			mk(slice264Opts{frameNum: 1}),                                   // 33
			mk(slice264Opts{ref: true, frameNum: 1, mmco: [][]uint64{{5}}}), // 34 → 0
			mk(slice264Opts{ref: true, frameNum: 1}),                        // frame_num restarts from 0: 2
			mk(slice264Opts{frameNum: 2}),                                   // 3
		}
		want := []pocResult{
			{0, true, true},
			{2, false, true},
			{3, false, true},
			{4, false, true},
			{5, false, true},
			{30, false, true},
			{32, false, true},
			{33, false, true},
			{0, true, true},
			{2, false, true},
			{3, false, true},
		}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})

	t.Run("start at a non-IDR picture", func(t *testing.T) {
		s := sps264Opts{pocType: 0}
		p := pps264Opts{}
		mk := func(o slice264Opts) [][]byte { return slice264AU(s, p, o) }
		aus := [][][]byte{
			mk(slice264Opts{sliceType: 1, frameNum: 5, lsb: 10}),            // no reference picture yet
			mk(slice264Opts{ref: true, sliceType: 0, frameNum: 5, lsb: 14}), // relative start: msb 0
			mk(slice264Opts{sliceType: 1, frameNum: 6, lsb: 12}),
			mk(slice264Opts{ref: true, sliceType: 0, frameNum: 6, lsb: 2}), // wraps: 18
		}
		want := []pocResult{{0, false, false}, {14, true, true}, {12, false, true}, {18, false, true}}
		assert.Equal(t, want, runPOCs(t, newCounter264(t, s, p), aus))
	})
}

// slice265NAL builds the first slice segment of an H.265 picture for the
// parameter sets s and p.
func slice265NAL(s *SPS, p *PPS, typ, tid uint8, lsb uint64) []byte {
	w := &bitWriter{}
	w.flag(true) // first_slice_segment_in_pic_flag
	if typ >= 16 && typ <= 23 {
		w.flag(false) // no_output_of_prior_pics_flag
	}
	w.ue(uint64(p.ID))
	w.u(int(p.NumExtraSliceHeaderBits), 0)
	w.ue(1) // slice_type P
	if p.OutputFlagPresent {
		w.flag(true)
	}
	if s.SeparateColourPlane {
		w.u(2, 0)
	}
	if typ != 19 && typ != 20 {
		w.u(int(s.Log2MaxPicOrderCntLsb), lsb)
	}
	w.flag(false).ue(0) // slice data stand-in
	return w.nal(typ<<1, tid+1)
}

// fixtureParams265 returns parameter sets loaded from h265_aac51.ts.
func fixtureParams265(t *testing.T) *ParamSets {
	t.Helper()
	sps, pps := fixtureSets(t, "h265_aac51.ts", H265)
	ps := &ParamSets{}
	require.NoError(t, ps.Update(H265, sps[0]))
	require.NoError(t, ps.Update(H265, pps[0]))
	return ps
}

func TestPOCH265Rules(t *testing.T) {
	ps := fixtureParams265(t)
	pps := ps.PPS(0)
	require.NotNil(t, pps)
	sps := ps.SPS(pps.SPSID)
	require.Equal(t, uint8(8), sps.Log2MaxPicOrderCntLsb, "the expected values assume MaxPicOrderCntLsb 256")
	mk := func(typ, tid uint8, lsb uint64) [][]byte { return [][]byte{slice265NAL(sps, pps, typ, tid, lsb)} }
	eos := []byte{36 << 1, 1}
	// Steps near half the lsb range tell which earlier picture is
	// prevTid0Pic.
	aus := [][][]byte{
		mk(1, 0, 40),   // TRAIL_R: relative start, 40
		mk(0, 0, 38),   // TRAIL_N: 38, a sub-layer non-reference picture
		mk(1, 0, 167),  // from 40 (from 38 it would be −89): 167
		mk(1, 0, 250),  // 250
		mk(1, 0, 4),    // wraps: 260
		mk(9, 0, 250),  // RASL_R: 250, a leading picture
		mk(1, 0, 125),  // from 260 (from 250 it would be 125): 381
		mk(1, 1, 240),  // TemporalId 1: 496
		mk(1, 0, 254),  // from 381 (from 496 it would be 510): 254
		mk(21, 0, 140), // CRA after the first picture: no reset, 140
		append(mk(1, 0, 150), eos),
		mk(21, 0, 30), // CRA after end of sequence: reset, 30
		mk(1, 0, 31),  // 31
		mk(16, 0, 70), // BLA_W_LP: reset, 70
		mk(20, 0, 0),  // IDR_N_LP: reset, 0
		mk(1, 0, 255), // more than half the range ahead: −1
	}
	want := []pocResult{
		{40, true, true},
		{38, false, true},
		{167, false, true},
		{250, false, true},
		{260, false, true},
		{250, false, true},
		{381, false, true},
		{496, false, true},
		{254, false, true},
		{140, false, true},
		{150, false, true},
		{30, true, true},
		{31, false, true},
		{70, true, true},
		{0, true, true},
		{-1, false, true},
	}
	assert.Equal(t, want, runPOCs(t, NewPOCCounter(H265, ps), aus))

	// The first picture must be a TemporalId 0 reference picture that is not
	// a leading picture.
	c := NewPOCCounter(H265, ps)
	for _, au := range [][][]byte{mk(0, 0, 3), mk(1, 1, 3), mk(9, 0, 3), mk(7, 0, 3)} {
		_, _, ok := c.POC(au)
		assert.False(t, ok)
	}
	poc, reset, ok := c.POC(mk(21, 0, 9))
	assert.Equal(t, pocResult{9, true, true}, pocResult{poc, reset, ok}, "the first CRA resets")
}

func TestPOCMissingParams(t *testing.T) {
	s := sps264Opts{pocType: 0}
	p := pps264Opts{}
	idr, _ := slice264NAL(s, p, slice264Opts{idr: true, sliceType: 7})

	c := NewPOCCounter(H264, nil)
	_, _, ok := c.POC([][]byte{idr})
	assert.False(t, ok, "no parameter sets")

	ps := &ParamSets{}
	require.NoError(t, ps.Update(H264, pocSPS264(s)))
	c = NewPOCCounter(H264, ps)
	_, _, ok = c.POC([][]byte{idr})
	assert.False(t, ok, "no PPS")
	_, _, ok = c.POC([][]byte{{0x09, 0xf0}})
	assert.False(t, ok, "no slice")
	_, _, ok = c.POC(nil)
	assert.False(t, ok, "empty access unit")

	// In-band parameter sets before the slice are stored and used.
	poc, reset, ok := c.POC([][]byte{{0x09, 0xf0}, pocPPS264(p), idr})
	assert.Equal(t, pocResult{0, true, true}, pocResult{poc, reset, ok})
	assert.NotNil(t, ps.PPS(0))

	// H.265: unknown PPS id, then a codec mismatch.
	ps265 := fixtureParams265(t)
	pps := ps265.PPS(0)
	sps := ps265.SPS(pps.SPSID)
	other := *pps
	other.ID = 9
	c = NewPOCCounter(H265, ps265)
	_, _, ok = c.POC([][]byte{slice265NAL(sps, &other, 20, 0, 0)})
	assert.False(t, ok, "unknown PPS id")
	c = NewPOCCounter(H264, ps265)
	_, _, ok = c.POC([][]byte{slice265NAL(sps, pps, 20, 0, 0)})
	assert.False(t, ok, "parameter sets of another codec")
	c = NewPOCCounter(Codec(9), ps265)
	_, _, ok = c.POC([][]byte{slice265NAL(sps, pps, 20, 0, 0)})
	assert.False(t, ok, "unknown codec")
}

// compareFromStart asserts that the POC differences of part, a counter
// started at some later point of a stream, equal those of full, a counter
// started at its beginning, for the access units of part's first period
// (from its first dated picture to the next reset of either). key matches
// access units between the two runs. It returns the number compared.
func compareFromStart(t *testing.T, fullAUs []fixtureAU, full []pocResult, partAUs []fixtureAU, part []pocResult) int {
	t.Helper()
	byPTS := map[int64]int{}
	for i, au := range fullAUs {
		byPTS[au.pts] = i
	}
	first := -1
	for i, r := range part {
		if !r.ok {
			require.Equal(t, -1, first, "au %d: ok=false after the counter started", i)
			continue
		}
		j, found := byPTS[partAUs[i].pts]
		require.True(t, found)
		if first < 0 {
			require.True(t, r.reset, "the first dated picture starts a period")
			first = i
			continue
		}
		if r.reset || full[j].reset {
			return i - first
		}
		f := byPTS[partAUs[first].pts]
		assert.Equal(t, full[j].poc-full[f].poc, r.poc-part[first].poc, "au %d (pts %d)", i, partAUs[i].pts)
	}
	return len(part) - first
}

func TestPOCStartMidStream(t *testing.T) {
	t.Run("h264 open GOP", func(t *testing.T) {
		fullAUs := fixtureAUs(t, "h264_opengop.ts")
		full := countPOCs(NewPOCCounter(H264, nil), fullAUs)
		partAUs := fixtureAUs(t, "h264_opengop_midgop.ts")
		part := countPOCs(NewPOCCounter(H264, nil), partAUs)
		require.False(t, part[0].ok, "the cut starts without parameter sets")
		start := 0
		for !part[start].ok {
			start++
		}
		typ := -1
		for _, n := range partAUs[start].nals {
			if nt := n[0] & 0x1f; nt == 1 || nt == 5 {
				typ = int(nt)
				break
			}
		}
		assert.Equal(t, 1, typ, "the counter starts at a non-IDR picture")
		assert.GreaterOrEqual(t, compareFromStart(t, fullAUs, full, partAUs, part), 20)
	})

	t.Run("h265 at a CRA and at a trailing picture", func(t *testing.T) {
		aus := fixtureAUs(t, "h265_aac51.ts")
		full := countPOCs(NewPOCCounter(H265, nil), aus)
		cras := 0
		for i, au := range aus {
			if vclType265(au.nals) != 21 {
				continue
			}
			cras++
			if cras != 2 {
				continue
			}
			for _, from := range []int{i, i + 5} {
				part := countPOCs(NewPOCCounter(H265, fixtureParams265(t)), aus[from:])
				assert.GreaterOrEqual(t, compareFromStart(t, aus, full, aus[from:], part), 20, "from au %d", from)
			}
		}
		require.GreaterOrEqual(t, cras, 2)
	})
}

func TestPOCHostile(t *testing.T) {
	s := sps264Opts{pocType: 0}
	p := pps264Opts{weighted: true, bipred: 1, redundant: true}
	idr := slice264AU(s, p, slice264Opts{idr: true, sliceType: 7})
	rich := slice264Opts{
		ref: true, sliceType: 0, frameNum: 1, lsb: 6, numRefL0: u64(3),
		mods: [][]uint64{{0, 1}, {2, 0}}, mmco: [][]uint64{{1, 0}, {3, 1, 0}},
	}

	cases := map[string]struct {
		o    slice264Opts
		want string
	}{
		"num_ref_idx 2^20":   {slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 6, numRefL0: u64(1 << 20)}, "num_ref_idx_l0_active_minus1"},
		"endless mmco":       {slice264Opts{ref: true, sliceType: 2, frameNum: 1, lsb: 6, mmco: repeatOp([]uint64{1, 0}, 1000)}, "memory_management_control_operation"},
		"endless list mods":  {slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 6, mods: repeatOp([]uint64{0, 0}, 100)}, "ref_pic_list_modification"},
		"one list mod over":  {slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 6, numRefL0: u64(1), mods: repeatOp([]uint64{0, 0}, 3)}, "ref_pic_list_modification"},
		"36 leading zeros":   {slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 6, ue36: true}, "exp-Golomb"},
		"slice_type 10":      {slice264Opts{ref: true, sliceType: 10, frameNum: 1, lsb: 6}, "slice_type"},
		"mmco 7":             {slice264Opts{ref: true, sliceType: 2, frameNum: 1, lsb: 6, mmco: [][]uint64{{7}}}, "memory_management_control_operation"},
		"modification idc 4": {slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 6, mods: [][]uint64{{4, 0}}}, "modification_of_pic_nums_idc"},
	}
	ps := &ParamSets{}
	require.NoError(t, ps.Update(H264, pocSPS264(s)))
	require.NoError(t, ps.Update(H264, pocPPS264(p)))
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n, _ := slice264NAL(s, p, tc.o)
			_, _, err := parseSlice264(n, ps)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			c := NewPOCCounter(H264, ps)
			_, _, ok := c.POC(idr)
			require.True(t, ok)
			_, _, ok = c.POC([][]byte{n})
			assert.False(t, ok)
		})
	}

	t.Run("truncated at every byte", func(t *testing.T) {
		n, end := slice264NAL(s, p, rich)
		for cut := 0; cut <= len(n); cut++ {
			c := NewPOCCounter(H264, ps)
			_, _, ok := c.POC(idr)
			require.True(t, ok)
			poc, _, ok := c.POC([][]byte{n[:cut]})
			complete := len(unescape(n[:cut]))*8 >= end
			assert.Equal(t, complete, ok, "cut %d of %d (header ends at bit %d)", cut, len(n), end)
			// A failed picture leaves the state as it was.
			poc2, _, ok2 := c.POC([][]byte{n})
			require.True(t, ok2)
			assert.Equal(t, int32(6), poc2)
			if ok {
				assert.Equal(t, int32(6), poc)
			}
		}
	})

	t.Run("h265 truncated and malformed", func(t *testing.T) {
		ps265 := fixtureParams265(t)
		pps := ps265.PPS(0)
		sps := ps265.SPS(pps.SPSID)
		n := slice265NAL(sps, pps, 1, 0, 77)
		for cut := 0; cut < len(n)-1; cut++ {
			c := NewPOCCounter(H265, ps265)
			_, _, ok := c.POC([][]byte{n[:cut]})
			assert.False(t, ok, "cut %d", cut)
		}
		bad := append([]byte{}, n...)
		bad[1] &^= 7 // nuh_temporal_id_plus1 0
		_, _, ok := NewPOCCounter(H265, ps265).POC([][]byte{bad})
		assert.False(t, ok, "nuh_temporal_id_plus1 0")
		layer := append([]byte{}, n...)
		layer[1] |= 1 << 3 // nuh_layer_id 1: not the base layer
		_, _, ok = NewPOCCounter(H265, ps265).POC([][]byte{layer})
		assert.False(t, ok, "only base-layer slices count")

		// A dependent slice segment (first_slice_segment_in_pic_flag 0)
		// with an address beyond the picture.
		w := &bitWriter{}
		w.flag(false).ue(uint64(pps.ID))
		if pps.DependentSliceSegmentsEnabled {
			w.flag(false)
		}
		bits := ceilLog2(sps.PicSizeInCtbsY())
		require.GreaterOrEqual(t, uint32(1)<<bits-1, sps.PicSizeInCtbsY())
		w.u(bits, 1<<bits-1)
		w.u(int(pps.NumExtraSliceHeaderBits), 0).ue(1)
		if pps.OutputFlagPresent {
			w.flag(true)
		}
		w.u(int(sps.Log2MaxPicOrderCntLsb), 5)
		_, _, err := parseSlice265(w.nal(1<<1, 1), ps265)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "slice_segment_address")
	})

	t.Run("random access units", func(t *testing.T) {
		rng := rand.New(rand.NewSource(1))
		ps265 := fixtureParams265(t)
		n264, _ := slice264NAL(s, p, rich)
		n265 := slice265NAL(ps265.SPS(0), ps265.PPS(0), 1, 0, 77)
		c264, c265 := NewPOCCounter(H264, ps), NewPOCCounter(H265, ps265)
		for range 20000 {
			a := append([]byte{}, n264...)
			b := append([]byte{}, n265...)
			for range 1 + rng.Intn(4) {
				a[rng.Intn(len(a))] ^= byte(1 << rng.Intn(8))
				b[rng.Intn(len(b))] ^= byte(1 << rng.Intn(8))
			}
			c264.POC([][]byte{a[:rng.Intn(len(a)+1)]})
			c265.POC([][]byte{b[:rng.Intn(len(b)+1)]})
		}
	})
}

func repeatOp(op []uint64, n int) [][]uint64 {
	out := make([][]uint64, n)
	for i := range out {
		out[i] = op
	}
	return out
}

func FuzzPOC(f *testing.F) {
	s := sps264Opts{pocType: 0}
	p := pps264Opts{weighted: true, bipred: 1}
	n, _ := slice264NAL(s, p, slice264Opts{ref: true, sliceType: 0, frameNum: 1, lsb: 6, numRefL0: u64(1), mmco: [][]uint64{{5}}})
	f.Add(uint8(H264), n)
	f.Add(uint8(H264), pocSPS264(sps264Opts{pocType: 1, offsets: []int64{3, -1}}))
	f.Add(uint8(H265), []byte{0x2a, 0x01, 0xa0})
	f.Fuzz(func(t *testing.T, c uint8, n []byte) {
		codec := Codec(c%2 + 1)
		ps := &ParamSets{}
		if codec == H264 {
			_ = ps.Update(H264, pocSPS264(s))
			_ = ps.Update(H264, pocPPS264(p))
		}
		pc := NewPOCCounter(codec, ps)
		pc.POC([][]byte{n})
		pc.POC([][]byte{n, n})
	})
}
