package h26x

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBitReaderExpGolomb(t *testing.T) {
	// 31 leading zeros: the largest code that fits 32 bits.
	w := (&bitWriter{}).u(31, 0).u(1, 1).u(31, 0x7fffffff)
	r := &bitReader{buf: w.buf}
	assert.Equal(t, uint32(0xfffffffe), r.ue())
	require.NoError(t, r.err)

	w = (&bitWriter{}).u(32, 0).u(1, 1).u(32, 0)
	r = &bitReader{buf: w.buf}
	r.ue()
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "exp-Golomb")

	w = (&bitWriter{}).se(-5).se(7).se(0).ue(0).ue(254)
	r = &bitReader{buf: w.buf}
	assert.Equal(t, int32(-5), r.se())
	assert.Equal(t, int32(7), r.se())
	assert.Equal(t, int32(0), r.se())
	assert.Equal(t, uint32(0), r.ue())
	assert.Equal(t, uint32(254), r.ue())
	require.NoError(t, r.err)

	r = &bitReader{buf: []byte{0xff}}
	assert.Equal(t, uint32(0xff), r.u(8))
	r.u(1)
	require.ErrorIs(t, r.err, errTruncated)
}

func TestUnescape(t *testing.T) {
	assert.Equal(t, []byte{0, 0, 1, 0, 0, 0, 0, 0}, unescape([]byte{0, 0, 3, 1, 0, 0, 3, 0, 0, 0, 3}))
	in := []byte{1, 2}
	out := unescape(in)
	out[0] = 9
	assert.Equal(t, byte(1), in[0], "unescape returns a fresh slice")
}

func TestParseSPSFixtures(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h264_opengop.ts"} {
		t.Run(name, func(t *testing.T) {
			spss, ppss := fixtureSets(t, name, H264)
			sets := map[uint32]*SPS{}
			mpSets := map[uint32]*avc.SPS{}
			for _, n := range spss {
				s, err := ParseSPS(H264, n)
				require.NoError(t, err)
				want, err := avc.ParseSPSNALUnit(n, true)
				require.NoError(t, err)
				assert.Equal(t, H264, s.Codec)
				assert.Equal(t, want.ParameterID, s.ID)
				assert.Equal(t, want.Profile, uint32(s.ProfileIDC))
				assert.Equal(t, want.ProfileCompatibility, uint32(s.ConstraintFlags))
				assert.Equal(t, want.Level, uint32(s.LevelIDC))
				assert.Equal(t, want.ChromaFormatIDC, s.ChromaFormatIDC)
				assert.Equal(t, want.SeparateColourPlaneFlag, s.SeparateColourPlane)
				assert.Equal(t, want.Log2MaxFrameNumMinus4+4, uint(s.Log2MaxFrameNum))
				assert.Equal(t, want.PicOrderCntType, uint(s.PicOrderCntType))
				assert.Equal(t, want.Log2MaxPicOrderCntLsbMinus4+4, uint(s.Log2MaxPicOrderCntLsb))
				assert.Equal(t, want.NumRefFrames, uint(s.MaxNumRefFrames))
				assert.Equal(t, want.FrameMbsOnlyFlag, s.FrameMbsOnly)
				assert.Equal(t, want.Width, uint(s.Width))
				assert.Equal(t, want.Height, uint(s.Height))
				require.NotNil(t, want.VUI)
				assert.Equal(t, int(want.VUI.MaxNumReorderFrames), s.ReorderFrames)
				assert.Equal(t, 2, s.ReorderFrames)
				// x264 at 25 fps: num_units_in_tick 1, time_scale 50.
				assert.Equal(t, int64(180000*want.VUI.NumUnitsInTick/want.VUI.TimeScale), s.FrameDuration)
				assert.Equal(t, int64(3600), s.FrameDuration)
				sets[s.ID], mpSets[s.ID] = s, want
			}
			for _, n := range ppss {
				p, err := ParsePPS(H264, n, sets)
				require.NoError(t, err)
				want, err := avc.ParsePPSNALUnit(n, mpSets)
				require.NoError(t, err)
				assert.Equal(t, want.PicParameterSetID, p.ID)
				assert.Equal(t, want.SeqParameterSetID, p.SPSID)
				assert.Equal(t, want.EntropyCodingModeFlag, p.EntropyCodingMode)
				assert.Equal(t, want.BottomFieldPicOrderInFramePresentFlag, p.BottomFieldPicOrderInFramePresent)
				assert.Equal(t, want.NumRefIdxI0DefaultActiveMinus1, uint(p.NumRefIdxL0DefaultActiveMinus1))
				assert.Equal(t, want.NumRefIdxI1DefaultActiveMinus1, uint(p.NumRefIdxL1DefaultActiveMinus1))
				assert.Equal(t, want.WeightedPredFlag, p.WeightedPred)
				assert.Equal(t, want.WeightedBipredIDC, uint(p.WeightedBipredIDC))
				assert.Equal(t, want.RedundantPicCntPresentFlag, p.RedundantPicCntPresent)
				assert.Equal(t, want.DeblockingFilterControlPresentFlag, p.DeblockingFilterControlPresent)
				assert.Equal(t, want.Transform8x8ModeFlag, p.Transform8x8Mode)
				// Values from ffmpeg's trace_headers.
				assert.Equal(t, uint32(2), p.NumRefIdxL0DefaultActiveMinus1)
				assert.True(t, p.WeightedPred)
				assert.Equal(t, uint8(2), p.WeightedBipredIDC)
			}
		})
	}

	t.Run("h265_aac51.ts", func(t *testing.T) {
		spss, ppss := fixtureSets(t, "h265_aac51.ts", H265)
		sets := map[uint32]*SPS{}
		mpSets := map[uint32]*hevc.SPS{}
		for _, n := range spss {
			s, err := ParseSPS(H265, n)
			require.NoError(t, err)
			want, err := hevc.ParseSPSNALUnit(n)
			require.NoError(t, err)
			assert.Equal(t, H265, s.Codec)
			assert.Equal(t, uint32(want.SpsID), s.ID)
			assert.Equal(t, want.VpsID, s.VPSID)
			assert.Equal(t, want.ProfileTierLevel.GeneralProfileIDC, s.ProfileIDC)
			assert.Equal(t, want.ProfileTierLevel.GeneralLevelIDC, s.LevelIDC)
			assert.Equal(t, want.MaxSubLayersMinus1+1, s.MaxSubLayers)
			assert.Equal(t, want.ChromaFormatIDC, s.ChromaFormatIDC)
			assert.Equal(t, want.SeparateColourPlaneFlag, s.SeparateColourPlane)
			assert.Equal(t, want.PicWidthInLumaSamples, s.PicWidthInLumaSamples)
			assert.Equal(t, want.PicHeightInLumaSamples, s.PicHeightInLumaSamples)
			w, h := want.ImageSize()
			assert.Equal(t, int(w), s.Width)
			assert.Equal(t, int(h), s.Height)
			assert.Equal(t, want.Log2MaxPicOrderCntLsbMinus4+4, s.Log2MaxPicOrderCntLsb)
			assert.Equal(t, want.Log2MinLumaCodingBlockSizeMinus3+3, s.Log2MinCbSize)
			assert.Equal(t, want.Log2MinLumaCodingBlockSizeMinus3+3+want.Log2DiffMaxMinLumaCodingBlockSize, s.Log2CtbSize)
			last := want.SubLayeringOrderingInfos[len(want.SubLayeringOrderingInfos)-1]
			assert.Equal(t, int(last.MaxNumReorderPics), s.ReorderFrames)
			assert.Equal(t, uint32(last.MaxDecPicBufferingMinus1), s.MaxDecPicBufferingMinus1)
			assert.Equal(t, 2, s.ReorderFrames)
			assert.Equal(t, int(want.NumShortTermRefPicSets), s.NumShortTermRefPicSets())
			assert.Equal(t, want.LongTermRefPicsPresentFlag, s.LongTermRefPicsPresent)
			assert.Equal(t, want.SpsTemporalMvpEnabledFlag, s.TemporalMVPEnabled)
			assert.Equal(t, want.SampleAdaptiveOffsetEnabledFlag, s.SampleAdaptiveOffsetEnabled)
			require.NotNil(t, want.VUI)
			// x265 at 25 fps: num_units_in_tick 1, time_scale 25.
			assert.Equal(t, int64(90000*want.VUI.NumUnitsInTick/want.VUI.TimeScale), s.FrameDuration)
			assert.Equal(t, int64(3600), s.FrameDuration)
			sets[s.ID], mpSets[s.ID] = s, want
		}
		for _, n := range ppss {
			p, err := ParsePPS(H265, n, sets)
			require.NoError(t, err)
			want, err := hevc.ParsePPSNALUnit(n, mpSets)
			require.NoError(t, err)
			assertPPS265(t, want, p)
		}
	})
}

