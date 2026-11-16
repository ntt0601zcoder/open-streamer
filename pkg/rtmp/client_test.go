package rtmp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

type playResult struct {
	c   *rtmp.ClientConn
	err error
}

// startPlay runs PlayWith in a goroutine.
func startPlay(ctx context.Context, rawURL string, cfg rtmp.Config) <-chan playResult {
	res := make(chan playResult, 1)
	go func() {
		c, err := rtmp.PlayWith(ctx, rawURL, cfg)
		res <- playResult{c, err}
	}()
	return res
}

// awaitPlay returns PlayWith's result, failing after d.
func awaitPlay(t *testing.T, res <-chan playResult, d time.Duration) playResult {
	t.Helper()
	select {
	case r := <-res:
		if r.c != nil {
			t.Cleanup(func() { _ = r.c.Close() })
		}
		return r
	case <-time.After(d):
		t.Fatalf("Play did not return within %v", d)
	}
	return playResult{}
}

// play opens rawURL with the default Config, failing on error.
func play(t *testing.T, rawURL string) *rtmp.ClientConn {
	t.Helper()
	r := awaitPlay(t, startPlay(context.Background(), rawURL, rtmp.Config{}), 5*time.Second)
	if r.err != nil {
		t.Fatalf("Play: %v", r.err)
	}
	return r.c
}

// playPeer is the server end of one client connection, driven from the test
// goroutine. A goroutine reads every message the client writes into msgs.
type playPeer struct {
	conn net.Conn
	cw   *rtmp.ChunkWriter
	msgs chan rtmp.Message
}

// acceptPeers listens on 127.0.0.1 and hands each connection to the test
// once the server handshake is done. It returns the rtmp:// base address.
func acceptPeers(t *testing.T) (string, <-chan *playPeer) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	peers := make(chan *playPeer, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() {
				br := bufio.NewReader(c)
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				if err := rtmp.ServerHandshake(struct {
					io.Reader
					io.Writer
				}{br, c}, rand.Reader); err != nil {
					_ = c.Close()
					return
				}
				_ = c.SetDeadline(time.Time{})
				p := &playPeer{conn: c, cw: rtmp.NewChunkWriter(c), msgs: make(chan rtmp.Message, 256)}
				go func() {
					defer close(p.msgs)
					cr := rtmp.NewChunkReader(br, rtmp.ChunkLimits{})
					for {
						m, err := cr.ReadMessage()
						if err != nil {
							return
						}
						p.msgs <- m
					}
				}()
				peers <- p
			}()
		}
	}()
	return "rtmp://" + ln.Addr().String(), peers
}

func awaitPeer(t *testing.T, peers <-chan *playPeer) *playPeer {
	t.Helper()
	select {
	case p := <-peers:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("the client did not connect")
	}
	return nil
}

