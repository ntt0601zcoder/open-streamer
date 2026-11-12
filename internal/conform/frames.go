// Package conform checks property rules over media frames and packets.
package conform

import (
	"fmt"
	"slices"
	"sort"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// Frame rule names.
const (
	RuleConfig      = "config-present"
	RuleDTS         = "dts-monotonic"       // Strictly increasing per track, across epochs too.
	RulePTS         = "pts-ge-dts"          // PTS never below DTS.
	RuleEpochOrder  = "epoch-monotonic"     // Per track, the epoch never decreases.
	RuleEpochKey    = "epoch-starts-on-key" // First video frame of each epoch per track.
	RuleRASL        = "no-rasl-after-start" // No RASL frame right after an epoch's first keyframe.
	RuleStartOffset = "track-start-offset"  // Per epoch: each track's first PTS vs the earliest first PTS.
	RuleOverlap     = "epoch-no-overlap"    // An epoch's smallest PTS is above the previous epoch's largest.

	// RuleAudioKey: every audio frame has Key true and RASL false.
	RuleAudioKey = "audio-key-not-rasl"
	// RuleParamSets: video Data holds no VPS, SPS, PPS or AUD NAL unit (by
	// the Config codec); parameter sets live only in the Config.
	RuleParamSets = "no-param-sets-in-data"
	// RuleConfigChange: a track's Config pointer changes only at that
	// track's first frame of a new epoch. Raw demuxer output has no epochs,
	// so checks on it skip this rule.
	RuleConfigChange = "config-change-at-epoch"
)

// defaultMaxStartOffset is 40 ms in 90 kHz ticks.
const defaultMaxStartOffset = 3600

// Violation is one broken rule at one frame.
type Violation struct {
	Rule   string
	Track  int
	Index  int
	Detail string
}

// String formats the violation for test output.
func (v Violation) String() string {
	return fmt.Sprintf("%s: track %d frame %d: %s", v.Rule, v.Track, v.Index, v.Detail)
}

// FrameOptions tunes CheckFrames.
type FrameOptions struct {
	MaxStartOffset int64    // 0 means 3600 (40 ms).
	Skip           []string // Rule names not to check.
}

type trackState struct {
	seen       bool
	lastDTS    int64
	lastEpoch  uint32
	lastConfig *media.TrackConfig
}

type trackEpoch struct {
	track int
	epoch uint32
}

type trackEpochState struct {
	inStart  bool // Between the epoch's first video keyframe and the next keyframe.
	firstPTS int64
	index    int
}

type epochRange struct {
	minPTS, maxPTS int64
	firstIndex     int
	firstTrack     int
}

// paramSetIn returns the type of the first VPS, SPS, PPS or AUD NAL unit in
// the data of f, a video frame.
func paramSetIn(f media.Frame) (uint8, bool) {
	for _, n := range f.Data {
		if len(n) == 0 {
			continue
		}
		switch f.Config.Codec {
		case media.CodecH264:
			if typ := n[0] & 0x1f; typ >= 7 && typ <= 9 { // SPS, PPS, AUD
				return typ, true
			}
		case media.CodecH265:
			if typ := (n[0] >> 1) & 0x3f; typ >= 32 && typ <= 35 { // VPS, SPS, PPS, AUD
				return typ, true
			}
		case media.CodecAAC:
		}
	}
	return 0, false
}

// CheckFrames returns every rule violation found in frames, in input order.
// It returns nil when the sequence conforms.
func CheckFrames(frames []media.Frame, opt FrameOptions) []Violation {
	maxOffset := opt.MaxStartOffset
	if maxOffset == 0 {
		maxOffset = defaultMaxStartOffset
	}
	on := func(rule string) bool { return !slices.Contains(opt.Skip, rule) }

	var out []Violation
	add := func(rule string, f media.Frame, i int, format string, args ...any) {
		out = append(out, Violation{Rule: rule, Track: f.Track, Index: i, Detail: fmt.Sprintf(format, args...)})
	}

	tracks := map[int]*trackState{}
	te := map[trackEpoch]*trackEpochState{}
	var teOrder []trackEpoch
	ranges := map[uint32]*epochRange{}
	var epochOrder []uint32

	for i, f := range frames {
		kind := f.Kind()
		if f.Config == nil && on(RuleConfig) {
			add(RuleConfig, f, i, "frame has no track config")
		}

		ts := tracks[f.Track]
		if ts == nil {
			ts = &trackState{}
			tracks[f.Track] = ts
		}
		if ts.seen {
			if f.DTS <= ts.lastDTS && on(RuleDTS) {
				add(RuleDTS, f, i, "DTS %d is not above previous DTS %d", f.DTS, ts.lastDTS)
			}
			if f.Epoch < ts.lastEpoch && on(RuleEpochOrder) {
				add(RuleEpochOrder, f, i, "epoch %d is below previous epoch %d", f.Epoch, ts.lastEpoch)
			}
			if f.Config != ts.lastConfig && f.Epoch == ts.lastEpoch && on(RuleConfigChange) {
				add(RuleConfigChange, f, i, "config pointer changed within epoch %d", f.Epoch)
			}
		}
		ts.seen, ts.lastDTS, ts.lastEpoch, ts.lastConfig = true, f.DTS, f.Epoch, f.Config

		switch kind {
		case media.KindAudio:
			if (!f.Key || f.RASL) && on(RuleAudioKey) {
				add(RuleAudioKey, f, i, "audio frame has Key %v and RASL %v", f.Key, f.RASL)
			}
		case media.KindVideo:
			if typ, ok := paramSetIn(f); ok && on(RuleParamSets) {
				add(RuleParamSets, f, i, "%s data holds NAL unit type %d", f.Config.Codec, typ)
			}
		}

		if f.PTS < f.DTS && on(RulePTS) {
			add(RulePTS, f, i, "PTS %d is below DTS %d", f.PTS, f.DTS)
		}

		key := trackEpoch{f.Track, f.Epoch}
		st := te[key]
		if st == nil {
			st = &trackEpochState{inStart: kind == media.KindVideo && f.Key, firstPTS: f.PTS, index: i}
			te[key] = st
			teOrder = append(teOrder, key)
			if kind == media.KindVideo && !f.Key && on(RuleEpochKey) {
				add(RuleEpochKey, f, i, "first video frame of epoch %d is not a keyframe", f.Epoch)
			}
		} else if kind == media.KindVideo {
			switch {
			case f.Key:
				st.inStart = false
			case f.RASL && st.inStart && on(RuleRASL):
				add(RuleRASL, f, i, "RASL frame after epoch %d start keyframe", f.Epoch)
			}
		}

		r := ranges[f.Epoch]
		if r == nil {
			r = &epochRange{minPTS: f.PTS, maxPTS: f.PTS, firstIndex: i, firstTrack: f.Track}
			ranges[f.Epoch] = r
			epochOrder = append(epochOrder, f.Epoch)
		}
		r.minPTS = min(r.minPTS, f.PTS)
		r.maxPTS = max(r.maxPTS, f.PTS)
	}

	if on(RuleStartOffset) {
		earliest := map[uint32]int64{}
		for _, k := range teOrder {
			st := te[k]
			if e, ok := earliest[k.epoch]; !ok || st.firstPTS < e {
				earliest[k.epoch] = st.firstPTS
			}
		}
		for _, k := range teOrder {
			st := te[k]
			if d := st.firstPTS - earliest[k.epoch]; d > maxOffset {
				out = append(out, Violation{
					Rule: RuleStartOffset, Track: k.track, Index: st.index,
					Detail: fmt.Sprintf("first PTS %d is %d ticks after earliest %d in epoch %d (max %d)",
						st.firstPTS, d, earliest[k.epoch], k.epoch, maxOffset),
				})
			}
		}
	}

	if on(RuleOverlap) {
		for n := 1; n < len(epochOrder); n++ {
			prev, cur := ranges[epochOrder[n-1]], ranges[epochOrder[n]]
			if cur.minPTS <= prev.maxPTS {
				out = append(out, Violation{
					Rule: RuleOverlap, Track: cur.firstTrack, Index: cur.firstIndex,
					Detail: fmt.Sprintf("epoch %d smallest PTS %d is not above epoch %d largest PTS %d",
						epochOrder[n], cur.minPTS, epochOrder[n-1], prev.maxPTS),
				})
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}
