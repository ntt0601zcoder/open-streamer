package server

import (
	"log"
	"strings"
	"sync/atomic"
)

// handshakePrefix starts net/http's message for a TLS handshake that
// failed: "http: TLS handshake error from <remote>: <reason>".
const handshakePrefix = "http: TLS handshake error from "

// handshakeFailed writes the one debug line of a failed TLS handshake:
// "server: tls: handshake failed" with the listener ("https", "api",
// "rtmps" or "rtsps"), the remote address and the error. It counts nothing.
// Debug, because a port scanner must not fill the log at info (R-S6).
func (s *Server) handshakeFailed(listener, remote, reason string) {
	s.log.Debug("server: tls: handshake failed", "listener", listener, "remote", remote, "error", reason)
}

// tlsFailed is the hook of the RTMPS and RTSPS listeners for a failed
// handshake, which they count themselves. It only logs, so it never blocks
// its listener; once Run has begun to stop it says nothing.
func (s *Server) tlsFailed(listener, remote, reason string) {
	if s.stopping.Load() {
		return
	}
	s.handshakeFailed(listener, remote, reason)
}

// httpErrorLog is the ErrorLog of the HTTP server named listener ("https"
// for media, "api"); failures counts its TLS handshake failures.
//
// net/http's message for a failed handshake is never logged as it is: it is
// counted in failures and reported by handshakeFailed, and neither happens
// once Run has begun to stop. Every other message (a handler's panic with
// its stack, an accept error) is logged at error, as
// "server: <listener>: <message>".
func (s *Server) httpErrorLog(listener string, failures *atomic.Uint64) *log.Logger {
	return log.New(&httpLogWriter{s: s, listener: listener, failures: failures}, "", 0)
}

// httpLogWriter takes the messages of one http.Server's ErrorLog: a
// log.Logger makes one Write per message.
type httpLogWriter struct {
	s        *Server
	listener string
	failures *atomic.Uint64
}

// Write handles one message; it never fails.
func (w *httpLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	rest, ok := strings.CutPrefix(msg, handshakePrefix)
	if !ok {
		w.s.log.Error("server: " + w.listener + ": " + msg)
		return len(p), nil
	}
	if w.s.stopping.Load() {
		return len(p), nil // cut short by the stop: no failure of the peer
	}
	// The remote address has colons but no ": ": it ends at the first one.
	remote, reason, _ := strings.Cut(rest, ": ")
	w.failures.Add(1)
	w.s.handshakeFailed(w.listener, remote, reason)
	return len(p), nil
}
