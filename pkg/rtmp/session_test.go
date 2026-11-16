package rtmp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// sessionPeer is the far end of a net.Pipe: a goroutine reads every message
// the session writes into msgs, and stream bytes are written by another.
type sessionPeer struct {
	conn net.Conn
	msgs chan Message
}

// newSessionPair connects a session to a peer. When handshake is set, the
// session runs its handshake against the peer's opposite one before the peer
// starts reading chunks; otherwise both sides start with the chunk stream.
func newSessionPair(t *testing.T, cfg Config, r role, handshake bool) (*session, *sessionPeer) {
	t.Helper()
	a, b := net.Pipe()
	s := newSession(a, cfg, r)
	p := &sessionPeer{conn: b, msgs: make(chan Message, 1024)}
	t.Cleanup(func() {
		_ = s.Close()
		_ = b.Close()
	})
	br := bufio.NewReader(b)
	hsDone := make(chan error, 1)
	go func() {
		if handshake {
			rw := struct {
				io.Reader
				io.Writer
			}{br, b}
			hs := ClientHandshake
			if r == roleClient {
				hs = ServerHandshake
			}
			if err := hs(rw, rand.Reader); err != nil {
				hsDone <- err
				close(p.msgs)
				return
			}
		}
		hsDone <- nil
		cr := NewChunkReader(br, ChunkLimits{})
		for {
			m, err := cr.ReadMessage()
			if err != nil {
				close(p.msgs)
				return
			}
			p.msgs <- m
		}
	}()
	if handshake {
		if err := s.handshake(); err != nil {
			t.Fatalf("session handshake: %v", err)
		}
		if err := <-hsDone; err != nil {
			t.Fatalf("peer handshake: %v", err)
		}
	}
	return s, p
}

// stream encodes messages as the peer's chunk stream and returns the bytes
// and the stream offset after each message.
func stream(t *testing.T, msgs ...Message) ([]byte, []int) {
	t.Helper()
	var buf bytes.Buffer
	w := NewChunkWriter(&buf)
	ends := make([]int, 0, len(msgs))
	for _, m := range msgs {
		csid := uint32(3)
		if m.Type <= MsgSetPeerBandwidth {
			csid = controlCSID
		}
		if err := w.WriteMessage(csid, m); err != nil {
			t.Fatal(err)
		}
		ends = append(ends, buf.Len())
	}
	return buf.Bytes(), ends
}

// write sends b to the session from a goroutine, since net.Pipe writes block
// until the session reads.
func (p *sessionPeer) write(b []byte) {
	go func() { _, _ = p.conn.Write(b) }()
}

