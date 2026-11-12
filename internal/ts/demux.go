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
	// packets and packet runs, PES without a PTS, and bad parameter sets and
	// ADTS frames.
	DecodeErrors uint64
	// PMTChanges counts PMTs after the first whose (PID, stream_type) set
	// differed from the current one. It counts only in Remap mode.
	PMTChanges uint64
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
			delete(d.byPID, s.PID)
			if !supported {
				d.skip(s)
			}
		}
	}
}

func newTrack(idx int, s mpegts.Stream, info streamInfo) *track {
	return &track{
		idx: idx, pid: s.PID, stype: s.Type, active: true, kind: info.kind, codec: info.codec,
		vps: map[uint32][]byte{}, sps: map[uint32][]byte{}, pps: map[uint32][]byte{},
	}
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
	}
	d.curSet = slices.Clone(pmt.Streams)
	if d.curSet == nil {
		d.curSet = []mpegts.Stream{}
	}
	next := make([]bool, len(d.tracks), d.maxTrks)
	d.byPID = map[uint16]*track{}
	ordinal := map[media.Kind]int{}
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
			t = newTrack(slot, s, info)
			d.tracks[slot] = t
			next[slot] = true
		}
		d.byPID[s.PID] = t
	}
	for i, t := range d.tracks {
		t.active = next[i]
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

// Read returns the next frame, or io.EOF at the end of the stream. A read
// error from the input is returned wrapped, and again on later calls.
func (d *Demuxer) Read() (media.Frame, error) {
	for {
		if len(d.queue) > 0 {
			f := d.queue[0]
			d.queue = d.queue[1:]
			return f, nil
		}
		if d.err != nil {
			return media.Frame{}, d.err
		}
		ev, err := d.dmx.Next()
		switch {
		case errors.Is(err, io.EOF):
			d.err = io.EOF
			continue
		case err != nil:
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

func (d *Demuxer) handlePES(t *track, pes *mpegts.PES) {
	if !pes.HasPTS {
		d.stats.DecodeErrors++
		return
	}
	pts := d.timeDec.Decode(pes.PTS)
	dts := pts
	if pes.DTS != pes.PTS {
		dts = d.timeDec.Decode(pes.DTS)
	}
	if t.kind == media.KindAudio {
		d.handleAudio(t, pes.Data, pts)
		return
	}
	d.handleVideo(t, pes.Data, pts, dts)
}

func (d *Demuxer) handleAudio(t *track, payload []byte, pts int64) {
	frames, err := mpegts.ParseADTS(payload)
	if err != nil {
		d.stats.DecodeErrors++
	}
	for i, a := range frames {
		asc := a.AudioSpecificConfig()
		if t.config == nil || !bytes.Equal(t.config.ASC, asc) {
			cfg, cerr := media.NewAACConfig(asc)
			if cerr != nil {
				d.stats.DecodeErrors++
				continue
			}
			t.config = cfg
		}
		ts := pts + media.SampleTime(int64(i)*1024, t.config.SampleRate)
		d.queue = append(d.queue, media.Frame{
			Track:  t.idx,
			Config: t.config,
			PTS:    ts,
			DTS:    ts,
			Key:    true,
			Data:   [][]byte{a.Payload},
		})
	}
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

func (t *track) isKeySlice(typ uint8) bool {
	if t.codec == media.CodecH265 {
		return typ >= 16 && typ <= 21
	}
	return typ == 5
}

func (t *track) isRASL(typ uint8) bool {
	return t.codec == media.CodecH265 && (typ == 8 || typ == 9)
}

func (d *Demuxer) handleVideo(t *track, payload []byte, pts, dts int64) {
	nalus := avc.ExtractNalusFromByteStream(payload)
	var (
		data  [][]byte
		key   bool
		rasl  bool
		dirty bool
		bad   bool
	)
	for _, n := range nalus {
		if len(n) == 0 || (t.codec == media.CodecH265 && len(n) < 2) {
			continue
		}
		typ := t.nalType(n)
		if t.isAUD(typ) {
			continue
		}
		if sets := t.paramSets(typ); sets != nil {
			id, err := media.ParamSetID(t.codec, n)
			if err != nil {
				// The slices that follow may depend on this set, so the
				// current config can no longer be trusted.
				d.stats.DecodeErrors++
				t.config = nil
				bad = true
				continue
			}
			if old, have := sets[id]; !have || !bytes.Equal(old, n) {
				sets[id] = bytes.Clone(n)
				dirty = true
			}
			if t.config == nil {
				dirty = true // Retry the rebuild whenever a set arrives while invalid.
			}
			continue
		}
		if t.isKeySlice(typ) {
			key = true
		}
		if t.isRASL(typ) {
			rasl = true
		}
		data = append(data, n)
	}
	if dirty && !bad {
		d.rebuildVideoConfig(t)
	}
	if len(data) == 0 {
		return
	}
	if t.config == nil {
		d.stats.DroppedNoConfig++
		return
	}
	d.queue = append(d.queue, media.Frame{
		Track:  t.idx,
		Config: t.config,
		PTS:    pts,
		DTS:    dts,
		Key:    key,
		RASL:   rasl,
		Data:   data,
	})
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
