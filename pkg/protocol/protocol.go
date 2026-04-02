// Package protocol provides URL-based protocol detection and media stream utilities.
package protocol

import (
	"net"
	"net/url"
	"strings"
)

// Kind classifies a stream URL into a transport category used internally by
// the ingestor to choose the right reader implementation.
type Kind string

// Kind constants classify ingest URLs; see Detect.
const (
	KindUDP     Kind = "udp"     // raw MPEG-TS over UDP (unicast or multicast)
	KindHLS     Kind = "hls"     // HLS playlist pull over HTTP/HTTPS
	KindFile    Kind = "file"    // local filesystem path
	KindRTMP    Kind = "rtmp"    // RTMP / RTMPS (pull or push-listen)
	KindRTSP    Kind = "rtsp"    // RTSP pull
	KindSRT     Kind = "srt"     // SRT (pull caller or push listener)
	KindPublish Kind = "publish" // accept any push protocol; stream code is the routing key
	KindUnknown Kind = "unknown"
)

// Detect returns the protocol Kind for the given URL.
// All classification is done purely from the scheme and URL structure — the
// caller never needs to specify the protocol manually.
//
//	rtmp://...                → KindRTMP
//	rtmps://...               → KindRTMP
//	srt://...                 → KindSRT
//	udp://...                 → KindUDP
//	rtsp:// or rtsps://...    → KindRTSP
//	http(s)://...*.m3u8       → KindHLS
//	file:// or /absolute/path → KindFile
//	publish://                → KindPublish (push-listen, any protocol)
func Detect(rawURL string) Kind {
	u, err := url.Parse(rawURL)
	if err != nil {
		return KindUnknown
	}

	switch strings.ToLower(u.Scheme) {
	case "rtmp", "rtmps":
		return KindRTMP
	case "srt":
		return KindSRT
	case "udp":
		return KindUDP
	case "rtsp", "rtsps":
		return KindRTSP
	case "file":
		return KindFile
	case "publish":
		return KindPublish
	case "http", "https":
		if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") ||
			strings.HasSuffix(strings.ToLower(u.Path), ".m3u") {
			return KindHLS
		}
		return KindUnknown
	case "":
		// Bare path — treat as local file.
		if strings.HasPrefix(rawURL, "/") {
			return KindFile
		}
	}

	return KindUnknown
}

// IsPushListen returns true when the URL signals that the server should
// accept incoming encoder connections (push mode) for a stream.
//
// Two forms are recognised:
//
//  1. publish:// — the preferred, protocol-agnostic form.  Encoders may push
//     via RTMP or SRT; the stream code is the only routing key needed.
//
//  2. Legacy wildcard-host form — rtmp://0.0.0.0:port/... or srt://0.0.0.0:port/...
//     Still recognised for backward compatibility.
//
// Examples:
//
//	publish://          → true  (preferred)
//	rtmp://0.0.0.0:1935 → true  (legacy)
//	srt://0.0.0.0:9999  → true  (legacy)
//	rtmp://server.com   → false (remote pull source)
func IsPushListen(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "publish" {
		return true
	}
	if scheme != "rtmp" && scheme != "rtmps" && scheme != "srt" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return true // bare port like ":1935"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// DNS name → definitely a remote server, not a local bind address.
		return false
	}
	return ip.IsUnspecified() || ip.IsLoopback()
}

// IsMPEGTS returns true when data begins with the MPEG-TS sync byte 0x47.
func IsMPEGTS(data []byte) bool {
	return len(data) >= 188 && data[0] == 0x47
}

// SplitTSPackets splits a raw byte slice into 188-byte MPEG-TS packets.
// Incomplete trailing bytes are discarded.
func SplitTSPackets(data []byte) [][]byte {
	var packets [][]byte
	for i := 0; i+188 <= len(data); i += 188 {
		pkt := make([]byte, 188)
		copy(pkt, data[i:i+188])
		packets = append(packets, pkt)
	}
	return packets
}
