package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixture = "../../testdata/fixtures/h264_aac.ts"

func httpGet(u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func TestOsdevServes(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, []string{"-listen", "127.0.0.1:0", "-stream", "demo=" + fixture, "-loop"}, pw)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	require.True(t, sc.Scan())
	first := sc.Text()
	require.True(t, strings.HasPrefix(first, "listening on 127.0.0.1:"), first)
	addr := strings.TrimPrefix(first, "listening on ")
	var urls []string
	for i := 0; i < 4 && sc.Scan(); i++ {
		urls = append(urls, sc.Text())
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	require.Len(t, urls, 4)
	assert.Equal(t, "http://"+addr+"/live/demo/hls/index.m3u8", urls[0])
	assert.True(t, strings.HasSuffix(urls[3], "/live/demo/stream.ts"))

	get := func(path string) (int, string) {
		resp, err := httpGet("http://" + addr + path)
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	require.Eventually(t, func() bool {
		st, body := get("/live/demo/hls/index.m3u8")
		return st == 200 && strings.Contains(body, ".ts")
	}, 15*time.Second, 50*time.Millisecond)
	st, _ := get("/live/nope/hls/index.m3u8")
	assert.Equal(t, 404, st)
	st, _ = get("/live/nope/stream.ts")
	assert.Equal(t, 404, st)

	// an open HTTP-TS response must not hold up shutdown
	resp, err := httpGet("http://" + addr + "/live/demo/stream.ts")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	cancel()
	select {
	case c := <-code:
		assert.Equal(t, 0, c)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

func TestOsdevBadArgs(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	for name, args := range map[string][]string{
		"bad flag":    {"-nope"},
		"extra arg":   {"-stream", "a=" + fixture, "x"},
		"no stream":   {},
		"no equals":   {"-stream", "demo"},
		"empty name":  {"-stream", "=x.ts"},
		"empty path":  {"-stream", "demo="},
		"slash name":  {"-stream", "a/b=x.ts"},
		"duplicate":   {"-stream", "a=x.ts", "-stream", "a=y.ts"},
		"port in use": {"-listen", ln.Addr().String(), "-stream", "a=" + fixture},
	} {
		t.Run(name, func(t *testing.T) {
			var sb strings.Builder
			assert.Equal(t, 2, run(context.Background(), args, &sb))
			assert.NotContains(t, sb.String(), "listening")
		})
	}
}

func TestOsdevMissingFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var sb strings.Builder
	assert.Equal(t, 0, run(ctx, []string{"-listen", "127.0.0.1:0", "-stream", "a=/nonexistent.ts"}, &sb))
}
