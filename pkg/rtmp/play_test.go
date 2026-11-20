package rtmp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// sampleAccess is the AMF0 name of the |RtmpSampleAccess data message.
var sampleAccess = []byte{0x02, 0x00, 0x11, '|', 'R', 't', 'm', 'p', 'S', 'a', 'm', 'p', 'l', 'e', 'A', 'c', 'c', 'e', 's', 's'}

// readPlayed reads the next media message a ClientConn plays, skipping the
// |RtmpSampleAccess notice the server sends after NetStream.Play.Start.
func readPlayed(t *testing.T, c *rtmp.ClientConn) (rtmp.Media, error) {
	t.Helper()
	for {
		m, err := readMedia(t, c)
		if err != nil || m.Type != rtmp.MsgDataAMF0 || !bytes.HasPrefix(m.Payload, sampleAccess) {
			return m, err
		}
	}
}

// playTo runs connect, createStream and play for live/s with play
// transaction 3, and returns the accepted ServerConn once the client has read
// the play replies, up to |RtmpSampleAccess.
func (c *rawClient) playTo(t *testing.T) *rtmp.ServerConn {
	t.Helper()
	c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "live"}, {Key: "tcUrl", Value: "rtmp://h/live"}})
	c.command(t, 0, "createStream", 2, nil)
	c.command(t, 1, "play", 3, nil, "s", -2)
	r := await(t, c.res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	for {
		m := c.next(t)
		if m.Type == rtmp.MsgDataAMF0 && bytes.HasPrefix(m.Payload, sampleAccess) {
			return r.sc
		}
	}
}

// awaitEnd waits for a play conn to end.
func awaitEnd(t *testing.T, sc *rtmp.ServerConn) {
	t.Helper()
	select {
	case <-sc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the play conn did not end")
	}
}

func TestServerPlayFlow(t *testing.T) {
	tags := loadFLV(t, "h264_aac.flv")
	reqs := make(chan rtmp.Request, 1)
	base, res := listen(t, rtmp.Config{}, func(r rtmp.Request) error {
		reqs <- r
		return nil
	})
	c := play(t, base+"/live/s?k=v")
	t.Cleanup(func() { _ = c.Close() })
	r := await(t, res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	sc := r.sc
	want := rtmp.Request{Play: true, App: "live", Stream: "s", Query: "k=v", TCURL: base + "/live"}
	if got := <-reqs; got != want {
		t.Fatalf("decide saw %+v, want %+v", got, want)
	}
	if !sc.IsPlay() || sc.Request() != want || sc.Request().Path() != "live/s" {
		t.Fatalf("IsPlay %v, Request %+v", sc.IsPlay(), sc.Request())
	}
	if _, err := sc.ReadMedia(); err == nil {
		t.Fatal("ReadMedia on a play conn succeeded")
	}

	sent := make(chan error, 1)
	go func() {
		for _, tag := range tags {
			if err := sc.WriteMedia(rtmp.Media{Type: tag.Type, Timestamp: tag.Timestamp, Payload: tag.Data}); err != nil {
				sent <- err
				return
			}
		}
		sent <- sc.EndOfStream()
	}()
	for i, tag := range tags {
		m, err := readPlayed(t, c)
		if err != nil {
			t.Fatalf("tag %d: ReadMedia: %v", i, err)
		}
		if m.Type != tag.Type || m.Timestamp != tag.Timestamp || !bytes.Equal(m.Payload, tag.Data) {
			t.Fatalf("tag %d: got type %d ts %d len %d, want type %d ts %d len %d",
				i, m.Type, m.Timestamp, len(m.Payload), tag.Type, tag.Timestamp, len(tag.Data))
		}
	}
	if _, err := readPlayed(t, c); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadMedia after EndOfStream: %v, want io.EOF", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("WriteMedia or EndOfStream: %v", err)
	}
	select {
	case <-sc.Done():
		t.Fatalf("the play conn ended (%v) while the player is connected", sc.Err())
	default:
	}
	_ = c.Close()
	awaitEnd(t, sc)
	if sc.Err() == nil {
		t.Fatal("Err is nil after the player left")
	}
}

