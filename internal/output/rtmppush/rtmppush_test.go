package rtmppush

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

const waitFor = 10 * time.Second

// Secrets of the redaction tests, built so the source holds none of them
// whole.
var (
	vKey   = "sekrit" + "key123"
	vPass  = "pw-" + "hunter2"
	vToken = "tok-" + "sesame"
)

// source returns a fixture's demuxed frames, in source time, and its track
// kinds.
func source(t *testing.T, name string) ([]media.Frame, []media.Kind) {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/fixtures/" + name)
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out, d.TrackKinds()
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

// looped returns n back-to-back copies of a fixture in stream time, as one
// epoch.
func looped(t *testing.T, name string, n int) []media.Frame {
	t.Helper()
	src, kinds := source(t, name)
	lo, hi := src[0].DTS, src[0].DTS
	for _, f := range src {
		lo, hi = min(lo, f.DTS), max(hi, f.DTS)
	}
	span := hi - lo + 3600
	c := clock.New(clock.Options{Kinds: kinds})
	var out []media.Frame
	for i := range n {
		for _, f := range src {
			f.PTS += int64(i) * span
			f.DTS += int64(i) * span
			out = append(out, c.Push(f)...)
		}
	}
	return append(out, c.Flush()...)
}

// shift moves frames by d ticks and sets their epoch.
func shift(frames []media.Frame, d int64, epoch uint32) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		out[i].PTS += d
		out[i].DTS += d
		out[i].Epoch = epoch
	}
	return out
}

func lastDTS(frames []media.Frame) int64 {
	m := frames[0].DTS
	for _, f := range frames {
		m = max(m, f.DTS)
	}
	return m
}

// keyIndexes returns the indexes of the video keyframes.
func keyIndexes(frames []media.Frame) []int {
	var out []int
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			out = append(out, i)
		}
	}
	return out
}

// liveHub returns a hub holding one video keyframe: it is live, so a Pusher
// publishes at once.
func liveHub(t *testing.T) *hub.Hub {
	t.Helper()
	h := hub.New(hub.Options{})
	frames := looped(t, "h264_aac.ts", 1)
	h.Write(frames[keyIndexes(frames)[0]])
	require.True(t, h.Live())
	return h
}

// firstGOP returns the frames before the second video keyframe.
func firstGOP(frames []media.Frame) (gop, rest []media.Frame) {
	if k := keyIndexes(frames); len(k) >= 2 {
		return frames[:k[1]], frames[k[1]:]
	}
	return frames, nil
}

// feed writes frames to h in a goroutine, paced at speed times real time
// by DTS, until they run out or ctx ends. The channel yields the longest
// Write, then closes.
func feed(ctx context.Context, h *hub.Hub, frames []media.Frame, speed float64) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	go func() {
		defer close(out)
		var longest time.Duration
		defer func() { out <- longest }()
		start, base := time.Now(), frames[0].DTS
		for _, f := range frames {
			due := start.Add(time.Duration(float64(f.DTS-base) / media.ClockRate / speed * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				tm := time.NewTimer(d)
				select {
				case <-ctx.Done():
					tm.Stop()
					return
				case <-tm.C:
				}
			}
			if ctx.Err() != nil {
				return
			}
			t0 := time.Now()
			h.Write(f)
			longest = max(longest, time.Since(t0))
		}
	}()
	return out
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// waits records the backoff waits a Pusher asks for and lets each one end
// after a millisecond.
type waits struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waits) After(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	w.d = append(w.d, d)
	w.mu.Unlock()
	return time.After(time.Millisecond)
}

func (w *waits) get() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.d...)
}

// run starts p.Run; the channel yields its result. The run is canceled and
// awaited when the test ends.
func run(t *testing.T, p *Pusher) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- p.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(waitFor):
			t.Error("Run did not return")
		}
	})
	return cancel, done
}

func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * waitFor):
		t.Fatal("Run did not return")
		return nil
	}
}

func newPusher(t *testing.T, h *hub.Hub, target string, opt Options) *Pusher {
	t.Helper()
	p, err := New(h, target, opt)
	require.NoError(t, err)
	return p
}

// summary describes a checked connection.
type summary struct {
	metas, video, audio, keys int
	firstVideoTS              uint32
	video0                    bool
	codecs                    map[rtmp.VideoCodec]bool
}

