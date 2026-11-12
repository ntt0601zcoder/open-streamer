package live

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// smallBufListener shrinks the send buffer of every accepted connection.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		if err := tc.SetWriteBuffer(4096); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// smallRcvBuf sets SO_RCVBUF before connect, so the window stays small.
func smallRcvBuf(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
	}); err != nil {
		return err
	}
	return serr
}

// A client that requests a large file and reads nothing has its response
// abandoned once the write deadline passes, instead of holding the handler
// forever.
func TestLiveStalledClient(t *testing.T) {
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Now: farFuture})
	handleAll(p, synth(h264Config(t), 1, 0, 60))
	big := strings.Repeat("#", 32<<20)
	routes := []playlistRoute{{
		dir:   dirHLS,
		match: func(name string) bool { return name == "big.m3u8" },
		build: func(*Packager, Snapshot, string) ([]byte, string, bool) { return []byte(big), "text/plain", true },
	}}
	inner := newHandler(func(string) (*Packager, bool) { return p, true }, routes, 200*time.Millisecond)
	returned := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		inner.ServeHTTP(w, r)
	}))
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	defer srv.Close()

	tr := &http.Transport{DialContext: (&net.Dialer{Control: smallRcvBuf}).DialContext}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/live/s/hls/big.m3u8", nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still writing to a stalled client")
	}
	// The connection was closed under the client: reading ends early.
	n, _ := io.Copy(io.Discard, resp.Body)
	require.Less(t, n, int64(len(big)))
}
