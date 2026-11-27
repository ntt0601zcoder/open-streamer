package conform

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	rtpmux "github.com/ntt0601zcoder/open-streamer/internal/mux/rtp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// epochFrames returns h264_aac.ts as epoch 1 followed by h264_aac_640.ts
// (new parameter sets) as epoch 2. With leadAudio the first epoch-2 audio
// frame whose PTS is at or above the new keyframe's is sent before it.
func epochFrames(t *testing.T, leadAudio bool) []media.Frame {
	t.Helper()
	a := newRTSPHandler(t, "h264_aac.ts", rtspFaults{}).frames
	var maxPTS int64
	for _, f := range a {
		maxPTS = max(maxPTS, f.PTS)
	}
	var kf media.Frame
	braw := streamClip(t, "h264_aac_640.ts")
	for _, f := range braw {
		if f.Kind() == media.KindVideo && f.Key {
			kf = f
			break
		}
	}
	shift := maxPTS + 3600 - kf.DTS
	var b []media.Frame
	started := false
	for _, f := range braw {
		if !started {
			if f.Kind() != media.KindVideo || !f.Key {
				continue
			}
			started = true
		}
		f.PTS += shift
		f.DTS += shift
		f.Epoch = 2
		if f.Kind() == media.KindAudio && f.PTS < kf.PTS+shift {
			continue
		}
		b = append(b, f)
	}
	if leadAudio {
		for i, f := range b {
			if f.Kind() == media.KindAudio {
				b = append([]media.Frame{f}, append(b[:i:i], b[i+1:]...)...)
				break
			}
		}
	}
	for i := range a {
		a[i].Epoch = 1
	}
	return append(a, b...)
}

func epochCounts(c Capture) map[uint32]int {
	m := map[uint32]int{}
	for _, f := range c.Frames {
		m[f.Epoch]++
	}
	return m
}

func TestReadRTSPEpochChange(t *testing.T) {
	for _, lead := range []bool{false, true} {
		for _, q := range []string{"", "?transport=udp"} {
			name := map[bool]string{false: "inorder", true: "audiolead"}[lead] + q
			t.Run(name, func(t *testing.T) {
				h := newRTSPHandler(t, "h264_aac.ts", rtspFaults{})
				h.frames = epochFrames(t, lead)
				c := readRTSP(t, startRTSP(t, h)+q, ReadOptions{})
				assert.Empty(t, c.Violations)
				ep := epochCounts(c)
				assert.Len(t, ep, 2, "%v", ep)
				assert.Positive(t, ep[1])
				assert.Positive(t, ep[2])
			})
		}
	}
}

// muxed is what a client reads of a clip played through our muxer, without
// a network: the SDP, RTP-Info and the packets in arrival order.
type muxed struct {
	desc rtsp.Description
	info []rtsp.RTPInfo
	pkts []rtsp.Packet
}

var muxBases = []uint32{0xFFFF0000, 0xFFFFFF00}

func muxFrames(t *testing.T, frames []media.Frame) muxed {
	t.Helper()
	var cfgs []hub.TrackConfig
	for _, f := range frames {
		seen := false
		for _, c := range cfgs {
			seen = seen || c.Track == f.Track
		}
		if !seen {
			cfgs = append(cfgs, hub.TrackConfig{Track: f.Track, Kind: f.Kind(), Config: f.Config})
		}
	}
	descs, err := rtpmux.Tracks(cfgs)
	require.NoError(t, err)
	var out muxed
	trs := make([]rtpmux.Track, len(descs))
	seq := make([]uint16, len(descs))
	for i, d := range descs {
		trs[i] = rtpmux.Track{SDPIndex: i, HubTrack: d.ID, Desc: d, Setup: true, SSRC: 0x1000 + uint32(i), TSBase: muxBases[i]}
		seq[i] = 100
		out.desc.Media = append(out.desc.Media, rtsp.Media{
			Index: i, Codec: d.Codec, Payload: d.Payload, ClockRate: d.ClockRate, ParamSets: d.ParamSets, AAC: d.AAC,
		})
		out.info = append(out.info, rtsp.RTPInfo{Seq: 100, RTPTime: muxBases[i], HasSeq: true, HasTime: true})
	}
	var now time.Time
	m := rtpmux.NewMuxer(trs, 0, func() time.Time { return now })
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := frames[0].DTS
	for _, f := range frames {
		now = t0.Add(time.Duration(f.DTS-base) * time.Second / media.ClockRate)
		p, reps, err := m.Frame(f)
		require.NoError(t, err)
		if p != nil {
			for _, x := range p.RTP {
				x.Version, x.SSRC, x.PayloadType, x.SequenceNumber = 2, trs[p.SDPIndex].SSRC, descs[p.SDPIndex].Payload, seq[p.SDPIndex]
				seq[p.SDPIndex]++
				out.pkts = append(out.pkts, rtsp.Packet{Track: p.SDPIndex, RTP: x, Arrival: now})
			}
		}
		for _, r := range reps {
			out.pkts = append(out.pkts, rtsp.Packet{Track: r.SDPIndex, RTCP: r.RTCP, Arrival: now})
		}
	}
	return out
}

