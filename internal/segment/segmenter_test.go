package segment

import (
	"bytes"
	"cmp"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

var (
	h264 = &media.TrackConfig{Codec: media.CodecH264}
	aac  = &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 48000, Channels: 2}
)

const aDur = 1920

func vid(epoch uint32, pts int64, key bool) hub.Delivery {
	return hub.Delivery{Frame: media.Frame{Track: 0, Config: h264, PTS: pts, DTS: pts, Key: key, Epoch: epoch}}
}

func aud(pts int64) hub.Delivery {
	return hub.Delivery{Frame: media.Frame{Track: 1, Config: aac, PTS: pts, DTS: pts, Key: true, Epoch: 1}}
}

// video pushes n 25 fps frames from start with a keyframe every gop ticks.
func video(s *Segmenter, epoch uint32, start int64, n int, gop int64) []Segment {
	var out []Segment
	for i := 0; i < n; i++ {
		pts := start + int64(i)*3600
		out = append(out, s.Push(vid(epoch, pts, (pts-start)%gop == 0))...)
	}
	return out
}

func TestSegmentCutsOnKeyAfterTarget(t *testing.T) {
	s := New(Options{})
	segs := video(s, 1, 0, 125, 90000)
	require.Len(t, segs, 2)
	assert.EqualValues(t, 0, segs[0].Start)
	assert.EqualValues(t, 180000, segs[0].Duration)
	assert.EqualValues(t, 180000, segs[1].Start)
	assert.EqualValues(t, 180000, segs[1].Duration)
	assert.EqualValues(t, 180000, segs[0].End)
	assert.Len(t, segs[0].Frames, 50)
	assert.False(t, segs[0].Discontinuity)
	assert.False(t, segs[1].Discontinuity)
	rest := s.Flush()
	require.Len(t, rest, 1)
	assert.EqualValues(t, 360000, rest[0].Start)
	assert.EqualValues(t, 90000, rest[0].Duration)
}

func TestSegmentLongGOP(t *testing.T) {
	s := New(Options{})
	segs := video(s, 1, 0, 200, 270000)
	require.Len(t, segs, 2)
	assert.EqualValues(t, 270000, segs[0].Duration)
	assert.EqualValues(t, 270000, segs[1].Start)
}

func TestSegmentEpochChange(t *testing.T) {
	s := New(Options{})
	segs := video(s, 1, 0, 30, 90000)
	require.Empty(t, segs)
	segs = video(s, 2, 1000000, 10, 90000)
	require.Len(t, segs, 1)
	assert.EqualValues(t, 30*3600, segs[0].Duration)
	assert.EqualValues(t, 1, segs[0].Epoch)
	out := s.Flush()
	require.Len(t, out, 1)
	assert.EqualValues(t, 2, out[0].Epoch)
	assert.True(t, out[0].Discontinuity)
	assert.EqualValues(t, 1, out[0].Seq)
}

func TestSegmentLost(t *testing.T) {
	s := New(Options{})
	require.Empty(t, video(s, 1, 0, 10, 90000))
	d := vid(1, 500000, true)
	d.Lost = 7
	segs := s.Push(d)
	require.Len(t, segs, 1)
	assert.EqualValues(t, 10*3600, segs[0].Duration)
	segs = append(segs, video(s, 1, 503600, 5, 90000)...)
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 2)
	assert.True(t, segs[1].Discontinuity)
	assert.EqualValues(t, 500000, segs[1].Start)
}

func TestSegmentLostDropsEarlyAudio(t *testing.T) {
	s := New(Options{})
	require.Empty(t, video(s, 1, 0, 10, 90000))
	d := vid(1, 500000, true)
	d.Lost = 3
	segs := s.Push(d)
	require.Len(t, segs, 1)
	assert.Empty(t, s.Push(aud(499000)))
	assert.EqualValues(t, 1, s.Dropped())
	assert.Empty(t, s.Push(aud(500500)))
	assert.EqualValues(t, 1, s.Dropped())
	out := s.Flush()
	require.Len(t, out, 1)
	assert.Len(t, out[0].Frames, 2)
	assert.True(t, out[0].Discontinuity)
}

