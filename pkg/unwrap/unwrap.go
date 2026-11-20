// Package unwrap turns wrapping N-bit counters (RTMP's 32-bit milliseconds,
// RTP's 32-bit ticks) into a continuous int64 timeline.
package unwrap

// Unwrapper unwraps an N-bit timestamp. Each value is placed at the lap
// nearest the previous one, so small backward steps stay backward. Unlike
// mpegts.TimeDecoder it does not rebase: the first value is returned as is.
// It is not safe for concurrent use.
type Unwrapper struct {
	bits    uint
	mask    uint64
	last    int64
	started bool
}

// New returns an Unwrapper for bits-wide values. It panics unless 1 <= bits <= 63.
func New(bits uint) *Unwrapper {
	if bits < 1 || bits > 63 {
		panic("unwrap: bit width out of range 1..63")
	}
	return &Unwrapper{bits: bits, mask: 1<<bits - 1}
}

// Unwrap returns raw (taken modulo 2^bits) on the continuous timeline. The
// first call returns raw unchanged; each later call returns the previous
// output plus the difference to the previous input taken modulo 2^bits into
// [-2^(bits-1), 2^(bits-1)), so the exact half-range tie resolves negative,
// as in mpegts.TimeDecoder.
func (u *Unwrapper) Unwrap(raw uint64) int64 {
	raw &= u.mask
	if !u.started {
		u.started = true
		u.last = int64(raw)
		return u.last
	}
	prev := uint64(u.last) & u.mask
	diff := (raw - prev) & u.mask
	half := uint64(1) << (u.bits - 1)
	if diff >= half {
		u.last -= int64(u.mask + 1 - diff)
	} else {
		u.last += int64(diff)
	}
	return u.last
}

// Reset forgets the previous value: the next Unwrap returns its input as is.
func (u *Unwrapper) Reset() {
	u.started = false
	u.last = 0
}
