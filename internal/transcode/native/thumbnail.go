//go:build libav

package native

// #cgo pkg-config: libswscale libavutil
// #include <libavutil/frame.h>
// #include <libavutil/opt.h>
// #include <libavutil/pixfmt.h>
// #include <libswscale/swscale.h>
//
// // osv2_unjpeg maps a deprecated full-range yuvj format to its yuv
// // format: the range is given to the scaler explicitly instead.
// static enum AVPixelFormat osv2_unjpeg(enum AVPixelFormat f) {
// 	switch (f) {
// 	case AV_PIX_FMT_YUVJ420P: return AV_PIX_FMT_YUV420P;
// 	case AV_PIX_FMT_YUVJ422P: return AV_PIX_FMT_YUV422P;
// 	case AV_PIX_FMT_YUVJ444P: return AV_PIX_FMT_YUV444P;
// 	case AV_PIX_FMT_YUVJ440P: return AV_PIX_FMT_YUV440P;
// 	case AV_PIX_FMT_YUVJ411P: return AV_PIX_FMT_YUV411P;
// 	default: return f;
// 	}
// }
//
// // osv2_is_yuvj reports whether f is a deprecated full-range yuvj format.
// static int osv2_is_yuvj(int f) { return osv2_unjpeg((enum AVPixelFormat)f) != (enum AVPixelFormat)f; }
//
// // osv2_shot_scaler returns a bicubic scaler from w x h of format f, read
// // in full range when src_full is set and limited range otherwise, to
// // dw x dh yuv420p in full range, as JFIF expects. NULL on failure. The
// // ranges are set before sws_init_context: a scaler initialised first
// // may pick its unscaled copy path, which a later
// // sws_setColorspaceDetails does not undo.
// static struct SwsContext *osv2_shot_scaler(int w, int h, int f, int dw, int dh, int src_full) {
// 	struct SwsContext *c = sws_alloc_context();
// 	if (!c)
// 		return NULL;
// 	if (av_opt_set_int(c, "srcw", w, 0) < 0 || av_opt_set_int(c, "srch", h, 0) < 0 ||
// 	    av_opt_set_int(c, "src_format", osv2_unjpeg((enum AVPixelFormat)f), 0) < 0 ||
// 	    av_opt_set_int(c, "dstw", dw, 0) < 0 || av_opt_set_int(c, "dsth", dh, 0) < 0 ||
// 	    av_opt_set_int(c, "dst_format", AV_PIX_FMT_YUV420P, 0) < 0 ||
// 	    av_opt_set_int(c, "src_range", src_full, 0) < 0 || av_opt_set_int(c, "dst_range", 1, 0) < 0 ||
// 	    av_opt_set_int(c, "sws_flags", SWS_BICUBIC, 0) < 0 || sws_init_context(c, NULL, NULL) < 0) {
// 		sws_freeContext(c);
// 		return NULL;
// 	}
// 	return c;
// }
//
// // osv2_shot_scale scales src into dst; it returns a negative AVERROR or
// // the height written.
// static int osv2_shot_scale(struct SwsContext *c, const AVFrame *src, AVFrame *dst) {
// 	return sws_scale(c, (const uint8_t *const *)src->data, src->linesize, 0, src->height,
// 		dst->data, dst->linesize);
// }
//
// static void osv2_shot_free(struct SwsContext *c) { sws_freeContext(c); }
//
// // End of the cgo helpers.
import "C"

