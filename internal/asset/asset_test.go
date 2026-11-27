package asset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

const fixtureFont = "../../testdata/fixtures/fonts/DejaVuSansMono.ttf"

func readFixtureFont(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(fixtureFont)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// noisy returns a w × h picture of random pixels, seeded so that two seeds
// give different bytes.
func noisy(w, h int, seed uint64) *image.NRGBA {
	r := rand.New(rand.NewPCG(seed, seed))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(r.Uint32())
	}
	return img
}

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gifBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White})
	if err := gif.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// hugePNG is a valid PNG header that claims w × h, with almost no data.
func hugePNG(t *testing.T, w, h uint32) []byte {
	t.Helper()
	b := pngBytes(t, noisy(1, 1, 1))
	// The IHDR chunk follows the 8-byte signature: length, "IHDR", width, height, ...
	binary.BigEndian.PutUint32(b[16:], w)
	binary.BigEndian.PutUint32(b[20:], h)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:])[:16] + `"`
}

func open(t *testing.T, dir string, opt Options) *Store {
	t.Helper()
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *Store, name string, b []byte) (Info, bool) {
	t.Helper()
	inf, created, err := s.Put(context.Background(), name, bytes.NewReader(b), "", false)
	if err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
	return inf, created
}

// snapshot returns every entry of dir with its bytes (or its type when it is
// not a regular file).
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			m[e.Name()] = e.Type().String()
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = string(b)
	}
	return m
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left %s", e.Name())
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

func TestKindOf(t *testing.T) {
	stem64 := strings.Repeat("a", 64)
	for name, want := range map[string]Kind{
		"a.png": Image, "a.jpg": Image, "a.jpeg": Image, "logo_2-b.png": Image, stem64 + ".png": Image,
		"a.ttf": Font, "a.otf": Font,
	} {
		if kind, ok := KindOf(name); kind != want || !ok {
			t.Errorf("KindOf(%q) = %q, %v; want %q", name, kind, ok, want)
		}
	}
	for _, name := range []string{
		"", ".png", "A.png", "a.PNG", "a.gif", "a.ttc", "a.woff", "a", "a.png.tmp", ".a.png.0a1b2c3d.tmp",
		"a/b.png", "../a.png", "a\x00.png", "a b.png", "..", stem64 + "a.png",
	} {
		if kind, ok := KindOf(name); kind != "" || ok {
			t.Errorf("KindOf(%q) = %q, %v; want not ok", name, kind, ok)
		}
	}
	if MaxBytes(Image) != picture.MaxImageBytes || MaxBytes(Font) != picture.MaxFontBytes {
		t.Errorf("MaxBytes: %d, %d", MaxBytes(Image), MaxBytes(Font))
	}
}

func TestPutAndRead(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	pngA := pngBytes(t, noisy(8, 8, 1))
	pngB := pngBytes(t, noisy(8, 8, 2))
	jpg := jpegBytes(t, noisy(8, 8, 3))
	font := readFixtureFont(t)
	rows := []struct {
		name, typ string
		kind      Kind
		b         []byte
	}{
		{"logo.png", "image/png", Image, pngA},
		{"b.jpg", "image/jpeg", Image, jpg},
		{"mono.ttf", "font/ttf", Font, font},
	}
	for _, r := range rows {
		inf, created := put(t, s, r.name, r.b)
		if !created {
			t.Errorf("%s: created false", r.name)
		}
		want := Info{Name: r.name, Kind: r.kind, Type: r.typ, Size: int64(len(r.b)), ETag: etagOf(r.b)}
		got := inf
		got.Modified = time.Time{}
		if got != want {
			t.Errorf("%s: %+v, want %+v", r.name, got, want)
		}
		if inf.Modified.IsZero() {
			t.Errorf("%s: no modification time", r.name)
		}
		b, rinf, err := s.Read(r.name)
		if err != nil || !bytes.Equal(b, r.b) || rinf != inf {
			t.Errorf("%s: read %d bytes, %+v, %v; want %+v", r.name, len(b), rinf, err, inf)
		}
		fi, err := os.Stat(filepath.Join(dir, r.name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", r.name, fi.Mode().Perm())
		}
	}
	inf, created := put(t, s, "logo.png", pngB)
	if created {
		t.Error("replacement: created true")
	}
	if inf.ETag != etagOf(pngB) || inf.ETag == etagOf(pngA) {
		t.Errorf("replacement ETag %s", inf.ETag)
	}
	if b, _, _ := s.Read("logo.png"); !bytes.Equal(b, pngB) {
		t.Error("replacement: old bytes read")
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list))
	for _, i := range list {
		names = append(names, i.Name)
	}
	if !slices.Equal(names, []string{"b.jpg", "logo.png", "mono.ttf"}) {
		t.Errorf("list %v", names)
	}
	for _, i := range list {
		st, err := s.Stat(i.Name)
		if err != nil || st != i {
			t.Errorf("stat %s: %+v, %v; listed %+v", i.Name, st, err, i)
		}
	}
	otf := bytes.Clone(font)
	copy(otf, "OTTO")
	if inf, _ := put(t, s, "o.otf", otf); inf.Type != "font/otf" {
		t.Errorf("OTTO: type %s", inf.Type)
	}
}

