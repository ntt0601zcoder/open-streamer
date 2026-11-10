package clock

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// One pointer per track: a different pointer is a config change.
var (
	h264 = &media.TrackConfig{Codec: media.CodecH264}
	aac  = &media.TrackConfig{Codec: media.CodecAAC}
)

const (
	vDur = 3600 // 25 fps video
	aDur = 1920 // AAC at 48 kHz
)

var av = []media.Kind{media.KindVideo, media.KindAudio}

func video(dts int64, key bool) media.Frame {
	return media.Frame{Track: 0, Config: h264, PTS: dts, DTS: dts, Key: key}
}

func audio(dts int64) media.Frame {
	return media.Frame{Track: 1, Config: aac, PTS: dts, DTS: dts, Key: true}
}

// videoRun returns n video frames from dts, with a keyframe every gop frames
// starting at the first.
func videoRun(from int64, n, gop int) []media.Frame {
	out := make([]media.Frame, n)
	for i := range out {
		out[i] = video(from+int64(i)*vDur, i%gop == 0)
	}
	return out
}

func audioRun(from int64, n int) []media.Frame {
	out := make([]media.Frame, n)
	for i := range out {
		out[i] = audio(from + int64(i)*aDur)
	}
	return out
}

// timed is a frame with the time it arrives in a multiplexed stream.
type timed struct {
	at int64
	f  media.Frame
}

// arrive appends frames that arrive at their DTS plus offset.
func arrive(items []timed, offset int64, frames []media.Frame) []timed {
	for _, f := range frames {
		items = append(items, timed{at: f.DTS + offset, f: f})
	}
	return items
}

// arrange orders frames by arrival time, keeping insertion order on ties.
func arrange(items []timed) []media.Frame {
	sort.SliceStable(items, func(i, j int) bool { return items[i].at < items[j].at })
	out := make([]media.Frame, len(items))
	for i, it := range items {
		out[i] = it.f
	}
	return out
}

// mux interleaves video and audio by DTS, with audio arriving lag ticks late.
func mux(lag int64, v, a []media.Frame) []media.Frame {
	return arrange(arrive(arrive(nil, 0, v), lag, a))
}

func push(c *Clock, frames ...media.Frame) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		out = append(out, c.Push(f)...)
	}
	return out
}

func epochIDs(c *Clock) []uint32 {
	var ids []uint32
	for _, e := range c.Epochs() {
		ids = append(ids, e.ID)
	}
	return ids
}

func inEpoch(frames []media.Frame, id uint32) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Epoch == id {
			out = append(out, f)
		}
	}
	return out
}

func onTrack(frames []media.Frame, track int) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

// nextStart applies rule 6 to the frames of one epoch: the largest over
// tracks of the largest PTS plus that track's last DTS delta.
func nextStart(frames []media.Frame) int64 {
	var start int64
	for _, track := range []int{0, 1} {
		fs := onTrack(frames, track)
		if len(fs) == 0 {
			continue
		}
		dur := int64(3600)
		if n := len(fs); n > 1 {
			dur = fs[n-1].DTS - fs[n-2].DTS
		}
		var maxPTS int64
		for _, f := range fs {
			maxPTS = max(maxPTS, f.PTS)
		}
		start = max(start, maxPTS+dur)
	}
	return start
}

func requireConform(t *testing.T, frames []media.Frame, skip ...string) {
	t.Helper()
	require.Nil(t, conform.CheckFrames(frames, conform.FrameOptions{Skip: skip}))
}

func TestClockFirstEpoch(t *testing.T) {
	c := New(Options{Kinds: av})
	out := push(c,
		video(1_000_000, true), audio(1_001_000),
		video(1_003_600, false), audio(1_002_920),
	)
	require.Len(t, out, 4)
	assert.Equal(t, []int64{0, 1000, 3600, 2920}, []int64{out[0].DTS, out[1].DTS, out[2].DTS, out[3].DTS})
	for _, f := range out {
		assert.Equal(t, uint32(1), f.Epoch)
		assert.Equal(t, f.DTS, f.PTS)
	}
	require.Len(t, c.Epochs(), 1)
	assert.Equal(t, uint32(1), c.Epochs()[0].ID)
	assert.Equal(t, int64(0), c.Epochs()[0].Start)
	requireConform(t, out)
}

