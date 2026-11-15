package media

const (
	// sliceHeaderBytes is how much of a slice NAL unit, unescaped, is read
	// for first_mb_in_slice and slice_type: two exp-Golomb codes of at most
	// 65 bits each.
	sliceHeaderBytes = 32
	// maxSEIScan caps the escaped bytes of an SEI NAL unit walked for a
	// recovery point; payloads past it are not looked at.
	maxSEIScan = 4096
)

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
//
// Only slice headers and SEI payload headers are read, without allocating:
// the cost does not grow with the slice size.
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
			// One non-I slice decides it: the others need no look.
			if !nonI && !h264ISlice(n) {
				nonI = true
			}
		case 6:
			if !sei && !nonI && h264RecoveryPoint0(n) {
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
	var buf [sliceHeaderBytes]byte
	e := ebsp{b: n[1:]}
	k := 0
	for k < len(buf) {
		v, ok := e.next()
		if !ok {
			break
		}
		buf[k] = v
		k++
	}
	r := bitReader{b: buf[:k]}
	if _, ok := r.ue(); !ok { // first_mb_in_slice
		return false
	}
	st, ok := r.ue()
	return ok && (st == 2 || st == 7)
}

// h264RecoveryPoint0 reports whether an SEI NAL unit carries a recovery
// point (payload type 6) with recovery_frame_cnt 0.
//
// It walks the payload headers over at most maxSEIScan escaped bytes,
// unescaping as it reads, and copies out only the start of the recovery
// point payload.
func h264RecoveryPoint0(n []byte) bool {
	e := ebsp{b: n[1:min(len(n), 1+maxSEIScan)]}
	for e.left() > 2 {
		typ, ok := e.seiValue()
		if !ok {
			return false
		}
		size, ok := e.seiValue()
		if !ok {
			return false
		}
		if typ != 6 {
			if !e.skip(size) {
				return false
			}
			continue
		}
		// recovery_frame_cnt is one exp-Golomb code of at most 33 bits.
		var buf [8]byte
		k := 0
		for k < min(size, len(buf)) {
			v, ok := e.next()
			if !ok {
				return false
			}
			buf[k] = v
			k++
		}
		if k < min(size, len(buf)) {
			return false
		}
		r := bitReader{b: buf[:k]}
		cnt, ok := r.ue()
		return ok && cnt == 0
	}
	return false
}

// ebsp reads the bytes of an escaped NAL unit payload, dropping the
// emulation-prevention bytes (the 03 of 00 00 03) as it goes.
type ebsp struct {
	b     []byte
	i     int
	zeros int
}

// next returns the next unescaped byte.
func (e *ebsp) next() (byte, bool) {
	for e.i < len(e.b) {
		v := e.b[e.i]
		e.i++
		if e.zeros >= 2 && v == 3 {
			e.zeros = 0
			continue
		}
		if v == 0 {
			e.zeros++
		} else {
			e.zeros = 0
		}
		return v, true
	}
	return 0, false
}

// left returns the escaped bytes not read yet, an upper bound of the
// unescaped ones.
func (e *ebsp) left() int { return len(e.b) - e.i }

// skip drops n unescaped bytes and reports whether there were as many.
func (e *ebsp) skip(n int) bool {
	for range n {
		if _, ok := e.next(); !ok {
			return false
		}
	}
	return true
}

// seiValue reads an SEI payload type or size: 0xFF bytes adding 255 each,
// then a final byte.
func (e *ebsp) seiValue() (int, bool) {
	x := 0
	for {
		v, ok := e.next()
		if !ok {
			return 0, false
		}
		x += int(v)
		if v != 0xff {
			return x, true
		}
	}
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
