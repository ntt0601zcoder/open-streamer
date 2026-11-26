package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

type openResult struct {
	r   Reader
	err error
}

// openAsync opens rawURL in the background; the reader, if any, is closed
// when the test ends.
func openAsync(t *testing.T, ctx context.Context, rawURL string, env Env) <-chan openResult {
	t.Helper()
	ch := make(chan openResult, 1)
	go func() {
		r, err := Open(ctx, rawURL, env)
		ch <- openResult{r, err}
	}()
	return ch
}

// awaitOpen waits for an Open started by openAsync and requires success.
func awaitOpen(t *testing.T, ch <-chan openResult) Reader {
	t.Helper()
	select {
	case o := <-ch:
		require.NoError(t, o.err)
		t.Cleanup(func() { _ = o.r.Close() })
		return o.r
	case <-time.After(10 * time.Second):
		t.Fatal("Open did not return")
		return nil
	}
}

func rtmpListenURL(port, path string) string {
	return "rtmp://127.0.0.1:" + port + "/" + path + "?mode=listen"
}

// waitListening waits until a listen input has bound addr.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	require.Eventually(t, func() bool { return rtmpListeners.Stats(addr).ID != 0 },
		5*time.Second, 5*time.Millisecond, "nothing listens on %s", addr)
}

// waitPrePublish waits until the listener on addr has n sessions that have
// not published.
func waitPrePublish(t *testing.T, addr string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		st := rtmpListeners.Stats(addr)
		return st.ID != 0 && st.PrePublish == n
	}, 5*time.Second, 5*time.Millisecond, "want %d pre-publish sessions", n)
}

// freeTCPPort returns a TCP port that was free a moment ago.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// mustReject publishes to url and expects BadName with "stream not
// available".
func mustReject(t *testing.T, url string) {
	t.Helper()
	p, code, err := rtmptest.Dial(context.Background(), url)
	require.Nil(t, p)
	require.Error(t, err)
	assert.Equal(t, "NetStream.Publish.BadName", code)
	assert.Contains(t, err.Error(), "stream not available")
}

// idleTCP opens a TCP connection that sends nothing.
func idleTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// closedByPeer reports whether the server closed c within d.
func closedByPeer(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.Is(err, io.EOF) || !errors.As(err, &ne) || !ne.Timeout()
}

func fixtureTags(t *testing.T, name string) []rtmp.FLVTag {
	t.Helper()
	raw, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	tags, err := rtmp.ReadFLV(bytes.NewReader(raw))
	require.NoError(t, err)
	return tags
}

func tagsOf(msgs []rtmp.Media) []rtmp.FLVTag {
	out := make([]rtmp.FLVTag, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, rtmp.FLVTag{Type: m.Type, Timestamp: m.Timestamp, Data: m.Payload})
	}
	return out
}

// sourceFrames converts tags as one session, without arrivals.
func sourceFrames(t *testing.T, tags []rtmp.FLVTag) []media.Frame {
	t.Helper()
	f := newFLVFrames(&media.ConfigCache{}, time.Now)
	var out []media.Frame
	for i, tg := range tags {
		got, err := f.push(rtmp.Media{Type: tg.Type, Timestamp: tg.Timestamp, Payload: tg.Data})
		require.NoError(t, err, "tag %d", i)
		for _, fr := range got {
			out = append(out, fr.Frame)
		}
	}
	for _, fr := range f.flush() {
		out = append(out, fr.Frame)
	}
	return out
}

// heldAtEnd is the number of video frames flvFrames still holds after tags.
func heldAtEnd(t *testing.T, tags []rtmp.FLVTag) int {
	t.Helper()
	f := newFLVFrames(&media.ConfigCache{}, time.Now)
	for _, tg := range tags {
		_, err := f.push(rtmp.Media{Type: tg.Type, Timestamp: tg.Timestamp, Payload: tg.Data})
		require.NoError(t, err)
	}
	return len(f.flush())
}

func mediaFrames(frames []Frame) []media.Frame {
	out := make([]media.Frame, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Frame)
	}
	return out
}

// dialPublisher publishes to url and fails the test on a rejection. The
// publisher is abandoned and closed when the test ends.
func dialPublisher(t *testing.T, url string) *rtmptest.Publisher {
	t.Helper()
	p, code, err := rtmptest.Dial(context.Background(), url)
	require.NoError(t, err)
	require.Equal(t, "NetStream.Publish.Start", code)
	t.Cleanup(func() {
		p.Abandon()
		_ = p.Close()
	})
	return p
}