// refusals are the Puts TestPutRefusesContent and TestPutLeavesNoTemp make.
func refusals(t *testing.T) []struct {
	label, name string
	b           []byte
	probe       func(context.Context, string) (bool, string, error)
	want        error
	reason      string
} {
	t.Helper()
	pngB := pngBytes(t, noisy(8, 8, 1))
	jpg := jpegBytes(t, noisy(8, 8, 2))
	big := pngBytes(t, noisy(64, 64, 3))
	font := readFixtureFont(t)
	ttc := bytes.Clone(font)
	copy(ttc, "ttcf")
	refuse := func(context.Context, string) (bool, string, error) { return true, "the glyphs are broken", nil }
	broken := func(context.Context, string) (bool, string, error) {
		return false, "", errors.New("exec: no such file")
	}
	return []struct {
		label, name string
		b           []byte
		probe       func(context.Context, string) (bool, string, error)
		want        error
		reason      string
	}{
		{"png as jpg", "a.jpg", pngB, nil, ErrContent, "image/png"},
		{"jpeg as png", "a.png", jpg, nil, ErrContent, "image/jpeg"},
		{"svg", "a.png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), nil, ErrContent, "image"},
		{"html", "a.png", []byte("<html><body>x</body></html>"), nil, ErrContent, "image"},
		{"cut png", "a.png", big[:len(big)/2], nil, ErrContent, "decode"},
		{"wide png", "a.png", pngBytes(t, noisy(2049, 10, 4)), nil, ErrContent, "2049x10"},
		{"huge header", "a.png", hugePNG(t, 20000, 20000), nil, ErrContent, "20000x20000"},
		{"gif", "a.png", gifBytes(t), nil, ErrContent, "gif"},
		{"text as ttf", "a.ttf", []byte("hello, this is not a font file\n"), nil, ErrContent, "font"},
		{"ttcf", "a.ttf", ttc, nil, ErrContent, "collection"},
		{"probe refuses", "a.ttf", font, refuse, ErrContent, "the glyphs are broken"},
		{"probe errs", "a.ttf", font, broken, ErrProbe, "no such file"},
	}
}

func TestPutRefusesContent(t *testing.T) {
	for _, r := range refusals(t) {
		t.Run(r.label, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir, Options{ProbeFont: r.probe})
			put(t, s, "keep.png", pngBytes(t, noisy(4, 4, 9)))
			before := snapshot(t, dir)
			_, _, err := s.Put(context.Background(), r.name, bytes.NewReader(r.b), "", false)
			if !errors.Is(err, r.want) || !strings.Contains(err.Error(), r.reason) {
				t.Fatalf("err %v, want %v with %q", err, r.want, r.reason)
			}
			if r.want == ErrProbe && errors.Is(err, ErrContent) {
				t.Errorf("a probe that could not run judged the font: %v", err)
			}
			if after := snapshot(t, dir); !maps.Equal(before, after) {
				t.Errorf("directory changed: %v", after)
			}
		})
	}
}

