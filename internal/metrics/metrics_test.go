package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics/metricstest"
)

func scrape(t *testing.T, r *metrics.Registry, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, "/metrics", nil))
	return rec
}

func TestWriterGolden(t *testing.T) {
	var r metrics.Registry
	r.Register(func(w *metrics.Writer) {
		w.Counter("x_total", "X \\ help\nline.", 1, metrics.L("a", "1"))
		w.Gauge("g", "A gauge.", 2.5)
		w.Counter("x_total", "X \\ help\nline.", 3, metrics.L("a", "a\"b\\c\n"))
	})
	rec := scrape(t, &r, http.MethodGet)
	want := "# HELP x_total X \\\\ help\\nline.\n# TYPE x_total counter\n" +
		"x_total{a=\"1\"} 1\nx_total{a=\"a\\\"b\\\\c\\n\"} 3\n" +
		"# HELP g A gauge.\n# TYPE g gauge\ng 2.5\n"
	if rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("code %d body:\n%q\nwant:\n%q", rec.Code, rec.Body.String(), want)
	}
}

func TestWriterRejects(t *testing.T) {
	cases := map[string]func(*metrics.Writer){
		"fam_total": func(w *metrics.Writer) {
			w.Counter("fam_total", "h", 1)
			w.Gauge("fam_total", "h", 1)
		},
		"fam2": func(w *metrics.Writer) {
			w.Gauge("fam2", "h1", 1)
			w.Gauge("fam2", "h2", 1, metrics.L("a", "b"))
		},
		"nototal":  func(w *metrics.Writer) { w.Counter("nototal", "h", 1) },
		"bad-name": func(w *metrics.Writer) { w.Gauge("bad-name", "h", 1) },
		"lbl":      func(w *metrics.Writer) { w.Gauge("lbl", "h", 1, metrics.L("0x", "v")) },
		"dup": func(w *metrics.Writer) {
			w.Gauge("dup", "h", 1, metrics.L("a", "1"), metrics.L("b", "2"))
			w.Gauge("dup", "h", 2, metrics.L("b", "2"), metrics.L("a", "1"))
		},
	}
	for fam, f := range cases {
		t.Run(fam, func(t *testing.T) {
			var r metrics.Registry
			r.Register(func(w *metrics.Writer) { w.Gauge("fine", "ok", 1) })
			r.Register(f)
			rec := scrape(t, &r, http.MethodGet)
			if rec.Code != 500 || !strings.Contains(rec.Body.String(), fam) || strings.Contains(rec.Body.String(), "# HELP") {
				t.Fatalf("code %d body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerParses(t *testing.T) {
	var r metrics.Registry
	r.Register(func(w *metrics.Writer) { w.Gauge("a", "A.", 1, metrics.L("s", "x")) })
	r.Register(func(w *metrics.Writer) { w.Counter("b_total", "B.", 2) })
	r.Register(func(w *metrics.Writer) { w.Gauge("a", "A.", 3, metrics.L("s", "y")) })
	rec := scrape(t, &r, http.MethodGet)
	s, err := metricstest.Parse(rec.Body)
	if err != nil || len(s) != 3 {
		t.Fatalf("parse: %v %v", s, err)
	}
	if v, ok := metricstest.Find(s, "a", "s", "y"); !ok || v != 3 {
		t.Fatalf("find: %v %v", v, ok)
	}
	const ct = "text/plain; version=0.0.4; charset=utf-8"
	if rec.Header().Get("Content-Type") != ct {
		t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
	}
	head := scrape(t, &r, http.MethodHead)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Type") != ct {
		t.Fatalf("head: %d %q %q", head.Code, head.Body.String(), head.Header().Get("Content-Type"))
	}
	if rec := scrape(t, &r, http.MethodPost); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", rec.Code)
	}
}

func TestParseStrict(t *testing.T) {
	bad := map[string]string{
		"no type":   "x 1\n",
		"split":     "# HELP a h\n# TYPE a gauge\na{k=\"1\"} 1\n# HELP b h\n# TYPE b gauge\nb 1\na{k=\"2\"} 1\n",
		"duplicate": "# HELP a h\n# TYPE a gauge\na{k=\"1\"} 1\na{k=\"1\"} 2\n",
		"bad label": "# HELP a h\n# TYPE a gauge\na{0k=\"1\"} 1\n",
	}
	for n, in := range bad {
		if _, err := metricstest.Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", n)
		}
	}
}

func TestFind(t *testing.T) {
	in := "# HELP x h\n# TYPE x gauge\nx{a=\"1\",b=\"1\"} 3\nx{a=\"1\",b=\"2\"} 4\n"
	s, err := metricstest.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := metricstest.Find(s, "x", "a", "1", "b", "2"); !ok || v != 4 {
		t.Errorf("exact: %v %v", v, ok)
	}
	if v, ok := metricstest.Find(s, "x", "b", "1"); !ok || v != 3 {
		t.Errorf("subset: %v %v", v, ok)
	}
	for _, c := range [][]string{{"x", "a", "1"}, {"x", "a", "9"}, {"y"}, {"x"}} {
		if _, ok := metricstest.Find(s, c[0], c[1:]...); ok {
			t.Errorf("%v found", c)
		}
	}
}