// check verifies the messages of one connection: they start with
// onMetaData; each carried track's sequence header comes after every
// onMetaData before the track's next frame; on a connection with video the
// first video frame after every onMetaData is a keyframe, and no audio is
// sent before the first one or presented before it; timestamps never
// decrease per track.
func check(t *testing.T, msgs []rtmp.Media) summary {
	t.Helper()
	s := summary{codecs: map[rtmp.VideoCodec]bool{}}
	var (
		withVideo, vHdr, aHdr, needKey bool
		started                        bool // the connection's first keyframe was sent
		keyTS                          int64
		prevVideo, prevAudio           int64 = -1, -1
	)
	require.NotEmpty(t, msgs)
	require.Equal(t, uint8(rtmp.MsgDataAMF0), msgs[0].Type, "a connection starts with onMetaData")
	for i, m := range msgs {
		switch m.Type {
		case rtmp.MsgDataAMF0:
			vals, err := rtmp.DecodeAMF0(m.Payload)
			require.NoError(t, err, "message %d", i)
			require.Len(t, vals, 2)
			require.Equal(t, "onMetaData", vals[0])
			arr, ok := vals[1].(rtmp.ECMAArray)
			require.True(t, ok)
			_, withVideo = lookup(arr, "videocodecid")
			s.metas++
			vHdr, aHdr, needKey = false, false, withVideo
		case rtmp.MsgVideo:
			require.True(t, withVideo, "video on a connection without video")
			tag, err := rtmp.ParseVideoTag(m.Payload)
			require.NoError(t, err, "message %d", i)
			s.codecs[tag.Codec] = true
			if tag.Kind == rtmp.VideoSequenceStart {
				vHdr = true
				continue
			}
			require.Equal(t, rtmp.VideoCodedFrames, tag.Kind)
			require.True(t, vHdr, "video frame %d before its header", i)
			at := int64(m.Timestamp)
			if needKey {
				require.True(t, tag.Key, "first video frame after onMetaData is a keyframe (message %d)", i)
				needKey = false
				if !started {
					started, keyTS = true, at+int64(tag.CompTime)
				}
			}
			require.GreaterOrEqual(t, at, prevVideo, "video timestamps never decrease (message %d)", i)
			if !s.video0 {
				s.video0, s.firstVideoTS = true, m.Timestamp
			}
			prevVideo = at
			s.video++
			if tag.Key {
				s.keys++
			}
		case rtmp.MsgAudio:
			tag, err := rtmp.ParseAudioTag(m.Payload)
			require.NoError(t, err, "message %d", i)
			if tag.SequenceHeader {
				aHdr = true
				continue
			}
			require.True(t, aHdr, "audio frame %d before its header", i)
			at := int64(m.Timestamp)
			if withVideo {
				require.True(t, started, "audio before the first keyframe (message %d)", i)
				require.GreaterOrEqual(t, at, keyTS, "audio presented before the keyframe (message %d)", i)
			}
			require.GreaterOrEqual(t, at, prevAudio, "audio timestamps never decrease (message %d)", i)
			prevAudio = at
			s.audio++
		default:
			t.Fatalf("message %d: unexpected type %d", i, m.Type)
		}
	}
	return s
}

func lookup(arr rtmp.ECMAArray, key string) (any, bool) {
	for _, p := range arr {
		if p.Key == key {
			return p.Value, true
		}
	}
	return nil, false
}

// split cuts the received messages into connections: each starts with an
// onMetaData at timestamp 0, while one within a connection (a new epoch, a
// resume) comes later.
func split(msgs []rtmp.Media) [][]rtmp.Media {
	var out [][]rtmp.Media
	for _, m := range msgs {
		if m.Type == rtmp.MsgDataAMF0 && m.Timestamp == 0 {
			out = append(out, nil)
		}
		if len(out) > 0 {
			out[len(out)-1] = append(out[len(out)-1], m)
		}
	}
	return out
}

func TestPushNewRejects(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	for _, bad := range []string{
		"http://h/live/" + vKey,
		"rtmp+insecure://u:" + vPass + "@h/live/" + vKey + "?token=" + vToken,
		"rtmps+other://u:" + vPass + "@h/live/" + vKey,
		"srt://h:9000?passphrase=" + vToken,
		"rtmp://h/" + vKey,
		"rtmp:///live/" + vKey,
		"rtmp://h/live//" + vKey,
		"::" + vKey,
		"",
	} {
		p, err := New(h, bad, Options{})
		require.Error(t, err, "%q", bad)
		assert.Nil(t, p)
		assert.True(t, strings.HasPrefix(err.Error(), "rtmppush: "), err.Error())
		for _, s := range []string{vKey, vPass, vToken} {
			assert.NotContains(t, err.Error(), s)
		}
	}
	_, err := New(nil, "rtmp://h/live/s", Options{})
	require.Error(t, err)
}

