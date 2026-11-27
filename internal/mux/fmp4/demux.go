package fmp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// maxMediaTime bounds decode times and edit-list media times, in track
// timescale units, so that sums with composition offsets stay inside int64.
const maxMediaTime = 1 << 62

// Bound on the samples one segment may declare. Every sample that carries a
// frame needs at least one byte of mdat, so a segment may declare at most one
// sample per byte of input plus maxSampleSlack, and never more than
// maxSegmentSamples. The bound is checked on the raw trun headers before
// mp4ff decodes a moof, so a small hostile segment cannot make it allocate
// per declared sample.
const (
	maxSampleSlack    = 1024
	maxSegmentSamples = 1 << 20
)

// errTimeRange reports a time that does not fit the 90 kHz source timeline.
var errTimeRange = errors.New("time out of range")

// DemuxStats counts what a Demuxer dropped.
type DemuxStats struct {
	// DroppedNoConfig counts video frames dropped while their track had no
	// complete, valid set of parameter sets.
	DroppedNoConfig uint64
	// DroppedPTSBeforeDTS counts frames whose PTS stayed below their DTS
	// after the track's composition shift.
	DroppedPTSBeforeDTS uint64
	// DecodeErrors counts malformed video samples and parameter sets that
	// have no readable ID or do not build a config.
	DecodeErrors uint64
	// DroppedEmpty counts samples that carry no frame: size 0, or video
	// samples holding only parameter sets and access unit delimiters.
	DroppedEmpty uint64
	// ClampedOverlaps counts trafs whose decode time started before the end
	// of the previous traf of their track in the segment, by at most their
	// first sample's duration, and were moved to that end.
	ClampedOverlaps uint64
}

// Demuxer reads frames from the media segments of a fragmented MP4 stream.
// It keeps every H.264, H.265 and AAC track of one init segment; Frame.Track
// is the track's position among the kept tracks. It is not safe for
// concurrent use.
type Demuxer struct {
	tracks  []*track
	byID    map[uint32]*track
	trex    map[uint32]*mp4.TrexBox // every track's defaults, kept or not
	skipped []string
	cache   media.ConfigCache
	stats   DemuxStats
}

// track is one kept track of the init segment.
type track struct {
	idx       int
	id        uint32
	codec     media.Codec
	timescale uint32
	nalLen    int // NAL unit length field size, video only
	// edit is the media time of the first non-empty edit, subtracted from
	// PTS and DTS.
	edit int64
	// shift moves DTS down by the most negative composition offset of the
	// track's first segment; set once, when shiftSet becomes true.
	shift    int64
	shiftSet bool
	// next is the decode time after the track's last sample, used by a traf
	// without tfdt.
	next uint64
	// config is nil while a video track has no usable parameter sets.
	config        *media.TrackConfig
	vps, sps, pps map[uint32][]byte // video parameter sets by ID, newest wins
	cls           *media.NALClassifier
}

// NewDemuxer reads an init segment. Every H.264, H.265 and AAC track becomes
// a track of the demuxer; any other track is skipped with a reason (see
// Skipped). An init without a usable track is an error.
func NewDemuxer(init []byte) (*Demuxer, error) {
	d, err := newDemuxer(init)
	if err != nil {
		return nil, fmt.Errorf("fmp4: demux init: %w", err)
	}
	return d, nil
}

func newDemuxer(init []byte) (*Demuxer, error) {
	refs, err := walkBoxes(init)
	if err != nil {
		return nil, err
	}
	var moov *mp4.MoovBox
	for _, r := range refs {
		if r.typ != "moov" {
			continue
		}
		box, err := decodeBox(init, r)
		if err != nil {
			return nil, err
		}
		var ok bool
		if moov, ok = box.(*mp4.MoovBox); !ok {
			return nil, errors.New("moov did not decode as a movie box")
		}
		break
	}
	if moov == nil {
		return nil, errors.New("no moov box")
	}
	d := &Demuxer{byID: map[uint32]*track{}, trex: map[uint32]*mp4.TrexBox{}}
	if moov.Mvex != nil {
		for _, trex := range moov.Mvex.Trexs {
			d.trex[trex.TrackID] = trex
		}
	}
	for _, trak := range moov.Traks {
		if reason := d.addTrack(trak); reason != "" {
			d.skipped = append(d.skipped, reason)
		}
	}
	if len(d.tracks) == 0 {
		return nil, fmt.Errorf("no supported track (%s)", strings.Join(d.skipped, "; "))
	}
	return d, nil
}

