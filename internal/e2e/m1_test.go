package e2e

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
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// timed returns the frames of a fixture after demux and clock.
func timed(t *testing.T, name string) []media.Frame {
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

// delivered runs a fixture through demux, clock and a hub with one subscriber
// whose queue holds the whole clip.
func delivered(t *testing.T, name string) []media.Frame {
	t.Helper()
	frames := timed(t, name)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("e2e", len(frames)+1)
	for _, f := range frames {
		h.Write(f)
	}
	h.Close()
	var got []media.Frame
	for dl := range sub.C() {
		got = append(got, dl.Frame)
	}
	return got
}

func epochs(frames []media.Frame) []uint32 {
	var ids []uint32
	for _, f := range frames {
		if len(ids) == 0 || ids[len(ids)-1] != f.Epoch {
			ids = append(ids, f.Epoch)
		}
	}
	return ids
}

// epochTrack names one track within one epoch.
type epochTrack struct {
	epoch uint32
	track int
}

// counts returns the number of frames per epoch and track.
func counts(frames []media.Frame) map[epochTrack]int {
	out := map[epochTrack]int{}
	for _, f := range frames {
		out[epochTrack{f.Epoch, f.Track}]++
	}
	return out
}

// TestM1Fixtures checks every fixture and the exact number of frames each
// track delivers in each epoch, so a missing track or a lost GOP fails.
//
// Where the counts fall short of the demuxed frames (track 0 is video,
// track 1 audio):
//   - One audio frame per clip start is presented before the first video
//     keyframe and dropped by the Clock (rule 5).
//   - The loop and then_640 fixtures demux one video frame fewer from the
//     first clip: the continuity counter jumps at the concatenation point,
//     and go-astits then discards the PES it was still assembling, the
//     first clip's last video frame (M2 counts this loss).
//   - midgop drops audio before the first keyframe.
func TestM1Fixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		want map[epochTrack]int
	}{
		{"h264_aac.ts", map[epochTrack]int{{1, 0}: 150, {1, 1}: 282}},
		{"h265_aac51.ts", map[epochTrack]int{{1, 0}: 150, {1, 1}: 259}},
		{"h264_aacmono_wrap.ts", map[epochTrack]int{{1, 0}: 150, {1, 1}: 259}},
		{"h264_aac_loop.ts", map[epochTrack]int{{1, 0}: 149, {1, 1}: 282, {2, 0}: 150, {2, 1}: 282}},
		{"h264_aac_then_640.ts", map[epochTrack]int{{1, 0}: 149, {1, 1}: 282, {2, 0}: 50}},
		{"h264_aac_midgop.ts", map[epochTrack]int{{1, 0}: 75, {1, 1}: 141}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := delivered(t, tc.name)
			require.NotEmpty(t, got)
			assert.Nil(t, conform.CheckFrames(got, conform.FrameOptions{}))
			assert.Equal(t, tc.want, counts(got))
		})
	}
}

func TestM1LoopEpochs(t *testing.T) {
	got := delivered(t, "h264_aac_loop.ts")
	assert.Equal(t, []uint32{1, 2}, epochs(got))
	var last int64
	for _, f := range got {
		last = max(last, f.DTS)
	}
	assert.Less(t, last, int64(13*media.ClockRate))
}

func TestM1ConfigChangeEpochs(t *testing.T) {
	got := delivered(t, "h264_aac_then_640.ts")
	assert.Equal(t, []uint32{1, 2}, epochs(got))
}

func TestM1WrapStartsAtZero(t *testing.T) {
	got := delivered(t, "h264_aacmono_wrap.ts")
	require.NotEmpty(t, got)
	first, last := int64(1<<62), int64(0)
	for _, f := range got {
		first = min(first, f.DTS)
		last = max(last, f.DTS)
	}
	assert.Zero(t, first)
	assert.Less(t, last, int64(7*media.ClockRate))
}

func TestM1SlowConsumer(t *testing.T) {
	frames := timed(t, "h264_aac_loop.ts")
	h := hub.New(hub.Options{})
	sub := h.Subscribe("slow", 8)
	var got []media.Frame
	lost := 0
	take := func(dl hub.Delivery) {
		got = append(got, dl.Frame)
		if dl.Lost > 0 {
			lost++
			assert.True(t, dl.Frame.Key && dl.Frame.Kind() == media.KindVideo,
				"delivery after loss must be a video keyframe")
		}
	}
	for i, f := range frames {
		h.Write(f)
		if (i+1)%40 == 0 {
			for range 8 {
				select {
				case dl := <-sub.C():
					take(dl)
				default:
				}
			}
		}
	}
	h.Close()
	for dl := range sub.C() {
		take(dl)
	}
	assert.Positive(t, lost)
	assert.Nil(t, conform.CheckFrames(got, conform.FrameOptions{Skip: []string{conform.RuleStartOffset}}))
}
