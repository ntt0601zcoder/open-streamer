package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// caLife is how long a CA is valid; its leaves can outlive no test.
const caLife = 10 * 365 * 24 * time.Hour

// filesLife is the validity of the leaf Files writes.
const filesLife = 24 * time.Hour

// CA is a test certificate authority (ECDSA P-256), made in-process.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
}

// NewCA makes a CA with a fresh key, valid from an hour ago for ten years.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, "ca key", err)
	start := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "tlstest CA"},
		NotBefore:             start,
		NotAfter:              start.Add(caLife),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	check(t, "ca certificate", err)
	cert, err := x509.ParseCertificate(der)
	check(t, "ca certificate", err)
	return &CA{
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
	}
}

// Issue returns a leaf for hosts (DNS names and IP addresses), valid from an hour ago for d; d may be negative (expired).
// A positive d under one hour also yields a leaf that has already expired, since its validity starts an hour ago.
// A negative d makes a leaf that ended |d| before an hour ago and began an hour before that. The key is ECDSA P-256 in a
// PKCS #8 "PRIVATE KEY" block; each call makes a new key and a new random serial.
func (c *CA) Issue(t testing.TB, d time.Duration, hosts ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, "leaf key", err)
	start := time.Now().Add(-time.Hour)
	end := start.Add(d)
	if d < 0 {
		start = end.Add(-time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		NotBefore:    start,
		NotAfter:     end,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if len(hosts) > 0 {
		tmpl.Subject.CommonName = hosts[0]
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	check(t, "leaf certificate", err)
	k8, err := x509.MarshalPKCS8PrivateKey(key)
	check(t, "leaf key", err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k8})
}

// Files writes a leaf for hosts, valid 24 h, as cert.pem and key.pem (0600) in dir.
// Each file is written to a temporary name in dir and renamed into place, the certificate first, so a call on a dir
// that already holds a pair is a "full write" of a new one (a reader between the two renames sees the new
// certificate with the old key).
func (c *CA) Files(t testing.TB, dir string, hosts ...string) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM := c.Issue(t, filesLife, hosts...)
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	write(t, certFile, certPEM, 0o644)
	write(t, keyFile, keyPEM, 0o600)
	return certFile, keyFile
}

// Pool is the CA as a root pool; PEMFile writes the CA certificate into dir for a peer's -ca_file.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// PEMFile writes the CA certificate, one CERTIFICATE block, as ca.pem in dir and returns its path.
func (c *CA) PEMFile(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ca.pem")
	write(t, path, c.certPEM, 0o644)
	return path
}

// Server and Client are ready configs for a leaf of 127.0.0.1 and localhost, and for a client trusting the CA.
// Both have MinVersion TLS 1.2. Each Server call issues a new leaf, valid 24 h.
func (c *CA) Server(t testing.TB) *tls.Config {
	t.Helper()
	certPEM, keyPEM := c.Issue(t, filesLife, "127.0.0.1", "localhost")
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	check(t, "server pair", err)
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
}

// Client returns a config that trusts only the CA. The server name is left to the caller (crypto/tls takes it
// from the dialled address).
func (c *CA) Client() *tls.Config {
	return &tls.Config{RootCAs: c.Pool(), MinVersion: tls.VersionTLS12}
}

// serial returns a random 128-bit certificate serial.
func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	check(t, "serial", err)
	return n
}

// write puts data at path through a temporary file in the same directory
// and a rename, so a reader sees either the old content or the new.
func write(t testing.TB, path string, data []byte, mode os.FileMode) {
	t.Helper()
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	check(t, "write "+path, err)
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	check(t, "write "+path, err)
}

// check fails the test on err.
func check(t testing.TB, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("tlstest: %s: %v", what, err)
	}
}
