package rtspplay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

const waitFor = 10 * time.Second

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

// looped returns n back-to-back copies of a fixture in stream time: each
// copy's source timestamps continue after the previous one, and all of them
// go through one Clock, so the result is one epoch.
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

// firstGOP returns the frames before the second video keyframe.
func firstGOP(frames []media.Frame) (gop, rest []media.Frame) {
	keys := 0
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			if keys++; keys == 2 {
				return frames[:i], frames[i:]
			}
		}
	}
	return frames, nil
}

// feed writes frames to h in a goroutine, paced at speed times real time
// by DTS, until they run out or ctx ends. Before each Write it stores the
// frame's index in written, when set. The channel yields the longest Write,
// then closes.
func feed(ctx context.Context, h *hub.Hub, frames []media.Frame, speed float64, written *atomic.Int64) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	go func() {
		defer close(out)
		var longest time.Duration
		defer func() { out <- longest }()
		start, base := time.Now(), frames[0].DTS
		for i, f := range frames {
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
			if written != nil {
				written.Store(int64(i))
			}
			t0 := time.Now()
			h.Write(f)
			longest = max(longest, time.Since(t0))
		}
	}()
	return out
}

// env is a Server for one hub, stream "s" of app "live", on its own port.
type env struct {
	h    *hub.Hub
	srv  *Server
	addr string
	url  string
}

// start serves a fresh hub as live/s on 127.0.0.1, with the UDP pair on
// 127.0.0.1 when udp is set.
func start(t *testing.T, opt Options, udp bool) *env {
	t.Helper()
	h := hub.New(hub.Options{})
	if udp {
		opt.Server.UDPHost = "127.0.0.1"
	}
	srv := New(func(name string) (*hub.Hub, bool) {
		if name == "s" {
			return h, true
		}
		return nil, false
	}, opt)
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	if udp {
		require.Eventually(t, func() bool { return srv.UDPPorts()[0] != 0 }, waitFor, time.Millisecond)
	}
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		select {
		case err := <-served:
			assert.ErrorIs(t, err, rtsp.ErrServerClosed)
		case <-time.After(waitFor):
			t.Error("Serve did not return after Close")
		}
		h.Close()
	})
	addr := l.Addr().String()
	return &env{h: h, srv: srv, addr: addr, url: "rtsp://" + addr + "/live/s"}
}

// play dials a player over transport ("tcp" or "udp"); dial, when set,
// opens its control connection.
func play(t *testing.T, url, transport string, dial func(context.Context, string, string) (net.Conn, error)) *rtsp.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	c, err := rtsp.Dial(ctx, url, rtsp.Config{Transport: transport, Dial: dial})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// mustNotPlay checks that url is answered 404.
func mustNotPlay(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	c, err := rtsp.Dial(ctx, url, rtsp.Config{})
	if c != nil {
		_ = c.Close()
	}
	var st *rtsp.StatusError
	require.ErrorAs(t, err, &st)
	assert.Equal(t, 404, st.Code)
}

// result is what one player read: its packets and the error that ended it.
type result struct {
	pkts []rtsp.Packet
	err  error
}

// collect reads c in a goroutine until it ends, or until stop reports true
// for the packets read so far.
func collect(c *rtsp.Client, stop func([]rtsp.Packet) bool) <-chan result {
	out := make(chan result, 1)
	go func() {
		var r result
		for {
			p, err := c.ReadPacket()
			if err != nil {
				r.err = err
				break
			}
			r.pkts = append(r.pkts, p)
			if stop != nil && stop(r.pkts) {
				break
			}
		}
		out <- r
	}()
	return out
}

func wait(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * waitFor):
		t.Fatal("player did not finish")
		return result{}
	}
}

// session is what a player learnt from DESCRIBE and PLAY.
type session struct {
	desc rtsp.Description
	info []rtsp.RTPInfo
}

func sessionOf(c *rtsp.Client) session {
	return session{desc: c.Description(), info: c.RTPInfo()}
}

func (s session) video() (rtsp.Media, bool) {
	for _, m := range s.desc.Media {
		if m.Codec == rtsp.CodecH264 || m.Codec == rtsp.CodecH265 {
			return m, true
		}
	}
	return rtsp.Media{}, false
}

// videoAUs counts the video access units (distinct timestamps in a row) of
// the RTP packets read so far.
func videoAUs(s session, n int) func([]rtsp.Packet) bool {
	v, _ := s.video()
	return func(pkts []rtsp.Packet) bool {
		return countVideoAUs(v.Index, pkts) >= n
	}
}

func countVideoAUs(track int, pkts []rtsp.Packet) int {
	n := 0
	var prev uint32
	for _, p := range pkts {
		if p.RTP == nil || p.Track != track {
			continue
		}
		if n == 0 || p.RTP.Timestamp != prev {
			n++
		}
		prev = p.RTP.Timestamp
	}
	return n
}

// isParamSet reports whether a video RTP payload is a single-NAL SPS
// (H.264) or VPS (H.265): the first packet of every keyframe.
func isParamSet(c rtsp.Codec, pl []byte) bool {
	if len(pl) == 0 {
		return false
	}
	if c == rtsp.CodecH265 {
		return (pl[0]>>1)&0x3f == 32
	}
	return pl[0]&0x1f == 7
}

// RTCP packet types.
const (
	rtcpSR  = 200
	rtcpBye = 203
)

