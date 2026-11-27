package rtmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// Replies of the publish and play exchanges.
const (
	fmsVersion      = "FMS/3,0,1,123"
	fmsCapabilities = 31
	// mediaStreamID is the message stream id createStream returns, which
	// carries the published or played stream.
	mediaStreamID    = 1
	rejectedText     = "stream not available"
	amf3Encoding     = 3
	codePublishStart = "NetStream.Publish.Start"
)

// ErrStreamNotAvailable is what decide returns, alone or wrapped, to reject
// a request. Every rejection reads the same to the client, whatever the
// error: NetStream.Play.StreamNotFound for a play and
// NetStream.Publish.BadName for a publish, both with the description
// "stream not available".
var ErrStreamNotAvailable = errors.New("rtmp: stream not available")

// setDataFrame is the AMF0 string "@setDataFrame" that encoders put in front
// of onMetaData; ReadMedia removes it.
var setDataFrame = []byte{0x02, 0x00, 0x0D, '@', 's', 'e', 't', 'D', 'a', 't', 'a', 'F', 'r', 'a', 'm', 'e'}

// Request is what a client asked for: a play when Play is set, a publish
// otherwise. App and Stream have any "?…" removed into Query and their
// leading and trailing slashes trimmed; TCURL is the connect tcUrl as sent.
type Request struct {
	Play                      bool
	App, Stream, Query, TCURL string
}

// PublishRequest is the name Request had when the server took only
// publishers.
type PublishRequest = Request

// Path is App and Stream joined with "/".
func (r Request) Path() string {
	p, _ := NormalizePath(r.App, r.Stream)
	return p
}

// NormalizePath splits the query off app and stream, trims their slashes and
// joins the non-empty ones with "/". The two queries are joined with "&",
// app's first; empty ones are left out.
func NormalizePath(app, stream string) (path, query string) {
	app, qa := splitQuery(app)
	stream, qs := splitQuery(stream)
	return joinNonEmpty("/", app, stream), joinNonEmpty("&", qa, qs)
}

