package live

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

const (
	typeM3U8 = "application/vnd.apple.mpegurl"

	hlsVersionTS   = 3
	hlsVersionFMP4 = 7
	audioGroup     = "aud"
)

func init() {
	playlistRoutes = append(playlistRoutes,
		playlistRoute{
			dir:   dirHLS,
			match: func(name string) bool { return name == "index.m3u8" },
			build: func(p *Packager, s Snapshot, _ string) ([]byte, string, bool) {
				return finish(buildTSPlaylist(p, s))
			},
		},
		playlistRoute{
			dir:   dirCMAF,
			match: func(name string) bool { return name == "master.m3u8" },
			build: func(_ *Packager, s Snapshot, _ string) ([]byte, string, bool) {
				return finish(buildMaster(s))
			},
		},
		playlistRoute{
			dir: dirCMAF,
			match: func(name string) bool {
				_, _, ok := parseTrackPlaylist(name)
				return ok
			},
			build: func(p *Packager, s Snapshot, name string) ([]byte, string, bool) {
				kind, track, _ := parseTrackPlaylist(name)
				return finish(buildTrackPlaylist(p, s, kind, track, name))
			},
		},
	)
}

func finish(body []byte) ([]byte, string, bool) {
	return body, typeM3U8, body != nil
}

// parseTrackPlaylist parses "v{track}.m3u8" and "a{track}.m3u8".
func parseTrackPlaylist(name string) (media.Kind, int, bool) {
	base, ok := strings.CutSuffix(name, ".m3u8")
	if !ok || len(base) < 2 {
		return 0, 0, false
	}
	var kind media.Kind
	switch base[0] {
	case 'v':
		kind = media.KindVideo
	case 'a':
		kind = media.KindAudio
	default:
		return 0, 0, false
	}
	n, ok := parseUint(base[1:], 31)
	return kind, int(n), ok
}

// targetDuration returns the playlist's EXT-X-TARGETDURATION: ceil(Target)
// seconds, raised when a listed EXTINF rounds above it, and never lowered
// again for the same playlist key.
func (p *Packager) targetDuration(key string, s Snapshot, extinfs []time.Duration) int {
	td := int(math.Ceil(float64(s.Target) / media.ClockRate))
	for _, d := range extinfs {
		td = max(td, int(math.Round(d.Round(time.Millisecond).Seconds()))) // as printed by EXTINF
	}
	p.tdMu.Lock()
	defer p.tdMu.Unlock()
	if p.td == nil {
		p.td = map[string]int{}
	}
	td = max(td, p.td[key])
	p.td[key] = td
	return td
}

// independent reports whether every listed segment of the given epochs
// starts on an IDR: their video is H.264 (at least one video track, and none
// of another codec) and none of those segments is Forced.
func independent(s Snapshot, epochs map[uint32]bool) bool {
	for _, seg := range s.Segments[:s.Listed] {
		if seg.Forced && epochs[seg.Epoch] {
			return false
		}
	}
	h264 := false
	for _, e := range s.Epochs {
		if !epochs[e.Epoch] {
			continue
		}
		for _, t := range e.Tracks {
			if t.Config == nil || t.Config.Codec.Kind() != media.KindVideo {
				continue
			}
			if t.Config.Codec != media.CodecH264 {
				return false
			}
			h264 = true
		}
	}
	return h264
}

// pdt returns the program date-time of stream time pts in an epoch.
func pdt(s Snapshot, epoch uint32, pts int64) time.Time {
	e, _ := s.Epoch(epoch)
	return e.WallAt(pts)
}

// buildTSPlaylist renders hls/index.m3u8: the listed segments that have TS
// bytes. It returns nil when there are none.
func buildTSPlaylist(p *Packager, s Snapshot) []byte {
	pl := hls.MediaPlaylist{Version: hlsVersionTS, DiscontinuitySequence: s.EvictedTSDiscontinuities}
	epochs := map[uint32]bool{}
	var extinfs []time.Duration
	for _, seg := range s.Segments[:s.Listed] {
		if seg.TS == nil {
			continue
		}
		if len(pl.Segments) == 0 {
			pl.MediaSequence = seg.TSSeq
		}
		e := hls.Segment{
			URI:           strconv.FormatUint(seg.Seq, 10) + ".ts",
			Duration:      media.ToDuration(seg.Duration),
			Discontinuity: seg.TSDiscontinuity,
		}
		if len(pl.Segments) == 0 || seg.TSDiscontinuity {
			e.ProgramDateTime = pdt(s, seg.Epoch, seg.Start)
		}
		pl.Segments = append(pl.Segments, e)
		extinfs = append(extinfs, e.Duration)
		epochs[seg.Epoch] = true
	}
	if len(pl.Segments) == 0 {
		return nil
	}
	pl.TargetDuration = p.targetDuration("hls/index", s, extinfs)
	pl.Independent = independent(s, epochs)
	return pl.Encode()
}

// trackEntry is one listed part of a track with its segment and epoch track.
type trackEntry struct {
	seg  SegmentInfo
	part CMAFPart
	et   EpochTrack
}

// trackEntries returns the listed parts of a track, oldest first.
func trackEntries(s Snapshot, track int) []trackEntry {
	var out []trackEntry
	for _, seg := range s.Segments[:s.Listed] {
		for _, part := range seg.Parts {
			if part.Track != track {
				continue
			}
			e, ok := s.Epoch(seg.Epoch)
			if !ok {
				continue
			}
			et, ok := e.Track(track)
			if !ok || et.Config == nil {
				continue
			}
			out = append(out, trackEntry{seg, part, et})
		}
	}
	return out
}

