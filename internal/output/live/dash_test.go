package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
	assert.Nil(t, buildMPD(Snapshot{}))
	// Listed but nothing usable: no init for any track.
	s := p.Snapshot()
	for i := range s.Epochs {
		s.Epochs[i].Tracks = []EpochTrack{{Track: 0, Config: s.Epochs[i].Tracks[0].Config}}
	}
	assert.Nil(t, buildMPD(s))
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

func TestMPDPeriodAfterPause(t *testing.T) {
	cfg := h264Config(t)
	h := hub.New(hub.Options{})
	clk := &fakeNow{t: t0.Add(time.Hour)}
	p := NewPackager(h, Options{Window: 3, Now: clk.Now})
	srv := serve(p)

	h.SetAnchor(media.Anchor{Epoch: 1, Start: 0, Wall: t0})
	handleAll(p, synth(cfg, 1, 0, 100))
	h.SetAnchor(media.Anchor{Epoch: 2, Start: 360000, Wall: t0.Add(14 * time.Second)})
	handleAll(p, synth(cfg, 2, 360000, 100))
	p.Flush()

	m := mpdOf(t, srv)
	var e2 dash.Period
	for _, per := range m.Periods {
		if per.ID == "e2" {
			e2 = per
		}
	}
	require.Equal(t, "e2", e2.ID)
	assert.Equal(t, dash.Duration(14*time.Second), e2.Start)
	st := e2.AdaptationSets[0].SegmentTemplate
	assert.EqualValues(t, 360000, st.PresentationTimeOffset)

	// Nothing listed ends after Now.
	snap := p.Snapshot()
	for _, per := range m.Periods {
		for _, as := range per.AdaptationSets {
			tpl := as.SegmentTemplate
			for _, sg := range tpl.Timeline.Resolve() {
				end := time.Duration(per.Start) + time.Duration((sg.T+sg.D-tpl.PresentationTimeOffset)*uint64(time.Second)/uint64(tpl.Timescale))
				assert.LessOrEqual(t, snap.AST.Add(end), snap.Now)
			}
		}
	}

	// Later epoch 1 slides out: epoch 2 keeps its start.
	h.SetAnchor(media.Anchor{Epoch: 3, Start: 720000, Wall: t0.Add(40 * time.Second)})
	handleAll(p, synth(cfg, 3, 720000, 100))
	p.Flush()
	m2 := mpdOf(t, srv)
	var got dash.Duration = -1
	for _, per := range m2.Periods {
		assert.NotEqual(t, "e1", per.ID)
		if per.ID == "e2" {
			got = per.Start
		}
	}
	assert.Equal(t, e2.Start, got)
}

func TestMPDMissingTrack(t *testing.T) {
	p, _ := packClip(t, "h264_aac_then_640.ts", Options{Window: 100, Now: farFuture})
	m := mpdOf(t, serve(p))
	require.Len(t, m.Periods, 2)
	assert.Len(t, m.Periods[0].AdaptationSets, 2)
	p2 := m.Periods[1]
	assert.Equal(t, "e2", p2.ID)
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
	data := buildMPD(s)
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
