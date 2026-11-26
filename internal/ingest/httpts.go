package ingest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// httpTimeout is the dial, TLS handshake and response header timeout of the
// default HTTP client.
const httpTimeout = 10 * time.Second

// defaultClient is shared by every Open that has no Env.HTTP.
var defaultClient = NewHTTPClient(nil)

// NewHTTPClient returns the default client of HTTP inputs, trusting roots;
// nil means the system's. It is what Env.HTTP is set to when the roots are
// not the system's. It has 10 s dial, TLS handshake and response header
// timeouts and no overall timeout: the body of a live pull never ends.
// Keep-alives are off: a streaming pull gains nothing from connection reuse,
// and an idle pool would hold a connection (and its goroutines) for every
// failed attempt of a retry loop, e.g. a bare 503.
func NewHTTPClient(roots *x509.CertPool) *http.Client {
	t := newTransport()
	t.DisableKeepAlives = true
	if roots != nil {
		t.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: t}
}

// newTransport returns a transport with the default client's dial, TLS
// handshake and response header timeouts.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: httpTimeout}).DialContext,
		TLSHandshakeTimeout:   httpTimeout,
		ResponseHeaderTimeout: httpTimeout,
	}
}

// stripURLError drops the URL that net/http embeds in its errors.
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func httpErr(rawURL, op string, err error) error {
	return fmt.Errorf("ingest: http: %s %s: %w", op, Redact(rawURL), err)
}

// isHLSType reports whether a Content-Type names an HLS playlist.
func isHLSType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(contentType))
	}
	switch mt {
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl":
		return true
	}
	return false
}

// isPlaylist reports whether a body starts with #EXTM3U, after an optional
// BOM and white space.
func isPlaylist(r *bufio.Reader) bool {
	// MPEG-TS starts with 0x47; only a '#' needs a longer look.
	first, err := r.Peek(1)
	if err != nil || first[0] == 0x47 {
		return false
	}
	head, _ := r.Peek(64) // a short body ends the peek early
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	return bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte("#EXTM3U"))
}

// httpBody is the response body behind a sniffing buffer; Close cancels the
// request.
type httpBody struct {
	r      *bufio.Reader
	body   io.ReadCloser
	cancel context.CancelFunc
}

// Read implements io.Reader.
func (b *httpBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	return n, err //nolint:wrapcheck // io.Reader contract: errors pass through unwrapped.
}

// Close cancels the request and closes the body.
func (b *httpBody) Close() error {
	b.cancel()
	_ = b.body.Close()
	return nil
}

// httpReader reads one HTTP-TS response.
type httpReader struct {
	ts     *tsReader
	rawURL string
}

// openHTTP opens an HTTP input: HLS when the URL ends in .m3u8 or the
// response is a playlist by content type or body, HTTP-TS otherwise.
func openHTTP(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	client := env.HTTP
	if client == nil {
		client = defaultClient
	}
	// The request outlives ctx; only Close (or ctx, before the open ends)
	// cancels it.
	rctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	r, err := doHTTP(ctx, rctx, cancel, client, rawURL, p, env)
	if !stop() {
		// ctx ended during the open: the request is cancelled.
		if r != nil {
			_ = r.Close()
		}
		// doHTTP's errors already carry the prefix and the URL.
		switch {
		case err == nil:
			return nil, httpErr(rawURL, "open", ctx.Err())
		case !errors.Is(err, ctx.Err()):
			return nil, fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return nil, err
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return r, nil
}

func doHTTP(ctx, rctx context.Context, cancel context.CancelFunc, client *http.Client, rawURL string, p parsed, env Env) (Reader, error) {
	loaded := env.now()()
	playlistURL := p.u
	if p.spec.Pace {
		// A .m3u8 URL: the variant selector is ours, not the server's.
		playlistURL = dropQuery(p.u, variantKey)
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, playlistURL.String(), nil)
	if err != nil {
		return nil, httpErr(rawURL, "request", stripURLError(err))
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors embed the URL: report ours, redacted.
		return nil, httpErr(rawURL, "get", stripURLError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, httpErr(rawURL, "get", fmt.Errorf("status %s", resp.Status))
	}
	body := &httpBody{r: bufio.NewReaderSize(resp.Body, 64<<10), body: resp.Body, cancel: cancel}
	if p.spec.Pace || isHLSType(resp.Header.Get("Content-Type")) || isPlaylist(body.r) {
		// The request context stays alive: the HLS reader owns it.
		data, err := io.ReadAll(io.LimitReader(body.r, maxPlaylistBytes+1))
		_ = resp.Body.Close()
		if err != nil {
			return nil, httpErr(rawURL, "read playlist", stripURLError(err))
		}
		if len(data) > maxPlaylistBytes {
			return nil, httpErr(rawURL, "read playlist", fmt.Errorf("playlist exceeds %d bytes", maxPlaylistBytes))
		}
		h, err := openHLS(rctx, cancel, client, rawURL, playlistURL, data, resp.Request.URL, loaded, env)
		if err != nil {
			return nil, err
		}
		return h, nil
	}
	t, err := openTS(ctx, body, env.now(), Spec{Live: true}, &media.ConfigCache{})
	if err != nil {
		return nil, httpErr(rawURL, "open", err)
	}
	return &httpReader{ts: t, rawURL: rawURL}, nil
}

// Read implements Reader. The end of the body is an error, not io.EOF: a
// live pull never ends cleanly.
func (h *httpReader) Read() (Frame, error) {
	f, err := h.ts.Read()
	if err == nil || errors.Is(err, ErrDiscontinuity) {
		return f, err
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return Frame{}, httpErr(h.rawURL, "read", err)
}

// Kinds implements Reader.
func (h *httpReader) Kinds() []media.Kind { return h.ts.Kinds() }

// Spec implements Reader.
func (h *httpReader) Spec() Spec { return h.ts.Spec() }

// Stats implements Reader.
func (h *httpReader) Stats() map[string]uint64 { return h.ts.Stats() }

// Close implements Reader.
func (h *httpReader) Close() error {
	if err := h.ts.Close(); err != nil {
		return httpErr(h.rawURL, "close", err)
	}
	return nil
}
