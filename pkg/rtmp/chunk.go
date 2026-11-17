package rtmp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// Chunk stream constants (Adobe RTMP specification 1.0 §5.3).
const (
	defaultChunkSize  = 128
	maxChunkSize      = 0xFFFFFF
	maxMessageLength  = 0xFFFFFF
	extendedTimestamp = 0xFFFFFF
	minCSID           = 2
	maxCSID           = 65599
	controlCSID       = 2

	defaultMaxMessage = 8 << 20
	defaultMaxStreams = 64
	defaultMaxPending = 16 << 20

	// readStep bounds how much buffer one read allocates ahead of the bytes
	// that fill it, whatever chunk size the peer chose.
	readStep = 64 << 10
	// keepWriteBuf is the largest encode buffer a ChunkWriter keeps for reuse.
	keepWriteBuf = 1 << 20
)

// headerLen is the message header length for chunk formats 0 to 3.
var headerLen = [4]int{11, 7, 3, 0}

// ChunkLimits bounds what a peer can make a ChunkReader hold. A zero field
// takes its default: MaxMessage 8 MiB, MaxStreams 64, MaxPending 16 MiB.
type ChunkLimits struct {
	// MaxMessage is the largest message length accepted.
	MaxMessage int
	// MaxStreams is the most chunk stream ids one connection may use.
	MaxStreams int
	// MaxPending bounds the bytes of partially assembled messages across all
	// chunk streams.
	MaxPending int
}

func (l ChunkLimits) withDefaults() ChunkLimits {
	if l.MaxMessage <= 0 {
		l.MaxMessage = defaultMaxMessage
	}
	if l.MaxStreams <= 0 {
		l.MaxStreams = defaultMaxStreams
	}
	if l.MaxPending <= 0 {
		l.MaxPending = defaultMaxPending
	}
	return l
}

// inStream is the receive state of one chunk stream: the last header fields
// for header compression and the message being assembled.
type inStream struct {
	timestamp uint32 // timestamp of the current or last message
	delta     uint32 // 0 after a type 0 header
	length    uint32
	streamID  uint32
	typ       uint8
	extended  bool   // the last type 0/1/2 header carried an extended field
	extValue  uint32 // that field's value
	active    bool   // a message is being assembled
	buf       []byte // its bytes so far; grows as chunks arrive
}

// ChunkReader reassembles RTMP messages from a peer's chunk stream. It is not
// safe for concurrent use.
type ChunkReader struct {
	r         *bufio.Reader
	lim       ChunkLimits
	chunkSize int
	streams   map[uint32]*inStream
	pending   int
	dropped   uint64
	n         uint64
	hdr       [11]byte
}

// NewChunkReader reads chunks from r, which may already have been used for
// the handshake. The peer's chunk size starts at 128.
func NewChunkReader(r *bufio.Reader, lim ChunkLimits) *ChunkReader {
	return &ChunkReader{
		r:         r,
		lim:       lim.withDefaults(),
		chunkSize: defaultChunkSize,
		streams:   make(map[uint32]*inStream),
	}
}

// SetLimits replaces the limits; zero fields take their defaults. A new
// MaxMessage applies to messages that start after the call.
func (c *ChunkReader) SetLimits(lim ChunkLimits) {
	c.lim = lim.withDefaults()
}

// DroppedPartials counts partial messages abandoned because a type 0, 1 or 2
// header started a new message on the same chunk stream before the previous
// one was complete. Discards requested by an Abort message are not counted;
// the caller sees the Abort itself.
func (c *ChunkReader) DroppedPartials() uint64 {
	return c.dropped
}

// BytesRead is the number of bytes consumed from the reader so far, for
// acknowledgements.
func (c *ChunkReader) BytesRead() uint64 {
	return c.n
}

// ReadMessage returns the next complete message. An inbound Set Chunk Size is
// applied to the chunks that follow it, and an Abort discards the named chunk
// stream's partial message; both are still returned. The Payload is a fresh
// allocation owned by the caller.
//
// When an encoder omits the extended timestamp on the type 3 chunks of a
// message that needs one, a trailing chunk of 1 to 3 bytes makes the read wait
// for the next chunk (or an error) before the message is returned; the
// session's idle timeout bounds that wait.
func (c *ChunkReader) ReadMessage() (Message, error) {
	for {
		m, done, err := c.readChunk()
		if err != nil {
			return Message{}, err
		}
		if !done {
			continue
		}
		if err := c.applyControl(m); err != nil {
			return Message{}, err
		}
		return m, nil
	}
}

func (c *ChunkReader) applyControl(m Message) error {
	switch m.Type {
	case MsgSetChunkSize:
		if len(m.Payload) < 4 {
			return errors.New("rtmp: chunk: short Set Chunk Size")
		}
		v := binary.BigEndian.Uint32(m.Payload)
		if v == 0 || v&0x80000000 != 0 {
			return fmt.Errorf("rtmp: chunk: invalid chunk size %#x", v)
		}
		c.chunkSize = int(min(v, maxChunkSize))
	case MsgAbort:
		if len(m.Payload) < 4 {
			return errors.New("rtmp: chunk: short Abort")
		}
		if st := c.streams[binary.BigEndian.Uint32(m.Payload)]; st != nil {
			c.discard(st)
		}
	}
	return nil
}