// The pusher publishes the hub's stream: the cached GOP first, then every
// frame, with onMetaData and headers at the start and timestamps from 0; the
// target's query reaches it verbatim; closing the hub ends the publish
// cleanly and Run returns nil.
func TestPushFlow(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	frames := looped(t, "h264_aac.ts", 1)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	target := srv.URL("live/" + vKey + "?mode=x&k=v")
	p := newPusher(t, h, target, Options{})
	assert.Equal(t, StateIdle, p.Stats().State)
	_, done := run(t, p)
	require.Eventually(t, func() bool { return len(h.Stats()) == 1 }, waitFor, time.Millisecond)
	assert.Equal(t, "rtmppush "+redact.URL(target), h.Stats()[0].Name)
	assert.NotContains(t, h.Stats()[0].Name, vKey)
	assert.Equal(t, StatePublishing, p.Stats().State)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	longest := <-feed(ctx, h, rest, 8)
	assert.Less(t, longest, 100*time.Millisecond, "hub writes never block")
	h.Close()
	require.NoError(t, waitRun(t, done))
	require.Eventually(t, func() bool { return len(srv.Ends()) == 1 }, waitFor, time.Millisecond)
	require.ErrorIs(t, srv.Ends()[0], io.EOF, "the publish ended with FCUnpublish")

	msgs := srv.Media()
	s := check(t, msgs)
	assert.Equal(t, 1, s.metas)
	assert.Equal(t, 150, s.video, "every video frame of the clip")
	assert.Positive(t, s.audio)
	assert.Equal(t, uint32(0), s.firstVideoTS)

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "live", reqs[0].App)
	assert.Equal(t, vKey, reqs[0].Stream)
	assert.Equal(t, "mode=x&k=v", reqs[0].Query, "the target's query reaches it verbatim")

	st := p.Stats()
	assert.Equal(t, StateStopped, st.State)
	assert.Equal(t, uint64(1), st.Connects)
	assert.Zero(t, st.Failures)
	assert.Zero(t, st.Republishes)
	assert.Zero(t, st.Resyncs)
	assert.Equal(t, uint64(len(msgs)), st.Messages)
	assert.Equal(t, uint64(len(frames)), st.Frames, "every frame was delivered")
	assert.Empty(t, st.LastError)
	assert.Empty(t, h.Stats(), "unsubscribed")
}

// A target that closes mid-stream is reconnected after a backoff, and every
// connection starts at a keyframe with onMetaData and headers, its
// timestamps from 0.
func TestPushReconnects(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	const perConn = 60
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{CloseAfter: perConn})
	frames := looped(t, "h264_aac.ts", 6)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	w := &waits{}
	p := newPusher(t, h, srv.URL("live/"+vKey), Options{After: w.After})
	cancelRun, done := run(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fed := feed(ctx, h, rest, 4)
	require.Eventually(t, func() bool {
		ends := 0
		for _, e := range srv.Ends() {
			if e == nil {
				ends++
			}
		}
		return ends >= 4
	}, 3*waitFor, 10*time.Millisecond, "the server closed four connections")
	// The server sees a connection end before the pusher counts its
	// failure and asks for the next backoff.
	require.Eventually(t, func() bool {
		st := p.Stats()
		return st.Connects >= 4 && st.Failures >= 4 && len(w.get()) >= 4
	}, 3*waitFor, 10*time.Millisecond, "the pusher counted four failed connections")
	cancelRun()
	require.ErrorIs(t, waitRun(t, done), context.Canceled)
	cancel()
	assert.Less(t, <-fed, 100*time.Millisecond, "hub writes never block")

	conns := split(srv.Media())
	require.GreaterOrEqual(t, len(conns), 4)
	for i, c := range conns[:4] {
		require.Len(t, c, perConn, "connection %d", i)
		s := check(t, c)
		assert.Equal(t, 1, s.metas, "connection %d", i)
		require.True(t, s.video0, "connection %d carries video", i)
		assert.Equal(t, uint32(0), s.firstVideoTS, "connection %d starts at 0", i)
	}
	st := p.Stats()
	assert.GreaterOrEqual(t, st.Connects, uint64(4))
	assert.GreaterOrEqual(t, st.Failures, uint64(4))
	assert.NotEmpty(t, st.LastError)
	assert.Zero(t, st.Republishes)
	got := w.get()
	require.GreaterOrEqual(t, len(got), 4)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}, got[:4],
		"connections shorter than 30 s do not reset the backoff")
}