func TestClockHoldsUntilAllTracks(t *testing.T) {
	c := New(Options{Kinds: av})
	assert.Empty(t, c.Push(video(0, true)))
	assert.Empty(t, c.Push(video(3600, false)))
	assert.Empty(t, c.Push(video(7200, false)))
	out := c.Push(audio(0))
	require.Len(t, out, 4)
	assert.Equal(t, []int{0, 0, 0, 1}, []int{out[0].Track, out[1].Track, out[2].Track, out[3].Track})
	requireConform(t, out)
}

func TestClockAlignWindow(t *testing.T) {
	c := New(Options{Kinds: av})
	for i := range 25 { // DTS 0 to 86400: span below 90000
		require.Empty(t, c.Push(video(int64(i)*vDur, i == 0)), "frame %d", i)
	}
	out := c.Push(video(90000, false))
	require.Len(t, out, 26)
	assert.Equal(t, int64(0), out[0].DTS)
	assert.Equal(t, int64(90000), out[25].DTS)
	assert.Equal(t, []uint32{1}, epochIDs(c))
}

func TestClockVideoWait(t *testing.T) {
	c := New(Options{Kinds: av})
	i := 0
	for ; int64(i)*aDur < 5*media.ClockRate; i++ {
		require.Empty(t, c.Push(audio(int64(i)*aDur)), "audio %d", i)
	}
	all := c.Push(audio(int64(i) * aDur)) // DTS 451200: span reaches 450000
	require.Len(t, all, i+1)
	for _, f := range all {
		require.Equal(t, uint32(1), f.Epoch)
	}
	for i++; i < 300; i++ {
		all = append(all, c.Push(audio(int64(i)*aDur))...)
	}
	// A non-key video frame in the audio-only epoch is gated and changes nothing.
	all = append(all, c.Push(video(560000, false))...)
	assert.Equal(t, []uint32{1}, epochIDs(c))
	assert.Equal(t, uint64(1), c.Stats().DroppedBeforeKey)

	all = append(all, push(c, video(576000, true), audio(576000), video(579600, false), audio(577920))...)
	assert.Equal(t, []uint32{1, 2}, epochIDs(c))
	e2 := inEpoch(all, 2)
	v, a := onTrack(e2, 0), onTrack(e2, 1)
	require.NotEmpty(t, v)
	require.NotEmpty(t, a)
	assert.True(t, v[0].Key)
	d := a[0].PTS - v[0].PTS
	assert.LessOrEqual(t, max(d, -d), int64(3600))
	requireConform(t, all)
}

func TestClockLaggingTrackNoFalseEpoch(t *testing.T) {
	c := New(Options{Kinds: av})
	frames := mux(72000, videoRun(0, 270, 25), audioRun(0, 469))
	out := push(c, frames...)
	out = append(out, c.Flush()...)
	assert.Equal(t, []uint32{1}, epochIDs(c))
	assert.Equal(t, Stats{}, c.Stats())
	assert.Len(t, out, len(frames))
	requireConform(t, out)
}

func TestClockBackwardJumpOpensEpoch(t *testing.T) {
	c := New(Options{Kinds: av})
	const src, loop, lag = 1_000_000, 6 * media.ClockRate, 27000
	// The same 6 s clip twice, audio muxed 0.3 s late, as a looped file.
	var items []timed
	items = arrive(items, 0, videoRun(src, 150, 25))
	items = arrive(items, lag, audioRun(src, 281))
	items = arrive(items, loop, videoRun(src, 150, 25))
	items = arrive(items, loop+lag, audioRun(src, 281))
	frames := arrange(items)

	out := push(c, frames...)
	out = append(out, c.Flush()...)
	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	assert.Equal(t, Stats{}, c.Stats())
	assert.Len(t, out, len(frames))
	want := nextStart(inEpoch(out, 1))
	assert.Equal(t, int64(540000), want) // video: 536400 + 3600; audio: 537600 + 1920
	assert.Equal(t, want, c.Epochs()[1].Start)
	requireConform(t, out)
}