// addTrack keeps trak, or returns why it is skipped.
func (d *Demuxer) addTrack(trak *mp4.TrakBox) string {
	if trak.Tkhd == nil || trak.Mdia == nil || trak.Mdia.Mdhd == nil || trak.Mdia.Minf == nil ||
		trak.Mdia.Minf.Stbl == nil || trak.Mdia.Minf.Stbl.Stsd == nil {
		return "track: incomplete trak box"
	}
	id := trak.Tkhd.TrackID
	name := fmt.Sprintf("track %d", id)
	if _, dup := d.byID[id]; dup {
		return name + ": duplicate track ID"
	}
	t := &track{idx: len(d.tracks), id: id, timescale: trak.Mdia.Mdhd.Timescale}
	if t.timescale == 0 {
		return name + ": zero timescale"
	}
	edit, ok := editTime(trak.Edts)
	if !ok {
		return name + ": edit list media time out of range"
	}
	t.edit = edit
	stsd := trak.Mdia.Minf.Stbl.Stsd
	var sets [][]byte
	switch {
	case stsd.AvcX != nil:
		if stsd.AvcX.AvcC == nil {
			return name + ": " + stsd.AvcX.Type() + " without avcC"
		}
		t.codec, t.nalLen = media.CodecH264, 4 // mp4ff decodes only 4-byte AVC lengths
		sets = append(append(sets, stsd.AvcX.AvcC.SPSnalus...), stsd.AvcX.AvcC.PPSnalus...)
	case stsd.HvcX != nil:
		if stsd.HvcX.HvcC == nil {
			return name + ": " + stsd.HvcX.Type() + " without hvcC"
		}
		rec := &stsd.HvcX.HvcC.DecConfRec
		t.codec, t.nalLen = media.CodecH265, int(rec.LengthSizeMinusOne)+1
		if t.nalLen == 3 {
			return name + ": invalid NAL unit length size 3"
		}
		for _, typ := range []hevc.NaluType{hevc.NALU_VPS, hevc.NALU_SPS, hevc.NALU_PPS} {
			sets = append(sets, rec.GetNalusForType(typ)...)
		}
	case stsd.Mp4a != nil:
		cfg, reason := aacConfig(stsd.Mp4a)
		if reason != "" {
			return name + ": " + reason
		}
		t.codec, t.config = media.CodecAAC, d.cache.Stable(t.idx, cfg)
	default:
		entry := "none"
		if len(stsd.Children) > 0 {
			entry = stsd.Children[0].Type()
		}
		return fmt.Sprintf("%s: unsupported sample entry %q", name, entry)
	}
	if t.codec.Kind() == media.KindVideo {
		t.vps, t.sps, t.pps = map[uint32][]byte{}, map[uint32][]byte{}, map[uint32][]byte{}
		t.cls = media.NewNALClassifier(t.codec)
		if len(sets) > 0 {
			d.takeSets(t, sets)
		}
	}
	d.tracks = append(d.tracks, t)
	d.byID[id] = t
	return ""
}

// aacConfig builds the config of an mp4a sample entry, or returns why it
// cannot.
func aacConfig(e *mp4.AudioSampleEntryBox) (*media.TrackConfig, string) {
	if e.Esds == nil || e.Esds.DecConfigDescriptor == nil || e.Esds.DecConfigDescriptor.DecSpecificInfo == nil {
		return nil, "mp4a without decoder specific info"
	}
	// 0x40: MPEG-4 audio; 0x66 to 0x68: MPEG-2 AAC Main, LC and SSR.
	switch oti := e.Esds.DecConfigDescriptor.ObjectType; oti {
	case 0x40, 0x66, 0x67, 0x68:
	default:
		return nil, fmt.Sprintf("mp4a object type indication 0x%02x is not AAC", oti)
	}
	cfg, err := media.NewAACConfig(e.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig)
	if err != nil {
		return nil, err.Error()
	}
	return cfg, ""
}

