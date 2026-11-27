package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

const leafLife = 24 * time.Hour

// pairPEM is one issued leaf and its key.
type pairPEM struct{ cert, key []byte }

func issue(t *testing.T, ca *tlstest.CA, d time.Duration) pairPEM {
	t.Helper()
	c, k := ca.Issue(t, d, "127.0.0.1", "localhost")
	return pairPEM{c, k}
}

func serialOf(t *testing.T, certPEM []byte) *big.Int {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	require.NotNil(t, b)
	c, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return c.SerialNumber
}

func leafOf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	require.NotNil(t, b)
	c, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return c
}

// writeFile replaces path with data through a temporary name and a rename.
func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, data, mode))
	require.NoError(t, os.Chmod(tmp, mode))
	require.NoError(t, os.Rename(tmp, path))
}

// fullWrite writes both files of p to temp names and renames each into
// place, the certificate first.
func fullWrite(t *testing.T, dir string, p pairPEM) (certFile, keyFile string) {
	t.Helper()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	writeFile(t, certFile, p.cert, 0o644)
	writeFile(t, keyFile, p.key, 0o600)
	return certFile, keyFile
}

// echo serves cfg on 127.0.0.1 and echoes every connection's bytes.
func echo(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tl := tls.NewListener(ln, cfg)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = tl.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Go(func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			})
		}
	})
	return ln.Addr().String()
}

// dial completes a handshake with addr, trusting ca.
func dial(t *testing.T, addr string, ca *tlstest.CA) *tls.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := (&tls.Dialer{Config: ca.Client()}).DialContext(ctx, "tcp", addr)
	require.NoError(t, err)
	return c.(*tls.Conn)
}

// served returns the serial a new handshake with addr presents.
func served(t *testing.T, addr string, ca *tlstest.CA) *big.Int {
	t.Helper()
	c := dial(t, addr, ca)
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0].SerialNumber
}

// base64Body returns the base64 text of every PEM block in data, without
// line breaks.
func base64Body(data []byte) string {
	var b strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		b.WriteString(strings.TrimSpace(line))
	}
	return b.String()
}

// assertNoKeyBytes fails if msg holds any 8-character run of the key's
// base64 body.
func assertNoKeyBytes(t *testing.T, msg string, keyPEM []byte) {
	t.Helper()
	body := base64Body(keyPEM)
	require.NotEmpty(t, body)
	for i := 0; i+8 <= len(body); i++ {
		if strings.Contains(msg, body[i:i+8]) {
			t.Fatalf("error %q holds key bytes %q", msg, body[i:i+8])
		}
	}
}

