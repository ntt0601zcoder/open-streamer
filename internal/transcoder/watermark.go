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
func applyWatermark(base string, wm *domain.WatermarkConfig, onGPU bool) string {
	if !wm.IsActive() {
		return base
	}
	r := wm.Resolved()
	switch r.Type {
	case domain.WatermarkTypeText:
		return appendTextWatermark(base, r, onGPU)
	case domain.WatermarkTypeImage:
		return appendImageWatermark(base, r, onGPU)
	}
	return base
}

// appendTextWatermark composes a single-chain filter (no labels needed)
// because drawtext takes one input and produces one output. On GPU
// pipelines we wrap with hwdownload / hwupload_cuda.
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

// appendImageWatermark composes a 3-chain filter graph that loads the image
// via `movie=` (avoids an extra `-i` input which would complicate the
// multi-output args builder), and overlays it on the main stream.
//
// Layout:
//
//	<base>[,hwdownload,format=nv12][mid];
//	movie=...,format=rgba,colorchannelmixer=aa=...[wm];
//	[mid][wm]overlay=x=…:y=…[,hwupload_cuda]
//
// Each `;`-separated segment is its own chain; explicit labels wire them
// together. The implicit `-vf` input feeds the first chain (no `[in]`
// needed), and the final chain implicitly produces the `-vf` output.
func appendImageWatermark(base string, r *domain.WatermarkConfig, onGPU bool) string {
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
