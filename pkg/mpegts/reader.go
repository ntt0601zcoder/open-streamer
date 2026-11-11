package mpegts

import (
	"errors"
	"fmt"
	"io"
)

const (
	// readerBufSize holds 64 packets; it must exceed 2*PacketSize+1 so the
	// lock can look at three packet starts at once.
	readerBufSize = 64 * PacketSize
	// maxEmptyReads is how many consecutive (0, nil) reads are tolerated
	// before the reader gives up with io.ErrNoProgress.
	maxEmptyReads = 100
)

// ReaderStats counts what a Reader has seen.
type ReaderStats struct {
	Packets      uint64 // packets returned by Next
	Resyncs      uint64 // lock scans that skipped at least one byte
	SkippedBytes uint64 // bytes skipped by lock scans
	Errored      uint64 // packets dropped: transport_error_indicator, bad adaptation field, reserved control
}

// Reader reads 188-byte MPEG-TS packets from any io.Reader. It never seeks,
// and works with readers that return as little as one byte per call.
//
// The reader locks at the start, and again whenever the byte at the next
// expected packet start is not the 0x47 sync byte, by scanning forward for an
// offset where three consecutive packet starts (o, o+188, o+376) are all 0x47.
// A packet start beyond the end of the input counts as matching, so short
// inputs still lock.
//
// Known limitation: when bytes go missing inside a packet, that packet is
// returned with the first bytes of the next packet appended, and the next
// packet is lost to the resync. A demuxer's continuity check catches this
// when both packets are on one PID. 192-byte (M2TS) and 204-byte (Reed-Solomon)
// packets are not supported.
//
// A Reader is not safe for concurrent use.
type Reader struct {
	rd     io.Reader
	buf    []byte
	pos    int // next unread byte
	end    int // end of buffered data
	err    error
	empty  int
	locked bool
	stats  ReaderStats
}

// NewReader returns a Reader that reads packets from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{rd: r, buf: make([]byte, readerBufSize)}
}

// Stats returns the reader's counters.
func (r *Reader) Stats() ReaderStats {
	return r.stats
}

// Next returns the next valid packet. Its Payload aliases the reader's buffer
// and is valid only until the next call. Next returns io.EOF at the end of
// the input; a trailing partial packet is dropped. A read error other than
// io.EOF is returned as is, after every complete packet read before it.
func (r *Reader) Next() (Packet, error) {
	for {
		if err := r.fill(1); err != nil {
			return Packet{}, err
		}
		if !r.locked || r.buf[r.pos] != syncByte {
			if err := r.lock(); err != nil {
				return Packet{}, err
			}
		}
		if err := r.fill(PacketSize); err != nil {
			if errors.Is(err, io.EOF) {
				r.pos = r.end // drop the trailing partial packet
			}
			return Packet{}, err
		}
		b := r.buf[r.pos : r.pos+PacketSize]
		r.pos += PacketSize
		p, ok := parsePacket(b)
		if !ok {
			r.stats.Errored++
			continue
		}
		r.stats.Packets++
		return p, nil
	}
}

// lock advances r.pos to the next offset whose three packet starts all hold
// the sync byte, and records the skipped bytes.
func (r *Reader) lock() error {
	skipped := 0
	defer func() {
		if skipped > 0 {
			r.stats.Resyncs++
			r.stats.SkippedBytes += uint64(skipped)
		}
	}()
	for {
		if err := r.fill(1); err != nil {
			return err
		}
		if r.buf[r.pos] == syncByte && r.confirm() {
			r.locked = true
			return nil
		}
		r.pos++
		skipped++
	}
}

// confirm reports whether the bytes one and two packets after r.pos are sync
// bytes. A position the input ends before counts as matching; the error that
// ended the input is returned by the following fill.
func (r *Reader) confirm() bool {
	for k := 1; k <= 2; k++ {
		off := k * PacketSize
		if r.fill(off+1) != nil {
			return true
		}
		if r.buf[r.pos+off] != syncByte {
			return false
		}
	}
	return true
}

// fill makes at least need bytes available from r.pos. It returns the
// reader's terminal error (io.EOF or a read error) when the input ends first.
func (r *Reader) fill(need int) error {
	for r.end-r.pos < need {
		if r.err != nil {
			return r.err
		}
		if r.pos+need > len(r.buf) {
			r.end = copy(r.buf, r.buf[r.pos:r.end])
			r.pos = 0
		}
		n, err := r.rd.Read(r.buf[r.end:])
		r.end += n
		switch {
		case err != nil:
			r.err = err
		case n == 0:
			r.empty++
			if r.empty >= maxEmptyReads {
				r.err = fmt.Errorf("mpegts: read: %w", io.ErrNoProgress)
			}
		default:
			r.empty = 0
		}
	}
	return nil
}
