package rtsp

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

const (
	maxSDPBytes = 1 << 20
	maxMedia    = 16
)

// Codec identifies how a media's RTP payload is depacketised.
type Codec uint8

// Codecs the depacketiser understands; anything else is CodecOther.
const (
	CodecOther Codec = iota
	CodecH264
	CodecH265
	CodecAAC
)

// AACFormat holds the mpeg4-generic (RFC 3640) parameters of an AAC media.
type AACFormat struct {
	Config           []byte
	SizeLength       int
	IndexLength      int
	IndexDeltaLength int
	CTSDeltaLength   int
	DTSDeltaLength   int
	// RAPLength is 1 when randomAccessIndication is set, else 0.
	RAPLength int
	AuxLength int
}

// Media is one SDP media section.
type Media struct {
	// Index is the position in the SDP and the track number for every media,
	// supported or not.
	Index int
	// Control is the absolute SETUP URL, userinfo removed.
	Control   string
	Codec     Codec
	Payload   uint8
	ClockRate uint32
	// ParamSets are the decoded sprop parameter sets: SPS, PPS for H.264;
	// VPS, SPS, PPS for H.265.
	ParamSets [][]byte
	// AAC is set for CodecAAC.
	AAC *AACFormat
	// Reason says why the media is unsupported; empty when it is usable.
	Reason string
}

// Description is a parsed SDP.
type Description struct {
	// Control is the absolute session-level control URL.
	Control string
	Media   []Media
}

var staticPayloads = map[int]string{
	0: "PCMU", 3: "GSM", 4: "G723", 5: "DVI4", 6: "DVI4", 7: "LPC", 8: "PCMA",
	9: "G722", 10: "L16", 11: "L16", 12: "QCELP", 13: "CN", 14: "MPA",
	15: "G728", 16: "DVI4", 17: "DVI4", 18: "G729", 25: "CelB", 26: "JPEG",
	28: "nv", 31: "H261", 32: "MPV", 33: "MP2T", 34: "H263",
}

// ParseSDP parses an SDP body. base is the URL control attributes resolve
// against (Content-Base, else Content-Location, else the request URL). At most
// 16 media are returned; unsupported ones carry a Reason.
func ParseSDP(body []byte, base string) (Description, error) {
	if len(body) > maxSDPBytes {
		return Description{}, errors.New("rtsp: parse sdp: body too large")
	}
	var sd sdp.SessionDescription
	if err := sd.Unmarshal(body); err != nil {
		// The parser's text may quote server-echoed values, such as a URL
		// with credentials: it is not passed on.
		return Description{}, errors.New("rtsp: parse sdp: invalid syntax")
	}
	d := Description{Control: ResolveControl(base, attr(sd.Attributes, "control"))}
	for i, md := range sd.MediaDescriptions {
		if i >= maxMedia {
			break
		}
		m := parseMedia(md)
		m.Index = i
		m.Control = ResolveControl(base, attr(md.Attributes, "control"))
		d.Media = append(d.Media, m)
	}
	return d, nil
}

