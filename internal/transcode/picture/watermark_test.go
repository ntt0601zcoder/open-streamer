package picture

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestWatermarkValidate(t *testing.T) {
	var g bytes.Buffer
	if err := gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	big := pngBytes(t, 64, 64)
	cut := big[:33] // signature + IHDR chunk: DecodeConfig passes, the decode fails.
	tests := []struct {
		name string
		w    Watermark
		ok   bool
	}{
		{"neither", Watermark{}, false},
		{"both", Watermark{Text: "x", Image: big}, false},
		{"text 256", Watermark{Text: strings.Repeat("a", 256)}, true},
		{"text 257", Watermark{Text: strings.Repeat("a", 257)}, false},
		{"newline", Watermark{Text: "a\nb"}, false},
		{"nul", Watermark{Text: "a\x00"}, false},
		{"bad utf8", Watermark{Text: "a\xffb"}, false},
		{"%Q", Watermark{Text: "%Q"}, false},
		{"trailing %", Watermark{Text: "a%"}, false},
		{"directives", Watermark{Text: "%Y-%m-%d %H:%M:%S %%"}, true},
		{"literal", Watermark{Text: ":'\\%%{localtime},[in]"}, true},
		{"opacity 101", Watermark{Text: "x", OpacityPct: 101}, false},
		{"opacity -1", Watermark{Text: "x", OpacityPct: -1}, false},
		{"text size 51", Watermark{Text: "x", SizePct: 51}, false},
		{"text size 50", Watermark{Text: "x", SizePct: 50}, true},
		{"image size 101", Watermark{Image: big, SizePct: 101}, false},
		{"image size 100", Watermark{Image: big, SizePct: 100}, true},
		{"position 5", Watermark{Text: "x", Position: 5}, false},
		{"bad zone", Watermark{Text: "x", TimeZone: "Mars/Phobos"}, false},
		{"zone", Watermark{Text: "x", TimeZone: "Asia/Ho_Chi_Minh"}, true},
		{"gif", Watermark{Image: g.Bytes()}, false},
		{"wide png", Watermark{Image: pngBytes(t, 2049, 1)}, false},
		{"png 2048", Watermark{Image: pngBytes(t, 2048, 1)}, true},
		{"over 1 MiB", Watermark{Image: append(append([]byte{}, big...), make([]byte, MaxImageBytes)...)}, false},
		{"cut png", Watermark{Image: cut}, false},
		{"png", Watermark{Image: big}, true},
		{"jpeg", Watermark{Image: jpegBytes(t, 64, 64)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestDecodeImageAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 100, B: 50, A: 128})
	var b bytes.Buffer
	if err := png.Encode(&b, src); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeImage(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if c := got.NRGBAAt(0, 0); c != (color.NRGBA{R: 200, G: 100, B: 50, A: 128}) {
		t.Fatalf("pixel = %+v", c)
	}
}

func TestWatermarkResolved(t *testing.T) {
	if r := (Watermark{Text: "x"}).Resolved(); r.OpacityPct != 100 || r.SizePct != 5 {
		t.Fatalf("text: %+v", r)
	}
	if r := (Watermark{Image: []byte{1}}).Resolved(); r.OpacityPct != 100 || r.SizePct != 15 {
		t.Fatalf("image: %+v", r)
	}
	if r := (Watermark{Text: "x", OpacityPct: 40, SizePct: 9}).Resolved(); r.OpacityPct != 40 || r.SizePct != 9 {
		t.Fatalf("explicit: %+v", r)
	}
}

func TestLocation(t *testing.T) {
	if l, err := (Watermark{}).Location(); err != nil || l != time.UTC {
		t.Fatalf("empty zone: %v %v", l, err)
	}
	if _, err := (Watermark{TimeZone: "Local"}).Location(); err == nil {
		t.Fatal("Local accepted")
	}
}

func TestFormatText(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 3, 9, 7, 5, 3, 0, loc)
	for in, want := range map[string]string{
		"%Y-%m-%d %H:%M:%S": "2026-03-09 07:05:03",
		"100%% %H":          "100% 07",
		"plain":             "plain",
	} {
		if got := FormatText(in, ts); got != want {
			t.Errorf("FormatText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasTime(t *testing.T) {
	if HasTime("%%") || HasTime("plain") || HasTime("%%%%") {
		t.Fatal("no directive expected")
	}
	if !HasTime("at %H") || !HasTime("%%%H") {
		t.Fatal("directive expected")
	}
}

func TestPositionExpr(t *testing.T) {
	want := map[Position][2]string{
		TopLeft:     {"9", "9"},
		TopRight:    {"W-w-9", "9"},
		BottomLeft:  {"9", "H-h-9"},
		BottomRight: {"W-w-9", "H-h-9"},
		Center:      {"(W-w)/2", "(H-h)/2"},
	}
	for p, w := range want {
		x, y := PositionExpr(p, "W", "H", "w", "h", 9)
		if x != w[0] || y != w[1] {
			t.Errorf("position %d = %q,%q want %q,%q", p, x, y, w[0], w[1])
		}
	}
}

func TestMargin(t *testing.T) {
	for h, want := range map[int]int{1080: 22, 720: 14, 180: 4, 10: 1, 0: 1} {
		if got := Margin(h); got != want {
			t.Errorf("Margin(%d) = %d, want %d", h, got, want)
		}
	}
}

func TestLoadAsset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.png"), make([]byte, MaxImageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge.png"), make([]byte, MaxImageBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := LoadAsset(dir, "logo.png"); err != nil || string(b) != "data" {
		t.Fatalf("logo.png: %q %v", b, err)
	}
	if b, err := LoadAsset(dir, "edge.png"); err != nil || len(b) != MaxImageBytes {
		t.Fatalf("edge.png: %d %v", len(b), err)
	}
	for _, name := range []string{"../logo.png", "Logo.png", "logo.gif", "", "a/b.png", "huge.png", "missing.png"} {
		if _, err := LoadAsset(dir, name); err == nil {
			t.Errorf("LoadAsset(%q) succeeded", name)
		}
	}
	// A bad name is rejected without touching the disk: a missing dir still gives the name error.
	if _, err := LoadAsset(filepath.Join(dir, "nope"), "../x.png"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("bad name error = %v", err)
	}
}
