package clock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// Video alone for 3 s, then audio appears. Audio was not in epoch 1 when it
// was released, so it opens epoch 2 at the next video keyframe; video keeps
// emitting into epoch 1 until then, so no video frame is lost.
func TestLateJoinOpensEpoch(t *testing.T) {
	c := New(Options{Kinds: av})
	v := videoRun(0, 150)      // keyframes every 90000; last DTS 536400
	a := audioRun(272000, 160) // first audio after the keyframe at 270000
	frames := mux(0, v, a)
	out := push(c, frames...)
	out = append(out, c.Flush()...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	st := c.Stats()
	assert.Equal(t, uint64(1), st.LateJoins)
	assert.Zero(t, st.LateTracks)
	assert.Zero(t, st.DroppedBeforeKey)

	// Every video frame is delivered; those between the audio's arrival and
	// the next keyframe (360000) stay in epoch 1.
	assert.Len(t, onTrack(out, 0), len(v))
	e1 := inEpoch(out, 1)
	assert.Len(t, onTrack(e1, 0), 100)
	assert.Empty(t, onTrack(e1, 1))

	e2 := inEpoch(out, 2)
	v2, a2 := onTrack(e2, 0), onTrack(e2, 1)
	require.NotEmpty(t, v2)
	require.NotEmpty(t, a2)
	assert.True(t, v2[0].Key)
	d := a2[0].PTS - v2[0].PTS
	assert.GreaterOrEqual(t, d, int64(0))
	assert.LessOrEqual(t, d, int64(3600))
	requireConform(t, out)
}

// Audio arrives 1.5 s after the video it is presented with, for 60 s. The
// late join happens at most once, so the skew does not open epoch after epoch.
func TestLateJoinPersistentSkew(t *testing.T) {
	c := New(Options{Kinds: av})
	frames := mux(135000, videoRun(0, 1500), audioRun(0, 2813))
	out := push(c, frames...)
	out = append(out, c.Flush()...)

	assert.LessOrEqual(t, len(c.Epochs()), 2)
	assert.LessOrEqual(t, c.Stats().LateJoins, uint64(1))
	requireConform(t, out)
}

// staggered returns 6 s of muxed video and audio, audio arriving 12000 ticks
// late, with video switching to a new config at its keyframe at 180000. When
// audioCfg is set, audio switches to it at its frame presented at 182400,
// which arrives 4 video frames after that keyframe.
func staggered(videoCfg, audioCfg *media.TrackConfig) (v, a, frames []media.Frame) {
	v = videoRun(0, 150)
	a = audioRun(0, 282)
	for i := 50; i < len(v); i++ {
		v[i].Config = videoCfg
	}
	if audioCfg != nil {
		for i := 95; i < len(a); i++ { // a[94] (180480) is the last old-config frame
			a[i].Config = audioCfg
		}
	}
	return v, a, mux(12000, v, a)
}

// Video changes config at a keyframe and audio 4 frames later: one epoch
// change, and the audio change drops only audio's old-config frames from the
// pending epoch, not the video GOP held with them.
func TestStaggeredConfigChange(t *testing.T) {
	c := New(Options{Kinds: av})
	v2 := &media.TrackConfig{Codec: media.CodecH264}
	a2 := &media.TrackConfig{Codec: media.CodecAAC}
	v, _, frames := staggered(v2, a2)
	out := push(c, frames...)
	out = append(out, c.Flush()...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	assert.Zero(t, c.Stats().DroppedBeforeKey)
	assert.Len(t, onTrack(out, 0), len(v))
	e2 := inEpoch(out, 2)
	ev := onTrack(e2, 0)
	require.Len(t, ev, 100) // the whole GOP from the keyframe at 180000 on
	assert.True(t, ev[0].Key)
	for _, f := range ev {
		assert.Same(t, v2, f.Config)
	}
	ea := onTrack(e2, 1)
	require.NotEmpty(t, ea)
	for _, f := range ea {
		assert.Same(t, a2, f.Config)
	}
	assert.Equal(t, uint64(1), c.Stats().DroppedStale) // a[94], old config
	requireConform(t, out)
}

// Only video changes config. Audio keeps its pointer, and its frames
// presented at or after the new keyframe are kept in the new epoch.
func TestOldConfigAudioKept(t *testing.T) {
	c := New(Options{Kinds: av})
	v2 := &media.TrackConfig{Codec: media.CodecH264}
	_, a, frames := staggered(v2, nil)
	out := push(c, frames...)
	out = append(out, c.Flush()...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	var want int
	for _, f := range a {
		if f.PTS >= 180000 {
			want++
		}
	}
	ea := onTrack(inEpoch(out, 2), 1)
	assert.Len(t, ea, want)
	for _, f := range ea {
		assert.Same(t, aac, f.Config)
	}
	assert.Zero(t, c.Stats().DroppedStale)
	requireConform(t, out)
}

// A config-change epoch holds AlignWindow from its keyframe even when every
// track has a frame, so a change on another track inside the window still
// joins it.
func TestConfigEpochHoldsWindow(t *testing.T) {
	c := New(Options{Kinds: av})
	out := push(c, mux(0, videoRun(0, 10), audioRun(0, 19))...) // to 32400 / 34560
	changed := video(36000, true)
	changed.Config = &media.TrackConfig{Codec: media.CodecH264}
	out = append(out, push(c, changed, audio(36480))...)
	assert.Len(t, inEpoch(out, 2), 0) // every track held, still pending
	for i := int64(1); i < 25; i++ {  // to 122400: span 86400
		f := video(36000+i*vDur, false)
		f.Config = changed.Config
		out = append(out, c.Push(f)...)
	}
	assert.Len(t, inEpoch(out, 2), 0)
	last := video(36000+25*vDur, true) // span 90000 releases
	last.Config = changed.Config
	out = append(out, c.Push(last)...)
	assert.Len(t, inEpoch(out, 2), 27)
	requireConform(t, out)
}

// A track running in the released epoch keeps its frames presented before
// the late-join keyframe there, even those arriving after that keyframe.
func TestLateJoinKeepsRunningAudio(t *testing.T) {
	second := &media.TrackConfig{Codec: media.CodecAAC}
	c := New(Options{Kinds: []media.Kind{media.KindVideo, media.KindAudio, media.KindAudio}})
	v := videoRun(0, 150)
	a1 := audioRun(0, 282)
	a2 := audioRun(272000, 150)
	for i := range a2 {
		a2[i].Track, a2[i].Config = 2, second
	}
	frames := arrange(arrive(arrive(arrive(nil, 0, v), 12000, a1), 0, a2))
	out := push(c, frames...)
	out = append(out, c.Flush()...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	assert.Equal(t, uint64(1), c.Stats().LateJoins)
	assert.Len(t, onTrack(out, 0), len(v))
	assert.Len(t, onTrack(out, 1), len(a1))
	assert.Empty(t, onTrack(inEpoch(out, 1), 2))
	requireConform(t, out)
}

// Audio-only streams: a late track opens the next epoch at once.
func TestLateJoinAudioOnly(t *testing.T) {
	second := &media.TrackConfig{Codec: media.CodecAAC}
	c := New(Options{Kinds: []media.Kind{media.KindAudio, media.KindAudio}})
	a0 := audioRun(0, 300)
	for i := range a0 {
		a0[i].Track = 0
	}
	a1 := audioRun(272000, 150)
	for i := range a1 {
		a1[i].Config = second
	}
	frames := mux(0, a0, a1)
	out := push(c, frames...)
	out = append(out, c.Flush()...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	assert.Equal(t, uint64(1), c.Stats().LateJoins)
	assert.Len(t, onTrack(out, 0), len(a0))
	assert.Len(t, onTrack(out, 1), len(a1))
	requireConform(t, out)
}

// After a late join, the track's hold survives Break: an epoch opened by a
// loop waits for it. The late-join count survives Break(false) and is reset
// by Break(true). Arrival lags stay under MaxBackJump: a track arriving
// further behind the high-water mark is a jump, not a late join.
func TestLateJoinHold(t *testing.T) {
	const t1, t2 = 10_000_000, 20_000_000
	// pass1: video alone, then audio from 110000, after epoch 1's release at
	// 90000. Audio opens epoch 2, and its hold becomes 110000 + 18000.
	pass1 := func(t *testing.T, c *Clock) []media.Frame {
		t.Helper()
		out := push(c, mux(0, videoRun(0, 150), audioRun(110000, 230))...)
		require.Equal(t, []uint32{1, 2}, epochIDs(c))
		require.Equal(t, uint64(1), c.Stats().LateJoins)
		return out
	}
	// skewed is 10 s of video and audio, audio arriving lag ticks late.
	skewed := func(c *Clock, from, lag int64) []media.Frame {
		return push(c, mux(lag, videoRun(from, 250), audioRun(from, 469))...)
	}

	t.Run("loop waits for the late track", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := pass1(t, c)
		c.Break(false)
		out = append(out, skewed(c, t1, 117000)...) // 1.3 s late: within the hold
		out = append(out, c.Flush()...)
		require.Equal(t, []uint32{1, 2, 3}, epochIDs(c))
		assert.Equal(t, uint64(1), c.Stats().LateJoins)
		assert.Zero(t, c.Stats().LateTracks)
		assert.Len(t, onTrack(inEpoch(out, 3), 1), 469)
		requireConform(t, out)
	})

	t.Run("loop beyond the hold joins late", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := pass1(t, c)
		c.Break(false)
		out = append(out, skewed(c, t1, 162000)...) // 1.8 s late: beyond the hold
		out = append(out, c.Flush()...)
		require.Equal(t, []uint32{1, 2, 3}, epochIDs(c))
		assert.Equal(t, uint64(1), c.Stats().LateJoins)
		assert.Equal(t, uint64(1), c.Stats().LateTracks)
		requireConform(t, out)
	})

	t.Run("new input resets the count", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := pass1(t, c)
		c.Break(true)
		out = append(out, skewed(c, t2, 162000)...)
		out = append(out, c.Flush()...)
		require.Equal(t, []uint32{1, 2, 3, 4}, epochIDs(c))
		assert.Equal(t, uint64(2), c.Stats().LateJoins)
		requireConform(t, out)
	})
}
