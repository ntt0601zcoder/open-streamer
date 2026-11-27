package rtmp

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Chunk streams of a played or published stream's messages; a played
// stream's are all on message stream mediaStreamID.
const (
	audioCSID = 4
	dataCSID  = 5
	videoCSID = 6
)

// Codes and descriptions of the play exchange. No description echoes the
// requested name, which may be secret.
const (
	codePlayReset    = "NetStream.Play.Reset"
	codeNotFound     = "NetStream.Play.StreamNotFound"
	codeUnpublish    = "NetStream.Play.UnpublishNotify"
	descPlayReset    = "Playing and resetting"
	descPlayStart    = "Started playing"
	descStreamEnded  = "stream ended"
	sampleAccessName = "|RtmpSampleAccess"
)

// errNotPlay is the error of WriteMedia and EndOfStream on a publish conn.
var errNotPlay = errors.New("rtmp: server: not a play conn")

// onPlay asks decide and answers play, transaction txn. play is expected on
// the stream createStream returned (1); the replies go on the message stream
// play arrived on, and media always goes on stream 1 whatever that was. On
// success it sends Set Chunk Size,
// StreamBegin, onStatus NetStream.Play.Reset and NetStream.Play.Start, and
// |RtmpSampleAccess. A rejection is NetStream.Play.StreamNotFound with the
// fixed description.
func (sc *ServerConn) onPlay(streamID uint32, txn float64, args []any, app, tcURL string, decide func(Request) error) error {
	s := sc.s
	sc.req = request(true, args, app, tcURL)
	if decide != nil {
		if err := decide(sc.req); err != nil {
			return sc.reject(streamID, txn, "play", codeNotFound, err)
		}
	}
	if err := s.setChunkSize(); err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	if err := s.sendUserControl(ucStreamBegin, mediaStreamID); err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	for _, st := range [][2]string{{codePlayReset, descPlayReset}, {codePlayStart, descPlayStart}} {
		if err := s.writeCommand(streamID, "onStatus", txn, nil, Object{
			{Key: "level", Value: "status"},
			{Key: "code", Value: st[0]},
			{Key: "description", Value: st[1]},
		}); err != nil {
			return fmt.Errorf("rtmp: server: %w", err)
		}
	}
	p, err := EncodeAMF0(sampleAccessName, true, true)
	if err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	if err := s.write(false, func(cw *ChunkWriter) error {
		return cw.WriteMessage(dataCSID, Message{Type: MsgDataAMF0, StreamID: mediaStreamID, Payload: p})
	}); err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	return nil
}

// handlePlayer is a play conn's message handler, on the reader goroutine:
// the session has already answered pings and taken acknowledgements and
// window sizes. closeStream and deleteStream end the conn with io.EOF;
// receiveAudio, receiveVideo, pause and seek are ignored, since every play
// is live and the player always gets both kinds; other commands are counted,
// and everything else (user control, media) is ignored. It never blocks.
func (sc *ServerConn) handlePlayer(m Message) error {
	switch m.Type {
	case MsgCommandAMF3:
		sc.unknown.Add(1)
	case MsgCommandAMF0:
		name, _, _, perr := parseCommand(m.Payload)
		switch {
		case perr != nil:
			sc.bad.Add(1) // skipped, as a publisher's
		case name == "closeStream", name == "deleteStream":
			return fmt.Errorf("rtmp: server: player sent %s: %w", name, io.EOF)
		case name == "receiveAudio", name == "receiveVideo", name == "pause", name == "seek":
		default:
			sc.unknown.Add(1)
		}
	}
	return nil
}

// WriteMedia sends one audio, video or data message to a player, always on
// message stream 1 (the one createStream returns; play is expected on it)
// and chunk stream 4 (audio), 6 (video) or 5 (data), with
// m.Timestamp as is. It must finish within the write timeout of the call
// (Config.WriteTimeout or SetWriteTimeout), the wait behind another write
// included; a write that misses it, or fails, ends the conn and its error
// becomes Err. m.Payload is not kept after WriteMedia returns. A publish
// conn, or a message of another type, is an error that leaves the conn as it
// was.
func (sc *ServerConn) WriteMedia(m Media) error {
	if !sc.req.Play {
		return errNotPlay
	}
	csid, ok := mediaCSID(m.Type)
	if !ok {
		return fmt.Errorf("rtmp: server: WriteMedia: message type %d is not audio, video or data", m.Type)
	}
	err := sc.s.writeMedia(csid, Message{Type: m.Type, StreamID: mediaStreamID, Timestamp: m.Timestamp, Payload: m.Payload})
	if err != nil {
		return fmt.Errorf("rtmp: server: %w", err)
	}
	return nil
}

// mediaCSID is the chunk stream of an outgoing audio (4), video (6) or data
// (5) message; ok is false for any other type.
func mediaCSID(typ uint8) (csid uint32, ok bool) {
	switch typ {
	case MsgAudio:
		return audioCSID, true
	case MsgVideo:
		return videoCSID, true
	case MsgDataAMF0:
		return dataCSID, true
	}
	return 0, false
}

// SetWriteTimeout replaces Config.WriteTimeout for the writes of a play conn
// that follow, so it is called before the first WriteMedia; d <= 0 restores
// Config.WriteTimeout (or its 10 s default). It is safe from any goroutine.
func (sc *ServerConn) SetWriteTimeout(d time.Duration) {
	sc.s.setWriteTimeout(d)
}

// EndOfStream tells a player the stream has ended: user control StreamEOF(1)
// and onStatus NetStream.Play.UnpublishNotify. It is best effort and leaves
// the conn open; the caller Closes it next. A failed write ends the conn.
func (sc *ServerConn) EndOfStream() error {
	if !sc.req.Play {
		return errNotPlay
	}
	s := sc.s
	err := s.sendUserControl(ucStreamEOF, mediaStreamID)
	if err == nil {
		err = s.writeCommand(mediaStreamID, "onStatus", 0, nil, Object{
			{Key: "level", Value: "status"},
			{Key: "code", Value: codeUnpublish},
			{Key: "description", Value: descStreamEnded},
		})
	}
	if err != nil {
		err = fmt.Errorf("rtmp: server: end of stream: %w", err)
		s.fail(err)
	}
	return err
}

// Done is closed once a play conn has ended: the player left (closeStream,
// deleteStream, the connection closed), a write failed or timed out, or
// Close was called. Err then tells why. A publish conn's Done never closes;
// its end is ReadMedia's error.
func (sc *ServerConn) Done() <-chan struct{} {
	return sc.s.Done()
}

// Err is why the conn ended (an error matching io.EOF when the player closed
// the stream, os.ErrDeadlineExceeded for a write timeout, net.ErrClosed
// after Close), or nil while it runs.
func (sc *ServerConn) Err() error {
	return sc.s.Err()
}
