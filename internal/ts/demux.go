// Package ts demultiplexes MPEG-TS into media frames.
package ts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/Eyevinn/mp4ff/avc"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// Supported PMT stream_type values.
const (
	typeH264 = 0x1B
	typeH265 = 0x24
	typeAAC  = 0x0F
)

// DemuxStats counts events the demuxer recovered from.
type DemuxStats struct {
	// DroppedNoConfig counts video frames dropped before the first complete
	// parameter-set group.
	DroppedNoConfig uint64
	// DecodeErrors counts continuity gaps, malformed PSI sections, PES
	// packets and packet runs, PES without a PTS that continue nothing, and
	// bad parameter sets and ADTS frames.
	DecodeErrors uint64
	// PMTChanges counts PMTs after the first whose (PID, stream_type) set
	// differed from the current one. It counts only in Remap mode.
	PMTChanges uint64
	// LostFrames counts access units not emitted because PES data for them
	// was lost or dropped (mpegts LostPES, signalled by Discontinuity): one
	// per Discontinuity for the lost data, which held at least one unit
	// (several PES lost in one run count once), plus every unit the demuxer
	// discards because of the loss: the pending video unit on a track that
	// carries PES without PTS, and the units or audio frames in PES without
	// PTS dropped after the loss.
	LostFrames uint64
	// SplitAUs counts access units found after the first one inside one PES
	// (or starting in a PES without PTS) and given interpolated or
	// extrapolated timestamps.
	SplitAUs uint64
	// SplitAUsReordered counts such access units dropped because the PES had
	// PTS != DTS.
	SplitAUsReordered uint64
	// SplitAUsNoRoom counts such access units dropped because no DTS was
	// left for them: below the next PES's DTS and above the track's last.
	SplitAUsNoRoom uint64
	// Continued counts video PES without PTS appended to the previous access
	// unit.
	Continued uint64
}

// defaultMaxTracks is the track cap when DemuxOptions.MaxTracks is zero.
const defaultMaxTracks = 16

// DemuxOptions configures a Demuxer.
type DemuxOptions struct {
	// Decoder unwraps PES timestamps. Demuxers that read consecutive parts
	// of one stream share it to keep a single timeline; nil means a private
	// decoder.
	Decoder *mpegts.TimeDecoder
	// FollowPMT applies PMTs after the first: new supported PIDs become new
	// tracks, and PIDs a later PMT removes stop producing frames. Without
	// FollowPMT or Remap later PMTs are ignored.
	FollowPMT bool
	// Remap is the ingest mode: a PMT whose (PID, stream_type) set differs
	// from the current one re-assigns the tracks by (kind, ordinal). The
	// track slots are fixed by the first PMT; see remap.
	Remap bool
	// MaxTracks caps the tracks; streams beyond it are skipped with a
	// reason. 0 means 16.
	MaxTracks int
}

type streamInfo struct {
	kind  media.Kind
	codec media.Codec
}

var supportedTypes = map[uint8]streamInfo{
	typeH264: {media.KindVideo, media.CodecH264},
	typeH265: {media.KindVideo, media.CodecH265},
	typeAAC:  {media.KindAudio, media.CodecAAC},
}

type track struct {
	idx    int
	pid    uint16
	stype  uint8
	active bool // Remap: the current PMT has a stream for this slot
	kind   media.Kind
	codec  media.Codec
	config *media.TrackConfig // nil while the parameter sets are invalid
	good   *media.TrackConfig // the last valid video config, kept while config is nil

	// Video parameter sets, one per ID (the newest wins), keyed by ID.
	vps, sps, pps map[uint32][]byte
	// dirty: a set changed, or arrived while config is nil, since the last
	// rebuild; bad: a set since then had no readable ID.
	dirty, bad bool

	// Video access-unit assembly.
	au       *videoAU // the pending access unit; nil when none
	held     [][]byte // parameter sets and prefix SEI after au's last slice
	timed    bool     // a PES with a PTS has been seen
	ptsIsDTS bool     // the last PES with a PTS had PTS == DTS
	pesDTS   int64    // DTS of the last PES with a PTS
	pesAUs   int      // access units started since it: its group
	step     int64    // last evenly spaced per-unit DTS step; 0 when unknown
	// group holds the completed split units of the group, waiting for the
	// next PES with a PTS to time them; groupSize is their size.
	group     []*videoAU
	groupSize int
	groupLoss bool  // data was lost since the group started: no even spacing
	lastOut   int64 // DTS of the last video frame queued
	haveOut   bool
	// fieldSPS holds the IDs of H.264 SPS that allow field coding.
	fieldSPS map[uint32]bool
	// cls decides Key and RASL for each timed access unit; nil for audio.
	cls *media.NALClassifier

	// lost: PES data was lost since the last PES with a PTS, so a PES
	// without PTS is dropped.
	lost bool
	// ptsLess: a PES without PTS has been seen, so an access unit may span
	// several PES.
	ptsLess bool

	// Audio timing: the next frame is aSamples samples at aRate after aBase.
	aTimed   bool
	aBase    int64
	aSamples int64
	aRate    int
}