func TestLoad(t *testing.T) {
	ca := tlstest.NewCA(t)

	t.Run("good pair", func(t *testing.T) {
		p := issue(t, ca, leafLife)
		certFile, keyFile := fullWrite(t, t.TempDir(), p)
		before := time.Now()
		s, err := Load(certFile, keyFile)
		require.NoError(t, err)
		in := s.Info()
		leaf := leafOf(t, p.cert)
		assert.True(t, leaf.NotAfter.Equal(in.NotAfter))
		assert.Contains(t, in.DNSNames, "localhost")
		assert.Contains(t, in.DNSNames, "127.0.0.1")
		assert.False(t, in.LoadedAt.Before(before))
		assert.Empty(t, in.LastError)
		assert.Equal(t, uint64(1), in.Changed)
		assert.Zero(t, in.Unchanged)
		assert.Zero(t, in.Failed)

		in.DNSNames[0] = "changed"
		assert.NotContains(t, s.Info().DNSNames, "changed", "Info returns a copy")

		addr := echo(t, s.ServerConfig())
		assert.Equal(t, leaf.SerialNumber, served(t, addr, ca))
	})

	t.Run("expired pair loads", func(t *testing.T) {
		p := issue(t, ca, -time.Minute)
		s, err := Load(fullWrite(t, t.TempDir(), p))
		require.NoError(t, err)
		assert.True(t, s.Info().NotAfter.Before(time.Now()))
	})

	t.Run("chain", func(t *testing.T) {
		p := issue(t, ca, leafLife)
		caPEM, err := os.ReadFile(ca.PEMFile(t, t.TempDir()))
		require.NoError(t, err)
		chain := pairPEM{append(bytes.Clone(p.cert), caPEM...), p.key}
		s, err := Load(fullWrite(t, t.TempDir(), chain))
		require.NoError(t, err)
		assert.True(t, leafOf(t, p.cert).NotAfter.Equal(s.Info().NotAfter), "the first block is the leaf")
	})

	t.Run("key encodings", func(t *testing.T) {
		// SEC 1, after an EC PARAMETERS block as openssl ecparam -genkey writes it.
		p := issue(t, ca, leafLife)
		b, _ := pem.Decode(p.key)
		k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
		require.NoError(t, err)
		sec1, err := x509.MarshalECPrivateKey(k.(*ecdsa.PrivateKey))
		require.NoError(t, err)
		key := append(pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{6, 8, 42, 134, 72, 206, 61, 3, 1, 7}}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})...)
		_, err = Load(fullWrite(t, t.TempDir(), pairPEM{p.cert, key}))
		require.NoError(t, err)

		// PKCS #1, with a self-signed RSA certificate.
		rk, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(7),
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			DNSNames:     []string{"localhost"},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rk.PublicKey, rk)
		require.NoError(t, err)
		rsaPair := pairPEM{
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rk)}),
		}
		s, err := Load(fullWrite(t, t.TempDir(), rsaPair))
		require.NoError(t, err)
		assert.Equal(t, []string{"localhost"}, s.Info().DNSNames)
	})

	good := issue(t, ca, leafLife)
	other := issue(t, ca, leafLife)
	for _, c := range []struct {
		name      string
		cert, key []byte
		missing   string // "cert" or "key": that file does not exist
		blame     string // "cert" or "key": the file the error names
		reason    string // when set, the error's exact fixed text after the file
	}{
		{name: "missing cert", key: good.key, missing: "cert", blame: "cert"},
		{name: "missing key", cert: good.cert, missing: "key", blame: "key"},
		{name: "key of another certificate", cert: good.cert, key: other.key, blame: "key"},
		{name: "cert not PEM", cert: []byte("not a certificate\n"), key: good.key, blame: "cert"},
		{name: "key not PEM", cert: good.cert, key: []byte("not a key\n"), blame: "key", reason: "no PEM data"},
		{name: "certificate as the key", cert: good.cert, key: good.cert, blame: "key", reason: "holds a certificate, not a private key"},
		{name: "key as the certificate", cert: good.key, key: good.key, blame: "cert"},
		{name: "cert PEM without a certificate", cert: pem.EncodeToMemory(&pem.Block{Type: "OTHER", Bytes: []byte{1}}), key: good.key, blame: "cert"},
		{name: "key PEM without a key", cert: good.cert, key: pem.EncodeToMemory(&pem.Block{Type: "OTHER", Bytes: []byte{1}}), blame: "key", reason: "no PRIVATE KEY block"},
		{name: "cert block not DER", cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}), key: good.key, blame: "cert", reason: "the certificate does not parse"},
		{name: "key block not DER", cert: good.cert, key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}), blame: "key", reason: "the private key does not parse as PKCS #1, PKCS #8 or SEC 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			certFile := filepath.Join(dir, "cert.pem")
			keyFile := filepath.Join(dir, "key.pem")
			if c.missing != "cert" {
				writeFile(t, certFile, c.cert, 0o644)
			}
			if c.missing != "key" {
				writeFile(t, keyFile, c.key, 0o600)
			}
			s, err := Load(certFile, keyFile)
			require.Error(t, err)
			assert.Nil(t, s)
			msg := err.Error()
			t.Log(msg)
			assert.True(t, strings.HasPrefix(msg, "certs: load: "), msg)
			blamed := keyFile
			if c.blame == "cert" {
				blamed = certFile
			}
			assert.Contains(t, msg, blamed)
			if c.reason != "" {
				assert.Equal(t, "certs: load: "+blamed+": "+c.reason, msg)
			}
			assertNoKeyBytes(t, msg, good.key)
			assertNoKeyBytes(t, msg, other.key)
		})
	}
}

