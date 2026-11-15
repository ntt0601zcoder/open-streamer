package rtmptest

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

const (
	playStreamID = 1
	csidControl  = 2
	csidAggr     = 7

	// exchangeTimeout bounds a play server's handshake and command
	// exchange, and each write.
	exchangeTimeout = 10 * time.Second

	// Aggregates hold at most aggMaxTags tags spanning less than aggMaxSpan
	// milliseconds.
	aggMaxTags = 8
	aggMaxSpan = 1000
)

// PlayOptions script a PlayServer.
type PlayOptions struct {
	// Aggregate sends the tags as aggregate messages (type 22) of at most 8
	// tags spanning less than 1 s, with the sub-tags' own timestamps.
	Aggregate bool
	// NotFound answers play with onStatus NetStream.Play.StreamNotFound
	// and sends no media. The description echoes the play name as sent,
	// query included ("stream <name> not found"), as some servers do.
	NotFound bool
	// Hold sends nothing after the tags and never ends the stream, like a
	// live source gone silent.
	Hold bool
}

// PlayRequest is what a client asked a PlayServer for. Stream is the play
// name as sent, query included.
type PlayRequest struct {
	App, Stream, TCURL string
}

// PlayServer is a scripted RTMP server on 127.0.0.1 that every client may
// play from. It answers connect, createStream and play; then it sends
// StreamBegin(1), onStatus NetStream.Play.Start, the tags, onStatus
// NetStream.Play.UnpublishNotify and StreamEOF(1) (unless Hold is set), and
// keeps the connection open until the client closes it. It never pings.
type PlayServer struct {
	// Addr is the host:port the server listens on.
	Addr string

	tags []rtmp.FLVTag
	opt  PlayOptions
	ln   net.Listener
	wg   sync.WaitGroup

	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	closed     bool
	reqs       []PlayRequest
	aggregates int
}

// NewPlayServer starts a play server that serves tags to every client. It is
// closed when the test ends, closing its connections and waiting for their
// goroutines.
func NewPlayServer(t testing.TB, tags []rtmp.FLVTag, opt PlayOptions) *PlayServer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("rtmptest: play server: %v", err)
	}
	s := &PlayServer{Addr: ln.Addr().String(), tags: tags, opt: opt, ln: ln, conns: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.close)
	return s
}

// URL returns rtmp://Addr/path.
func (s *PlayServer) URL(path string) string {
	return "rtmp://" + s.Addr + "/" + path
}

// Requests returns the play requests received so far, in order.
func (s *PlayServer) Requests() []PlayRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PlayRequest(nil), s.reqs...)
}

// Aggregates counts the aggregate messages sent so far.
func (s *PlayServer) Aggregates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aggregates
}

func (s *PlayServer) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *PlayServer) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.serve(c)
		}()
	}
}

// playConn is one client connection of a PlayServer.
type playConn struct {
	conn net.Conn
	cw   *rtmp.ChunkWriter
}

func (p *playConn) write(csid uint32, m rtmp.Message) error {
	_ = p.conn.SetWriteDeadline(time.Now().Add(exchangeTimeout))
	return p.cw.WriteMessage(csid, m)
}

func (p *playConn) command(streamID uint32, vals ...any) error {
	b, err := rtmp.EncodeAMF0(vals...)
	if err != nil {
		return err
	}
	return p.write(csidCommand, rtmp.Message{Type: rtmp.MsgCommandAMF0, StreamID: streamID, Payload: b})
}

func (p *playConn) status(level, code, desc string) error {
	return p.command(playStreamID, "onStatus", 0, nil, rtmp.Object{
		{Key: "level", Value: level}, {Key: "code", Value: code}, {Key: "description", Value: desc},
	})
}

func (p *playConn) userControl(event uint16) error {
	b := binary.BigEndian.AppendUint16(nil, event)
	b = binary.BigEndian.AppendUint32(b, playStreamID)
	return p.write(csidControl, rtmp.Message{Type: rtmp.MsgUserControl, Payload: b})
}

// serve runs one connection; any error ends it.
func (s *PlayServer) serve(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(exchangeTimeout))
	br := bufio.NewReader(c)
	if err := rtmp.ServerHandshake(struct {
		io.Reader
		io.Writer
	}{br, c}, rand.Reader); err != nil {
		return
	}
	p := &playConn{conn: c, cw: rtmp.NewChunkWriter(c)}
	cr := rtmp.NewChunkReader(br, rtmp.ChunkLimits{})
	if !s.exchange(p, cr) {
		return
	}
	if !s.opt.NotFound && s.sendTags(p) == nil && !s.opt.Hold {
		if p.status("status", "NetStream.Play.UnpublishNotify", "stream ended") == nil {
			_ = p.userControl(1) // StreamEOF
		}
	}
	// Hold the connection until the client closes it.
	_ = c.SetReadDeadline(time.Time{})
	_, _ = io.Copy(io.Discard, br)
}

