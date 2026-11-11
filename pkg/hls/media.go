package hls

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Segment is one media segment of a MediaPlaylist.
type Segment struct {
	URI             string
	Duration        time.Duration
	Discontinuity   bool
	ProgramDateTime time.Time
	// Map is the initialization section URI (EXT-X-MAP) in effect for this segment.
	Map string
}

// MediaPlaylist is an HLS media playlist.
type MediaPlaylist struct {
	Version, TargetDuration int
	MediaSequence           uint64
	DiscontinuitySequence   uint64
	Independent             bool
	Segments                []Segment
	Ended                   bool
}

// Encode renders the playlist. EXT-X-MAP is written on the first segment and
// whenever Map differs from the previous segment.
func (p MediaPlaylist) Encode() []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", p.Version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", p.TargetDuration)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", p.MediaSequence)
	if p.DiscontinuitySequence != 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", p.DiscontinuitySequence)
	}
	if p.Independent {
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}
	prevMap := ""
	for _, s := range p.Segments {
		if s.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.Map != "" && s.Map != prevMap {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", s.Map)
		}
		prevMap = s.Map
		if !s.ProgramDateTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", s.ProgramDateTime.Format("2006-01-02T15:04:05.000Z07:00"))
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", s.Duration.Seconds(), s.URI)
	}
	if p.Ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String())
}

// ParseMedia parses a media playlist. Unknown tags are ignored; EXT-X-MAP is
// carried to every following segment.
func ParseMedia(b []byte) (MediaPlaylist, error) {
	var p MediaPlaylist
	lines := splitLines(b)
	if len(lines) == 0 || lines[0] != "#EXTM3U" {
		return p, fmt.Errorf("hls: missing #EXTM3U header")
	}
	var (
		cur     Segment
		haveInf bool
		curMap  string
	)
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "#") {
			if !haveInf {
				return p, fmt.Errorf("hls: URI %q without #EXTINF", l)
			}
			cur.URI = l
			cur.Map = curMap
			p.Segments = append(p.Segments, cur)
			cur, haveInf = Segment{}, false
			continue
		}
		name, val, _ := strings.Cut(l, ":")
		var err error
		switch name {
		case "#EXT-X-VERSION":
			p.Version, err = strconv.Atoi(val)
		case "#EXT-X-TARGETDURATION":
			p.TargetDuration, err = strconv.Atoi(val)
		case "#EXT-X-MEDIA-SEQUENCE":
			p.MediaSequence, err = strconv.ParseUint(val, 10, 64)
		case "#EXT-X-DISCONTINUITY-SEQUENCE":
			p.DiscontinuitySequence, err = strconv.ParseUint(val, 10, 64)
		case "#EXT-X-INDEPENDENT-SEGMENTS":
			p.Independent = true
		case "#EXT-X-ENDLIST":
			p.Ended = true
		case "#EXT-X-DISCONTINUITY":
			cur.Discontinuity = true
		case "#EXT-X-PROGRAM-DATE-TIME":
			cur.ProgramDateTime, err = parseDateTime(val)
		case "#EXT-X-MAP":
			curMap, err = parseMapURI(val)
		case "#EXTINF":
			cur.Duration, err = parseExtinf(val)
			haveInf = err == nil
		case "#EXT-X-STREAM-INF":
			return p, fmt.Errorf("hls: multivariant playlist given to ParseMedia")
		}
		if err != nil {
			return p, fmt.Errorf("hls: %s: %w", strings.TrimPrefix(name, "#"), err)
		}
	}
	return p, nil
}

func parseMapURI(val string) (string, error) {
	attrs, err := parseAttrs(val)
	if err != nil {
		return "", err
	}
	uri, ok := attrs["URI"]
	if !ok {
		return "", fmt.Errorf("missing URI")
	}
	return uri, nil
}

func parseExtinf(val string) (time.Duration, error) {
	num, _, _ := strings.Cut(val, ",")
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, fmt.Errorf("negative duration %v", f)
	}
	return time.Duration(f*float64(time.Second) + 0.5), nil
}

// parseDateTime parses RFC 3339, falling back to offsets without a colon
// (for example +0700) as written by ffmpeg.
func parseDateTime(val string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, val)
	if err == nil {
		return t, nil
	}
	if t2, err2 := time.Parse("2006-01-02T15:04:05.999999999Z0700", val); err2 == nil {
		return t2, nil
	}
	return time.Time{}, err
}
