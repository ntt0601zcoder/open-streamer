package ingest

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/h26x"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

var (
	rtpT0  = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	rtpNTP = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC) // the sender's wall clock
)

// rtpSrc is one track of a synthetic RTSP source: source frames in decode
// order and the RTP timestamp base their PTS map onto.
type rtpSrc struct {
	m      rtsp.Media
	frames []media.Frame
	base   uint32 // RTP timestamp of the source PTS origin
	origin int64
	ssrc   uint32
}

// rtpTS is the RTP timestamp of a source PTS.
func (s *rtpSrc) rtpTS(pts int64) uint32 {
	return s.base + uint32(media.Rescale(pts-s.origin, int(s.m.ClockRate)))
}

// sr is a sender report for source instant pts: the sender's NTP time of
// that instant plus skew, and its RTP timestamp on this track.
func (s *rtpSrc) sr(pts int64, skew time.Duration) rtsp.SenderReport {
	return rtsp.SenderReport{SSRC: s.ssrc, NTP: rtpNTP.Add(media.ToDuration(pts) + skew), RTP: s.rtpTS(pts)}
}

func rtpAnnexB(nalus [][]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// rtpAUs packetises the sources in arrival order and depacketises each
// track with rtsp.NewDepacketizer, returning the AUs in the order they
// complete.
func rtpAUs(t *testing.T, srcs []*rtpSrc, arrival func(media.Frame) time.Time) []rtsp.AU {
	t.Helper()
	type item struct {
		src int
		f   media.Frame
		at  time.Time
	}
	var items []item
	for i, s := range srcs {
		for _, f := range s.frames {
			items = append(items, item{i, f, arrival(f)})
		}
	}
	slices.SortStableFunc(items, func(a, b item) int { return a.at.Compare(b.at) })
	deps := make([]rtsp.Depacketizer, len(srcs))
	seqs := make([]uint16, len(srcs))
	h264 := make([]codecs.H264Payloader, len(srcs))
	h265 := make([]codecs.H265Payloader, len(srcs))
	for i, s := range srcs {
		d, err := rtsp.NewDepacketizer(s.m, 0)
		require.NoError(t, err)
		deps[i] = d
		seqs[i] = uint16(s.ssrc)
	}
	var out []rtsp.AU
	for _, it := range items {
		s := srcs[it.src]
		ts := s.rtpTS(it.f.PTS)
		var pkts []*rtp.Packet
		switch s.m.Codec {
		case rtsp.CodecH264, rtsp.CodecH265:
			var pls [][]byte
			if s.m.Codec == rtsp.CodecH264 {
				pls = h264[it.src].Payload(1200, rtpAnnexB(it.f.Data))
			} else {
				pls = h265[it.src].Payload(1200, rtpAnnexB(it.f.Data))
			}
			for j, pl := range pls {
				pkts = append(pkts, &rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: ts, Marker: j == len(pls)-1}, Payload: pl})
			}
		case rtsp.CodecAAC:
			spa := uint32(1024)
			if c, err := media.NewAACConfig(s.m.AAC.Config); err == nil {
				spa = uint32(1024 * int(s.m.ClockRate) / c.SampleRate)
			}
			pkts = rtsp.PacketizeAAC(it.f.Data, ts, spa, 1400, *s.m.AAC)
		case rtsp.CodecOther:
			t.Fatalf("unsupported source codec")
		}
		for _, p := range pkts {
			p.SequenceNumber = seqs[it.src]
			seqs[it.src]++
			p.PayloadType = s.m.Payload
			p.SSRC = s.ssrc
			out = append(out, deps[it.src].Push(p, it.at)...)
		}
	}
	for _, d := range deps {
		out = append(out, d.Flush()...)
	}
	return out
}

