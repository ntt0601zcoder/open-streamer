package rtmp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// raw builds chunk streams byte by byte, independently of ChunkWriter.
type raw struct{ bytes.Buffer }

func (b *raw) basic(format byte, csid uint32) *raw {
	switch {
	case csid < 64:
		b.WriteByte(format<<6 | byte(csid))
	case csid < 320:
		b.WriteByte(format << 6)
		b.WriteByte(byte(csid - 64))
	default:
		b.WriteByte(format<<6 | 1)
		b.WriteByte(byte(csid - 64))
		b.WriteByte(byte((csid - 64) >> 8))
	}
	return b
}

func (b *raw) u24(v uint32) *raw {
	b.Write([]byte{byte(v >> 16), byte(v >> 8), byte(v)})
	return b
}

func (b *raw) u32(v uint32) *raw {
	b.Write(binary.BigEndian.AppendUint32(nil, v))
	return b
}

func (b *raw) le32(v uint32) *raw {
	b.Write(binary.LittleEndian.AppendUint32(nil, v))
	return b
}

func (b *raw) u8(v byte) *raw {
	b.WriteByte(v)
	return b
}

func (b *raw) data(p []byte) *raw {
	b.Write(p)
	return b
}

// fmt0 writes a type 0 header without an extended timestamp field.
func (b *raw) fmt0(csid, ts, length uint32, typ uint8, sid uint32) *raw {
	return b.basic(0, csid).u24(ts).u24(length).u8(typ).le32(sid)
}

func newReader(b []byte, lim ChunkLimits) *ChunkReader {
	return NewChunkReader(bufio.NewReader(bytes.NewReader(b)), lim)
}

func readAll(t *testing.T, c *ChunkReader) []Message {
	t.Helper()
	var out []Message
	for {
		m, err := c.ReadMessage()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		out = append(out, m)
	}
}

func fill(n int, seed byte) []byte {
	p := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range p {
		x = x*1103515245 + 12345
		p[i] = byte(x >> 16)
	}
	return p
}

func sameMessage(a, b Message) bool {
	return a.Type == b.Type && a.StreamID == b.StreamID && a.Timestamp == b.Timestamp &&
		bytes.Equal(a.Payload, b.Payload)
}

type plannedMsg struct {
	m      Message
	format byte // the writer's expected header format at chunk size 128
}

// roundTripPlan exercises every writer format choice: timestamps crossing
// 0xFFFFFF with small deltas, a backwards timestamp, stream id changes, an
// extended delta and extended absolute timestamps reused by type 3 headers.
// The first payload byte is the tag, so interleaved messages can be grouped.
func roundTripPlan(tag byte) []plannedMsg {
	type step struct {
		ts     uint32
		sid    uint32
		typ    uint8
		n      int
		format byte
	}
	steps := []step{
		{0xFFFF9B, 1, MsgAudio, 10, 0},
		{0xFFFFBC, 1, MsgAudio, 10, 2},
		{0xFFFFDD, 1, MsgAudio, 10, 3},
		{0xFFFFFE, 1, MsgAudio, 10, 3},
		{0x100001F, 1, MsgAudio, 10, 3},
		{0x1000040, 1, MsgVideo, 5000, 1},
		{0xFFF000, 1, MsgVideo, 5000, 0}, // backwards
		{0xFFF000, 2, MsgAudio, 300, 0},  // stream id change
		{0x1000000, 2, MsgVideo, 5000, 1},
		{0x12345678, 2, MsgVideo, 5000, 2}, // extended delta
		{0x2368ACF0, 2, MsgVideo, 5000, 3}, // repeated extended delta
		{0x2368ACF0, 3, MsgVideo, 5000, 0}, // extended absolute
		{0x2368ACF0, 3, MsgVideo, 5000, 3}, // delta 0 after type 0
		{0xFFFFFFF0, 3, MsgDataAMF0, 7, 1},
		{0x00000010, 3, MsgDataAMF0, 7, 0}, // wrapped, so backwards
	}
	out := make([]plannedMsg, 0, len(steps))
	for i, s := range steps {
		p := fill(s.n, tag*31+byte(i))
		p[0] = tag
		out = append(out, plannedMsg{Message{Type: s.typ, StreamID: s.sid, Timestamp: s.ts, Payload: p}, s.format})
	}
	return out
}