func TestSegmentAudioFirstCarried(t *testing.T) {
	s := New(Options{})
	require.Empty(t, s.Push(aud(1000)))
	require.Empty(t, s.Push(aud(1000+aDur)))
	require.Empty(t, s.Push(vid(1, 1000, true))) // re-anchors; no segment emitted
	require.Empty(t, s.Push(vid(1, 4600, false)))
	out := s.Flush()
	require.Len(t, out, 1)
	assert.EqualValues(t, 0, out[0].Seq)
	assert.False(t, out[0].Discontinuity)
	assert.EqualValues(t, 1000, out[0].Start)
	require.Len(t, out[0].Frames, 4)
	assert.Equal(t, 1, out[0].Frames[0].Track)
	assert.Equal(t, 1, out[0].Frames[1].Track)
	assert.True(t, out[0].Frames[2].Key)
	assert.Zero(t, s.Dropped())
}

func TestSegmentAudioFirstWaitsForKey(t *testing.T) {
	s := New(Options{})
	require.Empty(t, s.Push(aud(0)))
	require.Empty(t, s.Push(aud(aDur)))
	require.Empty(t, s.Push(vid(1, 0, false))) // dropped: not a keyframe
	require.Empty(t, s.Push(aud(2*aDur)))      // joins the pending audio
	require.Empty(t, s.Push(vid(1, 3*aDur, true)))
	out := s.Flush()
	require.Len(t, out, 1)
	assert.EqualValues(t, 0, out[0].Seq)
	assert.EqualValues(t, 3*aDur, out[0].Start)
	assert.Len(t, out[0].Frames, 1)       // audio below the key's PTS was dropped
	assert.EqualValues(t, 4, s.Dropped()) // 3 audio + the non-key video
}

func TestSegmentAnchorChangeAfterAudioSegment(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	for i := int64(0); i < 100; i++ { // real audio-only stretch: closes by rule 4
		segs = append(segs, s.Push(aud(i*aDur))...)
	}
	require.Len(t, segs, 1)
	segs = append(segs, s.Push(vid(1, 100*aDur, true))...)
	require.Len(t, segs, 1, "the closed audio segment waits for audio to reach the cut")
	segs = append(segs, s.Push(aud(100*aDur))...)
	require.Len(t, segs, 2) // the open audio segment closed, not carried
	assert.Equal(t, uint64(1), segs[1].Seq)
	assert.False(t, segs[1].Discontinuity)
	out := s.Flush()
	require.Len(t, out, 1)
	assert.False(t, out[0].Discontinuity)
	assert.EqualValues(t, 2, out[0].Seq)
}

func TestSegmentLostWithoutOpenSegment(t *testing.T) {
	s := New(Options{})
	require.Empty(t, video(s, 1, 0, 5, 90000))
	require.Len(t, s.Flush(), 1) // nothing open now
	d := vid(1, 900000, true)
	d.Lost = 2
	require.Empty(t, s.Push(d))
	out := s.Flush()
	require.Len(t, out, 1)
	assert.True(t, out[0].Discontinuity)
	assert.EqualValues(t, 1, out[0].Seq)
}

func TestSegmentAudioOnly(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	for i := int64(0); i < 300; i++ {
		segs = append(segs, s.Push(aud(i*aDur))...)
	}
	require.NotEmpty(t, segs)
	// Cut at the first frame at or after 180000: frame 94 (180480).
	assert.EqualValues(t, 94*aDur, segs[0].Duration)
	assert.EqualValues(t, 94*aDur, segs[1].Start)
	assert.Len(t, segs[0].Frames, 94)
	assert.EqualValues(t, 94*aDur, segs[0].End)
}

