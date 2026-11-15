package rtmp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

func loadFLV(t *testing.T, name string) []rtmp.FLVTag {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := rtmp.ReadFLV(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

type acceptResult struct {
	sc  *rtmp.ServerConn
	err error
}

// await returns Accept's result, failing after d.
func await(t *testing.T, res <-chan acceptResult, d time.Duration) acceptResult {
	t.Helper()
	select {
	case r := <-res:
		if r.sc != nil {
			t.Cleanup(func() { _ = r.sc.Close() })
		}
		return r
	case <-time.After(d):
		t.Fatalf("Accept did not return within %v", d)
	}
	return acceptResult{}
}

// listen runs Accept for every connection to a TCP listener on 127.0.0.1 and
// returns the rtmp:// base address and the results.
func listen(t *testing.T, cfg rtmp.Config, decide func(rtmp.PublishRequest) error) (string, <-chan acceptResult) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	res := make(chan acceptResult, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, err := rtmp.Accept(context.Background(), c, cfg, decide)
				res <- acceptResult{sc, err}
			}()
		}
	}()
	return "rtmp://" + ln.Addr().String(), res
}

// rawClient drives Accept over a net.Pipe with hand-built messages. A
// goroutine reads every message the server writes into msgs, and another
// writes what the client queues, in order, since a net.Pipe write blocks
// until the server reads.
type rawClient struct {
	conn net.Conn
	cw   *rtmp.ChunkWriter
	out  chan []byte
	msgs chan rtmp.Message
	res  chan acceptResult
}

// queue hands each chunk writer output to the writing goroutine.
type queue chan []byte

func (q queue) Write(p []byte) (int, error) {
	q <- bytes.Clone(p)
	return len(p), nil
}

// startRaw runs Accept on one end of a net.Pipe and returns a client on the
// other. When handshake is set the client completes the handshake first.
func startRaw(t *testing.T, ctx context.Context, cfg rtmp.Config, decide func(rtmp.PublishRequest) error, handshake bool) *rawClient {
	t.Helper()
	a, b := net.Pipe()
	out := make(chan []byte, 64)
	c := &rawClient{conn: b, cw: rtmp.NewChunkWriter(queue(out)), out: out, msgs: make(chan rtmp.Message, 256), res: make(chan acceptResult, 1)}
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	go func() {
		for p := range out {
			if _, err := b.Write(p); err != nil {
				return
			}
		}
	}()
	go func() {
		sc, err := rtmp.Accept(ctx, a, cfg, decide)
		c.res <- acceptResult{sc, err}
	}()
	if !handshake {
		return c
	}
	br := bufio.NewReader(b)
	_ = b.SetDeadline(time.Now().Add(2 * time.Second))
	if err := rtmp.ClientHandshake(struct {
		io.Reader
		io.Writer
	}{br, b}, rand.Reader); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	_ = b.SetDeadline(time.Time{})
	go func() {
		defer close(c.msgs)
		cr := rtmp.NewChunkReader(br, rtmp.ChunkLimits{})
		for {
			m, err := cr.ReadMessage()
			if err != nil {
				return
			}
			c.msgs <- m
		}
	}()
	return c
}

// write queues m for the server.
func (c *rawClient) write(t *testing.T, csid uint32, m rtmp.Message) {
	t.Helper()
	if err := c.cw.WriteMessage(csid, m); err != nil {
		t.Fatalf("client write: %v", err)
	}
}

func (c *rawClient) command(t *testing.T, streamID uint32, vals ...any) {
	t.Helper()
	p, err := rtmp.EncodeAMF0(vals...)
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, 3, rtmp.Message{Type: rtmp.MsgCommandAMF0, StreamID: streamID, Payload: p})
}

// next returns the next message from the server, failing after a second or
// when the server closed the connection.
func (c *rawClient) next(t *testing.T) rtmp.Message {
	t.Helper()
	select {
	case m, ok := <-c.msgs:
		if !ok {
			t.Fatal("server closed the connection")
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("no message from the server")
	}
	return rtmp.Message{}
}

// nextCommand returns the next AMF0 command from the server, skipping other
// messages.
func (c *rawClient) nextCommand(t *testing.T) []any {
	t.Helper()
	for {
		m := c.next(t)
		if m.Type != rtmp.MsgCommandAMF0 {
			continue
		}
		v, err := rtmp.DecodeAMF0(m.Payload)
		if err != nil || len(v) < 2 {
			t.Fatalf("bad command from the server: %v %x", err, m.Payload)
		}
		return v
	}
}

// closed fails unless the server closes the connection within a second,
// returning the commands it wrote before closing.
func (c *rawClient) closed(t *testing.T) [][]any {
	t.Helper()
	var cmds [][]any
	timeout := time.After(time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return cmds
			}
			if m.Type == rtmp.MsgCommandAMF0 {
				v, _ := rtmp.DecodeAMF0(m.Payload)
				cmds = append(cmds, v)
			}
		case <-timeout:
			t.Fatal("server did not close the connection")
		}
	}
}

