package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// mask replaces the value of an attribute with a secret-like name.
const mask = "xxxxx"

// urlLike matches a URL-shaped substring. It runs to the next white space
// because net/url accepts quotes and most punctuation in the userinfo, so
// stopping earlier would leave the tail of a password in the log.
var urlLike = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)

// secretKeys are the lower-cased attribute names whose values are always hidden.
var secretKeys = map[string]bool{
	"token": true, "password": true, "passphrase": true, "secret": true, "authorization": true,
}

// New returns a logger writing to w. level is "debug", "info", "warn" or
// "error" ("" means info); format is "json" or "text" ("" means json).
// Anything else is an error.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch level {
	case "", "info":
		lv = slog.LevelInfo
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("logging: level: unknown level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	var inner slog.Handler
	switch format {
	case "", "json":
		inner = slog.NewJSONHandler(w, opts)
	case "text":
		inner = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: format: unknown format %q", format)
	}
	return slog.New(&handler{inner: inner}), nil
}

// Scrub returns s with every URL-shaped substring passed through redact.URL.
func Scrub(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlLike.ReplaceAllStringFunc(s, redact.URL)
}

// handler scrubs every record before the wrapped handler writes it.
type handler struct {
	inner slog.Handler
}

// Enabled delegates to the wrapped handler.
func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle writes r with its message and attributes scrubbed.
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(scrubAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

// WithAttrs scrubs attrs before handing them to the wrapped handler.
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = scrubAttr(a)
	}
	return &handler{inner: h.inner.WithAttrs(out)}
}

// WithGroup delegates to the wrapped handler.
func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name)}
}

// scrubAttr returns a with LogValuers resolved, strings and errors scrubbed,
// groups scrubbed recursively and a secret-named value replaced by mask.
func scrubAttr(a slog.Attr) slog.Attr {
	if secretKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, mask)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Scrub(v.String()))
	case slog.KindGroup:
		src := v.Group()
		dst := make([]slog.Attr, len(src))
		for i, g := range src {
			dst[i] = scrubAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(dst...)}
	case slog.KindAny:
		if err, ok := v.Any().(error); ok && err != nil {
			return slog.String(a.Key, Scrub(err.Error()))
		}
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64,
		slog.KindTime, slog.KindUint64, slog.KindLogValuer:
	}
	return slog.Attr{Key: a.Key, Value: v}
}