// partDuration returns a part's duration from its own timing.
func partDuration(e trackEntry) time.Duration {
	ts := uint64(fmp4.Timescale(e.et.Config))
	if ts == 0 {
		return 0
	}
	return time.Duration((e.part.Timing.Duration*uint64(time.Second) + ts/2) / ts)
}

// buildTrackPlaylist renders cmaf/v{track}.m3u8 or cmaf/a{track}.m3u8 for a
// track of the given kind. It returns nil when the track has no listed part.
//
// The playlist follows the track's own numbering (MEDIA-SEQUENCE is the N of
// its first entry) and its own discontinuity flags. Known limitation (M3a):
// when a track is absent for a whole epoch, no entry carries that epoch's
// discontinuity, so this playlist's discontinuity count diverges from the
// other renditions' (RFC 8216 section 6.2.4). No EXT-X-GAP placeholders are
// written.
func buildTrackPlaylist(p *Packager, s Snapshot, kind media.Kind, track int, name string) []byte {
	pl := hls.MediaPlaylist{Version: hlsVersionFMP4, DiscontinuitySequence: s.EvictedTrackDiscontinuities[track]}
	epochs := map[uint32]bool{}
	var extinfs []time.Duration
	for _, e := range trackEntries(s, track) {
		if e.et.Config.Codec.Kind() != kind {
			continue
		}
		if len(pl.Segments) == 0 {
			pl.MediaSequence = e.part.N
		}
		seg := hls.Segment{
			URI:           strconv.Itoa(track) + "-" + strconv.FormatUint(e.part.N, 10) + ".m4s",
			Duration:      partDuration(e),
			Discontinuity: e.part.Discontinuity,
			Map:           "init-" + strconv.Itoa(track) + "-" + strconv.FormatUint(uint64(e.seg.Epoch), 10) + ".mp4",
		}
		if len(pl.Segments) == 0 || e.part.Discontinuity {
			first := int64(0)
			if ts := int64(fmp4.Timescale(e.et.Config)); ts > 0 {
				first = int64(e.part.Timing.Earliest) * media.ClockRate / ts
			}
			seg.ProgramDateTime = pdt(s, e.seg.Epoch, first)
		}
		pl.Segments = append(pl.Segments, seg)
		extinfs = append(extinfs, seg.Duration)
		epochs[e.seg.Epoch] = true
	}
	if len(pl.Segments) == 0 {
		return nil
	}
	pl.TargetDuration = p.targetDuration("cmaf/"+name, s, extinfs)
	pl.Independent = independent(s, epochs)
	return pl.Encode()
}

// lowestTracks returns the lowest track of each kind with a listed part.
func lowestTracks(s Snapshot) (video, audio int, haveV, haveA bool) {
	for _, seg := range s.Segments[:s.Listed] {
		for _, part := range seg.Parts {
			e, ok := s.Epoch(seg.Epoch)
			if !ok {
				continue
			}
			et, ok := e.Track(part.Track)
			if !ok || et.Config == nil {
				continue
			}
			switch et.Config.Codec.Kind() {
			case media.KindVideo:
				if !haveV || part.Track < video {
					video, haveV = part.Track, true
				}
			case media.KindAudio:
				if !haveA || part.Track < audio {
					audio, haveA = part.Track, true
				}
			}
		}
	}
	return
}

// buildMaster renders cmaf/master.m3u8. It returns nil when no track has a
// listed part.
func buildMaster(s Snapshot) []byte {
	vTrack, aTrack, haveV, haveA := lowestTracks(s)
	if !haveV && !haveA {
		return nil
	}
	pl := hls.MultivariantPlaylist{Version: hlsVersionFMP4}
	v := hls.Variant{}
	epochs := map[uint32]bool{}
	var codecs []string
	bandwidth := map[uint64]float64{} // segment Seq -> bits per second summed over the chosen tracks
	add := func(track int, kind media.Kind) {
		for _, e := range trackEntries(s, track) {
			if e.et.Config.Codec.Kind() != kind {
				continue
			}
			epochs[e.seg.Epoch] = true
			if c, err := media.CodecString(e.et.Config); err == nil && !slices.Contains(codecs, c) {
				codecs = append(codecs, c)
			}
			if kind == media.KindVideo && e.et.Config.Width*e.et.Config.Height > v.Width*v.Height {
				v.Width, v.Height = e.et.Config.Width, e.et.Config.Height
			}
			if d := partDuration(e).Seconds(); d > 0 {
				bandwidth[e.seg.Seq] += float64(len(e.part.Data)) * 8 / d
			}
		}
	}
	if haveV {
		add(vTrack, media.KindVideo)
		v.URI = "v" + strconv.Itoa(vTrack) + ".m3u8"
	}
	if haveA {
		add(aTrack, media.KindAudio)
		uri := "a" + strconv.Itoa(aTrack) + ".m3u8"
		if haveV {
			pl.Renditions = []hls.Rendition{{Type: "AUDIO", GroupID: audioGroup, Name: "audio", URI: uri, Default: true}}
			v.Audio = audioGroup
		} else {
			v.URI = uri
		}
	}
	peak := 1.0
	for _, b := range bandwidth {
		peak = max(peak, b)
	}
	v.Bandwidth = int(math.Ceil(peak))
	v.Codecs = strings.Join(codecs, ",")
	pl.Variants = []hls.Variant{v}
	pl.Independent = independent(s, epochs)
	return pl.Encode()
}