func assertPPS265(t *testing.T, want *hevc.PPS, p *PPS) {
	t.Helper()
	assert.Equal(t, H265, p.Codec)
	assert.Equal(t, want.PicParameterSetID, p.ID)
	assert.Equal(t, want.SeqParameterSetID, p.SPSID)
	assert.Equal(t, want.DependentSliceSegmentsEnabledFlag, p.DependentSliceSegmentsEnabled)
	assert.Equal(t, want.OutputFlagPresentFlag, p.OutputFlagPresent)
	assert.Equal(t, want.NumExtraSliceHeaderBits, p.NumExtraSliceHeaderBits)
	assert.Equal(t, want.SignDataHidingEnabledFlag, p.SignDataHidingEnabled)
	assert.Equal(t, want.CabacInitPresentFlag, p.CabacInitPresent)
	assert.Equal(t, uint32(want.NumRefIdxL0DefaultActiveMinus1), p.NumRefIdxL0DefaultActiveMinus1)
	assert.Equal(t, uint32(want.NumRefIdxL1DefaultActiveMinus1), p.NumRefIdxL1DefaultActiveMinus1)
	assert.Equal(t, want.CuQpDeltaEnabledFlag, p.CuQpDeltaEnabled)
	assert.Equal(t, want.SliceChromaQpOffsetsPresentFlag, p.SliceChromaQpOffsetsPresent)
	assert.Equal(t, want.WeightedPredFlag, p.WeightedPred)
	assert.Equal(t, want.WeightedBipredFlag, p.WeightedBipred)
	assert.Equal(t, want.TilesEnabledFlag, p.TilesEnabled)
	assert.Equal(t, want.EntropyCodingSyncEnabledFlag, p.EntropyCodingSyncEnabled)
	assert.Equal(t, want.NumTileColumnsMinus1, uint(p.NumTileColumnsMinus1))
	assert.Equal(t, want.NumTileRowsMinus1, uint(p.NumTileRowsMinus1))
	assert.Equal(t, want.LoopFilterAcrossSlicesEnabledFlag, p.LoopFilterAcrossSlicesEnabled)
	assert.Equal(t, want.DeblockingFilterControlPresentFlag, p.DeblockingFilterControlPresent)
	assert.Equal(t, want.DeblockingFilterOverrideEnabledFlag, p.DeblockingFilterOverrideEnabled)
	assert.Equal(t, want.DeblockingFilterDisabledFlag, p.DeblockingFilterDisabled)
	assert.Equal(t, want.ListsModificationPresentFlag, p.ListsModificationPresent)
	assert.Equal(t, want.SliceSegmentHeaderExtensionPresentFlag, p.SliceSegmentHeaderExtensionPresent)
}

// h264POC1SPS is a Main-profile SPS with pic_order_cnt_type 1 and no VUI.
func h264POC1SPS(cycle uint64, offsets []int64) []byte {
	w := &bitWriter{}
	w.u(8, 77).u(8, 0x40).u(8, 30) // profile_idc, constraint flags, level_idc
	w.ue(3)                        // seq_parameter_set_id
	w.ue(2)                        // log2_max_frame_num_minus4
	w.ue(1)                        // pic_order_cnt_type
	w.flag(false)                  // delta_pic_order_always_zero_flag
	w.se(-3)                       // offset_for_non_ref_pic
	w.se(-1)                       // offset_for_top_to_bottom_field
	w.ue(cycle)                    // num_ref_frames_in_pic_order_cnt_cycle
	for _, o := range offsets {
		w.se(o) // offset_for_ref_frame[i]
	}
	w.ue(4)       // max_num_ref_frames
	w.flag(false) // gaps_in_frame_num_value_allowed_flag
	w.ue(19)      // pic_width_in_mbs_minus1: 320
	w.ue(14)      // pic_height_in_map_units_minus1: 240
	w.flag(true)  // frame_mbs_only_flag
	w.flag(true)  // direct_8x8_inference_flag
	w.flag(false) // frame_cropping_flag
	w.flag(false) // vui_parameters_present_flag
	return w.nal(0x67)
}

func TestParseSPSSignedOffsets(t *testing.T) {
	s, err := ParseSPS(H264, h264POC1SPS(3, []int64{2, -4, 7}))
	require.NoError(t, err)
	assert.Equal(t, uint32(3), s.ID)
	assert.Equal(t, uint8(1), s.PicOrderCntType)
	assert.Equal(t, uint8(6), s.Log2MaxFrameNum)
	assert.False(t, s.DeltaPicOrderAlwaysZero)
	assert.Equal(t, int32(-3), s.OffsetForNonRefPic)
	assert.Equal(t, int32(-1), s.OffsetForTopToBottomField)
	assert.Equal(t, []int32{2, -4, 7}, s.OffsetForRefFrame)
	assert.Equal(t, uint32(4), s.MaxNumRefFrames)
	assert.Equal(t, 320, s.Width)
	assert.Equal(t, 240, s.Height)
	assert.Equal(t, -1, s.ReorderFrames)
	assert.Equal(t, int64(0), s.FrameDuration)
	r, assumed := ReorderOf(H264, s)
	assert.Equal(t, 2, r)
	assert.True(t, assumed)
}

