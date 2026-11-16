package rtmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

// The play exchange.
const (
	defaultPort   = "1935"
	clientFlash   = "LNX 9,0,124,2"
	playLive      = -2   // play start: live, falling back to recorded
	playBufferMS  = 1000 // Set Buffer Length we announce
	txnConnect    = 1
	txnCreate     = 2
	txnPlay       = 3
	codePlayStart = "NetStream.Play.Start"

	// maxHeldMedia and maxHeldBytes bound the data messages kept for
	// ReadMedia when a server sends them before NetStream.Play.Start.
	maxHeldMedia = 8
	maxHeldBytes = 1 << 20
)

// StatusError is a server's refusal: an _error reply, or an onStatus whose
// level is "error" (such as NetStream.Play.StreamNotFound or
// NetStream.Play.Failed). Description is the server's text, except that the
// play name, which may be a stream key, is redacted (see ClientConn).
type StatusError struct {
	Code, Description string
}

func (e *StatusError) Error() string {
	code := e.Code
	if code == "" {
		code = "error"
	}
	if e.Description == "" {
		return code
	}
	return code + ": " + e.Description
}

// statusOf reads the level and the StatusError of a command's info object,
// its last Object argument.
func statusOf(args []any) (level string, st *StatusError) {
	st = &StatusError{}
	for i := len(args) - 1; i >= 0; i-- {
		if o, ok := args[i].(Object); ok {
			v, _ := o.Get("level")
			level, _ = v.(string)
			v, _ = o.Get("code")
			st.Code, _ = v.(string)
			v, _ = o.Get("description")
			st.Description, _ = v.(string)
			break
		}
	}
	return level, st
}

// playTarget is a play URL split into what the exchange sends.
type playTarget struct {
	addr   string // host:port to dial
	app    string
	name   string // the stream name, without the query
	stream string // the name with the query, minus our mode parameter
	tcURL  string
}

// redactedName is name cut to its first 2 characters plus "xxxxx", or
// "xxxxx" alone for a name of up to 2 characters: the form ingest.Redact
// gives the last path segment of an rtmp:// URL.
func redactedName(name string) string {
	r := []rune(name)
	if len(r) > 2 {
		return string(r[:2]) + "xxxxx"
	}
	return "xxxxx"
}

// redactor replaces the play name in a server's text: the name with its
// query (when there is one), then the bare name, by the redacted name. A
// bare name of up to 2 characters is left alone: replacing so short a
// string would mangle the text, and a name that short is no secret.
func (t playTarget) redactor() *strings.Replacer {
	masked := redactedName(t.name)
	var pairs []string
	if t.stream != t.name {
		pairs = append(pairs, t.stream, masked)
	}
	if len([]rune(t.name)) > 2 {
		pairs = append(pairs, t.name, masked)
	}
	return strings.NewReplacer(pairs...)
}

// splitPlayURL splits rtmp://host[:port]/app[/…]/stream[?query]. The app is
// every path segment but the last, so rtmp://h/live/inst/s plays s from
// live/inst. The stream name keeps the query, without the mode parameter
// that only means something to us. Errors never echo the URL.
func splitPlayURL(rawURL string) (playTarget, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return playTarget{}, errors.New("rtmp: client: invalid URL")
	}
	if u.Scheme != "rtmp" {
		return playTarget{}, errors.New("rtmp: client: not an rtmp:// URL")
	}
	if u.Hostname() == "" {
		return playTarget{}, errors.New("rtmp: client: the URL has no host")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return playTarget{}, errors.New("rtmp: client: invalid port")
	}
	segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), "/"), "/")
	if len(segs) < 2 {
		return playTarget{}, errors.New("rtmp: client: the URL needs an app and a stream name")
	}
	for i, s := range segs {
		if s == "" {
			return playTarget{}, errors.New("rtmp: client: empty path segment")
		}
		if segs[i], err = url.PathUnescape(s); err != nil {
			return playTarget{}, errors.New("rtmp: client: invalid path escape")
		}
	}
	host := u.Host
	if u.Port() == "" && strings.HasSuffix(host, ":") {
		host = strings.TrimSuffix(host, ":")
	}
	app := strings.Join(segs[:len(segs)-1], "/")
	t := playTarget{
		addr:   net.JoinHostPort(u.Hostname(), port),
		app:    app,
		name:   segs[len(segs)-1],
		stream: segs[len(segs)-1],
		tcURL:  "rtmp://" + host + "/" + app,
	}
	if q := stripMode(u.RawQuery); q != "" {
		t.stream += "?" + q
	}
	return t, nil
}

// stripMode removes every mode parameter from a raw query, keeping the
// others in order and as escaped.
func stripMode(raw string) string {
	var keep []string
	for p := range strings.SplitSeq(raw, "&") {
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		if key, err := url.QueryUnescape(k); err == nil && key == "mode" {
			continue
		}
		keep = append(keep, p)
	}
	return strings.Join(keep, "&")
}

