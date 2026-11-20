package e2e

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmpplay"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// The M3b1 scenarios run fixtures through a pipeline into a hub and out of
// the RTMP outputs: the play output, read by conform.ReadRTMP, ffprobe,
// ffmpeg and MediaMTX (pulling it for its RTSP readers), and the push-out to
// MediaMTX, read back with our RTMP pull input. conform.ReadRTMP reads only
// our own output: MediaMTX's RTMP output may resend headers or relay H.265
// in the legacy form, which the strict RTMP rules would flag, so what passed
// through MediaMTX is read with our inputs and checked as frames.

const (
	// m3b1App is the play app of every scenario: players read live/<name>.
	m3b1App = "live"
	// m3b1Read bounds the RTMP set-up and every wait for the next message:
	// it covers the muxer's start hold (at most 1 s), a failover's gap and a
	// throttled player's pause.
	m3b1Read = 15 * time.Second
	// m3b1PullAddr is the play address that the RTSP MediaMTX pulls its
	// paths m3b1pull and m3b1pullhevc from (CI's step and the local
	// container set MTX_PATHS_M3B1PULL*_SOURCE to it).
	m3b1PullAddr = "127.0.0.1:19361"
	// m3b1MTXReady bounds the wait for a read-back of MediaMTX to deliver:
	// MediaMTX makes a path ready 2 to 2.5 s after its publisher connects
	// (mtxReady), more when the whole suite loads the machine.
	m3b1MTXReady = 10 * time.Second
)

// playOut is an RTMP play output on its own registry and listener.
type playOut struct {
	srv  *rtmpplay.Server
	reg  *rtmpsrv.Registry
	addr string
}

// servePlay serves hubs by stream name on addr (a free port when empty).
// hook, when set, sees every accepted connection.
func servePlay(t *testing.T, addr string, opt rtmpplay.Options, hook func(net.Conn), hubs map[string]*hub.Hub) *playOut {
	t.Helper()
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	}
	srv := rtmpplay.New(func(name string) (*hub.Hub, bool) {
		h, ok := hubs[name]
		return h, ok
	}, opt)
	reg := &rtmpsrv.Registry{ConnHook: hook}
	unregister, err := reg.RegisterPlay(addr, m3b1App, srv, rtmp.Config{}, rtmpsrv.Policy{})
	require.NoError(t, err)
	t.Cleanup(unregister)
	return &playOut{srv: srv, reg: reg, addr: addr}
}

// url is the play URL of the stream s.
func (p *playOut) url() string { return "rtmp://" + p.addr + "/" + m3b1App + "/s" }

// waitLive waits until the stream can be played.
func waitLive(t *testing.T, srv *rtmpplay.Server, name string) {
	t.Helper()
	require.Eventually(t, func() bool { return srv.Live(name) }, 15*time.Second, 5*time.Millisecond,
		"stream %s live", name)
}

// capResult is what one conform.ReadRTMP returned.
type capResult struct {
	c   conform.Capture
	err error
}

// readPlay reads u with conform.ReadRTMP until the end of the stream, in the
// background.
func readPlay(u string) <-chan capResult {
	ch := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c, err := conform.ReadRTMP(ctx, u, conform.ReadOptions{Timeout: m3b1Read})
		ch <- capResult{c, err}
	}()
	return ch
}

func waitCapture(t *testing.T, ch <-chan capResult) conform.Capture {
	t.Helper()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.c
	case <-time.After(runLimit + m3b1Read):
		t.Fatal("the RTMP read did not end")
		return conform.Capture{}
	}
}

// waitRaw returns the messages of a rawPlay, failing the test if the read
// does not end within the run limit plus m3b1Read.
func waitRaw(t *testing.T, ch <-chan []rtmp.Media) []rtmp.Media {
	t.Helper()
	select {
	case msgs := <-ch:
		return msgs
	case <-time.After(runLimit + m3b1Read):
		t.Fatal("the raw RTMP read did not end")
		return nil
	}
}