func TestStoreReloadKeepsOld(t *testing.T) {
	ca := tlstest.NewCA(t)
	dir := t.TempDir()
	old := issue(t, ca, leafLife)
	s, err := Load(fullWrite(t, dir, old))
	require.NoError(t, err)
	addr := echo(t, s.ServerConfig())
	oldSerial := serialOf(t, old.cert)
	require.Equal(t, oldSerial, served(t, addr, ca))
	loadedAt := s.Info().LoadedAt

	// Half a rotation: the new certificate with the old key.
	next := issue(t, ca, leafLife)
	writeFile(t, filepath.Join(dir, "cert.pem"), next.cert, 0o644)
	changed, err := s.Reload()
	require.Error(t, err)
	assert.False(t, changed)
	assert.True(t, strings.HasPrefix(err.Error(), "certs: reload: "), err.Error())
	assertNoKeyBytes(t, err.Error(), old.key)
	assert.Equal(t, oldSerial, served(t, addr, ca), "the pair in force stays")
	in := s.Info()
	assert.Equal(t, uint64(1), in.Failed)
	assert.Equal(t, err.Error(), in.LastError)
	assert.Equal(t, uint64(1), in.Changed)
	assert.True(t, loadedAt.Equal(in.LoadedAt))

	// The rotation completes.
	writeFile(t, filepath.Join(dir, "key.pem"), next.key, 0o600)
	changed, err = s.Reload()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, serialOf(t, next.cert), served(t, addr, ca))
	in = s.Info()
	assert.Empty(t, in.LastError)
	assert.Equal(t, uint64(2), in.Changed)
	assert.Equal(t, uint64(1), in.Failed)
	assert.True(t, leafOf(t, next.cert).NotAfter.Equal(in.NotAfter))
	assert.False(t, in.LoadedAt.Before(loadedAt))
	loadedAt = in.LoadedAt

	// Nothing changed.
	changed, err = s.Reload()
	require.NoError(t, err)
	assert.False(t, changed)
	in = s.Info()
	assert.Equal(t, uint64(1), in.Unchanged)
	assert.Equal(t, uint64(2), in.Changed)
	assert.True(t, loadedAt.Equal(in.LoadedAt), "an unchanged leaf keeps LoadedAt")
}

func TestStoreServesInFlight(t *testing.T) {
	ca := tlstest.NewCA(t)
	dir := t.TempDir()
	a, b := issue(t, ca, leafLife), issue(t, ca, leafLife)
	sa, sb := serialOf(t, a.cert), serialOf(t, b.cert)
	s, err := Load(fullWrite(t, dir, a))
	require.NoError(t, err)
	addr := echo(t, s.ServerConfig())

	// A connection made before a Reload keeps working after it.
	c := dial(t, addr, ca)
	defer c.Close()
	require.Equal(t, sa, c.ConnectionState().PeerCertificates[0].SerialNumber)
	roundTrip := func(msg string) {
		t.Helper()
		require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
		_, err := c.Write([]byte(msg))
		require.NoError(t, err)
		got := make([]byte, len(msg))
		_, err = io.ReadFull(c, got)
		require.NoError(t, err)
		require.Equal(t, msg, string(got))
	}
	roundTrip("before")
	fullWrite(t, dir, b)
	changed, err := s.Reload()
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, sb, served(t, addr, ca))
	roundTrip("after")

	// Handshakes during Reloads see one pair or the other.
	var wg sync.WaitGroup
	serials := make(chan *big.Int, 50)
	errs := make(chan error, 50)
	for range 50 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := (&tls.Dialer{Config: ca.Client()}).DialContext(ctx, "tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			serials <- conn.(*tls.Conn).ConnectionState().PeerCertificates[0].SerialNumber
			_ = conn.Close()
		})
	}
	for i := range 50 {
		if i%2 == 0 {
			fullWrite(t, dir, a)
		} else {
			fullWrite(t, dir, b)
		}
		_, err := s.Reload()
		require.NoError(t, err)
	}
	wg.Wait()
	close(serials)
	close(errs)
	for err := range errs {
		t.Errorf("handshake: %v", err)
	}
	n := 0
	for sn := range serials {
		n++
		assert.True(t, sn.Cmp(sa) == 0 || sn.Cmp(sb) == 0, "serial %v", sn)
	}
	assert.Equal(t, 50, n)
	roundTrip("still")
}