// A target that rejects the publish is retried with an exponential backoff
// from 1 s to 30 s.
func TestPushRejectedBackoff(t *testing.T) {
	h := liveHub(t)
	defer h.Close()
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{Reject: true})
	w := &waits{}
	p := newPusher(t, h, srv.URL("live/"+vKey), Options{After: w.After})
	cancelRun, done := run(t, p)
	require.Eventually(t, func() bool { return len(w.get()) >= 8 }, 3*waitFor, time.Millisecond)
	cancelRun()
	require.ErrorIs(t, waitRun(t, done), context.Canceled)
	s := time.Second
	assert.Equal(t, []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s}, w.get()[:8])
	st := p.Stats()
	assert.Zero(t, st.Connects)
	assert.GreaterOrEqual(t, st.Failures, uint64(8))
	assert.Contains(t, st.LastError, "NetStream.Publish.BadName")
	assert.Equal(t, StateStopped, st.State)
	assert.Empty(t, h.Stats(), "a rejected publish never subscribes")
}

// No publish is made before the hub is live: the pusher polls it, and ends
// without reaching the target when ctx ends or the hub closes first.
func TestPushWaitsForLive(t *testing.T) {
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	h := hub.New(hub.Options{})
	defer h.Close()
	w := &waits{}
	p := newPusher(t, h, srv.URL("live/"+vKey), Options{After: w.After})
	_, done := run(t, p)
	require.Eventually(t, func() bool { return len(w.get()) >= 5 }, waitFor, time.Millisecond)
	assert.Empty(t, srv.Requests(), "no publish before the hub is live")
	assert.Equal(t, StateWaiting, p.Stats().State)
	assert.Empty(t, h.Stats(), "no subscription either")
	for _, d := range w.get() {
		assert.Equal(t, livePoll, d)
	}
	frames := looped(t, "h264_aac.ts", 1)
	gop, _ := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	require.Eventually(t, func() bool { return p.Stats().Connects == 1 }, waitFor, time.Millisecond)
	h.Close()
	require.NoError(t, waitRun(t, done))
	assert.Len(t, srv.Requests(), 1)
	assert.Zero(t, p.Stats().Failures)

	// The hub closes, or ctx ends, while the pusher waits.
	h = hub.New(hub.Options{})
	w = &waits{}
	p = newPusher(t, h, srv.URL("live/"+vKey), Options{After: w.After})
	_, done = run(t, p)
	require.Eventually(t, func() bool { return len(w.get()) >= 1 }, waitFor, time.Millisecond)
	h.Close()
	require.NoError(t, waitRun(t, done))
	h = hub.New(hub.Options{})
	defer h.Close()
	p = newPusher(t, h, srv.URL("live/"+vKey), Options{After: func(time.Duration) <-chan time.Time { return nil }})
	cancelRun, done := run(t, p)
	require.Eventually(t, func() bool { return p.Stats().State == StateWaiting }, waitFor, time.Millisecond)
	cancelRun()
	require.ErrorIs(t, waitRun(t, done), context.Canceled)
	assert.Len(t, srv.Requests(), 1, "no publish from a hub that never went live")
	assert.Zero(t, p.Stats().Failures)
}

// The backoff starts again at its minimum after a connection that sent for
// 30 s.
func TestPushBackoffResets(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{CloseAfter: 20})
	frames := looped(t, "h264_aac.ts", 6)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	w := &waits{}
	p := newPusher(t, h, srv.URL("live/"+vKey), Options{
		After: w.After,
		// Every reading is 31 s after the last: each connection has sent
		// for more than 30 s when it fails.
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(31 * time.Second)
			return now
		},
		MinBackoff: 2 * time.Second,
	})
	cancelRun, done := run(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fed := feed(ctx, h, rest, 4)
	require.Eventually(t, func() bool { return len(w.get()) >= 4 }, 3*waitFor, 10*time.Millisecond)
	cancelRun()
	require.ErrorIs(t, waitRun(t, done), context.Canceled)
	cancel()
	<-fed
	for i, d := range w.get()[:4] {
		assert.Equal(t, 2*time.Second, d, "wait %d", i)
	}
}

// A hub that closes while the target keeps failing ends Run during the
// backoff, without another attempt; a hub closed before Run never reaches
// the target.
func TestPushHubClosedWhileTargetDown(t *testing.T) {
	h := liveHub(t)
	defer h.Close()
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{Reject: true})
	waiting := make(chan struct{}, 16)
	never := make(chan time.Time) // the backoff never ends by itself
	p := newPusher(t, h, srv.URL("live/"+vKey), Options{After: func(time.Duration) <-chan time.Time {
		waiting <- struct{}{}
		return never
	}})
	_, done := run(t, p)
	select {
	case <-waiting:
	case <-time.After(waitFor):
		t.Fatal("no backoff")
	}
	h.Close()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not return when the hub closed")
	}
	assert.Len(t, srv.Requests(), 1, "no attempt after the hub closed")
	assert.Equal(t, StateStopped, p.Stats().State)

	p = newPusher(t, h, srv.URL("live/"+vKey), Options{After: func(time.Duration) <-chan time.Time { return never }})
	_, done = run(t, p)
	require.NoError(t, waitRun(t, done))
	assert.Len(t, srv.Requests(), 1, "a closed hub is never pushed")
	assert.Zero(t, p.Stats().Failures)
}

