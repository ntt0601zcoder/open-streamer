package mpegts

const (
	tsModulus = int64(1) << 33
	tsHalf    = int64(1) << 32
)

// anchor is where a TimeDecoder's first output lands.
type anchor uint8

const (
	anchorZero anchor = iota // the first output is 0
	anchorRaw                // the first output is the input
	anchorNear               // the first output is the input's lap nearest ref
)

// TimeDecoder unwraps 33-bit MPEG-TS timestamps into a continuous int64 timeline that starts at 0.
//
// One decoder may be shared across tracks and fed both PTS and DTS values:
// every step is the signed difference to the previous input, so small
// backward jumps (B-frames, audio slightly behind video) are preserved.
// The zero value is ready to use and starts its timeline at 0; AnchorRaw and
// AnchorNear keep it on the raw 33-bit timeline instead, so that decoders
// over different streams of one program stay comparable. It is not safe for
// concurrent use.
type TimeDecoder struct {
	started bool
	anchor  anchor
	ref     int64
	prevIn  int64
	prevOut int64
}

// AnchorRaw makes the first Decode return its input unchanged: the timeline
// is the raw 33-bit one, unwrapped from there. It has no effect once Decode
// was called.
func (d *TimeDecoder) AnchorRaw() {
	if !d.started {
		d.anchor = anchorRaw
	}
}

// AnchorNear makes the first Decode return the value that equals its input
// modulo 2^33 and lies nearest ref, an output of another decoder on the same
// raw timeline: both then stay comparable across a wrap of the 33-bit
// counter. It has no effect once Decode was called.
func (d *TimeDecoder) AnchorNear(ref int64) {
	if !d.started {
		d.anchor, d.ref = anchorNear, ref
	}
}

// Decode returns the unwrapped timestamp for ts, a 90 kHz value in [0, 2^33).
// The first call returns 0, or what an anchor set. Each later call returns
// the previous output plus the difference to the previous input taken modulo
// 2^33 into [-2^32, 2^32).
func (d *TimeDecoder) Decode(ts int64) int64 {
	if !d.started {
		d.started = true
		d.prevIn = ts
		switch d.anchor {
		case anchorZero:
			d.prevOut = 0
		case anchorRaw:
			d.prevOut = ts
		case anchorNear:
			d.prevOut = d.ref + wrapDiff(ts-d.ref)
		}
		return d.prevOut
	}
	d.prevOut += wrapDiff(ts - d.prevIn)
	d.prevIn = ts
	return d.prevOut
}

// wrapDiff returns the value equal to x modulo 2^33 in [-2^32, 2^32).
func wrapDiff(x int64) int64 {
	x = (x + tsHalf) % tsModulus
	if x < 0 {
		x += tsModulus
	}
	return x - tsHalf
}