// rtpFixture returns a video source (track 0) and, when the fixture has
// audio and withAudio is set, an audio source (track 1). The video carries
// its parameter sets in the SDP, or in band before every keyframe.
func rtpFixture(t *testing.T, name string, inband, withAudio bool, rng *rand.Rand) (srcs []*rtpSrc, desc rtsp.Description) {
	t.Helper()
	var v, a *rtpSrc
	for _, f := range tsFrames(t, name) {
		switch f.Kind() {
		case media.KindVideo:
			if v == nil {
				c := rtsp.CodecH264
				if f.Config.Codec == media.CodecH265 {
					c = rtsp.CodecH265
				}
				v = &rtpSrc{m: rtsp.Media{Index: 0, Codec: c, Payload: 96, ClockRate: 90000}, origin: f.PTS, ssrc: 0x1000}
				if !inband {
					v.m.ParamSets = slices.Concat(f.Config.VPS, f.Config.SPS, f.Config.PPS)
				}
			}
			if inband && f.Key {
				f.Data = slices.Concat(f.Config.VPS, f.Config.SPS, f.Config.PPS, f.Data)
			}
			v.frames = append(v.frames, f)
		case media.KindAudio:
			if !withAudio {
				continue
			}
			if a == nil {
				a = &rtpSrc{m: rtsp.Media{
					Index: 1, Codec: rtsp.CodecAAC, Payload: 97, ClockRate: uint32(f.Config.SampleRate),
					AAC: &rtsp.AACFormat{Config: f.Config.ASC, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3},
				}, origin: f.PTS, ssrc: 0x2000}
			}
			a.frames = append(a.frames, f)
		}
	}
	require.NotNil(t, v)
	srcs = []*rtpSrc{v}
	if a != nil {
		srcs = append(srcs, a)
	}
	for _, s := range srcs {
		s.base = rng.Uint32()
		desc.Media = append(desc.Media, s.m)
	}
	return srcs, desc
}

// rtpInfo returns RTP-Info entries that all mark source instant pts.
func rtpInfo(srcs []*rtpSrc, pts int64) []rtsp.RTPInfo {
	out := make([]rtsp.RTPInfo, len(srcs))
	for i, s := range srcs {
		out[i] = rtsp.RTPInfo{RTPTime: s.rtpTS(pts), HasTime: true}
	}
	return out
}

// srcArrival stamps a source frame at its decode time after rtpT0.
func srcArrival(f media.Frame) time.Time { return rtpT0.Add(media.ToDuration(f.DTS)) }

// ptsArrival stamps a source frame at its presentation time after rtpT0.
func ptsArrival(f media.Frame) time.Time { return rtpT0.Add(media.ToDuration(f.PTS)) }

// rtpRun pushes AUs and returns the frames and the indexes in out at which a
// discontinuity was reported (the frame at that index is the first after it).
func rtpRun(t *testing.T, r *rtpFrames, aus []rtsp.AU) (out []Frame, breaks []int) {
	t.Helper()
	for i, au := range aus {
		fs, disc, err := r.push(au)
		require.NoError(t, err, "au %d", i)
		if disc {
			breaks = append(breaks, len(out))
		}
		out = append(out, fs...)
	}
	return out, breaks
}

// rtpClock runs frames through a Clock, breaking it at each break index, and
// checks the frame rules on its output.
func rtpClock(t *testing.T, kinds []media.Kind, frames []Frame, breaks []int) clock.Stats {
	t.Helper()
	c := clock.New(clock.Options{Kinds: kinds})
	out := make([]media.Frame, 0, len(frames))
	for i, f := range frames {
		if slices.Contains(breaks, i) {
			c.Break(false)
		}
		out = append(out, c.PushAt(f.Frame, f.Arrival)...)
	}
	out = append(out, c.Flush()...)
	require.NotEmpty(t, out)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
	return c.Stats()
}

