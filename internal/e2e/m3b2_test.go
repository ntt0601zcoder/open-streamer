package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtspplay"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// The M3b2 scenarios run fixtures through a pipeline into a hub and out of
// the RTSP play output, read by conform.ReadRTSP (over TCP and UDP),
// ffprobe, ffmpeg, our RTSP pull input and MediaMTX (pulling it on demand
// for its own RTSP readers). Over TCP a player that falls behind loses hub
// frames, not RTP packets (dropped frames consume no sequence numbers), so
// ReadRTSP cannot see where it resumed: the slow-consumer scenario maps the
// frames it read back to the hub's to prove every resume is a keyframe.

const (
	// m3b2App is the play app of every scenario: players read live/<name>.
	m3b2App = "live"
	// m3b2Read bounds the RTSP set-up and every wait for the next packet: it
	// covers a failover's gap and a throttled player's pause.
	m3b2Read = 15 * time.Second
	// m3b2PullAddr is the play address that the RTSP MediaMTX pulls its
	// paths m3b2pull and m3b2pullhevc from (CI's step and the local
	// container set MTX_PATHS_M3B2PULL*_SOURCE to it).
	m3b2PullAddr = "127.0.0.1:19362"
)

// rtspOut is an RTSP play output on its own listener, with UDP on
// 127.0.0.1.
type rtspOut struct {
	srv  *rtspplay.Server
	addr string
}

// serveRTSP serves hubs by stream name on addr (a free port when empty)
// until the test ends.
func serveRTSP(t *testing.T, addr string, opt rtspplay.Options, hubs map[string]*hub.Hub) *rtspOut {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	opt.App = m3b2App
	opt.Server.UDPHost = "127.0.0.1"
	srv := rtspplay.New(func(name string) (*hub.Hub, bool) {
		h, ok := hubs[name]
		return h, ok
	}, opt)
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		assert.NoError(t, srv.Close())
		select {
		case err := <-served:
			assert.ErrorIs(t, err, rtsp.ErrServerClosed)
		case <-time.After(m3b2Read):
			t.Error("Serve did not return after Close")
		}
	})
	require.Eventually(t, func() bool { return srv.UDPPorts()[0] != 0 }, m3b2Read, time.Millisecond, "UDP pair bound")
	return &rtspOut{srv: srv, addr: l.Addr().String()}
}

// url is the play URL of the stream s.
func (o *rtspOut) url() string { return "rtsp://" + o.addr + "/" + m3b2App + "/s" }

// waitAV waits until the hub is live with a video and an audio track, so
// that DESCRIBE lists both at once.
func waitAV(t *testing.T, h *hub.Hub) {
	t.Helper()
	require.Eventually(t, func() bool { return h.Live() && len(h.TrackConfigs()) >= 2 }, 15*time.Second,
		5*time.Millisecond, "hub live with video and audio")
}

// readRTSP reads u with conform.ReadRTSP until the end of the stream, in
// the background.
func readRTSP(u string) <-chan capResult {
	ch := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c, err := conform.ReadRTSP(ctx, u, conform.ReadOptions{Timeout: m3b2Read})
		ch <- capResult{c, err}
	}()
	return ch
}

// pullRTSP reads u with our RTSP pull input in a pipeline of its own, until
// src has ended and the play output's session with it has ended.
func pullRTSP(t *testing.T, src *m2aRun, u string) (*m2aRun, *readerLog) {
	t.Helper()
	ended := make(chan struct{})
	rl := &readerLog{}
	back := startRun(t, runOptions{inputs: []string{u}, open: rl.wrap(mtxOpen(ended))})
	mtxEnd(back, src.stop, ended)
	return back, rl
}

// epochStarts returns the PTS of the first video frame of each hub epoch
// after the first in hv.
func epochStarts(hv []media.Frame) []int64 {
	var out []int64
	for i := 1; i < len(hv); i++ {
		if hv[i].Epoch != hv[i-1].Epoch {
			out = append(out, hv[i].PTS)
		}
	}
	return out
}

