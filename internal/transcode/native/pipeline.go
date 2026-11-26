//go:build libav

package native

// #cgo pkg-config: libavcodec libavutil libswscale libswresample
// #include <libavcodec/avcodec.h>
// #include <libavutil/avutil.h>
// #include <libswscale/swscale.h>
// #include <libswresample/swresample.h>
import "C"

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// Defaults of Rung and Audio.
const (
	defaultBFrames      = 2
	maxBFrames          = 3
	defaultPreset       = "veryfast"
	defaultAudioBitrate = 128
	defaultSampleRate   = 48000
	defaultChannels     = 2
)

// Rung is one rendition of the video ladder.
//
// Height is the output height; the width keeps the source's aspect ratio,
// rounded to an even number. BitrateKbps is the target bitrate and
// MaxBitrateKbps the peak (0 means BitrateKbps); the VBV buffer is twice the
// peak. BFrames is the number of consecutive B-frames: 0 means 2 and −1
// means none. At most 3 are allowed: the rendition delay D (0.5 s) covers
// the encoder's reorder delay only down to about 8 fps with 3 B-frames, or
// about 6 fps with 2; slower sources should use −1. Encoder is EncoderX264
// (libx264, the default when empty) or EncoderNVENC (h264_nvenc). Preset is
// the libx264 preset, "veryfast" when empty, or for NVENC p1 to p7, "p4"
// when empty. An NVENC rung whose encoder fails to open is encoded by
// libx264 with preset veryfast, and counted in Stats.EncoderFallbacks,
// unless NoFallback is set: the build then fails with a *RefusedError, and
// no rung is opened. NoFallback does nothing on a libx264 rung.
type Rung struct {
	Height, BitrateKbps, MaxBitrateKbps, BFrames int
	Preset, Encoder                              string
	NoFallback                                   bool
}

// Config is what a pipeline encodes: the video ladder and the audio. A nil
// Audio ignores audio frames; at least one rung, Audio or Thumbnail is
// required.
//
// Watermark is drawn on every decoded picture before the ladder, so every
// rung carries it; it needs at least one rung. Its Font, when set, is kept
// by the pipeline and must not change; the caller may drop its reference
// once NewWith returns. Now is the clock of a text watermark's time
// directives; nil means time.Now.
//
// Thumbnail asks for JPEG shots, taken from the decoded picture before the
// watermark. With rungs the pipeline shoots a picture every IntervalSec of
// stream time and at the first picture of each epoch. Without rungs it is
// key-only: it keeps no schedule and shoots every key frame of the
// lowest-numbered video track it is given, decoded alone, and drops the
// rest of the video. A key that gives no picture counts in
// Stats.ThumbnailsFailed; a damaged key the decoder conceals still gives a
// shot.
type Config struct {
	Rungs     []Rung
	Audio     *Audio
	Watermark *picture.Watermark
	Thumbnail *picture.Thumbnail
	Now       func() time.Time
}

// Audio is the AAC encoding of every audio track. Zero values mean 128 kbps,
// 48000 Hz and 2 channels. Channels must be 1, 2, 6 or 8.
type Audio struct{ BitrateKbps, SampleRate, Channels int }

// Output is one encoded frame or one thumbnail. Rung is the index of the
// video rung, −1 for audio, or RungShot for a thumbnail. F.Track is the
// source track the frame was made from. Shot is non-nil exactly when Rung
// is RungShot, and F is then zero.
type Output struct {
	Rung int
	F    media.Frame
	Shot *picture.Shot
}

// Stats counts what the pipeline dropped or corrected.
//
// DroppedTracks is the number of video tracks that are not encoded, or in
// a key-only pipeline not shot (only the lowest-numbered video track is).
// Reanchors counts audio re-anchors after a forward gap, and AudioDropped
// the audio samples (at the output rate) dropped because the input ran
// early. AudioCorrupt and VideoCorrupt count the input frames a decoder
// rejected and that were skipped: any decoder error but ENOMEM. Damage a video decoder conceals is not counted. A
// decoder that rejects 50 frames in a row fails the pipeline.
// EncoderFallbacks counts the rung encoder opens that fell back from NVENC
// to libx264: one per rung per build. Thumbnails counts the shots made and
// ThumbnailsFailed the attempts that gave none; a failed shot never fails
// the pipeline and never counts in VideoCorrupt. A key-only attempt fails
// only when the key gives no picture at all: the decoder outputs damaged
// pictures (SHOW_ALL, OUTPUT_CORRUPT), so a key it conceals still gives a
// shot, of the concealed picture.
type Stats struct {
	DroppedTracks, Reanchors, AudioDropped uint64
	AudioCorrupt, VideoCorrupt             uint64
	EncoderFallbacks                       uint64
	Thumbnails, ThumbnailsFailed           uint64
}

