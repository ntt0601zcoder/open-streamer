package conform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

var (
	vcfg = &media.TrackConfig{Codec: media.CodecH264}
	acfg = &media.TrackConfig{Codec: media.CodecAAC}
)

func vf(epoch uint32, dts, pts int64, key bool) media.Frame {
	return media.Frame{Track: 0, Config: vcfg, DTS: dts, PTS: pts, Key: key, Epoch: epoch}
}

func af(epoch uint32, pts int64) media.Frame {
	return media.Frame{Track: 1, Config: acfg, DTS: pts, PTS: pts, Key: true, Epoch: epoch}
}

func requireOne(t *testing.T, vs []Violation, rule string, track, index int) {
	t.Helper()
	require.Len(t, vs, 1, "%v", vs)
	assert.Equal(t, rule, vs[0].Rule)
	assert.Equal(t, track, vs[0].Track)
	assert.Equal(t, index, vs[0].Index)
	assert.NotEmpty(t, vs[0].Detail)
	assert.Contains(t, vs[0].String(), rule)
}

func TestCheckFramesClean(t *testing.T) {
	frames := []media.Frame{
		vf(1, 0, 7200, true),
		af(1, 7200),
		vf(1, 3600, 14400, false),
		af(1, 8000),
		vf(2, 20000, 27200, true),
		af(2, 27200),
		vf(2, 23600, 34400, false),
	}
	assert.Nil(t, CheckFrames(frames, FrameOptions{}))
}

func TestCheckFramesMissingConfig(t *testing.T) {
	f := vf(1, 0, 0, true)
	f.Config = nil
	requireOne(t, CheckFrames([]media.Frame{f}, FrameOptions{}), RuleConfig, 0, 0)
}

func TestCheckFramesDTSRepeat(t *testing.T) {
	frames := []media.Frame{vf(1, 0, 0, true), vf(1, 0, 3600, false)}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RuleDTS, 0, 1)
}

func TestCheckFramesDTSBackwards(t *testing.T) {
	frames := []media.Frame{vf(1, 3600, 3600, true), vf(1, 0, 7200, false)}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RuleDTS, 0, 1)
}

func TestCheckFramesPTSBeforeDTS(t *testing.T) {
	frames := []media.Frame{vf(1, 3600, 0, true)}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RulePTS, 0, 0)
}

func TestCheckFramesEpochDecrease(t *testing.T) {
	frames := []media.Frame{vf(2, 0, 0, true), vf(1, 3600, 3600, true)}
	vs := CheckFrames(frames, FrameOptions{Skip: []string{RuleOverlap}})
	requireOne(t, vs, RuleEpochOrder, 0, 1)
}

func TestCheckFramesEpochStartsOnNonKey(t *testing.T) {
	frames := []media.Frame{vf(1, 0, 0, true), vf(2, 3600, 3600, false)}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RuleEpochKey, 0, 1)
}

func TestCheckFramesRASLAfterStart(t *testing.T) {
	r := vf(1, 3600, 3600, false)
	r.RASL = true
	frames := []media.Frame{vf(1, 0, 0, true), r}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RuleRASL, 0, 1)
}

func startOffsetFrames(audioPTS int64) []media.Frame {
	return []media.Frame{vf(1, 0, 7200, true), af(1, audioPTS)}
}

func TestCheckFramesStartOffset(t *testing.T) {
	requireOne(t, CheckFrames(startOffsetFrames(10801), FrameOptions{}), RuleStartOffset, 1, 1)
	assert.Nil(t, CheckFrames(startOffsetFrames(10800), FrameOptions{}))
}

func TestCheckFramesEpochOverlap(t *testing.T) {
	frames := []media.Frame{vf(1, 0, 0, true), vf(1, 3600, 3600, false), af(2, 3600)}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RuleOverlap, 1, 2)
}

func TestCheckFramesSkip(t *testing.T) {
	opt := FrameOptions{Skip: []string{RuleStartOffset}}
	assert.Nil(t, CheckFrames(startOffsetFrames(10801), opt))
}
