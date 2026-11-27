package h26x

import (
	"bytes"
	"errors"
	"fmt"
)

// Codec selects the bitstream syntax.
type Codec uint8

// Supported codecs.
const (
	H264 Codec = iota + 1 // ITU-T H.264 / AVC
	H265                  // ITU-T H.265 / HEVC
)

// NAL unit types of parameter sets.
const (
	nalSPS264 = 7
	nalPPS264 = 8
	nalVPS265 = 32
	nalSPS265 = 33
	nalPPS265 = 34
)

// Bounds that are not ranges of the specifications but sanity limits on
// picture size, well above every level's maximum.
const (
	maxMbs  = 4096  // H.264 macroblocks per dimension (65536 luma samples)
	maxLuma = 65536 // H.265 luma samples per dimension
)

// SPS is a parsed sequence parameter set. Fields marked H.264 or H.265 are
// zero for the other codec. Values named after a syntax element without its
// "minus" suffix hold the derived value (for example Log2MaxFrameNum is
// log2_max_frame_num_minus4 + 4).
type SPS struct {
	Codec Codec
	ID    uint32 // seq_parameter_set_id

	ProfileIDC      uint8 // H.264 profile_idc; H.265 general_profile_idc
	ConstraintFlags uint8 // H.264: constraint_set0..5 flags and reserved_zero_2bits, as one byte
	LevelIDC        uint8 // H.264 level_idc; H.265 general_level_idc

	ChromaFormatIDC     uint8
	SeparateColourPlane bool
	BitDepthLuma        uint8
	BitDepthChroma      uint8

	// Width and Height are the cropped picture size in luma samples.
	Width, Height int

	// Log2MaxPicOrderCntLsb is log2_max_pic_order_cnt_lsb_minus4 + 4 (H.264
	// pic_order_cnt_type 0, and H.265).
	Log2MaxPicOrderCntLsb uint8

	// H.264.
	Log2MaxFrameNum           uint8
	PicOrderCntType           uint8
	DeltaPicOrderAlwaysZero   bool
	OffsetForNonRefPic        int32
	OffsetForTopToBottomField int32
	OffsetForRefFrame         []int32 // num_ref_frames_in_pic_order_cnt_cycle entries
	MaxNumRefFrames           uint32
	GapsInFrameNumAllowed     bool
	PicWidthInMbs             uint32
	PicHeightInMapUnits       uint32
	FrameMbsOnly              bool
	MbAdaptiveFrameField      bool
	Direct8x8Inference        bool

	// H.265.
	VPSID                       uint8
	MaxSubLayers                uint8  // sps_max_sub_layers_minus1 + 1
	MaxDecPicBufferingMinus1    uint32 // of the highest sub-layer
	PicWidthInLumaSamples       uint32
	PicHeightInLumaSamples      uint32
	Log2MinCbSize               uint8 // MinCbLog2SizeY
	Log2CtbSize                 uint8 // CtbLog2SizeY
	AmpEnabled                  bool
	SampleAdaptiveOffsetEnabled bool
	PCMEnabled                  bool
	PCMLoopFilterDisabled       bool
	ShortTermRefPicSets         []ShortTermRPS
	LongTermRefPicsPresent      bool
	NumLongTermRefPicsSPS       uint32
	TemporalMVPEnabled          bool
	StrongIntraSmoothingEnabled bool

	// ReorderFrames is H.264 VUI max_num_reorder_frames or H.265
	// sps_max_num_reorder_pics of the highest sub-layer; -1 when absent.
	ReorderFrames int
	// FrameDuration is the frame duration in 90 kHz units from the VUI timing
	// information, rounded to the nearest unit (H.264: 2·num_units_in_tick /
	// time_scale; H.265: num_units_in_tick / time_scale); 0 when absent or
	// above 10 s.
	FrameDuration int64
}

// ChromaArrayType returns the ChromaArrayType variable: 0 with separate
// colour planes, else chroma_format_idc.
func (s *SPS) ChromaArrayType() uint8 {
	if s.SeparateColourPlane {
		return 0
	}
	return s.ChromaFormatIDC
}

// NumShortTermRefPicSets returns num_short_term_ref_pic_sets (H.265).
func (s *SPS) NumShortTermRefPicSets() int { return len(s.ShortTermRefPicSets) }

// PicSizeInCtbsY returns the picture size in coding tree blocks (H.265).
func (s *SPS) PicSizeInCtbsY() uint32 {
	return s.picWidthInCtbs() * s.picHeightInCtbs()
}

func (s *SPS) picWidthInCtbs() uint32 {
	ctb := uint32(1) << s.Log2CtbSize
	return (s.PicWidthInLumaSamples + ctb - 1) / ctb
}

func (s *SPS) picHeightInCtbs() uint32 {
	ctb := uint32(1) << s.Log2CtbSize
	return (s.PicHeightInLumaSamples + ctb - 1) / ctb
}

// ShortTermRPS is one H.265 st_ref_pic_set with its derived lists: the
// negative POC deltas in decreasing order (DeltaPocS0) and the positive ones
// in increasing order (DeltaPocS1), each with its used_by_curr_pic flag.
type ShortTermRPS struct {
	DeltaPocS0 []int32
	UsedS0     []bool
	DeltaPocS1 []int32
	UsedS1     []bool
}

// NumNegativePics returns NumNegativePics.
func (s ShortTermRPS) NumNegativePics() int { return len(s.DeltaPocS0) }

// NumPositivePics returns NumPositivePics.
func (s ShortTermRPS) NumPositivePics() int { return len(s.DeltaPocS1) }

// NumDeltaPocs returns NumDeltaPocs.
func (s ShortTermRPS) NumDeltaPocs() int { return len(s.DeltaPocS0) + len(s.DeltaPocS1) }

