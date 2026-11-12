package mpegts

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

const (
	// pesPrefixLen is packet_start_code_prefix, stream_id and
	// PES_packet_length.
	pesPrefixLen = 6
	// pesOptHeaderLen is pesPrefixLen plus the two flag bytes and
	// PES_header_data_length.
	pesOptHeaderLen = 9
	// defaultMaxPES bounds a length-0 PES that never sees its next PUSI.
	defaultMaxPES = 64 << 20
)

// PES is one complete PES packet of an elementary stream in the PMT.
type PES struct {
	PID           uint16
	PTS, DTS      int64 // raw 33-bit; DTS == PTS when the header has no DTS
	HasPTS        bool
	RandomAccess  bool   // from the PES's first packet
	Discontinuity bool   // data on this PID was lost before this PES
	Data          []byte // PES_packet_data_byte; owned by the caller
}

// Event is one demultiplexing result; exactly one field is set.
type Event struct {
	PMT *PMT
	PES *PES
}

// DemuxStats counts what a Demux has seen. Decoding errors are only counted,
// never returned.
type DemuxStats struct {
	Reader    ReaderStats
	CCErrors  uint64 // continuity_counter gaps on tracked PIDs
	PSIErrors uint64 // malformed PAT or PMT sections and bad CRCs
	PESErrors uint64 // unparseable PES headers and incomplete sized PES
}

// esState is the assembly state of one elementary-stream PID in the PMT.
type esState struct {
	typ  uint8
	pes  *pesBuf
	lost bool // a CC gap since the last PES start; marks the next PES
}

// pesBuf is a PES under assembly.
type pesBuf struct {
	raw   []byte
	known bool // the 6-byte prefix has been seen
	want  int  // total size from PES_packet_length; 0 for an unbounded PES
	ra    bool
	disc  bool
}

// Demux turns a transport stream into PMT and PES events for the first
// program of the PAT with a non-zero program_number (ISO/IEC 13818-1).
//
// Continuity is tracked on PID 0, the PMT PID and the PMT's elementary
// streams. A packet without payload must repeat the previous
// continuity_counter; a payload packet must advance it by one, and one that
// repeats it is a duplicate and is ignored. A gap counts one CCErrors, drops
// the PES or section in progress on that PID (the check runs before the
// payload_unit_start_indicator is handled) and marks the PID's next PES
// Discontinuity. A discontinuity_indicator restarts tracking without error:
// a payload packet carrying it becomes the new reference, and after an
// adaptation-only one the next packet does.
//
// A PES with a non-zero PES_packet_length is returned as soon as it is
// complete; one with length 0 at the next PES start on its PID, at EOF, or
// when its PID leaves the PMT. A Demux starts no goroutines and is not safe
// for concurrent use.
type Demux struct {
	r      *Reader
	stats  DemuxStats
	queue  []Event
	head   int
	eof    bool
	maxPES int

	havePMTPID bool
	program    uint16
	pmtPID     uint16
	// patSection and patVersion identify the PAT section the program was
	// chosen from.
	patSection uint8
	patVersion uint8
	pmt        *PMT
	es         map[uint16]*esState // the current PMT's streams
	cc         map[uint16]uint8    // last continuity_counter per tracked PID

	pat, pmtSec sectionAssembler
}

// NewDemux returns a Demux reading the transport stream from r.
func NewDemux(r io.Reader) *Demux {
	return &Demux{
		r:      NewReader(r),
		maxPES: defaultMaxPES,
		es:     map[uint16]*esState{},
		cc:     map[uint16]uint8{},
		pat:    sectionAssembler{table: tablePAT},
		pmtSec: sectionAssembler{table: tablePMT},
	}
}

// Stats returns the demuxer's counters.
func (d *Demux) Stats() DemuxStats {
	s := d.stats
	s.Reader = d.r.Stats()
	return s
}

// Next returns the next event. At the end of the input it flushes the pending
// PES in ascending PID order and then returns io.EOF. A read error from the
// underlying reader is returned as is; decoding errors are only counted.
func (d *Demux) Next() (Event, error) {
	for {
		if d.head < len(d.queue) {
			ev := d.queue[d.head]
			d.queue[d.head] = Event{}
			d.head++
			if d.head == len(d.queue) {
				d.queue, d.head = d.queue[:0], 0
			}
			return ev, nil
		}
		if d.eof {
			return Event{}, io.EOF
		}
		p, err := d.r.Next()
		if errors.Is(err, io.EOF) {
			for _, pid := range slices.Sorted(maps.Keys(d.es)) {
				d.endPES(pid, d.es[pid])
			}
			d.eof = true
			continue
		}
		if err != nil {
			return Event{}, err
		}
		d.handle(p)
	}
}