// h264VUISPS is a High-profile 1280x720 SPS with scaling lists, cropping
// and a VUI carrying timing, NAL and VCL HRD and bitstream_restriction. end
// is the bit position, NAL header included, after max_dec_frame_buffering.
func h264VUISPS() (nalu []byte, end int) {
	w := &bitWriter{}
	w.u(8, 100).u(8, 0).u(8, 31)
	w.ue(0)       // seq_parameter_set_id
	w.ue(1)       // chroma_format_idc
	w.ue(0).ue(0) // bit depths
	w.flag(false) // qpprime_y_zero_transform_bypass_flag
	w.flag(true)  // seq_scaling_matrix_present_flag
	for i := 0; i < 8; i++ {
		switch i {
		case 0: // explicit 4x4 list
			w.flag(true)
			for j := 0; j < 16; j++ {
				w.se(int64(j%5) - 2)
			}
		case 1: // useDefaultScalingMatrix: delta -8 on the first coefficient
			w.flag(true).se(-8)
		case 6: // 8x8 list that stops early: nextScale reaches 0 at j=3
			w.flag(true).se(1).se(1).se(1).se(-11)
		default:
			w.flag(false)
		}
	}
	w.ue(0)                              // log2_max_frame_num_minus4
	w.ue(0)                              // pic_order_cnt_type
	w.ue(2)                              // log2_max_pic_order_cnt_lsb_minus4
	w.ue(4)                              // max_num_ref_frames
	w.flag(false)                        // gaps
	w.ue(79).ue(44)                      // 1280x720
	w.flag(true)                         // frame_mbs_only_flag
	w.flag(true)                         // direct_8x8_inference_flag
	w.flag(true).ue(0).ue(0).ue(0).ue(4) // cropping: bottom 8 rows
	w.flag(true)                         // vui_parameters_present_flag
	w.flag(true).u(8, 255).u(16, 4).u(16, 3)
	w.flag(true).flag(false) // overscan
	w.flag(true).u(3, 5).flag(false).flag(true).u(8, 1).u(8, 1).u(8, 1)
	w.flag(true).ue(0).ue(0)                         // chroma loc
	w.flag(true).u(32, 1001).u(32, 60000).flag(true) // timing: 29.97 fps
	for k := 0; k < 2; k++ {                         // NAL and VCL HRD
		w.flag(true)
		w.ue(1).u(4, 2).u(4, 3)
		for i := 0; i < 2; i++ {
			w.ue(5000).ue(9000).flag(i == 1)
		}
		w.u(5, 23).u(5, 23).u(5, 23).u(5, 24)
	}
	w.flag(false)                          // low_delay_hrd_flag
	w.flag(true)                           // pic_struct_present_flag
	w.flag(true)                           // bitstream_restriction_flag
	w.flag(true).ue(2).ue(1).ue(16).ue(16) // mv flag, denoms, mv lengths
	w.ue(3).ue(5)                          // max_num_reorder_frames, max_dec_frame_buffering
	end = w.nbit + 8                       // after the NAL header
	return w.nal(0x67), end
}

func TestParseSPSH264VUI(t *testing.T) {
	n, end := h264VUISPS()
	s, info, err := parseSPS(H264, n)
	require.NoError(t, err)
	assert.Equal(t, end, info.endBit)
	assert.Equal(t, 1280, s.Width)
	assert.Equal(t, 712, s.Height)
	assert.Equal(t, 3, s.ReorderFrames)
	assert.Equal(t, int64(3003), s.FrameDuration)
	want, err := avc.ParseSPSNALUnit(n, true)
	require.NoError(t, err)
	assert.Equal(t, want.Width, uint(s.Width))
	assert.Equal(t, want.Height, uint(s.Height))
	assert.Equal(t, int(want.VUI.MaxNumReorderFrames), s.ReorderFrames)
	r, assumed := ReorderOf(H264, s)
	assert.Equal(t, 3, r)
	assert.False(t, assumed)
}

// h265Opts shapes a hand-built H.265 SPS.
type h265Opts struct {
	subLayers    int  // sps_max_sub_layers_minus1 + 1
	scalingList  bool // sps_scaling_list_data with mixed pred modes
	pcm          bool
	longTerm     bool
	vui          bool // timing (1001/30000), HRD over every sub-layer, bitstream_restriction
	numSTRPS     uint64
	stRPS        func(w *bitWriter)
	sepColour    bool
	maxDecMinus1 uint64
	ext          func(w *bitWriter) // from sps_extension_present_flag on; nil writes 0
}

// h265SPS builds an SPS; end is the bit position, NAL header included, after
// the extension flags.
func h265SPS(o h265Opts) (nalu []byte, end int) {
	if o.subLayers == 0 {
		o.subLayers = 1
	}
	if o.maxDecMinus1 == 0 {
		o.maxDecMinus1 = 4
	}
	maxSub := o.subLayers - 1
	w := &bitWriter{}
	w.u(4, 0).u(3, uint64(maxSub)).flag(true)
	// profile_tier_level(1, maxSub)
	w.u(2, 0).flag(false).u(5, 1).u(32, 0x60000000).u(4, 0b1001).u(43, 0).u(1, 0).u(8, 93)
	for i := 0; i < maxSub; i++ {
		w.flag(i == 0).flag(true) // sub_layer_profile_present_flag, sub_layer_level_present_flag
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			w.u(2, 0)
		}
	}
	for i := 0; i < maxSub; i++ {
		if i == 0 {
			w.u(2, 0).flag(false).u(5, 1).u(32, 0x60000000).u(4, 0b1001).u(43, 0).u(1, 0)
		}
		w.u(8, 90)
	}
	w.ue(1) // sps_seq_parameter_set_id
	if o.sepColour {
		w.ue(3).flag(true)
	} else {
		w.ue(1)
	}
	w.ue(1920).ue(1088)
	w.flag(true).ue(0).ue(0).ue(0).ue(4) // conformance window: 8 rows off the bottom (4:2:0)
	w.ue(0).ue(0)                        // bit depths
	w.ue(4)                              // log2_max_pic_order_cnt_lsb_minus4
	w.flag(true)                         // sps_sub_layer_ordering_info_present_flag
	for i := 0; i <= maxSub; i++ {
		w.ue(o.maxDecMinus1).ue(uint64(i)).ue(0)
	}
	w.ue(0).ue(3).ue(0).ue(3).ue(1).ue(1)
	w.flag(o.scalingList)
	if o.scalingList {
		w.flag(true) // sps_scaling_list_data_present_flag
		for sizeID := 0; sizeID < 4; sizeID++ {
			step := 1
			if sizeID == 3 {
				step = 3
			}
			for m := 0; m < 6; m += step {
				if (sizeID+m)%2 == 0 {
					w.flag(false)
					if sizeID == 3 {
						w.ue(uint64(m / 3))
					} else {
						w.ue(uint64(m))
					}
					continue
				}
				w.flag(true)
				coefNum := min(64, 1<<(4+(sizeID<<1)))
				if sizeID > 1 {
					w.se(5)
				}
				for i := 0; i < coefNum; i++ {
					w.se(int64(i%3) - 1)
				}
			}
		}
	}
	w.flag(true).flag(true) // amp, sample_adaptive_offset
	w.flag(o.pcm)
	if o.pcm {
		w.u(4, 7).u(4, 7).ue(0).ue(1).flag(true)
	}
	w.ue(o.numSTRPS)
	if o.stRPS != nil {
		o.stRPS(w)
	}
	w.flag(o.longTerm)
	if o.longTerm {
		w.ue(2).u(8, 17).flag(true).u(8, 200).flag(false)
	}
	w.flag(true).flag(true) // sps_temporal_mvp_enabled_flag, strong_intra_smoothing
	w.flag(o.vui)
	if o.vui {
		w.flag(true).u(8, 255).u(16, 1).u(16, 1)
		w.flag(true).flag(true)
		w.flag(true).u(3, 5).flag(false).flag(true).u(8, 1).u(8, 1).u(8, 1)
		w.flag(true).ue(1).ue(1)
		w.flag(false).flag(false).flag(false) // neutral chroma, field_seq, frame_field_info
		w.flag(true).ue(0).ue(0).ue(0).ue(0)  // default display window
		w.flag(true).u(32, 1001).u(32, 30000)
		w.flag(true).ue(0) // poc proportional, num_ticks_poc_diff_one_minus1
		w.flag(true)       // vui_hrd_parameters_present_flag
		// hrd_parameters(1, maxSub)
		w.flag(true).flag(true) // nal, vcl
		w.flag(true).u(8, 23).u(5, 4).flag(true).u(5, 4)
		w.u(4, 1).u(4, 2).u(4, 3)
		w.u(5, 23).u(5, 23).u(5, 23)
		for i := 0; i <= maxSub; i++ {
			switch i % 3 {
			case 0: // fixed_pic_rate_general_flag: within_cvs inferred, elemental duration
				w.flag(true).ue(0).ue(1)
			case 1: // not fixed, low delay: no cpb_cnt
				w.flag(false).flag(false).flag(true)
				for k := 0; k < 2; k++ { // NAL and VCL, CpbCnt 0
					w.ue(100).ue(200).ue(10).ue(20).flag(false)
				}
				continue
			case 2:
				w.flag(false).flag(true).ue(3).ue(0)
			}
			cpb := 2
			if i%3 == 2 {
				cpb = 1
			}
			for k := 0; k < 2; k++ {
				for c := 0; c < cpb; c++ {
					w.ue(100).ue(200).ue(10).ue(20).flag(true)
				}
			}
		}
		w.flag(true).flag(false).flag(true).flag(false)
		w.ue(0).ue(2).ue(1).ue(15).ue(15)
	}
	if o.ext != nil {
		o.ext(w)
	} else {
		w.flag(false) // sps_extension_present_flag
	}
	end = w.nbit + 16
	return w.nal(0x42, 0x01), end
}

