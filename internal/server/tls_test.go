package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
	"github.com/ntt0601zcoder/open-streamer/pkg/tlstest"
)

// handshakeLine is the message of the one line a failed TLS handshake logs.
const handshakeLine = "server: tls: handshake failed"

// tlsServerYAML is the server section of the TLS tests: every listener and
// every tls_listen on an ephemeral loopback port, with the pair.
func tlsServerYAML(cert, key string) string {
	return "server:\n" +
		"  tls: {cert_file: \"" + cert + "\", key_file: \"" + key + "\"}\n" +
		"  http: {listen: \"127.0.0.1:0\", tls_listen: \"127.0.0.1:0\"}\n" +
		"  api: {listen: \"127.0.0.1:0\", tls_listen: \"127.0.0.1:0\"}\n" +
		"  rtmp: {listen: \"127.0.0.1:0\", tls_listen: \"127.0.0.1:0\"}\n" +
		"  rtsp: {listen: \"127.0.0.1:0\", tls_listen: \"127.0.0.1:0\"}\n" +
		"  srt: {listen: \"127.0.0.1:0\"}\n"
}

// tlsText is a whole config around a server section, with the given stream
// entries.
func tlsText(server string, streams ...string) string {
	text := "version: 1\n" + server + usersYAML()
	if len(streams) == 0 {
		return text + "streams: []\n"
	}
	return text + "streams:\n" + strings.Join(streams, "")
}

// tlsPair makes a CA and writes a pair (the key 0600) for 127.0.0.1 and
// localhost into a new directory.
func tlsPair(t *testing.T) (ca *tlstest.CA, cert, key string) {
	t.Helper()
	ca = tlstest.NewCA(t)
	cert, key = ca.Files(t, t.TempDir(), "127.0.0.1", "localhost")
	return ca, cert, key
}

// tlsConfig writes a config with every listener and every tls_listen on
// 127.0.0.1:0 and a tlstest pair (0600) for 127.0.0.1 and localhost; extra
// are its stream entries.
func tlsConfig(t *testing.T, extra string) (path string, ca *tlstest.CA) {
	t.Helper()
	ca, cert, key := tlsPair(t)
	if extra == "" {
		return writeConfig(t, tlsText(tlsServerYAML(cert, key))), ca
	}
	return writeConfig(t, tlsText(tlsServerYAML(cert, key), extra)), ca
}

// tlsOptions are the options of a TLS test server: the scripted input, and
// the CA as the roots of the streams' clients.
func tlsOptions(t *testing.T, ca *tlstest.CA, logs io.Writer) Options {
	t.Helper()
	return Options{Version: "test", LogOut: logs, RequiredIterations: testIter, Open: newScript(t).Open, RootCAs: ca.Pool()}
}

// tlsStart starts a server from tlsConfig; mutate may change the options.
func tlsStart(t *testing.T, extra string, mutate func(*Options)) (*testServer, *tlstest.CA) {
	t.Helper()
	path, ca := tlsConfig(t, extra)
	return tlsStartPath(t, path, ca, mutate), ca
}

// tlsStartPath starts a server from the config at path.
func tlsStartPath(t *testing.T, path string, ca *tlstest.CA, mutate func(*Options)) *testServer {
	t.Helper()
	logs := &lockedBuffer{}
	opt := tlsOptions(t, ca, logs)
	if mutate != nil {
		mutate(&opt)
	}
	srv, err := New(path, opt)
	require.NoError(t, err)
	return start(t, srv, path, logs)
}

// httpsClient is an HTTP client that trusts only the CA.
func httpsClient(t *testing.T, ca *tlstest.CA) *http.Client {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: ca.Client()}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: waitFor}
}

// answer is what the TLS tests look at in a response.
type answer struct {
	StatusCode int
	ProtoMajor int
	TLS        *tls.ConnectionState
}

// getStatus fetches url with c, as the editor when auth is set, and returns
// the answer, the body read and closed.
func getStatus(t *testing.T, c *http.Client, url string, auth bool) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	if auth {
		req.SetBasicAuth("ops", editorPW)
	}
	res, err := c.Do(req)
	require.NoError(t, err, url)
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	require.NoError(t, res.Body.Close())
	return answer{StatusCode: res.StatusCode, ProtoMajor: res.ProtoMajor, TLS: res.TLS}
}

// leafSerial is the serial of the first certificate of a PEM file.
func leafSerial(t *testing.T, certFile string) *big.Int {
	t.Helper()
	data, err := os.ReadFile(certFile)
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert.SerialNumber
}

// certFileOf is the cert_file of the config at path.
func certFileOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return testParse(t, string(b)).Doc.Server.TLS.CertFile
}

// dialTCP connects to addr.
func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{Timeout: waitFor}).DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err, addr)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dialTLS runs a client handshake with addr.
func dialTLS(addr string, cfg *tls.Config) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return c.(*tls.Conn), nil //nolint:forcetypeassert // a tls.Dialer's connection
}

// garbage sends bytes that are no ClientHello to addr, waits until the
// server closed the connection and returns the client's address.
func garbage(t *testing.T, addr string) string {
	t.Helper()
	c := dialTCP(t, addr)
	_, err := c.Write([]byte("\x01\x02\x03 this is no TLS record\r\n\r\n"))
	require.NoError(t, err)
	require.True(t, closedByServer(c, waitFor), "%s was not closed", addr)
	return c.LocalAddr().String()
}

// closedByServer reports whether the peer closes c within d: the read ends
// with anything but a timeout.
func closedByServer(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, c)
	var ne net.Error
	return err == nil || !errors.As(err, &ne) || !ne.Timeout()
}

