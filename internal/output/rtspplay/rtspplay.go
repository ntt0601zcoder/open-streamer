// Package rtspplay serves streams to RTSP players.
//
// A Server owns a pkg/rtsp server and is its Handler: a player of
// rtsp://host:port/<app>/<name> gets the hub that lookup finds for <name>.
// A stream can be played once its hub is live (hub.Live) and has a live
// track RTP can carry; DESCRIBE then lists the lowest-numbered such track
// of each kind (rtpmux.Tracks). An unknown name, another app and a stream
// that is not live yet all get the server's one 404 answer. A session's
// SDP is frozen and a frame of a kind it lacks ends the session, so a
// stream whose video is live but whose audio is not yet (its first
// keyframe came first) is described once audio comes, or video-only after
// Options.DescribeWait.
//
// Each session reads the hub through its own subscription and keeps one
// rtpmux.Muxer, fed in the session's goroutine together with a 1 s sender
// report timer (the muxer is not safe for concurrent use). A TCP session
// subscribes with the GOP cache (hub.SubscribeCached), so it starts at once
// on the cached keyframe and TCP backpressure paces the replay; a UDP
// session subscribes live (hub.SubscribeLive) and starts at the next
// keyframe, since a replay at line rate would overflow the client's
// receive buffer. A delivery that reports lost frames resyncs the muxer,
// which resumes at the next keyframe with its parameter sets.
//
// A codec change, a frame the muxer cannot send, the hub closing and the
// session's carried tracks going silent (none of its set-up tracks has
// delivered a frame for hub.LiveWindow of stream time while the hub goes on
// delivering others, as when a failover renumbers the tracks) end the
// session with a BYE per track; players reconnect and DESCRIBE again. A
// write that fails or misses the server's write timeout ends it at once,
// so a stalled player never holds up the hub or other players.
package rtspplay

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	rtpmux "github.com/ntt0601zcoder/open-streamer/internal/mux/rtp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

const (
	defaultApp     = "live"
	defaultQueue   = 256
	reportInterval = time.Second
	// defaultDescribeWait is how long Describe waits for audio on a stream
	// whose video is live but whose audio is not yet.
	defaultDescribeWait = time.Second
	// describePoll is how often Describe looks for audio while it waits.
	describePoll = 10 * time.Millisecond
	// noAudioMemo is how long a stream that showed no audio for a whole
	// wait is answered at once: other players, and a player that SETUPs
	// without a DESCRIBE, do not wait again.
	noAudioMemo = 10 * time.Second
)

// Options tunes a Server. Zero values select the defaults.
type Options struct {
	// App is the first path segment of play URLs, slashes trimmed; "" means
	// "live".
	App string
	// MTU is the RTP payload size; 0 means 1200.
	MTU int
	// Queue is a session's subscription queue: the smallest one for TCP
	// sessions, which grows to the cached frames plus 64
	// (hub.SubscribeCached), and the whole queue for UDP sessions. 0 means
	// 256.
	Queue int
	// DescribeWait is how long Describe waits for an audio config when the
	// hub is live with video but no audio track yet (the first keyframe
	// often comes before the first audio frame, and a session whose SDP
	// lacks audio ends when audio comes); a stream still without audio is
	// then described video-only, and for the next 10 s without waiting. 0
	// means 1 s; negative disables the wait.
	DescribeWait time.Duration
	// Server configures the RTSP server: timeouts, caps and UDPHost, which
	// enables UDP sessions.
	Server rtsp.ServerConfig
}

// Stats counts a Server's players.
type Stats struct {
	// Players is the number of sessions being played; PlayersStarted counts
	// every session played.
	Players        int
	PlayersStarted uint64
	// Frames counts the hub frames handed to the sessions' muxers, Packets
	// the RTP packets and Reports the RTCP sender reports written.
	Frames, Packets, Reports uint64
	// Resyncs counts deliveries that reported lost frames.
	Resyncs uint64
	// SkippedTracks counts, once per session played, the hub tracks live
	// at PLAY that its SDP leaves out: a second track of a kind, a codec
	// RTP does not carry here, or a track that came after the SETUP.
	SkippedTracks uint64
	// DescribeWaits counts the Describes that waited for audio, and
	// DescribeWaitsTimedOut those that still had no audio at the end of the
	// whole wait and answered video-only.
	DescribeWaits, DescribeWaitsTimedOut uint64
	// Why sessions ended: a write that missed the write timeout, another
	// failed write, a codec change, a frame the muxer could not send, the
	// hub closing, the carried tracks going silent, TEARDOWN, a UDP session
	// timeout, the client closing (or resetting) its connection, and Close.
	EndedDeadline, EndedWriteError, EndedCodecChange, EndedFrameError uint64
	EndedHubClosed, EndedTracksGone, EndedTeardown, EndedTimeout      uint64
	EndedLeft, EndedShutdown                                          uint64
	// Mux sums the muxer counters of every ended session.
	Mux rtpmux.MuxStats
	// Server is the RTSP server's counters.
	Server rtsp.ServerStats
}