// rpsSets writes the three st_ref_pic_sets used by TestParseSPSH265RPS.
func rpsSets(w *bitWriter) {
	// Set 0, explicit: S0 = [-1, -3] used [1, 0]; S1 = [2] used [1].
	w.ue(2).ue(1)
	w.ue(0).flag(true).ue(1).flag(false)
	w.ue(1).flag(true)
	// Set 1, predicted from set 0 with deltaRps = -1.
	w.flag(true)
	w.flag(true).ue(0)
	w.flag(true)              // j=0 used
	w.flag(false).flag(true)  // j=1 not used, use_delta
	w.flag(true)              // j=2 used
	w.flag(false).flag(false) // j=3 (deltaRps itself) dropped
	// Set 2, predicted from set 1 with deltaRps = +2, everything used.
	w.flag(true)
	w.flag(false).ue(1)
	w.flag(true).flag(true).flag(true).flag(true)
}

func TestParseSPSH265RPS(t *testing.T) {
	n, end := h265SPS(h265Opts{numSTRPS: 3, stRPS: rpsSets})
	s, info, err := parseSPS(H265, n)
	require.NoError(t, err)
	assert.Equal(t, end, info.endBit)
	require.Equal(t, 3, s.NumShortTermRefPicSets())
	want := []ShortTermRPS{
		{DeltaPocS0: []int32{-1, -3}, UsedS0: []bool{true, false}, DeltaPocS1: []int32{2}, UsedS1: []bool{true}},
		{DeltaPocS0: []int32{-2, -4}, UsedS0: []bool{true, false}, DeltaPocS1: []int32{1}, UsedS1: []bool{true}},
		{DeltaPocS0: []int32{-2}, UsedS0: []bool{true}, DeltaPocS1: []int32{2, 3}, UsedS1: []bool{true, true}},
	}
	assert.Equal(t, want, s.ShortTermRefPicSets)
	assert.Equal(t, 3, s.ShortTermRefPicSets[0].NumDeltaPocs())
	assert.Equal(t, 1, s.ShortTermRefPicSets[2].NumNegativePics())
	assert.Equal(t, 2, s.ShortTermRefPicSets[2].NumPositivePics())

	mp, err := hevc.ParseSPSNALUnit(n)
	require.NoError(t, err)
	require.Len(t, mp.ShortTermRefPicSets, 3)
	// mp4ff leaves the counts of inter-predicted sets at zero, so only the
	// explicit set is compared.
	assert.Equal(t, int(mp.ShortTermRefPicSets[0].NumNegativePics), s.ShortTermRefPicSets[0].NumNegativePics())
	assert.Equal(t, int(mp.ShortTermRefPicSets[0].NumPositivePics), s.ShortTermRefPicSets[0].NumPositivePics())

	// A slice-header set (idx == num_short_term_ref_pic_sets) predicted
	// from set 0 via delta_idx_minus1 = 2, deltaRps = +1.
	w := (&bitWriter{}).flag(true).ue(2).flag(false).ue(0)
	w.flag(true).flag(true).flag(true).flag(true)
	r := &bitReader{buf: w.buf}
	got := parseShortTermRPS(r, 3, 3, s.ShortTermRefPicSets, s.MaxDecPicBufferingMinus1)
	require.NoError(t, r.err)
	assert.Equal(t, ShortTermRPS{
		DeltaPocS0: []int32{-2}, UsedS0: []bool{true},
		DeltaPocS1: []int32{1, 3}, UsedS1: []bool{true, true},
	}, got)
}

func TestParseSPSH265Rich(t *testing.T) {
	n, end := h265SPS(h265Opts{
		subLayers: 3, scalingList: true, pcm: true, longTerm: true, vui: true,
		numSTRPS: 3, stRPS: rpsSets,
	})
	s, info, err := parseSPS(H265, n)
	require.NoError(t, err)
	assert.Equal(t, end, info.endBit)
	assert.Equal(t, uint8(3), s.MaxSubLayers)
	assert.Equal(t, 2, s.ReorderFrames, "highest sub-layer")
	assert.Equal(t, int64(3003), s.FrameDuration)
	assert.Equal(t, 1920, s.Width)
	assert.Equal(t, 1080, s.Height)
	assert.True(t, s.LongTermRefPicsPresent)
	assert.Equal(t, uint32(2), s.NumLongTermRefPicsSPS)
	assert.True(t, s.TemporalMVPEnabled)
	assert.True(t, s.SampleAdaptiveOffsetEnabled)
	assert.Equal(t, uint8(8), s.Log2MaxPicOrderCntLsb)
	assert.Equal(t, uint8(3), s.Log2MinCbSize)
	assert.Equal(t, uint8(6), s.Log2CtbSize)
	assert.Equal(t, uint32(30*17), s.PicSizeInCtbsY())
	r, assumed := ReorderOf(H265, s)
	assert.Equal(t, 2, r)
	assert.False(t, assumed)

	mp, err := hevc.ParseSPSNALUnit(n)
	require.NoError(t, err)
	require.NotNil(t, mp.VUI)
	assert.Equal(t, uint(1001), mp.VUI.NumUnitsInTick)
	assert.True(t, mp.VUI.BitstreamRestrictionFlag, "mp4ff also reaches bitstream_restriction")
	w, h := mp.ImageSize()
	assert.Equal(t, int(w), s.Width)
	assert.Equal(t, int(h), s.Height)

	sep, _ := h265SPS(h265Opts{sepColour: true})
	s, err = ParseSPS(H265, sep)
	require.NoError(t, err)
	assert.True(t, s.SeparateColourPlane)
	assert.Equal(t, uint8(0), s.ChromaArrayType())
	assert.Equal(t, 1084, s.Height, "conformance window in luma units without chroma")
}

// h264PPS builds a PPS for sps 0 of h264VUISPS (1280x720, 80x45 MBs) with
// slice group map type 6 and an 8x8 scaling matrix.
func h264PPS(picSizeMinus1 uint64, numRefIdx uint64) []byte {
	w := &bitWriter{}
	w.ue(5).ue(0)           // pps id, sps id
	w.flag(true).flag(true) // entropy_coding_mode, bottom_field_pic_order_in_frame_present
	w.ue(2).ue(6)           // num_slice_groups_minus1, slice_group_map_type
	w.ue(picSizeMinus1)     // pic_size_in_map_units_minus1
	for i := uint64(0); i <= picSizeMinus1 && i < 80*45; i++ {
		w.u(2, i%3) // slice_group_id
	}
	w.ue(numRefIdx).ue(1)   // num_ref_idx defaults
	w.flag(true).u(2, 1)    // weighted_pred_flag, weighted_bipred_idc
	w.se(-30).se(3).se(-12) // pic_init_qp_minus26 (high bit depth not needed), qs, chroma offset
	w.flag(true).flag(false).flag(true)
	w.flag(true).flag(true) // transform_8x8_mode, pic_scaling_matrix_present
	for i := 0; i < 8; i++ {
		if i == 7 {
			w.flag(true)
			for j := 0; j < 64; j++ {
				w.se(1)
			}
			continue
		}
		w.flag(false)
	}
	w.se(7)
	return w.nal(0x68)
}

