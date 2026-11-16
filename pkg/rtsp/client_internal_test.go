package rtsp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipeClient is a Client over one end of a net.Pipe nobody reads, so every
// write blocks until its deadline.
func pipeClient(t *testing.T) *Client {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return &Client{
		cfg: Config{Timeout: 10 * time.Second}.withDefaults(), conn: a, auth: NewAuth("", ""),
		pending: map[int]chan *Response{}, done: make(chan struct{}),
		desc: Description{Control: "rtsp://127.0.0.1/x"},
	}
}

func TestTeardownCutsStuckWriter(t *testing.T) {
	c := pipeClient(t)
	errc := make(chan error, 1)
	go func() { errc <- c.writeFrame(1, []byte{1, 2, 3}) }()
	time.Sleep(50 * time.Millisecond) // the writer holds wmu, blocked
	start := time.Now()
	c.teardown(context.Background())
	assert.Less(t, time.Since(start), 300*time.Millisecond)
	select {
	case err := <-errc:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("stuck writer not cut short")
	}
}

func TestWritesClampedAfterClose(t *testing.T) {
	c := pipeClient(t)
	c.closeBy.Store(time.Now().Add(100 * time.Millisecond).UnixNano())
	start := time.Now()
	require.Error(t, c.writeFrame(1, []byte{1}))
	require.Error(t, c.writeMsg(&Response{StatusCode: 200, Reason: "OK", Header: Header{}}, time.Now().Add(10*time.Second)))
	assert.Less(t, time.Since(start), time.Second)
}