// writeChunks writes m and returns a copy of each chunk it produced.
func writeChunks(t *testing.T, w *ChunkWriter, out *bytes.Buffer, csid uint32, m Message) [][]byte {
	t.Helper()
	start := out.Len()
	if err := w.WriteMessage(csid, m); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	b := out.Bytes()[start:]
	cuts := append([]int{0}, w.bounds...)
	cuts = append(cuts, len(b))
	chunks := make([][]byte, 0, len(cuts)-1)
	for i := 0; i+1 < len(cuts); i++ {
		chunks = append(chunks, append([]byte(nil), b[cuts[i]:cuts[i+1]]...))
	}
	return chunks
}

// interleavedStream encodes the round-trip plan on three chunk streams (one
// per basic header form) and interleaves their chunks round robin.
func interleavedStream(t *testing.T, size int) (stream []byte, want map[byte][]Message) {
	t.Helper()
	var out bytes.Buffer
	w := NewChunkWriter(&out)
	if err := w.SetChunkSize(size); err != nil {
		t.Fatalf("SetChunkSize: %v", err)
	}
	stream = append([]byte(nil), out.Bytes()...)
	want = map[byte][]Message{}
	csids := []uint32{3, 100, 4000}
	queues := make([][][]byte, len(csids))
	for i, csid := range csids {
		tag := byte(i + 1)
		for _, pm := range roundTripPlan(tag) {
			chunks := writeChunks(t, w, &out, csid, pm.m)
			if size == 128 {
				if got := chunks[0][0] >> 6; got != pm.format {
					t.Fatalf("csid %d ts %#x: writer format %d, want %d", csid, pm.m.Timestamp, got, pm.format)
				}
			}
			queues[i] = append(queues[i], chunks...)
			want[tag] = append(want[tag], pm.m)
		}
	}
	for more := true; more; {
		more = false
		for i := range queues {
			if len(queues[i]) > 0 {
				stream = append(stream, queues[i][0]...)
				queues[i] = queues[i][1:]
				more = true
			}
		}
	}
	return stream, want
}

func TestChunkRoundTrip(t *testing.T) {
	for _, size := range []int{128, 4096} {
		stream, want := interleavedStream(t, size)
		c := newReader(stream, ChunkLimits{})
		got := map[byte][]Message{}
		for _, m := range readAll(t, c) {
			if m.Type == MsgSetChunkSize {
				if binary.BigEndian.Uint32(m.Payload) != uint32(size) {
					t.Fatalf("size %d: Set Chunk Size payload %x", size, m.Payload)
				}
				continue
			}
			got[m.Payload[0]] = append(got[m.Payload[0]], m)
		}
		for tag, msgs := range want {
			if len(got[tag]) != len(msgs) {
				t.Fatalf("size %d tag %d: got %d messages, want %d", size, tag, len(got[tag]), len(msgs))
			}
			for i := range msgs {
				if !sameMessage(got[tag][i], msgs[i]) {
					g := got[tag][i]
					t.Fatalf("size %d tag %d msg %d: got type %d sid %d ts %#x len %d, want type %d sid %d ts %#x len %d",
						size, tag, i, g.Type, g.StreamID, g.Timestamp, len(g.Payload),
						msgs[i].Type, msgs[i].StreamID, msgs[i].Timestamp, len(msgs[i].Payload))
				}
			}
		}
		if c.BytesRead() != uint64(len(stream)) {
			t.Fatalf("size %d: BytesRead %d, want %d", size, c.BytesRead(), len(stream))
		}
	}
}

