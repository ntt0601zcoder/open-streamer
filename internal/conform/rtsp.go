package conform

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/h26x"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/unwrap"
)

// RTSP container rule names, checked by ReadRTSP on top of the frame rules.
const (
	// RuleRTSPSeq: over TCP, a track's RTP sequence numbers are contiguous
	// and its SSRC does not change.
	RuleRTSPSeq = "rtsp-seq-contiguous"
	// RuleRTSPMarker: the last RTP packet of each video access unit carries
	// the marker bit, and no other packet of it does.
	RuleRTSPMarker = "rtsp-marker-last"
	// RuleRTSPMTU: an RTP payload is at most the MTU.
	RuleRTSPMTU = "rtsp-payload-mtu"
	// RuleRTSPRTPInfo: PLAY's RTP-Info carries seq and rtptime, the first
	// packet has the seq, and the sender reports agree with the rtptime.
	RuleRTSPRTPInfo = "rtsp-rtpinfo-consistent"
	// RuleRTSPInband: every parameter set of the stream is in band before
	// each keyframe access unit, and parses.
	RuleRTSPInband = "rtsp-param-sets-in-band"
	// RuleRTSPSR: a sender report within 1 s of a track's first packet, then
	// at most 6 s apart.
	RuleRTSPSR = "rtsp-sr-timely"
	// RuleRTSPSRAlign: equal stream time maps to equal NTP time on every
	// track, within 1 ms.
	RuleRTSPSRAlign = "rtsp-sr-aligned"
	// RuleRTSPSDP: every SDP media is supported and valid.
	RuleRTSPSDP = "rtsp-sdp-valid"
)

const (
	rtspDefaultMTU  = 1200
	rtspFirstSR     = time.Second
	rtspSRGap       = 6 * time.Second
	rtspSRTolerance = time.Millisecond
	rtspByeGrace    = 500 * time.Millisecond
	rtspAACSamples  = 1024
	rtcpTypeBye     = 203
)

// rtspVideo is the video state of a track.
type rtspVideo struct {
	codec media.Codec
	hc    h26x.Codec
	sets  [3][][]byte // VPS, SPS, PPS
	cfg   *media.TrackConfig
	cls   *media.NALClassifier
	dts   *h26x.DTSExtractor
	seen  bool // a video AU was read
	used  bool // the epoch holds a frame of this track
}

// rtspTrack is the state of one SDP media.
type rtspTrack struct {
	m     rtsp.Media
	track int
	info  rtsp.RTPInfo
	dp    rtsp.Depacketizer
	seq   rtsp.SeqTracker
	clock int64
	acfg  *media.TrackConfig
	v     *rtspVideo

	unw     *unwrap.Unwrapper // packet timestamps
	origin  int64             // unwrapped rtptime
	hasOrig bool
	npkts   int
	// firstTS and lastTS are the unwrapped timestamps of the first and the
	// latest packet; firstArr and lastArr their arrivals.
	firstTS, lastTS   int64
	firstArr, lastArr time.Time

	// Access-unit groups for the marker rule.
	grpOpen, grpLost, nextLost bool
	grpTS                      uint32
	grpMarker                  bool

	// Sender reports.
	srUnw *unwrap.Unwrapper
	nSR   int
	srTS  int64 // lastTS at the latest SR
	srArr time.Time
	delta int64 // NTP minus stream time of the first SR, ns
	bye   bool
}

// rtspCapture is the state of one ReadRTSP.
type rtspCapture struct {
	b       *builder
	tracks  []*rtspTrack // by media index; nil when not read
	udp     bool
	mtu     int
	epoch   uint32
	used    bool
	startAt int64 // PTS the current epoch starts at (video keyframe)
	refSet  bool
	refIdx  int
	refD    int64
}

