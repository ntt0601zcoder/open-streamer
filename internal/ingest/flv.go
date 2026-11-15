package ingest

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/unwrap"
)

// Tracks of an RTMP input.
const (
	flvVideoTrack = 0
	flvAudioTrack = 1
)

const (
	// holdSpan is the source time a session's first video frames are held
	// for at most, to learn the initial DTS shift.
	holdSpan = media.ClockRate
	// maxHeldVideo and maxHeldBytes bound the hold for a publisher whose
	// timestamps do not advance and that sends no second keyframe.
	maxHeldVideo = 512
	maxHeldBytes = 16 << 20
)

// FourCC codec ids that onMetaData may carry as a number.
const (
	fourccAVCNum  = 0x61766331 // "avc1"
	fourccHEVCNum = 0x68766331 // "hvc1"
)

// flvFrames turns the FLV tags of one RTMP session into frames in source
// time: video on track 0, AAC audio on track 1. It is used by one goroutine;
// stats and announced are safe concurrently with it.
//
// Timestamps are the 32-bit RTMP milliseconds, unwrapped across audio and
// video together, times 90; only coded frames feed the unwrapper, since
// sequence headers and metadata are often stamped 0 mid-stream. Video PTS is DTS plus the composition offset;
// negative offsets are absorbed by a DTS shift s that only grows, learned
// over the session's first video frames, which are held until the second
// keyframe or holdSpan of source time (see push). Audio is placed on the
// AAC sample grid (see audioGrid). Configs come from the sequence headers
// and from in-band parameter sets, through the shared ConfigCache, so a
// repeated identical header keeps the config pointer.
type flvFrames struct {
	cache *media.ConfigCache
	now   func() time.Time
	unw   *unwrap.Unwrapper

	// Video configuration: the codec of vcfg, its parameter sets by ID and
	// the NAL length size of the decoder configuration record.
	vcodec  media.Codec
	vcfg    *media.TrackConfig
	vsets   paramSets
	lenSize int
	cls     *media.NALClassifier

	// The initial hold: frames keep their unshifted DTS until release.
	released  bool
	held      []heldVideo
	heldBytes int
	heldKeys  int
	// shift is s in 90 kHz ticks; lastDTS is the shifted DTS of the last
	// video frame out; lateDelta is the last late increase of s, while the
	// frames after it are still below lastDTS.
	shift, lastDTS, lateDelta int64

	acfg *media.TrackConfig
	grid audioGrid

	metaVideo, metaAudio, seqVideo, seqAudio atomic.Bool
	n                                        flvCounters
}

type flvCounters struct {
	frames, unsupported, bad, noConfig, dtsShifts, dtsShiftLate,
	reanchors, droppedAhead, seqEnds atomic.Uint64
}

// heldVideo is a held video frame with its composition offset in ticks.
type heldVideo struct {
	f   Frame
	cts int64
}

// paramSets holds video parameter sets by ID, the newest per ID.
type paramSets struct {
	vps, sps, pps map[uint32][]byte
}

func newParamSets() paramSets {
	return paramSets{vps: map[uint32][]byte{}, sps: map[uint32][]byte{}, pps: map[uint32][]byte{}}
}

func (p paramSets) clone() paramSets {
	c := newParamSets()
	for id, b := range p.vps {
		c.vps[id] = b
	}
	for id, b := range p.sps {
		c.sps[id] = b
	}
	for id, b := range p.pps {
		c.pps[id] = b
	}
	return c
}

// of returns the map for a parameter-set role, nil for other NAL units.
func (p paramSets) of(r nalRole) map[uint32][]byte {
	switch r {
	case nalVPS:
		return p.vps
	case nalSPS:
		return p.sps
	case nalPPS:
		return p.pps
	case nalOther, nalAUD:
	}
	return nil
}

// nalRole is what a video NAL unit is to flvFrames.
type nalRole uint8

const (
	nalOther nalRole = iota
	nalVPS
	nalSPS
	nalPPS
	nalAUD
)

func roleOf(codec media.Codec, n []byte) nalRole {
	if len(n) == 0 {
		return nalOther
	}
	if codec == media.CodecH265 {
		switch (n[0] >> 1) & 0x3f {
		case 32:
			return nalVPS
		case 33:
			return nalSPS
		case 34:
			return nalPPS
		case 35:
			return nalAUD
		}
		return nalOther
	}
	switch n[0] & 0x1f {
	case 7:
		return nalSPS
	case 8:
		return nalPPS
	case 9:
		return nalAUD
	}
	return nalOther
}

// newFLVFrames returns the converter of one session. cache is shared by the
// sessions of one input; now stamps arrivals (nil means time.Now).
func newFLVFrames(cache *media.ConfigCache, now func() time.Time) *flvFrames {
	if now == nil {
		now = time.Now
	}
	return &flvFrames{cache: cache, now: now, unw: unwrap.New(32), vsets: newParamSets()}
}

