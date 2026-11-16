package h26x

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dtsRef is one video packet of a reference file, in decode order.
type dtsRef struct {
	pts, dts int64
	key      bool
}

// dtsFixtures are the reference files with the codec of their .ts.
var dtsFixtures = []struct {
	name  string
	codec Codec
}{
	{"h264_aac", H264},
	{"h264_pyramid", H264},
	{"h264_opengop", H264},
	{"h265_aac51", H265},
}

// loadDTSRef reads testdata/fixtures/<name>_dts.txt: one "pts,dts,flags"
// line per packet, with or without a trailing comma.
func loadDTSRef(t *testing.T, name string) []dtsRef {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixtures", name+"_dts.txt"))
	require.NoError(t, err)
	defer f.Close()
	var out []dtsRef
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		require.True(t, len(fields) == 3 || (len(fields) == 4 && fields[3] == ""), "line %q", line)
		pts, err := strconv.ParseInt(fields[0], 10, 64)
		require.NoError(t, err)
		dts, err := strconv.ParseInt(fields[1], 10, 64)
		require.NoError(t, err)
		out = append(out, dtsRef{pts: pts, dts: dts, key: strings.HasPrefix(fields[2], "K")})
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, out)
	return out
}

// fixtureTiming returns the reorder depth and frame duration of a fixture
// from its first SPS, through ReorderOf and SPS.FrameDuration.
func fixtureTiming(t *testing.T, name string, c Codec) (reorder int, dur int64) {
	t.Helper()
	spss, _ := fixtureSets(t, name+".ts", c)
	s, err := ParseSPS(c, spss[0])
	require.NoError(t, err)
	r, assumed := ReorderOf(c, s)
	t.Logf("%s: ReorderFrames=%d ReorderOf=%d assumed=%v FrameDuration=%d", name, s.ReorderFrames, r, assumed, s.FrameDuration)
	return r, s.FrameDuration
}

// dtsOut is the result of one Extract call.
type dtsOut struct {
	dts int64
	st  DTSStatus
}

// extractAll feeds every packet to d in order. On DTSBreak it feeds the same
// packet again, as a caller does after emitting the discontinuity; the
// recorded status is DTSBreak and the DTS that of the second call when it
// is DTSOK.
func extractAll(d *DTSExtractor, refs []dtsRef) []dtsOut {
	out := make([]dtsOut, len(refs))
	for i, r := range refs {
		dts, st := d.Extract(r.pts, r.key)
		if st == DTSBreak {
			var again DTSStatus
			dts, again = d.Extract(r.pts, r.key)
			if again != DTSOK {
				dts = -1
			}
		}
		out[i] = dtsOut{dts: dts, st: st}
	}
	return out
}

// emitted reports whether the packet was given a DTS.
func (o dtsOut) emitted() bool {
	switch o.st {
	case DTSOK:
		return true
	case DTSBreak:
		return o.dts >= 0
	case DTSDrop:
		return false
	default:
		return false
	}
}

// assertValidDTS checks that the emitted DTS are strictly increasing and
// never above their PTS.
func assertValidDTS(t *testing.T, refs []dtsRef, out []dtsOut) {
	t.Helper()
	last, have := int64(0), false
	for i, o := range out {
		if !o.emitted() {
			continue
		}
		assert.LessOrEqual(t, o.dts, refs[i].pts, "frame %d: DTS above PTS", i)
		if have {
			assert.Greater(t, o.dts, last, "frame %d: DTS not increasing", i)
		}
		last, have = o.dts, true
	}
}

func TestDTSAgainstFFmpeg(t *testing.T) {
	for _, fx := range dtsFixtures {
		t.Run(fx.name, func(t *testing.T) {
			refs := loadDTSRef(t, fx.name)
			r, dur := fixtureTiming(t, fx.name, fx.codec)
			d := NewDTSExtractor(r, dur)
			out := extractAll(d, refs)
			assertValidDTS(t, refs, out)
			// The files start at a keyframe with the lowest PTS, so there
			// is no leading picture of the first keyframe to skip.
			require.Equal(t, DTSOK, out[0].st)
			c := refs[0].dts - out[0].dts
			for i, o := range out {
				require.Equal(t, DTSOK, o.st, "frame %d", i)
				assert.Equal(t, refs[i].dts-c, o.dts, "frame %d (pts %d)", i, refs[i].pts)
			}
			t.Logf("%s: c=%d", fx.name, c)
			assert.Equal(t, DTSStats{}, d.Stats())
		})
	}
}

