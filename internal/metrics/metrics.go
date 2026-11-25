package metrics

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const contentType = "text/plain; version=0.0.4; charset=utf-8"

// Label is one name and value pair of a sample.
type Label struct{ Name, Value string }

// L returns a Label.
func L(name, value string) Label { return Label{Name: name, Value: value} }

type family struct {
	name, help, typ string
	lines           []string
	series          map[string]struct{}
}

// Writer collects the samples of one scrape. The first mistake is kept and
// later samples are ignored; Registry.Handler turns it into a 500.
type Writer struct {
	order []*family
	byKey map[string]*family
	err   error
}

// Counter writes a sample of a counter family; the name must end in _total.
func (w *Writer) Counter(name, help string, v float64, labels ...Label) {
	w.add(name, help, "counter", v, labels)
}

// Gauge writes a sample of a gauge family.
func (w *Writer) Gauge(name, help string, v float64, labels ...Label) {
	w.add(name, help, "gauge", v, labels)
}

func (w *Writer) fail(name, format string, args ...any) {
	if w.err == nil {
		w.err = fmt.Errorf("family %s: %s", name, fmt.Sprintf(format, args...))
	}
}

func (w *Writer) add(name, help, typ string, v float64, labels []Label) {
	if w.err != nil {
		return
	}
	if !validName(name, true) {
		w.fail(name, "illegal metric name")
		return
	}
	if typ == "counter" && !strings.HasSuffix(name, "_total") {
		w.fail(name, "counter name must end in _total")
		return
	}
	for _, l := range labels {
		if !validName(l.Name, false) || strings.HasPrefix(l.Name, "__") {
			w.fail(name, "illegal label name %q", l.Name)
			return
		}
	}
	sorted := append([]Label(nil), labels...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var key strings.Builder
	for i, l := range sorted {
		if i > 0 && sorted[i-1].Name == l.Name {
			w.fail(name, "label %q given twice", l.Name)
			return
		}
		key.WriteString(l.Name)
		key.WriteByte('=')
		key.WriteString(strconv.Quote(l.Value))
		key.WriteByte(',')
	}
	f := w.byKey[name]
	if f == nil {
		if w.byKey == nil {
			w.byKey = map[string]*family{}
		}
		f = &family{name: name, help: help, typ: typ, series: map[string]struct{}{}}
		w.byKey[name] = f
		w.order = append(w.order, f)
	}
	if f.typ != typ {
		w.fail(name, "used as both %s and %s", f.typ, typ)
		return
	}
	if f.help != help {
		w.fail(name, "two help texts")
		return
	}
	if _, dup := f.series[key.String()]; dup {
		w.fail(name, "series written twice")
		return
	}
	f.series[key.String()] = struct{}{}

	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.Name)
			b.WriteString(`="`)
			b.WriteString(escape(l.Value, true))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	f.lines = append(f.lines, b.String())
}

func (w *Writer) render() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	var buf bytes.Buffer
	for _, f := range w.order {
		fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s %s\n", f.name, escape(f.help, false), f.name, f.typ)
		for _, l := range f.lines {
			buf.WriteString(l)
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}

func escape(s string, quote bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '"' && quote:
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validName(s string, colon bool) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (colon && r == ':')
		if i > 0 && r >= '0' && r <= '9' {
			ok = true
		}
		if !ok {
			return false
		}
	}
	return true
}

// Registry holds the collectors; the zero value is ready.
type Registry struct {
	mu         sync.Mutex
	collectors []func(*Writer)
}

// Register adds a collector; collectors run in registration order at every scrape.
func (r *Registry) Register(collect func(*Writer)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, collect)
}

// Handler serves the exposition on GET and HEAD; other methods get 405.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			rw.Header().Set("Allow", "GET, HEAD")
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.mu.Lock()
		cs := slices.Clone(r.collectors)
		r.mu.Unlock()
		var w Writer
		for _, c := range cs {
			c(&w)
		}
		body, err := w.render()
		if err != nil {
			http.Error(rw, "metrics: "+err.Error(), http.StatusInternalServerError)
			return
		}
		rw.Header().Set("Content-Type", contentType)
		rw.Header().Set("Content-Length", strconv.Itoa(len(body)))
		rw.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = rw.Write(body)
		}
	})
}