// logLines are the JSON lines of text whose msg is, or starts with, msg.
func logLines(t *testing.T, text, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		if s, _ := m["msg"].(string); strings.HasPrefix(s, msg) {
			out = append(out, m)
		}
	}
	return out
}

func TestTLSListeners(t *testing.T) {
	s, ca := tlsStart(t, "", nil)
	a := s.Addrs()
	ports := map[string]bool{}
	for _, addr := range []string{a.HTTP, a.HTTPS, a.API, a.APITLS, a.RTMP, a.RTMPS, a.RTSP, a.RTSPS, a.SRT} {
		p := port(t, addr)
		require.NotEqual(t, "0", p)
		ports[p] = true
	}
	require.Len(t, ports, 9, "%+v", a)
	serial := leafSerial(t, certFileOf(t, s.path))

	c := httpsClient(t, ca)
	res := getStatus(t, c, "https://"+a.HTTPS+"/live/x/hls/index.m3u8", false)
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
	require.NotNil(t, res.TLS)
	assert.NotEmpty(t, res.TLS.VerifiedChains, "the handshake was verified")
	assert.Equal(t, serial, res.TLS.PeerCertificates[0].SerialNumber)
	assert.Equal(t, http.StatusOK, getStatus(t, c, "https://"+a.APITLS+"/healthz", false).StatusCode)
	assert.Equal(t, http.StatusUnauthorized, getStatus(t, c, "https://"+a.APITLS+"/api/v1/streams", false).StatusCode)
	assert.Equal(t, http.StatusOK, getStatus(t, c, "https://"+a.APITLS+"/api/v1/streams", true).StatusCode)
	// The name of the certificate, and not only its address.
	assert.Equal(t, http.StatusOK, getStatus(t, c, "https://localhost:"+port(t, a.APITLS)+"/healthz", false).StatusCode)

	// The plain listeners answer as before.
	assert.Equal(t, http.StatusOK, s.do(http.MethodGet, "/healthz", "", "").code)
	assert.Equal(t, http.StatusUnauthorized, s.do(http.MethodGet, "/api/v1/streams", "", "").code)
	assert.Equal(t, http.StatusOK, s.do(http.MethodGet, "/api/v1/streams", "ops", "").code)
	assert.Equal(t, http.StatusNotFound, s.mediaGet("/live/x/hls/index.m3u8"))

	for name, addr := range map[string]string{"https": a.HTTPS, "api": a.APITLS, "rtmps": a.RTMPS, "rtsps": a.RTSPS} {
		conn, err := dialTLS(addr, ca.Client())
		require.NoError(t, err, name)
		st := conn.ConnectionState()
		assert.Equal(t, serial, st.PeerCertificates[0].SerialNumber, name)
		assert.NotEmpty(t, st.VerifiedChains, name)
		assert.GreaterOrEqual(t, st.Version, uint16(tls.VersionTLS12), name)
		require.NoError(t, conn.Close(), name)

		// The system's roots do not know the test CA.
		_, err = dialTLS(addr, &tls.Config{MinVersion: tls.VersionTLS12})
		var unknown x509.UnknownAuthorityError
		require.ErrorAs(t, err, &unknown, name)
	}
	_, err := http.DefaultClient.Get("https://" + a.HTTPS + "/live/x/hls/index.m3u8") //nolint:bodyclose,noctx // it fails
	var unknown x509.UnknownAuthorityError
	require.ErrorAs(t, err, &unknown)
	http.DefaultClient.CloseIdleConnections()

	// Every address is free again after the stop.
	s.stop()
	for _, addr := range []string{a.HTTP, a.HTTPS, a.API, a.APITLS, a.RTMP, a.RTMPS, a.RTSP, a.RTSPS} {
		require.NoError(t, listenTCP(t, addr).Close(), addr)
	}
}

// R-S4: the TLS listeners speak HTTP/1.1 to a client that offers h2.
func TestHTTPSIsHTTP1(t *testing.T) {
	s, ca := tlsStart(t, "", nil)
	for name, addr := range map[string]string{"https": s.Addrs().HTTPS, "api": s.Addrs().APITLS} {
		cfg := ca.Client()
		cfg.NextProtos = []string{"h2", "http/1.1"}
		conn, err := dialTLS(addr, cfg)
		require.NoError(t, err, name)
		assert.Equal(t, "http/1.1", conn.ConnectionState().NegotiatedProtocol, name)
		require.NoError(t, conn.Close())

		tr := &http.Transport{TLSClientConfig: ca.Client(), ForceAttemptHTTP2: true}
		res := getStatus(t, &http.Client{Transport: tr, Timeout: waitFor}, "https://"+addr+"/healthz", false)
		assert.Equal(t, 1, res.ProtoMajor, name)
		assert.Equal(t, "http/1.1", res.TLS.NegotiatedProtocol, name)
		tr.CloseIdleConnections()
	}
}

func TestTLSOff(t *testing.T) {
	s := testConfig(t, nil)
	a := s.Addrs()
	assert.Empty(t, a.HTTPS)
	assert.Empty(t, a.APITLS)
	assert.Empty(t, a.RTMPS)
	assert.Empty(t, a.RTSPS)
	// The listeners bound are exactly the five of before.
	for _, addr := range []string{a.HTTP, a.API, a.RTMP, a.RTSP, a.SRT} {
		assert.NotEqual(t, "0", port(t, addr))
	}
	assert.Nil(t, s.store)
	assert.Nil(t, s.mediaTLS)
	assert.Nil(t, s.apiTLS)
	assert.Zero(t, s.rtsp.Stats().Server.TLSHandshakeFailures)
	assert.NotContains(t, s.logs.String(), "tls")
}

