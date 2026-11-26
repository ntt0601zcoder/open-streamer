package rtsp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp/rtsptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

const (
	testUser  = "admin"
	testPass  = "s3cr3t-pw"
	wrongPass = "wr0ng-pw"
)

var (
	fixtureOnce  sync.Once
	fixtureMedia []rtsptest.TrackSource
	fixtureErr   error
)

// splitAnnexB returns the NAL units of an Annex B byte stream.
func splitAnnexB(b []byte) [][]byte {
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

// loadFixture turns h264_aac.ts into an H.264 and an AAC track source.
func loadFixture() ([]rtsptest.TrackSource, error) {
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
			for _, n := range splitAnnexB(p.Data) {
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

func fixture(t *testing.T) []rtsptest.TrackSource {
	t.Helper()
	fixtureOnce.Do(func() { fixtureMedia, fixtureErr = loadFixture() })
	require.NoError(t, fixtureErr)
	return fixtureMedia
}

func newServer(t *testing.T, opt rtsptest.ServerOptions) *rtsptest.Server {
	t.Helper()
	if opt.Media == nil {
		opt.Media = fixture(t)
	}
	return rtsptest.NewServer(t, opt)
}

func testConfig() rtsp.Config {
	return rtsp.Config{Timeout: 2 * time.Second, IdleTimeout: time.Second}
}

func dial(t *testing.T, rawURL string, cfg rtsp.Config) *rtsp.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := rtsp.Dial(ctx, rawURL, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readFor reads packets until d has passed and returns them.
func readFor(t *testing.T, c *rtsp.Client, d time.Duration) []rtsp.Packet {
	t.Helper()
	var out []rtsp.Packet
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		p, err := c.ReadPacket()
		require.NoError(t, err)
		out = append(out, p)
	}
	return out
}

// readUntil reads packets until done returns true or d has passed.
func readUntil(t *testing.T, c *rtsp.Client, d time.Duration, done func(rtsp.Packet) bool) []rtsp.Packet {
	t.Helper()
	var out []rtsp.Packet
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		p, err := c.ReadPacket()
		require.NoError(t, err)
		out = append(out, p)
		if done(p) {
			return out
		}
	}
	t.Fatalf("condition not reached within %v (%d packets)", d, len(out))
	return nil
}

func rtpCount(ps []rtsp.Packet, track int) int {
	n := 0
	for _, p := range ps {
		if p.RTP != nil && p.Track == track {
			n++
		}
	}
	return n
}

func requestsOf(s *rtsptest.Server, method string) []rtsp.Request {
	var out []rtsp.Request
	for _, r := range s.Requests() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func TestClientPlayTCP(t *testing.T) {
	src := fixture(t)
	s := newServer(t, rtsptest.ServerOptions{SendRTPInfo: true, Seed: 1})
	c := dial(t, s.URL("/live/stream"), testConfig())

	d := c.Description()
	require.Len(t, d.Media, 2)
	assert.Equal(t, rtsp.CodecH264, d.Media[0].Codec)
	assert.Equal(t, rtsp.CodecAAC, d.Media[1].Codec)

	info := c.RTPInfo()
	require.Len(t, info, 2)
	for i := range info {
		assert.True(t, info[i].HasSeq && info[i].HasTime, "track %d", i)
		assert.Equal(t, d.Media[i].Control, info[i].URL)
	}

	video, err := rtsp.NewDepacketizer(d.Media[0], 0)
	require.NoError(t, err)
	var aus []rtsp.AU
	var seqs [2]rtsp.SeqTracker
	first := [2]bool{}
	pkts := readUntil(t, c, 3*time.Second, func(p rtsp.Packet) bool {
		if p.RTP == nil {
			return false
		}
		assert.False(t, p.Arrival.IsZero())
		if !first[p.Track] {
			first[p.Track] = true
			assert.Equal(t, info[p.Track].Seq, p.RTP.SequenceNumber, "track %d first seq", p.Track)
			assert.Equal(t, info[p.Track].RTPTime, p.RTP.Timestamp, "track %d first ts", p.Track)
		}
		gap, reset, dup := seqs[p.Track].Next(p.RTP.SequenceNumber, p.RTP.SSRC)
		assert.Zero(t, gap)
		assert.False(t, reset || dup)
		if p.Track == 0 {
			aus = append(aus, video.Push(p.RTP, p.Arrival)...)
		}
		return len(aus) >= 10
	})
	assert.Positive(t, rtpCount(pkts, 1), "audio packets")
	for i := range aus {
		want := src[0].AUs[i].NALUs
		require.Len(t, aus[i].NALUs, len(want), "au %d", i)
		for j := range want {
			assert.True(t, bytes.Equal(want[j], aus[i].NALUs[j]), "au %d nal %d", i, j)
		}
	}
	assert.Equal(t, rtsp.ClientStats{}, c.Stats())
}

func TestClientDigestAuth(t *testing.T) {
	for _, mode := range []string{"digest", "digest-noqop"} {
		t.Run(mode, func(t *testing.T) {
			s := newServer(t, rtsptest.ServerOptions{Auth: mode, User: testUser, Pass: testPass})
			u := strings.Replace(s.URL("/stream"), "rtsp://", "rtsp://"+testUser+":"+testPass+"@", 1)
			c := dial(t, u, testConfig())
			readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil && p.Track == 1 })
			reqs := s.Requests()
			require.NotEmpty(t, reqs)
			authed := 0
			for _, r := range reqs[1:] {
				a := r.Header.Get("Authorization")
				require.True(t, strings.HasPrefix(a, "Digest "), "%s without digest", r.Method)
				assert.Contains(t, a, `uri="`+r.URL+`"`)
				assert.Equal(t, mode == "digest", strings.Contains(a, "qop=auth"), a)
				authed++
			}
			assert.GreaterOrEqual(t, authed, 4)
			assert.Zero(t, c.Stats().Reauth)
		})
	}
}

func TestClientBasicAuth(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{Auth: "basic", User: testUser, Pass: testPass})
	u := strings.Replace(s.URL("/stream"), "rtsp://", "rtsp://"+testUser+":"+testPass+"@", 1)
	c := dial(t, u, testConfig())
	readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
	for _, r := range s.Requests()[1:] {
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Basic "), r.Method)
	}
}

