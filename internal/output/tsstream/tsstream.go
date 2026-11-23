// Package tsstream writes a stream's hub frames as one continuous MPEG-TS
// byte stream to a Sink, for the outputs that stream TS (HTTP-TS, SRT).
//
// Stream subscribes to the hub, starting at its cached GOP, and keeps one TS
// muxer for its life. Output starts after one second of stream time has been
// buffered, so the muxer is built with every track that starts together; a
// track that joins later is added to the program with fresh tables. A
// configured track whose codec changes ends the stream, and so do a closed
// subscription, a done context and a write that fails. Media timing comes
// only from frame timestamps; wallclock sets write deadlines.
//
// # Resync after loss
//
// A delivery that reports lost frames (a slow sink made the hub skip)
// restarts the output: frames are dropped until the next video keyframe (on
// an output without video, the next frame), which is preceded by the PAT and
// PMT, and the first packet of every track's PID from there on carries the
// discontinuity_indicator. Audio presented before that keyframe is dropped,
// and so are a second video track's frames until its own keyframe. When no
// video keyframe comes within hub.LiveWindow of stream time from the first
// frame dropped, the video track has ended (e.g. a failover to an audio-only
// source): the output resumes on audio, and video at its next keyframe.
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
	// maxKeyWait is how long, in stream time from the first frame dropped
	// after a loss, the output waits for a video keyframe before it resumes
	// on audio.
	maxKeyWait = hub.LiveWindow
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
	// Queue is the minimum subscription queue length (default 1024); the
	// queue always holds the hub's cached GOP plus 64 frames
	// (hub.SubscribeCached).
	Queue int
	// Prebuffer is the stream time buffered before output starts, in 90 kHz
	// ticks (default 90000).
	Prebuffer int64
	// WriteTimeout is the deadline of every write (default 10 s).
	WriteTimeout time.Duration
	// Meter, when set, is told the stream's progress, for a sink that
	// paces by bitrate. It never changes the bytes written.
	Meter Meter
}

// Meter follows a stream's bitrate. Prebuffered is called once, before
// the first write, with the frame payload bytes and the DTS span (90 kHz
// ticks, last minus first) of the deliveries buffered before output starts:
// the hub's cached GOP and what followed it. Frame is called before each
// frame's bytes are written, with its DTS and the TS bytes about to be
// written (tables included).
type Meter interface {
	Prebuffered(bytes int, span int64)
	Frame(dts int64, bytes int)
}

// Stats summarises a finished stream.
type Stats struct {
	Frames     uint64 // frames written
	LateTracks uint64 // tracks added after the first tables
	// Resyncs counts deliveries that reported lost frames.
	Resyncs uint64
	// DroppedBeforeKey counts frames dropped after a loss while waiting for
	// the resume point, and a video track's frames dropped until its own
	// keyframe after the resume.
	DroppedBeforeKey uint64
	// DroppedEarlyAudio counts audio frames presented before the keyframe
	// the output resumed at.
	DroppedEarlyAudio uint64
	Ended             string // why the stream ended: one of the Ended constants
}

// Stream subscribes to h under name (hub.SubscribeCached with opt.Queue) and
// writes MPEG-TS to s until ctx ends,
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
	sub := h.SubscribeCached(name, opt.Queue)
	defer sub.Close()
	ss := &session{s: s, timeout: opt.WriteTimeout, meter: opt.Meter}
	err := ss.run(ctx, sub.C(), opt.Prebuffer)
	if err != nil {
		ss.stats.Ended = EndedError
		return ss.stats, err
	}
	return ss.stats, nil
}

// endReason carries a clean end through the loop.
type endReason string

func (e endReason) Error() string { return string(e) }

// session is one stream: its muxer, the tracks configured in it, the buffer
// the muxer writes into and the resync state.
type session struct {
	s       Sink
	meter   Meter
	timeout time.Duration
	mux     *ts.Muxer
	tracks  []*media.TrackConfig
	buf     bytes.Buffer
	stats   Stats

	waitKey   bool  // after a loss: dropping frames until the resume point
	waiting   bool  // a frame was dropped since waitKey was set
	waitFrom  int64 // the DTS of that frame
	resumeDue bool  // the next frame written resumes: tables and discontinuity first
	resumePTS int64 // PTS of the frame the output resumed at
	// needKey marks video tracks dropping frames until their keyframe, and
	// early the tracks whose audio before resumePTS is dropped, after a
	// resume.
	needKey map[int]bool
	early   map[int]bool
}

// run streams the deliveries of ch until it closes, ctx is done or a write
// fails. A clean end sets stats.Ended and returns nil.
func (s *session) run(ctx context.Context, ch <-chan hub.Delivery, prebuffer int64) error {
	err := s.loop(ctx, ch, prebuffer)
	if r, ok := err.(endReason); ok { //nolint:errorlint // endReason is a sentinel value, never wrapped.
		s.stats.Ended = string(r)
		return nil
	}
	return err
}

func (s *session) loop(ctx context.Context, ch <-chan hub.Delivery, prebuffer int64) error {
	if err := s.start(ctx, ch, prebuffer); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return endReason(EndedContext)
		case d, ok := <-ch:
			if !ok {
				return endReason(EndedClosed)
			}
			if err := s.deliver(d); err != nil {
				return err
			}
		}
	}
}

// start buffers the first frames, builds the muxer with each track's first
// config, writes tables and then the buffered frames.
func (s *session) start(ctx context.Context, ch <-chan hub.Delivery, prebuffer int64) error {
	pending := buffer(ctx, ch, prebuffer)
	if len(pending) == 0 {
		if ctx.Err() != nil {
			return endReason(EndedContext)
		}
		return endReason(EndedClosed)
	}
	if s.meter != nil {
		s.meter.Prebuffered(payloadSpan(pending))
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
		if err := s.deliver(pending[i]); err != nil {
			return err
		}
	}
	return nil
}