// joinedVideo returns the hub video frames a subscriber that joined at the
// keyframe hv[k] receives: from it on, but for the RASL frames the hub's
// join gate drops after it (those up to the first trailing picture, a
// non-RASL frame presented after the keyframe), whose references precede
// the join.
func joinedVideo(hv []media.Frame, k int) []media.Frame {
	out := []media.Frame{hv[k]}
	gated := true
	for _, f := range hv[k+1:] {
		if gated && !f.RASL && f.PTS > hv[k].PTS {
			gated = false
		}
		if gated && f.RASL {
			continue
		}
		out = append(out, f)
	}
	return out
}

// rtspJoin returns the hub keyframe a player that received n video frames
// up to the end of hv joined at, and the video frames it was sent.
func rtspJoin(t *testing.T, hv []media.Frame, n int) (int, []media.Frame) {
	t.Helper()
	for k := len(hv) - 1; k >= 0; k-- {
		if !hv[k].Key {
			continue
		}
		if w := joinedVideo(hv, k); len(w) == n {
			return k, w
		} else if len(w) > n {
			break
		}
	}
	require.Failf(t, "no join keyframe", "%d video frames received, the hub has %d", n, len(hv))
	return 0, nil
}

// assertRTSPMatchesHub checks that a player that never lost a frame
// received exactly the hub's frames from its join keyframe on, up to the
// end of hubFrames: every video frame (but the RASL frames the hub's join
// gate drops), with PTS PTS−b in 90 kHz ticks (b the keyframe's DTS, the
// RTP-Info origin), its key flag, the parameter sets of its hub config (in
// band, as ReadRTSP rebuilds the config from them) and its capture epoch
// numbered from the keyframe's hub epoch; and every audio frame written
// after that keyframe and presented at or after it, with PTS PTS−b within
// one tick (the audio clock's rounding) and the capture epoch its PTS falls
// in.
func assertRTSPMatchesHub(t *testing.T, hubFrames, got []media.Frame) {
	t.Helper()
	hv, hvAt := byKind(hubFrames, media.KindVideo)
	gv, _ := byKind(got, media.KindVideo)
	require.NotEmpty(t, gv, "video received")
	k, wv := rtspJoin(t, hv, len(gv))
	base, keyPTS, epoch0 := hv[k].DTS, hv[k].PTS, hv[k].Epoch
	for i, f := range gv {
		w := wv[i]
		require.Equal(t, w.PTS-base, f.PTS, "video %d PTS", i)
		require.Equal(t, w.Key, f.Key, "video %d key", i)
		require.Equal(t, w.Epoch-epoch0+1, f.Epoch, "video %d epoch", i)
		require.Equal(t, w.Config.SPS, f.Config.SPS, "video %d SPS", i)
		require.Equal(t, w.Config.PPS, f.Config.PPS, "video %d PPS", i)
		require.Equal(t, w.Config.VPS, f.Config.VPS, "video %d VPS", i)
	}
	starts := epochStarts(hv[k:])
	var want []media.Frame
	for _, f := range hubFrames[hvAt[k]:] {
		if f.Kind() == media.KindAudio && f.PTS >= keyPTS {
			want = append(want, f)
		}
	}
	ga, _ := byKind(got, media.KindAudio)
	require.Len(t, ga, len(want), "audio frames from the join keyframe on")
	for i, f := range ga {
		d := f.PTS - (want[i].PTS - base)
		require.LessOrEqual(t, max(d, -d), int64(1), "audio %d PTS: got %d, hub %d", i, f.PTS, want[i].PTS-base)
		ep := uint32(1)
		for _, s := range starts {
			if f.PTS >= s-base {
				ep++
			}
		}
		require.Equal(t, ep, f.Epoch, "audio %d epoch", i)
	}
}

// kindEpochs returns the epochs of the frames of kind k, in order. A
// capture's frames are merged by DTS, which ReadRTSP derives from PTS (RTP
// carries no DTS): at a failover the new epoch's first DTS continues from
// the old one's, so its video interleaves with the old epoch's last audio,
// and only each kind's own sequence of epochs is meaningful.
func kindEpochs(frames []media.Frame, k media.Kind) []uint32 {
	of, _ := byKind(frames, k)
	return epochs(of)
}

