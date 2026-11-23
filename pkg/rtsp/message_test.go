package rtsp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

func br(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestRequestRoundTrip(t *testing.T) {
	req := &Request{
		Method: "DESCRIBE",
		URL:    "rtsp://cam.example/stream?x=1",
		Header: Header{"CSeq": {"2"}, "Accept": {"application/sdp"}},
	}
	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(bufio.NewReader(&buf), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != "DESCRIBE" || got.URL != req.URL || got.Header.Get("cseq") != "2" || got.Header.Get("Accept") != "application/sdp" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Body) != 0 {
		t.Fatalf("body %q", got.Body)
	}
}

func TestResponseRoundTripBody(t *testing.T) {
	resp := &Response{
		StatusCode: 200, Reason: "OK",
		Header: Header{"CSeq": {"3"}, "Content-Type": {"application/sdp"}, "Content-Length": {"999"}},
		Body:   []byte("v=0\r\n"),
	}
	var buf bytes.Buffer
	if err := resp.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "RTSP/1.0 200 OK\r\n") {
		t.Fatalf("wire %q", buf.String())
	}
	got, err := ReadResponse(bufio.NewReader(&buf), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != 200 || got.Reason != "OK" || string(got.Body) != "v=0\r\n" {
		t.Fatalf("got %+v", got)
	}
}

func TestHeaderCanonical(t *testing.T) {
	in := "RTSP/1.0 401 Unauthorized\r\ncseq: 1\r\nwww-authenticate: Basic realm=\"x\"\r\nrtp-info: url=a\r\ncontent-base: rtsp://h/\r\nx-custom-thing: 1\r\n\r\n"
	got, err := ReadResponse(br(in), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"CSeq", "WWW-Authenticate", "RTP-Info", "Content-Base", "X-Custom-Thing"} {
		if _, ok := got.Header[k]; !ok {
			t.Errorf("missing canonical key %q in %v", k, got.Header)
		}
	}
}

func TestFoldedHeader(t *testing.T) {
	in := "RTSP/1.0 200 OK\r\nCSeq: 1\r\nPublic: DESCRIBE,\r\n\tSETUP,\r\n PLAY\r\n\r\n"
	got, err := ReadResponse(br(in), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Header.Get("Public"); v != "DESCRIBE, SETUP, PLAY" {
		t.Fatalf("folded = %q", v)
	}
}

func TestNoContentLengthNoBody(t *testing.T) {
	got, err := ReadResponse(br("RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\nGARBAGE"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Body) != 0 {
		t.Fatalf("body %q", got.Body)
	}
}

func TestBareLF(t *testing.T) {
	got, err := ReadResponse(br("RTSP/1.0 200 OK\nCSeq: 7\n\n"), Limits{})
	if err != nil || got.Header.Get("CSeq") != "7" {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestLimitsEnforced(t *testing.T) {
	big := "RTSP/1.0 200 OK\r\nX-Pad: " + strings.Repeat("a", 5000) + "\r\n\r\n"
	if _, err := ReadResponse(br(big), Limits{HeaderBytes: 1024}); err == nil {
		t.Fatal("oversized header accepted")
	}
	if _, err := ReadResponse(br(big), Limits{}); err != nil {
		t.Fatalf("default limit rejected small header: %v", err)
	}
	// Many small lines also count.
	var sb strings.Builder
	sb.WriteString("RTSP/1.0 200 OK\r\n")
	for range 100 {
		sb.WriteString("X-A: bbbbbbbbbbbbbbbbbbbb\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := ReadResponse(br(sb.String()), Limits{HeaderBytes: 1000}); err == nil {
		t.Fatal("many lines accepted")
	}
	// A header with no end of line never grows unbounded.
	endless := "RTSP/1.0 200 OK\r\nX: " + strings.Repeat("z", 1<<20)
	if _, err := ReadResponse(br(endless), Limits{}); err == nil {
		t.Fatal("endless line accepted")
	}

	body := "RTSP/1.0 200 OK\r\nContent-Length: 2000\r\n\r\n" + strings.Repeat("b", 2000)
	if _, err := ReadResponse(br(body), Limits{BodyBytes: 1000}); err == nil {
		t.Fatal("oversized body accepted")
	}
	if got, err := ReadResponse(br(body), Limits{BodyBytes: 2000}); err != nil || len(got.Body) != 2000 {
		t.Fatalf("exact body: %v", err)
	}
	// Declared length is rejected before any read.
	huge := "RTSP/1.0 200 OK\r\nContent-Length: 99999999999\r\n\r\n"
	if _, err := ReadResponse(br(huge), Limits{}); err == nil {
		t.Fatal("huge length accepted")
	}
}

func TestBadContentLength(t *testing.T) {
	for _, v := range []string{"-1", "abc", "1x", ""} {
		in := "RTSP/1.0 200 OK\r\nContent-Length: " + v + "\r\n\r\n"
		if _, err := ReadResponse(br(in), Limits{}); err == nil {
			t.Errorf("Content-Length %q accepted", v)
		}
	}
}

func TestMalformedMessages(t *testing.T) {
	bad := []string{
		"",
		"HTTP/1.1 200 OK\r\n\r\n",
		"RTSP/1.0 abc OK\r\n\r\n",
		"RTSP/1.0 99 OK\r\n\r\n",
		"RTSP/1.0 200 OK\r\nnocolon\r\n\r\n",
		"RTSP/1.0 200 OK\r\n continuation-first\r\n\r\n",
		"RTSP/1.0 200 OK\r\nCSeq: 1\r\n", // truncated
		"RTSP/1.0 200 OK\r\nContent-Length: 5\r\n\r\nabc",
	}
	for _, in := range bad {
		if _, err := ReadResponse(br(in), Limits{}); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	for _, in := range []string{"", "DESCRIBE rtsp://x\r\n\r\n", "DESCRIBE rtsp://x HTTP/1.1\r\n\r\n"} {
		if _, err := ReadRequest(br(in), Limits{}); err == nil {
			t.Errorf("request accepted %q", in)
		}
	}
}

func TestWriteRejectsInjection(t *testing.T) {
	var buf bytes.Buffer
	r := &Request{Method: "PLAY", URL: "rtsp://x", Header: Header{"X": {"a\r\nEvil: 1"}}}
	if err := r.Write(&buf); err == nil {
		t.Fatal("header injection accepted")
	}
	r = &Request{Method: "PLAY", URL: "rtsp://x y"}
	if err := r.Write(&buf); err == nil {
		t.Fatal("space in URL accepted")
	}
	r = &Request{Method: "", URL: "rtsp://x"}
	if err := r.Write(&buf); err == nil {
		t.Fatal("empty method accepted")
	}
	resp := &Response{StatusCode: 200, Reason: "O\nK"}
	if err := resp.Write(&buf); err == nil {
		t.Fatal("reason injection accepted")
	}
	resp = &Response{StatusCode: 5}
	if err := resp.Write(&buf); err == nil {
		t.Fatal("bad status accepted")
	}
	r = &Request{Method: "PLAY", URL: "rtsp://x", Header: Header{"Bad Key": {"1"}}}
	if err := r.Write(&buf); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestReadTwoMessages(t *testing.T) {
	in := br("RTSP/1.0 200 OK\r\nContent-Length: 3\r\n\r\nabcRTSP/1.0 404 Not Found\r\n\r\n")
	a, err := ReadResponse(in, Limits{})
	if err != nil || string(a.Body) != "abc" {
		t.Fatalf("first: %v %+v", err, a)
	}
	b, err := ReadResponse(in, Limits{})
	if err != nil || b.StatusCode != 404 || b.Reason != "Not Found" {
		t.Fatalf("second: %v %+v", err, b)
	}
}

func TestParseTransport(t *testing.T) {
	cases := []struct {
		in   string
		want Transport
		out  string
	}{
		{
			"RTP/AVP/TCP;unicast;interleaved=0-1",
			Transport{Protocol: "RTP/AVP/TCP", Unicast: true, Interleaved: [2]int{0, 1}},
			"RTP/AVP/TCP;unicast;interleaved=0-1",
		},
		{
			"RTP/AVP;unicast;client_port=5000-5001;server_port=6970-6971;source=10.0.0.1;ssrc=DEADBEEF",
			Transport{
				Protocol: "RTP/AVP", Unicast: true,
				ClientPort: [2]int{5000, 5001}, ServerPort: [2]int{6970, 6971},
				Source: "10.0.0.1", SSRC: 0xDEADBEEF, HasSSRC: true,
			},
			"RTP/AVP;unicast;client_port=5000-5001;server_port=6970-6971;source=10.0.0.1;ssrc=DEADBEEF",
		},
		{
			"RTP/AVP/TCP;interleaved=2-3;unicast, RTP/AVP;unicast;client_port=1-2",
			Transport{Protocol: "RTP/AVP/TCP", Unicast: true, Interleaved: [2]int{2, 3}},
			"RTP/AVP/TCP;unicast;interleaved=2-3",
		},
		{
			"RTP/AVP;multicast;destination=239.1.1.1;port=5000-5001",
			Transport{Protocol: "RTP/AVP", Multicast: true, Destination: "239.1.1.1"},
			"RTP/AVP;multicast;destination=239.1.1.1",
		},
		{
			"RTP/AVP/TCP;unicast;interleaved=4",
			Transport{Protocol: "RTP/AVP/TCP", Unicast: true, Interleaved: [2]int{4, 5}},
			"RTP/AVP/TCP;unicast;interleaved=4-5",
		},
		{
			"RTP/AVP;unicast;client_port=5000-5001;server_port=6970",
			Transport{Protocol: "RTP/AVP", Unicast: true, ClientPort: [2]int{5000, 5001}, ServerPort: [2]int{6970, 6971}},
			"RTP/AVP;unicast;client_port=5000-5001;server_port=6970-6971",
		},
		{
			"RTP/AVP;unicast;server_port=0",
			Transport{Protocol: "RTP/AVP", Unicast: true},
			"RTP/AVP;unicast",
		},
		{
			"RTP/AVP;unicast;server_port=65535",
			Transport{Protocol: "RTP/AVP", Unicast: true, ServerPort: [2]int{65535, 65535}},
			"RTP/AVP;unicast;server_port=65535-65535",
		},
		{
			"RTP/AVP;unicast;ssrc=1A2B",
			Transport{Protocol: "RTP/AVP", Unicast: true, SSRC: 0x1A2B, HasSSRC: true},
			"RTP/AVP;unicast;ssrc=00001A2B",
		},
	}
	for _, c := range cases {
		got, err := ParseTransport(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
		if s := got.String(); s != c.out {
			t.Errorf("%q: String %q want %q", c.in, s, c.out)
		}
	}
	for _, bad := range []string{"", ";unicast", "RTP/AVP;interleaved=a-b", "RTP/AVP;interleaved=0-999", "RTP/AVP;client_port=70000-70001", "RTP/AVP;ssrc=zz", "RTP/AVP;ssrc=123456789"} {
		if _, err := ParseTransport(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseTransports(t *testing.T) {
	got, err := ParseTransports(`RTP/AVP/UDP;unicast;client_port=5000-5001;mode="PLAY", ` +
		`rtp/avp/tcp;unicast;interleaved=2-3;mode=record,RTP/AVP;destination=10.0.0.9`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Transport{
		{Protocol: "RTP/AVP/UDP", Unicast: true, ClientPort: [2]int{5000, 5001}, Mode: "PLAY"},
		{Protocol: "rtp/avp/tcp", Unicast: true, Interleaved: [2]int{2, 3}, Mode: "record"},
		{Protocol: "RTP/AVP", Destination: "10.0.0.9"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d offers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("offer %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if s := got[1].String(); s != "rtp/avp/tcp;unicast;interleaved=2-3;mode=record" {
		t.Errorf("String %q", s)
	}
	// An offer naming no delivery type renders none.
	if s := got[2].String(); s != "RTP/AVP;destination=10.0.0.9" {
		t.Errorf("String %q", s)
	}
	for _, bad := range []string{"", ",", "RTP/AVP;client_port=x", "RTP/AVP;client_port=x, ;unicast"} {
		if _, err := ParseTransports(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestParseTransportsSkipsMalformed checks that a malformed offer is skipped
// while another parses, and the error comes only when none does.
func TestParseTransportsSkipsMalformed(t *testing.T) {
	got, err := ParseTransports("RTP/AVP;client_port=x, RTP/AVP/TCP;unicast;interleaved=0-1, ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Protocol != "RTP/AVP/TCP" || got[0].Interleaved != [2]int{0, 1} {
		t.Fatalf("got %+v", got)
	}
	if _, err := ParseTransports("RTP/AVP;client_port=x, RTP/AVP;interleaved=999"); err == nil {
		t.Error("accepted a header with no valid offer")
	}
}

func TestParseSession(t *testing.T) {
	cases := []struct {
		in   string
		want Session
	}{
		{"12345678", Session{ID: "12345678"}},
		{"ABCDEF;timeout=30", Session{ID: "ABCDEF", Timeout: 30 * time.Second}},
		{" sess-1 ; timeout=60 ", Session{ID: "sess-1", Timeout: time.Minute}},
		{"x;foo=bar", Session{ID: "x"}},
	}
	for _, c := range cases {
		got, err := ParseSession(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: %v %+v", c.in, err, got)
		}
	}
	for _, bad := range []string{"", ";timeout=5", "a;timeout=zz", "a;timeout=-3"} {
		if _, err := ParseSession(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseRTPInfo(t *testing.T) {
	got := ParseRTPInfo("url=rtsp://h/trackID=1;seq=100;rtptime=9000,url=rtsp://h/trackID=2;rtptime=4294967295, url=rtsp://h/t3;seq=7")
	want := []RTPInfo{
		{URL: "rtsp://h/trackID=1", Seq: 100, RTPTime: 9000, HasSeq: true, HasTime: true},
		{URL: "rtsp://h/trackID=2", RTPTime: 4294967295, HasTime: true},
		{URL: "rtsp://h/t3", Seq: 7, HasSeq: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v want %+v", i, got[i], want[i])
		}
	}
	if r := ParseRTPInfo(""); len(r) != 0 {
		t.Fatalf("empty: %+v", r)
	}
	r := ParseRTPInfo("url=u;seq=99999;rtptime=-1")
	if len(r) != 1 || r[0].HasSeq || r[0].HasTime {
		t.Fatalf("out of range accepted: %+v", r)
	}
	// A comma inside a URL does not split the entry.
	r = ParseRTPInfo("url=rtsp://h/a,b;seq=1")
	if len(r) != 1 || r[0].URL != "rtsp://h/a,b" {
		t.Fatalf("comma url: %+v", r)
	}
}
