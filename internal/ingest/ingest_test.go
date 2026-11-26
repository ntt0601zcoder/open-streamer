package ingest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp/rtsptest"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

func TestParse(t *testing.T) {
	tests := []struct {
		url  string
		want Spec
	}{
		{"/tmp/a.ts", Spec{Pace: true}},
		{"relative/a.ts", Spec{Pace: true}},
		{"file:///tmp/a.ts", Spec{Pace: true}},
		{"file:///tmp/a.ts?loop=1", Spec{Pace: true}},
		{"file:///tmp/a.ts?loop=1&pace=0", Spec{}},
		{"file:///tmp/a.ts?pace=1", Spec{Pace: true}},
		{"udp://@:5000", Spec{Live: true, Passive: true}},
		{"udp://239.1.1.1:5000?iface=eth0", Spec{Live: true, Passive: true}},
		{"srt://host:9000?mode=caller&streamid=a&" + kPass + "=" + vSecret + "&latency=120", Spec{Live: true}},
		{"srt://host:9000", Spec{Live: true}},
		{"srt://:9000?mode=listener&streamid=a", Spec{Live: true, Passive: true}},
		{"http://h/stream.ts", Spec{Live: true}},
		{"https://h/live", Spec{Live: true}},
		{"http://h/index.m3u8", Spec{Pace: true, Live: true}},
		{"https://h/a/B.M3U8?token=1", Spec{Pace: true, Live: true}},
		{"copy://cam1", Spec{Live: true}},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := Parse(tt.url)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	for _, bad := range []string{
		"", "ftp://x", "file://", "file://host/a.ts", "file:///a.ts?loop=maybe", "file:///a.ts?pace=x",
		"udp://@", "udp://@:x", "srt://:9000", "srt://host", "srt://host:9000?mode=rendezvous",
		"srt://:9000?mode=caller", "http://", "copy://", "copy://?x=1", "://x", "http://[::1",
	} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}
}

// Redact is redact.URL (its tests are there).
func TestRedact(t *testing.T) {
	for _, u := range []string{
		"srt://u:p@h:1?" + kPass + "=" + vS + "&streamid=x", "rtmp://h/live/" + vStreamKey, "/tmp/a.ts", "http://[::1",
	} {
		assert.Equal(t, redact.URL(u), Redact(u), u)
	}
	assert.Equal(t, "rtmp://h/live/skxxxxx", Redact("rtmp://h/live/"+vStreamKey))
}