func timestamps(msgs []Message) []uint32 {
	ts := make([]uint32, len(msgs))
	for i, m := range msgs {
		ts[i] = m.Timestamp
	}
	return ts
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChunkFormat3AfterFmt0(t *testing.T) {
	var b raw
	b.fmt0(4, 100, 4, MsgAudio, 1).data([]byte("aaaa"))
	b.basic(3, 4).data([]byte("bbbb")) // delta 0 after type 0
	b.basic(2, 4).u24(40).data([]byte("cccc"))
	b.fmt0(4, 500, 4, MsgAudio, 1).data([]byte("dddd")) // resets the delta to 0
	b.basic(3, 4).data([]byte("eeee"))
	msgs := readAll(t, newReader(b.Bytes(), ChunkLimits{}))
	if got, want := timestamps(msgs), []uint32{100, 100, 140, 500, 500}; !equalU32(got, want) {
		t.Fatalf("timestamps %v, want %v", got, want)
	}
	for _, m := range msgs {
		if m.Type != MsgAudio || m.StreamID != 1 || len(m.Payload) != 4 {
			t.Fatalf("inherited header wrong: %+v", m)
		}
	}
}

func TestChunkFormat3RepeatsDelta(t *testing.T) {
	var b raw
	b.fmt0(4, 1000, 4, MsgVideo, 1).data([]byte("aaaa"))
	b.basic(2, 4).u24(40).data([]byte("bbbb"))
	b.basic(3, 4).data([]byte("cccc"))
	b.basic(3, 4).data([]byte("dddd"))
	b.basic(1, 4).u24(20).u24(2).u8(MsgAudio).data([]byte("ee"))
	b.basic(3, 4).data([]byte("ff"))
	msgs := readAll(t, newReader(b.Bytes(), ChunkLimits{}))
	if got, want := timestamps(msgs), []uint32{1000, 1040, 1080, 1120, 1140, 1160}; !equalU32(got, want) {
		t.Fatalf("timestamps %v, want %v", got, want)
	}
	if m := msgs[5]; m.Type != MsgAudio || string(m.Payload) != "ff" {
		t.Fatalf("type 3 after type 1 inherited %+v", m)
	}
}

func TestChunkExtendedTimestampFmt3(t *testing.T) {
	const abs = 0x01000000
	const delta = 0x01000005
	payload := bytes.Repeat([]byte{0xAB}, 300) // 128 + 128 + 44 at chunk size 128
	build := func(repeat bool) []byte {
		var b raw
		ext := func() {
			if repeat {
				b.u32(abs)
			}
		}
		// Message 1: type 0 with an extended absolute timestamp.
		b.basic(0, 3).u24(0xFFFFFF).u24(300).u8(MsgVideo).le32(1).u32(abs).data(payload[:128])
		b.basic(3, 3)
		ext()
		b.data(payload[128:256])
		b.basic(3, 3)
		ext()
		b.data(payload[256:])
		// Message 2: a type 3 header starting a new message, delta 0.
		b.basic(3, 3)
		ext()
		b.data(payload[:128])
		b.basic(3, 3)
		ext()
		b.data(payload[128:256])
		b.basic(3, 3)
		ext()
		b.data(payload[256:])
		// Message 3: type 1 with an extended delta, then message 4 repeating it.
		b.basic(1, 3).u24(0xFFFFFF).u24(4).u8(MsgVideo).u32(delta).data([]byte{1, 2, 3, 4})
		b.basic(3, 3)
		if repeat {
			b.u32(delta)
		}
		b.data([]byte{5, 6, 7, 8})
		return b.Bytes()
	}
	for _, repeat := range []bool{true, false} {
		msgs := readAll(t, newReader(build(repeat), ChunkLimits{}))
		want := []uint32{abs, abs, abs + delta, abs + 2*delta}
		if got := timestamps(msgs); !equalU32(got, want) {
			t.Fatalf("repeat=%v: timestamps %#x, want %#x", repeat, got, want)
		}
		for i := range 2 {
			if !bytes.Equal(msgs[i].Payload, payload) {
				t.Fatalf("repeat=%v: message %d payload corrupted", repeat, i)
			}
		}
		if !bytes.Equal(msgs[3].Payload, []byte{5, 6, 7, 8}) {
			t.Fatalf("repeat=%v: message 4 payload %x", repeat, msgs[3].Payload)
		}
	}

	// The writer repeats the extended field on every type 3 chunk.
	var out bytes.Buffer
	w := NewChunkWriter(&out)
	m := Message{Type: MsgVideo, StreamID: 1, Timestamp: abs, Payload: payload}
	if err := w.WriteMessage(3, m); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMessage(3, m); err != nil {
		t.Fatal(err)
	}
	if want := build(true); !bytes.HasPrefix(want, out.Bytes()) {
		t.Fatalf("writer output differs from the repeated-extended form:\n got %x\nwant %x", out.Bytes(), want[:out.Len()])
	}
}

func TestChunkSetChunkSizeMidStream(t *testing.T) {
	var out bytes.Buffer
	w := NewChunkWriter(&out)
	a := Message{Type: MsgVideo, StreamID: 1, Timestamp: 10, Payload: fill(300, 1)}
	b := Message{Type: MsgVideo, StreamID: 1, Timestamp: 20, Payload: fill(3000, 2)}
	if err := w.WriteMessage(6, a); err != nil {
		t.Fatal(err)
	}
	afterA := out.Len()
	if err := w.SetChunkSize(4096); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMessage(6, b); err != nil {
		t.Fatal(err)
	}
	// Set Chunk Size goes on csid 2, message stream 0, as a type 0 header.
	if got := out.Bytes()[afterA]; got != 0x02 {
		t.Fatalf("Set Chunk Size basic header %#x, want 0x02", got)
	}
	// b fits in one 4096-byte chunk: 1 basic + 7 (type 1 header) + 3000.
	if got, want := out.Len()-afterA, 12+4+1+7+3000; got != want {
		t.Fatalf("bytes after a: %d, want %d", got, want)
	}
	c := newReader(out.Bytes(), ChunkLimits{})
	msgs := readAll(t, c)
	if len(msgs) != 3 || !sameMessage(msgs[0], a) || msgs[1].Type != MsgSetChunkSize || !sameMessage(msgs[2], b) {
		t.Fatalf("messages %+v", msgs)
	}
	if !bytes.Equal(msgs[1].Payload, []byte{0, 0, 0x10, 0}) || msgs[1].StreamID != 0 {
		t.Fatalf("Set Chunk Size message %+v", msgs[1])
	}
	if c.chunkSize != 4096 {
		t.Fatalf("reader chunk size %d", c.chunkSize)
	}

	// A value above 0xFFFFFF is clamped, and a 70000-byte message then
	// arrives in one chunk.
	var r raw
	r.fmt0(2, 0, 4, MsgSetChunkSize, 0).u32(0x7FFFFFFF)
	big := fill(70000, 3)
	r.fmt0(5, 0, 70000, MsgVideo, 1).data(big)
	c = newReader(r.Bytes(), ChunkLimits{})
	msgs = readAll(t, c)
	if c.chunkSize != 0xFFFFFF {
		t.Fatalf("clamped chunk size %#x, want 0xFFFFFF", c.chunkSize)
	}
	if len(msgs) != 2 || !bytes.Equal(msgs[0].Payload, []byte{0x7F, 0xFF, 0xFF, 0xFF}) || !bytes.Equal(msgs[1].Payload, big) {
		t.Fatalf("clamped stream read wrong: %d messages", len(msgs))
	}

	for _, n := range []int{0, -1, 0x1000000} {
		if err := NewChunkWriter(io.Discard).SetChunkSize(n); err == nil {
			t.Fatalf("SetChunkSize(%d) accepted", n)
		}
	}
}

func TestChunkAbort(t *testing.T) {
	var b raw
	b.fmt0(5, 0, 300, MsgVideo, 1).data(fill(128, 1)) // partial message
	b.fmt0(2, 0, 4, MsgAbort, 0).u32(5)
	b.fmt0(2, 0, 4, MsgAbort, 0).u32(77) // unknown csid: harmless
	b.fmt0(5, 9, 4, MsgVideo, 1).data([]byte("new!"))
	c := newReader(b.Bytes(), ChunkLimits{})
	msgs := readAll(t, c)
	if len(msgs) != 3 || msgs[0].Type != MsgAbort || !bytes.Equal(msgs[0].Payload, []byte{0, 0, 0, 5}) ||
		msgs[1].Type != MsgAbort || string(msgs[2].Payload) != "new!" || msgs[2].Timestamp != 9 {
		t.Fatalf("messages %+v", msgs)
	}
	if c.pending != 0 {
		t.Fatalf("pending %d after abort", c.pending)
	}

	// After an abort a type 3 header starts a fresh message with the old header.
	b.Reset()
	b.fmt0(5, 7, 200, MsgAudio, 1).data(fill(128, 2))
	b.fmt0(2, 0, 4, MsgAbort, 0).u32(5)
	b.basic(3, 5).data(fill(128, 3)).basic(3, 5).data(fill(72, 4))
	msgs = readAll(t, newReader(b.Bytes(), ChunkLimits{}))
	want := append(fill(128, 3), fill(72, 4)...)
	if len(msgs) != 2 || !bytes.Equal(msgs[1].Payload, want) || msgs[1].Timestamp != 7 {
		t.Fatalf("type 3 after abort: %+v", msgs)
	}
}

// A type 0, 1 or 2 header on a chunk stream with a partial message abandons
// that message and starts the new one.
func TestChunkHeaderMidMessageDiscardsPartial(t *testing.T) {
	for _, format := range []byte{0, 1, 2} {
		var b raw
		b.fmt0(5, 100, 300, MsgVideo, 1).data(fill(128, 1))
		switch format {
		case 0:
			b.fmt0(5, 200, 4, MsgVideo, 1)
		case 1:
			b.basic(1, 5).u24(100).u24(4).u8(MsgVideo)
		case 2:
			b.fmt0(6, 0, 1, MsgAudio, 1).u8(0) // unrelated stream in between
			b.basic(2, 5).u24(100)
		}
		if format == 2 {
			b.data(fill(128, 2)).basic(3, 5).data(fill(128, 3)).basic(3, 5).data(fill(44, 4))
		} else {
			b.data([]byte("next"))
		}
		c := newReader(b.Bytes(), ChunkLimits{})
		msgs := readAll(t, c)
		last := msgs[len(msgs)-1]
		if last.Timestamp != 200 || (format != 2 && string(last.Payload) != "next") ||
			(format == 2 && len(last.Payload) != 300) {
			t.Fatalf("format %d: last message ts %d len %d", format, last.Timestamp, len(last.Payload))
		}
		if c.pending != 0 {
			t.Fatalf("format %d: pending %d", format, c.pending)
		}
	}
}

func TestChunkPayloadNotAliased(t *testing.T) {
	var out bytes.Buffer
	w := NewChunkWriter(&out)
	small := Message{Type: MsgAudio, StreamID: 1, Timestamp: 1, Payload: fill(50, 1)}
	large := Message{Type: MsgVideo, StreamID: 1, Timestamp: 2, Payload: fill(1000, 2)}
	for _, m := range []Message{small, small, large, large} {
		if err := w.WriteMessage(4, m); err != nil {
			t.Fatal(err)
		}
	}
	src := out.Bytes()
	c := newReader(src, ChunkLimits{})
	got := make([]Message, 0, 4)
	for range 4 {
		m, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
		for i := range m.Payload {
			m.Payload[i] ^= 0xFF // scribble over what we were given
		}
	}
	for i, want := range []Message{small, small, large, large} {
		for j := range got[i].Payload {
			got[i].Payload[j] ^= 0xFF
		}
		if !bytes.Equal(got[i].Payload, want.Payload) {
			t.Fatalf("message %d changed after later reads", i)
		}
	}
	if &got[0].Payload[0] == &got[1].Payload[0] || &got[2].Payload[0] == &got[3].Payload[0] {
		t.Fatal("payloads share memory")
	}
	if !bytes.Equal(src, out.Bytes()) {
		t.Fatal("input mutated")
	}
}

func TestChunkHostile(t *testing.T) {
	isLimit := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("want a limit error, got %v", err)
		}
	}

	t.Run("oversize length", func(t *testing.T) {
		var b raw
		b.fmt0(3, 0, 0xFFFFFF, MsgVideo, 1) // header only: no payload follows
		c := newReader(b.Bytes(), ChunkLimits{MaxMessage: 64 << 10})
		_, err := c.ReadMessage()
		isLimit(t, err)
		if c.BytesRead() != 12 {
			t.Fatalf("read %d bytes past the header", c.BytesRead()-12)
		}
	})

	t.Run("65 chunk streams", func(t *testing.T) {
		var b raw
		for csid := uint32(3); csid < 3+64; csid++ {
			b.fmt0(csid, 0, 1, MsgAudio, 1).u8(1)
		}
		c := newReader(b.Bytes(), ChunkLimits{})
		if got := len(readAll(t, c)); got != 64 {
			t.Fatalf("read %d messages", got)
		}
		b.fmt0(3+64, 0, 1, MsgAudio, 1).u8(1)
		c = newReader(b.Bytes(), ChunkLimits{})
		for range 64 {
			if _, err := c.ReadMessage(); err != nil {
				t.Fatal(err)
			}
		}
		_, err := c.ReadMessage()
		isLimit(t, err)
	})

	for _, v := range []uint32{0, 0x80000000, 0xFFFFFFFF} {
		var b raw
		b.fmt0(2, 0, 4, MsgSetChunkSize, 0).u32(v)
		_, err := newReader(b.Bytes(), ChunkLimits{}).ReadMessage()
		isLimit(t, err)
	}

	t.Run("short control payload", func(t *testing.T) {
		for _, typ := range []uint8{MsgSetChunkSize, MsgAbort} {
			var b raw
			b.fmt0(2, 0, 2, typ, 0).data([]byte{0, 1})
			_, err := newReader(b.Bytes(), ChunkLimits{}).ReadMessage()
			isLimit(t, err)
		}
	})

	t.Run("pending bytes", func(t *testing.T) {
		var b raw
		for csid := uint32(3); csid < 13; csid++ {
			b.fmt0(csid, 0, 1000, MsgVideo, 1).data(fill(128, byte(csid)))
		}
		c := newReader(b.Bytes(), ChunkLimits{MaxMessage: 64 << 10, MaxPending: 1000})
		_, err := c.ReadMessage()
		isLimit(t, err)
		if c.pending > 1000 {
			t.Fatalf("pending %d over the limit", c.pending)
		}
	})

	t.Run("buffers grow as data arrives", func(t *testing.T) {
		var b raw
		b.fmt0(3, 0, 60000, MsgVideo, 1).data(fill(128, 1))
		c := newReader(b.Bytes(), ChunkLimits{MaxMessage: 64 << 10})
		if _, err := c.ReadMessage(); !errors.Is(err, io.EOF) {
			t.Fatalf("want EOF, got %v", err)
		}
		if st := c.streams[3]; st == nil || len(st.buf) != 128 || cap(st.buf) >= 60000 {
			t.Fatalf("partial buffer pre-allocated")
		}
		if c.pending != 128 {
			t.Fatalf("pending %d", c.pending)
		}
	})

	t.Run("first chunk not type 0", func(t *testing.T) {
		for _, format := range []byte{1, 2, 3} {
			var b raw
			b.basic(format, 7).u24(0).u24(1).u8(MsgAudio).u8(1)
			_, err := newReader(b.Bytes(), ChunkLimits{}).ReadMessage()
			isLimit(t, err)
		}
	})
}