// editTime returns the media time of the first non-empty edit (0 without an
// edit list), and false when it is out of range.
func editTime(edts *mp4.EdtsBox) (int64, bool) {
	if edts == nil {
		return 0, true
	}
	for _, elst := range edts.Elst {
		for _, e := range elst.Entries {
			if e.MediaTime >= 0 {
				return e.MediaTime, e.MediaTime <= maxMediaTime
			}
		}
	}
	return 0, true
}

// Kinds returns the media kind of every track, by track index.
func (d *Demuxer) Kinds() []media.Kind {
	out := make([]media.Kind, len(d.tracks))
	for i, t := range d.tracks {
		out[i] = t.codec.Kind()
	}
	return out
}

// Skipped returns why each skipped track of the init segment was skipped.
func (d *Demuxer) Skipped() []string { return append([]string(nil), d.skipped...) }

// Stats returns the drop counters.
func (d *Demuxer) Stats() DemuxStats { return d.stats }

// Segment reads one media segment: any number of moof boxes, each followed
// by the mdat boxes holding its samples (other top-level boxes are ignored).
// It returns the frames of every track merged in DTS order (ties in track
// order), with 90 kHz source timestamps:
//
//   - A sample's decode time is its traf's tfdt plus the durations of the
//     samples before it in that traf (the previous traf's end without a
//     tfdt), rescaled to 90 kHz as one value, never summed per sample.
//   - PTS is the decode time plus the composition offset (signed in trun
//     version 1). The media time of the track's first non-empty edit is
//     subtracted from both. On top of that, DTS moves down by the most
//     negative composition offset of the track's first segment with
//     samples, a shift fixed for the demuxer's life; a later frame whose PTS
//     is still below its DTS is dropped (DemuxStats.DroppedPTSBeforeDTS).
//
// Video samples are split into NAL units; parameter sets move into the
// frame's Config and access unit delimiters are dropped. Key and RASL come
// from a media.NALClassifier, and configs go through a media.ConfigCache so
// that an unchanged config keeps its pointer. Samples without frame data
// (size 0, or video holding only parameter sets) are counted in
// DemuxStats.DroppedEmpty. A traf starting before the end of the previous
// traf of its track in the segment by at most its first sample's duration
// is moved to that end (DemuxStats.ClampedOverlaps). A segment declaring
// more samples than one per byte plus maxSampleSlack (at most
// maxSegmentSamples), a traf starting earlier than that, or a sample of a
// kept track outside every mdat is an error, no frame of the segment is
// returned and no counter changes; the samples of a skipped track are never
// read, so their positions are not checked. Frames do not alias b.
func (d *Demuxer) Segment(b []byte) ([]media.Frame, error) {
	frames, err := d.segment(bytes.Clone(b))
	if err != nil {
		return nil, fmt.Errorf("fmp4: demux segment: %w", err)
	}
	return frames, nil
}

// sample is one sample as the segment describes it, in track timescale.
type sample struct {
	dts  uint64
	cto  int64
	data []byte
}

// timed is a sample with its 90 kHz timestamps.
type timed struct {
	pts, dts int64
	drop     bool // PTS < DTS after the shift
	data     []byte
}

