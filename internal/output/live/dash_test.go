package live

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
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
	body := buildMPD(Snapshot{})
	assert.Nil(t, body)
	// Listed but nothing usable: no init for any track.
	s := p.Snapshot()
	for i := range s.Epochs {
		s.Epochs[i].Tracks = []EpochTrack{{Track: 0, Config: s.Epochs[i].Tracks[0].Config}}
	}
	body = buildMPD(s)
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
	bad := s.Epochs[0].Tracks[0]
	bad.Codec = ""
	s.Epochs[0].Tracks = []EpochTrack{bad, s.Epochs[0].Tracks[1]}
	m, err := dash.Parse(buildMPD(s))
	require.NoError(t, err)
	require.Len(t, m.Periods[0].AdaptationSets, 1, "the set without a codec string is left out")
	assert.Equal(t, "audio", m.Periods[0].AdaptationSets[0].ContentType)
}

// A codec string that cannot be built is counted once per epoch and track,
// when the packager meets the track, never per MPD request, and not as a
// dropped segment output.
func TestCodecErrorsCountedOnce(t *testing.T) {
	cfg := h264Config(t)
	noASC := &media.TrackConfig{Codec: media.CodecAAC, SampleRate: 48000, Channels: 2}
	_, err := media.CodecString(noASC)
	require.Error(t, err)
	h := hub.New(hub.Options{})
	h.SetAnchor(media.Anchor{Epoch: 1, Wall: t0})
	p := NewPackager(h, Options{Window: 100, Now: farFuture})
	for _, d := range synth(cfg, 1, 0, 200) {
		p.Handle(d)
		if d.Frame.DTS%3840 == 0 {
			p.Handle(hub.Delivery{Frame: media.Frame{Track: 1, Config: noASC, PTS: d.Frame.DTS, DTS: d.Frame.DTS, Key: true, Data: [][]byte{{0x21}}, Epoch: 1}})
		}
	}
	p.Flush()
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 4)
	et, ok := snap.Epochs[0].Track(1)
	require.True(t, ok)
	assert.Empty(t, et.Codec)
	vt, _ := snap.Epochs[0].Track(0)
	assert.Equal(t, "avc1.4D400D", vt.Codec)
	errs := p.Errors()
	srv := serve(p)
	for range 5 {
		m := mpdOf(t, srv)
		require.Len(t, m.Periods, 1)
		require.Len(t, m.Periods[0].AdaptationSets, 1)
		assert.Equal(t, "video", m.Periods[0].AdaptationSets[0].ContentType)
	}
	assert.EqualValues(t, 1, p.CodecErrors())
	assert.Equal(t, errs, p.Errors(), "MPD requests count nothing")
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

// videoSets returns the video and the audio adaptation sets of the MPD's
// only period.
func videoSets(t *testing.T, m dash.MPD) (video, audio []dash.AdaptationSet) {
	t.Helper()
	require.Len(t, m.Periods, 1)
	for _, as := range m.Periods[0].AdaptationSets {
		switch as.ContentType {
		case "video":
			video = append(video, as)
		case "audio":
			audio = append(audio, as)
		default:
			t.Fatalf("content type %q", as.ContentType)
		}
	}
	return video, audio
}

// noAlignment asserts that no video AdaptationSet element of body carries a
// segmentAlignment attribute.
func noAlignment(t *testing.T, body []byte) {
	t.Helper()
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "<AdaptationSet") && strings.Contains(line, `contentType="video"`) {
			n++
			assert.NotContains(t, line, "segmentAlignment", line)
		}
	}
	assert.Positive(t, n)
}

// checkRep checks an ABR video Representation's own SegmentTemplate.
func checkRep(t *testing.T, snap Snapshot, rep dash.Representation) {
	t.Helper()
	track, err := strconv.Atoi(rep.ID)
	require.NoError(t, err)
	et, ok := snap.Epochs[0].Track(track)
	require.True(t, ok)
	assert.Equal(t, et.Codec, rep.Codecs)
	assert.Equal(t, et.Config.Width, rep.Width)
	assert.Equal(t, et.Config.Height, rep.Height)
	assert.GreaterOrEqual(t, rep.Bandwidth, 1)
	st := rep.SegmentTemplate
	require.NotNil(t, st)
	assert.EqualValues(t, 90000, st.Timescale)
	assert.Equal(t, "init-"+rep.ID+"-1.mp4", st.Initialization)
	assert.Equal(t, rep.ID+"-$Number$.m4s", st.Media)
	assert.EqualValues(t, media.Rescale(snap.Epochs[0].Anchor.Start, 90000), st.PresentationTimeOffset)
	require.NotNil(t, st.StartNumber)
	require.NotNil(t, st.Timeline)
}

func TestABRMPD(t *testing.T) {
	p := packLadder(t, Options{ABR: true}, nil)
	snap := p.Snapshot()
	srv := serve(p)
	code, body := get(t, srv, "/live/s/cmaf/index.mpd")
	require.Equal(t, http.StatusOK, code)
	noAlignment(t, body)
	video, audio := videoSets(t, mpdOf(t, srv))

	require.Len(t, video, 1)
	as := video[0]
	assert.Equal(t, "video/mp4", as.MimeType)
	assert.False(t, as.SegmentAlignment)
	assert.Nil(t, as.SegmentTemplate, "the template is per Representation")
	require.Len(t, as.Representations, len(ladderSizes))
	for i, rep := range as.Representations {
		assert.Equal(t, strconv.Itoa(i), rep.ID)
		assert.Equal(t, ladderSizes[i][0], rep.Width)
		assert.Equal(t, ladderSizes[i][1], rep.Height)
		checkRep(t, snap, rep)
		assert.EqualValues(t, 0, *rep.SegmentTemplate.StartNumber)
		assert.Len(t, rep.SegmentTemplate.Timeline.S, 5)
		assert.Equal(t, as.Representations[0].SegmentTemplate.Timeline.Resolve(), rep.SegmentTemplate.Timeline.Resolve(), "rep %d", i)
	}

	// The audio set as without ABR.
	require.Len(t, audio, 1)
	assert.True(t, audio[0].SegmentAlignment)
	require.NotNil(t, audio[0].SegmentTemplate)
	assert.Equal(t, "init-3-1.mp4", audio[0].SegmentTemplate.Initialization)
	require.Len(t, audio[0].Representations, 1)
	assert.Equal(t, "3", audio[0].Representations[0].ID)
	assert.Nil(t, audio[0].Representations[0].SegmentTemplate)
}

