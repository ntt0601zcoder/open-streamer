package httpts

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestHTTPTSStalledClient(t *testing.T) {
	h := hub.New(hub.Options{})
	defer h.Close()
	srv := httptest.NewUnstartedServer(newHandler(lookupOne(h), 0, 200*time.Millisecond))
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	defer srv.Close()

	tr := &http.Transport{DialContext: (&net.Dialer{Control: smallRcvBuf}).DialContext}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/live/s/stream.ts", nil)
	require.NoError(t, err)
	type got struct {
		resp *http.Response
		err  error
	}
	ch := make(chan got, 1)
	go func() {
		resp, err := client.Do(req) //nolint:bodyclose // closed below
		ch <- got{resp, err}
	}()
	waitSubscribed(t, h)

	// The client reads nothing; frames keep coming until the handler gives up.
	frame := synth(configOf(t, "h264_aac.ts", media.CodecH264), 0, 1, 16<<10)[0]
	deadline := time.Now().Add(waitFor)
	for dts := int64(0); len(h.Stats()) > 0; dts += 3600 {
		require.True(t, time.Now().Before(deadline), "handler did not unsubscribe")
		frame.DTS, frame.PTS = dts, dts
		h.Write(frame)
		time.Sleep(time.Millisecond)
	}
	assert.Empty(t, h.Stats(), "unsubscribed while the hub is still open")

	cancel()
	g := <-ch
	if g.err == nil {
		_ = g.resp.Body.Close()
	}
}