func TestSegmentDropsBeforeKey(t *testing.T) {
	s := New(Options{})
	assert.Empty(t, s.Push(vid(1, 0, false)))
	assert.Empty(t, s.Push(vid(1, 3600, false)))
	assert.EqualValues(t, 2, s.Dropped())
	assert.Empty(t, s.Push(vid(1, 7200, true)))
	assert.Empty(t, s.Push(aud(7000))) // audio in an open video-anchored segment is kept
	out := s.Flush()
	require.Len(t, out, 1)
	assert.EqualValues(t, 7200, out[0].Start)
}

func TestSegmentFlush(t *testing.T) {
	s := New(Options{})
	assert.Empty(t, s.Flush())
	s.Push(vid(1, 0, true))
	out := s.Flush()
	require.Len(t, out, 1)
	assert.EqualValues(t, 3600, out[0].Duration) // one frame
	assert.EqualValues(t, 3600, out[0].End)
	assert.Empty(t, s.Flush())
}

func TestSegmentEndOverTracks(t *testing.T) {
	s := New(Options{})
	s.Push(vid(1, 0, true))
	s.Push(aud(0))
	s.Push(aud(aDur))
	s.Push(aud(2 * aDur))
	s.Push(vid(1, 3600, false))
	out := s.Flush()
	require.Len(t, out, 1)
	assert.EqualValues(t, 7200, out[0].Duration)
	assert.EqualValues(t, 7200, out[0].End) // video 3600+3600 beats audio 2*1920+1920
}

func TestSegmentSeqMonotonic(t *testing.T) {
	s := New(Options{})
	all := make([]Segment, 0, 8)
	all = append(all, video(s, 1, 0, 100, 90000)...)
	all = append(all, video(s, 2, 10000000, 100, 90000)...)
	all = append(all, s.Flush()...)
	require.Greater(t, len(all), 2)
	for i, g := range all {
		assert.EqualValues(t, i, g.Seq)
	}
}

// fixtureFrames returns the frames of a fixture as the Clock delivers them.
func fixtureFrames(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		out = append(out, c.Push(f)...)
	}
	return append(out, c.Flush()...)
}

func TestSegmentFixture(t *testing.T) {
	segs := pushAll(New(Options{}), fixtureFrames(t, "h264_aac.ts"))
	require.Len(t, segs, 3)
	for i, g := range segs {
		assert.EqualValues(t, i, g.Seq)
		assert.Positive(t, g.Duration)
	}
}

// One keyframe, then 60 s of video without one: the open segment is cut at
// MaxDuration (default 5 × Target) on a non-key frame, and the segment it
// opens is Forced and not a discontinuity.
func TestSegmentMaxDurationWithoutKeyframes(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	for i := 0; i < 1500; i++ {
		pts := int64(i) * 3600
		segs = append(segs, s.Push(vid(1, pts, i == 0))...)
		segs = append(segs, s.Push(aud(pts))...)
	}
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 6)
	for i, sg := range segs {
		assert.EqualValues(t, int64(i)*900000, sg.Start, "segment %d", i)
		assert.LessOrEqual(t, sg.Duration, int64(900000), "segment %d", i)
		assert.Equal(t, i > 0, sg.Forced, "segment %d", i)
		assert.False(t, sg.Discontinuity, "segment %d", i)
		require.NotEmpty(t, sg.Frames)
		assert.Equal(t, i == 0, sg.Frames[0].Key && sg.Frames[0].Track == 0, "segment %d", i)
	}
}

// The cap is a field; it never goes below Target.
func TestSegmentMaxDurationOption(t *testing.T) {
	s := New(Options{Target: 90000, MaxDuration: 180000})
	segs := video(s, 1, 0, 100, 1<<40)
	require.Len(t, segs, 1)
	assert.EqualValues(t, 180000, segs[0].Duration)
	s = New(Options{Target: 180000, MaxDuration: 90000})
	segs = video(s, 1, 0, 100, 1<<40)
	require.Len(t, segs, 1)
	assert.EqualValues(t, 180000, segs[0].Duration, "raised to Target")
}