// scriptServer accepts publishers on 127.0.0.1 and accepts the n-th publish
// (from 1) when accept(n) says so, rejecting it otherwise; an accepted
// publish is read until it ends.
func scriptServer(t *testing.T, accept func(n int) bool) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		n     int
		conns []net.Conn
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				sc, err := rtmp.Accept(context.Background(), c, rtmp.Config{}, func(rtmp.Request) error {
					mu.Lock()
					n++
					ok := accept(n)
					mu.Unlock()
					if !ok {
						return rtmp.ErrStreamNotAvailable
					}
					return nil
				})
				if err != nil {
					return
				}
				defer func() { _ = sc.Close() }()
				for {
					if _, err := sc.ReadMedia(); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return "rtmp://" + ln.Addr().String() + "/live/" + vKey
}

// The backoff starts again at its minimum after a connection published for
// 30 s, whatever ended it: a republish after a codec change counts too.
func TestPushBackoffResetsOnRepublish(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	// Publishes 1 and 2 are rejected (backoff 1 s, 2 s), 3 is accepted and
	// republished after a codec change, 4 is rejected: its backoff is 1 s
	// again, since connection 3 was published for 31 s.
	target := scriptServer(t, func(n int) bool { return n == 3 || n >= 5 })
	avc := looped(t, "h264_aac.ts", 1)
	hevc := looped(t, "h265_aac51.ts", 1)
	hevc = shift(hevc, lastDTS(avc)+3600-hevc[0].DTS, avc[len(avc)-1].Epoch+1)
	gop, rest := firstGOP(avc)
	for _, f := range gop {
		h.Write(f)
	}
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	w := &waits{}
	p := newPusher(t, h, target, Options{
		After: w.After,
		// Every reading is 31 s after the last; only a published
		// connection reads it, at its start and its end.
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(31 * time.Second)
			return now
		},
	})
	_, done := run(t, p)
	require.Eventually(t, func() bool { return p.Stats().Connects == 1 }, waitFor, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, h, append(rest, hevc...), 8)
	require.Eventually(t, func() bool { return len(w.get()) >= 3 }, waitFor, time.Millisecond)
	h.Close()
	require.NoError(t, waitRun(t, done))
	assert.Equal(t, uint64(1), p.Stats().Republishes)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, time.Second}, w.get()[:3])
}

// The stream key, the userinfo and secret query values never appear in
// the pusher's errors, stats, name or hub subscription, while the target
// gets them verbatim.
func TestPushRedactsKey(t *testing.T) {
	h := liveHub(t)
	defer h.Close()
	secrets := []string{vKey, vPass, vToken}
	noSecrets := func(what, s string) {
		t.Helper()
		for _, sec := range secrets {
			assert.NotContains(t, s, sec, what)
		}
	}

	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{Reject: true})
	target := "rtmp://user:" + vPass + "@" + srv.Addr + "/live/" + vKey + "?token=" + vToken
	w := &waits{}
	p := newPusher(t, h, target, Options{After: w.After})
	assert.Equal(t, redact.URL(target), p.String())
	assert.Contains(t, p.String(), "/live/sexxxxx")
	noSecrets("String", p.String())
	noSecrets("Sprint", fmt.Sprint(p))
	cancelRun, done := run(t, p)
	require.Eventually(t, func() bool { return p.Stats().Failures >= 2 }, waitFor, time.Millisecond)
	cancelRun()
	err := waitRun(t, done)
	require.Error(t, err)
	noSecrets("Run", err.Error())
	st := p.Stats()
	assert.Contains(t, st.LastError, "BadName")
	noSecrets("LastError", st.LastError)
	noSecrets("Stats", fmt.Sprintf("%+v", st))
	reqs := srv.Requests()
	require.NotEmpty(t, reqs)
	assert.Equal(t, vKey, reqs[0].Stream, "the target gets the key")
	assert.Equal(t, "token="+vToken, reqs[0].Query)

	// A target that refuses the connection.
	dead := "rtmp://" + freeAddr(t) + "/live/" + vKey + "?token=" + vToken
	p = newPusher(t, h, dead, Options{After: w.After})
	cancelRun, done = run(t, p)
	require.Eventually(t, func() bool { return p.Stats().Failures >= 1 }, waitFor, time.Millisecond)
	cancelRun()
	require.Error(t, waitRun(t, done))
	assert.NotEmpty(t, p.Stats().LastError)
	noSecrets("dial LastError", p.Stats().LastError)

	// Text that echoes the target is scrubbed too.
	p = newPusher(t, h, target, Options{})
	got := p.scrub(errors.New("server says: no " + vKey + "?token=" + vToken + " for user:" + vPass + " (" + vKey + ")"))
	noSecrets("scrub", got)
	assert.Contains(t, got, "sexxxxx")
}

