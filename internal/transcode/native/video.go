//go:build libav

package native

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	// x264Params keeps keyframes where the pipeline puts them: no scene
	// cuts, a keyframe interval long enough that the source's keys force
	// every IDR, and IDRs allowed at any distance. b-pyramid=none (ruling
	// R-T7b'): with x264's default B-pyramid, VLC 3.0.20 logged "buffer
	// deadlock prevented" at the start of 5 of 16 HLS-TS plays of an
	// encoded rung (0 of 8 for the copied source); without it, 0 of 8.
	// It also halves the reorder depth and DPB the SPS announces (2 → 1,
	// 4 → 2).
	x264Params = "scenecut=0:keyint=600:min-keyint=1:rc-lookahead=10:b-pyramid=none"
	// fpsDeltas is how many DTS deltas of the source estimate its framerate.
	fpsDeltas = 8
	// defaultFPS is the framerate when the source gives no delta.
	defaultFPS = 25
	// fpsSnap is the relative distance within which an estimate is taken as
	// the nearest common framerate.
	fpsSnap = 0.01
)

// timeBase is the time base of every codec context: 90 kHz, like Frame.
var timeBase = astiav.NewRational(1, media.ClockRate)

// commonFPS are the framerates an estimate snaps to.
var commonFPS = []astiav.Rational{
	astiav.NewRational(10, 1), astiav.NewRational(12, 1), astiav.NewRational(25, 2),
	astiav.NewRational(15, 1), astiav.NewRational(20, 1), astiav.NewRational(24000, 1001),
	astiav.NewRational(24, 1), astiav.NewRational(25, 1), astiav.NewRational(30000, 1001),
	astiav.NewRational(30, 1), astiav.NewRational(48, 1), astiav.NewRational(50, 1),
	astiav.NewRational(60000, 1001), astiav.NewRational(60, 1), astiav.NewRational(100, 1),
	astiav.NewRational(120, 1),
}

// videoPath decodes the encoded video track once and encodes every rung.
//
// A build starts at a key of the track: the decoder is opened for its
// Config, and the rung encoders at the first decoded frame. drain flushes
// and releases a build.
type videoPath struct {
	stats  *Stats
	rungs  []Rung
	caches []media.ConfigCache // one per rung, kept across builds
	track  int

	// The current build; dec is nil between builds.
	cfg      *media.TrackConfig
	dec      *astiav.CodecContext
	pkt      *inPacket
	frm      *astiav.Frame
	startPTS int64          // PTS of the build's first key; earlier pictures are not encoded
	skipRASL bool           // until the first trailing picture after the start key
	keys     []int64        // source key PTS not yet matched to a decoded picture, ascending
	pending  []media.Frame  // input held until the framerate is known
	seenDTS  []int64        // DTS of every frame received while the framerate is unknown, skipped ones too
	encs     []*rungEncoder // opened at the build's first decoded picture
	corrupt  corruptRun     // the decoder's consecutive corrupt frames

	// The framerate, estimated once per source config.
	fpsCfg *media.TrackConfig
	fps    astiav.Rational
}

func newVideoPath(rungs []Rung, st *Stats) *videoPath {
	return &videoPath{stats: st, rungs: rungs, caches: make([]media.ConfigCache, len(rungs))}
}

// push feeds one frame of the encoded video track.
func (v *videoPath) push(f media.Frame, epoch uint32) ([]Output, error) {
	var out []Output
	if v.dec != nil && f.Config != v.cfg {
		o, err := v.drain(epoch)
		out = o
		if err != nil {
			return out, err
		}
	}
	if v.dec == nil {
		if !f.Key {
			return out, nil
		}
		if err := v.build(f); err != nil {
			return out, err
		}
	}
	estimating := v.fpsCfg != v.cfg
	if estimating {
		v.seenDTS = append(v.seenDTS, f.DTS)
	}
	if v.skipRASL && !f.Key {
		if f.RASL {
			return out, nil
		}
		v.skipRASL = false
	}
	v.track = f.Track
	if estimating {
		v.pending = append(v.pending, f)
		if len(v.seenDTS) <= fpsDeltas {
			return out, nil
		}
		o, err := v.feedPending(epoch)
		return append(out, o...), err
	}
	o, err := v.feed(f, epoch)
	return append(out, o...), err
}

