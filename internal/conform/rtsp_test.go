package conform

import (
	"context"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	rtpmux "github.com/ntt0601zcoder/open-streamer/internal/mux/rtp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// rtspFaults are the defects the test handler injects.
type rtspFaults struct {
	dropSPSAt   int           // drop the SPS packets of this keyframe (1-based); 0 none
	clearMarker bool          // clear the marker bit of video packets
	noSR        bool          // send no sender reports
	skewSR      time.Duration // add to the NTP of the audio track's sender reports
	startDelta  bool          // send a delta frame before the first keyframe
	mtu         int
	pace        time.Duration // wait after each frame; 0 means 1 ms
}

// rtspHandler plays a clip through our RTP muxer.
type rtspHandler struct {
	frames []media.Frame
	f      rtspFaults
	descs  []rtsp.TrackDesc
}

func newRTSPHandler(t *testing.T, name string, f rtspFaults) *rtspHandler {
	t.Helper()
	var frames []media.Frame
	for _, fr := range streamClip(t, name) {
		if len(frames) == 0 && (fr.Kind() != media.KindVideo || !fr.Key) {
			continue
		}
		frames = append(frames, fr)
	}
	var cfgs []hub.TrackConfig
	for _, fr := range frames {
		if !slices.ContainsFunc(cfgs, func(c hub.TrackConfig) bool { return c.Track == fr.Track }) {
			cfgs = append(cfgs, hub.TrackConfig{Track: fr.Track, Kind: fr.Kind(), Config: fr.Config})
		}
	}
	descs, err := rtpmux.Tracks(cfgs)
	require.NoError(t, err)
	return &rtspHandler{frames: frames, f: f, descs: descs}
}

func (h *rtspHandler) Describe(path string) ([]rtsp.TrackDesc, bool) {
	return h.descs, path == "live/s"
}

func hasSet(p *rtp.Packet, codec rtsp.Codec) bool {
	if len(p.Payload) == 0 {
		return false
	}
	if codec == rtsp.CodecH265 {
		return (p.Payload[0]>>1)&0x3f == 33
	}
	return p.Payload[0]&0x1f == 7
}

func (h *rtspHandler) Play(ctx context.Context, s *rtsp.ServerSession) {
	setup := s.Tracks()
	trs := make([]rtpmux.Track, 0, len(h.descs))
	for i, d := range h.descs {
		ssrc, _, base := s.Origin(i)
		trs = append(trs, rtpmux.Track{
			SDPIndex: i, HubTrack: d.ID, Desc: d, Setup: slices.Contains(setup, i), SSRC: ssrc, TSBase: base,
		})
	}
	var clk time.Time
	m := rtpmux.NewMuxer(trs, h.f.mtu, func() time.Time { return clk })
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := h.frames[0].DTS
	keys := 0
	write := func(idx int, ps []*rtp.Packet) bool {
		return s.WritePackets(idx, ps) == nil
	}
	if h.f.startDelta {
		_, _, tsb := s.Origin(0)
		var delta media.Frame
		for _, fr := range h.frames {
			if fr.Kind() == media.KindVideo && !fr.Key {
				delta = fr
				break
			}
		}
		var pk rtsp.Packetizer
		if h.descs[0].Codec == rtsp.CodecH265 {
			pk = rtsp.NewH265Packetizer(1200)
		} else {
			pk = rtsp.NewH264Packetizer(1200)
		}
		pls := pk.Packetize(delta.Data)
		ps := make([]*rtp.Packet, 0, len(pls))
		for _, pl := range pls {
			ps = append(ps, &rtp.Packet{Header: rtp.Header{Timestamp: tsb - 3600}, Payload: pl})
		}
		ps[len(ps)-1].Marker = true
		if !write(0, ps) {
			return
		}
	}
	for _, fr := range h.frames {
		if ctx.Err() != nil {
			return
		}
		clk = t0.Add(time.Duration(fr.DTS-base) * time.Second / media.ClockRate)
		p, reps, err := m.Frame(fr)
		if err != nil {
			return
		}
		if p != nil {
			if fr.Kind() == media.KindVideo && fr.Key {
				keys++
			}
			ps := p.RTP
			if p.SDPIndex == 0 {
				if h.f.dropSPSAt > 0 && keys == h.f.dropSPSAt {
					ps = slices.DeleteFunc(slices.Clone(ps), func(x *rtp.Packet) bool { return hasSet(x, h.descs[0].Codec) })
				}
				if h.f.clearMarker {
					for _, x := range ps {
						x.Marker = false
					}
				}
			}
			if !write(p.SDPIndex, ps) {
				return
			}
		}
		for _, r := range reps {
			if h.f.noSR {
				continue
			}
			b := slices.Clone(r.RTCP)
			if h.f.skewSR != 0 && r.SDPIndex == 1 {
				sec := binary.BigEndian.Uint32(b[8:])
				binary.BigEndian.PutUint32(b[8:], sec+uint32(h.f.skewSR/time.Second))
			}
			if s.WriteRTCP(r.SDPIndex, b) != nil {
				return
			}
		}
		pace := h.f.pace
		if pace == 0 {
			pace = time.Millisecond
		}
		time.Sleep(pace)
	}
	s.Close()
}

// startRTSP serves h and returns the base URL (no query).
func startRTSP(t *testing.T, h rtsp.Handler) string {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := rtsp.NewServer(h, rtsp.ServerConfig{UDPHost: "127.0.0.1"})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); wg.Wait() })
	return "rtsp://" + l.Addr().String() + "/live/s"
}

