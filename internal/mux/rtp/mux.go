// Package rtpmux turns the hub frames of one RTSP play session into RTP
// packets and RTCP sender reports.
package rtpmux

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/pion/rtp"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

// ErrCodecChanged is returned when a carried track's codec or AAC config
// (ASC, sampling rate, channel count) changes, or a frame arrives whose kind
// the session's SDP does not list. The session cannot continue: players
// reconnect and DESCRIBE again.
var ErrCodecChanged = errors.New("rtpmux: codec changed")

// DefaultMTU is the RTP payload size used when NewMuxer gets less than 4.
const DefaultMTU = 1200

// RTP payload types of the carried codecs.
const (
	payloadH264 = 96
	payloadH265 = 97
	payloadAAC  = 98
)

const (
	rtpHeaderLen = 12
	aacAUSamples = 1024
	srInterval   = 5 * time.Second
	// maxKeyWait is how long, in stream time from the first frame dropped
	// while waiting for a video keyframe, a session carrying video waits
	// before it goes on with audio: the video track has ended
	// (hub.LiveWindow), e.g. after a failover to an audio-only source.
	maxKeyWait = hub.LiveWindow
)

// Track is one track of a session's frozen SDP.
type Track struct {
	SDPIndex int
	HubTrack int
	Desc     rtsp.TrackDesc
	// Setup reports whether the client SETUP the track; frames of a track
	// not SETUP are ignored.
	Setup bool
	// SSRC is used only in sender reports (the session stamps packets).
	SSRC uint32
	// TSBase is the RTP timestamp of the session base b.
	TSBase uint32
}

// Packets is the RTP packets of one frame, for one SDP track. Sequence
// numbers and SSRC are left for the session to stamp.
type Packets struct {
	SDPIndex int
	RTP      []*rtp.Packet
}

// Report is one RTCP sender report for one SDP track.
type Report struct {
	SDPIndex int
	RTCP     []byte
}

// MuxStats counts what a Muxer sent, ignored and dropped.
type MuxStats struct {
	// IgnoredFrames counts frames of a listed kind from a hub track the SDP
	// does not carry, or from a track the client did not SETUP.
	IgnoredFrames uint64
	// DroppedBeforeKey counts frames dropped while waiting for a video
	// keyframe (session start, Resync, video at an epoch start).
	DroppedBeforeKey uint64
	// DroppedEarlyAudio counts audio frames presented before the keyframe
	// the session started or resumed at.
	DroppedEarlyAudio uint64
	// DroppedBeforeBase counts frames whose DTS is below the session base.
	DroppedBeforeBase uint64
	// Resyncs counts Resync calls.
	Resyncs uint64
	// Packets and Octets count the RTP packets sent and their payload bytes.
	Packets, Octets uint64
}

// state is where a Muxer is in its session.
type state uint8

const (
	stateIdle    state = iota // nothing sent yet
	stateRun                  // sending
	stateWaitKey              // after Resync: dropping until the resume point
)

// track is the state of one carried SDP track.
type track struct {
	Track
	kind  media.Kind
	rate  int64 // RTP clock
	pack  rtsp.Packetizer
	aac   rtsp.AACFormat
	okCfg *media.TrackConfig // last config that passed the codec check

	needKey    bool // video: drop until a keyframe
	epoch      uint32
	epochSet   bool
	a0Set      bool // audio: the rounding anchor is set
	a0PTS      int64
	a0Off      int64 // RTP ticks from b to a0PTS
	a0Epoch    uint32
	sent       bool
	nextSR     time.Time // zero until the first SR: due right after the first packet
	lastTS     uint32
	lastOff    int64 // RTP ticks from b to the last packet's timestamp
	pkts, octs uint32
}

// Muxer turns the hub frames of one RTSP play session into RTP packets. It
// is not safe for concurrent use.
//
// The session base b is the DTS of the first frame sent: a video keyframe
// when a video track is SETUP, else the first audio frame. Video RTP
// timestamps are TSBase + (PTS − b); audio timestamps are TSBase +
// round((PTS_a0 − b)·rate/90000) + round((PTS − PTS_a0)·rate/90000), with
// PTS_a0 the track's first audio PTS sent in the current epoch. Anchored
// rounding keeps the 90 kHz rounding of each PTS from adding ±1 sample of
// jitter: frames step by exactly 1024 while their PTS stay within about a
// tick of PTS_a0's sample grid (a source whose PTS stray further can still
// step by 1023 or 1025). Every parameter set of the frame's Config goes
// before the slices of every keyframe. Frames with a DTS below b are
// dropped, and so is audio presented before the start or resume keyframe;
// video of a new epoch waits for that epoch's first keyframe.
//
// Sender reports use one NTP mapping per session: NTP(t) = NTP₀ + (t −
// b)/90000 s, NTP₀ the wallclock at the first packet. A track's first report
// is due right after its first packet, then one every 5 s.
type Muxer struct {
	now          func() time.Time
	mtu          int
	tracks       []*track // carried tracks, SDP order
	video, audio *track
	st           state
	base         int64
	keyPTS       int64
	ntp0         time.Time
	waiting      bool  // a frame was dropped waiting for a keyframe
	waitFrom     int64 // its DTS
	stats        MuxStats
}

