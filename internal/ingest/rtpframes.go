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
	// track may be from its provisional offset and still keep it: 50 ms,
	// plus the reference's reorder lead.
	rtpFirstSRTolerance = media.ClockRate / 20
	// rtpSRMove is how far later sender reports must move a track's offset
	// for it to change: 100 ms, plus the reference's reorder lead.
	rtpSRMove = media.ClockRate / 10
	// rtpDefaultFrameDur is the frame duration assumed for the reorder lead
	// when the SPS has no timing, as h26x.DTSExtractor assumes.
	rtpDefaultFrameDur = 3000
	// aacFrameSamples is the samples of one AAC frame at the core rate.
	aacFrameSamples = 1024
	// rtpMaxMedia bounds the media indexes taken from a description, as
	// pkg/rtsp bounds the media of an SDP.
	rtpMaxMedia = 16
)

// ntpEpoch is an NTP timestamp of zero, which cameras send before their
// clock is synchronised.
var ntpEpoch = time.Unix(-2208988800, 0)

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
// frame's DTS (an audio frame's PTS, a video frame's DTS from the
// extractor) sits where the latest frame of any track puts its arrival
// (sync_by_arrival); the anchor is that frame's arrival and DTS. Senders
// send in decode order, so arrival follows the DTS, which video PTS lead by
// up to the reorder depth. A video AU that yields no frame neither places
// its track nor moves the anchor.
//
// Sender reports. The reference track is the first video track, else the
// first converted track; sender reports align every other track to it. A
// track's SR offset is evaluated only on a fresh pair: both the reference's
// SR and its own were updated since its last evaluation. Senders such as
// MediaMTX build SRs from packet arrival, so each pair carries the video
// reorder lead as jitter (up to R frames). The thresholds are 50 ms for the
// first evaluation after the track was placed provisionally and 100 ms
// after that, each plus the lead: the reference's R times its frame
// duration, 0 for an audio reference. A pair beyond the threshold is a
// candidate; the offset changes only when the next fresh pair is beyond it
// too, on the same side, and within the threshold of the candidate: then
// the latest pair's offset applies (sync_corrected for the first change,
// sync_jumps after), one discontinuity. A candidate the next fresh pair
// does not confirm is counted (sync_unconfirmed). A track whose offset is
// still unknown when a pair is known takes the SR offset directly; so does
// the reference, from another track's offset, when that track was placed
// before the reference had an offset. Sender reports with a zero NTP time
// are ignored (sr_ignored).
//
// Timeline breaks. A track whose RTP timestamp steps by more than 10 s, an
// audio track whose timestamp steps back (timeline_breaks), or a track that
// restart reports as restarted (a new SSRC), drops its SR and any pending
// candidate, has its DTS extractor reset and is placed again by arrival;
// when it is the reference, every other track's next SR evaluation counts
// as a first one again. An audio AU with the timestamp of the
// previous one is a duplicate: it is dropped (duplicate_aus). A DTSBreak
// from the extractor (video stepping back past its bound, short of 10 s)
// is a break too: the track drops its SR and is placed again by arrival,
// the extractor having reset itself.
//
// Video. Parameter sets come from the SDP sprop sets and in band, through
// the shared ConfigCache, as in flvFrames: a bad set invalidates the config
// and frames are dropped (dropped_no_config) until the sets rebuild a valid
// one. Key and RASL come from a media.NALClassifier, DTS from an
// h26x.DTSExtractor whose reorder depth follows the newest SPS (SetReorder)
// and whose frame duration is the SPS's: when a new SPS changes it, the
// extractor is recreated at its next reset, or at once if it has not
// emitted since its last one. The extractor is reset only on a timeline
// break. Lost AUs are dropped (lost_aus) and reset nothing.
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
	// skipReason says why a media index is not converted.
	skipReason map[int]string

	// The latest frame's arrival and session-timeline DTS, for placing a
	// track by arrival.
	anchorWall time.Time
	anchorDTS  int64
	anchored   bool

	n rtpCounters
}

