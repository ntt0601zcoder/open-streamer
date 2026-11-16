package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default limits applied when a Limits field is zero.
const (
	defaultHeaderBytes = 64 << 10
	defaultBodyBytes   = 1 << 20
)

// protoVersion is the only RTSP version spoken.
const protoVersion = "RTSP/1.0"

// Limits bounds what the readers accept. A zero field means its default.
type Limits struct {
	// HeaderBytes caps every byte from the start line through the blank line
	// (default 64 KiB).
	HeaderBytes int
	// BodyBytes caps the Content-Length (default 1 MiB).
	BodyBytes int
}

func (l Limits) headerBytes() int {
	if l.HeaderBytes <= 0 {
		return defaultHeaderBytes
	}
	return l.HeaderBytes
}

func (l Limits) bodyBytes() int {
	if l.BodyBytes <= 0 {
		return defaultBodyBytes
	}
	return l.BodyBytes
}

// Header maps canonical header names to their values.
type Header map[string][]string

// canonicalSpelling lists the names whose RTSP spelling is not plain
// title-casing of dash-separated words.
var canonicalSpelling = map[string]string{
	"cseq":             "CSeq",
	"www-authenticate": "WWW-Authenticate",
	"rtp-info":         "RTP-Info",
}

// CanonicalKey returns the canonical RTSP spelling of a header name: the
// special cases (CSeq, WWW-Authenticate, RTP-Info) are kept exact and every
// other name is title-cased per dash-separated word.
func CanonicalKey(k string) string {
	lower := strings.ToLower(k)
	if c, ok := canonicalSpelling[lower]; ok {
		return c
	}
	b := []byte(lower)
	up := true
	for i, c := range b {
		if up && c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
		up = c == '-'
	}
	return string(b)
}

// Get returns the first value of the named header, or "".
func (h Header) Get(k string) string {
	if v := h[CanonicalKey(k)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Set replaces the named header with a single value.
func (h Header) Set(k, v string) { h[CanonicalKey(k)] = []string{v} }

// Add appends a value to the named header.
func (h Header) Add(k, v string) {
	k = CanonicalKey(k)
	h[k] = append(h[k], v)
}

// Request is an RTSP request.
type Request struct {
	Method string
	URL    string
	Header Header
	Body   []byte
}

// Response is an RTSP response.
type Response struct {
	StatusCode int
	Reason     string
	Header     Header
	Body       []byte
}

// validToken reports whether s is non-empty and free of whitespace and
// control characters.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func validText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' || s[i] == 0 {
			return false
		}
	}
	return true
}

// writeHeaders appends the headers in sorted order, CSeq first. Content-Length
// is always derived from the body, never taken from the map.
func writeHeaders(buf *bytes.Buffer, h Header, body []byte) error {
	keys := make([]string, 0, len(h))
	for k := range h {
		if CanonicalKey(k) == "Content-Length" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, cj := CanonicalKey(keys[i]) == "CSeq", CanonicalKey(keys[j]) == "CSeq"
		if ci != cj {
			return ci
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		if !validToken(k) || strings.ContainsRune(k, ':') {
			return fmt.Errorf("rtsp: write header: invalid name %q", k)
		}
		for _, v := range h[k] {
			if !validText(v) {
				return fmt.Errorf("rtsp: write header: invalid value for %s", k)
			}
			buf.WriteString(CanonicalKey(k))
			buf.WriteString(": ")
			buf.WriteString(v)
			buf.WriteString("\r\n")
		}
	}
	if len(body) > 0 {
		buf.WriteString("Content-Length: ")
		buf.WriteString(strconv.Itoa(len(body)))
		buf.WriteString("\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	return nil
}

// Write serialises the request in a single Write call.
func (r *Request) Write(w io.Writer) error {
	if !validToken(r.Method) || !validToken(r.URL) {
		return errors.New("rtsp: write request: invalid method or URL")
	}
	var buf bytes.Buffer
	buf.WriteString(r.Method + " " + r.URL + " " + protoVersion + "\r\n")
	if err := writeHeaders(&buf, r.Header, r.Body); err != nil {
		return err
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("rtsp: write request: %w", err)
	}
	return nil
}

// Write serialises the response in a single Write call.
func (r *Response) Write(w io.Writer) error {
	if r.StatusCode < 100 || r.StatusCode > 999 {
		return errors.New("rtsp: write response: invalid status code")
	}
	if !validText(r.Reason) {
		return errors.New("rtsp: write response: invalid reason")
	}
	var buf bytes.Buffer
	buf.WriteString(protoVersion + " " + strconv.Itoa(r.StatusCode) + " " + r.Reason + "\r\n")
	if err := writeHeaders(&buf, r.Header, r.Body); err != nil {
		return err
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("rtsp: write response: %w", err)
	}
	return nil
}

// readLine reads one line without its terminator, charging every byte read to
// *budget. It fails as soon as the budget is exceeded, so an endless line
// never grows without bound.
func readLine(br *bufio.Reader, budget *int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		*budget -= len(chunk)
		if *budget < 0 {
			return "", errors.New("rtsp: read: header block too large")
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			line = bytes.TrimSuffix(line, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			return string(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return "", io.EOF
			}
			return "", io.ErrUnexpectedEOF
		default:
			return "", fmt.Errorf("rtsp: read: %w", err)
		}
	}
}

// readMessage reads the start line, the headers and the body.
func readMessage(br *bufio.Reader, lim Limits) (start string, h Header, body []byte, err error) {
	budget := lim.headerBytes()
	start, err = readLine(br, &budget)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", nil, nil, fmt.Errorf("rtsp: read start line: %w", err)
		}
		return "", nil, nil, err
	}
	h = Header{}
	var lastKey string
	for {
		line, lerr := readLine(br, &budget)
		if lerr != nil {
			if errors.Is(lerr, io.EOF) {
				lerr = io.ErrUnexpectedEOF
			}
			if errors.Is(lerr, io.ErrUnexpectedEOF) {
				return "", nil, nil, fmt.Errorf("rtsp: read headers: %w", lerr)
			}
			return "", nil, nil, lerr
		}
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			vals := h[lastKey]
			if lastKey == "" || len(vals) == 0 {
				return "", nil, nil, errors.New("rtsp: read headers: continuation without header")
			}
			vals[len(vals)-1] += " " + strings.TrimSpace(line)
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return "", nil, nil, errors.New("rtsp: read headers: malformed header line")
		}
		lastKey = CanonicalKey(name)
		h[lastKey] = append(h[lastKey], strings.TrimSpace(value))
	}
	body, err = readBody(br, h, lim.bodyBytes())
	if err != nil {
		return "", nil, nil, err
	}
	return start, h, body, nil
}