// NewMuxer returns a Muxer for a session's frozen tracks. The first track of
// each kind is carried; mtu is the RTP payload size (below 4: DefaultMTU);
// now gives the wallclock for NTP₀ and the report timer.
func NewMuxer(tracks []Track, mtu int, now func() time.Time) *Muxer {
	if mtu < 4 {
		mtu = DefaultMTU
	}
	m := &Muxer{now: now, mtu: mtu}
	for _, tr := range tracks {
		t := &track{Track: tr, kind: kindOf(tr.Desc.Codec), rate: int64(tr.Desc.ClockRate)}
		switch tr.Desc.Codec {
		case rtsp.CodecH264:
			t.pack = rtsp.NewH264Packetizer(mtu)
		case rtsp.CodecH265:
			t.pack = rtsp.NewH265Packetizer(mtu)
		case rtsp.CodecAAC:
			if tr.Desc.AAC != nil {
				t.aac = *tr.Desc.AAC
			}
		case rtsp.CodecOther:
			continue
		default:
			continue
		}
		if t.kind == media.KindVideo {
			t.rate = media.ClockRate
		}
		slot := m.slot(t.kind)
		if *slot != nil {
			continue
		}
		*slot = t
		m.tracks = append(m.tracks, t)
	}
	return m
}

// Stats returns the counters.
func (m *Muxer) Stats() MuxStats { return m.stats }

// Resync restarts the output after frames were lost: frames are dropped
// until the next video keyframe (the next audio frame when no video track is
// SETUP), which gets the parameter sets as always, and audio presented
// before it is dropped. The base and the timestamp mapping are kept. When no
// video keyframe comes within hub.LiveWindow of stream time after the first
// frame dropped, the video track has ended: audio resumes, and video at its
// next keyframe.
func (m *Muxer) Resync() {
	m.stats.Resyncs++
	switch m.st {
	case stateIdle, stateWaitKey:
	case stateRun:
		m.st, m.waiting = stateWaitKey, false
	}
}

// Frame returns the RTP packets for one hub frame and any sender reports now
// due; the caller writes the packets first. A frame whose kind the SDP does
// not list, or a carried track's codec or AAC config change, is
// ErrCodecChanged. A frame of a listed kind from another hub track, or of a
// track not SETUP, is ignored and counted.
func (m *Muxer) Frame(f media.Frame) (*Packets, []Report, error) {
	if f.Config == nil {
		return nil, nil, errors.New("rtpmux: frame: no track config")
	}
	kind := f.Kind()
	slot := m.slot(kind)
	if slot == nil || *slot == nil {
		return nil, nil, fmt.Errorf("rtpmux: frame: %v track not in the SDP: %w", f.Config.Codec, ErrCodecChanged)
	}
	t := *slot
	if f.Track != t.HubTrack || !t.Setup {
		m.stats.IgnoredFrames++
		return nil, m.Reports(m.now()), nil
	}
	if err := t.check(f.Config); err != nil {
		return nil, nil, err
	}
	p := m.frame(t, f)
	return p, m.Reports(m.now()), nil
}

// Reports returns the sender reports due at now: a track's first right after
// its first packet, then one every 5 s.
func (m *Muxer) Reports(now time.Time) []Report {
	var out []Report
	for _, t := range m.tracks {
		if !t.sent || now.Before(t.nextSR) {
			continue
		}
		t.nextSR = now.Add(srInterval)
		ntp := m.ntp0.Add(ticksToDuration(t.lastOff, t.rate))
		out = append(out, Report{
			SDPIndex: t.SDPIndex,
			RTCP:     rtsp.BuildSenderReport(t.SSRC, ntp, t.lastTS, t.pkts, t.octs),
		})
	}
	return out
}

// slot returns the carried-track slot of kind k, or nil for an unknown kind.
func (m *Muxer) slot(k media.Kind) **track {
	switch k {
	case media.KindVideo:
		return &m.video
	case media.KindAudio:
		return &m.audio
	}
	return nil
}

