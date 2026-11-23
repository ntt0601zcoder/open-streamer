// Package tsstream writes a stream's hub frames as one continuous MPEG-TS
// byte stream to a Sink, for the outputs that stream TS (HTTP-TS, SRT).
//
// Stream subscribes to the hub and keeps one TS muxer for its life. Output
// starts after one second of stream time has been buffered, so the muxer is
// built with every track that starts together; a track that joins later is
// added to the program with fresh tables. A configured track whose codec
// changes ends the stream, and so do a closed subscription, a done context
// and a write that fails. Media timing comes only from frame timestamps;
// wallclock sets write deadlines.
package tsstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

const (
	defaultQueue        = 1024
	defaultWriteTimeout = 10 * time.Second
	// MaxTracks bounds track indexes: track i is on PID 256 + i, and PIDs
	// stop below the null PID 0x1FFF. Frames on other indexes are skipped.
	MaxTracks = 0x1FFF - 256
)

// Why a stream ended, as reported in Stats.Ended.
const (
	EndedContext     = "context"
	EndedClosed      = "closed"
	EndedCodecChange = "codec_change"
	EndedError       = "error"
)

// Sink receives the TS bytes. Write is called with whole muxer output for one
// frame; SetWriteDeadline is called before every Write; Flush after every
// Write.
type Sink interface {
	io.Writer
	SetWriteDeadline(time.Time) error // may be a no-op
	Flush() error                     // may be a no-op
}

// Options tunes a stream. Zero values select the defaults.
type Options struct {
	// Queue is the subscription queue length (default 1024).
	Queue int
	// Prebuffer is the stream time buffered before output starts, in 90 kHz
	// ticks (default 90000).
	Prebuffer int64
	// WriteTimeout is the deadline of every write (default 10 s).
	WriteTimeout time.Duration
}

// Stats summarises a finished stream.
type Stats struct {
	Frames     uint64 // frames written
	LateTracks uint64 // tracks added after the first tables
	Ended      string // why the stream ended: one of the Ended constants
}

// Stream subscribes to h under name and writes MPEG-TS to s until ctx ends,
// the hub closes, a configured track's codec changes, or a write fails. A
// failure is returned wrapped; every other end returns a nil error.
func Stream(ctx context.Context, h *hub.Hub, name string, s Sink, opt Options) (Stats, error) {
	if opt.Queue <= 0 {
		opt.Queue = defaultQueue
	}
	if opt.Prebuffer <= 0 {
		opt.Prebuffer = media.ClockRate
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = defaultWriteTimeout
	}
	sub := h.Subscribe(name, opt.Queue)
	defer sub.Close()
	ss := &session{s: s, timeout: opt.WriteTimeout}
	err := ss.run(ctx, sub, opt.Prebuffer)
	if err != nil {
		ss.stats.Ended = EndedError
		return ss.stats, err
	}
	return ss.stats, nil
}

// endReason carries a clean end through the loop.
type endReason string

func (e endReason) Error() string { return string(e) }

// session is one stream: its muxer, the tracks configured in it and the
// buffer the muxer writes into.
type session struct {
	s       Sink
	timeout time.Duration
	mux     *ts.Muxer
	tracks  []*media.TrackConfig
	buf     bytes.Buffer
	stats   Stats
}

// run streams sub until it closes, ctx is done or a write fails. A clean end
// sets stats.Ended and returns nil.
func (s *session) run(ctx context.Context, sub *hub.Sub, prebuffer int64) error {
	err := s.loop(ctx, sub, prebuffer)
	if r, ok := err.(endReason); ok { //nolint:errorlint // endReason is a sentinel value, never wrapped.
		s.stats.Ended = string(r)
		return nil
	}
	return err
}

func (s *session) loop(ctx context.Context, sub *hub.Sub, prebuffer int64) error {
	if err := s.start(ctx, sub, prebuffer); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return endReason(EndedContext)
		case d, ok := <-sub.C():
			if !ok {
				return endReason(EndedClosed)
			}
			if err := s.frame(d.Frame); err != nil {
				return err
			}
		}
	}
}

// start buffers the first frames, builds the muxer with each track's first
// config, writes tables and then the buffered frames.
func (s *session) start(ctx context.Context, sub *hub.Sub, prebuffer int64) error {
	pending := buffer(ctx, sub, prebuffer)
	if len(pending) == 0 {
		if ctx.Err() != nil {
			return endReason(EndedContext)
		}
		return endReason(EndedClosed)
	}
	s.tracks = firstConfigs(pending)
	mux, err := ts.NewMuxer(&s.buf, s.tracks)
	if err != nil {
		return fmt.Errorf("tsstream: new muxer: %w", err)
	}
	s.mux = mux
	if err := mux.WriteTables(); err != nil {
		return fmt.Errorf("tsstream: write tables: %w", err)
	}
	for i := range pending {
		if err := s.frame(pending[i]); err != nil {
			return err
		}
	}
	return nil
}

// buffer collects frames from the first delivery up to and including the
// first one whose DTS is at least prebuffer past it, or until sub closes. It
// returns nil when ctx is done first.
func buffer(ctx context.Context, sub *hub.Sub, prebuffer int64) []media.Frame {
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

func validTrack(i int) bool { return i >= 0 && i < MaxTracks }

// frame writes f, adding its track to the program first when it is new.
// Frames on an invalid track index and frames without a config on an
// unconfigured track are skipped.
func (s *session) frame(f media.Frame) error {
	if !validTrack(f.Track) {
		return nil
	}
	var tc *media.TrackConfig
	if f.Track < len(s.tracks) {
		tc = s.tracks[f.Track]
	}
	switch {
	case tc == nil && f.Config == nil:
		return nil
	case tc == nil:
		tracks := make([]*media.TrackConfig, max(len(s.tracks), f.Track+1))
		copy(tracks, s.tracks)
		tracks[f.Track] = f.Config
		if err := s.mux.SetTracks(tracks); err != nil {
			return fmt.Errorf("tsstream: add track %d: %w", f.Track, err)
		}
		s.tracks = tracks
		s.stats.LateTracks++
		if err := s.mux.WriteTables(); err != nil {
			return fmt.Errorf("tsstream: write tables: %w", err)
		}
	case f.Config != nil && f.Config.Codec != tc.Codec:
		return endReason(EndedCodecChange) // a codec change is out of M3a scope
	}
	if err := s.mux.WriteFrame(f); err != nil {
		return fmt.Errorf("tsstream: write frame: %w", err)
	}
	s.stats.Frames++
	return s.flush()
}

// flush writes the buffered bytes under the write deadline and flushes them
// to the sink.
func (s *session) flush() error {
	if err := s.s.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		return fmt.Errorf("tsstream: set write deadline: %w", err)
	}
	if _, err := s.s.Write(s.buf.Bytes()); err != nil {
		return fmt.Errorf("tsstream: write: %w", err)
	}
	s.buf.Reset()
	if err := s.s.Flush(); err != nil {
		return fmt.Errorf("tsstream: flush: %w", err)
	}
	return nil
}
