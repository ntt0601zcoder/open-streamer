package picture

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Position is where a watermark sits in the picture.
type Position uint8

// Watermark positions.
const (
	TopLeft Position = iota
	TopRight
	BottomLeft
	BottomRight
	Center
)

// Watermark limits.
const (
	MaxTextBytes  = 256
	MaxImageBytes = 1 << 20
	MaxImageSide  = 2048
)

// Watermark is the overlay drawn on every encoded picture. Text and Image are
// mutually exclusive.
type Watermark struct {
	Text       string   // Exclusive with Image.
	Image      []byte   // PNG or JPEG file bytes.
	Position   Position // Zero value is top-left.
	OpacityPct int      // 1..100; 0 means 100.
	SizePct    int      // Text: 1..50, 0 means 5; image: 1..100, 0 means 15.
	TimeZone   string   // IANA name; "" means UTC.
}

// Validate reports whether w can be built. An image is decoded in full, so
// what the host accepts the child can build.
func (w Watermark) Validate() error {
	hasText, hasImage := w.Text != "", len(w.Image) > 0
	switch {
	case !hasText && !hasImage:
		return errors.New("watermark: neither text nor image")
	case hasText && hasImage:
		return errors.New("watermark: text and image are exclusive")
	}
	if w.Position > Center {
		return fmt.Errorf("watermark: position %d out of range", w.Position)
	}
	if w.OpacityPct < 0 || w.OpacityPct > 100 {
		return fmt.Errorf("watermark: opacity %d out of range 0..100", w.OpacityPct)
	}
	if _, err := w.Location(); err != nil {
		return err
	}
	if hasText {
		if w.SizePct < 0 || w.SizePct > 50 {
			return fmt.Errorf("watermark: text size %d out of range 0..50", w.SizePct)
		}
		return validateText(w.Text)
	}
	if w.SizePct < 0 || w.SizePct > 100 {
		return fmt.Errorf("watermark: image size %d out of range 0..100", w.SizePct)
	}
	if _, err := DecodeImage(w.Image); err != nil {
		return fmt.Errorf("watermark: %w", err)
	}
	return nil
}

func validateText(s string) error {
	if len(s) > MaxTextBytes {
		return fmt.Errorf("watermark: text is %d bytes, over %d", len(s), MaxTextBytes)
	}
	if !utf8.ValidString(s) {
		return errors.New("watermark: text is not valid UTF-8")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' {
			if i+1 >= len(s) {
				return errors.New("watermark: text ends in a lone %")
			}
			i++
			if !isDirective(s[i]) && s[i] != '%' {
				return fmt.Errorf("watermark: unknown directive %%%c", s[i])
			}
		}
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("watermark: text has a control character")
		}
	}
	return nil
}

func isDirective(c byte) bool {
	switch c {
	case 'Y', 'm', 'd', 'H', 'M', 'S':
		return true
	}
	return false
}

// Resolved returns w with defaults filled in. Call it after Validate.
func (w Watermark) Resolved() Watermark {
	if w.OpacityPct == 0 {
		w.OpacityPct = 100
	}
	if w.SizePct == 0 {
		if len(w.Image) > 0 {
			w.SizePct = 15
		} else {
			w.SizePct = 5
		}
	}
	return w
}

// Location returns the time zone of the text clock.
func (w Watermark) Location() (*time.Location, error) {
	if w.TimeZone == "" {
		return time.UTC, nil
	}
	if w.TimeZone == "Local" {
		return nil, errors.New("watermark: time zone must be an IANA name, not Local")
	}
	loc, err := time.LoadLocation(w.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("watermark: time zone %q: %w", w.TimeZone, err)
	}
	return loc, nil
}

// HasTime reports whether text has any directive other than %%.
func HasTime(text string) bool {
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '%' {
			continue
		}
		if text[i+1] != '%' {
			return true
		}
		i++
	}
	return false
}

// FormatText expands the directives of text for t, which is already in the
// watermark's zone. %% becomes %.
func FormatText(text string, t time.Time) string {
	if !strings.Contains(text, "%") {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '%' || i+1 >= len(text) {
			b.WriteByte(c)
			continue
		}
		i++
		switch text[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(text[i])
		}
	}
	return b.String()
}

// DecodeImage decodes a PNG or JPEG within the limits (size, then
// DecodeConfig, then a full decode) as straight-alpha NRGBA.
func DecodeImage(b []byte) (*image.NRGBA, error) {
	if len(b) == 0 {
		return nil, errors.New("image: empty")
	}
	if len(b) > MaxImageBytes {
		return nil, fmt.Errorf("image: %d bytes, over %d", len(b), MaxImageBytes)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("image: config: %w", err)
	}
	if format != "png" && format != "jpeg" {
		return nil, fmt.Errorf("image: format %q is not PNG or JPEG", format)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxImageSide || cfg.Height > MaxImageSide {
		return nil, fmt.Errorf("image: %dx%d outside 1..%d per side", cfg.Width, cfg.Height, MaxImageSide)
	}
	var img image.Image
	if format == "png" {
		img, err = png.Decode(bytes.NewReader(b))
	} else {
		img, err = jpeg.Decode(bytes.NewReader(b))
	}
	if err != nil {
		return nil, fmt.Errorf("image: decode: %w", err)
	}
	if n, ok := img.(*image.NRGBA); ok && n.Rect.Min == (image.Point{}) {
		return n, nil
	}
	out := image.NewNRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	return out, nil
}

// PositionExpr returns the x and y expressions that place a w × h overlay in
// a W × H picture with the given margin.
func PositionExpr(p Position, bigW, bigH, w, h string, margin int) (x, y string) {
	left, top := fmt.Sprint(margin), fmt.Sprint(margin)
	right := fmt.Sprintf("%s-%s-%d", bigW, w, margin)
	bottom := fmt.Sprintf("%s-%s-%d", bigH, h, margin)
	switch p {
	case TopLeft:
		return left, top
	case TopRight:
		return right, top
	case BottomLeft:
		return left, bottom
	case BottomRight:
		return right, bottom
	case Center:
		return fmt.Sprintf("(%s-%s)/2", bigW, w), fmt.Sprintf("(%s-%s)/2", bigH, h)
	}
	return left, top
}

// Margin is 2 % of the picture height, rounded, and at least 1.
func Margin(picH int) int {
	return max(1, int(math.Round(0.02*float64(picH))))
}

var assetName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$`)

// LoadAsset reads the asset name from dir. The name must match the asset
// rule, so it cannot leave dir; the file is at most MaxImageBytes.
func LoadAsset(dir, name string) ([]byte, error) {
	if !assetName.MatchString(name) {
		return nil, fmt.Errorf("picture: asset name %q is not allowed", name)
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("picture: open asset: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("picture: read asset %q: %w", name, err)
	}
	if len(b) > MaxImageBytes {
		return nil, fmt.Errorf("picture: asset %q is over %d bytes", name, MaxImageBytes)
	}
	return b, nil
}