func (d *Demux) push(ev Event) { d.queue = append(d.queue, ev) }

// handle routes one packet. st is nil for the PAT and PMT PIDs.
func (d *Demux) handle(p Packet) {
	var st *esState
	switch {
	case p.PID == pidPAT, d.havePMTPID && p.PID == d.pmtPID:
	default:
		if st = d.es[p.PID]; st == nil {
			return
		}
	}
	if !d.checkCC(p, st) || !p.HasPayload {
		return
	}
	switch {
	case p.PID == pidPAT:
		d.stats.PSIErrors += uint64(d.pat.feed(p.PUSI, p.Payload, d.onPAT))
	case st == nil:
		d.stats.PSIErrors += uint64(d.pmtSec.feed(p.PUSI, p.Payload, d.onPMT))
	default:
		d.onES(p, st)
	}
}

// checkCC applies the continuity rules and reports whether the packet should
// be processed; it is false for a duplicate.
func (d *Demux) checkCC(p Packet, st *esState) bool {
	prev, ok := d.cc[p.PID]
	switch {
	case p.Discontinuity && !p.HasPayload:
		delete(d.cc, p.PID)
		return true
	case p.Discontinuity, !ok:
	case !p.HasPayload:
		if p.CC != prev {
			d.gap(p.PID, st)
		}
	case p.CC == prev:
		return false
	case p.CC != (prev+1)&0x0F:
		d.gap(p.PID, st)
	}
	d.cc[p.PID] = p.CC
	return true
}

// gap records a continuity gap and drops what was in progress on the PID.
func (d *Demux) gap(pid uint16, st *esState) {
	d.stats.CCErrors++
	switch {
	case st != nil:
		st.pes = nil
		st.lost = true
	case pid == pidPAT:
		d.pat.reset()
	default:
		d.pmtSec.reset()
	}
}

func (d *Demux) onPAT(sec []byte) {
	if sec[0] != tablePAT {
		return
	}
	ls, err := parseLongSection(sec)
	if err != nil {
		d.stats.PSIErrors++
		return
	}
	if !ls.current {
		return
	}
	prog, pid, ok, err := parsePAT(ls.body)
	if err != nil {
		d.stats.PSIErrors++
		return
	}
	if !ok {
		return
	}
	// The program comes from the lowest-numbered section that lists one, so
	// the sections of a multi-section PAT do not take turns choosing. A new
	// PAT version lets any section choose again.
	if d.havePMTPID && ls.version == d.patVersion && ls.number > d.patSection {
		return
	}
	d.patSection, d.patVersion = ls.number, ls.version
	if d.havePMTPID && pid == d.pmtPID && prog == d.program {
		return
	}
	d.selectProgram(prog, pid)
}

// selectProgram switches to a new program or PMT PID: pending length-0 PES
// are flushed, sized ones dropped, and the old PMT's PIDs forgotten, so the
// next PMT counts as a change.
func (d *Demux) selectProgram(prog, pid uint16) {
	if d.havePMTPID {
		for _, esPID := range slices.Sorted(maps.Keys(d.es)) {
			d.endPES(esPID, d.es[esPID])
			delete(d.cc, esPID)
		}
		delete(d.cc, d.pmtPID)
		clear(d.es)
	}
	d.pmt = nil
	d.program, d.pmtPID, d.havePMTPID = prog, pid, true
	d.pmtSec.reset()
}

func (d *Demux) onPMT(sec []byte) {
	if sec[0] != tablePMT {
		return
	}
	ls, err := parseLongSection(sec)
	if err != nil {
		d.stats.PSIErrors++
		return
	}
	if !ls.current || ls.idExt != d.program {
		return
	}
	pmt, err := parsePMT(ls, d.pmtPID)
	if err != nil {
		d.stats.PSIErrors++
		return
	}
	d.applyPMT(pmt)
}