func TestTLSStartErrors(t *testing.T) {
	opt := func() Options { return Options{LogOut: io.Discard, RequiredIterations: testIter} }
	ca, cert, key := tlsPair(t)
	_, _, otherKey := tlsPair(t)

	t.Run("a key of another certificate", func(t *testing.T) {
		// Every port is named, so that the test can see them free again.
		text := tlsText(tlsServerYAML(cert, otherKey))
		addrs := make([]string, 0, 8)
		for range 8 {
			ln := listenTCP(t, "127.0.0.1:0")
			addrs = append(addrs, ln.Addr().String())
			require.NoError(t, ln.Close())
		}
		for _, addr := range addrs {
			text = strings.Replace(text, "\"127.0.0.1:0\"", "\""+addr+"\"", 1)
		}
		_, err := New(writeConfig(t, text), opt())
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "server: tls: "), err.Error())
		// Neither the key nor its content is in the text.
		keyPEM, rerr := os.ReadFile(otherKey)
		require.NoError(t, rerr)
		block, _ := pem.Decode(keyPEM)
		assert.NotContains(t, err.Error(), string(pem.EncodeToMemory(block))[28:60])
		for _, addr := range addrs {
			require.NoError(t, listenTCP(t, addr).Close(), "%s is still bound", addr)
		}
	})

	t.Run("a missing file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "cert.pem")
		_, err := New(writeConfig(t, tlsText(tlsServerYAML(missing, key))), opt())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.tls.cert_file: ")
		assert.Contains(t, err.Error(), missing)
	})

	t.Run("a certificate without any tls_listen", func(t *testing.T) {
		text := strings.ReplaceAll(tlsText(tlsServerYAML(cert, key)), `, tls_listen: "127.0.0.1:0"`, "")
		s := tlsStartPath(t, writeConfig(t, text), ca, nil)
		a := s.Addrs()
		assert.Equal(t, Addrs{HTTP: a.HTTP, API: a.API, RTMP: a.RTMP, RTSP: a.RTSP, SRT: a.SRT}, a)
		assert.NotNil(t, s.store, "the pair is loaded all the same")
		lines := logLines(t, s.logs.String(), "server: tls: ")
		require.Len(t, lines, 1)
		assert.Equal(t, "WARN", lines[0]["level"])
		assert.Contains(t, lines[0]["msg"], "no tls_listen")
	})
}

func TestKeyFileMode(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("no Unix permission bits")
	}
	for _, c := range []struct {
		mode os.FileMode
		warn bool
	}{{0o644, true}, {0o640, true}, {0o600, false}, {0o400, false}} {
		ca, cert, key := tlsPair(t)
		require.NoError(t, os.Chmod(key, c.mode))
		s := tlsStartPath(t, writeConfig(t, tlsText(tlsServerYAML(cert, key))), ca, nil)
		name := fmt.Sprintf("%04o", c.mode)
		lines := logLines(t, s.logs.String(), "server: tls: key_file can be read")
		if c.warn {
			require.Len(t, lines, 1, name)
			assert.Equal(t, "WARN", lines[0]["level"], name)
			assert.Equal(t, key, lines[0]["path"], name)
			assert.Equal(t, name, lines[0]["mode"], name)
		} else {
			assert.Empty(t, lines, name)
		}
		// The server starts and serves TLS whatever the mode.
		res := getStatus(t, httpsClient(t, ca), "https://"+s.Addrs().APITLS+"/healthz", false)
		assert.Equal(t, http.StatusOK, res.StatusCode, name)
		s.stop()
	}
}