// Pipeline decodes the source video once, takes thumbnails from it when
// asked, draws the watermark on it when one is set, encodes it per rung
// with libx264 or NVENC, and re-encodes every audio track to AAC. Its
// input is already in rendition time: epoch-mapped and shifted by D.
// Output PTS are the input's; video output DTS are the encoder's. It is not
// safe for concurrent use.
type Pipeline struct {
	video  *videoPath // nil without rungs
	audio  *Audio     // resolved; nil when audio is ignored
	audios map[int]*audioTrack
	// thumb makes the thumbnails; nil without. Without rungs it shoots
	// key frames alone.
	thumb *thumbnailer

	started bool
	epoch   uint32
	closed  bool
	// err is the first error the pipeline returned; every later call
	// returns it.
	err error

	// vtrack is the encoded video track, or −1 before the first video frame.
	vtrack  int
	dropped map[int]bool
	// sawVideo: the current epoch has video, so a Discontinuity resumes
	// at the next video key rather than at the next frame.
	sawVideo bool
	// awaitKey: after a Discontinuity, every frame is dropped until the
	// next key of the video track. gated, gatePTS: then audio presented
	// before that key is dropped, until the epoch ends.
	awaitKey bool
	gated    bool
	gatePTS  int64

	stats Stats
}

// New builds a pipeline for the given ladder and audio, as NewWith.
func New(rungs []Rung, audio *Audio) (*Pipeline, error) {
	return NewWith(Config{Rungs: rungs, Audio: audio})
}

// NewWith builds a pipeline for c.
func NewWith(c Config) (*Pipeline, error) {
	rungs, audio := c.Rungs, c.Audio
	if len(rungs) == 0 && audio == nil && c.Thumbnail == nil {
		return nil, errors.New("transcode: new pipeline: no rungs, no audio and no thumbnail")
	}
	specs := make([]Rung, len(rungs))
	for i, r := range rungs {
		s, err := resolveRung(r)
		if err != nil {
			return nil, fmt.Errorf("transcode: new pipeline: rung %d: %w", i, err)
		}
		specs[i] = s
	}
	if c.Watermark != nil && len(specs) == 0 {
		return nil, errors.New("transcode: new pipeline: a watermark needs video rungs")
	}
	p := &Pipeline{audios: map[int]*audioTrack{}, vtrack: -1, dropped: map[int]bool{}}
	if t := c.Thumbnail; t != nil {
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("transcode: new pipeline: %w", err)
		}
		p.thumb = newThumbnailer(t.Resolved(), &p.stats, len(specs) > 0)
	}
	if audio != nil {
		a, err := resolveAudio(*audio)
		if err != nil {
			return nil, fmt.Errorf("transcode: new pipeline: audio: %w", err)
		}
		p.audio = &a
	}
	if len(specs) > 0 {
		wm := newWatermarker(c.Now)
		if err := wm.set(c.Watermark); err != nil {
			return nil, fmt.Errorf("transcode: new pipeline: %w", err)
		}
		p.video = newVideoPath(specs, &p.stats, wm, p.thumb)
	}
	return p, nil
}

func resolveRung(r Rung) (Rung, error) {
	switch {
	case r.Height <= 0 || r.Height%2 != 0:
		return r, fmt.Errorf("height %d is not a positive even number", r.Height)
	case r.BitrateKbps <= 0:
		return r, fmt.Errorf("bitrate %d kbps is not positive", r.BitrateKbps)
	case r.MaxBitrateKbps < 0:
		return r, fmt.Errorf("max bitrate %d kbps is negative", r.MaxBitrateKbps)
	case r.BFrames < -1 || r.BFrames > maxBFrames:
		return r, fmt.Errorf("b-frames %d is outside [-1, %d]", r.BFrames, maxBFrames)
	}
	if r.MaxBitrateKbps == 0 {
		r.MaxBitrateKbps = r.BitrateKbps
	}
	switch r.BFrames {
	case 0:
		r.BFrames = defaultBFrames
	case -1:
		r.BFrames = 0
	}
	switch r.Encoder {
	case "", EncoderX264:
		r.Encoder = EncoderX264
		if r.Preset == "" {
			r.Preset = defaultPreset
		}
	case EncoderNVENC:
		if r.Preset == "" {
			r.Preset = defaultNVENCPreset
		}
		if !validNVENCPreset(r.Preset) {
			return r, fmt.Errorf("nvenc preset %q is not p1 to p7", r.Preset)
		}
	default:
		return r, fmt.Errorf("encoder %q is not x264 or nvenc", r.Encoder)
	}
	return r, nil
}

