package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store is one certificate pair that can be read again while it is served.
type Store struct {
	certFile, keyFile string

	// cur is the pair in force. It is stored under mu, so Info sees it
	// together with the counters, and read without a lock by handshakes.
	cur atomic.Pointer[pair]

	// reload serialises Load and Reload, so the stamps in seen are those of
	// the read that set cur or failed last. It is taken before mu.
	reload sync.Mutex

	mu                         sync.Mutex
	seen                       stamps
	lastErr                    string
	changed, unchanged, failed uint64
}

// pair is one loaded certificate pair and what Info shows of it.
type pair struct {
	cert     *tls.Certificate
	notAfter time.Time
	names    []string
	loadedAt time.Time
}

// stamps are the two files' stats before a read; nil where the stat failed.
type stamps [2]os.FileInfo

// Info describes the pair in force and the reloads so far.
type Info struct {
	NotAfter  time.Time // the leaf's
	DNSNames  []string  // the leaf's SAN names and IP addresses, as text
	LoadedAt  time.Time // when the leaf in force was first read
	LastError string    // of the latest Reload; "" after a success
	// Reloads so far: Changed counts Load and every Reload that took a new leaf, Unchanged the ones that
	// found the same leaf, Failed the ones that kept the pair in force.
	Changed, Unchanged, Failed uint64
}

// Load reads and checks the pair. Its errors name the file and the reason, never file content.
// The certificate file may carry the chain after the leaf. An expired leaf loads: the files are the operator's word.
func Load(certFile, keyFile string) (*Store, error) {
	s := &Store{certFile: certFile, keyFile: keyFile}
	seen := s.stat()
	p, err := read(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("certs: load: %w", err)
	}
	p.loadedAt = time.Now()
	s.cur.Store(p)
	s.seen = seen
	s.changed = 1
	return s, nil
}

// Reload reads the pair again. On an error the pair in force stays. changed is false when the leaf is byte-identical.
// A pair whose leaf is the same is still taken, so a chain renewed under the same leaf is served; LoadedAt stays.
func (s *Store) Reload() (changed bool, err error) {
	s.reload.Lock()
	defer s.reload.Unlock()
	seen := s.stat()
	p, err := read(s.certFile, s.keyFile)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = seen
	if err != nil {
		err = fmt.Errorf("certs: reload: %w", err)
		s.failed++
		s.lastErr = err.Error()
		return false, err
	}
	s.lastErr = ""
	old := s.cur.Load()
	if bytes.Equal(old.cert.Certificate[0], p.cert.Certificate[0]) {
		p.loadedAt = old.loadedAt
		s.cur.Store(p)
		s.unchanged++
		return false, nil
	}
	p.loadedAt = time.Now()
	s.cur.Store(p)
	s.changed++
	return true, nil
}

// Watch stats both files every interval until ctx ends and calls Reload when a size or a modification time
// differs from the last look, and on every tick while the latest Reload failed (R-S3); report gets each
// Reload's result. It returns when ctx ends.
//
// The last look is the stat taken just before the latest Load or Reload, whoever called it. A file that is
// replaced by another one (a rename over it, a retargeted symlink) also counts as a difference. every must
// be positive; report may be nil.
func (s *Store) Watch(ctx context.Context, every time.Duration, report func(changed bool, err error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !s.due() {
			continue
		}
		changed, err := s.Reload()
		if report != nil {
			report(changed, err)
		}
	}
}

// ServerConfig returns a config whose GetCertificate serves the pair in force: MinVersion TLS 1.2, nothing else set.
func (s *Store) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.cur.Load().cert, nil
		},
	}
}

// Info describes the pair in force and the reloads so far.
func (s *Store) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.cur.Load()
	return Info{
		NotAfter:  p.notAfter,
		DNSNames:  slices.Clone(p.names),
		LoadedAt:  p.loadedAt,
		LastError: s.lastErr,
		Changed:   s.changed,
		Unchanged: s.unchanged,
		Failed:    s.failed,
	}
}

// Client returns the client config of R-S7: MinVersion TLS 1.2, RootCAs roots (nil means the system's),
// InsecureSkipVerify insecure. The caller's library sets ServerName.
func Client(roots *x509.CertPool, insecure bool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            roots,
		InsecureSkipVerify: insecure, //nolint:gosec // R-S7: the per-URL +insecure opt-out, never a default.
	}
}

// due reports whether Watch should read the pair again.
func (s *Store) due() bool {
	now := s.stat()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr != "" || !same(now[0], s.seen[0]) || !same(now[1], s.seen[1])
}

// stat stats both files, following symlinks.
func (s *Store) stat() stamps {
	var st stamps
	for i, name := range [2]string{s.certFile, s.keyFile} {
		if fi, err := os.Stat(name); err == nil {
			st[i] = fi
		}
	}
	return st
}

// same reports whether two stats show the same file with the same size and
// modification time.
func same(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && os.SameFile(a, b)
}

// read loads and checks the pair. Its errors start with the file at fault
// and never hold file content: both files are parsed here first, with fixed
// reasons, so whatever tls.X509KeyPair still refuses is a key that does not
// fit the certificate, in its own fixed words.
func read(certFile, keyFile string) (*pair, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	der, err := leafDER(certPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certFile, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%s: the certificate does not parse", certFile)
	}
	if err := checkKey(keyPEM); err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	cert.Leaf = leaf
	names := slices.Clone(leaf.DNSNames)
	for _, ip := range leaf.IPAddresses {
		names = append(names, net.IP(ip).String())
	}
	return &pair{cert: &cert, notAfter: leaf.NotAfter, names: names}, nil
}

// leafDER returns the first CERTIFICATE block of a PEM file.
func leafDER(data []byte) ([]byte, error) {
	blocks := pemBlocks(data)
	for _, b := range blocks {
		if b.Type == "CERTIFICATE" {
			return b.Bytes, nil
		}
	}
	if len(blocks) == 0 {
		return nil, errors.New("no PEM data")
	}
	return nil, errors.New("no CERTIFICATE block")
}

// checkKey finds the private key block as tls.X509KeyPair does (the first
// whose type ends in "PRIVATE KEY") and parses it as PKCS #1, PKCS #8 or
// SEC 1. Its reasons are fixed texts: a parser's own error can quote bytes
// of the key.
func checkKey(data []byte) error {
	blocks := pemBlocks(data)
	var der []byte
	found, cert := false, false
	for _, b := range blocks {
		if b.Type == "PRIVATE KEY" || strings.HasSuffix(b.Type, " PRIVATE KEY") {
			der, found = b.Bytes, true
			break
		}
		cert = cert || b.Type == "CERTIFICATE"
	}
	switch {
	case found:
	case len(blocks) == 0:
		return errors.New("no PEM data")
	case cert:
		return errors.New("holds a certificate, not a private key")
	default:
		return errors.New("no PRIVATE KEY block")
	}
	if _, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return nil
	}
	if _, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return nil
	}
	if _, err := x509.ParseECPrivateKey(der); err == nil {
		return nil
	}
	return errors.New("the private key does not parse as PKCS #1, PKCS #8 or SEC 1")
}

// pemBlocks returns every PEM block of data, in order.
func pemBlocks(data []byte) []*pem.Block {
	var out []*pem.Block
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return out
		}
		out = append(out, b)
	}
}