func TestHTTPSWrongProtocol(t *testing.T) {
	const header = time.Second
	s, ca := tlsStart(t, "", func(o *Options) { o.HeaderTimeout = header })
	a := s.Addrs()

	// HTTP in clear on the HTTPS port.
	c := dialTCP(t, a.HTTPS)
	_, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+a.HTTPS+"\r\n\r\n")
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(waitFor))
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	assert.Contains(t, strings.ToLower(string(body)), "client sent an http request to an https server")

	// A ClientHello on the plain port: closed, and the next request is
	// served.
	for _, addr := range []string{a.HTTP, a.API} {
		raw := dialTCP(t, addr)
		tc := tls.Client(raw, ca.Client())
		_ = raw.SetDeadline(time.Now().Add(waitFor))
		require.Error(t, tc.HandshakeContext(context.Background()), addr)
		require.True(t, closedByServer(raw, waitFor), addr)
	}
	assert.Equal(t, http.StatusNotFound, s.mediaGet("/live/x/hls/index.m3u8"))
	assert.Equal(t, http.StatusOK, s.do(http.MethodGet, "/healthz", "", "").code)

	// Clients that connect and say nothing hold no one up, and are closed
	// by the handshake timeout.
	silent := make([]net.Conn, 20)
	for i := range silent {
		silent[i] = dialTCP(t, a.HTTPS)
	}
	require.Eventually(t, func() bool { return s.mediaTLS.open() >= len(silent) }, waitFor, time.Millisecond)
	connected := time.Now()
	hc := httpsClient(t, ca)
	assert.Equal(t, http.StatusNotFound, getStatus(t, hc, "https://"+a.HTTPS+"/live/x/hls/index.m3u8", false).StatusCode)
	assert.Less(t, time.Since(connected), time.Second, "a good request waits for no silent client")
	var wg sync.WaitGroup
	var open atomic.Int64
	for _, sc := range silent {
		wg.Go(func() {
			if !closedByServer(sc, time.Until(connected.Add(header+2*time.Second))) {
				open.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Zero(t, open.Load(), "silent clients still open after the header timeout plus 2 s")
	assert.GreaterOrEqual(t, time.Since(connected), header-100*time.Millisecond, "they were given the header timeout")
	hc.CloseIdleConnections()
	require.Eventually(t, func() bool { return s.mediaTLS.open() == 0 }, waitFor, 5*time.Millisecond)
}

// errorLogServer is an httptest TLS server (its built-in certificate: trust
// is not under test) whose ErrorLog is a Server's httpErrorLog("https").
type errorLogServer struct {
	s    *Server
	srv  *httptest.Server
	logs *lockedBuffer
	n    atomic.Uint64
}

func newErrorLogServer(t *testing.T, level, listen string) *errorLogServer {
	t.Helper()
	e := &errorLogServer{logs: &lockedBuffer{}}
	log, err := logging.New(e.logs, level, "json")
	require.NoError(t, err)
	e.s = &Server{log: log}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("boom in the handler")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if listen != "" {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", listen)
		if err != nil {
			t.Skipf("cannot listen on %s: %v", listen, err)
		}
		require.NoError(t, e.srv.Listener.Close())
		e.srv.Listener = ln
	}
	e.srv.Config.ErrorLog = e.s.httpErrorLog("https", &e.n)
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	return e
}

func TestHTTPErrorLog(t *testing.T) {
	t.Run("info", func(t *testing.T) {
		e := newErrorLogServer(t, "info", "")
		garbage(t, e.srv.Listener.Addr().String())
		assert.EqualValues(t, 1, e.n.Load())
		assert.Empty(t, e.logs.String(), "the debug line is filtered, and net/http's text is not logged")
	})

	t.Run("debug", func(t *testing.T) {
		e := newErrorLogServer(t, "debug", "")
		remote := garbage(t, e.srv.Listener.Addr().String())
		assert.EqualValues(t, 1, e.n.Load())
		assert.Equal(t, 1, strings.Count(e.logs.String(), "\n"))
		lines := logLines(t, e.logs.String(), handshakeLine)
		require.Len(t, lines, 1)
		assert.Equal(t, handshakeLine, lines[0]["msg"])
		assert.Equal(t, "DEBUG", lines[0]["level"])
		assert.Equal(t, "https", lines[0]["listener"])
		assert.Equal(t, remote, lines[0]["remote"])
		assert.NotEmpty(t, lines[0]["error"])
		assert.NotContains(t, e.logs.String(), "http: TLS")
		assert.NotContains(t, lines[0]["error"], remote)

		// HTTP in clear is a failed handshake too, with net/http's reason.
		c := dialTCP(t, e.srv.Listener.Addr().String())
		_, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		require.NoError(t, err)
		require.True(t, closedByServer(c, waitFor))
		// net/http answers and closes before it reports.
		require.Eventually(t, func() bool { return len(logLines(t, e.logs.String(), handshakeLine)) == 2 }, waitFor, time.Millisecond)
		assert.EqualValues(t, 2, e.n.Load())
		lines = logLines(t, e.logs.String(), handshakeLine)
		require.Len(t, lines, 2)
		assert.Equal(t, "client sent an HTTP request to an HTTPS server", lines[1]["error"])
		assert.Equal(t, c.LocalAddr().String(), lines[1]["remote"])
	})

	t.Run("an IPv6 client", func(t *testing.T) {
		e := newErrorLogServer(t, "debug", "[::1]:0")
		remote := garbage(t, e.srv.Listener.Addr().String())
		require.True(t, strings.HasPrefix(remote, "[::1]:"), remote)
		lines := logLines(t, e.logs.String(), handshakeLine)
		require.Len(t, lines, 1)
		assert.Equal(t, remote, lines[0]["remote"], "the whole address")
		assert.NotEmpty(t, lines[0]["error"])
		assert.EqualValues(t, 1, e.n.Load())
	})

	t.Run("a handler that panics", func(t *testing.T) {
		e := newErrorLogServer(t, "info", "")
		res, err := e.srv.Client().Get(e.srv.URL + "/ok") //nolint:noctx // a test server
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		assert.Equal(t, http.StatusNoContent, res.StatusCode)
		assert.Empty(t, e.logs.String())
		// A POST: the client does not send it a second time.
		_, err = e.srv.Client().Post(e.srv.URL+"/panic", "text/plain", strings.NewReader("x")) //nolint:bodyclose,noctx // it fails
		require.Error(t, err)
		require.Eventually(t, func() bool { return e.logs.String() != "" }, waitFor, time.Millisecond)
		assert.Equal(t, 1, strings.Count(e.logs.String(), "\n"))
		// Under the server's name: it serves the plain listener too.
		lines := logLines(t, e.logs.String(), "server: media: http: panic serving")
		require.Len(t, lines, 1)
		assert.Equal(t, "ERROR", lines[0]["level"])
		assert.Contains(t, lines[0]["msg"], "boom in the handler")
		assert.Contains(t, lines[0]["msg"], "goroutine ", "the stack is kept")
		assert.Zero(t, e.n.Load())
	})

	t.Run("after the stop began", func(t *testing.T) {
		e := newErrorLogServer(t, "debug", "")
		garbage(t, e.srv.Listener.Addr().String())
		require.EqualValues(t, 1, e.n.Load())
		before := e.logs.String()
		e.s.stopping.Store(true)
		garbage(t, e.srv.Listener.Addr().String())
		assert.EqualValues(t, 1, e.n.Load())
		assert.Equal(t, before, e.logs.String())
	})
}

func TestTLSHandshakeFailedReported(t *testing.T) {
	debug := func(t *testing.T) (*testServer, *tlstest.CA) {
		t.Helper()
		ca, cert, key := tlsPair(t)
		text := tlsText(tlsServerYAML(cert, key) + "  log: {level: debug}\n")
		return tlsStartPath(t, writeConfig(t, text), ca, nil), ca
	}
	failures := func(s *testServer) map[string]uint64 {
		return map[string]uint64{
			"https": s.httpsFailures.Load(),
			"api":   s.apiFailures.Load(),
			"rtmps": rtmpsrv.Default.Stats(s.Addrs().RTMPS).TLSHandshakeFailures,
			"rtsps": s.rtsp.Stats().Server.TLSHandshakeFailures,
		}
	}
	s, ca := debug(t)
	other, _ := debug(t)
	a := s.Addrs()
	addrs := map[string]string{"https": a.HTTPS, "api": a.APITLS, "rtmps": a.RTMPS, "rtsps": a.RTSPS}
	remotes := map[string]string{}
	for name, addr := range addrs {
		remotes[name] = garbage(t, addr)
	}
	one := map[string]uint64{"https": 1, "api": 1, "rtmps": 1, "rtsps": 1}
	require.Eventually(t, func() bool { return len(logLines(t, s.logs.String(), handshakeLine)) >= 4 }, waitFor, 5*time.Millisecond)
	check := func() {
		t.Helper()
		lines := logLines(t, s.logs.String(), handshakeLine)
		require.Len(t, lines, 4)
		seen := map[string]bool{}
		for _, l := range lines {
			name, _ := l["listener"].(string)
			require.Contains(t, addrs, name)
			assert.False(t, seen[name], "one line for %s", name)
			seen[name] = true
			assert.Equal(t, "DEBUG", l["level"], name)
			assert.Equal(t, remotes[name], l["remote"], name)
			assert.NotEmpty(t, l["error"], name)
		}
		assert.Equal(t, one, failures(s))
		assert.NotContains(t, s.logs.String(), "http: TLS")
	}
	check()

	// A good handshake adds nothing.
	for name, addr := range addrs {
		conn, err := dialTLS(addr, ca.Client())
		require.NoError(t, err, name)
		require.NoError(t, conn.Close(), name)
	}
	// And neither does a plain request on a plain port.
	assert.Equal(t, http.StatusOK, s.do(http.MethodGet, "/healthz", "", "").code)
	time.Sleep(100 * time.Millisecond)
	check()

	// Nothing is process-wide: another server saw none of it.
	assert.Empty(t, logLines(t, other.logs.String(), handshakeLine))
	assert.Equal(t, map[string]uint64{"https": 0, "api": 0, "rtmps": 0, "rtsps": 0}, failures(other))

	// At info nothing is logged, and the failures are still counted.
	quiet, _ := tlsStart(t, "", nil)
	garbage(t, quiet.Addrs().HTTPS)
	garbage(t, quiet.Addrs().RTMPS)
	require.Eventually(t, func() bool {
		f := failures(quiet)
		return f["https"] == 1 && f["rtmps"] == 1
	}, waitFor, 5*time.Millisecond)
	assert.NotContains(t, quiet.logs.String(), "handshake")
}

// stalledReader asks for an HTTP-TS stream over https and never reads the
// answer. It returns once the server's writes to it have stalled: the hub
// drops frames for the handler's subscriber.
func stalledReader(t *testing.T, s *testServer, ca *tlstest.CA, name string) {
	t.Helper()
	before := s.mediaTLS.open()
	raw := dialTCP(t, s.Addrs().HTTPS)
	// A small window on this side, a small buffer on the server's.
	require.NoError(t, raw.(*net.TCPConn).SetReadBuffer(4096)) //nolint:forcetypeassert // a TCP connection
	require.Eventually(t, func() bool { return s.mediaTLS.open() == before+1 }, waitFor, time.Millisecond)
	s.mediaTLS.mu.Lock()
	for c := range s.mediaTLS.conns {
		if c.RemoteAddr().String() == raw.LocalAddr().String() {
			require.NoError(t, c.Conn.(*net.TCPConn).SetWriteBuffer(4096)) //nolint:forcetypeassert // a TCP connection
		}
	}
	s.mediaTLS.mu.Unlock()
	cfg := ca.Client()
	cfg.ServerName = "127.0.0.1"
	tc := tls.Client(raw, cfg)
	require.NoError(t, tc.HandshakeContext(context.Background()))
	_, err := io.WriteString(tc, "GET /live/"+name+"/stream.ts HTTP/1.1\r\nHost: "+s.Addrs().HTTPS+"\r\n\r\n")
	require.NoError(t, err)
	h, ok := s.Manager().Hub(stream.OutHTTPTS, name)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		for _, st := range h.Stats() {
			if st.Dropped > 0 {
				return true
			}
		}
		return false
	}, 2*waitFor, 10*time.Millisecond, "the reader of %s never stalled", name)
}

func TestHTTPTSOverTLS(t *testing.T) {
	outs := "{httpts: true}"
	s, ca := tlsStart(t, streamYAML("cam", outs)+streamYAML("gone", outs)+streamYAML("held", outs), nil)
	for _, name := range []string{"cam", "gone", "held"} {
		s.generation(name)
	}
	a := s.Addrs()

	// The stream reads over https as it does in clear.
	var capture conform.Capture
	require.Eventually(t, func() bool {
		var err error
		capture, err = conform.ReadTS(context.Background(), "https://"+a.HTTPS+"/live/cam/stream.ts",
			conform.ReadOptions{Duration: 3 * time.Second, Timeout: 5 * time.Second, Client: httpsClient(t, ca)})
		return err == nil && len(capture.Frames) > 0
	}, waitFor, 100*time.Millisecond)
	kinds := map[media.Kind]int{}
	for _, f := range capture.Frames {
		kinds[f.Kind()]++
	}
	assert.Positive(t, kinds[media.KindVideo])
	assert.Positive(t, kinds[media.KindAudio])

	// Deleting a stream waits for no reader, stalled or not, TLS or not.
	stalledReader(t, s, ca, "gone")
	began := time.Now()
	res := s.do(http.MethodDelete, "/api/v1/streams/gone", "ops", "")
	require.Equal(t, http.StatusNoContent, res.code, res.body)
	assert.Less(t, time.Since(began), time.Second, "the delete waited for the stalled reader")

	// Neither does the stop, beyond shutdownWait: net/http's own close of
	// the stalled connections would take its close_notify's 5 s each.
	stalledReader(t, s, ca, "held")
	began = time.Now()
	s.stop()
	assert.Less(t, time.Since(began), shutdownWait+time.Second, "the stop waited for a stalled reader")
	assert.Zero(t, s.mediaTLS.open(), "every raw connection is closed")
	require.NoError(t, listenTCP(t, a.HTTPS).Close())
}

// stallListener is a listener whose connections can be made to act as a
// peer with full buffers does: see stallConn.
type stallListener struct {
	net.Listener
	conns chan *stallConn
}

func (l *stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := &stallConn{Conn: c, closed: make(chan struct{})}
	l.conns <- sc
	return sc, nil
}

// stallConn is a connection whose writes, once stall is set, take nothing
// and wait for the write deadline or for Close, as a write to a peer that
// reads nothing does once the buffers are full. A real socket would do,
// were it not for how much a kernel still takes after it refused a write.
type stallConn struct {
	net.Conn
	stall  atomic.Bool
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	deadline time.Time
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) Write(p []byte) (int, error) {
	if !c.stall.Load() {
		return c.Conn.Write(p)
	}
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	var expired <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	}
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// R-S8's exemption has a bound: net/http closes a TLS connection with a
// close_notify, which waits up to 5 s on a peer whose buffers are full when
// no write is in flight. shutdown closes the raw connections at
// shutdownWait, so the stop takes no longer, for a request in flight and
// for an idle connection alike.
func TestShutdownClosesRawConns(t *testing.T) {
	for _, idle := range []bool{false, true} {
		ca := tlstest.NewCA(t)
		under := &stallListener{Listener: listenTCP(t, "127.0.0.1:0"), conns: make(chan *stallConn, 1)}
		ln := newTrackListener(under)
		entered := make(chan struct{}, 1)
		isIdle := make(chan struct{}, 1)
		srv := &http.Server{
			ReadHeaderTimeout: headerTimeout,
			TLSConfig:         ca.Server(t),
			ConnState: func(_ net.Conn, st http.ConnState) {
				if st == http.StateIdle {
					isIdle <- struct{}{}
				}
			},
			ErrorLog: (&Server{log: slog.New(slog.DiscardHandler)}).httpErrorLog("https", new(atomic.Uint64)),
			Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				if !idle {
					<-r.Context().Done() // a request in flight that writes nothing
				}
			}),
		}
		served := make(chan error, 1)
		go func() { served <- srv.ServeTLS(ln, "", "") }()

		tc, err := dialTLS(ln.Addr().String(), ca.Client())
		require.NoError(t, err)
		t.Cleanup(func() { _ = tc.Close() })
		_, err = io.WriteString(tc, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		require.NoError(t, err)
		select {
		case <-entered:
		case <-time.After(waitFor):
			require.Fail(t, "the request never reached the handler")
		}
		if idle {
			// The answer is read: the connection is kept alive, idle.
			res, err := http.ReadResponse(bufio.NewReader(tc), nil)
			require.NoError(t, err)
			require.NoError(t, res.Body.Close())
			// And the server has it down as idle: only then is it
			// Shutdown's to close, and not still its own goroutine's.
			select {
			case <-isIdle:
			case <-time.After(waitFor):
				require.Fail(t, "the connection never became idle")
			}
		}
		require.Equal(t, 1, ln.open())
		(<-under.conns).stall.Store(true)

		began := time.Now()
		shutdown(context.Background(), srv, ln)
		took := time.Since(began)
		assert.GreaterOrEqual(t, took, shutdownWait, "idle %v: the stalled connection was given shutdownWait", idle)
		assert.Less(t, took, shutdownWait+time.Second, "idle %v: the stop waited for a close_notify", idle)
		assert.Zero(t, ln.open(), "idle %v", idle)
		require.ErrorIs(t, <-served, http.ErrServerClosed)
		// Nothing is accepted after it.
		_, err = ln.Accept()
		require.Error(t, err)
	}
}

