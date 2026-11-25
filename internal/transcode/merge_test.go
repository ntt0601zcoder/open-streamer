package transcode

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// fakeTime is a settable clock with an After whose channels the test fires.
type fakeTime struct {
	mu    sync.Mutex
	now   time.Time
	calls []time.Duration
	chans []chan time.Time
	asked chan struct{}
}

func newFakeTime() *fakeTime {
	return &fakeTime{now: time.Unix(1000, 0), asked: make(chan struct{}, 64)}
}

func (ft *fakeTime) Now() time.Time {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.now
}

func (ft *fakeTime) advance(d time.Duration) {
	ft.mu.Lock()
	ft.now = ft.now.Add(d)
	ft.mu.Unlock()
}

func (ft *fakeTime) After(d time.Duration) <-chan time.Time {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	c := make(chan time.Time, 1)
	ft.calls = append(ft.calls, d)
	ft.chans = append(ft.chans, c)
	select {
	case ft.asked <- struct{}{}:
	default:
	}
	return c
}

// fireLast fires the latest timer.
func (ft *fakeTime) fireLast() {
	ft.mu.Lock()
	c := ft.chans[len(ft.chans)-1]
	now := ft.now
	ft.mu.Unlock()
	c <- now
}

func newTestMerger(ft *fakeTime) (*merger, *hub.Sub) {
	h := hub.New(hub.Options{})
	sub := h.Subscribe("test", 1<<14)
	m := newMerger(h, 2*time.Second, ft.Now, func(uint32) (media.Anchor, bool) { return media.Anchor{}, false })
	return m, sub
}

func drain(sub *hub.Sub) []media.Frame {
	var out []media.Frame
	for {
		select {
		case d := <-sub.C():
			out = append(out, d.Frame)
		default:
			return out
		}
	}
}

func TestMergerOrdersByDTS(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestMerger(ft)
	rng := rand.New(rand.NewPCG(1, 2))
	var video, audio []media.Frame
	for i := int64(0); i < 250; i++ {
		video = append(video, vframe(1, i*3600, i%25 == 0))
	}
	for i := int64(0); i < 470; i++ {
		audio = append(audio, aframe(1, i*1920))
	}
	// Each track in order; across tracks, arrival leads or lags by up to 500 ms.
	n := len(video) + len(audio)
	m.push(video[0])
	m.push(audio[0])
	vi, ai := 1, 1
	for vi < len(video) || ai < len(audio) {
		switch {
		case ai == len(audio):
			m.push(video[vi])
			vi++
		case vi == len(video):
			m.push(audio[ai])
			ai++
		case audio[ai].DTS-video[vi].DTS < int64(rng.IntN(45000))-22500:
			m.push(audio[ai])
			ai++
		default:
			m.push(video[vi])
			vi++
		}
	}
	m.flush()
	out := drain(sub)
	require.Len(t, out, n)
	for i := 1; i < len(out); i++ {
		assert.GreaterOrEqual(t, out[i].DTS, out[i-1].DTS, "frame %d", i)
	}
	o, dropped := m.counts()
	assert.EqualValues(t, n, o)
	assert.Zero(t, dropped)
}

func TestMergerHoldBound(t *testing.T) {
	t.Run("stream time", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		m.push(vframe(1, 0, true)) // video is expected, then silent
		var released []media.Frame
		for i := int64(0); i < 200; i++ {
			pts := i * 1920
			m.push(aframe(1, pts))
			released = append(released, drain(sub)...)
			// Every audio frame 2 s or more behind the newest one is out.
			var outAudio int
			for _, f := range released {
				if f.Kind() == media.KindAudio {
					outAudio++
				}
			}
			mustOut := 0
			for j := int64(0); j <= i; j++ {
				if pts-j*1920 >= 180000 {
					mustOut++
				}
			}
			require.GreaterOrEqual(t, outAudio, mustOut, "audio %d", i)
		}
		assert.Len(t, released, 201)
	})
	t.Run("wall time", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.run(ctx, ft.After)
		}()
		defer func() {
			cancel()
			<-done
		}()
		m.push(vframe(1, 0, true))
		require.Len(t, drain(sub), 1)
		m.push(aframe(1, 0))
		<-ft.asked
		ft.mu.Lock()
		assert.Equal(t, 2*time.Second, ft.calls[len(ft.calls)-1])
		ft.mu.Unlock()
		assert.Empty(t, drain(sub), "held")
		ft.advance(1900 * time.Millisecond)
		ft.fireLast()
		<-ft.asked // re-armed for the remaining 100 ms
		assert.Empty(t, drain(sub), "still held")
		ft.advance(100 * time.Millisecond)
		ft.fireLast()
		require.Eventually(t, func() bool { return len(drain(sub)) == 1 }, 2*time.Second, time.Millisecond)
		// The silent track has left the expected set: no more holds.
		m.push(aframe(1, 1920))
		assert.Len(t, drain(sub), 1)
	})
}

