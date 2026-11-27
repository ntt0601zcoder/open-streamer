package transcode

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

func TestEpochMap(t *testing.T) {
	var m epochMap
	var issued []uint32
	mapUp := func(up uint32) uint32 {
		t.Helper()
		r, opened, ok := m.mapEpoch(up)
		require.True(t, ok)
		if opened {
			issued = append(issued, r)
		}
		return r
	}
	take := func(up uint32) uint32 {
		t.Helper()
		require.True(t, m.isPending())
		r, _ := m.take(up)
		issued = append(issued, r)
		return r
	}

	assert.Equal(t, []uint32{1, 1, 2}, []uint32{mapUp(1), mapUp(1), mapUp(2)})

	m.bump()
	assert.Equal(t, uint32(2), mapUp(2), "before K, upstream 2 stays in its epoch")
	assert.Equal(t, uint32(3), take(2))
	assert.Equal(t, uint32(3), mapUp(2))
	assert.Equal(t, uint32(4), mapUp(3))

	m.bump()
	assert.Equal(t, uint32(5), take(3))
	assert.Equal(t, uint32(5), mapUp(3))
	assert.Equal(t, uint32(6), mapUp(4))

	// Two bumps before K issue one epoch.
	m.bump()
	m.bump()
	assert.Equal(t, uint32(7), take(4))
	assert.False(t, m.isPending())
	assert.Equal(t, uint32(7), mapUp(4))

	// An upstream epoch that starts exactly at K is the bump's epoch.
	m.bump()
	r, newUp := m.take(5)
	issued = append(issued, r)
	assert.True(t, newUp)
	assert.Equal(t, uint32(8), r)
	assert.Equal(t, uint32(8), mapUp(5))
	assert.Equal(t, uint32(9), mapUp(6))

	// Strictly increasing, never reused.
	for i := 1; i < len(issued); i++ {
		assert.Greater(t, issued[i], issued[i-1], "issued %v", issued)
	}

	// A stale upstream epoch has no place.
	_, _, ok := m.mapEpoch(5)
	assert.False(t, ok)
}

func TestEpochMapSkippedUpstream(t *testing.T) {
	var m epochMap
	r, _, _ := m.mapEpoch(1)
	assert.Equal(t, uint32(1), r)
	m.bump()
	r, newUp := m.take(4) // upstream jumped from 1 to 4 at K
	assert.True(t, newUp)
	assert.Equal(t, uint32(4), r)
	r, _, _ = m.mapEpoch(5)
	assert.Equal(t, uint32(5), r)
}

// routed records what a router hands out.
type routed struct {
	child    []wire.Message
	copied   []media.Frame
	anchors  []media.Anchor
	expected []media.Frame // epoch and track of each expectVideo call
	dropped  uint64
}

func newTestRouter(rt *routed, childVideo, copyVideo, childAudio bool) *router {
	src := hub.New(hub.Options{})
	src.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: time.Unix(100, 0)})
	return &router{
		childVideo: childVideo, copyVideo: copyVideo, childAudio: childAudio, copyAudio: !childAudio,
		mainVideo: -1,
		childFed:  true,
		toChild:   func(m wire.Message) bool { rt.child = append(rt.child, m); return true },
		expectVideo: func(epoch uint32, track int) {
			rt.expected = append(rt.expected, media.Frame{Epoch: epoch, Track: track})
		},
		tryChild:  func(m wire.Message) bool { rt.child = append(rt.child, m); return true },
		toCopy:    func(f media.Frame) { rt.copied = append(rt.copied, f) },
		setAnchor: func(a media.Anchor) { rt.anchors = append(rt.anchors, a) },
		srcAnchor: src.Anchor,
		now:       time.Now,
		onEpoch:   func(uint32, bool) {},
		onLoss:    func() {},
		dropped:   func(n uint64) { rt.dropped += n },
		warn:      func(string, ...any) {},
	}
}

var (
	testVideoCfg = &media.TrackConfig{Codec: media.CodecH264}
	testAudioCfg = &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 48000, Channels: 2}
)

func vframe(epoch uint32, pts int64, key bool) media.Frame {
	return media.Frame{Track: 0, Config: testVideoCfg, PTS: pts, DTS: pts, Key: key, Epoch: epoch}
}

func aframe(epoch uint32, pts int64) media.Frame { //nolint:unparam // epoch documents the call sites
	return media.Frame{Track: 1, Config: testAudioCfg, PTS: pts, DTS: pts, Key: true, Epoch: epoch}
}

