package h26x

import (
	"errors"
	"fmt"
)

// NAL unit types of slices and stream boundaries.
const (
	nalSlice264  = 1  // coded slice of a non-IDR picture
	nalSliceA264 = 2  // coded slice data partition A, which carries the slice header
	nalIDR264    = 5  // coded slice of an IDR picture
	nalRADLN265  = 6  // RADL_N
	nalRASLR265  = 9  // RASL_R
	nalBLAWLP265 = 16 // BLA_W_LP, the first IRAP type
	nalIDRWR265  = 19 // IDR_W_RADL
	nalIDRNLP265 = 20 // IDR_N_LP
	nalCRA265    = 21 // CRA_NUT
	nalIRAPMax   = 23 // RSV_IRAP_VCL23, the last IRAP type
	nalEOS265    = 36 // EOS_NUT
	nalEOB265    = 37 // EOB_NUT
)

// Sanity bounds on slice header syntax that the specifications bound only
// indirectly.
const (
	// maxSliceHeader is the prefix of a slice NAL unit that is unescaped
	// for header parsing; a header of a conforming stream is far shorter.
	maxSliceHeader = 16 << 10
	// maxMMCO bounds memory_management_control_operation entries per
	// slice: every operation but 4 and 5 acts on one of at most 32
	// reference fields, so 2·32 + 2 is above anything a decoder can apply.
	maxMMCO = 66
)

var errMissingParams = errors.New("missing parameter set")

// slice264 holds the fields of an H.264 slice header (§7.3.3) that picture
// order count derivation needs.
type slice264 struct {
	refIDC         uint8 // nal_ref_idc
	idr            bool  // IdrPicFlag
	frameNum       uint32
	fieldPic       bool
	bottomField    bool
	pocLsb         uint32
	deltaPocBottom int32
	deltaPoc       [2]int32
	mmco5          bool // dec_ref_pic_marking holds memory_management_control_operation 5
}

// slice265 holds the fields of an H.265 slice segment header (§7.3.6.1)
// that picture order count derivation needs.
type slice265 struct {
	nalType uint8
	tid     uint8 // TemporalId
	pocLsb  uint32
}

// sliceReader returns a bit reader over the unescaped head of a slice NAL
// unit, positioned after its hdr-byte NAL header.
func sliceReader(nalu []byte, hdr int) *bitReader {
	return &bitReader{buf: unescape(nalu[:min(len(nalu), maxSliceHeader)]), pos: hdr * 8}
}

// lookup returns the PPS with an ID and the SPS it references, both of
// codec c.
func lookup(ps *ParamSets, c Codec, ppsID uint32) (*PPS, *SPS, error) {
	pps := ps.PPS(ppsID)
	if pps == nil || pps.Codec != c {
		return nil, nil, fmt.Errorf("%w: pps %d", errMissingParams, ppsID)
	}
	sps := ps.SPS(pps.SPSID)
	if sps == nil || sps.Codec != c {
		return nil, nil, fmt.Errorf("%w: sps %d", errMissingParams, pps.SPSID)
	}
	return pps, sps, nil
}