// ClientConn plays a stream from an RTMP server. ReadMedia must be called
// from one goroutine; BadCommands and Close are safe from any.
//
// The play name is often a stream key, and servers may echo it in their
// status descriptions. Every StatusError therefore has the name, with or
// without its query, replaced by its redacted form: the first 2 characters
// plus "xxxxx", as ingest.Redact masks the last path segment.
type ClientConn struct {
	s        *session
	streamID uint32
	redact   *strings.Replacer
	held     []Media // media that arrived before the play exchange ended
	heldData int     // bytes of data messages in held
	err      error   // terminal ReadMedia error
	bad      atomic.Uint64
}

// status is statusOf with the play name redacted from the description.
func (c *ClientConn) status(args []any) (level string, st *StatusError) {
	level, st = statusOf(args)
	st.Description = c.redact.Replace(st.Description)
	return level, st
}

// Play is PlayWith with the default Config.
func Play(ctx context.Context, rawURL string) (*ClientConn, error) {
	return PlayWith(ctx, rawURL, Config{})
}

// PlayWith plays rtmp://host[:port]/app[/…]/stream[?query] (port 1935 by
// default): it dials, runs the handshake within cfg.HandshakeTimeout, then
// connect, createStream and play within cfg.CommandTimeout, and returns once
// the server sends NetStream.Play.Start (or, from a server that does not,
// the first audio or video message). An _error reply or an onStatus of level
// "error", at any step, returns a *StatusError with the server's description
// (the play name redacted). ctx bounds only PlayWith. Errors never contain
// the URL.
func PlayWith(ctx context.Context, rawURL string, cfg Config) (*ClientConn, error) {
	t, err := splitPlayURL(rawURL)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, fmt.Errorf("rtmp: client: dial: %w", err)
	}
	s := newSession(conn, cfg, roleClient)
	c := &ClientConn{s: s, redact: t.redactor()}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	err = c.open(t)
	if !stop() {
		err = fmt.Errorf("rtmp: client: play: %w", ctx.Err())
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return c, nil
}

// command is a decoded AMF0 command.
type command struct {
	name string
	txn  float64
	args []any
}

func (c *ClientConn) open(t playTarget) error {
	s := c.s
	if err := s.handshake(); err != nil {
		return fmt.Errorf("rtmp: client: handshake: %w", err)
	}
	s.setPhase(s.cfg.CommandTimeout, "command exchange")
	defer s.endPhase()
	if err := s.writeCommand(0, "connect", txnConnect, Object{
		{Key: "app", Value: t.app},
		{Key: "flashVer", Value: clientFlash},
		{Key: "tcUrl", Value: t.tcURL},
		{Key: "fpad", Value: false},
		{Key: "capabilities", Value: 15},
		{Key: "audioCodecs", Value: 0x0FFF},
		{Key: "videoCodecs", Value: 0x00FF},
		{Key: "videoFunction", Value: 1},
		// Enhanced RTMP: the FourCC codecs we read beyond the legacy codec
		// ids. A server that honours the list (MediaMTX does) omits an
		// H.265 track from a client that does not announce hvc1.
		{Key: "fourCcList", Value: []any{"hvc1"}},
	}); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	if _, err := c.awaitResult(txnConnect, "connect"); err != nil {
		return err
	}
	if err := s.setChunkSize(defaultOutChunkSize); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	// A hard Set Peer Bandwidth may already have made the session announce
	// a window of the server's size; keep that one.
	if s.sentWindow == 0 {
		if err := s.sendWindowAckSize(defaultWindowAckSize); err != nil {
			return fmt.Errorf("rtmp: client: %w", err)
		}
	}
	if err := s.writeCommand(0, "createStream", txnCreate, nil); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	res, err := c.awaitResult(txnCreate, "createStream")
	if err != nil {
		return err
	}
	var id float64
	if len(res) > 0 {
		id, _ = res[len(res)-1].(float64)
	}
	if id < 1 || id > math.MaxUint32 || id != math.Trunc(id) {
		return errors.New("rtmp: client: createStream: no stream id in the result")
	}
	c.streamID = uint32(id)
	if err := s.writeCommand(c.streamID, "play", txnPlay, nil, t.stream, playLive); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	if err := s.sendUserControl(ucSetBufferLength, c.streamID, playBufferMS); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	return c.awaitPlayStart()
}