// publishTags publishes tags unpaced, then ends the publish with
// FCUnpublish in the background, since Close waits for the server to close,
// which it does only once the reader reads that far. The returned wait
// blocks until Close has returned; the test's end waits too. Close may
// report an error: the server closes on FCUnpublish.
func publishTags(t *testing.T, url string, tags []rtmp.FLVTag) (wait func()) {
	t.Helper()
	p := dialPublisher(t, url)
	for _, tg := range tags {
		require.NoError(t, p.WriteTag(tg))
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = p.Close()
	}()
	wait = func() { <-closed }
	t.Cleanup(wait)
	return wait
}

// readLog records what a reader returned.
type readLog struct {
	mu     sync.Mutex
	frames []Frame
	breaks []int // len(frames) at each ErrDiscontinuity
	err    error
	done   chan struct{}
}

// readAll reads r in the background until Read fails.
func readAll(r Reader) *readLog {
	l := &readLog{done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for {
			f, err := r.Read()
			l.mu.Lock()
			switch {
			case errors.Is(err, ErrDiscontinuity):
				l.breaks = append(l.breaks, len(l.frames))
			case err != nil:
				l.err = err
				l.mu.Unlock()
				return
			default:
				l.frames = append(l.frames, f)
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *readLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.frames)
}

func (l *readLog) waitCount(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return l.count() >= n }, 10*time.Second, 5*time.Millisecond,
		"want %d frames, have %d", n, l.count())
}

// stop closes r and waits for the reading goroutine.
func (l *readLog) stop(t *testing.T, r Reader) {
	t.Helper()
	require.NoError(t, r.Close())
	select {
	case <-l.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}

// Frames published by our own publisher come out exactly as flvFrames makes
// them from the source tags, stamped by Env.Now: H.264 over legacy FLV and
// H.265 over enhanced RTMP.
func TestRTMPListenOurPublisher(t *testing.T) {
	for _, name := range []string{"h264_aac.flv", "h265_aac.flv"} {
		t.Run(name, func(t *testing.T) {
			port := freeTCPPort(t)
			at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
			ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{Now: func() time.Time { return at }})
			waitListening(t, "127.0.0.1:"+port)
			tags := fixtureTags(t, name)
			want := sourceFrames(t, tags)
			publishTags(t, "rtmp://127.0.0.1:"+port+"/live/s", tags)

			r := awaitOpen(t, ch)
			assert.Equal(t, Spec{Live: true, Passive: true}, r.Spec())
			assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
			log := readAll(r)
			log.waitCount(t, len(want))
			time.Sleep(50 * time.Millisecond) // nothing more arrives
			log.stop(t, r)
			assert.Equal(t, want, mediaFrames(log.frames))
			assert.Empty(t, log.breaks)
			for _, f := range log.frames {
				require.Equal(t, at, f.Arrival)
			}
			st := r.Stats()
			assert.EqualValues(t, len(want), st["frames"])
			assert.EqualValues(t, 1, st["publishers"])
			assert.Zero(t, st["takeovers"])
			assert.Zero(t, st["bad_tags"])
		})
	}
}

// A publisher that ends is followed by the next one on the same port, with
// one ErrDiscontinuity in between and stable config pointers.
func TestRTMPListenReconnect(t *testing.T) {
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, addr)
	ln := rtmpListeners.Stats(addr).ID
	tags := fixtureTags(t, "h264_aac.flv")
	want := sourceFrames(t, tags)

	ended := publishTags(t, "rtmp://"+addr+"/live/s", tags)
	r := awaitOpen(t, ch)
	log := readAll(r)
	log.waitCount(t, len(want))
	ended() // the server closed the first publisher
	assert.Equal(t, ln, rtmpListeners.Stats(addr).ID, "the listener stays up between publishers")

	publishTags(t, "rtmp://"+addr+"/live/s", tags)
	log.waitCount(t, 2*len(want))
	time.Sleep(50 * time.Millisecond)
	log.stop(t, r)

	require.Len(t, log.frames, 2*len(want))
	assert.Equal(t, []int{len(want)}, log.breaks, "one ErrDiscontinuity, after the first publisher's frames")
	assert.Equal(t, want, mediaFrames(log.frames[:len(want)]))
	assert.Equal(t, want, mediaFrames(log.frames[len(want):]))
	first, second := byTrack(log.frames[:len(want)], 0), byTrack(log.frames[len(want):], 0)
	assert.Same(t, first[0].Config, second[0].Config, "an equal config keeps its pointer across publishers")
	assert.EqualValues(t, 2, r.Stats()["publishers"])
	assert.EqualValues(t, 2*len(want), r.Stats()["frames"])
}