// PPS is a parsed picture parameter set. Fields marked H.264 or H.265 are
// zero for the other codec.
type PPS struct {
	Codec Codec
	ID    uint32 // pic_parameter_set_id
	SPSID uint32 // seq_parameter_set_id

	NumRefIdxL0DefaultActiveMinus1 uint32
	NumRefIdxL1DefaultActiveMinus1 uint32
	WeightedPred                   bool // weighted_pred_flag
	DeblockingFilterControlPresent bool

	// H.264.
	EntropyCodingMode                 bool
	BottomFieldPicOrderInFramePresent bool
	NumSliceGroupsMinus1              uint32
	SliceGroupMapType                 uint32
	SliceGroupChangeRateMinus1        uint32
	WeightedBipredIDC                 uint8
	ConstrainedIntraPred              bool
	RedundantPicCntPresent            bool
	Transform8x8Mode                  bool

	// H.265.
	DependentSliceSegmentsEnabled      bool
	OutputFlagPresent                  bool
	NumExtraSliceHeaderBits            uint8
	SignDataHidingEnabled              bool
	CabacInitPresent                   bool
	CuQpDeltaEnabled                   bool
	SliceChromaQpOffsetsPresent        bool
	WeightedBipred                     bool // weighted_bipred_flag
	TransquantBypassEnabled            bool
	TilesEnabled                       bool
	EntropyCodingSyncEnabled           bool
	NumTileColumnsMinus1               uint32
	NumTileRowsMinus1                  uint32
	LoopFilterAcrossSlicesEnabled      bool
	DeblockingFilterOverrideEnabled    bool
	DeblockingFilterDisabled           bool
	ListsModificationPresent           bool
	Log2ParallelMergeLevel             uint8
	SliceSegmentHeaderExtensionPresent bool
}

// spsInfo carries parse positions for tests: the bit offset (in the
// unescaped NAL unit, header included) of vui_parameters_present_flag and the
// offset after the last syntax element parsed.
type spsInfo struct {
	vuiBit, endBit int
}

// ParseSPS parses a sequence parameter set NAL unit (with its NAL header,
// without a start code). Emulation prevention bytes are removed internally.
func ParseSPS(c Codec, nalu []byte) (*SPS, error) {
	s, _, err := parseSPS(c, nalu)
	if err != nil {
		return nil, fmt.Errorf("h26x: parse sps: %w", err)
	}
	return s, nil
}

// CheckSPS parses an SPS and keeps only the verdict. It is the pre-check to
// run before handing an SPS to a parser without bounds.
func CheckSPS(c Codec, nalu []byte) error {
	if _, _, err := parseSPS(c, nalu); err != nil {
		return fmt.Errorf("h26x: check sps: %w", err)
	}
	return nil
}

func parseSPS(c Codec, nalu []byte) (*SPS, spsInfo, error) {
	var info spsInfo
	hdr, err := checkHeader(c, nalu, nalSPS264, nalSPS265)
	if err != nil {
		return nil, info, err
	}
	r := &bitReader{buf: unescape(nalu), pos: hdr * 8}
	s := &SPS{Codec: c, ReorderFrames: -1}
	switch c {
	case H264:
		info.vuiBit = parseSPS264(r, s)
	case H265:
		info.vuiBit = parseSPS265(r, s)
	}
	info.endBit = r.pos
	if r.err != nil {
		return nil, info, r.err
	}
	return s, info, nil
}

// checkHeader validates the NAL unit type and returns the header length.
func checkHeader(c Codec, nalu []byte, want264, want265 byte) (int, error) {
	switch c {
	case H264:
		if len(nalu) < 1 {
			return 0, errors.New("empty nal unit")
		}
		if typ := nalu[0] & 0x1f; typ != want264 {
			return 0, fmt.Errorf("nal type %d, want %d", typ, want264)
		}
		return 1, nil
	case H265:
		if len(nalu) < 2 {
			return 0, errors.New("nal unit shorter than its header")
		}
		if typ := nalu[0] >> 1 & 0x3f; typ != want265 {
			return 0, fmt.Errorf("nal type %d, want %d", typ, want265)
		}
		if layer := (nalu[0]&1)<<5 | nalu[1]>>3; layer != 0 {
			return 0, fmt.Errorf("unsupported nuh_layer_id %d", layer)
		}
		return 2, nil
	default:
		return 0, fmt.Errorf("unknown codec %d", c)
	}
}

// subWH returns SubWidthC and SubHeightC for a ChromaArrayType (Table 6-1),
// with 1 for monochrome and separate colour planes.
func subWH(chromaArrayType uint8) (w, h int64) {
	switch chromaArrayType {
	case 1:
		return 2, 2
	case 2:
		return 2, 1
	default:
		return 1, 1
	}
}

