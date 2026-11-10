// Package hub fans frames out to subscribers without ever blocking the writer.
//
// One Hub serves one (stream, rendition). Write hands each frame to every
// subscriber's bounded queue with a non-blocking send. A subscriber whose
// queue is full enters skip mode and resumes on whole GOPs, and the first
// frame it gets after a gap carries the number of frames it lost.
//
// # Live video tracks
//
// Stream time is the largest DTS written so far. A video track is
// live while stream time minus the DTS of that track's last frame is at most
// LiveWindow (10 s). A track that has sent nothing for longer than that has
// ended: it no longer holds audio back, and it no longer anchors the cache.
// It becomes live again with its next frame.
//
// # Gates
//
// Each subscriber gates each video track. A gated track drops frames until
// its next keyframe; after it, RASL frames are dropped until the track's
// first trailing picture: a non-RASL frame with PTS above the keyframe's,
// which opens the gate. Leading pictures are presented before their
// keyframe, so a RADL is delivered but leaves the gate in place. Frames
// that are not video (audio, or frames without a config) are delivered only
// when every live video track has had its keyframe delivered. A new subscriber starts
// with every video track gated, including tracks the hub has not seen yet,
// so it never gets a video frame before that track's keyframe.
//
// # Skip mode and Lost
//
// A failed send drops the frame, gates every video track again and puts the
// subscriber in skip mode. Skip mode ends at the next delivery, which carries
// Delivery.Lost: the failed send plus every frame the gates dropped in
// between. With a live video track that delivery is always a resume keyframe,
// so the gap is signalled exactly once, at a point where outputs can start a
// new epoch. Frames the gates drop after that delivery (RASL frames after the
// resume CRA, a second video track still waiting for its keyframe, audio held
// back for it) count in SubStats.Dropped but not in Lost, the same as frames
// gated after a subscribe or for a video track that is new to the
// subscriber; they never land mid-GOP as a spurious gap. A track that the
// subscriber was following stays open across a pause, so a track that comes
// back after ending is gated only for subscribers that skipped while it was
// away.
//
// # Cache
//
// With a live video track, the hub caches frames from the last keyframe of
// the lowest-numbered live video track (the anchor), capped at
// Options.GOPCacheFrames; a longer GOP empties the cache until the anchor's
// next keyframe, and a change of anchor empties it too. With no live video
// track, the cache holds the frames whose DTS is less than
// Options.AudioCacheTicks behind stream time. Subscribe replays the cache
// through the new subscriber's gates when it fits in the queue; otherwise
// the subscriber waits, gated, for the next keyframes.
//
// The hub starts no goroutines. Every state change and channel operation
// happens under one mutex, so Close never races a send.
package hub

import (
	"slices"
	"sync"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// LiveWindow is how long, in stream time, a video track stays live after its
// last frame.
const LiveWindow = 10 * media.ClockRate

const (
	defaultGOPCacheFrames  = 1500
	defaultAudioCacheTicks = media.ClockRate
	defaultQueue           = 1024
)

// Options tunes a Hub. Zero values select the defaults.
type Options struct {
	GOPCacheFrames  int   // 0 means 1500
	AudioCacheTicks int64 // audio-only streams: cache span; 0 means 90000 (1 s)
}

// Delivery is one frame handed to a subscriber. Lost is non-zero only on the
// first delivery after a failed send: the number of frames dropped for this
// subscriber since its previous delivery.
type Delivery struct {
	Frame media.Frame
	Lost  uint64
}

// SubStats counts one open subscriber's frames. Delivered includes frames
// replayed from the cache. Dropped counts written frames the subscriber did
// not get, for any reason: a full queue or a closed gate.
type SubStats struct {
	Name               string
	Delivered, Dropped uint64
}

// gate is one subscriber's state for one video track.
type gate uint8

const (
	gateKey  gate = iota // dropping frames until the next keyframe
	gateRASL             // keyframe delivered; dropping RASL frames until the first trailing picture
	gateOpen             // delivering every frame
)

// trackGate is one subscriber's gate for one video track, with the PTS of the
// keyframe that passed gateKey.
type trackGate struct {
	state  gate
	keyPTS int64
}

// videoTrack is the hub's view of one video track.
type videoTrack struct {
	lastDTS int64
}

// Hub fans frames out to subscribers. It is safe for concurrent use.
type Hub struct {
	gopCacheFrames  int
	audioCacheTicks int64

	mu      sync.Mutex
	closed  bool
	subs    []*Sub
	video   map[int]*videoTrack
	latest  int64 // stream time: the largest DTS written
	started bool  // latest is set
	anchor  int   // track the cache is anchored on, or -1 for the audio cache
	cache   []media.Frame
}

// Sub is one subscriber. Its methods are safe for concurrent use.
type Sub struct {
	h    *Hub
	name string
	ch   chan Delivery

	// Guarded by h.mu.
	closed    bool
	gates     map[int]trackGate // video track → gate; a missing track is gateKey
	skipping  bool
	lost      uint64
	delivered uint64
	dropped   uint64
}

// New returns an empty hub.
func New(opt Options) *Hub {
	h := &Hub{
		gopCacheFrames:  opt.GOPCacheFrames,
		audioCacheTicks: opt.AudioCacheTicks,
		video:           map[int]*videoTrack{},
		anchor:          -1,
	}
	if h.gopCacheFrames <= 0 {
		h.gopCacheFrames = defaultGOPCacheFrames
	}
	if h.audioCacheTicks <= 0 {
		h.audioCacheTicks = defaultAudioCacheTicks
	}
	return h
}

// Write hands f to every subscriber. It never blocks on a subscriber, and it
// does nothing after Close.
func (h *Hub) Write(f media.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if !h.started || f.DTS > h.latest {
		h.latest = f.DTS
		h.started = true
	}
	if f.Kind() == media.KindVideo {
		vt := h.video[f.Track]
		if vt == nil {
			vt = &videoTrack{}
			h.video[f.Track] = vt
		}
		vt.lastDTS = f.DTS
	}
	h.updateCache(f)
	for _, s := range h.subs {
		h.offer(s, f)
	}
}

// Subscribe adds a subscriber with a queue of the given length (0 means
// 1024) and replays the cache into it when the cache fits. On a closed hub
// it returns a subscriber whose channel is already closed.
func (h *Hub) Subscribe(name string, queue int) *Sub {
	if queue <= 0 {
		queue = defaultQueue
	}
	s := &Sub{h: h, name: name, ch: make(chan Delivery, queue), gates: map[int]trackGate{}}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		s.closed = true
		close(s.ch)
		return s
	}
	if len(h.cache) <= queue {
		for _, f := range h.cache {
			if !h.admit(s, f) {
				continue
			}
			select {
			case s.ch <- Delivery{Frame: f}:
				s.delivered++
			default: // unreachable: the cache fits in the fresh queue
			}
		}
	}
	h.subs = append(h.subs, s)
	return s
}