func TestOpenFile(t *testing.T) {
	p, err := filepath.Abs(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	for _, u := range []string{p, "file://" + p} {
		r, err := Open(context.Background(), u, Env{})
		require.NoError(t, err, u)
		assert.Equal(t, Spec{Pace: true}, r.Spec())
		require.NoError(t, r.Close())
	}

	for _, u := range []string{
		"srt://h:1?" + kPass + "=" + vSekrit, "http://u:" + vPW + "@h/x.ts", "http://h/x.m3u8", "copy://x",
	} {
		_, err := Open(context.Background(), u, Env{})
		require.Error(t, err, u)
		assert.NotContains(t, err.Error(), vSekrit)
		assert.NotContains(t, err.Error(), vPW+"@")
	}

	_, err = Open(context.Background(), "ftp://x/y?"+kPass+"="+vSekrit, Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), vSekrit)
	assert.Contains(t, err.Error(), "ftp://x/y")

	_, err = Open(context.Background(), filepath.Join(t.TempDir(), "missing.ts"), Env{})
	assert.Error(t, err)

	garbage := filepath.Join(t.TempDir(), "g.ts")
	require.NoError(t, os.WriteFile(garbage, []byte("not a transport stream"), 0o600))
	_, err = Open(context.Background(), garbage, Env{})
	assert.Error(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Open(ctx, p, Env{})
	assert.ErrorIs(t, err, context.Canceled)
}

// The TLS schemes parse to the Spec of their plain forms, in any case;
// +insecure is refused on a listener and on every other scheme, and udp on
// rtsps; no refusal holds a secret.
func TestParseTLSSchemes(t *testing.T) {
	pull, listen := Spec{Live: true}, Spec{Live: true, Passive: true}
	for _, c := range []struct {
		url, plain string
		want       Spec
	}{
		{"rtmps://h/live/s", "rtmp://h/live/s", pull},
		{"rtmps+insecure://h/live/s", "rtmp://h/live/s", pull},
		{"rtmps://:1936/pub/k?mode=listen", "rtmp://:1936/pub/k?mode=listen", listen},
		{"rtsps://cam/s", "rtsp://cam/s", pull},
		{"rtsps+insecure://u:p@cam:322/s?transport=tcp", "rtsp://u:p@cam:322/s?transport=tcp", pull},
		{"rtsps://cam/s?transport=auto", "rtsp://cam/s?transport=auto", pull},
		{"RTSPS://cam/s", "RTSP://cam/s", pull},
		{"RTMPS+Insecure://h/live/s", "RTMP://h/live/s", pull},
	} {
		got, err := Parse(c.url)
		require.NoError(t, err, c.url)
		assert.Equal(t, c.want, got, c.url)
		plain, err := Parse(c.plain)
		require.NoError(t, err, c.plain)
		assert.Equal(t, plain, got, "%s has the Spec of %s", c.url, c.plain)
	}

	const (
		noListen = "+insecure is for a pull: a listener presents the server's certificate"
		noUDP    = "rtsps carries RTP inside TLS: the transport must be tcp"
	)
	for _, c := range []struct{ url, want string }{
		{"rtmps+insecure://:1936/pub/k?mode=listen", "ingest: parse rtmps+insecure://:1936/pub/xxxxx?mode=listen: " + noListen},
		{"rtmps+insecure://:1936/pub/k?mode=listener", "ingest: parse rtmps+insecure://:1936/pub/xxxxx?mode=listener: " + noListen},
		{"rtsps://cam/s?transport=udp", "ingest: parse rtsps://cam/s?transport=udp: " + noUDP},
		{"rtsps://cam/s?transport=UDP", "ingest: parse rtsps://cam/s?transport=UDP: " + noUDP},
		{"rtsps://cam/s?transport=%75dp", "ingest: parse rtsps://cam/s?transport=%75dp: " + noUDP},
		{"rtsps+insecure://cam/s?transport=udp", "ingest: parse rtsps+insecure://cam/s?transport=udp: " + noUDP},
		{"rtmp+insecure://h/a/b", `ingest: parse rtmp+insecure://h/a/xxxxx: unsupported scheme "rtmp+insecure"`},
		{"rtsp+insecure://cam/s", `ingest: parse rtsp+insecure://cam/s: unsupported scheme "rtsp+insecure"`},
		{"http+insecure://h/x.ts", `ingest: parse http+insecure://h/x.ts: unsupported scheme "http+insecure"`},
		{"https+insecure://h/x.ts", `ingest: parse https+insecure://h/x.ts: unsupported scheme "https+insecure"`},
		{"srt+insecure://h:1", `ingest: parse srt+insecure://h:1: unsupported scheme "srt+insecure"`},
		{"rtmps+insecure+insecure://h/a/b", `ingest: parse rtmps+insecure+insecure://h/a/xxxxx: unsupported scheme "rtmps+insecure+insecure"`},
	} {
		_, err := Parse(c.url)
		require.Error(t, err, c.url)
		assert.Equal(t, c.want, err.Error(), c.url)
		// Open refuses it the same way, before any I/O.
		_, err = Open(context.Background(), c.url, Env{})
		require.Error(t, err, c.url)
		assert.Equal(t, c.want, err.Error(), c.url)
	}

	// The rules of the plain forms hold for the TLS ones.
	for _, bad := range []string{
		"rtmps:///live/s", "rtmps://h/live", "rtmps://:0/live/s?mode=listen", "rtmps://h/live/s?mode=caller",
		"rtsps://", "rtsps://cam:x/s", "rtsps://cam/s?transport=sctp", "rtsps://cam/s?transport=tcp&transport=tcp",
	} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}

	// No error text holds the stream key or the password.
	for _, bad := range []string{
		"rtmps://h/live/" + vS3Key + "?mode=caller",
		"rtmps+insecure://h/live/" + vS3Key + "?mode=listen",
		"rtmp+insecure://h/live/" + vS3Key,
		"RTMP+INSECURE://h/live/" + vS3Key,
		"rtmp+insecure://u:" + vP4ss + "@h/live/" + vS3Key + "?token=" + vSecret,
		"rtsps://u:" + vP4ss + "@cam/s?transport=udp",
		"rtsps+insecure://u:" + vP4ss + "@cam/s?transport=sctp",
		"rtsp+insecure://u:" + vP4ss + "@cam/s",
	} {
		_, err := Parse(bad)
		require.Error(t, err, bad)
		assert.NotContains(t, err.Error(), vS3Key, bad)
		assert.NotContains(t, err.Error(), vP4ss, bad)
		assert.NotContains(t, err.Error(), vSecret, bad)
	}
	assert.Equal(t, "rtmps://h/live/s3xxxxx", Redact("rtmps://h/live/"+vS3Key))
	assert.Equal(t, "rtmps+insecure://h/live/s3xxxxx", Redact("rtmps+insecure://h/live/"+vS3Key))
	assert.Equal(t, "rtsps+insecure://u:xxxxx@cam/s", Redact("rtsps+insecure://u:"+vP4ss+"@cam/s"))
}