func (p *playPeer) write(t *testing.T, csid uint32, m rtmp.Message) {
	t.Helper()
	if err := p.cw.WriteMessage(csid, m); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

func (p *playPeer) command(t *testing.T, streamID uint32, vals ...any) {
	t.Helper()
	b, err := rtmp.EncodeAMF0(vals...)
	if err != nil {
		t.Fatal(err)
	}
	p.write(t, 3, rtmp.Message{Type: rtmp.MsgCommandAMF0, StreamID: streamID, Payload: b})
}

func (p *playPeer) status(t *testing.T, streamID uint32, level, code, desc string) {
	t.Helper()
	p.command(t, streamID, "onStatus", 0, nil, rtmp.Object{
		{Key: "level", Value: level}, {Key: "code", Value: code}, {Key: "description", Value: desc},
	})
}

func (p *playPeer) userControl(t *testing.T, payload ...byte) {
	t.Helper()
	p.write(t, 2, rtmp.Message{Type: rtmp.MsgUserControl, Payload: payload})
}

// next returns the next message from the client, failing after a second or
// when the client closed the connection.
func (p *playPeer) next(t *testing.T) rtmp.Message {
	t.Helper()
	select {
	case m, ok := <-p.msgs:
		if !ok {
			t.Fatal("client closed the connection")
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("no message from the client")
	}
	return rtmp.Message{}
}

// nextCommand returns the next AMF0 command from the client, skipping other
// messages.
func (p *playPeer) nextCommand(t *testing.T) []any {
	t.Helper()
	for {
		m := p.next(t)
		if m.Type != rtmp.MsgCommandAMF0 {
			continue
		}
		v, err := rtmp.DecodeAMF0(m.Payload)
		if err != nil || len(v) < 2 {
			t.Fatalf("bad command from the client: %v %x", err, m.Payload)
		}
		return v
	}
}

// expect fails unless the next command from the client is called name.
func (p *playPeer) expect(t *testing.T, name string) []any {
	t.Helper()
	v := p.nextCommand(t)
	if v[0] != name {
		t.Fatalf("client sent %v, want %s", v, name)
	}
	return v
}

// connectOK answers the client's connect as a server does.
func (p *playPeer) connectOK(t *testing.T) {
	t.Helper()
	v := p.expect(t, "connect")
	p.write(t, 2, rtmp.Message{Type: rtmp.MsgWindowAckSize, Payload: []byte{0x00, 0x26, 0x25, 0xA0}})
	p.write(t, 2, rtmp.Message{Type: rtmp.MsgSetPeerBandwidth, Payload: []byte{0x00, 0x26, 0x25, 0xA0, 2}})
	if err := p.cw.SetChunkSize(4096); err != nil {
		t.Fatal(err)
	}
	p.command(t, 0, "_result", v[1],
		rtmp.Object{{Key: "fmsVer", Value: "FMS/3,0,1,123"}, {Key: "capabilities", Value: 31}},
		rtmp.Object{{Key: "level", Value: "status"}, {Key: "code", Value: "NetConnection.Connect.Success"}})
}

// toPlay answers connect and createStream (with stream id 1) and returns
// the client's play command.
func (p *playPeer) toPlay(t *testing.T) []any {
	t.Helper()
	p.connectOK(t)
	v := p.expect(t, "createStream")
	p.command(t, 0, "_result", v[1], nil, 1)
	return p.expect(t, "play")
}

// started answers connect, createStream and play, and returns the open
// ClientConn.
func (p *playPeer) started(t *testing.T, res <-chan playResult) *rtmp.ClientConn {
	t.Helper()
	p.toPlay(t)
	p.userControl(t, 0, 0, 0, 0, 0, 1) // StreamBegin(1)
	p.status(t, 1, "status", "NetStream.Play.Start", "Started playing")
	r := awaitPlay(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Play: %v", r.err)
	}
	return r.c
}

func TestClientPlayFlow(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	srv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{})
	c := play(t, srv.URL("live/s?x=1&mode=pull"))
	for i, tag := range tags {
		m, err := readMedia(t, c)
		if err != nil {
			t.Fatalf("tag %d: ReadMedia: %v", i, err)
		}
		if m.Type != tag.Type || m.Timestamp != tag.Timestamp || !bytes.Equal(m.Payload, tag.Data) {
			t.Fatalf("tag %d: got type %d ts %d len %d, want type %d ts %d len %d",
				i, m.Type, m.Timestamp, len(m.Payload), tag.Type, tag.Timestamp, len(tag.Data))
		}
	}
	for range 2 {
		if _, err := readMedia(t, c); !errors.Is(err, io.EOF) {
			t.Fatalf("ReadMedia after the last tag: %v, want io.EOF", err)
		}
	}
	want := []rtmptest.PlayRequest{{App: "live", Stream: "s?x=1", TCURL: "rtmp://" + srv.Addr + "/live"}}
	if got := srv.Requests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("server saw %+v, want %+v", got, want)
	}
}

func TestClientAggregate(t *testing.T) {
	for _, name := range []string{"h264_aac.flv", "h265_aac.flv"} {
		t.Run(name, func(t *testing.T) {
			tags := loadFLV(t, name)
			srv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{Aggregate: true})
			c := play(t, srv.URL("live/s"))
			for i, tag := range tags {
				m, err := readMedia(t, c)
				if err != nil {
					t.Fatalf("tag %d: ReadMedia: %v", i, err)
				}
				if m.Type != tag.Type || m.Timestamp != tag.Timestamp || !bytes.Equal(m.Payload, tag.Data) {
					t.Fatalf("tag %d: got type %d ts %d len %d, want type %d ts %d len %d",
						i, m.Type, m.Timestamp, len(m.Payload), tag.Type, tag.Timestamp, len(tag.Data))
				}
			}
			if _, err := readMedia(t, c); !errors.Is(err, io.EOF) {
				t.Fatalf("ReadMedia after the last tag: %v, want io.EOF", err)
			}
			// Every tag went out inside an aggregate of at most 8.
			if n := srv.Aggregates(); n < (len(tags)+7)/8 {
				t.Fatalf("server sent %d aggregates for %d tags", n, len(tags))
			}
		})
	}
}

