package rtmp

// RTMP message type ids (Adobe RTMP specification 1.0 §5.4 and §7.1).
const (
	MsgSetChunkSize     = 1
	MsgAbort            = 2
	MsgAck              = 3
	MsgUserControl      = 4
	MsgWindowAckSize    = 5
	MsgSetPeerBandwidth = 6
	MsgAudio            = 8
	MsgVideo            = 9
	MsgDataAMF3         = 15
	MsgCommandAMF3      = 17
	MsgDataAMF0         = 18
	MsgCommandAMF0      = 20
	MsgAggregate        = 22
)

// Message is one RTMP message, reassembled from its chunks. Timestamp is in
// milliseconds as carried on the wire, so it wraps at 2^32. A Payload returned
// by ChunkReader is a fresh allocation owned by the caller.
type Message struct {
	Type      uint8
	StreamID  uint32
	Timestamp uint32
	Payload   []byte
}