// highProfile264 reports whether an H.264 profile_idc carries the chroma
// format and bit depth fields (§7.3.2.1.1).
func highProfile264(p uint8) bool {
	switch p {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

// parseSPS264 parses seq_parameter_set_data (H.264 §7.3.2.1.1) and returns
// the bit offset of vui_parameters_present_flag.
func parseSPS264(r *bitReader, s *SPS) int {
	s.ProfileIDC = uint8(r.u(8))
	s.ConstraintFlags = uint8(r.u(8))
	s.LevelIDC = uint8(r.u(8))
	s.ID = r.ueMax("seq_parameter_set_id", 31)
	s.ChromaFormatIDC, s.BitDepthLuma, s.BitDepthChroma = 1, 8, 8
	if highProfile264(s.ProfileIDC) {
		s.ChromaFormatIDC = uint8(r.ueMax("chroma_format_idc", 3))
		if s.ChromaFormatIDC == 3 {
			s.SeparateColourPlane = r.flag()
		}
		s.BitDepthLuma = 8 + uint8(r.ueMax("bit_depth_luma_minus8", 6))
		s.BitDepthChroma = 8 + uint8(r.ueMax("bit_depth_chroma_minus8", 6))
		r.flag()      // qpprime_y_zero_transform_bypass_flag
		if r.flag() { // seq_scaling_matrix_present_flag
			n := 8
			if s.ChromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n && r.err == nil; i++ {
				if r.flag() { // seq_scaling_list_present_flag[i]
					scalingList264(r, i)
				}
			}
		}
	}
	s.Log2MaxFrameNum = 4 + uint8(r.ueMax("log2_max_frame_num_minus4", 12))
	s.PicOrderCntType = uint8(r.ueMax("pic_order_cnt_type", 2))
	switch s.PicOrderCntType {
	case 0:
		s.Log2MaxPicOrderCntLsb = 4 + uint8(r.ueMax("log2_max_pic_order_cnt_lsb_minus4", 12))
	case 1:
		s.DeltaPicOrderAlwaysZero = r.flag()
		s.OffsetForNonRefPic = r.se()
		s.OffsetForTopToBottomField = r.se()
		n := r.ueMax("num_ref_frames_in_pic_order_cnt_cycle", 255)
		if r.err == nil {
			s.OffsetForRefFrame = make([]int32, n)
			for i := range s.OffsetForRefFrame {
				s.OffsetForRefFrame[i] = r.se()
			}
		}
	}
	s.MaxNumRefFrames = r.ueMax("max_num_ref_frames", 16)
	s.GapsInFrameNumAllowed = r.flag()
	s.PicWidthInMbs = r.ueMax("pic_width_in_mbs_minus1", maxMbs-1) + 1
	s.PicHeightInMapUnits = r.ueMax("pic_height_in_map_units_minus1", maxMbs-1) + 1
	s.FrameMbsOnly = r.flag()
	if !s.FrameMbsOnly {
		s.MbAdaptiveFrameField = r.flag()
	}
	s.Direct8x8Inference = r.flag()
	frameMbs := int64(2)
	if s.FrameMbsOnly {
		frameMbs = 1
	}
	width := int64(s.PicWidthInMbs) * 16
	height := frameMbs * int64(s.PicHeightInMapUnits) * 16
	if r.flag() { // frame_cropping_flag
		cropX, cropY := subWH(s.ChromaArrayType())
		cropY *= frameMbs
		left, right := int64(r.ue()), int64(r.ue())
		top, bottom := int64(r.ue()), int64(r.ue())
		width -= cropX * (left + right)
		height -= cropY * (top + bottom)
		if r.err == nil && (width <= 0 || height <= 0) {
			r.fail(errors.New("frame cropping removes the whole picture"))
		}
	}
	s.Width, s.Height = int(width), int(height)
	vuiBit := r.pos
	if r.flag() { // vui_parameters_present_flag
		_ = lenientVUI(r, s, func() { parseVUI264(r, s) })
	}
	return vuiBit
}

// lenientVUI runs a VUI parser. A VUI cut short by the end of the NAL unit,
// a known encoder quirk, leaves the SPS usable with the VUI-derived fields
// absent; any other error stands.
// It reports whether the VUI was cut short.
func lenientVUI(r *bitReader, s *SPS, parse func()) (truncated bool) {
	parse()
	if errors.Is(r.err, errTruncated) {
		r.err = nil
		s.ReorderFrames, s.FrameDuration = -1, 0
		return true
	}
	return false
}

// scalingList264 skips scaling_list() for list i (H.264 §7.3.2.1.1.1): lists
// 0-5 have 16 coefficients, the others 64.
func scalingList264(r *bitReader, i int) {
	size := 16
	if i >= 6 {
		size = 64
	}
	last, next := int32(8), int32(8)
	for j := 0; j < size && r.err == nil; j++ {
		if next != 0 {
			delta := r.seRange("delta_scale", -128, 127)
			next = (last + delta + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// parseVUI264 parses vui_parameters (H.264 §E.1.1).
func parseVUI264(r *bitReader, s *SPS) {
	if r.flag() { // aspect_ratio_info_present_flag
		if r.u(8) == 255 { // aspect_ratio_idc == Extended_SAR
			r.skip(32) // sar_width, sar_height
		}
	}
	if r.flag() { // overscan_info_present_flag
		r.flag() // overscan_appropriate_flag
	}
	if r.flag() { // video_signal_type_present_flag
		r.skip(4)     // video_format, video_full_range_flag
		if r.flag() { // colour_description_present_flag
			r.skip(24)
		}
	}
	if r.flag() { // chroma_loc_info_present_flag
		r.ue() // chroma_sample_loc_type_top_field
		r.ue() // chroma_sample_loc_type_bottom_field
	}
	if r.flag() { // timing_info_present_flag
		units, scale := r.u(32), r.u(32)
		r.flag() // fixed_frame_rate_flag
		s.FrameDuration = frameDuration(2*int64(units), scale)
	}
	nal := r.flag()
	if nal {
		hrd264(r)
	}
	vcl := r.flag()
	if vcl {
		hrd264(r)
	}
	if nal || vcl {
		r.flag() // low_delay_hrd_flag
	}
	r.flag()      // pic_struct_present_flag
	if r.flag() { // bitstream_restriction_flag
		r.flag() // motion_vectors_over_pic_boundaries_flag
		r.ue()   // max_bytes_per_pic_denom
		r.ue()   // max_bits_per_mb_denom
		r.ue()   // log2_max_mv_length_horizontal
		r.ue()   // log2_max_mv_length_vertical
		reorder := r.ueMax("max_num_reorder_frames", 16)
		r.ueMax("max_dec_frame_buffering", 16)
		if r.err == nil {
			s.ReorderFrames = int(reorder)
		}
	}
}

// hrd264 skips hrd_parameters (H.264 §E.1.2).
func hrd264(r *bitReader) {
	cpb := r.ueMax("cpb_cnt_minus1", 31)
	r.skip(8) // bit_rate_scale, cpb_size_scale
	for i := uint32(0); i <= cpb && r.err == nil; i++ {
		r.ue()   // bit_rate_value_minus1
		r.ue()   // cpb_size_value_minus1
		r.flag() // cbr_flag
	}
	r.skip(20) // four delay and offset lengths
}

// frameDuration converts a tick of num/scale seconds to 90 kHz, rounded; 0
// when either value is zero or the duration is above 10 s (absurd timing
// is treated as absent).
func frameDuration(num int64, scale uint32) int64 {
	if num <= 0 || scale == 0 {
		return 0
	}
	d := (num*90000 + int64(scale)/2) / int64(scale)
	if d > maxFrameDuration {
		return 0
	}
	return d
}

// parseSPS265 parses seq_parameter_set_rbsp (H.265 §7.3.2.2.1) through the
// VUI and returns the bit offset of vui_parameters_present_flag.
func parseSPS265(r *bitReader, s *SPS) int {
	s.VPSID = uint8(r.u(4))
	maxSub := r.u(3)
	if r.err == nil && maxSub > 6 {
		r.fail(fmt.Errorf("sps_max_sub_layers_minus1 %d out of range [0, 6]", maxSub))
		return r.pos
	}
	s.MaxSubLayers = uint8(maxSub) + 1
	r.flag() // sps_temporal_id_nesting_flag
	profileTierLevel(r, s, int(maxSub))
	s.ID = r.ueMax("sps_seq_parameter_set_id", 15)
	s.ChromaFormatIDC = uint8(r.ueMax("chroma_format_idc", 3))
	if s.ChromaFormatIDC == 3 {
		s.SeparateColourPlane = r.flag()
	}
	s.PicWidthInLumaSamples = r.ueMax("pic_width_in_luma_samples", maxLuma)
	s.PicHeightInLumaSamples = r.ueMax("pic_height_in_luma_samples", maxLuma)
	if r.err == nil && (s.PicWidthInLumaSamples == 0 || s.PicHeightInLumaSamples == 0) {
		r.fail(errors.New("zero picture size"))
	}
	width, height := int64(s.PicWidthInLumaSamples), int64(s.PicHeightInLumaSamples)
	if r.flag() { // conformance_window_flag
		subW, subH := subWH(s.ChromaArrayType())
		left, right := int64(r.ue()), int64(r.ue())
		top, bottom := int64(r.ue()), int64(r.ue())
		width -= subW * (left + right)
		height -= subH * (top + bottom)
		if r.err == nil && (width <= 0 || height <= 0) {
			r.fail(errors.New("conformance window removes the whole picture"))
		}
	}
	s.Width, s.Height = int(width), int(height)
	s.BitDepthLuma = 8 + uint8(r.ueMax("bit_depth_luma_minus8", 8))
	s.BitDepthChroma = 8 + uint8(r.ueMax("bit_depth_chroma_minus8", 8))
	s.Log2MaxPicOrderCntLsb = 4 + uint8(r.ueMax("log2_max_pic_order_cnt_lsb_minus4", 12))
	first := maxSub
	if r.flag() { // sps_sub_layer_ordering_info_present_flag
		first = 0
	}
	for i := first; i <= maxSub && r.err == nil; i++ {
		dec := r.ueMax("sps_max_dec_pic_buffering_minus1", 15)
		reorder := r.ueMax("sps_max_num_reorder_pics", dec)
		r.ue() // sps_max_latency_increase_plus1
		s.MaxDecPicBufferingMinus1, s.ReorderFrames = dec, int(reorder)
	}
	s.Log2MinCbSize = 3 + uint8(r.ueMax("log2_min_luma_coding_block_size_minus3", 3))
	s.Log2CtbSize = s.Log2MinCbSize + uint8(r.ueMax("log2_diff_max_min_luma_coding_block_size", uint32(6-s.Log2MinCbSize)))
	minTb := 2 + uint8(r.ueMax("log2_min_luma_transform_block_size_minus2", uint32(max(int(s.Log2MinCbSize)-3, 0))))
	r.ueMax("log2_diff_max_min_luma_transform_block_size", uint32(max(int(min(s.Log2CtbSize, 5))-int(minTb), 0)))
	r.ueMax("max_transform_hierarchy_depth_inter", uint32(s.Log2CtbSize-minTb))
	r.ueMax("max_transform_hierarchy_depth_intra", uint32(s.Log2CtbSize-minTb))
	if r.flag() { // scaling_list_enabled_flag
		if r.flag() { // sps_scaling_list_data_present_flag
			scalingListData265(r)
		}
	}
	s.AmpEnabled = r.flag()
	s.SampleAdaptiveOffsetEnabled = r.flag()
	s.PCMEnabled = r.flag()
	if s.PCMEnabled {
		r.skip(8) // pcm_sample_bit_depth_luma_minus1, pcm_sample_bit_depth_chroma_minus1
		r.ueMax("log2_min_pcm_luma_coding_block_size_minus3", 2)
		r.ueMax("log2_diff_max_min_pcm_luma_coding_block_size", 2)
		s.PCMLoopFilterDisabled = r.flag()
	}
	n := r.ueMax("num_short_term_ref_pic_sets", 64)
	if r.err == nil && n > 0 {
		s.ShortTermRefPicSets = make([]ShortTermRPS, 0, n)
		for i := 0; i < int(n) && r.err == nil; i++ {
			set := parseShortTermRPS(r, i, int(n), s.ShortTermRefPicSets, s.MaxDecPicBufferingMinus1)
			s.ShortTermRefPicSets = append(s.ShortTermRefPicSets, set)
		}
	}
	s.LongTermRefPicsPresent = r.flag()
	if s.LongTermRefPicsPresent {
		s.NumLongTermRefPicsSPS = r.ueMax("num_long_term_ref_pics_sps", 32)
		for i := uint32(0); i < s.NumLongTermRefPicsSPS && r.err == nil; i++ {
			r.skip(int(s.Log2MaxPicOrderCntLsb)) // lt_ref_pic_poc_lsb_sps[i]
			r.flag()                             // used_by_curr_pic_lt_sps_flag[i]
		}
	}
	s.TemporalMVPEnabled = r.flag()
	s.StrongIntraSmoothingEnabled = r.flag()
	vuiBit := r.pos
	if r.flag() { // vui_parameters_present_flag
		reorder := s.ReorderFrames
		truncated := lenientVUI(r, s, func() { parseVUI265(r, s, int(maxSub)) })
		s.ReorderFrames = reorder // from the SPS itself, not the VUI
		if truncated {
			return vuiBit // nothing follows a VUI cut short
		}
	}
	spsExtensions(r)
	return vuiBit
}

// spsExtensions reads sps_extension_present_flag and the extension flags
// (H.265 §7.3.2.2.1). The range extension is skipped; the multilayer, 3D and
// SCC extensions and sps_extension_4bits are not supported and fail, so the
// SPS is never accepted with extension data this package does not check.
func spsExtensions(r *bitReader) {
	if !r.flag() { // sps_extension_present_flag
		return
	}
	rangeExt := r.flag()
	multilayer, d3, scc := r.flag(), r.flag(), r.flag()
	ext4 := r.u(4)
	if r.err != nil {
		return
	}
	switch {
	case multilayer:
		r.fail(errors.New("unsupported sps_multilayer_extension"))
	case d3:
		r.fail(errors.New("unsupported sps_3d_extension"))
	case scc:
		r.fail(errors.New("unsupported sps_scc_extension"))
	case ext4 != 0:
		r.fail(fmt.Errorf("unsupported sps_extension_4bits %#x", ext4))
	case rangeExt:
		r.skip(9) // sps_range_extension(): nine flags
	}
}

// profileTierLevel parses profile_tier_level(1, maxSub) (H.265 §7.3.3).
func profileTierLevel(r *bitReader, s *SPS, maxSub int) {
	r.skip(3) // general_profile_space, general_tier_flag
	s.ProfileIDC = uint8(r.u(5))
	r.skip(32)         // general_profile_compatibility_flag[32]
	r.skip(4 + 43 + 1) // source and constraint flags, general_inbld_flag
	s.LevelIDC = uint8(r.u(8))
	var profile, level [8]bool
	for i := range maxSub {
		profile[i] = r.flag()
		level[i] = r.flag()
	}
	if maxSub > 0 {
		r.skip(2 * (8 - maxSub)) // reserved_zero_2bits
	}
	for i := range maxSub {
		if profile[i] {
			r.skip(88) // sub-layer profile space, tier, idc, flags
		}
		if level[i] {
			r.skip(8) // sub_layer_level_idc
		}
	}
}

// scalingListData265 skips scaling_list_data() (H.265 §7.3.4).
func scalingListData265(r *bitReader) {
	for sizeID := 0; sizeID < 4; sizeID++ {
		step := 1
		if sizeID == 3 {
			step = 3
		}
		for matrixID := 0; matrixID < 6 && r.err == nil; matrixID += step {
			if !r.flag() { // scaling_list_pred_mode_flag
				limit := matrixID
				if sizeID == 3 {
					limit = matrixID / 3
				}
				r.ueMax("scaling_list_pred_matrix_id_delta", uint32(limit))
				continue
			}
			coefNum := min(64, 1<<(4+(sizeID<<1)))
			if sizeID > 1 {
				r.seRange("scaling_list_dc_coef_minus8", -7, 247)
			}
			for i := 0; i < coefNum && r.err == nil; i++ {
				r.seRange("scaling_list_delta_coef", -128, 127)
			}
		}
	}
}

// parseShortTermRPS parses st_ref_pic_set(idx) (H.265 §7.3.7) and derives
// its lists (§7.4.8). sets holds the sets 0..idx-1 and num is
// num_short_term_ref_pic_sets: a slice header passes idx == num ==
// len(sets). maxDecMinus1 is sps_max_dec_pic_buffering_minus1 of the
// highest sub-layer.
func parseShortTermRPS(r *bitReader, idx, num int, sets []ShortTermRPS, maxDecMinus1 uint32) ShortTermRPS {
	var out ShortTermRPS
	if idx != 0 && r.flag() { // inter_ref_pic_set_prediction_flag
		deltaIdx := 1
		if idx == num {
			deltaIdx += int(r.ueMax("delta_idx_minus1", uint32(idx-1)))
		}
		ref := sets[idx-deltaIdx]
		sign := r.flag()
		deltaRps := int32(r.ueMax("abs_delta_rps_minus1", 1<<15-1)) + 1
		if sign {
			deltaRps = -deltaRps
		}
		n := ref.NumDeltaPocs()
		used := make([]bool, n+1)
		useDelta := make([]bool, n+1)
		for j := 0; j <= n && r.err == nil; j++ {
			used[j] = r.flag()
			useDelta[j] = used[j] || r.flag()
		}
		if r.err != nil {
			return out
		}
		out = deriveRPS(ref, deltaRps, used, useDelta)
		// The bound of the explicit form: NumNegativePics ≤ max and
		// NumPositivePics ≤ max − NumNegativePics.
		if uint32(out.NumDeltaPocs()) > maxDecMinus1 {
			r.fail(fmt.Errorf("st_ref_pic_set %d has %d pictures, more than sps_max_dec_pic_buffering_minus1 %d",
				idx, out.NumDeltaPocs(), maxDecMinus1))
			return ShortTermRPS{}
		}
		return out
	}
	neg := r.ueMax("num_negative_pics", maxDecMinus1)
	pos := r.ueMax("num_positive_pics", maxDecMinus1-neg)
	if r.err != nil {
		return out
	}
	out.DeltaPocS0, out.UsedS0 = make([]int32, neg), make([]bool, neg)
	out.DeltaPocS1, out.UsedS1 = make([]int32, pos), make([]bool, pos)
	var poc int32
	for i := range out.DeltaPocS0 {
		poc -= int32(r.ueMax("delta_poc_s0_minus1", 1<<15-1)) + 1
		out.DeltaPocS0[i], out.UsedS0[i] = poc, r.flag()
	}
	poc = 0
	for i := range out.DeltaPocS1 {
		poc += int32(r.ueMax("delta_poc_s1_minus1", 1<<15-1)) + 1
		out.DeltaPocS1[i], out.UsedS1[i] = poc, r.flag()
	}
	return out
}

// deriveRPS applies equations 7-61 and 7-62: the set predicted from ref by
// deltaRps, with entry j of used and useDelta describing ref's picture j (S0
// then S1) and the last entry describing deltaRps itself.
func deriveRPS(ref ShortTermRPS, deltaRps int32, used, useDelta []bool) ShortTermRPS {
	var out ShortTermRPS
	nNeg, nAll := ref.NumNegativePics(), ref.NumDeltaPocs()
	for j := ref.NumPositivePics() - 1; j >= 0; j-- {
		if d := ref.DeltaPocS1[j] + deltaRps; d < 0 && useDelta[nNeg+j] {
			out.DeltaPocS0 = append(out.DeltaPocS0, d)
			out.UsedS0 = append(out.UsedS0, used[nNeg+j])
		}
	}
	if deltaRps < 0 && useDelta[nAll] {
		out.DeltaPocS0 = append(out.DeltaPocS0, deltaRps)
		out.UsedS0 = append(out.UsedS0, used[nAll])
	}
	for j := 0; j < nNeg; j++ {
		if d := ref.DeltaPocS0[j] + deltaRps; d < 0 && useDelta[j] {
			out.DeltaPocS0 = append(out.DeltaPocS0, d)
			out.UsedS0 = append(out.UsedS0, used[j])
		}
	}
	for j := nNeg - 1; j >= 0; j-- {
		if d := ref.DeltaPocS0[j] + deltaRps; d > 0 && useDelta[j] {
			out.DeltaPocS1 = append(out.DeltaPocS1, d)
			out.UsedS1 = append(out.UsedS1, used[j])
		}
	}
	if deltaRps > 0 && useDelta[nAll] {
		out.DeltaPocS1 = append(out.DeltaPocS1, deltaRps)
		out.UsedS1 = append(out.UsedS1, used[nAll])
	}
	for j := 0; j < ref.NumPositivePics(); j++ {
		if d := ref.DeltaPocS1[j] + deltaRps; d > 0 && useDelta[nNeg+j] {
			out.DeltaPocS1 = append(out.DeltaPocS1, d)
			out.UsedS1 = append(out.UsedS1, used[nNeg+j])
		}
	}
	return out
}

// parseVUI265 parses vui_parameters (H.265 §E.2.1).
func parseVUI265(r *bitReader, s *SPS, maxSub int) {
	if r.flag() { // aspect_ratio_info_present_flag
		if r.u(8) == 255 { // aspect_ratio_idc == EXTENDED_SAR
			r.skip(32)
		}
	}
	if r.flag() { // overscan_info_present_flag
		r.flag()
	}
	if r.flag() { // video_signal_type_present_flag
		r.skip(4)
		if r.flag() { // colour_description_present_flag
			r.skip(24)
		}
	}
	if r.flag() { // chroma_loc_info_present_flag
		r.ue() // chroma_sample_loc_type_top_field
		r.ue() // chroma_sample_loc_type_bottom_field
	}
	r.skip(3)     // neutral_chroma_indication_flag, field_seq_flag, frame_field_info_present_flag
	if r.flag() { // default_display_window_flag
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	if r.flag() { // vui_timing_info_present_flag
		units, scale := r.u(32), r.u(32)
		if r.err == nil {
			s.FrameDuration = frameDuration(int64(units), scale)
		}
		if r.flag() { // vui_poc_proportional_to_timing_flag
			r.ue() // vui_num_ticks_poc_diff_one_minus1
		}
		if r.flag() { // vui_hrd_parameters_present_flag
			hrd265(r, true, maxSub)
		}
	}
	if r.flag() { // bitstream_restriction_flag
		r.skip(3) // tiles_fixed_structure, motion_vectors_over_pic_boundaries, restricted_ref_pic_lists
		r.ue()    // min_spatial_segmentation_idc
		r.ue()    // max_bytes_per_pic_denom
		r.ue()    // max_bits_per_min_cu_denom
		r.ue()    // log2_max_mv_length_horizontal
		r.ue()    // log2_max_mv_length_vertical
	}
}

// hrd265 skips hrd_parameters(common, maxSub) (H.265 §E.2.2).
func hrd265(r *bitReader, common bool, maxSub int) {
	var nal, vcl, subPic bool
	if common {
		nal, vcl = r.flag(), r.flag()
	}
	if nal || vcl {
		subPic = r.flag()
		if subPic {
			r.skip(8 + 5 + 1 + 5) // tick_divisor_minus2 .. dpb_output_delay_du_length_minus1
		}
		r.skip(8) // bit_rate_scale, cpb_size_scale
		if subPic {
			r.skip(4) // cpb_size_du_scale
		}
		r.skip(15) // three delay lengths
	}
	for i := 0; i <= maxSub && r.err == nil; i++ {
		fixedGeneral := r.flag()
		fixedCVS := fixedGeneral || r.flag()
		lowDelay := false
		if fixedCVS {
			r.ueMax("elemental_duration_in_tc_minus1", 2047)
		} else {
			lowDelay = r.flag()
		}
		var cpb uint32
		if !lowDelay {
			cpb = r.ueMax("cpb_cnt_minus1", 31)
		}
		for range btoi(nal) + btoi(vcl) {
			subLayerHRD265(r, cpb, subPic)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// subLayerHRD265 skips sub_layer_hrd_parameters (H.265 §E.2.3).
func subLayerHRD265(r *bitReader, cpb uint32, subPic bool) {
	for i := uint32(0); i <= cpb && r.err == nil; i++ {
		r.ue() // bit_rate_value_minus1
		r.ue() // cpb_size_value_minus1
		if subPic {
			r.ue() // cpb_size_du_value_minus1
			r.ue() // bit_rate_du_value_minus1
		}
		r.flag() // cbr_flag
	}
}

// CheckVPS parses an H.265 video parameter set NAL unit (with its NAL
// header, without a start code) through vps_timing_info and its HRD
// parameters, checking every count against its range, and keeps only the
// verdict. It is the pre-check to run before handing a VPS to a parser
// without bounds.
func CheckVPS(nalu []byte) error {
	if _, err := checkVPS(nalu); err != nil {
		return fmt.Errorf("h26x: check vps: %w", err)
	}
	return nil
}

// checkVPS parses video_parameter_set_rbsp (H.265 §7.3.2.1) up to
// vps_extension_flag and returns the bit offset where it stopped.
func checkVPS(nalu []byte) (int, error) {
	hdr, err := checkHeader(H265, nalu, 0, nalVPS265)
	if err != nil {
		return 0, err
	}
	r := &bitReader{buf: unescape(nalu), pos: hdr * 8}
	r.skip(4)                // vps_video_parameter_set_id
	baseInternal := r.flag() // vps_base_layer_internal_flag
	r.flag()                 // vps_base_layer_available_flag
	maxLayers := r.u(6)      // vps_max_layers_minus1
	maxSub := r.u(3)         // vps_max_sub_layers_minus1
	if r.err == nil && maxLayers > 6 {
		return r.pos, fmt.Errorf("vps_max_layers_minus1 %d out of range [0, 6]", maxLayers)
	}
	if r.err == nil && maxSub > 6 {
		return r.pos, fmt.Errorf("vps_max_sub_layers_minus1 %d out of range [0, 6]", maxSub)
	}
	r.flag()   // vps_temporal_id_nesting_flag
	r.skip(16) // vps_reserved_0xffff_16bits
	profileTierLevel(r, &SPS{}, int(maxSub))
	first := maxSub
	if r.flag() { // vps_sub_layer_ordering_info_present_flag
		first = 0
	}
	for i := first; i <= maxSub && r.err == nil; i++ {
		dec := r.ueMax("vps_max_dec_pic_buffering_minus1", 15)
		r.ueMax("vps_max_num_reorder_pics", dec)
		r.ue() // vps_max_latency_increase_plus1
	}
	maxLayerID := r.u(6)
	if r.err == nil && maxLayerID > 62 {
		return r.pos, fmt.Errorf("vps_max_layer_id %d out of range [0, 62]", maxLayerID)
	}
	numLayerSets := r.ueMax("vps_num_layer_sets_minus1", 1023)
	for i := uint32(1); i <= numLayerSets && r.err == nil; i++ {
		r.skip(int(maxLayerID) + 1) // layer_id_included_flag[i][0..vps_max_layer_id]
	}
	if r.flag() { // vps_timing_info_present_flag
		r.skip(64)    // vps_num_units_in_tick, vps_time_scale
		if r.flag() { // vps_poc_proportional_to_timing_flag
			r.ue() // vps_num_ticks_poc_diff_one_minus1
		}
		numHRD := r.ueMax("vps_num_hrd_parameters", numLayerSets+1)
		minIdx := uint32(1)
		if baseInternal {
			minIdx = 0
		}
		for i := uint32(0); i < numHRD && r.err == nil; i++ {
			if idx := r.ue(); r.err == nil && (idx < minIdx || idx > numLayerSets) {
				r.fail(fmt.Errorf("hrd_layer_set_idx %d out of range [%d, %d]", idx, minIdx, numLayerSets))
			}
			common := i == 0 || r.flag() // cprms_present_flag[i]
			hrd265(r, common, int(maxSub))
		}
	}
	return r.pos, r.err
}

// ParsePPS parses a picture parameter set NAL unit (with its NAL header,
// without a start code) against the SPSs it may reference, keyed by ID.
func ParsePPS(c Codec, nalu []byte, sps map[uint32]*SPS) (*PPS, error) {
	p, err := parsePPS(c, nalu, sps)
	if err != nil {
		return nil, fmt.Errorf("h26x: parse pps: %w", err)
	}
	return p, nil
}

func parsePPS(c Codec, nalu []byte, sps map[uint32]*SPS) (*PPS, error) {
	hdr, err := checkHeader(c, nalu, nalPPS264, nalPPS265)
	if err != nil {
		return nil, err
	}
	r := &bitReader{buf: unescape(nalu), pos: hdr * 8}
	p := &PPS{Codec: c}
	switch c {
	case H264:
		p.ID = r.ueMax("pic_parameter_set_id", 255)
		p.SPSID = r.ueMax("seq_parameter_set_id", 31)
	case H265:
		p.ID = r.ueMax("pps_pic_parameter_set_id", 63)
		p.SPSID = r.ueMax("pps_seq_parameter_set_id", 15)
	}
	if r.err != nil {
		return nil, r.err
	}
	s := sps[p.SPSID]
	if s == nil || s.Codec != c {
		return nil, fmt.Errorf("unknown sps id %d", p.SPSID)
	}
	switch c {
	case H264:
		parsePPS264(r, p, s)
	case H265:
		parsePPS265(r, p, s)
	}
	if r.err != nil {
		return nil, r.err
	}
	return p, nil
}

// parsePPS264 parses pic_parameter_set_rbsp after the IDs (H.264 §7.3.2.2).
func parsePPS264(r *bitReader, p *PPS, s *SPS) {
	p.EntropyCodingMode = r.flag()
	p.BottomFieldPicOrderInFramePresent = r.flag()
	p.NumSliceGroupsMinus1 = r.ueMax("num_slice_groups_minus1", 7)
	mapUnits := s.PicWidthInMbs * s.PicHeightInMapUnits
	if p.NumSliceGroupsMinus1 > 0 {
		p.SliceGroupMapType = r.ueMax("slice_group_map_type", 6)
		switch p.SliceGroupMapType {
		case 0:
			for i := uint32(0); i <= p.NumSliceGroupsMinus1 && r.err == nil; i++ {
				r.ueMax("run_length_minus1", mapUnits-1)
			}
		case 2:
			for i := uint32(0); i < p.NumSliceGroupsMinus1 && r.err == nil; i++ {
				r.ueMax("top_left", mapUnits-1)
				r.ueMax("bottom_right", mapUnits-1)
			}
		case 3, 4, 5:
			r.flag() // slice_group_change_direction_flag
			p.SliceGroupChangeRateMinus1 = r.ueMax("slice_group_change_rate_minus1", mapUnits-1)
		case 6:
			n := r.ue() // pic_size_in_map_units_minus1
			if r.err == nil && n != mapUnits-1 {
				r.fail(fmt.Errorf("pic_size_in_map_units_minus1 %d, want %d", n, mapUnits-1))
			}
			v := ceilLog2(p.NumSliceGroupsMinus1 + 1)
			if r.err == nil && int64(n+1)*int64(v) > int64(r.left()) {
				r.fail(errTruncated)
			}
			for i := uint32(0); i <= n && r.err == nil; i++ {
				r.u(v) // slice_group_id[i]
			}
		}
	}
	p.NumRefIdxL0DefaultActiveMinus1 = r.ueMax("num_ref_idx_l0_default_active_minus1", 31)
	p.NumRefIdxL1DefaultActiveMinus1 = r.ueMax("num_ref_idx_l1_default_active_minus1", 31)
	p.WeightedPred = r.flag()
	p.WeightedBipredIDC = uint8(r.u(2))
	if r.err == nil && p.WeightedBipredIDC > 2 {
		r.fail(errors.New("weighted_bipred_idc 3 out of range [0, 2]"))
	}
	qpBdOffset := 6 * (int32(s.BitDepthLuma) - 8)
	r.seRange("pic_init_qp_minus26", -(26 + qpBdOffset), 25)
	r.seRange("pic_init_qs_minus26", -26, 25)
	r.seRange("chroma_qp_index_offset", -12, 12)
	p.DeblockingFilterControlPresent = r.flag()
	p.ConstrainedIntraPred = r.flag()
	p.RedundantPicCntPresent = r.flag()
	if !r.moreRBSPData() {
		return
	}
	p.Transform8x8Mode = r.flag()
	if r.flag() { // pic_scaling_matrix_present_flag
		n := 6
		if p.Transform8x8Mode {
			if s.ChromaFormatIDC == 3 {
				n += 6
			} else {
				n += 2
			}
		}
		for i := 0; i < n && r.err == nil; i++ {
			if r.flag() { // pic_scaling_list_present_flag[i]
				scalingList264(r, i)
			}
		}
	}
	r.seRange("second_chroma_qp_index_offset", -12, 12)
}

// ceilLog2 returns Ceil(Log2(n)) for n ≥ 1.
func ceilLog2(n uint32) int {
	v := 0
	for uint32(1)<<v < n {
		v++
	}
	return v
}

// parsePPS265 parses pic_parameter_set_rbsp after the IDs, up to the
// extensions (H.265 §7.3.2.3.1).
func parsePPS265(r *bitReader, p *PPS, s *SPS) {
	p.DependentSliceSegmentsEnabled = r.flag()
	p.OutputFlagPresent = r.flag()
	p.NumExtraSliceHeaderBits = uint8(r.u(3))
	p.SignDataHidingEnabled = r.flag()
	p.CabacInitPresent = r.flag()
	p.NumRefIdxL0DefaultActiveMinus1 = r.ueMax("num_ref_idx_l0_default_active_minus1", 14)
	p.NumRefIdxL1DefaultActiveMinus1 = r.ueMax("num_ref_idx_l1_default_active_minus1", 14)
	qpBdOffset := 6 * (int32(s.BitDepthLuma) - 8)
	r.seRange("init_qp_minus26", -(26 + qpBdOffset), 25)
	p.ConstrainedIntraPred = r.flag()
	r.flag() // transform_skip_enabled_flag
	p.CuQpDeltaEnabled = r.flag()
	if p.CuQpDeltaEnabled {
		r.ueMax("diff_cu_qp_delta_depth", uint32(s.Log2CtbSize-s.Log2MinCbSize))
	}
	r.seRange("pps_cb_qp_offset", -12, 12)
	r.seRange("pps_cr_qp_offset", -12, 12)
	p.SliceChromaQpOffsetsPresent = r.flag()
	p.WeightedPred = r.flag()
	p.WeightedBipred = r.flag()
	p.TransquantBypassEnabled = r.flag()
	p.TilesEnabled = r.flag()
	p.EntropyCodingSyncEnabled = r.flag()
	if p.TilesEnabled {
		p.NumTileColumnsMinus1 = r.ueMax("num_tile_columns_minus1", s.picWidthInCtbs()-1)
		p.NumTileRowsMinus1 = r.ueMax("num_tile_rows_minus1", s.picHeightInCtbs()-1)
		if !r.flag() { // uniform_spacing_flag
			for i := uint32(0); i < p.NumTileColumnsMinus1 && r.err == nil; i++ {
				r.ueMax("column_width_minus1", s.picWidthInCtbs()-1)
			}
			for i := uint32(0); i < p.NumTileRowsMinus1 && r.err == nil; i++ {
				r.ueMax("row_height_minus1", s.picHeightInCtbs()-1)
			}
		}
		r.flag() // loop_filter_across_tiles_enabled_flag
	}
	p.LoopFilterAcrossSlicesEnabled = r.flag()
	p.DeblockingFilterControlPresent = r.flag()
	if p.DeblockingFilterControlPresent {
		p.DeblockingFilterOverrideEnabled = r.flag()
		p.DeblockingFilterDisabled = r.flag()
		if !p.DeblockingFilterDisabled {
			r.seRange("pps_beta_offset_div2", -6, 6)
			r.seRange("pps_tc_offset_div2", -6, 6)
		}
	}
	if r.flag() { // pps_scaling_list_data_present_flag
		scalingListData265(r)
	}
	p.ListsModificationPresent = r.flag()
	p.Log2ParallelMergeLevel = 2 + uint8(r.ueMax("log2_parallel_merge_level_minus2", uint32(s.Log2CtbSize-2)))
	p.SliceSegmentHeaderExtensionPresent = r.flag()
}

// ReorderOf returns the reorder depth to assume for DTS: ReorderFrames when
// the SPS carries it; otherwise 0 for H.264 pic_order_cnt_type 2 and for the
// Baseline and Constrained Baseline profiles (profile_idc 66), which have no
// B frames; otherwise 2 with assumed set. The result is clamped to [0, 16].
func ReorderOf(c Codec, s *SPS) (r int, assumed bool) {
	switch {
	case s == nil:
		return 2, true
	case s.ReorderFrames >= 0:
		return min(s.ReorderFrames, 16), false
	case c == H264 && (s.PicOrderCntType == 2 || s.ProfileIDC == 66):
		return 0, false
	default:
		return 2, true
	}
}

// ParamSets keeps the latest valid SPS and PPS of one stream by ID. The zero
// value is ready to use.
type ParamSets struct {
	codec  Codec
	spsRaw map[uint32][]byte
	sps    map[uint32]*SPS
	ppsRaw map[uint32][]byte
	pps    map[uint32]*PPS
}

// Update stores an SPS or PPS NAL unit (with its header, without a start
// code). Other NAL types, and H.265 sets of layers other than 0, are
// ignored. A set that does not parse is rejected with an error and leaves
// the stored sets unchanged. A changed SPS re-parses the PPSs that reference
// it and drops those that no longer parse. A different codec than the
// previous call starts over.
func (p *ParamSets) Update(c Codec, nalu []byte) error {
	if err := p.update(c, nalu); err != nil {
		return fmt.Errorf("h26x: update param sets: %w", err)
	}
	return nil
}

func (p *ParamSets) update(c Codec, nalu []byte) error {
	var isSPS, isPPS bool
	switch c {
	case H264:
		if len(nalu) < 1 {
			return errors.New("empty nal unit")
		}
		typ := nalu[0] & 0x1f
		isSPS, isPPS = typ == nalSPS264, typ == nalPPS264
	case H265:
		if len(nalu) < 2 {
			return errors.New("nal unit shorter than its header")
		}
		if (nalu[0]&1)<<5|nalu[1]>>3 != 0 {
			return nil // nuh_layer_id > 0: not the base layer
		}
		typ := nalu[0] >> 1 & 0x3f
		isSPS, isPPS = typ == nalSPS265, typ == nalPPS265
	default:
		return fmt.Errorf("unknown codec %d", c)
	}
	if !isSPS && !isPPS {
		return nil
	}
	if isPPS {
		// A PPS of another codec than the stored sets finds no SPS (ParsePPS
		// checks the codec), so it fails without touching them.
		pps, err := parsePPS(c, nalu, p.sps)
		if err != nil {
			return fmt.Errorf("pps: %w", err)
		}
		p.pps[pps.ID], p.ppsRaw[pps.ID] = pps, bytes.Clone(nalu)
		return nil
	}
	sps, _, err := parseSPS(c, nalu)
	if err != nil {
		return fmt.Errorf("sps: %w", err)
	}
	if p.codec != c || p.sps == nil {
		*p = ParamSets{
			codec:  c,
			spsRaw: map[uint32][]byte{},
			sps:    map[uint32]*SPS{},
			ppsRaw: map[uint32][]byte{},
			pps:    map[uint32]*PPS{},
		}
	}
	if bytes.Equal(p.spsRaw[sps.ID], nalu) {
		return nil
	}
	p.sps[sps.ID], p.spsRaw[sps.ID] = sps, bytes.Clone(nalu)
	for id, pps := range p.pps {
		if pps.SPSID != sps.ID {
			continue
		}
		np, err := parsePPS(c, p.ppsRaw[id], p.sps)
		if err != nil {
			delete(p.pps, id)
			delete(p.ppsRaw, id)
			continue
		}
		p.pps[id] = np
	}
	return nil
}

// SPS returns the stored SPS with an ID, or nil. The SPS, including its
// OffsetForRefFrame and ShortTermRefPicSets slices, is shared with the
// ParamSets and must be treated as read-only.
func (p *ParamSets) SPS(id uint32) *SPS { return p.sps[id] }

// PPS returns the stored PPS with an ID, or nil. The PPS is shared with the
// ParamSets and must be treated as read-only.
func (p *ParamSets) PPS(id uint32) *PPS { return p.pps[id] }
