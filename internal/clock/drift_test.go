package clock

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// Drift tests feed half an hour of fake arrival times through PushAt. Most
// use video alone at 25 fps: drift is measured per released frame, whatever
// its kind, so audio would only multiply the frame count under -race. The
// audio subtests are shorter, just long enough for one drift break.

var (
	vOnly   = []media.Kind{media.KindVideo}
	aOnly   = []media.Kind{media.KindAudio}
	driftT0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
)

const (
	driftMax = 2 * time.Second
	frameDur = 40 * time.Millisecond // vDur
	halfHour = 30 * 60 * 25          // 25 fps video frames in 30 minutes
	skew     = 0.002                 // 0.2 %
)

// skewed returns the arrival of a frame whose source time is d, from a
// source whose clock runs s slower than arrival (s < 0: faster), starting at
// from.
func skewed(from time.Time, d time.Duration, s float64) time.Time {
	return from.Add(time.Duration(float64(d) * (1 + s)))
}

// source returns the arrival of frame i of a 25 fps track whose clock runs s
// slower than arrival.
func source(s float64) func(i int) time.Time {
	return func(i int) time.Time { return skewed(driftT0, time.Duration(i)*frameDur, s) }
}

// byDTS returns the arrival of a frame from its source DTS, plus lag for
// audio, from a source whose clock runs s slower than arrival.
func byDTS(s float64, lag int64) func(f media.Frame) time.Time {
	return func(f media.Frame) time.Time {
		dts := f.DTS
		if f.Track == 1 {
			dts += lag
		}
		return skewed(driftT0, media.ToDuration(dts), s)
	}
}

// driftResult is the output of a run, and for each released epoch the
// drifts evaluated in it, in order, its baseline, and the arrival of the push
// that released it.
type driftResult struct {
	out       []media.Frame
	drifts    map[uint32][]time.Duration
	baselines map[uint32]time.Time
	released  map[uint32]time.Time
}

func newDriftResult() *driftResult {
	return &driftResult{
		drifts:    map[uint32][]time.Duration{},
		baselines: map[uint32]time.Time{},
		released:  map[uint32]time.Time{},
	}
}

func (r *driftResult) push(c *Clock, f media.Frame, at time.Time) {
	out := c.PushAt(f, at)
	for _, o := range out {
		if _, ok := r.released[o.Epoch]; !ok {
			r.released[o.Epoch] = at
		}
	}
	r.out = append(r.out, out...)
	if ep := c.cur; ep != nil && ep.drift.evals > len(r.drifts[ep.id]) {
		r.drifts[ep.id] = append(r.drifts[ep.id], ep.drift.last)
		r.baselines[ep.id] = ep.drift.baseline
	}
}

// feed pushes frames[i] at at(i).
func feed(c *Clock, frames []media.Frame, at func(i int) time.Time) *driftResult {
	r := newDriftResult()
	for i, f := range frames {
		r.push(c, f, at(i))
	}
	return r
}

// feedFrames pushes each frame at at(f).
func feedFrames(c *Clock, frames []media.Frame, at func(f media.Frame) time.Time) *driftResult {
	return feed(c, frames, func(i int) time.Time { return at(frames[i]) })
}

// framesIn is the number of 25 fps frames in d.
func framesIn(d time.Duration) uint64 { return uint64(d / frameDur) }

// requireNoOverlap checks Wall_n >= Wall_{n-1} + Δ for every epoch.
func requireNoOverlap(t *testing.T, c *Clock) {
	t.Helper()
	eps := c.Epochs()
	for n := 1; n < len(eps); n++ {
		end := eps[n-1].Wall.Add(media.ToDuration(eps[n].Start - eps[n-1].Start))
		require.False(t, eps[n].Wall.Before(end), "epoch %d starts %v before the previous one ends", eps[n].ID, end.Sub(eps[n].Wall))
	}
}

