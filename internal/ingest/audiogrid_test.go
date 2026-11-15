package ingest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gridAt is the grid time of frame k after anchor at rate.
func gridAt(anchor, k int64, rate int) int64 {
	return anchor + k*1024*90000/int64(rate)
}

func TestAudioGridBands(t *testing.T) {
	const rate = 48000 // one frame is exactly 1920 ticks
	var g audioGrid

	pts, keep, re := g.place(1000, rate)
	assert.Equal(t, int64(1000), pts)
	assert.True(t, keep)
	assert.False(t, re, "the first frame anchors, it does not re-anchor")

	// |dev| <= d stays on the grid, at both edges.
	pts, keep, re = g.place(1000+1920+1920, rate)
	assert.Equal(t, []any{int64(2920), true, false}, []any{pts, keep, re})
	pts, keep, re = g.place(1000+2*1920-1920, rate)
	assert.Equal(t, []any{int64(4840), true, false}, []any{pts, keep, re})

	// −1 s < dev < −d drops the frame and does not advance k.
	_, keep, re = g.place(1000+3*1920-1921, rate)
	assert.False(t, keep)
	assert.False(t, re)
	pts, keep, _ = g.place(1000+3*1920, rate)
	assert.True(t, keep)
	assert.Equal(t, int64(6760), pts, "the dropped frame did not take a grid slot")

	// dev > d re-anchors forward at the RTMP time.
	pts, keep, re = g.place(1000+4*1920+1921, rate)
	assert.Equal(t, []any{int64(1000 + 4*1920 + 1921), true, true}, []any{pts, keep, re})
	next := int64(1000 + 4*1920 + 1921 + 1920)
	pts, _, re = g.place(next, rate)
	assert.Equal(t, next, pts)
	assert.False(t, re)

	// dev <= −1 s re-anchors backward (an encoder reset).
	back := next - 90000
	pts, keep, re = g.place(back, rate)
	assert.Equal(t, []any{back, true, true}, []any{pts, keep, re})

	// A rate change continues from the end of the old grid.
	var h audioGrid
	h.place(0, rate)
	h.place(1920, rate) // k = 2, the grid ends at 3840
	pts, keep, re = h.place(3840, 44100)
	assert.Equal(t, []any{int64(3840), true, false}, []any{pts, keep, re})
	pts, _, _ = h.place(3840+2090, 44100)
	assert.Equal(t, int64(3840+1024*90000/44100), pts)
}

// runGrid places n frames whose RTMP times come from msAt and returns the
// placed PTS, the drop and re-anchor counts, after checking that the output
// never goes backward.
func runGrid(t *testing.T, rate int, n int64, msAt func(k int64) int64) (out []int64, drops, reanchors int) {
	t.Helper()
	var g audioGrid
	for k := range n {
		pts, keep, re := g.place(msAt(k)*90, rate)
		if re {
			reanchors++
		}
		if !keep {
			drops++
			continue
		}
		if len(out) > 0 {
			require.Greater(t, pts, out[len(out)-1], "frame %d went backward", k)
		}
		out = append(out, pts)
	}
	return out, drops, reanchors
}

func TestAudioGridDrift(t *testing.T) {
	for _, rate := range []int{48000, 44100} {
		perHour := int64(3600) * int64(rate) / 1024

		t.Run(fmt.Sprintf("%d jitter", rate), func(t *testing.T) {
			const base = int64(5_000_000) // ms
			jit := []int64{0, 1, -1, 1, 0, -1}
			msAt := func(k int64) int64 {
				return base + k*1024*1000/int64(rate) + jit[k%int64(len(jit))]
			}
			out, drops, re := runGrid(t, rate, 2000, msAt)
			assert.Zero(t, drops)
			assert.Zero(t, re)
			for k, pts := range out {
				require.Equal(t, gridAt(base*90, int64(k), rate), pts, "frame %d off the grid", k)
			}
		})

		// d is one frame: 100 ppm over an hour is 360 ms, about 16.9 frames
		// at 48 kHz and 15.5 at 44.1 kHz, and each correction removes one d.
		want := float64(360*int64(rate)) / (1024 * 1000)

		t.Run(fmt.Sprintf("%d plus 100 ppm", rate), func(t *testing.T) {
			msAt := func(k int64) int64 { return k * 1024 * 1000 * 10001 / (int64(rate) * 10000) }
			out, drops, re := runGrid(t, rate, perHour, msAt)
			assert.Zero(t, drops)
			assert.InDelta(t, want, re, 1.5)
			assert.Len(t, out, int(perHour))
		})

		t.Run(fmt.Sprintf("%d minus 100 ppm", rate), func(t *testing.T) {
			msAt := func(k int64) int64 { return k * 1024 * 1000 * 9999 / (int64(rate) * 10000) }
			out, drops, re := runGrid(t, rate, perHour, msAt)
			assert.Zero(t, re)
			assert.InDelta(t, want, drops, 1.5)
			assert.Len(t, out, int(perHour)-drops)
		})

		for _, jump := range []int64{1500, -1500} {
			t.Run(fmt.Sprintf("%d reset %+d ms", rate, jump), func(t *testing.T) {
				msAt := func(k int64) int64 {
					ms := 10_000 + k*1024*1000/int64(rate)
					if k >= 100 {
						ms += jump
					}
					return ms
				}
				var g audioGrid
				var reanchors int
				var anchor int64
				for k := range int64(300) {
					pts, keep, re := g.place(msAt(k)*90, rate)
					require.True(t, keep, "frame %d", k)
					if re {
						reanchors++
						anchor = pts
						require.Equal(t, int64(100), k)
					}
					if k > 100 {
						require.Equal(t, gridAt(anchor, k-100, rate), pts)
					}
				}
				assert.Equal(t, 1, reanchors, "jump %d ms", jump)
			})
		}
	}
}
