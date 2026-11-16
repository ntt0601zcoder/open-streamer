package ingest

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/h26x"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/unwrap"
)

const (
	// rtpVideoClock is the only RTP clock rate accepted for video.
	rtpVideoClock = 90000
	// rtpMaxJump is the RTP timestamp step, in seconds of the track's clock,
	// beyond which a track's timeline is taken as broken.
	rtpMaxJump = 10
	// rtpFirstSRTolerance is how far the first sender-report offset of a
	// track may be from its provisional offset and still keep it: 50 ms.
	rtpFirstSRTolerance = media.ClockRate / 20
	// rtpSRMove is how far a later sender report must move a track's offset
	// for it to change: 100 ms.
	rtpSRMove = media.ClockRate / 10
	// aacFrameSamples is the samples of one AAC frame at the core rate.
	aacFrameSamples = 1024
	// rtpMaxMedia bounds the media indexes taken from a description, as
	// pkg/rtsp bounds the media of an SDP.
	rtpMaxMedia = 16
)

// rtpFrames turns the access units of one RTSP session (pkg/rtsp) into
// frames in source time. Track numbers are the SDP media indexes; kinds
// reports 0 for the media that are not converted. It is used by one
// goroutine; stats is safe concurrently with it.
//
// Timing. Each track's RTP timestamps are unwrapped (32 bits) and scaled to
// 90 kHz as floor(ts·90000/clock), the RTP clock rate being the SDP's (for
// HE-AAC sent at the SBR clock, that rate). A per-track offset in 90 kHz
// ticks maps the track onto the session timeline, the source time of the
// frames. Offsets come, provisionally, from RTP-Info, when the PLAY response
// gave an rtptime for every converted track (those instants coincide), or
// else from arrival: a track without an offset is placed so that its first
// AU's PTS sits where the latest AU of any track puts its arrival
// (sync_by_arrival). The reference track is the first video track, else
// the first converted track; sender reports align every other track to it.
// The first SR pair of a track (its own SR and the reference's) replaces a
// provisional offset that differs by more than 50 ms (sync_corrected); after
// that an SR moves the offset only by more than 100 ms (sync_jumps). A track
// whose offset is still unknown when the pair is known takes the SR offset
// directly; so does the reference, from another track's offset, when that
// track was placed before the reference had an offset. A track whose RTP
// timestamp steps by more than 10 s (timeline_breaks), or that restart
// reports as restarted (a new SSRC), drops its SR, has its DTS extractor
// reset and is placed again by arrival; when it is the reference, every
// other track's next SR pair counts as a first pair again.
//
// Video. Parameter sets come from the SDP sprop sets and in band, through
// the shared ConfigCache, as in flvFrames: a bad set invalidates the config
// and frames are dropped (dropped_no_config) until the sets rebuild a valid
// one. Key and RASL come from a media.NALClassifier, DTS from an
// h26x.DTSExtractor whose reorder depth follows the newest SPS (SetReorder)
// and whose frame duration is the SPS's: when a new SPS changes it, the
// extractor is recreated at its next reset, or at once if it has not
// emitted since its last one. The extractor is reset only on a timeline
// break; a DTSBreak from it is one too. Lost AUs are dropped (lost_aus) and
// reset nothing.
//
// Discontinuities. A change of a track's offset or timeline is reported
// only when that track has returned a frame since the last report, so the
// restarts of every track of one camera restart give one report. senderReport
// and restart report through their result: the frames returned before
// belong before the break, those returned after belong after it. push
// reports through its discontinuity result: the frames it returns with it
// are the first after the break.
type rtpFrames struct {
	cache  *media.ConfigCache
	tracks []*rtpTrack // by media index; nil when not converted
	ref    *rtpTrack

	// The latest AU's arrival and session-timeline PTS, for placing a track
	// by arrival.
	anchorWall time.Time
	anchorPTS  int64
	anchored   bool

	n rtpCounters
}