// rtcpTypes returns the packet types of a compound RTCP packet.
func rtcpTypes(b []byte) []byte {
	var out []byte
	for len(b) >= 4 {
		out = append(out, b[1])
		n := (int(binary.BigEndian.Uint16(b[2:])) + 1) * 4
		if n > len(b) {
			break
		}
		b = b[n:]
	}
	return out
}

// summary describes a checked playback.
type summary struct {
	videoAUs, keys, audio int
	// resumes counts video timestamp gaps, each at a resume keyframe.
	resumes int
	// seqGaps counts RTP sequence gaps over every track.
	seqGaps    int
	byes, srs  int
	firstVideo *rtp.Packet
	firstAudio *rtp.Packet
}

// maxVideoStep is the largest video timestamp step between access units in
// decode order within a run: the fixtures are 25 fps with at most 3 B
// frames.
const maxVideoStep = 18000

// check verifies a player's packets against the session: every track starts
// at the RTP-Info sequence number; the first video access unit, and the one
// after every video timestamp gap, is a keyframe that starts with its
// parameter sets; audio is never presented before the keyframe the session
// started or resumed at. With ordered (a TCP session, one stream) audio must
// also never arrive before that keyframe.
func check(t *testing.T, s session, pkts []rtsp.Packet, ordered bool) summary {
	t.Helper()
	var (
		sum            summary
		v, hasVideo    = s.video()
		prevSeq        = map[int]uint16{}
		started        = map[int]bool{}
		prevTS         uint32
		prevST         int64
		keyST          int64
		sawKey         bool
		firstKeyST     int64
		earliestAudio  = int64(1) << 62
		audioBeforeKey bool
	)
	for i, p := range pkts {
		if p.RTCP != nil {
			for _, ty := range rtcpTypes(p.RTCP) {
				switch ty {
				case rtcpBye:
					sum.byes++
				case rtcpSR:
					sum.srs++
				}
			}
			continue
		}
		require.Less(t, p.Track, len(s.info))
		info := s.info[p.Track]
		r := p.RTP
		if !started[p.Track] {
			started[p.Track] = true
			require.True(t, info.HasSeq && info.HasTime, "RTP-Info for track %d", p.Track)
			assert.Equal(t, info.Seq, r.SequenceNumber, "track %d starts at the RTP-Info seq", p.Track)
		} else if r.SequenceNumber != prevSeq[p.Track]+1 {
			sum.seqGaps++
		}
		prevSeq[p.Track] = r.SequenceNumber
		st := int64(int32(r.Timestamp - info.RTPTime)) // ticks of the track clock from b
		m := s.desc.Media[p.Track]
		if hasVideo && p.Track == v.Index {
			if sum.videoAUs > 0 && r.Timestamp == prevTS {
				continue
			}
			key := isParamSet(m.Codec, r.Payload)
			d := st - prevST
			gap := sum.videoAUs > 0 && (d > maxVideoStep || d < -maxVideoStep)
			if sum.videoAUs == 0 {
				require.True(t, key, "the first video access unit is a keyframe with parameter sets")
				sum.firstVideo = r
				firstKeyST = st
			} else if gap {
				require.True(t, key, "a video gap only at a resume keyframe (packet %d)", i)
				sum.resumes++
			}
			if key {
				sum.keys++
				if sum.videoAUs == 0 || gap {
					keyST = st
				}
				sawKey = true
			}
			sum.videoAUs++
			prevTS, prevST = r.Timestamp, st
			continue
		}
		if m.Codec != rtsp.CodecAAC {
			continue
		}
		if sum.audio == 0 {
			sum.firstAudio = r
		}
		sum.audio++
		ast := st * media.ClockRate / int64(m.ClockRate)
		earliestAudio = min(earliestAudio, ast)
		if !hasVideo {
			continue
		}
		if !sawKey {
			audioBeforeKey = true
			continue
		}
		if ordered {
			require.GreaterOrEqual(t, ast, keyST-2, "audio presented before the start or resume keyframe (packet %d)", i)
		}
	}
	if hasVideo && sum.audio > 0 {
		if ordered {
			assert.False(t, audioBeforeKey, "audio before the first keyframe")
		}
		assert.GreaterOrEqual(t, earliestAudio, firstKeyST-2, "audio presented before the first keyframe")
	}
	return sum
}