func TestParsePPSH264(t *testing.T) {
	spsNAL, _ := h264VUISPS()
	sps, err := ParseSPS(H264, spsNAL)
	require.NoError(t, err)
	sets := map[uint32]*SPS{0: sps}

	// pic_init_qp_minus26 of -30 is out of range at 8 bits.
	_, err = ParsePPS(H264, h264PPS(80*45-1, 2), sets)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pic_init_qp_minus26")
}

// h264PPSWith builds a valid PPS for sps 0 of h264VUISPS with three slice
// groups whose map is written by groups, an 8x8 scaling list and
// second_chroma_qp_index_offset 7.
func h264PPSWith(groups func(w *bitWriter)) []byte {
	w := &bitWriter{}
	w.ue(5).ue(0).flag(true).flag(true)
	w.ue(2) // num_slice_groups_minus1
	groups(w)
	w.ue(2).ue(1).flag(true).u(2, 1)
	w.se(-20).se(3).se(-12)
	w.flag(true).flag(false).flag(true)
	w.flag(true).flag(true)
	for i := 0; i < 8; i++ {
		w.flag(i == 7)
		if i == 7 {
			for j := 0; j < 64; j++ {
				w.se(1)
			}
		}
	}
	w.se(7)
	return w.nal(0x68)
}

func TestParsePPSH264Valid(t *testing.T) {
	spsNAL, _ := h264VUISPS()
	sps, err := ParseSPS(H264, spsNAL)
	require.NoError(t, err)
	sets := map[uint32]*SPS{0: sps}
	mpSPS, err := avc.ParseSPSNALUnit(spsNAL, true)
	require.NoError(t, err)

	type6 := h264PPSWith(func(w *bitWriter) {
		w.ue(6).ue(80*45 - 1)
		for i := 0; i < 80*45; i++ {
			w.u(2, uint64(i%3)) // slice_group_id
		}
	})
	type2 := h264PPSWith(func(w *bitWriter) {
		w.ue(2).ue(0).ue(100).ue(200).ue(3599)
	})
	type4 := h264PPSWith(func(w *bitWriter) { w.ue(4).flag(true).ue(41) })
	for name, n := range map[string][]byte{"type 6": type6, "type 2": type2, "type 4": type4} {
		p, err := ParsePPS(H264, n, sets)
		require.NoError(t, err, name)
		assert.Equal(t, uint32(5), p.ID)
		assert.True(t, p.EntropyCodingMode)
		assert.True(t, p.BottomFieldPicOrderInFramePresent)
		assert.Equal(t, uint32(2), p.NumSliceGroupsMinus1)
		assert.Equal(t, uint32(2), p.NumRefIdxL0DefaultActiveMinus1, name)
		assert.Equal(t, uint32(1), p.NumRefIdxL1DefaultActiveMinus1, name)
		assert.True(t, p.WeightedPred, name)
		assert.Equal(t, uint8(1), p.WeightedBipredIDC, name)
		assert.True(t, p.RedundantPicCntPresent, name)
		assert.True(t, p.Transform8x8Mode, name)
	}
	p, err := ParsePPS(H264, type4, sets)
	require.NoError(t, err)
	assert.Equal(t, uint32(4), p.SliceGroupMapType)
	assert.Equal(t, uint32(41), p.SliceGroupChangeRateMinus1)

	// mp4ff mis-reads map types 2 and 6 (one top_left/bottom_right pair too
	// many; no pic_size_in_map_units_minus1), so it cross-checks type 4 only.
	n := type4
	mp, err := avc.ParsePPSNALUnit(n, map[uint32]*avc.SPS{0: mpSPS})
	require.NoError(t, err)
	assert.Equal(t, 7, mp.SecondChromaQpIndexOffset)
	assert.Equal(t, mp.Transform8x8ModeFlag, p.Transform8x8Mode)
	assert.Equal(t, mp.WeightedBipredIDC, uint(p.WeightedBipredIDC))

	// Without the optional tail (no more_rbsp_data).
	w := &bitWriter{}
	w.ue(1).ue(0).flag(false).flag(false).ue(0).ue(0).ue(0).flag(false).u(2, 0)
	w.se(0).se(0).se(0).flag(true).flag(false).flag(false)
	p, err = ParsePPS(H264, w.nal(0x68), sets)
	require.NoError(t, err)
	assert.False(t, p.Transform8x8Mode)

	// Unknown SPS.
	_, err = ParsePPS(H264, n, map[uint32]*SPS{})
	require.Error(t, err)

	// Map type 6 with a size that does not match the SPS.
	_, err = ParsePPS(H264, h264PPS(10, 2), sets)
	require.Error(t, err)

	// num_ref_idx_l0_default_active_minus1 out of range.
	w = &bitWriter{}
	w.ue(1).ue(0).flag(false).flag(false).ue(0).ue(32).ue(0).flag(false).u(2, 0)
	w.se(0).se(0).se(0).flag(true).flag(false).flag(false)
	_, err = ParsePPS(H264, w.nal(0x68), sets)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "num_ref_idx_l0_default_active_minus1")
}

func TestParsePPSH265(t *testing.T) {
	spsNAL, _ := h265SPS(h265Opts{})
	sps, err := ParseSPS(H265, spsNAL)
	require.NoError(t, err)
	sets := map[uint32]*SPS{1: sps}
	w := &bitWriter{}
	w.ue(7).ue(1)                       // pps id, sps id
	w.flag(true).flag(true).u(3, 2)     // dependent slices, output flag, extra bits
	w.flag(true).flag(true)             // sign data hiding, cabac_init_present
	w.ue(3).ue(1)                       // num_ref_idx defaults
	w.se(-4)                            // init_qp_minus26
	w.flag(false).flag(true)            // constrained intra, transform skip
	w.flag(true).ue(2)                  // cu_qp_delta_enabled, diff_cu_qp_delta_depth
	w.se(-2).se(3).flag(true)           // cb, cr offsets, slice chroma offsets present
	w.flag(true).flag(true)             // weighted pred, bipred
	w.flag(false)                       // transquant bypass
	w.flag(true).flag(true)             // tiles, entropy sync
	w.ue(2).ue(1).flag(false)           // 3 columns, 2 rows, not uniform
	w.ue(9).ue(9).ue(7)                 // column_width_minus1, row_height_minus1
	w.flag(true)                        // loop filter across tiles
	w.flag(true)                        // loop filter across slices
	w.flag(true).flag(true).flag(false) // deblocking control, override, not disabled
	w.se(-2).se(4)                      // beta, tc
	w.flag(false)                       // pps_scaling_list_data_present_flag
	w.flag(true).ue(1).flag(true)       // lists_modification, parallel merge, header ext
	w.flag(false)                       // pps_extension_present_flag
	n := w.nal(0x44, 0x01)
	p, err := ParsePPS(H265, n, sets)
	require.NoError(t, err)
	mpSPS, err := hevc.ParseSPSNALUnit(spsNAL)
	require.NoError(t, err)
	mp, err := hevc.ParsePPSNALUnit(n, map[uint32]*hevc.SPS{1: mpSPS})
	require.NoError(t, err)
	assertPPS265(t, mp, p)
	assert.Equal(t, uint8(2), p.NumExtraSliceHeaderBits)
	assert.True(t, p.SliceSegmentHeaderExtensionPresent)

	// More tile columns than CTB columns (1920/64 = 30).
	w = &bitWriter{}
	w.ue(0).ue(1).flag(false).flag(false).u(3, 0).flag(false).flag(false).ue(0).ue(0).se(0)
	w.flag(false).flag(false).flag(false).se(0).se(0).flag(false).flag(false).flag(false).flag(false)
	w.flag(true).flag(false).ue(30).ue(0).flag(true)
	_, err = ParsePPS(H265, w.nal(0x44, 0x01), sets)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "num_tile_columns_minus1")

	_, err = ParsePPS(H265, n, map[uint32]*SPS{})
	require.Error(t, err)
}