// Old-epoch frames after the hub moved on are dropped and counted; a new
// epoch's anchor is set before its first frame.
func TestMergerEpochs(t *testing.T) {
	ft := newFakeTime()
	h := hub.New(hub.Options{})
	sub := h.Subscribe("test", 64)
	anchors := map[uint32]media.Anchor{2: {Epoch: 2, Start: 90000, Wall: time.Unix(5, 0)}}
	m := newMerger(h, 2*time.Second, ft.Now, func(e uint32) (media.Anchor, bool) {
		a, ok := anchors[e]
		return a, ok
	})
	m.push(vframe(1, 0, true))
	m.push(vframe(2, 90000, true))
	m.push(vframe(1, 3600, false)) // older epoch: dropped
	same := vframe(2, 90000, false)
	m.push(same) // DTS not above the last: dropped
	m.flush()
	assert.Len(t, drain(sub), 2)
	_, dropped := m.counts()
	assert.EqualValues(t, 2, dropped)
	a, ok := h.Anchor(2)
	require.True(t, ok)
	assert.Equal(t, int64(90000), a.Start)
}

// A burst of new-epoch copy frames does not count as stream time elapsed
// for the old epoch's hold: the child's late old-epoch audio, arriving
// after 3 s of new-epoch copy video but within the wall-time hold, is still
// written (regression: the merger released the old epoch at once and
// dropped that audio). The wall-time bound still applies across epochs.
func TestMergerHoldAcrossEpochs(t *testing.T) {
	const v, a = 3600, 1920 // 25 fps video, 48 kHz AAC
	// feed pushes epoch 1 (1 s of video and audio, then 1 s more of video
	// with the audio lagging) and a 3 s burst of epoch 2 copy video. It
	// returns the late epoch 1 audio, not yet pushed.
	feed := func(m *merger) []media.Frame {
		var late []media.Frame
		m.push(aframe(1, 0)) // audio is expected from the start
		for i := int64(0); i < 2*90000/v; i++ {
			m.push(vframe(1, i*v, i%25 == 0))
		}
		for i := int64(1); i*a < 2*90000; i++ {
			if i*a < 90000 {
				m.push(aframe(1, i*a))
			} else {
				late = append(late, aframe(1, i*a))
			}
		}
		for i := int64(0); i < 3*90000/v; i++ {
			m.push(vframe(2, 2*90000+i*v, i%25 == 0))
		}
		return late
	}
	countAudio := func(fs []media.Frame, e uint32) (n int) {
		for _, f := range fs {
			if f.Kind() == media.KindAudio && f.Epoch == e {
				n++
			}
		}
		return n
	}

	t.Run("within the wall-time hold", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		late := feed(m)
		before := drain(sub)
		for _, f := range before {
			assert.EqualValues(t, 1, f.Epoch, "nothing of epoch 2 before epoch 1's audio")
		}
		ft.advance(1900 * time.Millisecond)
		for _, f := range late {
			m.push(f)
		}
		for i := int64(0); i*a < 3*90000; i++ {
			m.push(aframe(2, 2*90000+i*a))
		}
		m.flush()
		out := append(before, drain(sub)...)
		_, dropped := m.counts()
		assert.Zero(t, dropped)
		assert.Equal(t, int(2*90000+a-1)/a, countAudio(out, 1), "every epoch 1 audio frame")
		// Epochs never go back, and each track is in DTS order. (Within
		// epoch 2 the 3 s of video still runs out the stream-time hold
		// before epoch 2's audio comes: that is the same-epoch rule.)
		last := map[int]int64{}
		for i := 1; i < len(out); i++ {
			p, c := out[i-1], out[i]
			assert.GreaterOrEqual(t, c.Epoch, p.Epoch, "frame %d", i)
			if d, ok := last[c.Track]; ok {
				assert.Greater(t, c.DTS, d, "frame %d", i)
			}
			last[c.Track] = c.DTS
		}
	})
	t.Run("past the wall-time hold", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		late := feed(m)
		ft.advance(2 * time.Second)
		m.push(vframe(2, 5*90000, false)) // releases what waited out its hold
		out := drain(sub)
		assert.NotZero(t, len(out))
		for _, f := range late {
			m.push(f)
		}
		_, dropped := m.counts()
		assert.EqualValues(t, len(late), dropped, "audio later than the wall-time hold is dropped")
	})
}

// A track announced with expect for an epoch is waited for before any frame
// of that epoch is written, as if it had been seen: the child's audio that
// reaches the merger before the rung's first IDR (the encoder's delay) is
// not written ahead of it. Without expect, that audio went out first, and a
// subscriber that joined on the IDR (the GOP cache) never got it.
func TestMergerExpect(t *testing.T) {
	t.Run("audio waits for the expected video", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		m.expect(1, 0)
		for i := int64(0); i < 25; i++ { // 0.5 s of audio, video not yet out
			m.push(aframe(1, i*1920))
		}
		assert.Empty(t, drain(sub), "audio held for the expected video")
		for i := int64(0); i < 25; i++ {
			m.push(vframe(1, i*3600, i == 0))
		}
		m.flush()
		out := drain(sub)
		require.Len(t, out, 50)
		assert.Equal(t, media.KindVideo, out[0].Kind(), "the IDR comes first")
		assert.True(t, out[0].Key)
		for i := 1; i < len(out); i++ {
			assert.GreaterOrEqual(t, out[i].DTS, out[i-1].DTS, "frame %d", i)
		}
	})
	t.Run("the hold still bounds it", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		m.expect(1, 0)
		m.push(aframe(1, 0))
		assert.Empty(t, drain(sub))
		ft.advance(2 * time.Second)
		m.push(aframe(1, 1920))
		assert.Len(t, drain(sub), 2, "the expected track ran out its hold")
	})
	t.Run("an older epoch is ignored", func(t *testing.T) {
		ft := newFakeTime()
		m, sub := newTestMerger(ft)
		m.push(vframe(2, 0, true))
		m.expect(1, 1)
		m.push(vframe(2, 3600, false))
		assert.Len(t, drain(sub), 2)
	})
}

