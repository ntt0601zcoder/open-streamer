package rtsp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
)

var (
	genSPS = []byte{0x67, 0x42, 0x00, 0x1f, 0x95, 0xa8, 0x14, 0x01, 0x6e, 0x40}
	genPPS = []byte{0x68, 0xce, 0x3c, 0x80}
)

func genTracks() []TrackDesc {
	return []TrackDesc{
		{Codec: CodecH264, Payload: 96, ClockRate: 90000, ParamSets: [][]byte{genSPS, genPPS}, ID: 7},
		{Codec: CodecH265, Payload: 97, ClockRate: 90000, ParamSets: [][]byte{{0x40, 1, 2}, {0x42, 1, 3}, {0x44, 1, 4}}, ID: 8},
		{
			Codec: CodecAAC, Payload: 98, ClockRate: 48000, Channels: 2, ID: 9,
			AAC: &AACFormat{Config: []byte{0x11, 0x90}, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3},
		},
	}
}

func TestBuildSDPRoundTrip(t *testing.T) {
	body, err := BuildSDP("127.0.0.1", genTracks())
	if err != nil {
		t.Fatal(err)
	}
	var sd sdp.SessionDescription
	if err := sd.Unmarshal(body); err != nil {
		t.Fatalf("pion rejects: %v\n%s", err, body)
	}
	for _, id := range []string{"=7", " 7", "=8", " 8", "=9", " 9"} {
		if strings.Contains(string(body), id+"\r\n") {
			t.Fatalf("ID leaked (%q):\n%s", id, body)
		}
	}
	d, err := ParseSDP(body, "rtsp://h/live/s/")
	if err != nil {
		t.Fatal(err)
	}
	if d.Control != "rtsp://h/live/s/" || len(d.Media) != 3 {
		t.Fatalf("%+v\n%s", d, body)
	}
	want := genTracks()
	for i, m := range d.Media {
		w := want[i]
		if m.Reason != "" || m.Codec != w.Codec || m.Payload != w.Payload || m.ClockRate != w.ClockRate ||
			m.Control != "rtsp://h/live/s/trackID="+string(rune('0'+i)) {
			t.Fatalf("media %d %+v\n%s", i, m, body)
		}
		if w.Codec != CodecAAC {
			if len(m.ParamSets) != len(w.ParamSets) {
				t.Fatalf("media %d sets %v", i, m.ParamSets)
			}
			for j := range w.ParamSets {
				if !bytes.Equal(m.ParamSets[j], w.ParamSets[j]) {
					t.Fatalf("media %d set %d", i, j)
				}
			}
		}
	}
	a := d.Media[2].AAC
	if a == nil || !bytes.Equal(a.Config, []byte{0x11, 0x90}) || a.SizeLength != 13 || a.IndexLength != 3 || a.IndexDeltaLength != 3 {
		t.Fatalf("aac %+v", a)
	}
	s := string(body)
	for _, line := range []string{
		"a=control:*\r\n", "a=range:npt=now-\r\n", "c=IN IP4 0.0.0.0\r\n", "t=0 0\r\n",
		"a=rtpmap:98 mpeg4-generic/48000/2\r\n", "a=rtpmap:97 H265/90000\r\n",
	} {
		if !strings.Contains(s, line) {
			t.Errorf("missing %q in\n%s", line, s)
		}
	}
}

func TestBuildSDPProfileLevelID(t *testing.T) {
	body, err := BuildSDP("127.0.0.1", genTracks()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "packetization-mode=1;profile-level-id=42001f;sprop-parameter-sets=") {
		t.Fatalf("%s", body)
	}
}

func TestBuildSDPErrors(t *testing.T) {
	for name, tr := range map[string][]TrackDesc{
		"none":      nil,
		"other":     {{Codec: CodecOther, Payload: 96, ClockRate: 90000}},
		"aac nil":   {{Codec: CodecAAC, Payload: 96, ClockRate: 48000, Channels: 2}},
		"h264 sets": {{Codec: CodecH264, Payload: 96, ClockRate: 90000}},
	} {
		if _, err := BuildSDP("127.0.0.1", tr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
