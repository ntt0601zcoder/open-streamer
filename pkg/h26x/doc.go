// Package h26x is the module's own parser for H.264 (ITU-T H.264) and H.265
// (ITU-T H.265) parameter sets: an RBSP bit reader with exp-Golomb codes,
// sequence and picture parameter sets parsed through the VUI, and the
// decoding-order facts derived from them (reorder depth and frame duration).
//
// The parser is written for hostile input: exp-Golomb codes longer than 32
// bits fail, every count read from a bitstream is checked against its range
// in the specification before any loop or allocation, and a malformed set
// returns an error instead of panicking. Every slice it returns is a fresh
// allocation owned by the caller.
//
// The package is self-contained and does not depend on any other part of the
// module.
package h26x