// publishTo runs connect, createStream and publish for app and name and
// returns the accepted ServerConn.
func (c *rawClient) publishTo(t *testing.T, app, name string) *rtmp.ServerConn {
	t.Helper()
	c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: app}, {Key: "tcUrl", Value: "rtmp://h/" + app}})
	c.command(t, 0, "createStream", 2, nil)
	c.command(t, 1, "publish", 3, nil, name, "live")
	r := await(t, c.res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	return r.sc
}

// readMedia calls ReadMedia, failing after 2 s.
func readMedia(t *testing.T, sc *rtmp.ServerConn) (rtmp.Media, error) {
	t.Helper()
	type res struct {
		m   rtmp.Media
		err error
	}
	ch := make(chan res, 1)
	go func() {
		m, err := sc.ReadMedia()
		ch <- res{m, err}
	}()
	select {
	case r := <-ch:
		return r.m, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("ReadMedia did not return")
	}
	return rtmp.Media{}, nil
}

func TestServerPublishFlow(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	reqs := make(chan rtmp.PublishRequest, 1)
	base, res := listen(t, rtmp.Config{}, func(r rtmp.PublishRequest) error {
		reqs <- r
		return nil
	})
	pubErr := make(chan error, 1)
	go func() {
		pubErr <- rtmptest.Publish(context.Background(), base+"/live/s?key=1&x=2", tags, false)
	}()
	r := await(t, res, 5*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	req := <-reqs
	want := rtmp.PublishRequest{App: "live", Stream: "s", Query: "key=1&x=2", TCURL: base + "/live"}
	if req != want {
		t.Fatalf("decide saw %+v, want %+v", req, want)
	}
	if req.Path() != "live/s" || r.sc.Request() != want {
		t.Fatalf("Path %q, Request %+v", req.Path(), r.sc.Request())
	}
	for i, tag := range tags {
		m, err := readMedia(t, r.sc)
		if err != nil {
			t.Fatalf("tag %d: ReadMedia: %v", i, err)
		}
		if m.Type != tag.Type || m.Timestamp != tag.Timestamp || !bytes.Equal(m.Payload, tag.Data) {
			t.Fatalf("tag %d: got type %d ts %d len %d, want type %d ts %d len %d",
				i, m.Type, m.Timestamp, len(m.Payload), tag.Type, tag.Timestamp, len(tag.Data))
		}
	}
	if _, err := readMedia(t, r.sc); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadMedia after the last tag: %v, want io.EOF", err)
	}
	_ = r.sc.Close()
	if err := <-pubErr; err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if last := r.sc.LastInbound(); time.Since(last) > 5*time.Second {
		t.Fatalf("LastInbound %v is stale", last)
	}
}

func TestServerPublishPaced(t *testing.T) {
	var tags []rtmp.FLVTag
	for _, tag := range loadFLV(t, "h264_aac.flv") {
		if tag.Timestamp <= 300 {
			tags = append(tags, tag)
		}
	}
	base, res := listen(t, rtmp.Config{}, nil)
	start := time.Now()
	pubErr := make(chan error, 1)
	go func() { pubErr <- rtmptest.Publish(context.Background(), base+"/live/s", tags, true) }()
	r := await(t, res, 5*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	n := 0
	for {
		if _, err := readMedia(t, r.sc); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("ReadMedia: %v", err)
			}
			break
		}
		n++
	}
	_ = r.sc.Close()
	if err := <-pubErr; err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if n != len(tags) {
		t.Fatalf("got %d tags, want %d", n, len(tags))
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("paced publish of 300 ms took %v", elapsed)
	}
}