// Video stops entirely while audio goes on: the cap cuts on an audio frame,
// so the open segment still stays bounded.
func TestSegmentMaxDurationVideoStalls(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	segs = append(segs, video(s, 1, 0, 25, 90000)...)
	for i := 0; i < 1500; i++ {
		segs = append(segs, s.Push(aud(int64(i)*aDur))...)
	}
	require.NotEmpty(t, segs)
	for _, sg := range segs {
		// Cut on the first frame at or past the cap: within one audio frame.
		assert.LessOrEqual(t, sg.Duration, int64(900000+aDur))
	}
	assert.True(t, segs[len(segs)-1].Forced)
}

// Leading audio waiting for the video anchor's first keyframe is kept to
// MaxDuration, so a video track that never keys cannot grow it unbounded.
func TestSegmentCarryBounded(t *testing.T) {
	s := New(Options{})
	require.Empty(t, s.Push(aud(0)))
	require.Empty(t, s.Push(vid(1, 0, false))) // video anchor, no keyframe: audio waits
	for i := 1; i < 3000; i++ {
		require.Empty(t, s.Push(aud(int64(i)*aDur)))
	}
	assert.LessOrEqual(t, len(s.carry), 900000/aDur+1)
	assert.Positive(t, s.Dropped())
}

// assertAudioInside checks the R-F3 guarantee on segs: every audio frame
// lies in [Start, Start+Duration), and the first one starts within one AAC
// frame of Start.
func assertAudioInside(t *testing.T, segs []Segment) {
	t.Helper()
	for _, g := range segs {
		first := true
		for _, f := range g.Frames {
			if f.Kind() != media.KindAudio {
				continue
			}
			assert.GreaterOrEqual(t, f.PTS, g.Start, "segment %d: audio %d before Start", g.Seq, f.PTS)
			assert.Less(t, f.PTS, g.Start+g.Duration, "segment %d: audio %d past the end", g.Seq, f.PTS)
			if first {
				assert.Less(t, f.PTS, g.Start+aDur, "segment %d: first audio %d", g.Seq, f.PTS)
				first = false
			}
		}
	}
}

// reorder returns frames in an arrival order where audio leads video by
// lead ticks (lags it when lead is negative): video arrives at its DTS,
// audio at its PTS − lead.
func reorder(frames []media.Frame, lead int64) []media.Frame {
	out := slices.Clone(frames)
	key := func(f media.Frame) int64 {
		if f.Kind() == media.KindAudio {
			return f.PTS - lead
		}
		return f.DTS
	}
	slices.SortStableFunc(out, func(a, b media.Frame) int { return cmp.Compare(key(a), key(b)) })
	return out
}

// pushAll pushes frames and returns every segment, Flush included.
func pushAll(s *Segmenter, frames []media.Frame) []Segment {
	var segs []Segment
	for _, f := range frames {
		segs = append(segs, s.Push(hub.Delivery{Frame: f})...)
	}
	return append(segs, s.Flush()...)
}

func assertSeqs(t *testing.T, segs []Segment) {
	t.Helper()
	for i, g := range segs {
		assert.EqualValues(t, i, g.Seq, "segment %d out of order", i)
	}
}

