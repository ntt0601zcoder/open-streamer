package rtmpplay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
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

// env is a Server for one hub, stream "s", on its own registry and port.
type env struct {
	h   *hub.Hub
	srv *Server
	reg *rtmpsrv.Registry
	url string
}

// start serves a fresh hub as live/s. hook, when set, sees every accepted
// connection.
func start(t *testing.T, opt Options, hook func(net.Conn)) *env {
	t.Helper()
	h := hub.New(hub.Options{})
	srv := New(func(name string) (*hub.Hub, bool) {
		if name == "s" {
			return h, true
		}
		return nil, false
	}, opt)
	reg := &rtmpsrv.Registry{ConnHook: hook}
	addr := freeAddr(t)
	unregister, err := reg.RegisterPlay(addr, "live", srv, rtmp.Config{}, rtmpsrv.Policy{})
	require.NoError(t, err)
	t.Cleanup(unregister)
	t.Cleanup(h.Close)
	return &env{h: h, srv: srv, reg: reg, url: "rtmp://" + addr + "/live/s"}
}

func play(t *testing.T, url string, cfg rtmp.Config) *rtmp.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	c, err := rtmp.PlayWith(ctx, url, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustNotPlay(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
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

// result is what one player read: its messages and the error that ended it.
type result struct {
	msgs []rtmp.Media
	err  error
}

// collect reads c in a goroutine until it ends, or until stop reports true
// for the messages read so far.
func collect(c *rtmp.ClientConn, stop func([]rtmp.Media) bool) <-chan result {
	out := make(chan result, 1)
	go func() {
		var r result
		for {
			m, err := c.ReadMedia()
			if err != nil {
				r.err = err
				break
			}
			r.msgs = append(r.msgs, m)
			if stop != nil && stop(r.msgs) {
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

// videoFrames counts the coded video messages.
func videoFrames(n int) func([]rtmp.Media) bool {
	return func(msgs []rtmp.Media) bool {
		k := 0
		for _, m := range msgs {
			if m.Type == rtmp.MsgVideo && len(m.Payload) > 1 && m.Payload[1] == 1 {
				k++
			}
		}
		return k >= n
	}
}

// summary describes a checked playback.
type summary struct {
	metas, video, audio, keys int
	gaps                      int // video timestamp gaps, each at a resume
	firstVideoTS              uint32
	video0                    bool // a video frame was seen
}

// maxVideoStep is the largest video timestamp step within a run: the
// fixtures are 25 fps.
const maxVideoStep = 200

// check verifies a player's messages: they start with onMetaData; each
// carried track's sequence header comes after every onMetaData before the
// track's next frame; on a connection with video, the first video frame
// after every onMetaData is a keyframe and audio before it, or presented
// before it, is never sent; timestamps never decrease per track, and a
// video timestamp gap (lost frames) is only ever a resume after onMetaData.
// Data messages other than onMetaData (|RtmpSampleAccess) are skipped.
func check(t *testing.T, msgs []rtmp.Media) summary {
	t.Helper()
	var (
		s                    summary
		withVideo            bool
		vHdr, aHdr, needKey  bool
		resumed              bool
		keyTS                int64
		prevVideo, prevAudio int64 = -1, -1
	)
	for i, m := range msgs {
		switch m.Type {
		case rtmp.MsgDataAMF0:
			vals, err := rtmp.DecodeAMF0(m.Payload)
			require.NoError(t, err, "message %d", i)
			if len(vals) == 0 || vals[0] != "onMetaData" {
				continue
			}
			require.Len(t, vals, 2)
			arr, ok := vals[1].(rtmp.ECMAArray)
			require.True(t, ok)
			_, withVideo = lookup(arr, "videocodecid")
			_, withAudio := lookup(arr, "audiocodecid")
			require.True(t, withVideo || withAudio, "onMetaData lists a track")
			s.metas++
			vHdr, aHdr, needKey, resumed = false, false, withVideo, true
		case rtmp.MsgVideo:
			require.Positive(t, s.metas, "video before onMetaData")
			require.True(t, withVideo, "video on a connection without video")
			tag, err := rtmp.ParseVideoTag(m.Payload)
			require.NoError(t, err, "message %d", i)
			if tag.Kind == rtmp.VideoSequenceStart {
				vHdr = true
				continue
			}
			require.Equal(t, rtmp.VideoCodedFrames, tag.Kind)
			require.True(t, vHdr, "video frame %d before its header", i)
			at := int64(m.Timestamp)
			if needKey {
				require.True(t, tag.Key, "first video frame after onMetaData is a keyframe (message %d)", i)
				needKey, keyTS = false, at+int64(tag.CompTime)
			}
			if prevVideo >= 0 {
				require.GreaterOrEqual(t, at, prevVideo, "video timestamps never decrease (message %d)", i)
				if at-prevVideo > maxVideoStep {
					require.True(t, resumed && tag.Key, "a gap only at a resume keyframe (message %d)", i)
					s.gaps++
				}
			}
			if !s.video0 {
				s.video0, s.firstVideoTS = true, m.Timestamp
			}
			resumed = false
			prevVideo = at
			s.video++
			if tag.Key {
				s.keys++
			}
		case rtmp.MsgAudio:
			require.Positive(t, s.metas, "audio before onMetaData")
			tag, err := rtmp.ParseAudioTag(m.Payload)
			require.NoError(t, err, "message %d", i)
			if tag.SequenceHeader {
				aHdr = true
				continue
			}
			require.True(t, aHdr, "audio frame %d before its header", i)
			at := int64(m.Timestamp)
			if withVideo {
				require.False(t, needKey, "audio before the resume keyframe (message %d)", i)
				require.GreaterOrEqual(t, at, keyTS, "audio presented before the resume keyframe (message %d)", i)
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

// metadata returns the first onMetaData of msgs.
func metadata(t *testing.T, msgs []rtmp.Media) rtmp.ECMAArray {
	t.Helper()
	for _, m := range msgs {
		if m.Type != rtmp.MsgDataAMF0 {
			continue
		}
		vals, err := rtmp.DecodeAMF0(m.Payload)
		require.NoError(t, err)
		if len(vals) == 2 && vals[0] == "onMetaData" {
			arr, ok := vals[1].(rtmp.ECMAArray)
			require.True(t, ok)
			return arr
		}
	}
	t.Fatal("no onMetaData")
	return nil
}

// A player joining mid-GOP starts at a cached keyframe, with onMetaData and
// headers first, timestamps from 0 and no audio presented before the key.
func TestPlayJoinsAtKeyframe(t *testing.T) {
	e := start(t, Options{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := looped(t, "h264_aac.ts", 4)
	done := feed(ctx, e.h, frames, 2)
	require.Eventually(t, func() bool { return e.srv.Live("s") }, waitFor, time.Millisecond)
	time.Sleep(1300 * time.Millisecond) // into the stream, mid-GOP

	c := play(t, e.url, rtmp.Config{})
	r := wait(t, collect(c, videoFrames(60)))
	require.NoError(t, r.err)
	s := check(t, r.msgs)
	assert.Equal(t, 1, s.metas)
	assert.Equal(t, uint32(0), s.firstVideoTS, "the connection starts at its first keyframe")
	assert.GreaterOrEqual(t, s.keys, 2)
	assert.Positive(t, s.audio)

	meta := metadata(t, r.msgs)
	w, _ := lookup(meta, "width")
	h, _ := lookup(meta, "height")
	assert.InDelta(t, 320, w, 0)
	assert.InDelta(t, 240, h, 0)
	cancel()
	<-done
	assert.Equal(t, uint64(1), e.srv.Stats().PlayersStarted)
}

// A codec change on a carried track ends the player with end of stream.
func TestPlayCodecChangeEndsSession(t *testing.T) {
	e := start(t, Options{}, nil)
	avc := looped(t, "h264_aac.ts", 1)
	hevc := looped(t, "h265_aac51.ts", 1)
	hevc = shift(hevc, lastDTS(avc)+3600-hevc[0].DTS, avc[len(avc)-1].Epoch+1)
	gop, rest := firstGOP(avc)
	for _, f := range gop {
		e.h.Write(f)
	}
	c := play(t, e.url, rtmp.Config{})
	msgs := collect(c, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, e.h, append(rest, hevc...), 8)
	r := wait(t, msgs)
	require.ErrorIs(t, r.err, io.EOF, "end of stream")
	s := check(t, r.msgs)
	assert.Positive(t, s.video)
	for _, m := range r.msgs {
		if m.Type == rtmp.MsgVideo {
			tag, err := rtmp.ParseVideoTag(m.Payload)
			require.NoError(t, err)
			require.Equal(t, rtmp.VideoAVC, tag.Codec, "no H.265 reaches the player")
		}
	}
	require.Eventually(t, func() bool { return e.srv.Stats().EndedCodecChange == 1 }, waitFor, time.Millisecond)
	assert.Empty(t, e.h.Stats(), "unsubscribed")
}

// Closing the hub ends every player with end of stream and unsubscribes it.
func TestPlayEndOfStream(t *testing.T) {
	e := start(t, Options{}, nil)
	frames := looped(t, "h264_aac.ts", 1)
	gop, _ := firstGOP(frames)
	for _, f := range gop {
		e.h.Write(f)
	}
	c := play(t, e.url, rtmp.Config{})
	msgs := collect(c, nil)
	require.Eventually(t, func() bool { return len(e.h.Stats()) == 1 }, waitFor, time.Millisecond)
	e.h.Close()
	r := wait(t, msgs)
	require.ErrorIs(t, r.err, io.EOF)
	s := check(t, r.msgs)
	assert.Positive(t, s.video, "the cached GOP was played")
	require.Eventually(t, func() bool { return e.srv.Stats().EndedHubClosed == 1 }, waitFor, time.Millisecond)
	assert.Equal(t, 0, e.srv.Stats().Players)
	assert.False(t, e.srv.Live("s"), "a closed hub is not live")
}

// Unregistering the handler ends Serve and unsubscribes its players, each
// counted as ended by the shutdown.
func TestPlayUnregisterEndsPlayers(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := New(func(string) (*hub.Hub, bool) { return h, true }, Options{})
	reg := &rtmpsrv.Registry{}
	addr := freeAddr(t)
	unregister, err := reg.RegisterPlay(addr, "live", srv, rtmp.Config{}, rtmpsrv.Policy{})
	require.NoError(t, err)
	defer unregister()
	gop, _ := firstGOP(looped(t, "h264_aac.ts", 1))
	for _, f := range gop {
		h.Write(f)
	}
	const n = 8
	chans := make([]<-chan result, n)
	for i := range chans {
		chans[i] = collect(play(t, "rtmp://"+addr+"/live/any", rtmp.Config{}), nil)
	}
	require.Eventually(t, func() bool { return len(h.Stats()) == n }, waitFor, time.Millisecond)
	unregister()
	assert.Empty(t, h.Stats())
	st := srv.Stats()
	assert.Equal(t, 0, st.Players)
	assert.Equal(t, uint64(n), st.EndedShutdown, "a shutdown is no player leaving")
	assert.Zero(t, st.EndedLeft)
	for _, ch := range chans {
		require.Error(t, wait(t, ch).err)
	}
}

// Fifty players joining one after the other all get the same messages.
func TestPlayManyPlayers(t *testing.T) {
	// A queue that holds the whole clip: fifty players under the race
	// detector on a loaded machine can fall behind a feed at 4x, and frames
	// the hub drops at the end of the clip are never reported as lost.
	const queue = 1024
	e := start(t, Options{Queue: queue}, nil)
	frames := looped(t, "h264_aac.ts", 1)
	require.Less(t, len(frames), queue, "the clip fits a player's queue")
	gop, rest := firstGOP(frames)
	for _, f := range gop {
		e.h.Write(f)
	}
	const n = 50
	chans := make([]<-chan result, n)
	for i := range chans {
		chans[i] = collect(play(t, e.url, rtmp.Config{}), nil)
	}
	// A player subscribes in the server's goroutine after its play is
	// answered: feeding before would lose a late one the cached GOP.
	require.Eventually(t, func() bool { return len(e.h.Stats()) == n }, waitFor, time.Millisecond, "every player subscribed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, e.h, rest, 4)
	e.h.Close()
	first := wait(t, chans[0])
	require.ErrorIs(t, first.err, io.EOF)
	s := check(t, first.msgs)
	assert.Equal(t, 1, s.metas)
	assert.Equal(t, 150, s.video, "every video frame of the clip")
	for i := 1; i < n; i++ {
		r := wait(t, chans[i])
		require.ErrorIs(t, r.err, io.EOF, "player %d", i)
		require.Equal(t, first.msgs, r.msgs, "player %d", i)
	}
	st := e.srv.Stats()
	assert.Equal(t, uint64(n), st.PlayersStarted)
	assert.Equal(t, uint64(0), st.Resyncs)
}

// A stream is not playable before its first keyframe: the player gets
// StreamNotFound, the same as for an unknown name.
func TestPlayNotLiveYet(t *testing.T) {
	e := start(t, Options{}, nil)
	frames := looped(t, "h264_aac.ts", 1)
	mustNotPlay(t, e.url)
	assert.False(t, e.srv.Live("s"))

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
	assert.False(t, e.srv.Live("s"))

	for _, f := range shift(frames, lastDTS(frames)+3600-frames[0].DTS, frames[0].Epoch) {
		e.h.Write(f)
	}
	assert.True(t, e.srv.Live("s"))
	play(t, e.url, rtmp.Config{})
	mustNotPlay(t, e.url[:len(e.url)-1]+"nope")
	assert.False(t, e.srv.Live("nope"))
}

func requireFFprobe(t *testing.T) string {
	t.Helper()
	return testpeer.Require(t, "ffprobe", "rtmp")
}

// ffprobe plays the stream and sees H.264 and AAC with the fixture's
// dimensions.
func TestPlayFFprobe(t *testing.T) {
	ffprobe := requireFFprobe(t)
	e := start(t, Options{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := feed(ctx, e.h, looped(t, "h264_aac.ts", 4), 2)
	require.Eventually(t, func() bool { return e.srv.Live("s") }, waitFor, time.Millisecond)

	pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
	defer pcancel()
	out, err := exec.CommandContext(pctx, ffprobe, "-v", "error", "-show_entries",
		"stream=codec_name,codec_type,width,height,sample_rate", "-of", "json", e.url).Output()
	require.NoError(t, err, "%s", out)
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
	cancel()
	<-done
}

// An audio-only hub plays: onMetaData lists audio only, the AAC header comes
// first, and every frame follows from timestamp 0.
func TestPlayAudioOnly(t *testing.T) {
	e := start(t, Options{}, nil)
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
	require.True(t, e.srv.Live("s"))
	c := play(t, e.url, rtmp.Config{})
	msgs := collect(c, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-feed(ctx, e.h, rest, 8)
	e.h.Close()
	r := wait(t, msgs)
	require.ErrorIs(t, r.err, io.EOF)
	s := check(t, r.msgs)
	assert.Equal(t, 1, s.metas)
	assert.Zero(t, s.video)
	assert.Equal(t, len(audio), s.audio, "every audio frame, the cached ones too")
	meta := metadata(t, r.msgs)
	_, hasVideo := lookup(meta, "videocodecid")
	assert.False(t, hasVideo)
	id, _ := lookup(meta, "audiocodecid")
	assert.InDelta(t, 10, id, 0)
	for _, m := range r.msgs {
		if m.Type == rtmp.MsgAudio {
			assert.Equal(t, uint32(0), m.Timestamp, "first audio at 0")
			break
		}
	}
}
