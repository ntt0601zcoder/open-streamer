// Package rtsp is a reusable RTSP 1.0 library: message framing, the Transport,
// Session and RTP-Info headers, Basic and Digest client authentication, SDP
// parsing, H.264/H.265/AAC depacketisation, RTCP sender and receiver reports,
// and a playing client (Dial). Besides the standard library it depends only on
// pion/rtp and pion/sdp, and it imports nothing from internal/, so a future
// RTSP server can reuse it. Every slice it returns is a fresh allocation owned
// by the caller.
package rtsp