// Interleaved TS input with audio up to 300 ms ahead of video: audio is cut
// at the video keyframe's PTS, not by arrival order.
func TestAudioCutAtVideoPTS(t *testing.T) {
	frames := fixtureFrames(t, "h264_aac.ts")
	for _, lead := range []int64{0, 9000, 18000, 27000} {
		t.Run(strconv.FormatInt(lead/90, 10)+"ms", func(t *testing.T) {
			s := New(Options{})
			segs := pushAll(s, reorder(frames, lead))
			require.Len(t, segs, 3)
			assertSeqs(t, segs)
			assertAudioInside(t, segs[:len(segs)-1])
			assert.Zero(t, s.Late())
		})
	}
	// Audio 300 ms behind video: segments are held until audio passes the
	// cut, and audio below the first keyframe of the epoch is late.
	t.Run("lagging", func(t *testing.T) {
		s := New(Options{})
		segs := pushAll(s, reorder(frames, -27000))
		require.Len(t, segs, 3)
		assertSeqs(t, segs)
		var late uint64
		for _, f := range segs[0].Frames {
			if f.Kind() == media.KindAudio && f.PTS < segs[0].Start {
				late++
			}
		}
		assert.Equal(t, late, s.Late())
		assertAudioInside(t, segs[1:len(segs)-1])
	})
}

// Audio stops inside segment 2: segment 2's cut waits for it until the
// anchor is 1 s past the cut, and later cuts no longer wait.
func TestAudioCutHoldReleasedByAnchor(t *testing.T) {
	s := New(Options{})
	emitted := map[uint64]int64{} // Seq → last video PTS pushed when it came out
	var segs []Segment
	var nextA, lastV int64
	collect := func(out []Segment) {
		for _, g := range out {
			emitted[g.Seq] = lastV
			segs = append(segs, g)
		}
	}
	for i := 0; i <= 250; i++ {
		lastV = int64(i) * 3600
		collect(s.Push(vid(1, lastV, lastV%180000 == 0)))
		for ; nextA <= lastV+3600 && nextA < 405000; nextA += aDur {
			collect(s.Push(aud(nextA)))
		}
	}
	require.Len(t, segs, 5)
	assertSeqs(t, segs)
	assert.Equal(t, map[uint64]int64{0: 180000, 1: 360000, 2: 630000, 3: 720000, 4: 900000}, emitted)
	assertAudioInside(t, segs[:2])
	for _, g := range segs {
		assert.EqualValues(t, 180000, g.Duration, "segment %d", g.Seq)
	}
	assert.Zero(t, s.Late())
}

// A segment held for lagging audio takes the audio below its cut; once it
// is released, audio below the cut goes to the next segment and is late.
func TestAudioLateCounted(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	var nextA int64
	for v := int64(0); v <= 180000; v += 3600 {
		segs = append(segs, s.Push(vid(1, v, v%180000 == 0))...)
		for ; nextA <= v-9000; nextA += aDur {
			segs = append(segs, s.Push(aud(nextA))...)
		}
	}
	require.Empty(t, segs, "held: audio has not reached the cut")
	for ; nextA < 180000; nextA += aDur {
		require.Empty(t, s.Push(aud(nextA)))
	}
	segs = s.Push(aud(nextA)) // 180480: past the cut
	require.Len(t, segs, 1)
	assertAudioInside(t, segs)
	assert.Zero(t, s.Late())
	require.Empty(t, s.Push(aud(179000)))
	assert.EqualValues(t, 1, s.Late())
	segs = s.Flush()
	require.Len(t, segs, 1)
	assert.True(t, slices.ContainsFunc(segs[0].Frames, func(f media.Frame) bool { return f.PTS == 179000 }))
}

// heldVideo pushes epoch 1 video with a keyframe every 2 s from 0 to end,
// with audio arriving 100 ms late, and returns what came out.
func heldVideo(s *Segmenter, end int64) []Segment {
	var segs []Segment
	var nextA int64
	for v := int64(0); v <= end; v += 3600 {
		segs = append(segs, s.Push(vid(1, v, v%180000 == 0))...)
		for ; nextA <= v-9000; nextA += aDur {
			segs = append(segs, s.Push(aud(nextA))...)
		}
	}
	return segs
}

