package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/fixtures/" + name)
	require.NoError(t, err)
	return b
}

func openBytes(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

func drain(s *hub.Sub) []hub.Delivery {
	var out []hub.Delivery
	for {
		select {
		case d, ok := <-s.C():
			if !ok {
				return out
			}
			out = append(out, d)
		default:
			return out
		}
	}
}

func TestRunWritesAnchorsBeforeFrames(t *testing.T) {
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	// Check at delivery time: the anchor must exist when the frame is written.
	require.NoError(t, Run(context.Background(), h, Options{Open: openBytes(fixture(t, "h264_aac_loop.ts"))}))
	seen := map[uint32]bool{}
	n := 0
	for _, d := range drain(sub) {
		n++
		if seen[d.Frame.Epoch] {
			continue
		}
		seen[d.Frame.Epoch] = true
		_, ok := h.Anchor(d.Frame.Epoch)
		assert.True(t, ok, "epoch %d", d.Frame.Epoch)
	}
	assert.Greater(t, n, 0)
	assert.NotEmpty(t, seen)
}

func TestRunAnchorExistsAtWrite(t *testing.T) {
	// A hub subscriber cannot run inside Write, so verify ordering by polling
	// concurrently: the first frame of each epoch must never arrive without it.
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	done := make(chan struct{})
	bad := make(chan uint32, 1)
	go func() {
		defer close(done)
		seen := map[uint32]bool{}
		for d := range sub.C() {
			if !seen[d.Frame.Epoch] {
				seen[d.Frame.Epoch] = true
				if _, ok := h.Anchor(d.Frame.Epoch); !ok {
					select {
					case bad <- d.Frame.Epoch:
					default:
					}
				}
			}
		}
	}()
	require.NoError(t, Run(context.Background(), h, Options{Open: openBytes(fixture(t, "h264_aac_loop.ts"))}))
	h.Close()
	<-done
	assert.Empty(t, bad)
}

func TestRunLoop(t *testing.T) {
	b := fixture(t, "h264_aac.ts")
	h := hub.New(hub.Options{})
	sub := h.Subscribe("t", 1<<16)
	opens := 0
	boom := errors.New("boom")
	err := Run(context.Background(), h, Options{
		Loop: true,
		Open: func() (io.ReadCloser, error) {
			opens++
			if opens > 3 {
				return nil, boom
			}
			return io.NopCloser(bytes.NewReader(b)), nil
		},
	})
	require.ErrorIs(t, err, boom)
	assert.ErrorContains(t, err, "pipeline: open:")
	epochs := map[uint32]bool{}
	for _, d := range drain(sub) {
		epochs[d.Frame.Epoch] = true
	}
	assert.Equal(t, map[uint32]bool{1: true, 2: true, 3: true}, epochs)
}

func TestRunPace(t *testing.T) {
	b := fixture(t, "h264_aac.ts")
	// Unpaced run to learn the DTS span.
	h0 := hub.New(hub.Options{})
	s0 := h0.Subscribe("t", 1<<16)
	require.NoError(t, Run(context.Background(), h0, Options{Open: openBytes(b)}))
	var lo, hi int64
	first := true
	for _, d := range drain(s0) {
		if first || d.Frame.DTS < lo {
			lo = d.Frame.DTS
		}
		if first || d.Frame.DTS > hi {
			hi = d.Frame.DTS
		}
		first = false
	}
	require.False(t, first)

	now := time.Unix(1000, 0)
	var slept time.Duration
	h := hub.New(hub.Options{})
	err := Run(context.Background(), h, Options{
		Open: openBytes(b),
		Pace: true,
		Now:  func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept += d
			now = now.Add(d)
			return nil
		},
	})
	require.NoError(t, err)
	sleptTicks := int64(slept) * media.ClockRate / int64(time.Second)
	assert.InDelta(t, hi-lo, sleptTicks, 3600)
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	h := hub.New(hub.Options{})
	err := Run(ctx, h, Options{
		Open: openBytes(fixture(t, "h264_aac.ts")),
		Pace: true,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			calls++
			if calls == 3 {
				cancel()
			}
			return ctx.Err()
		},
	})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRunCancelDefaultSleep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	h := hub.New(hub.Options{})
	err := Run(ctx, h, Options{Open: openBytes(fixture(t, "h264_aac.ts")), Pace: true, Loop: true})
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestRunBadInput(t *testing.T) {
	h := hub.New(hub.Options{})
	err := Run(context.Background(), h, Options{Open: openBytes(bytes.Repeat([]byte("garbage!"), 500))})
	require.Error(t, err)
	assert.ErrorContains(t, err, "pipeline:")
}
