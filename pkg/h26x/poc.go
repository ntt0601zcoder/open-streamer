package h26x

// POCCounter derives the picture order counts of one stream's access units
// in decode order: H.264 §8.2.1 (pic_order_cnt_type 0, 1 and 2) and H.265
// §8.3.1. A counter is not safe for concurrent use.
type POCCounter struct {
	codec Codec
	ps    *ParamSets

	// started is set once a reference picture has been dated.
	started bool

	// H.264 pic_order_cnt_type 0: prevPicOrderCntMsb and
	// prevPicOrderCntLsb, from the previous reference picture.
	prevMsb, prevLsb int64
	// H.264 pic_order_cnt_type 1 and 2: FrameNumOffset and frame_num of the
	// previous picture.
	prevFrameNumOffset int64
	prevFrameNum       uint32

	// H.265: PicOrderCntVal of prevTid0Pic, and whether an end of sequence
	// or bitstream came after the last dated picture.
	prevTid0POC int64
	afterEOS    bool
}

// NewPOCCounter returns a counter for codec c that looks parameter sets up
// in ps. SPS and PPS NAL units found in access units are stored in ps
// (ParamSets.Update; a set that does not parse is ignored), so in-band
// parameter sets take effect for the slices that follow them. A nil ps
// starts empty.
func NewPOCCounter(c Codec, ps *ParamSets) *POCCounter {
	if ps == nil {
		ps = &ParamSets{}
	}
	return &POCCounter{codec: c, ps: ps}
}

// POC returns the picture order count of one access unit in decode order
// (NAL units without start codes), parsing the first slice header only up to
// the POC and dec_ref_pic_marking fields; ok=false when a parameter set is
// missing, the header does not parse, or no reference picture has been seen
// since the counter started. A counter started mid-stream begins at the first
// reference picture (H.265: TemporalId 0, not RASL/RADL or a sub-layer
// non-reference picture) with relative state (msb 0, FrameNumOffset 0), so
// POC differences inside the period are right even without an IDR.
//
// reset reports that the picture starts a new POC period: an IDR picture,
// H.264 memory_management_control_operation 5, an H.265 IRAP picture with
// NoRaslOutputFlag (IDR, BLA, the first CRA, a CRA after an end of sequence),
// and the first picture the counter dates. The POC of a picture with
// memory_management_control_operation 5 is the one after the reset (0).
// When an H.264 access unit holds both fields of a frame, POC returns the
// first field's and also counts the second.
//
// An access unit that fails leaves the counter's state unchanged.
func (pc *POCCounter) POC(au [][]byte) (poc int32, reset bool, ok bool) {
	switch pc.codec {
	case H264:
		return pc.poc264(au)
	case H265:
		return pc.poc265(au)
	default:
		return 0, false, false
	}
}

func (pc *POCCounter) poc264(au [][]byte) (poc int32, reset, ok bool) {
	var first slice264
	parsed, done := false, false
	for _, n := range au {
		if len(n) < 1 {
			continue
		}
		switch n[0] & 0x1f {
		case nalSPS264, nalPPS264:
			_ = pc.ps.Update(H264, n) // a set that does not parse leaves the stored ones
		case nalSlice264, nalSliceA264, nalIDR264:
			if done {
				continue
			}
			h, sps, err := parseSlice264(n, pc.ps)
			if !parsed {
				parsed = true
				if err != nil {
					done = true
					continue
				}
				first = h
				poc, reset, ok = pc.picture264(h, sps)
				done = !ok || !h.fieldPic
				continue
			}
			// Later slices of the first field are skipped; the first
			// slice of the opposite parity is the second field.
			if err == nil && h.fieldPic && h.bottomField != first.bottomField {
				pc.picture264(h, sps)
				done = true
			}
		}
	}
	return poc, reset, ok
}

// picture264 dates one H.264 picture and advances the state.
func (pc *POCCounter) picture264(h slice264, s *SPS) (int32, bool, bool) {
	start := !pc.started
	if start && h.refIDC == 0 {
		return 0, false, false
	}
	var top, bottom int64
	switch s.PicOrderCntType {
	case 0:
		top, bottom = pc.type0(h, s, start)
	case 1, 2:
		offset := pc.frameNumOffset(h, s, start)
		if s.PicOrderCntType == 1 {
			top, bottom = type1(h, s, offset)
		} else {
			top, bottom = type2(h, offset)
		}
		pc.prevFrameNumOffset, pc.prevFrameNum = offset, h.frameNum
	default:
		return 0, false, false // ParseSPS bounds pic_order_cnt_type to [0, 2]
	}
	poc := picOrderCnt264(h, top, bottom)
	if h.mmco5 {
		// After decoding, the picture's counts are relative to
		// tempPicOrderCnt (its own POC) and its frame_num is 0.
		top -= poc
		poc = 0
		pc.prevFrameNumOffset, pc.prevFrameNum = 0, 0
		pc.prevMsb, pc.prevLsb = 0, 0
		if !h.bottomField {
			pc.prevLsb = top
		}
	}
	pc.started = true
	return int32(poc), h.idr || h.mmco5 || start, true
}

// type0 applies pic_order_cnt_type 0 (§8.2.1.1) and returns
// TopFieldOrderCnt and BottomFieldOrderCnt (for a field, both hold its
// count). A reference picture becomes the previous reference picture.
func (pc *POCCounter) type0(h slice264, s *SPS, start bool) (top, bottom int64) {
	maxLsb := int64(1) << s.Log2MaxPicOrderCntLsb
	lsb := int64(h.pocLsb)
	var msb int64
	switch {
	case h.idr:
		msb = pocMsb(lsb, 0, 0, maxLsb)
	case start:
		msb = 0 // relative start
	default:
		msb = pocMsb(lsb, pc.prevLsb, pc.prevMsb, maxLsb)
	}
	if h.refIDC != 0 {
		pc.prevMsb, pc.prevLsb = msb, lsb
	}
	top = msb + lsb
	if h.fieldPic {
		return top, top
	}
	return top, top + int64(h.deltaPocBottom)
}

