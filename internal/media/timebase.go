package media

import "time"

// ClockRate is the timebase of every Frame timestamp, in ticks per second.
const ClockRate = 90000

// divRound returns num/den rounded half away from zero. den must be positive.
func divRound(num, den int64) int64 {
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

// mulDivRound returns x*num/den rounded half away from zero, without
// overflowing on the intermediate product for large x. den must be positive.
func mulDivRound(x, num, den int64) int64 {
	q, r := x/den, x%den
	return q*num + divRound(r*num, den)
}

// Rescale converts a 90 kHz timestamp to the given rate, rounded half away from zero.
func Rescale(ts int64, rate int) int64 {
	return mulDivRound(ts, int64(rate), ClockRate)
}

// SampleTime converts a sample count at sampleRate to 90 kHz ticks, rounded half away from zero.
func SampleTime(samples int64, sampleRate int) int64 {
	return mulDivRound(samples, ClockRate, int64(sampleRate))
}

// ToDuration converts 90 kHz ticks to a time.Duration.
func ToDuration(ts int64) time.Duration {
	return time.Duration(mulDivRound(ts, int64(time.Second), ClockRate))
}

// FromDuration converts a time.Duration to 90 kHz ticks, rounded half away from zero.
func FromDuration(d time.Duration) int64 {
	return mulDivRound(int64(d), ClockRate, int64(time.Second))
}