// allocPerRun reports the bytes allocated by one call of f, averaged.
func allocPerRun(f func()) uint64 {
	const runs = 50
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	for i := 0; i < runs; i++ {
		f()
	}
	runtime.ReadMemStats(&b)
	return (b.TotalAlloc - a.TotalAlloc) / runs
}

func TestParseSPSHostile(t *testing.T) {
	t.Run("36 leading zeros", func(t *testing.T) {
		n := append([]byte{0x67, 77, 0, 30, 0, 0, 0, 0}, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff)
		_, err := ParseSPS(H264, n)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exp-Golomb")
		assert.True(t, strings.HasPrefix(err.Error(), "h26x: parse sps: "), err.Error())

		w := (&bitWriter{}).u(4, 0).u(3, 0).flag(true).u(2, 0).flag(false).u(5, 1).u(32, 0).u(48, 0).u(8, 93)
		w.u(36, 0).u(1, 1).u(36, 0)
		_, err = ParseSPS(H265, w.nal(0x42, 0x01))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exp-Golomb")
	})

	t.Run("cycle 100000", func(t *testing.T) {
		offsets := make([]int64, 100000)
		n := h264POC1SPS(100000, offsets)
		_, err := ParseSPS(H264, n)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "num_ref_frames_in_pic_order_cnt_cycle")
		got := allocPerRun(func() { _, _ = ParseSPS(H264, n) })
		assert.Less(t, got, uint64(2*len(n)+4096), "allocation bounded by the input, not the count")
	})

	t.Run("cycle huge", func(t *testing.T) {
		n := h264POC1SPS(0xfffffffe, nil)
		got := allocPerRun(func() {
			_, err := ParseSPS(H264, n)
			require.Error(t, err)
		})
		assert.Less(t, got, uint64(4096))
	})

	t.Run("out of range", func(t *testing.T) {
		w := (&bitWriter{}).u(8, 77).u(8, 0).u(8, 30).ue(32)
		_, err := ParseSPS(H264, w.nal(0x67))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "seq_parameter_set_id")

		n, _ := h265SPS(h265Opts{numSTRPS: 65})
		_, err = ParseSPS(H265, n)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "num_short_term_ref_pic_sets")

		n, _ = h265SPS(h265Opts{subLayers: 8})
		_, err = ParseSPS(H265, n)
		require.Error(t, err)

		// An SCC extension with 2^40 palette predictor initializers.
		n, _ = h265SPS(h265Opts{ext: sccExt})
		got := allocPerRun(func() {
			err := CheckSPS(H265, n)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "sps_scc_extension")
		})
		assert.Less(t, got, uint64(2*len(n)+4096))

		// An inter-predicted set above sps_max_dec_pic_buffering_minus1:
		// set 0 has 2+1 pictures with maxDec 2, set 1 adds deltaRps.
		n, _ = h265SPS(h265Opts{maxDecMinus1: 3, numSTRPS: 2, stRPS: func(w *bitWriter) {
			w.ue(2).ue(1).ue(0).flag(true).ue(0).flag(true).ue(0).flag(true)
			w.flag(true).flag(true).ue(1) // inter, deltaRps = -2
			w.flag(true).flag(true).flag(true).flag(true)
		}})
		_, err = ParseSPS(H265, n)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sps_max_dec_pic_buffering_minus1")

		// num_negative_pics above sps_max_dec_pic_buffering_minus1.
		n, _ = h265SPS(h265Opts{numSTRPS: 1, stRPS: func(w *bitWriter) { w.ue(5).ue(0) }})
		_, err = ParseSPS(H265, n)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "num_negative_pics")
	})

	t.Run("wrong nal", func(t *testing.T) {
		n, _ := h264VUISPS()
		_, err := ParseSPS(H264, append([]byte{0x68}, n[1:]...))
		require.Error(t, err)
		_, err = ParseSPS(H264, nil)
		require.Error(t, err)
		h, _ := h265SPS(h265Opts{})
		_, err = ParseSPS(H265, append([]byte{0x42, 0x09}, h[2:]...)) // nuh_layer_id 1
		require.Error(t, err)
		_, err = ParseSPS(H265, h[:1])
		require.Error(t, err)
		require.Error(t, CheckSPS(Codec(9), h))
		require.NoError(t, CheckSPS(H265, h))
	})

	t.Run("truncated", func(t *testing.T) {
		type sample struct {
			c Codec
			n []byte
		}
		samples := make([]sample, 0, 6)
		for _, f := range []string{"h264_aac.ts", "h264_opengop.ts"} {
			s, _ := fixtureSets(t, f, H264)
			samples = append(samples, sample{H264, s[0]})
		}
		s, _ := fixtureSets(t, "h265_aac51.ts", H265)
		samples = append(samples, sample{H265, s[0]})
		v, _ := h264VUISPS()
		samples = append(samples, sample{H264, v}, sample{H264, h264POC1SPS(3, []int64{2, -4, 7})})
		rich, _ := h265SPS(h265Opts{subLayers: 3, scalingList: true, pcm: true, longTerm: true, vui: true, numSTRPS: 3, stRPS: rpsSets})
		samples = append(samples, sample{H265, rich})
		for i, sm := range samples {
			_, info, err := parseSPS(sm.c, sm.n)
			require.NoError(t, err, "sample %d", i)
			require.Positive(t, info.vuiBit)
			for l := 0; l < len(sm.n); l++ {
				var err error
				require.NotPanics(t, func() { _, err = ParseSPS(sm.c, sm.n[:l]) }, "sample %d len %d", i, l)
				if l*8 <= info.vuiBit {
					require.Error(t, err, "sample %d len %d cuts before the VUI", i, l)
				}
			}
		}
	})

	t.Run("mutations", func(t *testing.T) {
		v, _ := h264VUISPS()
		rich, _ := h265SPS(h265Opts{subLayers: 3, scalingList: true, pcm: true, longTerm: true, vui: true, numSTRPS: 3, stRPS: rpsSets})
		rng := rand.New(rand.NewSource(1))
		for _, base := range []struct {
			c Codec
			n []byte
		}{{H264, v}, {H265, rich}} {
			for k := 0; k < 3000; k++ {
				m := append([]byte(nil), base.n...)
				for j := 0; j < 1+rng.Intn(4); j++ {
					m[1+rng.Intn(len(m)-1)] ^= byte(1 << rng.Intn(8))
				}
				require.NotPanics(t, func() {
					s, err := ParseSPS(base.c, m)
					if err == nil {
						_, _ = ParsePPS(base.c, []byte{0x68, 0x80}, map[uint32]*SPS{s.ID: s})
					}
				})
			}
		}
	})
}

func TestTruncatedVUIIsAbsent(t *testing.T) {
	// The VUI ends early: the SPS stays usable, with the VUI fields absent.
	n, end := h264VUISPS()
	cut := n[:len(n)-3]
	s, info, err := parseSPS(H264, cut)
	require.NoError(t, err)
	assert.Less(t, info.vuiBit, end)
	assert.Equal(t, -1, s.ReorderFrames)
	assert.Equal(t, int64(0), s.FrameDuration)
	assert.Equal(t, 1280, s.Width)
}

