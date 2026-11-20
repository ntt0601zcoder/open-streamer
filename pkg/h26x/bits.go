package h26x

import (
	"errors"
	"fmt"
	"math/bits"
)

var (
	errTruncated = errors.New("truncated")
	errExpGolomb = errors.New("exp-Golomb code longer than 32 bits")
)

// unescape returns a copy of a NAL unit with every emulation prevention byte
// (the 0x03 of a 0x000003 sequence) removed.
func unescape(nalu []byte) []byte {
	out := make([]byte, 0, len(nalu))
	zeros := 0
	for _, b := range nalu {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// bitReader reads an RBSP most significant bit first. The first error is
// sticky: later reads return zero and leave it in place, so a parser checks
// err before every loop or allocation and once at the end.
type bitReader struct {
	buf []byte
	pos int // bit offset into buf
	err error
}

// left returns the number of unread bits.
func (r *bitReader) left() int { return len(r.buf)*8 - r.pos }

// fail records err unless an earlier error is already set.
func (r *bitReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// u reads n bits (n ≤ 32) as an unsigned value: u(n) in the specifications.
func (r *bitReader) u(n int) uint32 {
	if r.err != nil {
		return 0
	}
	if n > r.left() {
		r.fail(errTruncated)
		r.pos = len(r.buf) * 8
		return 0
	}
	var v uint32
	for range n {
		v = v<<1 | uint32(r.buf[r.pos>>3]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

// flag reads one bit.
func (r *bitReader) flag() bool { return r.u(1) == 1 }

// skip discards n bits.
func (r *bitReader) skip(n int) {
	for n > 0 && r.err == nil {
		k := min(n, 32)
		r.u(k)
		n -= k
	}
}

// ue reads an unsigned exp-Golomb code, ue(v). Codes with more than 31
// leading zero bits do not fit 32 bits and fail.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.err == nil && !r.flag() {
		zeros++
		if zeros > 31 {
			r.fail(errExpGolomb)
		}
	}
	if r.err != nil {
		return 0
	}
	if zeros == 0 {
		return 0
	}
	return uint32(1)<<zeros - 1 + r.u(zeros)
}

// se reads a signed exp-Golomb code, se(v).
func (r *bitReader) se() int32 {
	k := int64(r.ue())
	if k&1 == 1 {
		return int32((k + 1) / 2)
	}
	return int32(-k / 2)
}

// ueMax reads ue(v) and fails when the value exceeds limit. It returns 0 on
// any error, so the result is always safe as a loop bound.
func (r *bitReader) ueMax(name string, limit uint32) uint32 {
	v := r.ue()
	if r.err == nil && v > limit {
		r.fail(fmt.Errorf("%s %d out of range [0, %d]", name, v, limit))
	}
	if r.err != nil {
		return 0
	}
	return v
}

// seRange reads se(v) and fails when the value is outside [lo, hi]. It
// returns 0 on any error.
func (r *bitReader) seRange(name string, lo, hi int32) int32 {
	v := r.se()
	if r.err == nil && (v < lo || v > hi) {
		r.fail(fmt.Errorf("%s %d out of range [%d, %d]", name, v, lo, hi))
	}
	if r.err != nil {
		return 0
	}
	return v
}

// moreRBSPData reports whether syntax remains before the rbsp_stop_one_bit,
// the last set bit of the buffer: more_rbsp_data() in the specifications.
func (r *bitReader) moreRBSPData() bool {
	if r.err != nil {
		return false
	}
	for i := len(r.buf) - 1; i >= 0; i-- {
		if b := r.buf[i]; b != 0 {
			stop := i*8 + 7 - bits.TrailingZeros8(b)
			return r.pos < stop
		}
	}
	return false
}
