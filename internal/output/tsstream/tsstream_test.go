package tsstream

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

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// fixtureFrames returns a fixture's frames in stream time.
func fixtureFrames(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/fixtures/" + name)
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

// recSink records the order of sink calls.
type recSink struct {
	calls   []string
	failAt  int // fail the Write with this 1-based index; 0 never
	writes  int
	failErr error
	bytes   int
}

func (r *recSink) Write(p []byte) (int, error) {
	r.calls = append(r.calls, "write")
	r.writes++
	if r.failAt > 0 && r.writes == r.failAt {
		return 0, r.failErr
	}
	r.bytes += len(p)
	return len(p), nil
}

func (r *recSink) SetWriteDeadline(time.Time) error {
	r.calls = append(r.calls, "deadline")
	return nil
}

func (r *recSink) Flush() error {
	r.calls = append(r.calls, "flush")
	return nil
}

// run feeds frames to Stream through a hub and returns once Stream ends.
func run(t *testing.T, frames []media.Frame, s Sink) (Stats, error) {
	t.Helper()
	h := hub.New(hub.Options{})
	type res struct {
		st  Stats
		err error
	}
	done := make(chan res, 1)
	go func() {
		st, err := Stream(context.Background(), h, "test", s, Options{})
		done <- res{st, err}
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)
	for _, f := range frames {
		h.Write(f)
	}
	h.Close()
	select {
	case r := <-done:
		return r.st, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not end")
		return Stats{}, nil
	}
}

func TestStreamSink(t *testing.T) {
	t.Run("deadline before and flush after each write", func(t *testing.T) {
		s := &recSink{}
		st, err := run(t, fixtureFrames(t, "h264_aac.ts"), s)
		require.NoError(t, err)
		assert.Equal(t, EndedClosed, st.Ended)
		assert.Positive(t, st.Frames)
		require.NotEmpty(t, s.calls)
		require.Zero(t, len(s.calls)%3)
		for i := 0; i < len(s.calls); i += 3 {
			assert.Equal(t, []string{"deadline", "write", "flush"}, s.calls[i:i+3], "call %d", i/3)
		}
		assert.Equal(t, int(st.Frames), len(s.calls)/3)
	})

	t.Run("write error ends Stream wrapped", func(t *testing.T) {
		boom := errors.New("boom")
		s := &recSink{failAt: 3, failErr: boom}
		st, err := run(t, fixtureFrames(t, "h264_aac.ts"), s)
		require.ErrorIs(t, err, boom)
		assert.Equal(t, EndedError, st.Ended)
		assert.Equal(t, 3, s.writes, "no write after the failing one")
	})
}
