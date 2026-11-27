package rtmp_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

type publishResult struct {
	p   *rtmp.PublishConn
	err error
}

// startPublish runs Publish in a goroutine.
func startPublish(ctx context.Context, rawURL string, cfg rtmp.Config) <-chan publishResult {
	res := make(chan publishResult, 1)
	go func() {
		p, err := rtmp.Publish(ctx, rawURL, cfg)
		res <- publishResult{p, err}
	}()
	return res
}

// awaitPublish returns Publish's result, failing after d.
func awaitPublish(t *testing.T, res <-chan publishResult, d time.Duration) publishResult {
	t.Helper()
	select {
	case r := <-res:
		if r.p != nil {
			t.Cleanup(func() { _ = r.p.Close() })
		}
		return r
	case <-time.After(d):
		t.Fatalf("Publish did not return within %v", d)
	}
	return publishResult{}
}

// publish opens rawURL with cfg, failing on error.
func publish(t *testing.T, rawURL string, cfg rtmp.Config) *rtmp.PublishConn {
	t.Helper()
	r := awaitPublish(t, startPublish(context.Background(), rawURL, cfg), 5*time.Second)
	if r.err != nil {
		t.Fatalf("Publish: %v", r.err)
	}
	return r.p
}

// eventually fails unless cond holds within d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mediaOf(tag rtmp.FLVTag) rtmp.Media {
	return rtmp.Media{Type: tag.Type, Timestamp: tag.Timestamp, Payload: tag.Data}
}

