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
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

const (
	defaultQueue        = 1024
	defaultWriteTimeout = 10 * time.Second
	// prebuffer is the stream time buffered before the response starts.
	prebuffer = media.ClockRate
	// maxTracks bounds track indexes: track i is on PID 256 + i, and PIDs
	// stop below the null PID 0x1FFF. Frames on other indexes are skipped.
	maxTracks   = 0x1FFF - 256
	contentType = "video/mp2t"
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
	sub := hb.Subscribe("httpts "+r.RemoteAddr, h.queue)
	defer sub.Close()
	s := &session{w: w, rc: http.NewResponseController(w), timeout: h.timeout}
	s.run(r.Context(), sub)
}

// session is one response: its muxer, the tracks configured in it and the
// buffer the muxer writes into.
type session struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	mux     *ts.Muxer
	tracks  []*media.TrackConfig
	buf     bytes.Buffer
}

// run streams sub until it closes, ctx is done or a write fails.
func (s *session) run(ctx context.Context, sub *hub.Sub) {
	if !s.start(ctx, sub) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-sub.C():
			if !ok || !s.frame(d.Frame) {
				return
			}
		}
	}
}

// start buffers the first frames, builds the muxer with each track's first
// config, writes tables and then the buffered frames. It reports false when
// the response must end.
func (s *session) start(ctx context.Context, sub *hub.Sub) bool {
	pending := buffer(ctx, sub)
	if len(pending) == 0 {
		return false
	}
	s.tracks = firstConfigs(pending)
	mux, err := ts.NewMuxer(&s.buf, s.tracks)
	if err != nil {
		return false
	}
	s.mux = mux
	if err := mux.WriteTables(); err != nil {
		return false
	}
	for i := range pending {
		if !s.frame(pending[i]) {
			return false
		}
	}
	return true
}

// buffer collects frames from the first delivery up to and including the
// first one whose DTS is at least prebuffer past it, or until sub closes. It
// returns nil when ctx is done first.
func buffer(ctx context.Context, sub *hub.Sub) []media.Frame {
	var out []media.Frame
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-sub.C():
			if !ok {
				return out
			}
			out = append(out, d.Frame)
			if d.Frame.DTS >= out[0].DTS+prebuffer {
				return out
			}
		}
	}
}

// firstConfigs returns each track's first config among frames.
func firstConfigs(frames []media.Frame) []*media.TrackConfig {
	var tracks []*media.TrackConfig
	for _, f := range frames {
		if !validTrack(f.Track) || f.Config == nil {
			continue
		}
		if f.Track >= len(tracks) {
			tracks = append(tracks, make([]*media.TrackConfig, f.Track+1-len(tracks))...)
		}
		if tracks[f.Track] == nil {
			tracks[f.Track] = f.Config
		}
	}
	return tracks
}

func validTrack(i int) bool { return i >= 0 && i < maxTracks }

// frame writes f, adding its track to the program first when it is new. It
// reports false when the response must end. Frames on an invalid track index
// and frames without a config on an unconfigured track are skipped.
func (s *session) frame(f media.Frame) bool {
	if !validTrack(f.Track) {
		return true
	}
	var tc *media.TrackConfig
	if f.Track < len(s.tracks) {
		tc = s.tracks[f.Track]
	}
	switch {
	case tc == nil && f.Config == nil:
		return true
	case tc == nil:
		tracks := make([]*media.TrackConfig, max(len(s.tracks), f.Track+1))
		copy(tracks, s.tracks)
		tracks[f.Track] = f.Config
		if err := s.mux.SetTracks(tracks); err != nil {
			return false
		}
		s.tracks = tracks
		if err := s.mux.WriteTables(); err != nil {
			return false
		}
	case f.Config != nil && f.Config.Codec != tc.Codec:
		return false // a codec change is out of M3a scope
	}
	if err := s.mux.WriteFrame(f); err != nil {
		return false
	}
	return s.flush()
}

// flush writes the buffered bytes under the write deadline and flushes them
// to the client. It reports false on a write error.
func (s *session) flush() bool {
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return false
	}
	s.buf.Reset()
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	return true
}