import (
	"errors"
	"fmt"
	"image"
	"log/slog"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// RungShot is Output.Rung for a thumbnail.
const RungShot = -2

// encodeJPEG encodes a shot. A variable so that tests can make it fail.
var encodeJPEG = picture.EncodeJPEG

// thumbnailer makes JPEG shots from decoded pictures. Nothing it does fails
// the pipeline: a shot that cannot be made is counted in
// Stats.ThumbnailsFailed and logged once per pipeline.
//
// With rungs it keeps the schedule and shoots the decoded pictures it finds
// due. Key-only (sched nil) it decodes every key frame it is given alone
// and shoots it: the host's schedule is the only one. A key fails only
// when it gives no picture; one the decoder conceals is shot as concealed.
type thumbnailer struct {
	t      picture.Thumbnail // resolved
	stats  *Stats
	sched  *picture.Schedule // nil key-only
	warned bool

	// sws scales to the shot in full range. It is built through cgo
	// because the source and destination ranges must be set before
	// sws_init_context, which astiav's constructor does not allow.
	sws  *C.struct_SwsContext
	skey scaleKey
}

// scaleKey is what a shot scaler is built for. srcFull is the source's
// colour range: full for a yuvj format or AVCOL_RANGE_JPEG (how HEVC and
// H.264 decoders signal it on yuv420p), limited otherwise.
type scaleKey struct {
	srcW, srcH, dstW, dstH int
	pf                     astiav.PixelFormat
	srcFull                bool
}

// fullRange reports whether fr's samples are full range.
func fullRange(fr *astiav.Frame) bool {
	return fr.ColorRange() == astiav.ColorRangeJpeg || C.osv2_is_yuvj(C.int(fr.PixelFormat())) != 0
}

// newThumbnailer returns a thumbnailer for the resolved t, with a schedule
// when scheduled is set.
func newThumbnailer(t picture.Thumbnail, st *Stats, scheduled bool) *thumbnailer {
	th := &thumbnailer{t: t, stats: st}
	if scheduled {
		th.sched = picture.NewSchedule(t.IntervalSec)
	}
	return th
}

// due reports whether a picture at pts in epoch should be shot, and records
// the attempt: a shot that fails is not tried again before the next
// interval.
func (th *thumbnailer) due(epoch uint32, pts int64) bool {
	if !th.sched.Due(epoch, pts) {
		return false
	}
	th.sched.Took(epoch, pts)
	return true
}

// shoot makes a shot of fr at pts in epoch. It reports false when the shot
// failed, which is counted.
func (th *thumbnailer) shoot(fr *astiav.Frame, pts int64, epoch uint32) (Output, bool) {
	s, err := th.make(fr)
	if err != nil {
		th.fail(err)
		return Output{}, false
	}
	s.PTS, s.Epoch = pts, epoch
	th.stats.Thumbnails++
	return Output{Rung: RungShot, Shot: &s}, true
}

// fromKey decodes the key frame f alone and shoots its picture. It reports
// false when the shot failed, which is counted.
func (th *thumbnailer) fromKey(f media.Frame) (Output, bool) {
	fr, err := decodeKey(f)
	if err != nil {
		th.fail(err)
		return Output{}, false
	}
	defer fr.Free()
	pts := fr.Pts()
	if pts == astiav.NoPtsValue {
		pts = f.PTS
	}
	return th.shoot(fr, pts, f.Epoch)
}

// fail counts a failed shot and logs the first of the pipeline.
func (th *thumbnailer) fail(err error) {
	th.stats.ThumbnailsFailed++
	if th.warned {
		return
	}
	th.warned = true
	slog.Warn("transcode: thumbnail failed; later failures are only counted", "err", err)
}

// make scales fr to the shot size in full-range YUV 4:2:0 and encodes it.
// The scaler reads fr in its own range and writes full range, which is what
// image/jpeg takes an image.YCbCr to hold (JFIF).
func (th *thumbnailer) make(fr *astiav.Frame) (picture.Shot, error) {
	sar := fr.SampleAspectRatio()
	w, h := picture.ShotSize(fr.Width(), fr.Height(), sar.Num(), sar.Den(), th.t.Height)
	k := scaleKey{
		srcW: fr.Width(), srcH: fr.Height(), dstW: w, dstH: h, pf: fr.PixelFormat(), srcFull: fullRange(fr),
	}
	if err := th.ensureScaler(k); err != nil {
		return picture.Shot{}, err
	}
	dst := astiav.AllocFrame()
	defer dst.Free()
	dst.SetWidth(w)
	dst.SetHeight(h)
	dst.SetPixelFormat(astiav.PixelFormatYuv420P)
	dst.SetColorRange(astiav.ColorRangeJpeg)
	if err := dst.AllocBuffer(0); err != nil {
		return picture.Shot{}, fmt.Errorf("thumbnail: alloc %dx%d: %w", w, h, err)
	}
	ret := C.osv2_shot_scale(th.sws, (*C.AVFrame)(fr.UnsafePointer()), (*C.AVFrame)(dst.UnsafePointer()))
	if ret < 0 {
		return picture.Shot{}, fmt.Errorf("thumbnail: scale to %dx%d: %w", w, h, astiav.Error(ret))
	}
	img := &image.YCbCr{}
	if err := dst.Data().ToImage(img); err != nil {
		return picture.Shot{}, fmt.Errorf("thumbnail: read %dx%d: %w", w, h, err)
	}
	b, err := encodeJPEG(img, th.t.Quality)
	if err != nil {
		return picture.Shot{}, fmt.Errorf("thumbnail: %w", err)
	}
	return picture.Shot{Width: w, Height: h, JPEG: b}, nil
}

// ensureScaler (re)creates the scaler for k.
func (th *thumbnailer) ensureScaler(k scaleKey) error {
	if th.sws != nil && k == th.skey {
		return nil
	}
	th.release()
	full := C.int(0)
	if k.srcFull {
		full = 1
	}
	sws := C.osv2_shot_scaler(C.int(k.srcW), C.int(k.srcH), C.int(k.pf), C.int(k.dstW), C.int(k.dstH), full)
	if sws == nil {
		return fmt.Errorf("thumbnail: scaler %dx%d %s (full range %v) to %dx%d",
			k.srcW, k.srcH, k.pf, k.srcFull, k.dstW, k.dstH)
	}
	th.sws, th.skey = sws, k
	return nil
}

// release frees the scaler; the next shot builds a new one.
func (th *thumbnailer) release() {
	if th.sws != nil {
		C.osv2_shot_free(th.sws)
	}
	th.sws, th.skey = nil, scaleKey{}
}

// decodeKey decodes the key frame f with a decoder of its own and returns
// its picture, which the caller frees. The decoder runs on one thread and
// outputs every picture, so that a recovery point or a CRA decoded alone
// still gives one.
func decodeKey(f media.Frame) (*astiav.Frame, error) {
	if f.Config == nil {
		return nil, errors.New("thumbnail: key frame without a config")
	}
	name, err := videoDecoderName(f.Config.Codec)
	if err != nil {
		return nil, fmt.Errorf("thumbnail: %w", err)
	}
	codec := astiav.FindDecoderByName(name)
	if codec == nil {
		return nil, fmt.Errorf("thumbnail: decoder %s not in libavcodec", name)
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, fmt.Errorf("thumbnail: alloc %s context", name)
	}
	defer cc.Free()
	cc.SetTimeBase(timeBase)
	setPktTimebase(cc)
	cc.SetThreadCount(1)
	cc.SetFlags(cc.Flags().Add(astiav.CodecContextFlagOutputCorrupt))
	cc.SetFlags2(cc.Flags2().Add(astiav.CodecFlag2ShowAll))
	if err := cc.Open(codec, nil); err != nil {
		return nil, fmt.Errorf("thumbnail: open %s: %w", name, err)
	}
	pkt := newInPacket()
	defer pkt.free()
	if err := pkt.fill(annexB(f), f.PTS, f.DTS); err != nil {
		return nil, fmt.Errorf("thumbnail: %w", err)
	}
	if err := pkt.send(cc); err != nil {
		return nil, fmt.Errorf("thumbnail: decode: %w", err)
	}
	if err := cc.SendPacket(nil); err != nil {
		return nil, fmt.Errorf("thumbnail: flush: %w", err)
	}
	fr := astiav.AllocFrame()
	switch err := cc.ReceiveFrame(fr); {
	case errors.Is(err, astiav.ErrEof), errors.Is(err, astiav.ErrEagain):
		fr.Free()
		return nil, errors.New("thumbnail: the key frame gave no picture")
	case err != nil:
		fr.Free()
		return nil, fmt.Errorf("thumbnail: decode: %w", err)
	}
	return fr, nil
}