func TestServerReject(t *testing.T) {
	errUnknown := errors.New("unknown stream key")
	base, res := listen(t, rtmp.Config{}, func(rtmp.PublishRequest) error { return errUnknown })
	p, code, err := rtmptest.Dial(context.Background(), base+"/live/secretname?k=v")
	if err == nil || p != nil {
		t.Fatalf("Dial: %v, %v, want a rejection", p, err)
	}
	if code != "NetStream.Publish.BadName" {
		t.Fatalf("code %q, want NetStream.Publish.BadName", code)
	}
	if !strings.Contains(err.Error(), "stream not available") || strings.Contains(err.Error(), "secretname") {
		t.Fatalf("rejection %q: want the fixed description and no stream name", err)
	}
	r := await(t, res, 2*time.Second)
	if !errors.Is(r.err, errUnknown) || r.sc != nil {
		t.Fatalf("Accept: %v, want the decide error", r.err)
	}
}

func TestServerRejectDescription(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, func(rtmp.PublishRequest) error {
		return errors.New("busy")
	}, true)
	c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "live"}})
	c.command(t, 0, "createStream", 2, nil)
	c.command(t, 1, "publish", 3, nil, "s", "live")
	cmds := c.closed(t)
	last := cmds[len(cmds)-1]
	info, _ := last[len(last)-1].(rtmp.Object)
	code, _ := info.Get("code")
	desc, _ := info.Get("description")
	level, _ := info.Get("level")
	if last[0] != "onStatus" || code != "NetStream.Publish.BadName" || desc != "stream not available" || level != "error" {
		t.Fatalf("last command %v, want onStatus BadName \"stream not available\"", last)
	}
	if r := await(t, c.res, time.Second); r.err == nil {
		t.Fatal("Accept accepted a rejected publish")
	}
}

