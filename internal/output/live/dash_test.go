package live

import (
	"cmp"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
)

func mpdOf(t *testing.T, srv http.Handler) dash.MPD {
	t.Helper()
	code, body := get(t, srv, "/live/s/cmaf/index.mpd")
	require.Equal(t, http.StatusOK, code)
	m, err := dash.Parse(body)
	require.NoError(t, err, string(body))
	return m
}

func TestMPD(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	srv := serve(p)
	snap := p.Snapshot()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/live/s/cmaf/index.mpd", nil))
	assert.Equal(t, "application/dash+xml", rec.Header().Get("Content-Type"))

	m := mpdOf(t, srv)
	assert.Equal(t, "dynamic", m.Type)
	assert.Equal(t, dash.ProfileLive, m.Profiles)
	require.NotNil(t, m.AvailabilityStartTime)
	assert.True(t, m.AvailabilityStartTime.Equal(snap.AST))
	assert.True(t, m.PublishTime.Equal(snap.Now))
	assert.Equal(t, dash.Duration(2*time.Second), m.MinimumUpdatePeriod)
	assert.Equal(t, dash.Duration(200*time.Second), m.TimeShiftBufferDepth)
	assert.Equal(t, dash.Duration(6*time.Second), m.SuggestedPresentationDelay)
	assert.Equal(t, dash.Duration(2*time.Second), m.MinBufferTime)
	require.NotNil(t, m.UTCTiming)
	assert.Equal(t, dash.UTCTimingDirect, m.UTCTiming.SchemeIDURI)
	now, err := time.Parse(time.RFC3339Nano, m.UTCTiming.Value)
	require.NoError(t, err)
	assert.True(t, now.Equal(snap.Now))

	require.Len(t, m.Periods, 1)
	per := m.Periods[0]
	assert.Equal(t, "e1", per.ID)
	assert.Zero(t, per.Start)
	require.Len(t, per.AdaptationSets, 2)
	for i, as := range per.AdaptationSets {
		et := snap.Epochs[0].Tracks[i]
		require.Len(t, as.Representations, 1)
		rep := as.Representations[0]
		assert.Equal(t, strconv.Itoa(et.Track), rep.ID)
		assert.True(t, as.SegmentAlignment)
		wantCodec, _ := media.CodecString(et.Config)
		assert.Equal(t, wantCodec, rep.Codecs)
		assert.GreaterOrEqual(t, rep.Bandwidth, 1)
		st := as.SegmentTemplate
		require.NotNil(t, st)
		ts := timescaleOf(et.Config)
		assert.Equal(t, ts, st.Timescale)
		assert.Equal(t, strconv.Itoa(et.Track)+"-$Number$.m4s", st.Media)
		assert.Equal(t, "init-"+strconv.Itoa(et.Track)+"-1.mp4", st.Initialization)
		assert.EqualValues(t, media.Rescale(snap.Epochs[0].Anchor.Start, int(ts)), st.PresentationTimeOffset)
		require.NotNil(t, st.StartNumber)
		assert.EqualValues(t, 0, *st.StartNumber)
		require.Len(t, st.Timeline.S, 3)
		for j, s := range st.Timeline.S {
			part := snap.Segments[j].Parts[et.Track]
			require.NotNil(t, s.T)
			assert.Equal(t, part.Timing.Earliest, *s.T)
			assert.Equal(t, part.Timing.Duration, s.D)
		}
		if et.Track == 0 {
			assert.Equal(t, "video", as.ContentType)
			assert.Equal(t, "video/mp4", as.MimeType)
			assert.Equal(t, et.Config.Width, rep.Width)
			assert.Equal(t, et.Config.Height, rep.Height)
			assert.Equal(t, 320, rep.Width)
		} else {
			assert.Equal(t, "audio", as.ContentType)
			assert.Equal(t, "audio/mp4", as.MimeType)
			assert.Equal(t, et.Config.SampleRate, rep.AudioSamplingRate)
			require.NotNil(t, rep.AudioChannelConfiguration)
			assert.Equal(t, dash.AudioChannelScheme, rep.AudioChannelConfiguration.SchemeIDURI)
			assert.Equal(t, strconv.Itoa(et.Config.Channels), rep.AudioChannelConfiguration.Value)
		}
	}
}

func TestMPDUnlistedAndEmpty(t *testing.T) {
	clk := &fakeNow{t: farFuture()}
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: clk.Now})
	srv := serve(p)
	snap := p.Snapshot()
	clk.Set(snap.Epochs[0].WallAt(snap.Segments[0].End))
	m := mpdOf(t, srv)
	assert.Len(t, m.Periods[0].AdaptationSets[0].SegmentTemplate.Timeline.S, 1)
	clk.Set(t0.Add(-time.Second))
	code, _ := get(t, srv, "/live/s/cmaf/index.mpd")
	assert.Equal(t, http.StatusNotFound, code)
	body, _ := buildMPD(Snapshot{})
	assert.Nil(t, body)
	// Listed but nothing usable: no init for any track.
	s := p.Snapshot()
	for i := range s.Epochs {
		s.Epochs[i].Tracks = []EpochTrack{{Track: 0, Config: s.Epochs[i].Tracks[0].Config}}
	}
	body, _ = buildMPD(s)
	assert.Nil(t, body)
}