func (d *Demuxer) segment(b []byte) ([]media.Frame, error) {
	refs, err := walkBoxes(b)
	if err != nil {
		return nil, err
	}
	limit := min(uint64(len(b))+maxSampleSlack, maxSegmentSamples)
	var declared uint64
	for _, r := range refs {
		if r.typ != "moof" {
			continue
		}
		n, err := countSamples(b[r.body:r.end])
		if err != nil {
			return nil, fmt.Errorf("moof at %d: %w", r.start, err)
		}
		if declared += n; declared > limit {
			return nil, fmt.Errorf("segment of %d bytes declares more than %d samples", len(b), limit)
		}
	}
	var (
		moofs []*mp4.MoofBox
		mdats []boxRef
	)
	for _, r := range refs {
		switch r.typ {
		case "moof":
			box, err := decodeBox(b, r)
			if err != nil {
				return nil, err
			}
			moof, ok := box.(*mp4.MoofBox)
			if !ok {
				return nil, errors.New("moof did not decode as a movie fragment box")
			}
			moofs = append(moofs, moof)
		case "mdat":
			mdats = append(mdats, r)
		}
	}

	// Read every sample first: nothing changes the demuxer's state unless
	// the whole segment is valid.
	rd := segReader{
		b:     b,
		mdats: mdats,
		raw:   make([][]sample, len(d.tracks)),
		next:  make([]uint64, len(d.tracks)),
		seen:  make([]bool, len(d.tracks)),
	}
	for i, t := range d.tracks {
		rd.next[i] = t.next
	}
	for _, moof := range moofs {
		if err := d.readMoof(&rd, moof); err != nil {
			return nil, err
		}
	}
	raw, next := rd.raw, rd.next
	times := make([][]timed, len(d.tracks))
	shifts := make([]int64, len(d.tracks))
	for i, t := range d.tracks {
		if times[i], shifts[i], err = t.times(raw[i]); err != nil {
			return nil, fmt.Errorf("track %d: %w", t.id, err)
		}
	}

	d.stats.DroppedEmpty += rd.empty
	d.stats.ClampedOverlaps += rd.clamped
	per := make([][]media.Frame, len(d.tracks))
	for i, t := range d.tracks {
		t.next = next[i]
		if len(raw[i]) > 0 {
			t.shift, t.shiftSet = shifts[i], true
		}
		per[i] = d.frames(t, times[i])
	}
	return merge(per), nil
}

// countSamples returns the sample count the trun boxes of a moof payload
// declare, read from the raw box headers.
func countSamples(moof []byte) (uint64, error) {
	children, err := walkBoxes(moof)
	if err != nil {
		return 0, err
	}
	var n uint64
	for _, c := range children {
		if c.typ != "traf" {
			continue
		}
		boxes, err := walkBoxes(moof[c.body:c.end])
		if err != nil {
			return 0, fmt.Errorf("traf: %w", err)
		}
		for _, r := range boxes {
			if r.typ != "trun" {
				continue
			}
			if r.end-r.body < 8 {
				return 0, errors.New("traf: trun shorter than its header")
			}
			n += uint64(binary.BigEndian.Uint32(moof[c.body+r.body+4:])) // after version and flags
		}
	}
	return n, nil
}

// segReader collects the samples of one segment before any of them changes
// the demuxer's state.
type segReader struct {
	b       []byte
	mdats   []boxRef
	raw     [][]sample // per track, samples with data
	next    []uint64   // per track, decode time after the last sample
	seen    []bool     // per track, a traf was read in this segment
	empty   uint64     // size-0 samples of kept tracks
	clamped uint64     // trafs moved to the previous traf's end
}