// next returns the next message the session wrote, failing after a second.
func (p *sessionPeer) next(t *testing.T) Message {
	t.Helper()
	select {
	case m, ok := <-p.msgs:
		if !ok {
			t.Fatal("peer: session connection closed")
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("peer: no message from the session")
	}
	return Message{}
}

// quiet is how long none waits for an unexpected message.
const quiet = 50 * time.Millisecond

// none fails when the session writes a message within quiet.
func (p *sessionPeer) none(t *testing.T) {
	t.Helper()
	select {
	case m, ok := <-p.msgs:
		if ok {
			t.Fatalf("peer: unexpected message type %d %x", m.Type, m.Payload)
		}
	case <-time.After(quiet):
	}
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func audio(ts uint32, n int) Message {
	return Message{Type: MsgAudio, StreamID: 1, Timestamp: ts, Payload: bytes.Repeat([]byte{0xAF}, n)}
}

// drain reads n messages through the session.
func drain(t *testing.T, s *session, n int) []Message {
	t.Helper()
	out := make([]Message, 0, n)
	for range n {
		m, err := s.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// nextErr is the error of s.next, limited to two seconds, so a session that
// never times out fails the test instead of hanging it.
func nextErr(t *testing.T, s *session) error {
	t.Helper()
	const d = 2 * time.Second
	c := make(chan error, 1)
	go func() {
		_, err := s.next()
		c <- err
	}()
	select {
	case err := <-c:
		return err
	case <-time.After(d):
		t.Fatalf("next did not return within %v", d)
	}
	return nil
}

func wantAck(t *testing.T, m Message, seq int) {
	t.Helper()
	if m.Type != MsgAck || m.StreamID != 0 || !bytes.Equal(m.Payload, u32(uint32(seq))) {
		t.Fatalf("got type %d stream %d payload %x, want Ack %d", m.Type, m.StreamID, m.Payload, seq)
	}
}

func TestSessionAcknowledges(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleServer, false)
	msgs := make([]Message, 0, 8)
	msgs = append(msgs, Message{Type: MsgWindowAckSize, Payload: u32(8192)})
	for i := range 7 {
		msgs = append(msgs, audio(uint32(i*20), 3000))
	}
	b, ends := stream(t, msgs...)
	p.write(b)

	got := drain(t, s, 7) // the Window Ack Size is consumed
	for i, m := range got {
		if m.Type != MsgAudio || m.Timestamp != uint32(i*20) || len(m.Payload) != 3000 {
			t.Fatalf("message %d: type %d ts %d len %d", i, m.Type, m.Timestamp, len(m.Payload))
		}
	}
	// Acks follow the first message ending at or past each window boundary.
	last := 0
	var want []int
	for _, e := range ends {
		if e-last >= 8192 {
			want = append(want, e)
			last = e
		}
	}
	if len(want) != 2 {
		t.Fatalf("test setup: %d acks expected, want 2", len(want))
	}
	for _, seq := range want {
		wantAck(t, p.next(t), seq)
	}
	p.none(t)
	if s.cr.BytesRead() != uint64(len(b)) {
		t.Fatalf("BytesRead %d, want %d", s.cr.BytesRead(), len(b))
	}
}

func TestSessionWindowClamped(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleServer, false)
	msgs := make([]Message, 0, 7)
	msgs = append(msgs, Message{Type: MsgWindowAckSize, Payload: u32(1)})
	for i := range 6 {
		msgs = append(msgs, audio(uint32(i), 1000))
	}
	b, ends := stream(t, msgs...)
	p.write(b)
	drain(t, s, 6)

	// A window of 1 would ack right after the Window Ack Size itself; the
	// first ack must instead follow the first message ending past 4096.
	want := 0
	for _, e := range ends {
		if e >= 4096 {
			want = e
			break
		}
	}
	wantAck(t, p.next(t), want)
	p.none(t)
}

func TestSessionNoAckWithoutWindow(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleClient, false)
	msgs := make([]Message, 0, 4)
	for i := range 4 {
		msgs = append(msgs, audio(uint32(i), 3000))
	}
	b, _ := stream(t, msgs...)
	p.write(b)
	drain(t, s, 4)
	p.none(t)
}

func isPingRequest(m Message) bool {
	return m.Type == MsgUserControl && len(m.Payload) == 6 &&
		binary.BigEndian.Uint16(m.Payload) == ucPingRequest
}

func TestSessionPingAndIdle(t *testing.T) {
	cfg := Config{PingInterval: 100 * time.Millisecond, IdleTimeout: 300 * time.Millisecond}

	t.Run("server pings then fails idle", func(t *testing.T) {
		s, p := newSessionPair(t, cfg, roleServer, true)
		start := time.Now()
		err := nextErr(t, s)
		elapsed := time.Since(start)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("next: %v, want an idle timeout", err)
		}
		if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("idle error after %v, want about 300ms", elapsed)
		}
		if m := p.next(t); !isPingRequest(m) {
			t.Fatalf("got type %d %x, want PingRequest", m.Type, m.Payload)
		}
	})

	t.Run("inbound bytes reset idle", func(t *testing.T) {
		s, p := newSessionPair(t, cfg, roleServer, false)
		b, _ := stream(t, audio(0, 10))
		go func() {
			time.Sleep(200 * time.Millisecond)
			_, _ = p.conn.Write(b)
		}()
		go func() {
			for range p.msgs { // keep reading pings
			}
		}()
		start := time.Now()
		if m, err := s.next(); err != nil || m.Type != MsgAudio {
			t.Fatalf("next: %v type %d", err, m.Type)
		}
		before := s.LastInbound()
		if before.Before(start.Add(150*time.Millisecond)) || before.After(time.Now()) {
			t.Fatalf("LastInbound %v not at the message arrival", before.Sub(start))
		}
		err := nextErr(t, s)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("next: %v, want an idle timeout", err)
		}
		if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
			t.Fatalf("idle error after %v, want at least 500ms", elapsed)
		}
	})

	t.Run("client never pings", func(t *testing.T) {
		s, p := newSessionPair(t, cfg, roleClient, true)
		if err := nextErr(t, s); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("next: %v, want an idle timeout", err)
		}
		select {
		case m, ok := <-p.msgs:
			if ok {
				t.Fatalf("client sent type %d %x", m.Type, m.Payload)
			}
		default:
		}
	})
}

