// Package mpegts holds MPEG-TS helpers that complement go-astits: unwrapping
// of 33-bit PES timestamps into a continuous timeline, and parsing of ADTS
// AAC frames carried in PES payloads.
//
// The package is self-contained and does not depend on any other part of the
// module.
package mpegts