// TestServerReplies checks every reply of the publish exchange exactly.
func TestServerReplies(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	c.command(t, 0, "connect", 1, rtmp.Object{
		{Key: "app", Value: "live"},
		{Key: "type", Value: "nonprivate"},
		{Key: "flashVer", Value: "FMLE/3.0"},
		{Key: "tcUrl", Value: "rtmp://h/live"},
	})
	ctrl := []struct {
		typ     uint8
		payload []byte
	}{
		{rtmp.MsgWindowAckSize, []byte{0x00, 0x26, 0x25, 0xA0}},
		{rtmp.MsgSetPeerBandwidth, []byte{0x00, 0x26, 0x25, 0xA0, 2}},
		{rtmp.MsgSetChunkSize, []byte{0, 0, 0x10, 0}},
	}
	for _, w := range ctrl {
		m := c.next(t)
		if m.Type != w.typ || m.StreamID != 0 || !bytes.Equal(m.Payload, w.payload) {
			t.Fatalf("got type %d %x, want type %d %x", m.Type, m.Payload, w.typ, w.payload)
		}
	}
	wantCmd := func(want ...any) {
		t.Helper()
		if got := c.nextCommand(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	wantCmd("_result", 1.0,
		rtmp.Object{{Key: "fmsVer", Value: "FMS/3,0,1,123"}, {Key: "capabilities", Value: 31.0}},
		rtmp.Object{{Key: "level", Value: "status"}, {Key: "code", Value: "NetConnection.Connect.Success"}, {Key: "objectEncoding", Value: 0.0}})
	c.command(t, 0, "releaseStream", 2, nil, "s")
	wantCmd("_result", 2.0, rtmp.Null{}, rtmp.Undefined{})
	c.command(t, 0, "FCPublish", 3, nil, "s")
	wantCmd("_result", 3.0, rtmp.Null{}, rtmp.Undefined{})
	if got := c.nextCommand(t); got[0] != "onFCPublish" {
		t.Fatalf("got %v, want onFCPublish", got)
	}
	c.command(t, 0, "createStream", 4, nil)
	wantCmd("_result", 4.0, rtmp.Null{}, 1.0)
	c.command(t, 1, "publish", 5, nil, "s", "live")
	if m := c.next(t); m.Type != rtmp.MsgUserControl || !bytes.Equal(m.Payload, []byte{0, 0, 0, 0, 0, 1}) {
		t.Fatalf("got type %d %x, want StreamBegin(1)", m.Type, m.Payload)
	}
	got := c.nextCommand(t)
	info, _ := got[len(got)-1].(rtmp.Object)
	if code, _ := info.Get("code"); got[0] != "onStatus" || code != "NetStream.Publish.Start" {
		t.Fatalf("got %v, want onStatus NetStream.Publish.Start", got)
	}
	if r := await(t, c.res, time.Second); r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
}

func TestServerUnknownCommandsIgnored(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "live"}})
	c.command(t, 0, "_checkbw", 2, nil)
	c.command(t, 0, "getStreamLength", 3, nil, "s")
	c.command(t, 0, "FCSubscribe", 4, nil, "s")
	c.command(t, 0, "createStream", 5, nil)
	c.command(t, 1, "publish", 6, nil, "s", "live")
	r := await(t, c.res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	names := make([]string, 0, 3)
	for _, w := range []string{"_result", "_result", "onStatus"} {
		got := c.nextCommand(t)
		names = append(names, fmt.Sprint(got[0]))
		if got[0] != w {
			t.Fatalf("commands %v, want _result, _result, onStatus", names)
		}
	}
	if n := r.sc.UnknownCommands(); n != 3 {
		t.Fatalf("UnknownCommands %d, want 3", n)
	}
	c.command(t, 1, "someVendorCommand", 0, nil)
	c.write(t, 4, rtmp.Message{Type: rtmp.MsgAudio, StreamID: 1, Timestamp: 7, Payload: []byte{0xAF, 1, 2}})
	if m, err := readMedia(t, r.sc); err != nil || m.Type != rtmp.MsgAudio {
		t.Fatalf("ReadMedia: %v type %d", err, m.Type)
	}
	if n := r.sc.UnknownCommands(); n != 4 {
		t.Fatalf("UnknownCommands %d, want 4", n)
	}
	select {
	case m := <-c.msgs:
		if m.Type == rtmp.MsgCommandAMF0 {
			v, _ := rtmp.DecodeAMF0(m.Payload)
			t.Fatalf("server answered an unknown command: %v", v)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServerMediaAndEnd(t *testing.T) {
	for _, end := range []string{"deleteStream", "FCUnpublish", "closeStream"} {
		t.Run(end, func(t *testing.T) {
			c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
			sc := c.publishTo(t, "live", "s")
			go func() {
				for range c.msgs {
				}
			}()
			meta, _ := rtmp.EncodeAMF0("onMetaData", rtmp.ECMAArray{{Key: "width", Value: 640.0}})
			set, _ := rtmp.EncodeAMF0("@setDataFrame")
			c.write(t, 5, rtmp.Message{Type: rtmp.MsgDataAMF0, StreamID: 1, Payload: append(set, meta...)})
			// Larger than the pre-publish limit, accepted after publish.
			big := bytes.Repeat([]byte{0x27}, 100<<10)
			c.write(t, 6, rtmp.Message{Type: rtmp.MsgVideo, StreamID: 1, Timestamp: 40, Payload: big})
			if m, err := readMedia(t, sc); err != nil || m.Type != rtmp.MsgDataAMF0 || !bytes.Equal(m.Payload, meta) {
				t.Fatalf("metadata: %v type %d %x, want @setDataFrame stripped", err, m.Type, m.Payload)
			}
			if m, err := readMedia(t, sc); err != nil || m.Timestamp != 40 || !bytes.Equal(m.Payload, big) {
				t.Fatalf("big video: %v ts %d len %d", err, m.Timestamp, len(m.Payload))
			}
			c.command(t, 1, end, 4, nil, 1)
			for range 2 {
				if _, err := readMedia(t, sc); !errors.Is(err, io.EOF) {
					t.Fatalf("ReadMedia after %s: %v, want io.EOF", end, err)
				}
			}
		})
	}
}

func TestServerPathSplit(t *testing.T) {
	reqs := make(chan rtmp.PublishRequest, 1)
	c := startRaw(t, context.Background(), rtmp.Config{}, func(r rtmp.PublishRequest) error {
		reqs <- r
		return nil
	}, true)
	c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "/live/?token=a"}, {Key: "tcUrl", Value: "rtmp://h/live?token=a"}})
	c.command(t, 0, "createStream", 2, nil)
	c.command(t, 1, "publish", 3, nil, "s/?key=b", "live")
	if r := await(t, c.res, 2*time.Second); r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	want := rtmp.PublishRequest{App: "live", Stream: "s", Query: "token=a&key=b", TCURL: "rtmp://h/live?token=a"}
	if got := <-reqs; got != want || got.Path() != "live/s" {
		t.Fatalf("decide saw %+v (path %q), want %+v", got, got.Path(), want)
	}
}