func TestPublishFlow(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	p := publish(t, srv.URL("live/s?key=1&x=2"), rtmp.Config{})
	for i, tag := range tags {
		if err := p.WriteMedia(mediaOf(tag)); err != nil {
			t.Fatalf("tag %d: WriteMedia: %v", i, err)
		}
	}
	out := rtmp.PublishOutStreams(p)
	for csid, typ := range map[uint32]uint8{4: rtmp.MsgAudio, 5: rtmp.MsgDataAMF0, 6: rtmp.MsgVideo} {
		if out[csid] != (rtmp.OutStream{Type: typ, StreamID: 1}) {
			t.Fatalf("chunk stream %d last carried %+v, want type %d on stream 1", csid, out[csid], typ)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done is open after Close")
	}
	// The server may close first, on FCUnpublish.
	if err := p.Err(); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("Err after Close: %v, want net.ErrClosed", err)
	}
	// FCUnpublish or deleteStream ends the server's stream with io.EOF, not
	// a broken connection.
	eventually(t, 2*time.Second, "the server's stream ends", func() bool { return len(srv.Ends()) == 1 })
	if end := srv.Ends()[0]; !errors.Is(end, io.EOF) {
		t.Fatalf("the server's stream ended with %v, want io.EOF", end)
	}
	got := srv.Media()
	if len(got) != len(tags) {
		t.Fatalf("server received %d messages, want %d", len(got), len(tags))
	}
	for i, tag := range tags {
		m := got[i]
		if m.Type != tag.Type || m.Timestamp != tag.Timestamp || !bytes.Equal(m.Payload, tag.Data) {
			t.Fatalf("tag %d: got type %d ts %d len %d, want type %d ts %d len %d",
				i, m.Type, m.Timestamp, len(m.Payload), tag.Type, tag.Timestamp, len(tag.Data))
		}
	}
	reqs := srv.Requests()
	want := rtmp.Request{App: "live", Stream: "s", Query: "key=1&x=2", TCURL: "rtmp://" + srv.Addr + "/live"}
	if len(reqs) != 1 || reqs[0] != want {
		t.Fatalf("requests %+v, want [%+v]", reqs, want)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := p.WriteMedia(mediaOf(tags[1])); err == nil {
		t.Fatal("WriteMedia after Close succeeded")
	}
}

// With KeepQuery the URL's query reaches the server verbatim, a mode
// parameter included: a push target's query is the target's own. Without
// it, mode is dropped as for every input URL.
func TestPublishKeepQuery(t *testing.T) {
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	for _, tc := range []struct {
		keep  bool
		query string
	}{{true, "mode=x&k=v&a=%2F"}, {false, "k=v&a=%2F"}} {
		p := publish(t, srv.URL("live/key?mode=x&k=v&a=%2F"), rtmp.Config{KeepQuery: tc.keep})
		if err := p.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		reqs := srv.Requests()
		got := reqs[len(reqs)-1]
		if got.App != "live" || got.Stream != "key" || got.Query != tc.query {
			t.Fatalf("KeepQuery %v: request %+v, want stream key and query %q", tc.keep, got, tc.query)
		}
	}
}

// publishPeer answers the client's connect, releaseStream, FCPublish and
// createStream (stream id 1) and returns the connect and publish commands;
// sawChunkSize reports a Set Chunk Size 4096 before publish.
func (p *playPeer) toPublish(t *testing.T) (connect, pub []any, sawChunkSize bool) {
	t.Helper()
	for connect == nil || pub == nil {
		m := p.next(t)
		if m.Type == rtmp.MsgSetChunkSize {
			sawChunkSize = len(m.Payload) == 4 && binary.BigEndian.Uint32(m.Payload) == 4096
			continue
		}
		if m.Type != rtmp.MsgCommandAMF0 {
			continue
		}
		v, err := rtmp.DecodeAMF0(m.Payload)
		if err != nil || len(v) < 2 {
			t.Fatalf("bad command from the client: %v %x", err, m.Payload)
		}
		switch v[0] {
		case "connect":
			connect = v
			p.write(t, 2, rtmp.Message{Type: rtmp.MsgWindowAckSize, Payload: []byte{0x00, 0x26, 0x25, 0xA0}})
			p.write(t, 2, rtmp.Message{Type: rtmp.MsgSetPeerBandwidth, Payload: []byte{0x00, 0x26, 0x25, 0xA0, 2}})
			p.command(t, 0, "_result", v[1],
				rtmp.Object{{Key: "fmsVer", Value: "FMS/3,0,1,123"}, {Key: "capabilities", Value: 31}},
				rtmp.Object{{Key: "level", Value: "status"}, {Key: "code", Value: "NetConnection.Connect.Success"}})
		case "releaseStream", "FCPublish":
			if len(v) < 4 || v[3] != "s3cretkey?t=1" {
				t.Fatalf("%v names the wrong stream", v)
			}
			p.command(t, 0, "_result", v[1], nil, rtmp.Undefined{})
		case "createStream":
			p.command(t, 0, "_result", v[1], nil, 1)
		case "publish":
			if m.StreamID != 1 {
				t.Fatalf("publish on message stream %d, want 1", m.StreamID)
			}
			pub = v
		default:
			t.Fatalf("unexpected command %v", v)
		}
	}
	return connect, pub, sawChunkSize
}

// publishing answers the exchange up to NetStream.Publish.Start and returns
// the open PublishConn.
func (p *playPeer) publishing(t *testing.T, res <-chan publishResult) *rtmp.PublishConn {
	t.Helper()
	p.toPublish(t)
	p.status(t, 1, "status", "NetStream.Publish.Start", "Start publishing")
	r := awaitPublish(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Publish: %v", r.err)
	}
	return r.p
}

// The command exchange, as a server sees it: connect announces hvc1 and
// avc1, Set Chunk Size 4096 precedes publish "live", pings are answered once
// publishing, and Close sends FCUnpublish and deleteStream.
func TestPublishExchange(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPublish(context.Background(), base+"/live/app2/s3cretkey?t=1", rtmp.Config{})
	p := awaitPeer(t, peers)
	connect, pub, sawChunkSize := p.toPublish(t)
	obj, _ := connect[2].(rtmp.Object)
	for key, want := range map[string]any{"app": "live/app2", "tcUrl": base + "/live/app2", "type": "nonprivate"} {
		if v, _ := obj.Get(key); v != want {
			t.Fatalf("connect %s = %v, want %v", key, v, want)
		}
	}
	if v, _ := obj.Get("fourCcList"); fmt.Sprint(v) != "[hvc1 avc1]" {
		t.Fatalf("connect fourCcList %v, want [hvc1 avc1]", v)
	}
	if !sawChunkSize {
		t.Fatal("no Set Chunk Size 4096 before publish")
	}
	if len(pub) != 5 || pub[3] != "s3cretkey?t=1" || pub[4] != "live" {
		t.Fatalf("publish %v, want name and \"live\"", pub)
	}
	// A notice before the status is skipped.
	p.command(t, 0, "onFCPublish", 0, nil, rtmp.Object{{Key: "code", Value: "NetStream.Publish.Start"}})
	p.status(t, 1, "status", "NetStream.Publish.Start", "Start publishing")
	r := awaitPublish(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Publish: %v", r.err)
	}

	// Pings are answered by the reader goroutine.
	p.userControl(t, 0, 6, 0, 0, 0x12, 0x34)
	for {
		m := p.next(t)
		if m.Type == rtmp.MsgUserControl && bytes.Equal(m.Payload, []byte{0, 7, 0, 0, 0x12, 0x34}) {
			break
		}
	}
	// Status notices and other commands do not end the publish.
	p.status(t, 1, "status", "NetStream.Publish.Idle", "idle")
	p.command(t, 0, "onBWDone", 0, nil)
	p.command(t, 0, "_error", 9, nil, rtmp.Object{{Key: "level", Value: "error"}})
	if err := r.p.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Timestamp: 5, Payload: []byte{0xAF, 1, 2}}); err != nil {
		t.Fatalf("WriteMedia: %v", err)
	}
	for {
		m := p.next(t)
		if m.Type == rtmp.MsgAudio {
			if m.StreamID != 1 || m.Timestamp != 5 {
				t.Fatalf("audio on stream %d at %d", m.StreamID, m.Timestamp)
			}
			break
		}
	}
	select {
	case <-r.p.Done():
		t.Fatalf("the publish ended: %v", r.p.Err())
	default:
	}

	if err := r.p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var names []string
	for m := range p.msgs {
		if m.Type != rtmp.MsgCommandAMF0 {
			continue
		}
		v, err := rtmp.DecodeAMF0(m.Payload)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, fmt.Sprint(v[0]))
		if v[0] == "FCUnpublish" && (len(v) < 4 || v[3] != "s3cretkey?t=1") {
			t.Fatalf("FCUnpublish %v", v)
		}
		if v[0] == "deleteStream" && (len(v) < 4 || v[3] != float64(1)) {
			t.Fatalf("deleteStream %v", v)
		}
	}
	if strings.Join(names, ",") != "FCUnpublish,deleteStream" {
		t.Fatalf("commands on Close %v, want FCUnpublish, deleteStream", names)
	}
}