// The translation of a +insecure URL: the scheme alone changes, the config
// is a copy with verification off, and the base is left as it was.
func TestTLSDial(t *testing.T) {
	base := &tls.Config{ServerName: "cam", MinVersion: tls.VersionTLS13}
	for _, c := range []struct {
		url, dial string
		insecure  bool
	}{
		{"rtmps://h/live/s?a=b:c", "rtmps://h/live/s?a=b:c", false},
		{"rtmp://h/live/s", "rtmp://h/live/s", false},
		{"rtmps+insecure://u:p@h:443/live/s?a=b:c#f", "rtmps://u:p@h:443/live/s?a=b:c#f", true},
		{"RTMPS+INSECURE://h/live/s", "rtmps://h/live/s", true},
		{"rtsps+insecure://cam/s", "rtsps://cam/s", true},
	} {
		p, err := parse(c.url)
		require.NoError(t, err, c.url)
		for _, b := range []*tls.Config{nil, base} {
			dial, cfg := p.tlsDial(c.url, b)
			assert.Equal(t, c.dial, dial, c.url)
			if !c.insecure {
				assert.Same(t, b, cfg, "%s: the base config is passed on", c.url)
				continue
			}
			require.NotNil(t, cfg, c.url)
			assert.NotSame(t, b, cfg, c.url)
			assert.True(t, cfg.InsecureSkipVerify, c.url)
			if b == nil {
				assert.EqualValues(t, tls.VersionTLS12, cfg.MinVersion, c.url)
			} else {
				assert.Equal(t, "cam", cfg.ServerName, "%s: a clone of the base", c.url)
				assert.EqualValues(t, tls.VersionTLS13, cfg.MinVersion, c.url)
			}
		}
	}
	assert.False(t, base.InsecureSkipVerify, "the base config is never modified")
}