func readRTSP(t *testing.T, u string, opt ReadOptions) Capture {
	t.Helper()
	if opt.Timeout == 0 {
		opt.Timeout = rtmpWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*rtmpWait)
	defer cancel()
	c, err := ReadRTSP(ctx, u, opt)
	require.NoError(t, err)
	return c
}

func TestReadRTSPClean(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h265_aac51.ts"} {
		for _, q := range []string{"", "?transport=udp"} {
			t.Run(name+q, func(t *testing.T) {
				h := newRTSPHandler(t, name, rtspFaults{})
				c := readRTSP(t, startRTSP(t, h)+q, ReadOptions{})
				assert.Empty(t, c.Violations)
				var src, got []media.Frame
				for _, f := range h.frames {
					if f.Kind() == media.KindVideo {
						src = append(src, f)
					}
				}
				for _, f := range c.Frames {
					if f.Kind() == media.KindVideo {
						got = append(got, f)
					}
					assert.Equal(t, uint32(1), f.Epoch)
				}
				assert.Positive(t, countKind(c.Frames, media.KindAudio))
				require.Len(t, got, len(src))
				b := src[0].DTS
				for i, f := range got {
					assert.True(t, src[i].Config.Equal(f.Config), "frame %d config", i)
					assert.Equal(t, src[i].Data, f.Data, "frame %d data", i)
					assert.Equal(t, src[i].Key, f.Key, "frame %d key", i)
					assert.Equal(t, src[i].PTS-b, f.PTS, "frame %d PTS", i)
				}
			})
		}
	}
}

func ruleNames(c Capture) []string {
	out := make([]string, 0, len(c.Violations))
	for _, v := range c.Violations {
		out = append(out, v.Rule)
	}
	return out
}

func TestReadRTSPViolations(t *testing.T) {
	cases := []struct {
		name string
		f    rtspFaults
		rule string
	}{
		{"missing SPS", rtspFaults{dropSPSAt: 2}, RuleRTSPInband},
		{"missing marker", rtspFaults{clearMarker: true}, RuleRTSPMarker},
		{"no SR", rtspFaults{noSR: true}, RuleRTSPSR},
		{"skewed SR", rtspFaults{skewSR: 2 * time.Second}, RuleRTSPSRAlign},
		{"delta first", rtspFaults{startDelta: true}, RuleEpochKey},
		{"MTU", rtspFaults{mtu: 1400}, RuleRTSPMTU},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRTSPHandler(t, "h264_aac.ts", tc.f)
			c := readRTSP(t, startRTSP(t, h), ReadOptions{})
			assert.Contains(t, ruleNames(c), tc.rule, "%v", c.Violations)
		})
	}
}

// dropConn drops the nth interleaved frame on channel 0 sent by the server.
type dropConn struct {
	net.Conn
	n, seen int
	raw     []byte
	out     []byte
}

func (d *dropConn) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		buf := make([]byte, 64<<10)
		n, err := d.Conn.Read(buf)
		d.raw = append(d.raw, buf[:n]...)
		d.parse()
		if len(d.out) == 0 && err != nil {
			return 0, err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}

func (d *dropConn) parse() {
	for len(d.raw) > 0 {
		if d.raw[0] == '$' {
			if len(d.raw) < 4 {
				return
			}
			n := 4 + int(binary.BigEndian.Uint16(d.raw[2:]))
			if len(d.raw) < n {
				return
			}
			if d.raw[1] == 0 {
				d.seen++
			}
			if d.raw[1] != 0 || d.seen != d.n {
				d.out = append(d.out, d.raw[:n]...)
			}
			d.raw = d.raw[n:]
			continue
		}
		// An RTSP response: headers, then Content-Length bytes.
		end := -1
		for j := 0; j+3 < len(d.raw); j++ {
			if string(d.raw[j:j+4]) == "\r\n\r\n" {
				end = j + 4
				break
			}
		}
		if end < 0 {
			return
		}
		hdr := string(d.raw[:end])
		body := 0
		for _, l := range splitLines(hdr) {
			if len(l) > 15 && (l[:15] == "Content-Length:" || l[:15] == "content-length:") {
				for _, c := range l[15:] {
					if c >= '0' && c <= '9' {
						body = body*10 + int(c-'0')
					}
				}
			}
		}
		if len(d.raw) < end+body {
			return
		}
		d.out = append(d.out, d.raw[:end+body]...)
		d.raw = d.raw[end+body:]
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func TestReadRTSPSequenceGapTCP(t *testing.T) {
	h := newRTSPHandler(t, "h264_aac.ts", rtspFaults{})
	u := startRTSP(t, h)
	// The reader dials through opt.Dial.
	c := readRTSP(t, u, ReadOptions{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		nc, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &dropConn{Conn: nc, n: 10}, nil
	}})
	assert.Contains(t, ruleNames(c), RuleRTSPSeq, "%v", c.Violations)
}