// videoAU is a video access unit under assembly.
type videoAU struct {
	pts, dts int64
	idx      int // position in its group; 0 is the PES's own unit
	config   *media.TrackConfig
	data     [][]byte
	size     int  // bytes of its NAL units, held ones included
	slice    bool // it has a slice
	drop     bool // never emitted: unknown timestamps, or over maxAUSize
}

// Demuxer reads media frames from an MPEG-TS stream.
type Demuxer struct {
	dmx     *mpegts.Demux
	follow  bool
	remap   bool
	maxTrks int
	curSet  []mpegts.Stream // Remap: the (PID, stream_type) set of the current PMT
	tracks  []*track
	byPID   map[uint16]*track // PIDs whose PES are decoded
	seen    map[uint16]uint8  // stream_type of every PID at its first PMT
	skipped []string
	skipPID map[uint16]bool // PIDs reported in skipped
	timeDec *mpegts.TimeDecoder
	queue   []media.Frame
	qgen    []uint32         // PMT generation of each queued frame
	gen     uint32           // Remap: PMT changes so far
	lastGen uint32           // generation of the frame Read returned last
	remaps  map[uint32][]int // Remap: slots re-assigned by the PMT that opened a generation
	stats   DemuxStats
	err     error // a read error, returned once the queue is empty
}

// NewDemuxer is NewDemuxerWithOptions without options: a private timestamp
// decoder, and PMTs after the first are ignored (the pipeline handles PMT
// changes in M2).
func NewDemuxer(r io.Reader) (*Demuxer, error) {
	return NewDemuxerWithOptions(r, DemuxOptions{})
}

// NewDemuxerWithOptions reads r up to the first PMT and returns a demuxer for
// its supported tracks. Only PIDs in the current PMT produce frames.
func NewDemuxerWithOptions(r io.Reader, opt DemuxOptions) (*Demuxer, error) {
	if opt.FollowPMT && opt.Remap {
		return nil, errors.New("ts: demux: FollowPMT and Remap are exclusive")
	}
	d := &Demuxer{
		dmx:     mpegts.NewDemux(r),
		follow:  opt.FollowPMT,
		remap:   opt.Remap,
		maxTrks: opt.MaxTracks,
		byPID:   map[uint16]*track{},
		seen:    map[uint16]uint8{},
		skipPID: map[uint16]bool{},
		timeDec: opt.Decoder,
	}
	if d.maxTrks <= 0 {
		d.maxTrks = defaultMaxTracks
	}
	if d.timeDec == nil {
		d.timeDec = &mpegts.TimeDecoder{}
	}
	for {
		ev, err := d.dmx.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("ts: demux: no supported track: no PMT before end of input")
		}
		if err != nil {
			return nil, fmt.Errorf("ts: demux: read: %w", err)
		}
		if ev.PMT == nil {
			continue
		}
		d.applyPMT(ev.PMT)
		d.remapPMT(ev.PMT)
		if len(d.tracks) == 0 {
			return nil, errors.New("ts: demux: no supported track in PMT")
		}
		return d, nil
	}
}