// splitQuery cuts s at the first "?" and trims the slashes of the name.
func splitQuery(s string) (name, query string) {
	name, query, _ = strings.Cut(s, "?")
	return strings.Trim(name, "/"), query
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// ServerConn is an accepted publisher or player (IsPlay).
//
// A publish conn is read with ReadMedia, from one goroutine.
//
// A play conn has a reader goroutine of its own, started by Accept, that
// answers pings, takes acknowledgements and ignores receiveAudio,
// receiveVideo, pause and seek; closeStream, deleteStream or the end of the
// connection end the conn, and so does a failed or timed-out write. Done
// closes once it has ended and Err tells why. WriteMedia and EndOfStream are
// safe from any goroutine (each message is written whole), though a player
// normally has one writer, which keeps the messages in order.
//
// Request, IsPlay, LastInbound, UnknownCommands, BadCommands,
// SetWriteTimeout, Done, Err and Close are safe from any goroutine.
type ServerConn struct {
	s       *session
	req     Request
	unknown atomic.Uint64
	bad     atomic.Uint64
	err     error // terminal ReadMedia error
}

// Accept runs the server side of a publish or a play on c: the handshake
// within HandshakeTimeout, then the command exchange within CommandTimeout.
//
// A publisher sends connect, releaseStream, FCPublish, createStream and
// publish; a player sends connect, createStream, getStreamLength (optional)
// and play. Its play start (−2, −1, 0 or any other) is ignored: every play is
// live. decide sees the request and rejects it by returning an error, which
// should be ErrStreamNotAvailable or wrap it. Whatever the error, the client
// gets the same answer, NetStream.Publish.BadName or
// NetStream.Play.StreamNotFound with the description "stream not
// available", and the error is returned wrapped. A nil decide accepts every
// request. ctx bounds only Accept. On any error c is closed. For RTMP over
// TLS, c must be the *tls.Conn itself after its handshake, never a wrapper of
// it: the bounded close (see ServerConn.Close) recognises no other type.
//
// An accepted play conn keeps the pre-publish chunk limits and starts its
// reader goroutine before Accept returns; the caller writes media with
// WriteMedia and must Close the conn.
//
// An error can follow a decide that accepted: writing the replies can fail,
// or ctx can end just after the exchange. A caller that reserves something in
// decide (a route, a slot) must release it whenever Accept returns an error.
func Accept(ctx context.Context, c net.Conn, cfg Config, decide func(Request) error) (*ServerConn, error) {
	s := newSession(c, cfg, roleServer)
	sc := &ServerConn{s: s}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	err := sc.accept(decide)
	if !stop() {
		err = fmt.Errorf("rtmp: server: accept: %w", ctx.Err())
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if sc.req.Play {
		s.runReader(sc.handlePlayer)
	}
	return sc, nil
}

func (sc *ServerConn) accept(decide func(Request) error) error {
	s := sc.s
	if err := s.handshake(); err != nil {
		return fmt.Errorf("rtmp: server: handshake: %w", err)
	}
	s.setPhase(s.cfg.CommandTimeout, "command exchange")
	defer s.endPhase()
	var app, tcURL string
	connected := false
	for {
		m, err := s.next()
		if err != nil {
			return fmt.Errorf("rtmp: server: %w", err)
		}
		if m.Type == MsgCommandAMF3 {
			if name, txn, ok := amf3CommandHead(m.Payload); ok && name == "connect" {
				sc.rejectAMF3(txn)
				return errAMF3
			}
			sc.unknown.Add(1)
			continue
		}
		if m.Type != MsgCommandAMF0 {
			continue // data, media and user control before publish
		}
		name, txn, args, err := parseCommand(m.Payload)
		if err != nil {
			return fmt.Errorf("rtmp: server: %w", err)
		}
		switch name {
		case "connect":
			if app, tcURL, err = sc.onConnect(txn, args); err != nil {
				return err
			}
			connected = true
		case "releaseStream":
			if err := s.writeCommand(0, "_result", txn, nil, Undefined{}); err != nil {
				return fmt.Errorf("rtmp: server: %w", err)
			}
		case "FCPublish":
			if err := s.writeCommand(0, "_result", txn, nil, Undefined{}); err != nil {
				return fmt.Errorf("rtmp: server: %w", err)
			}
			if err := s.writeCommand(0, "onFCPublish", 0, nil, Object{
				{Key: "code", Value: codePublishStart},
				{Key: "description", Value: "FCPublish"},
			}); err != nil {
				return fmt.Errorf("rtmp: server: %w", err)
			}
		case "createStream":
			if err := s.writeCommand(0, "_result", txn, nil, mediaStreamID); err != nil {
				return fmt.Errorf("rtmp: server: %w", err)
			}
		case "getStreamLength":
			// A live stream has no length.
			if err := s.writeCommand(m.StreamID, "_result", txn, nil, 0); err != nil {
				return fmt.Errorf("rtmp: server: %w", err)
			}
		case "publish":
			if !connected {
				return errors.New("rtmp: server: publish before connect")
			}
			return sc.onPublish(m.StreamID, txn, args, app, tcURL, decide)
		case "play":
			if !connected {
				return errors.New("rtmp: server: play before connect")
			}
			return sc.onPlay(m.StreamID, txn, args, app, tcURL, decide)
		default:
			sc.unknown.Add(1)
		}
	}
}

// onConnect answers connect and returns its app and tcUrl, or rejects AMF3
// object encoding with _error.
func (sc *ServerConn) onConnect(txn float64, args []any) (app, tcURL string, err error) {
	s := sc.s
	var obj Object
	if len(args) > 0 {
		obj, _ = args[0].(Object)
	}
	if enc, _ := obj.Get("objectEncoding"); enc == float64(amf3Encoding) {
		sc.rejectAMF3(txn)
		return "", "", errAMF3
	}
	v, _ := obj.Get("app")
	app, _ = v.(string)
	v, _ = obj.Get("tcUrl")
	tcURL, _ = v.(string)
	if err := s.sendWindowAckSize(defaultWindowAckSize); err != nil {
		return "", "", fmt.Errorf("rtmp: server: %w", err)
	}
	if err := s.sendPeerBandwidth(defaultWindowAckSize, limitDynamic); err != nil {
		return "", "", fmt.Errorf("rtmp: server: %w", err)
	}
	if err := s.setChunkSize(); err != nil {
		return "", "", fmt.Errorf("rtmp: server: %w", err)
	}
	err = s.writeCommand(0, "_result", txn,
		Object{{Key: "fmsVer", Value: fmsVersion}, {Key: "capabilities", Value: fmsCapabilities}},
		Object{{Key: "level", Value: "status"}, {Key: "code", Value: "NetConnection.Connect.Success"}, {Key: "objectEncoding", Value: 0}},
	)
	if err != nil {
		return "", "", fmt.Errorf("rtmp: server: %w", err)
	}
	return app, tcURL, nil
}

// errAMF3 is returned for a connect that asks for AMF3, by objectEncoding 3
// or as a type-17 command.
var errAMF3 = errors.New("rtmp: server: connect: AMF3 object encoding is not supported")

// rejectAMF3 answers connect transaction txn with _error. The session is
// closed next, so a write error does not matter.
func (sc *ServerConn) rejectAMF3(txn float64) {
	_ = sc.s.writeCommand(0, "_error", txn, nil, Object{
		{Key: "level", Value: "error"},
		{Key: "code", Value: "NetConnection.Connect.Rejected"},
		{Key: "description", Value: "AMF3 object encoding is not supported"},
	})
}

// request builds the Request of a publish or play command, whose args[1] is
// the stream name (args[0] is null).
func request(play bool, args []any, app, tcURL string) Request {
	var name string
	if len(args) > 1 {
		name, _ = args[1].(string)
	}
	app, qa := splitQuery(app)
	stream, qs := splitQuery(name)
	return Request{Play: play, App: app, Stream: stream, Query: joinNonEmpty("&", qa, qs), TCURL: tcURL}
}

// reject answers a rejected publish or play, transaction txn, with code and
// the fixed description, and returns decide's error wrapped. The reply never
// echoes the requested name, and the session is closed next, so a write
// error does not matter.
func (sc *ServerConn) reject(streamID uint32, txn float64, what, code string, err error) error {
	_ = sc.s.writeCommand(streamID, "onStatus", txn, nil, Object{
		{Key: "level", Value: "error"},
		{Key: "code", Value: code},
		{Key: "description", Value: rejectedText},
	})
	return fmt.Errorf("rtmp: server: %s rejected: %w", what, err)
}

// onPublish asks decide and answers publish, transaction txn. The reply
// never echoes the requested name.
func (sc *ServerConn) onPublish(streamID uint32, txn float64, args []any, app, tcURL string, decide func(Request) error) error {
	s := sc.s
	sc.req = request(false, args, app, tcURL)
	if decide != nil {
		if err := decide(sc.req); err != nil {
			return sc.reject(streamID, txn, "publish", "NetStream.Publish.BadName", err)
		}
	}
	if err := s.sendUserControl(ucStreamBegin, mediaStreamID); err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	if err := s.writeCommand(streamID, "onStatus", txn, nil, Object{
		{Key: "level", Value: "status"},
		{Key: "code", Value: codePublishStart},
		{Key: "description", Value: "Start publishing"},
	}); err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	s.raiseMessageLimit()
	return nil
}

// parseCommand splits an AMF0 command into its name, transaction id and
// remaining arguments. A missing or non-number transaction id reads as 0.
// Its errors are wrapped by the caller with its role ("rtmp: server: …").
func parseCommand(p []byte) (name string, txn float64, args []any, err error) {
	vals, err := DecodeAMF0(p)
	if err != nil {
		return "", 0, nil, fmt.Errorf("command: %w", err)
	}
	if len(vals) == 0 {
		return "", 0, nil, errors.New("empty command")
	}
	name, ok := vals[0].(string)
	if !ok {
		return "", 0, nil, fmt.Errorf("command name is %T", vals[0])
	}
	if len(vals) > 1 {
		txn, _ = vals[1].(float64)
		args = vals[2:]
	}
	return name, txn, args, nil
}

// amf3CommandHead reads the name and transaction id of a type-17 (AMF3)
// command: a 0x00 format byte, then the name and transaction id in AMF0. The
// arguments, which may switch to AMF3, are not decoded.
func amf3CommandHead(p []byte) (name string, txn float64, ok bool) {
	if len(p) > 0 && p[0] == 0 {
		p = p[1:]
	}
	d := amfDecoder{b: p}
	v, err := d.value(0)
	if err != nil {
		return "", 0, false
	}
	if name, ok = v.(string); !ok {
		return "", 0, false
	}
	if v, err := d.value(0); err == nil {
		txn, _ = v.(float64)
	}
	return name, txn, true
}

// Request is the accepted request.
func (sc *ServerConn) Request() Request {
	return sc.req
}

// IsPlay reports whether the conn is a player, rather than a publisher.
func (sc *ServerConn) IsPlay() bool {
	return sc.req.Play
}

// errPlayRead is ReadMedia's error on a play conn.
var errPlayRead = errors.New("rtmp: server: ReadMedia on a play conn")

// ReadMedia returns the next audio, video or metadata message of a publish
// conn. A leading "@setDataFrame" is removed from data messages, so metadata
// reads as the onMetaData tag body. deleteStream, FCUnpublish and
// closeStream end the stream with io.EOF. Unknown commands, and every
// type-17 (AMF3) command, are counted and skipped, and so are AMF0 commands
// that do not decode, which BadCommands counts. Any error is terminal and is
// returned again by later calls; the caller then Closes. On a play conn it
// returns an error at once.
func (sc *ServerConn) ReadMedia() (Media, error) {
	if sc.req.Play {
		return Media{}, errPlayRead
	}
	if sc.err != nil {
		return Media{}, sc.err
	}
	for {
		m, err := sc.s.next()
		if err != nil {
			sc.err = fmt.Errorf("rtmp: server: %w", err)
			return Media{}, sc.err
		}
		if med, ok := mediaOf(m); ok {
			if med.Type == MsgDataAMF0 {
				med.Payload = bytes.TrimPrefix(med.Payload, setDataFrame)
			}
			return med, nil
		}
		if m.Type == MsgCommandAMF3 {
			sc.unknown.Add(1)
			continue
		}
		if m.Type != MsgCommandAMF0 {
			continue
		}
		name, _, _, err := parseCommand(m.Payload)
		if err != nil {
			sc.bad.Add(1)
			continue
		}
		switch name {
		case "deleteStream", "FCUnpublish", "closeStream":
			sc.err = io.EOF
			return Media{}, io.EOF
		default:
			sc.unknown.Add(1)
		}
	}
}

// LastInbound is when the last byte arrived from the publisher, for takeover
// decisions.
func (sc *ServerConn) LastInbound() time.Time {
	return sc.s.LastInbound()
}

// UnknownCommands counts the commands ignored because the server does not
// know them, such as _checkbw and FCSubscribe, and every type-17 (AMF3)
// command other than a connect, which is rejected.
func (sc *ServerConn) UnknownCommands() uint64 {
	return sc.unknown.Load()
}

// BadCommands counts the AMF0 commands from the client that did not decode
// after publish or play was accepted, and were skipped.
func (sc *ServerConn) BadCommands() uint64 {
	return sc.bad.Load()
}

// Close closes the connection, unblocking a pending ReadMedia or WriteMedia
// and stopping a play conn's reader. It writes no RTMP message. When Accept
// was given a *tls.Conn and no write is in flight, it sends close_notify and
// waits at most 100 ms for that write, so a player that stopped reading never
// holds it longer.
func (sc *ServerConn) Close() error {
	return sc.s.Close()
}
