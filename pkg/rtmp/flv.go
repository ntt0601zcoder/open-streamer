package rtmp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrUnsupportedCodec reports a codec or packet layout this package does not carry.
var ErrUnsupportedCodec = errors.New("rtmp: unsupported codec")

// VideoCodec is the video codec of a tag.
type VideoCodec uint8

// Video codecs carried by RTMP.
const (
	VideoAVC VideoCodec = iota + 1
	VideoHEVC
)

// VideoPacketKind is the packet type of a video tag.
type VideoPacketKind uint8

// Video packet kinds.
const (
	VideoSequenceStart VideoPacketKind = iota + 1
	VideoCodedFrames
	VideoSequenceEnd
	VideoMetadata
	VideoCommand
)

// VideoTag is a parsed VIDEODATA body. Data is a fresh allocation.
type VideoTag struct {
	Codec    VideoCodec
	Kind     VideoPacketKind
	Key      bool  // frame type 1
	CompTime int32 // ms, signed 24-bit; 0 for CodedFramesX
	// Data is the decoder configuration record for SequenceStart, and the
	// length-prefixed NAL units for CodedFrames (the length size comes from the record).
	Data []byte
}

// AudioTag is a parsed AAC AUDIODATA body. Data is a fresh allocation.
type AudioTag struct {
	SequenceHeader bool
	Data           []byte
}

// FLV tag types.
const (
	FLVAudio  = 8
	FLVVideo  = 9
	FLVScript = 18
)

const (
	frameKey     = 1
	frameCommand = 5

	exSeqStart   = 0
	exCoded      = 1
	exSeqEnd     = 2
	exCodedX     = 3
	exMetadata   = 4
	exMultitrack = 6
	exModEx      = 7

	codecIDAVC  = 7
	codecIDHEVC = 12
	soundAAC    = 10
)

var (
	fourccAVC  = [4]byte{'a', 'v', 'c', '1'}
	fourccHEVC = [4]byte{'h', 'v', 'c', '1'}
)

func errShort(what string) error { return fmt.Errorf("rtmp: flv: truncated %s", what) }

// ParseVideoTag parses a VIDEODATA body, legacy (codec ids 7 and 12) or enhanced RTMP.
func ParseVideoTag(b []byte) (VideoTag, error) {
	if len(b) < 1 {
		return VideoTag{}, errShort("video tag")
	}
	if b[0]&0x80 != 0 {
		return parseEnhancedVideo(b)
	}
	frame := b[0] >> 4
	t := VideoTag{Key: frame == frameKey}
	switch b[0] & 0x0F {
	case codecIDAVC:
		t.Codec = VideoAVC
	case codecIDHEVC:
		t.Codec = VideoHEVC
	default:
		return VideoTag{}, fmt.Errorf("rtmp: flv: video codec id %d: %w", b[0]&0x0F, ErrUnsupportedCodec)
	}
	if frame == frameCommand {
		t.Kind = VideoCommand
		t.Data = bytes.Clone(b[1:])
		return t, nil
	}
	if len(b) < 5 {
		return VideoTag{}, errShort("video packet")
	}
	switch b[1] {
	case 0:
		t.Kind = VideoSequenceStart
	case 1:
		t.Kind = VideoCodedFrames
		t.CompTime = si24(b[2:5])
	case 2:
		t.Kind = VideoSequenceEnd
	default:
		return VideoTag{}, fmt.Errorf("rtmp: flv: unknown AVC packet type %d", b[1])
	}
	t.Data = bytes.Clone(b[5:])
	return t, nil
}

func parseEnhancedVideo(b []byte) (VideoTag, error) {
	frame := (b[0] >> 4) & 7
	pkt := b[0] & 0x0F
	// Only frame type 1 is Key; type 4 (generated keyframe) is a seek frame, not a random access point.
	t := VideoTag{Key: frame == frameKey}
	pos := 1
	for pkt == exModEx {
		if len(b) < pos+1 {
			return VideoTag{}, errShort("ModEx size")
		}
		size := int(b[pos]) + 1
		pos++
		if size == 256 {
			if len(b) < pos+2 {
				return VideoTag{}, errShort("ModEx size")
			}
			size = int(binary.BigEndian.Uint16(b[pos:])) + 1
			pos += 2
		}
		if size+1 > len(b)-pos {
			return VideoTag{}, errShort("ModEx data")
		}
		pos += size
		pkt = b[pos] & 0x0F
		pos++
	}
	if frame == frameCommand && pkt != exMetadata {
		// A command frame carries a VideoFrameInfo byte and no codec.
		t.Kind = VideoCommand
		t.Data = bytes.Clone(b[pos:])
		return t, nil
	}
	if pkt == exMultitrack {
		return VideoTag{}, fmt.Errorf("rtmp: flv: multitrack video: %w", ErrUnsupportedCodec)
	}
	if len(b) < pos+4 {
		return VideoTag{}, errShort("FourCC")
	}
	switch [4]byte(b[pos : pos+4]) {
	case fourccAVC:
		t.Codec = VideoAVC
	case fourccHEVC:
		t.Codec = VideoHEVC
	default:
		return VideoTag{}, fmt.Errorf("rtmp: flv: FourCC %q: %w", b[pos:pos+4], ErrUnsupportedCodec)
	}
	pos += 4
	switch pkt {
	case exSeqStart:
		t.Kind = VideoSequenceStart
	case exCoded:
		if len(b) < pos+3 {
			return VideoTag{}, errShort("composition time")
		}
		t.Kind = VideoCodedFrames
		t.CompTime = si24(b[pos:])
		pos += 3
	case exCodedX:
		t.Kind = VideoCodedFrames
	case exSeqEnd:
		t.Kind = VideoSequenceEnd
	case exMetadata:
		t.Kind = VideoMetadata
	default:
		return VideoTag{}, fmt.Errorf("rtmp: flv: video packet type %d: %w", pkt, ErrUnsupportedCodec)
	}
	t.Data = bytes.Clone(b[pos:])
	return t, nil
}