// A TCP player joining mid-GOP starts at once on the cached keyframe, with
// parameter sets, at the RTP-Info origins, and gets no audio presented
// before it.
func TestPlayJoinsAtKeyframe(t *testing.T) {
	e := start(t, Options{}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := tagged(looped(t, "h264_aac.ts", 4))
	var written atomic.Int64
	written.Store(-1)
	done := feed(ctx, e.h, frames, 2, &written)
	latest := waitMidGOP(t, e, frames, &written)

	c := play(t, e.url, "tcp", nil)
	s := sessionOf(c)
	require.Len(t, s.desc.Media, 2)
	assert.Equal(t, rtsp.CodecH264, s.desc.Media[0].Codec)
	assert.Equal(t, rtsp.CodecAAC, s.desc.Media[1].Codec)
	r := wait(t, collect(c, videoAUs(s, 60)))
	require.NoError(t, r.err)
	sum := check(t, s, r.pkts, true)
	assert.GreaterOrEqual(t, sum.keys, 2)
	assert.Positive(t, sum.audio)
	assert.Zero(t, sum.seqGaps)
	assert.Zero(t, sum.resumes)
	first, jumps := tagRuns(t, s, frames, r.pkts)
	assert.Less(t, frames[first].DTS, latest, "the GOP cache was replayed: the first frame was written before PLAY")
	assert.Zero(t, jumps)
	// b is the keyframe's DTS: its timestamp is TSBase + its composition
	// offset.
	off := int32(sum.firstVideo.Timestamp - s.info[0].RTPTime)
	assert.GreaterOrEqual(t, off, int32(0))
	assert.Less(t, off, int32(media.ClockRate/5))
	cancel()
	<-done
	st := e.srv.Stats()
	assert.Equal(t, uint64(1), st.PlayersStarted)
	assert.Equal(t, 1, st.Players)
}

// A carried track's codec change ends the session with a BYE on every
// track, then the connection closes; no H.265 reaches the player. Every
// H.264 frame reaches it unless the hub dropped some for it (under load):
// then a GOP or more is missing, but never the cached one.
func TestPlayCodecChangeEndsSession(t *testing.T) {
	e := start(t, Options{}, false)
	avc := tagged(looped(t, "h264_aac.ts", 1))
	hevc := looped(t, "h265_aac51.ts", 1)
	hevc = shift(hevc, lastDTS(avc)+3600-hevc[0].DTS, avc[len(avc)-1].Epoch+1)
	gop, rest := firstGOP(avc)
	for _, f := range gop {
		e.h.Write(f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := play(t, e.url, "tcp", nil)
	s := sessionOf(c)
	pkts := collect(c, nil)
	// The session subscribes after the PLAY response: fed before that, it
	// can start at the second GOP's keyframe, from the cache.
	require.Eventually(t, func() bool { return len(e.h.Stats()) == 1 }, waitFor, time.Millisecond, "subscribed")
	// The session's hub drops, polled until it unsubscribes.
	var dropped uint64
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		seen := false
		for {
			if st := e.h.Stats(); len(st) > 0 {
				dropped, seen = st[len(st)-1].Dropped, true
			} else if seen {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
	<-feed(ctx, e.h, append(rest, hevc...), 8, nil)
	r := wait(t, pkts)
	require.Error(t, r.err)
	sum := check(t, s, r.pkts, true)
	for _, i := range tagIndexes(t, s, r.pkts) { // H.265 frames carry no tag
		require.Less(t, i, len(avc), "an H.264 frame")
	}
	select {
	case <-polled:
	case <-time.After(waitFor):
		t.Fatal("the session did not unsubscribe")
	}
	if dropped == 0 {
		assert.Equal(t, 150, sum.videoAUs, "every H.264 frame, none after")
	} else {
		t.Logf("the hub dropped %d frames; %d video access units", dropped, sum.videoAUs)
		assert.GreaterOrEqual(t, sum.videoAUs, 25, "the cached GOP")
		assert.Less(t, sum.videoAUs, 150, "a dropped GOP")
	}
	assert.Equal(t, 2, sum.byes, "a BYE per track")
	last := r.pkts[len(r.pkts)-1]
	require.NotNil(t, last.RTCP, "the BYEs come last")
	assert.Contains(t, rtcpTypes(last.RTCP), byte(rtcpBye))
	require.Eventually(t, func() bool { return e.srv.Stats().EndedCodecChange == 1 }, waitFor, time.Millisecond)
	assert.Empty(t, e.h.Stats(), "unsubscribed")
	assert.Equal(t, 0, e.srv.Stats().Players)
}

// Closing the hub ends every player with a BYE per track and unsubscribes
// it.
func TestPlayEndOfStream(t *testing.T) {
	e := start(t, Options{}, false)
	gop, _ := firstGOP(looped(t, "h264_aac.ts", 1))
	for _, f := range gop {
		e.h.Write(f)
	}
	c := play(t, e.url, "tcp", nil)
	s := sessionOf(c)
	pkts := collect(c, nil)
	require.Eventually(t, func() bool { return len(e.h.Stats()) == 1 }, waitFor, time.Millisecond)
	e.h.Close()
	r := wait(t, pkts)
	require.Error(t, r.err)
	sum := check(t, s, r.pkts, true)
	assert.Equal(t, 25, sum.videoAUs, "the cached GOP was played")
	assert.Equal(t, 2, sum.byes)
	require.Eventually(t, func() bool { return e.srv.Stats().EndedHubClosed == 1 }, waitFor, time.Millisecond)
	assert.Equal(t, 0, e.srv.Stats().Players)
	mustNotPlay(t, e.url)
}

// norm is an RTP packet with the session's origins taken out.
type norm struct {
	track   int
	seq     uint16
	ts      uint32
	marker  bool
	pt      uint8
	payload string
}

func normalise(s session, pkts []rtsp.Packet) []norm {
	var out []norm
	for _, p := range pkts {
		if p.RTP == nil {
			continue
		}
		info := s.info[p.Track]
		out = append(out, norm{
			track: p.Track, seq: p.RTP.SequenceNumber - info.Seq, ts: p.RTP.Timestamp - info.RTPTime,
			marker: p.RTP.Marker, pt: p.RTP.PayloadType, payload: string(p.RTP.Payload),
		})
	}
	return out
}

// Fifty players joining one after the other all get the same packets per
// track, but for their own SSRC, sequence and timestamp origins.
func TestPlayManyPlayers(t *testing.T) {
	e := start(t, Options{}, false)
	gop, rest := firstGOP(looped(t, "h264_aac.ts", 1))
	for _, f := range gop {
		e.h.Write(f)
	}
	const n = 50
	sessions := make([]session, n)
	chans := make([]<-chan result, n)
	for i := range chans {
		c := play(t, e.url, "tcp", nil)
		sessions[i] = sessionOf(c)
		chans[i] = collect(c, nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, e.h, rest, 4, nil)
	e.h.Close()
	first := wait(t, chans[0])
	sum := check(t, sessions[0], first.pkts, true)
	assert.Equal(t, 150, sum.videoAUs, "every video frame of the clip")
	want := normalise(sessions[0], first.pkts)
	ssrcs := map[uint32]bool{}
	for i := range n {
		r := first
		if i > 0 {
			r = wait(t, chans[i])
		}
		require.Equal(t, want, normalise(sessions[i], r.pkts), "player %d", i)
		for _, p := range r.pkts {
			if p.RTP != nil {
				ssrcs[p.RTP.SSRC] = true
			}
		}
	}
	assert.Len(t, ssrcs, 2*n, "an SSRC per track and session")
	st := e.srv.Stats()
	assert.Equal(t, uint64(n), st.PlayersStarted)
	assert.Zero(t, st.Resyncs)
	assert.Equal(t, uint64(n), st.EndedHubClosed)
}

// A stream is not playable before its first keyframe: DESCRIBE is answered
// 404, as for an unknown name.
func TestPlayNotLiveYet(t *testing.T) {
	e := start(t, Options{}, false)
	frames := looped(t, "h264_aac.ts", 1)
	mustNotPlay(t, e.url)
	var delta, audio media.Frame
	for _, f := range frames {
		if f.Kind() == media.KindVideo && !f.Key && delta.Config == nil {
			delta = f
		}
		if f.Kind() == media.KindAudio && audio.Config == nil {
			audio = f
		}
	}
	e.h.Write(delta)
	e.h.Write(audio)
	mustNotPlay(t, e.url)
	for _, f := range shift(frames, lastDTS(frames)+3600-frames[0].DTS, frames[0].Epoch) {
		e.h.Write(f)
	}
	play(t, e.url, "tcp", nil)
}

// describe sends a raw DESCRIBE for url and returns the response.
func describe(t *testing.T, addr, url string) *rtsp.Response {
	t.Helper()
	conn, err := new(net.Dialer).DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	req := &rtsp.Request{Method: "DESCRIBE", URL: url, Header: rtsp.Header{}}
	req.Header.Set("CSeq", "1")
	require.NoError(t, req.Write(conn))
	_ = conn.SetReadDeadline(time.Now().Add(waitFor))
	resp, err := rtsp.ReadResponse(bufio.NewReader(conn), rtsp.Limits{})
	require.NoError(t, err)
	return resp
}

// An unknown name, another app, a path that is not <app>/<name> and a
// stream that is not live yet all get the same 404.
func TestPlayUnknownStream(t *testing.T) {
	e := start(t, Options{}, false)
	notLive := describe(t, e.addr, e.url)
	require.Equal(t, 404, notLive.StatusCode)
	for _, path := range []string{"live/nope", "tv/s", "live", "s", "live/s/x", "live/live/s"} {
		r := describe(t, e.addr, "rtsp://"+e.addr+"/"+path)
		assert.Equal(t, notLive.StatusCode, r.StatusCode, path)
		assert.Equal(t, notLive.Reason, r.Reason, path)
		assert.Equal(t, notLive.Body, r.Body, path)
		assert.Equal(t, notLive.Header, r.Header, path)
	}
	assert.Equal(t, "Stream Not Found", notLive.Reason)
	gop, _ := firstGOP(looped(t, "h264_aac.ts", 1))
	for _, f := range gop {
		e.h.Write(f)
	}
	r := describe(t, e.addr, e.url+"/?q=1")
	assert.Equal(t, 200, r.StatusCode, "the path is normalised")
	assert.Equal(t, e.url+"/", r.Header.Get("Content-Base"))
}

// firstKey returns the index of the first video keyframe.
func firstKey(t *testing.T, frames []media.Frame) int {
	t.Helper()
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			return i
		}
	}
	t.Fatal("no keyframe")
	return -1
}

// A DESCRIBE right after the first keyframe, before the first audio frame,
// waits for the audio: the SDP lists both tracks and the session goes on
// when audio comes (a video-only SDP would end it).
func TestPlayDescribeWaitsForAudio(t *testing.T) {
	e := start(t, Options{}, false)
	frames := looped(t, "h264_aac.ts", 2)
	k := firstKey(t, frames)
	e.h.Write(frames[k])
	require.True(t, e.h.Live())
	require.Len(t, e.h.TrackConfigs(), 1, "video only so far")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fed := make(chan time.Duration, 1)
	go func() {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
		}
		fed <- <-feed(ctx, e.h, frames[k+1:], 2, nil)
	}()
	t0 := time.Now()
	c := play(t, e.url, "tcp", nil)
	waited := time.Since(t0)
	s := sessionOf(c)
	require.Len(t, s.desc.Media, 2, "the SDP waited for the audio")
	assert.GreaterOrEqual(t, waited, 150*time.Millisecond)
	assert.Less(t, waited, time.Second, "answered when the audio came, not at the end of the wait")
	r := wait(t, collect(c, videoAUs(s, 40)))
	require.NoError(t, r.err, "the session survives the audio")
	sum := check(t, s, r.pkts, true)
	assert.Positive(t, sum.audio)
	st := e.srv.Stats()
	assert.Equal(t, uint64(1), st.DescribeWaits, "only the DESCRIBE waited; the SETUP had audio")
	assert.Zero(t, st.DescribeWaitsTimedOut)
	assert.Zero(t, st.EndedCodecChange)
	assert.Equal(t, 1, st.Players)
	cancel()
	<-fed
}

// A video-only stream is described video-only after DescribeWait, and then
// without waiting again; a hub that closes during the wait is not found.
func TestPlayDescribeVideoOnly(t *testing.T) {
	const wait = 300 * time.Millisecond
	e := start(t, Options{DescribeWait: wait}, false)
	var video []media.Frame
	for _, f := range looped(t, "h264_aac.ts", 1) {
		if f.Kind() == media.KindVideo {
			video = append(video, f)
		}
	}
	gop, _ := firstGOP(video)
	for _, f := range gop {
		e.h.Write(f)
	}
	t0 := time.Now()
	r := describe(t, e.addr, e.url)
	took := time.Since(t0)
	require.Equal(t, 200, r.StatusCode)
	assert.GreaterOrEqual(t, took, wait)
	assert.Less(t, took, wait+500*time.Millisecond)

	t0 = time.Now()
	c := play(t, e.url, "tcp", nil) // DESCRIBE, SETUP, PLAY
	assert.Less(t, time.Since(t0), wait, "no second wait for the same stream")
	require.Len(t, sessionOf(c).desc.Media, 1)
	st := e.srv.Stats()
	assert.Equal(t, uint64(1), st.DescribeWaits)
	assert.Equal(t, uint64(1), st.DescribeWaitsTimedOut)

	e2 := start(t, Options{DescribeWait: waitFor}, false)
	for _, f := range gop {
		e2.h.Write(f)
	}
	time.AfterFunc(100*time.Millisecond, e2.h.Close)
	t0 = time.Now()
	r = describe(t, e2.addr, e2.url)
	assert.Equal(t, 404, r.StatusCode)
	assert.Less(t, time.Since(t0), waitFor/2, "the wait ends with the hub")
}

// SkippedTracks counts, once per session, the live tracks its SDP leaves
// out: here a second video track.
func TestPlaySkippedTracksOncePerSession(t *testing.T) {
	e := start(t, Options{}, false)
	gop, _ := firstGOP(looped(t, "h264_aac.ts", 1))
	for _, f := range gop {
		e.h.Write(f)
		if f.Kind() == media.KindVideo {
			f.Track = 2
			e.h.Write(f)
		}
	}
	require.Len(t, e.h.TrackConfigs(), 3)
	for range 2 {
		c := play(t, e.url, "tcp", nil)
		require.Len(t, sessionOf(c).desc.Media, 2)
	}
	waitStats(t, e, func(st Stats) bool { return st.PlayersStarted == 2 })
	assert.Equal(t, uint64(2), e.srv.Stats().SkippedTracks, "one per session, not per DESCRIBE and SETUP")
}

// An audio-only hub plays: the SDP lists audio only, the first packet is at
// the RTP-Info time, and every frame follows, the cached ones too.
func TestPlayAudioOnly(t *testing.T) {
	e := start(t, Options{}, false)
	var audio []media.Frame
	for _, f := range looped(t, "h264_aac.ts", 2) {
		if f.Kind() == media.KindAudio {
			f.Track = 0
			audio = append(audio, f)
		}
	}
	head, rest := audio[:20], audio[20:]
	for _, f := range head {
		e.h.Write(f)
	}
	c := play(t, e.url, "tcp", nil)
	s := sessionOf(c)
	require.Len(t, s.desc.Media, 1)
	assert.Equal(t, rtsp.CodecAAC, s.desc.Media[0].Codec)
	assert.Equal(t, uint32(48000), s.desc.Media[0].ClockRate)
	pkts := collect(c, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, e.h, rest, 8, nil)
	e.h.Close()
	r := wait(t, pkts)
	sum := check(t, s, r.pkts, true)
	assert.Zero(t, sum.videoAUs)
	assert.Equal(t, len(audio), sum.audio, "every audio frame, one packet each")
	assert.Zero(t, sum.seqGaps)
	require.NotNil(t, sum.firstAudio)
	assert.Equal(t, s.info[0].RTPTime, sum.firstAudio.Timestamp, "the first frame is b")
	assert.Equal(t, 1, sum.byes)
}

// tagUUID marks the SEI NAL units that TestPlayUDPJoinNoLoss adds to
// identify frames.
var tagUUID = []byte("rtspplay-tag-sei")

// tagged returns frames with an SEI user-data NAL unit carrying each video
// frame's index appended to its data.
func tagged(frames []media.Frame) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		if out[i].Kind() != media.KindVideo {
			continue
		}
		sei := []byte{6, 5, byte(len(tagUUID) + 8)}
		sei = append(sei, tagUUID...)
		sei = binary.BigEndian.AppendUint64(sei, uint64(i))
		sei = append(sei, 0x80)
		out[i].Data = append(append([][]byte(nil), out[i].Data...), sei)
	}
	return out
}

// tagOf returns the frame index an access unit carries.
func tagOf(au rtsp.AU) (int, bool) {
	for _, n := range au.NALUs {
		if len(n) == 3+len(tagUUID)+8+1 && n[0] == 6 && bytes.Equal(n[3:3+len(tagUUID)], tagUUID) {
			return int(binary.BigEndian.Uint64(n[3+len(tagUUID):])), true
		}
	}
	return 0, false
}

// tagIndexes returns the frame indexes the video access units of pkts
// carry, in arrival order, but for the last access unit.
func tagIndexes(t *testing.T, s session, pkts []rtsp.Packet) []int {
	t.Helper()
	v, ok := s.video()
	require.True(t, ok)
	dp, err := rtsp.NewDepacketizer(v, 0)
	require.NoError(t, err)
	var idx []int
	add := func(aus []rtsp.AU) {
		for _, au := range aus {
			require.False(t, au.Lost)
			i, ok := tagOf(au)
			require.True(t, ok, "every frame is tagged")
			idx = append(idx, i)
		}
	}
	for _, p := range pkts {
		if p.RTP != nil && p.Track == v.Index {
			add(dp.Push(p.RTP, p.Arrival))
		}
	}
	// The last access unit may be cut short: a reader stopped in it.
	return idx
}

// tagRuns checks the tagged video frames a player got: the first is a
// keyframe, and so is the first after every jump (a video frame of frames
// missing); it returns the first frame's index and the number of jumps.
func tagRuns(t *testing.T, s session, frames []media.Frame, pkts []rtsp.Packet) (first, jumps int) {
	t.Helper()
	idx := tagIndexes(t, s, pkts)
	require.NotEmpty(t, idx)
	require.True(t, frames[idx[0]].Key, "the first frame is a keyframe")
	for k := 1; k < len(idx); k++ {
		next := idx[k-1] + 1
		for next < len(frames) && frames[next].Kind() != media.KindVideo {
			next++
		}
		if idx[k] != next {
			require.True(t, frames[idx[k]].Key, "the first frame after a gap is a keyframe (frame %d after %d)", idx[k], idx[k-1])
			jumps++
		}
	}
	return idx[0], jumps
}

// waitMidGOP waits until the feed has written at least two keyframes and is
// 5 to 15 video frames past the last one, and returns the latest DTS
// written then. The hub's cache then holds a keyframe written before that
// DTS, which a replay sends first.
func waitMidGOP(t *testing.T, e *env, frames []media.Frame, written *atomic.Int64) int64 {
	t.Helper()
	var at int64
	require.Eventually(t, func() bool {
		at = written.Load()
		if at < 0 || !e.h.Live() {
			return false
		}
		keys, sinceKey := 0, 0
		for j := int64(0); j <= at; j++ {
			if f := frames[j]; f.Kind() == media.KindVideo {
				sinceKey++
				if f.Key {
					keys, sinceKey = keys+1, 0
				}
			}
		}
		return keys >= 2 && sinceKey >= 5 && sinceKey <= 15
	}, waitFor, time.Millisecond)
	return lastDTS(frames[:at+1])
}

// A UDP player that joins mid-GOP gets no replay burst: its first frame is
// a keyframe written after PLAY, and from it on every packet arrives, with
// no sequence gap and every video frame in order.
func TestPlayUDPJoinNoLoss(t *testing.T) {
	e := start(t, Options{}, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := tagged(looped(t, "h264_aac.ts", 4))
	var written atomic.Int64
	written.Store(-1)
	done := feed(ctx, e.h, frames, 2, &written)
	latest := waitMidGOP(t, e, frames, &written)

	c := play(t, e.url, "udp", nil)
	s := sessionOf(c)
	r := wait(t, collect(c, videoAUs(s, 60)))
	require.NoError(t, r.err)
	sum := check(t, s, r.pkts, false)
	assert.Zero(t, sum.seqGaps, "every packet from the first on")
	assert.Zero(t, sum.resumes)
	assert.Zero(t, c.Stats().Dropped)

	idx := tagIndexes(t, s, r.pkts)
	require.NotEmpty(t, idx)
	first := frames[idx[0]]
	assert.True(t, first.Key, "starts at a keyframe")
	assert.Greater(t, first.DTS, latest, "no replay: the first frame was written after PLAY")
	var want []int
	for j := idx[0]; len(want) < len(idx); j++ {
		if frames[j].Kind() == media.KindVideo {
			want = append(want, j)
		}
	}
	assert.Equal(t, want, idx, "every video frame from the first, in decode order")
	cancel()
	<-done
}

// ffprobe sees H.264 and AAC with the fixture's dimensions over TCP and
// UDP.
func TestPlayFFprobe(t *testing.T) {
	ffprobe := testpeer.Require(t, "ffprobe")
	e := start(t, Options{}, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := feed(ctx, e.h, looped(t, "h264_aac.ts", 4), 2, nil)
	require.Eventually(t, func() bool { return len(e.h.TrackConfigs()) == 2 }, waitFor, time.Millisecond)
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
			defer pcancel()
			var stderr bytes.Buffer
			cmd := exec.CommandContext(pctx, ffprobe, "-v", "error", "-rtsp_transport", transport, "-show_entries",
				"stream=codec_name,codec_type,width,height,sample_rate", "-of", "json", e.url)
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			require.NoError(t, err, "%s", stderr.String())
			var probe struct {
				Streams []struct {
					CodecName  string `json:"codec_name"`
					CodecType  string `json:"codec_type"`
					Width      int    `json:"width"`
					Height     int    `json:"height"`
					SampleRate string `json:"sample_rate"`
				} `json:"streams"`
			}
			require.NoError(t, json.Unmarshal(out, &probe))
			got := map[string]string{}
			for _, s := range probe.Streams {
				switch s.CodecType {
				case "video":
					got["video"] = fmt.Sprintf("%s %dx%d", s.CodecName, s.Width, s.Height)
				case "audio":
					got["audio"] = s.CodecName + " " + s.SampleRate
				}
			}
			assert.Equal(t, map[string]string{"video": "h264 320x240", "audio": "aac 48000"}, got)
		})
	}
	cancel()
	<-done
}