func TestClientStreamNotFound(t *testing.T) {
	srv := rtmptest.NewPlayServer(t, nil, rtmptest.PlayOptions{NotFound: true})
	r := awaitPlay(t, startPlay(context.Background(), srv.URL("live/secretname?token=abc"), rtmp.Config{}), 5*time.Second)
	if r.err == nil || r.c != nil {
		t.Fatalf("Play: %v, want an error", r.err)
	}
	var se *rtmp.StatusError
	if !errors.As(r.err, &se) || se.Code != "NetStream.Play.StreamNotFound" || se.Description != "stream sexxxxx not found" {
		t.Fatalf("Play: %v, want a StatusError NetStream.Play.StreamNotFound", r.err)
	}
	msg := r.err.Error()
	if !strings.HasPrefix(msg, "rtmp: client: ") || !strings.Contains(msg, "stream sexxxxx not found") || strings.Contains(msg, "secretname") || strings.Contains(msg, "abc") {
		t.Fatalf("Play error %q: want the server's description and no stream name", msg)
	}
}

func TestClientURLSplit(t *testing.T) {
	for _, tc := range []struct {
		url, addr, app, stream, tcURL string
	}{
		{"rtmp://h/live/s", "h:1935", "live", "s", "rtmp://h/live"},
		{"rtmp://h:1940/live/s", "h:1940", "live", "s", "rtmp://h:1940/live"},
		{"RTMP://h/live/s", "h:1935", "live", "s", "rtmp://h/live"},
		{"rtmp://h/live/inst/s", "h:1935", "live/inst", "s", "rtmp://h/live/inst"},
		{"rtmp://h/a/b/c/s", "h:1935", "a/b/c", "s", "rtmp://h/a/b/c"},
		{"rtmp://h/live/s/", "h:1935", "live", "s", "rtmp://h/live"},
		{"rtmp://h/live/s?x=1&mode=pull", "h:1935", "live", "s?x=1", "rtmp://h/live"},
		{"rtmp://h/live/s?mode=pull", "h:1935", "live", "s", "rtmp://h/live"},
		{"rtmp://h/live/s?mode=pull&x=1", "h:1935", "live", "s?x=1", "rtmp://h/live"},
		{"rtmp://h/live/s?b=2&mode=pull&a=%20&mode=x", "h:1935", "live", "s?b=2&a=%20", "rtmp://h/live"},
		{"rtmp://h/live/s?modex=1&m%6Fde=pull", "h:1935", "live", "s?modex=1", "rtmp://h/live"},
		{"rtmp://h/live/s?", "h:1935", "live", "s", "rtmp://h/live"},
		{"rtmp://h/live/a%2Fb", "h:1935", "live", "a/b", "rtmp://h/live"},
		{"rtmp://user:pw@h/live/s", "h:1935", "live", "s", "rtmp://h/live"},
		{"rtmp://[::1]/live/s", "[::1]:1935", "live", "s", "rtmp://[::1]/live"},
		{"rtmp://[::1]:1940/live/s", "[::1]:1940", "live", "s", "rtmp://[::1]:1940/live"},
	} {
		addr, app, stream, tcURL, err := rtmp.SplitPlayURL(tc.url)
		if err != nil || addr != tc.addr || app != tc.app || stream != tc.stream || tcURL != tc.tcURL {
			t.Errorf("split %q = %q %q %q %q %v; want %q %q %q %q",
				tc.url, addr, app, stream, tcURL, err, tc.addr, tc.app, tc.stream, tc.tcURL)
		}
	}
	for _, bad := range []string{
		"http://h/live/s",
		"rtmps://h/live/s",
		"rtmp:///live/s",
		"rtmp://h/live",
		"rtmp://h/live/",
		"rtmp://h//s",
		"rtmp://h/live//s",
		"rtmp://h:0/live/s",
		"rtmp://h:99999/live/s",
		"rtmp://h/live/%zz",
		"::",
	} {
		if _, _, _, _, err := rtmp.SplitPlayURL(bad); err == nil || !strings.HasPrefix(err.Error(), "rtmp: client: ") {
			t.Errorf("split %q: %v, want an rtmp: client: error", bad, err)
		}
	}
}

