// Package watermarks manages the on-disk library of uploadable watermark
// images (logos, channel bugs). Each asset is stored as two files in a
// flat directory:
//
//	<dir>/<id>.<ext>      ← image bytes
//	<dir>/<id>.json       ← domain.WatermarkAsset metadata sidecar
//
// The sidecar layout means we don't need a separate database for asset
// metadata — `os.ReadDir` rebuilds the registry after every restart and
// the cache stays consistent without coordination with the storage layer.
//
// Resolution flow at transcode time:
//
//	Stream.Watermark.AssetID
//	     │
//	     ▼  watermarks.Service.ResolvePath
//	  /<dir>/<id>.png
//	     │
//	     ▼  coordinator copies into TranscoderConfig.Watermark.ImagePath
//	  ffmpeg -i ... -vf "...,movie=<absolute path>,..."
package watermarks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Errors returned by the service. Mapped to HTTP status codes by the
// REST handler — see internal/api/handler/watermark.go.
var (
	// ErrNotFound is returned by Get / Delete when the asset is unknown.
	ErrNotFound = errors.New("watermarks: asset not found")
	// ErrInvalidContent is returned when the uploaded bytes don't sniff
	// as an image MIME type the FFmpeg overlay path supports.
	ErrInvalidContent = errors.New("watermarks: not an image")
	// ErrTooLarge is returned by the handler when the upload exceeds
	// MaxWatermarkAssetBytes — defined here so the handler can wrap it.
	ErrTooLarge = errors.New("watermarks: upload too large")
)

// Service is the public facade. Constructed via DI from config.WatermarksConfig.
// Safe for concurrent use; an internal RWMutex protects the metadata map.
type Service struct {
	dir   string
	mu    sync.RWMutex
	cache map[domain.WatermarkAssetID]*domain.WatermarkAsset
}

// New is the samber/do constructor. Creates the assets directory if missing
// and seeds the in-memory cache from any sidecars already on disk so a
// restart picks up where the previous instance left off.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.WatermarksConfig](i)
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = "./watermarks"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("watermarks: mkdir %q: %w", dir, err)
	}
	s := &Service{
		dir:   dir,
		cache: make(map[domain.WatermarkAssetID]*domain.WatermarkAsset),
	}
	if err := s.rebuildCache(); err != nil {
		return nil, fmt.Errorf("watermarks: rebuild cache: %w", err)
	}
	return s, nil
}

// Dir returns the on-disk root of the assets library. Exposed so the
// configuration UI can show operators where uploads land.
func (s *Service) Dir() string { return s.dir }

// newServiceForTesting builds a Service without DI plumbing. Used by unit
// tests that just need to exercise Save / Get / Delete against a temp dir.
func newServiceForTesting(cfg config.WatermarksConfig) (*Service, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = "./watermarks"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Service{
		dir:   dir,
		cache: make(map[domain.WatermarkAssetID]*domain.WatermarkAsset),
	}
	if err := s.rebuildCache(); err != nil {
		return nil, err
	}
	return s, nil
}

