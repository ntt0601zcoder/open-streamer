package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// parseWatermark parses a -watermark value, name=KEY=VALUE[,KEY=VALUE...],
// with the keys pos, opacity, size, tz, image and text. text must be the
// last key and takes the rest of the value verbatim, commas included. An
// image is an asset name, read from the assets directory.
func parseWatermark(v, assets string) (stream string, w picture.Watermark, err error) {
	const want = "want name=KEY=VALUE[,KEY=VALUE...] with keys pos, opacity, size, tz, image, text"
	stream, rest, ok := strings.Cut(v, "=")
	if !ok || !validName(stream) || rest == "" {
		return "", w, fmt.Errorf("malformed -watermark %q, %s", v, want)
	}
	seen := map[string]bool{}
	var asset string
	for rest != "" {
		var kv string
		if strings.HasPrefix(rest, "text=") {
			kv, rest = rest, "" // the rest of the value, commas included
		} else {
			kv, rest, _ = strings.Cut(rest, ",")
		}
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			return "", w, fmt.Errorf("-watermark %q: %q is not KEY=VALUE", v, kv)
		}
		if seen[key] {
			return "", w, fmt.Errorf("-watermark %q: key %s given twice", v, key)
		}
		seen[key] = true
		switch key {
		case "pos":
			if w.Position, ok = positions[val]; !ok {
				return "", w, fmt.Errorf("-watermark %q: pos %q is not tl, tr, bl, br or c", v, val)
			}
		case "opacity":
			w.OpacityPct, err = strconv.Atoi(val)
		case "size":
			w.SizePct, err = strconv.Atoi(val)
		case "tz":
			w.TimeZone = val
		case "image":
			asset = val
		case "text":
			w.Text = val
		default:
			return "", w, fmt.Errorf("-watermark %q: unknown key %q", v, key)
		}
		if err != nil {
			return "", w, fmt.Errorf("-watermark %q: %s must be an integer", v, key)
		}
	}
	if asset != "" {
		if w.Text != "" {
			return "", w, fmt.Errorf("-watermark %q: text and image are exclusive", v)
		}
		if assets == "" {
			return "", w, fmt.Errorf("-watermark %q: image needs -assets", v)
		}
		if w.Image, err = picture.LoadAsset(assets, asset); err != nil {
			return "", w, fmt.Errorf("-watermark %q: %w", v, err)
		}
	}
	if err := w.Validate(); err != nil {
		return "", w, fmt.Errorf("-watermark %q: %w", v, err)
	}
	return stream, w, nil
}

var positions = map[string]picture.Position{
	"tl": picture.TopLeft, "tr": picture.TopRight, "bl": picture.BottomLeft, "br": picture.BottomRight, "c": picture.Center,
}

// parseThumbnail parses a -thumbnail value, name=SECONDS[:HEIGHT[:QUALITY]].
// The interval must be at least 1: osdev has no default interval.
func parseThumbnail(v string) (stream string, th picture.Thumbnail, err error) {
	const want = "want name=SECONDS[:HEIGHT[:QUALITY]]"
	stream, spec, ok := strings.Cut(v, "=")
	if !ok || !validName(stream) {
		return "", th, fmt.Errorf("malformed -thumbnail %q, %s", v, want)
	}
	f := strings.Split(spec, ":")
	if len(f) > 3 {
		return "", th, fmt.Errorf("malformed -thumbnail %q, %s", v, want)
	}
	n := make([]int, 3)
	for i, s := range f {
		if n[i], err = strconv.Atoi(s); err != nil {
			return "", th, fmt.Errorf("-thumbnail %q: %q is not an integer", v, s)
		}
	}
	th = picture.Thumbnail{IntervalSec: n[0], Height: n[1], Quality: n[2]}
	if th.IntervalSec < 1 {
		return "", th, fmt.Errorf("-thumbnail %q: the interval must be at least 1 second", v)
	}
	if err := th.Validate(); err != nil {
		return "", th, fmt.Errorf("-thumbnail %q: %w", v, err)
	}
	return stream, th, nil
}

// thumbSrc is the thumbnail source of one stream: its transcoder's child,
// or a Thumbnailer of its own.
type thumbSrc struct {
	stream   string
	shot     func() (picture.Shot, time.Time, bool)
	nailer   *transcode.Thumbnailer // nil when the transcoder's child makes them
	lastLine string                 // the last report line
}

// report writes a Thumbnailer's counters when they differ from the last
// report. A transcoder's are in its own line.
func (t *thumbSrc) report(w io.Writer) {
	if t.nailer == nil {
		return
	}
	st := t.nailer.Stats()
	line := fmt.Sprintf("osdev: stream %s: thumbnailer spawns %d, exits %d, keys sent %d, keys skipped %d, shots %d, failed %d",
		t.stream, st.Spawns, st.Exits, st.KeysSent, st.KeysSkipped, st.Shots, st.Failed)
	if line == t.lastLine {
		return
	}
	t.lastLine = line
	_, _ = fmt.Fprintln(w, line)
}

// thumbHandler serves GET /live/{stream}/thumbnail.jpg for the streams in
// srcs: the latest shot, with its arrival time as Last-Modified and
// "<epoch>-<pts>" as ETag. Any other stream, and a stream before its first
// shot, is a 404.
func thumbHandler(srcs map[string]*thumbSrc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := srcs[r.PathValue("stream")]
		if src == nil {
			http.NotFound(w, r)
			return
		}
		shot, at, ok := src.shot()
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "image/jpeg")
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", fmt.Sprintf(`"%d-%d"`, shot.Epoch, shot.PTS))
		// ServeContent adds Last-Modified and answers If-None-Match with a
		// 304.
		http.ServeContent(w, r, "", at, bytes.NewReader(shot.JPEG))
	})
}
