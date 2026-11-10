package hub

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

var (
	h264 = &media.TrackConfig{Codec: media.CodecH264}
	h265 = &media.TrackConfig{Codec: media.CodecH265}
	aac  = &media.TrackConfig{Codec: media.CodecAAC}
)

const (
	vDur = 3600 // 25 fps video
	aDur = 1920 // AAC at 48 kHz
)

func vid(track int, dts int64, key bool) media.Frame {
	return media.Frame{Track: track, Config: h264, PTS: dts, DTS: dts, Key: key}
}

func hevc(dts int64, key, rasl bool) media.Frame {
	return media.Frame{Track: 0, Config: h265, PTS: dts, DTS: dts, Key: key, RASL: rasl}
}

func aud(track int, dts int64) media.Frame {
	return media.Frame{Track: track, Config: aac, PTS: dts, DTS: dts, Key: true}
}

// writeGOP writes one GOP of n frames on track 0 starting at dts and returns
// the DTS after it.
func writeGOP(h *Hub, dts int64, n int) int64 {
	for i := range n {
		h.Write(vid(0, dts, i == 0))
		dts += vDur
	}
	return dts
}

// recv takes one delivery without blocking: the hub starts no goroutines, so
// everything Write delivers is already queued when it returns.
func recv(t *testing.T, s *Sub) Delivery {
	t.Helper()
	select {
	case d, ok := <-s.C():
		require.True(t, ok, "channel closed")
		return d
	default:
		require.FailNow(t, "no delivery queued")
		return Delivery{}
	}
}

func requireEmpty(t *testing.T, s *Sub) {
	t.Helper()
	select {
	case d, ok := <-s.C():
		require.Failf(t, "unexpected delivery", "ok=%v frame=%+v lost=%d", ok, d.Frame, d.Lost)
	default:
	}
}

func drain(t *testing.T, s *Sub, n int) []Delivery {
	t.Helper()
	out := make([]Delivery, 0, n)
	for range n {
		out = append(out, recv(t, s))
	}
	requireEmpty(t, s)
	return out
}

func TestHubDeliversInOrder(t *testing.T) {
	h := New(Options{})
	a := h.Subscribe("a", 0)
	b := h.Subscribe("b", 0)
	var want []media.Frame
	var dts int64
	for i := range 30 {
		f := vid(0, dts, i%10 == 0)
		g := aud(1, dts)
		h.Write(f)
		h.Write(g)
		want = append(want, f, g)
		dts += vDur
	}
	for _, s := range []*Sub{a, b} {
		got := drain(t, s, len(want))
		for i, d := range got {
			assert.Equal(t, want[i], d.Frame, "delivery %d", i)
			assert.Zero(t, d.Lost, "delivery %d", i)
		}
	}
}

func TestHubSlowSubscriberSkipsToKeyframe(t *testing.T) {
	h := New(Options{})
	s := h.Subscribe("slow", 4)

	dts := writeGOP(h, 0, 10)
	got := drain(t, s, 4)
	for i, d := range got {
		assert.Equal(t, int64(i*vDur), d.Frame.DTS)
		assert.Zero(t, d.Lost)
	}

	for i := range 10 {
		h.Write(vid(0, dts, i == 0))
		d := recv(t, s)
		assert.Equal(t, dts, d.Frame.DTS, "frame %d", i)
		if i == 0 {
			assert.True(t, d.Frame.Key)
			assert.Equal(t, uint64(6), d.Lost)
		} else {
			assert.Zero(t, d.Lost, "frame %d", i)
		}
		dts += vDur
	}
	requireEmpty(t, s)
	assert.Equal(t, []SubStats{{Name: "slow", Delivered: 14, Dropped: 6}}, h.Stats())
}