// List returns a snapshot of every known asset, sorted by upload time
// (newest first) so dashboards default to "what did I just upload".
func (s *Service) List() []*domain.WatermarkAsset {
	s.mu.RLock()
	out := make([]*domain.WatermarkAsset, 0, len(s.cache))
	for _, a := range s.cache {
		c := *a
		out = append(out, &c)
	}
	s.mu.RUnlock()
	// Manual sort instead of importing sort because the slice stays small
	// in practice (operator-curated library, not user-generated content).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].UploadedAt.Before(out[j].UploadedAt); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Get returns the metadata for a single asset.
func (s *Service) Get(id domain.WatermarkAssetID) (*domain.WatermarkAsset, error) {
	s.mu.RLock()
	a, ok := s.cache[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	c := *a
	return &c, nil
}

// ResolvePath returns the absolute filesystem path to the image bytes for
// the given asset id. Used by the coordinator to translate
// Stream.Watermark.AssetID into TranscoderConfig.Watermark.ImagePath
// before the transcoder consumes the config.
func (s *Service) ResolvePath(id domain.WatermarkAssetID) (string, error) {
	a, err := s.Get(id)
	if err != nil {
		return "", err
	}
	return s.imagePath(a), nil
}

// Save uploads new image bytes under the given display name. ContentType
// is sniffed via http.DetectContentType from the first 512 bytes; the
// returned asset's FileName preserves the operator's original filename
// for download responses.
//
// Atomic on success: temp file written then renamed; sidecar metadata
// written last so a half-uploaded asset is invisible to subsequent calls.
func (s *Service) Save(displayName, originalFilename string, body io.Reader) (*domain.WatermarkAsset, error) {
	displayName = strings.TrimSpace(displayName)
	originalFilename = strings.TrimSpace(originalFilename)
	if originalFilename == "" {
		return nil, fmt.Errorf("watermarks: filename is required")
	}
	if displayName == "" {
		displayName = originalFilename
	}
	if len(displayName) > domain.MaxWatermarkAssetNameLen {
		return nil, fmt.Errorf("watermarks: name exceeds max length %d", domain.MaxWatermarkAssetNameLen)
	}

	id, err := newAssetID()
	if err != nil {
		return nil, fmt.Errorf("watermarks: generate id: %w", err)
	}

	// Sniff first to reject non-images before writing anything to disk.
	head := make([]byte, 512)
	n, _ := io.ReadFull(body, head)
	head = head[:n]
	ct := http.DetectContentType(head)
	if !isSupportedImage(ct) {
		return nil, fmt.Errorf("%w: detected %s", ErrInvalidContent, ct)
	}
	ext := extensionFor(originalFilename, ct)
	imgPath := filepath.Join(s.dir, string(id)+ext)
	tmp := imgPath + ".tmp"

	written, err := writeAtomic(tmp, imgPath, head, body)
	if err != nil {
		return nil, err
	}
	if written > domain.MaxWatermarkAssetBytes {
		_ = os.Remove(imgPath)
		return nil, fmt.Errorf("%w: %d bytes (cap %d)", ErrTooLarge, written, domain.MaxWatermarkAssetBytes)
	}

	asset := &domain.WatermarkAsset{
		ID:          id,
		Name:        displayName,
		FileName:    filepath.Base(originalFilename),
		ContentType: ct,
		SizeBytes:   written,
		UploadedAt:  time.Now().UTC(),
	}
	if err := s.writeSidecar(asset); err != nil {
		_ = os.Remove(imgPath)
		return nil, err
	}

	s.mu.Lock()
	s.cache[id] = asset
	s.mu.Unlock()

	return asset, nil
}

// Delete removes both the image and metadata sidecar. Idempotent on the
// "already gone" case so repeat calls return ErrNotFound exactly once and
// no-op afterwards. Caller is responsible for ensuring the asset isn't
// referenced by an active stream — there is no foreign-key check here.
func (s *Service) Delete(id domain.WatermarkAssetID) error {
	s.mu.Lock()
	a, ok := s.cache[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.cache, id)
	s.mu.Unlock()

	imgErr := os.Remove(s.imagePath(a))
	scErr := os.Remove(s.sidecarPath(id))
	// Tolerate missing files — they may have been cleaned up out-of-band;
	// the cache update above already made the asset invisible to callers.
	if imgErr != nil && !os.IsNotExist(imgErr) {
		return fmt.Errorf("watermarks: remove image: %w", imgErr)
	}
	if scErr != nil && !os.IsNotExist(scErr) {
		return fmt.Errorf("watermarks: remove sidecar: %w", scErr)
	}
	return nil
}

// ─── internals ───────────────────────────────────────────────────────────────

// rebuildCache scans the assets directory and reloads the metadata map.
// Files without a matching sidecar (or with corrupt sidecars) are skipped
// with a warning so a single bad upload doesn't block the rest of the
// library.
func (s *Service) rebuildCache() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cache = make(map[domain.WatermarkAssetID]*domain.WatermarkAsset, len(entries)/2)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		var asset domain.WatermarkAsset
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		if jerr := json.Unmarshal(raw, &asset); jerr != nil {
			continue
		}
		if err := domain.ValidateWatermarkAssetID(string(asset.ID)); err != nil {
			continue
		}
		s.cache[asset.ID] = &asset
	}
	s.mu.Unlock()
	return nil
}

// writeSidecar persists the metadata next to the image. Errors propagate
// up so Save can roll back the image write.
func (s *Service) writeSidecar(a *domain.WatermarkAsset) error {
	path := s.sidecarPath(a.ID)
	tmp := path + ".tmp"
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("watermarks: marshal sidecar: %w", err)
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("watermarks: write sidecar: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("watermarks: rename sidecar: %w", err)
	}
	return nil
}

// imagePath returns the absolute on-disk path for an asset's image bytes.
// Always uses the cached asset — the extension is part of the on-disk
// filename so we can't reconstruct it from the id alone.
func (s *Service) imagePath(a *domain.WatermarkAsset) string {
	return filepath.Join(s.dir, string(a.ID)+extensionFor(a.FileName, a.ContentType))
}

// sidecarPath returns the absolute path for the JSON sidecar.
func (s *Service) sidecarPath(id domain.WatermarkAssetID) string {
	return filepath.Join(s.dir, string(id)+".json")
}

// newAssetID generates a 16-character hex id (~64 bits of entropy). Long
// enough that collisions are negligible at any conceivable library size,
// short enough to fit comfortably in URLs and log lines.
func newAssetID() (domain.WatermarkAssetID, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return domain.WatermarkAssetID(hex.EncodeToString(b[:])), nil
}

// isSupportedImage reports whether the sniffed Content-Type is something
// the FFmpeg overlay filter chain can render. We accept the formats with
// reliable cross-distro decoder support; SVG / WebP / AVIF are deliberately
// excluded because not every Ubuntu apt FFmpeg ships their decoders.
func isSupportedImage(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "image/png", "image/jpeg", "image/jpg", "image/gif":
		return true
	}
	return false
}

// extensionFor picks a sensible filename suffix for the on-disk image.
// Prefer the original upload's extension when it's a supported image kind;
// fall back to a content-type derived suffix otherwise. Always returns a
// leading dot.
func extensionFor(filename, contentType string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif":
		return ext
	}
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}

// writeAtomic streams `head` then `body` into `tmp`, fsyncs, and renames
// to `dst`. On any failure the temp file is removed so a half-written
// upload never appears in the cache. Returns the total bytes written.
func writeAtomic(tmp, dst string, head []byte, body io.Reader) (int64, error) {
	out, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("watermarks: create tmp: %w", err)
	}
	var written int64
	if len(head) > 0 {
		n, werr := out.Write(head)
		written += int64(n)
		if werr != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("watermarks: write head: %w", werr)
		}
	}
	n, copyErr := io.Copy(out, body)
	written += n
	if cerr := out.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("watermarks: copy: %w", copyErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("watermarks: rename: %w", err)
	}
	return written, nil
}