// rawPlay reads every message of u until the stream ends, in the
// background.
func rawPlay(u string) <-chan []rtmp.Media {
	ch := make(chan []rtmp.Media, 1)
	go func() {
		var msgs []rtmp.Media
		defer func() { ch <- msgs }()
		ctx, cancel := context.WithTimeout(context.Background(), m3b1Read)
		c, err := rtmp.PlayWith(ctx, u, rtmp.Config{HandshakeTimeout: m3b1Read, CommandTimeout: m3b1Read})
		cancel()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			m, err := c.ReadMedia()
			if err != nil {
				return
			}
			msgs = append(msgs, m)
		}
	}()
	return ch
}

// msgCounts counts what a player received: onMetaData messages (with the
// width each lists), sequence headers and coded frames per kind.
type msgCounts struct {
	metas, videoHeaders, audioHeaders, video, audio int
	widths                                          []any
}

func countMsgs(t *testing.T, msgs []rtmp.Media) msgCounts {
	t.Helper()
	var n msgCounts
	for i, m := range msgs {
		switch m.Type {
		case rtmp.MsgDataAMF0:
			vals, err := rtmp.DecodeAMF0(m.Payload)
			require.NoError(t, err, "message %d", i)
			if len(vals) != 2 || vals[0] != "onMetaData" {
				continue
			}
			n.metas++
			arr, ok := vals[1].(rtmp.ECMAArray)
			require.True(t, ok, "message %d", i)
			for _, p := range arr {
				if p.Key == "width" {
					n.widths = append(n.widths, p.Value)
				}
			}
		case rtmp.MsgVideo:
			tag, err := rtmp.ParseVideoTag(m.Payload)
			require.NoError(t, err, "message %d", i)
			switch tag.Kind {
			case rtmp.VideoSequenceStart:
				n.videoHeaders++
			case rtmp.VideoCodedFrames:
				n.video++
			case rtmp.VideoSequenceEnd, rtmp.VideoMetadata, rtmp.VideoCommand:
			}
		case rtmp.MsgAudio:
			tag, err := rtmp.ParseAudioTag(m.Payload)
			require.NoError(t, err, "message %d", i)
			if tag.SequenceHeader {
				n.audioHeaders++
			} else {
				n.audio++
			}
		}
	}
	return n
}

// floorMS is a span of 90 kHz ticks in ms, rounded down.
func floorMS(ticks int64) int64 {
	q := ticks / 90
	if ticks%90 != 0 && ticks < 0 {
		q--
	}
	return q
}

// byKind returns the frames of kind k with their indexes in frames.
func byKind(frames []media.Frame, k media.Kind) (out []media.Frame, at []int) {
	for i, f := range frames {
		if f.Kind() == k {
			out = append(out, f)
			at = append(at, i)
		}
	}
	return out, at
}

// joinKey returns the index, among the hub's video frames hv, of the
// keyframe a connection that received n video frames up to the hub's end
// started at.
func joinKey(t *testing.T, hv []media.Frame, n int) int {
	t.Helper()
	k := len(hv) - n
	require.GreaterOrEqual(t, k, 0, "%d video frames received, the hub has %d", n, len(hv))
	require.True(t, hv[k].Key, "the connection starts at hub video frame %d, a keyframe", k)
	return k
}

// assertMatchesHub checks that a player that never lost a frame received
// exactly the hub's frames from its start keyframe on: every video frame,
// and every audio frame written after that keyframe and presented at or
// after it, each with DTS floor((DTS−b)/90) ms and PTS floor((PTS−b)/90)
// ms, b the keyframe's DTS, its key flag, and its capture epoch numbered
// from the keyframe's hub epoch (one onMetaData per hub epoch).
func assertMatchesHub(t *testing.T, hubFrames, got []media.Frame) {
	t.Helper()
	hv, hvAt := byKind(hubFrames, media.KindVideo)
	gv, _ := byKind(got, media.KindVideo)
	require.NotEmpty(t, gv, "video received")
	k := joinKey(t, hv, len(gv))
	base, keyPTS, epoch0 := hv[k].DTS, hv[k].PTS, hv[k].Epoch
	match := func(kind string, i int, want, f media.Frame) {
		t.Helper()
		require.Equal(t, floorMS(want.DTS-base)*90, f.DTS, "%s %d DTS", kind, i)
		require.Equal(t, floorMS(want.PTS-base)*90, f.PTS, "%s %d PTS", kind, i)
		require.Equal(t, want.Epoch-epoch0+1, f.Epoch, "%s %d epoch", kind, i)
	}
	for i, f := range gv {
		match("video", i, hv[k+i], f)
		require.Equal(t, hv[k+i].Key, f.Key, "video %d key", i)
	}
	var want []media.Frame
	for _, f := range hubFrames[hvAt[k]:] {
		if f.Kind() == media.KindAudio && f.PTS >= keyPTS {
			want = append(want, f)
		}
	}
	ga, _ := byKind(got, media.KindAudio)
	require.Len(t, ga, len(want), "audio frames from the start keyframe on")
	for i, f := range ga {
		match("audio", i, want[i], f)
	}
}

