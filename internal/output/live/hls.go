package live

import (
	"maps"
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
				if s.ABR {
					return finish(buildTSMaster(s))
				}
				return finish(buildTSPlaylist(p, s))
			},
		},
		playlistRoute{
			dir: dirHLS,
			match: func(name string) bool {
				kind, _, ok := parseTrackPlaylist(name)
				return ok && kind == media.KindVideo
			},
			build: func(p *Packager, s Snapshot, name string) ([]byte, string, bool) {
				_, track, _ := parseTrackPlaylist(name)
				return finish(buildTSVariant(p, s, track))
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
				return finish(buildTrackPlaylist(p, s, kind, track))
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

// ended reports whether a media playlist gets EXT-X-ENDLIST: the stream
// ended and every segment is listed, so nothing can follow.
func ended(s Snapshot) bool {
	return s.Ended && s.Listed == len(s.Segments)
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
	pl.Ended = ended(s)
	return pl.Encode()
}

// trackEntry is one listed part of a track with its segment and epoch track.
type trackEntry struct {
	seg  SegmentInfo
	part CMAFPart
	et   EpochTrack
}

// trackPart returns a track's part in a segment and the track's epoch
// track; ok is false when the segment has no part of the track or its epoch
// track has no config.
func trackPart(s Snapshot, seg SegmentInfo, track int) (trackEntry, bool) {
	for _, part := range seg.Parts {
		if part.Track != track {
			continue
		}
		e, ok := s.Epoch(seg.Epoch)
		if !ok {
			return trackEntry{}, false
		}
		et, ok := e.Track(track)
		if !ok || et.Config == nil {
			return trackEntry{}, false
		}
		return trackEntry{seg, part, et}, true
	}
	return trackEntry{}, false
}

// trackEntries returns the listed parts of a track of the given kind,
// oldest first.
func trackEntries(s Snapshot, track int, kind media.Kind) []trackEntry {
	var out []trackEntry
	for _, seg := range s.Segments[:s.Listed] {
		if e, ok := trackPart(s, seg, track); ok && e.et.Config.Codec.Kind() == kind {
			out = append(out, e)
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

// initName returns the URI of a track's init segment in an epoch.
func initName(track int, epoch uint32) string {
	return "init-" + strconv.Itoa(track) + "-" + strconv.FormatUint(uint64(epoch), 10) + ".mp4"
}

// servesTrack reports whether cmaf/{v|a}{track}.m3u8 exists for a track of
// the given kind: the track is known with that kind, or has a listed part of
// that kind (a track whose kind changed keeps both playlists while the
// window shows the old one).
func servesTrack(s Snapshot, kind media.Kind, track int) bool {
	if kt, ok := s.Track(track); ok && kt.Config != nil && kt.Config.Codec.Kind() == kind {
		return true
	}
	return len(trackEntries(s, track, kind)) > 0
}

// cmafTargetDuration returns the EXT-X-TARGETDURATION shared by every CMAF
// track playlist, computed like the HLS-TS playlist's (see targetDuration)
// over every listed segment's duration, which is each gap entry's EXTINF,
// and every listed part's, which is each real entry's. One value for all
// renditions satisfies RFC 8216 section 6.2.4, and it bounds every EXTINF
// of every one of them.
func (p *Packager) cmafTargetDuration(s Snapshot) int {
	var extinfs []time.Duration
	for _, seg := range s.Segments[:s.Listed] {
		extinfs = append(extinfs, media.ToDuration(seg.Duration))
		for _, part := range seg.Parts {
			if e, ok := trackPart(s, seg, part.Track); ok {
				extinfs = append(extinfs, partDuration(e))
			}
		}
	}
	return p.targetDuration("cmaf", s, extinfs)
}

// buildTrackPlaylist renders cmaf/v{track}.m3u8 or cmaf/a{track}.m3u8 for a
// track of the given kind. It returns nil when servesTrack is false.
//
// The playlist lists one entry per listed segment, so every rendition has
// the same media sequence numbers (MEDIA-SEQUENCE is the first segment's
// Seq) and the same discontinuity sequence numbers (each entry carries its
// segment's Discontinuity flag; DISCONTINUITY-SEQUENCE is the shared
// EvictedDiscontinuities), as RFC 8216 section 6.2.4 requires. A segment
// with the track's part lists it as {track}-{N}.m4s. A segment without one
// (the track absent from the epoch, lost or failed to mux) lists a gap
// entry (EXT-X-GAP, RFC 8216bis section 4.4.4.7): EXTINF is the segment's
// duration and the URI gap-{track}-{Seq}.m4s, which is never served.
//
// A gap entry's EXT-X-MAP comes from the stream's history, not from the
// window: it is the init of the epoch of the track's latest part before the
// segment (SegmentInfo.GapMap), fixed when the segment was packaged, so a
// listed entry's map never changes between refreshes (RFC 8216 section
// 6.2.1). The packager keeps that init servable while the gap is listed,
// even after its epoch left the window. Gaps before the track's first part
// ever carry no map. Gap entries are never loaded, so their map is never
// fetched for them; it only keeps the map in effect valid.
// EXT-X-PROGRAM-DATE-TIME is on the first entry and on every discontinuity:
// a real entry's from its part's earliest presentation time, a gap's from
// its segment's start.
func buildTrackPlaylist(p *Packager, s Snapshot, kind media.Kind, track int) []byte {
	if !servesTrack(s, kind, track) {
		return nil
	}
	listed := s.Segments[:s.Listed]
	type slot struct {
		seg  SegmentInfo
		e    trackEntry
		real bool
	}
	slots := make([]slot, len(listed))
	for i, seg := range listed {
		slots[i].seg = seg
		if e, ok := trackPart(s, seg, track); ok && e.et.Config.Codec.Kind() == kind {
			slots[i].e, slots[i].real = e, true
		}
	}
	pl := hls.MediaPlaylist{
		Version:               hlsVersionFMP4,
		MediaSequence:         listed[0].Seq,
		DiscontinuitySequence: s.EvictedDiscontinuities,
	}
	epochs := map[uint32]bool{}
	for i, sl := range slots {
		seg := hls.Segment{Discontinuity: sl.seg.Discontinuity}
		start := sl.seg.Start
		if sl.real {
			seg.Map = initName(track, sl.seg.Epoch)
			seg.URI = strconv.Itoa(track) + "-" + strconv.FormatUint(sl.e.part.N, 10) + ".m4s"
			seg.Duration = partDuration(sl.e)
			start = 0
			if ts := int64(fmp4.Timescale(sl.e.et.Config)); ts > 0 {
				start = int64(sl.e.part.Timing.Earliest) * media.ClockRate / ts
			}
		} else {
			seg.URI = "gap-" + strconv.Itoa(track) + "-" + strconv.FormatUint(sl.seg.Seq, 10) + ".m4s"
			seg.Duration = media.ToDuration(sl.seg.Duration)
			seg.Gap = true
			if epoch, ok := sl.seg.GapMap[track]; ok {
				seg.Map = initName(track, epoch)
			}
		}
		if i == 0 || seg.Discontinuity {
			seg.ProgramDateTime = pdt(s, sl.seg.Epoch, start)
		}
		pl.Segments = append(pl.Segments, seg)
		epochs[sl.seg.Epoch] = true
	}
	pl.TargetDuration = p.cmafTargetDuration(s)
	pl.Independent = independent(s, epochs)
	pl.Ended = ended(s)
	return pl.Encode()
}

// lowestTracks returns the lowest known track of each kind (by its latest
// epoch track), so a rendition whose window holds only gaps stays listed.
func lowestTracks(s Snapshot) (video, audio int, haveV, haveA bool) {
	for _, kt := range s.Tracks { // ascending Track
		if kt.Config == nil {
			continue
		}
		switch kt.Config.Codec.Kind() {
		case media.KindVideo:
			if !haveV {
				video, haveV = kt.Track, true
			}
		case media.KindAudio:
			if !haveA {
				audio, haveA = kt.Track, true
			}
		}
	}
	return
}

// trackSummary is what the master playlist takes from one track's listed
// parts, or from its known epoch track when the window holds only gaps.
type trackSummary struct {
	codecs        []string           // distinct codec strings, in window order
	width, height int                // video: the largest resolution
	bandwidth     map[uint64]float64 // segment Seq -> the part's bits per second
}

// summarize returns a track's trackSummary and adds the epochs of its
// listed parts to epochs.
func summarize(s Snapshot, track int, kind media.Kind, epochs map[uint32]bool) trackSummary {
	ts := trackSummary{bandwidth: map[uint64]float64{}}
	codec := func(et EpochTrack) {
		if c := et.Codec; c != "" && !slices.Contains(ts.codecs, c) {
			ts.codecs = append(ts.codecs, c)
		}
		if kind == media.KindVideo && et.Config.Width*et.Config.Height > ts.width*ts.height {
			ts.width, ts.height = et.Config.Width, et.Config.Height
		}
	}
	entries := trackEntries(s, track, kind)
	if len(entries) == 0 {
		// Only gaps in the window: the retained codec and resolution.
		if kt, ok := s.Track(track); ok {
			codec(kt)
		}
		return ts
	}
	for _, e := range entries {
		epochs[e.seg.Epoch] = true
		codec(e.et)
		if d := partDuration(e).Seconds(); d > 0 {
			ts.bandwidth[e.seg.Seq] += float64(len(e.part.Data)) * 8 / d
		}
	}
	return ts
}

// variant combines a video summary with the audio summary: CODECS lists
// the video codecs, then the audio ones; BANDWIDTH is the peak over
// segments of the summed part rates; RESOLUTION is the video's.
func variant(uri string, video, audio trackSummary) hls.Variant {
	v := hls.Variant{URI: uri, Width: video.width, Height: video.height}
	codecs := slices.Clone(video.codecs)
	for _, c := range audio.codecs {
		if !slices.Contains(codecs, c) {
			codecs = append(codecs, c)
		}
	}
	bandwidth := maps.Clone(video.bandwidth)
	if bandwidth == nil {
		bandwidth = map[uint64]float64{}
	}
	for seq, b := range audio.bandwidth {
		bandwidth[seq] += b
	}
	peak := 1.0
	for _, b := range bandwidth {
		peak = max(peak, b)
	}
	v.Bandwidth = int(math.Ceil(peak))
	v.Codecs = strings.Join(codecs, ",")
	return v
}

// inVariantAudio returns the audio entry of a master whose variants carry
// the audio themselves (R-L4): no URI, so the rendition's media is in the
// variant (RFC 8216 section 4.3.4.2.1), and every variant names its group.
func inVariantAudio(lang string) hls.Rendition {
	return hls.Rendition{Type: "AUDIO", GroupID: audioGroup, Name: "audio", Language: lang, Default: true}
}

// buildMaster renders cmaf/master.m3u8 from the lowest known video and audio
// tracks. It returns nil when no track is known.
//
// With Snapshot.ABR every known video track is a variant, ascending track
// (rendition order), each with the lowest audio track (ruling R-B5).
//
// With a declared audio language (R-L4) and video, the audio rendition gains
// LANGUAGE. Audio only, the variant is the audio track's playlist, so the
// master gains one URI-less audio entry carrying LANGUAGE and the variant
// AUDIO="aud". Without a language the master is as before.
func buildMaster(s Snapshot) []byte {
	vTrack, aTrack, haveV, haveA := lowestTracks(s)
	if !haveV && !haveA {
		return nil
	}
	lang := s.audioLanguage()
	pl := hls.MultivariantPlaylist{Version: hlsVersionFMP4}
	epochs := map[uint32]bool{}
	var audio trackSummary
	if haveA {
		audio = summarize(s, aTrack, media.KindAudio, epochs)
		uri := "a" + strconv.Itoa(aTrack) + ".m3u8"
		switch {
		case haveV:
			pl.Renditions = []hls.Rendition{{Type: "AUDIO", GroupID: audioGroup, Name: "audio", Language: lang, URI: uri, Default: true}}
		case lang != "":
			v := variant(uri, trackSummary{}, audio)
			v.Audio = audioGroup
			pl.Renditions = []hls.Rendition{inVariantAudio(lang)}
			pl.Variants = []hls.Variant{v}
		default:
			pl.Variants = []hls.Variant{variant(uri, trackSummary{}, audio)}
		}
	}
	if haveV {
		videos := []int{vTrack}
		if s.ABR {
			videos = videoTracks(s)
		}
		for _, track := range videos {
			v := variant("v"+strconv.Itoa(track)+".m3u8", summarize(s, track, media.KindVideo, epochs), audio)
			if haveA {
				v.Audio = audioGroup
			}
			pl.Variants = append(pl.Variants, v)
		}
	}
	pl.Independent = independent(s, epochs)
	return pl.Encode()
}

// videoTracks returns the known video tracks, ascending.
func videoTracks(s Snapshot) []int {
	var out []int
	for _, kt := range s.Tracks { // ascending Track
		if kt.Config != nil && kt.Config.Codec.Kind() == media.KindVideo {
			out = append(out, kt.Track)
		}
	}
	return out
}

// tsVariants returns the video tracks hls/index.m3u8 lists in ABR mode,
// ascending: every known video track and every track with a variant TS in a
// listed segment.
func tsVariants(s Snapshot) []int {
	out := videoTracks(s)
	for _, seg := range s.Segments[:s.Listed] {
		for _, v := range seg.VariantTS {
			if !slices.Contains(out, v.Track) {
				out = append(out, v.Track)
			}
		}
	}
	slices.Sort(out)
	return out
}

// findVariantTS returns a segment's TS of the variant of track, if it has one.
func findVariantTS(seg SegmentInfo, track int) (VariantTS, bool) {
	i := slices.IndexFunc(seg.VariantTS, func(v VariantTS) bool { return v.Track == track })
	if i < 0 {
		return VariantTS{}, false
	}
	return seg.VariantTS[i], true
}

// listedEpochs returns the epochs of the listed segments.
func listedEpochs(s Snapshot) map[uint32]bool {
	epochs := map[uint32]bool{}
	for _, seg := range s.Segments[:s.Listed] {
		epochs[seg.Epoch] = true
	}
	return epochs
}

// buildTSMaster renders hls/index.m3u8 in ABR mode (rulings R-B4, R-B5): a
// multivariant playlist with one variant per tsVariants track, in track
// order, URI v{track}.m3u8. Audio is muxed into every variant: CODECS lists
// the variant's video codecs, then every known audio track's. RESOLUTION is
// the largest over the window, as in cmaf/master.m3u8, and BANDWIDTH the
// peak over listed segments of the variant TS's bytes × 8 / its Duration.
// It returns nil when there is no variant.
//
// Without a declared audio language there is no audio rendition. With one
// and an audio track known (R-L4), the master gains one URI-less audio
// entry carrying LANGUAGE, which describes the audio muxed into the
// variants, and every variant gains AUDIO="aud"; the version stays 3.
func buildTSMaster(s Snapshot) []byte {
	tracks := tsVariants(s)
	if len(tracks) == 0 {
		return nil
	}
	_, _, _, haveA := lowestTracks(s)
	lang := s.audioLanguage()
	inVariant := haveA && lang != ""
	discard := map[uint32]bool{}
	var audio trackSummary // codecs only: the audio bytes are in the variant TS
	for _, kt := range s.Tracks {
		if kt.Config == nil || kt.Config.Codec.Kind() != media.KindAudio {
			continue
		}
		for _, c := range summarize(s, kt.Track, media.KindAudio, discard).codecs {
			if !slices.Contains(audio.codecs, c) {
				audio.codecs = append(audio.codecs, c)
			}
		}
	}
	pl := hls.MultivariantPlaylist{Version: hlsVersionTS}
	if inVariant {
		pl.Renditions = []hls.Rendition{inVariantAudio(lang)}
	}
	for _, track := range tracks {
		video := summarize(s, track, media.KindVideo, discard)
		video.bandwidth = map[uint64]float64{}
		for _, seg := range s.Segments[:s.Listed] {
			for _, v := range seg.VariantTS {
				if d := media.ToDuration(v.Duration).Seconds(); v.Track == track && d > 0 {
					video.bandwidth[seg.Seq] = float64(len(v.Data)) * 8 / d
				}
			}
		}
		v := variant("v"+strconv.Itoa(track)+".m3u8", video, audio)
		if inVariant {
			v.Audio = audioGroup
		}
		pl.Variants = append(pl.Variants, v)
	}
	pl.Independent = independent(s, listedEpochs(s))
	return pl.Encode()
}

// tsVariantTargetDuration returns the EXT-X-TARGETDURATION shared by every
// hls/v{track}.m3u8, computed like targetDuration over every listed
// segment's duration, which is each gap entry's EXTINF, and every listed
// variant TS's Duration, which is each real entry's. Whichever variant is
// rendered first, every variant gets one value that bounds every EXTINF of
// every one of them (RFC 8216 section 6.2.4), as cmafTargetDuration does
// for the CMAF track playlists.
func (p *Packager) tsVariantTargetDuration(s Snapshot) int {
	var extinfs []time.Duration
	for _, seg := range s.Segments[:s.Listed] {
		extinfs = append(extinfs, media.ToDuration(seg.Duration))
		for _, v := range seg.VariantTS {
			extinfs = append(extinfs, media.ToDuration(v.Duration))
		}
	}
	return p.targetDuration("hls-abr", s, extinfs)
}

// buildTSVariant renders hls/v{track}.m3u8 in ABR mode (ruling R-B4). It
// returns nil when the snapshot is not ABR or track is not a tsVariants
// track.
//
// Like the CMAF track playlists, it lists one entry per listed segment, so
// every variant has the same media sequence numbers (MEDIA-SEQUENCE is the
// first segment's Seq) and discontinuity sequence numbers (each entry
// carries its segment's Discontinuity; DISCONTINUITY-SEQUENCE is
// EvictedDiscontinuities), as RFC 8216 section 6.2.4 requires. A segment
// with the variant's TS lists {track}-{Seq}.ts; one without (the track had
// no frames in it, or its mux failed) lists a gap entry,
// gap-{track}-{Seq}.ts, which is never served. A real entry's EXTINF is its
// variant's Duration, which is the segment's duration unless the segment
// closed without a cut and the variant's video ended elsewhere; a gap's is
// the segment's duration. EXT-X-PROGRAM-DATE-TIME is on the first entry and every
// discontinuity, and every variant shares one TARGETDURATION.
func buildTSVariant(p *Packager, s Snapshot, track int) []byte {
	if !s.ABR || !slices.Contains(tsVariants(s), track) {
		return nil
	}
	listed := s.Segments[:s.Listed]
	pl := hls.MediaPlaylist{
		Version:               hlsVersionTS,
		MediaSequence:         listed[0].Seq,
		DiscontinuitySequence: s.EvictedDiscontinuities,
	}
	name := strconv.Itoa(track) + "-" // Seq follows
	for i, seg := range listed {
		e := hls.Segment{
			URI:           name + strconv.FormatUint(seg.Seq, 10) + ".ts",
			Duration:      media.ToDuration(seg.Duration),
			Discontinuity: seg.Discontinuity,
		}
		if v, ok := findVariantTS(seg, track); ok {
			e.Duration = media.ToDuration(v.Duration)
		} else {
			e.URI, e.Gap = "gap-"+e.URI, true
		}
		if i == 0 || seg.Discontinuity {
			e.ProgramDateTime = pdt(s, seg.Epoch, seg.Start)
		}
		pl.Segments = append(pl.Segments, e)
	}
	pl.TargetDuration = p.tsVariantTargetDuration(s)
	pl.Independent = independent(s, listedEpochs(s))
	pl.Ended = ended(s)
	return pl.Encode()
}