func TestHubResumeDropsRASL(t *testing.T) {
	h := New(Options{})
	s := h.Subscribe("s", 1)

	h.Write(hevc(0, true, false))
	h.Write(hevc(vDur, false, false)) // queue full: skip mode
	recv(t, s)
	h.Write(hevc(2*vDur, false, false)) // dropped: before the keyframe

	h.Write(hevc(3*vDur, true, false)) // CRA
	d := recv(t, s)
	assert.Equal(t, int64(3*vDur), d.Frame.DTS)
	assert.Equal(t, uint64(2), d.Lost)

	h.Write(hevc(4*vDur, false, true)) // RASL: dropped
	h.Write(hevc(5*vDur, false, true)) // RASL: dropped
	requireEmpty(t, s)
	h.Write(hevc(6*vDur, false, false))
	d = recv(t, s)
	assert.Equal(t, int64(6*vDur), d.Frame.DTS)
	assert.Zero(t, d.Lost, "the gap was signalled on the CRA, not mid-GOP")

	// Once resumed, RASL frames after a CRA are decodable and delivered.
	h.Write(hevc(7*vDur, true, false))
	d = recv(t, s)
	assert.Equal(t, int64(7*vDur), d.Frame.DTS)
	assert.Zero(t, d.Lost)
	h.Write(hevc(8*vDur, false, true))
	d = recv(t, s)
	assert.Equal(t, int64(8*vDur), d.Frame.DTS)
	assert.Zero(t, d.Lost)

	// A late joiner on a cached CRA skips the RASL frames after it.
	h2 := New(Options{})
	h2.Write(hevc(0, true, false))
	h2.Write(hevc(vDur, false, true))
	h2.Write(hevc(2*vDur, false, false))
	h2.Write(hevc(3*vDur, false, true)) // not directly after the keyframe
	late := h2.Subscribe("late", 0)
	got := drain(t, late, 3)
	assert.Equal(t, []int64{0, 2 * vDur, 3 * vDur},
		[]int64{got[0].Frame.DTS, got[1].Frame.DTS, got[2].Frame.DTS})
	for _, d := range got {
		assert.Zero(t, d.Lost)
	}
}

func TestHubTwoVideoTracks(t *testing.T) {
	t.Run("skip mode", func(t *testing.T) {
		h := New(Options{})
		s := h.Subscribe("s", 2)
		h.Write(vid(0, 0, true))
		h.Write(vid(1, 0, true))
		h.Write(vid(0, vDur, false)) // queue full: skip mode
		drain(t, s, 2)

		h.Write(vid(1, vDur, false))  // dropped: track 1 waits for its keyframe
		h.Write(aud(2, vDur))         // dropped: track 1 has not resumed
		h.Write(vid(0, 2*vDur, true)) // track 0 resumes
		d := recv(t, s)
		assert.Equal(t, 0, d.Frame.Track)
		assert.True(t, d.Frame.Key)
		assert.Equal(t, uint64(3), d.Lost)

		h.Write(vid(1, 2*vDur, false)) // still dropped though track 0 resumed
		h.Write(aud(2, 2*vDur))        // still dropped
		requireEmpty(t, s)
		h.Write(vid(0, 3*vDur, false))
		d = recv(t, s)
		assert.Equal(t, 0, d.Frame.Track)
		assert.Zero(t, d.Lost, "the gap was signalled on track 0's keyframe")

		h.Write(vid(1, 3*vDur, true)) // track 1 resumes
		d = recv(t, s)
		assert.Equal(t, 1, d.Frame.Track)
		assert.True(t, d.Frame.Key)
		assert.Zero(t, d.Lost)

		h.Write(aud(2, 3*vDur)) // every live video track resumed
		h.Write(vid(1, 4*vDur, false))
		got := drain(t, s, 2)
		assert.Equal(t, 2, got[0].Frame.Track)
		assert.Equal(t, 1, got[1].Frame.Track)
		assert.Zero(t, got[0].Lost+got[1].Lost)
	})

	t.Run("late joiner", func(t *testing.T) {
		h := New(Options{})
		h.Write(vid(1, 0, false)) // before the cache starts
		h.Write(vid(0, vDur, true))
		h.Write(vid(1, vDur, false))
		h.Write(aud(2, vDur))
		h.Write(vid(0, 2*vDur, false))
		h.Write(vid(1, 2*vDur, false))

		s := h.Subscribe("late", 0)
		got := drain(t, s, 2) // only track 0 from the cache
		for _, d := range got {
			assert.Equal(t, 0, d.Frame.Track)
			assert.Zero(t, d.Lost)
		}
		assert.True(t, got[0].Frame.Key)

		h.Write(vid(1, 3*vDur, false)) // still gated
		h.Write(aud(2, 3*vDur))        // waits for track 1
		h.Write(vid(0, 3*vDur, false))
		d := recv(t, s)
		assert.Equal(t, 0, d.Frame.Track)
		assert.Zero(t, d.Lost, "frames gated after a replay are not lost")

		h.Write(vid(1, 4*vDur, true))
		h.Write(aud(2, 4*vDur))
		got = drain(t, s, 2)
		assert.Equal(t, 1, got[0].Frame.Track)
		assert.True(t, got[0].Frame.Key)
		assert.Equal(t, 2, got[1].Frame.Track)
		assert.Zero(t, got[0].Lost+got[1].Lost)
	})
}