// ReadRTSP plays u with pkg/rtsp (TCP unless the URL has ?transport=udp,
// which is removed before dialling) for opt.Duration of media time and
// checks, per track: RTP sequence numbers contiguous over TCP (over UDP the
// gaps are not reported); the marker bit on the last packet of each video
// access unit; payloads at most opt.MTU (default 1200); RTP-Info consistent
// with the sender reports; every parameter set of the stream in band before
// each keyframe; a sender report within 1 s of the first packet and then at
// most 6 s apart (both in media time and in arrival time, since the two UDP
// sockets are not ordered); and the NTP times of both tracks' sender
// reports agreeing on stream time within 1 ms.
//
// Frames are rebuilt here with the pkg/rtsp depacketisers and pkg/h26x (DTS
// derived from PTS), aligned by RTP-Info; parameter sets are moved into
// Config. A parameter-set change opens a new capture epoch, and every frame
// check, the start rules included, runs on every capture epoch. Duration 0
// reads until the stream ends (an end of stream, or an RTCP BYE on every
// track). opt.Timeout bounds the connection set-up and every wait for a
// packet; as a backstop the read also ends cleanly after Duration plus
// Timeout of wall-clock time.
func ReadRTSP(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	pu, err := url.Parse(u)
	if err != nil {
		return Capture{}, errors.New("conform: read: rtsp: invalid URL")
	}
	transport := "tcp"
	q := pu.Query()
	if _, ok := q["transport"]; ok {
		if strings.EqualFold(q.Get("transport"), "udp") {
			transport = "udp"
		}
		q.Del("transport")
		pu.RawQuery = q.Encode()
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	fail := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("conform: read: rtsp: %w", ctx.Err())
		case stalled.Load():
			return fmt.Errorf("conform: read: rtsp: stalled for %v: %w", timeout, err)
		}
		return fmt.Errorf("conform: read: rtsp: %w", err)
	}
	c, err := rtsp.Dial(rctx, pu.String(), rtsp.Config{
		Timeout: timeout, IdleTimeout: timeout, Transport: transport, Dial: opt.Dial,
	})
	if err != nil {
		return Capture{}, fail(err)
	}
	closeClient := func() { _ = c.Close() } //nolint:contextcheck // Close bounds its TEARDOWN itself.
	defer closeClient()
	stop := context.AfterFunc(rctx, closeClient)
	defer stop()
	var ended atomic.Bool
	if opt.Duration > 0 {
		backstop := time.AfterFunc(opt.Duration+timeout, func() {
			ended.Store(true)
			closeClient()
		})
		defer backstop.Stop()
	}
	var grace *time.Timer
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()

	r := newRTSPCapture(c, opt, transport == "udp")
	atEOF := false
	for {
		timer.Reset(timeout)
		p, err := c.ReadPacket()
		if err != nil {
			switch {
			case ended.Load() && ctx.Err() == nil:
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				atEOF = true
			default:
				return Capture{}, fail(err)
			}
			break
		}
		if p.RTP != nil {
			r.rtp(p)
		} else if p.RTCP != nil {
			r.rtcp(p)
		}
		if r.allBye() {
			if grace == nil {
				grace = time.AfterFunc(rtspByeGrace, func() {
					ended.Store(true)
					closeClient()
				})
			} else if p.RTP != nil {
				grace.Reset(rtspByeGrace)
			}
		}
		if opt.Duration > 0 && r.span() >= opt.Duration {
			break
		}
	}
	r.finish(atEOF || r.allBye())
	return r.b.finish(), nil
}

func newRTSPCapture(c *rtsp.Client, opt ReadOptions, udp bool) *rtspCapture {
	r := &rtspCapture{b: newBuilder(), epoch: 1, mtu: opt.MTU, udp: udp}
	if r.mtu <= 0 {
		r.mtu = rtspDefaultMTU
	}
	r.b.start(1, true)
	desc := c.Description()
	info := c.RTPInfo()
	for _, m := range desc.Media {
		for len(r.tracks) <= m.Index {
			r.tracks = append(r.tracks, nil)
		}
		r.tracks[m.Index] = r.newTrack(m, info)
	}
	return r
}