// applyPMT adds the PMT's supported PIDs never seen before as tracks. A PID
// whose stream_type differs from its first one stops producing frames for
// good; its track index stays.
func (d *Demuxer) applyPMT(pmt *mpegts.PMT) {
	if d.remap {
		return
	}
	// A PID absent from this PMT sends no PES until it returns, so its
	// pending units complete now, as in Remap mode.
	for _, t := range d.tracks {
		absent := !slices.ContainsFunc(pmt.Streams, func(s mpegts.Stream) bool { return s.PID == t.pid })
		if absent && d.byPID[t.pid] == t {
			d.flushTrack(t)
		}
	}
	for _, s := range pmt.Streams {
		info, supported := supportedTypes[s.Type]
		first, known := d.seen[s.PID]
		switch {
		case !known:
			d.seen[s.PID] = s.Type
			if !supported {
				d.skip(s)
				continue
			}
			if len(d.tracks) >= d.maxTrks {
				d.skipCap(s)
				continue
			}
			t := newTrack(len(d.tracks), s, info)
			d.tracks = append(d.tracks, t)
			d.byPID[s.PID] = t
		case s.Type != first:
			if t := d.byPID[s.PID]; t != nil {
				d.flushTrack(t) // Its PES stop here.
			}
			delete(d.byPID, s.PID)
			if !supported {
				d.skip(s)
			}
		}
	}
}

func newTrack(idx int, s mpegts.Stream, info streamInfo) *track {
	t := &track{
		idx: idx, pid: s.PID, stype: s.Type, active: true, kind: info.kind, codec: info.codec,
		vps: map[uint32][]byte{}, sps: map[uint32][]byte{}, pps: map[uint32][]byte{},
	}
	if info.kind == media.KindVideo {
		t.cls = media.NewNALClassifier(info.codec)
	}
	return t
}

// skipCap reports a stream left out by the track cap, once.
func (d *Demuxer) skipCap(s mpegts.Stream) {
	if d.skipPID[s.PID] {
		return
	}
	d.skipPID[s.PID] = true
	d.skipped = append(d.skipped, fmt.Sprintf("pid %d: track limit %d reached (0x%02x)", s.PID, d.maxTrks, s.Type))
}

// remapPMT is the single place Remap mode re-assigns tracks. The first PMT
// fixes one slot per (kind, ordinal) in PMT order. A later PMT whose
// (PID, stream_type) list differs maps each supported stream to the slot of
// the same kind and ordinal, appending a slot for a new ordinal while the cap
// allows. A slot whose PID and type changed, or that was absent, starts
// fresh: no parameter sets and, once they arrive, a new Config. A slot with
// the same PID and type keeps its state. A slot with no stream in the new
// PMT produces nothing. A PMT with an identical list changes nothing.
func (d *Demuxer) remapPMT(pmt *mpegts.PMT) {
	if !d.remap {
		return
	}
	first := d.curSet == nil
	if !first && slices.Equal(d.curSet, pmt.Streams) {
		return
	}
	if !first {
		d.stats.PMTChanges++
		// Pending access units complete with the Config of the old PMT.
		d.flushAll()
		d.gen++
	}
	d.curSet = slices.Clone(pmt.Streams)
	if d.curSet == nil {
		d.curSet = []mpegts.Stream{}
	}
	next := make([]bool, len(d.tracks), d.maxTrks)
	d.byPID = map[uint16]*track{}
	ordinal := map[media.Kind]int{}
	var moved []int
	for _, s := range pmt.Streams {
		info, supported := supportedTypes[s.Type]
		if !supported {
			d.skip(s)
			continue
		}
		k := ordinal[info.kind]
		ordinal[info.kind]++
		slot := d.slotOf(info.kind, k)
		var t *track
		switch {
		case slot < 0:
			if len(d.tracks) >= d.maxTrks {
				d.skipCap(s)
				continue
			}
			slot = len(d.tracks)
			t = newTrack(slot, s, info)
			d.tracks = append(d.tracks, t)
			next = append(next, true)
		case d.tracks[slot].active && d.tracks[slot].pid == s.PID && d.tracks[slot].stype == s.Type:
			t = d.tracks[slot]
			next[slot] = true
		default:
			if d.tracks[slot].active {
				moved = append(moved, slot)
			}
			t = newTrack(slot, s, info)
			d.tracks[slot] = t
			next[slot] = true
		}
		d.byPID[s.PID] = t
	}
	for i, t := range d.tracks {
		t.active = next[i]
	}
	if len(moved) > 0 {
		if d.remaps == nil {
			d.remaps = map[uint32][]int{}
		}
		d.remaps[d.gen] = moved
	}
}

// slotOf returns the index of the ordinal-th track of a kind, or -1.
func (d *Demuxer) slotOf(kind media.Kind, ordinal int) int {
	n := 0
	for i, t := range d.tracks {
		if t.kind != kind {
			continue
		}
		if n == ordinal {
			return i
		}
		n++
	}
	return -1
}