type rtpCounters struct {
	frames, lostAUs, noConfig, badAUs, unknownTrack, syncByArrival,
	syncCorrected, syncJumps, syncUnconfirmed, srIgnored, timelineBreaks, restarts,
	duplicates, reorderAssumed,
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
	// prevAU is the unwrapped RTP time of the last AU itself; hasPrevAU
	// tells whether there is one since the start or the last restart.
	prevAU    int64
	hasPrevAU bool
	// off maps the track's 90 kHz time onto the session timeline.
	off    int64
	hasOff bool
	// sr is the latest sender report and srSeq counts them; paired tells
	// whether the track's first SR evaluation is behind it.
	sr     rtpSR
	srSeq  uint64
	paired bool
	// evalOwn and evalRef are the track's and the reference's srSeq at the
	// track's last SR evaluation; cand is a pending SR offset.
	evalOwn, evalRef uint64
	cand             int64
	hasCand          bool
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
// A media that is not converted gets its reason in r.skipReason.
func (r *rtpFrames) newTrack(m rtsp.Media) *rtpTrack {
	t, reason := r.buildTrack(m)
	if t == nil {
		if r.skipReason == nil {
			r.skipReason = map[int]string{}
		}
		r.skipReason[m.Index] = reason
	}
	return t
}

func (r *rtpFrames) buildTrack(m rtsp.Media) (*rtpTrack, string) {
	if m.Reason != "" {
		return nil, m.Reason
	}
	t := &rtpTrack{index: m.Index, clock: int64(m.ClockRate), unw: unwrap.New(32)}
	switch m.Codec {
	case rtsp.CodecH264, rtsp.CodecH265:
		if m.ClockRate != rtpVideoClock {
			return nil, fmt.Sprintf("video clock %d", m.ClockRate)
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
			return nil, "AAC without a format or clock"
		}
		cfg, err := media.NewAACConfig(m.AAC.Config)
		if err != nil {
			return nil, "invalid AAC config"
		}
		t.kind = media.KindAudio
		t.acfg = r.cache.Stable(m.Index, cfg)
		t.spa = max(1, aacFrameSamples*t.clock/int64(cfg.SampleRate))
	case rtsp.CodecOther:
		return nil, "codec not supported"
	default:
		return nil, "unknown codec"
	}
	return t, ""
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
	if !sr.NTP.After(ntpEpoch) {
		r.n.srIgnored.Add(1)
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
	t.srSeq++
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

// alignSR applies t's SR pair to its offset: a track without an offset
// takes it; otherwise a fresh pair is evaluated by the confirmation rule
// (see rtpFrames). It reports whether that is a discontinuity.
func (r *rtpFrames) alignSR(t *rtpTrack) bool {
	want, ok := r.srOffset(t)
	if !ok {
		return false
	}
	ref := r.ref
	if !t.hasOff {
		t.off, t.hasOff, t.paired = want, true, true
		t.evalOwn, t.evalRef, t.hasCand = t.srSeq, ref.srSeq, false
		return false
	}
	if t.srSeq == t.evalOwn || ref.srSeq == t.evalRef {
		return false // a stale pair
	}
	t.evalOwn, t.evalRef = t.srSeq, ref.srSeq
	thr := int64(rtpSRMove)
	if !t.paired {
		thr = rtpFirstSRTolerance
	}
	thr += r.lead()
	d := want - t.off
	if absInt(d) <= thr {
		if t.hasCand {
			r.n.syncUnconfirmed.Add(1)
		}
		t.hasCand, t.paired = false, true
		return false
	}
	if !t.hasCand || (t.cand-t.off > 0) != (d > 0) || absInt(want-t.cand) > thr {
		if t.hasCand {
			r.n.syncUnconfirmed.Add(1)
		}
		t.cand, t.hasCand = want, true
		return false
	}
	if t.paired {
		r.n.syncJumps.Add(1)
	} else {
		r.n.syncCorrected.Add(1)
	}
	t.off, t.hasCand, t.paired = want, false, true
	return r.report(t)
}

// lead is the reference's reorder lead in 90 kHz ticks: R frame durations,
// the jitter that SRs built from video arrival carry. It is 0 for an audio
// reference.
func (r *rtpFrames) lead() int64 {
	v := r.ref.v
	if v == nil {
		return 0
	}
	dur := v.frameDur
	if dur <= 0 {
		dur = rtpDefaultFrameDur
	}
	return int64(v.reorder) * dur
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
		o.evalOwn, o.evalRef, o.hasCand = o.srSeq, ref.srSeq, false
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
	t.hasLast, t.hasPrevAU = false, false
	r.breakTimeline(t)
	return r.report(t)
}

// breakTimeline drops what ties t to its old timeline, its DTS extractor
// and classifier state included. The reorder depth stays the current
// SPS's.
func (r *rtpFrames) breakTimeline(t *rtpTrack) {
	r.unbind(t)
	if t.v != nil {
		r.resetDTS(t.v)
		t.v.cls = media.NewNALClassifier(t.v.codec)
	}
}

// unbind drops t's offset and SR, so that it is placed again.
func (r *rtpFrames) unbind(t *rtpTrack) {
	t.hasOff, t.sr, t.paired, t.hasCand = false, rtpSR{}, false, false
	if t == r.ref {
		for _, o := range r.tracks {
			if o != nil {
				o.paired, o.hasCand = false, false
			}
		}
	}
}

// place gives t, which has no offset, one: from its SR pair when it can,
// else by arrival, dts (track time, 90 kHz) being the DTS of its first
// frame. It reports whether that moved another track's offset while that
// track had returned frames.
func (r *rtpFrames) place(t *rtpTrack, dts int64, arrival time.Time) bool {
	if t != r.ref {
		r.alignSR(t)
	}
	if !t.hasOff {
		t.off = 0
		if r.anchored {
			t.off = r.anchorDTS + media.FromDuration(arrival.Sub(r.anchorWall)) - dts
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
	broken := t.hasLast && absInt(ts-t.last) > rtpMaxJump*t.clock
	if t.kind == media.KindAudio && t.hasPrevAU {
		// Audio has no DTS extractor to catch a step back, and its PTS
		// must increase strictly. The previous AU, not an RTP-Info or SR
		// seed, is the reference: those may lie ahead of the first AUs.
		switch {
		case ts == t.prevAU:
			r.n.duplicates.Add(1)
			return nil, false, nil
		case ts < t.prevAU:
			// A forward step up to 10 s is a gap (loss): the RTP clock
			// keeps the PTS right, and the SR alignment holds.
			broken = true
		}
	}
	disc := false
	if broken {
		r.n.timelineBreaks.Add(1)
		r.breakTimeline(t)
		disc = r.report(t)
	}
	t.last, t.hasLast = ts, true
	t.prevAU, t.hasPrevAU = ts, true
	pts := t.to90(ts)
	if t.kind == media.KindAudio && !t.hasOff {
		disc = r.place(t, pts, au.Arrival) || disc
	}
	if au.Lost {
		if t.kind == media.KindAudio {
			r.setAnchor(au.Arrival, pts+t.off)
		}
		r.n.lostAUs.Add(1)
		return nil, disc, nil
	}
	switch t.kind {
	case media.KindVideo:
		var brk bool
		frames, brk, err = r.video(t, au, pts)
		if brk {
			// The extractor reset itself and took this frame as the first
			// of the new timeline; the track is placed again by arrival.
			disc = r.report(t) || disc
			r.unbind(t)
		}
		if len(frames) > 0 {
			if !t.hasOff {
				disc = r.place(t, frames[0].DTS, au.Arrival) || disc
			}
			for i := range frames {
				frames[i].PTS += t.off
				frames[i].DTS += t.off
			}
			r.setAnchor(au.Arrival, frames[0].DTS)
		}
	case media.KindAudio:
		frames = r.audio(t, au, ts)
		r.setAnchor(au.Arrival, pts+t.off)
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

func (r *rtpFrames) setAnchor(wall time.Time, dts int64) {
	r.anchorWall, r.anchorDTS, r.anchored = wall, dts, true
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

// video converts a video AU presented at pts (track time, 90 kHz) and
// returns its frame in track time: the caller adds the offset. brk reports
// a DTSBreak.
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
		// The classifier is kept: it was asked about this frame already,
		// and the extractor now drops every frame until the next key,
		// which resets the classifier's key PTS (IDR) or replaces it
		// (recovery point); a stale RASL mark only reaches dropped frames.
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
		Frame:   media.Frame{Track: t.index, Config: v.cfg, PTS: pts, DTS: dts, Key: key, RASL: rasl, Data: data},
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
		"sync_unconfirmed":   r.n.syncUnconfirmed.Load(),
		"sr_ignored":         r.n.srIgnored.Load(),
		"duplicate_aus":      r.n.duplicates.Load(),
		"timeline_breaks":    r.n.timelineBreaks.Load(),
		"restarts":           r.n.restarts.Load(),
		"reorder_assumed":    r.n.reorderAssumed.Load(),
		"dts_leading":        r.n.dtsLeading.Load(),
		"dts_late":           r.n.dtsLate.Load(),
		"dts_breaks":         r.n.dtsBreaks.Load(),
		"dts_reorder_raised": r.n.dtsReorderRaised.Load(),
	}
}