// Close is bounded when the server stops reading: the goodbye commands give
// up after about 100 ms.
func TestPublishCloseBounded(t *testing.T) {
	cfg, accepted := stalledServer(t, rtmp.Config{})
	p := publish(t, "rtmp://example.invalid/live/s", cfg)
	<-accepted
	// A writer stalls on the full socket, holding the write lock for up to
	// the 10 s WriteTimeout.
	big := make([]byte, 4<<20)
	written := make(chan error, 1)
	go func() {
		var err error
		for err == nil {
			err = p.WriteMedia(rtmp.Media{Type: rtmp.MsgVideo, Payload: big})
		}
		written <- err
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	_ = p.Close()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Close took %v", took)
	}
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("the stalled write succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the stalled write")
	}
}

func TestPublishRejected(t *testing.T) {
	t.Run("rtmptest", func(t *testing.T) {
		srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{Reject: true})
		r := awaitPublish(t, startPublish(context.Background(), srv.URL("live/s3cretkey"), rtmp.Config{}), 5*time.Second)
		var st *rtmp.StatusError
		if !errors.As(r.err, &st) || st.Code != "NetStream.Publish.BadName" {
			t.Fatalf("Publish: %v, want NetStream.Publish.BadName", r.err)
		}
		if !strings.Contains(r.err.Error(), "NetStream.Publish.BadName") || strings.Contains(r.err.Error(), "s3cret") {
			t.Fatalf("error %q", r.err)
		}
	})
	// A server that echoes the name: the key is cut to its first 2
	// characters, with and without its query.
	t.Run("echo", func(t *testing.T) {
		base, peers := acceptPeers(t)
		res := startPublish(context.Background(), base+"/live/s3cretkey?t=1", rtmp.Config{})
		p := awaitPeer(t, peers)
		p.toPublish(t)
		p.status(t, 1, "error", "NetStream.Publish.BadName", "s3cretkey?t=1 is taken; s3cretkey busy")
		r := awaitPublish(t, res, 2*time.Second)
		var st *rtmp.StatusError
		if !errors.As(r.err, &st) || st.Code != "NetStream.Publish.BadName" {
			t.Fatalf("Publish: %v, want NetStream.Publish.BadName", r.err)
		}
		if msg := r.err.Error(); strings.Contains(msg, "s3cret") || !strings.Contains(msg, "s3xxxxx is taken; s3xxxxx busy") {
			t.Fatalf("error %q leaks or lost the key's place", msg)
		}
	})
	t.Run("_error", func(t *testing.T) {
		base, peers := acceptPeers(t)
		res := startPublish(context.Background(), base+"/live/s3cretkey?t=1", rtmp.Config{})
		p := awaitPeer(t, peers)
		_, pub, _ := p.toPublish(t)
		p.command(t, 1, "_error", pub[1], nil, rtmp.Object{
			{Key: "level", Value: "error"}, {Key: "code", Value: "NetStream.Publish.Denied"}, {Key: "description", Value: "no s3cretkey"},
		})
		r := awaitPublish(t, res, 2*time.Second)
		var st *rtmp.StatusError
		if !errors.As(r.err, &st) || st.Code != "NetStream.Publish.Denied" || strings.Contains(r.err.Error(), "s3cret") {
			t.Fatalf("Publish: %v, want NetStream.Publish.Denied, redacted", r.err)
		}
	})
	t.Run("connect", func(t *testing.T) {
		base, peers := acceptPeers(t)
		res := startPublish(context.Background(), base+"/live/s3cretkey", rtmp.Config{})
		p := awaitPeer(t, peers)
		v := p.expect(t, "connect")
		p.command(t, 0, "_error", v[1], nil, rtmp.Object{
			{Key: "level", Value: "error"}, {Key: "code", Value: "NetConnection.Connect.Rejected"}, {Key: "description", Value: "go away"},
		})
		r := awaitPublish(t, res, 2*time.Second)
		var st *rtmp.StatusError
		if !errors.As(r.err, &st) || st.Code != "NetConnection.Connect.Rejected" {
			t.Fatalf("Publish: %v, want NetConnection.Connect.Rejected", r.err)
		}
	})
	t.Run("url", func(t *testing.T) {
		for _, u := range []string{"http://h/live/s3cretkey", "rtmp://h/s3cretkey", "rtmp:///live/s3cretkey", "rtmp://h:99999/live/s3cretkey"} {
			_, err := rtmp.Publish(context.Background(), u, rtmp.Config{})
			if err == nil || strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("Publish(%q): %v", u, err)
			}
		}
	})
}