// resumesAtKeys checks the frames of a player that read to the hub's end
// but lost frames on the way: every video frame is a hub video frame (by
// PTS, the hub's last frame aligned with the player's last), the first is a
// keyframe whose DTS is the session's base, and the first after every gap
// is a keyframe; every audio frame is a hub audio frame within one tick. It
// returns the number of gaps.
func resumesAtKeys(t *testing.T, hubFrames, got []media.Frame) (jumps int) {
	t.Helper()
	hv, _ := byKind(hubFrames, media.KindVideo)
	gv, _ := byKind(got, media.KindVideo)
	require.NotEmpty(t, gv, "video received")
	base := hv[len(hv)-1].PTS - gv[len(gv)-1].PTS
	at := map[int64]int{}
	for i, f := range hv {
		at[f.PTS] = i
	}
	prev := -1
	for i, f := range gv {
		j, ok := at[f.PTS+base]
		require.True(t, ok, "video %d (PTS %d) is a hub frame", i, f.PTS+base)
		require.Equal(t, hv[j].Key, f.Key, "video %d key", i)
		switch {
		case i == 0:
			require.True(t, hv[j].Key, "the first frame is a keyframe")
			require.Equal(t, base, hv[j].DTS, "the session's base is the first frame's DTS")
		case j != prev+1:
			require.Greater(t, j, prev, "video %d in decode order", i)
			require.True(t, hv[j].Key, "the first frame after a gap is a keyframe (hub video %d after %d)", j, prev)
			jumps++
		}
		prev = j
	}
	ha, _ := byKind(hubFrames, media.KindAudio)
	pts := make([]int64, 0, len(ha))
	for _, f := range ha {
		pts = append(pts, f.PTS)
	}
	slices.Sort(pts)
	ga, _ := byKind(got, media.KindAudio)
	for i, f := range ga {
		p := f.PTS + base
		n, _ := slices.BinarySearch(pts, p-1)
		require.True(t, n < len(pts) && pts[n] <= p+1, "audio %d (PTS %d) is a hub frame", i, p)
	}
	return jumps
}

// probeRTSPStreams runs ffprobe on u over transport and returns its
// streams as type:codec.
func probeRTSPStreams(ctx context.Context, ffprobe, u, transport string) ([]string, error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-rtsp_transport", transport,
		"-show_entries", "stream=codec_type,codec_name", "-of", "json", u).Output()
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
	s := make([]string, 0, len(v.Streams))
	for _, st := range v.Streams {
		s = append(s, st.Type+":"+st.Codec)
	}
	return s, nil
}

// rtspPacket is one video packet as ffprobe reports it, in the stream's
// time base; a timestamp is absent (has false) where ffprobe has none.
type rtspPacket struct {
	pts, dts       int64
	hasPTS, hasDTS bool
	key            bool
}