// replay feeds the packets to a capture as ReadRTSP would, to the end of the
// stream.
func (m muxed) replay(udp bool) Capture {
	r := newRTSPCaptureFor(m.desc, m.info, ReadOptions{}, udp)
	for _, p := range m.pkts {
		if p.RTP != nil {
			r.rtp(p)
		} else {
			r.rtcp(p)
		}
	}
	return r.result(true)
}

func cleanMux(t *testing.T) muxed {
	t.Helper()
	return muxFrames(t, newRTSPHandler(t, "h264_aac.ts", rtspFaults{}).frames)
}

func TestReplayClean(t *testing.T) {
	c := cleanMux(t).replay(false)
	assert.Empty(t, c.Violations)
	assert.Zero(t, c.RTPLost)
}

// An SR that comes before a track's first packet must not skew the SR gap
// checks.
func TestReadRTSPSRBeforeFirstPacket(t *testing.T) {
	m := cleanMux(t)
	t0 := m.pkts[0].Arrival
	pre := make([]rtsp.Packet, 0, len(m.desc.Media))
	for i := range m.desc.Media {
		ntp := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		pre = append(pre, rtsp.Packet{
			Track: i, Arrival: t0.Add(-time.Millisecond),
			RTCP: rtsp.BuildSenderReport(0x1000+uint32(i), ntp, muxBases[i], 0, 0),
		})
	}
	m.pkts = append(pre, m.pkts...)
	for _, udp := range []bool{false, true} {
		c := m.replay(udp)
		assert.Empty(t, c.Violations, "udp %v", udp)
	}
}

func dropEvery(m muxed, track, n int, from int) (muxed, int) {
	var out []rtsp.Packet
	k, dropped := 0, 0
	for _, p := range m.pkts {
		if p.RTP != nil && p.Track == track {
			k++
			if k >= from && k%n == 0 {
				dropped++
				continue
			}
		}
		out = append(out, p)
	}
	m.pkts = out
	return m, dropped
}

func TestReadRTSPLoss(t *testing.T) {
	m, dropped := dropEvery(cleanMux(t), 0, 7, 3)
	require.Positive(t, dropped)
	u := m.replay(true)
	assert.Equal(t, uint64(dropped), u.RTPLost)
	assert.NotContains(t, ruleNames(u), RuleRTSPSeq, "UDP loss is counted, not reported")
	tc := m.replay(false)
	assert.Equal(t, uint64(dropped), tc.RTPLost)
	assert.Contains(t, ruleNames(tc), RuleRTSPSeq)

	// A lost first packet is no RTP-Info violation over UDP.
	f := cleanMux(t)
	for i, p := range f.pkts {
		if p.RTP != nil && p.Track == 0 {
			f.pkts = append(f.pkts[:i:i], f.pkts[i+1:]...)
			break
		}
	}
	assert.NotContains(t, ruleNames(f.replay(true)), RuleRTSPRTPInfo)
	assert.Contains(t, ruleNames(f.replay(false)), RuleRTSPRTPInfo)
}

// retime sets the timestamp of the nth video access unit (decode order).
func retime(m muxed, au int, ts func(old uint32) uint32) muxed {
	var olds []uint32
	for _, p := range m.pkts {
		if p.RTP != nil && p.Track == 0 && (len(olds) == 0 || olds[len(olds)-1] != p.RTP.Timestamp) {
			olds = append(olds, p.RTP.Timestamp)
		}
	}
	old := olds[au]
	for _, p := range m.pkts {
		if p.RTP != nil && p.Track == 0 && p.RTP.Timestamp == old {
			q := *p.RTP
			q.Timestamp = ts(old)
			*p.RTP = q
		}
	}
	return m
}

func TestReadRTSPDroppedByDTS(t *testing.T) {
	first := uint32(muxBases[0])
	// A picture presented before the epoch's start keyframe.
	// An H.264 picture after an IDR is no leading picture: its DTS cannot be
	// derived.
	lead := retime(cleanMux(t), 3, func(uint32) uint32 { return first - 100 })
	assert.Contains(t, ruleNames(lead.replay(false)), RuleDTS)
	assert.NotContains(t, ruleNames(lead.replay(false)), RuleRASL)
	// A picture whose PTS is at or below the last DTS.
	m := cleanMux(t)
	var keyTS uint32
	for _, p := range m.pkts {
		if p.RTP != nil && p.Track == 0 {
			keyTS = p.RTP.Timestamp
			break
		}
	}
	late := retime(m, 5, func(uint32) uint32 { return keyTS + 1 })
	assert.Contains(t, ruleNames(late.replay(false)), RuleDTS)
}