func TestRTPFramesFromTS(t *testing.T) {
	for i, name := range []string{"h264_aac.ts", "h264_pyramid.ts", "h264_aacmono_wrap.ts"} {
		t.Run(name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(i), 11))
			srcs, desc := rtpFixture(t, name, false, true, rng)
			if i == 0 {
				srcs[0].base = 0xFFFFFFFF - 90000 // wraps after 1 s
			}
			require.Len(t, srcs, 2)
			aus := rtpAUs(t, srcs, srcArrival)
			r := newRTPFrames(desc, rtpInfo(srcs, srcs[0].origin), &media.ConfigCache{})
			assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.kinds())
			out, breaks := rtpRun(t, r, aus)
			require.Empty(t, breaks)

			srcV, srcA := srcs[0].frames, srcs[1].frames
			gotV, gotA := byTrack(out, 0), byTrack(out, 1)
			require.Len(t, gotV, len(srcV))
			require.Len(t, gotA, len(srcA))
			shift := gotV[0].PTS - srcV[0].PTS
			for k, g := range gotV {
				s := srcV[k]
				require.Equal(t, s.PTS+shift, g.PTS, "video %d", k)
				require.Equal(t, s.Data, g.Data, "video %d", k)
				require.Equal(t, s.Key, g.Key, "video %d", k)
				require.LessOrEqual(t, g.DTS, g.PTS, "video %d", k)
				require.True(t, g.Config.Equal(s.Config), "video %d", k)
				require.Equal(t, rtpT0, g.Arrival.Add(-media.ToDuration(s.DTS)), "video %d", k)
				if k > 0 {
					require.Greater(t, g.DTS, gotV[k-1].DTS, "video %d", k)
					require.Same(t, gotV[0].Config, g.Config)
				}
			}
			for k, g := range gotA {
				s := srcA[k]
				require.InDelta(t, s.PTS+shift, g.PTS, 1, "audio %d", k)
				require.Equal(t, g.PTS, g.DTS)
				require.True(t, g.Key)
				require.Equal(t, s.Data, g.Data)
				require.True(t, g.Config.Equal(s.Config))
			}
			st := r.stats()
			assert.Equal(t, uint64(len(out)), st["frames"])
			assert.Zero(t, st["sync_by_arrival"])
			assert.Zero(t, st["lost_aus"])
			assert.Zero(t, st["dts_late"])
			assert.Zero(t, st["dts_leading"])
			assert.Zero(t, st["reorder_assumed"])

			cs := rtpClock(t, r.kinds(), out, breaks)
			assert.Zero(t, cs.DroppedInvalid)
		})
	}
}

func TestRTPFramesH265OpenGOP(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	srcs, desc := rtpFixture(t, "h265_aac51.ts", false, false, rng)
	v := srcs[0]
	// Join at the second keyframe: a CRA followed in decode order by its
	// leading (RASL) pictures.
	var keys []int
	for k, f := range v.frames {
		if f.Key {
			keys = append(keys, k)
		}
	}
	require.Greater(t, len(keys), 2)
	join := v.frames[keys[1]]
	var cra bool
	for _, n := range join.Data {
		cra = cra || (n[0]>>1)&0x3f == 21
	}
	require.True(t, cra, "the second keyframe is a CRA")
	v.frames = v.frames[keys[1]:]
	leading := 0
	for _, f := range v.frames[1 : keys[2]-keys[1]] {
		if f.PTS < join.PTS {
			leading++
		}
	}
	require.Positive(t, leading)

	aus := rtpAUs(t, srcs, srcArrival)
	r := newRTPFrames(desc, nil, &media.ConfigCache{})
	out, breaks := rtpRun(t, r, aus)
	require.Empty(t, breaks)
	require.Len(t, out, len(v.frames)-leading)
	require.True(t, out[0].Key)
	shift := out[0].PTS - join.PTS
	srcByPTS := map[int64]media.Frame{}
	for _, f := range v.frames {
		srcByPTS[f.PTS] = f
	}
	rasl := 0
	for k, g := range out {
		s, ok := srcByPTS[g.PTS-shift]
		require.True(t, ok, "frame %d", k)
		require.GreaterOrEqual(t, s.PTS, join.PTS, "frame %d is a leading picture of the join", k)
		require.Equal(t, s.RASL, g.RASL, "frame %d", k)
		require.Equal(t, s.Key, g.Key, "frame %d", k)
		if g.RASL {
			rasl++
		}
	}
	assert.Positive(t, rasl, "later CRAs keep their RASL pictures, marked")
	st := r.stats()
	assert.Equal(t, uint64(leading), st["dts_leading"])
	assert.Equal(t, uint64(1), st["sync_by_arrival"])
	rtpClock(t, r.kinds(), out, breaks)
}

// shiftOf returns each frame's PTS minus its source frame's, matched in order.
func shiftOf(got []Frame, src []media.Frame) []int64 {
	out := make([]int64, len(got))
	for i := range got {
		out[i] = got[i].PTS - src[i].PTS
	}
	return out
}

