package mpegts

const (
	tsModulus = int64(1) << 33
	tsHalf    = int64(1) << 32
)

// TimeDecoder unwraps 33-bit MPEG-TS timestamps into a continuous int64 timeline that starts at 0.
//
// One decoder may be shared across tracks and fed both PTS and DTS values:
// every step is the signed difference to the previous input, so small
// backward jumps (B-frames, audio slightly behind video) are preserved.
// The zero value is ready to use. It is not safe for concurrent use.
type TimeDecoder struct {
	started bool
	prevIn  int64
	prevOut int64
}

// Decode returns the unwrapped timestamp for ts, a 90 kHz value in [0, 2^33).
// The first call returns 0. Each later call returns the previous output plus
// the difference to the previous input taken modulo 2^33 into [-2^32, 2^32).
func (d *TimeDecoder) Decode(ts int64) int64 {
	if !d.started {
		d.started = true
		d.prevIn = ts
		d.prevOut = 0
		return 0
	}
	diff := (ts-d.prevIn+tsHalf)%tsModulus - tsHalf
	if diff < -tsHalf {
		diff += tsModulus
	}
	d.prevIn = ts
	d.prevOut += diff
	return d.prevOut
}
