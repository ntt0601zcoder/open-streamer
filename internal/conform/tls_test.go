package conform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

// ReadOptions.TLS is the client config of an rtmps:// and an rtsps:// read:
// with the CA the capture is the plain one's, and without it the server is
// refused.
func TestReadTLS(t *testing.T) {
	ca := tlstest.NewCA(t)
	var unknown x509.UnknownAuthorityError

	t.Run("rtmps", func(t *testing.T) {
		tags := muxedTags(t, "h264_aac.ts")
		plain := readTags(t, tags)
		s := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{TLS: ca.Server(t)})
		u := s.URL("live/s")
		require.Contains(t, u, "rtmps://")
		r := awaitRTMP(t, readRTMPAsync(u, ReadOptions{TLS: ca.Client()}))
		require.NoError(t, r.err)
		assert.Empty(t, r.c.Violations)
		assert.Len(t, r.c.Frames, len(plain.Frames))

		r = awaitRTMP(t, readRTMPAsync(u, ReadOptions{}))
		require.ErrorAs(t, r.err, &unknown)
	})

	t.Run("rtsps", func(t *testing.T) {
		h := newRTSPHandler(t, "h264_aac.ts", rtspFaults{})
		l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		srv := rtsp.NewServer(h, rtsp.ServerConfig{})
		var wg sync.WaitGroup
		wg.Go(func() { _ = srv.ServeTLS(l, ca.Server(t)) })
		t.Cleanup(func() { _ = srv.Close(); wg.Wait() })
		u := "rtsps://" + l.Addr().String() + "/live/s"

		c := readRTSP(t, u, ReadOptions{TLS: ca.Client()})
		assert.Empty(t, c.Violations)
		assert.Positive(t, countKind(c.Frames, media.KindVideo))
		assert.Positive(t, countKind(c.Frames, media.KindAudio))

		ctx, cancel := context.WithTimeout(context.Background(), rtmpWait)
		defer cancel()
		_, err = ReadRTSP(ctx, u, ReadOptions{Timeout: rtmpWait, TLS: &tls.Config{MinVersion: tls.VersionTLS12}})
		require.ErrorAs(t, err, &unknown)
	})
}
