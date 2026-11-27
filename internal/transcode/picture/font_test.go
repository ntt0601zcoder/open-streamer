package picture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureFont = "../../../testdata/fixtures/fonts/DejaVuSansMono.ttf"

func readFixtureFont(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(fixtureFont)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tableRecord returns the offset of the record of the table tag in b.
func tableRecord(t *testing.T, b []byte, tag string) int {
	t.Helper()
	n := int(binary.BigEndian.Uint16(b[4:]))
	for i := range n {
		off := 12 + 16*i
		if string(b[off:off+4]) == tag {
			return off
		}
	}
	t.Fatalf("no %q table in the fixture", tag)
	return 0
}

// synthFont builds a font with the given magic and tables, each 4 bytes long.
func synthFont(magic string, tags ...string) []byte {
	dirLen := 12 + 16*len(tags)
	b := make([]byte, dirLen+4*len(tags))
	copy(b, magic)
	binary.BigEndian.PutUint16(b[4:], uint16(len(tags)))
	for i, tag := range tags {
		r := b[12+16*i:]
		copy(r, tag)
		binary.BigEndian.PutUint32(r[8:], uint32(dirLen+4*i))
		binary.BigEndian.PutUint32(r[12:], 4)
	}
	return b
}

func TestCheckFont(t *testing.T) {
	font := readFixtureFont(t)
	if err := CheckFont(font); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	edit := func(f func(b []byte)) []byte {
		b := bytes.Clone(font)
		f(b)
		return b
	}
	magic := func(m string) []byte { return edit(func(b []byte) { copy(b, m) }) }
	numTables := func(n uint16) []byte { return edit(func(b []byte) { binary.BigEndian.PutUint16(b[4:], n) }) }
	glyf := tableRecord(t, font, "glyf")
	refused := []struct {
		name, reason string
		b            []byte
	}{
		{"empty", "empty", nil},
		{"11 bytes", "11 bytes", font[:11]},
		{"over the limit", "over", make([]byte, MaxFontBytes+1)},
		{"collection", "a font collection is not accepted: extract one face", magic("ttcf")},
		{"woff", "WOFF is not accepted", magic("wOFF")},
		{"woff2", "WOFF is not accepted", magic("wOF2")},
		{"svg", "not a TrueType or OpenType file", magic("<svg")},
		{"no tables", "0 tables", numTables(0)},
		{"257 tables", "257 tables", numTables(257)},
		{"past the end", "past the end", edit(func(b []byte) {
			binary.BigEndian.PutUint32(b[glyf+12:], uint32(len(font)))
		})},
		{"overflow", "overflows", edit(func(b []byte) {
			binary.BigEndian.PutUint32(b[glyf+8:], 0xffff_fff0)
			binary.BigEndian.PutUint32(b[glyf+12:], 0x20)
		})},
		{"no cmap", `no "cmap" table`, edit(func(b []byte) { copy(b[tableRecord(t, font, "cmap"):], "cmaq") })},
		{"no glyf", "no outlines", edit(func(b []byte) { copy(b[glyf:], "glyq") })},
		{"cut directory", "table directory", font[:12+16*3+8]},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckFont(tc.b)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error %q does not name %q", err, tc.reason)
			}
		})
	}
	if err := CheckFont(magic("true")); err != nil {
		t.Errorf("magic true: %v", err)
	}
	otto := synthFont("OTTO", "CFF ", "cmap", "head", "hhea", "hmtx", "maxp")
	if err := CheckFont(otto); err != nil {
		t.Errorf("OTTO with CFF: %v", err)
	}
	if err := CheckFont(synthFont("OTTO", "CFF2", "cmap", "head", "hhea", "hmtx", "maxp")); err != nil {
		t.Errorf("OTTO with CFF2: %v", err)
	}
	if err := CheckFont(synthFont("\x00\x01\x00\x00", "glyf", "cmap", "head", "hhea", "hmtx", "maxp")); err == nil {
		t.Error("glyf without loca accepted")
	}
}