// abandon drops a partial message because a new header replaced it.
func (c *ChunkReader) abandon(st *inStream) {
	if st.active && len(st.buf) > 0 {
		c.dropped++
	}
	c.discard(st)
}

// discard drops a chunk stream's partial message.
func (c *ChunkReader) discard(st *inStream) {
	c.pending -= len(st.buf)
	st.buf = nil
	st.active = false
}

func (c *ChunkReader) readFull(b []byte) error {
	n, err := io.ReadFull(c.r, b)
	c.n += uint64(n)
	if err != nil {
		return fmt.Errorf("rtmp: chunk: read: %w", err)
	}
	return nil
}

// readExtended reads the extended timestamp field when field signals one,
// records it on st and returns the effective value.
func (c *ChunkReader) readExtended(st *inStream, field uint32) (uint32, error) {
	st.extended = field == extendedTimestamp
	if !st.extended {
		return field, nil
	}
	if err := c.readFull(c.hdr[:4]); err != nil {
		return 0, err
	}
	st.extValue = binary.BigEndian.Uint32(c.hdr[:4])
	return st.extValue, nil
}

// readChunk reads one chunk. done reports that it completed a message.
func (c *ChunkReader) readChunk() (m Message, done bool, err error) {
	if err := c.readFull(c.hdr[:1]); err != nil {
		return Message{}, false, err
	}
	format := c.hdr[0] >> 6
	csid := uint32(c.hdr[0] & 0x3F)
	switch csid {
	case 0:
		if err := c.readFull(c.hdr[:1]); err != nil {
			return Message{}, false, err
		}
		csid = 64 + uint32(c.hdr[0])
	case 1:
		if err := c.readFull(c.hdr[:2]); err != nil {
			return Message{}, false, err
		}
		csid = 64 + uint32(c.hdr[0]) + uint32(c.hdr[1])<<8
	}

	st := c.streams[csid]
	if st == nil {
		if format != 0 {
			return Message{}, false, fmt.Errorf("rtmp: chunk: csid %d: first chunk has format %d", csid, format)
		}
		if len(c.streams) >= c.lim.MaxStreams {
			return Message{}, false, fmt.Errorf("rtmp: chunk: more than %d chunk streams", c.lim.MaxStreams)
		}
		st = &inStream{}
		c.streams[csid] = st
	}

	h := c.hdr[:headerLen[format]]
	if err := c.readFull(h); err != nil {
		return Message{}, false, err
	}
	// A type 0, 1 or 2 header always starts a new message; one arriving while
	// a message is still being assembled on this chunk stream abandons it.
	start := !st.active
	switch format {
	case 0:
		c.abandon(st)
		st.length = be24(h[3:6])
		st.typ = h[6]
		st.streamID = binary.LittleEndian.Uint32(h[7:11])
		ts, err := c.readExtended(st, be24(h[0:3]))
		if err != nil {
			return Message{}, false, err
		}
		st.timestamp = ts
		st.delta = 0
		start = true
	case 1, 2:
		c.abandon(st)
		if format == 1 {
			st.length = be24(h[3:6])
			st.typ = h[6]
		}
		d, err := c.readExtended(st, be24(h[0:3]))
		if err != nil {
			return Message{}, false, err
		}
		st.delta = d
		st.timestamp += d
		start = true
	default:
		// Some encoders omit the extended field on type 3 chunks, so it is
		// consumed only when the next four bytes repeat the stored value.
		// Peek errors are left to the payload read.
		if st.extended {
			if p, err := c.r.Peek(4); err == nil && binary.BigEndian.Uint32(p) == st.extValue {
				_, _ = c.r.Discard(4)
				c.n += 4
			}
		}
		if start {
			st.timestamp += st.delta
		}
	}

	if start {
		if int(st.length) > c.lim.MaxMessage {
			return Message{}, false, fmt.Errorf("rtmp: chunk: message length %d exceeds limit %d", st.length, c.lim.MaxMessage)
		}
		st.active = true
		st.buf = nil
	}

	// The chunk payload is read in steps of at most readStep, so a peer that
	// declares a large message and a large chunk size and then stalls holds
	// at most one step of unfilled buffer.
	for left := min(int(st.length)-len(st.buf), c.chunkSize); left > 0; {
		step := min(left, readStep)
		if c.pending+step > c.lim.MaxPending {
			return Message{}, false, fmt.Errorf("rtmp: chunk: partial messages exceed %d bytes", c.lim.MaxPending)
		}
		have := len(st.buf)
		st.buf = slices.Grow(st.buf, step)[:have+step]
		got, err := io.ReadFull(c.r, st.buf[have:])
		c.n += uint64(got)
		st.buf = st.buf[:have+got]
		c.pending += got
		if err != nil {
			return Message{}, false, fmt.Errorf("rtmp: chunk: read: %w", err)
		}
		left -= step
	}
	if len(st.buf) < int(st.length) {
		return Message{}, false, nil
	}

	m = Message{Type: st.typ, StreamID: st.streamID, Timestamp: st.timestamp, Payload: st.buf}
	if m.Payload == nil {
		m.Payload = []byte{}
	}
	c.discard(st)
	return m, true, nil
}