func TestDTSNoReorder(t *testing.T) {
	for _, dur := range []int64{0, 3600} {
		d := NewDTSExtractor(0, dur)
		pts := int64(1_000_000)
		for i := range 100 {
			pts += 3000 + int64(i%3)*300 // jittered frame spacing
			dts, st := d.Extract(pts, i%25 == 0)
			require.Equal(t, DTSOK, st, "frame %d", i)
			require.Equal(t, pts, dts, "frame %d", i)
		}
		assert.Equal(t, DTSStats{}, d.Stats())
	}
}

func TestDTSJoinAtCRA(t *testing.T) {
	for _, name := range []string{"h265_aac51", "h264_opengop"} {
		t.Run(name, func(t *testing.T) {
			codec := H264
			if name == "h265_aac51" {
				codec = H265
			}
			refs := loadDTSRef(t, name)
			r, dur := fixtureTiming(t, name, codec)
			joins := 0
			for from := 1; from < len(refs); from++ {
				if !refs[from].key {
					continue
				}
				sub := refs[from:]
				leading := 0
				for i := 1; i < len(sub) && !sub[i].key; i++ {
					if sub[i].pts < sub[0].pts {
						leading++
					}
				}
				if leading > 0 {
					joins++
				}
				d := NewDTSExtractor(r, dur)
				out := extractAll(d, sub)
				assertValidDTS(t, sub, out)
				require.Equal(t, DTSOK, out[0].st, "join at %d", from)
				// The keyframe's reference DTS leaves room for the leading
				// pictures decoded after it; every later frame matches
				// the reference up to one constant.
				c, haveC := int64(0), false
				for i := 1; i < len(sub); i++ {
					o := out[i]
					if sub[i].pts < sub[0].pts {
						assert.Equal(t, DTSDrop, o.st, "join at %d, frame %d: leading picture", from, i)
						continue
					}
					require.Equal(t, DTSOK, o.st, "join at %d, frame %d", from, i)
					if !haveC {
						c, haveC = sub[i].dts-o.dts, true
					}
					assert.Equal(t, sub[i].dts-c, o.dts, "join at %d, frame %d", from, i)
				}
				assert.Equal(t, DTSStats{Leading: uint64(leading)}, d.Stats(), "join at %d", from)
			}
			assert.Positive(t, joins, "the fixture has keyframes with leading pictures")
		})
	}
}

func TestDTSAfterLoss(t *testing.T) {
	for _, fx := range dtsFixtures {
		t.Run(fx.name, func(t *testing.T) {
			refs := loadDTSRef(t, fx.name)
			r, dur := fixtureTiming(t, fx.name, fx.codec)
			// A B-frame is shown before a frame decoded earlier; a P-frame
			// is shown after every frame decoded before it.
			maxPTS := int64(-1 << 62)
			var bs, ps []int
			for i, f := range refs {
				if !f.key {
					if f.pts < maxPTS {
						bs = append(bs, i)
					} else {
						ps = append(ps, i)
					}
				}
				maxPTS = max(maxPTS, f.pts)
			}
			lostB, lostP := bs[len(bs)/5], ps[len(ps)/2]
			require.Less(t, lostB+10, lostP)

			var sub []dtsRef
			var orig []int
			for i, f := range refs {
				if i != lostB && i != lostP {
					sub = append(sub, f)
					orig = append(orig, i)
				}
			}
			d := NewDTSExtractor(r, dur)
			out := extractAll(d, sub)
			assertValidDTS(t, sub, out)
			assert.Equal(t, DTSStats{}, d.Stats(), "loss is neither a late frame nor a break")

			c := refs[0].dts - out[0].dts
			// The DTS of the k-th decoded frame is the PTS of display frame
			// k−R, so a lost frame shifts the DTS by one frame from its
			// decode slot until the heap reaches its display slot: R frames,
			// plus the frames decoded after it but shown before it.
			window := func(lost int) (from, to int) {
				ahead := 0
				for _, f := range refs[lost+1:] {
					if f.pts < refs[lost].pts {
						ahead++
					}
				}
				return lost, lost + r + ahead
			}
			bFrom, bTo := window(lostB)
			pFrom, pTo := window(lostP)
			for j, o := range out {
				require.Equal(t, DTSOK, o.st, "frame %d", orig[j])
				i := orig[j]
				if (i >= bFrom && i <= bTo) || (i >= pFrom && i <= pTo) {
					continue
				}
				assert.Equal(t, refs[i].dts-c, o.dts, "frame %d: not back to the reference", i)
			}
			t.Logf("%s: lost B %d (back after %d frames), lost P %d (back after %d frames), R=%d",
				fx.name, lostB, bTo-bFrom, lostP, pTo-pFrom, r)
		})
	}
}

