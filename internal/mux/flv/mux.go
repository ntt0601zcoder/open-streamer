package flv

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// ErrCodecChanged is returned when an epoch changes a carried track's codec
// or a kind (video or audio) appears that the connection does not carry. The
// connection cannot continue: players reconnect, a pusher republishes.
var ErrCodecChanged = errors.New("flv: codec changed")

// onMetaData codec ids: legacy AVC, the 'hvc1' FourCC of enhanced RTMP, AAC.
const (
	metaAVC  = 7
	metaHEVC = 0x68766331 // 'hvc1' = 1752589105
	metaAAC  = 10
)

// The start hold: a connection that starts on a video keyframe holds its
// frames until the first audio frame shows that audio is carried, so the
// opening onMetaData lists every carried track. Without audio it ends after
// holdSpan of video decode time or holdFrames held frames.
const (
	holdSpan   = media.ClockRate
	holdFrames = 256
)

// MuxStats counts what a Muxer dropped or skipped.
type MuxStats struct {
	// SkippedTracks counts tracks never sent or no longer sent: extra tracks
	// of a kind already carried, and tracks whose sequence header could not
	// be built.
	SkippedTracks uint64
	// DroppedBeforeKey counts frames dropped while waiting for a video
	// keyframe (connection start, Resync, video at an epoch start).
	DroppedBeforeKey uint64
	// DroppedBeforeBase counts frames whose DTS is below the connection base.
	DroppedBeforeBase uint64
	// DroppedEarlyAudio counts audio frames presented before the keyframe
	// the connection started or resumed at.
	DroppedEarlyAudio uint64
	// Resyncs counts Resync calls.
	Resyncs uint64
	// Epochs counts epoch changes after the connection start.
	Epochs uint64
}

// state is where a Muxer is in its connection.
type state uint8

const (
	stateIdle    state = iota // nothing sent yet
	stateHold                 // started on a video keyframe; holding until the carried set is known
	stateRun                  // sending
	stateWaitKey              // after Resync: dropping until the resume point
)

// maxKeyWait is how long, in stream time from the first frame dropped after
// a Resync, a connection carrying video waits for a video keyframe before it
// resumes on audio: the video track has ended (hub.LiveWindow), e.g. after a
// failover to an audio-only source.
const maxKeyWait = hub.LiveWindow

// track is one carried track of a connection.
type track struct {
	id     int
	codec  media.Codec
	cfg    *media.TrackConfig // latest seen, for onMetaData
	hdrCfg *media.TrackConfig // config of the last header sent
	hdrGen uint64             // onMetaData generation of the last header sent
	failed bool               // its header could not be built: skipped
	// needKey drops video until a keyframe (at an epoch start).
	needKey bool
}

func (t *track) active() bool { return t != nil && !t.failed }

// Muxer turns the hub frames of one RTMP connection (a player or a push) into
// RTMP media messages. It is not safe for concurrent use.
//
// The first video track and the first audio track a connection sees are
// carried; frames of other tracks are skipped. The connection base b is the
// DTS of the first frame sent: a video keyframe, or the first audio frame of
// an audio-only connection (one whose first frame is audio). Timestamps are
// floor((DTS−b)/90) ms, wrapping at 2^32, and the composition time is
// floor((PTS−b)/90) − floor((DTS−b)/90); frames with a DTS below b are dropped.
//
// One onMetaData listing the carried tracks is sent at the start, after every
// Resync and at the first frame of every new epoch, and each carried track's
// sequence header just before its next frame, built from that frame's Config.
// The carried set is final once the start hold ends (or at once for an
// audio-only connection); a kind appearing after that is ErrCodecChanged.
//
// A track whose sequence header cannot be built is skipped for the rest of the
// connection and counted in SkippedTracks; the connection goes on with the
// other kind (a connection whose video fails at the start is audio-only). An
// onMetaData already sent is not withdrawn.
type Muxer struct {
	st           state
	base         int64
	keyPTS       int64 // PTS of the start or resume keyframe
	waiting      bool  // a frame was dropped in stateWaitKey
	waitFrom     int64 // its DTS
	epoch        uint32
	gen          uint64 // incremented with every onMetaData
	metaDue      bool
	closed       bool // the carried set is final
	video, audio *track
	skipped      map[int]bool
	held         []media.Frame
	stats        MuxStats
}