// pocMsb returns PicOrderCntMsb from the lsb of the current picture and the
// lsb and msb of the previous one (H.264 8-3, H.265 8-1).
func pocMsb(lsb, prevLsb, prevMsb, maxLsb int64) int64 {
	switch {
	case lsb < prevLsb && prevLsb-lsb >= maxLsb/2:
		return prevMsb + maxLsb
	case lsb > prevLsb && lsb-prevLsb > maxLsb/2:
		return prevMsb - maxLsb
	default:
		return prevMsb
	}
}

// frameNumOffset returns FrameNumOffset (§8.2.1.2, §8.2.1.3).
func (pc *POCCounter) frameNumOffset(h slice264, s *SPS, start bool) int64 {
	switch {
	case h.idr || start:
		return 0
	case pc.prevFrameNum > h.frameNum:
		return pc.prevFrameNumOffset + int64(1)<<s.Log2MaxFrameNum
	default:
		return pc.prevFrameNumOffset
	}
}

// type1 applies pic_order_cnt_type 1 (§8.2.1.2).
func type1(h slice264, s *SPS, frameNumOffset int64) (top, bottom int64) {
	n := int64(len(s.OffsetForRefFrame)) // at most 255
	var abs int64
	if n != 0 {
		abs = frameNumOffset + int64(h.frameNum)
	}
	if h.refIDC == 0 && abs > 0 {
		abs--
	}
	var expected int64
	if abs > 0 {
		var perCycle int64
		for _, o := range s.OffsetForRefFrame {
			perCycle += int64(o)
		}
		expected = (abs - 1) / n * perCycle
		for _, o := range s.OffsetForRefFrame[:(abs-1)%n+1] {
			expected += int64(o)
		}
	}
	if h.refIDC == 0 {
		expected += int64(s.OffsetForNonRefPic)
	}
	switch {
	case !h.fieldPic:
		top = expected + int64(h.deltaPoc[0])
		return top, top + int64(s.OffsetForTopToBottomField) + int64(h.deltaPoc[1])
	case !h.bottomField:
		top = expected + int64(h.deltaPoc[0])
		return top, top
	default:
		bottom = expected + int64(s.OffsetForTopToBottomField) + int64(h.deltaPoc[0])
		return bottom, bottom
	}
}

// type2 applies pic_order_cnt_type 2 (§8.2.1.3).
func type2(h slice264, frameNumOffset int64) (top, bottom int64) {
	var poc int64
	switch {
	case h.idr:
	case h.refIDC == 0:
		poc = 2*(frameNumOffset+int64(h.frameNum)) - 1
	default:
		poc = 2 * (frameNumOffset + int64(h.frameNum))
	}
	return poc, poc
}

// picOrderCnt264 returns PicOrderCnt(CurrPic) (8-1).
func picOrderCnt264(h slice264, top, bottom int64) int64 {
	switch {
	case !h.fieldPic:
		return min(top, bottom)
	case h.bottomField:
		return bottom
	default:
		return top
	}
}

func (pc *POCCounter) poc265(au [][]byte) (poc int32, reset, ok bool) {
	done := false
	for _, n := range au {
		if len(n) < 2 || (n[0]&1)<<5|n[1]>>3 != 0 {
			continue // too short, or not the base layer
		}
		switch typ := n[0] >> 1 & 0x3f; {
		case typ == nalSPS265 || typ == nalPPS265:
			_ = pc.ps.Update(H265, n) // a set that does not parse leaves the stored ones
		case typ == nalEOS265 || typ == nalEOB265:
			pc.afterEOS = true
		case !done && (typ <= nalRASLR265 || typ >= nalBLAWLP265 && typ <= nalCRA265):
			done = true
			if h, sps, err := parseSlice265(n, pc.ps); err == nil {
				poc, reset, ok = pc.picture265(h, sps)
			}
		}
	}
	return poc, reset, ok
}

// picture265 dates one H.265 picture (§8.3.1) and advances the state.
func (pc *POCCounter) picture265(h slice265, s *SPS) (int32, bool, bool) {
	irap := h.nalType >= nalBLAWLP265 && h.nalType <= nalIRAPMax
	leading := h.nalType >= nalRADLN265 && h.nalType <= nalRASLR265
	subLayerNonRef := h.nalType <= 14 && h.nalType%2 == 0
	tid0Pic := h.tid == 0 && !leading && !subLayerNonRef
	start := !pc.started
	if start && !tid0Pic {
		return 0, false, false
	}
	// IDR and BLA always have NoRaslOutputFlag; a CRA when it is the first
	// picture or follows an end of sequence.
	noRasl := irap && (h.nalType < nalCRA265 || start || pc.afterEOS)
	maxLsb := int64(1) << s.Log2MaxPicOrderCntLsb
	lsb := int64(h.pocLsb)
	var msb int64
	if !noRasl && !start {
		prevLsb := pc.prevTid0POC & (maxLsb - 1)
		msb = pocMsb(lsb, prevLsb, pc.prevTid0POC-prevLsb, maxLsb)
	}
	poc := msb + lsb
	if tid0Pic {
		pc.prevTid0POC = poc
	}
	pc.started, pc.afterEOS = true, false
	return int32(poc), noRasl || start, true
}