// ffmpeg decodes 5 s of H.264 or H.265 with AAC over TCP and UDP without a
// single error.
func TestPlayFFmpegDecodes(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg")
	for _, fixture := range []string{"h264_aac.ts", "h265_aac51.ts"} {
		frames := looped(t, fixture, 3)
		for _, transport := range []string{"tcp", "udp"} {
			t.Run(fixture+"/"+transport, func(t *testing.T) {
				t.Parallel()
				e := start(t, Options{}, true)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := feed(ctx, e.h, frames, 1.5, nil)
				require.Eventually(t, func() bool { return len(e.h.TrackConfigs()) == 2 }, waitFor, time.Millisecond)
				pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
				defer pcancel()
				var stderr bytes.Buffer
				cmd := exec.CommandContext(pctx, ffmpeg, "-nostdin", "-xerror", "-v", "error",
					"-rtsp_transport", transport, "-i", e.url, "-t", "5", "-f", "null", "-")
				cmd.Stderr = &stderr
				require.NoError(t, cmd.Run(), "ffmpeg: %s", stderr.String())
				assert.Empty(t, stderr.String())
				cancel()
				<-done
			})
		}
	}
}

// renumbered returns frames with their tracks moved up by d.
func renumbered(frames []media.Frame, d int) []media.Frame {
	out := append([]media.Frame(nil), frames...)
	for i := range out {
		out[i].Track += d
	}
	return out
}

