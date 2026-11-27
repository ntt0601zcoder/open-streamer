package server

import (
	"net/http"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

// LiveOutputs returns the outputs a /live/<name>/<dir>/<file> path is part
// of: hls/* and cmaf/*.m3u8 are HLS; cmaf/*.mpd is DASH; any other cmaf
// file (an init or a media segment) is shared by both; anything else is
// nil.
func LiveOutputs(dir, file string) []stream.Output {
	switch dir {
	case "hls":
		return []stream.Output{stream.OutHLS}
	case "cmaf":
		switch {
		case strings.HasSuffix(file, ".m3u8"):
			return []stream.Output{stream.OutHLS}
		case strings.HasSuffix(file, ".mpd"):
			return []stream.Output{stream.OutDASH}
		default:
			return []stream.Output{stream.OutHLS, stream.OutDASH}
		}
	}
	return nil
}

// mediaHandler is the handler of the media listener: HTTP-TS, and the HLS
// and DASH files behind the output gate.
func (s *Server) mediaHandler() http.Handler {
	packager := func(out stream.Output, name string) (*live.Packager, bool) {
		m := s.Manager()
		if m == nil {
			return nil, false
		}
		return m.Packager(out, name)
	}
	// One packager serves both outputs of a stream; the gate decided that
	// the file is of an output that is on.
	either := func(name string) (*live.Packager, bool) {
		if p, ok := packager(stream.OutHLS, name); ok {
			return p, true
		}
		return packager(stream.OutDASH, name)
	}
	files := live.Handler(either)
	mux := http.NewServeMux()
	mux.Handle("GET /live/{stream}/stream.ts", httpts.Handler(func(n string) (*hub.Hub, bool) {
		return s.lookup(stream.OutHTTPTS)(n)
	}, 0))
	mux.Handle("/live/{stream}/{dir}/{file}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("stream")
		for _, out := range LiveOutputs(r.PathValue("dir"), r.PathValue("file")) {
			if _, ok := packager(out, name); ok {
				files.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
	return mux
}