func (r *rtspCapture) newTrack(m rtsp.Media, info []rtsp.RTPInfo) *rtspTrack {
	tn := r.b.track("rtsp", m.Index)
	if m.Reason != "" {
		r.b.violate(RuleRTSPSDP, tn, "media %d unsupported: %s", m.Index, m.Reason)
		return nil
	}
	dp, err := rtsp.NewDepacketizer(m, 0)
	if err != nil {
		r.b.violate(RuleRTSPSDP, tn, "media %d: %v", m.Index, err)
		return nil
	}
	t := &rtspTrack{m: m, track: tn, dp: dp, clock: int64(m.ClockRate), unw: unwrap.New(32), srUnw: unwrap.New(32)}
	if m.Index < len(info) {
		t.info = info[m.Index]
	}
	if !t.info.HasTime {
		r.b.violate(RuleRTSPRTPInfo, tn, "RTP-Info has no rtptime for media %d", m.Index)
	} else {
		t.origin, t.hasOrig = t.unw.Unwrap(uint64(t.info.RTPTime)), true
		t.srUnw.Unwrap(uint64(t.info.RTPTime))
	}
	if !t.info.HasSeq {
		r.b.violate(RuleRTSPRTPInfo, tn, "RTP-Info has no seq for media %d", m.Index)
	}
	switch m.Codec {
	case rtsp.CodecH264, rtsp.CodecH265:
		v := &rtspVideo{codec: media.CodecH264, hc: h26x.H264}
		if m.Codec == rtsp.CodecH265 {
			v.codec, v.hc = media.CodecH265, h26x.H265
		}
		v.cls = media.NewNALClassifier(v.codec)
		for _, n := range m.ParamSets {
			if i := setIndex(paramSetName(v.codec, n)); i >= 0 {
				v.sets[i] = append(v.sets[i], n)
			}
		}
		v.cfg = buildVideoConfig(v)
		v.dts = newExtractor(v)
		t.v = v
	case rtsp.CodecAAC:
		cfg, err := aacConfig(m.AAC.Config)
		if err != nil {
			r.b.violate(RuleRTSPSDP, tn, "media %d: AAC config: %v", m.Index, err)
			return nil
		}
		t.acfg = cfg
	case rtsp.CodecOther:
		return nil
	}
	return t
}

// setIndex maps "VPS", "SPS", "PPS" to 0, 1, 2 and anything else to -1.
func setIndex(name string) int {
	switch name {
	case "VPS":
		return 0
	case "SPS":
		return 1
	case "PPS":
		return 2
	}
	return -1
}

// buildVideoConfig returns the config of the video's current sets, nil when
// they do not make one.
func buildVideoConfig(v *rtspVideo) (cfg *media.TrackConfig) {
	defer func() {
		if p := recover(); p != nil {
			cfg = nil
		}
	}()
	var err error
	if v.codec == media.CodecH265 {
		cfg, err = media.NewH265Config(v.sets[0], v.sets[1], v.sets[2])
	} else {
		cfg, err = media.NewH264Config(v.sets[1], v.sets[2])
	}
	if err != nil {
		return nil
	}
	return cfg
}

// newExtractor returns a DTS extractor for the video's current SPS.
func newExtractor(v *rtspVideo) *h26x.DTSExtractor {
	var sps *h26x.SPS
	if len(v.sets[1]) > 0 {
		if s, err := h26x.ParseSPS(v.hc, v.sets[1][0]); err == nil {
			sps = s
		}
	}
	reorder, _ := h26x.ReorderOf(v.hc, sps)
	var dur int64
	if sps != nil {
		dur = sps.FrameDuration
	}
	return h26x.NewDTSExtractor(reorder, dur)
}

// arrival returns a packet's arrival, now when it has none.
func arrival(p rtsp.Packet) time.Time {
	if p.Arrival.IsZero() {
		return time.Now()
	}
	return p.Arrival
}

func (t *rtspTrack) ticksToDur(ticks int64) time.Duration {
	return time.Duration(float64(ticks) / float64(t.clock) * float64(time.Second))
}