// parseSlice264 parses an H.264 slice header (NAL unit types 1, 2 and 5,
// with the NAL header, without a start code) through dec_ref_pic_marking.
func parseSlice264(nalu []byte, ps *ParamSets) (slice264, *SPS, error) {
	var h slice264
	if len(nalu) < 1 {
		return h, nil, errTruncated
	}
	nalType := nalu[0] & 0x1f
	h.refIDC = nalu[0] >> 5 & 3
	h.idr = nalType == nalIDR264
	r := sliceReader(nalu, 1)
	r.ue() // first_mb_in_slice
	sliceType := r.ueMax("slice_type", 9) % 5
	ppsID := r.ueMax("pic_parameter_set_id", 255)
	if r.err != nil {
		return h, nil, r.err
	}
	pps, sps, err := lookup(ps, H264, ppsID)
	if err != nil {
		return h, nil, err
	}
	if sps.SeparateColourPlane {
		r.skip(2) // colour_plane_id
	}
	h.frameNum = r.u(int(sps.Log2MaxFrameNum))
	if !sps.FrameMbsOnly {
		h.fieldPic = r.flag()
		if h.fieldPic {
			h.bottomField = r.flag()
		}
	}
	if h.idr {
		r.ueMax("idr_pic_id", 65535)
	}
	switch sps.PicOrderCntType {
	case 0:
		h.pocLsb = r.u(int(sps.Log2MaxPicOrderCntLsb))
		if pps.BottomFieldPicOrderInFramePresent && !h.fieldPic {
			h.deltaPocBottom = r.se()
		}
	case 1:
		if !sps.DeltaPicOrderAlwaysZero {
			h.deltaPoc[0] = r.se()
			if pps.BottomFieldPicOrderInFramePresent && !h.fieldPic {
				h.deltaPoc[1] = r.se()
			}
		}
	}
	if pps.RedundantPicCntPresent {
		r.ueMax("redundant_pic_cnt", 127)
	}
	const sliceP, sliceB, sliceI, sliceSP, sliceSI = 0, 1, 2, 3, 4
	if sliceType == sliceB {
		r.flag() // direct_spatial_mv_pred_flag
	}
	numL0, numL1 := pps.NumRefIdxL0DefaultActiveMinus1, pps.NumRefIdxL1DefaultActiveMinus1
	if sliceType == sliceP || sliceType == sliceSP || sliceType == sliceB {
		if r.flag() { // num_ref_idx_active_override_flag
			numL0 = r.ueMax("num_ref_idx_l0_active_minus1", 31)
			if sliceType == sliceB {
				numL1 = r.ueMax("num_ref_idx_l1_active_minus1", 31)
			}
		}
	}
	// ref_pic_list_modification() (§7.3.3.1); NAL unit types 20 and 21,
	// which use the MVC form, are not parsed here.
	if sliceType != sliceI && sliceType != sliceSI {
		refPicListModification264(r, 0, numL0)
	}
	if sliceType == sliceB {
		refPicListModification264(r, 1, numL1)
	}
	if (pps.WeightedPred && (sliceType == sliceP || sliceType == sliceSP)) ||
		(pps.WeightedBipredIDC == 1 && sliceType == sliceB) {
		predWeightTable264(r, sps.ChromaArrayType(), numL0, numL1, sliceType == sliceB)
	}
	if h.refIDC != 0 {
		h.mmco5 = decRefPicMarking264(r, h.idr)
	}
	if r.err != nil {
		return h, nil, r.err
	}
	return h, sps, nil
}

// refPicListModification264 skips the modification loop of one reference
// picture list. Each operation but the final 3 places one entry of a list of
// num+1 entries, so more than num+1 of them fail.
func refPicListModification264(r *bitReader, list int, num uint32) {
	if !r.flag() { // ref_pic_list_modification_flag_lX
		return
	}
	for ops := uint32(0); r.err == nil; ops++ {
		idc := r.ueMax("modification_of_pic_nums_idc", 3)
		if r.err != nil || idc == 3 {
			return
		}
		if ops > num {
			r.fail(fmt.Errorf("ref_pic_list_modification l%d has more than %d operations", list, num+1))
			return
		}
		r.ue() // abs_diff_pic_num_minus1 or long_term_pic_num
	}
}

