package rtsp

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

const sdpHead = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=x\r\nt=0 0\r\n"

func TestParseSDPH264AAC(t *testing.T) {
	body := []byte(sdpHead +
		"a=control:*\r\n" +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 packetization-mode=1; sprop-parameter-sets=Z0IAH5WoFAFuQA==,aM48gA==\r\na=control:trackID=0\r\n" +
		"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 mpeg4-generic/48000/2\r\n" +
		"a=fmtp:97 profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1190\r\na=control:trackID=1\r\n")
	d, err := ParseSDP(body, "rtsp://h/live/")
	if err != nil {
		t.Fatal(err)
	}
	if d.Control != "rtsp://h/live/" || len(d.Media) != 2 {
		t.Fatalf("%+v", d)
	}
	v, a := d.Media[0], d.Media[1]
	if v.Codec != CodecH264 || v.Payload != 96 || v.ClockRate != 90000 || v.Index != 0 ||
		v.Control != "rtsp://h/live/trackID=0" || len(v.ParamSets) != 2 || v.Reason != "" {
		t.Fatalf("video %+v", v)
	}
	if a.Codec != CodecAAC || a.Index != 1 || a.ClockRate != 48000 || a.AAC == nil ||
		a.AAC.SizeLength != 13 || a.AAC.IndexLength != 3 || a.AAC.IndexDeltaLength != 3 ||
		!bytes.Equal(a.AAC.Config, []byte{0x11, 0x90}) {
		t.Fatalf("audio %+v %+v", a, a.AAC)
	}
}

func TestParseSDPH265(t *testing.T) {
	body := []byte(sdpHead +
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\n" +
		"a=fmtp:96 sprop-vps=QAEMAf//AWAAAAMAAAMAAAMAAAMAWKwJ;sprop-sps=QgEBAWAAAAMAAAMAAAMAAAMAWKABgIAtfmLK;sprop-pps=RAHA8vA8kA==\r\n" +
		"m=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/44100/2\r\na=fmtp:97 mode=AAC-lbr;sizelength=6;indexlength=2;indexdeltalength=2;config=1210\r\n")
	d, err := ParseSDP(body, "rtsp://h/x")
	if err != nil {
		t.Fatal(err)
	}
	v := d.Media[0]
	if v.Codec != CodecH265 || len(v.ParamSets) != 3 || v.Reason != "" {
		t.Fatalf("%+v", v)
	}
	if v.ParamSets[0][0]>>1&0x3f != 32 || v.ParamSets[1][0]>>1&0x3f != 33 || v.ParamSets[2][0]>>1&0x3f != 34 {
		t.Fatalf("order %x", v.ParamSets)
	}
	if d.Media[1].Codec != CodecAAC || d.Media[1].Control != "rtsp://h/x" {
		t.Fatalf("%+v", d.Media[1])
	}
}

func TestParseSDPCameras(t *testing.T) {
	// Hikvision style: Content-Base with trailing slash.
	hik := []byte(sdpHead + "a=control:rtsp://10.0.0.5/Streaming/Channels/101/\r\n" +
		"m=video 0 RTP/AVP 96\r\na=control:trackID=1\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 profile-level-id=420029; packetization-mode=1; sprop-parameter-sets=Z0IAH5WoFAFuQA==,aM48gA==\r\n")
	d, err := ParseSDP(hik, "rtsp://10.0.0.5/Streaming/Channels/101/")
	if err != nil || d.Media[0].Control != "rtsp://10.0.0.5/Streaming/Channels/101/trackID=1" ||
		d.Control != "rtsp://10.0.0.5/Streaming/Channels/101/" {
		t.Fatalf("%v %+v", err, d)
	}
	// Dahua style: query in the base.
	dah := []byte(sdpHead + "m=video 0 RTP/AVP 96\r\na=control:trackID=0\r\na=rtpmap:96 H264/90000\r\n")
	d, err = ParseSDP(dah, "rtsp://u:p@h/cam/realmonitor?channel=1&subtype=0/")
	if err != nil || d.Media[0].Control != "rtsp://h/cam/realmonitor?channel=1&subtype=0/trackID=0" {
		t.Fatalf("%v %+v", err, d)
	}
	// Absolute control with credentials.
	abs := []byte(sdpHead + "m=video 0 RTP/AVP 96\r\na=control:rtsp://u:p@h:554/v\r\na=rtpmap:96 H264/90000\r\n")
	d, err = ParseSDP(abs, "rtsp://h/x")
	if err != nil || d.Media[0].Control != "rtsp://h:554/v" {
		t.Fatalf("%v %+v", err, d)
	}
}

