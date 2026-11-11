package dash

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Namespace and scheme identifiers used by the supported subset.
const (
	// NS is the MPD XML namespace.
	NS = "urn:mpeg:dash:schema:mpd:2011"
	// ProfileLive is the live ISO BMFF profile.
	ProfileLive = "urn:mpeg:dash:profile:isoff-live:2011"
	// UTCTimingDirect is the scheme for a UTCTiming value that carries the
	// server time directly.
	UTCTimingDirect = "urn:mpeg:dash:utc:direct:2014"
	// AudioChannelScheme is the audio channel configuration scheme.
	AudioChannelScheme = "urn:mpeg:dash:23003:3:audio_channel_configuration:2011"
)

// Duration is an ISO 8601 xs:duration attribute value.
type Duration time.Duration

// MarshalXMLAttr implements xml.MarshalerAttr.
func (d Duration) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	return xml.Attr{Name: name, Value: FormatDuration(time.Duration(d))}, nil
}

// UnmarshalXMLAttr implements xml.UnmarshalerAttr.
func (d *Duration) UnmarshalXMLAttr(attr xml.Attr) error {
	v, err := ParseDuration(attr.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MPD is the root element.
type MPD struct {
	XMLName                    xml.Name   `xml:"MPD"`
	Xmlns                      string     `xml:"xmlns,attr,omitempty"`
	Type                       string     `xml:"type,attr,omitempty"`
	Profiles                   string     `xml:"profiles,attr,omitempty"`
	AvailabilityStartTime      *time.Time `xml:"availabilityStartTime,attr,omitempty"`
	PublishTime                *time.Time `xml:"publishTime,attr,omitempty"`
	MinimumUpdatePeriod        Duration   `xml:"minimumUpdatePeriod,attr,omitempty"`
	TimeShiftBufferDepth       Duration   `xml:"timeShiftBufferDepth,attr,omitempty"`
	SuggestedPresentationDelay Duration   `xml:"suggestedPresentationDelay,attr,omitempty"`
	MinBufferTime              Duration   `xml:"minBufferTime,attr,omitempty"`
	Periods                    []Period   `xml:"Period"`
	UTCTiming                  *UTCTiming `xml:"UTCTiming,omitempty"`
}

// UTCTiming tells clients how to synchronise their clock.
type UTCTiming struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr,omitempty"`
}

// Period is a media period. Start is always written, including zero.
type Period struct {
	ID             string          `xml:"id,attr,omitempty"`
	Start          Duration        `xml:"start,attr"`
	AdaptationSets []AdaptationSet `xml:"AdaptationSet"`
}

// AdaptationSet groups the representations of one media type.
type AdaptationSet struct {
	ContentType      string           `xml:"contentType,attr,omitempty"`
	MimeType         string           `xml:"mimeType,attr,omitempty"`
	SegmentAlignment bool             `xml:"segmentAlignment,attr,omitempty"`
	SegmentTemplate  *SegmentTemplate `xml:"SegmentTemplate,omitempty"`
	Representations  []Representation `xml:"Representation"`
}

// Representation is one encoding of an adaptation set.
type Representation struct {
	ID                        string                     `xml:"id,attr"`
	Codecs                    string                     `xml:"codecs,attr,omitempty"`
	Bandwidth                 int                        `xml:"bandwidth,attr,omitempty"`
	Width                     int                        `xml:"width,attr,omitempty"`
	Height                    int                        `xml:"height,attr,omitempty"`
	AudioSamplingRate         int                        `xml:"audioSamplingRate,attr,omitempty"`
	AudioChannelConfiguration *AudioChannelConfiguration `xml:"AudioChannelConfiguration,omitempty"`
	SegmentTemplate           *SegmentTemplate           `xml:"SegmentTemplate,omitempty"`
}

// AudioChannelConfiguration describes the audio channel layout.
type AudioChannelConfiguration struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr,omitempty"`
}

// SegmentTemplate locates the initialization and media segments. StartNumber
// is a pointer because an absent attribute means 1, while 0 is a valid value
// that must be written.
type SegmentTemplate struct {
	Timescale              uint32           `xml:"timescale,attr,omitempty"`
	PresentationTimeOffset uint64           `xml:"presentationTimeOffset,attr,omitempty"`
	Initialization         string           `xml:"initialization,attr,omitempty"`
	Media                  string           `xml:"media,attr,omitempty"`
	StartNumber            *uint64          `xml:"startNumber,attr,omitempty"`
	Timeline               *SegmentTimeline `xml:"SegmentTimeline,omitempty"`
}

// SegmentTimeline lists segment start times and durations.
type SegmentTimeline struct {
	S []S `xml:"S"`
}

// S is one timeline entry. T is always written; R is the repeat count
// (omitted when zero).
type S struct {
	T uint64 `xml:"t,attr"`
	D uint64 `xml:"d,attr"`
	R int64  `xml:"r,attr,omitempty"`
}