func TestClockForwardJump(t *testing.T) {
	const total = 2 * (50 + 94)
	run := func(gap int64) (*Clock, []media.Frame) {
		c := New(Options{Kinds: av})
		v := videoRun(0, 50, 25) // last DTS 176400
		a := audioRun(0, 94)     // last DTS 178560
		v = append(v, videoRun(176400+gap, 50, 25)...)
		a = append(a, audioRun(178560+gap, 94)...)
		out := push(c, mux(0, v, a)...)
		return c, append(out, c.Flush()...)
	}

	t.Run("4s stays", func(t *testing.T) {
		c, out := run(4 * media.ClockRate)
		assert.Equal(t, []uint32{1}, epochIDs(c))
		assert.Len(t, out, total)
		for _, track := range []int{0, 1} {
			fs := onTrack(out, track)
			var gaps int
			for i := 1; i < len(fs); i++ {
				if fs[i].DTS-fs[i-1].DTS == 360000 {
					gaps++
				}
			}
			assert.Equal(t, 1, gaps, "track %d", track)
		}
		requireConform(t, out)
	})

	t.Run("6s opens epoch", func(t *testing.T) {
		c, out := run(6 * media.ClockRate)
		require.Equal(t, []uint32{1, 2}, epochIDs(c))
		assert.Len(t, out, total)
		assert.Equal(t, int64(178560+1920), c.Epochs()[1].Start)
		requireConform(t, out)
	})
}

func TestClockSingleTrackGap(t *testing.T) {
	c := New(Options{Kinds: av})
	v := videoRun(0, 50, 25) // last DTS 176400
	v = append(v, videoRun(176400+8*media.ClockRate, 50, 25)...)
	frames := mux(0, v, audioRun(0, 562))
	out := push(c, frames...)
	out = append(out, c.Flush()...)
	assert.Equal(t, []uint32{1}, epochIDs(c))
	assert.Equal(t, Stats{}, c.Stats())
	assert.Len(t, out, len(frames))
	fs := onTrack(out, 0)
	var maxGap int64
	for i := 1; i < len(fs); i++ {
		maxGap = max(maxGap, fs[i].DTS-fs[i-1].DTS)
	}
	assert.Equal(t, int64(8*media.ClockRate), maxGap)
	requireConform(t, out)
}

func TestClockSmallBackStep(t *testing.T) {
	c := New(Options{Kinds: []media.Kind{media.KindVideo}})
	out := push(c, video(0, true), video(3600, false), video(7200, false), video(6200, false), video(10800, false))
	require.Len(t, out, 4)
	assert.Equal(t, uint64(1), c.Stats().DroppedBackStep)
	assert.Equal(t, []uint32{1}, epochIDs(c))
	requireConform(t, out)
}