func TestServerPlayWriteMediaTypes(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	sc := c.playTo(t)
	if err := sc.WriteMedia(rtmp.Media{Type: rtmp.MsgCommandAMF0, Payload: []byte{5}}); err == nil {
		t.Fatal("WriteMedia of a command succeeded")
	}
	// Each kind goes on its own chunk stream: the first message of a chunk
	// stream has a type 0 header naming its csid.
	for _, m := range []rtmp.Media{
		{Type: rtmp.MsgDataAMF0, Timestamp: 0, Payload: []byte{2, 0, 1, 'x'}},
		{Type: rtmp.MsgAudio, Timestamp: 1, Payload: []byte{0xAF, 1}},
		{Type: rtmp.MsgVideo, Timestamp: 2, Payload: []byte{0x17, 1}},
	} {
		if err := sc.WriteMedia(m); err != nil {
			t.Fatalf("WriteMedia type %d: %v", m.Type, err)
		}
		got := c.next(t)
		if got.Type != m.Type || got.StreamID != 1 || got.Timestamp != m.Timestamp || !bytes.Equal(got.Payload, m.Payload) {
			t.Fatalf("got type %d stream %d ts %d %x, want type %d stream 1 ts %d %x",
				got.Type, got.StreamID, got.Timestamp, got.Payload, m.Type, m.Timestamp, m.Payload)
		}
	}
	// The session is still up: a command type is refused without ending it.
	select {
	case <-sc.Done():
		t.Fatalf("the conn ended: %v", sc.Err())
	default:
	}
}

func TestServerPlayChunkStreams(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	sc := c.playTo(t)
	for _, m := range []rtmp.Media{
		{Type: rtmp.MsgDataAMF0, Payload: []byte{2, 0, 1, 'x'}},
		{Type: rtmp.MsgAudio, Payload: []byte{0xAF, 1}},
		{Type: rtmp.MsgVideo, Payload: []byte{0x17, 1}},
	} {
		if err := sc.WriteMedia(m); err != nil {
			t.Fatal(err)
		}
		c.next(t)
	}
	got := rtmp.OutStreams(sc)
	for csid, want := range map[uint32]rtmp.OutStream{
		4: {Type: rtmp.MsgAudio, StreamID: 1},
		5: {Type: rtmp.MsgDataAMF0, StreamID: 1},
		6: {Type: rtmp.MsgVideo, StreamID: 1},
	} {
		if got[csid] != want {
			t.Fatalf("csid %d last carried %+v, want %+v (all: %+v)", csid, got[csid], want, got)
		}
	}
}

func TestServerPlayRejected(t *testing.T) {
	reasons := []error{
		rtmp.ErrStreamNotAvailable,
		fmt.Errorf("player cap reached: %w", rtmp.ErrStreamNotAvailable),
		errors.New("busy"),
	}
	t.Run("client", func(t *testing.T) {
		texts := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			base, res := listen(t, rtmp.Config{}, func(rtmp.Request) error { return reason })
			r := awaitPlay(t, startPlay(context.Background(), base+"/live/secretname", rtmp.Config{}), 5*time.Second)
			var st *rtmp.StatusError
			if !errors.As(r.err, &st) {
				t.Fatalf("%v: Play: %v, want a StatusError", reason, r.err)
			}
			if st.Code != "NetStream.Play.StreamNotFound" || st.Description != "stream not available" {
				t.Fatalf("%v: rejection %q %q", reason, st.Code, st.Description)
			}
			texts = append(texts, r.err.Error())
			a := await(t, res, 2*time.Second)
			if a.sc != nil || !errors.Is(a.err, reason) {
				t.Fatalf("Accept: %v, want the decide error %v", a.err, reason)
			}
		}
		for _, s := range texts[1:] {
			if s != texts[0] {
				t.Fatalf("rejections differ: %q and %q", texts[0], s)
			}
		}
	})
	t.Run("raw", func(t *testing.T) {
		replies := make([][]any, 0, len(reasons))
		for _, reason := range reasons {
			c := startRaw(t, context.Background(), rtmp.Config{}, func(rtmp.Request) error { return reason }, true)
			c.command(t, 0, "connect", 1, rtmp.Object{{Key: "app", Value: "live"}})
			c.command(t, 0, "createStream", 2, nil)
			c.command(t, 1, "play", 7, nil, "secretname", -2)
			cmds := c.closed(t)
			replies = append(replies, cmds[len(cmds)-1])
		}
		want := []any{"onStatus", 7.0, rtmp.Null{}, rtmp.Object{
			{Key: "level", Value: "error"},
			{Key: "code", Value: "NetStream.Play.StreamNotFound"},
			{Key: "description", Value: "stream not available"},
		}}
		for i, got := range replies {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%v: last command %#v, want %#v", reasons[i], got, want)
			}
		}
	})
}