// A consumer that calls next later than IdleTimeout after the last byte still
// gets the bytes that are waiting; idle fails only a read that finds none.
func TestSessionSlowConsumer(t *testing.T) {
	cfg := Config{IdleTimeout: 200 * time.Millisecond}
	s, p := newSessionPair(t, cfg, roleServer, false)
	b, _ := stream(t, audio(0, 10), audio(20, 10))
	p.write(b)
	time.Sleep(400 * time.Millisecond)
	for i := range 2 {
		if m, err := s.next(); err != nil || m.Type != MsgAudio {
			t.Fatalf("next %d after a slow consumer: %v type %d", i, err, m.Type)
		}
	}
	if err := nextErr(t, s); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("next: %v, want an idle timeout once nothing is waiting", err)
	}
}

func TestSessionHandshakeTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := newSession(a, Config{HandshakeTimeout: 100 * time.Millisecond}, roleServer)
	defer s.Close()
	start := time.Now()
	err := s.handshake()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("handshake: %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("handshake failed after %v", elapsed)
	}
}

func TestSessionPhaseDeadline(t *testing.T) {
	s, _ := newSessionPair(t, Config{}, roleServer, false)
	s.setPhase(100*time.Millisecond, "connect")
	start := time.Now()
	err := nextErr(t, s)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("next: %v, want a phase timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("phase timeout after %v", elapsed)
	}
}

// flvSub builds one aggregate sub-message: an FLV tag header, the data and the
// back-pointer.
func flvSub(typ uint8, ts uint32, data []byte) []byte {
	b := []byte{typ}
	b = append24(b, uint32(len(data)))
	b = append24(b, ts&0xFFFFFF)
	b = append(b, byte(ts>>24))
	b = append24(b, 0)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, uint32(11+len(data)))
}