func TestNormalizePath(t *testing.T) {
	for _, tc := range []struct{ app, stream, path, query string }{
		{"live", "s", "live/s", ""},
		{"/live/", "/s/", "live/s", ""},
		{"live/inst", "s", "live/inst/s", ""},
		{"live?a=1", "s?b=2", "live/s", "a=1&b=2"},
		{"live?a=1", "s", "live/s", "a=1"},
		{"live", "s?b=2&c=3", "live/s", "b=2&c=3"},
		{"live?", "s?", "live/s", ""},
		{"", "s", "s", ""},
		{"live", "", "live", ""},
	} {
		path, query := rtmp.NormalizePath(tc.app, tc.stream)
		if path != tc.path || query != tc.query {
			t.Errorf("NormalizePath(%q, %q) = %q, %q; want %q, %q", tc.app, tc.stream, path, query, tc.path, tc.query)
		}
	}
}

func TestServerAMF3Rejected(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "live"}, {Key: "objectEncoding", Value: 3.0}})
	cmds := c.closed(t)
	if len(cmds) != 1 || cmds[0][0] != "_error" || cmds[0][1] != 1.0 {
		t.Fatalf("server wrote %v, want only _error for txn 1", cmds)
	}
	if r := await(t, c.res, time.Second); r.err == nil {
		t.Fatal("Accept accepted an AMF3 connect")
	}
}

func TestServerHostilePeer(t *testing.T) {
	t.Run("handshake stall", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{HandshakeTimeout: 100 * time.Millisecond}, nil, false)
		c.out <- []byte{3} // C0 only
		start := time.Now()
		r := await(t, c.res, 2*time.Second)
		if !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Accept: %v, want a handshake timeout", r.err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("handshake stall failed after %v", elapsed)
		}
	})
	t.Run("command stall", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{CommandTimeout: 100 * time.Millisecond}, nil, true)
		c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "live"}})
		if r := await(t, c.res, 2*time.Second); !errors.Is(r.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Accept: %v, want a command timeout", r.err)
		}
	})
	t.Run("oversized message before publish", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
		// Format 0 header on csid 3: timestamp 0, length 0xFFFFFF, type 20,
		// stream 0, then a few payload bytes.
		hdr := []byte{0x03, 0, 0, 0, 0xFF, 0xFF, 0xFF, 20, 0, 0, 0, 0, 1, 2, 3, 4}
		c.out <- hdr
		r := await(t, c.res, 2*time.Second)
		if r.err == nil || !strings.Contains(r.err.Error(), "exceeds limit") {
			t.Fatalf("Accept: %v, want a message size error", r.err)
		}
		c.closed(t)
	})
	t.Run("AMF0 bomb in connect", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
		p, _ := rtmp.EncodeAMF0("connect", 1)
		for range 100 {
			p = append(p, 0x03, 0x00, 0x01, 'a') // object { a: object { a: …
		}
		c.write(t, 3, rtmp.Message{Type: rtmp.MsgCommandAMF0, Payload: p})
		r := await(t, c.res, 2*time.Second)
		if r.err == nil || !strings.Contains(r.err.Error(), "nesting too deep") {
			t.Fatalf("Accept: %v, want an AMF0 depth error", r.err)
		}
		c.closed(t)
	})
	t.Run("publish before connect", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
		c.command(t, 1, "publish", 1, nil, "s", "live")
		if r := await(t, c.res, 2*time.Second); r.err == nil {
			t.Fatal("Accept accepted a publish without connect")
		}
	})
}

func TestServerAcceptContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := startRaw(t, ctx, rtmp.Config{}, nil, false)
	time.AfterFunc(50*time.Millisecond, cancel)
	r := await(t, c.res, time.Second)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Accept: %v, want context.Canceled", r.err)
	}
}

func TestServerAbandonedPublisher(t *testing.T) {
	base, res := listen(t, rtmp.Config{IdleTimeout: 200 * time.Millisecond}, nil)
	p, code, err := rtmptest.Dial(context.Background(), base+"/live/s")
	if err != nil || code != "NetStream.Publish.Start" {
		t.Fatalf("Dial: %q %v", code, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	r := await(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	if err := p.WriteTag(rtmp.FLVTag{Type: rtmp.FLVAudio, Timestamp: 1, Data: []byte{0xAF, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := readMedia(t, r.sc); err != nil {
		t.Fatal(err)
	}
	p.Abandon()
	if err := p.WriteTag(rtmp.FLVTag{Type: rtmp.FLVAudio, Timestamp: 2, Data: []byte{0xAF, 0}}); err == nil {
		t.Fatal("WriteTag after Abandon succeeded")
	}
	if _, err := readMedia(t, r.sc); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("ReadMedia from an abandoned publisher: %v, want an idle timeout", err)
	}
}
