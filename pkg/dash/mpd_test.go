package dash

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleMPD() MPD {
	ast := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	five := uint64(5)
	return MPD{
		Type:                       "dynamic",
		Profiles:                   ProfileLive,
		AvailabilityStartTime:      &ast,
		PublishTime:                &ast,
		MinimumUpdatePeriod:        Duration(2 * time.Second),
		TimeShiftBufferDepth:       Duration(12 * time.Second),
		SuggestedPresentationDelay: Duration(6 * time.Second),
		MinBufferTime:              Duration(2 * time.Second),
		UTCTiming:                  &UTCTiming{SchemeIDURI: UTCTimingDirect, Value: "2026-01-02T03:04:05Z"},
		Periods: []Period{{
			ID: "p0", Start: Duration(1500 * time.Millisecond),
			AdaptationSets: []AdaptationSet{
				{
					ContentType: "video", MimeType: "video/mp4", SegmentAlignment: true,
					Representations: []Representation{{
						ID: "v1", Codecs: "avc1.64001f", Bandwidth: 2000000, Width: 1280, Height: 720,
						SegmentTemplate: &SegmentTemplate{
							Timescale: 90000, PresentationTimeOffset: 900, StartNumber: &five,
							Initialization: "init-1-0.mp4", Media: "1-$Number$.m4s",
							Timeline: &SegmentTimeline{S: []S{{T: u64(900), D: 180000}, {T: u64(180900), D: 180000}}},
						},
					}},
				},
				{
					ContentType: "audio", MimeType: "audio/mp4", SegmentAlignment: true,
					Representations: []Representation{{
						ID: "a2", Codecs: "mp4a.40.2", Bandwidth: 128000, AudioSamplingRate: 48000,
						AudioChannelConfiguration: &AudioChannelConfiguration{SchemeIDURI: AudioChannelScheme, Value: "2"},
					}},
				},
			},
		}},
	}
}

func TestRoundTrip(t *testing.T) {
	m := sampleMPD()
	b := m.Encode()
	s := string(b)
	assert.Contains(t, s, `xmlns="urn:mpeg:dash:schema:mpd:2011"`)
	assert.Contains(t, s, `minimumUpdatePeriod="PT2S"`)
	assert.Contains(t, s, `availabilityStartTime="2026-01-02T03:04:05Z"`)
	assert.Contains(t, s, `start="PT1.5S"`)
	assert.Contains(t, s, `media="1-$Number$.m4s"`)

	got, err := Parse(b)
	require.NoError(t, err)
	m.Xmlns = NS
	assert.Equal(t, m, got)
}

func TestEncodeUTCConversion(t *testing.T) {
	loc := time.FixedZone("x", 7*3600)
	ts := time.Date(2026, 1, 2, 10, 0, 0, 0, loc)
	b := MPD{AvailabilityStartTime: &ts}.Encode()
	assert.Contains(t, string(b), `availabilityStartTime="2026-01-02T03:00:00Z"`)
	assert.Equal(t, 10, ts.Hour(), "caller value must not be mutated")
}

func TestDurationFormat(t *testing.T) {
	cases := map[time.Duration]string{
		2 * time.Second:                       "PT2S",
		533 * time.Millisecond:                "PT0.533S",
		90*time.Second + 500*time.Millisecond: "PT1M30.5S",
		time.Hour + 2*time.Second:             "PT1H2S",
		26*time.Hour + 3*time.Minute:          "P1DT2H3M",
		time.Hour:                             "PT1H",
		-1500 * time.Millisecond:              "-PT1.5S",
	}
	for d, want := range cases {
		assert.Equal(t, want, FormatDuration(d), want)
	}
	assert.Equal(t, "PT0S", FormatDuration(0))
}

func TestDurationParse(t *testing.T) {
	cases := map[string]time.Duration{
		"PT2S":       2 * time.Second,
		"PT0.533S":   533 * time.Millisecond,
		"PT1M30.5S":  90*time.Second + 500*time.Millisecond,
		"P1DT2H3M4S": 26*time.Hour + 3*time.Minute + 4*time.Second,
		"PT1H":       time.Hour,
		"-PT1.5S":    -1500 * time.Millisecond,
		"PT.5S":      500 * time.Millisecond,
		"PT0S":       0,
		"P0D":        0,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "2S", "P", "PT", "PTS", "PT1X", "P1Y", "PT1S2M", "PT1.2.3S", "PT1", "P1DT", "PTxS"} {
		_, err := ParseDuration(bad)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "dash:")
	}
}