// Barrier tests: 25 fps video in closed GOPs of 10 frames from tick 90000,
// on three presentation video tracks (0, 1, 2), and 48 kHz AAC on track 3.
const (
	bfd    = 3600
	bgop   = 10
	bbase  = 90000
	baudio = 3
)

// gopKey returns the PTS of GOP g's keyframe: barrier g + 1.
func gopKey(g int) int64 { return bbase + int64(g*bgop)*bfd }

// bOrder is the display index of each decode position of a GOP with two
// B-frames.
var bOrder = []int{0, 3, 1, 2, 6, 4, 5, 9, 7, 8}

// trackFrame returns track's frame at decode position pos of GOP g. With
// bframes the GOP is reordered with two B-frames and its DTS runs two frames
// below its PTS; without, DTS = PTS.
func trackFrame(track, g, pos int, bframes bool) media.Frame {
	disp, dts := int64(pos), int64(pos)
	if bframes {
		disp, dts = int64(bOrder[pos]), int64(pos-2)
	}
	start := gopKey(g)
	return media.Frame{
		Track: track, Config: testVideoCfg, Epoch: 1,
		PTS: start + disp*bfd, DTS: start + dts*bfd, Key: pos == 0,
	}
}

// barrierFeed drives a presentation merger the way the transcoder does: at
// each decode slot, the barrier of a GOP's keyframe first (the router
// announces it before the source keyframe goes anywhere), then each video
// track's frame that arrives in that slot, then the slot's audio.
type barrierFeed struct {
	gops    int
	bframes [3]bool
	// arrive returns the slot in which track's frame of decode slot d
	// arrives; nil means d. It must not decrease in d.
	arrive func(track, d int) int
	// noIDR lists {track, gop}: that track's frame at the GOP's keyframe
	// PTS is not a keyframe.
	noIDR map[[2]int]bool
}

// event is one call a barrierFeed makes: a barrier (f unset) or a push.
type event struct {
	barrier bool
	pts     int64
	f       media.Frame
}

// events returns the feed's calls in order.
func (bf barrierFeed) events() []event {
	n := bf.gops * bgop
	arrive := bf.arrive
	if arrive == nil {
		arrive = func(_, d int) int { return d }
	}
	bySlot := map[int][]media.Frame{}
	last := 0
	for track := range 3 {
		for d := range n {
			f := trackFrame(track, d/bgop, d%bgop, bf.bframes[track])
			if d%bgop == 0 && bf.noIDR[[2]int{track, d / bgop}] {
				f.Key = false
			}
			s := arrive(track, d)
			bySlot[s] = append(bySlot[s], f)
			last = max(last, s)
		}
	}
	var out []event
	apts := int64(bbase)
	for s := 0; s <= last; s++ {
		if s < n && s%bgop == 0 {
			out = append(out, event{barrier: true, pts: gopKey(s / bgop)})
		}
		fs := bySlot[s]
		slices.SortStableFunc(fs, func(a, b media.Frame) int { return a.Track - b.Track })
		for _, f := range fs {
			out = append(out, event{f: f})
		}
		for s < n && apts < bbase+int64(s+1)*bfd {
			out = append(out, event{f: media.Frame{
				Track: baudio, Config: testAudioCfg, PTS: apts, DTS: apts, Key: true, Epoch: 1,
			}})
			apts += 1920
		}
	}
	return out
}

// run announces the video tracks for epoch 1, as the router does for
// encoded rungs, then plays the feed into m and returns the frames pushed.
func (bf barrierFeed) run(m *merger) int {
	for track := range 3 {
		m.expect(1, track)
	}
	n := 0
	for _, ev := range bf.events() {
		if ev.barrier {
			m.barrier(1, ev.pts)
			continue
		}
		m.push(ev.f)
		n++
	}
	return n
}

func newTestPresMerger(ft *fakeTime) (*merger, *hub.Sub) {
	h := hub.New(hub.Options{})
	sub := h.Subscribe("test", 1<<16)
	m := newPresentationMerger(h, 2*time.Second, ft.Now, func(uint32) (media.Anchor, bool) { return media.Anchor{}, false })
	return m, sub
}

// keyIndex returns the index in out of track's keyframe at pts, or -1.
func keyIndex(out []media.Frame, track int, pts int64) int {
	return slices.IndexFunc(out, func(f media.Frame) bool {
		return f.Track == track && f.Kind() == media.KindVideo && f.Key && f.PTS == pts
	})
}