// countingReader yields an endless run of b's bytes and counts them.
type countingReader struct {
	b []byte
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		k := copy(p[n:], c.b[(c.n+int64(n))%int64(len(c.b)):])
		n += k
	}
	c.n += int64(n)
	return n, nil
}

func TestPutTooLarge(t *testing.T) {
	dir := t.TempDir()
	probed := false
	s := open(t, dir, Options{ProbeFont: func(context.Context, string) (bool, string, error) {
		probed = true
		return false, "", nil
	}})
	for _, r := range []struct {
		name  string
		limit int64
		b     []byte
	}{
		{"a.png", picture.MaxImageBytes, pngBytes(t, noisy(4, 4, 1))},
		{"a.ttf", picture.MaxFontBytes, readFixtureFont(t)},
	} {
		c := &countingReader{b: r.b}
		_, _, err := s.Put(context.Background(), r.name, c, "", false)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: %v", r.name, err)
		}
		if c.n > r.limit+1 {
			t.Errorf("%s: read %d bytes, limit %d", r.name, c.n, r.limit)
		}
	}
	if probed {
		t.Error("a font over the limit was probed")
	}
	assertNoTemp(t, dir)
	if names := slices.Collect(maps.Keys(snapshot(t, dir))); len(names) != 0 {
		t.Errorf("left %v", names)
	}
}

