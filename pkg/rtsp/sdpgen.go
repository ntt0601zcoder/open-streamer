package rtsp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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
			AddressType:    "IP4",
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
		if len(t.ParamSets) < 2 || len(t.ParamSets[0]) < 4 {
			return nil, fmt.Errorf("rtsp: build sdp: track %d: H.264 needs SPS and PPS", i)
		}
		md.MediaName.Media = "video"
		rtpmap = "H264/" + strconv.FormatUint(uint64(t.ClockRate), 10)
		fmtp = fmt.Sprintf("packetization-mode=1;profile-level-id=%s;sprop-parameter-sets=%s,%s",
			hex.EncodeToString(t.ParamSets[0][1:4]), b64(t.ParamSets[0]), b64(t.ParamSets[1]))
	case CodecH265:
		if len(t.ParamSets) < 3 {
			return nil, fmt.Errorf("rtsp: build sdp: track %d: H.265 needs VPS, SPS and PPS", i)
		}
		md.MediaName.Media = "video"
		rtpmap = "H265/" + strconv.FormatUint(uint64(t.ClockRate), 10)
		fmtp = fmt.Sprintf("sprop-vps=%s;sprop-sps=%s;sprop-pps=%s",
			b64(t.ParamSets[0]), b64(t.ParamSets[1]), b64(t.ParamSets[2]))
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

func b64(b []byte) string { return strings.TrimSpace(base64.StdEncoding.EncodeToString(b)) }