// A session whose carried tracks deliver nothing for hub.LiveWindow of
// stream time while the hub goes on delivering other tracks (the tracks
// were renumbered at a failover) ends with a BYE per track; players then
// DESCRIBE the new tracks.
func TestPlayCarriedTracksGone(t *testing.T) {
	e := start(t, Options{}, false)
	avc := looped(t, "h264_aac.ts", 1)
	gop, rest := firstGOP(avc)
	for _, f := range gop {
		e.h.Write(f)
	}
	c := play(t, e.url, "tcp", nil)
	s := sessionOf(c)
	pkts := collect(c, nil)
	next := looped(t, "h264_aac.ts", 3)
	next = renumbered(shift(next, lastDTS(avc)+3600-next[0].DTS, avc[len(avc)-1].Epoch+1), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := feed(ctx, e.h, append(rest, next...), 16, nil)
	r := wait(t, pkts)
	require.Error(t, r.err)
	sum := check(t, s, r.pkts, true)
	assert.Equal(t, 150, sum.videoAUs, "every frame of the carried tracks")
	assert.Equal(t, 2, sum.byes)
	require.Eventually(t, func() bool { return e.srv.Stats().EndedTracksGone == 1 }, waitFor, time.Millisecond)
	assert.Positive(t, e.srv.Stats().Mux.IgnoredFrames, "the new tracks were ignored until then")

	// A new player gets the new tracks.
	c2 := play(t, e.url, "tcp", nil)
	r2 := wait(t, collect(c2, videoAUs(sessionOf(c2), 5)))
	require.NoError(t, r2.err)
	cancel()
	<-done
}

// rawConn is a raw RTSP connection with a playing session.
type rawConn struct {
	net.Conn
	br   *bufio.Reader
	sess string
	cseq int
}

// request sends a request and returns its response, skipping interleaved
// frames before it.
func (c *rawConn) request(t *testing.T, method, u string, h rtsp.Header) *rtsp.Response {
	t.Helper()
	c.cseq++
	req := &rtsp.Request{Method: method, URL: u, Header: h}
	req.Header.Set("CSeq", strconv.Itoa(c.cseq))
	require.NoError(t, req.Write(c))
	_ = c.SetReadDeadline(time.Now().Add(waitFor))
	for {
		b, err := c.br.Peek(4)
		require.NoError(t, err)
		if b[0] != '$' {
			break
		}
		_, err = c.br.Discard(4 + int(binary.BigEndian.Uint16(b[2:])))
		require.NoError(t, err)
	}
	resp, err := rtsp.ReadResponse(c.br, rtsp.Limits{})
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, method)
	return resp
}