// applyPMT installs a changed PMT. Streams that left it, or whose type
// changed, end their pending PES first, so those events precede the PMT's.
func (d *Demux) applyPMT(n *PMT) {
	if d.pmt != nil && d.pmt.equal(n) {
		return
	}
	types := make(map[uint16]uint8, len(n.Streams))
	for _, s := range n.Streams {
		types[s.PID] = s.Type
	}
	for _, pid := range slices.Sorted(maps.Keys(d.es)) {
		st := d.es[pid]
		typ, ok := types[pid]
		switch {
		case !ok:
			d.endPES(pid, st)
			delete(d.es, pid)
			delete(d.cc, pid)
		case typ != st.typ:
			d.endPES(pid, st)
			d.es[pid] = &esState{typ: typ}
		}
	}
	for pid, typ := range types {
		if d.es[pid] == nil {
			d.es[pid] = &esState{typ: typ}
		}
	}
	d.pmt = n
	d.push(Event{PMT: n.clone()})
}

// onES assembles PES packets on an elementary-stream PID.
func (d *Demux) onES(p Packet, st *esState) {
	if p.PUSI {
		d.endPES(p.PID, st)
		st.pes = &pesBuf{ra: p.RandomAccess, disc: st.lost}
		st.lost = false
	}
	if st.pes == nil {
		return // a PES whose start was never seen
	}
	b := st.pes
	b.raw = append(b.raw, p.Payload...)
	if !b.known {
		if len(b.raw) < pesPrefixLen {
			return
		}
		if b.raw[0] != 0 || b.raw[1] != 0 || b.raw[2] != 1 {
			st.pes = nil
			d.stats.PESErrors++
			return
		}
		b.known = true
		if n := int(be16(b.raw[4:])); n != 0 {
			b.want = pesPrefixLen + n
			if b.want > len(b.raw) {
				b.raw = slices.Grow(b.raw, b.want-len(b.raw))
			}
		}
	}
	switch {
	case b.want > 0 && len(b.raw) >= b.want:
		st.pes = nil
		d.finishPES(p.PID, b, b.raw[:b.want])
	case len(b.raw) > d.maxPES:
		st.pes = nil
		d.stats.PESErrors++
	}
}

// endPES ends the PES pending on a PID: a length-0 one is complete, any
// other is incomplete and counted.
func (d *Demux) endPES(pid uint16, st *esState) {
	b := st.pes
	if b == nil {
		return
	}
	st.pes = nil
	if b.known && b.want == 0 {
		d.finishPES(pid, b, b.raw)
		return
	}
	d.stats.PESErrors++
}

func (d *Demux) finishPES(pid uint16, b *pesBuf, raw []byte) {
	pes, err := parsePES(raw)
	if err != nil {
		d.stats.PESErrors++
		return
	}
	pes.PID = pid
	pes.RandomAccess = b.ra
	pes.Discontinuity = b.disc
	d.push(Event{PES: pes})
}

// parsePES parses a complete PES packet whose 6-byte prefix has been checked.
func parsePES(raw []byte) (*PES, error) {
	p := &PES{}
	if !hasOptionalHeader(raw[3]) {
		p.Data = raw[pesPrefixLen:len(raw):len(raw)]
		return p, nil
	}
	if len(raw) < pesOptHeaderLen {
		return nil, errors.New("mpegts: pes: header too short")
	}
	if raw[6]&0xC0 != 0x80 {
		return nil, errors.New("mpegts: pes: bad marker bits")
	}
	hdl := int(raw[8])
	start := pesOptHeaderLen + hdl
	if start > len(raw) {
		return nil, fmt.Errorf("mpegts: pes: PES_header_data_length %d beyond the packet", hdl)
	}
	switch flags := raw[7] >> 6; flags {
	case 0:
	case 2, 3:
		need := 5
		if flags == 3 {
			need = 10
		}
		if hdl < need {
			return nil, fmt.Errorf("mpegts: pes: PES_header_data_length %d too short for timestamps", hdl)
		}
		p.HasPTS = true
		p.PTS = readTimestamp(raw[9:])
		p.DTS = p.PTS
		if flags == 3 {
			p.DTS = readTimestamp(raw[14:])
		}
	default:
		return nil, errors.New("mpegts: pes: forbidden PTS_DTS_flags '01'")
	}
	p.Data = raw[start:len(raw):len(raw)]
	return p, nil
}

// hasOptionalHeader reports whether a stream_id is followed by the optional
// PES header (ISO/IEC 13818-1 Table 2-21).
func hasOptionalHeader(streamID byte) bool {
	switch streamID {
	case 0xBC, 0xBE, 0xBF, 0xF0, 0xF1, 0xF2, 0xF8, 0xFF:
		return false
	}
	return true
}

// readTimestamp decodes a 5-byte 33-bit PTS or DTS field.
func readTimestamp(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 |
		int64(b[3])<<7 | int64(b[4]>>1)
}
