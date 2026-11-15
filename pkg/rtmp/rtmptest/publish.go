// Package rtmptest provides RTMP peers for tests: a publisher that pushes FLV
// tags to an RTMP server, and a scripted server that clients play FLV tags
// from. It imports nothing from internal/.
package rtmptest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

const (
	defaultPort  = "1935"
	chunkSize    = 4096
	writeTimeout = 10 * time.Second
	// closeWait bounds how long Close waits for the server to close after
	// our FIN.
	closeWait = 2 * time.Second

	csidCommand = 3
	csidAudio   = 4
	csidData    = 5
	csidVideo   = 6

	txnConnect      = 1
	txnRelease      = 2
	txnFCPublish    = 3
	txnCreateStream = 4
	txnPublish      = 5
	txnFCUnpublish  = 6
	txnDeleteStream = 7

	codePublishStart = "NetStream.Publish.Start"
)

// setDataFrame is the AMF0 string "@setDataFrame" that encoders put in front
// of script data.
var setDataFrame = []byte{0x02, 0x00, 0x0D, '@', 's', 'e', 't', 'D', 'a', 't', 'a', 'F', 'r', 'a', 'm', 'e'}

var errAbandoned = errors.New("rtmptest: publisher abandoned")

// dialTimeout bounds Dial when its ctx has no deadline, so a server that
// stops answering fails a test quickly instead of hanging it. Tests in this
// package lower it.
var dialTimeout = 10 * time.Second

// Publisher is a connected RTMP publisher whose publish was accepted. Its
// methods must be called from one goroutine, except Abandon.
type Publisher struct {
	conn      net.Conn
	cw        *rtmp.ChunkWriter
	stream    string
	streamID  uint32
	abandoned atomic.Bool
	drained   chan struct{} // closed when the server's side has ended
	closeOnce sync.Once
	closeErr  error
}

// Dial connects to an rtmp://host[:port]/app[/…]/stream[?query] URL and
// publishes: handshake, connect, releaseStream, FCPublish, createStream and
// publish. The app is every path segment but the last; the stream name is the
// last one plus the query. It returns the code of the server's onStatus
// reply. When the server rejects the publish it returns nil, that code (for
// example NetStream.Publish.BadName) and an error carrying the server's
// description. ctx bounds only Dial; without a deadline Dial gives up after
// 10 s.
func Dial(ctx context.Context, rawURL string) (*Publisher, string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "rtmp" {
		return nil, "", errors.New("rtmptest: dial: not an rtmp:// URL")
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || segs[len(segs)-1] == "" {
		return nil, "", errors.New("rtmptest: dial: the URL needs an app and a stream name")
	}
	app := strings.Join(segs[:len(segs)-1], "/")
	stream := segs[len(segs)-1]
	if u.RawQuery != "" {
		stream += "?" + u.RawQuery
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), defaultPort)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("rtmptest: dial: %w", err)
	}
	p := &Publisher{conn: conn, cw: rtmp.NewChunkWriter(conn), stream: stream, drained: make(chan struct{})}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	br := bufio.NewReader(conn)
	code, err := p.publish(br, app, "rtmp://"+u.Host+"/"+app)
	if !stop() {
		err = fmt.Errorf("rtmptest: dial: %w", ctx.Err())
	}
	if err != nil {
		_ = conn.Close()
		return nil, code, err
	}
	// Read and discard whatever the server sends from now on (pings,
	// acknowledgements), so it never fills our receive buffer.
	go func() {
		defer close(p.drained)
		_, _ = io.Copy(io.Discard, br)
	}()
	return p, code, nil
}

// publish runs the handshake and the command exchange.
func (p *Publisher) publish(br *bufio.Reader, app, tcURL string) (string, error) {
	rw := struct {
		io.Reader
		io.Writer
	}{br, p.conn}
	if err := rtmp.ClientHandshake(rw, rand.Reader); err != nil {
		return "", fmt.Errorf("rtmptest: handshake: %w", err)
	}
	cr := rtmp.NewChunkReader(br, rtmp.ChunkLimits{})
	err := p.command(0, "connect", txnConnect, rtmp.Object{
		{Key: "app", Value: app},
		{Key: "type", Value: "nonprivate"},
		{Key: "flashVer", Value: "FMLE/3.0 (compatible; rtmptest)"},
		{Key: "tcUrl", Value: tcURL},
	})
	if err != nil {
		return "", err
	}
	if _, err := awaitResult(cr, txnConnect, "connect"); err != nil {
		return "", err
	}
	if err := p.cw.SetChunkSize(chunkSize); err != nil {
		return "", fmt.Errorf("rtmptest: %w", err)
	}
	for _, c := range []struct {
		name string
		txn  int
	}{{"releaseStream", txnRelease}, {"FCPublish", txnFCPublish}} {
		if err := p.command(0, c.name, c.txn, nil, p.stream); err != nil {
			return "", err
		}
	}
	if err := p.command(0, "createStream", txnCreateStream, nil); err != nil {
		return "", err
	}
	res, err := awaitResult(cr, txnCreateStream, "createStream")
	if err != nil {
		return "", err
	}
	id, ok := res[len(res)-1].(float64)
	if !ok || id < 1 || id > 1<<24 {
		return "", errors.New("rtmptest: createStream: no stream id in the result")
	}
	p.streamID = uint32(id)
	if err := p.command(p.streamID, "publish", txnPublish, nil, p.stream, "live"); err != nil {
		return "", err
	}
	for {
		vals, err := readCommand(cr)
		if err != nil {
			return "", err
		}
		name, txn := vals[0], vals[1]
		if name != "onStatus" && (name != "_error" || txn != float64(txnPublish)) {
			continue
		}
		code, desc := info(vals)
		if code != codePublishStart {
			return code, fmt.Errorf("rtmptest: publish rejected: %s: %s", code, desc)
		}
		return code, nil
	}
}