// skip reports an unsupported PID once.
func (d *Demuxer) skip(s mpegts.Stream) {
	if d.skipPID[s.PID] {
		return
	}
	d.skipPID[s.PID] = true
	name := "unsupported"
	if s.Type == 0x03 || s.Type == 0x04 {
		name = "mpeg audio"
	}
	d.skipped = append(d.skipped, fmt.Sprintf("pid %d: %s (0x%02x)", s.PID, name, s.Type))
}

// NumTracks returns the number of supported tracks.
func (d *Demuxer) NumTracks() int { return len(d.tracks) }

// TrackKinds returns the media kind of each track, by track index.
func (d *Demuxer) TrackKinds() []media.Kind {
	kinds := make([]media.Kind, len(d.tracks))
	for i, t := range d.tracks {
		kinds[i] = t.kind
	}
	return kinds
}

// TrackPIDs returns the PID of each track, by track index.
func (d *Demuxer) TrackPIDs() []uint16 {
	pids := make([]uint16, len(d.tracks))
	for i, t := range d.tracks {
		pids[i] = t.pid
	}
	return pids
}

// Skipped describes the PMT entries that are not demuxed.
func (d *Demuxer) Skipped() []string { return append([]string(nil), d.skipped...) }

// Stats returns the recovery counters.
func (d *Demuxer) Stats() DemuxStats {
	s := d.stats
	if d.dmx != nil {
		ms := d.dmx.Stats()
		s.DecodeErrors += ms.CCErrors + ms.PSIErrors + ms.PESErrors + ms.Reader.Errored
	}
	return s
}

// enqueue queues a frame under the current PMT generation.
func (d *Demuxer) enqueue(f media.Frame) {
	d.queue = append(d.queue, f)
	d.qgen = append(d.qgen, d.gen)
}

// ReadGen is Read that also returns the PMT generation of the frame: the
// number of PMT changes (see DemuxStats.PMTChanges) in effect when the frame
// was queued. Frames flushed by a PMT change carry the old generation. The
// generation is 0 without Remap.
func (d *Demuxer) ReadGen() (media.Frame, uint32, error) {
	f, err := d.Read()
	if err != nil {
		return f, d.gen, err
	}
	return f, d.lastGen, nil
}

// TakeRemapped returns, once, the slots that the PMT opening generation gen
// moved to another PID or stream_type while they were active. A PMT that
// only adds a slot, drops one or changes nothing for the existing slots
// returns none.
func (d *Demuxer) TakeRemapped(gen uint32) []int {
	s := d.remaps[gen]
	delete(d.remaps, gen)
	return s
}

// CCErrors returns the continuity-counter gaps on tracked PIDs, which Stats
// folds into DecodeErrors.
func (d *Demuxer) CCErrors() uint64 {
	if d.dmx == nil {
		return 0
	}
	return d.dmx.Stats().CCErrors
}

// Read returns the next frame, or io.EOF at the end of the stream. A read
// error from the input is returned wrapped, and again on later calls. A video
// access unit may span PES, so it is returned once something completes it:
// the next access-unit start on its track, a PMT change, or the end of input
// or a read error, which return every pending unit first. A split unit (see
// handleVideo) also waits for the next PES with a PTS on its track, which
// times it.
func (d *Demuxer) Read() (media.Frame, error) {
	for {
		if len(d.queue) > 0 {
			f := d.queue[0]
			d.queue = d.queue[1:]
			d.lastGen = d.qgen[0]
			d.qgen = d.qgen[1:]
			return f, nil
		}
		if d.err != nil {
			return media.Frame{}, d.err
		}
		ev, err := d.dmx.Next()
		switch {
		case errors.Is(err, io.EOF):
			d.flushAll()
			d.err = io.EOF
			continue
		case err != nil:
			d.flushAll()
			d.err = fmt.Errorf("ts: demux: read: %w", err)
			continue
		case ev.PMT != nil:
			switch {
			case d.follow:
				d.applyPMT(ev.PMT)
			case d.remap:
				d.remapPMT(ev.PMT)
			}
		case ev.PES != nil:
			if t := d.byPID[ev.PES.PID]; t != nil {
				d.handlePES(t, ev.PES)
			}
		}
	}
}

// defaultFrameStep is the DTS step of a split access unit on a track with no
// measured step.
const defaultFrameStep = 3600

// maxSplitStep is the largest per-unit gap split units are spaced over:
// above 0.5 s the next PES is taken as a timestamp jump.
const maxSplitStep = 45000