// A respawn bump: K waits for Hello, encode video before K is dropped, audio
// is buffered and split at K, and the bump anchor follows the formula.
func TestRouterRespawnBump(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, true, false, true)
	for i := int64(0); i < 10; i++ {
		r.deliver(hub.Delivery{Frame: vframe(1, i*3600, i == 0)})
	}
	r.childExited()
	r.deliver(hub.Delivery{Frame: vframe(1, 36000, true)}) // no Hello yet: not K
	r.deliver(hub.Delivery{Frame: aframe(1, 37000)})
	r.childHello()
	r.deliver(hub.Delivery{Frame: aframe(1, 40000)})
	r.deliver(hub.Delivery{Frame: vframe(1, 39600, false)})
	r.deliver(hub.Delivery{Frame: aframe(1, 43500)})
	r.deliver(hub.Delivery{Frame: vframe(1, 43200, true)}) // K
	r.deliver(hub.Delivery{Frame: aframe(1, 42000)})       // below K, after it: dropped

	// 2 video before K and 2 audio below K at K, 1 audio below K after it.
	assert.EqualValues(t, 5, rt.dropped)
	require.Len(t, rt.anchors, 2)
	a := rt.anchors[1]
	assert.Equal(t, uint32(2), a.Epoch)
	assert.Equal(t, int64(43200+Delay), a.Start)
	assert.Equal(t, time.Unix(100, 0).Add(media.ToDuration(43200)).Add(media.ToDuration(Delay)), a.Wall)
	var after []media.Frame
	for _, m := range rt.child {
		if f, ok := m.(wire.Frame); ok && f.F.Epoch == 2 {
			after = append(after, f.F)
		}
	}
	require.Len(t, after, 2)
	assert.True(t, after[0].Key)
	assert.Equal(t, int64(43200+Delay), after[0].PTS)
	assert.Equal(t, int64(43500+Delay), after[1].PTS)
}

// A loss with live video: the resume keyframe is not K, the next keyframe
// is; the copy path keeps the old epoch until K.
func TestRouterLossLiveVideo(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, false, true, true)
	r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
	r.deliver(hub.Delivery{Frame: vframe(1, 3600, false)})
	r.deliver(hub.Delivery{Frame: vframe(1, 90000, true), Lost: 5})
	r.deliver(hub.Delivery{Frame: vframe(1, 93600, false)})
	r.deliver(hub.Delivery{Frame: vframe(1, 180000, true)})
	_, isDisc := rt.child[0].(wire.Discontinuity)
	assert.True(t, isDisc)
	eps := make([]uint32, 0, len(rt.copied))
	for _, f := range rt.copied {
		eps = append(eps, f.Epoch)
	}
	assert.Equal(t, []uint32{1, 1, 1, 1, 2}, eps)
}

// A loss without video: the Lost delivery itself is K.
func TestRouterLossAudioOnly(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, false, false, true)
	r.deliver(hub.Delivery{Frame: aframe(1, 0)})
	r.deliver(hub.Delivery{Frame: aframe(1, 1920)})
	r.deliver(hub.Delivery{Frame: aframe(1, 9600), Lost: 3})
	var eps []uint32
	for _, m := range rt.child {
		if f, ok := m.(wire.Frame); ok {
			eps = append(eps, f.F.Epoch)
		}
	}
	assert.Equal(t, []uint32{1, 1, 2}, eps)
}

// K must be above every PTS already sent in the old epoch; audio buffered
// for over 1 s goes out in the old epoch; the copy path drops RASL after K.
func TestRouterKAboveSentAndRASL(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, false, true, false)
	r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
	r.deliver(hub.Delivery{Frame: aframe(1, 100000)}) // sent: old epoch now reaches 100000
	r.deliver(hub.Delivery{Frame: vframe(1, 3600, false), Lost: 1})
	r.deliver(hub.Delivery{Frame: vframe(1, 90000, true)}) // below 100000: not K
	r.deliver(hub.Delivery{Frame: aframe(1, 102000)})      // buffered
	r.deliver(hub.Delivery{Frame: vframe(1, 300000, false)})
	// 102000 is now over 1 s old: out in the old epoch.
	r.deliver(hub.Delivery{Frame: vframe(1, 360000, true)}) // K
	f := vframe(1, 352800, false)
	f.RASL = true
	r.deliver(hub.Delivery{Frame: f})                        // RASL after K: dropped
	r.deliver(hub.Delivery{Frame: vframe(1, 356400, false)}) // RADL: kept
	r.deliver(hub.Delivery{Frame: vframe(1, 363600, false)})

	got := make([][2]int64, 0, len(rt.copied))
	for _, f := range rt.copied {
		got = append(got, [2]int64{f.PTS - Delay, int64(f.Epoch)})
	}
	assert.Equal(t, [][2]int64{
		{0, 1},
		{100000, 1},
		{3600, 1},
		{90000, 1},
		{300000, 1},
		{102000, 1},
		{360000, 2},
		{356400, 2},
		{363600, 2},
	}, got)
}