func TestParseSDPUnsupported(t *testing.T) {
	cases := []struct{ name, media, reason string }{
		{"pcma", "m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n", "PCMA"},
		{"static no rtpmap", "m=audio 0 RTP/AVP 0\r\n", "PCMU"},
		{"opus", "m=audio 0 RTP/AVP 111\r\na=rtpmap:111 opus/48000/2\r\n", "opus"},
		{"pm2", "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=2\r\n", "packetization-mode 2"},
		{"don", "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 sprop-max-don-diff=3\r\n", "sprop-max-don-diff"},
		{"video clock", "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/48000\r\n", "video clock 48000"},
		{"h265 clock", "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/12000\r\n", "video clock 12000"},
		{"aac mode", "m=audio 0 RTP/AVP 97\r\na=rtpmap:97 mpeg4-generic/48000/2\r\na=fmtp:97 mode=CELP-cbr;sizelength=6;config=1210\r\n", "mode"},
		{"aac no sizelength", "m=audio 0 RTP/AVP 97\r\na=rtpmap:97 mpeg4-generic/48000/2\r\na=fmtp:97 mode=AAC-hbr;config=1210\r\n", "sizelength"},
		{"aac bad config", "m=audio 0 RTP/AVP 97\r\na=rtpmap:97 mpeg4-generic/48000/2\r\na=fmtp:97 mode=AAC-hbr;sizelength=13;config=zz\r\n", "config"},
		{"application", "m=application 0 RTP/AVP 107\r\na=rtpmap:107 vnd.onvif.metadata/90000\r\n", "application"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseSDP([]byte(sdpHead+c.media), "rtsp://h/a")
			if err != nil || len(d.Media) != 1 {
				t.Fatalf("%v %+v", err, d)
			}
			m := d.Media[0]
			if m.Codec != CodecOther && m.Reason == "" {
				t.Fatalf("supported: %+v", m)
			}
			if m.Reason == "" || !strings.Contains(m.Reason, c.reason) {
				t.Fatalf("reason %q want %q", m.Reason, c.reason)
			}
		})
	}
}

func TestParseSDPBadParamSetSkipped(t *testing.T) {
	body := []byte(sdpHead + "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 sprop-parameter-sets=!!!,aM48gA==\r\n")
	d, err := ParseSDP(body, "rtsp://h/a")
	if err != nil || d.Media[0].Codec != CodecH264 || len(d.Media[0].ParamSets) != 1 {
		t.Fatalf("%v %+v", err, d.Media)
	}
}

func TestParseSDPHEAAC(t *testing.T) {
	// HE-AAC: ASC 2b11 8800 carries SBR; RTP clock is the SBR rate.
	body := []byte(sdpHead + "m=audio 0 RTP/AVP 97\r\na=rtpmap:97 mpeg4-generic/48000/2\r\n" +
		"a=fmtp:97 mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=2B118800\r\n")
	d, err := ParseSDP(body, "rtsp://h/a")
	if err != nil || d.Media[0].Codec != CodecAAC || d.Media[0].ClockRate != 48000 ||
		!bytes.Equal(d.Media[0].AAC.Config, []byte{0x2b, 0x11, 0x88, 0x00}) {
		t.Fatalf("%v %+v", err, d.Media[0])
	}
}

func TestParseSDPHostile(t *testing.T) {
	var b strings.Builder
	b.WriteString(sdpHead)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:t%d\r\n", i)
	}
	d, err := ParseSDP([]byte(b.String()), "rtsp://h/a")
	if err != nil || len(d.Media) != maxMedia {
		t.Fatalf("%v %d", err, len(d.Media))
	}
	if _, err = ParseSDP(bytes.Repeat([]byte("a"), 1<<20+1), "rtsp://h/a"); err == nil {
		t.Fatal("oversized accepted")
	}
	if _, err = ParseSDP([]byte("garbage"), "rtsp://h/a"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestParseSDPAACFlagsAndSize(t *testing.T) {
	mk := func(f string) []byte {
		return []byte(sdpHead + "m=audio 0 RTP/AVP 97\r\na=rtpmap:97 mpeg4-generic/48000/2\r\na=fmtp:97 mode=AAC-hbr;config=1190;" + f + "\r\n")
	}
	d, err := ParseSDP(mk("sizelength=13;RandomAccessIndication=1"), "rtsp://h/a")
	if err != nil || d.Media[0].AAC == nil || d.Media[0].AAC.RAPLength != 1 {
		t.Fatalf("%v %+v", err, d.Media[0])
	}
	d, _ = ParseSDP(mk("sizelength=13;RandomAccessIndication=2"), "rtsp://h/a")
	if d.Media[0].Reason == "" {
		t.Fatal("RAI=2 accepted")
	}
	d, _ = ParseSDP(mk("sizelength=0"), "rtsp://h/a")
	if d.Media[0].Codec != CodecOther || !strings.Contains(d.Media[0].Reason, "sizelength") {
		t.Fatalf("%+v", d.Media[0])
	}
}
