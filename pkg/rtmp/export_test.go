package rtmp

// SplitPlayURL exposes the play URL split to the external tests.
func SplitPlayURL(rawURL string) (addr, app, stream, tcURL string, err error) {
	t, err := splitPlayURL(rawURL)
	return t.addr, t.app, t.stream, t.tcURL, err
}