func TestReorderOf(t *testing.T) {
	cases := []struct {
		name    string
		c       Codec
		s       *SPS
		r       int
		assumed bool
	}{
		{"vui", H264, &SPS{ReorderFrames: 1, ProfileIDC: 100}, 1, false},
		{"vui zero", H264, &SPS{ReorderFrames: 0, ProfileIDC: 100}, 0, false},
		{"clamped", H264, &SPS{ReorderFrames: 40}, 16, false},
		{"poc type 2", H264, &SPS{ReorderFrames: -1, PicOrderCntType: 2, ProfileIDC: 100}, 0, false},
		{"baseline", H264, &SPS{ReorderFrames: -1, ProfileIDC: 66}, 0, false},
		{"constrained baseline", H264, &SPS{ReorderFrames: -1, ProfileIDC: 66, ConstraintFlags: 0x40}, 0, false},
		{"main no vui", H264, &SPS{ReorderFrames: -1, ProfileIDC: 77}, 2, true},
		{"h265", H265, &SPS{ReorderFrames: 3}, 3, false},
		{"h265 absent", H265, &SPS{ReorderFrames: -1, PicOrderCntType: 2, ProfileIDC: 66}, 2, true},
		{"nil", H264, nil, 2, true},
	}
	for _, c := range cases {
		r, assumed := ReorderOf(c.c, c.s)
		assert.Equal(t, c.r, r, c.name)
		assert.Equal(t, c.assumed, assumed, c.name)
	}
}

func TestParamSets(t *testing.T) {
	var p ParamSets
	spsNAL, _ := h264VUISPS()
	w := &bitWriter{}
	w.ue(1).ue(0).flag(false).flag(false).ue(0).ue(0).ue(0).flag(false).u(2, 0)
	w.se(0).se(0).se(0).flag(true).flag(false).flag(false)
	ppsNAL := w.nal(0x68)

	// A PPS before its SPS is rejected.
	require.Error(t, p.Update(H264, ppsNAL))
	assert.Nil(t, p.PPS(1))

	require.NoError(t, p.Update(H264, spsNAL))
	require.NoError(t, p.Update(H264, ppsNAL))
	require.NotNil(t, p.SPS(0))
	require.NotNil(t, p.PPS(1))
	assert.Equal(t, 1280, p.SPS(0).Width)

	// Other NAL types are ignored.
	require.NoError(t, p.Update(H264, []byte{0x65, 0x88, 0x80}))
	require.NoError(t, p.Update(H264, []byte{0x06, 0x05}))
	require.Error(t, p.Update(H264, nil))

	// A bad SPS is rejected and the stored one kept.
	require.Error(t, p.Update(H264, h264POC1SPS(100000, make([]int64, 300))))
	assert.Equal(t, 1280, p.SPS(0).Width)

	// Replacing the SPS keeps PPSs that still parse against it.
	w = &bitWriter{}
	w.u(8, 77).u(8, 0).u(8, 30).ue(0).ue(0).ue(2).ue(4).flag(false).ue(19).ue(14)
	w.flag(true).flag(true).flag(false).flag(false)
	require.NoError(t, p.Update(H264, w.nal(0x67)))
	assert.Equal(t, 320, p.SPS(0).Width)
	require.NotNil(t, p.PPS(1))

	// A PPS that no longer parses against a replaced SPS is dropped:
	// pic_init_qp_minus26 -30 is valid at 10 bits only.
	var b ParamSets
	sps10 := func(depthMinus8 uint64) []byte {
		w := &bitWriter{}
		w.u(8, 110).u(8, 0).u(8, 30).ue(2).ue(1).ue(depthMinus8).ue(depthMinus8).flag(false).flag(false)
		w.ue(0).ue(2).ue(4).flag(false).ue(19).ue(14).flag(true).flag(true).flag(false).flag(false)
		return w.nal(0x67)
	}
	pw := (&bitWriter{}).ue(4).ue(2).flag(false).flag(false).ue(0).ue(0).ue(0).flag(false).u(2, 0)
	pw.se(-30).se(0).se(0).flag(true).flag(false).flag(false)
	require.NoError(t, b.Update(H264, sps10(2)))
	require.NoError(t, b.Update(H264, pw.nal(0x68)))
	require.NotNil(t, b.PPS(4))
	require.NoError(t, b.Update(H264, sps10(2)), "an identical SPS is a no-op")
	require.NotNil(t, b.PPS(4))
	require.NoError(t, b.Update(H264, sps10(0)))
	assert.Nil(t, b.PPS(4))
	assert.Equal(t, uint8(8), b.SPS(2).BitDepthLuma)

	// H.265: VPS and enhancement-layer sets are ignored.
	var q ParamSets
	h, _ := h265SPS(h265Opts{})
	require.NoError(t, q.Update(H265, []byte{0x40, 0x01, 0x0c}))
	require.NoError(t, q.Update(H265, append([]byte{0x42, 0x09}, h[2:]...)))
	assert.Nil(t, q.SPS(1))
	require.NoError(t, q.Update(H265, h))
	require.NotNil(t, q.SPS(1))
	p2 := (&bitWriter{}).ue(0).ue(1).flag(false).flag(false).u(3, 0).flag(false).flag(false).ue(0).ue(0).se(0)
	p2.flag(false).flag(false).flag(false).se(0).se(0).flag(false).flag(false).flag(false).flag(false)
	p2.flag(false).flag(false).flag(true).flag(false).flag(false).flag(false).ue(0).flag(false).flag(false)
	require.NoError(t, q.Update(H265, p2.nal(0x44, 0x01)))
	require.NotNil(t, q.PPS(0))
	require.Error(t, q.Update(H265, []byte{0x42}))
	require.Error(t, q.Update(Codec(7), h))

	// A bad set of another codec leaves the stored sets unchanged.
	require.Error(t, q.Update(H264, h264POC1SPS(100000, make([]int64, 300))))
	require.Error(t, q.Update(H264, ppsNAL))
	require.NotNil(t, q.SPS(1))
	require.NotNil(t, q.PPS(0))

	// A codec change starts over.
	require.NoError(t, q.Update(H264, spsNAL))
	assert.Nil(t, q.SPS(1))
	assert.NotNil(t, q.SPS(0))
}

func FuzzParseSPS(f *testing.F) {
	v, _ := h264VUISPS()
	f.Add(uint8(H264), v)
	f.Add(uint8(H264), h264POC1SPS(3, []int64{2, -4, 7}))
	rich, _ := h265SPS(h265Opts{subLayers: 3, scalingList: true, pcm: true, longTerm: true, vui: true, numSTRPS: 3, stRPS: rpsSets})
	f.Add(uint8(H265), rich)
	f.Fuzz(func(t *testing.T, c uint8, n []byte) {
		codec := Codec(c%2 + 1)
		s, err := ParseSPS(codec, n)
		if err != nil {
			return
		}
		var p ParamSets
		_ = p.Update(codec, n)
		_, _ = ParsePPS(codec, n, map[uint32]*SPS{s.ID: s})
	})
}

// sccExt writes sps_extension flags with sps_scc_extension_flag set and an
// SCC extension whose sps_num_palette_predictor_initializers_minus1 is
// 2^40, which a parser without bounds turns into a huge allocation.
func sccExt(w *bitWriter) {
	w.flag(true)                                     // sps_extension_present_flag
	w.flag(false).flag(false).flag(false).flag(true) // range, multilayer, 3d, scc
	w.u(4, 0)                                        // sps_extension_4bits
	w.flag(false)                                    // sps_curr_pic_ref_enabled_flag
	w.flag(true).ue(63).ue(64)                       // palette_mode_enabled_flag, palette_max_size, delta
	w.flag(true).ue(1 << 40)                         // initializers present, count minus1
	for range 64 {
		w.u(8, 0x80)
	}
}