func TestPutLeavesNoTemp(t *testing.T) {
	for _, r := range refusals(t) {
		t.Run(r.label, func(t *testing.T) {
			dir := t.TempDir()
			sawTemp := false
			probe := r.probe
			if probe != nil {
				probe = func(ctx context.Context, path string) (bool, string, error) {
					if _, err := os.Stat(path); err == nil && strings.HasPrefix(filepath.Base(path), ".") {
						sawTemp = true
					}
					return r.probe(ctx, path)
				}
			}
			s := open(t, dir, Options{ProbeFont: probe})
			_, _, _ = s.Put(context.Background(), r.name, bytes.NewReader(r.b), "", false)
			if r.probe != nil && !sawTemp {
				t.Error("the probe was not given the temp file")
			}
			assertNoTemp(t, dir)
		})
	}
	t.Run("after the lock", func(t *testing.T) {
		dir := t.TempDir()
		s := open(t, dir, Options{})
		img := pngBytes(t, noisy(4, 4, 1))
		put(t, s, "a.png", img)
		if _, _, err := s.Put(context.Background(), "a.png", bytes.NewReader(img), "", true); !errors.Is(err, ErrPrecondition) {
			t.Fatalf("condition: %v", err)
		}
		s.maxFiles = 1
		if _, _, err := s.Put(context.Background(), "b.png", bytes.NewReader(img), "", false); !errors.Is(err, ErrFull) {
			t.Fatalf("quota: %v", err)
		}
		s.maxFiles = MaxFiles
		if err := os.Mkdir(filepath.Join(dir, "x.png"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Put(context.Background(), "x.png", bytes.NewReader(img), "", false); !errors.Is(err, ErrWrite) {
			t.Fatalf("rename over a directory: %v", err)
		}
		assertNoTemp(t, dir)
	})
}

func TestPutConditions(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	a, b := pngBytes(t, noisy(4, 4, 1)), pngBytes(t, noisy(4, 4, 2))
	ctx := context.Background()
	if _, _, err := s.Put(ctx, "m.png", bytes.NewReader(a), "*", false); !errors.Is(err, ErrPrecondition) {
		t.Errorf("match * on a missing name: %v", err)
	}
	if _, _, err := s.Put(ctx, "m.png", bytes.NewReader(a), etagOf(a), false); !errors.Is(err, ErrPrecondition) {
		t.Errorf("match a tag on a missing name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.png")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("written: %v", err)
	}
	inf, created, err := s.Put(ctx, "m.png", bytes.NewReader(a), "", true)
	if err != nil || !created {
		t.Fatalf("none-match on a new name: %v, %v", created, err)
	}
	if _, _, err := s.Put(ctx, "m.png", bytes.NewReader(b), "", true); !errors.Is(err, ErrPrecondition) {
		t.Errorf("none-match on an existing name: %v", err)
	}
	if _, _, err := s.Put(ctx, "m.png", bytes.NewReader(b), etagOf(b), false); !errors.Is(err, ErrPrecondition) {
		t.Errorf("stale tag: %v", err)
	}
	if got, _, _ := s.Read("m.png"); !bytes.Equal(got, a) {
		t.Fatal("a failed condition wrote")
	}
	inf2, created, err := s.Put(ctx, "m.png", bytes.NewReader(b), inf.ETag, false)
	if err != nil || created || inf2.ETag != etagOf(b) {
		t.Fatalf("current tag: %+v, %v, %v", inf2, created, err)
	}
	if _, _, err := s.Put(ctx, "m.png", bytes.NewReader(a), "*", false); err != nil {
		t.Errorf("match * on an existing name: %v", err)
	}
	assertNoTemp(t, dir)
}

func TestQuota(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	ctx := context.Background()
	small := pngBytes(t, noisy(4, 4, 1))
	bigger := pngBytes(t, noisy(16, 16, 2))
	s.maxFiles = 2
	put(t, s, "a.png", small)
	put(t, s, "b.png", small)
	if _, _, err := s.Put(ctx, "c.png", bytes.NewReader(small), "", false); !errors.Is(err, ErrFull) {
		t.Errorf("one file too many: %v", err)
	}
	if _, created := put(t, s, "b.png", bigger); created {
		t.Error("replacement at the file limit: created")
	}
	s.maxFiles = MaxFiles
	s.maxTotal = int64(len(small) + len(bigger))
	if _, _, err := s.Put(ctx, "c.png", bytes.NewReader(small), "", false); !errors.Is(err, ErrFull) {
		t.Errorf("bytes over the total: %v", err)
	}
	if _, _, err := s.Put(ctx, "a.png", bytes.NewReader(bigger), "", false); !errors.Is(err, ErrFull) {
		t.Errorf("a larger replacement over the total: %v", err)
	}
	put(t, s, "b.png", small)
	put(t, s, "c.png", small)
	if _, err := os.Stat(filepath.Join(dir, "c.png")); err != nil {
		t.Fatal(err)
	}
	tot := s.Totals()
	if got := tot[Image]; got != [2]int64{3, int64(3 * len(small))} {
		t.Errorf("totals %v", tot)
	}
	if got := tot[Font]; got != [2]int64{0, 0} {
		t.Errorf("font totals %v", got)
	}
	assertNoTemp(t, dir)
}

func TestDelete(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "assets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "x.png")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir, Options{})
	img := pngBytes(t, noisy(4, 4, 1))
	inf, _ := put(t, s, "a.png", img)
	if err := s.Delete("a.png", etagOf([]byte("other"))); !errors.Is(err, ErrPrecondition) {
		t.Errorf("stale match: %v", err)
	}
	if err := s.Delete("a.png", inf.ETag); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.png")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("still there: %v", err)
	}
	if err := s.Delete("a.png", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, _, err := s.Read("a.png"); !errors.Is(err, ErrNotFound) {
		t.Errorf("read after delete: %v", err)
	}
	for _, name := range []string{"../x.png", "", "a.gif"} {
		if err := s.Delete(name, ""); !errors.Is(err, ErrName) {
			t.Errorf("Delete(%q): %v", name, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the file outside: %v", err)
	}
	put(t, s, "b.png", img)
	if err := s.Delete("b.png", "*"); err != nil {
		t.Errorf("match *: %v", err)
	}
	if err := s.Delete("b.png", "*"); !errors.Is(err, ErrNotFound) {
		t.Errorf("match * on a missing name: %v", err)
	}
}

func TestOpenSweepsTemp(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".a.png.0a1b2c3d.tmp")
	write(".mono.ttf.ffffffff.tmp")
	write(".A.png.0a1b2c3d.tmp") // not an asset's temp file
	write("notes.txt")
	write(".hidden")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".b.png.0a1b2c3d.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir, Options{})
	got := slices.Sorted(maps.Keys(snapshot(t, dir)))
	want := []string{".A.png.0a1b2c3d.tmp", ".b.png.0a1b2c3d.tmp", ".hidden", "notes.txt", "sub"}
	if !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
	list, err := s.List()
	if err != nil || len(list) != 0 {
		t.Errorf("list %v, %v", list, err)
	}
}

func TestHandCopiedFiles(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	img := pngBytes(t, noisy(4, 4, 1))
	if err := os.WriteFile(filepath.Join(dir, "hand.png"), img, 0o644); err != nil {
		t.Fatal(err)
	}
	b, inf, err := s.Read("hand.png")
	if err != nil || !bytes.Equal(b, img) || inf.ETag != etagOf(img) || inf.Type != "image/png" {
		t.Fatalf("read: %+v, %v", inf, err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0] != inf {
		t.Fatalf("list %+v, %v", list, err)
	}
	if err := s.Delete("hand.png", inf.ETag); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "x.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	refused := func(t *testing.T, name string) {
		t.Helper()
		if err := within(t, func() error { _, _, err := s.Read(name); return err }); !errors.Is(err, ErrNotFound) {
			t.Errorf("read %s: %v", name, err)
		}
		if err := within(t, func() error {
			f, _, err := s.Open(name)
			if err == nil {
				_ = f.Close()
			}
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Errorf("open %s: %v", name, err)
		}
		if err := within(t, func() error { _, err := s.Stat(name); return err }); !errors.Is(err, ErrNotFound) {
			t.Errorf("stat %s: %v", name, err)
		}
		if err := within(t, func() error { return s.Delete(name, "") }); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete %s: %v", name, err)
		}
		var list []Info
		if err := within(t, func() error { var err error; list, err = s.List(); return err }); err != nil || len(list) != 0 {
			t.Errorf("list %+v, %v", list, err)
		}
	}
	refused(t, "x.png")
	t.Run("over the limit", func(t *testing.T) {
		path := filepath.Join(dir, "big.png")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(img); err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(picture.MaxImageBytes + 1); err != nil { // sparse
			t.Fatal(err)
		}
		_ = f.Close()
		s.hashes.Store(0)
		if list, err := s.List(); err != nil || len(list) != 0 {
			t.Errorf("listed: %+v, %v", list, err)
		}
		if _, err := s.Stat("big.png"); !errors.Is(err, ErrTooLarge) {
			t.Errorf("stat: %v", err)
		}
		if f, _, err := s.Open("big.png"); !errors.Is(err, ErrTooLarge) {
			if err == nil {
				_ = f.Close()
			}
			t.Errorf("open: %v", err)
		}
		if _, _, err := s.Read("big.png"); !errors.Is(err, ErrTooLarge) {
			t.Errorf("read: %v", err)
		}
		if _, _, err := s.Put(context.Background(), "big.png", bytes.NewReader(img), etagOf(img), false); !errors.Is(err, ErrPrecondition) {
			t.Errorf("a Put with a tag: %v", err)
		}
		if err := s.Delete("big.png", etagOf(img)); !errors.Is(err, ErrPrecondition) {
			t.Errorf("a Delete with a tag: %v", err)
		}
		if n := s.hashes.Load(); n != 0 {
			t.Errorf("hashed %d times", n)
		}
		if got := s.Totals()[Image]; got != [2]int64{1, picture.MaxImageBytes + 1} {
			t.Errorf("not counted toward the quota: %v", got)
		}
		if err := s.Delete("big.png", ""); err != nil {
			t.Errorf("unconditional delete: %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("still there: %v", err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(dir, "y.png")
		mkfifo(t, path)
		refused(t, "y.png")
		assertNoReader(t, path)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real.png")
		if err := os.WriteFile(target, img, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "z.png")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("no symbolic links: %v", err)
		}
		refused(t, "z.png")
		other := pngBytes(t, noisy(4, 4, 2))
		if _, created := put(t, s, "z.png", other); !created {
			t.Error("a Put over a symlink: created false")
		}
		if b, _ := os.ReadFile(target); !bytes.Equal(b, img) {
			t.Error("the link's target was written")
		}
		fi, err := os.Lstat(link)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("the link was not replaced: %v, %v", fi, err)
		}
	})
}

func TestOpenKeepsOldBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file open on Windows cannot be renamed over")
	}
	dir := t.TempDir()
	s := open(t, dir, Options{})
	old := pngBytes(t, noisy(64, 64, 1))
	put(t, s, "a.png", old)
	f, inf, err := s.Open("a.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if inf.ETag != etagOf(old) {
		t.Errorf("ETag %s", inf.ETag)
	}
	put(t, s, "a.png", pngBytes(t, noisy(8, 8, 2)))
	b, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(b, old) {
		t.Fatalf("read %d bytes of %d, %v", len(b), len(old), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if inf.ETag != etagOf(old) || inf.Size != int64(len(old)) {
		t.Errorf("info changed: %+v", inf)
	}
}

func TestHashCache(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	a := pngBytes(t, noisy(4, 4, 1))
	inf, _ := put(t, s, "a.png", a)
	st, err := s.Stat("a.png")
	if err != nil || st.ETag != etagOf(a) {
		t.Fatalf("stat %+v, %v", st, err)
	}
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	if n := s.hashes.Load(); n != 0 {
		t.Errorf("after Put, Stat and List hashed %d times", n)
	}
	path := filepath.Join(dir, "a.png")
	b := pngBytes(t, noisy(5, 5, 2))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stat("a.png"); st.ETag != etagOf(b) {
		t.Errorf("a hand edit of another size: %s", st.ETag)
	}
	if n := s.hashes.Load(); n != 1 {
		t.Errorf("hashed %d times, want 1", n)
	}
	if st, _ := s.Stat("a.png"); st.ETag != etagOf(b) || s.hashes.Load() != 1 {
		t.Errorf("not cached: %s, %d", st.ETag, s.hashes.Load())
	}

	// Delete drops the entry: a hand copy of other bytes of the same size and
	// modification time is hashed afresh.
	inf, _ = put(t, s, "a.png", a)
	if err := s.Delete("a.png", ""); err != nil {
		t.Fatal(err)
	}
	same := bytes.Clone(a)
	same[len(same)-1] ^= 0xff
	if err := os.WriteFile(path, same, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, inf.Modified, inf.Modified); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Stat("a.png"); err != nil || st.ETag != etagOf(same) || st.Modified != inf.Modified {
		t.Errorf("after Delete and a hand copy: %+v, %v (old %s)", st, err, inf.ETag)
	}
}

func TestOff(t *testing.T) {
	s := open(t, "", Options{})
	ctx := context.Background()
	if _, err := s.List(); !errors.Is(err, ErrOff) {
		t.Errorf("List: %v", err)
	}
	if _, err := s.Stat("a.png"); !errors.Is(err, ErrOff) {
		t.Errorf("Stat: %v", err)
	}
	if _, _, err := s.Read("a.png"); !errors.Is(err, ErrOff) {
		t.Errorf("Read: %v", err)
	}
	if _, _, err := s.Open("a.png"); !errors.Is(err, ErrOff) {
		t.Errorf("Open: %v", err)
	}
	if _, _, err := s.Put(ctx, "a.png", bytes.NewReader(nil), "", false); !errors.Is(err, ErrOff) {
		t.Errorf("Put: %v", err)
	}
	if err := s.Delete("a.png", ""); !errors.Is(err, ErrOff) {
		t.Errorf("Delete: %v", err)
	}
	if tot := s.Totals(); tot != nil {
		t.Errorf("Totals: %v", tot)
	}
}

func TestOpenNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, Options{}); err == nil {
		t.Error("a file opened as the directory")
	}
	if _, err := Open(filepath.Join(file, "missing"), Options{}); err == nil {
		t.Error("a missing directory opened")
	}
}

func TestConcurrentPuts(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	imgs := [][]byte{pngBytes(t, noisy(32, 32, 1)), pngBytes(t, noisy(40, 40, 2))}
	put(t, s, "a.png", imgs[0])
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 64)
	for i := range 16 {
		wg.Go(func() {
			for j := range 8 {
				b := imgs[(i+j)%2]
				if _, _, err := s.Put(context.Background(), "a.png", bytes.NewReader(b), "", false); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, inf, err := s.Read("a.png")
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(b, imgs[0]) && !bytes.Equal(b, imgs[1]) {
					errs <- fmt.Errorf("read %d bytes that are neither image", len(b))
					return
				}
				if inf.ETag != etagOf(b) {
					errs <- fmt.Errorf("ETag %s for the bytes %s", inf.ETag, etagOf(b))
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertNoTemp(t, dir)
}

// failingReader yields n bytes of b, then err.
type failingReader struct {
	b   []byte
	n   int
	err error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, r.err
	}
	k := copy(p[:min(len(p), r.n)], r.b)
	r.b, r.n = r.b[k:], r.n-k
	return k, nil
}

func TestPutBodyError(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	img := pngBytes(t, noisy(16, 16, 1))
	cut := errors.New("connection reset")
	_, _, err := s.Put(context.Background(), "a.png", &failingReader{b: img, n: len(img) / 2, err: cut}, "", false)
	if !errors.Is(err, ErrBody) || !errors.Is(err, cut) {
		t.Errorf("a failing body: %v", err)
	}
	r := http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(img)), 10)
	_, _, err = s.Put(context.Background(), "a.png", r, "", false)
	var mb *http.MaxBytesError
	if !errors.Is(err, ErrBody) || !errors.As(err, &mb) {
		t.Errorf("a body over http.MaxBytesReader's limit: %v", err)
	}
	if len(snapshot(t, dir)) != 0 {
		t.Error("the directory changed")
	}
}

func TestReason(t *testing.T) {
	dir := t.TempDir()
	refuse := func(context.Context, string) (bool, string, error) { return true, "the glyphs are broken", nil }
	broken := func(context.Context, string) (bool, string, error) { return false, "", errors.New("exit status 2") }
	ctx := context.Background()
	_, _, err := open(t, dir, Options{}).Put(ctx, "a.ttf", strings.NewReader("not a font at all"), "", false)
	if r := Reason(err); r == "" || strings.Contains(r, "asset:") || !strings.HasPrefix(r, "font: ") {
		t.Errorf("a bad font: %q from %v", r, err)
	}
	_, _, err = open(t, dir, Options{}).Put(ctx, "a.jpg", bytes.NewReader(pngBytes(t, noisy(4, 4, 1))), "", false)
	if r := Reason(err); r != "image: the file is image/png, the name says image/jpeg" {
		t.Errorf("a PNG as a.jpg: %q", r)
	}
	font := readFixtureFont(t)
	_, _, err = open(t, dir, Options{ProbeFont: refuse}).Put(ctx, "a.ttf", bytes.NewReader(font), "", false)
	if r := Reason(err); r != "the glyphs are broken" || !errors.Is(err, ErrContent) {
		t.Errorf("a refused font: %q from %v", r, err)
	}
	_, _, err = open(t, dir, Options{ProbeFont: broken}).Put(ctx, "a.ttf", bytes.NewReader(font), "", false)
	if r := Reason(err); r != "" {
		t.Errorf("a probe that did not run: %q", r)
	}
	if Reason(nil) != "" || Reason(ErrContent) != "" || Reason(fmt.Errorf("x: %w", ErrFull)) != "" {
		t.Error("a reason outside an ErrContent error")
	}
	wrapped := fmt.Errorf("api: %w", &contentError{reason: "r"})
	if Reason(wrapped) != "r" || !errors.Is(wrapped, ErrContent) {
		t.Errorf("wrapped: %q", Reason(wrapped))
	}
}

// TestCacheSameFile: another file renamed over the name with the size and the
// modification time of the cached one is hashed afresh.
func TestCacheSameFile(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	a := pngBytes(t, noisy(4, 4, 1))
	inf, _ := put(t, s, "a.png", a)
	b := bytes.Clone(a)
	b[len(b)-1] ^= 0xff
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, inf.Modified, inf.Modified); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, filepath.Join(dir, "a.png")); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stat("a.png")
	if err != nil || st.ETag != etagOf(b) || st.Size != inf.Size || st.Modified != inf.Modified {
		t.Errorf("stat %+v, %v; want the ETag %s, not %s", st, err, etagOf(b), inf.ETag)
	}
}