// Encode returns the MPD as XML. Times are written in UTC. Xmlns defaults to
// NS. The receiver is not modified.
func (m MPD) Encode() []byte {
	m.XMLName = xml.Name{}
	if m.Xmlns == "" {
		m.Xmlns = NS
	}
	m.AvailabilityStartTime = utcCopy(m.AvailabilityStartTime)
	m.PublishTime = utcCopy(m.PublishTime)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	// Marshalling this fixed tree of strings and numbers cannot fail.
	_ = enc.Encode(m)
	buf.WriteByte('\n')
	return buf.Bytes()
}

func utcCopy(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Parse decodes an MPD. Unknown attributes and elements are ignored.
func Parse(b []byte) (MPD, error) {
	var m MPD
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return MPD{}, errors.New("dash: parse: empty document")
		}
		return MPD{}, fmt.Errorf("dash: parse: %w", err)
	}
	if m.XMLName.Local != "MPD" {
		return MPD{}, fmt.Errorf("dash: parse: unexpected root element %q", m.XMLName.Local)
	}
	m.XMLName = xml.Name{}
	return m, nil
}

// FormatDuration renders d as an xs:duration such as PT2S, PT0.533S or
// PT1M30.5S. Whole days are written as D; there are no months or years.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "PT0S"
	}
	var sb strings.Builder
	if d < 0 {
		sb.WriteByte('-')
		d = -d
	}
	sb.WriteByte('P')
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	d -= mins * time.Minute
	if days > 0 {
		sb.WriteString(strconv.FormatInt(int64(days), 10) + "D")
	}
	if hours == 0 && mins == 0 && d == 0 {
		return sb.String()
	}
	sb.WriteByte('T')
	if hours > 0 {
		sb.WriteString(strconv.FormatInt(int64(hours), 10) + "H")
	}
	if mins > 0 {
		sb.WriteString(strconv.FormatInt(int64(mins), 10) + "M")
	}
	if d > 0 {
		secs := int64(d / time.Second)
		frac := int64(d % time.Second)
		sb.WriteString(strconv.FormatInt(secs, 10))
		if frac > 0 {
			f := strings.TrimRight(fmt.Sprintf("%09d", frac), "0")
			sb.WriteString("." + f)
		}
		sb.WriteByte('S')
	}
	return sb.String()
}

// ParseDuration parses an xs:duration with day, hour, minute and second
// components (seconds may be fractional). Years, months and weeks are
// rejected because they have no fixed length.
func ParseDuration(s string) (time.Duration, error) {
	bad := func(why string) (time.Duration, error) {
		return 0, fmt.Errorf("dash: duration %q: %s", s, why)
	}
	rest := s
	neg := false
	if strings.HasPrefix(rest, "-") {
		neg = true
		rest = rest[1:]
	}
	if !strings.HasPrefix(rest, "P") {
		return bad("missing P designator")
	}
	rest = rest[1:]
	if rest == "" {
		return bad("no components")
	}

	var total time.Duration
	inTime := false
	// Position in the D, H, M, S order; components must not go backwards.
	last := 0
	order := map[byte]int{'D': 1, 'H': 2, 'M': 3, 'S': 4}
	unit := map[byte]time.Duration{'D': 24 * time.Hour, 'H': time.Hour, 'M': time.Minute, 'S': time.Second}
	components := 0
	for len(rest) > 0 {
		if rest[0] == 'T' {
			if inTime {
				return bad("repeated T")
			}
			inTime = true
			rest = rest[1:]
			if rest == "" {
				return bad("T without components")
			}
			if last > 1 {
				return bad("T out of order")
			}
			continue
		}
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == len(rest) {
			return bad("number without unit")
		}
		num, u := rest[:i], rest[i]
		rest = rest[i+1:]
		pos, ok := order[u]
		if !ok || (u == 'D') == inTime {
			return bad("unsupported or misplaced unit " + string(u))
		}
		if pos <= last {
			return bad("components out of order")
		}
		last = pos
		v, err := parseNumber(num, u == 'S')
		if err != nil {
			return bad(err.Error())
		}
		total += time.Duration(v) * unit[u]
		if u == 'S' {
			total += fracNanos(num)
		}
		components++
	}
	if components == 0 {
		return bad("no components")
	}
	if neg {
		total = -total
	}
	return total, nil
}

// parseNumber returns the integer part of num; fractions are
// accepted only when allowFrac is set (seconds) and are handled by fracNanos.
func parseNumber(num string, allowFrac bool) (int64, error) {
	whole, frac, hasFrac := strings.Cut(num, ".")
	if hasFrac && (!allowFrac || frac == "" || strings.Contains(frac, ".")) {
		return 0, errors.New("invalid fraction")
	}
	if whole == "" {
		if !hasFrac {
			return 0, errors.New("empty number")
		}
		whole = "0"
	}
	v, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, errors.New("invalid number")
	}
	return v, nil
}

func fracNanos(num string) time.Duration {
	_, frac, ok := strings.Cut(num, ".")
	if !ok {
		return 0
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	n, _ := strconv.ParseInt(frac, 10, 64)
	return time.Duration(n)
}