// requireDriftWall checks Wall_n = R_n = ceil_ms(arrival_key − b) for the
// drift epoch n = 2 of a video-only run, whose keyframe releases it on its
// own push. Epoch 1's baseline must lie in [lo, hi].
func requireDriftWall(t *testing.T, c *Clock, r *driftResult, lo, hi time.Time) {
	t.Helper()
	eps := c.Epochs()
	require.GreaterOrEqual(t, len(eps), 2)
	baseline := r.baselines[1]
	require.False(t, baseline.Before(lo) || baseline.After(hi), "baseline %v", baseline.Sub(driftT0))
	b := baseline.Sub(eps[0].Wall)
	assert.Equal(t, ceilMS(r.released[2].Add(-b)), eps[1].Wall)
	if c.cur.id == 2 {
		assert.Equal(t, c.cur.wall, c.cur.raw)
	}
}

// checkDriftReset checks that the drift stays within MaxDrift (plus one
// second's evaluation step) and restarts near 0 in every epoch after a
// drift break.
func checkDriftReset(t *testing.T, c *Clock, r *driftResult) {
	t.Helper()
	for _, e := range c.Epochs() {
		ds := r.drifts[e.ID]
		require.NotEmpty(t, ds, "epoch %d", e.ID)
		for _, d := range ds {
			assert.LessOrEqual(t, d.Abs(), driftMax+50*time.Millisecond, "epoch %d", e.ID)
		}
		if e.ID > 1 {
			assert.Less(t, ds[0].Abs(), 50*time.Millisecond, "epoch %d starts with drift %v", e.ID, ds[0])
		}
	}
}

func TestDriftSlowEncoder(t *testing.T) {
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax})
	r := feed(c, videoRun(0, halfHour), source(skew))
	st := c.Stats()
	require.GreaterOrEqual(t, st.DriftBreaks, uint64(1))
	require.Len(t, c.Epochs(), int(1+st.DriftBreaks))
	assert.Zero(t, st.DroppedDrift)
	requireNoOverlap(t, c)
	checkDriftReset(t, c, r)
	requireConform(t, r.out)

	// A positive-drift break leaves a wall gap of about the drift.
	eps := c.Epochs()
	gap := eps[1].Wall.Sub(eps[0].Wall.Add(media.ToDuration(eps[1].Start - eps[0].Start)))
	assert.InDelta(t, float64(driftMax), float64(gap), float64(100*time.Millisecond))
	requireDriftWall(t, c, r, driftT0, driftT0)
}

func TestDriftFastEncoder(t *testing.T) {
	t.Run("video", func(t *testing.T) {
		c := New(Options{Kinds: vOnly, MaxDrift: driftMax})
		r := feed(c, videoRun(0, halfHour), source(-skew))
		st := c.Stats()
		require.GreaterOrEqual(t, st.DriftBreaks, uint64(1))
		require.Len(t, c.Epochs(), int(1+st.DriftBreaks))
		assert.Positive(t, st.DroppedDrift)
		assert.LessOrEqual(t, st.DroppedDrift, st.DriftBreaks*framesIn(driftMax+time.Second))
		requireNoOverlap(t, c)
		checkDriftReset(t, c, r)
		requireConform(t, r.out)
		requireDriftWall(t, c, r, driftT0.Add(-21*time.Millisecond), driftT0.Add(-19*time.Millisecond))
	})

	// Audio presented before the break keyframe, arriving after it (200 ms
	// late), stays in the old epoch, and the new one starts with audio
	// aligned to its keyframe.
	t.Run("audio and video", func(t *testing.T) {
		const n = 20 * 60 * 25 // 20 minutes: one break
		const lag = 18000
		v := videoRun(0, n)
		a := audioRun(0, int(int64(n)*vDur/aDur))
		c := New(Options{Kinds: av, MaxDrift: driftMax})
		r := feedFrames(c, mux(lag, v, a), byDTS(-skew, lag))
		st := c.Stats()
		require.Equal(t, uint64(1), st.DriftBreaks)
		require.Len(t, c.Epochs(), 2)
		requireNoOverlap(t, c)
		checkDriftReset(t, c, r)
		requireConform(t, r.out)
		assert.Zero(t, st.DroppedStale+st.DroppedLate+st.DroppedBackStep)

		// Epoch 1 maps source time to itself; its video ends right before the
		// break keyframe, and it has every audio frame presented before it.
		e1 := inEpoch(r.out, 1)
		ev := onTrack(e1, 0)
		cut := ev[len(ev)-1].DTS + vDur
		assert.Len(t, onTrack(e1, 1), int((cut+aDur-1)/aDur))
	})

	// Any frame opens the drift epoch of an audio-only stream.
	t.Run("audio only", func(t *testing.T) {
		a := audioRun(0, 20*60*48000/1024)
		c := New(Options{Kinds: aOnly, MaxDrift: driftMax})
		r := feedFrames(c, a, byDTS(-skew, 0))
		st := c.Stats()
		require.Equal(t, uint64(1), st.DriftBreaks)
		require.Len(t, c.Epochs(), 2)
		assert.LessOrEqual(t, st.DroppedDrift, uint64((driftMax+40*time.Millisecond)*48000/1024/time.Second))
		requireNoOverlap(t, c)
		checkDriftReset(t, c, r)
		requireConform(t, r.out)
	})
}