// counters are the Stats a session updates as it goes.
type counters struct {
	players                                             atomic.Int64
	started, frames, packets, reports, resyncs, skipped atomic.Uint64
	waits, waitsTimedOut                                atomic.Uint64
}

// Server plays hub streams to RTSP players; it is the rtsp.Handler of its
// own rtsp.Server. It is safe for concurrent use.
type Server struct {
	lookup  func(name string) (*hub.Hub, bool)
	opt     Options
	srv     *rtsp.Server
	closing atomic.Bool
	done    chan struct{} // closed by Close: Describe stops waiting
	once    sync.Once

	live counters

	mu      sync.Mutex
	ended   Stats                  // the Ended fields and Mux
	noAudio map[*hub.Hub]time.Time // hubs that showed no audio for a whole wait, and when
}

// New returns a Server for the hubs that lookup finds by stream name.
func New(lookup func(name string) (*hub.Hub, bool), opt Options) *Server {
	opt.App = strings.Trim(opt.App, "/")
	if opt.App == "" {
		opt.App = defaultApp
	}
	if opt.MTU <= 0 {
		opt.MTU = rtpmux.DefaultMTU
	}
	if opt.Queue <= 0 {
		opt.Queue = defaultQueue
	}
	if opt.DescribeWait == 0 {
		opt.DescribeWait = defaultDescribeWait
	}
	s := &Server{lookup: lookup, opt: opt, done: make(chan struct{}), noAudio: map[*hub.Hub]time.Time{}}
	s.srv = rtsp.NewServer(s, opt.Server)
	return s
}

// Serve accepts players on l until Close, then returns rtsp.ErrServerClosed.
// The first Serve binds the UDP pair when Options.Server.UDPHost is set.
func (s *Server) Serve(l net.Listener) error { return s.srv.Serve(l) }

// Close stops every listener, ends every session with a BYE (best effort)
// and waits for them. It is idempotent.
func (s *Server) Close() error {
	s.closing.Store(true)
	s.once.Do(func() { close(s.done) })
	return s.srv.Close()
}

// UDPPorts returns the RTP and RTCP ports of the UDP pair, or zeros while
// there is none.
func (s *Server) UDPPorts() [2]int { return s.srv.UDPPorts() }

// Stats returns the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	st := s.ended
	s.mu.Unlock()
	st.Players = int(s.live.players.Load())
	st.PlayersStarted = s.live.started.Load()
	st.Frames = s.live.frames.Load()
	st.Packets = s.live.packets.Load()
	st.Reports = s.live.reports.Load()
	st.Resyncs = s.live.resyncs.Load()
	st.SkippedTracks = s.live.skipped.Load()
	st.DescribeWaits = s.live.waits.Load()
	st.DescribeWaitsTimedOut = s.live.waitsTimedOut.Load()
	st.Server = s.srv.Stats()
	return st
}

// stream returns the hub of a normalised path <app>/<name>.
func (s *Server) stream(path string) (*hub.Hub, string, bool) {
	name, ok := strings.CutPrefix(path, s.opt.App+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return nil, "", false
	}
	h, ok := s.lookup(name)
	if !ok || h == nil {
		return nil, "", false
	}
	return h, name, true
}

// Describe implements rtsp.Handler: the carried tracks of the live stream
// at path, each descriptor's ID its hub track. A stream whose video is live
// but whose audio is not yet is described once audio comes, or video-only
// after Options.DescribeWait.
func (s *Server) Describe(path string) ([]rtsp.TrackDesc, bool) {
	h, _, ok := s.stream(path)
	if !ok || !h.Live() {
		return nil, false
	}
	cfgs := h.TrackConfigs()
	if has(cfgs, media.KindVideo) && !has(cfgs, media.KindAudio) {
		cfgs = s.waitAudio(h, cfgs)
	}
	descs, err := rtpmux.Tracks(cfgs)
	if err != nil {
		return nil, false
	}
	return descs, true
}