// readMoof appends the samples of every traf of a kept track to rd.raw and
// advances rd.next. Size-0 samples only advance the decode time.
func (d *Demuxer) readMoof(rd *segReader, moof *mp4.MoofBox) error {
	b := rd.b
	var prevEnd int64 // end of the previous traf's data
	for i, traf := range moof.Trafs {
		tfhd := traf.Tfhd
		if tfhd == nil {
			return errors.New("traf without tfhd")
		}
		t := d.byID[tfhd.TrackID]
		trex := d.trex[tfhd.TrackID]

		// ISO/IEC 14496-12 8.8.7.1: the base is the explicit offset, else
		// the moof for default-base-is-moof and for the first traf, else
		// the end of the previous traf's data.
		var base int64
		switch {
		case tfhd.HasBaseDataOffset():
			if tfhd.BaseDataOffset > uint64(len(b)) {
				if t == nil {
					// A skipped track's data is never read: only a later
					// traf relying on its end can fail.
					prevEnd = -1
					continue
				}
				return fmt.Errorf("track %d: base data offset %d beyond the segment", tfhd.TrackID, tfhd.BaseDataOffset)
			}
			base = int64(tfhd.BaseDataOffset)
		case tfhd.DefaultBaseIfMoof() || i == 0:
			base = int64(moof.StartPos)
		default:
			base = prevEnd
		}
		if t == nil {
			prevEnd = skippedEnd(traf, tfhd, trex, base)
			continue
		}
		pos := base
		dts := rd.next[t.idx]
		if traf.Tfdt != nil {
			dts = traf.Tfdt.BaseMediaDecodeTime()
		}
		if dts > maxMediaTime {
			return fmt.Errorf("track %d: decode time %d: %w", t.id, dts, errTimeRange)
		}
		if rd.seen[t.idx] && dts < rd.next[t.idx] {
			// A start up to one sample early is a rounding slip of the
			// packager: move it to the previous end.
			if rd.next[t.idx]-dts > uint64(firstDuration(traf, tfhd, trex)) {
				return fmt.Errorf("track %d: decode time goes backwards from %d to %d", t.id, rd.next[t.idx], dts)
			}
			dts = rd.next[t.idx]
			rd.clamped++
		}
		for _, trun := range traf.Truns {
			if trun.HasDataOffset() {
				pos = base + int64(trun.DataOffset)
			}
			for k, s := range trun.Samples {
				if pos < 0 || pos > int64(len(b)) {
					return fmt.Errorf("track %d: sample %d at %d outside the segment", tfhd.TrackID, k, pos)
				}
				dur, size := sampleDefaults(trun, tfhd, trex, s)
				if size == 0 {
					rd.empty++
				} else {
					if !inMdat(rd.mdats, pos, size) {
						return fmt.Errorf("track %d: sample %d (%d bytes at %d) outside every mdat", t.id, k, size, pos)
					}
					if dts > maxMediaTime {
						return fmt.Errorf("track %d: decode time %d: %w", t.id, dts, errTimeRange)
					}
					var cto int64
					if trun.HasSampleCompositionTimeOffset() {
						cto = int64(s.CompositionTimeOffset) // signed in version 1
						if trun.Version == 0 {
							cto = int64(uint32(s.CompositionTimeOffset))
						}
					}
					data := b[pos : pos+size : pos+size]
					rd.raw[t.idx] = append(rd.raw[t.idx], sample{dts: dts, cto: cto, data: data})
				}
				pos += size
				dts += uint64(dur)
			}
		}
		prevEnd = pos
		// dts started at most maxMediaTime and grew by at most
		// maxSegmentSamples durations of 32 bits: no wrap.
		if dts > maxMediaTime {
			return fmt.Errorf("track %d: decode time %d: %w", t.id, dts, errTimeRange)
		}
		rd.next[t.idx], rd.seen[t.idx] = dts, true
	}
	return nil
}

// skippedEnd returns the end of a skipped track's traf data, which starts at
// base, without checking any position: the samples are never read. The sum
// cannot wrap, as the segment declares at most maxSegmentSamples samples of
// at most 2^32 bytes.
func skippedEnd(traf *mp4.TrafBox, tfhd *mp4.TfhdBox, trex *mp4.TrexBox, base int64) int64 {
	pos := base
	for _, trun := range traf.Truns {
		if trun.HasDataOffset() {
			pos = base + int64(trun.DataOffset)
		}
		for _, s := range trun.Samples {
			_, size := sampleDefaults(trun, tfhd, trex, s)
			pos += size
		}
	}
	return pos
}

// firstDuration returns the duration of a traf's first sample, 0 when it
// has none.
func firstDuration(traf *mp4.TrafBox, tfhd *mp4.TfhdBox, trex *mp4.TrexBox) uint32 {
	for _, trun := range traf.Truns {
		if len(trun.Samples) > 0 {
			dur, _ := sampleDefaults(trun, tfhd, trex, trun.Samples[0])
			return dur
		}
	}
	return 0
}

// sampleDefaults returns a sample's duration and size: from the trun, else
// the tfhd defaults, else the trex defaults.
func sampleDefaults(trun *mp4.TrunBox, tfhd *mp4.TfhdBox, trex *mp4.TrexBox, s mp4.Sample) (dur uint32, size int64) {
	switch {
	case trun.HasSampleDuration():
		dur = s.Dur
	case tfhd.HasDefaultSampleDuration():
		dur = tfhd.DefaultSampleDuration
	case trex != nil:
		dur = trex.DefaultSampleDuration
	}
	var sz uint32
	switch {
	case trun.HasSampleSize():
		sz = s.Size
	case tfhd.HasDefaultSampleSize():
		sz = tfhd.DefaultSampleSize
	case trex != nil:
		sz = trex.DefaultSampleSize
	}
	return dur, int64(sz)
}