func TestDriftJitterIgnored(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 2))
	const jitter = 800 * time.Millisecond
	at := func(i int) time.Time {
		j := jitter
		if i > 0 {
			j = time.Duration(rng.Int64N(int64(2*jitter)+1)) - jitter
		}
		return driftT0.Add(time.Duration(i)*frameDur + j)
	}
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax})
	r := feed(c, videoRun(0, halfHour), at)
	assert.Zero(t, c.Stats().DriftBreaks)
	assert.Len(t, c.Epochs(), 1)
	requireNearZero(t, r)
}

// requireNearZero checks that every drift evaluated in epoch 1 is near 0: the
// baseline and the samples both find the true offset, whatever the first
// frame's delay or a burst.
func requireNearZero(t *testing.T, r *driftResult) {
	t.Helper()
	require.NotEmpty(t, r.drifts[1])
	for _, d := range r.drifts[1] {
		require.Less(t, d.Abs(), 100*time.Millisecond)
	}
}

// burst returns the arrival of frame i of a 25 fps track whose first n
// frames are delivered at once, and the rest from then on with a clock
// running s slower than arrival.
func burst(n int, s float64) func(i int) time.Time {
	return func(i int) time.Time {
		if i < n {
			return driftT0
		}
		return skewed(driftT0, time.Duration(i-n+1)*frameDur, s)
	}
}

func TestDriftBurstStart(t *testing.T) {
	const gop = 50 // 2 s
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax})
	r := feed(c, gopRun(0, h264, 0, halfHour, gop), burst(gop, 0))
	assert.Zero(t, c.Stats().DriftBreaks)
	assert.Zero(t, c.Stats().DroppedDrift)
	assert.Len(t, c.Epochs(), 1)
	requireNearZero(t, r)
}

// A 2 s burst raises the epoch's constant offset by 2 s; the drift epoch
// keeps it, so it drops only the drift (plus up to a GOP for the keyframe),
// not the burst as well.
func TestDriftFastAfterBurst(t *testing.T) {
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax})
	r := feed(c, videoRun(0, halfHour), burst(2*videoGOP, -skew))
	st := c.Stats()
	require.GreaterOrEqual(t, st.DriftBreaks, uint64(1))
	assert.Positive(t, st.DroppedDrift)
	assert.LessOrEqual(t, st.DroppedDrift, st.DriftBreaks*framesIn(driftMax+time.Second))
	requireNoOverlap(t, c)
	checkDriftReset(t, c, r)
	burstEnd := driftT0.Add(-(2*videoGOP - 1) * frameDur) // the burst's offset
	requireDriftWall(t, c, r, burstEnd.Add(-21*time.Millisecond), burstEnd.Add(-19*time.Millisecond))
}