// heAACSource replaces the audio of srcs with HE-AAC sent at the SBR clock:
// 24 kHz core (implicit SBR) and a 48 kHz RTP clock, so each frame lasts 2048
// RTP ticks and 3840 90 kHz ticks.
func heAACSource(srcs []*rtpSrc, end int64) {
	a := srcs[1]
	a.m.ClockRate = 48000
	a.m.AAC = &rtsp.AACFormat{Config: []byte{0x13, 0x10}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3}
	cfg, err := media.NewAACConfig(a.m.AAC.Config)
	if err != nil {
		panic(err)
	}
	data := a.frames[0].Data
	a.frames = a.frames[:0]
	for pts := a.origin; pts < end; pts += 3840 {
		a.frames = append(a.frames, media.Frame{Track: 1, Config: cfg, PTS: pts, DTS: pts, Key: true, Data: data})
	}
}

func TestRTPFramesSync(t *testing.T) {
	type step struct {
		at   int // push AUs up to this index first
		srcs []int
		skew []time.Duration
		want bool
	}
	run := func(t *testing.T, srcs []*rtpSrc, desc rtsp.Description, prep func([]rtsp.AU) []rtsp.AU, steps []step) (*rtpFrames, []Frame, []int) {
		t.Helper()
		// Video is sent in decode order, at its DTS plus the first frame's
		// composition offset, so that placing a track by arrival at its
		// first AU is exact; audio at its PTS.
		lead := srcs[0].frames[0].PTS - srcs[0].frames[0].DTS
		aus := rtpAUs(t, srcs, func(f media.Frame) time.Time {
			if f.Kind() == media.KindVideo {
				return rtpT0.Add(media.ToDuration(f.DTS + lead))
			}
			return ptsArrival(f)
		})
		if prep != nil {
			aus = prep(aus)
		}
		r := newRTPFrames(desc, nil, &media.ConfigCache{})
		var (
			out    []Frame
			breaks []int
			next   int
		)
		mid := srcs[0].frames[len(srcs[0].frames)/4].PTS
		for _, s := range steps {
			got, b := rtpRun(t, r, aus[next:s.at])
			for _, x := range b {
				breaks = append(breaks, len(out)+x)
			}
			out = append(out, got...)
			next = s.at
			disc := false
			for j, si := range s.srcs {
				disc = r.senderReport(si, srcs[si].sr(mid, s.skew[j])) || disc
			}
			require.Equal(t, s.want, disc, "step at %d", s.at)
			if disc {
				breaks = append(breaks, len(out))
			}
		}
		got, b := rtpRun(t, r, aus[next:])
		for _, x := range b {
			breaks = append(breaks, len(out)+x)
		}
		return r, append(out, got...), breaks
	}
	ms := time.Millisecond
	tick := func(d time.Duration) int64 { return media.FromDuration(d) }

	t.Run("arrival then SR within 50 ms", func(t *testing.T) {
		srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rand.New(rand.NewPCG(5, 6)))
		r, out, breaks := run(t, srcs, desc, nil, []step{
			{at: 100, srcs: []int{0, 1}, skew: []time.Duration{0, 30 * ms}, want: false},
		})
		require.Empty(t, breaks)
		gotV, gotA := byTrack(out, 0), byTrack(out, 1)
		require.Len(t, gotV, len(srcs[0].frames))
		v := shiftOf(gotV, srcs[0].frames)[0]
		for k, s := range shiftOf(gotA, srcs[1].frames) {
			require.InDelta(t, v, s, 1, "audio %d aligned by arrival", k)
		}
		st := r.stats()
		assert.Equal(t, uint64(2), st["sync_by_arrival"])
		assert.Zero(t, st["sync_corrected"])
		assert.Zero(t, st["sync_jumps"])
	})

	t.Run("SR 200 ms off, drift, jump", func(t *testing.T) {
		srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rand.New(rand.NewPCG(7, 8)))
		// Audio SR first: it pairs when the reference's SR arrives.
		r, out, breaks := run(t, srcs, desc, nil, []step{
			{at: 60, srcs: []int{1}, skew: []time.Duration{200 * ms}, want: false},
			{at: 80, srcs: []int{0}, skew: []time.Duration{0}, want: true},
			{at: 120, srcs: []int{1, 0}, skew: []time.Duration{230 * ms, 0}, want: false},
			{at: 200, srcs: []int{1}, skew: []time.Duration{5200 * ms}, want: true},
			{at: 240, srcs: []int{1}, skew: []time.Duration{5170 * ms}, want: false},
		})
		require.Len(t, breaks, 2)
		gotV, gotA := byTrack(out, 0), byTrack(out, 1)
		require.Len(t, gotV, len(srcs[0].frames))
		v := shiftOf(gotV, srcs[0].frames)
		for _, s := range v {
			require.Equal(t, v[0], s, "video is the reference and never moves")
		}
		a := shiftOf(gotA, srcs[1].frames)
		// Audio frames before the first break are aligned by arrival, then
		// moved by 200 ms, then by 5 s more.
		idx := func(brk int) int {
			n := 0
			for _, f := range out[:brk] {
				if f.Track == 1 {
					n++
				}
			}
			return n
		}
		b1, b2 := idx(breaks[0]), idx(breaks[1])
		for k, s := range a {
			want := v[0]
			switch {
			case k >= b2:
				want += tick(5200 * ms)
			case k >= b1:
				want += tick(200 * ms)
			}
			require.InDelta(t, want, s, 2, "audio %d", k)
		}
		st := r.stats()
		assert.Equal(t, uint64(1), st["sync_corrected"])
		assert.Equal(t, uint64(1), st["sync_jumps"])
		rtpClock(t, r.kinds(), out, breaks)
	})

	t.Run("SRs before frames", func(t *testing.T) {
		srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rand.New(rand.NewPCG(9, 10)))
		// The SRs arrive before any frame (Open's SyncWait): the SR offset
		// is taken directly, without a discontinuity.
		r, out, breaks := run(t, srcs, desc, nil, []step{
			{at: 0, srcs: []int{0, 1}, skew: []time.Duration{0, 300 * ms}, want: false},
		})
		require.Empty(t, breaks)
		v := shiftOf(byTrack(out, 0), srcs[0].frames)[0]
		for k, s := range shiftOf(byTrack(out, 1), srcs[1].frames) {
			require.InDelta(t, v+tick(300*ms), s, 1, "audio %d", k)
		}
		st := r.stats()
		assert.Equal(t, uint64(1), st["sync_by_arrival"], "only the first track placed")
		assert.Zero(t, st["sync_corrected"])
	})

	for _, order := range [][]int{{1, 0}, {0, 1}} {
		t.Run(fmt.Sprintf("reference placed by SR, SR order %v", order), func(t *testing.T) {
			srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rand.New(rand.NewPCG(21, 22)))
			// Twenty audio AUs come before any video AU; then the SRs: the
			// reference takes its offset from the audio track's through the SR
			// pair, without being placed by arrival.
			front := func(aus []rtsp.AU) []rtsp.AU {
				var audio, rest []rtsp.AU
				for _, au := range aus {
					if au.Track == 1 && len(audio) < 20 {
						audio = append(audio, au)
					} else {
						rest = append(rest, au)
					}
				}
				return append(audio, rest...)
			}
			r, out, breaks := run(t, srcs, desc, front, []step{
				{at: 20, srcs: order, skew: []time.Duration{-250 * ms * time.Duration(order[0]), -250 * ms * time.Duration(order[1])}, want: false},
			})
			require.Empty(t, breaks)
			v := shiftOf(byTrack(out, 0), srcs[0].frames)[0]
			for k, s := range shiftOf(byTrack(out, 1), srcs[1].frames) {
				require.InDelta(t, v-tick(250*ms), s, 1, "audio %d", k)
			}
			st := r.stats()
			assert.Equal(t, uint64(1), st["sync_by_arrival"], "only the audio track")
			assert.Zero(t, st["sync_corrected"])
		})
	}

	t.Run("HE-AAC at the SBR clock", func(t *testing.T) {
		srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rand.New(rand.NewPCG(11, 12)))
		heAACSource(srcs, srcs[0].frames[len(srcs[0].frames)-1].PTS)
		desc.Media[1] = srcs[1].m
		r, out, breaks := run(t, srcs, desc, nil, []step{
			{at: 80, srcs: []int{0, 1}, skew: []time.Duration{0, -40 * ms}, want: false},
			{at: 160, srcs: []int{1}, skew: []time.Duration{-150 * ms}, want: true},
		})
		require.Len(t, breaks, 1)
		gotA := byTrack(out, 1)
		require.Len(t, gotA, len(srcs[1].frames))
		for k := 1; k < len(gotA); k++ {
			d := gotA[k].PTS - gotA[k-1].PTS
			if d != 3840 {
				require.Equal(t, int64(3840-tick(150*ms)), d, "audio %d: only at the jump", k)
			}
		}
		v := shiftOf(byTrack(out, 0), srcs[0].frames)[0]
		a := shiftOf(gotA, srcs[1].frames)
		require.InDelta(t, v, a[0], 1)
		require.InDelta(t, v-tick(150*ms), a[len(a)-1], 1)
		assert.Equal(t, uint64(1), r.stats()["sync_jumps"])
	})
}

