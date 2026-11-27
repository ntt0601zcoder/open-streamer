package server

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/certs"
)

// certLoaded writes the start's line of the pair in force (F-18), unless a
// reload before Run (a SIGHUP) already logged that pair.
func (s *Server) certLoaded() {
	s.certMu.Lock()
	defer s.certMu.Unlock()
	info := s.store.Info()
	if info.Changed == s.certShown {
		return
	}
	s.certLine(info)
}

// certLine writes the line of one loaded pair: "server: tls: certificate
// loaded" with not_after and names, at info, or at warning with expired when
// the leaf has expired (R-S3: the files are the operator's word). The caller
// holds certMu. It remembers which pair it was by info.Changed, so a pair is
// never logged twice.
func (s *Server) certLine(info certs.Info) {
	s.certShown = info.Changed
	names := info.DNSNames
	if names == nil {
		names = []string{} // [] in the log, not null
	}
	attrs := []any{"not_after", info.NotAfter, "names", names}
	if time.Now().After(info.NotAfter) {
		s.log.Warn("server: tls: certificate loaded", append(attrs, "expired", true)...)
		return
	}
	s.log.Info("server: tls: certificate loaded", attrs...)
}

// certReload is a read of the pair outside the poll, by Reload: the read and
// what it took are one step under certMu, so its line is its own pair's.
func (s *Server) certReload() {
	s.certMu.Lock()
	defer s.certMu.Unlock()
	changed, err := s.store.Reload()
	s.certReportLocked(changed, err, s.store.Info())
}

// certReport is the poll's report of one read of the pair (Store.Watch).
func (s *Server) certReport(changed bool, err error) {
	s.certMu.Lock()
	defer s.certMu.Unlock()
	s.certReportLocked(changed, err, s.store.Info())
}

// certReportLocked reports one read of the pair, with info read after it
// under certMu: a new leaf is logged; a failure is logged at error once per
// distinct text, not again until a read succeeds or the text changes; a read
// that found the same leaf logs nothing. The text names the file and the
// reason, never the key's content (internal/certs).
//
// Only Reload reads the pair under certMu; the poll's read runs inside
// Store.Watch, whose report carries no leaf. So info is the pair in force
// after this read and any of Reload's that came between: when such a read
// took a new leaf (Changed moved past the one this read made) or succeeded
// (LastError is no longer this text), its own report says it, and this one
// says nothing.
func (s *Server) certReportLocked(changed bool, err error, info certs.Info) {
	if err != nil {
		text := err.Error()
		if text == s.certLogged || info.LastError != text {
			return
		}
		s.certLogged = text
		s.log.Error("server: tls: reload failed", "error", text)
		return
	}
	s.certLogged = ""
	if changed && info.Changed != s.certShown {
		s.certLine(info)
	}
}

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