// inMdat reports whether size bytes at pos lie inside one mdat payload.
func inMdat(mdats []boxRef, pos, size int64) bool {
	for _, m := range mdats {
		if pos >= int64(m.body) && pos <= int64(m.end) && size <= int64(m.end)-pos {
			return true
		}
	}
	return false
}

// times converts a track's samples of one segment to 90 kHz timestamps. It
// returns the DTS shift in force: the track's, or for its first segment the
// one this segment sets.
func (t *track) times(ss []sample) ([]timed, int64, error) {
	if len(ss) == 0 {
		return nil, t.shift, nil
	}
	shift := t.shift
	if !t.shiftSet {
		for _, s := range ss {
			shift = max(shift, -s.cto)
		}
	}
	out := make([]timed, len(ss))
	for i, s := range ss {
		dts := int64(s.dts) - t.edit - shift
		pts := int64(s.dts) + s.cto - t.edit
		d90, err := rescale(dts, t.timescale)
		if err != nil {
			return nil, 0, err
		}
		p90, err := rescale(pts, t.timescale)
		if err != nil {
			return nil, 0, err
		}
		out[i] = timed{pts: p90, dts: d90, drop: pts < dts, data: s.data}
	}
	return out, shift, nil
}

// rescale converts v in timescale units to 90 kHz ticks, rounded half away
// from zero, with a 128-bit intermediate product. |v| must be below 2^63.
func rescale(v int64, timescale uint32) (int64, error) {
	a := uint64(v)
	if v < 0 {
		a = uint64(-v)
	}
	hi, lo := bits.Mul64(a, media.ClockRate)
	lo, carry := bits.Add64(lo, uint64(timescale/2), 0)
	hi += carry
	if hi >= uint64(timescale) {
		return 0, errTimeRange
	}
	q, _ := bits.Div64(hi, lo, uint64(timescale))
	if q > math.MaxInt64 {
		return 0, errTimeRange
	}
	if v < 0 {
		return -int64(q), nil
	}
	return int64(q), nil
}

// frames turns a track's timed samples into frames.
func (d *Demuxer) frames(t *track, ss []timed) []media.Frame {
	out := make([]media.Frame, 0, len(ss))
	for _, s := range ss {
		if t.codec.Kind() == media.KindAudio {
			if s.drop {
				d.stats.DroppedPTSBeforeDTS++
			} else {
				out = append(out, media.Frame{
					Track: t.idx, Config: t.config, PTS: s.pts, DTS: s.dts, Key: true, Data: [][]byte{s.data},
				})
			}
			continue
		}
		nalus, err := splitNALUs(s.data, t.nalLen)
		if err != nil {
			d.stats.DecodeErrors++
			continue
		}
		// Parameter sets apply even when the frame itself is dropped.
		data := d.takeSets(t, nalus)
		switch {
		case len(data) == 0:
			d.stats.DroppedEmpty++
		case s.drop:
			d.stats.DroppedPTSBeforeDTS++
		case t.config == nil:
			d.stats.DroppedNoConfig++
		default:
			key, rasl := t.cls.Classify(data, s.pts)
			out = append(out, media.Frame{
				Track: t.idx, Config: t.config, PTS: s.pts, DTS: s.dts, Key: key, RASL: rasl, Data: data,
			})
		}
	}
	return out
}

// splitNALUs splits length-prefixed sample data into NAL units, skipping
// empty ones.
func splitNALUs(data []byte, size int) ([][]byte, error) {
	var out [][]byte
	for len(data) > 0 {
		if len(data) < size {
			return nil, errors.New("truncated NAL unit length")
		}
		n := 0
		for _, c := range data[:size] {
			n = n<<8 | int(c)
		}
		data = data[size:]
		if n > len(data) {
			return nil, fmt.Errorf("NAL unit length %d beyond the sample", n)
		}
		if n > 0 {
			out = append(out, data[:n:n])
		}
		data = data[n:]
	}
	return out, nil
}