func TestMPDBandwidth(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	snap := p.Snapshot()
	m := mpdOf(t, serve(p))
	for _, as := range m.Periods[0].AdaptationSets {
		track := as.Representations[0].ID
		tr, _ := strconv.Atoi(track)
		et, _ := snap.Epochs[0].Track(tr)
		peak := 1.0
		for _, seg := range snap.Segments {
			for _, part := range seg.Parts {
				if part.Track == tr {
					peak = max(peak, float64(len(part.Data))*8*float64(timescaleOf(et.Config))/float64(part.Timing.Duration))
				}
			}
		}
		assert.InDelta(t, peak, float64(as.Representations[0].Bandwidth), 1)
	}
}

// synthAV is synth plus an audio track 1 (48 kHz AAC, 1024-sample frames)
// covering the same span of stream time.
func synthAV(t *testing.T, epoch uint32, start int64) []hub.Delivery {
	t.Helper()
	aac, err := media.NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	out := synth(h264Config(t), epoch, start, 100)
	for ts := start; ts < start+100*3600; ts += 1920 {
		out = append(out, hub.Delivery{Frame: media.Frame{
			Track: 1, Config: aac, PTS: ts, DTS: ts, Key: true, Data: [][]byte{{0x21, 0x00}}, Epoch: epoch,
		}})
	}
	slices.SortStableFunc(out, func(a, b hub.Delivery) int { return cmp.Compare(a.Frame.DTS, b.Frame.DTS) })
	return out
}

// mediaEnd returns the end of a timeline entry as an offset from the start of
// its period, rounded up.
func mediaEnd(tpl *dash.SegmentTemplate, sg dash.Seg) time.Duration {
	ticks := sg.T + sg.D - tpl.PresentationTimeOffset
	ts := uint64(tpl.Timescale)
	return time.Duration((ticks*uint64(time.Second) + ts - 1) / ts)
}

func TestMPDPeriodAfterPause(t *testing.T) {
	h := hub.New(hub.Options{})
	clk := &fakeNow{t: t0.Add(time.Hour)}
	p := NewPackager(h, Options{Window: 10, Now: clk.Now})
	srv := serve(p)

	// Epoch 2 continues stream time after a 10 s wallclock pause.
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	handleAll(p, synthAV(t, 1, 0))
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(14 * time.Second)})
	handleAll(p, synthAV(t, 2, 360000))
	p.Flush()

	// Now sits exactly at the end of the first segment of epoch 2.
	snap := p.Snapshot()
	var seg2 SegmentInfo
	for _, sg := range snap.Segments {
		if sg.Epoch == 2 {
			seg2 = sg
			break
		}
	}
	e2info, _ := snap.Epoch(2)
	clk.Set(e2info.WallAt(seg2.End))
	snap = p.Snapshot()
	require.Less(t, snap.Listed, len(snap.Segments), "later segments are not listed yet")

	m := mpdOf(t, srv)
	require.Len(t, m.Periods, 2)
	e2 := m.Periods[1]
	assert.Equal(t, "e2", e2.ID)
	assert.Equal(t, dash.Duration(14*time.Second), e2.Start)
	require.Len(t, e2.AdaptationSets, 2)
	assert.EqualValues(t, 360000, e2.AdaptationSets[0].SegmentTemplate.PresentationTimeOffset)
	assert.EqualValues(t, 192000, e2.AdaptationSets[1].SegmentTemplate.PresentationTimeOffset)

	oneSample := time.Second/48000 + 1
	for i, per := range m.Periods {
		var last time.Duration
		for _, as := range per.AdaptationSets {
			tpl := as.SegmentTemplate
			for _, sg := range tpl.Timeline.Resolve() {
				end := mediaEnd(tpl, sg)
				last = max(last, end)
				abs := snap.AST.Add(time.Duration(per.Start) + end)
				assert.LessOrEqual(t, abs.Sub(snap.Now), oneSample, "%s %s ends after Now", per.ID, as.ContentType)
			}
		}
		if i+1 < len(m.Periods) {
			assert.LessOrEqual(t, time.Duration(per.Start)+last, time.Duration(m.Periods[i+1].Start), "%s overlaps the next period", per.ID)
		}
	}
}