func si24(b []byte) int32 {
	v := int32(b[0])<<16 | int32(b[1])<<8 | int32(b[2])
	return v << 8 >> 8
}

// ParseAudioTag parses an AAC AUDIODATA body; other sound formats return ErrUnsupportedCodec.
func ParseAudioTag(b []byte) (AudioTag, error) {
	if len(b) < 1 {
		return AudioTag{}, errShort("audio tag")
	}
	if b[0]>>4 != soundAAC {
		return AudioTag{}, fmt.Errorf("rtmp: flv: sound format %d: %w", b[0]>>4, ErrUnsupportedCodec)
	}
	if len(b) < 2 {
		return AudioTag{}, errShort("AAC packet")
	}
	switch b[1] {
	case 0, 1:
	default:
		return AudioTag{}, fmt.Errorf("rtmp: flv: unknown AAC packet type %d", b[1])
	}
	return AudioTag{SequenceHeader: b[1] == 0, Data: bytes.Clone(b[2:])}, nil
}

// AppendVideoTag appends the VIDEODATA body for t. enhanced selects the enhanced
// RTMP layout (FourCC, CodedFramesX for a zero composition time); otherwise the
// legacy layout is used, where VideoMetadata has no representation and is omitted.
func AppendVideoTag(dst []byte, t VideoTag, enhanced bool) []byte {
	frame := byte(2)
	if t.Key {
		frame = frameKey
	}
	if t.Kind == VideoCommand {
		frame = frameCommand
	}
	if enhanced {
		if t.Kind == VideoCommand {
			return append(append(dst, 0x80|frame<<4), t.Data...)
		}
		var pkt byte
		switch t.Kind {
		case VideoSequenceStart:
			pkt = exSeqStart
		case VideoCodedFrames:
			pkt = exCoded
			if t.CompTime == 0 {
				pkt = exCodedX
			}
		case VideoSequenceEnd:
			pkt = exSeqEnd
		case VideoMetadata:
			pkt = exMetadata
		case VideoCommand: // handled above
		}
		fcc := fourccAVC
		if t.Codec == VideoHEVC {
			fcc = fourccHEVC
		}
		dst = append(dst, 0x80|frame<<4|pkt)
		dst = append(dst, fcc[:]...)
		if pkt == exCoded {
			dst = appendSI24(dst, t.CompTime)
		}
		return append(dst, t.Data...)
	}
	id := byte(codecIDAVC)
	if t.Codec == VideoHEVC {
		id = codecIDHEVC
	}
	switch t.Kind {
	case VideoCommand:
		return append(append(dst, frame<<4|id), t.Data...)
	case VideoMetadata:
		return dst
	case VideoSequenceStart:
		dst = append(dst, frame<<4|id, 0, 0, 0, 0)
	case VideoCodedFrames:
		dst = appendSI24(append(dst, frame<<4|id, 1), t.CompTime)
	case VideoSequenceEnd:
		dst = append(dst, frame<<4|id, 2, 0, 0, 0)
	}
	return append(dst, t.Data...)
}

func appendSI24(dst []byte, v int32) []byte {
	return append(dst, byte(v>>16), byte(v>>8), byte(v))
}

// AppendAudioTag appends the AUDIODATA body for an AAC tag (44 kHz, 16-bit, stereo flags).
func AppendAudioTag(dst []byte, t AudioTag) []byte {
	typ := byte(1)
	if t.SequenceHeader {
		typ = 0
	}
	dst = append(dst, soundAAC<<4|0x0F, typ)
	return append(dst, t.Data...)
}

// FLVTag is one tag of an FLV file.
type FLVTag struct {
	Type      uint8
	Timestamp uint32
	Data      []byte
}

const maxFLVTag = 8 << 20

// errFLVTagTooLarge is wrapped when a tag declares more than maxFLVTag bytes.
var errFLVTagTooLarge = errors.New("rtmp: flv: tag too large")

// ReadFLV reads an FLV file: the header and every tag, each at most 8 MiB.
func ReadFLV(r io.Reader) ([]FLVTag, error) {
	var hdr [9]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("rtmp: flv: header: %w", err)
	}
	if string(hdr[:3]) != "FLV" {
		return nil, errors.New("rtmp: flv: bad signature")
	}
	if off := binary.BigEndian.Uint32(hdr[5:]); off > 9 {
		if _, err := io.CopyN(io.Discard, r, int64(off-9)); err != nil {
			return nil, fmt.Errorf("rtmp: flv: header: %w", err)
		}
	}
	var tags []FLVTag
	for {
		var prev [4]byte // PreviousTagSize
		if _, err := io.ReadFull(r, prev[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return tags, nil
			}
			return nil, fmt.Errorf("rtmp: flv: tag size: %w", err)
		}
		var th [11]byte
		if _, err := io.ReadFull(r, th[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return tags, nil
			}
			return nil, fmt.Errorf("rtmp: flv: tag header: %w", err)
		}
		n := int(th[1])<<16 | int(th[2])<<8 | int(th[3])
		if n > maxFLVTag {
			return nil, fmt.Errorf("rtmp: flv: tag of %d bytes exceeds %d: %w", n, maxFLVTag, errFLVTagTooLarge)
		}
		ts := uint32(th[4])<<16 | uint32(th[5])<<8 | uint32(th[6]) | uint32(th[7])<<24
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("rtmp: flv: tag body: %w", err)
		}
		tags = append(tags, FLVTag{Type: th[0], Timestamp: ts, Data: data})
	}
}