func TestTLSRestartRequired(t *testing.T) {
	path, ca := tlsConfig(t, "")
	s := tlsStartPath(t, path, ca, nil)
	addrs := s.Addrs()
	text := s.fileText()
	// The same certificate under another name: the pair still loads.
	cert := certFileOf(t, path)
	data, err := os.ReadFile(cert)
	require.NoError(t, err)
	moved := filepath.Join(t.TempDir(), "moved.pem")
	require.NoError(t, os.WriteFile(moved, data, 0o600))

	// Two ports that are free now, and still are after the reload.
	// Both are held until both are taken, so that they differ.
	free := make([]string, 2)
	held := make([]net.Listener, len(free))
	for i := range free {
		held[i] = listenTCP(t, "127.0.0.1:0")
		free[i] = held[i].Addr().String()
	}
	for _, ln := range held {
		require.NoError(t, ln.Close())
	}
	edited := strings.Replace(text, cert, moved, 1)
	edited = strings.Replace(edited, `http: {listen: "127.0.0.1:0", tls_listen: "127.0.0.1:0"}`, `http: {listen: "127.0.0.1:0", tls_listen: "`+free[0]+`"}`, 1)
	edited = strings.Replace(edited, `rtmp: {listen: "127.0.0.1:0", tls_listen: "127.0.0.1:0"}`, `rtmp: {listen: "127.0.0.1:0", tls_listen: "`+free[1]+`"}`, 1)
	require.NotEqual(t, text, edited)
	s.handEdit(edited)
	res, err := s.Reload(bg)
	require.NoError(t, err)
	want := []string{"server.http.tls_listen", "server.rtmp.tls_listen", "server.tls.cert_file"}
	assert.Equal(t, want, res.RestartRequired)
	assert.Equal(t, want, s.status().RestartRequired)
	// No listener changed.
	assert.Equal(t, addrs, s.Addrs())
	for name, addr := range map[string]string{"https": addrs.HTTPS, "rtmps": addrs.RTMPS} {
		conn, err := dialTLS(addr, ca.Client())
		require.NoError(t, err, name)
		require.NoError(t, conn.Close())
	}
	for _, addr := range free {
		require.NoError(t, listenTCP(t, addr).Close(), "%s was bound", addr)
	}

	s.handEdit(text)
	res, err = s.Reload(bg)
	require.NoError(t, err)
	assert.Empty(t, res.RestartRequired)
}

