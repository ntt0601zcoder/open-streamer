package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestRedact(t *testing.T) {
	r := Redact("srt://u:p@h:1?" + kPass + "=" + vS + "&streamid=x")
	assert.NotContains(t, r, "p@")
	assert.NotContains(t, r, kPass+"="+vS)
	assert.NotContains(t, r, "=s&")
	assert.Contains(t, r, "streamid=x")
	assert.Contains(t, r, "h:1")
	assert.Equal(t, "srt://h:1?b=1&"+kPass+"=xxxxx&a=2", Redact("srt://h:1?b=1&"+kPass+"="+vS+"&a=2"), "order is kept")
	assert.Equal(t, "srt://h:1?PassPhrase=xxxxx", Redact("srt://h:1?PassPhrase="+vS), "keys are case-insensitive")
	assert.Equal(t, "udp://@:5000", Redact("udp://@:5000"))
	assert.Equal(t, "/tmp/a.ts", Redact("/tmp/a.ts"))
	assert.Equal(t, "http://h/x", Redact("http://h/x"))
	assert.NotContains(t, Redact("http://h/x?token=abc"), "abc")
	assert.NotContains(t, Redact("http://u@h/x"), "u@")
	assert.NotContains(t, Redact("http://[::1"), "::1")
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