func TestChunkSetLimits(t *testing.T) {
	var b raw
	b.fmt0(2, 0, 4, MsgSetChunkSize, 0).u32(1 << 20)
	b.fmt0(3, 0, 100<<10, MsgVideo, 1).data(fill(100<<10, 1))
	c := newReader(b.Bytes(), ChunkLimits{MaxMessage: 64 << 10})
	if _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	c.SetLimits(ChunkLimits{}) // back to the 8 MiB default
	if msgs := readAll(t, c); len(msgs) != 1 || len(msgs[0].Payload) != 100<<10 {
		t.Fatal("raised limit not applied")
	}
}

func TestChunkSetLimitsLowered(t *testing.T) {
	var b raw
	b.fmt0(3, 0, 100<<10, MsgVideo, 1)
	_, err := newReader(b.Bytes(), ChunkLimits{}).ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("100 KiB under the default limit: %v", err)
	}
	c := newReader(b.Bytes(), ChunkLimits{})
	c.SetLimits(ChunkLimits{MaxMessage: 64 << 10})
	if _, err := c.ReadMessage(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("lowered limit not applied: %v", err)
	}
}

func TestChunkWriterRejects(t *testing.T) {
	w := NewChunkWriter(io.Discard)
	for _, csid := range []uint32{0, 1, 65600} {
		if err := w.WriteMessage(csid, Message{Payload: []byte{1}}); err == nil {
			t.Fatalf("csid %d accepted", csid)
		}
	}
	if err := w.WriteMessage(3, Message{Payload: make([]byte, 0x1000000)}); err == nil {
		t.Fatal("oversize payload accepted")
	}
}