func TestClientStaleNonce(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{
		Auth: "digest", User: testUser, Pass: testPass,
		NonceTTL: 300 * time.Millisecond, SessionTimeout: time.Second,
	})
	u := strings.Replace(s.URL("/stream"), "rtsp://", "rtsp://"+testUser+":"+testPass+"@", 1)
	c := dial(t, u, testConfig())
	start := time.Now()
	pkts := readFor(t, c, 1300*time.Millisecond)
	assert.GreaterOrEqual(t, c.Stats().Reauth, uint64(1))
	last := pkts[len(pkts)-1]
	assert.True(t, last.Arrival.After(start.Add(1200*time.Millisecond)), "stream continued")
	assert.NotEmpty(t, requestsOf(s, "GET_PARAMETER"))
}

func TestClientNoCredentialLeak(t *testing.T) {
	t.Run("wrong password", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{Auth: "digest", User: testUser, Pass: testPass})
		u := strings.Replace(s.URL("/stream"), "rtsp://", "rtsp://"+testUser+":"+wrongPass+"@", 1)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := rtsp.Dial(ctx, u, testConfig())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
		assert.NotContains(t, err.Error(), wrongPass)
		assert.NotContains(t, err.Error(), testUser)
		for _, r := range s.Requests() {
			assert.NotContains(t, r.URL, wrongPass)
			assert.NotContains(t, r.URL, "@")
		}
	})
	t.Run("content base with userinfo", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{
			Auth: "basic", User: testUser, Pass: testPass, ContentBaseUserinfo: true,
		})
		u := strings.Replace(s.URL("/stream"), "rtsp://", "rtsp://"+testUser+":"+testPass+"@", 1)
		c := dial(t, u, testConfig())
		readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
		for _, r := range s.Requests() {
			assert.NotContains(t, r.URL, testPass, r.Method)
			assert.NotContains(t, r.URL, "@", r.Method)
		}
		d := c.Description()
		assert.NotContains(t, d.Control, testPass)
		for _, m := range d.Media {
			assert.NotContains(t, m.Control, testPass)
		}
	})
}

func TestClientKeepalive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		noGP   bool
		method string
	}{{"get_parameter", false, "GET_PARAMETER"}, {"options", true, "OPTIONS"}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, rtsptest.ServerOptions{SessionTimeout: time.Second, NoGetParameter: tc.noGP})
			c := dial(t, s.URL("/stream"), testConfig())
			readFor(t, c, 1700*time.Millisecond)
			var keep []rtsp.Request
			for _, r := range requestsOf(s, tc.method) {
				if r.Header.Get("Session") != "" {
					keep = append(keep, r)
				}
			}
			assert.GreaterOrEqual(t, len(keep), 2)
			if tc.noGP {
				assert.Empty(t, requestsOf(s, "GET_PARAMETER"))
			}
			d := c.Description()
			for _, r := range keep {
				assert.Equal(t, d.Control, r.URL)
			}
		})
	}
}

