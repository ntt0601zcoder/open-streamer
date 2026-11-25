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

// arrival is a delivery with its arrival time, for ordering.
type arrival struct {
	at int
	d  hub.Delivery
}

// arrive returns the deliveries sorted by arrival time; ties keep their
// order in as.
func arrive(as []arrival) []hub.Delivery {
	slices.SortStableFunc(as, func(a, b arrival) int { return cmp.Compare(a.at, b.at) })
	out := make([]hub.Delivery, len(as))
	for i, a := range as {
		out[i] = a.d
	}
	return out
}

// ladderDeliveries returns n frames of each track in tracks (ascending) from
// start, epoch 1: 25 fps H.264 with a keyframe every gopFrames frames on
// tracks 0..2, so every rung keys at the same PTS, and AAC frames over the
// same span on track 3. order gives frame i of a track its arrival time;
// ties go by track, then by i; a negative time leaves the frame out.
func ladderDeliveries(tracks []int, start int64, n, gopFrames int, order func(track, i int) int) []hub.Delivery {
	var as []arrival
	for _, tr := range tracks {
		count, cfg, step := n, h264, int64(3600)
		if tr == 3 {
			count, cfg, step = int(int64(n)*3600/aDur), aac, aDur
		}
		for i := 0; i < count; i++ {
			at := order(tr, i)
			if at < 0 {
				continue
			}
			pts := start + int64(i)*step
			as = append(as, arrival{at, frame(tr, cfg, pts, pts, tr == 3 || i%gopFrames == 0)})
		}
	}
	return arrive(as)
}

// ladder pushes ladderDeliveries from PTS 0 and returns every segment,
// Flush included.
func ladder(s *Segmenter, tracks []int, n, gopFrames int, order func(track, i int) int) []Segment {
	return pushDeliveries(s, ladderDeliveries(tracks, 0, n, gopFrames, order))
}

// inDTS is the order the keyframe barrier delivers ladder frames in (ruling
// R-B3): by DTS, so every pre-K frame of every track comes before the
// keyframes at K, which come in track order.
func inDTS(track, i int) int {
	if track == 3 {
		return i * aDur
	}
	return i * 3600
}

// pushEach pushes ds and returns the segments they completed.
func pushEach(s *Segmenter, ds []hub.Delivery) []Segment {
	segs := make([]Segment, 0, 8)
	for _, d := range ds {
		segs = append(segs, s.Push(d)...)
	}
	return segs
}

// pushDeliveries pushes ds and returns every segment, Flush included.
func pushDeliveries(s *Segmenter, ds []hub.Delivery) []Segment {
	return append(pushEach(s, ds), s.Flush()...)
}

// at returns the index of track tr's delivery with PTS pts.
func at(t *testing.T, ds []hub.Delivery, tr int, pts int64) int {
	t.Helper()
	i := slices.IndexFunc(ds, func(d hub.Delivery) bool { return d.Frame.Track == tr && d.Frame.PTS == pts })
	require.GreaterOrEqual(t, i, 0, "no frame of track %d at %d", tr, pts)
	return i
}

// perTrack counts a segment's frames by track.
func perTrack(g Segment) map[int]int {
	n := map[int]int{}
	for _, f := range g.Frames {
		n[f.Track]++
	}
	return n
}

// assertRungsAligned checks ruling R-B8 on the segments that are not Forced:
// every video track with frames in a segment starts there with a keyframe
// at the segment's Start.
func assertRungsAligned(t *testing.T, segs []Segment) {
	t.Helper()
	for _, g := range segs {
		if g.Forced {
			continue
		}
		seen := map[int]bool{}
		for _, f := range g.Frames {
			if f.Kind() != media.KindVideo || seen[f.Track] {
				continue
			}
			seen[f.Track] = true
			assert.True(t, f.Key && f.PTS == g.Start,
				"segment %d (Start %d): track %d starts at %d, key %v", g.Seq, g.Start, f.Track, f.PTS, f.Key)
		}
	}
}

// assertStartsRise checks that every segment starts after its predecessor.
func assertStartsRise(t *testing.T, segs []Segment) {
	t.Helper()
	for i := 1; i < len(segs); i++ {
		assert.Greater(t, segs[i].Start, segs[i-1].Start, "segment %d starts at or before segment %d", segs[i].Seq, segs[i-1].Seq)
	}
}

