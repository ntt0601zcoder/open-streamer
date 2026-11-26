package server

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
)

// trackListener is the TCP listener under an HTTPS listener: it remembers
// each raw connection until it closes, so that the stop can close the ones
// still open itself. net/http's own close of a TLS connection sends a
// close_notify, which a peer that reads nothing can hold up for seconds
// (R-S8); the close of the raw connection never waits.
type trackListener struct {
	net.Listener

	mu     sync.Mutex
	conns  map[*trackedConn]struct{}
	closed bool // closeConns ran: nothing more is accepted
}

// trackedConn is one raw connection of a trackListener.
type trackedConn struct {
	net.Conn
	l *trackListener
}

// newTrackListener wraps ln.
func newTrackListener(ln net.Listener) *trackListener {
	return &trackListener{Listener: ln, conns: map[*trackedConn]struct{}{}}
}

// Accept returns the next connection, remembered until its Close.
func (l *trackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, l: l}
	l.mu.Lock()
	closed := l.closed
	if !closed {
		l.conns[tc] = struct{}{}
	}
	l.mu.Unlock()
	if closed {
		_ = c.Close()
		return nil, net.ErrClosed
	}
	return tc, nil
}

// Close closes the connection and forgets it.
func (c *trackedConn) Close() error {
	c.l.mu.Lock()
	delete(c.l.conns, c)
	c.l.mu.Unlock()
	return c.Conn.Close()
}

// closeConns closes every raw connection still open, at once: whatever
// reads or writes them fails. It is idempotent.
func (l *trackListener) closeConns() {
	l.mu.Lock()
	l.closed = true
	conns := l.conns
	l.conns = map[*trackedConn]struct{}{}
	l.mu.Unlock()
	for c := range conns {
		_ = c.Conn.Close()
	}
}

// open returns the number of raw connections not yet closed.
func (l *trackListener) open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

// warnKeyMode logs once, at the start, that the key file can be read by
// more than its owner (R-S2). It never refuses the file: a Kubernetes
// secret mount is 0644 or 0444 and must keep working. Unix only.
func warnKeyMode(log *slog.Logger, path string) {
	fi, err := os.Stat(path)
	if err != nil || !keyExposed(fi.Mode()) {
		return
	}
	log.Warn("server: tls: key_file can be read by its group or by others",
		"path", path, "mode", fmt.Sprintf("%04o", fi.Mode().Perm()))
}