// probeStreams runs ffprobe on u and returns its streams as type:codec.
func probeStreams(ctx context.Context, ffprobe, u string) ([]string, error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type,codec_name",
		"-of", "json", u).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var v struct {
		Streams []struct {
			Type  string `json:"codec_type"`
			Codec string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var s []string
	for _, st := range v.Streams {
		s = append(s, st.Type+":"+st.Codec)
	}
	return s, nil
}

// probePacket is one video packet as ffprobe reports it, in ms.
type probePacket struct {
	PTS   int64  `json:"pts"`
	DTS   int64  `json:"dts"`
	Flags string `json:"flags"`
}

// probeVideoPackets runs ffprobe on u until the stream ends and returns its video
// packets.
func probeVideoPackets(ctx context.Context, ffprobe, u string) ([]probePacket, error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v",
		"-show_entries", "packet=pts,dts,flags", "-of", "json", u).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var v struct {
		Packets []probePacket `json:"packets"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	return v.Packets, nil
}

// fixtureFile is a fixture's absolute file:// URL.
func fixtureFile(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return fileURL(p, "")
}

// TestM3b1SteadyState plays h264_aac.ts and h265_aac51.ts, looped twice,
// from the play output: conform.ReadRTMP reads one clean epoch, exactly the
// hub's frames from the cached keyframe it joined at, and ffprobe, a second
// player, sees both streams.
func TestM3b1SteadyState(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe", "rtmp")
	for _, tc := range []struct {
		name  string
		video media.Codec
		probe []string
	}{
		{"h264_aac.ts", media.CodecH264, []string{"video:h264", "audio:aac"}},
		{"h265_aac51.ts", media.CodecH265, []string{"video:hevc", "audio:aac"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clip := loopClip(t, ffmpeg, tc.name, 2, 0)
			r := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
			p := servePlay(t, "", rtmpplay.Options{}, nil, map[string]*hub.Hub{"s": r.h})
			waitLive(t, p.srv, "s")
			capCh := readPlay(p.url())
			type probed struct {
				streams []string
				err     error
			}
			probeCh := make(chan probed, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), runLimit)
				defer cancel()
				s, err := probeStreams(ctx, ffprobe, p.url())
				probeCh <- probed{s, err}
			}()

			r.check(t, []uint32{1})
			c := waitCapture(t, capCh)
			assert.Empty(t, c.Violations)
			assert.Equal(t, []uint32{1}, epochs(c.Frames))
			assertCodecs(t, c.Frames, tc.video, media.CodecAAC)
			assertMatchesHub(t, r.col.frames, c.Frames)
			pr := <-probeCh
			require.NoError(t, pr.err)
			assert.ElementsMatch(t, tc.probe, pr.streams)
			st := p.srv.Stats()
			assert.Zero(t, st.Resyncs)
			assert.Zero(t, st.FrameErrors)
			t.Logf("hub %s; read %s; ffprobe %v; play %+v", kindCounts(r.col.frames), kindCounts(c.Frames), pr.streams, st)
		})
	}
}

// errPrimaryEnded reports the end of failAtEOF's first input as a failure.
var errPrimaryEnded = errors.New("e2e: primary ended")

// eofFails turns a reader's end into a failure, calling ended first.
type eofFails struct {
	ingest.Reader
	ended func()
}

func (r eofFails) Read() (ingest.Frame, error) {
	f, err := r.Reader.Read()
	if errors.Is(err, io.EOF) {
		r.ended()
		return f, errPrimaryEnded
	}
	return f, err //nolint:wrapcheck // the reader's errors pass through
}

// failAtEOF is ingest.Open, except that first opens only once and its end is
// a failure, and that the other inputs open only once first has ended: the
// pipeline, which opens every input at once at start-up, starts on first
// however loaded the machine, fails over to the next input at first's end
// and stays there.
func failAtEOF(first string) func(ctx context.Context, u string) (ingest.Reader, error) {
	var (
		opened   atomic.Bool
		once     sync.Once
		firstEnd = make(chan struct{})
	)
	ended := func() { once.Do(func() { close(firstEnd) }) }
	return func(ctx context.Context, u string) (ingest.Reader, error) {
		if u != first {
			select {
			case <-firstEnd:
			case <-ctx.Done():
				return nil, ctx.Err() //nolint:wrapcheck // the pipeline wraps it
			}
			return ingest.Open(ctx, u, ingest.Env{}) //nolint:wrapcheck // the pipeline wraps it
		}
		if opened.Swap(true) {
			return nil, errPrimaryEnded
		}
		r, err := ingest.Open(ctx, u, ingest.Env{})
		if err != nil {
			return nil, err //nolint:wrapcheck // the pipeline wraps it
		}
		return eofFails{Reader: r, ended: ended}, nil
	}
}

// runPusher runs p until the hub closes; done is closed once Run has
// returned, and err holds its result by then.
func runPusher(t *testing.T, p *rtmppush.Pusher) (done <-chan struct{}, err *error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	var perr error
	go func() {
		defer close(ch)
		perr = p.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-ch
	})
	return ch, &perr
}

// pushRow is a push-out to MediaMTX and our RTMP pull reading it back.
type pushRow struct {
	src, back *m2aRun
	rl        *readerLog
	p         *rtmppush.Pusher
	done      <-chan struct{}
	err       *error
	join      time.Duration
}

// startPushRow pushes the hub of src to url and reads url back with our RTMP
// pull once MediaMTX has the path ready. The read-back run ends once the
// source has ended, the hub closed and the push ended.
func startPushRow(t *testing.T, src *m2aRun, url string, opt rtmppush.Options) *pushRow {
	t.Helper()
	p, err := rtmppush.New(src.h, url, opt)
	require.NoError(t, err)
	started := time.Now()
	done, perr := runPusher(t, p)
	go src.wait() // closes the hub at the source's end, which ends the push
	ended := make(chan struct{})
	rl := &readerLog{}
	back := startRun(t, runOptions{inputs: []string{url}, open: rl.wrap(mtxOpen(ended))})
	mtxEnd(back, done, ended)
	back.waitActive(t, 0, m3b1MTXReady)
	return &pushRow{src: src, back: back, rl: rl, p: p, done: done, err: perr, join: time.Since(started)}
}

// finish waits for the source, the push and the read-back, and checks that
// the read-back is clean in wantEpochs and the push ended without error.
func (r *pushRow) finish(t *testing.T, wantEpochs ...uint32) {
	t.Helper()
	r.back.check(t, wantEpochs)
	<-r.done
	require.NoError(t, *r.err)
}

// TestM3b1EpochChange fails over from h264_aac.ts (looped twice) to
// h264_aac_640.ts (a new resolution, audio kept) and, separately, to the
// video-only h264_640.ts (the audio track disappears): one epoch change in
// the hub. A player of the
// play output gets one onMetaData per epoch, the second listing the new
// width, and one sequence header per carried track per epoch (the audio
// header not again when audio is gone); conform.ReadRTMP reads both epochs
// clean, exactly the hub's frames; ffmpeg decodes it and exits 0 at the end
// of the stream. The same hub is pushed to MediaMTX on one connection (one
// epoch change pushed, no republish), and our RTMP pull reads it back clean
// in two epochs: MediaMTX relays the new parameter sets, a new config.
func TestM3b1EpochChange(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTX(t)
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	for _, tc := range []struct {
		backup       string
		audioHeaders int
		backEpochs   []uint32
	}{
		{"h264_aac_640.ts", 2, []uint32{1, 2}},
		{"h264_640.ts", 1, []uint32{1, 2}},
	} {
		t.Run(tc.backup, func(t *testing.T) {
			t.Parallel()
			// 12 s, so that the read-back of MediaMTX joins before the
			// failover however long MediaMTX takes (m3b1MTXReady).
			primary := fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 2, 0), "")
			r := startRun(t, runOptions{
				inputs: []string{primary, fixtureFile(t, tc.backup)},
				open:   failAtEOF(primary),
			})
			p := servePlay(t, "", rtmpplay.Options{}, nil, map[string]*hub.Hub{"s": r.h})
			waitLive(t, p.srv, "s")
			capCh := readPlay(p.url())
			rawCh := rawPlay(p.url())
			f := startFFmpeg(t, ffmpeg, "-i", p.url(), "-f", "null", "-")
			push := startPushRow(t, r, "rtmp://"+mtx+"/"+mtxPath("epoch"), rtmppush.Options{})

			r.check(t, []uint32{1, 2})
			assert.Equal(t, uint64(1), r.stats.Switches)
			c := waitCapture(t, capCh)
			assert.Empty(t, c.Violations)
			assert.Equal(t, []uint32{1, 2}, epochs(c.Frames))
			assertMatchesHub(t, r.col.frames, c.Frames)
			n := countMsgs(t, waitRaw(t, rawCh))
			assert.Equal(t, 2, n.metas, "onMetaData per epoch")
			assert.Equal(t, []any{float64(320), float64(640)}, n.widths, "onMetaData widths")
			assert.Equal(t, 2, n.videoHeaders, "video headers")
			assert.Equal(t, tc.audioHeaders, n.audioHeaders, "audio headers")
			f.requireExited(t)
			st := p.srv.Stats()
			assert.Zero(t, st.EndedCodecChange)
			assert.Zero(t, st.Resyncs)

			push.finish(t, tc.backEpochs...)
			ps := push.p.Stats()
			assert.Equal(t, uint64(1), ps.Connects)
			assert.Zero(t, ps.Failures)
			assert.Zero(t, ps.Republishes)
			assert.Zero(t, ps.Resyncs)
			assert.Equal(t, uint64(1), ps.Mux.Epochs, "one epoch change pushed")
			t.Logf("hub %s; read %s; messages %+v; play %+v; push %+v joined %v, read back %s, reader %v",
				kindCounts(r.col.frames), kindCounts(c.Frames), n, st, ps, push.join.Round(time.Millisecond),
				kindCounts(push.back.col.frames), push.rl.stats())
		})
	}
}

// shrinkSend shrinks the send buffer of an accepted connection, so that a
// player that reads slowly blocks the server's writes early.
func shrinkSend(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
}

// slowProxy relays TCP connections to upstream. On each it stops reading
// from upstream for pause once it has relayed after bytes downstream; its
// upstream socket has a small receive buffer, so the server's writes block
// soon after. It returns the proxy's address.
func slowProxy(t *testing.T, upstream string, after int, pause time.Duration) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	stop := make(chan struct{})
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	track := func(c net.Conn) {
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
	}
	relay := func(client net.Conn) {
		defer wg.Done()
		defer client.Close()
		up, err := new(net.Dialer).DialContext(context.Background(), "tcp", upstream)
		if err != nil {
			return
		}
		defer up.Close()
		track(up)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(4096)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(up, client)
			_ = up.Close()
		}()
		buf := make([]byte, 4096)
		total, paused := 0, false
		for {
			n, err := up.Read(buf)
			if n > 0 {
				if _, werr := client.Write(buf[:n]); werr != nil {
					return
				}
				total += n
				if total >= after && !paused {
					paused = true
					select {
					case <-time.After(pause):
					case <-stop:
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			track(c)
			wg.Add(1)
			go relay(c)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// TestM3b1SlowConsumer plays h264_aac.ts, looped three times, to a normal
// player and, through slowProxy, to a throttled one that stops reading for
// 4 s, 2 s into the stream (player queues of the cached GOP plus 64 frames,
// small socket buffers). The throttled player loses frames and resumes at a
// keyframe with onMetaData and headers each time, clean in every capture
// epoch; the normal one receives exactly the hub's frames in one epoch.
func TestM3b1SlowConsumer(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 3, 0)
	r := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
	p := servePlay(t, "", rtmpplay.Options{Queue: 32}, shrinkSend, map[string]*hub.Hub{"s": r.h})
	waitLive(t, p.srv, "s")
	normalCh := readPlay(p.url())
	proxy := slowProxy(t, p.addr, 100<<10, 4*time.Second)
	slowCh := readPlay("rtmp://" + proxy + "/" + m3b1App + "/s")

	r.check(t, []uint32{1})
	normal := waitCapture(t, normalCh)
	slow := waitCapture(t, slowCh)
	st := p.srv.Stats()
	assert.Empty(t, normal.Violations)
	assert.Equal(t, []uint32{1}, epochs(normal.Frames))
	assertMatchesHub(t, r.col.frames, normal.Frames)

	assert.Empty(t, slow.Violations)
	require.Positive(t, st.Resyncs, "the throttled player lost frames")
	assert.Equal(t, st.Resyncs, st.Mux.Resyncs)
	assert.Len(t, epochs(slow.Frames), 1+int(st.Resyncs), "one capture epoch per resume")
	assert.Less(t, len(slow.Frames), len(normal.Frames), "the throttled player lost frames")
	assert.Equal(t, uint64(2), st.EndedHubClosed, "both players read to the end")
	t.Logf("hub %s; normal %s; throttled %s; play %+v",
		kindCounts(r.col.frames), kindCounts(normal.Frames), kindCounts(slow.Frames), st)
}

// TestM3b1BFrames plays h264_pyramid.ts (a B-pyramid, decode delay 3) to
// ffprobe: every video packet's pts and dts, in ms, aligned at the first
// keyframe ffprobe receives (DTS b), are floor((PTS−b)/90) and
// floor((DTS−b)/90) of the hub's frames from that keyframe on.
// conform.ReadRTMP, a second player, reads the same clean.
func TestM3b1BFrames(t *testing.T) {
	t.Parallel()
	ffprobe := testpeer.Require(t, "ffprobe", "rtmp")
	r := startRun(t, runOptions{inputs: []string{fixtureFile(t, "h264_pyramid.ts")}})
	p := servePlay(t, "", rtmpplay.Options{}, nil, map[string]*hub.Hub{"s": r.h})
	waitLive(t, p.srv, "s")
	capCh := readPlay(p.url())
	type probed struct {
		pkts []probePacket
		err  error
	}
	probeCh := make(chan probed, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		pkts, err := probeVideoPackets(ctx, ffprobe, p.url())
		probeCh <- probed{pkts, err}
	}()

	r.check(t, []uint32{1})
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	assertMatchesHub(t, r.col.frames, c.Frames)
	pr := <-probeCh
	require.NoError(t, pr.err)
	require.NotEmpty(t, pr.pkts)
	hv, _ := byKind(r.col.frames, media.KindVideo)
	k := joinKey(t, hv, len(pr.pkts))
	b := hv[k].DTS
	reordered := 0
	for i, pk := range pr.pkts {
		f := hv[k+i]
		require.Equal(t, floorMS(f.DTS-b), pk.DTS, "packet %d dts", i)
		require.Equal(t, floorMS(f.PTS-b), pk.PTS, "packet %d pts", i)
		require.Equal(t, f.Key, strings.HasPrefix(pk.Flags, "K"), "packet %d key", i)
		if pk.PTS != pk.DTS {
			reordered++
		}
	}
	assert.Positive(t, reordered, "packets with a composition time")
	t.Logf("hub %s; ffprobe %d video packets from hub video frame %d (%d reordered); read %s",
		kindCounts(r.col.frames), len(pr.pkts), k, reordered, kindCounts(c.Frames))
}

// TestM3b1MediaMTXPull has MediaMTX pull our play output on demand, for
// H.264+AAC (path m3b1pull) and H.265+AAC (m3b1pullhevc), and our RTSP pull
// read it from MediaMTX over TCP: one epoch of violation-free frames. Each
// path is warmed with a throwaway reader and joined 1 s later, once the
// relay's sender-report mapping has settled (as TestM2cRTSPMediaMTXH264
// does): MediaMTX's RTMP client receives the cached GOP as a burst.
func TestM3b1MediaMTXPull(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTXPull(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	// The rows run one after the other, each with its own source and play
	// listener on the fixed port: a parallel subtest may wait for a slot,
	// and its source must not run out meanwhile.
	for _, row := range []struct {
		path, fixture string
		video         media.Codec
	}{
		{"m3b1pull", "h264_aac.ts", media.CodecH264},
		{"m3b1pullhevc", "h265_aac51.ts", media.CodecH265},
	} {
		t.Run(row.path, func(t *testing.T) {
			clip := loopClip(t, ffmpeg, row.fixture, 4, 0)
			src := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
			p := servePlay(t, m3b1PullAddr, rtmpplay.Options{}, nil, map[string]*hub.Hub{row.path: src.h})
			waitLive(t, p.srv, row.path)
			u := "rtsp://" + mtx + "/" + row.path + "?transport=tcp"
			settleRTSP(t, u)
			var rl readerLog
			r := startRun(t, runOptions{inputs: []string{u}, open: rl.wrap(nil)})
			r.waitActive(t, 0, rtspReady)
			time.Sleep(12 * time.Second)
			r.cancel()
			r.check(t, []uint32{1})
			assertCodecs(t, r.col.frames, row.video, media.CodecAAC)
			st := rl.stats()
			assert.Zero(t, st["sync_jumps"], "offset jumps")
			t.Logf("read %s over %v; clock %+v; reader %v; failures %v", r.col.counts(),
				r.col.span().Round(time.Millisecond), r.stats.Clock, st, r.failures())
		})
	}
}

// pausingConn stalls its writes once, for pause, after after bytes: to the
// pusher, a push target that stops reading for a while (MediaMTX itself
// always reads at once).
type pausingConn struct {
	net.Conn
	after  int
	pause  time.Duration
	sent   int
	paused bool
}

// Write is called under pkg/rtmp's write mutex, one at a time.
func (c *pausingConn) Write(p []byte) (int, error) {
	if c.sent >= c.after && !c.paused {
		c.paused = true
		time.Sleep(c.pause)
	}
	n, err := c.Conn.Write(p)
	c.sent += n
	return n, err //nolint:wrapcheck // net.Conn contract
}

// pausingDial dials with small socket buffers and returns a pausingConn.
func pausingDial(after int, pause time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err //nolint:wrapcheck // the RTMP client wraps it
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetWriteBuffer(4096)
			_ = tc.SetReadBuffer(4096)
		}
		return &pausingConn{Conn: c, after: after, pause: pause}, nil
	}
}

// TestM3b1PushToMediaMTX pushes h264_aac.ts, h264_pyramid.ts (B-frames) and
// h265_aac51.ts, each looped twice, to MediaMTX, and our RTMP pull reads
// each back: one epoch of clean frames, short of the source by at most what
// was sent before the read-back joined (joinBounds). A slow-target row, with
// small socket buffers and a connection that stalls its writes for 2.5 s,
// about 5 s in, loses frames, resumes at a keyframe with onMetaData and
// headers on the same connection, and reads back clean in one epoch.
func TestM3b1PushToMediaMTX(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTX(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	for _, tc := range []struct {
		name, fixture string
		video         media.Codec
		slow          bool
	}{
		{"h264", "h264_aac.ts", media.CodecH264, false},
		{"pyramid", "h264_pyramid.ts", media.CodecH264, false},
		{"h265", "h265_aac51.ts", media.CodecH265, false},
		{"slow", "h264_aac.ts", media.CodecH264, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			passes := 2
			var opt rtmppush.Options
			if tc.slow {
				// The stall starts about 5 s in, once the read-back has
				// joined, and is shorter than the read-back's 4 s stall
				// limit (a reconnect there would be a new epoch), but
				// longer than the 1.9 s the pusher's queue holds.
				passes = 3
				opt = rtmppush.Options{Queue: 32, RTMP: rtmp.Config{Dial: pausingDial(300<<10, 2500*time.Millisecond)}}
			}
			clip := loopClip(t, ffmpeg, tc.fixture, passes, 0)
			src := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
			row := startPushRow(t, src, "rtmp://"+mtx+"/"+mtxPath("push-"+tc.name), opt)
			row.finish(t, 1)
			src.check(t, []uint32{1})
			assertCodecs(t, row.back.col.frames, tc.video, media.CodecAAC)
			ps := row.p.Stats()
			assert.Equal(t, uint64(1), ps.Connects)
			assert.Zero(t, ps.Failures)
			assert.Zero(t, ps.Republishes)
			assert.Zero(t, ps.FrameErrors)
			st := row.rl.stats()
			got := row.back.col.counts()
			if tc.slow {
				require.Positive(t, ps.Resyncs, "the pusher lost frames")
				assert.Equal(t, ps.Resyncs, ps.Mux.Resyncs)
				assert.Less(t, got[epochKind{1, media.KindVideo}], src.col.counts()[epochKind{1, media.KindVideo}])
			} else {
				assert.Zero(t, ps.Resyncs)
				assertRTMPCounts(t, src.col.counts(), got, row.back, st, joinBounds(row.join, 0))
			}
			t.Logf("source %s; joined %v; read back %s; push %+v; reader %v; clock %+v",
				src.col.counts(), row.join.Round(time.Millisecond), got, ps, st, row.back.stats.Clock)
		})
	}
}

// TestM3b1SharedPort runs publishers and players on one listener: ffmpeg
// publishes to live/<secret key> on the pipeline's RTMP listen input, the
// play output serves the pipeline's hub as stream cam on the same address,
// and three players read live/cam from it, each exactly the hub's frames in
// one clean epoch. A player of live/<secret key> gets StreamNotFound.
func TestM3b1SharedPort(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg", "rtmp")
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 2, 0)
	port := freeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	key := m3b1App + "/" + rand.Text()[:16]
	r := startRun(t, runOptions{inputs: []string{"rtmp://" + addr + "/" + key + "?mode=listen"}})
	waitListening(t, port)
	before := rtmpsrv.Default.Stats(addr)
	require.Equal(t, 1, before.Routes)

	srv := rtmpplay.New(func(name string) (*hub.Hub, bool) { return r.h, name == "cam" }, rtmpplay.Options{})
	unregister, err := rtmpsrv.Default.RegisterPlay(addr, m3b1App, srv, rtmp.Config{}, rtmpsrv.Policy{})
	require.NoError(t, err)
	t.Cleanup(unregister)
	shared := rtmpsrv.Default.Stats(addr)
	assert.Equal(t, before.ID, shared.ID, "one listener")
	assert.Equal(t, 1, shared.Apps)

	f := startFFmpeg(t, ffmpeg, "-re", "-i", clip, "-c", "copy", "-f", "flv", "rtmp://"+addr+"/"+key)
	r.endWith(f.done)
	waitLive(t, srv, "cam")
	play := "rtmp://" + addr + "/" + m3b1App + "/cam"
	players := make([]<-chan capResult, 0, 3)
	for range 3 {
		players = append(players, readPlay(play))
	}
	require.Eventually(t, func() bool { return rtmpsrv.Default.Stats(addr).StreamPlayers[m3b1App+"/cam"] == 3 },
		m3b1Read, 10*time.Millisecond, "three players of live/cam")

	ctx, cancel := context.WithTimeout(context.Background(), m3b1Read)
	defer cancel()
	c, err := rtmp.Play(ctx, "rtmp://"+addr+"/"+key)
	if c != nil {
		_ = c.Close()
	}
	var se *rtmp.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "NetStream.Play.StreamNotFound", se.Code)
	assert.Equal(t, "stream not available", se.Description)
	assert.NotContains(t, err.Error(), key[len(m3b1App)+1:], "the key is not echoed")
	assert.Equal(t, uint64(1), rtmpsrv.Default.Stats(addr).RejectedPlayPublishPath)

	r.check(t, []uint32{1})
	f.requireExited(t)
	for i, ch := range players {
		got := waitCapture(t, ch)
		assert.Empty(t, got.Violations, "player %d", i)
		assert.Equal(t, []uint32{1}, epochs(got.Frames), "player %d", i)
		assertMatchesHub(t, r.col.frames, got.Frames)
	}
	st := srv.Stats()
	assert.Equal(t, uint64(3), st.PlayersStarted)
	t.Logf("hub %s; play %+v", kindCounts(r.col.frames), st)
}
