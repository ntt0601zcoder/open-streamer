// Package ts demultiplexes MPEG-TS into media frames.
package ts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/asticode/go-astits"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// maxConsecutiveErrors is how many NextData errors in a row Read tolerates.
const maxConsecutiveErrors = 100

// DemuxStats counts events the demuxer recovered from.
type DemuxStats struct {
	// DroppedNoConfig counts video frames dropped before the first complete
	// parameter-set group.
	DroppedNoConfig uint64
	// DecodeErrors counts malformed packets, PES payloads and ADTS frames.
	DecodeErrors uint64
}

type streamInfo struct {
	kind  media.Kind
	codec media.Codec
}

var supportedTypes = map[astits.StreamType]streamInfo{
	astits.StreamTypeH264Video: {media.KindVideo, media.CodecH264},
	astits.StreamTypeH265Video: {media.KindVideo, media.CodecH265},
	astits.StreamTypeAACAudio:  {media.KindAudio, media.CodecAAC},
}

type track struct {
	pid    uint16
	kind   media.Kind
	codec  media.Codec
	config *media.TrackConfig // nil while the parameter sets are invalid
	good   *media.TrackConfig // the last valid video config, kept while config is nil

	// Video parameter sets, one per ID (the newest wins), keyed by ID.
	vps, sps, pps map[uint32][]byte
}

// Demuxer reads media frames from an MPEG-TS stream.
type Demuxer struct {
	dmx      *astits.Demuxer
	tracks   []*track
	byPID    map[uint16]*track
	skipped  []string
	timeDec  mpegts.TimeDecoder
	queue    []media.Frame
	stats    DemuxStats
	errsRun  int
	finished bool
}

// NewDemuxer reads r up to the first PMT and returns a demuxer for its
// supported tracks.
func NewDemuxer(r io.Reader) (*Demuxer, error) {
	d := &Demuxer{
		dmx:   astits.NewDemuxer(context.Background(), r),
		byPID: make(map[uint16]*track),
	}
	for {
		data, err := d.dmx.NextData()
		if err != nil {
			if isEOF(err) {
				return nil, errors.New("ts: demux: no supported track: no PMT before end of input")
			}
			return nil, fmt.Errorf("ts: demux: read: %w", err)
		}
		if data.PMT == nil {
			continue
		}
		d.setupTracks(data.PMT)
		if len(d.tracks) == 0 {
			return nil, errors.New("ts: demux: no supported track in PMT")
		}
		return d, nil
	}
}

func isEOF(err error) bool {
	return errors.Is(err, astits.ErrNoMorePackets) || errors.Is(err, io.EOF)
}

func (d *Demuxer) setupTracks(pmt *astits.PMTData) {
	for _, es := range pmt.ElementaryStreams {
		t := &track{pid: es.ElementaryPID}
		if info, ok := supportedTypes[es.StreamType]; ok {
			t.kind, t.codec = info.kind, info.codec
		} else {
			name := "unsupported"
			if es.StreamType == 0x03 || es.StreamType == 0x04 {
				name = "mpeg audio"
			}
			d.skipped = append(d.skipped, fmt.Sprintf("pid %d: %s (0x%02x)", es.ElementaryPID, name, uint8(es.StreamType)))
			continue
		}
		if _, dup := d.byPID[t.pid]; dup {
			continue
		}
		t.vps, t.sps, t.pps = map[uint32][]byte{}, map[uint32][]byte{}, map[uint32][]byte{}
		d.tracks = append(d.tracks, t)
		d.byPID[t.pid] = t
	}
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

// Skipped describes the PMT entries that are not demuxed.
func (d *Demuxer) Skipped() []string { return append([]string(nil), d.skipped...) }

// Stats returns the recovery counters.
func (d *Demuxer) Stats() DemuxStats { return d.stats }

// Read returns the next frame, or io.EOF at the end of the stream.
func (d *Demuxer) Read() (media.Frame, error) {
	for {
		if len(d.queue) > 0 {
			f := d.queue[0]
			d.queue = d.queue[1:]
			return f, nil
		}
		if d.finished {
			return media.Frame{}, io.EOF
		}
		data, err := d.dmx.NextData()
		if err != nil {
			if isEOF(err) {
				d.finished = true
				return media.Frame{}, io.EOF
			}
			d.stats.DecodeErrors++
			d.errsRun++
			if d.errsRun >= maxConsecutiveErrors {
				d.finished = true
				return media.Frame{}, fmt.Errorf("ts: demux: %d consecutive read errors, last: %w", d.errsRun, err)
			}
			continue
		}
		d.errsRun = 0
		if data.PES == nil {
			continue
		}
		t := d.byPID[data.PID]
		if t == nil {
			continue
		}
		d.handlePES(t, data.PES)
	}
}

func (d *Demuxer) trackIndex(t *track) int {
	for i, x := range d.tracks {
		if x == t {
			return i
		}
	}
	return -1
}

// timestamps returns the unwrapped PTS and DTS of a PES.
func (d *Demuxer) timestamps(pes *astits.PESData) (pts, dts int64, ok bool) {
	if pes.Header == nil || pes.Header.OptionalHeader == nil || pes.Header.OptionalHeader.PTS == nil {
		return 0, 0, false
	}
	oh := pes.Header.OptionalHeader
	pts = d.timeDec.Decode(oh.PTS.Base)
	dts = pts
	if oh.DTS != nil {
		dts = d.timeDec.Decode(oh.DTS.Base)
	}
	return pts, dts, true
}

func (d *Demuxer) handlePES(t *track, pes *astits.PESData) {
	pts, dts, ok := d.timestamps(pes)
	if !ok {
		d.stats.DecodeErrors++
		return
	}
	payload := bytes.Clone(pes.Data)
	if t.kind == media.KindAudio {
		d.handleAudio(t, payload, pts)
		return
	}
	d.handleVideo(t, payload, pts, dts)
}

func (d *Demuxer) handleAudio(t *track, payload []byte, pts int64) {
	frames, err := mpegts.ParseADTS(payload)
	if err != nil {
		d.stats.DecodeErrors++
	}
	idx := d.trackIndex(t)
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
			Track:  idx,
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
		Track:  d.trackIndex(t),
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