// takeSets stores the parameter sets among nalus, rebuilding the track's
// config when they change, and returns the other NAL units except access
// unit delimiters. A set without a readable ID leaves the track without a
// config until a later set rebuilds one, as in the TS demuxer.
func (d *Demuxer) takeSets(t *track, nalus [][]byte) [][]byte {
	var data [][]byte
	dirty, bad := false, false
	for _, n := range nalus {
		typ := t.nalType(n)
		if t.isAUD(typ) {
			continue
		}
		sets := t.paramSets(typ)
		if sets == nil {
			data = append(data, n)
			continue
		}
		id, err := media.ParamSetID(t.codec, n)
		if err != nil {
			d.stats.DecodeErrors++
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
	}
	switch {
	case bad:
		t.config = nil
	case dirty:
		d.rebuildConfig(t)
	}
	return data
}

func (d *Demuxer) rebuildConfig(t *track) {
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
		d.stats.DecodeErrors++
		t.config = nil
		return
	}
	t.config = d.cache.Stable(t.idx, cfg)
}

func collect(m map[uint32][]byte) [][]byte {
	out := make([][]byte, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	return out
}

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

// paramSets returns the set map a parameter-set NAL type belongs to, or nil.
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

// merge interleaves per-track frames in DTS order, ties in track order,
// keeping each track's own order.
func merge(per [][]media.Frame) []media.Frame {
	total := 0
	for _, p := range per {
		total += len(p)
	}
	out := make([]media.Frame, 0, total)
	heads := make([]int, len(per))
	for len(out) < total {
		best := -1
		for i, p := range per {
			if heads[i] < len(p) && (best < 0 || p[heads[i]].DTS < per[best][heads[best]].DTS) {
				best = i
			}
		}
		out = append(out, per[best][heads[best]])
		heads[best]++
	}
	return out
}

// boxRef locates a top-level box: start of the box, of its payload, and its
// end, as offsets in the buffer.
type boxRef struct {
	typ              string
	start, body, end int
}

// walkBoxes lists the top-level boxes of b, which must hold whole boxes.
// Size 0 (to the end) is accepted only for mdat.
func walkBoxes(b []byte) ([]boxRef, error) {
	var out []boxRef
	for pos := 0; pos < len(b); {
		if len(b)-pos < 8 {
			return nil, fmt.Errorf("truncated box header at %d", pos)
		}
		size := uint64(binary.BigEndian.Uint32(b[pos:]))
		typ := string(b[pos+4 : pos+8])
		hdr := uint64(8)
		switch size {
		case 0:
			if typ != "mdat" {
				return nil, fmt.Errorf("box %q at %d: size 0 is accepted only for mdat", typ, pos)
			}
			size = uint64(len(b) - pos)
		case 1:
			if len(b)-pos < 16 {
				return nil, fmt.Errorf("box %q at %d: truncated large size", typ, pos)
			}
			size, hdr = binary.BigEndian.Uint64(b[pos+8:]), 16
		}
		if size < hdr || size > uint64(len(b)-pos) {
			return nil, fmt.Errorf("box %q at %d: size %d outside the %d bytes left", typ, pos, size, len(b)-pos)
		}
		out = append(out, boxRef{typ: typ, start: pos, body: pos + int(hdr), end: pos + int(size)})
		pos += int(size)
	}
	return out, nil
}

// decodeBox decodes one top-level box with mp4ff, turning a panic on
// hostile input into an error.
func decodeBox(b []byte, r boxRef) (box mp4.Box, err error) {
	defer func() {
		if p := recover(); p != nil {
			box, err = nil, fmt.Errorf("box %q at %d: decoder panic: %v", r.typ, r.start, p)
		}
	}()
	box, err = mp4.DecodeBox(uint64(r.start), bytes.NewReader(b[r.start:r.end]))
	if err != nil {
		return nil, fmt.Errorf("box %q at %d: %w", r.typ, r.start, err)
	}
	return box, nil
}