// maxGroupSize bounds the split units of one track waiting for the next PES
// with a PTS, as mpegts bounds all partial PES of a demux.
const maxGroupSize = 32 << 20

// aacFrameSamples is the number of samples in one AAC frame.
const aacFrameSamples = 1024

// maxAUSize bounds one video access unit, as mpegts bounds one PES: a stream
// of PES without PTS and no access-unit start would otherwise grow it
// without limit.
const maxAUSize = 8 << 20

func (d *Demuxer) handlePES(t *track, pes *mpegts.PES) {
	if t.kind == media.KindAudio {
		d.handleAudio(t, pes)
		return
	}
	d.handleVideo(t, pes)
}

// decodeTimes unwraps the timestamps of a PES with a PTS.
func (d *Demuxer) decodeTimes(pes *mpegts.PES) (pts, dts int64) {
	pts = d.timeDec.Decode(pes.PTS)
	dts = pts
	if pes.DTS != pes.PTS {
		dts = d.timeDec.Decode(pes.DTS)
	}
	return pts, dts
}

// handleAudio emits the ADTS frames of an audio PES. Frames are timed by
// sample count from the last PES with a PTS, so a PES without PTS continues
// 1024 samples per frame after the previous frame. After a loss
// (Discontinuity) such a PES is dropped until the next PES with a PTS.
func (d *Demuxer) handleAudio(t *track, pes *mpegts.PES) {
	if pes.Discontinuity {
		d.stats.LostFrames++
		t.lost = true
	}
	if pes.HasPTS {
		pts, _ := d.decodeTimes(pes)
		t.lost = false
		t.aTimed, t.aBase, t.aSamples = true, pts, 0
	}
	frames, err := mpegts.ParseADTS(pes.Data)
	if err != nil {
		d.stats.DecodeErrors++
	}
	if !pes.HasPTS {
		switch {
		case t.lost:
			d.stats.LostFrames += uint64(len(frames))
			return
		case !t.aTimed:
			d.stats.DecodeErrors++
			return
		}
	}
	for _, a := range frames {
		asc := a.AudioSpecificConfig()
		if t.config == nil || !bytes.Equal(t.config.ASC, asc) {
			cfg, cerr := media.NewAACConfig(asc)
			if cerr != nil {
				d.stats.DecodeErrors++
				t.aSamples += aacFrameSamples
				continue
			}
			t.config = cfg
		}
		ts := t.audioTime(t.config.SampleRate)
		t.aSamples += aacFrameSamples
		d.enqueue(media.Frame{
			Track:  t.idx,
			Config: t.config,
			PTS:    ts,
			DTS:    ts,
			Key:    true,
			Data:   [][]byte{a.Payload},
		})
	}
}

// audioTime returns the PTS of the next audio frame at rate. A rate change
// moves the base to the end of the samples counted at the old rate.
func (t *track) audioTime(rate int) int64 {
	if rate != t.aRate {
		if t.aRate > 0 {
			t.aBase += media.SampleTime(t.aSamples, t.aRate)
			t.aSamples = 0
		}
		t.aRate = rate
	}
	return t.aBase + media.SampleTime(t.aSamples, rate)
}

// NAL unit classification helpers.

func (t *track) nalType(n []byte) uint8 {
	if t.codec == media.CodecH265 {
		return (n[0] >> 1) & 0x3f
	}
	return n[0] & 0x1f
}

func (t *track) isAUD(typ uint8) bool {
	if t.codec == media.CodecH265 {
		return typ == 35
	}
	return typ == 9
}

// paramSets returns the set map a parameter-set NAL type belongs to.
func (t *track) paramSets(typ uint8) map[uint32][]byte {
	if t.codec == media.CodecH265 {
		switch typ {
		case 32:
			return t.vps
		case 33:
			return t.sps
		case 34:
			return t.pps
		}
		return nil
	}
	switch typ {
	case 7:
		return t.sps
	case 8:
		return t.pps
	}
	return nil
}

// isVCL reports a slice NAL unit.
func (t *track) isVCL(typ uint8) bool {
	if t.codec == media.CodecH265 {
		return typ <= 31
	}
	return typ >= 1 && typ <= 5
}

// isFirstSlice reports a slice that starts a picture: first_mb_in_slice == 0
// (H.264; partitions B and C carry no slice header) or
// first_slice_segment_in_pic_flag (H.265). Both are the first bit after the
// NAL unit header; ue(v) 0 is the single bit 1.
func (t *track) isFirstSlice(typ uint8, n []byte) bool {
	if t.codec == media.CodecH265 {
		return len(n) > 2 && n[2]&0x80 != 0
	}
	return (typ == 1 || typ == 2 || typ == 5) && len(n) > 1 && n[1]&0x80 != 0
}