func readBody(br *bufio.Reader, h Header, max int) ([]byte, error) {
	vals, ok := h["Content-Length"]
	if !ok {
		return nil, nil
	}
	n := -1
	for _, v := range vals {
		x, err := strconv.ParseUint(v, 10, 31)
		if err != nil {
			if ne := (*strconv.NumError)(nil); errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) {
				return nil, errors.New("rtsp: read body: Content-Length too large")
			}
			return nil, errors.New("rtsp: read body: invalid Content-Length")
		}
		if n >= 0 && n != int(x) {
			return nil, errors.New("rtsp: read body: conflicting Content-Length")
		}
		n = int(x)
	}
	if n > max {
		return nil, errors.New("rtsp: read body: Content-Length too large")
	}
	if n == 0 {
		return nil, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("rtsp: read body: %w", err)
	}
	return body, nil
}

// ReadRequest reads one request. A clean end of stream before any byte
// returns an error wrapping io.EOF.
func ReadRequest(br *bufio.Reader, lim Limits) (*Request, error) {
	start, h, body, err := readMessage(br, lim)
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(start)
	if len(parts) != 3 || parts[2] != protoVersion {
		return nil, errors.New("rtsp: read request: malformed request line")
	}
	return &Request{Method: parts[0], URL: parts[1], Header: h, Body: body}, nil
}

// ReadResponse reads one response. A clean end of stream before any byte
// returns an error wrapping io.EOF.
func ReadResponse(br *bufio.Reader, lim Limits) (*Response, error) {
	start, h, body, err := readMessage(br, lim)
	if err != nil {
		return nil, err
	}
	proto, rest, _ := strings.Cut(start, " ")
	if proto != protoVersion {
		return nil, errors.New("rtsp: read response: malformed status line")
	}
	codeStr, reason, _ := strings.Cut(strings.TrimLeft(rest, " "), " ")
	code, cerr := strconv.Atoi(codeStr)
	if cerr != nil || len(codeStr) != 3 || code < 100 {
		return nil, errors.New("rtsp: read response: invalid status code")
	}
	return &Response{StatusCode: code, Reason: strings.TrimSpace(reason), Header: h, Body: body}, nil
}

// Transport is a parsed Transport header entry (RFC 2326 §12.39).
type Transport struct {
	// Protocol is the transport spec, e.g. "RTP/AVP", "RTP/AVP/TCP".
	Protocol string
	// Unicast is true for "unicast", false for multicast.
	Unicast bool
	// Interleaved is the channel pair of an interleaved (TCP) transport.
	Interleaved [2]int
	// ClientPort and ServerPort are the RTP/RTCP port pairs of a UDP transport.
	ClientPort, ServerPort [2]int
	// Source is the "source" parameter.
	Source string
	// SSRC is valid when HasSSRC is set.
	SSRC    uint32
	HasSSRC bool
}

