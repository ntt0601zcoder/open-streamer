package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
)

// headClip returns the first n whole TS packets of a fixture.
func headClip(t *testing.T, name string, n int) []byte {
	t.Helper()
	b := fixtureBytes(t, name)
	require.Greater(t, len(b), n*188)
	return b[:n*188]
}

// TestPacedLoopDoesNotDrift loops a short clip with pacing for 1000 epochs
// on a fake clock, packaging as the pipeline runs. Each epoch starts after
// the previous epoch's presentation end, past where the wall pacing reached;
// that excess must not accumulate into the epoch walls, or the live window
// drifts into the future and stops being listed.
func TestPacedLoopDoesNotDrift(t *testing.T) {
	clip := headClip(t, "h264_aac.ts", 120)
	const loops = 1000
	errStop := errors.New("stop")

	clk := newFakeClock(m3aT0)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("packager", 1<<12)
	defer sub.Close()
	p := live.NewPackager(h, live.Options{Now: clk.Now})

	var opens []time.Time // fake wall at each Open: the next epoch's arrival
	leads := map[uint32]time.Duration{}
	warm, checks, minListed := false, 0, -1
	check := func() {
		for {
			select {
			case d := <-sub.C():
				p.Handle(d)
				continue
			default:
			}
			break
		}
		s := p.Snapshot()
		for _, e := range s.Epochs {
			if _, ok := leads[e.Epoch]; ok {
				continue
			}
			require.LessOrEqual(t, int(e.Epoch), len(opens), "one epoch per loop")
			require.Equal(t, e.Anchor.Wall, e.Wall, "epoch %d: W_n is the anchor wall", e.Epoch)
			leads[e.Epoch] = e.Wall.Sub(opens[e.Epoch-1])
		}
		if len(s.Segments) == s.Window {
			warm = true
		}
		if warm {
			checks++
			if minListed < 0 || s.Listed < minListed {
				minListed = s.Listed
			}
			require.GreaterOrEqual(t, s.Listed, s.Window-1, "listed at %v, %d epochs", clk.Now().Sub(m3aT0), len(opens))
		}
	}
	err := pipeline.Run(context.Background(), h, pipeline.Options{
		Open: func() (io.ReadCloser, error) {
			if len(opens) == loops {
				return nil, errStop
			}
			opens = append(opens, clk.Now())
			return io.NopCloser(bytes.NewReader(clip)), nil
		},
		Loop: true,
		Pace: true,
		Now:  clk.Now,
		Sleep: func(_ context.Context, d time.Duration) error {
			check()
			clk.Set(clk.Now().Add(d))
			return nil
		},
	})
	require.ErrorIs(t, err, errStop)
	check()
	require.Positive(t, checks)
	last := uint32(loops - 1) // the last epoch's segment is still open
	require.Equal(t, int(last), len(leads), "every closed epoch was packaged")

	// The lead of an epoch's wall over its arrival is one presentation-end
	// excess (the clip's last GOP is cut, so its presented frames run up to
	// four frames past its last DTS), never the sum over the epochs before
	// it; pacing waits it out.
	bound := 200 * time.Millisecond
	var hi time.Duration
	for id, l := range leads {
		assert.GreaterOrEqual(t, l, time.Duration(0), "epoch %d", id)
		hi = max(hi, l)
	}
	assert.Less(t, hi, bound)
	assert.InDelta(t, float64(leads[2]), float64(leads[last]), float64(time.Millisecond), "lead does not grow")
	t.Logf("%d epochs over %v of wall; lead epoch 2 %v, epoch %d %v, max %v; min listed %d",
		loops, clk.Now().Sub(m3aT0), leads[2], last, leads[last], hi, minListed)
}