// TestClientCommands checks every message the client sends exactly.
func TestClientCommands(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPlay(context.Background(), base+"/live/inst/s?k=v&mode=pull", rtmp.Config{})
	p := awaitPeer(t, peers)
	m := p.next(t)
	got, err := rtmp.DecodeAMF0(m.Payload)
	if err != nil || m.Type != rtmp.MsgCommandAMF0 || m.StreamID != 0 {
		t.Fatalf("first message: type %d stream %d %v", m.Type, m.StreamID, err)
	}
	want := []any{"connect", 1.0, rtmp.Object{
		{Key: "app", Value: "live/inst"},
		{Key: "flashVer", Value: "LNX 9,0,124,2"},
		{Key: "tcUrl", Value: base + "/live/inst"},
		{Key: "fpad", Value: false},
		{Key: "capabilities", Value: 15.0},
		{Key: "audioCodecs", Value: 4095.0},
		{Key: "videoCodecs", Value: 255.0},
		{Key: "videoFunction", Value: 1.0},
		{Key: "fourCcList", Value: []any{"hvc1"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("connect %#v, want %#v", got, want)
	}
	p.write(t, 2, rtmp.Message{Type: rtmp.MsgSetPeerBandwidth, Payload: []byte{0x00, 0x26, 0x25, 0xA0, 2}})
	p.command(t, 0, "_result", 1, nil, rtmp.Object{{Key: "code", Value: "NetConnection.Connect.Success"}})
	for _, w := range []struct {
		typ     uint8
		payload []byte
	}{
		{rtmp.MsgSetChunkSize, []byte{0, 0, 0x10, 0}},
		{rtmp.MsgWindowAckSize, []byte{0x00, 0x26, 0x25, 0xA0}},
	} {
		m := p.next(t)
		if m.Type != w.typ || m.StreamID != 0 || !bytes.Equal(m.Payload, w.payload) {
			t.Fatalf("got type %d %x, want type %d %x", m.Type, m.Payload, w.typ, w.payload)
		}
	}
	if got := p.nextCommand(t); !reflect.DeepEqual(got, []any{"createStream", 2.0, rtmp.Null{}}) {
		t.Fatalf("got %#v, want createStream", got)
	}
	// A stream id other than 1 shows the client uses the server's.
	p.command(t, 0, "_result", 2, nil, 5)
	m = p.next(t)
	got, _ = rtmp.DecodeAMF0(m.Payload)
	if m.Type != rtmp.MsgCommandAMF0 || m.StreamID != 5 || !reflect.DeepEqual(got, []any{"play", 3.0, rtmp.Null{}, "s?k=v", -2.0}) {
		t.Fatalf("got type %d stream %d %#v, want play(s?k=v, -2) on stream 5", m.Type, m.StreamID, got)
	}
	m = p.next(t)
	if m.Type != rtmp.MsgUserControl || !bytes.Equal(m.Payload, []byte{0, 3, 0, 0, 0, 5, 0, 0, 0x03, 0xE8}) {
		t.Fatalf("got type %d %x, want SetBufferLength(5, 1000)", m.Type, m.Payload)
	}
	p.userControl(t, 0, 0, 0, 0, 0, 5)
	p.status(t, 5, "status", "NetStream.Play.Reset", "Playing and resetting")
	p.status(t, 5, "status", "NetStream.Play.Start", "Started playing")
	r := awaitPlay(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Play: %v", r.err)
	}
	p.write(t, 4, rtmp.Message{Type: rtmp.MsgAudio, StreamID: 5, Timestamp: 9, Payload: []byte{0xAF, 1, 2}})
	if m, err := readMedia(t, r.c); err != nil || m.Type != rtmp.MsgAudio || m.Timestamp != 9 {
		t.Fatalf("ReadMedia: %v %+v", err, m)
	}
	// The client never pings and sends nothing more on its own.
	select {
	case m := <-p.msgs:
		t.Fatalf("client sent type %d %x after play", m.Type, m.Payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestClientErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serve  func(t *testing.T, p *playPeer)
		code   string // StatusError code, "" when none
		inText string
	}{
		{"connect _error", func(t *testing.T, p *playPeer) {
			p.expect(t, "connect")
			p.command(t, 0, "_error", 1, nil, rtmp.Object{
				{Key: "level", Value: "error"}, {Key: "code", Value: "NetConnection.Connect.Rejected"}, {Key: "description", Value: "no such app"},
			})
		}, "NetConnection.Connect.Rejected", "connect: NetConnection.Connect.Rejected: no such app"},
		{"createStream _error", func(t *testing.T, p *playPeer) {
			p.connectOK(t)
			v := p.expect(t, "createStream")
			p.command(t, 0, "_error", v[1], nil, rtmp.Object{{Key: "code", Value: "X.Busy"}, {Key: "description", Value: "busy"}})
		}, "X.Busy", "createStream: X.Busy: busy"},
		{"play failed", func(t *testing.T, p *playPeer) {
			p.toPlay(t)
			p.status(t, 1, "error", "NetStream.Play.Failed", "cannot play")
		}, "NetStream.Play.Failed", "play: NetStream.Play.Failed: cannot play"},
		{"play status level error", func(t *testing.T, p *playPeer) {
			p.toPlay(t)
			p.status(t, 1, "error", "NetStream.Play.Vendor", "denied")
		}, "NetStream.Play.Vendor", "denied"},
		{"play _error", func(t *testing.T, p *playPeer) {
			v := p.toPlay(t)
			p.command(t, 1, "_error", v[1], nil, rtmp.Object{{Key: "code", Value: "NetStream.Play.Failed"}, {Key: "description", Value: "no"}})
		}, "NetStream.Play.Failed", "play: NetStream.Play.Failed: no"},
		{"bad createStream result", func(t *testing.T, p *playPeer) {
			p.connectOK(t)
			v := p.expect(t, "createStream")
			p.command(t, 0, "_result", v[1], nil, "one")
		}, "", "createStream"},
		// An onStatus of level error before play fails at once, well within
		// the 2 s wait, not after the 10 s CommandTimeout.
		{"connect onStatus error", func(t *testing.T, p *playPeer) {
			p.expect(t, "connect")
			p.status(t, 0, "error", "NetConnection.Connect.Rejected", "auth failed")
		}, "NetConnection.Connect.Rejected", "connect: NetConnection.Connect.Rejected: auth failed"},
		{"createStream onStatus error", func(t *testing.T, p *playPeer) {
			p.connectOK(t)
			p.expect(t, "createStream")
			p.status(t, 0, "error", "NetConnection.Call.Failed", "no streams")
		}, "NetConnection.Call.Failed", "createStream: NetConnection.Call.Failed: no streams"},
		{"server closes", func(t *testing.T, p *playPeer) {
			p.expect(t, "connect")
			_ = p.conn.Close()
		}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, peers := acceptPeers(t)
			res := startPlay(context.Background(), base+"/live/s", rtmp.Config{})
			tc.serve(t, awaitPeer(t, peers))
			r := awaitPlay(t, res, 2*time.Second)
			if r.err == nil || r.c != nil {
				t.Fatalf("Play: %v, want an error", r.err)
			}
			if !strings.HasPrefix(r.err.Error(), "rtmp: client: ") || !strings.Contains(r.err.Error(), tc.inText) {
				t.Fatalf("Play error %q, want prefix \"rtmp: client: \" and %q", r.err, tc.inText)
			}
			var se *rtmp.StatusError
			if gotStatus := errors.As(r.err, &se); gotStatus != (tc.code != "") || (gotStatus && se.Code != tc.code) {
				t.Fatalf("Play error %v: StatusError %v, want code %q", r.err, se, tc.code)
			}
		})
	}
}

func TestClientEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, p *playPeer)
	}{
		{"StreamEOF", func(t *testing.T, p *playPeer) { p.userControl(t, 0, 1, 0, 0, 0, 1) }},
		{"Play.Stop", func(t *testing.T, p *playPeer) {
			p.status(t, 1, "status", "NetStream.Play.Stop", "Stopped")
		}},
		{"UnpublishNotify", func(t *testing.T, p *playPeer) {
			p.status(t, 1, "status", "NetStream.Play.UnpublishNotify", "gone")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, peers := acceptPeers(t)
			res := startPlay(context.Background(), base+"/live/s", rtmp.Config{})
			p := awaitPeer(t, peers)
			c := p.started(t, res)
			go func() {
				for range p.msgs {
				}
			}()
			// Mid-stream notices that do not end the stream.
			p.status(t, 1, "status", "NetStream.Play.Start", "again")
			p.status(t, 1, "status", "NetStream.Play.PublishNotify", "back")
			p.command(t, 0, "onBWDone", 0, nil)
			p.userControl(t, 0, 0, 0, 0, 0, 1)
			p.userControl(t, 0, 2, 0, 0, 0, 1) // StreamDry
			meta, _ := rtmp.EncodeAMF0("onMetaData", rtmp.ECMAArray{{Key: "width", Value: 640.0}})
			set, _ := rtmp.EncodeAMF0("@setDataFrame")
			p.write(t, 5, rtmp.Message{Type: rtmp.MsgDataAMF0, StreamID: 1, Payload: append(set, meta...)})
			p.write(t, 6, rtmp.Message{Type: rtmp.MsgVideo, StreamID: 1, Timestamp: 40, Payload: []byte{0x17, 1}})
			tc.end(t, p)
			if m, err := readMedia(t, c); err != nil || m.Type != rtmp.MsgDataAMF0 || !bytes.Equal(m.Payload, meta) {
				t.Fatalf("metadata: %v type %d %x, want @setDataFrame stripped", err, m.Type, m.Payload)
			}
			if m, err := readMedia(t, c); err != nil || m.Type != rtmp.MsgVideo || m.Timestamp != 40 {
				t.Fatalf("video: %v %+v", err, m)
			}
			for range 2 {
				if _, err := readMedia(t, c); !errors.Is(err, io.EOF) {
					t.Fatalf("ReadMedia after %s: %v, want io.EOF", tc.name, err)
				}
			}
		})
	}
}