// result is one report of Watch.
type result struct {
	changed bool
	err     error
}

// recorder collects Watch's reports.
type recorder struct {
	mu   sync.Mutex
	list []result
}

func (r *recorder) report(changed bool, err error) {
	r.mu.Lock()
	r.list = append(r.list, result{changed, err})
	r.mu.Unlock()
}

func (r *recorder) take() []result {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.list
	r.list = nil
	return l
}

func (r *recorder) snapshot() []result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]result(nil), r.list...)
}

// waitFor polls the recorder until match finds a report, for up to 1 s.
func (r *recorder) waitFor(t *testing.T, what string, match func(result) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, x := range r.snapshot() {
			if match(x) {
				return true
			}
		}
		return false
	}, time.Second, 2*time.Millisecond, what)
}

func isOK(changed bool) func(result) bool {
	return func(x result) bool { return x.err == nil && x.changed == changed }
}

func isErr(x result) bool { return x.err != nil }

// watch runs s.Watch every 10 ms with a recorder; stop cancels it and
// waits for it to return.
func watch(t *testing.T, s *Store) (r *recorder, stop func()) {
	t.Helper()
	r = &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Watch(ctx, 10*time.Millisecond, r.report)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Watch did not return after its ctx ended")
		}
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r, stop
}

func TestStoreWatch(t *testing.T) {
	ca := tlstest.NewCA(t)
	dir := t.TempDir()
	s, err := Load(fullWrite(t, dir, issue(t, ca, leafLife)))
	require.NoError(t, err)
	addr := echo(t, s.ServerConfig())
	r, stop := watch(t, s)

	// A full write is reported (true, nil) once.
	b := issue(t, ca, leafLife)
	fullWrite(t, dir, b)
	r.waitFor(t, "new pair reported", isOK(true))
	time.Sleep(100 * time.Millisecond)
	got := r.take()
	nTrue := 0
	for i, x := range got {
		if x.err == nil && x.changed {
			nTrue++
			for _, after := range got[i+1:] {
				assert.NoError(t, after.err, "no error after the new pair was taken")
			}
		}
	}
	assert.Equal(t, 1, nTrue, "%v", got)
	assert.Equal(t, serialOf(t, b.cert), served(t, addr, ca))

	// No change reports nothing: no error and no new leaf. A trailing
	// (false, nil) from a stat taken between the renames above is allowed.
	time.Sleep(200 * time.Millisecond)
	for _, x := range r.take() {
		assert.False(t, x.err != nil || x.changed, "no change reported %v", x)
	}

	// A half-written pair is reported with an error on every tick until the
	// next full write.
	c := issue(t, ca, leafLife)
	writeFile(t, filepath.Join(dir, "cert.pem"), c.cert, 0o644)
	r.waitFor(t, "half-written pair reported", isErr)
	time.Sleep(150 * time.Millisecond)
	got = r.take()
	require.GreaterOrEqual(t, len(got), 4, "an error on every tick: %v", got)
	for _, x := range got {
		require.Error(t, x.err)
		assert.False(t, x.changed)
	}
	assert.NotEmpty(t, s.Info().LastError)
	assert.Equal(t, serialOf(t, b.cert), served(t, addr, ca), "the pair in force stays")
	fullWrite(t, dir, c)
	r.waitFor(t, "completed pair reported", isOK(true))
	assert.Empty(t, s.Info().LastError)
	assert.Equal(t, serialOf(t, c.cert), served(t, addr, ca))

	// An unreadable key, then the same key readable again.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		time.Sleep(50 * time.Millisecond)
		r.take()
		keyFile := filepath.Join(dir, "key.pem")
		fi, err := os.Stat(keyFile)
		require.NoError(t, err)
		// The mode goes first: a touch taken alone could be read, as
		// unchanged, before the chmod.
		require.NoError(t, os.Chmod(keyFile, 0))
		touched := fi.ModTime().Add(2 * time.Second)
		require.NoError(t, os.Chtimes(keyFile, touched, touched))
		r.waitFor(t, "unreadable key reported", isErr)
		assert.NotEmpty(t, s.Info().LastError)

		r.take()
		require.NoError(t, os.Chmod(keyFile, 0o600))
		fi2, err := os.Stat(keyFile)
		require.NoError(t, err)
		require.True(t, fi2.ModTime().Equal(touched), "chmod leaves the modification time")
		r.waitFor(t, "readable key reported", isOK(false))
		assert.Empty(t, s.Info().LastError)
		assert.Equal(t, serialOf(t, c.cert), served(t, addr, ca))
	}

	stop()
}