func TestAudioCutReleaseOnEpoch(t *testing.T) {
	s := New(Options{})
	segs := heldVideo(s, 360000)
	require.Len(t, segs, 1)
	require.Len(t, s.held, 1, "the cut at 4 s waits for audio")
	segs = append(segs, s.Push(vid(2, 10_000_000, true))...)
	require.Len(t, segs, 3)
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 4)
	assertSeqs(t, segs)
	assert.EqualValues(t, []uint32{1, 1, 1, 2}, []uint32{segs[0].Epoch, segs[1].Epoch, segs[2].Epoch, segs[3].Epoch})
	assert.EqualValues(t, 180000, segs[1].Start)
	assert.EqualValues(t, 180000, segs[1].Duration)
	assert.EqualValues(t, 360000, segs[2].Start)
	assert.True(t, segs[3].Discontinuity)
	assert.Empty(t, s.held)
}

func TestAudioCutReleaseOnLost(t *testing.T) {
	s := New(Options{})
	segs := heldVideo(s, 360000)
	require.Len(t, segs, 1)
	require.Len(t, s.held, 1)
	d := vid(1, 500000, true)
	d.Lost = 4
	segs = append(segs, s.Push(d)...)
	require.Len(t, segs, 3)
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 4)
	assertSeqs(t, segs)
	assert.EqualValues(t, 360000, segs[2].Start)
	assert.EqualValues(t, 500000, segs[3].Start)
	assert.True(t, segs[3].Discontinuity)
	assert.Empty(t, s.held)
}

// No keyframe for 6.4 s with MaxDuration 4 s: the forced cut, at the PTS of
// the first frame presented 4 s past the start, cuts audio there too. With
// audio ahead that frame is audio; with audio behind it is video, and the
// segment waits for the audio below the cut.
func TestAudioCutForcedCut(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lead, at   int64
		durations  []int64
		forcedFrom int
	}{
		{"audio-ahead", 18000, 188 * aDur, []int64{188 * aDur, 576000 - 188*aDur, 180000}, 1},
		{"audio-behind", -18000, 360000, []int64{360000, 216000, 180000}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{MaxDuration: 360000})
			var segs []Segment
			var nextA int64
			for v := int64(0); v <= 900000; v += 3600 {
				key := v == 0 || (v >= 576000 && (v-576000)%180000 == 0)
				segs = append(segs, s.Push(vid(1, v, key))...)
				for ; nextA <= v+tc.lead; nextA += aDur {
					segs = append(segs, s.Push(aud(nextA))...)
				}
			}
			segs = append(segs, s.Flush()...)
			require.Len(t, segs, 4)
			assertSeqs(t, segs)
			assert.True(t, segs[1].Forced)
			assert.False(t, segs[2].Forced)
			assert.Equal(t, tc.at, segs[1].Start)
			for i, d := range tc.durations {
				assert.Equal(t, d, segs[i].Duration, "segment %d", i)
			}
			assertAudioInside(t, segs[:len(segs)-1])
			assert.Zero(t, s.Late())
		})
	}
}

// Target 0.5 s (50 fps video, a keyframe every 0.5 s), and audio falls
// 0.9 s behind after 2 s: several segments wait at once and come out in
// order.
func TestAudioCutShortTarget(t *testing.T) {
	s := New(Options{Target: 45000})
	var segs []Segment
	var nextA int64
	maxHeld := 0
	for v := int64(0); v <= 900000; v += 1800 {
		segs = append(segs, s.Push(vid(1, v, v%45000 == 0))...)
		lag := int64(0)
		if v >= 180000 {
			lag = 81000
		}
		for ; nextA <= v-lag; nextA += aDur {
			segs = append(segs, s.Push(aud(nextA))...)
		}
		maxHeld = max(maxHeld, len(s.held))
	}
	assert.GreaterOrEqual(t, maxHeld, 2)
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 21)
	assertSeqs(t, segs)
	assertAudioInside(t, segs[:len(segs)-1])
	assert.Zero(t, s.Late())
}