// pushRung pushes track tr's 25 fps frames with PTS in [from, to), with a
// keyframe every second on the shared timeline.
func pushRung(s *Segmenter, tr int, from, to int64) []Segment {
	var segs []Segment
	for pts := from; pts < to; pts += 3600 {
		segs = append(segs, s.Push(frame(tr, h264, pts, pts, pts%90000 == 0))...)
	}
	return segs
}

// pushRungs pushes tracks 0 and 2 interleaved in DTS order, track 0 first,
// as pushRung does for one track.
func pushRungs(s *Segmenter, from, to int64) []Segment {
	var segs []Segment
	for pts := from; pts < to; pts += 3600 {
		segs = append(segs, pushRung(s, 0, pts, pts+1)...)
		segs = append(segs, pushRung(s, 2, pts, pts+1)...)
	}
	return segs
}

// Ruling R-B8: frames in the barrier's order give segments where every rung
// starts on its keyframe at the segment's Start, as without AlignVideo.
func TestAlignVideoBarrierOrder(t *testing.T) {
	tracks := []int{0, 1, 2}
	s := New(Options{AlignVideo: true})
	segs := ladder(s, tracks, 250, 25, inDTS)
	require.Len(t, segs, 5)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	for _, g := range segs {
		assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(g), "segment %d", g.Seq)
	}
	assert.Zero(t, s.Dropped())
	assert.Equal(t, ladder(New(Options{}), tracks, 250, 25, inDTS), segs)
}

// Ruling R-B8 (2): track 2's last two frames before K = 360000 arrive after
// IDR_0(K): they are dropped and track 2 enters the segment at its IDR.
func TestAlignVideoLateTrackAtOneCut(t *testing.T) {
	tracks := []int{0, 1, 2}
	order := func(tr, i int) int {
		if tr == 2 && (i == 98 || i == 99) {
			return 100 * 3600
		}
		return inDTS(tr, i)
	}
	s := New(Options{AlignVideo: true})
	segs := ladder(s, tracks, 250, 25, order)
	off := ladder(New(Options{}), tracks, 250, 25, order)
	require.Len(t, segs, 5)
	require.Len(t, off, 5)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	assert.EqualValues(t, 2, s.Dropped())
	assert.Equal(t, map[int]uint64{2: 2}, s.LeftOut())
	assert.Nil(t, New(Options{}).LeftOut())
	for i := range segs {
		if i == 2 {
			continue
		}
		assert.Equal(t, off[i], segs[i], "segment %d", i)
	}
	g := segs[2]
	require.EqualValues(t, 360000, g.Start)
	want := slices.DeleteFunc(slices.Clone(off[2].Frames), func(f media.Frame) bool {
		return f.Track == 2 && f.PTS < 360000
	})
	assert.Equal(t, want, g.Frames)
	assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(g))
}

