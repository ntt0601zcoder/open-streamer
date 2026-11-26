//go:build libav

package native

import (
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// The encoders a Rung can name.
const (
	EncoderX264  = "x264"
	EncoderNVENC = "nvenc"
)

const (
	// defaultNVENCPreset is the NVENC preset of a rung that names none.
	defaultNVENCPreset = "p4"
	// nvencGOP is the NVENC keyframe interval, long enough that the
	// source's keys force every IDR, like x264Params' keyint.
	nvencGOP = 600
	// probeWidth, probeHeight and probeFPS are the encoder ProbeNVENC
	// opens; probeWidth × probeHeight is also the picture ProbeFont draws.
	probeWidth, probeHeight, probeFPS = 256, 144, 25
	// probeKbps is the probe encoder's bitrate.
	probeKbps = 200
)

// nvencCodec is the libavcodec encoder of NVENC rungs. A variable so that
// tests can name one that is missing.
var nvencCodec = "h264_nvenc"

// x264Codec is the libavcodec encoder of libx264 rungs and of the NVENC
// fallback. A variable so that tests can name one that is missing.
var x264Codec = "libx264"

// nvencGPU is the NVENC gpu option; −1 leaves libavcodec's default (any
// device). Tests set a GPU that does not exist to force a failed open. A
// test that sets it restores it and is never parallel.
var nvencGPU = -1

// openRungs is the number of rung encoders open in the process: opened by
// openRung, released by rungEncoder.release. Tests check that a failed
// build leaves none behind.
var openRungs atomic.Int64

// RefusedError is the build error of a NoFallback rung whose encoder could
// not be opened.
type RefusedError struct {
	Rung int
	Err  error // the NVENC failure
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("rung %d: nvenc could not be opened and no_fallback is set: %v", e.Rung, e.Err)
}

func (e *RefusedError) Unwrap() error { return e.Err }

// nvencOptions are the h264_nvenc options of a rung, in a fixed order.
// Keyframes stay where the pipeline puts them: forced IDRs, no scene cuts,
// no lookahead and a strict GOP. delay 0 returns each packet as soon as it
// is encoded, aud 0 writes no access unit delimiters, and B-frames are
// never references, which keeps the reorder depth the SPS announces at 1.
func nvencOptions(r Rung) [][2]string {
	return [][2]string{
		{"preset", r.Preset},
		{"tune", "hq"},
		{"rc", "vbr"},
		{"profile", "high"},
		{"forced-idr", "1"},
		{"no-scenecut", "1"},
		{"rc-lookahead", "0"},
		{"strict_gop", "1"},
		{"delay", "0"},
		{"aud", "0"},
		{"b_ref_mode", "disabled"},
	}
}

// validNVENCPreset reports whether p is one of p1 to p7.
func validNVENCPreset(p string) bool {
	return len(p) == 2 && p[0] == 'p' && p[1] >= '1' && p[1] <= '7'
}

// openEncoders opens every rung or none: on a failure it closes the rungs
// it opened. When the build succeeds, each NVENC rung that fell back to
// libx264 counts in EncoderFallbacks, and one warning is logged for the
// build. A failed build, a fallback that failed too or a NoFallback rung's
// *RefusedError included, is the build's error and counts nothing.
func (v *videoPath) openEncoders(fr *astiav.Frame) error {
	encs := make([]*rungEncoder, 0, len(v.rungs))
	var (
		fellBack []int
		firstErr error
	)
	for i, r := range v.rungs {
		e, nvErr, err := openRung(i, r, fr, v.fps, &v.caches[i])
		if err != nil {
			for _, o := range encs {
				o.release()
			}
			return err
		}
		if nvErr != nil {
			fellBack = append(fellBack, i)
			if firstErr == nil {
				firstErr = nvErr
			}
		}
		encs = append(encs, e)
	}
	v.encs = encs
	if len(fellBack) > 0 {
		v.stats.EncoderFallbacks += uint64(len(fellBack))
		slog.Warn("transcode: nvenc open failed, using libx264", "rungs", fellBack, "err", firstErr)
	}
	return nil
}

// openRung opens the rung's encoder. An NVENC rung that fails to open is
// encoded by libx264 with the rung's height, rates and B-frames and preset
// veryfast; nvErr is then the NVENC failure, and err the libx264 one, if
// that failed too. With NoFallback, err is instead a *RefusedError and
// libx264 is not tried.
func openRung(idx int, r Rung, fr *astiav.Frame, fps astiav.Rational, cache *media.ConfigCache) (e *rungEncoder, nvErr, err error) {
	w := evenWidth(fr.Width(), fr.Height(), r.Height)
	sar := fr.SampleAspectRatio()
	var cc *astiav.CodecContext
	if r.Encoder == EncoderNVENC {
		if cc, nvErr = openNVENC(idx, r, w, sar, fps); nvErr != nil {
			if r.NoFallback {
				return nil, nvErr, &RefusedError{Rung: idx, Err: nvErr}
			}
			r.Preset = defaultPreset
		}
	}
	if cc == nil {
		if cc, err = openX264(idx, r, w, sar, fps); err != nil {
			return nil, nvErr, err
		}
	}
	openRungs.Add(1)
	return &rungEncoder{idx: idx, cache: cache, cc: cc, pkt: astiav.AllocPacket(), w: w, h: r.Height}, nvErr, nil
}

// newVideoContext allocates an encoder context for the rung at w × height,
// with the settings libx264 and NVENC share.
func newVideoContext(codec *astiav.Codec, name string, r Rung, w int, sar, fps astiav.Rational) (*astiav.CodecContext, error) {
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, fmt.Errorf("video encoder: alloc %s context", name)
	}
	cc.SetWidth(w)
	cc.SetHeight(r.Height)
	cc.SetPixelFormat(astiav.PixelFormatYuv420P)
	cc.SetTimeBase(timeBase)
	cc.SetFramerate(fps)
	if sar.Num() > 0 && sar.Den() > 0 {
		cc.SetSampleAspectRatio(sar)
	}
	cc.SetBitRate(int64(r.BitrateKbps) * 1000)
	cc.SetRateControlMaxRate(int64(r.MaxBitrateKbps) * 1000)
	cc.SetRateControlBufferSize(2 * r.MaxBitrateKbps * 1000)
	cc.SetMaxBFrames(r.BFrames)
	return cc, nil
}