// A publisher that ends while its first video frames are still held has
// them returned, in order, before the ErrDiscontinuity.
func TestRTMPListenReconnectHeldVideo(t *testing.T) {
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, addr)
	tags := fixtureTags(t, "h264_aac.flv")
	short := tags[:40]
	require.Positive(t, heldAtEnd(t, short), "the short session must end with video held")
	wantShort, want := sourceFrames(t, short), sourceFrames(t, tags)

	ended := publishTags(t, "rtmp://"+addr+"/live/s", short)
	r := awaitOpen(t, ch)
	log := readAll(r)
	log.waitCount(t, len(wantShort))
	ended()
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds(), "kinds stay after the publisher ends")
	publishTags(t, "rtmp://"+addr+"/live/s", tags)
	log.waitCount(t, len(wantShort)+len(want))
	time.Sleep(50 * time.Millisecond)
	log.stop(t, r)
	require.Len(t, log.frames, len(wantShort)+len(want))
	assert.Equal(t, []int{len(wantShort)}, log.breaks)
	assert.Equal(t, wantShort, mediaFrames(log.frames[:len(wantShort)]))
	assert.Equal(t, want, mediaFrames(log.frames[len(wantShort):]))
}

// A publisher that ends before its tracks are known is dropped with the
// frames queued from it; Open goes on with the next one, without a break.
func TestRTMPListenDropsPublisherBeforeTracks(t *testing.T) {
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, addr)
	early := tagsOf(kindsSession(t, 0, -1, 500)) // video only, under 1 s
	require.NotEmpty(t, sourceFrames(t, early))
	ended := publishTags(t, "rtmp://"+addr+"/live/s", early)
	ended() // read by Open, which is still waiting
	select {
	case o := <-ch:
		t.Fatalf("Open returned with the first publisher: %v", o.err)
	default:
	}
	tags := fixtureTags(t, "h264_aac.flv")
	want := sourceFrames(t, tags)
	publishTags(t, "rtmp://"+addr+"/live/s", tags)
	r := awaitOpen(t, ch)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	log := readAll(r)
	log.waitCount(t, len(want))
	time.Sleep(50 * time.Millisecond)
	log.stop(t, r)
	assert.Equal(t, want, mediaFrames(log.frames))
	assert.Empty(t, log.breaks)
	assert.EqualValues(t, 2, r.Stats()["publishers"])
	assert.EqualValues(t, len(want), r.Stats()["frames"], "frames dropped during Open are not counted")
}

// busy holds while the current publisher received a byte, or was offered,
// within 2 s.
func TestRTMPListenBusy(t *testing.T) {
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	sink := newRTMPListen(Spec{Live: true, Passive: true}, addr, time.Now)
	unregister, err := rtmpListeners.RegisterPublish(addr, "live/s", sink, rtmp.Config{}, rtmpsrv.Policy{})
	require.NoError(t, err)
	sink.unregister = unregister
	t.Cleanup(func() { _ = sink.Close() })
	assert.False(t, sink.Busy(time.Now()), "no publisher")

	p := dialPublisher(t, "rtmp://"+addr+"/live/s") // sends nothing after publish
	var cur *rtmpSession
	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		cur = sink.cur
		return cur != nil
	}, 5*time.Second, 5*time.Millisecond)
	last := cur.conn.LastInbound()
	require.False(t, cur.offered.Before(last))
	from := cur.offered
	assert.True(t, sink.Busy(from.Add(1900*time.Millisecond)))
	assert.False(t, sink.Busy(from.Add(2100*time.Millisecond)))
	// A slow handoff: the offer time counts as the last inbound byte.
	sink.mu.Lock()
	cur.offered = last.Add(5 * time.Second)
	sink.mu.Unlock()
	assert.True(t, sink.Busy(last.Add(6*time.Second)))
	assert.False(t, sink.Busy(last.Add(7100*time.Millisecond)))
	_ = p
}