func TestChunkZeroLengthMessage(t *testing.T) {
	var out bytes.Buffer
	w := NewChunkWriter(&out)
	for _, ts := range []uint32{5, 5} {
		if err := w.WriteMessage(3, Message{Type: MsgDataAMF0, StreamID: 1, Timestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	msgs := readAll(t, newReader(out.Bytes(), ChunkLimits{}))
	if len(msgs) != 2 || msgs[1].Payload == nil || len(msgs[1].Payload) != 0 || msgs[1].Timestamp != 5 {
		t.Fatalf("zero-length messages %+v", msgs)
	}
}

func FuzzChunkReader(f *testing.F) {
	for _, size := range []int{128, 4096} {
		var out bytes.Buffer
		w := NewChunkWriter(&out)
		_ = w.SetChunkSize(size)
		for _, pm := range roundTripPlan(1) {
			_ = w.WriteMessage(3, pm.m)
		}
		f.Add(out.Bytes())
	}
	lim := ChunkLimits{MaxMessage: 1 << 16, MaxStreams: 8, MaxPending: 1 << 17}
	f.Fuzz(func(t *testing.T, data []byte) {
		c := newReader(data, lim)
		for {
			m, err := c.ReadMessage()
			if err != nil {
				break
			}
			if len(m.Payload) > lim.MaxMessage {
				t.Fatalf("payload %d over MaxMessage", len(m.Payload))
			}
			if c.pending > lim.MaxPending || len(c.streams) > lim.MaxStreams {
				t.Fatalf("limits exceeded: pending %d streams %d", c.pending, len(c.streams))
			}
		}
		if c.BytesRead() > uint64(len(data)) {
			t.Fatalf("BytesRead %d > input %d", c.BytesRead(), len(data))
		}
	})
}
