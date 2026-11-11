package hls

import (
	"fmt"
	"strconv"
	"strings"
)

// Rendition is an EXT-X-MEDIA entry.
type Rendition struct {
	Type, GroupID, Name, URI string
	Default                  bool
}

// Variant is an EXT-X-STREAM-INF entry.
type Variant struct {
	URI, Codecs, Audio string
	Bandwidth          int
	Width, Height      int
}

// MultivariantPlaylist is an HLS multivariant (master) playlist.
type MultivariantPlaylist struct {
	Version     int
	Independent bool
	Renditions  []Rendition
	Variants    []Variant
}

// Encode renders the playlist.
func (p MultivariantPlaylist) Encode() []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", p.Version)
	if p.Independent {
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}
	for _, r := range p.Renditions {
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=%s,GROUP-ID=\"%s\",NAME=\"%s\"", r.Type, r.GroupID, r.Name)
		if r.Default {
			b.WriteString(",DEFAULT=YES,AUTOSELECT=YES")
		}
		if r.URI != "" {
			fmt.Fprintf(&b, ",URI=\"%s\"", r.URI)
		}
		b.WriteString("\n")
	}
	for _, v := range p.Variants {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", v.Bandwidth)
		if v.Width > 0 && v.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
		}
		if v.Codecs != "" {
			fmt.Fprintf(&b, ",CODECS=\"%s\"", v.Codecs)
		}
		if v.Audio != "" {
			fmt.Fprintf(&b, ",AUDIO=\"%s\"", v.Audio)
		}
		fmt.Fprintf(&b, "\n%s\n", v.URI)
	}
	return []byte(b.String())
}

// ParseMultivariant parses a multivariant playlist. Unknown tags and
// attributes are ignored.
func ParseMultivariant(b []byte) (MultivariantPlaylist, error) {
	var p MultivariantPlaylist
	lines := splitLines(b)
	if len(lines) == 0 || lines[0] != "#EXTM3U" {
		return p, fmt.Errorf("hls: missing #EXTM3U header")
	}
	var pending *Variant
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "#") {
			if pending == nil {
				continue
			}
			pending.URI = l
			p.Variants = append(p.Variants, *pending)
			pending = nil
			continue
		}
		name, val, _ := strings.Cut(l, ":")
		switch name {
		case "#EXT-X-VERSION":
			v, err := strconv.Atoi(val)
			if err != nil {
				return p, fmt.Errorf("hls: EXT-X-VERSION: %w", err)
			}
			p.Version = v
		case "#EXT-X-INDEPENDENT-SEGMENTS":
			p.Independent = true
		case "#EXT-X-MEDIA":
			r, err := parseRendition(val)
			if err != nil {
				return p, err
			}
			p.Renditions = append(p.Renditions, r)
		case "#EXT-X-STREAM-INF":
			if pending != nil {
				return p, fmt.Errorf("hls: EXT-X-STREAM-INF without URI")
			}
			v, err := parseVariant(val)
			if err != nil {
				return p, err
			}
			pending = &v
		}
	}
	if pending != nil {
		return p, fmt.Errorf("hls: EXT-X-STREAM-INF without URI")
	}
	return p, nil
}

func parseRendition(val string) (Rendition, error) {
	a, err := parseAttrs(val)
	if err != nil {
		return Rendition{}, fmt.Errorf("hls: EXT-X-MEDIA: %w", err)
	}
	if a["TYPE"] == "" {
		return Rendition{}, fmt.Errorf("hls: EXT-X-MEDIA: missing TYPE")
	}
	return Rendition{Type: a["TYPE"], GroupID: a["GROUP-ID"], Name: a["NAME"], URI: a["URI"], Default: a["DEFAULT"] == "YES"}, nil
}

func parseVariant(val string) (Variant, error) {
	a, err := parseAttrs(val)
	if err != nil {
		return Variant{}, fmt.Errorf("hls: EXT-X-STREAM-INF: %w", err)
	}
	v := Variant{Codecs: a["CODECS"], Audio: a["AUDIO"]}
	if s := a["BANDWIDTH"]; s != "" {
		if v.Bandwidth, err = strconv.Atoi(s); err != nil {
			return v, fmt.Errorf("hls: EXT-X-STREAM-INF: BANDWIDTH: %w", err)
		}
	}
	if s := a["RESOLUTION"]; s != "" {
		w, h, ok := strings.Cut(s, "x")
		if !ok {
			return v, fmt.Errorf("hls: EXT-X-STREAM-INF: bad RESOLUTION %q", s)
		}
		if v.Width, err = strconv.Atoi(w); err != nil {
			return v, fmt.Errorf("hls: EXT-X-STREAM-INF: RESOLUTION: %w", err)
		}
		if v.Height, err = strconv.Atoi(h); err != nil {
			return v, fmt.Errorf("hls: EXT-X-STREAM-INF: RESOLUTION: %w", err)
		}
	}
	return v, nil
}