// A second video track is dropped; frames without a config are ignored.
func TestRouterSecondVideoTrack(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, false, true, false)
	r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
	f := vframe(1, 0, true)
	f.Track = 2
	r.deliver(hub.Delivery{Frame: f})
	r.deliver(hub.Delivery{Frame: media.Frame{Track: 3}})
	assert.Len(t, rt.copied, 1)
}

// A loss absorbed into a pending bump sends no second Discontinuity.
func TestRouterAbsorbedLossNoDiscontinuity(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, true, false, true)
	r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
	r.deliver(hub.Delivery{Frame: vframe(1, 90000, true), Lost: 1})
	r.deliver(hub.Delivery{Frame: vframe(1, 93600, true), Lost: 1})
	var discs int
	for _, m := range rt.child {
		if _, ok := m.(wire.Discontinuity); ok {
			discs++
		}
	}
	assert.Equal(t, 1, discs)
}

// Without live video, a straggler from an older upstream epoch is never K:
// it is dropped and the map does not move backwards.
func TestRouterStaleNeverK(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, false, false, true)
	r.deliver(hub.Delivery{Frame: aframe(1, 0)})
	r.deliver(hub.Delivery{Frame: aframe(2, 1920)})
	r.childExited()
	r.childHello()
	r.deliver(hub.Delivery{Frame: aframe(1, 3840)}) // stale: dropped, not K
	assert.True(t, r.em.isPending())
	assert.EqualValues(t, 1, rt.dropped)
	r.deliver(hub.Delivery{Frame: aframe(2, 5760)}) // K
	assert.False(t, r.em.isPending())
	assert.Equal(t, uint32(2), r.em.up)
	assert.Equal(t, uint32(3), r.em.cur)
}

// The router announces the main video track to the encoded renditions'
// mergers once per rendition epoch, before the first video frame of that
// epoch goes to the child, so that the child's audio for the epoch waits
// for the rung's video in the merger. Copied video needs no announcement:
// it reaches the mergers at once.
func TestRouterExpectsChildVideo(t *testing.T) {
	t.Run("encoded video", func(t *testing.T) {
		var rt routed
		r := newTestRouter(&rt, true, false, true)
		r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
		require.Equal(t, []media.Frame{{Epoch: 1, Track: 0}}, rt.expected)
		require.Len(t, rt.child, 1, "announced before the frame went to the child")
		r.deliver(hub.Delivery{Frame: aframe(1, 0)})
		r.deliver(hub.Delivery{Frame: vframe(1, 3600, false)})
		r.childExited()
		r.childHello()
		r.deliver(hub.Delivery{Frame: vframe(1, 90000, true)}) // K of epoch 2
		r.deliver(hub.Delivery{Frame: vframe(1, 93600, false)})
		assert.Equal(t, []media.Frame{{Epoch: 1, Track: 0}, {Epoch: 2, Track: 0}}, rt.expected)
	})
	t.Run("not while the child is not fed", func(t *testing.T) {
		var rt routed
		r := newTestRouter(&rt, true, false, true)
		r.childFed = false
		r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
		assert.Empty(t, rt.expected)
	})
	t.Run("copied video", func(t *testing.T) {
		var rt routed
		r := newTestRouter(&rt, false, true, true)
		r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
		assert.Empty(t, rt.expected)
	})
}

