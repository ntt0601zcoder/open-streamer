// Package dash encodes and parses a live DASH MPD subset (ISO/IEC 23009-1,
// the isoff-live profile) with encoding/xml. It has no dependencies outside
// the standard library.
//
// The parser is tolerant: attributes and elements it does not model, as
// written by other packagers, are ignored. Durations use the ISO 8601
// xs:duration form and times use RFC 3339.
package dash
