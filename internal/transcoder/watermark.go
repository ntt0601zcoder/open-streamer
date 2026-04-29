package transcoder

// watermark.go — text & image overlay filter graph generation.
//
// Two output flavours, depending on the active hardware pipeline:
//
//   - CPU path (libx264 / VAAPI / VideoToolbox / NVENC-with-no-cuda-overlay):
//     drawtext / overlay run directly on CPU frames. Filter chain stays
//     a single -vf chain.
//
//   - GPU/NVENC path:
//     drawtext is CPU-only and overlay_cuda requires --enable-cuda-nvcc
//     (Ubuntu apt builds skip it). To stay portable across distros we
//     round-trip through CPU memory: scale_cuda → hwdownload → drawtext/
//     overlay → hwupload_cuda. The cost is ~1 frame copy in each direction,
//     measured around 4-6% of one CPU core per FFmpeg process at 1080p25 —
//     acceptable trade-off vs requiring a custom FFmpeg build on every
//     deployment.
//
// Image overlays use the `movie=` source filter instead of an extra `-i`
// input so the args structure (single -vf) stays uniform with text watermarks
// and the multi-output mode doesn't need filter_complex restructuring.

import (
	"fmt"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// applyWatermark wraps the existing scale/setsar chain (`base`) with a
// watermark filter graph. Returns the original chain unchanged when the
// watermark config is missing / disabled / inactive.
//
// onGPU=true triggers a hwdownload→filter→hwupload_cuda round-trip because
// drawtext is CPU-only and overlay_cuda isn't part of every distro's
// FFmpeg build (Ubuntu apt). The portability win outweighs the ~5% CPU
// cost of the round-trip.
//
// frameScale is the per-profile sizing factor relative to the largest
// profile in the ladder (computed by buildVideoFilter via
// computeWatermarkFrameScale). 1.0 = native pixel size; <1.0 = proportional
// shrink. Only consulted when the operator opted into Resize=true; ignored
// otherwise so configs without resize behave as before.
func applyWatermark(base string, wm *domain.WatermarkConfig, onGPU bool, frameScale float64) string {
	if !wm.IsActive() {
		return base
	}
	r := scaledForRender(wm.Resolved(), frameScale)
	switch r.Type {
	case domain.WatermarkTypeText:
		return appendTextWatermark(base, r, onGPU)
	case domain.WatermarkTypeImage:
		return appendImageWatermark(base, r, onGPU, frameScale)
	}
	return base
}

// scaledForRender returns a deep-enough copy of r whose pixel-scale fields
// (FontSize, OffsetX, OffsetY) are multiplied by frameScale. Used by the
// reference-frame sizing model: largest profile renders at native size
// (frameScale=1.0); smaller profiles shrink everything proportionally so
// the on-screen ratio stays consistent across the ladder.
//
// When Resize=false or frameScale>=1.0 the input is returned unchanged
// (cheap fast-path — same pointer, no allocation).
func scaledForRender(r *domain.WatermarkConfig, frameScale float64) *domain.WatermarkConfig {
	if r == nil || !r.Resize || frameScale >= 1.0 {
		return r
	}
	out := *r
	if out.FontSize > 0 {
		out.FontSize = int(float64(out.FontSize)*frameScale + 0.5)
	}
	if out.OffsetX > 0 {
		out.OffsetX = int(float64(out.OffsetX)*frameScale + 0.5)
	}
	if out.OffsetY > 0 {
		out.OffsetY = int(float64(out.OffsetY)*frameScale + 0.5)
	}
	return &out
}

// appendTextWatermark composes a single-chain filter (no labels needed)
// because drawtext takes one input and produces one output. On GPU
// pipelines we wrap with hwdownload / hwupload_cuda.
//
// Resize-aware sizing happens upstream in scaledForRender: by the time we
// see r, FontSize and offsets already reflect the per-profile shrink ratio
// (or the original values when Resize=false / largest profile).
func appendTextWatermark(base string, r *domain.WatermarkConfig, onGPU bool) string {
	x, y := resolveTextCoords(r)
	dt := strings.Join([]string{
		"drawtext=text=" + ffmpegQuote(r.Text),
		"fontsize=" + itoa(r.FontSize),
		"fontcolor=" + ffmpegQuote(fmt.Sprintf("%s@%.2f", r.FontColor, r.Opacity)),
		"x=" + ffmpegQuote(x),
		"y=" + ffmpegQuote(y),
	}, ":")
	if r.FontFile != "" {
		dt += ":fontfile=" + ffmpegQuote(r.FontFile)
	}

	if !onGPU {
		return joinChain(base, dt)
	}
	return joinChain(base, "hwdownload", "format=nv12", dt, "hwupload_cuda")
}

// appendImageWatermark composes the filter graph that loads the watermark
// asset via `movie=` and overlays it on the main stream.
//
// Sizing model: the asset's NATIVE pixel dimensions render as-is on the
// largest profile in the ladder; smaller profiles scale the asset down by
// `frameScale` (their_width / largest_width) so the on-screen ratio stays
// constant whatever rendition the viewer plays. This treats the asset
// shape (tight-cropped logo vs full-frame transparent canvas with embedded
// logo) uniformly — the operator designs once at the top rendition and
// every smaller rendition matches the visual proportion automatically.
// The asset's intrinsic dimensions also dictate position interaction with
// `overlay=` corner expressions: a full-canvas asset placed at top_left
// with offset 0 covers the whole frame; a tight logo placed at top_right
// with offset 10 lands at the corner.
//
// Layout (Resize=false OR frameScale==1.0 — native pixel size):
//
//	<base>[,hwdownload,format=nv12][mid];
//	movie=...,format=rgba,colorchannelmixer=aa=...[wm];
//	[mid][wm]overlay=x=…:y=…[,hwupload_cuda]
//
// Layout (Resize=true with frameScale<1.0 — proportional shrink):
//
//	<base>...[mid];
//	movie=...,format=rgba,scale=iw*<f>:ih*<f>...[wm];
//	[mid][wm]overlay=x=…:y=…[,hwupload_cuda]
//
// The shrink lives inside the movie chain (a plain `scale` filter, no
// scale2ref) — no second input is needed since we just multiply the
// asset's own iw/ih by a precomputed constant. Offsets baked into the
// overlay coords are likewise pre-scaled (see scaledForRender) so corner
// padding stays visually consistent across the ladder.
func appendImageWatermark(base string, r *domain.WatermarkConfig, onGPU bool, frameScale float64) string {
	leading := base
	if onGPU {
		leading = joinChain(leading, "hwdownload", "format=nv12")
	}
	leading += "[mid]"

	srcParts := []string{
		"movie=" + ffmpegQuote(r.ImagePath),
		"format=rgba",
	}
	if r.Opacity > 0 && r.Opacity < 1 {
		srcParts = append(srcParts, fmt.Sprintf("colorchannelmixer=aa=%.2f", r.Opacity))
	}
	if r.Resize && frameScale > 0 && frameScale < 1.0 {
		// Static scale ratio — known at filter-graph build time, no runtime
		// expression. iw/ih here are the watermark asset's own pre-scale
		// dimensions (movie filter input).
		srcParts = append(srcParts, fmt.Sprintf("scale=iw*%.4f:ih*%.4f", frameScale, frameScale))
	}
	srcChain := strings.Join(srcParts, ",") + "[wm]"

	x, y := resolveImageCoords(r)
	overlay := fmt.Sprintf("[mid][wm]overlay=x=%s:y=%s", ffmpegQuote(x), ffmpegQuote(y))
	if onGPU {
		overlay += ",hwupload_cuda"
	}
	return leading + ";" + srcChain + ";" + overlay
}

// resolveTextCoords picks the drawtext coordinate expressions for the given
// watermark config. position=custom forwards user-supplied X/Y unchanged
// (defaulting to "0" when blank), giving operators full FFmpeg expression
// power; presets dispatch to the canned drawTextCoords() formulas.
func resolveTextCoords(r *domain.WatermarkConfig) (x, y string) {
	if r.Position == domain.WatermarkCustom {
		return defaultExpr(r.X), defaultExpr(r.Y)
	}
	return drawTextCoords(r.Position, r.OffsetX, r.OffsetY)
}

// resolveImageCoords is the overlay-flavoured equivalent of resolveTextCoords.
// Same X/Y forwarding; preset presets dispatch to overlayCoords (capital W/H,
// lowercase w/h).
func resolveImageCoords(r *domain.WatermarkConfig) (x, y string) {
	if r.Position == domain.WatermarkCustom {
		return defaultExpr(r.X), defaultExpr(r.Y)
	}
	return overlayCoords(r.Position, r.OffsetX, r.OffsetY)
}

// defaultExpr substitutes "0" for an empty / whitespace-only expression so
// the resulting FFmpeg fragment is always syntactically valid.
func defaultExpr(s string) string {
	if t := strings.TrimSpace(s); t != "" {
		return t
	}
	return "0"
}

// drawTextCoords returns the FFmpeg expression for the (x, y) drawtext
// origin given a position preset. `tw`/`th` are drawtext-evaluated text
// width/height; `w`/`h` are the input frame size.
func drawTextCoords(pos domain.WatermarkPosition, ox, oy int) (x, y string) {
	switch pos {
	case domain.WatermarkTopLeft:
		return itoa(ox), itoa(oy)
	case domain.WatermarkTopRight:
		return fmt.Sprintf("w-tw-%d", ox), itoa(oy)
	case domain.WatermarkBottomLeft:
		return itoa(ox), fmt.Sprintf("h-th-%d", oy)
	case domain.WatermarkCenter:
		return "(w-tw)/2", "(h-th)/2"
	case domain.WatermarkCustom:
		// Caller (resolveTextCoords) intercepts WatermarkCustom before
		// this fn is reached, but cover it for exhaustiveness — fall
		// through to the BottomRight default rather than emitting a
		// useless empty expression.
		fallthrough
	case domain.WatermarkBottomRight:
		fallthrough
	default:
		return fmt.Sprintf("w-tw-%d", ox), fmt.Sprintf("h-th-%d", oy)
	}
}

// overlayCoords returns the FFmpeg expression for the (x, y) overlay origin.
// `W`/`H` are the main video size; `w`/`h` are the overlay (image) size.
func overlayCoords(pos domain.WatermarkPosition, ox, oy int) (x, y string) {
	switch pos {
	case domain.WatermarkTopLeft:
		return itoa(ox), itoa(oy)
	case domain.WatermarkTopRight:
		return fmt.Sprintf("W-w-%d", ox), itoa(oy)
	case domain.WatermarkBottomLeft:
		return itoa(ox), fmt.Sprintf("H-h-%d", oy)
	case domain.WatermarkCenter:
		return "(W-w)/2", "(H-h)/2"
	case domain.WatermarkCustom:
		// resolveImageCoords intercepts custom before us; fall through
		// to BottomRight default for exhaustiveness.
		fallthrough
	case domain.WatermarkBottomRight:
		fallthrough
	default:
		return fmt.Sprintf("W-w-%d", ox), fmt.Sprintf("H-h-%d", oy)
	}
}

// joinChain joins filter nodes with a comma, skipping empty entries so
// callers can feed in optional fragments without sentinel checks.
func joinChain(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// ffmpegQuote wraps a value in single quotes, escaping any embedded
// single-quote with the FFmpeg-mandated `\'` outside-the-quotes sequence.
// Safe for arbitrary user-supplied strings (paths, drawtext bodies, color
// expressions). FFmpeg requires this within filtergraph descriptors when
// a value contains the graph delimiters comma, semicolon, colon, square
// brackets, or whitespace.
func ffmpegQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
