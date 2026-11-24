package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp/rtsptest"
)

const (
	rtspUser = "admin"
	rtspPass = "s3cr3t" + "-pw"
	rtspWrng = "wr0ng" + "-pw"
)

var (
	rtspFixOnce sync.Once
	rtspFixMed  []rtsptest.TrackSource
	rtspFixErr  error
)

func rtspSplitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// loadRTSPFixture turns h264_aac.ts into an H.264 and an AAC track source,
// both starting at RTP time 0.
func loadRTSPFixture() ([]rtsptest.TrackSource, error) {
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := mpegts.NewDemux(f)
	var videoPID, audioPID uint16
	video := rtsptest.TrackSource{Codec: rtsp.CodecH264, ClockRate: 90000}
	audio := rtsptest.TrackSource{Codec: rtsp.CodecAAC}
	var v0, a0 int64
	haveV, haveA := false, false
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				switch s.Type {
				case 0x1b:
					videoPID = s.PID
				case 0x0f:
					audioPID = s.PID
				}
			}
			continue
		}
		p := ev.PES
		switch {
		case p == nil:
		case p.PID == videoPID:
			if !haveV {
				v0, haveV = p.PTS, true
			}
			var nalus [][]byte
			for _, n := range rtspSplitAnnexB(p.Data) {
				switch n[0] & 0x1f {
				case 9:
					continue
				case 7, 8:
					if len(video.ParamSets) < 2 {
						video.ParamSets = append(video.ParamSets, n)
					}
				}
				nalus = append(nalus, n)
			}
			video.AUs = append(video.AUs, rtsptest.TestAU{TS: uint32(p.PTS - v0), NALUs: nalus})
		case p.PID == audioPID:
			frames, err := mpegts.ParseADTS(p.Data)
			if err != nil || len(frames) == 0 {
				continue
			}
			if !haveA {
				a0, haveA = p.PTS, true
				audio.ClockRate = uint32(frames[0].SampleRate)
				audio.AAC = &rtsp.AACFormat{
					Config: frames[0].AudioSpecificConfig(), SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
				}
			}
			au := rtsptest.TestAU{TS: uint32((p.PTS - a0) * int64(audio.ClockRate) / 90000)}
			for _, fr := range frames {
				au.Frames = append(au.Frames, fr.Payload)
			}
			audio.AUs = append(audio.AUs, au)
		}
	}
	if len(video.AUs) == 0 || len(audio.AUs) == 0 || len(video.ParamSets) < 2 {
		return nil, errors.New("fixture: missing tracks")
	}
	return []rtsptest.TrackSource{video, audio}, nil
}

func rtspFixture(t *testing.T) []rtsptest.TrackSource {
	t.Helper()
	rtspFixOnce.Do(func() { rtspFixMed, rtspFixErr = loadRTSPFixture() })
	require.NoError(t, rtspFixErr)
	return rtspFixMed
}

func newRTSPServer(t *testing.T, opt rtsptest.ServerOptions) *rtsptest.Server {
	t.Helper()
	if opt.Media == nil {
		opt.Media = rtspFixture(t)
	}
	return rtsptest.NewServer(t, opt)
}

// setRTSPSyncWait sets how long Open waits for sender reports until the test
// ends. Tests that use it must not run in parallel.
func setRTSPSyncWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := rtspSyncWait
	rtspSyncWait = d
	t.Cleanup(func() { rtspSyncWait = old })
}