// A publisher that goes silent without closing is taken over by a new one
// once it has sent nothing for 2 s, within 3 s, with one discontinuity.
func TestRTMPListenTakeoverHalfOpen(t *testing.T) {
	port := freeTCPPort(t)
	url := "rtmp://127.0.0.1:" + port + "/live/s"
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, "127.0.0.1:"+port)
	tags := fixtureTags(t, "h264_aac.flv")
	short := tags[:40] // ends with video held: it comes out before the break
	require.Positive(t, heldAtEnd(t, short))
	wantShort, want := sourceFrames(t, short), sourceFrames(t, tags)

	p1 := dialPublisher(t, url)
	for _, tg := range short {
		require.NoError(t, p1.WriteTag(tg))
	}
	r := awaitOpen(t, ch)
	log := readAll(r)
	log.waitCount(t, len(wantShort)-heldAtEnd(t, short))
	p1.Abandon()
	start := time.Now()

	var p2 *rtmptest.Publisher
	rejected := 0
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p, code, err := rtmptest.Dial(ctx, url)
		if err != nil {
			assert.Equal(t, "NetStream.Publish.BadName", code)
			rejected++
			return false
		}
		p2 = p
		return true
	}, 4*time.Second, 50*time.Millisecond)
	elapsed := time.Since(start)
	t.Cleanup(func() { p2.Abandon(); _ = p2.Close() })
	assert.Less(t, elapsed, 3*time.Second, "takeover took %v", elapsed)
	assert.Greater(t, elapsed, 1500*time.Millisecond, "a publisher that sent recently is not taken over")
	assert.Positive(t, rejected)

	for _, tg := range tags {
		require.NoError(t, p2.WriteTag(tg))
	}
	log.waitCount(t, len(wantShort)+len(want))
	time.Sleep(50 * time.Millisecond)
	log.stop(t, r)
	require.Len(t, log.frames, len(wantShort)+len(want))
	assert.Equal(t, []int{len(wantShort)}, log.breaks)
	assert.Equal(t, wantShort, mediaFrames(log.frames[:len(wantShort)]))
	assert.Equal(t, want, mediaFrames(log.frames[len(wantShort):]))
	st := r.Stats()
	assert.EqualValues(t, 1, st["takeovers"])
	assert.EqualValues(t, 2, st["publishers"])
	assert.EqualValues(t, rejected, st["rejected_busy"])
}

// mustNotPlay plays url and expects NetStream.Play.StreamNotFound with
// "stream not available": a publish path is never playable.
func mustNotPlay(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := rtmp.Play(ctx, url)
	if c != nil {
		_ = c.Close()
	}
	var st *rtmp.StatusError
	require.ErrorAs(t, err, &st)
	assert.Equal(t, "NetStream.Play.StreamNotFound", st.Code)
	assert.Equal(t, "stream not available", st.Description)
}

// A play on a publish path is rejected before any reservation: it neither
// counts as a publisher nor takes over a quiet one, and the route stays free.
func TestRTMPListenPlayIsNotPublish(t *testing.T) {
	port := freeTCPPort(t)
	url := "rtmp://127.0.0.1:" + port + "/live/s"
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, "127.0.0.1:"+port)

	mustNotPlay(t, url) // the route is idle
	tags := fixtureTags(t, "h264_aac.flv")
	p1 := dialPublisher(t, url)
	for _, tg := range tags[:40] {
		require.NoError(t, p1.WriteTag(tg))
	}
	r := awaitOpen(t, ch)
	log := readAll(r)
	log.waitCount(t, 10)
	p1.Abandon()
	time.Sleep(takeoverIdle + 300*time.Millisecond) // p1 is now quiet

	mustNotPlay(t, url)
	st := r.Stats()
	assert.EqualValues(t, 1, st["publishers"])
	assert.Zero(t, st["takeovers"])
	assert.EqualValues(t, 2, st["rejected_play_publish_path"])
	assert.Zero(t, st["rejected_unknown"], "plays have their own counters")
	assert.Zero(t, st["rejected_busy"])
	assert.Empty(t, log.breaks, "the quiet publisher was not taken over")

	// The route was left unreserved: a real publisher still takes over.
	dialPublisher(t, url)
	require.Eventually(t, func() bool { return r.Stats()["takeovers"] == 1 }, 2*time.Second, 20*time.Millisecond)
	assert.EqualValues(t, 2, r.Stats()["publishers"])
	log.stop(t, r)
}

// A second publisher is rejected with BadName while the first is sending,
// and the rejection does not echo the stream key.
func TestRTMPListenRejectsSecond(t *testing.T) {
	port := freeTCPPort(t)
	path := "live/" + vStreamKey
	url := "rtmp://127.0.0.1:" + port + "/" + path
	ch := openAsync(t, context.Background(), rtmpListenURL(port, path), Env{})
	waitListening(t, "127.0.0.1:"+port)
	ctx, cancel := context.WithCancel(context.Background())
	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		_ = rtmptest.Publish(ctx, url, fixtureTags(t, "h264_aac.flv"), true)
	}()
	t.Cleanup(func() { cancel(); <-pubDone })
	r := awaitOpen(t, ch)
	log := readAll(r)
	log.waitCount(t, 10)

	p, code, err := rtmptest.Dial(context.Background(), url)
	require.Nil(t, p)
	require.Error(t, err)
	assert.Equal(t, "NetStream.Publish.BadName", code)
	assert.Contains(t, err.Error(), "stream not available")
	assert.NotContains(t, err.Error(), vStreamKey)
	assert.EqualValues(t, 1, r.Stats()["rejected_busy"])
	assert.Zero(t, r.Stats()["takeovers"])
	cancel()
	<-pubDone
	log.stop(t, r)
	assert.Empty(t, log.breaks)
}