func TestFontNames(t *testing.T) {
	stem64 := strings.Repeat("a", 64)
	for name, want := range map[string]bool{
		"a.ttf": true, "a_b-c.otf": true, stem64 + ".ttf": true,
		"A.ttf": false, "a.ttc": false, "a.png": false, ".ttf": false, "a/b.ttf": false,
		"../a.ttf": false, stem64 + "a.ttf": false, "a.woff": false, "a.ttf\x00": false, "a b.ttf": false,
		"_a.ttf": false, "a.TTF": false, "": false,
	} {
		if got := FontName(name); got != want {
			t.Errorf("FontName(%q) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{
		"logo.png": true, "a.jpg": true, "a_b-c.jpeg": true, stem64 + ".png": true,
		"Logo.png": false, "logo.gif": false, "a.ttf": false, ".png": false, "a/b.png": false,
		"../logo.png": false, stem64 + "a.png": false, "": false,
	} {
		if got := ImageName(name); got != want {
			t.Errorf("ImageName(%q) = %v, want %v", name, got, want)
		}
	}
}

// within runs fn and fails the test if it does not return within a second:
// opening a FIFO without O_NONBLOCK would hang until a writer came.
func within(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("did not return within 1 s")
		return nil
	}
}

// specials makes a symbolic link to target and a directory under the given
// names in dir.
func specials(t *testing.T, dir, target, link, sub string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRegular(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRegular(file)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	n, _ := f.Read(b)
	_ = f.Close()
	if string(b[:n]) != "data" {
		t.Fatalf("read %q", b[:n])
	}
	if _, err := OpenRegular(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	specials(t, dir, file, "link", "dir")
	for _, name := range []string{"link", "dir"} {
		err := within(t, func() error {
			f, err := OpenRegular(filepath.Join(dir, name))
			if err == nil {
				_ = f.Close()
			}
			return err
		})
		if err == nil {
			t.Errorf("%s opened", name)
		}
	}
	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(dir, "fifo")
		mkfifo(t, path)
		err := within(t, func() error {
			f, err := OpenRegular(path)
			if err == nil {
				_ = f.Close()
			}
			return err
		})
		if err == nil {
			t.Fatal("fifo opened")
		}
		assertNoReader(t, path)
	})
}

func TestLoadAssetRegularOnly(t *testing.T) {
	dir := t.TempDir()
	png := pngBytes(t, 4, 4)
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadAsset(dir, "logo.png")
	if err != nil || !bytes.Equal(b, png) {
		t.Fatalf("logo.png: %v", err)
	}
	specials(t, dir, filepath.Join(dir, "logo.png"), "link.png", "dir.png")
	for _, name := range []string{"link.png", "dir.png"} {
		if err := refusedWithin(t, func() ([]byte, error) { return LoadAsset(dir, name) }); err == nil {
			t.Errorf("%s loaded", name)
		}
	}
	t.Run("fifo", func(t *testing.T) {
		mkfifo(t, filepath.Join(dir, "fifo.png"))
		if err := refusedWithin(t, func() ([]byte, error) { return LoadAsset(dir, "fifo.png") }); err == nil {
			t.Fatal("fifo loaded")
		}
		assertNoReader(t, filepath.Join(dir, "fifo.png"))
	})
}

// refusedWithin runs a load within a second and returns its error.
func refusedWithin(t *testing.T, load func() ([]byte, error)) error {
	t.Helper()
	return within(t, func() error {
		_, err := load()
		return err
	})
}

func TestLoadFont(t *testing.T) {
	dir := t.TempDir()
	font := readFixtureFont(t)
	if err := os.WriteFile(filepath.Join(dir, "mono.ttf"), font, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadFont(dir, "mono.ttf")
	if err != nil || !bytes.Equal(b, font) {
		t.Fatalf("mono.ttf: %v", err)
	}
	if cap(b) != len(b) {
		t.Errorf("cap %d, len %d: an append could write into the shared bytes", cap(b), len(b))
	}
	// A bad name is refused before anything is opened: a missing dir still
	// gives the name error.
	for _, name := range []string{"../mono.ttf", "Mono.ttf", "mono.ttc", "mono.png", "", "a/b.ttf"} {
		_, err := LoadFont(filepath.Join(dir, "nope"), name)
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("LoadFont(%q) = %v, want the name error", name, err)
		}
	}
	if _, err := LoadFont(dir, "missing.ttf"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.ttf"), append([]byte("wOFF"), font[4:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFont(dir, "bad.ttf"); err == nil || !strings.Contains(err.Error(), "WOFF") {
		t.Errorf("bad.ttf: %v", err)
	}
	specials(t, dir, filepath.Join(dir, "mono.ttf"), "link.ttf", "dir.ttf")
	for _, name := range []string{"link.ttf", "dir.ttf"} {
		if err := refusedWithin(t, func() ([]byte, error) { return LoadFont(dir, name) }); err == nil {
			t.Errorf("%s loaded", name)
		}
	}
	t.Run("fifo", func(t *testing.T) {
		mkfifo(t, filepath.Join(dir, "fifo.ttf"))
		if err := refusedWithin(t, func() ([]byte, error) { return LoadFont(dir, "fifo.ttf") }); err == nil {
			t.Fatal("fifo loaded")
		}
		assertNoReader(t, filepath.Join(dir, "fifo.ttf"))
	})
}

func TestLoadFontTooLarge(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "huge.ttf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFontBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := LoadFont(dir, "huge.ttf"); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("huge.ttf: %v", err)
	}
}

func TestWatermarkValidateFont(t *testing.T) {
	font := readFixtureFont(t)
	if err := (Watermark{Text: "x", Font: font}).Validate(); err != nil {
		t.Errorf("text and font: %v", err)
	}
	err := (Watermark{Image: pngBytes(t, 4, 4), Font: font}).Validate()
	if err == nil || err.Error() != "watermark: a font needs text" {
		t.Errorf("image and font: %v", err)
	}
	bad := append([]byte("ttcf"), font[4:]...)
	err = (Watermark{Text: "x", Font: bad}).Validate()
	if err == nil || !strings.Contains(err.Error(), CheckFont(bad).Error()) {
		t.Errorf("bad font: %v", err)
	}
}
