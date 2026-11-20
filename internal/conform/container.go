package conform

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// Container rule names, checked by the readers (ReadHLS, ReadDASH, ReadTS)
// on top of the frame rules.
const (
	// RuleSegmentKey: the first video frame of every segment is a keyframe;
	// a TS segment whose demuxer dropped frames for lack of a config also
	// breaks it.
	RuleSegmentKey = "segment-starts-on-key"
	// RuleExtinf: |EXTINF − media duration| is at most one frame of the
	// playlist's track, in integer ticks.
	RuleExtinf = "extinf-matches"
	// RuleCodecString: DASH @codecs equals the codec string of the period's
	// content, and the HLS master's CODECS holds the codec string of every
	// epoch's content in the capture.
	RuleCodecString = "codec-string"
	// RuleConfigSignalled: every config change falls on a discontinuity or
	// a new period.
	RuleConfigSignalled = "config-change-signalled"
	// RuleASTStable: the MPD's availabilityStartTime is the same on every
	// refresh (polling mode).
	RuleASTStable = "mpd-ast-stable"
	// RulePeriodOrder: each period starts at or after the previous period's
	// start plus its media, with 1 ms tolerance.
	RulePeriodOrder = "period-order"
	// RuleTimeline: each S@t is the segment's earliest presentation time and
	// S@d its summed sample durations.
	RuleTimeline = "timeline-matches-media"
	// RuleSegmentAvailable: every listed segment (and the init it needs)
	// answers 200.
	RuleSegmentAvailable = "segment-available"
)

// frameRules are the rules CheckFrames knows.
var frameRules = []string{
	RuleConfig, RuleDTS, RulePTS, RuleEpochOrder, RuleEpochKey, RuleRASL,
	RuleStartOffset, RuleOverlap, RuleAudioKey, RuleParamSets, RuleConfigChange,
}

// startRules are the frame rules that need the start of an epoch.
var startRules = []string{RuleStartOffset, RuleRASL}

// trackKey identifies a source track: a playlist or representation and a
// track inside it.
type trackKey struct {
	src   string
	local int
}

// cfgState is a track's last config and the epoch it was seen in.
type cfgState struct {
	cfg   *media.TrackConfig
	epoch uint32
}

// builder collects a capture: frames from every segment, container
// violations, and what the frame rules need to know about the start.
type builder struct {
	frames  []media.Frame
	viol    []Violation
	tracks  map[trackKey]int
	last    map[int]cfgState
	seen    map[string]bool // reported violation keys, to report each once
	httpTS  bool
	started bool // the first segment has been seen
	// fromStart: the capture's first segment is its epoch's first, so the
	// start rules apply to the first epoch.
	fromStart  bool
	firstEpoch uint32
}

func newBuilder() *builder {
	return &builder{tracks: map[trackKey]int{}, last: map[int]cfgState{}, seen: map[string]bool{}}
}

// track maps a source track to the capture's track number, in order of
// first appearance.
func (b *builder) track(src string, local int) int {
	k := trackKey{src, local}
	n, ok := b.tracks[k]
	if !ok {
		n = len(b.tracks)
		b.tracks[k] = n
	}
	return n
}

// start records the capture's first segment: its epoch and whether it is
// that epoch's first segment.
func (b *builder) start(epoch uint32, first bool) {
	if b.started {
		return
	}
	b.started, b.firstEpoch, b.fromStart = true, epoch, first
}

// intern returns the config to put on a frame: within an epoch, a config
// Equal to the track's previous one reuses its pointer.
func (b *builder) intern(track int, epoch uint32, cfg *media.TrackConfig) *media.TrackConfig {
	if st, ok := b.last[track]; ok && st.epoch == epoch && st.cfg.Equal(cfg) {
		return st.cfg
	}
	b.last[track] = cfgState{cfg, epoch}
	return cfg
}

// add appends a frame on the capture's track, interning its config.
func (b *builder) add(f media.Frame) {
	f.Config = b.intern(f.Track, f.Epoch, f.Config)
	b.frames = append(b.frames, f)
}

