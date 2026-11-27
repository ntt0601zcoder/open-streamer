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

// withTLS sets the certificate pair on s.
func withTLS(s *Server) {
	s.TLS.CertFile, s.TLS.KeyFile = "/etc/os/cert.pem", "/etc/os/key.pem"
}

func TestServerTLSCheck(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Server)
		want []string // nil means the section passes
	}{
		{"no tls and no tls_listen", func(*Server) {}, nil},
		{"tls without tls_listen", withTLS, nil},
		{"tls and http.tls_listen", func(s *Server) { withTLS(s); s.HTTP.TLSListen = ":8443" }, nil},
		{"every tls_listen", func(s *Server) {
			withTLS(s)
			s.HTTP.TLSListen, s.API.TLSListen = ":8443", "127.0.0.1:8444"
			s.RTMP.TLSListen, s.RTSP.TLSListen = ":1936", ":8555"
		}, nil},
		{"rtmps only", func(s *Server) { withTLS(s); s.RTMP.Listen, s.RTMP.TLSListen = "", ":1936" }, nil},
		{"rtsp udp with both listeners", func(s *Server) {
			withTLS(s)
			s.RTSP.Listen, s.RTSP.TLSListen, s.RTSP.UDP = ":8554", ":8555", true
		}, nil},
		{"port 0 on both", func(s *Server) {
			withTLS(s)
			s.HTTP.Listen, s.HTTP.TLSListen = "127.0.0.1:0", "127.0.0.1:0"
			s.RTMP.Listen, s.RTMP.TLSListen = ":0", ":0"
		}, nil},
		{"cert without key", func(s *Server) { s.TLS.CertFile = "c.pem" }, []string{"server.tls.cert_file: needs server.tls.key_file"}},
		{"key without cert", func(s *Server) { s.TLS.KeyFile = "k.pem" }, []string{"server.tls.key_file: needs server.tls.cert_file"}},
		{"http.tls_listen without tls", func(s *Server) { s.HTTP.TLSListen = ":8443" }, []string{"server.http.tls_listen: needs server.tls.cert_file and server.tls.key_file"}},
		{"api.tls_listen without tls", func(s *Server) { s.API.TLSListen = ":8444" }, []string{"server.api.tls_listen: needs server.tls.cert_file and server.tls.key_file"}},
		{"rtmp.tls_listen without tls", func(s *Server) { s.RTMP.TLSListen = ":1936" }, []string{"server.rtmp.tls_listen: needs server.tls.cert_file and server.tls.key_file"}},
		{"rtsp.tls_listen without tls", func(s *Server) { s.RTSP.TLSListen = ":8555" }, []string{"server.rtsp.tls_listen: needs server.tls.cert_file and server.tls.key_file"}},
		{"tls_listen with half a pair", func(s *Server) { s.TLS.KeyFile, s.HTTP.TLSListen = "k.pem", ":8443" }, []string{
			"server.tls.key_file: needs server.tls.cert_file",
			"server.http.tls_listen: needs server.tls.cert_file and server.tls.key_file",
		}},
		{"tls_listen not host:port", func(s *Server) { withTLS(s); s.API.TLSListen = "x" }, []string{`server.api.tls_listen: "x" is not host:port`}},
		{"tls_listen bad port", func(s *Server) { withTLS(s); s.RTSP.TLSListen = ":65536" }, []string{`server.rtsp.tls_listen: ":65536": the port must be a number from 0 to 65535`}},
		{"http.tls_listen is http.listen", func(s *Server) { withTLS(s); s.HTTP.TLSListen = ":8080" }, []string{`server.http.tls_listen: ":8080" is also server.http.listen`}},
		{"api.tls_listen is api.listen", func(s *Server) { withTLS(s); s.API.TLSListen = "127.0.0.1:8081" }, []string{`server.api.tls_listen: "127.0.0.1:8081" is also server.api.listen`}},
		{"rtmp.tls_listen is rtmp.listen", func(s *Server) { withTLS(s); s.RTMP.Listen, s.RTMP.TLSListen = ":1935", ":1935" }, []string{`server.rtmp.tls_listen: ":1935" is also server.rtmp.listen`}},
		{"rtsp.tls_listen is rtsp.listen", func(s *Server) { withTLS(s); s.RTSP.Listen, s.RTSP.TLSListen = ":8554", ":8554" }, []string{`server.rtsp.tls_listen: ":8554" is also server.rtsp.listen`}},
		{"rtsp udp with only tls_listen", func(s *Server) { withTLS(s); s.RTSP.TLSListen, s.RTSP.UDP = ":8555", true }, []string{"server.rtsp.udp: needs server.rtsp.listen"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s Server
			c.set(&s)
			errs := s.check()
			got := make([]string, 0, len(errs))
			for _, err := range errs {
				got = append(got, err.Error())
			}
			if c.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestServerTLSDiff(t *testing.T) {
	var s Server
	for key, set := range map[string]func(*Server){
		"server.tls.cert_file":   func(o *Server) { o.TLS.CertFile = "c.pem" },
		"server.tls.key_file":    func(o *Server) { o.TLS.KeyFile = "k.pem" },
		"server.http.tls_listen": func(o *Server) { o.HTTP.TLSListen = ":8443" },
		"server.api.tls_listen":  func(o *Server) { o.API.TLSListen = ":8444" },
		"server.rtmp.tls_listen": func(o *Server) { o.RTMP.TLSListen = ":1936" },
		"server.rtsp.tls_listen": func(o *Server) { o.RTSP.TLSListen = ":8555" },
	} {
		o := s
		set(&o)
		assert.Equal(t, []string{key}, s.Diff(o), key)
		assert.Equal(t, []string{key}, o.Diff(s), key)
	}
	// None has a default: Resolved leaves them as given.
	r := s.Resolved()
	assert.Empty(t, r.TLS.CertFile+r.TLS.KeyFile+r.HTTP.TLSListen+r.API.TLSListen+r.RTMP.TLSListen+r.RTSP.TLSListen)
	var o Server
	withTLS(&o)
	o.HTTP.TLSListen, o.API.TLSListen, o.RTMP.TLSListen, o.RTSP.TLSListen = "a:1", "b:2", "c:3", "d:4"
	r = o.Resolved()
	assert.Equal(t, o.TLS, r.TLS)
	assert.Equal(t, []string{"a:1", "b:2", "c:3", "d:4"}, []string{r.HTTP.TLSListen, r.API.TLSListen, r.RTMP.TLSListen, r.RTSP.TLSListen})
}

func TestServerTLSCheckFiles(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(cert, []byte("not a certificate"), 0o600))
	require.NoError(t, os.WriteFile(key, []byte("not a key"), 0o600))

	var s Server
	s.TLS.CertFile, s.TLS.KeyFile = cert, key
	require.NoError(t, s.CheckFiles(), "the pair itself is server.New's check")

	s.TLS.CertFile = filepath.Join(dir, "missing")
	require.ErrorContains(t, s.CheckFiles(), "server.tls.cert_file: ")
	s.TLS.CertFile = dir
	require.EqualError(t, s.CheckFiles(), "server.tls.cert_file: "+dir+" is not a regular file")

	s.TLS.CertFile = cert
	s.TLS.KeyFile = filepath.Join(dir, "missing")
	require.ErrorContains(t, s.CheckFiles(), "server.tls.key_file: ")
	s.TLS.KeyFile = dir
	require.EqualError(t, s.CheckFiles(), "server.tls.key_file: "+dir+" is not a regular file")

	s.TLS.CertFile = filepath.Join(dir, "missing")
	var es Errors
	require.ErrorAs(t, s.CheckFiles(), &es)
	assert.Len(t, es, 2)
}

func TestParseTLSKeys(t *testing.T) {
	p, err := Parse([]byte(head()+`server:
  tls: {cert_file: /etc/os/cert.pem, key_file: /etc/os/key.pem}
  http: {listen: ":80", tls_listen: ":443"}
  api: {listen: "127.0.0.1:8081", tls_listen: ":8444"}
  rtmp: {listen: ":1935", tls_listen: ":1936"}
  rtsp: {listen: ":8554", tls_listen: ":8555", udp: true}
`), testOpt)
	require.NoError(t, err)
	assert.Equal(t, "/etc/os/cert.pem", p.Doc.Server.TLS.CertFile)
	assert.Equal(t, "/etc/os/key.pem", p.Doc.Server.TLS.KeyFile)
	assert.Equal(t, ":443", p.Doc.Server.HTTP.TLSListen)
	assert.Equal(t, ":8444", p.Doc.Server.API.TLSListen)
	assert.Equal(t, ":1936", p.Doc.Server.RTMP.TLSListen)
	assert.Equal(t, ":8555", p.Doc.Server.RTSP.TLSListen)

	_, err = Parse([]byte(head()+"server: {tls: {cert: x}}\n"), testOpt)
	var se *SyntaxError
	require.ErrorAs(t, err, &se)
	assert.Contains(t, err.Error(), "cert")
}