// frame returns a frame of any track and config.
func frame(track int, cfg *media.TrackConfig, pts, dts int64, key bool) hub.Delivery {
	return hub.Delivery{Frame: media.Frame{Track: track, Config: cfg, PTS: pts, DTS: dts, Key: key, Epoch: 1}}
}

// bframes returns the n-th frame, in decode order, of 25 fps video in
// closed GOPs of 49 frames (I, then P B B groups): DTS n×3600, PTS its
// display slot + 2 frames, keyframe on each GOP's I.
func bframes(n int64) (pts, dts int64, key bool) {
	j := n % 49
	disp := j
	switch {
	case j == 0:
	case (j-1)%3 == 0:
		disp = j + 2 // P
	default:
		disp = j - 1 // B
	}
	return (n - j + disp + 2) * 3600, n * 3600, j == 0
}

// shifted returns bframes for a GOP phase 20 frames off, on the same
// timeline, so the anchor's cuts fall in the middle of its GOPs.
func shifted(n int64) (pts, dts int64, key bool) {
	pts, dts, key = bframes(n + 20)
	return pts - 20*3600, dts - 20*3600, key
}

// assertDTSOrder checks that every track's DTS strictly increases across
// the segments, in segment order.
func assertDTSOrder(t *testing.T, segs []Segment) {
	t.Helper()
	last := map[int]int64{}
	for _, g := range segs {
		for _, f := range g.Frames {
			if prev, ok := last[f.Track]; ok {
				assert.Greater(t, f.DTS, prev, "segment %d: track %d DTS goes backwards", g.Seq, f.Track)
			}
			last[f.Track] = f.DTS
		}
	}
}

// Ruling R-T1d: only audio is cut at K. A second video track with B-frames
// (decode order P B B) stays in arrival order, so its DTS never goes
// backwards across segments, with audio 200 ms behind; likewise the old
// anchor at a video-to-video anchor change.
func TestVideoTracksKeepDecodeOrder(t *testing.T) {
	t.Run("second-video-track", func(t *testing.T) {
		s := New(Options{})
		var segs []Segment
		var nextA int64
		for n := int64(0); n < 300; n++ {
			pts, dts, key := bframes(n)
			segs = append(segs, s.Push(frame(0, h264, pts, dts, key))...)
			pts2, dts2, key2 := shifted(n)
			segs = append(segs, s.Push(frame(2, h264, pts2, dts2, key2))...)
			for ; nextA <= dts-18000; nextA += aDur {
				segs = append(segs, s.Push(aud(nextA))...)
			}
		}
		segs = append(segs, s.Flush()...)
		require.Greater(t, len(segs), 3)
		assertSeqs(t, segs)
		assertDTSOrder(t, segs)
		assertAudioInside(t, segs[1:len(segs)-1])
	})
	t.Run("video-anchor-change", func(t *testing.T) {
		s := New(Options{})
		var segs []Segment
		for n := int64(0); n < 300; n++ {
			pts2, dts2, key2 := shifted(n)
			segs = append(segs, s.Push(frame(2, h264, pts2, dts2, key2))...)
			pts, dts, key := bframes(n)
			if n >= 98 { // track 0 joins on a keyframe and becomes the anchor
				segs = append(segs, s.Push(frame(0, h264, pts, dts, key))...)
			}
		}
		segs = append(segs, s.Flush()...)
		require.Greater(t, len(segs), 2)
		assertSeqs(t, segs)
		assertDTSOrder(t, segs)
	})
}