// An onStatus error after publish started ends the conn with an error
// naming its code, the key redacted.
func TestPublishStatusErrorEnds(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPublish(context.Background(), base+"/live/s3cretkey?t=1", rtmp.Config{})
	peer := awaitPeer(t, peers)
	p := peer.publishing(t, res)
	peer.status(t, 1, "error", "NetStream.Publish.Rejected", "s3cretkey was kicked")
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the publish did not end")
	}
	var st *rtmp.StatusError
	if !errors.As(p.Err(), &st) || st.Code != "NetStream.Publish.Rejected" {
		t.Fatalf("Err %v, want NetStream.Publish.Rejected", p.Err())
	}
	if msg := p.Err().Error(); strings.Contains(msg, "s3cret") || !strings.Contains(msg, "NetStream.Publish.Rejected") {
		t.Fatalf("Err %q", msg)
	}
	if err := p.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Payload: []byte{0xAF, 1}}); err == nil {
		t.Fatal("WriteMedia after the end succeeded")
	}
}

func TestPublishPeerCloses(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{CloseAfter: 5})
	p := publish(t, srv.URL("live/s"), rtmp.Config{})
	var err error
	for _, tag := range tags {
		if err = p.WriteMedia(mediaOf(tag)); err != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after the server closed")
	}
	if p.Err() == nil || errors.Is(p.Err(), net.ErrClosed) {
		t.Fatalf("Err %v, want the peer's close", p.Err())
	}
	if err := p.WriteMedia(mediaOf(tags[1])); err == nil {
		t.Fatal("WriteMedia after the peer closed succeeded")
	}
	if n := len(srv.Media()); n != 5 {
		t.Fatalf("server recorded %d messages, want 5", n)
	}
}

// stalledServer returns a Config whose Dial, asked for example.invalid:1935,
// connects to a local server with small socket buffers, which accepts the
// publish (rtmp.Accept) and then never reads again. accepted is closed once
// Accept returned.
func stalledServer(t *testing.T, cfg rtmp.Config) (rtmp.Config, <-chan struct{}) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*net.TCPConn).SetReadBuffer(4096)
		t.Cleanup(func() { _ = c.Close() })
		if _, err := rtmp.Accept(context.Background(), c, rtmp.Config{}, nil); err != nil {
			t.Errorf("Accept: %v", err)
		}
	}()
	cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" || addr != "example.invalid:1935" {
			return nil, fmt.Errorf("Dial(%q, %q)", network, addr)
		}
		var d net.Dialer
		c, err := d.DialContext(ctx, network, ln.Addr().String())
		if err != nil {
			return nil, err
		}
		_ = c.(*net.TCPConn).SetWriteBuffer(4096)
		return c, nil
	}
	return cfg, accepted
}

func TestPublishWriteDeadline(t *testing.T) {
	const timeout = 300 * time.Millisecond
	cfg, accepted := stalledServer(t, rtmp.Config{WriteTimeout: timeout})
	p := publish(t, "rtmp://example.invalid/live/s", cfg)
	<-accepted
	big := make([]byte, 1<<20)
	var err error
	var took time.Duration
	for i := 0; i < 8 && err == nil; i++ {
		start := time.Now()
		err = p.WriteMedia(rtmp.Media{Type: rtmp.MsgVideo, Payload: big})
		took = time.Since(start)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("WriteMedia to a server that stopped reading: %v, want a write timeout", err)
	}
	if took < timeout*3/4 || took > timeout+time.Second {
		t.Fatalf("WriteMedia failed after %v, want about %v", took, timeout)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after the write timeout")
	}
	if !errors.Is(p.Err(), os.ErrDeadlineExceeded) {
		t.Fatalf("Err %v, want the write timeout", p.Err())
	}
}

