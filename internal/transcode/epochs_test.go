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
	child   []wire.Message
	copied  []media.Frame
	anchors []media.Anchor
	dropped uint64
}

func newTestRouter(rt *routed, childVideo, copyVideo, childAudio bool) *router {
	src := hub.New(hub.Options{})
	src.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: time.Unix(100, 0)})
	return &router{
		childVideo: childVideo, copyVideo: copyVideo, childAudio: childAudio, copyAudio: !childAudio,
		mainVideo: -1,
		childFed:  true,
		toChild:   func(m wire.Message) { rt.child = append(rt.child, m) },
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
