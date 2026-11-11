package mpegts

import (
	"errors"
	"fmt"
	"slices"
)

const (
	pidPAT  = 0x0000
	pidNull = 0x1FFF

	tablePAT      = 0x00
	tablePMT      = 0x02
	tableStuffing = 0xFF

	// maxPSISectionLen is the largest section_length of a PAT or PMT section.
	maxPSISectionLen = 1021
	// minLongSectionLen is the smallest section_length of a long-form section:
	// 5 header bytes after the length field and the 4-byte CRC_32.
	minLongSectionLen = 9
	// longHeaderLen is table_id through last_section_number.
	longHeaderLen = 8
	crcLen        = 4
)

// Stream is one elementary stream listed in a PMT.
type Stream struct {
	PID  uint16
	Type uint8 // stream_type
}

// PMT is the program map of the selected program.
type PMT struct {
	Program uint16 // program_number
	PID     uint16 // PID the PMT is carried on
	Version uint8
	PCRPID  uint16
	// Streams lists the elementary streams in PMT order. Entries on PID 0,
	// the PMT PID or 0x1FFF, and repeats of an earlier PID, are left out.
	Streams []Stream
}

// equal reports whether two PMTs of the same program carry the same version,
// PCR PID and stream list.
func (p *PMT) equal(o *PMT) bool {
	return p.Version == o.Version && p.PCRPID == o.PCRPID && slices.Equal(p.Streams, o.Streams)
}

func (p *PMT) clone() *PMT {
	c := *p
	c.Streams = slices.Clone(p.Streams)
	return &c
}

// sectionAssembler reassembles the PSI sections carried on one PID.
type sectionAssembler struct {
	// table is the table_id whose section_length is checked against
	// maxPSISectionLen as soon as the length is known, so an oversized PAT
	// or PMT is rejected instead of waiting for bytes that never come.
	table  uint8
	buf    []byte
	active bool // a section start has been seen and buf continues it
}

func (s *sectionAssembler) reset() {
	s.buf = s.buf[:0]
	s.active = false
}

// feed adds one packet's payload and passes every completed section to emit;
// the section is valid only during the call. It returns how many framing
// errors it found: a pointer_field beyond the payload, a PAT or PMT section
// cut short by the next section start, or an oversized PAT or PMT
// section_length. Sections of other tables are ignored without error.
func (s *sectionAssembler) feed(pusi bool, payload []byte, emit func([]byte)) int {
	if len(payload) == 0 {
		return 0
	}
	errs := 0
	if pusi {
		ptr := int(payload[0])
		if 1+ptr > len(payload) {
			s.reset()
			return 1
		}
		if s.active {
			s.buf = append(s.buf, payload[1:1+ptr]...)
			errs += s.drain(emit)
			if s.active && len(s.buf) > 0 && s.buf[0] == s.table {
				errs++
			}
		}
		s.buf = append(s.buf[:0], payload[1+ptr:]...)
		s.active = true
	} else {
		if !s.active {
			return 0
		}
		s.buf = append(s.buf, payload...)
	}
	return errs + s.drain(emit)
}

// drain emits the complete sections at the start of buf, stopping at a
// partial section or at 0xFF stuffing, which ends the packet's sections.
func (s *sectionAssembler) drain(emit func([]byte)) int {
	for len(s.buf) > 0 {
		if s.buf[0] == tableStuffing {
			s.reset()
			return 0
		}
		if len(s.buf) < 3 {
			return 0
		}
		slen := int(s.buf[1]&0x0F)<<8 | int(s.buf[2])
		if s.buf[0] == s.table && slen > maxPSISectionLen {
			s.reset()
			return 1
		}
		total := 3 + slen
		if len(s.buf) < total {
			return 0
		}
		emit(s.buf[:total])
		n := copy(s.buf, s.buf[total:])
		s.buf = s.buf[:n]
	}
	return 0
}

// longSection is the parsed header of a long-form PSI section.
type longSection struct {
	idExt   uint16 // table_id_extension: transport_stream_id or program_number
	version uint8
	current bool   // current_next_indicator
	number  uint8  // section_number
	body    []byte // after last_section_number, before CRC_32
}

var errShortSection = errors.New("mpegts: psi: section too short")

// parseLongSection checks the syntax indicator, length and CRC of a PAT or
// PMT section.
func parseLongSection(sec []byte) (longSection, error) {
	slen := len(sec) - 3
	if slen < minLongSectionLen {
		return longSection{}, errShortSection
	}
	if sec[1]&0x80 == 0 {
		return longSection{}, errors.New("mpegts: psi: section_syntax_indicator is 0")
	}
	if slen > maxPSISectionLen {
		return longSection{}, fmt.Errorf("mpegts: psi: section_length %d too large", slen)
	}
	end := len(sec) - crcLen
	want := be32(sec[end:])
	if got := crc32MPEG2(sec[:end]); got != want {
		return longSection{}, fmt.Errorf("mpegts: psi: CRC mismatch: got %08x, want %08x", got, want)
	}
	return longSection{
		idExt:   be16(sec[3:]),
		version: sec[5] >> 1 & 0x1F,
		current: sec[5]&0x01 != 0,
		number:  sec[6],
		body:    sec[longHeaderLen:end],
	}, nil
}

// parsePAT returns the first program with a non-zero program_number and a
// usable PMT PID; ok is false when the section lists none.
func parsePAT(body []byte) (program, pmtPID uint16, ok bool, err error) {
	if len(body)%4 != 0 {
		return 0, 0, false, fmt.Errorf("mpegts: pat: program loop of %d bytes", len(body))
	}
	for i := 0; i < len(body); i += 4 {
		prog := be16(body[i:])
		pid := be16(body[i+2:]) & 0x1FFF
		if prog == 0 || pid == pidPAT || pid == pidNull {
			continue
		}
		return prog, pid, true, nil
	}
	return 0, 0, false, nil
}

// parsePMT parses the body of a PMT section carried on pid.
func parsePMT(ls longSection, pid uint16) (*PMT, error) {
	b := ls.body
	if len(b) < 4 {
		return nil, errShortSection
	}
	pmt := &PMT{
		Program: ls.idExt,
		PID:     pid,
		Version: ls.version,
		PCRPID:  be16(b) & 0x1FFF,
	}
	i := 4 + int(be16(b[2:])&0x0FFF)
	if i > len(b) {
		return nil, errors.New("mpegts: pmt: program_info_length beyond the section")
	}
	for i < len(b) {
		if i+5 > len(b) {
			return nil, errors.New("mpegts: pmt: truncated stream entry")
		}
		s := Stream{PID: be16(b[i+1:]) & 0x1FFF, Type: b[i]}
		i += 5 + int(be16(b[i+3:])&0x0FFF)
		if i > len(b) {
			return nil, errors.New("mpegts: pmt: ES_info_length beyond the section")
		}
		if s.PID == pidPAT || s.PID == pidNull || s.PID == pid ||
			slices.ContainsFunc(pmt.Streams, func(o Stream) bool { return o.PID == s.PID }) {
			continue
		}
		pmt.Streams = append(pmt.Streams, s)
	}
	return pmt, nil
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
