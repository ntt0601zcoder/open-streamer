package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseServer(t *testing.T) {
	cases := []struct {
		name, server, want string // want "" means the section passes
	}{
		{"defaults", "server: {}\n", ""},
		{"every key", `server:
  http: {listen: "0.0.0.0:80"}
  api: {listen: "[::1]:9000"}
  rtmp: {listen: ":1935", app: live}
  rtsp: {listen: ":8554", app: cams, udp: true}
  srt: {listen: ":6000", app: live, latency_ms: 65535}
  transcoder: {binary: /nowhere/ostranscoder}
  assets_dir: /nowhere/assets
  log: {level: debug, format: text}
`, ""},
		{"port 0", "server: {rtmp: {listen: \"127.0.0.1:0\"}}\n", ""},
		{"listen without port", "server: {http: {listen: localhost}}\n", `server.http.listen: "localhost" is not host:port`},
		{"listen bad port", "server: {api: {listen: \"127.0.0.1:http\"}}\n", `server.api.listen: "127.0.0.1:http": the port must be a number from 0 to 65535`},
		{"listen port too big", "server: {rtmp: {listen: \":65536\"}}\n", `server.rtmp.listen: ":65536": the port must be a number from 0 to 65535`},
		{"listen negative port", "server: {srt: {listen: \":-1\"}}\n", `server.srt.listen: ":-1": the port must be a number from 0 to 65535`},
		{"rtsp listen", "server: {rtsp: {listen: \"a:b:c\"}}\n", `server.rtsp.listen: "a:b:c" is not host:port`},
		{"app leading slash", "server: {rtmp: {app: /live}}\n", `server.rtmp.app: "/live" must not start or end with /`},
		{"app trailing slash", "server: {rtsp: {app: live/}}\n", `server.rtsp.app: "live/" must not start or end with /`},
		{"srt app", "server: {srt: {app: /}}\n", `server.srt.app: "/" must not start or end with /`},
		{"udp without listen", "server: {rtsp: {udp: true}}\n", "server.rtsp.udp: needs server.rtsp.listen"},
		{"latency negative", "server: {srt: {latency_ms: -1}}\n", "server.srt.latency_ms: -1, want 1 to 65535"},
		{"latency too big", "server: {srt: {latency_ms: 65536}}\n", "server.srt.latency_ms: 65536, want 1 to 65535"},
		{"log level", "server: {log: {level: trace}}\n", `server.log.level: "trace", want debug, info, warn or error`},
		{"log format", "server: {log: {format: xml}}\n", `server.log.format: "xml", want json or text`},
		{"version", "server: {}\nversion: 2\n", "version: 2, want 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := head() + c.server
			if c.name == "version" {
				text = "users:" + head()[len("version: 1\nusers:"):] + c.server
			}
			_, err := Parse([]byte(text), testOpt)
			if c.want == "" {
				require.NoError(t, err)
				return
			}
			var es Errors
			require.ErrorAs(t, err, &es)
			require.Len(t, es, 1, err.Error())
			assert.Equal(t, c.want, es[0].Error())
		})
	}
}

func TestServerResolvedAndDiff(t *testing.T) {
	var s Server
	r := s.Resolved()
	assert.Equal(t, r, r.Resolved())
	assert.Empty(t, s.Diff(Server{}))
	assert.Empty(t, s.Diff(r), "a default written out is no difference")

	o := s
	o.RTMP.Listen = ":1935"
	o.SRT.LatencyMS = 200
	o.Log.Level = "debug"
	o.Transcoder.Binary = "/usr/bin/ostranscoder"
	o.RTSP.UDP = true
	assert.Equal(t, []string{
		"server.log.level",
		"server.rtmp.listen",
		"server.rtsp.udp",
		"server.srt.latency_ms",
		"server.transcoder.binary",
	}, s.Diff(o))
	assert.Equal(t, s.Diff(o), o.Diff(s))

	o = s
	o.AssetsDir = "/srv/assets"
	o.HTTP.Listen = ":80"
	o.API.Listen = ":81"
	o.RTSP.App = "x"
	o.SRT.App = "y"
	o.RTMP.App = "z"
	o.RTSP.Listen = ":554"
	o.SRT.Listen = ":6000"
	o.Log.Format = "text"
	assert.Equal(t, []string{
		"server.api.listen",
		"server.assets_dir",
		"server.http.listen",
		"server.log.format",
		"server.rtmp.app",
		"server.rtsp.app",
		"server.rtsp.listen",
		"server.srt.app",
		"server.srt.listen",
	}, s.Diff(o))
}

func TestServerCheckFiles(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "ostranscoder")
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700))
	plain := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(plain, []byte("x"), 0o600))

	var s Server
	require.NoError(t, s.CheckFiles(), "nothing given is nothing to check")

	s.Transcoder.Binary = exe
	s.AssetsDir = dir
	require.NoError(t, s.CheckFiles())

	s.Transcoder.Binary = filepath.Join(dir, "missing")
	require.ErrorContains(t, s.CheckFiles(), "server.transcoder.binary: ")

	s.Transcoder.Binary = dir
	require.ErrorContains(t, s.CheckFiles(), "server.transcoder.binary: "+dir+" is not a regular file")

	s.Transcoder.Binary = plain
	if runtime.GOOS == "windows" {
		require.NoError(t, s.CheckFiles())
	} else {
		require.ErrorContains(t, s.CheckFiles(), "server.transcoder.binary: "+plain+" is not executable")
	}

	s.Transcoder.Binary = exe
	s.AssetsDir = exe
	require.ErrorContains(t, s.CheckFiles(), "server.assets_dir: "+exe+" is not a directory")
	s.AssetsDir = filepath.Join(dir, "missing")
	require.ErrorContains(t, s.CheckFiles(), "server.assets_dir: ")

	s.Transcoder.Binary = dir
	var es Errors
	require.ErrorAs(t, s.CheckFiles(), &es)
	assert.Len(t, es, 2)
}