// carriesVideo reports whether the session sends video.
func (m *Muxer) carriesVideo() bool { return m.video != nil && m.video.Setup }

// frame runs the session state machine for f, a frame of carried track t.
func (m *Muxer) frame(t *track, f media.Frame) *Packets {
	switch m.st {
	case stateIdle, stateWaitKey:
		if m.st == stateWaitKey && f.DTS < m.base {
			m.stats.DroppedBeforeBase++
			return nil
		}
		switch {
		case !m.carriesVideo():
			// An audio-only session starts or resumes at its next frame.
		case t.kind == media.KindVideo && f.Key:
			t.needKey = false
		case t.kind == media.KindAudio && m.waiting && f.DTS-m.waitFrom >= maxKeyWait:
			// The video track ended: go on with audio, video at its next
			// keyframe.
			m.video.needKey = true
		default:
			if !m.waiting {
				m.waiting, m.waitFrom = true, f.DTS
			}
			m.stats.DroppedBeforeKey++
			return nil
		}
		if m.st == stateIdle {
			m.base, m.ntp0 = f.DTS, m.now()
		}
		m.keyPTS = f.PTS
		m.st, m.waiting = stateRun, false
		return m.send(t, f)
	case stateRun:
		return m.send(t, f)
	}
	return nil
}

// send packetises f on a running session.
func (m *Muxer) send(t *track, f media.Frame) *Packets {
	if t.kind == media.KindAudio && f.PTS < m.keyPTS {
		m.stats.DroppedEarlyAudio++
		return nil
	}
	if f.DTS < m.base {
		m.stats.DroppedBeforeBase++
		return nil
	}
	var (
		pkts []*rtp.Packet
		off  int64
	)
	switch t.kind {
	case media.KindVideo:
		// A new epoch may start on a frame that refers to pictures of the
		// old source: wait for its keyframe.
		if t.epochSet && f.Epoch != t.epoch {
			t.needKey = true
		}
		t.epoch, t.epochSet = f.Epoch, true
		if t.needKey {
			if !f.Key {
				m.stats.DroppedBeforeKey++
				return nil
			}
			t.needKey = false
		}
		off = f.PTS - m.base
		pkts = m.videoPackets(t, f, t.TSBase+uint32(off))
	case media.KindAudio:
		if !t.a0Set || f.Epoch != t.a0Epoch {
			t.a0Set, t.a0PTS, t.a0Epoch = true, f.PTS, f.Epoch
			t.a0Off = roundDiv((f.PTS-m.base)*t.rate, media.ClockRate)
		}
		off = t.a0Off + roundDiv((f.PTS-t.a0PTS)*t.rate, media.ClockRate)
		pkts = rtsp.PacketizeAAC(f.Data, t.TSBase+uint32(off), aacAUSamples, m.mtu+rtpHeaderLen, t.aac)
	}
	if len(pkts) == 0 {
		return nil
	}
	for _, p := range pkts {
		p.Version = 2
		p.PayloadType = t.Desc.Payload
		t.pkts++
		t.octs += uint32(len(p.Payload))
		m.stats.Packets++
		m.stats.Octets += uint64(len(p.Payload))
	}
	t.lastTS, t.lastOff, t.sent = pkts[len(pkts)-1].Timestamp, off, true
	return &Packets{SDPIndex: t.SDPIndex, RTP: pkts}
}

// videoPackets packetises one H.264 or H.265 access unit, the parameter sets of
// its Config first when it is a keyframe; the marker is on the last packet.
func (m *Muxer) videoPackets(t *track, f media.Frame, ts uint32) []*rtp.Packet {
	nalus := f.Data
	if f.Key {
		cfg := f.Config
		nalus = make([][]byte, 0, len(cfg.VPS)+len(cfg.SPS)+len(cfg.PPS)+len(f.Data))
		nalus = append(nalus, cfg.VPS...)
		nalus = append(nalus, cfg.SPS...)
		nalus = append(nalus, cfg.PPS...)
		nalus = append(nalus, f.Data...)
	}
	pls := t.pack.Packetize(nalus)
	out := make([]*rtp.Packet, len(pls))
	for i, pl := range pls {
		out[i] = &rtp.Packet{
			Header:  rtp.Header{Version: 2, Timestamp: ts, Marker: i == len(pls)-1},
			Payload: pl,
		}
	}
	return out
}