func TestHubLostOnlyOnResumeCRA(t *testing.T) {
	h := New(Options{})
	s := h.Subscribe("s", 1)
	var got []Delivery
	write := func(f media.Frame) {
		h.Write(f)
		select {
		case d := <-s.C():
			got = append(got, d)
		default:
		}
	}

	h.Write(hevc(0, true, false))
	h.Write(aud(1, 0)) // queue full: skip mode
	recv(t, s)
	write(hevc(vDur, false, false))  // dropped
	write(aud(1, vDur))              // dropped
	write(hevc(2*vDur, true, false)) // resume CRA
	write(aud(1, 2*vDur))
	write(hevc(3*vDur, false, true)) // RASL: dropped
	write(aud(1, 3*vDur))
	write(hevc(4*vDur, false, true)) // RASL: dropped
	write(aud(1, 4*vDur))
	write(hevc(5*vDur, false, false))
	write(aud(1, 5*vDur))

	require.Len(t, got, 6)
	var withLost []Delivery
	for _, d := range got {
		if d.Lost > 0 {
			withLost = append(withLost, d)
		}
	}
	require.Len(t, withLost, 1)
	assert.Equal(t, int64(2*vDur), withLost[0].Frame.DTS)
	assert.True(t, withLost[0].Frame.Key)
	assert.Equal(t, media.KindVideo, withLost[0].Frame.Kind())
	assert.Equal(t, uint64(3), withLost[0].Lost) // the failed send and two gated frames
	assert.Equal(t, []SubStats{{Name: "s", Delivered: 7, Dropped: 5}}, h.Stats())
}

func TestHubAllIntraGateOpens(t *testing.T) {
	h := New(Options{})
	s := h.Subscribe("s", 1)
	h.Write(vid(0, 0, true))
	h.Write(vid(0, vDur, true)) // queue full: skip mode
	recv(t, s)
	h.Write(vid(0, 2*vDur, true))
	d := recv(t, s)
	assert.Equal(t, uint64(1), d.Lost)
	h.Write(vid(0, 3*vDur, true))
	recv(t, s)
	h.mu.Lock()
	g := s.gates[0]
	h.mu.Unlock()
	assert.Equal(t, gateOpen, g, "a non-RASL keyframe opens the gate")

	// A new video track starting on a non-key frame is gated, and so is
	// audio, but the subscriber resumed long ago: nothing is lost.
	h.Write(vid(1, 3*vDur, false))
	h.Write(aud(2, 3*vDur))
	requireEmpty(t, s)
	h.Write(vid(1, 4*vDur, true))
	d = recv(t, s)
	assert.Equal(t, 1, d.Frame.Track)
	assert.Zero(t, d.Lost)
	h.Write(vid(0, 4*vDur, true))
	d = recv(t, s)
	assert.Zero(t, d.Lost)
}

func TestHubStreamTimeIsMaxDTS(t *testing.T) {
	h := New(Options{})
	s := h.Subscribe("s", 1)
	h.Write(vid(1, 0, true)) // track 1 video, then silent
	h.Write(vid(0, 0, true)) // queue full: skip mode, track 1 gated
	recv(t, s)
	h.Write(vid(0, 11*media.ClockRate, true)) // track 1 has ended
	recv(t, s)
	// A late frame with a small DTS does not pull stream time back, so
	// track 1 stays ended and does not hold the audio back.
	h.Write(aud(2, media.ClockRate))
	d := recv(t, s)
	assert.Equal(t, media.KindAudio, d.Frame.Kind())
}