func parseRange(v string, max int, single func(int) [2]int) ([2]int, error) {
	a, b, hasB := strings.Cut(v, "-")
	x, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil || x < 0 || x > max {
		return [2]int{}, errors.New("value out of range")
	}
	if !hasB {
		return single(x), nil
	}
	y, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil || y < 0 || y > max {
		return [2]int{}, errors.New("value out of range")
	}
	return [2]int{x, y}, nil
}

// ParseTransport parses the first entry of a Transport header value.
func ParseTransport(v string) (Transport, error) {
	first, _, _ := strings.Cut(v, ",")
	parts := strings.Split(first, ";")
	var t Transport
	t.Protocol = strings.TrimSpace(parts[0])
	if t.Protocol == "" || strings.ContainsAny(t.Protocol, " =") {
		return Transport{}, errors.New("rtsp: parse transport: missing protocol")
	}
	for _, p := range parts[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.TrimSpace(val)
		var err error
		switch k {
		case "unicast":
			t.Unicast = true
		case "interleaved":
			t.Interleaved, err = parseRange(val, 255, func(x int) [2]int { return [2]int{x, x + 1} })
		case "client_port":
			t.ClientPort, err = parseRange(val, 65535, func(x int) [2]int { return [2]int{x, x} })
		case "server_port":
			t.ServerPort, err = parseRange(val, 65535, func(x int) [2]int { return [2]int{x, x} })
		case "source":
			t.Source = val
		case "ssrc":
			var n uint64
			n, err = strconv.ParseUint(val, 16, 32)
			t.SSRC, t.HasSSRC = uint32(n), err == nil
		}
		if err != nil {
			return Transport{}, fmt.Errorf("rtsp: parse transport: invalid %s", k)
		}
	}
	return t, nil
}

// String renders the transport as a request-side Transport header value.
func (t Transport) String() string {
	var sb strings.Builder
	sb.WriteString(t.Protocol)
	if t.Unicast {
		sb.WriteString(";unicast")
	} else {
		sb.WriteString(";multicast")
	}
	if t.Interleaved != [2]int{} {
		fmt.Fprintf(&sb, ";interleaved=%d-%d", t.Interleaved[0], t.Interleaved[1])
	}
	if t.ClientPort != [2]int{} {
		fmt.Fprintf(&sb, ";client_port=%d-%d", t.ClientPort[0], t.ClientPort[1])
	}
	if t.ServerPort != [2]int{} {
		fmt.Fprintf(&sb, ";server_port=%d-%d", t.ServerPort[0], t.ServerPort[1])
	}
	if t.Source != "" {
		sb.WriteString(";source=" + t.Source)
	}
	if t.HasSSRC {
		fmt.Fprintf(&sb, ";ssrc=%08X", t.SSRC)
	}
	return sb.String()
}

// Session is a parsed Session header.
type Session struct {
	ID string
	// Timeout is zero when the header carried none; callers apply their own
	// default (RFC 2326 says 60 s).
	Timeout time.Duration
}

// ParseSession parses a Session header value ("id[;timeout=n]").
func ParseSession(v string) (Session, error) {
	parts := strings.Split(v, ";")
	s := Session{ID: strings.TrimSpace(parts[0])}
	if s.ID == "" {
		return Session{}, errors.New("rtsp: parse session: empty id")
	}
	for _, p := range parts[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		if !strings.EqualFold(strings.TrimSpace(k), "timeout") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 31)
		if err != nil {
			return Session{}, errors.New("rtsp: parse session: invalid timeout")
		}
		s.Timeout = time.Duration(n) * time.Second
	}
	return s, nil
}

// RTPInfo is one entry of an RTP-Info header.
type RTPInfo struct {
	URL     string
	Seq     uint16
	RTPTime uint32
	HasSeq  bool
	HasTime bool
}

// ParseRTPInfo parses an RTP-Info header value. Fields that are missing or out
// of range are left unset; entries without a URL and fields are skipped.
func ParseRTPInfo(v string) []RTPInfo {
	var entries []string
	for _, piece := range strings.Split(v, ",") {
		trim := strings.TrimSpace(piece)
		if len(entries) > 0 && !strings.HasPrefix(strings.ToLower(trim), "url=") {
			// A comma inside a URL: glue the piece back on.
			entries[len(entries)-1] += "," + piece
			continue
		}
		entries = append(entries, trim)
	}
	var out []RTPInfo
	for _, e := range entries {
		var info RTPInfo
		seen := false
		for _, p := range strings.Split(e, ";") {
			k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok {
				continue
			}
			val = strings.TrimSpace(val)
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "url":
				info.URL, seen = val, true
			case "seq":
				if n, err := strconv.ParseUint(val, 10, 16); err == nil {
					info.Seq, info.HasSeq, seen = uint16(n), true, true
				}
			case "rtptime":
				if n, err := strconv.ParseUint(val, 10, 32); err == nil {
					info.RTPTime, info.HasTime, seen = uint32(n), true, true
				}
			}
		}
		if seen {
			out = append(out, info)
		}
	}
	return out
}
