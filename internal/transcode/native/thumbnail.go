//go:build libav

package native

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
// and shoots it: the host's schedule is the only one.
type thumbnailer struct {
	t      picture.Thumbnail // resolved
	stats  *Stats
	sched  *picture.Schedule // nil key-only
	warned bool

	sws  *astiav.SoftwareScaleContext
	skey scaleKey
}

// scaleKey is what a shot scaler is built for.
type scaleKey struct {
	srcW, srcH, dstW, dstH int
	pf                     astiav.PixelFormat
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
func (th *thumbnailer) make(fr *astiav.Frame) (picture.Shot, error) {
	sar := fr.SampleAspectRatio()
	w, h := picture.ShotSize(fr.Width(), fr.Height(), sar.Num(), sar.Den(), th.t.Height)
	k := scaleKey{srcW: fr.Width(), srcH: fr.Height(), dstW: w, dstH: h, pf: fr.PixelFormat()}
	if err := th.ensureScaler(k); err != nil {
		return picture.Shot{}, err
	}
	dst := astiav.AllocFrame()
	defer dst.Free()
	dst.SetWidth(w)
	dst.SetHeight(h)
	dst.SetPixelFormat(astiav.PixelFormatYuvj420P)
	if err := dst.AllocBuffer(0); err != nil {
		return picture.Shot{}, fmt.Errorf("thumbnail: alloc %dx%d: %w", w, h, err)
	}
	if err := th.sws.ScaleFrame(fr, dst); err != nil {
		return picture.Shot{}, fmt.Errorf("thumbnail: scale to %dx%d: %w", w, h, err)
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
	sws, err := astiav.CreateSoftwareScaleContext(k.srcW, k.srcH, k.pf,
		k.dstW, k.dstH, astiav.PixelFormatYuvj420P,
		astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBicubic))
	if err != nil {
		return fmt.Errorf("thumbnail: scaler %dx%d %s to %dx%d: %w", k.srcW, k.srcH, k.pf, k.dstW, k.dstH, err)
	}
	th.sws, th.skey = sws, k
	return nil
}

// release frees the scaler; the next shot builds a new one.
func (th *thumbnailer) release() {
	if th.sws != nil {
		th.sws.Free()
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