func TestRTPFramesRestart(t *testing.T) {
	for _, viaCall := range []bool{true, false} {
		name := "restart call"
		if !viaCall {
			name = "timestamp jump"
		}
		t.Run(name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(13, 14))
			srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rng)
			cut := srcs[0].frames[len(srcs[0].frames)/2+3].DTS // mid-GOP
			split := func(s *rtpSrc, before bool) *rtpSrc {
				c := *s
				c.frames = nil
				for _, f := range s.frames {
					if (f.DTS < cut) == before {
						c.frames = append(c.frames, f)
					}
				}
				return &c
			}
			first := []*rtpSrc{split(srcs[0], true), split(srcs[1], true)}
			second := []*rtpSrc{split(srcs[0], false), split(srcs[1], false)}
			for i, s := range second {
				s.ssrc += 0x100
				if viaCall {
					s.base = rng.Uint32()
				} else {
					s.base = first[i].base + uint32(20*s.m.ClockRate) // 20 s ahead
				}
			}
			r := newRTPFrames(desc, rtpInfo(first, first[0].origin), &media.ConfigCache{})
			out, breaks := rtpRun(t, r, rtpAUs(t, first, srcArrival))
			require.Empty(t, breaks)
			n1 := len(out)
			if viaCall {
				assert.True(t, r.restart(0), "first track restart is a break")
				assert.False(t, r.restart(1), "nothing went out since the last break")
			}
			got, b := rtpRun(t, r, rtpAUs(t, second, srcArrival))
			if viaCall {
				require.Empty(t, b)
				breaks = []int{n1}
			} else {
				require.Equal(t, []int{0}, b, "one discontinuity")
				breaks = []int{n1}
			}
			out = append(out, got...)

			// DTS restarts at the next keyframe: the video frames before it
			// in the second half are dropped.
			v2 := second[0].frames
			skip := slices.IndexFunc(v2, func(f media.Frame) bool { return f.Key })
			require.Positive(t, skip)
			gotV2 := byTrack(got, 0)
			require.Len(t, gotV2, len(v2)-skip)
			require.True(t, gotV2[0].Key)
			for k := 1; k < len(gotV2); k++ {
				require.Greater(t, gotV2[k].DTS, gotV2[k-1].DTS)
			}
			// Re-aligned by arrival: the new timeline continues the old one.
			lastV1 := byTrack(out[:n1], 0)
			prev := lastV1[len(lastV1)-1]
			gap := media.FromDuration(gotV2[0].Arrival.Sub(prev.Arrival))
			assert.InDelta(t, prev.PTS+gap, gotV2[0].PTS, float64(media.FromDuration(200*time.Millisecond)))
			// Audio stays aligned with video across the restart.
			vs := shiftOf(gotV2, v2[skip:])[0]
			for k, s := range shiftOf(byTrack(got, 1), second[1].frames) {
				require.InDelta(t, vs, s, float64(media.FromDuration(200*time.Millisecond)), "audio %d", k)
			}
			st := r.stats()
			if viaCall {
				assert.Equal(t, uint64(2), st["restarts"])
			} else {
				assert.Equal(t, uint64(2), st["timeline_breaks"])
			}
			assert.Zero(t, st["dts_breaks"])
			rtpClock(t, r.kinds(), out, breaks)
		})
	}
}