// Ruling R-B8 (1): track 2 alone, then track 0 with frames from an older K.
// Track 0 takes the anchor only at a keyframe at or above the open
// segment's Start (strictly above the newest closed one's when a Lost has
// closed it), so no segment starts at or before its predecessor.
func TestAlignVideoLowerTrackAfterClose(t *testing.T) {
	t.Run("at-start", func(t *testing.T) {
		s := New(Options{AlignVideo: true})
		segs := pushRung(s, 2, 0, 363600) // seg 2 opens on IDR_2(360000)
		require.Len(t, segs, 2)
		segs = append(segs, pushRung(s, 0, 270000, 360000)...)
		assert.EqualValues(t, 25, s.Dropped(), "track 0 below the open segment's Start")
		assert.Equal(t, 2, s.anchor)
		segs = append(segs, pushRung(s, 0, 360000, 363600)...) // IDR_0 at Start: seg 2 reopens
		assert.Equal(t, 0, s.anchor)
		segs = append(segs, pushRungs(s, 363600, 900000)...)
		segs = append(segs, s.Flush()...)
		assertSeqs(t, segs)
		assertStartsRise(t, segs)
		require.GreaterOrEqual(t, len(segs), 3)
		assert.EqualValues(t, 360000, segs[2].Start)
		assertRungsAligned(t, segs[2:])
		assert.Equal(t, 2, segs[2].Frames[0].Track, "the old anchor's frames come back from the carry")
		assert.EqualValues(t, 50, perTrack(segs[2])[2])
	})
	t.Run("above-start", func(t *testing.T) {
		s := New(Options{AlignVideo: true})
		segs := pushRung(s, 2, 0, 363600)
		require.Len(t, segs, 2)
		segs = append(segs, pushRungs(s, 363600, 720000)...)
		segs = append(segs, s.Flush()...)
		assert.EqualValues(t, 24, s.Dropped(), "track 0 before its keyframe at 450000")
		assertSeqs(t, segs)
		assertStartsRise(t, segs)
		require.Len(t, segs, 5)
		assert.EqualValues(t, 360000, segs[2].Start)
		assert.EqualValues(t, 90000, segs[2].Duration)
		assert.Equal(t, map[int]int{2: 25}, perTrack(segs[2]))
		assert.EqualValues(t, 450000, segs[3].Start)
		assert.Equal(t, map[int]int{0: 50, 2: 50}, perTrack(segs[3]))
		assertRungsAligned(t, segs)
	})
	t.Run("after-lost", func(t *testing.T) {
		s := New(Options{AlignVideo: true})
		segs := pushRung(s, 2, 0, 399600)
		require.Len(t, segs, 2)
		d := frame(0, h264, 270000, 270000, true)
		d.Lost = 5
		segs = append(segs, s.Push(d)...)
		require.Len(t, segs, 3, "the Lost closes seg 2")
		segs = append(segs, pushRung(s, 0, 273600, 450000)...)
		assert.EqualValues(t, 50, s.Dropped(), "IDR_0(360000) is not above the closed segment's Start")
		segs = append(segs, pushRungs(s, 450000, 810000)...)
		segs = append(segs, s.Flush()...)
		assert.Equal(t, 0, s.anchor)
		assertSeqs(t, segs)
		assertStartsRise(t, segs)
		require.Len(t, segs, 5)
		assert.EqualValues(t, 450000, segs[3].Start)
		assert.True(t, segs[3].Discontinuity)
		assert.Equal(t, map[int]int{0: 50, 2: 50}, perTrack(segs[3]))
		assertRungsAligned(t, segs)
	})
}

// Ruling R-B8: a leading audio anchor keeps today's behaviour; the first
// video frame takes over, above or below the audio's first PTS.
func TestAlignVideoLeadingAudioUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    int64
	}{{"above", 36000}, {"below", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			var as []arrival
			for i := int64(0); i < 450; i++ { // audio from 18000, 0.6 s ahead
				pts := 18000 + i*aDur
				as = append(as, arrival{int(pts - 54000), frame(3, aac, pts, pts, true)})
			}
			for _, tr := range []int{0, 1, 2} {
				for i := int64(0); i < 200; i++ {
					pts := tc.k + i*3600
					as = append(as, arrival{int(pts), frame(tr, h264, pts, pts, i%25 == 0)})
				}
			}
			ds := arrive(as)
			require.Equal(t, 3, ds[0].Frame.Track, "audio leads")
			on := New(Options{AlignVideo: true})
			off := New(Options{})
			segs := pushDeliveries(on, ds)
			require.NotEmpty(t, segs)
			assertSeqs(t, segs)
			assert.Equal(t, pushDeliveries(off, ds), segs)
			assert.Equal(t, off.Dropped(), on.Dropped())
			assert.Equal(t, off.Late(), on.Late())
		})
	}
}

// Ruling R-B8 (1): a new epoch whose first video frame is IDR_2(K), then
// IDR_0(K) and IDR_1(K): the segment opened by track 2 is taken back and
// reopened by track 0 with all three rungs, without a Seq hole or a
// one-frame segment.
func TestAlignVideoLeadingRungTakenBack(t *testing.T) {
	tracks := []int{0, 1, 2}
	s := New(Options{AlignVideo: true})
	segs := pushEach(s, ladderDeliveries(tracks, 0, 100, 25, inDTS))
	const k = 10_000_000
	ds := ladderDeliveries(tracks, k, 100, 25, func(tr, i int) int {
		if tr == 2 && i == 0 {
			return 0
		}
		return inDTS(tr, i) + 1
	})
	for i := range ds {
		ds[i].Frame.Epoch = 2
	}
	require.Equal(t, 2, ds[0].Frame.Track)
	segs = append(segs, pushEach(s, ds)...)
	assert.Equal(t, 0, s.anchor)
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 4)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	g := segs[2]
	assert.EqualValues(t, 2, g.Epoch)
	assert.True(t, g.Discontinuity)
	assert.EqualValues(t, k, g.Start)
	assert.EqualValues(t, 180000, g.Duration)
	assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(g))
	assert.EqualValues(t, k+180000, segs[3].Start)
	for _, g := range segs {
		assert.Greater(t, len(g.Frames), 1, "segment %d", g.Seq)
	}
	assert.Zero(t, s.Dropped())
}

