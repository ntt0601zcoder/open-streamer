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
		build: func(p *Packager, s Snapshot, _ string) ([]byte, string, bool) {
			body, skipped := buildMPD(s)
			p.errors.Add(uint64(skipped))
			return body, typeMPD, body != nil
		},
	})
}

// buildMPD renders cmaf/index.mpd from the listed prefix of the window: one
// Period per epoch that has a listed part, one AdaptationSet per track that
// has an init segment and a listed part in it. It returns nil when nothing is
// listed. skipped counts the adaptation sets left out because their codec
// string could not be built.
func buildMPD(s Snapshot) (body []byte, skipped int) {
	if s.Listed == 0 || s.AST.IsZero() {
		return nil, 0
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
		per, ok, n := buildPeriod(s, e)
		skipped += n
		if ok {
			m.Periods = append(m.Periods, per)
		}
	}
	if len(m.Periods) == 0 {
		return nil, skipped
	}
	return m.Encode(), skipped
}

func buildPeriod(s Snapshot, e EpochInfo) (per dash.Period, ok bool, skipped int) {
	per = dash.Period{ID: "e" + strconv.FormatUint(uint64(e.Epoch), 10), Start: dash.Duration(e.Start)}
	for _, et := range e.Tracks {
		if et.Init == nil || et.Config == nil {
			continue
		}
		as, ok, err := buildSet(s, e, et)
		if err {
			skipped++
		}
		if ok {
			per.AdaptationSets = append(per.AdaptationSets, as)
		}
	}
	return per, len(per.AdaptationSets) > 0, skipped
}

// buildSet returns the track's set; failed reports a codec string error.
func buildSet(s Snapshot, e EpochInfo, et EpochTrack) (as dash.AdaptationSet, ok, failed bool) {
	ts := fmp4.Timescale(et.Config)
	codec, err := media.CodecString(et.Config)
	if ts == 0 || err != nil {
		return dash.AdaptationSet{}, false, ts != 0
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
		return dash.AdaptationSet{}, false, false
	}
	pto := max(media.Rescale(e.Anchor.Start, int(ts)), 0)
	track := strconv.Itoa(et.Track)
	rep := dash.Representation{ID: track, Codecs: codec, Bandwidth: int(math.Ceil(peak))}
	as = dash.AdaptationSet{
		SegmentAlignment: true,
		SegmentTemplate: &dash.SegmentTemplate{
			Timescale:              ts,
			PresentationTimeOffset: uint64(pto),
			Initialization:         "init-" + track + "-" + strconv.FormatUint(uint64(e.Epoch), 10) + ".mp4",
			Media:                  track + "-$Number$.m4s",
			StartNumber:            &first,
			Timeline:               &tl,
		},
	}
	if et.Config.Codec.Kind() == media.KindAudio {
		as.ContentType, as.MimeType = "audio", "audio/mp4"
		rep.AudioSamplingRate = et.Config.SampleRate
		rep.AudioChannelConfiguration = &dash.AudioChannelConfiguration{
			SchemeIDURI: dash.AudioChannelScheme,
			Value:       strconv.Itoa(et.Config.Channels),
		}
	} else {
		as.ContentType, as.MimeType = "video", "video/mp4"
		rep.Width, rep.Height = et.Config.Width, et.Config.Height
	}
	as.Representations = []dash.Representation{rep}
	return as, true, false
}