func TestControlURLs(t *testing.T) {
	for _, tc := range []struct {
		style, path string
		setup       func(s *rtsptest.Server) []string
		play        func(s *rtsptest.Server) string
	}{
		{
			"hik", "/Streaming/Channels/101",
			func(s *rtsptest.Server) []string {
				return []string{s.URL("/Streaming/Channels/101/trackID=1"), s.URL("/Streaming/Channels/101/trackID=2")}
			},
			func(s *rtsptest.Server) string { return s.URL("/Streaming/Channels/101/") },
		},
		{
			"dahua", "/cam/realmonitor?channel=1&subtype=0",
			func(s *rtsptest.Server) []string {
				return []string{
					s.URL("/cam/realmonitor?channel=1&subtype=0/trackID=0"),
					s.URL("/cam/realmonitor?channel=1&subtype=0/trackID=1"),
				}
			},
			func(s *rtsptest.Server) string { return s.URL("/cam/realmonitor?channel=1&subtype=0/") },
		},
		{
			"absolute", "/live",
			func(s *rtsptest.Server) []string {
				return []string{s.URL("/media/track0"), s.URL("/media/track1")}
			},
			func(s *rtsptest.Server) string { return s.URL("/media") },
		},
	} {
		t.Run(tc.style, func(t *testing.T) {
			s := newServer(t, rtsptest.ServerOptions{ControlStyle: tc.style, SendRTPInfo: true})
			c := dial(t, s.URL(tc.path), testConfig())
			got := [2]bool{}
			readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool {
				if p.RTP != nil {
					got[p.Track] = true
				}
				return got[0] && got[1]
			})
			var setups []string
			for _, r := range requestsOf(s, "SETUP") {
				setups = append(setups, r.URL)
			}
			assert.Equal(t, tc.setup(s), setups)
			plays := requestsOf(s, "PLAY")
			require.Len(t, plays, 1)
			assert.Equal(t, tc.play(s), plays[0].URL)
			info := c.RTPInfo()
			require.Len(t, info, 2)
			assert.True(t, info[0].HasSeq && info[1].HasSeq)
		})
	}
}

func TestClientServerChannels(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{Channels: []int{10, 4}})
	cfg := testConfig()
	cfg.ReportInterval = 200 * time.Millisecond
	c := dial(t, s.URL("/stream"), cfg)
	got := [2]int{}
	readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool {
		if p.RTP != nil {
			got[p.Track]++
		}
		return got[0] > 5 && got[1] > 5
	})
	readFor(t, c, 300*time.Millisecond)
	chans := map[int]bool{}
	for _, f := range s.FromClient() {
		chans[f.Channel] = true
	}
	assert.True(t, chans[11] && chans[5], "receiver reports on the server's RTCP channels: %v", chans)
	assert.Zero(t, c.Stats().UnknownChannels)
}

func TestInterleavedResync(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{Garbage: true, SendRTPInfo: true})
	c := dial(t, s.URL("/stream"), testConfig())
	info := c.RTPInfo()
	var seqs [2]rtsp.SeqTracker
	first := [2]bool{}
	readUntil(t, c, 3*time.Second, func(p rtsp.Packet) bool {
		if p.RTP != nil {
			if !first[p.Track] {
				first[p.Track] = true
				// The frames sent before the PLAY response were kept.
				assert.Equal(t, info[p.Track].Seq, p.RTP.SequenceNumber)
			}
			gap, reset, _ := seqs[p.Track].Next(p.RTP.SequenceNumber, p.RTP.SSRC)
			assert.Zero(t, gap)
			assert.False(t, reset)
		}
		return c.Stats().ServerRequests >= 3 && rtpCount([]rtsp.Packet{p}, 0) == 1
	})
	readFor(t, c, 200*time.Millisecond)
	st := c.Stats()
	assert.GreaterOrEqual(t, st.Resyncs, uint64(2))
	assert.Equal(t, uint64(1), st.UnknownChannels)
	assert.Equal(t, uint64(3), st.ServerRequests)
	codes := map[string]int{}
	for _, r := range s.ClientResponses() {
		codes[r.Method] = r.StatusCode
	}
	assert.Equal(t, map[string]int{"GET_PARAMETER": 200, "SET_PARAMETER": 501, "ANNOUNCE": 501}, codes)
}

