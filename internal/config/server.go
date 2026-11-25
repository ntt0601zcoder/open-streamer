package config

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Server is the server section: the shared servers, the transcoder, the
// assets directory and logging. A change here needs a restart (R-D4).
type Server struct {
	// HTTP is the media listener, /live/; default ":8080".
	HTTP struct {
		Listen string `yaml:"listen"`
	} `yaml:"http"`
	// API is the API listener; default "127.0.0.1:8081".
	API struct {
		Listen string `yaml:"listen"`
	} `yaml:"api"`
	// RTMP is the shared RTMP server: an empty listen turns it off; app
	// defaults to "live".
	RTMP struct {
		Listen string `yaml:"listen"`
		App    string `yaml:"app"`
	} `yaml:"rtmp"`
	// RTSP is the shared RTSP server: an empty listen turns it off; app
	// defaults to "live"; udp needs a listen.
	RTSP struct {
		Listen string `yaml:"listen"`
		App    string `yaml:"app"`
		UDP    bool   `yaml:"udp"`
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
// never looks at the file system, and leaves transcoder.binary "" as "".
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
			errs = append(errs, fmt.Errorf("%s: %q must not start or end with /", a.key, a.v))
		}
	}
	if r.RTSP.UDP && r.RTSP.Listen == "" {
		errs = append(errs, fmt.Errorf("server.rtsp.udp: needs server.rtsp.listen"))
	}
	if r.SRT.LatencyMS < 1 || r.SRT.LatencyMS > 65535 {
		errs = append(errs, fmt.Errorf("server.srt.latency_ms: %d, want 1 to 65535", r.SRT.LatencyMS))
	}
	switch r.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("server.log.level: %q, want debug, info, warn or error", r.Log.Level))
	}
	switch r.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("server.log.format: %q, want json or text", r.Log.Format))
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
		return fmt.Errorf("%q is not host:port", v)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
		return fmt.Errorf("%q: the port must be a number from 0 to 65535", v)
	}
	return nil
}

// CheckFiles looks at the file system, which Parse never does: a
// transcoder.binary that is given must be a regular file (and on Unix
// have an execute bit), and assets_dir, when given, a directory. The
// server calls it once, at start. It returns Errors or nil.
func (s Server) CheckFiles() error {
	var errs Errors
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