type rtpCounters struct {
	frames, lostAUs, noConfig, badAUs, unknownTrack, syncByArrival,
	syncCorrected, syncJumps, timelineBreaks, restarts, reorderAssumed,
	dtsLeading, dtsLate, dtsBreaks, dtsReorderRaised atomic.Uint64
}

// rtpTrack is the state of one converted track.
type rtpTrack struct {
	index int
	kind  media.Kind
	clock int64 // RTP clock rate
	unw   *unwrap.Unwrapper

	// last is the unwrapped RTP time of the last AU, or of the RTP-Info or
	// SR that seeded the unwrapper; hasLast tells whether there is one.
	last    int64
	hasLast bool
	// off maps the track's 90 kHz time onto the session timeline.
	off    int64
	hasOff bool
	// sr is the latest sender report; paired tells whether the track's
	// first SR pair was applied.
	sr     rtpSR
	paired bool
	// live: a frame of this track went out since the last reported
	// discontinuity.
	live bool

	v    *rtpVideo
	acfg *media.TrackConfig
	spa  int64 // RTP ticks of one AAC frame
}

// rtpSR is a sender report in the track's unwrapped RTP time.
type rtpSR struct {
	ok  bool
	ntp time.Time
	rtp int64
}

// rtpVideo is the video state of a track.
type rtpVideo struct {
	codec  media.Codec
	hcodec h26x.Codec
	sets   paramSets
	cfg    *media.TrackConfig
	cls    *media.NALClassifier

	dts      *h26x.DTSExtractor
	reorder  int
	frameDur int64
	recreate bool // frameDur changed: build a new extractor at its next reset
	emitted  bool // the extractor returned a DTS since its creation or reset
	seen     h26x.DTSStats
}

// newRTPFrames returns the converter of a session described by desc. info
// is the PLAY response's RTP-Info, one entry per media (Client.RTPInfo);
// cache is shared by the sessions of one input.
func newRTPFrames(desc rtsp.Description, info []rtsp.RTPInfo, cache *media.ConfigCache) *rtpFrames {
	r := &rtpFrames{cache: cache}
	for _, m := range desc.Media {
		if m.Index < 0 || m.Index >= rtpMaxMedia {
			continue
		}
		t := r.newTrack(m)
		for len(r.tracks) <= m.Index {
			r.tracks = append(r.tracks, nil)
		}
		r.tracks[m.Index] = t
		if t != nil && (r.ref == nil || t.kind == media.KindVideo && r.ref.kind != media.KindVideo) {
			r.ref = t
		}
	}
	r.applyRTPInfo(info)
	return r
}

// newTrack returns the state of a media, nil when it is not converted.
func (r *rtpFrames) newTrack(m rtsp.Media) *rtpTrack {
	if m.Reason != "" {
		return nil
	}
	t := &rtpTrack{index: m.Index, clock: int64(m.ClockRate), unw: unwrap.New(32)}
	switch m.Codec {
	case rtsp.CodecH264, rtsp.CodecH265:
		if m.ClockRate != rtpVideoClock {
			return nil
		}
		t.kind = media.KindVideo
		t.v = &rtpVideo{codec: media.CodecH264, hcodec: h26x.H264, sets: newParamSets()}
		if m.Codec == rtsp.CodecH265 {
			t.v.codec, t.v.hcodec = media.CodecH265, h26x.H265
		}
		t.v.cls = media.NewNALClassifier(t.v.codec)
		t.v.reorder, _ = h26x.ReorderOf(t.v.hcodec, nil)
		// The SDP sets are applied like in-band ones; a bad one is counted
		// and in-band sets may repair it.
		if len(m.ParamSets) > 0 {
			if _, err := r.updateSets(t, m.ParamSets); err != nil {
				r.n.badAUs.Add(1)
			}
		}
	case rtsp.CodecAAC:
		if m.AAC == nil || m.ClockRate == 0 {
			return nil
		}
		cfg, err := media.NewAACConfig(m.AAC.Config)
		if err != nil {
			return nil
		}
		t.kind = media.KindAudio
		t.acfg = r.cache.Stable(m.Index, cfg)
		t.spa = max(1, aacFrameSamples*t.clock/int64(cfg.SampleRate))
	case rtsp.CodecOther:
		return nil
	default:
		return nil
	}
	return t
}

