package media

// NALClassifier tracks one video track's key and RASL state across access
// units. It is independent of the container, so every demuxer shares it.
type NALClassifier struct {
	codec Codec
	// recovery: a recovery-point key was seen since the last IDR; its PTS
	// is keyPTS. Frames presented before it are leading pictures.
	recovery bool
	keyPTS   int64
}

// NewNALClassifier returns a classifier for video codec c.
func NewNALClassifier(c Codec) *NALClassifier {
	return &NALClassifier{codec: c}
}

// Classify returns Key and RASL for one access unit (NAL units without start
// codes) presented at pts. Access units must arrive in decode order.
//
// H.265 keys are IDR, CRA and BLA; RASL pictures are NAL types 8 and 9. An
// H.264 IDR is a key. An H.264 access unit whose slices are all I slices and
// that carries a recovery point SEI with recovery_frame_cnt 0 is a key as
// well (open GOP); until the next key, frames with a PTS below that key's
// are RASL.
func (c *NALClassifier) Classify(nalus [][]byte, pts int64) (key, rasl bool) {
	if c.codec == CodecH265 {
		for _, n := range nalus {
			if len(n) < 2 {
				continue
			}
			switch typ := (n[0] >> 1) & 0x3f; {
			case typ >= 16 && typ <= 21:
				key = true
			case typ == 8 || typ == 9:
				rasl = true
			}
		}
		return key, rasl
	}
	var (
		idr, sei, nonI bool
		slices         int
	)
	for _, n := range nalus {
		if len(n) < 2 {
			continue
		}
		switch n[0] & 0x1f {
		case 5:
			idr = true
			slices++
		case 1:
			slices++
			if !h264ISlice(n) {
				nonI = true
			}
		case 6:
			if h264RecoveryPoint0(n) {
				sei = true
			}
		}
	}
	switch {
	case idr:
		c.recovery = false
		return true, false
	case slices > 0 && !nonI && sei:
		c.recovery, c.keyPTS = true, pts
		return true, false
	case c.recovery && slices > 0 && pts < c.keyPTS:
		return false, true
	}
	return false, false
}

// h264ISlice reports whether a non-IDR slice NAL unit is an I slice
// (slice_type 2 or 7).
func h264ISlice(n []byte) bool {
	r := bitReader{b: unescape(n[1:])}
	if _, ok := r.ue(); !ok { // first_mb_in_slice
		return false
	}
	st, ok := r.ue()
	return ok && (st == 2 || st == 7)
}

// h264RecoveryPoint0 reports whether an SEI NAL unit carries a recovery
// point (payload type 6) with recovery_frame_cnt 0.
func h264RecoveryPoint0(n []byte) bool {
	b := unescape(n[1:])
	for len(b) > 2 {
		typ, size := 0, 0
		for len(b) > 0 && b[0] == 0xff {
			typ += 255
			b = b[1:]
		}
		if len(b) == 0 {
			return false
		}
		typ += int(b[0])
		b = b[1:]
		for len(b) > 0 && b[0] == 0xff {
			size += 255
			b = b[1:]
		}
		if len(b) == 0 {
			return false
		}
		size += int(b[0])
		b = b[1:]
		if size > len(b) {
			return false
		}
		if typ == 6 {
			r := bitReader{b: b[:size]}
			cnt, ok := r.ue()
			return ok && cnt == 0
		}
		b = b[size:]
	}
	return false
}

// unescape removes emulation-prevention bytes (00 00 03).
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, v := range b {
		if zeros >= 2 && v == 3 {
			zeros = 0
			continue
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// bitReader reads exp-Golomb codes from a byte slice.
type bitReader struct {
	b   []byte
	pos int // bit position
}

func (r *bitReader) bit() (uint, bool) {
	if r.pos>>3 >= len(r.b) {
		return 0, false
	}
	v := uint(r.b[r.pos>>3]>>(7-uint(r.pos&7))) & 1
	r.pos++
	return v, true
}

// ue reads an unsigned exp-Golomb code.
func (r *bitReader) ue() (uint, bool) {
	zeros := 0
	for {
		v, ok := r.bit()
		if !ok || zeros > 31 {
			return 0, false
		}
		if v == 1 {
			break
		}
		zeros++
	}
	var x uint
	for i := 0; i < zeros; i++ {
		v, ok := r.bit()
		if !ok {
			return 0, false
		}
		x = x<<1 | v
	}
	return 1<<uint(zeros) - 1 + x, true
}