func TestClientReceiverReports(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{SRAfter: 100 * time.Millisecond})
	cfg := testConfig()
	cfg.ReportInterval = 250 * time.Millisecond
	c := dial(t, s.URL("/stream"), cfg)
	ssrc := map[int]uint32{}
	pkts := readFor(t, c, 900*time.Millisecond)
	srs := 0
	for _, p := range pkts {
		if p.RTP != nil {
			ssrc[p.Track] = p.RTP.SSRC
		}
		if p.RTCP != nil {
			srs += len(rtsp.ParseSenderReports(p.RTCP))
		}
	}
	assert.GreaterOrEqual(t, srs, 2)
	reports := map[int][]rtsp.ReportBlock{}
	for _, f := range s.FromClient() {
		require.Equal(t, 1, f.Channel%2, "RTCP on odd channels")
		b := f.Data
		require.GreaterOrEqual(t, len(b), 8+24)
		require.Equal(t, byte(201), b[1])
		blk := b[8:]
		reports[f.Channel/2] = append(reports[f.Channel/2], rtsp.ReportBlock{
			SSRC:       binary.BigEndian.Uint32(blk[0:]),
			HighestSeq: binary.BigEndian.Uint32(blk[8:]),
			LSR:        binary.BigEndian.Uint32(blk[16:]),
		})
	}
	for track := 0; track < 2; track++ {
		rs := reports[track]
		require.GreaterOrEqual(t, len(rs), 2, "track %d", track)
		last := rs[len(rs)-1]
		assert.Equal(t, ssrc[track], last.SSRC)
		assert.NotZero(t, last.LSR, "track %d LSR after a sender report", track)
		assert.GreaterOrEqual(t, last.HighestSeq, rs[0].HighestSeq)
	}
}

func TestClientSSRCChange(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{SSRCChangeAfter: 300 * time.Millisecond})
	cfg := testConfig()
	cfg.ReportInterval = 20 * time.Millisecond
	c := dial(t, s.URL("/stream"), cfg)
	seen := map[int][]uint32{}
	for _, p := range readFor(t, c, 700*time.Millisecond) {
		if p.RTP == nil {
			continue
		}
		if l := seen[p.Track]; len(l) == 0 || l[len(l)-1] != p.RTP.SSRC {
			seen[p.Track] = append(seen[p.Track], p.RTP.SSRC)
		}
	}
	time.Sleep(50 * time.Millisecond)
	last := map[int]uint32{}
	for _, f := range s.FromClient() {
		last[f.Channel/2] = binary.BigEndian.Uint32(f.Data[8:])
	}
	for track := 0; track < 2; track++ {
		require.Len(t, seen[track], 2, "track %d restarts once", track)
		assert.Equal(t, seen[track][1], last[track], "track %d reports the new SSRC", track)
	}
}

func TestClientKeepaliveStatus(t *testing.T) {
	t.Run("454 ends the session", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{SessionTimeout: time.Second, KeepaliveStatus: 454})
		c := dial(t, s.URL("/stream"), testConfig())
		errc := make(chan error, 1)
		go func() {
			for {
				if _, err := c.ReadPacket(); err != nil {
					errc <- err
					return
				}
			}
		}()
		select {
		case err := <-errc:
			var se *rtsp.StatusError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, 454, se.Code)
		case <-time.After(2 * time.Second):
			t.Fatal("454 keepalive did not end the session")
		}
	})
	t.Run("other errors are alive", func(t *testing.T) {
		s := newServer(t, rtsptest.ServerOptions{SessionTimeout: time.Second, KeepaliveStatus: 500})
		c := dial(t, s.URL("/stream"), testConfig())
		readFor(t, c, 1200*time.Millisecond)
		assert.GreaterOrEqual(t, len(requestsOf(s, "GET_PARAMETER")), 2)
	})
}

func TestClientTeardownOnClose(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{})
	c := dial(t, s.URL("/stream"), testConfig())
	readUntil(t, c, 2*time.Second, func(p rtsp.Packet) bool { return p.RTP != nil })
	start := time.Now()
	require.NoError(t, c.Close())
	assert.Less(t, time.Since(start), time.Second)
	require.Eventually(t, func() bool { return len(requestsOf(s, "TEARDOWN")) == 1 }, time.Second, 10*time.Millisecond)
	td := requestsOf(s, "TEARDOWN")[0]
	assert.NotEmpty(t, td.Header.Get("Session"))
	assert.Equal(t, c.Description().Control, td.URL)
	_, err := c.ReadPacket()
	require.Error(t, err)
	require.NoError(t, c.Close())
}