func TestClockBreak(t *testing.T) {
	c := New(Options{Kinds: av})
	out := push(c, mux(0, videoRun(0, 10, 25), audioRun(0, 19))...) // to 32400 / 34560
	c.Break()
	out = append(out, c.Push(video(36000, true))...)
	c.Break() // already pending: nothing changes
	out = append(out, push(c, audio(36480), video(39600, false), audio(38400))...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	e1, e2 := inEpoch(out, 1), inEpoch(out, 2)
	assert.Len(t, e1, 29)
	require.Len(t, e2, 4)
	assert.Equal(t, nextStart(e1), c.Epochs()[1].Start)
	assert.Equal(t, int64(34560+1920), c.Epochs()[1].Start)
	assert.Equal(t, c.Epochs()[1].Start, e2[0].DTS)
	assert.Equal(t, Stats{}, c.Stats())
	requireConform(t, out)
}

func TestClockConfigChangeOpensEpoch(t *testing.T) {
	newH264 := func() *media.TrackConfig { return &media.TrackConfig{Codec: media.CodecH264} }

	t.Run("continuous timestamps", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, mux(0, videoRun(0, 10, 25), audioRun(0, 19))...)
		n := len(out)
		changed := video(36000, true)
		changed.Config = newH264()
		later := video(39600, false)
		later.Config = changed.Config
		out = append(out, push(c, changed, audio(36480), later, audio(38400))...)

		require.Equal(t, []uint32{1, 2}, epochIDs(c))
		require.Len(t, out, n+4)
		assert.Same(t, changed.Config, out[n].Config)
		for _, f := range out[n:] {
			assert.Equal(t, uint32(2), f.Epoch)
		}
		requireConform(t, out)
	})

	t.Run("second change while pending restarts", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, mux(0, videoRun(0, 10, 25), audioRun(0, 19))...)
		first := video(36000, true)
		first.Config = newH264()
		second := video(39600, true)
		second.Config = newH264()
		out = append(out, push(c, first, second, audio(40320))...)

		require.Equal(t, []uint32{1, 2}, epochIDs(c))
		assert.Equal(t, uint64(1), c.Stats().DroppedStale)
		e2 := inEpoch(out, 2)
		require.Len(t, e2, 2)
		assert.Same(t, second.Config, e2[0].Config)
		requireConform(t, out)
	})
}

func TestClockStaleDropped(t *testing.T) {
	c := New(Options{Kinds: av})
	out := push(c, mux(0, videoRun(0, 251, 25), audioRun(0, 470))...) // to 900000 / 900480
	// Video loops back to 0; epoch 2 releases by AlignWindow with video only.
	out = append(out, push(c, videoRun(0, 26, 25)...)...)
	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	// Audio still on the old timeline after the release.
	out = append(out, push(c, audio(902400), audio(904320), audio(906240))...)
	assert.Equal(t, uint64(3), c.Stats().DroppedStale)
	// Audio on the new timeline joins epoch 2.
	out = append(out, c.Push(audio(93600))...)
	last := out[len(out)-1]
	assert.Equal(t, 1, last.Track)
	assert.Equal(t, uint32(2), last.Epoch)
	assert.Equal(t, []uint32{1, 2}, epochIDs(c))
	requireConform(t, out, conform.RuleStartOffset) // audio joins epoch 2 after release
}

func TestClockLateTrackDropped(t *testing.T) {
	c := New(Options{Kinds: []media.Kind{media.KindVideo, media.KindVideo}})
	second := &media.TrackConfig{Codec: media.CodecH264}
	v1 := func(dts int64, key bool) media.Frame {
		return media.Frame{Track: 1, Config: second, PTS: dts, DTS: dts, Key: key}
	}
	out := push(c, videoRun(100000, 25, 25)...) // to 186400
	require.Empty(t, out)
	out = c.Push(video(190000, false)) // span from the keyframe reaches 90000
	require.Len(t, out, 26)

	out = append(out, c.Push(v1(50000, true))...)
	assert.Equal(t, uint64(1), c.Stats().DroppedLate)
	// The late keyframe did not open track 1's gate.
	out = append(out, c.Push(v1(53600, false))...)
	assert.Equal(t, uint64(1), c.Stats().DroppedBeforeKey)
	out = append(out, c.Push(v1(193600, true))...)
	t1 := onTrack(out, 1)
	require.Len(t, t1, 1)
	assert.True(t, t1[0].Key)
	assert.Equal(t, int64(93600), t1[0].DTS)
	requireConform(t, out, conform.RuleStartOffset) // track 1 starts after release
}

