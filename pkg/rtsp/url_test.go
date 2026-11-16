package rtsp

import "testing"

func TestControlURLs(t *testing.T) {
	cases := []struct{ name, base, ctl, want string }{
		{"empty", "rtsp://h/a/", "", "rtsp://h/a/"},
		{"star", "rtsp://h/a", "*", "rtsp://h/a"},
		{"trailing slash", "rtsp://h/a/", "trackID=1", "rtsp://h/a/trackID=1"},
		{"no trailing slash", "rtsp://h/a", "trackID=1", "rtsp://h/a/trackID=1"},
		{"dahua query", "rtsp://h/cam/realmonitor?channel=1&subtype=0/", "trackID=0", "rtsp://h/cam/realmonitor?channel=1&subtype=0/trackID=0"},
		{"dahua query no slash", "rtsp://h/cam/realmonitor?channel=1&subtype=0", "trackID=0", "rtsp://h/cam/realmonitor?channel=1&subtype=0/trackID=0"},
		{"absolute", "rtsp://h/a", "rtsp://other:8554/x/track1", "rtsp://other:8554/x/track1"},
		{"absolute creds", "rtsp://h/a", "rtsp://u:p@other/x", "rtsp://other/x"},
		{"leading slash", "rtsp://h/a/", "/track1", "rtsp://h/a/track1"},
		{"base creds", "rtsp://u:p@h/a", "t1", "rtsp://h/a/t1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveControl(c.base, c.ctl); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestStripUserinfo(t *testing.T) {
	clean, u, p, err := StripUserinfo("rtsp://us%40er:p%3Ass@host:8554/a?b=1")
	if err != nil || clean != "rtsp://host:8554/a?b=1" || u != "us@er" || p != "p:ss" {
		t.Fatalf("got %q %q %q %v", clean, u, p, err)
	}
	clean, u, p, err = StripUserinfo("rtsp://host/a")
	if err != nil || clean != "rtsp://host/a" || u != "" || p != "" {
		t.Fatalf("got %q %q %q %v", clean, u, p, err)
	}
	if _, _, _, err = StripUserinfo("rtsp://[bad/"); err == nil {
		t.Fatal("expected error")
	}
	// The error must not leak the password.
	_, _, _, err = StripUserinfo("rtsp://u:secret@[bad/")
	if err == nil || contains(err.Error(), "secret") {
		t.Fatalf("leak or nil: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
