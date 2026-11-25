package picture

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

// Thumbnail configures the periodic preview picture.
type Thumbnail struct{ IntervalSec, Height, Quality int }

// MaxShotBytes caps one encoded shot.
const MaxShotBytes = 4 << 20

// Validate checks the ranges; zero values mean the default.
func (t Thumbnail) Validate() error {
	if t.IntervalSec < 0 || t.IntervalSec > 3600 {
		return fmt.Errorf("thumbnail: interval %d out of range 0..3600", t.IntervalSec)
	}
	if t.Height < 0 || t.Height > 720 || (t.Height != 0 && (t.Height < 2 || t.Height%2 != 0)) {
		return fmt.Errorf("thumbnail: height %d must be even, 2..720", t.Height)
	}
	if t.Quality < 0 || t.Quality > 100 {
		return fmt.Errorf("thumbnail: quality %d out of range 0..100", t.Quality)
	}
	return nil
}

// Resolved fills the defaults: 10 s, 180 px, quality 80.
func (t Thumbnail) Resolved() Thumbnail {
	if t.IntervalSec == 0 {
		t.IntervalSec = 10
	}
	if t.Height == 0 {
		t.Height = 180
	}
	if t.Quality == 0 {
		t.Quality = 80
	}
	return t
}

// Shot is one encoded thumbnail.
type Shot struct {
	PTS           int64
	Epoch         uint32
	Width, Height int
	JPEG          []byte
}

// Schedule decides, in stream time (90 kHz), when the next shot is due. It is
// not safe for concurrent use.
type Schedule struct {
	interval int64
	have     bool
	epoch    uint32
	last     int64
}

// NewSchedule returns a schedule with an interval of intervalSec seconds.
func NewSchedule(intervalSec int) *Schedule {
	return &Schedule{interval: int64(intervalSec) * 90000}
}

// Due reports whether a shot should be taken at pts: none yet, another epoch,
// pts at least one interval past the last shot, or pts before it.
func (s *Schedule) Due(epoch uint32, pts int64) bool {
	return !s.have || epoch != s.epoch || pts < s.last || pts >= s.last+s.interval
}

// Took records that a shot was taken at pts.
func (s *Schedule) Took(epoch uint32, pts int64) {
	s.have, s.epoch, s.last = true, epoch, pts
}

// ShotSize returns the size of a shot of a srcW × srcH picture with sample
// aspect ratio sarNum:sarDen at the given height. The height is min(height,
// srcH) rounded down to even, at least 2; the width keeps the display aspect
// ratio, rounded to the nearest even, at least 2. A SAR with a zero is 1:1.
func ShotSize(srcW, srcH, sarNum, sarDen, height int) (w, h int) {
	if srcW <= 0 || srcH <= 0 {
		return 2, 2
	}
	h = max(min(height, srcH)&^1, 2)
	if sarNum <= 0 || sarDen <= 0 {
		sarNum, sarDen = 1, 1
	}
	num := int64(h) * int64(srcW) * int64(sarNum)
	den := int64(srcH) * int64(sarDen)
	w = int((num+den)/(2*den)) * 2
	return max(w, 2), h
}

// EncodeJPEG encodes img at the given quality.
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("picture: encode jpeg: %w", err)
	}
	if buf.Len() > MaxShotBytes {
		return nil, fmt.Errorf("picture: shot is %d bytes, over %d", buf.Len(), MaxShotBytes)
	}
	return buf.Bytes(), nil
}
