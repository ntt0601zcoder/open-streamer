package picture

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestThumbnailValidate(t *testing.T) {
	bad := []Thumbnail{{IntervalSec: 3601}, {IntervalSec: -1}, {Height: 181}, {Height: 722}, {Height: 1}, {Height: -2}, {Quality: 101}, {Quality: -1}}
	for _, th := range bad {
		if err := th.Validate(); err == nil {
			t.Errorf("%+v accepted", th)
		}
	}
	for _, th := range []Thumbnail{{}, {IntervalSec: 1, Height: 2, Quality: 1}, {IntervalSec: 3600, Height: 720, Quality: 100}} {
		if err := th.Validate(); err != nil {
			t.Errorf("%+v: %v", th, err)
		}
	}
}

func TestThumbnailResolved(t *testing.T) {
	if r := (Thumbnail{}).Resolved(); r != (Thumbnail{IntervalSec: 10, Height: 180, Quality: 80}) {
		t.Fatalf("%+v", r)
	}
	if r := (Thumbnail{IntervalSec: 3, Height: 90, Quality: 50}).Resolved(); r != (Thumbnail{IntervalSec: 3, Height: 90, Quality: 50}) {
		t.Fatalf("%+v", r)
	}
}

func TestSchedule(t *testing.T) {
	s := NewSchedule(2)
	if !s.Due(1, 0) {
		t.Fatal("first shot not due")
	}
	s.Took(1, 0)
	if s.Due(1, 179999) {
		t.Fatal("due before the interval")
	}
	if !s.Due(1, 180000) {
		t.Fatal("not due at the interval")
	}
	s.Took(1, 180000)
	if !s.Due(2, 0) {
		t.Fatal("new epoch not due")
	}
	s.Took(2, 900000)
	if !s.Due(2, 450000) {
		t.Fatal("PTS going back not due")
	}
}

func TestShotSize(t *testing.T) {
	tests := []struct{ sw, sh, n, d, h, w, wantH int }{
		{320, 240, 1, 1, 180, 240, 180},
		{320, 240, 1, 1, 720, 320, 240},
		{720, 576, 16, 15, 180, 240, 180},
		{1920, 1080, 0, 0, 180, 320, 180},
		{320, 241, 1, 1, 720, 318, 240},
		{1000, 10, 1, 1, 2, 200, 2},
		{2, 2, 1, 1, 180, 2, 2},
		{16, 1000, 1, 1, 180, 2, 180},
	}
	for _, tc := range tests {
		w, h := ShotSize(tc.sw, tc.sh, tc.n, tc.d, tc.h)
		if w != tc.w || h != tc.wantH {
			t.Errorf("ShotSize(%d,%d,%d,%d,%d) = %dx%d, want %dx%d", tc.sw, tc.sh, tc.n, tc.d, tc.h, w, h, tc.w, tc.wantH)
		}
	}
}

func TestEncodeJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range 16 {
		img.Set(i, i, color.White)
	}
	b, err := EncodeJPEG(img, 80)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds().Dx() != 16 || got.Bounds().Dy() != 16 {
		t.Fatalf("size %v", got.Bounds())
	}
}