// assertBarrierOrder checks GOP g of out: every frame with PTS below its
// keyframe PTS K comes before track 0's IDR at K, the IDRs of tracks follow
// it at once, in track order, and every later frame has PTS K or above.
func assertBarrierOrder(t *testing.T, out []media.Frame, g int, tracks []int) {
	t.Helper()
	k := gopKey(g)
	i0 := keyIndex(out, tracks[0], k)
	require.GreaterOrEqual(t, i0, 0, "gop %d: IDR of track %d", g, tracks[0])
	for j := range i0 {
		require.Less(t, out[j].PTS, k, "gop %d: frame %d (track %d) before the first IDR", g, j, out[j].Track)
	}
	for n, track := range tracks {
		f := out[i0+n]
		require.True(t, f.Track == track && f.Key && f.PTS == k, "gop %d: frame %d is track %d's IDR, got %+v", g, i0+n, track, f)
	}
	for j := i0 + len(tracks); j < len(out); j++ {
		require.GreaterOrEqual(t, out[j].PTS, k, "gop %d: frame %d (track %d) after the IDRs", g, j, out[j].Track)
	}
}

// assertTrackDTS checks that each track is in DTS order.
func assertTrackDTS(t *testing.T, out []media.Frame) {
	t.Helper()
	last := map[int]int64{}
	for i, f := range out {
		if d, ok := last[f.Track]; ok {
			require.Greater(t, f.DTS, d, "frame %d track %d", i, f.Track)
		}
		last[f.Track] = f.DTS
	}
}

// A track whose decode order lags the others by two frames (more
// B-frames) and that arrives two slots late: every frame before K, of
// every track, comes before IDR_0(K), then IDR_0, IDR_1, IDR_2.
func TestMergerBarrierLaggingTrack(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	bf := barrierFeed{
		gops:    5,
		bframes: [3]bool{false, false, true},
		arrive: func(track, d int) int {
			if track == 2 {
				return d + 2
			}
			return d
		},
	}
	n := bf.run(m)
	m.flush()
	out := drain(sub)
	require.Len(t, out, n)
	for g := range bf.gops {
		assertBarrierOrder(t, out, g, []int{0, 1, 2})
	}
	assertTrackDTS(t, out)
	o, dropped := m.counts()
	assert.EqualValues(t, n, o)
	assert.Zero(t, dropped)
}

// A track whose decode order leads the others by two frames (the others
// have B-frames, and arrive two slots late): same order.
func TestMergerBarrierLeadingTrack(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	bf := barrierFeed{
		gops:    5,
		bframes: [3]bool{true, true, false},
		arrive: func(track, d int) int {
			if track != 2 {
				return d + 2
			}
			return d
		},
	}
	n := bf.run(m)
	m.flush()
	out := drain(sub)
	require.Len(t, out, n)
	for g := range bf.gops {
		assertBarrierOrder(t, out, g, []int{0, 1, 2})
	}
	assertTrackDTS(t, out)
}

// Track 1 has no IDR at K: tracks 0 and 2 are not held for it, only until
// each track has a frame pending, and nothing waits for MergeHold (the
// clock never moves). Track 1's frame at K passes the barrier as a
// non-keyframe, after the other IDRs.
func TestMergerBarrierMissingIDR(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	bf := barrierFeed{gops: 3, noIDR: map[[2]int]bool{{1, 1}: true}}
	for track := range 3 {
		m.expect(1, track)
	}
	k := gopKey(1)
	var out []media.Frame
	for _, ev := range bf.events() {
		if ev.barrier {
			m.barrier(1, ev.pts)
			continue
		}
		m.push(ev.f)
		out = append(out, drain(sub)...)
		if ev.f.Track == baudio && ev.f.PTS >= k+bfd {
			// Every track has pushed its frame after K.
			assert.GreaterOrEqual(t, keyIndex(out, 0, k), 0, "IDR_0 released without a hold")
			assert.GreaterOrEqual(t, keyIndex(out, 2, k), 0, "IDR_2 released without a hold")
		}
	}
	m.flush()
	out = append(out, drain(sub)...)
	assertBarrierOrder(t, out, 0, []int{0, 1, 2})
	assertBarrierOrder(t, out, 1, []int{0, 2})
	assertBarrierOrder(t, out, 2, []int{0, 1, 2})
	i := slices.IndexFunc(out, func(f media.Frame) bool { return f.Track == 1 && f.PTS == k })
	assert.Greater(t, i, keyIndex(out, 2, k), "track 1's frame at K after the IDRs")
	assert.False(t, out[i].Key)
}

// presKey is a frame's order key in the presentation merger.
type presKey struct {
	b     int64
	phase uint8
	dts   int64
}

func (k presKey) less(o presKey) bool {
	switch {
	case k.b != o.b:
		return k.b < o.b
	case k.phase != o.phase:
		return k.phase < o.phase
	default:
		return k.dts < o.dts
	}
}