// waitAudio polls h's track configs until one is audio, the wait ends, the
// hub closes or the server closes, and returns the last configs seen. It
// does not wait for a hub that showed no audio for a whole wait in the
// last 10 s.
func (s *Server) waitAudio(h *hub.Hub, cfgs []hub.TrackConfig) []hub.TrackConfig {
	if s.opt.DescribeWait < 0 || s.recentlyNoAudio(h) {
		return cfgs
	}
	s.live.waits.Add(1)
	deadline := time.NewTimer(s.opt.DescribeWait)
	defer deadline.Stop()
	poll := time.NewTicker(describePoll)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			// Audio may have come since the last poll: only a stream still
			// without it timed out and is remembered as audio-less.
			if cfgs = h.TrackConfigs(); !has(cfgs, media.KindAudio) {
				s.live.waitsTimedOut.Add(1)
				s.markNoAudio(h)
			}
			return cfgs
		case <-h.Done():
			return nil
		case <-s.done:
			return cfgs
		case <-poll.C:
			if cfgs = h.TrackConfigs(); has(cfgs, media.KindAudio) {
				return cfgs
			}
		}
	}
}

// recentlyNoAudio reports whether h showed no audio for a whole wait in the
// last 10 s.
func (s *Server) recentlyNoAudio(h *hub.Hub) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.noAudio[h]
	return ok && time.Since(at) < noAudioMemo
}

// markNoAudio records that h showed no audio for a whole wait, forgetting
// the records that have expired.
func (s *Server) markNoAudio(h *hub.Hub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, at := range s.noAudio {
		if now.Sub(at) >= noAudioMemo {
			delete(s.noAudio, k)
		}
	}
	s.noAudio[h] = now
}

// has reports whether cfgs holds a track of kind k.
func has(cfgs []hub.TrackConfig, k media.Kind) bool {
	for _, c := range cfgs {
		if c.Kind == k {
			return true
		}
	}
	return false
}

// end is why a session ended.
type end uint8

const (
	endShutdown end = iota
	endLeft
	endTeardown
	endTimeout
	endDeadline
	endWriteError
	endCodecChange
	endFrameError
	endHubClosed
	endTracksGone
)

// Play implements rtsp.Handler: it plays the session's stream until the
// session ends, then ends the session with a BYE per track when it is still
// open.
func (s *Server) Play(ctx context.Context, ss *rtsp.ServerSession) {
	h, name, ok := s.stream(ss.Path())
	if !ok {
		return // the stream went away after SETUP: the server sends BYE
	}
	var sub *hub.Sub
	if ss.UDP() {
		sub = h.SubscribeLive("rtsp "+name, s.opt.Queue)
	} else {
		sub = h.SubscribeCached("rtsp "+name, s.opt.Queue)
	}
	defer sub.Close()
	p := newPlayer(s, ss)
	// Counted before the start, so a reader of Stats that sees the session
	// sees its skipped tracks.
	s.live.skipped.Add(p.skipped(h.TrackConfigs()))
	s.live.started.Add(1)
	s.live.players.Add(1)
	why := p.run(ctx, sub)
	// Unsubscribed, closed and uncounted before the end is counted, so a
	// reader of Stats that sees the end sees the session gone.
	sub.Close()
	ss.Close()
	s.live.players.Add(-1)
	s.finish(why, p.mux.Stats())
}

// finish counts an ended session.
func (s *Server) finish(why end, ms rtpmux.MuxStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.ended
	st.Mux.IgnoredFrames += ms.IgnoredFrames
	st.Mux.DroppedBeforeKey += ms.DroppedBeforeKey
	st.Mux.DroppedEarlyAudio += ms.DroppedEarlyAudio
	st.Mux.DroppedBeforeBase += ms.DroppedBeforeBase
	st.Mux.Resyncs += ms.Resyncs
	st.Mux.Packets += ms.Packets
	st.Mux.Octets += ms.Octets
	switch why {
	case endShutdown:
		st.EndedShutdown++
	case endLeft:
		st.EndedLeft++
	case endTeardown:
		st.EndedTeardown++
	case endTimeout:
		st.EndedTimeout++
	case endDeadline:
		st.EndedDeadline++
	case endWriteError:
		st.EndedWriteError++
	case endCodecChange:
		st.EndedCodecChange++
	case endFrameError:
		st.EndedFrameError++
	case endHubClosed:
		st.EndedHubClosed++
	case endTracksGone:
		st.EndedTracksGone++
	}
}

// player is one session being played.
type player struct {
	s   *Server
	ss  *rtsp.ServerSession
	mux *rtpmux.Muxer
	// carried holds the hub tracks of the set-up SDP tracks, listed those
	// of every SDP track.
	carried, listed map[int]bool
	// last is the DTS of the last frame of a carried track, or of the
	// session's first frame until one comes; seen reports that it is set.
	last int64
	seen bool
}