// push converts one message and returns the frames it completes: none, one,
// or the held video frames at the end of the hold. Unsupported codecs are
// counted (unsupported_tags) and skipped, as are frames before their kind's
// first sequence header (dropped_no_config) and sequence ends. A tag that
// does not parse, or whose configuration or NAL units are invalid, is counted
// in bad_tags and skipped, leaving the state as it was; push then returns an
// error describing it. The error is not terminal: later tags are converted
// as usual.
//
// The session's first video frames are held until the second keyframe, or
// until a frame 1 s of source time after the first, and released together
// with the shift s = max(0, −min composition offset) over them, so that B
// frames keep PTS ≥ DTS without dropping. Held frames keep the arrival of
// the push that received them.
func (f *flvFrames) push(m rtmp.Media) ([]Frame, error) {
	var (
		out []Frame
		err error
	)
	switch m.Type {
	case rtmp.MsgVideo:
		out, err = f.video(m)
	case rtmp.MsgAudio:
		out, err = f.audio(m)
	case rtmp.MsgDataAMF0:
		err = f.metadata(m.Payload)
	}
	if err != nil {
		f.n.bad.Add(1)
		return nil, err
	}
	f.n.frames.Add(uint64(len(out)))
	return out, nil
}

// flush releases the held video frames, in order, at the end of a session.
func (f *flvFrames) flush() []Frame {
	if f.released || len(f.held) == 0 {
		return nil
	}
	out := f.release()
	f.n.frames.Add(uint64(len(out)))
	return out
}

// announced reports the kinds the session announced: by onMetaData with a
// supported codec id (video 7, 12, or the FourCC avc1 or hvc1 as a number or
// a string; audio 10), or by a valid sequence header.
func (f *flvFrames) announced() (video, audio bool) {
	return f.metaVideo.Load() || f.seqVideo.Load(), f.metaAudio.Load() || f.seqAudio.Load()
}

// stats returns the session's counters.
func (f *flvFrames) stats() map[string]uint64 {
	return map[string]uint64{
		"frames":              f.n.frames.Load(),
		"unsupported_tags":    f.n.unsupported.Load(),
		"bad_tags":            f.n.bad.Load(),
		"dropped_no_config":   f.n.noConfig.Load(),
		"dts_shifts":          f.n.dtsShifts.Load(),
		"dts_shift_late":      f.n.dtsShiftLate.Load(),
		"audio_reanchors":     f.n.reanchors.Load(),
		"audio_dropped_ahead": f.n.droppedAhead.Load(),
		"sequence_ends":       f.n.seqEnds.Load(),
	}
}

func (f *flvFrames) video(m rtmp.Media) ([]Frame, error) {
	tag, err := rtmp.ParseVideoTag(m.Payload)
	if errors.Is(err, rtmp.ErrUnsupportedCodec) {
		f.n.unsupported.Add(1)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ingest: flv: video tag: %w", err)
	}
	codec := media.CodecH264
	if tag.Codec == rtmp.VideoHEVC {
		codec = media.CodecH265
	}
	switch tag.Kind {
	case rtmp.VideoSequenceStart:
		return nil, f.videoConfig(codec, tag.Data)
	case rtmp.VideoCodedFrames:
		return f.videoFrame(codec, m.Timestamp, tag)
	case rtmp.VideoSequenceEnd:
		f.n.seqEnds.Add(1)
	case rtmp.VideoMetadata, rtmp.VideoCommand:
	}
	return nil, nil
}

// videoConfig applies a sequence header: its decoder configuration record
// replaces the parameter sets. An empty header is ignored.
func (f *flvFrames) videoConfig(codec media.Codec, rec []byte) error {
	if len(rec) == 0 {
		return nil
	}
	sets, lenSize, err := decodeRecord(codec, rec)
	if err != nil {
		return fmt.Errorf("ingest: flv: %v decoder configuration record: %w", codec, err)
	}
	cfg, err := f.buildVideoConfig(codec, sets)
	if err != nil {
		return err
	}
	if codec != f.vcodec || f.cls == nil {
		f.cls = media.NewNALClassifier(codec)
	}
	f.vcodec, f.vcfg, f.vsets, f.lenSize = codec, cfg, sets, lenSize
	f.seqVideo.Store(true)
	return nil
}

// buildVideoConfig builds the config of sets and returns the cached pointer
// when it equals the previous one.
func (f *flvFrames) buildVideoConfig(codec media.Codec, sets paramSets) (*media.TrackConfig, error) {
	var (
		cfg *media.TrackConfig
		err error
	)
	if codec == media.CodecH265 {
		cfg, err = media.NewH265Config(values(sets.vps), values(sets.sps), values(sets.pps))
	} else {
		cfg, err = media.NewH264Config(values(sets.sps), values(sets.pps))
	}
	if err != nil {
		return nil, fmt.Errorf("ingest: flv: %w", err)
	}
	return f.cache.Stable(flvVideoTrack, cfg), nil
}