// 50 barriers, pruned to the last 8: per track, audio and video keys never
// decrease. Track 1, without an IDR at barrier 3, is ordered with the
// others again from barrier 4. Track 0 stalls for ten GOPs, so its IDRs
// find their barriers pruned: it catches up through the non-keyframe rule
// and is ordered with the others again once its barriers are kept.
func TestMergerBarrierPruneKeepsOrder(t *testing.T) {
	const capacity = 8
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	m.pres.cap = capacity
	bf := barrierFeed{
		gops:  50,
		noIDR: map[[2]int]bool{{1, 2}: true},
		arrive: func(track, d int) int {
			switch {
			case track == 0 && d >= 200 && d < 300:
				return 302 // stalled, then a burst
			case track == 2:
				return d
			}
			return d + 2
		},
	}
	// Replay the feed's barriers and frames into a lone order to see each
	// frame's key.
	p := newPresOrder(capacity)
	last := map[int]presKey{}
	type at struct {
		track int
		pts   int64
	}
	keys := map[at]presKey{}
	for _, ev := range bf.events() {
		if ev.barrier {
			p.addBarrier(1, ev.pts)
			continue
		}
		b, phase := p.key(ev.f)
		k := presKey{b: b, phase: phase, dts: ev.f.DTS}
		if l, ok := last[ev.f.Track]; ok {
			require.False(t, k.less(l), "track %d: key %+v after %+v", ev.f.Track, k, l)
		}
		last[ev.f.Track] = k
		if ev.f.Track != baudio {
			keys[at{ev.f.Track, ev.f.PTS}] = k
		}
	}
	bp := func(track, g int) (int64, uint8) {
		k := keys[at{track, gopKey(g)}]
		return k.b, k.phase
	}
	assertKey := func(track, g int, b int64, phase uint8) {
		t.Helper()
		gb, gp := bp(track, g)
		assert.Equal(t, [2]int64{b, int64(phase)}, [2]int64{gb, int64(gp)}, "track %d gop %d", track, g)
	}
	assertKey(1, 2, 3, 1) // no IDR: passes barrier 3 as a non-keyframe
	assertKey(1, 3, 4, 0)
	assertKey(0, 20, 20, 1) // barrier 21 pruned before track 0's IDR came
	assertKey(0, 22, 23, 1) // the base (barrier 23): caught up
	assertKey(0, 23, 24, 0) // kept: ordered with the others again
	assertKey(2, 20, 21, 0)
	n := bf.run(m)
	m.flush()
	out := drain(sub)
	require.Len(t, out, n)
	_, dropped := m.counts()
	assert.Zero(t, dropped)
	assertTrackDTS(t, out)

	eb := m.pres.barriers[1]
	require.NotNil(t, eb)
	assert.Len(t, eb.list, capacity)
	assert.EqualValues(t, 50-capacity, eb.base.seq)

	idrRun := func(g int, tracks []int) {
		t.Helper()
		k := gopKey(g)
		i0 := keyIndex(out, tracks[0], k)
		require.GreaterOrEqual(t, i0, 0, "gop %d", g)
		for n, track := range tracks {
			f := out[i0+n]
			require.True(t, f.Track == track && f.Key && f.PTS == k, "gop %d: frame %d is track %d's IDR, got %+v", g, i0+n, track, f)
		}
	}
	idrRun(2, []int{0, 2})
	for g := 3; g < bf.gops; g++ {
		if g >= 20 && g <= 30 {
			continue // track 0 stalled, then caught up
		}
		idrRun(g, []int{0, 1, 2})
	}
}

// Track 1 is announced and silent: before the first write only the
// wall-time hold applies, so 3 s of the other tracks do not release the
// epoch; after presHoldFactor × MergeHold (fake clock) the others go out,
// IDR_0 first.
func TestMergerBarrierHoldRunsOut(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.run(ctx, ft.After)
	}()
	defer func() {
		cancel()
		<-done
	}()
	for track := range 3 {
		m.expect(1, track)
	}
	pushed := 0
	for d := range 75 { // 3 s
		if d%bgop == 0 {
			m.barrier(1, gopKey(d/bgop))
		}
		for _, track := range []int{0, 2} {
			m.push(trackFrame(track, d/bgop, d%bgop, false))
			pushed++
		}
	}
	<-ft.asked
	assert.Empty(t, drain(sub), "held for track 1")
	ft.advance(presHoldFactor * 2 * time.Second)
	ft.fireLast()
	var out []media.Frame
	require.Eventually(t, func() bool {
		out = append(out, drain(sub)...)
		return len(out) >= pushed-1 // each track's tail may wait for the other
	}, 2*time.Second, time.Millisecond)
	assert.True(t, out[0].Track == 0 && out[0].Key, "IDR_0 first")
	assert.True(t, out[1].Track == 2 && out[1].Key, "then IDR_2")
	// Track 1 has left the expected set: no more holds.
	m.push(trackFrame(0, 7, 5, false))
	m.push(trackFrame(2, 7, 5, false))
	out = append(out, drain(sub)...)
	assert.GreaterOrEqual(t, len(out), pushed+1)
}

