package segment

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestSegmentFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	s := New(Options{})
	var segs []Segment
	feed := func(fs []media.Frame) {
		for _, f := range fs {
			segs = append(segs, s.Push(hub.Delivery{Frame: f})...)
		}
	}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		feed(c.Push(f))
	}
	feed(c.Flush())
	segs = append(segs, s.Flush()...)
	require.Len(t, segs, 3)
	for i, g := range segs {
		assert.EqualValues(t, i, g.Seq)
		assert.Positive(t, g.Duration)
	}
}
