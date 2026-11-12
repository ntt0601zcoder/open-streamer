// Package hls encodes and parses HLS media and multivariant playlists
// (RFC 8216). It has no dependencies outside the standard library.
//
// The parsers are tolerant of real-world variations: CRLF line endings, a
// leading byte order mark, blank lines, comments and unknown tags are
// accepted; malformed values of known tags are reported as errors.
package hls
