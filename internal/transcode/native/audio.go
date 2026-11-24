//go:build libav

package native

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// sampleFormat is the AAC encoder's input format, planar float.
const sampleFormat = astiav.SampleFormatFltp

// defaultFrameSize is the AAC frame size in samples.
const defaultFrameSize = 1024

// channelLayout returns the layout of an output channel count.
func channelLayout(n int) (astiav.ChannelLayout, bool) {
	switch n {
	case 1:
		return astiav.ChannelLayoutMono, true
	case 2:
		return astiav.ChannelLayoutStereo, true
	case 6:
		return astiav.ChannelLayout5Point1, true
	case 8:
		return astiav.ChannelLayout7Point1, true
	default:
		return astiav.ChannelLayout{}, false
	}
}

// audioTrack decodes one AAC track, resamples it and encodes it again.
//
// Encoder input PTS are anchor + SampleTime(n × frame size): the anchor is
// the build's first input PTS, and n counts the frames sent since. An input
// more than one frame duration after its predicted PTS re-anchors to it; one
// more than a frame duration early drops input samples until it meets the
// prediction. Smaller deviations are ignored.
type audioTrack struct {
	track  int
	out    Audio
	layout astiav.ChannelLayout
	cache  media.ConfigCache // kept across builds

	// The current build; dec is nil between builds.
	cfg       *media.TrackConfig // input
	dec       *astiav.CodecContext
	decPkt    *astiav.Packet
	decFrm    *astiav.Frame
	swr       *astiav.SoftwareResampleContext
	res       *astiav.Frame
	enc       *astiav.CodecContext
	encPkt    *astiav.Packet
	fifo      *astiav.AudioFifo // samples waiting for a full encoder frame
	stage     *astiav.AudioFifo // one resampled frame while samples are dropped from its start
	frameSize int
	outCfg    *media.TrackConfig

	anchored  bool
	anchor    int64
	consumed  int64 // samples placed on the timeline since the anchor
	sent      int64 // encoder frames since the anchor
	catchUp   bool  // the input is early: drop until it meets the prediction
	primed    bool  // the build's first packet has been seen
	resampled bool  // the resampler has had input
}

func newAudioTrack(track int, out Audio) *audioTrack {
	layout, _ := channelLayout(out.Channels) // checked by New
	return &audioTrack{track: track, out: out, layout: layout}
}

// push feeds one AAC frame.
func (a *audioTrack) push(f media.Frame, epoch uint32, st *Stats) ([]Output, error) {
	var out []Output
	if a.dec != nil && f.Config != a.cfg {
		o, err := a.drain(epoch, st)
		out = o
		if err != nil {
			return out, err
		}
	}
	if a.dec == nil {
		if err := a.build(f.Config); err != nil {
			a.release()
			return out, err
		}
	}
	if err := a.decPkt.FromData(bytes.Join(f.Data, nil)); err != nil {
		return out, fmt.Errorf("audio decoder: packet: %w", err)
	}
	a.decPkt.SetPts(f.PTS)
	a.decPkt.SetDts(f.DTS)
	err := a.dec.SendPacket(a.decPkt)
	a.decPkt.Unref()
	if err != nil && !errors.Is(err, astiav.ErrInvaliddata) {
		return out, fmt.Errorf("audio decoder: track %d: send: %w", a.track, err)
	}
	// A corrupt frame is skipped: the timeline closes the hole.
	o, err := a.receive(epoch, st)
	return append(out, o...), err
}

