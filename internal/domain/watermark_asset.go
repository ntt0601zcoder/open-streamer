package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxWatermarkAssetIDLen is the maximum length of an asset ID. The bound
// matches the rendered filename length the watermarks service produces on
// disk so it can never overflow PATH_MAX in practice.
const MaxWatermarkAssetIDLen = 64

// MaxWatermarkAssetNameLen caps the human display name. UI dropdowns and
// activity logs render this; oversize values would push the layout around.
const MaxWatermarkAssetNameLen = 128

// MaxWatermarkAssetBytes caps a single uploaded watermark. Logos are
// typically a few hundred KB at most; the cap defends the assets directory
// against a runaway upload accidentally filling the volume.
const MaxWatermarkAssetBytes int64 = 8 * 1024 * 1024 // 8 MiB

// WatermarkAssetID is the stable identifier for an uploaded watermark
// asset. It is the basename (without extension) of the file on disk and
// also the identifier referenced from Stream.Watermark.AssetID.
type WatermarkAssetID string

var watermarkAssetIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateWatermarkAssetID enforces the safe filename charset. Asset IDs
// flow into filesystem paths, so we forbid anything that would let a caller
// escape the assets directory or collide with sidecar metadata names.
func ValidateWatermarkAssetID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("watermark asset id is required")
	}
	if len(id) > MaxWatermarkAssetIDLen {
		return fmt.Errorf("watermark asset id exceeds max length %d", MaxWatermarkAssetIDLen)
	}
	if !watermarkAssetIDPattern.MatchString(id) {
		return errors.New("watermark asset id must contain only a-z, A-Z, 0-9, '-' and '_'")
	}
	return nil
}

// WatermarkAsset is the persisted metadata for one uploaded watermark
// image. The actual image bytes live next to the metadata sidecar in
// the assets directory. Both files share the asset ID basename:
//
//	<assets_dir>/<id>.<ext>     ← image
//	<assets_dir>/<id>.json      ← this struct serialised
//
// The simple two-file layout means we don't need a separate database
// for watermark metadata; an `os.ReadDir` rebuilds the registry after
// any restart.
type WatermarkAsset struct {
	// ID is the immutable filesystem-safe identifier (also the on-disk basename).
	ID WatermarkAssetID `json:"id" yaml:"id"`

	// Name is the human display label shown in UIs. Defaults to the upload's
	// original filename when the operator doesn't override.
	Name string `json:"name" yaml:"name"`

	// FileName is the original filename at upload time, preserved for
	// audit / download responses. Includes extension (e.g. "logo.png").
	FileName string `json:"file_name" yaml:"file_name"`

	// ContentType is the MIME type sniffed at upload time
	// (e.g. "image/png", "image/jpeg"). Used for Content-Type on /raw GET.
	ContentType string `json:"content_type" yaml:"content_type"`

	// SizeBytes is the on-disk size of the image. Sidecar JSON is excluded.
	SizeBytes int64 `json:"size_bytes" yaml:"size_bytes"`

	// UploadedAt is the wall-clock UTC time the asset was first stored.
	UploadedAt time.Time `json:"uploaded_at" yaml:"uploaded_at"`
}