// Ruling R-T1d: a hold times out when any track is 1 s past its cut. Video
// stalls after 2 s, audio track 1 keeps flowing and audio track 3 stalls
// with the video: the forced cuts on track 1 still come out, and at most
// two segments are held at any time.
func TestHoldBoundedWhenAnchorStalls(t *testing.T) {
	aac3 := &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 48000, Channels: 2}
	s := New(Options{})
	var segs []Segment
	maxHeld := 0
	for a := int64(0); a < 120*90000; a += aDur {
		if a < 180000 && a%3600 < aDur {
			segs = append(segs, s.Push(vid(1, a-a%3600, a%90000 < aDur))...)
		}
		segs = append(segs, s.Push(aud(a))...)
		if a < 180000 {
			segs = append(segs, s.Push(frame(3, aac3, a, a, true))...)
		}
		maxHeld = max(maxHeld, len(s.held))
	}
	assert.LessOrEqual(t, maxHeld, 2)
	assert.Greater(t, len(segs), 10, "forced cuts keep coming out")
	assertSeqs(t, segs)
}

// M1: after a cut moves audio, the closed segment's End comes from the
// frames that stay (the accumulators are rebuilt).
func TestAudioCutEndAfterMove(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	var nextA int64
	for v := int64(0); v <= 180000; v += 3600 {
		for ; nextA <= v+18000; nextA += aDur {
			segs = append(segs, s.Push(aud(nextA))...)
		}
		segs = append(segs, s.Push(vid(1, v, v%180000 == 0))...)
	}
	require.Len(t, segs, 1)
	g := segs[0]
	var want int64
	for _, f := range g.Frames {
		d := int64(3600)
		if f.Kind() == media.KindAudio {
			d = aDur
		}
		want = max(want, f.PTS+d)
	}
	assert.Equal(t, want, g.End)
	assert.Less(t, g.End, g.Start+g.Duration+aDur)
}

// M1: an audio track that made a hold time out is waited for again once it
// delivers: audio stops at 4.5 s and resumes 100 ms behind video at 7.5 s,
// so the cut at 8 s is held until the audio reaches it.
func TestAudioTimedOutTrackRejoins(t *testing.T) {
	s := New(Options{})
	emitted := map[uint64]int64{}
	var nextA, lastV int64
	for i := 0; i <= 250; i++ {
		lastV = int64(i) * 3600
		for _, g := range s.Push(vid(1, lastV, lastV%180000 == 0)) {
			emitted[g.Seq] = lastV
		}
		for ; nextA <= lastV-9000; nextA += aDur {
			if nextA >= 405000 && nextA < 675000 {
				continue
			}
			for _, g := range s.Push(aud(nextA)) {
				emitted[g.Seq] = lastV
			}
		}
	}
	assert.EqualValues(t, 630000, emitted[2], "the cut at 6 s times out 1 s later")
	assert.Greater(t, emitted[3], int64(720000), "the cut at 8 s waits for the returned audio")
}

// M1: at an anchor change whose new anchor frame is not a keyframe, the
// audio presented at or after the cut goes to the carry and reaches the
// next segment.
func TestPromoteCutCarriesMovedAudio(t *testing.T) {
	s := New(Options{})
	var segs []Segment
	for a := int64(0); a <= 300*aDur; a += aDur { // an audio-only stretch
		segs = append(segs, s.Push(aud(a))...)
	}
	require.NotEmpty(t, segs)
	k := int64(300*aDur) - 18000
	segs = append(segs, s.Push(vid(1, k, false))...)     // the anchor changes on a non-key frame
	segs = append(segs, s.Push(vid(1, k+3600, true))...) // then the keyframe
	segs = append(segs, s.Flush()...)
	last := segs[len(segs)-1]
	assert.EqualValues(t, k+3600, last.Start)
	var carried int
	for _, f := range last.Frames {
		if f.Kind() == media.KindAudio {
			carried++
			assert.GreaterOrEqual(t, f.PTS, k+3600)
		}
	}
	assert.Equal(t, int((int64(300*aDur)-(k+3600))/aDur)+1, carried, "audio at or after the keyframe is carried")
	for _, g := range segs[:len(segs)-1] {
		for _, f := range g.Frames {
			assert.Less(t, f.PTS, k, "segment %d keeps no audio at or after the cut", g.Seq)
		}
	}
}
