package rtmp

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// The publish exchange. Transaction ids follow the order of the commands,
// as encoders number them.
const (
	publishFlash     = "FMLE/3.0 (compatible; rtmp)"
	txnRelease       = 2
	txnFCPublish     = 3
	txnPubCreate     = 4
	txnPublish       = 5
	txnFCUnpublish   = 6
	txnDeleteStream  = 7
	publishLive      = "live"
	publishCloseWait = 100 * time.Millisecond
)

// PublishConn publishes a stream to an RTMP server.
//
// Once Publish returns, a reader goroutine owned by the conn answers the
// server's pings, takes acknowledgements, window sizes and user control
// events, and watches onStatus: one of level "error" ends the conn with a
// *StatusError named by its code. There is no inbound idle limit, since a
// server sends only an acknowledgement per window, minutes apart at a low
// bitrate; a dead peer shows as a write that misses Config.WriteTimeout, or
// through TCP keepalive (on with the default dialer). Done closes when the
// conn has ended and Err tells why.
//
// WriteMedia may be called from any goroutine, though one writer keeps the
// messages in order; Done, Err and Close are safe from any goroutine.
//
// The stream name is often a key, and servers may echo it: every
// StatusError has it redacted as ClientConn does (the first 2 characters
// plus "xxxxx"), and no error contains the URL.
type PublishConn struct {
	s        *session
	streamID uint32
	stream   string // the publish name, with its query
	redact   *strings.Replacer

	closeOnce sync.Once
	closeErr  error
}

