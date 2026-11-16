// Package rtsp is a reusable RTSP 1.0 library: message framing, the Transport,
// Session and RTP-Info headers, and Basic and Digest client authentication.
// It depends only on the standard library and imports nothing from internal/,
// so a future RTSP server can reuse it. Every slice it returns is a fresh
// allocation owned by the caller.
package rtsp