// violate records a container violation. Index is -1: it is not tied to a
// frame of the merged capture.
func (b *builder) violate(rule string, track int, format string, args ...any) {
	b.viol = append(b.viol, Violation{Rule: rule, Track: track, Index: -1, Detail: fmt.Sprintf(format, args...)})
}

// once records a violation unless one with the same key was recorded.
func (b *builder) once(key, rule string, track int, format string, args ...any) {
	if b.seen[key] {
		return
	}
	b.seen[key] = true
	b.violate(rule, track, format, args...)
}

// checkSegmentKey applies RuleSegmentKey to one segment's frames.
func (b *builder) checkSegmentKey(url string, frames []media.Frame, droppedNoConfig uint64) {
	if droppedNoConfig > 0 {
		b.violate(RuleSegmentKey, -1, "%s: %d video frames dropped before a config", url, droppedNoConfig)
	}
	done := map[int]bool{}
	for _, f := range frames {
		if f.Kind() != media.KindVideo || done[f.Track] {
			continue
		}
		done[f.Track] = true
		if !f.Key {
			b.violate(RuleSegmentKey, f.Track, "%s: first video frame (PTS %d) is not a keyframe", url, f.PTS)
		}
	}
}

// checkExtinf applies RuleExtinf: extinf and media in ticks, frame the
// tolerance in ticks.
func (b *builder) checkExtinf(url string, track int, extinf, mediaDur, frame int64) {
	if d := extinf - mediaDur; d > frame || -d > frame {
		b.violate(RuleExtinf, track, "%s: EXTINF %d ticks, media %d ticks (tolerance %d)", url, extinf, mediaDur, frame)
	}
}

// checkMasterCodecs applies RuleCodecString to an HLS master: every config
// in the capture has its codec string in codecs.
func (b *builder) checkMasterCodecs(codecs []string) {
	done := map[*media.TrackConfig]bool{}
	for _, f := range b.frames {
		if f.Config == nil || done[f.Config] {
			continue
		}
		done[f.Config] = true
		c, err := media.CodecString(f.Config)
		switch {
		case err != nil:
			b.once("codecs-err:"+err.Error(), RuleCodecString, f.Track, "epoch %d: %v", f.Epoch, err)
		case !slices.Contains(codecs, c):
			b.once("codecs:"+c, RuleCodecString, f.Track, "epoch %d: %s is not in master CODECS %q",
				f.Epoch, c, strings.Join(codecs, ","))
		}
	}
}

// finish merges the frames by DTS, checks config signalling and runs the
// frame rules.
func (b *builder) finish() Capture {
	frames := b.frames
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].DTS < frames[j].DTS })
	out := slices.Clone(b.viol)

	if !b.httpTS {
		prev := map[int]media.Frame{}
		for i, f := range frames {
			if p, ok := prev[f.Track]; ok && p.Epoch == f.Epoch && !p.Config.Equal(f.Config) {
				out = append(out, Violation{
					Rule: RuleConfigSignalled, Track: f.Track, Index: i,
					Detail: fmt.Sprintf("config changed within epoch %d without a discontinuity or period", f.Epoch),
				})
			}
			prev[f.Track] = f
		}
	}

	var skip []string
	if b.httpTS {
		// One epoch with no signalled start: neither config rule applies
		// and the stream is joined mid-epoch.
		skip = append(skip, RuleConfigChange)
		skip = append(skip, startRules...)
	}
	switch {
	case b.httpTS || b.fromStart:
		out = append(out, CheckFrames(frames, FrameOptions{Skip: skip})...)
	default:
		// The first epoch was joined mid-way: the start rules apply only to
		// the later epochs.
		out = append(out, CheckFrames(frames, FrameOptions{Skip: startRules})...)
		var later []media.Frame
		var index []int
		for i, f := range frames {
			if f.Epoch != b.firstEpoch {
				later = append(later, f)
				index = append(index, i)
			}
		}
		only := slices.DeleteFunc(slices.Clone(frameRules), func(r string) bool { return slices.Contains(startRules, r) })
		for _, v := range CheckFrames(later, FrameOptions{Skip: only}) {
			v.Index = index[v.Index]
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return Capture{Frames: frames, Violations: out}
}