// retype sets the NAL unit type of every VCL NAL unit of the nth H.265
// video access unit (decode order).
func retype(m muxed, au int, typ byte) muxed {
	var olds []uint32
	for _, p := range m.pkts {
		if p.RTP != nil && p.Track == 0 && (len(olds) == 0 || olds[len(olds)-1] != p.RTP.Timestamp) {
			olds = append(olds, p.RTP.Timestamp)
		}
	}
	for _, p := range m.pkts {
		if p.RTP == nil || p.Track != 0 || p.RTP.Timestamp != olds[au] {
			continue
		}
		q := *p.RTP
		pl := append([]byte(nil), q.Payload...)
		switch nt := (pl[0] >> 1) & 0x3f; {
		case nt == 49: // FU: the type is in the FU header
			pl[2] = pl[2]&0xc0 | typ
		case nt < 32:
			pl[0] = pl[0]&0x81 | typ<<1
		}
		q.Payload = pl
		*p.RTP = q
	}
	return m
}

// A leading H.265 picture, presented before the epoch's start keyframe, is
// a RuleRASL violation only when it is RASL: a RADL picture is dropped
// silently, and any other picture is a RuleDTS violation.
func TestReadRTSPLeadingPictures(t *testing.T) {
	hevc := func() muxed {
		return muxFrames(t, newRTSPHandler(t, "h265_aac51.ts", rtspFaults{}).frames)
	}
	first := uint32(muxBases[0])
	early := func(uint32) uint32 { return first - 100 }
	require.Empty(t, hevc().replay(false).Violations)

	rasl := retime(retype(hevc(), 3, 8), 3, early).replay(false)
	assert.Contains(t, ruleNames(rasl), RuleRASL)
	assert.NotContains(t, ruleNames(rasl), RuleDTS)

	radl := retime(retype(hevc(), 3, 6), 3, early).replay(false)
	assert.Empty(t, radl.Violations, "a RADL leading picture is dropped silently")

	trail := retime(retype(hevc(), 3, 1), 3, early).replay(false)
	assert.Contains(t, ruleNames(trail), RuleDTS)
	assert.NotContains(t, ruleNames(trail), RuleRASL)
}

func TestStripTransport(t *testing.T) {
	q, tr, err := stripTransport("rtsp://h:1/a/b?x=1&transport=udp&y=a%20b")
	require.NoError(t, err)
	assert.Equal(t, "rtsp://h:1/a/b?x=1&y=a%20b", q)
	assert.Equal(t, "udp", tr)
	q, tr, err = stripTransport("rtsp://h:1/a/b?transport=TCP")
	require.NoError(t, err)
	assert.Equal(t, "rtsp://h:1/a/b", q)
	assert.Equal(t, "tcp", tr)
	_, _, err = stripTransport("rtsp://h:1/a/b?transport=sctp")
	assert.Error(t, err)
	q, tr, err = stripTransport("rtsp://h:1/a/b")
	require.NoError(t, err)
	assert.Equal(t, "rtsp://h:1/a/b", q)
	assert.Equal(t, "tcp", tr)
}

func TestReadRTSPBackstopShort(t *testing.T) {
	h := newRTSPHandler(t, "h264_aac.ts", rtspFaults{pace: 100 * time.Millisecond})
	u := startRTSP(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := ReadRTSP(ctx, u, ReadOptions{Duration: 1500 * time.Millisecond, Timeout: 400 * time.Millisecond})
	require.NoError(t, err)
	assert.Contains(t, ruleNames(c), RuleRTSPCaptureShort, "%v", c.Violations)
}

func TestReadRTSPBadTransport(t *testing.T) {
	_, err := ReadRTSP(context.Background(), "rtsp://127.0.0.1:1/live/s?transport=sctp", ReadOptions{})
	require.Error(t, err)
}

// TestReadRTSPCountsBye checks that Capture.ByeTracks counts the tracks that
// sent an RTCP BYE, once each however many they send.
func TestReadRTSPCountsBye(t *testing.T) {
	m := cleanMux(t)
	require.Len(t, m.desc.Media, 2)
	assert.Zero(t, m.replay(false).ByeTracks)
	last := m.pkts[len(m.pkts)-1].Arrival
	bye := func(track int) rtsp.Packet {
		return rtsp.Packet{Track: track, RTCP: rtsp.BuildBye(0x2000+uint32(track), "t", nil), Arrival: last}
	}
	m.pkts = append(m.pkts, bye(0), bye(0))
	assert.Equal(t, 1, m.replay(false).ByeTracks)
	m.pkts = append(m.pkts, bye(1))
	assert.Equal(t, 2, m.replay(true).ByeTracks)
}
