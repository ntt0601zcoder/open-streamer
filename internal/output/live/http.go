package live

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	dirHLS  = "hls"
	dirCMAF = "cmaf"

	typeTS   = "video/mp2t"
	typeInit = "video/mp4"
	typeM4S  = "video/iso.segment"
)

// playlistRoute serves one kind of playlist or manifest file in dir.
type playlistRoute struct {
	dir   string
	match func(name string) bool
	// build renders the file from a snapshot that lists at least one
	// segment. ok false means 404.
	build func(p *Packager, s Snapshot, name string) (body []byte, contentType string, ok bool)
}

// playlistRoutes are the playlist and manifest files Handler serves. The HLS
// playlists and the DASH MPD register here.
var playlistRoutes []playlistRoute

// Handler serves the HLS and DASH files of every stream that lookup finds,
// under /live/{stream}/hls/ and /live/{stream}/cmaf/. It answers 404 for an
// unknown stream or file, for a segment or init outside the window, and for
// every playlist until the window lists a segment.
func Handler(lookup func(stream string) (*Packager, bool)) http.Handler {
	return newHandler(lookup, playlistRoutes)
}

func newHandler(lookup func(stream string) (*Packager, bool), routes []playlistRoute) http.Handler {
	mux := http.NewServeMux()
	for _, dir := range []string{dirHLS, dirCMAF} {
		mux.HandleFunc("GET /live/{stream}/"+dir+"/{file}", func(w http.ResponseWriter, r *http.Request) {
			p, ok := lookup(r.PathValue("stream"))
			if !ok || p == nil {
				http.NotFound(w, r)
				return
			}
			name := r.PathValue("file")
			if body, ctype, ok := p.file(dir, name); ok {
				writeBody(w, r, ctype, body)
				return
			}
			for _, rt := range routes {
				if rt.dir != dir || !rt.match(name) {
					continue
				}
				if s := p.Snapshot(); s.Listed > 0 {
					if body, ctype, ok := rt.build(p, s, name); ok {
						writeBody(w, r, ctype, body)
						return
					}
				}
				break
			}
			http.NotFound(w, r)
		})
	}
	return mux
}

// writeBody answers with body, honouring HEAD and Range requests. No lock is
// held here.
func writeBody(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// file returns a segment or init file in the window by its name in dir.
func (p *Packager) file(dir, name string) ([]byte, string, bool) {
	var (
		body  []byte
		ctype string
	)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case dir == dirHLS && strings.HasSuffix(name, ".ts"):
		if seq, ok := parseUint(strings.TrimSuffix(name, ".ts"), 64); ok {
			body, ctype = p.st.segmentTS(seq), typeTS
		}
	case dir == dirCMAF && strings.HasPrefix(name, "init-") && strings.HasSuffix(name, ".mp4"):
		if track, epoch, ok := parsePair(strings.TrimSuffix(strings.TrimPrefix(name, "init-"), ".mp4"), 32); ok {
			body, ctype = p.st.init(int(track), uint32(epoch)), typeInit
		}
	case dir == dirCMAF && strings.HasSuffix(name, ".m4s"):
		if track, n, ok := parsePair(strings.TrimSuffix(name, ".m4s"), 64); ok {
			body, ctype = p.st.part(int(track), n), typeM4S
		}
	}
	return body, ctype, body != nil
}

// parsePair parses "{track}-{b}" with a track below 2^31 and b below 2^bits.
func parsePair(s string, bits int) (track, b uint64, ok bool) {
	ts, bs, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false
	}
	track, ok1 := parseUint(ts, 31)
	b, ok2 := parseUint(bs, bits)
	return track, b, ok1 && ok2
}

// parseUint parses a canonical decimal number: digits only, no leading zero.
func parseUint(s string, bits int) (uint64, bool) {
	v, err := strconv.ParseUint(s, 10, bits)
	if err != nil || strconv.FormatUint(v, 10) != s {
		return 0, false
	}
	return v, true
}