func TestClockDropsUntilKey(t *testing.T) {
	c := New(Options{Kinds: []media.Kind{media.KindVideo}})
	out := push(c, video(0, false), video(3600, false), video(7200, false), video(10800, true), video(14400, false))
	assert.Equal(t, uint64(3), c.Stats().DroppedBeforeKey)
	require.Len(t, out, 2)
	assert.True(t, out[0].Key)
	assert.Equal(t, int64(0), out[0].DTS)
	requireConform(t, out)
}

func TestClockMidGOPStart(t *testing.T) {
	c := New(Options{Kinds: av})
	out := push(c, audio(-3840), audio(-1920)) // held: no keyframe yet
	key := video(0, true)
	key.PTS = 7200
	out = append(out, c.Push(key)...)
	for i := int64(1); i <= 25; i++ { // DTS 90000 at i = 25 releases by AlignWindow
		f := video(i*vDur, false)
		f.PTS = f.DTS + 7200
		out = append(out, c.Push(f)...)
	}
	require.Equal(t, []uint32{1}, epochIDs(c))
	// Audio after release, presented between the keyframe's DTS and PTS.
	out = append(out, push(c, audio(0), audio(1920), audio(3840), audio(5760), audio(7680), audio(9600))...)
	assert.Equal(t, uint64(6), c.Stats().DroppedEarlyAudio)
	v, a := onTrack(out, 0), onTrack(out, 1)
	require.NotEmpty(t, v)
	require.NotEmpty(t, a)
	d := a[0].PTS - v[0].PTS
	assert.GreaterOrEqual(t, d, int64(0))
	assert.LessOrEqual(t, d, int64(aDur))
	requireConform(t, out)
}

func TestClockDropsRASLAtEpochStart(t *testing.T) {
	h265 := &media.TrackConfig{Codec: media.CodecH265}
	frame := func(dts, pts int64, key, rasl bool) media.Frame {
		return media.Frame{Track: 0, Config: h265, DTS: dts, PTS: pts, Key: key, RASL: rasl}
	}
	gop := []media.Frame{
		frame(0, 10800, true, false), // CRA
		frame(3600, 3600, false, true),
		frame(7200, 7200, false, true),
		frame(10800, 14400, false, false),
	}

	t.Run("video and audio", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, gop...)
		require.Empty(t, out)
		out = c.Push(audio(10800))
		require.Len(t, out, 3)
		assert.True(t, out[0].Key) // the CRA
		assert.Equal(t, int64(0), out[0].DTS)
		assert.Equal(t, int64(10800), out[1].DTS)
		assert.Equal(t, 1, out[2].Track)
		assert.Equal(t, uint64(2), c.Stats().DroppedRASL)
		requireConform(t, out)
	})

	t.Run("video only", func(t *testing.T) {
		c := New(Options{Kinds: []media.Kind{media.KindVideo}})
		require.Len(t, c.Push(gop[0]), 1) // the CRA alone releases the epoch
		out := push(c, gop[1:]...)
		require.Len(t, out, 1)
		assert.Equal(t, int64(10800), out[0].DTS)
		assert.Equal(t, uint64(2), c.Stats().DroppedRASL)
	})
}