func TestHubVideoTrackEnds(t *testing.T) {
	const step = 2000 // divides the 10 s window exactly
	h := New(Options{})
	s := h.Subscribe("s", 1)
	h.Write(vid(0, 0, true))
	h.Write(vid(0, vDur, false)) // queue full: skip mode
	recv(t, s)

	// The video track is live while its last DTS is within 10 s of the
	// newest frame, boundary included.
	var i int64
	for i = 1; i*step <= 10*media.ClockRate; i++ {
		h.Write(aud(1, vDur+i*step))
	}
	requireEmpty(t, s)
	assert.Equal(t, int64(451), i)
	h.Write(aud(1, vDur+i*step)) // 10 s + 2000 ticks without video
	d := recv(t, s)
	assert.Equal(t, media.KindAudio, d.Frame.Kind())
	assert.Equal(t, uint64(451), d.Lost) // the failed send and 450 audio frames

	// Video comes back on a non-key frame: it is live again and gates audio,
	// but the subscriber had resumed, so the gated frames are not lost.
	dts := vDur + (i+1)*step
	h.Write(vid(0, dts, false))
	h.Write(aud(1, dts))
	requireEmpty(t, s)
	h.Write(vid(0, dts+vDur, true))
	d = recv(t, s)
	assert.True(t, d.Frame.Key)
	assert.Zero(t, d.Lost)
}

func TestHubWriteNeverBlocks(t *testing.T) {
	const n = 100_000
	h := New(Options{})
	h.Subscribe("stuck", 0)
	h.Subscribe("reader", 1)
	for i := range n {
		h.Write(vid(0, int64(i)*vDur, i%50 == 0))
	}
	assert.Equal(t, []SubStats{
		{Name: "stuck", Delivered: 1024, Dropped: n - 1024},
		{Name: "reader", Delivered: 1, Dropped: n - 1},
	}, h.Stats())
}

func TestHubLateJoinStartsOnKey(t *testing.T) {
	h := New(Options{})
	h.Write(vid(0, 0, false)) // before the first keyframe: never cached
	h.Write(vid(0, vDur, false))
	writeGOP(h, 2*vDur, 6)

	s := h.Subscribe("late", 0)
	got := drain(t, s, 6)
	for i, d := range got {
		assert.Equal(t, int64((i+2)*vDur), d.Frame.DTS)
		assert.Equal(t, i == 0, d.Frame.Key)
		assert.Zero(t, d.Lost)
	}
}

func TestHubLateJoinCacheTooLarge(t *testing.T) {
	h := New(Options{})
	dts := writeGOP(h, 0, 5)
	s := h.Subscribe("small", 2)
	requireEmpty(t, s)

	h.Write(vid(0, dts, false)) // dropped: waiting for a keyframe
	requireEmpty(t, s)
	dts += vDur
	h.Write(vid(0, dts, true))
	d := recv(t, s)
	assert.True(t, d.Frame.Key)
	assert.Equal(t, dts, d.Frame.DTS)
	assert.Zero(t, d.Lost, "a new subscriber has lost nothing")
}

func TestHubGOPCacheCap(t *testing.T) {
	h := New(Options{GOPCacheFrames: 4})
	writeGOP(h, 0, 4)
	drain(t, h.Subscribe("fits", 0), 4)

	dts := writeGOP(h, 10*vDur, 5) // over-long GOP: the cache empties
	s := h.Subscribe("over", 0)
	requireEmpty(t, s)
	h.Write(vid(0, dts, true))
	d := recv(t, s)
	assert.True(t, d.Frame.Key)
	drain(t, h.Subscribe("again", 0), 1)
}

func TestHubAudioOnly(t *testing.T) {
	h := New(Options{})
	for i := range 100 {
		h.Write(aud(0, int64(i)*aDur))
	}
	got := drain(t, h.Subscribe("late", 0), 47)
	assert.Equal(t, int64(53*aDur), got[0].Frame.DTS)
	assert.Equal(t, int64(99*aDur), got[46].Frame.DTS)
	assert.Less(t, got[46].Frame.DTS-got[0].Frame.DTS, int64(media.ClockRate))

	h2 := New(Options{AudioCacheTicks: 3 * aDur})
	s := h2.Subscribe("s", 1)
	h2.Write(aud(0, 0))
	h2.Write(aud(0, aDur)) // queue full: skip mode
	recv(t, s)
	h2.Write(aud(0, 2*aDur)) // skip mode ends at the next frame
	d := recv(t, s)
	assert.Equal(t, int64(2*aDur), d.Frame.DTS)
	assert.Equal(t, uint64(1), d.Lost)
	for i := 3; i < 10; i++ {
		h2.Write(aud(0, int64(i)*aDur))
		recv(t, s)
	}
	assert.Len(t, drain(t, h2.Subscribe("late", 0), 3), 3)
}

