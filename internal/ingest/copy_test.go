package ingest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

var (
	copyH264 = &media.TrackConfig{Codec: media.CodecH264}
	copyAAC  = &media.TrackConfig{Codec: media.CodecAAC}
)

func copyVid(dts int64, key bool, epoch uint32) media.Frame {
	return media.Frame{Track: 0, Config: copyH264, PTS: dts, DTS: dts, Key: key, Data: [][]byte{{1}}, Epoch: epoch}
}

func copyAud(dts int64, epoch uint32) media.Frame {
	return media.Frame{Track: 1, Config: copyAAC, PTS: dts, DTS: dts, Data: [][]byte{{2}}, Epoch: epoch}
}

func copyEnv(h *hub.Hub) Env {
	return Env{Lookup: func(name string) (*hub.Hub, bool) { return h, name == "src" }}
}

func openCopyT(t *testing.T, h *hub.Hub) Reader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Open(ctx, "copy://src", copyEnv(h))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// openCopyAsync opens while a writer feeds the hub after the open began.
func openCopyAsync(t *testing.T, h *hub.Hub, feed func()) Reader {
	t.Helper()
	type res struct {
		r   Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := Open(ctx, "copy://src", copyEnv(h))
		ch <- res{r, err}
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) == 1 }, 2*time.Second, time.Millisecond)
	feed()
	got := <-ch
	require.NoError(t, got.err)
	t.Cleanup(func() { _ = got.r.Close() })
	return got.r
}

func TestCopyFrames(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	h.Write(copyVid(0, true, 3))
	h.Write(copyAud(10, 3))
	h.Write(copyVid(3000, false, 3))
	r := openCopyT(t, h)

	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	assert.Equal(t, Spec{Live: true}, r.Spec())
	want := []media.Frame{copyVid(0, true, 0), copyAud(10, 0), copyVid(3000, false, 0)}
	for _, w := range want {
		f, err := r.Read()
		require.NoError(t, err)
		assert.Equal(t, w, f.Frame)
		assert.False(t, f.Arrival.IsZero())
	}
	h.Write(copyVid(6000, false, 3))
	f, err := r.Read()
	require.NoError(t, err)
	assert.Equal(t, copyVid(6000, false, 0), f.Frame)
	assert.NotNil(t, r.Stats())
}

func TestCopyLostIsDiscontinuity(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	r := openCopyAsync(t, h, func() { h.Write(copyVid(0, true, 0)) })
	// Overflow the subscriber queue (1024): the extra frames are lost.
	for i := 1; i <= 1100; i++ {
		h.Write(copyVid(int64(i)*3000, false, 0))
	}
	for i := range 1025 {
		f, err := r.Read()
		require.NoError(t, err)
		assert.Equal(t, int64(i)*3000, f.DTS)
	}
	h.Write(copyVid(5_000_000, true, 0))
	_, err := r.Read()
	require.ErrorIs(t, err, ErrDiscontinuity)
	f, err := r.Read()
	require.NoError(t, err)
	assert.Equal(t, int64(5_000_000), f.DTS)
	assert.Positive(t, r.Stats()["lost_frames"])
}

func TestCopyEpochIsDiscontinuity(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	r := openCopyAsync(t, h, func() { h.Write(copyVid(0, true, 1)) })
	h.Write(copyVid(3000, false, 1))
	h.Write(copyVid(0, true, 2))
	h.Write(copyVid(3000, false, 2))

	var seq []string
	for range 5 {
		f, err := r.Read()
		if err != nil {
			require.ErrorIs(t, err, ErrDiscontinuity)
			seq = append(seq, "D")
			continue
		}
		assert.Zero(t, f.Epoch)
		seq = append(seq, "F")
	}
	assert.Equal(t, []string{"F", "F", "D", "F", "F"}, seq)
}

func TestCopyUnknown(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	_, err := Open(context.Background(), "copy://nope", copyEnv(h))
	require.Error(t, err)
	_, err = Open(context.Background(), "copy://src", Env{})
	require.Error(t, err)
}

func TestCopyClose(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	h.Write(copyVid(0, true, 0))
	r := openCopyT(t, h)
	require.Len(t, h.Stats(), 1)
	assert.Equal(t, "copy:copy://src", h.Stats()[0].Name)

	done := make(chan error, 1)
	go func() {
		for {
			if _, err := r.Read(); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, r.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Read not unblocked by Close")
	}
	assert.Empty(t, h.Stats())
	require.NoError(t, r.Close())
}

func TestCopyHubClosedIsEOF(t *testing.T) {
	h := hub.New(hub.Options{})
	h.Write(copyVid(0, true, 0))
	r := openCopyT(t, h)
	_, err := r.Read()
	require.NoError(t, err)
	h.Close()
	_, err = r.Read()
	assert.ErrorIs(t, err, io.EOF)
}

func TestCopyOpenCtx(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Open(ctx, "copy://src", copyEnv(h))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, h.Stats())
}

func TestOpenCopy(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	h.Write(copyAud(0, 0))
	r := openCopyT(t, h) // audio-only: any frame ends the wait
	assert.Equal(t, []media.Kind{0, media.KindAudio}, r.Kinds())
}
