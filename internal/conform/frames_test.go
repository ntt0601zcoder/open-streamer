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

func rasl(dts int64) media.Frame {
	f := vf(1, dts, dts, false)
	f.RASL = true
	return f
}

func TestCheckFramesRASLRun(t *testing.T) {
	vs := CheckFrames([]media.Frame{vf(1, 0, 0, true), rasl(1), rasl(2), rasl(3)}, FrameOptions{})
	require.Len(t, vs, 3)
	for n, v := range vs {
		assert.Equal(t, RuleRASL, v.Rule)
		assert.Equal(t, n+1, v.Index)
	}
}

func TestCheckFramesRASLAfterNonRASL(t *testing.T) {
	frames := []media.Frame{vf(1, 0, 0, true), vf(1, 1, 1, false), rasl(2)}
	requireOne(t, CheckFrames(frames, FrameOptions{}), RuleRASL, 0, 2)
}

func TestCheckFramesRASLAfterLaterKey(t *testing.T) {
	frames := []media.Frame{vf(1, 0, 0, true), vf(1, 1, 1, true), rasl(2)}
	assert.Nil(t, CheckFrames(frames, FrameOptions{}))
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

func TestCheckFramesAudioKey(t *testing.T) {
	notKey := af(1, 7200)
	notKey.Key = false
	requireOne(t, CheckFrames([]media.Frame{vf(1, 0, 7200, true), notKey}, FrameOptions{}), RuleAudioKey, 1, 1)

	raslAudio := af(1, 7200)
	raslAudio.RASL = true
	requireOne(t, CheckFrames([]media.Frame{vf(1, 0, 7200, true), raslAudio}, FrameOptions{}), RuleAudioKey, 1, 1)
}

func TestCheckFramesParamSetsInData(t *testing.T) {
	h265 := &media.TrackConfig{Codec: media.CodecH265}
	slice264 := []byte{0x65, 0x88}
	slice265 := []byte{0x26, 0x01, 0xaf} // IDR_W_RADL
	for _, tc := range []struct {
		name  string
		cfg   *media.TrackConfig
		slice []byte
		bad   []byte
	}{
		{"h264 sps", vcfg, slice264, []byte{0x67, 0x4d}},
		{"h264 pps", vcfg, slice264, []byte{0x68, 0xee}},
		{"h264 aud", vcfg, slice264, []byte{0x09, 0xf0}},
		{"h265 vps", h265, slice265, []byte{0x40, 0x01, 0x0c}},
		{"h265 sps", h265, slice265, []byte{0x42, 0x01, 0x01}},
		{"h265 pps", h265, slice265, []byte{0x44, 0x01, 0xc1}},
		{"h265 aud", h265, slice265, []byte{0x46, 0x01, 0x50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean := vf(1, 0, 0, true)
			clean.Config, clean.Data = tc.cfg, [][]byte{tc.slice}
			dirty := vf(1, 3600, 3600, false)
			dirty.Config, dirty.Data = tc.cfg, [][]byte{tc.bad, tc.slice, tc.bad}
			requireOne(t, CheckFrames([]media.Frame{clean, dirty}, FrameOptions{}), RuleParamSets, 0, 1)
		})
	}

	// Audio data is not parsed as NAL units.
	a := af(1, 0)
	a.Data = [][]byte{{0x67, 0x68, 0x09}}
	assert.Nil(t, CheckFrames([]media.Frame{a}, FrameOptions{}))
}

func TestCheckFramesConfigChange(t *testing.T) {
	other := &media.TrackConfig{Codec: media.CodecH264}
	changed := func(f media.Frame) media.Frame {
		f.Config = other
		return f
	}

	t.Run("mid-epoch", func(t *testing.T) {
		frames := []media.Frame{vf(1, 0, 0, true), changed(vf(1, 3600, 3600, true))}
		requireOne(t, CheckFrames(frames, FrameOptions{}), RuleConfigChange, 0, 1)
	})

	t.Run("at an epoch start", func(t *testing.T) {
		frames := []media.Frame{vf(1, 0, 0, true), changed(vf(2, 3600, 3600, true)), changed(vf(2, 7200, 7200, false))}
		assert.Nil(t, CheckFrames(frames, FrameOptions{}))
	})

	t.Run("skipped", func(t *testing.T) {
		frames := []media.Frame{vf(0, 0, 0, true), changed(vf(0, 3600, 3600, true))}
		assert.Nil(t, CheckFrames(frames, FrameOptions{Skip: []string{RuleConfigChange}}))
	})
}