// leadsAU reports the non-VCL types that, after the last slice of an access
// unit, belong to the next one: parameter sets and prefix SEI.
func (t *track) leadsAU(typ uint8) bool {
	if t.paramSets(typ) != nil {
		return true
	}
	if t.codec == media.CodecH265 {
		return typ == 39
	}
	return typ == 6
}

// fieldCoded reports an H.264 track with an SPS that allows field coding
// (frame_mbs_only_flag == 0: PAFF or MBAFF). The fields of a frame commonly
// share one PES, each starting with first_mb_in_slice == 0 and often with an
// AUD, and their order is unknown without POC, so such a track never splits:
// each PES stays one frame, and a PES without PTS continues it.
func (t *track) fieldCoded() bool {
	return t.codec == media.CodecH264 && len(t.fieldSPS) > 0
}

// splitsOnSlice reports whether a first slice after a slice starts a new
// access unit: always for H.265, and for H.264 only once SPS are known and
// all are progressive (frame_mbs_only_flag == 1).
func (t *track) splitsOnSlice() bool {
	if t.codec == media.CodecH265 {
		return true
	}
	return len(t.sps) > 0 && !t.fieldCoded()
}

// startsAU reports a NAL unit that starts an access unit when it follows a
// slice: an AUD, or a first slice when splitsOnSlice. Nothing does on a
// field-coded track.
func (t *track) startsAU(typ uint8, n []byte) bool {
	if t.fieldCoded() {
		return false
	}
	return t.isAUD(typ) || t.isVCL(typ) && t.isFirstSlice(typ, n) && t.splitsOnSlice()
}

// countAUs counts the access units that start in a PES.
func (t *track) countAUs(nalus [][]byte) int {
	n, slice, started := 0, false, false
	for _, nal := range nalus {
		if len(nal) == 0 || (t.codec == media.CodecH265 && len(nal) < 2) {
			continue
		}
		typ := t.nalType(nal)
		if t.startsAU(typ, nal) && (!started || slice) {
			n++
			started, slice = true, false
		}
		if t.isVCL(typ) {
			slice = true
		}
	}
	return n
}

// handleVideo adds a video PES to its track's access units. A PES with a PTS
// starts an access unit and completes the pending one. A PES without PTS
// continues the pending unit. Inside either, an AUD or (see splitsOnSlice) a
// first slice after a slice starts another access unit, a split unit, timed
// by releaseGroup once the next PES with a PTS arrives. A field-coded H.264
// track never splits (see fieldCoded).
//
// A Discontinuity counts one lost access unit for the lost data. On a track
// that carries PES without PTS the pending unit may be missing data, so it is
// dropped and counted too; on a track whose PES all have a PTS the lost data
// was whole PES and the pending unit is complete. A PES without PTS after a
// loss is dropped, with the units starting in it, until the next PES with a
// PTS. Later frames pass through: decoders conceal the loss.
func (d *Demuxer) handleVideo(t *track, pes *mpegts.PES) {
	if !pes.HasPTS {
		t.ptsLess = true
	}
	var pts, dts int64
	if pes.HasPTS {
		pts, dts = d.decodeTimes(pes)
	}
	if pes.Discontinuity {
		d.stats.LostFrames++
		if t.ptsLess && d.finishAU(t, false) {
			d.stats.LostFrames++
		}
		// The group waits for the next PES with a PTS, which times it with
		// the last step clamped below its DTS: units were lost in between,
		// so neither even spacing nor a step measurement is valid.
		t.groupLoss = true
		if !pes.HasPTS {
			t.lost = true
		}
	}
	nalus := avc.ExtractNalusFromByteStream(pes.Data)
	switch {
	case pes.HasPTS:
		d.finishAU(t, true)
		d.releaseGroup(t, dts, true, !t.groupLoss)
		t.lost = false
		t.startPES(pts, dts, pes.PTS == pes.DTS)
	case t.lost:
		d.stats.LostFrames += uint64(t.countAUs(nalus))
		return
	case t.au == nil:
		// Nothing to continue: data before the next access-unit start is
		// discarded.
		d.stats.DecodeErrors++
	default:
		d.stats.Continued++
	}
	for _, n := range nalus {
		if len(n) == 0 || (t.codec == media.CodecH265 && len(n) < 2) {
			continue
		}
		d.addNAL(t, n)
	}
}