func TestClockEpochStartAfterPresentationEnd(t *testing.T) {
	c := New(Options{Kinds: []media.Kind{media.KindVideo}})
	frame := func(dts, pts int64, key bool) media.Frame {
		return media.Frame{Track: 0, Config: h264, DTS: dts, PTS: pts, Key: key}
	}
	out := push(c,
		frame(0, 7200, true),       // I
		frame(3600, 18000, false),  // P
		frame(7200, 10800, false),  // B
		frame(10800, 14400, false), // B
	)
	require.Len(t, out, 4)
	c.Break()
	out = append(out, push(c, frame(14400, 14400, true), frame(18000, 18000, false), frame(21600, 21600, false))...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	assert.Equal(t, int64(21600), c.Epochs()[1].Start)
	e2 := inEpoch(out, 2)
	require.Len(t, e2, 3)
	assert.Greater(t, e2[0].PTS, int64(18000))
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
}

func TestClockJumpWhilePending(t *testing.T) {
	const t1, t2 = 5_000_000, 9_000_000
	c := New(Options{Kinds: av})
	out := push(c, mux(0, videoRun(0, 50, 25), audioRun(0, 94))...) // to 176400 / 178560
	// First jump: audio leads onto t1 and is held; video on t1 has no keyframe yet.
	out = append(out, push(c, audio(t1), audio(t1+1920), audio(t1+3840), video(t1, false), video(t1+3600, false))...)
	assert.Equal(t, uint64(2), c.Stats().DroppedBeforeKey)
	// Second jump before release: video keyframe on t2 drops the held audio.
	out = append(out, c.Push(video(t2, true))...)
	assert.Equal(t, uint64(3), c.Stats().DroppedStale)
	// Audio still lagging on t1 is stale and does not restart the epoch.
	out = append(out, push(c, audio(t1+5760), audio(t1+7680))...)
	assert.Equal(t, uint64(5), c.Stats().DroppedStale)
	out = append(out, push(c, audio(t2), video(t2+3600, false), audio(t2+1920))...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	start := c.Epochs()[1].Start
	e2 := inEpoch(out, 2)
	require.Len(t, e2, 4)
	assert.True(t, e2[0].Key)
	assert.Equal(t, start, e2[0].DTS)
	assert.Equal(t, []int64{start, start, start + 3600, start + 1920},
		[]int64{e2[0].DTS, e2[1].DTS, e2[2].DTS, e2[3].DTS})
	requireConform(t, out)
}

// A restart keeps each joined track's lastSourceDTS for the lag test, and a
// joined track's next accepted frame rejoins the restarted epoch, so a second
// jump backwards does not turn the new timeline into back steps.
func TestClockRestartRejoinsBackward(t *testing.T) {
	const t1, t2 = 5_000_000, 1_000_000
	c := New(Options{Kinds: av})
	out := push(c, mux(0, videoRun(0, 50, 25), audioRun(0, 94))...)
	out = append(out, push(c, audio(t1), audio(t1+1920), video(t1, false))...)
	out = append(out, c.Push(video(t2, true))...) // jump back against the held audio
	out = append(out, push(c, audio(t1+3840))...) // lagging on t1: stale
	out = append(out, push(c, audio(t2), video(t2+3600, false))...)

	require.Equal(t, []uint32{1, 2}, epochIDs(c))
	assert.Equal(t, uint64(0), c.Stats().DroppedBackStep)
	assert.Equal(t, uint64(3), c.Stats().DroppedStale)
	e2 := inEpoch(out, 2)
	require.Len(t, e2, 3)
	assert.Equal(t, 1, e2[1].Track)
	requireConform(t, out)
}

func TestClockWallAnchor(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clk := t0
	c := New(Options{Kinds: av, Now: func() time.Time { return clk }})
	at := func(sec int, f media.Frame) []media.Frame {
		clk = t0.Add(time.Duration(sec) * time.Second)
		return c.Push(f)
	}
	key := video(0, true)
	key.PTS = 7200

	require.Empty(t, at(1, audio(-3840)))        // first push: held, then dropped as early audio
	require.Empty(t, at(2, video(-3600, false))) // dropped before the keyframe: never held
	require.Empty(t, at(3, audio(7680)))         // held and kept: the first frame released
	out := at(4, key)                            // drops the early audio, then releases the epoch
	require.Len(t, out, 2)
	assert.Equal(t, []int{1, 0}, []int{out[0].Track, out[1].Track})
	assert.Equal(t, uint64(1), c.Stats().DroppedEarlyAudio)
	require.Len(t, c.Epochs(), 1)
	assert.Equal(t, t0.Add(3*time.Second), c.Epochs()[0].Wall) // not 1 s (first push) or 4 s (release)

	// The next epoch anchors on its own first released frame.
	c.Break()
	require.Empty(t, at(6, video(14400, false))) // before the keyframe
	require.Empty(t, at(7, video(18000, true)))
	require.Len(t, at(8, audio(18000)), 2)
	require.Len(t, c.Epochs(), 2)
	assert.Equal(t, t0.Add(7*time.Second), c.Epochs()[1].Wall)
}

// A Config change with continuous timestamps restarts a pending epoch in which
// audio had joined. Audio has nothing held when video alone releases, and its
// continuous frames afterwards are not a jump, so none of them is stale.
func TestClockConfigRestartKeepsContinuousAudio(t *testing.T) {
	newH264 := func(dts int64, key bool) media.Frame {
		f := video(dts, key)
		f.Config = &media.TrackConfig{Codec: media.CodecH264}
		return f
	}
	run := func(t *testing.T, c *Clock, out []media.Frame, audioFrom int64) {
		t.Helper()
		require.Equal(t, []uint32{1, 2}, epochIDs(c))
		out = append(out, push(c, audioRun(audioFrom, 400)...)...)
		assert.Equal(t, uint64(1), c.Stats().DroppedStale) // the held audio frame only
		assert.Len(t, onTrack(inEpoch(out, 2), 1), 400)
		assert.Equal(t, []uint32{1, 2}, epochIDs(c))
		requireConform(t, out)
	}

	t.Run("after Break", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, mux(0, videoRun(0, 10, 25), audioRun(0, 19))...) // to 32400 / 34560
		c.Break()
		out = append(out, c.Push(audio(36480))...) // joins epoch 2, held
		changed := newH264(36000, true)            // restarts epoch 2
		out = append(out, c.Push(changed)...)
		for i := int64(1); i <= 25; i++ { // video alone releases by AlignWindow
			f := video(36000+i*vDur, false)
			f.Config = changed.Config
			out = append(out, c.Push(f)...)
		}
		run(t, c, out, 38400)
	})

	t.Run("after a jump", func(t *testing.T) {
		const t1 = 5_000_000
		c := New(Options{Kinds: av})
		out := push(c, mux(0, videoRun(0, 50, 25), audioRun(0, 94))...)
		out = append(out, c.Push(audio(t1))...) // opens epoch 2, held
		changed := newH264(t1, true)            // restarts epoch 2
		out = append(out, c.Push(changed)...)
		for i := int64(1); i <= 25; i++ {
			f := video(t1+i*vDur, false)
			f.Config = changed.Config
			out = append(out, c.Push(f)...)
		}
		run(t, c, out, t1+1920)
	})
}