// Ruling R-B8: a Lost delivery on IDR_2(K), the hub resuming track 2 first:
// nothing opens until track 0's next keyframe K2, and the segment at K2
// holds every rung from its keyframe at K2.
func TestAlignVideoResumeOnHigherTrack(t *testing.T) {
	const k, k2 = 540000, 630000
	ds := ladderDeliveries([]int{0, 1, 2}, 0, 300, 25, func(tr, i int) int {
		if (i >= 140 && i < 150) || (tr != 2 && i >= 150 && i < 175) {
			return -1
		}
		return inDTS(tr, i)
	})
	i := at(t, ds, 2, k)
	require.Equal(t, 2, ds[i-1].Frame.Track, "IDR_2(K) is the first frame after the gap")
	ds[i].Lost = 30
	s := New(Options{AlignVideo: true})
	segs := pushDeliveries(s, ds)
	assertSeqs(t, segs)
	assertStartsRise(t, segs)
	assertRungsAligned(t, segs)
	require.Len(t, segs, 6)
	assert.EqualValues(t, 360000, segs[2].Start)
	g := segs[3]
	assert.EqualValues(t, k2, g.Start)
	assert.True(t, g.Discontinuity)
	assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(g))
	assert.EqualValues(t, 25, s.Dropped(), "track 2 from K to K2")
}

// Ruling R-B8 (2): after a Lost, track 1 resumes at K + GOP, inside the
// segment opened at K: it is left out of that segment and is in the next.
func TestAlignVideoTrackResumesLate(t *testing.T) {
	const k = 540000
	ds := ladderDeliveries([]int{0, 1, 2}, 0, 300, 25, func(tr, i int) int {
		if (i >= 140 && i < 150) || (tr == 1 && i >= 150 && i < 175) {
			return -1
		}
		return inDTS(tr, i)
	})
	ds[at(t, ds, 0, k)].Lost = 30
	s := New(Options{AlignVideo: true})
	segs := pushDeliveries(s, ds)
	off := pushDeliveries(New(Options{}), ds)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	require.Len(t, segs, 6)
	g := segs[3]
	assert.EqualValues(t, k, g.Start)
	assert.True(t, g.Discontinuity)
	assert.Equal(t, map[int]int{0: 50, 2: 50}, perTrack(g))
	assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(segs[4]))
	assert.EqualValues(t, 25, s.Dropped())
	assert.Equal(t, 25, perTrack(off[3])[1], "without AlignVideo the part starts mid-segment")
}

// Ruling R-B8 (2): track 1 has no keyframe at K = 180000 (its next is at
// K + GOP): the segment at K has no track-1 frame; the next one has.
func TestAlignVideoMissingKeyframe(t *testing.T) {
	ds := ladderDeliveries([]int{0, 1, 2}, 0, 250, 25, inDTS)
	ds[at(t, ds, 1, 180000)].Frame.Key = false
	s := New(Options{AlignVideo: true})
	segs := pushDeliveries(s, ds)
	require.Len(t, segs, 5)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	assert.Equal(t, map[int]int{0: 50, 2: 50}, perTrack(segs[1]))
	assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(segs[2]))
	assert.EqualValues(t, 50, s.Dropped())
}

// Ruling R-B8 (2) does not apply to Forced segments: with GOP 20 s and
// MaxDuration 10 s they keep every rung's frames, as without AlignVideo.
func TestAlignVideoForcedSegmentKeepsAll(t *testing.T) {
	tracks := []int{0, 1, 2}
	s := New(Options{AlignVideo: true, MaxDuration: 900000})
	segs := ladder(s, tracks, 750, 500, inDTS)
	require.Len(t, segs, 3)
	assertSeqs(t, segs)
	require.True(t, segs[1].Forced)
	assert.Equal(t, map[int]int{0: 250, 1: 250, 2: 250}, perTrack(segs[1]))
	assert.Equal(t, ladder(New(Options{MaxDuration: 900000}), tracks, 750, 500, inDTS), segs)
	assert.Zero(t, s.Dropped())
}

