// Package httpts serves a stream as one continuous MPEG-TS response at
// /live/{stream}/stream.ts.
//
// Each request subscribes to the stream's hub and keeps one TS muxer for its
// life. The response starts after one second of stream time has been
// buffered, so the muxer is built with every track that starts together; a
// track that joins later is added to the program with fresh tables. A
// configured track whose codec changes ends the response, and so do a closed
// subscription, a gone client and a write that misses its deadline. Media
// timing comes only from frame timestamps; wallclock sets write deadlines.
package httpts

import (
	"errors"
	"net/http"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/tsstream"
)

const (
	defaultQueue        = 1024
	defaultWriteTimeout = 10 * time.Second
	// prebuffer is the stream time buffered before the response starts.
	prebuffer   = media.ClockRate
	contentType = "video/mp2t"
	// maxTracks bounds track indexes, as in tsstream.
	maxTracks = tsstream.MaxTracks
)

// Handler serves /live/{stream}/stream.ts for every stream that lookup
// finds, and 404 otherwise. Each request subscribes with a queue of the given
// length (0 means 1024).
func Handler(lookup func(stream string) (*hub.Hub, bool), queue int) http.Handler {
	return newHandler(lookup, queue, defaultWriteTimeout)
}

// handler serves the streams; timeout is the deadline of every write.
type handler struct {
	lookup  func(stream string) (*hub.Hub, bool)
	queue   int
	timeout time.Duration
}

func newHandler(lookup func(stream string) (*hub.Hub, bool), queue int, timeout time.Duration) http.Handler {
	if queue <= 0 {
		queue = defaultQueue
	}
	h := &handler{lookup: lookup, queue: queue, timeout: timeout}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live/{stream}/stream.ts", h.serve)
	return mux
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	hb, ok := h.lookup(r.PathValue("stream"))
	if !ok || hb == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodHead {
		return
	}
	sink := &sink{w: w, rc: http.NewResponseController(w)}
	// Stream ends are not errors worth reporting here: the client is gone or
	// the stream finished, and the response simply stops.
	_, _ = tsstream.Stream(r.Context(), hb, "httpts "+r.RemoteAddr, sink,
		tsstream.Options{Queue: h.queue, Prebuffer: prebuffer, WriteTimeout: h.timeout})
}

// sink adapts a response writer to tsstream.Sink; deadlines and flushes the
// writer does not support are no-ops.
type sink struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// Write writes p to the response.
func (s *sink) Write(p []byte) (int, error) { return s.w.Write(p) } //nolint:wrapcheck // Passed through to the client.

// SetWriteDeadline sets the response write deadline when the writer supports it.
func (s *sink) SetWriteDeadline(t time.Time) error {
	if err := s.rc.SetWriteDeadline(t); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err //nolint:wrapcheck // Wrapped by tsstream.
	}
	return nil
}

// Flush flushes the response when the writer supports it.
func (s *sink) Flush() error {
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err //nolint:wrapcheck // Wrapped by tsstream.
	}
	return nil
}
