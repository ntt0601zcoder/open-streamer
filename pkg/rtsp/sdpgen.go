package rtsp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

// TrackDesc describes one track of a server-side SDP.
type TrackDesc struct {
	Codec     Codec      // CodecH264, CodecH265 or CodecAAC.
	Payload   uint8      // RTP payload type.
	ClockRate uint32     // 90000 for video, the AAC core rate for audio.
	Channels  int        // AAC channel count.
	ParamSets [][]byte   // H.264 SPS, PPS; H.265 VPS, SPS, PPS.
	AAC       *AACFormat // AAC Config (ASC) and lengths.
	// ID is opaque to this package and never written into the SDP.
	ID int
}

// BuildSDP renders a session description for tracks. Each media carries
// a=control:trackID=<index>, the position in tracks; the session carries
// a=control:* and a=range:npt=now-. origin is the o= line's address.
func BuildSDP(origin string, tracks []TrackDesc) ([]byte, error) {
	if len(tracks) == 0 {
		return nil, errors.New("rtsp: build sdp: no tracks")
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("rtsp: build sdp: session id: %w", err)
	}
	sd := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      binary.BigEndian.Uint64(id[:]) >> 1,
			SessionVersion: 0,
			NetworkType:    "IN",
			AddressType:    originAddrType(origin),
			UnicastAddress: origin,
		},
		SessionName:           "Open Streamer",
		ConnectionInformation: &sdp.ConnectionInformation{NetworkType: "IN", AddressType: "IP4", Address: &sdp.Address{Address: "0.0.0.0"}},
		TimeDescriptions:      []sdp.TimeDescription{{}},
		Attributes:            []sdp.Attribute{{Key: "control", Value: "*"}, {Key: "range", Value: "npt=now-"}},
	}
	for i, t := range tracks {
		md, err := buildMedia(i, t)
		if err != nil {
			return nil, err
		}
		sd.MediaDescriptions = append(sd.MediaDescriptions, md)
	}
	b, err := sd.Marshal()
	if err != nil {
		return nil, fmt.Errorf("rtsp: build sdp: %w", err)
	}
	return b, nil
}

func buildMedia(i int, t TrackDesc) (*sdp.MediaDescription, error) {
	pt := strconv.Itoa(int(t.Payload))
	md := &sdp.MediaDescription{
		MediaName: sdp.MediaName{Protos: []string{"RTP", "AVP"}, Formats: []string{pt}},
	}
	var rtpmap, fmtp string
	switch t.Codec {
	case CodecH264:
		sets := groupSets(t.ParamSets, func(n []byte) byte { return n[0] & 0x1f })
		sps, pps := sets[h264NALSPS], sets[h264NALPPS]
		if len(sps) == 0 || len(pps) == 0 || len(sps[0]) < 4 {
			return nil, fmt.Errorf("rtsp: build sdp: track %d: H.264 needs SPS and PPS", i)
		}
		md.MediaName.Media = "video"
		rtpmap = "H264/" + strconv.FormatUint(uint64(t.ClockRate), 10)
		fmtp = fmt.Sprintf("packetization-mode=1;profile-level-id=%s;sprop-parameter-sets=%s",
			hex.EncodeToString(sps[0][1:4]), b64All(t.ParamSets, func(n []byte) bool {
				k := n[0] & 0x1f
				return k == h264NALSPS || k == h264NALPPS
			}))
	case CodecH265:
		sets := groupSets(t.ParamSets, func(n []byte) byte { return (n[0] >> 1) & 0x3f })
		vps, sps, pps := sets[h265NALVPS], sets[h265NALSPS], sets[h265NALPPS]
		if len(vps) == 0 || len(sps) == 0 || len(pps) == 0 {
			return nil, fmt.Errorf("rtsp: build sdp: track %d: H.265 needs VPS, SPS and PPS", i)
		}
		md.MediaName.Media = "video"
		rtpmap = "H265/" + strconv.FormatUint(uint64(t.ClockRate), 10)
		fmtp = fmt.Sprintf("sprop-vps=%s;sprop-sps=%s;sprop-pps=%s", b64Join(vps), b64Join(sps), b64Join(pps))
	case CodecAAC:
		if t.AAC == nil || len(t.AAC.Config) == 0 || t.Channels <= 0 || t.ClockRate == 0 {
			return nil, fmt.Errorf("rtsp: build sdp: track %d: AAC needs config, rate and channels", i)
		}
		md.MediaName.Media = "audio"
		rtpmap = fmt.Sprintf("mpeg4-generic/%d/%d", t.ClockRate, t.Channels)
		fmtp = fmt.Sprintf("streamtype=5;profile-level-id=1;mode=AAC-hbr;sizelength=%d;indexlength=%d;indexdeltalength=%d;config=%s",
			t.AAC.SizeLength, t.AAC.IndexLength, t.AAC.IndexDeltaLength, hex.EncodeToString(t.AAC.Config))
	case CodecOther:
		return nil, fmt.Errorf("rtsp: build sdp: track %d: unsupported codec", i)
	default:
		return nil, fmt.Errorf("rtsp: build sdp: track %d: unknown codec", i)
	}
	md.Attributes = []sdp.Attribute{
		{Key: "rtpmap", Value: pt + " " + rtpmap},
		{Key: "fmtp", Value: pt + " " + fmtp},
		{Key: "control", Value: "trackID=" + strconv.Itoa(i)},
	}
	return md, nil
}

const (
	h264NALSPS = 7
	h264NALPPS = 8
	h265NALVPS = 32
	h265NALSPS = 33
	h265NALPPS = 34
)

// groupSets buckets the non-empty sets by NAL unit type, keeping order.
func groupSets(sets [][]byte, typ func([]byte) byte) map[byte][][]byte {
	out := map[byte][][]byte{}
	for _, n := range sets {
		if len(n) > 0 {
			k := typ(n)
			out[k] = append(out[k], n)
		}
	}
	return out
}

func b64Join(sets [][]byte) string {
	parts := make([]string, len(sets))
	for i, n := range sets {
		parts[i] = base64.StdEncoding.EncodeToString(n)
	}
	return strings.Join(parts, ",")
}

// b64All joins, in Config order, the non-empty sets accepted by keep.
func b64All(sets [][]byte, keep func([]byte) bool) string {
	var sel [][]byte
	for _, n := range sets {
		if len(n) > 0 && keep(n) {
			sel = append(sel, n)
		}
	}
	return b64Join(sel)
}

func originAddrType(origin string) string {
	if ip := net.ParseIP(origin); ip != nil && ip.To4() == nil {
		return "IP6"
	}
	return "IP4"
}