// AlignVideo leaves audio alone: with AAC track 3 0.5 s behind the rungs,
// audio is placed as without it.
func TestAlignVideoAudioUntouched(t *testing.T) {
	tracks := []int{0, 1, 2, 3}
	order := func(tr, i int) int {
		if tr == 3 {
			return i*aDur + 45000
		}
		return inDTS(tr, i)
	}
	on := New(Options{AlignVideo: true})
	off := New(Options{})
	segs := ladder(on, tracks, 250, 25, order)
	require.Len(t, segs, 5)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	assertAudioInside(t, segs[:len(segs)-1])
	assert.Equal(t, ladder(off, tracks, 250, 25, order), segs)
	assert.Equal(t, off.Late(), on.Late())
	assert.Zero(t, on.Dropped())
}

// A respawn under AlignVideo: rung 0, the anchor, stops right after its
// keyframe at K = 180000 while rungs 1 and 2 go on for 10 frames, until an
// epoch change closes the segment without a cut. Duration is the anchor's
// one frame; VideoDurations gives rungs 1 and 2 their own spans. Segments
// cut at a keyframe, and those whose rungs end together, have none.
func TestAlignVideoUncutRungSpans(t *testing.T) {
	const k = 180000
	ds := ladderDeliveries([]int{0, 1, 2}, 0, 60, 25, func(tr, i int) int {
		if tr == 0 && i > 50 {
			return -1
		}
		return inDTS(tr, i)
	})
	ds = append(ds, hub.Delivery{Frame: media.Frame{Track: 0, Config: h264, PTS: 300000, DTS: 300000, Key: true, Epoch: 2}})
	s := New(Options{AlignVideo: true})
	segs := pushDeliveries(s, ds)
	require.Len(t, segs, 3)
	assertSeqs(t, segs)
	assert.Nil(t, segs[0].VideoDurations, "cut at K")
	g := segs[1]
	assert.EqualValues(t, k, g.Start)
	assert.EqualValues(t, 3600, g.Duration)
	assert.Equal(t, map[int]int64{1: 36000, 2: 36000}, g.VideoDurations)
	assert.Nil(t, segs[2].VideoDurations, "one rung")

	off := pushDeliveries(New(Options{}), ds)
	require.Len(t, off, 3)
	assert.EqualValues(t, 3600, off[1].Duration)
	assert.Nil(t, off[1].VideoDurations, "without AlignVideo")
}

// Finding I1 (ruling R-B8, third rule): the anchor rung arrives 15 frames
// after the others, so at every cut K rungs 1 and 2 have delivered their
// IDR at K and 14 frames more. Those move to the next segment: every rung's
// part is 50 frames from its keyframe at Start, and nothing is dropped.
// Without AlignVideo, video does not move.
func TestAlignVideoLeadingRungMovedAtCut(t *testing.T) {
	tracks := []int{0, 1, 2, 3}
	order := func(tr, i int) int {
		if tr == 0 {
			return (i + 15) * 3600
		}
		return inDTS(tr, i)
	}
	s := New(Options{AlignVideo: true})
	segs := ladder(s, tracks, 250, 25, order)
	require.Len(t, segs, 5)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	assertStartsRise(t, segs)
	assertAudioInside(t, segs[:4])
	for _, g := range segs {
		n := perTrack(g)
		delete(n, 3)
		assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, n, "segment %d", g.Seq)
		assert.EqualValues(t, 180000, g.Duration, "segment %d", g.Seq)
		assert.Nil(t, g.VideoDurations, "segment %d", g.Seq)
		for _, f := range g.Frames {
			if f.Kind() == media.KindVideo {
				assert.True(t, f.PTS >= g.Start && f.PTS < g.Start+180000, "segment %d: track %d frame at %d", g.Seq, f.Track, f.PTS)
			}
		}
	}
	assert.Zero(t, s.Dropped())
	assertDTSOrder(t, segs)

	off := ladder(New(Options{}), tracks, 250, 25, order)
	require.NotEmpty(t, off)
	straddle := false
	for _, g := range off[:len(off)-1] {
		for _, f := range g.Frames {
			if f.Track == 1 && f.PTS >= g.Start+g.Duration {
				straddle = true
			}
		}
	}
	assert.True(t, straddle, "without AlignVideo the leading rung's frames stay where they arrived")
}