func TestRTPFramesDTSBreak(t *testing.T) {
	srcs, desc := rtpFixture(t, "h264_aac.ts", false, false, rand.New(rand.NewPCG(23, 24)))
	v := srcs[0]
	// From mid-GOP on, the timestamps step 3 s back: past the extractor's
	// break bound, short of the 10 s timeline break.
	cut := len(v.frames)/2 + 3
	back := int64(3 * media.ClockRate)
	for k := cut; k < len(v.frames); k++ {
		v.frames[k].PTS -= back
	}
	r := newRTPFrames(desc, nil, &media.ConfigCache{})
	out, breaks := rtpRun(t, r, rtpAUs(t, srcs, srcArrival))
	require.Len(t, breaks, 1, "one discontinuity")
	after := out[breaks[0]:]
	require.True(t, after[0].Key, "the DTS restarts at the next keyframe")
	skip := slices.IndexFunc(v.frames[cut:], func(f media.Frame) bool { return f.Key })
	require.Positive(t, skip)
	require.Len(t, after, len(v.frames)-cut-skip)
	st := r.stats()
	assert.Equal(t, uint64(1), st["dts_breaks"])
	assert.Zero(t, st["timeline_breaks"])
	rtpClock(t, r.kinds(), out, breaks)
}

func TestRTPFramesInbandConfig(t *testing.T) {
	rng := rand.New(rand.NewPCG(15, 16))
	srcs, desc := rtpFixture(t, "h264_aac.ts", true, false, rng)
	more, _ := rtpFixture(t, "h264_640.ts", true, false, rng)
	v := srcs[0]
	last := v.frames[len(v.frames)-1]
	lastPTS := slices.MaxFunc(v.frames, func(a, b media.Frame) int { return int(a.PTS - b.PTS) }).PTS
	off := lastPTS + 3600 - more[0].frames[0].PTS
	for _, f := range more[0].frames {
		f.PTS += off
		f.DTS = f.DTS - more[0].frames[0].DTS + last.DTS + 3600
		v.frames = append(v.frames, f)
	}
	require.Empty(t, desc.Media[0].ParamSets)

	r := newRTPFrames(desc, nil, &media.ConfigCache{})
	out, breaks := rtpRun(t, r, rtpAUs(t, srcs, srcArrival))
	require.Empty(t, breaks)
	require.Len(t, out, len(v.frames))
	n1 := len(v.frames) - len(more[0].frames)
	for k, g := range out {
		require.NotNil(t, g.Config)
		require.Empty(t, slices.DeleteFunc(slices.Clone(g.Data), func(n []byte) bool { typ := n[0] & 0x1f; return typ != 7 && typ != 8 }),
			"frame %d: parameter sets live in the Config", k)
		switch {
		case k == n1:
			require.NotSame(t, out[k-1].Config, g.Config, "resolution change")
			require.Equal(t, 640, g.Config.Width)
		case k > 0:
			require.Same(t, out[k-1].Config, g.Config, "frame %d", k)
		}
	}
	assert.Equal(t, 320, out[0].Config.Width)
	rtpClock(t, r.kinds(), out, breaks)

	t.Run("bad parameter set invalidates and recovers", func(t *testing.T) {
		srcs, desc := rtpFixture(t, "h264_aac.ts", true, false, rand.New(rand.NewPCG(17, 18)))
		aus := rtpAUs(t, srcs, srcArrival)
		r := newRTPFrames(desc, nil, &media.ConfigCache{})
		var keys []int
		for k, au := range aus {
			if au.NALUs[0][0]&0x1f == 7 {
				keys = append(keys, k)
			}
		}
		require.Greater(t, len(keys), 2)
		// The second GOP's SPS is broken.
		aus[keys[1]].NALUs[0] = []byte{0x67, 0x42}
		var out []Frame
		for k, au := range aus {
			fs, disc, err := r.push(au)
			require.False(t, disc)
			if k == keys[1] {
				require.Error(t, err)
				require.ErrorContains(t, err, "ingest: rtp")
			} else {
				require.NoError(t, err, "au %d", k)
			}
			out = append(out, fs...)
		}
		gop := keys[2] - keys[1]
		require.Len(t, out, len(aus)-gop)
		st := r.stats()
		assert.Equal(t, uint64(1), st["bad_aus"])
		assert.Equal(t, uint64(gop-1), st["dropped_no_config"])
		require.Same(t, out[0].Config, out[len(out)-1].Config, "the recovered config is the cached pointer")
	})
}