// startPES starts the access unit of a PES with a PTS, unit 0 of a new group.
func (t *track) startPES(pts, dts int64, ptsIsDTS bool) {
	t.timed = true
	t.pesDTS, t.pesAUs, t.ptsIsDTS = dts, 1, ptsIsDTS
	t.groupLoss = false
	t.au = &videoAU{pts: pts, dts: dts}
}

// startSplitAU starts a split access unit: one found inside a PES after its
// first, or in a PES without PTS. Its presentation order is known only when
// the last PES with a PTS had PTS == DTS; otherwise it is dropped.
func (d *Demuxer) startSplitAU(t *track) {
	au := &videoAU{idx: t.pesAUs}
	t.pesAUs++
	switch {
	case !t.timed:
		au.drop = true // Counted with the PES that continues nothing.
	case !t.ptsIsDTS:
		au.drop = true
		d.stats.SplitAUsReordered++
	}
	t.au = au
}

// releaseGroup times and queues the completed split units of the group that
// started at the last PES with a PTS (pesDTS, pesAUs units). next is the DTS
// of the next PES with a PTS when haveNext. Unit i of N gets
// pesDTS + i*(next-pesDTS)/N when even spacing is allowed (no loss between)
// and the gap per unit is within maxSplitStep; that gap becomes the track's
// step. Otherwise unit i gets pesDTS + i*step (defaultFrameStep if none),
// and a unit whose DTS would reach next, or not pass the last DTS queued on
// the track, has no room and is dropped (SplitAUsNoRoom). PTS = DTS.
func (d *Demuxer) releaseGroup(t *track, next int64, haveNext, even bool) {
	n := int64(t.pesAUs)
	gap := next - t.pesDTS
	spaced := even && haveNext && t.timed && n > 0 && gap > 0 && gap/n <= maxSplitStep
	if spaced {
		t.step = gap / n
	}
	step := t.step
	if step <= 0 {
		step = defaultFrameStep
	}
	for _, au := range t.group {
		i := int64(au.idx)
		var dts int64
		if spaced {
			dts = t.pesDTS + i*gap/n
		} else {
			dts = t.pesDTS + i*step
		}
		if (haveNext && dts >= next) || (t.haveOut && dts <= t.lastOut) {
			d.stats.SplitAUsNoRoom++
			continue
		}
		au.pts, au.dts = dts, dts
		d.stats.SplitAUs++
		d.queueAU(t, au)
	}
	t.group, t.groupSize = nil, 0
}

// addNAL adds one NAL unit to the pending access unit. Parameter sets and
// prefix SEI after a slice are held until the next NAL unit shows whether
// they start the next access unit.
func (d *Demuxer) addNAL(t *track, n []byte) {
	typ := t.nalType(n)
	aud := t.isAUD(typ)
	vcl := t.isVCL(typ)
	if t.startsAU(typ, n) && (t.au == nil || t.au.slice) {
		held := t.held
		t.held = nil
		if aud {
			// Units before an AUD end the pending access unit.
			d.commit(t, held)
			held = nil
		}
		d.finishAU(t, true)
		d.startSplitAU(t)
		d.commit(t, held)
	}
	if au := t.au; au != nil && !aud {
		au.size += len(n)
		if au.size > maxAUSize && !au.drop {
			d.stats.DecodeErrors++
			au.drop, au.data = true, nil
		}
	}
	switch {
	case aud:
	case t.au == nil:
		// Data of an access unit no longer pending; parameter sets still
		// apply.
		if t.paramSets(typ) != nil {
			d.commitNAL(t, n)
		}
	case vcl:
		d.commit(t, t.held)
		t.held = nil
		d.commitNAL(t, n)
	case t.au.slice && !t.au.drop && t.leadsAU(typ):
		t.held = append(t.held, n)
	default:
		d.commitNAL(t, n)
	}
}

func (d *Demuxer) commit(t *track, nalus [][]byte) {
	for _, n := range nalus {
		d.commitNAL(t, n)
	}
}

