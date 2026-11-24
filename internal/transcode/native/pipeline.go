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

	"github.com/ntt0601zcoder/open-streamer/internal/media"
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
// about 6 fps with 2; slower sources should use −1. Preset is the libx264
// preset, "veryfast" when empty.
type Rung struct {
	Height, BitrateKbps, MaxBitrateKbps, BFrames int
	Preset                                       string
}

// Audio is the AAC encoding of every audio track. Zero values mean 128 kbps,
// 48000 Hz and 2 channels. Channels must be 1, 2, 6 or 8.
type Audio struct{ BitrateKbps, SampleRate, Channels int }

// Output is one encoded frame. Rung is the index of the video rung, or −1
// for audio. F.Track is the source track the frame was made from.
type Output struct {
	Rung int
	F    media.Frame
}

// Stats counts what the pipeline dropped or corrected.
//
// DroppedTracks is the number of video tracks that are not encoded (only the
// lowest-numbered video track is). Reanchors counts audio re-anchors after a
// forward gap, and AudioDropped the audio samples (at the output rate)
// dropped because the input ran early.
type Stats struct{ DroppedTracks, Reanchors, AudioDropped uint64 }

// Pipeline decodes the source video once, encodes it per rung with libx264,
// and re-encodes every audio track to AAC. Its input is already in rendition
// time: epoch-mapped and shifted by D. Output PTS are the input's; video
// output DTS are the encoder's. It is not safe for concurrent use.
type Pipeline struct {
	video  *videoPath // nil without rungs
	audio  *Audio     // resolved; nil when audio is ignored
	audios map[int]*audioTrack

	started bool
	epoch   uint32
	closed  bool

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

// New builds a pipeline for the given ladder. A nil audio ignores audio
// frames; at least one rung or audio is required.
func New(rungs []Rung, audio *Audio) (*Pipeline, error) {
	if len(rungs) == 0 && audio == nil {
		return nil, errors.New("transcode: new pipeline: no rungs and no audio")
	}
	specs := make([]Rung, len(rungs))
	for i, r := range rungs {
		s, err := resolveRung(r)
		if err != nil {
			return nil, fmt.Errorf("transcode: new pipeline: rung %d: %w", i, err)
		}
		specs[i] = s
	}
	p := &Pipeline{audios: map[int]*audioTrack{}, vtrack: -1, dropped: map[int]bool{}}
	if len(specs) > 0 {
		p.video = newVideoPath(specs)
	}
	if audio != nil {
		a, err := resolveAudio(*audio)
		if err != nil {
			return nil, fmt.Errorf("transcode: new pipeline: audio: %w", err)
		}
		p.audio = &a
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
	if r.Preset == "" {
		r.Preset = defaultPreset
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
func (p *Pipeline) Push(f media.Frame) ([]Output, error) {
	if p.closed {
		return nil, errors.New("transcode: push: pipeline closed")
	}
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
	out = append(out, o...)
	if err != nil {
		return out, fmt.Errorf("transcode: push: %w", err)
	}
	return out, nil
}

func (p *Pipeline) pushVideo(f media.Frame) ([]Output, error) {
	if p.video == nil {
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

func (p *Pipeline) dropTrack(track int) {
	if p.dropped[track] {
		return
	}
	p.dropped[track] = true
	p.stats.DroppedTracks++
	slog.Warn("transcode: dropping a video track: only the lowest-numbered video track is encoded",
		"track", track, "encoded", p.vtrack)
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
	if p.closed {
		return nil, errors.New("transcode: discontinuity: pipeline closed")
	}
	out, err := p.drain()
	if err != nil {
		return out, fmt.Errorf("transcode: discontinuity: %w", err)
	}
	p.awaitKey = p.sawVideo && p.video != nil
	p.gated = false
	return out, nil
}

// Flush drains the decoders and every encoder at the end of the input. The
// pipeline can take frames again afterwards, starting at a key.
func (p *Pipeline) Flush() ([]Output, error) {
	if p.closed {
		return nil, errors.New("transcode: flush: pipeline closed")
	}
	out, err := p.drain()
	if err != nil {
		return out, fmt.Errorf("transcode: flush: %w", err)
	}
	p.awaitKey, p.gated = false, false
	return out, nil
}

// Stats returns the counters.
func (p *Pipeline) Stats() Stats { return p.stats }

// Close releases every libav resource without draining. The pipeline is
// unusable afterwards.
func (p *Pipeline) Close() {
	if p.closed {
		return
	}
	p.closed = true
	if p.video != nil {
		p.video.release()
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