func TestRTPFramesLostAU(t *testing.T) {
	srcs, desc := rtpFixture(t, "h264_aac.ts", false, true, rand.New(rand.NewPCG(19, 20)))
	aus := rtpAUs(t, srcs, srcArrival)
	r := newRTPFrames(desc, rtpInfo(srcs, srcs[0].origin), &media.ConfigCache{})
	lost := 0
	for k := range aus {
		if k%37 == 20 {
			aus[k].Lost = true
			lost++
		}
	}
	out, breaks := rtpRun(t, r, aus)
	require.Empty(t, breaks)
	assert.Len(t, out, len(aus)-lost, "frames after a loss pass through: no reset, no wait for a keyframe")
	st := r.stats()
	assert.Equal(t, uint64(lost), st["lost_aus"])
	assert.Zero(t, st["dts_breaks"])
	assert.Zero(t, st["dts_leading"])
	gotV := byTrack(out, 0)
	for k := 1; k < len(gotV); k++ {
		require.Greater(t, gotV[k].DTS, gotV[k-1].DTS)
		require.LessOrEqual(t, gotV[k].DTS, gotV[k].PTS)
	}
	rtpClock(t, r.kinds(), out, breaks)
}

func TestRTPFramesUnsupportedTrack(t *testing.T) {
	desc := rtsp.Description{Media: []rtsp.Media{
		{Index: 0, Codec: rtsp.CodecOther, Reason: "codec PCMA"},
		{Index: 1, Codec: rtsp.CodecH264, Payload: 96, ClockRate: 8000},
		{Index: 2, Codec: rtsp.CodecAAC, Payload: 97, ClockRate: 48000, AAC: &rtsp.AACFormat{Config: asc48k, SizeLength: 13}},
	}}
	r := newRTPFrames(desc, nil, &media.ConfigCache{})
	assert.Equal(t, []media.Kind{0, 0, media.KindAudio}, r.kinds())
	fs, disc, err := r.push(rtsp.AU{Track: 0, Frames: [][]byte{{1}}, Arrival: rtpT0})
	assert.Empty(t, fs)
	assert.False(t, disc)
	assert.NoError(t, err)
	assert.False(t, r.senderReport(7, rtsp.SenderReport{}))
	assert.False(t, r.restart(7))
	fs, _, err = r.push(rtsp.AU{Track: 2, Timestamp: 1000, Frames: [][]byte{{1, 2}}, Arrival: rtpT0})
	require.NoError(t, err)
	require.Len(t, fs, 1)
	assert.Equal(t, 2, fs[0].Track)
	assert.Equal(t, uint64(1), r.stats()["unknown_track"])
}

