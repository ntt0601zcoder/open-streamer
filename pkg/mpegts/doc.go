// Package mpegts is the module's own MPEG-TS layer (ISO/IEC 13818-1): a
// packet reader that resynchronises after loss or garbage and works on
// non-seekable input, a demultiplexer that follows the PAT and PMT and
// reassembles PES packets with continuity checking, a writer for
// single-program streams, unwrapping of 33-bit PES timestamps into a
// continuous timeline, and parsing of ADTS AAC frames carried in PES
// payloads.
//
// The package is self-contained, does not depend on any other part of the
// module, and replaces a third-party TS library.
package mpegts