func TestABRMPDGap(t *testing.T) {
	// Track 1 has no IDR at segment 2's Start: R-B8 leaves it out of segment 2.
	k := int64(2 * 180000)
	p := packLadder(t, Options{ABR: true}, func(ds []hub.Delivery) {
		for i := range ds {
			if f := &ds[i].Frame; f.Track == 1 && f.PTS == k {
				f.Key = false
				f.Data = [][]byte{{0x41, 0x9a, 0, 1}}
			}
		}
	})
	snap := p.Snapshot()
	require.Len(t, snap.Segments, 5)
	_, ok := trackPart(snap, snap.Segments[2], 1)
	require.False(t, ok, "track 1 left out of segment 2")
	srv := serve(p)
	video, _ := videoSets(t, mpdOf(t, srv))
	require.Len(t, video, 1)
	require.Len(t, video[0].Representations, 3)

	for _, rep := range video[0].Representations {
		checkRep(t, snap, rep)
		st := rep.SegmentTemplate
		segs := st.Timeline.Resolve()
		if rep.ID == "1" {
			require.Len(t, segs, 4)
			for _, sg := range segs {
				assert.NotEqual(t, uint64(k), sg.T, "no entry for the left-out segment")
			}
		} else {
			require.Len(t, segs, 5)
		}
		_, init := get(t, srv, "/live/s/cmaf/"+st.Initialization)
		require.NotEmpty(t, init)
		for i, sg := range segs {
			path := "/live/s/cmaf/" + strings.ReplaceAll(st.Media, "$Number$", strconv.FormatUint(*st.StartNumber+uint64(i), 10))
			code, body := get(t, srv, path)
			require.Equal(t, http.StatusOK, code, path)
			f, err := mp4.DecodeFile(bytes.NewReader(append(slices.Clone(init), body...)))
			require.NoError(t, err, path)
			traf := f.Segments[0].Fragments[0].Moof.Traf
			earliest := int64(traf.Tfdt.BaseMediaDecodeTime()) + int64(traf.Trun.Samples[0].CompositionTimeOffset)
			assert.Equal(t, int64(sg.T), earliest, path)
		}
	}
}

// h265Config returns the video config of the h265_aac51.ts fixture.
func h265Config(t *testing.T) *media.TrackConfig {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixture(t, "h265_aac51.ts")))
	require.NoError(t, err)
	for {
		f, err := d.Read()
		require.NoError(t, err)
		if f.Kind() == media.KindVideo && f.Config != nil {
			require.Equal(t, media.CodecH265, f.Config.Codec)
			return f.Config
		}
	}
}

func TestABRMPDTwoCodecs(t *testing.T) {
	hevc := h265Config(t)
	p := packLadder(t, Options{ABR: true}, func(ds []hub.Delivery) {
		for i := range ds {
			if f := &ds[i].Frame; f.Track == 2 {
				f.Config = hevc
				if f.Key {
					f.Data = [][]byte{{0x26, 0x01, 0xaf}} // IDR_W_RADL
				} else {
					f.Data = [][]byte{{0x02, 0x01, 0xd0}} // TRAIL_R
				}
			}
		}
	})
	snap := p.Snapshot()
	srv := serve(p)
	code, body := get(t, srv, "/live/s/cmaf/index.mpd")
	require.Equal(t, http.StatusOK, code)
	noAlignment(t, body)
	video, audio := videoSets(t, mpdOf(t, srv))
	require.Len(t, audio, 1)
	require.Len(t, video, 2)
	ids := make([][]string, 0, len(video))
	for _, as := range video {
		assert.Nil(t, as.SegmentTemplate)
		var set []string
		for _, rep := range as.Representations {
			set = append(set, rep.ID)
			checkRep(t, snap, rep)
		}
		ids = append(ids, set)
	}
	assert.Equal(t, [][]string{{"0", "1"}, {"2"}}, ids)
	assert.True(t, strings.HasPrefix(video[0].Representations[0].Codecs, "avc1."))
	assert.True(t, strings.HasPrefix(video[1].Representations[0].Codecs, "hvc1.") || strings.HasPrefix(video[1].Representations[0].Codecs, "hev1."))
}

func TestABRMPDFFprobe(t *testing.T) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	p := packLadder(t, Options{ABR: true}, nil)
	ts := httptest.NewServer(serve(p))
	defer ts.Close()
	out, err := exec.CommandContext(context.Background(), bin, "-v", "error", "-show_entries", "stream=codec_type,codec_name",
		"-of", "json", ts.URL+"/live/s/cmaf/index.mpd").Output()
	require.NoError(t, err, string(out))
	// The top-level streams; ffprobe also lists them per program.
	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &probe), string(out))
	count := map[string]int{}
	for _, st := range probe.Streams {
		count[st.CodecName+","+st.CodecType]++
	}
	assert.Equal(t, map[string]int{"h264,video": 3, "aac,audio": 1}, count, string(out))
}