// fastSource is a video track of tiny one-packet AUs, one per millisecond,
// so the packet queue fills in about a second.
func fastSource() rtsptest.TrackSource {
	src := rtsptest.TrackSource{
		Codec: rtsp.CodecH264, ClockRate: 90000,
		ParamSets: [][]byte{{0x67, 0x42, 0x00, 0x1e}, {0x68, 0xce, 0x38, 0x80}},
	}
	for i := 0; i < 1000; i++ {
		src.AUs = append(src.AUs, rtsptest.TestAU{TS: uint32(i) * 90, NALUs: [][]byte{{0x41, 0x9a, byte(i)}}})
	}
	return src
}

func TestClientBackpressure(t *testing.T) {
	s := newServer(t, rtsptest.ServerOptions{Media: []rtsptest.TrackSource{fastSource()}, SessionTimeout: time.Second})
	cfg := rtsp.Config{Timeout: 500 * time.Millisecond, IdleTimeout: 300 * time.Millisecond}
	c := dial(t, s.URL("/stream"), cfg)
	// Not reading: the queue fills and the reader blocks, which must not
	// count as idle or as an unanswered keepalive.
	time.Sleep(1500 * time.Millisecond)
	var seq rtsp.SeqTracker
	for i := 0; i < 1500; i++ {
		p, err := c.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		if p.RTP == nil {
			continue
		}
		gap, reset, dup := seq.Next(p.RTP.SequenceNumber, p.RTP.SSRC)
		require.Zero(t, gap, "packet %d", i)
		require.False(t, reset || dup)
	}
	assert.Zero(t, c.Stats().Dropped)
}

func TestClientIdleTimeout(t *testing.T) {
	src := fixture(t)
	silent := src[0]
	silent.AUs = nil
	s := newServer(t, rtsptest.ServerOptions{Media: []rtsptest.TrackSource{silent}})
	cfg := testConfig()
	cfg.IdleTimeout = 300 * time.Millisecond
	c := dial(t, s.URL("/stream"), cfg)
	errc := make(chan error, 1)
	go func() {
		_, err := c.ReadPacket()
		errc <- err
	}()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, rtsp.ErrIdle)
	case <-time.After(2 * time.Second):
		t.Fatal("no idle error")
	}
}

func TestClientUnsupportedTransport(t *testing.T) {
	_, err := rtsp.Dial(context.Background(), "rtsp://127.0.0.1:1/x", rtsp.Config{Transport: "sctp"})
	require.ErrorContains(t, err, "unknown transport")
	_, err = rtsp.Dial(context.Background(), "http://127.0.0.1:1/x", rtsp.Config{})
	require.Error(t, err)
}

// rawServer accepts one connection, reads requests and answers each with
// reply(conn, request). It closes the connection when reply returns false.
func rawServer(t *testing.T, reply func(c net.Conn, r *rtsp.Request) bool) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var wg sync.WaitGroup
	conns := make(chan net.Conn, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conns <- c
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			r, err := rtsp.ReadRequest(br, rtsp.Limits{})
			if err != nil || !reply(c, r) {
				return
			}
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		select {
		case c := <-conns:
			c.Close()
		default:
		}
		wg.Wait()
	})
	return "rtsp://" + ln.Addr().String() + "/x"
}

