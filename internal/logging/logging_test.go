package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewLevelsAndFormats(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "", "")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("dropped")
	log.Info("hello", "n", 1)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want exactly one", lines)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, lines[0])
	}
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %s", k, lines[0])
		}
	}

	buf.Reset()
	log, err = New(&buf, "debug", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hi")
	if out := buf.String(); !strings.Contains(out, "msg=hi") || json.Valid([]byte(out)) {
		t.Errorf("text output = %q", out)
	}

	for _, c := range [][2]string{{"loud", "json"}, {"info", "xml"}} {
		if _, err := New(&buf, c[0], c[1]); err == nil {
			t.Errorf("New(%q, %q) succeeded, want error", c[0], c[1])
		}
	}
	for _, lv := range []string{"debug", "info", "warn", "error"} {
		if _, err := New(&buf, lv, "json"); err != nil {
			t.Errorf("level %q: %v", lv, err)
		}
	}
}

func TestScrub(t *testing.T) {
	in := `open rtsp://u:hunter2@cam/1 failed, then srt://h:9?passphrase=topsecret&x=1`
	out := Scrub(in)
	for _, s := range []string{"hunter2", "topsecret"} {
		if strings.Contains(out, s) {
			t.Errorf("%q leaked in %q", s, out)
		}
	}
	for _, s := range []string{"cam/1", "x=1"} {
		if !strings.Contains(out, s) {
			t.Errorf("%q lost in %q", s, out)
		}
	}

	if out := Scrub("rtmp://h/live/secretkey"); strings.Contains(out, "secretkey") {
		t.Errorf("rtmp key leaked: %q", out)
	}

	out = Scrub(`rtsp://u:pa'ss"w<d@cam/1 down`)
	for _, s := range []string{"pa'ss", `"w`, "w<d", "ss", "pa'"} {
		if strings.Contains(out, s) {
			t.Errorf("password piece %q leaked in %q", s, out)
		}
	}
	if !strings.HasSuffix(out, " down") {
		t.Errorf("trailing word lost: %q", out)
	}

	const plain = "no url here: a:b/c"
	if got := Scrub(plain); got != plain {
		t.Errorf("Scrub(plain) = %q", got)
	}
}

type valuer struct{ u string }

func (v valuer) LogValue() slog.Value { return slog.StringValue("via " + v.u) }

func TestHandlerScrubs(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	u1 := "rtsp://a:pw1x@host1/1"
	u2 := "rtsp://b:pw2x@host2/1"
	u3 := "rtsp://c:pw3x@host3/1"
	u4 := "rtsp://d:pw4x@host4/1"
	u5 := "rtsp://e:pw5x@host5/1"
	log.With("url", u1).Error("failed "+u2, "err", fmt.Errorf("dial %s: refused", u3),
		slog.Group("g", "url", u4), "Token", "abc123", "password", "pw",
		"v", valuer{u5}, slog.Group("authorization", "x", "y"))
	out := buf.String()
	for _, s := range []string{"pw1x", "pw2x", "pw3x", "pw4x", "pw5x", "abc123", `"pw"`, `"y"`} {
		if strings.Contains(out, s) {
			t.Errorf("%q leaked: %s", s, out)
		}
	}
	for i := 1; i <= 5; i++ {
		if h := fmt.Sprintf("host%d", i); !strings.Contains(out, h) {
			t.Errorf("%s lost: %s", h, out)
		}
	}

	// WithGroup then WithAttrs still scrubs.
	buf.Reset()
	log.WithGroup("w").With("secret", "s3", "u", u1).Info("x")
	if out := buf.String(); strings.Contains(out, "s3") || strings.Contains(out, "pw1x") {
		t.Errorf("leak via WithGroup: %s", out)
	}
	if !log.Handler().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("Enabled should delegate")
	}
}

func TestHandlerKeepsStructure(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	log.Info("m", "i", 42, "b", true, "d", 1500*time.Millisecond, "t", ts, "f", 1.5,
		slog.Group("grp", "k", 7))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["i"] != float64(42) || m["b"] != true || m["d"] != float64(1.5e9) || m["f"] != 1.5 {
		t.Errorf("values changed: %v", m)
	}
	if m["t"] != "2026-01-02T03:04:05Z" {
		t.Errorf("time = %v", m["t"])
	}
	g, ok := m["grp"].(map[string]any)
	if !ok || g["k"] != float64(7) {
		t.Errorf("group = %v", m["grp"])
	}
}

type strg struct{ u string }

func (s strg) String() string { return "dial " + s.u }

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg }

type holder struct {
	Name string
	URL  string
}

func TestHandlerScrubsAnyValues(t *testing.T) {
	const pw = "rtsp://u:pwsecret@cam/1?token=qsecret"
	pu, err := url.Parse(pw)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			log, err := New(&buf, "info", format)
			if err != nil {
				t.Fatal(err)
			}
			log.Info("m", "u", pu, "s", strg{pw}, "l", []string{pw}, "h", holder{"n", pw},
				"mp", map[string]string{"k": pw}, "k", "v", "api_key", "ak", "ok", []int{1, 2})
			out := buf.String()
			for _, s := range []string{"pwsecret", "qsecret", `"ak"`, "=ak"} {
				if strings.Contains(out, s) {
					t.Errorf("%q leaked: %s", s, out)
				}
			}
			if strings.Count(out, "cam/1") != 5 {
				t.Errorf("hosts lost: %s", out)
			}
			if !strings.Contains(out, "[1 2]") && !strings.Contains(out, "[1,2]") {
				t.Errorf("plain slice changed: %s", out)
			}
		})
	}
}

func TestHandlerTypedNil(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "info", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("m", "u", (*url.URL)(nil), "e", error((*nilErr)(nil)))
	out := buf.String()
	if strings.Count(out, "<nil>") != 2 {
		t.Errorf("output = %s", out)
	}
}

func TestHandlerScrubsURLValue(t *testing.T) {
	u := url.URL{Scheme: "srt", Host: "h:9", RawQuery: "passphrase=qsecret&x=1", Path: "/live/pathsecret"}
	r := url.URL{Scheme: "rtmp", Host: "h", Path: "/live/streamkeyval"}
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		log, err := New(&buf, "info", format)
		if err != nil {
			t.Fatal(err)
		}
		log.Info("m", "u", u, "r", r)
		out := buf.String()
		for _, s := range []string{"qsecret", "streamkeyval"} {
			if strings.Contains(out, s) {
				t.Errorf("%s: %q leaked: %s", format, s, out)
			}
		}
		if !strings.Contains(out, "x=1") {
			t.Errorf("%s: x=1 lost: %s", format, out)
		}
	}
}
