package transcode

import (
	"context"
	"math/rand/v2"
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