func TestPublishContext(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		// A server that accepts TCP and never answers.
		var lc net.ListenConfig
		ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					_, _ = io.Copy(io.Discard, c)
				}()
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		r := awaitPublish(t, startPublish(ctx, "rtmp://"+ln.Addr().String()+"/live/s3cretkey", rtmp.Config{}), 3*time.Second)
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("Publish: %v, want context.DeadlineExceeded", r.err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("Publish gave up after %v", took)
		}
		if strings.Contains(r.err.Error(), "s3cret") {
			t.Fatalf("error %q leaks the key", r.err)
		}
	})
	t.Run("only Publish", func(t *testing.T) {
		srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		r := awaitPublish(t, startPublish(ctx, srv.URL("live/s"), rtmp.Config{}), 5*time.Second)
		if r.err != nil {
			t.Fatalf("Publish: %v", r.err)
		}
		cancel()
		time.Sleep(50 * time.Millisecond)
		if err := r.p.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Payload: []byte{0xAF, 1}}); err != nil {
			t.Fatalf("WriteMedia after ctx ended: %v", err)
		}
		eventually(t, 2*time.Second, "the server receives the message", func() bool { return len(srv.Media()) == 1 })
		select {
		case <-r.p.Done():
			t.Fatalf("the publish ended with its ctx: %v", r.p.Err())
		default:
		}
	})
}

// A server that sends nothing for 3× IdleTimeout (it acks once per window,
// and a 64 kbit/s stream does not fill one) does not end a healthy publish.
func TestPublishQuietServerNotIdle(t *testing.T) {
	const idle = 200 * time.Millisecond
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{AckOnly: true})
	p := publish(t, srv.URL("live/s"), rtmp.Config{IdleTimeout: idle})
	// 64 kbit/s: 400 bytes every 50 ms.
	payload := append([]byte{0xAF, 1}, make([]byte, 398)...)
	n := 0
	for start := time.Now(); time.Since(start) < 3*idle+100*time.Millisecond; n++ {
		if err := p.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Timestamp: uint32(n * 50), Payload: payload}); err != nil {
			t.Fatalf("message %d: WriteMedia: %v", n, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-p.Done():
		t.Fatalf("a quiet server ended the publish: %v", p.Err())
	default:
	}
	if p.Err() != nil {
		t.Fatalf("Err %v", p.Err())
	}
	eventually(t, 2*time.Second, "the server receives every message", func() bool { return len(srv.Media()) == n })
}

// Data messages go out behind "@setDataFrame", once, and WriteMedia leaves
// the caller's payload alone.
func TestPublishSetDataFrame(t *testing.T) {
	base, peers := acceptPeers(t)
	res := startPublish(context.Background(), base+"/live/s3cretkey?t=1", rtmp.Config{})
	peer := awaitPeer(t, peers)
	p := peer.publishing(t, res)
	meta, err := rtmp.EncodeAMF0("onMetaData", rtmp.ECMAArray{{Key: "width", Value: 320}})
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := rtmp.EncodeAMF0("@setDataFrame")
	if err != nil {
		t.Fatal(err)
	}
	orig := bytes.Clone(meta)
	prefixed := append(bytes.Clone(prefix), meta...)
	for _, payload := range [][]byte{meta, prefixed} {
		if err := p.WriteMedia(rtmp.Media{Type: rtmp.MsgDataAMF0, Payload: payload}); err != nil {
			t.Fatalf("WriteMedia: %v", err)
		}
	}
	if !bytes.Equal(meta, orig) {
		t.Fatal("WriteMedia changed the caller's payload")
	}
	for i := 0; i < 2; {
		m := peer.next(t)
		if m.Type != rtmp.MsgDataAMF0 {
			continue
		}
		if !bytes.Equal(m.Payload, prefixed) || m.StreamID != 1 {
			t.Fatalf("data message %d on stream %d: %x, want %x", i, m.StreamID, m.Payload, prefixed)
		}
		i++
	}
	if err := p.WriteMedia(rtmp.Media{Type: rtmp.MsgCommandAMF0, Payload: meta}); err == nil {
		t.Fatal("WriteMedia of a command succeeded")
	}
	select {
	case <-p.Done():
		t.Fatalf("a bad message type ended the publish: %v", p.Err())
	default:
	}
}

// PlayWith dials through Config.Dial too.
func TestPlayUsesDial(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	srv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{})
	var calls atomic.Int32
	cfg := rtmp.Config{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls.Add(1)
		if addr != srv.Addr {
			return nil, fmt.Errorf("dial %s", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	r := awaitPlay(t, startPlay(context.Background(), srv.URL("live/s"), cfg), 5*time.Second)
	if r.err != nil {
		t.Fatalf("PlayWith: %v", r.err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Dial called %d times, want 1", calls.Load())
	}
	failing := rtmp.Config{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no route")
	}}
	if _, err := rtmp.PlayWith(context.Background(), srv.URL("live/s"), failing); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("PlayWith with a failing Dial: %v", err)
	}
	if _, err := rtmp.Publish(context.Background(), srv.URL("live/s"), failing); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("Publish with a failing Dial: %v", err)
	}
}