// Publishers are routed on the joined, normalised path (the rules are
// rtmpsrv's TestRegistryRouting).
func TestRTMPListenRouting(t *testing.T) {
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	chS := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	chInst := openAsync(t, context.Background(), "rtmp://127.0.0.1:"+port+"/live/inst/s?mode=listener", Env{})
	waitListening(t, addr)
	require.Eventually(t, func() bool { return rtmpListeners.Stats(addr).Routes == 2 },
		5*time.Second, 5*time.Millisecond)

	tags := fixtureTags(t, "h264_aac.flv")
	want := sourceFrames(t, tags)
	publishTags(t, "rtmp://"+addr+"/live/s?token=x", tags)
	publishTags(t, "rtmp://"+addr+"/live/inst/s", tags)
	mustReject(t, "rtmp://"+addr+"/live/zz")
	for _, ch := range []<-chan openResult{chS, chInst} {
		r := awaitOpen(t, ch)
		log := readAll(r)
		log.waitCount(t, len(want))
		log.stop(t, r)
		assert.Equal(t, want, mediaFrames(log.frames))
	}
}

// kindsSession builds a session of video (40 ms frames, a key every 25)
// and AAC (48 kHz) over [0, spanMS), with the audio sequence header at
// audioMS; a negative videoMS or audioMS leaves that kind out.
func kindsSession(t *testing.T, videoMS, audioMS, spanMS int) []rtmp.Media {
	t.Helper()
	cfg := videoConfig(t, "h264_aac.ts")
	type stamped struct {
		ms  int
		seq int
		m   rtmp.Media
	}
	var all []stamped
	add := func(ms int, m rtmp.Media) { all = append(all, stamped{ms, len(all), m}) }
	if videoMS >= 0 {
		add(videoMS, avcHeader(t, uint32(videoMS), cfg))
		for i := 0; videoMS+40*i < spanMS; i++ {
			ms := videoMS + 40*i
			if i%25 == 0 {
				add(ms, avcFrame(uint32(ms), true, 0, nalIDR))
			} else {
				add(ms, avcFrame(uint32(ms), false, 0, nalP))
			}
		}
	}
	if audioMS >= 0 {
		add(audioMS, audioMedia(uint32(audioMS), true, asc48k))
		for k := 0; ; k++ {
			ms := audioMS + k*1024*1000/48000
			if ms >= spanMS {
				break
			}
			add(ms, audioMedia(uint32(ms), false, []byte{0x21, 0x10, byte(k)}))
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ms != all[j].ms {
			return all[i].ms < all[j].ms
		}
		return all[i].seq < all[j].seq
	})
	out := make([]rtmp.Media, 0, len(all))
	for _, s := range all {
		out = append(out, s.m)
	}
	return out
}

// Open waits for both kinds to be announced, for at most 1 s of source time
// after the first media tag, and Read returns the frames queued meanwhile.
func TestFLVFramesKindsAtOpen(t *testing.T) {
	both := []media.Kind{media.KindVideo, media.KindAudio}
	for _, c := range []struct {
		name             string
		videoMS, audioMS int
		atOpen, later    []media.Kind
	}{
		{"audio header 500 ms after video", 0, 500, both, both},
		{"audio only", -1, 0, []media.Kind{0, media.KindAudio}, []media.Kind{0, media.KindAudio}},
		{"video only", 0, -1, []media.Kind{media.KindVideo}, []media.Kind{media.KindVideo}},
		{"audio header 1.5 s after video", 0, 1500, []media.Kind{media.KindVideo}, both},
	} {
		t.Run(c.name, func(t *testing.T) {
			port := freeTCPPort(t)
			ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
			waitListening(t, "127.0.0.1:"+port)
			tags := tagsOf(kindsSession(t, c.videoMS, c.audioMS, 3000))
			want := sourceFrames(t, tags)
			p := dialPublisher(t, "rtmp://127.0.0.1:"+port+"/live/s")
			for _, tg := range tags {
				require.NoError(t, p.WriteTag(tg))
			}
			r := awaitOpen(t, ch)
			assert.Equal(t, c.atOpen, r.Kinds(), "at open")
			log := readAll(r)
			log.waitCount(t, len(want))
			assert.Equal(t, c.later, r.Kinds(), "after reading")
			// The kinds stay once the publisher has gone.
			go func() { _ = p.Close() }()
			require.Eventually(t, func() bool {
				l := r.(*rtmpListen)
				l.mu.Lock()
				defer l.mu.Unlock()
				return l.active == nil
			}, 5*time.Second, 5*time.Millisecond)
			assert.Equal(t, c.later, r.Kinds(), "after the publisher ended")
			log.stop(t, r)
			assert.Equal(t, want, mediaFrames(log.frames))
		})
	}

	t.Run("both announced before any frame", func(t *testing.T) {
		port := freeTCPPort(t)
		ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
		waitListening(t, "127.0.0.1:"+port)
		p := dialPublisher(t, "rtmp://127.0.0.1:"+port+"/live/s")
		meta := metaMedia(t, "onMetaData", rtmp.ECMAArray{{Key: "videocodecid", Value: 7.0}, {Key: "audiocodecid", Value: 10.0}})
		require.NoError(t, p.WriteTag(rtmp.FLVTag{Type: rtmp.FLVScript, Data: meta.Payload}))
		r := awaitOpen(t, ch) // nothing else is sent
		assert.Equal(t, both, r.Kinds())
	})
}