// Publish publishes to rtmp://host[:port]/app[/…]/stream[?query] (port 1935
// by default; the app is every path segment but the last, and a "mode"
// query parameter is dropped, as for PlayWith, unless cfg.KeepQuery). It
// dials with cfg.Dial, runs the handshake within cfg.HandshakeTimeout, then
// within cfg.CommandTimeout connect (announcing the Enhanced RTMP FourCCs
// hvc1 and avc1), Set Chunk Size 4096, releaseStream, FCPublish,
// createStream and publish "live", and returns once the server answers
// NetStream.Publish.Start. An _error reply or an onStatus of level "error",
// at any step, returns a *StatusError with the server's description (the
// stream name redacted). ctx bounds only Publish. The caller must Close the
// conn.
func Publish(ctx context.Context, rawURL string, cfg Config) (*PublishConn, error) {
	t, err := splitURL(rawURL, cfg.KeepQuery)
	if err != nil {
		return nil, err
	}
	conn, err := cfg.dial(ctx, t.addr)
	if err != nil {
		return nil, fmt.Errorf("rtmp: client: dial: %w", err)
	}
	s := newSession(conn, cfg, roleClient)
	p := &PublishConn{s: s, stream: t.stream, redact: t.redactor()}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	err = p.open(t)
	if !stop() {
		err = fmt.Errorf("rtmp: client: publish: %w", ctx.Err())
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.noIdle = true
	s.runReader(p.handle)
	return p, nil
}

func (p *PublishConn) open(t playTarget) error {
	s := p.s
	if err := s.handshake(); err != nil {
		return fmt.Errorf("rtmp: client: handshake: %w", err)
	}
	s.setPhase(s.cfg.CommandTimeout, "command exchange")
	defer s.endPhase()
	if err := s.writeCommand(0, "connect", txnConnect, Object{
		{Key: "app", Value: t.app},
		{Key: "type", Value: "nonprivate"},
		{Key: "flashVer", Value: publishFlash},
		{Key: "tcUrl", Value: t.tcURL},
		// Enhanced RTMP: the FourCC codecs we may send.
		{Key: "fourCcList", Value: []any{"hvc1", "avc1"}},
	}); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	if _, err := awaitResult(p.nextCommand, p.redact, txnConnect, "connect"); err != nil {
		return err
	}
	if err := s.clientConnected(); err != nil {
		return err
	}
	for _, c := range []struct {
		name string
		txn  float64
	}{{"releaseStream", txnRelease}, {"FCPublish", txnFCPublish}} {
		if err := s.writeCommand(0, c.name, c.txn, nil, t.stream); err != nil {
			return fmt.Errorf("rtmp: client: %w", err)
		}
	}
	if err := s.writeCommand(0, "createStream", txnPubCreate, nil); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	res, err := awaitResult(p.nextCommand, p.redact, txnPubCreate, "createStream")
	if err != nil {
		return err
	}
	if p.streamID, err = streamIDOf(res); err != nil {
		return err
	}
	if err := s.writeCommand(p.streamID, "publish", txnPublish, nil, t.stream, publishLive); err != nil {
		return fmt.Errorf("rtmp: client: %w", err)
	}
	return p.awaitPublishStart()
}

// nextCommand returns the next AMF0 command of the publish exchange,
// skipping every other message.
func (p *PublishConn) nextCommand() (command, error) {
	for {
		m, err := p.s.next()
		if err != nil {
			return command{}, fmt.Errorf("rtmp: client: %w", err)
		}
		if m.Type != MsgCommandAMF0 {
			continue
		}
		name, txn, args, err := parseCommand(m.Payload)
		if err != nil {
			return command{}, fmt.Errorf("rtmp: client: %w", err)
		}
		return command{name: name, txn: txn, args: args}, nil
	}
}

// awaitPublishStart waits for onStatus NetStream.Publish.Start. Other status
// notices (and onFCPublish) are skipped.
func (p *PublishConn) awaitPublishStart() error {
	for {
		cmd, err := p.nextCommand()
		if err != nil {
			return err
		}
		switch {
		case cmd.name == "_error" && cmd.txn == txnPublish:
			_, st := redactedStatus(p.redact, cmd.args)
			return fmt.Errorf("rtmp: client: publish: %w", st)
		case cmd.name != "onStatus":
			continue
		}
		level, st := redactedStatus(p.redact, cmd.args)
		if st.Code == codePublishStart {
			return nil
		}
		if level == "error" {
			return fmt.Errorf("rtmp: client: publish: %w", st)
		}
	}
}

// handle is the reader goroutine's handler, once publishing: the session has
// already answered pings and taken acknowledgements and window sizes. An
// onStatus of level "error" ends the conn; every other message is ignored.
// It never blocks.
func (p *PublishConn) handle(m Message) error {
	if m.Type != MsgCommandAMF0 {
		return nil
	}
	// A command that does not decode is skipped, as ClientConn does.
	if name, _, args, err := parseCommand(m.Payload); err == nil && name == "onStatus" {
		if level, st := redactedStatus(p.redact, args); level == "error" {
			return fmt.Errorf("rtmp: client: publish: %w", st)
		}
	}
	return nil
}

// WriteMedia sends one audio, video or data message on the published stream,
// on chunk stream 4 (audio), 6 (video) or 5 (data), with m.Timestamp as is. A
// data message (onMetaData) goes out behind "@setDataFrame", unless it
// already starts with it. It must finish within Config.WriteTimeout of the
// call, the wait behind another write included; a write that misses it, or
// fails, ends the conn and its error becomes Err. m.Payload is not kept
// after WriteMedia returns. A message of another type is an error that
// leaves the conn as it was.
func (p *PublishConn) WriteMedia(m Media) error {
	csid, ok := mediaCSID(m.Type)
	if !ok {
		return fmt.Errorf("rtmp: client: WriteMedia: message type %d is not audio, video or data", m.Type)
	}
	payload := m.Payload
	if m.Type == MsgDataAMF0 && !bytes.HasPrefix(payload, setDataFrame) {
		payload = append(append(make([]byte, 0, len(setDataFrame)+len(payload)), setDataFrame...), payload...)
	}
	err := p.s.writeMedia(csid, Message{Type: m.Type, StreamID: p.streamID, Timestamp: m.Timestamp, Payload: payload})
	if err != nil {
		return fmt.Errorf("rtmp: client: publish: %w", err)
	}
	return nil
}

// Close ends the publish: unless the conn has already ended, it sends
// FCUnpublish and deleteStream, giving up after 100 ms (neither a server
// that stopped reading nor a WriteMedia stalled on it holds Close up), then
// closes the connection, which fails a pending WriteMedia, and waits for the
// reader goroutine. Later calls return the first one's result. When nothing
// ended the conn before, Err then reports net.ErrClosed.
func (p *PublishConn) Close() error {
	p.closeOnce.Do(func() {
		s := p.s
		select {
		case <-s.done:
		default:
			// The goodbye waits for the write lock, which a stalled
			// WriteMedia may hold for its whole WriteTimeout, so it runs
			// apart; closing the conn below ends it.
			s.setWriteTimeout(publishCloseWait)
			bye := make(chan struct{})
			go func() {
				defer close(bye)
				if s.writeCommand(0, "FCUnpublish", txnFCUnpublish, nil, p.stream) == nil {
					_ = s.writeCommand(0, "deleteStream", txnDeleteStream, nil, float64(p.streamID))
				}
			}()
			timer := time.NewTimer(publishCloseWait)
			select {
			case <-bye:
			case <-timer.C:
			}
			timer.Stop()
			_ = s.Close()
			<-bye
		}
		// Only the first Close of the session closes; this returns its
		// result.
		p.closeErr = s.Close()
		<-s.done
	})
	return p.closeErr
}

// Done is closed once the conn has ended: the server closed the connection
// or sent an onStatus error, a write failed or timed out, or Close was
// called. Err then tells why.
func (p *PublishConn) Done() <-chan struct{} {
	return p.s.Done()
}

// Err is why the conn ended (a *StatusError for an onStatus error,
// os.ErrDeadlineExceeded for a write timeout, the read error when the
// server closed, net.ErrClosed after Close), or nil while it runs.
func (p *PublishConn) Err() error {
	return p.s.Err()
}