func TestStoreWatchSymlink(t *testing.T) {
	ca := tlstest.NewCA(t)
	root := t.TempDir()
	da, db := filepath.Join(root, "a"), filepath.Join(root, "b")
	require.NoError(t, os.Mkdir(da, 0o755))
	require.NoError(t, os.Mkdir(db, 0o755))
	a, b := issue(t, ca, leafLife), issue(t, ca, leafLife)
	fullWrite(t, da, a)
	fullWrite(t, db, b)

	live := filepath.Join(root, "live")
	require.NoError(t, os.Mkdir(live, 0o755))
	certFile, keyFile := filepath.Join(live, "cert.pem"), filepath.Join(live, "key.pem")
	if err := os.Symlink(filepath.Join(da, "cert.pem"), certFile); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	require.NoError(t, os.Symlink(filepath.Join(da, "key.pem"), keyFile))
	s, err := Load(certFile, keyFile)
	require.NoError(t, err)
	addr := echo(t, s.ServerConfig())
	require.Equal(t, serialOf(t, a.cert), served(t, addr, ca))
	r, stop := watch(t, s)

	retarget := func(link, target string) {
		tmp := link + ".tmp"
		require.NoError(t, os.Symlink(target, tmp))
		require.NoError(t, os.Rename(tmp, link))
	}
	retarget(certFile, filepath.Join(db, "cert.pem"))
	retarget(keyFile, filepath.Join(db, "key.pem"))
	r.waitFor(t, "retargeted pair reported", isOK(true))
	assert.Equal(t, serialOf(t, b.cert), served(t, addr, ca))
	stop()
}

func TestClient(t *testing.T) {
	ca := tlstest.NewCA(t)
	addr := echo(t, ca.Server(t))

	c := Client(ca.Pool(), false)
	assert.Equal(t, uint16(tls.VersionTLS12), c.MinVersion)
	assert.False(t, c.InsecureSkipVerify)
	assert.Empty(t, c.ServerName)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: c}).DialContext(ctx, "tcp", addr)
	require.NoError(t, err)
	_ = conn.Close()

	_, err = (&tls.Dialer{Config: Client(tlstest.NewCA(t).Pool(), false)}).DialContext(ctx, "tcp", addr)
	require.ErrorAs(t, err, new(x509.UnknownAuthorityError))

	in := Client(nil, true)
	assert.True(t, in.InsecureSkipVerify)
	assert.Nil(t, in.RootCAs)
	assert.Equal(t, uint16(tls.VersionTLS12), in.MinVersion)
	conn, err = (&tls.Dialer{Config: in}).DialContext(ctx, "tcp", addr)
	require.NoError(t, err)
	_ = conn.Close()

	assert.Nil(t, Client(nil, false).RootCAs, "nil means the system's roots")
}