// awaitResult waits for the _result or _error of transaction txn and
// returns the _result's arguments. An onStatus of level "error" fails at
// once, since a server that sends one may never answer the transaction.
func (c *ClientConn) awaitResult(txn float64, what string) ([]any, error) {
	for {
		cmd, _, err := c.openCommand(false)
		if err != nil {
			return nil, err
		}
		if cmd.name == "onStatus" {
			if level, st := c.status(cmd.args); level == "error" {
				return nil, fmt.Errorf("rtmp: client: %s: %w", what, st)
			}
			continue
		}
		if cmd.txn != txn {
			continue
		}
		switch cmd.name {
		case "_result":
			return cmd.args, nil
		case "_error":
			_, st := c.status(cmd.args)
			return nil, fmt.Errorf("rtmp: client: %s: %w", what, st)
		}
	}
}

// awaitPlayStart waits for onStatus NetStream.Play.Start, or the first audio
// or video message.
func (c *ClientConn) awaitPlayStart() error {
	for {
		cmd, started, err := c.openCommand(true)
		if err != nil {
			return err
		}
		if started {
			return nil
		}
		switch {
		case cmd.name == "_error" && cmd.txn == txnPlay:
			_, st := c.status(cmd.args)
			return fmt.Errorf("rtmp: client: play: %w", st)
		case cmd.name != "onStatus":
			continue
		}
		level, st := c.status(cmd.args)
		switch st.Code {
		case codePlayStart:
			return nil
		case "NetStream.Play.StreamNotFound", "NetStream.Play.Failed":
			return fmt.Errorf("rtmp: client: play: %w", st)
		}
		if level == "error" {
			return fmt.Errorf("rtmp: client: play: %w", st)
		}
	}
}

// openCommand returns the next AMF0 command of the play exchange, skipping
// other messages. Once play was sent (playing), media is held for ReadMedia
// and an audio or video message ends the exchange: openCommand then reports
// started. Data messages beyond maxHeldMedia or maxHeldBytes are dropped, and
// so is media before play.
func (c *ClientConn) openCommand(playing bool) (command, bool, error) {
	for {
		m, err := c.s.next()
		if err != nil {
			return command{}, false, fmt.Errorf("rtmp: client: %w", err)
		}
		if med, ok := mediaOf(m); ok {
			if !playing {
				continue
			}
			if med.Type != MsgDataAMF0 {
				c.held = append(c.held, med)
				return command{}, true, nil
			}
			if len(c.held) < maxHeldMedia && c.heldData+len(med.Payload) <= maxHeldBytes {
				med.Payload = bytes.TrimPrefix(med.Payload, setDataFrame)
				c.held = append(c.held, med)
				c.heldData += len(med.Payload)
			}
			continue
		}
		if m.Type != MsgCommandAMF0 {
			continue
		}
		name, txn, args, err := parseCommand(m.Payload)
		if err != nil {
			return command{}, false, fmt.Errorf("rtmp: client: %w", err)
		}
		return command{name: name, txn: txn, args: args}, false, nil
	}
}

// ReadMedia returns the next audio, video or metadata message. A leading
// "@setDataFrame" is removed from data messages. The stream ends with io.EOF
// on user control StreamEOF or onStatus NetStream.Play.Stop or
// NetStream.Play.UnpublishNotify; an onStatus of level "error" returns a
// *StatusError. Other commands and notices are skipped, and so are commands
// that do not decode, which BadCommands counts. Any error is terminal and is
// returned again by later calls; the caller then Closes.
func (c *ClientConn) ReadMedia() (Media, error) {
	if len(c.held) > 0 {
		m := c.held[0]
		c.held[0] = Media{}
		c.held = c.held[1:]
		return m, nil
	}
	if c.err != nil {
		return Media{}, c.err
	}
	for {
		m, err := c.s.next()
		if err != nil {
			c.err = fmt.Errorf("rtmp: client: %w", err)
			return Media{}, c.err
		}
		if med, ok := mediaOf(m); ok {
			if med.Type == MsgDataAMF0 {
				med.Payload = bytes.TrimPrefix(med.Payload, setDataFrame)
			}
			return med, nil
		}
		switch m.Type {
		case MsgUserControl:
			if ev, _, err := parseUserControl(m.Payload); err == nil && ev == ucStreamEOF {
				c.err = io.EOF
				return Media{}, io.EOF
			}
		case MsgCommandAMF0:
			name, _, args, err := parseCommand(m.Payload)
			if err != nil {
				c.bad.Add(1)
				continue
			}
			if name != "onStatus" {
				continue
			}
			level, st := c.status(args)
			switch st.Code {
			case "NetStream.Play.Stop", "NetStream.Play.UnpublishNotify":
				c.err = io.EOF
				return Media{}, io.EOF
			}
			if level == "error" {
				c.err = fmt.Errorf("rtmp: client: %w", st)
				return Media{}, c.err
			}
		}
	}
}

// BadCommands counts the commands from the server that did not decode
// during playback and were skipped.
func (c *ClientConn) BadCommands() uint64 {
	return c.bad.Load()
}

// Close closes the connection, unblocking a pending ReadMedia. It never
// writes.
func (c *ClientConn) Close() error {
	return c.s.Close()
}