// A codec change republishes at once, without a backoff, and the new
// connection starts at a keyframe of the new codec, with nothing replayed
// from before the change, even when the change comes before the new
// codec's first keyframe.
func TestPushCodecChangeRepublishes(t *testing.T) {
	for _, midGOP := range []bool{false, true} {
		t.Run(fmt.Sprintf("midGOP=%v", midGOP), func(t *testing.T) {
			h := hub.New(hub.Options{})
			defer h.Close()
			srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
			avc := looped(t, "h264_aac.ts", 1)
			hevc := looped(t, "h265_aac51.ts", 2)
			hevc = shift(hevc, lastDTS(avc)+3600-hevc[0].DTS, avc[len(avc)-1].Epoch+1)
			keys := keyIndexes(hevc)
			require.GreaterOrEqual(t, len(keys), 2)
			// before is written before the pusher can reconnect, after
			// once it has.
			before, after := hevc[:keys[0]+1], hevc[keys[0]+1:]
			if midGOP {
				// The change comes on delta frames: the new codec's
				// first keyframe is lost.
				before = append(append([]media.Frame(nil), hevc[:keys[0]]...), hevc[keys[0]+1:keys[1]]...)
				after = hevc[keys[1]:]
			}
			gop, rest := firstGOP(avc)
			for _, f := range gop {
				h.Write(f)
			}
			w := &waits{}
			p := newPusher(t, h, srv.URL("live/"+vKey), Options{After: w.After})
			_, done := run(t, p)
			require.Eventually(t, func() bool { return len(h.Stats()) == 1 }, waitFor, time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			<-feed(ctx, h, append(rest, before...), 8)
			require.Eventually(t, func() bool { return p.Stats().Connects == 2 }, waitFor, time.Millisecond)
			<-feed(ctx, h, after, 8)
			h.Close()
			require.NoError(t, waitRun(t, done))

			st := p.Stats()
			assert.Equal(t, uint64(1), st.Republishes)
			assert.Equal(t, uint64(2), st.Connects)
			assert.Zero(t, st.Failures)
			assert.Empty(t, w.get(), "no backoff")
			require.Len(t, srv.Requests(), 2)
			require.Eventually(t, func() bool { return len(srv.Ends()) == 2 }, waitFor, time.Millisecond)
			for _, e := range srv.Ends() {
				require.ErrorIs(t, e, io.EOF, "each publish ended cleanly")
			}
			conns := split(srv.Media())
			require.Len(t, conns, 2)
			first, second := check(t, conns[0]), check(t, conns[1])
			assert.Equal(t, map[rtmp.VideoCodec]bool{rtmp.VideoAVC: true}, first.codecs)
			assert.Equal(t, map[rtmp.VideoCodec]bool{rtmp.VideoHEVC: true}, second.codecs)
			assert.Equal(t, 1, second.metas)
			assert.Equal(t, uint32(0), second.firstVideoTS)
			assert.Positive(t, second.audio)
			if midGOP {
				assert.Positive(t, st.DroppedOnRepublish, "the replayed frames from before the change")
			}
		})
	}
}

// Codec changes republish at once, but after 3 republishes within 60 s the
// next one waits the backoff first, and is counted.
func TestPushRepublishRateLimited(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	avc := looped(t, "h264_aac.ts", 1)
	hevc := looped(t, "h265_aac51.ts", 1)
	epochs := [][]media.Frame{avc}
	for i := 1; i < 5; i++ {
		src := avc
		if i%2 == 1 {
			src = hevc
		}
		prev := epochs[i-1]
		epochs = append(epochs, shift(src, lastDTS(prev)+3600-src[0].DTS, prev[len(prev)-1].Epoch+1))
	}
	gop, rest := firstGOP(avc)
	for _, f := range gop {
		h.Write(f)
	}
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	w := &waits{}
	p := newPusher(t, h, srv.URL("live/"+vKey), Options{
		After: w.After,
		// Every reading is 1 s after the last: every republish is within
		// 60 s of the others.
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(time.Second)
			return now
		},
	})
	_, done := run(t, p)
	require.Eventually(t, func() bool { return p.Stats().Connects == 1 }, waitFor, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, h, rest, 8)
	for i, ep := range epochs[1:] {
		<-feed(ctx, h, ep, 8)
		require.Eventually(t, func() bool { return p.Stats().Connects == uint64(i+2) }, waitFor, time.Millisecond)
	}
	h.Close()
	require.NoError(t, waitRun(t, done))
	st := p.Stats()
	assert.Equal(t, uint64(4), st.Republishes)
	assert.Equal(t, uint64(1), st.RepublishBackoffs)
	assert.Zero(t, st.Failures)
	assert.Equal(t, []time.Duration{time.Second}, w.get(), "the 4th republish waits the backoff")
}

