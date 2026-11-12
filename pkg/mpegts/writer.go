package mpegts

import (
	"errors"
	"fmt"
	"io"
	"slices"
)

const (
	defaultProgram = 1
	defaultPMTPID  = 4096
	// tsID is the transport_stream_id of the generated PAT.
	tsID = 1

	streamIDVideo = 0xE0
	streamIDAudio = 0xC0

	// maxSizedPES is the largest PES_packet_length.
	maxSizedPES = 65535
	// tsPayloadLen is the payload capacity of a packet without adaptation field.
	tsPayloadLen = PacketSize - 4
	ts33Mask     = 1<<33 - 1
	// maxPMTSection is the largest PMT section that fits one packet after the
	// pointer_field.
	maxPMTSection = tsPayloadLen - 1
)

// PESOptions describes one PES packet to write.
type PESOptions struct {
	PTS, DTS     int64 // 90 kHz; written modulo 2^33
	HasDTS       bool  // write DTS; otherwise only the PTS is written
	RandomAccess bool  // random_access_indicator on the first packet
	PCR          int64 // PCR base on the first packet, modulo 2^33; negative: none
	Video        bool  // stream_id 0xE0 and PES_packet_length 0 when over 65535 bytes; otherwise 0xC0
}

// Writer produces a transport stream with one program: PAT, PMT and PES
// packets. Continuity counters are kept per PID for the writer's life. A
// Writer is not safe for concurrent use.
type Writer struct {
	out     io.Writer
	program PMT
	set     bool
	patVer  uint8 // PAT version
	cc      map[uint16]uint8
	buf     []byte
}

// NewWriter returns a Writer that writes packets to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{out: w, cc: map[uint16]uint8{}}
}

// SetOutput redirects later packets to out. Continuity counters are kept.
func (w *Writer) SetOutput(out io.Writer) { w.out = out }

// SetProgram sets the program the writer describes in its tables. Program 0
// means 1 and PID 0 means 4096. The version is managed by the writer: it
// starts at 0 and increases modulo 32 whenever the program number, PMT PID,
// PCR PID or stream list changes, and p.Version is ignored.
func (w *Writer) SetProgram(p PMT) error {
	if p.Program == 0 {
		p.Program = defaultProgram
	}
	if p.PID == 0 {
		p.PID = defaultPMTPID
	}
	if p.PID < 0x10 || p.PID >= pidNull {
		return fmt.Errorf("mpegts: set program: PMT PID %d out of range", p.PID)
	}
	if len(p.Streams) == 0 {
		return errors.New("mpegts: set program: no streams")
	}
	seen := map[uint16]bool{}
	for _, s := range p.Streams {
		if s.PID < 0x10 || s.PID >= pidNull || s.PID == p.PID {
			return fmt.Errorf("mpegts: set program: stream PID %d not usable", s.PID)
		}
		if seen[s.PID] {
			return fmt.Errorf("mpegts: set program: duplicate stream PID %d", s.PID)
		}
		seen[s.PID] = true
	}
	if !seen[p.PCRPID] {
		return fmt.Errorf("mpegts: set program: PCR PID %d is not a stream", p.PCRPID)
	}
	if n := pmtSectionLen(len(p.Streams)); n > maxPMTSection {
		return fmt.Errorf("mpegts: set program: %d streams need a %d-byte PMT section, over the %d that fit one packet",
			len(p.Streams), n, maxPMTSection)
	}
	p.Streams = slices.Clone(p.Streams)
	if w.set {
		if w.program.Program != p.Program || w.program.PID != p.PID {
			w.patVer = (w.patVer + 1) & 0x1F
		}
		p.Version = w.program.Version
		old := w.program
		if old.Program != p.Program || old.PID != p.PID || old.PCRPID != p.PCRPID ||
			!slices.Equal(old.Streams, p.Streams) {
			p.Version = (p.Version + 1) & 0x1F
		}
	} else {
		p.Version = 0
	}
	w.program, w.set = p, true
	return nil
}

// WriteTables writes one PAT packet and one PMT packet.
func (w *Writer) WriteTables() error {
	if !w.set {
		return errors.New("mpegts: write tables: no program set")
	}
	pat := longSectionBytes(tablePAT, tsID, w.patVer, []byte{
		byte(w.program.Program >> 8), byte(w.program.Program),
		0xE0 | byte(w.program.PID>>8), byte(w.program.PID),
	})
	body := []byte{
		0xE0 | byte(w.program.PCRPID>>8), byte(w.program.PCRPID),
		0xF0, 0x00, // program_info_length 0
	}
	for _, s := range w.program.Streams {
		body = append(body, s.Type, 0xE0|byte(s.PID>>8), byte(s.PID), 0xF0, 0x00)
	}
	pmt := longSectionBytes(tablePMT, w.program.Program, w.program.Version, body)

	w.buf = w.buf[:0]
	w.buf = w.appendPSI(w.buf, 0, pat)
	w.buf = w.appendPSI(w.buf, w.program.PID, pmt)
	return w.flush()
}