func TestDTSReorderRaised(t *testing.T) {
	refs := loadDTSRef(t, "h264_pyramid")
	_, dur := fixtureTiming(t, "h264_pyramid", H264)
	d := NewDTSExtractor(0, dur)
	out := extractAll(d, refs)
	assertValidDTS(t, refs, out)
	st := d.Stats()
	assert.GreaterOrEqual(t, d.reorder, 2)
	assert.LessOrEqual(t, d.reorder, 4)
	assert.Equal(t, uint64(d.reorder), st.ReorderRaised)
	assert.Positive(t, st.Late)
	assert.LessOrEqual(t, st.Late, uint64(10))
	assert.Zero(t, st.Breaks)
	assert.Zero(t, st.Leading)
	lastDrop := 0
	for i, o := range out {
		if o.st == DTSDrop {
			lastDrop = i
		}
	}
	assert.Less(t, lastDrop, 25, "drops only while R is learned")
	// Once R has settled, the output matches the reference up to a constant.
	tail := len(refs) - 100
	c := refs[tail].dts - out[tail].dts
	for i := tail; i < len(refs); i++ {
		require.Equal(t, DTSOK, out[i].st, "frame %d", i)
		assert.Equal(t, refs[i].dts-c, out[i].dts, "frame %d", i)
	}
}

func TestDTSBackwardJump(t *testing.T) {
	const jump = 5 * 90000
	refs := loadDTSRef(t, "h264_aac")
	r, dur := fixtureTiming(t, "h264_aac", H264)
	at := 60
	require.False(t, refs[at].key)
	nextKey := at
	for !refs[nextKey].key {
		nextKey++
	}
	shifted := append([]dtsRef(nil), refs...)
	for i := at; i < len(shifted); i++ {
		shifted[i].pts -= jump
		shifted[i].dts -= jump
	}
	d := NewDTSExtractor(r, dur)
	out := extractAll(d, shifted)
	assertValidDTS(t, shifted[:at], out[:at])
	assertValidDTS(t, shifted[nextKey:], out[nextKey:])
	breaks := 0
	for i, o := range out {
		switch o.st {
		case DTSBreak:
			breaks++
			assert.Equal(t, at, i)
		case DTSDrop:
			assert.True(t, i >= at && i < nextKey, "frame %d dropped", i)
		case DTSOK:
			assert.False(t, i > at && i < nextKey, "frame %d before the next keyframe", i)
		}
	}
	assert.Equal(t, 1, breaks)
	c := shifted[nextKey].dts - out[nextKey].dts
	for i := nextKey; i < len(shifted); i++ {
		assert.Equal(t, shifted[i].dts-c, out[i].dts, "frame %d", i)
	}
	st := d.Stats()
	assert.Equal(t, uint64(1), st.Breaks)
	assert.Equal(t, uint64(nextKey-at), st.Leading)
	assert.Zero(t, st.Late)
	assert.Zero(t, st.ReorderRaised)
	assert.Equal(t, r, d.reorder, "R unchanged")
}

func TestDTSClampAndDuplicates(t *testing.T) {
	assert.Equal(t, 0, NewDTSExtractor(-1, 0).reorder)
	assert.Equal(t, 16, NewDTSExtractor(99, 0).reorder)

	// Without a frame duration the start window uses 3000.
	d := NewDTSExtractor(2, 0)
	dts, st := d.Extract(90000, true)
	require.Equal(t, DTSOK, st)
	assert.Equal(t, int64(90000-2*3000), dts)

	// A PTS equal to the last DTS cannot get a larger DTS: it is late.
	d = NewDTSExtractor(0, 3600)
	_, st = d.Extract(1000, true)
	require.Equal(t, DTSOK, st)
	_, st = d.Extract(1000, false)
	assert.Equal(t, DTSDrop, st)
	assert.Equal(t, DTSStats{Late: 1, ReorderRaised: 1}, d.Stats())
}