// ffmpeg listening for a publisher receives the pushed stream and writes a
// file ffprobe decodes as H.264 and AAC.
func TestPushFFmpegListener(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	ffprobe := testpeer.Require(t, "ffprobe")
	h := hub.New(hub.Options{})
	defer h.Close()
	frames := looped(t, "h264_aac.ts", 1)
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		h.Write(f)
	}
	target := "rtmp://" + freeAddr(t) + "/live/" + vKey
	out := filepath.Join(t.TempDir(), "out.flv")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-listen", "1", "-i", target, "-c", "copy", "-f", "flv", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// The pusher retries until ffmpeg listens.
	p := newPusher(t, h, target, Options{After: func(time.Duration) <-chan time.Time {
		return time.After(50 * time.Millisecond)
	}})
	_, done := run(t, p)
	require.Eventually(t, func() bool { return p.Stats().Connects == 1 }, waitFor, 10*time.Millisecond,
		"ffmpeg: %s", &stderr)
	<-feed(ctx, h, rest, 4)
	h.Close()
	require.NoError(t, waitRun(t, done))
	select {
	case err := <-exited:
		require.NoError(t, err, "ffmpeg: %s", stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("ffmpeg did not exit: %s", stderr.String())
	}
	probe, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries",
		"stream=codec_type,codec_name,width,height", "-of", "default=nw=1", out).CombinedOutput()
	require.NoError(t, err, "%s", probe)
	for _, s := range []string{"codec_name=h264", "width=320", "height=240", "codec_name=aac"} {
		assert.Contains(t, string(probe), s)
	}
	assert.Equal(t, uint64(1), p.Stats().Connects)
}

// The secrets of TestPushTLS, built so the source holds neither whole.
var (
	tlsKey   = "s3cret" + "key"
	tlsToken = "tok-" + "opensesame"
)

func TestCheckTargetTLS(t *testing.T) {
	for _, ok := range []string{
		"rtmps://h/live/k",
		"rtmps+insecure://h:443/live/k?token=t",
		"RTMPS://h/live/k",
	} {
		assert.NoError(t, checkTarget(ok), "%q", ok)
	}
	for _, bad := range []string{
		"rtmp+insecure://h/live/k",
		"https://h/live/k",
		"rtmps+other://h/live/k",
	} {
		err := checkTarget(bad)
		require.Error(t, err, "%q", bad)
		assert.Equal(t, "not an rtmp:// or rtmps:// URL", err.Error(), "%q", bad)
	}
	// Through New, the refusal is prefixed and never quotes the URL.
	h := hub.New(hub.Options{})
	defer h.Close()
	target := "rtmp+insecure://u:" + vPass + "@h/live/" + vKey + "?token=" + vToken
	_, err := New(h, target, Options{})
	require.Error(t, err)
	assert.Equal(t, "rtmppush: new: not an rtmp:// or rtmps:// URL", err.Error())
}

// pushAll publishes the h264_aac clip through a Pusher of opt to srv's
// path, from its first GOP to the hub's close, and returns what srv
// received and the request it saw.
func pushAll(t *testing.T, srv *rtmptest.PublishServer, target string, opt Options) ([]rtmp.Media, rtmp.Request) {
	t.Helper()
	h := hub.New(hub.Options{})
	defer h.Close()
	gop, rest := firstGOP(looped(t, "h264_aac.ts", 1))
	for _, f := range gop {
		h.Write(f)
	}
	p := newPusher(t, h, target, opt)
	_, done := run(t, p)
	require.Eventually(t, func() bool { return p.Stats().State == StatePublishing && len(h.Stats()) == 1 },
		waitFor, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, h, rest, 8)
	h.Close()
	require.NoError(t, waitRun(t, done))
	require.Eventually(t, func() bool { return len(srv.Ends()) == 1 }, waitFor, time.Millisecond)
	require.ErrorIs(t, srv.Ends()[0], io.EOF, "the publish ended with FCUnpublish")
	st := p.Stats()
	assert.Equal(t, uint64(1), st.Connects)
	assert.Zero(t, st.Failures)
	assert.Empty(t, st.LastError)
	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	return srv.Media(), reqs[0]
}