// Stats returns the counters of every open subscriber, in subscription
// order.
func (h *Hub) Stats() []SubStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SubStats, 0, len(h.subs))
	for _, s := range h.subs {
		out = append(out, SubStats{Name: s.name, Delivered: s.delivered, Dropped: s.dropped})
	}
	return out
}

// Close closes every subscriber's channel and drops the cache. Later Writes
// do nothing. Close is idempotent.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, s := range h.subs {
		s.closed = true
		close(s.ch)
	}
	h.subs = nil
	h.resetCache()
}

// C returns the subscriber's delivery channel. It is closed by Close on the
// subscriber or the hub; deliveries already queued stay readable.
func (s *Sub) C() <-chan Delivery { return s.ch }

// Close removes the subscriber and closes its channel. It is idempotent.
func (s *Sub) Close() {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
	h.subs = slices.DeleteFunc(h.subs, func(x *Sub) bool { return x == s })
}

// offer delivers f to s, or drops it. Called with h.mu held.
func (h *Hub) offer(s *Sub, f media.Frame) {
	if !h.admit(s, f) {
		s.dropped++
		if s.skipping {
			s.lost++
		}
		return
	}
	select {
	case s.ch <- Delivery{Frame: f, Lost: s.lost}:
		s.delivered++
		s.lost = 0
		s.skipping = false
	default:
		s.dropped++
		s.lost++
		s.skipping = true
		clear(s.gates)
	}
}

// admit applies s's gates to f, advancing the gate of f's video track when f
// passes. Called with h.mu held.
func (h *Hub) admit(s *Sub, f media.Frame) bool {
	switch f.Kind() {
	case media.KindVideo:
		g := s.gates[f.Track]
		switch g.state {
		case gateKey:
			if !f.Key {
				return false
			}
			s.gates[f.Track] = trackGate{state: gateRASL, keyPTS: f.PTS}
		case gateRASL:
			if f.RASL {
				return false
			}
			if f.PTS > g.keyPTS {
				s.gates[f.Track] = trackGate{state: gateOpen}
			}
		case gateOpen:
		}
		return true
	case media.KindAudio:
		return h.videoStarted(s)
	default:
		return h.videoStarted(s)
	}
}

// videoStarted reports whether every live video track has had its keyframe
// delivered to s. Called with h.mu held.
func (h *Hub) videoStarted(s *Sub) bool {
	for id, vt := range h.video {
		if h.live(vt) && s.gates[id].state == gateKey {
			return false
		}
	}
	return true
}

func (h *Hub) live(vt *videoTrack) bool {
	return h.latest-vt.lastDTS <= LiveWindow
}

// updateCache adds f to the cache after Write recorded it. Called with h.mu
// held.
func (h *Hub) updateCache(f media.Frame) {
	anchor := -1
	for id, vt := range h.video {
		if h.live(vt) && (anchor < 0 || id < anchor) {
			anchor = id
		}
	}
	if anchor != h.anchor {
		h.anchor = anchor
		h.resetCache()
	}

	if anchor < 0 {
		h.cache = append(h.cache, f)
		n := 0
		for n < len(h.cache) && (h.latest-h.cache[n].DTS >= h.audioCacheTicks ||
			len(h.cache)-n > h.gopCacheFrames) {
			n++
		}
		h.dropCacheFront(n)
		return
	}

	switch {
	case f.Track == anchor && f.Kind() == media.KindVideo && f.Key:
		h.resetCache()
		h.cache = append(h.cache, f)
	case len(h.cache) == 0:
		// Waiting for the anchor's keyframe.
	case len(h.cache) >= h.gopCacheFrames:
		h.resetCache() // over-long GOP
	default:
		h.cache = append(h.cache, f)
	}
}

// resetCache empties the cache, releasing the frames it referenced.
func (h *Hub) resetCache() {
	clear(h.cache)
	h.cache = h.cache[:0]
}

// dropCacheFront removes the first n cached frames.
func (h *Hub) dropCacheFront(n int) {
	if n == 0 {
		return
	}
	m := copy(h.cache, h.cache[n:])
	clear(h.cache[m:])
	h.cache = h.cache[:m]
}