// check returns ErrCodecChanged when cfg does not match the track's SDP
// description: another codec, or for AAC another ASC, sampling rate or
// channel count.
func (t *track) check(cfg *media.TrackConfig) error {
	if cfg == t.okCfg {
		return nil
	}
	if codecOf(cfg.Codec) != t.Desc.Codec {
		return fmt.Errorf("rtpmux: frame: %v in a %v track: %w", cfg.Codec, t.Desc.Codec, ErrCodecChanged)
	}
	if cfg.Codec == media.CodecAAC && (!bytes.Equal(cfg.ASC, t.aac.Config) ||
		int64(cfg.SampleRate) != t.rate || cfg.Channels != t.Desc.Channels) {
		return fmt.Errorf("rtpmux: frame: aac config %x (%d Hz, %d ch), sdp %x (%d Hz, %d ch): %w",
			cfg.ASC, cfg.SampleRate, cfg.Channels, t.aac.Config, t.rate, t.Desc.Channels, ErrCodecChanged)
	}
	t.okCfg = cfg
	return nil
}

// Tracks picks the carried tracks from the hub's live track configs, the
// lowest-numbered usable track of each kind, and returns their descriptors
// in SDP order (video first) with ID = hub track. Tracks whose codec is not
// supported or whose config lacks what the SDP needs are skipped. It fails
// when no track is usable.
func Tracks(cfgs []hub.TrackConfig) ([]rtsp.TrackDesc, error) {
	var video, audio *rtsp.TrackDesc
	for _, c := range cfgs {
		d, ok := describe(c)
		if !ok {
			continue
		}
		slot := &audio
		if d.Codec != rtsp.CodecAAC {
			slot = &video
		}
		if *slot == nil || c.Track < (*slot).ID {
			*slot = &d
		}
	}
	var out []rtsp.TrackDesc
	for _, d := range []*rtsp.TrackDesc{video, audio} {
		if d != nil {
			out = append(out, *d)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("rtpmux: tracks: no supported live track")
	}
	return out, nil
}

// describe returns the SDP descriptor of one hub track, or false when its
// codec is not supported or its config is incomplete.
func describe(c hub.TrackConfig) (rtsp.TrackDesc, bool) {
	cfg := c.Config
	if cfg == nil {
		return rtsp.TrackDesc{}, false
	}
	d := rtsp.TrackDesc{Codec: codecOf(cfg.Codec), ClockRate: media.ClockRate, ID: c.Track}
	switch d.Codec {
	case rtsp.CodecH264:
		if len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
			return rtsp.TrackDesc{}, false
		}
		d.Payload = payloadH264
		d.ParamSets = append(append([][]byte{}, cfg.SPS...), cfg.PPS...)
	case rtsp.CodecH265:
		if len(cfg.VPS) == 0 || len(cfg.SPS) == 0 || len(cfg.PPS) == 0 {
			return rtsp.TrackDesc{}, false
		}
		d.Payload = payloadH265
		d.ParamSets = append(append(append([][]byte{}, cfg.VPS...), cfg.SPS...), cfg.PPS...)
	case rtsp.CodecAAC:
		if len(cfg.ASC) == 0 || cfg.SampleRate <= 0 || cfg.Channels <= 0 {
			return rtsp.TrackDesc{}, false
		}
		d.Payload = payloadAAC
		d.ClockRate = uint32(cfg.SampleRate)
		d.Channels = cfg.Channels
		d.AAC = &rtsp.AACFormat{Config: cfg.ASC, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3}
	case rtsp.CodecOther:
		return rtsp.TrackDesc{}, false
	default:
		return rtsp.TrackDesc{}, false
	}
	return d, true
}

func codecOf(c media.Codec) rtsp.Codec {
	switch c {
	case media.CodecH264:
		return rtsp.CodecH264
	case media.CodecH265:
		return rtsp.CodecH265
	case media.CodecAAC:
		return rtsp.CodecAAC
	default:
		return rtsp.CodecOther
	}
}

func kindOf(c rtsp.Codec) media.Kind {
	switch c {
	case rtsp.CodecH264, rtsp.CodecH265:
		return media.KindVideo
	case rtsp.CodecAAC:
		return media.KindAudio
	case rtsp.CodecOther:
		return 0
	default:
		return 0
	}
}

// roundDiv divides rounding to the nearest integer, halves away from zero;
// b is positive.
func roundDiv(a, b int64) int64 {
	if a < 0 {
		return -((-a + b/2) / b)
	}
	return (a + b/2) / b
}

// ticksToDuration converts n ticks of a rate-Hz clock to a duration without
// overflowing for long sessions.
func ticksToDuration(n, rate int64) time.Duration {
	if rate <= 0 {
		return 0
	}
	sec, rem := n/rate, n%rate
	return time.Duration(sec)*time.Second + time.Duration(rem*int64(time.Second)/rate)
}