func TestHubAudioCacheAfterVideoEnds(t *testing.T) {
	h := New(Options{AudioCacheTicks: 4 * aDur})
	h.Write(vid(0, 0, true))
	var dts int64
	for dts = aDur; dts <= 10*media.ClockRate+20*aDur; dts += aDur {
		h.Write(aud(1, dts))
	}
	got := drain(t, h.Subscribe("late", 0), 4)
	for _, d := range got {
		assert.Equal(t, media.KindAudio, d.Frame.Kind())
	}
}

func TestHubStats(t *testing.T) {
	h := New(Options{})
	fast := h.Subscribe("fast", 0)
	h.Subscribe("slow", 1)
	writeGOP(h, 0, 3)
	drain(t, fast, 3)
	assert.Equal(t, []SubStats{
		{Name: "fast", Delivered: 3, Dropped: 0},
		{Name: "slow", Delivered: 1, Dropped: 2},
	}, h.Stats())
}

func TestHubClose(t *testing.T) {
	h := New(Options{})
	a := h.Subscribe("a", 0)
	b := h.Subscribe("b", 0)
	h.Write(vid(0, 0, true))

	a.Close()
	a.Close() // idempotent
	d, ok := <-a.C()
	require.True(t, ok, "queued deliveries stay readable")
	assert.True(t, d.Frame.Key)
	_, ok = <-a.C()
	assert.False(t, ok)
	h.Write(vid(0, vDur, false)) // a is gone; b still gets it
	assert.Equal(t, []SubStats{{Name: "b", Delivered: 2}}, h.Stats())

	h.Close()
	h.Close()
	assert.NotPanics(t, func() { h.Write(vid(0, 2*vDur, false)) })
	drain2 := 0
	for range b.C() {
		drain2++
	}
	assert.Equal(t, 2, drain2)
	b.Close()
	assert.Empty(t, h.Stats())

	c := h.Subscribe("c", 0)
	_, ok = <-c.C()
	assert.False(t, ok, "subscribing to a closed hub returns a closed channel")
	c.Close()
}

func TestHubConcurrentChurn(t *testing.T) {
	const iterations = 10_000
	h := New(Options{GOPCacheFrames: 64})

	type result struct {
		delivered, lost uint64
		ordered         bool
	}
	readers := make([]*Sub, 4)
	results := make([]result, len(readers))
	var wg sync.WaitGroup
	for i := range readers {
		readers[i] = h.Subscribe(fmt.Sprintf("reader%d", i), 8)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := result{ordered: true}
			last := map[int]int64{}
			for d := range readers[i].C() {
				if prev, ok := last[d.Frame.Track]; ok && d.Frame.DTS <= prev {
					r.ordered = false
				}
				last[d.Frame.Track] = d.Frame.DTS
				r.delivered++
				r.lost += d.Lost
			}
			results[i] = r
		}()
	}

	var work sync.WaitGroup
	work.Add(2)
	go func() {
		defer work.Done()
		for i := range iterations {
			dts := int64(i) * vDur
			h.Write(vid(0, dts, i%30 == 0))
			h.Write(aud(1, dts))
		}
	}()
	go func() {
		defer work.Done()
		for i := range iterations {
			s := h.Subscribe("churn", 1+i%16)
			select {
			case <-s.C():
			default:
			}
			if i%100 == 0 {
				_ = h.Stats()
			}
			s.Close()
		}
	}()
	work.Wait()

	stats := map[string]SubStats{}
	for _, st := range h.Stats() {
		stats[st.Name] = st
	}
	h.Close()
	wg.Wait()

	for i, r := range results {
		st := stats[fmt.Sprintf("reader%d", i)]
		assert.True(t, r.ordered, "reader %d out of order", i)
		assert.Equal(t, uint64(2*iterations), st.Delivered+st.Dropped, "reader %d", i)
		assert.Equal(t, st.Delivered, r.delivered, "reader %d", i)
		assert.LessOrEqual(t, r.lost, st.Dropped, "reader %d", i)
	}
	assert.Len(t, stats, len(readers), "every churn subscriber was removed")
}
