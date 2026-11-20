package conform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/unwrap"
)

// RTMP container rule names, checked by ReadRTMP on top of the frame rules.
const (
	// RuleRTMPMetadata: an onMetaData comes before the first audio or video
	// message.
	RuleRTMPMetadata = "rtmp-metadata-first"
	// RuleRTMPHeader: in every capture epoch, each track's sequence header
	// comes before its first coded frame, and has the frame's codec.
	RuleRTMPHeader = "rtmp-header-first"
	// RuleRTMPTimestamp: per track, the timestamps of the coded frames,
	// unwrapped modulo 2^32, never decrease.
	RuleRTMPTimestamp = "rtmp-timestamp-monotonic"
	// RuleRTMPCompTime: a video CompositionTime is never negative.
	RuleRTMPCompTime = "rtmp-composition-time"
	// RuleRTMPNALU: the sequence header declares 4-byte NAL unit lengths, and
	// the length-prefixed NAL units fill each video tag exactly.
	RuleRTMPNALU = "rtmp-nalu-length-prefixed"
	// RuleRTMPInband: every parameter set of the track's sequence header is in
	// band at the start of each keyframe, and every in-band parameter set is
	// one of the header's.
	RuleRTMPInband = "rtmp-param-sets-in-band"
	// RuleRTMPTag: every audio, video and data message parses, with a codec
	// RTMP outputs carry (H.264, H.265, AAC), and every sequence header
	// decodes.
	RuleRTMPTag = "rtmp-tag-valid"
)

// rtmpTrack is the state of one kind (video or audio) of an RTMP capture.
type rtmpTrack struct {
	track   int // capture track
	unw     *unwrap.Unwrapper
	seen    bool  // a coded frame was read
	last    int64 // its unwrapped timestamp, ms
	cfg     *media.TrackConfig
	epoch   uint32 // epoch of the last valid sequence header
	lenSize int    // NAL unit length size of the video header
}

// rtmpCapture is the state of one ReadRTMP.
type rtmpCapture struct {
	b            *builder
	video, audio *rtmpTrack
	epoch        uint32
	used         bool // the epoch holds a header or a frame
	meta         bool // an onMetaData was read
	lo, hi       map[int]int64
}

// ReadRTMP plays u with pkg/rtmp for opt.Duration and checks: a sequence
// header precedes each carried track's first media message; per track
// timestamps never decrease (mod 2^32, unwrapped with pkg/unwrap);
// CompositionTime ≥ 0; NAL units length-prefixed; every parameter set of the
// header in-band before each keyframe. Frames are rebuilt in 90 kHz
// (DTS = ts·90, PTS = (ts+cts)·90), parameter sets moved from Data into
// Config. Each onMetaData opens a new capture epoch, and every frame check —
// including the start rules (keyframe first, track start offset) — runs on
// every capture epoch, the first one included (the muxer aligns every start).
//
// The video and the audio track are numbered in order of first appearance.
// Data messages other than onMetaData are ignored. A coded frame without a
// valid sequence header of its codec is reported and left out of the
// frames; one whose epoch has no header yet is reported and kept with the
// previous header's config. Timestamps are checked on coded frames only,
// since some servers stamp headers 0 mid-stream. The read stops at the end
// of the stream (io.EOF, a clean end) or once any track spans opt.Duration;
// Duration 0 means the end of the stream. opt.Timeout bounds the connection
// set-up and every wait for the next message; Once and Client are ignored.
func ReadRTMP(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	fail := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("conform: read: rtmp: %w", ctx.Err())
		case stalled.Load():
			return fmt.Errorf("conform: read: rtmp: stalled for %v: %w", timeout, err)
		}
		return fmt.Errorf("conform: read: rtmp: %w", err)
	}
	c, err := rtmp.PlayWith(rctx, u, rtmp.Config{HandshakeTimeout: timeout, CommandTimeout: timeout})
	if err != nil {
		return Capture{}, fail(err)
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(rctx, func() { _ = c.Close() })
	defer stop()

	r := &rtmpCapture{b: newBuilder(), epoch: 1, lo: map[int]int64{}, hi: map[int]int64{}}
	r.b.start(1, true)
	limit := media.FromDuration(opt.Duration)
	for {
		timer.Reset(timeout)
		m, err := c.ReadMedia()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Capture{}, fail(err)
		}
		if r.message(m) && limit > 0 && r.span() >= limit {
			break
		}
	}
	return r.b.finish(), nil
}