func resolveAudio(a Audio) (Audio, error) {
	if a.BitrateKbps == 0 {
		a.BitrateKbps = defaultAudioBitrate
	}
	if a.SampleRate == 0 {
		a.SampleRate = defaultSampleRate
	}
	if a.Channels == 0 {
		a.Channels = defaultChannels
	}
	switch {
	case a.BitrateKbps < 0:
		return a, fmt.Errorf("bitrate %d kbps is negative", a.BitrateKbps)
	case a.SampleRate < 0:
		return a, fmt.Errorf("sample rate %d is negative", a.SampleRate)
	}
	if _, ok := channelLayout(a.Channels); !ok {
		return a, fmt.Errorf("unsupported channel count %d", a.Channels)
	}
	return a, nil
}

// Push feeds one frame and returns the outputs it completed. The frame is
// already in rendition time, epoch and shift. A new epoch first drains the
// decoder and every encoder, whose outputs keep the old epoch.
//
// An error from Push, Discontinuity or Flush is permanent: the pipeline
// releases its codecs and every later call returns the same error.
func (p *Pipeline) Push(f media.Frame) ([]Output, error) {
	if err := p.usable("push"); err != nil {
		return nil, err
	}
	out, err := p.push(f)
	if err != nil {
		return out, p.fail(fmt.Errorf("transcode: push: %w", err))
	}
	return out, nil
}

// usable returns the error that stops a call to op, if any.
func (p *Pipeline) usable(op string) error {
	switch {
	case p.closed:
		return fmt.Errorf("transcode: %s: pipeline closed", op)
	case p.err != nil:
		return p.err
	}
	return nil
}

// fail makes err permanent and releases every codec.
func (p *Pipeline) fail(err error) error {
	p.err = err
	p.release()
	return err
}

func (p *Pipeline) push(f media.Frame) ([]Output, error) {
	var out []Output
	switch {
	case !p.started:
		p.started, p.epoch = true, f.Epoch
	case f.Epoch != p.epoch:
		o, err := p.drain()
		out = o
		if err != nil {
			return out, err
		}
		p.epoch = f.Epoch
		p.sawVideo, p.awaitKey, p.gated = false, false, false
	}
	var (
		o   []Output
		err error
	)
	switch f.Kind() {
	case media.KindVideo:
		o, err = p.pushVideo(f)
	case media.KindAudio:
		o, err = p.pushAudio(f)
	}
	return append(out, o...), err
}

func (p *Pipeline) pushVideo(f media.Frame) ([]Output, error) {
	if p.video == nil {
		if p.thumb != nil {
			return p.shootKey(f), nil
		}
		return nil, nil
	}
	var out []Output
	switch {
	case p.vtrack < 0:
		p.vtrack = f.Track
	case f.Track > p.vtrack:
		p.dropTrack(f.Track)
		return nil, nil
	case f.Track < p.vtrack:
		// A lower-numbered video track takes over at its next key.
		o, err := p.video.drain(p.epoch)
		out = o
		if err != nil {
			return out, err
		}
		p.dropTrack(p.vtrack)
		p.vtrack = f.Track
	}
	p.sawVideo = true
	if p.awaitKey {
		if !f.Key {
			return out, nil
		}
		p.awaitKey, p.gated, p.gatePTS = false, true, f.PTS
	}
	o, err := p.video.push(f, p.epoch)
	return append(out, o...), err
}

// shootKey takes a shot from a key frame of the lowest-numbered video
// track, for a pipeline without rungs, and drops every other video frame.
// A higher-numbered track is dropped and counted as with rungs. It leaves
// the epoch's video and gate state alone: audio goes on as in an
// audio-only pipeline. A failed shot is counted, never returned.
func (p *Pipeline) shootKey(f media.Frame) []Output {
	switch {
	case p.vtrack < 0:
		p.vtrack = f.Track
	case f.Track > p.vtrack:
		p.dropTrack(f.Track)
		return nil
	case f.Track < p.vtrack:
		p.dropTrack(p.vtrack)
		p.vtrack = f.Track
	}
	if !f.Key {
		return nil
	}
	if o, ok := p.thumb.fromKey(f); ok {
		return []Output{o}
	}
	return nil
}