// longSectionBytes builds a long-form section: version with current_next 1,
// section 0 of 0, and the CRC_32.
func longSectionBytes(table uint8, idExt uint16, version uint8, body []byte) []byte {
	slen := 5 + len(body) + crcLen
	s := make([]byte, 0, 3+slen)
	s = append(s, table, 0xB0|byte(slen>>8), byte(slen),
		byte(idExt>>8), byte(idExt), 0xC0|version<<1|1, 0, 0)
	s = append(s, body...)
	c := crc32MPEG2(s)
	return append(s, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

// appendPSI appends one packet carrying section with a zero pointer_field.
func (w *Writer) appendPSI(dst []byte, pid uint16, section []byte) []byte {
	start := len(dst)
	dst = append(dst, syncByte, 0x40|byte(pid>>8), byte(pid), 0x10|w.nextCC(pid), 0)
	dst = append(dst, section...)
	for len(dst)-start < PacketSize {
		dst = append(dst, 0xFF)
	}
	return dst
}

func (w *Writer) nextCC(pid uint16) byte {
	c := w.cc[pid]
	w.cc[pid] = (c + 1) & 0x0F
	return c
}

func (w *Writer) flush() error {
	_, err := w.out.Write(w.buf)
	if err != nil {
		return fmt.Errorf("mpegts: write: %w", err)
	}
	return nil
}

// WritePES writes data as one PES packet on pid, which must be a stream of
// the program. PES_packet_length is set when the PES fits in 65535 bytes;
// otherwise it is 0, which is allowed only for opt.Video.
func (w *Writer) WritePES(pid uint16, data []byte, opt PESOptions) error {
	if !w.set {
		return errors.New("mpegts: write PES: no program set")
	}
	if !slices.ContainsFunc(w.program.Streams, func(s Stream) bool { return s.PID == pid }) {
		return fmt.Errorf("mpegts: write PES: PID %d is not in the program", pid)
	}
	hdrData := 5
	if opt.HasDTS {
		hdrData = 10
	}
	pesLen := 3 + hdrData + len(data)
	if pesLen > maxSizedPES {
		if !opt.Video {
			return fmt.Errorf("mpegts: write PES: %d bytes too large for a PES with a length", pesLen)
		}
		pesLen = 0
	}
	sid := byte(streamIDAudio)
	if opt.Video {
		sid = streamIDVideo
	}
	pes := make([]byte, 0, pesPrefixLen+3+hdrData+len(data))
	pes = append(pes, 0, 0, 1, sid, byte(pesLen>>8), byte(pesLen), 0x80)
	if opt.HasDTS {
		pes = append(pes, 0xC0, 10)
		pes = appendTimestamp(pes, 0x3, opt.PTS)
		pes = appendTimestamp(pes, 0x1, opt.DTS)
	} else {
		pes = append(pes, 0x80, 5)
		pes = appendTimestamp(pes, 0x2, opt.PTS)
	}
	pes = append(pes, data...)

	w.buf = w.buf[:0]
	first := true
	for len(pes) > 0 {
		var af []byte // adaptation field bytes after the length byte
		hdr1 := byte(pid >> 8)
		if first {
			hdr1 |= 0x40
			if opt.RandomAccess || opt.PCR >= 0 {
				flags := byte(0)
				if opt.RandomAccess {
					flags |= 0x40
				}
				af = append(af, flags)
				if opt.PCR >= 0 {
					af[0] |= 0x10
					b := opt.PCR & ts33Mask
					af = append(af, byte(b>>25), byte(b>>17), byte(b>>9), byte(b>>1),
						byte(b&1)<<7|0x7E, 0)
				}
			}
		}
		avail := tsPayloadLen
		if af != nil {
			avail -= 1 + len(af)
		}
		n := min(avail, len(pes))
		if n < avail {
			// Stuff the adaptation field so the payload ends the packet.
			stuff := avail - n
			if af == nil {
				// The length byte itself takes one of the stuffing bytes.
				stuff--
				af = []byte{}
				if stuff > 0 {
					af = append(af, 0) // flags
					stuff--
				}
			}
			for range stuff {
				af = append(af, 0xFF)
			}
		}
		afc := byte(0x10)
		if af != nil {
			afc = 0x30
		}
		w.buf = append(w.buf, syncByte, hdr1, byte(pid), afc|w.nextCC(pid))
		if af != nil {
			w.buf = append(w.buf, byte(len(af)))
			w.buf = append(w.buf, af...)
		}
		w.buf = append(w.buf, pes[:n]...)
		pes = pes[n:]
		first = false
	}
	return w.flush()
}

// appendTimestamp appends a 5-byte PES timestamp with the 4-bit prefix.
func appendTimestamp(dst []byte, prefix byte, ts int64) []byte {
	t := ts & ts33Mask
	return append(dst,
		prefix<<4|byte(t>>29)&0x0E|1,
		byte(t>>22),
		byte(t>>14)|1,
		byte(t>>7),
		byte(t<<1)|1,
	)
}

// pmtSectionLen is the size of the PMT section the writer generates for n
// streams: header, PCR PID and program info, 5 bytes per stream, CRC_32.
func pmtSectionLen(n int) int { return longHeaderLen + 4 + 5*n + crcLen }