// TestServerPlayFFmpegSequence drives the play exchange as ffmpeg does and
// checks every reply exactly.
func TestServerPlayFFmpegSequence(t *testing.T) {
	reqs := make(chan rtmp.Request, 1)
	c := startRaw(t, context.Background(), rtmp.Config{}, func(r rtmp.Request) error {
		reqs <- r
		return nil
	}, true)
	c.command(t, 0, "connect", 1, rtmp.Object{
		{Key: "app", Value: "live"},
		{Key: "flashVer", Value: "LNX 9,0,124,2"},
		{Key: "tcUrl", Value: "rtmp://127.0.0.1:1935/live"},
		{Key: "fpad", Value: false},
		{Key: "capabilities", Value: 15},
		{Key: "audioCodecs", Value: 4071},
		{Key: "videoCodecs", Value: 252},
		{Key: "videoFunction", Value: 1},
	})
	for _, typ := range []uint8{rtmp.MsgWindowAckSize, rtmp.MsgSetPeerBandwidth, rtmp.MsgSetChunkSize} {
		if m := c.next(t); m.Type != typ {
			t.Fatalf("got type %d, want %d", m.Type, typ)
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
	c.write(t, 2, rtmp.Message{Type: rtmp.MsgWindowAckSize, Payload: []byte{0x00, 0x26, 0x25, 0xA0}})
	c.command(t, 0, "createStream", 2, nil)
	wantCmd("_result", 2.0, rtmp.Null{}, 1.0)
	// ffmpeg sends getStreamLength and play on chunk stream 8.
	invoke := func(vals ...any) {
		t.Helper()
		p, err := rtmp.EncodeAMF0(vals...)
		if err != nil {
			t.Fatal(err)
		}
		c.write(t, 8, rtmp.Message{Type: rtmp.MsgCommandAMF0, StreamID: 1, Payload: p})
	}
	invoke("getStreamLength", 3, nil, "x")
	wantCmd("_result", 3.0, rtmp.Null{}, 0.0)
	invoke("play", 4, nil, "x", -2000)
	c.write(t, 2, rtmp.Message{Type: rtmp.MsgUserControl, Payload: []byte{0, 3, 0, 0, 0, 1, 0, 0, 0x0B, 0xB8}})

	r := await(t, c.res, 2*time.Second)
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	if got, want := <-reqs, (rtmp.Request{Play: true, App: "live", Stream: "x", TCURL: "rtmp://127.0.0.1:1935/live"}); got != want {
		t.Fatalf("decide saw %+v, want %+v", got, want)
	}
	status := func(code, desc string) []any {
		return []any{"onStatus", 4.0, rtmp.Null{}, rtmp.Object{
			{Key: "level", Value: "status"},
			{Key: "code", Value: code},
			{Key: "description", Value: desc},
		}}
	}
	type reply struct {
		typ      uint8
		streamID uint32
		payload  []byte
		cmd      []any
	}
	want := []reply{
		{typ: rtmp.MsgSetChunkSize, payload: []byte{0, 0, 0x10, 0}},
		{typ: rtmp.MsgUserControl, payload: []byte{0, 0, 0, 0, 0, 1}},
		{typ: rtmp.MsgCommandAMF0, streamID: 1, cmd: status("NetStream.Play.Reset", "Playing and resetting")},
		{typ: rtmp.MsgCommandAMF0, streamID: 1, cmd: status("NetStream.Play.Start", "Started playing")},
		{typ: rtmp.MsgDataAMF0, streamID: 1, cmd: []any{"|RtmpSampleAccess", true, true}},
	}
	for i, w := range want {
		m := c.next(t)
		if m.Type != w.typ || m.StreamID != w.streamID {
			t.Fatalf("reply %d: type %d stream %d %x, want type %d stream %d", i, m.Type, m.StreamID, m.Payload, w.typ, w.streamID)
		}
		if w.cmd == nil {
			if !bytes.Equal(m.Payload, w.payload) {
				t.Fatalf("reply %d: %x, want %x", i, m.Payload, w.payload)
			}
			continue
		}
		got, err := rtmp.DecodeAMF0(m.Payload)
		if err != nil || !reflect.DeepEqual(got, w.cmd) {
			t.Fatalf("reply %d: %#v (%v), want %#v", i, got, err, w.cmd)
		}
	}

	// receiveVideo and receiveAudio are ignored; media keeps flowing.
	invoke("receiveVideo", 0, nil, true)
	invoke("receiveAudio", 0, nil, true)
	if err := r.sc.WriteMedia(rtmp.Media{Type: rtmp.MsgVideo, Timestamp: 40, Payload: []byte{0x17, 1, 0, 0, 0}}); err != nil {
		t.Fatalf("WriteMedia: %v", err)
	}
	if m := c.next(t); m.Type != rtmp.MsgVideo || m.StreamID != 1 || m.Timestamp != 40 {
		t.Fatalf("got type %d stream %d ts %d, want video on stream 1 at 40", m.Type, m.StreamID, m.Timestamp)
	}
	select {
	case <-r.sc.Done():
		t.Fatalf("the play conn ended: %v", r.sc.Err())
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServerPlayCloseStream(t *testing.T) {
	for _, name := range []string{"closeStream", "deleteStream"} {
		t.Run(name, func(t *testing.T) {
			c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
			sc := c.playTo(t)
			// Commands a player may send that do not end it.
			c.command(t, 1, "receiveAudio", 0, nil, false)
			c.command(t, 1, "receiveVideo", 0, nil, false)
			c.command(t, 1, "pause", 0, nil, true, 0)
			c.command(t, 1, "seek", 0, nil, 0)
			c.command(t, 1, "FCSubscribe", 0, nil, "s")
			c.write(t, 2, rtmp.Message{Type: rtmp.MsgAck, Payload: []byte{0, 0, 0x10, 0}})
			select {
			case <-sc.Done():
				t.Fatalf("the play conn ended: %v", sc.Err())
			case <-time.After(50 * time.Millisecond):
			}
			if sc.Err() != nil {
				t.Fatalf("Err %v while playing", sc.Err())
			}
			c.command(t, 1, name, 0, nil, 1)
			awaitEnd(t, sc)
			if !errors.Is(sc.Err(), io.EOF) {
				t.Fatalf("Err %v, want io.EOF", sc.Err())
			}
			c.closed(t)
			if err := sc.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Payload: []byte{0xAF, 1}}); err == nil {
				t.Fatal("WriteMedia after the player closed the stream succeeded")
			}
		})
	}
	t.Run("EOF", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
		sc := c.playTo(t)
		_ = c.conn.Close()
		awaitEnd(t, sc)
		if sc.Err() == nil {
			t.Fatal("Err is nil after the player closed the connection")
		}
	})
	t.Run("Close", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
		sc := c.playTo(t)
		_ = sc.Close()
		awaitEnd(t, sc)
		c.closed(t)
	})
}