// NewMuxer returns a Muxer for one connection.
func NewMuxer() *Muxer {
	return &Muxer{skipped: map[int]bool{}}
}

// Stats returns the counters.
func (m *Muxer) Stats() MuxStats { return m.stats }

// Resync restarts the output after frames were lost: frames are dropped until
// the next video keyframe (the next audio frame on a connection without
// video), which gets onMetaData and every header again, and audio presented
// before that keyframe is dropped. The base is kept. When no video keyframe
// comes within hub.LiveWindow of stream time after the first frame dropped,
// the video track has ended: the output resumes at the next audio frame,
// and video at its next keyframe, with its header.
func (m *Muxer) Resync() {
	m.stats.Resyncs++
	switch m.st {
	case stateIdle:
	case stateHold:
		// Nothing was sent: start again at the next keyframe.
		m.held = nil
		m.st = stateIdle
	case stateRun:
		m.st, m.waiting = stateWaitKey, false
	case stateWaitKey:
		// Keep waiting since the first frame dropped.
	}
}

// Frame returns the RTMP messages for one hub frame, onMetaData and sequence
// headers first when due. During the start hold it returns nothing, and then
// the messages of every held frame at once. ErrCodecChanged is returned when
// a carried track's codec changes or a kind appears that is not carried.
func (m *Muxer) Frame(f media.Frame) ([]rtmp.Media, error) {
	if f.Config == nil {
		return nil, errors.New("flv: frame: no track config")
	}
	if m.skipped[f.Track] {
		return nil, nil
	}
	kind := f.Kind()
	slot := m.slot(kind)
	if slot == nil {
		return nil, fmt.Errorf("flv: frame: codec %v: %w", f.Config.Codec, rtmp.ErrUnsupportedCodec)
	}
	t := *slot
	if t == nil {
		if m.closed {
			return nil, fmt.Errorf("flv: frame: %v track appeared: %w", f.Config.Codec, ErrCodecChanged)
		}
		t = &track{id: f.Track, codec: f.Config.Codec, cfg: f.Config}
		*slot = t
		if m.tryHeader(t, f.Config) == nil {
			if m.st == stateHold {
				return m.release()
			}
			return nil, nil
		}
	}
	if t.id != f.Track {
		m.skip(f.Track)
		return nil, nil
	}
	if f.Config.Codec != t.codec {
		return nil, fmt.Errorf("flv: frame: %v to %v: %w", t.codec, f.Config.Codec, ErrCodecChanged)
	}
	t.cfg = f.Config

	switch m.st {
	case stateIdle:
		return m.start(t, f)
	case stateHold:
		if kind == media.KindVideo && f.Epoch == m.epoch &&
			f.DTS-m.base < holdSpan && len(m.held) < holdFrames {
			m.held = append(m.held, f)
			return nil, nil
		}
		out, err := m.release()
		if err != nil {
			return nil, err
		}
		more, err := m.run(t, f)
		if err != nil {
			return nil, err
		}
		return append(out, more...), nil
	case stateWaitKey:
		if f.DTS < m.base {
			m.stats.DroppedBeforeBase++
			return nil, nil
		}
		switch {
		case !m.video.active():
			// An audio-only connection resumes at its next frame.
			m.st, m.metaDue = stateRun, true
			return m.run(t, f)
		case kind == media.KindVideo && f.Key:
			t.needKey = false
		case kind == media.KindAudio && m.waiting && f.DTS-m.waitFrom >= maxKeyWait:
			// The video track ended: resume on audio, video at its next
			// keyframe.
			m.video.needKey = true
		default:
			if !m.waiting {
				m.waiting, m.waitFrom = true, f.DTS
			}
			m.stats.DroppedBeforeKey++
			return nil, nil
		}
		m.keyPTS = f.PTS
		m.st, m.metaDue, m.waiting = stateRun, true, false
		return m.run(t, f)
	case stateRun:
		return m.run(t, f)
	}
	return nil, nil
}

// slot returns the carried-track slot of kind k, or nil for an unknown kind.
func (m *Muxer) slot(k media.Kind) **track {
	switch k {
	case media.KindVideo:
		return &m.video
	case media.KindAudio:
		return &m.audio
	}
	return nil
}

func (m *Muxer) skip(id int) {
	if !m.skipped[id] {
		m.skipped[id] = true
		m.stats.SkippedTracks++
	}
}