func TestDTSLateRaisesOncePerEpisode(t *testing.T) {
	d := NewDTSExtractor(0, 3600)
	for _, pts := range []int64{10000, 20000, 30000} {
		_, st := d.Extract(pts, pts == 10000)
		require.Equal(t, DTSOK, st)
	}
	for _, pts := range []int64{25000, 26000, 27000} {
		_, st := d.Extract(pts, false)
		assert.Equal(t, DTSDrop, st)
	}
	assert.Equal(t, DTSStats{Late: 3, ReorderRaised: 1}, d.Stats())
	assert.Equal(t, 1, d.reorder)
	// With the heap below R after the raise, the DTS is lastDTS+1.
	dts, st := d.Extract(40000, false)
	require.Equal(t, DTSOK, st)
	assert.Equal(t, int64(30001), dts)
	dts, st = d.Extract(50000, false)
	require.Equal(t, DTSOK, st)
	assert.Equal(t, int64(40000), dts)
	// A DTS was emitted, so the next late frame raises again.
	_, st = d.Extract(35000, false)
	assert.Equal(t, DTSDrop, st)
	assert.Equal(t, 2, d.reorder)

	d = NewDTSExtractor(16, 3600)
	d.Extract(100000, true)
	d.Extract(10000, false)
	assert.Equal(t, 16, d.reorder, "R never exceeds 16")
	assert.Equal(t, DTSStats{Leading: 1}, d.Stats(), "below the first keyframe is leading, not late")
}

func TestDTSSetReorder(t *testing.T) {
	d := NewDTSExtractor(0, 3600)
	pts := int64(0)
	next := func(key bool) (int64, DTSStatus) {
		pts += 3600
		return d.Extract(pts, key)
	}
	dts, st := next(true)
	require.Equal(t, DTSOK, st)
	require.Equal(t, pts, dts)

	// A higher R applies at once: the heap fills with DTS = lastDTS+1.
	d.SetReorder(2)
	assert.Equal(t, 2, d.reorder)
	last := dts
	for i := range 2 {
		dts, st = next(false)
		require.Equal(t, DTSOK, st)
		assert.Equal(t, last+1, dts, "fill %d", i)
		last = dts
	}
	dts, st = next(false)
	require.Equal(t, DTSOK, st)
	assert.Equal(t, pts-2*3600, dts)

	// A lower R waits for the next Reset.
	d.SetReorder(1)
	assert.Equal(t, 2, d.reorder)
	d.SetReorder(-5)
	assert.Equal(t, 2, d.reorder)
	d.Reset()
	assert.Equal(t, 0, d.reorder)

	// A raise cancels a pending lower value.
	d.SetReorder(3)
	d.SetReorder(1)
	d.SetReorder(4)
	d.Reset()
	assert.Equal(t, 4, d.reorder)
	d.SetReorder(4)
	d.Reset()
	assert.Equal(t, 4, d.reorder)

	// A DTSBreak resets the extractor, so it applies a pending lower R too.
	d.Extract(10_000_000, true)
	d.SetReorder(1)
	_, st = d.Extract(1000, true)
	require.Equal(t, DTSBreak, st)
	assert.Equal(t, 1, d.reorder)
}

func TestDTSReset(t *testing.T) {
	d := NewDTSExtractor(1, 3600)
	for i, f := range []dtsRef{{pts: 900000, key: true}, {pts: 907200}, {pts: 903600}} {
		_, st := d.Extract(f.pts, f.key)
		require.Equal(t, DTSOK, st, "frame %d", i)
	}
	d.Reset()
	// The new timeline starts lower, with no bound from the old one and no
	// break: frames before its first keyframe are dropped.
	_, st := d.Extract(3600, false)
	assert.Equal(t, DTSDrop, st)
	dts, st := d.Extract(7200, true)
	require.Equal(t, DTSOK, st)
	assert.Equal(t, int64(3600), dts)
	_, st = d.Extract(3600, false)
	assert.Equal(t, DTSDrop, st, "below the first keyframe after Reset")
	dts, st = d.Extract(14400, false)
	require.Equal(t, DTSOK, st)
	assert.Equal(t, int64(7200), dts)
	assert.Equal(t, DTSStats{Leading: 2}, d.Stats())
}
