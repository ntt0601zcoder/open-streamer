// Package rtsp is a reusable RTSP 1.0 library: message framing, the Transport,
// Session and RTP-Info headers, Basic and Digest client authentication, SDP
// parsing, H.264/H.265/AAC depacketisation and packetisation, RTCP sender and
// receiver reports, a playing client (Dial, for rtsp:// and rtsps://) and a
// server for playing clients (NewServer; Serve, and ServeTLS for RTSPS, whose
// RTP is interleaved only). Besides the standard library it depends only on pion/rtp and
// pion/sdp, and it imports nothing from internal/. Every slice it returns is a
// fresh allocation owned by the caller.
package rtsp