// buffer collects deliveries from the first one up to and including the
// first one whose DTS is at least prebuffer past it, or until ch closes. It
// returns nil when ctx is done first.
func buffer(ctx context.Context, ch <-chan hub.Delivery, prebuffer int64) []hub.Delivery {
	var out []hub.Delivery
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, d)
			if d.Frame.DTS >= out[0].Frame.DTS+prebuffer {
				return out
			}
		}
	}
}

// payloadSpan returns the payload bytes of the deliveries and the span of
// their DTS.
func payloadSpan(ds []hub.Delivery) (bytes int, span int64) {
	lo, hi := ds[0].Frame.DTS, ds[0].Frame.DTS
	for i := range ds {
		f := &ds[i].Frame
		for _, b := range f.Data {
			bytes += len(b)
		}
		lo, hi = min(lo, f.DTS), max(hi, f.DTS)
	}
	return bytes, hi - lo
}

// firstConfigs returns each track's first config among the deliveries.
func firstConfigs(ds []hub.Delivery) []*media.TrackConfig {
	var tracks []*media.TrackConfig
	for i := range ds {
		f := &ds[i].Frame
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

// deliver writes one delivery, restarting the output first when it reports
// lost frames.
func (s *session) deliver(d hub.Delivery) error {
	if d.Lost > 0 {
		s.stats.Resyncs++
		if !s.waitKey {
			s.waitKey, s.waiting = true, false
		}
		// Already waiting: keep the wait that started at the first frame
		// dropped.
	}
	return s.frame(d.Frame)
}

// frame writes f, adding its track to the program first when it is new.
// Frames on an invalid track index, frames without a config on an
// unconfigured track and frames the resync rules drop are skipped.
func (s *session) frame(f media.Frame) error {
	if !validTrack(f.Track) {
		return nil
	}
	var tc *media.TrackConfig
	if f.Track < len(s.tracks) {
		tc = s.tracks[f.Track]
	}
	cfg := tc
	switch {
	case tc == nil && f.Config == nil:
		return nil
	case tc == nil:
		cfg = f.Config
	case f.Config != nil && f.Config.Codec != tc.Codec:
		return endReason(EndedCodecChange) // a codec change is out of M3a scope
	}
	if !s.admit(f, cfg.Codec.Kind() == media.KindVideo) {
		return nil
	}
	tables := s.resumeDue
	if tc == nil {
		tracks := make([]*media.TrackConfig, max(len(s.tracks), f.Track+1))
		copy(tracks, s.tracks)
		tracks[f.Track] = f.Config
		if err := s.mux.SetTracks(tracks); err != nil {
			return fmt.Errorf("tsstream: add track %d: %w", f.Track, err)
		}
		s.tracks = tracks
		s.stats.LateTracks++
		tables = true
	}
	if tables {
		if err := s.mux.WriteTables(); err != nil {
			return fmt.Errorf("tsstream: write tables: %w", err)
		}
	}
	if s.resumeDue {
		s.mux.MarkDiscontinuity()
		s.resumeDue = false
	}
	if err := s.mux.WriteFrame(f); err != nil {
		return fmt.Errorf("tsstream: write frame: %w", err)
	}
	s.stats.Frames++
	if s.meter != nil {
		s.meter.Frame(f.DTS, s.buf.Len())
	}
	return s.flush()
}

// admit applies the resync rules to f, a frame of a video track when video
// is set, and reports whether it is written.
func (s *session) admit(f media.Frame, video bool) bool {
	if s.waitKey {
		switch {
		case !video && !s.hasVideo():
			// An output without video resumes at its next frame.
		case video && f.Key:
		case !video && s.waiting && f.DTS-s.waitFrom >= maxKeyWait:
			// No video keyframe within the live window: the video track
			// ended. Resume on audio; video waits for its keyframe.
		default:
			if !s.waiting {
				s.waiting, s.waitFrom = true, f.DTS
			}
			s.stats.DroppedBeforeKey++
			return false
		}
		s.resume(f)
		return true
	}
	if video && s.needKey[f.Track] {
		if !f.Key {
			s.stats.DroppedBeforeKey++
			return false
		}
		delete(s.needKey, f.Track)
	}
	if !video && s.early[f.Track] {
		if f.PTS < s.resumePTS {
			s.stats.DroppedEarlyAudio++
			return false
		}
		delete(s.early, f.Track)
	}
	return true
}

// resume ends the wait at f: it is written after fresh tables with the
// discontinuity indicator on every PID. Every other configured video track
// waits for its keyframe, and audio presented before f is dropped.
func (s *session) resume(f media.Frame) {
	s.waitKey, s.waiting = false, false
	s.resumeDue, s.resumePTS = true, f.PTS
	if s.needKey == nil {
		s.needKey, s.early = map[int]bool{}, map[int]bool{}
	}
	clear(s.needKey)
	clear(s.early)
	for i, tc := range s.tracks {
		switch {
		case tc == nil, i == f.Track:
		case tc.Codec.Kind() == media.KindVideo:
			s.needKey[i] = true
		default:
			s.early[i] = true
		}
	}
}

// hasVideo reports whether a video track is configured.
func (s *session) hasVideo() bool {
	for _, tc := range s.tracks {
		if tc != nil && tc.Codec.Kind() == media.KindVideo {
			return true
		}
	}
	return false
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