func TestClientErrorDuringPlay(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPlay(context.Background(), base+"/live/s", rtmp.Config{})
	p := awaitPeer(t, peers)
	c := p.started(t, res)
	p.status(t, 1, "error", "NetStream.Play.Failed", "source lost")
	for range 2 {
		_, err := readMedia(t, c)
		var se *rtmp.StatusError
		if !errors.As(err, &se) || se.Code != "NetStream.Play.Failed" || !strings.HasPrefix(err.Error(), "rtmp: client: ") {
			t.Fatalf("ReadMedia: %v, want the server's NetStream.Play.Failed", err)
		}
	}
}

// A server's description that echoes the play name, with or without its
// query, carries the redacted name instead (the first 2 characters kept),
// in the StatusError and in the error text, at every stage. Names of up to
// 2 characters are left alone.
func TestClientRedactsStreamName(t *testing.T) {
	const name = "secretname"
	for _, tc := range []struct {
		name  string
		path  string
		serve func(t *testing.T, p *playPeer, res <-chan playResult) error
		want  string // the Description received
	}{
		{"play onStatus", "live/" + name + "?token=abc", func(t *testing.T, p *playPeer, res <-chan playResult) error {
			p.toPlay(t)
			p.status(t, 1, "error", "NetStream.Play.StreamNotFound", "no stream live/"+name+"?token=abc, also "+name)
			return awaitPlay(t, res, 2*time.Second).err
		}, "no stream live/sexxxxx, also sexxxxx"},
		{"play _error", "live/" + name, func(t *testing.T, p *playPeer, res <-chan playResult) error {
			v := p.toPlay(t)
			p.command(t, 1, "_error", v[1], nil, rtmp.Object{{Key: "code", Value: "NetStream.Play.Failed"}, {Key: "description", Value: name + " denied"}})
			return awaitPlay(t, res, 2*time.Second).err
		}, "sexxxxx denied"},
		{"during playback", "live/" + name, func(t *testing.T, p *playPeer, res <-chan playResult) error {
			c := p.started(t, res)
			p.status(t, 1, "error", "NetStream.Play.Failed", "lost "+name)
			_, err := readMedia(t, c)
			return err
		}, "lost sexxxxx"},
		{"short name kept", "live/s", func(t *testing.T, p *playPeer, res <-chan playResult) error {
			p.toPlay(t)
			p.status(t, 1, "error", "NetStream.Play.StreamNotFound", "stream s not found")
			return awaitPlay(t, res, 2*time.Second).err
		}, "stream s not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, peers := acceptPeers(t)
			res := startPlay(context.Background(), base+"/"+tc.path, rtmp.Config{})
			p := awaitPeer(t, peers)
			err := tc.serve(t, p, res)
			var se *rtmp.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error %v, want a StatusError", err)
			}
			if se.Description != tc.want {
				t.Fatalf("Description %q, want %q", se.Description, tc.want)
			}
			if strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "abc") {
				t.Fatalf("error %q carries the stream name or its query", err)
			}
		})
	}
}