func TestSetDrift(t *testing.T) {
	t.Run("clamped", func(t *testing.T) {
		c := New(Options{Kinds: vOnly, MaxDrift: time.Second})
		assert.Equal(t, 2*media.MaxAnchorLead, c.pend.drift.max)
		c = New(Options{Kinds: vOnly})
		assert.Zero(t, c.pend.drift.max)
		c.SetDrift(time.Second, false, 0)
		assert.Equal(t, 2*media.MaxAnchorLead, c.pend.drift.max)
		c.SetDrift(3*time.Second, true, 0)
		assert.Equal(t, driftCfg{max: 3 * time.Second, positiveOnly: true, window: 10}, c.pend.drift)
	})

	t.Run("after construction", func(t *testing.T) {
		c := New(Options{Kinds: vOnly})
		c.SetDrift(driftMax, false, 0)
		r := feed(c, videoRun(0, halfHour), source(skew))
		assert.GreaterOrEqual(t, c.Stats().DriftBreaks, uint64(1))
		requireNoOverlap(t, c)
		checkDriftReset(t, c, r)
	})

	t.Run("after Break", func(t *testing.T) {
		frames := videoRun(0, halfHour)
		at := source(skew)
		c := New(Options{Kinds: vOnly})
		r := newDriftResult()
		for i, f := range frames {
			if i == 100 {
				c.Break(true)
				c.SetDrift(driftMax, false, 0)
			}
			r.push(c, f, at(i))
		}
		st := c.Stats()
		require.GreaterOrEqual(t, st.DriftBreaks, uint64(1))
		eps := c.Epochs()
		require.Len(t, eps, int(2+st.DriftBreaks))
		assert.Empty(t, r.drifts[1], "epoch 1 had drift detection disabled")
		requireNoOverlap(t, c)
	})

	// With no epoch pending, the setting applies from the next epoch opened.
	t.Run("released epoch unchanged", func(t *testing.T) {
		frames := videoRun(0, halfHour)
		at := source(skew)
		c := New(Options{Kinds: vOnly})
		for i, f := range frames {
			if i == 100 {
				require.Nil(t, c.pend)
				c.SetDrift(driftMax, false, 0)
			}
			c.PushAt(f, at(i))
		}
		assert.Zero(t, c.Stats().DriftBreaks)
		assert.Len(t, c.Epochs(), 1)
	})
}

// A second epoch placed 1 s past its arrival (the previous epoch's
// presentation excess, capped at MaxAnchorLead) keeps that lead as a
// constant: it is no drift.
func TestDriftAfterChainLead(t *testing.T) {
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax})
	first := videoRun(0, videoGOP)
	first[len(first)-1].PTS += 30 * vDur // 1.2 s past its DTS
	at := source(0)
	for i, f := range first {
		c.PushAt(f, at(i))
	}
	c.Break(false)
	r := feed(c, videoRun(videoGOP*vDur, halfHour), func(i int) time.Time { return at(videoGOP + i) })
	eps := c.Epochs()
	require.GreaterOrEqual(t, len(eps), 2)
	assert.Equal(t, media.MaxAnchorLead, eps[1].Wall.Sub(at(videoGOP)))
	assert.Zero(t, c.Stats().DriftBreaks)
	assert.Len(t, eps, 2)
	assert.NotEmpty(t, r.drifts[2])
}

func TestDriftPositiveOnly(t *testing.T) {
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax, DriftPositiveOnly: true})
	feed(c, videoRun(0, halfHour), source(-skew))
	assert.Zero(t, c.Stats().DriftBreaks)
	assert.Zero(t, c.Stats().DroppedDrift)

	c = New(Options{Kinds: vOnly, MaxDrift: driftMax, DriftPositiveOnly: true})
	feed(c, videoRun(0, halfHour), source(skew))
	assert.GreaterOrEqual(t, c.Stats().DriftBreaks, uint64(1))
}