// TestListPruneEpoch: a listing does not prune an entry that a Put committed
// after its ReadDir.
func TestListPruneEpoch(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	put(t, s, "a.png", pngBytes(t, noisy(4, 4, 1)))
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	}
	// Stand in for a Put committing between List's ReadDir and its prune:
	// an entry for a name the directory does not hold, then a commit.
	fi, err := os.Stat(filepath.Join(dir, "a.png"))
	if err != nil {
		t.Fatal(err)
	}
	s.listHook = func() { s.commitEntry("b.png", &entry{fi: fi, typ: "image/png", etag: `"b"`}) }
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	s.cmu.Lock()
	_, ok := s.cache["b.png"]
	s.cmu.Unlock()
	if !ok {
		t.Error("the entry of a Put after the ReadDir was pruned")
	}
	s.listHook = nil
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	s.cmu.Lock()
	_, ok = s.cache["b.png"]
	s.cmu.Unlock()
	if ok {
		t.Error("a quiet listing kept the entry of a name that is gone")
	}
}

func TestPutWriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop writes here")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	s := open(t, dir, Options{})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, _, err := s.Put(context.Background(), "a.png", bytes.NewReader(pngBytes(t, noisy(4, 4, 1))), "", false)
	if !errors.Is(err, ErrWrite) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err %v", err)
	}
}