// requireFFprobe returns the ffprobe path, skipping the test when it is
// missing, or failing it when OS_REQUIRE_PEERS=1. It mirrors
// testpeer.Require, which pkg/ cannot import.
func requireFFprobe(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("OS_REQUIRE_PEERS") == "1" {
			t.Fatalf("peer ffprobe not found: %v", err)
		}
		t.Skipf("peer ffprobe not found: %v", err)
	}
	return path
}

// requireMediaMTX returns the host:port of the MediaMTX RTMP listener named by
// OS_MEDIAMTX_RTMP, skipping the test when it is unset, or failing it when
// OS_REQUIRE_MEDIAMTX=1. It mirrors testpeer.MediaMTX.
func requireMediaMTX(t *testing.T) string {
	t.Helper()
	if addr := os.Getenv("OS_MEDIAMTX_RTMP"); addr != "" {
		return addr
	}
	if os.Getenv("OS_REQUIRE_MEDIAMTX") == "1" {
		t.Fatal("peer MediaMTX: OS_MEDIAMTX_RTMP is not set")
	}
	t.Skip("peer MediaMTX: OS_MEDIAMTX_RTMP is not set")
	return ""
}

func TestPublishFFmpegListener(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ffprobe := requireFFprobe(t)
	var tags []rtmp.FLVTag
	for _, tag := range loadFLV(t, "h264_aac.flv") {
		if tag.Timestamp <= 3000 {
			tags = append(tags, tag)
		}
	}
	url := fmt.Sprintf("rtmp://127.0.0.1:%d/live/x", freePort(t))
	out := filepath.Join(t.TempDir(), "out.flv")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-listen", "1", "-i", url, "-c", "copy", "-f", "flv", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// ffmpeg needs a moment to listen.
	var p *rtmp.PublishConn
	deadline := time.Now().Add(10 * time.Second)
	for {
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		var err error
		p, err = rtmp.Publish(pctx, url, rtmp.Config{})
		pcancel()
		if err == nil {
			break
		}
		select {
		case werr := <-exited:
			t.Fatalf("ffmpeg exited (%v) before Publish succeeded: %v (%s)", werr, err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Publish to ffmpeg: %v (ffmpeg: %s)", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i, tag := range tags {
		if err := p.WriteMedia(mediaOf(tag)); err != nil {
			t.Fatalf("tag %d: WriteMedia: %v (ffmpeg: %s)", i, err, stderr.String())
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("ffmpeg: %v (%s)", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("ffmpeg did not exit (%s)", stderr.String())
	}
	probe, err := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-show_entries",
		"stream=codec_type,codec_name:format=duration", "-of", "default=nw=1", out).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v (%s)", err, probe)
	}
	for _, s := range []string{"codec_name=h264", "codec_type=video", "codec_name=aac", "codec_type=audio"} {
		if !bytes.Contains(probe, []byte(s)) {
			t.Fatalf("ffprobe of the received file lacks %q:\n%s", s, probe)
		}
	}
}

// frame is a coded video frame or a raw AAC frame, for comparing what went
// into MediaMTX with what comes out.
type frame struct {
	dts, pts uint32 // ms; equal for audio
	key      bool   // video: holds an IDR (H.264) or IRAP (H.265) NAL unit
	units    [][]byte
}

// classifyNAL reports whether a NAL unit is a parameter set or an access
// unit delimiter (which a server may add, drop or move), and whether it is
// a random access point.
func classifyNAL(codec rtmp.VideoCodec, nal []byte) (skip, key bool) {
	if len(nal) == 0 {
		return true, false
	}
	switch codec {
	case rtmp.VideoHEVC:
		typ := (nal[0] >> 1) & 0x3F
		return typ >= 32 && typ <= 35, typ >= 16 && typ <= 21
	case rtmp.VideoAVC:
		typ := nal[0] & 0x1F
		return typ >= 7 && typ <= 9, typ == 5
	}
	return false, false
}

// frameOf parses a message into a frame; ok is false for headers, metadata
// and audio this package does not parse. Video keeps its NAL units without
// parameter sets and AUDs. The key flag comes from the NAL units, since
// MediaMTX leaves the frame type of enhanced (H.265) tags at 0.
func frameOf(t *testing.T, m rtmp.Media) (f frame, video, ok bool) {
	t.Helper()
	switch m.Type {
	case rtmp.MsgVideo:
		v, err := rtmp.ParseVideoTag(m.Payload)
		if err != nil {
			t.Fatalf("video tag: %v", err)
		}
		if v.Kind != rtmp.VideoCodedFrames {
			return frame{}, true, false
		}
		f = frame{dts: m.Timestamp, pts: m.Timestamp + uint32(v.CompTime)}
		for b := v.Data; len(b) >= 4; {
			n := int(binary.BigEndian.Uint32(b))
			if n > len(b)-4 {
				t.Fatalf("NAL unit length %d past the tag", n)
			}
			nal := b[4 : 4+n]
			skip, key := classifyNAL(v.Codec, nal)
			if !skip {
				f.units = append(f.units, nal)
			}
			f.key = f.key || key
			b = b[4+n:]
		}
		return f, true, true
	case rtmp.MsgAudio:
		a, err := rtmp.ParseAudioTag(m.Payload)
		if err != nil || a.SequenceHeader {
			return frame{}, false, false
		}
		return frame{dts: m.Timestamp, pts: m.Timestamp, units: [][]byte{a.Data}}, false, true
	}
	return frame{}, false, false
}

// within1 reports whether two millisecond timestamps differ by at most 1 ms
// (mod 2^32). MediaMTX re-times through its own clock rates (90 kHz video,
// the sample rate for audio) and truncates on the way back to milliseconds,
// rounding the re-derived video DTS and the composition time apart, so a
// timestamp read back may be 1 ms short.
func within1(a, b uint32) bool {
	d := int32(a - b)
	return d >= -1 && d <= 1
}

func sameUnits(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Our publish into MediaMTX, read back with our Play, which joins once the
// publish runs. From the first keyframe read, the video NAL units and AAC
// frames equal the published ones in order, and their presentation
// timestamps keep the published deltas (video against the keyframe, audio
// against the keyframe too, so A/V alignment holds). MediaMTX re-times,
// re-derives the DTS of video (its own DTS extractor) and re-packs, so the
// tag bytes and the video DTS may differ, and a timestamp may be 1 ms short
// from rounding (within1); the DTS read back must still never decrease nor
// pass the PTS.
func TestPublishMediaMTX(t *testing.T) {
	addr := requireMediaMTX(t)
	for _, tc := range []struct{ name, fixture string }{
		{"h264", "h264_aac.flv"},
		{"h265", "h265_aac.flv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags := loadFLV(t, tc.fixture)
			url := fmt.Sprintf("rtmp://%s/m3b1t4/%s-%d", addr, tc.name, time.Now().UnixNano())
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			p, err := rtmp.Publish(ctx, url, rtmp.Config{})
			if err != nil {
				t.Fatalf("Publish: %v", err)
			}
			t.Cleanup(func() { _ = p.Close() })

			// Send the tags in real time; the player joins meanwhile.
			joined := make(chan struct{})
			sent := make(chan error, 1)
			go func() {
				start := time.Now()
				for _, tag := range tags {
					if wait := time.Until(start.Add(time.Duration(tag.Timestamp) * time.Millisecond)); wait > 0 {
						select {
						case <-time.After(wait):
						case <-ctx.Done():
							sent <- ctx.Err()
							return
						}
					}
					if err := p.WriteMedia(mediaOf(tag)); err != nil {
						sent <- err
						return
					}
				}
				// Let the player catch up before the stream ends.
				select {
				case <-joined:
				case <-ctx.Done():
				}
				time.Sleep(time.Second)
				sent <- p.Close()
			}()
			// MediaMTX makes the path available once it has seen the
			// tracks, about 2 s in; the player retries until then.
			time.Sleep(time.Second)
			var c *rtmp.ClientConn
			for deadline := time.Now().Add(5 * time.Second); ; {
				c, err = rtmp.PlayWith(ctx, url, rtmp.Config{})
				if err == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			close(joined)
			if err != nil {
				t.Fatalf("Play: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			var got []rtmp.Media
			for {
				m, err := c.ReadMedia()
				if err != nil {
					break
				}
				got = append(got, m)
			}
			if err := <-sent; err != nil {
				t.Fatalf("publishing: %v", err)
			}
			compareReadBack(t, tags, got)
		})
	}
}

// compareReadBack checks got, read back from a server, against the
// published tags, from the first keyframe read.
func compareReadBack(t *testing.T, tags []rtmp.FLVTag, got []rtmp.Media) {
	t.Helper()
	var pubV, pubA, gotV, gotA []frame
	for _, tag := range tags {
		if f, video, ok := frameOf(t, mediaOf(tag)); ok {
			if video {
				pubV = append(pubV, f)
			} else {
				pubA = append(pubA, f)
			}
		}
	}
	for _, m := range got {
		f, video, ok := frameOf(t, m)
		switch {
		case !ok:
		case video:
			if len(gotV) > 0 || f.key {
				gotV = append(gotV, f)
			}
		case len(gotV) > 0:
			gotA = append(gotA, f)
		}
	}
	if len(gotV) < 25 {
		t.Fatalf("read %d video frames from the first keyframe, want at least 25 (%d messages)", len(gotV), len(got))
	}
	j := -1
	for i, f := range pubV {
		if f.key && sameUnits(f.units, gotV[0].units) {
			j = i
			break
		}
	}
	if j < 0 {
		t.Fatal("the first keyframe read is not a published one")
	}
	if n := len(pubV) - j; len(gotV) != n {
		t.Fatalf("read %d video frames from the join, want the %d published from there", len(gotV), n)
	}
	for i, g := range gotV {
		w := pubV[j+i]
		if !sameUnits(g.units, w.units) || g.key != w.key {
			t.Fatalf("video frame %d after the join differs", i)
		}
		if !within1(g.pts-gotV[0].pts, w.pts-pubV[j].pts) {
			t.Fatalf("video frame %d: PTS delta %d, want %d", i, g.pts-gotV[0].pts, w.pts-pubV[j].pts)
		}
		if int32(g.pts-g.dts) < 0 || (i > 0 && int32(g.dts-gotV[i-1].dts) < 0) {
			t.Fatalf("video frame %d: DTS %d (previous %d) with PTS %d", i, g.dts, gotV[i-1].dts, g.pts)
		}
	}
	if len(gotA) < 25 {
		t.Fatalf("read %d audio frames after the keyframe, want at least 25", len(gotA))
	}
	// The first audio frame read after the keyframe lines up with the
	// published frame at the same offset from the keyframe's PTS.
	want := pubV[j].pts + (gotA[0].pts - gotV[0].pts)
	k := -1
	for i, f := range pubA {
		if within1(f.pts, want) && sameUnits(f.units, gotA[0].units) {
			k = i
			break
		}
	}
	if k < 0 {
		t.Fatalf("no published audio frame at %d ms", want)
	}
	if n := len(pubA) - k; len(gotA) != n {
		t.Fatalf("read %d audio frames, want the %d published from there", len(gotA), n)
	}
	for i, g := range gotA {
		w := pubA[k+i]
		if !sameUnits(g.units, w.units) || !within1(g.pts-gotA[0].pts, w.pts-pubA[k].pts) {
			t.Fatalf("audio frame %d: delta %d, want delta %d, or the frames differ",
				i, g.pts-gotA[0].pts, w.pts-pubA[k].pts)
		}
	}
}

func TestPublishTLS(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	// received publishes the tags to srv and returns what it got.
	received := func(t *testing.T, srv *rtmptest.PublishServer, cfg rtmp.Config) []rtmp.Media {
		t.Helper()
		p := publish(t, srv.URL("live/s?key=1"), cfg)
		for i, tag := range tags {
			if err := p.WriteMedia(mediaOf(tag)); err != nil {
				t.Fatalf("tag %d: WriteMedia: %v", i, err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		eventually(t, 5*time.Second, "the server's stream ends", func() bool { return len(srv.Ends()) == 1 })
		if end := srv.Ends()[0]; !errors.Is(end, io.EOF) {
			t.Fatalf("the server's stream ended with %v, want io.EOF", end)
		}
		return srv.Media()
	}
	plain := received(t, rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{}), rtmp.Config{})
	if len(plain) != len(tags) {
		t.Fatalf("the plain run delivered %d messages of %d tags", len(plain), len(tags))
	}
	ca := tlstest.NewCA(t)
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{TLS: ca.Server(t)})
	if !strings.HasPrefix(srv.URL("live/s"), "rtmps://") {
		t.Fatalf("URL %q, want rtmps://", srv.URL("live/s"))
	}

	// Untrusted first, so the server's record holds only the trusted run.
	r := awaitPublish(t, startPublish(context.Background(), srv.URL("live/s3cretkey"), rtmp.Config{}), 5*time.Second)
	var e x509.UnknownAuthorityError
	if !errors.As(r.err, &e) || !strings.HasPrefix(r.err.Error(), "rtmp: client: tls: ") {
		t.Fatalf("Publish with the system roots: %v, want an x509.UnknownAuthorityError", r.err)
	}
	if strings.Contains(r.err.Error(), "s3cretkey") {
		t.Fatalf("Publish: %q holds the stream name", r.err)
	}

	got := received(t, srv, rtmp.Config{TLS: ca.Client()})
	if len(got) != len(plain) {
		t.Fatalf("the server got %d messages over TLS, the plain run %d", len(got), len(plain))
	}
	for i := range got {
		if got[i].Type != plain[i].Type || got[i].Timestamp != plain[i].Timestamp || !bytes.Equal(got[i].Payload, plain[i].Payload) {
			t.Fatalf("message %d over TLS differs from the plain run's", i)
		}
	}
	reqs := srv.Requests()
	want := rtmp.Request{App: "live", Stream: "s", Query: "key=1", TCURL: "rtmps://" + srv.Addr + "/live"}
	if len(reqs) != 1 || reqs[0] != want {
		t.Fatalf("requests %+v, want [%+v]", reqs, want)
	}
}