func rtspOpenT(t *testing.T, rawURL string) Reader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, err := Open(ctx, rawURL, Env{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// rtspRead is what readRTSP collected.
type rtspRead struct {
	frames []Frame
	breaks []int // len(frames) at each ErrDiscontinuity
	err    error // the error that ended the read, nil when n frames came
}

// readRTSP reads until video video frames have come (or Read fails).
func readRTSP(t *testing.T, r Reader, video int) rtspRead {
	t.Helper()
	var out rtspRead
	nv := 0
	deadline := time.Now().Add(15 * time.Second)
	for nv < video {
		require.True(t, time.Now().Before(deadline), "no %d video frames in time (%d frames)", video, len(out.frames))
		f, err := r.Read()
		switch {
		case errors.Is(err, ErrDiscontinuity):
			out.breaks = append(out.breaks, len(out.frames))
		case err != nil:
			out.err = err
			return out
		default:
			out.frames = append(out.frames, f)
			if f.Kind() == media.KindVideo {
				nv++
			}
		}
	}
	return out
}

func checkRTSPFrames(t *testing.T, frames []Frame) {
	t.Helper()
	last := map[int]int64{}
	kinds := map[media.Kind]int{}
	for _, f := range frames {
		kinds[f.Kind()]++
		assert.False(t, f.Arrival.IsZero())
		assert.Zero(t, f.Epoch)
		assert.LessOrEqual(t, f.DTS, f.PTS)
		if d, ok := last[f.Track]; ok {
			assert.GreaterOrEqual(t, f.DTS, d, "track %d DTS decreases", f.Track)
		}
		last[f.Track] = f.DTS
		for _, b := range f.Data {
			assert.False(t, bytes.HasPrefix(b, []byte{0, 0, 0, 1}), "no start codes")
		}
	}
	assert.Positive(t, kinds[media.KindVideo])
	assert.Positive(t, kinds[media.KindAudio])
}

func TestRTSPPullTCP(t *testing.T) {
	s := newRTSPServer(t, rtsptest.ServerOptions{SendRTPInfo: true, SRAfter: 100 * time.Millisecond, Seed: 1})
	r := rtspOpenT(t, s.URL("/stream"))
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	assert.Equal(t, Spec{Live: true}, r.Spec())

	got := readRTSP(t, r, 40)
	require.NoError(t, got.err)
	checkRTSPFrames(t, got.frames)
	assert.Empty(t, got.breaks, "RTP-Info and SRs agree: no discontinuity")

	st := r.Stats()
	assert.Positive(t, st["frames"])
	assert.Zero(t, st["lost_packets"])
	assert.Zero(t, st["lost_aus"])
	assert.Zero(t, st["sync_by_arrival"])
	assert.Zero(t, st["sync_corrected"])
	assert.Zero(t, st["skipped_tracks"])
	require.NoError(t, r.Close())
	require.NoError(t, r.Close())
}

func TestRTSPPullUDP(t *testing.T) {
	s := newRTSPServer(t, rtsptest.ServerOptions{SRAfter: 100 * time.Millisecond, Seed: 2})
	r := rtspOpenT(t, s.URL("/stream?transport=udp"))
	got := readRTSP(t, r, 30)
	require.NoError(t, got.err)
	checkRTSPFrames(t, got.frames)
	require.NotEmpty(t, s.Requests())
	var setups int
	for _, req := range s.Requests() {
		if req.Method == "SETUP" {
			setups++
			assert.Contains(t, req.Header.Get("Transport"), "client_port")
		}
	}
	assert.Equal(t, 2, setups)
}

func TestRTSPPullUDPLoss(t *testing.T) {
	s := newRTSPServer(t, rtsptest.ServerOptions{
		UDPLoss: 0.03, UDPSwap: 0.02, UDPDup: 0.02, SRAfter: 100 * time.Millisecond, Seed: 3,
	})
	r := rtspOpenT(t, s.URL("/stream?transport=udp"))
	got := readRTSP(t, r, 60)
	require.NoError(t, got.err)
	lost := 0
	for _, in := range s.Injected() {
		if in.Kind == rtsptest.InjectLoss {
			lost++
		}
	}
	require.Positive(t, lost)
	st := r.Stats()
	assert.Positive(t, st["lost_packets"])
	assert.LessOrEqual(t, st["lost_packets"], uint64(lost))
	assert.Positive(t, st["duplicate_packets"])
	assert.Positive(t, st["lost_aus"])
}

// Review Focus 5 end to end: random RTP bases per track and no RTP-Info.
// With the sender reports inside the wait, the tracks are aligned from the
// first frame within one audio frame. With the wait cut short the tracks
// are placed by arrival, the first SR corrects them by at most one
// discontinuity, and what follows is aligned.
func TestRTSPSyncBySR(t *testing.T) {
	src := rtspFixture(t)
	// The PTS of the i-th video AU and the j-th AAC frame, in 90 kHz source time.
	vts := make([]int64, 0, len(src[0].AUs))
	var ats []int64
	for _, au := range src[0].AUs {
		vts = append(vts, int64(au.TS))
	}
	spa := int64(1024) * 90000 / int64(src[1].ClockRate)
	for _, au := range src[1].AUs {
		for k := range au.Frames {
			ats = append(ats, int64(au.TS)*90000/int64(src[1].ClockRate)+int64(k)*spa)
		}
	}
	// offsets returns the track offsets (output PTS minus source PTS) of the
	// frames from index from on, in the first loop of the source.
	offsets := func(frames []Frame, from int) (v, a []int64) {
		nv, na := 0, 0
		for i, f := range frames {
			switch f.Kind() {
			case media.KindVideo:
				if nv < len(vts) && i >= from {
					v = append(v, f.PTS-vts[nv])
				}
				nv++
			case media.KindAudio:
				if na < len(ats) && i >= from {
					a = append(a, f.PTS-ats[na])
				}
				na++
			}
		}
		return v, a
	}
	spread := func(x, y []int64) int64 {
		require.NotEmpty(t, x)
		require.NotEmpty(t, y)
		return absInt(x[len(x)-1] - y[len(y)-1])
	}

	t.Run("sender reports within the wait", func(t *testing.T) {
		setRTSPSyncWait(t, time.Second)
		s := newRTSPServer(t, rtsptest.ServerOptions{SRAfter: 150 * time.Millisecond, Seed: 4})
		r := rtspOpenT(t, s.URL("/stream"))
		got := readRTSP(t, r, 60)
		require.NoError(t, got.err)
		assert.Empty(t, got.breaks)
		v, a := offsets(got.frames, 0)
		require.NotEmpty(t, v)
		require.NotEmpty(t, a)
		require.LessOrEqual(t, spread(v, a), spa)
		assert.Zero(t, r.Stats()["sync_corrected"])
	})
	t.Run("sender reports after the wait", func(t *testing.T) {
		// SR rounds every 500 ms: a correction needs two fresh pairs (1 s),
		// inside the 80 video frames (3.2 s) read.
		setRTSPSyncWait(t, 0)
		s := newRTSPServer(t, rtsptest.ServerOptions{SRAfter: 500 * time.Millisecond, Seed: 5})
		r := rtspOpenT(t, s.URL("/stream"))
		got := readRTSP(t, r, 80)
		require.NoError(t, got.err)
		st := r.Stats()
		assert.Positive(t, st["sync_by_arrival"])
		assert.LessOrEqual(t, len(got.breaks), 1)
		assert.Equal(t, uint64(len(got.breaks)), st["sync_corrected"]+st["sync_jumps"])
		from := 0
		if len(got.breaks) > 0 {
			from = got.breaks[0]
		}
		v, a := offsets(got.frames, from)
		require.NotEmpty(t, v)
		require.NotEmpty(t, a)
		require.LessOrEqual(t, spread(v, a), spa, "aligned after the first SR")
	})
	t.Run("an SR that jumps by 5 s", func(t *testing.T) {
		// SR rounds every 150 ms: the first places the tracks, the second
		// (the jump) is a candidate, the third confirms it, about 450 ms
		// after PLAY, well inside the 60 video frames (2.4 s) read.
		const period = 150 * time.Millisecond
		setRTSPSyncWait(t, time.Second)
		s := newRTSPServer(t, rtsptest.ServerOptions{SRAfter: period, SRJump: 5 * time.Second, Seed: 6})
		r := rtspOpenT(t, s.URL("/stream"))
		got := readRTSP(t, r, 60)
		require.NoError(t, got.err)
		require.Len(t, got.breaks, 1)
		st := r.Stats()
		assert.Equal(t, uint64(1), st["sync_jumps"])
		assert.Zero(t, st["sync_unconfirmed"])
		require.Less(t, got.breaks[0], len(got.frames))
		at := got.frames[got.breaks[0]].Arrival.Sub(got.frames[0].Arrival)
		assert.GreaterOrEqual(t, at, 2*period-50*time.Millisecond, "the jump applies once a second fresh pair confirms it")
	})
}

func TestRTSPPullAuthRedacted(t *testing.T) {
	s := newRTSPServer(t, rtsptest.ServerOptions{Auth: "digest", User: rtspUser, Pass: rtspPass, Seed: 7})
	u := "rtsp://" + rtspUser + ":" + rtspPass + "@" + s.Addr + "/stream"
	r := rtspOpenT(t, u)
	got := readRTSP(t, r, 5)
	require.NoError(t, got.err)
	for _, req := range s.Requests() {
		assert.NotContains(t, req.URL, rtspPass)
		assert.NotContains(t, req.URL, "@")
	}

	bad := "rtsp://" + rtspUser + ":" + rtspWrng + "@" + s.Addr + "/stream"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Open(ctx, bad, Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), rtspWrng)
	assert.NotContains(t, err.Error(), rtspPass)
	assert.Contains(t, err.Error(), "ingest: rtsp: open")
}

func TestRTSPPullUnsupportedTrack(t *testing.T) {
	src := rtspFixture(t)
	pcma := rtsptest.TrackSource{Codec: rtsp.CodecOther, ClockRate: 8000}
	for i := range 50 {
		pcma.AUs = append(pcma.AUs, rtsptest.TestAU{TS: uint32(i) * 160, Frames: [][]byte{make([]byte, 160)}})
	}
	s := newRTSPServer(t, rtsptest.ServerOptions{Media: []rtsptest.TrackSource{src[0], pcma}, Seed: 8})
	r := rtspOpenT(t, s.URL("/stream"))
	assert.Equal(t, []media.Kind{media.KindVideo, 0}, r.Kinds())
	got := readRTSP(t, r, 20)
	require.NoError(t, got.err)
	for _, f := range got.frames {
		assert.Equal(t, media.KindVideo, f.Kind())
	}
	st := r.Stats()
	assert.Equal(t, uint64(1), st["skipped_tracks"])
	var reason string
	for k := range st {
		if strings.HasPrefix(k, "skipped_track_1_") {
			reason = k
		}
	}
	assert.Equal(t, "skipped_track_1_pcma_not_supported", reason)
}

func TestRTSPPullNoTrack(t *testing.T) {
	pcma := rtsptest.TrackSource{
		Codec: rtsp.CodecOther, ClockRate: 8000,
		AUs: []rtsptest.TestAU{{Frames: [][]byte{make([]byte, 160)}}},
	}
	s := newRTSPServer(t, rtsptest.ServerOptions{Media: []rtsptest.TrackSource{pcma}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Open(ctx, s.URL("/stream"), Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supported")
}

func TestRTSPPullCloseUnblocks(t *testing.T) {
	setRTSPSyncWait(t, 100*time.Millisecond)
	s := newRTSPServer(t, rtsptest.ServerOptions{
		UDPGapAfter: 200 * time.Millisecond, UDPGap: 20 * time.Second, Seed: 9,
	})
	r := rtspOpenT(t, s.URL("/stream?transport=udp"))
	errc := make(chan error, 1)
	go func() {
		for {
			if _, err := r.Read(); err != nil && !errors.Is(err, ErrDiscontinuity) {
				errc <- err
				return
			}
		}
	}()
	time.Sleep(600 * time.Millisecond)
	require.NoError(t, r.Close())
	select {
	case err := <-errc:
		require.Error(t, err)
		assert.False(t, errors.Is(err, io.EOF))
	case <-time.After(3 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}

// The server ending the session ends the pull cleanly, over TCP and over
// UDP (where the client first reads its sockets dry).
func TestRTSPPullServerEnds(t *testing.T) {
	for _, path := range []string{"/stream", "/stream?transport=udp"} {
		t.Run(path, func(t *testing.T) {
			s := newRTSPServer(t, rtsptest.ServerOptions{Seed: 10})
			r := rtspOpenT(t, s.URL(path))
			got := readRTSP(t, r, 10)
			require.NoError(t, got.err)
			s.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := r.Read()
				if err == nil || errors.Is(err, ErrDiscontinuity) {
					require.True(t, time.Now().Before(deadline), "the stream did not end")
					continue
				}
				assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
				return
			}
		})
	}
}

func TestRTSPPullSSRCChange(t *testing.T) {
	s := newRTSPServer(t, rtsptest.ServerOptions{SSRCChangeAfter: 700 * time.Millisecond, SendRTPInfo: true, Seed: 11})
	r := rtspOpenT(t, s.URL("/stream"))
	got := readRTSP(t, r, 70)
	require.NoError(t, got.err)
	st := r.Stats()
	assert.Equal(t, uint64(2), st["restarts"])
	assert.Equal(t, uint64(2), st["resets"])
	assert.Len(t, got.breaks, 1, "the restarts of both tracks give one discontinuity")
	assert.Zero(t, st["lost_packets"])
	// A restart is not a loss: only the old pending AU may be lost, never
	// the new stream's first one (the IDR with in-band sets).
	assert.LessOrEqual(t, st["lost_aus"], uint64(1))
	assert.Equal(t, uint64(1), st["restart_dropped_aus"], "the second track's old pending AU")
	require.Len(t, got.breaks, 1)
	firstVideo := false
	for _, f := range got.frames[got.breaks[0]:] {
		if f.Kind() == media.KindVideo {
			firstVideo = true
			break
		}
	}
	assert.True(t, firstVideo, "the new stream's first video AU is emitted")
}

// restartPull builds an rtspPull of an AAC track 0 and an H.264 track 1
// without a client, for driving handleRTP with chosen packets.
type restartPull struct {
	r        *rtspPull
	src      []rtsptest.TrackSource
	vseq     uint16
	aseq     uint16
	h264     codecs.H264Payloader
	arrival  time.Time
	vi, ai   int
	vts, ats uint32
}

func newRestartPull(t *testing.T) *restartPull {
	t.Helper()
	src := rtspFixture(t)
	desc := rtsp.Description{Media: []rtsp.Media{
		{Index: 0, Codec: rtsp.CodecAAC, Payload: 97, ClockRate: src[1].ClockRate, AAC: src[1].AAC},
		{Index: 1, Codec: rtsp.CodecH264, Payload: 96, ClockRate: 90000, ParamSets: src[0].ParamSets},
	}}
	r := &rtspPull{frames: newRTPFrames(desc, nil, &media.ConfigCache{}), restarted: map[int]bool{}}
	r.tracks = make([]*rtspTrack, 2)
	for _, m := range desc.Media {
		d, err := rtsp.NewDepacketizer(m, 0)
		require.NoError(t, err)
		r.tracks[m.Index] = &rtspTrack{dep: d}
	}
	return &restartPull{r: r, src: src, arrival: rtpT0}
}

// video sends the next video AU of the fixture with ssrc.
func (p *restartPull) video(ssrc uint32) {
	au := p.src[0].AUs[p.vi%len(p.src[0].AUs)]
	p.vi++
	var ab []byte
	for _, n := range au.NALUs {
		ab = append(ab, 0, 0, 0, 1)
		ab = append(ab, n...)
	}
	pls := p.h264.Payload(1200, ab)
	for j, pl := range pls {
		p.arrival = p.arrival.Add(time.Millisecond)
		p.r.handleRTP(rtsp.Packet{Track: 1, Arrival: p.arrival, RTP: &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: p.vseq, Timestamp: p.vts, SSRC: ssrc, Marker: j == len(pls)-1},
			Payload: pl,
		}})
		p.vseq++
	}
	p.vts += 3600
}

// audio sends the next AAC AU with ssrc.
func (p *restartPull) audio(ssrc uint32) {
	au := p.src[1].AUs[p.ai%len(p.src[1].AUs)]
	p.ai++
	for _, pk := range rtsp.PacketizeAAC(au.Frames, p.ats, 1024, 1200, *p.src[1].AAC) {
		pk.SequenceNumber, pk.SSRC, pk.PayloadType = p.aseq, ssrc, 97
		p.aseq++
		p.arrival = p.arrival.Add(time.Millisecond)
		p.r.handleRTP(rtsp.Packet{Track: 0, Arrival: p.arrival, RTP: pk})
	}
	p.ats += 1024
}

func (p *restartPull) videoFrames() int {
	n := 0
	for _, e := range p.r.events {
		if !e.disc && e.frame.Kind() == media.KindVideo {
			n++
		}
	}
	return n
}

// A track restarting alone, followed by normal flow, ends its restart
// episode (here: by time): a later restart of the other track is a separate one and its old
// pending AU is emitted, not dropped. Without the flow in between, the
// second track joins the episode and drops it.
func TestRTSPRestartEpisodes(t *testing.T) {
	run := func(t *testing.T, flowBetween bool) (dropped uint64, videoBefore, videoAfter int) {
		p := newRestartPull(t)
		for range 3 {
			p.video(100)
			p.audio(200)
		}
		p.audio(201) // the audio track restarts alone
		if flowBetween {
			p.arrival = p.arrival.Add(2 * time.Second)
			for range 4 {
				p.audio(201)
			}
		}
		videoBefore = p.videoFrames()
		p.video(101) // the video track restarts: the AU pending since the old stream
		videoAfter = p.videoFrames()
		return p.r.n.restartDropped.Load(), videoBefore, videoAfter
	}
	dropped, before, after := run(t, true)
	assert.Zero(t, dropped)
	assert.Equal(t, before+1, after, "the old pending AU is emitted")
	dropped, before, after = run(t, false)
	assert.Equal(t, uint64(1), dropped)
	assert.Equal(t, before, after, "it joins the audio restart and is dropped")
}

func TestRTSPParseTransportTwice(t *testing.T) {
	_, err := Parse("rtsp://cam/s?transport=udp&transport=tcp")
	require.Error(t, err)
	_, err = Parse("rtsp://cam/s?transport=%75dp")
	require.NoError(t, err)
	d, tr := rtspDialURL("rtsp://cam/s?a=1&transport=%75dp&b=2")
	assert.Equal(t, "rtsp://cam/s?a=1&b=2", d)
	assert.Equal(t, "udp", tr)
}

func TestOpenRTSP(t *testing.T) {
	for _, u := range []string{
		"rtsp://cam/stream",
		"rtsp://cam:8554/stream",
		"rtsp://u:p@cam/stream?transport=udp",
		"rtsp://cam/stream?transport=auto&channel=1",
		"rtsp://cam/stream?transport=TCP",
		"RTSP://cam:554/a",
	} {
		spec, err := Parse(u)
		require.NoError(t, err, u)
		assert.Equal(t, Spec{Live: true}, spec, u)
	}
	for _, u := range []string{
		"rtsp://", "rtsp:///stream", "rtsp://cam:x/stream", "rtsp://cam:0/stream",
		"rtsp://cam/stream?transport=sctp", "rtsp://cam/stream?transport=",
	} {
		_, err := Parse(u)
		assert.Error(t, err, u)
	}
	_, err := Parse("rtsps://u:" + rtspPass + "@cam/stream")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rtsps is not supported")
	assert.NotContains(t, err.Error(), rtspPass)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = Open(ctx, "rtsps://cam/stream", Env{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rtsps is not supported")

	t.Run("transport removed from the request URL", func(t *testing.T) {
		s := newRTSPServer(t, rtsptest.ServerOptions{Seed: 12})
		r := rtspOpenT(t, s.URL("/stream?transport=tcp&ch=1&subtype=0"))
		got := readRTSP(t, r, 3)
		require.NoError(t, got.err)
		reqs := s.Requests()
		require.NotEmpty(t, reqs)
		for _, req := range reqs {
			assert.NotContains(t, req.URL, "transport", req.Method)
			assert.Contains(t, req.URL, "ch=1&subtype=0", req.Method)
		}
	})
	t.Run("port 554 is implied", func(t *testing.T) {
		// A unit check: no connection is made. The URL handed to the
		// client keeps no port, and pkg/rtsp dials 554 for it
		// (TestNewTargetDefaultPort).
		dial, tr := rtspDialURL("rtsp://127.0.0.1/stream?transport=udp")
		assert.Equal(t, "rtsp://127.0.0.1/stream", dial)
		assert.Equal(t, "udp", tr)
		_, err := Parse("rtsp://127.0.0.1/stream")
		require.NoError(t, err)
	})
}

// TestRTSPPullClientStats checks that the depacketizers' oversize counts and
// the client's unknown-channel and server-request counts reach Stats.
func TestRTSPPullClientStats(t *testing.T) {
	src := rtspFixture(t)
	med := append([]rtsptest.TrackSource(nil), src...)
	video := med[0]
	video.AUs = append([]rtsptest.TestAU(nil), video.AUs...)
	// One AU with more NAL units than an AU may hold.
	flood := append([][]byte(nil), video.AUs[5].NALUs...)
	for range 4100 {
		flood = append(flood, []byte{0x06, 0x80})
	}
	video.AUs[5].NALUs = flood
	med[0] = video
	s := newRTSPServer(t, rtsptest.ServerOptions{Media: med, Garbage: true, SendRTPInfo: true, Seed: 4})
	r := rtspOpenT(t, s.URL("/stream"))
	got := readRTSP(t, r, 60)
	require.NoError(t, got.err)
	st := r.Stats()
	assert.GreaterOrEqual(t, st["oversize_aus"], uint64(1))
	assert.Positive(t, st["unknown_channels"])
	assert.Equal(t, uint64(3), st["server_requests"])
}