// exchange answers commands up to play and reports whether play was
// answered.
func (s *PlayServer) exchange(p *playConn, cr *rtmp.ChunkReader) bool {
	var app, tcURL string
	for {
		vals, err := readCommand(cr)
		if err != nil {
			return false
		}
		txn := vals[1]
		switch vals[0] {
		case "connect":
			if len(vals) > 2 {
				obj, _ := vals[2].(rtmp.Object)
				v, _ := obj.Get("app")
				app, _ = v.(string)
				v, _ = obj.Get("tcUrl")
				tcURL, _ = v.(string)
			}
			if p.connected(txn) != nil {
				return false
			}
		case "createStream":
			if p.command(0, "_result", txn, nil, playStreamID) != nil {
				return false
			}
		case "play":
			var name string
			if len(vals) > 3 {
				name, _ = vals[3].(string)
			}
			s.mu.Lock()
			s.reqs = append(s.reqs, PlayRequest{App: app, Stream: name, TCURL: tcURL})
			s.mu.Unlock()
			if s.opt.NotFound {
				return p.status("error", "NetStream.Play.StreamNotFound", "stream "+name+" not found") == nil
			}
			return p.userControl(0) == nil && // StreamBegin
				p.status("status", "NetStream.Play.Start", "Started playing") == nil
		}
	}
}

// connected answers connect with Window Ack Size, Set Peer Bandwidth, Set
// Chunk Size 4096 and _result.
func (p *playConn) connected(txn any) error {
	win := binary.BigEndian.AppendUint32(nil, 2_500_000)
	if err := p.write(csidControl, rtmp.Message{Type: rtmp.MsgWindowAckSize, Payload: win}); err != nil {
		return err
	}
	if err := p.write(csidControl, rtmp.Message{Type: rtmp.MsgSetPeerBandwidth, Payload: append(win, 2)}); err != nil {
		return err
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(exchangeTimeout))
	if err := p.cw.SetChunkSize(chunkSize); err != nil {
		return err
	}
	return p.command(0, "_result", txn,
		rtmp.Object{{Key: "fmsVer", Value: "FMS/3,0,1,123"}, {Key: "capabilities", Value: 31}},
		rtmp.Object{
			{Key: "level", Value: "status"},
			{Key: "code", Value: "NetConnection.Connect.Success"},
			{Key: "description", Value: "Connection succeeded."},
			{Key: "objectEncoding", Value: 0},
		})
}

// sendTags sends the tags one message each, or grouped into aggregates.
func (s *PlayServer) sendTags(p *playConn) error {
	if !s.opt.Aggregate {
		for _, t := range s.tags {
			csid := uint32(csidData)
			switch t.Type {
			case rtmp.FLVAudio:
				csid = csidAudio
			case rtmp.FLVVideo:
				csid = csidVideo
			}
			if err := p.write(csid, rtmp.Message{Type: t.Type, StreamID: playStreamID, Timestamp: t.Timestamp, Payload: t.Data}); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < len(s.tags); {
		first := s.tags[i].Timestamp
		var body []byte
		n := 0
		for ; i < len(s.tags) && n < aggMaxTags; i, n = i+1, n+1 {
			t := s.tags[i]
			if n > 0 && int32(t.Timestamp-first) >= aggMaxSpan {
				break
			}
			body = appendAggregateTag(body, t)
		}
		if err := p.write(csidAggr, rtmp.Message{Type: rtmp.MsgAggregate, StreamID: playStreamID, Timestamp: first, Payload: body}); err != nil {
			return err
		}
		s.mu.Lock()
		s.aggregates++
		s.mu.Unlock()
	}
	return nil
}

// appendAggregateTag appends t as an aggregate sub-message: an FLV tag
// header (type, 24-bit size, 24+8-bit timestamp, stream id 0), the data and
// the back-pointer.
func appendAggregateTag(b []byte, t rtmp.FLVTag) []byte {
	n, ts := len(t.Data), t.Timestamp
	b = append(b, t.Type, byte(n>>16), byte(n>>8), byte(n),
		byte(ts>>16), byte(ts>>8), byte(ts), byte(ts>>24), 0, 0, 0)
	b = append(b, t.Data...)
	return binary.BigEndian.AppendUint32(b, uint32(11+n))
}
