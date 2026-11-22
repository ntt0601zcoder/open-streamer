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
		{Codec: CodecH264, Payload: 96, ClockRate: 90000, ParamSets: [][]byte{genSPS, genPPS}, ID: 4242},
		{Codec: CodecH265, Payload: 97, ClockRate: 90000, ParamSets: [][]byte{{0x40, 1, 2}, {0x42, 1, 3}, {0x44, 1, 4}}, ID: 4243},
		{
			Codec: CodecAAC, Payload: 98, ClockRate: 48000, Channels: 2, ID: 4244,
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
	for _, id := range []string{"4242", "4243", "4244"} {
		if strings.Contains(string(body), id) {
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

func TestBuildSDPMultipleSets(t *testing.T) {
	pps2 := []byte{0x68, 0xee, 0x3c, 0x80}
	h265 := [][]byte{{0x40, 1, 2}, {0x42, 1, 3}, {0x44, 1, 4}, {0x44, 1, 5}}
	cases := map[string]TrackDesc{
		"h264": {Codec: CodecH264, Payload: 96, ClockRate: 90000, ParamSets: [][]byte{genPPS, genSPS, pps2}},
		"h265": {Codec: CodecH265, Payload: 96, ClockRate: 90000, ParamSets: [][]byte{h265[2], h265[0], h265[3], h265[1]}},
	}
	for name, tr := range cases {
		body, err := BuildSDP("127.0.0.1", []TrackDesc{tr})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := ParseSDP(body, "rtsp://h/x/")
		if err != nil || len(d.Media) != 1 {
			t.Fatalf("%s: %v\n%s", name, err, body)
		}
		got := d.Media[0].ParamSets
		wantN := 3
		if name == "h265" {
			wantN = 4
		}
		if len(got) != wantN {
			t.Fatalf("%s: %d sets\n%s", name, len(got), body)
		}
		if name == "h264" && !bytes.Equal(got[2], pps2) {
			t.Fatalf("h264 sets %x", got)
		}
		if name == "h265" && !bytes.Equal(got[3], h265[3]) {
			t.Fatalf("h265 sets %x", got)
		}
	}
	// Missing kinds are rejected by NAL type, not position.
	for name, ps := range map[string][][]byte{"two pps": {genPPS, genPPS}, "two sps": {genSPS, genSPS}} {
		if _, err := BuildSDP("h", []TrackDesc{{Codec: CodecH264, Payload: 96, ClockRate: 90000, ParamSets: ps}}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := BuildSDP("h", []TrackDesc{{Codec: CodecH265, Payload: 96, ClockRate: 90000, ParamSets: h265[:2]}}); err == nil {
		t.Error("h265 without pps: no error")
	}
}

func TestBuildSDPOriginFamily(t *testing.T) {
	for origin, want := range map[string]string{
		"127.0.0.1": "IN IP4 127.0.0.1", "::1": "IN IP6 ::1", "example.com": "IN IP4 example.com",
	} {
		body, err := BuildSDP(origin, genTracks()[:1])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want+"\r\n") {
			t.Errorf("%s: want %q in\n%s", origin, want, body)
		}
	}
}