// build opens a decoder for the key frame f.
func (v *videoPath) build(f media.Frame) error {
	var name string
	switch f.Config.Codec {
	case media.CodecH264:
		name = "h264"
	case media.CodecH265:
		name = "hevc"
	case media.CodecAAC:
		return fmt.Errorf("video decoder: codec %s is not video", f.Config.Codec)
	default:
		return fmt.Errorf("video decoder: unknown codec %d", f.Config.Codec)
	}
	codec := astiav.FindDecoderByName(name)
	if codec == nil {
		return fmt.Errorf("video decoder: %s not in libavcodec", name)
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return fmt.Errorf("video decoder: alloc %s context", name)
	}
	cc.SetTimeBase(timeBase)
	setPktTimebase(cc)
	cc.SetThreadCount(0)
	if err := cc.Open(codec, nil); err != nil {
		cc.Free()
		return fmt.Errorf("video decoder: open %s: %w", name, err)
	}
	v.cfg, v.dec = f.Config, cc
	v.pkt, v.frm = newInPacket(), astiav.AllocFrame()
	v.startPTS, v.skipRASL = f.PTS, true
	v.corrupt = corruptRun{}
	return nil
}

// feedPending estimates the framerate from the DTS received so far and
// feeds the held frames.
func (v *videoPath) feedPending(epoch uint32) ([]Output, error) {
	v.fps, v.fpsCfg = estimateFPS(v.seenDTS), v.cfg
	held := v.pending
	v.pending, v.seenDTS = nil, nil
	var out []Output
	for _, f := range held {
		o, err := v.feed(f, epoch)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// estimateFPS returns the framerate of up to fpsDeltas+1 DTS in decode
// order: the median of their deltas, snapped to a common rate within 1 %.
// The median ignores a delta that spans frames not received.
func estimateFPS(dts []int64) astiav.Rational {
	deltas := make([]int64, 0, len(dts))
	for i := 1; i < len(dts); i++ {
		if d := dts[i] - dts[i-1]; d > 0 {
			deltas = append(deltas, d)
		}
	}
	if len(deltas) == 0 {
		return astiav.NewRational(defaultFPS, 1)
	}
	slices.Sort(deltas)
	n := len(deltas)
	med := float64(deltas[n/2])
	if n%2 == 0 {
		med = float64(deltas[n/2-1]+deltas[n/2]) / 2
	}
	r := media.ClockRate / med
	best, bestDist := astiav.Rational{}, math.Inf(1)
	for _, c := range commonFPS {
		if d := math.Abs(r-c.Float64()) / c.Float64(); d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist <= fpsSnap {
		return best
	}
	r = min(max(r, 1), 240)
	num, den := int(math.Round(r*1000)), 1000
	g := gcd(num, den)
	return astiav.NewRational(num/g, den/g)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// feed sends one frame to the decoder and encodes every picture it returns.
func (v *videoPath) feed(f media.Frame, epoch uint32) ([]Output, error) {
	if err := v.pkt.fill(annexB(f), f.PTS, f.DTS); err != nil {
		return nil, fmt.Errorf("video decoder: %w", err)
	}
	if f.Key {
		v.keys = append(v.keys, f.PTS)
	}
	// A corrupt access unit is skipped and counted. Damage the decoder can
	// conceal is not an error.
	switch err := v.pkt.send(v.dec); {
	case isCorruptData(err):
		if err := v.corrupt.add(&v.stats.VideoCorrupt, err); err != nil {
			return nil, fmt.Errorf("video decoder: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("video decoder: send: %w", err)
	}
	return v.receive(epoch)
}

// receive encodes every picture the decoder has ready.
func (v *videoPath) receive(epoch uint32) ([]Output, error) {
	var out []Output
	for {
		err := v.dec.ReceiveFrame(v.frm)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return out, nil
		case isCorruptData(err):
			// A picture that failed to decode is skipped.
			if err := v.corrupt.add(&v.stats.VideoCorrupt, err); err != nil {
				return out, fmt.Errorf("video decoder: %w", err)
			}
			continue
		case err != nil:
			return out, fmt.Errorf("video decoder: receive: %w", err)
		}
		v.corrupt.ok()
		o, err := v.encode(v.frm, epoch)
		v.frm.Unref()
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
}

// encode sends one decoded picture to every rung. A picture at the PTS of a
// source key is forced to an IDR.
func (v *videoPath) encode(fr *astiav.Frame, epoch uint32) ([]Output, error) {
	pts := fr.Pts()
	if pts == astiav.NoPtsValue || pts < v.startPTS {
		return nil, nil
	}
	if v.encs == nil {
		if err := v.openEncoders(fr); err != nil {
			return nil, err
		}
	}
	force := false
	for len(v.keys) > 0 && v.keys[0] <= pts {
		force = force || v.keys[0] == pts
		v.keys = v.keys[1:]
	}
	var out []Output
	for _, e := range v.encs {
		o, err := e.encode(fr, pts, force, epoch, v.track)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// openEncoders opens every rung or none: on a failure it closes the rungs
// it opened.
func (v *videoPath) openEncoders(fr *astiav.Frame) error {
	encs := make([]*rungEncoder, 0, len(v.rungs))
	for i, r := range v.rungs {
		e, err := openRung(i, r, fr, v.fps, &v.caches[i])
		if err != nil {
			for _, o := range encs {
				o.release()
			}
			return err
		}
		encs = append(encs, e)
	}
	v.encs = encs
	return nil
}

// drain flushes the build's held input, decoder and encoders, then
// releases it.
func (v *videoPath) drain(epoch uint32) ([]Output, error) {
	if v.dec == nil {
		return nil, nil
	}
	defer v.release()
	var out []Output
	if len(v.pending) > 0 {
		o, err := v.feedPending(epoch)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	if err := v.dec.SendPacket(nil); err != nil {
		return out, fmt.Errorf("video decoder: flush: %w", err)
	}
	o, err := v.receive(epoch)
	out = append(out, o...)
	if err != nil {
		return out, err
	}
	for _, e := range v.encs {
		o, err := e.flush(epoch, v.track)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// release frees the build without draining it.
func (v *videoPath) release() {
	for _, e := range v.encs {
		e.release()
	}
	if v.dec != nil {
		v.dec.Free()
	}
	if v.pkt != nil {
		v.pkt.free()
	}
	if v.frm != nil {
		v.frm.Free()
	}
	v.cfg, v.dec, v.pkt, v.frm = nil, nil, nil, nil
	v.keys, v.pending, v.seenDTS, v.encs = nil, nil, nil, nil
}

// rungEncoder scales decoded pictures to one rung and encodes them with
// libx264.
type rungEncoder struct {
	idx   int
	cache *media.ConfigCache
	cc    *astiav.CodecContext
	pkt   *astiav.Packet
	w, h  int

	sws    *astiav.SoftwareScaleContext
	srcW   int
	srcH   int
	srcFmt astiav.PixelFormat

	// cfg comes from the build's first keyframe.
	cfg *media.TrackConfig
}

// evenWidth returns the width that keeps the aspect ratio of srcW × srcH at
// height h, rounded to the nearest even number, at least 2.
func evenWidth(srcW, srcH, h int) int {
	w := (srcW*h + srcH) / (2 * srcH) * 2
	return max(w, 2)
}

func openRung(idx int, r Rung, fr *astiav.Frame, fps astiav.Rational, cache *media.ConfigCache) (*rungEncoder, error) {
	codec := astiav.FindEncoderByName("libx264")
	if codec == nil {
		return nil, errors.New("video encoder: libx264 not in libavcodec")
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, errors.New("video encoder: alloc libx264 context")
	}
	e := &rungEncoder{idx: idx, cache: cache, cc: cc, w: evenWidth(fr.Width(), fr.Height(), r.Height), h: r.Height}
	cc.SetWidth(e.w)
	cc.SetHeight(e.h)
	cc.SetPixelFormat(astiav.PixelFormatYuv420P)
	cc.SetTimeBase(timeBase)
	cc.SetFramerate(fps)
	if sar := fr.SampleAspectRatio(); sar.Num() > 0 && sar.Den() > 0 {
		cc.SetSampleAspectRatio(sar)
	}
	cc.SetBitRate(int64(r.BitrateKbps) * 1000)
	cc.SetRateControlMaxRate(int64(r.MaxBitrateKbps) * 1000)
	cc.SetRateControlBufferSize(2 * r.MaxBitrateKbps * 1000)
	cc.SetMaxBFrames(r.BFrames)
	cc.SetThreadCount(0)

	opts := astiav.NewDictionary()
	defer opts.Free()
	for _, kv := range [][2]string{{"preset", r.Preset}, {"x264-params", x264Params}, {"forced-idr", "1"}} {
		if err := opts.Set(kv[0], kv[1], 0); err != nil {
			cc.Free()
			return nil, fmt.Errorf("video encoder: option %s: %w", kv[0], err)
		}
	}
	if err := cc.Open(codec, opts); err != nil {
		cc.Free()
		return nil, fmt.Errorf("video encoder: rung %d: open libx264 %dx%d: %w", idx, e.w, e.h, err)
	}
	e.pkt = astiav.AllocPacket()
	return e, nil
}

// encode scales one picture and sends it to the encoder, as an IDR when
// force is set and with no picture type otherwise.
func (e *rungEncoder) encode(fr *astiav.Frame, pts int64, force bool, epoch uint32, track int) ([]Output, error) {
	if err := e.ensureScaler(fr); err != nil {
		return nil, err
	}
	dst := astiav.AllocFrame()
	defer dst.Free()
	dst.SetWidth(e.w)
	dst.SetHeight(e.h)
	dst.SetPixelFormat(astiav.PixelFormatYuv420P)
	if err := dst.AllocBuffer(0); err != nil {
		return nil, fmt.Errorf("video scaler: alloc %dx%d: %w", e.w, e.h, err)
	}
	if err := e.sws.ScaleFrame(fr, dst); err != nil {
		return nil, fmt.Errorf("video scaler: scale to %dx%d: %w", e.w, e.h, err)
	}
	dst.SetPts(pts)
	if force {
		dst.SetPictureType(astiav.PictureTypeI)
		dst.SetFlags(dst.Flags().Add(astiav.FrameFlagKey))
	} else {
		dst.SetPictureType(astiav.PictureTypeNone)
		dst.SetFlags(dst.Flags().Del(astiav.FrameFlagKey))
	}
	if err := e.cc.SendFrame(dst); err != nil {
		return nil, fmt.Errorf("video encoder: rung %d: send: %w", e.idx, err)
	}
	return e.receive(epoch, track)
}

// ensureScaler (re)creates the scaler for the picture's format.
func (e *rungEncoder) ensureScaler(fr *astiav.Frame) error {
	w, h, pf := fr.Width(), fr.Height(), fr.PixelFormat()
	if e.sws != nil && w == e.srcW && h == e.srcH && pf == e.srcFmt {
		return nil
	}
	if e.sws != nil {
		e.sws.Free()
		e.sws = nil
	}
	sws, err := astiav.CreateSoftwareScaleContext(w, h, pf, e.w, e.h, astiav.PixelFormatYuv420P,
		astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBicubic))
	if err != nil {
		return fmt.Errorf("video scaler: %dx%d %s to %dx%d: %w", w, h, pf, e.w, e.h, err)
	}
	e.sws, e.srcW, e.srcH, e.srcFmt = sws, w, h, pf
	return nil
}

// flush drains the encoder.
func (e *rungEncoder) flush(epoch uint32, track int) ([]Output, error) {
	if err := e.cc.SendFrame(nil); err != nil {
		return nil, fmt.Errorf("video encoder: rung %d: flush: %w", e.idx, err)
	}
	return e.receive(epoch, track)
}

// receive returns every packet the encoder has ready.
func (e *rungEncoder) receive(epoch uint32, track int) ([]Output, error) {
	var out []Output
	for {
		err := e.cc.ReceivePacket(e.pkt)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return out, nil
		case err != nil:
			return out, fmt.Errorf("video encoder: rung %d: receive: %w", e.idx, err)
		}
		f, err := e.frame(epoch, track)
		e.pkt.Unref()
		if err != nil {
			return out, err
		}
		out = append(out, Output{Rung: e.idx, F: f})
	}
}

// frame turns the current packet into a Frame: its NAL units without AUD,
// SPS and PPS, and the Config of the build's first keyframe.
func (e *rungEncoder) frame(epoch uint32, track int) (media.Frame, error) {
	nals := splitAnnexB(e.pkt.Data())
	key := e.pkt.Flags().Has(astiav.PacketFlagKey)
	if e.cfg == nil {
		if !key {
			return media.Frame{}, fmt.Errorf("video encoder: rung %d: first packet is not a keyframe", e.idx)
		}
		var sps, pps [][]byte
		for _, n := range nals {
			switch h264Type(n) {
			case nalSPS:
				sps = append(sps, n)
			case nalPPS:
				pps = append(pps, n)
			}
		}
		cfg, err := media.NewH264Config(sps, pps)
		if err != nil {
			return media.Frame{}, fmt.Errorf("video encoder: rung %d: %w", e.idx, err)
		}
		e.cfg = e.cache.Stable(0, cfg)
	}
	data := make([][]byte, 0, len(nals))
	for _, n := range nals {
		switch h264Type(n) {
		case nalSPS, nalPPS, nalAUD:
		default:
			data = append(data, n)
		}
	}
	return media.Frame{
		Track:  track,
		Config: e.cfg,
		PTS:    e.pkt.Pts(),
		DTS:    e.pkt.Dts(),
		Key:    key,
		Data:   data,
		Epoch:  epoch,
	}, nil
}

func (e *rungEncoder) release() {
	if e.sws != nil {
		e.sws.Free()
	}
	if e.pkt != nil {
		e.pkt.Free()
	}
	if e.cc != nil {
		e.cc.Free()
	}
	e.sws, e.pkt, e.cc = nil, nil, nil
}