// A publisher whose timestamps do not advance ends the Open wait at the
// queue bound instead of holding it until the ctx ends.
func TestRTMPListenOpenQueueBound(t *testing.T) {
	port := freeTCPPort(t)
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, "127.0.0.1:"+port)
	cfg := videoConfig(t, "h264_aac.ts")
	msgs := make([]rtmp.Media, 0, maxOpenFrames+102)
	msgs = append(msgs, avcHeader(t, 0, cfg), avcFrame(0, true, 0, nalIDR))
	for range maxOpenFrames + 100 {
		msgs = append(msgs, avcFrame(0, false, 0, nalP))
	}
	tags := tagsOf(msgs)
	p := dialPublisher(t, "rtmp://127.0.0.1:"+port+"/live/s")
	for _, tg := range tags {
		require.NoError(t, p.WriteTag(tg))
	}
	r := awaitOpen(t, ch)
	assert.Equal(t, []media.Kind{media.KindVideo}, r.Kinds())
	l := r.(*rtmpListen)
	assert.GreaterOrEqual(t, l.queue.len(), maxOpenFrames)
	assert.Less(t, l.queue.len(), maxOpenFrames+100, "Open stopped at the bound")
}

// frameQueue keeps its byte total as frames come and go, and tracksKnown
// applies Open's rule: both kinds, or one kind with 1 s of source time or a
// full queue.
func TestRTMPOpenQueue(t *testing.T) {
	var q frameQueue
	fr := func(sizes ...int) Frame {
		var f Frame
		for _, n := range sizes {
			f.Data = append(f.Data, make([]byte, n))
		}
		return f
	}
	q.push([]Frame{fr(10, 5), fr(7)})
	q.push(nil)
	q.push([]Frame{fr(3)})
	assert.Equal(t, 3, q.len())
	assert.Equal(t, 25, q.bytes)
	f, ok := q.pop()
	require.True(t, ok)
	assert.Len(t, f.Data, 2)
	assert.Equal(t, 10, q.bytes)
	assert.Equal(t, 2, q.drop())
	assert.Zero(t, q.len())
	assert.Zero(t, q.bytes)
	_, ok = q.pop()
	assert.False(t, ok)

	known := func(video, audio bool, spanned int64, q *frameQueue) bool {
		f := newFLVFrames(&media.ConfigCache{}, nil)
		f.seqVideo.Store(video)
		f.seqAudio.Store(audio)
		f.firstTS, f.maxTS = 1000, 1000+spanned
		return tracksKnown(f, q)
	}
	var empty frameQueue
	assert.False(t, known(false, false, 10*openSpan, &empty), "no kind announced")
	assert.True(t, known(true, true, 0, &empty), "both kinds")
	assert.False(t, known(true, false, openSpan-1, &empty))
	assert.True(t, known(false, true, openSpan, &empty), "one kind and 1 s")
	var full frameQueue
	full.push(make([]Frame, maxOpenFrames))
	assert.True(t, known(true, false, 0, &full), "one kind and the frame bound")
	assert.False(t, known(false, false, 0, &full), "the bound needs a kind")
	var big frameQueue
	big.push([]Frame{fr(maxOpenBytes)})
	assert.True(t, known(true, false, 0, &big), "one kind and the byte bound")
}