func TestServerPlayWriteDeadline(t *testing.T) {
	big := rtmp.Media{Type: rtmp.MsgVideo, Payload: make([]byte, 1<<20)}
	check := func(t *testing.T, sc *rtmp.ServerConn, c *rawClient, want time.Duration) {
		t.Helper()
		c.stopReading()
		var err error
		var took time.Duration
		for i := 0; i < 4 && err == nil; i++ {
			start := time.Now()
			err = sc.WriteMedia(big)
			took = time.Since(start)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("WriteMedia to a stalled player: %v, want a write timeout", err)
		}
		if took < want*3/4 || took > want+time.Second {
			t.Fatalf("WriteMedia failed after %v, want about %v", took, want)
		}
		awaitEnd(t, sc)
		if !errors.Is(sc.Err(), os.ErrDeadlineExceeded) {
			t.Fatalf("Err %v, want the write timeout", sc.Err())
		}
		start := time.Now()
		if err := sc.WriteMedia(big); err == nil || time.Since(start) > time.Second {
			t.Fatalf("WriteMedia after the timeout: %v after %v", err, time.Since(start))
		}
	}
	t.Run("config", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{WriteTimeout: 300 * time.Millisecond}, nil, true)
		check(t, c.playTo(t), c, 300*time.Millisecond)
	})
	t.Run("SetWriteTimeout", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
		sc := c.playTo(t)
		sc.SetWriteTimeout(250 * time.Millisecond)
		check(t, sc, c, 250*time.Millisecond)
	})
	t.Run("restore", func(t *testing.T) {
		c := startRaw(t, context.Background(), rtmp.Config{WriteTimeout: 300 * time.Millisecond}, nil, true)
		sc := c.playTo(t)
		sc.SetWriteTimeout(time.Minute)
		sc.SetWriteTimeout(0) // back to Config.WriteTimeout
		check(t, sc, c, 300*time.Millisecond)
	})
}