func values(m map[uint32][]byte) [][]byte {
	out := make([][]byte, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	return out
}

// decodeRecord returns the parameter sets and the NAL length size of an
// AVC or HEVC decoder configuration record. mp4ff decodes only 4-byte
// lengths, so the length field is read here and set to 4 on a copy before
// decoding. NAL units of other types in the record are ignored.
func decodeRecord(codec media.Codec, rec []byte) (paramSets, int, error) {
	b := bytes.Clone(rec)
	var nalus [][]byte
	var lenSize int
	if codec == media.CodecH265 {
		const lenByte = 21
		if len(b) <= lenByte+1 {
			return paramSets{}, 0, errors.New("record too short")
		}
		lenSize = int(b[lenByte]&3) + 1
		b[lenByte] |= 3
		dcr, err := hevc.DecodeHEVCDecConfRec(b)
		if err != nil {
			return paramSets{}, 0, err
		}
		for _, a := range dcr.NaluArrays {
			nalus = append(nalus, a.Nalus...)
		}
	} else {
		const lenByte = 4
		if len(b) <= lenByte {
			return paramSets{}, 0, errors.New("record too short")
		}
		lenSize = int(b[lenByte]&3) + 1
		b[lenByte] |= 3
		dcr, err := avc.DecodeAVCDecConfRec(b)
		// SPS extensions (high profiles) are not needed: the sets are read.
		if err != nil && !errors.Is(err, avc.ErrCannotParseAVCExtension) {
			return paramSets{}, 0, err
		}
		nalus = append(append(nalus, dcr.SPSnalus...), dcr.PPSnalus...)
	}
	sets := newParamSets()
	for _, n := range nalus {
		m := sets.of(roleOf(codec, n))
		if m == nil {
			continue
		}
		id, err := media.ParamSetID(codec, n)
		if err != nil {
			return paramSets{}, 0, err
		}
		m[id] = n
	}
	return sets, lenSize, nil
}

// splitNALUs splits length-prefixed NAL units with size-byte lengths.
func splitNALUs(b []byte, size int) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if len(b) < size {
			return nil, errors.New("ingest: flv: truncated NAL unit length")
		}
		n := 0
		for _, c := range b[:size] {
			n = n<<8 | int(c)
		}
		b = b[size:]
		if n > len(b) {
			return nil, fmt.Errorf("ingest: flv: NAL unit of %d bytes exceeds the %d left", n, len(b))
		}
		if n > 0 {
			out = append(out, b[:n])
		}
		b = b[n:]
	}
	return out, nil
}

// videoFrame converts coded frames: parameter sets move into the config,
// AUDs are dropped and the rest is timed.
func (f *flvFrames) videoFrame(codec media.Codec, ms uint32, tag rtmp.VideoTag) ([]Frame, error) {
	dts := f.unw.Unwrap(uint64(ms)) * 90
	if f.vcfg == nil || codec != f.vcodec {
		f.n.noConfig.Add(1)
		return nil, nil
	}
	nalus, err := splitNALUs(tag.Data, f.lenSize)
	if err != nil {
		return nil, err
	}
	data := make([][]byte, 0, len(nalus))
	sets, changed := f.vsets, false
	for _, n := range nalus {
		role := roleOf(codec, n)
		m := sets.of(role)
		switch {
		case role == nalAUD:
			continue
		case m == nil:
			data = append(data, n)
			continue
		}
		id, err := media.ParamSetID(codec, n)
		if err != nil {
			return nil, fmt.Errorf("ingest: flv: in-band parameter set: %w", err)
		}
		if bytes.Equal(m[id], n) {
			continue
		}
		if !changed {
			sets, changed = sets.clone(), true
			m = sets.of(role)
		}
		m[id] = n
	}
	if changed {
		cfg, err := f.buildVideoConfig(codec, sets)
		if err != nil {
			return nil, err
		}
		f.vcfg, f.vsets = cfg, sets
	}
	if len(data) == 0 {
		return nil, nil
	}
	cts := int64(tag.CompTime) * 90
	pts := dts + cts
	key, rasl := f.cls.Classify(data, pts)
	fr := Frame{
		Frame:   media.Frame{Track: flvVideoTrack, Config: f.vcfg, PTS: pts, DTS: dts, Key: key, RASL: rasl, Data: data},
		Arrival: f.now(),
	}
	return f.timeVideo(fr, cts), nil
}

