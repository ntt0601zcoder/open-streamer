package rtsp

import (
	"context"
	"crypto/tls"
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

// TestNewTargetDefaultPort checks the address Dial connects to: port 554
// when the URL names none. No connection is made.
func TestNewTargetDefaultPort(t *testing.T) {
	for _, tc := range []struct{ in, addr, url string }{
		{"rtsp://127.0.0.1/stream", "127.0.0.1:554", "rtsp://127.0.0.1/stream"},
		{"rtsp://u:p@cam/stream?ch=1", "cam:554", "rtsp://cam/stream?ch=1"},
		{"rtsp://[::1]/x", "[::1]:554", "rtsp://[::1]/x"},
		{"rtsp://cam:8554/x", "cam:8554", "rtsp://cam:8554/x"},
	} {
		tg, err := newTarget(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.addr, tg.addr, tc.in)
		assert.Equal(t, tc.url, tg.url, tc.in)
	}
	for _, bad := range []string{"http://cam/x", "rtsp:///x", "rtsp://%zz/x"} {
		_, err := newTarget(bad)
		assert.Error(t, err, bad)
	}
}

// TestNewTargetTLS checks the rtsps:// target: port 322 when the URL names
// none, the TLS flag and server name, and that the product's +insecure
// spelling is not this package's. No connection is made.
func TestNewTargetTLS(t *testing.T) {
	for _, tc := range []struct{ in, addr, url, host, user, pass string }{
		{"rtsps://cam/s", "cam:322", "rtsps://cam/s", "cam", "", ""},
		{"rtsps://u:p@cam:8322/s", "cam:8322", "rtsps://cam:8322/s", "cam", "u", "p"},
		{"RTSPS://cam/s", "cam:322", "RTSPS://cam/s", "cam", "", ""},
		{"rtsps://[::1]/s", "[::1]:322", "rtsps://[::1]/s", "::1", "", ""},
		{"rtsps://[fe80::1%25eth0]/s", "[fe80::1%eth0]:322", "rtsps://[fe80::1%25eth0]/s", "fe80::1%eth0", "", ""},
	} {
		tg, err := newTarget(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.addr, tg.addr, tc.in)
		assert.Equal(t, tc.url, tg.url, tc.in)
		assert.Equal(t, tc.host, tg.host, tc.in)
		assert.True(t, tg.secure, tc.in)
		assert.Equal(t, tc.user, tg.user, tc.in)
		assert.Equal(t, tc.pass, tg.pass, tc.in)
	}
	tg, err := newTarget("rtsp://cam/s")
	require.NoError(t, err)
	assert.False(t, tg.secure)
	_, err = newTarget("rtsps+insecure://cam/s")
	assert.EqualError(t, err, "rtsp: dial: unsupported scheme")
}

// TestClientTLSServerName checks the server name an rtsps:// dial verifies:
// the URL's host without an IPv6 zone, unless the caller's config names one;
// the caller's config is cloned, not changed. No connection is made.
func TestClientTLSServerName(t *testing.T) {
	for _, tc := range []struct{ in, name string }{
		{"rtsps://cam/s", "cam"},
		{"rtsps://[::1]/s", "::1"},
		{"rtsps://[fe80::1%25eth0]/s", "fe80::1"},
		{"rtsps://127.0.0.1:8322/s", "127.0.0.1"},
	} {
		tg, err := newTarget(tc.in)
		require.NoError(t, err, tc.in)
		c := clientTLS(Config{}, tg)
		assert.Equal(t, tc.name, c.ServerName, tc.in)
		assert.EqualValues(t, tls.VersionTLS12, c.MinVersion, tc.in)

		own := &tls.Config{MinVersion: tls.VersionTLS13}
		c = clientTLS(Config{TLS: own}, tg)
		assert.Equal(t, tc.name, c.ServerName, tc.in)
		assert.EqualValues(t, tls.VersionTLS13, c.MinVersion, tc.in)
		assert.Empty(t, own.ServerName, "%s: the caller's config is cloned", tc.in)
	}
	tg, err := newTarget("rtsps://[fe80::1%25eth0]/s")
	require.NoError(t, err)
	named := &tls.Config{ServerName: "cam.example", MinVersion: tls.VersionTLS12}
	assert.Equal(t, "cam.example", clientTLS(Config{TLS: named}, tg).ServerName)
}
