// Package rtmp is an independent RTMP library: the AMF0 codec, chunk stream
// framing, handshake, FLV tag mapping and publish/play sessions, client and
// server, used by the RTMP inputs and outputs.
//
// RTMPS is RTMP over TLS. A client gets it from an rtmps:// URL and
// Config.TLS; a server runs the TLS handshake itself and gives Accept the
// *tls.Conn. Closing a TLS session never waits more than 100 ms on the peer.
//
// It is written from the public Adobe RTMP and AMF0 specifications and imports
// nothing from internal/. Every Payload or Data slice it returns is a fresh
// allocation owned by the caller.
package rtmp