func TestDriftDisabled(t *testing.T) {
	for _, s := range []float64{skew, -skew} {
		c := New(Options{Kinds: vOnly})
		feed(c, videoRun(0, halfHour), source(s))
		assert.Zero(t, c.Stats().DriftBreaks)
		assert.Zero(t, c.Stats().DroppedDrift)
		assert.Len(t, c.Epochs(), 1)
	}
}

// segmented returns the arrival of frame i of a 25 fps track delivered in
// segments of seg, as paced HLS pulls them: each segment arrives whole,
// 100 ms after it ends, from a source whose clock runs s slower than
// arrival; the segments in slow arrive late by that much on top.
func segmented(seg time.Duration, s float64, slow map[int]time.Duration) func(i int) time.Time {
	per := int(seg / frameDur)
	return func(i int) time.Time {
		k := i / per
		return skewed(driftT0, time.Duration(k+1)*seg, s).Add(100*time.Millisecond + slow[k])
	}
}

// With 6 s segments, a single fetch 2.5 s slower than the others is no
// drift: the window spans three segment arrivals, and the others in it keep
// its smallest offset where it was.
func TestDriftSlowFetchLongSegments(t *testing.T) {
	const seg = 6 * time.Second
	slow := map[int]time.Duration{20: 2500 * time.Millisecond, 80: 2500 * time.Millisecond, 200: 2500 * time.Millisecond}
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax, DriftPositiveOnly: true, ArrivalGranularity: seg})
	r := feed(c, videoRun(0, halfHour), segmented(seg, 0, slow))
	assert.Zero(t, c.Stats().DriftBreaks)
	assert.Len(t, c.Epochs(), 1)
	require.NotEmpty(t, r.drifts[1], "drift evaluated")
	for _, d := range r.drifts[1] {
		require.Less(t, d.Abs(), 100*time.Millisecond)
	}
}

// A source that keeps falling behind is still re-anchored with 6 s segments,
// once its drift has held over a whole window (three segments).
func TestDriftSlowSourceLongSegments(t *testing.T) {
	const seg = 6 * time.Second
	c := New(Options{Kinds: vOnly, MaxDrift: driftMax, DriftPositiveOnly: true, ArrivalGranularity: seg})
	r := feed(c, videoRun(0, halfHour), segmented(seg, skew, nil))
	st := c.Stats()
	require.GreaterOrEqual(t, st.DriftBreaks, uint64(1))
	requireNoOverlap(t, c)

	// The drift passes MaxDrift when k·seg·skew does. The break waits for
	// the window's oldest segment to be past it (three segments), then for
	// the next arrival to evaluate it, plus up to a segment of rounding.
	cross := driftT0.Add(time.Duration(float64(driftMax) / skew))
	at := r.released[2]
	assert.False(t, at.Before(cross), "break %v before the drift reached MaxDrift", cross.Sub(at))
	assert.Less(t, at.Sub(cross), 5*seg+2*time.Second, "break too late")
}

// The drift window grows to three arrival steps; ten seconds is its floor.
func TestDriftWindow(t *testing.T) {
	for _, tc := range []struct {
		gran time.Duration
		want int64
	}{
		{0, 10},
		{time.Second, 10},
		{4 * time.Second, 12},
		{6 * time.Second, 18},
		{6500 * time.Millisecond, 20},
	} {
		assert.Equal(t, tc.want, newDriftCfg(driftMax, false, tc.gran).window, "%v", tc.gran)
	}
	c := New(Options{Kinds: vOnly})
	c.SetDrift(driftMax, true, 6*time.Second)
	assert.Equal(t, int64(18), c.pend.drift.window, "SetDrift applies to the pending epoch")
}