// A flood of idle connections is cut at the pre-publish cap; a legitimate
// publisher gets in once a slot frees.
// Not parallel: it sets the package's listen policy.
func TestRTMPListenFlood(t *testing.T) {
	setRTMPListenPolicy(t, rtmpsrv.Policy{MaxPrePublish: 16, MaxPrePublishPerIP: 16})
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, addr)

	conns := make([]net.Conn, 200)
	for i := range conns {
		conns[i] = idleTCP(t, addr)
	}
	require.Eventually(t, func() bool { return rtmpListeners.Stats(addr).RejectedFlood == 184 },
		5*time.Second, 5*time.Millisecond, "rejected_flood=%d", rtmpListeners.Stats(addr).RejectedFlood)
	waitPrePublish(t, addr, 16)
	var open []net.Conn
	closed := 0
	for _, c := range conns {
		if closedByPeer(c, 20*time.Millisecond) {
			closed++
		} else {
			open = append(open, c)
		}
	}
	assert.Equal(t, 184, closed, "the connections over the cap are closed")
	require.Len(t, open, 16)

	// While the slots are full a publisher is turned away; once one frees,
	// it is accepted.
	p, _, err := rtmptest.Dial(context.Background(), "rtmp://"+addr+"/live/s")
	require.Error(t, err)
	require.Nil(t, p)
	require.NoError(t, open[0].Close())
	waitPrePublish(t, addr, 15)
	tags := fixtureTags(t, "h264_aac.flv")
	pub := dialPublisher(t, "rtmp://"+addr+"/live/s")
	for _, tg := range tags[:20] {
		require.NoError(t, pub.WriteTag(tg))
	}
	r := awaitOpen(t, ch)
	assert.EqualValues(t, 185, r.Stats()["rejected_flood"])
}

// ffmpeg publishes into our listener; the frames pass the Clock clean.
func TestRTMPFFmpegPublish(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
	waitListening(t, addr)
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
		"-i", fixturePath("h264_aac.ts"), "-c", "copy", "-f", "flv", "rtmp://"+addr+"/live/s")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	r := awaitOpen(t, ch)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	log := readAll(r)
	select {
	case err := <-waited:
		waited <- err
		require.NoError(t, err, "ffmpeg: %s", stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("ffmpeg did not finish")
	}
	want := map[int]int{}
	for _, f := range demuxFrames(t, "h264_aac.ts") {
		want[f.Track]++
	}
	log.waitCount(t, want[0])
	time.Sleep(200 * time.Millisecond)
	log.stop(t, r)
	assert.Len(t, byTrack(log.frames, 0), want[0])
	assert.InDelta(t, want[1], len(byTrack(log.frames, 1)), 2)
	assert.Empty(t, log.breaks)
	st := r.Stats()
	assert.Zero(t, st["bad_tags"])
	assert.Zero(t, st["dropped_no_config"])
	clockCheck(t, r, log.frames)
}

// Close unblocks a Read that waits for media from a silent publisher, and
// one that waits for the next publisher; it frees the port and leaves no
// goroutine behind.
// Not parallel: it counts goroutines.
func TestRTMPListenCloseUnblocks(t *testing.T) {
	tags := fixtureTags(t, "h264_aac.flv")
	want := sourceFrames(t, tags[:40])
	cycle := func(t *testing.T, endPublisher bool) {
		port := stableTCPPort(t)
		addr := "127.0.0.1:" + port
		ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/s"), Env{})
		waitListening(t, addr)
		p, code, err := rtmptest.Dial(context.Background(), "rtmp://"+addr+"/live/s")
		require.NoError(t, err, code)
		for _, tg := range tags[:40] {
			require.NoError(t, p.WriteTag(tg))
		}
		closed := make(chan struct{})
		if endPublisher {
			go func() {
				defer close(closed)
				_ = p.Close()
			}()
		}
		r := <-ch
		require.NoError(t, r.err)
		log := readAll(r.r)
		if endPublisher {
			log.waitCount(t, len(want))
			<-closed
		} else {
			log.waitCount(t, 1)
		}
		time.Sleep(100 * time.Millisecond) // Read blocks
		log.stop(t, r.r)
		assert.ErrorIs(t, log.err, errRTMPClosed)
		_, err = r.r.Read()
		assert.ErrorIs(t, err, errRTMPClosed, "Read after Close")
		require.NoError(t, r.r.Close(), "Close is idempotent")
		assert.Zero(t, rtmpListeners.Stats(addr).ID)
		ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
		require.NoError(t, err, "Close frees the port")
		require.NoError(t, ln.Close())
		p.Abandon()
		_ = p.Close()
	}
	for _, end := range []bool{false, true} {
		cycle(t, end) // warm up lazily started goroutines
	}
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 3 {
		cycle(t, false)
		cycle(t, true)
	}
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		3*time.Second, 20*time.Millisecond, "goroutines before=%d now=%d", before, runtime.NumGoroutine())
}