// pts90 returns the 90 kHz stream time of a raw RTP timestamp.
func (t *rtspTrack) pts90(ts uint32) int64 {
	u := t.unw.Unwrap(uint64(ts)) - t.origin
	if t.clock == media.ClockRate {
		return u
	}
	return int64(math.Round(float64(u) * media.ClockRate / float64(t.clock)))
}

// span returns the longest media time any track has read.
func (r *rtspCapture) span() time.Duration {
	var s time.Duration
	for _, t := range r.tracks {
		if t != nil && t.npkts > 0 {
			s = max(s, t.ticksToDur(t.lastTS-t.firstTS))
		}
	}
	return s
}

func (r *rtspCapture) allBye() bool {
	n := 0
	for _, t := range r.tracks {
		if t == nil {
			continue
		}
		if !t.bye {
			return false
		}
		n++
	}
	return n > 0
}

func (r *rtspCapture) trackOf(i int) *rtspTrack {
	if i < 0 || i >= len(r.tracks) {
		return nil
	}
	return r.tracks[i]
}

func (r *rtspCapture) rtp(p rtsp.Packet) {
	t := r.trackOf(p.Track)
	if t == nil {
		return
	}
	pk, arr := p.RTP, arrival(p)
	if len(pk.Payload) > r.mtu {
		r.b.once(fmt.Sprintf("mtu:%d", t.track), RuleRTSPMTU, t.track,
			"payload of %d bytes (seq %d) exceeds the MTU %d", len(pk.Payload), pk.SequenceNumber, r.mtu)
	}
	first := t.npkts == 0
	if first && t.info.HasSeq && pk.SequenceNumber != t.info.Seq {
		r.b.violate(RuleRTSPRTPInfo, t.track, "first packet has seq %d, RTP-Info says %d", pk.SequenceNumber, t.info.Seq)
	}
	gap, reset, dup := t.seq.Next(pk.SequenceNumber, pk.SSRC)
	switch {
	case dup:
		if !r.udp {
			r.b.violate(RuleRTSPSeq, t.track, "sequence number %d repeated or behind", pk.SequenceNumber)
		}
		return
	case reset && !first:
		r.b.violate(RuleRTSPSeq, t.track, "SSRC or sequence jump at seq %d", pk.SequenceNumber)
		t.dp.Gap()
	case gap > 0:
		if !r.udp {
			r.b.violate(RuleRTSPSeq, t.track, "%d packets missing before seq %d", gap, pk.SequenceNumber)
		}
		t.dp.Gap()
		t.grpLost, t.nextLost = true, true
		if t.grpOpen && pk.Timestamp == t.grpTS {
			t.nextLost = false
		}
	}
	ts := t.unw.Unwrap(uint64(pk.Timestamp))
	if !t.hasOrig {
		t.origin, t.hasOrig = ts, true
		t.srUnw.Unwrap(uint64(pk.Timestamp))
	}
	if first {
		t.firstTS, t.firstArr = ts, arr
	}
	t.npkts++
	t.lastTS, t.lastArr = ts, arr
	if t.v != nil {
		r.marker(t, pk)
	}
	for _, au := range t.dp.Push(pk, arr) {
		r.au(t, au)
	}
}

// marker applies RuleRTSPMarker: packets with one timestamp make one access
// unit.
func (r *rtspCapture) marker(t *rtspTrack, pk *rtp.Packet) {
	if t.grpOpen && pk.Timestamp != t.grpTS {
		r.endGroup(t)
	}
	if !t.grpOpen {
		t.grpOpen, t.grpTS, t.grpLost, t.nextLost = true, pk.Timestamp, t.nextLost, false
	} else if t.grpMarker {
		r.b.violate(RuleRTSPMarker, t.track, "marker set before the last packet of the access unit at RTP time %d", pk.Timestamp)
	}
	t.grpMarker = pk.Marker
}