func TestMPDPeriodStartSurvivesSlide(t *testing.T) {
	h := hub.New(hub.Options{})
	p := NewPackager(h, Options{Window: 3, Now: farFuture})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	handleAll(p, synthAV(t, 1, 0))
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(14 * time.Second)})
	handleAll(p, synthAV(t, 2, 360000))
	p.Flush()
	before := mpdOf(t, serve(p))
	require.Len(t, before.Periods, 2)
	h.SetAnchor(media.Anchor{Epoch: 3, Start: 720000, Wall: t0.Add(40 * time.Second)})
	handleAll(p, synthAV(t, 3, 720000))
	p.Flush()
	after := mpdOf(t, serve(p))
	var got dash.Duration = -1
	for _, per := range after.Periods {
		assert.NotEqual(t, "e1", per.ID, "epoch 1 slid out")
		if per.ID == "e2" {
			got = per.Start
		}
	}
	assert.Equal(t, before.Periods[1].Start, got)
}

func TestMPDAudioOffset(t *testing.T) {
	h := hub.New(hub.Options{})
	p := NewPackager(h, Options{Window: 10, Now: farFuture})
	h.SetAnchor(media.Anchor{Epoch: 1, Start: 548640, Wall: t0})
	handleAll(p, synthAV(t, 1, 548640))
	p.Flush()
	m := mpdOf(t, serve(p))
	require.Len(t, m.Periods, 1)
	sets := m.Periods[0].AdaptationSets
	require.Len(t, sets, 2)
	assert.EqualValues(t, 548640, sets[0].SegmentTemplate.PresentationTimeOffset)
	assert.EqualValues(t, 292608, sets[1].SegmentTemplate.PresentationTimeOffset)
	// The first segment starts at the offset in both timescales.
	assert.Equal(t, int64(548640), int64(*sets[0].SegmentTemplate.Timeline.S[0].T))
	assert.InDelta(t, 292608, float64(*sets[1].SegmentTemplate.Timeline.S[0].T), 1024)
}

func TestMPDCodecStringFailure(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	s := p.Snapshot()
	bad := *s.Epochs[0].Tracks[0].Config
	bad.SPS = nil
	s.Epochs[0].Tracks = []EpochTrack{
		{Track: 0, Config: &bad, Init: s.Epochs[0].Tracks[0].Init},
		s.Epochs[0].Tracks[1],
	}
	body, skipped := buildMPD(s)
	assert.Equal(t, 1, skipped)
	m, err := dash.Parse(body)
	require.NoError(t, err)
	require.Len(t, m.Periods[0].AdaptationSets, 1)
	assert.Equal(t, "audio", m.Periods[0].AdaptationSets[0].ContentType)

	// The route counts the skip as a packager error.
	before := p.Errors()
	i := slices.IndexFunc(playlistRoutes, func(r playlistRoute) bool { return r.dir == dirCMAF && r.match("index.mpd") })
	require.GreaterOrEqual(t, i, 0)
	_, _, ok := playlistRoutes[i].build(p, s, "index.mpd")
	assert.True(t, ok)
	assert.EqualValues(t, before+1, p.Errors())
}

func TestMPDMissingTrack(t *testing.T) {
	p, _ := packClip(t, "h264_aac_then_640.ts", Options{Window: 100, Now: farFuture})
	m := mpdOf(t, serve(p))
	require.Len(t, m.Periods, 2)
	assert.Len(t, m.Periods[0].AdaptationSets, 2)
	p2 := m.Periods[1]
	assert.Equal(t, "e2", p2.ID)
	assert.Equal(t, dash.Duration(6096*time.Millisecond), p2.Start)
	require.Len(t, p2.AdaptationSets, 1)
	assert.Equal(t, "video", p2.AdaptationSets[0].ContentType)
	assert.Equal(t, "avc1.4D401E", p2.AdaptationSets[0].Representations[0].Codecs)
	assert.Equal(t, 640, p2.AdaptationSets[0].Representations[0].Width)
	st := p2.AdaptationSets[0].SegmentTemplate
	assert.Equal(t, "init-0-2.mp4", st.Initialization)
	require.NotNil(t, st.StartNumber)
	assert.EqualValues(t, 3, *st.StartNumber)
}

func TestMPDSkipsTrackWithoutInit(t *testing.T) {
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	s := p.Snapshot()
	s.Epochs[0].Tracks = []EpochTrack{s.Epochs[0].Tracks[0], {Track: 1, Config: s.Epochs[0].Tracks[1].Config}}
	data, _ := buildMPD(s)
	m, err := dash.Parse(data)
	require.NoError(t, err)
	require.Len(t, m.Periods[0].AdaptationSets, 1)
	assert.Equal(t, "0", m.Periods[0].AdaptationSets[0].Representations[0].ID)
}

func TestMPDFFprobe(t *testing.T) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	p, _ := packClip(t, "h264_aac.ts", Options{Window: 100, Now: farFuture})
	ts := httptest.NewServer(serve(p))
	defer ts.Close()
	out, err := exec.CommandContext(context.Background(), bin, "-v", "error", "-show_entries", "stream=codec_name",
		"-of", "csv=p=0", ts.URL+"/live/s/cmaf/index.mpd").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "h264")
	assert.Contains(t, string(out), "aac")
}
