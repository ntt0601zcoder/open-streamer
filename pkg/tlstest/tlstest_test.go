package tlstest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve accepts one connection on a TLS listener with cfg and runs its
// handshake. The returned address is the listener's.
func serve(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tl := tls.NewListener(ln, cfg)
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = tl.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.(*tls.Conn).HandshakeContext(ctx)
			cancel()
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// dial runs a client handshake against addr, naming the server name.
func dial(addr, name string, cfg *tls.Config) error {
	cfg = cfg.Clone()
	cfg.ServerName = name
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

func TestCA(t *testing.T) {
	ca := NewCA(t)

	t.Run("server and client", func(t *testing.T) {
		addr := serve(t, ca.Server(t))
		for _, name := range []string{"127.0.0.1", "localhost"} {
			require.NoError(t, dial(addr, name, ca.Client()), name)
		}
		assert.Equal(t, uint16(tls.VersionTLS12), ca.Client().MinVersion)
		assert.Equal(t, uint16(tls.VersionTLS12), ca.Server(t).MinVersion)
	})

	t.Run("another CA", func(t *testing.T) {
		addr := serve(t, ca.Server(t))
		err := dial(addr, "127.0.0.1", NewCA(t).Client())
		var ua x509.UnknownAuthorityError
		require.ErrorAs(t, err, &ua)
	})

	t.Run("expired", func(t *testing.T) {
		certPEM, keyPEM := ca.Issue(t, -time.Minute, "127.0.0.1")
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		require.NoError(t, err)
		require.True(t, pair.Leaf.NotAfter.Before(time.Now()))
		require.True(t, pair.Leaf.NotBefore.Before(pair.Leaf.NotAfter))
		addr := serve(t, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
		err = dial(addr, "127.0.0.1", ca.Client())
		var ci x509.CertificateInvalidError
		require.ErrorAs(t, err, &ci)
		assert.Equal(t, x509.Expired, ci.Reason)
	})

	t.Run("hosts", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, time.Hour, "example.test", "::1", "10.0.0.1")
		b, _ := pem.Decode(certPEM)
		require.NotNil(t, b)
		leaf, err := x509.ParseCertificate(b.Bytes)
		require.NoError(t, err)
		assert.Equal(t, []string{"example.test"}, leaf.DNSNames)
		require.Len(t, leaf.IPAddresses, 2)
		assert.Equal(t, "::1", leaf.IPAddresses[0].String())
		assert.Equal(t, "10.0.0.1", leaf.IPAddresses[1].String())
		assert.WithinDuration(t, time.Now().Add(-time.Hour), leaf.NotBefore, time.Minute)
		assert.WithinDuration(t, leaf.NotBefore.Add(time.Hour), leaf.NotAfter, time.Second)
	})

	t.Run("files", func(t *testing.T) {
		dir := t.TempDir()
		certFile, keyFile := ca.Files(t, dir, "127.0.0.1", "localhost")
		assert.Equal(t, filepath.Join(dir, "cert.pem"), certFile)
		assert.Equal(t, filepath.Join(dir, "key.pem"), keyFile)
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(keyFile)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		}
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(23*time.Hour), pair.Leaf.NotAfter, time.Minute)
		addr := serve(t, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
		require.NoError(t, dial(addr, "localhost", ca.Client()))

		// A second call replaces both files with a new leaf.
		ca.Files(t, dir, "127.0.0.1")
		again, err := tls.LoadX509KeyPair(certFile, keyFile)
		require.NoError(t, err)
		assert.NotEqual(t, pair.Leaf.SerialNumber, again.Leaf.SerialNumber)
		left, err := filepath.Glob(filepath.Join(dir, "*.tmp*"))
		require.NoError(t, err)
		assert.Empty(t, left, "no temp file is left behind")
	})

	t.Run("pem file", func(t *testing.T) {
		path := ca.PEMFile(t, t.TempDir())
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		b, rest := pem.Decode(data)
		require.NotNil(t, b)
		assert.Equal(t, "CERTIFICATE", b.Type)
		assert.Empty(t, rest, "one block")

		pool := x509.NewCertPool()
		require.True(t, pool.AppendCertsFromPEM(data))
		certPEM, _ := ca.Issue(t, 2*time.Hour, "localhost")
		lb, _ := pem.Decode(certPEM)
		leaf, err := x509.ParseCertificate(lb.Bytes)
		require.NoError(t, err)
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"})
		require.NoError(t, err)
		_, err = leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: "localhost"})
		require.NoError(t, err)
		_, err = leaf.Verify(x509.VerifyOptions{Roots: NewCA(t).Pool(), DNSName: "localhost"})
		require.True(t, errors.As(err, new(x509.UnknownAuthorityError)))
	})
}