// span returns the longest DTS span of a track.
func (r *rtmpCapture) span() int64 {
	var s int64
	for t, lo := range r.lo {
		s = max(s, r.hi[t]-lo)
	}
	return s
}

// message handles one media message and reports whether it added a frame.
func (r *rtmpCapture) message(m rtmp.Media) bool {
	switch m.Type {
	case rtmp.MsgDataAMF0:
		r.data(m)
	case rtmp.MsgVideo:
		return r.videoMsg(m)
	case rtmp.MsgAudio:
		return r.audioMsg(m)
	}
	return false
}

// data opens a new epoch on onMetaData.
func (r *rtmpCapture) data(m rtmp.Media) {
	vals, err := rtmp.DecodeAMF0(m.Payload)
	if err != nil {
		r.b.violate(RuleRTMPTag, -1, "data message at %d ms: %v", m.Timestamp, err)
		return
	}
	if len(vals) == 0 || vals[0] != "onMetaData" {
		return
	}
	if r.used {
		r.epoch++
		r.used = false
	}
	r.meta = true
}

// use marks the epoch used by an audio or video message.
func (r *rtmpCapture) use(at uint32) {
	if !r.meta {
		r.b.once("meta", RuleRTMPMetadata, -1, "media message at %d ms before any onMetaData", at)
	}
	r.used = true
}

// trackOf returns the state of a kind, creating it on first use.
func (r *rtmpCapture) trackOf(slot **rtmpTrack, kind string) *rtmpTrack {
	if *slot == nil {
		*slot = &rtmpTrack{track: r.b.track("rtmp:"+kind, 0), unw: unwrap.New(32)}
	}
	return *slot
}

// header records a sequence header's config, nil when it did not decode.
func (r *rtmpCapture) header(t *rtmpTrack, cfg *media.TrackConfig) {
	t.cfg = cfg
	if cfg != nil {
		t.epoch = r.epoch
	}
}

// coded checks a coded frame's header and timestamp and returns its
// unwrapped time in ms; ok is false when it has no config to build a frame.
func (r *rtmpCapture) coded(t *rtmpTrack, codec media.Codec, at uint32) (int64, bool) {
	ms := t.unw.Unwrap(uint64(at))
	if t.seen && ms < t.last {
		r.b.violate(RuleRTMPTimestamp, t.track, "timestamp %d ms is below the previous %d ms", ms, t.last)
	}
	t.seen, t.last = true, ms
	key := "hdr:" + strconv.Itoa(t.track) + ":" + strconv.FormatUint(uint64(r.epoch), 10)
	switch {
	case t.cfg == nil:
		r.b.once(key, RuleRTMPHeader, t.track, "epoch %d: frame at %d ms without a valid sequence header", r.epoch, at)
		return ms, false
	case t.cfg.Codec != codec:
		r.b.once(key+":codec", RuleRTMPHeader, t.track, "epoch %d: frame at %d ms is %v, the sequence header %v",
			r.epoch, at, codec, t.cfg.Codec)
		return ms, false
	case t.epoch != r.epoch:
		r.b.once(key, RuleRTMPHeader, t.track, "epoch %d: frame at %d ms before the epoch's sequence header", r.epoch, at)
	}
	return ms, true
}