// predWeightTable264 skips pred_weight_table() (§7.3.3.2).
func predWeightTable264(r *bitReader, chromaArrayType uint8, numL0, numL1 uint32, bSlice bool) {
	r.ueMax("luma_log2_weight_denom", 7)
	if chromaArrayType != 0 {
		r.ueMax("chroma_log2_weight_denom", 7)
	}
	lists := []uint32{numL0}
	if bSlice {
		lists = append(lists, numL1)
	}
	for _, num := range lists {
		for i := uint32(0); i <= num && r.err == nil; i++ {
			if r.flag() { // luma_weight_lX_flag
				r.se() // luma_weight_lX
				r.se() // luma_offset_lX
			}
			if chromaArrayType != 0 && r.flag() { // chroma_weight_lX_flag
				for range 2 {
					r.se() // chroma_weight_lX
					r.se() // chroma_offset_lX
				}
			}
		}
	}
}

// decRefPicMarking264 parses dec_ref_pic_marking() (§7.3.3.3) and reports
// whether it holds memory_management_control_operation 5.
func decRefPicMarking264(r *bitReader, idr bool) (mmco5 bool) {
	if idr {
		r.skip(2) // no_output_of_prior_pics_flag, long_term_reference_flag
		return false
	}
	if !r.flag() { // adaptive_ref_pic_marking_mode_flag
		return false
	}
	for n := 0; r.err == nil; n++ {
		op := r.ueMax("memory_management_control_operation", 6)
		if r.err != nil || op == 0 {
			break
		}
		if n >= maxMMCO {
			r.fail(fmt.Errorf("more than %d memory_management_control_operation entries", maxMMCO))
			break
		}
		switch op {
		case 1, 2, 4, 6:
			// difference_of_pic_nums_minus1, long_term_pic_num,
			// max_long_term_frame_idx_plus1 or long_term_frame_idx.
			r.ue()
		case 3:
			r.ue() // difference_of_pic_nums_minus1
			r.ue() // long_term_frame_idx
		case 5:
			mmco5 = true
		}
	}
	return mmco5
}

// parseSlice265 parses an H.265 slice segment header (with the NAL header,
// without a start code) through slice_pic_order_cnt_lsb. A dependent slice
// segment carries no picture order count and fails.
func parseSlice265(nalu []byte, ps *ParamSets) (slice265, *SPS, error) {
	var h slice265
	if len(nalu) < 2 {
		return h, nil, errTruncated
	}
	h.nalType = nalu[0] >> 1 & 0x3f
	tidPlus1 := nalu[1] & 7
	if tidPlus1 == 0 {
		return h, nil, errors.New("nuh_temporal_id_plus1 0")
	}
	h.tid = tidPlus1 - 1
	r := sliceReader(nalu, 2)
	first := r.flag() // first_slice_segment_in_pic_flag
	if h.nalType >= nalBLAWLP265 && h.nalType <= nalIRAPMax {
		r.flag() // no_output_of_prior_pics_flag
	}
	ppsID := r.ueMax("slice_pic_parameter_set_id", 63)
	if r.err != nil {
		return h, nil, r.err
	}
	pps, sps, err := lookup(ps, H265, ppsID)
	if err != nil {
		return h, nil, err
	}
	if !first {
		dependent := pps.DependentSliceSegmentsEnabled && r.flag()
		size := sps.PicSizeInCtbsY()
		addr := r.u(ceilLog2(size))
		switch {
		case r.err != nil:
		case addr >= size:
			r.fail(fmt.Errorf("slice_segment_address %d out of range [0, %d]", addr, size-1))
		case dependent:
			r.fail(errors.New("dependent slice segment carries no slice_pic_order_cnt_lsb"))
		}
	}
	r.skip(int(pps.NumExtraSliceHeaderBits)) // slice_reserved_flag[i]
	r.ueMax("slice_type", 2)
	if pps.OutputFlagPresent {
		r.flag() // pic_output_flag
	}
	if sps.SeparateColourPlane {
		r.skip(2) // colour_plane_id
	}
	if h.nalType != nalIDRWR265 && h.nalType != nalIDRNLP265 {
		h.pocLsb = r.u(int(sps.Log2MaxPicOrderCntLsb))
	}
	if r.err != nil {
		return h, nil, r.err
	}
	return h, sps, nil
}