// An undecodable command during playback is counted and skipped; the
// session goes on.
func TestClientSkipsBadCommand(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPlay(context.Background(), base+"/live/s", rtmp.Config{})
	p := awaitPeer(t, peers)
	c := p.started(t, res)
	go func() {
		for range p.msgs {
		}
	}()
	p.write(t, 3, rtmp.Message{Type: rtmp.MsgCommandAMF0, Payload: []byte{0xFF, 0x01}})                   // unknown AMF0 marker
	p.write(t, 3, rtmp.Message{Type: rtmp.MsgCommandAMF0, Payload: []byte{0x00, 0, 0, 0, 0, 0, 0, 0, 0}}) // a number, not a name
	p.write(t, 6, rtmp.Message{Type: rtmp.MsgVideo, StreamID: 1, Timestamp: 40, Payload: []byte{0x17, 1}})
	p.userControl(t, 0, 1, 0, 0, 0, 1) // StreamEOF
	if m, err := readMedia(t, c); err != nil || m.Type != rtmp.MsgVideo || m.Timestamp != 40 {
		t.Fatalf("ReadMedia after bad commands: %v %+v, want the video message", err, m)
	}
	if _, err := readMedia(t, c); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadMedia: %v, want io.EOF", err)
	}
	if n := c.BadCommands(); n != 2 {
		t.Fatalf("BadCommands %d, want 2", n)
	}
}

