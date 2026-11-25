package transcode

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// The retry rule alone: the first failure after a success, or after a
// child's start, asks for a reset; failures that stay consecutive do not.
func TestShotRetry(t *testing.T) {
	type look struct {
		thumbs, failed uint64
		exited         bool // childExited instead of a look
		want           bool
	}
	for name, looks := range map[string][]look{
		"nothing happens":      {{0, 0, false, false}, {0, 0, false, false}},
		"successes only":       {{1, 0, false, false}, {5, 0, false, false}},
		"first failure":        {{0, 1, false, true}},
		"consecutive failures": {{0, 1, false, true}, {0, 2, false, false}, {0, 9, false, false}, {0, 9, false, false}},
		"failure after success": {
			{0, 1, false, true}, {0, 2, false, false}, {1, 2, false, false}, {1, 3, false, true}, {1, 4, false, false},
		},
		"both rose: the success counts first": {
			{0, 1, false, true}, {0, 2, false, false}, {1, 3, false, true}, {1, 4, false, false},
		},
		"both rose at the start": {{3, 2, false, true}, {3, 3, false, false}},
		"child exited": {
			{0, 1, false, true},
			{0, 2, false, false},
			{exited: true},
			{0, 0, false, false},
			{0, 1, false, true},
			{0, 2, false, false},
		},
		"child exited after a success": {
			{2, 0, false, false}, {exited: true}, {0, 1, false, true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShotRetry()
			for i, l := range looks {
				if l.exited {
					r.childExited()
					continue
				}
				assert.Equal(t, l.want, r.look(l.thumbs, l.failed), "look %d (%d, %d)", i, l.thumbs, l.failed)
			}
		})
	}
}

func helloBytes(t *testing.T, msgs ...wire.Message) *wire.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	for _, m := range msgs {
		require.NoError(t, w.Write(m))
	}
	return wire.NewReader(&buf)
}

func TestReadHello(t *testing.T) {
	h, err := readHello(helloBytes(t, wire.Hello{Version: wire.Version, PID: 42}))
	require.NoError(t, err)
	assert.Equal(t, wire.Hello{Version: wire.Version, PID: 42}, h)

	_, err = readHello(helloBytes(t, wire.Hello{Version: 1, PID: 42}))
	require.ErrorIs(t, err, errVersion)
	assert.Contains(t, err.Error(), "1, want 2")

	_, err = readHello(helloBytes(t, wire.Stats{In: 1}))
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVersion)
	assert.Contains(t, err.Error(), "first message is wire.Stats")

	// A zero length is malformed.
	_, err = readHello(wire.NewReader(bytes.NewReader([]byte{0, 0, 0, 0, 0xff, 0xff})))
	require.ErrorIs(t, err, wire.ErrMalformed)

	_, err = readHello(wire.NewReader(bytes.NewReader(nil)))
	require.ErrorIs(t, err, io.EOF)
}

func TestBackoff(t *testing.T) {
	ms := time.Millisecond
	b := backoff{min: 250 * ms, max: 5 * time.Second, healthy: 30 * time.Second}
	got := make([]time.Duration, 0, 7)
	for range 7 {
		got = append(got, b.next(0))
	}
	assert.Equal(t, []time.Duration{250 * ms, 500 * ms, 1000 * ms, 2000 * ms, 4000 * ms, 5000 * ms, 5000 * ms}, got)
	assert.Equal(t, 5*time.Second, b.next(29*time.Second), "not yet healthy")
	assert.Equal(t, 250*ms, b.next(30*time.Second), "back to min after a healthy run")
	assert.Equal(t, 500*ms, b.next(time.Second))
	assert.Equal(t, 250*ms, b.next(time.Hour))

	// A max below twice the min caps the second delay.
	b = backoff{min: 20 * ms, max: 30 * ms, healthy: time.Second}
	assert.Equal(t, 20*ms, b.next(0))
	assert.Equal(t, 30*ms, b.next(0))
	assert.Equal(t, 30*ms, b.next(0))
}

func TestWaitOrKill(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		start := time.Now()
		waitOrKill(done, func() { t.Error("kill called for a child that is done") })
		assert.Less(t, time.Since(start), stopGrace)
	})
	t.Run("done in time", func(t *testing.T) {
		done := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { close(done) })
		waitOrKill(done, func() { t.Error("kill called for a child that stopped in time") })
	})
	t.Run("never done", func(t *testing.T) {
		done := make(chan struct{}) // never closed
		var kills atomic.Int32
		start := time.Now()
		waitOrKill(done, func() { kills.Add(1) })
		// It returned although done is still open.
		assert.GreaterOrEqual(t, time.Since(start), stopGrace)
		assert.EqualValues(t, 1, kills.Load())
	})
}

func TestHelloFailed(t *testing.T) {
	read := func(b []byte) error {
		_, err := readHello(wire.NewReader(bytes.NewReader(b)))
		require.Error(t, err)
		return err
	}
	eof, junk := read(nil), read([]byte{0, 0, 0, 0, 0xff, 0xff})
	assert.NoError(t, helloFailed(eof, false, time.Second), "the socket ended")
	assert.NoError(t, helloFailed(junk, true, time.Second), "the child has exited")
	assert.EqualError(t, helloFailed(fmt.Errorf("transcode: hello: %w", os.ErrDeadlineExceeded), false, 3*time.Second),
		"transcode: hello: none within 3s")
	err := helloFailed(junk, false, time.Second)
	require.ErrorIs(t, err, wire.ErrMalformed)
	assert.Contains(t, err.Error(), "transcode: hello: ")
}