// A source whose certificate cannot be verified is an open failure that
// names the x509 reason and no secret, and is never silently accepted;
// +insecure accepts it, for that URL only, and stays in the errors.
func TestTLSPullUntrusted(t *testing.T) {
	ca := tlstest.NewCA(t)
	other := tlstest.NewCA(t).Client() // trusts another CA only
	tags := fixtureTags(t, "h264_aac.flv")
	rtmpSrv := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{TLS: ca.Server(t)})
	rtspSrv := newRTSPServer(t, rtsptest.ServerOptions{TLS: ca.Server(t), SRAfter: 100 * time.Millisecond, Seed: 21})

	for _, c := range []struct {
		name, addr, rest string
		prefix           string // of the error of a refused open
		read             func(t *testing.T, r Reader)
	}{
		{
			"rtmps", rtmpSrv.Addr, "/live/" + vS3Key,
			"ingest: rtmp: open rtmps://" + rtmpSrv.Addr + "/live/s3xxxxx: ",
			func(t *testing.T, r Reader) {
				log := readAll(r)
				log.wait(t)
				assert.Equal(t, sourceFrames(t, tags), mediaFrames(log.frames))
				require.ErrorIs(t, log.err, io.ErrUnexpectedEOF)
				assert.Contains(t, log.err.Error(), "ingest: rtmp: read rtmps+insecure://"+rtmpSrv.Addr+"/live/s3xxxxx: ")
				reqs := rtmpSrv.Requests()
				require.Len(t, reqs, 1, "the refused opens never reached RTMP")
				assert.Equal(t, "rtmps://"+rtmpSrv.Addr+"/live", reqs[0].TCURL, "the scheme the peer sees is rtmps")
				assert.Equal(t, vS3Key, reqs[0].Stream)
			},
		},
		{
			"rtsps", rtspSrv.Addr, "/stream",
			"ingest: rtsp: open rtsps://u:xxxxx@" + rtspSrv.Addr + "/stream: ",
			func(t *testing.T, r Reader) {
				got := readRTSP(t, r, 10)
				require.NoError(t, got.err)
				checkRTSPFrames(t, got.frames)
				require.NotEmpty(t, rtspSrv.Requests())
				for _, req := range rtspSrv.Requests() {
					assert.True(t, strings.HasPrefix(req.URL, "rtsps://"+rtspSrv.Addr+"/stream"), "%s %s", req.Method, req.URL)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secure := c.name + "://u:" + vP4ss + "@" + c.addr + c.rest
			insecure := c.name + "+insecure://u:" + vP4ss + "@" + c.addr + c.rest
			if c.name == "rtmps" {
				secure, insecure = c.name+"://"+c.addr+c.rest, c.name+"+insecure://"+c.addr+c.rest
			}
			for _, env := range []Env{{}, {TLS: other}} {
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := Open(ctx, secure, env)
				cancel()
				require.Error(t, err, "an unverified source is never accepted")
				assert.Less(t, time.Since(start), 2*time.Second)
				assert.True(t, strings.HasPrefix(err.Error(), c.prefix), err.Error())
				var ua x509.UnknownAuthorityError
				assert.True(t, errors.As(err, &ua), "the x509 reason: %v", err)
				assert.NotContains(t, err.Error(), vS3Key)
				assert.NotContains(t, err.Error(), vP4ss)
			}

			// +insecure opens, with the configured base too, which stays as
			// it was.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			r := openPull(t, ctx, insecure, Env{TLS: other})
			assert.False(t, other.InsecureSkipVerify, "Env.TLS is not modified")
			assert.Equal(t, Spec{Live: true}, r.Spec())
			assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
			c.read(t, r)
			require.NoError(t, r.Close())

			// It is for that URL only: the next plain open is refused again.
			_, err := Open(ctx, secure, Env{TLS: other})
			var ua x509.UnknownAuthorityError
			require.True(t, errors.As(err, &ua), "after a +insecure open: %v", err)

			// Its errors and Redact keep the scheme as written.
			dead := c.name + "+insecure://u:" + vP4ss + "@127.0.0.1:" + freeTCPPort(t) + "/live/" + vS3Key
			_, err = Open(ctx, dead, Env{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), " open "+c.name+"+insecure://u:xxxxx@127.0.0.1:")
			if c.name == "rtmps" {
				assert.NotContains(t, err.Error(), vS3Key, "the stream key of an rtmps+insecure URL")
			}
			assert.NotContains(t, err.Error(), vP4ss)
			assert.True(t, strings.HasPrefix(Redact(insecure), c.name+"+insecure://"), Redact(insecure))
			assert.NotContains(t, Redact(dead), vP4ss)
		})
	}
}