// playRTMPS plays url over RTMPS and returns once n media messages came.
func playRTMPS(t *testing.T, ca *tlstest.CA, url string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	var c *rtmp.ClientConn
	require.Eventually(t, func() bool {
		var err error
		c, err = rtmp.PlayWith(ctx, url, rtmp.Config{TLS: ca.Client()})
		return err == nil
	}, waitFor, 50*time.Millisecond, "no play of %s", url)
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	for range n {
		_, err := c.ReadMedia()
		require.NoError(t, err)
	}
}

// frameData is the payload of every frame of kind, each as one string.
func frameData(frames []media.Frame, kind media.Kind) []string {
	var out []string
	for _, f := range frames {
		if f.Kind() == kind {
			out = append(out, string(bytes.Join(f.Data, nil)))
		}
	}
	return out
}

// overlap is the number of frames two captures of one live stream share:
// the longest run on which one, laid over the other from some frame on,
// agrees with it frame for frame to the end of the shorter. Every
// alignment is tried and the best kept, since a payload may repeat (the
// fixture's audio does); 0 when none agrees.
func overlap(a, b []string) int {
	best := 0
	for range 2 {
		for i := range a {
			n := min(len(a)-i, len(b))
			if n <= best {
				break // no later alignment is longer
			}
			same := true
			for j := 0; j < n && same; j++ {
				same = a[i+j] == b[j]
			}
			if same {
				best = n
			}
		}
		a, b = b, a
	}
	return best
}

