package watermarks

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/config"
)

// pngBytes is a minimal valid 1×1 PNG so http.DetectContentType returns
// "image/png". Used by every Save() test; sniff sees only the first 512
// bytes so this is enough to clear the validator.
var pngBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	return mustNewServiceForTest(t.TempDir())
}

// mustNewServiceForTest builds a Service rooted at dir without going
// through the do.Injector, so unit tests don't need DI plumbing.
func mustNewServiceForTest(dir string) *Service {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	s, err := newServiceForTesting(config.WatermarksConfig{Dir: dir})
	if err != nil {
		panic(err)
	}
	return s
}

func TestServiceSaveListGet(t *testing.T) {
	s := newTestService(t)

	a, err := s.Save("My Logo", "logo.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if a.ID == "" {
		t.Fatal("empty ID returned")
	}
	if a.Name != "My Logo" {
		t.Errorf("Name = %q, want My Logo", a.Name)
	}
	if a.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", a.ContentType)
	}
	if a.SizeBytes != int64(len(pngBytes)) {
		t.Errorf("SizeBytes = %d, want %d", a.SizeBytes, len(pngBytes))
	}

	// Round-trip via Get.
	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("Get returned wrong ID")
	}

	// List should contain it.
	listed := s.List()
	if len(listed) != 1 || listed[0].ID != a.ID {
		t.Errorf("List = %+v, want one matching asset", listed)
	}
}

func TestServiceResolvePath(t *testing.T) {
	s := newTestService(t)
	a, err := s.Save("", "logo.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.ResolvePath(a.ID)
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("expected .png suffix, got %q", path)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not on disk: %v", err)
	}
	if st.Size() != int64(len(pngBytes)) {
		t.Errorf("size mismatch: %d vs %d", st.Size(), len(pngBytes))
	}
}

func TestServiceRejectsNonImage(t *testing.T) {
	s := newTestService(t)
	_, err := s.Save("", "evil.txt", bytes.NewReader([]byte("just plain text")))
	if !errors.Is(err, ErrInvalidContent) {
		t.Errorf("want ErrInvalidContent, got %v", err)
	}
}

func TestServiceDeleteRemovesBoth(t *testing.T) {
	s := newTestService(t)
	a, err := s.Save("", "logo.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: want ErrNotFound, got %v", err)
	}

	// Repeat delete is also ErrNotFound, never an error.
	if err := s.Delete(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("repeat Delete: want ErrNotFound, got %v", err)
	}
	// Both files should be gone from disk.
	files, _ := os.ReadDir(s.Dir())
	for _, f := range files {
		if strings.Contains(f.Name(), string(a.ID)) {
			t.Errorf("file %q still on disk", f.Name())
		}
	}
}

func TestServiceRebuildCacheFromDisk(t *testing.T) {
	dir := t.TempDir()
	// First instance uploads an asset.
	{
		s := mustNewServiceForTest(dir)
		if _, err := s.Save("first", "a.png", bytes.NewReader(pngBytes)); err != nil {
			t.Fatal(err)
		}
	}
	// Fresh instance pointed at the same dir picks up the sidecar.
	s2 := mustNewServiceForTest(dir)
	if got := s2.List(); len(got) != 1 || got[0].Name != "first" {
		t.Errorf("rebuild missed sidecar: %+v", got)
	}
}

func TestExtensionForFallbacks(t *testing.T) {
	cases := map[string]struct {
		filename, contentType string
		want                  string
	}{
		"png by ext":   {"foo.PNG", "image/png", ".png"},
		"jpeg by ext":  {"foo.jpg", "image/jpeg", ".jpg"},
		"gif by ct":    {"weird", "image/gif; charset=utf-8", ".gif"},
		"unknown fall": {"x", "application/octet-stream", ".bin"},
	}
	for name, c := range cases {
		if got := extensionFor(c.filename, c.contentType); got != c.want {
			t.Errorf("%s: extensionFor(%q,%q)=%q, want %q", name, c.filename, c.contentType, got, c.want)
		}
	}
}