const dashjsStyle = `<?xml version="1.0" encoding="utf-8"?>
<!-- generated elsewhere -->
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
     xsi:schemaLocation="urn:mpeg:dash:schema:mpd:2011 DASH-MPD.xsd"
     type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011"
     availabilityStartTime="2026-01-02T03:04:05.250Z" minimumUpdatePeriod="PT2S"
     minBufferTime="PT4S" timeShiftBufferDepth="PT1M30.5S" maxSegmentDuration="PT2S">
  <ProgramInformation><Title>x</Title></ProgramInformation>
  <Period id="1" start="PT0S">
    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" segmentAlignment="true" par="16:9">
      <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
      <SegmentTemplate timescale="90000" initialization="i-$RepresentationID$.mp4" media="$RepresentationID$-$Number$.m4s" startNumber="1">
        <SegmentTimeline><S t="0" d="180000" r="2"/><S d="90000"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v" bandwidth="800000" codecs="avc1.4d401f" width="640" height="360" frameRate="30"/>
    </AdaptationSet>
  </Period>
  <UTCTiming schemeIdUri="urn:mpeg:dash:utc:direct:2014" value="2026-01-02T03:04:06Z"/>
</MPD>`

func TestParseForeign(t *testing.T) {
	m, err := Parse([]byte(dashjsStyle))
	require.NoError(t, err)
	assert.Equal(t, "dynamic", m.Type)
	assert.Equal(t, Duration(90*time.Second+500*time.Millisecond), m.TimeShiftBufferDepth)
	require.NotNil(t, m.AvailabilityStartTime)
	assert.Equal(t, 250*time.Millisecond, time.Duration(m.AvailabilityStartTime.Nanosecond()))
	require.NotNil(t, m.UTCTiming)
	require.Len(t, m.Periods, 1)
	as := m.Periods[0].AdaptationSets[0]
	assert.True(t, as.SegmentAlignment)
	st := as.SegmentTemplate
	require.NotNil(t, st)
	require.Len(t, st.Timeline.S, 2)
	assert.Equal(t, int64(2), st.Timeline.S[0].R)
	assert.Equal(t, uint64(90000), st.Timeline.S[1].D)
	assert.Nil(t, st.Timeline.S[1].T)
	assert.Equal(t, []Seg{{0, 180000}, {180000, 180000}, {360000, 180000}, {540000, 90000}}, st.Timeline.Resolve())
	assert.Equal(t, 640, as.Representations[0].Width)
}

func TestParseErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"not xml":      "hello",
		"wrong root":   `<foo/>`,
		"bad duration": `<MPD minBufferTime="2S"/>`,
		"bad time":     `<MPD availabilityStartTime="yesterday"/>`,
		"bad period":   `<MPD><Period start="x"/></MPD>`,
		"truncated":    `<MPD><Period>`,
	} {
		_, err := Parse([]byte(in))
		require.Error(t, err, name)
		assert.True(t, strings.HasPrefix(err.Error(), "dash:"), name+": "+err.Error())
	}
}

func u64(v uint64) *uint64 { return &v }

func TestResolveImplicitStart(t *testing.T) {
	tl := SegmentTimeline{S: []S{{D: 96256, R: 1}, {D: 256}, {T: u64(1000000), D: 10}, {D: 10, R: -1}}}
	assert.Equal(t, []Seg{
		{0, 96256},
		{96256, 96256},
		{192512, 256},
		{1000000, 10},
		{1000010, 10},
	}, tl.Resolve())
	assert.Empty(t, SegmentTimeline{}.Resolve())
	assert.Len(t, SegmentTimeline{S: []S{{D: 1, R: 1 << 40}}}.Resolve(), maxRepeat+1)
}

func TestMissingTNotWritten(t *testing.T) {
	m := MPD{Periods: []Period{{AdaptationSets: []AdaptationSet{{Representations: []Representation{{
		ID: "v", SegmentTemplate: &SegmentTemplate{Timeline: &SegmentTimeline{S: []S{{D: 5, R: 1}, {D: 6}}}},
	}}}}}}}
	b := m.Encode()
	assert.NotContains(t, string(b), ` t="`)
	got, err := Parse(b)
	require.NoError(t, err)
	assert.Nil(t, got.Periods[0].AdaptationSets[0].Representations[0].SegmentTemplate.Timeline.S[0].T)
}

func TestMinBufferTimeAlwaysWritten(t *testing.T) {
	assert.Contains(t, string(MPD{}.Encode()), `minBufferTime="PT0S"`)
}

func TestParseDurationOverflow(t *testing.T) {
	for _, in := range []string{
		"P999999999D", "PT9999999999999S", "PT9223372036854775807S", "PT9999999999H",
		"P106751DT24H", "P106751D106751D",
	} {
		_, err := ParseDuration(in)
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), "dash:")
	}
	d, err := ParseDuration("PT9223372036S")
	require.NoError(t, err)
	assert.Equal(t, 9223372036*time.Second, d)
}