func TestSessionAggregateSplit(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleClient, false)
	vid := []byte{0x17, 0x01, 0, 0, 0, 1, 2, 3}
	aud := []byte{0xAF, 0x01, 9, 9}
	meta := []byte{0x02, 0x00, 0x0A, 'o', 'n', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a'}
	// The sub timestamps wrap inside the aggregate, and so does the result.
	agg := flvSub(MsgVideo, 0xFFFFFFF0, vid)
	agg = append(agg, flvSub(MsgAudio, 0x00000010, aud)...)
	agg = append(agg, flvSub(MsgDataAMF0, 0x00000030, meta)...)
	b, _ := stream(t,
		Message{Type: MsgAggregate, StreamID: 1, Timestamp: 5000, Payload: agg},
		Message{Type: MsgAggregate, StreamID: 1, Timestamp: 0xFFFFFFE0, Payload: agg},
		audio(77, 3),
	)
	p.write(b)

	got := drain(t, s, 7)
	want := []struct {
		typ  uint8
		ts   uint32
		data []byte
	}{
		{MsgVideo, 5000, vid},
		{MsgAudio, 5032, aud},
		{MsgDataAMF0, 5064, meta},
		{MsgVideo, 0xFFFFFFE0, vid},
		{MsgAudio, 0x00000000, aud},
		{MsgDataAMF0, 0x00000020, meta},
		{MsgAudio, 77, []byte{0xAF, 0xAF, 0xAF}},
	}
	media := make([]Media, 0, len(got))
	for i, m := range got {
		md, ok := mediaOf(m)
		if !ok {
			t.Fatalf("message %d type %d is not media", i, m.Type)
		}
		if md.Type != want[i].typ || md.Timestamp != want[i].ts || !bytes.Equal(md.Payload, want[i].data) {
			t.Fatalf("media %d: type %d ts %#x payload %x, want type %d ts %#x payload %x",
				i, md.Type, md.Timestamp, md.Payload, want[i].typ, want[i].ts, want[i].data)
		}
		if m.StreamID != 1 {
			t.Fatalf("media %d stream id %d, want the aggregate's 1", i, m.StreamID)
		}
		media = append(media, md)
	}
	// Each payload is its own allocation: appending to one never writes into
	// the next.
	_ = append(media[0].Payload, 0xEE, 0xEE, 0xEE, 0xEE)
	if !bytes.Equal(media[1].Payload, aud) {
		t.Fatal("aggregate payloads alias each other")
	}
}

func TestSessionAggregateTruncated(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleClient, false)
	sub := flvSub(MsgVideo, 0, []byte{1, 2, 3, 4})
	// The last back-pointer may be missing; a cut tag is an error.
	b, _ := stream(t,
		Message{Type: MsgAggregate, StreamID: 1, Timestamp: 10, Payload: sub[:len(sub)-4]},
		Message{Type: MsgAggregate, StreamID: 1, Timestamp: 20, Payload: sub[:len(sub)-6]},
	)
	p.write(b)
	if m, err := s.next(); err != nil || m.Type != MsgVideo || m.Timestamp != 10 {
		t.Fatalf("next: %v type %d ts %d", err, m.Type, m.Timestamp)
	}
	if _, err := s.next(); err == nil {
		t.Fatal("truncated aggregate accepted")
	}
}

func TestSessionUserControl(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleClient, false)
	ping := binary.BigEndian.AppendUint16(nil, ucPingRequest)
	ping = binary.BigEndian.AppendUint32(ping, 0x01020304)
	eof := binary.BigEndian.AppendUint16(nil, ucStreamEOF)
	eof = binary.BigEndian.AppendUint32(eof, 1)
	pong := binary.BigEndian.AppendUint16(nil, ucPingResponse)
	pong = binary.BigEndian.AppendUint32(pong, 7)
	b, _ := stream(t,
		Message{Type: MsgUserControl, Payload: ping},
		Message{Type: MsgUserControl, Payload: pong},
		Message{Type: MsgUserControl, Payload: eof},
	)
	p.write(b)

	// Pings are answered and consumed; stream events reach the caller.
	m, err := s.next()
	if err != nil {
		t.Fatal(err)
	}
	ev, data, err := parseUserControl(m.Payload)
	if err != nil || m.Type != MsgUserControl || ev != ucStreamEOF || !bytes.Equal(data, u32(1)) {
		t.Fatalf("got type %d event %d data %x err %v, want StreamEOF(1)", m.Type, ev, data, err)
	}
	r := p.next(t)
	want := binary.BigEndian.AppendUint16(nil, ucPingResponse)
	want = binary.BigEndian.AppendUint32(want, 0x01020304)
	if r.Type != MsgUserControl || r.StreamID != 0 || !bytes.Equal(r.Payload, want) {
		t.Fatalf("got type %d payload %x, want PingResponse %x", r.Type, r.Payload, want)
	}
	p.none(t)
}