func TestTLSOutputs(t *testing.T) {
	const off = "the server's rtmp listener is off: set server.rtmp.listen or server.rtmp.tls_listen"
	real := func(o *Options) { o.Open = nil }

	t.Run("RTMPS alone", func(t *testing.T) {
		ca, cert, key := tlsPair(t)
		server := strings.Replace(tlsServerYAML(cert, key), `rtmp: {listen: "127.0.0.1:0", tls_listen: "127.0.0.1:0"}`, `rtmp: {tls_listen: "127.0.0.1:0"}`, 1)
		s := tlsStartPath(t, writeConfig(t, tlsText(server, fileStream(t, "cam", "{rtmp: true}"))), ca, real)
		a := s.Addrs()
		require.Empty(t, a.RTMP)
		require.NotEmpty(t, a.RTMPS)
		assert.Empty(t, s.ConfigStatus().LoadErrors)
		s.generation("cam")
		playRTMPS(t, ca, "rtmps://"+a.RTMPS+"/live/cam", 20)
		assert.InDelta(t, 1, s.metric("openstreamer_output_sessions_started_total", "output", "rtmp"), 0)

		// Env.RTMPApp is set with the RTMPS listener alone: an rtmps listen
		// input stays out of the play app.
		put := func(name, path string) response {
			return s.do(http.MethodPut, "/api/v1/streams/"+name, "ops",
				streamJSON(name, "rtmps://"+a.RTMPS+"/"+path+"?mode=listen", `{"hls":true}`))
		}
		res := put("shadow", "live/anything")
		assert.Equal(t, http.StatusUnprocessableEntity, res.code, res.body)
		assert.Contains(t, res.body, "may not publish under the play app")
		res = put("pub", "in/key-0123456789")
		assert.Equal(t, http.StatusCreated, res.code, res.body)
		// A plain listen input cannot share the RTMPS listener.
		res = s.do(http.MethodPut, "/api/v1/streams/plain", "ops",
			streamJSON("plain", "rtmp://"+a.RTMPS+"/in/other?mode=listen", `{"hls":true}`))
		assert.NotEqual(t, http.StatusCreated, res.code, res.body)
	})

	t.Run("both RTMP listeners off", func(t *testing.T) {
		ca, cert, key := tlsPair(t)
		server := strings.Replace(tlsServerYAML(cert, key), "  rtmp: {listen: \"127.0.0.1:0\", tls_listen: \"127.0.0.1:0\"}\n", "", 1)
		s := tlsStartPath(t, writeConfig(t, tlsText(server, fileStream(t, "cam", "{rtmp: true}"))), ca, real)
		assert.Empty(t, s.Addrs().RTMP)
		assert.Empty(t, s.Addrs().RTMPS)
		assert.Nil(t, s.rtmp)
		errs := s.ConfigStatus().LoadErrors
		require.Len(t, errs, 1)
		assert.Contains(t, errs[0], off)
		st, ok := s.Status("cam")
		require.True(t, ok)
		assert.Equal(t, "stopped", st.State)
	})

	t.Run("publish over RTMPS, play over RTSPS", func(t *testing.T) {
		s, ca := tlsStart(t, "", real)
		a := s.Addrs()
		const key = "key-0123456789abcdef"
		res := s.do(http.MethodPut, "/api/v1/streams/pub", "ops",
			streamJSON("pub", "rtmps://"+a.RTMPS+"/in/"+key+"?mode=listen", `{"rtmp":true,"rtsp":true}`))
		require.Equal(t, http.StatusCreated, res.code, res.body)

		raw, err := os.ReadFile("../../testdata/fixtures/h264_aac.flv")
		require.NoError(t, err)
		tags, err := rtmp.ReadFLV(bytes.NewReader(raw))
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var p *rtmp.PublishConn
		// The route appears when the stream has opened its input.
		require.Eventually(t, func() bool {
			p, err = rtmp.Publish(ctx, "rtmps://"+a.RTMPS+"/in/"+key, rtmp.Config{TLS: ca.Client()})
			return err == nil
		}, waitFor, 20*time.Millisecond, "no publish was accepted")
		var pwg sync.WaitGroup
		defer pwg.Wait()
		defer cancel()
		defer func() { _ = p.Close() }()
		pwg.Go(func() {
			// The fixture in a loop, paced by its timestamps.
			span := tags[len(tags)-1].Timestamp + 40
			began := time.Now()
			for round := uint32(0); ; round++ {
				for _, tg := range tags {
					ts := round*span + tg.Timestamp
					if round > 0 && tg.Timestamp == 0 {
						continue // the headers go once
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Until(began.Add(time.Duration(ts) * time.Millisecond))):
					}
					if p.WriteMedia(rtmp.Media{Type: tg.Type, Timestamp: ts, Payload: tg.Data}) != nil {
						return
					}
				}
			}
		})
		s.generation("pub")
		started := func(out string) float64 {
			return s.metric("openstreamer_output_sessions_started_total", "output", out)
		}
		rtmpBefore, rtspBefore := started("rtmp"), started("rtsp")

		// An rtsps:// URL, verified against the CA.
		rctx, rcancel := context.WithTimeout(ctx, waitFor)
		defer rcancel()
		var c *rtsp.Client
		require.Eventually(t, func() bool {
			c, err = rtsp.Dial(rctx, "rtsps://"+a.RTSPS+"/live/pub", rtsp.Config{TLS: ca.Client()})
			return err == nil
		}, waitFor, 100*time.Millisecond, "no rtsps play")
		require.Len(t, c.Description().Media, 2)
		for range 50 {
			_, err := c.ReadPacket()
			require.NoError(t, err)
		}
		require.NoError(t, c.Close())
		// The system's roots refuse it.
		_, err = rtsp.Dial(rctx, "rtsps://"+a.RTSPS+"/live/pub", rtsp.Config{})
		var unknown x509.UnknownAuthorityError
		require.ErrorAs(t, err, &unknown)
		playRTMPS(t, ca, "rtmps://"+a.RTMPS+"/live/pub", 20)
		// The TLS players are players of the same outputs.
		assert.InDelta(t, rtmpBefore+1, started("rtmp"), 0)
		assert.InDelta(t, rtspBefore+1, started("rtsp"), 0)

		// Frame for frame as the plain path: two readers of the one hub, one
		// through each listener, at the same time.
		tlsDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&tls.Dialer{Config: ca.Client()}).DialContext(ctx, network, addr)
		}
		var (
			plain, secure conform.Capture
			perr, serr    error
			wg            sync.WaitGroup
		)
		opt := conform.ReadOptions{Duration: 2 * time.Second, Timeout: 5 * time.Second}
		wg.Go(func() { plain, perr = conform.ReadRTSP(ctx, "rtsp://"+a.RTSP+"/live/pub", opt) })
		wg.Go(func() {
			o := opt
			o.Dial = tlsDial
			secure, serr = conform.ReadRTSP(ctx, "rtsp://"+a.RTSPS+"/live/pub", o)
		})
		wg.Wait()
		require.NoError(t, perr)
		require.NoError(t, serr)
		assert.Empty(t, secure.Violations)
		assert.Zero(t, secure.RTPLost)
		for _, kind := range []media.Kind{media.KindVideo, media.KindAudio} {
			pf, sf := frameData(plain.Frames, kind), frameData(secure.Frames, kind)
			require.NotEmpty(t, sf, "kind %d over rtsps", kind)
			assert.Greater(t, overlap(pf, sf), 10, "kind %d: %d plain frames, %d over rtsps", kind, len(pf), len(sf))
		}
	})
}