func TestClientHostile(t *testing.T) {
	endless := func(c net.Conn, prefix string) {
		_, _ = c.Write([]byte(prefix))
		chunk := bytes.Repeat([]byte("A"), 4096)
		for {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	}
	for _, tc := range []struct {
		name  string
		reply func(c net.Conn, r *rtsp.Request) bool
	}{
		{"huge header", func(c net.Conn, r *rtsp.Request) bool {
			endless(c, "RTSP/1.0 200 OK\r\nCSeq: "+r.Header.Get("CSeq")+"\r\nX-Big: ")
			return false
		}},
		{"endless header lines", func(c net.Conn, r *rtsp.Request) bool {
			_, _ = c.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: " + r.Header.Get("CSeq") + "\r\n"))
			for {
				if _, err := c.Write([]byte("X-Header: value\r\n")); err != nil {
					return false
				}
			}
		}},
		{"endless body", func(c net.Conn, r *rtsp.Request) bool {
			endless(c, "RTSP/1.0 200 OK\r\nCSeq: "+r.Header.Get("CSeq")+"\r\nContent-Length: 999999999\r\n\r\n")
			return false
		}},
		{"oversize body", func(c net.Conn, r *rtsp.Request) bool {
			if r.Method == "OPTIONS" {
				_, _ = c.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: " + r.Header.Get("CSeq") + "\r\n\r\n"))
				return true
			}
			endless(c, "RTSP/1.0 200 OK\r\nCSeq: "+r.Header.Get("CSeq")+"\r\nContent-Length: 2000000\r\n\r\n")
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := rawServer(t, tc.reply)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			_, err := rtsp.Dial(ctx, u, testConfig())
			require.Error(t, err)
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			assert.Less(t, time.Since(start), 2*time.Second)
		})
	}
}

func TestClientCtxCancel(t *testing.T) {
	u := rawServer(t, func(net.Conn, *rtsp.Request) bool { return true }) // never answers
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := rtsp.Dial(ctx, u, rtsp.Config{Timeout: 5 * time.Second})
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second)
}