func TestClockDropsInvalid(t *testing.T) {
	c := New(Options{Kinds: []media.Kind{media.KindVideo}})
	bad := video(3600, false)
	bad.PTS = 0
	out := push(c, video(0, true), bad, video(7200, false))
	require.Len(t, out, 2)
	assert.Equal(t, uint64(1), c.Stats().DroppedInvalid)
	assert.Equal(t, Stats{DroppedInvalid: 1}, c.Stats())
	requireConform(t, out)
}

// A track that joined before a restart but had no frame held at release
// rejoins the released epoch on its new timeline, and its frames still on the
// earlier timeline are stale.
func TestClockRestartedTrackRejoinsAfterRelease(t *testing.T) {
	t.Run("jump epoch", func(t *testing.T) {
		const t1, t2 = 5_000_000, 1_000_000
		c := New(Options{Kinds: av})
		out := push(c, mux(0, videoRun(0, 50, 25), audioRun(0, 94))...)
		out = append(out, push(c, audio(t1), audio(t1+1920))...) // opens epoch 2, held
		out = append(out, push(c, videoRun(t2, 26, 25)...)...)   // restarts it; video alone releases by AlignWindow
		require.Equal(t, []uint32{1, 2}, epochIDs(c))
		out = append(out, c.Push(audio(t1+3840))...) // lagging on t1: stale
		out = append(out, push(c, audioRun(t2+93600, 100)...)...)

		assert.Equal(t, uint64(0), c.Stats().DroppedBackStep)
		assert.Equal(t, uint64(3), c.Stats().DroppedStale)
		assert.Len(t, onTrack(inEpoch(out, 2), 1), 100)
		assert.Equal(t, []uint32{1, 2}, epochIDs(c))
		requireConform(t, out, conform.RuleStartOffset) // audio joins epoch 2 after release
	})

	t.Run("epoch 1", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, audio(0), audio(1920))                         // held
		out = append(out, push(c, videoRun(1_000_000, 26, 25)...)...) // restarts epoch 1; releases by AlignWindow
		require.Equal(t, []uint32{1}, epochIDs(c))
		out = append(out, c.Push(audio(3840))...) // lagging on the earlier timeline: stale, no new epoch
		out = append(out, push(c, audioRun(1_093_600, 10)...)...)

		assert.Equal(t, uint64(3), c.Stats().DroppedStale)
		assert.Equal(t, []uint32{1}, epochIDs(c))
		assert.Len(t, onTrack(out, 1), 10)
		requireConform(t, out, conform.RuleStartOffset)
	})
}

