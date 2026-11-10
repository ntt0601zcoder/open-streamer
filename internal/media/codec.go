// Package media defines the types that cross package boundaries on the media path.
package media

// Kind is the media type of a track.
type Kind uint8

// Track kinds.
const (
	KindVideo Kind = 1
	KindAudio Kind = 2
)

// Codec identifies a supported elementary stream codec.
type Codec uint8

// Supported codecs.
const (
	CodecH264 Codec = 1
	CodecH265 Codec = 2
	CodecAAC  Codec = 3
)

// Kind returns the media kind of the codec, or 0 for an unknown codec.
func (c Codec) Kind() Kind {
	switch c {
	case CodecH264, CodecH265:
		return KindVideo
	case CodecAAC:
		return KindAudio
	default:
		return 0
	}
}

// String returns the lowercase codec name.
func (c Codec) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	case CodecAAC:
		return "aac"
	default:
		return "unknown"
	}
}
