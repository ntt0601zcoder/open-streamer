package rtmp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// User control event types (Adobe RTMP specification 1.0 §7.1.7).
const (
	ucStreamBegin      = 0
	ucStreamEOF        = 1
	ucStreamDry        = 2
	ucSetBufferLength  = 3
	ucStreamIsRecorded = 4
	ucPingRequest      = 6
	ucPingResponse     = 7
)

// Set Peer Bandwidth limit types (§5.4.5).
const (
	limitHard    = 0
	limitSoft    = 1
	limitDynamic = 2
)

// Session defaults we announce to a peer.
const (
	// defaultOutChunkSize is the chunk size we send after connect.
	defaultOutChunkSize = 4096
	// defaultWindowAckSize is the Window Ack Size and Set Peer Bandwidth we
	// send.
	defaultWindowAckSize = 2_500_000
	// minWindowAckSize is the smallest peer window honoured; a smaller one
	// would make us acknowledge after nearly every message.
	minWindowAckSize = 4096
)

// commandCSID carries AMF0 commands, on message stream 0 or a media stream.
const commandCSID = 3

// parseUserControl splits a user control payload into its event type and
// event data.
func parseUserControl(p []byte) (event uint16, data []byte, err error) {
	if len(p) < 2 {
		return 0, nil, errors.New("rtmp: session: short user control message")
	}
	return binary.BigEndian.Uint16(p), p[2:], nil
}

// appendUserControl builds a user control payload with 32-bit event data
// fields.
func appendUserControl(b []byte, event uint16, data ...uint32) []byte {
	b = binary.BigEndian.AppendUint16(b, event)
	for _, d := range data {
		b = binary.BigEndian.AppendUint32(b, d)
	}
	return b
}

// controlUint32 reads the leading 32-bit field of a protocol control message.
func controlUint32(m Message, what string) (uint32, error) {
	if len(m.Payload) < 4 {
		return 0, fmt.Errorf("rtmp: session: short %s", what)
	}
	return binary.BigEndian.Uint32(m.Payload), nil
}

// writeControl writes a protocol control or user control message on chunk
// stream 2, message stream 0.
func (s *session) writeControl(typ uint8, payload []byte) error {
	return s.write(false, func(cw *ChunkWriter) error {
		return cw.WriteMessage(controlCSID, Message{Type: typ, Payload: payload})
	})
}

// sendWindowAckSize tells the peer how many bytes it may send before it
// expects our acknowledgement.
func (s *session) sendWindowAckSize(n uint32) error {
	if err := s.writeControl(MsgWindowAckSize, binary.BigEndian.AppendUint32(nil, n)); err != nil {
		return err
	}
	s.sentWindow = n
	return nil
}

// sendPeerBandwidth limits the peer's output bandwidth.
func (s *session) sendPeerBandwidth(n uint32, limit uint8) error {
	return s.writeControl(MsgSetPeerBandwidth, append(binary.BigEndian.AppendUint32(nil, n), limit))
}

// setChunkSize sends Set Chunk Size defaultOutChunkSize and uses it for our
// following chunks.
func (s *session) setChunkSize() error {
	return s.write(false, func(cw *ChunkWriter) error { return cw.SetChunkSize(defaultOutChunkSize) })
}

// sendUserControl sends a user control event, such as StreamBegin(stream id)
// or SetBufferLength(stream id, ms).
func (s *session) sendUserControl(event uint16, data ...uint32) error {
	return s.writeControl(MsgUserControl, appendUserControl(nil, event, data...))
}

// writeCommand sends an AMF0 command on chunk stream 3 and message stream
// streamID (0 for NetConnection commands).
func (s *session) writeCommand(streamID uint32, vals ...any) error {
	p, err := EncodeAMF0(vals...)
	if err != nil {
		return fmt.Errorf("rtmp: session: encode command: %w", err)
	}
	return s.write(false, func(cw *ChunkWriter) error {
		return cw.WriteMessage(commandCSID, Message{Type: MsgCommandAMF0, StreamID: streamID, Payload: p})
	})
}

// handleControl consumes protocol control messages and ping user control
// events. It reports whether m was consumed; stream user control events are
// left to the caller.
func (s *session) handleControl(m Message) (bool, error) {
	switch m.Type {
	case MsgSetChunkSize, MsgAbort, MsgAck:
		// The chunk reader applied Set Chunk Size and Abort; the peer's
		// acknowledgements of our bytes need no action.
		return true, nil
	case MsgWindowAckSize:
		v, err := controlUint32(m, "Window Ack Size")
		if err != nil {
			return true, err
		}
		s.window = max(v, minWindowAckSize)
		return true, nil
	case MsgSetPeerBandwidth:
		return true, s.handlePeerBandwidth(m)
	case MsgUserControl:
		ev, data, err := parseUserControl(m.Payload)
		if err != nil {
			return true, err
		}
		switch ev {
		case ucPingRequest:
			if len(data) < 4 {
				return true, errors.New("rtmp: session: short PingRequest")
			}
			return true, s.sendUserControl(ucPingResponse, binary.BigEndian.Uint32(data))
		case ucPingResponse:
			return true, nil
		}
	}
	return false, nil
}

// handlePeerBandwidth applies a Set Peer Bandwidth (§5.4.5): a hard limit
// replaces ours, a soft one applies only when smaller, and a dynamic one acts
// as hard when the previous limit was hard and is otherwise ignored. When the
// limit in force differs from the last Window Ack Size we sent, we send one of
// that size.
func (s *session) handlePeerBandwidth(m Message) error {
	if len(m.Payload) < 5 {
		return errors.New("rtmp: session: short Set Peer Bandwidth")
	}
	n := binary.BigEndian.Uint32(m.Payload)
	limit := m.Payload[4]
	switch limit {
	case limitHard:
	case limitSoft:
		if s.hasPeerBW && n >= s.peerBW {
			return nil
		}
	case limitDynamic:
		if !s.hasPeerBW || s.peerBWLimit != limitHard {
			return nil
		}
		limit = limitHard
	default:
		return nil
	}
	s.peerBW, s.peerBWLimit, s.hasPeerBW = n, limit, true
	if n == s.sentWindow {
		return nil
	}
	return s.sendWindowAckSize(n)
}