func TestSessionPeerBandwidth(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleClient, false)
	spb := func(n uint32, limit byte) Message {
		return Message{Type: MsgSetPeerBandwidth, Payload: append(u32(n), limit)}
	}
	b, _ := stream(t,
		spb(1_000_000, limitHard),
		spb(1_000_000, limitDynamic), // unchanged: no reply
		spb(2_000_000, limitSoft),    // soft and larger: ignored
		spb(500_000, limitSoft),      // soft and smaller: applied
		audio(1, 1),
	)
	p.write(b)
	drain(t, s, 1)
	for _, n := range []uint32{1_000_000, 500_000} {
		m := p.next(t)
		if m.Type != MsgWindowAckSize || !bytes.Equal(m.Payload, u32(n)) {
			t.Fatalf("got type %d payload %x, want Window Ack Size %d", m.Type, m.Payload, n)
		}
	}
	p.none(t)
}

func TestSessionWrites(t *testing.T) {
	s, p := newSessionPair(t, Config{}, roleServer, false)
	if err := s.sendWindowAckSize(defaultWindowAckSize); err != nil {
		t.Fatal(err)
	}
	if err := s.sendPeerBandwidth(defaultWindowAckSize, limitDynamic); err != nil {
		t.Fatal(err)
	}
	if err := s.setChunkSize(defaultOutChunkSize); err != nil {
		t.Fatal(err)
	}
	if err := s.sendUserControl(ucStreamBegin, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCommand(1, "onStatus", 0.0, Null{}, Object{{Key: "code", Value: "NetStream.Publish.Start"}}); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		typ     uint8
		sid     uint32
		payload []byte
	}{
		{MsgWindowAckSize, 0, u32(2_500_000)},
		{MsgSetPeerBandwidth, 0, append(u32(2_500_000), limitDynamic)},
		{MsgSetChunkSize, 0, u32(4096)},
		{MsgUserControl, 0, append([]byte{0, 0}, u32(1)...)},
	}
	for _, c := range checks {
		m := p.next(t)
		if m.Type != c.typ || m.StreamID != c.sid || !bytes.Equal(m.Payload, c.payload) {
			t.Fatalf("got type %d sid %d payload %x, want type %d sid %d payload %x",
				m.Type, m.StreamID, m.Payload, c.typ, c.sid, c.payload)
		}
	}
	m := p.next(t)
	vals, err := DecodeAMF0(m.Payload)
	if err != nil || m.Type != MsgCommandAMF0 || m.StreamID != 1 || len(vals) != 4 || vals[0] != "onStatus" {
		t.Fatalf("command: type %d sid %d vals %v err %v", m.Type, m.StreamID, vals, err)
	}
}

func TestSessionPrePublishLimit(t *testing.T) {
	s, p := newSessionPair(t, Config{MaxPrePublishMessage: 1000, MaxMessage: 5000}, roleServer, false)
	b, _ := stream(t, audio(0, 900), audio(1, 4000), audio(2, 4000))
	p.write(b)
	drain(t, s, 1)
	if _, err := s.next(); err == nil {
		t.Fatal("message over the pre-publish limit accepted")
	}

	s2, p2 := newSessionPair(t, Config{MaxPrePublishMessage: 1000, MaxMessage: 5000}, roleServer, false)
	s2.raiseMessageLimit()
	p2.write(b)
	drain(t, s2, 3)
}

func TestSessionCloseUnblocksRead(t *testing.T) {
	s, _ := newSessionPair(t, Config{}, roleServer, false)
	done := make(chan error, 1)
	go func() {
		_, err := s.next()
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("next returned no error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock next")
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.HandshakeTimeout != 5*time.Second || c.CommandTimeout != 10*time.Second ||
		c.IdleTimeout != 30*time.Second || c.PingInterval != 10*time.Second ||
		c.MaxMessage != 8<<20 || c.MaxPrePublishMessage != 64<<10 ||
		c.MaxPrePublishStreams != 8 || c.MaxPrePublishPending != 256<<10 || c.Now == nil {
		t.Fatalf("defaults: %+v", c)
	}
}