// TestClientMediaBeforeStart covers a server that sends media before (or
// without) onStatus NetStream.Play.Start: the first audio or video message
// counts as the start, and nothing is lost.
func TestClientMediaBeforeStart(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPlay(context.Background(), base+"/live/s", rtmp.Config{})
	p := awaitPeer(t, peers)
	p.toPlay(t)
	meta, _ := rtmp.EncodeAMF0("onMetaData", rtmp.ECMAArray{{Key: "width", Value: 640.0}})
	p.write(t, 5, rtmp.Message{Type: rtmp.MsgDataAMF0, StreamID: 1, Payload: meta})
	p.write(t, 4, rtmp.Message{Type: rtmp.MsgAudio, StreamID: 1, Timestamp: 3, Payload: []byte{0xAF, 0, 0x12, 0x10}})
	r := awaitPlay(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Play: %v", r.err)
	}
	p.status(t, 1, "status", "NetStream.Play.Start", "late")
	p.write(t, 6, rtmp.Message{Type: rtmp.MsgVideo, StreamID: 1, Timestamp: 5, Payload: []byte{0x17, 0}})
	for i, want := range []rtmp.Media{
		{Type: rtmp.MsgDataAMF0, Payload: meta},
		{Type: rtmp.MsgAudio, Timestamp: 3, Payload: []byte{0xAF, 0, 0x12, 0x10}},
		{Type: rtmp.MsgVideo, Timestamp: 5, Payload: []byte{0x17, 0}},
	} {
		m, err := readMedia(t, r.c)
		if err != nil || !reflect.DeepEqual(m, want) {
			t.Fatalf("media %d: %v %+v, want %+v", i, err, m, want)
		}
	}
}

