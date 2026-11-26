package config

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// Server is the server section: the shared servers, the certificate, the
// transcoder, the assets directory and logging. A change here needs a
// restart (R-D4); only the content of the certificate files is taken up
// without one.
//
// Each tls_listen is a TLS listener beside its plain sibling, serving the
// same thing with the server.tls certificate. None has a default: ""
// turns it off.
type Server struct {
	// TLS is the one certificate every TLS listener presents: cert_file
	// (PEM, may carry the chain) and key_file (PEM), used as given. Both
	// or neither; a tls_listen needs both.
	TLS struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
	// HTTP is the media listener, /live/; listen defaults to ":8080".
	HTTP struct {
		Listen    string `yaml:"listen"`
		TLSListen string `yaml:"tls_listen"`
	} `yaml:"http"`
	// API is the API listener; listen defaults to "127.0.0.1:8081".
	API struct {
		Listen    string `yaml:"listen"`
		TLSListen string `yaml:"tls_listen"`
	} `yaml:"api"`
	// RTMP is the shared RTMP server: an empty listen turns the plain
	// listener off; app defaults to "live" and is shared by both.
	RTMP struct {
		Listen    string `yaml:"listen"`
		TLSListen string `yaml:"tls_listen"`
		App       string `yaml:"app"`
	} `yaml:"rtmp"`
	// RTSP is the shared RTSP server: an empty listen turns the plain
	// listener off; app defaults to "live" and is shared by both; udp
	// applies to the plain listener and needs its listen.
	RTSP struct {
		Listen    string `yaml:"listen"`
		TLSListen string `yaml:"tls_listen"`
		App       string `yaml:"app"`
		UDP       bool   `yaml:"udp"`
	} `yaml:"rtsp"`
	// SRT is the shared SRT server: an empty listen turns it off; app
	// defaults to "live"; latency_ms defaults to 120, from 1 to 65535.
	SRT struct {
		Listen    string `yaml:"listen"`
		App       string `yaml:"app"`
		LatencyMS int    `yaml:"latency_ms"`
	} `yaml:"srt"`
	// Transcoder.Binary is the transcoder executable. "" means that
	// server.New looks once for ostranscoder next to the executable;
	// Parse and Resolved leave "" as it is.
	Transcoder struct {
		Binary string `yaml:"binary"`
	} `yaml:"transcoder"`
	// AssetsDir is the assets directory; "" means no assets.
	AssetsDir string `yaml:"assets_dir"`
	// Log is the log level (debug, info, warn, error; default info) and
	// format (json, text; default json).
	Log struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"log"`
}

// The defaults of the server section.
const (
	defaultHTTPListen = ":8080"
	defaultAPIListen  = "127.0.0.1:8081"
	defaultApp        = "live"
	defaultLatencyMS  = 120
	defaultLogLevel   = "info"
	defaultLogFormat  = "json"
)

// Resolved returns s with the defaults filled in, from constants only: it
// never looks at the file system, and leaves transcoder.binary, the
// tls files and every tls_listen as given.
func (s Server) Resolved() Server {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&s.HTTP.Listen, defaultHTTPListen)
	def(&s.API.Listen, defaultAPIListen)
	def(&s.RTMP.App, defaultApp)
	def(&s.RTSP.App, defaultApp)
	def(&s.SRT.App, defaultApp)
	if s.SRT.LatencyMS == 0 {
		s.SRT.LatencyMS = defaultLatencyMS
	}
	def(&s.Log.Level, defaultLogLevel)
	def(&s.Log.Format, defaultLogFormat)
	return s
}

// Diff returns the dotted keys whose resolved values differ, sorted, as
// "server.rtmp.listen".
func (s Server) Diff(o Server) []string {
	a, b := map[string]string{}, map[string]string{}
	flatten("server", reflect.ValueOf(s.Resolved()), a)
	flatten("server", reflect.ValueOf(o.Resolved()), b)
	var keys []string
	for k, v := range a {
		if b[k] != v {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// flatten adds every leaf of the struct v to out, keyed by its dotted
// yaml path under prefix.
func flatten(prefix string, v reflect.Value, out map[string]string) {
	t := v.Type()
	for i := range t.NumField() {
		key := prefix + "." + strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if f := v.Field(i); f.Kind() == reflect.Struct {
			flatten(key, f, out)
		} else {
			out[key] = fmt.Sprint(f.Interface())
		}
	}
}

// check returns every error of the section, each naming its key.
func (s Server) check() []error {
	r := s.Resolved()
	var errs []error
	for _, l := range []struct{ key, v string }{
		{"server.http.listen", r.HTTP.Listen},
		{"server.api.listen", r.API.Listen},
		{"server.rtmp.listen", r.RTMP.Listen},
		{"server.rtsp.listen", r.RTSP.Listen},
		{"server.srt.listen", r.SRT.Listen},
	} {
		if err := checkListen(l.v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.key, err))
		}
	}
	for _, a := range []struct{ key, v string }{
		{"server.rtmp.app", r.RTMP.App},
		{"server.rtsp.app", r.RTSP.App},
		{"server.srt.app", r.SRT.App},
	} {
		if strings.HasPrefix(a.v, "/") || strings.HasSuffix(a.v, "/") {
			errs = append(errs, fmt.Errorf("%s: %s must not start or end with /", a.key, spec.Quote(a.v)))
		}
	}
	errs = append(errs, r.checkTLS()...)
	if r.RTSP.UDP && r.RTSP.Listen == "" {
		errs = append(errs, fmt.Errorf("server.rtsp.udp: needs server.rtsp.listen"))
	}
	if r.SRT.LatencyMS < 1 || r.SRT.LatencyMS > 65535 {
		errs = append(errs, fmt.Errorf("server.srt.latency_ms: %d, want 1 to 65535", r.SRT.LatencyMS))
	}
	switch r.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("server.log.level: %s, want debug, info, warn or error", spec.Quote(r.Log.Level)))
	}
	switch r.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("server.log.format: %s, want json or text", spec.Quote(r.Log.Format)))
	}
	return errs
}

// checkTLS returns the errors of server.tls and the tls_listen keys of
// the resolved section r: the pair is both files or neither, a tls_listen
// is host:port and needs the pair, and it is not the same text as its
// plain sibling's listen unless the port is 0.
func (r Server) checkTLS() []error {
	var errs []error
	hasPair := r.TLS.CertFile != "" && r.TLS.KeyFile != ""
	switch {
	case r.TLS.CertFile != "" && r.TLS.KeyFile == "":
		errs = append(errs, fmt.Errorf("server.tls.cert_file: needs server.tls.key_file"))
	case r.TLS.KeyFile != "" && r.TLS.CertFile == "":
		errs = append(errs, fmt.Errorf("server.tls.key_file: needs server.tls.cert_file"))
	}
	for _, l := range []struct{ name, listen, tls string }{
		{"http", r.HTTP.Listen, r.HTTP.TLSListen},
		{"api", r.API.Listen, r.API.TLSListen},
		{"rtmp", r.RTMP.Listen, r.RTMP.TLSListen},
		{"rtsp", r.RTSP.Listen, r.RTSP.TLSListen},
	} {
		if l.tls == "" {
			continue
		}
		key := "server." + l.name + ".tls_listen"
		err := checkListen(l.tls)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
		if !hasPair {
			errs = append(errs, fmt.Errorf("%s: needs server.tls.cert_file and server.tls.key_file", key))
		}
		if err == nil && l.tls == l.listen {
			if _, port, _ := net.SplitHostPort(l.tls); strings.TrimLeft(port, "0") != "" {
				errs = append(errs, fmt.Errorf("%s: %s is also server.%s.listen", key, spec.Quote(l.tls), l.name))
			}
		}
	}
	return errs
}

// checkListen accepts "" and host:port with a decimal port from 0 to
// 65535; the host may be empty.
func checkListen(v string) error {
	if v == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%s is not host:port", spec.Quote(v))
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
		return fmt.Errorf("%s: the port must be a number from 0 to 65535", spec.Quote(v))
	}
	return nil
}

// CheckFiles looks at the file system, which Parse never does: a
// transcoder.binary that is given must be a regular file (and on Unix
// have an execute bit), assets_dir, when given, a directory, and
// tls.cert_file and tls.key_file, when given, regular files. It never
// reads a file: whether the pair loads is the server's check. The server
// calls it once, at start. It returns Errors or nil.
func (s Server) CheckFiles() error {
	var errs Errors
	for _, f := range []struct{ key, path string }{
		{"server.tls.cert_file", s.TLS.CertFile},
		{"server.tls.key_file", s.TLS.KeyFile},
	} {
		if f.path == "" {
			continue
		}
		fi, err := os.Stat(f.path)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", f.key, err))
		case !fi.Mode().IsRegular():
			errs = append(errs, fmt.Errorf("%s: %s is not a regular file", f.key, f.path))
		}
	}
	if b := s.Transcoder.Binary; b != "" {
		fi, err := os.Stat(b)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("server.transcoder.binary: %w", err))
		case !fi.Mode().IsRegular():
			errs = append(errs, fmt.Errorf("server.transcoder.binary: %s is not a regular file", b))
		case !executable(fi.Mode()):
			errs = append(errs, fmt.Errorf("server.transcoder.binary: %s is not executable", b))
		}
	}
	if d := s.AssetsDir; d != "" {
		fi, err := os.Stat(d)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("server.assets_dir: %w", err))
		case !fi.IsDir():
			errs = append(errs, fmt.Errorf("server.assets_dir: %s is not a directory", d))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}
