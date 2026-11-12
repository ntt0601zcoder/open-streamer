package hls

import (
	"fmt"
	"math"
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
	// Key is the EXT-X-KEY in effect for this segment; zero when none was
	// given.
	Key Key
	// ByteRange is the segment's sub-range of URI (EXT-X-BYTERANGE); nil
	// means the whole resource.
	ByteRange *ByteRange
}

// Key is an EXT-X-KEY: how the segments that follow it are encrypted.
type Key struct {
	Method string // NONE, AES-128, SAMPLE-AES, ...
	URI    string // the key's URI; empty for NONE
}

// ByteRange is a sub-range of a resource: Length bytes from Offset.
type ByteRange struct {
	Length, Offset int64
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

// Encode renders the playlist. EXT-X-MAP and EXT-X-KEY are written on the
// first segment that has one and whenever they differ from the previous
// segment's; EXT-X-BYTERANGE always carries its offset.
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
	var prevKey Key
	for _, s := range p.Segments {
		if s.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.Key != prevKey && s.Key.Method != "" {
			fmt.Fprintf(&b, "#EXT-X-KEY:METHOD=%s", s.Key.Method)
			if s.Key.URI != "" {
				fmt.Fprintf(&b, ",URI=\"%s\"", s.Key.URI)
			}
			b.WriteString("\n")
		}
		prevKey = s.Key
		if s.Map != "" && s.Map != prevMap {
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", s.Map)
		}
		prevMap = s.Map
		if s.ByteRange != nil {
			fmt.Fprintf(&b, "#EXT-X-BYTERANGE:%d@%d\n", s.ByteRange.Length, s.ByteRange.Offset)
		}
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

// ParseMedia parses a media playlist. Unknown tags are ignored; EXT-X-MAP and
// EXT-X-KEY are carried to every following segment. An EXT-X-BYTERANGE
// without an offset starts where the previous range of the same URI ended.
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
		curKey  Key
		rng     *pendingRange
		rngEnd  = map[string]int64{} // end of the last range of each URI
	)
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "#") {
			if !haveInf {
				return p, fmt.Errorf("hls: URI %q without #EXTINF", l)
			}
			cur.URI = l
			cur.Map = curMap
			cur.Key = curKey
			if rng != nil {
				br, err := rng.resolve(l, rngEnd)
				if err != nil {
					return p, fmt.Errorf("hls: BYTERANGE: %w", err)
				}
				cur.ByteRange = br
			}
			p.Segments = append(p.Segments, cur)
			cur, haveInf, rng = Segment{}, false, nil
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
		case "#EXT-X-KEY":
			curKey, err = parseKey(val)
		case "#EXT-X-BYTERANGE":
			rng, err = parseByteRange(val)
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

func parseKey(val string) (Key, error) {
	attrs, err := parseAttrs(val)
	if err != nil {
		return Key{}, err
	}
	k := Key{Method: attrs["METHOD"], URI: attrs["URI"]}
	switch {
	case k.Method == "":
		return Key{}, fmt.Errorf("missing METHOD")
	case k.Method != "NONE" && k.URI == "":
		return Key{}, fmt.Errorf("missing URI")
	}
	return k, nil
}

// pendingRange is an EXT-X-BYTERANGE waiting for its segment's URI.
type pendingRange struct {
	length, offset int64
	hasOffset      bool
}

// parseByteRange parses "<length>[@<offset>]".
func parseByteRange(val string) (*pendingRange, error) {
	l, o, hasOffset := strings.Cut(val, "@")
	length, err := strconv.ParseInt(strings.TrimSpace(l), 10, 64)
	if err != nil {
		return nil, err
	}
	if length <= 0 {
		return nil, fmt.Errorf("length %d", length)
	}
	r := &pendingRange{length: length, hasOffset: hasOffset}
	if hasOffset {
		if r.offset, err = strconv.ParseInt(strings.TrimSpace(o), 10, 64); err != nil {
			return nil, err
		}
		if r.offset < 0 {
			return nil, fmt.Errorf("offset %d", r.offset)
		}
	}
	return r, nil
}

// resolve returns the range of uri, defaulting its offset to the end of the
// previous range of uri, and records where it ends.
func (r *pendingRange) resolve(uri string, ends map[string]int64) (*ByteRange, error) {
	off := r.offset
	if !r.hasOffset {
		end, ok := ends[uri]
		if !ok {
			return nil, fmt.Errorf("no offset and no previous range of %q", uri)
		}
		off = end
	}
	if off > math.MaxInt64-r.length {
		return nil, fmt.Errorf("range %d@%d overflows", r.length, off)
	}
	ends[uri] = off + r.length
	return &ByteRange{Length: r.length, Offset: off}, nil
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