func TestClientContext(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		base, peers := acceptPeers(t)
		ctx, cancel := context.WithCancel(context.Background())
		res := startPlay(ctx, base+"/live/s", rtmp.Config{})
		p := awaitPeer(t, peers)
		p.expect(t, "connect")
		cancel()
		if r := awaitPlay(t, res, time.Second); !errors.Is(r.err, context.Canceled) || r.c != nil {
			t.Fatalf("Play: %v, want context.Canceled", r.err)
		}
	})
	t.Run("command timeout", func(t *testing.T) {
		base, peers := acceptPeers(t)
		res := startPlay(context.Background(), base+"/live/s", rtmp.Config{CommandTimeout: 100 * time.Millisecond})
		p := awaitPeer(t, peers)
		p.expect(t, "connect")
		if r := awaitPlay(t, res, time.Second); !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Play: %v, want a command timeout", r.err)
		}
	})
	t.Run("handshake timeout", func(t *testing.T) {
		var lc net.ListenConfig
		ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		res := startPlay(context.Background(), "rtmp://"+ln.Addr().String()+"/live/s", rtmp.Config{HandshakeTimeout: 100 * time.Millisecond})
		if r := awaitPlay(t, res, time.Second); !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Play: %v, want a handshake timeout", r.err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		if r := awaitPlay(t, startPlay(context.Background(), "rtmp://127.0.0.1:1/live/s", rtmp.Config{}), 2*time.Second); r.err == nil {
			t.Fatal("Play to a closed port succeeded")
		}
	})
}

func TestClientCloseUnblocks(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPlay(context.Background(), base+"/live/s", rtmp.Config{})
	p := awaitPeer(t, peers)
	c := p.started(t, res)
	done := make(chan error, 1)
	go func() {
		_, err := c.ReadMedia()
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ReadMedia after Close returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock ReadMedia")
	}
}

// requireFFmpeg returns the ffmpeg path, skipping the test when it is
// missing, or failing it when OS_REQUIRE_PEERS=1. pkg/ cannot import
// internal/testpeer, so this mirrors testpeer.Require.
func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("OS_REQUIRE_PEERS") == "1" {
			t.Fatalf("peer ffmpeg not found: %v", err)
		}
		t.Skipf("peer ffmpeg not found: %v", err)
	}
	return path
}

// freePort returns a TCP port on 127.0.0.1 that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestClientFFmpegServer(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	url := "rtmp://127.0.0.1:" + strconv.Itoa(freePort(t)) + "/live/s"
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
		"-i", "../../testdata/fixtures/h264_aac.ts", "-c", "copy", "-f", "flv", "-listen", "1", url)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	var c *rtmp.ClientConn
	deadline := time.Now().Add(5 * time.Second)
	for {
		pctx, pcancel := context.WithTimeout(t.Context(), 2*time.Second)
		var err error
		c, err = rtmp.Play(pctx, url)
		pcancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Play from ffmpeg: %v (ffmpeg: %s)", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { _ = c.Close() })

	var videoHdr, audioHdr bool
	var frames, audio int
	for frames < 25 || audio < 10 {
		m, err := readMedia(t, c)
		if err != nil {
			t.Fatalf("ReadMedia after %d video and %d audio frames: %v (ffmpeg: %s)", frames, audio, err, stderr.String())
		}
		switch m.Type {
		case rtmp.MsgVideo:
			v, err := rtmp.ParseVideoTag(m.Payload)
			if err != nil {
				t.Fatalf("video tag: %v", err)
			}
			if v.Codec != rtmp.VideoAVC {
				t.Fatalf("video codec %v, want H.264", v.Codec)
			}
			if v.Kind == rtmp.VideoSequenceStart {
				videoHdr = true
			} else {
				if !videoHdr {
					t.Fatal("video frame before the sequence header")
				}
				frames++
			}
		case rtmp.MsgAudio:
			a, err := rtmp.ParseAudioTag(m.Payload)
			if err != nil {
				t.Fatalf("audio tag: %v", err)
			}
			if a.SequenceHeader {
				audioHdr = true
			} else {
				if !audioHdr {
					t.Fatal("audio frame before the sequence header")
				}
				audio++
			}
		}
	}
}
