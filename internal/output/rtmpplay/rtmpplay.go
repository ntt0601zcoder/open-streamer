// Package rtmpplay serves streams to RTMP players.
//
// A Server is the rtmpsrv.PlayHandler of one app: a player of
// rtmp://host/<app>/<name> gets the hub that lookup finds for <name>. A
// stream can be played once its hub is live (hub.Live: a video keyframe, or
// audio on a hub without video); before that the registry answers
// NetStream.Play.StreamNotFound.
//
// Each player subscribes with a queue that holds the whole GOP cache
// (hub.SubscribeCached), so it starts at once on the cached keyframe, and
// keeps one flv.Muxer for its connection. A delivery that reports lost
// frames resyncs the muxer, which resumes at the next keyframe with
// onMetaData and headers. A codec change and the hub closing end the player
// with end of stream; a write that misses the write timeout, or fails, ends
// it at once, so a stalled player never holds up the hub or other players.
package rtmpplay

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/flv"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

const (
	defaultQueue        = 256
	defaultWriteTimeout = 10 * time.Second
)

// Options tunes a Server. Zero values select the defaults.
type Options struct {
	// Queue is the smallest subscription queue of a player; it grows to the
	// cached frames plus 64 (hub.SubscribeCached). 0 means 256.
	Queue int
	// WriteTimeout bounds each write to a player; a write that misses it
	// ends the player. 0 means 10 s.
	WriteTimeout time.Duration
}

// Stats counts a Server's players.
type Stats struct {
	// Players is the number of players being served; PlayersStarted counts
	// every player served.
	Players        int
	PlayersStarted uint64
	// Frames counts the hub frames handed to the players' muxers, Messages
	// the RTMP messages written.
	Frames, Messages uint64
	// FrameErrors counts frames the muxer could not send (no track config,
	// a kind RTMP does not carry); they are skipped.
	FrameErrors uint64
	// Resyncs counts deliveries that reported lost frames.
	Resyncs uint64
	// Why players ended: a write that missed the write timeout, another
	// failed write, a codec change, the hub closing, the player leaving (or
	// its connection failing outside a write), and the handler being
	// unregistered.
	EndedDeadline, EndedWriteError, EndedCodecChange, EndedHubClosed uint64
	EndedLeft, EndedShutdown                                         uint64
	// Mux sums the muxer counters of every ended player.
	Mux flv.MuxStats
}

// counters are the Stats a player updates as it goes.
type counters struct {
	players                                  atomic.Int64
	started, frames, messages, errs, resyncs atomic.Uint64
}

// Server plays hub streams to RTMP players. It is safe for concurrent use.
type Server struct {
	lookup func(name string) (*hub.Hub, bool)
	opt    Options

	live counters

	mu    sync.Mutex
	ended Stats // the Ended fields and Mux
}

// New returns a Server for the hubs that lookup finds by stream name.
func New(lookup func(name string) (*hub.Hub, bool), opt Options) *Server {
	if opt.Queue <= 0 {
		opt.Queue = defaultQueue
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = defaultWriteTimeout
	}
	return &Server{lookup: lookup, opt: opt}
}

// Live reports whether name can be played now: lookup finds its hub and the
// hub is live.
func (s *Server) Live(name string) bool {
	h, ok := s.lookup(name)
	return ok && h != nil && h.Live()
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	st := s.ended
	s.mu.Unlock()
	st.Players = int(s.live.players.Load())
	st.PlayersStarted = s.live.started.Load()
	st.Frames = s.live.frames.Load()
	st.Messages = s.live.messages.Load()
	st.FrameErrors = s.live.errs.Load()
	st.Resyncs = s.live.resyncs.Load()
	return st
}

// end is why a player ended.
type end uint8

const (
	endShutdown end = iota
	endLeft
	endDeadline
	endWriteError
	endCodecChange
	endHubClosed
)

// Serve plays the stream name, which the registry resolved from the play
// path, until ctx ends, the player leaves, a write fails or misses its
// deadline, the codec changes or the hub closes. The last two send end of
// stream first. The caller closes conn.
func (s *Server) Serve(ctx context.Context, conn *rtmp.ServerConn, name string) {
	h, ok := s.lookup(name)
	if !ok || h == nil {
		_ = conn.EndOfStream() // the stream went away after the play decision
		return
	}
	conn.SetWriteTimeout(s.opt.WriteTimeout)
	sub := h.SubscribeCached("rtmp "+name, s.opt.Queue)
	defer sub.Close()
	s.live.started.Add(1)
	s.live.players.Add(1)

	p := &player{c: &s.live, conn: conn, mux: flv.NewMuxer()}
	why := p.run(ctx, sub)
	if why == endCodecChange || why == endHubClosed {
		_ = conn.EndOfStream() // best effort: the conn is closed next anyway
	}
	// Unsubscribed and uncounted before the end is counted, so a reader of
	// Stats that sees the end sees the player gone.
	sub.Close()
	s.live.players.Add(-1)
	s.finish(why, p.mux.Stats())
}

// finish counts an ended player.
func (s *Server) finish(why end, ms flv.MuxStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.ended
	st.Mux.SkippedTracks += ms.SkippedTracks
	st.Mux.DroppedBeforeKey += ms.DroppedBeforeKey
	st.Mux.DroppedBeforeBase += ms.DroppedBeforeBase
	st.Mux.DroppedEarlyAudio += ms.DroppedEarlyAudio
	st.Mux.Resyncs += ms.Resyncs
	st.Mux.Epochs += ms.Epochs
	switch why {
	case endShutdown:
		st.EndedShutdown++
	case endLeft:
		st.EndedLeft++
	case endDeadline:
		st.EndedDeadline++
	case endWriteError:
		st.EndedWriteError++
	case endCodecChange:
		st.EndedCodecChange++
	case endHubClosed:
		st.EndedHubClosed++
	}
}

// player is one connection being served.
type player struct {
	c    *counters
	conn *rtmp.ServerConn
	mux  *flv.Muxer
}

// run moves deliveries from sub to the player until it must end, and
// reports why.
func (p *player) run(ctx context.Context, sub *hub.Sub) end {
	for {
		select {
		case <-ctx.Done():
			return endShutdown
		case <-p.conn.Done():
			return connEnd(p.conn.Err())
		case d, ok := <-sub.C():
			if !ok {
				return endHubClosed
			}
			if why, done := p.deliver(d); done {
				return why
			}
		}
	}
}

// deliver muxes and writes one delivery; done reports that the player must
// end, and why.
func (p *player) deliver(d hub.Delivery) (why end, done bool) {
	if d.Lost > 0 {
		p.mux.Resync()
		p.c.resyncs.Add(1)
	}
	p.c.frames.Add(1)
	msgs, err := p.mux.Frame(d.Frame)
	switch {
	case errors.Is(err, flv.ErrCodecChanged):
		return endCodecChange, true
	case err != nil:
		p.c.errs.Add(1)
		return 0, false
	}
	for _, m := range msgs {
		if err := p.conn.WriteMedia(m); err != nil {
			return writeEnd(err), true
		}
		p.c.messages.Add(1)
	}
	return 0, false
}

// writeEnd classifies a failed write.
func writeEnd(err error) end {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return endDeadline
	}
	return endWriteError
}

// connEnd classifies a conn that ended outside the player's writes: a write
// timeout (of a control message) or the player leaving.
func connEnd(err error) end {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return endDeadline
	}
	return endLeft
}