// The key rules of presOrder, one frame at a time.
func TestPresOrderKeyRules(t *testing.T) {
	video := func(pts int64, key bool) media.Frame {
		return media.Frame{Track: 0, Config: testVideoCfg, PTS: pts, DTS: pts, Key: key, Epoch: 1}
	}
	audio := func(pts int64) media.Frame {
		return media.Frame{Track: baudio, Config: testAudioCfg, PTS: pts, DTS: pts, Key: true, Epoch: 1}
	}
	p := newPresOrder(maxBarriers)
	check := func(f media.Frame, b int64, phase uint8) {
		t.Helper()
		gb, gp := p.key(f)
		assert.Equal(t, [2]int64{b, int64(phase)}, [2]int64{gb, int64(gp)}, "PTS %d key %v kind %d", f.PTS, f.Key, f.Kind())
	}
	check(video(0, true), 0, 1) // no barrier yet
	check(audio(0), 0, 1)
	p.addBarrier(1, 100)
	p.addBarrier(1, 200)
	p.addBarrier(1, 300)
	check(audio(150), 1, 1)        // the largest barrier at or below its PTS
	check(audio(120), 1, 1)        // never below the track's last
	check(video(90, false), 0, 1)  // below every barrier
	check(video(250, true), 2, 1)  // keyframe off the barriers: the highest below
	check(video(300, true), 3, 0)  // equal: passes it, phase 0
	check(video(310, false), 3, 1) // nothing left to pass
	p.addBarrier(1, 400)
	check(video(400, false), 4, 1) // no IDR at the barrier: passes it as a non-keyframe
	check(video(400+1, true), 4, 1)
	p.addBarrier(2, 500)
	check(media.Frame{Track: 0, Config: testVideoCfg, PTS: 500, DTS: 500, Key: true, Epoch: 2}, 1, 0) // a new epoch counts from 1
	p.dropBefore(2)
	assert.NotContains(t, p.barriers, uint32(1))
	assert.NotContains(t, p.tracks, baudio)
}

// lagFeed drives a presentation merger like a paced ladder with a copy
// rung: video tracks 0..2 are encoded and come out delay frames behind the
// copy rung, track 3; the fake clock advances one frame duration per slot.
// GOPs are closed, gop frames long, from tick 90000, without B-frames.
type lagFeed struct {
	ft       *fakeTime
	m        *merger
	fd       int64 // frame duration in ticks
	gop      int
	delay    int // frames the encoded rungs trail the copy rung
	epoch    uint32
	from     int  // the epoch's first slot
	slot     int  // next slot
	silent   bool // the encoded rungs deliver nothing
	announce bool // the encoded rungs were announced for the epoch
}

const lagCopy = 3

func (lf *lagFeed) key(g int) int64 { return bbase + int64(g*lf.gop)*lf.fd }

func (lf *lagFeed) frame(track, n int) media.Frame {
	pts := bbase + int64(n)*lf.fd
	return media.Frame{Track: track, Config: testVideoCfg, Epoch: lf.epoch, PTS: pts, DTS: pts, Key: n%lf.gop == 0}
}

// step plays n slots: the barrier of a keyframe, the copy rung's frame,
// then the encoded rungs' frames of delay slots ago.
func (lf *lagFeed) step(n int) {
	if !lf.announce {
		lf.announce = true
		for track := range 3 {
			lf.m.expect(lf.epoch, track)
		}
	}
	for range n {
		s := lf.slot
		lf.slot++
		if s%lf.gop == 0 {
			lf.m.barrier(lf.epoch, lf.key(s/lf.gop))
		}
		lf.m.push(lf.frame(lagCopy, s))
		if d := s - lf.delay; d >= lf.from && !lf.silent {
			for track := range 3 {
				lf.m.push(lf.frame(track, d))
			}
		}
		lf.ft.advance(media.ToDuration(lf.fd))
	}
}

// assertLadderOrder checks the barrier order of every GOP whose keyframes
// all came out.
func assertLadderOrder(t *testing.T, out []media.Frame, lf *lagFeed, gops int) {
	t.Helper()
	tracks := []int{0, 1, 2, lagCopy}
	for g := range gops {
		k := lf.key(g)
		i0 := keyIndex(out, 0, k)
		require.GreaterOrEqual(t, i0, 0, "gop %d: IDR of track 0", g)
		for j := range i0 {
			require.Less(t, out[j].PTS, k, "gop %d: frame %d (track %d) before the first IDR", g, j, out[j].Track)
		}
		for n, track := range tracks {
			f := out[i0+n]
			require.True(t, f.Track == track && f.Key && f.PTS == k, "gop %d: frame %d is track %d's IDR, got %+v", g, i0+n, track, f)
		}
	}
}

// Finding C1: x264 trails the copy rung by about 30 frames, which is more
// stream time than MergeHold from 15 fps down. The presentation merger's
// hold follows the lag, so every barrier still holds: 15, 12, 10 and 5 fps.
func TestMergerPresHoldFollowsLag(t *testing.T) {
	for _, fps := range []int{15, 12, 10, 5} {
		ft := newFakeTime()
		m, sub := newTestPresMerger(ft)
		lf := &lagFeed{ft: ft, m: m, fd: int64(media.ClockRate / fps), gop: 2 * fps, delay: 30, epoch: 1}
		const gops = 8
		lf.step(gops*lf.gop + lf.delay + 1)
		out := drain(sub)
		assertLadderOrder(t, out, lf, gops)
		assertTrackDTS(t, out)
		_, dropped := m.counts()
		assert.Zero(t, dropped, "%d fps", fps)
		m.mu.Lock()
		hold, ticks := m.holds(false)
		m.mu.Unlock()
		lag := int64(lf.delay) * lf.fd
		assert.Equal(t, min(lag+lag/2, 8*media.ClockRate), ticks, "%d fps: hold in ticks", fps)
		assert.Equal(t, media.ToDuration(ticks), hold, "%d fps", fps)
	}
}