// openCodec sets opts on cc and opens it; on a failure it frees cc. idx is
// the rung named in the error, none when negative.
func openCodec(cc *astiav.CodecContext, codec *astiav.Codec, opts [][2]string, idx int, name string, w, h int) error {
	d := astiav.NewDictionary()
	defer d.Free()
	for _, kv := range opts {
		if err := d.Set(kv[0], kv[1], 0); err != nil {
			cc.Free()
			return fmt.Errorf("video encoder: option %s: %w", kv[0], err)
		}
	}
	if err := cc.Open(codec, d); err != nil {
		cc.Free()
		if idx < 0 {
			return fmt.Errorf("video encoder: open %s %dx%d: %w", name, w, h, err)
		}
		return fmt.Errorf("video encoder: rung %d: open %s %dx%d: %w", idx, name, w, h, err)
	}
	return nil
}

// openX264 opens libx264 for the rung.
func openX264(idx int, r Rung, w int, sar, fps astiav.Rational) (*astiav.CodecContext, error) {
	codec := astiav.FindEncoderByName(x264Codec)
	if codec == nil {
		return nil, fmt.Errorf("video encoder: %s not in libavcodec", x264Codec)
	}
	cc, err := newVideoContext(codec, "libx264", r, w, sar, fps)
	if err != nil {
		return nil, err
	}
	cc.SetThreadCount(0)
	opts := [][2]string{{"preset", r.Preset}, {"x264-params", x264Params}, {"forced-idr", "1"}}
	if err := openCodec(cc, codec, opts, idx, "libx264", w, r.Height); err != nil {
		return nil, err
	}
	return cc, nil
}

// openNVENC opens h264_nvenc for rung idx (none when negative). Its
// packets go through the same frame path as libx264's: NVENC repeats SPS
// and PPS in band on every IDR.
func openNVENC(idx int, r Rung, w int, sar, fps astiav.Rational) (*astiav.CodecContext, error) {
	codec := astiav.FindEncoderByName(nvencCodec)
	if codec == nil {
		return nil, fmt.Errorf("video encoder: %s not in libavcodec", nvencCodec)
	}
	cc, err := newVideoContext(codec, nvencCodec, r, w, sar, fps)
	if err != nil {
		return nil, err
	}
	cc.SetGopSize(nvencGOP)
	opts := nvencOptions(r)
	if nvencGPU != -1 {
		opts = append(opts, [2]string{"gpu", strconv.Itoa(nvencGPU)})
	}
	if err := openCodec(cc, codec, opts, idx, nvencCodec, w, r.Height); err != nil {
		return nil, err
	}
	return cc, nil
}

// ProbeNVENC opens and closes an h264_nvenc encoder at 256×144, 25 fps,
// preset p4. It returns an error when libavcodec has no h264_nvenc or no
// NVENC device opens.
func ProbeNVENC() error {
	r := Rung{
		Height: probeHeight, BitrateKbps: probeKbps, MaxBitrateKbps: probeKbps,
		Preset: defaultNVENCPreset, Encoder: EncoderNVENC,
	}
	cc, err := openNVENC(-1, r, probeWidth, astiav.Rational{}, astiav.NewRational(probeFPS, 1))
	if err != nil {
		return fmt.Errorf("transcode: probe nvenc: %w", err)
	}
	cc.Free()
	return nil
}