// newPlayer builds the muxer of a session from its frozen tracks: one
// rtpmux.Track per SDP entry, with the RTP origin of each set-up one.
func newPlayer(s *Server, ss *rtsp.ServerSession) *player {
	descs := ss.TrackDescs()
	setup := map[int]bool{}
	for _, i := range ss.Tracks() {
		setup[i] = true
	}
	p := &player{s: s, ss: ss, carried: map[int]bool{}, listed: map[int]bool{}}
	tracks := make([]rtpmux.Track, len(descs))
	for i, d := range descs {
		ssrc, _, base := ss.Origin(i) // zeros for a track not set up
		tracks[i] = rtpmux.Track{SDPIndex: i, HubTrack: d.ID, Desc: d, Setup: setup[i], SSRC: ssrc, TSBase: base}
		p.listed[d.ID] = true
		if setup[i] {
			p.carried[d.ID] = true
		}
	}
	p.mux = rtpmux.NewMuxer(tracks, s.opt.MTU, time.Now)
	return p
}

// skipped counts the live hub tracks of cfgs that the session's SDP leaves
// out.
func (p *player) skipped(cfgs []hub.TrackConfig) uint64 {
	var n uint64
	for _, c := range cfgs {
		if !p.listed[c.Track] {
			n++
		}
	}
	return n
}

// run moves deliveries from sub to the session and sends the sender reports
// that fall due, until the session must end, and reports why.
func (p *player) run(ctx context.Context, sub *hub.Sub) end {
	t := time.NewTicker(reportInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return p.sessionEnd(nil)
		case now := <-t.C:
			if err := p.writeReports(p.mux.Reports(now)); err != nil {
				return p.sessionEnd(err)
			}
		case d, ok := <-sub.C():
			if !ok {
				return p.stopped(endHubClosed)
			}
			if why, done := p.deliver(d); done {
				return why
			}
		}
	}
}

// deliver muxes and writes one delivery; done reports that the session must
// end, and why.
func (p *player) deliver(d hub.Delivery) (why end, done bool) {
	if d.Lost > 0 {
		p.mux.Resync()
		p.s.live.resyncs.Add(1)
	}
	if p.gone(d.Frame) {
		return p.stopped(endTracksGone), true
	}
	p.s.live.frames.Add(1)
	pk, reports, err := p.mux.Frame(d.Frame)
	switch {
	case errors.Is(err, rtpmux.ErrCodecChanged):
		return p.stopped(endCodecChange), true
	case err != nil:
		return p.stopped(endFrameError), true
	}
	if pk != nil {
		if err := p.ss.WritePackets(pk.SDPIndex, pk.RTP); err != nil {
			return p.sessionEnd(err), true
		}
		p.s.live.packets.Add(uint64(len(pk.RTP)))
	}
	if err := p.writeReports(reports); err != nil {
		return p.sessionEnd(err), true
	}
	return 0, false
}

// gone reports whether f, a frame of no carried track, comes more than
// hub.LiveWindow of stream time after the last frame of a carried track
// (or after the session's first frame, before any of them), and records
// the frames of carried tracks.
func (p *player) gone(f media.Frame) bool {
	switch {
	case p.carried[f.Track]:
		if !p.seen || f.DTS > p.last {
			p.last = f.DTS
		}
		p.seen = true
		return false
	case !p.seen:
		p.last, p.seen = f.DTS, true
		return false
	}
	return f.DTS-p.last > hub.LiveWindow
}

// writeReports writes sender reports on their tracks' RTCP channels.
func (p *player) writeReports(reports []rtpmux.Report) error {
	for _, r := range reports {
		if err := p.ss.WriteRTCP(r.SDPIndex, r.RTCP); err != nil {
			return err // classified by sessionEnd, never returned further
		}
		p.s.live.reports.Add(1)
	}
	return nil
}

// stopped is why for a session the player ends itself, unless the server is
// shutting down.
func (p *player) stopped(why end) end {
	if p.s.closing.Load() {
		return endShutdown
	}
	return why
}

// sessionEnd classifies a session that ended on its own or whose write,
// which returned writeErr, failed. A write that ended the session is the
// session's error; when Close is cutting a write the session has none yet.
func (p *player) sessionEnd(writeErr error) end {
	if p.s.closing.Load() {
		return endShutdown
	}
	err := p.ss.Err()
	if writeErr != nil && (err == nil || errors.Is(err, writeErr)) {
		switch {
		case errors.Is(writeErr, os.ErrDeadlineExceeded):
			return endDeadline
		case errors.Is(writeErr, syscall.EPIPE), errors.Is(writeErr, syscall.ECONNRESET):
			return endLeft // the player closed its connection under the write
		}
		return endWriteError
	}
	switch {
	case errors.Is(err, rtsp.ErrTeardown):
		return endTeardown
	case errors.Is(err, rtsp.ErrSessionClosed):
		return endShutdown
	case errors.Is(err, os.ErrDeadlineExceeded):
		return endTimeout // a UDP session without requests or RTCP
	}
	return endLeft
}