func TestRTPFramesFrameDurationChange(t *testing.T) {
	_, desc := rtpFixture(t, "h264_aac.ts", false, false, rand.New(rand.NewPCG(25, 26)))
	r := newRTPFrames(desc, nil, &media.ConfigCache{})
	v := r.tracks[0].v
	require.NotNil(t, v.dts)
	require.Equal(t, int64(3600), v.frameDur, "25 fps")

	// Before the extractor emits, a new frame duration replaces it at once.
	first := v.dts
	r.applySPS(v, &h26x.SPS{ReorderFrames: 2, FrameDuration: 1800})
	require.NotSame(t, first, v.dts)
	assert.False(t, v.recreate)

	// After it emitted, the replacement waits for the next reset.
	_, st := v.dts.Extract(0, true)
	require.Equal(t, h26x.DTSOK, st)
	v.emitted = true
	cur := v.dts
	r.applySPS(v, &h26x.SPS{ReorderFrames: 3, FrameDuration: 3600})
	require.Same(t, cur, v.dts)
	assert.True(t, v.recreate)
	assert.Equal(t, 3, v.reorder)
	r.resetDTS(v)
	require.NotSame(t, cur, v.dts)
	assert.False(t, v.recreate)
	assert.False(t, v.emitted)
	assert.Equal(t, int64(3600), v.frameDur)

	// An SPS without reorder information counts as assumed.
	r.applySPS(v, &h26x.SPS{ReorderFrames: -1, ProfileIDC: 100, FrameDuration: 3600})
	assert.Equal(t, uint64(1), r.stats()["reorder_assumed"])
	assert.Equal(t, 2, v.reorder)
}