// At 25 fps the encoder's lag (30 frames, 1.2 s) stays below MergeHold: the
// hold is MergeHold, as before, and a rung that falls silent is given up
// once 2 s of the copy rung wait, not later.
func TestMergerPresHoldUnchangedAt25(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	lf := &lagFeed{ft: ft, m: m, fd: 3600, gop: 50, delay: 30, epoch: 1}
	lf.step(4*lf.gop + lf.delay + 1)
	out := drain(sub)
	assertLadderOrder(t, out, lf, 4)
	m.mu.Lock()
	hold, ticks := m.holds(false)
	m.mu.Unlock()
	assert.Equal(t, 2*time.Second, hold)
	assert.Equal(t, int64(2*media.ClockRate), ticks)

	// The encoded rungs stop. The copy rung's head waits; it has 30 frames
	// pending already, and goes out when the pending span reaches 2 s.
	lf.silent = true
	lf.step(19)
	assert.Empty(t, drain(sub), "1.96 s pending: held")
	lf.step(2)
	got := drain(sub)
	require.NotEmpty(t, got, "2 s pending: released")
	assert.True(t, slices.ContainsFunc(got, func(f media.Frame) bool { return f.Track == lagCopy }), "the copy rung goes on")
}

// An encoder that trails by more than the cap (here 10 s, with MergeHold
// 2 s and so a cap of 8 s) is not waited for: the copy rung goes out after
// 8 s, at the start of the epoch and within it, and the hold stays at the
// cap.
func TestMergerPresHoldCap(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	lf := &lagFeed{ft: ft, m: m, fd: 9000, gop: 20, delay: 100, epoch: 1} // 10 fps, 10 s behind
	lf.step(79)
	assert.Empty(t, drain(sub), "7.9 s: the epoch's first frame still waits for rung 0")
	lf.step(2)
	out := drain(sub)
	require.NotEmpty(t, out, "8 s: released")
	assert.True(t, out[0].Track == lagCopy && out[0].Key, "the copy rung opens the epoch")
	lf.step(100)
	m.mu.Lock()
	hold, ticks := m.holds(false)
	m.mu.Unlock()
	assert.Equal(t, 8*time.Second, hold)
	assert.Equal(t, int64(8*media.ClockRate), ticks)
	// Everything pushed comes out at flush, each track in DTS order.
	m.flush()
	assertTrackDTS(t, append(out, drain(sub)...))
	_, dropped := m.counts()
	assert.Zero(t, dropped)
}

// A respawn at 10 fps: the new epoch's copy IDR waits for rung 0's, which
// comes 3 s later (more than MergeHold, less than the cap), so every
// variant starts the epoch on its IDR, rung 0's first.
func TestMergerPresHoldRespawn(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	lf := &lagFeed{ft: ft, m: m, fd: 9000, gop: 20, delay: 30, epoch: 1}
	lf.step(3*lf.gop + lf.delay + 1)
	drain(sub)

	// The child dies: epoch 1's copy rung goes on alone, then epoch 2
	// starts at a keyframe once the new child is fed.
	lf.silent = true
	lf.step(9)
	lf2 := &lagFeed{ft: ft, m: m, fd: 9000, gop: 20, delay: 30, epoch: 2, from: 100, slot: 100}
	lf2.step(29)
	for _, f := range drain(sub) {
		require.Equal(t, uint32(1), f.Epoch, "epoch 2 waits for its rungs: %+v", f)
	}
	lf2.step(lf2.gop + 2)
	var e2 []media.Frame
	for _, f := range drain(sub) {
		if f.Epoch == 2 {
			e2 = append(e2, f)
		}
	}
	require.GreaterOrEqual(t, len(e2), 4)
	for n, track := range []int{0, 1, 2, lagCopy} {
		assert.True(t, e2[n].Track == track && e2[n].Key && e2[n].PTS == lf2.key(5), "epoch 2 frame %d: %+v", n, e2[n])
	}
}

// A rendition merger's hold is fixed.
func TestMergerRenditionHoldFixed(t *testing.T) {
	ft := newFakeTime()
	m, _ := newTestMerger(ft)
	for i := range 100 {
		m.push(vframe(1, int64(i)*9000, i%20 == 0))
		m.push(aframe(2, int64(i)*9000+900000))
	}
	for _, newEpoch := range []bool{false, true} {
		hold, ticks := m.holds(newEpoch)
		assert.Equal(t, 2*time.Second, hold)
		assert.Equal(t, int64(2*media.ClockRate), ticks)
	}
	assert.Nil(t, m.lag)
}

// lagFrame returns track's video frame of epoch at dts, with PTS = DTS.
func lagFrame(track int, epoch uint32, dts int64, key bool) media.Frame {
	return media.Frame{Track: track, Config: testVideoCfg, Epoch: epoch, PTS: dts, DTS: dts, Key: key}
}

// learnLag plays epoch 1 into m at 10 fps: track 1 trails track 0 by 25
// frames (225000 ticks, 2.5 s) for 20 frames; then it flushes. It returns
// the DTS after track 0's last frame.
func learnLag(m *merger) int64 {
	const fd = 9000
	for i := range 45 {
		m.push(lagFrame(0, 1, int64(i)*fd, i%20 == 0))
		if d := i - 25; d >= 0 {
			m.push(lagFrame(1, 1, int64(d)*fd, d%20 == 0))
		}
	}
	m.flush()
	return 45 * fd
}

// startRun runs m with ft's timers until the test ends.
func startRun(t *testing.T, m *merger, ft *fakeTime) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.run(ctx, ft.After)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// lastTimer returns the duration of the latest timer run armed.
func lastTimer(ft *fakeTime) time.Duration {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.calls[len(ft.calls)-1]
}