func (a *audioTrack) build(cfg *media.TrackConfig) error {
	if cfg.Codec != media.CodecAAC {
		return fmt.Errorf("audio decoder: track %d: codec %s is not aac", a.track, cfg.Codec)
	}
	codec := astiav.FindDecoderByName("aac")
	if codec == nil {
		return errors.New("audio decoder: aac not in libavcodec")
	}
	a.cfg = cfg
	a.dec = astiav.AllocCodecContext(codec)
	if a.dec == nil {
		return errors.New("audio decoder: alloc aac context")
	}
	a.dec.SetTimeBase(timeBase)
	if err := a.dec.SetExtraData(cfg.ASC); err != nil {
		return fmt.Errorf("audio decoder: track %d: extradata: %w", a.track, err)
	}
	if err := a.dec.Open(codec, nil); err != nil {
		return fmt.Errorf("audio decoder: track %d: open: %w", a.track, err)
	}

	enc := astiav.FindEncoderByName("aac")
	if enc == nil {
		return errors.New("audio encoder: aac not in libavcodec")
	}
	a.enc = astiav.AllocCodecContext(enc)
	if a.enc == nil {
		return errors.New("audio encoder: alloc aac context")
	}
	a.enc.SetSampleFormat(sampleFormat)
	a.enc.SetSampleRate(a.out.SampleRate)
	a.enc.SetChannelLayout(a.layout)
	a.enc.SetBitRate(int64(a.out.BitrateKbps) * 1000)
	a.enc.SetTimeBase(timeBase)
	a.enc.SetFlags(a.enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	if err := a.enc.Open(enc, nil); err != nil {
		return fmt.Errorf("audio encoder: track %d: open: %w", a.track, err)
	}
	a.frameSize = a.enc.FrameSize()
	if a.frameSize <= 0 {
		a.frameSize = defaultFrameSize
	}
	outCfg, err := media.NewAACConfig(a.enc.ExtraData())
	if err != nil {
		return fmt.Errorf("audio encoder: track %d: %w", a.track, err)
	}
	a.outCfg = a.cache.Stable(0, outCfg)

	a.swr = astiav.AllocSoftwareResampleContext()
	a.fifo = astiav.AllocAudioFifo(sampleFormat, a.out.Channels, a.frameSize)
	a.stage = astiav.AllocAudioFifo(sampleFormat, a.out.Channels, a.frameSize)
	if a.swr == nil || a.fifo == nil || a.stage == nil {
		return errors.New("audio: alloc resampler or fifo")
	}
	a.decPkt, a.decFrm = astiav.AllocPacket(), astiav.AllocFrame()
	a.res, a.encPkt = astiav.AllocFrame(), astiav.AllocPacket()
	a.anchored, a.primed, a.catchUp, a.resampled = false, false, false, false
	return nil
}

// receive places every decoded frame on the timeline and encodes it.
func (a *audioTrack) receive(epoch uint32, st *Stats) ([]Output, error) {
	var out []Output
	for bad := 0; ; {
		err := a.dec.ReceiveFrame(a.decFrm)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return out, nil
		case errors.Is(err, astiav.ErrInvaliddata) && bad < maxBadPictures:
			bad++
			continue
		case err != nil:
			return out, fmt.Errorf("audio decoder: track %d: receive: %w", a.track, err)
		}
		o, err := a.place(a.decFrm, epoch, st)
		a.decFrm.Unref()
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
}

// predicted returns the PTS the next input sample is expected at.
func (a *audioTrack) predicted() int64 {
	return a.anchor + media.SampleTime(a.consumed, a.out.SampleRate)
}

// place resamples one decoded frame into the FIFO, correcting a deviation
// of its PTS from the prediction, and encodes every full frame.
func (a *audioTrack) place(in *astiav.Frame, epoch uint32, st *Stats) ([]Output, error) {
	pts := in.Pts()
	switch {
	case pts == astiav.NoPtsValue && !a.anchored:
		return nil, nil
	case pts == astiav.NoPtsValue:
		pts = a.predicted()
	case !a.anchored:
		if err := a.reanchor(pts); err != nil {
			return nil, err
		}
	}
	dur := media.SampleTime(int64(in.NbSamples()), max(in.SampleRate(), 1))
	dev := pts - a.predicted()
	if dev > dur {
		if err := a.reanchor(pts); err != nil {
			return nil, err
		}
		st.Reanchors++
		dev = 0
	}
	if err := a.resample(in); err != nil {
		return nil, err
	}
	n := a.res.NbSamples()
	drop := 0
	if dev < -dur || a.catchUp {
		k := int(media.Rescale(max(-dev, 0), a.out.SampleRate))
		drop, a.catchUp = min(k, n), k > n
	}
	if err := a.write(a.res, drop); err != nil {
		return nil, err
	}
	st.AudioDropped += uint64(drop) //nolint:gosec // 0 <= drop <= n
	a.consumed += int64(n - drop)
	return a.encode(epoch, false)
}

// reanchor restarts the timeline at pts, discarding the samples that wait
// for a full frame.
func (a *audioTrack) reanchor(pts int64) error {
	a.anchored, a.anchor, a.consumed, a.sent, a.catchUp = true, pts, 0, 0, false
	return a.discard(a.fifo, a.fifo.Size())
}

func (a *audioTrack) resample(in *astiav.Frame) error {
	a.res.Unref()
	a.res.SetSampleFormat(sampleFormat)
	a.res.SetSampleRate(a.out.SampleRate)
	a.res.SetChannelLayout(a.layout)
	// A nil in flushes the resampler's delay.
	if err := a.swr.ConvertFrame(in, a.res); err != nil {
		return fmt.Errorf("audio resampler: track %d: %w", a.track, err)
	}
	a.resampled = true
	return nil
}

// newFrame allocates an encoder-format frame of n samples.
func (a *audioTrack) newFrame(n int) (*astiav.Frame, error) {
	f := astiav.AllocFrame()
	f.SetNbSamples(n)
	f.SetSampleFormat(sampleFormat)
	f.SetSampleRate(a.out.SampleRate)
	f.SetChannelLayout(a.layout)
	if err := f.AllocBuffer(0); err != nil {
		f.Free()
		return nil, fmt.Errorf("audio: alloc frame of %d samples: %w", n, err)
	}
	return f, nil
}

// discard reads n samples out of fifo and drops them.
func (a *audioTrack) discard(fifo *astiav.AudioFifo, n int) error {
	if n <= 0 {
		return nil
	}
	f, err := a.newFrame(n)
	if err != nil {
		return err
	}
	defer f.Free()
	if _, err := fifo.Read(f); err != nil {
		return fmt.Errorf("audio fifo: read: %w", err)
	}
	return nil
}

// write appends f to the FIFO without its first drop samples.
func (a *audioTrack) write(f *astiav.Frame, drop int) error {
	n := f.NbSamples()
	if n <= drop {
		return nil
	}
	if drop == 0 {
		if _, err := a.fifo.Write(f); err != nil {
			return fmt.Errorf("audio fifo: write: %w", err)
		}
		return nil
	}
	if _, err := a.stage.Write(f); err != nil {
		return fmt.Errorf("audio fifo: write: %w", err)
	}
	if err := a.discard(a.stage, drop); err != nil {
		return err
	}
	rest, err := a.newFrame(n - drop)
	if err != nil {
		return err
	}
	defer rest.Free()
	if _, err := a.stage.Read(rest); err != nil {
		return fmt.Errorf("audio fifo: read: %w", err)
	}
	if _, err := a.fifo.Write(rest); err != nil {
		return fmt.Errorf("audio fifo: write: %w", err)
	}
	return nil
}

// encode sends every full frame of the FIFO to the encoder, and with final
// the short rest too.
func (a *audioTrack) encode(epoch uint32, final bool) ([]Output, error) {
	var out []Output
	for a.fifo.Size() >= a.frameSize || (final && a.fifo.Size() > 0) {
		f, err := a.newFrame(min(a.fifo.Size(), a.frameSize))
		if err != nil {
			return out, err
		}
		if _, err := a.fifo.Read(f); err != nil {
			f.Free()
			return out, fmt.Errorf("audio fifo: read: %w", err)
		}
		f.SetPts(a.anchor + media.SampleTime(a.sent*int64(a.frameSize), a.out.SampleRate))
		a.sent++
		err = a.enc.SendFrame(f)
		f.Free()
		if err != nil {
			return out, fmt.Errorf("audio encoder: track %d: send: %w", a.track, err)
		}
		o, err := a.receivePackets(epoch)
		out = append(out, o...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// receivePackets returns every packet the encoder has ready. The build's
// first packet is the encoder's priming when it starts before the anchor,
// and is dropped.
func (a *audioTrack) receivePackets(epoch uint32) ([]Output, error) {
	var out []Output
	for {
		err := a.enc.ReceivePacket(a.encPkt)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return out, nil
		case err != nil:
			return out, fmt.Errorf("audio encoder: track %d: receive: %w", a.track, err)
		}
		pts := a.encPkt.Pts()
		first := !a.primed
		a.primed = true
		if first && pts < a.anchor {
			a.encPkt.Unref()
			continue
		}
		out = append(out, Output{Rung: -1, F: media.Frame{
			Track:  a.track,
			Config: a.outCfg,
			PTS:    pts,
			DTS:    pts,
			Key:    true,
			Data:   [][]byte{a.encPkt.Data()},
			Epoch:  epoch,
		}})
		a.encPkt.Unref()
	}
}

// drain flushes the decoder, the resampler, the FIFO and the encoder, then
// releases the build.
func (a *audioTrack) drain(epoch uint32, st *Stats) ([]Output, error) {
	if a.dec == nil {
		return nil, nil
	}
	defer a.release()
	if err := a.dec.SendPacket(nil); err != nil {
		return nil, fmt.Errorf("audio decoder: track %d: flush: %w", a.track, err)
	}
	out, err := a.receive(epoch, st)
	if err != nil {
		return out, err
	}
	if a.resampled && a.anchored {
		if err := a.resample(nil); err != nil {
			return out, err
		}
		if n := a.res.NbSamples(); n > 0 {
			if err := a.write(a.res, 0); err != nil {
				return out, err
			}
			a.consumed += int64(n)
		}
	}
	o, err := a.encode(epoch, true)
	out = append(out, o...)
	if err != nil {
		return out, err
	}
	if err := a.enc.SendFrame(nil); err != nil {
		return out, fmt.Errorf("audio encoder: track %d: flush: %w", a.track, err)
	}
	o, err = a.receivePackets(epoch)
	return append(out, o...), err
}

// release frees the build without draining it.
func (a *audioTrack) release() {
	for _, cc := range []*astiav.CodecContext{a.dec, a.enc} {
		if cc != nil {
			cc.Free()
		}
	}
	for _, p := range []*astiav.Packet{a.decPkt, a.encPkt} {
		if p != nil {
			p.Free()
		}
	}
	for _, f := range []*astiav.Frame{a.decFrm, a.res} {
		if f != nil {
			f.Free()
		}
	}
	for _, q := range []*astiav.AudioFifo{a.fifo, a.stage} {
		if q != nil {
			q.Free()
		}
	}
	if a.swr != nil {
		a.swr.Free()
	}
	a.cfg, a.dec, a.enc, a.decPkt, a.encPkt = nil, nil, nil, nil, nil
	a.decFrm, a.res, a.fifo, a.stage, a.swr = nil, nil, nil, nil, nil
}