// dials records when each connection attempt dials, then dials.
type dials struct {
	mu sync.Mutex
	at []time.Time
}

func (d *dials) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.at = append(d.at, time.Now())
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func (d *dials) get() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.at...)
}

func TestPushTLS(t *testing.T) {
	ca := tlstest.NewCA(t)
	path := "live/" + tlsKey + "?token=" + tlsToken
	noSecrets := func(what, s string) {
		t.Helper()
		assert.NotContains(t, s, tlsKey, what)
		assert.NotContains(t, s, tlsToken, what)
	}

	plain := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	want, _ := pushAll(t, plain, plain.URL(path), Options{})
	require.NotEmpty(t, want)

	t.Run("verified", func(t *testing.T) {
		srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{TLS: ca.Server(t)})
		target := srv.URL(path)
		require.True(t, strings.HasPrefix(target, "rtmps://"), target)
		got, req := pushAll(t, srv, target, Options{RTMP: rtmp.Config{TLS: ca.Client()}})
		assert.Equal(t, want, got, "the media over RTMPS equals the plain run's")
		assert.Equal(t, tlsKey, req.Stream)
		assert.Equal(t, "token="+tlsToken, req.Query)
	})

	t.Run("untrusted", func(t *testing.T) {
		h := liveHub(t)
		defer h.Close()
		srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{TLS: ca.Server(t)})
		const minBackoff = 150 * time.Millisecond
		var (
			mu    sync.Mutex
			asked []time.Duration
		)
		after := func(d time.Duration) <-chan time.Time {
			if d != livePoll {
				mu.Lock()
				asked = append(asked, d)
				mu.Unlock()
			}
			return time.After(d)
		}
		d := &dials{}
		p := newPusher(t, h, srv.URL(path), Options{
			MinBackoff: minBackoff,
			After:      after,
			RTMP:       rtmp.Config{Dial: d.Dial},
		})
		cancelRun, done := run(t, p)
		require.Eventually(t, func() bool { return p.Stats().Failures >= 2 && len(d.get()) >= 2 },
			waitFor, time.Millisecond)
		cancelRun()
		require.ErrorIs(t, waitRun(t, done), context.Canceled)

		st := p.Stats()
		assert.Zero(t, st.Connects, "never publishes")
		assert.GreaterOrEqual(t, st.Failures, uint64(2))
		assert.Contains(t, st.LastError, "x509")
		noSecrets("LastError", st.LastError)
		assert.Empty(t, srv.Requests(), "the handshake failed before any RTMP")
		at := d.get()
		assert.GreaterOrEqual(t, at[1].Sub(at[0]), minBackoff,
			"the second attempt is not sooner than the first retry delay")
		mu.Lock()
		defer mu.Unlock()
		require.GreaterOrEqual(t, len(asked), 2)
		assert.Equal(t, []time.Duration{minBackoff, 2 * minBackoff}, asked[:2], "the plain back-off")
	})

	t.Run("insecure", func(t *testing.T) {
		srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{TLS: ca.Server(t)})
		target := "rtmps+insecure://" + srv.Addr + "/" + path
		h := hub.New(hub.Options{})
		defer h.Close()
		p := newPusher(t, h, target, Options{})
		assert.Equal(t, "rtmps+insecure://"+srv.Addr+"/live/s3xxxxx?token=xxxxx", p.String())
		h.Close()

		got, req := pushAll(t, srv, target, Options{})
		assert.Equal(t, want, got)
		assert.Equal(t, "live", req.App)
		assert.Equal(t, tlsKey, req.Stream)
		assert.Equal(t, "token="+tlsToken, req.Query, "the target's own query")
		for _, s := range []string{req.App, req.Stream, req.Query, req.TCURL} {
			assert.NotContains(t, s, "insecure")
		}
		assert.True(t, strings.HasPrefix(req.TCURL, "rtmps://"), req.TCURL)
	})

	t.Run("insecure keeps the caller's config", func(t *testing.T) {
		srv := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{TLS: ca.Server(t)})
		cfg := tlstest.NewCA(t).Client() // trusts another CA: verification would fail
		_, _ = pushAll(t, srv, "RTMPS+INSECURE://"+srv.Addr+"/"+path, Options{RTMP: rtmp.Config{TLS: cfg}})
		assert.False(t, cfg.InsecureSkipVerify, "the caller's config is not modified")
	})
}
