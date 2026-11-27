//go:build libav

package native

import "github.com/ntt0601zcoder/open-streamer/internal/media"

// startCode prefixes every NAL unit handed to a decoder.
var startCode = []byte{0, 0, 0, 1}

// annexB builds the decoder input for one video frame: its NAL units in
// Annex B form, with every VPS, SPS and PPS of its Config in front on a key
// frame, so a decoder can start on any key.
func annexB(f media.Frame) []byte {
	var sets [][]byte
	if f.Key && f.Config != nil {
		sets = make([][]byte, 0, len(f.Config.VPS)+len(f.Config.SPS)+len(f.Config.PPS))
		sets = append(sets, f.Config.VPS...)
		sets = append(sets, f.Config.SPS...)
		sets = append(sets, f.Config.PPS...)
	}
	n := 0
	for _, s := range sets {
		n += len(startCode) + len(s)
	}
	for _, d := range f.Data {
		n += len(startCode) + len(d)
	}
	out := make([]byte, 0, n)
	for _, s := range sets {
		out = append(out, startCode...)
		out = append(out, s...)
	}
	for _, d := range f.Data {
		out = append(out, startCode...)
		out = append(out, d...)
	}
	return out
}

// splitAnnexB returns the NAL units of an Annex B byte stream without their
// start codes. The units alias b. Empty units are skipped.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1 // first byte of the current unit
	i := 0
	for i+2 < len(b) {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			i++
			continue
		}
		if start >= 0 {
			end := i
			// A four-byte start code leaves one zero byte on the previous unit.
			for end > start && b[end-1] == 0 {
				end--
			}
			if end > start {
				out = append(out, b[start:end])
			}
		}
		i += 3
		start = i
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// H.264 NAL unit types the pipeline looks at.
const (
	nalSPS = 7
	nalPPS = 8
	nalAUD = 9
)

// h264Type returns the type of an H.264 NAL unit.
func h264Type(n []byte) byte { return n[0] & 0x1f }