func (r *rtspCapture) endGroup(t *rtspTrack) {
	if t.grpOpen && !t.grpLost && !t.grpMarker {
		r.b.violate(RuleRTSPMarker, t.track, "no marker on the last packet of the access unit at RTP time %d", t.grpTS)
	}
	t.grpOpen = false
}

// rtcp handles an RTCP packet: BYE and sender reports.
func (r *rtspCapture) rtcp(p rtsp.Packet) {
	t := r.trackOf(p.Track)
	if t == nil {
		return
	}
	arr := arrival(p)
	if rtcpHasBye(p.RTCP) {
		t.bye = true
	}
	for _, sr := range rtsp.ParseSenderReports(p.RTCP) {
		r.senderReport(t, sr, arr)
	}
}

func rtcpHasBye(b []byte) bool {
	for len(b) >= 4 && b[0]>>6 == 2 {
		n := (int(binary.BigEndian.Uint16(b[2:])) + 1) * 4
		if n > len(b) {
			return false
		}
		if b[1] == rtcpTypeBye {
			return true
		}
		b = b[n:]
	}
	return false
}

func (r *rtspCapture) senderReport(t *rtspTrack, sr rtsp.SenderReport, arr time.Time) {
	if t.npkts > 0 {
		mediaGap := t.ticksToDur(t.lastTS - t.firstTS)
		wallGap := arr.Sub(t.firstArr)
		if t.nSR == 0 && mediaGap > rtspFirstSR && wallGap > rtspFirstSR {
			r.b.violate(RuleRTSPSR, t.track, "first sender report %v after the first packet (limit %v)", mediaGap, rtspFirstSR)
		}
		if t.nSR > 0 {
			mg, wg := t.ticksToDur(t.lastTS-t.srTS), arr.Sub(t.srArr)
			if mg > rtspSRGap && wg > rtspSRGap {
				r.b.violate(RuleRTSPSR, t.track, "%v between sender reports (limit %v)", mg, rtspSRGap)
			}
		}
		t.srTS, t.srArr = t.lastTS, arr
	}
	t.nSR++
	if !t.hasOrig {
		return
	}
	ticks := t.srUnw.Unwrap(uint64(sr.RTP)) - t.origin
	delta := sr.NTP.UnixNano() - int64(t.ticksToDur(ticks))
	tol := rtspSRTolerance.Nanoseconds()
	if t.nSR == 1 {
		t.delta = delta
	} else if abs64(delta-t.delta) > tol {
		r.b.violate(RuleRTSPRTPInfo, t.track,
			"sender report at RTP time %d: NTP minus stream time moved by %v since the first report",
			sr.RTP, time.Duration(delta-t.delta))
	}
	switch {
	case !r.refSet:
		r.refSet, r.refIdx, r.refD = true, t.m.Index, delta
	case t.m.Index != r.refIdx && abs64(delta-r.refD) > tol:
		r.b.violate(RuleRTSPSRAlign, t.track,
			"sender reports of tracks %d and %d map equal stream times %v apart in NTP", t.m.Index, r.refIdx,
			time.Duration(delta-r.refD))
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// finish flushes the depacketisers when the stream ended, closes the marker
// groups it can and checks sender report presence.
func (r *rtspCapture) finish(ended bool) {
	for _, t := range r.tracks {
		if t == nil {
			continue
		}
		if ended {
			for _, au := range t.dp.Flush() {
				r.au(t, au)
			}
			if t.v != nil {
				r.endGroup(t)
			}
		}
		if t.npkts == 0 {
			continue
		}
		last := t.ticksToDur(t.lastTS - t.firstTS)
		if t.nSR == 0 && last > rtspFirstSR {
			r.b.violate(RuleRTSPSR, t.track, "no sender report within %v of the first packet (%v read)", rtspFirstSR, last)
		}
		if t.nSR > 0 {
			if mg := t.ticksToDur(t.lastTS - t.srTS); mg > rtspSRGap && t.lastArr.Sub(t.srArr) > rtspSRGap {
				r.b.violate(RuleRTSPSR, t.track, "%v since the last sender report (limit %v)", mg, rtspSRGap)
			}
		}
	}
}

// au turns an access unit into frames.
func (r *rtspCapture) au(t *rtspTrack, au rtsp.AU) {
	if au.Lost {
		return
	}
	if t.v != nil {
		r.video(t, au)
		return
	}
	for i, fr := range au.Frames {
		pts := t.pts90(au.Timestamp + uint32(i)*rtspAACSamples)
		ep := r.epoch
		if ep > 1 && pts < r.startAt {
			ep--
		}
		r.add(media.Frame{Track: t.track, Config: t.acfg, PTS: pts, DTS: pts, Key: true, Data: [][]byte{fr}, Epoch: ep})
	}
}

func (r *rtspCapture) add(f media.Frame) {
	r.used = true
	r.b.add(f)
}

// isFiller reports an AUD or filler NAL unit.
func isFiller(codec media.Codec, n []byte) bool {
	if len(n) == 0 {
		return false
	}
	if codec == media.CodecH265 {
		typ := (n[0] >> 1) & 0x3f
		return typ == 35 || typ == 38
	}
	typ := n[0] & 0x1f
	return typ == 9 || typ == 12
}

func (r *rtspCapture) video(t *rtspTrack, au rtsp.AU) {
	v := t.v
	pts := t.pts90(au.Timestamp)
	var lead, data [][]byte
	leading := true
	for _, n := range au.NALUs {
		switch {
		case isFiller(v.codec, n):
		case paramSetName(v.codec, n) != "":
			if leading {
				lead = append(lead, n)
			}
		default:
			if isVCL(v.codec, n) {
				leading = false
			}
			data = append(data, n)
		}
	}
	key, rasl := v.cls.Classify(data, pts)
	if !v.seen {
		v.seen = true
		if !key {
			r.b.violate(RuleEpochKey, t.track, "the first video access unit (PTS %d) is not a keyframe", pts)
		}
	}
	if key {
		r.keySets(t, pts, lead)
	}
	dts, st := v.dts.Extract(pts, key)
	if st == h26x.DTSBreak {
		dts, st = v.dts.Extract(pts, key)
	}
	if st != h26x.DTSOK || v.cfg == nil {
		return
	}
	v.used = true
	r.add(media.Frame{Track: t.track, Config: v.cfg, PTS: pts, DTS: dts, Key: key, RASL: rasl, Data: data, Epoch: r.epoch})
}

// keySets applies RuleRTSPInband to a keyframe's leading parameter sets and
// opens a new epoch when they changed.
func (r *rtspCapture) keySets(t *rtspTrack, pts int64, lead [][]byte) {
	v := t.v
	all := slices.Concat(v.sets[0], v.sets[1], v.sets[2])
	changed := false
	var in [3][][]byte
	for _, n := range lead {
		i := setIndex(paramSetName(v.codec, n))
		in[i] = append(in[i], n)
		if !slices.ContainsFunc(all, func(s []byte) bool { return string(s) == string(n) }) {
			changed = true
		}
	}
	if changed {
		for i := range in {
			if len(in[i]) > 0 {
				v.sets[i] = in[i]
			}
		}
		if v.used && r.used {
			r.epoch++
			r.used = false
		}
		v.used = false
		r.startAt = pts
		v.cfg = buildVideoConfig(v)
		v.dts = newExtractor(v)
		if v.cfg == nil {
			r.b.violate(RuleRTSPInband, t.track, "keyframe at PTS %d: the parameter sets do not make a config", pts)
		}
	} else if v.cfg == nil {
		v.cfg = buildVideoConfig(v)
	}
	for _, s := range slices.Concat(v.sets[0], v.sets[1], v.sets[2]) {
		if !slices.ContainsFunc(lead, func(n []byte) bool { return string(n) == string(s) }) {
			r.b.violate(RuleRTSPInband, t.track, "keyframe at PTS %d: the %s is not in band before it", pts, paramSetName(v.codec, s))
		}
	}
}