func TestOpenRTMPListen(t *testing.T) {
	listen := Spec{Live: true, Passive: true}
	for _, c := range []struct {
		url  string
		want Spec
	}{
		{"rtmp://:1935/live/s?mode=listen", listen},
		{"rtmp:///live/s?mode=listen", listen},
		{"rtmp://0.0.0.0/live/s?mode=listener", listen},
		{"rtmp://[::]:1936/live/inst/s?mode=listen&token=x", listen},
		{"rtmp://127.0.0.1:1936/live/s?mode=listen", listen},
		{"RTMP://:1935/live/s?mode=listen", listen},
		{"rtmp://host/live/s", Spec{Live: true}},
		{"rtmp://host:1940/live/s?mode=pull&token=x", Spec{Live: true}},
	} {
		got, err := Parse(c.url)
		require.NoError(t, err, c.url)
		assert.Equal(t, c.want, got, c.url)
	}
	for _, bad := range []string{
		"rtmp:///live/s",                   // no host and no mode
		"rtmp:///live/s?mode=pull",         // pull needs a host
		"rtmp://:1935/live?mode=listen",    // no stream name
		"rtmp://:1935/?mode=listen",        // no path
		"rtmp://:1935/live/?mode=listen",   // empty stream name
		"rtmp://:0/live/s?mode=listen",     // bad port
		"rtmp://:99999/live/s?mode=listen", // bad port
		"rtmp://:1935/live/s?mode=caller",  // unknown mode
		"rtmp://host/live?mode=pull",       // no stream name
	} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}

	// The default port is 1935, for every wildcard spelling.
	for _, u := range []string{"rtmp:///live/s?mode=listen", "rtmp://0.0.0.0/live/s?mode=listen", "rtmp://[::]/live/s?mode=listener"} {
		p, err := parse(u)
		require.NoError(t, err)
		addr, path := rtmpListenTarget(p)
		key, err := rtmpsrv.AddrKey(addr)
		require.NoError(t, err)
		assert.Equal(t, ":1935", key, u)
		assert.Equal(t, "/live/s", path, u)
	}

	// The listener synonym opens and binds.
	port := freeTCPPort(t)
	ch := openAsync(t, context.Background(), "rtmp://127.0.0.1:"+port+"/live/s?mode=listener", Env{})
	waitListening(t, "127.0.0.1:"+port)
	publishTags(t, "rtmp://127.0.0.1:"+port+"/live/s", fixtureTags(t, "h264_aac.flv")[:20])
	awaitOpen(t, ch)

	// A port in use is an error that does not name the stream key.
	busy, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	_, err = Open(context.Background(), "rtmp://"+busy.Addr().String()+"/live/"+vStreamKey+"?mode=listen", Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), vStreamKey)
	assert.Contains(t, err.Error(), "live/skxxxxx")

	// An open nobody publishes to ends with its ctx and frees the port.
	port = stableTCPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = Open(ctx, rtmpListenURL(port, "live/"+vStreamKey), Env{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), vStreamKey)
	assert.Zero(t, rtmpListeners.Stats("127.0.0.1:"+port).ID)
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:"+port)
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	// A second listen input on the same path and port is refused.
	port = freeTCPPort(t)
	ch = openAsync(t, context.Background(), rtmpListenURL(port, "live/"+vStreamKey), Env{})
	waitListening(t, "127.0.0.1:"+port)
	_, err = Open(context.Background(), rtmpListenURL(port, "live/"+vStreamKey), Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), vStreamKey)
	publishTags(t, "rtmp://127.0.0.1:"+port+"/live/"+vStreamKey, fixtureTags(t, "h264_aac.flv")[:20])
	awaitOpen(t, ch)
}

// Rejections, open errors and stats never carry the stream key.
func TestRedactRTMPStreamKey(t *testing.T) {
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	ch := openAsync(t, context.Background(), rtmpListenURL(port, "live/"+vStreamKey), Env{})
	waitListening(t, addr)
	for _, url := range []string{"rtmp://" + addr + "/live/" + vStreamKey + "x", "rtmp://" + addr + "/other/" + vStreamKey} {
		p, code, err := rtmptest.Dial(context.Background(), url)
		require.Nil(t, p)
		require.Error(t, err)
		assert.Equal(t, "NetStream.Publish.BadName", code)
		assert.NotContains(t, err.Error(), vStreamKey)
	}
	publishTags(t, "rtmp://"+addr+"/live/"+vStreamKey, fixtureTags(t, "h264_aac.flv")[:20])
	r := awaitOpen(t, ch)
	for k := range r.Stats() {
		assert.NotContains(t, k, vStreamKey)
	}
	_, err := Open(context.Background(), rtmpListenURL(port, "live/"+vStreamKey), Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), vStreamKey)
}