func attr(as []sdp.Attribute, key string) string {
	for _, a := range as {
		if strings.EqualFold(a.Key, key) {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

func unsupported(m Media, format string, args ...any) Media {
	m.Codec = CodecOther
	m.Reason = fmt.Sprintf(format, args...)
	return m
}

func parseMedia(md *sdp.MediaDescription) Media {
	var m Media
	kind := strings.ToLower(md.MediaName.Media)
	if len(md.MediaName.Formats) == 0 {
		return unsupported(m, "%s media without a format", kind)
	}
	pt, err := strconv.Atoi(md.MediaName.Formats[0])
	if err != nil || pt < 0 || pt > 127 {
		return unsupported(m, "invalid payload type")
	}
	m.Payload = uint8(pt)

	var enc string
	for _, a := range md.Attributes {
		if !strings.EqualFold(a.Key, "rtpmap") {
			continue
		}
		f := strings.Fields(a.Value)
		if len(f) != 2 || f[0] != md.MediaName.Formats[0] {
			continue
		}
		parts := strings.Split(f[1], "/")
		enc = parts[0]
		if len(parts) > 1 {
			if c, perr := strconv.ParseUint(parts[1], 10, 32); perr == nil {
				m.ClockRate = uint32(c)
			}
		}
		break
	}
	if kind != "video" && kind != "audio" {
		return unsupported(m, "%s media not supported", kind)
	}
	if enc == "" {
		if n, ok := staticPayloads[pt]; ok {
			return unsupported(m, "%s not supported", n)
		}
		return unsupported(m, "payload %d has no rtpmap", pt)
	}

	fmtp := parseFmtp(md.Attributes, md.MediaName.Formats[0])
	switch strings.ToUpper(enc) {
	case "H264":
		return parseH26x(m, kind, CodecH264, fmtp)
	case "H265", "HEVC":
		return parseH26x(m, kind, CodecH265, fmtp)
	case "MPEG4-GENERIC":
		return parseAAC(m, kind, fmtp)
	}
	return unsupported(m, "%s not supported", enc)
}

func parseFmtp(as []sdp.Attribute, pt string) map[string]string {
	out := map[string]string{}
	for _, a := range as {
		if !strings.EqualFold(a.Key, "fmtp") {
			continue
		}
		p, rest, ok := strings.Cut(strings.TrimSpace(a.Value), " ")
		if !ok || p != pt {
			continue
		}
		for _, kv := range strings.Split(rest, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
				out[k] = strings.TrimSpace(v)
			}
		}
		break
	}
	return out
}

func decodeSets(v string) [][]byte {
	var out [][]byte
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(s); err != nil {
				continue
			}
		}
		if len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

func parseH26x(m Media, kind string, c Codec, f map[string]string) Media {
	if kind != "video" {
		return unsupported(m, "H.26x carried in audio media")
	}
	if m.ClockRate != 90000 {
		return unsupported(m, "video clock %d", m.ClockRate)
	}
	if f["packetization-mode"] == "2" {
		return unsupported(m, "packetization-mode 2 not supported")
	}
	if n, err := strconv.Atoi(f["sprop-max-don-diff"]); err == nil && n > 0 {
		return unsupported(m, "sprop-max-don-diff %d not supported", n)
	}
	m.Codec = c
	if c == CodecH264 {
		m.ParamSets = decodeSets(f["sprop-parameter-sets"])
		return m
	}
	for _, k := range []string{"sprop-vps", "sprop-sps", "sprop-pps"} {
		m.ParamSets = append(m.ParamSets, decodeSets(f[k])...)
	}
	return m
}

func parseAAC(m Media, kind string, f map[string]string) Media {
	if kind != "audio" {
		return unsupported(m, "mpeg4-generic carried in video media")
	}
	if m.ClockRate == 0 {
		return unsupported(m, "mpeg4-generic without a clock rate")
	}
	mode := strings.ToLower(f["mode"])
	if mode != "aac-hbr" && mode != "aac-lbr" {
		return unsupported(m, "mpeg4-generic mode %q not supported", f["mode"])
	}
	if n, err := strconv.Atoi(f["sizelength"]); err != nil || n <= 0 {
		return unsupported(m, "mpeg4-generic without a valid sizelength")
	}
	cfg, err := hex.DecodeString(f["config"])
	if err != nil || len(cfg) == 0 {
		return unsupported(m, "mpeg4-generic with invalid config")
	}
	a := &AACFormat{Config: cfg}
	for _, e := range []struct {
		key string
		dst *int
	}{
		{"sizelength", &a.SizeLength},
		{"indexlength", &a.IndexLength},
		{"indexdeltalength", &a.IndexDeltaLength},
		{"ctsdeltalength", &a.CTSDeltaLength},
		{"dtsdeltalength", &a.DTSDeltaLength},
		{"randomaccessindication", &a.RAPLength},
		{"auxiliarydatasizelength", &a.AuxLength},
	} {
		v, ok := f[e.key]
		if !ok {
			continue
		}
		n, perr := strconv.Atoi(v)
		limit := 32
		if e.key == "randomaccessindication" {
			limit = 1
		}
		if perr != nil || n < 0 || n > limit {
			return unsupported(m, "mpeg4-generic invalid %s", e.key)
		}
		*e.dst = n
	}
	m.Codec = CodecAAC
	m.AAC = a
	return m
}