// add appends a frame and widens its track's span.
func (r *rtmpCapture) add(f media.Frame) {
	if lo, ok := r.lo[f.Track]; !ok || f.DTS < lo {
		r.lo[f.Track] = f.DTS
	}
	r.hi[f.Track] = max(r.hi[f.Track], f.DTS)
	r.b.add(f)
}

func (r *rtmpCapture) audioMsg(m rtmp.Media) bool {
	tag, err := rtmp.ParseAudioTag(m.Payload)
	if err != nil {
		r.b.violate(RuleRTMPTag, -1, "audio message at %d ms: %v", m.Timestamp, err)
		return false
	}
	r.use(m.Timestamp)
	t := r.trackOf(&r.audio, "audio")
	if tag.SequenceHeader {
		cfg, err := media.NewAACConfig(tag.Data)
		if err != nil {
			r.b.violate(RuleRTMPTag, t.track, "AAC sequence header at %d ms: %v", m.Timestamp, err)
		}
		r.header(t, cfg)
		return false
	}
	ms, ok := r.coded(t, media.CodecAAC, m.Timestamp)
	if !ok {
		return false
	}
	ts := ms * 90
	r.add(media.Frame{
		Track: t.track, Config: t.cfg, PTS: ts, DTS: ts, Key: true, Data: [][]byte{tag.Data}, Epoch: r.epoch,
	})
	return true
}

func (r *rtmpCapture) videoMsg(m rtmp.Media) bool {
	tag, err := rtmp.ParseVideoTag(m.Payload)
	if err != nil {
		r.b.violate(RuleRTMPTag, -1, "video message at %d ms: %v", m.Timestamp, err)
		return false
	}
	codec := media.CodecH264
	if tag.Codec == rtmp.VideoHEVC {
		codec = media.CodecH265
	}
	switch tag.Kind {
	case rtmp.VideoSequenceStart:
		r.use(m.Timestamp)
		t := r.trackOf(&r.video, "video")
		cfg, lenSize, err := videoRecord(codec, tag.Data)
		if err != nil {
			r.b.violate(RuleRTMPTag, t.track, "%v sequence header at %d ms: %v", codec, m.Timestamp, err)
		} else if lenSize != 4 {
			r.b.violate(RuleRTMPNALU, t.track, "sequence header at %d ms declares %d-byte NAL unit lengths", m.Timestamp, lenSize)
		}
		r.header(t, cfg)
		t.lenSize = lenSize
		return false
	case rtmp.VideoCodedFrames:
		r.use(m.Timestamp)
		return r.videoFrame(r.trackOf(&r.video, "video"), codec, m.Timestamp, tag)
	case rtmp.VideoSequenceEnd, rtmp.VideoMetadata, rtmp.VideoCommand:
	}
	return false
}

// videoFrame checks and adds one coded video frame.
func (r *rtmpCapture) videoFrame(t *rtmpTrack, codec media.Codec, at uint32, tag rtmp.VideoTag) bool {
	ms, ok := r.coded(t, codec, at)
	if tag.CompTime < 0 {
		r.b.violate(RuleRTMPCompTime, t.track, "frame at %d ms has composition time %d ms", at, tag.CompTime)
	}
	if !ok {
		return false
	}
	nalus, err := splitNALUs(tag.Data, t.lenSize)
	if err != nil {
		r.b.violate(RuleRTMPNALU, t.track, "frame at %d ms: %v", at, err)
		return false
	}
	data := r.inband(t, codec, at, tag.Key, nalus)
	dts := ms * 90
	r.add(media.Frame{
		Track: t.track, Config: t.cfg, DTS: dts, PTS: dts + int64(tag.CompTime)*90,
		Key: tag.Key, RASL: isRASL(codec, data), Data: data, Epoch: r.epoch,
	})
	return true
}