func be24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// outStream is the send state of one chunk stream, mirroring what the peer's
// reader holds for header compression.
type outStream struct {
	started   bool
	timestamp uint32
	delta     uint32
	length    uint32
	streamID  uint32
	typ       uint8
	extended  bool
	extValue  uint32
	format    byte // header format of the last message
}

// ChunkWriter splits messages into chunks with header compression. It is not
// safe for concurrent use.
type ChunkWriter struct {
	w         io.Writer
	chunkSize int
	streams   map[uint32]*outStream
	buf       []byte
}

// NewChunkWriter writes chunks to w, one Write per message. The chunk size
// starts at 128.
func NewChunkWriter(w io.Writer) *ChunkWriter {
	return &ChunkWriter{w: w, chunkSize: defaultChunkSize, streams: make(map[uint32]*outStream)}
}

// SetChunkSize sends Set Chunk Size on csid 2 and then uses n for every
// following chunk. n must be in 1..0xFFFFFF.
func (c *ChunkWriter) SetChunkSize(n int) error {
	if n < 1 || n > maxChunkSize {
		return fmt.Errorf("rtmp: chunk: invalid chunk size %d", n)
	}
	p := binary.BigEndian.AppendUint32(nil, uint32(n))
	if err := c.WriteMessage(controlCSID, Message{Type: MsgSetChunkSize, Payload: p}); err != nil {
		return err
	}
	c.chunkSize = n
	return nil
}

// WriteMessage writes m on chunk stream csid (2..65599). The header is type 0
// for the stream's first message, a new message stream id or a backwards
// timestamp; type 1 when the length or type changes; type 2 when only the
// delta changes or the previous header was type 0; type 3 otherwise.
func (c *ChunkWriter) WriteMessage(csid uint32, m Message) error {
	if csid < minCSID || csid > maxCSID {
		return fmt.Errorf("rtmp: chunk: invalid csid %d", csid)
	}
	if len(m.Payload) > maxMessageLength {
		return fmt.Errorf("rtmp: chunk: message length %d too large", len(m.Payload))
	}
	c.encode(csid, m)
	_, err := c.w.Write(c.buf)
	if cap(c.buf) > keepWriteBuf {
		c.buf = nil // do not pin a large message's buffer between writes
	}
	if err != nil {
		return fmt.Errorf("rtmp: chunk: write: %w", err)
	}
	return nil
}

func (c *ChunkWriter) encode(csid uint32, m Message) {
	st := c.streams[csid]
	if st == nil {
		st = &outStream{}
		c.streams[csid] = st
	}
	length := uint32(len(m.Payload))
	var format byte = 3
	var field uint32
	switch {
	case !st.started || m.StreamID != st.streamID || m.Timestamp < st.timestamp:
		format, field = 0, m.Timestamp
		st.delta = 0
	default:
		delta := m.Timestamp - st.timestamp
		switch {
		case length != st.length || m.Type != st.typ:
			format = 1
		case delta != st.delta, st.format == 0:
			// ffmpeg adds the previous timestamp field to a type 3 header that
			// starts a message, so a type 0 header is never followed by one.
			format = 2
		}
		if format != 3 {
			field = delta
			st.delta = delta
		}
	}
	if format != 3 {
		st.extended = field >= extendedTimestamp
		st.extValue = field
	}
	st.format = format
	st.started = true
	st.timestamp = m.Timestamp
	st.length = length
	st.typ = m.Type
	st.streamID = m.StreamID

	b := appendBasicHeader(c.buf[:0], format, csid)
	field24 := min(field, extendedTimestamp)
	switch format {
	case 0:
		b = append24(b, field24)
		b = append24(b, length)
		b = append(b, m.Type)
		b = binary.LittleEndian.AppendUint32(b, m.StreamID)
	case 1:
		b = append24(b, field24)
		b = append24(b, length)
		b = append(b, m.Type)
	case 2:
		b = append24(b, field24)
	}
	if st.extended {
		b = binary.BigEndian.AppendUint32(b, st.extValue)
	}
	p := m.Payload
	for {
		n := min(len(p), c.chunkSize)
		b = append(b, p[:n]...)
		p = p[n:]
		if len(p) == 0 {
			break
		}
		b = appendBasicHeader(b, 3, csid)
		if st.extended {
			b = binary.BigEndian.AppendUint32(b, st.extValue)
		}
	}
	c.buf = b
}

func appendBasicHeader(b []byte, format byte, csid uint32) []byte {
	switch {
	case csid < 64:
		return append(b, format<<6|byte(csid))
	case csid < 320:
		return append(b, format<<6, byte(csid-64))
	default:
		return append(b, format<<6|1, byte(csid-64), byte((csid-64)>>8))
	}
}

func append24(b []byte, v uint32) []byte {
	return append(b, byte(v>>16), byte(v>>8), byte(v))
}
