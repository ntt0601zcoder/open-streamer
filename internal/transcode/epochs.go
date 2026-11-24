package transcode

import (
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// epochMap numbers rendition epochs. Upstream epoch e maps to e + offset. A
// bump opens a new rendition epoch, (largest issued) + 1, when it takes
// effect at K, and raises the offset so that later upstream epochs map
// above it. Issued numbers strictly increase and are never reused.
type epochMap struct {
	offset  int64
	largest uint32 // largest issued rendition epoch
	up      uint32 // current upstream epoch
	cur     uint32 // current rendition epoch
	started bool
	pending bool
}

// bump requests a new rendition epoch. A bump requested while one is
// pending is absorbed into it.
func (m *epochMap) bump() { m.pending = true }

// isPending reports whether a bump waits for its K.
func (m *epochMap) isPending() bool { return m.pending }

// stale reports whether upstream epoch up is older than the current one.
func (m *epochMap) stale(up uint32) bool { return m.started && up < m.up }

// mapEpoch maps a frame of upstream epoch up that is not K. opened reports a
// rendition epoch issued by this frame. ok is false for a frame of an
// upstream epoch older than the current one, which has no place left.
func (m *epochMap) mapEpoch(up uint32) (r uint32, opened, ok bool) {
	switch {
	case !m.started:
		m.started = true
		m.up, m.cur, m.largest = up, up, up
		return up, true, true
	case up == m.up:
		return m.cur, false, true
	case m.stale(up):
		return 0, false, false
	}
	next := int64(up) + m.offset
	if next <= int64(m.largest) {
		next = int64(m.largest) + 1
		m.offset = next - int64(up)
	}
	m.up, m.cur, m.largest = up, uint32(next), uint32(next)
	return m.cur, true, true
}

// take makes the pending bump take effect at K, a frame of upstream epoch
// up that is not stale, and returns the epoch it issues. newUp reports that K is the first
// frame of upstream epoch up: that upstream epoch is the bump's epoch, not
// the one after it.
func (m *epochMap) take(up uint32) (r uint32, newUp bool) {
	newUp = !m.started || up != m.up
	next := max(int64(m.largest)+1, int64(up)+m.offset)
	if !m.started {
		next = max(next, int64(up))
	}
	m.offset = next - int64(up)
	m.up, m.cur, m.largest = up, uint32(next), uint32(next)
	m.started, m.pending = true, false
	return m.cur, newUp
}

// anchorStore holds the anchor of every recent rendition epoch, shared by
// the mergers of every rendition hub. It is safe for concurrent use.
type anchorStore struct {
	mu    sync.Mutex
	order []uint32
	byID  map[uint32]media.Anchor
}

const maxStoredAnchors = 64

func (s *anchorStore) set(a media.Anchor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[uint32]media.Anchor{}
	}
	if _, ok := s.byID[a.Epoch]; !ok {
		s.order = append(s.order, a.Epoch)
		if len(s.order) > maxStoredAnchors {
			delete(s.byID, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.byID[a.Epoch] = a
}

func (s *anchorStore) get(epoch uint32) (media.Anchor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[epoch]
	return a, ok
}

// pendingAudioTicks bounds the audio buffered while a bump is pending: 1 s.
const pendingAudioTicks = media.ClockRate

// router assigns every source delivery its rendition epoch, shifts it by
// Delay and hands it to the child (encode tracks) or to the mergers (copy
// tracks). It applies the bump rules of the plan's "Rendition epochs":
//
//   - K is the next video keyframe of the source's main video track, or the
//     next frame when that track is not live, whose PTS is above every PTS
//     already sent in the old epoch. After a respawn it is searched for only
//     once the new child has said Hello; after a loss with live video, the
//     Lost delivery itself (the resume keyframe) is not K.
//   - Until K, encode-path video is dropped (DroppedBeforeKey), copy-path
//     video stays in the old epoch, and audio is buffered for at most 1 s,
//     its oldest going out in the old epoch beyond that (to the child only
//     when its queue has room).
//   - At K, buffered audio below K is dropped (DroppedBeforeKey) and the rest
//     goes into the new epoch; afterwards audio below K is dropped whenever
//     it arrives, and the copy path drops RASL frames until the first
//     trailing picture.
//
// It is used by the Run goroutine only.
type router struct {
	childVideo, copyVideo, childAudio, copyAudio bool

	em epochMap

	mainVideo int // lowest-numbered video track, or -1
	warned    bool

	maxDTS       int64 // largest DTS delivered, shifted
	lastVideoDTS int64
	haveDTS      bool
	haveVideo    bool

	needHello bool // the pending bump waits for a new child's Hello
	childFed  bool // the current child takes encode-path frames
	buf       []media.Frame

	expEpoch uint32 // rendition epoch last announced with expectVideo
	expOK    bool
	sentMax  int64 // largest PTS sent in the current rendition epoch
	sentAny  bool
	kPTS     int64 // PTS of the last K, shifted
	haveK    bool
	rasl     bool // copy path: dropping RASL frames after K
	raslKey  int64
	upEpoch  uint32 // upstream epoch of upAnchor
	upAnchor media.Anchor
	upOK     bool

	// Hooks.
	toChild  func(wire.Message) bool // false: not queued
	tryChild func(wire.Message) bool // toChild without waiting; false: not queued
	toCopy   func(f media.Frame)
	// expectVideo announces the main video track of a rendition epoch to
	// the encoded renditions' mergers; see send.
	expectVideo func(epoch uint32, track int)
	setAnchor   func(a media.Anchor)
	srcAnchor   func(up uint32) (media.Anchor, bool)
	now         func() time.Time
	onEpoch     func(epoch uint32, bump bool)
	onLoss      func()
	dropped     func(n uint64) // DroppedBeforeKey
	warn        func(msg string, args ...any)
}

// deliver handles one source delivery.
func (r *router) deliver(d hub.Delivery) {
	f := d.Frame
	kind := f.Kind()
	switch kind {
	case media.KindVideo:
		if r.mainVideo < 0 || f.Track < r.mainVideo {
			r.mainVideo = f.Track
		}
		if f.Track != r.mainVideo {
			if !r.warned {
				r.warned = true
				r.warn("transcode: dropping a second video track", "track", f.Track, "main", r.mainVideo)
			}
			return
		}
	case media.KindAudio:
	default:
		return
	}
	f.PTS += Delay
	f.DTS += Delay
	if !r.haveDTS || f.DTS > r.maxDTS {
		r.maxDTS, r.haveDTS = f.DTS, true
	}
	if kind == media.KindVideo {
		r.lastVideoDTS, r.haveVideo = f.DTS, true
	}

	notK := false
	if d.Lost > 0 {
		r.onLoss()
		// A loss absorbed into a pending bump needs no second Discontinuity:
		// the child gets nothing more until K.
		if r.childFed && !r.em.isPending() {
			_ = r.toChild(wire.Discontinuity{})
		}
		r.em.bump()
		// With live video, the Lost delivery is the resume keyframe and K is
		// the keyframe after it, so the audio that leads K can be collected.
		notK = r.liveVideo()
	}

	if !r.em.isPending() {
		r.normal(f)
		return
	}
	if r.em.stale(f.Epoch) {
		// A straggler from an older upstream epoch is never K: it would move
		// the map backwards.
		r.dropped(1)
		return
	}
	if !notK && r.isK(f) {
		r.takeK(f)
		return
	}
	r.beforeK(f)
}

// childExited records the loss of the current child: a bump whose K waits
// for the next child's Hello.
func (r *router) childExited() {
	r.childFed = false
	r.needHello = true
	r.em.bump()
}

// overflowBeforeHello records a message dropped because the current child's
// pre-Hello backlog is full: the child is no longer fed, and a bump feeds it
// again from K once it says Hello.
func (r *router) overflowBeforeHello() {
	r.dropped(1)
	if !r.childFed {
		return
	}
	r.childFed = false
	r.needHello = true
	r.em.bump()
}

// childHello records the current child's Hello.
func (r *router) childHello() { r.needHello = false }

// liveVideo reports whether the main video track is live in the hub's sense.
func (r *router) liveVideo() bool {
	return r.haveVideo && r.maxDTS-r.lastVideoDTS <= hub.LiveWindow
}

func (r *router) isK(f media.Frame) bool {
	if r.needHello {
		return false
	}
	if r.liveVideo() && (f.Kind() != media.KindVideo || !f.Key) {
		return false
	}
	return !r.sentAny || f.PTS > r.sentMax
}

// normal handles a frame while no bump is pending.
func (r *router) normal(f media.Frame) {
	ep, opened, ok := r.em.mapEpoch(f.Epoch)
	if !ok {
		r.dropped(1)
		return
	}
	if opened {
		r.openUpstream(f, ep)
	}
	if f.Kind() == media.KindAudio && r.haveK && f.PTS < r.kPTS {
		r.dropped(1)
		return
	}
	f.Epoch = ep
	r.send(f)
}

// openUpstream records the anchor of rendition epoch ep, opened by f, the
// first frame of its upstream epoch: Start and Wall shifted by Delay.
func (r *router) openUpstream(f media.Frame, ep uint32) {
	up := r.upstreamAnchor(f)
	r.setAnchor(media.Anchor{Epoch: ep, Start: up.Start + Delay, Wall: up.Wall.Add(media.ToDuration(Delay))})
	r.sentAny, r.haveK, r.rasl = false, false, false
	r.onEpoch(ep, false)
}

// upstreamAnchor returns the anchor of f's upstream epoch, from the source
// hub, or a stand-in at f's DTS and now when the hub no longer holds it.
func (r *router) upstreamAnchor(f media.Frame) media.Anchor {
	if r.upOK && r.upEpoch == f.Epoch {
		return r.upAnchor
	}
	a, ok := r.srcAnchor(f.Epoch)
	if !ok {
		a = media.Anchor{Epoch: f.Epoch, Start: f.DTS - Delay, Wall: r.now()}
	}
	r.upEpoch, r.upAnchor, r.upOK = f.Epoch, a, true
	return a
}

// takeK makes the pending bump take effect at f.
func (r *router) takeK(f media.Frame) {
	ep, newUp := r.em.take(f.Epoch)
	up := r.upstreamAnchor(f)
	if newUp {
		r.setAnchor(media.Anchor{Epoch: ep, Start: up.Start + Delay, Wall: up.Wall.Add(media.ToDuration(Delay))})
	} else {
		k := f.PTS - Delay
		r.setAnchor(media.Anchor{
			Epoch: ep,
			Start: f.PTS,
			Wall:  up.Wall.Add(media.ToDuration(k - up.Start)).Add(media.ToDuration(Delay)),
		})
	}
	r.onEpoch(ep, true)
	r.sentAny = false
	r.kPTS, r.haveK = f.PTS, true
	r.rasl = f.Kind() == media.KindVideo
	r.raslKey = f.PTS
	if !r.needHello && r.toChild != nil && (r.childVideo || r.childAudio) {
		r.childFed = true
	}
	f.Epoch = ep
	r.send(f)
	var drop uint64
	for _, a := range r.buf {
		if a.PTS < r.kPTS {
			drop++
			continue
		}
		a.Epoch = ep
		r.send(a)
	}
	clear(r.buf)
	r.buf = r.buf[:0]
	if drop > 0 {
		r.dropped(drop)
	}
}

// beforeK handles a frame while the bump waits for K.
func (r *router) beforeK(f media.Frame) {
	if f.Kind() == media.KindAudio {
		r.buf = append(r.buf, f)
	} else {
		if r.childVideo {
			r.dropped(1)
		}
		if r.copyVideo {
			if ep, opened, ok := r.em.mapEpoch(f.Epoch); ok {
				if opened {
					r.openUpstream(f, ep)
				}
				f.Epoch = ep
				_ = r.sendCopyVideo(f)
			}
		}
	}
	// Audio buffered for over 1 s goes out in the old epoch.
	for len(r.buf) > 0 && r.maxDTS-r.buf[0].DTS > pendingAudioTicks {
		a := r.buf[0]
		r.buf[0] = media.Frame{}
		r.buf = r.buf[1:]
		ep, opened, ok := r.em.mapEpoch(a.Epoch)
		if !ok || r.haveK && a.PTS < r.kPTS {
			r.dropped(1)
			continue
		}
		if opened {
			r.openUpstream(a, ep)
		}
		a.Epoch = ep
		r.sendEvicted(a)
	}
}

// sendEvicted sends audio evicted from the pending buffer in the old epoch.
// It never waits on the child: a pending bump must not block the source
// subscription, or every keyframe would arrive as a Lost resume keyframe
// and K would never come while the child is slow.
func (r *router) sendEvicted(a media.Frame) {
	switch {
	case r.copyAudio:
		r.toCopy(a)
	case r.childFed && r.tryChild(wire.Frame{Rung: -1, F: a}):
	default:
		r.dropped(1)
		return
	}
	r.noteSent(a.PTS)
}

// send hands f, already in its rendition epoch, to its paths. Only a frame
// that some path took counts as sent. Before the first video frame of a
// rendition epoch goes to the child, the encoded renditions' mergers are
// told to expect that video track in the epoch (expectVideo).
func (r *router) send(f media.Frame) {
	sent := false
	switch f.Kind() {
	case media.KindVideo:
		if r.copyVideo {
			sent = r.sendCopyVideo(f)
		}
		if r.childVideo {
			r.expectChildVideo(f)
			sent = r.sendChild(f) || sent
		}
	case media.KindAudio:
		if r.copyAudio {
			r.toCopy(f)
			sent = true
		} else {
			sent = r.sendChild(f)
		}
	}
	if sent {
		r.noteSent(f.PTS)
	}
}

// expectChildVideo announces f's track in f's epoch once per epoch, while
// the child is fed: the child's output for the epoch can only follow this
// call, so its audio never reaches a merger that does not yet expect the
// video.
func (r *router) expectChildVideo(f media.Frame) {
	if !r.childFed || r.expectVideo == nil || r.expOK && r.expEpoch == f.Epoch {
		return
	}
	r.expEpoch, r.expOK = f.Epoch, true
	r.expectVideo(f.Epoch, f.Track)
}

// sendChild hands f to the child when it is fed, and reports whether it was
// queued. A frame the child does not take is counted in DroppedBeforeKey
// (an overflow counts itself).
func (r *router) sendChild(f media.Frame) bool {
	if !r.childFed {
		r.dropped(1)
		return false
	}
	return r.toChild(wire.Frame{Rung: -1, F: f})
}

// sendCopyVideo hands a video frame to the copy renditions, dropping the
// RASL frames after K up to the first trailing picture, as the hub does on
// resume.
func (r *router) sendCopyVideo(f media.Frame) bool {
	if r.rasl {
		if f.RASL {
			return false
		}
		if f.PTS > r.raslKey {
			r.rasl = false
		}
	}
	r.toCopy(f)
	r.noteSent(f.PTS)
	return true
}

func (r *router) noteSent(pts int64) {
	if !r.sentAny || pts > r.sentMax {
		r.sentMax, r.sentAny = pts, true
	}
}