func TestServerPublishConnIsNotPlay(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	sc := c.publishTo(t, "live", "s")
	if sc.IsPlay() || sc.Request().Play {
		t.Fatal("a publish conn reports play")
	}
	if err := sc.WriteMedia(rtmp.Media{Type: rtmp.MsgAudio, Payload: []byte{0xAF, 1}}); err == nil {
		t.Fatal("WriteMedia on a publish conn succeeded")
	}
	if err := sc.EndOfStream(); err == nil {
		t.Fatal("EndOfStream on a publish conn succeeded")
	}
}

func TestServerPlayEndOfStream(t *testing.T) {
	c := startRaw(t, context.Background(), rtmp.Config{}, nil, true)
	sc := c.playTo(t)
	if err := sc.EndOfStream(); err != nil {
		t.Fatalf("EndOfStream: %v", err)
	}
	if m := c.next(t); m.Type != rtmp.MsgUserControl || !bytes.Equal(m.Payload, []byte{0, 1, 0, 0, 0, 1}) {
		t.Fatalf("got type %d %x, want StreamEOF(1)", m.Type, m.Payload)
	}
	got := c.nextCommand(t)
	want := []any{"onStatus", 0.0, rtmp.Null{}, rtmp.Object{
		{Key: "level", Value: "status"},
		{Key: "code", Value: "NetStream.Play.UnpublishNotify"},
		{Key: "description", Value: "stream ended"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestServerPlayFFmpeg(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	tags := loadFLV(t, "h264_aac.flv")
	base, res := listen(t, rtmp.Config{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "info",
		"-i", base+"/live/x", "-t", "3", "-c", "copy", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var r acceptResult
	select {
	case r = <-res:
	case err := <-exited:
		t.Fatalf("ffmpeg exited before Accept: %v (%s)", err, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("Accept did not return (ffmpeg: %s)", stderr.String())
	}
	if r.err != nil {
		t.Fatalf("Accept: %v (ffmpeg: %s)", r.err, stderr.String())
	}
	defer r.sc.Close()
	// ffmpeg stops reading after 3 s of media, so later writes may fail.
	for _, tag := range tags {
		if err := r.sc.WriteMedia(rtmp.Media{Type: tag.Type, Timestamp: tag.Timestamp, Payload: tag.Data}); err != nil {
			break
		}
	}
	_ = r.sc.EndOfStream()

	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("ffmpeg: %v (%s)", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("ffmpeg did not exit (%s)", stderr.String())
	}
	out := stderr.String()
	for _, s := range []string{"Video: h264", "Audio: aac"} {
		if !strings.Contains(out, s) {
			t.Fatalf("ffmpeg did not see %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "time=00:00:03") {
		t.Fatalf("ffmpeg did not read 3 s of media:\n%s", out)
	}
}
