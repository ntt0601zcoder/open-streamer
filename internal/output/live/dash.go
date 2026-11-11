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
// has an init segment and a listed part in it. It returns nil when nothing is
// listed.
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

func buildPeriod(s Snapshot, e EpochInfo) (dash.Period, bool) {
	per := dash.Period{ID: "e" + strconv.FormatUint(uint64(e.Epoch), 10), Start: dash.Duration(e.Start)}
	for _, et := range e.Tracks {
		if et.Init == nil || et.Config == nil {
			continue
		}
		if as, ok := buildSet(s, e, et); ok {
			per.AdaptationSets = append(per.AdaptationSets, as)
		}
	}
	return per, len(per.AdaptationSets) > 0
}

func buildSet(s Snapshot, e EpochInfo, et EpochTrack) (dash.AdaptationSet, bool) {
	ts := fmp4.Timescale(et.Config)
	if ts == 0 {
		return dash.AdaptationSet{}, false
	}
	var tl dash.SegmentTimeline
	var first uint64
	peak := 1.0
	for _, seg := range s.Segments[:s.Listed] {
		if seg.Epoch != e.Epoch {
			continue
		}
		for _, part := range seg.Parts {
			if part.Track != et.Track || part.Timing.Duration == 0 {
				continue
			}
			if len(tl.S) == 0 {
				first = part.N
			}
			t, d := part.Timing.Earliest, part.Timing.Duration
			tl.S = append(tl.S, dash.S{T: &t, D: d})
			peak = max(peak, float64(len(part.Data))*8*float64(ts)/float64(d))
		}
	}
	if len(tl.S) == 0 {
		return dash.AdaptationSet{}, false
	}
	pto := max(media.Rescale(e.Anchor.Start, int(ts)), 0)
	track := strconv.Itoa(et.Track)
	rep := dash.Representation{ID: track, Bandwidth: int(math.Ceil(peak))}
	if c, err := media.CodecString(et.Config); err == nil {
		rep.Codecs = c
	}
	as := dash.AdaptationSet{
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
	return as, true
}