// rawSetup opens a raw RTSP connection with dial, SETUPs track 0 of url with
// transport and PLAYs it; it returns the connection after the PLAY
// response.
func rawSetup(t *testing.T, addr, url, transport string, dial func(context.Context, string, string) (net.Conn, error)) *rawConn {
	t.Helper()
	if dial == nil {
		dial = new(net.Dialer).DialContext
	}
	nc, err := dial(context.Background(), "tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = nc.Close() })
	c := &rawConn{Conn: nc, br: bufio.NewReader(nc)}
	resp := c.request(t, "SETUP", url+"/trackID=0", rtsp.Header{"Transport": {transport}})
	sess, err := rtsp.ParseSession(resp.Header.Get("Session"))
	require.NoError(t, err)
	c.sess = sess.ID
	c.request(t, "PLAY", url, rtsp.Header{"Session": {sess.ID}})
	return c
}

// waitStats waits until cond holds for the server's counters and reports
// them when it never does.
func waitStats(t *testing.T, e *env, cond func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond(e.srv.Stats()) {
		if time.Now().After(deadline) {
			t.Fatalf("counters never matched: %+v", e.srv.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}

// Each way a session ends is counted: TEARDOWN, the client closing its
// connection, a UDP client gone silent and the server shutting down.
func TestPlayEndReasons(t *testing.T) {
	e := start(t, Options{Server: rtsp.ServerConfig{SessionTimeout: 300 * time.Millisecond}}, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := feed(ctx, e.h, looped(t, "h264_aac.ts", 4), 1, nil)
	require.Eventually(t, e.h.Live, waitFor, time.Millisecond)

	rc := rawSetup(t, e.addr, e.url, "RTP/AVP/TCP;unicast;interleaved=0-1", nil)
	waitStats(t, e, func(st Stats) bool { return st.Packets > 0 })
	rc.request(t, "TEARDOWN", e.url, rtsp.Header{"Session": {rc.sess}})
	waitStats(t, e, func(st Stats) bool { return st.EndedTeardown == 1 })

	rc = rawSetup(t, e.addr, e.url, "RTP/AVP/TCP;unicast;interleaved=0-1", nil)
	go func() { _, _ = io.Copy(io.Discard, rc) }()
	waitStats(t, e, func(st Stats) bool { return st.Players == 1 })
	_ = rc.Close()
	waitStats(t, e, func(st Stats) bool { return st.EndedLeft == 1 })

	a, b, err := rtsp.ListenUDPPair("127.0.0.1")
	require.NoError(t, err)
	defer a.Close()
	defer b.Close()
	ports := fmt.Sprintf("%d-%d", a.LocalAddr().(*net.UDPAddr).Port, b.LocalAddr().(*net.UDPAddr).Port)
	rawSetup(t, e.addr, e.url, "RTP/AVP;unicast;client_port="+ports, nil) // never sends RTCP
	waitStats(t, e, func(st Stats) bool { return st.EndedTimeout == 1 })

	play(t, e.url, "tcp", nil)
	waitStats(t, e, func(st Stats) bool { return st.Players == 1 })
	require.NoError(t, e.srv.Close())
	st := e.srv.Stats()
	assert.Equal(t, uint64(1), st.EndedShutdown)
	assert.Equal(t, 0, st.Players)
	assert.Equal(t, uint64(4), st.PlayersStarted)
	assert.Zero(t, st.EndedWriteError+st.EndedDeadline)
	assert.Empty(t, e.h.Stats(), "every player unsubscribed")
	cancel()
	<-done
}

// At the end of the DESCRIBE wait the track configs are read once more:
// audio that came after the last poll is described, and the stream is
// neither counted as timed out nor remembered as audio-less.
func TestWaitAudioFinalRead(t *testing.T) {
	s := New(func(string) (*hub.Hub, bool) { return nil, false }, Options{DescribeWait: time.Millisecond})
	defer func() { require.NoError(t, s.Close()) }()
	h := hub.New(hub.Options{})
	defer h.Close()
	frames := looped(t, "h264_aac.ts", 1)
	k := firstKey(t, frames)
	h.Write(frames[k])
	stale := h.TrackConfigs()
	require.False(t, has(stale, media.KindAudio))
	for _, f := range frames[k+1:] {
		if f.Kind() == media.KindAudio && f.DTS >= frames[k].DTS {
			h.Write(f)
			break
		}
	}
	require.True(t, has(h.TrackConfigs(), media.KindAudio))

	got := s.waitAudio(h, stale) // the deadline (1 ms) comes before the first poll (10 ms)
	assert.True(t, has(got, media.KindAudio), "the final read's audio is described")
	st := s.Stats()
	assert.Equal(t, uint64(1), st.DescribeWaits)
	assert.Zero(t, st.DescribeWaitsTimedOut, "audio came: the wait did not time out")
	assert.False(t, s.recentlyNoAudio(h), "a stream with audio is not remembered as audio-less")
}

// A session freezes the SDP its player was described, not the stream's
// tracks at SETUP: a player described video-only (audio came after the
// DESCRIBE wait) that SETUPs once audio is live ends with a BYE when audio
// reaches it, instead of ignoring the audio, and DESCRIBEs again.
func TestPlaySetupFreezesDescribe(t *testing.T) {
	e := start(t, Options{DescribeWait: 100 * time.Millisecond}, false)
	gop, rest := firstGOP(looped(t, "h264_aac.ts", 2))
	for _, f := range gop {
		if f.Kind() == media.KindVideo {
			e.h.Write(f)
		}
	}
	nc, err := new(net.Dialer).DialContext(context.Background(), "tcp", e.addr)
	require.NoError(t, err)
	defer nc.Close()
	c := &rawConn{Conn: nc, br: bufio.NewReader(nc)}
	resp := c.request(t, "DESCRIBE", e.url, rtsp.Header{})
	assert.Equal(t, 1, bytes.Count(resp.Body, []byte("m=")), "described video-only: %s", resp.Body)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := feed(ctx, e.h, rest, 4, nil)
	require.Eventually(t, func() bool { return has(e.h.TrackConfigs(), media.KindAudio) }, waitFor, time.Millisecond)
	resp = c.request(t, "SETUP", e.url+"/trackID=0", rtsp.Header{"Transport": {"RTP/AVP/TCP;unicast;interleaved=0-1"}})
	sess, err := rtsp.ParseSession(resp.Header.Get("Session"))
	require.NoError(t, err)
	c.request(t, "PLAY", e.url, rtsp.Header{"Session": {sess.ID}})

	bye := false
	_ = c.SetReadDeadline(time.Now().Add(waitFor))
	for !bye {
		var hdr [4]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			break
		}
		require.Equal(t, byte('$'), hdr[0])
		b := make([]byte, binary.BigEndian.Uint16(hdr[2:]))
		_, err := io.ReadFull(c.br, b)
		require.NoError(t, err)
		bye = hdr[1] == 1 && bytes.IndexByte(rtcpTypes(b), rtcpBye) >= 0
	}
	require.True(t, bye, "the audio ends the session with a BYE")
	waitStats(t, e, func(st Stats) bool { return st.EndedCodecChange == 1 })
	assert.Zero(t, e.srv.Stats().Mux.IgnoredFrames, "no audio ignored")
	cancel()
	<-done
}
