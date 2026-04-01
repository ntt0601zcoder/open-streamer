package transcoder

import "github.com/ntthuan060102github/open-streamer/internal/domain"

// RenditionTarget binds one encoded ladder rung to its Buffer Hub output slot.
type RenditionTarget struct {
	BufferID domain.StreamCode
	Profile  Profile
}