// The router announces a barrier at every main-video keyframe it routes,
// at K + D, before the keyframe goes to the child or the copy path, in
// the old epoch too while a bump waits for K. It announces the encoded
// tracks before it pushes the copy video, so a presentation merger never
// sees a new epoch's copy IDR without the encoded rungs expected.
func TestRouterBarriers(t *testing.T) {
	var rt routed
	r := newTestRouter(&rt, true, true, true)
	type call struct {
		what  string
		epoch uint32
		pts   int64
	}
	var calls []call
	r.barrier = func(e uint32, pts int64) { calls = append(calls, call{"barrier", e, pts}) }
	r.expectVideo = func(e uint32, _ int) { calls = append(calls, call{"expect", e, 0}) }
	r.toCopy = func(f media.Frame) { calls = append(calls, call{"copy", f.Epoch, f.PTS}) }
	r.toChild = func(m wire.Message) bool {
		if v, ok := m.(wire.Frame); ok {
			calls = append(calls, call{"child", v.F.Epoch, v.F.PTS})
		}
		return true
	}
	r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
	r.deliver(hub.Delivery{Frame: vframe(1, 3600, false)})
	r.childExited()
	r.deliver(hub.Delivery{Frame: vframe(1, 90000, true)}) // before Hello: old epoch, copy only
	r.childHello()
	r.deliver(hub.Delivery{Frame: vframe(1, 180000, true)}) // K of epoch 2
	assert.Equal(t, []call{
		{"barrier", 1, Delay},
		{"expect", 1, 0},
		{"copy", 1, Delay},
		{"child", 1, Delay},
		{"copy", 1, 3600 + Delay},
		{"child", 1, 3600 + Delay},
		{"barrier", 1, 90000 + Delay},
		{"copy", 1, 90000 + Delay},
		{"barrier", 2, 180000 + Delay},
		{"expect", 2, 0},
		{"copy", 2, 180000 + Delay},
		{"child", 2, 180000 + Delay},
	}, calls)
}

// shotRouter is a router for a ladder that copies video and encodes audio,
// with thumbnails every intervalSec seconds: the child is sent the due
// keyframes. ready and the two counters are the test's.
type shotRouter struct {
	*router
	rt             routed
	ready          bool
	room           bool // tryChild takes the message
	thumbs, failed uint64
	skipped        int
}

func newShotRouter(intervalSec int) *shotRouter {
	s := &shotRouter{ready: true, room: true}
	s.router = newTestRouter(&s.rt, false, true, true)
	s.tryChild = func(m wire.Message) bool {
		if s.room {
			s.rt.child = append(s.rt.child, m)
		}
		return s.room
	}
	s.shotKeys(intervalSec)
	s.childReady = func() bool { return s.ready }
	s.childShots = func() (uint64, uint64) { return s.thumbs, s.failed }
	s.shotSkipped = func() { s.skipped++ }
	return s
}

// keys returns the PTS, less Delay, of the video frames the child was sent.
func (s *shotRouter) keys() []int64 {
	var out []int64
	for _, m := range s.rt.child {
		if f, ok := m.(wire.Frame); ok && f.F.Kind() == media.KindVideo {
			out = append(out, f.F.PTS-Delay)
		}
	}
	return out
}

// key delivers a main-video keyframe at pts seconds.
func (s *shotRouter) key(sec int64) {
	s.deliver(hub.Delivery{Frame: vframe(1, sec*media.ClockRate, true)})
}