// commitNAL stores a parameter set, or appends any other NAL unit to the
// pending access unit.
func (d *Demuxer) commitNAL(t *track, n []byte) {
	typ := t.nalType(n)
	if sets := t.paramSets(typ); sets != nil {
		id, err := media.ParamSetID(t.codec, n)
		if err != nil {
			// The slices that follow may depend on this set, so the
			// current config can no longer be trusted.
			d.stats.DecodeErrors++
			t.config = nil
			t.bad = true
			return
		}
		if old, have := sets[id]; !have || !bytes.Equal(old, n) {
			sets[id] = bytes.Clone(n)
			t.dirty = true
			if t.codec == media.CodecH264 && typ == 7 {
				t.noteFieldCoding(id, n)
			}
		}
		if t.config == nil {
			t.dirty = true // Retry the rebuild whenever a set arrives while invalid.
		}
		return
	}
	au := t.au
	if au == nil {
		return
	}
	if t.isVCL(typ) {
		au.slice = true
	}
	if !au.drop {
		au.data = append(au.data, n)
	}
}

// noteFieldCoding records whether an H.264 SPS allows field coding. An SPS
// that does not parse is treated as field-coded, the safe side for
// splitsOnSlice.
func (t *track) noteFieldCoding(id uint32, sps []byte) {
	parsed, err := avc.ParseSPSNALUnit(sps, false)
	if err == nil && parsed.FrameMbsOnlyFlag {
		delete(t.fieldSPS, id)
		return
	}
	if t.fieldSPS == nil {
		t.fieldSPS = map[uint32]bool{}
	}
	t.fieldSPS[id] = true
}

// finishAU completes the pending access unit: the parameter sets it carried
// update the track's config. With emit, unit 0 of a group is queued and a
// split unit joins the group; without, it reports whether a frame that would
// have been queued was discarded.
func (d *Demuxer) finishAU(t *track, emit bool) (discarded bool) {
	au := t.au
	if au == nil {
		return false
	}
	d.commit(t, t.held)
	t.held = nil
	t.au = nil
	switch {
	case t.bad:
		t.bad, t.dirty = false, false
	case t.dirty:
		t.dirty = false
		d.rebuildVideoConfig(t)
	}
	if au.drop || len(au.data) == 0 {
		return false
	}
	if t.config == nil {
		if emit {
			d.stats.DroppedNoConfig++
		}
		return false
	}
	if !emit {
		return true
	}
	au.config = t.config
	if au.idx == 0 {
		d.queueAU(t, au)
		return false
	}
	if t.groupSize+au.size > maxGroupSize {
		// Bound what waits for the next PES: time the group now.
		d.releaseGroup(t, 0, false, false)
	}
	t.group = append(t.group, au)
	t.groupSize += au.size
	return false
}

// queueAU queues a timed access unit.
func (d *Demuxer) queueAU(t *track, au *videoAU) {
	t.lastOut, t.haveOut = au.dts, true
	key, rasl := t.cls.Classify(au.data, au.pts)
	d.enqueue(media.Frame{
		Track:  t.idx,
		Config: au.config,
		PTS:    au.pts,
		DTS:    au.dts,
		Key:    key,
		RASL:   rasl,
		Data:   au.data,
	})
}

// flushTrack queues a track's pending access unit and split units, without
// a next DTS.
func (d *Demuxer) flushTrack(t *track) {
	d.finishAU(t, true)
	d.releaseGroup(t, 0, false, false)
}

// flushAll flushes every track.
func (d *Demuxer) flushAll() {
	for _, t := range d.tracks {
		d.flushTrack(t)
	}
}

func collect(m map[uint32][]byte) [][]byte {
	out := make([][]byte, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	return out
}

func (d *Demuxer) rebuildVideoConfig(t *track) {
	if len(t.sps) == 0 || len(t.pps) == 0 || (t.codec == media.CodecH265 && len(t.vps) == 0) {
		return
	}
	var (
		cfg *media.TrackConfig
		err error
	)
	if t.codec == media.CodecH265 {
		cfg, err = media.NewH265Config(collect(t.vps), collect(t.sps), collect(t.pps))
	} else {
		cfg, err = media.NewH264Config(collect(t.sps), collect(t.pps))
	}
	if err != nil {
		// The stored sets no longer match the current config: drop frames
		// until a parameter set arrives that rebuilds successfully.
		d.stats.DecodeErrors++
		t.config = nil
		return
	}
	if cfg.Equal(t.good) {
		// Unchanged, or repaired back to the last valid config: keep its
		// pointer, since a new pointer means a configuration change.
		t.config = t.good
		return
	}
	t.config, t.good = cfg, cfg
}