func TestClientRequestTimeout(t *testing.T) {
	u := rawServer(t, func(net.Conn, *rtsp.Request) bool { return true }) // never answers
	start := time.Now()
	_, err := rtsp.Dial(context.Background(), u, rtsp.Config{Timeout: 200 * time.Millisecond})
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestClientGoroutinesExit(t *testing.T) {
	src := fixture(t)
	before := runtime.NumGoroutine()

	s := rtsptest.NewServer(t, rtsptest.ServerOptions{Media: src, SessionTimeout: time.Second})
	cfg := testConfig()
	cfg.ReportInterval = 100 * time.Millisecond
	c, err := rtsp.Dial(context.Background(), s.URL("/stream"), cfg)
	require.NoError(t, err)
	readFor(t, c, 300*time.Millisecond)
	require.NoError(t, c.Close())

	// A client whose server goes away mid-stream.
	c2, err := rtsp.Dial(context.Background(), s.URL("/stream"), cfg)
	require.NoError(t, err)
	readFor(t, c2, 100*time.Millisecond)
	s.Close()
	for {
		if _, err := c2.ReadPacket(); err != nil {
			break
		}
	}
	require.NoError(t, c2.Close())

	// A client closed while its reader is blocked on a full queue.
	fast := rtsptest.NewServer(t, rtsptest.ServerOptions{Media: []rtsptest.TrackSource{fastSource()}})
	c3, err := rtsp.Dial(context.Background(), fast.URL("/stream"), cfg)
	require.NoError(t, err)
	time.Sleep(1300 * time.Millisecond)
	require.NoError(t, c3.Close())
	fast.Close()

	// A Dial that fails.
	u := rawServer(t, func(net.Conn, *rtsp.Request) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = rtsp.Dial(ctx, u, cfg)
	require.Error(t, err)
	// Polled by hand: require.Eventually runs its condition on a goroutine
	// of its own.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: before %d, now %d\n%s", before, n, stacks())
	}
}

func stacks() string {
	b := make([]byte, 1<<20)
	return string(b[:runtime.Stack(b, true)])
}

// bigSource is an H.264 track and an AAC track whose video access units are
// 40 KiB, larger than one TLS record (16 KiB).
func bigSource() []rtsptest.TrackSource {
	video := rtsptest.TrackSource{
		Codec: rtsp.CodecH264, ClockRate: 90000,
		ParamSets: [][]byte{{0x67, 0x42, 0x00, 0x1e}, {0x68, 0xce, 0x38, 0x80}},
	}
	audio := rtsptest.TrackSource{
		Codec: rtsp.CodecAAC, ClockRate: 48000,
		AAC: &rtsp.AACFormat{Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3},
	}
	for i := range 50 {
		nal := make([]byte, 40<<10)
		nal[0] = 0x41
		for j := 1; j < len(nal); j++ {
			nal[j] = byte(i*31 + j*7)
		}
		video.AUs = append(video.AUs, rtsptest.TestAU{TS: uint32(i) * 1800, NALUs: [][]byte{nal}})
		frame := bytes.Repeat([]byte{byte(i + 1)}, 300)
		audio.AUs = append(audio.AUs, rtsptest.TestAU{TS: uint32(i) * 1024, Frames: [][]byte{frame}})
	}
	return []rtsptest.TrackSource{video, audio}
}

// rtpOf is what two runs of one scripted session must agree on.
type rtpOf struct {
	Seq     uint16
	TS      uint32
	SSRC    uint32
	Marker  bool
	Payload string
}

// firstRTP reads until both tracks have n RTP packets and returns the first
// n of each.
func firstRTP(t *testing.T, c *rtsp.Client, n int) [2][]rtpOf {
	t.Helper()
	var out [2][]rtpOf
	readUntil(t, c, 5*time.Second, func(p rtsp.Packet) bool {
		if p.RTP != nil && len(out[p.Track]) < n {
			out[p.Track] = append(out[p.Track], rtpOf{
				Seq: p.RTP.SequenceNumber, TS: p.RTP.Timestamp, SSRC: p.RTP.SSRC, Marker: p.RTP.Marker,
				Payload: string(p.RTP.Payload),
			})
		}
		return len(out[0]) >= n && len(out[1]) >= n
	})
	return out
}

func TestDialTLS(t *testing.T) {
	ca := tlstest.NewCA(t)
	media := bigSource()
	opt := rtsptest.ServerOptions{Media: media, Seed: 7, SendRTPInfo: true}
	plain := newServer(t, opt)
	want := firstRTP(t, dial(t, plain.URL("/live/stream"), testConfig()), 80)

	secure := func(t *testing.T, opt rtsptest.ServerOptions) *rtsptest.Server {
		t.Helper()
		opt.TLS = ca.Server(t)
		s := newServer(t, opt)
		require.True(t, strings.HasPrefix(s.URL("/x"), "rtsps://"), s.URL("/x"))
		return s
	}
	withTLS := func(c *tls.Config) rtsp.Config {
		cfg := testConfig()
		cfg.TLS = c
		return cfg
	}
	dialErr := func(t *testing.T, rawURL string, cfg rtsp.Config) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := rtsp.Dial(ctx, rawURL, cfg)
		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err)
		return err
	}

	t.Run("plays the plain run's packets", func(t *testing.T) {
		s := secure(t, opt)
		trust := ca.Client()
		c := dial(t, s.URL("/live/stream"), withTLS(trust))
		assert.Equal(t, want, firstRTP(t, c, 80))
		assert.Equal(t, rtsp.ClientStats{}, c.Stats())
		assert.Empty(t, trust.ServerName, "the caller's config is cloned, not changed")
		for _, r := range s.Requests() {
			assert.True(t, strings.HasPrefix(r.URL, "rtsps://"), "%s %s", r.Method, r.URL)
		}
		require.NoError(t, c.Close())
	})

	t.Run("the server name is the URL's host", func(t *testing.T) {
		s := secure(t, opt)
		_, port, err := net.SplitHostPort(s.Addr)
		require.NoError(t, err)
		// Every name is dialled at the server's address: only the name
		// the certificate is checked against differs.
		cfg := withTLS(ca.Client())
		cfg.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, s.Addr)
		}
		c := dial(t, "rtsps://localhost:"+port+"/live/stream", cfg)
		firstRTP(t, c, 5)

		err = dialErr(t, "rtsps://cam.example:"+port+"/live/stream", cfg)
		var he x509.HostnameError
		require.ErrorAs(t, err, &he)
		assert.True(t, strings.HasPrefix(err.Error(), "rtsp: dial: tls: "), err.Error())
	})

	t.Run("an unknown authority is refused", func(t *testing.T) {
		s := secure(t, opt)
		err := dialErr(t, s.URL("/live/stream"), testConfig())
		var ua x509.UnknownAuthorityError
		require.ErrorAs(t, err, &ua)
		assert.True(t, strings.HasPrefix(err.Error(), "rtsp: dial: tls: "), err.Error())
		assert.Empty(t, requestsOf(s, "OPTIONS"), "nothing is sent to an unverified server")
	})

	t.Run("InsecureSkipVerify plays", func(t *testing.T) {
		s := secure(t, opt)
		c := dial(t, s.URL("/live/stream"), withTLS(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec // the opt-out under test
		firstRTP(t, c, 5)
	})

	t.Run("digest auth", func(t *testing.T) {
		o := opt
		o.Auth, o.User, o.Pass = "digest", testUser, testPass
		s := secure(t, o)
		c := dial(t, "rtsps://"+testUser+":"+testPass+"@"+s.Addr+"/live/stream", withTLS(ca.Client()))
		firstRTP(t, c, 5)
		for _, r := range s.Requests() {
			assert.NotContains(t, r.URL, testPass)
		}
		err := dialErr(t, "rtsps://"+testUser+":"+wrongPass+"@"+s.Addr+"/live/stream", withTLS(ca.Client()))
		var se *rtsp.StatusError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, 401, se.Code)
	})

	t.Run("udp is refused before any dial", func(t *testing.T) {
		cfg := withTLS(ca.Client())
		cfg.Transport = "udp"
		dials := 0
		cfg.Dial = func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("dialled")
		}
		err := dialErr(t, "rtsps://127.0.0.1:1/live/stream", cfg)
		assert.EqualError(t, err, "rtsp: dial: rtsps needs the tcp transport")
		assert.Zero(t, dials)
	})

	t.Run("auto plays over tcp", func(t *testing.T) {
		s := secure(t, opt)
		cfg := withTLS(ca.Client())
		cfg.Transport = "auto"
		c := dial(t, s.URL("/live/stream"), cfg)
		firstRTP(t, c, 5)
		assert.Zero(t, c.Stats().FallbackTCP)
		setups := requestsOf(s, "SETUP")
		require.Len(t, setups, 2, "one session, no UDP attempt")
		for _, r := range setups {
			assert.Contains(t, r.Header.Get("Transport"), "RTP/AVP/TCP")
		}
	})

	t.Run("a stalled handshake is bounded by the timeout", func(t *testing.T) {
		var lc net.ListenConfig
		l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
			}
		}()
		cfg := withTLS(ca.Client())
		cfg.Timeout = 200 * time.Millisecond
		start := time.Now()
		err = dialErr(t, "rtsps://"+l.Addr().String()+"/live/stream", cfg)
		assert.Less(t, time.Since(start), 2*time.Second)
		assert.True(t, strings.HasPrefix(err.Error(), "rtsp: dial: tls: "), err.Error())
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// tlsRecords follows one direction of a TLS byte stream and remembers the
// type of its last complete record. With TLS 1.2 the type is in clear: 21 is
// an alert, which close_notify is.
type tlsRecords struct {
	mu   sync.Mutex
	hdr  []byte
	left int
	last byte
	n    int
}