func TestClockFlush(t *testing.T) {
	second := &media.TrackConfig{Codec: media.CodecAAC}
	c := New(Options{Kinds: []media.Kind{media.KindVideo, media.KindAudio, media.KindAudio}})
	a2 := media.Frame{Track: 2, Config: second, DTS: 3600, PTS: 3600, Key: true}
	out := push(c, a2, video(0, true), video(3600, false))
	require.Empty(t, out)
	out = c.Flush()
	require.Len(t, out, 3)
	assert.Equal(t, []int{2, 0, 0}, []int{out[0].Track, out[1].Track, out[2].Track})
	assert.Equal(t, []int64{3600, 0, 3600}, []int64{out[0].DTS, out[1].DTS, out[2].DTS})
	assert.Equal(t, []uint32{1}, epochIDs(c))
	assert.Empty(t, c.Flush())
}

func TestClockPendingEdges(t *testing.T) {
	t.Run("back step while pending", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, video(0, true), video(3600, false), video(2600, false), audio(0))
		assert.Len(t, out, 3)
		assert.Equal(t, uint64(1), c.Stats().DroppedBackStep)
	})

	t.Run("align window without video counts from the first held frame", func(t *testing.T) {
		second := &media.TrackConfig{Codec: media.CodecAAC}
		c := New(Options{Kinds: []media.Kind{media.KindAudio, media.KindAudio}})
		a := func(dts int64) media.Frame {
			return media.Frame{Track: 1, Config: second, DTS: dts, PTS: dts, Key: true}
		}
		for i := range 47 { // DTS 1000 to 89320 from the first held 1000
			require.Empty(t, c.Push(a(1000+int64(i)*aDur)))
		}
		out := c.Push(a(1000 + 47*aDur)) // span 90240
		assert.Len(t, out, 48)
	})

	t.Run("joining track jumps against held frames", func(t *testing.T) {
		c := New(Options{Kinds: av})
		out := push(c, video(0, true), audio(1_000_000), video(1_000_000, true))
		assert.Equal(t, uint64(1), c.Stats().DroppedStale)
		require.Len(t, out, 2)
		assert.Equal(t, []int64{0, 0}, []int64{out[0].DTS, out[1].DTS})
		assert.Equal(t, []int{1, 0}, []int{out[0].Track, out[1].Track})
	})
}

func TestClockEpochsKeepsLast64(t *testing.T) {
	c := New(Options{Kinds: []media.Kind{media.KindVideo}})
	for i := range 70 {
		require.Len(t, c.Push(video(int64(i)*vDur, true)), 1)
		c.Break()
	}
	ids := epochIDs(c)
	require.Len(t, ids, 64)
	assert.Equal(t, uint32(7), ids[0])
	assert.Equal(t, uint32(70), ids[63])
}