// probeRTSPVideo runs ffprobe on u over TCP until the stream ends and
// returns the video stream's time base and packets.
func probeRTSPVideo(ctx context.Context, ffprobe, u string) (timeBase string, pkts []rtspPacket, err error) {
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-rtsp_transport", "tcp", "-select_streams", "v",
		"-show_entries", "packet=pts,dts,flags:stream=time_base", "-of", "json", u).Output()
	if err != nil {
		return "", nil, fmt.Errorf("ffprobe: %w", err)
	}
	var v struct {
		Packets []struct {
			PTS   json.RawMessage `json:"pts"`
			DTS   json.RawMessage `json:"dts"`
			Flags string          `json:"flags"`
		} `json:"packets"`
		Streams []struct {
			TimeBase string `json:"time_base"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", nil, fmt.Errorf("ffprobe: %w", err)
	}
	if len(v.Streams) != 1 {
		return "", nil, fmt.Errorf("ffprobe: %d video streams", len(v.Streams))
	}
	ts := func(raw json.RawMessage) (int64, bool) {
		var n int64
		if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
			return 0, false // absent or "N/A"
		}
		return n, true
	}
	for _, p := range v.Packets {
		var pk rtspPacket
		pk.pts, pk.hasPTS = ts(p.PTS)
		pk.dts, pk.hasDTS = ts(p.DTS)
		pk.key = strings.HasPrefix(p.Flags, "K")
		pkts = append(pkts, pk)
	}
	return v.Streams[0].TimeBase, pkts, nil
}

// aacChannelConfig returns the channel configuration of an
// AudioSpecificConfig with a 4-bit sampling index, -1 when too short.
func aacChannelConfig(asc []byte) int {
	if len(asc) < 2 {
		return -1
	}
	return int(asc[1]>>3) & 0x0f
}

// describe returns the SDP the play output answers for u, opening a
// session and closing it at once.
func describe(t *testing.T, u string) (rtsp.Description, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), m3b2Read)
	defer cancel()
	c, err := rtsp.Dial(ctx, u, rtsp.Config{})
	if err != nil {
		return rtsp.Description{}, fmt.Errorf("e2e: describe: %w", err)
	}
	d := c.Description()
	_ = c.Close()
	return d, nil
}

// TestM3b2SteadyState plays h264_aac.ts and h265_aac51.ts, looped twice,
// from the RTSP play output: conform.ReadRTSP reads one clean epoch over
// TCP and over UDP, each exactly the hub's frames from the keyframe it
// joined at (the cached one over TCP, the next live one over UDP) with no
// RTP packet lost; ffprobe sees both streams; our RTSP pull input reads one
// clean epoch with no A/V correction and no offset jump.
func TestM3b2SteadyState(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
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
			o := serveRTSP(t, "", rtspplay.Options{}, map[string]*hub.Hub{"s": r.h})
			waitAV(t, r.h)
			tcpCh := readRTSP(o.url())
			udpCh := readRTSP(o.url() + "?transport=udp")
			type probed struct {
				streams []string
				err     error
			}
			probeCh := make(chan probed, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), runLimit)
				defer cancel()
				s, err := probeRTSPStreams(ctx, ffprobe, o.url(), "tcp")
				probeCh <- probed{s, err}
			}()
			back, rl := pullRTSP(t, r, o.url()+"?transport=tcp")

			r.check(t, []uint32{1})
			for _, tr := range []struct {
				name string
				ch   <-chan capResult
			}{{"tcp", tcpCh}, {"udp", udpCh}} {
				c := waitCapture(t, tr.ch)
				assert.Empty(t, c.Violations, tr.name)
				assert.Equal(t, []uint32{1}, epochs(c.Frames), tr.name)
				assert.Zero(t, c.RTPLost, tr.name)
				assertCodecs(t, c.Frames, tc.video, media.CodecAAC)
				assertRTSPMatchesHub(t, r.col.frames, c.Frames)
				t.Logf("%s read %s, %d RTP packets lost", tr.name, kindCounts(c.Frames), c.RTPLost)
			}
			pr := <-probeCh
			require.NoError(t, pr.err)
			assert.ElementsMatch(t, tc.probe, pr.streams)

			back.check(t, []uint32{1})
			assertCodecs(t, back.col.frames, tc.video, media.CodecAAC)
			st := rl.stats()
			assert.Zero(t, st["sync_corrected"], "A/V corrections")
			assert.Zero(t, st["sync_jumps"], "offset jumps")
			ps := o.srv.Stats()
			assert.Zero(t, ps.Resyncs)
			assert.Zero(t, ps.EndedCodecChange)
			assert.Zero(t, ps.EndedFrameError)
			t.Logf("hub %s; ffprobe %v; pull %s, reader %v; play %+v", kindCounts(r.col.frames), pr.streams,
				back.col.counts(), st, ps)
		})
	}
}

// TestM3b2EpochChange fails over from h264_aac.ts (looped twice) to
// h264_aac_640.ts (a new resolution, audio kept), to the video-only
// h264_640.ts (the audio track disappears, the session goes on with video)
// and to h265_aac51.ts (a codec change). Within a codec, conform.ReadRTSP
// reads two clean capture epochs, exactly the hub's frames, the second
// rebuilt from the new parameter sets sent in band before its keyframe,
// and ffmpeg plays to the end and exits 0. The codec change ends the
// session with a BYE per track after the last H.264 frame; a fresh
// DESCRIBE then lists H.265 and the 5.1 AAC, and a new ReadRTSP reads the
// H.265 epoch clean.
func TestM3b2EpochChange(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	for _, tc := range []struct {
		backup      string
		audio       bool // the backup has audio
		codecChange bool
	}{
		{"h264_aac_640.ts", true, false},
		{"h264_640.ts", false, false},
		{"h265_aac51.ts", true, true},
	} {
		t.Run(tc.backup, func(t *testing.T) {
			t.Parallel()
			primary := fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 2, 0), "")
			r := startRun(t, runOptions{
				inputs: []string{primary, fixtureFile(t, tc.backup)},
				open:   failAtEOF(primary),
			})
			o := serveRTSP(t, "", rtspplay.Options{}, map[string]*hub.Hub{"s": r.h})
			waitAV(t, r.h)
			capCh := readRTSP(o.url())
			if tc.codecChange {
				codecChange(t, r, o, capCh)
				return
			}
			f := startFFmpeg(t, ffmpeg, "-rtsp_transport", "tcp", "-i", o.url(), "-f", "null", "-")

			r.check(t, []uint32{1, 2})
			assert.Equal(t, uint64(1), r.stats.Switches)
			c := waitCapture(t, capCh)
			assert.Empty(t, c.Violations)
			assert.Equal(t, []uint32{1, 2}, kindEpochs(c.Frames, media.KindVideo))
			if tc.audio {
				assert.Equal(t, []uint32{1, 2}, kindEpochs(c.Frames, media.KindAudio))
			} else {
				assert.Equal(t, []uint32{1}, kindEpochs(c.Frames, media.KindAudio))
			}
			assert.Zero(t, c.RTPLost)
			assertRTSPMatchesHub(t, r.col.frames, c.Frames)
			got := kindCounts(c.Frames)
			assert.Positive(t, got[epochKind{2, media.KindVideo}], "video after the failover")
			if tc.audio {
				assert.Positive(t, got[epochKind{2, media.KindAudio}], "audio after the failover")
			} else {
				assert.Zero(t, got[epochKind{2, media.KindAudio}], "no audio after the failover")
			}
			for _, fr := range c.Frames {
				if fr.Kind() == media.KindVideo {
					assert.Equal(t, 320*int(fr.Epoch), fr.Config.Width, "width in epoch %d", fr.Epoch)
				}
			}
			f.requireExited(t)
			st := o.srv.Stats()
			assert.Zero(t, st.EndedCodecChange)
			assert.Zero(t, st.EndedTracksGone)
			assert.Zero(t, st.Resyncs)
			t.Logf("hub %s; read %s; play %+v", kindCounts(r.col.frames), got, st)
		})
	}
}

// codecChange runs the codec-change row of TestM3b2EpochChange: capCh is a
// ReadRTSP of the stream from before the failover.
func codecChange(t *testing.T, r *m2aRun, o *rtspOut, capCh <-chan capResult) {
	t.Helper()
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	assert.Equal(t, []uint32{1}, epochs(c.Frames))
	assert.Zero(t, c.RTPLost)
	assertCodecs(t, c.Frames, media.CodecH264, media.CodecAAC)
	require.Eventually(t, func() bool { return o.srv.Stats().EndedCodecChange == 1 }, m3b2Read, time.Millisecond,
		"the session ended for the codec change")

	var desc rtsp.Description
	require.Eventually(t, func() bool {
		d, err := describe(t, o.url())
		if err != nil || len(d.Media) != 2 {
			return false
		}
		desc = d
		v, a := d.Media[0], d.Media[1]
		return v.Codec == rtsp.CodecH265 && a.Codec == rtsp.CodecAAC && a.AAC != nil && aacChannelConfig(a.AAC.Config) == 6
	}, m3b2Read, 20*time.Millisecond, "a fresh DESCRIBE lists H.265 and 5.1 AAC")
	hevcCh := readRTSP(o.url())

	r.check(t, []uint32{1, 2})
	assert.Equal(t, uint64(1), r.stats.Switches)
	var first []media.Frame
	for _, f := range r.col.frames {
		if f.Epoch == 1 {
			first = append(first, f)
		}
	}
	// The session got every H.264 frame and nothing after: the H.264 epoch
	// is all written before the first H.265 frame.
	assertRTSPMatchesHub(t, first, c.Frames)

	h := waitCapture(t, hevcCh)
	assert.Empty(t, h.Violations)
	assert.Equal(t, []uint32{1}, epochs(h.Frames))
	assert.Zero(t, h.RTPLost)
	assertCodecs(t, h.Frames, media.CodecH265, media.CodecAAC)
	assertRTSPMatchesHub(t, r.col.frames, h.Frames)
	st := o.srv.Stats()
	assert.Equal(t, uint64(1), st.EndedCodecChange)
	assert.Zero(t, st.Resyncs)
	t.Logf("hub %s; H.264 read %s; DESCRIBE %d media; H.265 read %s; play %+v", kindCounts(r.col.frames),
		kindCounts(c.Frames), len(desc.Media), kindCounts(h.Frames), st)
}

// TestM3b2SlowConsumer plays h264_aac.ts, looped three times, to a normal
// TCP player and, through slowProxy, to a throttled one that stops reading
// for 4 s once 150 KiB have been relayed, about 3 s in (so after its first
// GOP, audio included, has been read), with session queues of the cached
// GOP plus 64 frames and small socket buffers. The throttled player loses
// frames: rtspplay counts the resyncs, and its frames, mapped back to the
// hub's, resume at a keyframe after every gap; ReadRTSP reads it clean, in
// one capture epoch (the parameter sets did not change) and with no RTP
// packet missing (dropped frames consume no sequence numbers). The normal
// player receives exactly the hub's frames.
func TestM3b2SlowConsumer(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 3, 0)
	r := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
	o := serveRTSP(t, "", rtspplay.Options{Queue: 32, Server: rtsp.ServerConfig{ConnHook: shrinkSend}},
		map[string]*hub.Hub{"s": r.h})
	waitAV(t, r.h)
	normalCh := readRTSP(o.url())
	proxy := slowProxy(t, o.addr, 150<<10, 4*time.Second)
	slowCh := readRTSP("rtsp://" + proxy + "/" + m3b2App + "/s")

	r.check(t, []uint32{1})
	normal := waitCapture(t, normalCh)
	slow := waitCapture(t, slowCh)
	require.Eventually(t, func() bool { return o.srv.Stats().EndedHubClosed == 2 }, m3b2Read, time.Millisecond,
		"both players read to the end")
	st := o.srv.Stats()
	assert.Empty(t, normal.Violations)
	assert.Equal(t, []uint32{1}, epochs(normal.Frames))
	assert.Zero(t, normal.RTPLost)
	assertRTSPMatchesHub(t, r.col.frames, normal.Frames)

	assert.Empty(t, slow.Violations)
	assert.Equal(t, []uint32{1}, epochs(slow.Frames))
	assert.Zero(t, slow.RTPLost, "dropped frames consume no sequence numbers")
	require.Positive(t, st.Resyncs, "the throttled player lost frames")
	assert.Equal(t, st.Resyncs, st.Mux.Resyncs)
	jumps := resumesAtKeys(t, r.col.frames, slow.Frames)
	assert.Positive(t, jumps, "the throttled player resumed after a gap")
	assert.LessOrEqual(t, uint64(jumps), st.Resyncs, "a gap only where the hub reported a loss")
	assert.Less(t, len(slow.Frames), len(normal.Frames), "the throttled player lost frames")
	t.Logf("hub %s; normal %s; throttled %s with %d gaps; play %+v",
		kindCounts(r.col.frames), kindCounts(normal.Frames), kindCounts(slow.Frames), jumps, st)
}

// TestM3b2BFrames plays h264_pyramid.ts (a B-pyramid, decode delay 3) to
// ffprobe over TCP: the video stream's time base is 1/90000 (the RTP
// clock), and from the first keyframe ffprobe receives on, every packet's
// pts equals the hub frame's PTS exactly, up to one offset (libavformat
// gives the first packet no pts, on any RTSP source, so the offset is taken
// at the first packet that has one).
// ffprobe's dts is libavformat's estimate (absent for the first packets):
// only checked increasing and at most the pts where present.
// conform.ReadRTSP, a second player, reads exactly the hub's frames, clean.
func TestM3b2BFrames(t *testing.T) {
	t.Parallel()
	ffprobe := testpeer.Require(t, "ffprobe")
	r := startRun(t, runOptions{inputs: []string{fixtureFile(t, "h264_pyramid.ts")}})
	o := serveRTSP(t, "", rtspplay.Options{}, map[string]*hub.Hub{"s": r.h})
	waitAV(t, r.h)
	capCh := readRTSP(o.url())
	type probed struct {
		timeBase string
		pkts     []rtspPacket
		err      error
	}
	probeCh := make(chan probed, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		tb, pkts, err := probeRTSPVideo(ctx, ffprobe, o.url())
		probeCh <- probed{tb, pkts, err}
	}()

	r.check(t, []uint32{1})
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	assert.Zero(t, c.RTPLost)
	assertRTSPMatchesHub(t, r.col.frames, c.Frames)
	pr := <-probeCh
	require.NoError(t, pr.err)
	assert.Equal(t, "1/90000", pr.timeBase)
	j0 := slices.IndexFunc(pr.pkts, func(p rtspPacket) bool { return p.key })
	require.GreaterOrEqual(t, j0, 0, "ffprobe received a keyframe")
	pkts := pr.pkts[j0:]
	hv, _ := byKind(r.col.frames, media.KindVideo)
	k := joinKey(t, hv, len(pkts))
	// libavformat's parser outputs the first access unit without a pts:
	// the RTSP demuxer's packets have no byte position, so the parser
	// finds no packet descriptor for the first frame. Only that packet may
	// lack one; the alignment is taken at the first packet with a pts.
	reordered, withDTS, aligned := 0, 0, false
	var prevDTS, off int64
	for i, pk := range pkts {
		f := hv[k+i]
		if !pk.hasPTS {
			require.Zero(t, j0+i, "packet %d has a pts", i)
		} else {
			if !aligned {
				off, aligned = pk.pts-(f.PTS-hv[k].PTS), true
			}
			require.Equal(t, f.PTS-hv[k].PTS, pk.pts-off, "packet %d pts", i)
		}
		require.Equal(t, f.Key, pk.key, "packet %d key", i)
		if f.PTS != f.DTS {
			reordered++
		}
		if !pk.hasDTS {
			continue
		}
		require.LessOrEqual(t, pk.dts, pk.pts, "packet %d dts at most its pts", i)
		if withDTS > 0 {
			require.Greater(t, pk.dts, prevDTS, "packet %d dts increasing", i)
		}
		prevDTS = pk.dts
		withDTS++
	}
	assert.Positive(t, reordered, "frames with a composition time")
	require.True(t, aligned, "ffprobe packets with a pts")
	t.Logf("hub %s; ffprobe %d video packets (%d before the first keyframe, first pts %v, keyframe at pts %d, %d with a dts) from hub video frame %d (%d reordered); read %s",
		kindCounts(r.col.frames), len(pr.pkts), j0, pkts[0].hasPTS, off, withDTS, k, reordered, kindCounts(c.Frames))
}

// TestM3b2MediaMTXPull has MediaMTX pull our RTSP play output on demand,
// over TCP, for H.264+AAC (path m3b2pull) and H.265+AAC (m3b2pullhevc), and
// our RTSP pull read it from MediaMTX over TCP: one epoch of
// violation-free frames with no offset jump. Each path is warmed with a
// throwaway reader and joined 1 s later, once the relay's sender-report
// mapping has settled (as TestM2cRTSPMediaMTXH264 does).
func TestM3b2MediaMTXPull(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTXPull(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	// The rows run one after the other, each with its own source and play
	// listener on the fixed port.
	for _, row := range []struct {
		path, fixture string
		video         media.Codec
	}{
		{"m3b2pull", "h264_aac.ts", media.CodecH264},
		{"m3b2pullhevc", "h265_aac51.ts", media.CodecH265},
	} {
		t.Run(row.path, func(t *testing.T) {
			clip := loopClip(t, ffmpeg, row.fixture, 4, 0)
			src := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
			o := serveRTSP(t, m3b2PullAddr, rtspplay.Options{}, map[string]*hub.Hub{row.path: src.h})
			waitAV(t, src.h)
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
			ps := o.srv.Stats()
			assert.Zero(t, ps.EndedCodecChange)
			assert.Zero(t, ps.EndedFrameError)
			t.Logf("read %s over %v; clock %+v; reader %v; play %+v; failures %v", r.col.counts(),
				r.col.span().Round(time.Millisecond), r.stats.Clock, st, ps, r.failures())
		})
	}
}