// waitsFor checks that the head frame, pushed just now with run started,
// is not written after before (the timer fires early) and is written at
// hold.
func waitsFor(t *testing.T, ft *fakeTime, sub *hub.Sub, before, hold time.Duration) {
	t.Helper()
	<-ft.asked
	assert.Equal(t, hold, lastTimer(ft), "the armed hold")
	ft.advance(before)
	ft.fireLast()
	<-ft.asked
	assert.Empty(t, drain(sub), "held at %v", before)
	ft.advance(hold - before)
	ft.fireLast()
	require.Eventually(t, func() bool { return len(drain(sub)) == 1 }, 2*time.Second, time.Millisecond, "written at %v", hold)
}

// A new epoch's opener waits 1.5 × the lag learned in the previous epoch
// for the tracks announced for it, not presHoldFactor × MergeHold.
func TestMergerNewEpochUsesLearnedHold(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	next := learnLag(m)
	drain(sub)
	m.expect(2, 0)
	m.expect(2, 1)
	m.push(lagFrame(0, 2, next, true))
	assert.Empty(t, drain(sub), "the opener waits for track 1")
	startRun(t, m, ft)
	waitsFor(t, ft, sub, 3700*time.Millisecond, 3750*time.Millisecond)
}

// The first epoch's opener has no lag to learn from: it waits
// presHoldFactor × MergeHold, although its own push has put its track in
// the lag map.
func TestMergerNewEpochBeforeAnyLag(t *testing.T) {
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	m.expect(1, 0)
	m.expect(1, 1)
	m.push(lagFrame(0, 1, 0, true))
	m.mu.Lock()
	e, ok := m.lag[0]
	m.mu.Unlock()
	require.True(t, ok, "the opener's track has a lag entry")
	assert.Equal(t, uint32(1), e.epoch)
	assert.Empty(t, drain(sub))
	startRun(t, m, ft)
	waitsFor(t, ft, sub, 7900*time.Millisecond, presHoldFactor*2*time.Second)
}

// A track that pushed once, 5 s behind, then fell silent sets the hold to
// 7.5 s until the others have run more than the 8 s cap of stream time
// without it; then its lag is forgotten and the hold is MergeHold again.
func TestMergerStaleLagPruned(t *testing.T) {
	const fd = 3600 // 25 fps
	ft := newFakeTime()
	m, sub := newTestPresMerger(ft)
	both := func(from, to int) {
		for d := from; d <= to; d++ {
			m.push(lagFrame(0, 1, int64(d)*fd, d%25 == 0))
			m.push(lagFrame(2, 1, int64(d)*fd, d%25 == 0))
		}
	}
	both(0, 125)                    // 5 s, to DTS 450000
	m.push(lagFrame(1, 1, 0, true)) // 450000 ticks behind
	headHold := func() time.Duration {
		m.mu.Lock()
		defer m.mu.Unlock()
		c, ok := m.candidate()
		require.True(t, ok)
		require.NotEmpty(t, m.missing(c), "the head waits")
		dl, _ := m.deadline()
		return dl.Sub(m.q[c][0].at)
	}
	both(126, 125+200) // 8 s of stream time since track 1's push: not more
	assert.Equal(t, 7500*time.Millisecond, headHold(), "8 s: track 1's lag still counts")
	both(125+201, 125+225) // 9 s
	m.push(lagFrame(0, 1, int64(125+226)*fd, false))
	assert.Equal(t, 2*time.Second, headHold(), "9 s: track 1's lag is forgotten")
	m.mu.Lock()
	assert.NotContains(t, m.lag, 1)
	m.mu.Unlock()
	drain(sub)
	startRun(t, m, ft)
	waitsFor(t, ft, sub, 1900*time.Millisecond, 2*time.Second)
}

// The lag learned in epoch 1 survives into epoch 2's start, and is
// forgotten once epoch 2 has run more than the cap without the track.
func TestMergerLagSurvivesEpochChange(t *testing.T) {
	const fd = 9000
	ft := newFakeTime()
	m, _ := newTestPresMerger(ft)
	next := learnLag(m)
	hold := func() (time.Duration, int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.holds(false)
	}
	for k := range 11 { // 1 s of epoch 2
		m.push(lagFrame(0, 2, next+int64(k)*fd, k%20 == 0))
	}
	d, ticks := hold()
	assert.Equal(t, 3750*time.Millisecond, d, "epoch 2 after 1 s")
	assert.Equal(t, int64(337500), ticks)
	for k := 11; k <= 80; k++ { // 8 s: not more than the cap
		m.push(lagFrame(0, 2, next+int64(k)*fd, k%20 == 0))
	}
	d, _ = hold()
	assert.Equal(t, 3750*time.Millisecond, d, "epoch 2 after 8 s")
	m.push(lagFrame(0, 2, next+81*fd, false))
	d, ticks = hold()
	assert.Equal(t, 2*time.Second, d, "epoch 2 after 8.1 s")
	assert.Equal(t, int64(2*media.ClockRate), ticks)
	m.mu.Lock()
	assert.NotContains(t, m.lag, 1, "track 1's entry is gone")
	m.mu.Unlock()
}
