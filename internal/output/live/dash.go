package live

import (
	"math"
	"strconv"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
)

const typeMPD = "application/dash+xml"

func init() {
	playlistRoutes = append(playlistRoutes, playlistRoute{
		dir:   dirCMAF,
		match: func(name string) bool { return name == "index.mpd" },
		build: func(_ *Packager, s Snapshot, _ string) ([]byte, string, bool) {
			body := buildMPD(s)
			return body, typeMPD, body != nil
		},
	})
}

// buildMPD renders cmaf/index.mpd from the listed prefix of the window: one
// Period per epoch that has a listed part, one AdaptationSet per track that
// has an init segment, a codec string and a listed part in it (a track
// without a codec string was counted once in Packager.CodecErrors). With
// Snapshot.ABR, video tracks of one codec share an AdaptationSet instead
// (see buildPeriod). It returns nil when nothing is listed.
func buildMPD(s Snapshot) []byte {
	if s.Listed == 0 || s.AST.IsZero() {
		return nil
	}
	target := media.ToDuration(s.Target)
	now := s.Now
	m := dash.MPD{
		Type:                       "dynamic",
		Profiles:                   dash.ProfileLive,
		AvailabilityStartTime:      &s.AST,
		PublishTime:                &now,
		MinimumUpdatePeriod:        dash.Duration(target),
		TimeShiftBufferDepth:       dash.Duration(target * time.Duration(s.Window)),
		SuggestedPresentationDelay: dash.Duration(3 * target),
		MinBufferTime:              dash.Duration(target),
		UTCTiming: &dash.UTCTiming{
			SchemeIDURI: dash.UTCTimingDirect,
			Value:       now.UTC().Format(time.RFC3339Nano),
		},
	}
	for _, e := range s.Epochs {
		if per, ok := buildPeriod(s, e); ok {
			m.Periods = append(m.Periods, per)
		}
	}
	if len(m.Periods) == 0 {
		return nil
	}
	return m.Encode()
}

// buildPeriod returns an epoch's Period, if any track has a set in it. Sets
// are in order of their lowest track.
//
// With Snapshot.ABR (ruling R-B6) the video tracks with the same codec share
// one AdaptationSet, each Representation with its own SegmentTemplate, and
// the set has no segmentAlignment attribute: part numbers stay per track, so
// after a gap the Representations' numbers differ. Audio tracks keep one set
// each.
func buildPeriod(s Snapshot, e EpochInfo) (dash.Period, bool) {
	per := dash.Period{ID: "e" + strconv.FormatUint(uint64(e.Epoch), 10), Start: dash.Duration(e.Start)}
	videoSet := map[media.Codec]int{} // ABR: codec -> index of its set in per.AdaptationSets
	for _, et := range e.Tracks {
		if et.Init == nil || et.Config == nil || et.Codec == "" {
			continue
		}
		if !s.ABR || et.Config.Codec.Kind() != media.KindVideo {
			if as, ok := buildSet(s, e, et); ok {
				per.AdaptationSets = append(per.AdaptationSets, as)
			}
			continue
		}
		rep, tpl, ok := buildRep(s, e, et)
		if !ok {
			continue
		}
		rep.SegmentTemplate = tpl
		if i, found := videoSet[et.Config.Codec]; found {
			per.AdaptationSets[i].Representations = append(per.AdaptationSets[i].Representations, rep)
			continue
		}
		videoSet[et.Config.Codec] = len(per.AdaptationSets)
		per.AdaptationSets = append(per.AdaptationSets, dash.AdaptationSet{
			ContentType:     "video",
			MimeType:        "video/mp4",
			Representations: []dash.Representation{rep},
		})
	}
	return per, len(per.AdaptationSets) > 0
}

// buildSet returns the track's set, with the SegmentTemplate at set level,
// if it has a listed part.
func buildSet(s Snapshot, e EpochInfo, et EpochTrack) (dash.AdaptationSet, bool) {
	rep, tpl, ok := buildRep(s, e, et)
	if !ok {
		return dash.AdaptationSet{}, false
	}
	as := dash.AdaptationSet{SegmentAlignment: true, SegmentTemplate: tpl}
	if et.Config.Codec.Kind() == media.KindAudio {
		as.ContentType, as.MimeType = "audio", "audio/mp4"
	} else {
		as.ContentType, as.MimeType = "video", "video/mp4"
	}
	as.Representations = []dash.Representation{rep}
	return as, true
}

// buildRep returns the track's Representation, without a template, and its
// SegmentTemplate, if it has a listed part in the epoch.
func buildRep(s Snapshot, e EpochInfo, et EpochTrack) (dash.Representation, *dash.SegmentTemplate, bool) {
	ts := fmp4.Timescale(et.Config)
	if ts == 0 {
		return dash.Representation{}, nil, false
	}
	var tl dash.SegmentTimeline
	var first uint64
	peak := 1.0
	for _, seg := range s.Segments[:s.Listed] {
		if seg.Epoch != e.Epoch {
			continue
		}
		for _, part := range seg.Parts {
			if part.Track != et.Track { // part.N is consecutive within an epoch: the packager never leaves a hole
				continue
			}
			if len(tl.S) == 0 {
				first = part.N
			}
			t, d := part.Timing.Earliest, part.Timing.Duration
			tl.S = append(tl.S, dash.S{T: &t, D: d})
			if d > 0 {
				peak = max(peak, float64(len(part.Data))*8*float64(ts)/float64(d))
			}
		}
	}
	if len(tl.S) == 0 {
		return dash.Representation{}, nil, false
	}
	pto := max(media.Rescale(e.Anchor.Start, int(ts)), 0)
	track := strconv.Itoa(et.Track)
	rep := dash.Representation{ID: track, Codecs: et.Codec, Bandwidth: int(math.Ceil(peak))}
	tpl := &dash.SegmentTemplate{
		Timescale:              ts,
		PresentationTimeOffset: uint64(pto),
		Initialization:         "init-" + track + "-" + strconv.FormatUint(uint64(e.Epoch), 10) + ".mp4",
		Media:                  track + "-$Number$.m4s",
		StartNumber:            &first,
		Timeline:               &tl,
	}
	if et.Config.Codec.Kind() == media.KindAudio {
		rep.AudioSamplingRate = et.Config.SampleRate
		rep.AudioChannelConfiguration = &dash.AudioChannelConfiguration{
			SchemeIDURI: dash.AudioChannelScheme,
			Value:       strconv.Itoa(et.Config.Channels),
		}
	} else {
		rep.Width, rep.Height = et.Config.Width, et.Config.Height
	}
	return rep, tpl, true
}