var errCrash = errors.New("crash")

// TestPutCrashPoints stops a replacement at each step, as a crash would,
// and opens the directory again: the name holds the old bytes or the new
// ones whole, and no temp file is left.
func TestPutCrashPoints(t *testing.T) {
	for _, step := range []string{"created", "written", "synced", "probed", "renamed"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir, Options{ProbeFont: func(context.Context, string) (bool, string, error) { return false, "", nil }})
			old := readFixtureFont(t)
			put(t, s, "f.ttf", old)
			nu := bytes.Clone(old)
			nu[len(nu)-1] ^= 0xff
			s.hook = func(st string) error {
				if st == step {
					return errCrash
				}
				return nil
			}
			if _, _, err := s.Put(context.Background(), "f.ttf", bytes.NewReader(nu), "", false); !errors.Is(err, errCrash) {
				t.Fatalf("err %v", err)
			}
			s2 := open(t, dir, Options{})
			b, _, err := s2.Read("f.ttf")
			if err != nil {
				t.Fatal(err)
			}
			want := old
			if step == "renamed" {
				want = nu
			}
			if !bytes.Equal(b, want) {
				t.Errorf("the name holds neither whole file as expected")
			}
			assertNoTemp(t, dir)
			// The lock was given back.
			s.hook = nil
			put(t, s, "f.ttf", old)
		})
	}
}

// TestCacheEpoch: a reader that hashed a file before a Put or a Delete
// committed does not cache what it hashed, which may be the replaced bytes
// under the same size and modification time.
func TestCacheEpoch(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	img := pngBytes(t, noisy(4, 4, 1))
	put(t, s, "a.png", img)
	fi, err := os.Stat(filepath.Join(dir, "a.png"))
	if err != nil {
		t.Fatal(err)
	}
	_, epoch, ok := s.lookup("a.png", fi)
	if !ok {
		t.Fatal("Put left no entry")
	}
	s.commitEntry("a.png", &entry{fi: fi, typ: "image/png", etag: `"new"`})
	s.remember("a.png", epoch, entry{fi: fi, typ: "image/png", etag: `"stale"`})
	if st, _ := s.Stat("a.png"); st.ETag != `"new"` {
		t.Errorf("a reader from before the commit cached %s", st.ETag)
	}
	s.commitEntry("a.png", nil)
	s.remember("a.png", epoch, entry{fi: fi, typ: "image/png", etag: `"stale"`})
	if st, _ := s.Stat("a.png"); st.ETag != etagOf(img) {
		t.Errorf("after a Delete: %s", st.ETag)
	}
}