func TestParseSPSH265Extensions(t *testing.T) {
	// The range extension is accepted and skipped.
	n, end := h265SPS(h265Opts{ext: func(w *bitWriter) {
		w.flag(true).flag(true).flag(false).flag(false).flag(false).u(4, 0)
		w.u(9, 0x1ff)
	}})
	_, info, err := parseSPS(H265, n)
	require.NoError(t, err)
	assert.Equal(t, end, info.endBit)

	for name, ext := range map[string]func(w *bitWriter){
		"multilayer": func(w *bitWriter) { w.flag(true).flag(false).flag(true).flag(false).flag(false).u(4, 0).flag(true) },
		"3d":         func(w *bitWriter) { w.flag(true).flag(false).flag(false).flag(true).flag(false).u(4, 0).u(8, 0) },
		"scc":        sccExt,
		"4bits": func(w *bitWriter) {
			w.flag(true).flag(true).flag(false).flag(false).flag(false).u(4, 2).u(9, 0).flag(true)
		},
	} {
		n, _ := h265SPS(h265Opts{ext: ext})
		_, err := ParseSPS(H265, n)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "unsupported", name)
		require.Error(t, CheckSPS(H265, n), name)
	}

	// The extension flags are required unless the VUI was cut short.
	n, _ = h265SPS(h265Opts{})
	cut := n[:len(n)-1] // drops the byte with sps_extension_present_flag and the stop bit
	_, err = ParseSPS(H265, cut)
	require.Error(t, err)
}

// h265VPS builds a VPS with three sub-layers, two layer sets over layer IDs
// 0..3, timing information and two HRD parameter sets (the second without
// common information); ext is vps_extension_flag. end is the bit position,
// NAL header included, before vps_extension_flag.
func h265VPS(numHRD uint64, ext bool) (nalu []byte, end int) {
	w := &bitWriter{}
	w.u(4, 0).flag(true).flag(true).u(6, 0).u(3, 2).flag(true).u(16, 0xffff)
	// profile_tier_level(1, 2)
	w.u(2, 0).flag(false).u(5, 1).u(32, 0x60000000).u(4, 0b1001).u(43, 0).u(1, 0).u(8, 93)
	w.flag(true).flag(true).flag(false).flag(true)
	for i := 2; i < 8; i++ {
		w.u(2, 0)
	}
	w.u(88, 0).u(8, 90).u(8, 90)
	w.flag(true)
	for i := 0; i < 3; i++ {
		w.ue(4).ue(uint64(i)).ue(0)
	}
	w.u(6, 3).ue(1).u(4, 0b1111) // vps_max_layer_id, num_layer_sets_minus1, flags of set 1
	w.flag(true).u(32, 1).u(32, 25).flag(true).ue(0)
	w.ue(numHRD)
	for i := uint64(0); i < numHRD && i < 2; i++ {
		w.ue(i) // hrd_layer_set_idx
		if i > 0 {
			w.flag(false) // cprms_present_flag: sub-layer part only
		} else {
			w.flag(true).flag(false).flag(false).u(8, 0x55).u(15, 0) // nal, vcl, sub_pic, scales, lengths
		}
		for s := 0; s < 3; s++ {
			w.flag(true).ue(0).ue(1) // fixed general, elemental duration, cpb_cnt_minus1
			if i == 0 {
				for c := 0; c < 2; c++ {
					w.ue(1000).ue(2000).flag(false)
				}
			}
		}
	}
	end = w.nbit + 16
	w.flag(ext)
	return w.nal(0x40, 0x01), end
}

func TestCheckVPS(t *testing.T) {
	vps := fixtureNALs(t, "h265_aac51.ts", H265)[nalVPS265]
	require.NotEmpty(t, vps)
	for _, n := range vps {
		require.NoError(t, CheckVPS(n))
		_, err := hevc.ParseVPSNALUnit(n)
		require.NoError(t, err)
	}

	n, end := h265VPS(2, true)
	got, err := checkVPS(n)
	require.NoError(t, err)
	assert.Equal(t, end, got)
	mp, err := hevc.ParseVPSNALUnit(n)
	require.NoError(t, err)
	require.NotNil(t, mp.TimingInfo)
	assert.Equal(t, uint(2), mp.TimingInfo.NumHrdParameters)
	assert.True(t, mp.ExtensionFlag, "mp4ff reads vps_extension_flag where h26x stopped")

	t.Run("hostile", func(t *testing.T) {
		huge, _ := h265VPS(0xfffffffe, false)
		err := CheckVPS(huge)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vps_num_hrd_parameters")
		assert.True(t, strings.HasPrefix(err.Error(), "h26x: check vps: "), err.Error())

		base := (&bitWriter{}).u(4, 0).flag(true).flag(true)
		for name, c := range map[string]struct {
			n    []byte
			want string
		}{
			"max layers": {(&bitWriter{}).u(4, 0).flag(true).flag(true).u(6, 7).u(3, 0).nal(0x40, 0x01), "vps_max_layers_minus1"},
			"sub layers": {(&bitWriter{}).u(4, 0).flag(true).flag(true).u(6, 0).u(3, 7).nal(0x40, 0x01), "vps_max_sub_layers_minus1"},
			"layer id":   {vpsTail(func(w *bitWriter) { w.u(6, 63) }), "vps_max_layer_id"},
			"layer sets": {vpsTail(func(w *bitWriter) { w.u(6, 62).ue(0xfffffffe) }), "vps_num_layer_sets_minus1"},
			"exp-Golomb": {vpsTail(func(w *bitWriter) { w.u(6, 62).u(40, 0).u(1, 1).u(40, 0) }), "exp-Golomb"},
			"hrd idx":    {vpsTail(func(w *bitWriter) { w.u(6, 0).ue(0).flag(true).u(64, 0).flag(false).ue(1).ue(3) }), "hrd_layer_set_idx"},
			"cpb count": {vpsTail(func(w *bitWriter) {
				w.u(6, 0).ue(0).flag(true).u(64, 0).flag(false).ue(1).ue(0).flag(false).flag(false).flag(false).flag(false).flag(false).ue(32)
			}), "cpb_cnt_minus1"},
			"wrong nal":   {base.nal(0x42, 0x01), "nal type"},
			"layer 1 vps": {(&bitWriter{}).u(8, 0).nal(0x40, 0x09), "nuh_layer_id"},
		} {
			err := CheckVPS(c.n)
			require.Error(t, err, name)
			assert.Contains(t, err.Error(), c.want, name)
		}
		got := allocPerRun(func() { _ = CheckVPS(huge) })
		assert.Less(t, got, uint64(4096))
	})

	t.Run("truncated", func(t *testing.T) {
		samples := [][]byte{vps[0], n}
		for i, sm := range samples {
			end, err := checkVPS(sm)
			require.NoError(t, err)
			for l := 0; l < len(sm); l++ {
				var err error
				require.NotPanics(t, func() { _, err = checkVPS(sm[:l]) }, "sample %d len %d", i, l)
				if l*8 < end {
					require.Error(t, err, "sample %d len %d", i, l)
				}
			}
		}
	})
}

// vpsTail builds a VPS with one sub-layer and no ordering info up to
// vps_max_layer_id, which tail writes along with what follows.
func vpsTail(tail func(w *bitWriter)) []byte {
	w := &bitWriter{}
	w.u(4, 0).flag(true).flag(true).u(6, 0).u(3, 0).flag(true).u(16, 0xffff)
	w.u(2, 0).flag(false).u(5, 1).u(32, 0x60000000).u(4, 0b1001).u(43, 0).u(1, 0).u(8, 93)
	w.flag(false).ue(4).ue(2).ue(0)
	tail(w)
	return w.nal(0x40, 0x01)
}