// applyRTPInfo seeds each track's unwrapper with its rtptime and sets the
// provisional offsets, when every converted track has one.
func (r *rtpFrames) applyRTPInfo(info []rtsp.RTPInfo) {
	if r.ref == nil {
		return
	}
	for _, t := range r.tracks {
		if t != nil && (t.index >= len(info) || !info[t.index].HasTime) {
			return
		}
	}
	for _, t := range r.tracks {
		if t == nil {
			continue
		}
		t.last, t.hasLast = t.unw.Unwrap(uint64(info[t.index].RTPTime)), true
	}
	for _, t := range r.tracks {
		if t == nil {
			continue
		}
		t.off, t.hasOff = r.ref.to90(r.ref.last)-t.to90(t.last), true
	}
}

// to90 scales unwrapped RTP time to 90 kHz, rounding down.
func (t *rtpTrack) to90(ts int64) int64 {
	if t.clock == media.ClockRate {
		return ts
	}
	return floorDiv(ts*media.ClockRate, t.clock)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func absInt(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func (r *rtpFrames) track(i int) *rtpTrack {
	if i < 0 || i >= len(r.tracks) {
		return nil
	}
	return r.tracks[i]
}

// kinds returns the kind of each track by media index, 0 for media that
// are not converted.
func (r *rtpFrames) kinds() []media.Kind {
	out := make([]media.Kind, len(r.tracks))
	for i, t := range r.tracks {
		if t != nil {
			out[i] = t.kind
		}
	}
	return out
}

// report returns whether a change to t's timeline must be reported: when t
// returned a frame since the last report. Reporting clears that for every
// track.
func (r *rtpFrames) report(t *rtpTrack) bool {
	if !t.live {
		return false
	}
	for _, o := range r.tracks {
		if o != nil {
			o.live = false
		}
	}
	return true
}

// senderReport applies an RTCP sender report of a track (the caller maps
// the SSRC to the track) and reports whether it moved an offset of a track
// that has returned frames (see rtpFrames).
func (r *rtpFrames) senderReport(track int, sr rtsp.SenderReport) (discontinuity bool) {
	t := r.track(track)
	if t == nil {
		return false
	}
	var ts int64
	if t.hasLast {
		ts = t.last + int64(int32(sr.RTP-uint32(t.last)))
	} else {
		ts = t.unw.Unwrap(uint64(sr.RTP))
		t.last, t.hasLast = ts, true
	}
	t.sr = rtpSR{ok: true, ntp: sr.NTP, rtp: ts}
	if t != r.ref {
		disc := r.alignSR(t)
		return r.syncRef() || disc
	}
	return r.syncRef() || r.alignOthers()
}

// srOffset returns the offset that t's and the reference's sender reports
// imply for t, given the reference's offset.
func (r *rtpFrames) srOffset(t *rtpTrack) (int64, bool) {
	ref := r.ref
	if t == ref || !t.sr.ok || !ref.sr.ok || !ref.hasOff {
		return 0, false
	}
	return ref.off + ref.to90(ref.sr.rtp) - t.to90(t.sr.rtp) + media.FromDuration(t.sr.ntp.Sub(ref.sr.ntp)), true
}

// alignSR applies t's SR pair to its offset by the first-pair and later-SR
// rules, and reports whether that is a discontinuity.
func (r *rtpFrames) alignSR(t *rtpTrack) bool {
	want, ok := r.srOffset(t)
	if !ok {
		return false
	}
	switch {
	case !t.hasOff:
		t.off, t.hasOff, t.paired = want, true, true
		return false
	case !t.paired:
		t.paired = true
		if absInt(want-t.off) <= rtpFirstSRTolerance {
			return false
		}
		t.off = want
		r.n.syncCorrected.Add(1)
		return r.report(t)
	case absInt(want-t.off) > rtpSRMove:
		t.off = want
		r.n.syncJumps.Add(1)
		return r.report(t)
	}
	return false
}

// alignOthers applies the SR pair of every track but the reference.
func (r *rtpFrames) alignOthers() bool {
	disc := false
	for _, o := range r.tracks {
		if o != nil && o != r.ref {
			disc = r.alignSR(o) || disc
		}
	}
	return disc
}

// syncRef gives the reference an offset from another track's when the
// reference has none yet and both have an SR, then aligns the other tracks.
func (r *rtpFrames) syncRef() bool {
	ref := r.ref
	if ref.hasOff || !ref.sr.ok {
		return false
	}
	for _, o := range r.tracks {
		if o == nil || o == ref || !o.hasOff || !o.sr.ok {
			continue
		}
		ref.off = o.off + o.to90(o.sr.rtp) - ref.to90(ref.sr.rtp) + media.FromDuration(ref.sr.ntp.Sub(o.sr.ntp))
		ref.hasOff, o.paired = true, true
		return r.alignOthers()
	}
	return false
}

// restart reports that a track's source restarted (a new SSRC): its SR is
// dropped, its DTS extractor reset, and its next AU is placed by arrival.
// It reports whether that is a discontinuity (see rtpFrames).
func (r *rtpFrames) restart(track int) (discontinuity bool) {
	t := r.track(track)
	if t == nil {
		return false
	}
	r.n.restarts.Add(1)
	t.unw.Reset()
	t.hasLast = false
	r.breakTimeline(t)
	return r.report(t)
}

// breakTimeline drops what ties t to its old timeline.
func (r *rtpFrames) breakTimeline(t *rtpTrack) {
	t.hasOff, t.sr, t.paired = false, rtpSR{}, false
	if t == r.ref {
		for _, o := range r.tracks {
			if o != nil {
				o.paired = false
			}
		}
	}
	if t.v != nil {
		r.resetDTS(t.v)
		t.v.cls = media.NewNALClassifier(t.v.codec)
		t.v.reorder, _ = h26x.ReorderOf(t.v.hcodec, nil)
	}
}

// place gives t, which has no offset, one: from its SR pair when it can,
// else by arrival. It reports whether that moved another track's offset
// while that track had returned frames.
func (r *rtpFrames) place(t *rtpTrack, ts int64, arrival time.Time) bool {
	if t != r.ref {
		r.alignSR(t)
	}
	if !t.hasOff {
		t.off = 0
		if r.anchored {
			t.off = r.anchorPTS + media.FromDuration(arrival.Sub(r.anchorWall)) - t.to90(ts)
		}
		t.hasOff = true
		r.n.syncByArrival.Add(1)
	}
	if t == r.ref {
		return r.alignOthers()
	}
	return r.syncRef()
}

// push converts one AU and returns its frames: none or one for video, one
// per AAC frame for audio. AUs of tracks that are not converted are counted
// (unknown_track) and skipped. An AU whose parameter sets are bad is
// counted (bad_aus) and dropped, and push returns an error describing it;
// the error is not terminal. When discontinuity is set, the frames returned
// are the first after the break.
func (r *rtpFrames) push(au rtsp.AU) (frames []Frame, discontinuity bool, err error) {
	t := r.track(au.Track)
	if t == nil {
		r.n.unknownTrack.Add(1)
		return nil, false, nil
	}
	ts := t.unw.Unwrap(uint64(au.Timestamp))
	disc := false
	if t.hasLast && absInt(ts-t.last) > rtpMaxJump*t.clock {
		r.n.timelineBreaks.Add(1)
		r.breakTimeline(t)
		disc = r.report(t)
	}
	t.last, t.hasLast = ts, true
	if !t.hasOff {
		disc = r.place(t, ts, au.Arrival) || disc
	}
	pts := t.to90(ts)
	r.anchorWall, r.anchorPTS, r.anchored = au.Arrival, pts+t.off, true
	if au.Lost {
		r.n.lostAUs.Add(1)
		return nil, disc, nil
	}
	var brk bool
	switch t.kind {
	case media.KindVideo:
		frames, brk, err = r.video(t, au, pts)
	case media.KindAudio:
		frames = r.audio(t, au, ts)
	}
	if brk {
		disc = r.report(t) || disc
	}
	if err != nil {
		r.n.badAUs.Add(1)
		return nil, disc, err
	}
	if len(frames) > 0 {
		t.live = true
		r.n.frames.Add(uint64(len(frames)))
	}
	return frames, disc, nil
}

func (r *rtpFrames) audio(t *rtpTrack, au rtsp.AU, ts int64) []Frame {
	out := make([]Frame, 0, len(au.Frames))
	for i, data := range au.Frames {
		if len(data) == 0 {
			continue
		}
		pts := t.to90(ts+int64(i)*t.spa) + t.off
		out = append(out, Frame{
			Frame:   media.Frame{Track: t.index, Config: t.acfg, PTS: pts, DTS: pts, Key: true, Data: [][]byte{data}},
			Arrival: au.Arrival,
		})
	}
	return out
}

// video converts a video AU presented at pts (track time, 90 kHz). brk
// reports a DTSBreak.
func (r *rtpFrames) video(t *rtpTrack, au rtsp.AU, pts int64) (out []Frame, brk bool, err error) {
	v := t.v
	data, err := r.updateSets(t, au.NALUs)
	if err != nil {
		return nil, false, err
	}
	if v.cfg == nil {
		r.n.noConfig.Add(1)
		return nil, false, nil
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	key, rasl := v.cls.Classify(data, pts)
	if v.dts == nil {
		r.newExtractor(v)
	}
	dts, st := v.dts.Extract(pts, key)
	if st == h26x.DTSBreak {
		brk = true
		r.dtsStats(v)
		if v.recreate {
			r.newExtractor(v)
		}
		v.emitted = false
		dts, st = v.dts.Extract(pts, key)
	}
	r.dtsStats(v)
	if st != h26x.DTSOK {
		return nil, brk, nil
	}
	v.emitted = true
	return []Frame{{
		Frame:   media.Frame{Track: t.index, Config: v.cfg, PTS: pts + t.off, DTS: dts + t.off, Key: key, RASL: rasl, Data: data},
		Arrival: au.Arrival,
	}}, brk, nil
}

// updateSets moves the parameter sets of nalus into the track's sets,
// rebuilds the config when they changed (or when it is invalid and a set
// arrived), applies a new SPS to the DTS extractor, and returns the other
// NAL units, AUDs dropped. A bad set invalidates the config.
func (r *rtpFrames) updateSets(t *rtpTrack, nalus [][]byte) ([][]byte, error) {
	v := t.v
	data := make([][]byte, 0, len(nalus))
	sets, changed, retry := v.sets, false, false
	var (
		badSet error
		newSPS *h26x.SPS
	)
	for _, n := range nalus {
		role := roleOf(v.codec, n)
		m := sets.of(role)
		switch {
		case role == nalAUD:
			continue
		case m == nil:
			data = append(data, n)
			continue
		}
		id, err := media.ParamSetID(v.codec, n)
		var sps *h26x.SPS
		if err == nil && role == nalSPS && !bytes.Equal(m[id], n) {
			sps, err = h26x.ParseSPS(v.hcodec, n)
		}
		if err != nil {
			badSet = err
			continue
		}
		retry = true
		if bytes.Equal(m[id], n) {
			continue
		}
		if !changed {
			sets, changed = sets.clone(), true
			m = sets.of(role)
		}
		m[id] = n
		if sps != nil {
			newSPS = sps
		}
	}
	v.sets = sets
	if newSPS != nil {
		r.applySPS(v, newSPS)
	}
	switch {
	case badSet != nil:
		// The slices may depend on the set that was lost.
		v.cfg = nil
		return nil, fmt.Errorf("ingest: rtp: track %d parameter set: %w", t.index, badSet)
	case changed || retry && v.cfg == nil:
		cfg, err := buildVideoConfig(v.codec, sets)
		if err != nil {
			v.cfg = nil
			return nil, fmt.Errorf("ingest: rtp: track %d: %w", t.index, err)
		}
		v.cfg = r.cache.Stable(t.index, cfg)
	}
	return data, nil
}

func buildVideoConfig(codec media.Codec, sets paramSets) (*media.TrackConfig, error) {
	if codec == media.CodecH265 {
		return media.NewH265Config(values(sets.vps), values(sets.sps), values(sets.pps))
	}
	return media.NewH264Config(values(sets.sps), values(sets.pps))
}

// applySPS takes the reorder depth and frame duration of a new SPS.
func (r *rtpFrames) applySPS(v *rtpVideo, sps *h26x.SPS) {
	reorder, assumed := h26x.ReorderOf(v.hcodec, sps)
	if assumed {
		r.n.reorderAssumed.Add(1)
	}
	v.reorder = reorder
	if v.dts == nil {
		v.frameDur = sps.FrameDuration
		r.newExtractor(v)
		return
	}
	v.dts.SetReorder(reorder)
	if sps.FrameDuration != v.frameDur {
		v.frameDur = sps.FrameDuration
		v.recreate = true
		if !v.emitted {
			r.newExtractor(v)
		}
	}
}

// newExtractor replaces the DTS extractor with one for the current reorder
// depth and frame duration.
func (r *rtpFrames) newExtractor(v *rtpVideo) {
	if v.dts != nil {
		r.dtsStats(v)
	}
	v.dts = h26x.NewDTSExtractor(v.reorder, v.frameDur)
	v.seen, v.recreate, v.emitted = h26x.DTSStats{}, false, false
}

// resetDTS starts a new timeline on the extractor, recreating it when the
// frame duration changed.
func (r *rtpFrames) resetDTS(v *rtpVideo) {
	switch {
	case v.dts == nil:
	case v.recreate:
		r.newExtractor(v)
	default:
		v.dts.SetReorder(v.reorder)
		v.dts.Reset()
		v.emitted = false
	}
}

// dtsStats adds what the extractor counted since the last call.
func (r *rtpFrames) dtsStats(v *rtpVideo) {
	s := v.dts.Stats()
	r.n.dtsLeading.Add(s.Leading - v.seen.Leading)
	r.n.dtsLate.Add(s.Late - v.seen.Late)
	r.n.dtsBreaks.Add(s.Breaks - v.seen.Breaks)
	r.n.dtsReorderRaised.Add(s.ReorderRaised - v.seen.ReorderRaised)
	v.seen = s
}

// stats returns the session's counters.
func (r *rtpFrames) stats() map[string]uint64 {
	return map[string]uint64{
		"frames":             r.n.frames.Load(),
		"lost_aus":           r.n.lostAUs.Load(),
		"dropped_no_config":  r.n.noConfig.Load(),
		"bad_aus":            r.n.badAUs.Load(),
		"unknown_track":      r.n.unknownTrack.Load(),
		"sync_by_arrival":    r.n.syncByArrival.Load(),
		"sync_corrected":     r.n.syncCorrected.Load(),
		"sync_jumps":         r.n.syncJumps.Load(),
		"timeline_breaks":    r.n.timelineBreaks.Load(),
		"restarts":           r.n.restarts.Load(),
		"reorder_assumed":    r.n.reorderAssumed.Load(),
		"dts_leading":        r.n.dtsLeading.Load(),
		"dts_late":           r.n.dtsLate.Load(),
		"dts_breaks":         r.n.dtsBreaks.Load(),
		"dts_reorder_raised": r.n.dtsReorderRaised.Load(),
	}
}