// Only due keyframes of the main video go to a child that encodes no video,
// in their rendition epoch and time; a key the child cannot take is counted
// and the next one is offered; nothing else about the routing changes.
func TestRouterShotKeys(t *testing.T) {
	s := newShotRouter(2)
	const sec = media.ClockRate
	s.key(0)
	s.deliver(hub.Delivery{Frame: vframe(1, 3600, false)})
	s.deliver(hub.Delivery{Frame: aframe(1, 1000)})
	s.key(1) // not due
	s.key(2)
	s.deliver(hub.Delivery{Frame: media.Frame{Track: 2, Config: testVideoCfg, PTS: 4 * sec, DTS: 4 * sec, Key: true, Epoch: 1}}) // a second video track
	assert.Equal(t, []int64{0, 2 * sec}, s.keys())
	f := s.rt.child[0].(wire.Frame)
	assert.Equal(t, -1, f.Rung)
	assert.Equal(t, uint32(1), f.F.Epoch)
	assert.True(t, f.F.Key)
	assert.Zero(t, s.skipped)

	// Not ready (no Hello yet): skipped, and the next key is offered.
	s.ready = false
	s.key(4)
	assert.Equal(t, 1, s.skipped)
	s.ready = true
	s.key(5)
	assert.Equal(t, []int64{0, 2 * sec, 5 * sec}, s.keys())
	// No room in the child's queue: the same.
	s.room = false
	s.key(7)
	s.key(8)
	assert.Equal(t, 3, s.skipped)
	s.room = true
	s.key(9)
	assert.Equal(t, []int64{0, 2 * sec, 5 * sec, 9 * sec}, s.keys())

	// The copy path got every main-video frame and the child every audio
	// frame, and nothing was dropped: the keys are invisible.
	assert.Len(t, s.rt.copied, 9)
	assert.Zero(t, s.rt.dropped)
	assert.Empty(t, s.rt.expected)
	assert.Equal(t, int64(9*sec+Delay), s.sentMax)

	// A child that is not fed is offered nothing (sendChild's rule).
	s.childFed = false
	s.key(12)
	s.childFed = true
	assert.Equal(t, []int64{0, 2 * sec, 5 * sec, 9 * sec}, s.keys())
	assert.Equal(t, 3, s.skipped)

	// After the child exits nothing of the old epoch is offered: not before
	// the next child's Hello, and not before K.
	s.childExited()
	s.ready = false
	s.key(20)
	s.ready = true
	s.childHello()
	s.deliver(hub.Delivery{Frame: aframe(1, 21*sec+500)})
	assert.Equal(t, []int64{0, 2 * sec, 5 * sec, 9 * sec}, s.keys())
	assert.Equal(t, 3, s.skipped, "a key before K is not offered, so not skipped either")
	s.key(21) // K, in epoch 2
	assert.Equal(t, []int64{0, 2 * sec, 5 * sec, 9 * sec, 21 * sec}, s.keys())
	last := s.rt.child[len(s.rt.child)-2].(wire.Frame)
	assert.Equal(t, media.KindVideo, last.F.Kind(), "K goes to the child ahead of the audio buffered for it")
	assert.Equal(t, uint32(2), last.F.Epoch)

	// A router without thumbnails sends the child no video.
	var rt routed
	r := newTestRouter(&rt, false, true, true)
	r.deliver(hub.Delivery{Frame: vframe(1, 0, true)})
	assert.Empty(t, rt.child)
}

// A failed shot is retried once: at the next keyframe after the failure is
// reported, and then only at the schedule's pace while failures stay
// consecutive.
func TestRouterShotRetry(t *testing.T) {
	s := newShotRouter(3600)
	const sec = media.ClockRate
	s.key(0)
	assert.Equal(t, []int64{0}, s.keys(), "key 1")
	s.failed = 1
	s.key(1)
	assert.Equal(t, []int64{0, sec}, s.keys(), "key 2: the early retry")
	s.failed = 2
	s.key(2)
	s.key(3)
	s.key(4)
	assert.Equal(t, []int64{0, sec}, s.keys(), "a second failure in a row: no key inside the interval")
	s.thumbs = 1
	s.key(5)
	assert.Equal(t, []int64{0, sec}, s.keys(), "a success alone changes nothing")
	s.failed = 3
	s.key(6)
	assert.Equal(t, []int64{0, sec, 6 * sec}, s.keys(), "a failure after a success")
	s.failed = 4
	s.key(7)
	assert.Equal(t, []int64{0, sec, 6 * sec}, s.keys())
	s.thumbs, s.failed = 2, 5
	s.key(8)
	assert.Equal(t, []int64{0, sec, 6 * sec, 8 * sec}, s.keys(), "both rose: the success counts first")
	s.failed = 6
	s.key(9)
	assert.Equal(t, []int64{0, sec, 6 * sec, 8 * sec}, s.keys())
	assert.Zero(t, s.skipped)

	// The child exits: the schedule starts over, and so does the rule, for
	// a child that counts from zero.
	s.childExited()
	s.childHello()
	s.thumbs, s.failed = 0, 0
	s.key(10) // K
	assert.Equal(t, []int64{0, sec, 6 * sec, 8 * sec, 10 * sec}, s.keys())
	s.failed = 1
	s.key(11)
	assert.Equal(t, []int64{0, sec, 6 * sec, 8 * sec, 10 * sec, 11 * sec}, s.keys(), "the early retry again")
	s.failed = 2
	s.key(12)
	assert.Equal(t, []int64{0, sec, 6 * sec, 8 * sec, 10 * sec, 11 * sec}, s.keys())
}