// command sends an AMF0 command on message stream streamID.
func (p *Publisher) command(streamID uint32, vals ...any) error {
	b, err := rtmp.EncodeAMF0(vals...)
	if err != nil {
		return fmt.Errorf("rtmptest: encode command: %w", err)
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := p.cw.WriteMessage(csidCommand, rtmp.Message{Type: rtmp.MsgCommandAMF0, StreamID: streamID, Payload: b}); err != nil {
		return fmt.Errorf("rtmptest: %w", err)
	}
	return nil
}

// readCommand returns the next AMF0 command with at least a name and a
// transaction id, skipping every other message.
func readCommand(cr *rtmp.ChunkReader) ([]any, error) {
	for {
		m, err := cr.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("rtmptest: read: %w", err)
		}
		if m.Type != rtmp.MsgCommandAMF0 {
			continue
		}
		vals, err := rtmp.DecodeAMF0(m.Payload)
		if err != nil {
			return nil, fmt.Errorf("rtmptest: decode command: %w", err)
		}
		if len(vals) >= 2 {
			return vals, nil
		}
	}
}

// awaitResult waits for the _result or _error of transaction txn.
func awaitResult(cr *rtmp.ChunkReader, txn int, what string) ([]any, error) {
	for {
		vals, err := readCommand(cr)
		if err != nil {
			return nil, err
		}
		if vals[1] != float64(txn) {
			continue
		}
		switch vals[0] {
		case "_result":
			return vals, nil
		case "_error":
			code, desc := info(vals)
			return nil, fmt.Errorf("rtmptest: %s: %s: %s", what, code, desc)
		}
	}
}

// info returns the code and description of a command's info object, its last
// Object argument.
func info(vals []any) (code, desc string) {
	for i := len(vals) - 1; i >= 0; i-- {
		if o, ok := vals[i].(rtmp.Object); ok {
			c, _ := o.Get("code")
			d, _ := o.Get("description")
			code, _ = c.(string)
			desc, _ = d.(string)
			return code, desc
		}
	}
	return "", ""
}

// WriteTag sends one FLV tag as an RTMP message on the published stream.
// Script tags are sent behind "@setDataFrame", as encoders do. After Abandon
// it fails.
func (p *Publisher) WriteTag(t rtmp.FLVTag) error {
	if p.abandoned.Load() {
		return errAbandoned
	}
	csid, payload := uint32(csidData), t.Data
	switch t.Type {
	case rtmp.FLVAudio:
		csid = csidAudio
	case rtmp.FLVVideo:
		csid = csidVideo
	case rtmp.FLVScript:
		if !bytes.HasPrefix(payload, setDataFrame) {
			payload = append(append(make([]byte, 0, len(setDataFrame)+len(t.Data)), setDataFrame...), t.Data...)
		}
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	err := p.cw.WriteMessage(csid, rtmp.Message{Type: t.Type, StreamID: p.streamID, Timestamp: t.Timestamp, Payload: payload})
	if err != nil {
		return fmt.Errorf("rtmptest: write tag: %w", err)
	}
	return nil
}

// Abandon stops the publisher without a word: later writes fail, nothing more
// is sent and the socket stays open, like a half-open peer, until Close. It
// is safe to call from any goroutine.
func (p *Publisher) Abandon() {
	p.abandoned.Store(true)
}

// Close ends the publish with FCUnpublish and deleteStream, sends FIN and
// waits up to 2 s for the server to close before closing the socket. After
// Abandon it only closes the socket.
func (p *Publisher) Close() error {
	p.closeOnce.Do(func() {
		var err error
		if !p.abandoned.Load() {
			err = p.command(0, "FCUnpublish", txnFCUnpublish, nil, p.stream)
			if err == nil {
				err = p.command(0, "deleteStream", txnDeleteStream, nil, float64(p.streamID))
			}
			if cw, ok := p.conn.(interface{ CloseWrite() error }); ok && err == nil {
				err = cw.CloseWrite()
				select {
				case <-p.drained:
				case <-time.After(closeWait):
				}
			}
		}
		if cerr := p.conn.Close(); err == nil {
			err = cerr
		}
		<-p.drained
		p.closeErr = err
	})
	return p.closeErr
}

// Publish dials rawURL, sends tags (script tags behind "@setDataFrame") and
// closes. With pace, each tag is sent when its timestamp is due relative to
// the first. ctx bounds the whole publish.
func Publish(ctx context.Context, rawURL string, tags []rtmp.FLVTag, pace bool) error {
	p, _, err := Dial(ctx, rawURL)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = p.conn.Close() })
	defer stop()
	start := time.Now()
	for _, t := range tags {
		if pace {
			due := start.Add(time.Duration(int32(t.Timestamp-tags[0].Timestamp)) * time.Millisecond)
			if wait := time.Until(due); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
			}
		}
		if err := ctx.Err(); err != nil {
			_ = p.Close()
			return fmt.Errorf("rtmptest: publish: %w", err)
		}
		if err := p.WriteTag(t); err != nil {
			_ = p.Close()
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("rtmptest: publish: %w", cerr)
			}
			return err
		}
	}
	return p.Close()
}