// inband applies RuleRTMPInband to a frame's NAL units and returns them
// without the parameter sets.
func (r *rtmpCapture) inband(t *rtmpTrack, codec media.Codec, at uint32, key bool, nalus [][]byte) [][]byte {
	var lead [][]byte // the parameter sets before the first other NAL unit
	data := make([][]byte, 0, len(nalus))
	leading := true
	for _, n := range nalus {
		name := paramSetName(codec, n)
		if name == "" {
			if !isAUD(codec, n) {
				leading = false
			}
			data = append(data, n)
			continue
		}
		if leading {
			lead = append(lead, n)
		}
		if !slices.ContainsFunc(slices.Concat(t.cfg.VPS, t.cfg.SPS, t.cfg.PPS), func(s []byte) bool { return bytes.Equal(s, n) }) {
			r.b.violate(RuleRTMPInband, t.track, "frame at %d ms: in-band %s is not in the sequence header", at, name)
		}
	}
	if key {
		for _, s := range slices.Concat(t.cfg.VPS, t.cfg.SPS, t.cfg.PPS) {
			if !slices.ContainsFunc(lead, func(n []byte) bool { return bytes.Equal(s, n) }) {
				r.b.violate(RuleRTMPInband, t.track, "keyframe at %d ms: the header's %s is not in band before it", at, paramSetName(codec, s))
			}
		}
	}
	return data
}

// paramSetName returns "VPS", "SPS" or "PPS" for a parameter set NAL unit,
// "" for any other.
func paramSetName(codec media.Codec, n []byte) string {
	if len(n) == 0 {
		return ""
	}
	if codec == media.CodecH265 {
		switch (n[0] >> 1) & 0x3f {
		case 32:
			return "VPS"
		case 33:
			return "SPS"
		case 34:
			return "PPS"
		}
		return ""
	}
	switch n[0] & 0x1f {
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	}
	return ""
}

// isAUD reports an access unit delimiter.
func isAUD(codec media.Codec, n []byte) bool {
	if len(n) == 0 {
		return false
	}
	if codec == media.CodecH265 {
		return (n[0]>>1)&0x3f == 35
	}
	return n[0]&0x1f == 9
}

// videoRecord decodes an AVC or HEVC decoder configuration record into a
// config and its NAL unit length size. mp4ff decodes only 4-byte lengths,
// so the size is read here and set to 4 on a copy before decoding; a
// decoder panic on hostile input is an error.
func videoRecord(codec media.Codec, rec []byte) (cfg *media.TrackConfig, lenSize int, err error) {
	defer func() {
		if p := recover(); p != nil {
			cfg, lenSize, err = nil, 0, fmt.Errorf("decoder panic: %v", p)
		}
	}()
	b := bytes.Clone(rec)
	if codec == media.CodecH265 {
		const lenByte = 21
		if len(b) <= lenByte+1 {
			return nil, 0, errors.New("record too short")
		}
		lenSize = int(b[lenByte]&3) + 1
		b[lenByte] |= 3
		dcr, err := hevc.DecodeHEVCDecConfRec(b)
		if err != nil {
			return nil, 0, fmt.Errorf("decode: %w", err)
		}
		cfg, err = media.NewH265Config(dcr.GetNalusForType(hevc.NALU_VPS),
			dcr.GetNalusForType(hevc.NALU_SPS), dcr.GetNalusForType(hevc.NALU_PPS))
		if err != nil {
			return nil, 0, err
		}
		return cfg, lenSize, nil
	}
	const lenByte = 4
	if len(b) <= lenByte {
		return nil, 0, errors.New("record too short")
	}
	lenSize = int(b[lenByte]&3) + 1
	b[lenByte] |= 3
	dcr, err := avc.DecodeAVCDecConfRec(b)
	// SPS extensions (high profiles) are not needed: the sets are read.
	if err != nil && !errors.Is(err, avc.ErrCannotParseAVCExtension) {
		return nil, 0, fmt.Errorf("decode: %w", err)
	}
	cfg, err = media.NewH264Config(dcr.SPSnalus, dcr.PPSnalus)
	if err != nil {
		return nil, 0, err
	}
	return cfg, lenSize, nil
}