// dropTrack drops a video track that is not the lowest-numbered one, which
// alone is encoded, or in a key-only pipeline shot.
func (p *Pipeline) dropTrack(track int) {
	if p.dropped[track] {
		return
	}
	p.dropped[track] = true
	p.stats.DroppedTracks++
	use := "encoded"
	if p.video == nil {
		use = "shot"
	}
	slog.Warn("transcode: dropping a video track: only the lowest-numbered video track is "+use,
		"track", track, "kept", p.vtrack)
}

func (p *Pipeline) pushAudio(f media.Frame) ([]Output, error) {
	if p.audio == nil || p.awaitKey || (p.gated && f.PTS < p.gatePTS) {
		return nil, nil
	}
	a := p.audios[f.Track]
	if a == nil {
		a = newAudioTrack(f.Track, *p.audio)
		p.audios[f.Track] = a
	}
	return a.push(f, p.epoch, &p.stats)
}

// drain flushes the decoders and every encoder in the current epoch and
// releases their libav state; the next frames rebuild them.
func (p *Pipeline) drain() ([]Output, error) {
	var out []Output
	if p.video != nil {
		o, err := p.video.drain(p.epoch)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	for _, a := range p.sortedAudio() {
		o, err := a.drain(p.epoch, &p.stats)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// sortedAudio returns the audio tracks by track index, so outputs come in a
// stable order.
func (p *Pipeline) sortedAudio() []*audioTrack {
	out := make([]*audioTrack, 0, len(p.audios))
	for _, t := range slices.Sorted(maps.Keys(p.audios)) {
		out = append(out, p.audios[t])
	}
	return out
}

// Discontinuity drains the decoders and every encoder, for a gap in the
// input within the epoch. Output resumes at the next key of the video
// track, dropping audio until then and audio presented before it; when the
// epoch has no video, it resumes at the next frame.
func (p *Pipeline) Discontinuity() ([]Output, error) {
	if err := p.usable("discontinuity"); err != nil {
		return nil, err
	}
	out, err := p.drain()
	if err != nil {
		return out, p.fail(fmt.Errorf("transcode: discontinuity: %w", err))
	}
	p.awaitKey = p.sawVideo && p.video != nil
	p.gated = false
	return out, nil
}

// Flush drains the decoders and every encoder at the end of the input. The
// pipeline can take frames again afterwards, starting at a key.
func (p *Pipeline) Flush() ([]Output, error) {
	if err := p.usable("flush"); err != nil {
		return nil, err
	}
	out, err := p.drain()
	if err != nil {
		return out, p.fail(fmt.Errorf("transcode: flush: %w", err))
	}
	p.awaitKey, p.gated = false, false
	return out, nil
}

// SetWatermark replaces the watermark from the next decoded picture on; nil
// removes it. Its Font is kept as in NewWith. The encoders, timestamps and
// epoch are untouched. An invalid
// watermark, or one on a pipeline without rungs, is an error that leaves the
// pipeline as it was. A watermark that then fails to build at the next
// picture fails the pipeline.
func (p *Pipeline) SetWatermark(w *picture.Watermark) error {
	if err := p.usable("set watermark"); err != nil {
		return err
	}
	if p.video == nil {
		if w == nil {
			return nil
		}
		return errors.New("transcode: set watermark: a watermark needs video rungs")
	}
	if err := p.video.wm.set(w); err != nil {
		return fmt.Errorf("transcode: set watermark: %w", err)
	}
	return nil
}

// Stats returns the counters.
func (p *Pipeline) Stats() Stats { return p.stats }

// Close releases every libav resource without draining and removes the
// watermark's temp directory. The pipeline is unusable afterwards.
func (p *Pipeline) Close() {
	if p.closed {
		return
	}
	p.closed = true
	p.release()
}

// release frees every codec without draining.
func (p *Pipeline) release() {
	if p.video != nil {
		p.video.release()
		p.video.wm.close()
	}
	if p.thumb != nil {
		p.thumb.release()
	}
	for _, a := range p.audios {
		a.release()
	}
}

// Versions returns the versions of the linked libav libraries, starting with
// "libavcodec <major.minor.micro>".
func Versions() string {
	v := func(x C.uint) string { return fmt.Sprintf("%d.%d.%d", x>>16, (x>>8)&0xff, x&0xff) }
	return fmt.Sprintf("libavcodec %s libavutil %s libswscale %s libswresample %s ffmpeg %s",
		v(C.avcodec_version()), v(C.avutil_version()), v(C.swscale_version()),
		v(C.swresample_version()), C.GoString(C.av_version_info()))
}