// timeVideo applies the DTS shift to a frame whose DTS is unshifted, holding
// it while the initial shift is being learned.
func (f *flvFrames) timeVideo(fr Frame, cts int64) []Frame {
	if !f.released {
		f.held = append(f.held, heldVideo{f: fr, cts: cts})
		for _, n := range fr.Data {
			f.heldBytes += len(n)
		}
		if fr.Key {
			f.heldKeys++
		}
		if f.heldKeys >= 2 || fr.DTS-f.held[0].f.DTS >= holdSpan ||
			len(f.held) >= maxHeldVideo || f.heldBytes >= maxHeldBytes {
			return f.release()
		}
		return nil
	}
	if cts < -f.shift {
		// A late increase: frames from here on map lower by the increase,
		// so those still below the last DTS out are raised past it.
		f.lateDelta = -cts - f.shift
		f.shift = -cts
		f.n.dtsShifts.Add(1)
	}
	dts := fr.DTS - f.shift
	switch {
	case dts > f.lastDTS:
		f.lateDelta = 0
	case f.lastDTS-dts < f.lateDelta:
		dts = f.lastDTS + 1
		if dts > fr.PTS {
			f.n.dtsShiftLate.Add(1)
			return nil
		}
	}
	fr.DTS = dts
	f.lastDTS = dts
	return []Frame{fr}
}

// release ends the hold: the shift is learned over the held frames, which
// are returned in order.
func (f *flvFrames) release() []Frame {
	minCTS := int64(0)
	for _, h := range f.held {
		minCTS = min(minCTS, h.cts)
	}
	if minCTS < 0 {
		f.shift = -minCTS
		f.n.dtsShifts.Add(1)
	}
	out := make([]Frame, len(f.held))
	for i, h := range f.held {
		h.f.DTS -= f.shift
		out[i] = h.f
	}
	f.lastDTS = out[len(out)-1].DTS
	f.held, f.heldBytes, f.heldKeys, f.released = nil, 0, 0, true
	return out
}

func (f *flvFrames) audio(m rtmp.Media) ([]Frame, error) {
	tag, err := rtmp.ParseAudioTag(m.Payload)
	if errors.Is(err, rtmp.ErrUnsupportedCodec) {
		f.n.unsupported.Add(1)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ingest: flv: audio tag: %w", err)
	}
	if tag.SequenceHeader {
		// Encoders may send an empty header before they know the ASC.
		if len(tag.Data) == 0 {
			return nil, nil
		}
		cfg, err := media.NewAACConfig(tag.Data)
		if err != nil {
			return nil, fmt.Errorf("ingest: flv: %w", err)
		}
		f.acfg = f.cache.Stable(flvAudioTrack, cfg)
		f.seqAudio.Store(true)
		return nil, nil
	}
	rtmpPTS := f.unw.Unwrap(uint64(m.Timestamp)) * 90
	if f.acfg == nil {
		f.n.noConfig.Add(1)
		return nil, nil
	}
	if len(tag.Data) == 0 {
		return nil, errors.New("ingest: flv: empty AAC frame")
	}
	pts, keep, reanchored := f.grid.place(rtmpPTS, f.acfg.SampleRate)
	if reanchored {
		f.n.reanchors.Add(1)
	}
	if !keep {
		f.n.droppedAhead.Add(1)
		return nil, nil
	}
	return []Frame{{
		Frame:   media.Frame{Track: flvAudioTrack, Config: f.acfg, PTS: pts, DTS: pts, Key: true, Data: [][]byte{tag.Data}},
		Arrival: f.now(),
	}}, nil
}

// metadata reads the codec ids of onMetaData for announced. Other data
// messages are ignored.
func (f *flvFrames) metadata(b []byte) error {
	vals, err := rtmp.DecodeAMF0(b)
	if err != nil {
		return fmt.Errorf("ingest: flv: metadata: %w", err)
	}
	if len(vals) > 0 && vals[0] == "@setDataFrame" {
		vals = vals[1:]
	}
	if len(vals) < 2 || vals[0] != "onMetaData" {
		return nil
	}
	var props rtmp.Object
	switch v := vals[1].(type) {
	case rtmp.Object:
		props = v
	case rtmp.ECMAArray:
		props = rtmp.Object(v)
	default:
		return nil
	}
	if v, ok := props.Get("videocodecid"); ok && supportedVideoID(v) {
		f.metaVideo.Store(true)
	}
	if v, ok := props.Get("audiocodecid"); ok && v == any(float64(10)) {
		f.metaAudio.Store(true)
	}
	return nil
}

// supportedVideoID reports an onMetaData videocodecid of H.264 or H.265: the
// legacy ids 7 and 12, or the FourCC as a number or a string.
func supportedVideoID(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == 7 || x == 12 || x == fourccAVCNum || x == fourccHEVCNum
	case string:
		return x == "avc1" || x == "hvc1"
	}
	return false
}