func (m *Muxer) fail(t *track) {
	t.failed = true
	m.skip(t.id)
}

// tryHeader returns the sequence header for cfg, or nil after skipping t when
// it cannot be built.
func (m *Muxer) tryHeader(t *track, cfg *media.TrackConfig) []byte {
	b, err := header(cfg)
	if err != nil {
		m.fail(t)
		return nil
	}
	return b
}

// start begins the connection at f: a video keyframe starts the hold, an
// audio frame an audio-only connection unless a video keyframe is awaited.
func (m *Muxer) start(t *track, f media.Frame) ([]rtmp.Media, error) {
	if f.Kind() == media.KindVideo {
		if !f.Key {
			m.stats.DroppedBeforeKey++
			return nil, nil
		}
		m.base, m.keyPTS, m.epoch = f.DTS, f.PTS, f.Epoch
		m.st = stateHold
		m.held = append(m.held[:0], f)
		return nil, nil
	}
	if m.video.active() {
		m.stats.DroppedBeforeKey++
		return nil, nil
	}
	m.base, m.keyPTS, m.epoch = f.DTS, f.PTS, f.Epoch
	m.st, m.closed, m.metaDue = stateRun, true, true
	return m.run(t, f)
}

// release ends the hold: the carried set becomes final and the held video
// frames are sent after onMetaData.
func (m *Muxer) release() ([]rtmp.Media, error) {
	held := m.held
	m.held = nil
	m.st, m.closed, m.metaDue = stateRun, true, true
	var out []rtmp.Media
	for _, f := range held {
		if !m.video.active() {
			break
		}
		msgs, err := m.run(m.video, f)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// run sends f, a frame of carried track t, on a running connection.
func (m *Muxer) run(t *track, f media.Frame) ([]rtmp.Media, error) {
	kind := f.Kind()
	if kind == media.KindAudio && f.PTS < m.keyPTS {
		m.stats.DroppedEarlyAudio++
		return nil, nil
	}
	if f.DTS < m.base {
		m.stats.DroppedBeforeBase++
		return nil, nil
	}
	if f.Epoch != m.epoch {
		m.epoch = f.Epoch
		m.metaDue = true
		m.stats.Epochs++
		if m.video.active() {
			m.video.needKey = true
		}
	}
	if kind == media.KindVideo && t.needKey {
		if !f.Key {
			m.stats.DroppedBeforeKey++
			return nil, nil
		}
		t.needKey = false
	}

	gen := m.gen
	if m.metaDue {
		gen++
	}
	var hdr []byte
	if t.hdrCfg != f.Config || t.hdrGen != gen {
		if hdr = m.tryHeader(t, f.Config); hdr == nil {
			return nil, nil
		}
	}
	dtsMS := (f.DTS - m.base) / 90
	ts := uint32(dtsMS) // wraps at 2^32 by design
	out := make([]rtmp.Media, 0, 3)
	if m.metaDue {
		meta, err := m.metadata()
		if err != nil {
			return nil, err
		}
		out = append(out, rtmp.Media{Type: rtmp.MsgDataAMF0, Timestamp: ts, Payload: meta})
		m.gen, m.metaDue = gen, false
	}
	if hdr != nil {
		out = append(out, rtmp.Media{Type: msgType(kind), Timestamp: ts, Payload: hdr})
		t.hdrCfg, t.hdrGen = f.Config, m.gen
	}
	payload, err := body(f, dtsMS, floorDiv(f.PTS-m.base, 90))
	if err != nil {
		return nil, err
	}
	return append(out, rtmp.Media{Type: msgType(kind), Timestamp: ts, Payload: payload}), nil
}

// metadata returns the onMetaData data message body for the carried tracks.
func (m *Muxer) metadata() ([]byte, error) {
	var arr rtmp.ECMAArray
	if v := m.video; v.active() {
		id := metaAVC
		if v.codec == media.CodecH265 {
			id = metaHEVC
		}
		arr = append(arr,
			rtmp.Property{Key: "width", Value: v.cfg.Width},
			rtmp.Property{Key: "height", Value: v.cfg.Height},
			rtmp.Property{Key: "videocodecid", Value: id})
	}
	if a := m.audio; a.active() {
		arr = append(arr,
			rtmp.Property{Key: "audiocodecid", Value: metaAAC},
			rtmp.Property{Key: "audiosamplerate", Value: a.cfg.SampleRate},
			rtmp.Property{Key: "audiochannels", Value: a.cfg.Channels},
			rtmp.Property{Key: "stereo", Value: a.cfg.Channels > 1})
	}
	b, err := rtmp.EncodeAMF0("onMetaData", arr)
	if err != nil {
		return nil, fmt.Errorf("flv: metadata: %w", err)
	}
	return b, nil
}

func msgType(k media.Kind) uint8 {
	if k == media.KindVideo {
		return rtmp.MsgVideo
	}
	return rtmp.MsgAudio
}

// floorDiv divides rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// header returns the sequence header tag body for cfg, with frame type key:
// an AVCDecoderConfigurationRecord with the legacy codec id 7, an
// HEVCDecoderConfigurationRecord as enhanced SequenceStart with FourCC hvc1,
// or an AAC AudioSpecificConfig with AACPacketType 0.
func header(cfg *media.TrackConfig) ([]byte, error) {
	var (
		tag rtmp.VideoTag
		err error
	)
	switch cfg.Codec {
	case media.CodecH264:
		tag = rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoSequenceStart, Key: true}
		tag.Data, err = AVCRecord(cfg)
	case media.CodecH265:
		tag = rtmp.VideoTag{Codec: rtmp.VideoHEVC, Kind: rtmp.VideoSequenceStart, Key: true}
		tag.Data, err = HEVCRecord(cfg)
	case media.CodecAAC:
		if len(cfg.ASC) == 0 {
			return nil, errors.New("flv: aac header: no AudioSpecificConfig")
		}
		return rtmp.AppendAudioTag(nil, rtmp.AudioTag{SequenceHeader: true, Data: cfg.ASC}), nil
	default:
		return nil, fmt.Errorf("flv: header: codec %v: %w", cfg.Codec, rtmp.ErrUnsupportedCodec)
	}
	if err != nil {
		return nil, err
	}
	b, err := rtmp.AppendVideoTag(nil, tag, cfg.Codec == media.CodecH265)
	if err != nil {
		return nil, fmt.Errorf("flv: header: %w", err)
	}
	return b, nil
}

// body returns the tag body of a coded frame whose DTS and PTS are dtsMS and
// ptsMS after the base. Video NAL units are 4-byte length-prefixed, with every
// parameter set of the Config first on a keyframe; H.265 uses enhanced
// CodedFrames, or CodedFramesX when the composition time is 0.
func body(f media.Frame, dtsMS, ptsMS int64) ([]byte, error) {
	cfg := f.Config
	switch cfg.Codec {
	case media.CodecH264, media.CodecH265:
		groups := [][][]byte{f.Data}
		if f.Key {
			groups = [][][]byte{cfg.VPS, cfg.SPS, cfg.PPS, f.Data}
		}
		n := 0
		for _, g := range groups {
			for _, nalu := range g {
				n += 4 + len(nalu)
			}
		}
		data := make([]byte, 0, n)
		for _, g := range groups {
			for _, nalu := range g {
				data = binary.BigEndian.AppendUint32(data, uint32(len(nalu)))
				data = append(data, nalu...)
			}
		}
		cts := ptsMS - dtsMS
		if cts < -1<<23 || cts >= 1<<23 {
			return nil, fmt.Errorf("flv: frame: composition time %d ms outside the signed 24-bit range", cts)
		}
		hevc := cfg.Codec == media.CodecH265
		codec := rtmp.VideoAVC
		if hevc {
			codec = rtmp.VideoHEVC
		}
		tag, err := rtmp.AppendVideoTag(nil, rtmp.VideoTag{
			Codec: codec, Kind: rtmp.VideoCodedFrames, Key: f.Key, CompTime: int32(cts), Data: data,
		}, hevc)
		if err != nil {
			return nil, fmt.Errorf("flv: frame: %w", err)
		}
		return tag, nil
	case media.CodecAAC:
		n := 0
		for _, d := range f.Data {
			n += len(d)
		}
		data := make([]byte, 0, n)
		for _, d := range f.Data {
			data = append(data, d...)
		}
		return rtmp.AppendAudioTag(nil, rtmp.AudioTag{Data: data}), nil
	}
	return nil, fmt.Errorf("flv: frame: codec %v: %w", cfg.Codec, rtmp.ErrUnsupportedCodec)
}