// A rung that leads the anchor by more than a segment: from frame 50 on,
// rung 0 arrives 60 frames (2.4 s) late. Every cut moves the leading rungs'
// frames from their IDR at K on, the IDR at the next K among them, and the
// next cut moves that one again.
func TestAlignVideoLeadingRungBeyondSegment(t *testing.T) {
	tracks := []int{0, 1, 2}
	order := func(tr, i int) int {
		if tr == 0 && i >= 50 {
			return (i + 60) * 3600
		}
		return inDTS(tr, i)
	}
	s := New(Options{AlignVideo: true})
	segs := ladder(s, tracks, 300, 25, order)
	require.Len(t, segs, 6)
	assertSeqs(t, segs)
	assertRungsAligned(t, segs)
	for _, g := range segs {
		assert.Equal(t, map[int]int{0: 50, 1: 50, 2: 50}, perTrack(g), "segment %d", g.Seq)
	}
	assert.Zero(t, s.Dropped())
}

// The third rule on a cut made by a lower track taking the anchor: rung 2
// runs alone for 35 frames, past its keyframe at frame 25 (K = 90000, short
// of the target, so no cut), then rung 0 arrives from its IDR at K and cuts
// the segment there. Rung 2's frames from its IDR at K on move into the
// segment rung 0 opens.
func TestAlignVideoLeadingRungAtPromoteCut(t *testing.T) {
	s := New(Options{AlignVideo: true})
	var ds []hub.Delivery
	for i := range 35 {
		ds = append(ds, frame(2, h264, int64(i)*3600, int64(i)*3600, i%25 == 0))
	}
	for i := 25; i < 75; i++ {
		ds = append(ds, frame(0, h264, int64(i)*3600, int64(i)*3600, i%25 == 0))
		if i >= 35 {
			ds = append(ds, frame(2, h264, int64(i)*3600, int64(i)*3600, i%25 == 0))
		}
	}
	segs := pushDeliveries(s, ds)
	require.Len(t, segs, 2)
	assertRungsAligned(t, segs)
	assert.Equal(t, map[int]int{2: 25}, perTrack(segs[0]))
	assert.Equal(t, map[int]int{0: 50, 2: 50}, perTrack(segs[1]))
	assert.EqualValues(t, 90000, segs[1].Start)
	assert.Zero(t, s.Dropped())
}

// An open GOP on a leading rung: the pictures after its IDR at K that are
// presented before K are dropped when the rung moves. A leading rung with
// no keyframe at K is not moved.
func TestAlignVideoLeadingRungEdges(t *testing.T) {
	const k = 180000
	feed := func(key1 bool) []hub.Delivery {
		ds := make([]hub.Delivery, 0, 105)
		for i := range 50 {
			ds = append(ds, frame(0, h264, int64(i)*3600, int64(i)*3600, i == 0), frame(1, h264, int64(i)*3600, int64(i)*3600, i == 0))
		}
		// Rung 1 leads: its frame at K, a leading picture below K, one above.
		ds = append(ds,
			frame(1, h264, k, k, key1),
			frame(1, h264, k-1800, k+1800, false),
			frame(1, h264, k+3600, k+3600, false),
			frame(0, h264, k, k, true),
			frame(0, h264, k+3600, k+3600, false),
		)
		return ds
	}
	s := New(Options{AlignVideo: true})
	segs := pushDeliveries(s, feed(true))
	require.Len(t, segs, 2)
	assert.Equal(t, map[int]int{0: 50, 1: 50}, perTrack(segs[0]))
	assert.Equal(t, map[int]int{0: 2, 1: 2}, perTrack(segs[1]))
	assertRungsAligned(t, segs)
	assert.EqualValues(t, 1, s.Dropped())
	assert.Equal(t, map[int]uint64{1: 1}, s.LeftOut())

	s = New(Options{AlignVideo: true})
	segs = pushDeliveries(s, feed(false))
	require.Len(t, segs, 2)
	assert.Equal(t, map[int]int{0: 50, 1: 53}, perTrack(segs[0]))
	assert.Equal(t, map[int]int{0: 2}, perTrack(segs[1]))
	assert.Zero(t, s.Dropped())
}