const tlsAlertRecord = 21

func (r *tlsRecords) feed(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(b) > 0 {
		if r.left > 0 {
			k := min(r.left, len(b))
			r.left -= k
			b = b[k:]
			if r.left == 0 {
				r.last = r.hdr[0]
				r.n++
				r.hdr = r.hdr[:0]
			}
			continue
		}
		k := min(5-len(r.hdr), len(b))
		r.hdr = append(r.hdr, b[:k]...)
		b = b[k:]
		if len(r.hdr) == 5 {
			if r.left = int(binary.BigEndian.Uint16(r.hdr[3:])); r.left == 0 {
				r.last = r.hdr[0]
				r.n++
				r.hdr = r.hdr[:0]
			}
		}
	}
}

// lastType returns the type of the last complete record and whether the
// stream ended on a record boundary.
func (r *tlsRecords) lastType() (typ byte, whole bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.n > 0 && r.left == 0 && len(r.hdr) == 0
}

// recordedConn feeds what is read from the connection to in and what is
// written to it to out.
type recordedConn struct {
	net.Conn
	in, out tlsRecords
}

func (c *recordedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.in.feed(b[:n])
	return n, err
}

func (c *recordedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.out.feed(b[:n])
	return n, err
}

// TestDialTLSCloseNotify checks the healthy side of the close rule: a client
// closed while its server reads sends close_notify, the last record it writes.
func TestDialTLSCloseNotify(t *testing.T) {
	ca := tlstest.NewCA(t)
	s := newServer(t, rtsptest.ServerOptions{TLS: ca.Server(t)})
	cfg := testConfig()
	cfg.TLS = ca.Client()
	cfg.TLS.MaxVersion = tls.VersionTLS12 // the record type is in clear
	var rec *recordedConn
	cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		rec = &recordedConn{Conn: c}
		return rec, nil
	}
	c := dial(t, s.URL("/live/stream"), cfg)
	readFor(t, c, 100*time.Millisecond)
	start := time.Now()
	require.NoError(t, c.Close())
	assert.Less(t, time.Since(start), time.Second)
	typ, whole := rec.out.lastType()
	assert.True(t, whole, "the stream ends on a record boundary")
	assert.EqualValues(t, tlsAlertRecord, typ, "the last record written is the close_notify alert")
	require.Len(t, requestsOf(s, "TEARDOWN"), 1)
}
